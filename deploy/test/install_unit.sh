#!/bin/sh
# Unit tests for deploy/install.sh (docs/m1/06-deploy-and-ci.md §11.1).
#
#   dash deploy/test/install_unit.sh
#
# `task test:sh` runs this file under dash, `bash --posix` and `busybox sh`, the three shells the installer must
# work in. It sources install.sh with ISSHONI_INSTALL_SOURCED=1, which defines the installer's functions without
# running it, and calls them one by one. Nothing here touches the machine or the network: the files are in a
# temporary directory, and the programs the functions would run (ss, iptables, the isshoni binary, ...) are shell
# functions or small scripts of this file.
#
# It needs ssh-keygen from OpenSSH 8.1 or newer, which signs the test release of the verify_release cases with a
# key pair made for this run.
set -eu

# install.sh reads ISSHONI_ variables, and a caller may have some set (CI sets ISSHONI_VERSION for its builds).
for unit_name in $(env | sed -n 's/^\(ISSHONI_[A-Za-z0-9_]*\)=.*/\1/p'); do
  unset "$unit_name"
done

HERE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
INSTALL_SH=$HERE/../install.sh
SAMPLES=$HERE/testdata/ss

ISSHONI_INSTALL_SOURCED=1
# shellcheck source=deploy/install.sh
. "$INSTALL_SH"
unset ISSHONI_INSTALL_SOURCED

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
trap 'exit 1' INT TERM

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

# run CMD ARGS... runs a function of install.sh in a subshell, as the installer's `die` ends the shell it runs
# in. The exit code goes to RC, and stdout with stderr to OUT.
run() {
  RC=0
  ("$@") >"$WORK/out" 2>&1 || RC=$?
  OUT=$(cat "$WORK/out")
}

# Tools the installer uses only where they are installed are "not installed" here, whatever this machine has:
# cosign and gh would talk to the network. A test adds a tool to HAVE_ALSO to pretend that it is there.
HAVE_NOT='cosign gh'
HAVE_ALSO=''
have() {
  case " $HAVE_ALSO " in *" $1 "*) return 0 ;; esac
  case " $HAVE_NOT " in *" $1 "*) return 1 ;; esac
  command -v "$1" >/dev/null 2>&1
}

# ---------------------------------------------------------------------------------------------------------------
# parse_args

# parsed ARGS... prints what parse_args made of the arguments and of the ISSHONI_ variables, or "exit N" when it
# ended the script.
parsed() {
  (
    parse_args "$@" >/dev/null 2>&1
    printf 'action=%s mode=%s domain=%s version=%s yes=%s' \
      "$OPT_ACTION" "$OPT_TLS_MODE" "$OPT_DOMAIN" "$OPT_VERSION" "$OPT_YES"
    printf ' nofw=%s fw=%s nostart=%s downgrade=%s reinstall=%s' \
      "$OPT_NO_FIREWALL" "$OPT_FIREWALL" "$OPT_NO_START" "$OPT_ALLOW_DOWNGRADE" "$OPT_REINSTALL"
  ) || printf 'exit %s' "$?"
}

DEFAULTS='yes=0 nofw=0 fw=0 nostart=0 downgrade=0 reinstall=0'

test_parse_args_flags() {
  assert_eq 'parse_args: no arguments' "action=install mode= domain= version= $DEFAULTS" "$(parsed)"
  assert_eq 'parse_args: --domain NAME' \
    "action=install mode=auto domain=share.example.com version= $DEFAULTS" "$(parsed --domain share.example.com)"
  assert_eq 'parse_args: --domain=NAME, cleaned up' \
    "action=install mode=auto domain=share.example.com version= $DEFAULTS" "$(parsed --domain=HTTPS://Share.Example.com/)"
  assert_eq 'parse_args: --ip' "action=install mode=ip domain= version= $DEFAULTS" "$(parsed --ip)"
  assert_eq 'parse_args: --tls-mode ip' "action=install mode=ip domain= version= $DEFAULTS" "$(parsed --tls-mode ip)"
  assert_eq 'parse_args: --ip --tls-mode ip' "action=install mode=ip domain= version= $DEFAULTS" \
    "$(parsed --ip --tls-mode=ip)"
  assert_eq 'parse_args: --tls-mode auto --domain' \
    "action=install mode=auto domain=share.example.com version= $DEFAULTS" \
    "$(parsed --tls-mode auto --domain share.example.com)"
  assert_eq 'parse_args: --tls-mode off with ISSHONI_PUBLIC_URL' \
    "action=install mode=off domain= version= $DEFAULTS" \
    "$(ISSHONI_PUBLIC_URL=https://share.example.com && parsed --tls-mode off)"
  assert_eq 'parse_args: --tls-mode manual with the certificate files' \
    "action=install mode=manual domain=share.example.com version= $DEFAULTS" \
    "$(ISSHONI_TLS_CERT_FILE=/etc/ssl/c.pem && ISSHONI_TLS_KEY_FILE=/etc/ssl/k.pem &&
      parsed --tls-mode manual --domain share.example.com)"

  assert_eq 'parse_args: --version 0.1.0' "action=install mode= domain= version=0.1.0 $DEFAULTS" \
    "$(parsed --version 0.1.0)"
  assert_eq 'parse_args: --version v0.1.0' "action=install mode= domain= version=0.1.0 $DEFAULTS" \
    "$(parsed --version v0.1.0)"
  assert_eq 'parse_args: --version=0.2.0-rc.1' "action=install mode= domain= version=0.2.0-rc.1 $DEFAULTS" \
    "$(parsed --version=0.2.0-rc.1)"
  assert_eq 'parse_args: --version latest' "action=install mode= domain= version=latest $DEFAULTS" \
    "$(parsed --version latest)"

  assert_eq 'parse_args: --yes' \
    'action=install mode= domain= version= yes=1 nofw=0 fw=0 nostart=0 downgrade=0 reinstall=0' "$(parsed --yes)"
  assert_eq 'parse_args: -y' \
    'action=install mode= domain= version= yes=1 nofw=0 fw=0 nostart=0 downgrade=0 reinstall=0' "$(parsed -y)"
  assert_eq 'parse_args: --no-firewall' \
    'action=install mode= domain= version= yes=0 nofw=1 fw=0 nostart=0 downgrade=0 reinstall=0' "$(parsed --no-firewall)"
  assert_eq 'parse_args: --firewall' \
    'action=install mode= domain= version= yes=0 nofw=0 fw=1 nostart=0 downgrade=0 reinstall=0' "$(parsed --firewall)"
  assert_eq 'parse_args: --no-start' \
    'action=install mode= domain= version= yes=0 nofw=0 fw=0 nostart=1 downgrade=0 reinstall=0' "$(parsed --no-start)"
  assert_eq 'parse_args: --allow-downgrade' \
    'action=install mode= domain= version= yes=0 nofw=0 fw=0 nostart=0 downgrade=1 reinstall=0' \
    "$(parsed --allow-downgrade)"
  assert_eq 'parse_args: --reinstall' \
    'action=install mode= domain= version= yes=0 nofw=0 fw=0 nostart=0 downgrade=0 reinstall=1' "$(parsed --reinstall)"
  assert_eq 'parse_args: --uninstall' "action=uninstall mode= domain= version= $DEFAULTS" "$(parsed --uninstall)"
  assert_eq 'parse_args: --purge' "action=purge mode= domain= version= $DEFAULTS" "$(parsed --purge)"
  assert_eq 'parse_args: --purge --uninstall is a purge' "action=purge mode= domain= version= $DEFAULTS" \
    "$(parsed --purge --uninstall)"
  assert_eq 'parse_args: every flag at once' \
    'action=install mode=auto domain=share.example.com version=0.3.0 yes=1 nofw=1 fw=0 nostart=1 downgrade=1 reinstall=1' \
    "$(parsed --domain share.example.com --version v0.3.0 -y --no-firewall --no-start --allow-downgrade --reinstall)"

  run parse_args --help
  assert_eq 'parse_args: --help exits 0' 0 "$RC"
  for flag in --domain --ip --tls-mode --version --yes --no-firewall --firewall --no-start --allow-downgrade \
    --reinstall --uninstall --purge --help ISSHONI_PUBLIC_IP ISSHONI_TLS_ACME_EMAIL ISSHONI_DOWNLOAD_BASE; do
    assert_contains "parse_args: --help names $flag" "$flag" "$OUT"
  done
  run parse_args -h
  assert_eq 'parse_args: -h exits 0' 0 "$RC"
}

test_parse_args_env() {
  assert_eq 'parse_args: ISSHONI_DOMAIN' "action=install mode=auto domain=share.example.com version= $DEFAULTS" \
    "$(ISSHONI_DOMAIN=share.example.com && parsed)"
  assert_eq 'parse_args: ISSHONI_TLS_MODE=ip' "action=install mode=ip domain= version= $DEFAULTS" \
    "$(ISSHONI_TLS_MODE=ip && parsed)"
  assert_eq 'parse_args: ISSHONI_TLS_MODE=off with ISSHONI_PUBLIC_URL' \
    "action=install mode=off domain= version= $DEFAULTS" \
    "$(ISSHONI_TLS_MODE=off && ISSHONI_PUBLIC_URL=https://share.example.com && parsed)"
  assert_eq 'parse_args: ISSHONI_VERSION=v1.0.0' "action=install mode= domain= version=1.0.0 $DEFAULTS" \
    "$(ISSHONI_VERSION=v1.0.0 && parsed)"
  assert_eq 'parse_args: ISSHONI_YES=1 and ISSHONI_NO_FIREWALL=1' \
    'action=install mode= domain= version= yes=1 nofw=1 fw=0 nostart=0 downgrade=0 reinstall=0' \
    "$(ISSHONI_YES=1 && ISSHONI_NO_FIREWALL=1 && parsed)"
  assert_eq 'parse_args: ISSHONI_YES=0 is no' "action=install mode= domain= version= $DEFAULTS" \
    "$(ISSHONI_YES=0 && parsed)"

  # A flag wins over its environment variable.
  assert_eq 'parse_args: --domain wins over ISSHONI_DOMAIN' \
    "action=install mode=auto domain=flag.example.com version= $DEFAULTS" \
    "$(ISSHONI_DOMAIN=env.example.com && parsed --domain flag.example.com)"
  assert_eq 'parse_args: --ip wins over ISSHONI_DOMAIN' "action=install mode=ip domain= version= $DEFAULTS" \
    "$(ISSHONI_DOMAIN=env.example.com && parsed --ip)"
  assert_eq 'parse_args: --domain wins over ISSHONI_TLS_MODE=ip' \
    "action=install mode=auto domain=flag.example.com version= $DEFAULTS" \
    "$(ISSHONI_TLS_MODE=ip && parsed --domain flag.example.com)"
  assert_eq 'parse_args: --tls-mode wins over ISSHONI_TLS_MODE' \
    "action=install mode=off domain= version= $DEFAULTS" \
    "$(ISSHONI_TLS_MODE=ip && ISSHONI_PUBLIC_URL=https://share.example.com && parsed --tls-mode off)"
  assert_eq 'parse_args: --tls-mode off wins over ISSHONI_DOMAIN' \
    "action=install mode=off domain= version= $DEFAULTS" \
    "$(ISSHONI_DOMAIN=env.example.com && ISSHONI_PUBLIC_URL=https://share.example.com && parsed --tls-mode off)"
  assert_eq 'parse_args: --tls-mode auto takes ISSHONI_DOMAIN' \
    "action=install mode=auto domain=env.example.com version= $DEFAULTS" \
    "$(ISSHONI_DOMAIN=env.example.com && parsed --tls-mode auto)"
  assert_eq 'parse_args: --version wins over ISSHONI_VERSION' \
    "action=install mode= domain= version=0.2.0 $DEFAULTS" "$(ISSHONI_VERSION=0.1.0 && parsed --version 0.2.0)"
  assert_eq 'parse_args: --domain keeps ISSHONI_TLS_MODE=manual, which takes a domain' \
    "action=install mode=manual domain=flag.example.com version= $DEFAULTS" \
    "$(ISSHONI_TLS_MODE=manual && ISSHONI_TLS_CERT_FILE=/c.pem && ISSHONI_TLS_KEY_FILE=/k.pem &&
      parsed --domain flag.example.com)"

  # What parse_args copies for later steps.
  assert_eq 'parse_args: the values for config init' \
    '203.0.113.7|https://share.example.com|admin@example.com|/c.pem|/k.pem|127.0.0.1:9000' \
    "$(
      ISSHONI_PUBLIC_IP=203.0.113.7 ISSHONI_PUBLIC_URL=https://share.example.com
      ISSHONI_TLS_ACME_EMAIL=admin@example.com ISSHONI_TLS_CERT_FILE=/c.pem ISSHONI_TLS_KEY_FILE=/k.pem
      ISSHONI_LISTEN_HTTP=127.0.0.1:9000
      parse_args --tls-mode off
      printf '%s|%s|%s|%s|%s|%s' "$OPT_PUBLIC_IP" "$OPT_PUBLIC_URL" "$OPT_ACME_EMAIL" "$OPT_CERT_FILE" \
        "$OPT_KEY_FILE" "$OPT_LISTEN_HTTP"
    )"
  assert_eq 'parse_args: the default download base' 'https://github.com/MoonWX/isshoni/releases' \
    "$(parse_args && printf '%s' "$RELEASES_BASE")"
  assert_eq 'parse_args: ISSHONI_DOWNLOAD_BASE, without its last slash' 'https://mirror.example.com/isshoni' \
    "$(ISSHONI_DOWNLOAD_BASE=https://mirror.example.com/isshoni/ && parse_args && printf '%s' "$RELEASES_BASE")"
}

test_parse_args_errors() {
  assert_eq 'parse_args: unknown flag' 'exit 2' "$(parsed --bogus)"
  assert_eq 'parse_args: an argument that is no flag' 'exit 2' "$(parsed share.example.com)"
  assert_eq 'parse_args: --ip with --domain' 'exit 2' "$(parsed --ip --domain share.example.com)"
  assert_eq 'parse_args: --domain with --ip' 'exit 2' "$(parsed --domain share.example.com --ip)"
  assert_eq 'parse_args: --ip with --tls-mode off' 'exit 2' \
    "$(ISSHONI_PUBLIC_URL=https://share.example.com && parsed --ip --tls-mode off)"
  assert_eq 'parse_args: --tls-mode ip with --domain' 'exit 2' "$(parsed --tls-mode ip --domain share.example.com)"
  assert_eq 'parse_args: ISSHONI_TLS_MODE=ip with ISSHONI_DOMAIN' 'exit 2' \
    "$(ISSHONI_TLS_MODE=ip && ISSHONI_DOMAIN=share.example.com && parsed)"
  assert_eq 'parse_args: --tls-mode off with --domain' 'exit 2' \
    "$(ISSHONI_PUBLIC_URL=https://share.example.com && parsed --tls-mode off --domain share.example.com)"
  assert_eq 'parse_args: ISSHONI_TLS_MODE=off with ISSHONI_DOMAIN' 'exit 2' \
    "$(ISSHONI_TLS_MODE=off && ISSHONI_PUBLIC_URL=https://share.example.com && ISSHONI_DOMAIN=share.example.com &&
      parsed)"
  assert_eq 'parse_args: --domain without a value' 'exit 2' "$(parsed --domain)"
  assert_eq 'parse_args: --domain with an empty value' 'exit 2' "$(parsed --domain=)"
  assert_eq 'parse_args: --domain that is no name' 'exit 2' "$(parsed --domain bad_name)"
  assert_eq 'parse_args: --domain with an IP address' 'exit 2' "$(parsed --domain 203.0.113.7)"
  assert_eq 'parse_args: --version without a value' 'exit 2' "$(parsed --version)"
  assert_eq 'parse_args: --version that is none' 'exit 2' "$(parsed --version 1.2)"
  assert_eq 'parse_args: --version with a path in it' 'exit 2' "$(parsed --version 1.2.3/../x)"
  assert_eq 'parse_args: ISSHONI_VERSION that is none' 'exit 2' "$(ISSHONI_VERSION=newest && parsed)"
  assert_eq 'parse_args: --tls-mode that is none' 'exit 2' "$(parsed --tls-mode selfsigned)"
  assert_eq 'parse_args: --tls-mode auto without a domain' 'exit 2' "$(parsed --tls-mode auto)"
  assert_eq 'parse_args: --tls-mode manual without the certificate files' 'exit 2' "$(parsed --tls-mode manual)"
  assert_eq 'parse_args: --firewall with --no-firewall' 'exit 2' "$(parsed --firewall --no-firewall)"
  assert_eq 'parse_args: --firewall with ISSHONI_NO_FIREWALL=1' 'exit 2' \
    "$(ISSHONI_NO_FIREWALL=1 && parsed --firewall)"

  run parse_args --tls-mode off
  assert_eq 'parse_args: --tls-mode off without ISSHONI_PUBLIC_URL' 2 "$RC"
  assert_contains 'parse_args: the off-mode message' 'off mode needs ISSHONI_PUBLIC_URL=https://your.domain' "$OUT"
  assert_eq 'parse_args: ISSHONI_TLS_MODE=off without ISSHONI_PUBLIC_URL' 'exit 2' "$(ISSHONI_TLS_MODE=off && parsed)"
  # An uninstall writes no config, so a TLS setting of the environment can't be wrong for it.
  assert_eq 'parse_args: --uninstall ignores the TLS settings' "action=uninstall mode= domain= version= $DEFAULTS" \
    "$(ISSHONI_TLS_MODE=off && parsed --uninstall)"

  run parse_args --bogus
  assert_contains 'parse_args: the unknown-flag message names the flag' '--bogus' "$OUT"
  assert_contains 'parse_args: the unknown-flag message says what to do' '--help' "$OUT"
}

test_scrub_env() {
  assert_eq 'scrub_env: no ISSHONI_ variable is left for the programs the installer runs' '0' "$(
    ISSHONI_DOMAIN=share.example.com ISSHONI_TLS_MODE=auto ISSHONI_LOG_LEVEL=debug ISSHONI_SOMETHING_ELSE=1
    ISSHONI_INSTALLER_VERSION=9.9.9
    export ISSHONI_DOMAIN ISSHONI_TLS_MODE ISSHONI_LOG_LEVEL ISSHONI_SOMETHING_ELSE ISSHONI_INSTALLER_VERSION
    OTHER_VARIABLE=kept
    export OTHER_VARIABLE
    parse_args
    scrub_env
    env | grep -c '^ISSHONI_' || :
  )"
  assert_eq 'scrub_env: other variables stay, and parse_args had its copy first' 'kept share.example.com' "$(
    ISSHONI_DOMAIN=share.example.com OTHER_VARIABLE=kept
    export ISSHONI_DOMAIN OTHER_VARIABLE
    parse_args
    scrub_env
    printf '%s %s' "$(env | sed -n 's/^OTHER_VARIABLE=//p')" "$OPT_DOMAIN"
  )"
}

# ---------------------------------------------------------------------------------------------------------------
# detect_arch, normalize_domain

test_detect_arch() {
  for pair in x86_64:amd64 amd64:amd64 aarch64:arm64 arm64:arm64; do
    assert_eq "detect_arch: ${pair%%:*}" "${pair##*:}" "$(detect_arch "${pair%%:*}" && printf '%s' "$ARCH")"
  done
  run detect_arch armv7l
  assert_eq 'detect_arch: armv7l is not supported' 4 "$RC"
  assert_contains 'detect_arch: the message names the machine' 'armv7l' "$OUT"
  run detect_arch riscv64
  assert_eq 'detect_arch: riscv64 is not supported' 4 "$RC"
}

# The machine that preflight sees: uname and id answer from variables, and dpkg owns the packaged program or not.
FAKE_OS=Linux
FAKE_MACHINE=x86_64
FAKE_UID=0
DPKG_OWNS=0

uname() {
  case ${1-} in
  -m) printf '%s\n' "$FAKE_MACHINE" ;;
  *) printf '%s\n' "$FAKE_OS" ;;
  esac
}

id() {
  printf '%s\n' "$FAKE_UID"
}

dpkg() {
  [ "$DPKG_OWNS" = 1 ]
}

test_preflight() {
  SYSTEMD_DIR=$WORK/run-systemd
  PACKAGE_BIN=$WORK/usr-bin-isshoni
  mkdir -p "$SYSTEMD_DIR"
  rm -f "$PACKAGE_BIN"
  HAVE_ALSO='systemctl dpkg'
  parse_args

  run preflight
  assert_eq "preflight: root on Linux with systemd ($OUT)" 0 "$RC"
  assert_eq 'preflight: root needs no sudo, and the architecture is known' '|amd64' \
    "$(preflight && printf '%s|%s' "$SUDO" "$ARCH")"

  FAKE_OS=Darwin
  run preflight
  assert_eq 'preflight: not Linux' 4 "$RC"
  assert_contains 'preflight: not Linux, the message' 'runs on Linux only (this is Darwin)' "$OUT"
  assert_contains 'preflight: not Linux points to Docker' 'https://moonwx.github.io/isshoni/install/docker' "$OUT"
  FAKE_OS=Linux

  FAKE_MACHINE=armv7l
  run preflight
  assert_eq 'preflight: a CPU without a build' 4 "$RC"
  FAKE_MACHINE=x86_64

  SYSTEMD_DIR=$WORK/no-systemd-here
  run preflight
  assert_eq 'preflight: no systemd' 4 "$RC"
  assert_contains 'preflight: no systemd points to Docker' 'https://moonwx.github.io/isshoni/install/docker' "$OUT"
  SYSTEMD_DIR=$WORK/run-systemd

  FAKE_UID=1000
  HAVE_NOT="$HAVE_NOT sudo"
  run preflight
  assert_eq 'preflight: not root, and no sudo' 4 "$RC"
  assert_contains 'preflight: not root, and no sudo, the message' 'sudo is not installed' "$OUT"
  HAVE_NOT='cosign gh'
  FAKE_UID=0

  HAVE_NOT="$HAVE_NOT tar"
  run preflight
  assert_eq 'preflight: a tool is missing' 4 "$RC"
  assert_contains 'preflight: a tool is missing, its name' '"tar"' "$OUT"
  HAVE_NOT='cosign gh'

  # A program in /usr/bin that a package owns belongs to the package manager.
  : >"$PACKAGE_BIN"
  run preflight
  assert_eq "preflight: a program in the package's place that no package owns ($OUT)" 0 "$RC"
  DPKG_OWNS=1
  run preflight
  assert_eq 'preflight: installed from a package' 4 "$RC"
  assert_contains 'preflight: installed from a package, the message' 'installed here from a package' "$OUT"
  parse_args --uninstall
  run preflight
  assert_eq 'preflight: --uninstall of a package installation' 4 "$RC"
  assert_contains 'preflight: --uninstall of a package installation, the way' 'sudo apt remove isshoni' "$OUT"
  parse_args
  DPKG_OWNS=0
  rm -f "$PACKAGE_BIN"
  HAVE_ALSO=''
  SUDO=''
}

# normalized INPUT prints "KIND VALUE" as normalize_domain sets them.
normalized() {
  normalize_domain "$1" || :
  printf '%s %s' "$NORM_KIND" "$NORM_VALUE"
}

test_normalize_domain() {
  assert_eq 'normalize_domain: a URL in capitals' 'domain share.example.com' "$(normalized 'HTTPS://Share.Example.com/x/')"
  assert_eq 'normalize_domain: a plain name' 'domain share.example.com' "$(normalized 'share.example.com')"
  assert_eq 'normalize_domain: spaces around it' 'domain share.example.com' "$(normalized '  share.example.com  ')"
  assert_eq 'normalize_domain: a trailing dot' 'domain share.example.com' "$(normalized 'share.example.com.')"
  assert_eq 'normalize_domain: http:// and a port' 'domain share.example.com' "$(normalized 'http://share.example.com:8443/a?b#c')"
  assert_eq 'normalize_domain: punycode' 'domain xn--bcher-kva.example' "$(normalized 'xn--bcher-kva.example')"
  assert_eq 'normalize_domain: a punycode top-level domain' 'domain example.xn--p1ai' "$(normalized 'example.xn--p1ai')"
  assert_eq 'normalize_domain: IPv4 means IP mode' 'ip 203.0.113.7' "$(normalized '203.0.113.7')"
  assert_eq 'normalize_domain: IPv4 in a URL' 'ip 203.0.113.7' "$(normalized 'https://203.0.113.7:443/')"
  assert_eq 'normalize_domain: IPv6 means IP mode' 'ip 2001:db8::1' "$(normalized '2001:db8::1')"
  assert_eq 'normalize_domain: IPv6 in capitals' 'ip 2001:db8::1' "$(normalized '2001:DB8::1')"
  assert_eq 'normalize_domain: IPv6 in brackets' 'ip 2001:db8::1' "$(normalized '[2001:db8::1]')"
  assert_eq 'normalize_domain: IPv6 in a URL with a port' 'ip 2001:db8::1' "$(normalized 'https://[2001:db8::1]:8443/x')"
  assert_eq 'normalize_domain: nothing means the IP address' 'empty ' "$(normalized '')"
  assert_eq 'normalize_domain: only spaces' 'empty ' "$(normalized '   ')"

  # shellcheck disable=SC2016 # a dollar sign in a name, not a variable
  for bad in 'bad_name' 'exa mple.com' 'localhost' 'example' '-a.example.com' 'a-.example.com' 'a..example.com' \
    '.example.com' 'example.c' 'example.123' '1.2.3' '1.2.3.4.5' '256.1.1.1' '1.2.3.4x' 'https://' 'a:b' \
    'share.example.com:port' ':::1' '2001:db8::zz' '[2001:db8::1' 'exa$mple.com' 'ex;ample.com' "ex'ample.com" \
    'user@example.com'; do
    normalize_domain "$bad" && bad_rc=0 || bad_rc=$?
    assert_eq "normalize_domain: \"$bad\" is rejected" '1 invalid' "$bad_rc $NORM_KIND"
  done
  # A label may have 63 characters and no more.
  long=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  assert_eq 'normalize_domain: a label of 63 characters' "domain $long.example.com" "$(normalized "$long.example.com")"
  assert_eq 'normalize_domain: a label of 64 characters' "invalid a$long.example.com" "$(normalized "a$long.example.com")"
}

# ---------------------------------------------------------------------------------------------------------------
# Versions

test_version_cmp() {
  assert_eq 'version_cmp: 0.2.0-rc.1 < 0.2.0' '-1' "$(version_cmp 0.2.0-rc.1 0.2.0)"
  assert_eq 'version_cmp: 0.2.0 > 0.2.0-rc.1' '1' "$(version_cmp 0.2.0 0.2.0-rc.1)"
  assert_eq 'version_cmp: 0.10.0 > 0.9.9' '1' "$(version_cmp 0.10.0 0.9.9)"
  assert_eq 'version_cmp: 0.9.9 < 0.10.0' '-1' "$(version_cmp 0.9.9 0.10.0)"
  assert_eq 'version_cmp: rc.2 < rc.10' '-1' "$(version_cmp 0.2.0-rc.2 0.2.0-rc.10)"
  assert_eq 'version_cmp: rc.10 > rc.2' '1' "$(version_cmp 0.2.0-rc.10 0.2.0-rc.2)"
  assert_eq 'version_cmp: equal releases' '0' "$(version_cmp 1.2.3 1.2.3)"
  assert_eq 'version_cmp: equal prereleases' '0' "$(version_cmp 0.0.1-ci.1 0.0.1-ci.1)"
  assert_eq 'version_cmp: 0.0.1-ci.1 < 0.0.1-ci.2' '-1' "$(version_cmp 0.0.1-ci.1 0.0.1-ci.2)"
  assert_eq 'version_cmp: 1.0.0 > 0.99.99' '1' "$(version_cmp 1.0.0 0.99.99)"
  assert_eq 'version_cmp: 1.2.10 > 1.2.9' '1' "$(version_cmp 1.2.10 1.2.9)"
  assert_eq 'version_cmp: a leading v is ignored' '0' "$(version_cmp v1.2.3 1.2.3)"
  assert_eq 'version_cmp: build metadata is ignored' '0' "$(version_cmp 0.0.0-dev+abc123 0.0.0-dev+def456)"
  assert_eq 'version_cmp: a development build < its release' '-1' "$(version_cmp 0.1.0-dev+abc123 0.1.0)"
  # The order of semver.org §11: 1.0.0-alpha < alpha.1 < alpha.beta < beta < beta.2 < beta.11 < rc.1 < 1.0.0.
  previous=''
  for v in 1.0.0-alpha 1.0.0-alpha.1 1.0.0-alpha.beta 1.0.0-beta 1.0.0-beta.2 1.0.0-beta.11 1.0.0-rc.1 1.0.0; do
    if [ -n "$previous" ]; then
      assert_eq "version_cmp: $previous < $v" '-1' "$(version_cmp "$previous" "$v")"
      assert_eq "version_cmp: $v > $previous" '1' "$(version_cmp "$v" "$previous")"
    fi
    previous=$v
  done
}

test_resolve_version() {
  assert_eq 'version_from_url: a GitHub release URL' '1.2.3' \
    "$(version_from_url 'https://github.com/MoonWX/isshoni/releases/tag/v1.2.3')"
  assert_eq 'version_from_url: a prerelease' '0.2.0-rc.1' \
    "$(version_from_url 'https://github.com/MoonWX/isshoni/releases/tag/v0.2.0-rc.1')"
  assert_eq 'version_from_url: a mirror, and the line end of an HTTP header' '0.0.1-ci.2' \
    "$(version_from_url "$(printf 'https://mirror.example.com:8443/x/tag/v0.0.1-ci.2\r')")"
  assert_eq 'version_from_url: a relative Location' '1.2.3' "$(version_from_url '/MoonWX/isshoni/releases/tag/v1.2.3')"
  for bad in '' 'https://github.com/MoonWX/isshoni/releases' 'https://github.com/MoonWX/isshoni/releases/latest' \
    'https://github.com/MoonWX/isshoni/releases/tag/1.2.3' 'https://github.com/login?return_to=x' \
    'https://example.com/tag/vnext'; do
    version_from_url "$bad" >/dev/null && bad_rc=0 || bad_rc=$?
    assert_eq "version_from_url: \"$bad\" names no version" 1 "$bad_rc"
  done

  # latest_url is the one function that talks to the network; here it answers from a variable.
  DL=stub
  latest_url() {
    : >"$WORK/latest-asked"
    printf '%s' "$LATEST_ANSWER"
  }
  LATEST_ANSWER='https://github.com/MoonWX/isshoni/releases/tag/v1.2.3'

  assert_eq 'resolve_version: an unstamped script installs the latest release' '1.2.3' \
    "$(parse_args && resolve_version && printf '%s' "$TV")"
  rm -f "$WORK/latest-asked"
  assert_eq 'resolve_version: a stamped version wins over latest' '0.3.0' \
    "$(INSTALLER_VERSION=0.3.0 && parse_args && resolve_version && printf '%s' "$TV")"
  assert_eq 'resolve_version: a stamped script does not ask for the latest release' 'not asked' \
    "$([ -e "$WORK/latest-asked" ] && printf asked || printf 'not asked')"
  assert_eq 'resolve_version: ISSHONI_VERSION=v1.0.0' '1.0.0' \
    "$(ISSHONI_VERSION=v1.0.0 && parse_args && resolve_version && printf '%s' "$TV")"
  assert_eq 'resolve_version: ISSHONI_VERSION wins over the stamped version' '1.0.0' \
    "$(INSTALLER_VERSION=0.3.0 && ISSHONI_VERSION=v1.0.0 && parse_args && resolve_version && printf '%s' "$TV")"
  assert_eq 'resolve_version: --version wins over ISSHONI_VERSION and the stamp' '2.0.0' \
    "$(INSTALLER_VERSION=0.3.0 && ISSHONI_VERSION=1.0.0 && parse_args --version 2.0.0 && resolve_version &&
      printf '%s' "$TV")"
  assert_eq 'resolve_version: --version latest on a stamped script' '1.2.3' \
    "$(INSTALLER_VERSION=0.3.0 && parse_args --version latest && resolve_version && printf '%s' "$TV")"

  LATEST_ANSWER='https://github.com/login'
  run sh_resolve
  assert_eq 'resolve_version: no version behind /latest' 1 "$RC"
  assert_contains 'resolve_version: the message says what to do' '--version' "$OUT"
  LATEST_ANSWER=''
  run sh_resolve
  assert_eq 'resolve_version: /latest does not answer' 1 "$RC"
}

sh_resolve() {
  parse_args
  resolve_version
}

# ---------------------------------------------------------------------------------------------------------------
# checksum_for, verify_release

test_checksum_for() {
  sums=$WORK/sums.txt
  {
    printf '%s  %s\n' 1111111111111111111111111111111111111111111111111111111111111111 x.tar.gz.sbom.json
    printf '%s  %s\n' 2222222222222222222222222222222222222222222222222222222222222222 x.tar.gz
    printf '%s  %s\n' 3333333333333333333333333333333333333333333333333333333333333333 ax.tar.gz
    printf '%s *%s\n' 4444444444444444444444444444444444444444444444444444444444444444 binary.tar.gz
    printf '%s  %s\n' 5555555555555555555555555555555555555555555555555555555555555555 twice.tar.gz
    printf '%s  %s\n' 6666666666666666666666666666666666666666666666666666666666666666 twice.tar.gz
  } >"$sums"
  assert_eq 'checksum_for: the exact name' 2222222222222222222222222222222222222222222222222222222222222222 \
    "$(checksum_for x.tar.gz "$sums")"
  assert_eq 'checksum_for: x.tar.gz.sbom.json is another file' \
    1111111111111111111111111111111111111111111111111111111111111111 "$(checksum_for x.tar.gz.sbom.json "$sums")"
  assert_eq 'checksum_for: a name sha256sum wrote in binary mode' \
    4444444444444444444444444444444444444444444444444444444444444444 "$(checksum_for binary.tar.gz "$sums")"
  for name in twice.tar.gz missing.tar.gz tar.gz 'x.tar' 'x.tar.gz.sbom' '.*'; do
    got=$(checksum_for "$name" "$sums") && rc=0 || rc=$?
    assert_eq "checksum_for: \"$name\" fails" "1 " "$rc $got"
  done
}

# make_release DIR KEY NAMESPACE writes a release into DIR: an archive, a checksums.txt that lists it between two
# other files, and the signature of checksums.txt by KEY for NAMESPACE.
make_release() {
  mkdir -p "$1"
  printf 'the archive of a test release\n' >"$1/$TEST_ARCHIVE"
  {
    printf '%s  %s\n' 1111111111111111111111111111111111111111111111111111111111111111 "$TEST_ARCHIVE.sbom.json"
    printf '%s  %s\n' "$(sha256_of "$1/$TEST_ARCHIVE")" "$TEST_ARCHIVE"
    printf '%s  %s\n' 3333333333333333333333333333333333333333333333333333333333333333 install.sh
  } >"$1/checksums.txt"
  sign_release "$1" "$2" "$3"
}

sign_release() {
  rm -f "$1/checksums.txt.sig"
  ssh-keygen -Y sign -f "$2" -n "$3" "$1/checksums.txt" >/dev/null 2>&1
}

test_verify_release() {
  TEST_ARCHIVE=isshoni_0.0.1-ci.1_linux_amd64.tar.gz
  TV=0.0.1-ci.1
  if ! ssh-keygen -q -t ed25519 -N '' -C isshoni-unit-test -f "$WORK/key" >/dev/null 2>&1 ||
    ! ssh-keygen -q -t ed25519 -N '' -C isshoni-unit-test-other -f "$WORK/other" >/dev/null 2>&1; then
    FAILED=$((FAILED + 1))
    printf 'FAIL verify_release: ssh-keygen could not make the test keys (OpenSSH 8.1 or newer is needed)\n'
    return 0
  fi
  # The key block of a release, with the key of this run in it.
  ALLOWED_SIGNERS="isshoni-release namespaces=\"isshoni-checksums\" $(cat "$WORK/key.pub")"

  make_release "$WORK/good" "$WORK/key" isshoni-checksums
  run verify_release "$WORK/good" "$TEST_ARCHIVE"
  assert_eq "verify_release: a valid release ($OUT)" 0 "$RC"

  # Two keys in the block, as a release has: the signature of either is good.
  ALLOWED_SIGNERS="isshoni-release namespaces=\"isshoni-checksums\" $(cat "$WORK/other.pub")
isshoni-release namespaces=\"isshoni-checksums\" $(cat "$WORK/key.pub")"
  run verify_release "$WORK/good" "$TEST_ARCHIVE"
  assert_eq "verify_release: valid with the second key of the block ($OUT)" 0 "$RC"
  ALLOWED_SIGNERS="isshoni-release namespaces=\"isshoni-checksums\" $(cat "$WORK/key.pub")"

  make_release "$WORK/changed" "$WORK/key" isshoni-checksums
  sed 's/^1/9/' "$WORK/changed/checksums.txt" >"$WORK/changed/checksums.new"
  mv "$WORK/changed/checksums.new" "$WORK/changed/checksums.txt"
  run verify_release "$WORK/changed" "$TEST_ARCHIVE"
  assert_eq 'verify_release: one byte changed in checksums.txt' 3 "$RC"
  assert_contains 'verify_release: the message says that nothing was done' 'Nothing was installed or changed.' "$OUT"

  make_release "$WORK/nosig" "$WORK/key" isshoni-checksums
  rm "$WORK/nosig/checksums.txt.sig"
  run verify_release "$WORK/nosig" "$TEST_ARCHIVE"
  assert_eq 'verify_release: the signature is missing' 3 "$RC"

  make_release "$WORK/emptysig" "$WORK/key" isshoni-checksums
  : >"$WORK/emptysig/checksums.txt.sig"
  run verify_release "$WORK/emptysig" "$TEST_ARCHIVE"
  assert_eq 'verify_release: the signature is empty' 3 "$RC"

  make_release "$WORK/otherkey" "$WORK/other" isshoni-checksums
  run verify_release "$WORK/otherkey" "$TEST_ARCHIVE"
  assert_eq 'verify_release: signed with another key' 3 "$RC"

  make_release "$WORK/namespace" "$WORK/key" file
  run verify_release "$WORK/namespace" "$TEST_ARCHIVE"
  assert_eq 'verify_release: the right key, but the namespace "file"' 3 "$RC"

  # The signature is good, and the archive is not the one that was signed for.
  make_release "$WORK/swapped" "$WORK/key" isshoni-checksums
  printf 'another archive\n' >"$WORK/swapped/$TEST_ARCHIVE"
  run verify_release "$WORK/swapped" "$TEST_ARCHIVE"
  assert_eq 'verify_release: the archive does not have the listed checksum' 3 "$RC"
  assert_contains 'verify_release: the message names the checksum' 'does not match checksums.txt' "$OUT"

  make_release "$WORK/unlisted" "$WORK/key" isshoni-checksums
  grep -v " $TEST_ARCHIVE\$" "$WORK/unlisted/checksums.txt" >"$WORK/unlisted/checksums.new"
  mv "$WORK/unlisted/checksums.new" "$WORK/unlisted/checksums.txt"
  sign_release "$WORK/unlisted" "$WORK/key" isshoni-checksums
  run verify_release "$WORK/unlisted" "$TEST_ARCHIVE"
  assert_eq 'verify_release: checksums.txt does not list the archive' 3 "$RC"

  make_release "$WORK/twice" "$WORK/key" isshoni-checksums
  printf '%s  %s\n' "$(sha256_of "$WORK/twice/$TEST_ARCHIVE")" "$TEST_ARCHIVE" >>"$WORK/twice/checksums.txt"
  sign_release "$WORK/twice" "$WORK/key" isshoni-checksums
  run verify_release "$WORK/twice" "$TEST_ARCHIVE"
  assert_eq 'verify_release: checksums.txt lists the archive twice' 3 "$RC"

  make_release "$WORK/noarchive" "$WORK/key" isshoni-checksums
  rm "$WORK/noarchive/$TEST_ARCHIVE"
  run verify_release "$WORK/noarchive" "$TEST_ARCHIVE"
  assert_eq 'verify_release: the archive is missing' 3 "$RC"

  # A key that may sign for another identity, or for another namespace only, is not a release key.
  ALLOWED_SIGNERS="someone-else namespaces=\"isshoni-checksums\" $(cat "$WORK/key.pub")"
  run verify_release "$WORK/good" "$TEST_ARCHIVE"
  assert_eq 'verify_release: the key belongs to another identity' 3 "$RC"
  ALLOWED_SIGNERS="isshoni-release namespaces=\"file\" $(cat "$WORK/key.pub")"
  run verify_release "$WORK/good" "$TEST_ARCHIVE"
  assert_eq 'verify_release: the key is for another namespace' 3 "$RC"

  # The key block of the repository's own copy is empty until a release key exists: it must verify nothing.
  ALLOWED_SIGNERS=''
  run verify_release "$WORK/good" "$TEST_ARCHIVE"
  assert_eq 'verify_release: a script without a key' 3 "$RC"
  assert_contains 'verify_release: the message of a script without a key' 'no release signing key' "$OUT"
  ALLOWED_SIGNERS="isshoni-release namespaces=\"isshoni-checksums\" $(cat "$WORK/key.pub")"

  # An OpenSSH before 8.1 has no -Y: that is an unsupported system, not a bad release.
  run with_old_ssh_keygen verify_release "$WORK/good" "$TEST_ARCHIVE"
  assert_eq 'verify_release: an OpenSSH without -Y verify' 4 "$RC"

  # Where cosign is installed, its check must pass too, and it is asked about this release's workflow run.
  HAVE_ALSO=cosign
  run with_cosign 0 verify_release "$WORK/good" "$TEST_ARCHIVE"
  assert_eq "verify_release: cosign agrees ($OUT)" 0 "$RC"
  cosign_want="verify-blob --bundle $WORK/good/checksums.txt.sigstore.json"
  cosign_want="$cosign_want --certificate-identity"
  cosign_want="$cosign_want https://github.com/MoonWX/isshoni/.github/workflows/release.yml@refs/tags/v0.0.1-ci.1"
  cosign_want="$cosign_want --certificate-oidc-issuer https://token.actions.githubusercontent.com"
  cosign_want="$cosign_want $WORK/good/checksums.txt"
  assert_eq 'verify_release: what cosign is asked' "$cosign_want" "$(cat "$WORK/cosign-args")"
  run with_cosign 1 verify_release "$WORK/good" "$TEST_ARCHIVE"
  assert_eq 'verify_release: cosign disagrees' 3 "$RC"
  HAVE_ALSO=''
}

# with_old_ssh_keygen CMD... runs CMD with an ssh-keygen that answers like OpenSSH 8.0. It is a program on the
# PATH: a shell function can't have a hyphen in its name.
with_old_ssh_keygen() {
  mkdir -p "$WORK/oldssh"
  cat >"$WORK/oldssh/ssh-keygen" <<'EOF'
#!/bin/sh
printf '%s\n' 'ssh-keygen: illegal option -- Y' 'usage: ssh-keygen [-q] [-b bits] [-t dsa | ecdsa | ed25519 | rsa]' >&2
exit 1
EOF
  chmod +x "$WORK/oldssh/ssh-keygen"
  PATH=$WORK/oldssh:$PATH
  "$@"
}

# with_cosign RC CMD... runs CMD with a cosign that records its arguments and exits with RC.
with_cosign() {
  cosign_rc=$1
  shift
  # shellcheck disable=SC2329 # verify_release calls it
  cosign() {
    printf '%s' "$*" >"$WORK/cosign-args"
    return "$cosign_rc"
  }
  "$@"
}

# ---------------------------------------------------------------------------------------------------------------
# check_ports

# ss here prints a recorded sample: `ss -Hltnp` the one named by SS_TCP, `ss -Hlunp` the one named by SS_UDP.
ss() {
  case $1 in
  -Hltnp) cat "$SAMPLES/$SS_TCP.tcp" ;;
  -Hlunp) cat "$SAMPLES/$SS_UDP.udp" ;;
  *) return 2 ;;
  esac
}

test_check_ports() {
  CFG_TLS_MODE=auto

  SS_TCP=nginx SS_UDP=idle
  run check_ports '443 80 7882' '7882'
  assert_eq 'check_ports: nginx on 443' 7 "$RC"
  assert_contains 'check_ports: nginx on 443, who and where' \
    'Port 443 is used by nginx (pid 812). isshoni needs ports 80 and 443.' "$OUT"
  assert_contains 'check_ports: nginx on 443, the first way out' 'Either stop nginx, or run isshoni behind it:' "$OUT"
  assert_contains 'check_ports: nginx on 443, the command for the second' \
    'ISSHONI_PUBLIC_URL=https://share.example.com sh -s -- --tls-mode off' "$OUT"
  assert_contains 'check_ports: nginx on 443, the guide' 'https://moonwx.github.io/isshoni/install/reverse-proxy' "$OUT"

  SS_TCP=isshoni SS_UDP=isshoni
  run check_ports '443 80 7882' '7882'
  assert_eq "check_ports: isshoni itself on its ports ($OUT)" 0 "$RC"

  SS_TCP=idle SS_UDP=idle
  run check_ports '443 80 7882' '7882'
  assert_eq "check_ports: nothing listening ($OUT)" 0 "$RC"

  SS_TCP=idle SS_UDP=python
  run check_ports '443 80 7882' '7882'
  assert_eq 'check_ports: python3 on 7882/udp' 7 "$RC"
  assert_contains 'check_ports: python3 on 7882/udp, who and where' 'Port 7882/udp is used by python3 (pid 596).' "$OUT"

  # Without root, ss does not say whose socket it is.
  SS_TCP=unprivileged SS_UDP=idle
  run check_ports '443 80 7882' '7882'
  assert_eq 'check_ports: a listener ss does not name' 7 "$RC"
  assert_contains 'check_ports: a listener ss does not name, the message' 'Port 80 is used by another program.' "$OUT"

  # Behind a proxy (tls.mode off), 80 and 443 are the proxy's and isshoni's HTTP port is its own.
  CFG_TLS_MODE=off
  SS_TCP=proxy SS_UDP=idle
  run check_ports '9000 7882' '7882'
  assert_eq "check_ports: off mode, the proxy has 443 ($OUT)" 0 "$RC"
  run check_ports '8080 7882' '7882'
  assert_eq 'check_ports: off mode, the HTTP port is taken' 7 "$RC"
  assert_contains 'check_ports: off mode, who has the HTTP port' 'Port 8080/tcp is used by node (pid 2001).' "$OUT"
  CFG_TLS_MODE=auto

  # A port that the config turns off is not in the list, and not checked.
  SS_TCP=idle SS_UDP=python
  run check_ports '443 80 7882' ''
  assert_eq "check_ports: UDP turned off ($OUT)" 0 "$RC"

  assert_eq 'port_conflict: the first conflict, as "port proto pid program"' '443 tcp 812 nginx' \
    "$(port_conflict tcp '80 443 7882' <"$SAMPLES/nginx.tcp")"
  assert_eq 'port_conflict: a program name with a space' '80 tcp 77 Web Content' \
    "$(printf '%s\n' 'LISTEN 0 5 *:80 *:* users:(("Web Content",pid=77,fd=3))' | port_conflict tcp 80)"
  assert_eq 'port_conflict: port 8 is not port 80' '' \
    "$(printf '%s\n' 'LISTEN 0 5 *:80 *:* users:(("nginx",pid=1,fd=3))' | port_conflict tcp '8 800')"
  assert_eq 'port_conflict: an IPv6 address with a zone' '7882 udp 9 other' \
    "$(printf '%s\n' 'UNCONN 0 0 [fe80::1%eth0]:7882 [::]:* users:(("other",pid=9,fd=3))' | port_conflict udp 7882)"

  assert_eq 'port_of: all interfaces' 443 "$(port_of :443)"
  assert_eq 'port_of: an IPv4 address' 8080 "$(port_of 127.0.0.1:8080)"
  assert_eq 'port_of: an IPv6 address' 8080 "$(port_of '[::1]:8080')"
  assert_eq 'port_of: a listener that is off' '' "$(port_of '')"

  assert_eq 'ports_needed: with TLS' ' 443 80 7882 | 7882' "$(
    CFG_TLS_MODE=ip CFG_HTTPS=:443 CFG_HTTP=:80 CFG_ICE_TCP=:7882 CFG_ICE_UDP=:7882
    ports_needed
    printf ' %s | %s' "$PORTS_TCP" "$PORTS_UDP"
  )"
  assert_eq 'ports_needed: behind a proxy' '  8080 7882 | 7882' "$(
    CFG_TLS_MODE=off CFG_HTTPS=:443 CFG_HTTP=127.0.0.1:8080 CFG_ICE_TCP=:7882 CFG_ICE_UDP=:7882
    ports_needed
    printf ' %s | %s' "$PORTS_TCP" "$PORTS_UDP"
  )"
  assert_eq 'ports_needed: UDP and ICE-TCP turned off' ' 443 80  | ' "$(
    CFG_TLS_MODE=auto CFG_HTTPS=:443 CFG_HTTP=:80 CFG_ICE_TCP='' CFG_ICE_UDP=''
    ports_needed
    printf ' %s | %s' "$PORTS_TCP" "$PORTS_UDP"
  )"
}

# ---------------------------------------------------------------------------------------------------------------
# ask, confirm

# answers LINE... makes the file that stands for the terminal, with one answer per line.
answers() {
  if [ $# -gt 0 ]; then
    printf '%s\n' "$@" >"$WORK/tty"
  else
    : >"$WORK/tty"
  fi
  TTY_DEV=$WORK/tty
  TTY_OPEN=0
}

no_terminal() {
  TTY_DEV=$WORK/no-such-terminal
  TTY_OPEN=0
}

# asked ARGS... prints "RC ANSWER" of one ask.
asked() {
  (
    ask "$@" 2>/dev/null && rc=0 || rc=$?
    printf '%s %s' "$rc" "$ANSWER"
  )
}

# confirmed ARGS... prints the return code of one confirm.
confirmed() {
  (
    confirm "$@" 2>/dev/null && rc=0 || rc=$?
    printf '%s' "$rc"
  )
}

test_ask() {
  OPT_YES=0
  answers 'share.example.com'
  assert_eq 'ask: an answer' '0 share.example.com' "$(asked 'Name?')"
  answers '   spaced out   '
  assert_eq 'ask: without the spaces around it' '0 spaced out' "$(asked 'Name?')"
  answers ''
  assert_eq 'ask: Enter takes the default' '0 fallback' "$(asked 'Name?' fallback)"
  assert_eq 'ask: Enter without a default is an empty answer' '0 ' "$(asked 'Name?')"
  answers
  assert_eq 'ask: the end of the input is a cancel' '1 ' "$(asked 'Name?' fallback)"
  answers 'first' 'second'
  assert_eq 'ask: two questions read two lines' 'first second' "$(
    ask 'One?' 2>/dev/null
    one=$ANSWER
    ask 'Two?' 2>/dev/null
    printf '%s %s' "$one" "$ANSWER"
  )"
  answers 'typed'
  assert_eq 'ask: the prompt goes to stderr, not into the output' '' "$(ask 'Name?' 2>/dev/null)"

  no_terminal
  assert_eq 'ask: without a terminal, the default' '0 fallback' "$(asked 'Name?' fallback)"
  run ask 'Name?'
  assert_eq 'ask: without a terminal and without a default' 2 "$RC"

  answers 'typed'
  OPT_YES=1
  assert_eq 'ask: with --yes the default, and nothing is read' '0 fallback' "$(asked 'Name?' fallback)"
  run ask 'Name?'
  assert_eq 'ask: with --yes and without a default' 2 "$RC"
  OPT_YES=0
}

test_confirm() {
  OPT_YES=0
  for yes in y Y yes YES Yes; do
    answers "$yes"
    assert_eq "confirm: \"$yes\" is yes" 0 "$(confirmed 'Open?' n)"
  done
  for no in n N no NO No; do
    answers "$no"
    assert_eq "confirm: \"$no\" is no" 1 "$(confirmed 'Open?' y)"
  done
  answers ''
  assert_eq 'confirm: Enter with the default yes' 0 "$(confirmed 'Open?' y)"
  assert_eq 'confirm: Enter with the default no' 1 "$(confirmed 'Open?' n)"
  answers 'maybe' 'what' 'y'
  assert_eq 'confirm: it asks again until the answer is one' 0 "$(confirmed 'Open?' n)"
  answers 'maybe'
  assert_eq 'confirm: the end of the input is a cancel' 2 "$(confirmed 'Open?' y)"
  answers 'x'
  assert_contains 'confirm: the prompt shows the default' 'Open? [Y/n]' "$(confirm 'Open?' y 2>&1 || :)"
  assert_contains 'confirm: the prompt shows the default no' 'Open? [y/N]' "$(confirm 'Open?' n 2>&1 || :)"

  no_terminal
  assert_eq 'confirm: without a terminal, the default yes' 0 "$(confirmed 'Open?' y)"
  assert_eq 'confirm: without a terminal, the default no' 1 "$(confirmed 'Open?' n)"
  answers 'n'
  OPT_YES=1
  assert_eq 'confirm: with --yes the default, whatever is typed' 0 "$(confirmed 'Open?' y)"
  OPT_YES=0
}

# ---------------------------------------------------------------------------------------------------------------
# The question and the config file

# fake_isshoni makes a program that stands for the isshoni binary: it appends its arguments to $WORK/calls, and
#   config init   creates the file of --path, or exits with the next code of $WORK/init-codes (default 0);
#   doctor        exits with the next code of $WORK/doctor-codes (default 0).
fake_isshoni() {
  BIN=$WORK/isshoni
  CONFIG=$WORK/etc/isshoni.toml
  mkdir -p "$WORK/etc"
  rm -f "$CONFIG" "$WORK/calls" "$WORK/init-codes" "$WORK/doctor-codes"
  cat >"$BIN" <<'EOF'
#!/bin/sh
work=${0%/*}
printf '%s\n' "$*" >>"$work/calls"
next_code() {
  code=0
  if [ -s "$1" ]; then
    code=$(sed -n 1p "$1")
    sed 1d "$1" >"$1.rest"
    mv "$1.rest" "$1"
  fi
  printf '%s' "$code"
}
case "$1 ${2-}" in
'config init')
  code=$(next_code "$work/init-codes")
  [ "$code" = 0 ] || exit "$code"
  [ ! -e "$4" ] || exit 7
  printf 'config\n' >"$4"
  ;;
doctor*) exit "$(next_code "$work/doctor-codes")" ;;
esac
EOF
  chmod +x "$BIN"
}

# as_root here runs the command as it is, and leaves out the change of owner, which only root can do.
as_root() {
  case $1 in
  chown) return 0 ;;
  esac
  "$@"
}

# config_calls prints the `config init` calls so far, one per line, without the path.
config_calls() {
  sed -n "s|^config init --path $CONFIG ||p" "$WORK/calls" 2>/dev/null | tr '\n' ';'
}

test_config_init() {
  fake_isshoni
  no_terminal
  OPT_YES=0

  parse_args --domain share.example.com
  run write_config
  assert_eq "write_config: --domain ($OUT)" 0 "$RC"
  assert_eq 'write_config: --domain, the flags' '--domain share.example.com;' "$(config_calls)"
  assert_eq 'write_config: the pre-check that follows' \
    "doctor --config $CONFIG --only dns,public_ip,clock,firewall_hint" "$(sed -n 2p "$WORK/calls")"
  assert_eq 'write_config: the config is there' yes "$([ -f "$CONFIG" ] && printf yes || printf no)"

  fake_isshoni
  ISSHONI_PUBLIC_IP=203.0.113.7 ISSHONI_TLS_ACME_EMAIL=admin@example.com
  parse_args --ip
  unset ISSHONI_PUBLIC_IP ISSHONI_TLS_ACME_EMAIL
  run write_config
  assert_eq 'write_config: --ip with an address and an e-mail' \
    '--tls.mode ip --public-ip 203.0.113.7 --tls.acme-email admin@example.com;' "$(config_calls)"

  fake_isshoni
  ISSHONI_PUBLIC_URL=https://share.example.com ISSHONI_LISTEN_HTTP=127.0.0.1:9000
  parse_args --tls-mode off
  unset ISSHONI_PUBLIC_URL ISSHONI_LISTEN_HTTP
  run write_config
  assert_eq 'write_config: off mode' \
    '--tls.mode off --public-url https://share.example.com --listen.http 127.0.0.1:9000;' "$(config_calls)"

  fake_isshoni
  ISSHONI_TLS_CERT_FILE=/etc/ssl/c.pem ISSHONI_TLS_KEY_FILE=/etc/ssl/k.pem
  parse_args --tls-mode manual --domain share.example.com
  unset ISSHONI_TLS_CERT_FILE ISSHONI_TLS_KEY_FILE
  run write_config
  assert_eq 'write_config: manual mode' \
    '--tls.mode manual --tls.cert-file /etc/ssl/c.pem --tls.key-file /etc/ssl/k.pem --domain share.example.com;' \
    "$(config_calls)"

  fake_isshoni
  ISSHONI_TLS_ACME_CA=https://pebble.test:14000/dir ISSHONI_TLS_ACME_CA_ROOT=/etc/pebble.pem
  parse_args --domain isshoni.test
  unset ISSHONI_TLS_ACME_CA ISSHONI_TLS_ACME_CA_ROOT
  run write_config
  assert_eq 'write_config: a test ACME server' \
    '--domain isshoni.test --tls.acme-ca https://pebble.test:14000/dir --tls.acme-ca-root /etc/pebble.pem;' \
    "$(config_calls)"

  # --yes takes the default answer of the question: the IP address.
  fake_isshoni
  parse_args --yes
  run write_config
  assert_eq "write_config: --yes alone ($OUT)" 0 "$RC"
  assert_eq 'write_config: --yes alone means the IP address' '--tls.mode ip;' "$(config_calls)"

  fake_isshoni
  parse_args
  run write_config
  assert_eq 'write_config: nobody to ask' 2 "$RC"
  assert_contains 'write_config: nobody to ask, the message' 'Non-interactive install: pass --domain NAME or --ip' "$OUT"
  assert_eq 'write_config: nobody to ask, nothing written' '' "$(config_calls)"

  # Values that isshoni refuses (exit 78) end a run that can't ask again.
  fake_isshoni
  printf '78\n' >"$WORK/init-codes"
  parse_args --domain share.example.com
  run write_config
  assert_eq 'write_config: config init refuses the flags' 2 "$RC"
  assert_eq 'write_config: config init refuses, no config' no "$([ -e "$CONFIG" ] && printf yes || printf no)"
}

test_question() {
  fake_isshoni
  OPT_YES=0
  parse_args

  answers 'Share.Example.com'
  run write_config
  assert_eq "write_config: a domain is typed ($OUT)" 0 "$RC"
  assert_eq 'write_config: a domain is typed, the flags' '--domain share.example.com;' "$(config_calls)"
  assert_contains 'write_config: the question' 'Domain name for this server (for example share.example.com).' "$OUT"
  assert_contains 'write_config: the question, second line' "Press Enter to use this server's IP address instead:" "$OUT"

  fake_isshoni
  answers ''
  run write_config
  assert_eq 'write_config: Enter means the IP address' '--tls.mode ip;' "$(config_calls)"

  fake_isshoni
  answers '203.0.113.7'
  run write_config
  assert_eq 'write_config: an IP address is typed' '--tls.mode ip --public-ip 203.0.113.7;' "$(config_calls)"

  fake_isshoni
  answers 'bad_name' 'exa mple.com' 'share.example.com'
  run write_config
  assert_eq 'write_config: the third answer is a name' '--domain share.example.com;' "$(config_calls)"

  fake_isshoni
  answers 'bad_name' 'exa mple.com' 'still bad' 'share.example.com'
  run write_config
  assert_eq 'write_config: three answers that are no names' 2 "$RC"
  assert_eq 'write_config: three bad answers, nothing written' '' "$(config_calls)"

  fake_isshoni
  answers
  run write_config
  assert_eq 'write_config: the question is cancelled' 8 "$RC"

  # isshoni refuses the first answer (exit 78, say a private address): the question comes again.
  fake_isshoni
  printf '78\n' >"$WORK/init-codes"
  answers '10.0.0.1' 'share.example.com'
  run write_config
  assert_eq "write_config: asked again after a refused answer ($OUT)" 0 "$RC"
  assert_eq 'write_config: asked again, the two calls' '--tls.mode ip --public-ip 10.0.0.1;--domain share.example.com;' \
    "$(config_calls)"
}

test_precheck() {
  OPT_YES=0
  parse_args

  # The pre-check fails, and the admin enters another domain: the first config is deleted and written again.
  fake_isshoni
  printf '5\n0\n' >"$WORK/doctor-codes"
  answers 'wrong.example.com' 'R' 'right.example.com'
  run write_config
  assert_eq "write_config: re-enter after a failed pre-check ($OUT)" 0 "$RC"
  assert_eq 'write_config: re-enter, the two calls' '--domain wrong.example.com;--domain right.example.com;' \
    "$(config_calls)"
  assert_contains 'write_config: the prompt after a failed pre-check' \
    '[R]e-enter the domain, use the [I]P instead, or [C]ontinue anyway?' "$OUT"

  fake_isshoni
  printf '5\n0\n' >"$WORK/doctor-codes"
  answers 'wrong.example.com' '' 'right.example.com'
  run write_config
  assert_eq 'write_config: Enter at the prompt is re-enter' '--domain wrong.example.com;--domain right.example.com;' \
    "$(config_calls)"

  fake_isshoni
  printf '5\n0\n' >"$WORK/doctor-codes"
  answers 'wrong.example.com' 'i'
  run write_config
  assert_eq 'write_config: the IP address instead' '--domain wrong.example.com;--tls.mode ip;' "$(config_calls)"

  fake_isshoni
  printf '5\n' >"$WORK/doctor-codes"
  answers 'wrong.example.com' 'c'
  run write_config
  assert_eq "write_config: continue anyway ($OUT)" 0 "$RC"
  assert_eq 'write_config: continue anyway keeps the config' '--domain wrong.example.com;' "$(config_calls)"
  assert_eq 'write_config: continue anyway, the config is there' yes "$([ -f "$CONFIG" ] && printf yes || printf no)"

  # A cancel at the prompt deletes the config of this run, so the next run asks again.
  fake_isshoni
  printf '5\n' >"$WORK/doctor-codes"
  answers 'wrong.example.com'
  run write_config
  assert_eq 'write_config: cancel at the prompt' 8 "$RC"
  assert_eq 'write_config: cancel at the prompt deletes the config' no "$([ -e "$CONFIG" ] && printf yes || printf no)"

  # Ctrl-C at the prompt ends the script through the cleanup of main, which deletes that config too.
  fake_isshoni
  assert_eq 'cleanup: a config that did not pass the pre-check is deleted' no "$(
    printf 'config\n' >"$CONFIG"
    CONFIG_PENDING=1
    cleanup
    [ -e "$CONFIG" ] && printf yes || printf no
  )"
  assert_eq 'cleanup: a config that passed, or was there before, is kept' yes "$(
    printf 'config\n' >"$CONFIG"
    CONFIG_PENDING=0
    cleanup
    [ -e "$CONFIG" ] && printf yes || printf no
  )"

  # --yes goes on after a failed pre-check, with a warning.
  fake_isshoni
  printf '5\n' >"$WORK/doctor-codes"
  parse_args --domain wrong.example.com --yes
  run write_config
  assert_eq "write_config: --yes after a failed pre-check ($OUT)" 0 "$RC"
  assert_contains 'write_config: --yes after a failed pre-check warns' 'Going on anyway' "$OUT"

  # Nobody to ask, and no --yes: the config is deleted, and the run ends.
  fake_isshoni
  no_terminal
  printf '5\n' >"$WORK/doctor-codes"
  parse_args --domain wrong.example.com
  run write_config
  assert_eq 'write_config: a failed pre-check and nobody to ask' 2 "$RC"
  assert_eq 'write_config: a failed pre-check and nobody to ask, no config' no \
    "$([ -e "$CONFIG" ] && printf yes || printf no)"

  # A doctor that can't run the checks (another exit code) does not stop the installation.
  fake_isshoni
  printf '2\n' >"$WORK/doctor-codes"
  parse_args --domain share.example.com
  run write_config
  assert_eq "write_config: a pre-check that could not run ($OUT)" 0 "$RC"
  parse_args
}

# ---------------------------------------------------------------------------------------------------------------
# How the script ends

# The three ways a run ends, each with the installer's exit trap in place.
end_at_failed_command() {
  trap cleanup EXIT
  step 'installing the program'
  exit "$1" # what `set -e` does when a command fails that no code handles
}

end_with_die() {
  trap cleanup EXIT
  step 'verifying the release'
  die "$1" 'A message of the installer.' 'What to do next.'
}

end_well() {
  trap cleanup EXIT
  step 'making the setup link'
}

test_exit_codes() {
  TMP=''
  # A tool's own exit code must not be read as one of the installer's: 3 is "verification failed" there.
  run end_at_failed_command 3
  assert_eq 'exit: a command that failed is exit 1, whatever its own code' 1 "$RC"
  assert_contains 'exit: a command that failed, the step' \
    'The installer stopped at an unexpected error while installing the program' "$OUT"
  assert_contains 'exit: a command that failed, what to do' 'run this installer again' "$OUT"
  run end_at_failed_command 127
  assert_eq 'exit: a command that is missing is exit 1' 1 "$RC"

  for code in 2 3 4 6 7 8; do
    run end_with_die "$code"
    assert_eq "exit: die $code keeps its code" "$code" "$RC"
    assert_eq "exit: die $code prints its own message only" \
      'Error: A message of the installer.
What to do next.' "$OUT"
  done

  run end_well
  assert_eq 'exit: a run that ends well' "0 " "$RC $OUT"

  # The download directory goes away however the run ends.
  for how in 'end_at_failed_command 1' 'end_with_die 3' end_well; do
    TMP=$WORK/download-dir
    mkdir -p "$TMP/dl"
    # shellcheck disable=SC2086 # a function and its argument
    run $how
    assert_eq "exit: the download directory is removed ($how)" no "$([ -e "$WORK/download-dir" ] && printf yes || printf no)"
  done
  TMP=''
}

test_backup_lookup() {
  DATA_DIR=$WORK/data
  TMP=$WORK/run
  mkdir -p "$DATA_DIR/backups" "$TMP"
  # The run began in June; a backup of an upgrade in January is there already.
  : >"$TMP/started"
  touch -t 202606010000 "$TMP/started"
  : >"$DATA_DIR/backups/pre-2-20260101T000000Z.db"
  touch -t 202601010000 "$DATA_DIR/backups/pre-2-20260101T000000Z.db"
  assert_eq 'newest_premigration_backup: a backup of an earlier upgrade is not offered' '' \
    "$(newest_premigration_backup)"

  : >"$DATA_DIR/backups/pre-3-20260601T000100Z.db"
  touch -t 202606010001 "$DATA_DIR/backups/pre-3-20260601T000100Z.db"
  assert_eq 'newest_premigration_backup: the backup of this run' "$DATA_DIR/backups/pre-3-20260601T000100Z.db" \
    "$(newest_premigration_backup)"

  : >"$DATA_DIR/backups/pre-4-20260601T000200Z.db"
  touch -t 202606010002 "$DATA_DIR/backups/pre-4-20260601T000200Z.db"
  : >"$DATA_DIR/backups/other-file.db"
  assert_eq 'newest_premigration_backup: the newest of two' "$DATA_DIR/backups/pre-4-20260601T000200Z.db" \
    "$(newest_premigration_backup)"

  DATA_DIR=$WORK/no-such-data
  assert_eq 'newest_premigration_backup: no backup directory' '' "$(newest_premigration_backup)"
  TMP=''
}

# ---------------------------------------------------------------------------------------------------------------
# iptables

# iptables here prints a recorded INPUT chain for -S and records every other call.
iptables() {
  case "$1 ${2-}" in
  '-S INPUT') printf '%s\n' "$IPTABLES_RULES" ;;
  *) printf '%s\n' "$*" >>"$WORK/iptables-calls" ;;
  esac
}

test_iptables() {
  HAVE_ALSO=iptables
  # The INPUT chain of an Oracle Cloud Ubuntu image.
  IPTABLES_RULES='-P INPUT ACCEPT
-A INPUT -m state --state RELATED,ESTABLISHED -j ACCEPT
-A INPUT -p icmp -j ACCEPT
-A INPUT -i lo -j ACCEPT
-A INPUT -p tcp -m state --state NEW -m tcp --dport 22 -j ACCEPT
-A INPUT -j REJECT --reject-with icmp-host-prohibited'
  rm -f "$WORK/iptables-calls"
  assert_eq 'iptables_blocks: a REJECT rule' 0 "$(iptables_blocks iptables && printf 0 || printf 1)"
  iptables_open iptables
  assert_eq 'iptables_open: both rules go before the REJECT rule, tagged isshoni' \
    '-I INPUT 5 -p udp --dport 7882 -m comment --comment isshoni -j ACCEPT
-I INPUT 5 -p tcp -m multiport --dports 80,443,7882 -m comment --comment isshoni -j ACCEPT' \
    "$(cat "$WORK/iptables-calls")"

  IPTABLES_RULES='-P INPUT DROP
-A INPUT -i lo -j ACCEPT'
  rm -f "$WORK/iptables-calls"
  assert_eq 'iptables_blocks: a DROP policy' 0 "$(iptables_blocks iptables && printf 0 || printf 1)"
  iptables_open iptables
  assert_eq 'iptables_open: under a DROP policy the rules are appended' \
    '-A INPUT -p tcp -m multiport --dports 80,443,7882 -m comment --comment isshoni -j ACCEPT
-A INPUT -p udp --dport 7882 -m comment --comment isshoni -j ACCEPT' \
    "$(cat "$WORK/iptables-calls")"

  IPTABLES_RULES='-P INPUT ACCEPT
-A INPUT -p tcp -m multiport --dports 80,443,7882 -m comment --comment isshoni -j ACCEPT
-A INPUT -p udp -m udp --dport 7882 -m comment --comment isshoni -j ACCEPT
-A INPUT -j DROP'
  rm -f "$WORK/iptables-calls"
  iptables_open iptables
  assert_eq 'iptables_open: rules that are there are not added again' no \
    "$([ -e "$WORK/iptables-calls" ] && printf yes || printf no)"

  IPTABLES_RULES='-P INPUT ACCEPT
-A INPUT -p tcp -m tcp --dport 22 -j ACCEPT'
  assert_eq 'iptables_blocks: an open chain' 1 "$(iptables_blocks iptables && printf 0 || printf 1)"
  HAVE_ALSO=''
  HAVE_NOT="$HAVE_NOT iptables"
  assert_eq 'iptables_blocks: no iptables' 1 "$(iptables_blocks iptables && printf 0 || printf 1)"
  HAVE_NOT='cosign gh'
}

# ---------------------------------------------------------------------------------------------------------------
# The script file

test_script_file() {
  # The release pipeline stamps this exact line (task release:prepare).
  assert_eq 'install.sh: one line to stamp the version into' 1 \
    "$(grep -c '^ISSHONI_INSTALLER_VERSION="@VERSION@"$' "$INSTALL_SH")"
  # A download that was cut off must not run a part of the script: main is called from the last line only.
  # shellcheck disable=SC2016 # the line as it is written in the script
  assert_eq 'install.sh: the last line calls main' '[ "${ISSHONI_INSTALL_SOURCED:-0}" = 1 ] || main "$@"' \
    "$(tail -n 1 "$INSTALL_SH")"
  assert_eq 'install.sh: main is called nowhere else' 1 "$(grep -c 'main "\$@"' "$INSTALL_SH")"

  # The key block: one BEGIN line, one END line, and between them ALLOWED_SIGNERS='...' and nothing else.
  assert_eq 'install.sh: one BEGIN ALLOWED SIGNERS line' 1 "$(grep -c '^# BEGIN ALLOWED SIGNERS' "$INSTALL_SH")"
  assert_eq 'install.sh: one END ALLOWED SIGNERS line' 1 "$(grep -c '^# END ALLOWED SIGNERS' "$INSTALL_SH")"
  block=$(sed -n '/^# BEGIN ALLOWED SIGNERS/,/^# END ALLOWED SIGNERS/p' "$INSTALL_SH" | sed '1d;$d')
  assert_eq 'install.sh: the key block is the value that the script uses' "ALLOWED_SIGNERS='$SCRIPT_SIGNERS'" "$block"

  keys=$HERE/../keys/allowed_signers
  if [ -f "$keys" ]; then
    # The installer writes the value and a newline to a file for ssh-keygen: that file must be the key file.
    printf '%s\n' "$SCRIPT_SIGNERS" >"$WORK/embedded_signers"
    assert_eq 'install.sh: the key block is deploy/keys/allowed_signers, byte for byte' \
      "$(sha256_of "$keys")" "$(sha256_of "$WORK/embedded_signers")"
  else
    printf 'note: deploy/keys/allowed_signers does not exist yet, so the key block is only checked for its form\n'
    assert_eq 'install.sh: without a key file, the key block is empty' '' "$SCRIPT_SIGNERS"
  fi
}

# ---------------------------------------------------------------------------------------------------------------

# The key block as install.sh has it; the verify_release cases put their own key in its place.
SCRIPT_SIGNERS=$ALLOWED_SIGNERS

test_script_file
test_parse_args_flags
test_parse_args_env
test_parse_args_errors
test_scrub_env
test_detect_arch
test_preflight
test_normalize_domain
test_version_cmp
test_resolve_version
test_checksum_for
test_verify_release
test_check_ports
test_ask
test_confirm
test_config_init
test_question
test_precheck
test_exit_codes
test_backup_lookup
test_iptables

printf '%s passed, %s failed\n' "$PASSED" "$FAILED"
[ "$FAILED" -eq 0 ]
