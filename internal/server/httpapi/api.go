package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/auth"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// Access is the access level of an /api/v1 route (03 §12.1).
type Access uint8

const (
	// Public routes need no authentication; the CSRF check still applies to their unsafe methods.
	Public Access = iota
	// User routes need an active user: the session cookie in M1 (cookie or bearer in M2).
	User
	// Admin routes need an active admin.
	Admin
)

func (a Access) String() string {
	switch a {
	case Public:
		return "public"
	case User:
		return "user"
	case Admin:
		return "admin"
	}
	return fmt.Sprintf("Access(%d)", uint8(a))
}

// apiPrefix is where 04's router mounts the API. Route patterns keep it: the API sees the full request path.
const apiPrefix = "/api/v1/"

// Body limits of 03 §12.1 and §14: the auth and device endpoints accept 16 KiB, everything else 64 KiB.
const (
	authBodyLimit  = 16 << 10
	otherBodyLimit = 64 << 10
)

// Deps are what the API needs (03 §12.5). The wiring fills them (04 §6.6).
type Deps struct {
	DB   *store.DB     // required
	Auth *auth.Service // required
	// Signal is the wiring adapter over 01's hub; nil (tests) means no presence and no-op hooks.
	Signal Signal
	// Push is the wiring adapter over 04's push service; nil when push is off, which answers push_unavailable and
	// omits push from GET /info.
	Push Push
	// Info is 04's build information; nil means this binary's version.Version() and protocol range.
	Info InfoSource
	// Site is 04's site (config.NewSite). GET /info reports Site.Origin as server.publicUrl and falls back to its
	// host name for server.name when the serverName setting is empty (03 §12.4.1). Not in 03 §12.5's field list,
	// which has no other way to reach the public origin; the wiring passes the RouterOptions.Site value.
	Site Site
	// ClientIP is 04's httpapi.ClientIP (trusted-proxy aware); nil means ClientIP.
	ClientIP func(*http.Request) netip.Addr
	// Clock is the API's clock; nil means time.Now.
	Clock func() time.Time
	// Logger is the server logger; the API adds component=api. nil means slog.Default().
	Logger *slog.Logger
}

// authService is the part of *auth.Service that the /api/v1 chain calls. Tests replace it with a fake; the handlers
// of later slices call Deps.Auth directly.
type authService interface {
	Authenticate(r *http.Request) (auth.Principal, error)
	MaybeRotate(w http.ResponseWriter, p auth.Principal)
	CSRF(next http.Handler) http.Handler
}

var _ authService = (*auth.Service)(nil)

// API is 03's REST API under /api/v1 (03 §12). 04's router mounts it at /api/v1/ (RouterOptions.API), behind the
// global chain of 04 §9.3. Its own chain runs, outermost first (03 §12.1):
//
//  1. body limit: 16 KiB for /api/v1/auth/ and /api/v1/device/, 64 KiB elsewhere; a larger Content-Length gets 413
//     payload_too_large at once, and the body reader stops at the limit (DecodeJSON then answers the same);
//  2. no-store: Cache-Control: no-store on every response, errors included, whatever a handler sets;
//  3. route lookup: a path no route matches gets 404 not_found, a known path with another method 405
//     method_not_allowed with an Allow header, both as 03's JSON envelope;
//  4. CSRF: the route's unsafe methods (all but GET, HEAD and OPTIONS) pass Auth.CSRF, the cross-origin and
//     Content-Type check of 03 §7.5 (403 csrf_failed, 415 unsupported_media_type);
//  5. authenticate (User and Admin routes): Auth.Authenticate; its error is the answer (401 unauthenticated);
//  6. rotate: Auth.MaybeRotate;
//  7. access: an Admin route answers a non-admin 403 forbidden;
//  8. the handler, with the principal in its context (PrincipalFrom).
//
// Steps 4–8 are per route, so a route registered with Handle gets them all.
type API struct {
	d       Deps
	auth    authService
	mux     *http.ServeMux
	handler http.Handler
}

// New builds the API and registers 03's routes. It panics when Deps.DB or Deps.Auth is nil, a programming error of
// the wiring.
func New(d Deps) *API {
	if d.DB == nil {
		panic("httpapi.New: Deps.DB is nil")
	}
	if d.Auth == nil {
		panic("httpapi.New: Deps.Auth is nil")
	}
	return newAPI(d, d.Auth)
}

// newAPI is New with the chain's auth calls going to au.
func newAPI(d Deps, au authService) *API {
	if d.Signal == nil {
		d.Signal = nopSignal{}
	}
	if d.Info == nil {
		d.Info = buildInfo{}
	}
	if d.ClientIP == nil {
		d.ClientIP = ClientIP
	}
	if d.Clock == nil {
		d.Clock = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	d.Logger = d.Logger.With(slog.String("component", "api"))
	a := &API{d: d, auth: au, mux: http.NewServeMux()}
	a.handler = a.bodyLimit(a.noStore(http.HandlerFunc(a.route)))
	a.routes()
	return a
}

// routes registers 03's endpoints (03 §12.3). The later slices add theirs here.
func (a *API) routes() {
	a.Handle("GET /api/v1/info", Public, http.HandlerFunc(a.getInfo))
}

// ServeHTTP serves every request under /api/v1/ through the API's chain. After the no-store step it answers every
// unmatched /api/v1 path with 404 not_found and every method mismatch with 405 method_not_allowed plus an Allow
// header, through WriteError. This covers routes added through Handle. 04's router handles only /api/ paths outside
// /api/v1/.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.handler.ServeHTTP(w, r) }

// Handle registers a route owned by another doc behind the same /api/v1 chain (body limit, no-store, CSRF, auth,
// rotation, access), e.g. a.Handle("POST /api/v1/admin/doctor", httpapi.Admin, h). Patterns use Go 1.22+ ServeMux
// syntax without a host, and their path starts with /api/v1/. A GET pattern also serves HEAD. Like
// http.ServeMux.Handle, it panics on an invalid or conflicting pattern; it also panics on a path outside /api/v1/, an
// unknown Access or a nil handler.
func (a *API) Handle(pattern string, access Access, h http.Handler) {
	if access > Admin {
		panic(fmt.Sprintf("httpapi: API.Handle(%q): unknown %v", pattern, access))
	}
	if h == nil {
		panic(fmt.Sprintf("httpapi: API.Handle(%q): nil handler", pattern))
	}
	path := pattern
	if i := strings.IndexAny(pattern, " \t"); i >= 0 {
		path = strings.TrimLeft(pattern[i:], " \t")
	}
	if !strings.HasPrefix(path, apiPrefix) {
		panic(fmt.Sprintf("httpapi: API.Handle(%q): the path must start with %s and have no host", pattern, apiPrefix))
	}
	if access != Public {
		h = a.authenticate(access, h)
	}
	a.mux.Handle(pattern, a.auth.CSRF(h))
}

// DashboardAccounts builds the "accounts" object of 04's admin dashboard (03 §12.4.8). It comes with the audit and
// dashboard slice (03 §18 slice 13); until then it returns an error that wraps *api.Error{internal}.
func (a *API) DashboardAccounts(ctx context.Context) (api.DashboardAccounts, error) {
	return api.DashboardAccounts{}, fmt.Errorf("httpapi: DashboardAccounts not implemented: %w",
		api.NewError(api.CodeInternal))
}

type principalKey struct{}

// PrincipalFrom returns the authenticated caller of a User or Admin route. It reports false on Public routes and
// outside the API.
func PrincipalFrom(ctx context.Context) (auth.Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(auth.Principal)
	return p, ok
}

// bodyLimitFor returns the body limit of a request path (03 §12.1).
func bodyLimitFor(path string) int64 {
	if strings.HasPrefix(path, apiPrefix+"auth/") || strings.HasPrefix(path, apiPrefix+"device/") {
		return authBodyLimit
	}
	return otherBodyLimit
}

// bodyLimit is step 1. It checks a declared Content-Length up front and caps the body reader for chunked bodies.
// The reader is http.MaxBytesReader on the innermost writer (maxBytesReader), so net/http closes the connection
// after the reply instead of draining an oversized body.
func (a *API) bodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := bodyLimitFor(r.URL.Path)
		if r.ContentLength > limit {
			WriteError(w, r, api.NewError(api.CodePayloadTooLarge))
			return
		}
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = maxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

// noStore is step 2: every /api/v1 response is per-user, so none may be cached (03 §12.1, §13). The global chain's
// security headers already set no-store; the writer below also restores it if a handler changes Cache-Control.
func (a *API) noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", cacheNoStore)
		next.ServeHTTP(&noStoreWriter{ResponseWriter: w}, r)
	})
}

// noStoreWriter sets Cache-Control: no-store when the header goes out. It implements Unwrap (for
// http.ResponseController and maxBytesReader) and Flush.
type noStoreWriter struct {
	http.ResponseWriter
	sent bool // a final (non-1xx) header was written
}

func (w *noStoreWriter) WriteHeader(code int) {
	w.ResponseWriter.Header().Set("Cache-Control", cacheNoStore)
	if code >= 200 || code == http.StatusSwitchingProtocols {
		w.sent = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *noStoreWriter) Write(b []byte) (int, error) {
	if !w.sent {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap returns the wrapped writer, for http.ResponseController.
func (w *noStoreWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush sends the header (status 200 if none was set) and any buffered body.
func (w *noStoreWriter) Flush() {
	if !w.sent {
		w.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// route is step 3: it hands a request to the route that matches it, or answers 404 or 405 with 03's envelope.
func (a *API) route(w http.ResponseWriter, r *http.Request) {
	h, pattern := a.mux.Handler(r)
	if pattern != "" {
		a.mux.ServeHTTP(w, r) // sets r.Pattern and the path values
		return
	}
	// No route: ServeMux's own handler knows whether another method would match. Run it on a probe that keeps only
	// the status and the Allow header, then answer in JSON.
	probe := &methodProbe{header: http.Header{}}
	h.ServeHTTP(probe, r)
	if probe.status == http.StatusMethodNotAllowed {
		if allow := probe.header.Get("Allow"); allow != "" {
			w.Header().Set("Allow", allow)
		}
		WriteError(w, r, api.NewError(api.CodeMethodNotAllowed))
		return
	}
	WriteError(w, r, api.NewError(api.CodeNotFound))
}

// methodProbe is a ResponseWriter that records the status and header and drops the body.
type methodProbe struct {
	header http.Header
	status int
}

func (p *methodProbe) Header() http.Header { return p.header }

func (p *methodProbe) WriteHeader(code int) {
	if p.status == 0 {
		p.status = code
	}
}

func (p *methodProbe) Write(b []byte) (int, error) {
	if p.status == 0 {
		p.status = http.StatusOK
	}
	return len(b), nil
}

// authenticate is steps 5–7 for a User or Admin route.
func (a *API) authenticate(access Access, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := a.auth.Authenticate(r)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		a.auth.MaybeRotate(w, p)
		if access == Admin && !p.IsAdmin() {
			WriteError(w, r, api.NewError(api.CodeForbidden))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}
