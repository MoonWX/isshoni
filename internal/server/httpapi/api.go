package httpapi

import (
	"context"
	"errors"
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

// Access is the access level of an /api/v1 route (03 §12.1; M2 02 §6.4): which credential the route takes.
type Access uint8

const (
	// Public routes need no authentication and read no credential in the chain: the Authorization header is ignored,
	// cookies matter only to the handler's own logic, and the CSRF check applies to their unsafe methods.
	Public Access = iota
	// User routes need an active user, with either credential: the session cookie or a device's bearer access token.
	User
	// Admin routes need an active admin's session cookie. A bearer, even an admin's, is 403 forbidden
	// {credential: session} (M2 02 §6.4): no desktop screen calls an admin route, and a device's credential must not
	// reach server administration.
	Admin
	// UserSession routes need an active user's session cookie; a bearer is 403 forbidden {credential: session}
	// (M2 02 §6.4: account changes, the device approval, web push).
	UserSession
	// UserDevice routes need a device's bearer access token; a session cookie is 403 forbidden {credential: device}
	// (M2 02 §6.4: the app's own sign-out).
	UserDevice
)

func (a Access) String() string {
	switch a {
	case Public:
		return "public"
	case User:
		return "user"
	case Admin:
		return "admin"
	case UserSession:
		return "user-session"
	case UserDevice:
		return "user-device"
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
	// Site is 04's site (config.NewSite); required, New panics on an empty Origin. GET /info reports Site.Origin as
	// server.publicUrl and falls back to its host name for server.name when the serverName setting is empty
	// (03 §12.4.1). The wiring passes the RouterOptions.Site value.
	Site Site
	// ClientIP is 04's httpapi.ClientIP (trusted-proxy aware); nil means ClientIP.
	ClientIP func(*http.Request) netip.Addr
	// DesktopOrigins are the origins of the desktop app's bundled SPA, the only cross-origin pages the API answers
	// CORS for, and only with bearer tokens (M2 02 §6.4). The wiring passes Site.ExtraOrigins, which it sets to
	// protocol.DesktopOrigins() (M2 02 §6.3 item 1). nil or empty means no CORS at all: every Origin passes the CORS
	// step untouched, as in M1.
	DesktopOrigins []string
	// Clock is the API's clock; nil means time.Now.
	Clock func() time.Time
	// Logger is the server logger; the API adds component=api. nil means slog.Default().
	Logger *slog.Logger
}

// authService is the part of *auth.Service that the /api/v1 chain calls. Tests of the chain replace it with a fake;
// the handlers (auth.go) call Deps.Auth directly.
type authService interface {
	Authenticate(r *http.Request) (auth.Principal, error)
	MaybeRotate(w http.ResponseWriter, p auth.Principal)
	CSRF(next http.Handler) http.Handler
}

var _ authService = (*auth.Service)(nil)

// API is 03's REST API under /api/v1 (03 §12). 04's router mounts it at /api/v1/ (RouterOptions.API), behind the
// global chain of 04 §9.3. Its own chain runs, outermost first (03 §12.1, M2 02 §6.4):
//
//  0. CORS (cors.go), first so that every answer of the chain carries it: a request whose Origin is one of
//     Deps.DesktopOrigins has its preflight answered here (204, with the allow headers only for the methods and
//     request headers the desktop SPA uses, cached 600 s), and any other request from such an origin loses its
//     Cookie header and gets Access-Control-Allow-Origin and Access-Control-Expose-Headers. Every response to such an
//     origin carries Vary: Origin, and none ever Access-Control-Allow-Credentials. Other origins pass untouched;
//  1. body limit: 16 KiB for /api/v1/auth/ and /api/v1/device/, 64 KiB elsewhere; a larger Content-Length gets 413
//     payload_too_large at once, and the body reader stops at the limit (DecodeJSON then answers the same);
//  2. no-store: Cache-Control: no-store on every response, errors included, whatever a handler sets;
//  3. route lookup: a path no route matches gets 404 not_found, a known path with another method 405
//     method_not_allowed with an Allow header, both as 03's JSON envelope; a matched route reports its own pattern
//     to 04's RouteObserver and 5xx log lines;
//  4. credentials: a Public route goes on to step 5 without reading the Authorization header. On any other route a
//     request with an Authorization header takes the bearer path: its cookies are dropped unread, there is no CSRF
//     step (only the Content-Type rule of unsafe methods, 415 unsupported_media_type), Auth.Authenticate checks the
//     token (401 invalid_token with WWW-Authenticate, or 500), no rotation, and an Admin or UserSession route then
//     answers 403 forbidden {credential: session}; the handler follows (step 9). A request without the header takes
//     the cookie path, steps 5–8;
//  5. CSRF: the route's unsafe methods (all but GET, HEAD and OPTIONS) pass Auth.CSRF, the cross-origin and
//     Content-Type check of 03 §7.5 (403 csrf_failed, 415 unsupported_media_type);
//  6. authenticate (every route but Public): Auth.Authenticate; its error is the answer (401 unauthenticated);
//  7. rotate: Auth.MaybeRotate;
//  8. access: an Admin route answers a non-admin 403 forbidden, a UserDevice route every cookie 403 forbidden
//     {credential: device};
//  9. the handler, with the principal in its context (PrincipalFrom).
//
// Steps 4–9 are per route, so a route registered with Handle gets them all. Skipping the CSRF check on the header's
// presence is safe (M2 02 §6.4): a cross-site page can send Authorization only after a preflight, only the desktop
// origins get one, and the bearer path reads no cookie, so no ambient credential is ever used.
type API struct {
	d       Deps
	auth    authService
	mux     *http.ServeMux
	handler http.Handler
	// desktop is the set of Deps.DesktopOrigins.
	desktop map[string]struct{}
	// registered is every route of Handle with its access level, in registration order (tests, and nothing else).
	registered []registeredRoute
	// pushTests is the push-test bucket of POST /api/v1/push/test (03 §7.3).
	pushTests *pushTestLimiter
}

// registeredRoute is one route of Handle.
type registeredRoute struct {
	pattern string
	access  Access
}

// New builds the API and registers 03's routes. It panics when Deps.DB or Deps.Auth is nil or Deps.Site.Origin is
// empty, a programming error of the wiring.
func New(d Deps) *API {
	if d.DB == nil {
		panic("httpapi.New: Deps.DB is nil")
	}
	if d.Auth == nil {
		panic("httpapi.New: Deps.Auth is nil")
	}
	if d.Site.Origin == "" {
		panic("httpapi.New: Deps.Site.Origin is empty")
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
	a := &API{d: d, auth: au, mux: http.NewServeMux(), desktop: originSet(d.DesktopOrigins),
		pushTests: newPushTestLimiter(d.Clock)}
	a.handler = a.cors(a.bodyLimit(a.noStore(http.HandlerFunc(a.route))))
	a.routes()
	return a
}

// routes registers 03's endpoints (03 §12.3; the numbers are the rows of its table) with the access of M2 02 §6.4:
// every M1 row but #43, the dashboard, which 04's wiring registers through Handle with DashboardAccounts inside
// (03 §12.6), and the device rows of M2 02 §7.1 (#44–#49, #52; device.go).
func (a *API) routes() {
	a.Handle("GET /api/v1/info", Public, http.HandlerFunc(a.getInfo))                                      // #1
	a.Handle("POST /api/v1/auth/login", Public, http.HandlerFunc(a.postLogin))                             // #2
	a.Handle("POST /api/v1/auth/logout", Public, http.HandlerFunc(a.postLogout))                           // #3
	a.Handle("POST /api/v1/auth/logout-everywhere", User, http.HandlerFunc(a.postLogoutEverywhere))        // #4
	a.Handle("POST /api/v1/auth/register", Public, http.HandlerFunc(a.postRegister))                       // #5
	a.Handle("POST /api/v1/auth/invite/check", Public, http.HandlerFunc(a.postInviteCheck))                // #6
	a.Handle("POST /api/v1/auth/setup/check", Public, http.HandlerFunc(a.postSetupCheck))                  // #7
	a.Handle("POST /api/v1/auth/setup/complete", Public, http.HandlerFunc(a.postSetupComplete))            // #8
	a.Handle("POST /api/v1/auth/reset/check", Public, http.HandlerFunc(a.postResetCheck))                  // #9
	a.Handle("POST /api/v1/auth/reset/complete", Public, http.HandlerFunc(a.postResetComplete))            // #10
	a.Handle("GET /api/v1/me", User, http.HandlerFunc(a.getMe))                                            // #11
	a.Handle("POST /api/v1/me/password", UserSession, http.HandlerFunc(a.postPassword))                    // #12
	a.Handle("POST /api/v1/me/delete", UserSession, http.HandlerFunc(a.postDeleteSelf))                    // #13
	a.Handle("GET /api/v1/me/sessions", User, http.HandlerFunc(a.getSessions))                             // #14
	a.Handle("DELETE /api/v1/me/sessions/{id}", User, http.HandlerFunc(a.deleteSession))                   // #15
	a.Handle("POST /api/v1/me/sessions/revoke-others", UserSession, http.HandlerFunc(a.postRevokeOthers))  // #16
	a.Handle("GET /api/v1/me/devices", User, http.HandlerFunc(a.getDevices))                               // #17
	a.Handle("DELETE /api/v1/me/devices/{id}", User, http.HandlerFunc(a.deleteDevice))                     // #18
	a.Handle("GET /api/v1/rooms", User, http.HandlerFunc(a.getRooms))                                      // #19
	a.Handle("GET /api/v1/invites", User, http.HandlerFunc(a.getInvites))                                  // #21
	a.Handle("POST /api/v1/invites", User, http.HandlerFunc(a.postInvite))                                 // #22
	a.Handle("DELETE /api/v1/invites/{id}", User, http.HandlerFunc(a.deleteInvite))                        // #23
	a.Handle("POST /api/v1/push/subscriptions", UserSession, http.HandlerFunc(a.postPushSubscription))     // #24
	a.Handle("POST /api/v1/push/unsubscribe", User, http.HandlerFunc(a.postPushUnsubscribe))               // #27
	a.Handle("POST /api/v1/push/test", UserSession, http.HandlerFunc(a.postPushTest))                      // #28
	a.Handle("GET /api/v1/admin/users", Admin, http.HandlerFunc(a.getUsers))                               // #29
	a.Handle("PATCH /api/v1/admin/users/{id}", Admin, http.HandlerFunc(a.patchUser))                       // #30
	a.Handle("DELETE /api/v1/admin/users/{id}", Admin, http.HandlerFunc(a.deleteUser))                     // #31
	a.Handle("POST /api/v1/admin/users/{id}/password-reset", Admin, http.HandlerFunc(a.postPasswordReset)) // #32
	a.Handle("POST /api/v1/admin/users/{id}/sign-out", Admin, http.HandlerFunc(a.postSignOut))             // #33
	a.Handle("GET /api/v1/admin/approvals", Admin, http.HandlerFunc(a.getApprovals))                       // #34
	a.Handle("POST /api/v1/admin/approvals/{id}/approve", Admin, http.HandlerFunc(a.postApprove))          // #35
	a.Handle("POST /api/v1/admin/approvals/{id}/reject", Admin, http.HandlerFunc(a.postReject))            // #36
	a.Handle("POST /api/v1/admin/rooms", Admin, http.HandlerFunc(a.postRoom))                              // #37
	a.Handle("PATCH /api/v1/admin/rooms/{id}", Admin, http.HandlerFunc(a.patchRoom))                       // #38
	a.Handle("DELETE /api/v1/admin/rooms/{id}", Admin, http.HandlerFunc(a.deleteRoom))                     // #39
	a.Handle("GET /api/v1/admin/settings", Admin, http.HandlerFunc(a.getSettings))                         // #40
	a.Handle("PATCH /api/v1/admin/settings", Admin, http.HandlerFunc(a.patchSettings))                     // #41
	a.Handle("GET /api/v1/admin/audit", Admin, http.HandlerFunc(a.getAudit))                               // #42
	a.Handle("POST /api/v1/device/code", Public, http.HandlerFunc(a.postDeviceCode))                       // #44
	a.Handle("POST /api/v1/device/token", Public, http.HandlerFunc(a.postDeviceToken))                     // #45
	a.Handle("POST /api/v1/device/lookup", UserSession, http.HandlerFunc(a.postDeviceLookup))              // #46
	a.Handle("POST /api/v1/device/decide", UserSession, http.HandlerFunc(a.postDeviceDecide))              // #47
	a.Handle("POST /api/v1/device/password", Public, http.HandlerFunc(a.postDevicePassword))               // #48
	a.Handle("POST /api/v1/device/revoke", UserDevice, http.HandlerFunc(a.postDeviceRevoke))               // #49
	a.Handle("GET /api/v1/push/preferences", User, http.HandlerFunc(a.getPushPreferences))                 // #50
	a.Handle("PUT /api/v1/push/preferences", User, http.HandlerFunc(a.putPushPreferences))                 // #51
	a.Handle("PATCH /api/v1/me/devices/{id}", User, http.HandlerFunc(a.patchDevice))                       // #52
}

// ServeHTTP serves every request under /api/v1/ through the API's chain. After the no-store step it answers every
// unmatched /api/v1 path with 404 not_found and every method mismatch with 405 method_not_allowed plus an Allow
// header, through WriteError. This covers routes added through Handle. 04's router handles only /api/ paths outside
// /api/v1/.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.handler.ServeHTTP(w, r) }

// Handle registers a route owned by another doc behind the same /api/v1 chain (CORS, body limit, no-store, the
// credential step, CSRF, auth, rotation, access), e.g. a.Handle("POST /api/v1/admin/doctor", httpapi.Admin, h). An
// Admin route refuses every bearer, an admin's included (403 forbidden {credential: session}, M2 02 §6.4). Patterns
// use Go 1.22+ ServeMux syntax without a host, and their path starts with /api/v1/. A GET pattern also serves HEAD.
// Like http.ServeMux.Handle, it panics on an invalid or conflicting pattern; it also panics on a path outside
// /api/v1/, an unknown Access or a nil handler.
func (a *API) Handle(pattern string, access Access, h http.Handler) {
	if access > UserDevice {
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
	a.mux.Handle(pattern, a.credentials(access, h))
	a.registered = append(a.registered, registeredRoute{pattern: pattern, access: access})
}

type principalKey struct{}

// PrincipalFrom returns the authenticated caller of a route that is not Public: a web session (auth.MethodSession)
// or a device (auth.MethodBearer). It reports false on Public routes and outside the API.
func PrincipalFrom(ctx context.Context) (auth.Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(auth.Principal)
	return p, ok
}

// opErr is the answer for a failed store call of a handler that works on the store itself (rooms, push): an
// *api.Error that came out of the transaction goes out as it is, and anything else (the store, a cancelled request)
// is wrapped with what was being done, for the log line of the 500 that WriteError makes of it.
func opErr(op string, err error) error {
	var ae *api.Error
	if errors.As(err, &ae) {
		return err
	}
	return fmt.Errorf("httpapi: %s: %w", op, err)
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
//
// The router resolved the request's route pattern as "/api/v1/" (its own mount). A matched request reports the API's
// pattern instead ("GET /api/v1/info"), so 04's per-route metrics and the 5xx log lines tell the API's routes apart:
// the transfer and recover steps read the shared request state after this returns. The 404 and 405 keep "/api/v1/",
// so the label set stays bounded by the registered patterns.
func (a *API) route(w http.ResponseWriter, r *http.Request) {
	h, pattern := a.mux.Handler(r)
	if pattern != "" {
		if st := stateOf(r); st != nil {
			st.route = pattern
		}
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

// credentials is step 4 (M2 02 §6.4), which replaces M1's rejection of every Authorization header. A Public route
// reads no credential here: it goes to the CSRF step whatever its headers. On any other route the presence of an
// Authorization header, whatever its value, chooses the bearer path (bearerAuth); its absence the cookie path
// (CSRF, then cookieAuth).
func (a *API) credentials(access Access, next http.Handler) http.Handler {
	if access == Public {
		return a.auth.CSRF(next)
	}
	bearer := a.bearerAuth(access, next)
	cookie := a.auth.CSRF(a.cookieAuth(access, next))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["Authorization"]; ok {
			bearer.ServeHTTP(w, r)
			return
		}
		cookie.ServeHTTP(w, r)
	})
}

// bearerAuth is the bearer path of step 4 (M2 02 §6.1, §6.4) for a route that is not Public. In order:
//
//  1. the request's Cookie header is dropped: a bearer request never reads a cookie, neither here nor in the
//     handler, so the cross-origin half of the CSRF check has nothing to protect and is skipped;
//  2. the Content-Type rule of 03 §7.5 still holds for unsafe methods: exactly one application/json (a charset only
//     if utf-8), else 415 unsupported_media_type. It comes before the token, as the CSRF step comes before the
//     cookie on the cookie path;
//  3. Auth.Authenticate checks the header's grammar and the token: 401 invalid_token (WriteError adds
//     WWW-Authenticate), or another error as it is (500 internal). A principal that is not a device's is
//     invalid_token too (Authenticate never returns one for a request with the header);
//  4. an Admin or UserSession route answers the valid token 403 forbidden {credential: session}, after the token
//     checked out, so that a bad token is still 401;
//  5. the handler, with the device's principal. There is no rotation: MaybeRotate does nothing for a device.
func (a *API) bearerAuth(access Access, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = withoutCookies(r)
		if !isSafeMethod(r.Method) && !jsonContentTypeOK(r.Header.Values("Content-Type")) {
			WriteError(w, r, api.NewError(api.CodeUnsupportedMediaType))
			return
		}
		p, err := a.auth.Authenticate(r)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		if p.Method != auth.MethodBearer {
			WriteError(w, r, api.NewError(api.CodeInvalidToken))
			return
		}
		if access == Admin || access == UserSession {
			WriteError(w, r, credentialRequired(api.CredentialKindSession))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

// cookieAuth is steps 6–8, the cookie path of a route that is not Public: authenticate (401 unauthenticated), rotate,
// then the access check. An Admin route answers a non-admin 403 forbidden, a UserDevice route every cookie 403
// forbidden {credential: device} (M2 02 §6.4); both come after the rotation, which a refused request keeps.
func (a *API) cookieAuth(access Access, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := a.auth.Authenticate(r)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		if p.Method != auth.MethodSession {
			// Authenticate never returns a device for a request without the header.
			WriteError(w, r, api.NewError(api.CodeUnauthenticated))
			return
		}
		a.auth.MaybeRotate(w, p)
		switch {
		case access == Admin && !p.IsAdmin():
			WriteError(w, r, api.NewError(api.CodeForbidden))
			return
		case access == UserDevice:
			WriteError(w, r, credentialRequired(api.CredentialKindDevice))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

// credentialRequired is 403 forbidden with params {credential}: the only credential the route takes (M2 02 §6.4).
func credentialRequired(kind api.CredentialKind) *api.Error {
	return &api.Error{Code: api.CodeForbidden, Params: map[string]any{api.ParamCredential: string(kind)}}
}

// withoutCookies returns r without its Cookie header, as a copy when it has one: the request the outer steps hold
// keeps its headers.
func withoutCookies(r *http.Request) *http.Request {
	if _, ok := r.Header["Cookie"]; !ok {
		return r
	}
	r = r.Clone(r.Context())
	r.Header.Del("Cookie")
	return r
}

// isSafeMethod reports whether a method never changes state (03 §7.5; http.CrossOriginProtection's safe methods).
func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// jsonContentTypeOK is the Content-Type rule of 03 §7.5 for unsafe requests: exactly one Content-Type header,
// application/json, with a charset only if utf-8 (auth's CSRFGuard applies the same rule on the cookie path).
func jsonContentTypeOK(values []string) bool {
	return len(values) == 1 && isJSONRequest(values[0])
}
