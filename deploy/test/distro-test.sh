#!/bin/sh
# The install.sh scenarios in systemd containers (docs/m1/06-deploy-and-ci.md §11.2, "06" below).
#
#   sh deploy/test/distro-test.sh [-r DIR] [-o PARTS] [DISTRO...]   (`task deploy:test`, DISTRO=debian-12 for one)
#   sh deploy/test/distro-test.sh -b DIR                            build the test releases into DIR, and stop
#
# DISTRO is one of the supported distros (06 §4.13): debian-12, debian-13, ubuntu-22.04, ubuntu-24.04,
# ubuntu-26.04, fedora-44. Without one, all of them run, one after the other.
#
# The release. Two versions of isshoni, 0.0.1-ci.1 and 0.0.1-ci.2, are built as a release is: `task
# release:snapshot` (goreleaser, .goreleaser.yaml) with ISSHONI_SNAPSHOT_VERSION, which builds the web app for that
# version first, without the images (--skip=docker). The scenarios take from each snapshot what a release serves to
# the installer: the two linux archives, checksums.txt and install.sh, stamped with the version by goreleaser's
# before hook. This run then signs each checksums.txt with a key pair made for it, puts that key between install.sh's
# key markers (the script has no other way to take a key) and updates install.sh's line in checksums.txt, and
# tools/relserve serves the result over HTTPS like GitHub Releases, with a certificate authority made for the run
# that the containers are told to trust. A snapshot needs web/node_modules (task setup) and replaces dist/,
# dist-extra/ and web/dist of the working tree, as `task release:snapshot` always does. -b builds the releases into
# DIR and stops, and -r takes them from such a DIR instead of building them: CI builds them once and runs the
# distros in parallel.
#
# The containers. For each distro a systemd container (deploy/test/distro/Containerfile, without the OpenSSH client,
# which the installer has to install itself) runs the installer the way an admin does, fetched with curl and fed to
# sh without a terminal. The parts (-o) and the scenarios they run (the numbers are 06 §11.2's):
#   main       one container, in this order: C12 non-root without sudo; C1 fresh install of ci.1 with --yes
#              --tls-mode off; C2 seed the admin; C3 the same version again; C6 an upgrade from tampered mirrors;
#              C4 the upgrade to ci.2; C5 a downgrade without --allow-downgrade; C8 --uninstall; C9 the installer
#              again after the uninstall; C10 backup, --purge, a fresh install and the restore
#   c7         a second container: a port is taken by another program (exit 7), then free (the run finishes)
#   c13        a third container: every start fails (exit 5), then the cause is fixed (the run finishes)
#   c11        the distro's plain image, without systemd (exit 4)
#   v3         fedora-44 and ubuntu-24.04 only, a container with a running firewall: the firewall offer opens the
#              ports, and --uninstall takes them out again. On Fedora the service is first put into a second zone,
#              a policy and a rich rule, and written by hand into a third zone's file that firewalld has not read;
#              that file keeps isshoni.xml, and firewalld must still load its configuration after the uninstall
#              (06 §4.9, §4.11). 06 §11.2 also has V3 in the nightly's virtual machines (README S83).
# Default: all of them.
#
# The scenarios give the installer a public address of the documentation range (ISSHONI_PUBLIC_IP), which keeps
# this machine's real address out of the output (it ends up in CI logs and pull requests). Doctor's NAT check still
# asks the STUN servers, though, so the pre-check's public_ip result depends on the network of the machine that
# runs the containers: behind Docker Desktop's NAT it fails, as a symmetric NAT. C1 asserts that the pre-check ran
# and checked the public address, and the dns result, which off mode decides without the network; not whether the
# pre-check passed.
#
# Nothing is installed on this machine. The key, the CA, the releases and the containers are deleted when the
# script ends. The container images, $IMAGE_NAME:<distro> (and :<distro>-firewall), stay: they are the build cache of
# the next run (a few hundred MB each). Remove them with `docker image rm` (or `podman image rm`).
#
# Environment:
#   CONTAINER_ENGINE   podman or docker (default: podman when it is installed, docker otherwise)
#   IMAGE_NAME         the name of the images, and the prefix of the containers' names (default isshoni-distro-test)
#   KEEP=1             leave the containers and the work directory in place, to look around after a failure
#
# It needs Go, ssh-keygen (OpenSSH 8.1 or newer), a container engine, and for the releases Task (.bin/task or
# task) and the web app's npm packages.
set -eu

# install.sh reads ISSHONI_ variables, and a caller may have some set (CI sets ISSHONI_VERSION for its builds).
for dt_name in $(env | sed -n 's/^\(ISSHONI_[A-Za-z0-9_]*\)=.*/\1/p'); do
  unset "$dt_name"
done

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
V1=0.0.1-ci.1
V2=0.0.1-ci.2
# The name a container reaches this machine by. podman provides it; docker gets it with --add-host.
HOST_NAME=host.containers.internal
# The public address the installed server is told it has (RFC 5737's TEST-NET-3).
PUBLIC_IP=203.0.113.7
# The admin account that C2 creates.
ADMIN_NAME=alex
KEEP=${KEEP:-0}
IMAGE_NAME=${IMAGE_NAME:-isshoni-distro-test}
PARTS_ALL='main c7 c13 c11 v3'

ENGINE=${CONTAINER_ENGINE:-}
if [ -z "$ENGINE" ]; then
  if command -v podman >/dev/null 2>&1; then
    ENGINE=podman
  else
    ENGINE=docker
  fi
fi

WORK=''
CONTAINER=''  # the container the scenario runs in
CONTAINERS='' # every container this run started, for the cleanup
RELSERVE_PID=''
BASE_URL=''
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

usage() {
  sed -n '2,/^set -eu$/p' "$0" | sed -e '$d' -e 's/^# \{0,1\}//'
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

# firewall_of DISTRO prints the firewall that scenario V3 runs on the distro (06 §11.2: ufw on Ubuntu, firewalld on
# Fedora), or fails for a distro that V3 leaves out.
firewall_of() {
  case $1 in
  fedora-44) say firewalld ;;
  ubuntu-24.04) say ufw ;;
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

assert_lacks() {
  case $3 in
  *"$2"*) fail "$1" "want it not to contain: $2" ;;
  *) ok "$1" ;;
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

# yes_no CMD ARGS... prints yes when a command in the container succeeds, and no otherwise.
yes_no() {
  if in_container "$@" >/dev/null 2>&1; then
    printf yes
  else
    printf no
  fi
}

# exit_of CMD ARGS... prints the exit code of a command in the container.
exit_of() {
  eo_rc=0
  in_container "$@" >/dev/null 2>&1 || eo_rc=$?
  printf '%s' "$eo_rc"
}

# json_of COMMAND EXPR runs a shell command in the container and prints a Python expression of the JSON it prints,
# with the document as d: '["server"]["version"]' style subscripts go into EXPR as d["server"]["version"]. The image
# has python3 for this.
json_of() {
  # shellcheck disable=SC2016 # the variables are the container shell's
  in_container sh -c '$1 | python3 -c "$2"' sh "$1" "import json, sys; d = json.load(sys.stdin); print($2)" \
    2>/dev/null || printf '(failed)'
}

# served_version prints the version that the running server reports to its clients.
served_version() {
  json_of 'curl -fsS http://127.0.0.1:8080/api/v1/info' 'd["server"]["version"]'
}

# admin_count prints the number of admin accounts that `isshoni admin users list --json` lists.
admin_count() {
  json_of 'isshoni admin users list --json' 'len([u for u in d["users"] if u["role"] == "admin"])'
}

# post PATH prints the HTTP status of a POST of the JSON on stdin to the server's PATH, as a client that is no
# browser sends it (03 §7.5).
post() {
  "$ENGINE" exec -i "$CONTAINER" curl -s -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' \
    --data-binary @- "http://127.0.0.1:8080$1" 2>/dev/null || printf '(failed)'
}

# login prints the HTTP status of a login of the admin that C2 created.
login() {
  printf '{"username":"%s","password":"%s"}' "$ADMIN_NAME" "$(cat "$WORK/password")" | post /api/v1/auth/login
}

# wait_ready waits up to 60 seconds for the server to be ready.
wait_ready() {
  wr_tries=0
  while ! in_container isshoni healthcheck --ready >/dev/null 2>&1; do
    wr_tries=$((wr_tries + 1))
    [ "$wr_tries" -lt 60 ] || return 1
    sleep 1
  done
}

# file_state PATH... prints, for each path, whether it is in the container: "/etc/isshoni=yes ...".
file_state() {
  fs_out=''
  for fs_path in "$@"; do
    fs_out="$fs_out $fs_path=$(yes_no test -e "$fs_path")"
  done
  printf '%s' "${fs_out# }"
}

cleanup() {
  if [ -n "$RELSERVE_PID" ]; then
    kill "$RELSERVE_PID" 2>/dev/null || :
    wait "$RELSERVE_PID" 2>/dev/null || :
    RELSERVE_PID=''
  fi
  if [ "$KEEP" = 1 ]; then
    [ -z "$CONTAINERS" ] || say "distro-test: KEEP=1, the containers are still there:$CONTAINERS"
    [ -z "$WORK" ] || say "distro-test: KEEP=1, and the work directory: $WORK"
    return 0
  fi
  for cl_name in $CONTAINERS; do
    "$ENGINE" rm -f "$cl_name" >/dev/null 2>&1 || :
  done
  CONTAINERS=''
  if [ -n "$WORK" ]; then
    rm -rf "$WORK"
    WORK=''
  fi
}

# ---------------------------------------------------------------------------------------------------------------
# The test release

# find_task prints the Task to run: the pinned one in .bin/ (task setup builds it), else the one on the PATH.
find_task() {
  if [ -x "$ROOT/.bin/task" ]; then
    say "$ROOT/.bin/task"
  elif command -v task >/dev/null 2>&1; then
    command -v task
  else
    die 'Task is needed for the release snapshots: build it with "go -C tools build -o ../.bin/ github.com/go-task/task/v3/cmd/task" (CONTRIBUTING.md)'
  fi
}

# build_releases DIR builds the two versions with `task release:snapshot` and copies what the installer downloads
# into DIR/download/v<version>/: the linux archives, checksums.txt and the stamped install.sh. Unsigned: each run
# signs them with its own key (prepare_releases).
build_releases() {
  br_task=$(find_task)
  [ -d "$ROOT/web/node_modules" ] || die 'web/node_modules is missing: run "task setup" (npm ci in web/) first'
  br_log=${WORK:-$1}/snapshot.log
  for br_version in "$V1" "$V2"; do
    say "distro-test: building the release $br_version (task release:snapshot -- --skip=docker)"
    if ! (cd "$ROOT" && ISSHONI_SNAPSHOT_VERSION=$br_version "$br_task" release:snapshot -- --skip=docker) \
      >"$br_log" 2>&1; then
      tail -n 40 "$br_log" >&2
      die "task release:snapshot failed for $br_version (the end of its output is above)"
    fi
    br_dir=$1/download/v$br_version
    mkdir -p "$br_dir"
    cp "$ROOT/dist/checksums.txt" "$ROOT/dist/isshoni_${br_version}_linux_amd64.tar.gz" \
      "$ROOT/dist/isshoni_${br_version}_linux_arm64.tar.gz" "$br_dir/"
    cp "$ROOT/dist-extra/install.sh" "$br_dir/install.sh"
    grep -q "^ISSHONI_INSTALLER_VERSION=\"$br_version\"\$" "$br_dir/install.sh" ||
      die "the snapshot's install.sh is not stamped with $br_version"
  done
  rm -f "$br_log"
}

# put_key FILE writes this run's public key into the key block of the installer FILE, which is a release's copy.
put_key() {
  pk_new=$1.new
  awk -v key="$(cat "$WORK/key.pub")" -v q="'" '
    /^# BEGIN ALLOWED SIGNERS/ {
      print
      print "ALLOWED_SIGNERS=" q "isshoni-release namespaces=\"isshoni-checksums\" " key q
      skip = 1
      next
    }
    /^# END ALLOWED SIGNERS/ { skip = 0 }
    skip { next }
    { print }' "$1" >"$pk_new"
  grep -q '^ALLOWED_SIGNERS=.isshoni-release namespaces="isshoni-checksums" ssh-ed25519 ' "$pk_new" ||
    die "$1 has no key block to fill"
  mv "$pk_new" "$1"
}

# sign_release DIR updates the line of install.sh in DIR/checksums.txt, which put_key changed, and signs
# checksums.txt with this run's key.
sign_release() {
  awk -v sum="$(sha256 "$1/install.sh")" '$2 == "install.sh" { $1 = sum } { print $1 "  " $2 }' \
    "$1/checksums.txt" >"$1/checksums.txt.new"
  mv "$1/checksums.txt.new" "$1/checksums.txt"
  rm -f "$1/checksums.txt.sig"
  ssh-keygen -Y sign -f "$WORK/key" -n isshoni-checksums "$1/checksums.txt" >/dev/null 2>&1 ||
    die 'ssh-keygen could not sign checksums.txt (OpenSSH 8.1 or newer is needed)'
}

# prepare_releases SRC copies the releases built by build_releases from SRC into the release root that relserve
# serves, and gives each this run's key and signature. Then it makes the three mirrors of scenario C6, each with one
# thing wrong in the release of the second version:
#   tampered-checksums   one character of each archive's checksum is changed; the signature is the original
#   tampered-nosig       checksums.txt.sig is missing
#   tampered-archive     the archives are the first version's; checksums and signature are original
prepare_releases() {
  for pr_version in "$V1" "$V2"; do
    pr_dir=$WORK/root/download/v$pr_version
    [ -f "$1/download/v$pr_version/checksums.txt" ] || die "$1 has no release $pr_version (build it with -b)"
    mkdir -p "$pr_dir"
    cp "$1/download/v$pr_version/"* "$pr_dir/"
    grep -q "^ISSHONI_INSTALLER_VERSION=\"$pr_version\"\$" "$pr_dir/install.sh" ||
      die "the installer of $pr_version is not stamped with its version"
    put_key "$pr_dir/install.sh"
    sign_release "$pr_dir"
  done
  pr_good=$WORK/root/download/v$V2
  for pr_name in checksums nosig archive; do
    mkdir -p "$WORK/root/tampered-$pr_name/download/v$V2"
    cp "$pr_good"/* "$WORK/root/tampered-$pr_name/download/v$V2/"
  done
  awk '
    $2 ~ /_linux_(amd64|arm64)\.tar\.gz$/ { first = substr($1, 1, 1); print (first == "0" ? "1" : "0") substr($1, 2) "  " $2; next }
    { print }' "$pr_good/checksums.txt" >"$WORK/root/tampered-checksums/download/v$V2/checksums.txt"
  rm "$WORK/root/tampered-nosig/download/v$V2/checksums.txt.sig"
  for pr_arch in amd64 arm64; do
    cp "$WORK/root/download/v$V1/isshoni_${V1}_linux_$pr_arch.tar.gz" \
      "$WORK/root/tampered-archive/download/v$V2/isshoni_${V2}_linux_$pr_arch.tar.gz"
  done
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
# The containers

# build_image TAG BASE [EXTRA] builds a test image from the Containerfile, with the distro's EXTRA packages.
build_image() {
  "$ENGINE" build -q -t "$1" --build-arg "BASE=$2" --build-arg "EXTRA=${3-}" \
    -f "$ROOT/deploy/test/distro/Containerfile" "$ROOT/deploy/test/distro" >/dev/null
}

# start_container NAME IMAGE starts a container with systemd as PID 1, makes it the one the scenarios run in, waits
# until systemd is up, and makes the container trust the CA of relserve. A container for a firewall gets all
# privileges under podman too.
start_container() {
  CONTAINER=$IMAGE_NAME-$1-$$
  CONTAINERS="$CONTAINERS $CONTAINER"
  case $ENGINE in
  podman)
    sc_priv=''
    case $1 in *-v3) sc_priv=--privileged ;; esac
    # shellcheck disable=SC2086 # one flag or none
    "$ENGINE" run -d --name "$CONTAINER" --systemd=always $sc_priv "$2" >/dev/null
    ;;
  *)
    "$ENGINE" run -d --name "$CONTAINER" --privileged --cgroupns=private --tmpfs /run --tmpfs /run/lock \
      --add-host="$HOST_NAME:host-gateway" "$2" >/dev/null
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

# stop_container removes the container the scenarios ran in, unless KEEP=1.
stop_container() {
  if [ "$KEEP" != 1 ]; then
    "$ENGINE" rm -f "$CONTAINER" >/dev/null 2>&1 || :
    st_left=''
    for st_name in $CONTAINERS; do
      [ "$st_name" = "$CONTAINER" ] || st_left="$st_left $st_name"
    done
    CONTAINERS=$st_left
  fi
  CONTAINER=''
}

# run_installer VERSION MIRROR ARGS... runs the installer of VERSION in the container as an admin would: fetched
# with curl and fed to sh on stdin, without a terminal, as the user RUN_AS (root by default). MIRROR is the path of
# the release root under relserve, "" for the good one. The exit code goes to RC, and stdout with stderr to OUT,
# which is printed too.
RUN_AS=root
run_installer() {
  ri_version=$1
  ri_mirror=$2
  shift 2
  RC=0
  # shellcheck disable=SC2016 # the variables are the container shell's
  "$ENGINE" exec -u "$RUN_AS" -e "ISSHONI_DOWNLOAD_BASE=$BASE_URL$ri_mirror" \
    -e ISSHONI_PUBLIC_URL=http://127.0.0.1:8080 -e "ISSHONI_PUBLIC_IP=$PUBLIC_IP" "$CONTAINER" sh -c '
      url=$1
      shift
      f=$(mktemp) || exit 1
      if curl -fsSL -o "$f" "$url"; then
        rc=0
        sh -s -- "$@" <"$f" || rc=$?
      else
        rc=1
      fi
      rm -f "$f"
      exit "$rc"' sh "$BASE_URL/download/v$ri_version/install.sh" "$@" >"$WORK/out" 2>&1 || RC=$?
  OUT=$(cat "$WORK/out")
  sed 's/^/  | /' "$WORK/out"
}

# ---------------------------------------------------------------------------------------------------------------
# Scenarios in the main container

# C12: a user that is not root, on a machine without sudo for them, gets exit 4 and changes nothing.
scenario_c12() {
  say '' 'C12: the installer run by a user who is not root'
  c12_sudo=$(yes_no sh -c 'command -v sudo')
  RUN_AS=nobody
  run_installer "$V1" '' --yes --tls-mode off
  RUN_AS=root
  assert_eq 'C12: exit code' 4 "$RC"
  if [ "$c12_sudo" = no ]; then
    assert_contains 'C12: the message' 'This installer needs root, and sudo is not installed.' "$OUT"
  else
    assert_contains 'C12: the message' 'sudo did not give this installer root.' "$OUT"
  fi
  assert_eq 'C12: nothing was installed' '/usr/local/bin/isshoni=no /etc/isshoni=no' \
    "$(file_state /usr/local/bin/isshoni /etc/isshoni)"
}

# C1: a fresh install of the first version, behind "a proxy" on the loopback address (tls.mode off), so that no
# certificate is needed.
scenario_c1() {
  say '' "C1: fresh install of $V1 with --yes --tls-mode off"
  assert_eq 'C1 before: the container has no ssh-keygen' 'no' "$(yes_no sh -c 'command -v ssh-keygen')"

  run_installer "$V1" '' --yes --tls-mode off
  assert_eq 'C1: exit code' 0 "$RC"
  assert_contains 'C1: the pre-check ran' 'Checking this server before the first start:' "$OUT"
  # Behind a proxy (off mode) the DNS check passes without a lookup. The public_ip check runs, but its result
  # depends on the host's network (see the top of this file).
  assert_contains 'C1: the pre-check checked DNS' '[ ok ] dns' "$OUT"
  assert_contains 'C1: the pre-check checked the public address' '] public_ip ' "$OUT"
  assert_contains 'C1: the config has the public address it was given' "public_ip = \"$PUBLIC_IP\"" \
    "$(value isshoni config print --config /etc/isshoni/isshoni.toml)"

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
  assert_eq 'C1: openssh-client was installed by the installer' yes "$(yes_no sh -c 'command -v ssh-keygen')"
  assert_eq 'C1: the installed version' "$V1" "$(value /usr/local/bin/isshoni version --short)"
  assert_eq 'C1: GET /api/v1/info says the version' "$V1" "$(served_version)"
  assert_contains 'C1: isshoni setup-url --json gives a setup link' '/setup#' \
    "$(json_of 'isshoni setup-url --json' 'd["url"]')"
  assert_contains 'C1: the installer printed a setup link' '/setup#' "$OUT"
  assert_contains 'C1: the installer said that isshoni runs' "isshoni $V1 is running." "$OUT"
  assert_contains 'C1: the firewall step ran' 'Firewall:' "$OUT"
  assert_eq 'C1: no ISSHONI_ variable reached the service' '' \
    "$(in_container sh -c "tr '\\0' '\\n' </proc/$c1_pid/environ | grep '^ISSHONI_' || :")"
}

# C2: the admin account is created through the setup endpoint of 03 (§12.4.2), with the token of a setup link.
scenario_c2() {
  say '' 'C2: seed the admin account'
  od -An -tx1 -N16 /dev/urandom | tr -d ' \n' >"$WORK/password"
  c2_url=$(json_of 'isshoni setup-url --json' 'd["url"]')
  assert_contains 'C2: a setup link' '/setup#' "$c2_url"
  assert_eq 'C2: POST /api/v1/auth/setup/complete' 201 "$(
    printf '{"token":"%s","username":"%s","password":"%s"}' "${c2_url#*#}" "$ADMIN_NAME" "$(cat "$WORK/password")" |
      post /api/v1/auth/setup/complete
  )"
  assert_eq 'C2: isshoni admin users list --json has one admin' 1 "$(admin_count)"
  assert_eq 'C2: the admin logs in' 200 "$(login)"
}

# C3: the installer of the installed version again: a repair that finds nothing to do.
scenario_c3() {
  say '' "C3: the installer of $V1 again"
  c3_config=$(value sha256sum /etc/isshoni/isshoni.toml)
  c3_since=$(value systemctl show -p ActiveEnterTimestampMonotonic --value isshoni)
  run_installer "$V1" ''
  assert_eq 'C3: exit code (no terminal, no flags, and no question)' 0 "$RC"
  assert_lacks 'C3: no question' 'Domain name for this server' "$OUT"
  assert_contains 'C3: setup is done' 'Setup is already done. Open http://127.0.0.1:8080/' "$OUT"
  assert_eq 'C3: the config is unchanged' "$c3_config" "$(value sha256sum /etc/isshoni/isshoni.toml)"
  assert_eq 'C3: the service was not restarted' "$c3_since" \
    "$(value systemctl show -p ActiveEnterTimestampMonotonic --value isshoni)"
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
    assert_eq "C6 $c6_name: no new program was left behind" no "$(yes_no test -e /usr/local/bin/.isshoni.new)"
    assert_eq "C6 $c6_name: the config is unchanged" "$c6_config" "$(value sha256sum /etc/isshoni/isshoni.toml)"
    assert_eq "C6 $c6_name: the service is active" active "$(value systemctl is-active isshoni)"
    assert_eq "C6 $c6_name: the service was not restarted" "$c6_since" \
      "$(value systemctl show -p ActiveEnterTimestampMonotonic --value isshoni)"
    assert_eq "C6 $c6_name: the running version" "$V1" "$(served_version)"
  done
}

# C4: the upgrade to the second version keeps the accounts.
scenario_c4() {
  say '' "C4: upgrade to $V2"
  run_installer "$V2" ''
  assert_eq 'C4: exit code' 0 "$RC"
  assert_contains 'C4: the headline' "Upgraded $V1 → $V2" "$OUT"
  assert_contains 'C4: setup is done' 'Setup is already done.' "$OUT"
  assert_eq 'C4: the installed version' "$V2" "$(value /usr/local/bin/isshoni version --short)"
  assert_eq 'C4: GET /api/v1/info says the new version' "$V2" "$(served_version)"
  assert_eq 'C4: the admin still logs in' 200 "$(login)"
  assert_eq 'C4: isshoni setup-url exits 7 (an admin exists)' 7 "$(exit_of isshoni setup-url)"
}

# C5: a downgrade without --allow-downgrade is refused, and changes nothing.
scenario_c5() {
  say '' "C5: downgrade to $V1 without --allow-downgrade"
  c5_sum=$(value sha256sum /usr/local/bin/isshoni)
  run_installer "$V2" '' --version "$V1"
  assert_eq 'C5: exit code' 6 "$RC"
  assert_contains 'C5: the message' "isshoni $V2 is installed, which is newer than $V1. Nothing was changed." "$OUT"
  assert_eq 'C5: the program is unchanged' "$c5_sum" "$(value sha256sum /usr/local/bin/isshoni)"
  assert_eq 'C5: the service is active' active "$(value systemctl is-active isshoni)"
  assert_eq 'C5: the running version' "$V2" "$(served_version)"
}

# C8: --uninstall removes isshoni and keeps its config, data and user.
scenario_c8() {
  say '' 'C8: --uninstall'
  run_installer "$V2" '' --uninstall
  assert_eq 'C8: exit code' 0 "$RC"
  assert_eq 'C8: the program, the unit and the sysctl file are gone' \
    '/usr/local/bin/isshoni=no /usr/local/lib/systemd/system/isshoni.service=no /etc/sysctl.d/60-isshoni.conf=no' \
    "$(file_state /usr/local/bin/isshoni /usr/local/lib/systemd/system/isshoni.service /etc/sysctl.d/60-isshoni.conf)"
  assert_eq 'C8: the config and the data stay' '/etc/isshoni/isshoni.toml=yes /var/lib/isshoni/isshoni.db=yes' \
    "$(file_state /etc/isshoni/isshoni.toml /var/lib/isshoni/isshoni.db)"
  assert_eq 'C8: the user stays' yes "$(yes_no getent passwd isshoni)"
  assert_eq 'C8: the service is stopped' no "$(yes_no systemctl is-active --quiet isshoni)"
  assert_eq 'C8: the unit is gone' no "$(yes_no systemctl is-enabled isshoni)"
  assert_eq 'C8: no isshoni process is left' no "$(yes_no pgrep -u isshoni)"
  assert_contains 'C8: the installer says what it kept' \
    'Kept: /etc/isshoni (config), /var/lib/isshoni (database, certificates, backups) and the isshoni user.' "$OUT"
}

# C9: the installer after an uninstall brings isshoni back with the kept config and data.
scenario_c9() {
  say '' "C9: the installer of $V2 after the uninstall"
  run_installer "$V2" ''
  assert_eq 'C9: exit code' 0 "$RC"
  assert_lacks 'C9: no question' 'Domain name for this server' "$OUT"
  assert_lacks 'C9: the kept config is used (no pre-check of a new one)' 'Checking this server before the first start' "$OUT"
  assert_eq 'C9: the service is enabled' enabled "$(value systemctl is-enabled isshoni)"
  assert_eq 'C9: the service is active' active "$(value systemctl is-active isshoni)"
  assert_contains 'C9: setup is done' 'Setup is already done. Open http://127.0.0.1:8080/' "$OUT"
  assert_eq 'C9: the admin is still there' 1 "$(admin_count)"
  assert_eq 'C9: the admin logs in' 200 "$(login)"
}

# C10: a backup, --purge, a fresh install and a restore of the backup.
scenario_c10() {
  say '' 'C10: backup, purge, install, restore'
  assert_eq 'C10: isshoni admin backup --out /root/b.tar.gz' 0 "$(exit_of isshoni admin backup --out /root/b.tar.gz)"
  run_installer "$V2" '' --purge --yes
  assert_eq 'C10: --purge exit code' 0 "$RC"
  assert_eq 'C10: nothing is left' \
    '/usr/local/bin/isshoni=no /usr/local/lib/systemd/system/isshoni.service=no /etc/isshoni=no /var/lib/isshoni=no /run/isshoni=no /etc/sysctl.d/60-isshoni.conf=no' \
    "$(file_state /usr/local/bin/isshoni /usr/local/lib/systemd/system/isshoni.service /etc/isshoni /var/lib/isshoni \
      /run/isshoni /etc/sysctl.d/60-isshoni.conf)"
  assert_eq 'C10: the user and the group are gone' 'no no' \
    "$(yes_no getent passwd isshoni) $(yes_no getent group isshoni)"
  assert_eq 'C10: the backup outside the data is still there' yes "$(yes_no test -s /root/b.tar.gz)"

  run_installer "$V2" '' --yes --tls-mode off
  assert_eq 'C10: the fresh install' 0 "$RC"
  assert_contains 'C10: the fresh install has a setup link' '/setup#' "$OUT"
  assert_eq 'C10: no admin before the restore' 0 "$(admin_count)"
  assert_eq 'C10: isshoni admin restore --yes /root/b.tar.gz' 0 "$(exit_of isshoni admin restore --yes /root/b.tar.gz)"
  if wait_ready; then
    ok 'C10: the server is ready after the restore'
  else
    fail 'C10: the server is ready after the restore' 'it was not ready within 60 s'
  fi
  assert_eq 'C10: the admin logs in again' 200 "$(login)"
  assert_eq 'C10: isshoni setup-url exits 7 (the admin is back)' 7 "$(exit_of isshoni setup-url)"
}

# ---------------------------------------------------------------------------------------------------------------
# Scenarios in containers of their own

# C7: another program holds a port isshoni needs. The run stops with exit 7 before the start and keeps the config;
# once the port is free, the next run finishes the job.
scenario_c7() {
  say '' 'C7: a port isshoni needs is taken (UDP 7882)'
  "$ENGINE" exec -d "$CONTAINER" python3 -c '
import os, socket, time
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.bind(("0.0.0.0", 7882))
open("/run/c7.pid.new", "w").write(str(os.getpid()))
os.rename("/run/c7.pid.new", "/run/c7.pid")
time.sleep(3600)'
  c7_tries=0
  while [ "$(yes_no test -s /run/c7.pid)" = no ]; do
    c7_tries=$((c7_tries + 1))
    [ "$c7_tries" -lt 50 ] || die 'the Python program that takes UDP 7882 did not start'
    sleep 0.1
  done

  run_installer "$V1" '' --yes --tls-mode off
  assert_eq 'C7 taken: exit code' 7 "$RC"
  assert_contains 'C7 taken: the message names the program' 'Port 7882/udp is used by python3' "$OUT"
  assert_eq 'C7 taken: the unit is not enabled' no "$(yes_no systemctl is-enabled isshoni)"
  assert_eq 'C7 taken: the config is kept' yes "$(yes_no test -s /etc/isshoni/isshoni.toml)"

  # shellcheck disable=SC2016 # the container shell expands it
  in_container sh -c 'kill "$(cat /run/c7.pid)"'
  c7_tries=0
  while [ -n "$(value ss -Hlun 'sport = :7882')" ]; do
    c7_tries=$((c7_tries + 1))
    [ "$c7_tries" -lt 50 ] || die 'UDP 7882 is still taken after the Python program was stopped'
    sleep 0.1
  done

  run_installer "$V1" '' --yes --tls-mode off
  assert_eq 'C7 free: exit code' 0 "$RC"
  assert_lacks 'C7 free: no question and no new config' 'Checking this server before the first start' "$OUT"
  assert_contains 'C7 free: the firewall step ran' 'Firewall:' "$OUT"
  assert_eq 'C7 free: the service is enabled' enabled "$(value systemctl is-enabled isshoni)"
  assert_eq 'C7 free: the service is active' active "$(value systemctl is-active isshoni)"
  assert_contains 'C7 free: the setup link' '/setup#' "$OUT"
}

# C13: a run whose server does not become ready ends with exit 5 and the unit enabled; once the admin has fixed the
# cause, the next run finishes the job. The cause here is a drop-in that makes every start fail.
scenario_c13() {
  say '' 'C13: the server does not start, then the cause is fixed'
  in_container mkdir -p /etc/systemd/system/isshoni.service.d
  in_container sh -c 'printf "[Service]\nExecStartPre=/bin/false\n" >/etc/systemd/system/isshoni.service.d/zz-test.conf'

  run_installer "$V1" '' --yes --tls-mode off
  assert_eq 'C13 failing: exit code' 5 "$RC"
  assert_eq 'C13 failing: the last line' 'Fix the problem above, then run this installer again.' \
    "$(printf '%s\n' "$OUT" | sed -n '$p')"
  assert_contains 'C13 failing: the log is shown' 'The last lines of its log (journalctl -u isshoni):' "$OUT"
  assert_eq 'C13 failing: the unit is enabled' enabled "$(value systemctl is-enabled isshoni)"

  in_container rm /etc/systemd/system/isshoni.service.d/zz-test.conf
  in_container systemctl daemon-reload
  run_installer "$V1" '' --yes --tls-mode off
  assert_eq 'C13 fixed: exit code' 0 "$RC"
  assert_lacks 'C13 fixed: no question and no new config' 'Checking this server before the first start' "$OUT"
  assert_eq 'C13 fixed: the service is active' active "$(value systemctl is-active isshoni)"
  assert_contains 'C13 fixed: the setup link' '/setup#' "$OUT"
}

# C11: a machine without systemd (a plain container of the distro) gets exit 4 and the way to Docker.
scenario_c11() {
  say '' "C11: no systemd ($1)"
  c11_name=$IMAGE_NAME-$2-c11-$$
  CONTAINERS="$CONTAINERS $c11_name"
  RC=0
  "$ENGINE" run --rm -i --name "$c11_name" --tmpfs /run -e ISSHONI_PUBLIC_URL=http://127.0.0.1:8080 "$1" \
    sh -s -- --yes --tls-mode off <"$WORK/root/download/v$V1/install.sh" >"$WORK/out" 2>&1 || RC=$?
  OUT=$(cat "$WORK/out")
  sed 's/^/  | /' "$WORK/out"
  assert_eq 'C11: exit code' 4 "$RC"
  assert_contains 'C11: the message' 'This system does not run systemd, which the installer needs.' "$OUT"
  assert_contains 'C11: the way to Docker' 'https://moonwx.github.io/isshoni/install/docker' "$OUT"
}

# V3 with firewalld (Fedora): the offer opens the ports in firewalld, right after isshoni.xml was put in place,
# which takes the reload before --add-service. The admin then puts the service into a second zone and a policy and
# names it in a rich rule, and also writes it into a third zone's file by hand, without a reload. --uninstall takes
# it out of all that the running firewalld knows; the file it does not know about keeps isshoni.xml, with a
# warning that names it. After the admin has done what the warning says, firewalld loads its configuration, at a
# reload and at a restart.
scenario_v3_firewalld() {
  say '' 'V3: firewalld running'
  v3_tries=0
  while [ "$(value firewall-cmd --state)" != running ]; do
    v3_tries=$((v3_tries + 1))
    [ "$v3_tries" -lt 60 ] || die 'firewalld did not start in the container'
    sleep 0.5
  done
  run_installer "$V1" '' --yes --tls-mode off
  assert_eq 'V3 firewalld install: exit code' 0 "$RC"
  assert_contains 'V3 firewalld install: the offer' 'Firewall: opened ports 80, 443 (TCP) and 7882 (TCP+UDP) in firewalld.' "$OUT"
  assert_eq 'V3 firewalld install: the service is open now' yes "$(yes_no firewall-cmd --query-service=isshoni)"
  assert_eq 'V3 firewalld install: and after a reload' yes "$(yes_no firewall-cmd --permanent --query-service=isshoni)"

  in_container sh -c '
    firewall-cmd --permanent --zone=internal --add-service=isshoni &&
      firewall-cmd --permanent --zone=trusted --add-rich-rule="rule family=\"ipv4\" source address=\"192.0.2.0/24\" service name=\"isshoni\" accept" &&
      firewall-cmd --permanent --new-policy=isshonitest &&
      firewall-cmd --permanent --policy=isshonitest --add-ingress-zone=internal &&
      firewall-cmd --permanent --policy=isshonitest --add-egress-zone=HOST &&
      firewall-cmd --permanent --policy=isshonitest --add-service=isshoni &&
      firewall-cmd --reload' >/dev/null || fail 'V3 firewalld: the admin puts the service into a zone, a policy and a rich rule'
  # shellcheck disable=SC2016 # the variables are the container shell's
  in_container sh -c 'sed "s|</zone>|  <service name=\"isshoni\"/>\n</zone>|" /usr/lib/firewalld/zones/work.xml >"$1"' \
    sh /etc/firewalld/zones/work.xml || fail 'V3 firewalld: the admin writes the service into the zone work by hand'

  run_installer "$V1" '' --uninstall
  assert_eq 'V3 firewalld uninstall: exit code' 0 "$RC"
  assert_contains 'V3 firewalld uninstall: the rich rule, printed in full' \
    "Firewall: removed this rule from firewalld's zone trusted: rule family=\"ipv4\" source address=\"192.0.2.0/24\" service name=\"isshoni\" accept" \
    "$OUT"
  v3_where=$(printf '%s\n' "$OUT" | sed -n 's/^Firewall: removed the isshoni service from firewalld (\(.*\))\.$/\1/p')
  for v3_label in 'zone public' 'zone internal' 'zone trusted' 'policy isshonitest'; do
    assert_contains "V3 firewalld uninstall: out of the $v3_label" "$v3_label" "$v3_where"
  done
  assert_contains 'V3 firewalld uninstall: the hand-written zone file is named' \
    'Could not take the isshoni service out of firewalld here: /etc/firewalld/zones/work.xml.' "$OUT"
  assert_eq 'V3 firewalld uninstall: so isshoni.xml stays' yes "$(yes_no test -e /etc/firewalld/services/isshoni.xml)"
  assert_eq 'V3 firewalld uninstall: firewall-cmd --reload succeeds with the file' 0 "$(exit_of firewall-cmd --reload)"
  in_container sh -c 'firewall-cmd --permanent --zone=work --remove-service=isshoni &&
    rm /etc/firewalld/services/isshoni.xml' >/dev/null || fail 'V3 firewalld: the admin does what the warning says'
  assert_eq 'V3 firewalld uninstall: no zone or policy names the service' '' \
    "$(in_container sh -c 'grep -l isshoni /etc/firewalld/zones/*.xml /etc/firewalld/policies/*.xml 2>/dev/null || :')"
  assert_eq 'V3 firewalld uninstall: firewall-cmd --reload succeeds' 0 "$(exit_of firewall-cmd --reload)"
  assert_eq 'V3 firewalld uninstall: firewalld runs' running "$(value firewall-cmd --state)"
  in_container systemctl restart firewalld
  assert_eq 'V3 firewalld uninstall: firewalld loads its configuration at a restart' running "$(value firewall-cmd --state)"
  assert_lacks 'V3 firewalld uninstall: the ports are closed' isshoni "$(value firewall-cmd --list-services)"
}

# V3 with ufw (Ubuntu): ufw enabled first, the offer adds its rule, --uninstall removes the rule and the profile.
scenario_v3_ufw() {
  say '' 'V3: ufw enabled'
  in_container ufw --force enable >/dev/null
  run_installer "$V1" '' --yes --tls-mode off
  assert_eq 'V3 ufw install: exit code' 0 "$RC"
  assert_contains 'V3 ufw install: the offer' 'Firewall: opened ports 80, 443 (TCP) and 7882 (TCP+UDP) in ufw.' "$OUT"
  assert_contains 'V3 ufw install: the rule' 'isshoni' "$(value ufw status)"

  run_installer "$V1" '' --uninstall
  assert_eq 'V3 ufw uninstall: exit code' 0 "$RC"
  assert_contains 'V3 ufw uninstall: the rule is removed' 'Firewall: removed the isshoni rule from ufw.' "$OUT"
  assert_lacks 'V3 ufw uninstall: no isshoni rule is left' 'isshoni' "$(value ufw status)"
  assert_contains 'V3 ufw uninstall: ufw is still on' 'Status: active' "$(value ufw status)"
  assert_eq 'V3 ufw uninstall: the profile is gone' no "$(yes_no test -e /etc/ufw/applications.d/isshoni)"
}

# ---------------------------------------------------------------------------------------------------------------

# wanted PART reports whether the part runs (-o).
wanted() {
  case " $PARTS " in *" $1 "*) return 0 ;; esac
  return 1
}

# run_distro DISTRO runs the parts in fresh containers of one distro.
run_distro() {
  rd_base=$(base_image "$1")
  rd_image=$IMAGE_NAME:$1
  say '' "==== $1 ($rd_base, $ENGINE)"
  build_image "$rd_image" "$rd_base"
  rd_arch=$("$ENGINE" image inspect -f '{{.Architecture}}' "$rd_image")
  case $rd_arch in
  amd64 | arm64) ;;
  *) die "the image $rd_image is for $rd_arch, and isshoni is built for amd64 and arm64" ;;
  esac

  if wanted main; then
    start_container "$1-main" "$rd_image"
    scenario_c12
    scenario_c1
    scenario_c2
    scenario_c3
    scenario_c6
    scenario_c4
    scenario_c5
    scenario_c8
    scenario_c9
    scenario_c10
    stop_container
  fi
  if wanted c7; then
    start_container "$1-c7" "$rd_image"
    scenario_c7
    stop_container
  fi
  if wanted c13; then
    start_container "$1-c13" "$rd_image"
    scenario_c13
    stop_container
  fi
  if wanted c11; then
    scenario_c11 "$rd_base" "$1"
  fi
  if wanted v3 && rd_fw=$(firewall_of "$1"); then
    build_image "$rd_image-firewall" "$rd_base" "$rd_fw"
    start_container "$1-v3" "$rd_image-firewall"
    "scenario_v3_$rd_fw"
    stop_container
  fi
}

main() {
  BUILD_ONLY=''
  RELEASES=''
  PARTS=$PARTS_ALL
  while getopts b:r:o:h m_opt; do
    case $m_opt in
    b) BUILD_ONLY=$OPTARG ;;
    r) RELEASES=$OPTARG ;;
    o) PARTS=$(printf '%s' "$OPTARG" | tr ',' ' ') ;;
    h)
      usage
      exit 0
      ;;
    *) die 'usage: distro-test.sh [-b DIR | -r DIR] [-o PARTS] [DISTRO...] (-h tells more)' ;;
    esac
  done
  shift $((OPTIND - 1))
  for m_part in $PARTS; do
    case " $PARTS_ALL " in
    *" $m_part "*) ;;
    *) die "unknown part \"$m_part\"" "known: $PARTS_ALL" ;;
    esac
  done

  if [ -n "$BUILD_ONLY" ]; then
    [ $# -eq 0 ] || die '-b builds the releases only and takes no distro'
    mkdir -p "$BUILD_ONLY"
    BUILD_ONLY=$(CDPATH='' cd -- "$BUILD_ONLY" && pwd)
    build_releases "$BUILD_ONLY"
    say "distro-test: the releases $V1 and $V2 are in $BUILD_ONLY"
    return 0
  fi

  # shellcheck disable=SC2086 # the list of names
  [ $# -gt 0 ] || set -- $DISTROS
  for m_distro in "$@"; do
    base_image "$m_distro" >/dev/null || die "unknown distro \"$m_distro\"" "known: $DISTROS"
  done
  for m_tool in go ssh-keygen tar awk "$ENGINE"; do
    command -v "$m_tool" >/dev/null 2>&1 || die "\"$m_tool\" is needed and not installed"
  done
  if [ -n "$RELEASES" ]; then
    RELEASES=$(CDPATH='' cd -- "$RELEASES" && pwd) || die "-r: there is no directory $RELEASES"
  fi

  WORK=$(mktemp -d)
  trap cleanup EXIT
  trap 'exit 1' INT TERM
  mkdir -p "$WORK/root"
  if [ -z "$RELEASES" ]; then
    RELEASES=$WORK/releases
    build_releases "$RELEASES"
  fi
  ssh-keygen -q -t ed25519 -N '' -C isshoni-distro-test -f "$WORK/key" >/dev/null
  prepare_releases "$RELEASES"
  start_relserve

  for m_distro in "$@"; do
    run_distro "$m_distro"
  done

  say '' "distro-test: $PASSED passed, $FAILED failed"
  [ "$FAILED" -eq 0 ]
}

main "$@"
