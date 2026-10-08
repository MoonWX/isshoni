// Package server is the isshoni server itself: it builds every component from the config, runs them and stops them
// in order (docs/m1/04-server-platform.md §6). cmd/isshoni's serve command and the servertest harness are its only
// callers.
//
// Lifecycle (04 §6.1–§6.4):
//   - New checks the config and builds the Server. It does no I/O and starts nothing.
//   - Start runs the startup sequence: the config warnings go to the log, the umask and the data directory are made
//     private, the data-directory lock is taken, secrets.json is opened (created on first run), the listeners are
//     bound and the HTTP server starts. An error that a restart can't fix (a container without a data volume, a
//     data directory the process can't write, a corrupt or wrongly owned secrets.json, an invalid config) satisfies
//     NeedsOperator: cmd/isshoni prints it and exits 78, so systemd stops retrying (04 §6.3).
//   - Run is Start (unless it was called), then waiting for the context to end, and then the graceful shutdown.
//     Its result tells cmd/isshoni how the server ended: stopped (nil, or ErrShutdownForced: exit 0), to be
//     restarted (ErrRestartRequested), or failed.
//   - Shutdown stops the server in the order of 04 §6.4: readiness and liveness off, the shutdown gate (503
//     server_shutdown for every new request), then the components, then the HTTP servers and the 443 multiplexer,
//     the TLS manager, and last the data-directory lock. It finishes within shutdown_timeout (10 s); what is still
//     open then is closed by force, and its error says so with ErrShutdownForced.
//
// What exists so far (README slices S31 and S44; 04 slices S4 and S7): the HTTP side of every TLS mode of 04 §8.1.
// httpapi's router serves the embedded web app with its fallback, GET /healthz and GET /readyz from ops.Health, and
// JSON 404 for /api/… and /ws until the wiring mounts 03's API and 01's hub there.
//   - off: the router on plain HTTP on listen.http, behind the operator's HTTPS proxy, or on localhost for
//     development (04 §8.5).
//   - auto, ip, manual (tls.go): the router over TLS behind netx.PortMux on listen.https, with the certificate of
//     tlsmgr.Manager; ICE-TCP shares that port by its first byte (04 §7.2). listen.http is the plain port of 04
//     §8.3: ACME http-01 challenges, then the redirect to HTTPS. The certificate arrives in the background; until
//     it is there the server runs and the readiness check "tls" fails. ip mode's site is the public address that
//     Start detects (04 §7.4); without one the server runs without a site and the check "public_ip" fails.
//
// The later slices of the M1 plan extend Start and Shutdown at the marked steps: the admin socket (S43), the
// store, auth, the REST API and the hub with the adapters of 04 §6.6 in wire.go (S54), the SFU on netx's Transport,
// which takes over the multiplexer's ICE side (S59), restore and secret rotation with the re-exec (S65), push (S71)
// and the SIGHUP reload of manual certificates (S90).
//
// Of the packages of 04 §2, only this one, cmd/isshoni and servertest import signal, sfu and sfuplane, or combine
// store and auth with ops and push.
package server
