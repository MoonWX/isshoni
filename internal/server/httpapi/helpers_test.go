package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/version"
)

const (
	testHost   = "watch.example.com"
	testOrigin = "https://" + testHost
	testJS     = "assets/index-3f2a1b9c.js"
	indexHTML  = "<!doctype html><html><head><title>isshoni</title></head><body><div id=root></div></body></html>\n"
)

// domainSite is an auto-mode site on a domain.
func domainSite() Site {
	return Site{Origin: testOrigin, Host: testHost, Hostname: testHost, TLSMode: "auto"}
}

// devSite is an off-mode dev site on localhost (any loopback Host passes the Host check).
func devSite() Site {
	return Site{Origin: "http://localhost:8080", Host: "localhost:8080", Hostname: "localhost", TLSMode: "off", Dev: true}
}

// testFS is a built SPA as 05 §17.1 lays it out, with precompressed siblings for index.html and the entry chunk.
func testFS() fstest.MapFS {
	file := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	return fstest.MapFS{
		".gitkeep":                    file(""),
		".hidden/secret.txt":          file("hidden"),
		"index.html":                  file(indexHTML),
		"index.html.br":               file("BR:index"),
		"index.html.gz":               file("GZ:index"),
		testJS:                        file("console.log('app')\n"),
		testJS + ".br":                file("BR:js"),
		testJS + ".gz":                file("GZ:js"),
		"assets/index-77aa01.css":     file("body{margin:0}\n"),
		"assets/worker-1a2b.mjs":      file("export {}\n"),
		"assets/inter-2b3c.woff2":     file("wOF2"),
		"assets/decoder-3c4d.wasm":    file("\x00asm"),
		"assets/logo-4d5e.svg":        file("<svg xmlns='http://www.w3.org/2000/svg'/>"),
		"assets/data-5e6f.json":       file("{}"),
		"assets/blob-6f70.bin":        file("\x01\x02"),
		"assets/only-gz-7081.js":      file("console.log('gz')\n"),
		"assets/only-gz-7081.js.gz":   file("GZ:only"),
		"assets/archive-8192.tar.gz":  file("a tarball, not a sibling"),
		"sw.js":                       file("self.addEventListener('fetch', () => {})\n"),
		"manifest.webmanifest":        file(`{"name":"isshoni"}`),
		"boot-check.js":               file("/* boot check */\n"),
		"version.json":                file(`{"version":"` + version.Version() + `","protocol":1}`),
		"licenses.txt":                file("licenses\n"),
		"icons/icon-192.png":          file("\x89PNG"),
		"icons/icon.svg":              file("<svg xmlns='http://www.w3.org/2000/svg'/>"),
		"icons/favicon.ico":           file("\x00\x00\x01\x00"),
		"icons/maskable-512.webp":     file("RIFF"),
		"icons/apple-touch-icon.png":  file("\x89PNG"),
		"icons/Upper-Case.PNG":        file("\x89PNG"),
		"assets/nested/deep-9a0b.js":  file("deep\n"),
		"assets/nested/deep-9a0b.css": file("deep{}\n"),
	}
}

// syncBuffer collects log output from handlers running on server goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// records returns the JSON log lines.
func (s *syncBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(strings.NewReader(s.String()))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("log line %q: %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	return out
}

// captureDefaultLog routes slog.Default, which WriteError logs to outside the router, into a buffer for one test.
func captureDefaultLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return buf
}

type observation struct {
	pattern string
	status  int
	bytes   int64
}

type recObserver struct {
	mu  sync.Mutex
	obs []observation
}

func (o *recObserver) ObserveRoute(pattern string, status int, bytes int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.obs = append(o.obs, observation{pattern, status, bytes})
}

func (o *recObserver) all() []observation {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]observation(nil), o.obs...)
}

// fixture is a router over testFS with a recording observer, a gate and a JSON log buffer at debug level.
type fixture struct {
	rt   *Router
	obs  *recObserver
	gate *Gate
	logs *syncBuffer
}

func newFixture(t *testing.T, mod func(*RouterOptions)) *fixture {
	t.Helper()
	f := &fixture{obs: &recObserver{}, gate: &Gate{}, logs: &syncBuffer{}}
	opts := RouterOptions{
		Site:     domainSite(),
		SPA:      testFS(),
		Gate:     f.gate,
		Observer: f.obs,
		Logger:   slog.New(slog.NewJSONHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	if mod != nil {
		mod(&opts)
	}
	f.rt = NewRouter(opts)
	return f
}

// serve runs one request through the whole chain.
func (f *fixture) serve(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.rt.Handler().ServeHTTP(rec, req)
	return rec
}

// get serves GET https://watch.example.com<path> with optional header pairs.
func (f *fixture) get(path string, header ...string) *httptest.ResponseRecorder {
	return f.serve(newReq(http.MethodGet, path, header...))
}

func newReq(method, path string, header ...string) *http.Request {
	req := httptest.NewRequestWithContext(context.Background(), method, testOrigin+path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	return req
}

// decodeEnvelope parses 03's error envelope from a response body.
func decodeEnvelope(t *testing.T, body []byte) api.Error {
	t.Helper()
	var env api.ErrorResponse
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		t.Fatalf("body %q is not an error envelope: %v", body, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		t.Fatalf("body %q has trailing data", body)
	}
	return env.Error
}

// wantError checks a JSON error response: status, envelope code, Content-Type and no-store.
func wantError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) api.Error {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, status, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != jsonContentType {
		t.Errorf("Content-Type = %q, want %q", ct, jsonContentType)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != cacheNoStore {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	e := decodeEnvelope(t, rec.Body.Bytes())
	if e.Code != code {
		t.Fatalf("error code = %q, want %q", e.Code, code)
	}
	return e
}
