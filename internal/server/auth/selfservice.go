package auth

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// Self-service (03 §7.7, §12.4.3; README S58): what a logged-in user does to the own account. Revoking one session,
// "sign out other browsers", "log out everywhere", changing the password, deleting the account, and revoking a
// linked device. Each one is a row of the revocation table (revoke.go) with its audit row (03 §10).
//
// Every call checks its caller again inside its own Write (loadSelf): the account must be active and the caller's
// web session must still exist. All of them are for web sessions; a device's principal (M2) gets a
// not-implemented error, as in Logout.

// Audit actions written here (03 §10).
const (
	auditLogoutEverywhere = "auth.logout_everywhere"
	auditPasswordChanged  = "auth.password_changed"
	auditSessionRevoked   = "session.revoked"
	auditUserDeleted      = "user.deleted"
	auditDeviceRevoked    = "device.revoked"
)

// targetDevice is the audit target kind of a linked device (03 §5).
const targetDevice = "device"

// fieldNewPassword is the JSON name of the new password in POST /api/v1/me/password, the key of its
// validation_failed answer (CheckPassword names the field "password").
const fieldNewPassword = "newPassword"

// errWrongPassword is 403 wrong_password: the re-authentication of a logged-in user failed.
func errWrongPassword() error { return api.NewError(api.CodeWrongPassword) }

// webCaller is the principal rule of the calls a web session makes for its own account: a device's principal (M2)
// is told that method is not implemented for it, and a principal without a user or a session is unauthenticated.
func webCaller(method string, p Principal) error {
	if p.Method == MethodBearer {
		return notImplemented(method + " for a device")
	}
	if p.Method != MethodSession || p.UserID == "" || p.SessionID == "" {
		return errUnauthenticated()
	}
	return nil
}

// readSelf is loadSelf in a Read of its own, for the calls that verify a password before their Write.
func (s *Service) readSelf(ctx context.Context, op string, p Principal) (self, error) {
	now := s.now()
	var me self
	err := s.db.Read(ctx, func(q *store.Q) error {
		var err error
		me, err = loadSelf(q, p, now)
		return err
	})
	if err != nil {
		return self{}, serviceErr(op, err)
	}
	return me, nil
}

// lastActiveAdmin reports whether user is the only active admin left (03 §7.11): demoting, disabling or deleting
// that account would leave the server without one.
func lastActiveAdmin(q *store.Q, user store.User) (bool, error) {
	if user.Role != store.RoleAdmin || user.Status != store.StatusActive {
		return false, nil
	}
	n, err := q.CountActiveAdmins()
	if err != nil {
		return false, err
	}
	return n <= 1, nil
}

// verifyOwnPassword checks the password a logged-in user typed again to confirm a change to the own account: the
// currentPassword of POST /me/password and the password of POST /me/delete. A password that does not match is
// *api.Error{wrong_password} (403, "re-authentication failed", 03 §12.2); so is one that can't be a password at
// all (empty, over 1024 bytes, refused by PRECIS), without a hash.
//
// It is a password check like a login's, so the two per-username buckets of failed password checks count it
// (03 §7.3), with a login's rules: the attempt takes its tokens before the hash (auth-user-ip, then auth-user, where
// the user's known IPs still pass when it is empty), a wrong password keeps them, and every other end gives them
// back. A session cookie in the wrong hands is therefore no faster way to guess the password than the login form:
// a blocked attempt is 429 rate_limited and costs no hash. The auth-ip and auth-hash buckets are for anonymous
// requests and don't apply (03 §7.3); the hash still waits for a slot of the semaphore (a full queue is 503
// server_busy).
//
// A user whose hash is NULL (an admin reset is pending) has no session, so this is only reached by a race; the
// attempt then costs the same one hash and fails.
func (s *Service) verifyOwnPassword(ctx context.Context, op string, user store.User, password string, m ReqMeta) error {
	return s.reauthenticate(ctx, op, user, password, m, false)
}

// reauthenticate is verifyOwnPassword. With change set it is the currentPassword of a password change, which also
// holds a token of the password-change bucket (03 §7.3, README F08): taken after the password's shape and before
// the two per-username buckets, so before the first hash, and kept when the password was right. Every other end
// gives it back: a refusal of another bucket, a full hash queue, a store or context error, and a wrong password,
// which is a failed password check that auth-user-ip and auth-user count as before.
//
// That bucket bounds the one path the other two can't: a correct password gives them their tokens back, so a
// member could otherwise loop POST /me/password with the own password (or two passwords in turn) and keep every
// hash slot busy. A change whose current password was right runs at most two hashes, so such changes run at most
// twice the bucket's tokens in hashes, and requests sent in parallel can't outrun it: each takes its token before
// it hashes. A wrong current password costs one hash and the tokens of the two per-username buckets, as on every
// path that asks for the password again (DeleteSelf's too), so the bucket would add nothing there; it also leaves
// the rule "five wrong tries from one address block that address" as it was.
func (s *Service) reauthenticate(ctx context.Context, op string, user store.User, password string, m ReqMeta,
	change bool,
) error {
	if password == "" {
		return errWrongPassword()
	}
	pw, err := normalizePassword(password)
	if err != nil {
		return errWrongPassword()
	}
	var token *changeToken
	if change {
		if token, err = s.takePasswordChange(ctx, user.ID); err != nil {
			return err
		}
		defer token.release()
	}
	attempt, err := s.reservePasswordAttempt(ctx, user.UsernameKey, true, m)
	if err != nil {
		return err
	}
	defer attempt.release()

	ok := false
	if user.PasswordHash != "" {
		ok, _, err = s.hasher.verify(ctx, pw, user.PasswordHash)
		if errors.Is(err, errBadHash) {
			s.log.LogAttrs(ctx, slog.LevelError, "a stored password hash is malformed; an admin reset repairs the account",
				slog.String("user_id", string(user.ID)))
			err = s.hasher.verifyDummy(ctx, pw)
		}
	} else {
		err = s.hasher.verifyDummy(ctx, pw)
	}
	if err != nil {
		return hashErr(op, err)
	}
	if !ok {
		attempt.failed()
		return errWrongPassword()
	}
	// A correct password from this address: like a login, it gives the auth-user token back and refills this
	// address's auth-user-ip bucket for the username. A password change keeps its token, whatever comes next.
	attempt.loggedIn()
	token.keep()
	return nil
}

// changeToken is the token one password change holds in the password-change bucket (reauthenticate). A nil
// *changeToken is the re-authentication of a call that has no such bucket, and its methods do nothing.
type changeToken struct {
	l    *limiter[store.UserID]
	user store.UserID
	kept bool
}

// keep spends the token: the current password was right, and the change goes on to hash the new one.
func (c *changeToken) keep() {
	if c != nil {
		c.kept = true
	}
}

// release gives the token back unless keep spent it. reauthenticate defers it, so no other end keeps a token.
func (c *changeToken) release() {
	if c != nil && !c.kept {
		c.kept = true
		c.l.refund(c.user)
	}
}

// takePasswordChange takes a token of the user's password-change bucket (03 §7.3). An empty bucket is 429
// rate_limited with retryAfter, before any hash; the first refusal of an episode logs one warn line with the user's
// ID. It writes no auth.throttled row: 03 §10 has no scope for this bucket, as for auth-user-ip and push-test.
func (s *Service) takePasswordChange(ctx context.Context, user store.UserID) (*changeToken, error) {
	v := s.throttle.passwordChange.take(user)
	if !v.OK {
		if v.First {
			s.log.LogAttrs(ctx, slog.LevelWarn, "password changes throttled", slog.String("bucket", "password-change"),
				slog.String("user_id", string(user)))
		}
		return nil, rateLimited(v)
	}
	return &changeToken{l: s.throttle.passwordChange, user: user}, nil
}

// ---- sessions ----

// RevokeSession deletes one of the principal's own web sessions (DELETE /api/v1/me/sessions/{id}; 03 §7.7 "Revoke
// one session"): the row and its push subscriptions go, session.revoked {name} is written, and after the commit
// the session leaves the cache and its connections are closed with ReasonSessionRevoked. A session that is not one
// of the user's own (unknown, another user's, or gone already) is *api.Error{not_found}.
//
// It may be the caller's own session: that is then a logout under another reason, and the caller's next request is
// unauthenticated.
func (s *Service) RevokeSession(ctx context.Context, p Principal, id store.SessionID, m ReqMeta) error {
	if err := webCaller("RevokeSession", p); err != nil {
		return err
	}
	now := s.now()
	err := s.db.Write(ctx, func(q *store.Q) error {
		me, err := loadSelf(q, p, now)
		if err != nil {
			return err
		}
		sess, ok := sessionNamed(me.sessions, id)
		if !ok {
			return api.NewError(api.CodeNotFound)
		}
		if _, err := q.DeleteSession(me.user.ID, sess.ID); err != nil {
			return err
		}
		return q.AppendAudit(store.AuditEntry{At: now, Action: auditSessionRevoked, Actor: userActor(me.user, m),
			TargetKind: targetSession, TargetID: string(sess.ID), TargetName: sess.Name,
			Detail: map[string]any{"name": sess.Name}})
	})
	if err != nil {
		return serviceErr("revoke session", err)
	}
	s.sessionsRevoked(p.UserID, []store.SessionID{id}, ReasonSessionRevoked)
	return nil
}

// RevokeOtherSessions deletes every web session of the principal's user except the caller's
// (POST /api/v1/me/sessions/revoke-others; 03 §7.7 "Sign out other browsers"). Their push subscriptions go with
// them; devices and a pending reset link stay. One session.revoked {name} row is written per session. After the
// commit the user's sessions leave the cache and the connections of exactly the deleted sessions are closed with
// ReasonSessionRevoked; the caller's stay.
//
// It returns how many browsers were signed out: the deleted sessions that were live until then, which are the ones
// GET /me/sessions lists besides the caller's. A row that had expired already and that the janitor has not pruned
// yet (03 §4.7) goes like the others, with its audit row, but was no session any more and is not counted.
func (s *Service) RevokeOtherSessions(ctx context.Context, p Principal, m ReqMeta) (int, error) {
	if err := webCaller("RevokeOtherSessions", p); err != nil {
		return 0, err
	}
	now := s.now()
	var rv revoked
	var live int
	err := s.db.Write(ctx, func(q *store.Q) error {
		me, err := loadSelf(q, p, now)
		if err != nil {
			return err
		}
		if rv, err = revokeOtherBrowsers.apply(q, me.user.ID, me.session.ID); err != nil {
			return err
		}
		live = 0
		actor := userActor(me.user, m)
		for _, id := range rv.sessions {
			sess, _ := sessionNamed(me.sessions, id) // deleted in this Write, so it was in the list read above
			if sessionLive(sess, now) {
				live++
			}
			err := q.AppendAudit(store.AuditEntry{At: now, Action: auditSessionRevoked, Actor: actor,
				TargetKind: targetSession, TargetID: string(id), TargetName: sess.Name,
				Detail: map[string]any{"name": sess.Name}})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, serviceErr("revoke other sessions", err)
	}
	s.revocationCommitted(rv)
	return live, nil
}

// LogoutEverywhere deletes every web session and every device of the principal's user, the caller's session
// included (POST /api/v1/auth/logout-everywhere; 03 §7.7 "Log out everywhere"). Their push subscriptions and the
// device codes the user decided go with them; a pending reset link stays. It writes auth.logout_everywhere
// {sessions, devices}. After the commit the user's sessions leave the cache and every connection of the user is
// closed with ReasonLoggedOut.
func (s *Service) LogoutEverywhere(ctx context.Context, p Principal, m ReqMeta) error {
	if err := webCaller("LogoutEverywhere", p); err != nil {
		return err
	}
	now := s.now()
	var rv revoked
	err := s.db.Write(ctx, func(q *store.Q) error {
		me, err := loadSelf(q, p, now)
		if err != nil {
			return err
		}
		if rv, err = revokeEverywhere.apply(q, me.user.ID, me.session.ID); err != nil {
			return err
		}
		return q.AppendAudit(store.AuditEntry{At: now, Action: auditLogoutEverywhere, Actor: userActor(me.user, m),
			TargetKind: targetUser, TargetID: string(me.user.ID), TargetName: me.user.Username,
			Detail: map[string]any{"sessions": len(rv.sessions), "devices": len(rv.devices)}})
	})
	if err != nil {
		return serviceErr("log out everywhere", err)
	}
	s.revocationCommitted(rv)
	return nil
}

// ---- password and account ----

// ChangePassword sets a new password for the principal's user (POST /api/v1/me/password; 03 §7.7 "Change own
// password"). In order:
//
//  1. the caller: an active account and a live session, else unauthenticated;
//  2. the new password's rules (CheckPassword, with the username) → 422 validation_failed {newPassword: code}, so
//     a request that can't succeed costs no hash;
//  3. the current password (reauthenticate): one that is missing or can't be a password → 403 wrong_password;
//     the user's password-change bucket (03 §7.3: 3, then 1 per 10 min) → 429 rate_limited; then as in
//     verifyOwnPassword → 429 rate_limited, 403 wrong_password, 503 server_busy. Only a right current password
//     keeps the bucket's token;
//  4. the new hash, outside the transaction → 503 server_busy;
//  5. one Write, which checks the caller again and that the password is still the one that was verified (a
//     parallel change or an admin reset in between answers wrong_password): the hash and password_changed_at; every
//     other web session, every device and a pending reset link are deleted (revokePasswordChanged); the caller's
//     session gets a new token (rotateForCaller); auth.password_changed is written.
//
// After the commit the user's sessions leave the cache and every connection of the user but those of the caller's
// session is closed with ReasonPasswordChanged.
//
// The returned LoginResult is the caller's session under its new token, which httpapi sets as the cookie. That is a
// rotation of 03 §7.4, like the daily one: the token the request arrived with works for 60 s more, so the requests
// the browser has under way and its other tabs are not signed out before the new cookie reaches them. The change
// counts as a use of the session (last_seen_at, last_ip and idle_expires_at move, and the cookie's Max-Age is a
// fresh 30 days, or what is left of the 180).
func (s *Service) ChangePassword(ctx context.Context, p Principal, current, next string, m ReqMeta) (LoginResult, error) {
	const op = "change password"
	if err := webCaller("ChangePassword", p); err != nil {
		return LoginResult{}, err
	}
	me, err := s.readSelf(ctx, op, p)
	if err != nil {
		return LoginResult{}, err
	}
	pw, err := CheckPassword(next, me.user.UsernameKey)
	if err != nil {
		var fe *FieldError
		if errors.As(err, &fe) {
			return LoginResult{}, validationFailed(map[string]string{fieldNewPassword: fe.Code})
		}
		return LoginResult{}, internalErr(op, err)
	}
	if err := s.reauthenticate(ctx, op, me.user, current, m, true); err != nil {
		return LoginResult{}, err
	}
	phc, err := s.hasher.hash(ctx, pw)
	if err != nil {
		return LoginResult{}, hashErr(op, err)
	}

	now := s.now()
	token := newToken(sessionTokenBytes)
	var rv revoked
	var res LoginResult
	err = s.db.Write(ctx, func(q *store.Q) error {
		cur, err := loadSelf(q, p, now)
		if err != nil {
			return err
		}
		if cur.user.PasswordHash != me.user.PasswordHash {
			return errWrongPassword()
		}
		user, sess := cur.user, cur.session
		if err := q.SetPasswordHash(user.ID, phc, now); err != nil {
			return err
		}
		user.PasswordHash, user.PasswordChangedAt, user.UpdatedAt = phc, now, now
		if rv, err = revokePasswordChanged.apply(q, user.ID, sess.ID); err != nil {
			return err
		}
		if err := s.rotateForCaller(q, sess, token, m, now); err != nil {
			return err
		}
		if err := q.TouchSession(sess.ID, ipString(m.IP), now, now.Add(sessionIdleTTL)); err != nil {
			return err
		}
		if sess, err = sessionByID(q, user.ID, sess.ID); err != nil {
			return err
		}
		res = LoginResult{User: user, Session: sess, Token: token}
		return q.AppendAudit(store.AuditEntry{At: now, Action: auditPasswordChanged, Actor: userActor(user, m),
			TargetKind: targetUser, TargetID: string(user.ID), TargetName: user.Username})
	})
	if err != nil {
		return LoginResult{}, serviceErr(op, err)
	}
	s.revocationCommitted(rv)
	return res, nil
}

// rotateForCaller gives the caller's session a new token inside a Write, as a rotation of 03 §7.4: the token the
// browser holds becomes the session's previous one and works for sessionPrevGrace more, so the requests it has
// under way and its other tabs are let in until the new cookie reaches them.
//
// The token the browser holds is the one the request arrived with (m.SessionToken). As a rule that is the session's
// current token, and one rotation does it. It is the previous one, still in its own grace, when a rotation came in
// between: the chain's daily rotation runs before the handler (03 §7.4), and the cookie it set never reaches the
// browser, because the handler's replaces it. A session has one previous token, so a plain rotation would give the
// grace to that token nobody holds and end the browser's own at once. The session is therefore rotated back to the
// request's token first, which the rotation to the new token then keeps; the token in between stops working at
// once. (The rotation of a parallel request in the last 60 s looks the same from here and is treated the same:
// the request's own token is the one the browser is known to have had.) A request that says nothing about its
// cookie (a caller other than httpapi) gets the plain rotation, and a previous token past its grace is not revived.
func (s *Service) rotateForCaller(q *store.Q, sess store.Session, token string, m ReqMeta, now time.Time) error {
	grace := now.Add(sessionPrevGrace)
	if tokenWellFormed(m.SessionToken, sessionTokenBytes) {
		held := s.keys.sessionTokenHash(m.SessionToken)
		if bytes.Equal(held, sess.PrevTokenHash) && !now.After(sess.PrevValidUntil) {
			if err := q.RotateSession(sess.ID, held, grace, now); err != nil {
				return err
			}
		}
	}
	return q.RotateSession(sess.ID, s.keys.sessionTokenHash(token), grace, now)
}

// DeleteSelf deletes the principal's own account (POST /api/v1/me/delete; 03 §7.7 "Delete user", 03 §7.11). In
// order:
//
//  1. the caller: an active account and a live session, else unauthenticated;
//  2. the last-admin rule: the only active admin can't leave → 409 last_admin, before the password is looked at,
//     so the answer costs no hash;
//  3. the password (verifyOwnPassword) → 403 wrong_password, 429 rate_limited, 503 server_busy;
//  4. one Write, which checks 1 and 2 again and that the password is still the one that was verified: the users
//     row is deleted, and its sessions, devices, push subscriptions and preferences and reset link go with it (the
//     cascade); invites and rooms the user created stay, without a creator. user.deleted {self: true} is written;
//     the row keeps the username as a snapshot.
//
// After the commit the user's sessions leave the cache and every connection of the user is closed with
// ReasonAccountDeleted. The username is free again.
func (s *Service) DeleteSelf(ctx context.Context, p Principal, password string, m ReqMeta) error {
	const op = "delete account"
	if err := webCaller("DeleteSelf", p); err != nil {
		return err
	}
	now := s.now()
	var me self
	err := s.db.Read(ctx, func(q *store.Q) error {
		var err error
		if me, err = loadSelf(q, p, now); err != nil {
			return err
		}
		return refuseLastAdmin(q, me.user)
	})
	if err != nil {
		return serviceErr(op, err)
	}
	if err := s.verifyOwnPassword(ctx, op, me.user, password, m); err != nil {
		return err
	}

	now = s.now()
	err = s.db.Write(ctx, func(q *store.Q) error {
		cur, err := loadSelf(q, p, now)
		if err != nil {
			return err
		}
		if cur.user.PasswordHash != me.user.PasswordHash {
			return errWrongPassword()
		}
		if err := refuseLastAdmin(q, cur.user); err != nil {
			return err
		}
		if err := q.DeleteUser(cur.user.ID); err != nil {
			return err
		}
		return q.AppendAudit(store.AuditEntry{At: now, Action: auditUserDeleted, Actor: userActor(cur.user, m),
			TargetKind: targetUser, TargetID: string(cur.user.ID), TargetName: cur.user.Username,
			Detail: map[string]any{"self": true}})
	})
	if err != nil {
		return serviceErr(op, err)
	}
	s.revocationCommitted(revokeAccountDeleted.deleted(p.UserID))
	return nil
}

// refuseLastAdmin is *api.Error{last_admin} when user is the only active admin (03 §7.11).
func refuseLastAdmin(q *store.Q, user store.User) error {
	last, err := lastActiveAdmin(q, user)
	if err != nil {
		return err
	}
	if last {
		return api.NewError(api.CodeLastAdmin)
	}
	return nil
}

// ---- devices ----

// RevokeDevice deletes one of the principal's own linked devices (DELETE /api/v1/me/devices/{id}; 03 §7.7 "Revoke
// device"): the row goes with its tokens and push subscriptions, device.revoked is written, and after the commit
// the device's connections are closed with ReasonDeviceRevoked. A device that is not one of the user's own is
// *api.Error{not_found}.
//
// Linking an app is M2, so no device exists before then and every ID answers not_found; the devices table and this
// path are there already, so that the Devices page's Revoke needs nothing new the day the list has rows. The device
// codes the user decided stay: another app may be waiting for its approval. M2 adds what belongs to the flow: the
// app's own "Sign out" (a bearer principal) and the refresh-token reuse row.
func (s *Service) RevokeDevice(ctx context.Context, p Principal, id store.DeviceID, m ReqMeta) error {
	if err := webCaller("RevokeDevice", p); err != nil {
		return err
	}
	now := s.now()
	err := s.db.Write(ctx, func(q *store.Q) error {
		me, err := loadSelf(q, p, now)
		if err != nil {
			return err
		}
		devices, err := q.ListDevices(me.user.ID)
		if err != nil {
			return err
		}
		var dev *store.Device
		for i := range devices {
			if devices[i].ID == id {
				dev = &devices[i]
			}
		}
		if dev == nil {
			return api.NewError(api.CodeNotFound)
		}
		if _, err := q.DeleteDevice(me.user.ID, dev.ID); err != nil {
			return err
		}
		return q.AppendAudit(store.AuditEntry{At: now, Action: auditDeviceRevoked, Actor: userActor(me.user, m),
			TargetKind: targetDevice, TargetID: string(dev.ID), TargetName: dev.Name})
	})
	if err != nil {
		return serviceErr("revoke device", err)
	}
	// No cache holds a device before M2 (the access-token cache comes with the flow), so only the connections are
	// left to close.
	if s.conns != nil {
		s.conns.CloseConnections(ConnSelector{UserID: p.UserID, DeviceID: id}, ReasonDeviceRevoked)
	}
	return nil
}
