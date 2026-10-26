package httpapi

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// The CORS step of M2 02 §6.4 (README WA47): the CORS matrix of §16.1 through the whole chain with a real
// auth.Service and the desktop origins the wiring passes (protocol.DesktopOrigins()).

// The CORS response headers.
const (
	hACAO = "Access-Control-Allow-Origin"
	hACAM = "Access-Control-Allow-Methods"
	hACAH = "Access-Control-Allow-Headers"
	hACMA = "Access-Control-Max-Age"
	hACEH = "Access-Control-Expose-Headers"
	hACAC = "Access-Control-Allow-Credentials"
)

// newCORSFixture is the auth fixture with the desktop origins of the wiring.
func newCORSFixture(t *testing.T) *authFixture {
	t.Helper()
	return newAuthFixtureDeps(t, func(d *Deps) { d.DesktopOrigins = protocol.DesktopOrigins() })
}

// preflight sends a CORS preflight from origin for method, with each element of reqHeaders as one
// Access-Control-Request-Headers line.
func (f *authFixture) preflight(origin, path, method string, reqHeaders ...string) *httptest.ResponseRecorder {
	f.t.Helper()
	opts := []reqOpt{header("Origin", origin), header("Sec-Fetch-Site", "cross-site"), header("Content-Type", ""),
		header("Access-Control-Request-Method", method)}
	for _, h := range reqHeaders {
		opts = append(opts, func(r *http.Request) { r.Header.Add("Access-Control-Request-Headers", h) })
	}
	return f.call(http.MethodOptions, path, "", opts...)
}

// fromDesktop makes a request come from the desktop SPA at origin: a cross-site fetch in the webview.
func fromDesktop(origin string) []reqOpt {
	return []reqOpt{header("Origin", origin), header("Sec-Fetch-Site", "cross-site")}
}

// wantCORS checks the CORS headers of an answer to a desktop origin that is not a preflight: Allow-Origin, Expose
// and Vary, never Allow-Credentials, and none of the preflight's headers.
func wantCORS(t *testing.T, what string, rec *httptest.ResponseRecorder, origin string) {
	t.Helper()
	h := rec.Header()
	if got := h.Values(hACAO); !slices.Equal(got, []string{origin}) {
		t.Errorf("%s: %s %q, want %q", what, hACAO, got, origin)
	}
	if got := h.Values(hACEH); !slices.Equal(got, []string{"Retry-After, WWW-Authenticate, X-Request-Id"}) {
		t.Errorf("%s: %s %q", what, hACEH, got)
	}
	if !slices.Contains(h.Values("Vary"), "Origin") {
		t.Errorf("%s: Vary %q, want Origin", what, h.Values("Vary"))
	}
	for _, k := range []string{hACAC, hACAM, hACAH, hACMA} {
		if got := h.Values(k); len(got) != 0 {
			t.Errorf("%s: %s %q", what, k, got)
		}
	}
}

// wantNoCORS checks that an answer carries no CORS header and no Vary: Origin.
func wantNoCORS(t *testing.T, what string, rec *httptest.ResponseRecorder) {
	t.Helper()
	for _, k := range []string{hACAO, hACAM, hACAH, hACMA, hACEH, hACAC} {
		if got := rec.Header().Values(k); len(got) != 0 {
			t.Errorf("%s: %s %q", what, k, got)
		}
	}
	if slices.Contains(rec.Header().Values("Vary"), "Origin") {
		t.Errorf("%s: Vary: Origin", what)
	}
}

// TestCORSPreflight: preflights from each desktop origin get the allow headers, cached 600 s, exactly for the methods
// and request headers the desktop SPA uses (any case, any order, one line or several), and 204 without them
// otherwise, also on paths no route has; never Allow-Credentials. Preflights from other origins reach the route
// lookup and get M1's 405; without desktop origins every origin is another one.
func TestCORSPreflight(t *testing.T) {
	f := newCORSFixture(t)
	origins := protocol.DesktopOrigins()
	if len(origins) != 5 || origins[0] != "http://wails.localhost" {
		t.Fatalf("protocol.DesktopOrigins() = %q", origins)
	}

	// M2 02 §7.2's example, header by header.
	rec := f.preflight("http://wails.localhost", "/api/v1/rooms", http.MethodGet, "authorization")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("the example preflight: status %d, body %q", rec.Code, rec.Body)
	}
	for k, want := range map[string]string{
		hACAO:           "http://wails.localhost",
		hACAM:           "GET, POST, PUT, PATCH, DELETE",
		hACAH:           "Authorization, Content-Type",
		hACMA:           "600",
		"Vary":          "Origin",
		"Cache-Control": "no-store",
	} {
		if got := rec.Header().Values(k); !slices.Equal(got, []string{want}) {
			t.Errorf("the example preflight: %s %q, want %q", k, got, want)
		}
	}

	allowedHeaders := [][]string{
		nil, {""}, {"authorization"}, {"content-type"}, {"authorization,content-type"}, {"Content-Type, Authorization"},
		{"AUTHORIZATION"}, {"authorization", "content-type"}, {"authorization, "}, {"content-type,\tauthorization"},
	}
	refusedHeaders := [][]string{
		{"x-requested-with"}, {"authorization, cookie"}, {"authorization", "x-custom"}, {"content-type;"},
		{"authorization content-type"},
	}
	n := 0
	for _, origin := range origins {
		for _, path := range []string{"/api/v1/rooms", "/api/v1/device/revoke", "/api/v1/nothing-here"} {
			for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
				for _, hs := range allowedHeaders {
					rec := f.preflight(origin, path, method, hs...)
					n++
					what := origin + " " + method + " " + path + " " + strings.Join(hs, "|")
					if rec.Code != http.StatusNoContent || rec.Header().Get(hACAO) != origin ||
						rec.Header().Get(hACAM) == "" || rec.Header().Get(hACAH) == "" || rec.Header().Get(hACMA) != "600" ||
						len(rec.Header().Values(hACAC)) != 0 || !slices.Contains(rec.Header().Values("Vary"), "Origin") {
						t.Fatalf("allowed preflight %s: status %d, headers %v", what, rec.Code, rec.Header())
					}
				}
			}
			refused := func(what string, rec *httptest.ResponseRecorder) {
				t.Helper()
				n++
				if rec.Code != http.StatusNoContent || !slices.Contains(rec.Header().Values("Vary"), "Origin") {
					t.Fatalf("refused preflight %s: status %d, Vary %q", what, rec.Code, rec.Header().Values("Vary"))
				}
				for _, k := range []string{hACAO, hACAM, hACAH, hACMA, hACAC, hACEH} {
					if got := rec.Header().Values(k); len(got) != 0 {
						t.Fatalf("refused preflight %s: %s %q", what, k, got)
					}
				}
			}
			for _, method := range []string{"HEAD", "OPTIONS", "CONNECT", "TRACE", "patch", "Get"} {
				refused(origin+" "+method+" "+path, f.preflight(origin, path, method, "authorization"))
			}
			for _, hs := range refusedHeaders {
				refused(origin+" GET "+path+" "+strings.Join(hs, "|"), f.preflight(origin, path, http.MethodGet, hs...))
			}
			// Two Access-Control-Request-Method headers ask for nothing a browser sends.
			refused(origin+" two methods", f.call(http.MethodOptions, path, "", header("Origin", origin),
				header("Access-Control-Request-Method", "GET"), func(r *http.Request) {
					r.Header.Add("Access-Control-Request-Method", "POST")
				}))
		}
	}
	t.Logf("%d preflights from the desktop origins", n)

	// Other origins, and the desktop ones spelled otherwise or sent twice: untouched, M1's 405 (404 for no route).
	for _, origin := range []string{"https://evil.example", testOrigin, "null", "http://wails.localhost:34115",
		"HTTP://WAILS.LOCALHOST", "http://wails.localhost/", "http://wails.localhost.evil.example", ""} {
		rec := f.preflight(origin, "/api/v1/rooms", http.MethodGet, "authorization")
		wantError(t, rec, http.StatusMethodNotAllowed, api.CodeMethodNotAllowed)
		wantNoCORS(t, "a preflight from "+origin, rec)
		rec = f.preflight(origin, "/api/v1/nothing-here", http.MethodGet)
		wantError(t, rec, http.StatusNotFound, api.CodeNotFound)
		wantNoCORS(t, "a preflight to no route from "+origin, rec)
	}
	rec = f.call(http.MethodOptions, "/api/v1/rooms", "", header("Origin", "http://wails.localhost"),
		header("Access-Control-Request-Method", "GET"), func(r *http.Request) { r.Header.Add("Origin", "http://wails.localhost") })
	wantError(t, rec, http.StatusMethodNotAllowed, api.CodeMethodNotAllowed)
	wantNoCORS(t, "two Origin headers", rec)

	// Without desktop origins (Deps.DesktopOrigins empty) there is no CORS at all.
	plain := newAuthFixture(t)
	rec = plain.preflight("http://wails.localhost", "/api/v1/rooms", http.MethodGet, "authorization")
	wantError(t, rec, http.StatusMethodNotAllowed, api.CodeMethodNotAllowed)
	wantNoCORS(t, "a preflight without desktop origins", rec)
	rec = plain.call(http.MethodGet, "/api/v1/info", "", fromDesktop("http://wails.localhost")...)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /info without desktop origins: status %d", rec.Code)
	}
	wantNoCORS(t, "GET /info without desktop origins", rec)
}

// TestCORSRequests: a desktop origin's requests carry Allow-Origin, Expose and Vary on every answer of the chain,
// errors included (401, 403, 404, 405, 413, 429), never Allow-Credentials; their Cookie header is dropped, so only a
// bearer gets in; other origins' answers carry no CORS header.
func TestCORSRequests(t *testing.T) {
	f := newCORSFixture(t)
	_, cookie := f.setupAdmin("Alex")
	app := f.linkApp(cookie, "PC", addrA)
	f.api.Handle("GET /api/v1/test/limited", User, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, &api.Error{Code: api.CodeRateLimited, RetryAfter: 7})
	}))
	f.api.Handle("POST /api/v1/test/echo", User, echoPrincipal)

	for _, origin := range protocol.DesktopOrigins() {
		desktop := fromDesktop(origin)
		withDesktop := func(opts ...reqOpt) []reqOpt { return append(slices.Clone(desktop), opts...) }

		// A bearer gets in; the answer carries the CORS headers.
		rec := f.call(http.MethodGet, "/api/v1/rooms", "", withDesktop(withBearer(app.AccessToken))...)
		decodeBody[api.Rooms](t, rec, http.StatusOK)
		wantCORS(t, origin+" GET /rooms", rec, origin)
		// The webview's cookie is dropped before anything reads it: alone it is no credential, beside a bearer the
		// handler never sees it.
		rec = f.call(http.MethodGet, "/api/v1/me", "", withDesktop(cookieValue(cookie.Name, cookie.Value))...)
		wantError(t, rec, http.StatusUnauthorized, api.CodeUnauthenticated)
		wantCORS(t, origin+" GET /me with a cookie", rec, origin)
		rec = f.call(http.MethodPost, "/api/v1/test/echo", `{}`, withDesktop(cookieValue(cookie.Name, cookie.Value),
			withBearer(app.AccessToken))...)
		if got := outcomeOf(t, rec); got != "200 bearer Alex" || strings.Contains(rec.Body.String(), `"cookie":true`) {
			t.Errorf("%s: a cookie and a bearer: %s, body %q", origin, got, rec.Body)
		}
		wantCORS(t, origin+" a cookie and a bearer", rec, origin)
		rec = f.call(http.MethodGet, "/api/v1/info", "", withDesktop(cookieValue(cookie.Name, cookie.Value))...)
		decodeBody[api.Info](t, rec, http.StatusOK)
		wantCORS(t, origin+" GET /info", rec, origin)

		// Errors of every step carry them too.
		rec = f.call(http.MethodGet, "/api/v1/me", "", withDesktop(withBearer(unknownAccessToken))...)
		wantInvalidToken(t, rec)
		wantCORS(t, origin+" 401 invalid_token", rec, origin)
		rec = f.call(http.MethodPost, "/api/v1/device/revoke", `{}`, withDesktop(withBearer(app.AccessToken),
			header("Content-Type", "text/plain"))...)
		wantError(t, rec, http.StatusUnsupportedMediaType, api.CodeUnsupportedMediaType)
		wantCORS(t, origin+" 415", rec, origin)
		rec = f.call(http.MethodPost, "/api/v1/me/password", `{}`, withDesktop(withBearer(app.AccessToken))...)
		wantForbiddenFor(t, rec, api.CredentialKindSession)
		wantCORS(t, origin+" 403", rec, origin)
		rec = f.call(http.MethodPost, "/api/v1/device/code", `{"pad":"`+strings.Repeat("x", authBodyLimit)+`"}`, desktop...)
		wantError(t, rec, http.StatusRequestEntityTooLarge, api.CodePayloadTooLarge)
		wantCORS(t, origin+" 413", rec, origin)
		rec = f.call(http.MethodGet, "/api/v1/test/limited", "", withDesktop(withBearer(app.AccessToken))...)
		if e := wantError(t, rec, http.StatusTooManyRequests, api.CodeRateLimited); e.RetryAfter != 7 ||
			rec.Header().Get("Retry-After") != "7" {
			t.Errorf("%s 429: retryAfter %d, Retry-After %q", origin, e.RetryAfter, rec.Header().Get("Retry-After"))
		}
		wantCORS(t, origin+" 429", rec, origin)
		rec = f.call(http.MethodGet, "/api/v1/nothing-here", "", desktop...)
		wantError(t, rec, http.StatusNotFound, api.CodeNotFound)
		wantCORS(t, origin+" 404", rec, origin)
		rec = f.call(http.MethodPut, "/api/v1/rooms", `{}`, withDesktop(withBearer(app.AccessToken))...)
		wantError(t, rec, http.StatusMethodNotAllowed, api.CodeMethodNotAllowed)
		wantCORS(t, origin+" 405", rec, origin)
		// An unsafe Public route keeps M1's CSRF check: the desktop SPA never sends one (Go does, without an Origin).
		rec = f.call(http.MethodPost, "/api/v1/auth/logout", `{}`, withDesktop(cookieValue(cookie.Name, cookie.Value))...)
		wantError(t, rec, http.StatusForbidden, api.CodeCSRFFailed)
		wantCORS(t, origin+" 403 csrf_failed", rec, origin)
		// A non-preflight OPTIONS is an ordinary request: M1's 405, with the CORS headers.
		rec = f.call(http.MethodOptions, "/api/v1/rooms", "", desktop...)
		wantError(t, rec, http.StatusMethodNotAllowed, api.CodeMethodNotAllowed)
		wantCORS(t, origin+" a plain OPTIONS", rec, origin)
	}
	f.wantSignedIn(cookie) // nothing from a desktop origin used the cookie

	// The same requests from other origins carry no CORS header.
	for _, origin := range []string{testOrigin, "https://evil.example"} {
		rec := f.call(http.MethodGet, "/api/v1/rooms", "", header("Origin", origin), withBearer(app.AccessToken))
		decodeBody[api.Rooms](t, rec, http.StatusOK)
		wantNoCORS(t, origin+" GET /rooms", rec)
		rec = f.call(http.MethodGet, "/api/v1/me", "", header("Origin", origin), withBearer(unknownAccessToken))
		wantInvalidToken(t, rec)
		wantNoCORS(t, origin+" 401", rec)
	}
	rec := f.call(http.MethodGet, "/api/v1/me", "", with(cookie))
	decodeBody[api.Me](t, rec, http.StatusOK)
	wantNoCORS(t, "the SPA's own GET /me", rec)
}

// FuzzPreflightAllowed: preflightAllowed never panics and allows exactly what M2 02 §6.4 says, written out here
// independently: one of the five methods as sent, and request headers that are, after splitting at commas and
// trimming spaces and tabs, only authorization and content-type in any case (empty elements ignored).
func FuzzPreflightAllowed(f *testing.F) {
	for _, seed := range [][2]string{
		{"GET", "authorization"}, {"PATCH", "Content-Type, Authorization"}, {"DELETE", ""}, {"HEAD", "authorization"},
		{"POST", "authorization, cookie"}, {"PUT", ",,authorization,\t"}, {"get", "content-type"},
		{"GET", "authorization\ncontent-type"}, {"GET", "AUTHORIZATION;"}, {"OPTIONS", ""},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, method, headers string) {
		h := http.Header{}
		h.Set("Access-Control-Request-Method", method)
		lines := strings.Split(headers, "\n")
		for _, line := range lines {
			h.Add("Access-Control-Request-Headers", line)
		}
		want := slices.Contains([]string{"GET", "POST", "PUT", "PATCH", "DELETE"}, method)
		for _, line := range lines {
			for _, name := range strings.Split(line, ",") {
				switch strings.ToLower(strings.Trim(name, " \t")) {
				case "", "authorization", "content-type":
				default:
					want = false
				}
			}
		}
		if got := preflightAllowed(h); got != want {
			t.Fatalf("preflightAllowed(method %q, headers %q) = %v, want %v", method, headers, got, want)
		}
	})
}
