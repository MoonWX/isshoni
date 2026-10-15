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
//     a restore that only the operator can complete, an invalid config) satisfies NeedsOperator: cmd/isshoni
//     prints it and exits 78, so systemd stops retrying (04 §6.3).
//   - Run is Start (unless it was called), then waiting for the context to end, and then the graceful shutdown.
//     Its result tells cmd/isshoni how the server ended: stopped (nil, or ErrShutdownForced: exit 0), to be
//     restarted (ErrRestartRequested), or failed.
//   - Shutdown stops the server in the order of 04 §6.4: readiness and liveness off, the shutdown gate (503
//     server_shutdown for every new request), the hub (server.shutdown to every connection, close 1012), the SFU
//     and then the ICE Transport under it, then the HTTP servers (the metrics listener's too) and the 443
//     multiplexer, the TLS manager and the last flush of the transfer counters while the push service sends what is
//     queued (for at most 2 s, from the moment the HTTP servers are down), the admin socket, the store, and last the
//     data-directory lock. It finishes within shutdown_timeout (10 s); what is still open then is closed by force,
//     and its error says so with ErrShutdownForced.
//
// What runs in the server (README slices S31, S44, S54, S59, S71, S80, S85 and S90; 04 slices S4, S7, W1, W2, S10,
// S10b, S12b, S13 and S14):
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
//     returns ErrRestartRequested), and until then it looks every 30 s, not every ten minutes: it has nobody to
//     serve, and what kept the address away is often gone within the minute. It starts even on a machine whose
//     network is not up yet, which then has no
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
//   - The connection test (README S80; 04 §7.7, wire.go): POST /api/v1/conntest on the REST API for every signed-in
//     user, ops.ConnTest over the SFU's probe PCs. A transport without a listener (tcp443 in off mode, udp or
//     tcp7882 when the operator turned it off) is 409 transport_disabled; the answer's server object is the public
//     IPv4 address, NAT kind, provider, container and media ports that Start found.
//   - Web Push (04 §14; wire.go): 04's push service on 03's tables, built before the account service so that its
//     VAPID fingerprint check purges the subscriptions of a rotated key first. A share that goes live reaches it
//     from the hub (the users who are not in the room get "started sharing"), 03's admin alerts reach it from the
//     account service, and the REST API's push endpoints call it (the VAPID key in GET /info, the endpoint checks of
//     a subscription, the test notification). With push.enabled = false there is none: the hub notifies nobody,
//     admin alerts are only logged, and the push endpoints answer push_unavailable.
//   - The admin socket of 04 §12 on listen.admin_socket, for the operator's CLI (setup-url, healthcheck, admin …),
//     with its accounts part over 03's service.
//   - Backup, restore and secret rotation (README S65; 04 §5.3, §12.3, §12.4). The admin socket serves them over
//     ops.DataDir, with 03's snapshot of the running database and its file-level functions (DBFiles), and over
//     secrets.json. A restore and a rotation answer and then ask for the server's restart (restartFor): the
//     shutdown runs for ShutdownRestore or ShutdownRestart, and Run returns ErrRestartRequested. A restore's files
//     are swapped at the end of the shutdown, once the store is closed and still under the data-directory lock;
//     Start completes a swap that a crash interrupted before it opens anything (ops.RecoverRestore), and removes
//     the files from before the restore once nothing can fail it any more and before the admin socket takes a
//     request, the next restore's for one (ops.FinishRestore). What a rotated key protected is purged by the next
//     start's push service (the VAPID pair, 04 §5.2) and account service (03 §4.6). Reexec (exec_unix.go) is the
//     restart itself: cmd/isshoni calls it when Run returns ErrRestartRequested, and the process replaces itself
//     with a new run of the same command line; where a platform has none (exec_other.go), cmd/isshoni exits 75.
//   - The ops data (README S85; 04 §11, wire.go): the server's Prometheus registry on every server, with the hub's,
//     the push service's and the SFU's series on it (sfuCollector), served on metrics.listen with metrics.enabled
//     (Addrs.Metrics) and stopped with the HTTP servers; the transfer counter on the 443 multiplexer, listen.http
//     and the ICE Transport, which ops.Transfer stores per month (its last flush is step 6 of the shutdown, and a
//     backup flushes it first) and alerts the admins on; the daily release check; and the admin routes of ops on
//     the REST API: the dashboard (ops.Dashboard over the status, the hub and the SFU, 03's accounts and the other
//     ops parts), doctor's report and run, and the bandwidth estimate.
//   - The reload of 04 §6.5 (README S90, reload.go): cmd/isshoni's serve answers SIGHUP by loading the config
//     again and calling Reload, which reads the manual TLS files again and makes log.level the configured level
//     (a level that `isshoni admin log-level` set for a while falls back to it); every other key that changed is
//     logged by name, to be applied by a restart.
//   - Hardening (README S90): the server logs through a safety net that redacts an ICE password in the text of a
//     library's message (logsafety.go); the 503s that it gives on purpose (a polled /readyz that is not ready, the
//     shutdown's) are debug lines, not warnings (http.go); and in off mode the app's listener has the per-IP
//     connection limit too, except for the trusted proxies, whose connections carry every client (tls.go).
//
// Of the packages of 04 §2, only this one, cmd/isshoni and servertest import signal, sfu and sfuplane, or combine
// store and auth with ops and push.
package server
