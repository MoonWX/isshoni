# M1 · 04 · Server platform

`cmd/isshoni`, configuration, secrets, lifecycle, the 443 multiplexer and ICE transports, TLS, HTTP wiring and the
embedded SPA, logging, ops endpoints, the admin socket, `doctor`, and Web Push (server side).

Everything here is **M1** unless it is marked *later (Mx)*. Source of truth for product decisions: `docs/PLAN.md`.
Sibling specs, referenced instead of duplicated:
[01-protocol](01-protocol.md) · [02-sfu](02-sfu.md) · [03-accounts-and-store](03-accounts-and-store.md) ·
[05-web-client](05-web-client.md) · [06-deploy-and-ci](06-deploy-and-ci.md).

Example addresses use documentation ranges (`203.0.113.0/24`, `2001:db8::/32`) and `watch.example.com`.

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
  push/              service.go sender.go ssrf.go limits.go prune.go
  servertest/        servertest.go
```

Import rules (checked by a `depguard` rule in golangci-lint, 06):
- `version`, `logx`, `config` import nothing from `internal/server/...` (config may import `logx`).
- `netx` imports pion, `logx`; never `config` (it takes plain option structs, so `doctor` and tests can use it freely).
- `tlsmgr`, `ops`, `ops/doctor`, `push` import `config`, `logx`, `netx`, `internal/protocol`,
  `internal/protocol/api`. `httpapi` also holds 03's `API` and so imports `store` and `auth`.
- Only `internal/server` (the wiring) imports `sfu`, `sfuplane`, `signal`, `auth` and `store`, and adapts them to the
  small interfaces that `ops` and `push` declare. So `ops` and `push` compile and test without 01–03. `ops` and
  `push` expose plain `http.Handler`s that take the caller as arguments; the wiring registers them with 03's
  `API.Handle(pattern, access, h)` and passes the principal from `httpapi.PrincipalFrom`.
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
| `isshoni setup-url [--qr\|--no-qr] [--wait DUR] [--json]` | Mints a one-time setup link and prints it (plus a QR code on a TTY) | admin socket |
| `isshoni doctor [--json] [--strict] [--only ID,…] [--list-checks] [--bandwidth-flags…]` | Diagnostics (§13). Works with or without a running server. `--only` runs the named checks (install.sh uses `dns,public_ip,clock` before the first start); `--list-checks --json` prints the check ids (06's anchor test) | admin socket if up |
| `isshoni healthcheck [--ready] [--wait DUR]` | Liveness (default) or readiness. Docker `HEALTHCHECK` (liveness); install.sh uses `--ready` | admin socket |
| `isshoni admin status [--json]` | Version, uptime, TLS, rooms, connections | admin socket |
| `isshoni admin backup [--out PATH\|-] [--no-certs] [--offline]` | Writes a backup archive (§12.3) | admin socket |
| `isshoni admin restore PATH\|- [--yes] [--offline]` | Restores a backup archive, or only the database from a pre-migration `backups/pre-*.db` file, and restarts the server (§12.4) | admin socket |
| `isshoni admin users list [--json]` | Lists accounts | admin socket |
| `isshoni admin users reset-password NAME` | Prints a one-time password-reset link | admin socket |
| `isshoni admin users set-role NAME admin\|user` | Changes a role (03's roles) | admin socket |
| `isshoni admin users disable\|enable NAME` | Blocks or unblocks sign-in (disable revokes sessions and devices) | admin socket |
| `isshoni admin invite create [--uses N] [--ttl DUR]` | Prints an invite link (defaults from 03: 10 uses, 7 days) | admin socket |
| `isshoni admin rotate-secrets [--include-vapid] [--yes]` | Rotates generated secrets and restarts the server (§5.3) | admin socket |
| `isshoni admin log-level LEVEL [--for DUR]` | Changes the log level at runtime (default `--for 30m`, then back) | admin socket |
| `isshoni config check` | Loads and validates config; prints problems | – |
| `isshoni config print [--json]` | Effective config with the source of every value | – |
| `isshoni config example [config flags]` | Prints a commented `isshoni.toml` with the given values set | – |
| `isshoni config init --path PATH [config flags]` | Writes the same commented file to PATH (0640, the caller fixes the owner) and refuses to overwrite an existing file (exit 7). install.sh uses it, e.g. `--tls.mode ip --public-ip 203.0.113.7` or `--domain watch.example.com` (06 §4.8) | – |
| `isshoni version [--short\|--json]` · `isshoni --version` | Build info (§15); `--short` prints only the version, e.g. `0.1.0` (install.sh) | – |
| `isshoni help [CMD]` | Usage | – |

Conventions:
- Every command that reads config accepts `--config PATH` (env `ISSHONI_CONFIG`) and the config flags of §4.
  `serve` needs them; the others only need `listen.admin_socket` to find the socket.
- `--socket PATH` overrides `listen.admin_socket` for client commands.
- `--json` prints exactly one JSON document on stdout; human text goes to stderr.
- Colors only on a TTY and only when `NO_COLOR` is unset.
- Destructive commands (`restore`, `rotate-secrets`) ask `Continue? [y/N]` on a TTY; without a TTY they need `--yes`.
- The CLI never opens the database or writes to the data directory, except `--offline` commands (§12.6).

### 3.2 Exit codes

| Code | Meaning |
|---|---|
| 0 | Success (doctor: no `fail` results) |
| 1 | Runtime error: cannot bind a port, backup failed, unexpected server error |
| 2 | Usage error: unknown command or flag, missing argument |
| 78 | `EX_CONFIG`: a configuration or data problem that a restart can't fix: invalid config (all problems are printed, §4.5), a DB schema newer than this binary, a failed migration, a corrupt DB or `secrets.json`, or a container without a data volume (§6.3). systemd's `RestartPreventExitStatus=78` (06) stops the restart loop |
| 4 | Server not reachable on the admin socket (not running, wrong path, or permission denied) |
| 5 | `doctor`: at least one `fail` (with `--strict`, also any `warn`) |
| 7 | Refused: precondition not met (an admin already exists, backup is newer than this binary, `--offline` while the server runs, running as the wrong user, `config init` target exists) |
| 75 | `serve` on a platform without re-exec (Windows): "restart required" after a restore |
| 130 | Interrupted (SIGINT) |

**Exception, `healthcheck`**: exits only **0** (healthy) or **1** (unhealthy, unreachable, or any error), because
Docker reserves exit code 2 in `HEALTHCHECK`.

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

`--json`: `{"url":"https://203.0.113.7/setup#Qm9f…","expiresAt":"2026-09-30T10:00:00Z","tlsReady":true}`.

When the certificate is not ready yet, the link is still printed with a note: "The certificate isn't ready yet. The
link works once it is; check with `isshoni doctor`." `--wait 180s` first waits for readiness (install.sh uses this).

QR rendering uses `github.com/skip2/go-qrcode` (MIT) `ToSmallString`: half-block characters, 2-module quiet zone.
It is a new dependency (permissive) added for this command only.

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

All keys are declared once in `config/keys.go` (path, default, kind, help, `Policy` flag, consumer). That registry
drives the TOML decoder, env and flag parsing, `config print`, `config example`, `serve --help` and the generated
config reference on the project site (06). **Other docs add keys only through this registry.**

Unknown keys:
- In the file: error 78, with position and a "did you mean" suggestion (edit distance ≤ 2), from go-toml/v2
  `DisallowUnknownFields` (`*toml.StrictMissingError`).
- In env: a warning with the same suggestion (env may hold unrelated variables). `ISSHONI_VERSION` (install.sh) and
  `ISSHONI_CONFIG` are known and ignored.

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
| `network.include_loopback` | `false` | Dev only: advertise 127.0.0.1 / ::1 candidates |
| `network.udp_buffer_bytes` | `8388608` | SO_RCVBUF/SO_SNDBUF requested on media sockets; needs sysctl ≥ this (§13) |
| `network.trusted_proxies` | `[]` | CIDRs allowed to set `X-Forwarded-*` (`off` mode only, §8.5) |

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
| `limits.conns_per_ip` | `256` | netx: open TCP connections per source IP on 443 and 80 |
| `sfu.pause_unwatched_layers` | `true` | 02 §11: hint browsers to pause the full layer nobody watches |

**Env-only switches** (not keys; read directly): `ISSHONI_CONFIG` (config path), `ISSHONI_IN_CONTAINER=1` (set by
06's image; also detected, §5.1), `ISSHONI_ALLOW_EPHEMERAL_DATA=1` (tests only: skip the container data-volume check),
`ISSHONI_VERSION` (install.sh only; ignored).

**`[push]`, `[metrics]`, `[log]`, `[updates]`**

| Key | Default | Notes |
|---|---|---|
| `push.enabled` | `true` | Web Push (§14) |
| `push.subject` | `""` = derived | VAPID `sub`: `mailto:<tls.acme_email>` if set, else the public origin. Must start with `mailto:` or `https:` |
| `metrics.enabled` | `false` | Prometheus endpoint (§11.2) |
| `metrics.listen` | `"127.0.0.1:9469"` | Warning if not loopback: metrics are unauthenticated |
| `metrics.pprof` | `false` | `/debug/pprof/*` on the metrics listener |
| `log.level` | `"info"` | `debug` \| `info` \| `warn` \| `error` |
| `log.format` | `"auto"` | `auto` (text on a TTY, else JSON) \| `text` \| `json` |
| `updates.release_url` | GitHub releases API URL of `MoonWX/isshoni` | Hidden; tests only |

`isshoni config example --public-ip 203.0.113.7` prints (comments abbreviated here; `config init --path P` writes
the same text):

```toml
# isshoni server configuration. Every key also has an ISSHONI_* env variable and a --flag.
# Reference: https://<project-site>/config

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
	Dev      bool    // off mode on a loopback listener: accept any localhost Host header
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
| `push.subject` starts with `mailto:` or `https:` | E |
| `clients.min_version` is SemVer | E |
| `metrics.listen` not loopback | W |
| `tls.acme_staging` or `tls.acme_ca_root` set | W (not for production) |

Checks that need the machine (file readable, port free, cert matches) run at startup or in `doctor`, not here.

### 4.6 Policy keys and the admin UI

A few product settings can be changed in the admin UI (stored by 03 in `settings`, same dotted key names) **and**
pinned by an operator in config. The rule, borrowed from Mattermost's env overrides:
- If a policy key is set by flag, env or file, it wins, and the admin UI shows the field read-only with "Set in the
  server config".
- Otherwise the DB value applies; otherwise the registry default.

Mechanism: after `store.Open` and before serving, the wiring calls 03's `SettingsCache.Pin(field, value)` for every
policy key where `cfg.IsSet(key)` is true, using the key → field map in §4.3. 03's `Locked()` then lists the field,
the admin UI shows it read-only, and `PATCH /admin/settings` answers 409 `setting_locked`.

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
	TrustedProxies    []netip.Prefix
}
type Registration struct{ Mode string }
type Clients struct{ MinVersion string }
type Limits struct {
	TransferAlertGB            int
	MaxBitrateKbps             int
	WSHandshakesPerIPPerMinute int
	ConnsPerIP                 int
}
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
├─ secrets.json               0600 (§5.2)
├─ certmagic/                 0700, certmagic FileStorage (certificates, ACME account keys, locks)
├─ backups/                   0700: pre-<schema>-<ts>.db (03, last 5), pre-restore-<ts>.tar.gz (§12.4, last 5)
└─ restore/                   transient staging for restores
```

Startup checks (fail → exit 78 with a fix line; restarting can't help):
- The server sets `umask 0077` first, so everything it creates is private.
- `data_dir` must exist and be writable by the process (fix: `sudo chown -R isshoni:isshoni /var/lib/isshoni`, or in
  Docker for bind mounts `sudo chown -R 65532:65532 ./data`).
- **In a container, `data_dir` must be a mount** (checked in `/proc/self/mountinfo`: a mount point equal to `data_dir`
  or a parent other than `/`). Otherwise exit 78: "Your data would be lost when the container is removed. Mount a
  volume at /var/lib/isshoni (see compose.yaml), e.g. `-v isshoni-data:/var/lib/isshoni`." `ISSHONI_ALLOW_EPHEMERAL_DATA=1`
  skips the check (tests only). Rationale: losing the admin account and certs on `docker compose down` is the worst
  failure for a foolproof setup (a stricter form of the plan's "doctor fails").
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
- File mode wider than 0600 → fixed to 0600 with a warning. Owner ≠ process uid → exit 1 ("run: sudo chown isshoni:
  isshoni /var/lib/isshoni/secrets.json"). Corrupt JSON → exit 78 with reason `secrets_corrupt` and the fix "restore
  it: `isshoni admin restore --offline <backup>`" (never silently regenerated: that would log everyone out).
- VAPID keys come from `webpush.GenerateVAPIDKeys()`.

What each key is for is 03's and 01's business; 04 fixes the **rotation contract**. Rotation always restarts the
server (§5.3), so every consumer only has to handle "the key differs from last time" **at startup**:

| Key | Consumer | After rotation (at the next start) |
|---|---|---|
| `session` | 03 | Key fingerprint changed → all web sessions **and** device tokens are deleted (03 §4.6; everyone signs in again; desktop apps relink, M2+) |
| `invite` | 03 | Fingerprint changed → outstanding invite, setup, password-reset and device-code links are deleted |
| `resume` | 01 | Nothing to do: old resume tokens fail the HMAC; clients do a fresh join (harmless) |
| `vapid` | push (§14) | Fingerprint in 03's `meta` key `vapid_key_fp` changed → all push subscriptions are deleted; each browser re-subscribes the next time the app opens (05) |

### 5.3 Rotation (`isshoni admin rotate-secrets`)

- Default set: `session`, `invite`, `resume`. `--include-vapid` adds VAPID. Rationale: VAPID rotation silently stops
  phone notifications until each phone reopens the app, and a leaked VAPID key only lets someone send notifications
  if they also have the subscription list.
- Hard cut, no overlap window: rotation is for suspected compromise, so old keys must stop working immediately.
- Order: write the new file atomically → reply `202 {"rotated": [...], "restarting": true}` → graceful shutdown with
  reason `restart` (clients see the restart notice, §6.4) → re-exec (§6.5). At startup each consumer compares key
  fingerprints (table above) and purges; 03 writes the `secrets.rotated` audit row and the `secrets_rotated` admin
  alert. Rationale: no live key swapping and no hook ordering; a restart costs clients a few seconds, and rotation is
  rare.

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
	PushSender push.Sender
	ReleaseHTTP *http.Client
}

type Server struct{ /* … */ }

func New(cfg *config.Config, log *slog.Logger, deps Deps) (*Server, error)
// Run blocks until ctx is cancelled (then shuts down gracefully) or a restore requests a restart.
func (s *Server) Run(ctx context.Context) error
func (s *Server) Shutdown(ctx context.Context, reason ShutdownReason) error
func (s *Server) Site() config.Site

type ShutdownReason string // "stop" | "restart" | "restore"

var ErrRestartRequested = errors.New("server: restart requested") // cmd/isshoni re-execs on this
```

### 6.1 Startup sequence (`serve`)

1. Parse config (exit 78 on errors). Build the logger (§10).
2. Data dir checks, `umask`, memory limit (§5.1).
3. Open `secrets.json` (create on first run; corrupt → exit 78).
4. Open the store (03: migrations with the pre-migration backup). A `*store.SchemaTooNewError`, a failed migration or
   a corrupt database → print 03's message (it names the backup to restore) and exit 78 (§6.3). Then pin the policy
   settings that the config sets (§4.6).
5. Detect public addresses (§7.4; ≤ 5 s).
6. Bind listeners: 443 multiplexer, 80, 7882/udp (per local IP), 7882/tcp, metrics, admin socket. A busy port → exit 1
   with the owner if known ("port 443 is in use (another web server?). Stop it, or run isshoni behind it with
   `tls.mode = "off"`").
7. Start the TLS manager (certificate is obtained asynchronously).
8. Build `netx.Transport`, then the components and adapters of §6.6 (SFU, hub, auth, httpapi, push, ops); register
   readiness checks.
9. Start HTTP servers and the admin socket (listeners are bound; the certificate may still be pending).
10. Log one line: `isshoni 0.3.0 ready: https://203.0.113.7 (tls=ip) media udp/7882 ice-tcp 443,7882`. If no admin
    exists: "Finish setup: run `isshoni setup-url` (Docker: `docker compose exec isshoni isshoni setup-url`)". The
    setup token itself is **never** logged.

### 6.2 Liveness and readiness

`ops.Health` aggregates named checks. Readiness needs all of:

| Check | Owner | Ready when |
|---|---|---|
| `db` | 03 | `db.Ping` succeeds |
| `tls` | tlsmgr | A valid certificate for the site name is loaded (`off`: always) |
| `media` | netx + 02 | ≥ 1 UDP socket bound (or UDP disabled and a TCP mux up) and `SFU.Ready()` |
| `signal` | 01 | `Hub.Ready()` |
| `public_ip` | netx | `ip` mode only: a public address is known |

Liveness is false after shutdown began. Endpoints: §11.1.

### 6.3 Refusing to start

When the server loads its config but can't safely serve (newer schema, failed migration, corrupt DB, corrupt
`secrets.json`, no data volume in a container), it **exits 78** with one actionable message (plan: "refuses to start
on a newer schema and tells the user which backup to restore"). Rationale: simpler than a half-running server, and
systemd's `RestartPreventExitStatus=78` (06) prevents a restart loop.

Recovery is offline (§12.6), because nothing is listening:
- systemd: `sudo -u isshoni isshoni admin restore --offline /var/lib/isshoni/backups/pre-4-20261014T021500Z.db` (a
  pre-migration DB file) or `… --offline backup.tar.gz`, then `sudo systemctl start isshoni`. Installing the newer
  version again is the other fix for a newer schema.
- Docker (the container restarts in a loop under `restart: unless-stopped`): `docker compose stop`, then
  `docker compose run --rm isshoni admin restore --offline /var/lib/isshoni/backups/…`, then `docker compose up -d`.
- Offline `isshoni doctor` shows the same reason (check `schema` or `secrets`).

Reasons (codes, used in logs and doctor): `schema_newer`, `migration_failed`, `db_corrupt`, `secrets_corrupt`,
`data_not_mounted`.

### 6.4 Graceful shutdown: what clients see

On SIGTERM/SIGINT (systemd stop and restart both send SIGTERM, so every shutdown is treated as a possible restart),
and for a restore or `rotate-secrets` restart:

| Step | Time budget | Effect |
|---|---|---|
| 1. `readyz` → 503 `shutting_down` | 0 | Load balancers/monitors stop sending |
| 2. New `/ws` upgrades and API calls → 503 `server_shutdown` (`Retry-After: 5`) | 0 | |
| 3. `Hub.Shutdown(ctx, reason)` (01): every connection gets `server.shutdown{reason, reconnectInMs}` and `error{code: "server_shutdown", retryable: true, scope: "connection"}`, then WebSocket close **1012 (Service Restart)**. The wire reason is `restart` for SIGTERM, restore and rotation (SIGTERM can't tell stop from restart) | ≤ 2 s | The SPA shows "Server restarting… reconnecting" and reconnects after `reconnectInMs`; after the restart it rejoins and re-publishes (01 §10.5–§10.6) |
| 4. `SFU.Close()` closes all PeerConnections; then `Transport.Close()` closes the muxes | ≤ 1 s | |
| 5. `http.Server.Shutdown` on all servers | ≤ 5 s | Idle keep-alives close |
| 6. Flush transfer counters, drain the push queue (≤ 2 s), close the admin socket, close the store (WAL checkpoint) | ≤ 2 s | |
| 7. Exit 0 (or re-exec, §6.5) | total ≤ `shutdown_timeout` (10 s) | |

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

The only place that imports 01's `signal` and `sfuplane`, 02's `sfu`, and 03's `store`, `auth` and `httpapi` together.
Each adapter is a few lines and has a table test.

| Consumer interface | Implemented by | Mapping |
|---|---|---|
| `sfu.Config.Transport` | `netx.NewTransport` (§7.3) | built once; the SFU calls `Apply` |
| `sfu.Deps.Events` (`RoomEvents`) and `signal.Deps.Media` (`MediaPlane`) | 01's `sfuplane.New`, then `Plane.Bind(sfu)` | 01 §15.4 |
| `signal.Authenticator` | 03's `auth.Service` | `AuthenticateRequest` → `Authenticate(r)` (cookie only in M1; `Principal` → `Identity{UserID, Name: Username, Admin: Role == admin, SessionID}`); `Revalidate(ctx, id, ip)` → `Touch` + user re-read; `AuthenticateBearer` → `ErrInvalid` in M1 (M2: `AuthenticateBearerToken`) |
| `signal.RoomDirectory` | 03's store | `GetRoom` → `RoomByID` (`store.ErrNotFound` → `signal.ErrNotFound`); `DefaultRoomID` → `store.DefaultRoomID`; `CanJoin` → nil |
| `signal.Deps.Policy` | 03's `SettingsCache.Get()` | `minClientVersion`, `maxParticipantsPerRoom`, `maxSharesPerRoom`, `maxShareBitrateKbps × 1000` |
| `signal.PushNotifier` | this doc's `push.Service.ShareStarted` | same fields (`PresentUserIDs`, `At`) |
| `auth.ConnCloser` | 01's `Hub.CloseConnections` | selector fields copied; `account_disabled` → `ErrorCodeAccountDisabled`, every other reason → `ErrorCodeSessionRevoked` |
| `auth.AdminAlerter` | `push.Service.AdminAlert` | payload `admin.alert` (§14.3) |
| `httpapi.Signal` | 01's hub | `RoomPresence`/`OnlineUserIDs` from `Hub.Snapshot()`; `RoomDeleted` → `CloseRoom`; `UserChanged` → `UpdateUser`; `Notify` → `Hub.Notify` |
| `httpapi.Push` | `push.Service` | `VAPIDPublicKey`, `ValidateEndpoint`, `SendTest` |
| `httpapi.InfoSource` | `internal/version`, `internal/protocol` | `ServerVersion`, `Protocol` |
| 02's live limits | `SettingsCache.OnChange` | `SFU.SetLimits(Limits{MaxShareKbps: maxShareBitrateKbps})` |
| `ops.LiveSource`, `ops.AccountsSource`, `ops.Prober` | hub + SFU, 03's `API.DashboardAccounts`, `SFU.Probe` | §11.4, §7.7 |
| `push.Store`, `push.Directory` | 03's store | §14.7 |
| REST routes of `ops` and `push`-adjacent handlers | 03's `API.Handle` | `POST /api/v1/conntest` (User), `GET /api/v1/admin/dashboard`, `GET\|POST /api/v1/admin/doctor`, `GET /api/v1/admin/bandwidth` (Admin) |
| Admin socket handlers | 03's `auth.Service`, `store.DB` | 03 §12.6 table |
| Metrics | `ops.Metrics.Registerer()` to 01; an adapter over `SFU.Metrics()` | §11.2 |

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
	MaxPending       int           // 1024 connections waiting for their first byte
	MaxPendingPerIP  int           // 32
	MaxConnsPerIP    int           // limits.conns_per_ip (256): open TLS + ICE connections per source IP
	MaxICEConnsPerIP int           // 64
	QueueLen         int           // 128 per sub-listener; a full queue for 1 s drops the connection
	PublicHost       string        // for the plain-HTTP hint response
	Counter          *TransferCounter
	Logger           *slog.Logger
}

type PortMux struct{ /* … */ }

func ListenPortMux(opts PortMuxOptions) (*PortMux, error)
func (m *PortMux) TLS() net.Listener // conns whose first byte was 0x16; the byte is replayed
func (m *PortMux) ICE() net.Listener // conns that start with an RFC 4571 frame; Addr() is the *net.TCPAddr of :443
func (m *PortMux) Addr() net.Addr
func (m *PortMux) Close() error      // both sub-listeners then return net.ErrClosed from Accept
func (m *PortMux) Stats() PortMuxStats

type PortMuxStats struct {
	TLS, ICE, PlainHTTP, Garbage, Timeout, Limited uint64 // accepted-connection outcomes
}
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
| anything else | close | Counted as `Garbage` |

Handoff:
- `TLS()` goes to `http.Server.ServeTLS(listener, "", "")` with the tlsmgr `TLSConfig`. Go's server uses the smallest
  of `ReadHeaderTimeout`/`ReadTimeout`/`WriteTimeout` as the TLS handshake timeout; `ReadHeaderTimeout = 10 s` gives
  the plan's 10 s handshake limit.
- `ICE()` goes to `ice.NewTCPMuxDefault(ice.TCPMuxParams{Listener: m.ICE(), FirstStunBindTimeout: 10 * time.Second,
  AliveDurationForConnFromStun: 30 * time.Second, ReadBufferSize: 64, WriteBufferSize: 4 << 20, Logger: pionLog})`.
  DTLS then has 10 s too (02 sets it with `SetDTLSConnectContextMaker`).
- Per-IP counts are decremented when a connection closes (the wrapper's `Close` hook). Exceeding a limit closes the
  new connection immediately and counts it as `Limited`.
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
	ExcludeInterfaces []string
	IncludeLoopback   bool
	IPv6              bool
	UDPBufferBytes    int
	Public            PublicAddrs
	InContainer       bool
	CloudProvider     string   // §13.3
	Counter           *TransferCounter
	PacketConns       []net.PacketConn // tests only: use these instead of binding UDPAddr (02's sfutest.FaultConn)
	Logger            *slog.Logger
}

type AdvertisedAddr struct {
	Proto string // "udp" | "tcp"
	Addr  netip.AddrPort
	Via   string // "udp7882" | "tcp443" | "tcp7882"
	LAN   bool   // kept private address (Append mode, §7.5)
}

type Transport struct {
	UDPMux          ice.UDPMux // *ice.MultiUDPMuxDefault; nil if UDP disabled
	TCPMux          ice.TCPMux // *ice.MultiTCPMuxDefault over 443 and 7882; nil if neither
	TCPMux443       ice.TCPMux // the 443 part alone (02's per-transport probe APIs); nil in off mode
	TCPMux7882      ice.TCPMux // the 7882 part alone; nil if listen.ice_tcp is 
	NetworkTypes    []webrtc.NetworkType // udp4/udp6/tcp4/tcp6 as available
	RewriteRules    []webrtc.ICEAddressRewriteRule
	InterfaceFilter func(name string) bool
	IPFilter        func(ip net.IP) bool
	IncludeLoopback bool
	Advertised      []AdvertisedAddr
	RcvBuf, SndBuf  int // effective socket buffers read back with getsockopt
}

func NewTransport(ctx context.Context, opts TransportOptions) (*Transport, error)

// Apply configures a SettingEngine: SetICEUDPMux, SetICETCPMux, SetNetworkTypes, SetInterfaceFilter,
// SetIPFilter, SetICEAddressRewriteRules, SetIncludeLoopbackCandidate, and mDNS disabled.
// 02 calls it for each webrtc.API it builds, then adds its own settings (DTLS timeout, ICE timeouts…).
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
  (tests): bind the first address, reuse its port for the others.
- **TCP**: `ice.NewTCPMuxDefault` on the 443 ICE sub-listener and on a plain 7882 listener, combined with
  `ice.NewMultiTCPMuxDefault(mux443, mux7882)`. pion's gatherer uses `GetAllConns` of the multi mux, so each local
  address gets a passive TCP candidate on **both** 443 and 7882. Both sub-listeners report an unspecified IP, so pion
  advertises every local address (then filtered and rewritten).
- Interfaces are enumerated once at startup (06: systemd `After=network-online.target`). Hot-plugged interfaces need a
  restart.

### 7.4 Public IP detection and NAT classification

```go
package netx

type Method string  // "config" | "interface" | "stun" | "none"
type NATKind string // "none" | "one_to_one" | "port_forward" | "symmetric" | "cgnat_likely" | "unknown"

type PublicAddrs struct {
	V4, V6        netip.Addr // zero if none
	V4Method      Method
	V6Method      Method
	LocalV4       netip.Addr // IPv4 of the default-route interface (may equal V4)
	NAT           NATKind
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

Privacy: STUN contacts Cloudflare and Google at startup and every 10 minutes (one UDP packet each). Setting `public_ip`
limits this to one NAT check at startup. The privacy page says so (§16).

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
               "server": {"publicIp": "203.0.113.7", "provider": "hetzner",
                          "udpPort": 7882, "tcpPorts": [443, 7882], "nat": "none"}}
Errors:   400 bad_sdp · 409 transport_disabled (tcp443 in off mode) · 429 rate_limited · 503 not_ready
```

```go
package ops

type Prober interface { // the wiring adapts 02's SFU.Probe
	Probe(ctx context.Context, userID string, transport string, offerSDP string) (answerSDP string, err error)
}
```

- 02 builds one probe `webrtc.API` per transport from `Transport.Apply` with only that transport's mux
  (`UDPMux`, `TCPMux443`, `TCPMux7882`), so the answer is complete, non-trickle and holds only that transport's
  candidates. The browser's own candidates are not needed: its checks create peer-reflexive candidates on the server.
- The probe PC echoes every data-channel message (the browser measures RTT; `getStats` `currentRoundTripTime` also
  works). It closes after 20 s or when the channel closes. ICE timeouts: disconnected 3 s, failed 8 s; DTLS 10 s.
- Limits: 12 requests per minute per user here; 3 concurrent probes per user (05 runs all three at once) and 20
  globally in 02 (`sfu.probe_limit` → 429 `rate_limited`).
- `provider` (§13.3) and `nat` (§7.4) let 05 show provider-specific fix text from its catalog; 05 shows it to admins
  only.

Types (`internal/protocol/api/conntest.go`): `ConnTestRequest`, `ConnTestResponse`, `ConnTestServerInfo`.

### 7.8 Limits and timeouts (summary)

| Item | Value |
|---|---|
| 443 first byte | 10 s |
| TLS handshake (via `ReadHeaderTimeout`) | 10 s |
| ICE-TCP first STUN Binding | 10 s; connection created from STUN with unknown ufrag lives 30 s |
| DTLS handshake (02 sets it) | 10 s |
| Pending (unclassified) connections | 1024 total, 32 per IP |
| Open connections per IP on 443/80 | 256 (`limits.conns_per_ip`); ICE-TCP 64 per IP |
| HTTP servers | `ReadHeaderTimeout` 10 s, `IdleTimeout` 120 s, `MaxHeaderBytes` 16 KiB, no `ReadTimeout`/`WriteTimeout` (WebSocket) |
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

- **Pin `github.com/caddyserver/certmagic v0.25.4`** (June 2026). Needed features: ACME `Profile` (since v0.22.0);
  IP identifiers allowed for Let's Encrypt in `ACMEIssuer.PreCheck` (PR #345, merged July 2025, first in v0.24.0);
  HTTP-01 for IPv6 literals fixed in v0.25.3. Caddy issue #7399 (Dec 2025) reported a PreCheck refusal for IP
  certificates with an older build, so slice S7 must issue a real staging IP certificate on a VPS before M1 is done.
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
func (m *Manager) TLSConfig() *tls.Config            // nil in off mode
func (m *Manager) HTTPHandler(app http.Handler) http.Handler // port 80 (§8.3); app is used only in off mode
func (m *Manager) Reload() error                     // manual: re-read files (SIGHUP)
func (m *Manager) Status() Status
func (m *Manager) Ready() (bool, string)             // readiness check "tls"
```

certmagic configuration (auto and ip):

```go
cache := certmagic.NewCache(certmagic.CacheOptions{GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) { return cfg, nil }, Logger: zl})
cfg = certmagic.New(cache, certmagic.Config{
	Storage:            &certmagic.FileStorage{Path: opts.StorageDir},
	DefaultServerName:  name, // IP clients send no SNI (RFC 6066 forbids IP literals in SNI)
	FallbackServerName: name, // unknown SNI gets our cert and a clear name-mismatch error instead of a handshake failure
	RenewalWindowRatio: ratio, // auto: certmagic default (1/3); ip: 0.5, so a 160 h cert renews after ~80 h with 3 days of slack
	OnEvent:            m.onEvent, // cert_obtained / cert_failed / cached_managed_cert → Status, readiness
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

### 8.4 Manual mode

- Load with `tls.LoadX509KeyPair`; keep it in an `atomic.Pointer[tls.Certificate]`; `GetCertificate` returns it.
- Reload on `SIGHUP` and when either file's mtime or size changes (polled every 60 s; no fsnotify dependency). A bad
  new pair is rejected and logged; the old one keeps serving.
- Checks at load (errors keep the server not ready; warnings go to doctor): key matches cert; not expired; SANs cover
  `Site.Hostname` (warning); expires within 14 days (warning); files readable by the service user (error with fix).

### 8.5 Off mode and trusted proxies

- The app is served over plain HTTP on `listen.http`. There is no 443 multiplexer, so ICE-TCP uses 7882 only (the
  proxy owns 443).
- `X-Forwarded-For` and `X-Forwarded-Proto` are honored only when the TCP peer is inside `network.trusted_proxies`.
  Client IP = the right-most `X-Forwarded-For` entry that is not itself a trusted proxy. `X-Forwarded-Host` and
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
	SPA            fs.FS          // web.Dist()
	SPAStatus      func(path string) int // 03 hook: 404 for "/setup" once an admin exists
	Gate           *Gate          // shutting-down switch
	API            http.Handler   // 03's *API, mounted at /api/v1/
	WS             http.Handler   // 01's *signal.Hub, mounted at /ws
	Metrics        *ops.Metrics
	Logger         *slog.Logger
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
```

### 9.3 Middleware chain (outermost first)

1. **Recover**: panic → 500 `internal`, logged with stack and route pattern, never with body or query.
2. **Request ID**: 16 hex chars, `X-Request-Id` response header.
3. **Transfer/metrics**: response bytes and status class per route pattern.
4. **Gate**: shutting down → 503 `server_shutdown` (`Retry-After: 5`). Exempt: `/healthz`, `/readyz`.
5. **Host check**: `Host` must equal `Site.Host` (or, when `Site.Dev`, any `localhost`/`127.0.0.1`/`[::1]` host so the
   Vite proxy works). Otherwise `421 Misdirected Request`, plain text. Exempt: `/healthz`, `/readyz`. Rationale: blocks
   DNS-rebinding against LAN installs and keeps cookies and Origin checks consistent.
6. **Real IP**: stores `ClientIP` in the context.
7. **Security headers** (§9.6).
8. Inside `/api/v1/`: 03's chain (body limit → no-store → CSRF → authenticate → rotate → access, 03 §12.1), then the
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
`server_shutdown`, `doctor_busy`; it also uses 03's `bad_request`, `payload_too_large`, `unsupported_media_type`,
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
  `index.html` is missing, the server serves a built-in page "Web UI not built: run `task build:web`" (dev only).
- 06 adds `ignore ./web/node_modules` to `go.mod` (Go 1.25+ directive) so `go build ./...` never walks npm packages
  that ship `.go` files.
- At startup the handler walks `Dist()` once and precomputes, per file, an ETag (first 16 hex chars of SHA-256) and the
  content type.

Serving rules:

| Request | Response | `Cache-Control` |
|---|---|---|
| `GET/HEAD /assets/<hashed name>` that exists | the file | `public, max-age=31536000, immutable` |
| `/assets/…` that does not exist | `404` (never `index.html`: a stale hashed asset after an upgrade must fail as a 404, not as HTML with a JS MIME error) | `no-store` |
| `/sw.js`, `/manifest.webmanifest`, `/index.html` | the file | `no-cache` (revalidate with ETag) |
| other existing root files (icons, `robots.txt`) | the file | `public, max-age=86400` |
| path whose last segment contains a `.` and does not exist | `404` | `no-store` |
| `/api/…`, `/ws`, `/healthz`, `/readyz` not matched by a route | `404` JSON `not_found` | `no-store` |
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
  startup; a mismatch logs a warning (only possible in dev builds).

### 9.6 Security headers

| Header | Value | Applies to |
|---|---|---|
| `Content-Security-Policy` | `default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; media-src 'self' blob:; connect-src 'self' wss://<host>; worker-src 'self'; manifest-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'` | HTML |
| `Content-Security-Policy` | `default-src 'none'; frame-ancestors 'none'` | everything else |
| `X-Content-Type-Options` | `nosniff` | all |
| `Referrer-Policy` | `no-referrer` (plan) | all |
| `Cross-Origin-Opener-Policy` | `same-origin` | HTML |
| `Cross-Origin-Resource-Policy` | `same-origin` | all |
| `Permissions-Policy` | `display-capture=(self), fullscreen=(self), picture-in-picture=(self), autoplay=(self), camera=(), microphone=(), geolocation=(), usb=(), payment=(), browsing-topics=()` | HTML |
| `Strict-Transport-Security` | `max-age=31536000` (no `includeSubDomains`, no `preload`: other subdomains of the admin's domain are not ours) | TLS responses when serving a domain and `tls.hsts`; never for IP hosts (RFC 6797 ignores them) |
| `Cache-Control` | `no-store` | every `/api` response (03 may relax specific GETs) |
| `X-Robots-Tag` | `noindex` | all |

Notes: `connect-src` lists `wss://<host>` explicitly because older Safari versions don't match `wss:` with `'self'`
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
func (Secret) Format(f fmt.State, verb rune)     // "[redacted]" for every verb
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
| `GET /readyz` | `{"status":"ready"}` | `{"status":"not_ready"}` |

Public responses carry no detail. Requests from loopback (and the admin socket's `/v1/ready`) add
`"checks": {"db":"ok","tls":"waiting: obtaining certificate for 203.0.113.7","media":"ok"}`.

```go
package ops

type Health struct{ /* … */ }

func NewHealth() *Health
func (h *Health) AddCheck(name string, fn func() (ok bool, detail string))
func (h *Health) SetMaintenance(reason string)
func (h *Health) SetShuttingDown()
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
`isshoni_sfu_selected_transport`). No per-user labels.

### 11.3 Transfer accounting and alerts

- Bytes are counted at the socket layer: UDP mux sockets (`media_udp`), ICE-TCP connections (`media_tcp`), HTTPS and
  HTTP connections (`web`). Rationale: VPS providers bill packets on the wire, including SRTP, RTCP and STUN overhead
  that RTP payload counters miss.
- `netx.TransferCounter` keeps atomic totals. `ops.Transfer` flushes deltas to the DB every 60 s and at shutdown, per
  **calendar month in UTC**, in 03's table `transfer_months` (03 §5, `AddTransfer`/`TransferMonth`).
- Alerts when month-to-date egress crosses 80 % and 100 % of the setting `transferAlertGb` (pinnable by
  `limits.transfer_alert_gb`; 1 GB = 10⁹ bytes): once per threshold per month (remembered in 03's `meta` key
  `ops.transfer_alert_sent` = `"2026-09:80"`), shown as a dashboard alert and pushed to admins as the admin alert
  `transfer_threshold` (03 §7.11, payload §14.3).
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
    "advertised": [{"proto": "udp", "addr": "203.0.113.7:7882", "via": "udp7882"}, {"proto": "tcp", "addr": "203.0.113.7:443", "via": "tcp443"}],
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

```go
package ops

type LiveSource interface {
	Rooms(ctx context.Context) []api.RoomLive // participants, connections, shares, layers, viewers
	Media(ctx context.Context) api.MediaTotals
}
type AccountsSource interface {
	Accounts(ctx context.Context) (api.DashboardAccounts, error) // 03's API.DashboardAccounts; 1 s timeout
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
- First run 5–60 minutes after start (random), then every 24 h ± 1 h. Off when `updates.release_check=false` (policy
  key, so the admin UI can switch it off).
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
- Every mutating call is written to 03's audit log with actor `cli` and the peer uid.
- Base URL for the client: `http://isshoni/v1/…` with a `DialContext` to the socket. Errors use the envelope of §9.4
  plus an English `message` and `fix` for the CLI to print.

### 12.2 Endpoints

| Method and path | Request | Response | Errors |
|---|---|---|---|
| `GET /v1/health` | – | `{"status":"ok","version":"0.3.0"}` | 503 `{"status":"shutting_down"}` |
| `GET /v1/ready` | – | `{"status":"ready","checks":{…}}` | 503 not ready |
| `GET /v1/status` | – | version, uptime, site, TLS status, public addrs, listeners, rooms/participants/shares counts, schema version | – |
| `POST /v1/setup-url` | `{"waitReadyS": 0}` | `{"url":"https://…/setup#…","expiresAt":"…","tlsReady":true}` | 409 `admin_exists` |
| `GET /v1/users` | – | `{"users":[{"id","username","role","status","createdAt","lastSeenAt"}]}` | – |
| `POST /v1/users/{name}/reset-link` | – | `{"url":"https://…/reset#…","expiresAt":"…"}` | 404 `user_not_found` |
| `POST /v1/users/{name}/role` | `{"role":"admin"}` | 204 | 404, 409 `last_admin` |
| `POST /v1/users/{name}/disable` · `/enable` | – | 204 | 404, 409 `last_admin` |
| `POST /v1/invites` | `{"uses":10,"ttl":"168h"}` | `{"url":"https://…/invite#…","expiresAt":"…"}` | 409 `registration_closed` |
| `GET /v1/backup?certs=1` | – | `application/gzip` stream, `Content-Disposition: attachment; filename="isshoni-backup-…tar.gz"` | 500 |
| `POST /v1/restore` | `application/gzip` archive or `application/vnd.sqlite3` DB body (≤ 2 GiB) | 202 `{"restarting":true,"preRestoreBackup":"backups/pre-restore-20260929T101500Z.tar.gz"}` | 400 `backup_invalid`, 409 `backup_newer`, 409 `restore_in_progress`, 507 `insufficient_storage` |
| `POST /v1/rotate-secrets` | `{"keys":["session","invite","resume"],"vapid":false}` | 202 `{"rotated":[…],"restarting":true}` (the server restarts, §5.3) | 400 |
| `POST /v1/doctor` | `{"only": ["dns"]}` (optional) | `api.DoctorReport` (server-side checks) | – |
| `POST /v1/log-level` | `{"level":"debug","for":"30m"}` | 204 | 400 |

User, invite and setup operations call 03's service layer (§19); this doc only defines the socket surface.

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
  `docker compose exec -T isshoni isshoni admin backup --out - > isshoni-backup.tar.gz`.
- `--offline` (server stopped, §12.6): the CLI opens the DB read-only and runs the same `VACUUM INTO`; if the DB can't
  be opened (corrupt, or newer than this binary), it copies the raw files (`isshoni.db`, `-wal`, `-shm`) and sets
  `"raw": true` in the manifest.

### 12.4 Restore

Two inputs are accepted:
- a **backup archive** (`.tar.gz` from §12.3): database, `secrets.json` and certificates;
- a **database file** (the SQLite header `SQLite format 3\0`), typically 03's pre-migration backup
  `backups/pre-<schema>-<ts>.db`: only the database is replaced; `secrets.json` and `certmagic/` stay. This is the
  recovery named by 03's "schema newer" message (03 §4.5).

Server side (or the CLI with `--offline`), after receiving the body into `restore/incoming`:
1. **Validate**: gzip/tar well-formed; entries are regular files or directories only; names limited to the manifest's
   contents; no absolute paths or `..` (extraction goes through `os.OpenRoot(restore/new)`, Go 1.24+); ≤ 10,000
   entries; SHA-256 matches; `schemaVersion` ≤ the binary's (`409 backup_newer`: "This backup is from isshoni 0.5.0;
   install 0.5.0 or newer first"); `PRAGMA integrity_check` on the extracted DB (opened read-only). A bare DB file
   gets the schema and integrity checks only.
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

CLI: `isshoni admin restore PATH` (or `-` for stdin: `docker compose exec -T isshoni isshoni admin restore - <
backup.tar.gz`) shows the manifest, asks for confirmation, streams the file, then waits (≤ 60 s) for
`/v1/health` to answer again and prints the result.

### 12.5 Setup URL, users, invites

- `setup-url` works only while no admin exists (`409 admin_exists` otherwise, with the fix "use `isshoni admin users
  reset-password <name>`"). Each call mints a **new** token and cancels earlier unused setup tokens, so only the most
  recently printed link works (no stale links in scrollback). Token rules (hashed, single-use, 24 h, fragment) are
  03's.
- `reset-password` prints a one-time link (24 h, single use); using it revokes all of that user's sessions and devices
  (plan: "A password reset revokes everything"). It is the recovery path for a forgotten admin password.
- `set-role`/`disable` refuse to remove the last active admin (`409 last_admin`).
- Deleting users is only offered in the admin UI (fewer destructive CLI verbs).

### 12.6 Offline commands

`isshoni admin backup --offline` and `isshoni admin restore --offline PATH` act directly on `data_dir`, for when the
server can't start at all (for example invalid config):
- They refuse if the admin socket answers (`exit 7`: "the server is running; drop --offline").
- They refuse to run as root outside a container (`exit 7`: "run as the service user: `sudo -u isshoni isshoni admin
  restore --offline PATH`"), which keeps root from creating DB files.
- Docker: `docker compose stop`, then `docker compose run --rm isshoni admin restore --offline
  /var/lib/isshoni/backups/x.tar.gz` (or a `pre-*.db` file), then `docker compose up -d`.
- They are the recovery path when the server refuses to start (§6.3). An offline restore doesn't re-exec anything;
  the operator starts the service afterwards.

---

## 13. `doctor` (`internal/server/ops/doctor`)

### 13.1 Modes

- **With a running server** (admin socket answers): the CLI calls `POST /v1/doctor`, so every check runs inside the
  server's process and namespace (it can read `/proc`, DMI and its own TLS state), then adds nothing else. Port checks
  are reported from the server's bound listeners.
- **Without a server**: the CLI runs the same checks itself; checks that need live state (TLS status, transfer,
  release) are `skip`; `schema` opens the DB read-only (so a refusal to start is explained); port checks try to bind
  each port. install.sh runs `isshoni doctor --config /etc/isshoni/isshoni.toml --only dns,public_ip,clock` this way
  before the first start (06 §4.8).
- `--list-checks [--json]` prints the check ids of §13.2 (06's site has one troubleshooting anchor per id).
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
	Now       func() time.Time
	GOOS      string
	UID       int
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
| `schema` | DB schema vs binary | – | newer than binary (the server refuses to start; the fix names 03's backup) |
| `public_ip` | Detection result, method and NAT kind (§7.4) | `port_forward` ("forward TCP 80, 443, 7882 and UDP 7882 to {local}"); IPv6 missing (`info`) | `symmetric`, `cgnat_likely`, or no public IPv4/IPv6 |
| `dns` | `auto`/`manual` with domain: A/AAAA via the system resolver vs public IPs; CAA allows `letsencrypt.org` | AAAA present but not this server | no A/AAAA, or A points elsewhere |
| `tls` | Mode, names, issuer, `not_after`, next renewal, last ACME error code (§8.7) | renewal failing but cert valid; manual cert < 14 days | no cert after 10 min; expired; key mismatch |
| `clock` | Linux `adjtimex` `STA_UNSYNC` flag (no network); in `auto`/`ip`, skew vs the `Date` header of the ACME directory | not synchronized, or skew 30 s–5 min | skew > 5 min |
| `udp_buffers` | Linux: `net.core.rmem_max`/`wmem_max` ≥ `udp_buffer_bytes`; effective SO_RCVBUF on the media sockets | below target (fix: `sysctl -w net.core.rmem_max=8388608 net.core.wmem_max=8388608`, and in Docker "on the Docker host") | – |
| `ports` | **Local only**: isshoni listening on 443/tcp, 80/tcp, 7882/udp, 7882/tcp (or, offline, whether they can be bound). The text always says: "This only checks this machine. The browser connection test checks from outside." | a port disabled by config | a port in use by another program (offline) |
| `firewall_hint` | `info`: provider-specific text to open 80/tcp, 443/tcp, 7882/udp, 7882/tcp (§13.3); Docker bridge note that published ports bypass ufw | – | – |
| `container` | Docker/Podman detected, network mode (bridge vs host), port numbers must match (§7.1) | – | – |
| `lan_privacy` | macOS hosts only (`info`): browsers without Local Network permission can't reach LAN ICE candidates (S4 finding 6) | – | – |
| `transfer` | Month-to-date vs `limits.transfer_alert_gb` | ≥ 80 % | – (never blocks) |
| `release` | Running vs latest release | newer release, or security release | – |
| `nofile` | `info`: open-file limit | < 8192 | – |
| `bandwidth` | `info`: calculator output (§13.4) and NIC speed from `/sys/class/net/<if>/speed` | estimate > 80 % of NIC speed | – |

Text output (abbreviated):

```
isshoni doctor · 0.3.0 · server running · 2026-09-29 20:15 UTC
[ ok ] config        /etc/isshoni/isshoni.toml
[ ok ] public_ip     203.0.113.7 (on interface eth0, confirmed by STUN)
[ ok ] tls           ip certificate for 203.0.113.7, valid until 2026-10-05 10:00 UTC, renews 2026-10-02
[warn] udp_buffers   net.core.rmem_max is 212992, isshoni wants 8388608
       fix: sudo sysctl -w net.core.rmem_max=8388608 net.core.wmem_max=8388608
[info] ports         listening on 443/tcp, 80/tcp, 7882/udp, 7882/tcp (local check only)
[info] firewall_hint Hetzner: Cloud Console → Firewalls → allow TCP 80, 443, 7882 and UDP 7882
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
the same content.

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
     "fix": "sudo sysctl -w net.core.rmem_max=8388608 net.core.wmem_max=8388608",
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
- `SendTest(ctx, subs)`: queues a `push.test` payload to the given subscriptions.

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

type Options struct {
	Enabled      bool                    // push.enabled
	VAPID        func() config.VAPIDKeys
	Subject      string
	Store        Store
	Sender       Sender        // real: webpush-go with the guarded client (§14.5)
	Workers      int           // 4
	QueueLen     int           // 1024
	Now          func() time.Time
	Logger       *slog.Logger
	allowPrivate bool          // tests only (set through an internal test hook)
}

type Service struct{ /* … */ }

func New(opts Options) (*Service, error)
func (s *Service) Run(ctx context.Context) error          // workers + daily prune; at start: VAPID fingerprint check (§5.2)
func (s *Service) ShareStarted(ev ShareStarted)           // non-blocking; drops when the queue is full
func (s *Service) AdminAlert(a AdminAlert)                // non-blocking
func (s *Service) VAPIDPublicKey() string                 // 03's httpapi.Push
func (s *Service) ValidateEndpoint(ctx context.Context, endpoint string) error
func (s *Service) SendTest(ctx context.Context, subs []Subscription) error

type Sender interface {
	Send(ctx context.Context, sub Subscription, payload []byte, o SendOptions) (SendResult, error)
}
type SendOptions struct {
	TTL     time.Duration
	Urgency string // "very-low" | "low" | "normal" | "high"
	Topic   string
}
type SendResult struct {
	Status     int
	RetryAfter time.Duration
}
```

### 14.4 Dedup and rate limits

- `share.started`: at most once per (room, sharer) per **10 minutes**, whatever the number of connections the sharer
  has (web plus desktop). A share that resumes within 01's 30 s grace period is the same share and sends nothing.
- Per recipient: token bucket, burst 3, refill 1 per 10 minutes; excess is dropped and counted (`dropped`).
- `Topic` header per (room, sharer): `s` + first 22 chars of base64url(SHA-256(roomID + "/" + userID)) (≤ 32
  URL-safe chars), so a phone that was offline receives only the latest notification per sharer.
- `admin.alert`: 03 already coalesces `signup_pending` (one per 10 min); transfer alerts fire once per threshold per
  month. Re-sends after a server restart are also covered by the `server.shutdown` rule of 01 (a re-published share
  carries `replaces`, so it never triggers `share.started`).
- State is in memory (a restart can repeat one notification; acceptable).

### 14.5 Sending and the SSRF guard

The endpoint URL comes from a browser, so the server treats it as untrusted.

At subscribe time (`ValidateEndpoint`, called by 03's handler; failures are `push_endpoint_rejected` with
`params.reason`):
- `https` scheme only; port 443 only (explicit or implicit); no userinfo; length ≤ 2048; host must be a DNS name, not
  an IP literal; `keys.p256dh` decodes to 65 bytes and `keys.auth` to 16.
- The host must currently resolve to at least one allowed address (below).

At send time (the real guard, because DNS can change):
- A dedicated `http.Client` whose `net.Dialer.Control` rejects the **actual IP being dialed** unless it is a public
  unicast address. Rejected: unspecified, loopback, private (RFC 1918), CGNAT `100.64.0.0/10`, link-local (incl.
  `169.254.169.254` metadata), ULA `fc00::/7`, multicast, `0.0.0.0/8`, `192.0.0.0/24`, `198.18.0.0/15`, documentation
  ranges, IPv4-mapped/NAT64 forms of those, and the server's own public addresses. Checking in `Control` also defeats
  DNS rebinding between check and connect.
- `Proxy: nil` (environment proxies ignored), `CheckRedirect` returns `http.ErrUseLastResponse` (no redirects),
  timeout 10 s, response body read ≤ 4 KiB.
- `webpush.Options{HTTPClient: guarded, Subscriber: subject, VAPIDPublicKey, VAPIDPrivateKey, TTL, Urgency, Topic}`.

Results:

| Response | Action |
|---|---|
| 201/200/202 | `RecordPushResult(ok)`: `last_success_at = now`, `failures = 0` |
| 404, 410 | delete the subscription (gone) |
| 401, 403 | delete (the subscription belongs to another VAPID key, e.g. after rotation) |
| 400, 413 | log a warning (our bug), `failures++` |
| 429, 5xx, network error | retry at 5 s and 30 s (or `Retry-After` ≤ 60 s) while still within the TTL, then `failures++` |

Queue: 1024 jobs, 4 workers; a full queue drops the job (`dropped`).

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
	Pref           string // "share_started" | "admin_alerts" | ""
	SessionID      string // push.test
	SubscriptionIDs []string
}

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
  -X github.com/MoonWX/isshoni/internal/version.date={{.Date}}"
```

- Without ldflags (`go run`, `go install …@v0.3.0`), `debug.ReadBuildInfo` supplies `Main.Version` and `vcs.revision`
  / `vcs.modified`; otherwise `0.0.0-dev+<short commit>[-dirty]`.

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

`isshoni version`: `isshoni 0.3.0 (commit 1a2b3c4, built 2026-09-29, go1.26.5, linux/amd64, protocol 1, schema 7)`;
`--short` prints `0.3.0` (install.sh compares it); `--json` prints `BuildInfo` (camelCase keys). The SPA gets the same
string at build time (06 passes `ISSHONI_VERSION` to Vite; §9.5 checks it). 06's goreleaser config and `task build`
use exactly the ldflags above; there is no separate `buildinfo` package.

---

## 16. Privacy facts for the project site (06)

What this part of the server sends to third parties, all documented on the privacy page:

| Destination | When | What | Off switch |
|---|---|---|---|
| STUN (Cloudflare, Google) | Startup and every 10 min | One UDP Binding request (reveals the server IP) | set `public_ip` (then one NAT check at startup only) |
| Let's Encrypt | Issuance, renewal, clock check | ACME protocol; optional email | `tls.mode=manual` or `off` |
| GitHub API | Daily | One GET with `User-Agent: isshoni/<ver>` | `updates.release_check=false` |
| Browser push services (Google, Apple, Mozilla, Microsoft) | On notifications | Encrypted payload (RFC 8291); the service sees timing and size (padded) only | `push.enabled=false`, or users don't subscribe |

Nothing else leaves the server. Logs contain no tokens, SDP, push endpoints or usernames.

---

## 17. Test plan

### Unit (`go test -race ./...`; time-dependent code uses `testing/synctest`)

| Package | Asserted |
|---|---|
| `config` | Precedence flag > env > file > default for every kind; policy `IsSet`; unknown file key → error with line/column and suggestion; unknown env → warning; list/duration/bool parsing; derived TLS mode; `off`-mode default of `listen.http`; each §4.5 rule produces the right key, source and fix; `config example` output re-parses to the defaults; registry has no duplicate env/flag names |
| `config` secrets | First start creates 0600 with all keys; restart keeps them; missing registered key is added; wide mode fixed; wrong owner errors; corrupt → `secrets_corrupt`; atomic write leaves the old file on a simulated failure; `Rotate` changes only the chosen keys and runs hooks in order |
| `netx` portmux | TLS ClientHello → `TLS()` and a full handshake (HTTP/1.1 and h2 ALPN); RFC 4571 STUN frame → `ICE()` with the byte replayed; `GET ` → 400 hint; `0xFF` → closed; silent client closed at the timeout (shortened in tests); 33rd pending conn per IP closed; per-IP open limit; `Close` unblocks both `Accept`s; `LocalAddr` is `*net.TCPAddr`; goleak clean |
| `netx` transport | Counting `PacketConn` keeps `AddrPortReaderWriter`; byte counts match; port 0 reuse; interface filter globs; buffer read-back |
| `netx` public IP | Fake STUN (pion/stun) + fake interfaces: each row of §7.4's table; timeouts; literal config skips STUN for the result |
| `netx` rewrite | Rules/filters for each row of §7.5 (direct, 1:1 NAT, Docker bridge, home LAN Append, IPv6 literal) |
| `tlsmgr` | Manual: load, reload on file change and SIGHUP, bad new pair keeps the old, key mismatch, expiry warning; `HTTPHandler`: ACME path passthrough, 503 while not ready, 308 to config host (ignores request Host), off-mode passthrough; ACME problem → hint code table |
| `httpapi` | Middleware order; security headers per response type; HSTS only with domain+TLS; host check incl. dev localhost; trusted-proxy XFF (right-most untrusted) and ignoring XFF in non-off modes; error envelope; `DecodeJSON` limits; SPA table of §9.5 (asset 404, dotted path 404, deep link → index 200, `/setup` → 404 via hook, ETag/304, `.br` negotiation, MIME types without `/etc/mime.types`) |
| `logx` | `Secret` redacted in text and JSON handlers, all fmt verbs, `json.Marshal`; `ReplaceAttr` key list; pion bridge drops SDP-like lines and rate-limits; format auto-detection |
| `ops` | Health state machine (starting → ready → shutting_down); conntest handler (transport validation, `transport_disabled` in off mode, per-user rate limit, `sfu.probe_limit` → 429) with a fake `Prober`; transfer flush, month rollover at UTC midnight, 80/100 % alerts once; release parser (drafts, prereleases, semver order, security marker, ETag 304); dashboard JSON golden file |
| `ops` admin | Peer-cred filter (injected creds); every endpoint via `httptest` over a real unix socket (short path under `/tmp`: macOS `sun_path` is 104 bytes); backup tar layout and manifest; restore validation rejects traversal, symlinks, extra names, bad hashes, newer schema; swap crash recovery from each `plan.json` state; old WAL never next to the new DB |
| `doctor` | Each check with fake FS/resolver/STUN/clock/DMI; provider table; bandwidth numbers (plan example 10.53 Mbps/viewer and 105 Mbps; exit scenario 41.5 Mbps); JSON golden; text render; exit codes 0/5 and `--strict` |
| `push` | `ValidateEndpoint` table (http, :8443, userinfo, IP literal, localhost, 10/8, [::1], fd00::/8, 169.254.169.254, 100.64/10, name resolving private → `push_endpoint_rejected` with the right `reason`); `Control` blocks a rebinding resolver; no redirects followed; recipient filter passed to the store (sharer and `PresentUserIDs` excluded, `Pref`); `admin.alert` payload per 03 alert kind; VAPID fingerprint change deletes all subscriptions at start; dedup window; per-recipient bucket; 404/410/401/403 prune; 429 retry with `Retry-After`; payload < 1 KB; round trip through a fake push service that **decrypts** `aes128gcm` with the test subscription's private key and checks the VAPID JWT (`aud`, `exp`, `sub`) |
| `version` | Build with `-ldflags -X …` in a test and check `isshoni version --json`; BuildInfo fallback |
| `cmd/isshoni` | `testscript` (rogpeppe/go-internal, test-only): every subcommand's usage, exit codes (incl. healthcheck 0/1 only), `--json` shapes, confirmation prompts and `--yes`, container `--out` requirement |

### Integration (in-process, `servertest`)

- Boot (`tls.mode=off`, ephemeral ports, loopback candidates) → ready in < 1 s; `/healthz`, `/readyz`, SPA and API
  headers.
- **TLS path through the 443 mux**: `servertest.Options{TLS: true}` (a private test CA, manual mode) → WSS signaling
  over HTTP/1.1 and SPA over h2 on the same port.
- **ICE-TCP via 443**: `listen.ice_udp=""`; a Pion client restricted to `tcp4` joins, a fake-engine publisher shares,
  media arrives through the 443 multiplexer; the same via 7882/tcp.
- **Graceful shutdown**: connected clients receive `server.shutdown`, `error{server_shutdown}` and close code 1012
  within 2 s; REST gets 503 `server_shutdown`; `Run` returns within `shutdown_timeout`; goleak clean.
- **Refusing to start**: seed a DB with a newer schema → `serve` exits 78 and prints 03's message with the backup
  name; `admin restore --offline <pre-*.db>` → the next `serve` starts. A container without a data mount (fake
  mountinfo) → exit 78.
- **Backup/restore round trip**: create users and a push subscription → backup → mutate → restore → state equals the
  backup; sessions from the backup still work.
- **rotate-secrets**: the server restarts (tests call `Run` again instead of `exec`); web sessions invalid, invites
  invalid, push rows gone only with `--include-vapid`.
- **Wiring** (§6.6): every adapter has a table test; in-process, `logout-everywhere` closes that user's WebSocket
  within 100 ms, and an admin settings change of `maxShareBitrateKbps` reaches `SFU.SetLimits`.
- **ACME** (CI job with Pebble + challtestsrv as service containers; Pebble listens with `httpPort`/`tlsPort` matching
  our test listeners): `auto` for a test domain and `ip` for `127.0.0.1`-mapped IP identifiers with the `shortlived`
  profile; a no-SNI handshake gets the IP certificate; renewal fires under a shortened lifetime.
- **Log canary**: run the full flow at `debug`, capture logs, assert that no token, SDP line (`a=`), `candidate:` or
  push endpoint appears.

### End-to-end (Playwright harness from 05; assertions owned here)

- TCP-only viewer: server with `listen.ice_udp=""`; Chrome viewer plays (`framesDecoded > 0`) and the selected
  candidate pair is TCP.
- Connection test: wizard step 2 reports UDP ✓, TCP 443 ✓ (TLS test mode), TCP 7882 ✓ on localhost; with UDP disabled
  it reports UDP ✗ and shows the provider fix text.

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
  consts `KeySession|KeyInvite|KeyResume`, `RotateResult`, `VAPIDKeys`; the key registry in `keys.go` (other docs add
  keys there); env/flag naming rule; policy keys `registration.mode`, `clients.min_version`,
  `limits.max_participants_per_room`, `limits.max_shares_per_room`, `limits.max_bitrate_kbps`,
  `limits.transfer_alert_gb`, `updates.release_check` with their 03 settings fields (§4.3); guard
  `limits.ws_handshakes_per_ip_per_minute`; `sfu.pause_unwatched_layers`.
- `internal/server/netx`: `Transport` (`UDPMux`, `TCPMux`, `TCPMux443`, `TCPMux7882`, `Advertised`, `RcvBuf`,
  `SndBuf`, `Apply(*webrtc.SettingEngine) error`, `Close`), `NewTransport`, `TransportOptions` (incl. `PacketConns`
  for tests), `AdvertisedAddr`, `PublicAddrs`, `NATKind`, `DetectPublicAddrs`, `STUNClient`, `Resolver`, `PortMux`,
  `TransferCounter`, `Path` consts.
- `internal/server/httpapi` (router part): `Router` (`Handle`, `HandleFunc`, `Handler`), `RouterOptions` (`SPAStatus`,
  `API`, `WS`), `ClientIP`, `IsSecure`, `RequestID`, `WriteJSON`, `WriteError(w, r, err)`, `DecodeJSON(w, r, dst,
  maxBytes)`, `Gate`; the global middleware order of §9.3.
- `internal/server/ops`: `Health` (`AddCheck`, …), `Metrics.Registerer()`, `LiveSource`, `AccountsSource`, `Prober`,
  the dashboard and conntest handlers, admin socket API (§12.2) and client (`ops.DialAdmin(path) *AdminClient`).
- `internal/server/ops/doctor`: `Run`, `Bandwidth`, `RenderText`, `CheckIDs`, `Env`.
- `internal/server/push`: `Service` (`ShareStarted`, `AdminAlert`, `VAPIDPublicKey`, `ValidateEndpoint`, `SendTest`,
  `Run`), `ShareStarted`, `AdminAlert`, `Store`, `RecipientFilter`, `Sender`, `Subscription`.
- `internal/server`: `Server`, `New`, `Run`, `Shutdown`, `ShutdownReason`, `ErrRestartRequested`, `Deps`, and the
  wiring table of §6.6.
- `internal/server/servertest`: `Start(t testing.TB, opts Options) *Server` with fields `URL`, `WSURL`, `Client`
  (`*http.Client`, trusts the test CA when `Options.TLS`), `AdminSocket`, `UDPPort`, `TCPPort`, `DataDir`, `Cfg`,
  `Srv`; methods `Restart(t)`, `Stop(t)`; `Options{TLS bool; Config func(*config.Config); Deps server.Deps}`.
- `internal/protocol/api` (TS via tygo, next to 03's DTOs): `OpsDashboard` and its parts (`ServerInfo`,
  `ProcessInfo`, `TLSInfo`, `AdvertisedAddr`, `UpdateInfo`, `TransferInfo`, `MediaTotals`, `RoomLive`,
  `ParticipantLive`, `ConnectionLive`, `ShareLive`, `LayerLive`, `ViewerCounts`, `ClientVersionCount`,
  `DoctorSummary`, `Alert`), `ServerStatus`, `DoctorReport`, `DoctorCheck`, `DoctorStatus`, `DoctorEnv`,
  `BandwidthInput`, `BandwidthEstimate`, `PushPayload`, `ConnTestRequest`, `ConnTestResponse`, `ConnTestServerInfo`;
  the error codes added to 03's table (§9.4).
- `web` package: `web.Dist() fs.FS`.
- HTTP: `/healthz`, `/readyz`, `/api/v1/conntest`, `/api/v1/admin/dashboard`, `/api/v1/admin/doctor`,
  `/api/v1/admin/bandwidth`; push payload types `share.started`, `admin.alert`, `push.test`.
- CLI and ops contract for 06: subcommands and flags (§3.1: `config init`, `version --short`, `doctor --only
  --list-checks`, `healthcheck --ready --wait`, `setup-url --qr --wait --json`, `admin backup --out -`, `admin restore
  PATH|-` incl. `.db` files and `--offline`), exit codes (§3.2: 78 for "restart can't help"; healthcheck 0/1; setup-url
  4 unreachable, 7 admin exists; doctor 0/5), env names (§4.2, §4.3 env-only switches), `SIGHUP` reload, re-exec after
  restore and rotation, no `sd_notify` (unit `Type=exec`), Docker needs `/run/isshoni` writable by uid 65532 (tmpfs
  with a read-only root) and a data volume at `/var/lib/isshoni`, the release-note marker `<!-- isshoni:security -->`.

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
- `sfu.New(Config{Transport, …})` calls `Transport.Apply` on each `SettingEngine` and sets the 10 s DTLS timeout;
  `SFU.Ready()`; `SFU.Snapshot()` and `Metrics()` (per-share and per-layer bitrate, resolution, fps, loss, viewers per
  layer, ingress/egress, selected transport per PeerConnection) for `LiveSource` and the metrics adapter;
  `SFU.Probe` for `/api/v1/conntest`; `SetLimits`; `Close()` for shutdown.

**03-accounts-and-store**
- `store.Open` with pre-migration backups; `*store.SchemaTooNewError{DBVersion, BinaryVersion, LastAppVersion,
  Backup}` (message printed, exit 78); migration and corruption errors distinguishable by type; `(*DB).BackupTo`,
  `Close` with WAL checkpoint, `Ping` for readiness, `QuickCheck` and `Stats` for doctor; `meta` get/set; the tables
  `push_subscriptions`, `push_preferences`, `transfer_months` and their `*Q` methods (03 §6).
- Auth service calls used by the admin socket: `SetupAvailable`, `IssueSetupToken`, `UserByUsername`,
  `IssuePasswordReset`, `UpdateUser` (role, status; last-admin guard), `CreateInvite`, `db.Read → ListUsers` (03 §12.6).
- `httpapi.API` (the /api/v1 chain, `Handle`, `PrincipalFrom`, `DashboardAccounts`), `api.Error` and `api.StatusOf`;
  `SettingsCache.Pin`, `Get`, `OnChange`; the `SPAStatus` hook (`/setup` → 404 after setup); `auth.ConnCloser`,
  `auth.AdminAlerter`, `httpapi.Signal`, `httpapi.Push`, `httpapi.InfoSource` for the wiring.
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
- ldflags (§15); `go.mod` `ignore ./web/node_modules`; `web/dist/.gitkeep`.
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
- Release template marker `<!-- isshoni:security -->`; privacy page facts of §16; config reference from the registry
  (`isshoni config example`), hand-written in M1.

---

## 20. Implementation slices

Ordered; each ends green in CI and is testable alone. The integrated plan (`README.md`, ids `Sxx`) sequences them
with the other docs and adds the wiring slices of §6.6.

| # | Slice | Scope | Acceptance | Depends on |
|---|---|---|---|---|
| S1 | CLI skeleton, version, logx | `cmd/isshoni` dispatch, help, exit codes (incl. 78); `internal/version` (+ `DocsURL`, `--short`); `internal/logx` (Secret, formats, ReplaceAttr) | `go build -ldflags -X…` → `isshoni version --json` shows the values and `--short` prints the version; logx unit tests; testscript for usage and exit 2 | – |
| S2 | Config | Registry, TOML/env/flag loading, precedence, validation messages, `config check/print/example/init`, `Site` | All §17 config tests; `config example` round-trips; `config init` refuses to overwrite; errors show file:line and a fix | S1 |
| S3 | Data dir and secrets | Layout checks, umask, container volume check (exit 78, escape env), `secrets.json` create/load/rotate | Secrets tests; container check with a fake mountinfo | S2 |
| S4 | HTTP skeleton and SPA | `internal/server` with `off` mode, router, global middleware chain, JSON/error helpers on 03's `api.Error`, SPA handler, security headers, `/healthz` `/readyz`, graceful shutdown skeleton, `servertest` | httpapi tests; `servertest.Start` ready < 1 s; built SPA served with correct caching | S2, S3, 03 DTOs |
| S5 | Public IP and ICE transports | `DetectPublicAddrs`, rewrite rules, UDP mux with counting conns, 7882/tcp mux, `Transport` (incl. `TCPMux443/7882`, `PacketConns`), `Transport.Apply`, `TransferCounter` | netx unit tests; a Pion PeerConnection pair connects through `Transport` over UDP and over TCP 7882 on loopback | S1 |
| S6 | 443 multiplexer | `PortMux`, `prefixConn`, limits, ICE sub-listener into `TCPMuxDefault` | portmux tests (TLS, RFC 4571, plain HTTP hint, garbage, slow client, limits, Close); goleak clean | S5 |
| S7 | TLS manager | certmagic `auto`/`ip`, `manual` reload, port 80 handler, HSTS, zap bridge, hints; `servertest` TLS option with a test CA | Pebble CI job issues domain and IP certs; WSS + h2 + ICE-TCP on one port in `servertest`; manual reload tests; **a Let's Encrypt staging IP certificate issued on a real VPS** | S4, S6 |
| S8 | Admin socket basics | Socket server/client, peer creds, `health`/`ready`/`status`, `healthcheck`, `setup-url` (+QR, `--wait`), `users`, `invite create`, `log-level` | testscript CLI tests; healthcheck exits 0/1 only; setup-url exits 7 after setup (fake auth) | S4 |
| S9 | Backup, restore, rotation | Backup tar, restore of archives and `.db` files with validate/swap/re-exec and crash recovery, offline mode, `rotate-secrets` with restart | Round-trip and crash-recovery tests; a newer-schema DB → `serve` exits 78 → offline restore of the pre-migration DB → serving | S8, 03 store API |
| W1 | Wiring v1 | §6.6 adapters for 01's hub (fake media) and 03's store/auth/httpapi; settings pins | `serve` in off mode: login over REST, `/ws` joins Lounge; logout-everywhere closes the socket in < 100 ms | S4, S8, 01 P5, 03 sessions/setup |
| W2 | Wiring v2 | SFU + `sfuplane` + Transport + PortMux in `serve`; metrics adapter | 01 P8's integration tests pass against `servertest` | W1, S7, 01 P8, 02 slice 7 |
| S10 | Ops data | Transfer accounting and alerts, metrics endpoint, release check (dashboard: S10b) | metrics scrape test; month rollover and alert-once tests; release parser tests | S5, S8 |
| S10b | Dashboard | `/api/v1/admin/dashboard` with `LiveSource`/`AccountsSource` adapters | Golden dashboard JSON with fake sources; a failing source doesn't fail the response | S10, W2 |
| S11 | doctor | All checks, `--only`, `--list-checks`, provider table, bandwidth calculator, text/JSON output, `/api/v1/admin/doctor`, `/api/v1/admin/bandwidth`, periodic run | doctor unit tests; numbers of §13.4; `doctor --json` on a VPS all ok | S7, S8, S10 |
| S12 | Web Push | VAPID, `httpapi.Push` methods, guarded sender, triggers, dedup, limits, pruning, VAPID fingerprint check | push unit tests incl. decrypting fake push service; manual iPhone and Android notification | S3, 03 tables |
| S12b | Push wiring | push as `PushNotifier`, `AdminAlerter`, `httpapi.Push` | a share going live in `servertest` reaches the fake push service for an absent user only | S12, W2 |
| S13 | Connection test | `/api/v1/conntest` handler on 02's `Probe` | Playwright wizard step 2 shows UDP/TCP443/TCP7882 ✓ locally, UDP ✗ when disabled | S7, 02 slice 14 |
| S14 | Hardening | SIGHUP, full shutdown with 01's notice, log canary, TCP-only e2e, `doctor` in Docker bridge and host modes | All integration and e2e tests of §17 pass; manual 2-hour session checklist | all above |

## Decisions taken at integration (formerly open questions)

1. **Transfer allowance in the wizard**: not in M1. The wizard keeps the plan's three steps; admins set
   `transferAlertGb` under Admin → Settings, and the dashboard always shows month-to-date transfer.
2. **Lock-screen content of "X started streaming"**: shows the sharer's name and the room (the plan's own example
   "Alex started streaming"); each user can switch these notifications off (`shareStarted: off`).
3. **Docker without a data volume**: a hard refusal (exit 78) with the exact fix, plus `ISSHONI_ALLOW_EPHEMERAL_DATA=1`
   for tests. Both 04 and 06 recommended it; it is a stricter form of the plan's "doctor fails".
