package httpapi

import (
	"crypto/tls"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const wantHTMLCSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; " +
	"media-src 'self' blob:; connect-src 'self' wss://watch.example.com; worker-src 'self'; manifest-src 'self'; " +
	"font-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

const wantPermissions = "display-capture=(self), fullscreen=(self), picture-in-picture=(self), autoplay=(self), " +
	"camera=(), microphone=(), geolocation=(), usb=(), payment=(), browsing-topics=()"

// TestSecurityHeaders checks 04 §9.6 per response type.
func TestSecurityHeaders(t *testing.T) {
	f := newFixture(t, func(o *RouterOptions) {
		o.API = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { WriteJSON(w, 200, map[string]int{}) })
	})
	f.rt.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { WriteJSON(w, 200, map[string]string{"status": "ok"}) })
	shutting := newFixture(t, nil)
	shutting.gate.SetShuttingDown()
	misdirected := newReq(http.MethodGet, "/")
	misdirected.Host = "evil.example"

	type want struct {
		csp, coop, permissions string
	}
	html := want{wantHTMLCSP, "same-origin", wantPermissions}
	other := want{cspOther, "", ""}
	tests := []struct {
		name string
		f    *fixture
		req  *http.Request
		want want
	}{
		{"index.html fallback", f, newReq(http.MethodGet, "/r/lounge"), html},
		{"root", f, newReq(http.MethodGet, "/"), html},
		{"/index.html", f, newReq(http.MethodGet, "/index.html"), html},
		{"/setup as 404", newFixture(t, func(o *RouterOptions) { o.SPAStatus = func(string) int { return 404 } }), newReq(http.MethodGet, "/setup"), html},
		{"sw.js: the HTML policy only", f, newReq(http.MethodGet, "/sw.js"), want{wantHTMLCSP, "", ""}},
		{"hashed script", f, newReq(http.MethodGet, "/"+testJS), other},
		{"manifest", f, newReq(http.MethodGet, "/manifest.webmanifest"), other},
		{"icon", f, newReq(http.MethodGet, "/icons/icon-192.png"), other},
		{"robots", f, newReq(http.MethodGet, "/robots.txt"), other},
		{"API JSON", f, newReq(http.MethodGet, "/api/v1/me"), other},
		{"health", f, newReq(http.MethodGet, "/healthz"), other},
		{"missing asset", f, newReq(http.MethodGet, "/assets/gone-000.js"), other},
		{"dotted 404", f, newReq(http.MethodGet, "/favicon.ico"), other},
		{"405", f, newReq(http.MethodPost, "/"), other},
		{"reserved 404", f, newReq(http.MethodGet, "/api/x"), other},
		{"gate 503", shutting, newReq(http.MethodGet, "/"), other},
		{"421", f, misdirected, other},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := tt.f.serve(tt.req).Header()
			if got := h.Get("Content-Security-Policy"); got != tt.want.csp {
				t.Errorf("CSP = %q\nwant %q", got, tt.want.csp)
			}
			if got := h.Get("Cross-Origin-Opener-Policy"); got != tt.want.coop {
				t.Errorf("COOP = %q, want %q", got, tt.want.coop)
			}
			if got := h.Get("Permissions-Policy"); got != tt.want.permissions {
				t.Errorf("Permissions-Policy = %q, want %q", got, tt.want.permissions)
			}
			for k, v := range map[string]string{
				"X-Content-Type-Options":       "nosniff",
				"Referrer-Policy":              "no-referrer",
				"Cross-Origin-Resource-Policy": "same-origin",
				"X-Robots-Tag":                 "noindex",
			} {
				if got := h.Get(k); got != v {
					t.Errorf("%s = %q, want %q (every response)", k, got, v)
				}
			}
		})
	}
}

func TestHTMLCSPConnectSrc(t *testing.T) {
	tests := []struct {
		site Site
		want string
	}{
		{domainSite(), "connect-src 'self' wss://watch.example.com;"},
		{Site{Origin: "https://203.0.113.7:8443", Host: "203.0.113.7:8443"}, "connect-src 'self' wss://203.0.113.7:8443;"},
		{Site{Origin: "https://[2001:db8::1]", Host: "[2001:db8::1]"}, "connect-src 'self' wss://[2001:db8::1];"},
		{devSite(), "connect-src 'self' ws://localhost:8080;"},
		{Site{}, "connect-src 'self';"},
	}
	for _, tt := range tests {
		if got := htmlCSP(tt.site); !strings.Contains(got, tt.want) {
			t.Errorf("site %q: CSP %q lacks %q", tt.site.Origin, got, tt.want)
		}
	}
}

// TestHSTS: only on TLS responses for a domain with tls.hsts, never for IP hosts or plain HTTP (04 §9.6). The zero
// RouterOptions sends it, as tls.hsts defaults to true (04 §4.3).
func TestHSTS(t *testing.T) {
	ipSite := Site{Origin: "https://203.0.113.7", Host: "203.0.113.7", Hostname: "203.0.113.7", TLSMode: "ip"}
	v6Site := Site{Origin: "https://[2001:db8::1]", Host: "[2001:db8::1]", Hostname: "2001:db8::1", TLSMode: "ip"}
	tests := []struct {
		name    string
		site    Site
		disable bool
		tls     bool
		want    bool
	}{
		{"domain over TLS (default options)", domainSite(), false, true, true},
		{"tls.hsts off", domainSite(), true, true, false},
		{"plain HTTP (off mode behind a proxy)", domainSite(), false, false, false},
		{"IPv4 host", ipSite, false, true, false},
		{"IPv6 host", v6Site, false, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, func(o *RouterOptions) { o.Site, o.DisableHSTS = tt.site, tt.disable })
			for _, path := range []string{"/", "/" + testJS, "/api/v1/x"} {
				req := newReq(http.MethodGet, path)
				req.Host = tt.site.Host
				req.TLS = nil
				if tt.tls {
					req.TLS = &tls.ConnectionState{}
				}
				got := f.serve(req).Header().Get("Strict-Transport-Security")
				if (got == hstsValue) != tt.want || (got != "" && got != hstsValue) {
					t.Errorf("%s: Strict-Transport-Security = %q, want sent=%v", path, got, tt.want)
				}
			}
		})
	}
	// A wiring that fills only 04 §9.2's fields keeps HSTS on.
	rt := NewRouter(RouterOptions{Site: domainSite(), Logger: slog.New(slog.DiscardHandler)})
	req := newReq(http.MethodGet, "/")
	req.TLS = &tls.ConnectionState{}
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, req)
	if got := rec.Header().Get("Strict-Transport-Security"); got != hstsValue {
		t.Errorf("zero RouterOptions: Strict-Transport-Security = %q, want %q", got, hstsValue)
	}
}
