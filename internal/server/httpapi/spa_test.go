package httpapi

import (
	"io/fs"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

var etagPattern = regexp.MustCompile(`^"[0-9a-f]{16}"$`)

// TestSPATable walks the serving rules of 04 §9.5.
func TestSPATable(t *testing.T) {
	f := newFixture(t, func(o *RouterOptions) {
		o.SPAStatus = func(path string) int {
			if path == "/setup" {
				return http.StatusNotFound // 03's hook once an admin exists
			}
			return http.StatusOK
		}
	})
	tests := []struct {
		method, path string
		status       int
		cache        string
		contentType  string
		body         string // "" = don't check
	}{
		// Hashed assets: immutable.
		{"GET", "/" + testJS, 200, cacheImmutable, "text/javascript; charset=utf-8", "console.log('app')\n"},
		{"GET", "/assets/index-77aa01.css", 200, cacheImmutable, "text/css; charset=utf-8", "body{margin:0}\n"},
		{"GET", "/assets/nested/deep-9a0b.js", 200, cacheImmutable, "text/javascript; charset=utf-8", "deep\n"},
		{"HEAD", "/" + testJS, 200, cacheImmutable, "text/javascript; charset=utf-8", ""},
		// Missing assets: 404, never index.html.
		{"GET", "/assets/index-00000000.js", 404, cacheNoStore, "text/plain; charset=utf-8", "404 not found\n"},
		{"GET", "/assets/no-dot-in-name", 404, cacheNoStore, "text/plain; charset=utf-8", ""},
		{"GET", "/assets/", 404, cacheNoStore, "text/plain; charset=utf-8", ""},
		// Precompressed siblings are variants, not files.
		{"GET", "/" + testJS + ".br", 404, cacheNoStore, "text/plain; charset=utf-8", ""},
		{"GET", "/index.html.gz", 404, cacheNoStore, "text/plain; charset=utf-8", ""},
		// Root files: no-cache (revalidate with the ETag).
		{"GET", "/sw.js", 200, cacheNoCache, "text/javascript; charset=utf-8", "self.addEventListener('fetch', () => {})\n"},
		{"GET", "/manifest.webmanifest", 200, cacheNoCache, "application/manifest+json", `{"name":"isshoni"}`},
		{"GET", "/index.html", 200, cacheNoCache, "text/html; charset=utf-8", indexHTML},
		{"GET", "/boot-check.js", 200, cacheNoCache, "text/javascript; charset=utf-8", "/* boot check */\n"},
		{"GET", "/version.json", 200, cacheNoCache, "application/json", ""},
		{"GET", "/licenses.txt", 200, cacheNoCache, "text/plain; charset=utf-8", "licenses\n"},
		{"GET", "/robots.txt", 200, cacheNoCache, "text/plain; charset=utf-8", robotsTxt}, // built in
		// Icons: a day.
		{"GET", "/icons/icon-192.png", 200, cacheIcons, "image/png", "\x89PNG"},
		{"GET", "/icons/icon.svg", 200, cacheIcons, "image/svg+xml", ""},
		{"GET", "/icons/missing.png", 404, cacheNoStore, "text/plain; charset=utf-8", ""},
		// A dotted last segment that doesn't exist: 404.
		{"GET", "/favicon.ico", 404, cacheNoStore, "text/plain; charset=utf-8", ""},
		{"GET", "/r/lounge/clip.mp4", 404, cacheNoStore, "text/plain; charset=utf-8", ""},
		{"GET", "/.gitkeep", 404, cacheNoStore, "text/plain; charset=utf-8", ""},
		{"GET", "/.hidden/secret.txt", 404, cacheNoStore, "text/plain; charset=utf-8", ""},
		// Every other GET/HEAD: index.html with SPAStatus.
		{"GET", "/", 200, cacheNoCache, "text/html; charset=utf-8", indexHTML},
		{"GET", "/r/lounge", 200, cacheNoCache, "text/html; charset=utf-8", indexHTML},
		{"GET", "/admin/users", 200, cacheNoCache, "text/html; charset=utf-8", indexHTML},
		{"GET", "/r.lounge/x", 200, cacheNoCache, "text/html; charset=utf-8", indexHTML}, // only the last segment counts
		{"GET", "/assets", 200, cacheNoCache, "text/html; charset=utf-8", indexHTML},
		{"HEAD", "/r/lounge", 200, cacheNoCache, "text/html; charset=utf-8", ""},
		{"GET", "/setup", 404, cacheNoCache, "text/html; charset=utf-8", indexHTML},
		{"HEAD", "/setup", 404, cacheNoCache, "text/html; charset=utf-8", ""},
		// Other methods on SPA paths: 405.
		{"POST", "/", 405, cacheNoStore, "text/plain; charset=utf-8", "405 method not allowed\n"},
		{"PUT", "/r/lounge", 405, cacheNoStore, "text/plain; charset=utf-8", ""},
		{"DELETE", "/" + testJS, 405, cacheNoStore, "text/plain; charset=utf-8", ""},
		{"OPTIONS", "/", 405, cacheNoStore, "text/plain; charset=utf-8", ""},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			rec := f.serve(newReq(tt.method, tt.path))
			h := rec.Header()
			if rec.Code != tt.status || h.Get("Cache-Control") != tt.cache || h.Get("Content-Type") != tt.contentType {
				t.Fatalf("got %d, Cache-Control %q, Content-Type %q; want %d, %q, %q",
					rec.Code, h.Get("Cache-Control"), h.Get("Content-Type"), tt.status, tt.cache, tt.contentType)
			}
			if tt.body != "" && rec.Body.String() != tt.body {
				t.Fatalf("body %q, want %q", rec.Body, tt.body)
			}
			if tt.method == http.MethodHead && rec.Body.Len() != 0 {
				t.Fatalf("HEAD sent a body: %q", rec.Body)
			}
			if tt.status == http.StatusMethodNotAllowed && h.Get("Allow") != "GET, HEAD" {
				t.Fatalf("Allow = %q", h.Get("Allow"))
			}
			if tt.status == http.StatusOK && !etagPattern.MatchString(h.Get("ETag")) {
				t.Fatalf("ETag = %q, want 16 quoted hex characters", h.Get("ETag"))
			}
			if tt.status != http.StatusOK && h.Get("ETag") != "" {
				t.Fatalf("a %d carries ETag %q", tt.status, h.Get("ETag"))
			}
		})
	}
}

// TestSPAHeadMatchesGet: HEAD sends the same headers as GET, without the body.
func TestSPAHeadMatchesGet(t *testing.T) {
	f := newFixture(t, nil)
	for _, path := range []string{"/" + testJS, "/r/lounge", "/sw.js"} {
		get, head := f.get(path), f.serve(newReq(http.MethodHead, path))
		for _, k := range []string{"Content-Type", "Content-Length", "Cache-Control", "ETag", "Vary", "Content-Security-Policy"} {
			if get.Header().Get(k) != head.Header().Get(k) {
				t.Errorf("%s %s: GET %q, HEAD %q", path, k, get.Header().Get(k), head.Header().Get(k))
			}
		}
	}
}

func TestSPAETag(t *testing.T) {
	f := newFixture(t, nil)
	for _, path := range []string{"/" + testJS, "/sw.js", "/r/lounge", "/", "/icons/icon-192.png", "/robots.txt"} {
		first := f.get(path)
		etag := first.Header().Get("ETag")
		if !etagPattern.MatchString(etag) {
			t.Fatalf("%s: ETag %q", path, etag)
		}
		if rec := f.get(path, "If-None-Match", etag); rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
			t.Errorf("%s with If-None-Match: %d %q, want 304", path, rec.Code, rec.Body)
		}
		if rec := f.get(path, "If-None-Match", `"0000000000000000"`); rec.Code != http.StatusOK {
			t.Errorf("%s with a stale ETag: %d, want 200", path, rec.Code)
		}
	}
	// The SPA fallback shares index.html's validator.
	if a, b := f.get("/r/a").Header().Get("ETag"), f.get("/index.html").Header().Get("ETag"); a != b {
		t.Errorf("fallback ETag %q, index.html ETag %q", a, b)
	}
	// Different content, different ETag; the ETag is the content's SHA-256 prefix.
	if a, b := f.get("/"+testJS).Header().Get("ETag"), f.get("/sw.js").Header().Get("ETag"); a == b {
		t.Errorf("two files share ETag %q", a)
	}
	if got, want := f.get("/robots.txt").Header().Get("ETag"), etagOf([]byte(robotsTxt)); got != want {
		t.Errorf("robots.txt ETag %q, want %q", got, want)
	}
}

// TestSPAPrecompressed: .br/.gz siblings by Accept-Encoding, with Vary on every response of such a file.
func TestSPAPrecompressed(t *testing.T) {
	f := newFixture(t, nil)
	tests := []struct {
		path, accept string
		encoding     string
		body         string
	}{
		{"/" + testJS, "gzip, deflate, br, zstd", "br", "BR:js"},
		{"/" + testJS, "br", "br", "BR:js"},
		{"/" + testJS, "BR;q=1.0", "br", "BR:js"},
		{"/" + testJS, "gzip", "gzip", "GZ:js"},
		{"/" + testJS, "x-gzip", "gzip", "GZ:js"},
		{"/" + testJS, "br;q=0, gzip", "gzip", "GZ:js"},
		{"/" + testJS, "br;q=0.5, gzip;q=0.8", "gzip", "GZ:js"},
		{"/" + testJS, "br;q=0.8, gzip;q=0.8", "br", "BR:js"},
		{"/" + testJS, "*", "br", "BR:js"},
		{"/" + testJS, "*;q=0, gzip", "gzip", "GZ:js"},
		{"/" + testJS, "br;q=0, gzip;q=0", "", "console.log('app')\n"},
		{"/" + testJS, "deflate, identity", "", "console.log('app')\n"},
		{"/" + testJS, "br;q=bogus", "", "console.log('app')\n"},
		{"/" + testJS, "br;q=NaN, gzip", "gzip", "GZ:js"},
		{"/" + testJS, "", "", "console.log('app')\n"},
		{"/r/lounge", "br", "br", "BR:index"},
		{"/", "gzip", "gzip", "GZ:index"},
		{"/assets/only-gz-7081.js", "br, gzip", "gzip", "GZ:only"},
		{"/assets/only-gz-7081.js", "br", "", "console.log('gz')\n"},
	}
	for _, tt := range tests {
		rec := f.get(tt.path, "Accept-Encoding", tt.accept)
		h := rec.Header()
		if rec.Code != 200 || h.Get("Content-Encoding") != tt.encoding || rec.Body.String() != tt.body {
			t.Errorf("%s with %q: %d, Content-Encoding %q, body %q; want %q, %q", tt.path, tt.accept, rec.Code,
				h.Get("Content-Encoding"), rec.Body, tt.encoding, tt.body)
		}
		if h.Get("Vary") != "Accept-Encoding" {
			t.Errorf("%s with %q: Vary %q", tt.path, tt.accept, h.Get("Vary"))
		}
		if !strings.HasPrefix(h.Get("Content-Type"), "text/") {
			t.Errorf("%s: Content-Type %q is not the original's", tt.path, h.Get("Content-Type"))
		}
	}
	// Each variant has its own validator.
	br := f.get("/"+testJS, "Accept-Encoding", "br").Header().Get("ETag")
	gz := f.get("/"+testJS, "Accept-Encoding", "gzip").Header().Get("ETag")
	id := f.get("/" + testJS).Header().Get("ETag")
	if br == gz || br == id || gz == id {
		t.Errorf("variants share an ETag: br %s gzip %s identity %s", br, gz, id)
	}
	if rec := f.get("/"+testJS, "Accept-Encoding", "br", "If-None-Match", br); rec.Code != http.StatusNotModified {
		t.Errorf("br variant revalidation: %d", rec.Code)
	}
	// A file without siblings: no Vary, no encoding. A .gz without an original is a file of its own.
	if rec := f.get("/sw.js", "Accept-Encoding", "br, gzip"); rec.Header().Get("Vary") != "" || rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("sw.js: %v", rec.Header())
	}
	if rec := f.get("/assets/archive-8192.tar.gz", "Accept-Encoding", "gzip"); rec.Code != 200 ||
		rec.Header().Get("Content-Encoding") != "" || rec.Header().Get("Content-Type") != defaultMIMEType {
		t.Errorf("a standalone .gz file: %d %v", rec.Code, rec.Header())
	}
	// /setup as a 404 still negotiates.
	g := newFixture(t, func(o *RouterOptions) { o.SPAStatus = func(string) int { return 404 } })
	if rec := g.get("/setup", "Accept-Encoding", "br"); rec.Code != 404 || rec.Body.String() != "BR:index" ||
		rec.Header().Get("Content-Length") != "8" {
		t.Errorf("/setup 404 with br: %d %v %q", rec.Code, rec.Header(), rec.Body)
	}
}

// TestMIMETypes checks the explicit table (04 §9.5): the answers don't depend on /etc/mime.types.
func TestMIMETypes(t *testing.T) {
	want := map[string]string{
		"a.js":           "text/javascript; charset=utf-8",
		"a.mjs":          "text/javascript; charset=utf-8",
		"a.css":          "text/css; charset=utf-8",
		"a.html":         "text/html; charset=utf-8",
		"a.json":         "application/json",
		"a.webmanifest":  "application/manifest+json",
		"a.svg":          "image/svg+xml",
		"a.png":          "image/png",
		"A.PNG":          "image/png",
		"a.ico":          "image/x-icon",
		"a.webp":         "image/webp",
		"a.woff2":        "font/woff2",
		"a.wasm":         "application/wasm",
		"a.txt":          "text/plain; charset=utf-8",
		"a.bin":          defaultMIMEType,
		"a.tar.gz":       defaultMIMEType,
		"noext":          defaultMIMEType,
		"dir.js/file":    defaultMIMEType,
		"assets/x.js.br": defaultMIMEType,
	}
	for name, ct := range want {
		if got := contentTypeOf(name); got != ct {
			t.Errorf("contentTypeOf(%q) = %q, want %q", name, got, ct)
		}
	}
	// Served files carry the same types.
	f := newFixture(t, nil)
	served := map[string]string{
		"/assets/worker-1a2b.mjs":      "text/javascript; charset=utf-8",
		"/assets/inter-2b3c.woff2":     "font/woff2",
		"/assets/decoder-3c4d.wasm":    "application/wasm",
		"/assets/logo-4d5e.svg":        "image/svg+xml",
		"/assets/data-5e6f.json":       "application/json",
		"/assets/blob-6f70.bin":        defaultMIMEType,
		"/icons/favicon.ico":           "image/x-icon",
		"/icons/maskable-512.webp":     "image/webp",
		"/icons/Upper-Case.PNG":        "image/png",
		"/icons/apple-touch-icon.png":  "image/png",
		"/assets/nested/deep-9a0b.css": "text/css; charset=utf-8",
	}
	for path, ct := range served {
		if rec := f.get(path); rec.Code != 200 || rec.Header().Get("Content-Type") != ct {
			t.Errorf("%s: %d %q, want %q", path, rec.Code, rec.Header().Get("Content-Type"), ct)
		}
	}
}

func TestSPARobotsShipped(t *testing.T) {
	fsys := testFS()
	fsys["robots.txt"] = &fstest.MapFile{Data: []byte("User-agent: *\nAllow: /\n")}
	f := newFixture(t, func(o *RouterOptions) { o.SPA = fsys })
	if rec := f.get("/robots.txt"); rec.Body.String() != "User-agent: *\nAllow: /\n" {
		t.Fatalf("the SPA's robots.txt was not served: %q", rec.Body)
	}
}

// TestSPANotBuilt: with only dist/.gitkeep (or no FS), SPA routes show the 503 page; files and 404s behave as usual.
func TestSPANotBuilt(t *testing.T) {
	for name, spaFS := range map[string]fs.FS{
		"gitkeep only": fstest.MapFS{".gitkeep": {}},
		"nil FS":       nil,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, func(o *RouterOptions) { o.SPA = spaFS })
			for _, path := range []string{"/", "/r/lounge", "/setup"} {
				rec := f.get(path)
				h := rec.Header()
				if rec.Code != http.StatusServiceUnavailable || h.Get("Content-Type") != "text/html; charset=utf-8" ||
					h.Get("Cache-Control") != cacheNoStore || h.Get("Content-Security-Policy") != wantHTMLCSP {
					t.Fatalf("%s: %d %v", path, rec.Code, h)
				}
				if body := rec.Body.String(); !strings.Contains(body, "Web UI not built") || !strings.Contains(body, "task build:web") {
					t.Fatalf("%s: body %q", path, body)
				}
			}
			if rec := f.serve(newReq(http.MethodHead, "/")); rec.Code != 503 || rec.Body.Len() != 0 {
				t.Fatalf("HEAD /: %d %q", rec.Code, rec.Body)
			}
			if rec := f.get("/assets/index-1.js"); rec.Code != 404 {
				t.Fatalf("missing asset: %d", rec.Code)
			}
			if rec := f.get("/robots.txt"); rec.Code != 200 || rec.Body.String() != robotsTxt {
				t.Fatalf("robots.txt: %d %q", rec.Code, rec.Body)
			}
			wantError(t, f.get("/api/v1/info"), 404, api.CodeNotFound)
			// A dev build (go test) logs no warning; the check is TestSPAStartupWarnings.
			for _, l := range f.logs.records(t) {
				if l["level"] == "WARN" && strings.Contains(l["msg"].(string), "no web UI") {
					t.Errorf("dev build warned: %v", l)
				}
			}
		})
	}
}

func TestSPAStartupWarnings(t *testing.T) {
	warnings := func(t *testing.T, spaFS fs.FS) []string {
		t.Helper()
		logs := &syncBuffer{}
		NewRouter(RouterOptions{SPA: spaFS, Logger: slog.New(slog.NewJSONHandler(logs, nil))})
		var out []string
		for _, l := range logs.records(t) {
			if l["level"] == "WARN" {
				out = append(out, l["msg"].(string))
			}
		}
		return out
	}
	t.Run("matching build: quiet", func(t *testing.T) {
		if w := warnings(t, testFS()); len(w) != 0 {
			t.Fatalf("warnings %v", w)
		}
	})
	t.Run("version mismatch", func(t *testing.T) {
		fsys := testFS()
		fsys["version.json"] = &fstest.MapFile{Data: []byte(`{"version":"9.9.9"}`)}
		if w := warnings(t, fsys); len(w) != 1 || !strings.Contains(w[0], "another version") {
			t.Fatalf("warnings %v", w)
		}
	})
	t.Run("no web UI in a release build", func(t *testing.T) {
		old := isDevBuild
		isDevBuild = func() bool { return false }
		defer func() { isDevBuild = old }()
		if w := warnings(t, fstest.MapFS{".gitkeep": {}}); len(w) != 1 || !strings.Contains(w[0], "no web UI") {
			t.Fatalf("warnings %v", w)
		}
		if w := warnings(t, testFS()); len(w) != 0 {
			t.Fatalf("a built release warned: %v", w)
		}
	})
}

func TestPickEncoding(t *testing.T) {
	tests := []struct {
		header       string
		haveBr, hvGz bool
		want         string
	}{
		{"br, gzip", true, true, "br"},
		{"br, gzip", false, true, "gzip"},
		{"br, gzip", true, false, "br"},
		{"br, gzip", false, false, ""},
		{"gzip", true, false, ""},
		{"gzip;q=1, br;q=0.9", true, true, "gzip"},
		{" gzip ; q = 0.5 , br ; q = 0.4 ", true, true, "gzip"},
		{"*;q=0.1", true, true, "br"},
		{"br;q=2", true, true, ""},
		{"br;q=-1, gzip", true, true, "gzip"},
		{",,,;;;==", true, true, ""},
	}
	for _, tt := range tests {
		if got := pickEncoding(tt.header, tt.haveBr, tt.hvGz); got != tt.want {
			t.Errorf("pickEncoding(%q, %v, %v) = %q, want %q", tt.header, tt.haveBr, tt.hvGz, got, tt.want)
		}
	}
}

// TestSPAStatusHookOutOfRange: a hook value WriteHeader would reject falls back to 200.
func TestSPAStatusHookOutOfRange(t *testing.T) {
	for _, bad := range []int{0, 99, 101, 600} {
		f := newFixture(t, func(o *RouterOptions) { o.SPAStatus = func(string) int { return bad } })
		if rec := f.get("/r/lounge"); rec.Code != http.StatusOK || rec.Body.String() != indexHTML {
			t.Errorf("hook %d: status %d", bad, rec.Code)
		}
	}
}
