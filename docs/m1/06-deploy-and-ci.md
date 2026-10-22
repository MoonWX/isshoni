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
| D8 | install.sh tests: systemd **containers** on PRs (fast), **KVM VMs** nightly and before a release (SELinux, real firewalls, Pebble ACME) | Containers can't test SELinux labels, sysctls or firewalls; VMs are too slow for every PR. (Firewalls they can, in a privileged systemd container: README S78 runs scenario V3 there too, on every PR that touches deploy, 11.2) |
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
| `isshoni_<v>_{amd64,arm64}.deb`, `isshoni-<v>-1.{x86_64,aarch64}.rpm` | packages (section 5); file names from nfpm's `ConventionalFileName` with `-` where the package version has `~` (README S73, below), so `<v>` is the release's version here too: `isshoni_0.1.0-rc.1_amd64.deb` |
| `install.sh` | the installer, stamped with `<v>` |
| `compose.yaml`, `compose.host.yaml` | Docker Compose files (section 6) |
| `checksums.txt` | SHA-256 of every archive, package, `install.sh`, compose file and SBOM (`<hex>  <name>` per line) |
| `checksums.txt.sig` | `ssh-keygen -Y sign` signature, namespace `isshoni-checksums` |
| `checksums.txt.sigstore.json` | cosign v3 keyless bundle for `checksums.txt` |
| `*.sbom.json` | SPDX JSON SBOM per archive (syft) |

Also: images `ghcr.io/moonwx/isshoni:<v>` (multi-arch amd64+arm64), signed with cosign keyless, with a buildx SBOM
attestation, and GitHub build-provenance attestations for every archive, package and image digest.

**The assets as README S73 built them** (`.goreleaser.yaml`, 9.2; `task release:assets`, 7.2, checks every name
and archive after each goreleaser run: CI's `goreleaser-check`, `task release:snapshot` and release.yml's `build`):
- **Package file names have `-` where the package's version has `~`** (README §6 "Decided after group 9" (1)).
  nfpm gives a prerelease the package version `0.1.0~rc.1`, which dpkg and rpm order before `0.1.0`, and its
  conventional file names carry that `~`. GitHub renames a release asset whose name has a character other than
  `A-Z a-z 0-9 . _ -` (`~` becomes `.`), or that starts or ends with `.`, so the draft would hold
  `isshoni_0.1.0.rc.1_amd64.deb` while `checksums.txt` lists the `~` name, and the `sign` job's `sha256sum -c` would
  fail after the owner's approval (9.3). So the file name template replaces `~` with `-`
  (`{{ replace .ConventionalFileName "~" "-" }}`), and the package's own version keeps the `~` (the deb control's
  `Version: 0.1.0~rc.1`, the rpm's `%{VERSION}`), so a package manager still orders the rc before the release.
  `release:assets` checks both, and that every asset name is one GitHub keeps.
- **`checksums.txt` lists the SBOMs too**: goreleaser sums every file it uploads, the `*.sbom.json` included, and
  the `extra_files` (`install.sh`, both compose files) by their base names. A snapshot built without SBOMs (`task
  release:snapshot`) has none to list. `checksums.txt.sigstore.json` (goreleaser's cosign step) and
  `checksums.txt.sig` (the `sign` job) are not in it, and the `sign` job refuses a draft that holds any other asset
  that `checksums.txt` does not list.
- **Every archive ships the root `README.md`**, which also carries the release-key fingerprints (9.4, 10.3).
  README S73 took it by the pattern `README.m[d]` while the repository had none, so that CI's snapshots built
  meanwhile; release.yml's `build` job refuses a release without it, before a draft or an image exists. README F05
  adds the root `README.md` and makes the archive entry the plain `src: README.md`, so that goreleaser itself fails
  without it.
- **The archives' entries are owned by root and dated by the commit**, wherever the archive is built; install.sh
  unpacks with `--no-same-owner` and installs each file with its own owner and mode (4.4).

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

**As README S67 built the flow** (`deploy/install.sh`: `main`, `do_install`, `first_start`, `restart_or_repair`),
where it settles what the diagram leaves open:
- **No question without a terminal, said first.** On a machine without a config, with neither `--domain`, `--ip`,
  `--tls-mode` nor `--yes`, and no terminal to ask on, the script exits 2 ("Non-interactive install: pass
  --domain NAME or --ip") before it downloads or installs anything.
- **What is fetched.** The archive is downloaded and verified for another version, for `--reinstall`, **and when a
  file that comes from it is missing** (`payload_complete`: the unit; the sysctl file while a buffer limit is
  below 8 MiB; the ufw profile where ufw and its profile directory are there; the firewalld service file where
  `firewall-cmd` is). So a same-version run on a machine where a file went missing repairs it. The program is
  replaced only for another version or `--reinstall`; the unit, the sysctl file and the firewall profiles are
  installed from the archive whenever it was fetched, the unit only when it differs from the installed one. The
  user and the directories are ensured on every run.
- **`--no-start` on a started server** installs and leaves the running server as it is, and prints the command
  that restarts it (`sudo systemctl daemon-reload && sudo systemctl restart isshoni`).
- **Restart or not** (`STARTED`): the server is restarted when this run replaced the program or the unit, or when
  the running server reports another version than the installed program (an earlier run replaced the program and
  did not get to the restart); the port check runs before that restart. Otherwise it is only started when it is not
  running (with the port check first). The headline is "Upgraded IV → TV", "Downgraded IV → TV" with
  `--allow-downgrade`, or "isshoni TV is running." for a repair and a first start (4.10).
- **How a run ends when it is stopped**: Ctrl-C is "Cancelled." with exit 8; a kill (TERM) or a hangup (HUP, a
  dropped SSH session) is exit 1 with "Stopped by a signal while <step>"; a command that fails where no code of the
  script handles it is exit 1 with "The installer stopped at an unexpected error while <step>", never that
  command's own exit code, which could be read as one of the installer's. Every way out removes the download
  directory and a config that this run wrote and that did not get past the pre-check (4.8); the message always
  ends with "run this installer again: it goes on where it stopped".
- **The copy in the repository installs nothing yet**: its key block is empty until the release key exists (the
  owner creates it before README S73, §9.4) and is in `deploy/keys/allowed_signers` and in the block, which must
  equal each other (§4.3). Without a key the script says so before it downloads (exit 3: "a development copy").
  The tests put a key of their own between the markers (§11).
- The script sets `LC_ALL=C` (it reads what `ufw` and `ss` print) and adds the `sbin` directories to `PATH` (root's
  tools, which an ordinary user's `PATH` may lack).

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
  - **The reload as README S90 built it** (group 13; 04 §6.5): `serve` loads the config again from its own flags,
    the unit's environment and the file as it is now, reads the manual TLS files again and applies `log.level`.
    Every other key that changed is named in one WARN line in the journal ("the config has changes that a reload
    does not apply; restart isshoni to apply them", with the attribute `keys`), and a config with errors changes
    nothing and says that a restart would refuse it (exit 78). The server never stops for the signal; across a
    restart in place (a restore, `rotate-secrets`) the signal is ignored until the new process takes it again.
    `docker compose kill -s HUP isshoni` does the same in a container.
  - **Before S90 the server ignored SIGHUP** (decided after group 7; README S65 built the ignoring). As merged
    after group 7 `serve` did not handle the signal at all, so it ended the process, and systemd takes an end by
    SIGHUP for a clean exit that `Restart=on-failure` does not answer: a `reload` would have left the server
    stopped. From S65 to S90 `serve` took the signal, logged `reload is not supported yet; restart to apply
    changes` at INFO and went on serving. The unit kept the line throughout, so no file that the tarball and the
    packages ship (D3) had to change for the reload.
- **`ProtectClock=yes`** (and `@system-service`) also blocks the read-only `adjtimex` call, so the server-side doctor
  can't read the kernel's clock-sync flag. 04's `clock` check then treats the sync state as unknown and relies on the
  Date-header skew check (04 §13.2). Don't relax `ProtectClock` for this.
- **`ConfigurationDirectory` keeps the default mode 0755**: the config file itself is 0640 root:isshoni.
- **Target score**: `systemd-analyze security isshoni` should report an exposure of 2.5 or less. Slice S5 records the
  real number, and CI fails if a change raises it by more than 0.2.
  **Recorded: 1.7** (README S62, 2026-10-09), the same on systemd 249, 252, 255, 257 and 259 (Ubuntu 22.04 to
  Fedora 44), so V6 (§11.2) fails above 1.9. `deploy/systemd/README.md` has the table per distro and the reason
  for each of the 13 lines the report still marks.

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
- **As README S67 built it** (`apply_sysctl`): the installed file holds **only the keys whose running value is
  below 8 MiB**; a key that is high enough is left out of it, so the file can't lower that value at the next boot
  either, and when both are high enough no file is written. A value that can't be read counts as low. `sysctl -p`
  exits 0 even when the kernel refuses a value (in a container, say), so the script reads both values back
  afterwards and warns when one is still low: "Could not apply /etc/sysctl.d/60-isshoni.conf (larger UDP buffers
  for video). In a container that is expected. "sudo isshoni doctor" shows the sizes in use." The file comes from
  the release archive, so it is (re)written whenever a run fetched the archive, and a same-version run fetches it
  when the file is missing while a value is low (4.2).
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
| `firewall-cmd --state` is `running` | same text, "in firewalld" | `firewall-cmd --reload`, then `firewall-cmd --permanent --add-service=isshoni && firewall-cmd --reload` (the first reload is needed, below) |
| Neither, but `iptables -S INPUT` has a `-j REJECT`/`-j DROP` rule and `netfilter-persistent` exists (the Oracle Cloud Ubuntu image pattern) | same text, "in iptables" | Insert before the first REJECT/DROP, for IPv4 and IPv6, then `netfilter-persistent save`. Each rule is tagged `-m comment --comment isshoni` so uninstall finds it: `-p tcp -m multiport --dports 80,443,7882 -j ACCEPT` and `-p udp --dport 7882 -j ACCEPT` |
| None of these | nothing to open locally | |

- **An inactive ufw or firewalld is never enabled**, because that could lock the admin out of SSH.
- **firewalld needs a reload before `--add-service`** (README S62's finding, with firewalld 1.1 to 2.4). A
  running firewalld reads its service files when it starts and when it reloads. `isshoni.xml` was put in place
  moments ago (`install_fw_profiles`; by the package, for the commands its `postinstall.sh` prints, §5), so the
  daemon doesn't know a service of that name yet, and `--permanent --add-service=isshoni` is refused. `firewall-cmd
  --reload` first makes the name known; the second reload applies the permanent change to the running firewall.
- **Ask systemd before `firewall-cmd`** (the same finding): without a daemon to talk to, firewalld 2.4's
  `firewall-cmd` waits 10 seconds before it answers, on every Fedora machine where firewalld is installed and
  not running. So the detection is `systemctl is-active --quiet firewalld.service` first and
  `firewall-cmd --state` only then; the packaging scripts do the same.
- **Only this offer reloads firewalld; the deb and rpm packages never do on install or upgrade** (README §6
  "Decided after group 9" (2); §5). A reload drops every runtime-only rule the admin has, and the offer asks first
  (or runs with `--yes`, which the admin typed). A package's `postinstall.sh` is not asked: it prints `firewall-cmd
  --reload` before `--add-service=isshoni` for the admin to run (§5), and its `preremove.sh` reloads only after it
  took the service out of a zone or policy, which closes the ports the service opened (4.11).
- **Who builds the two in install.sh: README S67 did.** Both were written back here after group 7, while S67 was
  already building this offer, and S67 merged with both (`offer_firewall`): `systemctl is-active --quiet
  firewalld` comes before `firewall-cmd --state`, and the offer runs `firewall-cmd --reload`, `--permanent
  --add-service=isshoni` and `--reload` again, in that order; a "no" prints the same three commands for later.
  What README S78 still adds is their tests: the unit tests of §11.1 with stubbed `systemctl` and `firewall-cmd`,
  and scenario V3 (§11.2) on a real firewalld.
- **The rest of the offer as S67 built it**: a "no" leaves the firewall as it is and prints how to open the ports
  later; Ctrl-C or the end of input at the question is exit 8 ("Nothing was started"); a command of the firewall
  that fails is a warning with the command to run by hand, not an error. The iptables row also counts a `DROP`
  policy of the INPUT chain as blocking (the rules are then appended), opens ip6tables only where it blocks too,
  and leaves a chain alone that has the `isshoni` rules already. Without `ss` the port check is skipped with a
  warning (package `iproute2`).
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

**As README S67 built it** (`start_or_restart`, `wait_ready`, `not_ready`, `print_setup`), where it differs from
the text above or settles what it leaves open:
- **Restart.** An upgrade, a downgrade and `--reinstall` restart the server only when the program or the unit
  changed, or when the running server is another version than the installed program (4.2); such a restart, and
  the start of a server that is not running on a repair, come after `systemctl reset-failed isshoni` and after the
  port check (4.9). A first start whose `enable --now` fails ends with exit 1 only when the unit did not even get
  enabled; a start that fails otherwise is noticed by the wait.
- **Wait.** The wait also ends as soon as systemd has given up on the unit (`systemctl is-failed`), not only at
  its timeout. Its line every 10 s is "Waiting for the HTTPS certificate… (20 s)" in `auto` and `ip` mode and
  "Waiting for isshoni to be ready… (20 s)" otherwise, and the Let's Encrypt hint after 30 s is printed in `auto`
  and `ip` mode only.
- **On timeout** the output also names the provider-firewall page next to troubleshooting. After an upgrade, the
  way back names the newest `backups/pre-*.db` that the server wrote **during this run** (found with privilege,
  `find … -newer` a file the run made at its start): a backup of an earlier upgrade is older than this run, and
  restoring it would lose what was written since. Without one only the `--version <old> --allow-downgrade` step
  is printed.
- **The setup link after an upgrade too.** Every path that ends well prints its headline ("isshoni TV is
  running.", "Upgraded IV → TV" or "Downgraded IV → TV", 4.2) and then runs `setup-url`: the link while no admin
  exists, else "Setup is already done. Open <origin>/", with the origin from the running server's
  `isshoni admin status --json`, or from the config when no server answers. Any other exit of `setup-url` is the
  timeout's diagnostics and exit 5 (the server answered the readiness check a moment before and is gone). The
  link goes from the server to stdout and nowhere else: never into a file or a log.

### 4.11 Uninstall and purge

**`--uninstall`** (exit 0 even when parts are already gone):
1. `systemctl disable --now isshoni`.
2. Remove the unit file and run `daemon-reload`.
3. Remove `/usr/local/bin/isshoni` and `/etc/sysctl.d/60-isshoni.conf`. Running values are left until reboot.
4. Firewall:
   - ufw: `ufw delete allow isshoni` if present, then remove the profile;
   - firewalld: `--permanent --remove-service=isshoni`, reload, remove the XML. **As written this leaves
     policies and rich rules behind** (README S62's finding; below): the service must be taken out of every
     place that names it before the XML goes;
   - iptables: delete the rules tagged `isshoni` and save.
5. Print: "Kept: /etc/isshoni (config), /var/lib/isshoni (database, certificates, backups) and the isshoni user.
   Remove them with --purge. Before --purge, back up with `sudo isshoni admin backup --out <file>` while isshoni is
   installed and running (run this installer again to bring it back)." It can't offer a backup command for right
   now: at this point the service is stopped and the binary deleted, and an offline backup refuses to run as root
   (04 §12.6).

**firewalld: take the service out of every zone, policy and rich rule before removing its file** (README S62
found this for the packages, whose `preremove.sh` does it; `--uninstall` has the same step to make, README S78).
`--remove-service=isshoni` without a zone edits the default zone only. An admin may have added the service to
another zone or to a policy, or written a rich rule that names it (`rule family="ipv4" source address="…" service
name="isshoni" accept`), and uninstall as step 4 has it leaves those. Once `isshoni.xml` is gone they name a
service firewalld can't find, and firewalld then **rejects its whole permanent configuration**: the next reload
fails, and from its next start on it runs the stock configuration (state `failed`), without any of the admin's
own rules. So before the XML is removed:
- for every zone (`--get-zones`) and every policy (`--get-policies`) of the permanent configuration:
  `--remove-service=isshoni` where `--query-service=isshoni` says it is there, and `--remove-rich-rule=<rule>`
  for each rule of `--list-rich-rules` that contains `service name="isshoni"`. A removed rich rule is printed in
  full, so that the admin can add it again: it may hold addresses that are written down nowhere else;
- through the daemon when it runs (`firewall-cmd --permanent …`, then one `--reload`, which works because the
  daemon still knows the service at that point and which closes the ports), and on the files with
  `firewall-offline-cmd` otherwise, whose options are `--remove-service-from-zone` and
  `--remove-service-from-policy`;
- a zone or policy that firewalld refuses to change is named in a warning that says to remove the service there
  by hand, and the uninstall goes on;
- nothing else in the firewall is touched. ufw needs none of this: `ufw delete allow isshoni` removes the rule,
  and a rule that is left keeps working without the profile.

The packages skip the step when the admin has a definition of their own in `/etc/firewalld/services/isshoni.xml`,
which keeps the name valid when the package's file in `/usr/lib` is gone. For install.sh that path is the file
it removes, so it always scrubs first.

**As README S78 built it** (`deploy/install.sh`: `uninstall_steps`, `stop_service`, `remove_program`,
`remove_ufw`, `remove_firewalld`, `remove_iptables`, `do_uninstall`, `do_purge`), where the steps above leave a
choice:
- **A stop that fails removes nothing.** After `systemctl disable --now isshoni`, a service that is still active
  ends the run with exit 1, "Could not stop isshoni, so nothing was removed.", and points at
  `systemctl status isshoni`. Then the unit file goes, with `daemon-reload` and `reset-failed` (a unit that had
  failed would stay listed as "not-found failed", and the system "degraded", until the next boot). A
  `.isshoni.new` left by an unfinished install goes with the program.
- **The admin's own drop-ins stay** (`/etc/systemd/system/isshoni.service.d/`, from `systemctl edit isshoni`):
  they are the admin's, and the uninstall and the purge say where they are.
- **`--no-firewall` holds here too** (and `ISSHONI_NO_FIREWALL=1`, 4.1: "never touch the firewall"): the firewall
  is left as it is, with ufw's profile and firewalld's service file, which its rules may still name, and the
  output says how to remove them (`--uninstall` again without it).
- **ufw**: `ufw delete allow isshoni`, then the profile. ufw needs the profile to delete the rule, so a rule ufw
  does not delete keeps its profile, with a warning.
- **firewalld, a file prefilter.** Only a zone or a policy that was changed on this machine can name the service,
  and firewalld keeps those as files of their names below `/etc/firewalld/zones` and `policies`. So the scrub
  above runs only when one of those files mentions `isshoni`, and then only on the zones and policies whose files
  do: about a dozen slow `firewall-cmd` calls fewer than asking every one (the packages' `preremove.sh` does the
  same).
- **The files have the final word, and `isshoni.xml` stays while one of them names the service.** Whatever
  firewalld answered, the `*.xml` files of `zones/` and `policies/` are what it loads at its next reload or start
  (not the `.xml.old` copies it keeps of files it rewrote). When one of them still names the service (`name` =
  `isshoni` in any quoting and spacing; a word in a log prefix or a description doesn't count),
  `/etc/firewalld/services/isshoni.xml` is kept, with a warning that names the file and says to take the service
  out there by hand and then delete the service file. That covers a zone or policy that firewalld refused to
  change, a hand edit or a `firewall-offline-cmd` change that the running daemon has not read yet, `--get-zones`
  or `--get-policies` failing, and firewalld stopped without `firewall-offline-cmd` (then nothing is changed, and
  the warning says why). Removing the file then would leave firewalld unable to load its permanent
  configuration, which is what this section prevents; "the uninstall goes on" holds: everything else is removed
  and the run ends with exit 0. README S78's review reproduced the hand-edit case against a real firewalld.
- **iptables**: the rules tagged `isshoni` are deleted from `INPUT` of iptables and ip6tables (a warning with the
  command for a rule that won't go), then saved with `netfilter-persistent save` where it is installed.
- **Nothing left to remove** is exit 0 too ("isshoni is not installed here: there was nothing to remove."); the
  "Kept:" lines show only when the config, the data or the user is there.
- **`--purge`** asks before anything changes; without a terminal and without `--yes` it exits 2. It deletes
  `/etc/isshoni`, `/var/lib/isshoni` and `/run/isshoni`, then the user, and the group where `userdel` left it,
  each with a warning and the command when it fails.

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
- Started with `podman run -d --systemd=always --cap-add=SYS_ADMIN --cap-add=SYS_PTRACE` (the F16 bullet below), or
  `docker run -d --privileged --cgroupns=private --tmpfs /run --tmpfs /run/lock` (Docker Desktop works on macOS; CI
  uses Docker, the S78 bullet below). `CONTAINER_ENGINE` selects which.
- **As README S78 built it, CI uses Docker too**: the `distro` job sets `CONTAINER_ENGINE=docker`, with the same
  `--privileged`, `--cgroupns=private` and host-gateway setup that release.yml's `verify` job used until README
  F09, because the runner's podman is rootless and its containers reach the runner's `relserve` another way (the
  next bullet). The script still takes podman, and picks it when it is installed, but that path is not tested on
  CI.
- **Review R4** (the M1 security review; README §6 "Decided after group 10" (1)), as README F09 fixed it.
  release.yml's `verify` job ran such a privileged container, built from Docker Hub bases that no digest pinned
  (`debian:12`, `ubuntu:24.04`), while it held a `contents: write` token. Now (9.3) it holds a read-only token,
  builds the image with `podman build` from bases pinned by the digest of their multi-arch index
  (`docker.io/library/debian@sha256:…`, the tag in a comment; Dependabot doesn't read a workflow's matrix, so they
  are bumped by hand with the monthly tool pins of 7.1), and starts it rootless, with no `--privileged`:
  `podman run -d --systemd=always --cap-add SYS_ADMIN --network slirp4netns`. **`CAP_SYS_ADMIN`** is the
  container's, in its own user namespace, and gives nothing on the runner; systemd needs it for the unit's sandbox
  (`ProtectSystem=` and the rest are mount namespaces). Without it systemd skips the sandbox ("namespace setup is
  prohibited (container manager?)"), and with it the `StateDirectory=` bind mount, so the server, which sees a
  Podman container (`/run/.containerenv`), refuses `/var/lib/isshoni` as not a mounted volume (exit 78, 04 §5.1).
  A privileged Docker container never met this; `distro-test.sh`'s podman path gives the capability too (the next
  bullet). **How a rootless container reaches `relserve`**: through slirp4netns, which connects from the runner to
  the runner's own address, the one podman puts into the
  container's `/etc/hosts` as `host.containers.internal` (a name in relserve's certificate). So `relserve` listens
  on every address, and the installer's base is `https://host.containers.internal:<port>`. The runner's loopback
  address is not reachable that way (with `allow_host_loopback=true` too, podman 4.9 maps the name to the runner's
  address), and with pasta, the default network from podman 5 on, podman 4.9 gives the name no address that
  reaches the runner; so the network is named, although slirp4netns is the default of the runner's podman 4.9.
  F09 checked this with Ubuntu 24.04's podman 4.9.3, rootless in a container with a cgroup delegated to its user:
  the default and the named slirp4netns reach a server on every address of the host as `host.containers.internal`,
  pasta and the loopback address don't, and the image built from a digest boots systemd with `--systemd=always`.
  There, `verify`'s steps as release.yml has them (taken out of the file and run in order, in a dry run's mode,
  against a local snapshot) passed on both bases: systemd up, `relserve` reached, the installer's run, the service
  active and `<v>` reported. **A systemd user session**: rootless podman gives the container a cgroup that systemd
  in it may write to only through the runner user's own systemd (podman's systemd cgroup manager, over the user's
  D-Bus at `/run/user/<uid>/bus`). A job runs under the runner's service, not in a login session, and without one
  podman falls back to cgroupfs, under which systemd in the container can't come up. So `verify` first enables
  lingering for the runner's user (`sudo loginctl enable-linger`, which starts the user's systemd without a login)
  unless the bus is there already, points `XDG_RUNTIME_DIR` and `DBUS_SESSION_BUS_ADDRESS` at it for the later
  steps, runs `podman system migrate`, and fails unless `podman info` then names the systemd cgroup manager. F09's
  review fix checked that in a stand-in for the runner (Ubuntu 24.04 with systemd as PID 1, podman 4.9.3, the user
  `runner` without a login session, in a privileged Docker container): without the step podman fell back to
  cgroupfs and already `podman build` failed (`sd-bus call: Permission denied`); with it `verify`'s steps passed on
  both bases, and without `dbus-user-session` the step fails with its own error. The release dry run on the runner
  is what shows it there: its log says whether the runner had a session already (the step prints `linger` and the
  cgroup manager) and where the container reached `relserve`.
- **`distro-test.sh`'s podman path, as README F16 built it** (review F09-R4): the containers start with
  `podman run -d --systemd=always --cap-add=SYS_ADMIN --cap-add=SYS_PTRACE`, with no `--privileged` except V3's
  firewall container, which has all privileges as before. **`CAP_SYS_ADMIN`** is the one `verify` gives, for the
  unit's sandbox (the bullet above): without it C1 fails, as the server refuses `/var/lib/isshoni` with exit 78
  and the installer exits 5. **`CAP_SYS_PTRACE`** is for C4, which `verify` doesn't run: `ss -p` names the
  program behind another user's socket only with it, and the installer's port check (4.9) takes a listener without
  a name for another program's, so the upgrade would exit 7 on port 8080, which the running isshoni holds. A
  machine's root has both. Under rootless podman they are the container's, in its own user namespace, and give
  nothing on the machine; under rootful podman they are the machine's, still less than the Docker path's
  `--privileged`. **What the script leaves to the caller**: rootless podman needs the caller's systemd user
  session, as `verify` sets it up (the bullet above): a login, or lingering with `XDG_RUNTIME_DIR` and
  `DBUS_SESSION_BUS_ADDRESS` pointing at it, so that podman uses the systemd cgroup manager. Without it podman falls
  back to cgroupfs, with a warning: `podman build` then fails when it has to build the image
  (`sd-bus call: Permission denied`, the bullet above), and in a container from an image built before, systemd doesn't
  come up. The script checks for the latter as `verify` does: before each container it asks `podman info`, and when that
  says rootless and `cgroupfs` it stops and names the cause and the session it needs, instead of waiting a minute for
  systemd. The network is podman's default: on Linux, where `relserve` listens on every address, slirp4netns (the
  rootless default up to podman 4.9, checked with 4.9.3) and pasta from podman 5.3 on (its `--map-guest-addr`, checked
  with 5.8.7) both give the container a `host.containers.internal` that reaches it. Pasta under podman 5.0 to 5.2 gives
  the name no address that reaches `relserve`, as under 4.9 (the bullet above), and the run stops at "the container does
  not reach relserve"; there, set `default_rootless_network_cmd = "slirp4netns"` in the `[network]` table of
  `containers.conf`. F16 checked it rootless, as a user with lingering and no login, in privileged Docker containers
  that stood in for a Linux machine. With podman 4.9.3 (Ubuntu 24.04, slirp4netns) the parts `main`, `c7`, `c13` and
  `c11` passed on debian-12 and ubuntu-24.04 (246 checks), and `v3` on ubuntu-24.04 (ufw, 8); with podman 5.8.7 (Fedora
  44, pasta) the same four parts passed on debian-12 and fedora-44 (246). Without the capabilities, C1 failed as above
  (podman 4.9.3); with `SYS_ADMIN` alone, C4 failed with exit 7, and C5 after it (both versions). Without a user session
  (podman 5.8.7 on cgroupfs), `podman build` failed on an empty image store, and with the image built before, the run
  stopped at the check, before it started a container. Podman on macOS (`podman machine`) was not tried. CI still
  doesn't take this path (the S78 bullet above).
- `tools/relserve` serves a locally built, test-signed release over HTTPS (section 11.2).

**VMs** (`deploy/test/vm/`):
- QEMU with KVM on `ubuntu-24.04` runners: enable `/dev/kvm` with the usual udev rule, boot the official cloud image
  with a cloud-init seed, and SSH in.
- If `/dev/kvm` is missing, the job is **skipped with a warning** (it is nightly, not a PR gate). Before a release the
  same script runs by hand in UTM/Tart on the Mac.
- This is the only place that tests SELinux, real sysctl writes, enabled ufw/firewalld, the Oracle-style iptables
  path and ACME (Pebble).
- **As README F13 built it**: `sh deploy/test/vm/vm-test.sh [-r DIR] [-o PARTS] [DISTRO...]` is the nightly's
  `distro-vm` job (8.5) and the run by hand before a release. It sources `deploy/test/distro-test.sh` with
  `DISTRO_TEST_LIB=1`, a guard that skips the library's main (the library refuses that variable when it runs as
  itself), and runs its scenario functions over SSH through `vm_engine`, which stands in for the container
  engine's `exec`. At its start it checks that the library still has every function and variable it takes or
  replaces (`die`, `assert_eq`, `ROOT`, `ENGINE`, `in_container`'s `"$ENGINE" exec` call among them), so
  `vm-test.sh -h` is a check that needs no VM, no Linux and no network. The parts are `main`, `c7`, `c13`, `v3`
  (fedora-44, ubuntu-24.04), `v4` (ubuntu-24.04) and `v5`, each in a fresh VM; a part is skipped with a note on a
  distro it doesn't apply to, an image is fetched only where a part runs, and a run in which no part applies
  fails. The script starts Pebble and challtestsrv for V5 itself (their digests match `ci.yml`'s `acme` job and
  are bumped together). `deploy/test/vm/cloud-image.sh DISTRO FILE` downloads a distro's cloud image and checks
  its checksum; `vm-test.sh` calls it when `VM_DIR/images/<distro>.qcow2` is missing. Without KVM, `VM_ACCEL=tcg`
  emulates the VMs, for example in a Linux container or a UTM/Tart VM on a Mac, many times slower.

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

**The scripts as README S62 built them.** Each is one POSIX sh script (`set -eu`) for both package managers: it
reads dpkg's action word (`install`, `upgrade`, `configure`, `remove`, `purge`, the `abort-*` cases) or rpm's
count (`$1` = 1 first install, 2 upgrade; on removal 0 removed, 1 upgrade) from its first argument. Every
`systemctl` step runs only where systemd runs (`/run/systemd/system` exists), so the scripts work in a chroot or
an image build.

- **`preinstall.sh`**
  - dpkg's `abort-*` calls (an upgrade being undone) do nothing.
  - It refuses, exit 1, when `/usr/local/bin/isshoni` **or** `/usr/local/lib/systemd/system/isshoni.service`
    exists (a symlink counts): systemd looks in `/usr/local` before `/usr` for the program and for the unit
    alike, so the service would keep running install.sh's copy. The message names the file, gives the uninstall
    command (`curl -fsSL …/install.sh | sh -s -- --uninstall`) and says what that keeps.
  - It creates the group and the user with the commands of 4.4, each only when missing. Where `groupadd` or
    `useradd` is not installed it says which package has them (`passwd` on Debian and Ubuntu, `shadow-utils` on
    Fedora) and exits 1.
- **`postinstall.sh`**
  - `systemctl daemon-reload`.
  - **The UDP buffer sizes are applied per key and never lowered.** For each of `rmem_max` and `wmem_max` whose
    running value is below 8388608, it runs `systemd-sysctl --prefix=/net/core/<key> 60-isshoni.conf`, which
    applies the file as the next boot will (an `/etc/sysctl.d` file of the same name wins). A key that another
    sysctl.d file set higher is left alone, also when the other key was low; the plain `systemd-sysctl
    60-isshoni.conf` of the list above would have lowered it until the next boot. Where the values can't be read
    (most containers) nothing happens, and a failed write is a warning that points at `isshoni doctor`.
  - **Everything but a first install counts as an upgrade** (dpkg's `abort-*` cases too, since the files of a
    running service may have changed): `systemctl try-restart isshoni.service`, which restarts only a running
    service, with a warning when the restart fails.
  - **A service that is enabled or running** means the admin has started it before, and the script ends there
    without a word. Running without being enabled counts, so an upgrade of such a server doesn't print the
    first-install steps.
  - Otherwise it prints numbered steps:
    1. only while `/etc/isshoni/isshoni.toml` does not exist: `sudo isshoni config init --path … --domain
       share.example.com`, or `--tls.mode ip` without a domain; then `sudo chown root:isshoni …`; and the Let's
       Encrypt Subscriber Agreement notice of 4.10, which a package installation would otherwise never show;
    2. open ports 80 and 443 (TCP) and 7882 (TCP and UDP), here and in the provider's cloud firewall, with the
       commands for a firewall that is active on this machine: `sudo ufw allow isshoni`; for firewalld
       `sudo firewall-cmd --reload` and then `sudo firewall-cmd --permanent --add-service=isshoni && sudo
       firewall-cmd --reload` (4.9). systemd is asked whether firewalld is active before `firewall-cmd` is;
    3. `sudo systemctl enable --now isshoni`, then `sudo isshoni setup-url --wait 180s`, and the link to the
       install guide.
- **`preremove.sh`** acts only when the package is removed (deb `remove`, rpm `$1 = 0`), never on an upgrade:
  - `systemctl disable --now isshoni.service`, then `systemctl reset-failed isshoni.service`, so that a unit
    that had failed (exit 78, say) doesn't stay listed as "not-found failed" with the system "degraded" until the
    next boot. Without a running systemd it only removes the enablement links;
  - **it takes the isshoni service out of firewalld** (`clean_firewalld`): every zone and policy of the
    permanent configuration, and the rich rules that name the service, as 4.11 describes and for its reason. It
    does nothing when no file under `/etc/firewalld/zones` or `/etc/firewalld/policies` names the service (only
    a zone or policy that was changed on this machine can), which saves a dozen slow firewalld commands on every
    other machine, and nothing when the admin has a definition of their own in
    `/etc/firewalld/services/isshoni.xml`. It prints what it removed, each rich rule in full, and warns about a
    zone or policy it could not change. ufw is left alone: its rule keeps working without the profile.
- **`postremove.sh`**
  - `systemctl daemon-reload`.
  - deb `purge` removes `/etc/isshoni`, unless `/usr/local/bin/isshoni` exists: a purge can come long after the
    remove, and if install.sh has installed isshoni in between, that directory is its configuration. The script
    says that it kept it.
  - rpm has no purge, so there the configuration stays as well. deb `remove` keeps it until a purge and prints
    nothing.
  - After a purge or an rpm removal it says what is left, `/etc/isshoni` and `/var/lib/isshoni` where they
    exist, and the isshoni user, and prints the one command that deletes them (`sudo rm -rf … && sudo userdel
    isshoni`). The data directory is never deleted by a script.

Checked with nfpm-built packages under dpkg and rpm in privileged systemd containers of Debian 12 and 13, Ubuntu
22.04 and 24.04 and Fedora 44 (firewalld 1.1 to 2.4, systemd 249 to 259): install, upgrade, a failed unit,
remove, reinstall, purge, and removal with the service in zones, a policy and rich rules, with the daemon running
and stopped. That was by hand (S62 adds no script and no CI change); the packages themselves are built from
README S73 on (`goreleaser-check`, 8.2).

The package maintainer field is the owner's GitHub noreply address (filled in once; no personal e-mail in the repo).

**The packages as README S73 built them** (`nfpms` in `.goreleaser.yaml`, 9.2; `task release:assets` checks the
files, the versions and the dependencies of each):
- **They never reload firewalld on install or upgrade** (README §6 "Decided after group 9" (2); 4.9): only
  install.sh's firewall offer does. `postinstall.sh` puts nothing into firewalld and prints `firewall-cmd --reload`
  before `--add-service=isshoni` for the admin; `preremove.sh` reloads only after it took the service out of a
  zone or policy.
- **The tools `preinstall.sh` needs come first**: the deb has `Pre-Depends: passwd` (dpkg runs a `preinst` only after
  its Pre-Depends are configured), the rpm `Requires: shadow-utils` (rpm orders a Requires before the package).
- **Nothing of the build machine is in a package**: the files are dated by the commit (`mtime`), and the rpm's build
  host is the fixed name `isshoni` (rpm would record the machine's host name).
- **File names** use `-` where the package version has `~` (§3).
- The maintainer is the owner's GitHub noreply address, which the owner confirms before the first release.

## 6. Docker

### 6.1 Image: `deploy/docker/Dockerfile`

```dockerfile
# syntax=docker/dockerfile:<exact version>@sha256:<pinned digest>
# Dependabot does not keep the line above current (its docker ecosystem reads only FROM lines): bump its version and
# digest by hand with the monthly bump of the tool pins (§6.1, §7.1).
# Built by goreleaser dockers_v2 (release) and deploy/docker/test/docker-build.sh (CI, task docker:smoke). No RUN
# steps: nothing is emulated for arm64, and the image holds only the static binary.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:<pinned digest, updated by Dependabot>
ARG TARGETPLATFORM
# rootfs/ contains var/lib/isshoni/.keep and run/isshoni/.keep: both directories owned by nonroot, so a new named
# volume is writable (Docker copies the ownership and the mode into an empty volume) and the admin socket has a
# home. One COPY per directory: one COPY of rootfs/ onto / would hand /var, /var/lib and /run to nonroot as well.
COPY --chown=65532:65532 --chmod=0700 deploy/docker/rootfs/var/lib/isshoni/ /var/lib/isshoni/
COPY --chown=65532:65532 --chmod=0750 deploy/docker/rootfs/run/isshoni/ /run/isshoni/
COPY --chmod=0755 $TARGETPLATFORM/isshoni /usr/local/bin/isshoni
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

**As README S68 built it** (`deploy/docker/Dockerfile`, `rootfs/`, `test/docker-build.sh`; `deploy/docker/README.md`
says the same for the repository's readers):
- **Two COPYs of the directories, each with `--chown` and `--chmod`**: `/var/lib/isshoni` `0700` and `/run/isshoni`
  `0750`, both `65532:65532`, the modes of a systemd install (`StateDirectory`, `RuntimeDirectory`). Docker copies
  the owner and the mode into a new named volume, so the data volume is writable without a `chown`, and the server
  finds its data directory private, as it wants it. One `COPY --chown` of `rootfs/` onto `/`, the first version of
  the block above, would also have given `/var`, `/var/lib` and `/run` to 65532.
- **The binary's mode is set by `COPY --chmod=0755`**, not taken from the build context: under `cap_drop: [ALL]`
  the image's user could not start a binary that a strict umask on the builder had left at `0700`. The notices'
  `COPY` has no `--chmod`: it would also apply to the directory the `COPY` creates, and `0644` would let nobody
  into it.
- **The `FROM` digest** is the multi-arch index of the `:nonroot` tag, and Dependabot's `docker` ecosystem for
  `/deploy/docker` updates it (§8.1).
- **The `# syntax=` line is pinned by digest too** (decided after group 8; README §6; README S73 implements it).
  BuildKit fetches the Dockerfile frontend that this line names from Docker Hub before it reads the rest of the
  file, so `docker/dockerfile:1` is a floating tag that every build, the release's included, pulls as it is at
  that moment: the one input of the image that no digest fixed. Like the `FROM` line it becomes
  `docker/dockerfile:1@sha256:<digest>`, so no build pulls a frontend nobody chose (§9.2). S73 also says, in a
  comment just below the line, how that digest is kept current. The comment can't go above the line: BuildKit
  looks for parser directives only at the top of the file, before any comment, empty line or instruction, and
  would read the line as a plain comment.
- **As README S73 pinned it** (with the group 9 integration's comment below it): the line names the frontend's
  exact version and its digest, `docker/dockerfile:1.27.1@sha256:…` (the version that `:1` pointed at then), not
  `:1@sha256:…`, so that a bump says what it moves to. **Dependabot does not update it**: its `docker` ecosystem
  reads only `FROM` lines, so the version and the digest are **bumped by hand with the monthly bump of the tool
  pins** (§7.1; README §6 "Decided after group 9" (3)), and the comment says so. `.github/dependabot.yml`'s "not
  covered here" comment names the line too.

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
  - **Docker Desktop** (macOS, Windows; README S68's finding): the engine runs in a virtual machine, and a
    published port is forwarded from that machine's own address, `192.168.65.1` by default on Docker Desktop for
    Mac. That is what the server sees for a client on `[::1]` there, where a Linux engine shows the bridge
    gateway: the finding is the same, the server does not see the client's address. `task docker:smoke` accepts
    either, the forwarding address only where the engine is not local, and fails on `::1` itself.
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
  Dependabot can't read `.tool-versions`. The same PR bumps the pins that follow them and that Dependabot doesn't
  read either (README §6 "Decided after group 9" (3), "Decided after group 12" (8)):
  - `tools/goreleaser/go.mod` with the goreleaser line (below);
  - the version and digest of the Dockerfile's `# syntax=` line (§6.1);
  - the image digests inside workflows, which no Dependabot ecosystem reads (its `docker` ecosystem reads
    Dockerfiles, its `github-actions` ecosystem `uses:` lines): `release.yml`'s `verify` matrix bases
    (`docker.io/library/debian@sha256:…` and `ubuntu@sha256:…`, the tag in a comment, 4.13) and the BuildKit image
    of `build`'s buildx driver (`moby/buildkit:v<x>@sha256:…`, the release that `buildx-stable-1` points to, 9.3;
    README F09), each to the digest of its multi-arch index that `docker buildx imagetools inspect` prints; and
    the Pebble and `pebble-challtestsrv` images of `ci.yml`'s `acme` job and of `deploy/test/vm/vm-test.sh`
    (README F13), which pin the same digests and are bumped together.

  `.github/dependabot.yml`'s "Not covered here" comment names the same list.
- `go.mod` has `go 1.26.0` and `toolchain go1.27.3`, the `.tool-versions` patch (the plan: language version 1.26,
  newest toolchain). The go line carries a patch because `golang.org/x/text`, `golang.org/x/crypto` and
  `modernc.org/sqlite` require `go 1.26.0`, so `go get` and `go mod tidy` write it that way; the language version is
  still 1.26. `tools/go.mod` has the same two lines. CI reads the version from `go.mod`, not from `.tool-versions`,
  and `task lint:pins` fails if the two differ. A toolchain patch that fixes a vulnerability govulncheck reports
  (the non-gating CI job, 8.2; nightly and release gate on it) doesn't wait for the monthly bump: go1.27.3 came
  that way after group 16, for GO-2026-6809 in `crypto/x509`, which the server reaches, with the toolchain line of
  `go.mod`, `tools/go.mod`, `tools/goreleaser/go.mod` and `.tool-versions` moved together (README §6 "Decided
  after group 16" (6)).
- Node's bundled npm (11.x) is used; `package.json` has `"engines": {"node": ">=26"}` and
  `"packageManager": "npm@<exact 11.x version>"`.

**`tools/go.mod`** is a separate module, `github.com/MoonWX/isshoni/tools`, with Go 1.24+ `tool` directives:
- tools: `github.com/gzuidhof/tygo`, `github.com/google/go-licenses/v2`, `golang.org/x/vuln/cmd/govulncheck`, and
  Task, golangci-lint and shfmt (`mvdan.cc/sh/v3`) at the `.tool-versions` versions (`task lint:pins` checks them).
  actionlint and ShellCheck come from mise or a package manager (actionlint v1.7.12 doesn't build against the
  `go.yaml.in/yaml/v4` release candidate that golangci-lint's gosec needs);
- our own small programs: `tools/repocheck` (behind `task lint:pins` and `lint:keys`), `tools/notices` (section 8.4),
  `tools/relserve` (section 11.2) and `tools/exittest` (section 12).

**`tools/goreleaser/go.mod`** (README S73) is a module of its own, `github.com/MoonWX/isshoni/tools/goreleaser`, with
one `tool` directive, `github.com/goreleaser/goreleaser/v2` at the `.tool-versions` version. goreleaser is not in
`tools/go.mod` because it needs a newer `go` directive than the repository's 1.26 (2.18 requires `go 1.27.1`), and
its large dependency graph (cloud SDKs, sigstore, …) would raise versions that golangci-lint, Task and the other
tools share. `task release:goreleaser` builds it into `.bin/` (§7.2), and the release tasks, CI's `goreleaser-check`
and release.yml's `build` job run it from there, each building it first: the `.bin/goreleaser` they run is always
the pinned one, whatever mise or a package manager installed (`CONTRIBUTING.md` says so).
`task lint:pins` (`tools/repocheck`) checks the module's toolchain line and its goreleaser version against
`.tool-versions`, like the other modules. Dependabot's `gomod` directories leave it out: it is bumped by hand with
`.tool-versions`.

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
| `test:sh` | `deploy/test/install_unit.sh` under dash, `bash --posix` and busybox, and after it under each shell `deploy/test/vm/vm-test.sh -h`, the VM harness's start-up check that `distro-test.sh` still has every name it takes (4.13; added by the group 12 integration, README F13's review) | Outside CI a shell that isn't installed is skipped with a note (macOS has no busybox); in CI, where `lint-deploy` installs busybox, a missing shell fails the task. The `vm-test.sh -h` check needs no VM, Linux or network, so a rename in `distro-test.sh` fails `lint-deploy` on the PR, not only the nightly. Under dash and `bash --posix` (not busybox, whose own `sha256sum` has no `--strict`) it then runs `deploy/test/check-draft-test.sh`, the test of `publish`'s draft check (9.3, README F15) |
| `lint` | `lint:go`, `lint:web`, `lint:sh`, `lint:actions`, `lint:keys`, `lint:pins`, `lint:unit` | |
| `lint:go` | `golangci-lint run`, `go mod tidy -diff`, `GOOS=darwin go vet ./...`, `GOOS=windows go vet ./...` | |
| `lint:web` | `npm --prefix web run lint && npm --prefix web run typecheck` | |
| `lint:sh` | `shellcheck --shell=sh` and `shfmt -p -i 2 -d` over `deploy/**/*.sh` | |
| `lint:actions` | `actionlint` | |
| `lint:keys` | embedded block in `install.sh` = `deploy/keys/allowed_signers`; from README F11 on, once that file is not empty, the root `README.md`'s "Release signing keys" section lists the fingerprint of each of its keys and no other one, and no row there says a key is not created (10.3) | |
| `lint:unit` | `systemd-analyze verify deploy/systemd/isshoni.service` (Linux only; skipped elsewhere) | |
| `build:web` | `npm --prefix web run build` with env `ISSHONI_VERSION={{.VERSION}}` → `web/dist/` (plus `web/dist/licenses.txt` and `version.json`) | Task `sources`/`generates` make it a no-op when nothing changed. A `status:` check next to them (a one-line `node -p` read of `web/dist/version.json`) makes it out of date whenever that file's `version` ≠ `{{.VERSION}}` |
| `build:go` | `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/MoonWX/isshoni/internal/version.version={{.VERSION}} -X github.com/MoonWX/isshoni/internal/version.commit=$(git rev-parse HEAD) -X github.com/MoonWX/isshoni/internal/version.date=$(git log -1 --format=%cI)" -o bin/isshoni ./cmd/isshoni` (04 §15) | Go only, no Node needed (CI's `build` job). Fails if `web/dist/index.html` is missing. The flags are identical in content to 9.2 (commit date, as goreleaser's `{{ .CommitDate }}` and `mod_timestamp`) |
| `build` | `build:web`, then `build:go` | one `VERSION` for both |
| `e2e` | `build` with `VERSION: '{{.ISSHONI_VERSION \| default "0.0.0-e2e.local"}}'`, then `npm --prefix web run e2e` with `ISSHONI_BIN={{.ROOT_DIR}}/bin/isshoni` | a non-dev version (see above). `ISSHONI_BIN` is absolute, because npm runs scripts from `web/`. Playwright, Chrome channel |
| `licenses` | go-licenses check (3 GOOS) and `node web/scripts/licenses.mjs check` for `web/` and `docs/` | section 8.3 |
| `notices` | `go -C tools run ./notices …` → `THIRD_PARTY_NOTICES` | section 8.4 |
| `release:prepare` | **no npm**: fails with "web/dist was built for X, not VERSION: run task build:web VERSION=…" unless `web/dist/version.json`'s `version` equals `VERSION` (read with `sed`, no Node); then `notices`, stamp `dist-extra/install.sh` (`VERSION=`) | goreleaser's before hook (`task release:prepare VERSION={{ .Version }}`, 9.2). The SPA comes from outside: the unprivileged `web` job in `release.yml` (9.3), or `release:snapshot` below. So the privileged release job runs no third-party JavaScript |
| `release:goreleaser` (README S73) | `go -C tools/goreleaser build -o ../../.bin/ github.com/goreleaser/goreleaser/v2` with `CGO_ENABLED=0` | the pinned goreleaser (7.1); rebuilt only when `tools/goreleaser/go.mod` or `go.sum` changed (Task `sources`/`generates`) |
| `release:snapshot` | `build:web`, then `goreleaser release --snapshot --clean --skip=sign,sbom`; both get `ISSHONI_SNAPSHOT_VERSION` (default `0.0.0-dev.<12-char commit>`), as `VERSION` for `build:web` and as env for goreleaser. As README S73 built it: first `release:goreleaser` and `goreleaser check`; `CLI_ARGS` go on to goreleaser (`task release:snapshot -- --skip=docker` leaves out the images); then `release:assets` for that version, and the image for this machine's CPU must start and report it. The images are built (9.2: one per platform, `<v>-amd64` and `<v>-arm64`, loaded locally, never pushed) and removed again when the task ends, passed or not; `KEEP_IMAGES=true` keeps them | local dry run; `goreleaser-check` and the distro tests use it too |
| `release:assets` (README S73) | checks `dist/` after a goreleaser run against §3 for `VERSION=<v>`: the asset names (each one GitHub keeps as it is), `checksums.txt` (exactly the summed files, each once, with the right hash), the stamped `dist-extra/install.sh`, every archive's files, this machine's binary's version, and each package's files, version (`~` before a prerelease), architecture and dependencies (rpm's with `rpm` where it is installed). `FULL=true` (a release) also wants the SBOMs, the cosign bundle and `README.md` | run by `release:snapshot`, CI's `goreleaser-check` and release.yml's `build` (`FULL=true`) |
| `release:keys` (README S73) | checks `deploy/keys/allowed_signers` and prints each key's fingerprint, writes the file into install.sh's key block, then runs `lint:keys`. It refuses, before it writes anything, a missing or empty file, a private key, a line that is not `isshoni-release namespaces="isshoni-checksums" ssh-ed25519 …`, a key that `ssh-keygen` does not read, a single quote anywhere and a missing final newline. From README F11 on, lint:keys' README.md check runs after the write, so the first run ends with an error until the root `README.md` lists the printed fingerprints (deploy/keys/README.md step 2, 10.3) | the owner's step 2 of `deploy/keys/README.md` (9.4) |
| `release:compat` | `task release:compat VERSION=<v>` (`<v>` is the tag without `v`, e.g. `0.2.0`): copies `internal/protocol/testdata/v1` to `internal/protocol/testdata/compat/<v>/`, then deletes all but the two newest final-release snapshots (SemVer order). Fails unless `VERSION` is a final `X.Y.Z`: prereleases (`-rc.N`) get no snapshot, and a forgotten `VERSION=` leaves the dev version, which fails too. It also refuses, changing nothing, a `VERSION` older than the two snapshots it would keep, a missing `testdata/v1`, and a `compat/` that holds anything but `X.Y.Z` snapshot directories (hidden files aside; a link counts as anything) | 01 §14.3. Run in the release-prep PR (9.1 "Before tagging"); release.yml checks the result (9.3). Plain `sh` and `cp`, no Go or Node. S01 writes it as a stub; S75 (01 P13) owns the body |
| `deploy:test` | distro container tests; `DISTRO=debian-12` selects one | needs podman or Docker |
| `docker:smoke` | section 6 smoke test (bridge with a backup → restore round trip and the IPv6 client-IP check, host, no-volume, no `.env`; 11.3) | |
| `site:dev` / `site:build` | `npx --prefix docs vitepress dev\|build docs` | |
| `clean` | remove `bin/`, `.bin/`, `dist/`, `dist-extra/`, `web/dist/*` (keeps `.gitkeep`), `.dev/` | as built, `.bin/task` stays (it is often the Task running the command), and the other pinned tools go, `.bin/goreleaser` included |

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

**The Go tests in a Linux container, from a checkout set up on a Mac** (the group 14 integration). `.bin/` holds
binaries for the machine that built them (7.1), so a container that mounts such a checkout (`golang:<version>`,
to run the tests on Linux as CI does) sees darwin binaries there: `go -C tools test ./...` then fails
`tools/notices`' `TestGoLicensesGate` with "exec format error", because the test runs `.bin/go-licenses` whenever
it exists (8.3). In such a container run only the root module's `go test ./...`, or the `tools/` packages other
than `notices` (`go -C tools test ./exittest/` runs `exittest`'s Linux-only `TestRealProc`), or point
`GO_LICENSES_BIN` at a Linux build of the pinned go-licenses. The root module's suite is unaffected, and so is CI,
whose jobs build `.bin/` on Linux.

## 8. CI (GitHub Actions)

### 8.1 Workflows

| File | Triggers | Purpose |
|---|---|---|
| `.github/workflows/ci.yml` | `pull_request`, `push` to `main`, `merge_group` | gate for every change |
| `.github/workflows/nightly.yml` | `schedule: '17 3 * * *'`, `workflow_dispatch` | slow and flaky-prone checks (8.5) |
| `.github/workflows/release.yml` | `push` tags `v*`, `workflow_dispatch` (dry run) | section 9 |
| `.github/workflows/site.yml` | `push` to `main` (paths `docs/**` without `docs/PLAN.md` and `docs/m1/**`, plus `deploy/keys/allowed_signers`, whose keys the site serves, and `site.yml` itself; README S82), `workflow_dispatch`, `workflow_call` | section 10 |
| `.github/dependabot.yml` | weekly | `gomod` (`/`, `/tools`), `npm` (`/web`, `/docs`), `github-actions`, and `docker` (`/deploy/docker`), added back by README S68 with the Dockerfile (Dependabot's update job fails on a directory without one); minor and patch updates grouped per ecosystem; `target-branch: m1/server-web` for every ecosystem while M1 lands there (README §4), switched to `main` after M1. The tools pinned in `.tool-versions` (Task, golangci-lint, shfmt) are ignored in `/tools` and bumped by hand. In `npm`, major updates of `eslint`, `@eslint/js` and `typescript` are ignored until eslint-plugin-jsx-a11y and typescript-eslint allow them (05 §2); the file names the blocking peer ranges |

**Rules for every workflow:**
- Top-level `permissions: contents: read`; jobs raise permissions individually.
- Every third-party action is pinned by full commit SHA with a version comment.
- `persist-credentials: false` on checkout unless a step pushes.
- `concurrency: ${{ github.workflow }}-${{ github.ref }}` with cancel-in-progress for PRs. One exception,
  `site.yml` (README S82), whose header says why: called from `release.yml`, `github.workflow` and `github.ref` are
  the caller's, so the group would be `release.yml`'s own, a deadlock that GitHub cancels. Its deploys are one at a
  time through the `deploy` job's own group, `pages`, which never cancels one under way (10.5).
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
- `release`: `.goreleaser.yaml`, `.github/workflows/release.yml`, `deploy/packaging/**`, `tools/goreleaser/**`
  (README S73)
- `site`: `docs/**` except `docs/PLAN.md` and `docs/m1/**`, plus `web/src/conntest/codes.json` (the anchor test,
  10.4)
- `tools`: `tools/**`
- `distro_tools`: `tools/relserve/**`, `tools/notices/**`: the inputs of the distro jobs that neither `deploy` nor
  `release` covers (README S78, below)
- `readme`: the root `README.md`, which every release archive carries (3): it runs `goreleaser-check`, so a pull
  request that deletes it fails there (README S82), and `lint-deploy`, whose `lint:keys` checks its "Release
  signing keys" section (README F11)
- `repocheck`: `tools/repocheck/**`, the checker behind `lint:keys` and `lint:pins`: it runs `lint-deploy`
- `doctor`: `internal/server/ops/doctor/**`, the checks and codes that docker-smoke's doctor step pins (11.3): it
  runs `docker-smoke` (added after README group 17; review F21-R2)

| Job | Runs when | Steps and assertions | Timeout | Gates merge |
|---|---|---|---|---|
| `lint-go` | go | `golangci-lint run` (config below); `go mod tidy -diff`; `go vet` for GOOS darwin and windows | 10 | yes |
| `test-go` | go | `task test:go`; coverage uploaded as an artifact (no external service) | 20 | yes |
| `web` | always (`build` always needs its artifact) | `npm ci`, then `lint`, `typecheck`, `test -- --run`, `check:i18n`, `build`, `check:size` (05 §2). Uploads `web/dist` | 10 | yes |
| `protocol` | go or web | `task gen:check` (tygo drift). The golden fixtures (`01`) run inside `test-go` and `web` | 5 | yes |
| `licenses` | go, web or site | `task licenses` | 10 | yes |
| `build` | always | downloads `web/dist`, `task notices`, `task build:go` (no Node on this runner); `bin/isshoni version --json` must show the commit and `0.0.0-ci.<run>`; warns above 60 MB. Uploads `bin/isshoni`; then builds `bin/isshoni-loadtest` (`CGO_ENABLED=0 go build -trimpath`, not a release artifact) and uploads it as `isshoni-loadtest-linux-amd64` for e2e's `decodable.spec` (README S89) | 10 | yes |
| `e2e` | go, web or deploy (below) | setup-node and `npm ci` in `web/`; downloads the two binaries and runs `chmod +x` on both (artifacts lose the mode bit); env `ISSHONI_BIN: ${{ github.workspace }}/bin/isshoni` and `LOADTEST_BIN: ${{ github.workspace }}/bin/isshoni-loadtest` (`decodable.spec`'s tool, README S89; without it the spec builds the tool with the Go on the `PATH`); the runner's preinstalled Google Chrome stable, plus Playwright's ffmpeg for the videos (`npx --prefix web playwright install ffmpeg`; `… install chrome`, which brings ffmpeg along, only if Chrome is missing); installs `xvfb`, `openbox` and `pulseaudio` and starts a PulseAudio null sink as the default sink (Chrome gets an output device for tab audio; the step can go if the specs that play audio pass without it); `xvfb-run -a --server-args="-screen 0 1920x1080x24" sh -c 'openbox & exec npm --prefix web run e2e'`: Chrome's capture picker needs a window manager under X11. One Playwright worker and one retry (05 §19.3). After every run that was not cancelled, green or red, uploads `web/test-results/` (the Playwright trace and video of a failed attempt, the per-server JSON logs `server-*.log`, 14) and `web/playwright-report/` as `e2e-results`, kept 7 days | 20 | yes |
| `lint-deploy` | deploy, readme or repocheck | `task lint:sh lint:actions lint:keys lint:unit lint:pins test:sh` | 10 | yes |
| `distro-release` | deploy or release, or `tools/relserve` or `tools/notices` (below) | builds the two test releases once for the matrix (README S78): setup-node and `npm ci` in `web/`, because `task release:snapshot` builds the SPA (7.2); rpm on the runner; `sh deploy/test/distro-test.sh -b`; uploads them as the artifact `distro-releases`, kept 1 day | 20 | yes |
| `distro` | as `distro-release` | matrix of the 6 container distros (4.13), scenarios from 11.2, on the releases of `distro-release` (`distro-test.sh -r`), with `CONTAINER_ENGINE=docker` | 30 | yes |
| `docker-smoke` | docker, deploy, release or doctor (doctor's checks and codes, which the step pins, 11.3) | `task docker:smoke` (11.3), linux/amd64 | 15 | yes |
| `goreleaser-check` | release, deploy or readme (below) | setup-node and `npm ci` in `web/`; `goreleaser check`; `task release:snapshot` (builds the SPA first, 7.2); asserts the asset list of section 3 (names and archive contents) | 20 | yes |
| `site` | site or deploy | setup-node (with npm's cache) and `npm ci --prefix docs`; `vitepress build`; `node docs/scripts/check-site.mjs`, the check that the site loads nothing from another origin (10.1, README S82); setup-go (from `go.mod`), then `go test ./internal/server/ops/doctor -run TestDocsAnchors` (10.4). It deploys nothing: that is `site.yml`'s (10.5). `ci-ok` needs it | 10 | yes |
| `govulncheck` | go | `.bin/govulncheck ./...`, `continue-on-error: true` (new CVEs mustn't block unrelated PRs; nightly and release gate on it) | 10 | no |
| `ci-ok` | always (`if: always()`) | fails if any job in `needs` is `failure` or `cancelled`; `skipped` counts as success | 2 | **the only required check** |

**The `e2e` job as README S61 built it.**
- **It also runs on deploy changes.** The table's "go or web" left out the job's own inputs: the servers' base
  config `deploy/dev/isshoni.e2e.toml`, the Node pin in `.tool-versions`, and the workflow itself, all of which
  the `deploy` filter covers. So the job runs when `go`, `web` or `deploy` is true. Like every job it is skipped,
  which counts as a pass, while one of its inputs doesn't exist yet (`.tool-versions`, `cmd/isshoni`,
  `web/package.json`, `web/package-lock.json`, `web/playwright.config.ts`, `deploy/dev/isshoni.e2e.toml`); the
  `changes` job writes each job's decision into the run's summary.
- **openbox under Xvfb.** Xvfb is a bare X server, and Chrome's capture picker reads window properties that only
  a window manager sets: without one `getDisplayMedia` never answers. openbox runs inside the `xvfb-run` session
  and ends with it. The screen is larger than the browser windows.
- **One worker.** All windows share the one X display, and with two workers about one pick in 25 hung on an X
  `BadWindow` error (05 §19.3). The config sets it for `CI`; a local run keeps Playwright's default.
- **ffmpeg for video.** Playwright records the videos of failed tests with its own ffmpeg build, which a
  preinstalled Chrome doesn't bring; the job installs only that. Locally video is off.
- **The null sink** is loaded with `pactl load-module module-null-sink` and made the default sink. In a container
  run of the same steps a captured tab's sound reached a receiving PeerConnection without any device, so the
  step is kept only until the specs that play audio show whether they need it.
- **The upload is not only for failures**: with one retry a flaky test leaves the job green, and the trace, the
  video and the server log of its failed attempt would never be seen otherwise. A run without any failure
  uploads only the logs and the report, which are small.
- **Auth throttles.** The job needs no config for them: the harness sends each account request from an address
  of its own, which the e2e server takes from `X-Forwarded-For` because its peer is loopback (05 §19.3, 04 §8.5).
- `ci-ok` needs the job.

**The `distro-release` and `distro` jobs as README S78 built them.**
- **The releases are built once.** The design had each of the six matrix jobs run setup-node, `npm ci` and the
  snapshot build. `distro-release` builds the two test releases (`0.0.1-ci.1` and `0.0.1-ci.2`, 11.2) and passes
  what the installer downloads (the linux archives, `checksums.txt` and the stamped `install.sh`, unsigned) to the
  matrix as an artifact; each `distro` job needs only Go (for `tools/relserve`), signs the releases with a key of
  its own and runs one distro. That saves about five snapshot builds per run. `ci-ok` needs both jobs, so a
  failed `distro-release` can't leave `distro` skipped and green.
- **Docker on the runner**, not podman (4.13): `CONTAINER_ENGINE=docker`.
- **Its own inputs count**, as for `e2e` (README S61): `tools/relserve`, which only this job runs, and
  `tools/notices`, whose output is inside the archives it installs, are in neither the `deploy` nor the `release`
  filter, so the `changes` job has a filter for them (`distro_tools`) that runs both jobs too. Like every job they
  are skipped, which counts as a pass, while one of their inputs doesn't exist yet.
- **Per distro** (11.2): C1–C6, C8–C10 and C12 in one systemd container, C7 and C13 in one each, C11 in the
  distro's plain image, and V3 with a running firewall on `fedora-44` (firewalld) and `ubuntu-24.04` (ufw).

**The `goreleaser-check` job as README S73 built it.**
- **It also runs on deploy changes**, not on release changes alone, as the `e2e` job does (README S61): its other
  inputs are deploy files, which the `deploy` filter covers: the files the archives and packages ship, install.sh
  (the before hook stamps it), the Dockerfile, the release tasks in `Taskfile.yml`, `.tool-versions` and the
  workflow itself. The `release` filter also has `tools/goreleaser/**`. Like every job it is skipped, which counts
  as a pass, while one of its inputs doesn't exist yet.
- **One step does it all**: `task release:snapshot` (7.2) builds the pinned goreleaser, the SPA for the snapshot's
  version, runs `goreleaser check`, the snapshot without signing, SBOMs or publishing, `task release:assets`, and
  starts the runner's image to read its version. The snapshot carries the run's CI version
  (`ISSHONI_SNAPSHOT_VERSION=0.0.0-ci.<run>`), not a dev one. The two images are built and loaded on the runner,
  never pushed.
- **rpm is installed** on the runner when missing, so `release:assets` reads the rpm packages' dependencies and
  version too.
- **Caches**: the Go modules and builds per job, and the pinned goreleaser binary with Task's checksum of its
  module (keyed by `tools/goreleaser/go.mod` and `go.sum`, no restore keys), so a hit neither downloads nor compiles
  goreleaser's module graph. release.yml uses no caches (9.3).

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

**`nightly.yml` as README S83 built it**, where the table leaves a choice or the workflow departs from it:
- **`vm-release` builds the two test releases once** (`0.0.1-ci.1` and `0.0.1-ci.2`, unsigned, as `ci.yml`'s
  `distro-release` does, 8.2), and each `distro-vm` job signs them with a key of its own. **`distro-vm`** boots
  each part in a fresh VM from the distro's official cloud image with a cloud-init seed (QEMU with KVM; `/dev/kvm`
  opened with GitHub's udev rule) and runs it over SSH. The parts: `main` (C12, C1, V1's sysctl values, C2, C3, C6,
  C4, C5, C8, C9, C10, then V6 and, on Fedora, V2), `c7`, `c13`, `v3` (Ubuntu 24.04 with ufw, Fedora with
  firewalld), `v4` (Ubuntu 24.04) and `v5` on every distro; C11 has no VM form and stays in `ci.yml`. Without
  `/dev/kvm` each job is skipped with a warning naming 4.13 and 9.1. The scenario step has a timeout of its own
  (75 of the job's 90 minutes), so a hang still leaves time to show Pebble's logs and upload the VMs' consoles.
- **The VM harness is `deploy/test/vm/vm-test.sh`** (4.13), which the `distro-vm` job's scenario step runs with
  `-r` (the `vm-release` job's releases) and `-o` (the matrix row's parts). S83 first wrote it inline in that step,
  sourcing `distro-test.sh` without its last line, because it could change only `nightly.yml` (README §6 "Decided
  after group 11" (5)); README F13 moved it into the script, behind `distro-test.sh`'s sourced guard, and
  `lint:sh` covers it. Pebble and challtestsrv start inside the script, not in a step of their own. The job's first
  real run is the first nightly after `m1/server-web` reaches `main`: F13 never ran the VMs of debian-13,
  ubuntu-22.04 and ubuntu-26.04 by hand, nor any VM on x86_64 with KVM, the only way the job runs (README §6 "Owner
  actions").
- **`goreleaser-full` does not sign**: `goreleaser release --snapshot --clean --skip=sign` with the images and the
  SBOMs, not the plain command of the table. cosign's keyless signature would need `id-token: write` in a job that
  also runs npm, which `release.yml` keeps apart (9.3), and every night would add an entry to the public
  transparency log; `release.yml`'s dry run covers the signing. `task release:assets FULL=true` then gates on
  every asset of section 3, with an empty stand-in for the one only a signed run makes,
  `checksums.txt.sigstore.json`; the job also checks that the SBOMs describe the archives and that both images
  are loaded and the runner's starts with the snapshot's version.
- **`go-cross-os`** runs `internal/server/ops` and `cmd/isshoni` (the backup, restore and re-exec code) as a step
  of their own, then every other package, with `-race` and `CGO_ENABLED=1`. On Windows it takes MinGW-w64's gcc
  from the runner image and installs it with Chocolatey only when the image has none (`choco install mingw`, not
  pinned to a version).
- **`site-links`** builds the site with `task site:build`, lays it out under its base path (`/isshoni/`) so that
  root-relative links resolve as on Pages, and runs lychee over it; it says so and does nothing while
  `docs/package.json` doesn't exist.
- **No caches**: like `release.yml`, the nightly builds from the module proxy and npm, and leaves `ci.yml`'s cache
  space alone; the jobs' timeouts allow for cold builds. Its own `concurrency` group never cancels a run under
  way; a newer run takes the place of one that is still waiting.

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
  5. **The root `README.md`, for every tag, the rc's too** (README F05's review): every release archive carries it
     (3), so what it says ships inside the release. Bring up to date, in a PR into `main` merged before the tag
     (for a final release, step 4's PR carries it, and the tag goes on that merge commit; for an rc, tag the merge
     commit of this PR): the pre-release banner at the top and the "Status" section ("No release yet" must not ship
     inside `v0.1.0-rc.1`), "What v0.1.0 will ship" and the Install intro ("Not available yet"), as the comment in
     its Status section lists. Its "Release signing keys" table must list the fingerprints of
     `deploy/keys/allowed_signers` (9.4), which `deploy/keys/README.md` step 2 fills in with the keys; from README
     F11 on, `repocheck keys` (`task lint:keys`, and release.yml) fails while a key's fingerprint is missing there, a
     fingerprint there is no key's, or a row still says the key is not created (10.3).
- **Tag and push** with plain git: `git tag -a v0.1.0 -m v0.1.0 && git push origin v0.1.0`. `gh` is not needed.
- **Approve.** The owner edits the draft notes on GitHub, then approves the `release` environment (works from the
  GitHub mobile app).

### 9.2 `.goreleaser.yaml` (goreleaser OSS v2, ≥ 2.12)

As README S73 built it (the comments here are shorter than the file's; the owner's address stays out of this page):

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
    builds_info: { owner: root, group: root, mode: 0755, mtime: "{{ .CommitDate }}" }
    files: # at the archive's root, with their repository paths (§3); owned by root, dated by the commit
      - &archived { src: LICENSE, info: { owner: root, group: root, mtime: "{{ .CommitDate }}" } }
      - { <<: *archived, src: NOTICE }
      - { <<: *archived, src: THIRD_PARTY_NOTICES }
      - { <<: *archived, src: README.md }   # README F05; S73 had "README.m[d]" while there was none (§3)
      - { <<: *archived, src: deploy/systemd/isshoni.service }
      - { <<: *archived, src: deploy/sysctl/60-isshoni.conf }
      - { <<: *archived, src: deploy/firewall/ufw-isshoni }
      - { <<: *archived, src: deploy/firewall/firewalld-isshoni.xml }

nfpms:
  - id: packages
    ids: [isshoni]
    package_name: isshoni
    file_name_template: '{{ replace .ConventionalFileName "~" "-" }}'   # §3: GitHub renames a "~"
    formats: [deb, rpm]
    homepage: https://moonwx.github.io/isshoni/
    maintainer: "<owner's GitHub noreply address>"
    description: Self-hosted screen and system-audio sharing for friend groups.
    license: Apache-2.0
    section: net
    bindir: /usr/bin
    mtime: "{{ .CommitDate }}"
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
    deb: { predepends: [passwd] }          # preinstall.sh's groupadd and useradd (§5)
    rpm: { buildhost: isshoni }            # not the build machine's host name
    overrides:
      rpm: { dependencies: [shadow-utils] }

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
  make_latest: false                # the publish job alone decides GitHub's "latest" (9.3)
  extra_files:
    - glob: dist-extra/install.sh
    - glob: deploy/compose.yaml
    - glob: deploy/compose.host.yaml
  header: |
    ## isshoni {{ .Version }}
    **Upgrade:** run the installer again, or `docker compose pull && docker compose up -d`.
    **Database migration:** TODO yes/no   **Protocol:** v1 (TODO bump?)   **Security fixes:** TODO
    **macOS and Windows archives** are experimental, untested server builds for LAN/dev use, with no installer; Linux (install.sh, deb, rpm, Docker) is the supported path.
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
- **What slice S9 had to verify, as README S73 found it** (goreleaser 2.18):
  - **`dockers_v2.extra_files` keep their repository paths** in the build context, next to the binary at
    `linux/<arch>/isshoni`: the Dockerfile's `COPY deploy/docker/rootfs/…` and `$TARGETPLATFORM/isshoni` work as
    written, the way `deploy/docker/test/docker-build.sh` lays the context out for CI.
  - **`dockers_v2` builds images in a snapshot too**, unlike the design's assumption that images come only with
    publishing: one image per platform, tagged `<v>-amd64` and `<v>-arm64`, loaded into the local Docker and never
    pushed. `task release:snapshot` therefore checks that this machine's image starts and reports the version, and
    removes both images when it ends (7.2); `-- --skip=docker` leaves them out. PR CI's Docker smoke test still
    builds with `docker buildx build` and the same Dockerfile (11.3). At a release goreleaser builds and pushes the
    one multi-arch `:<v>`.
- **The release notes' header says that the macOS and Windows archives are experimental**, untested server builds
  for LAN/dev use with no installer, as §3 requires of every release: the line is fixed in the header, not a TODO
  that the owner has to remember.
- **`make_latest: false`**: goreleaser never marks a release "latest"; the `publish` job does it, for a final
  release only (9.3).
- **Nothing of the build machine is in the output**: archives and packages are dated by the commit and owned by
  root, and the rpm's build host is fixed (§5).
- **Images are signed in the workflow** (`cosign sign --yes ghcr.io/moonwx/isshoni@<digest>`), not with goreleaser's
  `docker_signs`. It is simpler and doesn't depend on how dockers_v2 names its artifacts.
- The release workflow checks that no `TODO` is left in the release notes before publishing (9.3 `publish`).
- **The Dockerfile's frontend is pinned by digest** (decided after group 8; README §6; README S73 implements it,
  §6.1). `dockers_v2` builds with BuildKit, which pulls the frontend that the Dockerfile's `# syntax=` line names
  before anything else. With `docker/dockerfile:1` that is whatever the tag points at on the day of the release, and
  a release image would then depend on a Docker Hub tag that no digest fixes, unlike its base image. The line
  carries the frontend's exact version and digest (`docker/dockerfile:1.27.1@sha256:…`), so the release build, CI's
  `docker-build.sh` and `task docker:smoke` all use the frontend that was chosen. Dependabot does not update it: it
  is bumped by hand with the tool pins (§6.1, §7.1).

### 9.3 `release.yml`

```
tag v* ──► version ──► web ──► build ──► sign ──► verify ──► publish ──► site
                                         │ Environment "release": runs only after the owner approves
                               a draft release exists; nothing is public but the exact image tag :<v>
dry run:   version ──► web ──► build ──► sign-dry ──► verify
```

As README S73 built it; the workflow's header comment says the same in short. Every job has a timeout, the token is
read-only unless a job raises its own permissions, third-party actions are pinned by commit SHA, checkout never
persists credentials, and **nothing in the workflow uses a cache**: a release builds from the module proxy and npm
alone. Registry logins are a plain `docker login` with the job's token, not a third-party action. One run per tag
(`concurrency`), never cancelled half-way by another run.

**`version`** computes the version once: the tag without `v`, or `0.0.0-dryrun.<run>` in a dry run. `web` and
`build` both use it.
- **A release starts from a pushed tag.** A manual run with `dry_run` off fails; a run for anything but a tag fails.
- **The tag must be `vX.Y.Z` or `vX.Y.Z-rc.N`** (9.1); anything else fails before a job builds.
- Its outputs: `version`, `tag`, `dry-run`, `prerelease` (an `-rc.N` tag, and every dry run), the floating image tags
  that `publish` moves (`0.<minor> latest` in 0.x, `<major>.<minor> <major> latest` from 1.0, none for a
  prerelease), and Task's pin from `.tool-versions`.

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
- `needs: [version, web]`. Permissions: `contents: write`, `packages: write`, `id-token: write`, `attestations: write`.
- **No Node on this runner**: no setup-node, no npm, no npm cache. Only goreleaser and pinned Go tools run.
- Steps, in this order. Every check of the repository's inputs (steps 2, 5 and 6) comes before goreleaser, so a
  failed one leaves no draft and no image. The asset check of step 8 reads goreleaser's output, so it comes after
  it, as do the attestations and the image's signature: a failure there leaves the draft and the image `:<v>`, which
  a re-run for the same tag replaces (the draft by `replace_existing_draft`, 9.2; the image by the same tag pushed
  again):
  1. checkout with `fetch-depth: 0`;
  2. **compat snapshot check** (final releases only; skipped for `-rc.N` and in a dry run), right after checkout,
     before the download, the tool setup and goreleaser: fails with "run task release:compat VERSION=<v> in a
     release-prep PR (9.1)" unless `internal/protocol/testdata/compat/<v>/` exists, holds no symbolic link (a link
     to the fixtures would always match them and freeze nothing), and `diff -r internal/protocol/testdata/v1
     internal/protocol/testdata/compat/<v>` finds no difference (01 §14.3); a missing, a linked and a different
     snapshot each have their own error. S73 built it; README S75 owns `release:compat` and moved this check to the
     front, where it fails fastest;
  3. download the `web-dist` artifact into `web/dist/`;
  4. setup-go (from `go.mod`), Task (the pin), cosign v3, syft, buildx; `task release:goreleaser` (7.1); rpm, for
     `release:assets`;
  5. **the installer carries the release keys** (not in a dry run): fails while `deploy/keys/allowed_signers` is
     missing or empty, else runs `task lint:keys`. A released install.sh with an empty key block would refuse to
     install anything (9.4, `deploy/keys/README.md`);
  6. **the repository has the root `README.md`** that every archive carries (§3), in a dry run too;
  7. `docker login ghcr.io`, then `goreleaser release --clean`, which creates the **draft** release and pushes
     `ghcr.io/moonwx/isshoni:<v>`. Its before hook (`task release:prepare`, 7.2) runs no npm: it fails unless
     `web/dist/version.json` is `<v>`, then writes `THIRD_PARTY_NOTICES` (Go only, 8.4) and stamps `install.sh`. A
     dry run runs `goreleaser release --snapshot --clean --skip=publish` with `ISSHONI_SNAPSHOT_VERSION` instead;
  8. `task release:assets VERSION=<v> FULL=true` (7.2): the asset list of §3, with the SBOMs, the cosign bundle and
     `README.md` in every archive;
  9. `actions/attest-build-provenance` for `dist/*.tar.gz dist/*.zip dist/*.deb dist/*.rpm dist/checksums.txt
     dist-extra/install.sh` (a dry run attests its files too);
  10. not in a dry run: the image digest via `docker buildx imagetools inspect ghcr.io/moonwx/isshoni:<v> --format
     '{{json .Manifest}}'`, then `cosign sign --yes ghcr.io/moonwx/isshoni@<digest>` and `attest-build-provenance`
     with that `subject-digest` and `push-to-registry: true`. A dry run instead uploads its release files (the
     archives, packages, SBOMs, `checksums.txt` and its cosign bundle, `install.sh` and the compose files) as the
     artifact `release-assets`, kept 3 days, for `sign-dry`, which hands them on to `verify` signed.
- `docker/setup-buildx-action` runs BuildKit as a privileged container in this job, which holds the write
  permissions: its `driver-opts` pin the image by the digest of its multi-arch index
  (`moby/buildkit:v0.33.1@sha256:…`, the release that the action's default `buildx-stable-1` named), bumped by hand
  with the monthly tool pins of 7.1 (README F09; review R4).
- Timeout 45 min.

**`sign`**
- Not in a dry run; `needs: [version, build]`; `environment: release`; permissions `contents: write`. Runs only after
  the owner approves.
- Steps, with no actions but GitHub's checkout (sparse: `deploy/keys`) and upload-artifact:
  1. `gh release download "$TAG" --dir rel` (the draft is visible to `GITHUB_TOKEN`);
  2. `(cd rel && sha256sum --strict -c checksums.txt)`, so every asset matches what is about to be signed and a
     malformed line fails, and **nothing is in the draft that the signature would not cover**: the job fails on an
     asset, hidden ones included (`shopt -s dotglob nullglob`), that `checksums.txt` does not list (`checksums.txt`
     itself, its `.sig` and its `.sigstore.json` excepted; a `.sig` is there when the job runs again after it
     uploaded one). It is `publish`'s check without the signature, so a draft that `publish` would refuse is never
     signed. It stays inline in `sign` and `sign-dry`, and `deploy/test/check-draft-test.sh` fails if either
     loses `--strict`, sets `dotglob nullglob` only after its `for f in rel/*` loop or not at all, drops that loop
     or its `exit 1`, gets an `if:` or `continue-on-error:` from the check to the signature, or checks after it
     signs (README F15; reviews F09-R3, F15-R2);
  3. fails when the environment has no secret `RELEASE_SSH_SIGNING_KEY`; removes an earlier `checksums.txt.sig`
     first (without a terminal, `ssh-keygen -Y sign` keeps an existing signature file and still exits 0); writes
     the key under `umask 077` into `$RUNNER_TEMP` and deletes it when the step ends;
  4. `ssh-keygen -Y sign -f "$RUNNER_TEMP/k" -n isshoni-checksums rel/checksums.txt`, and prints the key's
     fingerprint, which the owner compares with the one in `README.md` (`deploy/keys/README.md` step 5);
  5. `ssh-keygen -Y verify -f deploy/keys/allowed_signers -I isshoni-release -n isshoni-checksums -s
     rel/checksums.txt.sig < rel/checksums.txt`, which catches a wrong key in the secret;
  6. `gh release upload "$TAG" rel/checksums.txt.sig --clobber`, so a re-run of the job replaces its signature;
  7. uploads `rel/`, the files it checked with their new signature, as the artifact `release-signed`, kept 7 days,
     hidden files included (`include-hidden-files: true`; upload-artifact leaves them out by default): `verify`
     installs these, so that it needs no token that can read the draft (README F09).
- Timeout 10 min.

**`verify`**
- `needs: [version, build, sign, sign-dry]` with `if: !cancelled() && needs.build.result == 'success' &&
  (needs.sign.result == 'success' || needs.sign-dry.result == 'success')`: one of the two sign jobs is always
  skipped, and a plain condition would skip this job too (`publish`, below). Permissions `contents: read`,
  `packages: read` (the image is private until the owner makes the package public, 8.6), `attestations: read`.
  **No write token** (README F09, review R4 below): the container runs third-party code as its root. `GH_TOKEN` is
  set only on the two steps that ask GitHub (`cosign verify` on the image, after a `docker login` that the step
  undoes, and `gh attestation verify`), which run before the container starts.
- Matrix: `debian-12`, `ubuntu-24.04`, `fail-fast: false`; each with its base by the digest of its multi-arch index
  (`docker.io/library/debian@sha256:…  # debian:12`), bumped by hand with the monthly tool pins (7.1): Dependabot
  doesn't read a workflow's matrix.
- Downloads the signed files, sign's artifact `release-signed` (a dry run: sign-dry's, and its `release-keys-dryrun`,
  whose key goes into that installer's key block in place of the production keys), into `mirror/download/<tag>/`,
  and serves them with `tools/relserve` (HTTPS with a test CA, on every address, so that the rootless container
  reaches it as `host.containers.internal`, 4.13).
- Checks that the installer is stamped with `<v>`; `cosign verify-blob` on the checksums bundle with the identity
  of 4.3 (`https://github.com/<repo>/.github/workflows/release.yml@<ref>`, the Actions OIDC issuer); `cosign verify
  ghcr.io/moonwx/isshoni:<v>` with the same identity and issuer (not in a dry run); `gh attestation verify` on the
  `linux_amd64` archive.
- Before the container, a systemd user session for rootless podman: unless `/run/user/<uid>/bus` is there,
  `sudo loginctl enable-linger` for the runner's user; `XDG_RUNTIME_DIR` and `DBUS_SESSION_BUS_ADDRESS` go into
  `GITHUB_ENV`; the step fails unless podman's cgroup manager is then `systemd` (4.13 says why).
- In a systemd container (`deploy/test/distro/Containerfile`, as C1 of 11.2), built with `podman build` and started
  rootless with `podman run -d --systemd=always --cap-add SYS_ADMIN --network slirp4netns` (no `--privileged`; 4.13
  says why the capability, which is the container's own, and the network are named), fetches the **released,
  stamped `install.sh` with the production key** with `curl` and runs
  it without a terminal: `--yes --tls-mode off` with `ISSHONI_DOWNLOAD_BASE`,
  `ISSHONI_PUBLIC_URL=http://127.0.0.1:8080` and a documentation-range `ISSHONI_PUBLIC_IP`, so the runner's own
  address stays out of the log. Then the service is active, and both `isshoni version --short` and
  `/api/v1/info`'s `server.version` are `<v>`. A container whose systemd doesn't come up, or that doesn't reach
  `relserve`, fails its step with the container's log (and podman's cgroup setup) or relserve's; a later failure
  prints the service's journal and relserve's log.
- Timeout 20 min.

**`publish`**
- `needs: [version, verify]`, with **`if: ${{ !cancelled() && needs.verify.result == 'success' &&
  needs.version.outputs.dry-run == 'false' }}`**. The explicit result check is needed: a job-level `if` without a
  status function means `success() && …`, and `success()` looks at every job upstream, not only at `needs`. On a tag
  `sign-dry` is skipped, so a plain condition would skip `publish` after a green `verify`, and the run would end
  green with the release still a draft and the floating tags not moved (actions/runner#491; the dry run can't show
  it, because it skips `publish` by design).
- Permissions: `contents: write`, `packages: write`. Checks out `deploy/keys` and `deploy/release` (sparse).
- Fails if the draft body still contains `TODO` ("edit them on GitHub, then re-run this job").
- **Checks the draft once more, right before it goes public** (README F09, review R4): `gh release download` into
  a new `rel/`, then `sh deploy/release/check-draft.sh rel deploy/keys/allowed_signers` (README F15, review
  F09-R1), which fails unless all three checks pass, in this order: `ssh-keygen -Y verify -f
  deploy/keys/allowed_signers -I isshoni-release -n isshoni-checksums -s rel/checksums.txt.sig <
  rel/checksums.txt`; `(cd rel && sha256sum --strict -c checksums.txt)`; and the asset check of `sign`: every
  file of the draft, hidden ones included, is listed in `checksums.txt`, except `checksums.txt`, its `.sig` and
  its `.sigstore.json`. The signature comes first, so `sha256sum` reads only names that the key signed.
- **`deploy/release/check-draft.sh DIR ALLOWED_SIGNERS`** as README F15 built it: POSIX sh, run with the runner's
  `sh` (dash). It reads nothing but the two paths and changes nothing; exit 0 with a line that says `ok`, 1 when a
  check fails (or the draft has no `checksums.txt` or `.sig`, or `ALLOWED_SIGNERS` is missing or empty), 2 for
  usage. Hidden files count without `dotglob`: it walks both `DIR/*` and `DIR/.*`, skips `.`, `..` and a pattern
  that matched nothing, and names every unlisted file, a directory too, not only the first. A file is listed when
  a line of `checksums.txt` is `<hex>  NAME` or `<hex> *NAME` for its whole name (taken from the environment, since
  `awk -v` would turn a backslash into an escape), so a name that starts, ends or extends a listed one is not
  listed. In GitHub Actions (`GITHUB_ACTIONS=true`) each error is an `::error::` annotation, with control
  characters shown as `?`, so that an asset's name can't start a workflow command of its own, and `%` as `%25`
  (as `@actions/core` escapes it: the runner decodes `%25`, `%0D` and `%0A` in a message, so a name with `%0A` in
  it would show as two lines); the step adds one annotation that names the tag.
- **Its test**, `deploy/test/check-draft-test.sh`, which `task test:sh` runs under dash and `bash --posix` (the
  script under the same shell; not busybox, whose own `sha256sum` has no `--strict`). It makes an ephemeral release
  key and another key with `ssh-keygen`, an `allowed_signers` for the first, and a draft as `sign` leaves it (the
  asset names of §3, `checksums.txt`, its cosign bundle and `.sig`); each case copies that draft, changes one thing
  and asserts the exit code and the error of the check meant to catch it. Passing: the draft as signed, from
  another directory and with relative paths as `publish` gives them; without the cosign bundle; with a hidden asset
  that the signed `checksums.txt` lists; with a `<hex> *NAME` line. Failing at the asset list: an added asset, an
  added hidden file, a name that extends, starts or ends a listed one, two added assets (both named), a directory,
  a missing `checksums.txt` line signed again, a hidden copy of `checksums.txt`. At the checksums: a changed
  `install.sh`, a missing asset, a malformed line signed again. At the signature: a changed `install.sh` with
  `checksums.txt` to match (unsigned, and signed by another key), a signature of another key, of the right key in
  the namespace `file`, a broken one, none, no `checksums.txt`, a missing line not signed again, a bad signature
  together with an added asset (only the signature is reported), and an `allowed_signers` that is empty, missing,
  for another principal or for another namespace. Then usage (exit 2, `-h`); the annotations with a name that
  holds a line break and one that holds `%0A` (shown as `%250A`; outside Actions as it is); and `release.yml`
  itself (review F15-R1, R2), with the lines it looks for matched whole: `publish` runs the script as `if ! sh
  deploy/release/check-draft.sh rel deploy/keys/allowed_signers; then` with `exit 1` as the only exit before that
  `if`'s `fi`, after the last `gh release download` and before the first `--draft=false`, with no `if:` or
  `continue-on-error:` from that download to `--draft=false`; it checks out `deploy/release` and keeps no inline
  copy of the check. `sign`'s and `sign-dry`'s inline checks run, in this order and before `ssh-keygen -Y sign`,
  `(cd rel && sha256sum --strict -c checksums.txt)`, `shopt -s dotglob nullglob`, and the `for f in rel/*` loop
  whose unlisted branch ends in `exit 1`, with no `if:` or `continue-on-error:` from the check's step to the
  signature. No key is committed: the keys live in the test's temporary directory.
- A final release: `gh release edit "$TAG" --draft=false --latest`. A prerelease: `--draft=false --prerelease
  --latest=false`, so GitHub's "latest" stays where it is (goreleaser sets none, `make_latest: false`, 9.2).
- Unless it is a prerelease: `docker buildx imagetools create -t ghcr.io/moonwx/isshoni:<tag> …
  ghcr.io/moonwx/isshoni:<v>` for each floating tag of the `version` job (`0.<minor>` and `latest` in 0.x).
  Copying the index keeps the digest, so the cosign signature stays valid.
- Timeout 10 min.

**Review R4** (the M1 security review; README §6 "Decided after group 10" (1)), as README F09 fixed it. Until F09
`verify` held a `contents: write` token in its job environment while it ran a privileged container from Docker Hub
bases that no digest pinned (4.13), and `publish` took the draft as it was: no new download, no `ssh-keygen -Y
verify`, no `sha256sum -c`, no check for assets that `checksums.txt` doesn't list. That went against this
section's rule of no write token near third-party code. Now, as the job descriptions above say: `sign` (and
`sign-dry`) checks the draft as `publish` will, hidden files included, and uploads the files it checked, with the
signature, as the artifact `release-signed`; `verify` takes them from there in both modes with `contents: read`
(plus `packages: read` and `attestations: read`) and `GH_TOKEN` on two steps only, and starts the distro container
rootless (`podman run -d --systemd=always --cap-add SYS_ADMIN --network slirp4netns`, in the runner user's systemd
session) from bases pinned by digest, reaching `relserve` as `host.containers.internal` (4.13); `publish`
downloads the draft right before `--draft=false` and fails unless the signature, the checksums and the asset list
all pass; and `build`'s buildx driver image is pinned by digest too. Turning on GitHub's immutable
releases, which the one-time setup of 8.6 doesn't include, is the owner's to consider: they would also stop a write
token from changing the assets of a release that is already out.

**`site`** (README S82)
- `uses: ./.github/workflows/site.yml`, after `publish`, with `contents: read`, `pages: write` and `id-token: write`;
  it deploys from the tag's ref, which the `github-pages` environment allows (8.6 step 5).
- A `release` event created with `GITHUB_TOKEN` doesn't trigger other workflows, so the site deploy is called directly.
- **It needs `if: ${{ !cancelled() && needs.publish.result == 'success' }}`**, not a plain condition, for the reason
  given at `publish`: `sign-dry`, skipped on a tag, is upstream of it too. The workflow's header comment says so.
- With Pages still off it fails once the release is out (`site.yml`'s `check` job, 10.5), since the stable install
  URL would serve nothing: turn Pages on, then run Site from `main`.

**Dry run** (`workflow_dispatch`, input `dry_run: true`, the default of a manual run):
- `web` builds the SPA as `0.0.0-dryrun.<run>`; `build` runs `goreleaser release --snapshot --clean --skip=publish`
  with `ISSHONI_SNAPSHOT_VERSION=0.0.0-dryrun.<run>`, checks the assets with `FULL=true`, attests them, and uploads
  them as an artifact. It skips the release-key check, the compat check, the registry login and everything about
  the image.
- goreleaser's snapshot still **signs `checksums.txt` with cosign keyless** (it doesn't skip signs), so every dry run
  leaves public entries in Sigstore's Rekor transparency log, as its attestations do.
- A `sign-dry` job (**no environment**, no permissions) makes the same checks and signs with an ephemeral key made
  in the job, verifies with it, and uploads the signed files as `release-signed`, as `sign` does, and the key's
  `allowed_signers` as `release-keys-dryrun`.
- `verify` runs against those files with an `install.sh` that carries that key.
- Nothing needs approval, and nothing is published. This proves the pipeline before the first real tag.

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
- The fingerprints (`ssh-keygen -lf`, or what `task release:keys` prints, 7.2) are published in the "Release
  signing keys" section of the root `README.md` (README F05 writes the section, without a fingerprint until the
  keys exist) and on `/security`. `deploy/keys/README.md` is the owner's checklist for these steps, before the
  first release candidate.

**Rotation (or a leak):**
1. Add the new key line to `allowed_signers`, and delete the old one: at once on a leak, otherwise in a later
   change. Don't mark it `valid-before=`: Windows 10's OpenSSH 8.1 can't parse that option, so `task lint:keys`
   refuses it (M2 06 §7.2).
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
  - a fresh Firefox may need about a minute to fetch OpenH264 before the first join (S4); one whose first join
    already listed H.264 and whose download takes longer than the server's 3 minutes of retries needs a reload
    (README §6 "Decided after group 9" (4); 02 §8.5);
  - DRM video shows black.
- **Not in M1:** update-manifest signing, installers, SignPath (apply after the first Windows release, M2), Homebrew
  tap (M3), signed apt/rpm repositories (Later).

## 10. Project site

### 10.1 Hosting and URLs

- VitePress (MIT, pinned in `docs/package-lock.json`) with `srcDir: docs`, `base: '/isshoni/'` and
  `srcExclude: ['PLAN.md', 'm1/**', 'm2/**']`. Design docs stay on GitHub, not on the site (open question Q3).
  As README S82 built it (`docs/.vitepress/config.mts`): `vitepress build docs` makes `docs/` the site's root, so
  `srcDir` stays at its default, which is the same directory; `srcExclude` also has `scripts/**`, the site's own
  tooling; `cleanUrls`, and no web fonts (`useWebFonts: false`). `docs/package.json` turns off the install scripts
  of `esbuild` and `fsevents` (`allowScripts`), as `web/` does for `fsevents`, so `npm ci` runs no third-party
  install script, and `docs/.npmrc` is `web/`'s (`engine-strict`, `fund=false`). It also overrides VitePress 1.6's
  Vite 5 with Vite 6.4.4 (`overrides`): the dev server advisories against Vite 5.4 and its esbuild 0.21 have no 5.x
  fix, and the built pages are the same (their text compared page by page; only the asset hashes differ). The
  override goes once a VitePress release requires a fixed Vite itself.
- **Site URL:** `https://moonwx.github.io/isshoni/`. Adding a custom domain later keeps these URLs working, because
  GitHub Pages redirects `github.io` to the custom domain.
- **Stable install URL:** `https://moonwx.github.io/isshoni/install.sh`. Mirror:
  `https://github.com/MoonWX/isshoni/releases/latest/download/install.sh`.
- **Privacy on the site:**
  - VitePress local search, not Algolia;
  - fonts bundled by the default theme, no Google Fonts;
  - no analytics, no cookies, no external embeds.

  The privacy page says that GitHub hosts the site and may log IPs. **`docs/scripts/check-site.mjs`** (README S82)
  checks the built site for it, in `ci.yml`'s `site` job and in `site.yml` after every build: every attribute
  that loads something (on any tag but `<a>` and `<area>`), every CSS `url()` and `@import`, and a
  `<meta http-equiv="refresh">` target must be a path of the site or a `data:` URL; no script, the inline ones of
  each page included (where a `head` entry would put an analytics loader), names a host that serves fonts,
  scripts, analytics, search or icons; and the config inlined in every page has local search and no Algolia. The
  theme's one such fallback, a named social icon from `api.iconify.design`, is accepted only while no page has
  such an icon (the config gives GitHub's as inline SVG). It is the automated form of S10's network-panel check.

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
| `/privacy` | The plan's "Privacy and trust model", word for word in substance, plus the **server's outbound connections exactly as 04 §16's table** (destination, when, what is sent, off switch), rendered in full: including STUN to Cloudflare and Google every 10 minutes (every 30 s while a server waits for its public address, 04 §16), the ACME-directory clock check, and the four push services (payloads encrypted, RFC 8291). 04 §16 is the single source; the page adds nothing and drops nothing. Also what the installer contacts (the site and GitHub) and what the site stores (nothing) |
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

  As README S82 built it (`site.yml`'s `build` job, the step "The latest release's installer, verified"):
  - the latest release is GitHub's `releases/latest` (never a prerelease, 9.3), whose tag must look like a
    release's (`vX.Y.Z`, or with `-rc.N`); called from a final release's tag, the step fails unless that tag is the
    latest, so a stale answer can't serve the previous installer;
  - the signature is checked with **main's** `deploy/keys/allowed_signers` (after a rotation the next release is
    signed with the new key, 9.4); a missing or empty key file, or a release without `checksums.txt.sig`, fails;
  - `install.sh`, `compose.yaml` and `compose.host.yaml` must each be listed in `checksums.txt` exactly once
    (`--ignore-missing` alone would pass a file that the list leaves out), then `sha256sum --ignore-missing -c`;
    `install.sh` must carry the release's version stamp (`ISSHONI_INSTALLER_VERSION`);
  - the site serves all three at its root (`/install.sh`, `/compose.yaml`, `/compose.host.yaml`), and the keys at
    `/keys/allowed_signers`; `release.json`'s fingerprints are computed from every key line of the file, as
    `ssh-keygen -l` prints them. These files are git-ignored. `docs/.vitepress/release.data.ts` reads
    `release.json` and fails the build when it is malformed; without the file (every local build, and every build
    before the first release) the pages say "not released yet".
  - After `vitepress build` and `check-site.mjs`, the step "The built site carries the verified installer and
    keys" compares `dist/install.sh` and `dist/keys/allowed_signers` with the SHA-256s that the verifying step put
    in its outputs, not with files: the build ran third-party code (every npm package) after the verification and
    could have changed any file of the workspace.
- **Before the first release** there is no `install.sh` on the site, and the install page says "not released yet".
- **An urgent installer fix** means a patch release. That keeps the served script signed and paired with a binary.
- **The key is pinned in four places, cross-checked:**
  - `deploy/keys/allowed_signers` (source of truth);
  - the block embedded in install.sh (checked by `lint:keys`);
  - the fingerprints on `/security` and in `README.md` (from `release.json` and the file). `README.md`'s are
    checked by `repocheck keys` (`lint:keys`, and release.yml) from README F11 on: once `allowed_signers` exists and
    is not empty, `README.md`'s "Release signing keys" section must list the SHA256 fingerprint of every key in it
    and no other one, and no row may still say the key is not created (a file of blank and comment lines only
    fails, as one without a key). Until F11 nothing checks them (README F05's review). As F11 built it: a line
    that names one key by its comment but shows another key's fingerprint fails too (the release and backup
    fingerprints swapped), the "not created" check reads every visible line of the section (HTML comments and
    what follows the next level-1 or level-2 heading outside a code fence don't count), a key line that
    `x/crypto/ssh` can't parse fails with its line number, and the README is checked even where `install.sh` is
    missing (the key block is then skipped with a note);
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
(any Go change) and in the `site` job (8.2), so a docs-only or codes-only PR runs it too. As README S82 built it,
it reads the `CloudProvider` constants from every non-test Go file of `internal/protocol/api`, not only
`conntest.go`, so the test keeps working if they move, and it also checks the fixed pages and anchors of the last
row below (the install pages, `/security#verify` and `#keys`, `/install/reverse-proxy#docker`) and the
`/privacy`, `/code-signing` and `/reference/config` pages that the product links to.

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

**As README S82 built it** (`build ──► deploy ──► check`):
- **`build`** runs the steps above and three more: "Is GitHub Pages on?" (`pages: read`;
  `GET repos/{repo}/pages` answers 404 while Pages is off, and a Pages site that builds from a branch fails the
  run, since Source must be "GitHub Actions", 8.6 step 5), and after the build `check-site.mjs` (10.1) and the
  re-check of the served installer and keys (10.3). Its outputs say whether Pages is on, whether a release exists,
  and the version and the installer's SHA-256.
- **`deploy` runs only once Pages is on**, and only from `main` or a `v*` tag, which the `github-pages`
  environment allows (8.6 step 5). Its concurrency group `pages` never cancels a deploy that is under way
  (`cancel-in-progress: false`). GitHub keeps at most one deploy waiting, and a newer one takes the waiting one's
  place, so that run's deploy ends cancelled while the newest run serves the latest site.
- **`check`** (a job of its own, with an explicit condition: a job-level `success()` would skip it with `deploy`)
  runs once a release exists, on `main` or a `v*` tag. It fails when `deploy` didn't succeed (Pages still off, or
  a failed deploy: the stable install URL would serve nothing or an old script) and says how to fix it; else it
  fetches the served `/install.sh` with a query string of its own per attempt, to get past the CDN's cache, for up
  to 10 minutes until its SHA-256 is the release's, and then asks GitHub again which release is the latest: a
  release published while this run built (a docs push just before a release, whose deploy queued after the
  release's own) fails it, and a new run from `main` serves the new one.
- **Before the first release `main` stays green while Pages is off**: the site is built and checked, the step
  warns that Pages is not on, `deploy` is skipped and `check` doesn't run. Once a release exists, a run without
  Pages fails in `check`, release.yml's `site` job included (README §6 "Decided after group 11" (3)).
  Whether the workflow token's `pages: read` may read `GET repos/{repo}/pages` is to be seen in the first run after
  Pages is on (README §6 "Owner actions").
- **Rules of 8.1 as applied**: no top-level `concurrency` (8.1's one exception); no caches, like `release.yml`;
  `GH_TOKEN` only on the steps that call `gh`, never on `npm ci` or the build, which run third-party code. The
  `push` trigger also has `deploy/keys/allowed_signers` and `site.yml` itself (8.1).

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
- **firewalld** (README S78), with `systemctl` and `firewall-cmd` replaced by functions that record their calls:
  - the offer asks `systemctl is-active --quiet firewalld.service` before `firewall-cmd --state`, and calls
    `--reload` before `--permanent --add-service=isshoni` and once more after it (4.9);
  - `--uninstall` with the service in the default zone, in a second zone and in a policy, and with a rich rule
    that names it: each of them is removed, and the rule printed in full, before `isshoni.xml` goes; a zone that
    refuses the change is named in a warning, and the uninstall goes on (4.11). As README S78 wrote them, the
    firewalld stand-in writes the files of the zones and policies it changed, as firewalld does, and the tests
    also cover: zones whose files don't mention the service are not asked; `isshoni.xml` stays, with the file
    named in the warning, when a zone refuses, when firewalld is stopped and `firewall-offline-cmd` is missing,
    when a zone file was edited by hand and not reloaded, and when firewalld lists no zones; with firewalld
    stopped, `firewall-offline-cmd` and its own options do the work and nothing reloads; the `.xml.old` copies and
    a mention that names no service don't count; a second run finds nothing to remove; and `--no-firewall`
    leaves the firewall alone.
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

**As README S67 built the setup** (`deploy/test/distro-test.sh`, `deploy/test/distro/Containerfile`,
`tools/relserve`; `task deploy:test`, with `DISTRO=<name>` for one), with scenarios C1 and C6 (README S78 adds the
rest):
- **The test release is a snapshot release** (README S78; S67 built it from the working tree, before goreleaser
  existed): `task release:snapshot` with `ISSHONI_SNAPSHOT_VERSION` set to `0.0.1-ci.1` and then `0.0.1-ci.2`,
  without the images (`--skip=docker`), which builds the web app for each version first. The scenarios take from
  each snapshot what a release serves the installer: the two linux archives, `checksums.txt` and `install.sh`,
  stamped with the version by goreleaser's before hook. The run then signs each `checksums.txt` with a key pair
  made for it, puts that key between `install.sh`'s key markers, and updates `install.sh`'s line in
  `checksums.txt`. A snapshot needs `web/node_modules` (`task setup`) and replaces `dist/`, `dist-extra/` and
  `web/dist` of the working tree, as `task release:snapshot` always does. With warm caches a snapshot takes about
  16 s per version; the first one on a cold machine also builds goreleaser.
- **`tools/relserve`** serves the release root over HTTPS the way GitHub Releases does (`/download/v<ver>/<file>`,
  and `/latest` → 302 to the newest tag), with a certificate authority made at its start whose key is never
  written; it writes the CA certificate for the container and its bound address to files. The container reaches
  it as `host.containers.internal` (podman provides the name; docker gets it with `--add-host`) and trusts the CA
  through `update-ca-certificates` or `update-ca-trust`.
- **The containers**: one image per distro (`isshoni-distro-test:<distro>`, from `deploy/test/distro/Containerfile`)
  with systemd as PID 1, D-Bus, curl, `ss`, procps and python3 (the tests' JSON reads), and **without** the
  OpenSSH client, which the installer has to install itself. The six distros of §4.13 run one after the other;
  the images stay as the next run's build cache, everything else (key, CA, release, containers) is deleted at the
  end. `CONTAINER_ENGINE` picks podman or docker (podman when installed), `KEEP=1` leaves the container for a look
  after a failure.
- **The installer runs as an admin runs it**: fetched with curl and piped into `sh` without a terminal, with
  `ISSHONI_DOWNLOAD_BASE` pointing at relserve, `ISSHONI_PUBLIC_URL=http://127.0.0.1:8080`, and
  `ISSHONI_PUBLIC_IP=203.0.113.7` (RFC 5737), which keeps the real address of the machine out of the output that
  ends up in CI logs.
- **C1 as built** asserts, besides the row below: that the pre-check ran and checked DNS (`[ ok ] dns`: off mode
  decides it without the network) and the public address, **not that the pre-check passed**: doctor's NAT check
  still asks the STUN servers when `public_ip` is set, so its `public_ip` result depends on the network of the
  machine that runs the containers (behind Docker Desktop it fails, as a symmetric NAT). Also: the config has the
  given `public_ip`; the service is enabled; the program is `root:root 755`, the unit
  (`/usr/local/lib/systemd/system/`) `root:root 644`, `/etc/isshoni` `root:root 755`; the installer printed
  "isshoni 0.0.1-ci.1 is running.", a `/setup#` link and its firewall line; and no `ISSHONI_*` variable reached the
  service's environment.
- **C6 as built** is an upgrade to ci.2 with `--yes` from three mirrors that each have one thing wrong
  (`checksums.txt` with one character of the archive's sum changed, no `.sig`, the archive of ci.1 in ci.2's
  place). Each run exits 3 with its reason and "Nothing was installed or changed."; the program, the config and
  the running version are unchanged, no `.isshoni.new` is left, and the service was not restarted
  (`ActiveEnterTimestamp`).

**As README S78 built the rest** (`deploy/test/distro-test.sh`, `deploy/test/distro/`; `task deploy:test`):
- **Flags**: `distro-test.sh [-r DIR] [-o PARTS] [DISTRO...]`, or `-b DIR` to build the two test releases into
  DIR and stop; `-r DIR` takes them from such a DIR instead of building them. CI's `distro-release` job runs `-b`
  once, and each `distro` job `-r` with its distro (8.2). Without a DISTRO all six run, one after the other.
- **Parts** (`-o`, default all): `main`, one container with, in this order, C12 (non-root without sudo, checked
  to have installed nothing), C1, C2, C3, **C6 before C4** (the tampered mirrors need ci.1 installed and ci.2 not
  yet), C4, C5, C8, C9 and C10; `c7` and `c13`, a container each; `c11`, the distro's plain image without systemd;
  `v3` (below).
- **V3 also runs in containers**, on every PR that touches deploy, not only in the nightly's virtual machines
  (D8 assumed containers couldn't test firewalls; S62 found that privileged systemd containers run a real
  firewalld and ufw): on `fedora-44` (firewalld) and `ubuntu-24.04` (ufw), in a container from an image with the
  firewall (`<image>:<distro>-firewall`). The firewall offer opens the ports, and `--uninstall` takes them out
  again. On Fedora the service is first put into a second zone, a policy and a rich rule, and written by hand into
  a third zone's file that firewalld has not read: that file keeps `isshoni.xml` (4.11), and after the uninstall
  firewalld must still load its configuration (`firewall-cmd --reload` succeeds, `--state` is `running`). README
  S83 runs V3 in the nightly's VMs too, with these scenario functions and an executor over SSH in place of the
  container engine (8.5).
- **Names**: `IMAGE_NAME` names the images and prefixes the containers (default `isshoni-distro-test`), so a
  run can keep away from another run's images and containers; the images stay as the next run's build cache,
  everything else is deleted at the end. `CONTAINER_ENGINE` picks podman or docker (podman when installed; CI
  sets docker, 4.13), `KEEP=1` leaves the containers and the work directory for a look after a failure.
- **Counts**: a distro runs 123 checks (131 on ubuntu-24.04 and 141 on fedora-44, with V3); the group 10
  integration ran all six locally in Docker. The GitHub path (the `distro-release` job, Docker on the runner) is
  checked only by CI, on the pull request.

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

**VM scenarios** (nightly; V3 also runs in containers since README S78, above):

| # | Scenario | Asserts |
|---|---|---|
| V1 | Everything in C1–C10 on real systemd | as above, plus the sysctl values applied (`sysctl -n net.core.rmem_max` = 8388608) |
| V2 | Fedora with SELinux enforcing | the service starts; `ausearch -m avc -ts boot` shows no isshoni denials; `ls -Z /usr/local/bin/isshoni` shows `bin_t` |
| V3 | ufw enabled first (Ubuntu), firewalld running (Fedora). On Fedora the test also puts the service into a second zone and into a policy, and adds a rich rule that names it, before `--uninstall` | `--yes` adds the profile or service (on firewalld right after `isshoni.xml` was put in place, which takes the reload before `--add-service`, 4.9); `--uninstall` removes it, on Fedora also from the second zone, the policy and the rich rule, which it prints; after it `firewall-cmd --reload` succeeds and `firewall-cmd --state` is `running` (4.11); SSH is still reachable |
| V4 | Oracle pattern: the Ubuntu VM gets `netfilter-persistent` and a REJECT rule | rules inserted before the REJECT and tagged `isshoni`; they survive a reboot; uninstall removes them |
| V5 | ACME with Pebble plus `pebble-challtestsrv` on the runner. The VM uses `ISSHONI_TLS_ACME_CA` and `ISSHONI_TLS_ACME_CA_ROOT` (04's keys `tls.acme_ca`, `tls.acme_ca_root`), and challtestsrv resolves `isshoni.test` to the VM | `--domain isshoni.test --yes` is ready within 60 s; the certificate issuer is Pebble. IP mode (`--ip`) the same, if the pinned Pebble supports IP identifiers; otherwise IP mode stays on the manual release checklist |
| V6 | `systemd-analyze security isshoni` | score ≤ the recorded value + 0.2 |

**As README S83 built the VM runs** (`nightly.yml`'s `distro-vm`, 8.5; since README F13 the script
`deploy/test/vm/vm-test.sh`, which starts Pebble and challtestsrv for V5 itself, 4.13), where the table leaves a
choice:
- **V1** is the `main` part: the container scenarios C1–C6, C8–C10 and C12 in that VM, in the order of the
  container run, plus the sysctl values; C7 and C13 get a VM each, as they get a container each. In a VM, C1's two
  checks that the installer installs the OpenSSH client are skipped and labelled so: an SSH server needs the
  client, so the VM always has `ssh-keygen`.
- **V4** inserts and checks the rules for IPv4 and IPv6 (`rules.v4` and `rules.v6`, both with a REJECT rule), as
  4.9's iptables row does for both; the rules survive a reboot of the VM, and `--uninstall` removes them.
- **V5 runs domain mode only** (`--domain isshoni.test`). The pinned Pebble does issue for IP addresses (`ci.yml`'s
  `acme` job), but ip mode takes only a public address (04 §4.5: a documentation or private `public_ip` is a config
  error), and a VM on a runner has no public address that Pebble could validate. So IP mode stays on the manual
  release checklist, the fallback the row allows, and the step prints a note saying so. Readiness within 60 s is
  measured from the service's start.
- **V6 reads the recorded value** from `deploy/systemd/README.md`'s heading "## Recorded exposure: <value>" (4.5),
  so a newly recorded score moves the limit with it; without the heading the step fails.

### 11.3 Docker smoke: `deploy/docker/test/docker-smoke.sh`

1. Build the image with `docker buildx build --platform linux/<arch> --load -f deploy/docker/Dockerfile` from a
   context laid out like goreleaser's (`linux/<arch>/isshoni` plus the extra files).
2. Make a test certificate with openssl (a throwaway CA). Manual TLS mode proves 443 binds without depending on ACME.
3. **Bridge:** `docker compose -f deploy/compose.yaml -f deploy/docker/test/compose.smoke.yaml up -d`. The override
   sets the local image, `ISSHONI_TLS_MODE=manual`, the certificate mount and `ISSHONI_PUBLIC_IP=127.0.0.1`, and
   puts `smoke.test` on `127.0.0.1` in the container's `/etc/hosts` (`extra_hosts`, README F21: the doctor step's
   `dns`). Asserts:
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
   In both setups also **doctor** (README F21): `docker compose exec -T isshoni isshoni doctor --json` reports
   from the running server (`mode` `cli-with-server`), exits 5 exactly when a check fails (0 otherwise), and every
   check of `isshoni doctor --list-checks` has the code the script wants for it, or one its allow-list names for
   this engine (below).
5. **No volume:** `docker run --rm <image>` exits 78 within 10 s and logs a message containing `/var/lib/isshoni`.
6. **No `.env`:** `deploy/compose.yaml` started from a directory without a `.env` file, with an override that sets
   the local image and points the ACME directory at a closed loopback port (`ISSHONI_TLS_ACME_CA`, below), so ip
   mode asks no real CA. `ISSHONI_DOMAIN` and `ISSHONI_PUBLIC_IP` reach the container as empty strings. Asserts:
   - the container keeps running (no exit 78 over an empty value);
   - `docker compose exec -T isshoni isshoni config print --json` shows `domain` and `public_ip` with source
     `default`, not `env`, and the derived TLS mode `ip` (04 §4.2 empty-means-unset rule). No certificate is
     expected: the runner can't pass ACME.
7. **Healthcheck:** `docker inspect` shows a HEALTHCHECK whose command is `/usr/local/bin/isshoni healthcheck`.

**As README S68 built it** (`deploy/docker/test/`; `deploy/docker/README.md` has the table of parts):
- **The files are in `deploy/docker/test/`**, next to the image, not in `deploy/test/` (install.sh's):
  `docker-smoke.sh`, `docker-build.sh` (builds the image from the working tree without goreleaser, from a build
  context laid out like goreleaser's: `linux/<arch>/isshoni`, `deploy/docker/rootfs/`, `LICENSE`, `NOTICE`, `THIRD_PARTY_NOTICES`), and
  one compose override per part: `compose.smoke.yaml` (bridge), `compose.smoke.host.yaml` (host) and
  `compose.smoke.noenv.yaml` (no `.env`). Each override sets the local image (`SMOKE_IMAGE`, `pull_policy: never`)
  and what its part needs; `docker-smoke.sh` exports the `SMOKE_*` variables.
- **The compose project is `isshoni-smoke`** (`docker compose -p`), so the test never touches a real install,
  whose project is `isshoni`; a project of that name left by an earlier run is removed first. Step 3's network is
  therefore `isshoni-smoke_default`, not `isshoni_default`.
- **The part without `.env` (step 6) points the ACME directory at a closed port** of the container's own loopback
  (`ISSHONI_TLS_ACME_CA: https://127.0.0.1:9/directory`): ip mode would otherwise ask Let's Encrypt for a
  certificate at every test run. No certificate is expected there.
- **Parts and flags**: `image` (the HEALTHCHECK, the user, that the binary starts without capabilities, the
  version), `novolume`, `bridge`, `backup` and `doctor` (each in bridge and in host; `doctor` from README F21),
  `ipv6`, `host` and `noenv`; `-s` skips parts,
  `-i IMAGE` tests an image that exists (a release image) and builds nothing, `-a` picks the architecture, `-k`
  keeps what the test made. `task docker:smoke` builds the web app and the notices first; flags after `--` go to the
  script.
- **On Docker Desktop** (macOS, Windows) the engine runs in a virtual machine: the IPv6 client is seen with the
  machine's forwarding address (6.4), and the host network of `compose.host.yaml` is the machine's, so the host
  part checks only what it can see from outside (healthy, confined, `setup-url` through `exec`, the data surviving
  `down` and `up`) and skips the HTTPS checks, the owner of port 443, the admin account and the backup, with a note.
  CI runs every part on Linux.

**The doctor step as README F21 built it** (`check_doctor` in `docker-smoke.sh`; part `doctor`, `-s doctor` skips
it). It runs right after the hardening checks, in the bridge and in the host setup, on every engine (the host part's
doctor runs through `exec`, so it needs nothing from outside the machine), and makes doctor's report a CI assertion
that the manual check of 04 §17 can't be:

- **Per code.** `doctor_want` lists one code per check of 04 §13.2: what a server with this setup gives on a host
  that has what a real install has (`ok`, or the `info` code where `info` is the best a check gives: `ports`,
  `firewall_hint`, `container`, `nofile`, `bandwidth`; `release.not_checked`, since the first look is 5 to 60
  minutes after the start). A check that `--list-checks` names and the list doesn't fails the step, so a new check
  gets an expectation of its own (on its own pull request: below); so does a report whose checks differ from that
  list.
- **The allow-list** (`doctor_allowed`), each entry with its reason in the script, replaces a check's code for one
  engine: `local` (a Docker engine on the Linux machine that runs the test, as CI's runner), `vm` (an engine in a
  virtual machine or on another host: Docker Desktop) or `any`. Nothing else is allowed, a `fail` or `warn` that it
  doesn't name included:

  | Check | Engine | Allowed | Why |
  |---|---|---|---|
  | `public_ip` | any | `public_ip.none` (local also `ipv6_only`) | The test has no public address, so the overrides set `ISSHONI_PUBLIC_IP=127.0.0.1`, which doctor must not take for a public one (04 §13.2 rule b). The step also checks that `params.unreachable` is `["127.0.0.1"]` and that there is no `ipv4`. A host part on a Linux machine with a global IPv6 address finds that one: `ipv6_only`, whatever the NAT kind of its IPv4 path, which stands in front of no public address (rule b) |
  | `dns` | any | `dns.unverified` | `smoke.test` is known to no DNS server; the overrides put it on `127.0.0.1` in the container's `/etc/hosts` (`extra_hosts`), so every machine resolves it alike, whatever its resolver answers for the name (Docker Desktop asks the computer's own, which may be a proxy tool's that answers every name). With no public address doctor can't judge the record: never `dns.ok`, never `dns.wrong` |
  | `clock` | local, vm | `clock.ok`, `clock.unsynced` | Manual mode has no ACME directory for the skew, so only the kernel's sync flag decides, and it is the Docker host's: a runner whose time daemon hasn't marked the clock synchronized yet, Docker Desktop's VM whose clock the hypervisor keeps |
  | `udp_buffers` | local, vm | `udp_buffers.ok`, `udp_buffers.low` | `net.core.rmem_max`/`wmem_max` are the Docker host's (6.4), and a test doesn't change a host's sysctls: the runner keeps the kernel's default, and Docker Desktop's VM has a limit of its own below the 8 MiB isshoni wants |

- **Two more facts**: `container`'s `params.network` is the setup's (`bridge`, `host`), which 04 §13.2's rule (a)
  needs; and the summary line names the allowed results it saw.
- **As run after F21** on Docker Desktop for Mac (the `vm` engine), both setups: `public_ip.none`, `dns.unverified`,
  `clock.unsynced`, `udp_buffers.low`, every other check its wanted code; exit 5. 04 §13.2's rule (a) is not part of
  the step (the loopback public address decides `public_ip` first); its Go tests hold it, and F21 checked it by hand
  with the same image: an offline `isshoni doctor --only public_ip` in a bridge container on Docker Desktop, whose
  STUN reading is `symmetric`, gives `public_ip.no_ipv6` with `dockerBridge`, and with `--network host`
  `public_ip.symmetric` as before.
- **As first run on CI's Linux runner** (the `local` engine), on the pull request that brought group 15 into `main`:
  the step passed, with the allowed results `public_ip.none`, `dns.unverified`, `clock.ok` and `udp_buffers.low`.
  The `local` list had been chosen by reasoning, not seen; that run is its proof (README §6 "Decided after group
  15" (1)).
- The step doesn't reach README F22's `dockerBridge` in the dashboard or the connection test (04 §7.7, §11.4): the
  loopback `public_ip` decides `public_ip` first, so no smoke run has a bridge NAT that passes, and the step asks
  neither endpoint. `TestDockerBridgeInServer` (04 §17), with a fake machine for doctor, is what holds that path
  (README §6 "Decided after group 16" (4)).
- "doctor reports ok in the Docker bridge and host setups" stays the manual pre-flight item 5 of 12.1, on a Linux
  host with a real public address: the step checks every code, but the smoke overrides can't give an all-ok report.
- **Doctor's package is an input of the step.** `doctor_want` and `doctor_allowed` pin every check and its codes, so
  a pull request that changes doctor's checks or codes (`internal/server/ops/doctor`, the `go` filter alone) must
  run docker-smoke, which a pull request runs only for the `docker`, `deploy` or `release` filters (8.2). One thing
  covers part of it, and the rest is still missing:
  - `TestDockerSmokeExpectations` (`ops/doctor`, in `test-go` on every Go change) holds both lists to doctor:
    `doctor_want` names every check of `--list-checks` in its order, and every code either list names is one of
    doctor's codes and its check's; an allow-list entry is `local`, `vm` or `any`, one per check and engine. A pull
    request that adds or removes a check, or renames or removes a code the lists name, fails there until the script
    knows it, and that edit of `deploy/docker/**` runs docker-smoke on the same pull request.
  - Which of its existing codes a check gives in the smoke's setup only the step can tell. ci.yml's `changes` job has
    a `doctor` filter (`internal/server/ops/doctor/**`, 8.2) that runs docker-smoke on a pull request that changes
    doctor's package, so such a change fails on its own pull request. README F21 named the filter; it was added after
    group 17 (README §6 "Decided after group 15" (3)).
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
5. **Doctor in Docker** (README S90's last acceptance item, made manual after group 13, README §6 "Decided after
   group 13"), run earlier than T−1 and never on the installed VPS: there the systemd isshoni holds 80, 443 and 7882,
   and `compose.host.yaml` would mount its data directory, `/var/lib/isshoni`, as root. So during Setup, on the VPS
   before `install.sh`, or on another Linux host with a real public address, in a directory with `deploy/`'s two
   compose files and a `.env`: both files' `image:` set to the rc's `ghcr.io/moonwx/isshoni:0.1.0-rc.N` (a
   prerelease never moves `:latest`, 3), `ISSHONI_DOMAIN` naming that host (or empty, for an IP certificate), and
   the host's UDP buffers raised (6.4). In this order:
   - bridge: `docker compose up -d`, `docker compose exec isshoni isshoni doctor`, record the output, then
     `docker compose down -v`, which frees the ports and deletes the volume (the host setup doesn't use it);
   - host: `docker compose -f compose.host.yaml up -d`, `docker compose -f compose.host.yaml exec isshoni isshoni
     doctor`, record the output, then `docker compose -f compose.host.yaml down` and `sudo rm -rf /var/lib/isshoni`,
     the directory it created on the host as root, which `install.sh` would otherwise take over.

   Both report ok. Docker Desktop on a Mac can't give this result: since README F21 a bridge's symmetric NAT passes
   (04 §13.2 rule (a)), but in the host setup the NAT is the VM's own and stays `public_ip.symmetric`, and the VM's
   clock and UDP buffers warn. Nor can CI's docker-smoke, which since F21 asserts every check by its code in both
   setups (11.3) but whose overrides have no public address (`127.0.0.1`, 04 §13.2 rule (b)) and a test name that
   no DNS server knows (04 §13.2, §17).

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
`sudo systemd-run --collect --unit=exittest-collect --working-directory="$PWD" "$PWD/exittest" collect -o run.csv
-metrics http://127.0.0.1:9469/metrics` (or inside `tmux`). `--collect` unloads the transient unit however the
collector ends, so the same command works again after a start that failed (a typo is exit 2, and without it the
failed unit would stay loaded under that name; review S94-F7). Every 10 s it records:
- the isshoni PID, re-resolved on every sample with `systemctl show -p MainPID --value isshoni`; a changed PID is
  written to `run.csv` as a `restart` row, so the T+60 restart is marked;
- isshoni CPU % (from `/proc/<pid>/stat` of that PID), RSS, threads and open fds;
- load average;
- NIC rx/tx bytes (`/proc/net/dev`);
- UDP `InDatagrams`, `OutDatagrams`, `RcvbufErrors`, `SndbufErrors` and `InErrors` (`/proc/net/snmp`, plus the
  `Udp6*` lines of `/proc/net/snmp6`, which Linux counts apart; clients may come over IPv6);
- TCP `RetransSegs`.

Every 30 s it saves a `/metrics` scrape.

`tools/exittest summarize run.csv` prints the table below. It computes every rate and total per process (split at the
`restart` rows) and treats a counter that goes down as a reset, the Prometheus way (the increase after a reset is the
new value), because the saved `/metrics` counters start again at zero at T+60. The owner then adds the journal
excerpt: the session's warnings and errors, with every IP address and name replaced. isshoni writes JSON lines (04
§10) without a `<N>` priority prefix, which journald stores at one priority, so `journalctl -p warning` doesn't find
them: the run book filters `journalctl -u isshoni --since … --until … -o cat` on the JSON `level` (`WARN`, `ERROR`)
and on a panic's first line, and takes systemd's own messages about the unit (a crash, the failed state) with `-p
warning` (review S94-F1).

**As README S94 built it** (`tools/exittest` in the module `tools/`; the run book `docs/m1/exit-test/README.md` and
its results template `results-template.md`):
- **Build and copy.** `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go -C tools build -o ../bin/exittest ./exittest`
  writes the binary into the checkout's ignored `bin/`, from where it is copied to the VPS. `run.csv`, the saved
  scrapes and the journal stay off the repository: they are copied off the VPS into the ignored `.dev/` (or outside
  the checkout), never next to tracked files (review S94-F6).
- **Flags.** `exittest collect [-o run.csv] [-metrics URL] [-unit isshoni | -pid-file FILE] [-nic NAME]
  [-metrics-dir DIR] [-proc /proc] [-interval 10s] [-scrape-interval 30s]`, with defaults that match the command
  line above. `-unit` names the systemd unit whose `MainPID` is isshoni; `-pid-file` reads the PID from a file
  instead (a server that systemd doesn't run, and the SIGTERM test); `-nic` defaults to the interface of the IPv4
  default route with the lowest metric (`/proc/net/route`), and without one, or with `all`, counts every interface
  except `lo`; `-metrics ""` scrapes nothing; `-metrics-dir` defaults to `-o`'s name without `.csv` plus `-metrics`;
  `-proc` is where the proc file system is (the tests give a fake one). Exit 0 when a signal stopped it, 1 when
  `run.csv` can't be written, 2 on a usage error. `exittest summarize [-t0 TIME] run.csv` is below.
- **`run.csv`.** One header line, then one row per event, oldest first, each row one write (a killed collector
  leaves whole rows). `time` is UTC with milliseconds; `kind` is one of `start` (the collector started; started
  again with the same `-o`, it appends after a new `start` row and still marks a restart in between; the note says
  how it samples, `user_hz=100` among it), `sample`, `down` (a sample without an isshoni process: systemd names
  none, or the process ended between the lookup and the read; the machine's columns are still filled), `restart`
  (the PID differs from the last one, or the same PID now belongs to a process that started later, told by
  `/proc/<pid>/stat`'s `starttime`; the note names the old PID), `scrape`, `scrape_error` (no metrics: refused,
  failed, timed out or not the text format; the note says why) and `stop` (the note says why: a signal, or the
  error it couldn't go on after); then `pid` and the columns, an empty cell where the row's kind doesn't fill a
  column or the value couldn't be read, and a free-text `note` last:
  - the process: `cpu_ticks` (utime + stime) and `cpu_pct`, its CPU since the previous sample **in percent of all
    CPUs** (100 = every CPU busy), `rss_bytes` (`VmRSS`), `threads` and `fds`;
  - the machine: `cpus` and `host_cpu_pct` (the whole machine's CPU since the previous sample, from `/proc/stat`,
    in percent of all CPUs; two columns beyond the list above, because "Server CPU" can be read as the machine's,
    so the summary shows both and judges by isshoni's own), `load1`, `load5`, `load15`, `nic_rx_bytes`,
    `nic_tx_bytes`, `udp_in_datagrams`, `udp_out_datagrams`, `udp_rcvbuf_errors`, `udp_sndbuf_errors`,
    `udp_in_errors` and `tcp_retrans_segs` (counters since boot);
  - the scrape: each one is saved whole as `<metrics-dir>/<UTC time>.prom` (the row's note names the file), and
    the series of the list below become columns, each the sum of a metric's series or of those with given labels:
    `go_goroutines`, `process_resident_memory_bytes`, `process_cpu_seconds_total`, `participants`, `shares`,
    `sfu_downtracks`, `sfu_video_downtracks_high` and `sfu_video_downtracks_low` (the video DownTracks forwarding
    `high` or `low`), the SFU's ingress and egress bytes, NACKs served, PLIs sent and layer switches,
    `transfer_ingress_bytes_total`, `transfer_egress_bytes_total`, `ws_resume_resumed_total`,
    `ws_resume_not_resumed_total`, `transport_udp`, `transport_tcp443`, `transport_tcp7882` and the six `client_*`
    counters, so `summarize` needs only `run.csv`. The parser skips and counts a line it can't read rather than
    failing the scrape.
- **CPU ticks** are read with a constant USER_HZ of 100: the standard library has no `sysconf` without cgo, and 100
  holds on x86-64, arm64 and riscv64.
- **How `summarize` judges.** It prints Markdown with `###` sections and no title, so it goes under the results
  file's "## Summary" as it is. A row is `pass`, `FAIL`, `check` (measured, needs a look), `n/a` (nothing to
  measure) or `manual` (the people, the dashboard or the survey answer it: manual fixes, phones and the survey,
  and the numbers it gives for recovery, viewing quality and ICE-TCP). Where the pass criteria are not numeric:
  - **T+0** is `-t0` (RFC 3339, or `HH:MM[:SS]` in UTC within 12 hours of the first row); without it, 60 minutes
    before the first restart, or 15 minutes after the first row when there is none (the collector starts at T−15).
  - **The recording covers the session** when it starts by T+2 and ends at T+118 or later: 2 minutes of slack at
    each end, because T+0 often comes from the restart, which the collector sees seconds late. A gap of more than
    2 minutes without a sample inside the session is listed.
  - **Session**: a restart within 5 minutes of T+60 is the scripted one, and more than one restart is `FAIL`. No
    restart, a restart elsewhere, a recording that doesn't cover the session, a gap, or several scraped byte
    counters going down at once without a PID change make it `check`; the last is a restart in place by re-exec
    (04 §6.5), which keeps the PID and its start time (review S94-F5).
  - **Server CPU**: isshoni's CPU from T+0 to T+120, the average and the peak (the largest pair of samples) in
    percent of all CPUs, with the machine's beside it; `FAIL` at 50 % average or 80 % peak; `check` when the
    samples cover less than 90 % of T+0 to T+120 or none of the stress, T+90 to T+100 (review S94-F4).
  - **Memory**: RSS from the first sample at or after T+65 to the last at or before T+120, in the process that runs
    at T+65; `FAIL` at 20 % growth, `check` when those samples are more than 2 minutes inside the window; the first
    process's T+5 to T+59 is reported beside it. **Goroutines** is a row of its own: the last scrape after the
    stress (T+100 to T+120) against the last one at or before T+85 of the same process; `pass` up to 10 % above
    that baseline, otherwise `check` (more people or shares raise it too).
  - **UDP socket errors**: the machine's `RcvbufErrors + SndbufErrors` against all datagrams; `FAIL` at 0.01 %,
    `check` when the recording doesn't cover the session.
  - **Recovery** stays `manual`. For the server restart the summary gives the outage as a range: at most from the
    old process's last scrape with everyone (the most participants of its last 5 minutes) to the new one's first,
    at least from the first sign of the outage, or the new PID's `restart` row when that comes first (the old
    process had stopped by then), to the last sign in between (a scrape with fewer participants, a failed scrape, a
    `down` row), since participants are scraped every 30 s. The new PID's row is no sign that the outage went on:
    it comes up to a sample late. It is `check` when even the shorter end is over 30 s, or when the participants
    never came back (review S94-F2).
  - **Viewing quality** is `FAIL` only when the viewers' freeze time is at least 3 % of the video tracks' watch
    time, every device together (the phones' limit, the larger one); otherwise `manual`, because the split per
    device comes from the dashboard and the notes. The watch time is the forwarding video DownTracks times time,
    because a viewer reports its freezes per inbound video track, the focus and each thumbnail (review S94-F3);
    dropped frames and concealed audio are shown beside it.
  - **ICE-TCP** is `FAIL` only when no scrape between T+0 and T+120 has a PeerConnection on `tcp443`; otherwise
    `manual`, listing the scrapes that had none, because E's own transport is on the dashboard.

  Then come each process's CPU, memory and gauges, and every counter's total and rates per process and in all.
  The output names no host, address or person. The golden fixture is `tools/exittest/testdata/run.csv` (about
  196 KB, a simulated session with the restart) with `summary.md`; after a format change
  `go -C tools test ./exittest -run 'Fixture|Summarize' -update` writes both again. Tests: `TestCollectSample`,
  `TestCollectNIC`, `TestCollectAppends`, `TestCollectSurvivesRestartAndOutage`, `TestScrapeRows`,
  `TestCollectStopsOnSIGTERM` (the binary as a child process), the parsers' tests, `TestRealProc` (Linux only: the
  real `/proc`), `TestStdlibOnly`, `TestFixtureIsCurrent`, `TestSummarizeFixture`, `TestSummarizeVerdicts`,
  `TestSummarizeCoverage`, `TestCounterResets` and `TestSummarizeT0`; the fuzz targets `FuzzParsePromText` and
  `FuzzSummarize`. `TestRealProc` has passed on linux/arm64; CI's linux/amd64 job is its first amd64 run.
- **The results template** has the timeline's per-person rows (at T+0 C and D; B and E signed up at T−5), the
  summary pasted under its "## Summary", the journal excerpt, the survey and the device-matrix rows checked that
  day (review S94-F8).

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
| Unit | `deploy/test/check-draft-test.sh` (9.3; `test:sh` under dash and `bash --posix`, 7.2) | `deploy/release/check-draft.sh` passes a good draft signed with throwaway keys and fails an added asset, a hidden file, a changed `install.sh`, a bad or missing signature and a missing checksums line; `release.yml`'s `publish` calls it, and `sign`/`sign-dry` keep `--strict` and `dotglob nullglob` (README F15) |
| Unit | `web/scripts/licenses.test.mjs` (Node's test runner: `task test:web` and the CI `licenses` job) | the SPDX `OR`/`AND` rules; a missing license fails; a GPL-only dev dep fails; `(MIT OR GPL-3.0)` passes; overrides apply |
| Unit | `tools/notices` golden test on a fixture module tree | deterministic output; Apache `NOTICE` included; a non-allowlisted license fails; the Go runtime entry is present |
| Unit | `tools/relserve` | `/latest` redirect format matches GitHub's; files served; the CA verifies |
| Static | `lint-deploy` | shellcheck, shfmt, actionlint, `systemd-analyze verify`, the key block, `.tool-versions` = `go.mod` toolchain; through `test:sh`, the VM harness's check of the names it takes from `distro-test.sh` (`vm-test.sh -h`, 7.2) |
| Integration | distro containers C1–C13 (11.2) | install, idempotency, upgrade, downgrade refusal, tamper refusal, port conflict, re-runs after exit 7 and exit 5, uninstall, purge, backup/restore |
| Integration | Docker smoke (11.3) | bridge and host bind 443 as designed, healthcheck, setup-url via exec, persistence, backup (a `0600` file) → restore through `exec -T` with `--yes`, an IPv6 client seen as the bridge gateway (6.4), no-volume refusal, start with no `.env`; in bridge and host, `isshoni doctor --json` with every check asserted by its code and a named allow-list per engine (README F21) |
| Integration | `goreleaser-check` | the asset names and archive contents of section 3 |
| Integration | site | the build, nothing loaded from another origin (`check-site.mjs`, 10.1, README S82), and **anchor coverage**: `go test ./internal/server/ops/doctor -run TestDocsAnchors` (10.4: reads `docs/troubleshooting.md` and `docs/install/vps.md`; ids from the doctor registry, the `CloudProvider` constants and `web/src/conntest/codes.json`; as README S82 built it, it parses every non-test file of `internal/protocol/api` for the constants and also checks the fixed pages and anchors the product links to) |
| E2E | `site.yml`'s `check` job (10.5) | the served `/install.sh` is the latest release's, once a release exists |
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
  - `systemctl reload` sends SIGHUP (`ExecReload`, 4.5): the server reads manual TLS files and `log.level` again
    and names every other changed key for a restart (README S90; it ignored the signal from S65 until then, 04
    §6.5).
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
| S12 | Exit test (1 d prep + the session) | `tools/exittest` (collect, summarize; as README S94 built them, 12.3), the run book as a checklist, the session, the results file | All pass criteria in 12.3 met on an rc; `v0.1.0` tagged from that commit (or a fixed one after a re-run) | everything |

README S94 is this slice, split after group 13 (README §6 "Decided after group 13"): its code, `tools/exittest` and
the run book in `docs/m1/exit-test/`, was built in group 14 (12.3 "As README S94 built it": `run.csv`'s rows and
columns, the flags beyond the command line, the `Udp6` counters, USER_HZ 100 and the summary's rules where the pass
criteria are not numeric); the session itself, 05 §19.4's device matrix and the `v0.1.0` tag stay with the owner
(README S94-day), after README S92's load gate on a VPS.

## 17. Decisions taken at integration (formerly open questions)

- **Project site address**: `https://moonwx.github.io/isshoni/` for M1 (free, in line with the owner's
  budget-conscious stance); a custom domain can come later, and GitHub redirects the old URLs.
- **Merging into `main`**: pull requests only, with `ci-ok` required and an admin bypass for emergencies. M1 work
  lands on `m1/server-web` first (see `README.md`, "Branches and PRs").
- **Design docs on the site**: `docs/PLAN.md`, `docs/m1/` and `docs/m2/` stay on GitHub only (`srcExclude`).
- **Docker image tag in `compose.yaml` during 0.x**: `:latest`; release notes flag migrations, and the server backs
  up before migrating. The docs show how to pin `:0.<minor>`.

Decided after group 7 (an engineering call the owner delegated; README §6):

- **`systemctl reload isshoni` does nothing until the server can reload.** The unit keeps
  `ExecReload=/bin/kill -HUP $MAINPID`; the server ignores SIGHUP from README S65 on and reloads manual TLS files
  and `log.level` on it from README S90 on (4.5; 04 §6.5). Before S65 the signal would stop the server without a
  restart. README S90 merged in group 13, so the reload is there now.

Decided after group 8 (an engineering call the owner delegated; README §6):

- **The Dockerfile's `# syntax=` line is pinned by digest, like its `FROM` line**, so no build, release builds
  included, pulls a floating Dockerfile frontend from Docker Hub (6.1, 9.2). README S73 implemented it: the line is
  `# syntax=docker/dockerfile:1.27.1@sha256:…`, and the comment below it says how it is kept current (group 9's
  decision 3, below).

Decided after group 9 (engineering calls the owner delegated; README §6):

- **Release package file names use `-` where the package's version has `~`** (3, 9.2). GitHub renames a release
  asset with a `~` in its name (to `.`), and the draft would then no longer match `checksums.txt`, so the first rc
  would stop in the `sign` job after the owner's approval. The package's own version keeps the `~`
  (`0.1.0~rc.1`), so dpkg and rpm order a prerelease before its release.
- **The deb and rpm packages never reload firewalld on install**; only install.sh's firewall offer does (4.9, 5). A
  reload drops the admin's runtime-only rules, and the packages ask nobody: their `postinstall.sh` prints the reload
  before `--add-service` for the admin, and `preremove.sh` reloads only after it took the service out.
- **The Dockerfile's `# syntax=` digest and the goreleaser pin are bumped by hand** with the monthly bump of the
  tool pins (6.1, 7.1). Dependabot reads neither: its `docker` ecosystem reads only `FROM` lines, and goreleaser is
  built from `tools/goreleaser/go.mod`, which stays out of its `gomod` directories and follows `.tool-versions`.
- **A known M1 limit for the release notes**: after `decoder_failed`, a viewer's codec retries start again only on
  a caps change or a reload, so a Firefox whose OpenH264 download outlasts the 3 minutes of retries needs a reload
  (9.5; 02 §8.5).

Decided after group 10 (an engineering call the owner delegated; README §6 (1)):

- **The M1 security review's finding in the release pipeline (R4) is fixed** by README F09, merged in group 12,
  where no other slice touched `release.yml` or `cmd/isshoni` (README S82 had the first in group 11, S90 has the
  second in group 13): `verify` has no write token and runs its container rootless from bases pinned by digest,
  and `publish` checks the draft again before it publishes it (4.13, 9.3). F09 also fixed R5, which is 04's:
  `admin backup` without `--out` never writes into a file that already exists at the default name (04 §12.3). The
  review was read-only, of `main` at f7053dc, before the first release candidate; README §6 has its other findings
  and their slices. Group 12's notes on F09 are below.

Decided after group 11 (engineering calls the owner delegated; README §6 (3), (5)):

- **The site deploys only once GitHub Pages is on** (10.5). `site.yml` asks first; before the first release a run
  without Pages stays green with the site built and checked and a warning, so `main` doesn't go red over a setting
  of the owner's (8.6 step 5); once a release exists, a run on `main` or a release tag that didn't deploy fails, so
  a release can't end green while its stable install URL serves nothing.
- **The nightly departed from this doc twice; the first, until README F13** (8.5): its VM harness was inline in
  `nightly.yml`, not a script in `deploy/test/vm/` (4.13), because S83 could change only that file. F13 moved it
  to `deploy/test/vm/vm-test.sh`. The second stays: `goreleaser-full` doesn't sign (`--skip=sign`), because
  cosign's keyless signature would need an OIDC token in a job that runs npm and a public transparency-log entry
  every night, while `release.yml`'s dry run covers the signing.

Decided after group 12 (an engineering call the owner delegated; README §6 (8)):

- **README F09's `verify` departs from the design in three ways, which this doc now has** (4.13, 9.3). Its rootless
  `podman run -d --systemd=always` also takes `--cap-add SYS_ADMIN` (the container's own, in its user namespace: systemd
  needs it for the unit's sandbox, or the server refuses `/var/lib/isshoni` as not a mounted volume) and
  `--network slirp4netns` (with pasta, the default from podman 5 on, the name `host.containers.internal` gets no address
  that reaches the runner); `relserve` listens on every address, since slirp4netns reaches the runner's own address and
  not its loopback; and a step first makes sure that the runner's user has a systemd user session, without which
  rootless podman falls back to cgroupfs and systemd can't come up in the container. `sign` checks the draft's assets as
  strictly as `publish` and uploads the files it signed, hidden ones included, as `release-signed` (with
  `actions/upload-artifact`, GitHub's and pinned by SHA like checkout), which `verify` installs.
- **What F09 left for later**: the release dry run on GitHub (`workflow_dispatch` with `dry_run`) is the proof that
  the hosted runner boots systemd in a rootless container, and runs once `m1/server-web` is pushed (README §6
  "Owner actions"); `publish`'s draft check was tested only by one-off runs taken out of the workflow, which README
  F15 (group 13) turned into `deploy/release/check-draft.sh` with a committed test under `task test:sh` (9.3); and
  `distro-test.sh`'s podman path lacked `--cap-add SYS_ADMIN`, which README F16 (group 13) added with `SYS_PTRACE`,
  leaving the user session to the caller and stopping with its cause when there is none (4.13). The image digests that F09 and F13 pin are bumped by hand with the tool pins (7.1).

Decided after group 13 (engineering calls the owner delegated; README §6 "Decided after group 13"):

- **"doctor reports ok in the Docker bridge and host setups" is a manual check on a Linux host with a real
  address, run with the exit test** (12.1 pre-flight 5; README S90's last acceptance item). Docker Desktop can't
  produce an all-ok report (its user-space NAT makes `public_ip` symmetric, its fake-IP DNS answers for a test
  domain, its VM's clock and UDP buffers warn), and neither can 11.3's smoke overrides on a Linux runner (the test
  domain resolves nowhere). README F21 (group 15) runs `isshoni doctor --json` in the `docker-smoke` job and
  asserts per code (11.3), after revising two `public_ip` rules (04 §13.2).
- **README S94 (this doc's S12) is split** (16): `tools/exittest` and the run book in `docs/m1/exit-test/` are
  built in group 14; the session, 05 §19.4's device matrix and the `v0.1.0` tag stay with the owner, after README
  S92's load gate on a VPS.
- **The e2e job gets `isshoni-loadtest` from the `build` job** (8.2; the group 13 integration, README S89-R2):
  `CGO_ENABLED=0 go build -trimpath`, uploaded as its own artifact and passed to the specs as `LOADTEST_BIN`, so
  `decodable.spec` doesn't build the tool with an unpinned Go inside the 15 minutes that the whole Playwright run has in
  CI (05 §19.3). The tool is no release artifact.
- The first item's later slice is README F21 (group 15): `isshoni doctor --json` asserted per code in
  `docker-smoke`, with an allow-list for what a runner can't satisfy, and the two `public_ip` rules reviewed
  (11.3; 04 §13.2).

Decided after group 14 (engineering calls the owner delegated; README §6 "Decided after group 14"):

- **The exit test's collector and summary as README S94 built them** (12.3 "As README S94 built it", 16). CPU is
  in percent of all CPUs, as the pass criteria count it, and `run.csv` also has the machine's (`cpus`,
  `host_cpu_pct`), because "Server CPU" can be read either way; the verdict uses isshoni's own. The UDP counters
  add `/proc/net/snmp6`'s `Udp6*`. Each scrape is saved whole and its series are a row of `run.csv`, so the
  summary needs that one file. Where the pass criteria are not numeric the summary applies rules of its own (T+0,
  2 minutes of coverage slack, the scripted restart within 5 minutes of T+60, goroutines within 10 % of the T+85
  baseline, viewing quality `FAIL` only at 3 % frozen, ICE-TCP `FAIL` only with no `tcp443` PeerConnection, and
  recovery left `manual` with the outage as a range); a row passes only on what was recorded.
- **The run book reads the journal by the JSON `level`** (12.3; review S94-F1): journald stores isshoni's JSON
  lines at one priority, so `journalctl -p warning` would show only systemd's lines about the unit. The collector
  runs under `systemd-run --collect`, the binary goes to the ignored `bin/`, and the recording to the ignored
  `.dev/` or outside the checkout.
- **A Linux container over a checkout set up on a Mac runs the root module's tests, not `tools/notices`'** (7.4;
  the group 14 integration): `.bin/go-licenses` is a darwin binary there.

Decided after group 15 (engineering calls the owner delegated; README §6 "Decided after group 15" (1), (3)):

- **docker-smoke asserts doctor per code, as README F21 built it** (11.3, 13). In the bridge and the host setup,
  every check of `isshoni doctor --list-checks` has the code `doctor_want` names or one that `doctor_allowed` names
  for the engine (`local`, `vm`, `any`), each entry with its reason in the script; the overrides put `smoke.test`
  on `127.0.0.1` in the container's `/etc/hosts`, so `dns` reads alike on every machine. The `local` list was
  chosen by reasoning, and the step's first run on CI's runner passed with it (`public_ip.none`, `dns.unverified`,
  `clock.ok`, `udp_buffers.low`). The manual pre-flight item 5 of 12.1 stays.
- **ci.yml's `doctor` filter** (8.2, 11.3; review F21-R2): a pull request that changes only doctor runs docker-smoke,
  which tells which code each check gives in the smoke's setup; `TestDockerSmokeExpectations` holds the step's lists to
  doctor's checks and codes on every Go change. Added after group 17.

Decided after group 16 (an engineering call the owner delegated; README §6 "Decided after group 16" (6)):

- **A toolchain patch for a vulnerability govulncheck reports is bumped at once**, outside the monthly bump of the
  tool pins (7.1): go1.27.3 for GO-2026-6809 (`crypto/x509`, reachable from the server), with `go.mod`,
  `tools/go.mod`, `tools/goreleaser/go.mod` and `.tool-versions` moved together, which `task lint:pins` checks.
