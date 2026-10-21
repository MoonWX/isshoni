// Package assets is the desktop app's asset handler (docs/m2/03-desktop-app.md §5, "03" below): the one
// http.Handler that serves everything the webview loads, and the /wails/* guard in front of the Wails runtime.
//
//   - New serves web.Dist() by 03 §5.1's table: index.html for every route with one <script
//     src="/desktop/bridge.js"> injected before boot-check.js, /desktop/bridge.js with the page's synchronous
//     config (BridgeConfig), the strict CSP of §5.3 as a header and as a <meta>, no service worker and no manifest.
//   - Guard is Wails' AssetOptions.Middleware: requests under /wails/ pass only from the asset origin, and of the
//     runtime's own objects only the bound-method calls (Call, CancelCall) get through (§5.6, W21's allowlist in
//     runtime_allow.go).
//
// The package imports no Wails code (03 D2): it builds and tests on every OS, and cmd/isshoni-desktop hands its
// handler and middleware to Wails. It never proxies to an isshoni server and never reads tokens. Only dev builds,
// with Options.DevServer, take the SPA from Vite's dev server instead of Dist (§5.3; dev.go).
package assets

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

// Options configures New.
type Options struct {
	Dist   fs.FS               // web.Dist(); unused (and may be empty) when DevServer is set
	Bridge func() BridgeConfig // read on every bridge.js and index.html request
	Origin string              // the asset origin: 02's protocol.DesktopAssetOriginWindows on Windows
	// Dev is true only in dev and e2e builds (-tags dev or e2e), never in a release: an http:// loopback server
	// origin then gets http:// and ws:// in connect-src (§5.3).
	Dev bool
	// DevServer is the Vite dev server's origin (dev builds only, from ISSHONI_DESKTOP_DEV_URL; `task
	// desktop:dev`): the SPA and its modules come from there, with live reload, instead of from Dist. It must be an
	// http:// loopback origin and needs Dev.
	DevServer string
}

// BridgeConfig is the page's synchronous config: bridge.js sets window.__ISSHONI_DESKTOP__ to it (§5.2). It holds no
// token and no user data beyond the active server's public name.
type BridgeConfig struct {
	V         int          `json:"v"`         // 1
	Transport string       `json:"transport"` // "wails"; e2ehost: "http"
	App       AppFacts     `json:"app"`
	Server    *ServerFacts `json:"server"`    // the active server, null before the first server is added
	ServerIDs []string     `json:"serverIds"` // the derived id of every server in servers.json, for storage cleanup (§14.3)
	Dev       bool         `json:"dev"`
}

// AppFacts describes the running app.
type AppFacts struct {
	Version string `json:"version"` // internal/version
	OS      string `json:"os"`      // protocol.ClientOS: "windows" (M2), "macos" (dev, M3)
	OSBuild int    `json:"osBuild"` // e.g. 26100
	Arch    string `json:"arch"`    // "amd64"
}

// ServerFacts describes the active server.
type ServerFacts struct {
	ID     string `json:"id"`     // derived local id (§8.1), never stored
	Origin string `json:"origin"` // "https://watch.example.com"
	Name   string `json:"name"`   // /info server.name, from 02's servers.json entry
}

// BridgeConfigVersion is BridgeConfig.V.
const BridgeConfigVersion = 1

// The paths the handler treats specially.
const (
	// BridgePath serves bridge.js.
	BridgePath = "/desktop/bridge.js"
	// bootCheckTag is the injection point New looks for in index.html: the classic script that runs before the
	// module entry (05 §4, §16.1).
	bootCheckTag = `<script src="/boot-check.js"></script>`
	// bridgeTag is inserted right before bootCheckTag.
	bridgeTag = `<script src="` + BridgePath + `"></script>`
	// wailsPrefix is the Wails runtime's path space; Wails answers what it knows before this handler runs.
	wailsPrefix = "/wails/"
)

// Cache-Control values (§5.1).
const (
	cacheNoStore   = "no-store"
	cacheNoCache   = "no-cache"
	cacheImmutable = "public, max-age=31536000, immutable"
)

// notServed are files of dist/ the app never serves: no PWA in the app (§5.1).
var notServed = map[string]bool{
	"sw.js":                true,
	"manifest.webmanifest": true,
}

// ErrNoInjectionPoint is New's error when index.html is missing or doesn't hold exactly one boot-check.js tag.
var ErrNoInjectionPoint = errors.New(`assets: index.html must hold exactly one <script src="/boot-check.js"></script>`)

// file is one servable file of Dist.
type file struct {
	name        string // path in Dist, e.g. "assets/index-3f2a1b.js"
	contentType string
	etag        string
	cache       string
}

// handler serves Dist (or the dev server) by §5.1's table.
type handler struct {
	opts  Options
	files map[string]*file
	// index is index.html with the bridge tag inserted, split where the CSP <meta> goes (per request, §5.3).
	indexHead, indexTail []byte
	dev                  *devServer // non-nil only with Options.DevServer
}

// New checks that Dist holds an index.html with exactly one `<script src="/boot-check.js">` tag (the injection
// point) and returns the handler for Wails' AssetOptions.Handler. With Options.DevServer (dev builds) it checks the
// dev server's origin instead and leaves Dist alone.
func New(o Options) (http.Handler, error) {
	if o.Bridge == nil {
		return nil, errors.New("assets: Options.Bridge is nil")
	}
	if err := checkAssetOrigin(o.Origin); err != nil {
		return nil, err
	}
	h := &handler{opts: o, files: map[string]*file{}}
	if o.DevServer != "" {
		if !o.Dev {
			return nil, errors.New("assets: Options.DevServer needs a dev build (Options.Dev)")
		}
		d, err := newDevServer(o.DevServer)
		if err != nil {
			return nil, err
		}
		h.dev = d
		return h, nil
	}
	if o.Dist == nil {
		return nil, errors.New("assets: Options.Dist is nil")
	}
	if err := h.load(); err != nil {
		return nil, err
	}
	index, err := fs.ReadFile(o.Dist, "index.html")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoInjectionPoint, err)
	}
	head, tail, err := rewriteIndex(index)
	if err != nil {
		return nil, err
	}
	h.indexHead, h.indexTail = head, tail
	delete(h.files, "index.html") // always served through the rewrite
	return h, nil
}

// rewriteIndex inserts the bridge tag before the boot-check tag and splits the page where the CSP <meta> goes: after
// the <head> tag and the <meta charset> that may follow it, so the charset declaration stays first.
func rewriteIndex(index []byte) (head, tail []byte, err error) {
	if bytes.Count(index, []byte(bootCheckTag)) != 1 {
		return nil, nil, ErrNoInjectionPoint
	}
	page := bytes.Replace(index, []byte(bootCheckTag), []byte(bridgeTag+bootCheckTag), 1)
	at := metaInsertionPoint(page)
	if at < 0 {
		return nil, nil, errors.New("assets: index.html has no <head> tag")
	}
	return page[:at:at], page[at:], nil
}

// metaInsertionPoint returns the offset right after the <head> tag, or after a <meta charset> that directly follows
// it, or -1 without a <head> tag.
func metaInsertionPoint(page []byte) int {
	lower := bytes.ToLower(page)
	i := 0
	for {
		j := bytes.Index(lower[i:], []byte("<head"))
		if j < 0 {
			return -1
		}
		i += j
		next := i + len("<head")
		if next < len(lower) && (lower[next] == '>' || isHTMLSpace(lower[next])) {
			break
		}
		i = next // <header>, <headline>, …
	}
	end := bytes.IndexByte(lower[i:], '>')
	if end < 0 {
		return -1
	}
	at := i + end + 1
	rest := bytes.TrimLeft(lower[at:], " \t\r\n\f")
	if bytes.HasPrefix(rest, []byte("<meta charset")) {
		skipped := len(lower[at:]) - len(rest)
		if e := bytes.IndexByte(rest, '>'); e >= 0 {
			return at + skipped + e + 1
		}
	}
	return at
}

func isHTMLSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' }

// load indexes Dist once: every regular file except dotfiles, the precompressed .br/.gz siblings of another file
// and the files of notServed.
func (h *handler) load() error {
	names := map[string]bool{}
	err := fs.WalkDir(h.opts.Dist, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name != "." && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			names[name] = true
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("assets: reading the SPA: %w", err)
	}
	for name := range names {
		if orig, ok := strings.CutSuffix(name, ".br"); ok && names[orig] {
			continue
		}
		if orig, ok := strings.CutSuffix(name, ".gz"); ok && names[orig] {
			continue
		}
		if notServed[name] {
			continue
		}
		etag, err := etagOf(h.opts.Dist, name)
		if err != nil {
			return fmt.Errorf("assets: reading %s: %w", name, err)
		}
		cache := cacheNoCache
		if strings.HasPrefix(name, "assets/") {
			cache = cacheImmutable
		}
		h.files[name] = &file{name: name, contentType: contentTypeOf(name), etag: etag, cache: cache}
	}
	return nil
}

func etagOf(fsys fs.FS, name string) (string, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return `"` + hex.EncodeToString(sum.Sum(nil)[:8]) + `"`, nil
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if strings.HasPrefix(p, wailsPrefix) || p == "/wails" {
		// Wails answers the runtime paths it serves before this handler; what reaches it here is unknown to Wails.
		plainError(w, r, http.StatusNotFound)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		plainError(w, r, http.StatusMethodNotAllowed)
		return
	}
	if p == BridgePath {
		h.serveBridge(w, r)
		return
	}
	name := strings.TrimPrefix(p, "/")
	if notServed[name] {
		plainError(w, r, http.StatusNotFound)
		return
	}
	if h.dev != nil {
		h.serveDev(w, r, name)
		return
	}
	if name == "index.html" {
		h.serveIndex(w, r, h.indexHead, h.indexTail, "")
		return
	}
	if f := h.files[name]; f != nil {
		h.serveFile(w, r, f)
		return
	}
	if strings.HasPrefix(p, "/assets/") || lastSegmentHasDot(name) {
		plainError(w, r, http.StatusNotFound)
		return
	}
	h.serveIndex(w, r, h.indexHead, h.indexTail, "")
}

// config reads the bridge config once per request, with the fixed fields filled in.
func (h *handler) config() BridgeConfig {
	c := h.opts.Bridge()
	c.V = BridgeConfigVersion
	if c.ServerIDs == nil {
		c.ServerIDs = []string{}
	}
	if c.Transport == "" {
		c.Transport = "wails"
	}
	return c
}

func (h *handler) serveBridge(w http.ResponseWriter, r *http.Request) {
	body, err := BridgeJS(h.config())
	if err != nil {
		slog.Default().Error("desktop: bridge.js", "component", "assets", "err", err)
		plainError(w, r, http.StatusInternalServerError)
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", "text/javascript; charset=utf-8")
	hd.Set("Cache-Control", cacheNoStore)
	hd.Set("X-Content-Type-Options", "nosniff")
	writeBody(w, r, http.StatusOK, body)
}

// serveIndex sends index.html with the CSP <meta> inserted between head and tail, and the HTML headers of §5.3.
// nonce is "" except for the dev server's pages (dev.go).
func (h *handler) serveIndex(w http.ResponseWriter, r *http.Request, head, tail []byte, nonce string) {
	csp := h.policy(h.config(), nonce)
	var b bytes.Buffer
	b.Grow(len(head) + len(tail) + len(csp) + 80)
	b.Write(head)
	b.WriteString(`<meta http-equiv="Content-Security-Policy" content="`)
	b.WriteString(csp) // the policy holds no '"', '<', '>' or '&' (policy.go)
	b.WriteString(`">`)
	b.Write(tail)
	hd := w.Header()
	hd.Set("Content-Type", "text/html; charset=utf-8")
	hd.Set("Cache-Control", cacheNoStore)
	setHTMLHeaders(hd, csp)
	writeBody(w, r, http.StatusOK, b.Bytes())
}

func (h *handler) serveFile(w http.ResponseWriter, r *http.Request, f *file) {
	src, err := h.opts.Dist.Open(f.name)
	if err != nil {
		plainError(w, r, http.StatusNotFound)
		return
	}
	defer func() { _ = src.Close() }()
	rs, ok := src.(io.ReadSeeker)
	if !ok {
		b, err := io.ReadAll(src)
		if err != nil {
			plainError(w, r, http.StatusInternalServerError)
			return
		}
		rs = bytes.NewReader(b)
	}
	hd := w.Header()
	hd.Set("Content-Type", f.contentType)
	hd.Set("Cache-Control", f.cache)
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("ETag", f.etag)
	if strings.HasPrefix(f.contentType, "text/html") {
		// No other page is planned; one that ships anyway gets the header policy (it has no <meta>).
		setHTMLHeaders(hd, h.policy(h.config(), ""))
	}
	http.ServeContent(w, r, "", time.Time{}, rs)
}

// writeBody sends a complete in-memory body (no body for HEAD).
func writeBody(w http.ResponseWriter, r *http.Request, status int, body []byte) {
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body) // a failed write means the page is gone
	}
}

// plainError writes a short plain-text error.
func plainError(w http.ResponseWriter, r *http.Request, status int) {
	hd := w.Header()
	hd.Set("Content-Type", "text/plain; charset=utf-8")
	hd.Set("Cache-Control", cacheNoStore)
	hd.Set("X-Content-Type-Options", "nosniff")
	writeBody(w, r, status, []byte(strconv.Itoa(status)+" "+strings.ToLower(http.StatusText(status))+"\n"))
}

func lastSegmentHasDot(name string) bool {
	return strings.Contains(name[strings.LastIndexByte(name, '/')+1:], ".")
}

// mimeTypes maps the built SPA's file extensions to their Content-Type, like the server's table (M1 04 §9.5): the
// system table is never consulted, so every OS answers the same.
var mimeTypes = map[string]string{
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".html":        "text/html; charset=utf-8",
	".json":        "application/json",
	".webmanifest": "application/manifest+json",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".ico":         "image/x-icon",
	".webp":        "image/webp",
	".woff2":       "font/woff2",
	".wasm":        "application/wasm",
	".txt":         "text/plain; charset=utf-8",
}

// contentTypeOf returns the Content-Type for a file name by its extension (case-insensitive). With nosniff, the
// webview neither runs nor renders a file of an unknown type.
func contentTypeOf(name string) string {
	if t, ok := mimeTypes[strings.ToLower(path.Ext(name))]; ok {
		return t
	}
	return "application/octet-stream"
}
