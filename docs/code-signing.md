# Code signing policy

This page says what the isshoni project signs, how, and who may approve a signature.

## What is signed, and how {#what}

| What | How |
|---|---|
| Server releases (from M1): `checksums.txt`, which lists the SHA-256 of every archive, package, install script and compose file | `ssh-keygen -Y sign` with the release key, which lives in a protected GitHub environment; the owner approves every release before the signing job can use it ([release keys](/security#keys)) |
| The Docker image `ghcr.io/moonwx/isshoni` | Sigstore keyless signing (cosign) by the release workflow's GitHub identity |
| Windows app and installer (from M2) | **0.x: unsigned** ([what Windows shows](/download#windows-smartscreen)); every file is in the release's signed `checksums.txt` ([verify it](/download#windows-verify)). After approval: Free code signing provided by SignPath.io, certificate by SignPath Foundation — signed: `isshoni.exe`, `isshoni-media.dll`, `uninstall.exe`, `isshoni-desktop_<version>_windows_amd64_setup.exe`; not signed: the FFmpeg DLLs (third-party, unmodified) |
| The Windows app's update info (from M2): `isshoni-desktop-update.json`, which names the installer's URL, size and SHA-256 | `ssh-keygen -Y sign` with the update key, which lives in the same protected environment and is used only after the owner approves the release; the app carries the public update keys and installs nothing whose signature doesn't verify ([update keys](/security#keys)) |
| macOS app (from M3) | A stable self-signed code-signing identity; its certificate's fingerprint will be published here |

## Roles {#roles}

- Committers and reviewers: [@MoonWX](https://github.com/MoonWX)
- Approvers of every signing request: [@MoonWX](https://github.com/MoonWX)

## Build rules {#rules}

- Only artifacts that GitHub Actions builds from a tagged commit of [this repository](https://github.com/MoonWX/isshoni)
  are signed. Local builds never are.
- The owner's accounts use multi-factor authentication.
- Third-party binaries are shipped unmodified and are **not** signed by this project: the FFmpeg libraries of the
  Windows app, `avcodec-<N>.dll` and `avutil-<N>.dll` ([their source](/download#ffmpeg-source)).
- Every signed Windows file carries the product name and version in its version information.

## Privacy {#privacy}

isshoni does not send any information to other networked systems unless the user or the person running the server
asks for it. The exceptions are exactly those listed on the [privacy page](/privacy#outbound):

- STUN requests to Cloudflare and Google, to learn the server's public IP address (`network.stun_servers = []` turns
  them off);
- Let's Encrypt, to get and renew the certificate, and one request to check the clock (off with `tls.mode` `manual`
  or `off`);
- one daily request to the GitHub API for the latest release (`updates.release_check = false` turns it off);
- notifications, encrypted, through the browsers' push services when users turn them on (`push.enabled = false` turns
  them off).

The Windows app's exceptions are listed on the [privacy page](/privacy#desktop):

- the servers the user links it to;
- one check a day for updates at GitHub Releases (on by default; turned off at install, in the app's settings or by a
  machine policy);
- one HTTPS download of Cisco's OpenH264 2.7.0 from `https://ciscobinary.openh264.org/openh264-2.7.0-win64.dll.bz2`,
  checked against its pinned size and SHA-256, only when the user turns OpenH264 on;
- Microsoft's WebView2 bootstrapper, run by the installer only when the WebView2 Runtime is missing.

## Contact {#contact}

To report a signed binary that misbehaves, or a suspected misuse of a signature, use
[private vulnerability reporting](https://github.com/MoonWX/isshoni/security/advisories/new) ([Security](/security)).
