package assets

import (
	"bytes"
	"context"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
)

// testOrigin is the asset origin of the tests: the Windows one (02's protocol.DesktopAssetOriginWindows).
const testOrigin = "http://wails.localhost"

// specPolicyNoServer is 03 §5.3's policy, verbatim, with connect-src 'self' only (no active server).
const specPolicyNoServer = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; " +
	"media-src 'self' blob:; connect-src 'self'; worker-src 'none'; manifest-src 'none'; font-src 'self'; " +
	"object-src 'none'; base-uri 'none'; form-action 'none'; frame-src 'none'; frame-ancestors 'none'"

func testDist(t *testing.T) fstest.MapFS {
	t.Helper()
	index, err := os.ReadFile("testdata/index.html")
	if err != nil {
		t.Fatal(err)
	}
	return fstest.MapFS{
		"index.html":                  {Data: index},
		"boot-check.js":               {Data: []byte("/* boot check */")},
		"assets/index-ZhGWVs6R.js":    {Data: []byte("console.log('entry')")},
		"assets/index-ZhGWVs6R.js.br": {Data: []byte("br")},
		"assets/index-ZhGWVs6R.js.gz": {Data: []byte("gz")},
		"assets/index-ByXtkcz9.css":   {Data: []byte("body{}")},
		"icons/icon.svg":              {Data: []byte("<svg/>")},
		"version.json":                {Data: []byte(`{"version":"0.0.0-dev"}`)},
		"licenses.txt":                {Data: []byte("MIT")},
		"sw.js":                       {Data: []byte("self.addEventListener('fetch',()=>{})")},
		"manifest.webmanifest":        {Data: []byte("{}")},
		".gitkeep":                    {Data: nil},
		"data.bin":                    {Data: []byte{0, 1, 2}},
	}
}

// testServer is a BridgeConfig source the tests can switch.
type testServer struct {
	calls  atomic.Int32
	server atomic.Pointer[ServerFacts]
}

func (s *testServer) bridge() BridgeConfig {
	s.calls.Add(1)
	return BridgeConfig{
		Transport: "wails",
		App:       AppFacts{Version: "0.2.0", OS: "windows", OSBuild: 26100, Arch: "amd64"},
		Server:    s.server.Load(),
	}
}

func newTestHandler(t *testing.T, o Options) (http.Handler, *testServer) {
	t.Helper()
	src := &testServer{}
	if o.Dist == nil {
		o.Dist = testDist(t)
	}
	if o.Bridge == nil {
		o.Bridge = src.bridge
	}
	if o.Origin == "" {
		o.Origin = testOrigin
	}
	h, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h, src
}

// response is a recorded response, its body read.
type response struct {
	StatusCode int
	Header     http.Header
	Body       string
}

func get(t *testing.T, h http.Handler, method, target string) response {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, testOrigin+target, nil))
	return response{StatusCode: rec.Code, Header: rec.Header(), Body: rec.Body.String()}
}

func TestNewNeedsExactlyOneInjectionPoint(t *testing.T) {
	cases := map[string]fstest.MapFS{
		"no index.html": {"boot-check.js": {Data: []byte("x")}},
		"no boot-check tag": {"index.html": {Data: []byte(
			`<html><head></head><body><script src="/other.js"></script></body></html>`)}},
		"two boot-check tags": {"index.html": {Data: []byte(`<html><head></head><body>` + bootCheckTag + bootCheckTag +
			`</body></html>`)}},
		"a module boot-check tag": {"index.html": {Data: []byte(
			`<html><head></head><body><script type="module" src="/boot-check.js"></script></body></html>`)}},
	}
	for name, dist := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := New(Options{Dist: dist, Bridge: func() BridgeConfig { return BridgeConfig{} }, Origin: testOrigin})
			if !errors.Is(err, ErrNoInjectionPoint) {
				t.Fatalf("New = %v, want ErrNoInjectionPoint", err)
			}
		})
	}
	if _, err := New(Options{Dist: fstest.MapFS{"index.html": {Data: []byte(bootCheckTag)}},
		Bridge: func() BridgeConfig { return BridgeConfig{} }, Origin: testOrigin}); err == nil {
		t.Fatal("New accepted an index.html without <head>")
	}
}

func TestNewChecksOptions(t *testing.T) {
	dist := testDist(t)
	bridge := func() BridgeConfig { return BridgeConfig{} }
	for name, o := range map[string]Options{
		"no bridge":               {Dist: dist, Origin: testOrigin},
		"no dist":                 {Bridge: bridge, Origin: testOrigin},
		"no origin":               {Dist: dist, Bridge: bridge},
		"origin with a path":      {Dist: dist, Bridge: bridge, Origin: testOrigin + "/x"},
		"origin with a quote":     {Dist: dist, Bridge: bridge, Origin: `http://wails.localhost"`},
		"dev server in a release": {Dist: dist, Bridge: bridge, Origin: testOrigin, DevServer: "http://localhost:5173"},
	} {
		if _, err := New(o); err == nil {
			t.Errorf("%s: New accepted %+v", name, o)
		}
	}
	for _, origin := range []string{"http://wails.localhost", "wails://localhost", "https://wails.localhost"} {
		if _, err := New(Options{Dist: dist, Bridge: bridge, Origin: origin}); err != nil {
			t.Errorf("New(Origin %q) = %v", origin, err)
		}
	}
}

func TestIndexGetsTheBridgeTagOnceBeforeBootCheck(t *testing.T) {
	h, _ := newTestHandler(t, Options{})
	for _, p := range []string{"/", "/index.html", "/rooms", "/r/lounge", "/desktop/setup", "/api/v1/info"} {
		resp := get(t, h, http.MethodGet, p)
		page := resp.Body
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d", p, resp.StatusCode)
		}
		if got := resp.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
			t.Errorf("GET %s: Content-Type %q", p, got)
		}
		if got := resp.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("GET %s: Cache-Control %q", p, got)
		}
		if n := strings.Count(page, bridgeTag); n != 1 {
			t.Fatalf("GET %s: %d bridge tags", p, n)
		}
		if !strings.Contains(page, bridgeTag+bootCheckTag) {
			t.Errorf("GET %s: the bridge tag is not right before boot-check.js", p)
		}
		if strings.Index(page, `<meta charset="UTF-8" />`) > strings.Index(page, `http-equiv="Content-Security-Policy"`) {
			t.Errorf("GET %s: the CSP <meta> comes before <meta charset>", p)
		}
	}
}

func TestHeadIndex(t *testing.T) {
	h, _ := newTestHandler(t, Options{})
	resp := get(t, h, http.MethodHead, "/")
	if resp.StatusCode != http.StatusOK || resp.Body != "" || resp.Header.Get("Content-Length") == "" {
		t.Fatalf("HEAD /: %d, Content-Length %q", resp.StatusCode, resp.Header.Get("Content-Length"))
	}
	if resp.Header.Get("Content-Security-Policy") == "" {
		t.Error("HEAD / has no CSP")
	}
}

var metaCSP = regexp.MustCompile(`<meta http-equiv="Content-Security-Policy" content="([^"]*)">`)

// policies returns the header and <meta> policies of GET /.
func policies(t *testing.T, h http.Handler) (header, meta string) {
	t.Helper()
	resp := get(t, h, http.MethodGet, "/")
	page := resp.Body
	m := metaCSP.FindAllStringSubmatch(page, -1)
	if len(m) != 1 {
		t.Fatalf("%d CSP <meta> tags", len(m))
	}
	return resp.Header.Get("Content-Security-Policy"), html.UnescapeString(m[0][1])
}

func TestCSPHeaderAndMetaAreEqual(t *testing.T) {
	h, src := newTestHandler(t, Options{})
	header, meta := policies(t, h)
	if header != specPolicyNoServer || meta != specPolicyNoServer {
		t.Fatalf("no server:\nheader %q\nmeta   %q\nwant   %q", header, meta, specPolicyNoServer)
	}

	src.server.Store(&ServerFacts{ID: "k3m9p2qx", Origin: "https://watch.example.com", Name: "Friends"})
	header, meta = policies(t, h)
	want := strings.Replace(specPolicyNoServer, "connect-src 'self'",
		"connect-src 'self' https://watch.example.com wss://watch.example.com", 1)
	if header != want || meta != want {
		t.Fatalf("active server:\nheader %q\nmeta   %q\nwant   %q", header, meta, want)
	}

	src.server.Store(&ServerFacts{Origin: "https://Watch.Example.com:8443"})
	header, _ = policies(t, h)
	if !strings.Contains(header, "connect-src 'self' https://watch.example.com:8443 wss://watch.example.com:8443;") {
		t.Fatalf("a port and upper case: %q", header)
	}
}

func TestCSPConnectSrcByOrigin(t *testing.T) {
	cases := []struct {
		origin string
		dev    bool
		want   string // the connect-src sources after 'self'
	}{
		{"https://watch.example.com", false, " https://watch.example.com wss://watch.example.com"},
		{"https://[2001:db8::1]:8443", false, " https://[2001:db8::1]:8443 wss://[2001:db8::1]:8443"},
		{"https://192.0.2.7", false, " https://192.0.2.7 wss://192.0.2.7"},
		{"https://watch.example.com/", false, " https://watch.example.com wss://watch.example.com"},
		// http only in dev and e2e builds, and only for loopback.
		{"http://127.0.0.1:8080", false, ""},
		{"http://localhost:8080", false, ""},
		{"http://127.0.0.1:8080", true, " http://127.0.0.1:8080 ws://127.0.0.1:8080"},
		{"http://localhost:8080", true, " http://localhost:8080 ws://localhost:8080"},
		{"http://[::1]:8080", true, " http://[::1]:8080 ws://[::1]:8080"},
		{"http://watch.example.com", true, ""},
		{"http://192.168.1.2:8080", true, ""},
		// Anything that isn't a bare http(s) origin gets nothing.
		{"", false, ""},
		{"wss://watch.example.com", false, ""},
		{"ftp://watch.example.com", false, ""},
		{"https://watch.example.com/path", false, ""},
		{"https://user@watch.example.com", false, ""},
		{"https://watch.example.com?x", false, ""},
		{"https://watch.example.com#x", false, ""},
		{"https://watch.example.com; script-src *", false, ""},
		{"https://watch.example.com\" onload=\"x", false, ""},
		{"https://watch.example.com:99999999", false, ""},
		{"https://watch.example.com:", false, ""},
		{"https://*.example.com", false, ""},
		{"https://watch.exa_mple.com", false, ""},
		{"https://wätch.example.com", false, ""},
		{"https://[::ffff:192.0.2.7]", false, ""},
	}
	for _, c := range cases {
		h, src := newTestHandler(t, Options{Dev: c.dev})
		src.server.Store(&ServerFacts{Origin: c.origin})
		header, meta := policies(t, h)
		want := strings.Replace(specPolicyNoServer, "connect-src 'self'", "connect-src 'self'"+c.want, 1)
		if header != want || meta != want {
			t.Errorf("origin %q dev %v:\n got %q\nwant %q", c.origin, c.dev, header, want)
		}
	}
}

// TestReleasePolicyHasNoDevSources is the release-build test of §5.3: whatever the active server, a handler that
// isn't a dev build's never allows http:, ws: or Vite's dev origin.
func TestReleasePolicyHasNoDevSources(t *testing.T) {
	h, src := newTestHandler(t, Options{})
	for _, origin := range []string{"", "https://watch.example.com", "http://127.0.0.1:8080", "http://localhost:5173",
		"http://[::1]:5173", "ws://localhost:5173"} {
		src.server.Store(&ServerFacts{Origin: origin})
		header, meta := policies(t, h)
		for _, p := range []string{header, meta} {
			if strings.Contains(p, "http:") || strings.Contains(p, "ws:") || strings.Contains(p, "localhost:5173") ||
				strings.Contains(p, "unsafe-") {
				t.Errorf("origin %q: release policy %q", origin, p)
			}
		}
	}
}

func TestHTMLHeaders(t *testing.T) {
	h, _ := newTestHandler(t, Options{})
	resp := get(t, h, http.MethodGet, "/")
	for k, want := range map[string]string{
		"X-Content-Type-Options":     "nosniff",
		"Referrer-Policy":            "no-referrer",
		"Cross-Origin-Opener-Policy": "same-origin",
		"Permissions-Policy": "fullscreen=(self), picture-in-picture=(self), autoplay=(self), display-capture=(), " +
			"camera=(), microphone=(), geolocation=(), usb=(), payment=()",
	} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestNoServiceWorkerOrManifest(t *testing.T) {
	h, _ := newTestHandler(t, Options{})
	for _, p := range []string{"/sw.js", "/manifest.webmanifest"} {
		if resp := get(t, h, http.MethodGet, p); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", p, resp.StatusCode)
		}
	}
}

func TestMisses(t *testing.T) {
	h, _ := newTestHandler(t, Options{})
	for _, p := range []string{"/missing.js", "/assets/missing.js", "/assets/no-dot", "/icons/x.png", "/.gitkeep",
		"/assets/index-ZhGWVs6R.js.br", "/assets/index-ZhGWVs6R.js.gz", "/wails/unknown", "/wails"} {
		resp := get(t, h, http.MethodGet, p)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", p, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
			t.Errorf("GET %s: a miss answered with %s", p, ct)
		}
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h, _ := newTestHandler(t, Options{})
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
		for _, p := range []string{"/", "/assets/index-ZhGWVs6R.js", BridgePath} {
			resp := get(t, h, m, p)
			if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "GET, HEAD" {
				t.Errorf("%s %s: status %d, Allow %q", m, p, resp.StatusCode, resp.Header.Get("Allow"))
			}
		}
	}
}

func TestFiles(t *testing.T) {
	h, _ := newTestHandler(t, Options{})
	cases := []struct {
		path, contentType, cache, body string
	}{
		{"/assets/index-ZhGWVs6R.js", "text/javascript; charset=utf-8", "public, max-age=31536000, immutable",
			"console.log('entry')"},
		{"/assets/index-ByXtkcz9.css", "text/css; charset=utf-8", "public, max-age=31536000, immutable", "body{}"},
		{"/boot-check.js", "text/javascript; charset=utf-8", "no-cache", "/* boot check */"},
		{"/version.json", "application/json", "no-cache", `{"version":"0.0.0-dev"}`},
		{"/icons/icon.svg", "image/svg+xml", "no-cache", "<svg/>"},
		{"/licenses.txt", "text/plain; charset=utf-8", "no-cache", "MIT"},
		{"/data.bin", "application/octet-stream", "no-cache", "\x00\x01\x02"},
	}
	for _, c := range cases {
		resp := get(t, h, http.MethodGet, c.path)
		got := resp.Body
		if resp.StatusCode != http.StatusOK || got != c.body {
			t.Errorf("GET %s: %d %q", c.path, resp.StatusCode, got)
		}
		if ct := resp.Header.Get("Content-Type"); ct != c.contentType {
			t.Errorf("GET %s: Content-Type %q, want %q", c.path, ct, c.contentType)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != c.cache {
			t.Errorf("GET %s: Cache-Control %q, want %q", c.path, cc, c.cache)
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Content-Encoding") != "" {
			t.Errorf("GET %s: headers %v", c.path, resp.Header)
		}
		etag := resp.Header.Get("ETag")
		if etag == "" {
			t.Errorf("GET %s: no ETag", c.path)
			continue
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, testOrigin+c.path, nil)
		req.Header.Set("If-None-Match", etag)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotModified {
			t.Errorf("GET %s with its ETag: %d, want 304", c.path, rec.Code)
		}
	}
	// A precompressed sibling is never chosen, whatever the webview accepts.
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, testOrigin+"/assets/index-ZhGWVs6R.js", nil)
	req.Header.Set("Accept-Encoding", "br, gzip")
	h.ServeHTTP(rec, req)
	if rec.Body.String() != "console.log('entry')" || rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("Accept-Encoding br: %q %v", rec.Body.String(), rec.Header())
	}
}

func TestBridgeJSRoute(t *testing.T) {
	h, src := newTestHandler(t, Options{})
	resp := get(t, h, http.MethodGet, BridgePath)
	got := resp.Body
	want := `window.__ISSHONI_DESKTOP__=Object.freeze({"v":1,"transport":"wails","app":{"version":"0.2.0",` +
		`"os":"windows","osBuild":26100,"arch":"amd64"},"server":null,"serverIds":[],"dev":false});`
	if resp.StatusCode != http.StatusOK || got != want {
		t.Fatalf("GET %s: %d\n got %s\nwant %s", BridgePath, resp.StatusCode, got, want)
	}
	for k, v := range map[string]string{"Content-Type": "text/javascript; charset=utf-8", "Cache-Control": "no-store"} {
		if resp.Header.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, resp.Header.Get(k), v)
		}
	}

	// The config is read on every request: a server switch shows in the next bridge.js and the next CSP.
	before := src.calls.Load()
	src.server.Store(&ServerFacts{ID: "k3m9p2qx", Origin: "https://watch.example.com", Name: "Friends"})
	got = get(t, h, http.MethodGet, BridgePath).Body
	if !strings.Contains(got, `"server":{"id":"k3m9p2qx","origin":"https://watch.example.com","name":"Friends"}`) {
		t.Errorf("after the switch: %s", got)
	}
	header, _ := policies(t, h)
	if !strings.Contains(header, "wss://watch.example.com") {
		t.Errorf("after the switch: %s", header)
	}
	if n := src.calls.Load() - before; n != 2 {
		t.Errorf("Bridge called %d times for two requests", n)
	}
}

func TestMetaInsertionPoint(t *testing.T) {
	cases := map[string]string{
		"<html><head><title>x</title></head></html>":                     "<html><head>|<title>x</title></head></html>",
		"<html><HEAD lang=en>\n  <META CHARSET=utf-8>\n<title>x</title>": "<html><HEAD lang=en>\n  <META CHARSET=utf-8>|\n<title>x</title>",
		"<html><header>no</header><head><meta charset=\"utf-8\"><x>":     "<html><header>no</header><head><meta charset=\"utf-8\">|<x>",
		"<html><head>\n<meta name=viewport><meta charset=utf-8>":         "<html><head>|\n<meta name=viewport><meta charset=utf-8>",
	}
	for in, want := range cases {
		at := metaInsertionPoint([]byte(in))
		if at < 0 {
			t.Errorf("%q: no insertion point", in)
			continue
		}
		if got := in[:at] + "|" + in[at:]; got != want {
			t.Errorf("%q:\n got %q\nwant %q", in, got, want)
		}
	}
	if at := metaInsertionPoint([]byte("<html><body></body></html>")); at != -1 {
		t.Errorf("no <head>: %d", at)
	}
}

func TestIndexIsTheDistIndex(t *testing.T) {
	h, _ := newTestHandler(t, Options{})
	page := get(t, h, http.MethodGet, "/").Body
	index, err := os.ReadFile("testdata/index.html")
	if err != nil {
		t.Fatal(err)
	}
	stripped := metaCSP.ReplaceAllString(strings.Replace(page, bridgeTag, "", 1), "")
	if !bytes.Equal([]byte(stripped), index) {
		t.Errorf("the page differs from dist/index.html beyond the bridge tag and the CSP <meta>:\n%s", page)
	}
}
