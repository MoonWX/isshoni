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
// The API part (api.go, cors.go, deps.go, info.go, auth.go, me.go, account.go, device.go, rooms.go, invites.go,
// push.go, admin_users.go, admin_rooms.go, admin_settings.go, admin_audit.go, accounts.go; 03 §12, M2 02 §6.4, §7):
//   - API: 03's REST API, mounted by the router at /api/v1/. Its chain (03 §12.1, M2 02 §6.4): CORS for the desktop
//     app's origins (Deps.DesktopOrigins: preflights answered, cached 600 s, the Cookie header dropped, never
//     Access-Control-Allow-Credentials) → body limit (16 KiB for auth and device endpoints, 64 KiB elsewhere) →
//     no-store on every response → route lookup (JSON 404 not_found, 405 method_not_allowed with Allow; a matched
//     route reports its own pattern to the RouteObserver) → credentials: a Public route reads none; on any other
//     route an Authorization header takes the bearer path (no cookie, no cross-origin check, the Content-Type rule,
//     auth.Service.Authenticate: 401 invalid_token with WWW-Authenticate; an Admin or UserSession route then 403
//     forbidden {credential: session}), its absence the cookie path: CSRF (auth.Service.CSRF, 03 §7.5) →
//     authenticate → MaybeRotate → access check (Admin: an admin; UserDevice: 403 forbidden {credential: device}) →
//     handler. API.Handle registers other docs' routes behind the same chain, with an Access level (Public, User,
//     Admin, UserSession, UserDevice); PrincipalFrom gives a handler its caller, a web session or a device.
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
//   - Rooms (README S53; 03 §8, §12.4.4): GET /api/v1/rooms for every signed-in user, with showRoomList (more
//     than one room exists) and each room's live counts from Signal.RoomPresence; POST /api/v1/admin/rooms and
//     PATCH and DELETE /api/v1/admin/rooms/{id} for admins. The room rules live in this package, not in
//     auth.Service: names are PRECIS Nickname of 1–40 characters and unique whatever their case, at most 200
//     rooms, and the default room is renamed but never deleted. Each change is one Write with its audit row
//     (room.created, room.renamed, room.deleted); after the commit a delete calls Signal.RoomDeleted, and every
//     change notifies {All}: rooms.
//   - Push subscriptions and preferences (README S53; 03 §12.4.6): POST /api/v1/push/subscriptions (the body's
//     shape is checked here, every URL and host rule by Push.ValidateEndpoint; the row is upserted by endpoint,
//     bound to the caller's web session, and a user keeps the 10 newest), POST /api/v1/push/unsubscribe,
//     POST /api/v1/push/test (this session's subscriptions through Push.SendTest, one per 10 s per user: the
//     push-test bucket of 03 §7.3, kept in this package) and GET and PUT /api/v1/push/preferences. With push off
//     (no Deps.Push, or one without a VAPID key) subscribe and test answer 503 push_unavailable; the caller's
//     stored data stays reachable. 04's sender reads the same rows through the store.
//   - Self-service (README S58; 03 §7.7, §12.4.3; account.go): GET /api/v1/me/sessions (this browser first and
//     marked current) and DELETE /api/v1/me/sessions/{id}, POST /api/v1/me/sessions/revoke-others,
//     POST /api/v1/auth/logout-everywhere, POST /api/v1/me/password (the session's cookie is replaced by a new
//     token), POST /api/v1/me/delete, and GET /api/v1/me/devices and DELETE /api/v1/me/devices/{id}. auth.Service
//     owns each change, with the revocation row of 03 §7.7 that goes with it; the handlers set or clear the cookie,
//     as the response's only Set-Cookie (never for a device), and notify {UserID}: devices, or {Admins}: admin.users
//     after an account was deleted. The account changes are UserSession routes (M2 02 §6.4); a device's bearer
//     reaches the rest, and GET /api/v1/me answers it with its device in place of the session.
//   - Devices (README WA47; M2 02 §5, §6.2, §7, §14; device.go): the device flow, POST /api/v1/device/code (Public),
//     /device/lookup and /device/decide (UserSession), both grants of POST /api/v1/device/token (Public), the
//     password fallback POST /api/v1/device/password (Public), the app's sign-out POST /api/v1/device/revoke
//     (UserDevice) and the rename PATCH /api/v1/me/devices/{id} (User). auth.Service owns every rule, throttle,
//     audit row, connection close and alert; the handlers notify {UserID}: devices after a link, a rename, a revoke
//     and a refresh token's reuse (auth.RefreshReusedError).
//   - Admin users and password resets (README S64; 03 §7.10–7.11, §12.4.8; admin_users.go, auth.go):
//     GET /api/v1/admin/users (every account, with ?status= for one status and online from Signal.OnlineUserIDs),
//     PATCH /api/v1/admin/users/{id} (rename, role, status), DELETE /api/v1/admin/users/{id},
//     POST /api/v1/admin/users/{id}/password-reset (201 with the one-time link) and
//     POST /api/v1/admin/users/{id}/sign-out; and the public side of a reset link, POST /api/v1/auth/reset/check
//     and POST /api/v1/auth/reset/complete (200 with the session cookie). auth.Service owns the last-admin and self
//     rules, the password an admin types again, the revocation rows and the admin alerts. The handlers notify
//     {Admins}: admin.users, and after a request that names a username or a role also call Signal.UserChanged
//     and notify {UserID}: me.
//   - The audit log and the dashboard's accounts (README S70; 03 §10, §12.4.8; admin_audit.go, accounts.go):
//     GET /api/v1/admin/audit, the audit rows newest first, filtered by an action prefix, the acting user and the
//     target, and paged by the id cursor (?before, with nextBefore null on the last page); and
//     API.DashboardAccounts, the accounts object that 04's GET /api/v1/admin/dashboard embeds: the accounts by
//     status, the active admins, invites and sessions, the linked devices, the registration mode and the security
//     events of the last 7 days. Both read only; the rows are written by the changes they describe.
//
// Imports (04 §2): config, logx, version, store, auth and internal/protocol(/api); never ops, push, netx, tlsmgr,
// signal, sfu or sfuplane, which reach this package through small interfaces declared here (RouteObserver, Signal,
// Push, InfoSource) that the wiring implements.
package httpapi
