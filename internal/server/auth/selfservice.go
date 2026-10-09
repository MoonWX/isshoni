package auth

import (
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

// sessionNoGrace is the prev_valid_until of a rotation whose old token stops working at once: a time that no clock
// reaches again, so a clock that steps back cannot revive the token either.
var sessionNoGrace = time.Unix(0, 0)

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
	if password == "" {
		return errWrongPassword()
	}
	pw, err := normalizePassword(password)
	if err != nil {
		return errWrongPassword()
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
	// address's auth-user-ip bucket for the username.
	attempt.loggedIn()
	return nil
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

// RevokeOtherSessions deletes every web session of the principal's user except the caller's and returns how many
// there were (POST /api/v1/me/sessions/revoke-others; 03 §7.7 "Sign out other browsers"). Their push subscriptions
// go with them; devices and a pending reset link stay. One session.revoked {name} row is written per session.
// After the commit the user's sessions leave the cache and the connections of exactly the deleted sessions are
// closed with ReasonSessionRevoked; the caller's stay.
func (s *Service) RevokeOtherSessions(ctx context.Context, p Principal, m ReqMeta) (int, error) {
	if err := webCaller("RevokeOtherSessions", p); err != nil {
		return 0, err
	}
	now := s.now()
	var rv revoked
	err := s.db.Write(ctx, func(q *store.Q) error {
		me, err := loadSelf(q, p, now)
		if err != nil {
			return err
		}
		if rv, err = revokeOtherBrowsers.apply(q, me.user.ID, me.session.ID); err != nil {
			return err
		}
		actor := userActor(me.user, m)
		for _, id := range rv.sessions {
			sess, _ := sessionNamed(me.sessions, id) // deleted in this Write, so it was in the list read above
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
	return len(rv.sessions), nil
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
//  3. the current password (verifyOwnPassword) → 403 wrong_password, 429 rate_limited, 503 server_busy;
//  4. the new hash, outside the transaction → 503 server_busy;
//  5. one Write, which checks the caller again and that the password is still the one that was verified (a
//     parallel change or an admin reset in between answers wrong_password): the hash and password_changed_at; every
//     other web session, every device and a pending reset link are deleted (revokePasswordChanged); the caller's
//     session gets a new token; auth.password_changed is written.
//
// After the commit the user's sessions leave the cache and every connection of the user but those of the caller's
// session is closed with ReasonPasswordChanged.
//
// The returned LoginResult is the caller's session under its new token, which httpapi sets as the cookie. The old
// token stops working at once, without the 60 s grace of the daily rotation: changing the password is how a user
// shuts out whoever else may hold this cookie. The change counts as a use of the session (last_seen_at, last_ip and
// idle_expires_at move, and the cookie's Max-Age is a fresh 30 days, or what is left of the 180).
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
	if err := s.verifyOwnPassword(ctx, op, me.user, current, m); err != nil {
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
		if err := q.RotateSession(sess.ID, s.keys.sessionTokenHash(token), sessionNoGrace, now); err != nil {
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
