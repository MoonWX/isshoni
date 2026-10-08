// Package httpapi is the HTTP side of the server: 04's router and helpers, and 03's /api/v1 (docs/m1/04 §9,
// docs/m1/03 §12).
//
// The router part (router.go, middleware.go, realip.go, headers.go, errors.go, json.go, spa.go, mime.go; 04 §9):
//   - Router: an http.ServeMux with the SPA at "/", 03's API at /api/v1/ and 01's hub at GET /ws, wrapped in the
//     global chain of 04 §9.3, outermost first: recover → request ID → read deadline → transfer/metrics → shutdown
//     gate → Host check → real IP → security headers. Reserved paths no route handles (/api/…, /ws, /healthz,
//     /readyz) answer JSON 404 not_found, never index.html.
//   - Helpers every handler uses (01, 03, 04): ClientIP and IsSecure (trusted proxies in off mode only, 04 §8.5),
//     RequestID, WriteJSON, WriteError (03's envelope; api.StatusOf) and DecodeJSON (Content-Type, size limit,
//     exactly one JSON value).
//   - The SPA handler: hashed assets immutable, index.html and other root files no-cache with ETags, .br/.gz
//     siblings by Accept-Encoding, 404 for missing assets and dotted paths, index.html for every other GET (with
//     03's SPAStatus hook), and a 503 "web UI not built" page when the binary has no web/dist.
//   - Security headers of 04 §9.6: the HTML policy for HTML and /sw.js, a deny-all policy for everything else.
//
// No access log: only 5xx responses log one line (route pattern, status, request ID), and a panic or an internal
// error logs its cause instead. URLs, queries and bodies are never logged.
//
// The API part (api.go, deps.go, info.go, auth.go, me.go, invites.go, admin_users.go, admin_settings.go; 03 §12):
//   - API: 03's REST API, mounted by the router at /api/v1/. Its chain (03 §12.1): body limit (16 KiB for auth and
//     device endpoints, 64 KiB elsewhere) → no-store on every response → route lookup (JSON 404 not_found, 405
//     method_not_allowed with Allow; a matched route reports its own pattern to the RouteObserver) → any
//     Authorization header is 401 unauthenticated (no bearer tokens in M1, 03 §7.5) → CSRF (auth.Service.CSRF,
//     03 §7.5) → authenticate (User and Admin routes) → MaybeRotate → access check → handler. API.Handle registers
//     other docs' routes behind the same chain, with an Access level (Public, User, Admin); PrincipalFrom gives a
//     handler its caller.
//   - The interfaces the wiring implements (04 §6.6): Signal (01's hub), Push (04's push service) and InfoSource
//     (04's build information), each declared in full; nil Signal and Info have built-in defaults, nil Push means
//     push is off.
//   - GET /api/v1/info (03 §12.4.1).
//   - Sessions and setup (README S30; 03 §12.4.2–12.4.3): POST /api/v1/auth/login and /auth/logout,
//     POST /api/v1/auth/setup/check and /auth/setup/complete, and GET /api/v1/me. The handlers decode the request,
//     call auth.Service (which owns the rules, the throttles and the audit rows), set or clear the session cookie
//     and tell the user's other tabs through Signal.Notify.
//   - Invites, registration, approvals and settings (README S42; 03 §7.9, §9, §12.4.2, §12.4.5, §12.4.8):
//     POST /api/v1/auth/register and /auth/invite/check; GET and POST /api/v1/invites and
//     DELETE /api/v1/invites/{id} (admins: every invite; members: their own, while membersCanInvite is on);
//     GET /api/v1/admin/approvals and POST /api/v1/admin/approvals/{id}/approve and /reject, with the reject-all
//     form (all in place of the ID plus the body {"all": true}); GET and PATCH /api/v1/admin/settings. The rules,
//     limits, audit rows and admin alerts are auth.Service's and the settings cache's; the lists are reads of the
//     store. After a change the handlers notify the SPAs by the topic table of 03 §12.5.
//
// The other endpoints of 03 §12.3, and DashboardAccounts, come with the later account slices.
//
// Imports (04 §2): config, logx, version, store, auth and internal/protocol(/api); never ops, push, netx, tlsmgr,
// signal, sfu or sfuplane, which reach this package through small interfaces declared here (RouteObserver, Signal,
// Push, InfoSource) that the wiring implements.
package httpapi
