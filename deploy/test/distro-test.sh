#!/bin/sh
# The install.sh scenarios in systemd containers (docs/m1/06-deploy-and-ci.md §11.2).
#
#   sh deploy/test/distro-test.sh [DISTRO...]     (`task deploy:test`, with DISTRO=debian-12 for one)
#
# DISTRO is one of the supported distros (06 §4.13): debian-12, debian-13, ubuntu-22.04, ubuntu-24.04,
# ubuntu-26.04, fedora-44. Without one, all of them run, one after the other.
#
# For each distro it builds a systemd container (deploy/test/distro/Containerfile), starts it, and runs the
# installer in it the way an admin would, piped into sh without a terminal, against a release that never leaves
# this machine:
#   - two versions of isshoni, built from this working tree for the container's CPU and packed like the archive
#     of a real release (06 §3), each with a checksums.txt signed by a key pair made for this run;
#   - a copy of install.sh per version, stamped with it, with that key between the key markers (the script has
#     no other way to take a key);
#   - tools/relserve, which serves all of it over HTTPS like GitHub Releases, with a certificate authority made
#     for this run that the container is told to trust.
# Nothing is installed on this machine, and the key, the CA and the release are deleted when the script ends.
#
# Scenarios (the numbers are 06 §11.2's):
#   C1  a fresh install of the first version with --yes --tls-mode off
#   C6  an upgrade from tampered mirrors: checksums changed, signature missing, archive swapped
#
# Environment:
#   CONTAINER_ENGINE   podman or docker (default: podman when it is installed, docker otherwise)
#   KEEP=1             leave the container and the work directory in place, to look around after a failure
#
# It needs Go, ssh-keygen (OpenSSH 8.1 or newer) and a container engine. The binary carries the web app only when
# web/dist was built before (task build:web); the scenarios here don't open it.
set -eu

# install.sh reads ISSHONI_ variables, and a caller may have some set (CI sets ISSHONI_VERSION for its builds).
for dt_name in $(env | sed -n 's/^\(ISSHONI_[A-Za-z0-9_]*\)=.*/\1/p'); do
  unset "$dt_name"
done

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
V1=0.0.1-ci.1
V2=0.0.1-ci.2
VERSION_PKG=github.com/MoonWX/isshoni/internal/version
# The name a container reaches this machine by. podman provides it; docker gets it with --add-host.
HOST_NAME=host.containers.internal
KEEP=${KEEP:-0}

ENGINE=${CONTAINER_ENGINE:-}
if [ -z "$ENGINE" ]; then
  if command -v podman >/dev/null 2>&1; then
    ENGINE=podman
  else
    ENGINE=docker
  fi
fi

WORK=''
CONTAINER=''
RELSERVE_PID=''
BASE_URL=''
ARCH=''
PASSED=0
FAILED=0
RC=0
OUT=''

# ---------------------------------------------------------------------------------------------------------------
# Helpers

say() {
  printf '%s\n' "$@"
}

die() {
  printf 'distro-test: %s\n' "$@" >&2
  exit 1
}

# The supported distros (06 §4.13). Fedora's entry follows the current Fedora release.
DISTROS='debian-12 debian-13 ubuntu-22.04 ubuntu-24.04 ubuntu-26.04 fedora-44'

# base_image DISTRO prints the image a distro's container is built from, or fails for a name that is not in the
# list.
base_image() {
  case $1 in
  debian-12) say debian:12 ;;
  debian-13) say debian:13 ;;
  ubuntu-22.04) say ubuntu:22.04 ;;
  ubuntu-24.04) say ubuntu:24.04 ;;
  ubuntu-26.04) say ubuntu:26.04 ;;
  fedora-44) say fedora:44 ;;
  *) return 1 ;;
  esac
}

ok() {
  PASSED=$((PASSED + 1))
  printf '  ok   %s\n' "$1"
}

fail() {
  FAILED=$((FAILED + 1))
  printf '  FAIL %s\n' "$1"
  shift
  [ $# -eq 0 ] || printf '       %s\n' "$@"
}

assert_eq() {
  if [ "$2" = "$3" ]; then
    ok "$1"
  else
    fail "$1" "want: $2" "got:  $3"
  fi
}

assert_contains() {
  case $3 in
  *"$2"*) ok "$1" ;;
  *) fail "$1" "want it to contain: $2" ;;
  esac
}

# sha256 FILE prints the file's SHA-256, on Linux and on macOS.
sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{ print $1 }'
  else
    shasum -a 256 "$1" | awk '{ print $1 }'
  fi
}

# in_container CMD ARGS... runs a command in the container, as root and without a terminal.
in_container() {
  "$ENGINE" exec "$CONTAINER" "$@"
}

# value CMD ARGS... prints what a command in the container prints, or "(failed)".
value() {
  in_container "$@" 2>/dev/null || printf '(failed)'
}

# json_of COMMAND PATH runs a shell command in the container and prints one value of the JSON it prints; PATH is
# the Python subscript of the value, like '["server"]["version"]'. The image has python3 for this.
json_of() {
  # shellcheck disable=SC2016 # the variables are the container shell's
  in_container sh -c '$1 | python3 -c "import json, sys; print(json.load(sys.stdin)$2)"' sh "$1" "$2" 2>/dev/null ||
    printf '(failed)'
}

# served_version prints the version that the running server reports to its clients.
served_version() {
  json_of 'curl -fsS http://127.0.0.1:8080/api/v1/info' '["server"]["version"]'
}

cleanup() {
  if [ -n "$RELSERVE_PID" ]; then
    kill "$RELSERVE_PID" 2>/dev/null || :
    wait "$RELSERVE_PID" 2>/dev/null || :
    RELSERVE_PID=''
  fi
  if [ "$KEEP" = 1 ]; then
    say "distro-test: KEEP=1, the containers isshoni-distro-test-*-$$ and $WORK are still there"
    return 0
  fi
  if [ -n "$CONTAINER" ]; then
    "$ENGINE" rm -f "$CONTAINER" >/dev/null 2>&1 || :
    CONTAINER=''
  fi
  if [ -n "$WORK" ]; then
    rm -rf "$WORK"
    WORK=''
  fi
}

# ---------------------------------------------------------------------------------------------------------------
# The test release

# make_installer VERSION FILE writes the copy of install.sh that a release of VERSION would ship: stamped with the
# version, as `task release:prepare` does, and with this run's public key as its key block.
make_installer() {
  awk -v version="$1" -v key="$(cat "$WORK/key.pub")" -v q="'" '
    /^# BEGIN ALLOWED SIGNERS/ {
      print
      print "ALLOWED_SIGNERS=" q "isshoni-release namespaces=\"isshoni-checksums\" " key q
      skip = 1
      next
    }
    /^# END ALLOWED SIGNERS/ { skip = 0 }
    skip { next }
    $0 == "ISSHONI_INSTALLER_VERSION=\"@VERSION@\"" { print "ISSHONI_INSTALLER_VERSION=\"" version "\""; next }
    { print }' "$ROOT/deploy/install.sh" >"$2"
  grep -q "^ISSHONI_INSTALLER_VERSION=\"$1\"\$" "$2" || die 'deploy/install.sh has no version line to stamp'
  grep -q '^ALLOWED_SIGNERS=.isshoni-release namespaces="isshoni-checksums" ssh-ed25519 ' "$2" ||
    die 'deploy/install.sh has no key block to fill'
}

# build_release VERSION builds the release of VERSION into $WORK/root/download/vVERSION: the archive for $ARCH with
# the files of a real one, the installer, checksums.txt and its signature.
build_release() {
  br_dir=$WORK/root/download/v$1
  br_stage=$WORK/stage-$1
  br_archive=isshoni_$1_linux_$ARCH.tar.gz
  mkdir -p "$br_dir" "$br_stage/deploy/systemd" "$br_stage/deploy/sysctl" "$br_stage/deploy/firewall"
  say "distro-test: building isshoni $1 for linux/$ARCH"
  (cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH go build -trimpath \
    -ldflags "-s -w -X $VERSION_PKG.version=$1" -o "$br_stage/isshoni" ./cmd/isshoni)
  cp "$ROOT/LICENSE" "$ROOT/NOTICE" "$br_stage/"
  cp "$ROOT/deploy/systemd/isshoni.service" "$br_stage/deploy/systemd/"
  cp "$ROOT/deploy/sysctl/60-isshoni.conf" "$br_stage/deploy/sysctl/"
  cp "$ROOT/deploy/firewall/ufw-isshoni" "$ROOT/deploy/firewall/firewalld-isshoni.xml" "$br_stage/deploy/firewall/"
  # COPYFILE_DISABLE keeps macOS's tar from adding its ._ files.
  COPYFILE_DISABLE=1 tar -czf "$br_dir/$br_archive" -C "$br_stage" isshoni LICENSE NOTICE deploy
  make_installer "$1" "$br_dir/install.sh"
  for br_file in "$br_archive" install.sh; do
    printf '%s  %s\n' "$(sha256 "$br_dir/$br_file")" "$br_file"
  done >"$br_dir/checksums.txt"
  ssh-keygen -Y sign -f "$WORK/key" -n isshoni-checksums "$br_dir/checksums.txt" >/dev/null 2>&1 ||
    die 'ssh-keygen could not sign checksums.txt (OpenSSH 8.1 or newer is needed)'
}

# make_mirrors VERSION makes three mirrors of the release of VERSION, each with one thing wrong (scenario C6):
#   tampered-checksums   one character of the archive's checksum is changed; the signature is the original
#   tampered-nosig       checksums.txt.sig is missing
#   tampered-archive     the archive is another one (the first version's); checksums and signature are original
make_mirrors() {
  mm_good=$WORK/root/download/v$1
  mm_archive=isshoni_$1_linux_$ARCH.tar.gz
  for mm_name in checksums nosig archive; do
    mkdir -p "$WORK/root/tampered-$mm_name/download/v$1"
    cp "$mm_good"/* "$WORK/root/tampered-$mm_name/download/v$1/"
  done
  awk -v f="$mm_archive" '
    $2 == f { first = substr($1, 1, 1); print (first == "0" ? "1" : "0") substr($1, 2) "  " $2; next }
    { print }' "$mm_good/checksums.txt" >"$WORK/root/tampered-checksums/download/v$1/checksums.txt"
  rm "$WORK/root/tampered-nosig/download/v$1/checksums.txt.sig"
  cp "$WORK/root/download/v$V1/isshoni_${V1}_linux_$ARCH.tar.gz" "$WORK/root/tampered-archive/download/v$1/$mm_archive"
}

# start_relserve serves $WORK/root over HTTPS and sets BASE_URL to where a container finds it.
start_relserve() {
  (cd "$ROOT/tools" && go build -o "$WORK/relserve" ./relserve)
  # Docker Desktop forwards a container's connection to this machine's loopback address. Elsewhere the container
  # comes in through a bridge, so the server listens on every address, for the minutes the test runs.
  sr_addr=0.0.0.0:0
  if [ "$(uname -s)" = Darwin ]; then
    sr_addr=127.0.0.1:0
  fi
  "$WORK/relserve" -dir "$WORK/root" -latest "$V2" -ca-out "$WORK/ca.pem" -addr "$sr_addr" \
    -ready-file "$WORK/addr" >"$WORK/relserve.log" 2>&1 &
  RELSERVE_PID=$!
  sr_tries=0
  while [ ! -s "$WORK/addr" ]; do
    sr_tries=$((sr_tries + 1))
    if [ "$sr_tries" -gt 100 ] || ! kill -0 "$RELSERVE_PID" 2>/dev/null; then
      cat "$WORK/relserve.log" >&2
      die 'relserve did not start'
    fi
    sleep 0.1
  done
  sr_bound=$(cat "$WORK/addr")
  BASE_URL=https://$HOST_NAME:${sr_bound##*:}
}

# ---------------------------------------------------------------------------------------------------------------
# The container

# start_container IMAGE starts a container with systemd as PID 1, waits until systemd is up, and makes the
# container trust the CA of relserve.
start_container() {
  case $ENGINE in
  podman) "$ENGINE" run -d --name "$CONTAINER" --systemd=always "$1" >/dev/null ;;
  *)
    "$ENGINE" run -d --name "$CONTAINER" --privileged --cgroupns=private --tmpfs /run --tmpfs /run/lock \
      --add-host="$HOST_NAME:host-gateway" "$1" >/dev/null
    ;;
  esac
  sc_tries=0
  while :; do
    sc_state=$(in_container systemctl is-system-running 2>/dev/null) || :
    case $sc_state in
    running | degraded) break ;;
    esac
    sc_tries=$((sc_tries + 1))
    if [ "$sc_tries" -gt 120 ]; then
      "$ENGINE" logs "$CONTAINER" 2>&1 | tail -n 20 >&2 || :
      die "systemd did not come up in the container (state: ${sc_state:-none})"
    fi
    sleep 0.5
  done
  if in_container sh -c 'command -v update-ca-certificates' >/dev/null 2>&1; then
    "$ENGINE" cp "$WORK/ca.pem" "$CONTAINER:/usr/local/share/ca-certificates/relserve.crt" >/dev/null
    in_container update-ca-certificates >/dev/null 2>&1
  else
    "$ENGINE" cp "$WORK/ca.pem" "$CONTAINER:/etc/pki/ca-trust/source/anchors/relserve.crt" >/dev/null
    in_container update-ca-trust >/dev/null 2>&1
  fi
  in_container curl -fsS -o /dev/null "$BASE_URL/tag/v$V1" ||
    die "the container does not reach relserve at $BASE_URL" "$(cat "$WORK/relserve.log")"
}

# run_installer VERSION MIRROR ARGS... runs the installer of VERSION in the container as an admin would: fetched
# with curl and fed to sh on stdin, without a terminal. MIRROR is the path of the release root under relserve, ""
# for the good one. The exit code goes to RC, and stdout with stderr to OUT, which is printed too.
run_installer() {
  ri_version=$1
  ri_mirror=$2
  shift 2
  RC=0
  # shellcheck disable=SC2016 # the variables are the container shell's
  "$ENGINE" exec -e "ISSHONI_DOWNLOAD_BASE=$BASE_URL$ri_mirror" -e ISSHONI_PUBLIC_URL=http://127.0.0.1:8080 \
    "$CONTAINER" sh -c 'url=$1; shift; curl -fsSL -o /root/install.sh "$url" && sh -s -- "$@" </root/install.sh' \
    sh "$BASE_URL/download/v$ri_version/install.sh" "$@" >"$WORK/out" 2>&1 || RC=$?
  OUT=$(cat "$WORK/out")
  sed 's/^/  | /' "$WORK/out"
}

# ---------------------------------------------------------------------------------------------------------------
# Scenarios

# C1: a fresh install of the first version, behind "a proxy" on the loopback address (tls.mode off), so that no
# certificate is needed.
scenario_c1() {
  say '' "C1: fresh install of $V1 with --yes --tls-mode off"
  assert_eq 'C1 before: the container has no ssh-keygen' 'no' \
    "$(in_container sh -c 'command -v ssh-keygen >/dev/null 2>&1 && echo yes || echo no')"

  run_installer "$V1" '' --yes --tls-mode off
  assert_eq 'C1: exit code' 0 "$RC"

  assert_eq 'C1: the service is active' active "$(value systemctl is-active isshoni)"
  assert_eq 'C1: the service is enabled' enabled "$(value systemctl is-enabled isshoni)"
  c1_pid=$(value systemctl show -p MainPID --value isshoni)
  assert_eq 'C1: the process runs as isshoni' isshoni "$(value ps -o user= -p "$c1_pid" | tr -d ' ')"
  # shellcheck disable=SC2016 # an awk program
  assert_eq 'C1: CapEff is CAP_NET_BIND_SERVICE alone' 0000000000000400 \
    "$(value awk '$1 == "CapEff:" { print $2 }' "/proc/$c1_pid/status")"
  assert_eq 'C1: the program' 'root:root 755' "$(value stat -c '%U:%G %a' /usr/local/bin/isshoni)"
  assert_eq 'C1: the unit' 'root:root 644' "$(value stat -c '%U:%G %a' /usr/local/lib/systemd/system/isshoni.service)"
  assert_eq 'C1: the config' 'root:isshoni 640' "$(value stat -c '%U:%G %a' /etc/isshoni/isshoni.toml)"
  assert_eq 'C1: the config directory' 'root:root 755' "$(value stat -c '%U:%G %a' /etc/isshoni)"
  assert_eq 'C1: the data directory' 'isshoni:isshoni 700' "$(value stat -c '%U:%G %a' /var/lib/isshoni)"
  assert_eq 'C1: openssh-client was installed by the installer' yes \
    "$(in_container sh -c 'command -v ssh-keygen >/dev/null 2>&1 && echo yes || echo no')"
  assert_eq 'C1: the installed version' "$V1" "$(value /usr/local/bin/isshoni version --short)"
  assert_eq 'C1: GET /api/v1/info says the version' "$V1" "$(served_version)"
  assert_contains 'C1: isshoni setup-url --json gives a setup link' '/setup#' \
    "$(json_of 'isshoni setup-url --json' '["url"]')"
  assert_contains 'C1: the installer printed a setup link' '/setup#' "$OUT"
  assert_contains 'C1: the installer said that isshoni runs' "isshoni $V1 is running." "$OUT"
  assert_contains 'C1: the firewall step ran' 'Firewall:' "$OUT"
  assert_eq 'C1: no ISSHONI_ variable reached the service' '' \
    "$(in_container sh -c "tr '\\0' '\\n' </proc/$c1_pid/environ | grep '^ISSHONI_' || :")"
}

# C6: an upgrade to the second version from three mirrors that each have one thing wrong. Every run must end with
# exit 3 and leave the program and the running service as they were.
scenario_c6() {
  say '' "C6: upgrade to $V2 from tampered mirrors"
  c6_sum=$(value sha256sum /usr/local/bin/isshoni)
  c6_since=$(value systemctl show -p ActiveEnterTimestampMonotonic --value isshoni)
  c6_config=$(value sha256sum /etc/isshoni/isshoni.toml)
  for c6_name in checksums nosig archive; do
    say '' "C6: the mirror tampered-$c6_name"
    run_installer "$V2" "/tampered-$c6_name" --yes
    assert_eq "C6 $c6_name: exit code" 3 "$RC"
    # Each mirror is refused for the one thing that is wrong with it.
    case $c6_name in
    checksums) c6_why='The signature of checksums.txt is not valid' ;;
    nosig) c6_why="Could not download $BASE_URL/tampered-nosig/download/v$V2/checksums.txt.sig" ;;
    *) c6_why='does not match checksums.txt' ;;
    esac
    assert_contains "C6 $c6_name: the reason" "$c6_why" "$OUT"
    assert_contains "C6 $c6_name: the installer says that nothing changed" 'Nothing was installed or changed.' "$OUT"
    assert_eq "C6 $c6_name: the program is unchanged" "$c6_sum" "$(value sha256sum /usr/local/bin/isshoni)"
    assert_eq "C6 $c6_name: no new program was left behind" no \
      "$(in_container sh -c 'test -e /usr/local/bin/.isshoni.new && echo yes || echo no')"
    assert_eq "C6 $c6_name: the config is unchanged" "$c6_config" "$(value sha256sum /etc/isshoni/isshoni.toml)"
    assert_eq "C6 $c6_name: the service is active" active "$(value systemctl is-active isshoni)"
    assert_eq "C6 $c6_name: the service was not restarted" "$c6_since" \
      "$(value systemctl show -p ActiveEnterTimestampMonotonic --value isshoni)"
    assert_eq "C6 $c6_name: the running version" "$V1" "$(served_version)"
  done
}

# run_distro DISTRO runs every scenario in a fresh container of one distro.
run_distro() {
  rd_base=$(base_image "$1")
  rd_image=isshoni-distro-test:$1
  say '' "==== $1 ($rd_base, $ENGINE)"
  "$ENGINE" build -q -t "$rd_image" --build-arg "BASE=$rd_base" -f "$ROOT/deploy/test/distro/Containerfile" \
    "$ROOT/deploy/test/distro" >/dev/null
  rd_arch=$("$ENGINE" image inspect -f '{{.Architecture}}' "$rd_image")
  case $rd_arch in
  amd64 | arm64) ;;
  *) die "the image $rd_image is for $rd_arch, and isshoni is built for amd64 and arm64" ;;
  esac
  if [ "$rd_arch" != "$ARCH" ]; then
    # The first distro, or one whose image is for another CPU: build the release for it.
    ARCH=$rd_arch
    build_release "$V1"
    build_release "$V2"
    make_mirrors "$V2"
  fi
  CONTAINER=isshoni-distro-test-$1-$$
  start_container "$rd_image"
  scenario_c1
  scenario_c6
  if [ "$KEEP" != 1 ]; then
    "$ENGINE" rm -f "$CONTAINER" >/dev/null 2>&1 || :
    CONTAINER=''
  fi
}

main() {
  # shellcheck disable=SC2086 # the list of names
  [ $# -gt 0 ] || set -- $DISTROS
  for m_distro in "$@"; do
    base_image "$m_distro" >/dev/null || die "unknown distro \"$m_distro\"" "known: $DISTROS"
  done
  for m_tool in go ssh-keygen tar awk "$ENGINE"; do
    command -v "$m_tool" >/dev/null 2>&1 || die "\"$m_tool\" is needed and not installed"
  done

  WORK=$(mktemp -d)
  trap cleanup EXIT
  trap 'exit 1' INT TERM
  mkdir -p "$WORK/root"
  ssh-keygen -q -t ed25519 -N '' -C isshoni-distro-test -f "$WORK/key" >/dev/null
  start_relserve

  for m_distro in "$@"; do
    run_distro "$m_distro"
  done

  say '' "distro-test: $PASSED passed, $FAILED failed"
  [ "$FAILED" -eq 0 ]
}

main "$@"
