package auth

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// Admin-issued password resets (README S64; 03 §7.10): issuing a link, with its rules, and the public side of it.

// resetLink issues a reset link for user id as actor a and returns the token of its fragment.
func (e *svcEnv) resetLink(a store.Actor, id store.UserID, password string) string {
	e.t.Helper()
	link, err := e.svc.IssuePasswordReset(context.Background(), a, id, password)
	if err != nil {
		e.t.Fatalf("IssuePasswordReset: %v", err)
	}
	token, ok := strings.CutPrefix(link.URL, testOrigin+"/reset#")
	if !ok || len(token) != 43 || !tokenWellFormed(token, resetTokenBytes) {
		e.t.Fatalf("reset link %q: want %s/reset#<43-character token>", link.URL, testOrigin)
	}
	if want := e.clk.now().Add(24 * time.Hour); !link.ExpiresAt.Equal(want) {
		e.t.Fatalf("the link expires at %v, want %v", link.ExpiresAt, want)
	}
	return token
}

// resetRow reads the reset link of a user straight from the table: its hash, who made it and when it ends.
func (e *svcEnv) resetRow(id store.UserID) (hash []byte, createdBy string, expires int64, ok bool) {
	e.t.Helper()
	db := e.raw()
	defer func() { _ = db.Close() }()
	var by sql.NullString
	err := db.QueryRowContext(context.Background(),
		"SELECT token_hash, created_by, expires_at FROM password_resets WHERE user_id = ?", string(id)).Scan(&hash, &by, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", 0, false
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return hash, by.String, expires, true
}

func TestIssuePasswordReset(t *testing.T) {
	a := newAdminEnv(t)
	ctx := context.Background()
	second := a.login("Sam", ipC)
	a.restart(func() { a.seedDevice(a.sam.ID, samDevice) })
	a.cache(a.alexIn, a.samIn, second)
	a.conns.take()
	a.hashes.Store(0)

	_, err := a.svc.IssuePasswordReset(ctx, a.asAlex, "nobody000000", "")
	wantCode(t, err, api.CodeUserNotFound)

	// A link for a member needs no password.
	a.later(a.alexIn, a.samIn, second)
	token := a.resetLink(a.asAlex, a.sam.ID, "")
	a.wantAudit(auditPasswordResetIssued, 1, a.asAlex, a.sam, "map[]")
	// The database holds the keyed hash of the token, never the token; the link is the admin's.
	hash, by, expires, ok := a.resetRow(a.sam.ID)
	if !ok || !bytes.Equal(hash, a.svc.keys.inviteTokenHash(token)) || by != string(a.alex.ID) ||
		expires != a.clk.now().Add(24*time.Hour).UnixMilli() {
		t.Errorf("the reset row: hash %x, created_by %q, expires %d, found %v", hash, by, expires, ok)
	}
	if n := a.rawInt("SELECT count(*) FROM audit_log WHERE instr(detail, ?) > 0 OR instr(target_name, ?) > 0", token, token); n != 0 {
		t.Error("the token is in the audit log")
	}
	if strings.Contains(a.logs.String(), token) {
		t.Error("the token is in the log")
	}
	// The password is gone, and so is every credential of the account (03 §7.7 "Admin: password reset").
	if u := a.userRow(a.sam.ID); u == nil || u.PasswordHash != "" || u.Status != store.StatusActive || u.Role != store.RoleUser {
		t.Errorf("the account after the reset was issued: %+v", u)
	}
	if n := len(a.sessions(a.sam.ID)); n != 0 || a.rawInt("SELECT count(*) FROM devices WHERE user_id = ?", string(a.sam.ID)) != 0 {
		t.Errorf("%d sessions left, or the device", n)
	}
	wantCalls := []closedConn{{ConnSelector{UserID: a.sam.ID}, ReasonPasswordReset}}
	if calls := a.conns.take(); fmt.Sprint(calls) != fmt.Sprint(wantCalls) {
		t.Errorf("CloseConnections calls = %+v, want %+v", calls, wantCalls)
	}
	for _, in := range []LoginResult{a.samIn, second} {
		_, err := a.svc.Authenticate(a.request(http.MethodGet, in.Token))
		wantCode(t, err, api.CodeUnauthenticated)
	}
	if alerts := a.alerts.take(); len(alerts) != 0 {
		t.Errorf("a reset of a member alerted: %+v", alerts)
	}
	if p := a.who(a.alexIn); p.UserID != a.alex.ID {
		t.Errorf("the admin's own session: %+v", p)
	}
	// The old password answers like a wrong one, at the cost of the same one hash.
	wantCode(t, a.tryLogin("Sam", testPassword, ipB), api.CodeInvalidCredentials)
	if a.hashes.Load() != 1 {
		t.Errorf("%d hashes, want the one of the refused login", a.hashes.Load())
	}

	// A second link replaces the first: only the newest works.
	a.advance(time.Hour)
	newer := a.resetLink(store.CLIActor, a.sam.ID, "")
	if _, by, _, _ := a.resetRow(a.sam.ID); by != "" || a.rawCount("password_resets") != 1 {
		t.Errorf("the second link: created_by %q, %d rows; want the CLI's, alone", by, a.rawCount("password_resets"))
	}
	_, err = a.svc.CheckPasswordReset(ctx, token, meta(ipB))
	wantCode(t, err, api.CodeResetTokenInvalid)
	if name, err := a.svc.CheckPasswordReset(ctx, newer, meta(ipB)); err != nil || name != "Sam" {
		t.Errorf("the newer link: %q, %v", name, err)
	}
	a.wantAudit(auditPasswordResetIssued, 2, store.CLIActor, a.sam, "map[]")
}

// TestIssuePasswordResetRules is 03 §7.10's rule list: another admin only with the acting admin's password, never
// the own account, and the CLI exempt from both.
func TestIssuePasswordResetRules(t *testing.T) {
	a := newAdminEnv(t)
	ctx := context.Background()
	kim := a.addAdmin("Kim")
	kimIn := a.login("Kim", ipC)
	a.cache(kimIn)
	a.hashes.Store(0)

	// Oneself: /me/password is the place, with or without the password.
	for _, pw := range []string{"", testPassword} {
		_, err := a.svc.IssuePasswordReset(ctx, a.asAlex, a.alex.ID, pw)
		wantCode(t, err, api.CodeSelfActionForbidden)
	}
	// Another admin: not without the acting admin's own password.
	for _, pw := range []string{"", "not the password"} {
		_, err := a.svc.IssuePasswordReset(ctx, a.asAlex, kim.ID, pw)
		wantCode(t, err, api.CodeWrongPassword)
	}
	if a.hashes.Load() != 1 || a.rawCount("password_resets") != 0 || len(a.conns.take()) != 0 || len(a.alerts.take()) != 0 ||
		a.auditCount(auditPasswordResetIssued) != 0 {
		t.Fatalf("refused resets: %d hashes (want 1), or something changed", a.hashes.Load())
	}
	if u := a.userRow(kim.ID); u == nil || u.PasswordHash != kim.PasswordHash || a.who(kimIn).UserID != kim.ID {
		t.Fatalf("the other admin after refused resets: %+v", u)
	}

	// With it: the link, and the alert.
	a.later(kimIn)
	token := a.resetLink(a.asAlex, kim.ID, testPassword)
	want := AdminAlert{Kind: AlertAdminPasswordReset, Actor: "Alex", Target: "Kim", At: a.clk.now()}
	if alerts := a.alerts.take(); len(alerts) != 1 || alerts[0] != want {
		t.Errorf("alerts = %+v, want %+v", alerts, want)
	}
	if name, err := a.svc.CheckPasswordReset(ctx, token, meta(ipC)); err != nil || name != "Kim" {
		t.Errorf("the link for the other admin: %q, %v", name, err)
	}
	_, err := a.svc.Authenticate(a.request(http.MethodGet, kimIn.Token))
	wantCode(t, err, api.CodeUnauthenticated)

	// A disabled admin is an admin: the link would hand the account over once it is enabled.
	a.write(func(q *store.Q) error { return q.SetStatus(kim.ID, store.StatusDisabled, a.clk.now()) })
	_, err = a.svc.IssuePasswordReset(ctx, a.asAlex, kim.ID, "")
	wantCode(t, err, api.CodeWrongPassword)

	// The CLI is exempt from both: it is how a sole admin who is locked out gets back in.
	a.alerts.take()
	a.conns.take()
	token = a.resetLink(store.CLIActor, a.alex.ID, "")
	want = AdminAlert{Kind: AlertAdminPasswordReset, Actor: "cli", Target: "Alex", At: a.clk.now()}
	if alerts := a.alerts.take(); len(alerts) != 1 || alerts[0] != want {
		t.Errorf("alerts after the CLI's reset = %+v, want %+v", alerts, want)
	}
	if _, err := a.svc.CompletePasswordReset(ctx, token, newPassword, meta(ipA)); err != nil {
		t.Fatalf("the locked-out admin completing the CLI's link: %v", err)
	}
	wantCode(t, a.tryLogin("Alex", testPassword, ipA), api.CodeInvalidCredentials)
	if _, err := a.svc.Login(ctx, "Alex", newPassword, meta(ipA)); err != nil {
		t.Fatalf("the admin's login with the new password: %v", err)
	}
}

// TestIssuePasswordResetRechecksInWrite: the accounts are judged as they are when the reset is written, not as they
// were when the acting admin's password was checked.
func TestIssuePasswordResetRechecksInWrite(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(q *store.Q, a *adminEnv, kim store.User, now time.Time) error
		code   string
	}{
		{"the actor was demoted", func(q *store.Q, a *adminEnv, _ store.User, now time.Time) error {
			return q.SetRole(a.alex.ID, store.RoleUser, now)
		}, api.CodeForbidden},
		{"the actor's password changed", func(q *store.Q, a *adminEnv, _ store.User, now time.Time) error {
			return q.SetPasswordHash(a.alex.ID, strings.Replace(a.alex.PasswordHash, "$m=64", "$m=65", 1), now)
		}, api.CodeWrongPassword},
		{"the target was deleted", func(q *store.Q, _ *adminEnv, kim store.User, _ time.Time) error {
			return q.DeleteUser(kim.ID)
		}, api.CodeUserNotFound},
	} {
		a := newAdminEnv(t)
		kim := a.addAdmin("Kim")
		kimIn := a.login("Kim", ipC)
		a.changeDuringCheck(func(q *store.Q, now time.Time) error { return tc.change(q, a, kim, now) })
		_, err := a.svc.IssuePasswordReset(context.Background(), a.asAlex, kim.ID, testPassword)
		wantCode(t, err, tc.code)
		if u := a.userRow(kim.ID); (u != nil && u.PasswordHash != kim.PasswordHash) || a.rawCount("password_resets") != 0 ||
			a.auditCount(auditPasswordResetIssued) != 0 || len(a.alerts.take()) != 0 || len(a.conns.take()) != 0 {
			t.Errorf("%s: the refused reset changed something: %+v", tc.name, u)
		}
		if u := a.userRow(kim.ID); u != nil && a.who(kimIn).UserID != kim.ID {
			t.Errorf("%s: the other admin was signed out", tc.name)
		}
	}
}

func TestCheckPasswordReset(t *testing.T) {
	a := newAdminEnv(t)
	ctx := context.Background()
	token := a.resetLink(a.asAlex, a.sam.ID, "")
	a.hashes.Store(0)

	if name, err := a.svc.CheckPasswordReset(ctx, token, meta(ipB)); err != nil || name != "Sam" {
		t.Fatalf("CheckPasswordReset = %q, %v", name, err)
	}
	// A check uses nothing up.
	if name, err := a.svc.CheckPasswordReset(ctx, token, meta(ipB)); err != nil || name != "Sam" {
		t.Fatalf("a second check = %q, %v", name, err)
	}
	for name, bad := range map[string]string{
		"unknown":         newToken(resetTokenBytes),
		"empty":           "",
		"too short":       token[:42],
		"too long":        token + "A",
		"not base64url":   strings.Repeat("!", 43),
		"an invite token": newToken(inviteTokenBytes),
	} {
		if _, err := a.svc.CheckPasswordReset(ctx, bad, meta(ipB)); !api.IsCode(err, api.CodeResetTokenInvalid) {
			t.Errorf("a token that is %s: %v, want reset_token_invalid", name, err)
		}
	}
	// It works for 24 hours, to the millisecond.
	a.advance(24 * time.Hour)
	if _, err := a.svc.CheckPasswordReset(ctx, token, meta(ipC)); err != nil {
		t.Errorf("the link at its last moment: %v", err)
	}
	a.advance(time.Millisecond)
	_, err := a.svc.CheckPasswordReset(ctx, token, meta(ipC))
	wantCode(t, err, api.CodeResetTokenInvalid)
	_, err = a.svc.CompletePasswordReset(ctx, token, newPassword, meta(ipC))
	wantCode(t, err, api.CodeResetTokenInvalid)
	if a.hashes.Load() != 0 {
		t.Errorf("checks and a dead link hashed %d times", a.hashes.Load())
	}

	// The auth-ip bucket: the 21st attempt of an address is refused before any lookup.
	token = a.resetLink(a.asAlex, a.sam.ID, "")
	const busy = "203.0.113.200"
	for range 20 {
		if _, err := a.svc.CheckPasswordReset(ctx, token, meta(busy)); err != nil {
			t.Fatal(err)
		}
	}
	_, err = a.svc.CheckPasswordReset(ctx, token, meta(busy))
	wantCode(t, err, api.CodeRateLimited)
	_, err = a.svc.CompletePasswordReset(ctx, token, newPassword, meta(busy))
	wantCode(t, err, api.CodeRateLimited)
	if a.userRow(a.sam.ID).PasswordHash != "" {
		t.Error("a throttled completion set the password")
	}
}

// TestResetLinkOfAnInactiveAccount: a link can be issued for an account of any status, but only an active account
// can use it. The answer is the one a login gives a correct password.
func TestResetLinkOfAnInactiveAccount(t *testing.T) {
	a := newAdminEnv(t)
	ctx := context.Background()
	pending := a.addUser("kim_p", func(u *store.User) { u.Status, u.CreatedVia = store.StatusPending, "signup" })
	off := store.StatusDisabled
	if _, err := a.update(a.sam.ID, UserChange{Status: &off}); err != nil {
		t.Fatal(err)
	}
	a.hashes.Store(0)
	for _, tc := range []struct {
		user store.User
		code string
	}{{a.sam, api.CodeAccountDisabled}, {pending, api.CodeAccountPending}} {
		token := a.resetLink(a.asAlex, tc.user.ID, "")
		_, err := a.svc.CheckPasswordReset(ctx, token, meta(ipB))
		wantCode(t, err, tc.code)
		_, err = a.svc.CompletePasswordReset(ctx, token, newPassword, meta(ipB))
		wantCode(t, err, tc.code)
		// Bad fields don't come first: the account's state does.
		_, err = a.svc.CompletePasswordReset(ctx, token, "short", meta(ipB))
		wantCode(t, err, tc.code)
		if u := a.userRow(tc.user.ID); u.PasswordHash != "" || a.rawCount("sessions") != 1 {
			t.Errorf("a refused completion for %s changed something: %+v", tc.user.Username, u)
		}
	}
	if a.hashes.Load() != 0 {
		t.Errorf("refused completions hashed %d times", a.hashes.Load())
	}
	// Disabling an account deletes its pending link (03 §7.7); enabling it does not bring one back.
	on := store.StatusActive
	if _, err := a.update(a.sam.ID, UserChange{Status: &on}); err != nil {
		t.Fatal(err)
	}
	token := a.resetLink(a.asAlex, a.sam.ID, "")
	if _, err := a.update(a.sam.ID, UserChange{Status: &off}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.update(a.sam.ID, UserChange{Status: &on}); err != nil {
		t.Fatal(err)
	}
	_, err := a.svc.CheckPasswordReset(ctx, token, meta(ipB))
	wantCode(t, err, api.CodeResetTokenInvalid)
	// The link of the sign-up works once an admin approved it.
	token = a.resetLink(a.asAlex, pending.ID, "")
	if _, err := a.svc.Approve(ctx, a.asAlex, pending.ID); err != nil {
		t.Fatal(err)
	}
	if res, err := a.svc.CompletePasswordReset(ctx, token, newPassword, meta(ipB)); err != nil || res.User.ID != pending.ID {
		t.Fatalf("the approved sign-up completing its link: %+v, %v", res, err)
	}
}

func TestCompletePasswordReset(t *testing.T) {
	a := newAdminEnv(t)
	ctx := context.Background()
	token := a.resetLink(a.asAlex, a.sam.ID, "")
	a.conns.take()
	a.hashes.Store(0)

	// A link that is not live is refused before the password is looked at, and costs no hash.
	_, err := a.svc.CompletePasswordReset(ctx, newToken(resetTokenBytes), "short", meta(ipB))
	wantCode(t, err, api.CodeResetTokenInvalid)
	_, err = a.svc.CompletePasswordReset(ctx, "", "", meta(ipB))
	wantCode(t, err, api.CodeResetTokenInvalid)
	// The password rules, with a live link: a code for the field, no hash, and the link still works.
	for pw, want := range map[string]string{
		"":                       "required",
		"1234567":                "too_short",
		strings.Repeat("x", 129): "too_long",
		"PassWord1":              "too_common",
		"bad\x00password":        "invalid",
	} {
		_, err := a.svc.CompletePasswordReset(ctx, token, pw, meta(ipB))
		wantFields(t, err, map[string]string{"password": want})
	}
	if a.hashes.Load() != 0 || a.rawCount("sessions") != 1 || a.userRow(a.sam.ID).PasswordHash != "" {
		t.Fatalf("refused completions: %d hashes, or something changed", a.hashes.Load())
	}
	if name, err := a.svc.CheckPasswordReset(ctx, token, meta(ipB)); err != nil || name != "Sam" {
		t.Fatalf("the link after refused completions: %q, %v", name, err)
	}

	// The real thing: the password, the session, the audit row; the link is used up.
	a.advance(time.Hour)
	now := a.clk.now()
	res, err := a.svc.CompletePasswordReset(ctx, token, newPassword, meta(ipB))
	if err != nil {
		t.Fatal(err)
	}
	if a.hashes.Load() != 1 {
		t.Errorf("the completion hashed %d times, want 1", a.hashes.Load())
	}
	u := a.userRow(a.sam.ID)
	if u.PasswordHash == "" || u.PasswordHash == a.sam.PasswordHash || !u.PasswordChangedAt.Equal(now) || !u.LastLoginAt.Equal(now) ||
		u.Status != store.StatusActive || u.Role != store.RoleUser {
		t.Errorf("the account after the reset: %+v", u)
	}
	if res.User.ID != a.sam.ID || res.User.Username != "Sam" || res.User.PasswordHash != u.PasswordHash ||
		res.Session.UserID != a.sam.ID || res.Session.Name != "Chrome on Windows" || res.Session.LastIP != ipB ||
		!res.Session.CreatedAt.Equal(now) || !res.Session.IdleExpiresAt.Equal(now.Add(sessionIdleTTL)) ||
		!res.Session.ExpiresAt.Equal(now.Add(sessionMaxTTL)) || !tokenWellFormed(res.Token, sessionTokenBytes) {
		t.Errorf("CompletePasswordReset = %+v", res)
	}
	if p := a.who(res); p.UserID != a.sam.ID || p.SessionID != res.Session.ID || p.Username != "Sam" {
		t.Errorf("the session of the completed reset: %+v", p)
	}
	a.wantAudit(auditPasswordResetCompleted, 1, userActor(*u, meta(ipB)), a.sam, "map[]")
	if a.rawCount("password_resets") != 0 || len(a.sessions(a.sam.ID)) != 1 {
		t.Error("the link is still there, or the account has other sessions")
	}
	if calls, alerts := a.conns.take(), a.alerts.take(); len(calls) != 0 || len(alerts) != 0 {
		t.Errorf("a completion without an old cookie closed %+v and alerted %+v", calls, alerts)
	}
	// It can't be used twice.
	_, err = a.svc.CompletePasswordReset(ctx, token, "a third long passphrase", meta(ipB))
	wantCode(t, err, api.CodeResetTokenInvalid)
	_, err = a.svc.CheckPasswordReset(ctx, token, meta(ipB))
	wantCode(t, err, api.CodeResetTokenInvalid)
	// The new password logs in; the old one does not.
	wantCode(t, a.tryLogin("Sam", testPassword, ipC), api.CodeInvalidCredentials)
	if _, err := a.svc.Login(ctx, "Sam", newPassword, meta(ipC)); err != nil {
		t.Fatalf("a login with the new password: %v", err)
	}

	// Not the username, whatever its case and width.
	alexander := a.addUser("Alexander")
	token = a.resetLink(a.asAlex, alexander.ID, "")
	_, err = a.svc.CompletePasswordReset(ctx, token, "ＡLEXANDER", meta(ipB))
	wantFields(t, err, map[string]string{"password": "same_as_username"})
}

// TestCompletePasswordResetHashBudget: the completion is a public request that hashes, so it takes a token of the
// auth-hash budget (03 §7.3). An empty budget is 503 server_busy, and the link is still there.
func TestCompletePasswordResetHashBudget(t *testing.T) {
	e := newSvcEnv(t, func(o *Options) { o.Hashes = HashBudget{Burst: 1, PerSecond: 1} })
	sam := e.addUser("Sam")
	token := e.resetLink(store.CLIActor, sam.ID, "")
	wantCode(t, e.tryLogin("nobody", testPassword, ipC), api.CodeInvalidCredentials) // takes the one token
	e.hashes.Store(0)
	_, err := e.svc.CompletePasswordReset(context.Background(), token, newPassword, meta(ipB))
	if ae := wantCode(t, err, api.CodeServerBusy); ae.RetryAfter < 1 {
		t.Errorf("retryAfter %d, want at least 1", ae.RetryAfter)
	}
	if e.hashes.Load() != 0 || e.userRow(sam.ID).PasswordHash != "" {
		t.Fatal("a completion without budget hashed or set the password")
	}
	e.advance(time.Second)
	if _, err := e.svc.CompletePasswordReset(context.Background(), token, newPassword, meta(ipB)); err != nil {
		t.Fatalf("the completion once the budget is back: %v", err)
	}
}

// TestCompletePasswordResetRace: two requests with one link leave one password and one session; the other is told
// that the link is used.
func TestCompletePasswordResetRace(t *testing.T) {
	a := newAdminEnv(t)
	token := a.resetLink(a.asAlex, a.sam.ID, "")
	a.hashes.Store(0)
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	results := make([]LoginResult, n)
	for i := range n {
		wg.Go(func() {
			results[i], errs[i] = a.svc.CompletePasswordReset(context.Background(), token,
				fmt.Sprintf("passphrase number %d", i), meta(clientAddr(i)))
		})
	}
	wg.Wait()
	winner := -1
	for i, err := range errs {
		switch {
		case err == nil && winner < 0:
			winner = i
		case err == nil:
			t.Errorf("requests %d and %d both completed the reset", winner, i)
		case !api.IsCode(err, api.CodeResetTokenInvalid):
			t.Errorf("request %d: %v, want reset_token_invalid", i, err)
		}
	}
	if winner < 0 {
		t.Fatal("no request completed the reset")
	}
	if n := len(a.sessions(a.sam.ID)); n != 1 || a.auditCount(auditPasswordResetCompleted) != 1 {
		t.Errorf("%d sessions and %d audit rows, want one of each", n, a.auditCount(auditPasswordResetCompleted))
	}
	// The password is the winner's.
	if _, err := a.svc.Login(context.Background(), "Sam", fmt.Sprintf("passphrase number %d", winner), meta(ipC)); err != nil {
		t.Errorf("a login with the winner's password: %v", err)
	}
	if p := a.who(results[winner]); p.UserID != a.sam.ID {
		t.Errorf("the winner's session: %+v", p)
	}
}

// clientAddr is the i-th address of a documentation range.
func clientAddr(i int) string { return fmt.Sprintf("192.0.2.%d", 1+i%90) }

// TestCompletePasswordResetRechecksInWrite: what happens to the link or the account while the new password is
// hashed decides, not what the first read saw.
func TestCompletePasswordResetRechecksInWrite(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(q *store.Q, a *adminEnv, now time.Time) error
		check  func(t *testing.T, err error)
	}{
		{"the link was replaced", func(q *store.Q, a *adminEnv, now time.Time) error {
			return q.ReplacePasswordReset(store.PasswordReset{TokenHash: []byte("a-newer-link"), UserID: a.sam.ID,
				ExpiresAt: now.Add(time.Hour)})
		}, func(t *testing.T, err error) { wantCode(t, err, api.CodeResetTokenInvalid) }},
		{"the account was disabled", func(q *store.Q, a *adminEnv, now time.Time) error {
			return q.SetStatus(a.sam.ID, store.StatusDisabled, now)
		}, func(t *testing.T, err error) { wantCode(t, err, api.CodeAccountDisabled) }},
		{"the account was deleted", func(q *store.Q, a *adminEnv, _ time.Time) error {
			return q.DeleteUser(a.sam.ID)
		}, func(t *testing.T, err error) { wantCode(t, err, api.CodeResetTokenInvalid) }},
		{"the account was renamed to the password", func(q *store.Q, a *adminEnv, now time.Time) error {
			return q.RenameUser(a.sam.ID, "another.long.passphrase", "another.long.passphrase", now)
		}, func(t *testing.T, err error) { wantFields(t, err, map[string]string{"password": "same_as_username"}) }},
	} {
		a := newAdminEnv(t)
		token := a.resetLink(a.asAlex, a.sam.ID, "")
		a.conns.take()
		a.changeDuringCheck(func(q *store.Q, now time.Time) error { return tc.change(q, a, now) })
		_, err := a.svc.CompletePasswordReset(context.Background(), token, "another.long.passphrase", meta(ipB))
		tc.check(t, err)
		if t.Failed() {
			t.Fatalf("(%s)", tc.name)
		}
		if u := a.userRow(a.sam.ID); (u != nil && u.PasswordHash != "") || a.rawCount("sessions") != 1 ||
			a.auditCount(auditPasswordResetCompleted) != 0 || len(a.conns.take()) != 0 {
			t.Errorf("%s: the refused completion changed something: %+v", tc.name, u)
		}
	}
}
