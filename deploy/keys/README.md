# deploy/keys

The public halves of the release signing keys (`docs/m1/06-deploy-and-ci.md` §9.4, "06" below). Every release's
`checksums.txt` is signed with `ssh-keygen -Y sign`, and `install.sh` checks that signature against these keys before
it installs anything (06 §4.3).

| File | What it is |
|---|---|
| `allowed_signers` | one line per key, in `ssh-keygen`'s allowed-signers format (below). It does not exist until the owner adds the keys: until then `task lint:keys` skips, and release.yml refuses to build a release |
| `update_signers` | the desktop app's update keys (`docs/m2/06-windows-release.md` §8.3), in the same format with the principal and namespace `isshoni-update`: one `isshoni-update-<n>` line or more and exactly one `isshoni-update-backup-<n>`. `task release:update-keys` writes it into `internal/update/signers.go`, and `task lint:keys` fails when the two differ. It does not exist until the owner creates the update keys ([below](#the-desktop-apps-update-keys-owner-before-the-release-dry-run-with-the-app)). The project site serves it as `/keys/update_signers` |
| `update_revoked` | update key fingerprints that the next desktop release revokes, empty normally (`docs/m2/06-windows-release.md` §8.1, §8.12) |
| `README.md` | this page |

Each line of `allowed_signers` names the principal and the namespace that `install.sh` and release.yml's `sign`
job ask for, then the public key:

```
isshoni-release namespaces="isshoni-checksums" ssh-ed25519 AAAA… isshoni-release-1
isshoni-release namespaces="isshoni-checksums" ssh-ed25519 AAAA… isshoni-release-backup-1
```

`deploy/install.sh` carries a copy of the file between its `# BEGIN ALLOWED SIGNERS` and `# END ALLOWED SIGNERS`
lines; the script has no other way to take a key. The desktop app's `deploy/install-desktop.ps1` carries the same
lines between the same markers, inside its `$AllowedSigners` here-string (`docs/m2/06-windows-release.md` §7.1).
`task release:keys` writes both copies, and `task lint:keys` (CI's `lint-deploy` job) and the install.sh unit tests
fail when a copy differs from the file. Never edit the blocks by hand.

Only public keys belong here. A private key is never committed, never pasted into an issue or a pull request, and
lives in exactly two places: the `release` environment's secret, and the owner's offline backup.

## Before the first release candidate (owner)

Done once, in this order, before `v0.1.0-rc.1`. Nobody but the owner does these steps, and no agent creates keys or
changes GitHub settings.

The root `README.md` has a "Release signing keys" section, whose table says "not created yet" for both keys until
step 2 puts their fingerprints there. Every release archive carries `README.md` (06 §3), and goreleaser and
release.yml (its dry run too) refuse to build without it.

1. **Generate the two keys** on the Mac, outside the repository (06 §9.4):
   ```sh
   ssh-keygen -t ed25519 -N '' -C isshoni-release-1 -f isshoni-release-1
   ssh-keygen -t ed25519 -C isshoni-release-backup-1 -f isshoni-release-backup-1   # asks for a passphrase
   ```
   `isshoni-release-1` signs every release and has no passphrase (a CI job uses it). `isshoni-release-backup-1`
   signs nothing until a rotation; it has a passphrase and lives offline only, in the password manager plus a
   printed copy.
2. **Add the public halves by pull request**, in a branch of the repository:
   ```sh
   for k in isshoni-release-1 isshoni-release-backup-1; do
     printf 'isshoni-release namespaces="isshoni-checksums" %s\n' "$(cat "/path/to/$k.pub")"
   done > deploy/keys/allowed_signers
   task release:keys        # copies the file into deploy/install.sh and deploy/install-desktop.ps1, prints the
                            # fingerprints, runs lint:keys
   # lint:keys fails until README.md lists them: replace the two "not created yet" cells of README.md's
   # "Release signing keys" table with the printed fingerprints, then check again
   task lint:keys
   git add deploy/keys/allowed_signers deploy/install.sh deploy/install-desktop.ps1 README.md
   git commit -m "Release signing keys"
   ```
   The change is data only: the key file, the blocks in `install.sh` and `install-desktop.ps1` and the two
   fingerprints that `task release:keys` prints (`ssh-keygen -lf <key>.pub` prints the same) in the "Release signing
   keys" section of the root `README.md` (06 §9.4). The first `task release:keys` ends with lint:keys' error that
   README.md does not list them yet; that is expected, and both scripts are already written. `task lint:keys` (CI's
   too) passes once the section lists each key's fingerprint, no other one, and no row still says a key is not
   created. Keep the fingerprints: they also go onto the site's `/security` page (06 §10.3), and you compare them in
   step 5. Merge once `ci-ok` is green.
3. **Create the environment `release`** (Settings → Environments → New environment; 06 §8.6 step 4):
   - required reviewers: you, with "Prevent self-review" **off** (you approve your own releases);
   - deployment branches and tags: "Selected branches and tags", with one rule for **tags** `v*` (no branches);
   - environment secret `RELEASE_SSH_SIGNING_KEY`: the whole content of the **private** file `isshoni-release-1`,
     from `-----BEGIN OPENSSH PRIVATE KEY-----` to `-----END OPENSSH PRIVATE KEY-----`.

   Then delete the local `isshoni-release-1` (the private file; keep the `.pub` if you like). Put
   `isshoni-release-backup-1` into the password manager, print it, and delete the local copy too. If the ruleset
   "release tags" of 06 §8.6 step 3 does not exist yet, create it now as well.
4. **Optional, recommended: run the release dry run** (Actions → Release → Run workflow, `dry_run` on). It builds
   everything as `0.0.0-dryrun.<run>`, signs with a key made in the job and installs the result in two systemd
   containers; it needs no approval and publishes nothing.
5. **Tag the release candidate** from the commit that should become `v0.1.0-rc.1` (on `main`):
   ```sh
   git tag -a v0.1.0-rc.1 -m v0.1.0-rc.1 && git push origin v0.1.0-rc.1
   ```
   release.yml then builds a **draft** release and pushes only `ghcr.io/moonwx/isshoni:0.1.0-rc.1`. Edit the draft's
   notes (each `TODO`; the `publish` job refuses a body that still has one), then approve the `release`
   environment (the GitHub mobile app works). The `sign` job signs `checksums.txt` with the secret and checks the
   signature against `deploy/keys/allowed_signers`, `verify` installs the release in Debian 12 and Ubuntu 24.04
   containers with the released `install.sh`, and `publish` publishes the draft as a prerelease: GitHub's
   "latest" release and `:latest` stay where they are. Compare the fingerprint in the `sign` job's log with the
   one from step 2.
6. **After the first release**, make the package `ghcr.io/moonwx/isshoni` public and check that it is linked to the
   repository (06 §8.6 step 8). Then try `install.sh` from that release on a VPS:
   `curl -fsSL https://github.com/MoonWX/isshoni/releases/download/v0.1.0-rc.1/install.sh | sh`.

## The desktop app's update keys (owner, before the release dry run with the app)

The Windows app installs an update only when the release's `isshoni-desktop-update.json` carries a valid signature by
one of the keys it embeds (`docs/m2/06-windows-release.md` §8.1, §8.3, §11.5, "M2 06" below). They are keys of their
own: never one of `allowed_signers`' keys (`task lint:keys` refuses that). Done once, like the release keys:

1. **Generate the two keys** on the Mac, outside the repository:
   ```sh
   ssh-keygen -t ed25519 -N '' -C isshoni-update-1 -f isshoni-update-1
   ssh-keygen -t ed25519 -C isshoni-update-backup-1 -f isshoni-update-backup-1   # asks for a passphrase
   ```
   `isshoni-update-1` signs every release's manifest and has no passphrase (the `sign` job uses it).
   `isshoni-update-backup-1` signs only the release that replaces a leaked update key (below); it has a passphrase
   and lives offline only, in the password manager plus a printed copy.
2. **Add the public halves by pull request**:
   ```sh
   for k in isshoni-update-1 isshoni-update-backup-1; do
     printf 'isshoni-update namespaces="isshoni-update" %s\n' "$(cat "/path/to/$k.pub")"
   done > deploy/keys/update_signers
   task release:update-keys   # writes internal/update/signers.go, prints the fingerprints, runs lint:keys
   # replace the two update keys' "not created yet" cells of README.md's "Release signing keys" table with the
   # printed fingerprints, then check again
   task lint:keys
   git add deploy/keys/update_signers internal/update/signers.go README.md
   git commit -m "Update signing keys"
   ```
   As with the release keys, the first `task release:update-keys` ends with lint:keys' error that README.md does not
   list them yet. Once merged and released, the project site serves the same file as `/keys/update_signers` and shows
   the fingerprints on `/security#keys` (site.yml writes them into `release.json`, and checks every release with the
   app against this file: its manifest's signature must verify with it).
3. **Put the private key into the environment `release`**: environment secret `RELEASE_UPDATE_SIGNING_KEY`, the whole
   content of the **private** file `isshoni-update-1`. Then delete the local private files, after putting
   `isshoni-update-backup-1` into the password manager and printing it.

`update_revoked` stays empty except for the release after a leak of the active update key (below).

**Rotation** (M2 06 §8.12): generate `isshoni-update-2`, add its line to `update_signers` (keep `-1`), run
`task release:update-keys`, add its fingerprint to README.md and release. After one release cycle, replace
`RELEASE_UPDATE_SIGNING_KEY` with `-2` and drop `-1`'s line (and its README row) in the next release. A rotation never
uses `update_revoked`.

**A leaked active update key** (M2 06 §8.12):
1. For one release, sign with the backup key: make a passphrase-free copy (`ssh-keygen -p` on a temporary copy of
   `isshoni-update-backup-1`), put it into `RELEASE_UPDATE_SIGNING_KEY` in place of the leaked key, and delete the
   copy.
2. In the same release, list the leaked key's fingerprint in `update_revoked`, remove its line from `update_signers`
   and add a new active key's (`task release:update-keys`, README.md's rows). The `sign` job refuses a non-empty
   `update_revoked` with any key but the backup, and `check-draft.sh` checks the same before `publish`.
3. Publish the release with a security advisory, then put the new active key into `RELEASE_UPDATE_SIGNING_KEY`, empty
   `update_revoked` again, and in the following release replace the backup (`isshoni-update-backup-2`), since the old
   one has been in CI.

Apps that check for updates stop trusting the leaked key for good; an app that never checks again can't be told
(`/security#verify-desktop` says so). **A leaked or lost backup key** can't be revoked by a manifest: a normal release
embeds a new backup line instead of the old one, and apps that update stop trusting the old backup.

## Rotation of the release keys, or a leaked one (06 §9.4)

1. Add the new key's line to `allowed_signers`, and delete the old key's line: at once after a leak, otherwise in a
   later change. Do not mark it `valid-before=` instead: Windows 10's OpenSSH 8.1 can't parse that option, so
   `task lint:keys` refuses it (`docs/m2/06-windows-release.md` §7.2).
   Run `task release:keys`, put the new key's fingerprint into README.md's "Release signing keys" section (and
   remove a deleted key's), run `task lint:keys`, and merge the change.
2. Replace the secret `RELEASE_SSH_SIGNING_KEY`.
3. Cut a patch release: the site's `install.sh` then carries the new block.

After a leak, the backup key signs until a new key exists, and a security advisory lists the affected versions.
