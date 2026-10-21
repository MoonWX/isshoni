package assets

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// viteIndex is what Vite's dev server answers for /index.html: its client and React's refresh preamble added.
const viteIndex = `<!doctype html>
<html lang="en">
  <head>
    <script type="module">import { injectIntoGlobalHook } from "/@react-refresh";
injectIntoGlobalHook(window);</script>
    <script type="module" src="/@vite/client"></script>
    <meta charset="UTF-8" />
    <style>body{margin:0}</style>
    <title>isshoni</title>
  </head>
  <body>
    <div id="root"></div>
    <script src="/boot-check.js"></script>
    <script type="module" src="/src/main.tsx"></script>
  </body>
</html>
`

// viteModules are the fake dev server's modules: path → body.
var viteModules = map[string]string{
	"/@vite/client":  "// /@vite/client",
	"/src/main.tsx":  "// /src/main.tsx",
	"/boot-check.js": "// /boot-check.js",
}

// fakeVite is a dev server that records what reaches it.
type fakeVite struct {
	mu       sync.Mutex
	requests []*http.Request
}

func (v *fakeVite) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	v.mu.Lock()
	v.requests = append(v.requests, r.Clone(r.Context()))
	v.mu.Unlock()
	if r.URL.Path == "/index.html" {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, viteIndex)
		return
	}
	if m, ok := viteModules[r.URL.Path]; ok {
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = io.WriteString(w, m)
		return
	}
	http.NotFound(w, r)
}

func (v *fakeVite) last() *http.Request {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.requests[len(v.requests)-1]
}

func newDevHandler(t *testing.T) (http.Handler, *fakeVite, string) {
	t.Helper()
	vite := &fakeVite{}
	srv := httptest.NewServer(vite)
	t.Cleanup(srv.Close)
	h, err := New(Options{Bridge: (&testServer{}).bridge, Origin: testOrigin, Dev: true, DevServer: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return h, vite, srv.URL
}

var nonceAttr = regexp.MustCompile(` nonce="([A-Za-z0-9_-]+)"`)

func TestDevServerIndex(t *testing.T) {
	h, _, devURL := newDevHandler(t)
	devHost := strings.TrimPrefix(devURL, "http://")
	for _, p := range []string{"/", "/index.html", "/rooms", "/desktop/setup"} {
		resp := get(t, h, http.MethodGet, p)
		page := resp.Body
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d", p, resp.StatusCode)
		}
		if strings.Count(page, bridgeTag) != 1 || !strings.Contains(page, bridgeTag+bootCheckTag) {
			t.Fatalf("GET %s: bridge tag: %s", p, page)
		}
		nonces := nonceAttr.FindAllStringSubmatch(page, -1)
		// The inline script, the style and the csp-nonce <meta>; scripts with a src stay as they are.
		if len(nonces) != 3 || !strings.Contains(page, `<script nonce="`+nonces[0][1]+`" type="module">import`) ||
			!strings.Contains(page, `<style nonce="`) || !strings.Contains(page, `<script type="module" src="/@vite/client">`) {
			t.Fatalf("GET %s: %d nonce attributes, want 3:\n%s", p, len(nonces), page)
		}
		nonce := nonces[0][1]
		for _, n := range nonces {
			if n[1] != nonce {
				t.Fatalf("GET %s: two nonces", p)
			}
		}
		if !strings.Contains(page, `<meta property="csp-nonce" nonce="`+nonce+`">`) {
			t.Errorf("GET %s: no csp-nonce <meta>", p)
		}
		csp := resp.Header.Get("Content-Security-Policy")
		for _, want := range []string{
			"script-src 'self' " + devURL + " 'nonce-" + nonce + "';",
			"style-src 'self' " + devURL + " 'nonce-" + nonce + "';",
			"connect-src 'self' " + devURL + " ws://" + devHost + ";",
		} {
			if !strings.Contains(csp, want) {
				t.Errorf("GET %s: CSP %q lacks %q", p, csp, want)
			}
		}
		if strings.Contains(csp, "unsafe-") {
			t.Errorf("GET %s: CSP %q", p, csp)
		}
		if m := metaCSP.FindStringSubmatch(page); m == nil || m[1] != csp {
			t.Errorf("GET %s: the <meta> CSP differs from the header", p)
		}
	}
	// Each page gets a nonce of its own.
	a := nonceAttr.FindStringSubmatch(get(t, h, http.MethodGet, "/").Body)
	b := nonceAttr.FindStringSubmatch(get(t, h, http.MethodGet, "/").Body)
	if a[1] == b[1] {
		t.Error("two pages share a nonce")
	}
}

func TestDevServerProxiesModules(t *testing.T) {
	h, vite, devURL := newDevHandler(t)
	for _, p := range []string{"/@vite/client", "/src/main.tsx", "/boot-check.js"} {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, testOrigin+p, nil)
		r.Header.Set("Origin", testOrigin)
		r.Header.Set("Referer", testOrigin+"/")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK || rec.Body.String() != "// "+p {
			t.Errorf("GET %s: %d %q", p, rec.Code, rec.Body.String())
		}
		got := vite.last()
		if got.URL.Path != p || got.Host != strings.TrimPrefix(devURL, "http://") || got.Header.Get("Origin") != "" ||
			got.Header.Get("Referer") != "" {
			t.Errorf("GET %s reached the dev server as %s %s, Origin %q, Referer %q", p, got.Host, got.URL.Path,
				got.Header.Get("Origin"), got.Header.Get("Referer"))
		}
	}
	// The app's own paths never go there.
	if resp := get(t, h, http.MethodGet, "/sw.js"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("/sw.js: %d", resp.StatusCode)
	}
	if resp := get(t, h, http.MethodGet, BridgePath); resp.StatusCode != http.StatusOK ||
		!strings.HasPrefix(resp.Body, bridgePrefix) {
		t.Errorf("bridge.js: %d", resp.StatusCode)
	}
	if resp := get(t, h, http.MethodPost, "/src/main.tsx"); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", resp.StatusCode)
	}
}

func TestDevServerDown(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	h, err := New(Options{Bridge: (&testServer{}).bridge, Origin: testOrigin, Dev: true, DevServer: url})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/", "/src/main.tsx"} {
		if resp := get(t, h, http.MethodGet, p); resp.StatusCode != http.StatusBadGateway {
			t.Errorf("GET %s: %d, want 502", p, resp.StatusCode)
		}
	}
}

func TestDevServerOrigin(t *testing.T) {
	bridge := (&testServer{}).bridge
	for _, origin := range []string{"https://localhost:5173", "http://example.com:5173", "http://192.168.1.2:5173",
		"http://localhost:5173/path", "localhost:5173", "ws://localhost:5173"} {
		if _, err := New(Options{Bridge: bridge, Origin: testOrigin, Dev: true, DevServer: origin}); err == nil {
			t.Errorf("New accepted the dev server %q", origin)
		}
	}
	for _, origin := range []string{"http://localhost:5173", "http://127.0.0.1:5173", "http://[::1]:5173"} {
		if _, err := New(Options{Bridge: bridge, Origin: testOrigin, Dev: true, DevServer: origin}); err != nil {
			t.Errorf("New(DevServer %q) = %v", origin, err)
		}
	}
}

func TestAddNonce(t *testing.T) {
	in := `<SCRIPT>a</SCRIPT><script src=x></script><script type="module" SRC = "/y"></script>` +
		`<script data-src="z">b</script><style>s</style><scripts><stylesheet></script><styl`
	want := `<SCRIPT nonce="n">a</SCRIPT><script src=x></script><script type="module" SRC = "/y"></script>` +
		`<script nonce="n" data-src="z">b</script><style nonce="n">s</style><scripts><stylesheet></script><styl`
	if got := string(addNonce([]byte(in), "n")); got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}
}
