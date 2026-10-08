package tlsmgr

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/mholt/acmez/v3/acme"

	"github.com/MoonWX/isshoni/internal/server/config"
)

// serve sends one request to the port 80 handler.
func serve(h http.Handler, method, target, host string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, target, nil)
	if host != "" {
		req.Host = host
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func wantWaitingPage(t *testing.T, what string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "30" {
		t.Errorf("%s = %d (Retry-After %q), want 503 with Retry-After: 30", what, rec.Code, rec.Header().Get("Retry-After"))
	}
	if !strings.Contains(rec.Body.String(), "isshoni is getting its certificate. Refresh in a minute.") {
		t.Errorf("%s body = %q", what, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("%s Content-Type = %q", what, got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("%s Cache-Control = %q", what, got)
	}
	if rec.Header().Get("Location") != "" {
		t.Errorf("%s redirects to %q", what, rec.Header().Get("Location"))
	}
}

// TestHTTPHandlerOffMode: in off mode the handler is the app itself, also for the ACME path.
func TestHTTPHandlerOffMode(t *testing.T) {
	m, _ := newManager(t, Options{Mode: config.TLSOff})
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "app:"+r.URL.Path) //nolint:gosec // G705: a test handler; plain text
	})
	h := m.HTTPHandler(app)
	for _, p := range []string{"/", "/r/lounge", acmeChallengePrefix + "token"} {
		if rec := serve(h, http.MethodGet, p, "localhost:8080"); rec.Code != 200 || rec.Body.String() != "app:"+p {
			t.Errorf("GET %s = %d %q, want the app's answer", p, rec.Code, rec.Body.String())
		}
	}
	if rec := serve(m.HTTPHandler(nil), http.MethodGet, "/", ""); rec.Code != http.StatusNotFound {
		t.Errorf("off mode without an app = %d, want 404", rec.Code)
	}
}

// TestHTTPHandlerRedirect: with a certificate, port 80 answers 308 to the configured origin, whatever the request's
// Host says.
func TestHTTPHandlerRedirect(t *testing.T) {
	now := time.Now()
	ca := newTestCA(t, now)
	files := newCertFiles(t)
	files.writePair(t, ca.issue(t, now.Add(-time.Hour), now.Add(90*24*time.Hour), testDomain))
	m, _ := newManager(t, Options{
		Mode: config.TLSManual, CertFile: files.cert, KeyFile: files.key,
		Site: testSite(config.TLSManual, testDomain+":8443"),
	})
	start(t, m)
	app := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("the app was called outside off mode") })
	h := m.HTTPHandler(app)

	const origin = "https://" + testDomain + ":8443"
	tests := []struct {
		method, target, host string
		location             string
	}{
		{"GET", "/", testDomain, origin + "/"},
		{"GET", "/r/lounge?focus=abc&x=1", testDomain, origin + "/r/lounge?focus=abc&x=1"},
		// The request's Host never reaches the Location header: no open redirect.
		{"GET", "/invite/xyz", "evil.example", origin + "/invite/xyz"},
		{"GET", "/", "evil.example:80", origin + "/"},
		// A path that looks like a host stays a path on our own origin.
		{"GET", "//evil.example/x", testDomain, origin + "//evil.example/x"},
		{"GET", `/\evil.example`, testDomain, origin + `/%5Cevil.example`},
		{"GET", "/a%2Fb%20c?q=%2F", testDomain, origin + "/a%2Fb%20c?q=%2F"},
		// A request line with an authority (a client that takes the server for a proxy).
		{"GET", "http://evil.example/path?q=1", "evil.example", origin + "/path?q=1"},
		// The method and body are kept by a 308.
		{"POST", "/api/v1/auth/login", testDomain, origin + "/api/v1/auth/login"},
		{"HEAD", "/sw.js", testDomain, origin + "/sw.js"},
		// Manual mode has no ACME client, so the challenge path is a path like any other.
		{"GET", acmeChallengePrefix + "token", testDomain, origin + acmeChallengePrefix + "token"},
	}
	for _, tc := range tests {
		rec := serve(h, tc.method, tc.target, tc.host)
		if rec.Code != http.StatusPermanentRedirect || rec.Header().Get("Location") != tc.location {
			t.Errorf("%s %s (Host %s) = %d → %q, want 308 → %q", tc.method, tc.target, tc.host, rec.Code, rec.Header().Get("Location"), tc.location)
		}
		if rec.Header().Get("Strict-Transport-Security") != "" {
			t.Errorf("%s %s: HSTS on a plain-HTTP response (RFC 6797 §7.2)", tc.method, tc.target)
		}
	}
	// "OPTIONS *" has no path.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodOptions, "/", nil)
	req.URL.Path, req.RequestURI = "*", "*"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusPermanentRedirect || rec.Header().Get("Location") != origin+"/" {
		t.Errorf("OPTIONS * = %d → %q, want 308 → %q", rec.Code, rec.Header().Get("Location"), origin+"/")
	}
}

// TestHTTPHandlerManualWithoutCertificate: manual mode redirects even while its files are broken (04 §8.3 keeps the
// waiting page to the modes that get a certificate by themselves), unless the site's address is unknown.
func TestHTTPHandlerManualWithoutCertificate(t *testing.T) {
	files := newCertFiles(t) // nothing written
	m, _ := newManager(t, Options{Mode: config.TLSManual, CertFile: files.cert, KeyFile: files.key, Site: testSite(config.TLSManual, testDomain)})
	start(t, m)
	wantNotReady(t, m, CodeCertUnreadable)
	if rec := serve(m.HTTPHandler(nil), "GET", "/x", testDomain); rec.Code != http.StatusPermanentRedirect || rec.Header().Get("Location") != "https://"+testDomain+"/x" {
		t.Errorf("GET /x = %d → %q", rec.Code, rec.Header().Get("Location"))
	}

	// No domain and no public IP address: there is no origin to redirect to.
	nowhere, _ := newManager(t, Options{Mode: config.TLSManual, CertFile: files.cert, KeyFile: files.key, Site: config.Site{TLSMode: config.TLSManual}})
	wantWaitingPage(t, "without an origin", serve(nowhere.HTTPHandler(nil), "GET", "/x", "198.51.100.4"))
}

// acmeManager builds an auto-mode manager whose CA is a closed port on this machine: every order fails at once and
// nothing leaves the machine.
func acmeManager(t *testing.T, opts Options) (*Manager, *logSink) {
	t.Helper()
	if opts.Mode == "" {
		opts.Mode = config.TLSAuto
		opts.Domain = testDomain
	}
	opts.CA = "https://127.0.0.1:1/dir"
	opts.StorageDir = t.TempDir()
	if opts.Site.Origin == "" {
		opts.Site = testSite(opts.Mode, testDomain)
	}
	return newManager(t, opts)
}

// waitFor polls cond for up to 10 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestHTTPHandlerWhileGettingTheCertificate: in auto and ip mode, before the first certificate, port 80 shows the
// waiting page instead of a redirect to an HTTPS port that can't answer yet; the ACME path is certmagic's.
func TestHTTPHandlerWhileGettingTheCertificate(t *testing.T) {
	m, _ := acmeManager(t, Options{})
	h := m.HTTPHandler(nil)

	// Also before Start.
	wantWaitingPage(t, "before Start", serve(h, "GET", "/", testDomain))
	if rec := serve(h, "GET", acmeChallengePrefix+"token", testDomain); rec.Code != http.StatusNotFound {
		t.Errorf("challenge path before Start = %d, want 404", rec.Code)
	}

	start(t, m)
	wantWaitingPage(t, "GET /", serve(h, "GET", "/", testDomain))
	wantWaitingPage(t, "GET /r/lounge (another Host)", serve(h, "GET", "/r/lounge?x=1", "evil.example"))
	wantWaitingPage(t, "POST", serve(h, "POST", "/api/v1/auth/login", testDomain))

	// A challenge nobody asked for.
	rec := serve(h, "GET", acmeChallengePrefix+"unknown-token", testDomain)
	if rec.Code != http.StatusNotFound || rec.Header().Get("Location") != "" {
		t.Errorf("unknown challenge = %d (Location %q), want 404", rec.Code, rec.Header().Get("Location"))
	}
	// Only GET is a challenge request.
	if rec := serve(h, "POST", acmeChallengePrefix+"unknown-token", testDomain); rec.Code != http.StatusNotFound {
		t.Errorf("POST to the challenge path = %d, want 404", rec.Code)
	}
}

// TestHTTPHandlerACMEChallenge: a request under /.well-known/acme-challenge/ is answered by certmagic with the
// key authorization of the open challenge, for the challenge's own host only.
//
// The challenge is planted where certmagic keeps the ones that another instance on the same storage opened (its
// "distributed" solving): acme/<CA>/challenge_tokens/<name>.json. A real one, opened by an order, is what the
// Pebble tests in acme_test.go see.
func TestHTTPHandlerACMEChallenge(t *testing.T) {
	m, _ := acmeManager(t, Options{})
	start(t, m)
	st := m.acme.Load()
	chal := acme.Challenge{
		Type: acme.ChallengeTypeHTTP01, Token: "tok-123", KeyAuthorization: "tok-123.thumbprint",
		Identifier: acme.Identifier{Type: "dns", Value: testDomain},
	}
	data, err := json.Marshal(chal)
	if err != nil {
		t.Fatal(err)
	}
	key := path.Join("acme", certmagic.StorageKeys.Safe(st.issuer.IssuerKey()), "challenge_tokens", certmagic.StorageKeys.Safe(testDomain)+".json")
	if err := st.cfg.Storage.Store(context.Background(), key, data); err != nil {
		t.Fatal(err)
	}
	h := m.HTTPHandler(nil)

	rec := serve(h, "GET", acmeChallengePrefix+"tok-123", testDomain)
	if rec.Code != 200 || rec.Body.String() != "tok-123.thumbprint" {
		t.Fatalf("challenge request = %d %q, want the key authorization", rec.Code, rec.Body.String())
	}
	// The same with the port a CA may add to the Host header.
	if rec := serve(h, "GET", acmeChallengePrefix+"tok-123", testDomain+":80"); rec.Code != 200 || rec.Body.String() != "tok-123.thumbprint" {
		t.Errorf("challenge request with a port in Host = %d %q", rec.Code, rec.Body.String())
	}
	// Another token, or another host (DNS rebinding), gets nothing.
	if rec := serve(h, "GET", acmeChallengePrefix+"other", testDomain); rec.Code != http.StatusNotFound {
		t.Errorf("another token = %d %q, want 404", rec.Code, rec.Body.String())
	}
	if rec := serve(h, "GET", acmeChallengePrefix+"tok-123", "evil.example"); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "thumbprint") {
		t.Errorf("another host = %d %q, want 404", rec.Code, rec.Body.String())
	}
	// Everything else still waits for the certificate.
	wantWaitingPage(t, "GET /", serve(h, "GET", "/", testDomain))
}

// TestHTTPHandlerIPModeWithoutAddress: an ip-mode server that found no public address shows the waiting page.
func TestHTTPHandlerIPModeWithoutAddress(t *testing.T) {
	m, sink := newManager(t, Options{Mode: config.TLSIP, PublicIP: netip.Addr{}, StorageDir: t.TempDir(), Site: config.Site{TLSMode: config.TLSIP}})
	start(t, m)
	wantNotReady(t, m, "no public IP address to get a certificate for")
	wantWaitingPage(t, "GET /", serve(m.HTTPHandler(nil), "GET", "/", "198.51.100.4"))
	if st := m.Status(); st.Mode != config.TLSIP || st.Ready || len(st.Names) != 0 || st.Names == nil {
		t.Errorf("Status = %+v", st)
	}
	if len(sink.records(t, "no public IP address")) != 1 {
		t.Errorf("want one warning:\n%s", sink.String())
	}
	if _, err := handshake(t, m, nil, ""); err == nil {
		t.Error("a handshake succeeded without a certificate")
	}
}
