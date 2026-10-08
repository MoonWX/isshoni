// Package ops is the operator's side of the server (docs/m1/04-server-platform.md §11–§12): health, metrics,
// transfer accounting, the admin dashboard, the release check and the admin socket.
//
// What exists so far (README slice S31, the health part of 04 slice S4):
//   - health.go: Health, the liveness and readiness state behind GET /healthz and GET /readyz (04 §6.2, §11.1).
//     The wiring registers one named check per component (db, tls, media, signal, public_ip) and calls
//     SetShuttingDown as the first step of a graceful shutdown (04 §6.4).
//
// The later ops slices add metrics.go, transfer.go, dashboard.go, release.go, conntest.go, the admin socket
// (adminsock.go, adminapi.go, adminclient.go) and backup.go/restore.go.
//
// Imports (04 §2): config, logx, version, netx and internal/protocol(/api), plus ops/doctor; never httpapi, store,
// auth, signal, sfu or sfuplane. The wiring (internal/server) adapts those to the small interfaces declared here,
// so this package compiles and tests without them.
package ops
