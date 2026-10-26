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

## Linked apps {#linked-apps}

The [desktop apps](/download) never keep your password or a browser cookie. You link an app to your server once, and
the server gives that app a sign-in of its own. This is that sign-in's threat model: what it is, where it is kept,
what someone who copies it can do, and how it ends.

### Linking {#linked-apps-linking}

- An app is approved only in a browser that is signed in to the server, on the server's own page (`/link`). A linked
  app can never approve another one.
- That page shows the code to compare with the one in the app, which app and version asks, the computer's name, and
  when and from which IP address the request came. When the request came from another network than the browser, or
  the server can't tell, the page warns and asks you to tick "I started this myself". A code works for 10 minutes.
  **Never approve a code that someone sent you**: whoever started it would get an app signed in as you.
- Instead of a browser, the app can sign in with your username and password. That form has exactly the login page's
  limits, so it is no faster way to guess a password, and the password is sent once and never stored.
- Every new app shows on your account's Devices page, and on the admins' dashboard as a security event.

### The sign-in and where it is kept {#linked-apps-tokens}

- The app holds a **refresh token**, which it trades for a new one every time it uses it, and **access tokens** that
  work for 15 minutes. The server keeps only keyed hashes of them (HMAC-SHA-256), so a copy of its database or of a
  backup holds no usable token.
- An app stays linked while it uses its refresh token at least once in 90 days; it does so when it starts and at
  least once a day while it runs. An app left unused for 90 days is removed.
- On Windows the refresh token is kept in **Windows Credential Manager**, one entry per server, stored for this PC
  only, so it doesn't follow a roaming profile to other PCs. Access tokens stay in memory, and the app's window only
  ever gets an access token, never the refresh token.
- **Any program that runs as the same Windows user can read that entry**, just as it can read your browser's
  cookies: Windows protects it from the PC's other users (DPAPI, tied to your Windows sign-in), not from your own
  programs. Malware running as you can take the app's sign-in, as it can take your browser's.

### What a copied sign-in can do {#linked-apps-scope}

- It acts as you on that server, as the app does: it can watch, share, change your settings, sign your browsers out,
  and create or revoke invites if you're allowed to.
- It can't change your password, delete your account, approve another app, subscribe to notifications, or use any
  admin page or admin API: those need a browser session. That holds for an admin's PC too, so a stolen desktop
  sign-in never becomes control over the server's users, settings or audit log. (An admin's app can create and revoke
  invites, as any member allowed to invite can.)
- The server accepts requests from the app's window (`http://wails.localhost`) only with an access token, never with
  cookies. Every app built with the same framework has that origin, so the origin proves nothing; the token does.

### How a copy is caught {#linked-apps-reuse}

- Each refresh token works once. When the app and a copy both use the same one, the server sees a used token come
  back: it removes that app at once, so the copy stops working too, records a security event and alerts the admins
  who turned on admin alerts. The app says that someone else used its sign-in and asks to be linked again. The app's
  own retries of a refresh whose answer got lost don't count as a second use.
- A copy is caught once both have used it: at the latest when the app next refreshes. Until then a copy that was used
  first keeps working, so remove an app you don't trust right away (below).

### Ending a sign-in {#linked-apps-revoke}

- Your account's Devices page, in the browser, lists every linked app with its name, version, and when and from which
  IP address it was last seen. Remove signs that app out at once and closes its connections; "Sign out" in the app
  does the same for that app.
- Changing your password or "Log out everywhere" removes all your apps, and so do an admin's "Create reset link",
  "Sign out everywhere" and "Disable".
- An account has at most 20 linked apps; linking a 21st removes the one seen least recently.
- `isshoni admin rotate-secrets` signs out every app, as it does every browser.
