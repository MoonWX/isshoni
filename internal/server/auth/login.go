package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/text/secure/precis"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// Login and logout (03 §7.2–7.4, §7.7), with the throttles of 03 §7.3 and the audit rows of 03 §10.

// Audit actions written here (03 §10).
const (
	auditLogin       = "auth.login"
	auditLoginFailed = "auth.login_failed"
	auditThrottled   = "auth.throttled"
	auditLogout      = "auth.logout"
)

// Reasons of an auth.login_failed row (03 §10).
const (
	failWrongPassword   = "wrong_password"
	failUnknownUser     = "unknown_user"
	failAccountPending  = "account_pending"
	failAccountDisabled = "account_disabled"
)

// Scopes of an auth.throttled row (03 §7.3).
const (
	scopeIP       = "ip"
	scopeUsername = "username"
	scopeHash     = "hash"
	scopeGlobal   = "global"
)

// Audit target kinds (03 §5).
const (
	targetUser    = "user"
	targetSession = "session"
)

// outcomeDenied is the outcome of the audit rows of a refused attempt: every auth.login_failed and auth.throttled
// row (03 §5, §10). The other rows keep the store's default, "ok".
const outcomeDenied = string(api.AuditOutcomeDenied)

// hashBusyRetryAfter is the Retry-After of a full hash queue, in seconds (03 §7.2).
const hashBusyRetryAfter = 5

// ---- errors ----

// rateLimited is 429 rate_limited with the seconds until the bucket's next token (also sent as Retry-After).
func rateLimited(v verdict) error {
	return &api.Error{Code: api.CodeRateLimited, RetryAfter: v.retryAfterSeconds()}
}

// validationFailed is 422 validation_failed with its field codes.
func validationFailed(fields map[string]string) error {
	return &api.Error{Code: api.CodeValidationFailed, Fields: fields}
}

// hashErr turns a hasher error into a service error: a full queue is 503 server_busy with Retry-After: 5.
func hashErr(op string, err error) error {
	if errors.Is(err, errHashBusy) {
		return &api.Error{Code: api.CodeServerBusy, RetryAfter: hashBusyRetryAfter}
	}
	return internalErr(op, err)
}

// statusErr is the answer for a correct password on an account that is not active.
func statusErr(st store.UserStatus) error {
	if st == store.StatusPending {
		return api.NewError(api.CodeAccountPending)
	}
	return api.NewError(api.CodeAccountDisabled)
}

// ---- audit helpers ----

// anonymous is the audit actor of a request nobody is logged in for: no name, only the IP.
func anonymous(m ReqMeta) store.Actor {
	return store.Actor{Kind: store.ActorAnonymous, IP: ipString(m.IP)}
}

// userActor is the audit actor of a user acting from the request's IP.
func userActor(u store.User, m ReqMeta) store.Actor {
	return store.Actor{Kind: store.ActorUser, UserID: u.ID, Name: u.Username, IP: ipString(m.IP)}
}

// auditAlone writes an audit row that has no other write: failed logins and throttle events (03 §10). It is best
// effort: the caller's answer does not depend on it, so a failure is only logged.
func (s *Service) auditAlone(ctx context.Context, e store.AuditEntry) {
	err := s.db.Write(ctx, func(q *store.Q) error { return q.AppendAudit(e) })
	if err != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "could not write an audit row",
			slog.String("action", e.Action), slog.String("err", err.Error()))
	}
}

// ---- throttles (03 §7.3) ----

// ipKeyText is the key of an auth.throttled {scope: ip} row: the address the bucket is for, which is the IPv4
// address itself or the IPv6 /64.
func ipKeyText(ip netip.Addr) string {
	k := IPKey(ip)
	if !k.IsValid() {
		return ""
	}
	if k.Addr().Is4() {
		return k.Addr().String()
	}
	return k.String()
}

// logBlocked logs a blocked attempt: one warn line with remote_ip and no username, at most once per IPKey per
// minute (03 §7.3, §11). No info line carries a client IP.
func (s *Service) logBlocked(ctx context.Context, m ReqMeta, bucket string) {
	if !s.throttle.blockLog.take(IPKey(m.IP)).OK {
		return
	}
	s.log.LogAttrs(ctx, slog.LevelWarn, "auth attempt throttled",
		slog.String("bucket", bucket), slog.String("remote_ip", ipString(m.IP)))
}

// takeAuthIP is the auth-ip bucket: every attempt at login, register, setup, reset and invite check takes a token,
// first and before any other work. The first block of an episode writes an auth.throttled {scope: ip} row.
func (s *Service) takeAuthIP(ctx context.Context, m ReqMeta) error {
	v := s.throttle.authIP.take(IPKey(m.IP))
	if v.OK {
		return nil
	}
	if v.First {
		s.auditAlone(ctx, store.AuditEntry{At: s.now(), Action: auditThrottled, Outcome: outcomeDenied,
			Actor: anonymous(m), Detail: map[string]any{"scope": scopeIP, "key": ipKeyText(m.IP)}})
	}
	s.logBlocked(ctx, m, "auth-ip")
	return rateLimited(v)
}

// takeHashBudget is the auth-hash bucket: every public request that reaches the hash takes a token, last, after all
// other buckets and before the semaphore. An empty bucket is 503 server_busy with the seconds until the next token
// (at least 1), and no hashing. The first block of an episode writes an auth.throttled {scope: hash} row.
func (s *Service) takeHashBudget(ctx context.Context, m ReqMeta) error {
	v := s.throttle.authHash.take(struct{}{})
	if v.OK {
		return nil
	}
	if v.First {
		s.auditAlone(ctx, store.AuditEntry{At: s.now(), Action: auditThrottled, Outcome: outcomeDenied,
			Actor: anonymous(m), Detail: map[string]any{"scope": scopeHash}})
	}
	return &api.Error{Code: api.CodeServerBusy, RetryAfter: v.retryAfterSeconds()}
}

// passwordAttempt is the place one login holds in the two per-username buckets of failed password checks while its
// password is verified (03 §7.3). The buckets count failures, but an attempt takes its tokens before the hash, not
// after the verdict: attempts that arrive while earlier ones are still hashing then find those in the buckets, so
// guesses sent in parallel cannot outrun the limits. How the attempt ends decides what becomes of the tokens:
//   - a wrong password or an unknown username keeps them (failed);
//   - a completed login gives the auth-user token back and refills this address's auth-user-ip bucket (loggedIn);
//   - every other end gives both back (release): an empty hash budget, a full hash queue, a store or context error,
//     and a correct password for an account that is not active. None of them is a failed password check.
//
// The password a logged-in user types again (verifyOwnPassword in selfservice.go) holds its place the same way.
type passwordAttempt struct {
	t      *throttles
	userIP userIPKey
	user   string
	// holdsUser is false for a known IP that went on while auth-user was empty: it took no auth-user token.
	holdsUser bool
	settled   bool
}

// failed keeps the tokens: the attempt was a failed password check.
func (a *passwordAttempt) failed() { a.settled = true }

// loggedIn ends an attempt that became a session: auth-user is as it was before the attempt, so a friend's login
// never hands an attacker a fresh budget, and this address's auth-user-ip bucket for the username is full again.
func (a *passwordAttempt) loggedIn() {
	if a.settled {
		return
	}
	a.settled = true
	if a.holdsUser {
		a.t.authUser.refund(a.user)
	}
	a.t.authUserIP.reset(a.userIP)
}

// release gives the tokens back unless failed or loggedIn settled the attempt. Login defers it, so no exit can leave
// a token behind for an attempt that was not a failed password check.
func (a *passwordAttempt) release() {
	if a.settled {
		return
	}
	a.settled = true
	a.t.authUserIP.refund(a.userIP)
	if a.holdsUser {
		a.t.authUser.refund(a.user)
	}
}

// reservePasswordAttempt takes the attempt's tokens from the two per-username buckets before any hashing:
//   - auth-user-ip (this username from this address): a hard block. It writes no auth.throttled row: the
//     auth.login_failed rows before it already show the address, and a row per pair would let a stranger with many
//     addresses flood the log;
//   - auth-user (this username from any address): when it is empty, only the user's known IPs get through, the
//     last_ip (IPv4) or its /64 (IPv6) of that user's live sessions. That is one indexed read, done only while the
//     bucket is empty. The first refusal of an episode writes auth.throttled {scope: username, key: <username>},
//     and only if that user exists.
//
// A refused attempt holds no token: the auth-user-ip token goes back when auth-user refuses.
func (s *Service) reservePasswordAttempt(ctx context.Context, key string, isKey bool, m ReqMeta) (passwordAttempt, error) {
	now := s.now()
	a := passwordAttempt{t: s.throttle, userIP: userIPKey{user: key, ip: IPKey(m.IP)}, user: key}
	if v := s.throttle.authUserIP.take(a.userIP); !v.OK {
		s.logBlocked(ctx, m, "auth-user-ip")
		return passwordAttempt{}, rateLimited(v)
	}
	v := s.throttle.authUser.take(key)
	if v.OK {
		a.holdsUser = true
		return a, nil
	}
	if v.First && isKey {
		s.auditUsernameThrottled(ctx, key, m, now)
	}
	if isKey && s.fromKnownIP(ctx, key, m.IP, now) {
		return a, nil
	}
	s.throttle.authUserIP.refund(a.userIP)
	s.logBlocked(ctx, m, "auth-user")
	return passwordAttempt{}, rateLimited(v)
}

// auditUsernameThrottled writes auth.throttled {scope: username, key: <the account's username>} if an account with
// that key exists. The name a stranger typed for an unknown account is never stored (03 §10).
func (s *Service) auditUsernameThrottled(ctx context.Context, key string, m ReqMeta, now time.Time) {
	err := s.db.Write(ctx, func(q *store.Q) error {
		u, err := q.UserByUsernameKey(key)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return q.AppendAudit(store.AuditEntry{At: now, Action: auditThrottled, Outcome: outcomeDenied,
			Actor: anonymous(m), Detail: map[string]any{"scope": scopeUsername, "key": u.Username}})
	})
	if err != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "could not write an audit row",
			slog.String("action", auditThrottled), slog.String("err", err.Error()))
	}
}

// fromKnownIP reports whether ip is one of the user's known IPs (03 §7.3). A failed read counts as unknown: the
// attempt stays blocked.
func (s *Service) fromKnownIP(ctx context.Context, key string, ip netip.Addr, now time.Time) bool {
	if !ip.IsValid() {
		return false
	}
	var ips []string
	err := s.db.Read(ctx, func(q *store.Q) error {
		var err error
		ips, err = q.KnownIPs(key, now)
		return err
	})
	if err != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "could not read the known IPs of a throttled account",
			slog.String("err", err.Error()))
		return false
	}
	want := IPKey(ip)
	for _, raw := range ips {
		if a, err := netip.ParseAddr(raw); err == nil && IPKey(a) == want {
			return true
		}
	}
	return false
}

// auditLoginFailed writes an auth.login_failed {reason} row: actor anonymous with the IP, target the account only
// if it exists. The rows have a server-wide cap of 600 per hour; beyond it, one auth.throttled {scope: global} row
// is written per hour (03 §7.3).
func (s *Service) auditLoginFailed(ctx context.Context, target *store.User, reason string, m ReqMeta) {
	now := s.now()
	if !s.throttle.loginFailedAudit.take(struct{}{}).OK {
		if s.throttle.globalThrottledAudit.take(struct{}{}).OK {
			s.auditAlone(ctx, store.AuditEntry{At: now, Action: auditThrottled, Outcome: outcomeDenied,
				Actor: anonymous(m), Detail: map[string]any{"scope": scopeGlobal}})
		}
		return
	}
	e := store.AuditEntry{At: now, Action: auditLoginFailed, Outcome: outcomeDenied, Actor: anonymous(m),
		Detail: map[string]any{"reason": reason}}
	if target != nil {
		e.TargetKind, e.TargetID, e.TargetName = targetUser, string(target.ID), target.Username
	}
	s.auditAlone(ctx, e)
}

// ---- login ----

// loginKey returns the comparison key a login looks the account up by, the key NormalizeUsername gives a valid
// name (03 §7.1 rule 5), without the shape rules: whether a name may be registered is not a login's question.
// isKey is false when the input has no key at all (PRECIS refuses it). No account can have such a name; the
// returned value then only keys the throttle buckets, and starts with a NUL so that it never equals a real key.
func loginKey(username string) (key string, isKey bool) {
	trimmed := strings.TrimSpace(username)
	display, err := precis.UsernameCasePreserved.String(trimmed)
	if err == nil {
		if key, err = precis.UsernameCaseMapped.CompareKey(display); err == nil && key != "" {
			return key, true
		}
	}
	return "\x00" + trimmed, false
}

// Login checks a username and password and creates a session (03 §7.2–7.4). In order:
//
//  1. the auth-ip bucket, before any other work → 429 rate_limited;
//  2. the two fields: a missing username or password is required, more than 128 bytes of username or 1024 bytes of
//     password is too_long, a password PRECIS refuses (a control character) is invalid → 422 validation_failed;
//  3. the auth-user-ip and auth-user buckets of this username → 429, with no hashing and no DB access other than
//     the known-IP read. The attempt takes one token from each here, before the hash (passwordAttempt), and gets
//     them back at every exit below that is not a failed password check;
//  4. the auth-hash budget → 503 server_busy;
//  5. exactly one password verification: against the account's hash, or against the dummy hash for an unknown
//     username and for an account whose hash is NULL (an admin reset is pending), so every attempt costs the same;
//  6. a wrong password or unknown username → 401 invalid_credentials, the same answer for both; it keeps the two
//     tokens and writes auth.login_failed;
//  7. only after a correct password: a pending account → 403 account_pending, a disabled one → 403
//     account_disabled;
//  8. one Write: the new hash if the stored one had other argon2 parameters (computed before the transaction), the
//     last-login time, the session (openSession: the old cookie's session goes, the cap of 50 applies) and the
//     auth.login row. Afterwards this address's auth-user-ip bucket for the username is refilled; auth-user is as
//     it was before the attempt, so a friend's login never hands an attacker a fresh budget.
//
// The returned token is the raw cookie value; httpapi sets it with SetSessionCookie.
func (s *Service) Login(ctx context.Context, username, password string, m ReqMeta) (LoginResult, error) {
	if err := s.takeAuthIP(ctx, m); err != nil {
		return LoginResult{}, err
	}

	fields := map[string]string{}
	switch {
	case len(username) > usernameMaxBytes:
		fields["username"] = FieldTooLong
	case strings.TrimSpace(username) == "":
		fields["username"] = FieldRequired
	}
	var pw string
	switch {
	case len(password) > passwordMaxBytes:
		fields["password"] = FieldTooLong
	case password == "":
		fields["password"] = FieldRequired
	default:
		var err error
		if pw, err = normalizePassword(password); err != nil {
			fields["password"] = FieldInvalid
		}
	}
	if len(fields) > 0 {
		return LoginResult{}, validationFailed(fields)
	}

	key, isKey := loginKey(username)
	attempt, err := s.reservePasswordAttempt(ctx, key, isKey, m)
	if err != nil {
		return LoginResult{}, err
	}
	defer attempt.release()
	if err := s.takeHashBudget(ctx, m); err != nil {
		return LoginResult{}, err
	}

	var user store.User
	found := false
	if isKey {
		err := s.db.Read(ctx, func(q *store.Q) error {
			var err error
			user, err = q.UserByUsernameKey(key)
			return err
		})
		switch {
		case err == nil:
			found = true
		case !errors.Is(err, store.ErrNotFound):
			return LoginResult{}, internalErr("login", err)
		}
	}

	var ok, rehash bool
	if found && user.PasswordHash != "" {
		ok, rehash, err = s.hasher.verify(ctx, pw, user.PasswordHash)
		if errors.Is(err, errBadHash) {
			// verify refuses a malformed stored hash before hashing. Nobody can log in to that account; the attempt
			// still costs one hash, like every other.
			s.log.LogAttrs(ctx, slog.LevelError, "a stored password hash is malformed; an admin reset repairs the account",
				slog.String("user_id", string(user.ID)))
			err = s.hasher.verifyDummy(ctx, pw)
		}
	} else {
		err = s.hasher.verifyDummy(ctx, pw)
	}
	if err != nil {
		return LoginResult{}, hashErr("login", err)
	}
	if !ok {
		attempt.failed()
		if found {
			s.auditLoginFailed(ctx, &user, failWrongPassword, m)
		} else {
			s.auditLoginFailed(ctx, nil, failUnknownUser, m)
		}
		return LoginResult{}, api.NewError(api.CodeInvalidCredentials)
	}
	if user.Status != store.StatusActive {
		reason := failAccountDisabled
		if user.Status == store.StatusPending {
			reason = failAccountPending
		}
		s.auditLoginFailed(ctx, &user, reason, m)
		return LoginResult{}, statusErr(user.Status)
	}

	// The rehash runs outside the transaction (03 decision 4). When the hash queue is full, the login goes on with
	// the old hash and the next login rehashes.
	newHash := ""
	if rehash {
		if h, err := s.hasher.hash(ctx, pw); err == nil {
			newHash = h
		} else if !errors.Is(err, errHashBusy) {
			return LoginResult{}, internalErr("login", err)
		}
	}

	now := s.now()
	token := newToken(sessionTokenBytes)
	var ns newSession
	err = s.db.Write(ctx, func(q *store.Q) error {
		// The account may have changed since the password was verified: an admin reset or a password change between
		// the read and this Write must not end in a session for the old password.
		cur, err := q.UserByID(user.ID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && cur.PasswordHash != user.PasswordHash) {
			return api.NewError(api.CodeInvalidCredentials)
		}
		if err != nil {
			return err
		}
		if cur.Status != store.StatusActive {
			return statusErr(cur.Status)
		}
		if newHash != "" {
			if err := q.SetPasswordHash(cur.ID, newHash, now); err != nil {
				return err
			}
			cur.PasswordHash, cur.PasswordChangedAt, cur.UpdatedAt = newHash, now, now
		}
		if err := q.TouchLogin(cur.ID, now); err != nil {
			return err
		}
		cur.LastLoginAt = now
		if err := s.openSession(q, cur, token, m, now, &ns); err != nil {
			return err
		}
		sess := ns.result.Session
		return q.AppendAudit(store.AuditEntry{At: now, Action: auditLogin, Actor: userActor(cur, m),
			TargetKind: targetSession, TargetID: string(sess.ID), TargetName: sess.Name})
	})
	if err != nil {
		return LoginResult{}, serviceErr("login", err)
	}
	attempt.loggedIn()
	s.sessionOpened(&ns)
	return ns.result, nil
}

// ---- logout ----

// Logout deletes the principal's session, with its push subscriptions (03 §7.7 "Logout"), and writes auth.logout.
// After the commit the session leaves the cache and its WebSocket connections are closed with ReasonLoggedOut. It is
// idempotent: a session that is already gone is no error. (M2: a bearer principal's device.)
func (s *Service) Logout(ctx context.Context, p Principal, m ReqMeta) error {
	if err := webCaller("Logout", p); err != nil {
		return err
	}
	now := s.now()
	err := s.db.Write(ctx, func(q *store.Q) error {
		sess, err := sessionByID(q, p.UserID, p.SessionID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := q.DeleteSession(p.UserID, p.SessionID); err != nil {
			return err
		}
		return q.AppendAudit(store.AuditEntry{At: now, Action: auditLogout, Actor: ActorOf(p, m.IP),
			TargetKind: targetSession, TargetID: string(sess.ID), TargetName: sess.Name})
	})
	if err != nil {
		return serviceErr("logout", err)
	}
	s.sessionsRevoked(p.UserID, []store.SessionID{p.SessionID}, ReasonLoggedOut)
	return nil
}
