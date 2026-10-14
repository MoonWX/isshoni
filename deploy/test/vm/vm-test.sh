#!/bin/sh
# The install.sh scenarios in virtual machines: the nightly's distro-vm job, and the run by hand before a release
# (docs/m1/06-deploy-and-ci.md §4.13, §8.5, §9.1 and §11.2, "06" below).
#
#   sh deploy/test/vm/vm-test.sh [-r DIR] [-o PARTS] [DISTRO...]
#
# DISTRO is one of the supported distros (06 §4.13): debian-12, debian-13, ubuntu-22.04, ubuntu-24.04,
# ubuntu-26.04, fedora-44. Without one, all of them run, one after the other. -r DIR takes the two test releases
# from DIR, which `sh deploy/test/distro-test.sh -b DIR` builds (the nightly's vm-release job does); without -r they
# are built first, as distro-test.sh builds them (Task and web/node_modules are needed then).
#
# Each part runs in a VM of its own, booted fresh with QEMU from the distro's official cloud image
# (deploy/test/vm/cloud-image.sh) and a cloud-init seed that lets root in with a key made for the run, and reached
# over SSH. The scenarios are deploy/test/distro-test.sh's: this script sources that file with DISTRO_TEST_LIB=1,
# which keeps its main from running, and puts vm_engine, which runs a command in the VM the way `docker exec` runs
# it in a container, in the place of the container engine. The test releases' signing and relserve are the
# library's too. The VM-only scenarios of 06 §11.2 are below. The parts (-o, default all, each where it applies):
#   main  C12, C1, then V1's sysctl values, C2, C3, C6, C4, C5, C8, C9, C10, then V6 and, on Fedora, V2
#   c7    C7, a port taken by another program
#   c13   C13, a server that does not start
#   v3    V3 (fedora-44 and ubuntu-24.04): the firewall offer with firewalld or ufw running; SSH still gets in
#   v4    V4 (ubuntu-24.04): iptables with netfilter-persistent and a REJECT rule, IPv4 and IPv6; the rules
#         survive a reboot
#   v5    V5: --domain isshoni.test against Pebble on this machine, ready within 60 s, a certificate from Pebble
# C11 (no systemd) has no VM form: it stays in distro-test.sh's container runs. A part is skipped with a note on a
# distro it does not apply to, and its image is fetched only when a part runs there; a run in which no part applies
# fails. Before anything else the script checks that distro-test.sh still has every function and variable it takes
# from it or replaces, so `vm-test.sh -h` is a check of that which needs no VM, no Linux and no network.
#
# Where it runs: Linux on x86_64 or aarch64, with QEMU (qemu-system-x86, or qemu-system-arm and qemu-efi-aarch64;
# qemu-utils), cloud-image-utils, OpenSSH, OpenSSL, curl, Go, and for v5 Docker (Pebble and pebble-challtestsrv run
# in containers on the host network, as in ci.yml's acme job). The nightly runs it on GitHub's ubuntu-24.04 runners
# with KVM. By hand, on a Mac: inside a Linux VM (UTM or Tart); without nested virtualization there is no
# /dev/kvm, and VM_ACCEL=tcg emulates the VMs instead, many times slower.
#
# Nothing is installed on this machine. The cloud images stay in VM_DIR/images/<distro>.qcow2 for the next run
# (delete them to get the distros' current builds); each VM's console log stays in VM_DIR/<distro>-<part>/, and
# Pebble's and challtestsrv's logs in VM_DIR/pebble.log and VM_DIR/challtestsrv.log. The VMs, their disks, the
# Pebble containers, the key and the releases are deleted when the script ends.
#
# Environment:
#   VM_DIR            the cloud images, and each VM's disk and console log (default: .dev/vm in the checkout)
#   VM_ACCEL          kvm (the default, which needs a usable /dev/kvm) or tcg
#   VM_BOOT_TIMEOUT   seconds a VM has to answer on SSH and to finish booting (default 300; cloud-init gets twice
#                     that)
#   CONTAINER_ENGINE  what runs Pebble for v5 (default docker)
#   KEEP=1            keep the work directory (key, releases, relserve's CA) and the VMs' disks
set -eu

VT_ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../../.." && pwd)
VT_ENGINE=${CONTAINER_ENGINE:-docker}

# vt_lib_fail WHAT stops the script before anything has started: distro-test.sh, WHAT. Until the checks below have
# passed, die is the library's or missing.
vt_lib_fail() {
  printf 'vm-test: deploy/test/distro-test.sh %s\n' "$1" >&2
  [ "${GITHUB_ACTIONS-}" != true ] || printf '::error::vm-test: deploy/test/distro-test.sh %s\n' "$1"
  exit 1
}

# The library. Without its guard its main would run here, with this script's arguments, and test containers in
# place of VMs. It sets ROOT from $0, which is this script here, and ENGINE from the machine: both are set again
# below. The variables this script takes from it are unset first, so that only the library can set them (KEEP is
# the caller's, as it is there).
# shellcheck disable=SC2016 # the text of the guard
grep -q '"${DISTRO_TEST_LIB-}" != 1' "$VT_ROOT/deploy/test/distro-test.sh" ||
  vt_lib_fail 'no longer skips its main when DISTRO_TEST_LIB=1'
unset ROOT ENGINE DISTROS V1 HOST_NAME WORK CONTAINER BASE_URL RC OUT PASSED FAILED
DISTRO_TEST_LIB=1
# shellcheck source=deploy/test/distro-test.sh
. "$VT_ROOT/deploy/test/distro-test.sh"

# What this script takes from distro-test.sh, and what it replaces there: the functions die and assert_eq, the
# variables ROOT and ENGINE, and in_container's call of "$ENGINE" exec "$CONTAINER", which vm_engine answers. A name
# that is renamed or removed there fails here, before a release is built or a VM booted. A function counts only when
# the shell has it as a function: `command -v` prints a path for a program of the same name (macOS has a `say`).
for vt_name in die assert_eq say usage ok fail assert_contains assert_lacks base_image firewall_of in_container \
  value yes_no exit_of json_of wait_ready cleanup build_releases prepare_releases start_relserve run_installer \
  scenario_c1 scenario_c2 scenario_c3 scenario_c4 scenario_c5 scenario_c6 scenario_c7 scenario_c8 scenario_c9 \
  scenario_c10 scenario_c12 scenario_c13 scenario_v3_firewalld scenario_v3_ufw; do
  case $(command -v "$vt_name" 2>/dev/null || echo /) in
  */*) vt_lib_fail "has no function $vt_name, which deploy/test/vm/vm-test.sh uses or replaces" ;;
  esac
done
for vt_name in ROOT ENGINE DISTROS V1 HOST_NAME KEEP WORK CONTAINER BASE_URL RC OUT PASSED FAILED; do
  eval "[ \"\${$vt_name+set}\" = set ]" ||
    vt_lib_fail "does not set $vt_name, which deploy/test/vm/vm-test.sh uses or replaces"
done
# in_container's call, with a stand-in for the engine that only records it.
vt_probe() {
  VT_PROBED="$*"
}
VT_PROBED='' ENGINE=vt_probe CONTAINER=vt-probe
in_container true >/dev/null 2>&1 || :
[ "$VT_PROBED" = 'exec vt-probe true' ] ||
  vt_lib_fail "no longer runs in_container's command as \"\$ENGINE\" exec \"\$CONTAINER\", which vm_engine answers"

ROOT=$VT_ROOT
ENGINE=vm_engine
CONTAINER=''

die() {
  printf 'vm-test: %s\n' "$@" >&2
  [ "${GITHUB_ACTIONS-}" != true ] || printf '::error::vm-test: %s\n' "$1"
  exit 1
}

VM_DIR=${VM_DIR:-$ROOT/.dev/vm}
VM_BOOT_TIMEOUT=${VM_BOOT_TIMEOUT:-300}
VM_ACCEL=${VM_ACCEL:-kvm}
VM_PARTS_ALL='main c7 c13 v3 v4 v5'
VM_SSH_PORT=2200 # each VM gets the next port for its SSH
VM_STARTED=''    # the VMs this run started, for the cleanup (VM_DIR may hold an earlier run's)
VM_ENV=''        # NAME=VALUE words that vm_engine adds to every command (V5's ACME server)
VM_PEBBLE=0      # 1 once this run has started Pebble
PEBBLE_MINICA='' # the CA of Pebble's own HTTPS endpoints, once Pebble runs

# Pebble v2.10.1 and its challtestsrv, both images by the digest of their multi-arch index, as ci.yml's acme job
# pins them (bump both places together).
VM_PEBBLE_IMAGE=ghcr.io/letsencrypt/pebble@sha256:ddf230642b1a584f519f32e347de1b05a6e4c1f6c35c1863b33effeab5f78199
VM_CHALLTESTSRV_IMAGE=ghcr.io/letsencrypt/pebble-challtestsrv@sha256:12ce21884def456bcf9786542113949e1f19dc7738d2c70e156c2d0c38a1405b

# ---------------------------------------------------------------------------------------------------------------
# Talking to a VM

# sh_quote WORD prints WORD in single quotes, for the VM's shell to read back as the same word.
sh_quote() {
  sq_q="'" sq_rest=$1 sq_out=''
  while :; do
    case $sq_rest in
    *"$sq_q"*)
      sq_out=$sq_out${sq_rest%%"$sq_q"*}$sq_q\\$sq_q$sq_q
      sq_rest=${sq_rest#*"$sq_q"}
      ;;
    *) break ;;
    esac
  done
  printf '%s%s%s%s' "$sq_q" "$sq_out" "$sq_rest" "$sq_q"
}

# vm_ssh VM COMMAND runs a command line in the VM's shell as root, through the VM's SSH master connection when it
# has one. stdin goes to the command.
vm_ssh() {
  vh_dir=$VM_DIR/$1
  vh_ctl=$WORK/$1.ctl
  shift
  ssh -F "$WORK/ssh_config" -p "$(cat "$vh_dir/port")" -o "ControlPath=$vh_ctl" root@127.0.0.1 "$@"
}

# vm_ssh_new VM COMMAND is vm_ssh over a connection of its own, as a new client sees the VM.
vm_ssh_new() {
  vn_dir=$VM_DIR/$1
  shift
  ssh -F "$WORK/ssh_config" -p "$(cat "$vn_dir/port")" -o ControlPath=none root@127.0.0.1 "$@"
}

# vm_master VM start|exit opens the SSH connection that the VM's commands share, or closes it. The control socket
# is in the work directory, whose path is short enough for a socket's name.
vm_master() {
  vm_m_ctl=$WORK/$1.ctl
  if [ "$2" = start ]; then
    ssh -F "$WORK/ssh_config" -p "$(cat "$VM_DIR/$1/port")" -o "ControlPath=$vm_m_ctl" \
      -o ControlMaster=yes -o ControlPersist=yes -f -N root@127.0.0.1 </dev/null >/dev/null 2>&1
  else
    ssh -F "$WORK/ssh_config" -o "ControlPath=$vm_m_ctl" -O exit root@127.0.0.1 </dev/null >/dev/null 2>&1 || :
  fi
}

# vm_engine exec [-d] [-i] [-u USER] [-e NAME=VALUE]... VM COMMAND [ARG...] is the library's container engine: it
# runs COMMAND in the VM the way `docker exec` runs it in a container. As root, or as USER; without a terminal;
# stdin only with -i; in the background with -d.
vm_engine() {
  if [ "${1-}" != exec ]; then
    printf 'vm_engine: "%s" has no VM form (only exec)\n' "${1-}" >&2
    return 125
  fi
  shift
  ve_detach=0 ve_stdin=0 ve_user=root ve_env=$VM_ENV
  while [ $# -gt 0 ]; do
    case $1 in
    -d) ve_detach=1 ;;
    -i) ve_stdin=1 ;;
    -u)
      ve_user=$2
      shift
      ;;
    -e)
      ve_env="$ve_env $(sh_quote "$2")"
      shift
      ;;
    -*)
      printf 'vm_engine: exec %s has no VM form\n' "$1" >&2
      return 125
      ;;
    *) break ;;
    esac
    shift
  done
  ve_vm=$1
  shift
  ve_cmd=''
  for ve_arg in "$@"; do
    ve_cmd="$ve_cmd $(sh_quote "$ve_arg")"
  done
  [ -z "$ve_env" ] || ve_cmd=" env$ve_env$ve_cmd"
  [ "$ve_user" = root ] || ve_cmd=" runuser -u $(sh_quote "$ve_user") --$ve_cmd"
  if [ "$ve_detach" = 1 ]; then
    vm_ssh "$ve_vm" "setsid$ve_cmd </dev/null >/dev/null 2>&1 &" </dev/null
  elif [ "$ve_stdin" = 1 ]; then
    vm_ssh "$ve_vm" "exec$ve_cmd"
  else
    vm_ssh "$ve_vm" "exec$ve_cmd" </dev/null
  fi
}

# vm_put FILE PATH copies a file of this machine into the VM, mode 0644.
vm_put() {
  vm_ssh "$CONTAINER" "mkdir -p $(sh_quote "${2%/*}") && cat >$(sh_quote "$2") && chmod 0644 $(sh_quote "$2")" <"$1" ||
    die "could not copy $1 to $2 in the VM"
}

# ---------------------------------------------------------------------------------------------------------------
# The VMs

# vm_setup makes what every VM of the run shares: the SSH key and client config, and the cloud-init seed, which
# lets root in with that key.
vm_setup() {
  ssh-keygen -q -t ed25519 -N '' -C isshoni-distro-vm -f "$WORK/id_ed25519"
  cat >"$WORK/ssh_config" <<EOF
Host *
  User root
  IdentityFile $WORK/id_ed25519
  IdentitiesOnly yes
  BatchMode yes
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
  LogLevel ERROR
  ConnectTimeout 10
  ServerAliveInterval 15
  ServerAliveCountMax 4
  ControlMaster no
EOF
  # No unattended upgrades or package-cache timers while the tests run: they hold the package manager's lock and
  # restart services.
  cat >"$WORK/user-data" <<EOF
#cloud-config
disable_root: false
ssh_pwauth: false
ssh_authorized_keys:
  - $(cat "$WORK/id_ed25519.pub")
bootcmd:
  - [sh, -c, 'systemctl stop apt-daily.timer apt-daily-upgrade.timer unattended-upgrades.service dnf-makecache.timer 2>/dev/null || true']
EOF
  printf 'instance-id: isshoni-distro-vm\nlocal-hostname: isshoni-vm\n' >"$WORK/meta-data"
  cloud-localds "$WORK/seed.img" "$WORK/user-data" "$WORK/meta-data"
}

# vm_image DISTRO sets VM_IMAGE to the distro's cloud image in VM_DIR, and downloads it first when it is not there
# yet.
vm_image() {
  VM_IMAGE=$VM_DIR/images/$1.qcow2
  if [ ! -s "$VM_IMAGE" ]; then
    sh "$ROOT/deploy/test/vm/cloud-image.sh" "$1" "$VM_IMAGE" || die "no cloud image of $1"
  fi
}

# vm_ready VM waits until the VM answers on SSH, has finished cloud-init and booting, and resolves this machine's
# names: relserve's (HOST_NAME) and Pebble's. QEMU's user network reaches this machine at 10.0.2.2. Every wait has
# a limit: cloud-init twice VM_BOOT_TIMEOUT, the boot VM_BOOT_TIMEOUT. A VM that is not done by then goes on to the
# scenarios, whose checks tell what is missing.
vm_ready() {
  vr_dir=$VM_DIR/$1
  vr_start=$(date +%s)
  vr_wait=$((VM_BOOT_TIMEOUT * 2))
  until vm_ssh_new "$1" true </dev/null >/dev/null 2>&1; do
    vr_pid=$(cat "$vr_dir/qemu.pid" 2>/dev/null) || vr_pid=''
    if [ -z "$vr_pid" ] || ! kill -0 "$vr_pid" 2>/dev/null; then
      tail -n 40 "$vr_dir/console.log" >&2 || :
      die "the VM $1 stopped while it booted (its console is above)"
    fi
    if [ $(($(date +%s) - vr_start)) -gt "$VM_BOOT_TIMEOUT" ]; then
      tail -n 40 "$vr_dir/console.log" >&2 || :
      die "the VM $1 did not answer on SSH within $VM_BOOT_TIMEOUT s (its console is above)"
    fi
    sleep 3
  done
  vm_master "$1" start
  vr_cmd="timeout $vr_wait cloud-init status --wait >/dev/null 2>&1"
  # shellcheck disable=SC2016 # the VM's shell expands $? (timeout's status 124: the time was up)
  vr_cmd="$vr_cmd"'; [ $? -ne 124 ] || printf "cloud-init still running after %s s, " '"$vr_wait"
  vr_cmd="$vr_cmd; printf 'systemd: '; timeout $VM_BOOT_TIMEOUT systemctl is-system-running --wait"
  vm_ssh "$1" "$vr_cmd" </dev/null >"$vr_dir/state" 2>&1 || :
  say "  the VM $1 is up after $(($(date +%s) - vr_start)) s ($(cat "$vr_dir/state"))"
  vm_ssh "$1" "grep -q '^10.0.2.2 $HOST_NAME ' /etc/hosts || echo '10.0.2.2 $HOST_NAME pebble' >>/etc/hosts" \
    </dev/null
}

# vm_packages PACKAGE... installs packages of the distro in the VM.
vm_packages() {
  if in_container sh -c 'command -v apt-get' >/dev/null 2>&1; then
    # shellcheck disable=SC2016 # the VM's shell expands it
    in_container sh -c 'export DEBIAN_FRONTEND=noninteractive
      apt-get -o DPkg::Lock::Timeout=300 update -qq &&
        apt-get -o DPkg::Lock::Timeout=300 install -y -qq --no-install-recommends "$@"' sh "$@" >/dev/null ||
      die "could not install $* in the VM"
  else
    in_container dnf install -y -q "$@" >/dev/null || die "could not install $* in the VM"
  fi
}

# vm_start PART [HOSTFWD...] boots a new VM of the distro from its cloud image, makes it the one the scenarios run
# in (CONTAINER, named DISTRO-PART), and gets it to where distro-test.sh's start_container gets a container: the
# programs the scenarios use, relserve's CA trusted, the release reachable. HOSTFWD forwards a port of this machine
# into the VM (QEMU's tcp:ADDR:PORT-:PORT).
vm_start() {
  vs_name=$DISTRO-$1
  shift
  vs_dir=$VM_DIR/$vs_name
  rm -rf "$vs_dir"
  mkdir -p "$vs_dir"
  VM_SSH_PORT=$((VM_SSH_PORT + 1))
  echo "$VM_SSH_PORT" >"$vs_dir/port"
  vs_fwd=hostfwd=tcp:127.0.0.1:$VM_SSH_PORT-:22
  for vs_extra in "$@"; do
    vs_fwd=$vs_fwd,hostfwd=$vs_extra
  done
  say '' "==== VM $vs_name ($(uname -m), $VM_ACCEL)"
  VM_STARTED="$VM_STARTED $vs_name"
  qemu-img create -q -f qcow2 -F qcow2 -b "$VM_IMAGE" "$vs_dir/disk.qcow2" 16G
  # shellcheck disable=SC2086 # VM_QEMU is the program and its machine flags
  $VM_QEMU -name "isshoni-$vs_name" -smp 2 -m 2048 \
    -drive "if=virtio,format=qcow2,file=$vs_dir/disk.qcow2" \
    -drive "if=virtio,format=raw,readonly=on,file=$WORK/seed.img" \
    -netdev "user,id=net0,$vs_fwd" -device virtio-net-pci,netdev=net0,romfile= \
    -device virtio-rng-pci -display none -monitor none -serial "file:$vs_dir/console.log" \
    -pidfile "$vs_dir/qemu.pid" -daemonize
  CONTAINER=$vs_name
  vm_ready "$vs_name"
  # What the container images have (deploy/test/distro/Containerfile) and a cloud image may lack.
  # shellcheck disable=SC2016 # the VM's shell expands it
  vs_missing=$(in_container sh -c 'for t in curl python3 ss ps pgrep; do command -v "$t" >/dev/null || echo "$t"; done')
  if [ -n "$vs_missing" ]; then
    vs_apt=no
    if in_container sh -c 'command -v apt-get' >/dev/null 2>&1; then
      vs_apt=yes
    fi
    vs_packages=''
    for vs_tool in $vs_missing; do
      case $vs_tool:$vs_apt in
      ss:yes) vs_packages="$vs_packages iproute2" ;;
      ss:no) vs_packages="$vs_packages iproute" ;;
      ps:yes | pgrep:yes) vs_packages="$vs_packages procps" ;;
      ps:no | pgrep:no) vs_packages="$vs_packages procps-ng" ;;
      *) vs_packages="$vs_packages $vs_tool" ;;
      esac
    done
    say "  installing what the scenarios use:$vs_packages"
    # shellcheck disable=SC2086 # package names
    vm_packages $vs_packages
  fi
  if in_container sh -c 'command -v update-ca-certificates' >/dev/null 2>&1; then
    vm_put "$WORK/ca.pem" /usr/local/share/ca-certificates/relserve.crt
    in_container update-ca-certificates >/dev/null 2>&1 || die 'update-ca-certificates failed in the VM'
  else
    vm_put "$WORK/ca.pem" /etc/pki/ca-trust/source/anchors/relserve.crt
    in_container update-ca-trust >/dev/null 2>&1 || die 'update-ca-trust failed in the VM'
  fi
  in_container curl -fsS -o /dev/null "$BASE_URL/tag/v$V1" ||
    die "the VM does not reach relserve at $BASE_URL" "$(cat "$WORK/relserve.log")"
}

# vm_reboot reboots the VM the scenarios run in, and waits until it is back.
vm_reboot() {
  vb_boot=$(value cat /proc/sys/kernel/random/boot_id)
  in_container systemctl reboot >/dev/null 2>&1 || :
  vm_master "$CONTAINER" exit
  vb_start=$(date +%s)
  while :; do
    vb_now=$(vm_ssh_new "$CONTAINER" cat /proc/sys/kernel/random/boot_id </dev/null 2>/dev/null) || vb_now=''
    if [ -n "$vb_now" ] && [ "$vb_now" != "$vb_boot" ]; then
      break
    fi
    [ $(($(date +%s) - vb_start)) -le "$VM_BOOT_TIMEOUT" ] || die "the VM $CONTAINER did not come back from its reboot"
    sleep 3
  done
  vm_ready "$CONTAINER"
}

# vm_stop stops the VM the scenarios ran in and deletes its disk (KEEP=1 keeps it); its console log stays.
vm_stop() {
  vm_kill "$CONTAINER"
  [ "$KEEP" = 1 ] || rm -f "$VM_DIR/$CONTAINER/disk.qcow2"
  CONTAINER=''
}

# vm_kill VM closes the VM's SSH master and stops its QEMU: SIGTERM, and SIGKILL if it still runs 10 s later.
vm_kill() {
  vk_pidfile=$VM_DIR/$1/qemu.pid
  vm_master "$1" exit
  [ -f "$vk_pidfile" ] || return 0
  vk_pid=$(cat "$vk_pidfile")
  kill "$vk_pid" 2>/dev/null || :
  vk_tries=0
  while kill -0 "$vk_pid" 2>/dev/null; do
    if [ "$vk_tries" -ge 50 ]; then
      kill -9 "$vk_pid" 2>/dev/null || :
      break
    fi
    vk_tries=$((vk_tries + 1))
    sleep 0.2
  done
  rm -f "$vk_pidfile"
}

# vm_cleanup, the EXIT trap, stops what is left of the run: the VMs it started (not an earlier run's, whose
# pidfiles may name other processes by now) and their disks (KEEP=1 keeps them), Pebble, and then what
# distro-test.sh's cleanup removes (relserve and the work directory).
vm_cleanup() {
  vc_status=$?
  for vc_name in $VM_STARTED; do
    vm_kill "$vc_name"
    [ "$KEEP" = 1 ] || rm -f "$VM_DIR/$vc_name/disk.qcow2"
  done
  pebble_stop "$vc_status"
  cleanup
}

# vm_assert_ssh LABEL asserts that a new SSH connection to the VM still gets in (V3, V4).
vm_assert_ssh() {
  if vm_ssh_new "$CONTAINER" true </dev/null >/dev/null 2>&1; then
    ok "$1: SSH is still reachable (a new connection)"
  else
    fail "$1: SSH is still reachable (a new connection)" 'a new SSH connection to the VM failed'
  fi
}

# ---------------------------------------------------------------------------------------------------------------
# Pebble (V5)

# pebble_start starts Pebble and pebble-challtestsrv as ci.yml's acme job starts them: on this machine's network,
# with Pebble's validation ports 5002 (http-01) and 5001 (tls-alpn-01) of
# internal/server/tlsmgr/testdata/pebble-config.json, and challtestsrv as Pebble's DNS, which resolves every name to
# 127.0.0.1. The V5 VM's ports 80 and 443 are forwarded there, so Pebble validates isshoni.test against the server
# in the VM. PEBBLE_MINICA gets the CA of Pebble's own HTTPS endpoints; its certificate names "pebble", which the
# VM resolves to this machine (vm_ready).
pebble_start() {
  for ps_name in challtestsrv pebble; do
    if "$VT_ENGINE" container inspect "isshoni-vm-$ps_name" >/dev/null 2>&1; then
      die "there is a container isshoni-vm-$ps_name already, left by an earlier run or running for another one" \
        "remove it with \"$VT_ENGINE rm -f isshoni-vm-$ps_name\" when no run uses it"
    fi
  done
  say '' "vm-test: starting Pebble and pebble-challtestsrv for V5 ($VT_ENGINE)"
  VM_PEBBLE=1
  "$VT_ENGINE" run -d --name isshoni-vm-challtestsrv --network host "$VM_CHALLTESTSRV_IMAGE" \
    -defaultIPv4 127.0.0.1 -defaultIPv6 '' -http01 '' -https01 '' -tlsalpn01 '' -doh '' >/dev/null ||
    die 'pebble-challtestsrv did not start'
  "$VT_ENGINE" run -d --name isshoni-vm-pebble --network host \
    -e PEBBLE_VA_NOSLEEP=1 -e PEBBLE_WFE_NONCEREJECT=0 \
    -v "$ROOT/internal/server/tlsmgr/testdata/pebble-config.json:/test/config/isshoni.json:ro" \
    "$VM_PEBBLE_IMAGE" -config /test/config/isshoni.json -dnsserver 127.0.0.1:8053 >/dev/null ||
    die 'Pebble did not start'
  PEBBLE_MINICA=$WORK/pebble.minica.pem
  "$VT_ENGINE" cp isshoni-vm-pebble:/test/certs/pebble.minica.pem "$PEBBLE_MINICA" >/dev/null ||
    die "could not copy Pebble's CA out of its container"
  ps_tries=0
  until curl -fsS --cacert "$PEBBLE_MINICA" -o /dev/null https://127.0.0.1:14000/dir 2>/dev/null; do
    ps_tries=$((ps_tries + 1))
    [ "$ps_tries" -lt 60 ] || die 'Pebble did not answer on https://127.0.0.1:14000/dir within 30 s'
    sleep 0.5
  done
  say '  Pebble is up'
}

# pebble_stop STATUS saves the logs of Pebble and challtestsrv in VM_DIR, prints them when the run failed (STATUS
# other than 0, or a failed check), and removes their containers.
pebble_stop() {
  [ "$VM_PEBBLE" = 1 ] || return 0
  VM_PEBBLE=0
  for pt_name in pebble challtestsrv; do
    "$VT_ENGINE" logs "isshoni-vm-$pt_name" >"$VM_DIR/$pt_name.log" 2>&1 || :
    "$VT_ENGINE" rm -f "isshoni-vm-$pt_name" >/dev/null 2>&1 || :
    if [ "$1" -ne 0 ] || [ "$FAILED" -ne 0 ]; then
      say '' "vm-test: the log of $pt_name ($VM_DIR/$pt_name.log)"
      cat "$VM_DIR/$pt_name.log" || :
    fi
  done
}

# ---------------------------------------------------------------------------------------------------------------
# What a container cannot show

# In a VM the OpenSSH client is part of the system (the SSH server needs it), so the installer finds ssh-keygen
# instead of installing it: C1's two checks of that apply to containers only.
assert_eq() {
  case $1 in
  'C1 before: the container has no ssh-keygen' | 'C1: openssh-client was installed by the installer')
    printf '  skip %s (containers only: a VM has the OpenSSH client)\n' "$1"
    return 0
    ;;
  esac
  if [ "$2" = "$3" ]; then
    ok "$1"
  else
    fail "$1" "want: $2" "got:  $3"
  fi
}

# V1, what only a real kernel shows: the UDP buffer sizes of 60-isshoni.conf are in effect.
scenario_vm_v1() {
  say '' 'V1: the sysctl values of 60-isshoni.conf are in effect'
  assert_eq 'V1: /etc/sysctl.d/60-isshoni.conf is installed' yes "$(yes_no test -s /etc/sysctl.d/60-isshoni.conf)"
  assert_eq 'V1: net.core.rmem_max' 8388608 "$(value sysctl -n net.core.rmem_max)"
  assert_eq 'V1: net.core.wmem_max' 8388608 "$(value sysctl -n net.core.wmem_max)"
}

# V2 (Fedora): SELinux enforcing, the program labeled bin_t, and no AVC denial that names isshoni since the VM
# booted, after the whole main part.
scenario_vm_v2() {
  say '' 'V2: SELinux enforcing'
  assert_eq 'V2: SELinux is enforcing' Enforcing "$(value getenforce)"
  assert_eq 'V2: auditd records the denials' active "$(value systemctl is-active auditd)"
  assert_eq 'V2: ausearch is installed' yes "$(yes_no sh -c 'command -v ausearch')"
  assert_eq 'V2: the service is active' active "$(value systemctl is-active isshoni)"
  assert_contains 'V2: ls -Z shows the program as bin_t' ':bin_t:' "$(value ls -Z /usr/local/bin/isshoni)"
  assert_eq 'V2: ausearch -m avc -ts boot names no isshoni denial' '' \
    "$(in_container sh -c 'ausearch -m avc -ts boot 2>/dev/null | grep -i isshoni || :')"
}

# vm_exposure sets VM_EXPOSURE_MAX for V6: the exposure recorded in deploy/systemd/README.md (06 §4.5) plus 0.2,
# read from there, so that a newly recorded value moves the limit with it.
vm_exposure() {
  VM_EXPOSURE_REC=$(sed -n 's/^## Recorded exposure: \([0-9][0-9.]*\)$/\1/p' "$ROOT/deploy/systemd/README.md" |
    sed -n 1p)
  [ -n "$VM_EXPOSURE_REC" ] ||
    die 'deploy/systemd/README.md has no heading "## Recorded exposure: <value>" (06 §4.5), which V6 reads'
  VM_EXPOSURE_MAX=$(awk -v r="$VM_EXPOSURE_REC" 'BEGIN { printf "%g", r + 0.2 }')
}

# V6: the exposure that systemd-analyze security reports is at most VM_EXPOSURE_MAX.
scenario_vm_v6() {
  say '' "V6: systemd-analyze security isshoni (at most $VM_EXPOSURE_MAX: $VM_EXPOSURE_REC recorded, + 0.2)"
  v6n_level=$(value systemd-analyze security --no-pager isshoni |
    sed -n 's/.*Overall exposure level for isshoni\.service: \([0-9][0-9.]*\).*/\1/p')
  if [ -z "$v6n_level" ]; then
    fail 'V6: the exposure level' 'systemd-analyze security printed no exposure level for isshoni.service'
  elif awk -v l="$v6n_level" -v m="$VM_EXPOSURE_MAX" 'BEGIN { exit !(l <= m) }'; then
    ok "V6: the exposure level is $v6n_level"
  else
    fail 'V6: the exposure level' "want: at most $VM_EXPOSURE_MAX (06 §4.5)" "got:  $v6n_level"
  fi
}

# ---------------------------------------------------------------------------------------------------------------
# The parts

vm_part_main() {
  vm_start main
  case $DISTRO in
  fedora-*)
    # V2 reads the denials from auditd's log, so auditd has to run before isshoni does.
    if [ "$(yes_no sh -c 'command -v ausearch')" = no ]; then
      vm_packages audit
    fi
    in_container systemctl start auditd >/dev/null 2>&1 || :
    ;;
  esac
  # The main part of distro-test.sh, in its order, with the VM's checks between.
  scenario_c12
  scenario_c1
  scenario_vm_v1
  scenario_c2
  scenario_c3
  scenario_c6
  scenario_c4
  scenario_c5
  scenario_c8
  scenario_c9
  scenario_c10
  scenario_vm_v6
  case $DISTRO in
  fedora-*) scenario_vm_v2 ;;
  esac
  vm_stop
}

vm_part_c7() {
  vm_start c7
  scenario_c7
  vm_stop
}

vm_part_c13() {
  vm_start c13
  scenario_c13
  vm_stop
}

# V3: distro-test.sh's scenario with the firewall running, as an admin has it: ufw lets SSH in before it is turned
# on; firewalld's default zone does. Afterwards a new SSH connection must still get in.
vm_part_v3() {
  v3n_fw=$(firewall_of "$DISTRO")
  vm_start v3
  case $v3n_fw in
  ufw)
    if [ "$(yes_no sh -c 'command -v ufw')" = no ]; then
      vm_packages ufw
    fi
    in_container ufw allow 22/tcp >/dev/null || die 'ufw did not take the rule for SSH'
    ;;
  firewalld)
    if [ "$(yes_no sh -c 'command -v firewall-cmd')" = no ]; then
      vm_packages firewalld
    fi
    in_container systemctl enable --now firewalld >/dev/null 2>&1 || die 'firewalld did not start'
    ;;
  esac
  "scenario_v3_$v3n_fw"
  vm_assert_ssh "V3 $v3n_fw"
  vm_stop
}

# v4_order CMD prints how many INPUT rules of CMD (iptables or ip6tables) carry the comment isshoni, and how many
# of them come before the first REJECT rule: "2 2" when the firewall offer did its job.
v4_order() {
  value "$1" -S INPUT | awk '
    /--comment isshoni/ { n++; if (!rejected) before++ }
    / -j REJECT/ { rejected = 1 }
    END { printf "%d %d", n, before }'
}

# v4_file CMD prints the name of the file in /etc/iptables that netfilter-persistent saves CMD's rules in.
v4_file() {
  case $1 in
  iptables) echo rules.v4 ;;
  ip6tables) echo rules.v6 ;;
  esac
}

# v4_saved CMD prints how many rules with the comment isshoni that file has.
v4_saved() {
  # shellcheck disable=SC2016 # the VM's shell expands it
  in_container sh -c 'grep -c -- "--comment isshoni" "$1" || :' sh "/etc/iptables/$(v4_file "$1")"
}

# V4: Oracle Cloud's Ubuntu images: no ufw, iptables-persistent, and an INPUT chain that rejects everything it does
# not allow. The offer inserts isshoni's rules before the REJECT, for IPv4 and IPv6 (06 §4.9), and saves them; they
# survive a reboot; --uninstall removes them, and SSH still gets in.
vm_part_v4() {
  vm_start v4
  say '' 'V4: iptables with netfilter-persistent and a REJECT rule (the Oracle Cloud pattern)'
  in_container sh -c 'printf "%s\n" \
    "iptables-persistent iptables-persistent/autosave_v4 boolean false" \
    "iptables-persistent iptables-persistent/autosave_v6 boolean false" | debconf-set-selections'
  vm_packages iptables-persistent
  cat >"$WORK/rules.v4" <<'EOF'
*filter
:INPUT ACCEPT [0:0]
:FORWARD ACCEPT [0:0]
:OUTPUT ACCEPT [0:0]
-A INPUT -m state --state RELATED,ESTABLISHED -j ACCEPT
-A INPUT -p icmp -j ACCEPT
-A INPUT -i lo -j ACCEPT
-A INPUT -p udp --sport 123 -j ACCEPT
-A INPUT -p tcp -m state --state NEW -m tcp --dport 22 -j ACCEPT
-A INPUT -j REJECT --reject-with icmp-host-prohibited
-A FORWARD -j REJECT --reject-with icmp-host-prohibited
COMMIT
EOF
  # The same for IPv6, so that the offer has to open ip6tables as well.
  cat >"$WORK/rules.v6" <<'EOF'
*filter
:INPUT ACCEPT [0:0]
:FORWARD ACCEPT [0:0]
:OUTPUT ACCEPT [0:0]
-A INPUT -m state --state RELATED,ESTABLISHED -j ACCEPT
-A INPUT -p ipv6-icmp -j ACCEPT
-A INPUT -i lo -j ACCEPT
-A INPUT -p tcp -m state --state NEW -m tcp --dport 22 -j ACCEPT
-A INPUT -j REJECT --reject-with icmp6-adm-prohibited
-A FORWARD -j REJECT --reject-with icmp6-adm-prohibited
COMMIT
EOF
  vm_put "$WORK/rules.v4" /etc/iptables/rules.v4
  vm_put "$WORK/rules.v6" /etc/iptables/rules.v6
  in_container netfilter-persistent reload >/dev/null 2>&1 || die 'netfilter-persistent did not load the rules'
  for v4n_cmd in iptables ip6tables; do
    assert_contains "V4 before, $v4n_cmd: the INPUT chain rejects" '-A INPUT -j REJECT' \
      "$(value "$v4n_cmd" -S INPUT)"
  done

  run_installer "$V1" '' --yes --tls-mode off
  assert_eq 'V4 install: exit code' 0 "$RC"
  assert_contains 'V4 install: the offer' \
    'Firewall: opened ports 80, 443 (TCP) and 7882 (TCP+UDP) in iptables, and saved the rules.' "$OUT"
  for v4n_cmd in iptables ip6tables; do
    assert_eq "V4 install, $v4n_cmd: two isshoni rules, both before the REJECT" '2 2' "$(v4_order "$v4n_cmd")"
    assert_eq "V4 install, $v4n_cmd: saved in $(v4_file "$v4n_cmd")" 2 "$(v4_saved "$v4n_cmd")"
  done

  vm_reboot
  for v4n_cmd in iptables ip6tables; do
    assert_eq "V4 after a reboot, $v4n_cmd: the rules are back, before the REJECT" '2 2' \
      "$(v4_order "$v4n_cmd")"
  done
  if wait_ready; then
    ok 'V4 after a reboot: the server is ready'
  else
    fail 'V4 after a reboot: the server is ready' 'it was not ready within 60 s'
  fi

  run_installer "$V1" '' --uninstall
  assert_eq 'V4 uninstall: exit code' 0 "$RC"
  assert_contains 'V4 uninstall: the rules are removed' \
    "Firewall: removed isshoni's iptables rules, and saved the rules." "$OUT"
  for v4n_cmd in iptables ip6tables; do
    assert_eq "V4 uninstall, $v4n_cmd: no isshoni rule is left" '0 0' "$(v4_order "$v4n_cmd")"
    assert_contains "V4 uninstall, $v4n_cmd: the REJECT stays" '-A INPUT -j REJECT' \
      "$(value "$v4n_cmd" -S INPUT)"
    assert_eq "V4 uninstall, $v4n_cmd: $(v4_file "$v4n_cmd") has no isshoni rule" 0 "$(v4_saved "$v4n_cmd")"
  done
  vm_assert_ssh 'V4 uninstall'
  vm_stop
}

# V5: --domain isshoni.test against Pebble on this machine. challtestsrv resolves the name to 127.0.0.1, where QEMU
# forwards Pebble's validation ports to the VM's 80 and 443. The VM finds Pebble's directory at
# https://pebble:14000/dir (vm_ready). IP mode (--ip) is not run, although the pinned Pebble issues for IP addresses
# (ci.yml's acme job): ip mode takes only a public address (04 §4.5, config/validate.go), and a VM on a runner has
# none that Pebble could validate. It stays on the release checklist (06 §9.1 step 2).
vm_part_v5() {
  [ -s "$PEBBLE_MINICA" ] || die "V5 needs Pebble, and its CA is missing (${PEBBLE_MINICA:-Pebble was not started})"
  vm_start v5 tcp:127.0.0.1:5002-:80 tcp:127.0.0.1:5001-:443
  say '' 'V5: ACME with Pebble: --domain isshoni.test --yes'
  vm_put "$PEBBLE_MINICA" /usr/local/share/isshoni-test/pebble.minica.pem
  VM_ENV=" $(sh_quote ISSHONI_TLS_ACME_CA=https://pebble:14000/dir)"
  VM_ENV="$VM_ENV $(sh_quote ISSHONI_TLS_ACME_CA_ROOT=/usr/local/share/isshoni-test/pebble.minica.pem)"
  run_installer "$V1" '' --domain isshoni.test --yes
  VM_ENV=''
  assert_eq 'V5: exit code' 0 "$RC"
  assert_eq 'V5: the server is ready' 0 "$(exit_of isshoni healthcheck --ready)"
  # The installer returns once the server is ready, so the time since the start is an upper bound.
  v5n_since=$(awk -v s="$(value systemctl show -p ActiveEnterTimestampMonotonic --value isshoni)" \
    -v u="$(value cut -d' ' -f1 /proc/uptime)" \
    'BEGIN { if (s !~ /^[0-9]+$/ || s == 0 || u !~ /^[0-9.]+$/) print "unknown"; else printf "%d", u - s / 1000000 }')
  case $v5n_since in
  unknown) fail 'V5: ready within 60 s of the start' 'could not read the start time of the service' ;;
  *)
    if [ "$v5n_since" -le 60 ]; then
      ok "V5: ready within 60 s of the start ($v5n_since s)"
    else
      fail 'V5: ready within 60 s of the start' "it took up to $v5n_since s"
    fi
    ;;
  esac
  v5n_cert=$(openssl s_client -connect 127.0.0.1:5001 -servername isshoni.test </dev/null 2>/dev/null |
    openssl x509 -noout -issuer -ext subjectAltName 2>/dev/null) || v5n_cert='(failed)'
  assert_contains 'V5: the certificate is issued by Pebble' 'Pebble' "$v5n_cert"
  assert_contains 'V5: the certificate is for isshoni.test' 'DNS:isshoni.test' "$v5n_cert"
  # And the chain leads to the root Pebble issues from (its management interface hands it out).
  curl -fsS --cacert "$PEBBLE_MINICA" -o "$WORK/pebble-root.pem" https://127.0.0.1:15000/roots/0 ||
    die 'Pebble did not hand out its root'
  vm_put "$WORK/pebble-root.pem" /usr/local/share/isshoni-test/pebble-root.pem
  assert_eq 'V5: GET https://isshoni.test/api/v1/info, trusting only the root Pebble issues from' "$V1" "$(json_of \
    'curl -fsS --cacert /usr/local/share/isshoni-test/pebble-root.pem --resolve isshoni.test:443:127.0.0.1 https://isshoni.test/api/v1/info' \
    'd["server"]["version"]')"
  say '  note: IP mode is not run here (no public address to validate); it stays on the release checklist'
  vm_stop
}

# vm_applies PART DISTRO reports whether a part runs on the distro: v3 where distro-test.sh's firewall_of names the
# distro's firewall (fedora-44, ubuntu-24.04), v4 in the Ubuntu VM of 06 §11.2 (ubuntu-24.04), the others on all.
vm_applies() {
  case $1 in
  v3) firewall_of "$2" >/dev/null ;;
  v4) [ "$2" = ubuntu-24.04 ] ;;
  *) return 0 ;;
  esac
}

# ---------------------------------------------------------------------------------------------------------------

vm_main() {
  RELEASES=''
  PARTS=$VM_PARTS_ALL
  while getopts r:o:h vm_opt; do
    case $vm_opt in
    r) RELEASES=$OPTARG ;;
    o) PARTS=$(printf '%s' "$OPTARG" | tr ',' ' ') ;;
    h)
      usage
      exit 0
      ;;
    *) die 'usage: vm-test.sh [-r DIR] [-o PARTS] [DISTRO...] (-h tells more)' ;;
    esac
  done
  shift $((OPTIND - 1))
  for vm_part in $PARTS; do
    case " $VM_PARTS_ALL " in
    *" $vm_part "*) ;;
    *) die "unknown part \"$vm_part\"" "known: $VM_PARTS_ALL" ;;
    esac
  done
  # shellcheck disable=SC2086 # the list of names
  [ $# -gt 0 ] || set -- $DISTROS
  for vm_distro in "$@"; do
    base_image "$vm_distro" >/dev/null || die "unknown distro \"$vm_distro\"" "known: $DISTROS"
  done
  # A run in which no part applies would end green with nothing tested: a mistake of -o or of the distros (a
  # matrix row of the nightly), found before anything is built or fetched.
  vm_any=''
  for vm_distro in "$@"; do
    for vm_part in $PARTS; do
      if vm_applies "$vm_part" "$vm_distro"; then
        vm_any=1
      fi
    done
  done
  [ -n "$vm_any" ] || die "none of the parts ($PARTS) runs on $*" \
    'v3 runs on fedora-44 and ubuntu-24.04, v4 on ubuntu-24.04, the others on every distro'

  case $(uname -s)/$(uname -m) in
  Linux/x86_64) VM_QEMU="qemu-system-x86_64 -machine q35,accel=$VM_ACCEL" ;;
  Linux/aarch64)
    vm_efi=/usr/share/qemu-efi-aarch64/QEMU_EFI.fd
    [ -f "$vm_efi" ] || die "$vm_efi is missing (the package qemu-efi-aarch64)"
    VM_QEMU="qemu-system-aarch64 -machine virt,accel=$VM_ACCEL -bios $vm_efi"
    ;;
  *) die "the VMs run on Linux with x86_64 or aarch64, not on $(uname -s) $(uname -m)" ;;
  esac
  case $VM_ACCEL in
  kvm)
    if [ ! -r /dev/kvm ] || [ ! -w /dev/kvm ]; then
      die 'there is no /dev/kvm that this user may use' \
        'VM_ACCEL=tcg emulates the VMs without it, many times slower'
    fi
    VM_QEMU="$VM_QEMU -cpu host"
    ;;
  tcg)
    if [ "$(uname -m)" = aarch64 ]; then
      # A plain Armv8.2 core: TCG emulates "max" with pointer authentication and SVE at a fraction of the speed.
      VM_QEMU="$VM_QEMU -cpu neoverse-n1"
    else
      VM_QEMU="$VM_QEMU -cpu max"
    fi
    ;;
  *) die "VM_ACCEL is kvm or tcg, not \"$VM_ACCEL\"" ;;
  esac
  vm_tools="${VM_QEMU%% *} qemu-img cloud-localds ssh ssh-keygen openssl go curl awk"
  case " $PARTS " in *' v5 '*) vm_tools="$vm_tools $VT_ENGINE" ;; esac
  for vm_tool in $vm_tools; do
    command -v "$vm_tool" >/dev/null 2>&1 || die "\"$vm_tool\" is needed and not installed"
  done
  if [ -n "$RELEASES" ]; then
    RELEASES=$(CDPATH='' cd -- "$RELEASES" && pwd) || die "-r: there is no directory $RELEASES"
  fi
  vm_exposure
  mkdir -p "$VM_DIR"
  VM_DIR=$(CDPATH='' cd -- "$VM_DIR" && pwd)

  # As distro-test.sh's main prepares a run: the signing key, the signed releases, relserve. Then what the VMs
  # share, and Pebble when a distro runs V5.
  WORK=$(mktemp -d)
  trap vm_cleanup EXIT
  trap 'exit 1' INT TERM
  mkdir -p "$WORK/root"
  if [ -z "$RELEASES" ]; then
    RELEASES=$WORK/releases
    build_releases "$RELEASES"
  fi
  ssh-keygen -q -t ed25519 -N '' -C isshoni-distro-test -f "$WORK/key" >/dev/null
  prepare_releases "$RELEASES"
  start_relserve
  vm_setup
  case " $PARTS " in *' v5 '*) pebble_start ;; esac

  # A distro's image is fetched only when one of the parts runs there.
  vm_summary=''
  for DISTRO in "$@"; do
    vm_todo=''
    for vm_part in $PARTS; do
      if vm_applies "$vm_part" "$DISTRO"; then
        vm_todo="$vm_todo $vm_part"
      else
        say '' "vm-test: $vm_part does not run on $DISTRO"
        [ "${GITHUB_ACTIONS-}" != true ] || printf '::warning::vm-test: %s does not run on %s\n' "$vm_part" "$DISTRO"
      fi
    done
    [ -n "$vm_todo" ] || continue
    vm_image "$DISTRO"
    vm_passed=$PASSED vm_failed=$FAILED
    for vm_part in $vm_todo; do
      "vm_part_$vm_part"
    done
    vm_summary="$vm_summary
distro-vm $DISTRO: $((PASSED - vm_passed)) passed, $((FAILED - vm_failed)) failed (parts:$vm_todo)"
  done

  say '' "vm-test: $PASSED passed, $FAILED failed$vm_summary"
  if [ -n "${GITHUB_STEP_SUMMARY-}" ]; then
    printf '%s\n' "$vm_summary" | sed -n 's/^distro-vm /### distro-vm /p' >>"$GITHUB_STEP_SUMMARY"
  fi
  [ "$FAILED" -eq 0 ]
}

vm_main "$@"
