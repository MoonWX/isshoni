package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	testHost      = "watch.example.com"
	testPublic    = "https://watch.example.com"
	testDevOrigin = "http://localhost:5173" // a second trusted origin that is not the Host
)

func newTestGuard(t *testing.T) *CSRFGuard {
	t.Helper()
	g, err := NewCSRFGuard([]string{"HTTPS://Watch.Example.com:443", testDevOrigin})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// csrfCase is one request of the CSRF matrix.
type csrfCase struct {
	method, secFetchSite, origin string
	contentType                  []string // nil = no header
}

// wantStatus is the rule of 03 §7.5, written out independently of the implementation.
func (c csrfCase) wantStatus() int {
	switch c.method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return http.StatusOK
	}
	trusted := c.origin == testPublic || c.origin == testDevOrigin
	var crossOriginOK bool
	switch c.secFetchSite {
	case "same-origin", "none":
		crossOriginOK = true
	case "":
		hostMatches := strings.TrimPrefix(strings.TrimPrefix(c.origin, "https://"), "http://") == testHost
		crossOriginOK = c.origin == "" || hostMatches || trusted
	default:
		crossOriginOK = trusted
	}
	if !crossOriginOK {
		return http.StatusForbidden
	}
	if len(c.contentType) != 1 {
		return http.StatusUnsupportedMediaType
	}
	switch strings.ToLower(strings.ReplaceAll(c.contentType[0], " ", "")) {
	case "application/json", "application/json;charset=utf-8", `application/json;charset="utf-8"`:
		return http.StatusOK
	}
	return http.StatusUnsupportedMediaType
}

func TestCSRFMatrix(t *testing.T) {
	g := newTestGuard(t)
	var reached int
	h := g.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++
		w.WriteHeader(http.StatusOK)
	}))

	methods := []string{"GET", "HEAD", "OPTIONS", "POST", "PUT", "PATCH", "DELETE"}
	sites := []string{"", "same-origin", "none", "same-site", "cross-site", "bogus"}
	origins := []string{
		"",                     // absent
		testPublic,             // matches Host, trusted
		"http://" + testHost,   // matches Host (the check ignores the scheme), not trusted
		testDevOrigin,          // trusted, does not match Host
		"https://evil.example", // mismatch
		"https://watch.example.com.evil.example",
		"null",
	}
	contentTypes := [][]string{
		nil,
		{"application/json"},
		{"application/json; charset=utf-8"},
		{"Application/JSON; Charset=UTF-8"},
		{`application/json;charset="utf-8"`},
		{"application/json; charset=iso-8859-1"},
		{"text/plain"},
		{"text/plain; charset=utf-8"},
		{"application/x-www-form-urlencoded"},
		{"multipart/form-data; boundary=xyz"},
		{""},
		{"application/json, text/plain"},
		{"application/jsonx"},
		{"application/vnd.api+json"},
		{"application/json", "application/json"},
	}
	n := 0
	for _, m := range methods {
		for _, sfs := range sites {
			for _, o := range origins {
				for _, ct := range contentTypes {
					c := csrfCase{method: m, secFetchSite: sfs, origin: o, contentType: ct}
					r := httptest.NewRequestWithContext(t.Context(), m, "http://"+testHost+"/api/v1/auth/login", strings.NewReader("{}"))
					if sfs != "" {
						r.Header.Set("Sec-Fetch-Site", sfs)
					}
					if o != "" {
						r.Header.Set("Origin", o)
					}
					for _, v := range ct {
						r.Header.Add("Content-Type", v)
					}
					before := reached
					w := httptest.NewRecorder()
					h.ServeHTTP(w, r)
					want := c.wantStatus()
					if w.Code != want {
						t.Errorf("%+v: status %d, want %d", c, w.Code, want)
						continue
					}
					if (reached > before) != (want == http.StatusOK) {
						t.Errorf("%+v: handler reached = %v with status %d", c, reached > before, want)
					}
					if want != http.StatusOK {
						checkEnvelope(t, w, want)
					}
					n++
				}
			}
		}
	}
	if n != len(methods)*len(sites)*len(origins)*len(contentTypes) {
		t.Fatalf("ran %d cases", n)
	}
}

func checkEnvelope(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	wantCode := map[int]string{http.StatusForbidden: "csrf_failed", http.StatusUnsupportedMediaType: "unsupported_media_type"}[status]
	if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("error Content-Type %q", ct)
	}
	var env map[string]map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("error body %q: %v", w.Body.String(), err)
	}
	if len(env) != 1 || len(env["error"]) != 1 || env["error"]["code"] != wantCode {
		t.Errorf("error body %s, want {\"error\":{\"code\":%q}}", w.Body.String(), wantCode)
	}
}

func TestCSRFCheckErrors(t *testing.T) {
	g := newTestGuard(t)
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+testHost+"/api/v1/x", nil)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.Header.Set("Content-Type", "application/json")
	var ce *CSRFError
	if err := g.Check(r); !errors.As(err, &ce) || ce.Code != "csrf_failed" || ce.Status != http.StatusForbidden {
		t.Fatalf("cross-site POST: %v", err)
	}
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Del("Content-Type")
	if err := g.Check(r); !errors.As(err, &ce) || ce.Code != "unsupported_media_type" || ce.Status != 415 {
		t.Fatalf("POST without a JSON Content-Type: %v", err)
	}
	if ce.Error() != "auth: unsupported_media_type" {
		t.Fatalf("Error() = %q", ce.Error())
	}
	r.Header.Set("Content-Type", "application/json")
	if err := g.Check(r); err != nil {
		t.Fatalf("same-origin JSON POST: %v", err)
	}
	// A non-browser client (no Sec-Fetch-Site, no Origin) still needs the JSON type.
	r.Header.Del("Sec-Fetch-Site")
	if err := g.Check(r); err != nil {
		t.Fatalf("non-browser JSON POST: %v", err)
	}
	// An empty body needs it too.
	r = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+testHost+"/api/v1/auth/logout", nil)
	if err := g.Check(r); !errors.As(err, &ce) || ce.Status != 415 {
		t.Fatalf("empty POST without Content-Type: %v", err)
	}
}

func TestCSRFNoTrustedOrigins(t *testing.T) {
	g, err := NewCSRFGuard(nil)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+testHost+"/api/v1/x", nil)
	r.Header.Set("Origin", testPublic)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.Header.Set("Content-Type", "application/json")
	if err := g.Check(r); err == nil {
		t.Fatal("a cross-site request passed with no trusted origins")
	}
}

func TestNormalizeOrigin(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://watch.example.com", "https://watch.example.com"},
		{"HTTPS://Watch.Example.COM", "https://watch.example.com"},
		{"https://watch.example.com:443", "https://watch.example.com"},
		{"https://watch.example.com:8443", "https://watch.example.com:8443"},
		{"https://watch.example.com:08443", "https://watch.example.com:8443"},
		{"http://localhost:80", "http://localhost"},
		{"http://localhost:443", "http://localhost:443"},
		{"http://localhost:5173", "http://localhost:5173"},
		{"https://203.0.113.7", "https://203.0.113.7"},
		{"https://[2001:DB8:0:0::1]", "https://[2001:db8::1]"},
		{"https://[2001:db8::1]:8443", "https://[2001:db8::1]:8443"},
		{"https://xn--bcher-kva.example", "https://xn--bcher-kva.example"},
	} {
		got, err := normalizeOrigin(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("normalizeOrigin(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{
		"", "watch.example.com", "https://", "https://watch.example.com/", "https://watch.example.com/app",
		"https://user@watch.example.com", "https://user:pw@watch.example.com", "ftp://watch.example.com",
		"https://watch.example.com?x=1", "https://watch.example.com?", "https://watch.example.com#top",
		"https://bücher.example", "https://watch.example.com:0", "https://watch.example.com:70000",
		"https://watch.example.com:x", "https://[fe80::1%25eth0]", "https:watch.example.com", "//watch.example.com",
		"https://:443",
	} {
		if got, err := normalizeOrigin(in); err == nil {
			t.Errorf("normalizeOrigin(%q) = %q, want an error", in, got)
		}
		if _, err := NewCSRFGuard([]string{testPublic, in}); err == nil {
			t.Errorf("NewCSRFGuard accepted %q", in)
		}
	}
}

func FuzzNormalizeOrigin(f *testing.F) {
	for _, s := range []string{"https://watch.example.com", "HTTP://[::1]:80", "https://a:08443", "https://x/", "http://%41"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		out, err := normalizeOrigin(in)
		if err != nil {
			return
		}
		again, err := normalizeOrigin(out)
		if err != nil || again != out {
			t.Fatalf("normalizeOrigin(%q) = %q, but normalizing that gives %q, %v", in, out, again, err)
		}
		if err := http.NewCrossOriginProtection().AddTrustedOrigin(out); err != nil {
			t.Fatalf("normalizeOrigin(%q) = %q, which AddTrustedOrigin refuses: %v", in, out, err)
		}
	})
}

// FuzzCSRFGuard checks the guard's safety properties on arbitrary headers: a safe method always passes, and an
// unsafe request passes only with a single JSON Content-Type and, when the browser says it is cross-site or
// same-site, only from a trusted origin.
func FuzzCSRFGuard(f *testing.F) {
	f.Add("POST", "cross-site", testPublic, "application/json")
	f.Add("POST", "", "https://evil.example", "application/json")
	f.Add("DELETE", "same-origin", "", "text/plain")
	f.Add("GET", "cross-site", "https://evil.example", "")
	f.Add("PATCH", "", "", "application/json; charset=utf-8")
	g, err := NewCSRFGuard([]string{testPublic})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, method, sfs, origin, ct string) {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+testHost+"/api/v1/x", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Method = method
		r.Header.Set("Sec-Fetch-Site", sfs)
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", ct)
		err = g.Check(r)
		if isSafeMethod(method) {
			if err != nil {
				t.Fatalf("safe method %q refused: %v", method, err)
			}
			return
		}
		if err != nil {
			var ce *CSRFError
			if !errors.As(err, &ce) || (ce.Status != 403 && ce.Status != 415) {
				t.Fatalf("Check error %v", err)
			}
			return
		}
		if !isJSONContentType([]string{ct}) {
			t.Fatalf("unsafe %q passed with Content-Type %q", method, ct)
		}
		if sfs != "" && sfs != "same-origin" && sfs != "none" && origin != testPublic {
			t.Fatalf("unsafe %q with Sec-Fetch-Site %q passed from origin %q", method, sfs, origin)
		}
	})
}
