package httpapi

import (
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// TLSMode is the effective TLS mode of the site (04 §4.4): "auto", "ip", "manual" or "off".
//
// It mirrors config.TLSMode, and Site mirrors config.Site field for field: S15's config package is built in the same
// plan group as this router, so this package cannot import it yet. Once both are merged, the integrator replaces the
// two declarations with `type TLSMode = config.TLSMode` and `type Site = config.Site`; nothing else changes.
type TLSMode string

// tlsOff is the mode in which X-Forwarded-For and X-Forwarded-Proto are honored from trusted proxies (04 §8.5).
const tlsOff TLSMode = "off"

// Site is how the outside world reaches this server (04 §4.4); see TLSMode.
type Site struct {
	Origin   string  // "https://watch.example.com", no trailing slash
	Host     string  // "watch.example.com", "203.0.113.7", "[2001:db8::1]", with ":port" if not default
	Hostname string  // without port and brackets
	TLSMode  TLSMode // effective mode
	// Dev is true in off mode with a loopback listen.http and an empty or loopback public_url: the Host check then
	// accepts any localhost, 127.0.0.1 or [::1] host (the Vite proxy), and IsSecure is true.
	Dev bool
	// ExtraOrigins: later (M2) the Wails asset origins, added in code, not config. The router doesn't use it.
	ExtraOrigins []string
}

// RouterOptions configures NewRouter (04 §9.2).
type RouterOptions struct {
	Site Site
	// TrustedProxies are the peers whose X-Forwarded-For and X-Forwarded-Proto are honored; only used when
	// Site.TLSMode is off (04 §8.5). The wiring passes config's effective network.trusted_proxies.
	TrustedProxies []netip.Prefix
	// HSTS is config tls.hsts: send Strict-Transport-Security on TLS responses when the site is a domain (04 §9.6).
	// Not in 04 §9.2's field list, which has no other way to reach the key.
	HSTS bool
	// SPA is the built web app, web.Dist(). nil serves the "web UI not built" page.
	SPA fs.FS
	// SPAStatus is 03's hook for the status of an index.html fallback (404 for "/setup" once an admin exists). nil
	// means 200 for every path.
	SPAStatus func(path string) int
	// Gate is the shutting-down switch (step 5 of the chain). nil never rejects.
	Gate *Gate
	// API is 03's *API, mounted at /api/v1/. nil answers every /api/v1/ path with 404 not_found (tests).
	API http.Handler
	// WS is 01's *signal.Hub, mounted at GET /ws with the read deadline cleared. nil answers 404 not_found (tests).
	WS http.Handler
	// Observer receives one call per response (step 4). ops.Metrics implements it; the wiring passes it in, so
	// httpapi never imports ops. nil drops the observations.
	Observer RouteObserver
	// Logger is the server logger; the router adds component=http. nil means slog.Default().
	Logger *slog.Logger
}

// RouteObserver receives one call per response from the transfer/metrics middleware (04 §9.3).
//
// pattern is the http.ServeMux pattern that routes the request ("/api/v1/", "GET /ws", "GET /healthz", "/" for the
// SPA), also for responses written before routing (the gate's 503, the Host check's 421). status is the final status
// (101 for a WebSocket upgrade, 500 for a panic) and bytes the response body bytes written through the
// ResponseWriter (not the bytes of a hijacked connection).
type RouteObserver interface {
	ObserveRoute(pattern string, status int, bytes int64)
}

// defaultReadTimeout is the per-request read deadline of step 3 (04 §7.8).
const defaultReadTimeout = 30 * time.Second

// Router is the main server's handler: an http.ServeMux with the reserved mounts (SPA, /api/v1/, /ws) wrapped in the
// global middleware chain of 04 §9.3. Build it with NewRouter, add routes with Handle (the server adds /healthz and
// /readyz), then serve Handler().
type Router struct {
	opts    RouterOptions
	log     *slog.Logger
	mux     *http.ServeMux
	sec     secHeaders
	handler http.Handler

	// readTimeout is the per-request read deadline; tests shorten it before serving.
	readTimeout time.Duration
}

// NewRouter builds the router. It walks opts.SPA once (ETags, content types, precompressed siblings; 04 §9.5) and
// logs a warning when the web build is missing (non-dev builds) or its version.json differs from this binary.
func NewRouter(opts RouterOptions) *Router {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With(slog.String("component", "http"))
	rt := &Router{
		opts:        opts,
		log:         log,
		mux:         http.NewServeMux(),
		sec:         newSecHeaders(opts.Site, opts.HSTS),
		readTimeout: defaultReadTimeout,
	}
	spa := newSPA(opts.SPA, opts.SPAStatus, rt.sec, log)

	notFound := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, api.NewError(api.CodeNotFound))
	})
	apiHandler := opts.API
	if apiHandler == nil {
		apiHandler = notFound
	}
	rt.mux.Handle("/", spa)
	rt.mux.Handle("/api/v1/", apiHandler)
	// Reserved paths that no route handles answer JSON 404, never index.html (04 §9.5).
	rt.mux.Handle("/api/", notFound)
	rt.mux.Handle("/ws", notFound)
	rt.mux.Handle("/healthz", notFound)
	rt.mux.Handle("/readyz", notFound)
	if opts.WS != nil {
		rt.mux.Handle("GET /ws", rt.wsMount(opts.WS))
	}

	h := http.Handler(rt.mux)
	chain := rt.chain()
	for i := len(chain) - 1; i >= 0; i-- {
		h = chain[i].wrap(h)
	}
	rt.handler = rt.withState(h)
	return rt
}

// Handle registers a route on the router's mux, inside the global chain. Patterns use Go 1.22+ ServeMux syntax
// ("GET /healthz"). Like http.ServeMux.Handle, it panics on an invalid or conflicting pattern. Routes under /api/v1/
// are 03's: register them with API.Handle, which adds 03's chain.
func (rt *Router) Handle(pattern string, h http.Handler) { rt.mux.Handle(pattern, h) }

// HandleFunc is Handle for a function.
func (rt *Router) HandleFunc(pattern string, fn http.HandlerFunc) { rt.mux.Handle(pattern, fn) }

// Handler returns the mux wrapped in the global middleware chain; the main server serves it.
func (rt *Router) Handler() http.Handler { return rt.handler }

// wsMount clears the read deadline of step 3 before 01's hub takes the request, so a WebSocket is never cut by it
// (net/http also clears deadlines on Hijack; this covers the time before the upgrade).
func (rt *Router) wsMount(ws http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).SetReadDeadline(time.Time{}); err != nil && !isNotSupported(err) {
			rt.log.LogAttrs(r.Context(), slog.LevelDebug, "clear read deadline", logx.Err(err))
		}
		ws.ServeHTTP(w, r)
	})
}

// Gate is the shutting-down switch of step 5 (04 §6.4, §9.3). The zero value is serving; SetShuttingDown flips it
// once and for good (a restart builds a new server). A nil *Gate is always serving.
type Gate struct {
	shuttingDown atomic.Bool
}

// SetShuttingDown makes the gate answer every request except /healthz and /readyz with 503 server_shutdown.
func (g *Gate) SetShuttingDown() { g.shuttingDown.Store(true) }

// ShuttingDown reports whether SetShuttingDown was called.
func (g *Gate) ShuttingDown() bool { return g != nil && g.shuttingDown.Load() }
