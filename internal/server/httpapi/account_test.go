package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/auth"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// Self-service over REST (README S58; 03 §15 "Sessions" and "Passwords"), on the fixture of 03 §15: the router and
// the /api/v1 chain over a real store and a real auth.Service, with fake Signal and ConnCloser.

const newPassword = "another long passphrase"

// linkDevice inserts a linked app of user behind the store's back: linking is M2, so the store has no writer for
// a device.
func (f *authFixture) linkDevice(user, id string) {
	f.t.Helper()
	ms := f.clk.now().UnixMilli()
	_, err := f.rawDB().ExecContext(context.Background(), `INSERT INTO devices (id, user_id, name, client_kind, os,
		app_version, linked_via, created_at, last_seen_at, last_ip)
		VALUES (?, ?, 'Alex-PC', 'desktop', 'windows', '0.2.0', 'device_flow', ?, ?, '203.0.113.7')`, id, user, ms, ms)
	if err != nil {
		f.t.Fatal(err)
	}
}

// sessionList is GET /api/v1/me/sessions as the browser behind c sees it.
func (f *authFixture) sessionList(c *http.Cookie) []api.SessionInfo {
	f.t.Helper()
	rec := f.call(http.MethodGet, "/api/v1/me/sessions", "", with(c))
	return decodeBody[api.SessionsResponse](f.t, rec, http.StatusOK).Sessions
}

// deviceList is GET /api/v1/me/devices.
func (f *authFixture) deviceList(c *http.Cookie) []api.DeviceInfo {
	f.t.Helper()
	rec := f.call(http.MethodGet, "/api/v1/me/devices", "", with(c))
	return decodeBody[api.DevicesResponse](f.t, rec, http.StatusOK).Devices
}

// changePassword sends POST /api/v1/me/password.
func (f *authFixture) changePassword(c *http.Cookie, current, next string, opts ...reqOpt) *httptest.ResponseRecorder {
	f.t.Helper()
	body := jsonBody(f.t, api.ChangePasswordRequest{CurrentPassword: current, NewPassword: next})
	return f.call(http.MethodPost, "/api/v1/me/password", body, append([]reqOpt{with(c)}, opts...)...)
}

// wantNoContent checks a 204 without a body.
func wantNoContent(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("status %d, body %q; want 204 without a body", rec.Code, rec.Body)
	}
}

// wantCleared checks that a response clears the session cookie, and sets no other.
func wantCleared(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Header().Values("Set-Cookie"); len(got) != 1 || got[0] != clearedCookie {
		t.Errorf("Set-Cookie = %q, want %q", got, clearedCookie)
	}
}

// wantUnauthenticated checks that the session behind c is gone.
func (f *authFixture) wantUnauthenticated(c *http.Cookie) {
	f.t.Helper()
	wantError(f.t, f.call(http.MethodGet, "/api/v1/me", "", with(c)), http.StatusUnauthorized, api.CodeUnauthenticated)
}

// wantSignedIn checks that the session behind c works.
func (f *authFixture) wantSignedIn(c *http.Cookie) {
	f.t.Helper()
	decodeBody[api.Me](f.t, f.call(http.MethodGet, "/api/v1/me", "", with(c)), http.StatusOK)
}

// TestSessionsOverREST is 03 §15's sessions case: the list marks current; revoking one → that cookie gets 401;
// revoke-others keeps the current one; logout-everywhere clears the cookie and deletes the user's devices; the 51st
// session evicts the least recently seen. It also follows the ConnCloser calls, the Signal hooks and the audit rows.
func TestSessionsOverREST(t *testing.T) {
	f := newAuthFixture(t)
	admin, first := f.setupAdmin("Alex") // from address A
	_, memberToken := f.createInvite(first, `{}`)
	samUser, sam := f.join(memberToken, "Sam", clientIP(1))
	t0 := f.clk.now()
	f.clk.advance(time.Minute)
	second := f.sessionCookie(f.login("Alex", testPassword, addrB), thirtyDays)
	f.clk.advance(time.Minute)
	third := f.sessionCookie(f.login("Alex", testPassword, addrC), thirtyDays)
	f.clk.advance(time.Minute)
	alex := store.UserID(admin.ID)
	ids := map[string]string{} // browser → session ID
	for name, c := range map[string]*http.Cookie{"first": first, "second": second, "third": third, "sam": sam} {
		ids[name] = string(f.sessionID(c))
	}
	f.conns.take()
	f.signal.takeNotes()

	// The list: this browser first and marked current, then the others, the most recently seen first.
	rec := f.call(http.MethodGet, "/api/v1/me/sessions", "", with(second))
	list := decodeBody[api.SessionsResponse](t, rec, http.StatusOK).Sessions
	want := []api.SessionInfo{
		{ID: ids["second"], Name: "Chrome on Windows", CreatedAt: t0.Add(time.Minute), LastSeenAt: t0.Add(time.Minute), LastIP: addrB, Current: true},
		{ID: ids["third"], Name: "Chrome on Windows", CreatedAt: t0.Add(2 * time.Minute), LastSeenAt: t0.Add(2 * time.Minute), LastIP: addrC},
		{ID: ids["first"], Name: "Chrome on Windows", CreatedAt: t0, LastSeenAt: t0, LastIP: addrA},
	}
	if fmt.Sprint(list) != fmt.Sprint(want) {
		t.Errorf("sessions = %+v\nwant %+v", list, want)
	}
	var shape map[string][]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &shape); err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(keysOf(shape["sessions"][0])), "[createdAt current id lastIp lastSeenAt name]"; got != want {
		t.Errorf("session keys = %s, want %s (03 §12.4.3)", got, want)
	}
	if got := shape["sessions"][0]["lastSeenAt"]; got != "2026-10-01T12:01:00.000Z" {
		t.Errorf("lastSeenAt = %v, want the fixed REST form", got)
	}
	// Every browser sees the same sessions with its own marked.
	if got := f.sessionList(first); len(got) != 3 || got[0].ID != ids["first"] || !got[0].Current || got[1].Current || got[2].Current {
		t.Errorf("the first browser's list = %+v", got)
	}
	// An expired row the janitor has not pruned yet is no session any more.
	f.write(func(q *store.Q) error {
		past := f.clk.now().Add(-40 * 24 * time.Hour)
		return q.CreateSession(&store.Session{UserID: alex, TokenHash: []byte("stale-hash"), Name: "Safari on iPhone",
			CreatedAt: past, IdleExpiresAt: past.Add(30 * 24 * time.Hour), ExpiresAt: past.Add(180 * 24 * time.Hour)})
	})
	if got := f.sessionList(second); len(got) != 3 {
		t.Errorf("%d sessions listed with an expired row in the table, want 3", len(got))
	}

	// Revoking one: 204; that cookie gets 401, its connections are closed and the user's tabs refetch the list.
	rec = f.call(http.MethodDelete, "/api/v1/me/sessions/"+ids["third"], "", with(second), from(addrB))
	wantNoContent(t, rec)
	if got := rec.Header().Values("Set-Cookie"); len(got) != 0 {
		t.Errorf("revoking another session set a cookie: %q", got)
	}
	f.wantUnauthenticated(third)
	wantCalls := []closedConn{{auth.ConnSelector{UserID: alex, SessionID: store.SessionID(ids["third"])}, auth.ReasonSessionRevoked}}
	if calls := f.conns.take(); fmt.Sprint(calls) != fmt.Sprint(wantCalls) {
		t.Errorf("CloseConnections calls = %+v, want %+v", calls, wantCalls)
	}
	if notes := f.signal.takeNotes(); fmt.Sprint(notes) != "[user "+admin.ID+" [devices]]" {
		t.Errorf("Signal notes after a revocation = %v", notes)
	}
	if rows := f.auditOf("session.revoked"); len(rows) != 1 || rows[0].Actor.Name != "Alex" || rows[0].Actor.IP != addrB ||
		rows[0].TargetID != ids["third"] || fmt.Sprint(rows[0].Detail) != "map[name:Chrome on Windows]" {
		t.Errorf("session.revoked rows = %+v", rows)
	}
	// It is gone: once more is 404, like an unknown ID and another user's session, and nothing else happens.
	for _, id := range []string{ids["third"], "aaaaaaaaaaaa", "x", ids["sam"]} {
		rec := f.call(http.MethodDelete, "/api/v1/me/sessions/"+id, "", with(second))
		wantError(t, rec, http.StatusNotFound, api.CodeNotFound)
		if got, want := rec.Body.String(), `{"error":{"code":"not_found"}}`+"\n"; got != want {
			t.Errorf("body = %s, want %s", got, want)
		}
	}
	f.wantSignedIn(sam)
	if calls, notes := f.conns.take(), f.signal.takeNotes(); len(calls) != 0 || len(notes) != 0 {
		t.Errorf("refused revocations: calls %+v, notes %v", calls, notes)
	}

	// Sign out other browsers: 200 {revoked}; this one stays. The number is the browsers the list showed (the
	// first); the expired row goes with it and is not counted.
	rec = f.call(http.MethodPost, "/api/v1/me/sessions/revoke-others", "{}", with(second))
	if got := decodeBody[api.RevokeOthersResponse](t, rec, http.StatusOK); got.Revoked != 1 {
		t.Errorf("revoked = %d, want 1", got.Revoked)
	}
	if got, want := rec.Body.String(), `{"revoked":1}`+"\n"; got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
	f.wantUnauthenticated(first)
	f.wantSignedIn(second)
	if got := f.sessionList(second); len(got) != 1 || got[0].ID != ids["second"] || !got[0].Current {
		t.Errorf("sessions after revoke-others = %+v", got)
	}
	if calls := f.conns.take(); len(calls) != 2 || calls[0].reason != auth.ReasonSessionRevoked || calls[0].sel.ExceptSessionID != "" ||
		calls[0].sel.SessionID == "" || calls[0].sel.SessionID == store.SessionID(ids["second"]) {
		t.Errorf("CloseConnections calls = %+v, want one per deleted row", calls)
	}
	if n := f.counter()("SELECT count(*) FROM sessions WHERE user_id = ?", admin.ID); n != 1 {
		t.Errorf("%d session rows after revoke-others, want this browser's only (the expired row goes too)", n)
	}
	if notes := f.signal.takeNotes(); fmt.Sprint(notes) != "[user "+admin.ID+" [devices]]" {
		t.Errorf("Signal notes after revoke-others = %v", notes)
	}
	// With nobody else signed in it revokes nothing, and tells nobody. The body may be missing.
	rec = f.call(http.MethodPost, "/api/v1/me/sessions/revoke-others", "", with(second))
	if got := decodeBody[api.RevokeOthersResponse](t, rec, http.StatusOK); got.Revoked != 0 {
		t.Errorf("revoked = %d, want 0", got.Revoked)
	}
	if calls, notes := f.conns.take(), f.signal.takeNotes(); len(calls) != 0 || len(notes) != 0 {
		t.Errorf("revoke-others with no other session: calls %+v, notes %v", calls, notes)
	}

	// Revoking this browser's own session through the list's route works like a logout: the cookie is cleared.
	fourth := f.sessionCookie(f.login("Alex", testPassword, addrC), thirtyDays)
	rec = f.call(http.MethodDelete, "/api/v1/me/sessions/"+string(f.sessionID(fourth)), "", with(fourth))
	wantNoContent(t, rec)
	wantCleared(t, rec)
	f.wantUnauthenticated(fourth)
	f.wantSignedIn(second)

	// Linked apps: none before M2 links one. With a row in the table, the list shows it and Revoke removes it.
	rec = f.call(http.MethodGet, "/api/v1/me/devices", "", with(second))
	if got, want := rec.Body.String(), `{"devices":[]}`+"\n"; rec.Code != http.StatusOK || got != want {
		t.Errorf("devices = %d %s, want 200 %s", rec.Code, got, want)
	}
	linked := f.clk.now()
	f.linkDevice(admin.ID, "d1d1d1d1d1d1")
	f.linkDevice(samUser.ID, "d2d2d2d2d2d2")
	f.conns.take()
	f.signal.takeNotes()
	rec = f.call(http.MethodGet, "/api/v1/me/devices", "", with(second))
	devices := decodeBody[api.DevicesResponse](t, rec, http.StatusOK).Devices
	wantDevice := api.DeviceInfo{ID: "d1d1d1d1d1d1", Name: "Alex-PC", ClientKind: "desktop", OS: "windows", AppVersion: "0.2.0",
		CreatedAt: linked, LastSeenAt: linked, LastIP: "203.0.113.7"}
	if len(devices) != 1 || devices[0] != wantDevice {
		t.Errorf("devices = %+v, want %+v", devices, wantDevice)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &shape); err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(keysOf(shape["devices"][0])), "[appVersion clientKind createdAt id lastIp lastSeenAt name os]"; got != want {
		t.Errorf("device keys = %s, want %s (03 §12.4.3)", got, want)
	}
	for _, id := range []string{"aaaaaaaaaaaa", "d2d2d2d2d2d2"} { // unknown, and another user's
		wantError(t, f.call(http.MethodDelete, "/api/v1/me/devices/"+id, "", with(second)), http.StatusNotFound, api.CodeNotFound)
	}
	wantNoContent(t, f.call(http.MethodDelete, "/api/v1/me/devices/d1d1d1d1d1d1", "", with(second)))
	if got := f.deviceList(second); len(got) != 0 {
		t.Errorf("devices after the revocation = %+v", got)
	}
	wantCalls = []closedConn{{auth.ConnSelector{UserID: alex, DeviceID: "d1d1d1d1d1d1"}, auth.ReasonDeviceRevoked}}
	if calls := f.conns.take(); fmt.Sprint(calls) != fmt.Sprint(wantCalls) {
		t.Errorf("CloseConnections calls = %+v, want %+v", calls, wantCalls)
	}
	if notes := f.signal.takeNotes(); fmt.Sprint(notes) != "[user "+admin.ID+" [devices]]" {
		t.Errorf("Signal notes after a device revocation = %v", notes)
	}
	wantError(t, f.call(http.MethodDelete, "/api/v1/me/devices/d1d1d1d1d1d1", "", with(second)), http.StatusNotFound, api.CodeNotFound)

	// Log out everywhere: 204 with the cookie cleared; every session and device of the user is gone.
	f.linkDevice(admin.ID, "d3d3d3d3d3d3")
	fifth := f.sessionCookie(f.login("Alex", testPassword, addrC), thirtyDays)
	count := f.counter()
	f.conns.take()
	f.signal.takeNotes()
	rec = f.call(http.MethodPost, "/api/v1/auth/logout-everywhere", "{}", with(second), from(addrB))
	wantNoContent(t, rec)
	wantCleared(t, rec)
	f.wantUnauthenticated(second)
	f.wantUnauthenticated(fifth)
	if s, d := count("SELECT count(*) FROM sessions WHERE user_id = ?", admin.ID), count("SELECT count(*) FROM devices WHERE user_id = ?", admin.ID); s != 0 || d != 0 {
		t.Errorf("%d sessions and %d devices left after logout-everywhere", s, d)
	}
	wantCalls = []closedConn{{auth.ConnSelector{UserID: alex}, auth.ReasonLoggedOut}}
	if calls := f.conns.take(); fmt.Sprint(calls) != fmt.Sprint(wantCalls) {
		t.Errorf("CloseConnections calls = %+v, want %+v", calls, wantCalls)
	}
	if notes := f.signal.takeNotes(); fmt.Sprint(notes) != "[user "+admin.ID+" [devices]]" {
		t.Errorf("Signal notes after logout-everywhere = %v", notes)
	}
	if rows := f.auditOf("auth.logout_everywhere"); len(rows) != 1 || rows[0].Actor.Name != "Alex" || rows[0].Actor.IP != addrB ||
		rows[0].TargetID != admin.ID || fmt.Sprint(rows[0].Detail) != "map[devices:1 sessions:2]" {
		t.Errorf("auth.logout_everywhere rows = %+v", rows)
	}
	// Sam's sessions and device were nobody's business.
	f.wantSignedIn(sam)
	if d := count("SELECT count(*) FROM devices"); d != 1 {
		t.Errorf("%d devices left in all, want Sam's", d)
	}
	// The route needs a session: once more with the dead cookie is 401, like any User route.
	wantError(t, f.call(http.MethodPost, "/api/v1/auth/logout-everywhere", "{}", with(second)), http.StatusUnauthorized, api.CodeUnauthenticated)

	// The 51st session evicts the least recently seen.
	var cookies []*http.Cookie
	for i := range 51 {
		f.clk.advance(time.Second)
		cookies = append(cookies, f.sessionCookie(f.login("Alex", testPassword, clientIP(i)), thirtyDays))
		if i == 49 {
			f.wantSignedIn(cookies[0])
			f.conns.take()
		}
	}
	f.wantUnauthenticated(cookies[0])
	f.wantSignedIn(cookies[1])
	f.wantSignedIn(cookies[50])
	if calls := f.conns.take(); len(calls) != 1 || calls[0].reason != auth.ReasonSessionRevoked || calls[0].sel.SessionID == "" {
		t.Errorf("CloseConnections calls after the 51st login = %+v, want the evicted session", calls)
	}
	if n := count("SELECT count(*) FROM sessions WHERE user_id = ?", admin.ID); n != 50 {
		t.Errorf("%d sessions after 51 logins, want 50", n)
	}
}

// TestPasswordOverREST is 03 §15's passwords case for self-service: changing the password keeps the current
// session and kills the others. And POST /me/password's answers (03 §12.3 #12).
func TestPasswordOverREST(t *testing.T) {
	f := newAuthFixture(t)
	admin, cookie := f.setupAdmin("Alex")
	alex := store.UserID(admin.ID)
	other := f.sessionCookie(f.login("Alex", testPassword, addrB), thirtyDays)
	sid := f.sessionID(cookie)
	f.linkDevice(admin.ID, "d1d1d1d1d1d1")
	count := f.counter()
	f.conns.take()
	f.signal.takeNotes()

	// Refused: the new password's rules (422 with the field), then the current password (403).
	for body, fields := range map[string]string{
		`{}`: `{"newPassword":"required"}`,
		`{"currentPassword":"` + testPassword + `","newPassword":"short"}`:     `{"newPassword":"too_short"}`,
		`{"currentPassword":"not the password","newPassword":"password1"}`:     `{"newPassword":"too_common"}`,
		`{"currentPassword":"` + testPassword + `","newPassword":"correct\n"}`: `{"newPassword":"invalid"}`,
	} {
		rec := f.call(http.MethodPost, "/api/v1/me/password", body, with(cookie))
		wantError(t, rec, http.StatusUnprocessableEntity, api.CodeValidationFailed)
		if got, want := rec.Body.String(), `{"error":{"code":"validation_failed","fields":`+fields+`}}`+"\n"; got != want {
			t.Errorf("POST /me/password %s: body %s, want %s", body, got, want)
		}
	}
	for _, body := range []string{
		`{"currentPassword":"not the password","newPassword":"` + newPassword + `"}`,
		`{"newPassword":"` + newPassword + `"}`, // without it: the re-authentication failed
		`{"currentPassword":"","newPassword":"` + newPassword + `"}`,
	} {
		rec := f.call(http.MethodPost, "/api/v1/me/password", body, with(cookie))
		wantError(t, rec, http.StatusForbidden, api.CodeWrongPassword)
		if got, want := rec.Body.String(), `{"error":{"code":"wrong_password"}}`+"\n"; got != want {
			t.Errorf("POST /me/password %s: body %s, want %s", body, got, want)
		}
	}
	for _, body := range []string{``, `{"newPassword":`, `[]`, `{} {}`} {
		wantError(t, f.call(http.MethodPost, "/api/v1/me/password", body, with(cookie)), http.StatusBadRequest, api.CodeBadRequest)
	}
	f.wantSignedIn(other)
	if calls, notes := f.conns.take(), f.signal.takeNotes(); len(calls) != 0 || len(notes) != 0 || len(f.auditOf("auth.password_changed")) != 0 {
		t.Fatalf("refused changes: calls %+v, notes %v", calls, notes)
	}

	// The change: 200 {} with this session's cookie rotated.
	f.clk.advance(time.Hour)
	rec := f.changePassword(cookie, testPassword, newPassword, from(addrC))
	if got := rec.Body.String(); rec.Code != http.StatusOK || got != "{}\n" {
		t.Fatalf("POST /me/password = %d %q, want 200 {}", rec.Code, got)
	}
	rotated := f.sessionCookie(rec, thirtyDays)
	if rotated.Value == cookie.Value {
		t.Fatal("the cookie was not rotated")
	}
	// The current session stays, under the new cookie. The cookie the request came with is let in for the 60 s of
	// a rotation (03 §7.4): this browser's other tabs hold it until the new one reaches them. The others are gone
	// at once, and so is the linked app.
	f.wantSignedIn(cookie)
	f.wantUnauthenticated(other)
	if got := f.sessionID(rotated); got != sid {
		t.Errorf("the session behind the new cookie is %s, want the same session %s", got, sid)
	}
	if s, d := count("SELECT count(*) FROM sessions WHERE user_id = ?", admin.ID), count("SELECT count(*) FROM devices"); s != 1 || d != 0 {
		t.Errorf("%d sessions and %d devices after the change, want 1 and 0", s, d)
	}
	wantCalls := []closedConn{{auth.ConnSelector{UserID: alex, ExceptSessionID: sid}, auth.ReasonPasswordChanged}}
	if calls := f.conns.take(); fmt.Sprint(calls) != fmt.Sprint(wantCalls) {
		t.Errorf("CloseConnections calls = %+v, want %+v", calls, wantCalls)
	}
	if notes := f.signal.takeNotes(); fmt.Sprint(notes) != "[user "+admin.ID+" [devices]]" {
		t.Errorf("Signal notes after the change = %v", notes)
	}
	if rows := f.auditOf("auth.password_changed"); len(rows) != 1 || rows[0].Actor.Name != "Alex" || rows[0].Actor.IP != addrC ||
		rows[0].TargetKind != "user" || rows[0].TargetID != admin.ID || len(rows[0].Detail) != 0 {
		t.Errorf("auth.password_changed rows = %+v", rows)
	}
	// The old cookie's 60 s: still in at their end, out a millisecond later. Using it changed nothing.
	f.clk.advance(60 * time.Second)
	f.wantSignedIn(cookie)
	f.clk.advance(time.Millisecond)
	f.wantUnauthenticated(cookie)
	f.wantSignedIn(rotated)
	if calls, notes := f.conns.take(), f.signal.takeNotes(); len(calls) != 0 || len(notes) != 0 {
		t.Errorf("requests under the old cookie: calls %+v, notes %v", calls, notes)
	}
	// The old password is none any more.
	wantError(t, f.login("Alex", testPassword, addrB), http.StatusUnauthorized, api.CodeInvalidCredentials)
	decodeBody[api.UserResponse](t, f.login("Alex", newPassword, addrB), http.StatusOK)

	// A session whose daily rotation is due: the chain rotates, then the handler; the response sets one cookie,
	// the handler's. The cookie the request came with is the one the browser holds, so it is the one with the 60 s,
	// although the chain's token came between it and the new one.
	f.clk.advance(25 * time.Hour)
	rec = f.changePassword(rotated, newPassword, "a third long passphrase")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /me/password after a day = %d %s", rec.Code, rec.Body)
	}
	latest := f.sessionCookie(rec, thirtyDays) // fails unless there is exactly one
	f.wantSignedIn(latest)
	f.wantSignedIn(rotated)
	f.clk.advance(60 * time.Second)
	f.wantSignedIn(rotated)
	f.clk.advance(time.Millisecond)
	f.wantUnauthenticated(rotated)
	f.wantSignedIn(latest)
	if got := f.sessionID(latest); got != sid {
		t.Errorf("the session behind the latest cookie is %s, want the same session %s", got, sid)
	}

	// Wrong current passwords are failed password checks (03 §7.3): the sixth from one address is 429.
	for range 5 {
		wantError(t, f.changePassword(latest, "not the password", newPassword, from(clientIP(7))), http.StatusForbidden, api.CodeWrongPassword)
	}
	rec = f.changePassword(latest, "a third long passphrase", newPassword, from(clientIP(7)))
	if e := wantError(t, rec, http.StatusTooManyRequests, api.CodeRateLimited); e.RetryAfter != 120 || rec.Header().Get("Retry-After") != "120" {
		t.Errorf("the throttled change: retryAfter %d, Retry-After %q; want 120", e.RetryAfter, rec.Header().Get("Retry-After"))
	}
	f.wantSignedIn(latest)
}

// TestDeleteSelfOverREST: POST /me/delete (03 §12.3 #13): 204 with the cookie cleared; 403 wrong_password; 409
// last_admin for the only admin (03 §15 "Roles": also for self-deletion).
func TestDeleteSelfOverREST(t *testing.T) {
	f := newAuthFixture(t)
	_, adminCookie, member, memberCookie := f.adminAndMember()
	f.conns.take()
	del := func(c *http.Cookie, body string, opts ...reqOpt) *httptest.ResponseRecorder {
		return f.call(http.MethodPost, "/api/v1/me/delete", body, append([]reqOpt{with(c)}, opts...)...)
	}

	// The only admin can't leave, whatever it sends.
	for _, body := range []string{`{"password":"` + testPassword + `"}`, `{"password":"not the password"}`, `{}`} {
		rec := del(adminCookie, body)
		wantError(t, rec, http.StatusConflict, api.CodeLastAdmin)
		if got, want := rec.Body.String(), `{"error":{"code":"last_admin"}}`+"\n"; got != want {
			t.Errorf("body = %s, want %s", got, want)
		}
	}
	// A member: the password decides. A missing one is a failed re-authentication, not a field error.
	for _, body := range []string{`{"password":"not the password"}`, `{}`, `{"password":""}`} {
		wantError(t, del(memberCookie, body), http.StatusForbidden, api.CodeWrongPassword)
	}
	wantError(t, del(memberCookie, `{"password":`), http.StatusBadRequest, api.CodeBadRequest)
	f.wantSignedIn(memberCookie)
	if calls, notes := f.conns.take(), f.signal.takeNotes(); len(calls) != 0 || len(notes) != 0 || f.tableRows("users") != 2 {
		t.Fatalf("refused deletions: calls %+v, notes %v, %d users", calls, notes, f.tableRows("users"))
	}

	rec := del(memberCookie, `{"password":"`+testPassword+`"}`, from(addrC))
	wantNoContent(t, rec)
	wantCleared(t, rec)
	f.wantUnauthenticated(memberCookie)
	if n := f.tableRows("users"); n != 1 {
		t.Errorf("%d users after the deletion, want 1", n)
	}
	wantCalls := []closedConn{{auth.ConnSelector{UserID: store.UserID(member.ID)}, auth.ReasonAccountDeleted}}
	if calls := f.conns.take(); fmt.Sprint(calls) != fmt.Sprint(wantCalls) {
		t.Errorf("CloseConnections calls = %+v, want %+v", calls, wantCalls)
	}
	// The admins' user list changed (03 §12.5).
	if notes := f.signal.takeNotes(); fmt.Sprint(notes) != "[admins [admin.users]]" {
		t.Errorf("Signal notes after the deletion = %v", notes)
	}
	if rows := f.auditOf("user.deleted"); len(rows) != 1 || rows[0].Actor.Name != "Sam" || rows[0].Actor.IP != addrC ||
		rows[0].TargetID != member.ID || rows[0].TargetName != "Sam" || fmt.Sprint(rows[0].Detail) != "map[self:true]" {
		t.Errorf("user.deleted rows = %+v", rows)
	}
	// The account is gone and its name is free again.
	wantError(t, f.login("Sam", testPassword, addrB), http.StatusUnauthorized, api.CodeInvalidCredentials)
	_, token := f.createInvite(adminCookie, `{}`)
	again, _ := f.join(token, "Sam", clientIP(3))
	if again.ID == member.ID {
		t.Error("the new Sam has the deleted account's ID")
	}
	// The admin is still the only one.
	wantError(t, del(adminCookie, `{"password":"`+testPassword+`"}`), http.StatusConflict, api.CodeLastAdmin)
	f.wantSignedIn(adminCookie)
}

// TestSessionEndWhenRotationIsDue: a request that ends its own session while that session's daily rotation is due
// (the chain rotates before the handler runs, 03 §7.4) answers with one Set-Cookie, the one that clears the cookie,
// and the rotated token is no session either.
func TestSessionEndWhenRotationIsDue(t *testing.T) {
	for name, end := range map[string]struct{ method, path, body string }{
		"logout-everywhere":     {http.MethodPost, "/api/v1/auth/logout-everywhere", `{}`},
		"deleting the account":  {http.MethodPost, "/api/v1/me/delete", `{"password":"` + testPassword + `"}`},
		"revoking this session": {http.MethodDelete, "/api/v1/me/sessions/", ``},
	} {
		f := newAuthFixture(t)
		_, _, _, cookie := f.adminAndMember()
		if end.method == http.MethodDelete {
			end.path += string(f.sessionID(cookie))
		}
		f.clk.advance(25 * time.Hour)
		rec := f.call(end.method, end.path, end.body, with(cookie))
		wantNoContent(t, rec)
		if got := rec.Header().Values("Set-Cookie"); len(got) != 1 || got[0] != clearedCookie {
			t.Errorf("%s with a rotation due: Set-Cookie %q, want only %q", name, got, clearedCookie)
		}
		f.wantUnauthenticated(cookie)
		if n := f.counter()("SELECT count(*) FROM sessions s JOIN users u ON u.id = s.user_id WHERE u.username = 'Sam'"); n != 0 {
			t.Errorf("%s: %d sessions of the user left", name, n)
		}
	}
}

// TestSelfServiceRoutes: the routes of this slice sit behind the /api/v1 chain like every other: a session is
// needed, JSON 405 with Allow, the CSRF check and the Content-Type rule on their unsafe methods (03 §12.1).
func TestSelfServiceRoutes(t *testing.T) {
	f := newAuthFixture(t)
	admin, cookie := f.setupAdmin("Alex")
	other := f.sessionCookie(f.login("Alex", testPassword, addrB), thirtyDays)
	otherID := string(f.sessionID(other))
	f.linkDevice(admin.ID, "d1d1d1d1d1d1")
	f.conns.take()

	routes := []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/auth/logout-everywhere", `{}`},
		{http.MethodPost, "/api/v1/me/password", `{"currentPassword":"` + testPassword + `","newPassword":"` + newPassword + `"}`},
		{http.MethodPost, "/api/v1/me/delete", `{"password":"` + testPassword + `"}`},
		{http.MethodGet, "/api/v1/me/sessions", ``},
		{http.MethodDelete, "/api/v1/me/sessions/" + otherID, ``},
		{http.MethodPost, "/api/v1/me/sessions/revoke-others", `{}`},
		{http.MethodGet, "/api/v1/me/devices", ``},
		{http.MethodDelete, "/api/v1/me/devices/d1d1d1d1d1d1", ``},
	}
	for _, rt := range routes {
		// User routes: nobody without a session, and no bearer tokens in M1.
		for name, opts := range map[string][]reqOpt{
			"no cookie":     nil,
			"a dead cookie": {cookieValue(sessionCookieName, strings.Repeat("A", 43))},
			"Authorization": {with(cookie), header("Authorization", "Bearer isa_"+strings.Repeat("A", 43))},
		} {
			rec := f.call(rt.method, rt.path, rt.body, opts...)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with %s: status %d, want 401", rt.method, rt.path, name, rec.Code)
			}
		}
		if rt.method == http.MethodGet {
			continue
		}
		// A cross-site request with the user's cookie changes nothing: 403 csrf_failed. Without a JSON Content-Type:
		// 415, also for a DELETE with no body.
		rec := f.call(rt.method, rt.path, rt.body, with(cookie), header("Sec-Fetch-Site", "cross-site"), header("Origin", "https://evil.example"))
		wantError(t, rec, http.StatusForbidden, api.CodeCSRFFailed)
		rec = f.call(rt.method, rt.path, rt.body, with(cookie), header("Content-Type", "text/plain"))
		wantError(t, rec, http.StatusUnsupportedMediaType, api.CodeUnsupportedMediaType)
		rec = f.call(rt.method, rt.path, rt.body, with(cookie), header("Content-Type", ""))
		wantError(t, rec, http.StatusUnsupportedMediaType, api.CodeUnsupportedMediaType)
	}
	// Nothing happened: both sessions, the device and the password are as they were.
	f.wantSignedIn(cookie)
	f.wantSignedIn(other)
	if got := f.deviceList(cookie); len(got) != 1 || len(f.conns.take()) != 0 {
		t.Errorf("refused requests changed something: devices %+v", got)
	}

	for path, allow := range map[string]string{
		"/api/v1/auth/logout-everywhere":       "POST",
		"/api/v1/me/password":                  "POST",
		"/api/v1/me/delete":                    "POST",
		"/api/v1/me/sessions/" + otherID:       "DELETE",
		"/api/v1/me/sessions/revoke-others":    "DELETE, POST",
		"/api/v1/me/devices/d1d1d1d1d1d1":      "DELETE",
		"/api/v1/me/sessions/" + otherID + "/": "",
		"/api/v1/me/devices/x/y":               "",
		"/api/v1/me/passwords":                 "",
	} {
		rec := f.call(http.MethodGet, path, "", with(cookie))
		if allow == "" {
			wantError(t, rec, http.StatusNotFound, api.CodeNotFound)
			continue
		}
		wantError(t, rec, http.StatusMethodNotAllowed, api.CodeMethodNotAllowed)
		if got := rec.Header().Get("Allow"); got != allow {
			t.Errorf("GET %s: Allow %q, want %q", path, got, allow)
		}
	}
	for _, path := range []string{"/api/v1/me/sessions", "/api/v1/me/devices"} {
		rec := f.call(http.MethodPost, path, `{}`, with(cookie))
		wantError(t, rec, http.StatusMethodNotAllowed, api.CodeMethodNotAllowed)
		if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
			t.Errorf("POST %s: Allow %q, want GET, HEAD", path, got)
		}
		// HEAD is served like GET.
		if rec := f.call(http.MethodHead, path, "", with(cookie)); rec.Code != http.StatusOK {
			t.Errorf("HEAD %s: status %d", path, rec.Code)
		}
	}

	// The auth endpoints accept 16 KiB, the others 64 KiB (03 §12.1): logout-everywhere is one of the first, the
	// account endpoints are not.
	big := strings.Repeat("x", authBodyLimit)
	wantError(t, f.call(http.MethodPost, "/api/v1/auth/logout-everywhere", `{"pad":"`+big+`"}`, with(cookie)),
		http.StatusRequestEntityTooLarge, api.CodePayloadTooLarge)
	wantError(t, f.call(http.MethodPost, "/api/v1/me/password", `{"newPassword":"`+big+`"}`, with(cookie)),
		http.StatusUnprocessableEntity, api.CodeValidationFailed)
	wantError(t, f.call(http.MethodPost, "/api/v1/me/delete", `{"password":"`+strings.Repeat("x", otherBodyLimit)+`"}`, with(cookie)),
		http.StatusRequestEntityTooLarge, api.CodePayloadTooLarge)
	f.wantSignedIn(cookie)
}

// TestSelfServiceCallerGone: a request the chain let in on a session that has gone since (the principal may be
// 30 s old) is 401, and changes nothing. The handlers' lists check their caller in their own read, the changes in
// auth.Service's Write.
func TestSelfServiceCallerGone(t *testing.T) {
	f := newAuthFixture(t)
	admin, cookie := f.setupAdmin("Alex")
	other := f.sessionCookie(f.login("Alex", testPassword, addrB), thirtyDays)
	otherID := string(f.sessionID(other))
	f.linkDevice(admin.ID, "d1d1d1d1d1d1")
	// The session is in auth's cache; its row goes behind the service's back.
	sid := f.sessionID(cookie)
	f.write(func(q *store.Q) error {
		_, err := q.DeleteSession(store.UserID(admin.ID), sid)
		return err
	})
	f.conns.take()
	for _, rt := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/me/sessions", ``},
		{http.MethodGet, "/api/v1/me/devices", ``},
		{http.MethodDelete, "/api/v1/me/sessions/" + otherID, ``},
		{http.MethodPost, "/api/v1/me/sessions/revoke-others", `{}`},
		{http.MethodDelete, "/api/v1/me/devices/d1d1d1d1d1d1", ``},
		{http.MethodPost, "/api/v1/me/password", `{"currentPassword":"` + testPassword + `","newPassword":"` + newPassword + `"}`},
		{http.MethodPost, "/api/v1/me/delete", `{"password":"` + testPassword + `"}`},
		{http.MethodPost, "/api/v1/auth/logout-everywhere", `{}`},
	} {
		rec := f.call(rt.method, rt.path, rt.body, with(cookie))
		wantError(t, rec, http.StatusUnauthorized, api.CodeUnauthenticated)
		if got := rec.Header().Values("Set-Cookie"); len(got) != 0 {
			t.Errorf("%s %s by a caller without a session set a cookie: %q", rt.method, rt.path, got)
		}
	}
	f.wantSignedIn(other)
	if got := f.deviceList(other); len(got) != 1 || len(f.conns.take()) != 0 || f.tableRows("users") != 1 {
		t.Errorf("a caller without a session changed something: devices %+v", got)
	}
}
