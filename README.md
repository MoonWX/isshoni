# isshoni

isshoni (一緒に, "together") is a self-hosted server for sharing screens and system audio with a group of friends.
Everyone can share and watch at the same time, while the group keeps talking in its usual voice app (Discord,
TeamSpeak, …). You run it on your own server; it is open source under the Apache License 2.0.

**isshoni is pre-release: nothing is released yet, and it is not ready for use.** See [Status](#status).

## Why

Discord keeps high-quality screen sharing behind Nitro. isshoni is a self-hostable alternative for friend groups:

- **One Go binary** holds the web app, the accounts and a WebRTC SFU (selective forwarding unit), with SQLite for its
  data. Each share goes up to the server once, however many friends watch. It is tuned for a VPS with a public IP.
- **Viewers install nothing.** Friends watch in the browser, phones included. On a phone, the web app can go on the
  Home Screen and notify you when someone starts sharing.
- **All system audio except voice apps** comes with the planned desktop apps: they share what you hear except Discord,
  TeamSpeak and the like, so friends never hear their own voices echoed back. Until then, you share from Chrome or Edge.

## Status

<!-- Before each release tag (the rc's too): bring this section, the banner above, "What v0.1.0 will ship" and the
Install intro up to date, and fill in the fingerprints under "Release signing keys" (deploy/keys/README.md step 2). -->
- **No release yet.** M1, "Watch together" (the server and the web client), is being built. It runs from source for
  development: see [CONTRIBUTING.md](CONTRIBUTING.md).
- **v0.1.0 ends M1.** Release candidates come first. v0.1.0 follows once five friends have finished a two-hour session
  on a small VPS with no manual fixes, with an iPhone and an Android phone among the viewers.
- **The desktop apps are later milestones**: Windows (M2), macOS (M3) and a Linux share agent (M4). Native iOS and
  Android apps are pending; phones watch in the browser. [docs/PLAN.md](docs/PLAN.md) has the whole roadmap.

## What v0.1.0 will ship

- **The server** for Linux (amd64, arm64): an install script, deb and rpm packages, and a multi-arch Docker image with
  compose files. The macOS and Windows archives are experimental, untested server builds for LAN and development use.
- **Accounts**: the first admin through a one-time setup link, then invite links and an optional approval queue.
  Everyone lands in the room "Lounge"; admins can add rooms.
- **Watching** in every current browser, including Safari on iPhone and iPad and Chrome on Android: a tile grid of
  everyone's shares, focus and fullscreen, and audio that follows the focused share. Each share is sent in two
  quality layers, so a small tile or a slow connection gets the lighter one.
- **Sharing** from Chrome or Edge on a desktop computer, best as a window together with its audio.
- **Setup**: HTTPS with a Let's Encrypt certificate for your domain or for the server's IP address (or your own
  certificate, or none behind your own reverse proxy); a setup wizard with a real connection test; `isshoni doctor`;
  backup and restore.
- **Signed releases** ([below](#release-signing-keys)) with SBOMs, build provenance and the third-party license
  notices, and the project site, with the install guide and the privacy and code-signing pages.

Known limits: no desktop apps yet; a whole-screen share from the browser includes the voice apps (the web app warns
you); on iOS, video plays only while the app is in the foreground, and sound needs a tap to unmute; a fresh Firefox
may need about a minute to fetch its H.264 decoder before the first join; DRM-protected video shows black.

## Install (from v0.1.0)

Not available yet: these commands work once v0.1.0 is released. The server needs Linux with systemd (Debian 12 and 13,
Ubuntu 22.04, 24.04 and 26.04, and the current Fedora are tested) or Docker, and the ports 80/tcp, 443/tcp and
7882/udp+tcp open in your cloud provider's firewall.

**Install script**, from the project site (`https://moonwx.github.io/isshoni/`, not live yet):

```sh
curl -fsSL https://moonwx.github.io/isshoni/install.sh | sh
```

It asks one question (your domain name, or Enter for the server's IP address), offers to open the ports in ufw or
firewalld, waits for the certificate, and prints the setup link with a QR code. It checks the signature of what it
downloads before it installs anything, and running it again upgrades. The latest release carries the same script:
`https://github.com/MoonWX/isshoni/releases/latest/download/install.sh`.

**Docker Compose**:

```sh
curl -fsSLO https://github.com/MoonWX/isshoni/releases/latest/download/compose.yaml
echo ISSHONI_DOMAIN=share.example.com > .env   # leave this out for a certificate for the server's IP address
docker compose up -d
docker compose exec isshoni isshoni setup-url
```

isshoni gets its certificate from Let's Encrypt. Using it means you accept the Let's Encrypt Subscriber Agreement:
<https://letsencrypt.org/repository/>

## Release signing keys

Every release's `checksums.txt` is signed with `ssh-keygen -Y sign`. `install.sh` checks that signature against the
public keys built into it before it installs anything, and stops if the check fails. The public keys will be in
[`deploy/keys/allowed_signers`](deploy/keys/README.md), and their fingerprints here, before the first release:

| Key | Use | Fingerprint |
|---|---|---|
| `isshoni-release-1` | signs every release | not created yet |
| `isshoni-release-backup-1` | kept offline; signs nothing until a key is replaced | not created yet |

The script can't vouch for itself, so to be sure of what you run, verify a download by hand (here 0.1.0):

```sh
V=0.1.0; B=https://github.com/MoonWX/isshoni/releases/download/v$V
curl -fsSLO $B/install.sh -O $B/checksums.txt -O $B/checksums.txt.sig
curl -fsSLo allowed_signers https://raw.githubusercontent.com/MoonWX/isshoni/v$V/deploy/keys/allowed_signers
ssh-keygen -Y verify -f allowed_signers -I isshoni-release -n isshoni-checksums -s checksums.txt.sig < checksums.txt
sha256sum --ignore-missing -c checksums.txt
```

`ssh-keygen` prints the fingerprint of the key that made the signature: it must be one of the fingerprints above.
Then `sudo sh install.sh` installs exactly that version. Each release also has a Sigstore bundle
(`checksums.txt.sigstore.json`, for `cosign verify-blob`) and GitHub build provenance (`gh attestation verify`), and
the image `ghcr.io/moonwx/isshoni` is signed with cosign.

## Privacy

The server is trusted: media is encrypted in transit, but the server and its operator could see the streams (there is
no end-to-end encryption yet). There is no recording and no telemetry. The server contacts others only for STUN (to
learn its public IP address), Let's Encrypt, push notifications and a daily GitHub release check that can be turned off.

## Documentation and contributing

- [CONTRIBUTING.md](CONTRIBUTING.md): build and run isshoni from source, the tasks, and the rules for changes.
- [docs/PLAN.md](docs/PLAN.md): the project plan, with the product decisions and the roadmap.
- [docs/m1/README.md](docs/m1/README.md): the M1 design (the server and the web client) and its implementation plan.
- The project site, from v0.1.0: the install guides, a guide for friends, troubleshooting and the privacy page.

## License

isshoni is licensed under the [Apache License 2.0](LICENSE) (see also [NOTICE](NOTICE)), with no GPL dependencies or
copied GPL code. Every release lists the licenses of what it bundles in `THIRD_PARTY_NOTICES`.
