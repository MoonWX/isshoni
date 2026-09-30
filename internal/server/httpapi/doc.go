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
// Imports (04 §2): config, logx, version, store, auth and internal/protocol(/api); never ops, push, netx, tlsmgr,
// signal, sfu or sfuplane, which reach the router through small interfaces declared here (RouteObserver) or through
// the wiring.
package httpapi
