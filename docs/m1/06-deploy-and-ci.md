# M1 design 06: Deploy, CI, release and project site

Part of the M1 ("Watch together", server + web) design set. Source of truth: `docs/PLAN.md`. Sibling docs, written in
parallel: `01-protocol.md`, `02-sfu.md`, `03-accounts-and-store.md`, `04-server-platform.md`, `05-web-client.md`.

**Scope marker.** Everything in this document is **M1** unless it is marked **Later (Mx)**. "Later" items are here
only so M1 leaves room for them.

**Integration status.** Reconciled with 01–05 by the integrator (`README.md`, "Integration decisions"). The main
changes: the CLI names, flags and exit codes are 04's (`config init` with 04's flag names, `healthcheck --ready`
exits 0/1, `setup-url` exits 4/7, `doctor` exits 0/5, `admin backup --out`, `admin restore PATH`); the build info
package is 04's `internal/version`; the container data directory is `/var/lib/isshoni` (not `/data`) and
`/run/isshoni` is a tmpfs; product links use 04's provider ids.

**Covers:** `deploy/install.sh`, the systemd unit, sysctl and firewall files, deb/rpm packages, the Docker image and
`compose.yaml`, the contributor workflow (`Taskfile.yml`, tool pins), GitHub Actions CI, the release pipeline and its
signing, the project site, and the run book for the M1 exit test.

**Does not cover:** what the server does at runtime (config keys, TLS, doctor, healthcheck, setup-url: `04`), the SPA
and its e2e test content (`05`), protocol fixtures (`01`), the SFU and load-test tool (`02`), schema and REST (`03`).
Where this doc needs something from them, section 15 lists it precisely.

## 1. Key decisions

| # | Decision | Why |
|---|---|---|
| D1 | `install.sh` is thin. The binary, the systemd unit, the sysctl file and the firewall profiles all come from the **signed release tarball** | Everything that runs as root, except the script itself, is covered by the release signature |
| D2 | Each release ships its own `install.sh`, stamped with its version. The site serves the copy from the **latest release**, re-verified in CI | Script and binary never drift apart; `…/releases/download/vX/install.sh` reproduces an install exactly |
| D3 | One unit file for the script and the packages: `ExecStart=isshoni serve …` resolved by systemd's fixed search path (`/usr/local/bin` before `/usr/bin`) | No second copy of the unit to keep in sync. Works on systemd ≥ 239; the oldest target (Ubuntu 22.04) has 249 |
| D4 | `ssh-keygen -Y sign` runs in a **separate job** behind the `release` Environment (owner approval). The build job never sees the key; the release stays a **draft** until signed and verified | The key is exposed only to a ~20-line job with no third-party build code |
| D5 | Docker binary lives at `/usr/local/bin/isshoni`, not `/isshoni` as the plan says | `docker compose exec isshoni isshoni setup-url` resolves the command through `PATH`; distroless has no `/` on `PATH` |
| D6 | The image contains `/var/lib/isshoni` owned by uid 65532 (the same path as systemd installs), and the server **refuses to start** (exit 78) in a container when it is not a mount (04 §5.1) | A named volume then works with no `chown`, nobody loses their users when the container is recreated, and every doc and command uses one data path |
| D7 | CI has **one required check, `ci-ok`**. Path-filtered jobs run inside the same workflow | GitHub blocks merges on required checks from workflows that were skipped by path filters |
| D8 | install.sh tests: systemd **containers** on PRs (fast), **KVM VMs** nightly and before a release (SELinux, real firewalls, Pebble ACME) | Containers can't test SELinux labels, sysctls or firewalls; VMs are too slow for every PR |
| D9 | Binaries are pinned in `.tool-versions` (mise or asdf). Go tools are pinned in a separate `tools/go.mod` | One pin file, as in the plan; the main `go.mod` stays free of tool dependencies |
| D10 | License policy: an **allowlist** for everything shipped, a **GPL/AGPL denylist** for dev-only npm packages. `THIRD_PARTY_NOTICES` is generated from the exact module set of `cmd/isshoni` | Shipped code must be provably permissive; dev tools only must not be GPL/AGPL |
| D11 | Site: VitePress in `docs/` (excluding `PLAN.md` and `m1/`), on GitHub Pages at `https://moonwx.github.io/isshoni/` | Free, no domain to buy; a custom domain can be added later and GitHub redirects the old URLs |
| D12 | Package manager is **npm** (replaces the plan's pnpm): `web/` and `docs/` each have their own `package-lock.json` | Node 26 no longer ships corepack/pnpm; npm needs no extra tool |

## 2. The deployment contract (ports, paths, names)

Other docs rely on these exact values (see section 14).

**Ports**

| Port | Proto | Purpose | Opened by the installer / published by compose |
|---|---|---|---|
| 80 | TCP | ACME HTTP-01 and redirect to HTTPS | yes (not needed with `tls.mode=off`) |
| 443 | TCP | HTTPS, WSS signaling, ICE-TCP (first byte ≠ `0x16`) | yes (not with `tls.mode=off`) |
| 7882 | UDP | WebRTC media, ICE UDP mux | always |
| 7882 | TCP | ICE-TCP (required with `tls.mode=off`, a second TCP path otherwise) | always |
| 127.0.0.1:9469 | TCP | Prometheus `/metrics`, off by default | never |

**Paths (shell install)**

| Path | Owner:group, mode | Created by | Notes |
|---|---|---|---|
| `/usr/local/bin/isshoni` | root:root 0755 | install.sh | packages use `/usr/bin/isshoni` |
| `/usr/local/lib/systemd/system/isshoni.service` | root:root 0644 | install.sh | packages: `/usr/lib/systemd/system/`. Local changes go in `systemctl edit isshoni` drop-ins, which survive upgrades |
| `/etc/isshoni/` | root:root 0755 | install.sh, `ConfigurationDirectory=` | read-only for the service (`ProtectSystem=strict`) |
| `/etc/isshoni/isshoni.toml` | root:isshoni 0640 | `isshoni config init` (via install.sh), only if missing | never rewritten by the installer |
| `/var/lib/isshoni/` | isshoni:isshoni 0700 | `StateDirectory=` | DB, `secrets.json`, certmagic storage, `backups/` (layout: `03`/`04`) |
| `/run/isshoni/` | isshoni:isshoni 0750 | `RuntimeDirectory=` | admin socket `admin.sock` (`04`) |
| `/etc/sysctl.d/60-isshoni.conf` | root:root 0644 | install.sh, only if the current values are lower | packages: `/usr/lib/sysctl.d/60-isshoni.conf` |
| `/etc/ufw/applications.d/isshoni` | root:root 0644 | install.sh if ufw is installed | ufw application profile |
| `/etc/firewalld/services/isshoni.xml` | root:root 0644 | install.sh if firewalld is installed | packages: `/usr/lib/firewalld/services/` |

**Identities**
- System user and group `isshoni` (system uid/gid, home `/var/lib/isshoni`, shell `nologin`).
- Unit `isshoni.service`.
- Container user `65532:65532` (distroless `nonroot`).
- Container image `ghcr.io/moonwx/isshoni` (lower case: registry names must be).
- Go module `github.com/MoonWX/isshoni`.

## 3. Release artifacts

Every `v*` tag produces these GitHub Release assets. Names are an interface: install.sh, the site and the docs build
URLs from them. `<v>` is the version without the leading `v` (e.g. `0.1.0`).

| Asset | Contents |
|---|---|
| `isshoni_<v>_linux_amd64.tar.gz`, `…_linux_arm64.tar.gz` | `isshoni`, `LICENSE`, `NOTICE`, `THIRD_PARTY_NOTICES`, `README.md`, `deploy/systemd/isshoni.service`, `deploy/sysctl/60-isshoni.conf`, `deploy/firewall/ufw-isshoni`, `deploy/firewall/firewalld-isshoni.xml` |
| `isshoni_<v>_darwin_{amd64,arm64}.tar.gz`, `isshoni_<v>_windows_{amd64,arm64}.zip` | same layout. **Experimental, untested server builds** for LAN/dev use; no installer. Release notes say so |
| `isshoni_<v>_{amd64,arm64}.deb`, `isshoni-<v>-1.{x86_64,aarch64}.rpm` | packages (section 5); file names from nfpm's `ConventionalFileName` |
| `install.sh` | the installer, stamped with `<v>` |
| `compose.yaml`, `compose.host.yaml` | Docker Compose files (section 6) |
| `checksums.txt` | SHA-256 of every archive, package, `install.sh` and compose file (`<hex>  <name>` per line) |
| `checksums.txt.sig` | `ssh-keygen -Y sign` signature, namespace `isshoni-checksums` |
| `checksums.txt.sigstore.json` | cosign v3 keyless bundle for `checksums.txt` |
| `*.sbom.json` | SPDX JSON SBOM per archive (syft) |

Also: images `ghcr.io/moonwx/isshoni:<v>` (multi-arch amd64+arm64), signed with cosign keyless, with a buildx SBOM
attestation, and GitHub build-provenance attestations for every archive, package and image digest.

**Docker tags.**
- `0.x`: `:<v>` (e.g. `0.1.0`), `:0.<minor>` and `:latest`. There is no `:0`, because a 0.x minor may break things.
- From 1.0: `:<v>`, `:<major>.<minor>`, `:<major>` and `:latest`, as in the plan.
- Prereleases (`-rc.N`) get only `:<v>` and never move `:latest`.

## 4. Server install: `deploy/install.sh`

### 4.1 Usage

```
curl -fsSL https://moonwx.github.io/isshoni/install.sh | sh
curl -fsSL https://moonwx.github.io/isshoni/install.sh | sh -s -- --domain share.example.com --yes
curl -fsSL https://github.com/MoonWX/isshoni/releases/download/v0.1.0/install.sh | sh   # exactly 0.1.0
```

Not root: the script prefixes privileged commands with `sudo` (which asks on the terminal). It fails with exit 4 if
`sudo` is missing.

| Flag | Env equivalent | Meaning |
|---|---|---|
| `--domain NAME` | `ISSHONI_DOMAIN` | Certificate for NAME (`tls.mode=auto`); skips the question |
| `--ip` | `ISSHONI_TLS_MODE=ip` | Certificate for the public IP (`tls.mode=ip`); skips the question |
| `--tls-mode off\|manual` | `ISSHONI_TLS_MODE` (also accepts `auto`, `ip`) | Advanced. `off`: behind your own proxy (needs `ISSHONI_PUBLIC_URL`). `manual`: your certificate (`ISSHONI_TLS_CERT_FILE`, `ISSHONI_TLS_KEY_FILE`) |
| `--version V` | `ISSHONI_VERSION` | `0.1.0`, `v0.1.0` or `latest` |
| `--yes`, `-y` | `ISSHONI_YES=1` | Never ask; take every default, which includes opening firewall ports |
| `--no-firewall` | `ISSHONI_NO_FIREWALL=1` | Never touch the firewall |
| `--firewall` | | Offer the firewall step again on an upgrade |
| `--no-start` | | Install only: no start, no certificate, no setup link. Running the installer again later without it does the first start (4.2) |
| `--allow-downgrade` | | Allow an older version (restore a matching backup first; section 4.10) |
| `--reinstall` | | Reinstall the same version (repairs the binary) |
| `--uninstall` | | Remove isshoni and keep its config, data and user |
| `--purge` | | `--uninstall`, then delete config, data and the user |
| `--help` | | Usage |
| | `ISSHONI_PUBLIC_IP` | Public IP, instead of STUN detection (passed to `config init`) |
| | `ISSHONI_TLS_ACME_EMAIL` | Optional ACME account e-mail (04's key `tls.acme_email`) |
| | `ISSHONI_DOWNLOAD_BASE` | Mirror of `https://github.com/MoonWX/isshoni/releases`. **Signatures are still checked against the embedded key** |

Flags win over env. Unknown flags → exit 2. `--tls-mode off` (or `ISSHONI_TLS_MODE=off`) without
`ISSHONI_PUBLIC_URL` → exit 2: "off mode needs ISSHONI_PUBLIC_URL=https://your.domain". The installer never writes
an off-mode config without a public URL.

**Exit codes**

| Code | Meaning |
|---|---|
| 0 | Done, or already up to date |
| 1 | Unexpected error (the message says which step) |
| 2 | Bad usage |
| 3 | **Verification failed.** Nothing was changed |
| 4 | Unsupported system: not Linux, no systemd, unsupported architecture, no root/sudo, installed from a package, or an OpenSSH without `ssh-keygen -Y verify` (before 8.1) |
| 5 | Installed, but the server did not become ready in time (certificate or startup). Diagnostics are printed |
| 6 | Downgrade refused |
| 7 | A required port is used by another program |
| 8 | Cancelled by the user |

### 4.2 Flow

```
main
 ├─ parse_args → preflight
 │    root or sudo · uname -s = Linux · arch ∈ {x86_64|amd64 → amd64, aarch64|arm64 → arm64}
 │    /run/systemd/system exists (else: "use Docker", exit 4)
 │    not package-managed (dpkg -S / rpm -qf /usr/bin/isshoni → "use apt/dnf", exit 4)
 │    curl or wget · ssh-keygen present (else install openssh-client/-clients, see 4.3)
 ├─ --uninstall / --purge → 4.11
 └─ install
      IV := installed version (`/usr/local/bin/isshoni version --short`, empty if none)
      TV := --version > ISSHONI_VERSION > stamped version > latest (4.3)
      STARTED := `systemctl is-enabled --quiet isshoni` succeeds (an earlier run got as far as the start)
      IV > TV, no --allow-downgrade ──► exit 6
      IV = TV, no --reinstall, STARTED ──► repair (unit, sysctl) ─► ensure running ─► wait ready (4.10; timeout
          ─► exit 5) ─► setup-url: link + QR, or "Setup is already done. Open https://host/" ─► exit 0
      IV ≠ TV or --reinstall:
          fetch + verify (4.3) ───────────────────────── fail ─► exit 3 (nothing changed)
          install binary (.new → version check → mv)
      user, dirs, unit, sysctl, firewall profiles (4.4–4.7)
      no config? ─► the one question (4.8) ─► `isshoni config init` ─► doctor pre-check ── cancel ─► exit 8 (4.8)
      --no-start ─► "Installed. Run this installer again to start isshoni." ─► exit 0
      port check (4.9) ──────────────────────────────── conflict ─► exit 7
      not STARTED (first start) ─► firewall offer (4.9) ─► daemon-reload ─► enable --now
      STARTED (upgrade, --reinstall) ─► firewall offer only with --firewall ─► daemon-reload ─► restart
      wait ready (4.10) ─────────────────────────────── timeout ─► diagnostics ─► "Fix the problem above,
                                                          then run this installer again." ─► exit 5
      first start ─► setup link + QR │ upgrade ─► "Upgraded IV → TV" │ admin exists ─► "Open https://host/"
```

Order matters: the firewall step runs **before** the start, so Let's Encrypt can reach ports 80/443 on the first try.
The doctor pre-check runs before the start, so a DNS mistake doesn't burn Let's Encrypt's failed-validation limit.

**Re-runs finish the job.** Whether the first-start tail runs (port check, firewall offer, `enable --now`, ready
wait, setup link) is decided by "unit not enabled", never by "no config" and never by IV. A run that stopped before
the start leaves the unit disabled, so running the installer again, with the same version, picks up where it
stopped:
- after exit 7 (port conflict): the config from the first run is kept, so there is no question; the port check runs
  again, then the firewall offer, the start and the setup link;
- after exit 8 at the pre-check prompt, or after `--no-start`: as above, and after exit 8 the question is asked
  again (that run's config was deleted, 4.8);
- after `--uninstall` (C9): the binary is fetched again, the kept config is used, and the start runs; `setup-url`
  exits 7, so the script prints "Setup is already done. Open https://<host>/".

After exit 5 the unit is already enabled, so a re-run takes the repair branch. That branch also ends with the ready
wait and `setup-url` (4.10), so once the admin has fixed the cause, the re-run prints the setup link.

### 4.3 Download and verification (fail closed)

**Version resolution.**
- The script carries `ISSHONI_INSTALLER_VERSION="@VERSION@"`; the release pipeline replaces `@VERSION@` (section 9.2).
- `latest`, or an unstamped script, resolves through the redirect of `$BASE/latest` (`…/releases/tag/vX.Y.Z`):
  - curl: `-fsSLI -o /dev/null -w '%{url_effective}'`;
  - wget: `-S --spider --max-redirect=0`, then the `Location:` header.
- The API is not used, because it is rate-limited and returns JSON.

**Download.**
- Base is `${ISSHONI_DOWNLOAD_BASE:-https://github.com/MoonWX/isshoni/releases}/download/v$TV/`.
- Files: `isshoni_${TV}_linux_${ARCH}.tar.gz`, `checksums.txt`, `checksums.txt.sig`, and
  `checksums.txt.sigstore.json` only when cosign is installed.
- curl: `--proto '=https' --tlsv1.2 -fsSL --connect-timeout 10 --max-time 300 --retry 3 --retry-delay 2`.
- wget: `--https-only --timeout=10 --tries=3 -q`.
- Files go into `mktemp -d`, which is removed by `trap … EXIT INT TERM`.

**Verification, in this order; any failure → exit 3 with "Nothing was installed or changed":**
1. `ssh-keygen -Y verify -f "$TMP/allowed_signers" -I isshoni-release -n isshoni-checksums -s checksums.txt.sig < checksums.txt`.
   `allowed_signers` is written from the block embedded in the script (below).
2. `expected=$(awk -v f="$ARCHIVE" '$2 == f { print $1 }' checksums.txt)`. The name must match exactly and appear
   once, then it is compared with `sha256sum` (fallback `shasum -a 256`, then `openssl dgst -sha256 -r`).
3. If `cosign` is installed: `cosign verify-blob --bundle checksums.txt.sigstore.json
   --certificate-identity "https://github.com/MoonWX/isshoni/.github/workflows/release.yml@refs/tags/v$TV"
   --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt`.
4. If `gh` is installed **and** `gh auth status` succeeds: `gh attestation verify "$ARCHIVE" --repo MoonWX/isshoni`.
   `gh attestation` needs a token, so the step is skipped silently when `gh` isn't logged in.

**The embedded key block.** It must equal `deploy/keys/allowed_signers` byte for byte; CI checks this (`task lint:keys`).

```sh
# BEGIN ALLOWED SIGNERS (generated from deploy/keys/allowed_signers; do not edit here)
ALLOWED_SIGNERS='isshoni-release namespaces="isshoni-checksums" ssh-ed25519 AAAA… isshoni-release-1
isshoni-release namespaces="isshoni-checksums" ssh-ed25519 AAAA… isshoni-release-backup-1'
# END ALLOWED SIGNERS
```

- **There is no runtime override of the key.** Tests replace the block between the markers with an ephemeral test key
  (section 11). A mirror (`ISSHONI_DOWNLOAD_BASE`) is safe because verification still uses the embedded key.
- **Missing `ssh-keygen`** is common in minimal containers, rare on a VPS, where sshd pulls it in. The script says
  "Installing openssh-client to verify the download" and runs `apt-get install -y --no-install-recommends
  openssh-client` or `dnf install -y openssh-clients`. If that fails → exit 4.
- **OpenSSH older than 8.1** (no `-Y verify`) is detected by a usage error → exit 4. Every supported distro ships
  ≥ 8.9.
- **Honest scope.** The signature protects against tampered release assets, mirrors and download corruption. It does
  **not** protect against a compromised project site serving a different `install.sh`. For that case the site
  publishes the script's SHA-256 and the manual verification steps (section 10.3).

### 4.4 Binary, user and directories

```sh
tar -xzf "$TMP/$ARCHIVE" -C "$TMP/x" --no-same-owner
install -m 0755 -o root -g root "$TMP/x/isshoni" /usr/local/bin/.isshoni.new
[ "$(/usr/local/bin/.isshoni.new version --short)" = "$TV" ] || die 1 "downloaded binary does not run here"
mv -f /usr/local/bin/.isshoni.new /usr/local/bin/isshoni
command -v restorecon >/dev/null 2>&1 && restorecon /usr/local/bin/isshoni
```

- The binary is installed next to its final location and test-run there. `/tmp` may be `noexec`.
- `install` creates a new file, so on SELinux it gets `bin_t`; `mv` from `/tmp` would keep `user_tmp_t` and
  systemd could not run it on Fedora. `restorecon` is a second safeguard.
- User: `getent group isshoni || groupadd --system isshoni`, then `getent passwd isshoni || useradd --system --gid
  isshoni --home-dir /var/lib/isshoni --no-create-home --shell "$(command -v nologin || echo /bin/false)"
  --comment "isshoni server" isshoni`.
- Directories: `install -d -m 0755 -o root -g root /etc/isshoni` and `install -d -m 0700 -o isshoni -g isshoni
  /var/lib/isshoni`. systemd would create both anyway; creating them early lets `config init` and backups work
  before the first start.

### 4.5 systemd unit: `deploy/systemd/isshoni.service`

```ini
# isshoni server. Local changes: `systemctl edit isshoni` (drop-ins survive upgrades).
[Unit]
Description=isshoni screen sharing server
Documentation=https://moonwx.github.io/isshoni/
Wants=network-online.target
After=network-online.target
StartLimitIntervalSec=120
StartLimitBurst=5

[Service]
Type=exec
User=isshoni
Group=isshoni
# Relative on purpose: systemd searches /usr/local/bin (install.sh) then /usr/bin (deb/rpm).
ExecStart=isshoni serve --config /etc/isshoni/isshoni.toml
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=3
# 78 = config error or database schema newer than this binary: restarting can't help.
RestartPreventExitStatus=78
TimeoutStopSec=20
UMask=0077

StateDirectory=isshoni
StateDirectoryMode=0700
ConfigurationDirectory=isshoni
RuntimeDirectory=isshoni
RuntimeDirectoryMode=0750

AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
ProtectProc=invisible
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
RemoveIPC=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM

[Install]
WantedBy=multi-user.target
```

Why these settings:
- **`AF_NETLINK`** is needed: Go's `net.Interfaces()`, which Pion's ICE gathering uses, reads addresses over netlink.
- **No `ProcSubset=pid`**: doctor and the server read `/proc/sys/net/core/{r,w}mem_max`.
- **No `PrivateUsers=`**: it would break binding 80/443, because the capability would no longer count in the host
  network namespace.
- **`Type=exec`, not `notify`**: readiness can take minutes (ACME), and the installer polls `isshoni healthcheck
  --ready` instead; 04 sends no `sd_notify` in M1.
- **`RestartPreventExitStatus=78`**: 04 exits 78 for every state a restart can't fix (invalid config, newer DB schema,
  failed migration, corrupt DB or secrets; 04 §6.3).
- **`SystemCallErrorNumber=EPERM`**: a filtered syscall returns an error instead of killing the process.
- **`ExecReload`**: `systemctl reload isshoni` sends SIGHUP, which re-reads manual TLS files and `log.level`
  (04 §6.5). Everything else needs `systemctl restart isshoni`.
- **`ProtectClock=yes`** (and `@system-service`) also blocks the read-only `adjtimex` call, so the server-side doctor
  can't read the kernel's clock-sync flag. 04's `clock` check then treats the sync state as unknown and relies on the
  Date-header skew check (04 §13.2). Don't relax `ProtectClock` for this.
- **`ConfigurationDirectory` keeps the default mode 0755**: the config file itself is 0640 root:isshoni.
- **Target score**: `systemd-analyze security isshoni` should report an exposure of 2.5 or less. Slice S5 records the
  real number, and CI fails if a change raises it by more than 0.2.

### 4.6 UDP buffers: `deploy/sysctl/60-isshoni.conf`

```
# isshoni: larger UDP socket buffers for WebRTC media. A 1080p keyframe (~150-300 KB) sent to several
# viewers at once overflows the 208 KiB default and shows up as loss and PLI storms.
net.core.rmem_max = 8388608
net.core.wmem_max = 8388608
```

- install.sh writes this file only when the current `/proc/sys/net/core/rmem_max` or `wmem_max` is below 8 MiB. It
  never lowers a value the admin set higher.
- It then runs `sysctl -p /etc/sysctl.d/60-isshoni.conf`. In a container that fails, and the script only warns.
- Packages ship the file in `/usr/lib/sysctl.d/`, where any `/etc/sysctl.d/` file overrides it.
- The server asks for 8 MiB per media socket, and doctor reports the effective size (`02`/`04`).

### 4.7 Firewall profiles

`deploy/firewall/ufw-isshoni` (ufw application profile):

```
[isshoni]
title=isshoni
description=isshoni screen sharing server (HTTPS, ACME, WebRTC media)
ports=80,443,7882/tcp|7882/udp
```

`deploy/firewall/firewalld-isshoni.xml`:

```xml
<?xml version="1.0" encoding="utf-8"?>
<service>
  <short>isshoni</short>
  <description>isshoni screen sharing server (HTTPS, ACME, WebRTC media)</description>
  <port protocol="tcp" port="80"/>
  <port protocol="tcp" port="443"/>
  <port protocol="tcp" port="7882"/>
  <port protocol="udp" port="7882"/>
</service>
```

Named profiles make uninstall exact (`ufw delete allow isshoni`, `--remove-service=isshoni`). They are also what an
admin sees in `ufw status`.

### 4.8 The one question, and the config file

Asked only when there is no `/etc/isshoni/isshoni.toml` (a fresh install, or a re-run after a cancel, 4.2) and
when neither `--domain`, `--ip` nor `--tls-mode` was given. It is read from `/dev/tty`, because stdin is the piped script. With no TTY and no
`--yes` → exit 2: "Non-interactive install: pass --domain NAME or --ip".

```
Domain name for this server (for example share.example.com).
Press Enter to use this server's IP address instead:
```

**Normalization.**
- Trim spaces, lower-case, strip `http://`/`https://`, any path and a trailing dot.
- An IPv4/IPv6 literal means IP mode with that IP as `--public-ip`.
- Otherwise the name must match `^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$` (punycode allowed). If not,
  ask again, at most 3 times, then exit 2.

**Config.** install.sh never writes TOML itself. It calls the binary (interface requested from `04`):

```
isshoni config init --path /etc/isshoni/isshoni.toml --domain share.example.com            # tls.mode derives to auto
isshoni config init --path /etc/isshoni/isshoni.toml --tls.mode ip [--public-ip 203.0.113.7]  # the real public IP
isshoni config init --path … --tls.mode off --public-url https://share.example.com [--listen.http 127.0.0.1:8080]
isshoni config init --path … --tls.mode manual --domain … --tls.cert-file … --tls.key-file …
                    [--tls.acme-email you@example.com]
```

The flags are 04's config flags (04 §4.2: `--` plus the key path with `_` → `-`). Then `chown root:isshoni` and
`chmod 0640`. `config init` refuses to overwrite an existing file (exit 7). `203.0.113.7` above stands for the
server's real public IP: in `ip` mode 04 rejects documentation ranges (04 §4.5).
- With `--tls.mode off` and a loopback `listen.http` (the default), `config init` also writes
  `network.trusted_proxies = ["127.0.0.0/8", "::1/128"]` into the file (04 §4.4), so a reverse proxy on the same host
  passes the real client IPs and the admin sees the value. install.sh passes no flag for it.

**Env isolation.** After `parse_args`, the script copies what it needs from `ISSHONI_*` variables into its own
variables and runs `unset` on every `ISSHONI_*` name it reads, and on any other `ISSHONI_*` variable found with
`env`. `config init`, `doctor`, `healthcheck` and `setup-url` therefore see only the config file, never a stray
variable from the admin's shell. `config init` takes its values from flags only and validates the result before
writing.
- `config init` exit 78 (invalid values, e.g. a private IP in IP mode): nothing was written. Print its message, then
  ask the question again (interactive) or exit 2 (non-interactive).

**Pre-check.** `isshoni doctor --config /etc/isshoni/isshoni.toml --only dns,public_ip,clock,firewall_hint`
(offline mode, 04 §13.1) prints its own human-readable text.
- `firewall_hint` is DMI-based and works offline. On a detected provider it prints that provider's cloud-firewall
  steps and the `/install/vps#<provider>` link **before** the start, so the admin can open 80/443 before Let's
  Encrypt tries. It only prints: no extra prompt.
- Exit 0: go on (warnings are printed and don't block).
- Exit 5 (a `fail`, e.g. "share.example.com points to 198.51.100.4 but this server is 203.0.113.7"): ask
  "[R]e-enter the domain, use the [I]P instead, or [C]ontinue anyway?", default R. With `--yes` → continue, with a
  warning.
- On R and I, the script deletes the `/etc/isshoni/isshoni.toml` it created in this run (never a file that existed
  before), calls `config init` again with the new answer and repeats the pre-check.
- **Cancel** (Ctrl-C or end of input at this prompt, exit 8) also deletes the config created in this run, like R and
  I. The next run then asks the question again (4.2). A port conflict (exit 7) keeps the config: the answer was
  fine, only the port was busy.

### 4.9 Ports in use, and the firewall offer

**Port check.**
- `ss -Hltnp` and `ss -Hlunp` for :80, :443 and :7882 (with `tls.mode=off`: :7882 and the configured HTTP port).
- A listener that isn't isshoni → exit 7, e.g.:

  ```
  Port 443 is used by nginx (pid 812). isshoni needs ports 80 and 443.
  Either stop nginx, or run isshoni behind it:
    curl -fsSL https://moonwx.github.io/isshoni/install.sh | ISSHONI_PUBLIC_URL=https://share.example.com sh -s -- --tls-mode off
  Guide: https://moonwx.github.io/isshoni/install/reverse-proxy
  ```

**Firewall offer** (first start, i.e. unit not enabled (4.2), or `--firewall`; skipped by `--no-firewall`; default
answer Yes; `--yes` accepts):

| Detected | Offer | Action |
|---|---|---|
| `ufw status` is `Status: active` | "Open ports 80, 443 (TCP) and 7882 (TCP+UDP) in ufw? [Y/n]" | `ufw allow isshoni` |
| `firewall-cmd --state` is `running` | same text, "in firewalld" | `firewall-cmd --permanent --add-service=isshoni && firewall-cmd --reload` |
| Neither, but `iptables -S INPUT` has a `-j REJECT`/`-j DROP` rule and `netfilter-persistent` exists (the Oracle Cloud Ubuntu image pattern) | same text, "in iptables" | Insert before the first REJECT/DROP, for IPv4 and IPv6, then `netfilter-persistent save`. Each rule is tagged `-m comment --comment isshoni` so uninstall finds it: `-p tcp -m multiport --dports 80,443,7882 -j ACCEPT` and `-p udp --dport 7882 -j ACCEPT` |
| None of these | nothing to open locally | |

- **An inactive ufw or firewalld is never enabled**, because that could lock the admin out of SSH.
- **Always printed at the end:** "If your provider has a cloud firewall (AWS, Google Cloud, Azure, Oracle, …), open
  the same ports there: https://moonwx.github.io/isshoni/install/vps". The provider-specific steps were already
  printed before the start by the pre-check's `firewall_hint` (4.8). The browser connection test in the setup
  wizard (`05`) is the real reachability check.

### 4.10 Start, certificate wait, setup link, upgrade

**Start.**
- `systemctl daemon-reload`.
- First start (unit not enabled, 4.2) with effective TLS mode `auto` or `ip`: first print one line (a notice, not a
  question; also printed with `--yes`): "isshoni gets its certificate from Let's Encrypt. Using it means you accept
  the Let's Encrypt Subscriber Agreement: https://letsencrypt.org/repository/".
- First start: `systemctl enable --now isshoni`.
- Upgrade: `systemctl restart isshoni` (signaling reconnect covers clients: `01`/`04`).
- Repair (same version, unit enabled): restart only if the unit or the binary changed. If it isn't active:
  `systemctl reset-failed isshoni` (a unit that exited 78 or hit `StartLimitBurst` won't start otherwise), then
  `systemctl start isshoni`.

**Wait** (every path that starts or keeps the server: first start, upgrade and repair).
- Poll `isshoni healthcheck --ready` (04: exit 0 ready, 1 not ready or unreachable). Ready means DB, TLS
  certificate, media sockets and the hub, per the plan.
- Every 2 s, for at most 180 s on a first start and 120 s on an upgrade or repair.
- Every 10 s it prints `Waiting for the HTTPS certificate… (20 s)`.
- After 30 s it adds: "Let's Encrypt must reach this server on port 80 or 443. Check your provider's firewall."
- On timeout: the last 30 lines of `journalctl -u isshoni --no-pager`, the full `isshoni doctor` output, the links
  to troubleshooting, and as the last line "Fix the problem above, then run this installer again." → exit 5. The
  unit stays enabled, so the re-run takes the repair branch, waits again and prints the setup link (4.2).

**Setup link** (first start and repair; an upgrade prints "Upgraded IV → TV" instead).
- Runs `isshoni setup-url` with `--qr` when stdout is a TTY.
- Exit 0: the link is printed (below). Exit 7 from `setup-url` means an admin already exists: print "Setup is already
  done. Open https://<host>/". Exit 4 means the server isn't reachable (diagnostics, exit 5).

```
isshoni 0.1.0 is running.

  Create the admin account (link valid for 24 hours, works once):
  https://share.example.com/setup#k3J…
  <QR code>

  Status   systemctl status isshoni        Logs     journalctl -u isshoni -f
  Check    sudo isshoni doctor             Upgrade  run this installer again
```

**Upgrade rules.**
- The server itself writes `backups/pre-<schema>-<ts>.db` before migrating (`03`/`04`). `<schema>` is the schema
  version before migrating, e.g. `pre-3-20261014T021500Z.db` (03 §4.4).
- The installer never edits an existing config and never re-asks the question.
- Firewall again only with `--firewall`.
- **No automatic rollback.** After a migration the old binary refuses the newer schema, so an automatic binary
  rollback would only produce a second failure.
- On exit 5 after an upgrade, the script prints the manual rollback with the exact file:
  `sudo systemctl stop isshoni && sudo -u isshoni isshoni admin restore --offline
  /var/lib/isshoni/backups/pre-3-20261014T021500Z.db` (04 §12.4 accepts a pre-migration DB file), then re-run the
  installer with `--version <old> --allow-downgrade`. The script finds that file itself, with privilege (the
  directory is `isshoni` 0700, so an unprivileged shell can't expand the glob): the newest match of
  `ls -t /var/lib/isshoni/backups/pre-*.db`, run through its `sudo` prefix. If there is none (this upgrade didn't
  migrate), it prints only the `--version <old> --allow-downgrade` step.

**Version comparison** is SemVer, implemented in awk:
- `X.Y.Z` is compared numerically;
- a prerelease is lower than its release;
- prerelease identifiers compare numerically when both are digits, lexically otherwise.

`sort -V` gets `0.2.0` vs `0.2.0-rc.1` wrong, so it is not used.

### 4.11 Uninstall and purge

**`--uninstall`** (exit 0 even when parts are already gone):
1. `systemctl disable --now isshoni`.
2. Remove the unit file and run `daemon-reload`.
3. Remove `/usr/local/bin/isshoni` and `/etc/sysctl.d/60-isshoni.conf`. Running values are left until reboot.
4. Firewall:
   - ufw: `ufw delete allow isshoni` if present, then remove the profile;
   - firewalld: `--permanent --remove-service=isshoni`, reload, remove the XML;
   - iptables: delete the rules tagged `isshoni` and save.
5. Print: "Kept: /etc/isshoni (config), /var/lib/isshoni (database, certificates, backups) and the isshoni user.
   Remove them with --purge. Before --purge, back up with `sudo isshoni admin backup --out <file>` while isshoni is
   installed and running (run this installer again to bring it back)." It can't offer a backup command for right
   now: at this point the service is stopped and the binary deleted, and an offline backup refuses to run as root
   (04 §12.6).

**`--purge`**:
- Asks for `purge` to be typed; `--yes` skips the question.
- Runs uninstall, then `rm -rf /etc/isshoni /var/lib/isshoni /run/isshoni`, then `userdel isshoni` and
  `groupdel isshoni` if the group still exists.
- It does **not** make a backup; the prompt says so and shows `sudo isshoni admin backup --out <file>`. The prompt
  comes before uninstall step 1, while the server still runs, so that command works; any answer but `purge` exits 8
  with nothing changed.

If isshoni was installed from a package, both refuse and print `apt remove isshoni` / `dnf remove isshoni`.

### 4.12 Script structure and coding rules

- **POSIX sh.** No `local`, no arrays, no `[[ ]]`, no `echo -e`; `printf` only.
- **Must pass** `shellcheck --shell=sh --severity=style` and `shfmt -p -i 2 -d`, and run under `dash`,
  `bash --posix` and `busybox sh`.
- **The whole body is in functions.** The last line is
  `[ "${ISSHONI_INSTALL_SOURCED:-0}" = 1 ] || main "$@"`. This means a truncated download never runs a partial
  script, and unit tests can source it.
- `set -eu`. Every external command's failure is handled explicitly in the functions that call it.
- **Functions** (one job each, unit-tested where marked \*):
  - `say`, `warn`, `die CODE MSG`, `ask PROMPT DEFAULT`\* (reads `/dev/tty`; without a TTY it returns the default or
    dies when there is none), `confirm`\*;
  - `parse_args`\*, `preflight`, `detect_arch`\*, `detect_distro`, `need_ssh_keygen`, `downloader`, `download`;
  - `resolve_version`\*, `version_cmp`\*, `normalize_domain`\*, `checksum_for`\*, `verify_release`\*;
  - `install_binary`, `ensure_user`, `install_unit`, `apply_sysctl`, `install_fw_profiles`, `write_config` (calls
    `config init` with flags only, after `ISSHONI_*` is unset; on a pre-check R or I it deletes the file it created
    in this run and calls `config init` again, and on a cancel there it deletes that file and exits 8; 4.8),
    `doctor_precheck`, `is_started` (`systemctl is-enabled --quiet isshoni`, 4.2), `check_ports`\* (parses `ss`
    output), `offer_firewall`, `start_or_restart`, `wait_ready`, `print_setup`, `newest_premigration_backup`
    (privileged `ls -t`, 4.10);
  - `do_uninstall`, `do_purge`, `main`.
- **Output.** Colors only when stdout is a TTY and `NO_COLOR` is unset. No emoji. Every error says what to do next.
- **Never** logs the setup token anywhere but stdout. Never writes it to a file.

### 4.13 Supported distros and how they are tested

| Distro | Container (every PR that touches deploy) | VM (nightly and before a release) | Notes |
|---|---|---|---|
| Debian 12 | `debian:12` | Debian 12 genericcloud | `nologin` in `/usr/sbin` |
| Debian 13 | `debian:13` | Debian 13 genericcloud | |
| Ubuntu 22.04 | `ubuntu:22.04` | jammy cloud image | systemd 249 (no `systemd-analyze security --threshold`), OpenSSH 8.9 |
| Ubuntu 24.04 | `ubuntu:24.04` | noble cloud image | ufw installed, inactive |
| Ubuntu 26.04 | `ubuntu:26.04` | 26.04 cloud image | current LTS, added because it exists now |
| Fedora (current, 44 today) | `fedora:44` | Fedora Cloud Base | SELinux enforcing, firewalld. Bump each Fedora release |

- Other systemd distros (Rocky/Alma, Arch, openSUSE) are "community-tested", like the plan's hardware rows.
- Not supported by install.sh: Alpine, WSL without systemd, and containers. Docker is the answer there.

**Containers** (`deploy/test/distro/`):
- `Containerfile` takes `ARG BASE` and installs `systemd`, `dbus`, `curl`, `ca-certificates`, `iproute2`, `procps`
  and `python3`. It deliberately does **not** install openssh-client, which exercises the auto-install path.
- Started with `podman run -d --systemd=always` on CI, or `docker run -d --privileged --cgroupns=private --tmpfs /run
  --tmpfs /run/lock` locally (Docker Desktop works on macOS). `CONTAINER_ENGINE` selects which.
- `tools/relserve` serves a locally built, test-signed release over HTTPS (section 11.2).

**VMs** (`deploy/test/vm/`):
- QEMU with KVM on `ubuntu-24.04` runners: enable `/dev/kvm` with the usual udev rule, boot the official cloud image
  with a cloud-init seed, and SSH in.
- If `/dev/kvm` is missing, the job is **skipped with a warning** (it is nightly, not a PR gate). Before a release the
  same script runs by hand in UTM/Tart on the Mac.
- This is the only place that tests SELinux, real sysctl writes, enabled ufw/firewalld, the Oracle-style iptables
  path and ACME (Pebble).

## 5. deb and rpm packages

These are for admins who prefer packages; `install.sh` stays the documented path. Signed apt/rpm repositories are
**Later** (plan). Built by goreleaser nfpm, amd64 and arm64.

| Path | Source |
|---|---|
| `/usr/bin/isshoni` | binary |
| `/usr/lib/systemd/system/isshoni.service` | `deploy/systemd/isshoni.service` (the same file, see D3) |
| `/usr/lib/sysctl.d/60-isshoni.conf` | `deploy/sysctl/60-isshoni.conf` |
| `/etc/ufw/applications.d/isshoni` | conffile |
| `/usr/lib/firewalld/services/isshoni.xml` | |
| `/etc/isshoni/` | dir, 0755 |
| `/usr/share/doc/isshoni/{LICENSE,NOTICE,THIRD_PARTY_NOTICES}` | |

Scripts in `deploy/packaging/`:
- `preinstall.sh`:
  - creates the user and group (same commands as 4.4);
  - fails when `/usr/local/bin/isshoni` exists: "isshoni was installed with install.sh; run it with --uninstall
    first".
- `postinstall.sh`:
  - `systemctl daemon-reload` and `systemd-sysctl 60-isshoni.conf || true`;
  - **does not enable or start** the service, because there is no config yet. It prints the next steps:
    `sudo isshoni config init --path /etc/isshoni/isshoni.toml --domain … && sudo chown root:isshoni
    /etc/isshoni/isshoni.toml && sudo systemctl enable --now isshoni && sudo isshoni setup-url`;
  - on upgrade: `systemctl try-restart isshoni`.
- `preremove.sh`: `systemctl disable --now isshoni` on remove (deb `remove`, rpm `$1 = 0`), not on upgrade.
- `postremove.sh`: `daemon-reload`. On deb `purge`: remove `/etc/isshoni`, keep `/var/lib/isshoni` and say so. Data is
  deleted only by explicit admin action.

The package maintainer field is the owner's GitHub noreply address (filled in once; no personal e-mail in the repo).

## 6. Docker

### 6.1 Image: `deploy/docker/Dockerfile`

```dockerfile
# syntax=docker/dockerfile:1
# Built by goreleaser dockers_v2 (release) and deploy/test/docker-build.sh (CI). No RUN steps: nothing is emulated
# for arm64, and the image holds only the static binary.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:<pinned digest, updated by Dependabot>
ARG TARGETPLATFORM
# rootfs/ contains var/lib/isshoni/.keep and run/isshoni/.keep: both directories owned by nonroot, so a new named
# volume is writable (Docker copies the ownership into an empty volume) and the admin socket has a home.
COPY --chown=65532:65532 deploy/docker/rootfs/ /
COPY $TARGETPLATFORM/isshoni /usr/local/bin/isshoni
COPY LICENSE NOTICE THIRD_PARTY_NOTICES /usr/share/doc/isshoni/
# data_dir keeps 04's default /var/lib/isshoni; the admin socket keeps /run/isshoni/admin.sock.
ENV ISSHONI_IN_CONTAINER=1
EXPOSE 80/tcp 443/tcp 7882/udp 7882/tcp
USER 65532:65532
HEALTHCHECK --interval=30s --timeout=5s --start-period=120s --retries=3 \
  CMD ["/usr/local/bin/isshoni", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/isshoni"]
CMD ["serve"]
```

- **No `VOLUME` instruction.** An anonymous volume would hide a missing mount.
- **Data volume check** (04 §5.1). With `ISSHONI_IN_CONTAINER=1`, the server checks that `/var/lib/isshoni` is a mount
  point (via `/proc/self/mountinfo`). If not, it exits 78 with "Mount a volume at /var/lib/isshoni (see
  compose.yaml)". `ISSHONI_ALLOW_EPHEMERAL_DATA=1` allows it for tests. doctor reports the same (plan: "doctor
  fails").
- **Admin socket.** `/run/isshoni` must be writable with a read-only root filesystem, so compose mounts a tmpfs there
  (below); `docker compose exec isshoni isshoni setup-url` and the `HEALTHCHECK` then find the socket with no flags.
- **Config.** There is no config file in the image. The config comes from env; `/etc/isshoni/isshoni.toml` is
  optional and read-only when mounted.
- **Start period.** 120 s covers ACME on first start.
- Multi-arch: `linux/amd64` and `linux/arm64`, with `org.opencontainers.image.*` labels (section 9.2).

### 6.2 `deploy/compose.yaml` (bridge networking, the default)

```yaml
# isshoni. Start: docker compose up -d   Then: docker compose exec isshoni isshoni setup-url
# Put ISSHONI_DOMAIN=share.example.com in a .env file next to this file, or leave it empty for an IP certificate.
# isshoni gets its certificate from Let's Encrypt. Using it means you accept the Let's Encrypt Subscriber Agreement: https://letsencrypt.org/repository/
# Docs: https://moonwx.github.io/isshoni/install/docker
name: isshoni
services:
  isshoni:
    image: ghcr.io/moonwx/isshoni:latest
    restart: unless-stopped
    ports:
      - "80:80/tcp"       # ACME HTTP-01, redirect to HTTPS
      - "443:443/tcp"     # HTTPS, signaling, ICE-TCP fallback
      - "7882:7882/udp"   # media
      - "7882:7882/tcp"   # ICE-TCP
    environment:
      ISSHONI_DOMAIN: ${ISSHONI_DOMAIN:-}        # empty: certificate for the public IP
      ISSHONI_PUBLIC_IP: ${ISSHONI_PUBLIC_IP:-}  # empty: detected with STUN (required behind Docker's NAT)
    volumes:
      - data:/var/lib/isshoni                    # mandatory: users, rooms, certificates
    read_only: true
    tmpfs:
      - /tmp
      - /run/isshoni:uid=65532,gid=65532,mode=0750   # admin socket (healthcheck, setup-url, backup)
    cap_drop: [ALL]
    security_opt:
      - no-new-privileges:true
    stop_grace_period: 20s
    logging:
      driver: json-file
      options: { max-size: "10m", max-file: "3" }
volumes:
  data:
```

- **Empty means unset.** An empty `ISSHONI_*` value counts as unset (04 §4.2), so leaving `ISSHONI_DOMAIN`/
  `ISSHONI_PUBLIC_IP` out of `.env` (or having no `.env` at all) gives an IP certificate and STUN detection. To set a
  key to the empty string, use a mounted config file or a flag.
- **Non-root on 443 in bridge mode.** Docker sets `net.ipv4.ip_unprivileged_port_start=0` inside container network
  namespaces (Docker ≥ 20.10), so the nonroot user binds 80/443 with `cap_drop: [ALL]`.
- **Public IP.** The container sees a 172.x address, so the server advertises the public IP from STUN or
  `ISSHONI_PUBLIC_IP` through `SetICEAddressRewriteRules` (`02`/`04`).
- **TLS mode default in the container** (requested from `04`): a non-empty `ISSHONI_DOMAIN` means `auto`, an empty
  one means `ip`, unless `ISSHONI_TLS_MODE` is set.
- **Upgrade:** `docker compose pull && docker compose up -d`. The server backs up before migrating (`03`/`04`).
- **Why `:latest` in 0.x:** see open question Q4.

### 6.3 Host-network alternative: `deploy/compose.host.yaml`

For IPv6, or to avoid Docker's NAT. Usage: `docker compose -f compose.host.yaml up -d`.

```yaml
name: isshoni
services:
  isshoni:
    image: ghcr.io/moonwx/isshoni:latest
    restart: unless-stopped
    network_mode: host
    user: "0:0"                  # host netns: ports < 1024 need the capability, which only root keeps
    cap_drop: [ALL]
    cap_add: [NET_BIND_SERVICE]
    environment:
      ISSHONI_DOMAIN: ${ISSHONI_DOMAIN:-}
    volumes:
      - /var/lib/isshoni:/var/lib/isshoni   # host directory, created root-owned by Docker; see below
    read_only: true
    tmpfs:
      - /tmp
      - /run/isshoni:mode=0750
    security_opt:
      - no-new-privileges:true
    stop_grace_period: 20s
    logging:
      driver: json-file
      options: { max-size: "10m", max-file: "3" }
```

- **Why a bind mount and not the named volume.** Root with `cap_drop: [ALL]` has no `CAP_DAC_OVERRIDE`, so it can't
  write into the image's 65532-owned `/var/lib/isshoni`. A root-owned host directory avoids that. It is the same path
  as a systemd install; a second server on the same data directory refuses to start (04 §5.1 lock).
- **Switching modes** needs a one-time copy, with the old containers stopped. The docs give this command, using a
  throwaway image, which is fine because it isn't shipped:

  ```
  docker run --rm -v isshoni_data:/from -v /var/lib/isshoni:/to alpine sh -c 'cp -a /from/. /to/ && chown -R 0:0 /to'
  ```

  The `chown` is required: host mode runs as root without `CAP_DAC_OVERRIDE` and can't write the copied 65532-owned
  0700 directory. For the reverse direction (host → bridge), swap the mounts and end with `chown -R 65532:65532 /to`.
- **The host firewall applies** in host mode (no Docker iptables rules), so the ufw/firewalld steps from 4.9 apply.
- **Why not non-root plus `cap_add`?** Docker doesn't give ambient capabilities to a non-root user, and file
  capabilities are blocked by `no-new-privileges`.

### 6.4 Things the Docker docs page must say

- **Host sysctls.** `net.core.rmem_max`/`wmem_max` are not per-namespace, so they can't be set from compose. One-liner:
  `printf 'net.core.rmem_max=8388608\nnet.core.wmem_max=8388608\n' | sudo tee /etc/sysctl.d/60-isshoni.conf && sudo
  sysctl --system`. doctor inside the container reports the effective buffer size (`04`).
- **Published ports bypass ufw.** Docker's iptables rules run before ufw's, so `ufw deny` doesn't protect them, and no
  ufw rule is needed for them. The cloud firewall still applies. To restrict, publish on one address
  (`"203.0.113.7:443:443/tcp"`) or use the `DOCKER-USER` chain.
- **Bind mount instead of a volume:** `sudo chown 65532:65532 ./data` first, and mount it at `/var/lib/isshoni`.
- **Rootless Docker and Podman:** binding 80/443 needs `net.ipv4.ip_unprivileged_port_start=80` on the host, and UDP
  through slirp4netns/pasta is slow. Rootful Docker or install.sh is recommended.
- **IPv6 clients in bridge mode.** The bridge network has no IPv6 by default (04 §7.6), so IPv6 clients reach
  isshoni through `docker-proxy`, which connects from the bridge gateway's address: they all share that one address's
  per-IP limits (login, registration, invite check, WebSocket handshakes; 03, 01), and one stranger can lock them all
  out. When the domain has an AAAA record, use `compose.host.yaml` (6.3). `task docker:smoke` checks this behaviour
  (11.3).
- **Operations:**
  - setup link: `docker compose exec isshoni isshoni setup-url`;
  - doctor: `docker compose exec isshoni isshoni doctor`;
  - backup to the host, the only documented Docker form (04 §12.3):
    `(umask 077; docker compose exec -T isshoni isshoni admin backup --out - > isshoni-backup.tar.gz)`. The archive
    holds `secrets.json` and the TLS private keys; a plain shell redirect would create the file with the host's umask
    (usually `0644`, readable by every local user), and the subshell's `umask 077` makes it `0600`;
    restore: `docker compose exec -T isshoni isshoni admin restore --yes - < isshoni-backup.tar.gz` (04 §12.4).
    `--yes` is required: `-T` gives no TTY and stdin carries the archive, so no confirmation can be read. Without it
    the CLI refuses and prints this exact command. `task docker:smoke` runs both (11.3), so the docs can't drift;
  - when the server refuses to start (exit 78, e.g. a newer DB schema after a downgrade): systemd stops on exit 78,
    but under `restart: unless-stopped` Docker restarts the container in a loop (the backoff is capped at about a
    minute). `docker compose logs --tail 50 isshoni` shows the reason and the exact restore command. Then, in this
    order: `docker compose stop` (first, before any restore), `docker compose run --rm isshoni admin restore
    --offline /var/lib/isshoni/backups/<file>`, `docker compose up -d` (04 §6.3). `docker compose stop` comes first
    because the loop keeps restarting the server, and the offline restore refuses (exit 7) while any container still
    holds the data directory (04 §5.1 lock). Every restart after a partial migration writes one more pre-migration
    backup; the rotation always keeps the newest file per schema version (03 §4.4), so the file the older image needs
    survives the loop;
  - logs: `docker compose logs -f` (JSON under Docker, per the plan).
- **Ports 80/443 already taken** by nginx, Caddy or Traefik on the host: `compose.yaml` fails with "port is already
  allocated". The page links to the Docker section of `/install/reverse-proxy` (6.5).

### 6.5 Docker behind the host's reverse proxy (`/install/reverse-proxy#docker`)

For hosts where a proxy already owns 80/443. The page gives a **complete compose file**, used instead of
`compose.yaml`, not a merge override: a compose override can't remove the base file's 80/443 mappings (only with
`!override`, Compose ≥ 2.24.4).

```yaml
# isshoni behind your own reverse proxy on this host. Your proxy forwards https://share.example.com to 127.0.0.1:8080.
# Put ISSHONI_PUBLIC_URL=https://share.example.com in a .env file next to this file.
# Docs: https://moonwx.github.io/isshoni/install/reverse-proxy#docker
name: isshoni
services:
  isshoni:
    image: ghcr.io/moonwx/isshoni:latest
    restart: unless-stopped
    ports:
      - "127.0.0.1:8080:8080/tcp" # plain HTTP for the proxy on this host only; never publish it on all addresses
      - "7882:7882/udp"           # media, direct (not through the proxy)
      - "7882:7882/tcp"           # ICE-TCP, direct (443 belongs to the proxy)
    environment:
      ISSHONI_TLS_MODE: "off"
      ISSHONI_PUBLIC_URL: ${ISSHONI_PUBLIC_URL:?set ISSHONI_PUBLIC_URL=https://your.domain in .env}
      ISSHONI_LISTEN_HTTP: 0.0.0.0:8080                  # inside the container; the mapping above keeps it on loopback
      ISSHONI_NETWORK_TRUSTED_PROXIES: 172.30.89.0/24    # this file's network: the proxy's connections arrive from its gateway
      ISSHONI_PUBLIC_IP: ${ISSHONI_PUBLIC_IP:-}          # empty: detected with STUN
    networks: [isshoni]
    volumes:
      - data:/var/lib/isshoni
    read_only: true
    tmpfs:
      - /tmp
      - /run/isshoni:uid=65532,gid=65532,mode=0750
    cap_drop: [ALL]
    security_opt:
      - no-new-privileges:true
    stop_grace_period: 20s
    logging:
      driver: json-file
      options: { max-size: "10m", max-file: "3" }
networks:
  isshoni:
    ipam:
      config:
        - subnet: 172.30.89.0/24   # fixed, so ISSHONI_NETWORK_TRUSTED_PROXIES can name it; any free private /24 works
volumes:
  data:
```

- **Why `0.0.0.0:8080` inside, `127.0.0.1` outside.** Off mode listens on loopback by default (04), which a published
  port can't reach inside the container. Publishing on `127.0.0.1` keeps plain HTTP off the internet.
- **Why the trusted range is the compose network.** The host proxy's connections reach the container through
  Docker's port forwarding, so the TCP peer is the network's gateway (`172.30.89.1`), not `127.0.0.1`. Without
  that range in `network.trusted_proxies`, client IPs are wrong and `X-Forwarded-Proto` is ignored (04 §8.5). Only
  isshoni, and a proxy container you attach (below), sit on this network, so trusting the /24 trusts nobody else.
- **A proxy that runs in Docker too** joins this network (`networks: isshoni: { external: true, name:
  isshoni_isshoni }` in its own compose file) and forwards to `http://isshoni:8080`; the `8080` mapping can then go.
- The proxy config itself is the same as for a shell install in off mode (WebSocket upgrade for `/ws`, upstream
  `127.0.0.1:8080`). ICE-TCP uses 7882 only, because the proxy owns 443 (04 §8.5), and 7882 must be open in the
  cloud firewall. Published ports bypass ufw (6.4).
- Operations (setup-url, doctor, backup, restore) are the same commands as in 6.4.

## 7. Development workflow

### 7.1 Tool pins

`.tool-versions` (read by mise and asdf):

```
golang 1.27.<latest patch>
nodejs 26.5.0
task 3.<latest>
golangci-lint 2.<latest>
goreleaser 2.<latest ≥ 2.12, for dockers_v2>
shellcheck 0.<latest>
shfmt 3.<latest>
actionlint 1.<latest>
```

- `<latest>` means: pin the exact latest patch when slice S1 lands. Versions are bumped by hand, monthly, in one PR;
  Dependabot can't read `.tool-versions`.
- `go.mod` has `go 1.26.0` and `toolchain go1.27.1`, the `.tool-versions` patch (the plan: language version 1.26,
  newest toolchain). The go line carries a patch because `golang.org/x/text`, `golang.org/x/crypto` and
  `modernc.org/sqlite` require `go 1.26.0`, so `go get` and `go mod tidy` write it that way; the language version is
  still 1.26. `tools/go.mod` has the same two lines. CI reads the version from `go.mod`, not from `.tool-versions`,
  and `task lint:pins` fails if the two differ.
- Node's bundled npm (11.x) is used; `package.json` has `"engines": {"node": ">=26"}` and
  `"packageManager": "npm@<exact 11.x version>"`.

**`tools/go.mod`** is a separate module, `github.com/MoonWX/isshoni/tools`, with Go 1.24+ `tool` directives:
- tools: `github.com/gzuidhof/tygo`, `github.com/google/go-licenses/v2`, `golang.org/x/vuln/cmd/govulncheck`, and
  Task, golangci-lint and shfmt (`mvdan.cc/sh/v3`) at the `.tool-versions` versions (`task lint:pins` checks them).
  actionlint and ShellCheck come from mise or a package manager (actionlint v1.7.12 doesn't build against the
  `go.yaml.in/yaml/v4` release candidate that golangci-lint's gosec needs);
- our own small programs: `tools/repocheck` (behind `task lint:pins` and `lint:keys`), `tools/notices` (section 8.4),
  `tools/relserve` (section 11.2) and `tools/exittest` (section 12).

`task tools` builds the pinned tools into `.bin/`, using the repo toolchain:
`go -C tools build -o ../.bin/ github.com/gzuidhof/tygo …`. They run from the repo root (tygo and go-licenses load the
main module's packages). Building them with the repo's own toolchain avoids go-licenses' known "stdlib has no module
info" error when its Go version differs from the one in `PATH`.

### 7.2 `Taskfile.yml`

Task runs every command through its built-in POSIX shell, so the tasks work on macOS, Linux and Windows.

**One version per build.** A top-level Task variable `VERSION` is the single version string of a build. Both halves
get the same value: the Go binary as ldflags `version`, and the Vite build (and `vite dev`) as env
`ISSHONI_VERSION`, which 05's `version-plugin.ts` writes into `web/dist/version.json`. A mismatch between the two
would trigger 01's stale-build reload.

```yaml
vars:
  DEV_VERSION:
    sh: printf '0.0.0-dev+%s%s' "$(git rev-parse --short=12 HEAD)" "$(test -z "$(git status --porcelain)" || printf '%s' -dirty)"
  VERSION: '{{.ISSHONI_VERSION | default .DEV_VERSION}}'
```

- Order: `task build VERSION=…` on the command line (a CLI variable wins over a global one), else env
  `ISSHONI_VERSION` (CI, 8.2), else `0.0.0-dev+<12-char commit>`, plus `-dirty` when the working tree has changes.
- A "dev build" is one whose SemVer prerelease starts with `dev` (04 §15). Local builds are dev builds; CI builds
  (`0.0.0-ci.<run>`) and releases are not. A local `task release:snapshot` without `ISSHONI_SNAPSHOT_VERSION` is one
  (`0.0.0-dev.<12-char commit>`).
- **Exception: `task e2e`.** When `ISSHONI_VERSION` is unset, it passes `VERSION=0.0.0-e2e.local` to `build`. That is
  a non-dev prerelease, so 05's `version.spec` (which needs a non-dev SPA, 8.2) passes on the Mac too, and the binary
  and the SPA still share one version.

| Task | Does | Notes |
|---|---|---|
| `setup` | `npm ci` in `web/` and `docs/`, `go mod download`, `task tools` | once per clone |
| `setup:e2e` | `npx --prefix web playwright install chrome` | only for e2e |
| `dev` | runs `dev:server` and `dev:web` in parallel | open http://localhost:5173 |
| `dev:server` | `go run -ldflags "-X github.com/MoonWX/isshoni/internal/version.version={{.VERSION}}" ./cmd/isshoni serve --config deploy/dev/isshoni.dev.toml` | restart by hand; optional `watchexec -r -e go -- task dev:server`. Same `VERSION` as `dev:web`, so dev never hits 01's stale-build reload |
| `dev:web` | `npm --prefix web run dev` with env `ISSHONI_VERSION={{.VERSION}}` | Vite on 5173; proxies `/api` and `/ws` to 127.0.0.1:8080 (`05` owns `vite.config.ts`) |
| `dev:setup-url` | `go run ./cmd/isshoni setup-url --config deploy/dev/isshoni.dev.toml` | first-run admin link |
| `gen` | `deps: [tools]`; `.bin/tygo generate` (config `tygo.yaml`, content owned by `01`; both `internal/protocol` and `internal/protocol/api`), then `go run ./internal/protocol/gen/tsregistry -o web/src/protocol/registry.gen.ts` | |
| `gen:check` | `gen`, then fail if `git status --porcelain -- 'web/src/protocol/*.gen.ts'` prints anything (changed or new untracked generated files; hand-written files there are not checked) | the CI drift check |
| `test` | `test:go`, `test:web`, `test:sh` | |
| `test:go` | `CGO_ENABLED=1 go test -race -count=1 -timeout 15m ./...` | the race detector needs cgo; the shipped binary stays `CGO_ENABLED=0` |
| `test:web` | `npm --prefix web test -- --run`, then `node --test scripts/licenses.test.mjs` in `web/` | Vitest; the license script's tests run under Node's test runner (CI runs them in the `licenses` job) |
| `test:sh` | `deploy/test/install_unit.sh` under dash, `bash --posix` and busybox | |
| `lint` | `lint:go`, `lint:web`, `lint:sh`, `lint:actions`, `lint:keys`, `lint:pins`, `lint:unit` | |
| `lint:go` | `golangci-lint run`, `go mod tidy -diff`, `GOOS=darwin go vet ./...`, `GOOS=windows go vet ./...` | |
| `lint:web` | `npm --prefix web run lint && npm --prefix web run typecheck` | |
| `lint:sh` | `shellcheck --shell=sh` and `shfmt -p -i 2 -d` over `deploy/**/*.sh` | |
| `lint:actions` | `actionlint` | |
| `lint:keys` | embedded block in `install.sh` = `deploy/keys/allowed_signers` | |
| `lint:unit` | `systemd-analyze verify deploy/systemd/isshoni.service` (Linux only; skipped elsewhere) | |
| `build:web` | `npm --prefix web run build` with env `ISSHONI_VERSION={{.VERSION}}` → `web/dist/` (plus `web/dist/licenses.txt` and `version.json`) | Task `sources`/`generates` make it a no-op when nothing changed. A `status:` check next to them (a one-line `node -p` read of `web/dist/version.json`) makes it out of date whenever that file's `version` ≠ `{{.VERSION}}` |
| `build:go` | `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/MoonWX/isshoni/internal/version.version={{.VERSION}} -X github.com/MoonWX/isshoni/internal/version.commit=$(git rev-parse HEAD) -X github.com/MoonWX/isshoni/internal/version.date=$(git log -1 --format=%cI)" -o bin/isshoni ./cmd/isshoni` (04 §15) | Go only, no Node needed (CI's `build` job). Fails if `web/dist/index.html` is missing. The flags are identical in content to 9.2 (commit date, as goreleaser's `{{ .CommitDate }}` and `mod_timestamp`) |
| `build` | `build:web`, then `build:go` | one `VERSION` for both |
| `e2e` | `build` with `VERSION: '{{.ISSHONI_VERSION \| default "0.0.0-e2e.local"}}'`, then `npm --prefix web run e2e` with `ISSHONI_BIN={{.ROOT_DIR}}/bin/isshoni` | a non-dev version (see above). `ISSHONI_BIN` is absolute, because npm runs scripts from `web/`. Playwright, Chrome channel |
| `licenses` | go-licenses check (3 GOOS) and `node web/scripts/licenses.mjs check` for `web/` and `docs/` | section 8.3 |
| `notices` | `go -C tools run ./notices …` → `THIRD_PARTY_NOTICES` | section 8.4 |
| `release:prepare` | **no npm**: fails with "web/dist was built for X, not VERSION: run task build:web VERSION=…" unless `web/dist/version.json`'s `version` equals `VERSION` (read with `sed`, no Node); then `notices`, stamp `dist-extra/install.sh` (`VERSION=`) | goreleaser's before hook (`task release:prepare VERSION={{ .Version }}`, 9.2). The SPA comes from outside: the unprivileged `web` job in `release.yml` (9.3), or `release:snapshot` below. So the privileged release job runs no third-party JavaScript |
| `release:snapshot` | `build:web`, then `goreleaser release --snapshot --clean --skip=sign,sbom`; both get `ISSHONI_SNAPSHOT_VERSION` (default `0.0.0-dev.<12-char commit>`), as `VERSION` for `build:web` and as env for goreleaser | local dry run; `goreleaser-check` and the distro tests use it too |
| `release:compat` | `task release:compat VERSION=<v>` (`<v>` is the tag without `v`, e.g. `0.2.0`): copies `internal/protocol/testdata/v1` to `internal/protocol/testdata/compat/<v>/`, then deletes all but the two newest final-release snapshots (SemVer order). Fails unless `VERSION` is a final `X.Y.Z`: prereleases (`-rc.N`) get no snapshot, and a forgotten `VERSION=` leaves the dev version, which fails too | 01 §14.3. Run in the release-prep PR (9.1 "Before tagging"); release.yml checks the result (9.3). Plain `sh` and `cp`, no Go or Node. S01 writes it as a stub; S75 (01 P13) owns the body |
| `deploy:test` | distro container tests; `DISTRO=debian-12` selects one | needs podman or Docker |
| `docker:smoke` | section 6 smoke test (bridge with a backup → restore round trip and the IPv6 client-IP check, host, no-volume, no `.env`; 11.3) | |
| `site:dev` / `site:build` | `npx --prefix docs vitepress dev\|build docs` | |
| `clean` | remove `bin/`, `.bin/`, `dist/`, `dist-extra/`, `web/dist/*` (keeps `.gitkeep`), `.dev/` | |

`.gitignore` gains: `.bin/`, `dist-extra/`, `.dev/`, `web/dist/*`, `!web/dist/.gitkeep`, `THIRD_PARTY_NOTICES`,
`web/build-report.json`, `web/playwright-report/`, `web/test-results/`,
`docs/public/{install.sh,compose*.yaml,keys/}`, `docs/.vitepress/{dist,cache}`, `docs/.vitepress/release.json`.

**`web/dist/.gitkeep`** is committed. `//go:embed all:dist` needs at least one file, so `go build`, `go test` and
`go vet` work on a fresh clone without Node. The server shows a plain "The web app is not built: run task build:web"
page when `index.html` is missing (`04`/`05`).

### 7.3 Dev config: `deploy/dev/isshoni.dev.toml` and `isshoni.e2e.toml`

Both files use 04's keys (04 §4.3):

```toml
# deploy/dev/isshoni.dev.toml                      # deploy/dev/isshoni.e2e.toml differs where noted
public_url = "http://localhost:5173"              # e2e: "http://127.0.0.1:18080" (SPA embedded; default only)
data_dir   = ".dev/data"                          # e2e: set per server by ISSHONI_DATA_DIR (a fresh temp dir)

[listen]
http    = "127.0.0.1:8080"                        # e2e: "127.0.0.1:18080" (default only, like the ICE ports)
ice_udp = ":7882"                                 # e2e: ":17882" (so a manual e2e run fits next to task dev)
ice_tcp = ":7882"                                 # e2e: ":17882"
admin_socket = ".dev/admin.sock"                  # e2e: the fixture passes --listen.admin-socket per server
                                                  # (<os tmpdir>/isshoni-e2e-<pid>-<n>.sock: macOS limits socket
                                                  # paths to 104 bytes)

[tls]
mode = "off"

[network]
include_loopback = true                           # S4's -loopback: same-machine ICE
stun_servers = []                                 # no STUN in dev/CI; public_ip stays unknown, which off mode allows

[log]
format = "text"                                   # e2e: "json" (so CI artifacts can be parsed)

[updates]
release_check = false
```

- The public URL is the Vite origin in dev, so the WebSocket Origin check (01) and the REST CSRF check (03) see the
  browser's real origin through the proxy, and 04's Host check accepts `localhost` because the site is in dev mode
  (off mode on a loopback listener).
- 04's validation accepts an `http://` public URL only for loopback hosts (`localhost`, `127.0.0.1`, `[::1]`).
- `.dev/data` and `.dev/` are created by the server on first start (04 §5.1); nothing needs to be created by hand.
- **The e2e file's ports are defaults only.** `web/e2e/global-setup.ts` only finds the binary; it starts no server.
  05's `startServer` fixture (05 §19.3) starts every e2e server with `--config deploy/dev/isshoni.e2e.toml` plus, per
  server: free ports chosen in Node, passed as `--listen.http 127.0.0.1:<p>`, `--listen.ice-udp :<u>`,
  `--listen.ice-tcp :<t>` and `--public-url http://127.0.0.1:<p>`; a fresh temp data dir (`ISSHONI_DATA_DIR`); and
  its own `--listen.admin-socket <os tmpdir>/isshoni-e2e-<pid>-<n>.sock`. Parallel workers, specs with their own
  server and a running `task dev` therefore never share a port, a data dir or a socket. The TOML values only matter
  for a manual `isshoni serve --config deploy/dev/isshoni.e2e.toml`.

localhost is a secure context, so `getDisplayMedia`, service workers and Secure cookies work in Chrome, Edge and
Firefox.
- **Safari and phones need HTTPS.** Test them against a VPS install of a prerelease (`--version 0.x.y-rc.N`) rather
  than adding TLS to the dev setup.
- **macOS Local Network privacy** (S4 finding 6) can block a browser's LAN ICE candidates. Dev uses loopback, which
  isn't affected.

### 7.4 Contributor start in 5 minutes (`CONTRIBUTING.md`)

```
git clone https://github.com/MoonWX/isshoni && cd isshoni
mise install              # Go, Node, Task, linters from .tool-versions (or install Go 1.26, Node 26, Task 3 yourself)
task setup                # npm ci, go mod download, pinned Go tools
task dev                  # server on :8080 + Vite on :5173
task dev:setup-url        # in a second terminal: open the printed link, create the admin
```

`CONTRIBUTING.md` also covers:
- before pushing: `task lint test`, plus `task e2e` after `task setup:e2e`;
- the protocol rule: edit Go types in `internal/protocol`, then `task gen` (`01`);
- the license rule: section 8.3;
- that `spikes/` is throwaway and not built by CI.

## 8. CI (GitHub Actions)

### 8.1 Workflows

| File | Triggers | Purpose |
|---|---|---|
| `.github/workflows/ci.yml` | `pull_request`, `push` to `main`, `merge_group` | gate for every change |
| `.github/workflows/nightly.yml` | `schedule: '17 3 * * *'`, `workflow_dispatch` | slow and flaky-prone checks (8.5) |
| `.github/workflows/release.yml` | `push` tags `v*`, `workflow_dispatch` (dry run) | section 9 |
| `.github/workflows/site.yml` | `push` to `main` (paths `docs/**`), `workflow_dispatch`, `workflow_call` | section 10 |
| `.github/dependabot.yml` | weekly | `gomod` (`/`, `/tools`), `npm` (`/web`, `/docs`), `github-actions`, and `docker` (`/deploy/docker`) once S68 adds the Dockerfile (Dependabot's update job fails on a directory without one); minor and patch updates grouped per ecosystem; `target-branch: m1/server-web` for every ecosystem while M1 lands there (README §4), switched to `main` after M1. The tools pinned in `.tool-versions` (Task, golangci-lint, shfmt) are ignored in `/tools` and bumped by hand. In `npm`, major updates of `eslint`, `@eslint/js` and `typescript` are ignored until eslint-plugin-jsx-a11y and typescript-eslint allow them (05 §2); the file names the blocking peer ranges |

**Rules for every workflow:**
- Top-level `permissions: contents: read`; jobs raise permissions individually.
- Every third-party action is pinned by full commit SHA with a version comment.
- `persist-credentials: false` on checkout unless a step pushes.
- `concurrency: ${{ github.workflow }}-${{ github.ref }}` with cancel-in-progress for PRs.
- `timeout-minutes` on every job.
- **No local `gh` is needed for anything.** Secrets, environments, rulesets and Pages are set once in the GitHub web
  UI (8.6), and everything else is YAML in the repo.

### 8.2 `ci.yml` jobs

All jobs run on `ubuntu-24.04` unless noted.

Workflow-level `env: ISSHONI_VERSION: 0.0.0-ci.${{ github.run_number }}`. The `web` job's SPA (05's Vite config reads
it) and the `build` job's binary (Task's `VERSION`, 7.2) therefore carry the same **non-dev** version. 05's
`version.spec` e2e test needs a non-dev SPA: 01's stale-build path is only testable in a non-dev build.
install.sh reads the same name as the version to install (4.1), so the install.sh tests (`test:sh`, `distro`,
`docker-smoke`) unset every `ISSHONI_*` variable before they set their own.

The `changes` job (`dorny/paths-filter`) sets these outputs. On `push` to `main` every filter is true.
- `go`: `**/*.go`, `go.mod`, `go.sum`, `tools/**`, `internal/protocol/testdata/**`, `.golangci.yml`, `tygo.yaml`
- `web`: `web/**`
- `deploy`: `deploy/**`, `.github/workflows/**`, `Taskfile.yml`, `.tool-versions`
- `docker`: `deploy/docker/**`, `deploy/compose*.yaml`
- `release`: `.goreleaser.yaml`, `.github/workflows/release.yml`, `deploy/packaging/**`
- `site`: `docs/**` except `docs/PLAN.md` and `docs/m1/**`, plus `web/src/conntest/codes.json` (the anchor test,
  10.4)

| Job | Runs when | Steps and assertions | Timeout | Gates merge |
|---|---|---|---|---|
| `lint-go` | go | `golangci-lint run` (config below); `go mod tidy -diff`; `go vet` for GOOS darwin and windows | 10 | yes |
| `test-go` | go | `task test:go`; coverage uploaded as an artifact (no external service) | 20 | yes |
| `web` | always (`build` always needs its artifact) | `npm ci`, then `lint`, `typecheck`, `test -- --run`, `check:i18n`, `build`, `check:size` (05 §2). Uploads `web/dist` | 10 | yes |
| `protocol` | go or web | `task gen:check` (tygo drift). The golden fixtures (`01`) run inside `test-go` and `web` | 5 | yes |
| `licenses` | go, web or site | `task licenses` | 10 | yes |
| `build` | always | downloads `web/dist`, `task notices`, `task build:go` (no Node on this runner); `bin/isshoni version --json` must show the commit and `0.0.0-ci.<run>`; warns above 60 MB. Uploads `bin/isshoni` | 10 | yes |
| `e2e` | go or web | setup-node and `npm ci` in `web/`; downloads the binary and runs `chmod +x bin/isshoni` (artifacts lose the mode bit); env `ISSHONI_BIN: ${{ github.workspace }}/bin/isshoni`; the runner's preinstalled Google Chrome stable (`npx --prefix web playwright install chrome` only if it is missing); starts a PulseAudio null sink (Chrome needs an output device for tab audio; drop this step if slice S4 shows it isn't needed); `xvfb-run -a npm --prefix web run e2e`. On failure uploads the Playwright trace, video and the per-server JSON logs (`web/test-results/server-*.log`, 14) | 20 | yes |
| `lint-deploy` | deploy | `task lint:sh lint:actions lint:keys lint:unit lint:pins test:sh` | 10 | yes |
| `distro` | deploy or release | matrix of the 6 container distros (4.13), scenarios from 11.2; setup-node and `npm ci` in `web/`, because `task release:snapshot` builds the SPA (7.2) | 30 | yes |
| `docker-smoke` | docker, deploy or release | `task docker:smoke` (11.3), linux/amd64 | 15 | yes |
| `goreleaser-check` | release | setup-node and `npm ci` in `web/`; `goreleaser check`; `task release:snapshot` (builds the SPA first, 7.2); asserts the asset list of section 3 (names and archive contents) | 20 | yes |
| `site` | site or deploy | `vitepress build`; setup-go (from `go.mod`), then `go test ./internal/server/ops/doctor -run TestDocsAnchors` (10.4) | 10 | yes |
| `govulncheck` | go | `.bin/govulncheck ./...`, `continue-on-error: true` (new CVEs mustn't block unrelated PRs; nightly and release gate on it) | 10 | no |
| `ci-ok` | always (`if: always()`) | fails if any job in `needs` is `failure` or `cancelled`; `skipped` counts as success | 2 | **the only required check** |

**`.golangci.yml`** (v2 format):
- linters: the `standard` set plus `bodyclose`, `errorlint`, `gosec`, `misspell`, `nilerr`, `noctx`,
  `sqlclosecheck`, `rowserrcheck`, `unconvert`, `usestdlibvars`, `depguard`, `forbidigo`;
- formatters: `gofmt`, `goimports` with local prefix `github.com/MoonWX/isshoni`;
- `depguard`:
  - `internal/protocol` must not import `internal/server/...`;
  - **Later (M2)**: `internal/client` may import only Pion, `x/net` and `internal/protocol`;
- `forbidigo`:
  - `fmt.Print*` and `log.*` in `internal/server/...` (use slog);
  - `SetNAT1To1IPs` (the plan says `SetICEAddressRewriteRules`).

### 8.3 License gate

**Go.** For GOOS linux, darwin and windows:
`.bin/go-licenses check ./cmd/... --ignore github.com/MoonWX/isshoni --disallowed_types=forbidden,restricted,unknown`.
- Blocks GPL, AGPL and LGPL (LGPL can't be linked statically into our binary), and anything unidentified.
- "reciprocal" (MPL-2.0) passes the tool. Any MPL package must also be listed with a reason in
  `deploy/notices/exceptions.md`, which `tools/notices` checks.

**npm.** `web/scripts/licenses.mjs` (ours, about 100 lines; dev deps `spdx-expression-parse` and `spdx-satisfies`,
both MIT). Usage: `node web/scripts/licenses.mjs check|notices [--dir web|docs]`.
- **Paths.** The script finds the repo root from `import.meta.url` (`web/scripts/` → `../..`). `--dir` is relative to
  the repo root (default `web`), and `notices` always writes `<root>/web/dist/licenses.txt`. So it works the same
  from the root (Task) and from `web/` (05's `build` script).
- Package list:
  - `npm query '.prod'`: packages bundled into the SPA (shipped);
  - `npm query '*'`: all installed packages.
- **Shipped packages: allowlist.** `MIT`, `ISC`, `BSD-2-Clause`, `BSD-3-Clause`, `Apache-2.0`, `0BSD`, `Zlib`,
  `CC0-1.0`, `Unlicense`, `BlueOak-1.0.0`.
  - An SPDX `OR` expression passes if any branch passes; `AND` needs all branches.
  - A missing `license` field fails.
- **All packages, including dev: denylist.** Fails when the expression can only be met with `GPL-*`, `AGPL-*`,
  `SSPL-*` or `BUSL-*`.
  - LGPL dev tools are allowed: not shipped, and the plan's gate is GPL/AGPL.
- **Overrides** go in `web/licenses.overrides.json` (`{"name@version": {"license": "MIT", "reason": "…"}}`), for
  packages that state their license only in a file.
- Exit 1 lists every offending `name@version (license) ← dependency path`.
- `notices` mode writes `web/dist/licenses.txt`: each shipped package with name, version, SPDX id and the text of its
  `LICENSE*`/`LICENCE*`/`COPYING*`/`NOTICE*` files. `05` links it from the SPA's About/footer.

**Tools are not dependencies.** ShellCheck (GPL-3.0) runs as a separate CI program and is never linked or shipped. The
same holds for git and PulseAudio on runners. The "no GPL" rule covers code in the repo and everything we ship.

### 8.4 `THIRD_PARTY_NOTICES` (`tools/notices`)

`go -C tools run ./notices -root .. -pkg ./cmd/isshoni -goos linux,darwin,windows -npm ../web/dist/licenses.txt
-extra ../deploy/notices/extra.txt -version <v> -o ../THIRD_PARTY_NOTICES`

- **Go modules.** The union of `go list -deps -f '{{with .Module}}{{.Path}} {{.Version}}{{end}}' ./cmd/isshoni` for
  each GOOS (run with `cmd.Dir = root`), minus the main module. Each module's directory comes from
  `go list -m -json`.
- **License files.** `LICENSE*`, `LICENCE*`, `COPYING*` and `NOTICE*` from the module root, identified with
  `github.com/google/licensecheck` (BSD-3-Clause).
  - A module whose license isn't on the shipped allowlist, or on `exceptions.md`, fails the build: the second gate.
  - Apache-2.0 `NOTICE` files are reproduced, as the license requires.
- **Go runtime.** A "Go standard library and runtime (go1.27.x)" entry from `$(go env GOROOT)/LICENSE`, because the
  runtime is linked into the binary.
- **Output.** Plain text, sorted by module path, deterministic (no dates; `<v>` only in the header). Sections: Go
  modules, "Web client (bundled JavaScript)" (from `licenses.txt`), and "Adapted source code" (`extra.txt`, a
  hand-kept list for code adapted from Galene (MIT) and LiveKit (Apache-2.0), whose attribution also goes in the
  top-level `NOTICE`, per the plan; `02` adds entries when it adapts code).
- **Where it ships.** Every archive, the deb/rpm (`/usr/share/doc/isshoni/`) and the image. It is gitignored and never
  committed, so Dependabot PRs don't need regenerating.
- **Later (M2+):** desktop artifacts get their own notices (native list, FFmpeg/LGPL files).

### 8.5 `nightly.yml`

| Job | What | Notes |
|---|---|---|
| `distro-vm` | 6 VMs (4.13), the VM scenarios from 11.2, including Pebble ACME | skipped with a warning without `/dev/kvm` |
| `docker-multiarch` | image build and smoke on `ubuntu-24.04` and `ubuntu-24.04-arm` (native arm64 runners, free for public repos) | |
| `go-cross-os` | `go test -race ./...` on `macos-15` and `windows-2025` | darwin/windows server builds are shipped (experimental) |
| `govulncheck` | fails on reachable vulnerabilities; npm: `npm audit --omit=dev --audit-level=high` in `web/` | |
| `goreleaser-full` | `goreleaser release --snapshot --clean` including images (built locally) and SBOMs | catches drift of the dockers_v2/nfpm config |
| `site-links` | lychee over the built site, external links included | |

- GitHub e-mails the owner when a scheduled run fails.
- GitHub disables scheduled workflows after 60 days without repository activity. `CONTRIBUTING.md` notes this.

### 8.6 One-time GitHub setup (owner, in the web UI)

The owner does this once. It can't be scripted from the dev machine, because the local `gh` isn't logged in as the
owner.

1. **Default branch → `main`** (the GitHub default is currently `m0/spikes`). Milestone branches merge into `main` by PR.
2. **Ruleset "main"**:
   - require a pull request (0 approvals; solo);
   - require the status check `ci-ok`;
   - block force pushes and deletion;
   - bypass list: repository admin, for emergencies only.
3. **Ruleset "release tags"** on `refs/tags/v*`: only the admin may create them; nobody may update or delete them.
4. **Environment `release`**:
   - required reviewer: the owner, with "prevent self-review" **off** (solo);
   - deployment branches and tags: **tags `v*` only**;
   - secret `RELEASE_SSH_SIGNING_KEY` (section 9.4).
5. **Pages**:
   - Source: "GitHub Actions";
   - environment `github-pages` → deployment branches: `main` **and tags `v*`**, because the release workflow calls
     the site deploy from a tag ref.
6. **Actions settings**:
   - default workflow permissions "read repository contents";
   - "Allow GitHub Actions to create and approve pull requests" off;
   - fork PRs from first-time contributors need approval (the default).
7. **Security**: private vulnerability reporting, Dependabot alerts, secret scanning with push protection, and CodeQL
   "default setup" (Go, JavaScript/TypeScript). CodeQL isn't gating in M1.
8. **After the first release**:
   - set the package `ghcr.io/moonwx/isshoni` to **public**; new container packages start private;
   - check that it is linked to the repo (the `org.opencontainers.image.source` label does that).

## 9. Release

### 9.1 Versions and the release process

- **Versions.** One SemVer tag (`vX.Y.Z`, 0.x until 1.0) for server + web, as the plan says; desktop apps join the
  same tag in M2+. The protocol version is separate (`01`). Prereleases are `vX.Y.Z-rc.N`.
- **Before tagging** (manual checklist in `docs/m1/`, not published):
  1. `nightly` is green, including `distro-vm`, or those VM runs were done by hand.
  2. Manual real-VPS smoke for a minor release (the patch rule is below):
     - install.sh with a domain;
     - install.sh in IP mode (Enter at the question), started at least 4 days before tagging. Keep that server up
       until one automatic renewal has been seen: the 160 h certificate renews after about 80 h (04 §8.1–8.2), so
       `isshoni_tls_cert_not_after_seconds` advances and doctor `tls` is ok. On it, one iPhone Home Screen app
       receives a `push.test` (Account → Notifications) on the IP origin;
     - Docker bridge mode with real Let's Encrypt;
     - iPhone and Android viewers.

     A patch release only needs install.sh with a domain, plus one phone.
  3. Release-notes facts are ready: migrations yes/no, protocol bump yes/no, security fixes.
  4. **Release-prep PR** (final releases only; a `-rc.N` tag skips this step): on a branch from `main`, run
     `task release:compat VERSION=X.Y.Z` (7.2), commit `internal/protocol/testdata/compat/`, open a PR into `main`
     and merge it once `ci-ok` is green. Tag that merge commit. The release job can't commit the snapshot itself:
     `main` requires a PR and `ci-ok`, and Actions may not open PRs (8.6). Without the merged snapshot, release.yml's
     `build` job fails before goreleaser runs (9.3).
- **Tag and push** with plain git: `git tag -a v0.1.0 -m v0.1.0 && git push origin v0.1.0`. `gh` is not needed.
- **Approve.** The owner edits the draft notes on GitHub, then approves the `release` environment (works from the
  GitHub mobile app).

### 9.2 `.goreleaser.yaml` (goreleaser OSS v2, ≥ 2.12)

```yaml
version: 2
project_name: isshoni

before:
  hooks:
    # No npm here: checks that web/dist (built by the unprivileged `web` job, 9.3) is for this version,
    # then THIRD_PARTY_NOTICES and the stamped dist-extra/install.sh.
    - task release:prepare VERSION={{ .Version }}

snapshot:
  version_template: >-
    {{ if index .Env "ISSHONI_SNAPSHOT_VERSION" }}{{ .Env.ISSHONI_SNAPSHOT_VERSION }}{{ else }}{{ incpatch .Version }}-dev.{{ .ShortCommit }}{{ end }}

builds:
  - id: isshoni
    main: ./cmd/isshoni
    binary: isshoni
    env: [CGO_ENABLED=0]
    goos: [linux, darwin, windows]
    goarch: [amd64, arm64]
    flags: [-trimpath]
    ldflags:
      - -s -w
      - -X github.com/MoonWX/isshoni/internal/version.version={{ .Version }}
      - -X github.com/MoonWX/isshoni/internal/version.commit={{ .FullCommit }}
      - -X github.com/MoonWX/isshoni/internal/version.date={{ .CommitDate }}
    mod_timestamp: "{{ .CommitTimestamp }}"

archives:
  - id: server
    ids: [isshoni]
    name_template: "isshoni_{{ .Version }}_{{ .Os }}_{{ .Arch }}"
    formats: [tar.gz]
    format_overrides:
      - goos: windows
        formats: [zip]
    files:
      - LICENSE
      - NOTICE
      - THIRD_PARTY_NOTICES
      - README.md
      - deploy/systemd/isshoni.service
      - deploy/sysctl/60-isshoni.conf
      - deploy/firewall/ufw-isshoni
      - deploy/firewall/firewalld-isshoni.xml

nfpms:
  - id: packages
    ids: [isshoni]
    package_name: isshoni
    file_name_template: "{{ .ConventionalFileName }}"
    formats: [deb, rpm]
    homepage: https://moonwx.github.io/isshoni/
    maintainer: "<owner's GitHub noreply address>"
    description: Self-hosted screen and system-audio sharing for friend groups.
    license: Apache-2.0
    section: net
    bindir: /usr/bin
    contents: # as in section 5
      - { src: deploy/systemd/isshoni.service, dst: /usr/lib/systemd/system/isshoni.service }
      - { src: deploy/sysctl/60-isshoni.conf, dst: /usr/lib/sysctl.d/60-isshoni.conf }
      - { src: deploy/firewall/ufw-isshoni, dst: /etc/ufw/applications.d/isshoni, type: config }
      - { src: deploy/firewall/firewalld-isshoni.xml, dst: /usr/lib/firewalld/services/isshoni.xml }
      - { dst: /etc/isshoni, type: dir, file_info: { mode: 0755 } }
      - { src: THIRD_PARTY_NOTICES, dst: /usr/share/doc/isshoni/THIRD_PARTY_NOTICES }
      - { src: NOTICE, dst: /usr/share/doc/isshoni/NOTICE }
      - { src: LICENSE, dst: /usr/share/doc/isshoni/LICENSE }
    scripts:
      preinstall: deploy/packaging/preinstall.sh
      postinstall: deploy/packaging/postinstall.sh
      preremove: deploy/packaging/preremove.sh
      postremove: deploy/packaging/postremove.sh

checksum:
  name_template: checksums.txt
  algorithm: sha256
  extra_files:
    - glob: dist-extra/install.sh
    - glob: deploy/compose.yaml
    - glob: deploy/compose.host.yaml

sboms:
  - id: archives
    artifacts: archive              # syft → <archive>.sbom.json (SPDX JSON)

signs:
  - id: cosign-checksums            # keyless (GitHub OIDC); the ssh signature comes from the protected `sign` job
    cmd: cosign
    artifacts: checksum
    signature: "${artifact}.sigstore.json"
    args: [sign-blob, "--bundle=${signature}", "${artifact}", "--yes"]

dockers_v2:
  - id: server
    ids: [isshoni]
    dockerfile: deploy/docker/Dockerfile
    images: [ghcr.io/moonwx/isshoni]
    tags: ["{{ .Version }}"]        # floating tags move only after approval (9.3 publish)
    platforms: [linux/amd64, linux/arm64]
    extra_files: [deploy/docker/rootfs, LICENSE, NOTICE, THIRD_PARTY_NOTICES]
    sbom: true
    labels:
      org.opencontainers.image.title: isshoni
      org.opencontainers.image.description: Self-hosted screen and system-audio sharing
      org.opencontainers.image.source: https://github.com/MoonWX/isshoni
      org.opencontainers.image.licenses: Apache-2.0
      org.opencontainers.image.version: "{{ .Version }}"
      org.opencontainers.image.revision: "{{ .FullCommit }}"
      org.opencontainers.image.created: "{{ .CommitDate }}"

release:
  github: { owner: MoonWX, name: isshoni }
  draft: true
  replace_existing_draft: true
  prerelease: auto
  extra_files:
    - glob: dist-extra/install.sh
    - glob: deploy/compose.yaml
    - glob: deploy/compose.host.yaml
  header: |
    ## isshoni {{ .Version }}
    **Upgrade:** run the installer again, or `docker compose pull && docker compose up -d`.
    **Database migration:** TODO yes/no   **Protocol:** v1 (TODO bump?)   **Security fixes:** TODO
    <!-- TODO: keep the next line only for a security release; 04's release check reads it -->
    <!-- isshoni:security -->
    Verify downloads: https://moonwx.github.io/isshoni/security#verify
  footer: |
    Release signing keys: https://moonwx.github.io/isshoni/security#keys

changelog:
  use: github
  sort: asc
  filters:
    exclude: ["^docs:", "^test:", "^ci:", "^chore\\(deps\\)"]
```

- **Snapshots go through `task release:snapshot`** (7.2), which builds the SPA with `ISSHONI_SNAPSHOT_VERSION` first.
  A bare `goreleaser release --snapshot` without it takes the `incpatch` fallback, and the before hook then stops at
  the `version.json` check, because no SPA was built for that version.
- **Verify in slice S9** (these could not be checked from docs alone):
  - that `dockers_v2.extra_files` keeps repo-relative paths in the build context. The Dockerfile relies on
    `deploy/docker/rootfs/`; if paths are flattened, change the `COPY` source;
  - the exact `snapshot` behavior of `dockers_v2`. Images are built only in the publish phase, so PR CI builds the
    image with plain `docker buildx build` and the same Dockerfile (11.3).
- **Images are signed in the workflow** (`cosign sign --yes ghcr.io/moonwx/isshoni@<digest>`), not with goreleaser's
  `docker_signs`. It is simpler and doesn't depend on how dockers_v2 names its artifacts.
- The release workflow checks that no `TODO` is left in the release notes before publishing (9.3 `publish`).

### 9.3 `release.yml`

```
tag v* ──► web ──► build ──► sign (Environment "release": owner approves) ──► verify ──► publish ──► site
                             draft release exists; nothing public except the exact image tag
```

The workflow computes the version once: the tag without `v`, or `0.0.0-dryrun.<run>` in a dry run. `web` and
`build` both use it.

**`web`** (unprivileged; the only job that runs npm)
- Permissions: `contents: read` only, no secrets, checkout with `persist-credentials: false`.
- Steps (the build half of ci.yml's `web` job; lint and tests already ran in CI): setup-node (from
  `.tool-versions`), `npm ci` in `web/`, then `npm --prefix web run build` with env `ISSHONI_VERSION=<version>` (so
  `web/dist/version.json` and `licenses.txt` are written); uploads `web/dist` as the artifact `web-dist`.
- Why a separate job: the web build runs third-party JavaScript (every npm dependency, bumped weekly by
  Dependabot). In `build` it would inherit goreleaser's `GITHUB_TOKEN` (contents, packages, OIDC). It could push
  images, swap draft assets together with `checksums.txt` before the owner signs, or mint OIDC tokens for cosign.
  Here it holds no write permission at all.
- Timeout 10 min.

**`build`**
- `needs: web`. Permissions: `contents: write`, `packages: write`, `id-token: write`, `attestations: write`.
- **No Node on this runner**: no setup-node, no npm, no npm cache. Only goreleaser and pinned Go tools run.
- Steps:
  1. checkout with `fetch-depth: 0`;
  2. download the `web-dist` artifact into `web/dist/`;
  3. setup-go (from `go.mod`), Task, syft, cosign v3, buildx, ghcr login with `GITHUB_TOKEN`;
  4. **compat snapshot check** (non-prerelease tags only; skipped for `-rc.N` and in a dry run): fails with "run task
     release:compat VERSION=<v> in a release-prep PR (9.1)" unless `internal/protocol/testdata/compat/<v>/` exists
     and `diff -r internal/protocol/testdata/v1 internal/protocol/testdata/compat/<v>` finds no difference (01
     §14.3). It runs before goreleaser, so a missing or stale snapshot creates no draft and pushes no image;
  5. `goreleaser release --clean`, which creates the **draft** release and pushes `ghcr.io/moonwx/isshoni:<v>`. Its
     before hook (`task release:prepare`, 7.2) runs no npm: it fails unless `web/dist/version.json` is `<v>`, then
     writes `THIRD_PARTY_NOTICES` (Go only, 8.4) and stamps `install.sh`;
  6. `actions/attest-build-provenance` for `dist/*.tar.gz dist/*.zip dist/*.deb dist/*.rpm dist/checksums.txt
     dist-extra/install.sh`;
  7. image digest via `docker buildx imagetools inspect ghcr.io/moonwx/isshoni:<v> --format '{{json .Manifest}}'`,
     then `cosign sign --yes ghcr.io/moonwx/isshoni@<digest>` and `attest-build-provenance` with that
     `subject-digest` and `push-to-registry: true`.
- Timeout 45 min.

**`sign`**
- `environment: release`; permissions `contents: write`. Runs only after the owner approves.
- Steps, with no third-party actions except checkout (sparse: `deploy/keys`):
  1. `gh release download "$TAG" --dir rel` (the draft is visible to `GITHUB_TOKEN`);
  2. `(cd rel && sha256sum -c checksums.txt)`, so every asset matches what is about to be signed;
  3. `umask 077; printf '%s\n' "$RELEASE_SSH_SIGNING_KEY" > "$RUNNER_TEMP/k"`;
  4. `ssh-keygen -Y sign -f "$RUNNER_TEMP/k" -n isshoni-checksums rel/checksums.txt`, then `rm -f "$RUNNER_TEMP/k"`;
  5. `ssh-keygen -Y verify -f deploy/keys/allowed_signers -I isshoni-release -n isshoni-checksums -s
     rel/checksums.txt.sig < rel/checksums.txt`, which catches a wrong key in the secret;
  6. `gh release upload "$TAG" rel/checksums.txt.sig`.
- Timeout 10 min.

**`verify`**
- Matrix: `debian-12`, `ubuntu-24.04`.
- Downloads the draft's assets into `mirror/v<v>/` and serves them with `tools/relserve` (HTTPS, test CA).
- In a systemd container, runs the **released, stamped `install.sh` with the production key** and
  `ISSHONI_DOWNLOAD_BASE`, `ISSHONI_PUBLIC_URL=http://127.0.0.1:8080` and `--tls-mode off --yes` (as C1 does), then
  asserts `/api/v1/info` reports `server.version` = `<v>`.
- Also:
  - `cosign verify-blob` on the checksums bundle (identity from 4.3);
  - `cosign verify ghcr.io/moonwx/isshoni:<v>` with the same identity and issuer;
  - `gh attestation verify` on one archive (`GH_TOKEN` is available in CI).
- Timeout 20 min.

**`publish`**
- Permissions: `contents: write`, `packages: write`.
- Fails if the draft body still contains `TODO`.
- `gh release edit "$TAG" --draft=false`, plus `--latest` unless it is a prerelease.
- Unless it is a prerelease:
  `docker buildx imagetools create -t ghcr.io/moonwx/isshoni:0.<minor> -t ghcr.io/moonwx/isshoni:latest
  ghcr.io/moonwx/isshoni:<v>`. Copying the index keeps the digest, so the cosign signature stays valid.

**`site`**
- `uses: ./.github/workflows/site.yml`.
- A `release` event created with `GITHUB_TOKEN` doesn't trigger other workflows, so the site deploy is called directly.

**Dry run** (`workflow_dispatch`, input `dry_run: true`):
- `web` builds the SPA as `0.0.0-dryrun.<run>`; `build` runs `goreleaser release --snapshot --clean --skip=publish`
  with `ISSHONI_SNAPSHOT_VERSION=0.0.0-dryrun.<run>`.
- A `sign-dry` job (**no environment**) signs with an ephemeral key made in the job and verifies with it.
- `verify` runs against those files with a test-stamped `install.sh`.
- This proves the pipeline before the first real tag.

### 9.4 Signing keys

| Key | What it signs | Where it lives | Verified by |
|---|---|---|---|
| `isshoni-release-1` (ed25519, no passphrase) | `checksums.txt` of every release | secret `RELEASE_SSH_SIGNING_KEY` in Environment `release` only | install.sh (embedded), the site workflow, users (`ssh-keygen -Y verify`) |
| `isshoni-release-backup-1` (ed25519, passphrase) | nothing until a rotation | **offline only** (password manager plus a printed copy); public half in `allowed_signers` | the same |
| Sigstore keyless (GitHub OIDC) | `checksums.txt` bundle and image digests | none: short-lived certificates | cosign (optional in install.sh) |
| GitHub attestations | provenance for archives, packages and images | GitHub | `gh attestation verify` |
| **Later (M2):** Ed25519 update-manifest key | desktop update manifests | the same Environment, a separate secret | desktop apps |
| **Later (M3):** macOS self-signed code-signing identity | the .app | the same Environment | macOS designated requirement |

**Generating the keys** (owner, once, on the Mac, never committed):
```
ssh-keygen -t ed25519 -N '' -C isshoni-release-1 -f isshoni-release-1
ssh-keygen -t ed25519 -C isshoni-release-backup-1 -f isshoni-release-backup-1
```
- Then the two public keys go into `deploy/keys/allowed_signers` by PR (the format is in 4.3).
- The private `isshoni-release-1` goes into the environment secret, and the local copy is deleted.
- The fingerprints (`ssh-keygen -lf`) are published in `README.md` and on `/security`.

**Rotation (or a leak):**
1. Add the new key line to `allowed_signers`, and mark the old one `valid-before="YYYYMMDD"` (OpenSSH ≥ 8.8;
   all targets have it) or delete it on a leak.
2. Replace the secret.
3. Cut a patch release: the site's `install.sh` then carries the new block.

After a leak, the backup key signs until a new key exists, and a security advisory lists the affected versions.

### 9.5 What M1 ships: v0.1.0

- **Sequence:** `v0.1.0-rc.1` (and further rc's) for the exit test (section 12), then `v0.1.0` once it passes.
- **Ships:**
  - server binaries: Linux amd64/arm64 are supported; darwin and windows are experimental;
  - deb and rpm (amd64, arm64);
  - the multi-arch image;
  - `install.sh`, `compose.yaml`, `compose.host.yaml`;
  - checksums with ssh and cosign signatures, SBOMs, provenance, `THIRD_PARTY_NOTICES`;
  - the project site.
- **Protocol:** `1`.
- **Known limits in the notes:**
  - no desktop apps yet (M2 Windows, M3 macOS, M4 Linux);
  - a whole-screen share from the browser includes voice apps (the SPA warns);
  - iOS: foreground only, tap to unmute;
  - a fresh Firefox may need about a minute to fetch OpenH264 before the first join (S4);
  - DRM video shows black.
- **Not in M1:** update-manifest signing, installers, SignPath (apply after the first Windows release, M2), Homebrew
  tap (M3), signed apt/rpm repositories (Later).

## 10. Project site

### 10.1 Hosting and URLs

- VitePress (MIT, pinned in `docs/package-lock.json`) with `srcDir: docs`, `base: '/isshoni/'` and
  `srcExclude: ['PLAN.md', 'm1/**']`. Design docs stay on GitHub, not on the site (open question Q3).
- **Site URL:** `https://moonwx.github.io/isshoni/`. Adding a custom domain later keeps these URLs working, because
  GitHub Pages redirects `github.io` to the custom domain.
- **Stable install URL:** `https://moonwx.github.io/isshoni/install.sh`. Mirror:
  `https://github.com/MoonWX/isshoni/releases/latest/download/install.sh`.
- **Privacy on the site:**
  - VitePress local search, not Algolia;
  - fonts bundled by the default theme, no Google Fonts;
  - no analytics, no cookies, no external embeds.

  The privacy page says that GitHub hosts the site and may log IPs.

### 10.2 Pages in M1

| Path | Content |
|---|---|
| `/` | What isshoni is (one paragraph plus screenshot), "Install in 2 minutes" (one-liner and Docker), the "voice apps are kept out" pitch with an honest "desktop apps coming" note, links |
| `/install/` | Shell install: the one-liner; what the script does, step by step; every flag and env var (4.1); non-interactive and cloud-init use; upgrade; uninstall/purge; **manual verification** (10.3); supported distros (4.13) |
| `/install/docker` | compose (bridge) with `.env` (an empty or missing value means unset: IP certificate, STUN detection); host-network alternative and switching modes (6.3); host sysctls; ufw bypass; bind-mount ownership; rootless notes; setup-url, doctor, backup, restore (`restore --yes -`), offline restore, logs, upgrade (6.4); a link to `/install/reverse-proxy#docker` for hosts whose 80/443 are taken; the Let's Encrypt notice: "isshoni gets its certificate from Let's Encrypt. Using it means you accept the Let's Encrypt Subscriber Agreement: https://letsencrypt.org/repository/" |
| `/install/vps` | Choosing a VPS (CPU, RAM, **transfer**, with the plan's bandwidth example and formula); ports table (section 2); per-provider firewall steps with stable anchors (10.4) |
| `/install/reverse-proxy` | `tls.mode=off` behind Caddy, nginx and Traefik: WebSocket upgrade for `/ws`, trusted `X-Forwarded-*` CIDRs (a proxy on the same host needs no setting: with a loopback `listen.http`, `127.0.0.0/8` and `::1/128` are trusted by default, 04 §8.5), and that **7882/udp and 7882/tcp must still be reachable directly**. A Docker section (`#docker`) with the compose file and notes of 6.5 |
| `/install/tls` | The four TLS modes: auto (domain), ip (6-day Let's Encrypt IP certificates, GA since 2026-01), manual, off (needs `ISSHONI_PUBLIC_URL`, 4.1); why self-signed is not supported (plan); for auto and ip, the Let's Encrypt notice: "isshoni gets its certificate from Let's Encrypt. Using it means you accept the Let's Encrypt Subscriber Agreement: https://letsencrypt.org/repository/" |
| `/guide/` | For friends: joining with an invite, with the chat-app tip "Opened from a chat app? Use its ⋯ menu → Open in Safari/Chrome first" (a chat app's built-in browser has its own cookie jar and no Add to Home Screen; 05's in-app banner); watching (focus, audio follows focus, fullscreen, tap to unmute), sharing from Chrome/Edge ("window + its audio"; the whole-screen warning), phones (Add to Home Screen, notifications, iOS limits) |
| `/troubleshooting` | Sections for every doctor check and every connection-test result (10.4), plus: certificate not issued; UDP blocked (ICE-TCP 443 still works); CGNAT/home server; Firefox's first join (OpenH264 download, S4); macOS Local Network permission (S4 finding 6); iOS tap to unmute; DRM shows black; "Copy diagnostics" **Later (M5)** |
| `/privacy` | The plan's "Privacy and trust model", word for word in substance, plus the **server's outbound connections exactly as 04 §16's table** (destination, when, what is sent, off switch), rendered in full: including STUN to Cloudflare and Google every 10 minutes, the ACME-directory clock check, and the four push services (payloads encrypted, RFC 8291). 04 §16 is the single source; the page adds nothing and drops nothing. Also what the installer contacts (the site and GitHub) and what the site stores (nothing) |
| `/code-signing` | Code-signing policy (below) |
| `/security` | Reporting a vulnerability (GitHub private vulnerability reporting); supported versions (latest minor); release signing keys and fingerprints (`#keys`); **how to verify** downloads, images and provenance (`#verify`) |
| `/reference/config`, `/reference/cli` | Hand-written from `04`'s key and command tables in M1. **Later (M5):** generated by an `isshoni docs` command |
| `/download` | **Later (M2).** Desktop installers; the M1 site says "coming" |

**`/code-signing` content** (what SignPath Foundation asks for; needed in M1 so the M2 application can start right
after the first Windows release):
- **What is signed and how:**
  - M1 release checksums: ssh-keygen, key in a protected environment, owner approval per release;
  - images: Sigstore keyless;
  - M2 Windows binaries and installer: "Free code signing provided by SignPath.io, certificate by SignPath
    Foundation";
  - M3 macOS: a stable self-signed identity (plan), with its certificate fingerprint published.
- **Roles:**
  - committers and reviewers: `@MoonWX`;
  - approvers of every signing request: `@MoonWX`.
- **Build rules:**
  - only artifacts built by GitHub Actions from a tagged commit of this repository are signed; local builds never
    are;
  - owner accounts use MFA;
  - third-party binaries (FFmpeg DLLs, M2) are shipped unmodified and **not** signed by us.
- **Privacy statement:** "isshoni does not send any information to other networked systems unless the user or the
  person running the server asks for it", followed by the exact exceptions from `/privacy`.
- **Contact:** where to report a signed binary that misbehaves.

### 10.3 Publishing `install.sh` and pinning its key

- **What the site serves.** `site.yml` never serves `deploy/install.sh` from `main`. It serves the latest release's
  stamped copy:
  1. `gh release download --pattern install.sh --pattern 'compose*.yaml' --pattern 'checksums.txt*'` for the latest
     release;
  2. `ssh-keygen -Y verify` against the repo's `deploy/keys/allowed_signers`;
  3. `sha256sum -c --ignore-missing`;
  4. copy into `docs/public/`, plus `deploy/keys/allowed_signers` → `docs/public/keys/allowed_signers`;
  5. write `docs/.vitepress/release.json`:
     `{"version":"0.1.0","installShSha256":"…","keyFingerprints":["SHA256:…","SHA256:…"]}`, which the install and
     security pages render.
- **Before the first release** there is no `install.sh` on the site, and the install page says "not released yet".
- **An urgent installer fix** means a patch release. That keeps the served script signed and paired with a binary.
- **The key is pinned in four places, cross-checked:**
  - `deploy/keys/allowed_signers` (source of truth);
  - the block embedded in install.sh (checked by `lint:keys`);
  - the fingerprints on `/security` and in `README.md` (from `release.json` and the file);
  - the release notes footer.
- **Manual verification** (on `/install/` and `/security#verify`):

```
V=0.1.0; B=https://github.com/MoonWX/isshoni/releases/download/v$V
curl -fsSLO $B/install.sh -O $B/checksums.txt -O $B/checksums.txt.sig
curl -fsSL https://moonwx.github.io/isshoni/keys/allowed_signers -o allowed_signers   # compare fingerprints with README
ssh-keygen -Y verify -f allowed_signers -I isshoni-release -n isshoni-checksums -s checksums.txt.sig < checksums.txt
sha256sum --ignore-missing -c checksums.txt
sudo sh install.sh
```

### 10.4 Anchor contract (links from the product into the site)

The server and the SPA link to fixed anchors. A test fails the build when one is missing: **one Go test,
`TestDocsAnchors` in `internal/server/ops/doctor/docsanchors_test.go`**. It reads the doctor check ids from the
registry (`CheckIDs()`), the `CloudProvider` constants from `internal/protocol/api/conntest.go`, and the `ct-` codes
from `web/src/conntest/codes.json`, then checks the headings in `docs/troubleshooting.md` and `docs/install/vps.md`.
No Vitest test is needed in `docs/`. It lives in the doctor package because no other slice of its group touches that
package, and because `tools/` is a separate module that can't import `internal/` packages. It runs in `test-go`
(any Go change) and in the `site` job (8.2), so a docs-only or codes-only PR runs it too.

| From | Link | Anchor rule |
|---|---|---|
| doctor text output, admin dashboard (`04`) | `/troubleshooting#doctor-<check-id>` | one `{#doctor-<id>}` heading per id of `doctor.CheckIDs()`, the list `isshoni doctor --list-checks` prints (04 §13.1; 04 §13.2 ids, with underscores: `public_ip`, `udp_buffers`, …) |
| wizard connection test (`05`) | `/troubleshooting#ct-<code>` | one `{#ct-<code>}` heading per result code in `web/src/conntest/codes.json` (05 §14.2) |
| wizard fix text per provider (`05`), doctor text output (`04`) | `/install/vps#<provider-id>` | one heading per `CloudProvider` constant: 04's typed constants in `internal/protocol/api/conntest.go`, which `task gen` also writes to `web/src/protocol/api.gen.ts` for 05 (today `aws`, `gcp`, `azure`, `oracle`, `hetzner`, `digitalocean`, `vultr`, `linode`, `scaleway`, `ovh`, `alibaba`, `tencent`, `unknown`). The test collects the constants from the Go file with `go/parser`, not from a hand-kept list. The page may add sections the product never links to (`aws-lightsail`, `contabo`, `home`) |
| installer and docs | `/install/reverse-proxy`, `/install/reverse-proxy#docker`, `/install/vps`, `/security#verify`, `/security#keys` | fixed |

**`ct-` sections whose fix is not the firewall.** Most `ct-` sections point to the cloud and host firewall. This one
must not:
- `#ct-no_public_ip` (05 §14.2; every row ✗ while the server doesn't know its public IPv4): the probe answers carry no
  IPv4 address, so browsers can't reach the media ports whatever the firewall says. It happens with a domain or
  off-mode server in a Docker bridge or behind a router when STUN is blocked, or with `network.stun_servers = []` and
  no `public_ip`. Fix: set `public_ip = "<the server's public IPv4>"` in `/etc/isshoni/isshoni.toml`, then
  `sudo systemctl restart isshoni`; under Docker set `ISSHONI_PUBLIC_IP` in `.env`, then `docker compose up -d` (which
  recreates the container with the new env; `docker compose restart` would keep the old env). Then run the test again.

The Go base URL is `version.DocsURL = "https://moonwx.github.io/isshoni/"` (04 §15), one constant.

- **doctor's text output** prints the `/troubleshooting#doctor-<id>` and `/install/vps#<provider>` links (04 §13.1).
  Its `--json` output carries no links; 05 builds the same links from `id` and `env.provider`.

**`/install/vps` per provider.** Each section says where the cloud firewall is, whether it blocks by default, the
ports from section 2, and quirks. Known quirks to state (re-check each against the provider's current docs when the
page is written):
- **AWS EC2** (`#aws`; Lightsail has its own `#aws-lightsail` section): security groups block everything but SSH on a
  new instance; add IPv4 and IPv6 rules; egress is billed
  per GB, and a 2-hour, 5-person session is about 40 GB (section 12). "Before installing without a domain, attach an
  Elastic IP": the default public IPv4 changes on stop/start, and in IP mode that moves the server's address, which
  breaks every friend's link, installed app and push subscription.
- **AWS Lightsail:** separate IPv4 and IPv6 firewall tabs.
- **Google Cloud:** VPC firewall rules with target tags; the "Allow HTTP/HTTPS" boxes don't cover 7882. "Before
  installing without a domain, reserve a static external IP" (the default one is ephemeral, same effect as on AWS).
- **Azure:** an NSG on the NIC or subnet.
- **Oracle Cloud:**
  - the VCN security list or NSG, **and** the image's own iptables REJECT rule (the installer handles that, 4.9);
  - the free Ampere arm64 tier is a good fit.
- **Hetzner, DigitalOcean, Linode, OVH, Contabo:** no cloud firewall unless you add one.
- **Vultr:** some images enable ufw (the installer handles that).
- **Scaleway:** security groups.
- **Home server:** router port forwarding for 80, 443 and 7882 TCP+UDP; CGNAT (doctor detects it) means a VPS is
  needed. "Use a domain with dynamic DNS; IP mode breaks when your IP changes."

### 10.5 `site.yml`

- `build` job:
  1. checkout `ref: main`, so docs are always from main, even when called from a tag;
  2. setup-node, then `npm ci --prefix docs`;
  3. the release fetch and verification from 10.3 (skipped cleanly when no release exists);
  4. `npx --prefix docs vitepress build docs`;
  5. `actions/upload-pages-artifact` with `docs/.vitepress/dist`.
- `deploy` job: `environment: github-pages`; permissions `pages: write`, `id-token: write`;
  `actions/deploy-pages`.
- Pages sets `Cache-Control: max-age=600`, so a new `install.sh` is live within about 10 minutes of a release.

## 11. Deploy test details

### 11.1 install.sh unit tests: `deploy/test/install_unit.sh`

- **Harness.** POSIX sh that sources `install.sh` with `ISSHONI_INSTALL_SOURCED=1` and asserts with a small
  `assert_eq`. It runs under dash, `bash --posix` and `busybox sh`.
- **`parse_args`:** every flag, flag-over-env precedence, unknown flag → 2, `--ip` together with `--domain` → 2.
- **`detect_arch`:** `x86_64`, `amd64`, `aarch64` and `arm64` map correctly; `armv7l` → 4.
- **`normalize_domain`:** `HTTPS://Share.Example.com/x/` → `share.example.com`; `203.0.113.7` → IP mode;
  `2001:db8::1` → IP mode; `bad_name` → rejected; `exa mple.com` → rejected.
- **`version_cmp`:** `0.2.0-rc.1 < 0.2.0`, `0.10.0 > 0.9.9`, `rc.2 < rc.10`, and equal versions compare equal.
- **`resolve_version`:** parses `…/releases/tag/v1.2.3` → `1.2.3`. A stamped version wins over latest;
  `ISSHONI_VERSION=v1.0.0` → `1.0.0`.
- **`checksum_for`:** exact name match; `x.tar.gz` doesn't match `x.tar.gz.sbom.json`; a duplicate line fails.
- **`verify_release`** with an ephemeral key pair made in the test:
  - valid → 0;
  - one byte changed in `checksums.txt` → 3;
  - `.sig` missing → 3;
  - signed with another key → 3;
  - signed with the right key but namespace `file` → 3;
  - archive hash mismatch → 3.
- **`check_ports`:** parses recorded `ss` output samples (nginx on 443, isshoni on 443, nothing listening).
- **Key block:** it is identical to `deploy/keys/allowed_signers`.

### 11.2 Distro scenarios: `deploy/test/distro-test.sh`

**Setup.**
- `ISSHONI_SNAPSHOT_VERSION=0.0.1-ci.1` and then `0.0.1-ci.2` with `task release:snapshot`, which builds the SPA
  with each version first (7.2), so binary and SPA always match.
- Each snapshot's `checksums.txt` is signed with an ephemeral key.
- A test copy of `install.sh` is made with that key between the markers.
- `tools/relserve` serves `https://host.containers.internal:8443/download/v<ver>/…` and `/latest` → 302 `/tag/v0.0.1-ci.2`, with a
  generated CA. The container trusts it through `update-ca-certificates` or `update-ca-trust`.
- Once a real release exists, "N−1" also means the latest real release, installed with its own script and the
  production key.

**Container scenarios** (each asserts the exit code and the listed state):

| # | Scenario | Asserts |
|---|---|---|
| C1 | Fresh install of ci.1 with `--yes --tls-mode off` and `ISSHONI_PUBLIC_URL=http://127.0.0.1:8080` | exit 0; `systemctl is-active` = active; the process runs as `isshoni`; `CapEff` = `0000000000000400` (only `CAP_NET_BIND_SERVICE`); config `root:isshoni 640`; `/var/lib/isshoni` `isshoni:isshoni 700`; openssh-client was auto-installed; `GET /api/v1/info` → `server.version` = `0.0.1-ci.1`; `isshoni setup-url --json` gives a URL containing `/setup#` |
| C2 | Seed | the admin is created through `03`'s setup endpoint; `isshoni admin users list --json` has 1 admin |
| C3 | Re-run with the same version | exit 0; config sha256 unchanged; `ActiveEnterTimestamp` unchanged (no restart); no question asked (no TTY, and it still exits 0); the output says "Setup is already done" (repair ends with `setup-url`, 4.10) |
| C4 | Upgrade to ci.2 | exit 0; `/api/v1/info` `server.version` = ci.2; the seeded admin still logs in; `isshoni setup-url` exits 7 (admin exists); the output says "Upgraded 0.0.1-ci.1 → 0.0.1-ci.2" |
| C5 | Downgrade with `--version 0.0.1-ci.1` | exit 6; still ci.2 and active |
| C6 | Tampered mirror: checksums changed, `.sig` missing, archive swapped | exit 3 each time; binary sha256 and service state unchanged |
| C7 | Port conflict: a Python UDP socket on 7882 before a fresh install with C1's flags (second container); then the socket is closed and the installer run again with the same flags | first run: exit 7; the message names `python3`; `systemctl is-enabled isshoni` fails. Re-run: exit 0; no question; the firewall step runs (its output line is present); the unit is enabled and active; the output has a `/setup#` link |
| C8 | `--uninstall` | exit 0; binary and unit gone; `/etc/isshoni`, `/var/lib/isshoni` and the user kept |
| C9 | Reinstall after uninstall | exit 0; no question asked (config exists); the unit is enabled and active again; the output says "Setup is already done"; admin still exists |
| C10 | Backup, purge, install, restore | `isshoni admin backup --out /root/b.tar.gz` → purge (`--yes`) → nothing left (paths, user) → fresh install with `--tls-mode off` → `isshoni admin restore --yes /root/b.tar.gz` → the admin logs in |
| C11 | No systemd (plain `docker run debian:12`) | exit 4, message points to Docker |
| C12 | Non-root without sudo | exit 4 |
| C13 | Re-run after exit 5 (third container): a test drop-in `/etc/systemd/system/isshoni.service.d/zz-test.conf` with `ExecStartPre=/bin/false` makes every start fail; fresh install with C1's flags; then the drop-in is removed with `systemctl daemon-reload` (the admin's fix) and the installer run again | first run: exit 5 after the wait; the last output line is "Fix the problem above, then run this installer again."; the unit is enabled. Re-run: exit 0; no question; the unit is active; the output has a `/setup#` link |

**VM-only scenarios** (nightly):

| # | Scenario | Asserts |
|---|---|---|
| V1 | Everything in C1–C10 on real systemd | as above, plus the sysctl values applied (`sysctl -n net.core.rmem_max` = 8388608) |
| V2 | Fedora with SELinux enforcing | the service starts; `ausearch -m avc -ts boot` shows no isshoni denials; `ls -Z /usr/local/bin/isshoni` shows `bin_t` |
| V3 | ufw enabled first (Ubuntu), firewalld running (Fedora) | `--yes` adds the profile or service; `--uninstall` removes it; SSH is still reachable |
| V4 | Oracle pattern: the Ubuntu VM gets `netfilter-persistent` and a REJECT rule | rules inserted before the REJECT and tagged `isshoni`; they survive a reboot; uninstall removes them |
| V5 | ACME with Pebble plus `pebble-challtestsrv` on the runner. The VM uses `ISSHONI_TLS_ACME_CA` and `ISSHONI_TLS_ACME_CA_ROOT` (04's keys `tls.acme_ca`, `tls.acme_ca_root`), and challtestsrv resolves `isshoni.test` to the VM | `--domain isshoni.test --yes` is ready within 60 s; the certificate issuer is Pebble. IP mode (`--ip`) the same, if the pinned Pebble supports IP identifiers; otherwise IP mode stays on the manual release checklist |
| V6 | `systemd-analyze security isshoni` | score ≤ the recorded value + 0.2 |

### 11.3 Docker smoke: `deploy/test/docker-smoke.sh`

1. Build the image with `docker buildx build --platform linux/<arch> --load -f deploy/docker/Dockerfile` from a
   context laid out like goreleaser's (`linux/<arch>/isshoni` plus the extra files).
2. Make a test certificate with openssl (a throwaway CA). Manual TLS mode proves 443 binds without depending on ACME.
3. **Bridge:** `docker compose -f deploy/compose.yaml -f deploy/test/compose.smoke.yaml up -d`. The override sets the
   local image, `ISSHONI_TLS_MODE=manual`, the certificate mount and `ISSHONI_PUBLIC_IP=127.0.0.1`. Asserts:
   - health is `healthy` within 90 s;
   - `curl --cacert ca.pem --resolve smoke.test:443:127.0.0.1 https://smoke.test/api/v1/info` → 200 with the
     version;
   - `docker compose exec -T isshoni isshoni setup-url` prints `^https://smoke\.test/setup#.+`;
   - `docker compose down && up -d` → the admin still exists after seeding (`setup-url` exits 7);
   - backup → restore round trip with the exact commands of 6.4:
     `(umask 077; docker compose exec -T isshoni isshoni admin backup --out - > b.tar.gz)` → `stat -c %a b.tar.gz`
     prints `600`; then `docker compose exec -T isshoni isshoni admin restore --yes - < b.tar.gz` → exit 0; health
     is `healthy` again within 60 s; `setup-url` still exits 7 (the admin survived);
   - **client IP over IPv6** (the 6.4 warning; skipped with a note when the runner has no IPv6 loopback): log in as
     the seeded admin with `curl --resolve 'smoke.test:443:[::1]'` (`POST /api/v1/auth/login`), then
     `GET /api/v1/me/sessions` with that cookie. The `current` session's `lastIp` is the compose network's gateway
     (from `docker network inspect isshoni_default`), not `::1`: docker-proxy forwards IPv6 clients from that
     address. If a Docker update changes this, the test fails and 6.4 is updated;
   - `docker inspect` shows `ReadonlyRootfs: true` and `CapDrop: [ALL]`.
4. **Host:** the same with `compose.host.yaml`, plus `ss -ltnp` on the runner shows :443 owned by the container's
   process.
5. **No volume:** `docker run --rm <image>` exits 78 within 10 s and logs a message containing `/var/lib/isshoni`.
6. **No `.env`:** `deploy/compose.yaml` started from a directory without a `.env` file, with an override that only
   sets the local image. `ISSHONI_DOMAIN` and `ISSHONI_PUBLIC_IP` reach the container as empty strings. Asserts:
   - the container keeps running (no exit 78 over an empty value);
   - `docker compose exec -T isshoni isshoni config print --json` shows `domain` and `public_ip` with source
     `default`, not `env`, and the derived TLS mode `ip` (04 §4.2 empty-means-unset rule). No certificate is
     expected: the runner can't pass ACME.
7. **Healthcheck:** `docker inspect` shows a HEALTHCHECK whose command is `/usr/local/bin/isshoni healthcheck`.

## 12. M1 exit test run book

**Exit criterion (plan):** 5 friends finish a 2-hour session on a small VPS with no manual fixes; iPhone and Android
can watch.

### 12.1 Setup

- **Build:** the latest `v0.1.0-rc.N`, installed with its own release script:
  `curl -fsSL https://github.com/MoonWX/isshoni/releases/download/v0.1.0-rc.N/install.sh | sh -s -- --domain <domain>`.
- **VPS:**
  - 2 vCPU (shared is fine), 2 GB RAM, public IPv4, Ubuntu 24.04 or Debian 13;
  - at least 1 TB of transfer per month, and **no per-GB egress billing**;
  - a common €4–6/month class.
  - Expected egress (plan formula: Σ viewers × (focus + thumbnails × preview)): 5 viewers × (one ~8.3 Mbps focus with
    audio + 0.3 Mbps per thumbnail) ≈ 45 Mbps, whether 2 or 5 people share. That is about 40 GB for 2 hours.
    Ingress is ~8.3 Mbps per sharer.
- **Domain:** a subdomain with an A record set before the day. Choose DNS that allows quick changes.
- **Metrics:** on for the test only (`[metrics] enabled = true`, listening on `127.0.0.1:9469`, 04 §11.2), never
  exposed.

| Participant | Device | Role |
|---|---|---|
| A (owner, admin) | Windows 11, Chrome | sharer (Movie preset: a 1080p60 film clip with sound, "window + its audio"); starts the collector on the VPS, detached from the SSH session (12.3) |
| B | macOS, Chrome or Edge | sharer (game or video in a window) |
| C | iPhone, iOS ≥ 16.4, Safari → Home Screen app | viewer; on mobile data part of the time |
| D | Android, Chrome → installed app | viewer |
| E | Windows or Linux laptop, **Chrome** | viewer; **all outbound traffic to port 7882 blocked, UDP and TCP**, so ICE-TCP on 443 is the only path (like a hotel or office network that allows only 443). Blocking only UDP would leave TCP 7882, which the server offers too, and ICE may pick it |

- **E's firewall rules**, removed afterwards:
  - Windows (these cover IPv4 and IPv6):
    `New-NetFirewallRule -DisplayName isshoni-test -Direction Outbound -Protocol UDP -RemotePort 7882 -Action Block`
    and the same line with `-Protocol TCP`; remove both with `Remove-NetFirewallRule -DisplayName isshoni-test`.
  - Linux (one nft rule for both protocols and both IP families):
    `sudo nft add table inet isshoni_test`,
    `sudo nft add chain inet isshoni_test out '{ type filter hook output priority 0; }'`,
    `sudo nft add rule inet isshoni_test out meta l4proto '{ tcp, udp }' th dport 7882 reject`; remove with
    `sudo nft delete table inet isshoni_test`.
- **E uses Chrome, not Firefox.** A Firefox viewer switches the whole room to Constrained Baseline, which takes B's
  macOS Chrome share off hardware simulcast (S4 finding 4), and E shares at T+90 while Firefox is not a tested sharer
  (05). Firefox viewing stays with the separate M-FF-1 row of 05 §19.4's manual matrix, outside the exit test.
- Everyone talks on their usual voice app (Discord), as in real use.
- Friends agree to the test, and nothing is recorded except the metrics below.

**T−1 day pre-flight:**
1. `isshoni doctor` passes.
2. The wizard's connection test from A shows UDP ✓, TCP ✓ and the RTT.
3. Load pre-flight from a second machine: `isshoni-loadtest` (`02`) with 5 publishers × 5 subscribers for 15
   minutes. Pass: CPU under 40% of 2 vCPU, loss under 0.5%, RSS flat.
4. On the day, before T+0 (the T−5 row): with E's firewall rules in place, the admin dashboard must show E's
   connection as `tcp443`. If it doesn't, fix E's rules before starting.

### 12.2 Timeline

| Time | Action | What to record |
|---|---|---|
| T−15 min | A starts `tools/exittest collect` on the VPS, detached from the SSH session (12.3) | |
| T−5 | B and E sign up (A sent them the invite link beforehand). B starts a window share (a video in a window), so the others have something to watch from their first second. E has the firewall rules in place | the admin dashboard shows E's connection as `tcp443` (pre-flight 4); E's seconds from opening the link to video playing |
| T+0 | A posts the invite link in the voice chat; C and D long-press it → Open in Safari (C) / Chrome (D), not the chat app's built-in browser, and sign up there | per person: seconds from opening the link to video playing (link → sign-up form → Lounge → B's share plays → one tap to unmute), the plan's first-join journey |
| T+5 | A shares (Movie) | A's share, the newest, takes focus; audio and video OK for everyone |
| T+10 | Everyone switches focus between A's and B's shares | switching focus moves the audio (audio follows focus) |
| T+15 | C and D: Add to Home Screen and allow notifications; E allows notifications | C and D log in once inside the Home Screen app (it has its own cookie jar; an expected action, 12.3) |
| T+19 | C, D and E leave: C swipes the Home Screen app away, D closes the installed app, E closes the tab. Everyone waits at least 40 s, so the 30 s resume grace ends and they are no longer present in the room | |
| T+20 | A stops and restarts the share, at least 10 minutes after A's previous share start (T+5): `share.started` pushes skip people present in the room and are sent at most once per (room, sharer) per 10 minutes (04 §14.3–14.4) | C, D and E get "A started streaming": each person's delivery time. Each taps the notification and must land on A's share (through `?focus=`) within 10 s. If the timing slips, `push.test` (Account → Notifications) is the fallback check for delivery |
| T+25–60 | Watch together, normal use; E uses fullscreen and keyboard navigation | freezes longer than 2 s (who, when) |
| T+60 | A runs `sudo systemctl restart isshoni` (announced) | seconds until each client plays again, with no action (target ≤ 30 s) |
| T+70 | C switches Wi-Fi → mobile data, and back at T+80 | seconds until C plays again (target ≤ 10 s) |
| T+75 | A turns Wi-Fi off for 10 s | A's share resumes without re-picking (inside the 30 s grace) |
| T+85 | C locks the phone for 2 minutes, then unlocks | video plays again after unlock (iOS: foreground only), with at most one tap (an expected action, 12.3) |
| T+90–100 | Stress: every desktop participant (A, B, E) shares at once | CPU, egress, loss |
| T+100–120 | Normal watching | |
| T+120 | End; A stops the collector (`sudo systemctl stop exittest-collect`); everyone fills in the survey | |

### 12.3 Measurements

**`tools/exittest collect -o run.csv -metrics http://127.0.0.1:9469/metrics`** (Go, stdlib only, runs as root). A
starts it detached from the SSH session, because A drops Wi-Fi at T+75:
`sudo systemd-run --unit=exittest-collect --working-directory="$PWD" "$PWD/exittest" collect -o run.csv -metrics
http://127.0.0.1:9469/metrics` (or inside `tmux`). Every 10 s it records:
- the isshoni PID, re-resolved on every sample with `systemctl show -p MainPID --value isshoni`; a changed PID is
  written to `run.csv` as a `restart` row, so the T+60 restart is marked;
- isshoni CPU % (from `/proc/<pid>/stat` of that PID), RSS, threads and open fds;
- load average;
- NIC rx/tx bytes (`/proc/net/dev`);
- UDP `InDatagrams`, `OutDatagrams`, `RcvbufErrors`, `SndbufErrors` and `InErrors` (`/proc/net/snmp`);
- TCP `RetransSegs`.

Every 30 s it saves a `/metrics` scrape.

`tools/exittest summarize run.csv` prints the table below. It computes every rate and total per process (split at
the `restart` rows) and treats a counter that goes down as a reset, the Prometheus way (the increase after a reset is
the new value), because the saved `/metrics` counters start again at zero at T+60. The owner then adds the journal
excerpt (`journalctl -u isshoni --since …`, with warnings and errors only).

**Metrics read from the server** (names from 01 §18, 02 §13 and 04 §11.2):
- Go and process collectors (`go_goroutines`, `process_resident_memory_bytes`, `process_cpu_seconds_total`);
- `isshoni_participants`, `isshoni_shares`, `isshoni_sfu_downtracks` (gauges);
- `isshoni_sfu_ingress_bytes_total`, `isshoni_sfu_egress_bytes_total`, `isshoni_sfu_nack_served_total`,
  `isshoni_sfu_pli_sent_total`, `isshoni_sfu_layer_switches_total`, `isshoni_transfer_bytes_total` (counters);
- `isshoni_ws_resume_total{result}` (signaling reconnects and resumes);
- `isshoni_sfu_selected_transport{transport}` (udp, tcp443, tcp7882);
- the viewer summaries that clients report through `stats` (01 §8.11) and the hub exports:
  `isshoni_client_frames_decoded_total`, `isshoni_client_frames_dropped_total`,
  `isshoni_client_freeze_seconds_total`, `isshoni_client_packets_lost_total`, `isshoni_client_audio_samples_total`
  and `isshoni_client_audio_concealed_samples_total`. Per-person detail (E's transport, C's freezes) comes from the
  admin dashboard, which shows each connection's transport and last stats.

**Pass criteria.** Every row must pass:

| Measure | Target |
|---|---|
| Manual fixes (reloads, re-joins, server commands other than the scripted restart) | **0**. Expected actions are not manual fixes: see the list below the table |
| Session | 2 h; the server never restarts except at T+60 |
| Server CPU | average < 50% of 2 vCPU, peak < 80% |
| Memory | RSS growth from T+65 to T+120 < 20% (the process started at T+60, about 55 minutes; no leak); goroutines back to the T+85 baseline (taken before the stress) after it. T+5 → T+59, the first process, is reported as a second sample |
| UDP socket errors | `RcvbufErrors + SndbufErrors` = 0, or < 0.01% of datagrams |
| Recovery | after the server restart: all 5 play again in ≤ 30 s; after C's network switch: ≤ 10 s; A's share survives a 10 s drop |
| Viewing quality | total freeze time < 1% of watch time on desktop and < 3% on phones; no decode errors that need a reload |
| Phones | iPhone and Android: video + audio after one tap, Home Screen app installed, notification received, playing again after unlock |
| ICE-TCP | E's selected transport is TCP on 443 all session long |
| Survey (1–5) | median ≥ 4 for picture and sound; nobody reports audio out of sync |

**Expected user actions** (not manual fixes; recorded per person, with the time):
- one tap to unmute after each join (the first join, and the return through the notification at T+20);
- one login inside each Home Screen app (C's iOS app, D's Android app; the app has its own cookie jar, 05 §16.3);
- at most one tap after unlock at T+85 (05 §12.8: iOS may need it after an interruption).

Anything beyond these, such as a reload, a second tap, a re-join or a second login, counts as a manual fix.

**Survey** (5 questions):
1. Did you have to reload or fix anything?
2. Picture quality (1–5).
3. Sound quality and sync (1–5).
4. How many freezes longer than 2 s?
5. Did the notification arrive?

**Afterwards.**
- Results go into `docs/m1/exit-test/<yyyy-mm-dd>.md`: participants are named A–E, with no IPs, names or domains.
- Every failure becomes an issue.
- After fixes, the failed segment is repeated. A crash, a leak or any manual fix means repeating the full 2 hours on
  the next rc.

## 13. Test plan (summary)

| Level | Test | Asserts |
|---|---|---|
| Unit | `deploy/test/install_unit.sh` (11.1) | parsing, normalization, version order, checksum lookup, signature fail-closed cases, `ss` parsing, key block |
| Unit | `web/scripts/licenses.test.mjs` (Node's test runner: `task test:web` and the CI `licenses` job) | the SPDX `OR`/`AND` rules; a missing license fails; a GPL-only dev dep fails; `(MIT OR GPL-3.0)` passes; overrides apply |
| Unit | `tools/notices` golden test on a fixture module tree | deterministic output; Apache `NOTICE` included; a non-allowlisted license fails; the Go runtime entry is present |
| Unit | `tools/relserve` | `/latest` redirect format matches GitHub's; files served; the CA verifies |
| Static | `lint-deploy` | shellcheck, shfmt, actionlint, `systemd-analyze verify`, the key block, `.tool-versions` = `go.mod` toolchain |
| Integration | distro containers C1–C13 (11.2) | install, idempotency, upgrade, downgrade refusal, tamper refusal, port conflict, re-runs after exit 7 and exit 5, uninstall, purge, backup/restore |
| Integration | Docker smoke (11.3) | bridge and host bind 443 as designed, healthcheck, setup-url via exec, persistence, backup (a `0600` file) → restore through `exec -T` with `--yes`, an IPv6 client seen as the bridge gateway (6.4), no-volume refusal, start with no `.env` |
| Integration | `goreleaser-check` | the asset names and archive contents of section 3 |
| Integration | site | the build, and **anchor coverage**: `go test ./internal/server/ops/doctor -run TestDocsAnchors` (10.4: reads `docs/troubleshooting.md` and `docs/install/vps.md`; ids from the doctor registry, the `CloudProvider` constants and `web/src/conntest/codes.json`) |
| E2E | Playwright in CI (content from `05`) | the CI plumbing: a Chrome sharer → SFU → Chrome viewer; `framesDecoded > 0`; audio energy |
| E2E | release dry run (9.3) | build → sign → verify on snapshot artifacts with ephemeral keys |
| E2E | VMs V1–V6 (nightly) | SELinux, sysctl, real firewalls, Pebble ACME, security score |
| Manual | release checklist (9.1) | real Let's Encrypt (domain and IP), Docker, phones |
| Manual | M1 exit test (12) | the exit criterion |

## 14. Interfaces other docs rely on

- **Ports and paths:** the tables in section 2, exactly (ports 80, 443, 7882/udp+tcp, 127.0.0.1:9469; `/etc/isshoni/isshoni.toml` root:isshoni 0640;
  `/var/lib/isshoni` 0700; `/run/isshoni`; `/usr/local/bin/isshoni` or `/usr/bin/isshoni`; unit `isshoni.service`;
  user/group `isshoni`).
- **systemd environment of the server:** `User=isshoni`, `CAP_NET_BIND_SERVICE` only, `ProtectSystem=strict`
  (only `/var/lib/isshoni` and `/run/isshoni` are writable), `PrivateTmp`, `AF_NETLINK` allowed, `UMask=0077`,
  journald captures stdout.
  - stderr is not a TTY under systemd, so 04's `log.format = "auto"` writes JSON lines (04 §10).
  - `systemctl reload` sends SIGHUP (`ExecReload`, 4.5).
  - Exit 78 stops restarts.
  - `TimeoutStopSec=20`: graceful shutdown must finish in under 15 s.
- **Container environment:** image `ghcr.io/moonwx/isshoni`; binary `/usr/local/bin/isshoni`; uid 65532; env
  `ISSHONI_IN_CONTAINER=1`; data at `/var/lib/isshoni` (a mandatory volume); read-only root filesystem with `/tmp` and
  `/run/isshoni` as tmpfs; no config file unless mounted; compose service name `isshoni`; user-facing env
  `ISSHONI_DOMAIN` and `ISSHONI_PUBLIC_IP`.
- **Build:**
  - ldflags variables `github.com/MoonWX/isshoni/internal/version.{version,commit,date}` (04 §15; `version` without
    `v`; unset → `0.0.0-dev+<commit>` from the build info);
  - constant `version.DocsURL`;
  - `CGO_ENABLED=0`, `-trimpath`;
  - `web/dist/.gitkeep` committed; `task build` requires `web/dist/index.html`;
  - `ISSHONI_VERSION` (build time, read by 05's Vite config) always equals the ldflags `version` of the binary it is
    embedded in, for `task build`, `task dev`, e2e and goreleaser (one Task `VERSION`, 7.2).
- **npm scripts** required in `web/package.json`: `dev`, `build`, `lint`, `typecheck`, `test`, `e2e`, `check:i18n`,
  `check:size`; `build` must also write `dist/licenses.txt` (it runs `node scripts/licenses.mjs notices`). In
  `docs/package.json`: `dev`, `build`.
- **e2e contract:** CI and `task e2e` provide `ISSHONI_BIN` (the absolute path of the built binary with the embedded
  SPA), `CI=1`, Google
  Chrome stable, Xvfb and a PulseAudio null sink. 05's fixtures start the e2e servers (05 §19.3) with
  `deploy/dev/isshoni.e2e.toml` as the base config. No port is fixed: each server gets free ports, a fresh
  `ISSHONI_DATA_DIR` temp dir and its own admin socket through flags (7.3). On failure, Playwright writes its report
  to `web/playwright-report/`; each server's stdout and stderr are in `web/test-results/server-<worker>-<n>.log`.
- **Task names** (7.2) and the required check name `ci-ok`.
- **Reserved non-key env names.** These `ISSHONI_*` names are not config keys, and 04 ignores them without a warning
  (04 §4.2's reserved list):
  - install.sh: `ISSHONI_VERSION` (version to install), `ISSHONI_YES`, `ISSHONI_NO_FIREWALL`,
    `ISSHONI_DOWNLOAD_BASE`, `ISSHONI_INSTALL_SOURCED` (unit tests);
  - goreleaser: `ISSHONI_SNAPSHOT_VERSION`;
  - e2e: `ISSHONI_BIN`;
  - 05 dev and build: `ISSHONI_VERSION` (version stamped into the SPA), `ISSHONI_DEV_SERVER` (Vite proxy target);
  - 02's load test: `ISSHONI_LOADTEST_PASSWORD` (the `isshoni-loadtest` admin password).

  Any new `ISSHONI_*` name that is not a config key must be added to 04 §4.2's reserved list. 04's own env-only names
  (`ISSHONI_CONFIG`, `ISSHONI_IN_CONTAINER`, `ISSHONI_ALLOW_EPHEMERAL_DATA`) are read by 04 and are not on this list.
- **Tools:** `tools/go.mod` (tygo, go-licenses v2, govulncheck, Task, golangci-lint, shfmt); `.bin/` output;
  `tygo.yaml` at the repo root.
- **Release assets and names** (section 3), Docker tags (section 3), the signing namespace `isshoni-checksums` and
  principal `isshoni-release`, `deploy/keys/allowed_signers`, and the cosign identity
  `https://github.com/MoonWX/isshoni/.github/workflows/release.yml@refs/tags/v<v>` with issuer
  `https://token.actions.githubusercontent.com`.
- **Site URLs:** base `https://moonwx.github.io/isshoni/`; `/install.sh`, `/keys/allowed_signers`, `/install/`,
  `/install/docker`, `/install/vps#<provider-id>`, `/install/reverse-proxy`, `/install/tls`, `/guide/`,
  `/troubleshooting#doctor-<id>` and `#ct-<code>`, `/privacy`, `/code-signing`, `/security#verify` and `#keys`.
- **Provider anchors:** 04's detected ids (`aws`, `gcp`, `azure`, `oracle`, `hetzner`, `digitalocean`, `vultr`,
  `linode`, `scaleway`, `ovh`, `alibaba`, `tencent`, `unknown`), plus site-only sections `aws-lightsail`, `contabo` and
  `home`.

## 15. Depends on

**From `04-server-platform.md`** (agreed values; 04 §3, §4, §18 are normative)
- **CLI:**
  - `isshoni serve --config PATH`;
  - `isshoni version [--short|--json]`: `--short` prints e.g. `0.1.0`; `--json` includes `version` and `commit`;
  - `isshoni config init --path P [config flags]` with 04's flag names (`--domain`, `--public-ip`, `--public-url`,
    `--tls.mode`, `--listen.http`, `--tls.cert-file`, `--tls.key-file`, `--tls.acme-email`): writes a commented TOML
    file and refuses to overwrite (exit 7);
  - `isshoni healthcheck [--ready] [--wait DUR]`: exit **0** healthy/ready, **1** anything else (unreachable
    included; Docker reserves 2). It needs no arguments under systemd (as root) or in the container;
  - `isshoni setup-url [--qr] [--json] [--wait DUR]`: exit 0 printed, **4** server unreachable, **7** an admin already
    exists;
  - `isshoni doctor [--config P] [--json] [--only ids] [--list-checks]`: exit **0** no failures (warnings allowed),
    **5** at least one failure (`--strict`: also warnings). Check ids are 04 §13.2's (`dns`, `public_ip`, `clock`,
    …), and each has a troubleshooting anchor;
  - `isshoni admin backup --out F|-`, `isshoni admin restore F|- [--yes] [--offline]` (an archive or a pre-migration
    `.db` file), `isshoni admin users list --json`.
- **Exit code 78** from `serve` for every state a restart can't fix: invalid config, newer DB schema, failed
  migration, corrupt DB or secrets, container without a data volume, data directory or `secrets.json` not owned by or
  writable for the service user.
- **Env names** (04's mechanical rule `ISSHONI_` + key path):
  - `ISSHONI_CONFIG`; `ISSHONI_DATA_DIR` (default `/var/lib/isshoni`); env-only `ISSHONI_IN_CONTAINER` and
    `ISSHONI_ALLOW_EPHEMERAL_DATA`;
  - `ISSHONI_DOMAIN`, `ISSHONI_TLS_MODE`, `ISSHONI_PUBLIC_IP`, `ISSHONI_PUBLIC_URL`, `ISSHONI_TLS_CERT_FILE`,
    `ISSHONI_TLS_KEY_FILE`, `ISSHONI_TLS_ACME_EMAIL`;
  - test-only: `ISSHONI_TLS_ACME_CA`, `ISSHONI_TLS_ACME_CA_ROOT`.
- **Container TLS default:** domain set → `auto`, empty → `ip` (04's derivation everywhere).
- **Container data guard** (04 §5.1): `/var/lib/isshoni` must be a mount; exit 78 otherwise.
- **Admin socket.** `/run/isshoni/admin.sock` in both environments (systemd `RuntimeDirectory`; a tmpfs in compose).
- **Other runtime behavior:**
  - config missing is OK at the default path (the container);
  - `/api/v1/info` returns `server.version`;
  - media sockets request 8 MiB buffers, and doctor reports the effective size;
  - a 7882/tcp ICE-TCP listener in every TLS mode;
  - dev settings (7.3) are ordinary 04 keys; an `http://` loopback `public_url` is valid in off mode;
  - `/metrics` series for the exit test (12.3), from 01, 02 and 04;
  - no `sd_notify` (the unit is `Type=exec`); graceful shutdown in ≤ 10 s.
- **A permissive QR library** for `--qr` (`skip2/go-qrcode`, MIT).

**From `03-accounts-and-store.md`:** `POST /api/v1/auth/setup/complete` (with the token taken from the fragment, after
`#`, of `url` in `setup-url --json`) and
`POST /api/v1/auth/login` for seeding in C2, C4 and C10; the pre-migration backup file name
`backups/pre-<schema>-<ts>.db`; restore semantics (04 §12.4).

**From `05-web-client.md`:**
- `web/package.json` scripts (section 14); `vite.config.ts` proxy to
  127.0.0.1:8080 for `/api` and `/ws`;
- Playwright config with the Chrome channel, reading `ISSHONI_BIN`, and the tone-tab or canvas sharer (S4 finding 7:
  headless Chrome can't auto-accept `getDisplayMedia`);
- connection-test result codes in `web/src/conntest/codes.json`;
- a link to `/licenses.txt` (served from `web/dist`);
- `web/embed.go` (`//go:embed all:dist`) with 04 §9.5's content, and `web/dist/.gitkeep`: both come from 05's web
  scaffold (README S09), not from the repo skeleton slice.

**From `01-protocol.md`:** `tygo.yaml` (both packages) and the generator `internal/protocol/gen/tsregistry`;
golden fixtures run by `go test ./internal/protocol/...` and by Vitest; the fixture directory
`internal/protocol/testdata/v1` that `task release:compat` snapshots in the release-prep PR (7.2, 9.1) and release.yml
checks (9.3), and the compat test that decodes the snapshots (01 §14.3); the `stats` counters and `isshoni_client_*`
metrics for the exit test (01 §8.11, §18).

**From `02-sfu.md`:** `cmd/isshoni-loadtest` flags for the pre-flight (5×5, 15 minutes); SFU metric names; entries in
`deploy/notices/extra.txt` for any adapted Galene or LiveKit code.

## 16. Implementation slices

In order. Each slice is testable on its own. The integrated plan (`README.md`, ids `Sxx`) sequences them with the
other docs; the goreleaser config lands before the distro matrix there, because the distro tests install snapshot
releases. Rough effort for one person, about 2.5 weeks in total, spread across the
M1 timeline:
- S1–S4 early, so the other docs' work lands on a green CI;
- S5–S10 in the second half;
- S12 last.

| # | Slice | Scope | Acceptance | Depends on |
|---|---|---|---|---|
| S1 | Scaffolding and dev loop (1.5 d) | `LICENSE` (Apache-2.0; the repo has none yet), `NOTICE`, `.tool-versions`, `go.mod` toolchain, `tools/go.mod`, `Taskfile.yml`, `.golangci.yml`, `.gitignore`, `go.mod` `ignore ./web/node_modules` and `ignore ./docs/node_modules`, `deploy/dev/*.toml`, `CONTRIBUTING.md` (the build info package is 04's `internal/version`) | Fresh clone: `mise install && task setup` succeed; `task lint:go lint:pins test:go` pass on the empty module; `task --list` shows every task of 7.2 (tasks whose inputs don't exist yet exit 0 with a note); `.bin/tygo` is v0.2.21; `task build:go` stops with a clear message while `web/dist/index.html` is missing. (`web/embed.go` and `web/dist/.gitkeep` come with 05's web scaffold, README S09) | — |
| S2 | CI core (1.5 d) | `ci.yml` with `changes`, `lint-go`, `test-go`, `web`, `protocol`, `build`, `ci-ok`; `dependabot.yml`; actionlint; one-time GitHub setup (8.6, steps 1–3 and 6–7) | A PR with a gofmt error, a tygo drift or a failing Vitest test makes `ci-ok` fail; a clean PR passes in < 12 minutes; `main` can't be merged red | S1 |
| S3 | License gate and notices (1.5 d) | go-licenses job, `web/scripts/licenses.mjs` with tests, `tools/notices` with golden test, `deploy/notices/*` | Adding a GPL-3.0 fixture dependency fails `licenses`; `THIRD_PARTY_NOTICES` lists every module in `go version -m bin/isshoni` plus the Go runtime and every prod npm package | S2 |
| S4 | e2e in CI (1 d) | `e2e` job: Chrome, Xvfb, Pulse null sink (keep only if needed), artifacts | `05`'s smoke test runs green; a forced failure uploads the trace, video and per-server logs | S2, 05 skeleton |
| S5 | Unit, sysctl, firewall, packaging files (1 d) | `deploy/systemd`, `deploy/sysctl`, `deploy/firewall`, `deploy/packaging` | `systemd-analyze verify` passes; in a Debian 13 VM a hand-placed binary starts as `isshoni`, binds 443, `CapEff` = 0x400; the security score is recorded in 4.5 | S1 |
| S6 | install.sh core (3 d) | 4.1–4.10 fresh-install path, 4.12 rules, unit tests (11.1), `tools/relserve`, `lint:keys` | All unit tests pass under the 3 shells; C1 and C6 pass in a Debian 12 container against a local test release | S5 |
| S7 | install.sh lifecycle and distro matrix (2.5 d) | upgrade, repair, downgrade, uninstall, purge, firewall offer, port check; `distro` CI job | C1–C13 pass on all 6 container distros in CI | S6, 03/04 CLI |
| S8 | Docker (1.5 d) | Dockerfile, rootfs, `compose.yaml`, `compose.host.yaml`, `docker-smoke` job | 11.3 passes on amd64 in CI and arm64 locally; the no-volume start is refused | S5, 04 container guard |
| S9 | goreleaser and release pipeline (2.5 d) | `.goreleaser.yaml`, `release.yml` (web, build, sign, verify, publish, dry run), keys generated by the owner, `allowed_signers`, environment `release` | The dry run passes end to end; tag `v0.1.0-rc.1` → draft → approval → signed → verified → published without moving `:latest` or GitHub's "latest"; `curl …/v0.1.0-rc.1/install.sh \| sh` works on a real VPS | S6, S8 |
| S10 | Project site (2.5 d) | VitePress skeleton, all M1 pages (10.2), `site.yml`, `release.json`, the anchor test `internal/server/ops/doctor/docsanchors_test.go` (10.4) and the `site` job's Go step, Pages settings (8.6 step 5) | The site is live; the served `install.sh` equals the latest release asset (sha256 test in `site.yml`); the anchor tests pass for the doctor ids, `ct-` codes and provider ids; no request leaves for a third-party origin (checked in the browser's network panel) | S9 (install.sh), 04/05 id lists |
| S11 | Nightly and VMs (2 d) | `nightly.yml`: VM matrix V1–V6 (Pebble), multi-arch smoke, cross-OS Go tests, govulncheck, full goreleaser snapshot, link check | One green nightly with all rows (or KVM rows marked skipped with the reason, and run by hand in UTM/Tart once) | S7, S8, S9 |
| S12 | Exit test (1 d prep + the session) | `tools/exittest` (collect, summarize), the run book as a checklist, the session, the results file | All pass criteria in 12.3 met on an rc; `v0.1.0` tagged from that commit (or a fixed one after a re-run) | everything |

## 17. Decisions taken at integration (formerly open questions)

- **Project site address**: `https://moonwx.github.io/isshoni/` for M1 (free, in line with the owner's
  budget-conscious stance); a custom domain can come later, and GitHub redirects the old URLs.
- **Merging into `main`**: pull requests only, with `ci-ok` required and an admin bypass for emergencies. M1 work
  lands on `m1/server-web` first (see `README.md`, "Branches and PRs").
- **Design docs on the site**: `docs/PLAN.md` and `docs/m1/` stay on GitHub only (`srcExclude`).
- **Docker image tag in `compose.yaml` during 0.x**: `:latest`; release notes flag migrations, and the server backs
  up before migrating. The docs show how to pin `:0.<minor>`.
