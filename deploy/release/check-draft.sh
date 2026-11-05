#!/bin/sh
# Is a downloaded release draft exactly what the release keys signed? (docs/m1/06-deploy-and-ci.md §9.3, "06" below;
# docs/m2/06-windows-release.md §11.3, "M2 06" below)
#
#   sh deploy/release/check-draft.sh DIR ALLOWED_SIGNERS [UPDATE_SIGNERS]
#
# release.yml's publish job runs it right before the draft goes public, on the assets that `gh release download` put
# into DIR, with the tag's deploy/keys/allowed_signers and deploy/keys/update_signers: anything with a write token
# could have changed the draft after the sign job signed it (README F09, review R4). It passes (exit 0, a line that
# says ok) only when every check passes, in this order:
#   1. DIR/checksums.txt.sig is a signature of DIR/checksums.txt by a key of ALLOWED_SIGNERS, for the principal
#      isshoni-release and the namespace isshoni-checksums, as install.sh checks it (06 §4.3);
#   2. every asset that checksums.txt lists is in DIR with that SHA-256 (`sha256sum --strict -c`, so a malformed
#      line fails too);
#   3. DIR holds no file that checksums.txt does not list, hidden ones included, except checksums.txt itself, the two
#      signatures checksums.txt.sig and isshoni-desktop-update.json.sig, and the cosign bundle
#      checksums.txt.sigstore.json: no signature would cover such an asset. Each one is named, not only the first.
# And when DIR holds the desktop app's update manifest isshoni-desktop-update.json (M2 06 §8.2), for which
# UPDATE_SIGNERS is then required (exit 2 without it):
#   4. DIR/isshoni-desktop-update.json.sig is a signature of the manifest by a key of UPDATE_SIGNERS, for the
#      principal and the namespace isshoni-update, as the app checks it (M2 06 §8.3);
#   5. unless the manifest holds the exact line `  "revokedKeys": [],` or `  "revokedKeys": []` (what
#      `tools/desktoppkg manifest` writes for an empty list; any other spelling counts as a revocation, so the check
#      fails closed), the same signature also verifies with UPDATE_SIGNERS' backup line alone (the key whose comment
#      is isshoni-update-backup-<n>): apps honour a revocation only from the backup key (M2 06 §8.1, §8.12), and a
#      release must not publish one that every app would ignore.
# The signature of checksums.txt comes first, so sha256sum reads only the names of a checksums.txt that the key
# signed; the manifest's signature comes after the checksums, which already cover the manifest's bytes. A manifest
# signature without a manifest fails too.
#
# It reads nothing but DIR, ALLOWED_SIGNERS and UPDATE_SIGNERS, and changes nothing but a temporary folder of its own.
# It needs sha256sum and ssh-keygen (OpenSSH 8.1 or newer), and no JSON parser. In GitHub Actions
# (GITHUB_ACTIONS=true) its errors are error annotations.
#
# Exit codes: 0 the draft is what the keys signed; 1 it is not, or a check could not run; 2 usage.
#
# deploy/test/check-draft-test.sh tests it on fixture drafts with keys made for the run; `task test:sh` runs that
# test under dash and bash --posix (README F15).
set -eu

SIGN_IDENTITY=isshoni-release
SIGN_NAMESPACE=isshoni-checksums
UPDATE_IDENTITY=isshoni-update
UPDATE_NAMESPACE=isshoni-update
MANIFEST=isshoni-desktop-update.json

usage() {
  printf 'Usage: sh %s DIR ALLOWED_SIGNERS [UPDATE_SIGNERS]\n' "$0"
  printf 'Checks that the release draft downloaded into DIR is what a key of ALLOWED_SIGNERS signed: the\n'
  printf 'signature of checksums.txt, every checksum, and no asset that checksums.txt does not list. When DIR\n'
  printf 'holds %s, UPDATE_SIGNERS is required: the manifest must be signed by one of its keys, and\n' "$MANIFEST"
  printf 'by its backup key when the manifest revokes keys.\n'
}

# err MESSAGE prints MESSAGE on stderr, as an error annotation in GitHub Actions. Control characters become "?", so
# an asset whose name has a line break in it can't start a workflow command of its own. In an annotation "%" becomes
# "%25", as @actions/core's escapeData writes it: the runner decodes %25, %0D and %0A in the message, so a name with
# "%0A" in it would otherwise be shown as two lines, not as itself.
err() {
  err_msg=$(printf '%s' "$1" | LC_ALL=C tr -c '[:print:]' '?')
  if [ "${GITHUB_ACTIONS:-}" = true ]; then
    err_msg=$(printf '%s' "$err_msg" | sed 's/%/%25/g')
    printf '::error::check-draft: %s\n' "$err_msg" >&2
  else
    printf 'check-draft: %s\n' "$err_msg" >&2
  fi
}

# listed NAME reports whether checksums.txt has a line for the file NAME: "<hex>  NAME", or "<hex> *NAME" as
# sha256sum also reads it. The name is compared whole, and taken from the environment: awk -v would turn a
# backslash in it into an escape.
listed() {
  CHECK_DRAFT_NAME=$1 awk '
    { line = $0 }
    sub(/^[0-9A-Fa-f]+ [ *]/, "", line) && line == ENVIRON["CHECK_DRAFT_NAME"] { found = 1 }
    END { exit !found }' "$dir/checksums.txt"
}

case ${1:-} in
-h | --help)
  usage
  exit 0
  ;;
esac
if [ $# -ne 2 ] && [ $# -ne 3 ]; then
  usage >&2
  exit 2
fi
dir=$1
signers=$2
update_signers=${3:-}

if [ ! -d "$dir" ]; then
  err "$dir is not a directory"
  exit 1
fi
# A draft with the update manifest is checked with the update keys too, or not at all.
if [ -f "$dir/$MANIFEST" ] && [ $# -ne 3 ]; then
  err "the draft has $MANIFEST, so UPDATE_SIGNERS is needed to check its signature"
  usage >&2
  exit 2
fi
if [ ! -s "$signers" ]; then
  err "$signers is missing or empty, so no key can check the signature"
  exit 1
fi
for f in checksums.txt checksums.txt.sig; do
  if [ ! -f "$dir/$f" ]; then
    err "the draft has no $f"
    exit 1
  fi
done

# 1. checksums.txt is signed by a key of ALLOWED_SIGNERS.
if ! ssh-keygen -Y verify -f "$signers" -I "$SIGN_IDENTITY" -n "$SIGN_NAMESPACE" \
  -s "$dir/checksums.txt.sig" <"$dir/checksums.txt"; then
  err "checksums.txt has no valid signature by a key of $signers (principal $SIGN_IDENTITY, namespace $SIGN_NAMESPACE)"
  exit 1
fi

# 2. Every asset it lists is there, with that SHA-256.
if ! (CDPATH='' cd -- "$dir" && sha256sum --strict -c checksums.txt); then
  err 'an asset that checksums.txt lists is missing or does not match it, or checksums.txt has a malformed line'
  exit 1
fi

# 3. And the draft has no asset that it does not list. Both patterns, so hidden files count too; a pattern that
# matches nothing stays as it is written and names no file, and . and .. are no assets.
unlisted=0
assets=0
for f in "$dir"/* "$dir"/.*; do
  name=${f##*/}
  case $name in
  . | .. | checksums.txt | checksums.txt.sig | checksums.txt.sigstore.json | "$MANIFEST.sig") continue ;;
  esac
  [ -e "$f" ] || [ -h "$f" ] || continue
  if listed "$name"; then
    assets=$((assets + 1))
  else
    err "the draft has $name, which checksums.txt does not list"
    unlisted=1
  fi
done
if [ "$unlisted" != 0 ]; then
  exit 1
fi

ok="checksums.txt is signed, and it lists each of the $assets assets with its checksum"
if [ ! -f "$dir/$MANIFEST" ]; then
  # The exception of check 3 is for the manifest's signature only: without a manifest it signs nothing.
  if [ -e "$dir/$MANIFEST.sig" ] || [ -h "$dir/$MANIFEST.sig" ]; then
    err "the draft has $MANIFEST.sig but no $MANIFEST"
    exit 1
  fi
  printf 'check-draft: ok: %s\n' "$ok"
  exit 0
fi

# 4. The update manifest is signed by a key of UPDATE_SIGNERS.
if [ ! -s "$update_signers" ]; then
  err "$update_signers is missing or empty, so no key can check the signature of $MANIFEST"
  exit 1
fi
if [ ! -f "$dir/$MANIFEST.sig" ]; then
  err "the draft has no $MANIFEST.sig"
  exit 1
fi
# verify_manifest SIGNERS checks the manifest's signature with the allowed signers file SIGNERS.
verify_manifest() {
  ssh-keygen -Y verify -f "$1" -I "$UPDATE_IDENTITY" -n "$UPDATE_NAMESPACE" \
    -s "$dir/$MANIFEST.sig" <"$dir/$MANIFEST"
}
if ! verify_manifest "$update_signers"; then
  err "$MANIFEST has no valid signature by a key of $update_signers (principal $UPDATE_IDENTITY, namespace $UPDATE_NAMESPACE)"
  exit 1
fi

# 5. A revocation is signed by the backup key. The two lines that `desktoppkg manifest` writes for an empty list are
# the only ones that mean "none": a CR, another indentation or a list over several lines counts as a revocation.
if grep -Fqx -e '  "revokedKeys": [],' -e '  "revokedKeys": []' "$dir/$MANIFEST"; then
  printf 'check-draft: ok: %s; %s is signed by an update key and revokes no key\n' "$ok" "$MANIFEST"
  exit 0
fi
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' INT TERM
# The backup line is the one whose key comment, its last field, is isshoni-update-backup-<n>; there must be exactly
# one, as `task lint:keys` requires of deploy/keys/update_signers.
awk '
  { sub(/\r$/, "") }
  /^[ \t]*(#|$)/ { next }
  $NF ~ /^isshoni-update-backup-[0-9]+$/ { print }' "$update_signers" >"$tmp/backup_signers"
backups=$(awk 'END { print NR }' "$tmp/backup_signers")
if [ "$backups" != 1 ]; then
  err "$update_signers has $backups backup lines (key comment isshoni-update-backup-<n>), not one, so nothing can check the revocation in $MANIFEST"
  exit 1
fi
if ! verify_manifest "$tmp/backup_signers"; then
  err "$MANIFEST revokes keys (its revokedKeys is not empty) but is not signed by the backup key of $update_signers: apps ignore a revocation that any other key signed"
  exit 1
fi
printf 'check-draft: ok: %s; %s is signed by the backup update key and revokes keys\n' "$ok" "$MANIFEST"
