package assets

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Guard is Wails' AssetOptions.Middleware: requests under /wails/ pass only from the asset origin (§5.6). Requests
// outside /wails/ pass through unchanged. Under /wails/:
//
//  1. Origin: the Origin header equals origin, or there is no Origin and Sec-Fetch-Site is same-origin; otherwise
//     403 and one rate-limited WARN line. For a custom-scheme origin (wails://localhost, the macOS dev build) a
//     request with neither header passes too: WKWebView sends neither to the scheme handler (guard.customScheme).
//  2. /wails/runtime: only POST; the object and method are read the way the pinned transport reads them (the JSON
//     body, or the query when the body is empty; runtime_allow.go), from at most 64 KiB of body (413 above), which
//     is then restored for the runtime. Only Call and CancelCall pass; every other object, a request whose object
//     can't be parsed, a body with object or method also in the query, and a request that uses the transport's
//     chunking get 403 and one rate-limited WARN line with the object and method names only.
//  3. The runtime's other paths: GET and HEAD of its static scripts and of /wails/eventpayload/ pass; everything
//     else under /wails/ (GoStream's /wails/stream/ included: the app registers no stream) gets 403.
//
// The WARN lines go to slog.Default() with component=assets.
func Guard(origin string) func(http.Handler) http.Handler {
	return newGuard(origin, nil, time.Now).middleware
}

// guard is Guard's state: the rate limit of its WARN lines.
type guard struct {
	origin string
	// customScheme is true for an asset origin whose scheme isn't http or https (wails://localhost, Wails on
	// macOS): WKWebView hands its URL-scheme handler no Origin and no Sec-Fetch-Site at all, not even on a POST
	// (measured with the checklist at beta.34), and a custom scheme is reachable only from the app's own webview,
	// so a request without both headers counts as same-origin there. A request that names another origin or
	// another site is still refused.
	customScheme bool
	log          *slog.Logger // nil: slog.Default() at the time of the line
	warn         *warnLimiter
}

func newGuard(origin string, log *slog.Logger, now func() time.Time) *guard {
	scheme, _, _ := strings.Cut(origin, "://")
	return &guard{
		origin:       origin,
		customScheme: origin != "" && scheme != "http" && scheme != "https",
		log:          log,
		warn:         newWarnLimiter(time.Minute, now),
	}
}

func (g *guard) logger() *slog.Logger {
	if g.log != nil {
		return g.log
	}
	return slog.Default()
}

func (g *guard) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if !strings.HasPrefix(p, wailsPrefix) && p != "/wails" {
			next.ServeHTTP(w, r)
			return
		}
		if !g.sameOrigin(r) {
			o := r.Header.Get("Origin")
			if len(o) > 128 {
				o = o[:128]
			}
			g.refuse(w, r, http.StatusForbidden, "origin", "desktop: runtime request from another origin",
				slog.String("origin", o))
			return
		}
		switch {
		case p == runtimePath:
			g.runtime(w, r, next)
		case staticRuntimePaths[p] || strings.HasPrefix(p, eventPayloadPath):
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				plainError(w, r, http.StatusMethodNotAllowed)
				return
			}
			next.ServeHTTP(w, r)
		default:
			g.refuse(w, r, http.StatusForbidden, "path", "desktop: runtime path refused")
		}
	})
}

// sameOrigin is check 1.
func (g *guard) sameOrigin(r *http.Request) bool {
	if g.origin == "" {
		return false
	}
	origins := r.Header.Values("Origin")
	switch len(origins) {
	case 0:
		site := r.Header.Get("Sec-Fetch-Site")
		return site == "same-origin" || (site == "" && g.customScheme)
	case 1:
		return origins[0] == g.origin
	default:
		return false
	}
}

// runtime is check 2.
func (g *guard) runtime(w http.ResponseWriter, r *http.Request, next http.Handler) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		plainError(w, r, http.StatusMethodNotAllowed)
		return
	}
	for name := range r.Header {
		if strings.HasPrefix(strings.ToLower(name), headerChunkPrefix) {
			g.refuse(w, r, http.StatusForbidden, "object", "desktop: runtime object refused",
				slog.String("reason", "chunked"))
			return
		}
	}
	body, err := readLimited(r.Body, runtimeBodyLimit)
	if errors.Is(err, errTooLarge) {
		plainError(w, r, http.StatusRequestEntityTooLarge)
		return
	}
	if err != nil {
		plainError(w, r, http.StatusBadRequest)
		return
	}
	object, method, ok := runtimeCall(body, r.URL)
	if !ok {
		g.refuse(w, r, http.StatusForbidden, "object", "desktop: runtime object refused",
			slog.String("reason", "unparsable"))
		return
	}
	if !allowedCall(object, method) {
		on, mn := callName(object, method)
		g.refuse(w, r, http.StatusForbidden, "object", "desktop: runtime object refused",
			slog.String("object", on), slog.String("method", mn))
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	next.ServeHTTP(w, r)
}

// runtimeRequest is the transport's view of a request body (transport_http.go's request type).
type runtimeRequest struct {
	Object *int            `json:"object"`
	Method *int            `json:"method"`
	Args   json.RawMessage `json:"args"`
}

// runtimeCall reads the object and method the way the pinned transport does: from the JSON body, or, when the body
// is empty, from the query. A non-empty body whose request also names an object or method in the query is refused
// (ok false): the pinned runtime never sends both, and a later transport that preferred the query must not see an
// object this check didn't.
func runtimeCall(body []byte, u *url.URL) (object, method int, ok bool) {
	q := u.Query()
	var req runtimeRequest
	if len(body) > 0 {
		if q.Has("object") || q.Has("method") {
			return 0, 0, false
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return 0, 0, false
		}
	} else {
		if o, err := strconv.Atoi(q.Get("object")); err == nil {
			req.Object = &o
		}
		if m, err := strconv.Atoi(q.Get("method")); err == nil {
			req.Method = &m
		}
	}
	if req.Object == nil || req.Method == nil {
		return 0, 0, false
	}
	return *req.Object, *req.Method, true
}

var errTooLarge = errors.New("assets: body too large")

// readLimited reads all of rc, failing with errTooLarge past limit bytes. It closes rc.
func readLimited(rc io.ReadCloser, limit int) ([]byte, error) {
	if rc == nil {
		return nil, nil
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > limit {
		return nil, errTooLarge
	}
	return b, nil
}

// refuse answers status and logs msg at WARN through the rate limit of kind.
func (g *guard) refuse(w http.ResponseWriter, r *http.Request, status int, kind, msg string, attrs ...slog.Attr) {
	if dropped, ok := g.warn.allow(kind); ok {
		args := make([]any, 0, len(attrs)+2)
		args = append(args, slog.String("component", "assets"))
		for _, a := range attrs {
			args = append(args, a)
		}
		if dropped > 0 {
			args = append(args, slog.Int("dropped", dropped))
		}
		g.logger().Warn(msg, args...)
	}
	plainError(w, r, status)
}

// warnLimiter lets one line per kind through per window and counts the rest, which the next line that gets through
// reports as "dropped".
type warnLimiter struct {
	every time.Duration
	now   func() time.Time

	mu      sync.Mutex
	next    map[string]time.Time
	dropped map[string]int
}

func newWarnLimiter(every time.Duration, now func() time.Time) *warnLimiter {
	return &warnLimiter{every: every, now: now, next: map[string]time.Time{}, dropped: map[string]int{}}
}

// allow reports whether a line of kind may be logged now, and how many were dropped since the last one.
func (l *warnLimiter) allow(kind string) (dropped int, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if next, seen := l.next[kind]; seen && now.Before(next) {
		l.dropped[kind]++
		return 0, false
	}
	dropped = l.dropped[kind]
	l.dropped[kind] = 0
	l.next[kind] = now.Add(l.every)
	return dropped, true
}
