package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
)

// handler answers the URLs of the package comment from the release roots under root.
type handler struct {
	root   *os.Root
	latest string // "" when there is no latest version

	mu  sync.Mutex // serializes the request log lines
	log io.Writer
}

// newHandler returns the handler for the release roots under root. latest is the version the latest URLs redirect
// to, without "v"; empty turns them off. One line per request goes to log.
func newHandler(root *os.Root, latest string, log io.Writer) http.Handler {
	return &handler{root: root, latest: latest, log: log}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	h.serve(rec, r)
	h.mu.Lock()
	defer h.mu.Unlock()
	// %q: the path is the client's, and a line break in it must not start a second log line.
	_, _ = fmt.Fprintf(h.log, "relserve: %s %q %d\n", r.Method, r.URL.Path, rec.status)
}

func (h *handler) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// A path that is not in its clean form (//, /./, /../, a trailing slash) names nothing here. Refusing it,
	// instead of cleaning it, keeps one URL per file.
	p := r.URL.Path
	if !strings.HasPrefix(p, "/") || path.Clean(p) != p {
		http.NotFound(w, r)
		return
	}
	var segs []string
	if p != "/" {
		segs = strings.Split(strings.TrimPrefix(p, "/"), "/")
	}

	// The segments before "download", "latest" or "tag" name the release root; the ones after it say what is asked
	// of that root. A position decides only when both fit, so a mirror directory that happens to be called
	// "download" still works.
	for i, seg := range segs {
		prefix, rest := segs[:i], segs[i+1:]
		switch {
		case seg == "download" && len(rest) == 2 && h.isRoot(prefix):
			h.file(w, r, join(prefix, "download", rest[0], rest[1]))
			return
		case seg == "latest" && len(rest) == 0 && h.isRoot(prefix):
			h.redirectLatest(w, r, prefix, "tag/v"+h.latest)
			return
		case seg == "latest" && len(rest) == 2 && rest[0] == "download" && h.isRoot(prefix):
			h.redirectLatest(w, r, prefix, "download/v"+h.latest+"/"+rest[1])
			return
		case seg == "tag" && len(rest) == 1 && h.isRoot(prefix):
			h.tag(w, r, prefix, rest[0])
			return
		}
	}
	http.NotFound(w, r)
}

// join returns the slash-separated path of elem under the directory that the segments of prefix name.
func join(prefix []string, elem ...string) string {
	return path.Join(append(append([]string{}, prefix...), elem...)...)
}

// isRoot reports whether the directory at prefix is a release root: it has a download directory.
func (h *handler) isRoot(prefix []string) bool {
	return h.isDir(join(prefix, "download"))
}

func (h *handler) isDir(name string) bool {
	fi, err := h.root.Stat(name)
	return err == nil && fi.IsDir()
}

// file serves the regular file name, a slash-separated path inside the root.
func (h *handler) file(w http.ResponseWriter, r *http.Request, name string) {
	f, err := h.root.Open(name)
	if err != nil {
		// Not there, or not allowed (a link that leaves the root): the same answer, as on a real server.
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	// Release assets are downloads, never pages: no content sniffing, no caching between two test steps.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

// redirectLatest answers 302 with the absolute URL of target inside the release root at prefix, as GitHub does for
// /releases/latest. Without a latest version there is nothing to point to.
func (h *handler) redirectLatest(w http.ResponseWriter, r *http.Request, prefix []string, target string) {
	if h.latest == "" {
		http.NotFound(w, r)
		return
	}
	// The host is the one the client asked, as in GitHub's answer, so the redirect stays on the server the client
	// already talks to; the path is made of the request's clean segments and the validated version.
	loc := url.URL{Scheme: "https", Host: r.Host, Path: "/" + join(prefix, target)}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, loc.String(), http.StatusFound) //nolint:gosec // G710: to the request's own host, see above
}

// tag answers 200 for the release page of a version that the root has.
func (h *handler) tag(w http.ResponseWriter, r *http.Request, prefix []string, tag string) {
	version, ok := strings.CutPrefix(tag, "v")
	if !ok || !validVersion(version) || !h.isDir(join(prefix, "download", tag)) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.WriteString(w, "A test release of isshoni, served by relserve.\n")
}

// statusRecorder remembers the status code of an answer for the request log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status, s.wrote = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wrote = true
	return s.ResponseWriter.Write(b)
}
