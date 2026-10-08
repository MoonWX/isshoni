package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/auth"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// Helpers of the invite, registration, approval and settings tests (README S42), on top of authFixture.

// clientIP is the i-th address of a documentation range: one client per sign-up, so that a test stays clear of the
// auth-ip and register-ip buckets unless it is about them.
func clientIP(i int) string { return fmt.Sprintf("192.0.2.%d", 1+i%90) }

// take returns the alerts recorded so far and clears them.
func (a *recAlerts) take() []auth.AdminAlert {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.alerts
	a.alerts = nil
	return out
}

// patchSettings sends PATCH /api/v1/admin/settings with a raw JSON body.
func (f *authFixture) patchSettings(c *http.Cookie, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.call(http.MethodPatch, "/api/v1/admin/settings", body, with(c))
}

// setMode changes the registration mode over REST and drops the hooks and the alert of that change.
func (f *authFixture) setMode(c *http.Cookie, mode string) {
	f.t.Helper()
	rec := f.patchSettings(c, `{"registrationMode":"`+mode+`"}`)
	if got := decodeBody[api.SettingsResponse](f.t, rec, http.StatusOK); string(got.Settings.RegistrationMode) != mode {
		f.t.Fatalf("registrationMode = %q after the patch, want %q", got.Settings.RegistrationMode, mode)
	}
	f.signal.takeNotes()
	f.alerts.take()
}

// createInvite creates an invite over REST and returns it with the token of its link.
func (f *authFixture) createInvite(c *http.Cookie, body string) (api.Invite, string) {
	f.t.Helper()
	rec := f.call(http.MethodPost, "/api/v1/invites", body, with(c))
	res := decodeBody[api.CreateInviteResponse](f.t, rec, http.StatusCreated)
	token, ok := strings.CutPrefix(res.URL, testOrigin+"/invite#")
	if !ok || len(token) != 32 || strings.ContainsAny(token, "/?#&=") {
		f.t.Fatalf("invite url %q: want %s/invite#<32-character token>", res.URL, testOrigin)
	}
	return res.Invite, token
}

// register sends POST /api/v1/auth/register with testPassword from address ip.
func (f *authFixture) register(inviteToken, username, ip string, opts ...reqOpt) *httptest.ResponseRecorder {
	f.t.Helper()
	opts = append([]reqOpt{from(ip)}, opts...)
	return f.call(http.MethodPost, "/api/v1/auth/register", jsonBody(f.t, api.RegisterRequest{
		InviteToken: inviteToken, Username: username, Password: testPassword}), opts...)
}

// join registers username with an invite and returns the new user and its session cookie.
func (f *authFixture) join(inviteToken, username, ip string) (api.User, *http.Cookie) {
	f.t.Helper()
	rec := f.register(inviteToken, username, ip)
	res := decodeBody[api.RegisterResponse](f.t, rec, http.StatusCreated)
	if res.Status != api.UserStatusActive || res.User == nil || res.User.Username != username {
		f.t.Fatalf("register %s = %+v", username, res)
	}
	return *res.User, f.sessionCookie(rec, thirtyDays)
}

// checkInvite sends POST /api/v1/auth/invite/check from address ip.
func (f *authFixture) checkInvite(token, ip string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.call(http.MethodPost, "/api/v1/auth/invite/check", jsonBody(f.t, api.TokenRequest{Token: token}), from(ip))
}

// invites lists the invites over REST.
func (f *authFixture) invites(c *http.Cookie, query string) []api.Invite {
	f.t.Helper()
	rec := f.call(http.MethodGet, "/api/v1/invites"+query, "", with(c))
	return decodeBody[api.InvitesResponse](f.t, rec, http.StatusOK).Invites
}

// pending lists the approval queue over REST.
func (f *authFixture) pending(c *http.Cookie) []api.PendingUser {
	f.t.Helper()
	rec := f.call(http.MethodGet, "/api/v1/admin/approvals", "", with(c))
	return decodeBody[api.ApprovalsResponse](f.t, rec, http.StatusOK).Pending
}

// auditOf returns the audit rows of one action, oldest first.
func (f *authFixture) auditOf(action string) []store.AuditEntry {
	f.t.Helper()
	var out []store.AuditEntry
	for _, row := range f.audit() {
		if row.Action == action {
			out = append(out, row)
		}
	}
	return out
}

// counter returns a function that runs one-value queries on a second connection to the database file.
func (f *authFixture) counter() func(query string, args ...any) int {
	f.t.Helper()
	raw := f.rawDB()
	return func(query string, args ...any) int {
		f.t.Helper()
		var n int
		if err := raw.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
			f.t.Fatalf("%s: %v", query, err)
		}
		return n
	}
}

// jsonKeys returns the sorted keys of a JSON object, for shape checks against 03 §12.4.
func jsonKeys(t interface{ Fatalf(string, ...any) }, raw []byte, path ...string) []string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("body %q: %v", raw, err)
	}
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("body %q: no object at %q", raw, p)
		}
		v = m[p]
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("body %q: no object at %v", raw, path)
	}
	return keysOf(m)
}
