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
//
// A known limit: only Linux and macOS check peer credentials. The experimental Windows build (06 §3) serves every
// connection to the admin socket, and file modes don't protect the socket there, so every local user who can open
// it can administer the server; ListenAdmin logs a warning that says so (peercred_other.go). Checking the peer's
// process token there, or refusing every connection, is an open decision.
//
// The socket's whole surface is declared: /v1/backup, /v1/restore, /v1/rotate-secrets and /v1/doctor answer "not
// implemented in this build yet" until their slices fill them in (README S60, S65), and the client already has
// their methods.
//
// The later ops slices add metrics.go, transfer.go, dashboard.go, release.go, conntest.go and backup.go/restore.go.
//
// Imports (04 §2): config, logx, version, netx and internal/protocol(/api), plus ops/doctor; never httpapi, store,
// auth, signal, sfu or sfuplane. The wiring (internal/server) adapts those to the small interfaces declared here,
// so this package compiles and tests without them.
package ops
