#!/bin/sh
# The Docker smoke test (docs/m1/06-deploy-and-ci.md §11.3; `task docker:smoke`, CI job docker-smoke).
#
# It builds the image from this working tree (docker-build.sh) and starts it the way the docs tell people to:
#
#   image     the HEALTHCHECK, the user and the version of the image
#   novolume  `docker run IMAGE` without a data volume exits 78 and says how to fix it
#   bridge    deploy/compose.yaml with a throwaway certificate (manual TLS mode, compose.smoke.yaml): healthy, HTTPS on
#             443, setup-url through `docker compose exec`, the admin survives `down` and `up`, a read-only root
#             without capabilities
#   backup    in bridge and host: the backup and restore commands of 06 §6.4, through `exec -T`
#   ipv6      in bridge: a client on the IPv6 loopback is seen with the address of Docker's proxy (06 §6.4)
#   host      deploy/compose.host.yaml (compose.smoke.host.yaml): the same, and the container's process owns the
#             host's port 443
#   noenv     deploy/compose.yaml without a .env file (compose.smoke.noenv.yaml): empty ISSHONI_* values count as
#             unset, and the container keeps running
#
# Usage: docker-smoke.sh [-a ARCH] [-i IMAGE] [-k] [-s PARTS] [-v VERSION]
#   -a ARCH     build for amd64 or arm64 (default: the architecture of the Docker engine)
#   -i IMAGE    test this image instead of building one (a release image, for example)
#   -k          keep what the test made (the last deployment, the image, the work directory) for a look around
#   -s PARTS    skip parts, comma-separated: novolume, bridge, backup, ipv6, host, noenv
#   -v VERSION  version of the binary to build (default: the version web/dist was built for)
#
# The test uses the host's ports 80, 443 and 7882 and the compose project "isshoni-smoke"; a project of that name
# left by an earlier run is removed first. It never touches a project named "isshoni", the name of a real install.
#
# Where the Docker engine runs in a virtual machine (Docker Desktop on macOS and Windows), the host network of
# compose.host.yaml is the machine's, not this computer's. The checks that need to reach it from here are skipped
# with a note; CI runs them on Linux.
#
# Needs docker with compose and buildx, curl, openssl and jq (and go and git to build). POSIX sh.

set -eu

PROJECT=isshoni-smoke
SITE=https://smoke.test
DATA_DIR=/var/lib/isshoni

say() { printf '%s\n' "$*"; }

ok() {
  passed=$((passed + 1))
  printf '  ok: %s\n' "$*"
}

skip() {
  skipped="$skipped
  - $*"
  printf '  SKIPPED: %s\n' "$*"
}

fail() {
  printf 'docker-smoke: FAILED: %s\n' "$*" >&2
  exit 1
}

usage() {
  sed -n '/^# Usage:/,/^#$/p' "$0" | sed -e '$d' -e 's/^# \{0,1\}//'
}

# skipping PART reports whether -s names the part.
skipping() {
  case ",$skip_parts," in
  *",$1,"*) return 0 ;;
  esac
  return 1
}

now() { date +%s; }

# mode_of FILE prints the permission bits in octal (GNU and busybox stat, then BSD stat).
mode_of() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }

# as_root CMD... runs CMD as root: directly, or through sudo when sudo needs no password. It fails when neither
# works.
as_root() {
  if [ "$(id -u)" -eq 0 ]; then
    "$@"
  else
    sudo -n "$@"
  fi
}

# compose ARGS... runs docker compose for the test's project from an empty project directory, so that no .env file
# is ever read.
compose() {
  (cd "$work/project" && docker compose -p "$PROJECT" --project-directory "$work/project" "$@")
}

# dc ARGS... is compose with the files of the deployment under test ($mode).
dc() {
  case $mode in
  bridge) compose -f "$root/deploy/compose.yaml" -f "$here/compose.smoke.yaml" "$@" ;;
  host) compose -f "$root/deploy/compose.host.yaml" -f "$here/compose.smoke.host.yaml" "$@" ;;
  noenv) compose -f "$root/deploy/compose.yaml" -f "$here/compose.smoke.noenv.yaml" "$@" ;;
  *) fail "dc: no deployment is up" ;;
  esac
}

# ctl ARGS... runs the isshoni CLI in the running container, as the docs do: docker compose exec -T isshoni isshoni.
ctl() { dc exec -T isshoni isshoni "$@" </dev/null; }

# api ADDRESS CURL-ARGS... runs curl against the test site on ADDRESS (127.0.0.1 or [::1]), trusting the test CA.
api() {
  address=$1
  shift
  curl -sS --max-time 20 --cacert "$work/tls/ca.pem" --resolve "smoke.test:443:$address" "$@"
}

container_id() { dc ps -a -q isshoni 2>/dev/null | head -n 1; }

# show_logs prints the end of the container's log, for a failure.
show_logs() {
  say "docker-smoke: the container's last log lines ($mode):"
  dc logs --no-color --no-log-prefix --tail 60 isshoni 2>&1 | sed 's/^/    /' || true
}

# up starts the deployment under test.
up() {
  if ! dc up -d --quiet-pull >"$work/up.log" 2>&1; then
    sed 's/^/    /' "$work/up.log" >&2
    fail "docker compose up failed ($mode). The test needs the host's ports 80, 443 and 7882."
  fi
}

# down stops the deployment under test and removes its containers and network; down -v also removes the volume.
down() {
  dc down --timeout 20 "$@" >"$work/down.log" 2>&1 || {
    sed 's/^/    /' "$work/down.log" >&2
    fail "docker compose down failed ($mode)"
  }
}

# wait_healthy SECONDS waits until the container runs, Docker's HEALTHCHECK says healthy and `isshoni healthcheck`
# agrees. Docker keeps the last health status while a server restarts in place, so the CLI is asked too.
wait_healthy() {
  deadline=$(($(now) + $1))
  state=unknown
  while :; do
    cid=$(container_id)
    if [ -n "$cid" ]; then
      state=$(docker inspect -f '{{.State.Status}}/{{if .State.Health}}{{.State.Health.Status}}{{end}}' "$cid" 2>/dev/null) || state=unknown
      if [ "$state" = running/healthy ] && ctl healthcheck >/dev/null 2>&1; then
        return 0
      fi
    fi
    [ "$(now)" -lt "$deadline" ] || fail "the container is not healthy after $1 s (state: $state)"
    sleep 1
  done
}

# check_info ADDRESS checks that GET /api/v1/info answers 200 with the image's version.
check_info() {
  status=$(api "$1" -o "$work/info.json" -w '%{http_code}' "$SITE/api/v1/info") ||
    fail "GET $SITE/api/v1/info on $1 failed"
  [ "$status" = 200 ] || fail "GET /api/v1/info on $1 answered $status, want 200"
  got=$(jq -r '.server.version' "$work/info.json")
  [ "$got" = "$version" ] || fail "/api/v1/info says version $got, the image is $version"
  ok "https://smoke.test/api/v1/info on $1 answers 200 with version $version"
}

# check_setup_url checks that setup-url prints a link to the test site. The token is never printed.
check_setup_url() {
  link=$(ctl setup-url 2>"$work/setup-url.err") || {
    cat "$work/setup-url.err" >&2
    fail "isshoni setup-url failed"
  }
  case $link in
  "$SITE/setup#"?*) ok "docker compose exec -T isshoni isshoni setup-url prints $SITE/setup#..." ;;
  *) fail "setup-url printed something else than a $SITE/setup#... link (${link%%#*})" ;;
  esac
}

# seed_admin ADDRESS creates the admin account over REST with a fresh setup link (03 §7.8). The password is random
# and stays in the work directory.
seed_admin() {
  link=$(ctl setup-url 2>/dev/null) || fail "isshoni setup-url failed"
  openssl rand -hex 16 >"$work/password"
  printf '{"token":"%s","username":"smokeadmin","password":"%s"}' "${link#*#}" "$(cat "$work/password")" >"$work/setup.json"
  status=$(api "$1" -o "$work/setup.out" -w '%{http_code}' -H 'Content-Type: application/json' \
    --data-binary "@$work/setup.json" "$SITE/api/v1/auth/setup/complete") || fail "POST /api/v1/auth/setup/complete failed"
  rm -f "$work/setup.json"
  [ "$status" = 201 ] || fail "POST /api/v1/auth/setup/complete answered $status: $(cat "$work/setup.out")"
}

# check_admin_exists WHEN checks that setup-url is refused with exit 7: an admin account exists.
check_admin_exists() {
  status=0
  ctl setup-url >/dev/null 2>&1 || status=$?
  [ "$status" -eq 7 ] || fail "setup-url exits $status $1, want 7 (an admin account exists)"
  ok "the admin account is there $1 (setup-url exits 7)"
}

# check_backup_restore runs the backup and restore commands of 06 §6.4 as they are documented.
check_backup_restore() {
  if skipping backup; then
    skip "backup and restore ($mode): -s backup"
    return 0
  fi
  archive=$work/backup-$mode.tar.gz
  (
    umask 077
    dc exec -T isshoni isshoni admin backup --out - >"$archive"
  ) </dev/null || fail "isshoni admin backup --out - failed"
  [ -s "$archive" ] || fail "the backup archive is empty"
  perms=$(mode_of "$archive")
  [ "$perms" = 600 ] || fail "the backup archive has mode $perms, want 600"
  ok "backup through exec -T under umask 077 writes a 0600 archive"

  dc exec -T isshoni isshoni admin restore --yes - <"$archive" || fail "isshoni admin restore --yes - failed"
  wait_healthy 60
  ok "restore through exec -T succeeds, and the server is healthy again"
  check_admin_exists "after the restore"
}

# check_ipv6_client checks what 06 §6.4 warns about: in bridge mode Docker's proxy forwards a client on the IPv6
# loopback from the compose network's gateway, so the server never sees the client's own address. When a Docker
# update changes that, this fails and 06 §6.4 needs an update.
check_ipv6_client() {
  if skipping ipv6; then
    skip "the client address over IPv6: -s ipv6"
    return 0
  fi
  if ! api '[::1]' -o /dev/null "$SITE/api/v1/info" 2>"$work/ipv6.err"; then
    skip "the client address over IPv6: port 443 does not answer on [::1] ($(tr '\n' ' ' <"$work/ipv6.err"))"
    return 0
  fi
  printf '{"username":"smokeadmin","password":"%s"}' "$(cat "$work/password")" >"$work/login.json"
  status=$(api '[::1]' -o /dev/null -w '%{http_code}' -c "$work/cookies" -H 'Content-Type: application/json' \
    --data-binary "@$work/login.json" "$SITE/api/v1/auth/login") || fail "POST /api/v1/auth/login on [::1] failed"
  rm -f "$work/login.json"
  [ "$status" = 200 ] || fail "POST /api/v1/auth/login on [::1] answered $status, want 200"
  seen=$(api '[::1]' -b "$work/cookies" "$SITE/api/v1/me/sessions" | jq -r '.sessions[] | select(.current) | .lastIp')
  rm -f "$work/cookies"
  gateways=$(docker network inspect "${PROJECT}_default" --format '{{range .IPAM.Config}}{{.Gateway}} {{end}}')
  case " $gateways " in
  *" $seen "*)
    ok "a client on [::1] is seen as $seen, the gateway of the compose network"
    return 0
    ;;
  esac
  case $seen in
  ::1 | '')
    fail "a client on [::1] is seen as '$seen', not as the compose network's gateway ($gateways): update 06 §6.4"
    ;;
  esac
  [ "$engine_local" = 0 ] ||
    fail "a client on [::1] is seen as $seen, not as the compose network's gateway ($gateways): update 06 §6.4"
  # A Docker engine in a virtual machine forwards published ports from the machine's own address.
  ok "a client on [::1] is seen as $seen, the address of the Docker machine's port forwarding"
}

# check_hardening checks the container's settings that the compose files promise.
check_hardening() {
  cid=$(container_id)
  got=$(docker inspect -f '{{.HostConfig.ReadonlyRootfs}} {{json .HostConfig.CapDrop}}' "$cid")
  [ "$got" = 'true ["ALL"]' ] || fail "ReadonlyRootfs and CapDrop are '$got', want 'true [\"ALL\"]'"
  ok "ReadonlyRootfs is true and CapDrop is [ALL]"
}

test_image() {
  say "docker-smoke: image $image"
  got=$(docker image inspect -f '{{json .Config.Healthcheck.Test}}' "$image")
  [ "$got" = '["CMD","/usr/local/bin/isshoni","healthcheck"]' ] ||
    fail "the image's HEALTHCHECK is $got, want CMD /usr/local/bin/isshoni healthcheck"
  ok "HEALTHCHECK is /usr/local/bin/isshoni healthcheck"
  got=$(docker image inspect -f '{{.Config.User}}' "$image")
  [ "$got" = 65532:65532 ] || fail "the image's user is '$got', want 65532:65532"
  ok "the image runs as 65532:65532"
  version=$(docker run --rm "$image" version --short) || fail "'docker run $image version --short' failed"
  [ -n "$version" ] || fail "'isshoni version --short' printed nothing"
  [ -z "$want_version" ] || [ "$version" = "$want_version" ] ||
    fail "the image's binary is version $version, built as $want_version"
  ok "isshoni version --short prints $version"
}

test_novolume() {
  if skipping novolume; then
    skip "no data volume: -s novolume"
    return 0
  fi
  say "docker-smoke: no data volume"
  name=$PROJECT-novolume
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker run -d --name "$name" "$image" >/dev/null
  deadline=$(($(now) + 10))
  while [ "$(docker inspect -f '{{.State.Status}}' "$name")" != exited ]; do
    if [ "$(now)" -ge "$deadline" ]; then
      docker rm -f "$name" >/dev/null 2>&1 || true
      fail "without a data volume the container still runs after 10 s, want exit 78"
    fi
    sleep 1
  done
  status=$(docker inspect -f '{{.State.ExitCode}}' "$name")
  docker logs "$name" >"$work/novolume.log" 2>&1
  docker rm -f "$name" >/dev/null 2>&1 || true
  [ "$status" = 78 ] || {
    sed 's/^/    /' "$work/novolume.log" >&2
    fail "without a data volume the container exits $status, want 78"
  }
  grep -q "Mount a volume at $DATA_DIR (see compose.yaml)" "$work/novolume.log" || {
    sed 's/^/    /' "$work/novolume.log" >&2
    fail "without a data volume the log lacks the fix 'Mount a volume at $DATA_DIR (see compose.yaml)'"
  }
  ok "docker run without a volume exits 78 and says: Mount a volume at $DATA_DIR (see compose.yaml)"
}

# make_certificate makes a throwaway CA and a certificate for smoke.test in $work/tls. The container's users read
# the files through a bind mount, so they are world-readable; the keys are worth nothing.
make_certificate() {
  tls=$work/tls
  mkdir -p "$tls"
  {
    openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 2 \
      -subj '/CN=isshoni smoke test CA' -keyout "$tls/ca.key" -out "$tls/ca.pem" \
      -addext 'basicConstraints=critical,CA:TRUE' -addext 'keyUsage=critical,keyCertSign'
    openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
      -subj '/CN=smoke.test' -keyout "$tls/key.pem" -out "$tls/cert.csr"
    printf 'subjectAltName=DNS:smoke.test\nbasicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=serverAuth\n' >"$tls/cert.ext"
    openssl x509 -req -in "$tls/cert.csr" -CA "$tls/ca.pem" -CAkey "$tls/ca.key" -CAcreateserial \
      -days 2 -extfile "$tls/cert.ext" -out "$tls/cert.pem"
  } >"$work/openssl.log" 2>&1 || {
    sed 's/^/    /' "$work/openssl.log" >&2
    fail "openssl could not make the test certificate"
  }
  chmod 0755 "$tls"
  chmod 0644 "$tls/cert.pem" "$tls/key.pem"
}

test_bridge() {
  if skipping bridge; then
    skip "bridge networking: -s bridge"
    return 0
  fi
  say "docker-smoke: bridge networking (deploy/compose.yaml)"
  mode=bridge
  up
  wait_healthy 90
  ok "the container is healthy"
  check_info 127.0.0.1
  check_setup_url
  check_hardening

  seed_admin 127.0.0.1
  down
  up
  wait_healthy 90
  check_admin_exists "after docker compose down and up"
  check_backup_restore
  check_ipv6_client

  down -v
  mode=''
}

test_host() {
  if skipping host; then
    skip "host networking: -s host"
    return 0
  fi
  say "docker-smoke: host networking (deploy/compose.host.yaml)"
  mode=host
  up
  wait_healthy 90
  ok "the container is healthy"
  check_setup_url
  check_hardening
  cid=$(container_id)
  got=$(docker inspect -f '{{.HostConfig.NetworkMode}} {{.Config.User}} {{json .HostConfig.CapAdd}}' "$cid")
  case $got in
  'host 0:0 ["NET_BIND_SERVICE"]' | 'host 0:0 ["CAP_NET_BIND_SERVICE"]') ;;
  *) fail "NetworkMode, user and CapAdd are '$got', want 'host 0:0 [\"NET_BIND_SERVICE\"]'" ;;
  esac
  ok "host network, user 0:0, and NET_BIND_SERVICE is the only capability"

  if [ "$engine_local" = 1 ]; then
    check_info 127.0.0.1
    check_port_owner "$cid"
    seed_admin 127.0.0.1
    down
    up
    wait_healthy 90
    check_admin_exists "after docker compose down and up"
    check_backup_restore
  else
    # The data directory is a bind mount, so its files show whether the second start found the first one's data.
    before=$(cksum <"$SMOKE_HOST_DATA/secrets.json") || fail "the data directory has no secrets.json"
    down
    up
    wait_healthy 90
    after=$(cksum <"$SMOKE_HOST_DATA/secrets.json") || fail "the data directory has no secrets.json"
    [ "$before" = "$after" ] || fail "secrets.json changed over docker compose down and up: the data was lost"
    ok "the data directory is kept over docker compose down and up"
    skip "host networking from outside (HTTPS on 127.0.0.1:443, the owner of port 443, the admin account, backup): the Docker engine runs in a virtual machine, whose network this computer can't reach"
  fi

  down -v
  mode=''
}

# check_port_owner CONTAINER checks that the listener on the host's port 443 belongs to the container's process.
check_port_owner() {
  if ! command -v ss >/dev/null 2>&1; then
    skip "the owner of port 443: ss (iproute2) is not installed"
    return 0
  fi
  if ! listeners=$(as_root ss -H -ltnp 'sport = :443' 2>/dev/null); then
    skip "the owner of port 443: ss needs root to name the process, and sudo asks for a password"
    return 0
  fi
  pid=$(docker inspect -f '{{.State.Pid}}' "$1")
  case $listeners in
  *"pid=$pid,"*) ok "ss -ltnp: port 443 of the host belongs to the container's process (pid $pid)" ;;
  *) fail "ss -ltnp does not show the container's process (pid $pid) on port 443: $listeners" ;;
  esac
}

test_noenv() {
  if skipping noenv; then
    skip "no .env file: -s noenv"
    return 0
  fi
  say "docker-smoke: no .env file (deploy/compose.yaml)"
  mode=noenv
  up
  wait_healthy 90
  cid=$(container_id)
  env=$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$cid")
  for name in ISSHONI_DOMAIN ISSHONI_PUBLIC_IP; do
    printf '%s\n' "$env" | grep -qx "$name=" || fail "$name is not an empty variable in the container"
  done
  ok "ISSHONI_DOMAIN and ISSHONI_PUBLIC_IP reach the container as empty strings"
  restarts=$(docker inspect -f '{{.RestartCount}}' "$cid")
  [ "$restarts" = 0 ] || fail "the server exited and Docker restarted it $restarts time(s)"
  ok "the container is healthy and was not restarted"
  ctl config print --json >"$work/config.json" 2>"$work/config.err" || {
    cat "$work/config.err" >&2
    fail "isshoni config print --json failed"
  }
  got=$(jq -r '[.tlsMode, (.keys[] | select(.key == "domain" or .key == "public_ip") | .key + "=" + .source.kind)] | join(" ")' "$work/config.json")
  [ "$got" = 'ip domain=default public_ip=default' ] ||
    fail "config print shows '$got', want 'ip domain=default public_ip=default'"
  ok "config print: domain and public_ip come from the defaults, and the TLS mode is ip"
  down -v
  mode=''
}

cleanup() {
  status=$?
  trap - EXIT
  set +e
  if [ "$status" -ne 0 ] && [ -n "$mode" ]; then
    show_logs
  fi
  if [ "$keep" = 1 ]; then
    say "docker-smoke: -k: kept the image $image and the work directory $work"
    [ -z "$mode" ] || say "docker-smoke: -k: kept the deployment; remove it with: docker compose -p $PROJECT down -v"
    exit "$status"
  fi
  docker rm -f "$PROJECT-novolume" >/dev/null 2>&1
  docker compose -p "$PROJECT" down -v --remove-orphans --timeout 5 >/dev/null 2>&1
  [ "$built" = 0 ] || docker image rm "$image" >/dev/null 2>&1
  # In host mode on Linux the data directory belongs to root.
  if [ -n "$work" ] && ! rm -rf "$work" 2>/dev/null && ! as_root rm -rf "$work" 2>/dev/null; then
    say "docker-smoke: could not remove $work (its data directory belongs to root): sudo rm -rf $work"
  fi
  exit "$status"
}

main() {
  arch='' image='' keep=0 skip_parts='' want_version=''
  while getopts a:i:ks:v:h opt; do
    case $opt in
    a) arch=$OPTARG ;;
    i) image=$OPTARG ;;
    k) keep=1 ;;
    s) skip_parts=$OPTARG ;;
    v) want_version=$OPTARG ;;
    h)
      usage
      exit 0
      ;;
    *)
      usage >&2
      exit 2
      ;;
    esac
  done
  shift $((OPTIND - 1))
  if [ $# -ne 0 ]; then
    usage >&2
    exit 2
  fi
  for part in $(printf '%s' "$skip_parts" | tr ',' ' '); do
    case $part in
    novolume | bridge | backup | ipv6 | host | noenv) ;;
    *)
      printf 'docker-smoke: -s %s: the parts are novolume, bridge, backup, ipv6, host and noenv\n' "$part" >&2
      exit 2
      ;;
    esac
  done

  # The compose files read ISSHONI_* variables, and CI sets ISSHONI_VERSION for its builds: the test starts from
  # none (06 §8.2).
  for name in $(env | sed -n 's/^\(ISSHONI_[A-Za-z0-9_]*\)=.*/\1/p'); do
    unset "$name"
  done

  here=$(cd "$(dirname "$0")" && pwd)
  root=$here
  while [ ! -f "$root/go.mod" ] || [ ! -f "$root/Taskfile.yml" ]; do
    [ "$root" != / ] || fail "can't find the repository root above $here"
    root=$(dirname "$root")
  done
  for tool in docker curl openssl jq; do
    command -v "$tool" >/dev/null || fail "$tool is not installed"
  done
  docker compose version >/dev/null 2>&1 || fail "docker compose (the Compose plugin) is not installed"
  kernel=$(docker info --format '{{.KernelVersion}}') || fail "can't reach the Docker engine: is Docker running?"
  # The engine is local when it runs on this kernel; otherwise it runs in a virtual machine or on another host.
  engine_local=0
  [ "$(uname -r)" != "$kernel" ] || engine_local=1

  passed=0 skipped='' mode='' built=0 version=''
  work=$(mktemp -d "${TMPDIR:-/tmp}/isshoni-smoke.XXXXXX")
  # The real path: Docker Desktop shares directories by their real names (/private/var/... on macOS).
  work=$(cd "$work" && pwd -P)
  trap cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  mkdir "$work/project"
  docker compose -p "$PROJECT" down -v --remove-orphans --timeout 5 >/dev/null 2>&1 || true

  if [ -z "$image" ]; then
    image=isshoni:smoke
    built=1
    sh "$here/docker-build.sh" -t "$image" ${arch:+-a "$arch"} ${want_version:+-v "$want_version"} ||
      fail "docker-build.sh failed"
  fi
  docker image inspect "$image" >/dev/null 2>&1 || fail "Docker has no image $image: pull or build it first"
  SMOKE_IMAGE=$image
  SMOKE_TLS_DIR=$work/tls
  SMOKE_HOST_DATA=$work/host-data
  export SMOKE_IMAGE SMOKE_TLS_DIR SMOKE_HOST_DATA

  test_image
  test_novolume
  make_certificate
  test_bridge
  test_host
  test_noenv

  if [ -n "$skipped" ]; then
    say "docker-smoke: passed, $passed checks. Skipped:$skipped"
  else
    say "docker-smoke: passed, $passed checks"
  fi
}

main "$@"
