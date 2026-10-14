# Security

## Reporting a vulnerability {#report}

Please report security problems privately, through GitHub's private vulnerability reporting:
[Report a vulnerability](https://github.com/MoonWX/isshoni/security/advisories/new) (the repository's Security tab).
Don't open a public issue for them: the report and its answers stay between you and the maintainer until a fix is
out.

## Supported versions {#versions}

isshoni is at 0.x. Security fixes go into the latest minor release (for example 0.1.x while 0.1 is the latest), and
the server's admin page and `isshoni doctor` say when a security release is out. Upgrade with the install script
([Upgrade](/install/#upgrade)) or `docker compose pull && docker compose up -d`.

## Release signing keys {#keys}

Every release's `checksums.txt` lists the SHA-256 of every file of the release, and is signed with `ssh-keygen -Y
sign` by a key that only the release workflow can use, after the owner approves each release. The install script
carries the public keys and checks that signature before it installs anything.

| Key | Use |
|---|---|
| `isshoni-release-1` | signs every release |
| `isshoni-release-backup-1` | kept offline; signs nothing until a key is replaced |

Their fingerprints, which the "Release signing keys" section of the repository's
[README.md](https://github.com/MoonWX/isshoni#release-signing-keys) lists too:

<KeyFingerprints />

The public keys are in `deploy/keys/allowed_signers` in the repository
([deploy/keys](https://github.com/MoonWX/isshoni/tree/main/deploy/keys)), and this site serves the same file as
`https://moonwx.github.io/isshoni/keys/allowed_signers` once there is a release. If a key is ever replaced, the new
one is added to that file, the old one is marked as valid only before a date (or removed, if it leaked), and a new
release follows at once; a leak also gets a security advisory.

## How to verify a download {#verify}

### The install script and the archives {#verify-checksums}

The install script does this by itself; by hand it is (here for 0.1.0):

```sh
V=0.1.0; B=https://github.com/MoonWX/isshoni/releases/download/v$V
curl -fsSLO $B/install.sh -O $B/checksums.txt -O $B/checksums.txt.sig
curl -fsSL https://moonwx.github.io/isshoni/keys/allowed_signers -o allowed_signers   # compare fingerprints with README
ssh-keygen -Y verify -f allowed_signers -I isshoni-release -n isshoni-checksums -s checksums.txt.sig < checksums.txt
sha256sum --ignore-missing -c checksums.txt
sudo sh install.sh
```

1. Download the script (or an archive or package: add it to the `curl` line), `checksums.txt` and its signature,
   from the release on GitHub.
2. Get the public keys: from this site as above, or from the repository at that release's tag,
   `https://raw.githubusercontent.com/MoonWX/isshoni/v$V/deploy/keys/allowed_signers`. Two sources that agree are
   better than one.
3. `ssh-keygen -Y verify` checks the signature and prints the fingerprint of the key that made it. It must be one of
   the [fingerprints above](#keys), which the README lists too.
4. `sha256sum` checks the files you downloaded against the signed list: each must say `OK`.
5. Only then run the script. It installs exactly the version it came with.

The signature protects against changed release files, mirrors and broken downloads. A changed `install.sh` on this
site is what the manual check is for: the site shows the SHA-256 of the script it serves on the
[install page](/install/), and it must be the one in the release's `checksums.txt`.

### Sigstore {#verify-sigstore}

`checksums.txt` is also signed with Sigstore (keyless, by the release workflow's identity on GitHub):

```sh
curl -fsSLO $B/checksums.txt.sigstore.json
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/MoonWX/isshoni/.github/workflows/release.yml@refs/tags/v$V" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
```

### The Docker image {#verify-image}

The image `ghcr.io/moonwx/isshoni` is signed with cosign by the same identity, and has a build provenance attestation
and an SBOM:

```sh
cosign verify ghcr.io/moonwx/isshoni:$V \
  --certificate-identity "https://github.com/MoonWX/isshoni/.github/workflows/release.yml@refs/tags/v$V" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://ghcr.io/moonwx/isshoni:$V --repo MoonWX/isshoni
```

### Build provenance {#verify-provenance}

Every archive, package, `checksums.txt` and `install.sh` has a GitHub build provenance attestation: proof that the
release workflow of this repository built it from the tagged commit.

```sh
gh attestation verify isshoni_${V}_linux_amd64.tar.gz --repo MoonWX/isshoni
```

Each archive also comes with an SPDX SBOM (`*.sbom.json`) that lists what it contains, and each archive, package and
image carries `THIRD_PARTY_NOTICES` with the licenses of everything isshoni bundles.

See also the [code signing policy](/code-signing).
