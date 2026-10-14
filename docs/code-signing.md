# Code signing policy

This page says what the isshoni project signs, how, and who may approve a signature.

## What is signed, and how {#what}

| What | How |
|---|---|
| Server releases (from M1): `checksums.txt`, which lists the SHA-256 of every archive, package, install script and compose file | `ssh-keygen -Y sign` with the release key, which lives in a protected GitHub environment; the owner approves every release before the signing job can use it ([release keys](/security#keys)) |
| The Docker image `ghcr.io/moonwx/isshoni` | Sigstore keyless signing (cosign) by the release workflow's GitHub identity |
| Windows binaries and installer (from M2) | Free code signing provided by SignPath.io, certificate by SignPath Foundation |
| macOS app (from M3) | A stable self-signed code-signing identity; its certificate's fingerprint will be published here |

## Roles {#roles}

- Committers and reviewers: [@MoonWX](https://github.com/MoonWX)
- Approvers of every signing request: [@MoonWX](https://github.com/MoonWX)

## Build rules {#rules}

- Only artifacts that GitHub Actions builds from a tagged commit of [this repository](https://github.com/MoonWX/isshoni)
  are signed. Local builds never are.
- The owner's accounts use multi-factor authentication.
- Third-party binaries (the FFmpeg libraries of the Windows app, from M2) are shipped unmodified and are **not**
  signed by this project.

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

## Contact {#contact}

To report a signed binary that misbehaves, or a suspected misuse of a signature, use
[private vulnerability reporting](https://github.com/MoonWX/isshoni/security/advisories/new) ([Security](/security)).
