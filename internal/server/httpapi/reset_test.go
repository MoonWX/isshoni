package httpapi

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/auth"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// Admin-issued password resets over REST (README S64; 03 §15 "Passwords", 03 §7.10).

// issueReset sends POST /api/v1/admin/users/{id}/password-reset with a raw body ("" for none).
func (f *authFixture) issueReset(c *http.Cookie, id, body string, opts ...reqOpt) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.call(http.MethodPost, "/api/v1/admin/users/"+id+"/password-reset", body, append([]reqOpt{with(c)}, opts...)...)
}

// resetToken issues a reset link over REST and returns the token of its fragment.
func (f *authFixture) resetToken(c *http.Cookie, id, body string) string {
	f.t.Helper()
	rec := f.issueReset(c, id, body)
	link := decodeBody[api.ResetLink](f.t, rec, http.StatusCreated)
	token, ok := strings.CutPrefix(link.URL, testOrigin+"/reset#")
	if !ok || len(token) != 43 || strings.ContainsAny(token, "/?#&=") {
		f.t.Fatalf("reset url %q: want %s/reset#<43-character token>", link.URL, testOrigin)
	}
	if want := f.clk.now().Add(24 * time.Hour); !link.ExpiresAt.Equal(want) {
		f.t.Fatalf("the link expires at %v, want %v", link.ExpiresAt, want)
	}
	return token
}

// resetCheck sends POST /api/v1/auth/reset/check from address ip.
func (f *authFixture) resetCheck(token, ip string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.call(http.MethodPost, "/api/v1/auth/reset/check", jsonBody(f.t, api.TokenRequest{Token: token}), from(ip))
}

// resetComplete sends POST /api/v1/auth/reset/complete from address ip.
func (f *authFixture) resetComplete(token, password, ip string, opts ...reqOpt) *httptest.ResponseRecorder {
	f.t.Helper()
	body := jsonBody(f.t, api.ResetCompleteRequest{Token: token, Password: password})
	return f.call(http.MethodPost, "/api/v1/auth/reset/complete", body, append([]reqOpt{from(ip)}, opts...)...)
}

// TestPasswordResetOverREST is 03 §15's passwords case for an admin reset: it makes the old cookie get 401, makes
// the old password get 401 invalid_credentials, and the reset link logs the user in; the link can't be used twice.
func TestPasswordResetOverREST(t *testing.T) {
	f := newAuthFixture(t)
	admin, cookie, sam, samCookie := f.adminAndMember()
	samID := store.UserID(sam.ID)
	f.linkDevice(sam.ID, "d1d1d1d1d1d1")
	f.alerts.take()
	f.conns.take()
	count := f.counter()

	wantBody(t, f.issueReset(cookie, "nobody000000", `{}`), http.StatusNotFound, `{"error":{"code":"user_not_found"}}`)
	// The own account: /me/password is the place.
	wantBody(t, f.issueReset(cookie, admin.ID, `{"currentPassword":"`+testPassword+`"}`), http.StatusConflict,
		`{"error":{"code":"self_action_forbidden"}}`)
	for _, body := range []string{`{"currentPassword":`, `[]`, `{} {}`} {
		wantError(t, f.issueReset(cookie, sam.ID, body), http.StatusBadRequest, api.CodeBadRequest)
	}
	f.wantSignedIn(samCookie)
	if len(f.signal.takeNotes()) != 0 || count("SELECT count(*) FROM password_resets") != 0 {
		t.Fatal("refused resets did something")
	}

	// Issue → 201 {url, expiresAt}. A member's reset needs no password, and no body.
	f.clk.advance(time.Minute)
	f.wantSignedIn(samCookie) // in the session cache, as a request a moment ago leaves it
	rec := f.issueReset(cookie, sam.ID, "")
	link := decodeBody[api.ResetLink](t, rec, http.StatusCreated)
	token, ok := strings.CutPrefix(link.URL, testOrigin+"/reset#")
	if !ok || len(token) != 43 {
		t.Fatalf("reset url %q: want %s/reset#<43-character token>", link.URL, testOrigin)
	}
	if got, want := rec.Body.String(), `{"url":"`+link.URL+`","expiresAt":"2026-10-02T12:01:00.000Z"}`+"\n"; got != want {
		t.Errorf("the answer = %s, want %s", got, want)
	}
	// The database has no plaintext token, and neither has the audit log.
	if n := count("SELECT count(*) FROM password_resets WHERE instr(token_hash, ?) > 0", token) +
		count("SELECT count(*) FROM audit_log WHERE instr(detail, ?) > 0", token); n != 0 {
		t.Error("the token is in the database")
	}
	if n := count("SELECT count(*) FROM password_resets WHERE user_id = ? AND length(token_hash) = 32", sam.ID); n != 1 {
		t.Errorf("%d reset rows with a 32-byte hash for the user, want 1", n)
	}
	// The old cookie gets 401, the old password 401 invalid_credentials; the device is gone too.
	f.wantUnauthenticated(samCookie)
	wantError(t, f.login("Sam", testPassword, clientIP(1)), http.StatusUnauthorized, api.CodeInvalidCredentials)
	wantCalls := []closedConn{{auth.ConnSelector{UserID: samID}, auth.ReasonPasswordReset}}
	if calls := f.conns.take(); fmt.Sprint(calls) != fmt.Sprint(wantCalls) {
		t.Errorf("CloseConnections calls = %+v, want %+v", calls, wantCalls)
	}
	if notes := f.signal.takeNotes(); fmt.Sprint(notes) != "[admins [admin.users]]" {
		t.Errorf("Signal notes after the reset was issued = %v", notes)
	}
	if got := f.userRow(cookie, sam.ID); !got.ResetPending || got.Sessions != 0 || got.Devices != 0 || got.Status != api.UserStatusActive {
		t.Errorf("the account with a pending reset = %+v", got)
	}
	if rows := f.auditOf("user.password_reset_issued"); len(rows) != 1 || rows[0].Actor.Name != "Alex" || rows[0].Actor.IP != addrA ||
		rows[0].TargetKind != "user" || rows[0].TargetID != sam.ID || rows[0].TargetName != "Sam" || len(rows[0].Detail) != 0 {
		t.Errorf("user.password_reset_issued rows = %+v", rows)
	}
	if alerts := f.alerts.take(); len(alerts) != 0 {
		t.Errorf("a member's reset alerted: %+v", alerts)
	}

	// The link's page: whose password it is, or that the link is dead.
	wantBody(t, f.resetCheck(token, addrB), http.StatusOK, `{"username":"Sam"}`)
	for _, bad := range []string{"", "x", strings.Repeat("A", 43), token[:42] + "!"} {
		wantBody(t, f.resetCheck(bad, addrB), http.StatusNotFound, `{"error":{"code":"reset_token_invalid"}}`)
		// A dead link is refused before the password is looked at.
		wantBody(t, f.resetComplete(bad, "short", addrB), http.StatusNotFound, `{"error":{"code":"reset_token_invalid"}}`)
	}
	// The password rules, with a live link.
	for pw, code := range map[string]string{"": "required", "short": "too_short", "password1": "too_common"} {
		wantBody(t, f.resetComplete(token, pw, addrB), http.StatusUnprocessableEntity,
			`{"error":{"code":"validation_failed","fields":{"password":"`+code+`"}}}`)
	}
	if len(f.signal.takeNotes()) != 0 || count("SELECT count(*) FROM sessions WHERE user_id = ?", sam.ID) != 0 {
		t.Fatal("refused completions did something")
	}

	// Complete → 200 {user} with the session cookie: the link logs the user in.
	f.clk.advance(time.Hour)
	rec = f.resetComplete(token, newPassword, addrB)
	if got, want := rec.Body.String(), `{"user":{"id":"`+sam.ID+`","username":"Sam","role":"user"}}`+"\n"; rec.Code != http.StatusOK || got != want {
		t.Fatalf("reset/complete = %d %s, want 200 %s", rec.Code, got, want)
	}
	fresh := f.sessionCookie(rec, thirtyDays)
	if me := decodeBody[api.Me](t, f.call(http.MethodGet, "/api/v1/me", "", with(fresh)), http.StatusOK); me.User.ID != sam.ID ||
		me.Session == nil || me.Session.Name != "Chrome on Windows" {
		t.Errorf("me behind the reset's cookie = %+v", me)
	}
	if notes := f.signal.takeNotes(); fmt.Sprint(notes) != "[user "+sam.ID+" [devices] admins [admin.users]]" {
		t.Errorf("Signal notes after the completion = %v", notes)
	}
	if rows := f.auditOf("user.password_reset_completed"); len(rows) != 1 || rows[0].Actor != (store.Actor{Kind: store.ActorUser,
		UserID: samID, Name: "Sam", IP: addrB}) || rows[0].TargetID != sam.ID || len(rows[0].Detail) != 0 {
		t.Errorf("user.password_reset_completed rows = %+v", rows)
	}
	if got := f.userRow(cookie, sam.ID); got.ResetPending || got.Sessions != 1 || !got.LastLoginAt.Equal(f.clk.now()) {
		t.Errorf("the account after the reset = %+v", got)
	}
	if calls := f.conns.take(); len(calls) != 0 {
		t.Errorf("a completion without an old cookie closed connections: %+v", calls)
	}
	// The link can't be used twice.
	wantBody(t, f.resetComplete(token, "a third long passphrase", addrB), http.StatusNotFound, `{"error":{"code":"reset_token_invalid"}}`)
	wantBody(t, f.resetCheck(token, addrB), http.StatusNotFound, `{"error":{"code":"reset_token_invalid"}}`)
	// The new password logs in; the old one never again.
	wantError(t, f.login("Sam", testPassword, clientIP(1)), http.StatusUnauthorized, api.CodeInvalidCredentials)
	decodeBody[api.UserResponse](t, f.login("Sam", newPassword, clientIP(1)), http.StatusOK)
	f.wantSignedIn(cookie) // the admin's own session was never touched
}

// unknownLength gives a request a body whose length the server is not told: a chunked one, or an HTTP/2 stream
// without a content-length.
func unknownLength(body io.ReadCloser) reqOpt {
	return func(r *http.Request) { r.ContentLength, r.Body = -1, body }
}

// TestPasswordResetWithoutBodyOverREST: a reset that needs no password needs no body, however the client leaves it
// out. What counts is that there are no bytes, not a Content-Length of 0: an empty body of unknown length and a
// blank one send no password either.
func TestPasswordResetWithoutBodyOverREST(t *testing.T) {
	f := newAuthFixture(t)
	_, cookie, sam, _ := f.adminAndMember()
	streamed := func(body string) []reqOpt {
		return []reqOpt{unknownLength(io.NopCloser(strings.NewReader(body)))}
	}
	issued := map[string]struct {
		body string
		opts []reqOpt
	}{
		"no body":                           {"", nil},
		"no body, of unknown length":        {"", []reqOpt{unknownLength(http.NoBody)}},
		"an empty body of unknown length":   {"", streamed("")},
		"a space":                           {" ", nil},
		"white space":                       {" \r\n\t", nil},
		"white space of unknown length":     {"", streamed("\n")},
		"an empty object":                   {`{}`, nil},
		"an empty object of unknown length": {"", streamed(`{}`)},
	}
	for name, send := range issued {
		f.clk.advance(time.Minute)
		rec := f.issueReset(cookie, sam.ID, send.body, send.opts...)
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s: status %d, body %s; want 201", name, rec.Code, rec.Body)
		}
		link := decodeBody[api.ResetLink](t, rec, http.StatusCreated)
		token, ok := strings.CutPrefix(link.URL, testOrigin+"/reset#")
		if !ok || !link.ExpiresAt.Equal(f.clk.now().Add(24*time.Hour)) {
			t.Fatalf("%s: the link = %+v", name, link)
		}
		wantBody(t, f.resetCheck(token, addrB), http.StatusOK, `{"username":"Sam"}`)
	}

	// A body that is there is held to the rules, whatever its length is said to be, and to the limit.
	for _, body := range []string{`x`, `{"currentPassword":`, `{} {}`, "\u00a0"} {
		wantError(t, f.issueReset(cookie, sam.ID, "", streamed(body)...), http.StatusBadRequest, api.CodeBadRequest)
	}
	wantError(t, f.issueReset(cookie, sam.ID, "", streamed(strings.Repeat(" ", otherBodyLimit+1))...),
		http.StatusRequestEntityTooLarge, api.CodePayloadTooLarge)
	if rows := f.auditOf("user.password_reset_issued"); len(rows) != len(issued) {
		t.Errorf("%d user.password_reset_issued rows, want the %d of the links that were issued", len(rows), len(issued))
	}
}

// TestResetAnotherAdminOverREST is the rest of 03 §15's passwords case: resetting another admin without
// currentPassword → 403 wrong_password. With it, the link and the admin_password_reset alert.
func TestResetAnotherAdminOverREST(t *testing.T) {
	f := newAuthFixture(t)
	_, cookie, sam, samCookie := f.adminAndMember()
	f.patched(cookie, sam.ID, `{"role":"admin","currentPassword":"`+testPassword+`"}`)
	f.alerts.take()
	f.conns.take()
	f.signal.takeNotes()

	for _, body := range []string{``, ` `, `{}`, `{"currentPassword":""}`, `{"currentPassword":"not the password"}`} {
		wantBody(t, f.issueReset(cookie, sam.ID, body), http.StatusForbidden, `{"error":{"code":"wrong_password"}}`)
	}
	f.wantSignedIn(samCookie)
	if alerts, notes := f.alerts.take(), f.signal.takeNotes(); len(alerts) != 0 || len(notes) != 0 || len(f.conns.take()) != 0 ||
		len(f.auditOf("user.password_reset_issued")) != 0 {
		t.Fatalf("refused resets: alerts %+v, notes %v", alerts, notes)
	}

	f.clk.advance(time.Minute)
	token := f.resetToken(cookie, sam.ID, `{"currentPassword":"`+testPassword+`"}`)
	want := auth.AdminAlert{Kind: auth.AlertAdminPasswordReset, Actor: "Alex", Target: "Sam", At: f.clk.now()}
	if alerts := f.alerts.take(); len(alerts) != 1 || alerts[0] != want {
		t.Errorf("alerts = %+v, want %+v", alerts, want)
	}
	f.wantUnauthenticated(samCookie)
	wantBody(t, f.resetCheck(token, addrB), http.StatusOK, `{"username":"Sam"}`)
	// The other admin completes the link and is an admin still.
	rec := f.resetComplete(token, newPassword, addrB)
	if got := decodeBody[api.UserResponse](t, rec, http.StatusOK).User; got.Role != api.RoleAdmin {
		t.Errorf("the admin after the reset = %+v", got)
	}
	if got := names(f.users(f.sessionCookie(rec, thirtyDays), "")); got != "Alex Sam" {
		t.Errorf("the reset admin's user list = %s", got)
	}
}

// TestResetCompleteWithOldCookieOverREST is the 03 §7.7 row "reset completion that arrives with an existing
// session cookie": the old session is deleted and ConnCloser gets that session with session_revoked.
func TestResetCompleteWithOldCookieOverREST(t *testing.T) {
	f := newAuthFixture(t)
	admin, cookie, sam, _ := f.adminAndMember()
	// A second browser of Alex's, which opens Sam's link.
	old := f.sessionCookie(f.login("Alex", testPassword, addrB), thirtyDays)
	oldID := f.sessionID(old)
	token := f.resetToken(cookie, sam.ID, `{}`)
	f.conns.take()

	// A completion that fails keeps the old session.
	wantError(t, f.resetComplete(token, "short", addrB, with(old)), http.StatusUnprocessableEntity, api.CodeValidationFailed)
	f.wantSignedIn(old)
	if calls := f.conns.take(); len(calls) != 0 {
		t.Fatalf("a failed completion closed connections: %+v", calls)
	}

	rec := f.resetComplete(token, newPassword, addrB, with(old))
	decodeBody[api.UserResponse](t, rec, http.StatusOK)
	fresh := f.sessionCookie(rec, thirtyDays)
	want := []closedConn{{auth.ConnSelector{UserID: store.UserID(admin.ID), SessionID: oldID}, auth.ReasonSessionRevoked}}
	if calls := f.conns.take(); fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("CloseConnections calls = %+v, want %+v", calls, want)
	}
	f.wantUnauthenticated(old)
	f.wantSignedIn(cookie)
	if me := decodeBody[api.Me](t, f.call(http.MethodGet, "/api/v1/me", "", with(fresh)), http.StatusOK); me.User.ID != sam.ID {
		t.Errorf("me behind the new cookie = %+v", me.User)
	}
}

// TestResetLinkLimitsOverREST: the link's two endpoints take a token of the auth-ip bucket each (03 §7.3), and a
// link of an account that is disabled can't be used until the account is enabled.
func TestResetLinkLimitsOverREST(t *testing.T) {
	f := newAuthFixture(t)
	_, cookie, sam, _ := f.adminAndMember()
	token := f.resetToken(cookie, sam.ID, `{}`)

	const busy = "203.0.113.200"
	for range 20 {
		wantBody(t, f.resetCheck(token, busy), http.StatusOK, `{"username":"Sam"}`)
	}
	for _, rec := range []*httptest.ResponseRecorder{f.resetCheck(token, busy), f.resetComplete(token, newPassword, busy)} {
		if e := wantError(t, rec, http.StatusTooManyRequests, api.CodeRateLimited); e.RetryAfter != 15 || rec.Header().Get("Retry-After") != "15" {
			t.Errorf("the 21st attempt: retryAfter %d, Retry-After %q; want 15", e.RetryAfter, rec.Header().Get("Retry-After"))
		}
	}

	// Disabling the account deletes its link; a link issued while it is disabled waits for it to be enabled.
	f.patched(cookie, sam.ID, `{"status":"disabled"}`)
	wantError(t, f.resetCheck(token, addrB), http.StatusNotFound, api.CodeResetTokenInvalid)
	token = f.resetToken(cookie, sam.ID, `{}`)
	wantBody(t, f.resetCheck(token, addrB), http.StatusForbidden, `{"error":{"code":"account_disabled"}}`)
	wantBody(t, f.resetComplete(token, newPassword, addrB), http.StatusForbidden, `{"error":{"code":"account_disabled"}}`)
	f.patched(cookie, sam.ID, `{"status":"active"}`)
	decodeBody[api.UserResponse](t, f.resetComplete(token, newPassword, addrB), http.StatusOK)
}
