# deploy/keys

The public halves of the release signing keys (`docs/m1/06-deploy-and-ci.md` §9.4, "06" below). Every release's
`checksums.txt` is signed with `ssh-keygen -Y sign`, and `install.sh` checks that signature against these keys before
it installs anything (06 §4.3).

| File | What it is |
|---|---|
| `allowed_signers` | one line per key, in `ssh-keygen`'s allowed-signers format (below). It does not exist until the owner adds the keys: until then `task lint:keys` skips, and release.yml refuses to build a release |
| `update_signers` | the desktop app's update keys (`docs/m2/06-windows-release.md` §8.3), in the same format with the principal and namespace `isshoni-update`: one `isshoni-update-<n>` line or more and exactly one `isshoni-update-backup-<n>`. `task release:update-keys` writes it into `internal/update/signers.go`, and `task lint:keys` fails when the two differ. It does not exist until the owner creates the update keys (`docs/m2/README.md` §8) |
| `update_revoked` | update key fingerprints that the next desktop release revokes, empty normally (`docs/m2/06-windows-release.md` §8.1, §8.12) |
| `README.md` | this page |

Each line of `allowed_signers` names the principal and the namespace that `install.sh` and release.yml's `sign`
job ask for, then the public key:

```
isshoni-release namespaces="isshoni-checksums" ssh-ed25519 AAAA… isshoni-release-1
isshoni-release namespaces="isshoni-checksums" ssh-ed25519 AAAA… isshoni-release-backup-1
```

`deploy/install.sh` carries a copy of the file between its `# BEGIN ALLOWED SIGNERS` and `# END ALLOWED SIGNERS`
lines; the script has no other way to take a key. `task release:keys` writes that copy, and `task lint:keys` (CI's
`lint-deploy` job) and the install.sh unit tests fail when the two differ. Never edit the block by hand.

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
   task release:keys        # copies the file into deploy/install.sh, prints the fingerprints, runs lint:keys
   # lint:keys fails until README.md lists them: replace the two "not created yet" cells of README.md's
   # "Release signing keys" table with the printed fingerprints, then check again
   task lint:keys
   git add deploy/keys/allowed_signers deploy/install.sh README.md
   git commit -m "Release signing keys"
   ```
   The change is data only: the key file, the block in `install.sh` and the two fingerprints that
   `task release:keys` prints (`ssh-keygen -lf <key>.pub` prints the same) in the "Release signing keys" section of
   the root `README.md` (06 §9.4). The first `task release:keys` ends with lint:keys' error that README.md does not
   list them yet; that is expected, and install.sh is already written. `task lint:keys` (CI's too) passes once the
   section lists each key's fingerprint, no other one, and no row still says a key is not created. Keep the
   fingerprints: they also go onto the site's `/security` page (06 §10.3), and you compare them in step 5. Merge
   once `ci-ok` is green.
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

## Rotation, or a leaked key (06 §9.4)

1. Add the new key's line to `allowed_signers`, and delete the old key's line: at once after a leak, otherwise in a
   later change. Do not mark it `valid-before=` instead: Windows 10's OpenSSH 8.1 can't parse that option, so
   `task lint:keys` refuses it (`docs/m2/06-windows-release.md` §7.2).
   Run `task release:keys`, put the new key's fingerprint into README.md's "Release signing keys" section (and
   remove a deleted key's), run `task lint:keys`, and merge the change.
2. Replace the secret `RELEASE_SSH_SIGNING_KEY`.
3. Cut a patch release: the site's `install.sh` then carries the new block.

After a leak, the backup key signs until a new key exists, and a security advisory lists the affected versions.
