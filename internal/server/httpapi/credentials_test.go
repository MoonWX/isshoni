package httpapi

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/auth"
)

// The credential step of M2 02 §6.4 (README WA47) through the real chain with a real auth.Service: which credential
// each access level takes, the routes' access levels, the admin routes refusing every bearer, and the bearer path
// skipping the cross-origin check.

// outcomeOf sums up an answer of the matrix's echo handler: "200 <method> <user>" ("200 none" without a principal),
// or "<status> <code>" plus the credential of a forbidden's params. It also checks WWW-Authenticate: present exactly
// on 401 invalid_token.
func outcomeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code == http.StatusOK {
		var got struct {
			OK     bool   `json:"ok"`
			Method string `json:"method"`
			User   string `json:"user"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("body %q: %v", rec.Body, err)
		}
		if !got.OK {
			return "200 none"
		}
		return "200 " + got.Method + " " + got.User
	}
	e := decodeEnvelope(t, rec.Body.Bytes())
	out := fmt.Sprintf("%d %s", rec.Code, e.Code)
	if c, ok := e.Params[api.ParamCredential]; ok {
		out += fmt.Sprint(" ", c)
	}
	if www := rec.Header().Get("WWW-Authenticate"); (e.Code == api.CodeInvalidToken) != (www != "") {
		t.Errorf("%s with WWW-Authenticate %q", out, www)
	}
	return out
}

// echoPrincipal answers 200 with the principal the chain handed over, and whether a Cookie header reached it.
var echoPrincipal = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	method := ""
	switch p.Method {
	case auth.MethodSession:
		method = "session"
	case auth.MethodBearer:
		method = "bearer"
	}
	_, cookie := r.Header["Cookie"]
	WriteJSON(w, http.StatusOK, map[string]any{"ok": ok, "method": method, "user": p.Username, "cookie": cookie})
})

// TestCredentialMatrix is M2 02 §16.1's credential matrix: each of Public, User, Admin, UserSession and UserDevice ×
// {none, cookie, bearer, bad bearer, cookie + bearer}, each credential of a member and of an admin. An admin's valid
// bearer on an Admin route is 403 forbidden {credential: session}, a bad bearer 401 invalid_token, the admin's cookie
// as in M1.
func TestCredentialMatrix(t *testing.T) {
	f := newAuthFixture(t)
	_, adminCookie := f.setupAdmin("Alex")
	_, memberCookie := f.newMember(adminCookie, "Sam", addrB)
	adminApp := f.linkApp(adminCookie, "Alex-PC", addrA)
	memberApp := f.linkApp(memberCookie, "Sam-PC", addrB)
	levels := []Access{Public, User, Admin, UserSession, UserDevice}
	for _, a := range levels {
		f.api.Handle("POST /api/v1/test/"+a.String(), a, echoPrincipal)
	}

	const (
		none      = "200 none"
		unauth    = "401 unauthenticated"
		invalid   = "401 invalid_token"
		forbidden = "403 forbidden"
		session   = "403 forbidden session"
		device    = "403 forbidden device"
	)
	for _, row := range []struct {
		name string
		opts []reqOpt
		want [5]string // Public, User, Admin, UserSession, UserDevice
	}{
		{"none", nil, [5]string{none, unauth, unauth, unauth, unauth}},
		{"a member's cookie", []reqOpt{with(memberCookie)},
			[5]string{none, "200 session Sam", forbidden, "200 session Sam", device}},
		{"an admin's cookie", []reqOpt{with(adminCookie)},
			[5]string{none, "200 session Alex", "200 session Alex", "200 session Alex", device}},
		{"a dead cookie", []reqOpt{cookieValue(sessionCookieName, strings.Repeat("A", 43))},
			[5]string{none, unauth, unauth, unauth, unauth}},
		{"a member's bearer", []reqOpt{withBearer(memberApp.AccessToken)},
			[5]string{none, "200 bearer Sam", session, session, "200 bearer Sam"}},
		{"an admin's bearer", []reqOpt{withBearer(adminApp.AccessToken)},
			[5]string{none, "200 bearer Alex", session, session, "200 bearer Alex"}},
		{"a bad bearer", []reqOpt{withBearer(unknownAccessToken)},
			[5]string{none, invalid, invalid, invalid, invalid}},
		{"a refresh token as bearer", []reqOpt{withBearer(adminApp.RefreshToken)},
			[5]string{none, invalid, invalid, invalid, invalid}},
		{"another scheme", []reqOpt{header("Authorization", "Basic dTpw")},
			[5]string{none, invalid, invalid, invalid, invalid}},
		{"two Authorization headers", []reqOpt{withBearer(adminApp.AccessToken), func(r *http.Request) {
			r.Header.Add("Authorization", "Bearer "+adminApp.AccessToken)
		}}, [5]string{none, invalid, invalid, invalid, invalid}},
		{"an admin's cookie and bearer", []reqOpt{with(adminCookie), withBearer(adminApp.AccessToken)},
			[5]string{none, "200 bearer Alex", session, session, "200 bearer Alex"}},
		{"an admin's cookie and a member's bearer", []reqOpt{with(adminCookie), withBearer(memberApp.AccessToken)},
			[5]string{none, "200 bearer Sam", session, session, "200 bearer Sam"}},
		{"an admin's cookie and a bad bearer", []reqOpt{with(adminCookie), withBearer(unknownAccessToken)},
			[5]string{none, invalid, invalid, invalid, invalid}},
	} {
		for i, a := range levels {
			rec := f.call(http.MethodPost, "/api/v1/test/"+a.String(), `{}`, row.opts...)
			if got := outcomeOf(t, rec); got != row.want[i] {
				t.Errorf("%s on a %s route: %s, want %s", row.name, a, got, row.want[i])
			}
			// A bearer request never reads a cookie: neither the chain nor the handler sees one, nor is one set.
			if strings.HasPrefix(row.want[i], "200 bearer") && strings.Contains(rec.Body.String(), `"cookie":true`) {
				t.Errorf("%s on a %s route: the handler saw the Cookie header", row.name, a)
			}
			if strings.Contains(row.name, "bearer") && a != Public && len(rec.Header().Values("Set-Cookie")) != 0 {
				t.Errorf("%s on a %s route: Set-Cookie %q", row.name, a, rec.Header().Values("Set-Cookie"))
			}
		}
	}

	// The bearer path skips the cross-origin check (no ambient credential is used), and keeps the Content-Type rule;
	// the cookie path keeps both.
	crossSite := []reqOpt{header("Sec-Fetch-Site", "cross-site"), header("Origin", "https://evil.example")}
	for _, a := range []Access{User, UserDevice} {
		path := "/api/v1/test/" + a.String()
		if got := outcomeOf(t, f.call(http.MethodPost, path, `{}`, append(crossSite, withBearer(adminApp.AccessToken))...)); got != "200 bearer Alex" {
			t.Errorf("a cross-site bearer request on a %s route: %s", a, got)
		}
		wantError(t, f.call(http.MethodPost, path, `{}`, append(crossSite, withBearer(adminApp.AccessToken),
			header("Content-Type", "text/plain"))...), http.StatusUnsupportedMediaType, api.CodeUnsupportedMediaType)
		wantError(t, f.call(http.MethodPost, path, `{}`, append(crossSite, with(adminCookie))...), http.StatusForbidden,
			api.CodeCSRFFailed)
	}
	// A Public route keeps its CSRF check whatever the Authorization header says.
	wantError(t, f.call(http.MethodPost, "/api/v1/test/public", `{}`, append(crossSite, withBearer(adminApp.AccessToken))...),
		http.StatusForbidden, api.CodeCSRFFailed)
}

// wantRouteAccess is §6.4's access of every route the API registers itself (03 §12.3, M2 02 §7.1).
var wantRouteAccess = map[string]Access{
	"GET /api/v1/info":                             Public, // #1
	"POST /api/v1/auth/login":                      Public, // #2
	"POST /api/v1/auth/logout":                     Public, // #3
	"POST /api/v1/auth/logout-everywhere":          User,   // #4
	"POST /api/v1/auth/register":                   Public, // #5
	"POST /api/v1/auth/invite/check":               Public, // #6
	"POST /api/v1/auth/setup/check":                Public, // #7
	"POST /api/v1/auth/setup/complete":             Public, // #8
	"POST /api/v1/auth/reset/check":                Public, // #9
	"POST /api/v1/auth/reset/complete":             Public, // #10
	"GET /api/v1/me":                               User,   // #11
	"POST /api/v1/me/password":                     UserSession,
	"POST /api/v1/me/delete":                       UserSession,
	"GET /api/v1/me/sessions":                      User,
	"DELETE /api/v1/me/sessions/{id}":              User,
	"POST /api/v1/me/sessions/revoke-others":       UserSession,
	"GET /api/v1/me/devices":                       User,
	"DELETE /api/v1/me/devices/{id}":               User,
	"GET /api/v1/rooms":                            User,
	"GET /api/v1/invites":                          User, // #21–#23: the desktop invite dialog
	"POST /api/v1/invites":                         User,
	"DELETE /api/v1/invites/{id}":                  User,
	"POST /api/v1/push/subscriptions":              UserSession,
	"POST /api/v1/push/unsubscribe":                User,
	"POST /api/v1/push/test":                       UserSession,
	"GET /api/v1/admin/users":                      Admin, // #29–#42
	"PATCH /api/v1/admin/users/{id}":               Admin,
	"DELETE /api/v1/admin/users/{id}":              Admin,
	"POST /api/v1/admin/users/{id}/password-reset": Admin,
	"POST /api/v1/admin/users/{id}/sign-out":       Admin,
	"GET /api/v1/admin/approvals":                  Admin,
	"POST /api/v1/admin/approvals/{id}/approve":    Admin,
	"POST /api/v1/admin/approvals/{id}/reject":     Admin,
	"POST /api/v1/admin/rooms":                     Admin,
	"PATCH /api/v1/admin/rooms/{id}":               Admin,
	"DELETE /api/v1/admin/rooms/{id}":              Admin,
	"GET /api/v1/admin/settings":                   Admin,
	"PATCH /api/v1/admin/settings":                 Admin,
	"GET /api/v1/admin/audit":                      Admin,
	"POST /api/v1/device/code":                     Public, // #44
	"POST /api/v1/device/token":                    Public, // #45
	"POST /api/v1/device/lookup":                   UserSession,
	"POST /api/v1/device/decide":                   UserSession,
	"POST /api/v1/device/password":                 Public, // #48
	"POST /api/v1/device/revoke":                   UserDevice,
	"GET /api/v1/push/preferences":                 User,
	"PUT /api/v1/push/preferences":                 User,
	"PATCH /api/v1/me/devices/{id}":                User, // #52
}

// TestRouteAccess: the API registers exactly §6.4's routes with §6.4's access levels; every /api/v1/admin/* route
// is Admin (#29–#42).
func TestRouteAccess(t *testing.T) {
	f := newAPIFixture(t, nil)
	got := map[string]Access{}
	for _, r := range f.api.registered {
		if _, dup := got[r.pattern]; dup {
			t.Errorf("%s is registered twice", r.pattern)
		}
		got[r.pattern] = r.access
		if strings.Contains(r.pattern, " /api/v1/admin/") && r.access != Admin {
			t.Errorf("%s is %s, want admin (M2 02 §6.4)", r.pattern, r.access)
		}
	}
	for _, p := range slices.Sorted(maps.Keys(wantRouteAccess)) {
		if a, ok := got[p]; !ok || a != wantRouteAccess[p] {
			t.Errorf("%s: registered %v (%v), want %s", p, a, ok, wantRouteAccess[p])
		}
	}
	for _, p := range slices.Sorted(maps.Keys(got)) {
		if _, ok := wantRouteAccess[p]; !ok {
			t.Errorf("%s (%s) is not a route of 03 §12.3 or M2 02 §7.1", p, got[p])
		}
	}
}

// TestAdminRoutesRefuseBearer: every /api/v1/admin/* route, 04's added through Handle included, answers an admin's
// valid bearer 403 forbidden {credential: session} and a bad one 401 invalid_token before its handler runs, while
// the admin's cookie gets through; the invite routes (#21–#23) accept the admin's bearer.
func TestAdminRoutesRefuseBearer(t *testing.T) {
	f := newAuthFixture(t)
	_, adminCookie := f.setupAdmin("Alex")
	_, memberCookie := f.newMember(adminCookie, "Sam", addrB)
	adminApp := f.linkApp(adminCookie, "Alex-PC", addrA)
	memberApp := f.linkApp(memberCookie, "Sam-PC", addrB)
	// 04's routes, registered as the wiring does (internal/server/wire.go).
	ran := 0
	ops := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ran++
		w.WriteHeader(http.StatusNoContent)
	})
	for _, p := range []string{"GET /api/v1/admin/dashboard", "GET /api/v1/admin/doctor", "POST /api/v1/admin/doctor",
		"GET /api/v1/admin/bandwidth"} {
		f.api.Handle(p, Admin, ops)
	}

	n := 0
	for _, r := range f.api.registered {
		method, path, _ := strings.Cut(r.pattern, " ")
		if !strings.HasPrefix(path, "/api/v1/admin/") {
			continue
		}
		n++
		path = strings.ReplaceAll(path, "{id}", "x1x1x1x1x1x1")
		before := f.audit()
		wantForbiddenFor(t, f.call(method, path, `{}`, withBearer(adminApp.AccessToken)), api.CredentialKindSession)
		wantForbiddenFor(t, f.call(method, path, `{}`, with(adminCookie), withBearer(adminApp.AccessToken)),
			api.CredentialKindSession)
		wantForbiddenFor(t, f.call(method, path, `{}`, withBearer(memberApp.AccessToken)), api.CredentialKindSession)
		wantInvalidToken(t, f.call(method, path, `{}`, withBearer(unknownAccessToken)))
		wantError(t, f.call(method, path, `{}`), http.StatusUnauthorized, api.CodeUnauthenticated)
		if e := wantError(t, f.call(method, path, `{}`, with(memberCookie)), http.StatusForbidden, api.CodeForbidden); len(e.Params) != 0 {
			t.Errorf("%s with a member's cookie: params %v, want none (M1's answer)", r.pattern, e.Params)
		}
		if after := f.audit(); len(after) != len(before) {
			t.Errorf("%s: refused requests wrote %d audit rows", r.pattern, len(after)-len(before))
		}
		if method == http.MethodGet {
			if rec := f.call(method, path, "", with(adminCookie)); rec.Code/100 != 2 {
				t.Errorf("GET %s with the admin's cookie: status %d (body %q)", path, rec.Code, rec.Body)
			}
		}
	}
	if n != 14+4 || ran != 3 { // #29–#42 and 04's four; the cookie reached 04's three GETs
		t.Errorf("%d admin routes, 04's handlers ran %d times", n, ran)
	}

	// #21–#23 take the admin's bearer: list, create, revoke.
	decodeBody[api.InvitesResponse](t, f.call(http.MethodGet, "/api/v1/invites", "", withBearer(adminApp.AccessToken)),
		http.StatusOK)
	inv := decodeBody[api.CreateInviteResponse](t, f.call(http.MethodPost, "/api/v1/invites", `{}`,
		withBearer(adminApp.AccessToken)), http.StatusCreated)
	wantNoContent(t, f.call(http.MethodDelete, "/api/v1/invites/"+inv.Invite.ID, "", withBearer(adminApp.AccessToken)))
	// A member's bearer gets what the member's cookie gets (membersCanInvite is off).
	for _, rq := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/invites", ""},
		{http.MethodPost, "/api/v1/invites", `{}`},
	} {
		byCookie := f.call(rq.method, rq.path, rq.body, with(memberCookie))
		byBearer := f.call(rq.method, rq.path, rq.body, withBearer(memberApp.AccessToken))
		if byCookie.Code != byBearer.Code || byCookie.Body.String() != byBearer.Body.String() {
			t.Errorf("%s %s: the member's cookie gets %d %q, its bearer %d %q", rq.method, rq.path, byCookie.Code,
				byCookie.Body, byBearer.Code, byBearer.Body)
		}
	}
	// The invite routes take a cross-site bearer request (no CSRF headers needed), not a cross-site cookie one.
	crossSite := []reqOpt{header("Sec-Fetch-Site", "cross-site"), header("Origin", "https://evil.example")}
	decodeBody[api.CreateInviteResponse](t, f.call(http.MethodPost, "/api/v1/invites", `{}`,
		append(crossSite, withBearer(adminApp.AccessToken))...), http.StatusCreated)
	wantError(t, f.call(http.MethodPost, "/api/v1/invites", `{}`, append(crossSite, with(adminCookie))...),
		http.StatusForbidden, api.CodeCSRFFailed)
}

// TestSessionOnlyRoutesRefuseBearer: the UserSession routes of M2 02 §6.4 (#12, #13, #16, #24, #28, #46, #47)
// answer a valid bearer 403 forbidden {credential: session} before anything happens, and the User routes a device
// calls answer it as they answer the cookie.
func TestSessionOnlyRoutesRefuseBearer(t *testing.T) {
	f := newAuthFixture(t)
	_, cookie := f.setupAdmin("Alex")
	app := f.linkApp(cookie, "PC", addrA)
	other := f.sessionCookie(f.login("Alex", testPassword, addrB), thirtyDays)
	f.conns.take()
	for _, rq := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/me/password", `{"currentPassword":"` + testPassword + `","newPassword":"a new passphrase"}`},
		{http.MethodPost, "/api/v1/me/delete", `{"password":"` + testPassword + `"}`},
		{http.MethodPost, "/api/v1/me/sessions/revoke-others", `{}`},
		{http.MethodPost, "/api/v1/push/subscriptions", `{}`},
		{http.MethodPost, "/api/v1/push/test", `{}`},
		{http.MethodPost, "/api/v1/device/lookup", `{"userCode":"BCDF-GHJK"}`},
		{http.MethodPost, "/api/v1/device/decide", `{"userCode":"BCDF-GHJK","approve":true}`},
	} {
		wantForbiddenFor(t, f.call(rq.method, rq.path, rq.body, withBearer(app.AccessToken)), api.CredentialKindSession)
		wantInvalidToken(t, f.call(rq.method, rq.path, rq.body, withBearer(unknownAccessToken)))
	}
	f.wantSignedIn(cookie)
	f.wantSignedIn(other)
	if got := f.conns.take(); len(got) != 0 {
		t.Errorf("refused requests closed %+v", got)
	}
	for _, path := range []string{"/api/v1/me", "/api/v1/me/sessions", "/api/v1/me/devices", "/api/v1/rooms",
		"/api/v1/invites", "/api/v1/push/preferences"} {
		if rec := f.call(http.MethodGet, path, "", withBearer(app.AccessToken)); rec.Code != http.StatusOK {
			t.Errorf("GET %s with a bearer: status %d (body %q)", path, rec.Code, rec.Body)
		}
	}
	decodeBody[api.PushPreferences](t, f.call(http.MethodPut, "/api/v1/push/preferences",
		`{"shareStarted":"off","adminAlerts":false}`, withBearer(app.AccessToken)), http.StatusOK)
	wantNoContent(t, f.call(http.MethodPost, "/api/v1/push/unsubscribe", `{"endpoint":"https://push.example/x"}`,
		withBearer(app.AccessToken)))
	wantNoContent(t, f.call(http.MethodDelete, "/api/v1/me/sessions/"+string(f.sessionID(other)), "",
		withBearer(app.AccessToken)))
	f.wantUnauthenticated(other)
	f.wantSignedIn(cookie)
}
