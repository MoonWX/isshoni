// Package ops is the operator's side of the server (docs/m1/04-server-platform.md §11–§12): health, metrics,
// transfer accounting, the admin dashboard, the release check and the admin socket.
//
// What exists so far:
//   - health.go (README slice S31): Health, the liveness and readiness state behind GET /healthz and GET /readyz
//     (04 §6.2, §11.1). The wiring registers one named check per component (db, tls, media, signal, public_ip) and
//     calls SetShuttingDown as the first step of a graceful shutdown (04 §6.4).
//   - The admin socket (README slice S43, 04 §12.1–§12.2, §12.5): HTTP/1.1 over a unix socket for the operator's
//     CLI. adminsock.go has ListenAdmin (the socket file, mode 0600, and the peer-credential check of every
//     connection: peercred_linux.go, peercred_darwin.go) and AdminServer with its lifecycle; adminapi.go has the
//     endpoints and their wire documents; adminclient.go has AdminClient (DialAdmin), which classifies "not running"
//     and "permission denied" for the CLI and doctor (connerr*.go has the error numbers behind the two, which
//     Windows names differently). Accounts reach the socket through the AdminAccounts interface, which the wiring
//     implements over 03's auth service. loglevel.go has LogLevel, the runtime log level behind POST /v1/log-level.
//   - The ops data (README slice S55, 04 §11.2, §11.3, §11.5). metrics.go has Metrics: the server's private
//     Prometheus registry, which every component registers on through Registerer whether or not metrics are
//     served, the series that describe the server as a whole, the router's RouteObserver, and the handler and
//     lifecycle of the listener on metrics.listen. transfer.go has Transfer: it moves the bytes that netx counts at
//     the sockets into 03's monthly totals every 60 s, at each month's end and at shutdown, and alerts the admins
//     at 80 % and 100 % of the setting transferAlertGb, each once per month. release.go has ReleaseCheck: the daily
//     look at the project's releases on GitHub, which an admin can switch off and which sends one GET and nothing
//     else. Transfer and ReleaseCheck each give the dashboard, `admin status` and doctor their part (Info, Update)
//     and their alerts (Alerts). sources.go declares what they need from the rest of the server: Policy (the
//     settings, read on every tick), MetaStore, AdminAlerter, and for the slices to come the dashboard's
//     LiveSource and AccountsSource and the connection test's Prober.
//     Nothing in the server builds these three yet: the wiring does, in the steps their files describe, when it
//     gets the metrics listener and the dashboard (README S85); until then `serve` only warns that
//     metrics.enabled has no listener.
//   - doctor inside the server (README slice S60, 04 §13). The checks themselves are the subpackage ops/doctor,
//     which `isshoni doctor` also runs by itself while the server is stopped. doctorrun.go has Doctor, which runs
//     them in the server's process: 20 s after the start and every 24 h, with the last report kept in 03's meta
//     table for the dashboard (Summary, Alerts); on demand for the admin page (Check and Last, the typed functions
//     of POST and GET /api/v1/admin/doctor, with 429 doctor_busy for a second run within 10 s); and for the CLI
//     through the admin socket's POST /v1/doctor (RunFor). Bandwidth and BandwidthFromQuery are the typed
//     functions of GET /api/v1/admin/bandwidth. The wiring builds the Doctor with a function that returns the
//     doctor.Env of one run (the config, the server's status, 03's schema version), hands it to AdminOptions, runs
//     Run, and registers the three REST routes (README S85); until it does, POST /v1/doctor answers that this
//     server has no doctor.
//
// A known limit: only Linux and macOS check peer credentials. The experimental Windows build (06 §3) serves every
// connection to the admin socket, and file modes don't protect the socket there, so every local user who can open
// it can administer the server; ListenAdmin logs a warning that says so (peercred_other.go). Checking the peer's
// process token there, or refusing every connection, is an open decision.
//
// The socket's whole surface is declared: /v1/backup, /v1/restore and /v1/rotate-secrets answer "not implemented
// in this build yet" until their slice fills them in (README S65), and the client already has their methods.
//
// The later ops slices add dashboard.go, conntest.go and backup.go/restore.go.
//
// Imports (04 §2): config, logx, version, netx and internal/protocol(/api), plus ops/doctor; never httpapi, store,
// auth, signal, sfu or sfuplane. The wiring (internal/server) adapts those to the small interfaces declared here,
// so this package compiles and tests without them.
package ops
