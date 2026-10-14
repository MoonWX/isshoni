#!/bin/sh
# Is a downloaded release draft exactly what the release key signed? (docs/m1/06-deploy-and-ci.md §9.3, "06" below)
#
#   sh deploy/release/check-draft.sh DIR ALLOWED_SIGNERS
#
# release.yml's publish job runs it right before the draft goes public, on the assets that `gh release download` put
# into DIR, with the tag's deploy/keys/allowed_signers: anything with a write token could have changed the draft
# after the sign job signed it (README F09, review R4). It passes (exit 0, a line that says ok) only when all three
# checks pass, in this order:
#   1. DIR/checksums.txt.sig is a signature of DIR/checksums.txt by a key of ALLOWED_SIGNERS, for the principal
#      isshoni-release and the namespace isshoni-checksums, as install.sh checks it (06 §4.3);
#   2. every asset that checksums.txt lists is in DIR with that SHA-256 (`sha256sum --strict -c`, so a malformed
#      line fails too);
#   3. DIR holds no file that checksums.txt does not list, hidden ones included, except checksums.txt itself, its
#      .sig and its cosign bundle checksums.txt.sigstore.json: no signature would cover such an asset. Each one is
#      named, not only the first.
# The signature comes first, so sha256sum reads only the names of a checksums.txt that the key signed.
#
# It reads nothing but DIR and ALLOWED_SIGNERS, and changes nothing. It needs sha256sum and ssh-keygen (OpenSSH 8.1
# or newer). In GitHub Actions (GITHUB_ACTIONS=true) its errors are error annotations.
#
# Exit codes: 0 the draft is what the key signed; 1 it is not, or a check could not run; 2 usage.
#
# deploy/test/check-draft-test.sh tests it on fixture drafts with keys made for the run; `task test:sh` runs that
# test under dash and bash --posix (README F15).
set -eu

SIGN_IDENTITY=isshoni-release
SIGN_NAMESPACE=isshoni-checksums

usage() {
  printf 'Usage: sh %s DIR ALLOWED_SIGNERS\n' "$0"
  printf 'Checks that the release draft downloaded into DIR is what a key of ALLOWED_SIGNERS signed: the\n'
  printf 'signature of checksums.txt, every checksum, and no asset that checksums.txt does not list.\n'
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
if [ $# -ne 2 ]; then
  usage >&2
  exit 2
fi
dir=$1
signers=$2

if [ ! -d "$dir" ]; then
  err "$dir is not a directory"
  exit 1
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
  . | .. | checksums.txt | checksums.txt.sig | checksums.txt.sigstore.json) continue ;;
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

printf 'check-draft: ok: checksums.txt is signed, and it lists each of the %s assets with its checksum\n' "$assets"
