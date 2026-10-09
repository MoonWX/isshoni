# M1 · 04 · Server platform

`cmd/isshoni`, configuration, secrets, lifecycle, the 443 multiplexer and ICE transports, TLS, HTTP wiring and the
embedded SPA, logging, ops endpoints, the admin socket, `doctor`, and Web Push (server side).

Everything here is **M1** unless it is marked *later (Mx)*. Source of truth for product decisions: `docs/PLAN.md`.
Sibling specs, referenced instead of duplicated:
[01-protocol](01-protocol.md) · [02-sfu](02-sfu.md) · [03-accounts-and-store](03-accounts-and-store.md) ·
[05-web-client](05-web-client.md) · [06-deploy-and-ci](06-deploy-and-ci.md).

Example addresses use documentation ranges (`203.0.113.0/24`, `2001:db8::/32`) and `watch.example.com`. In a command
line such as `--public-ip 203.0.113.7`, the address stands for the server's real public IP: with `tls.mode=ip`,
`config` rejects documentation ranges (§4.5), so the example exits 78 if run as written.

Integration status: reconciled with 01–03, 05 and 06 by the integrator (`README.md`, "Integration decisions"). The main
changes: no maintenance mode (unrecoverable states exit 78, recovery is offline); one REST error envelope and one JSON
casing (camelCase) shared with 03; push REST and its tables are 03's, the sender is here; the admin dashboard is here
and embeds 03's `accounts` part; `rotate-secrets` restarts the server; no `sd_notify` in M1; the wiring (§6.6) holds
every adapter between 01, 02 and 03.

---

## 1. Scope

This doc owns:

| Area | Package(s) |
|---|---|
| CLI: `serve`, `setup-url`, `doctor`, `healthcheck`, `admin …`, `config …`, `version` | `cmd/isshoni` |
| Wiring and lifecycle (startup, readiness, refusing to start, shutdown, re-exec), and every adapter between 01, 02 and 03 (§6.6) | `internal/server` |
| Config file, env, flags, validation; `secrets.json` | `internal/server/config` |
| 443 first-byte multiplexer, ICE UDP/TCP transports, public IP detection | `internal/server/netx` (new) |
| Connection-test REST endpoint (on 02's probe PCs) | `internal/server/ops` (`conntest.go`) |
| TLS modes via certmagic, port 80 | `internal/server/tlsmgr` |
| Router, global middleware, JSON and error helpers, SPA serving, security headers | `internal/server/httpapi` (router part; 03 owns `API`, the /api/v1 chain and the account handlers) |
| Health, metrics, transfer accounting, dashboard, release check, admin socket | `internal/server/ops` |
| Diagnostics | `internal/server/ops/doctor` |
| Web Push sender (VAPID, SSRF guard, triggers, dedup); 03 owns the push REST and tables | `internal/server/push` |
| slog setup, `Secret` type, pion/certmagic log bridges | `internal/logx` (new) |
| Build version | `internal/version` (new) |
| In-process test server for every doc's integration tests | `internal/server/servertest` (new) |

Not here: signaling messages and the hub (01), SFU internals (02), schema, auth, sessions, invites and account REST
(03), the SPA (05), packaging, CI and the project site (06).

Why the new packages (not in the plan's layout):
- `netx`: the multiplexer, transports and public-IP code are used by `tlsmgr`, the SFU and `doctor`. Putting them in
  `sfu` would make `doctor` import the SFU.
- `logx` and `version`: needed by the server now and by `internal/client` and the desktop app later (M2), so they sit
  outside `internal/server`.
- `servertest`: 01, 02, 03 and 05 all need "start a real server in-process" for integration tests. One harness avoids
  five. (01's media integration tests live in `internal/server/itest` and use it.)

---

## 2. Package map and import rules

```
cmd/isshoni/
  main.go            dispatch, global flags, exit codes
  serve.go  setupurl.go  doctor.go  healthcheck.go  admin.go  config.go  version.go
internal/version/    version.go
internal/logx/       logx.go  secret.go  pion.go  zap.go  ratelimit.go
internal/server/
  server.go          Server, New, Run, Shutdown; startup order; refusing to start (exit 78)
  wire.go            every adapter between 01, 02, 03 and ops/push (§6.6)
  exec_unix.go       re-exec after restore and rotate-secrets (syscall.Exec); exec_other.go (exit 75)
  config/            config.go keys.go load.go validate.go site.go secrets.go paths.go
  netx/              portmux.go prefixconn.go transport.go udpconn.go publicip.go stun.go rewrite.go
                     ifaces.go cloud.go counter.go
  tlsmgr/            manager.go manual.go redirect.go hints.go
  httpapi/           router.go middleware.go realip.go errors.go json.go spa.go headers.go mime.go
                     (03's api.go and handler files share this package)
  ops/               health.go metrics.go transfer.go dashboard.go release.go conntest.go
                     adminsock.go adminapi.go adminclient.go backup.go restore.go peercred_linux.go peercred_darwin.go
  ops/doctor/        doctor.go checks_*.go bandwidth.go render.go messages_en.go
  push/              service.go sender.go ssrf.go limits.go prune.go payload.go store.go
  servertest/        servertest.go
```

Import rules. This table is the single source for the `depguard` rules in golangci-lint (06 §8); other docs point
here instead of repeating it. It covers packages of this repo only; the standard library and third-party modules are
governed by the license gate (06).

| Package | May import (this repo) | Never imports |
|---|---|---|
| `internal/version` | nothing (a leaf) | any package of this repo |
| `internal/logx` | `version` | `internal/server/...` |
| `config` | `logx`, `version` | any other `internal/server/...` package |
| `netx` | `logx`, `version`, `internal/protocol/api` (for `api.NATKind`, `api.CloudProvider`); plus pion | `config` (it takes plain option structs, so `doctor` and tests can use it freely), `sfu` |
| 03's `store` | `internal/protocol/api` only (plus the standard library, modernc and `golang.org/x/text/secure/precis`) | anything else |
| 03's `auth` | `store`, `internal/protocol/api` | anything else |
| `httpapi` (this router and 03's `API`) | `config`, `logx`, `version`, `store`, `auth`, `internal/protocol`, `internal/protocol/api` | `ops`, `push`, `netx`, `tlsmgr`, `signal`, `sfu`, `sfuplane` |
| `tlsmgr`, `ops`, `ops/doctor`, `push` | `config`, `logx`, `version`, `netx`, `internal/protocol`, `internal/protocol/api` (and `ops` → `ops/doctor`) | `httpapi`, `store`, `auth`, `signal`, `sfu`, `sfuplane` |
| 02's `internal/server/sfu`, `sfu/sfutest` | `netx` (`Transport`, `TransportOptions`); the rest per 02 | – (`netx` never imports `sfu`) |
| `internal/server` (the wiring), `cmd/isshoni`, `servertest` | all of the above | – |

- Of the packages in this table, only `internal/server`, `cmd/isshoni` and `servertest` import `signal`, `sfu` and
  `sfuplane`, or combine `store`/`auth` with `ops`/`push` (01's and 02's own packages and tests follow their docs). They adapt 01–03 to the small interfaces that `ops` and `push` declare, so `ops`
  and `push` compile and test without 01–03.
- The router takes a small interface declared in `httpapi` (`RouteObserver`, §9.2) in place of `*ops.Metrics`.
- `ops` and `push` export typed functions that return DTOs and `*api.Error` (e.g.
  `Dashboard.Snapshot(ctx) (api.OpsDashboard, error)`); the wiring wraps each with `httpapi.DecodeJSON`/`WriteJSON`/
  `WriteError` and registers it with `API.Handle`, passing the principal from `httpapi.PrincipalFrom`.
- `httpapi.Push` is implemented by a wiring adapter that converts `store.PushSubscription` to `push.Subscription`
  (§6.6).
- 03's file-level store functions (`store.LatestSchemaVersion`, `InspectFile`, `BackupFile`) reach `ops` and
  `ops/doctor` as function values passed in by `cmd/isshoni` and the wiring (§12.3, §12.4, §13.1).
- JSON types that the SPA reads (dashboard, doctor report, bandwidth, connection test, push payload) live in
  `internal/protocol/api` next to 03's DTOs (files `ops.go`, `doctor.go`, `conntest.go`, `push.go`), so tygo
  generates them into `web/src/protocol/api.gen.ts` (01 §14.4). The error envelope is 03's `api.Error`. All JSON
  field names are camelCase, like 01 and 03.

---

## 3. `cmd/isshoni`

### 3.1 Subcommands

| Command | What it does | Talks to |
|---|---|---|
| `isshoni serve [config flags]` | Runs the server. Docker `CMD ["serve"]` | – |
| `isshoni setup-url [--qr\|--no-qr] [--wait DUR] [--json]` | Mints a one-time setup link and prints it (plus a QR code on a TTY). `--wait` is for Docker and manual use; install.sh waits with `healthcheck --ready` and then calls `setup-url` once | admin socket |
| `isshoni doctor [--json] [--strict] [--only ID,…] [--list-checks] [--bandwidth-flags…]` | Diagnostics (§13). Works with or without a running server. `--only` runs the named checks (install.sh uses `dns,public_ip,clock` before the first start); `--list-checks --json` prints a JSON array of the check ids (06's anchor test, §13.1) | admin socket if up |
| `isshoni healthcheck [--ready] [--wait DUR]` | Liveness (default) or readiness. Docker `HEALTHCHECK` (liveness); install.sh uses `--ready` | admin socket |
| `isshoni admin status [--json]` | Version, uptime, TLS, rooms, connections | admin socket |
| `isshoni admin backup [--out PATH\|-] [--no-certs] [--offline]` | Writes a backup archive (§12.3) | admin socket |
| `isshoni admin restore PATH\|- [--yes] [--offline]` | Restores a backup archive, or only the database from a pre-migration `backups/pre-*.db` file, and restarts the server (§12.4) | admin socket |
| `isshoni admin users list [--json]` | Lists accounts | admin socket |
| `isshoni admin users reset-password NAME` | Prints a one-time password-reset link | admin socket |
| `isshoni admin users set-role NAME admin\|user` | Changes a role (03's roles) | admin socket |
| `isshoni admin users disable\|enable NAME` | Blocks or unblocks sign-in (disable revokes sessions and devices) | admin socket |
| `isshoni admin invite create [--uses N] [--ttl DUR]` | Prints an invite link. Flags left out are not sent; defaults: the server's invite settings (03 §9). `--ttl` is whole hours from `1h` to `720h` | admin socket |
| `isshoni admin rotate-secrets [--yes]` | Rotates all generated secrets (session, invite, resume and VAPID keys) and restarts the server (§5.3) | admin socket |
| `isshoni admin log-level LEVEL [--for DUR]` | Changes the log level at runtime (default `--for 30m`, then back) | admin socket |
| `isshoni config check` | Loads and validates config; prints problems | – |
| `isshoni config print [--json]` | Effective config with the source of every value | – |
| `isshoni config example [config flags]` | Prints a commented `isshoni.toml` with the given values set. Like `config init`, values come from flags only and are validated (errors → exit 78, nothing on stdout) | – |
| `isshoni config init --path PATH [config flags]` | Writes the same commented file to PATH (0640, the caller fixes the owner) and refuses to overwrite an existing file (exit 7). Values come from flags only; `ISSHONI_*` env is ignored, so the file holds exactly what the caller passed, plus `network.trusted_proxies` in `off` mode with a loopback `listen.http` (§4.4). The resulting config is validated like `config check`: on any error the problems are printed and it exits 78 without writing (warnings are printed and the file is written). install.sh uses it, e.g. `--tls.mode ip --public-ip 203.0.113.7` or `--domain watch.example.com` (06 §4.8) | – |
| `isshoni version [--short\|--json]` · `isshoni --version` | Build info (§15); `--short` prints only the version, e.g. `0.1.0` (install.sh) | – |
| `isshoni help [CMD]` | Usage | – |

Conventions:
- Every command that reads config accepts `--config PATH` (env `ISSHONI_CONFIG`) and the config flags of §4.
  `serve` needs them; the others only need `listen.admin_socket` to find the socket.
- `--socket PATH` overrides `listen.admin_socket` for client commands.
- `--json` prints exactly one JSON document on stdout; human text goes to stderr.
- Colors only on a TTY and only when `NO_COLOR` is unset.
- Destructive commands (`restore`, `rotate-secrets`) ask `Continue? [y/N]` only when stdin is a TTY (so never for
  `restore -`, whose stdin is the archive); otherwise they need `--yes`. Without it they refuse (exit 2) and print the
  exact command with `--yes` added, in the environment's form (Docker when a container is detected, §5.1; the same
  detection as §6.1 step 4), e.g. `sudo isshoni admin restore --yes /root/backup.tar.gz` under systemd, or
  `docker compose exec -T isshoni isshoni admin restore --yes - < backup.tar.gz` in Docker.
- The CLI never opens the database or writes to the data directory, except `--offline` commands (§12.6).

### 3.2 Exit codes

| Code | Meaning |
|---|---|
| 0 | Success (doctor: no `fail` results) |
| 1 | Runtime error: cannot bind a port, another isshoni process holds the data-directory lock (§5.1), a `store.Open` error that does not wrap `store.ErrNeedsOperator`, backup failed, unexpected server error |
| 2 | Usage error: unknown command or flag, missing argument, a destructive command without a TTY and without `--yes` (§3.1) |
| 78 | `EX_CONFIG`: a configuration or data problem that a restart can't fix: invalid config (all problems are printed, §4.5, including a policy value that 03's `SettingsCache.Pin` rejects), `config init`/`config example`: the given values are invalid, any `store.Open` error for which `errors.Is(err, store.ErrNeedsOperator)` (newer schema, history mismatch, failed migration, corrupt DB, …; 03's message is printed), a corrupt `secrets.json`, wrong owner of data files (§5.2), or a container without a data volume (§6.3). systemd's `RestartPreventExitStatus=78` (06) stops the restart loop |
| 4 | Server not reachable on the admin socket: not running or wrong path, or permission denied, which prints its own message: "Permission denied on /run/isshoni/admin.sock: run it with sudo (sudo isshoni …)" (§12.1) |
| 5 | `doctor`: at least one `fail` (with `--strict`, also any `warn`) |
| 7 | Refused: precondition not met (an admin already exists: `setup_unavailable`, backup is newer than this binary, `--offline` while the server runs or holds the data-directory lock, running as the wrong user, `config init` target exists) |
| 75 | `serve` on a platform without re-exec (Windows): "restart required" after a restore |
| 130 | Interrupted (SIGINT) |

**Exception, `healthcheck`**: exits only **0** (healthy) or **1** (unhealthy, unreachable, or any error), because
Docker reserves exit code 2 in `HEALTHCHECK`.

**Exit 7 for the admin socket's error codes** (as README S43 built `refusalExit`; later slices add to it). A client
command branches on the `code` of the server's error, never on the HTTP status (§12.2). Six codes mean "refused:
a precondition is not met, and repeating the command won't change that" and exit **7**:

| Code | From | The precondition |
|---|---|---|
| `setup_unavailable` | `setup-url` | an admin already exists (fix: `isshoni admin users reset-password NAME`) |
| `last_admin` | `admin users set-role NAME user`, `admin users disable NAME` | it would remove the last active admin |
| `registration_closed` | `admin invite create` | `registration.mode` is `closed`: no invite can be made or used |
| `limit_reached` | `admin invite create` | the server has its 100 active invites (03 §7.9) |
| `backup_newer` | `admin restore` | the backup is from a newer isshoni than this binary |
| `restore_in_progress` | `admin restore` | another restore is running |

Every other error answer of the server exits **1**: `user_not_found`, `validation_failed`, `bad_request`,
`server_shutdown` (§12.2), `internal`. In both cases the command prints the socket's `message` and, when there is
one, a line `fix: …` (§12.1). No server on the socket, and permission denied on it, are exit 4 as in the table.

### 3.3 Output examples

`isshoni setup-url` on a TTY:

```
Open this link in your browser to create the admin account (valid 24 hours, works once):

  https://203.0.113.7/setup#Qm9fS2tXb0p6c1F4dVZ3UzBqWmN5Z2hB

  ▄▄▄▄▄▄▄ ▄ ▄▄ ▄▄▄▄▄▄▄
  █ ▄▄▄ █ ▀█▄▀ █ ▄▄▄ █      (QR code of the same link)
  …
Running `isshoni setup-url` again makes a new link and cancels this one.
```

`--json`: `{"url":"https://203.0.113.7/setup#Qm9f…","expiresAt":"2026-09-30T10:00:00.000Z","tlsReady":true}`.

When the certificate is not ready yet, the link is still printed with a note: "The certificate isn't ready yet. The
link works once it is; check with `isshoni doctor`." `--wait 180s` first waits for readiness; it is for Docker and
manual use. install.sh waits with `isshoni healthcheck --ready` and then calls `setup-url` once (06).

QR rendering uses `github.com/skip2/go-qrcode` (MIT) for the code's modules and draws them in the style of its
`ToSmallString`: half-block characters, with a 2-module quiet zone (below). It is a new dependency (permissive)
added for this command only.

**As README S43 built the output** (`cmd/isshoni/client.go` `printLink`, `qr.go`):

- **Off a terminal, stdout carries the bare link.** When stdout is not a terminal (a pipe, a file,
  `docker compose exec -T`), stdout gets the link alone on its first line, with no heading and no indentation, so a
  script takes it as it is: `url=$(isshoni setup-url)`. The heading ("Open this link…") and the notes ("The
  certificate isn't ready yet…", "Running `isshoni setup-url` again…") go to stderr. On a terminal everything goes
  to stdout in the layout above. `admin users reset-password` and `admin invite create` print their links the same
  way (without a QR code).
- **The QR code** is drawn by default only when stdout is a terminal. `--no-qr` never draws it; `--qr` draws it
  also off a terminal, where it follows the link on stdout (the link stays the first line). `--qr` with `--no-qr`
  is a usage error (exit 2). With `--json`, stdout is the one JSON document and the code of `--qr`, like the
  certificate note, goes to stderr. A link too long for a QR code prints `isshoni: no QR code: …` on stderr and
  the link all the same.
- **The quiet zone is 2 modules, and the CLI draws the code itself.** go-qrcode's `ToSmallString` has a fixed
  border of 4 modules (the standard's quiet zone), which makes the code of a setup link wider and taller than it
  needs to be on an 80-column terminal. So `qr.go` takes the module bitmap from the library with its border off
  (error correction level Medium) and draws it with the same half-block characters, two rows of modules per line,
  and a 2-module border; phone cameras read it. Light modules are drawn (`█`) and dark ones left blank, as
  `ToSmallString` and `qrencode` do: right on the usual dark terminal, and the negative on a light one, which
  phones read as well. On a terminal the code is indented like the link.
- "valid 24 hours" is computed from the answer's `expiresAt` (days from three days on, hours below, minutes below
  an hour), so the text stays right if 03's token lifetime changes.
- **`--wait DUR`** takes at most 1 h (`ops.AdminMaxWaitReady`, the server's own limit for `waitReadyS`); more is a
  usage error. While no server answers yet (a container that has just started), the command tries again every
  500 ms; once a server answers, the server itself waits for readiness; both together take at most `DUR`. After
  2 s of waiting it says so once on stderr ("Waiting for the server to be ready (up to 3m)…"). Running out of time
  on the server's side is no error: the link is minted and `tlsReady` says whether the certificate is there.

---

## 4. Configuration (`internal/server/config`)

### 4.1 Sources and precedence

```
flag  >  env  >  file  >  [policy keys only: admin UI setting in the DB]  >  built-in default
```

- File: `--config PATH`, else `ISSHONI_CONFIG`, else `/etc/isshoni/isshoni.toml`. A missing file at the **default**
  path is fine (Docker runs on env only). A missing file at an explicit path is error 78.
- The file is read-only to the service (install.sh: `root:isshoni 0640`, 06). The server never writes it.
- Changes need a restart (`systemctl restart isshoni`); clients reconnect on their own (§6.4). `SIGHUP` reloads only
  manual TLS files and `log.level` (§6.5).
- Relative paths (dev only) are resolved against the working directory at startup.

### 4.2 Naming rule (mechanical, one registry)

For TOML path `section.key` (or top-level `key`):
- env: `ISSHONI_` + upper-case path with `.` → `_`. Example: `tls.mode` → `ISSHONI_TLS_MODE`, `public_ip` →
  `ISSHONI_PUBLIC_IP`.
- flag: `--` + path with `_` → `-`. Example: `--tls.mode`, `--public-ip`, `--listen.admin-socket`.
- Lists in env and flags are comma-separated (`ISSHONI_NETWORK_TRUSTED_PROXIES=10.0.0.0/8,172.16.0.0/12`). Durations
  use Go syntax (`10s`, `168h`). Booleans: `true/false/1/0`.
- An env variable set to the empty string (`ISSHONI_PUBLIC_IP=`) is treated as unset: the value comes from the file or
  the default, `Source` is not `env`, and `IsSet` is false. This is what compose's `${VAR:-}` produces. To set a key
  to an empty string, use the file or a flag.

All keys are declared once in `config/keys.go` (path, default, kind, help, `Policy` flag, consumer). That registry
drives the TOML decoder, env and flag parsing, `config print`, `config example` and `serve --help`; the site's
`/reference/config` is hand-written from this registry in M1 and generated later (M5, 06 §10.2). **Other docs add
keys only through this registry.**

Unknown keys:
- In the file: error 78, with position and a "did you mean" suggestion (edit distance ≤ 2). go-toml/v2 decodes the
  file into a map (syntax errors and duplicate keys come with their positions), the registry is walked against that
  map, and go-toml's parser gives each key's line and column, so one run reports every unknown key and type error (a
  strict `DisallowUnknownFields` decode would stop at the first).
- In env: a warning with the same suggestion (env may hold unrelated variables).
- Reserved non-key names are ignored without a warning. The list lives in `config/keys.go` next to the registry and
  matches 06's list; a new non-key `ISSHONI_*` name must be added to both: `ISSHONI_CONFIG`, `ISSHONI_VERSION`
  (install.sh target version and the Vite build version), `ISSHONI_YES`, `ISSHONI_NO_FIREWALL`,
  `ISSHONI_DOWNLOAD_BASE`, `ISSHONI_INSTALL_SOURCED`, `ISSHONI_INSTALLER_VERSION`, `ISSHONI_SNAPSHOT_VERSION`,
  `ISSHONI_BIN`, `ISSHONI_DEV_SERVER`, `ISSHONI_LOADTEST_PASSWORD` (02's `isshoni-loadtest`). The env-only switches of
  §4.3 are read, not ignored.

### 4.3 Key reference

**Top level**

| Key | Default | Consumer | Notes |
|---|---|---|---|
| `domain` | `""` | tlsmgr, site | DNS name friends use. Empty = use the public IP |
| `public_ip` | `"auto"` | netx | `"auto"` (interface, then STUN) or an IPv4/IPv6 literal. Docker: `ISSHONI_PUBLIC_IP` |
| `public_ipv6` | `"auto"` | netx | `"auto"`, `"off"` or a literal. Extra IPv6 media candidates (§7.6) |
| `public_url` | `""` (derived) | site | Required with `tls.mode="off"` unless `listen.http` is loopback (dev) |
| `data_dir` | `"/var/lib/isshoni"` | all | §5 |
| `shutdown_timeout` | `"10s"` | server | Upper bound for graceful shutdown (§6.4) |

**`[listen]`**

| Key | Default | Notes |
|---|---|---|
| `listen.https` | `":443"` | TLS (HTTPS/WSS) **and** ICE-TCP, split by the first byte (§7.2). Ignored with a warning in `off` mode |
| `listen.http` | `":80"`; `"127.0.0.1:8080"` when `tls.mode="off"` | ACME HTTP-01 plus redirect; in `off` mode the app itself |
| `listen.ice_udp` | `":7882"` | ICE UDP mux for all media. `""` disables UDP (tests of the TCP path) |
| `listen.ice_tcp` | `":7882"` | ICE-TCP fallback (needed in `off` mode). `""` disables |
| `listen.admin_socket` | `"/run/isshoni/admin.sock"` | §12. Path ≤ 104 bytes (macOS `sun_path`) |

**`[tls]`**

| Key | Default | Notes |
|---|---|---|
| `tls.mode` | `""` = derived | `auto` \| `ip` \| `manual` \| `off`. Derived: `domain` set → `auto`, else `ip` (§4.4) |
| `tls.acme_email` | `""` | Optional ACME account contact |
| `tls.acme_ca` | `"https://acme-v02.api.letsencrypt.org/directory"` | ACME directory |
| `tls.acme_staging` | `false` | Use Let's Encrypt staging (untrusted certificates; testing only) |
| `tls.acme_ca_root` | `""` | PEM file trusted for the ACME server's own TLS. Testing only (Pebble in CI) |
| `tls.cert_file`, `tls.key_file` | `""` | `manual` mode |
| `tls.hsts` | `true` | Sends HSTS only when serving a domain over TLS (§9.6) |

**`[network]`**

| Key | Default | Notes |
|---|---|---|
| `network.stun_servers` | `["stun.cloudflare.com:3478", "stun.l.google.com:19302"]` | Public IP detection only (§7.4) |
| `network.ipv6` | `true` | Gather and advertise IPv6 media candidates when a global address exists |
| `network.exclude_interfaces` | `["docker*","br-*","veth*","virbr*","cni*","flannel*","cali*","kube-*"]` | Glob list; never used for media |
| `network.include_loopback` | `false` | Dev only: advertise 127.0.0.1 / ::1 candidates (::1 with `network.ipv6`, and for UDP only, §7.6) |
| `network.udp_buffer_bytes` | `8388608` | SO_RCVBUF/SO_SNDBUF requested on media sockets; needs sysctl ≥ this (§13) |
| `network.trusted_proxies` | `[]`; `["127.0.0.0/8", "::1/128"]` when the effective mode is `off` and `listen.http` is loopback (§4.4) | CIDRs allowed to set `X-Forwarded-*` (`off` mode only, §8.5). `config init` writes the loopback default into the file explicitly |

**Policy keys** (§4.6: editable in the admin UI unless set here; 03's settings field in brackets)

| Key | Default | Consumer |
|---|---|---|
| `registration.mode` | `"invite"` | 03 [`registrationMode`]. `invite` \| `approval` \| `closed` |
| `clients.min_version` | `""` | 01 [`minClientVersion`]. SemVer; older native clients get the "Update the app" screen |
| `limits.max_participants_per_room` | `0` (none) | 01 [`maxParticipantsPerRoom`]. Admin soft limit |
| `limits.max_shares_per_room` | `0` (none) | 01 [`maxSharesPerRoom`]. Admin soft limit |
| `limits.max_bitrate_kbps` | `0` (preset caps only) | 01, 02 [`maxShareBitrateKbps`]. Cap on any share's full layer |
| `limits.transfer_alert_gb` | `0` (off) | ops (§11.3) [`transferAlertGb`]. 1 GB = 10⁹ bytes |
| `updates.release_check` | `true` | ops (§11.5) [`updateCheck`] |

**Guards and advanced** (not policy; boot-time)

| Key | Default | Consumer |
|---|---|---|
| `limits.ws_handshakes_per_ip_per_minute` | `20` | 01's hub (plan guard). Raise it for LAN parties behind one IP |
| `limits.conns_per_ip` | `256` | netx: open TCP connections per source IP (IPv4 address or IPv6 /64, §7.2) on 443 and 80 |
| `sfu.pause_unwatched_layers` | `true` | 02 §11: hint browsers to pause the full layer nobody watches |

**Env-only switches** (not keys; read directly): `ISSHONI_CONFIG` (config path), `ISSHONI_IN_CONTAINER=1` (set by
06's image; also detected, §5.1), `ISSHONI_ALLOW_EPHEMERAL_DATA=1` (tests only: skip the container data-volume check).
The other reserved names of §4.2 (`ISSHONI_VERSION`, install.sh's and the dev tasks' variables) are ignored.

**`[push]`, `[metrics]`, `[log]`, `[updates]`**

| Key | Default | Notes |
|---|---|---|
| `push.enabled` | `true` | Web Push (§14) |
| `push.subject` | `""` = derived | VAPID `sub`: `mailto:<tls.acme_email>` if set, else the public origin (an `http://` origin is given as `https://`, §14.1). Must start with `mailto:` or `https:` |
| `metrics.enabled` | `false` | Prometheus endpoint (§11.2) |
| `metrics.listen` | `"127.0.0.1:9469"` | Warning if not loopback: metrics are unauthenticated |
| `metrics.pprof` | `false` | `/debug/pprof/*` on the metrics listener |
| `log.level` | `"info"` | `debug` \| `info` \| `warn` \| `error` |
| `log.format` | `"auto"` | `auto` (text on a TTY, else JSON) \| `text` \| `json` |
| `updates.release_url` | GitHub releases API URL of `MoonWX/isshoni` | Hidden; tests only |

`isshoni config example --public-ip 203.0.113.7` prints (203.0.113.7 stands for the server's real public IP, see the
top of this document; comments abbreviated here; `config init --path P` writes the same text; the `Reference` URL is
built from `version.DocsURL`):

```toml
# isshoni server configuration. Every key also has an ISSHONI_* env variable and a --flag.
# Reference: https://moonwx.github.io/isshoni/reference/config

# domain = "watch.example.com"   # set this to use a domain; empty = use the public IP
public_ip = "203.0.113.7"        # "auto" detects it with STUN

[tls]
# mode = "auto"                  # derived: domain → auto, otherwise ip

[registration]
# mode = "invite"                # invite | approval | closed (also editable in the admin page)
```

### 4.4 Derived values

**Effective TLS mode**: `tls.mode` if set; else `auto` when `domain` is set; else `ip`. Explicit `auto` without a
domain is error 78 (fix: set `domain`, or use `ip`).

**Site** (computed after public IP detection, passed to every component):

| Mode | `Origin` | `Host` |
|---|---|---|
| `auto` | `https://<domain>` (plus `:port` if `listen.https` is not 443) | domain |
| `ip` | `https://<public IPv4>` (or `https://[v6]` on IPv6-only hosts) | IP literal |
| `manual` | `https://<domain>` if set, else `https://<public ip>` | same |
| `off` | `public_url`; dev (loopback `listen.http`, no `public_url`): `http://localhost:<port>` | from the URL |

`Dev` is true only when the effective mode is `off`, `listen.http` is a loopback address, and `public_url` is empty or
its host is `localhost`, `127.0.0.1` or `[::1]`. A reverse-proxy install (an `https://` public URL on a real host
name) is never dev.

**Trusted proxies in `off` mode**: when the effective mode is `off`, `listen.http` is a loopback address and
`network.trusted_proxies` is not set (`IsSet` false), the effective value is `["127.0.0.0/8", "::1/128"]`. Only
local processes can reach a loopback listener, so trusting them is safe, and it keeps the common setup (Caddy or nginx
on the same host, `listen.http = "127.0.0.1:8080"`) from showing every friend as `127.0.0.1`. Otherwise every per-IP
guard of 01 and 03 would be one shared bucket, and one stranger's failed logins would block sign-in for everyone. An
explicit value, even `[]`, wins. `config init` writes the value into the file explicitly in this case (the one value
it writes that the caller did not pass), so the operator sees it.

```go
package config

type TLSMode string

const (
	TLSAuto   TLSMode = "auto"
	TLSIP     TLSMode = "ip"
	TLSManual TLSMode = "manual"
	TLSOff    TLSMode = "off"
)

// Site is how the outside world reaches this server. Built once at startup.
type Site struct {
	Origin   string  // "https://watch.example.com", no trailing slash
	Host     string  // "watch.example.com", "203.0.113.7", "[2001:db8::1]", with ":port" if not default
	Hostname string  // without port and brackets
	TLSMode  TLSMode
	Dev      bool    // off mode, loopback listen.http, public_url empty or on a loopback host: accept any localhost Host header
	// ExtraOrigins: later (M2) the Wails asset origins, added in code, not config.
	ExtraOrigins []string
}

func NewSite(c *Config, publicV4, publicV6 netip.Addr) (Site, error)
func (s Site) URL(pathAndFragment string) string // s.Origin + p; p must start with "/"
```

### 4.5 Validation

`Validate` collects **all** problems, then `serve` exits 78 if any is an error. Each problem names the key, where the
value came from, what is wrong, and the fix:

```
config error: tls.mode = "ip" (file /etc/isshoni/isshoni.toml:7) needs a public address, but public_ip = "192.168.1.20" is private.
  fix: Let's Encrypt only issues IP certificates for public addresses. Use a VPS, or set domain and tls.mode = "auto",
       or run behind your own HTTPS proxy with tls.mode = "off".
config warning: metrics.listen = "0.0.0.0:9469" (env ISSHONI_METRICS_LISTEN) exposes unauthenticated metrics.
  fix: use 127.0.0.1:9469 and scrape through an SSH tunnel or a local agent.
```

Rules (E = error, W = warning):

| Rule | Level |
|---|---|
| Enum keys (`tls.mode`, `registration.mode`, `log.*`) have a known value | E |
| `domain` is a valid DNS name, not an IP literal (fix: use `public_ip`) and not `localhost` | E |
| `public_ip` / `public_ipv6` literal is valid; with `tls.mode=ip` it must be public (not RFC 1918, CGNAT, loopback, link-local, ULA, documentation) | E |
| `auto` needs `domain`; `manual` needs `cert_file` and `key_file` | E |
| `off`: `public_url` set, or `listen.http` is loopback; `public_url` is `https://` unless its host is a loopback name or address (`localhost`, `127.0.0.1`, `[::1]`: dev and e2e, 06 §7.3) | E |
| `off` with `listen.https` explicitly set | W (ignored) |
| `off` without `trusted_proxies` and a non-loopback `listen.http` | W (client IPs will be the proxy's) |
| Listen addresses parse as `host:port`; no two TCP listeners share a port (`https`, `http`, `ice_tcp`, `metrics`) | E |
| At least one of `ice_udp`, `ice_tcp`, `https` (non-off) carries media | E |
| `trusted_proxies` entries are CIDRs; `stun_servers` are `host:port` | E |
| `admin_socket` ≤ 104 bytes; `data_dir` not empty | E |
| Durations > 0; `udp_buffer_bytes` between 1 MiB and 64 MiB | E |
| `push.subject` starts with `mailto:` or `https:` (only the prefix is checked; `push.New` also refuses a bare `mailto:` or `https:`, §14.1) | E |
| `clients.min_version` is SemVer | E |
| Policy key value accepted by 03's `SettingsCache.Pin` (ranges of 03 §9) | E (at serve startup; reported like other config errors, exit 78) |
| `metrics.listen` not loopback | W |
| `tls.acme_staging` or `tls.acme_ca_root` set | W (not for production) |

Checks that need the machine (file readable, port free, cert matches) run at startup or in `doctor`, not here.

### 4.6 Policy keys and the admin UI

A few product settings can be changed in the admin UI (stored by 03 in `settings`, same dotted key names) **and**
pinned by an operator in config. The rule, borrowed from Mattermost's env overrides:
- If a policy key is set by flag, env or file, it wins, and the admin UI shows the field read-only with "Set in the
  server config".
- Otherwise the DB value applies; otherwise 03's default (`SettingsCache.Defaults()`). The registry repeats that
  default for docs and `config example` only, and a wiring unit test asserts they match.

Mechanism: after `store.Open` and before serving, the wiring calls 03's `SettingsCache.Pin(field, value)` for every
policy key where `cfg.IsSet(key)` is true, using the key → field map in §4.3. 03's `Locked()` then lists the field,
the admin UI shows it read-only, and `PATCH /admin/settings` answers 409 `setting_locked`. 03's settings are the only
source of validation for policy values: a `Pin` error becomes a config `Problem` naming the TOML key, its source and
03's field code, and `serve` exits 78 (§4.5).

`config print` shows an unpinned policy key as "not set (admin UI value applies)", because the effective value lives
in the DB.

### 4.7 Go API

```go
package config

type Duration struct{ time.Duration } // TextUnmarshaler: "10s"

type Config struct {
	Domain          string   `toml:"domain"`
	PublicIP        string   `toml:"public_ip"`
	PublicIPv6      string   `toml:"public_ipv6"`
	PublicURL       string   `toml:"public_url"`
	DataDir         string   `toml:"data_dir"`
	ShutdownTimeout Duration `toml:"shutdown_timeout"`

	Listen       Listen       `toml:"listen"`
	TLS          TLS          `toml:"tls"`
	Network      Network      `toml:"network"`
	Registration Registration `toml:"registration"`
	Clients      Clients      `toml:"clients"`
	Limits       Limits       `toml:"limits"`
	SFU          SFU          `toml:"sfu"`
	Push         Push         `toml:"push"`
	Metrics      Metrics      `toml:"metrics"`
	Log          Log          `toml:"log"`
	Updates      Updates      `toml:"updates"`
	// unexported: sources map[string]Source
}

type Listen struct {
	HTTPS, HTTP, ICEUDP, ICETCP, AdminSocket string // toml: https, http, ice_udp, ice_tcp, admin_socket
}
type TLS struct {
	Mode                          TLSMode
	ACMEEmail, ACMECA, ACMECARoot string
	ACMEStaging                   bool
	CertFile, KeyFile             string
	HSTS                          bool
}
type Network struct {
	STUNServers       []string
	IPv6              bool
	ExcludeInterfaces []string
	IncludeLoopback   bool
	UDPBufferBytes    int
	TrustedProxies    []netip.Prefix // effective value: the loopback default of §4.4 is filled in by Load
}
type Registration struct{ Mode string }
type Clients struct{ MinVersion string }
type Limits struct {
	MaxParticipantsPerRoom     int // toml: max_participants_per_room
	MaxSharesPerRoom           int // toml: max_shares_per_room
	TransferAlertGB            int
	MaxBitrateKbps             int
	WSHandshakesPerIPPerMinute int
	ConnsPerIP                 int
}
type SFU struct{ PauseUnwatchedLayers bool } // toml: pause_unwatched_layers
type Push struct {
	Enabled bool
	Subject string
}
type Metrics struct {
	Enabled bool
	Listen  string
	PProf   bool
}
type Log struct{ Level, Format string }
type Updates struct {
	ReleaseCheck bool
	ReleaseURL   string
}

type SourceKind string // "default" | "file" | "env" | "flag"
type Source struct {
	Kind SourceKind
	File string // for "file"
	Line int
	Name string // env var or flag name
}

type Problem struct {
	Key      string
	Source   Source
	Severity string // "error" | "warning"
	Message  string
	Fix      string
}
type ValidationError struct{ Problems []Problem } // Error() prints them as in §4.5

// Load registers the config flags on fs, parses args, reads the file and env, applies defaults
// and validates. Returns *ValidationError for errors; warnings are in cfg.Warnings().
func Load(fs *flag.FlagSet, args []string, environ []string) (*Config, error)

func (c *Config) Warnings() []Problem
func (c *Config) Source(key string) Source
func (c *Config) IsSet(key string) bool // source != default
func (c *Config) EffectiveTLSMode() TLSMode
func (c *Config) Paths() Paths

type Paths struct {
	DataDir, DB, Secrets, CertMagic, Backups, Restore string
}
```

---

## 5. Data directory and secrets

### 5.1 Layout

```
/var/lib/isshoni/             0700 isshoni:isshoni (systemd StateDirectory; Docker volume, uid 65532)
├─ isshoni.db, -wal, -shm     03 (SQLite)
├─ isshoni.lock               data-directory lock (flock; see below)
├─ secrets.json               0600 (§5.2)
├─ certmagic/                 0700, certmagic FileStorage (certificates, ACME account keys, locks)
├─ backups/                   0700: pre-<schema>-<ts>.db (03, last 5), pre-restore-<ts>.tar.gz (§12.4, last 5)
└─ restore/                   transient staging for restores
```

Startup checks (fail → exit 78 with a fix line; restarting can't help). After the umask, `config.PrepareDataDir`
runs the four data-directory checks, from creating `data_dir` to the admin socket's directory, in the order given
here (S25):
- The server sets `umask 0077` first, so everything it creates is private.
- `serve` creates `data_dir` with mode 0700 (and its parents) if missing.
- **In a container, `data_dir` must be a mount** (checked in `/proc/self/mountinfo`: a mount point equal to `data_dir`
  or a parent other than `/`). The check runs after the directory is created, so a directory that `serve` just created
  in a container still exits 78: "Your data would be lost when the container is removed. Mount a volume at
  /var/lib/isshoni (see compose.yaml), e.g. `-v isshoni-data:/var/lib/isshoni`." `ISSHONI_ALLOW_EPHEMERAL_DATA=1`
  skips the check (tests only). Rationale: losing the admin account and certs on `docker compose down` is the worst
  failure for a foolproof setup (a stricter form of the plan's "doctor fails"). **This check runs before the
  ownership and writable checks below**, and before the result of creating the directory is looked at: a container
  without a volume, whose `data_dir` often can't be created or written either (a read-only root, uid 65532 on a
  root-owned path), is told to mount a volume (`data_not_mounted`), not to `chown` a directory that would be lost
  with the container. A mount table that can't be read fails the check with the same fix.
- `data_dir` must be a directory. If it is owned by the process and wider than 0700, it is chmod-ed to 0700 with a
  warning. It must then be writable by the process (`serve` creates and removes a file in it); the fix line prints
  the process's real uid:gid (e.g. `sudo chown -R isshoni:isshoni /var/lib/isshoni` on systemd,
  `sudo chown -R 65532:65532 <host path>` for a bind mount in bridge compose, `0:0` in `compose.host.yaml`). Reason
  `data_dir_not_writable` (§6.3).
- Last, the admin socket's parent directory is created with mode 0700 if missing (reason `admin_socket_dir`).
- **Data-directory lock**: `serve` holds an exclusive `flock` (`LockFileEx` on Windows) on `data_dir/isshoni.lock` for
  its lifetime. If it can't get it, it exits 1: "another isshoni process is using <data_dir>". Offline commands take
  the same lock non-blocking (§12.6). The lock also works across containers that share the volume.
- Running as uid 0 outside a container logs a warning (install.sh creates the `isshoni` user). Root inside a container
  is the documented host-network variant (06), so it is allowed.
- If `GOMEMLIMIT` is unset and a cgroup memory limit exists, the server calls `debug.SetMemoryLimit(0.8 × limit)`.
  (Go 1.25+ already sizes `GOMAXPROCS` from the cgroup CPU limit.)

Container detection: `ISSHONI_IN_CONTAINER=1` (06's image sets it), `/.dockerenv`, `/run/.containerenv` (Podman), or
env `container` set. The data dir is `/var/lib/isshoni` in the container too (06's image creates it owned by 65532).

### 5.2 `secrets.json`

Generated once at first start with `crypto/rand`; written atomically (temp file `O_EXCL` 0600 in the same directory,
fsync, rename, fsync of the directory). Format:

```json
{
  "format": 1,
  "created_at": "2026-09-29T10:00:00Z",
  "keys": {
    "session": {"id": "1", "key": "<base64url, 32 bytes>", "created_at": "2026-09-29T10:00:00Z"},
    "invite":  {"id": "1", "key": "<base64url, 32 bytes>", "created_at": "2026-09-29T10:00:00Z"},
    "resume":  {"id": "1", "key": "<base64url, 32 bytes>", "created_at": "2026-09-29T10:00:00Z"}
  },
  "vapid": {
    "public_key": "<base64url, 65-byte uncompressed P-256 point>",
    "private_key": "<base64url, 32 bytes>",
    "created_at": "2026-09-29T10:00:00Z"
  }
}
```

- Keys are registered by name in code; a registered name missing from the file (added by a newer version) is generated
  and saved at startup. Unknown names in the file are kept.
- File mode wider than 0600 → fixed to 0600 with a warning. Owner ≠ process uid → exit 78 ("run: sudo chown isshoni:
  isshoni /var/lib/isshoni/secrets.json"; restarting can't fix it). Corrupt JSON → exit 78 with reason `secrets_corrupt` and the fix "restore
  it: `isshoni admin restore --offline <backup>`" (never silently regenerated: that would log everyone out).
- VAPID keys are generated in `config` with the standard library: `ecdh.P256().GenerateKey(rand.Reader)`
  (`crypto/ecdh`), stored in the form of webpush-go's `GenerateVAPIDKeys()` (the 65-byte uncompressed public point
  and the 32-byte private scalar, base64url without padding), so `push` hands them to webpush-go as they are (§14.1)
  and `config` does not import webpush-go (S25). A pair read from the file is checked with `crypto/ecdh` too: the
  private key must be a P-256 key and the public key its point, else `secrets_corrupt`.

What each key is for is 03's and 01's business; 04 fixes the **rotation contract**. Rotation always restarts the
server (§5.3), so every consumer only has to handle "the key differs from last time" **at startup**:

| Key | Consumer | After rotation (at the next start) |
|---|---|---|
| `session` | 03 | Key fingerprint changed → all web sessions, device tokens and device codes are deleted (03 §4.6), and with the sessions every web push subscription (cascade, 03 §5); users sign in again and the SPA re-subscribes (desktop apps relink, M2+) |
| `invite` | 03 | Fingerprint changed → outstanding invite, setup and password-reset links are deleted |
| `resume` | 01 | Nothing to do: old resume tokens fail the HMAC; clients do a fresh join (harmless) |
| `vapid` | push (§14) | Fingerprint in 03's `meta` key `vapid_key_fp` changed → all push subscriptions are deleted; each browser re-subscribes the next time the app opens (05). The check runs synchronously in `push.New`, before any listener serves (§6.1 step 8) |

### 5.3 Rotation (`isshoni admin rotate-secrets`)

- Rotates all four: `session`, `invite`, `resume` and VAPID (plan: "`isshoni admin rotate-secrets` rotates them").
  There is no flag to pick keys; the CLI always sends `vapid: true`. The session rotation already deletes every push
  subscription (cascade), so rotating VAPID at the same time costs nothing extra.
- Hard cut, no overlap window: rotation is for suspected compromise, so old keys must stop working immediately.
- Order: write the new file atomically → reply `202 {"rotated": [...], "restarting": true}` → graceful shutdown with
  reason `restart` (clients see the restart notice, §6.4) → re-exec (§6.5). At startup each consumer compares key
  fingerprints (table above) and purges; 03 writes the `secrets.rotated` audit row and the `secrets_rotated` admin
  alert. The alert stays as an audit and security event plus a log line; after a session rotation its push copy
  reaches no one, because no subscriptions are left. Rationale: no live key swapping and no hook ordering; a restart
  costs clients a few seconds, and rotation is rare.

```go
package config

type KeyName string

const (
	KeySession KeyName = "session"
	KeyInvite  KeyName = "invite"
	KeyResume  KeyName = "resume"
)

type VAPIDKeys struct {
	Public  string      // base64url; safe to publish
	Private logx.Secret // base64url
}

type RotateResult struct {
	Rotated []KeyName
	VAPID   bool
}

type SecretStore struct{ /* … */ }

func OpenSecrets(path string, log *slog.Logger) (*SecretStore, error)
func (s *SecretStore) Key(name KeyName) []byte // 32 bytes; panics on an unregistered name
func (s *SecretStore) KeyID(name KeyName) string
func (s *SecretStore) VAPID() VAPIDKeys
// Rotate writes the new file; the caller (the admin socket handler) then requests a restart (§5.3).
func (s *SecretStore) Rotate(ctx context.Context, names []KeyName, vapid bool) (RotateResult, error)
```

---

## 6. Lifecycle (`internal/server`)

```go
package server

type Deps struct { // test seams; zero values = real implementations
	Now        func() time.Time
	STUN       netx.STUNClient
	Resolver   netx.Resolver
	PushSender push.Sender // joins the struct with the push wiring (README S71)
	ReleaseHTTP *http.Client
	Host       config.Host  // the machine as the data-directory checks see it (§5.1); zero = the running process
	SPA        fs.FS        // the built web app; nil = web.Dist()
	API, WS    http.Handler // /api/v1/ and GET /ws; nil = the real ones once wired (README S54), JSON 404 before
	InProcess  bool         // servertest: leave the process-wide umask and Go memory limit alone
	LogLevel   *slog.LevelVar   // the logger's level variable; with it `admin log-level` works (§12). cmd/isshoni
	                            // and servertest pass theirs; nil: the command answers that the level is fixed
	Argon      auth.ArgonParams // the cost of a password hash (03 §7.2); zero = auth.DefaultArgon. Tests lower it
}

type Server struct{ /* … */ }

// New checks cfg and builds the server. It takes no ctx, so it opens and starts nothing.
func New(cfg *config.Config, log *slog.Logger, deps Deps) (*Server, error)
// Start runs the startup sequence of §6.1 from step 2 on and returns once the listeners serve; Site and Addrs are
// known from then on. Run calls it unless the caller already did (servertest needs the bound port before Run
// blocks). On an error everything it opened is released again.
func (s *Server) Start(ctx context.Context) error
// Run blocks until ctx is cancelled (then shuts down gracefully) or a restore requests a restart.
func (s *Server) Run(ctx context.Context) error
func (s *Server) Shutdown(ctx context.Context, reason ShutdownReason) error
func (s *Server) Site() config.Site
func (s *Server) Addrs() Addrs // as bound: a configured port 0 shows as the port the kernel picked

type Addrs struct {
	HTTP  net.Addr // listen.http: the app in off mode; else the plain port (ACME http-01, redirect; §8.3)
	HTTPS net.Addr // listen.https: the 443 multiplexer (S44); nil in off mode
} // later slices add the ICE ports and the metrics listener

type ShutdownReason string // "stop" | "restart" | "restore"

var ErrRestartRequested = errors.New("server: restart requested") // cmd/isshoni re-execs on this
// A shutdown step ran out of time and closed by force what it still had (§6.4 step 7). Shutdown returns it whatever
// the reason; Run only after a stop, where it is no failure: the warning is logged already.
var ErrShutdownForced = errors.New("server: shutdown finished by force")

// NeedsOperator reports whether err, from New, Start or Run, is a refusal of §6.3 (exit 78).
func NeedsOperator(err error) bool
```

`cmd/isshoni` maps `Run`'s result to the exit code (§3.2): nil → 0; `NeedsOperator(err)` → 78; `ErrRestartRequested`
→ re-exec (§6.5; 75 where there is none); `ErrShutdownForced` → 0; anything else → 1. The server returns a refusal
without logging it, so `cmd/isshoni` prints the one actionable message; `Start` logs the config warnings.

A request handler that asks for a restart (the admin socket's restore and `rotate-secrets`) calls `Shutdown` in a
goroutine and does not wait for it: its own request would hold up the HTTP step.

### 6.1 Startup sequence (`serve`)

1. Parse config (exit 78 on errors). Build the logger (§10).
2. `umask`, data dir checks (create with 0700, container mount, tighten, writable, in that order), the
   data-directory lock, memory limit (§5.1).
3. Open `secrets.json` (create on first run; corrupt or wrong owner → exit 78).
4. Open the store (03: migrations with the pre-migration backup). A `store.Open` error for which
   `errors.Is(err, store.ErrNeedsOperator)` (newer schema, history mismatch, failed migration, corrupt database, …) →
   print 03's message and exit 78 (§6.3); any other `store.Open` error → print it and exit 1. On a
   `*store.SchemaTooNewError` with a non-empty `Backup`, 03's message states the problem, the versions and the backup
   file; 04 then prints the restore command for the environment (Docker when a container is detected, §5.1):
   - systemd: `sudo -u isshoni isshoni admin restore --offline <Backup>`, then `sudo systemctl start isshoni`;
   - Docker: `docker compose stop && docker compose run --rm isshoni admin restore --offline <Backup> && docker compose up -d`.

   Offline doctor's `schema` fix uses the same formatter (§13.2). Then pin the policy settings that the config sets
   (§4.6); a value that `Pin` rejects → exit 78.
5. Detect public addresses (§7.4; ≤ 5 s). A detection that finds nothing is no error: the server goes on, and
   where its site is the address it starts without a site and is not ready (§6.2).
6. Bind listeners: 443 `PortMux`, 80, metrics, admin socket; then `netx.NewTransport` binds 7882/udp (per local IP)
   and 7882/tcp. `NewTransport` is the only code that binds 7882. A busy port → exit 1 with the owner if known
   ("port 443 is in use (another web server?). Stop it, or run isshoni behind it with `tls.mode = "off"`"; "port
   7882/udp is in use (another isshoni?). Stop it, or change `listen.ice_udp`").
7. Start the TLS manager (certificate is obtained asynchronously).
8. Build the components and adapters of §6.6 on top of the existing Transport, in this order: `push.New` (VAPID
   fingerprint check and purge; not built with `push.enabled=false`), then `auth.New` (session and invite fingerprint
   checks with the push `AdminAlerter`, 03 §4.6), then httpapi, hub, SFU (on the Transport), ops; register readiness
   checks. Every fingerprint purge finishes here, before any listener serves in step 9.
9. Start HTTP servers and the admin socket (listeners are bound; the certificate may still be pending).
10. Log one line: `isshoni 0.3.0 ready: https://203.0.113.7 (tls=ip) media udp/7882 ice-tcp 443,7882`. If no admin
    exists: "Finish setup: run `sudo isshoni setup-url` (Docker: `docker compose exec isshoni isshoni setup-url`)". The
    setup token itself is **never** logged.

### 6.2 Liveness and readiness

`ops.Health` aggregates named checks. Readiness needs all of:

| Check | Owner | Ready when |
|---|---|---|
| `db` | 03 | `db.Ping` succeeds |
| `tls` | tlsmgr | A valid certificate for the site name is loaded (`off`: always) |
| `media` | netx + 02 | ≥ 1 UDP socket bound (or UDP disabled and a TCP mux up) and `SFU.Ready()` |
| `signal` | 01 | `Hub.Ready()` |
| `public_ip` | netx | only where the site is the server's address (`ip` mode, and `manual` mode without a `domain`): a public address is known |

Liveness is false after shutdown began. Endpoints: §11.1.

**A server whose site is its address and that finds none starts, and is not ready** (as README S44 built it;
kept, decided after group 5). The site is the public address in ip mode and in manual mode without a `domain`
(§4.4); the check `public_ip` exists in exactly those two cases. When `public_ip = "auto"` finds nothing at
startup (§7.4: no public address on an interface and no STUN answer within 5 s), `serve` does **not** exit 78: the
cause is often gone a minute later (the network isn't up yet when the unit starts at boot, a DHCP lease or the
cloud's address is still on its way, STUN is briefly unreachable), and 78 means "a restart can't fix this", after
which systemd stops trying for good (§3.2, `RestartPreventExitStatus=78`). A literal `public_ip` that is not a
public address is a different case and stays a config error with exit 78 (§4.5). install.sh runs doctor's
`public_ip` check before the first start (06 §4.8), so a fresh install hears of a missing address there. This rule
is about a later start that finds none: after a reboot, or in Docker, where an unset `ISSHONI_PUBLIC_IP` means
`auto` and nothing checked beforehand.

What the operator sees on such a server:

| Where | What |
|---|---|
| The log | WARN `public address detection failed` from `netx` when the detection itself failed, then its INFO line `public addresses` with neither a `public_ipv4` nor a `public_ipv6` attribute (`nat=unknown`, or `nat=cgnat_likely` when the default route's interface has a carrier-NAT address and no STUN server answered); in ip mode WARN from `tls`: `tls.mode "ip" has no public IP address to get a certificate for; set public_ip and restart`; and in place of the "ready" line of §6.1 step 10, WARN `isshoni 0.3.0 started without a public IP address (tls=ip) and is not ready: set public_ip to this server's public address, or set domain, and restart` |
| Readiness | `public_ip`: "no public IP address found; set public_ip and restart". In ip mode also `tls`: "no public IP address to get a certificate for" (there is no name to order a certificate for, so no ACME request is made). Liveness is true: the process is healthy, and Docker's `HEALTHCHECK` passes |
| `isshoni healthcheck --ready`, the admin socket's `/v1/ready` | not ready (exit 1), with the two checks. install.sh's wait for readiness ends at its timeout and prints the journal's last lines and doctor's output (06 §4.10) |
| Port 80 | **503** with `Retry-After: 30` and the waiting page (§8.3) for every request outside `/.well-known/acme-challenge/`: there is no origin to redirect to |
| Port 443 | ip mode: the TLS handshake fails, there is no certificate. Manual mode without a domain: the operator's certificate serves, and every request but `/healthz` and `/readyz` gets 421 from the Host check (§9.3), because no host is the site's |
| `isshoni doctor` | `public_ip` is `fail` with this state's own text and fix (§13.2) |

The server does not take a site later in the same process: the origin, the Host check, the certificate's name and
the ICE rewrite rules are made once, at startup (§7.4's rationale for "restart to apply"). So the way out is a
restart with an address to find: `public_ip` set to the address (the fix every message names), `domain` set, or
simply `sudo systemctl restart isshoni` once the network is up.

**Nothing restarts such a server by itself.** systemd's `Restart=on-failure` and Docker's restart policy act on a
process that ended, not on one that runs and is not ready, and Docker's `HEALTHCHECK` is liveness, which passes.
So a cause that was gone a minute after boot still leaves the server not ready until the operator restarts it.
What staying up gives is a server that says why, in every place of the table above. The periodic detection of
§7.4 does not change this. It is not wired yet after S44, whose `Start` detects once and only in the TLS modes;
it comes with the wiring (README S54). From then on it looks again every 10 minutes and reports an address that
shows up the way it reports any change: a warning in the log that says to restart isshoni to apply it (the
dashboard's `public_ip.changed` alert is the same event, but nobody can open the dashboard of a server without a
site).

### 6.3 Refusing to start

When the server loads its config but can't safely serve (a `store.Open` error wrapping `store.ErrNeedsOperator`:
newer schema, history mismatch, failed migration, corrupt DB, …; a corrupt or wrongly owned `secrets.json`; no data
volume in a container), it **exits 78** with one actionable message (plan: "refuses to start on a newer schema and
tells the user which backup to restore"). Rationale: simpler than a half-running server, and systemd's
`RestartPreventExitStatus=78` (06) prevents a restart loop.

Recovery is offline (§12.6), because nothing is listening. For a newer schema the server prints the exact command for
its environment (§6.1 step 4):
- systemd: `sudo -u isshoni isshoni admin restore --offline /var/lib/isshoni/backups/pre-4-20261014T021500Z.db` (a
  pre-migration DB file) or `… --offline backup.tar.gz`, then `sudo systemctl start isshoni`. Installing the newer
  version again is the other fix for a newer schema.
- Docker (the container restarts in a loop under `restart: unless-stopped`): `docker compose stop`, then
  `docker compose run --rm isshoni admin restore --offline /var/lib/isshoni/backups/…`, then `docker compose up -d`.
- Offline `isshoni doctor` shows the same reason (check `schema` or `secrets`).

Reasons (codes, used in logs and doctor): `schema_newer`, `migration_failed`, `db_corrupt` (03's store),
`secrets_corrupt`, `secrets_owner` (a `secrets.json` owned by another uid), `data_not_mounted`,
`data_dir_not_writable` (the data directory can't be created or written, or its lock file can't be opened) and
`admin_socket_dir` (the admin socket's directory can't be created). `config.ReasonOf(err)` returns config's codes.

**Not refusals** (S44): what the server lacks from outside never stops the start. A certificate that can't be had
yet, in any mode, and a public address that the detection didn't find leave a running server that is not ready
(§6.2, §8.2), where the admin socket and doctor can say why. The certificate then arrives by itself once its
cause is gone (certmagic retries, manual files are polled); the address takes the restart of §6.2. Exit 78 is for
what only the operator can change on this machine.

### 6.4 Graceful shutdown: what clients see

On SIGTERM/SIGINT (systemd stop and restart both send SIGTERM, so every shutdown is treated as a possible restart),
and for a restore or `rotate-secrets` restart:

| Step | Time budget | Effect |
|---|---|---|
| 1. `readyz` → 503 `shutting_down` | 0 | Load balancers/monitors stop sending |
| 2. New `/ws` upgrades and API calls → 503 `server_shutdown` (`Retry-After: 5`) | 0 | |
| 3. `Hub.Shutdown(ctx, reason)` (01): every connection gets `server.shutdown{reason, reconnectInMs}` and `error{code: "server_shutdown", retryable: true, scope: "connection"}`, then WebSocket close **1012 (Service Restart)**. The wire reason is `restart` for SIGTERM, restore and rotation (SIGTERM can't tell stop from restart) | ≤ 2 s | The SPA shows "Server restarting… reconnecting" and reconnects after `reconnectInMs`; after the restart it rejoins and re-publishes (01 §10.5–§10.6) |
| 4. `SFU.Close()` closes all PeerConnections (concurrently; 02 guarantees it returns within 1 s); then `Transport.Close()` closes the muxes | ≤ 1 s | |
| 5. `http.Server.Shutdown` on all servers, then `PortMux.Close()` (closes the raw :443 listener and both sub-listeners; the ICE sub-listener may already be closed by `Transport.Close`, which is harmless) | ≤ 5 s | Idle keep-alives close |
| 6. Stop the TLS manager (`tlsmgr.Manager.Shutdown`, §8.2: no order, renewal or reload writes the certificate storage after it; S44), flush transfer counters, drain the push queue (≤ 2 s), close the admin socket, close the store (WAL checkpoint) | ≤ 2 s | |
| 7. Exit 0 (or re-exec, §6.5) | total ≤ `shutdown_timeout` (10 s) | |

A step that runs out of its budget closes by force what it still has (a request that won't finish, say), and the
shutdown goes on: everything is released all the same. The server logs a warning, `Shutdown` returns an error
wrapping `ErrShutdownForced`, and after a stop the process still exits 0.

A second SIGTERM/SIGINT exits immediately with code 1. 06 sets systemd `TimeoutStopSec=20` and compose
`stop_grace_period: 20s`.

### 6.5 Signals and re-exec

- `SIGHUP`: reload manual TLS files and `log.level` from the config file; everything else needs a restart.
  06: `ExecReload=/bin/kill -HUP $MAINPID`.
- No `sd_notify` in M1: 06's unit uses `Type=exec`, and install.sh polls `isshoni healthcheck --ready`. *Later*:
  `Type=notify` with `READY=1`/`STATUS=` if the unit ever needs it.
- Restart after a restore or `rotate-secrets`: `Run` returns `ErrRestartRequested` after a full graceful shutdown;
  `cmd/isshoni` then calls `syscall.Exec(os.Args[0], os.Args, os.Environ())`. The PID stays the same, so systemd and
  Docker (PID 1) see no exit. On Windows (compile-only in M1) it exits 75 instead.

### 6.6 Wiring (`internal/server/wire.go`)

The only place (with `cmd/isshoni` and `servertest`, §2) that imports 01's `signal` and `sfuplane`, 02's `sfu`, and
03's `store`, `auth` and `httpapi` together. Each adapter is a few lines and has a table test.

| Consumer interface | Implemented by | Mapping |
|---|---|---|
| `sfu.Config` | built by the wiring | `Transport` = `netx.NewTransport` (§7.3; built once in §6.1 step 6, the SFU calls `Apply`); `PauseUnwatchedLayers` = config `sfu.pause_unwatched_layers`; `Limits` = `Limits{MaxShareKbps: SettingsCache.Get().maxShareBitrateKbps}` at construction |
| `sfu.Deps.Events` (`RoomEvents`) and `signal.Deps.Media` (`MediaPlane`) | 01's `sfuplane.New`, then `Plane.Bind(sfu)` | 01 §15.4 |
| `signal.Authenticator` | 03's `auth.Service` | `AuthenticateRequest` → `AuthenticateCookie(r)` (`auth.ErrNoCookie` → `ErrNoCredentials`, `*api.Error{unauthenticated}` → `ErrInvalid`, other errors passed through; `Principal` → `Identity{UserID, Name: Username, Admin: Role == admin, SessionID}`); `Revalidate(ctx, id, ip)` → `Touch` + user re-read; `*api.Error{unauthenticated}` → `signal.ErrInvalid`, any other error passed through (transient); `AuthenticateBearer` → `ErrInvalid` in M1 (M2: `AuthenticateBearerToken`) |
| `signal.RoomDirectory` | 03's store | `GetRoom` → `RoomByID` (`store.ErrNotFound` → `signal.ErrNotFound`); `DefaultRoomID` → `store.DefaultRoomID`; `CanJoin` → nil |
| `signal.Deps.Policy` | 03's `SettingsCache.Get()` | `minClientVersion`, `maxParticipantsPerRoom`, `maxSharesPerRoom`, `maxShareBitrateKbps × 1000` |
| `signal.PushNotifier` | this doc's `push.Service.ShareStarted` | same fields (`PresentUserIDs`, `At`) |
| `auth.ConnCloser` | 01's `Hub.CloseConnections` | selector fields copied; `account_disabled` → `ErrorCodeAccountDisabled`, every other reason → `ErrorCodeSessionRevoked` |
| `auth.AdminAlerter` (`auth.Options.Alerts`) | `push.Service.AdminAlert` | payload `admin.alert` (§14.3); `nil` with `push.enabled=false` (alerts are then only logged) |
| `httpapi.Signal` | 01's hub | `RoomPresence`/`OnlineUserIDs` from `Hub.Snapshot()`; `RoomDeleted` → `CloseRoom`; `UserChanged` → `UpdateUser`; `Notify` → `Hub.Notify` |
| `httpapi.Push` | a wiring adapter over `push.Service` | `VAPIDPublicKey` and `ValidateEndpoint` passed straight through; `SendTest` converts `[]store.PushSubscription` to `[]push.Subscription` |
| `httpapi.InfoSource` | `internal/version`, `internal/protocol` | `ServerVersion`, `Protocol` |
| `httpapi.RouteObserver` (`RouterOptions.Observer`) | `ops.Metrics` | `ObserveRoute(pattern, status, bytes)` (§9.2) |
| 02's live limits | `SettingsCache.OnChange` | `SFU.SetLimits(Limits{MaxShareKbps: maxShareBitrateKbps})` |
| `ops.Policy` (`TransferAlertGB() int`, `ReleaseCheck() bool`) | 03's `SettingsCache.Get()` | `transferAlertGb`, `updateCheck`; read on every tick (§11.3, §11.5) |
| `ops.LiveSource`, `ops.AccountsSource`, `ops.Prober` | hub + SFU, 03's `API.DashboardAccounts`, `SFU.Probe` | §11.4, §7.7 |
| Offline DB functions of `ops` (backup, restore) and `ops/doctor` (`schema`) | 03's `store.LatestSchemaVersion`, `store.InspectFile`, `store.BackupFile` | passed as function values (§2); `cmd/isshoni` does the same for offline commands (§12.3, §12.4, §13.1) |
| `push.Store` | 03's store | §14.7; `store.ErrNotFound` becomes nil in `RecordResult` and `Delete`, and `""` with a nil error in `Meta` |
| `push.Options` | built by the wiring | `Subject` = `push.DeriveSubject(push.subject, tls.acme_email, Site.Origin)`; `VAPID` = `SecretStore.VAPID`; `OwnAddrs` = the detected `PublicAddrs.V4` and `V6`; `Metrics` = `ops.Metrics.Registerer()` (§14.1, §14.3) |
| `push.enabled=false` | – | the wiring passes `httpapi.Deps.Push = nil` and `auth.Options.Alerts = nil`, and does not build or start `push.Service`: it never calls `push.New`, which refuses `Options.Enabled == false` with `push.ErrDisabled` (§14.3); 03 then answers `push_unavailable` and omits `push` from `/info` |
| REST routes of `ops` and `push` | 03's `API.Handle` | each typed `ops`/`push` function is wrapped with `httpapi.DecodeJSON`/`WriteJSON`/`WriteError` and registered through `API.Handle` with the principal from `httpapi.PrincipalFrom` (§2): `POST /api/v1/conntest` (User), `GET /api/v1/admin/dashboard`, `GET\|POST /api/v1/admin/doctor`, `GET /api/v1/admin/bandwidth` (Admin) |
| Admin socket handlers | 03's `auth.Service`, `store.DB` | 03 §12.6 table |
| Metrics | `ops.Metrics.Registerer()` to 01; a `prometheus.Collector` over `SFU.Metrics()` | §11.2 |

## 7. Networking (`internal/server/netx`)

### 7.1 Listeners

| Port | Proto | Listener | Carries |
|---|---|---|---|
| 443 | TCP | `netx.PortMux` | first byte `0x16` → TLS (HTTPS, WSS, ACME tls-alpn-01); `0x00–0x02` → ICE-TCP |
| 80 | TCP | plain `http.Server` | ACME http-01, redirect to HTTPS (§8.3); the app itself in `off` mode (default `127.0.0.1:8080`) |
| 7882 | UDP | one `*net.UDPConn` per usable local IP, wrapped in `ice.UDPMuxDefault`, combined in `ice.MultiUDPMuxDefault` | All ICE/DTLS/SRTP media |
| 7882 | TCP | `net.Listener` → `ice.TCPMuxDefault` | ICE-TCP (the only TCP path in `off` mode) |
| 9469 | TCP (loopback) | `http.Server` | `/metrics`, `/healthz`, `/readyz`, optional pprof |
| – | unix | `/run/isshoni/admin.sock` | Admin API (§12) |

Docker port mappings must keep the same numbers (`443:443`, `7882:7882/udp`, `7882:7882/tcp`). Rationale: ICE
candidates advertise the container's port, and `webrtc.ICEAddressRewriteRule` in pion/webrtc v4.2.22 has no port
fields (pion/ice v4.4.4's `AddressRewriteRule` does: `OriginalPort`/`NewPort`); remapped ports are *later* when pion
exposes them. `doctor` states this in its Docker section.

### 7.2 The 443 first-byte multiplexer

```go
package netx

type PortMuxOptions struct {
	Addr             string        // ":443"
	ClassifyTimeout  time.Duration // 10 s: time allowed for the first byte
	MaxPending       int           // 1024 connections waiting for their first byte; when full, the oldest is closed
	MaxPendingPerIP  int           // 32 per IPKey
	MaxConnsPerIP    int           // limits.conns_per_ip (256): every open connection per IPKey, accept to close
	MaxICEConnsPerIP int           // 64 per IPKey; one count across 443 and 7882/tcp (§7.3)
	QueueLen         int           // 128 per sub-listener; a full queue for 1 s drops the connection
	PublicHost       string        // for the plain-HTTP hint response
	Counter          *TransferCounter
	Logger           *slog.Logger
}

type PortMux struct{ /* … */ }

func ListenPortMux(opts PortMuxOptions) (*PortMux, error)
func (m *PortMux) TLS() net.Listener // conns whose first byte was 0x16; the byte is replayed
func (m *PortMux) ICE() net.Listener // conns that start with an RFC 4571 frame; Addr() is the *net.TCPAddr of :443;
                                     // its Close is idempotent (Transport.Close closes it, then PortMux.Close again)
func (m *PortMux) Addr() net.Addr
func (m *PortMux) Close() error      // raw listener and both sub-listeners (Accept then returns net.ErrClosed),
                                     // then every conn that came through it and is still open; call it after
                                     // http.Server.Shutdown (§6.4): hijacked WebSockets, stuck handshakes
func (m *PortMux) Stats() PortMuxStats

type PortMuxStats struct {
	TLS, ICE, PlainHTTP, Garbage, Timeout, Limited uint64 // accepted-connection outcomes
}

// IPKey is the key of every per-IP count in netx (pending, open and ICE, on 443, 80 and 7882/tcp): an IPv4 address
// (IPv4-mapped IPv6 is unmapped first) as its /32, an IPv6 address as its /64. It gives the same result as 03's
// limiter key (03 §7.3) and 01's pre-auth key (01 §3.1).
func IPKey(a netip.Addr) netip.Prefix
```

Accept loop: the raw listener's `Accept` never blocks on classification; each connection gets a goroutine that sets a
10 s read deadline, reads **one** byte, clears the deadline and dispatches a `prefixConn` (a `net.Conn` whose `Read`
returns the peeked byte first; `LocalAddr`/`RemoteAddr` pass through, so pion gets its `*net.TCPAddr`).

Classification:

| First byte | Route | Why it is unambiguous |
|---|---|---|
| `0x16` | TLS | TLS handshake record type |
| `0x00`–`0x02` | ICE | RFC 4571 length prefix. pion's `TCPMuxDefault.handleConn` reads the first frame into a 512-byte buffer, so a valid first frame is ≤ `0x0200` bytes; a TLS record can never start with these values |
| `A`–`Z` | Plain HTTP on the TLS port | Reply `HTTP/1.1 400 Bad Request` with `This port speaks HTTPS. Use https://<host>/` and close |
| anything else | close | Counted as `Garbage`, like a connection that closes or resets before its first byte |

Handoff:
- `TLS()` goes to `http.Server.ServeTLS(listener, "", "")` with the tlsmgr `TLSConfig`. Go's server uses the smallest
  of `ReadHeaderTimeout`/`ReadTimeout`/`WriteTimeout` as the TLS handshake timeout; `ReadHeaderTimeout = 10 s` gives
  the plan's 10 s handshake limit.
- `ICE()` goes to `ice.NewTCPMuxDefault(ice.TCPMuxParams{Listener: m.ICE(), FirstStunBindTimeout: 10 * time.Second,
  AliveDurationForConnFromStun: 30 * time.Second, ReadBufferSize: 64, WriteBufferSize: 4 << 20, Logger: pionLog})`.
  The 7882/tcp listener uses the same `TCPMuxParams` (§7.3). DTLS then has 10 s too (02 sets it with
  `SetDTLSConnectContextMaker`).
- Per-IP counts are keyed by `IPKey`, so one IPv6 host that owns a /64 (every VPS has one) counts once, not once per
  address. netx may not import 03's `auth` (§2), so it has its own few-line function; a wiring unit test (§17) pins
  `netx.IPKey` and 03's limiter key to the same result.
- Per-IP counts are decremented when a connection closes (the wrapper's `Close` hook). Exceeding a per-IP limit closes
  the new connection immediately and counts it as `Limited`. `MaxConnsPerIP` counts every connection of an IPKey from
  accept to close, including ones still waiting for their first byte or getting the plain-HTTP hint, so it bounds all
  the sockets one IP holds on 443.
- A full pending pool works the other way round: when `MaxPending` connections are waiting for their first byte, the
  **oldest** pending connection is closed (counted as `Limited`) and the new one is admitted (after the per-IP pending
  check, which still closes the new one when its key already has 32 pending). Silent sockets then only
  push each other out; they can't lock friends out of 443. Pending connections sit in a FIFO list under the mux's
  mutex; the evicted one is taken off the list under the mutex when it is chosen (so a burst evicts one waiting
  connection per new one), and closing it makes its classifier goroutine's `Read` fail.
- Byte counters (`Counter`) wrap both routes: ICE → path `media_tcp`, TLS → path `web`.

HTTP/2, ALPN and WebSocket:
- `NextProtos = ["h2", "http/1.1", "acme-tls/1"]` (certmagic adds `acme-tls/1` and answers tls-alpn-01 challenges
  itself). The SPA and REST use HTTP/2.
- WebSocket signaling uses **HTTP/1.1 Upgrade**. Go's HTTP/2 server (checked in 1.26.5's `h2_bundle.go`) advertises
  `SETTINGS_ENABLE_CONNECT_PROTOCOL` (RFC 8441) only with `GODEBUG=http2xconnect=1`, so browsers open a separate
  HTTP/1.1 connection for `/ws`. We keep it that way (never set that GODEBUG): coder/websocket's HTTP/2 support is
  experimental.
- No h2c. In `off` mode the proxy talks HTTP/1.1 to isshoni.

Known limit: networks with deep packet inspection that allow only real TLS on 443 will block ICE-TCP there. TURN over
TLS is *later*.

### 7.3 ICE transports handed to the SFU

netx builds the sockets and muxes; 02 applies them to its `SettingEngine`s (publish, subscribe) and the connection
test (§7.7) uses the same ones. All PeerConnections share one UDP port and the TCP muxes, routed by ICE ufrag.

```go
package netx

type TransportOptions struct {
	UDPAddr           string   // listen.ice_udp; "" = no UDP
	TCPAddr           string   // listen.ice_tcp; "" = no 7882/tcp
	PortMux           *PortMux // 443 ICE sub-listener; nil in off mode
	MaxICEConnsPerIP  int      // 64 per IPKey; shares PortMux's ICE count when PortMux is set (one count for 443 and 7882)
	ExcludeInterfaces []string
	IncludeLoopback   bool
	IPv6              bool
	UDPBufferBytes    int
	Public            PublicAddrs
	InContainer       bool
	CloudProvider     api.CloudProvider // §13.3
	Counter           *TransferCounter
	PacketConns       []net.PacketConn // tests only: use these instead of binding UDPAddr (02's sfutest.FaultConn)
	Logger            *slog.Logger
}

type AdvertisedAddr struct {
	Proto string // "udp" | "tcp"
	Addr  netip.AddrPort
	Via   string // "udp" | "tcp443" | "tcp7882"
	LAN   bool   // kept private address (Append mode, §7.5)
}

type Transport struct {
	UDPMux          ice.UDPMux // *ice.MultiUDPMuxDefault; nil if UDP disabled
	TCPMux          ice.TCPMux // *ice.MultiTCPMuxDefault over 443 and 7882; nil if neither
	TCPMux443       ice.TCPMux // the 443 part alone (02's per-transport probe APIs); nil in off mode
	TCPMux7882      ice.TCPMux // the 7882 part alone; nil if listen.ice_tcp is ""
	NetworkTypes    []webrtc.NetworkType // udp4/udp6/tcp4/tcp6 as available
	RewriteRules    []webrtc.ICEAddressRewriteRule
	InterfaceFilter func(name string) bool
	IPFilter        func(ip net.IP) bool
	IncludeLoopback bool
	Advertised      []AdvertisedAddr // every advertised address with its final address:port after rewrite rules
	RcvBuf, SndBuf  int // effective socket buffers read back with getsockopt
}

func NewTransport(ctx context.Context, opts TransportOptions) (*Transport, error)

// Apply configures a SettingEngine: SetICEUDPMux, SetICETCPMux, SetNetworkTypes, SetInterfaceFilter,
// SetIPFilter, SetICEAddressRewriteRules, SetIncludeLoopbackCandidate, and mDNS disabled.
// 02 calls it for each webrtc.API it builds, then adds its own settings (DTLS timeout, ICE timeouts…).
// Apply only calls SettingEngine setters and keeps no other state, so a later setter call overrides it
// (02's probe APIs rely on this).
func (t *Transport) Apply(se *webrtc.SettingEngine) error

func (t *Transport) Close() error
```

Construction details:
- **UDP**: enumerate interfaces with the same filter pion uses (skip down, loopback unless `include_loopback`, and
  `exclude_interfaces` globs), bind `ListenUDP("udp", ip:7882)` per address (the same as
  `ice.NewMultiUDPMuxFromPort`), set SO_RCVBUF/SO_SNDBUF to `udp_buffer_bytes`, wrap each in a byte-counting
  `PacketConn`, then `ice.NewUDPMuxDefault` per socket and `ice.NewMultiUDPMuxDefault(muxes...)`. We build it
  ourselves only to count bytes and read back buffer sizes. The counting wrapper implements
  `ice.AddrPortReaderWriter` (`ReadFromAddrPort`/`WriteToAddrPort`) so pion keeps its allocation-free path. Port `0`
  (tests): bind the first address, reuse its port for the others. If `RcvBuf` or `SndBuf` read back below
  `udp_buffer_bytes`, `NewTransport` logs one warn line (`component=netx`) naming the sysctl fix; doctor's
  `udp_buffers` check reports the same (§13.2). The SFU does not log buffer sizes.
- **TCP**: `ice.NewTCPMuxDefault` on the 443 ICE sub-listener and on a plain 7882 listener, combined with
  `ice.NewMultiTCPMuxDefault(mux443, mux7882)`. pion's gatherer uses `GetAllConns` of the multi mux, so each local
  address gets a passive TCP candidate on **both** 443 and 7882. Both sub-listeners report an unspecified IP, so pion
  advertises every local address (then filtered and rewritten). The 7882 listener is wrapped like the 443 ICE route:
  a per-IP limit of `MaxICEConnsPerIP` (64) ICE connections per `IPKey` (IPv4 address or IPv6 /64, §7.2), counted
  together with 443 (excess closed and counted as `Limited`), a byte counter (path `media_tcp`), and the same `ice.TCPMuxParams` (`FirstStunBindTimeout` 10 s,
  `AliveDurationForConnFromStun` 30 s, `ReadBufferSize` 64, `WriteBufferSize` 4 MiB).
- **Advertised**: `Advertised` holds every advertised address with its final address:port after the rewrite rules
  (§7.5). 02 uses it to label selected candidate pairs (`Via`); the same value set `udp` | `tcp443` | `tcp7882` is used
  everywhere (dashboard, metrics). It lists exactly what pion gathers, no more: a test compares it with the
  candidates of a real offer. The one kept address that gets no TCP entry is the IPv6 loopback `::1` (§7.6).
- Interfaces are enumerated once at startup (06: systemd `After=network-online.target`). Hot-plugged interfaces need a
  restart.

### 7.4 Public IP detection and NAT classification

```go
package netx

type Method string // "config" | "interface" | "stun" | "none"

// NAT kinds are api.NATKind (internal/protocol/api/conntest.go, §7.7):
// "none" | "one_to_one" | "port_forward" | "symmetric" | "cgnat_likely" | "unknown".

type PublicAddrs struct {
	V4, V6        netip.Addr // zero if none
	V4Method      Method
	V6Method      Method
	LocalV4       netip.Addr // IPv4 of the default-route interface (may equal V4)
	NAT           api.NATKind
	STUNMapped    []netip.AddrPort // per STUN server, for doctor
	DetectedAt    time.Time
}

type DetectOptions struct {
	PublicIP, PublicIPv6 string   // config values: "auto", "off" or literals
	STUNServers          []string
	Timeout              time.Duration // 2 s per server; whole detection ≤ 5 s
	IPv6                 bool
	STUN                 STUNClient    // real: pion/stun/v4 over an ephemeral UDP socket
	Interfaces           InterfaceLister
}

func DetectPublicAddrs(ctx context.Context, opts DetectOptions) (PublicAddrs, error)

type STUNClient interface {
	// Mapped sends a Binding request from one local socket to server and returns XOR-MAPPED-ADDRESS.
	Mapped(ctx context.Context, localSocket net.PacketConn, server string) (netip.AddrPort, error)
}
```

IPv4 algorithm:
1. `public_ip` literal → use it (`Method=config`), still run STUN once for doctor's NAT report, never for the result.
2. `LocalV4` = source address the kernel picks for `192.0.2.1:9` (UDP "connect", no packet is sent).
3. Query both STUN servers in parallel from **one** ephemeral UDP socket (2 s timeout, 1 retry).
4. Classify:

| Observation | `V4` | `NAT` |
|---|---|---|
| STUN IP equals a local interface address | that IP (`interface`) | `none` |
| No STUN answer, a public (global unicast, not CGNAT) IPv4 on an interface | that IP (`interface`) | `unknown` |
| STUN IP ≠ local, both servers agree on IP **and** port | STUN IP (`stun`) | `one_to_one` if a cloud provider or a container is detected, else `port_forward` |
| Servers see different mapped ports | STUN IP | `symmetric` (typical of CGNAT; friends probably can't connect) |
| `LocalV4` in `100.64.0.0/10` on a non-Tailscale interface | STUN IP | `cgnat_likely` |
| Nothing found | none | `unknown` (ip mode not ready; doctor explains) |

Tailscale interfaces (`tailscale*`, `utun*` with 100.64/10) are ignored for the CGNAT rule. Detection runs once at
startup and then every 10 minutes; a change is logged as a warning and shown on the dashboard ("Public IP changed from
A to B; restart isshoni to apply"). Rationale: rewrite rules are fixed per `webrtc.API`; a hot swap is *later*. VPS
addresses rarely change.

**"Nothing found" at startup** (the table's last row, and its `cgnat_likely` row when no STUN server answered:
no address then either, with the NAT kind `cgnat_likely`; S44, kept after group 5): `DetectPublicAddrs` returns
what it found, and an error next to it when a step failed; neither stops the start. In ip mode, and in manual mode
without a domain, the server then starts without a site and is not ready; §6.2 lists what the operator sees and
why it is not an exit 78. Once the periodic detection is wired (README S54; §6.2), it goes on for such a server
as for any other. An address it finds later is a change like "A to B": logged, and applied only by a restart.
The server does not take it by itself, and nothing restarts it (§6.2).

Privacy: STUN contacts Cloudflare and Google at startup and every 10 minutes (one UDP packet each). Setting `public_ip`
limits this to one NAT check at startup; `network.stun_servers = []` stops STUN entirely (set `public_ip` to a literal
then). The privacy page says so (§16).

### 7.5 Address rewrite rules

Goal: advertise exactly the addresses friends can reach.

| Situation | Rules | Filters |
|---|---|---|
| Public IP is on an interface (Hetzner, DigitalOcean, Vultr…) | none | IPFilter keeps global addresses (and loopback in dev); private VPC addresses are not advertised |
| 1:1 NAT (AWS, GCP, Oracle, Azure) or Docker bridge | `{External: [V4], Local: LocalV4, AsCandidateType: host, Mode: Replace, Networks: [udp4, tcp4]}` | IPFilter keeps `LocalV4` |
| Home/LAN server behind a router (no cloud, no container, `LocalV4` private) | same rule with `Mode: Append` | keeps `LocalV4`, so LAN friends connect directly and remote friends use the forwarded public address |
| IPv6 global address on an interface | none | kept |
| `public_ipv6` literal not on an interface | `{External: [V6], Mode: Replace, Networks: [udp6, tcp6]}` | – |

`Networks` includes TCP so the 443/7882 passive candidates are rewritten too. pion/ice v4.4.4 applies rewrite rules to
UDP-mux host candidates (`applyHostRewriteForUDPMux`), including `Replace` and `Append`.

S4 finding 6 (macOS Local Network privacy blocks LAN ICE for browsers without the permission) matters only for the
Append/LAN case and for local development; doctor shows it on macOS hosts (§13.2).

### 7.6 IPv6

- The UDP mux binds global IPv6 addresses when `network.ipv6` is true and one exists (ULA `fc00::/7`, link-local and
  temporary privacy addresses are skipped).
- `::1` (development: `network.include_loopback` with `network.ipv6`) carries **UDP only**. pion gathers UDP
  candidates from the UDP mux's sockets, so `[::1]:7882/udp` is gathered and advertised. It gathers passive TCP
  candidates from its own interface scan, which drops every address in `::/96` as a deprecated IPv4-compatible one
  (RFC 8445 §5.1.1.1), `::1` included, whatever `include_loopback` says. So `Transport` keeps `::1` out of the
  addresses for ICE-TCP: `Advertised` has no `tcp` entry on `::1` (443 or 7882), `IPFilter` drops it, and `::1`
  alone adds no `tcp6` network type. An ICE-TCP listener bound to `[::1]` alone advertises nothing; with no UDP
  socket either, `NewTransport` fails with `ErrNoTransport`. Loopback TCP in development uses 127.0.0.1.
- Docker bridge networks have no IPv6 by default; doctor reports "IPv6 media off (Docker bridge)".
- `tls.mode=ip` certifies **one** address: the IPv4 if present, otherwise the IPv6. A second certificate for IPv6 is
  *later*.

### 7.7 Connection test endpoint (server side of the wizard's step 2)

05's wizard (and "Test my connection" for any signed-in user) runs real ICE checks per transport. This doc owns the
REST endpoint (`ops/conntest.go`); 02 owns the probe PeerConnections (`SFU.Probe`, 02 §7.6), reached through the
small `ops.Prober` interface.

```
POST /api/v1/conntest            access: User (03 API.Handle) · CSRF: 03
Request:  {"transport": "udp" | "tcp443" | "tcp7882", "offer": "<SDP with one data channel>"}
Response: 200 {"answer": "<SDP>", "expiresInS": 20,
               "server": {"publicIp": "203.0.113.7", "provider": "hetzner", "container": "none",
                          "udpPort": 7882, "tcpPorts": [443, 7882], "nat": "none"}}
Errors:   400 bad_sdp · 409 transport_disabled (params.transport) · 429 rate_limited · 503 not_ready
```

- 409 `transport_disabled` (with `params.transport`) is returned when that transport has no listener: `tcp443` in off
  mode, `udp` when `listen.ice_udp = ""`, `tcp7882` when `listen.ice_tcp = ""`. 02's `Probe` returns
  `sfu.transport_disabled` whenever the mux for the requested transport is nil.
- `server.publicIp` is `""` when no public IPv4 is known (dev or CI without STUN). `server.container` is `none` |
  `docker` | `podman` | `other`, the same values as doctor's `env.container` (§13.5), from `InContainer` detection.

```go
package ops

type Prober interface {
	// The wiring adapts 02's SFU.Probe: it converts userID/transport to sfu.UserID/sfu.ProbeTransport, discards
	// the result channel, and maps sfu.probe_limit to 429 rate_limited and sfu.transport_disabled to 409.
	Probe(ctx context.Context, userID string, transport string, offerSDP string) (answerSDP string, err error)
}
```

- 02 builds one probe `webrtc.API` per transport from `Transport.Apply` with only that transport's mux
  (`UDPMux`, `TCPMux443`, `TCPMux7882`), so the answer is complete, non-trickle and holds only that transport's
  candidates. The browser's own candidates are not needed: its checks create peer-reflexive candidates on the server.
- The probe PC echoes every data-channel message (the browser measures RTT; `getStats` `currentRoundTripTime` also
  works). It closes after 20 s or when the channel closes. ICE timeouts: disconnected 3 s, failed 8 s; DTLS 10 s.
- Limits: 12 requests per minute per user here (429 `rate_limited` with `Retry-After`); in 02, one live probe per
  (user, transport), where a new one replaces the old, and 20 globally (`sfu.probe_limit` → 429).
- Probe answers hold IPv4 candidates only when a public IPv4 is known (02 §7.6).
- `provider` (§13.3) and `nat` (§7.4) let 05 show provider-specific fix text from its catalog; 05 shows it to admins
  only.

Types (`internal/protocol/api/conntest.go`): `ConnTestRequest`, `ConnTestResponse`, `ConnTestServerInfo`, plus
`type CloudProvider string` with one constant per §13.3 id and `type NATKind string` with the §7.4 values, so
`task gen` puts them in `api.gen.ts` for 05's check and 06's anchor test. `ConnTestServerInfo.Provider` and `.NAT` use
these types, and `netx` and `doctor` use them instead of their own.

### 7.8 Limits and timeouts (summary)

| Item | Value |
|---|---|
| 443 first byte | 10 s |
| TLS handshake (via `ReadHeaderTimeout`) | 10 s |
| ICE-TCP first STUN Binding | 10 s on 443 and 7882; connection created from STUN with unknown ufrag lives 30 s |
| DTLS handshake (02 sets it) | 10 s |
| Per-IP key (every row below that says "per IP") | IPv4 address, or the IPv6 /64 (`netx.IPKey`, §7.2) |
| Pending (unclassified) connections | 1024 total (when full, the oldest pending one is closed), 32 per IP |
| Open connections per IP on 443/80 | 256 (`limits.conns_per_ip`) |
| ICE-TCP connections per IP | 64 across 443 and 7882 |
| HTTP servers | `ReadHeaderTimeout` 10 s, `IdleTimeout` 120 s, `MaxHeaderBytes` 16 KiB, no server `ReadTimeout`/`WriteTimeout` (WebSocket) |
| Main server request read deadline | 30 s per request via `http.ResponseController`, except `/ws` (§9.3) |
| Port 80 server | `ReadHeaderTimeout` 5 s, `IdleTimeout` 30 s |
| JSON request bodies | 64 KiB default (`httpapi.DecodeJSON`) |
| UDP socket buffers | 8 MiB requested |
| TCPMux write buffer | 4 MiB per connection (pion's recommendation) |
| STUN | 2 s per server, 1 retry, ≤ 5 s total |
| Shutdown | ≤ 10 s |

---

## 8. TLS (`internal/server/tlsmgr`)

### 8.1 Modes

| Mode | Certificate | Name | Challenges | Port 80 |
|---|---|---|---|---|
| `auto` | Let's Encrypt via ACME (default profile, 90 days) | `domain` | http-01 (80) and tls-alpn-01 (443) | ACME + redirect |
| `ip` | Let's Encrypt **`shortlived` profile** (160 hours), IP identifier (RFC 8738) | public IP | http-01 and tls-alpn-01 (DNS-01 can't prove an IP) | ACME + redirect |
| `manual` | User files (`cert_file`, `key_file`), reloaded on change | whatever the file covers | – | redirect |
| `off` | None; a proxy terminates TLS | – | – | the app itself |

Both challenge types stay enabled, so issuance works when only one of 80 or 443 is open.

### 8.2 certmagic version and setup

- **`github.com/caddyserver/certmagic` v0.25.4 or newer** (v0.25.4 is from June 2026). Needed features: ACME
  `Profile` (since v0.22.0); IP identifiers allowed for Let's Encrypt in `ACMEIssuer.PreCheck` (PR #345, merged
  July 2025, first in v0.24.0); HTTP-01 for IPv6 literals fixed in v0.25.3. Caddy issue #7399 (Dec 2025) reported
  a PreCheck refusal for IP certificates with an older build, so slice S7 must issue a real staging IP certificate
  on a VPS before M1 is done.
  - **Built with v0.25.6, not v0.25.4** (README S44; `go.mod`, with `github.com/mholt/acmez/v3` v3.1.7). README
    §5's rule for shared modules is that the first slice to need a module adds it at the current release, and
    that is where v0.25.6 comes from. Every feature listed here is in it: they all arrived by v0.25.3. So
    v0.25.4 is a floor, not a pin to hold: Dependabot moves the module from here on, and the Pebble job (a domain
    and an IP certificate over http-01 and tls-alpn-01, a renewal, `serve` in auto mode) is what a bump has to
    pass.
- certmagic logs through `*zap.Logger`; `logx.NewZapBridge(slog)` is a ~60-line `zapcore.Core` that forwards to slog
  (component `tls`).

```go
package tlsmgr

type Options struct {
	Mode        config.TLSMode
	Domain      string
	PublicIP    netip.Addr // ip mode (and manual without domain)
	Email       string
	CA          string
	Staging     bool
	CARootFile  string // tests
	CertFile    string // manual
	KeyFile     string // manual
	StorageDir  string // <data_dir>/certmagic
	HSTS        bool
	Logger      *slog.Logger

	// Added by README S44; the manager needs them from the wiring (below).
	PublicIPv6          netip.Addr    // the public IPv6 address next to PublicIP, when there is one
	Site                config.Site   // Origin: the redirect target of port 80; Hostname: manual mode's SAN warning
	HTTPAddr, HTTPSAddr string        // the bound "host:port" of the port 80 and the 443 listener
	Resolver            netx.Resolver // auto mode's DNS check (§8.7); nil = net.DefaultResolver
}

type Status struct {
	Mode        config.TLSMode `json:"mode"`
	Names       []string       `json:"names"`
	Ready       bool           `json:"ready"`
	Issuer      string         `json:"issuer,omitempty"`
	NotBefore   time.Time      `json:"not_before,omitzero"`
	NotAfter    time.Time      `json:"not_after,omitzero"`
	NextRenewal time.Time      `json:"next_renewal,omitzero"`
	LastError   string         `json:"last_error,omitempty"`   // English, for logs/CLI
	LastErrorCode string       `json:"last_error_code,omitempty"` // §8.7
	LastErrorAt time.Time      `json:"last_error_at,omitzero"`
}

type Manager struct{ /* … */ }

func New(opts Options) (*Manager, error)
func (m *Manager) Start(ctx context.Context) error   // ManageAsync / load files / start file watcher
func (m *Manager) Shutdown(ctx context.Context) error // S44: ends the work; nil = nothing writes StorageDir any more
func (m *Manager) TLSConfig() *tls.Config            // nil in off mode
func (m *Manager) HTTPHandler(app http.Handler) http.Handler // port 80 (§8.3); app is used only in off mode
func (m *Manager) Reload() error                     // manual: re-read files (SIGHUP)
func (m *Manager) Status() Status
func (m *Manager) Ready() (bool, string)             // readiness check "tls"
```

**The manager as README S44 built it** (`tlsmgr/manager.go`), beyond the block above:

- **`Options` has four more fields**, all filled by the wiring from the site and the listeners it bound
  (`internal/server/tls.go`):
  - `PublicIPv6`. `PublicIP` is the one address that stands for the server (the IPv4 one, or the IPv6 one on an
    IPv6-only host, §7.6): the certificate's name in ip mode. A dual-stack server also has `PublicIPv6`, which auto
    mode reads wherever it asks "is this address mine?": in its DNS check and for the hint of a failed validation
    (§8.7).
  - `Site`. The port 80 handler redirects to `Site.Origin` (never to the request's `Host`, §8.3), and manual mode
    warns when the certificate doesn't cover `Site.Hostname` (§8.4).
  - `HTTPAddr` and `HTTPSAddr`. certmagic binds a challenge port only when it finds it free. The manager names the
    two bound listeners to its ACME issuer (`ListenHost`, `AltHTTPPort`, `AltTLSALPNPort`), which finds them taken
    and opens none of its own: the challenges arrive at `HTTPHandler` and `TLSConfig`. Empty means ports 80 and 443
    on every address.
  - `Resolver`, for the DNS check; the wiring passes `server.Deps.Resolver`, so tests fake it.

  `HSTS` stays in the struct and the manager doesn't use it: `Strict-Transport-Security` belongs to HTTPS
  responses, which the router writes (§9.6), never to a response on port 80 (RFC 6797 §7.2). A nil `Logger` means
  `slog.Default()`; the manager adds `component=tls`.
- **`Shutdown(ctx)`** is new. It cancels the orders and renewals in flight and the file watcher, then waits, until
  `ctx` ends, for the manager's goroutines, for certmagic's maintenance loop and for certmagic to let go of the
  storage directory (a wrapper around `certmagic.FileStorage` counts what goes on in it). After a nil return
  nothing writes below `StorageDir`, so a restore may replace the directory (§12.4 step 5). When `ctx` ends first
  the error wraps `ctx`'s and the stop goes on in the background; a later or concurrent call waits for that same
  stop, so nil means the same from every call. The certificate that is loaded keeps serving the handshakes of
  connections still open. The server calls it in step 6 of the shutdown (§6.4), after the HTTP servers, when
  nothing handshakes any more. A `Manager` is not started again: `Start` after `Shutdown` is an error.
- **`New` starts nothing and reads no file.** It refuses only what can't be a configuration: auto mode without a
  domain, manual mode without both files, auto or ip mode without a `StorageDir`. `Start` returns at once; its only
  errors are a `StorageDir` that can't be created and a `CARootFile` that can't be used. `TLSConfig`, `HTTPHandler`,
  `Status` and `Ready` work before `Start` and after `Shutdown`.
- **A certificate that can't be had is never a failed start.** The server runs without it and is not ready
  (§6.2), so that doctor and the admin socket can say why. That covers a manual pair that can't be loaded (§8.4),
  an ACME order that keeps failing (§8.7), and **ip mode without a public address** (`PublicIP` is the zero
  `Addr`): the manager then has no name to ask a certificate for. It starts no ACME work, logs one warning, and
  `Ready` answers false with "no public IP address to get a certificate for" (§6.2 has what the operator sees).
- **`Ready`** is true when a certificate is loaded and the clock is inside its validity; an expired one, or one
  that isn't valid yet, is not ready and says which. While there is none, the detail names what the manager waits
  for, with the last error's code: "getting a certificate for 203.0.113.7 (tls.acme_unreachable)". `Status.Names`
  is never null; in manual mode it lists the certificate's own names.
- **certmagic setup**, in addition to the listing below: `ShouldEmitFunc` lets through only `cert_obtaining`,
  `cert_obtained` and `cert_failed` (certmagic would build an event for every handshake otherwise); the first
  `cert_obtaining` that isn't a renewal starts auto mode's DNS check (§8.7); a `cert_failed` that the shutdown
  caused is logged at debug and sets no error. `Staging` selects Let's Encrypt's staging directory only when `CA`
  is empty or Let's Encrypt's production one; it has no effect on another CA.

certmagic configuration (auto and ip):

```go
cache := certmagic.NewCache(certmagic.CacheOptions{GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) { return cfg, nil }, Logger: zl})
cfg = certmagic.New(cache, certmagic.Config{
	Storage:            &certmagic.FileStorage{Path: opts.StorageDir},
	DefaultServerName:  name, // IP clients send no SNI (RFC 6066 forbids IP literals in SNI)
	FallbackServerName: name, // unknown SNI gets our cert and a clear name-mismatch error instead of a handshake failure
	RenewalWindowRatio: ratio, // auto: certmagic default (1/3); ip: 0.5, so a 160 h cert renews after ~80 h with 3 days of slack
	OnEvent:            m.onEvent, // cert_obtained / cert_failed / cached_managed_cert → Status, readiness
	OCSP:               certmagic.OCSPConfig{DisableStapling: true}, // no OCSP requests (§16)
	Logger:             zl,
})
issuer := certmagic.NewACMEIssuer(cfg, certmagic.ACMEIssuer{
	CA: ca, Email: opts.Email, Agreed: true,
	Profile: profile, // "" in auto mode, "shortlived" in ip mode (Let's Encrypt requires it for IP identifiers)
	TrustedRoots: pool, // only with tls.acme_ca_root
	Logger: zl,
})
cfg.Issuers = []certmagic.Issuer{issuer}
cfg.ManageAsync(ctx, []string{name}) // name = domain, or the IP string
```

- `certmagic.UserAgent = version.UserAgent()`.
- OCSP stapling is disabled (`OCSP.DisableStapling = true`): Let's Encrypt no longer runs OCSP, and the server should
  make no outbound requests the privacy page (§16) doesn't list. Manual certificates are loaded without certmagic
  (§8.4), so they cause no OCSP requests either.
- The TLS config gets `MinVersion: tls.VersionTLS12` and `NextProtos` from §7.2.
- certmagic answers challenges through our listeners: `HTTPChallengeHandler` on port 80 and `acme-tls/1` in the TLS
  config on 443. It never opens its own ports.
- `Agreed: true`: install.sh and the docs tell the admin that Let's Encrypt's Subscriber Agreement applies (06).
- Rate limits: a 160-hour certificate renewed at half-life is about 2 issuances per week, under Let's Encrypt's 5 per
  week per identifier set, and ARI-driven renewals are exempt. Reinstalling without the `certmagic/` directory is what
  burns the limit, so backups include it (§12.3) and doctor names the rate-limit error.

### 8.3 Port 80

`HTTPHandler`:
1. `/.well-known/acme-challenge/*` → certmagic (auto, ip).
2. `off` mode → the app handler (the proxy connects here).
3. While the certificate is not ready (auto, ip): `503` with `Retry-After: 30` and a tiny page "isshoni is getting its
   certificate. Refresh in a minute." Rationale: redirecting to a broken HTTPS page looks like a failed install.
4. Otherwise `308` to `Site.Origin` + path + query. The target host comes from config, never from the request `Host`
   (no open redirect).

As S44 built it (`tlsmgr/redirect.go`): a request under `/.well-known/acme-challenge/` that is no open challenge
is a 404, never a redirect. The page of step 3 reloads itself every 30 s (`<meta http-equiv="refresh">`) and is
sent with `Cache-Control: no-store`. **A server without a site** (ip mode, or manual mode without a domain, that
found no public address, §6.2) has no origin to redirect to: it answers every other request with the same 503
page, in manual mode too. The port 80 server speaks HTTP/1.1 only, bounds every exchange at 30 s, and applies
`limits.conns_per_ip` like the 443 multiplexer (§7.8).

### 8.4 Manual mode

- Load with `tls.LoadX509KeyPair`; keep it in an `atomic.Pointer[tls.Certificate]`; `GetCertificate` returns it.
- Reload on `SIGHUP` and when either file's mtime or size changes (polled every 60 s; no fsnotify dependency). A bad
  new pair is rejected and logged; the old one keeps serving.
- Checks at load (errors keep the server not ready; warnings go to doctor): key matches cert; not expired; SANs cover
  `Site.Hostname` (warning); expires within 14 days (warning); files readable by the service user (error with fix).

As S44 built it (`tlsmgr/manual.go`). A load that fails sets `Status.LastErrorCode` to one of three codes of its
own (§8.7 lists them with their fix texts), and the readiness check `tls` fails while no valid certificate serves:

| What the load finds | Code | What serves |
|---|---|---|
| a file is missing, may not be read by the service user, or can't be read | `tls.cert_unreadable` | the pair loaded before, if any |
| the two files are not a certificate with its private key, or the first certificate can't be parsed | `tls.cert_invalid` | the pair loaded before, if any |
| the certificate is expired, or not valid yet | `tls.cert_expired` | the old pair while that one is still valid; otherwise the new one, although the server is not ready: a browser then says "expired", which tells more than a failed handshake |

- A failed load at startup is no error of `Start`: the server runs without a certificate, not ready, until good
  files appear. A load that failed is not repeated until a file changes again, so a broken pair is logged once and
  not every minute.
- The poll compares each file's modification time and size with what the last load saw (taken before reading, so
  a file replaced during the read is caught by the next poll). A tool that replaces the two files one after the
  other may be caught in between: that load is rejected (`tls.cert_invalid`), the old pair keeps serving, and the
  next poll finds the second file changed.
- The two warnings are log lines with a `fix` attribute: the certificate doesn't cover `Site.Hostname` ("browsers
  will show a warning"), and fewer than 14 days are left. Neither touches readiness or `Status.LastError`.
- `Reload()` returns the load's error, which says what is wrong with the files and how to fix it. `serve` calls it
  on SIGHUP once the hardening slice handles that signal (§6.5, README S90); until then the 60 s poll is what
  picks up new files.

### 8.5 Off mode and trusted proxies

- The app is served over plain HTTP on `listen.http`. There is no 443 multiplexer, so ICE-TCP uses 7882 only (the
  proxy owns 443).
- `X-Forwarded-For` and `X-Forwarded-Proto` are honored only when the TCP peer is inside `network.trusted_proxies`.
  With a loopback `listen.http` that defaults to `127.0.0.0/8` and `::1/128` (§4.4), so a proxy on the same host works
  without setting it. Client IP = the right-most `X-Forwarded-For` entry that is not itself a trusted proxy. `X-Forwarded-Host` and
  `Forwarded` are ignored; links always use `Site.Origin`.
- In `auto`, `ip` and `manual` modes all `X-Forwarded-*` headers are ignored.
- 06 documents example Caddy/nginx configs, including the WebSocket upgrade headers.

### 8.6 Why there is no self-signed mode

WKWebView/WebView2 apps, installed PWAs, service workers (so Web Push) and iOS Safari all reject or permanently warn on
untrusted certificates, and there is no one-click fix for friends. Every mode therefore ends with a publicly trusted
certificate, or a proxy that has one. Tests use a private CA through `servertest` (§17), never a product mode.

### 8.7 ACME error hints

`hints.go` maps ACME problems (`errors.As` to `acme.Problem` from mholt/acmez, a certmagic dependency) to stable codes
used by logs, `Status.LastErrorCode` and doctor:

| ACME problem | Code | Fix text (English template) |
|---|---|---|
| `connection` | `tls.acme_unreachable` | Let's Encrypt couldn't connect to {name} on port 80 or 443. Open TCP 80 and 443 in your cloud firewall |
| `dns`, NXDOMAIN | `tls.dns_missing` | {domain} doesn't resolve. Add an A record pointing to {ip} |
| `unauthorized` / wrong response | `tls.dns_wrong` | {domain} points to {other}, but this server is {ip} |
| `rateLimited` | `tls.rate_limited` | Let's Encrypt rate limit until {retry_after}. Restore `certmagic/` from a backup if you reinstalled |
| `rejectedIdentifier` | `tls.ip_rejected` | Let's Encrypt refused {ip} (not a public address?) |
| `caa` | `tls.caa_forbids` | A CAA record on {domain} doesn't allow letsencrypt.org |
| other | `tls.acme_failed` | {detail} |

In `auto` mode, before the first order, tlsmgr resolves the domain and logs `tls.dns_wrong` early if no A/AAAA record
matches the public IP (it still tries: DNS may be split-horizon).

**As S44 built the hints** (`tlsmgr/hints.go`; the codes are constants there: `CodeACMEUnreachable` …
`CodeCertExpired`).

*Three rows are narrower than their ACME problem type.* The CA uses one type for several causes, and a fix text
must not send the operator the wrong way. What a row leaves out is `tls.acme_failed` with the CA's own `detail`:

| ACME problem | The row applies when | Otherwise `tls.acme_failed`, because |
|---|---|---|
| `dns` | the name is a domain and the CA's detail says it has no address record (`NXDOMAIN`, "no valid A records", "No valid IP addresses found") → `tls.dns_missing` | the CA also says `dns` for SERVFAIL, a timeout and a failed CAA lookup. The record may well be there then, and "add an A record" would be wrong advice |
| `unauthorized` | the name is a domain, and the address the CA reached (the head of its detail, "203.0.113.9: Invalid response…") is not one of this server's (`PublicIP`, `PublicIPv6`), or the detail names none → `tls.dns_wrong` | when the CA did reach this server, DNS is right and something else on this machine answered the challenge; and an IP address (ip mode) has no DNS record to fix |
| `rejectedIdentifier` | the name is an IP address → `tls.ip_rejected` | a refused domain is not an address problem |

`connection`, `rateLimited` and `caa` map as the table above says. An error that carries no ACME problem at all
(the CA can't be reached from here, a broken directory URL) is `tls.acme_failed` with the error's text.

*The fix texts take what is known.* `tls.dns_missing` ends "pointing to {ip}", or "pointing to this server" when no
public address is known. `tls.dns_wrong` has four forms, from "{domain} points to {other}, but this server is
{ip}" down to "{domain} doesn't point to this server" when neither address is known. `tls.rate_limited` names the
time when the CA's detail has one ("retry after 2026-10-09 12:30:00 UTC") and says "rate limit reached" otherwise.
`{detail}` is the CA's own text, cut to one line of at most 300 bytes.

*Three more codes, for manual mode* (§8.4). They are `Status.LastErrorCode` values like the seven above, so logs,
the dashboard (`api.TLSInfo.lastErrorCode`) and doctor's `tls` check carry them, and 05 needs a text for each when
it shows them (README S91):

| Code | Cause | Fix text |
|---|---|---|
| `tls.cert_unreadable` | `tls.cert_file` or `tls.key_file` is missing, or the service user may not read it | "tls.key_file = /etc/ssl/k.pem does not exist. Check the path." · "isshoni may not read tls.key_file = …. Let the user that isshoni runs as read it, for example: sudo chgrp isshoni … && sudo chmod 640 …" · "isshoni can't read tls.key_file = …." |
| `tls.cert_invalid` | the files are not a certificate with its private key | "tls.cert_file and tls.key_file are not a certificate with its private key (…). Put the full chain in tls.cert_file and the matching key in tls.key_file, both as PEM." |
| `tls.cert_expired` | the certificate is past `notAfter`, or before `notBefore` | "The certificate in tls.cert_file expired on 2026-10-01. Replace it and its key; isshoni loads new files within a minute." · "… is not valid before 2026-10-20. Check the server's clock, or use a certificate that is valid now." |

*The DNS check* runs once, when certmagic begins the first order that isn't a renewal, with a 5 s timeout and
`Options.Resolver`. It compares the domain's A and AAAA records with `PublicIP` and `PublicIPv6` and only logs:
`tls.dns_missing` when the name doesn't resolve, `tls.dns_wrong` when no record is one of this server's
addresses. It never sets `Status.LastError`, which the CA's answer decides, and it is skipped when no public
address is known. An attempt that fails logs one WARN line with the code and the fix ("could not get a
certificate; trying again later", or "could not renew…"); certmagic retries with its own backoff, and a success
clears the error.

---

## 9. HTTP wiring (`internal/server/httpapi`)

### 9.1 Servers

| Server | Listener | Handler |
|---|---|---|
| main | `PortMux.TLS()` (`auto`/`ip`/`manual`) or `listen.http` (`off`) | router (§9.2) |
| port 80 | `listen.http` (non-off) | `tlsmgr.HTTPHandler` |
| metrics | `metrics.listen` | `/metrics`, `/healthz`, `/readyz`, `/debug/pprof/*` |
| admin | unix socket | admin API (§12) |

`http.Server.Protocols` = HTTP/1 + HTTP/2 over TLS. `ErrorLog` goes to slog at debug level and drops
`TLS handshake error` lines unless `log.level=debug` (scanners make them constant).

### 9.2 Route map and owners

Go 1.22+ `http.ServeMux` patterns with methods.

| Pattern | Owner | Auth |
|---|---|---|
| `GET /` and every SPA route (fallback, §9.5) | 04 | none |
| `GET /assets/`, `/sw.js`, `/manifest.webmanifest`, icons, `/robots.txt` | 04 | none |
| `GET /healthz`, `GET /readyz` | 04 | none |
| `GET /ws` | 01 | session cookie (M1) or `hello.auth` bearer (M2), checked by the hub |
| `/api/v1/` subtree | 03's `httpapi.API` (03 §12.3) | per route: Public, User or Admin |
| `GET /api/v1/info` | 03 (with 01's protocol fields and this doc's VAPID key) | none |
| `POST /api/v1/conntest` (§7.7) | 04, registered through 03's `API.Handle` | User |
| `GET /api/v1/admin/dashboard` (§11.4), `GET\|POST /api/v1/admin/doctor`, `GET /api/v1/admin/bandwidth` (§13) | 04, registered through 03's `API.Handle` | Admin |
| `/api/v1/push/…` | 03 (03 §12.3 #24–28, #50–51); the sender behind them is §14 | User |
| `/install-desktop.sh`, `/install-desktop.ps1` | reserved, *later (M2)* | none |

```go
package httpapi

type RouterOptions struct {
	Site           config.Site
	TrustedProxies []netip.Prefix // only used when Site.TLSMode == off
	DisableHSTS    bool           // !tls.hsts; the zero value keeps HSTS on (§9.6)
	SPA            fs.FS          // web.Dist()
	SPAStatus      func(path string) int // 03 hook: 404 for "/setup" once an admin exists
	Gate           *Gate          // shutting-down switch
	API            http.Handler   // 03's *API, mounted at /api/v1/
	WS             http.Handler   // 01's *signal.Hub, mounted at /ws
	Observer       RouteObserver  // ops.Metrics implements it; the wiring passes it in (httpapi never imports ops)
	Logger         *slog.Logger
}

// RouteObserver receives one call per response from the transfer/metrics middleware (§9.3).
type RouteObserver interface {
	ObserveRoute(pattern string, status int, bytes int64)
}

type Router struct{ /* … */ }

func NewRouter(opts RouterOptions) *Router
func (r *Router) Handle(pattern string, h http.Handler)
func (r *Router) HandleFunc(pattern string, fn http.HandlerFunc)
func (r *Router) Handler() http.Handler // mux wrapped in the global middleware chain

// Helpers for every handler (01, 03, 04), defined once here and used by 03's API too.
func ClientIP(r *http.Request) netip.Addr // proxy-aware per §8.5
func IsSecure(r *http.Request) bool       // TLS, or trusted proxy said https, or Site.Dev
func RequestID(r *http.Request) string
func WriteJSON(w http.ResponseWriter, status int, v any)
// WriteError writes 03's envelope: *api.Error → api.StatusOf(code), Retry-After from RetryAfter; any other error →
// 500 internal with requestId (logged with the route pattern).
func WriteError(w http.ResponseWriter, r *http.Request, err error)
// DecodeJSON enforces maxBytes and the JSON Content-Type; errors are *api.Error bad_request, payload_too_large or
// unsupported_media_type.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) error

type Gate struct{ /* atomic state: serving | shutting_down */ }
func (g *Gate) SetShuttingDown()
func (g *Gate) ShuttingDown() bool // a nil *Gate is serving
```

### 9.3 Middleware chain (outermost first)

1. **Recover**: panic → 500 `internal`, logged with stack and route pattern, never with body or query.
2. **Request ID**: 16 hex chars, `X-Request-Id` response header.
3. **Read deadline**: `http.ResponseController(w).SetReadDeadline(now + 30 s)` on every request. The server has no
   `ReadTimeout` (it would kill WebSockets, §7.8), so without this a client could trickle a request body for as long
   as it likes. The router's `/ws` mount clears the deadline (`SetReadDeadline(time.Time{})`) before it hands the
   request to 01's hub, so 01 needs nothing. Every `ResponseWriter` wrapper in the chain implements
   `Unwrap() http.ResponseWriter` so the controller reaches the connection. Main server only; the admin socket streams
   large backup and restore bodies and sets no deadline.
4. **Transfer/metrics**: response bytes and status class per route pattern, reported to `RouterOptions.Observer`.
5. **Gate**: shutting down → 503 `server_shutdown` (`Retry-After: 5`). Exempt: `/healthz`, `/readyz`.
6. **Host check**: `Host` must equal `Site.Host` (or, when `Site.Dev`, any `localhost`/`127.0.0.1`/`[::1]` host so the
   Vite proxy works). Otherwise `421 Misdirected Request`, plain text. Exempt: `/healthz`, `/readyz`. Rationale: blocks
   DNS-rebinding against LAN installs and keeps cookies and Origin checks consistent.
7. **Real IP**: stores `ClientIP` in the context.
8. **Security headers** (§9.6).
9. Inside `/api/v1/`: 03's chain (body limit → no-store → CSRF → authenticate → rotate → access, 03 §12.1), then the
   handler.

No access log (plan). 5xx responses log one line with `route` (the pattern, not the URL), status and request ID.

### 9.4 Error envelope

Every JSON error from any `/api` route uses 03's envelope and code table (03 §12.2; `api.Error`, `api.StatusOf`):

```json
{"error": {"code": "push_endpoint_rejected", "params": {"reason": "private_address"}}}
{"error": {"code": "internal", "requestId": "9f2c41d07a1be355"}}
```

`code` is a stable snake_case string that 05 translates; the server never sends English UI text. The WebSocket
`error` message (01) uses the same codes where the meaning is the same (`rate_limited`, `internal`,
`server_shutdown`). Codes this doc adds to 03's table: `not_found`, `bad_sdp`, `transport_disabled`, `not_ready`,
`server_shutdown`, `doctor_busy`, and, on the admin socket only (§12), `backup_invalid`, `backup_newer`,
`restore_in_progress` and `insufficient_storage`; it also uses 03's `bad_request`, `payload_too_large`, `unsupported_media_type`,
`method_not_allowed`, `rate_limited`, `internal`, `push_endpoint_rejected` and `push_unavailable`. The Host check's 421
is plain text, not JSON.

### 9.5 SPA embedding, caching and fallback

`web/embed.go` (a Go file inside `web/`, because `go:embed` cannot reach parent directories):

```go
package web

import ("embed"; "io/fs")

//go:embed all:dist
var dist embed.FS

// Dist returns the built SPA rooted at dist/.
func Dist() fs.FS { sub, _ := fs.Sub(dist, "dist"); return sub }
```

- The repo keeps `web/dist/.gitkeep` (06 adds the `.gitignore` exception) so Go builds without a web build. If
  `index.html` is missing, the server serves a built-in page "Web UI not built: run `task build:web`" in any build,
  with status 503; a non-dev build also logs a startup warning "this binary has no web UI (built without web/dist);
  use a release artifact".
- 06 adds `ignore ./web/node_modules` and `ignore ./docs/node_modules` to `go.mod` (Go 1.25+ directive) so
  `go build ./...` never walks npm packages that ship `.go` files.
- At startup the handler walks `Dist()` once and precomputes, per file, an ETag (first 16 hex chars of SHA-256) and the
  content type.

Serving rules:

| Request | Response | `Cache-Control` |
|---|---|---|
| `GET/HEAD /assets/<hashed name>` that exists | the file | `public, max-age=31536000, immutable` |
| `/assets/…` that does not exist | `404` (never `index.html`: a stale hashed asset after an upgrade must fail as a 404, not as HTML with a JS MIME error) | `no-store` |
| any existing file outside `/assets/` and `/icons/` (`/sw.js`, `/manifest.webmanifest`, `/index.html`, `/boot-check.js`, `/version.json`, `/licenses.txt`, `/robots.txt`) | the file | `no-cache` (revalidate with ETag) |
| `/icons/*` | the file | `public, max-age=86400` |
| path whose last segment contains a `.` and does not exist | `404` | `no-store` |
| `/api/…` outside `/api/v1/`, `/ws`, `/healthz`, `/readyz` not matched | `404` JSON `not_found` | `no-store` |
| inside `/api/v1/` | 03's API answers unmatched paths with 404 JSON `not_found` and method mismatches with 405 JSON `method_not_allowed` plus `Allow` (03 §12.5), including routes added through `Handle` | `no-store` (03's chain) |
| any other `GET/HEAD` path | `index.html` with status `SPAStatus(path)` (200, or 404 for `/setup` after setup) | `no-cache` |
| other methods on SPA paths | `405` | – |

- `If-None-Match` → `304` (via `http.ServeContent` with the ETag header set; embedded files have no modtime).
- Precompressed siblings: if `<file>.br` or `<file>.gz` exists in `dist` and the client accepts it, serve it with
  `Content-Encoding` and `Vary: Accept-Encoding`. 05 decides whether the build emits them.
- MIME types are registered explicitly (distroless images have no `/etc/mime.types`): `.js`/`.mjs`
  `text/javascript`, `.css`, `.html`, `.json`, `.webmanifest` `application/manifest+json`, `.svg`, `.png`, `.ico`,
  `.webp`, `.woff2`, `.wasm`, `.txt`.
- `/robots.txt` (if the SPA doesn't ship one): `User-agent: *` / `Disallow: /`. Every response also sends
  `X-Robots-Tag: noindex`. Friend servers should not appear in search engines.
- 05 must not use dots in client-side route segments (rule above) and keeps `sw.js` and `manifest.webmanifest` at the
  root (scope `/`).
- `dist/version.json` (`{"version":"0.3.0"}`, written by the Vite build) is compared with `version.Version()` at
  startup; a mismatch logs a warning. 06 feeds the same version to both halves in every build (§15), so a mismatch in
  a release build is a bug.

### 9.6 Security headers

| Header | Value | Applies to |
|---|---|---|
| `Content-Security-Policy` | `default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; media-src 'self' blob:; connect-src 'self' wss://<host>; worker-src 'self'; manifest-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'` | HTML and `/sw.js` |
| `Content-Security-Policy` | `default-src 'none'; frame-ancestors 'none'` | everything else (not HTML, not `/sw.js`) |
| `X-Content-Type-Options` | `nosniff` | all |
| `Referrer-Policy` | `no-referrer` (plan) | all |
| `Cross-Origin-Opener-Policy` | `same-origin` | HTML |
| `Cross-Origin-Resource-Policy` | `same-origin` | all |
| `Permissions-Policy` | `display-capture=(self), fullscreen=(self), picture-in-picture=(self), autoplay=(self), camera=(), microphone=(), geolocation=(), usb=(), payment=(), browsing-topics=()` | HTML |
| `Strict-Transport-Security` | `max-age=31536000` (no `includeSubDomains`, no `preload`: other subdomains of the admin's domain are not ours) | TLS responses when serving a domain and `tls.hsts`; never for IP hosts (RFC 6797 ignores them) |
| `Cache-Control` | `no-store` | responses written by this doc's router outside `/api/v1/` (§9.5 404 rows, `/healthz`, `/readyz`); everything under `/api/v1/`, including routes registered through `API.Handle`, gets it from 03's chain (03 §13), with no exceptions |
| `X-Robots-Tag` | `noindex` | all |

Notes: a service worker's CSP comes from its script response. `/sw.js` needs `connect-src 'self'` for its cache
fill, network-first navigations and push re-subscription, so it gets the HTML policy; the worker only fetches
same-origin, so it needs nothing beyond that policy.
`connect-src` lists `wss://<host>` explicitly because older Safari versions don't match `wss:` with `'self'`
(`ws://<host>` when `Site.Origin` is `http://`, i.e. dev).
`style-src 'self'` works because Vite extracts CSS to files and React sets inline styles through the CSSOM, which CSP
does not block. 05 asks here if it needs a relaxation. The desktop webview's CSP (M2) is separate.

---

## 10. Logging (`internal/logx`)

```go
package logx

type Options struct {
	Level  *slog.LevelVar
	Format string // "auto" | "text" | "json"
	Out    io.Writer // os.Stderr
}

func New(opts Options) *slog.Logger

// Secret holds a value that must never be logged or serialized by accident.
type Secret string

func (s Secret) Reveal() string
func (Secret) String() string                    // "[redacted]"
func (Secret) GoString() string                  // "[redacted]"
func (Secret) Format(f fmt.State, verb rune)     // "[redacted]" for every verb except %p and %w (below)
func (Secret) LogValue() slog.Value              // "[redacted]"
func (Secret) MarshalJSON() ([]byte, error)      // "\"[redacted]\""; use Reveal() to send a real value
func (Secret) MarshalText() ([]byte, error)      // "[redacted]"

type SecretBytes []byte // same methods

func NewPionLoggerFactory(l *slog.Logger) logging.LoggerFactory // pion/logging bridge
func NewZapBridge(l *slog.Logger) *zap.Logger                  // for certmagic
func Err(err error) slog.Attr                                  // slog.Any("err", err)
```

Rules:
- **Format**: `auto` = text when stderr is a TTY, otherwise JSON (systemd journal and Docker both get JSON lines).
- **Levels** from `log.level`; runtime change through the admin socket (`--for` reverts automatically).
- **`Secret` gaps** that come from Go itself, because a `Secret` is a string (and `SecretBytes` a slice) underneath:
  fmt prints the raw operand for `%p` and `%w`, which it never passes to a `Formatter` for a string. go vet's printf
  check (part of `go test` and CI) rejects `%w` for a `Secret` (a `Secret` is not an error), but not `%p`: vet
  accepts any other verb for a type with a `Format` method, and `%p` prints `%!p(logx.Secret=<raw value>)`. Never
  format a `Secret` with `%p`; code review enforces it. fmt also can't call methods on unexported struct fields: keep
  `Secret` fields exported, or don't print the struct. A `Secret` is never a map key (encoding/json writes a
  string-kind key as it is). For `SecretBytes`, `%p` prints the slice's address.
- **Safety net `ReplaceAttr`**: attributes whose key is `password`, `token`, `secret`, `authorization`, `cookie`,
  `sdp`, `offer`, `answer`, `candidate`, `endpoint` or ends in `_token` are replaced with `[redacted]`, whatever
  their type.
- **No SDP**: our code never logs signaling payloads. The pion bridge maps pion levels to slog (trace/debug → debug),
  defaults to warn, rate-limits each scope to 20 lines per minute ("N messages suppressed"), and drops any message
  containing `a=` lines or `candidate:`.
- **No access logs.** Security events (rate-limit hits, login failures) may carry `remote_ip`; 03's audit log in the DB
  holds the durable record (30-day pruning).
- **Attribute names**: `component` (e.g. `tls`, `netx`, `push`, `sfu`, `signal`, `store`), `user_id`, `room_id`,
  `share_id`, `conn_id`, `remote_ip`, `route`, `err`. Usernames and share labels are not logged.
- Push endpoints are capability URLs: log only their host (`push_host`).

---

## 11. Ops (`internal/server/ops`)

### 11.1 Health endpoints

| Endpoint | 200 | 503 |
|---|---|---|
| `GET /healthz` | `{"status":"ok"}` | `{"status":"shutting_down"}` |
| `GET /readyz` | `{"status":"ready"}` | `{"status":"not_ready"}`, or `{"status":"shutting_down"}` once a shutdown began (§6.4 step 1) |

Both answer `GET` and `HEAD` (405 otherwise) with `Cache-Control: no-store`, and keep answering during a shutdown;
the router exempts them from its Host check (§9.3).

Public responses carry no detail. On `/readyz`, requests from loopback (and the admin socket's `/v1/ready`) add
`"checks": {"db":"ok","tls":"waiting: obtaining certificate for 203.0.113.7","media":"ok"}`. A request counts as
loopback when its client address is a loopback address and it carries no forwarding header (`Forwarded`,
`X-Forwarded-For`, `X-Forwarded-Host`, `X-Forwarded-Proto`, `X-Real-Ip`, `Via`): in off mode every proxied request
arrives from loopback, and the public must not see the checks through the proxy. The client address is the TCP peer
unless the wiring passes `httpapi.ClientIP` with `SetClientIP` (`ops` may not import `httpapi`).

```go
package ops

type Health struct{ /* … */ }

func NewHealth() *Health
func (h *Health) AddCheck(name string, fn func() (ok bool, detail string))
func (h *Health) SetMaintenance(reason string)
func (h *Health) SetShuttingDown()
func (h *Health) SetClientIP(fn func(*http.Request) netip.Addr) // nil = the TCP peer
func (h *Health) Live() (ok bool, state string)
func (h *Health) Ready() (ok bool, checks map[string]string)
func (h *Health) Handlers() (healthz, readyz http.Handler)
```

`isshoni healthcheck` asks the admin socket (`/v1/health`, or `/v1/ready` with `--ready`), not HTTPS: inside a
container `127.0.0.1` would fail certificate verification, and the distroless image has no curl. `--wait DUR` polls
every second.

### 11.2 Metrics (optional)

Off by default. When `metrics.enabled`, a separate server on `127.0.0.1:9469` serves `/metrics` (a private
`prometheus.Registry` with Go and process collectors), `/healthz`, `/readyz`, and `/debug/pprof/*` if
`metrics.pprof`. Components register unconditionally through `(*Metrics).Registerer()`; when metrics are off it is a
registry nobody scrapes.

| Metric | Type | Labels |
|---|---|---|
| `isshoni_build_info` | gauge (1) | `version`, `commit`, `go` |
| `isshoni_ready` | gauge | – |
| `isshoni_transfer_bytes_total` | counter | `direction` (egress/ingress), `path` (media_udp/media_tcp/web) |
| `isshoni_portmux_conns_total` | counter | `result` (tls/ice/plain_http/garbage/timeout/limited) |
| `isshoni_http_responses_total` | counter | `route`, `class` (2xx/3xx/4xx/5xx) |
| `isshoni_push_sent_total` | counter | `result` (ok/gone/rejected/error/dropped) |
| `isshoni_tls_cert_not_after_seconds` | gauge | – |

One owner per metric: 01 registers the signaling and presence metrics (`isshoni_ws_*`, `isshoni_rooms`,
`isshoni_participants`, `isshoni_shares{status}`, `isshoni_client_*`, 01 §18) through `Registerer()`; the wiring
registers an adapter over 02's `SFU.Metrics()` for the `isshoni_sfu_*` names (02 §13, including
`isshoni_sfu_selected_transport`). That adapter is a `prometheus.Collector` in `wire.go`: on every scrape it calls
`SFU.Metrics()` once and emits the 02 §13 names with exactly the label values that 02 lists (probe PCs are not
counted); it adds no labels of its own. Its table test covers every 02 §13 name, including
`isshoni_sfu_ingress_duplicates_total` (from `IngressDuplicates`). No per-user labels.

### 11.3 Transfer accounting and alerts

- Bytes are counted at the socket layer: UDP mux sockets (`media_udp`), ICE-TCP connections (`media_tcp`), HTTPS and
  HTTP connections (`web`). Rationale: VPS providers bill packets on the wire, including SRTP, RTCP and STUN overhead
  that RTP payload counters miss.
- `netx.TransferCounter` keeps atomic totals. `ops.Transfer` flushes deltas to the DB every 60 s and at shutdown, per
  **calendar month in UTC**, in 03's table `transfer_months` (03 §5, `AddTransfer`/`TransferMonth`).
- Alerts when month-to-date egress crosses 80 % and 100 % of the setting `transferAlertGb` (pinnable by
  `limits.transfer_alert_gb`; 1 GB = 10⁹ bytes). `ops` reads it only through `ops.Policy.TransferAlertGB()` on every
  tick (the wiring backs it with 03's `SettingsCache.Get()`, §6.6), never from `config.Config`. Each threshold fires
  once per month (remembered in 03's `meta` key `ops.transfer_alert_sent` = `"2026-09:80"`), shown as a dashboard
  alert and pushed to admins as the admin alert `transfer_threshold` (03 §7.11, payload §14.3).
- Projection: `mtd × days_in_month / days_elapsed`, shown after day 3.

```go
package netx

type Path string // "media_udp" | "media_tcp" | "web"

type TransferCounter struct{ /* atomics */ }

func (c *TransferCounter) Add(p Path, egress bool, n int)
func (c *TransferCounter) Totals() map[Path]struct{ Egress, Ingress uint64 }
```

### 11.4 Admin dashboard API

`GET /api/v1/admin/dashboard` (admin; the only dashboard endpoint; 05 polls it every 2 s while the page is visible,
02's load test every 5 s; the snapshot is cached for 1 s).

```json
{
  "generatedAt": "2026-09-29T20:14:05.000Z",
  "server": {
    "version": "0.3.0", "startedAt": "2026-09-29T18:00:00.000Z", "origin": "https://203.0.113.7",
    "process": {"cpuSeconds": 1834.2, "rssBytes": 187000000, "numCpu": 2, "goroutines": 412},
    "tls": {"mode": "ip", "names": ["203.0.113.7"], "ready": true, "notAfter": "2026-10-05T10:00:00.000Z", "nextRenewal": "2026-10-02T12:00:00.000Z"},
    "publicIpv4": "203.0.113.7", "publicIpv6": "", "nat": "none",
    "advertised": [{"proto": "udp", "addr": "203.0.113.7:7882", "via": "udp"}, {"proto": "tcp", "addr": "203.0.113.7:443", "via": "tcp443"}],
    "update": {"latest": "0.3.1", "url": "https://github.com/MoonWX/isshoni/releases/tag/v0.3.1", "security": true, "checkedAt": "2026-09-29T09:12:00.000Z"}
  },
  "transfer": {"month": "2026-09", "egressBytes": 412000000000, "ingressBytes": 98000000000,
               "alertGb": 1000, "projectedEgressBytes": 428000000000, "egressBps": 41500000, "ingressBps": 17000000},
  "media": {"ingressBps": 16800000, "egressBps": 41200000, "downTracks": 18, "peerConnections": {"udp": 7, "tcp443": 1, "tcp7882": 0}},
  "rooms": [{
    "id": "lounge", "name": "Lounge",
    "participants": [{
      "userId": "k3m9p2qxw7ht", "username": "alex",
      "connections": [{"id": "c_k3v9q2m7xw4pa8d1", "kind": "web", "role": "full", "version": "0.3.0", "os": "ios",
                       "transport": "udp", "rttMs": 38, "connectedAt": "2026-09-29T19:02:11.000Z"}],
      "watching": ["s_q7m2x9c4v8b1n5k3"]
    }],
    "shares": [{
      "shareId": "s_q7m2x9c4v8b1n5k3", "ownerUserId": "b8f2n4r6t0vz", "kind": "window", "startedAt": "2026-09-29T19:05:00.000Z",
      "codec": "h264/640c",
      "layers": [{"rid": "f", "width": 1920, "height": 1080, "fps": 59.8, "bitrate": 7900000, "lossPct": 0.1},
                 {"rid": "q", "width": 640, "height": 360, "fps": 15.0, "bitrate": 290000, "lossPct": 0.0}],
      "viewers": {"high": 3, "low": 1, "audio": 3},
      "ingressBps": 8400000, "egressBps": 24300000
    }]
  }],
  "clients": [{"kind": "web", "version": "0.3.0", "count": 5, "outdated": false}],
  "doctor": {"ranAt": "2026-09-29T18:00:07.000Z", "ok": 12, "warn": 1, "fail": 0},
  "alerts": [{"code": "release.security_update", "severity": "warn", "params": {"version": "0.3.1"}}],
  "accounts": {"…": "03 §12.4.8"}
}
```

- Types in `internal/protocol/api/ops.go`: `OpsDashboard`, `ServerInfo`, `ProcessInfo`, `TLSInfo`, `AdvertisedAddr`,
  `UpdateInfo`, `TransferInfo`, `MediaTotals`, `RoomLive`, `ParticipantLive`, `ConnectionLive`, `ShareLive`,
  `LayerLive`, `ViewerCounts`, `ClientVersionCount`, `DoctorSummary`, `Alert`; `accounts` is 03's
  `DashboardAccounts`. Share labels are not shown (user-typed labels are rare, and the kind is enough).
- Live data comes through interfaces declared in `ops`; `internal/server/wire.go` (§6.6) adapts 01's
  `Hub.Snapshot()` (participants, connections, last client stats, watchers) and 02's `SFU.Snapshot()` (per-share
  layers, bitrates, loss, egress, selected transports):
  - `connections[].transport` (`udp` | `tcp443` | `tcp7882`, the value set of §7.3) and `rttMs` come from 02's
    `RoomSnapshot.Conns` (`ConnSummary`), joined on the connection id; all other connection fields come from
    `Hub.Snapshot()`. The hub's client stats are not used for these two fields.
  - `participants[].watching` comes from `Hub.Snapshot()` (desired subscriptions, what `room.state` shows users);
    `shares[].viewers` comes from 02's `ShareSnapshot.Viewers` (forwarded quality).
  - `rooms[].shares[]` comes from `RoomSnapshot.Shares`, one to one: `codec` = `"h264/"` + `Info.Profile`, `kind` =
    `Info.Source`, `ownerUserId` = `Info.User`, `layers[]` from `Info.Layers` (`rid`, `width`, `height`, `fps`,
    `bitrate`, `lossPct`), `viewers` from `Viewers`, and `ingressBps`/`egressBps` from the `ShareSnapshot`.
  - `media` comes from `SnapshotTotals`, with `peerConnections` = `PCsByTransport` (probe PCs are not included). 02's
    load test reads `media.egressBps`.

```go
package ops

type LiveSource interface {
	Rooms(ctx context.Context) []api.RoomLive // participants, connections, shares, layers, viewers
	Media(ctx context.Context) api.MediaTotals
}
type AccountsSource interface {
	Accounts(ctx context.Context) (api.DashboardAccounts, error) // 03's API.DashboardAccounts; 1 s timeout
}
// Policy is the only way ops reads policy settings (§11.3, §11.5); the wiring backs it with 03's
// SettingsCache.Get() and ops calls it on every tick.
type Policy interface {
	TransferAlertGB() int // transferAlertGb
	ReleaseCheck() bool   // updateCheck
}
```

- A failing source yields an empty section and an alert `dashboard.source_failed`, never a failed response.
- Admins see who watches what; this matches the plan's "no hidden viewers" rule.
- Alert codes: `transfer.80`, `transfer.100`, `release.update`, `release.security_update`, `public_ip.changed`,
  `tls.renewal_failing`, `doctor.fail`, `dashboard.source_failed`.

### 11.5 Release check

- `GET https://api.github.com/repos/MoonWX/isshoni/releases?per_page=20` with `Accept: application/vnd.github+json`,
  `X-GitHub-Api-Version: 2022-11-28`, `User-Agent: isshoni/<version>` and `If-None-Match` (ETag). Nothing else is
  sent. 10 s timeout, 1 MiB response limit.
- First run 5–60 minutes after start (random), then every 24 h ± 1 h. Off when the setting `updateCheck` is false
  (policy key `updates.release_check`, so the admin UI can switch it off). `ops` reads it only through
  `ops.Policy.ReleaseCheck()` on every tick, never from `config.Config`.
- Ignore drafts; ignore prereleases unless the running version is a prerelease. Compare with
  `golang.org/x/mod/semver`.
- **Security flag**: any release newer than the running one whose body contains `<!-- isshoni:security -->` (06's
  release template adds it) sets `security: true`.
- The result is kept in memory and in 03's `meta` key `ops.release_check` (JSON), so the dashboard shows it after a
  restart. Failures log at debug and retry at the next tick.

---

## 12. Admin socket and admin CLI

### 12.1 Transport and access control

- **HTTP/1.1 over a unix socket** at `listen.admin_socket`. Rationale: streaming request/response bodies for
  backup/restore, status codes, `httptest`-friendly handlers, and `curl --unix-socket` for debugging. JSON is camelCase
  like every other API.
- Socket file `0600`, owned by the service user. systemd `RuntimeDirectory=isshoni` (mode 0750) creates
  `/run/isshoni`; the Docker image must contain `/run/isshoni` owned by 65532 (06).
- Peer credentials are checked on every connection (`SO_PEERCRED` on Linux, `LOCAL_PEERCRED` on macOS, via
  `golang.org/x/sys/unix`): only uid 0 and the server's own uid are served; others get the connection closed.
- **Root never touches the DB files**: `sudo isshoni admin …` only talks to the socket; the server process (user
  `isshoni`) does the reads and writes. In Docker, `docker compose exec isshoni isshoni admin …` runs as the container
  user.
- User, invite and setup calls are audited by 03's service with `store.CLIActor` (03 §10); `rotate-secrets` is audited
  at the next start (`secrets.rotated`); `backup`, `restore` and `log-level` log one INFO line with the peer uid and
  are not audited (a restore replaces the audit log anyway). No uid goes into the audit log.
- **Client dial errors** (`ops.DialAdmin` classifies them; every client command and doctor use it):
  - *Not running*: `ENOENT` (no socket or no `/run/isshoni`) or `ECONNREFUSED` (a stale socket file). Exit 4 with
    "isshoni is not running (no server on /run/isshoni/admin.sock)"; `doctor` runs offline (§13.1).
  - *Permission denied*: `EACCES`/`EPERM` on stat or connect (for example `/run/isshoni` is `0750` and the caller is
    neither root nor in the `isshoni` group, or the socket is `0600`), or the server closes the connection before the
    first response (the peer-cred refusal above; only for a caller the server can refuse: for root and the socket
    file's owner such a hang-up is *not running*, §12.2). Exit 4 with "Permission denied on
    /run/isshoni/admin.sock: run it with sudo (sudo isshoni <command>)", using the real socket path and the command
    as typed. `doctor` prints the same message and runs no checks: offline checks as an ordinary user would only
    show a wall of false failures (config and data dir unreadable, ports "in use" by the running server).
    `healthcheck` prints it too and still exits 1.
- Base URL for the client: `http://isshoni/v1/…` with a `DialContext` to the socket. Errors use a socket-only response
  wrapper around 03's envelope: `{"error": api.Error, "message": "…", "fix": "…"}`, where `message` and `fix` are
  English for the CLI to print. They are never added to `api.Error` itself. The socket passes 03's service errors
  through unchanged, and the status is always `api.StatusOf(code)`.

### 12.2 Endpoints

| Method and path | Request | Response | Errors |
|---|---|---|---|
| `GET /v1/health` | – | `{"status":"ok","version":"0.3.0"}` | 503 `{"status":"shutting_down"}` |
| `GET /v1/ready` | – | `{"status":"ready","checks":{…}}` | 503 not ready |
| `GET /v1/status` | – | version, uptime, site, TLS status, public addrs, listeners, rooms/participants/shares counts, schema version | – |
| `POST /v1/setup-url` | `{"waitReadyS": 0}` | `{"url":"https://…/setup#…","expiresAt":"…","tlsReady":true}` | 404 `setup_unavailable` |
| `GET /v1/users` | – | `{"users":[{"id","username","role","status","createdAt","lastSeenAt"}]}` | – |
| `POST /v1/users/{name}/reset-link` | – | `{"url":"https://…/reset#…","expiresAt":"…"}` | 404 `user_not_found` |
| `POST /v1/users/{name}/role` | `{"role":"admin"}` | 204 | 404, 409 `last_admin` |
| `POST /v1/users/{name}/disable` · `/enable` | – | 204 | 404, 409 `last_admin` |
| `POST /v1/invites` | `{"uses":10,"ttl":"168h"}`; both optional (omitted → 0 → the setting's default); `ttl` in whole hours `1h`–`720h` | `{"url":"https://…/invite#…","expiresAt":"…"}` | 403 `registration_closed`, 422 `validation_failed` |
| `GET /v1/backup?certs=1` | – | `application/gzip` stream, `Content-Disposition: attachment; filename="isshoni-backup-…tar.gz"` | 500 |
| `POST /v1/restore` | `application/gzip` archive or `application/vnd.sqlite3` DB body (≤ 2 GiB) | 202 `{"restarting":true,"preRestoreBackup":"backups/pre-restore-20260929T101500Z.tar.gz"}` | 400 `backup_invalid`, 409 `backup_newer`, 409 `restore_in_progress`, 507 `insufficient_storage` |
| `POST /v1/rotate-secrets` | `{"keys":["session","invite","resume"],"vapid":true}` (the CLI always sends all four) | 202 `{"rotated":[…],"restarting":true}` (the server restarts, §5.3) | 400 |
| `POST /v1/doctor` | `{"only": ["dns"]}` (optional) | `api.DoctorReport` (server-side checks) | – |
| `POST /v1/log-level` | `{"level":"debug","for":"30m"}` | 204 | 400 |

**The socket as README S43 built it** (`ops/adminsock.go`, `adminapi.go`, `adminclient.go`), where the table
leaves a case open:

- **During a shutdown the socket answers 503, like every API call** (§6.4 step 2). From the moment liveness goes
  false until the socket closes (step 6):
  - `GET /v1/health`, `/v1/ready` and `/v1/status` keep answering: health and ready with 503
    `{"status":"shutting_down"}`, status with 200. `isshoni healthcheck` exits 1, and `admin status` still prints.
  - Every other endpoint answers **503 `server_shutdown`** with the message "the server is shutting down" and the
    fix "try again once it is back". The CLI exits 1 on it (§3.2): it is no refusal, a retry after the restart
    works.
  - A `setup-url` that is waiting for readiness (`waitReadyS`) stops waiting and gets the same 503; no link is
    minted.
  - Connections that have not sent a request yet are closed. A client that is root or the socket's owner reads
    such a hang-up as "not running" (exit 4), so `healthcheck --wait` and `setup-url --wait` keep polling across a
    restart. Only a caller the server could have refused reads a hang-up before the first answer as the
    peer-credential refusal of §12.1 ("Permission denied… run it with sudo").
- **`/v1/ready`** always carries the `checks` object, also with 503 `{"status":"not_ready","checks":{…}}`: whoever
  reaches the socket is the operator (the public `/readyz` shows them to loopback only, §11.1).
- **Methods and paths.** A `GET` route also answers `HEAD`; any other method is 405 `method_not_allowed` with an
  `Allow` header. A path the server doesn't have is 404 `not_found` with the fix "the running server and this
  isshoni binary may be different versions: restart the server after an upgrade", which is what the operator has
  when a new CLI talks to an old server. A user name that is empty, `.` or `..` never reaches the server: the
  client answers `user_not_found` itself, because the router would redirect such a path.
- **Endpoints of later slices** are routed from the start: `GET /v1/backup`, `POST /v1/restore` and
  `POST /v1/rotate-secrets` (README S65) and `POST /v1/doctor` (S60) answer 500 `internal` with "… over the admin
  socket is not implemented in this build yet" until then.
- **`waitReadyS`** is 0 to 3600. A handler that panics is answered 500 `internal` with a `ref` that is also in the
  log, with the stack.
- **Platforms without the peer-credential check** (anything but Linux and macOS; Windows is compile-only in M1)
  serve every connection and say so in the log at startup.

User, invite and setup operations call 03's service layer (§19); this doc only defines the socket surface. Timestamps
in these bodies (`expiresAt`, `createdAt`, `lastSeenAt`) have 03 §3.3's fixed form, `2026-09-30T10:00:00.000Z`, the
same as REST and signaling: `ops` declares them as `api.WireTime` (or uses 03's DTOs). Statuses come from
`api.StatusOf`; the four backup/restore codes are (04) rows in 03's code table. The CLI branches on `code`,
never on the HTTP status, and maps codes to exit codes (e.g. `setup_unavailable` → 7 with the reset-password fix,
§12.5).

### 12.3 Backup format

```
isshoni-backup-0.3.0-20260929T101500Z.tar.gz
├─ manifest.json
├─ isshoni.db          consistent snapshot: 03's store.BackupTo (VACUUM INTO a temp file)
├─ secrets.json
└─ certmagic/…         whole certmagic storage except locks/ (omitted with --no-certs)
```

```json
{"format": 1, "isshoniVersion": "0.3.0", "schemaVersion": 7, "createdAt": "2026-09-29T10:15:00Z",
 "contents": ["isshoni.db", "secrets.json", "certmagic/"],
 "sha256": {"isshoni.db": "…", "secrets.json": "…"}}
```

- CLI: `--out PATH` (default `./isshoni-backup-<ver>-<ts>.tar.gz`, created `0600`), or `--out -` for stdout. The CLI
  prints "This file contains your server's keys. Keep it private."
- In a container there is no useful working directory, so `--out` is required; the error prints the Docker form:
  `(umask 077; docker compose exec -T isshoni isshoni admin backup --out - > isshoni-backup.tar.gz)`. The archive
  holds `secrets.json` and the TLS private keys, and a plain host shell redirect would create the file with the host's
  umask (usually `0644`, readable by every local user); the subshell's `umask 077` makes it `0600`. This is the only
  documented Docker form (06 §6.4 and the Docker site page use it too).
- `--out -` refuses to write to a terminal (exit 2: "stdout is a terminal; redirect it to a file") and prints the
  Docker form above as the fix. This also catches `docker compose exec` without `-T`, whose pseudo-TTY would corrupt
  the archive.
- `--offline` (server stopped, §12.6): the CLI calls 03's `store.BackupFile(ctx, src, dst)` (a read-only open, then
  `VACUUM INTO`; it never migrates or writes the source). A DB with a newer schema works too, because a read-only open
  works on it. Only when `BackupFile` fails (for example on a corrupt DB) does it copy the raw files (`isshoni.db`,
  `-wal`, `-shm`) and set `"raw": true` in the manifest.

### 12.4 Restore

Two inputs are accepted:
- a **backup archive** (`.tar.gz` from §12.3): database, `secrets.json` and certificates;
- a **database file** (the SQLite header `SQLite format 3\0`), typically 03's pre-migration backup
  `backups/pre-<schema>-<ts>.db`: only the database is replaced; `secrets.json` and `certmagic/` stay. This is the
  recovery named by 03's "schema newer" message (03 §4.5).

Server side (or the CLI with `--offline`), after receiving the body into `restore/incoming`:
1. **Validate**: gzip/tar well-formed; entries are regular files or directories only; names limited to the manifest's
   contents; no absolute paths or `..` (extraction goes through `os.OpenRoot(restore/new)`, Go 1.24+); ≤ 10,000
   entries; SHA-256 matches. Then, for every DB (from an archive, raw or not, or a bare `.db` file), call 03's
   `store.InspectFile` (read-only; it never migrates or writes the file) and do not trust the manifest's
   `schemaVersion` alone: `SchemaVersion > store.LatestSchemaVersion()` → `409 backup_newer` ("This backup is from
   isshoni 0.5.0; install 0.5.0 or newer first", using `LastAppVersion`); `HistoryOK` false or `Integrity != nil`
   (`PRAGMA integrity_check`) → `400 backup_invalid`. A bare DB file gets these DB checks only.
2. Write `backups/pre-restore-<ts>.tar.gz` of the current state (keep the last 5).
3. Write `restore/plan.json` `{"ts":…,"state":"swapping"}`.
4. Graceful shutdown with reason `restore` (clients see the restart notice, §6.4); close the store with a
   `wal_checkpoint(TRUNCATE)`.
5. Move current `isshoni.db`, **`isshoni.db-wal`, `isshoni.db-shm`**, `secrets.json` and `certmagic/` to
   `restore/old-<ts>/`, then move the staged files into `data_dir` (renames on one filesystem; fsync the directory).
   The old WAL must never sit next to the new database: SQLite would replay it into the wrong file.
6. Mark the plan `done`; re-exec (§6.5). Startup migrates an older restored DB as usual.
7. After the next successful start, delete `restore/old-<ts>/` and `restore/plan.json`.

Crash safety: at startup, a plan in state `swapping` is completed idempotently (for each item: if the target is
missing and the staged copy exists, move it). A leftover `restore/new` without a plan is deleted with a log line.

CLI: `isshoni admin restore PATH` (or `-` for stdin, the Docker form: `docker compose exec -T isshoni isshoni admin
restore --yes - < backup.tar.gz`) shows the manifest, asks for confirmation, streams the file, then waits (≤ 60 s)
for `/v1/health` to answer again and prints the result. It asks for confirmation only when stdin is a terminal;
otherwise `--yes` is required (§3.1). With `-` stdin carries the archive, so `--yes` is always required there;
`docker compose exec -T` also gives no TTY. Without `--yes` it refuses and prints the exact command with `--yes` for
its environment (06 §6.4 documents the Docker form above, and `task docker:smoke` runs it).

### 12.5 Setup URL, users, invites

- `setup-url` works only while no admin exists (03's `setup_unavailable` otherwise; the CLI exits 7 with the fix "use
  `isshoni admin users reset-password <name>`"). Each call mints a **new** token and cancels earlier unused setup
  tokens, so only the most recently printed link works (no stale links in scrollback). Token rules (hashed,
  single-use, 24 h, fragment) are 03's. With `waitReadyS` the server asks whether setup is still available before
  it waits, so after setup the answer is `setup_unavailable` at once; `tlsReady` in the answer is the readiness
  check `tls` (§6.2) at the moment the link is minted (S43).
- The other refusals of these commands exit 7 too (§3.2): `last_admin` from `set-role` and `disable`, and
  `registration_closed` and `limit_reached` from `invite create`.
- `reset-password` prints a one-time link (24 h, single use); issuing it immediately clears the password and signs the
  user out everywhere (sessions, devices, push subscriptions; 03 §7.10); the CLI prints this after the link (plan: "A
  password reset revokes everything"). It is the recovery path for a forgotten admin password.
- `invite create` sends only the flags the operator set; the server passes 0 for the others so 03's invite settings
  apply (03 §9).
- `set-role`/`disable` refuse to remove the last active admin (`409 last_admin`).
- Deleting users is only offered in the admin UI (fewer destructive CLI verbs).

### 12.6 Offline commands

`isshoni admin backup --offline` and `isshoni admin restore --offline PATH` act directly on `data_dir`, for when the
server can't start at all (for example invalid config):
- They refuse if the admin socket answers (`exit 7`: "the server is running; drop --offline"). That check comes first
  because its message is friendlier; then they take the data-directory lock (`data_dir/isshoni.lock`, §5.1)
  non-blocking and exit 7 with the same message if it is held, which also works across containers that share the
  volume.
- They refuse to run as root outside a container (`exit 7`: "run as the service user: `sudo -u isshoni isshoni admin
  restore --offline PATH`"), which keeps root from creating DB files.
- Docker: `docker compose stop`, then `docker compose run --rm isshoni admin restore --offline
  /var/lib/isshoni/backups/x.tar.gz` (or a `pre-*.db` file), then `docker compose up -d`.
- They are the recovery path when the server refuses to start (§6.3). On a newer schema, `serve` prints 03's message
  followed by the exact command for its environment (systemd or Docker, §6.1 step 4), and offline doctor's `schema`
  fix uses the same formatter. An offline restore doesn't re-exec anything; the operator starts the service
  afterwards.

---

## 13. `doctor` (`internal/server/ops/doctor`)

### 13.1 Modes

- **With a running server** (admin socket answers): the CLI calls `POST /v1/doctor`, so every check runs inside the
  server's process and namespace (it can read `/proc`, DMI and its own TLS state), then adds nothing else. Port checks
  are reported from the server's bound listeners.
- **Without a server** (the socket dial fails with `ENOENT` or `ECONNREFUSED`, §12.1; a permission error prints the
  `sudo` message and exits 4 instead): the CLI runs the same checks itself; checks that need live state (TLS status, transfer,
  release) are `skip`; port checks try to bind each port. `schema` (so a refusal to start is explained) reads the DB
  through 03's `store.InspectFile` and `store.LatestSchemaVersion`, passed in by `cmd/isshoni` (§2), and opens it with
  `file:<path>?mode=ro&immutable=1`, which never creates `-wal`/`-shm` files. So running `sudo isshoni doctor` while
  the server is stopped can't leave root-owned DB files (plan: root never creates the WAL files). With a leftover WAL
  the reported schema may be one step old; the server's own exit-78 message stays authoritative. Offline doctor never
  creates database files. install.sh runs `isshoni doctor --config /etc/isshoni/isshoni.toml --only
  dns,public_ip,clock` this way before the first start (06 §4.8).
- `--list-checks` prints the check ids of §13.2 in table order, one per line; `--list-checks --json` prints them as a
  JSON array (e.g. `["config","data_dir",…]`). 06's site has one troubleshooting anchor per id.
- The server also runs doctor 20 s after startup and every 24 h, keeping the last report (03's `meta` key
  `ops.doctor_last`) for the dashboard. `POST /api/v1/admin/doctor` runs it on demand (one run at a time globally,
  at most one per 10 s: `429 doctor_busy`).

```go
package doctor

type Env struct { // all fakeable
	Config    *config.Config
	Live      *api.ServerStatus      // nil when the server isn't reachable
	FS        fs.FS                  // for /proc, /sys
	Resolver  netx.Resolver
	STUN      netx.STUNClient
	HTTP      *http.Client           // clock check against the ACME directory's Date header
	Adjtimex  func() (unsynced bool, err error) // Linux; EPERM/ENOSYS = sync state unknown (§13.2 clock)
	DB        DBFiles                // offline `schema` check
	Now       func() time.Time
	GOOS      string
	UID       int
}

// DBFiles carries 03's file-level store functions, which never migrate or write the source file. doctor and ops
// may not import store (§2), so cmd/isshoni and the wiring fill it; ops's offline backup and restore validation
// (§12.3, §12.4) use the same struct.
type DBFiles struct {
	LatestSchemaVersion func() int                                              // store.LatestSchemaVersion
	Inspect             func(ctx context.Context, dbPath string) (DBInfo, error) // store.InspectFile (backupDir bound)
	Backup              func(ctx context.Context, srcPath, dstPath string) error // store.BackupFile
}
type DBInfo struct { // store.FileInfo, copied field by field
	SchemaVersion  int
	LastAppVersion string
	HistoryOK      bool
	Integrity      error  // PRAGMA integrity_check
	TooNewBackup   string // SchemaTooNewError.Backup, for the restore command of §6.1 step 4
}

func Run(ctx context.Context, env Env, in api.BandwidthInput, only []string) api.DoctorReport // only: nil = all
func RenderText(w io.Writer, r api.DoctorReport, color bool)
func Bandwidth(in api.BandwidthInput) api.BandwidthEstimate
func CheckIDs() []string // the §13.2 ids, for --list-checks
```

### 13.2 Checks

Status values: `ok`, `warn`, `fail`, `skip`, `info`. Messages are templates keyed by `code` with `params` (English
templates live in `messages_en.go` for the CLI; 05 renders the same codes from its catalog).

| ID | Checks | warn | fail |
|---|---|---|---|
| `config` | Loads and validates | config warnings | config errors |
| `data_dir` | Exists, owned by the service uid, 0700, free space; in a container: is a mount (§5.1) | free < 1 GB; mode wider than 0700 | not writable; ephemeral in a container |
| `secrets` | Present, 0600, parseable | mode wider (server fixes it) | unreadable or corrupt |
| `schema` | DB schema vs binary (offline: through `DBFiles`, §13.1) | – | newer than binary (the server refuses to start; the fix is 03's message plus the restore command for the environment, from the same formatter as §6.1 step 4) |
| `public_ip` | Detection result, method and NAT kind (§7.4) | `port_forward` ("forward TCP 80, 443, 7882 and UDP 7882 to {local}"); IPv6 missing (`info`); `ip` mode on provider `aws` or `gcp` (§13.3), otherwise ok (`info`, code `public_ip.ephemeral`): "The default public IPv4 changes on stop/start; in IP mode that moves the server's address and breaks every link, installed app and push subscription. Attach an Elastic IP (AWS) / reserve a static external IP (GCP), or use a domain", the same advice as 06's `/install/vps#aws` and `#gcp` | `symmetric`, `cgnat_likely`, or no public IPv4/IPv6 |
| `dns` | `auto`/`manual` with domain: A/AAAA via the system resolver vs public IPs; CAA allows `letsencrypt.org` | AAAA present but not this server | no A/AAAA, or A points elsewhere |
| `tls` | Mode, names, issuer, `not_after`, next renewal, last ACME error code (§8.7) | no cert yet and at least one ACME attempt has failed (from the first failure, not after 10 min: `fixCode` = `Status.LastErrorCode`, `fix` = its §8.7 fix text, so install.sh's doctor output at its 180 s timeout names the cause); renewal failing but cert valid; manual cert < 14 days | no cert after 10 min (with the same `fixCode`/`fix` when an attempt failed); expired; key mismatch |
| `clock` | Linux `adjtimex` `STA_UNSYNC` flag (no network); in `auto`/`ip`, skew vs the `Date` header of the ACME directory. If `adjtimex` fails with EPERM or ENOSYS (systemd `ProtectClock=`/`SystemCallFilter=` in 06's unit, or a container seccomp profile), the sync flag is reported as unknown (no warn) and only the skew check against the ACME directory's `Date` header decides; in `manual`/`off` mode without that check the status is `info` | not synchronized, or skew 30 s–5 min | skew > 5 min |
| `udp_buffers` | Linux: `net.core.rmem_max`/`wmem_max` ≥ `udp_buffer_bytes`; effective SO_RCVBUF on the media sockets | below target. Fix: if `/etc/sysctl.d/60-isshoni.conf` or `/usr/lib/sysctl.d/60-isshoni.conf` exists, `sudo sysctl --system`; otherwise `printf 'net.core.rmem_max=8388608\nnet.core.wmem_max=8388608\n' \| sudo tee /etc/sysctl.d/60-isshoni.conf && sudo sysctl --system` (06 §6.4); in Docker, "on the Docker host" | – |
| `ports` | **Local only**: isshoni listening on 443/tcp, 80/tcp, 7882/udp, 7882/tcp (or, offline, whether they can be bound). The text always says: "This only checks this machine. The browser connection test checks from outside." | a port disabled by config | a port in use by another program (offline) |
| `firewall_hint` | `info`: provider-specific text to open 80/tcp, 443/tcp, 7882/udp, 7882/tcp (§13.3); Docker bridge note that published ports bypass ufw | – | – |
| `container` | Docker/Podman detected, network mode (bridge vs host), port numbers must match (§7.1) | – | – |
| `lan_privacy` | macOS hosts only (`info`): browsers without Local Network permission can't reach LAN ICE candidates (S4 finding 6) | – | – |
| `transfer` | Month-to-date vs `limits.transfer_alert_gb` | ≥ 80 % | – (never blocks) |
| `release` | Running vs latest release | newer release, or security release | – |
| `nofile` | `info`: open-file limit | < 8192 | – |
| `bandwidth` | `info`: calculator output (§13.4) and NIC speed from `/sys/class/net/<if>/speed` | estimate > 80 % of NIC speed | – |

**When no public address was found** (decided after group 5; the server then runs and is not ready, §6.2). Doctor
is where the operator reads why, so the two checks say it in these words:

- `public_ip` is `fail` with the code `public_ip.none`: "No public IP address found: no public address on a network
  interface, and no STUN answer." In `ip` mode, and in `manual` mode without a domain, it goes on: "isshoni is
  running but not ready, and nobody can open it" (offline: "isshoni will start, but it won't be ready"). The fix
  names both ways out: "Set public_ip to this server's public address (Docker: ISSHONI_PUBLIC_IP in .env), or set
  domain, then restart isshoni. If the network just wasn't up yet when isshoni started, restarting is enough: sudo
  systemctl restart isshoni (Docker: docker compose restart)." With a domain (`auto`, or `manual` with one) the
  check is `fail` too, since media needs the address, but the site and the certificate don't wait for it.
- `tls` in `ip` mode is `fail` with "No certificate: there is no public IP address to get one for" and points at
  `public_ip`'s fix. It carries no `fixCode` of §8.7 (no ACME request was made, so there is no ACME error) and
  does not wait 10 minutes to turn from `warn` to `fail`: nothing is under way.

```
[fail] public_ip     no public IP address found (no public address on an interface, no STUN answer)
       isshoni is running but not ready, and nobody can open it
       fix: set public_ip to this server's public address, or set domain, then restart isshoni
            (if the network wasn't up yet when isshoni started: sudo systemctl restart isshoni)
       more: https://moonwx.github.io/isshoni/troubleshooting#doctor-public_ip
[fail] tls           no certificate: there is no public IP address to get one for (see public_ip)
```

Text output (abbreviated). After every `warn`/`fail` line the text output adds a line
`more: <version.DocsURL>troubleshooting#doctor-<id>` (indented like `fix:`); after `firewall_hint` with a provider
other than `unknown`, and after `public_ip`'s `public_ip.ephemeral` line, it adds
`<version.DocsURL>install/vps#<provider>`. JSON output stays as in §13.5; 05 builds the same links from `id`
and `env.provider`.

```
isshoni doctor · 0.3.0 · server running · 2026-09-29 20:15 UTC
[ ok ] config        /etc/isshoni/isshoni.toml
[ ok ] public_ip     203.0.113.7 (on interface eth0, confirmed by STUN)
[ ok ] tls           ip certificate for 203.0.113.7, valid until 2026-10-05 10:00 UTC, renews 2026-10-02
[warn] udp_buffers   net.core.rmem_max is 212992, isshoni wants 8388608
       fix: printf 'net.core.rmem_max=8388608\nnet.core.wmem_max=8388608\n' | sudo tee /etc/sysctl.d/60-isshoni.conf && sudo sysctl --system
       more: https://moonwx.github.io/isshoni/troubleshooting#doctor-udp_buffers
[info] ports         listening on 443/tcp, 80/tcp, 7882/udp, 7882/tcp (local check only)
[info] firewall_hint Hetzner: Cloud Console → Firewalls → allow TCP 80, 443, 7882 and UDP 7882
       https://moonwx.github.io/isshoni/install/vps#hetzner
[info] bandwidth     5 people, 2 sharing: ~41.5 Mbps egress (~44 on the wire), ~39 GB per 2-hour session
12 ok · 1 warn · 0 fail
```

### 13.3 Cloud provider detection

From DMI only (no metadata-service calls): `/sys/class/dmi/id/{sys_vendor,product_name,chassis_asset_tag,bios_vendor}`.

| Provider id | Match |
|---|---|
| `aws` | `sys_vendor` = "Amazon EC2" or `bios_vendor` contains "Amazon" |
| `gcp` | `sys_vendor` = "Google" |
| `azure` | `chassis_asset_tag` = "7783-7084-3265-9085-8269-3286-77" |
| `oracle` | `chassis_asset_tag` = "OracleCloud.com" |
| `hetzner` | `sys_vendor` = "Hetzner" |
| `digitalocean` | `sys_vendor` = "DigitalOcean" |
| `vultr` | `sys_vendor` = "Vultr" |
| `linode` | `sys_vendor` contains "Linode" or "Akamai" |
| `scaleway` | `sys_vendor` = "Scaleway" |
| `ovh` | `sys_vendor` contains "OVH" |
| `alibaba` | `sys_vendor` = "Alibaba Cloud" |
| `tencent` | `sys_vendor` = "Tencent Cloud" |
| `unknown` | otherwise |

05 owns the fix text per provider id (catalog keys `fix.firewall.<provider>`); doctor's CLI uses English templates of
the same content. The ids are `api.CloudProvider` constants in `internal/protocol/api/conntest.go` (§7.7), so `task
gen` puts them in `api.gen.ts`; `netx` and `doctor` use that type instead of their own.

### 13.4 Bandwidth calculator

Model (plan: egress ≈ Σ viewers × (focus + thumbnails × preview)):

```
for each person v (N people, S sharing):
    visible   = S − (1 if v shares else 0)
    focus     = full_bps  if visible ≥ 1 else 0
    thumbs    = min(T, visible − 1) × preview_bps   (0 if visible ≤ 1)
    audio     = audio_bps if visible ≥ 1 else 0
egress_media  = Σ_v (focus + thumbs + audio)
egress_wire   = egress_media × 1.05              (RTP/SRTP/UDP/IP headers at ~1200-byte packets)
ingress_media = S × (full_bps + preview_bps + audio_bps)
transfer_per_session_bytes = egress_wire × hours × 3600 / 8
```

Defaults: `full` by quality (1080p60 8 Mbps; 1440p60 16; 2160p60 32), `preview` 0.3 Mbps, `audio` 128 kbps (256 for
Movie), `T` = 8. Worked examples (media bitrate, as in the plan):

| Scenario | Per viewer | Egress | 2-hour session (wire) |
|---|---|---|---|
| Plan example: 10 people all sharing, 1 focus + 8 thumbnails | 10.53 Mbps | **105 Mbps** | ~99 GB |
| M1 exit: 5 people, 2 sharing, 1080p60 | 8.1–8.4 Mbps | **41.5 Mbps** | ~39 GB |

CLI flags: `--people N --sharing S --thumbnails T --quality 1080p60|1440p60|2160p60 --preset auto|movie --hours H`.
The admin UI calls `GET /api/v1/admin/bandwidth?people=5&sharing=2&thumbnails=8&quality=1080p60&preset=auto&hours=2`
(same function) so there is one implementation.

```json
{"input": {"people": 5, "sharing": 2, "thumbnails": 8, "quality": "1080p60", "preset": "auto", "hours": 2},
 "perViewerMbps": {"sharer": 8.13, "viewer": 8.43},
 "egressMediaMbps": 41.54, "egressWireMbps": 43.61, "ingressMediaMbps": 16.86,
 "transferPerSessionGb": 39.2}
```

### 13.5 JSON output (`doctor --json`, `POST /api/v1/admin/doctor`)

```json
{
  "schema": 1,
  "version": "0.3.0",
  "ranAt": "2026-09-29T20:15:00.000Z",
  "mode": "server",
  "summary": {"ok": 12, "warn": 1, "fail": 0, "skip": 0, "info": 4},
  "env": {"os": "linux", "arch": "amd64", "kernel": "6.8.0-45-generic", "container": "none", "systemd": true,
          "provider": "hetzner", "user": "isshoni"},
  "checks": [
    {"id": "udp_buffers", "status": "warn", "code": "udp_buffers.low",
     "params": {"rmemMax": 212992, "want": 8388608},
     "message": "net.core.rmem_max is 212992, isshoni wants 8388608",
     "fixCode": "udp_buffers.fix_sysctl",
     "fix": "printf 'net.core.rmem_max=8388608\\nnet.core.wmem_max=8388608\\n' | sudo tee /etc/sysctl.d/60-isshoni.conf && sudo sysctl --system",
     "localOnly": false, "durationMs": 1}
  ],
  "bandwidth": {"input": {"people": 5, "sharing": 2, "thumbnails": 8, "quality": "1080p60", "preset": "auto", "hours": 2},
                "egressMediaMbps": 41.54, "egressWireMbps": 43.61, "ingressMediaMbps": 16.86, "transferPerSessionGb": 39.2}
}
```

`mode`: `server` | `cli-with-server` | `cli-offline`. `env.container`: `none` | `docker` | `podman` | `other`. Types
(`internal/protocol/api/doctor.go`): `DoctorReport`, `DoctorCheck`, `DoctorStatus`, `DoctorEnv`, `BandwidthInput`,
`BandwidthEstimate`. The schema number increases only on breaking changes; fields are only added within a schema.
`message` and `fix` are English for the CLI; 05 renders `code`, `params` and `fixCode` from its catalog.

---

## 14. Web Push, server side (`internal/server/push`)

### 14.1 Keys and subject

- VAPID key pair from `secrets.json` (§5.2), public key published base64url (65-byte uncompressed P-256 point).
- `sub` claim = `push.subject` or derived (§4.3). Apple rejects tokens with an invalid `sub`, so the iPhone check in the
  M1 exit test covers the derived IP-origin form.
- The derivation is `push.DeriveSubject(pushSubject, acmeEmail, origin string) string` (S32), which the wiring calls
  with `push.subject`, `tls.acme_email` and `Site.Origin` to fill `Options.Subject`: `push.subject` when it is set,
  else `"mailto:" + tls.acme_email` when that is set, else the public origin. The claim must be a `mailto:` or an
  `https:` URL (RFC 8292 §2.1), so **an `http://` origin** (off mode without a proxy that says https, a development
  server) **is given as `https://`** with the same host: the claim only names a contact, and nothing connects to it.
  `New` refuses any other subject: it must start with `mailto:` or `https:` and have something after the prefix.
  `config` checks only the prefix (§4.5), so a bare `mailto:` or `https:` in `push.subject` passes config validation
  and is refused by `New` at startup (§6.1 step 8; with `push.enabled = false` `New` isn't called, §6.6). The
  derived forms always pass: `mailto:` with a non-empty address, or the origin that `config.NewSite` built, which
  has a host. Making the config rule as strict as `New`'s, and correcting the comment on `validSubject` (it says
  the two rules are the same), is a code follow-up for a slice that may touch `internal/server/config` and
  `internal/server/push`.
- The key pair comes from `Options.VAPID()` (`SecretStore.VAPID`), read once in `New`, which checks that it is a
  pair: a 65-byte uncompressed P-256 point that belongs to the 32-byte private scalar (`crypto/ecdh`), else `New`
  fails. `VAPIDPublicKey()` returns the point in base64url without padding.
- JWT expiry 12 h (webpush-go `VapidExpiration`); library `github.com/SherClockHolmes/webpush-go v1.4.0`
  (`SendNotificationWithContext`, RFC 8291 `aes128gcm`, RFC 8292 VAPID).

### 14.2 REST: owned by 03

03 owns the push REST endpoints and their tables (03 §12.3 #24–28 and #50–51, §12.4.6): subscribe (upsert),
list, unsubscribe by endpoint (`POST /api/v1/push/unsubscribe`), delete by id, test (this session, 1 per 10 s), and
`GET/PUT /api/v1/push/preferences`. The VAPID public key is published in `GET /api/v1/info` (`push.vapidPublicKey`);
there is no separate config endpoint. This service implements what those handlers call (03's `httpapi.Push`):

- `VAPIDPublicKey() string`;
- `ValidateEndpoint(ctx, endpoint) error`: the subscribe-time checks of §14.5 (https, port 443, no userinfo, DNS name
  that currently resolves only to allowed addresses), returning `*api.Error{push_endpoint_rejected, params.reason}`;
- `SendTest(ctx, subs)`: queues a `push.test` payload to the given subscriptions and doesn't wait for the delivery.
  With no subscriptions it does nothing and returns nil (03's handler answers 404 for that case itself). It returns
  `*api.Error{server_busy, retryAfter: 5}` when the queue took none of them, and
  `*api.Error{server_shutdown, retryAfter: 5}` once `Run`'s context has ended; both are 503 through
  `httpapi.WriteError` (03 §12.3 #28). The per-recipient bucket of §14.4 does not apply to it.

05 posts its subscription on every app start when permission is granted, and re-subscribes when the key in `/info`
differs from the subscription's `applicationServerKey`.

### 14.3 Triggers, recipients, preferences

| Type | Trigger (who calls) | Recipients | TTL | Urgency |
|---|---|---|---|---|
| `share.started` | A share goes `starting → live`, without `replaces` (01's hub calls `ShareStarted`, through the wiring) | Active users whose `shareStarted` preference is `all`, except the sharer and except `PresentUserIDs` (participants in that room, who already see it) | 600 s | `high` |
| `admin.alert` | 03's `AdminAlerter` (§7.11 there: `admin_granted`, `admin_revoked`, `admin_password_reset`, `registration_mode_changed`, `signup_pending`, `secrets_rotated`) and this doc's transfer alerts (`transfer_threshold`, §11.3) | Active admins whose `adminAlerts` preference is on | 86400 s | `normal` |
| `push.test` | `SendTest` (03's `POST /api/v1/push/test`) | The caller's current-session subscriptions | 60 s | `high` |

- Recipients come from 03's store: `ListPushSubscriptions(PushFilter{ExcludeUserIDs, AdminsOnly, Pref, SessionID})`
  (03 §6), which applies the preferences and returns active users only. Per-room muting is *later*.
- Every push must produce a visible notification: iOS revokes subscriptions after silent pushes. So there is no
  "silent" type, and 05's service worker shows a notification for every payload.
- The payload carries data, not English text; the service worker renders it from `en.json` (05). camelCase, like every
  other API:

```json
{"v": 1, "type": "share.started", "ts": 1790712000000, "tag": "share:lounge:k3m9p2qxw7ht",
 "url": "/r/lounge?focus=s_q7m2x9c4v8b1n5k3",
 "room": {"id": "lounge", "name": "Lounge"}, "user": {"id": "k3m9p2qxw7ht", "name": "Alex"},
 "shareId": "s_q7m2x9c4v8b1n5k3"}
{"v": 1, "type": "admin.alert", "ts": 1790712000000, "tag": "admin:signup_pending", "url": "/admin/approvals",
 "kind": "signup_pending", "actor": "system", "target": "sam_k"}
{"v": 1, "type": "push.test", "ts": 1790712000000, "tag": "test", "url": "/account/notifications"}
```

  `tag` lets a newer notification replace an older one for the same sharer and room. `url` follows 05's routes
  (`/r/{roomId}?focus={shareId}`). The name is the username (03 has no display names in M1). Types:
  `api.PushPayload` (`internal/protocol/api/push.go`).
- Payloads stay under 1 KB; webpush-go pads records to the default 4096-byte record size, which also hides lengths
  from the push service.

```go
package push

type ShareStarted struct { // the wiring converts 01's signal.PushShareStarted
	RoomID, RoomName string
	ShareID          string
	UserID, UserName string
	PresentUserIDs   []string
	At               time.Time
}

type AdminAlert struct { // the wiring converts 03's auth.AdminAlert
	Kind, Actor, Target string
	At                  time.Time
}

type Options struct { // only Enabled, VAPID, Subject and Store are required
	Enabled      bool                    // push.enabled; New refuses a disabled service (ErrDisabled)
	VAPID        func() config.VAPIDKeys // SecretStore.VAPID; read once, in New
	Subject      string                  // the VAPID sub claim: a mailto: or https: URL (DeriveSubject, §14.1)
	Store        Store
	Sender       Sender           // nil: the real one, webpush-go with the guarded client (§14.5)
	Workers      int              // 0: 4
	QueueLen     int              // 0: 1024
	Now          func() time.Time // nil: time.Now
	Logger       *slog.Logger     // nil: discard; the service adds component=push
	Resolver     netx.Resolver    // resolves an endpoint's host in ValidateEndpoint; nil: net.DefaultResolver
	OwnAddrs     []netip.Addr     // the server's public addresses (netx.PublicAddrs.V4 and V6): the guard refuses
	                              // them, so that an endpoint can't make the server call itself; zero values ignored
	Metrics      prometheus.Registerer // ops.Metrics.Registerer(): gets isshoni_push_sent_total{result} (§11.2),
	                                   // every result exported at 0 from the start; nil: no metric
	allowPrivate bool             // tests only: turns off the guard's address, port and IP-literal rules. Unexported,
	                              // so only this package's tests can set it
	rootCAs      *x509.CertPool   // tests only: replaces the system roots in the real sender
}

// ErrDisabled is New's error for Options.Enabled == false. With push.enabled = false the wiring doesn't call New
// at all (§6.6).
var ErrDisabled = errors.New("push: disabled by push.enabled = false")

type Service struct{ /* … */ }

func New(ctx context.Context, opts Options) (*Service, error) // checks vapid_key_fp and purges before returning (§5.2);
                                                          // starts no goroutine
func DeriveSubject(pushSubject, acmeEmail, origin string) string // §14.1
func (s *Service) Run(ctx context.Context) error          // workers, dispatcher and the daily prune; blocks until ctx
                                                          // ends, drains for ≤ 2 s, returns nil; callable once
func (s *Service) ShareStarted(ev ShareStarted)           // non-blocking; drops when the queue is full
func (s *Service) AdminAlert(a AdminAlert)                // non-blocking
func (s *Service) VAPIDPublicKey() string                 // 03's httpapi.Push
func (s *Service) ValidateEndpoint(ctx context.Context, endpoint string) error
func (s *Service) SendTest(ctx context.Context, subs []Subscription) error

type Sender interface {
	// The push service's answer comes back as a SendResult with a nil error, whatever the status. An error means no
	// answer arrived and is retried like a 5xx, unless it wraps ErrUndeliverable. Errors never contain the endpoint.
	Send(ctx context.Context, sub Subscription, payload []byte, o SendOptions) (SendResult, error)
}
type SendOptions struct {
	TTL     time.Duration
	Urgency string // "very-low" | "low" | "normal" | "high" (UrgencyVeryLow … UrgencyHigh)
	Topic   string
}
type SendResult struct {
	Status     int
	RetryAfter time.Duration // from a Retry-After header (seconds or an HTTP date); 0 when there was none
}

var ErrUndeliverable = errors.New("push: the message can't be delivered to this subscription") // never retried (§14.5)
var ErrBlockedAddress = fmt.Errorf("%w: the SSRF guard refused the address", ErrUndeliverable)
```

`ShareStarted`, `AdminAlert` and `SendTest` only queue. They may be called before `Run` and from any goroutine, and
are ignored (`SendTest`: `server_shutdown`) once `Run`'s context has ended. `push` imports
`github.com/prometheus/client_golang` for the one counter; like every component it registers through the
`Registerer` it is handed (§11.2), and a nil `Metrics` keeps tests free of it.

### 14.4 Dedup and rate limits

- `share.started`: at most once per (room, sharer) per **10 minutes**, whatever the number of connections the sharer
  has (web plus desktop). A share that resumes within 01's 30 s grace period is the same share and sends nothing.
- Per recipient: token bucket, burst 3, refill 1 per 10 minutes; excess is dropped and counted (`dropped`). A
  recipient is a user: one event costs one token, whatever the number of that user's subscriptions, and a user
  without a token gets it on none of them. The bucket covers `share.started` and `admin.alert`.
- **`push.test` is exempt from the bucket** (S32): 03 limits the endpoint itself (1 per 10 s per user, 03 §7.3
  `push-test`), and a test that silently sends nothing would look like broken notifications. A test doesn't take a
  token either, so it never uses up the budget of real notifications.
- `Topic` header per (room, sharer): `s` + first 22 chars of base64url(SHA-256(roomID + "/" + userID)) (≤ 32
  URL-safe chars), so a phone that was offline receives only the latest notification per sharer.
- `admin.alert`: 03 already coalesces `signup_pending` (one per 10 min); transfer alerts fire once per threshold per
  month. Re-sends after a server restart are also covered by the `server.shutdown` rule of 01 (a re-published share
  carries `replaces`, so it never triggers `share.started`).
- State is in memory (a restart can repeat one notification; acceptable).

### 14.5 Sending and the SSRF guard

The endpoint URL comes from a browser, so the server treats it as untrusted.

At subscribe time (`ValidateEndpoint`, called by 03's handler; failures are `push_endpoint_rejected` with
`params.reason`). 03's handler checks the body shape first (endpoint length ≤ 2048: `too_long`; key lengths:
`bad_keys`); `ValidateEndpoint` owns every URL and host rule:
- `https` scheme only (`not_https`); port 443 only, explicit or implicit (`bad_port`); no userinfo (`userinfo`); host
  must be a DNS name, any IP literal is rejected (`ip_literal`).
- The host must currently resolve (`unresolvable`), and only to allowed addresses (below; `private_address`).

How S32 reads these rules, where the two lines above leave room (it is stricter in each case, and only
`params.reason` depends on it):
- **Order.** The URL rules are checked in the order scheme, port, userinfo, IP literal, and an endpoint that breaks
  several is refused with the reason of the first: `https://user@push.example.com:8443/x` is `bad_port`.
- **Scheme**: also `not_https` for an opaque URL (`https:push.example.com`) and for anything that isn't a URL and
  doesn't start with `https://`. A string that starts with `https://` but that `net/url` can't parse follows the
  same order: a port that isn't a number is `bad_port`, then userinfo is `userinfo`, anything else is
  `unresolvable`.
- **Port**: an empty port (`https://push.example.com:/x`) is `bad_port` too.
- **IP literal, in every spelling**: a bracketed IPv6 address, dotted decimal, and the IPv4 spellings that C
  resolvers and browsers also accept (`2130706433`, `127.1`, `0x7f.0.0.1`, `0177.0.0.1`). The rule is WHATWG's: a
  host whose last label is a number (decimal, or hex after `0x`) is an IPv4 address. No DNS top-level domain is
  numeric, so nothing real is refused.
- **A host that is no DNS name** is `unresolvable` without a lookup: an empty host, a label that is empty or over 63
  bytes, a name over 253 bytes, or a character other than a letter, a digit, `-` or `_` (browsers send an
  internationalized name as punycode). One trailing dot is allowed.
- **`localhost` and every name under `.localhost`** are `private_address` without a lookup, whatever the resolver
  says (RFC 6761).
- **Resolution**: `Options.Resolver.LookupNetIP(ctx, "ip", host)` with a 5 s timeout. An error or an empty answer is
  `unresolvable`. Every address of the answer must be allowed, IPv4 and IPv6 alike: one private address among public
  ones is `private_address`.
- **Allowed addresses** are public unicast addresses that are not in `Options.OwnAddrs`. IPv4: everything outside
  the blocks listed below, to which S32 adds `192.88.99.0/24` (6to4 relay anycast) and `240.0.0.0/4` (reserved,
  with the broadcast address). IPv6 is checked the other way round: only `2000::/3` is global unicast, so loopback,
  link-local, ULA, site-local, multicast and the discard prefix are refused without a list; inside `2000::/3`,
  `2001::/23` (Teredo, benchmarking, ORCHID), `2001:db8::/32` and `3fff::/20` (documentation) and `2002::/16` (6to4)
  are refused. An IPv4-mapped address and an address in the NAT64 prefix `64:ff9b::/96` are judged by the IPv4
  address inside. An address with a zone is refused.

At send time (the real guard, because DNS can change):
- A dedicated `http.Client` whose `net.Dialer.Control` rejects the **actual IP being dialed** unless it is a public
  unicast address. Rejected: unspecified, loopback, private (RFC 1918), CGNAT `100.64.0.0/10`, link-local (incl.
  `169.254.169.254` metadata), ULA `fc00::/7`, multicast, `0.0.0.0/8`, `192.0.0.0/24`, `198.18.0.0/15`, documentation
  ranges, IPv4-mapped/NAT64 forms of those, and the server's own public addresses. Checking in `Control` also defeats
  DNS rebinding between check and connect.
- `Proxy: nil` (environment proxies ignored), `CheckRedirect` returns `http.ErrUseLastResponse` (no redirects),
  timeout 10 s, response body read ≤ 4 KiB.
- `webpush.Options{HTTPClient: guarded, Subscriber: subject, VAPIDPublicKey, VAPIDPrivateKey, TTL, Urgency, Topic}`.
- S32's guard also refuses, in `Control`, any port other than 443, and before each send it runs the stored endpoint
  through the URL rules above again, so a row written in any other way never reaches the client. Both refusals
  are `ErrBlockedAddress` or `ErrUndeliverable`: the subscription counts a failure and nothing is retried. The
  endpoint goes to webpush-go with its origin in the form of RFC 6454 (lower-case host, no `:443`), because the
  VAPID token's `aud` claim is built from the URL as written. Errors and logs carry only the endpoint's host
  (`push_host`), never the URL.

Results:

| Response | Action |
|---|---|
| 201/200/202 | `RecordPushResult(ok)`: `last_success_at = now`, `failures = 0` |
| 404, 410 | delete the subscription (gone) |
| 401, 403 | delete (the subscription belongs to another VAPID key, e.g. after rotation) |
| 400, 413 | log a warning (our bug), `failures++` |
| 429, 5xx, network error | retry at 5 s and 30 s (or `Retry-After` ≤ 60 s) while still within the TTL, then `failures++` |

Queue: 1024 jobs, 4 workers; a full queue drops the job (`dropped`).

How S32 reads the table (every status has a row, and the `isshoni_push_sent_total` result is named):
- **ok**: every 2xx, not only 200, 201 and 202.
- **gone** (404, 410) and **rejected** (401, 403): as above, the subscription is deleted.
- **error, not retried**: 400, 413 and every other answer that a retry can't change (any 3xx, since no redirect is
  followed, and any 4xx that has no row of its own), and a send that fails with `ErrUndeliverable` (the
  subscription's keys don't decode, its endpoint breaks the URL rules, or the guard refused the address). One
  warning, `failures++`.
- **transient**: 408, 429, every 5xx, and no answer at all (a network error, a timeout). At most two retries, 5 s
  after the first attempt and 30 s after the second; a `Retry-After` of at most 60 s replaces that wait. There is no
  retry when the push service asks for more than 60 s, or when the wait would end at or after the end of the
  message's TTL (which runs from the moment the dispatcher takes the event): the message is then an **error**
  (`failures++`) at once, as it is after the third attempt. A retry asks the push service to keep the message only
  for what is left of its TTL, and it holds no worker while it waits.
- **dropped** (never sent, no `failures++`): the queue is full, the recipient has no token (§14.4), the TTL ran out
  while the job was queued, or the server is shutting down (the retries that wait are given up, and what is
  still queued after the 2 s drain of §6.4). Drops are logged at most once a minute, with their count.

### 14.6 Pruning

- Immediately: 404/410/401/403 as above; at the first start after a VAPID rotation, all rows
  (`DeleteAllPushSubscriptions`, §5.2).
- Linked lifetime: each subscription references the web session that created it (`ON DELETE CASCADE`, 03 §5), so
  logout, password reset, device revoke and user deletion remove it; sessions themselves end after 30 days idle or
  180 days. Rationale: an attacker's browser must not keep receiving "Alex started streaming" after the account is
  recovered.
- Daily job: `PrunePushSubscriptions(20, now − 30 d)` deletes rows with `failures ≥ 20` and no success for 30 days.
- At most 10 subscriptions per user; 03's subscribe handler evicts the oldest.

### 14.7 Storage (03's tables)

The tables are 03's (`push_subscriptions`, `push_preferences`, `transfer_months` and the `meta` keys
`vapid_key_fp`, `ops.release_check`, `ops.doctor_last`, `ops.transfer_alert_sent`; 03 §5). The service reads them
through a small interface that the wiring implements over 03's `*store.Q` methods:

```go
package push

type Store interface { // wiring adapter over 03's store (03 §6)
	Recipients(ctx context.Context, f RecipientFilter) ([]Subscription, error) // ListPushSubscriptions(PushFilter)
	RecordResult(ctx context.Context, id string, ok bool, at time.Time) error   // RecordPushResult
	Delete(ctx context.Context, id string) error                                // DeletePushSubscriptionByID
	DeleteAll(ctx context.Context) (int, error)                                 // DeleteAllPushSubscriptions
	Prune(ctx context.Context, minFailures int, noSuccessSince time.Time) (int, error)
	Meta(ctx context.Context, key string) (string, error)                       // GetMeta
	SetMeta(ctx context.Context, key, value string) error                       // SetMeta
}

type RecipientFilter struct {
	ExcludeUserIDs []string
	AdminsOnly     bool
	Pref           string // PrefShareStarted ("share_started") | PrefAdminAlerts ("admin_alerts") | ""
	SessionID      string // push.test
} // fields map 1:1 to 03's PushFilter

type Subscription struct {
	ID, UserID, SessionID string
	Endpoint              logx.Secret
	P256dh, Auth          string
}
```

## 15. Versions (`internal/version`)

- One SemVer tag (`vX.Y.Z`) for server, web and (later) desktop; 0.x until v1.0 (plan).
- The binary learns it from ldflags set by goreleaser (06):

```
-ldflags "-s -w
  -X github.com/MoonWX/isshoni/internal/version.version={{.Version}}
  -X github.com/MoonWX/isshoni/internal/version.commit={{.FullCommit}}
  -X github.com/MoonWX/isshoni/internal/version.date={{.CommitDate}}"
```

- `date` is the commit date, not the build time: it keeps builds reproducible and matches goreleaser's
  `mod_timestamp` and the OCI `created` label (06). `BuildDate()` returns it (RFC 3339 in `BuildInfo.Date`).
- `task build` and the dev tasks set the same three symbols from 06's `{{.VERSION}}` (06 §7.2); there is no other
  ldflags variant.
- Without ldflags (`go run`, `go build`), `debug.ReadBuildInfo` supplies `Main.Version` and `vcs.revision` /
  `vcs.modified`; otherwise `0.0.0-dev+<short commit>[-dirty]`; without VCS info either, `0.0.0-dev`. A
  `go install …@v0.3.0` binary builds without the web UI (module downloads don't contain `web/dist`) and is not a
  supported server install.
- `IsDev()` is true when the SemVer prerelease starts with `dev`: `0.0.0-dev`, `0.0.0-dev+1a2b3c4`,
  `0.1.1-dev.1a2b3c4`, and goreleaser's snapshot default. 01 and 05 apply the same rule. CI builds get a non-dev
  version (06), so the stale-build path is testable.

```go
package version

func Version() string        // "0.3.0" (no leading v)
func Commit() string
func BuildDate() time.Time
func IsDev() bool
func UserAgent() string      // "isshoni/0.3.0 (+https://github.com/MoonWX/isshoni)"

// DocsURL is the project site; product links use it plus 06's anchor contract (06 §10.4).
const DocsURL = "https://moonwx.github.io/isshoni/"

type BuildInfo struct {
	Version, Commit, Date, Go, OS, Arch string
	Protocol int `json:"protocol,omitempty"` // filled by cmd/isshoni from protocol.Version (01)
	Schema   int `json:"schema,omitempty"`   // filled by cmd/isshoni from 03's latest migration number
}
func Info() BuildInfo // Protocol and Schema zero: this package imports nothing from internal/
```

`isshoni version`: `isshoni 0.3.0 (commit 1a2b3c4, built 2026-09-29, go1.27.x, linux/amd64, protocol 1, schema 7)`
(the plan's toolchain: `go 1.26.0` directive, toolchain pinned to the latest go1.27 patch, 06 §7.1); `--short`
prints `0.3.0` (install.sh compares it); `--json` prints `BuildInfo` (camelCase keys). The SPA gets the same string at build time:
06 builds the SPA with `ISSHONI_VERSION` equal to the ldflags `version` in every task and in goreleaser (06 §7.2), so
`dist/version.json` always equals `version.Version()`. The §9.5 startup comparison stays as a warning; a mismatch in a
release build is a bug. There is no separate `buildinfo` package.

---

## 16. Privacy facts for the project site (06)

What this part of the server sends to third parties, all documented on the privacy page. This section is the single
source for the outbound list; 06's `/privacy` page renders this table in full.

| Destination | When | What | Off switch |
|---|---|---|---|
| STUN (Cloudflare, Google) | Startup and every 10 min | One UDP Binding request (reveals the server IP) | `network.stun_servers = []` stops STUN entirely (set `public_ip` to a literal then); a literal `public_ip` alone leaves one NAT check at startup |
| Let's Encrypt | Issuance, renewal; one HTTPS GET of the ACME directory for the clock check (doctor, also install.sh's pre-check; `auto`/`ip` modes) | ACME protocol; optional email | `tls.mode=manual` or `off` |
| GitHub API | Daily | One GET with `User-Agent: isshoni/<ver>` | `updates.release_check=false` |
| Browser push services (Google, Apple, Mozilla, Microsoft) | On notifications | Encrypted payload (RFC 8291); the service sees timing and size (padded) only | `push.enabled=false`, or users don't subscribe |

OCSP stapling is disabled in certmagic (`OCSP.DisableStapling=true`, §8.2), so the server makes no OCSP requests;
manual certificates are loaded without certmagic and cause none either. Nothing else leaves the server. Logs contain
no tokens, SDP, push endpoints or usernames.

---

## 17. Test plan

### Unit (`go test -race ./...`; time-dependent code uses `testing/synctest`)

| Package | Asserted |
|---|---|
| `config` | Precedence flag > env > file > default for every kind; policy `IsSet`; unknown file key → error with line/column and suggestion; unknown env → warning; list/duration/bool parsing; derived TLS mode; `off`-mode default of `listen.http`; `off`-mode default of `trusted_proxies` (loopback `listen.http` → `127.0.0.0/8`, `::1/128`; non-loopback → `[]` plus the §4.5 warning; an explicit `[]` wins); `config init --tls.mode off` with a loopback `listen.http` writes `trusted_proxies` into the file; each §4.5 rule produces the right key, source and fix; `config example` output re-parses to the defaults; registry has no duplicate env/flag names; empty env counts as unset: `ISSHONI_PUBLIC_IP=` and `ISSHONI_DOMAIN=` with no file → effective `public_ip="auto"`, derived tls mode `ip`, no validation error, `IsSet("public_ip")` false; reserved names of §4.2 ignored without a warning; `config init` ignores env and exits 78 without writing on invalid values; `Site.Dev` only for off mode + loopback `listen.http` + empty or loopback `public_url` |
| `config` secrets | First start creates 0600 with all keys; restart keeps them; missing registered key is added; wide mode fixed; wrong owner → exit 78; corrupt → `secrets_corrupt`; atomic write leaves the old file on a simulated failure; `Rotate` changes only the chosen keys and writes the file atomically (no hooks: consumers compare key fingerprints at the next start, §5.3) |
| `netx` portmux | TLS ClientHello → `TLS()` and a full handshake (HTTP/1.1 and h2 ALPN); RFC 4571 STUN frame → `ICE()` with the byte replayed; `GET ` → 400 hint; `0xFF` → closed; silent client closed at the timeout (shortened in tests); 33rd pending conn per IP closed, also when the 33 come from different addresses in one IPv6 /64; with `MaxPending` reached a new connection is admitted and the oldest pending one is closed (`Limited`); `IPKey` table (IPv4, IPv4-mapped IPv6, two addresses in one /64, neighbouring /64s); per-IP open limit; `Close` unblocks both `Accept`s; `LocalAddr` is `*net.TCPAddr`; goleak clean |
| `netx` transport | Counting `PacketConn` keeps `AddrPortReaderWriter`; byte counts match; port 0 reuse; interface filter globs; buffer read-back and the one warn line when it is below target; 7882/tcp per-IP ICE limit shared with 443 (65th connection across both closed, also from different addresses in one /64); `Advertised` holds post-rewrite addresses with `Via` `udp`/`tcp443`/`tcp7882`, and equals the candidates of a real pion offer; with `include_loopback` and `ipv6`, `::1` is advertised for UDP only (no `tcp` entry, no `tcp6` type from it alone, `IPFilter` drops it; §7.6); a setter called after `Apply` wins |
| `netx` public IP | Fake STUN (pion/stun) + fake interfaces: each row of §7.4's table; timeouts; literal config skips STUN for the result |
| `netx` rewrite | Rules/filters for each row of §7.5 (direct, 1:1 NAT, Docker bridge, home LAN Append, IPv6 literal) |
| `tlsmgr` | Manual: load, reload on file change and SIGHUP, bad new pair keeps the old, key mismatch, expiry warning; `HTTPHandler`: ACME path passthrough, 503 while not ready, 308 to config host (ignores request Host), off-mode passthrough; ACME problem → hint code table |
| `httpapi` | Middleware order; read deadline: a request body trickled past it (shortened in tests) is cut, and `/ws` has no deadline after the upgrade; security headers per response type (`/sw.js` carries the HTML CSP, with `connect-src` including `'self'`; other non-HTML files carry `default-src 'none'`); HSTS only with domain+TLS; host check incl. dev localhost; trusted-proxy XFF (right-most untrusted) and ignoring XFF in non-off modes; error envelope; `DecodeJSON` limits; SPA table of §9.5 (asset 404, dotted path 404, deep link → index 200, `/setup` → 404 via hook, ETag/304, `.br` negotiation, MIME types without `/etc/mime.types`) |
| `logx` | `Secret` redacted in text and JSON handlers, every fmt verb except `%p` and `%w` (fmt prints both raw; vet rejects only `%w`), `json.Marshal`; `ReplaceAttr` key list; pion bridge drops SDP-like lines and rate-limits; format auto-detection |
| `ops` | Health state machine (starting → ready → shutting_down); conntest function (transport validation, `transport_disabled` with `params.transport` for each transport without a listener, per-user rate limit, `sfu.probe_limit` → 429, `publicIp` `""` without a public IPv4, `container` value) with a fake `Prober`; transfer flush, month rollover at UTC midnight, 80/100 % alerts once; release parser (drafts, prereleases, semver order, security marker, ETag 304); dashboard JSON golden file |
| `ops` admin | Peer-cred filter (injected creds); every endpoint via `httptest` over a real unix socket (short path under `/tmp`: macOS `sun_path` is 104 bytes); backup tar layout and manifest; offline backup falls back to raw files only when `BackupFile` fails; restore validation rejects traversal, symlinks, extra names, bad hashes, and (through `InspectFile`, whatever the manifest says) a newer schema, a history mismatch or a failed integrity check; offline commands exit 7 while the data-directory lock is held; swap crash recovery from each `plan.json` state; old WAL never next to the new DB |
| `doctor` | Each check with fake FS/resolver/STUN/clock/DMI; `tls` with no certificate is `warn` with `fixCode` = the §8.7 code right after one failed ACME attempt and `fail` after 10 min; `public_ip` in `ip` mode with DMI `aws`/`gcp` adds the `public_ip.ephemeral` info (not in `auto` mode or on other providers); `clock` with a fake `adjtimex` returning EPERM (sync unknown, no warn; skew check decides; `info` in `manual`/`off`); provider table; bandwidth numbers (plan example 10.53 Mbps/viewer and 105 Mbps; exit scenario 41.5 Mbps); JSON golden; text render; exit codes 0/5 and `--strict` |
| `push` | `ValidateEndpoint` table (http, :8443, userinfo, IP literal, localhost, 10/8, [::1], fd00::/8, 169.254.169.254, 100.64/10, name resolving private → `push_endpoint_rejected` with the right `reason`; also the order of the reasons for an endpoint that breaks several rules, the IPv4 spellings `2130706433`, `127.1` and `0x7f.0.0.1`, a host that is no DNS name, an empty port, the server's own address, and a fuzz target for the URL rules: §14.5); `Control` blocks a rebinding resolver and any port but 443; no redirects followed; the result table per status (every 2xx ok; 3xx and other 4xx an error without a retry; 408 retried; a `Retry-After` over 60 s and a wait past the TTL end the retries); `push.test` is not rate-limited per recipient; `New` with `Enabled: false` → `ErrDisabled`; `DeriveSubject` (an `http://` origin becomes `https://`); recipient filter passed to the store (sharer and `PresentUserIDs` excluded, `Pref`); `admin.alert` payload per 03 alert kind; VAPID fingerprint change deletes all subscriptions before `push.New` returns; dedup window; per-recipient bucket; 404/410/401/403 prune; 429 retry with `Retry-After`; payload < 1 KB; round trip through a fake push service that **decrypts** `aes128gcm` with the test subscription's private key and checks the VAPID JWT (`aud`, `exp`, `sub`) |
| `version` | Build with `-ldflags -X …` in a test and check `isshoni version --json`; BuildInfo fallback |
| `cmd/isshoni` | `testscript` (rogpeppe/go-internal, test-only): every subcommand's usage, exit codes (incl. healthcheck 0/1 only), a permission case run as a non-root uid that is not the server's (a socket directory it can't enter, and a server whose peer-cred filter rejects it): `setup-url` and `admin status` exit 4 with the `sudo` message, `doctor` prints it and runs no offline checks (skipped when the tests run as root), `--json` shapes, confirmation prompts and `--yes`, the no-TTY refusal without `--yes` (exit 2, printing the `--yes` command in the systemd form and, with container detection faked, the `docker compose exec -T` form), container `--out` requirement, `admin backup --out -` refused with exit 2 and the umask Docker form when stdout is a terminal (testscript `ttyout`) |

### Integration (in-process, `servertest`)

- Boot (`tls.mode=off`, ephemeral ports, loopback candidates) → ready in < 1 s; `/healthz`, `/readyz`, SPA and API
  headers.
- **TLS path through the 443 mux**: `servertest.Options{TLS: true}` (a private test CA, manual mode) → WSS signaling
  over HTTP/1.1 and SPA over h2 on the same port.
- **ICE-TCP via 443** (in `internal/server/itest`, README S59, once the hub and the SFU are wired into `serve`):
  `servertest.Options{TLS: true}` with `listen.ice_udp = ""` and `listen.ice_tcp = ""`; a Go publisher restricted to
  `tcp4` shares, an `sfutest.Viewer` decodes its media, and WSS signaling runs on the same port; both Go clients'
  selected candidate pairs are TCP with the TLS listener's port as the remote port. The same via 7882/tcp in off mode
  (no 443 multiplexer there, §8.5). README S85 adds the metric assertion to this test:
  `isshoni_sfu_selected_transport{transport="tcp443"}` counts both PCs. S7's `servertest` check (README S44) runs
  before that wiring, so it uses a stub `/ws` handler and a raw RFC 4571 STUN frame on the 443 port, not media.
- **Connection test over TLS**: `servertest.Options{TLS: true}`; `POST /api/v1/conntest` with `tcp443` returns an
  answer that connects through the 443 multiplexer (README S80). The browser e2e harness runs in off mode only, so
  this Go test is the only `tcp443` probe check (05 §19.3).
- **Graceful shutdown**: connected clients receive `server.shutdown`, `error{server_shutdown}` and close code 1012
  within 2 s; REST gets 503 `server_shutdown`; `Run` returns within `shutdown_timeout`; goleak clean.
- **Refusing to start**: seed a DB with a newer schema → `serve` exits 78 and prints 03's message with the backup
  name, followed by the restore command for the environment; `admin restore --offline <pre-*.db>` → the next `serve`
  starts. A `store.Open` error without `store.ErrNeedsOperator` → exit 1. A container without a data mount (fake
  mountinfo) → exit 78. A second `serve` on the same data dir → exit 1 (lock held).
- **Offline doctor leaves no DB files**: `doctor` against a stopped data dir leaves the directory listing unchanged
  (no `-wal`/`-shm` created), tested as a different uid than the data owner where the platform allows.
- **Backup/restore round trip**: create users and a push subscription → backup → mutate → restore → state equals the
  backup; sessions from the backup still work.
- **rotate-secrets**: the server restarts (tests call `Run` again instead of `exec`); web sessions, device codes and
  invites invalid; push rows gone (session cascade and VAPID purge) before the first request is served.
- **Wiring** (§6.6): every adapter has a table test; a unit test asserts that the registry's policy defaults equal
  03's `SettingsCache.Defaults()`; a unit test asserts that `netx.IPKey` and 03's limiter key (exported by `auth` for
  this test) give the same key for the `IPKey` table of the netx tests; in-process (off mode),
  `POST /api/v1/auth/logout` closes that session's WebSocket (`session_revoked` on the wire) within 100 ms, and an
  admin settings change of `maxShareBitrateKbps` reaches `SFU.SetLimits`. Logout-everywhere and the rest of 03 §7.7's
  revocation matrix are 03's tests, with a fake `ConnCloser`.
- **ACME** (CI job with Pebble + challtestsrv as service containers; Pebble listens with `httpPort`/`tlsPort` matching
  our test listeners): `auto` for a test domain and `ip` for `127.0.0.1`-mapped IP identifiers with the `shortlived`
  profile; a no-SNI handshake gets the IP certificate; renewal fires under a shortened lifetime.
- **Log canary**: run the full flow at `debug`, capture logs, assert that no token, SDP line (`a=`), `candidate:` or
  push endpoint appears.

### End-to-end (Playwright harness from 05; assertions owned here)

- TCP-only viewer (`tcp-only.spec`): its own server (05's `startServer` fixture) with `listen.ice_udp = ""`; Chrome
  viewer plays (`framesDecoded > 0`) and the selected candidate pair is TCP.
- Connection test: wizard step 2 reports UDP ✓ and TCP 7882 ✓ on localhost with the TCP 443 row hidden (the harness
  runs in off mode, so `tcp443` answers `transport_disabled`; the TLS case is the Go test above); with
  `listen.ice_udp = ""` the UDP row shows ✗ with the "UDP turned off" text.

### Manual (M1 exit)

- Fresh VPS (2 vCPU, 2–4 GB): install.sh in IP mode, a real staging then production IP certificate, `doctor` all ok.
- 5 friends, 2 hours: dashboard numbers plausible, transfer counter vs provider's graph within 5 %, no errors, RSS flat.
- iPhone (Home Screen app) and Android Chrome receive "X started streaming"; tapping opens the room.
- `docker compose` bridge mode: setup-url, backup to host via `--out -`, restore via stdin.

---

## 18. Interfaces other docs rely on

Packages and names (exact):

- `internal/version`: `Version()`, `Commit()`, `BuildDate()`, `IsDev()`, `UserAgent()`, `DocsURL`, `Info() BuildInfo`;
  ldflags symbols `github.com/MoonWX/isshoni/internal/version.version|commit|date`.
- `internal/logx`: `New(Options) *slog.Logger`, `Secret`, `SecretBytes`, `Err`, `NewPionLoggerFactory`,
  `NewZapBridge`; attribute names of §10.
- `internal/server/config`: `Config` (+ sections), `Load`, `(*Config).IsSet/Source/EffectiveTLSMode/Paths/Warnings`,
  `TLSMode` consts, `Site` + `NewSite` + `(Site).URL`, `SecretStore` (`Key`, `KeyID`, `VAPID`, `Rotate`), `KeyName`
  consts `KeySession|KeyInvite|KeyResume`, `RotateResult`, `VAPIDKeys`, `OpenSecrets`, `InspectSecrets`, `KeyNames`,
  `ErrUnknownKey`; the data directory and refusals of §5.1 and §6.3 (`SetPrivateUmask`, `PrepareDataDir`,
  `LockDataDir`/`DataDirLock`/`ErrDataDirLocked`, `ApplyMemoryLimit`, `Host` with `Container`, `AllowEphemeralData`,
  `DataDirMounted` and `CgroupMemoryLimit`, `ContainerKind`, `ErrNeedsOperator`, `OperatorError`, `Reason` and
  `ReasonOf`); the key registry in `keys.go` (other docs add keys there); env/flag naming rule; policy keys
  `registration.mode`, `clients.min_version`, `limits.max_participants_per_room`, `limits.max_shares_per_room`,
  `limits.max_bitrate_kbps`, `limits.transfer_alert_gb`, `updates.release_check` with their 03 settings fields
  (§4.3); guard `limits.ws_handshakes_per_ip_per_minute`; `sfu.pause_unwatched_layers`.
- `internal/server/netx`: `Transport` (`UDPMux`, `TCPMux`, `TCPMux443`, `TCPMux7882`, `NetworkTypes`,
  `IncludeLoopback`, `Advertised`, `RcvBuf`, `SndBuf`, `Apply(*webrtc.SettingEngine) error`, `Close`), `NewTransport`,
  `TransportOptions` (incl. `PacketConns` for tests), `AdvertisedAddr` (`Via`: `udp` | `tcp443` | `tcp7882`),
  `PublicAddrs`, `DetectPublicAddrs`, `STUNClient`, `Resolver`, `PortMux` (`ICE().Close` idempotent), `IPKey`,
  `TransferCounter`, `Path` consts. 02's `sfu` and `sfutest` may import `netx`; `netx` never imports `sfu` (§2).
- `internal/server/httpapi` (router part): `Router` (`Handle`, `HandleFunc`, `Handler`), `RouterOptions` (`SPAStatus`,
  `API`, `WS`, `Observer`), `RouteObserver`, `ClientIP`, `IsSecure`, `RequestID`, `WriteJSON`,
  `WriteError(w, r, err)`, `DecodeJSON(w, r, dst, maxBytes)`, `Gate`; the global middleware order of §9.3.
- `internal/server/ops`: `Health` (`AddCheck`, …), `Metrics` (`Registerer()`, `ObserveRoute`), `LiveSource`,
  `AccountsSource`, `Policy`, `Prober`, the typed dashboard, doctor, bandwidth and conntest functions (§2), admin
  socket API (§12.2) and client (`ops.DialAdmin(path) *AdminClient`).
- `internal/server/ops/doctor`: `Run`, `Bandwidth`, `RenderText`, `CheckIDs`, `Env`, `DBFiles`, `DBInfo`.
- `internal/server/push`: `New(ctx, opts)`, `Options` (with `Resolver`, `OwnAddrs`, `Metrics`), `ErrDisabled`,
  `DeriveSubject`, `Service` (`ShareStarted`, `AdminAlert`, `VAPIDPublicKey`,
  `ValidateEndpoint`, `SendTest`, `Run`), `ShareStarted`, `AdminAlert`, `Store`, `RecipientFilter`
  (`PrefShareStarted`, `PrefAdminAlerts`), `Sender`, `SendOptions`, `SendResult`, `ErrUndeliverable`,
  `ErrBlockedAddress`, `Subscription`.
- `internal/server`: `Server`, `New`, `Start`, `Run`, `Shutdown`, `Site`, `Addrs` (`HTTP`, and `HTTPS` from S44
  on: the 443 multiplexer, nil in off mode), `ShutdownReason`, `ErrRestartRequested`, `ErrShutdownForced`,
  `NeedsOperator`, `Deps`, and the wiring table of §6.6.
- `internal/server/tlsmgr` (S44; §8.2): `Options` (with `PublicIPv6`, `Site`, `HTTPAddr`, `HTTPSAddr`, `Resolver`),
  `New`, `Manager` (`Start`, `Shutdown`, `TLSConfig`, `HTTPHandler`, `Reload`, `Status`, `Ready`), `Status`, and the
  ten `Code…` constants of `Status.LastErrorCode`: the seven ACME hints of §8.7 (`tls.acme_unreachable`,
  `tls.dns_missing`, `tls.dns_wrong`, `tls.rate_limited`, `tls.ip_rejected`, `tls.caa_forbids`, `tls.acme_failed`)
  and manual mode's `tls.cert_unreadable`, `tls.cert_invalid`, `tls.cert_expired`. The dashboard carries the code
  as `api.TLSInfo.lastErrorCode`, so 05's dashboard page (README S91) needs a text for all ten, and doctor's `tls`
  check (S60) uses them as `fixCode`.
- `internal/server/servertest`: `Start(t testing.TB, opts Options) *Server` with fields `URL` (`http://` plus the
  site's host; `Client` dials the listener whatever host the URL names), `WSURL`, `Client` (`*http.Client`, keeps
  cookies, trusts the test CA when `Options.TLS`), `AdminSocket`, `UDPPort`, `TCPPort` (0 until README S59), `DataDir`,
  `Cfg`, `Srv`; methods `Restart(t)`, `Stop(t)`, `Wait(t) error` (the result of `Run` after a shutdown the test
  began itself) and `Logs()`; `Try(t, opts) (*Server, error)` for a server that is expected to refuse;
  `Options{TLS bool; Roots *x509.CertPool; Flags []string; Config func(*config.Config); Deps server.Deps; DataDir
  string}`. `Flags` are `serve` flags, so a key set there counts as set by the operator (`config.IsSet`, which pins
  a policy key, §4.6); a change made in `Config` does not. Since the wiring (README S54) the admin socket is live
  on `AdminSocket` (`ops.DialAdmin`), and the harness fills two of `Deps` when the test leaves them zero:
  `LogLevel` is the level variable of its own logger, and `Argon` is a hash of 64 KiB and one pass, because the
  server computes one hash at startup and the real cost is a good part of a second under the race detector.
  - **`Options.TLS`** (S44) runs the server in `tls.mode = "manual"` with a certificate from a private CA made for
    the test (§8.6): HTTPS, WSS and ICE-TCP on one port behind the 443 multiplexer, and the plain port that
    redirects to it. The certificate is for `TLSDomain`, `localhost`, `127.0.0.1` and `::1`; its files are
    `Cfg.TLS.CertFile` and `Cfg.TLS.KeyFile`, which a test may replace (the reload tests do).
  - **`TLSDomain`** is the constant `"isshoni.test"`, the `domain` of such a server, so its site is
    `https://isshoni.test:<port>` and `URL` and `WSURL` (`wss://…/ws`) use it. `.test` names never resolve
    (RFC 6761); `Client` reaches the server all the same, because it dials the bound listener whatever the URL
    says. In a TLS mode `Client` speaks HTTP/2 where a browser would, and a WebSocket upgrade gets an HTTP/1.1
    connection of its own.
  - **`Options.Roots`** are further roots that `Client` trusts: the issuing root of an ACME test server, for a
    test that puts the server into `tls.mode = "auto"` through `Flags` (the Pebble tests). **`Server.Roots`** is
    what `Client` trusts in a TLS mode, the test CA plus `Options.Roots`, and nil in off mode; a test that dials the
    HTTPS port by itself (`Srv.Addrs().HTTPS`) verifies the server with it.
  - No harness config has STUN servers, so no test asks the public ones for the machine's address.
- `internal/protocol/api` (TS via tygo, next to 03's DTOs): `OpsDashboard` and its parts (`ServerInfo`,
  `ProcessInfo`, `TLSInfo`, `AdvertisedAddr`, `UpdateInfo`, `TransferInfo`, `MediaTotals`, `RoomLive`,
  `ParticipantLive`, `ConnectionLive`, `ShareLive`, `LayerLive`, `ViewerCounts`, `ClientVersionCount`,
  `DoctorSummary`, `Alert`), `ServerStatus`, `DoctorReport`, `DoctorCheck`, `DoctorStatus`, `DoctorEnv`,
  `BandwidthInput`, `BandwidthEstimate`, `PushPayload`, `ConnTestRequest`, `ConnTestResponse`, `ConnTestServerInfo`,
  `CloudProvider` and `NATKind` (typed string constants); the error codes added to 03's table (§9.4).
- `web` package: `web.Dist() fs.FS`.
- HTTP: `/healthz`, `/readyz`, `/api/v1/conntest`, `/api/v1/admin/dashboard`, `/api/v1/admin/doctor`,
  `/api/v1/admin/bandwidth`; push payload types `share.started`, `admin.alert`, `push.test`.
- CLI and ops contract for 06: subcommands and flags (§3.1: `config init`, `version --short`, `doctor --only
  --list-checks`, `healthcheck --ready --wait`, `setup-url --qr --wait --json`, `admin backup --out -`, `admin restore
  PATH|-` incl. `.db` files and `--offline`), exit codes (§3.2: 78 for "restart can't help"; healthcheck 0/1; setup-url
  4 unreachable, 7 `setup_unavailable`; doctor 0/5), the output of the link commands (§3.3: off a terminal the bare
  link is the first line of stdout and everything else is on stderr), env names (§4.2, including the
  empty-means-unset rule and the reserved names; §4.3 env-only switches), the data-directory lock `isshoni.lock`
  (§5.1), `SIGHUP` reload, re-exec after restore and rotation, no `sd_notify` (unit `Type=exec`), Docker needs
  `/run/isshoni` writable by uid 65532 (tmpfs with a read-only root) and a data volume at `/var/lib/isshoni`, the
  release-note marker `<!-- isshoni:security -->`.

---

## 19. Depends on

**01-protocol**
- `protocol.Version`/`MinVersion` (for `version` and `/info`); `tygo.yaml` includes `internal/protocol/api`.
- Signal hub: `Shutdown(ctx, reason)` sending `server.shutdown` and `error{server_shutdown, retryable, scope
  connection}`, then close 1012; `Ready()`; `Snapshot()` (connections with kind, role, version, os, last client
  stats, for the dashboard); `CloseConnections`, `UpdateUser`, `CloseRoom`, `Notify` for the wiring's adapters.
- The hub calls `PushNotifier.ShareStarted` once per share (on `live`, not for `replaces`), with `PresentUserIDs`.
- Uses `Site.Origin` for the WebSocket Origin allowlist (M2 adds Wails origins in code),
  `limits.ws_handshakes_per_ip_per_minute`, `Deps.Policy` (03 settings), `config.KeyResume`.
- `internal/server/sfuplane` (01 §15.4), built by the wiring.

**02-sfu**
- `sfu.New(Config{Transport, PauseUnwatchedLayers, Limits, …})` calls `Transport.Apply` on each `SettingEngine`
  (probe engines: `Apply`, then its own setters) and sets the 10 s DTLS timeout; `SFU.Ready()`; `SFU.Snapshot()` and
  `Metrics()` as Go types (02 §13: `RoomSnapshot.Shares`/`Conns`, `ShareSnapshot`, `ConnSummary`, `SnapshotTotals`
  with `PCsByTransport`; every label's fixed value set; probe PCs left out of every PC count) for `LiveSource` and the
  metrics collector; selected pairs labeled from `Transport.Advertised` (`udp` | `tcp443` | `tcp7882`).
- `SFU.Probe` for `/api/v1/conntest`: one live probe per (user, transport), a new one replacing the old, 20 globally
  (`sfu.probe_limit`); `sfu.transport_disabled` whenever that transport's mux is nil; a result channel buffered with
  capacity 1 that is never waited on; IPv4 candidates only when a public IPv4 is known (02 §7.6).
- `SetLimits`; `Close()` for shutdown, closing PCs concurrently and returning within 1 s.

**03-accounts-and-store**
- `store.Open` with pre-migration backups; `*store.SchemaTooNewError{DBVersion, BinaryVersion, LastAppVersion,
  Backup}` (its `Error()` states the problem, the versions and the backup path, with no command; 04 appends the
  command, §6.1 step 4); the sentinel `store.ErrNeedsOperator`, wrapped by every `Open` failure that a restart can't
  fix: `store.Open` errors for which `errors.Is(err, store.ErrNeedsOperator)` → print the error and exit 78; any other
  `store.Open` error → exit 1. `(*DB).BackupTo`, `Close` with WAL checkpoint, `Ping` for readiness, `QuickCheck` and
  `Stats` for doctor; `meta` get/set; the tables `push_subscriptions`, `push_preferences`, `transfer_months` and their
  `*Q` methods (03 §6); `store.PushSubscription` (the wiring converts it to `push.Subscription`).
- File-level functions that never migrate and never write the source file: `store.LatestSchemaVersion()`,
  `store.InspectFile(ctx, dbPath, backupDir) (store.FileInfo, error)` and `store.BackupFile(ctx, srcPath, dstPath)`
  (read-only; for offline commands, restore validation and doctor's `schema` check, which needs the
  `mode=ro&immutable=1` open of §13.1), passed into `ops`/`ops/doctor` by `cmd/isshoni` and the wiring (§2).
- Auth service calls used by the admin socket: `SetupAvailable`, `IssueSetupToken` (`setup_unavailable` once an admin
  exists), `UserByUsername`, `IssuePasswordReset`, `UpdateUser` (role, status; last-admin guard), `CreateInvite`
  (0 = setting default), `db.Read → ListUsers` / `UserRow` (03 §12.6, §16); audit with `store.CLIActor`.
- `auth.Service.AuthenticateCookie(r)` (cookie only, `auth.ErrNoCookie`) for `/ws`; `Touch` for `Revalidate`.
- `httpapi.API` (the /api/v1 chain with no-store and JSON 404/405 for every path under `/api/v1/`, `Handle`,
  `PrincipalFrom`, `DashboardAccounts`), `api.Error` and `api.StatusOf` (with this doc's codes as (04) rows);
  `SettingsCache.Pin` (errors carry the field code), `Get`, `OnChange`, `Defaults()`; the `SPAStatus` hook (`/setup`
  → 404 after setup); `auth.ConnCloser`, `auth.AdminAlerter` (`auth.Options.Alerts`, may be nil), `httpapi.Signal`,
  `httpapi.Push` (nil answers `push_unavailable`), `httpapi.InfoSource` for the wiring.
- 03's handler checks a push subscription's body shape (`too_long`, `bad_keys`) before calling `ValidateEndpoint`.
- 03's startup fingerprint purge honours the rotation contract of §5.2.

**05-web-client**
- The service worker renders every push type (§14.3) from `en.json`, never silent; subscribe/refresh/re-subscribe
  flow using `/info.push.vapidPublicKey`; room route `/r/{roomId}?focus={shareId}`.
- Admin pages consume `OpsDashboard`, `DoctorReport` (by `code`/`params`/`fixCode`) and `/api/v1/admin/bandwidth`;
  wizard step 2 and "Test my connection" use `/api/v1/conntest` and provider fix texts `fix.firewall.<provider>`.
- Build output: hashed `/assets/*`, root `sw.js` and `manifest.webmanifest`, `version.json`, no dots in route
  segments, optional `.br`/`.gz`; shows "Server restarting…" on `server.shutdown` / close 1012 and on 503
  `server_shutdown`.

**06-deploy-and-ci**
- ldflags (§15, `date` = commit date) and one VERSION per build, passed as ldflags `version` and as
  `ISSHONI_VERSION` to the Vite build in every task and in goreleaser; `go.mod` `ignore ./web/node_modules` and
  `ignore ./docs/node_modules`; `web/dist/.gitkeep`; the same reserved `ISSHONI_*` list as §4.2.
- systemd unit: `Type=exec`, `RuntimeDirectory=isshoni`, `StateDirectory=isshoni` (`StateDirectoryMode=0700`),
  `ConfigurationDirectory=isshoni`, `AmbientCapabilities=CAP_NET_BIND_SERVICE`, `ExecReload=/bin/kill -HUP $MAINPID`,
  `TimeoutStopSec=20`, `Restart=on-failure`, `RestartPreventExitStatus=78`, `After=network-online.target`.
- Docker: binary at `/usr/local/bin/isshoni` (on `PATH`, so `docker compose exec isshoni isshoni …` works),
  `ENTRYPOINT ["/usr/local/bin/isshoni"]`, `CMD ["serve"]`, `HEALTHCHECK` `isshoni healthcheck`, `ISSHONI_IN_CONTAINER=1`,
  `/var/lib/isshoni` owned by 65532 in the image and mounted as a volume, `/run/isshoni` as a tmpfs owned by 65532
  (read-only root filesystem), same port numbers, `stop_grace_period: 20s`.
- install.sh: sysctls `net.core.rmem_max`/`wmem_max = 8388608`, config via `isshoni config init` then
  `root:isshoni 0640`, `isshoni doctor --only dns,public_ip,clock` before the first start, waits with `isshoni
  healthcheck --ready`, then `isshoni setup-url --qr`; Let's Encrypt agreement notice.
- CI: Pebble + challtestsrv job, golangci-lint `depguard` rules of §2, license gate covers the new deps
  (`skip2/go-qrcode` MIT, `x/sys`, `x/mod`, `x/time` BSD, `go-internal` BSD test-only, zap MIT and acmez Apache via
  certmagic).
- Release template marker `<!-- isshoni:security -->`; the `/privacy` page renders §16's table in full; config
  reference `/reference/config` from the registry (`isshoni config example`), hand-written in M1 (generated in M5).

---

## 20. Implementation slices

Ordered; each ends green in CI and is testable alone. The integrated plan (`README.md`, ids `Sxx`) sequences them
with the other docs and adds the wiring slices of §6.6.

| # | Slice | Scope | Acceptance | Depends on |
|---|---|---|---|---|
| S1 | CLI skeleton, version, logx | `cmd/isshoni` dispatch, help, exit codes (incl. 78); `internal/version` (+ `DocsURL`, `--short`); `internal/logx` (Secret, formats, ReplaceAttr) | `go build -ldflags -X…` → `isshoni version --json` shows the values and `--short` prints the version; logx unit tests; testscript for usage and exit 2 | – |
| S2 | Config | Registry, TOML/env/flag loading, precedence, validation messages, `config check/print/example/init`, `Site` | All §17 config tests; `config example` round-trips; `config init` refuses to overwrite; errors show file:line and a fix | S1 |
| S3 | Data dir and secrets | In `internal/server/config` only (§5.1–5.2; `serve` calls it from S4 on): umask; `PrepareDataDir` (create 0700, container volume check with exit 78 and the escape env, tighten, writable, admin socket directory, root warning); container detection (`Host`); the data-directory lock; the cgroup memory limit (`ApplyMemoryLimit`); the refusals as `OperatorError` with the reason codes of §6.3; `secrets.json` create/load (owner, mode, corrupt), VAPID pair, `InspectSecrets`, and `Rotate`, which only writes the file (the `rotate-secrets` command and its restart are S9) | Secrets tests (create, load, permissions, rotate); container check with a fake mountinfo exits 78 and passes with `ISSHONI_ALLOW_EPHEMERAL_DATA=1` | S2 |
| S4 | HTTP skeleton and SPA | `internal/server` with `off` mode, router, global middleware chain, JSON/error helpers on 03's `api.Error`, SPA handler, security headers, `/healthz` `/readyz`, graceful shutdown skeleton, `servertest` | httpapi tests; `servertest.Start` ready < 1 s; built SPA served with correct caching | S2, S3, 03 DTOs |
| S5 | Public IP and ICE transports | `DetectPublicAddrs`, rewrite rules, UDP mux with counting conns, 7882/tcp mux, `Transport` (incl. `TCPMux443/7882`, `PacketConns`), `Transport.Apply`, `TransferCounter` | netx unit tests; a Pion PeerConnection pair connects through `Transport` over UDP and over TCP 7882 on loopback | S1 |
| S6 | 443 multiplexer | `PortMux`, `prefixConn`, limits, ICE sub-listener into `TCPMuxDefault` | portmux tests (TLS, RFC 4571, plain HTTP hint, garbage, slow client, limits, Close); goleak clean | S5 |
| S7 | TLS manager | certmagic `auto`/`ip`, `manual` reload, port 80 handler, HSTS, zap bridge, hints; `servertest` TLS option with a test CA | Pebble CI job issues domain and IP certs; WSS (to a stub `/ws` handler) + h2 + ICE-TCP (a raw RFC 4571 STUN frame) on one port in `servertest`, with media over 443 left to W2; manual reload tests; **a Let's Encrypt staging IP certificate issued on a real VPS** | S4, S6 |
| S8 | Admin socket basics | Socket server/client, peer creds, `health`/`ready`/`status`, `healthcheck`, `setup-url` (+QR, `--wait`), `users`, `invite create`, `log-level` | testscript CLI tests; healthcheck exits 0/1 only; setup-url exits 7 after setup (fake auth) | S4 |
| S9 | Backup, restore, rotation | Backup tar, restore of archives and `.db` files with validate/swap/re-exec and crash recovery, offline mode, `rotate-secrets` with restart | Round-trip and crash-recovery tests; a newer-schema DB → `serve` exits 78 → offline restore of the pre-migration DB → serving | S8, 03 store API |
| W1 | Wiring v1 | §6.6 adapters for 01's hub (fake media) and 03's store/auth/httpapi; settings pins | `serve` in off mode: login over REST, `/ws` joins Lounge; `POST /api/v1/auth/logout` closes that session's socket (`session_revoked` on the wire) in < 100 ms | S4, S8, 01 P5, 03 sessions/setup |
| W2 | Wiring v2 | SFU + `sfuplane` + Transport + PortMux in `serve`; metrics adapter | 01 P8's integration tests pass against `servertest`; the ICE-TCP-via-443 test of §17 passes in `internal/server/itest` | W1, S7, 01 P8, 02 slice 7 |
| S10 | Ops data | Transfer accounting and alerts, metrics endpoint, release check (dashboard: S10b) | metrics scrape test; month rollover and alert-once tests; release parser tests | S5, S8 |
| S10b | Dashboard | `/api/v1/admin/dashboard` with `LiveSource`/`AccountsSource` adapters | Golden dashboard JSON with fake sources; a failing source doesn't fail the response | S10, W2 |
| S11 | doctor | All checks, `--only`, `--list-checks`, provider table, bandwidth calculator, text/JSON output, `/api/v1/admin/doctor`, `/api/v1/admin/bandwidth`, periodic run | doctor unit tests; numbers of §13.4; `doctor --json` on a VPS all ok | S7, S8, S10 |
| S12 | Web Push | VAPID, `httpapi.Push` methods, guarded sender, triggers, dedup, limits, pruning, VAPID fingerprint check | push unit tests incl. decrypting fake push service; manual iPhone and Android notification | S3, 03 tables |
| S12b | Push wiring | push as `PushNotifier`, `AdminAlerter`, `httpapi.Push` | a share going live in `servertest` reaches the fake push service for an absent user only | S12, W2 |
| S13 | Connection test | `/api/v1/conntest` handler on 02's `Probe` | In `servertest` with TLS the `tcp443` probe connects (§17); Playwright wizard step 2 shows UDP ✓ and TCP 7882 ✓ locally with the TCP 443 row hidden (off mode); with `listen.ice_udp = ""` the UDP row shows ✗ with the "UDP turned off" text | S7, 02 slice 14 |
| S14 | Hardening | SIGHUP, full shutdown with 01's notice, log canary, TCP-only e2e, `doctor` in Docker bridge and host modes | All integration and e2e tests of §17 pass; manual 2-hour session checklist | all above |

## Decisions taken at integration (formerly open questions)

1. **Transfer allowance in the wizard**: not in M1. The wizard keeps the plan's three steps; admins set
   `transferAlertGb` under Admin → Settings, and the dashboard always shows month-to-date transfer.
2. **Lock-screen content of "X started streaming"**: shows the sharer's name and the room (the plan's own example
   "Alex started streaming"); each user can switch these notifications off (`shareStarted: off`).
3. **Docker without a data volume**: a hard refusal (exit 78) with the exact fix, plus `ISSHONI_ALLOW_EPHEMERAL_DATA=1`
   for tests. Both 04 and 06 recommended it; it is a stricter form of the plan's "doctor fails".

Decided after group 5 (an engineering call the owner delegated; README §6):

4. **An ip-mode server that finds no public address starts and is not ready; it does not exit 78.** The cause is
   often transient at boot (the network or STUN isn't there yet), and exit 78 is for what a restart can't fix
   (§3.2); here a restart once the network is up is the fix. The server stays up, port 80 answers 503 with the
   waiting page, and the readiness checks `public_ip` and `tls` and doctor say what is missing (§6.2, §7.4, §8.2,
   §8.3, §13.2). It does not recover by itself: the site, the Host check, the certificate's name and the ICE
   rewrite rules are fixed at startup, so an address that the 10-minute detection finds later (wired by README
   S54) is only logged, and the operator restarts isshoni (§6.2). systemd does not restart a running server, and
   Docker's `HEALTHCHECK` passes meanwhile because it checks liveness. The same holds for manual mode without a
   domain. A `public_ip` literal that is not a public address stays a config error (exit 78, §4.5).
