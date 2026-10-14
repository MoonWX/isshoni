# Install script

`install.sh` installs isshoni on a Linux server with systemd, or upgrades, repairs or removes an installation. It is
the recommended way to run isshoni; [Docker](/install/docker) is the other one.

<ReleaseNotice />

```sh
curl -fsSL https://moonwx.github.io/isshoni/install.sh | sh
```

You need:

- a Linux server with systemd, amd64 or arm64, with root or `sudo` (see [Supported distributions](#distros));
- a public IP address: a VPS is the easy case ([Choosing a VPS and its firewall](/install/vps));
- TCP 80, 443, 7882 and UDP 7882 open in your provider's firewall;
- optionally a domain name whose A record points to the server. Without one, isshoni gets a certificate for the
  server's IP address ([Certificates](/install/tls)).

Run it as an ordinary user with `sudo`, or as root. The same script is attached to every release, so this installs
exactly one version:

```sh
curl -fsSL https://github.com/MoonWX/isshoni/releases/download/v0.1.0/install.sh | sh
```

The latest release's copy is also at `https://github.com/MoonWX/isshoni/releases/latest/download/install.sh`.

## What the script does {#steps}

1. **Checks the system**: Linux, systemd, amd64 or arm64, root or `sudo`, and that isshoni was not installed from a
   deb or rpm package. It needs `curl` or `wget`, and `ssh-keygen` to check the download; when `ssh-keygen` is
   missing it installs `openssh-client` (`openssh-clients` on Fedora).
2. **Downloads and verifies the release**: the archive for your CPU, `checksums.txt` and its signature. It checks the
   signature with the release keys built into the script (`ssh-keygen -Y verify`), then the archive's SHA-256. If
   `cosign` or a logged-in `gh` is installed, it checks the Sigstore bundle and the GitHub build provenance as well.
   Any failure stops it with exit code 3, and nothing has been installed or changed.
3. **Installs** the program as `/usr/local/bin/isshoni`, creates the system user `isshoni`, the directories
   `/etc/isshoni` and `/var/lib/isshoni`, the systemd unit `isshoni.service`, the UDP buffer settings
   (`/etc/sysctl.d/60-isshoni.conf`, only where the current limits are lower) and the ufw and firewalld profiles.
4. **Asks one question**, only on a fresh install: your domain name, or Enter for the server's IP address. It writes
   `/etc/isshoni/isshoni.toml` with `isshoni config init`, and never edits that file afterwards.
5. **Checks before the start** with `isshoni doctor --only dns,public_ip,clock,firewall_hint`: that the domain points
   to this server, that the server has a public address, that the clock is right, and where your provider's firewall
   is. If a check fails it asks: re-enter the domain, use the IP address instead, or continue anyway.
6. **Checks the ports** 80, 443 and 7882. If another program uses one, it stops with exit code 7 and tells you how
   to run isshoni [behind that program](/install/reverse-proxy).
7. **Offers to open the ports** in ufw or firewalld when one of them is running (or in iptables, on images like Oracle
   Cloud's that block with their own rules). An inactive firewall is never turned on: that could lock you out of
   SSH.
8. **Starts isshoni** and waits for it to be ready: up to 3 minutes on a first start, while it gets its certificate.
9. **Prints the setup link** with a QR code. Open it to create the admin account. The link works once and for 24
   hours; `sudo isshoni setup-url` makes a new one.

If the script stops on the way, fix what it says and run it again: it goes on where it stopped.

```
isshoni 0.1.0 is running.

  Create the admin account (link valid for 24 hours, works once):
  https://share.example.com/setup#k3J…

  Status   systemctl status isshoni        Logs     journalctl -u isshoni -f
  Check    sudo isshoni doctor             Upgrade  run this installer again
```

## Options {#options}

Pass options after `sh -s --`, for example
`curl -fsSL https://moonwx.github.io/isshoni/install.sh | sh -s -- --domain share.example.com --yes`. A flag wins over
its environment variable.

| Flag | Environment variable | Meaning |
|---|---|---|
| `--domain NAME` | `ISSHONI_DOMAIN` | A certificate for NAME; skips the question |
| `--ip` | `ISSHONI_TLS_MODE=ip` | A certificate for the server's public IP address; skips the question |
| `--tls-mode off\|manual` | `ISSHONI_TLS_MODE` (also `auto`, `ip`) | Advanced. `off`: behind your own HTTPS proxy, needs `ISSHONI_PUBLIC_URL` ([reverse proxy](/install/reverse-proxy)). `manual`: your own certificate, `ISSHONI_TLS_CERT_FILE` and `ISSHONI_TLS_KEY_FILE` ([certificates](/install/tls)) |
| `--version V` | `ISSHONI_VERSION` | Install version V: `0.1.0`, `v0.1.0` or `latest` |
| `--yes`, `-y` | `ISSHONI_YES=1` | Never ask; take every default, which includes opening firewall ports |
| `--no-firewall` | `ISSHONI_NO_FIREWALL=1` | Never touch the firewall |
| `--firewall` | | Offer the firewall step again on an upgrade |
| `--no-start` | | Install only: no start, no certificate, no setup link. Running the script again without it does the first start |
| `--allow-downgrade` | | Allow an older version than the installed one ([Downgrade](#downgrade)) |
| `--reinstall` | | Install the same version again (repairs the program file) |
| `--uninstall` | | Remove isshoni; keeps its configuration, data and user |
| `--purge` | | `--uninstall`, then delete the configuration, the data and the user |
| `--help`, `-h` | | Show the usage |
| | `ISSHONI_PUBLIC_IP` | This server's public IP address, instead of detecting it |
| | `ISSHONI_PUBLIC_URL` | The URL friends open, for `--tls-mode off` (`https://your.domain`) |
| | `ISSHONI_TLS_ACME_EMAIL` | An optional contact address for the Let's Encrypt account |
| | `ISSHONI_DOWNLOAD_BASE` | A mirror of `https://github.com/MoonWX/isshoni/releases`. The signature is still checked against the keys built into the script |

**Exit codes**

| Code | Meaning |
|---|---|
| 0 | Done, or already up to date |
| 1 | Unexpected error (the message says at which step) |
| 2 | Bad usage |
| 3 | Verification failed; nothing was installed or changed |
| 4 | Unsupported system: not Linux, no systemd, no build for this CPU, no root or `sudo`, installed from a package, or an OpenSSH older than 8.1 |
| 5 | Installed, but isshoni did not become ready in time; the diagnostics are printed |
| 6 | Downgrade refused (see `--allow-downgrade`) |
| 7 | A port isshoni needs is used by another program |
| 8 | Cancelled |

## Without questions: automation and cloud-init {#non-interactive}

Without a terminal the script can't ask, so name the certificate with `--domain` or `--ip`, and add `--yes` to take
every default (it then opens the ports in ufw or firewalld if one runs):

```sh
curl -fsSL https://moonwx.github.io/isshoni/install.sh | sh -s -- --domain share.example.com --yes
```

As cloud-init user data for a new VPS:

```yaml
#cloud-config
runcmd:
  - curl -fsSL https://moonwx.github.io/isshoni/install.sh | sh -s -- --ip --yes
```

cloud-init keeps the script's output, and with it the setup link, in its log (`/var/log/cloud-init-output.log`).
Rather than copying the link from there, log in and run `sudo isshoni setup-url`: it makes a new link and cancels the
old one.

## Upgrade {#upgrade}

Run the script again. It installs the latest release and restarts isshoni; friends' apps reconnect by themselves.
Before it changes its database, the server writes a backup to `/var/lib/isshoni/backups/`. Your configuration file
is never touched. To install a given version, add `--version 0.2.0`.

### Downgrade {#downgrade}

An older version refuses a database that a newer one has already changed. If an upgrade goes wrong, the script prints
the way back with the exact file names:

```sh
sudo systemctl stop isshoni
sudo -u isshoni isshoni admin restore --offline /var/lib/isshoni/backups/pre-3-20261014T021500Z.db
curl -fsSL https://moonwx.github.io/isshoni/install.sh | sh -s -- --version 0.1.0 --allow-downgrade
```

## Uninstall and purge {#uninstall}

```sh
curl -fsSL https://moonwx.github.io/isshoni/install.sh | sh -s -- --uninstall
```

`--uninstall` stops isshoni and removes the program, the systemd unit, the UDP buffer settings and the firewall rules
it added. It keeps `/etc/isshoni` (the configuration), `/var/lib/isshoni` (database, certificates, backups) and the
`isshoni` user; running the script again brings isshoni back with them.

`--purge` deletes those as well, after you type `purge` (`--yes` skips the question). It makes no backup, so make one
first, while isshoni runs:

```sh
sudo isshoni admin backup --out /root/isshoni-backup.tar.gz
```

If you installed isshoni from a deb or rpm package, remove it with `sudo apt remove isshoni` or
`sudo dnf remove isshoni` instead.

## Verify the script by hand {#verify}

The signature check protects against changed release files, mirrors and broken downloads. It can't protect against
this site serving a changed `install.sh`. To be sure of what you run, download the script from the release and check
it yourself before you run it (here for 0.1.0):

```sh
V=0.1.0; B=https://github.com/MoonWX/isshoni/releases/download/v$V
curl -fsSLO $B/install.sh -O $B/checksums.txt -O $B/checksums.txt.sig
curl -fsSL https://moonwx.github.io/isshoni/keys/allowed_signers -o allowed_signers   # compare fingerprints with README
ssh-keygen -Y verify -f allowed_signers -I isshoni-release -n isshoni-checksums -s checksums.txt.sig < checksums.txt
sha256sum --ignore-missing -c checksums.txt
sudo sh install.sh
```

`ssh-keygen` prints the fingerprint of the key that made the signature, and `sha256sum` must print
`install.sh: OK`. [Security](/security#verify) explains each step and lists the fingerprints to compare with.

## Supported distributions {#distros}

| Distribution | Notes |
|---|---|
| Debian 12, Debian 13 | |
| Ubuntu 22.04, 24.04, 26.04 | |
| Fedora (the current release, 44 today) | SELinux enforcing and firewalld are tested |

Each of these is tested with every change to the installer, in containers, and in virtual machines before a release.
Other distributions with systemd (Rocky Linux, AlmaLinux, Arch, openSUSE) are community-tested: they should work,
and reports are welcome. The script does not support Alpine, WSL without systemd, or containers: use
[Docker](/install/docker) there.

Each release also has deb and rpm packages (amd64, arm64) for admins who prefer packages. They install the same
program and systemd unit under `/usr`, but ask nothing and start nothing: after the install they print the steps
(`isshoni config init`, the firewall, `systemctl enable --now isshoni`, `isshoni setup-url`). Upgrade them with the
next release's package. A server is installed either way, not both: the packages refuse to install over the script's
files.
