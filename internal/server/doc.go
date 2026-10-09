// Package server is the isshoni server itself: it builds every component from the config, runs them and stops them
// in order (docs/m1/04-server-platform.md §6). cmd/isshoni's serve command and the servertest harness are its only
// callers.
//
// Lifecycle (04 §6.1–§6.4):
//   - New checks the config and builds the Server. It does no I/O and starts nothing.
//   - Start runs the startup sequence: the config warnings go to the log, the umask and the data directory are made
//     private, the data-directory lock is taken, secrets.json is opened (created on first run), 03's store is opened
//     (migrated after a backup of its own) and the policy keys of the config are pinned into its settings, the
//     public addresses are detected, the listeners, the admin socket and the media sockets are bound, the components
//     are built and the servers start. An error that a restart can't fix (a container without a data volume, a data
//     directory the process can't write, a corrupt or wrongly owned secrets.json, a database from a newer isshoni,
//     an invalid config) satisfies NeedsOperator: cmd/isshoni prints it and exits 78, so systemd stops retrying
//     (04 §6.3).
//   - Run is Start (unless it was called), then waiting for the context to end, and then the graceful shutdown.
//     Its result tells cmd/isshoni how the server ended: stopped (nil, or ErrShutdownForced: exit 0), to be
//     restarted (ErrRestartRequested), or failed.
//   - Shutdown stops the server in the order of 04 §6.4: readiness and liveness off, the shutdown gate (503
//     server_shutdown for every new request), the hub (server.shutdown to every connection, close 1012), the SFU
//     and then the ICE Transport under it, then the HTTP servers and the 443 multiplexer, the TLS manager, the admin
//     socket, the store, and last the data-directory lock. It finishes within shutdown_timeout (10 s); what is
//     still open then is closed by force, and its error says so with ErrShutdownForced.
//
// What runs in the server (README slices S31, S44, S54 and S59; 04 slices S4, S7, W1 and W2):
//   - The HTTP side of every TLS mode of 04 §8.1. httpapi's router serves the embedded web app with its fallback,
//     GET /healthz and GET /readyz from ops.Health, 03's REST API at /api/v1/ and 01's hub at GET /ws.
//     In off mode the router is on plain HTTP on listen.http, behind the operator's HTTPS proxy, or on localhost for
//     development (04 §8.5). In auto, ip and manual mode (tls.go) it is over TLS behind netx.PortMux on
//     listen.https, with the certificate of tlsmgr.Manager; ICE-TCP shares that port by its first byte (04 §7.2),
//     and listen.http is the plain port of 04 §8.3: ACME http-01 challenges, then the redirect to HTTPS. The
//     certificate arrives in the background; until it is there the server runs and the readiness check "tls" fails.
//   - The public addresses (04 §7.4, public.go), detected at startup in every mode and looked at again every ten
//     minutes. They are what the server's ICE candidates name, and ip mode's site (manual mode's too, without a
//     domain). A server whose site is its address and that finds none runs without a site, and the check
//     "public_ip" fails; it is the one server that restarts by itself, when a later look finds the address (Run
//     returns ErrRestartRequested). It starts even on a machine whose network is not up yet, which then has no
//     local address for the media sockets either: without them and without an SFU, and the check "media" fails
//     too. Everywhere else an address that changed is logged, and the operator's restart applies it; and a
//     machine without an address for media ends the start with an error that says so.
//   - Accounts and signaling (wire.go, the wiring of 04 §6.6): 03's store and account service, 03's REST API, and
//     01's hub, with the adapters between them: the hub authenticates through the session cookie and reads rooms
//     and policy from the store; a revocation in the account service closes the hub's connections at once; the REST
//     handlers reach the hub for presence and notifications. The readiness checks "db" and "signal" belong to them.
//   - Media: netx's Transport, bound in Start with the other listeners (listen.ice_udp on every local address that
//     carries media, listen.ice_tcp, and the ICE side of the 443 multiplexer), and 02's SFU on it. 01's sfuplane is
//     the hub's MediaPlane in front of the SFU, so share.start, the pc.* messages and subscribe.update reach it;
//     the admin's bitrate limit follows 03's settings live. The readiness check "media" is theirs.
//   - The admin socket of 04 §12 on listen.admin_socket, for the operator's CLI (setup-url, healthcheck, admin …),
//     with its accounts part over 03's service.
//
// The later slices of the M1 plan extend Start and Shutdown at the marked steps: restore and secret rotation with
// the re-exec (S65), push (S71), the ops data, metrics and their REST routes (S80, S85) and the SIGHUP reload (S90).
//
// Of the packages of 04 §2, only this one, cmd/isshoni and servertest import signal, sfu and sfuplane, or combine
// store and auth with ops and push.
package server
