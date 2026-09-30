package httpapi

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strings"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// reqState is the per-request state that the chain fills in and the helpers read (RequestID, ClientIP, IsSecure,
// WriteError). It is created before the first step and shared by pointer, so the outer steps (recover, transfer) see
// what the inner ones set even after r.WithContext copies.
type reqState struct {
	route  string       // mux pattern serving the request; the API's route step narrows "/api/v1/" to its own
	log    *slog.Logger // router logger
	id     string       // step 2
	ip     netip.Addr   // step 7
	secure bool         // step 7
	ipSet  bool         // step 7 ran
	logged bool         // this request's 5xx was already logged with its cause (WriteError, recover)
}

type stateKey struct{}

func stateOf(r *http.Request) *reqState {
	st, _ := r.Context().Value(stateKey{}).(*reqState)
	return st
}

// middleware is one named step of the global chain.
type middleware struct {
	name string
	wrap func(http.Handler) http.Handler
}

// chain returns the global middleware chain of 04 §9.3, outermost first. 03's chain (body limit, no-store, CSRF,
// authenticate, rotate, access) runs inside /api/v1/, after these.
func (rt *Router) chain() []middleware {
	return []middleware{
		{"recover", rt.recoverPanics},
		{"request_id", rt.requestID},
		{"read_deadline", rt.readDeadline},
		{"transfer", rt.transfer},
		{"gate", rt.gate},
		{"host_check", rt.hostCheck},
		{"real_ip", rt.realIP},
		{"security_headers", rt.securityHeaders},
	}
}

// withState creates the request state and resolves the route pattern up front, so every step can report it.
func (rt *Router) withState(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := rt.mux.Handler(r)
		st := &reqState{route: pattern, log: rt.log}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), stateKey{}, st)))
	})
}

// recoverPanics is step 1: a panic becomes 500 internal, logged with the stack and the route pattern (never the
// URL, query or body). http.ErrAbortHandler passes through. A panic after the header was sent aborts the response.
func (rt *Router) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w}
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			if p == http.ErrAbortHandler { //nolint:errorlint // the sentinel value itself, as net/http compares it
				panic(p)
			}
			st := stateOf(r)
			st.logged = true
			rt.log.LogAttrs(r.Context(), slog.LevelError, "http handler panic",
				slog.String("route", st.route), slog.String("request_id", st.id),
				slog.String("panic", fmt.Sprint(p)), slog.String("stack", string(debug.Stack())))
			switch {
			case sw.hijacked:
			case sw.status != 0:
				panic(http.ErrAbortHandler) // part of the response is out: break the connection instead
			default:
				// Nothing was sent: drop what the handler prepared for its own answer (WriteError drops the
				// validators and the encoding) and answer 500.
				h := w.Header()
				for _, k := range []string{"Set-Cookie", "Content-Range", "Content-Disposition", "Location"} {
					h.Del(k)
				}
				setBaseHeaders(h)
				WriteError(sw, r, api.NewError(api.CodeInternal))
			}
		}()
		next.ServeHTTP(sw, r)
	})
}

// requestID is step 2: 16 hex characters, in the X-Request-Id response header and RequestID(r).
func (rt *Router) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := stateOf(r)
		st.id = newRequestID()
		w.Header().Set("X-Request-Id", st.id)
		next.ServeHTTP(w, r)
	})
}

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error (Go 1.24+)
	return hex.EncodeToString(b[:])
}

// RequestID returns the request's ID (16 hex characters, also in the X-Request-Id header), or "" for a request that
// did not pass through the router.
func RequestID(r *http.Request) string {
	if st := stateOf(r); st != nil {
		return st.id
	}
	return ""
}

// readDeadline is step 3: the main server has no ReadTimeout (it would cut WebSockets), so each request gets a read
// deadline through the ResponseController and a client can't trickle a body forever (04 §9.3, §7.8). net/http's
// background read (which watches for the client going away) hits the same deadline, so a handler still running
// after it sees its request context cancelled; the /ws mount clears the deadline before the hub's upgrade. Writers
// that don't support deadlines (tests' recorders) are skipped.
func (rt *Router) readDeadline(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(rt.readTimeout))
		if err != nil && !isNotSupported(err) {
			rt.log.LogAttrs(r.Context(), slog.LevelDebug, "set read deadline", logx.Err(err))
		}
		next.ServeHTTP(w, r)
	})
}

func isNotSupported(err error) bool { return errors.Is(err, http.ErrNotSupported) }

// transfer is step 4: it reports the status and body bytes of every response to the RouteObserver, and logs one line
// for a 5xx whose cause was not logged already (no access log otherwise, 04 §9.3).
func (rt *Router) transfer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w}
		st := stateOf(r)
		completed := false
		defer func() {
			status := sw.status
			switch {
			case status != 0:
			case !completed && !sw.hijacked:
				status = http.StatusInternalServerError // a panic: recover answers 500 (and logs it)
			default:
				status = http.StatusOK // nothing written: net/http sends 200
			}
			if rt.opts.Observer != nil {
				rt.opts.Observer.ObserveRoute(st.route, status, sw.bytes)
			}
			if completed && status >= 500 && !st.logged {
				level := slog.LevelWarn
				if status == http.StatusInternalServerError {
					level = slog.LevelError
				}
				rt.log.LogAttrs(r.Context(), level, "http request failed", slog.String("route", st.route),
					slog.Int("status", status), slog.String("request_id", st.id))
			}
		}()
		next.ServeHTTP(sw, r)
		completed = true
	})
}

// shutdownRetryAfter is the Retry-After of the gate's 503 in seconds (04 §6.4).
const shutdownRetryAfter = 5

// gate is step 5: while shutting down, every request except /healthz and /readyz gets 503 server_shutdown.
func (rt *Router) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rt.opts.Gate.ShuttingDown() && !isHealthPath(r.URL.Path) {
			setBaseHeaders(w.Header())
			WriteError(w, r, &api.Error{Code: api.CodeServerShutdown, RetryAfter: shutdownRetryAfter})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isHealthPath(p string) bool { return p == "/healthz" || p == "/readyz" }

// hostCheck is step 6: the Host header must name this site (or, in dev, any loopback host so the Vite proxy works).
// Anything else gets 421 in plain text. It blocks DNS rebinding against LAN installs and keeps cookies and Origin
// checks consistent (04 §9.3).
func (rt *Router) hostCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isHealthPath(r.URL.Path) && !rt.hostAllowed(r.Host) {
			h := w.Header()
			setBaseHeaders(h)
			h.Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusMisdirectedRequest)
			_, _ = w.Write([]byte("421 misdirected request\n"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostAllowed compares case-insensitively with Site.Host, also accepting the scheme's default port spelled out
// ("watch.example.com:443"). In dev, localhost, 127.0.0.1 and [::1] match with any port.
func (rt *Router) hostAllowed(host string) bool {
	site := rt.opts.Site
	if site.Host != "" {
		if strings.EqualFold(host, site.Host) {
			return true
		}
		if !hasPort(site.Host) && strings.EqualFold(host, site.Host+":"+defaultPort(site.Origin)) {
			return true
		}
	}
	if site.Dev {
		name := host
		if h, _, err := net.SplitHostPort(host); err == nil {
			name = h
		}
		switch strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(name, "["), "]")) {
		case "localhost", "127.0.0.1", "::1":
			return true
		}
	}
	return false
}

// hasPort reports whether a host ("example.com", "[::1]", "203.0.113.7:8443", "[::1]:8443") carries a port.
func hasPort(host string) bool {
	i := strings.LastIndexByte(host, ':')
	return i >= 0 && i > strings.LastIndexByte(host, ']')
}

func defaultPort(origin string) string {
	if strings.HasPrefix(strings.ToLower(origin), "http://") {
		return "80"
	}
	return "443"
}

// statusWriter records the final status and the body bytes of a response. Every wrapper of the chain implements
// Unwrap, so http.ResponseController reaches the connection (read deadlines, Flush, Hijack); Flush and Hijack are
// also implemented directly for handlers that type-assert.
type statusWriter struct {
	http.ResponseWriter
	status   int // 0 until the header is written
	bytes    int64
	hijacked bool
}

func (w *statusWriter) WriteHeader(code int) {
	// 1xx other than 101 are informational: more headers follow.
	if w.status == 0 && (code < 100 || code > 199 || code == http.StatusSwitchingProtocols) {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Unwrap returns the wrapped writer, for http.ResponseController.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush sends buffered data (and the header, with status 200 if none was set).
func (w *statusWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// Hijack takes over the connection (WebSocket upgrades); it fails with http.ErrNotSupported on HTTP/2.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.hijacked = true
		if w.status == 0 {
			w.status = http.StatusSwitchingProtocols
		}
	}
	return c, rw, err
}
