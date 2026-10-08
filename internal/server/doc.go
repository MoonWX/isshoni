// Package server is the isshoni server itself: it builds every component from the config, runs them and stops them
// in order (docs/m1/04-server-platform.md §6). cmd/isshoni's serve command and the servertest harness are its only
// callers.
//
// Lifecycle (04 §6.1–§6.4):
//   - New checks the config and builds the Server. It does no I/O and starts nothing.
//   - Start runs the startup sequence: the config warnings go to the log, the umask and the data directory are made
//     private, the data-directory lock is taken, secrets.json is opened (created on first run), 03's store is opened
//     (migrated after a backup of its own) and the policy keys of the config are pinned into its settings, the
//     listeners and the admin socket are bound, the components are built and the servers start. An error that a
//     restart can't fix (a container without a data volume, a data directory the process can't write, a corrupt or
//     wrongly owned secrets.json, a database from a newer isshoni, an invalid config) satisfies NeedsOperator:
//     cmd/isshoni prints it and exits 78, so systemd stops retrying (04 §6.3).
//   - Run is Start (unless it was called), then waiting for the context to end, and then the graceful shutdown.
//     Its result tells cmd/isshoni how the server ended: stopped (nil, or ErrShutdownForced: exit 0), to be
//     restarted (ErrRestartRequested), or failed.
//   - Shutdown stops the server in the order of 04 §6.4: readiness and liveness off, the shutdown gate (503
//     server_shutdown for every new request), the hub (server.shutdown to every connection, close 1012), then the
//     HTTP servers and the 443 multiplexer, the TLS manager, the admin socket, the store, and last the
//     data-directory lock. It finishes within shutdown_timeout (10 s); what is still open then is closed by force,
//     and its error says so with ErrShutdownForced.
//
// What runs in the server (README slices S31, S44 and S54; 04 slices S4, S7 and W1):
//   - The HTTP side of every TLS mode of 04 §8.1. httpapi's router serves the embedded web app with its fallback,
//     GET /healthz and GET /readyz from ops.Health, 03's REST API at /api/v1/ and 01's hub at GET /ws.
//     In off mode the router is on plain HTTP on listen.http, behind the operator's HTTPS proxy, or on localhost for
//     development (04 §8.5). In auto, ip and manual mode (tls.go) it is over TLS behind netx.PortMux on
//     listen.https, with the certificate of tlsmgr.Manager; ICE-TCP shares that port by its first byte (04 §7.2),
//     and listen.http is the plain port of 04 §8.3: ACME http-01 challenges, then the redirect to HTTPS. The
//     certificate arrives in the background; until it is there the server runs and the readiness check "tls" fails.
//     ip mode's site is the public address that Start detects (04 §7.4, public.go); without one the server runs
//     without a site and the check "public_ip" fails.
//   - Accounts and signaling (wire.go, the wiring of 04 §6.6): 03's store and account service, 03's REST API, and
//     01's hub, with the adapters between them: the hub authenticates through the session cookie and reads rooms
//     and policy from the store; a revocation in the account service closes the hub's connections at once; the REST
//     handlers reach the hub for presence and notifications. The readiness checks "db" and "signal" belong to them.
//     The hub has no media yet: connections join rooms and see each other, and what needs the SFU is refused.
//   - The admin socket of 04 §12 on listen.admin_socket, for the operator's CLI (setup-url, healthcheck, admin …),
//     with its accounts part over 03's service.
//
// The later slices of the M1 plan extend Start and Shutdown at the marked steps: the SFU on netx's Transport, which
// takes over the multiplexer's ICE side (S59), restore and secret rotation with the re-exec (S65), push (S71), the
// ops data and its REST routes (S80, S85) and the SIGHUP reload (S90).
//
// Of the packages of 04 §2, only this one, cmd/isshoni and servertest import signal, sfu and sfuplane, or combine
// store and auth with ops and push.
package server
