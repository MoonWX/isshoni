package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/deploy"
)

// tmplPS1 is a small template with the layout of deploy/install-desktop.ps1 (M2 06 §7.1).
const tmplPS1 = "# isshoni desktop installer for Windows. Usage: irm https://<your isshoni server>/install-desktop.ps1 | iex\n" +
	"# Docs: https://moonwx.github.io/isshoni/download#windows-powershell\n" +
	"# >>> isshoni-prefill (the server rewrites this block; see docs/m2/06-windows-release.md 7.4)\n" +
	"$IsshoniServer  = ''\n" +
	"$IsshoniVersion = '@VERSION@'\n" +
	"# <<< isshoni-prefill\n" +
	"$AllowedSigners = @'\n" +
	"# BEGIN ALLOWED SIGNERS (generated from deploy/keys/allowed_signers; do not edit here)\n" +
	"# END ALLOWED SIGNERS\n" +
	"'@\n" +
	"& {\n" +
	"    # >>> isshoni-prefill is only a marker at the start of a line\n" +
	"    Write-Host 'body'\n" +
	"}\n"

// tmplHead and tmplTail are the parts of tmplPS1 around its prefill block.
var (
	tmplHead = tmplPS1[:strings.Index(tmplPS1, prefillOpen)]
	tmplTail = tmplPS1[strings.Index(tmplPS1, prefillClose+"\n")+len(prefillClose)+1:]
)

// block is the prefill block RenderInstallPS1 writes for a server (written into the opening line as origin) and a
// version.
func block(origin, server, version string) string {
	by := " (written by an isshoni server)"
	if server != "" {
		by = " (written by the isshoni server at " + origin + ")"
	}
	return prefillOpen + by + "\n$IsshoniServer  = '" + server + "'\n$IsshoniVersion = '" + version + "'\n" +
		prefillClose + "\n"
}

// TestRenderInstallPS1Server: $IsshoniServer is the origin with ' doubled when it is ASCII without control
// characters and https:// or loopback http://; otherwise empty (M2 06 §7.4).
func TestRenderInstallPS1Server(t *testing.T) {
	for _, tc := range []struct {
		name, origin, server string
	}{
		{"https domain", "https://watch.example.com", "https://watch.example.com"},
		{"https with port", "https://watch.example.com:8443", "https://watch.example.com:8443"},
		{"https IPv4", "https://203.0.113.7", "https://203.0.113.7"},
		{"https IPv6", "https://[2001:db8::1]", "https://[2001:db8::1]"},
		{"quotes doubled", "https://o'brien.example", "https://o''brien.example"},
		{"two quotes doubled", "https://a''b.example", "https://a''''b.example"},
		{"dev server on localhost", "http://localhost:5173", "http://localhost:5173"},
		{"localhost in capitals", "http://LOCALHOST:8080", "http://LOCALHOST:8080"},
		{"loopback IPv4", "http://127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"loopback IPv4 range", "http://127.1.2.3", "http://127.1.2.3"},
		{"loopback IPv6", "http://[::1]:8080", "http://[::1]:8080"},
		{"no site", "", ""},
		{"non-ASCII host", "https://wätch.example", ""},
		{"typographic quote", "https://o’brien.example", ""},
		{"newline", "https://watch.example.com\n$x = 1", ""},
		{"tab", "https://watch.example.com\t", ""},
		{"NUL", "https://watch\x00.example.com", ""},
		{"DEL", "https://watch\x7f.example.com", ""},
		{"http on a domain", "http://watch.example.com", ""},
		{"http on a private address", "http://192.168.1.5:8080", ""},
		{"http on a host named like loopback", "http://localhost.example.com", ""},
		{"http on 0.0.0.0", "http://0.0.0.0:8080", ""},
		{"scheme in capitals", "HTTPS://watch.example.com", ""},
		{"other scheme", "ftp://watch.example.com", ""},
		{"no host", "https://", ""},
		{"not a URL", "watch.example.com", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RenderInstallPS1([]byte(tmplPS1), tc.origin, "0.2.0")
			if err != nil {
				t.Fatal(err)
			}
			if want := tmplHead + block(tc.origin, tc.server, "0.2.0") + tmplTail; string(got) != want {
				t.Errorf("got\n%s\nwant\n%s", got, want)
			}
			if !isASCII(got) {
				t.Error("the rendered script is not ASCII")
			}
		})
	}
}

// TestRenderInstallPS1Version: a release or rc version is kept, any other (dev, CI, e2e builds) gives an empty one.
func TestRenderInstallPS1Version(t *testing.T) {
	for _, tc := range []struct{ version, want string }{
		{"0.2.0", "0.2.0"},
		{"10.20.30", "10.20.30"},
		{"0.2.0-rc.1", "0.2.0-rc.1"},
		{"0.2.0-rc.12", "0.2.0-rc.12"},
		{"0.0.0-dev", ""},
		{"0.0.0-dev+1a2b3c4d5e6f", ""},
		{"0.0.0-dev+1a2b3c4d5e6f-dirty", ""},
		{"0.1.1-dev.1a2b3c4", ""},
		{"0.0.0-ci.42", ""},
		{"0.0.0-e2e.local", ""},
		{"0.2.0-beta.1", ""},
		{"0.2.0-rc", ""},
		{"0.2.0-rc.1+build", ""},
		{"0.2.0+build", ""},
		{"v0.2.0", ""},
		{"0.2", ""},
		{"0.2.0\n", ""},
		{"0.2.0'", ""},
		{"", ""},
	} {
		got, err := RenderInstallPS1([]byte(tmplPS1), testOrigin, tc.version)
		if err != nil {
			t.Fatal(err)
		}
		if want := tmplHead + block(testOrigin, testOrigin, tc.want) + tmplTail; string(got) != want {
			t.Errorf("version %q: got\n%s\nwant $IsshoniVersion = '%s'", tc.version, got, tc.want)
		}
	}
}

// TestRenderInstallPS1Markers: exactly one opening marker line and one closing marker line after it, else an error;
// whatever the block held is replaced, and the bytes around it are kept.
func TestRenderInstallPS1Markers(t *testing.T) {
	open, closeLine := prefillOpen+" (comment)\n", prefillClose+"\n"
	for _, tc := range []struct {
		name, tmpl string
		wantErr    string
	}{
		{"empty", "", "no \"# >>> isshoni-prefill\" line"},
		{"no markers", "Write-Host 'x'\n", "no \"# >>> isshoni-prefill\" line"},
		{"no closing marker", "a\n" + open + "b\n", "no \"# <<< isshoni-prefill\" line"},
		{"no opening marker", "a\n" + closeLine + "b\n", "no \"# >>> isshoni-prefill\" line"},
		{"opening marker twice", open + open + closeLine, "2 \"# >>> isshoni-prefill\" lines, want one"},
		{"closing marker twice", open + closeLine + "x\n" + closeLine, "2 \"# <<< isshoni-prefill\" lines, want one"},
		{"both twice", open + closeLine + open + closeLine, "2 \"# >>> isshoni-prefill\" lines, want one"},
		{"closing before opening", "a\n" + closeLine + open + "b\n", "\"# <<< isshoni-prefill\" line before its"},
		{"indented markers don't count", "  " + open + "  " + closeLine, "no \"# >>> isshoni-prefill\" line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RenderInstallPS1([]byte(tc.tmpl), testOrigin, "0.2.0")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || got != nil {
				t.Fatalf("got %q, %v; want an error with %q", got, err, tc.wantErr)
			}
		})
	}

	want := block(testOrigin, testOrigin, "0.2.0")
	for _, tc := range []struct{ name, tmpl, want string }{
		{"adjacent markers", "a\n" + open + closeLine + "b\n", "a\n" + want + "b\n"},
		{"other lines inside are replaced", open + "$X = 1\n$Y = 2\n$Z = 3\n" + closeLine, want},
		{"closing marker at the end without newline", "a\n" + open + prefillClose, "a\n" + want},
		{"closing marker with a comment", open + prefillClose + " end\nb\n", want + "b\n"},
		{"CRLF around the block is kept", "a\r\n" + prefillOpen + "\r\n" + prefillClose + "\r\nb\r\n",
			"a\r\n" + want + "b\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RenderInstallPS1([]byte(tc.tmpl), testOrigin, "0.2.0")
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}

	// The template is not changed in place.
	tmpl := []byte(tmplPS1)
	if _, err := RenderInstallPS1(tmpl, testOrigin, "0.2.0"); err != nil || string(tmpl) != tmplPS1 {
		t.Errorf("the template changed (%v)", err)
	}
}

// TestRenderEmbeddedInstallPS1: the committed script (deploy.InstallDesktopPS1) renders, and only its prefill block
// changes. This is the test that catches a build the wiring would refuse to start (M2 06 §7.4).
func TestRenderEmbeddedInstallPS1(t *testing.T) {
	tmpl := deploy.InstallDesktopPS1
	if len(tmpl) == 0 {
		t.Fatal("deploy.InstallDesktopPS1 is empty")
	}
	if !isASCII(tmpl) || bytes.IndexByte(tmpl, '\r') >= 0 {
		t.Error("deploy/install-desktop.ps1 is not ASCII with LF line ends (06 §7.1)")
	}
	for _, tc := range []struct{ origin, version, server, wantVersion string }{
		{"https://watch.example.com", "0.2.0", "https://watch.example.com", "0.2.0"},
		{"http://localhost:5173", "0.0.0-dev+1a2b3c4d5e6f", "http://localhost:5173", ""}, // task dev
		{"http://watch.example.com", "0.2.0-rc.1", "", "0.2.0-rc.1"},
	} {
		got, err := RenderInstallPS1(tmpl, tc.origin, tc.version)
		if err != nil {
			t.Fatalf("the committed install-desktop.ps1 doesn't render: %v", err)
		}
		if !isASCII(got) || bytes.IndexByte(got, '\r') >= 0 {
			t.Errorf("%s: the rendered script is not ASCII with LF line ends", tc.origin)
		}
		gotLines, tmplLines := strings.Split(string(got), "\n"), strings.Split(string(tmpl), "\n")
		if len(gotLines) != len(tmplLines) {
			t.Fatalf("%s: %d lines, the template has %d", tc.origin, len(gotLines), len(tmplLines))
		}
		at := -1
		for i, l := range tmplLines {
			if strings.HasPrefix(l, prefillOpen) {
				at = i
			}
		}
		if at < 0 || !strings.HasPrefix(tmplLines[at+3], prefillClose) {
			t.Fatalf("the template's prefill block is not four lines (06 §7.1)")
		}
		wantBlock := strings.Split(strings.TrimSuffix(block(tc.origin, tc.server, tc.wantVersion), "\n"), "\n")
		for i := range gotLines {
			want := tmplLines[i]
			if i >= at && i < at+4 {
				want = wantBlock[i-at]
			}
			if gotLines[i] != want {
				t.Errorf("%s: line %d is %q, want %q", tc.origin, i+1, gotLines[i], want)
			}
		}
	}
}

// desktopFixture is a router with RouterOptions.Desktop set from tmplPS1.
func desktopFixture(t *testing.T, mod func(*RouterOptions)) *fixture {
	t.Helper()
	return newFixture(t, func(o *RouterOptions) {
		o.Desktop = &DesktopScripts{PS1: []byte(tmplPS1), Origin: testOrigin, Version: "0.2.0"}
		if mod != nil {
			mod(o)
		}
	})
}

// TestInstallPS1Route: 200 with charset, Cache-Control no-cache, the ETag of the body, Content-Length; HEAD without a
// body; 304 for the current ETag; no auth (M2 06 §7.4).
func TestInstallPS1Route(t *testing.T) {
	f := desktopFixture(t, nil)
	want, err := RenderInstallPS1([]byte(tmplPS1), testOrigin, "0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(want)
	wantETag := `"` + hex.EncodeToString(sum[:])[:16] + `"`

	get := f.get(installPS1Path)
	if get.Code != http.StatusOK || !bytes.Equal(get.Body.Bytes(), want) {
		t.Fatalf("GET: %d %q, want 200 and the rendered script", get.Code, get.Body)
	}
	for k, v := range map[string]string{
		"Content-Type":            "text/plain; charset=utf-8",
		"Cache-Control":           "no-cache",
		"ETag":                    wantETag,
		"Content-Length":          strconv.Itoa(len(want)),
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": cspOther,
	} {
		if got := get.Header().Get(k); got != v {
			t.Errorf("GET %s = %q, want %q", k, got, v)
		}
	}
	if get.Header().Get("Content-Encoding") != "" || get.Header().Get("Set-Cookie") != "" {
		t.Errorf("GET headers %v", get.Header())
	}

	head := f.serve(newReq(http.MethodHead, installPS1Path))
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Errorf("HEAD: %d with %d body bytes, want 200 without a body", head.Code, head.Body.Len())
	}
	for _, k := range []string{"Content-Type", "Cache-Control", "ETag", "Content-Length"} {
		if head.Header().Get(k) != get.Header().Get(k) {
			t.Errorf("HEAD %s = %q, GET %q", k, head.Header().Get(k), get.Header().Get(k))
		}
	}

	if rec := f.get(installPS1Path, "If-None-Match", wantETag); rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Errorf("If-None-Match: %d %q, want 304", rec.Code, rec.Body)
	}
	if rec := f.get(installPS1Path, "If-None-Match", `"0000000000000000"`); rec.Code != http.StatusOK {
		t.Errorf("stale If-None-Match: %d, want 200", rec.Code)
	}
	// A cookie or an Origin header changes nothing: the route has no auth and no CSRF check.
	foreign := f.get(installPS1Path, "Cookie", "isshoni_session=x", "Origin", "https://evil.example")
	if foreign.Code != http.StatusOK || !bytes.Equal(foreign.Body.Bytes(), want) {
		t.Errorf("with a cookie and a foreign Origin: %d", foreign.Code)
	}
	// Other methods never get the script, nor index.html.
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := f.serve(newReq(m, installPS1Path))
		if rec.Code != http.StatusMethodNotAllowed || strings.Contains(rec.Body.String(), "isshoni-prefill") ||
			rec.Body.String() == indexHTML {
			t.Errorf("%s: %d %q, want 405", m, rec.Code, rec.Body)
		}
	}
	for _, o := range f.obs.all()[:3] {
		if o.pattern != "GET "+installPS1Path {
			t.Errorf("observed pattern %q, want %q", o.pattern, "GET "+installPS1Path)
		}
	}
	if logs := f.logs.String(); logs != "" {
		t.Errorf("logs: %s", logs)
	}
}

// TestInstallPS1RouteDev is what `task dev` serves: the dev server on 127.0.0.1:8080 behind Vite's
// http://localhost:5173 (deploy/dev/isshoni.dev.toml) gives a script pre-filled with the Vite origin, and a dev
// build's version leaves the version empty.
func TestInstallPS1RouteDev(t *testing.T) {
	f := newFixture(t, func(o *RouterOptions) {
		o.Site = Site{Origin: "http://localhost:5173", Host: "localhost:5173", Hostname: "localhost", TLSMode: "off",
			Dev: true}
		o.Desktop = &DesktopScripts{PS1: deploy.InstallDesktopPS1, Origin: o.Site.Origin, Version: "0.0.0-dev+1a2b3c4d5e6f"}
	})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8080"+installPS1Path, nil)
	rec := f.serve(req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %q", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, line := range []string{
		"\n" + prefillOpen + " (written by the isshoni server at http://localhost:5173)\n",
		"\n$IsshoniServer  = 'http://localhost:5173'\n",
		"\n$IsshoniVersion = ''\n",
	} {
		if !strings.Contains(body, line) {
			t.Errorf("the script lacks the line %q", strings.TrimSpace(line))
		}
	}
	if strings.Contains(f.logs.String(), `"level":"WARN"`) {
		t.Errorf("a dev origin warned: %s", f.logs)
	}
}

// TestInstallPS1RouteWithoutScript: without RouterOptions.Desktop, and with a template that doesn't render (which
// the wiring refuses at startup), /install-desktop.ps1 is a plain 404, never index.html.
func TestInstallPS1RouteWithoutScript(t *testing.T) {
	wantPlain404 := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusNotFound || rec.Body.String() != "404 not found\n" ||
			rec.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Errorf("%d %q (%s), want a plain 404", rec.Code, rec.Body, rec.Header().Get("Content-Type"))
		}
	}
	t.Run("no Desktop", func(t *testing.T) {
		f := newFixture(t, nil)
		wantPlain404(t, f.get(installPS1Path))
		if obs := f.obs.all(); obs[0].pattern != "GET "+installPS1Path {
			t.Errorf("observed %+v", obs)
		}
	})
	t.Run("no Desktop, no web UI", func(t *testing.T) {
		wantPlain404(t, newFixture(t, func(o *RouterOptions) { o.SPA = nil }).get(installPS1Path))
	})
	for name, d := range map[string]*DesktopScripts{
		"no markers": {PS1: []byte("Write-Host 'x'\n"), Origin: testOrigin, Version: "0.2.0"},
		"nil PS1":    {Origin: testOrigin, Version: "0.2.0"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, func(o *RouterOptions) { o.Desktop = d })
			wantPlain404(t, f.get(installPS1Path))
			recs := f.logs.records(t)
			if len(recs) != 1 || recs[0]["level"] != "ERROR" || recs[0]["msg"] != "install-desktop.ps1 is not served" {
				t.Errorf("logs %v, want one error", recs)
			}
		})
	}
}

// TestInstallPS1OriginWarning: an origin the script can't carry is left out with one warning when the router is
// built, not on every request.
func TestInstallPS1OriginWarning(t *testing.T) {
	for _, tc := range []struct {
		origin string
		warn   bool
	}{
		{testOrigin, false},
		{"", false},
		{"http://watch.example.com", true},
		{"https://wätch.example", true},
	} {
		logs := &syncBuffer{}
		rt := NewRouter(RouterOptions{
			Site:    domainSite(),
			Desktop: &DesktopScripts{PS1: []byte(tmplPS1), Origin: tc.origin, Version: "0.2.0"},
			Logger:  slog.New(slog.NewJSONHandler(logs, nil)),
		})
		for range 3 {
			rec := httptest.NewRecorder()
			rt.Handler().ServeHTTP(rec, newReq(http.MethodGet, installPS1Path))
			if rec.Code != http.StatusOK {
				t.Fatalf("%q: status %d", tc.origin, rec.Code)
			}
			if tc.warn && !strings.Contains(rec.Body.String(), "\n$IsshoniServer  = ''\n") {
				t.Errorf("%q: the script carries a server", tc.origin)
			}
		}
		var warns []map[string]any
		for _, r := range logs.records(t) {
			if r["level"] == "WARN" {
				warns = append(warns, r)
			}
		}
		switch {
		case !tc.warn && len(warns) != 0:
			t.Errorf("%q: warnings %v, want none", tc.origin, warns)
		case tc.warn && (len(warns) != 1 || warns[0]["origin"] != tc.origin ||
			!strings.HasPrefix(warns[0]["msg"].(string), "install-desktop.ps1 is served without this server's address")):
			t.Errorf("%q: warnings %v, want one naming the origin", tc.origin, warns)
		}
	}
}

// TestInstallShRoute: /install-desktop.sh is a 404 text for every method until M3, never the SPA, with the script
// served or not.
func TestInstallShRoute(t *testing.T) {
	for name, mod := range map[string]func(*RouterOptions){
		"with the ps1":   func(o *RouterOptions) { o.Desktop = &DesktopScripts{PS1: []byte(tmplPS1), Origin: testOrigin} },
		"without it":     nil,
		"without web UI": func(o *RouterOptions) { o.SPA = nil },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, mod)
			for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
				rec := f.serve(newReq(m, installShPath))
				wantBody := installShBody
				if m == http.MethodHead {
					wantBody = ""
				}
				if rec.Code != http.StatusNotFound || rec.Body.String() != wantBody {
					t.Errorf("%s: %d %q, want 404 %q", m, rec.Code, rec.Body, wantBody)
				}
				for k, v := range map[string]string{
					"Content-Type":   "text/plain; charset=utf-8",
					"Cache-Control":  "no-store",
					"Content-Length": strconv.Itoa(len(installShBody)),
				} {
					if got := rec.Header().Get(k); got != v {
						t.Errorf("%s %s = %q, want %q", m, k, got, v)
					}
				}
			}
			if installShBody != "Not available yet.\n" {
				t.Errorf("body %q", installShBody)
			}
			for _, o := range f.obs.all() {
				if o.pattern != installShPath {
					t.Errorf("observed pattern %q, want %q", o.pattern, installShPath)
				}
			}
		})
	}
	// Neighbors stay what they were: a dotted path is the SPA's plain 404, an undotted one index.html.
	f := newFixture(t, nil)
	if rec := f.get("/install-desktop.bat"); rec.Code != http.StatusNotFound || rec.Body.String() == installShBody {
		t.Errorf("/install-desktop.bat: %d %q", rec.Code, rec.Body)
	}
	if rec := f.get("/install-desktop"); rec.Code != http.StatusOK || rec.Body.String() != indexHTML {
		t.Errorf("/install-desktop: %d, want index.html", rec.Code)
	}
}

func isASCII(b []byte) bool {
	for _, c := range b {
		if c >= 0x80 {
			return false
		}
	}
	return true
}
