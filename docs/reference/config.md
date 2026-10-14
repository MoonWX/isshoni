# Configuration

isshoni reads its configuration from `/etc/isshoni/isshoni.toml` (another file with `--config PATH` or
`ISSHONI_CONFIG`), from `ISSHONI_*` environment variables and from command-line flags. The install script writes the
file once, with `isshoni config init`; Docker uses the environment.

- **Precedence**: flag, then environment variable, then the file, then (for the policy keys below) the admin page's
  setting, then the default.
- **Names**: the key `tls.mode` is `[tls] mode = "auto"` in the file, `ISSHONI_TLS_MODE` in the environment and
  `--tls.mode` as a flag. A top-level key like `public_ip` is `ISSHONI_PUBLIC_IP` and `--public-ip`.
- **Values**: lists are comma-separated in the environment and in flags
  (`ISSHONI_NETWORK_TRUSTED_PROXIES=10.0.0.0/8,172.16.0.0/12`), durations look like `10s` or `168h`, booleans are
  `true`, `false`, `1` or `0`. **An empty environment variable counts as unset.**
- **Changes need a restart**: `sudo systemctl restart isshoni` (Docker: `docker compose up -d`). Friends' apps
  reconnect by themselves.
- **Checking**: `sudo isshoni config check` validates the configuration and lists every problem with its fix;
  `sudo isshoni config print` shows each value and where it comes from. An unknown key in the file is an error, with
  a suggestion when it looks like a typo.

`isshoni config example` prints a commented file with every key, and the values you pass as flags set; the start of
`isshoni config example --domain share.example.com`:

```toml
# isshoni server configuration. Every key also has an ISSHONI_* env variable and a --flag.
# Reference: https://moonwx.github.io/isshoni/reference/config
#
# A line that starts with # shows the default (or an example) and changes nothing. Remove the # to set
# a value. Changes need a restart (sudo systemctl restart isshoni).

# The DNS name friends use. Empty: use the public IP.
# Empty by default.
domain = "share.example.com"

# "auto" (from the interfaces, then STUN) or this server's public IPv4 or IPv6 address.
# public_ip = "auto"
```

## Top level {#top}

| Key | Default | Meaning |
|---|---|---|
| `domain` | `""` | The DNS name friends use. Empty: use the public IP address |
| `public_ip` | `"auto"` | `"auto"` (from the network interfaces, then STUN) or this server's public IPv4 or IPv6 address. Docker: `ISSHONI_PUBLIC_IP` |
| `public_ipv6` | `"auto"` | `"auto"`, `"off"` or a public IPv6 address: extra IPv6 media candidates |
| `public_url` | `""` | The URL friends open when `tls.mode = "off"` (behind your own HTTPS proxy). Required in that mode, unless `listen.http` is a loopback address (development) |
| `data_dir` | `"/var/lib/isshoni"` | The data directory: database, keys, certificates and backups |
| `shutdown_timeout` | `"10s"` | The upper bound for a graceful shutdown |

## `[listen]` {#listen}

| Key | Default | Meaning |
|---|---|---|
| `listen.https` | `":443"` | HTTPS and the signaling WebSocket, and media over TCP on the same port. Ignored when `tls.mode = "off"` |
| `listen.http` | `":80"`; `"127.0.0.1:8080"` when `tls.mode = "off"` | Let's Encrypt's HTTP challenge and the redirect to HTTPS; the app itself when `tls.mode = "off"` |
| `listen.ice_udp` | `":7882"` | The UDP port for all media. `""` turns UDP off |
| `listen.ice_tcp` | `":7882"` | The TCP port for media (needed when `tls.mode = "off"`). `""` turns it off |
| `listen.admin_socket` | `"/run/isshoni/admin.sock"` | The socket the `isshoni` commands use to reach the running server |

## `[tls]` {#tls}

| Key | Default | Meaning |
|---|---|---|
| `tls.mode` | `auto` when `domain` is set, else `ip` | `auto` (Let's Encrypt for `domain`), `ip` (Let's Encrypt for the public IP address), `manual` (your files) or `off` (your own HTTPS proxy). See [Certificates](/install/tls) |
| `tls.acme_email` | `""` | An optional contact address for the Let's Encrypt account |
| `tls.acme_ca` | `"https://acme-v02.api.letsencrypt.org/directory"` | The ACME directory |
| `tls.acme_staging` | `false` | Use Let's Encrypt's staging service: untrusted certificates, for testing only |
| `tls.acme_ca_root` | `""` | A PEM file trusted for the ACME server's own TLS. Testing only |
| `tls.cert_file` | `""` | The certificate chain (PEM) when `tls.mode = "manual"` |
| `tls.key_file` | `""` | The private key (PEM) when `tls.mode = "manual"` |
| `tls.hsts` | `true` | Send HSTS (only when serving a domain over TLS) |

## `[network]` {#network}

| Key | Default | Meaning |
|---|---|---|
| `network.stun_servers` | `["stun.cloudflare.com:3478", "stun.l.google.com:19302"]` | STUN servers for finding the public IP address only. `[]` turns STUN off: then set `public_ip` |
| `network.ipv6` | `true` | Offer IPv6 media candidates when the server has a global IPv6 address |
| `network.exclude_interfaces` | `["docker*", "br-*", "veth*", "virbr*", "cni*", "flannel*", "cali*", "kube-*"]` | Network interfaces never used for media (name patterns) |
| `network.include_loopback` | `false` | Development only: offer `127.0.0.1` and `::1` media candidates |
| `network.udp_buffer_bytes` | `8388608` | The UDP buffer size asked for on media sockets (1 MiB to 64 MiB); the kernel limits must allow it ([udp_buffers](/troubleshooting#doctor-udp_buffers)) |
| `network.trusted_proxies` | `[]`; `["127.0.0.0/8", "::1/128"]` when `tls.mode = "off"` and `listen.http` is a loopback address | The addresses (CIDRs) allowed to set `X-Forwarded-For` and `X-Forwarded-Proto`, in off mode only ([reverse proxy](/install/reverse-proxy)) |

## Policy keys {#policy}

These are settings on the admin page too. Set here, they win, and the admin page shows them read-only.

| Key | Default | Meaning |
|---|---|---|
| `registration.mode` | `"invite"` | Who can create an account: `invite` (with an invite link), `approval` (anyone, then an admin approves) or `closed` |
| `clients.min_version` | `""` | The oldest app version (SemVer) allowed to connect; older apps show "Update the app". Empty: no minimum |
| `limits.max_participants_per_room` | `0` | A soft limit of people per room. `0`: no limit |
| `limits.max_shares_per_room` | `0` | A soft limit of shares per room. `0`: no limit |
| `limits.max_bitrate_kbps` | `0` | A cap on any share's full-quality bitrate in kbps (500 to 100000). `0`: the quality presets' own caps |
| `limits.transfer_alert_gb` | `0` | Alert admins at 80% and 100% of this monthly transfer (1 GB = 10⁹ bytes). `0`: off |
| `updates.release_check` | `true` | Check GitHub once a day for a newer release and show it on the admin page |

## Limits {#limits}

| Key | Default | Meaning |
|---|---|---|
| `limits.ws_handshakes_per_ip_per_minute` | `20` | Signaling connections before sign-in, per client IP address and minute. Raise it for a LAN party behind one address |
| `limits.conns_per_ip` | `256` | Open TCP connections per client IP address (an IPv4 address or an IPv6 /64) on the HTTPS and HTTP ports |

## `[sfu]`, `[push]`, `[metrics]`, `[log]` {#other}

| Key | Default | Meaning |
|---|---|---|
| `sfu.pause_unwatched_layers` | `true` | Ask sharers' browsers to pause the full-quality layer while nobody watches it |
| `push.enabled` | `true` | Web Push notifications ("Alex started sharing") |
| `push.subject` | `mailto:<tls.acme_email>` if set, else the server's address | The contact sent to push services: a `mailto:` or `https:` URL |
| `metrics.enabled` | `false` | Serve Prometheus metrics on `metrics.listen` |
| `metrics.listen` | `"127.0.0.1:9469"` | The metrics listener. The metrics have no authentication: keep it on loopback |
| `metrics.pprof` | `false` | Also serve Go's profiler at `/debug/pprof/` on the metrics listener |
| `log.level` | `"info"` | `debug`, `info`, `warn` or `error` |
| `log.format` | `"auto"` | `auto` (text on a terminal, else JSON lines), `text` or `json` |

## Environment only {#environment}

These are read from the environment only, and are not keys:

| Variable | Meaning |
|---|---|
| `ISSHONI_CONFIG` | The configuration file's path |
| `ISSHONI_IN_CONTAINER=1` | Set by the Docker image (a container is also detected without it) |
| `ISSHONI_ALLOW_EPHEMERAL_DATA=1` | Tests only: start in a container without a data volume |

The install script's own variables (`ISSHONI_VERSION`, `ISSHONI_YES`, …) are on the [install page](/install/#options).
