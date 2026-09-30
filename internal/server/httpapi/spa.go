package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/version"
)

// isDevBuild reports a dev build; tests replace it to see the non-dev startup warning.
var isDevBuild = version.IsDev

// robotsTxt is served at /robots.txt when the SPA doesn't ship one: friend servers stay out of search engines.
const robotsTxt = "User-agent: *\nDisallow: /\n"

// notBuiltHTML is the page every SPA route shows, with status 503, when the binary was built without web/dist.
const notBuiltHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>isshoni: web UI not built</title>
</head>
<body>
<h1>Web UI not built</h1>
<p>This server binary has no web app. Run <code>task build:web</code>, then build the server again, or use a release artifact.</p>
</body>
</html>
`

// spaFile is one file of the built SPA, with what the handler precomputes at startup (04 §9.5).
type spaFile struct {
	name        string // path in the FS, e.g. "assets/index-3f2a1b.js"
	etag        string // strong ETag: the first 16 hex characters of the content's SHA-256, quoted
	contentType string
	size        int64
	br, gz      *spaFile // precompressed siblings, served with Content-Encoding
}

// spa serves the embedded web app by the table of 04 §9.5.
type spa struct {
	fsys   fs.FS
	files  map[string]*spaFile // by path in the FS; precompressed siblings hang off their original
	index  *spaFile            // nil: the web UI is not built
	status func(path string) int
	sec    secHeaders
	log    *slog.Logger
	robots *spaFile // the built-in robots.txt (content robotsTxt), when the SPA has none
}

// newSPA walks fsys once. Files and directories whose name starts with "." (dist/.gitkeep) are not served.
func newSPA(fsys fs.FS, status func(string) int, sec secHeaders, log *slog.Logger) *spa {
	s := &spa{fsys: fsys, files: map[string]*spaFile{}, status: status, sec: sec, log: log}
	if fsys != nil {
		s.load()
	}
	s.index = s.files["index.html"]
	if s.files["robots.txt"] == nil {
		s.robots = &spaFile{name: "robots.txt", etag: etagOf([]byte(robotsTxt)), contentType: contentTypeOf("robots.txt")}
	}
	if s.index == nil {
		if !isDevBuild() {
			log.Warn("this binary has no web UI (built without web/dist); use a release artifact")
		}
	} else {
		s.checkVersion()
	}
	return s
}

func (s *spa) load() {
	err := fs.WalkDir(s.fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name != "." && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		f, err := s.hashFile(name)
		if err != nil {
			s.log.Warn("web file not served", slog.String("file", name), logx.Err(err))
			return nil
		}
		s.files[name] = f
		return nil
	})
	if err != nil {
		s.log.Warn("reading the web UI files", logx.Err(err))
	}
	// Precompressed siblings: <file>.br and <file>.gz next to an existing <file> are variants of it, not files.
	var siblings []string
	for name, f := range s.files {
		if orig, ok := strings.CutSuffix(name, ".br"); ok && s.files[orig] != nil {
			s.files[orig].br = f
			siblings = append(siblings, name)
		} else if orig, ok := strings.CutSuffix(name, ".gz"); ok && s.files[orig] != nil {
			s.files[orig].gz = f
			siblings = append(siblings, name)
		}
	}
	for _, name := range siblings {
		delete(s.files, name)
	}
}

func (s *spa) hashFile(name string) (*spaFile, error) {
	f, err := s.fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return nil, err
	}
	return &spaFile{name: name, etag: etagFromSum(h.Sum(nil)), contentType: contentTypeOf(name), size: n}, nil
}

// etagFromSum is the strong ETag of a SHA-256 digest: its first 16 hex characters, quoted (04 §9.5).
func etagFromSum(sum []byte) string { return `"` + hex.EncodeToString(sum[:8]) + `"` }

func etagOf(b []byte) string {
	sum := sha256.Sum256(b)
	return etagFromSum(sum[:])
}

// checkVersion compares dist/version.json with this binary's version: 06 builds both halves with one version, so a
// mismatch in a release is a packaging bug. It only logs.
func (s *spa) checkVersion() {
	b, err := fs.ReadFile(s.fsys, "version.json")
	if err != nil {
		s.log.Warn("the web UI has no version.json", logx.Err(err))
		return
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(b, &v); err != nil || v.Version != version.Version() {
		s.log.Warn("the web UI was built for another version", slog.String("web_version", v.Version),
			slog.String("server_version", version.Version()))
	}
}

func (s *spa) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		plainError(w, r, http.StatusMethodNotAllowed)
		return
	}
	// The mux has cleaned the path; the lookup is exact, so no request reaches a file outside the table.
	name := strings.TrimPrefix(r.URL.Path, "/")
	if f := s.files[name]; f != nil {
		s.serveFile(w, r, f, cachePolicy(name), http.StatusOK)
		return
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/assets/"):
		// Never index.html: a stale hashed asset after an upgrade must fail as a 404, not as HTML with a MIME error.
		plainError(w, r, http.StatusNotFound)
	case name == "robots.txt" && s.robots != nil:
		s.serveBuiltin(w, r, s.robots, []byte(robotsTxt))
	case lastSegmentHasDot(name):
		plainError(w, r, http.StatusNotFound)
	case s.index == nil:
		s.serveNotBuilt(w, r)
	default:
		status := http.StatusOK
		if s.status != nil {
			if st := s.status(r.URL.Path); st >= 200 && st <= 599 { // anything else would make WriteHeader panic
				status = st
			}
		}
		s.serveFile(w, r, s.index, cacheNoCache, status)
	}
}

// cachePolicy is the Cache-Control of an existing file (04 §9.5): hashed assets are immutable, icons are cached for
// a day, and everything else at the root (sw.js, manifest, index.html, version.json, …) revalidates every time.
func cachePolicy(name string) string {
	switch {
	case strings.HasPrefix(name, "assets/"):
		return cacheImmutable
	case strings.HasPrefix(name, "icons/"):
		return cacheIcons
	default:
		return cacheNoCache
	}
}

func lastSegmentHasDot(name string) bool {
	return strings.Contains(name[strings.LastIndexByte(name, '/')+1:], ".")
}

// serveFile sends f (or a precompressed sibling the client accepts) with the given Cache-Control. With status 200 it
// goes through http.ServeContent, which answers If-None-Match with 304 and handles HEAD and ranges; any other status
// (index.html as 404 for /setup) sends the body with that status and no validators.
func (s *spa) serveFile(w http.ResponseWriter, r *http.Request, f *spaFile, cacheControl string, status int) {
	h := w.Header()
	v, encoding := f, ""
	if f.br != nil || f.gz != nil {
		h.Add("Vary", "Accept-Encoding")
		switch encoding = pickEncoding(r.Header.Get("Accept-Encoding"), f.br != nil, f.gz != nil); encoding {
		case "br":
			v = f.br
		case "gzip":
			v = f.gz
		}
	}
	s.setTypeHeaders(h, f, cacheControl)

	file, err := s.fsys.Open(v.name)
	if err != nil {
		WriteError(w, r, fmt.Errorf("open web file %s: %w", v.name, err))
		return
	}
	defer func() { _ = file.Close() }()
	if status == http.StatusOK {
		rs, ok := file.(io.ReadSeeker)
		if !ok {
			b, err := io.ReadAll(file)
			if err != nil {
				WriteError(w, r, fmt.Errorf("read web file %s: %w", v.name, err))
				return
			}
			rs = bytes.NewReader(b)
		}
		h.Set("ETag", v.etag)
		if encoding != "" {
			w = &encodingWriter{ResponseWriter: w, encoding: encoding}
		}
		http.ServeContent(w, r, "", time.Time{}, rs)
		return
	}
	if encoding != "" {
		h.Set("Content-Encoding", encoding)
	}
	h.Set("Content-Length", strconv.FormatInt(v.size, 10))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, file) // a failed write means the client is gone
	}
}

// encodingWriter sets Content-Encoding when http.ServeContent sends a precompressed variant's bytes (200, 206), not
// before. ServeContent leaves Content-Length out when Content-Encoding is already set (it expects a writer that
// compresses on the fly), which would send every .br/.gz chunked and HEAD without a length. Its 304, 412 and 416
// answers don't carry the variant, so they get no encoding (and no length of it) either.
type encodingWriter struct {
	http.ResponseWriter
	encoding string
}

func (w *encodingWriter) WriteHeader(code int) {
	if code == http.StatusOK || code == http.StatusPartialContent {
		w.Header().Set("Content-Encoding", w.encoding)
	}
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap returns the wrapped writer, for http.ResponseController.
func (w *encodingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// serveBuiltin sends an in-memory file (the built-in robots.txt) like serveFile does.
func (s *spa) serveBuiltin(w http.ResponseWriter, r *http.Request, f *spaFile, content []byte) {
	h := w.Header()
	s.setTypeHeaders(h, f, cacheNoCache)
	h.Set("ETag", f.etag)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(content))
}

// setTypeHeaders sets Content-Type and Cache-Control, and the HTML policy for HTML documents and /sw.js (04 §9.6).
func (s *spa) setTypeHeaders(h http.Header, f *spaFile, cacheControl string) {
	h.Set("Content-Type", f.contentType)
	h.Set("Cache-Control", cacheControl)
	switch {
	case isHTMLType(f.contentType):
		s.sec.setHTML(h, true)
	case f.name == "sw.js":
		s.sec.setHTML(h, false)
	}
}

// serveNotBuilt answers an SPA route of a binary built without web/dist (04 §9.5).
func (s *spa) serveNotBuilt(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", contentTypeOf("index.html"))
	h.Set("Cache-Control", cacheNoStore)
	h.Set("Content-Length", strconv.Itoa(len(notBuiltHTML)))
	s.sec.setHTML(h, true)
	w.WriteHeader(http.StatusServiceUnavailable)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, notBuiltHTML)
	}
}

// plainError writes a short plain-text error with Cache-Control: no-store (the SPA's 404 and 405 rows of 04 §9.5).
func plainError(w http.ResponseWriter, r *http.Request, status int) {
	body := strconv.Itoa(status) + " " + strings.ToLower(http.StatusText(status)) + "\n"
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", cacheNoStore)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, body)
	}
}

// pickEncoding chooses the precompressed variant for an Accept-Encoding header: "br", "gzip" or "" (identity).
// Brotli wins a tie. A coding with q=0 is refused, "*" stands for codings not listed, and without the header the
// client gets identity (curl and friends without --compressed can't decode).
func pickEncoding(acceptEncoding string, haveBr, haveGz bool) string {
	br, gz := acceptedQ(acceptEncoding)
	if !haveBr {
		br = 0
	}
	if !haveGz {
		gz = 0
	}
	switch {
	case br > 0 && br >= gz:
		return "br"
	case gz > 0:
		return "gzip"
	default:
		return ""
	}
}

// acceptedQ returns the quality values of br and gzip in an Accept-Encoding header (0 when not acceptable).
func acceptedQ(header string) (br, gz float64) {
	br, gz, star := -1.0, -1.0, 0.0
	for part := range strings.SplitSeq(header, ",") {
		coding, params, _ := strings.Cut(part, ";")
		q := 1.0
		for p := range strings.SplitSeq(params, ";") {
			k, v, ok := strings.Cut(p, "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(k), "q") {
				continue
			}
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil || !(f >= 0 && f <= 1) { // also NaN
				f = 0
			}
			q = f
		}
		switch strings.ToLower(strings.TrimSpace(coding)) {
		case "br":
			br = q
		case "gzip", "x-gzip":
			gz = q
		case "*":
			star = q
		}
	}
	if br < 0 {
		br = star
	}
	if gz < 0 {
		gz = star
	}
	return br, gz
}
