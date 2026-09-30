package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/auth"
)

// noContent is a handler that answers 204.
var noContent = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

// csrfCase is one request of the chain's CSRF and Content-Type matrix.
type csrfCase struct {
	method, site, origin string
	contentType          []string // nil = no header
}

// jsonTypes are Content-Type header lists and whether 03 §7.5 accepts them: exactly one header, application/json,
// a charset only if utf-8 (any case), other parameters ignored.
var jsonTypes = []struct {
	values []string
	ok     bool
}{
	{nil, false},
	{[]string{""}, false},
	{[]string{"text/plain"}, false},
	{[]string{"text/plain;charset=UTF-8"}, false},
	{[]string{"application/x-www-form-urlencoded"}, false},
	{[]string{"multipart/form-data; boundary=x"}, false},
	{[]string{"application/json"}, true},
	{[]string{"application/json; charset=utf-8"}, true},
	{[]string{"application/json;charset=UTF-8"}, true},
	{[]string{`application/json; charset="utf-8"`}, true},
	{[]string{"Application/JSON"}, true},
	{[]string{"application/json; foo=bar"}, true},
	{[]string{"application/json; charset=iso-8859-1"}, false},
	{[]string{"application/json; charset=utf-16"}, false},
	{[]string{"application/json", "application/json"}, false},
	{[]string{"application/json, text/plain"}, false},
	{[]string{"application/jsonx"}, false},
	{[]string{"application/problem+json"}, false},
	{[]string{"application/json;"}, true},
}

// want is the rule of 03 §7.5 written out independently of the implementation: status 204 when the request reaches
// the handler, else 403 csrf_failed or 415 unsupported_media_type. The fake auth trusts testOrigin.
func (c csrfCase) want(jsonOK bool) (int, string) {
	switch c.method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return http.StatusNoContent, ""
	}
	trusted := c.origin == testOrigin
	var crossOriginOK bool
	switch c.site {
	case "same-origin", "none":
		crossOriginOK = true
	case "":
		host := strings.TrimPrefix(strings.TrimPrefix(c.origin, "https://"), "http://")
		crossOriginOK = c.origin == "" || host == testHost || trusted
	default:
		crossOriginOK = trusted
	}
	if !crossOriginOK {
		return http.StatusForbidden, api.CodeCSRFFailed
	}
	if !jsonOK {
		return http.StatusUnsupportedMediaType, api.CodeUnsupportedMediaType
	}
	return http.StatusNoContent, ""
}

// TestAPICSRFMatrix runs 03 §15's CSRF matrix through the whole stack (04's router, the /api/v1 chain, auth's guard):
// Sec-Fetch-Site × Origin × method × Content-Type, with an empty body. Only the requests the rule admits reach the
// handler, and every answer carries no-store.
func TestAPICSRFMatrix(t *testing.T) {
	f := newAPIFixture(t, nil)
	reached := 0
	f.api.Handle("/api/v1/test/any", Public, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++
		w.WriteHeader(http.StatusNoContent)
	}))

	methods := []string{"GET", "HEAD", "OPTIONS", "POST", "PUT", "PATCH", "DELETE"}
	sites := []string{"", "same-origin", "none", "same-site", "cross-site"}
	origins := []string{
		"",                     // absent
		testOrigin,             // matches Host, trusted
		"http://" + testHost,   // matches Host (the check ignores the scheme), not trusted
		"https://evil.example", // mismatch
		"null",
	}
	n := 0
	for _, m := range methods {
		for _, site := range sites {
			for _, origin := range origins {
				for _, ct := range jsonTypes {
					c := csrfCase{m, site, origin, ct.values}
					wantStatus, wantCode := c.want(ct.ok)
					var header []string
					if site != "" {
						header = append(header, "Sec-Fetch-Site", site)
					}
					if origin != "" {
						header = append(header, "Origin", origin)
					}
					for _, v := range ct.values {
						header = append(header, "Content-Type", v)
					}
					before := reached
					rec := f.do(m, "/api/v1/test/any", nil, header...)
					n++
					name := fmt.Sprintf("%s site=%q origin=%q content-type=%q", m, site, origin, ct.values)
					if wantCode == "" {
						if rec.Code != wantStatus || reached != before+1 {
							t.Fatalf("%s: status %d, handler reached %v; want %d and reached", name, rec.Code,
								reached > before, wantStatus)
						}
						if cc := rec.Header().Get("Cache-Control"); cc != cacheNoStore {
							t.Fatalf("%s: Cache-Control %q", name, cc)
						}
						continue
					}
					if reached != before {
						t.Fatalf("%s: the handler ran", name)
					}
					if rec.Code != wantStatus {
						t.Fatalf("%s: status %d, want %d (body %q)", name, rec.Code, wantStatus, rec.Body)
					}
					wantError(t, rec, wantStatus, wantCode)
				}
			}
		}
	}
	t.Logf("%d requests", n)
}

// TestAPIContentTypeWithBody shows that the Content-Type rule applies to unsafe requests with a body too, and that an
// accepted JSON body reaches the handler intact.
func TestAPIContentTypeWithBody(t *testing.T) {
	f := newAPIFixture(t, nil)
	var got map[string]any
	f.api.Handle("/api/v1/test/decode", Public, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = nil
		if err := DecodeJSON(w, r, &got, 0); err != nil {
			WriteError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		for _, ct := range jsonTypes {
			header := []string{"Sec-Fetch-Site", "same-origin"}
			for _, v := range ct.values {
				header = append(header, "Content-Type", v)
			}
			rec := f.do(m, "/api/v1/test/decode", strings.NewReader(`{"a":1}`), header...)
			name := fmt.Sprintf("%s content-type=%q", m, ct.values)
			if !ct.ok {
				wantError(t, rec, http.StatusUnsupportedMediaType, api.CodeUnsupportedMediaType)
				continue
			}
			if rec.Code != http.StatusNoContent || got["a"] != float64(1) {
				t.Fatalf("%s: status %d, decoded %v (body %q)", name, rec.Code, got, rec.Body)
			}
		}
	}
}

// TestAPICSRFBeforeAuthenticate: a cross-site POST that carries a valid session cookie is refused before the chain
// looks at the cookie (03 §15: "a cross-site POST with a cookie → 403 csrf_failed").
func TestAPICSRFBeforeAuthenticate(t *testing.T) {
	f := newAPIFixture(t, nil)
	called := false
	f.api.Handle("POST /api/v1/test/user", User, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, h := range [][]string{
		{"Sec-Fetch-Site", "cross-site", "Origin", "https://evil.example", "Content-Type", "application/json"},
		{"Origin", "https://evil.example", "Content-Type", "application/json"}, // an older browser
		{"Sec-Fetch-Site", "same-site", "Origin", "https://sub.watch.example.com", "Content-Type", "application/json"},
	} {
		rec := f.do(http.MethodPost, "/api/v1/test/user", nil, withCookie("user", h...)...)
		wantError(t, rec, http.StatusForbidden, api.CodeCSRFFailed)
		if calls := f.auth.take(); !slices.Equal(calls, []string{"csrf"}) || called {
			t.Fatalf("calls %v, handler called %v; want only the CSRF check", calls, called)
		}
		if sc := rec.Header().Values("Set-Cookie"); len(sc) != 0 {
			t.Fatalf("a refused request rotated the cookie: %q", sc)
		}
	}
	// A POST without JSON Content-Type → 415, also before authentication.
	rec := f.do(http.MethodPost, "/api/v1/test/user", nil, withCookie("user", "Sec-Fetch-Site", "same-origin")...)
	wantError(t, rec, http.StatusUnsupportedMediaType, api.CodeUnsupportedMediaType)
	if calls := f.auth.take(); !slices.Equal(calls, []string{"csrf"}) {
		t.Fatalf("calls %v; want only the CSRF check", calls)
	}
}

// TestAPIChainOrder pins the /api/v1 chain of 03 §12.1 as far as it is observable: body limit → no-store → route
// lookup → the M1 Authorization rejection → CSRF → authenticate → rotate → access → handler.
func TestAPIChainOrder(t *testing.T) {
	f := newAPIFixture(t, nil)
	f.api.Handle("POST /api/v1/test/user", User, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.auth.record("handler")
		w.WriteHeader(http.StatusNoContent)
	}))
	f.api.Handle("POST /api/v1/test/admin", Admin, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.auth.record("handler")
		w.WriteHeader(http.StatusNoContent)
	}))
	f.api.Handle("POST /api/v1/auth/test", Public, noContent)

	t.Run("csrf, authenticate, rotate, handler", func(t *testing.T) {
		rec := f.do(http.MethodPost, "/api/v1/test/user", nil, withCookie("user", sameOriginJSON...)...)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status %d (body %q)", rec.Code, rec.Body)
		}
		if calls := f.auth.take(); !slices.Equal(calls, []string{"csrf", "authenticate", "rotate", "handler"}) {
			t.Fatalf("calls %v", calls)
		}
	})
	t.Run("rotate before the access check", func(t *testing.T) {
		rec := f.do(http.MethodPost, "/api/v1/test/admin", nil, withCookie("user", sameOriginJSON...)...)
		wantError(t, rec, http.StatusForbidden, api.CodeForbidden)
		if calls := f.auth.take(); !slices.Equal(calls, []string{"csrf", "authenticate", "rotate"}) {
			t.Fatalf("calls %v", calls)
		}
		if sc := rec.Header().Get("Set-Cookie"); !strings.HasPrefix(sc, "session=rotated-") {
			t.Fatalf("the 403 lost the rotated cookie: Set-Cookie %q", sc)
		}
	})
	t.Run("Authorization before CSRF", func(t *testing.T) {
		rec := f.do(http.MethodPost, "/api/v1/test/user", nil,
			withCookie("user", "Sec-Fetch-Site", "cross-site", "Authorization", "Bearer isa_x")...)
		wantError(t, rec, http.StatusUnauthorized, api.CodeUnauthenticated)
		if calls := f.auth.take(); len(calls) != 0 {
			t.Fatalf("calls %v; want none", calls)
		}
	})
	t.Run("body limit before Authorization and CSRF", func(t *testing.T) {
		body := strings.NewReader(strings.Repeat("x", authBodyLimit+1))
		rec := f.do(http.MethodPost, "/api/v1/auth/test", body, "Sec-Fetch-Site", "cross-site",
			"Authorization", "Bearer isa_x")
		wantError(t, rec, http.StatusRequestEntityTooLarge, api.CodePayloadTooLarge)
		if calls := f.auth.take(); len(calls) != 0 {
			t.Fatalf("calls %v; want none", calls)
		}
	})
	t.Run("route lookup before Authorization and CSRF", func(t *testing.T) {
		rec := f.do(http.MethodPost, "/api/v1/test/nothing-here", nil, "Sec-Fetch-Site", "cross-site",
			"Authorization", "Bearer isa_x")
		wantError(t, rec, http.StatusNotFound, api.CodeNotFound)
		rec = f.do(http.MethodDelete, "/api/v1/test/user", nil, "Sec-Fetch-Site", "cross-site",
			"Authorization", "Bearer isa_x")
		wantError(t, rec, http.StatusMethodNotAllowed, api.CodeMethodNotAllowed)
		if calls := f.auth.take(); len(calls) != 0 {
			t.Fatalf("calls %v; want none", calls)
		}
	})
}

// TestAPIAuthorizationHeader: M1 has no bearer tokens, so any Authorization header gets 401 unauthenticated on every
// route, whatever its Access, before the CSRF check, the cookie and the rotation (03 §7.5; 03 §15 "an Authorization
// header in M1 → 401").
func TestAPIAuthorizationHeader(t *testing.T) {
	f := newAPIFixture(t, nil)
	reached := 0
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++
		w.WriteHeader(http.StatusNoContent)
	})
	f.api.Handle("POST /api/v1/test/public", Public, h)
	f.api.Handle("GET /api/v1/test/user", User, h)
	f.api.Handle("POST /api/v1/test/admin", Admin, h)

	requests := []struct {
		name, method, path string
		header             []string
		status             int // without the Authorization header
	}{
		{"GET /info", http.MethodGet, "/api/v1/info", nil, http.StatusOK},
		{"Public POST, same-origin JSON", http.MethodPost, "/api/v1/test/public", sameOriginJSON, http.StatusNoContent},
		{"Public POST, cross-site", http.MethodPost, "/api/v1/test/public",
			[]string{"Sec-Fetch-Site", "cross-site", "Origin", "https://evil.example"}, http.StatusForbidden},
		{"User GET with a valid cookie", http.MethodGet, "/api/v1/test/user", withCookie("user"), http.StatusNoContent},
		{"Admin POST with a valid cookie", http.MethodPost, "/api/v1/test/admin", withCookie("admin", sameOriginJSON...),
			http.StatusNoContent},
	}
	// Without the header the requests get their usual answers: the header alone makes the 401.
	for _, rq := range requests {
		if rec := f.do(rq.method, rq.path, nil, rq.header...); rec.Code != rq.status {
			t.Fatalf("%s without Authorization: status %d, want %d (body %q)", rq.name, rec.Code, rq.status, rec.Body)
		}
	}
	reached = 0
	f.auth.take()

	for _, authz := range []string{"Bearer isa_x", "Basic dTpw", "Bearer", ""} {
		for _, rq := range requests {
			name := fmt.Sprintf("%s, Authorization %q", rq.name, authz)
			rec := f.do(rq.method, rq.path, nil, append(slices.Clone(rq.header), "Authorization", authz)...)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s: status %d, want 401 (body %q)", name, rec.Code, rec.Body)
			}
			wantError(t, rec, http.StatusUnauthorized, api.CodeUnauthenticated)
			if cc := rec.Header().Values("Cache-Control"); !slices.Equal(cc, []string{cacheNoStore}) {
				t.Errorf("%s: Cache-Control %q, want exactly no-store", name, cc)
			}
			if sc := rec.Header().Values("Set-Cookie"); len(sc) != 0 {
				t.Errorf("%s: Set-Cookie %q", name, sc)
			}
			if calls := f.auth.take(); len(calls) != 0 {
				t.Errorf("%s: auth calls %v; want none (no CSRF, authenticate or rotate)", name, calls)
			}
			if reached != 0 {
				t.Fatalf("%s: the handler ran", name)
			}
		}
	}
}

// TestAPIRoutePattern: a request that an API route serves reports the API's pattern to 04's RouteObserver and in the
// 5xx log lines, whatever answers it (the handler or the chain); the API's own 404 and 405 report the router's
// "/api/v1/", so the label set stays bounded (04 §9.3, §11.2).
func TestAPIRoutePattern(t *testing.T) {
	f := newAPIFixture(t, nil)
	captureDefaultLog(t)
	f.api.Handle("GET /api/v1/test/things/{id}", Public, noContent)
	f.api.Handle("GET /api/v1/test/user", User, noContent)
	f.api.Handle("GET /api/v1/test/panic", Public, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("handler bug")
	}))
	f.api.Handle("GET /api/v1/test/error", Public, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, errors.New("db down"))
	}))
	for _, tc := range []struct {
		method, path string
		header       []string
		pattern      string
		status       int
	}{
		{"GET", "/api/v1/info", nil, "GET /api/v1/info", 200},
		{"HEAD", "/api/v1/info", nil, "GET /api/v1/info", 200},
		{"GET", "/api/v1/test/things/x1", nil, "GET /api/v1/test/things/{id}", 204},
		{"GET", "/api/v1/test/user", nil, "GET /api/v1/test/user", 401},
		{"GET", "/api/v1/test/user", []string{"Authorization", "Bearer isa_x"}, "GET /api/v1/test/user", 401},
		{"GET", "/api/v1/test/panic", nil, "GET /api/v1/test/panic", 500},
		{"GET", "/api/v1/test/error", nil, "GET /api/v1/test/error", 500},
		{"GET", "/api/v1/nope", nil, "/api/v1/", 404},
		{"POST", "/api/v1/info", sameOriginJSON, "/api/v1/", 405},
	} {
		name := tc.method + " " + tc.path
		rec := f.do(tc.method, tc.path, nil, tc.header...)
		if rec.Code != tc.status {
			t.Fatalf("%s: status %d, want %d (body %q)", name, rec.Code, tc.status, rec.Body)
		}
		obs := f.obs.all()
		if last := obs[len(obs)-1]; last.pattern != tc.pattern || last.status != tc.status {
			t.Errorf("%s: observed %q %d, want %q %d", name, last.pattern, last.status, tc.pattern, tc.status)
		}
	}
	want := map[string]string{
		"http handler panic":  "GET /api/v1/test/panic",
		"http internal error": "GET /api/v1/test/error",
	}
	for _, r := range f.logs.records(t) {
		msg, _ := r["msg"].(string)
		if route, ok := want[msg]; ok && r["route"] == route {
			delete(want, msg)
		}
	}
	if len(want) != 0 {
		t.Fatalf("log lines without their API route %v:\n%s", want, f.logs)
	}
}

// TestAPIAccess covers the access levels of 03 §12.1 and PrincipalFrom.
func TestAPIAccess(t *testing.T) {
	f := newAPIFixture(t, nil)
	whoami := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFrom(r.Context())
		WriteJSON(w, http.StatusOK, map[string]any{"ok": ok, "user": string(p.UserID), "admin": p.IsAdmin()})
	})
	f.api.Handle("GET /api/v1/test/public", Public, whoami)
	f.api.Handle("GET /api/v1/test/user", User, whoami)
	f.api.Handle("GET /api/v1/test/admin", Admin, whoami)

	type result struct {
		OK    bool   `json:"ok"`
		User  string `json:"user"`
		Admin bool   `json:"admin"`
	}
	for _, tc := range []struct {
		path, cookie string
		status       int
		code         string
		want         result
		calls        []string
	}{
		{"/api/v1/test/public", "", 200, "", result{}, []string{"csrf"}},
		{"/api/v1/test/public", "admin", 200, "", result{}, []string{"csrf"}}, // Public never authenticates
		{"/api/v1/test/user", "", 401, api.CodeUnauthenticated, result{}, []string{"csrf", "authenticate"}},
		{"/api/v1/test/user", "expired", 401, api.CodeUnauthenticated, result{}, []string{"csrf", "authenticate"}},
		{"/api/v1/test/user", "broken", 500, api.CodeInternal, result{}, []string{"csrf", "authenticate"}},
		{"/api/v1/test/user", "user", 200, "", result{true, "u1u1u1u1u1u1", false}, []string{"csrf", "authenticate", "rotate"}},
		{"/api/v1/test/user", "admin", 200, "", result{true, "a1a1a1a1a1a1", true}, []string{"csrf", "authenticate", "rotate"}},
		{"/api/v1/test/admin", "", 401, api.CodeUnauthenticated, result{}, []string{"csrf", "authenticate"}},
		{"/api/v1/test/admin", "user", 403, api.CodeForbidden, result{}, []string{"csrf", "authenticate", "rotate"}},
		{"/api/v1/test/admin", "admin", 200, "", result{true, "a1a1a1a1a1a1", true}, []string{"csrf", "authenticate", "rotate"}},
	} {
		var header []string
		if tc.cookie != "" {
			header = withCookie(tc.cookie)
		}
		rec := f.do(http.MethodGet, tc.path, nil, header...)
		name := tc.path + " cookie=" + tc.cookie
		if calls := f.auth.take(); !slices.Equal(calls, tc.calls) {
			t.Errorf("%s: calls %v, want %v", name, calls, tc.calls)
		}
		if tc.code != "" {
			wantError(t, rec, tc.status, tc.code)
			continue
		}
		if rec.Code != tc.status {
			t.Fatalf("%s: status %d (body %q)", name, rec.Code, rec.Body)
		}
		var got result
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("%s: handler saw %+v, want %+v", name, got, tc.want)
		}
	}
	if _, ok := PrincipalFrom(context.Background()); ok {
		t.Fatal("PrincipalFrom(empty context) reported a principal")
	}
}

// TestAPINotFoundAndMethodNotAllowed: every unmatched /api/v1 path gets JSON 404 not_found and every method mismatch
// JSON 405 method_not_allowed with Allow, also for routes added through Handle (03 §12.5).
func TestAPINotFoundAndMethodNotAllowed(t *testing.T) {
	f := newAPIFixture(t, nil)
	f.api.Handle("POST /api/v1/test/thing", User, noContent)
	f.api.Handle("DELETE /api/v1/test/things/{id}", Admin, noContent)

	for _, tc := range []struct {
		method, path string
		status       int
		allow        string
	}{
		{"GET", "/api/v1/", 404, ""},
		{"GET", "/api/v1/nope", 404, ""},
		{"POST", "/api/v1/nope", 404, ""},
		{"GET", "/api/v1/info/", 404, ""},
		{"GET", "/api/v1/INFO", 404, ""},
		{"GET", "/api/v1/info/extra", 404, ""},
		{"GET", "/api/v1/test/things/", 404, ""},
		{"GET", "/api/v1/test/things/a/b", 404, ""},
		{"POST", "/api/v1/info", 405, "GET, HEAD"},
		{"PUT", "/api/v1/info", 405, "GET, HEAD"},
		{"DELETE", "/api/v1/info", 405, "GET, HEAD"},
		{"OPTIONS", "/api/v1/info", 405, "GET, HEAD"}, // no CORS in M1: a preflight is just another method
		{"GET", "/api/v1/test/thing", 405, "POST"},
		{"HEAD", "/api/v1/test/thing", 405, "POST"},
		{"GET", "/api/v1/test/things/x1", 405, "DELETE"},
	} {
		rec := f.do(tc.method, tc.path, nil, sameOriginJSON...)
		name := tc.method + " " + tc.path
		code := api.CodeNotFound
		if tc.status == http.StatusMethodNotAllowed {
			code = api.CodeMethodNotAllowed
		}
		if rec.Code != tc.status {
			t.Fatalf("%s: status %d, want %d (body %q)", name, rec.Code, tc.status, rec.Body)
		}
		wantError(t, rec, tc.status, code)
		if got := rec.Header().Get("Allow"); got != tc.allow {
			t.Errorf("%s: Allow %q, want %q", name, got, tc.allow)
		}
		if calls := f.auth.take(); len(calls) != 0 {
			t.Errorf("%s: auth calls %v; want none", name, calls)
		}
	}
}

// TestAPINoStore: every /api/v1 response carries Cache-Control: no-store, whatever its status and whatever the
// handler set (03 §12.1).
func TestAPINoStore(t *testing.T) {
	f := newAPIFixture(t, nil)
	f.api.Handle("GET /api/v1/test/cached", Public, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=60")
		WriteJSON(w, http.StatusOK, map[string]int{"a": 1})
	}))
	f.api.Handle("GET /api/v1/test/implicit", Public, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "max-age=60")
		_, _ = io.WriteString(w, "{}")
	}))
	f.api.Handle("GET /api/v1/test/flush", Public, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "public")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush: %v", err)
		}
		_, _ = io.WriteString(w, "{}")
	}))
	f.api.Handle("GET /api/v1/test/early-hints", Public, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "public")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Set("Cache-Control", "public")
		_, _ = io.WriteString(w, "{}")
	}))
	f.api.Handle("GET /api/v1/test/empty", Public, noContent)
	f.api.Handle("GET /api/v1/test/panic", Public, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "public")
		panic("handler bug")
	}))
	f.api.Handle("GET /api/v1/test/error", Public, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public")
		WriteError(w, r, errors.New("db down"))
	}))
	f.api.Handle("GET /api/v1/test/rate", Public, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, &api.Error{Code: api.CodeRateLimited, RetryAfter: 42})
	}))
	f.api.Handle("POST /api/v1/test/decode", Public, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v struct{}
		if err := DecodeJSON(w, r, &v, 0); err != nil {
			WriteError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	f.api.Handle("GET /api/v1/test/user", User, noContent)
	f.api.Handle("GET /api/v1/test/admin", Admin, noContent)
	captureDefaultLog(t)

	for _, tc := range []struct {
		name, method, path string
		body               string
		header             []string
		status             int
	}{
		{"info", "GET", "/api/v1/info", "", nil, 200},
		{"info HEAD", "HEAD", "/api/v1/info", "", nil, 200},
		{"handler sets public", "GET", "/api/v1/test/cached", "", nil, 200},
		{"implicit 200", "GET", "/api/v1/test/implicit", "", nil, 200},
		{"flush", "GET", "/api/v1/test/flush", "", nil, 200},
		{"204", "GET", "/api/v1/test/empty", "", nil, 204},
		{"400", "POST", "/api/v1/test/decode", "{", sameOriginJSON, 400},
		{"401", "GET", "/api/v1/test/user", "", nil, 401},
		{"403 forbidden", "GET", "/api/v1/test/admin", "", withCookie("user"), 403},
		{"403 csrf", "POST", "/api/v1/test/decode", "{}", []string{"Sec-Fetch-Site", "cross-site"}, 403},
		{"404", "GET", "/api/v1/nope", "", nil, 404},
		{"405", "PUT", "/api/v1/info", "", nil, 405},
		{"413", "POST", "/api/v1/test/decode", strings.Repeat(" ", otherBodyLimit+1), sameOriginJSON, 413},
		{"415", "POST", "/api/v1/test/decode", "{}", []string{"Sec-Fetch-Site", "same-origin"}, 415},
		{"429", "GET", "/api/v1/test/rate", "", nil, 429},
		{"500 error", "GET", "/api/v1/test/error", "", nil, 500},
		{"500 panic", "GET", "/api/v1/test/panic", "", nil, 500},
	} {
		var body io.Reader
		if tc.body != "" {
			body = strings.NewReader(tc.body)
		}
		rec := f.do(tc.method, tc.path, body, tc.header...)
		if rec.Code != tc.status {
			t.Errorf("%s: status %d, want %d (body %q)", tc.name, rec.Code, tc.status, rec.Body)
		}
		if cc := rec.Header().Values("Cache-Control"); !slices.Equal(cc, []string{cacheNoStore}) {
			t.Errorf("%s: Cache-Control %q, want exactly no-store", tc.name, cc)
		}
	}

	// httptest.ResponseRecorder keeps a 1xx status as the final one, so the 103 case runs over a real connection.
	t.Run("103 then 200", func(t *testing.T) {
		srv := httptest.NewServer(f.rt.Handler())
		defer srv.Close()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/api/v1/test/early-hints", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = testHost
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if cc := resp.Header.Values("Cache-Control"); resp.StatusCode != http.StatusOK || !slices.Equal(cc, []string{cacheNoStore}) {
			t.Fatalf("status %d, Cache-Control %q", resp.StatusCode, cc)
		}
	})

	t.Run("shutting down", func(t *testing.T) {
		f.gate.SetShuttingDown()
		rec := f.do(http.MethodGet, "/api/v1/info", nil)
		wantError(t, rec, http.StatusServiceUnavailable, api.CodeServerShutdown)
	})
}

// TestAPIBodyLimit: 16 KiB for /api/v1/auth/ and /api/v1/device/, 64 KiB elsewhere (03 §12.1), whether the length is
// declared or not, and even when the handler asks DecodeJSON for more.
func TestAPIBodyLimit(t *testing.T) {
	f := newAPIFixture(t, nil)
	decode := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v map[string]string
		if err := DecodeJSON(w, r, &v, 1<<20); err != nil {
			WriteError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	for _, p := range []string{"/api/v1/auth/test", "/api/v1/device/test", "/api/v1/test", "/api/v1/authx/test"} {
		f.api.Handle("POST "+p, Public, decode)
	}
	// jsonOfSize returns a JSON object of exactly n bytes.
	jsonOfSize := func(n int) string { return `{"p":"` + strings.Repeat("x", n-8) + `"}` }
	for _, tc := range []struct {
		path  string
		limit int
	}{
		{"/api/v1/auth/test", authBodyLimit},
		{"/api/v1/device/test", authBodyLimit},
		{"/api/v1/test", otherBodyLimit},
		{"/api/v1/authx/test", otherBodyLimit},
	} {
		for _, known := range []bool{true, false} {
			for _, size := range []int{tc.limit, tc.limit + 1} {
				var body io.Reader = strings.NewReader(jsonOfSize(size))
				if !known {
					body = io.MultiReader(body) // hides the length: the request is chunked
				}
				rec := f.do(http.MethodPost, tc.path, body, sameOriginJSON...)
				name := fmt.Sprintf("%s %d bytes (known length %v)", tc.path, size, known)
				if size <= tc.limit {
					if rec.Code != http.StatusNoContent {
						t.Fatalf("%s: status %d (body %q)", name, rec.Code, rec.Body)
					}
					continue
				}
				wantError(t, rec, http.StatusRequestEntityTooLarge, api.CodePayloadTooLarge)
			}
		}
	}
}

func TestAPIHandlePanics(t *testing.T) {
	f := newAPIFixture(t, nil)
	for _, tc := range []struct {
		name, pattern string
		access        Access
		h             http.Handler
	}{
		{"outside /api/v1", "GET /api/v2/x", Public, noContent},
		{"not under the prefix", "GET /healthz", Public, noContent},
		{"the prefix without its slash", "GET /api/v1", Public, noContent},
		{"a host", "GET watch.example.com/api/v1/x", Public, noContent},
		{"unknown access", "GET /api/v1/x", Admin + 1, noContent},
		{"nil handler", "GET /api/v1/x", Public, nil},
		{"conflict with /info", "GET /api/v1/info", Public, noContent},
		{"invalid pattern", "GET /api/v1/{", Public, noContent},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: Handle(%q) did not panic", tc.name, tc.pattern)
				}
			}()
			f.api.Handle(tc.pattern, tc.access, tc.h)
		}()
	}
	if s := (Admin + 1).String(); s != "Access(3)" {
		t.Errorf("String of an unknown Access = %q", s)
	}
	if s := Public.String() + " " + User.String() + " " + Admin.String(); s != "public user admin" {
		t.Errorf("Access names = %q", s)
	}
}

// TestNew checks New's required dependencies and that it accepts auth's Service, whose methods later slices fill in.
func TestNew(t *testing.T) {
	db := openTestDB(t)
	for _, tc := range []struct {
		d    Deps
		want string
	}{
		{Deps{Auth: new(auth.Service), Site: domainSite()}, "Deps.DB is nil"},
		{Deps{DB: db, Site: domainSite()}, "Deps.Auth is nil"},
		{Deps{DB: db, Auth: new(auth.Service)}, "Deps.Site.Origin is empty"},
		{Deps{DB: db, Auth: new(auth.Service), Site: Site{Hostname: testHost}}, "Deps.Site.Origin is empty"},
	} {
		func() {
			defer func() {
				if p := recover(); !strings.Contains(fmt.Sprint(p), tc.want) {
					t.Errorf("New(%+v) panicked with %v, want %q", tc.d, p, tc.want)
				}
			}()
			New(tc.d)
		}()
	}
	a := New(Deps{DB: db, Auth: new(auth.Service), Site: domainSite()})
	f := newFixture(t, func(o *RouterOptions) { o.API = a })
	wantError(t, f.get("/api/v1/nope"), http.StatusNotFound, api.CodeNotFound)
}

func TestDashboardAccountsNotImplemented(t *testing.T) {
	f := newAPIFixture(t, nil)
	_, err := f.api.DashboardAccounts(context.Background())
	if !api.IsCode(err, api.CodeInternal) || !strings.Contains(err.Error(), "DashboardAccounts") {
		t.Fatalf("DashboardAccounts error = %v; want one that names the method and wraps internal", err)
	}
}

func TestNopSignal(t *testing.T) {
	var s Signal = nopSignal{}
	if len(s.RoomPresence()) != 0 || len(s.OnlineUserIDs()) != 0 {
		t.Fatal("nopSignal reports presence")
	}
	s.RoomDeleted("lounge")
	s.UserChanged("u1u1u1u1u1u1", "Sam", false)
	s.Notify(NotifyTarget{All: true})

	// A nil Deps.Signal becomes the no-op one.
	f := newAPIFixture(t, func(d *Deps) { d.Signal = nil })
	if _, ok := f.api.d.Signal.(nopSignal); !ok {
		t.Fatalf("Deps.Signal = %T, want nopSignal", f.api.d.Signal)
	}
}
