package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// Self-service (README S58). The rows of 03 §7.7 are in the matrix of revoke_test.go; these tests are about each
// call's own rules, answers and audit rows.

const newPassword = "another long passphrase"

// principal is the Principal of a login.
func principal(res LoginResult) Principal { return principalOf(res.Session, res.User) }

// userRow reads a user back, or nil.
func (e *svcEnv) userRow(id store.UserID) *store.User {
	e.t.Helper()
	var out *store.User
	e.read(func(q *store.Q) error {
		u, err := q.UserByID(id)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		out = &u
		return err
	})
	return out
}

func TestRevokeSession(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	alex, sam := e.addUser("Alex"), e.addUser("Sam")
	mine, other := e.login("Alex", ipA), e.login("Alex", ipB)
	sams := e.login("Sam", ipB)
	p := principal(mine)
	e.conns.take()
	e.advance(time.Minute)
	now := e.clk.now()

	// Only the caller's own sessions exist for this call: an unknown ID and another user's are not found.
	for _, id := range []store.SessionID{"", "nope", "aaaaaaaaaaaa", sams.Session.ID} {
		wantCode(t, e.svc.RevokeSession(ctx, p, id, meta(ipC)), api.CodeNotFound)
	}
	if e.session(sam.ID, sams.Session.ID) == nil || len(e.sessions(alex.ID)) != 2 || len(e.conns.take()) != 0 ||
		e.auditCount(auditSessionRevoked) != 0 {
		t.Fatal("a refused revocation changed something")
	}

	if err := e.svc.RevokeSession(ctx, p, other.Session.ID, meta(ipC)); err != nil {
		t.Fatal(err)
	}
	rows := e.audit(auditSessionRevoked)
	if len(rows) != 1 {
		t.Fatalf("%d session.revoked rows, want 1", len(rows))
	}
	if r := rows[0]; r.Actor != (store.Actor{Kind: store.ActorUser, UserID: alex.ID, Name: "Alex", IP: ipC}) ||
		r.TargetKind != "session" || r.TargetID != string(other.Session.ID) || r.TargetName != "Chrome on Windows" ||
		!r.At.Equal(now) || fmt.Sprint(r.Detail) != "map[name:Chrome on Windows]" || r.Outcome != "ok" {
		t.Errorf("session.revoked row = %+v", r)
	}
	// It is gone already: a second call finds nothing, and writes nothing.
	wantCode(t, e.svc.RevokeSession(ctx, p, other.Session.ID, meta(ipC)), api.CodeNotFound)
	if n := e.auditCount(auditSessionRevoked); n != 1 {
		t.Errorf("%d session.revoked rows after a second call, want 1", n)
	}
	e.conns.take()

	// The caller's own session is revoked like any other; the caller is nobody afterwards.
	if err := e.svc.RevokeSession(ctx, p, mine.Session.ID, meta(ipC)); err != nil {
		t.Fatal(err)
	}
	want := []closedConn{{ConnSelector{UserID: alex.ID, SessionID: mine.Session.ID}, ReasonSessionRevoked}}
	if calls := e.conns.take(); fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("CloseConnections calls = %+v, want %+v", calls, want)
	}
	_, err := e.svc.Authenticate(e.request(http.MethodGet, mine.Token))
	wantCode(t, err, api.CodeUnauthenticated)
	wantCode(t, e.svc.RevokeSession(ctx, p, mine.Session.ID, meta(ipC)), api.CodeUnauthenticated)
}

func TestRevokeOtherSessions(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	alex := e.addUser("Alex")
	mine := e.login("Alex", ipA)
	p := principal(mine)
	e.conns.take()

	// Nothing to sign out: zero, no row, no call.
	if n, err := e.svc.RevokeOtherSessions(ctx, p, meta(ipA)); err != nil || n != 0 {
		t.Fatalf("RevokeOtherSessions with one session = %d, %v", n, err)
	}
	if calls := e.conns.take(); len(calls) != 0 || e.auditCount(auditSessionRevoked) != 0 {
		t.Errorf("nothing was revoked, but: calls %+v, %d audit rows", calls, e.auditCount(auditSessionRevoked))
	}
	if _, err := e.svc.Authenticate(e.request(http.MethodGet, mine.Token)); err != nil {
		t.Fatalf("the caller's session afterwards: %v", err)
	}

	// Two other browsers, one of them expired but not pruned yet: both rows go, each with its audit row and its
	// CloseConnections call. The answer counts the one that was a session until now, which is what the list showed.
	second := e.login("Alex", ipB)
	var stale store.Session
	e.write(func(q *store.Q) error {
		past := e.clk.now().Add(-40 * 24 * time.Hour)
		stale = store.Session{UserID: alex.ID, TokenHash: []byte("stale-hash"), Name: "Safari on iPhone", CreatedAt: past,
			LastIP: ipC, IdleExpiresAt: past.Add(sessionIdleTTL), ExpiresAt: past.Add(sessionMaxTTL)}
		return q.CreateSession(&stale)
	})
	e.advance(time.Minute)
	now := e.clk.now()
	n, err := e.svc.RevokeOtherSessions(ctx, p, meta(ipC))
	if err != nil || n != 1 {
		t.Fatalf("RevokeOtherSessions = %d, %v; want 1 (the live session, not the expired row)", n, err)
	}
	if got := e.sessions(alex.ID); len(got) != 1 || got[0].ID != mine.Session.ID {
		t.Errorf("sessions afterwards = %+v, want only the caller's", got)
	}
	// One session.revoked row per session, each with that session's name.
	names := map[string]string{}
	for _, r := range e.audit(auditSessionRevoked) {
		if r.Actor != (store.Actor{Kind: store.ActorUser, UserID: alex.ID, Name: "Alex", IP: ipC}) || r.TargetKind != "session" ||
			!r.At.Equal(now) || r.TargetName != fmt.Sprint(r.Detail["name"]) {
			t.Errorf("session.revoked row = %+v", r)
		}
		names[r.TargetID] = r.TargetName
	}
	wantNames := map[string]string{string(second.Session.ID): "Chrome on Windows", string(stale.ID): "Safari on iPhone"}
	if fmt.Sprint(names) != fmt.Sprint(wantNames) {
		t.Errorf("session.revoked rows name %v, want %v", names, wantNames)
	}
	if calls := e.conns.take(); len(calls) != 2 {
		t.Errorf("CloseConnections calls = %+v, want one per revoked session", calls)
	}
	_, err = e.svc.Authenticate(e.request(http.MethodGet, second.Token))
	wantCode(t, err, api.CodeUnauthenticated)

	// Only an expired row besides the caller's: it goes like any other, and no browser was signed out.
	e.write(func(q *store.Q) error {
		past := e.clk.now().Add(-40 * 24 * time.Hour)
		stale = store.Session{UserID: alex.ID, TokenHash: []byte("stale-hash-2"), Name: "Firefox on Linux", CreatedAt: past,
			LastIP: ipC, IdleExpiresAt: past.Add(sessionIdleTTL), ExpiresAt: past.Add(sessionMaxTTL)}
		return q.CreateSession(&stale)
	})
	if n, err := e.svc.RevokeOtherSessions(ctx, p, meta(ipC)); err != nil || n != 0 {
		t.Fatalf("RevokeOtherSessions with only an expired row = %d, %v; want 0", n, err)
	}
	want := []closedConn{{ConnSelector{UserID: alex.ID, SessionID: stale.ID}, ReasonSessionRevoked}}
	if calls := e.conns.take(); fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("CloseConnections calls = %+v, want %+v", calls, want)
	}
	if got, rows := e.sessions(alex.ID), e.auditCount(auditSessionRevoked); len(got) != 1 || got[0].ID != mine.Session.ID || rows != 3 {
		t.Errorf("after the expired row went: sessions %+v, %d session.revoked rows; want the caller's and 3", got, rows)
	}
}

func TestLogoutEverywhere(t *testing.T) {
	f := newRevFixture(t)
	ctx := context.Background()
	f.advance(time.Minute)
	now := f.clk.now()
	if err := f.svc.LogoutEverywhere(ctx, f.caller, meta(ipC)); err != nil {
		t.Fatal(err)
	}
	rows := f.audit(auditLogoutEverywhere)
	if len(rows) != 1 {
		t.Fatalf("%d auth.logout_everywhere rows, want 1", len(rows))
	}
	if r := rows[0]; r.Actor != (store.Actor{Kind: store.ActorUser, UserID: f.alex.ID, Name: "Alex", IP: ipC}) ||
		r.TargetKind != "user" || r.TargetID != string(f.alex.ID) || r.TargetName != "Alex" || !r.At.Equal(now) ||
		fmt.Sprint(r.Detail) != "map[devices:1 sessions:3]" {
		t.Errorf("auth.logout_everywhere row = %+v", r)
	}
	// The caller went with everything else: a second call is nobody's.
	f.conns.take()
	wantCode(t, f.svc.LogoutEverywhere(ctx, f.caller, meta(ipC)), api.CodeUnauthenticated)
	if n, calls := f.auditCount(auditLogoutEverywhere), f.conns.take(); n != 1 || len(calls) != 0 {
		t.Errorf("a second call wrote %d rows in all and closed %+v", n, calls)
	}
	// The account itself is untouched: the password still logs in.
	f.login("Alex", ipA)
}

func TestChangePassword(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	alex := e.addUser("Alex")
	mine, other := e.login("Alex", ipA), e.login("Alex", ipB)
	p := principal(mine)
	e.conns.take()
	e.hashes.Store(0)
	unchanged := func(when string) {
		t.Helper()
		if u := e.userRow(alex.ID); u.PasswordHash != alex.PasswordHash || len(e.sessions(alex.ID)) != 2 ||
			len(e.conns.take()) != 0 || e.auditCount(auditPasswordChanged) != 0 {
			t.Fatalf("%s: the refused change changed something", when)
		}
	}

	// The new password's rules come first and cost no hash, whatever the current password is.
	for next, code := range map[string]string{
		"":                       FieldRequired,
		"short":                  FieldTooShort,
		"PassWord1":              FieldTooCommon,
		"ALEX":                   FieldTooShort,
		"with a\x00control":      FieldInvalid,
		strings.Repeat("x", 129): FieldTooLong,
	} {
		for _, current := range []string{testPassword, "not the password", ""} {
			_, err := e.svc.ChangePassword(ctx, p, current, next, meta(ipA))
			ae := wantCode(t, err, api.CodeValidationFailed)
			if fmt.Sprint(ae.Fields) != "map[newPassword:"+code+"]" {
				t.Errorf("ChangePassword to %q: fields %v, want newPassword: %s", next, ae.Fields, code)
			}
		}
	}
	if n := e.hashes.Load(); n != 0 {
		t.Errorf("refused new passwords hashed %d times, want 0", n)
	}
	unchanged("a bad new password")

	// A current password that can't be one is wrong without a hash and without a token; a wrong one costs one hash
	// and one token of each bucket of failed password checks.
	for _, current := range []string{"", strings.Repeat("x", passwordMaxBytes+1), "a\x00b"} {
		_, err := e.svc.ChangePassword(ctx, p, current, newPassword, meta(ipA))
		wantCode(t, err, api.CodeWrongPassword)
	}
	if n, held := e.hashes.Load(), e.heldTokens(); n != 0 || held != [2]int{} {
		t.Errorf("impossible current passwords: %d hashes, %v keys with tokens taken; want none", n, held)
	}
	_, err := e.svc.ChangePassword(ctx, p, "not the password", newPassword, meta(ipA))
	wantCode(t, err, api.CodeWrongPassword)
	if n, held := e.hashes.Load(), e.heldTokens(); n != 1 || held != [2]int{1, 1} {
		t.Errorf("a wrong current password: %d hashes, %v keys with tokens taken; want 1 and [1 1]", n, held)
	}
	if n := e.auditCount(auditLoginFailed); n != 0 {
		t.Errorf("%d auth.login_failed rows for a wrong current password: it is no login", n)
	}
	unchanged("a wrong current password")

	// The change.
	e.hashes.Store(0)
	e.advance(time.Hour)
	now := e.clk.now()
	res, err := e.svc.ChangePassword(ctx, p, testPassword, newPassword, meta(ipC))
	if err != nil {
		t.Fatal(err)
	}
	if n := e.hashes.Load(); n != 2 {
		t.Errorf("the change hashed %d times, want 2 (the current password, the new one)", n)
	}
	u := e.userRow(alex.ID)
	if u.PasswordHash == alex.PasswordHash || strings.Contains(u.PasswordHash, newPassword) || !u.PasswordChangedAt.Equal(now) {
		t.Errorf("the user's row after the change = %+v", u)
	}
	if ok, _, err := e.svc.hasher.verify(ctx, newPassword, u.PasswordHash); !ok || err != nil {
		t.Errorf("the stored hash is not the new password's: %v, %v", ok, err)
	}
	// The caller's session: the same row under a new token, used just now from the request's address.
	sess := e.session(alex.ID, mine.Session.ID)
	if sess == nil || e.session(alex.ID, other.Session.ID) != nil {
		t.Fatal("the change must keep the caller's session and delete the other")
	}
	if !sess.RotatedAt.Equal(now) || !sess.LastSeenAt.Equal(now) || sess.LastIP != ipC ||
		!sess.IdleExpiresAt.Equal(now.Add(sessionIdleTTL)) || !sess.ExpiresAt.Equal(mine.Session.ExpiresAt) ||
		!sess.CreatedAt.Equal(mine.Session.CreatedAt) {
		t.Errorf("the caller's session after the change = %+v", sess)
	}
	if res.Token == mine.Token || !tokenWellFormed(res.Token, sessionTokenBytes) || res.Session.ID != mine.Session.ID ||
		!res.Session.IdleExpiresAt.Equal(sess.IdleExpiresAt) || res.User.ID != alex.ID || res.User.PasswordHash != u.PasswordHash ||
		!res.User.PasswordChangedAt.Equal(now) {
		t.Errorf("LoginResult = %+v", res)
	}
	// A rotation of 03 §7.4: the new token is the session, and the old one is its previous token for 60 s, so that
	// the browser's other tabs are not signed out before the new cookie reaches them.
	if !bytes.Equal(sess.TokenHash, e.svc.keys.sessionTokenHash(res.Token)) ||
		!bytes.Equal(sess.PrevTokenHash, e.svc.keys.sessionTokenHash(mine.Token)) || !sess.PrevValidUntil.Equal(now.Add(sessionPrevGrace)) {
		t.Errorf("the caller's session after the change = %+v; want the new hash, and the old one as previous for 60 s", sess)
	}
	for name, tok := range map[string]string{"old": mine.Token, "new": res.Token} {
		if got, err := e.svc.Authenticate(e.request(http.MethodGet, tok)); err != nil || got.SessionID != mine.Session.ID {
			t.Errorf("the %s token right after the change: %+v, %v; want the caller's session", name, got, err)
		}
	}
	if e.svc.Touch(ctx, p, meta(ipC).IP) != nil {
		t.Error("the caller's connection would be closed at its next revalidation")
	}
	// The old token works until the grace ends, and not a millisecond longer; the new one goes on.
	e.advance(sessionPrevGrace)
	if _, err := e.svc.Authenticate(e.request(http.MethodGet, mine.Token)); err != nil {
		t.Errorf("the old token 60 s after the change: %v", err)
	}
	e.advance(time.Millisecond)
	_, err = e.svc.Authenticate(e.request(http.MethodGet, mine.Token))
	wantCode(t, err, api.CodeUnauthenticated)
	if got, err := e.svc.Authenticate(e.request(http.MethodGet, res.Token)); err != nil || got.SessionID != mine.Session.ID {
		t.Errorf("the new token after the grace: %+v, %v", got, err)
	}
	rows := e.audit(auditPasswordChanged)
	if len(rows) != 1 {
		t.Fatalf("%d auth.password_changed rows, want 1", len(rows))
	}
	if r := rows[0]; r.Actor != (store.Actor{Kind: store.ActorUser, UserID: alex.ID, Name: "Alex", IP: ipC}) ||
		r.TargetKind != "user" || r.TargetID != string(alex.ID) || r.TargetName != "Alex" || !r.At.Equal(now) || len(r.Detail) != 0 {
		t.Errorf("auth.password_changed row = %+v", r)
	}
	// The old password is no password any more, and the new one is.
	wantCode(t, e.tryLogin("Alex", testPassword, ipB), api.CodeInvalidCredentials)
	if err := e.tryLogin("Alex", newPassword, ipB); err != nil {
		t.Errorf("a login with the new password: %v", err)
	}
	// Changing it again needs the new password as the current one.
	_, err = e.svc.ChangePassword(ctx, p, testPassword, "a third long passphrase", meta(ipC))
	wantCode(t, err, api.CodeWrongPassword)

	// The new password may not be the account's own name, in any case or width (03 §7.2 rule 5).
	e.addUser("Maximilian")
	max := principal(e.login("Maximilian", ipB))
	for _, next := range []string{"maximilian", "MAXIMILIAN", "Ｍaximilian"} {
		_, err := e.svc.ChangePassword(ctx, max, testPassword, next, meta(ipB))
		if ae := wantCode(t, err, api.CodeValidationFailed); fmt.Sprint(ae.Fields) != "map[newPassword:same_as_username]" {
			t.Errorf("ChangePassword to %q: fields %v, want newPassword: same_as_username", next, ae.Fields)
		}
	}
}

// TestChangePasswordAfterARotation: which token keeps the 60 s when the session was rotated just before the
// change. The chain's daily rotation runs before the handler (03 §7.4), so the request of a password change can
// arrive with the session's previous token. That is the token the browser holds, and the one that is let in until
// the new cookie arrives; the chain's token never reaches the browser and stops working at once. In every other
// case the session's current token becomes the previous one, as in any rotation.
func TestChangePasswordAfterARotation(t *testing.T) {
	for _, tc := range []struct {
		name string
		// wait is how long after the daily rotation the change arrives.
		wait time.Duration
		// cookie picks the token the change's request arrives with ("" for a caller that names none).
		cookie func(before, chains string) string
		// keeps: the token from before the rotation is let in after the change; otherwise the chain's token is.
		keeps bool
	}{
		{"the chain rotated in this request", 0, func(before, _ string) string { return before }, true},
		{"the previous token at the end of its grace", sessionPrevGrace, func(before, _ string) string { return before }, true},
		{"the previous token after its grace", sessionPrevGrace + time.Millisecond, func(before, _ string) string { return before }, false},
		{"the browser has the rotated cookie already", 0, func(_, chains string) string { return chains }, false},
		{"a caller that names no cookie", 0, func(_, _ string) string { return "" }, false},
		{"a cookie that is no token", 0, func(_, _ string) string { return "nonsense" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newSvcEnv(t)
			alex := e.addUser("Alex")
			mine := e.login("Alex", ipA)
			e.advance(sessionRotateAfter)
			cookies, err := e.rotate(mine.Token)
			if err != nil || len(cookies) != 1 {
				t.Fatalf("the daily rotation: cookies %v, err %v; want one", cookies, err)
			}
			before, chains := mine.Token, cookies[0].Value
			e.advance(tc.wait)

			now := e.clk.now()
			m := meta(ipA)
			m.SessionToken = tc.cookie(before, chains)
			res, err := e.svc.ChangePassword(context.Background(), principal(mine), testPassword, newPassword, m)
			if err != nil {
				t.Fatal(err)
			}
			kept, ended := before, chains
			if !tc.keeps {
				kept, ended = chains, before
			}
			sess := e.session(alex.ID, mine.Session.ID)
			if sess == nil || !bytes.Equal(sess.TokenHash, e.svc.keys.sessionTokenHash(res.Token)) ||
				!bytes.Equal(sess.PrevTokenHash, e.svc.keys.sessionTokenHash(kept)) ||
				!sess.PrevValidUntil.Equal(now.Add(sessionPrevGrace)) || !sess.RotatedAt.Equal(now) {
				t.Fatalf("the session after the change = %+v; want the new hash, and the kept token's as previous for 60 s", sess)
			}
			for name, tok := range map[string]string{"new": res.Token, "kept": kept} {
				if got, err := e.svc.Authenticate(e.request(http.MethodGet, tok)); err != nil || got.SessionID != mine.Session.ID {
					t.Errorf("the %s token right after the change: %+v, %v; want the caller's session", name, got, err)
				}
			}
			_, err = e.svc.Authenticate(e.request(http.MethodGet, ended))
			wantCode(t, err, api.CodeUnauthenticated)

			e.advance(sessionPrevGrace)
			if _, err := e.svc.Authenticate(e.request(http.MethodGet, kept)); err != nil {
				t.Errorf("the kept token 60 s after the change: %v", err)
			}
			e.advance(time.Millisecond)
			for _, tok := range []string{kept, ended} {
				_, err = e.svc.Authenticate(e.request(http.MethodGet, tok))
				wantCode(t, err, api.CodeUnauthenticated)
			}
			if got, err := e.svc.Authenticate(e.request(http.MethodGet, res.Token)); err != nil || got.SessionID != mine.Session.ID {
				t.Errorf("the new token after the grace: %+v, %v", got, err)
			}
		})
	}
}

// TestChangePasswordThrottled: wrong current passwords are failed password checks (03 §7.3). The sixth from one
// address is refused without a hash, so a stolen session is no faster way to guess the password than the login
// form; the user's own other address is not blocked.
func TestChangePasswordThrottled(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	alex := e.addUser("Alex")
	mine := e.login("Alex", ipB)
	p := principal(mine)
	e.hashes.Store(0)
	for i := range 5 {
		_, err := e.svc.ChangePassword(ctx, p, fmt.Sprintf("guess number %d", i), newPassword, meta(ipA))
		wantCode(t, err, api.CodeWrongPassword)
	}
	for _, current := range []string{"guess number 6", testPassword} {
		_, err := e.svc.ChangePassword(ctx, p, current, newPassword, meta(ipA))
		if ae := wantCode(t, err, api.CodeRateLimited); ae.RetryAfter != 120 {
			t.Errorf("the blocked attempt: retryAfter %d, want 120", ae.RetryAfter)
		}
	}
	if n := e.hashes.Load(); n != 5 {
		t.Errorf("%d hashes for 5 wrong passwords and 2 blocked attempts, want 5", n)
	}
	wantCode(t, e.svc.DeleteSelf(ctx, p, testPassword, meta(ipA)), api.CodeRateLimited)
	// The login form shares the buckets: this address is blocked there too.
	wantCode(t, e.tryLogin("Alex", testPassword, ipA), api.CodeRateLimited)
	if u := e.userRow(alex.ID); u == nil || u.PasswordHash != alex.PasswordHash {
		t.Fatal("a blocked attempt changed the account")
	}
	// From the user's other address the right password works.
	if _, err := e.svc.ChangePassword(ctx, p, testPassword, newPassword, meta(ipB)); err != nil {
		t.Fatalf("ChangePassword from another address: %v", err)
	}

	// A correct password ends like a login (03 §7.3): this address's bucket for the username is full again, and
	// the bucket of all addresses is as it was before the attempt, with the four failures still in it.
	e = newSvcEnv(t)
	e.addUser("Alex")
	p = principal(e.login("Alex", ipA))
	if got := e.heldTokens(); got != [2]int{} {
		t.Fatalf("%v keys with tokens taken after a login, want none", got)
	}
	for range 4 {
		_, err := e.svc.ChangePassword(ctx, p, "not the password", newPassword, meta(ipA))
		wantCode(t, err, api.CodeWrongPassword)
	}
	if got := e.heldTokens(); got != [2]int{1, 1} {
		t.Fatalf("%v keys with tokens taken after four wrong passwords, want [1 1]", got)
	}
	if _, err := e.svc.ChangePassword(ctx, p, testPassword, newPassword, meta(ipA)); err != nil {
		t.Fatal(err)
	}
	if got := e.heldTokens(); got != [2]int{0, 1} {
		t.Errorf("%v keys with tokens taken after the correct password, want [0 1]", got)
	}
	for i := range 5 {
		_, err := e.svc.ChangePassword(ctx, p, "not the password", newPassword, meta(ipA))
		wantCode(t, err, api.CodeWrongPassword) // five fresh tokens: none of these is blocked
		if i == 4 {
			_, err = e.svc.ChangePassword(ctx, p, newPassword, "a third long passphrase", meta(ipA))
			wantCode(t, err, api.CodeRateLimited)
		}
	}
}

// changeDuringCheck makes the hasher run change once, while a password is being verified: after the call read the
// account, before its Write.
func (e *svcEnv) changeDuringCheck(change func(q *store.Q, now time.Time) error) {
	var once sync.Once
	e.svc.hasher.derive = func(pw, salt []byte, tm, mem uint32, th uint8, kl uint32) []byte {
		once.Do(func() { e.write(func(q *store.Q) error { return change(q, e.clk.now()) }) })
		return argon2.IDKey(pw, salt, tm, mem, th, kl)
	}
}

// TestSelfServiceRechecksInWrite: an account that changes between the password check and the Write is judged as it
// is at the Write.
func TestSelfServiceRechecksInWrite(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(q *store.Q, alex store.User, mine store.Session, now time.Time) error
		code   string
	}{
		{"the password was changed elsewhere", func(q *store.Q, alex store.User, _ store.Session, now time.Time) error {
			return q.SetPasswordHash(alex.ID, strings.Replace(alex.PasswordHash, "$m=64", "$m=65", 1), now)
		}, api.CodeWrongPassword},
		{"an admin cleared the password", func(q *store.Q, alex store.User, _ store.Session, now time.Time) error {
			return q.SetPasswordHash(alex.ID, "", now)
		}, api.CodeWrongPassword},
		{"the session was revoked", func(q *store.Q, alex store.User, mine store.Session, _ time.Time) error {
			_, err := q.DeleteSession(alex.ID, mine.ID)
			return err
		}, api.CodeUnauthenticated},
		{"the account was disabled", func(q *store.Q, alex store.User, _ store.Session, now time.Time) error {
			return q.SetStatus(alex.ID, store.StatusDisabled, now)
		}, api.CodeUnauthenticated},
		{"the account was deleted", func(q *store.Q, alex store.User, _ store.Session, _ time.Time) error {
			return q.DeleteUser(alex.ID)
		}, api.CodeUnauthenticated},
	} {
		for _, call := range []string{"ChangePassword", "DeleteSelf"} {
			e := newSvcEnv(t)
			alex := e.addUser("Alex")
			e.addUser("Sam")
			mine := e.login("Alex", ipA)
			e.login("Alex", ipB)
			users, sessions := e.rawCount("users"), e.rawCount("sessions")
			e.conns.take()
			e.changeDuringCheck(func(q *store.Q, now time.Time) error { return tc.change(q, alex, mine.Session, now) })
			var err error
			if call == "ChangePassword" {
				_, err = e.svc.ChangePassword(context.Background(), principal(mine), testPassword, newPassword, meta(ipA))
			} else {
				err = e.svc.DeleteSelf(context.Background(), principal(mine), testPassword, meta(ipA))
			}
			wantCode(t, err, tc.code)
			// Nothing but the change in between happened: no revocation, no audit row, no connection closed.
			wantUsers, wantSessions := users, sessions
			switch tc.name {
			case "the session was revoked":
				wantSessions--
			case "the account was deleted":
				wantUsers, wantSessions = users-1, 0
			}
			if e.rawCount("users") != wantUsers || e.rawCount("sessions") != wantSessions || len(e.conns.take()) != 0 ||
				e.auditCount(auditPasswordChanged)+e.auditCount(auditUserDeleted) != 0 {
				t.Errorf("%s, %s: the refused call changed something", call, tc.name)
			}
		}
	}
}

func TestDeleteSelf(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	alex := e.addAdmin("Alex")
	sam := e.addUser("Sam")
	admin, member := e.login("Alex", ipA), e.login("Sam", ipB)
	e.set("membersCanInvite", true)
	inv, _ := e.invite(actorFor(sam, ipB), InviteInput{})
	e.conns.take()
	e.hashes.Store(0)

	// The only active admin can't leave, whatever the password: the rule comes first and costs no hash.
	for _, pw := range []string{testPassword, "not the password", ""} {
		wantCode(t, e.svc.DeleteSelf(ctx, principal(admin), pw, meta(ipA)), api.CodeLastAdmin)
	}
	// A second admin who is disabled does not count as one.
	robin := e.addAdmin("Robin")
	e.write(func(q *store.Q) error { return q.SetStatus(robin.ID, store.StatusDisabled, e.clk.now()) })
	wantCode(t, e.svc.DeleteSelf(ctx, principal(admin), testPassword, meta(ipA)), api.CodeLastAdmin)
	if n := e.hashes.Load(); n != 0 {
		t.Errorf("the last-admin answers hashed %d times, want 0", n)
	}

	// A member: the password decides.
	for _, pw := range []string{"", "not the password", strings.Repeat("x", passwordMaxBytes+1)} {
		wantCode(t, e.svc.DeleteSelf(ctx, principal(member), pw, meta(ipB)), api.CodeWrongPassword)
	}
	if n := e.hashes.Load(); n != 1 {
		t.Errorf("three refused passwords hashed %d times, want 1 (only one could be a password)", n)
	}
	if e.userRow(sam.ID) == nil || len(e.conns.take()) != 0 || e.auditCount(auditUserDeleted) != 0 {
		t.Fatal("a refused deletion changed something")
	}
	e.advance(time.Minute)
	now := e.clk.now()
	if err := e.svc.DeleteSelf(ctx, principal(member), testPassword, meta(ipC)); err != nil {
		t.Fatal(err)
	}
	if e.userRow(sam.ID) != nil || len(e.sessions(sam.ID)) != 0 {
		t.Fatal("the account or its sessions are still there")
	}
	want := []closedConn{{ConnSelector{UserID: sam.ID}, ReasonAccountDeleted}}
	if calls := e.conns.take(); fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("CloseConnections calls = %+v, want %+v", calls, want)
	}
	rows := e.audit(auditUserDeleted)
	if len(rows) != 1 {
		t.Fatalf("%d user.deleted rows, want 1", len(rows))
	}
	// The row keeps the name as a snapshot, and is a security event (03 §10).
	if r := rows[0]; r.Actor != (store.Actor{Kind: store.ActorUser, UserID: sam.ID, Name: "Sam", IP: ipC}) ||
		r.TargetKind != "user" || r.TargetID != string(sam.ID) || r.TargetName != "Sam" || !r.At.Equal(now) ||
		fmt.Sprint(r.Detail) != "map[self:true]" {
		t.Errorf("user.deleted row = %+v", r)
	}
	e.read(func(q *store.Q) error {
		events, err := q.SecurityEvents(now.Add(-time.Hour), 10)
		if err == nil && (len(events) != 1 || events[0].Action != auditUserDeleted) {
			t.Errorf("security events = %+v, want the user.deleted row", events)
		}
		return err
	})
	// What the user made stays, without a creator; the name is free again.
	if got := e.inviteRow(inv.ID); got.CreatedBy != nil {
		t.Errorf("the deleted user's invite still names a creator: %+v", got.CreatedBy)
	}
	wantCode(t, e.tryLogin("Sam", testPassword, ipB), api.CodeInvalidCredentials)
	wantCode(t, e.svc.DeleteSelf(ctx, principal(member), testPassword, meta(ipC)), api.CodeUnauthenticated)
	e.addUser("Sam")

	// With a second active admin, an admin may leave.
	e.write(func(q *store.Q) error { return q.SetStatus(robin.ID, store.StatusActive, e.clk.now()) })
	if err := e.svc.DeleteSelf(ctx, principal(admin), testPassword, meta(ipA)); err != nil {
		t.Fatalf("an admin's own deletion with another admin left: %v", err)
	}
	if e.userRow(alex.ID) != nil {
		t.Error("the admin's account is still there")
	}
}

// TestDeleteSelfLastAdminRace: two admins who delete their accounts at the same moment both pass the first check;
// the Write checks again, so one of them stays.
func TestDeleteSelfLastAdminRace(t *testing.T) {
	e := newSvcEnv(t)
	e.addAdmin("Alex")
	e.addAdmin("Robin")
	sessions := []LoginResult{e.login("Alex", ipA), e.login("Robin", ipB)}
	gate := make(chan struct{})
	var inHasher atomic.Int32
	e.svc.hasher.derive = gatedDerive(&inHasher, gate)
	errs := make(chan error, len(sessions))
	for _, s := range sessions {
		go func() { errs <- e.svc.DeleteSelf(context.Background(), principal(s), testPassword, meta(ipA)) }()
	}
	// Both are at their password check, past the first last-admin check.
	deadline := time.Now().Add(10 * time.Second)
	for int(inHasher.Load()) != len(sessions) {
		if time.Now().After(deadline) {
			t.Fatalf("%d calls reached the password check, want %d", inHasher.Load(), len(sessions))
		}
		time.Sleep(time.Millisecond)
	}
	close(gate)
	codes := map[string]int{}
	for range sessions {
		err := <-errs
		switch {
		case err == nil:
			codes["ok"]++
		case api.IsCode(err, api.CodeLastAdmin):
			codes[api.CodeLastAdmin]++
		default:
			t.Errorf("DeleteSelf: %v", err)
		}
	}
	if codes["ok"] != 1 || codes[api.CodeLastAdmin] != 1 {
		t.Errorf("results = %v, want one deletion and one last_admin", codes)
	}
	var admins int
	e.read(func(q *store.Q) error {
		var err error
		admins, err = q.CountActiveAdmins()
		return err
	})
	if admins != 1 {
		t.Errorf("%d active admins left, want 1", admins)
	}
}

func TestRevokeDevice(t *testing.T) {
	f := newRevFixture(t)
	ctx := context.Background()
	// Only the caller's own devices exist for this call.
	for _, id := range []store.DeviceID{"", "aaaaaaaaaaaa", samDevice} {
		wantCode(t, f.svc.RevokeDevice(ctx, f.caller, id, meta(ipC)), api.CodeNotFound)
	}
	if f.rawCount("devices") != 2 || len(f.conns.take()) != 0 || f.auditCount(auditDeviceRevoked) != 0 {
		t.Fatal("a refused revocation changed something")
	}
	f.advance(time.Minute)
	now := f.clk.now()
	if err := f.svc.RevokeDevice(ctx, f.caller, alexDevice, meta(ipC)); err != nil {
		t.Fatal(err)
	}
	// The device goes with its tokens and its push subscription; the sessions, their subscriptions and the code
	// the user approved (for whichever app is waiting) stay.
	alex := string(f.alex.ID)
	for what, got := range map[string][2]int{
		"devices":            {f.rawInt("SELECT count(*) FROM devices WHERE user_id = ?", alex), 0},
		"device tokens":      {f.rawInt("SELECT count(*) FROM device_tokens WHERE device_id = ?", string(alexDevice)), 0},
		"push subscriptions": {f.rawInt("SELECT count(*) FROM push_subscriptions WHERE user_id = ?", alex), 3},
		"device codes":       {f.rawInt("SELECT count(*) FROM device_codes WHERE user_id = ?", alex), 1},
		"sessions":           {f.rawInt("SELECT count(*) FROM sessions WHERE user_id = ?", alex), 3},
		"devices of Sam's":   {f.rawInt("SELECT count(*) FROM devices WHERE user_id = ?", string(f.sam.ID)), 1},
	} {
		if got[0] != got[1] {
			t.Errorf("%d %s afterwards, want %d", got[0], what, got[1])
		}
	}
	want := []closedConn{{ConnSelector{UserID: f.alex.ID, DeviceID: alexDevice}, ReasonDeviceRevoked}}
	if calls := f.conns.take(); fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("CloseConnections calls = %+v, want %+v", calls, want)
	}
	rows := f.audit(auditDeviceRevoked)
	if len(rows) != 1 {
		t.Fatalf("%d device.revoked rows, want 1", len(rows))
	}
	if r := rows[0]; r.Actor != (store.Actor{Kind: store.ActorUser, UserID: f.alex.ID, Name: "Alex", IP: ipC}) ||
		r.TargetKind != "device" || r.TargetID != string(alexDevice) || r.TargetName != "Alex-PC" || !r.At.Equal(now) ||
		len(r.Detail) != 0 {
		t.Errorf("device.revoked row = %+v", r)
	}
	wantCode(t, f.svc.RevokeDevice(ctx, f.caller, alexDevice, meta(ipC)), api.CodeNotFound)
	// The caller's sessions are as they were.
	if _, err := f.svc.Authenticate(f.request(http.MethodGet, f.s[0].Token)); err != nil {
		t.Errorf("the caller's session afterwards: %v", err)
	}
}

// selfServiceCalls are the calls of a web session for its own account, each with input that would succeed.
func selfServiceCalls(svc *Service, target store.SessionID) map[string]func(p Principal) error {
	ctx := context.Background()
	return map[string]func(p Principal) error{
		"Logout":           func(p Principal) error { return svc.Logout(ctx, p, meta(ipA)) },
		"LogoutEverywhere": func(p Principal) error { return svc.LogoutEverywhere(ctx, p, meta(ipA)) },
		"RevokeSession":    func(p Principal) error { return svc.RevokeSession(ctx, p, target, meta(ipA)) },
		"RevokeOtherSessions": func(p Principal) error {
			_, err := svc.RevokeOtherSessions(ctx, p, meta(ipA))
			return err
		},
		"ChangePassword": func(p Principal) error {
			_, err := svc.ChangePassword(ctx, p, testPassword, newPassword, meta(ipA))
			return err
		},
		"DeleteSelf":   func(p Principal) error { return svc.DeleteSelf(ctx, p, testPassword, meta(ipA)) },
		"RevokeDevice": func(p Principal) error { return svc.RevokeDevice(ctx, p, alexDevice, meta(ipA)) },
	}
}

// TestSelfServiceCaller: every self-service call checks its caller again at the moment of the change. A principal
// whose session is gone or expired, or whose account is no longer active, is unauthenticated and changes nothing,
// whatever the chain let in a moment ago; a principal that is no web session's is refused before any work.
func TestSelfServiceCaller(t *testing.T) {
	for name, spoil := range map[string]func(f *revFixture){
		"the session is gone": func(f *revFixture) {
			f.write(func(q *store.Q) error {
				_, err := q.DeleteSession(f.alex.ID, f.s[0].Session.ID)
				return err
			})
		},
		"the session has expired": func(f *revFixture) { f.advance(sessionIdleTTL + time.Hour) },
		"the account is disabled": func(f *revFixture) {
			f.write(func(q *store.Q) error { return q.SetStatus(f.alex.ID, store.StatusDisabled, f.clk.now()) })
		},
		"the account is pending": func(f *revFixture) {
			f.write(func(q *store.Q) error { return q.SetStatus(f.alex.ID, store.StatusPending, f.clk.now()) })
		},
	} {
		f := newRevFixture(t)
		spoil(f)
		before := f.rowCounts()
		for call, run := range selfServiceCalls(f.svc, f.s[1].Session.ID) {
			if call == "Logout" {
				continue // idempotent by design: a session that is gone already is no error (login_test.go)
			}
			wantCode(t, run(f.caller), api.CodeUnauthenticated)
		}
		if after := f.rowCounts(); fmt.Sprint(after) != fmt.Sprint(before) || len(f.conns.take()) != 0 || f.hashes.Load() != 0 {
			t.Errorf("%s: a refused call changed something: rows %v → %v, %d hashes", name, before, after, f.hashes.Load())
		}
	}

	f := newRevFixture(t)
	before := f.rowCounts()
	for call, run := range selfServiceCalls(f.svc, f.s[1].Session.ID) {
		for _, p := range []Principal{
			{},
			{Method: MethodSession},
			{Method: MethodSession, UserID: f.alex.ID},
			{Method: MethodSession, SessionID: f.s[0].Session.ID},
			{Method: 9, UserID: f.alex.ID, SessionID: f.s[0].Session.ID},
			// Another user's session ID is nobody's.
			{Method: MethodSession, UserID: f.alex.ID, SessionID: f.samSession.Session.ID},
			{Method: MethodSession, UserID: "aaaaaaaaaaaa", SessionID: f.s[0].Session.ID},
		} {
			err := run(p)
			if call == "Logout" && p.UserID != "" && p.SessionID != "" && p.Method == MethodSession {
				// Logout is idempotent by design: no such session is nothing to log out. It still asks for that
				// session's connections to be closed, which matches none.
				if err != nil {
					t.Errorf("Logout(%+v): %v, want nil (nothing to log out)", p, err)
				}
				f.conns.take()
				continue
			}
			wantCode(t, err, api.CodeUnauthenticated)
		}
		// A device's principal (M2) is told so.
		err := run(Principal{Method: MethodBearer, UserID: f.alex.ID, DeviceID: alexDevice})
		if !api.IsCode(err, api.CodeInternal) || !strings.Contains(err.Error(), call+" for a device not implemented") {
			t.Errorf("%s for a device: %v", call, err)
		}
	}
	if after := f.rowCounts(); fmt.Sprint(after) != fmt.Sprint(before) || len(f.conns.take()) != 0 {
		t.Errorf("a refused call changed something: rows %v → %v", before, after)
	}
}

// TestSelfServiceInternalErrors: on a closed store every self-service call answers internal, never a verdict, and
// closes nothing.
func TestSelfServiceInternalErrors(t *testing.T) {
	f := newRevFixture(t)
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	for call, run := range selfServiceCalls(f.svc, f.s[1].Session.ID) {
		if err := run(f.caller); !api.IsCode(err, api.CodeInternal) {
			t.Errorf("%s on a closed store: %v", call, err)
		}
	}
	if calls := f.conns.take(); len(calls) != 0 {
		t.Errorf("failed calls closed connections: %+v", calls)
	}
}

// TestOwnPasswordWithoutAUsableHash: a signed-in account whose stored hash can't be used (cleared by a reset that
// raced, or malformed) fails its re-authentication like a wrong password, at the cost of the same one hash.
func TestOwnPasswordWithoutAUsableHash(t *testing.T) {
	for name, hash := range map[string]string{"cleared": "", "malformed": "$argon2id$v=19$not-a-hash"} {
		e := newSvcEnv(t)
		alex := e.addUser("Alex")
		p := principal(e.login("Alex", ipA))
		e.write(func(q *store.Q) error {
			if hash == "" {
				return q.SetPasswordHash(alex.ID, "", e.clk.now())
			}
			return q.SetPasswordHash(alex.ID, hash, e.clk.now())
		})
		e.hashes.Store(0)
		_, err := e.svc.ChangePassword(context.Background(), p, testPassword, newPassword, meta(ipA))
		wantCode(t, err, api.CodeWrongPassword)
		wantCode(t, e.svc.DeleteSelf(context.Background(), p, testPassword, meta(ipA)), api.CodeWrongPassword)
		if n := e.hashes.Load(); n != 2 {
			t.Errorf("%s hash: %d hashes for two attempts, want 2", name, n)
		}
		if logged := strings.Contains(e.logs.String(), "a stored password hash is malformed"); logged != (name == "malformed") {
			t.Errorf("%s hash: the log says malformed = %v:\n%s", name, logged, e.logs.String())
		}
		if u := e.userRow(alex.ID); u == nil || u.PasswordHash != hash {
			t.Errorf("%s hash: the account changed: %+v", name, u)
		}
	}
}

// TestOwnPasswordHashQueueFull: with the hash queue full, a re-authentication answers 503 server_busy and is no
// failed password check.
func TestOwnPasswordHashQueueFull(t *testing.T) {
	e := newSvcEnv(t)
	e.addUser("Alex")
	p := principal(e.login("Alex", ipA))
	gate := make(chan struct{})
	var maxInFlight atomic.Int32
	h := blockingHasher(t, 1, gate, &maxInFlight)
	e.svc.hasher = h
	var wg sync.WaitGroup
	for range 1 + maxHashWaiters {
		wg.Go(func() { _, _ = h.hash(context.Background(), "a long enough password") })
	}
	deadline := time.Now().Add(10 * time.Second)
	for h.waiting.Load() != maxHashWaiters {
		if time.Now().After(deadline) {
			t.Fatalf("%d callers wait for the hash slot, want %d", h.waiting.Load(), maxHashWaiters)
		}
		time.Sleep(time.Millisecond)
	}
	_, err := e.svc.ChangePassword(context.Background(), p, testPassword, newPassword, meta(ipA))
	if ae := wantCode(t, err, api.CodeServerBusy); ae.RetryAfter != 5 {
		t.Errorf("ChangePassword with a full hash queue: retryAfter %d, want 5", ae.RetryAfter)
	}
	wantCode(t, e.svc.DeleteSelf(context.Background(), p, testPassword, meta(ipA)), api.CodeServerBusy)
	if got := e.heldTokens(); got != [2]int{} {
		t.Errorf("attempts that never hashed left %v keys with tokens taken, want none", got)
	}
	close(gate)
	wg.Wait()
}
