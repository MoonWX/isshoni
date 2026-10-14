#!/bin/sh
# isshoni server installer (docs/m1/06-deploy-and-ci.md §4).
#
#   curl -fsSL https://moonwx.github.io/isshoni/install.sh | sh
#   curl -fsSL https://moonwx.github.io/isshoni/install.sh | sh -s -- --domain share.example.com --yes
#
# It installs the isshoni server on a Linux machine with systemd, asks one question (the domain name), starts the
# service and prints the link that creates the admin account. Run it again to upgrade, to repair an installation,
# or to finish one that stopped half way; `--uninstall` and `--purge` remove it again. `--help` lists the options
# and the exit codes.
#
# The script is thin on purpose. The program, the systemd unit, the sysctl file and the firewall profiles all come
# from the release archive, and nothing from it is used before the archive passed verification: the release's
# checksums.txt must carry a signature of a key embedded below, and the archive must have the checksum listed
# there. There is no way to switch that off or to pass another key.
#
# POSIX sh: it runs under dash, bash --posix and busybox sh. The whole body is in functions and the last line calls
# main, so a download that was cut off never runs half a script, and the unit tests (deploy/test/install_unit.sh)
# can source it.
set -eu

# The release pipeline replaces the placeholder with the release's version (task release:prepare). A copy without
# a version, like the one in the repository, installs the latest release.
ISSHONI_INSTALLER_VERSION="@VERSION@"
INSTALLER_VERSION=$ISSHONI_INSTALLER_VERSION

# The keys that may sign a release's checksums.txt, in ssh-keygen's allowed_signers format. An empty block is a
# development copy: it can verify nothing, so it installs nothing.
# BEGIN ALLOWED SIGNERS (generated from deploy/keys/allowed_signers; do not edit here)
ALLOWED_SIGNERS=''
# END ALLOWED SIGNERS

REPO_URL=https://github.com/MoonWX/isshoni
SITE_URL=https://moonwx.github.io/isshoni
SIGN_IDENTITY=isshoni-release
SIGN_NAMESPACE=isshoni-checksums

BIN=/usr/local/bin/isshoni
UNIT_DIR=/usr/local/lib/systemd/system
UNIT=$UNIT_DIR/isshoni.service
CONFIG_DIR=/etc/isshoni
CONFIG=$CONFIG_DIR/isshoni.toml
DATA_DIR=/var/lib/isshoni
RUN_DIR=/run/isshoni
DROPIN_DIR=/etc/systemd/system/isshoni.service.d # the admin's `systemctl edit isshoni`
SYSCTL_FILE=/etc/sysctl.d/60-isshoni.conf
UFW_PROFILE=/etc/ufw/applications.d/isshoni
FIREWALLD_DIR=/etc/firewalld # zones/ and policies/ there hold what was changed on this machine
FIREWALLD_SERVICE=$FIREWALLD_DIR/services/isshoni.xml
PACKAGE_BIN=/usr/bin/isshoni    # where the deb and rpm packages put the program
SYSTEMD_DIR=/run/systemd/system # there when systemd is the init system
BUFFER_BYTES=8388608            # both values of deploy/sysctl/60-isshoni.conf

# Where questions are read from: stdin is the script itself under `curl | sh`. The unit tests point it at a file.
TTY_DEV=/dev/tty

# State. Everything is global (POSIX sh has no local variables), so a function's own variables carry a prefix made
# of its initials.
SUDO=''          # "sudo" when the script does not run as root
TMP=''           # the download directory, removed on exit
TTY_OPEN=0       # 1 once file descriptor 3 reads from $TTY_DEV
DL=''            # curl or wget
ARCH=''          # amd64 or arm64
PKG=''           # apt, dnf or empty
IV=''            # the installed version, empty when there is none
TV=''            # the version to install
ARCHIVE=''       # the archive's file name
STARTED=0        # 1 when the unit is enabled: an earlier run got as far as the start
BIN_CHANGED=0    # 1 when this run replaced the program
UNIT_CHANGED=0   # 1 when this run replaced the systemd unit
CONFIG_PENDING=0 # 1 while a config this run wrote has not passed the pre-check: a cancel deletes it
ANSWER=''        # the answer to the last question
STEP='starting'  # what the installer is doing, for the message when a command fails unexpectedly
PLANNED_EXIT=0   # 1 when the script ends with one of its own exit codes, not at a command that failed
REMOVED=0        # what an uninstall removed, counted
FW_ONLINE=''     # an uninstall's way to firewalld: 1 the daemon, 0 its files (firewall-offline-cmd)
FW_REMOVED=''    # the zones and policies an uninstall took the isshoni service out of
FW_FAILED=''     # the ones firewalld refused to change
OPT_ACTION=install
OPT_YES=0
CFG_TLS_MODE=''
# Colors, for stdout (C_) and stderr (E_): empty unless setup_colors turns them on.
C_BOLD=''
C_RESET=''
E_RED=''
E_YELLOW=''
E_RESET=''

# ---------------------------------------------------------------------------------------------------------------
# Output and questions

# say prints its arguments on stdout, one per line.
say() {
  printf '%s\n' "$@"
}

# warn prints a warning on stderr; further arguments are further lines.
warn() {
  printf '%sWarning:%s %s\n' "$E_YELLOW" "$E_RESET" "$1" >&2
  shift
  [ $# -eq 0 ] || printf '         %s\n' "$@" >&2
}

# die CODE MSG... prints an error on stderr (further arguments are further lines, which say what to do next) and
# exits with CODE, one of the codes of `--help`.
die() {
  die_code=$1
  shift
  printf '%sError:%s %s\n' "$E_RED" "$E_RESET" "$1" >&2
  shift
  [ $# -eq 0 ] || printf '%s\n' "$@" >&2
  PLANNED_EXIT=1
  exit "$die_code"
}

# step TEXT notes what the installer does next. cleanup names it when a command fails that nothing here expected
# to fail.
step() {
  STEP=$1
}

# setup_colors turns colors on for a stream that is a terminal, unless NO_COLOR is set.
setup_colors() {
  [ -z "${NO_COLOR:-}" ] || return 0
  if [ -t 1 ]; then
    C_BOLD=$(printf '\033[1m')
    C_RESET=$(printf '\033[0m')
  fi
  if [ -t 2 ]; then
    E_RED=$(printf '\033[31m')
    E_YELLOW=$(printf '\033[33m')
    E_RESET=$(printf '\033[0m')
  fi
}

# have reports whether a command is installed.
have() {
  command -v "$1" >/dev/null 2>&1
}

# as_root runs a command as root: directly when the script is root, through sudo otherwise.
as_root() {
  if [ -n "$SUDO" ]; then
    "$SUDO" "$@"
  else
    "$@"
  fi
}

# have_tty reports whether there is a terminal to ask on. /dev/tty exists without one, but can't be opened.
have_tty() {
  [ "$TTY_OPEN" = 1 ] || (exec <"$TTY_DEV") 2>/dev/null
}

# ask PROMPT [DEFAULT] puts the answer to a question into $ANSWER, without the spaces around it.
#   - With --yes, or without a terminal, the answer is DEFAULT; a question without a default then ends the
#     script (exit 2).
#   - An empty answer (Enter) is DEFAULT too.
#   - It returns 1 at the end of the input (Ctrl-D), which callers treat as a cancel.
ask() {
  if [ "$OPT_YES" = 1 ] || ! have_tty; then
    [ $# -ge 2 ] || die 2 "This needs an answer, and there is no terminal to ask on (or --yes was given): $1" \
      'Run the installer in a terminal, without --yes.'
    ANSWER=$2
    return 0
  fi
  if [ "$TTY_OPEN" = 0 ]; then
    exec 3<"$TTY_DEV"
    TTY_OPEN=1
  fi
  printf '%s ' "$1" >&2
  if ! read -r ANSWER <&3; then
    printf '\n' >&2
    ANSWER=''
    return 1
  fi
  if [ -z "$ANSWER" ] && [ $# -ge 2 ]; then
    ANSWER=$2
  fi
}

# confirm PROMPT y|n asks a yes/no question with that default. It returns 0 for yes, 1 for no, and 2 at the end of
# the input (a cancel).
confirm() {
  case $2 in
  y) cf_hint='[Y/n]' ;;
  *) cf_hint='[y/N]' ;;
  esac
  while :; do
    ask "$1 $cf_hint" "$2" || return 2
    case $ANSWER in
    [Yy] | [Yy][Ee][Ss]) return 0 ;;
    [Nn] | [Nn][Oo]) return 1 ;;
    esac
    printf '%s\n' 'Please answer y or n.' >&2
  done
}

usage() {
  cat <<EOF
isshoni installer$(installer_version_text)

Usage:
  curl -fsSL $SITE_URL/install.sh | sh
  curl -fsSL $SITE_URL/install.sh | sh -s -- [options]

Installs isshoni, or upgrades, repairs or finishes an installation that is already there.

Options (and the environment variable that does the same):
  --domain NAME      ISSHONI_DOMAIN         Get a certificate for NAME; skips the question
  --ip               ISSHONI_TLS_MODE=ip    Get a certificate for the public IP address; skips the question
  --tls-mode MODE    ISSHONI_TLS_MODE       Advanced: off (behind your own HTTPS proxy; needs ISSHONI_PUBLIC_URL)
                                            or manual (your certificate: ISSHONI_TLS_CERT_FILE and
                                            ISSHONI_TLS_KEY_FILE). auto and ip are accepted too
  --version V        ISSHONI_VERSION        Install version V (0.1.0, v0.1.0 or latest)
  --yes, -y          ISSHONI_YES=1          Never ask; take every default, which includes opening firewall ports
  --no-firewall      ISSHONI_NO_FIREWALL=1  Never touch the firewall
  --firewall                                Offer the firewall step again on an upgrade
  --no-start                                Install only; run the installer again later to start isshoni
  --allow-downgrade                         Allow an older version than the installed one
  --reinstall                               Install the same version again (repairs the program file)
  --uninstall                               Remove isshoni; keeps its configuration, data and user
  --purge                                   --uninstall, then delete the configuration, the data and the user
  --help, -h                                Show this help

More environment variables:
  ISSHONI_PUBLIC_IP        This server's public IP address, instead of detecting it
  ISSHONI_PUBLIC_URL       The URL friends open, for --tls-mode off (https://your.domain)
  ISSHONI_TLS_ACME_EMAIL   An optional contact address for the Let's Encrypt account
  ISSHONI_DOWNLOAD_BASE    A mirror of $REPO_URL/releases
                           (the release signature is still checked against the key in this script)

A flag wins over its environment variable.

Exit codes:
  0  done, or already up to date          5  installed, but isshoni did not become ready in time
  1  unexpected error                     6  downgrade refused (see --allow-downgrade)
  2  bad usage                            7  a port isshoni needs is used by another program
  3  verification failed; nothing was     8  cancelled
     installed or changed
  4  unsupported system: not Linux, no systemd, no build for this CPU, no root or sudo, installed
     from a package, or an OpenSSH older than 8.1

Guide: $SITE_URL/install/
EOF
}

# installer_version_text is the stamped version for the first line of the help, or nothing for an unstamped copy.
installer_version_text() {
  case $INSTALLER_VERSION in
  '' | @*) ;;
  *) printf ' %s' "$INSTALLER_VERSION" ;;
  esac
}

# ---------------------------------------------------------------------------------------------------------------
# Arguments and environment

# truthy reports whether an environment value means yes.
truthy() {
  case $1 in
  1 | [Tt][Rr][Uu][Ee] | [Yy][Ee][Ss]) return 0 ;;
  esac
  return 1
}

# valid_version reports whether its argument is a release version as it appears in tags and asset names: SemVer
# without the leading "v" and without build metadata. It also keeps anything else out of URLs and file names.
valid_version() {
  printf '%s\n' "$1" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'
}

# parse_args reads the flags and their environment equivalents into the OPT_ variables. A flag wins over its
# environment variable. Bad usage ends the script with exit 2; --help prints the usage and exits 0.
parse_args() {
  OPT_ACTION=install
  OPT_DOMAIN=''
  OPT_TLS_MODE='' # empty: not chosen, the installer asks its question
  OPT_VERSION=''  # empty: the stamped version, or the latest release
  OPT_YES=0
  OPT_NO_FIREWALL=0
  OPT_FIREWALL=0
  OPT_NO_START=0
  OPT_ALLOW_DOWNGRADE=0
  OPT_REINSTALL=0
  pa_domain=''
  pa_mode=''
  pa_ip=0
  pa_version=''
  pa_has_domain=0
  pa_value=''

  while [ $# -gt 0 ]; do
    pa_arg=$1
    shift
    case $pa_arg in
    --domain | --tls-mode | --version)
      [ $# -ge 1 ] || die 2 "$pa_arg needs a value." 'Run the installer with --help to see the options.'
      pa_value=$1
      shift
      ;;
    --domain=* | --tls-mode=* | --version=*)
      pa_value=${pa_arg#*=}
      pa_arg=${pa_arg%%=*}
      ;;
    esac
    case $pa_arg in
    --domain)
      pa_domain=$pa_value
      pa_has_domain=1
      ;;
    --tls-mode)
      [ -n "$pa_value" ] || die 2 '--tls-mode needs a value: off or manual (auto and ip work too).'
      pa_mode=$pa_value
      ;;
    --version)
      [ -n "$pa_value" ] || die 2 '--version needs a value, like 0.1.0 or latest.'
      pa_version=$pa_value
      ;;
    --ip) pa_ip=1 ;;
    --yes | -y) OPT_YES=1 ;;
    --no-firewall) OPT_NO_FIREWALL=1 ;;
    --firewall) OPT_FIREWALL=1 ;;
    --no-start) OPT_NO_START=1 ;;
    --allow-downgrade) OPT_ALLOW_DOWNGRADE=1 ;;
    --reinstall) OPT_REINSTALL=1 ;;
    --uninstall) [ "$OPT_ACTION" = purge ] || OPT_ACTION=uninstall ;;
    --purge) OPT_ACTION=purge ;;
    --help | -h)
      usage
      exit 0
      ;;
    *) die 2 "Unknown option: $pa_arg" 'Run the installer with --help to see the options.' ;;
    esac
  done

  # Environment equivalents, where no flag decided.
  [ "$OPT_YES" = 1 ] || ! truthy "${ISSHONI_YES:-}" || OPT_YES=1
  [ "$OPT_NO_FIREWALL" = 1 ] || ! truthy "${ISSHONI_NO_FIREWALL:-}" || OPT_NO_FIREWALL=1
  [ -n "$pa_version" ] || pa_version=${ISSHONI_VERSION:-}
  OPT_PUBLIC_IP=${ISSHONI_PUBLIC_IP:-}
  OPT_PUBLIC_URL=${ISSHONI_PUBLIC_URL:-}
  OPT_ACME_EMAIL=${ISSHONI_TLS_ACME_EMAIL:-}
  OPT_CERT_FILE=${ISSHONI_TLS_CERT_FILE:-}
  OPT_KEY_FILE=${ISSHONI_TLS_KEY_FILE:-}
  OPT_LISTEN_HTTP=${ISSHONI_LISTEN_HTTP:-}
  # These two are for tests against a private ACME server (Pebble) only.
  OPT_ACME_CA=${ISSHONI_TLS_ACME_CA:-}
  OPT_ACME_CA_ROOT=${ISSHONI_TLS_ACME_CA_ROOT:-}
  RELEASES_BASE=${ISSHONI_DOWNLOAD_BASE:-$REPO_URL/releases}
  RELEASES_BASE=${RELEASES_BASE%/}

  [ "$OPT_FIREWALL" = 0 ] || [ "$OPT_NO_FIREWALL" = 0 ] ||
    die 2 '--firewall and --no-firewall (or ISSHONI_NO_FIREWALL) contradict each other.' 'Pass only one of them.'

  if [ -n "$pa_version" ]; then
    OPT_VERSION=${pa_version#v}
    [ "$OPT_VERSION" = latest ] || valid_version "$OPT_VERSION" ||
      die 2 "\"$pa_version\" is not a version." 'Use a version like 0.1.0 or v0.1.0, or "latest".'
  fi

  # The rest is about the config of a new installation, which --uninstall and --purge do not write.
  [ "$OPT_ACTION" = install ] || return 0

  # The TLS choice. --ip is --tls-mode ip.
  [ "$pa_has_domain" = 0 ] || [ -n "$pa_domain" ] || die 2 '--domain needs a name, like --domain share.example.com.'
  if [ "$pa_ip" = 1 ]; then
    [ "$pa_has_domain" = 0 ] || die 2 '--ip and --domain contradict each other.' \
      'Use --domain NAME for a certificate for a name, or --ip for one for the IP address.'
    [ -z "$pa_mode" ] || [ "$pa_mode" = ip ] ||
      die 2 "--ip and --tls-mode $pa_mode contradict each other." 'Pass only one of them.'
    pa_mode=ip
  fi
  if [ -n "$pa_mode" ] || [ "$pa_has_domain" = 1 ]; then
    # Flags decide. What the environment says about the other half is used only where it fits them: ip and off
    # take no domain, and a domain goes with auto or manual.
    OPT_TLS_MODE=$pa_mode
    OPT_DOMAIN=$pa_domain
    if [ "$pa_has_domain" = 0 ]; then
      case $pa_mode in
      ip | off) ;;
      *) OPT_DOMAIN=${ISSHONI_DOMAIN:-} ;;
      esac
    elif [ -z "$pa_mode" ] && [ "${ISSHONI_TLS_MODE:-}" = manual ]; then
      OPT_TLS_MODE=manual
    fi
  else
    OPT_TLS_MODE=${ISSHONI_TLS_MODE:-}
    OPT_DOMAIN=${ISSHONI_DOMAIN:-}
  fi

  case $OPT_TLS_MODE in
  '' | auto | ip | off | manual) ;;
  *) die 2 "\"$OPT_TLS_MODE\" is not a TLS mode." 'Use --tls-mode off or --tls-mode manual (auto and ip work too).' ;;
  esac

  if [ -n "$OPT_DOMAIN" ]; then
    normalize_domain "$OPT_DOMAIN" || :
    case $NORM_KIND in
    domain) OPT_DOMAIN=$NORM_VALUE ;;
    ip)
      die 2 "\"$OPT_DOMAIN\" is an IP address, not a domain name." \
        "For a certificate for the IP address use --ip (ISSHONI_PUBLIC_IP=$NORM_VALUE sets the address)."
      ;;
    *) die 2 "\"$OPT_DOMAIN\" is not a domain name." 'Write only the name, like share.example.com.' ;;
    esac
    [ -n "$OPT_TLS_MODE" ] || OPT_TLS_MODE=auto
  fi

  case $OPT_TLS_MODE in
  auto)
    [ -n "$OPT_DOMAIN" ] || die 2 'auto mode needs a domain name: pass --domain NAME.' \
      'Without a domain name, use --ip for a certificate for the IP address.'
    ;;
  ip)
    [ -z "$OPT_DOMAIN" ] || die 2 "ip mode and the domain name $OPT_DOMAIN contradict each other." \
      'Use --domain NAME for a certificate for a name, or --ip for one for the IP address.'
    ;;
  off)
    [ -n "$OPT_PUBLIC_URL" ] || die 2 'off mode needs ISSHONI_PUBLIC_URL=https://your.domain' \
      "Example: curl -fsSL $SITE_URL/install.sh | ISSHONI_PUBLIC_URL=https://share.example.com sh -s -- --tls-mode off"
    [ -z "$OPT_DOMAIN" ] || die 2 "off mode and the domain name $OPT_DOMAIN contradict each other." \
      'Behind your own proxy, the address friends open is ISSHONI_PUBLIC_URL; leave the domain out.'
    ;;
  manual)
    if [ -z "$OPT_CERT_FILE" ] || [ -z "$OPT_KEY_FILE" ]; then
      die 2 'manual mode needs ISSHONI_TLS_CERT_FILE and ISSHONI_TLS_KEY_FILE (the PEM files of your certificate).' \
        "Guide: $SITE_URL/install/tls"
    fi
    ;;
  esac
}

# scrub_env removes every ISSHONI_ variable from the environment, after parse_args copied what the installer
# reads. `isshoni config init`, doctor, healthcheck and setup-url then see the config file alone, never a stray
# variable of the admin's shell.
scrub_env() {
  unset ISSHONI_DOMAIN ISSHONI_TLS_MODE ISSHONI_VERSION ISSHONI_YES ISSHONI_NO_FIREWALL ISSHONI_PUBLIC_IP \
    ISSHONI_PUBLIC_URL ISSHONI_TLS_ACME_EMAIL ISSHONI_TLS_CERT_FILE ISSHONI_TLS_KEY_FILE ISSHONI_LISTEN_HTTP \
    ISSHONI_TLS_ACME_CA ISSHONI_TLS_ACME_CA_ROOT ISSHONI_DOWNLOAD_BASE ISSHONI_INSTALL_SOURCED \
    ISSHONI_INSTALLER_VERSION
  for se_name in $(env | sed -n 's/^\(ISSHONI_[A-Za-z0-9_]*\)=.*/\1/p'); do
    unset "$se_name"
  done
}

# normalize_domain cleans up what someone typed for the domain question and says what it is: it sets NORM_KIND to
# domain, ip, empty or invalid, and NORM_VALUE to the name or the address. It returns 1 for invalid.
#   "HTTPS://Share.Example.com/x/" is the domain share.example.com; "203.0.113.7" and "2001:db8::1" are addresses.
normalize_domain() {
  NORM_KIND=invalid
  nd=$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]' | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')
  NORM_VALUE=$nd
  nd=${nd#http://}
  nd=${nd#https://}
  nd=${nd%%[/?#]*}
  if [ -z "$nd" ]; then
    # Nothing at all means "use the IP address"; a bare "https://" is not an answer.
    [ -z "$NORM_VALUE" ] || return 1
    NORM_KIND=empty
    return 0
  fi
  # An IPv6 address may come in brackets, as in a URL, and then with a port. A name or an IPv4 address may have a
  # port too.
  case $nd in
  \[*\])
    nd=${nd#\[}
    nd=${nd%\]}
    ;;
  \[*\]:*)
    nd_port=${nd##*\]:}
    nd=${nd#\[}
    nd=${nd%\]:*}
    case $nd_port in '' | *[!0-9]*) return 1 ;; esac
    ;;
  *:*:*) ;; # an IPv6 address without brackets: its colons are not a port
  *:*)
    nd_port=${nd##*:}
    nd=${nd%:*}
    case $nd_port in '' | *[!0-9]*) return 1 ;; esac
    ;;
  esac
  nd=${nd%.}
  NORM_VALUE=$nd
  case $nd in
  '' | *[!0-9a-z.:-]*) return 1 ;;
  *:*)
    # IPv6: at least two colons, hex groups, optionally an IPv4 tail. `isshoni config init` checks it for real.
    case $nd in
    *[!0-9a-f:.]* | *:::*) return 1 ;;
    *:*:*) NORM_KIND=ip ;;
    *) return 1 ;;
    esac
    ;;
  *[!0-9.]*)
    [ ${#nd} -le 253 ] || return 1
    printf '%s\n' "$nd" |
      grep -Eq '^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+([a-z]{2,63}|xn--[a-z0-9-]{1,59})$' || return 1
    NORM_KIND=domain
    ;;
  *)
    # Only digits and dots: an IPv4 address, or nothing.
    printf '%s\n' "$nd" | awk -F. '
      NF != 4 { exit 1 }
      { for (i = 1; i <= 4; i++) if ($i !~ /^[0-9]+$/ || length($i) > 3 || $i + 0 > 255) exit 1 }' || return 1
    NORM_KIND=ip
    ;;
  esac
  return 0
}

# ---------------------------------------------------------------------------------------------------------------
# The system

# detect_arch [MACHINE] sets ARCH from `uname -m`; a machine isshoni has no build for is exit 4.
detect_arch() {
  da_machine=${1:-$(uname -m)}
  case $da_machine in
  x86_64 | amd64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *)
    die 4 "isshoni has no build for this CPU architecture ($da_machine)." \
      'It runs on x86_64 (amd64) and on aarch64 (arm64) machines.'
    ;;
  esac
}

# detect_distro sets PKG to the package manager this installer can use for a missing tool: apt or dnf, or empty.
detect_distro() {
  PKG=''
  if have apt-get; then
    PKG=apt
  elif have dnf; then
    PKG=dnf
  fi
}

# preflight checks that this is a machine the installer supports, before anything is downloaded or changed.
preflight() {
  if [ "$(id -u)" -ne 0 ]; then
    have sudo || die 4 'This installer needs root, and sudo is not installed.' \
      'Run it as root, or install sudo first.'
    SUDO=sudo
  fi
  pf_os=$(uname -s)
  [ "$pf_os" = Linux ] || die 4 "This installer runs on Linux only (this is $pf_os)." \
    "On other systems, run isshoni with Docker: $SITE_URL/install/docker"
  detect_arch
  [ -d "$SYSTEMD_DIR" ] || die 4 'This system does not run systemd, which the installer needs.' \
    "In a container, or on a system without systemd, run isshoni with Docker: $SITE_URL/install/docker"
  detect_distro
  check_not_packaged
  for pf_tool in tar awk sed grep tr install mktemp systemctl; do
    have "$pf_tool" ||
      die 4 "The installer needs the \"$pf_tool\" command, which is missing here." \
        'Install it with your package manager, then run this installer again.'
  done
  if [ -n "$SUDO" ]; then
    # Ask for the password once, now, and not in the middle of the installation.
    sudo -n true 2>/dev/null || sudo -v || die 4 'sudo did not give this installer root.' \
      'Run it as root, or as a user who may use sudo.'
  fi
}

# check_not_packaged refuses to work next to the deb or rpm package: the package manager owns that installation.
check_not_packaged() {
  [ -e "$PACKAGE_BIN" ] || return 0
  if have dpkg && dpkg -S "$PACKAGE_BIN" >/dev/null 2>&1; then
    np_tool=apt
  elif have rpm && rpm -qf "$PACKAGE_BIN" >/dev/null 2>&1; then
    np_tool=dnf
  else
    return 0
  fi
  if [ "$OPT_ACTION" = install ]; then
    die 4 "isshoni was installed here from a package, so $np_tool manages it, not this installer." \
      "Upgrade it with the newer package from $REPO_URL/releases (sudo $np_tool install ./<file>)," \
      "or remove the package first (sudo $np_tool remove isshoni) and run this installer again."
  fi
  die 4 "isshoni was installed here from a package, so $np_tool manages it, not this installer." \
    "Remove it with: sudo $np_tool remove isshoni"
}

# downloader sets DL to curl or wget; without either the installer can't download (exit 4).
downloader() {
  [ -z "$DL" ] || return 0
  if have curl; then
    DL=curl
  elif have wget; then
    DL=wget
  else
    die 4 'The installer needs curl or wget to download isshoni, and neither is installed.' \
      'Install one of them (for example: sudo apt-get install curl), then run this installer again.'
  fi
}

# quietly runs a command and shows what it printed only when it fails.
quietly() {
  q_rc=0
  q_out=$("$@" 2>&1) || q_rc=$?
  [ "$q_rc" -eq 0 ] || printf '%s\n' "$q_out" >&2
  return "$q_rc"
}

# apt_install installs Debian packages without questions and without recommended extras.
apt_install() {
  quietly as_root env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "$@"
}

# need_ssh_keygen makes sure ssh-keygen is there, which verifies the release signature. Minimal systems lack it
# (a VPS has it: sshd brings it), so the installer installs the OpenSSH client package itself.
need_ssh_keygen() {
  ! have ssh-keygen || return 0
  case $PKG in
  apt)
    say 'Installing openssh-client to verify the download'
    if ! apt_install openssh-client 2>/dev/null; then
      # A fresh image has no package lists yet: update them and try once more.
      as_root apt-get update >/dev/null 2>&1 || :
      apt_install openssh-client || :
    fi
    ;;
  dnf)
    say 'Installing openssh-clients to verify the download'
    quietly as_root dnf install -y openssh-clients || :
    ;;
  esac
  have ssh-keygen ||
    die 4 'The installer needs ssh-keygen (OpenSSH 8.1 or newer) to verify the download, and could not install it.' \
      'Install the OpenSSH client package of your distribution, then run this installer again.'
}

# download URL FILE fetches URL into FILE, over HTTPS only.
download() {
  case $DL in
  curl)
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL --connect-timeout 10 --max-time 300 --retry 3 \
      --retry-delay 2 -o "$2" "$1"
    ;;
  *) wget --https-only --timeout=10 --tries=3 -q -O "$2" "$1" ;;
  esac
}

# sha256_of FILE prints the file's SHA-256 in lower-case hex, or nothing when no tool for it is there.
sha256_of() {
  if have sha256sum; then
    sha256sum "$1" | awk '{ print $1 }'
  elif have shasum; then
    shasum -a 256 "$1" | awk '{ print $1 }'
  elif have openssl; then
    openssl dgst -sha256 -r "$1" | awk '{ print $1 }'
  fi
}

# same_file A B reports whether B exists with the content of A.
same_file() {
  [ -f "$2" ] || return 1
  sf_a=$(sha256_of "$1")
  sf_b=$(sha256_of "$2")
  [ -n "$sf_a" ] && [ "$sf_a" = "$sf_b" ]
}

# ---------------------------------------------------------------------------------------------------------------
# Versions

# installed_version sets IV to the version of the installed program, or to nothing.
installed_version() {
  IV=''
  [ -x "$BIN" ] || return 0
  IV=$("$BIN" version --short 2>/dev/null) || IV=''
  # A development build carries build metadata (0.0.0-dev+abc123), which a version comparison ignores.
  valid_version "${IV%%+*}" || IV=''
}

# latest_url prints where $RELEASES_BASE/latest redirects to, which ends in /tag/v<version>. The GitHub API is not
# used: it is rate-limited.
latest_url() {
  case $DL in
  curl)
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSLI -o /dev/null -w '%{url_effective}' \
      --connect-timeout 10 --max-time 60 --retry 3 --retry-delay 2 "$RELEASES_BASE/latest"
    ;;
  *)
    # wget stops at the redirect (and so fails); the Location header is in what it printed.
    wget --https-only --timeout=10 --tries=3 -S --spider --max-redirect=0 "$RELEASES_BASE/latest" 2>&1 |
      sed -n 's/^[[:space:]]*[Ll]ocation:[[:space:]]*\([^[:space:]]*\).*/\1/p' | sed -n 1p
    ;;
  esac
}

# version_from_url prints the version of a release URL: ".../releases/tag/v1.2.3" is 1.2.3. It fails for a URL
# that names no version.
version_from_url() {
  case $1 in
  */tag/v*) ;;
  *) return 1 ;;
  esac
  vu=${1##*/tag/v}
  vu=${vu%%[/?#]*}
  vu=$(printf '%s' "$vu" | tr -d '\r')
  valid_version "$vu" || return 1
  printf '%s\n' "$vu"
}

# resolve_version sets TV, the version to install: --version, else ISSHONI_VERSION, else the version stamped into
# this script, else the latest release.
resolve_version() {
  TV=$OPT_VERSION
  if [ -z "$TV" ]; then
    case $INSTALLER_VERSION in
    '' | @*) TV=latest ;;
    *) TV=${INSTALLER_VERSION#v} ;;
    esac
  fi
  if [ "$TV" = latest ]; then
    downloader
    rv_url=$(latest_url) || rv_url=''
    TV=$(version_from_url "$rv_url") ||
      die 1 "Could not find out the latest version of isshoni from $RELEASES_BASE/latest" \
        'Check the network connection and run this installer again, or name a version: --version 0.1.0'
  fi
  valid_version "$TV" || die 1 "This installer carries a version it can't use ($TV)." \
    "Get the installer again from $SITE_URL/install.sh, or name a version: --version 0.1.0"
}

# version_cmp A B prints -1, 0 or 1 as A is older than, the same as or newer than B, by SemVer's rules: X.Y.Z as
# numbers; a prerelease is older than its release; prerelease identifiers compare as numbers when both are numbers
# and as text otherwise, a number before text, and fewer identifiers before more; build metadata is ignored.
# (`sort -V` gets 0.2.0 against 0.2.0-rc.1 wrong.)
version_cmp() {
  awk -v a="$1" -v b="$2" '
    function isnum(s) { return s ~ /^[0-9]+$/ }
    function cmp(x, y) { return x < y ? -1 : (x > y ? 1 : 0) }
    function prerelease(s,   i) {
      i = index(s, "-")
      return i ? substr(s, i + 1) : ""
    }
    BEGIN {
      sub(/^v/, "", a); sub(/^v/, "", b)
      sub(/\+.*$/, "", a); sub(/\+.*$/, "", b)
      pa = prerelease(a); pb = prerelease(b)
      sub(/-.*$/, "", a); sub(/-.*$/, "", b)
      split(a, na, "."); split(b, nb, ".")
      for (i = 1; i <= 3; i++) {
        r = cmp(na[i] + 0, nb[i] + 0)
        if (r) { print r; exit }
      }
      if (pa == "" || pb == "") { print cmp(pa == "", pb == ""); exit }
      la = split(pa, ia, "."); lb = split(pb, ib, ".")
      for (i = 1; i <= la && i <= lb; i++) {
        if (isnum(ia[i]) && isnum(ib[i])) r = cmp(ia[i] + 0, ib[i] + 0)
        else if (isnum(ia[i])) r = -1
        else if (isnum(ib[i])) r = 1
        else r = cmp(ia[i] "", ib[i] "")
        if (r) { print r; exit }
      }
      print cmp(la, lb)
    }'
}

# ---------------------------------------------------------------------------------------------------------------
# Download and verification (fail closed)

# checksum_for NAME FILE prints the checksum that FILE, a checksums.txt, lists for NAME. The name must be the
# whole name of a line and appear exactly once; anything else fails.
checksum_for() {
  awk -v f="$1" '
    $2 == f || $2 == "*" f { n++; sum = $1 }
    END { if (n != 1) exit 1; print sum }' "$2"
}

# verify_fail ends the script after a failed download or verification (exit 3). Nothing was changed up to here.
verify_fail() {
  die 3 "$@" 'Nothing was installed or changed.' "If this keeps happening, see $SITE_URL/security#verify"
}

# need_key ends the script when this copy of the installer has no key to verify a release with: the copy in the
# repository, before a release stamps it.
need_key() {
  [ -n "$ALLOWED_SIGNERS" ] ||
    verify_fail 'This copy of the installer carries no release signing key: it is a development copy.' \
      "It can verify no release, so it installs none. Get the installer from $SITE_URL/install.sh"
}

# fetch_release DIR downloads the archive of TV for ARCH and the files that prove it into DIR. cosign's bundle is
# fetched only where cosign is installed to check it.
fetch_release() {
  fr_base=$RELEASES_BASE/download/v$TV
  say "Downloading isshoni $TV ($ARCH)"
  for fr_file in checksums.txt checksums.txt.sig "$ARCHIVE"; do
    download "$fr_base/$fr_file" "$1/$fr_file" ||
      verify_fail "Could not download $fr_base/$fr_file" \
        "Check the network connection, and that version $TV exists: $RELEASES_BASE"
  done
  if have cosign; then
    download "$fr_base/checksums.txt.sigstore.json" "$1/checksums.txt.sigstore.json" ||
      verify_fail "Could not download $fr_base/checksums.txt.sigstore.json" \
        'cosign is installed here, so the installer checks the Sigstore bundle too.'
  fi
}

# verify_release DIR ARCHIVE proves the downloaded archive in DIR, in this order, and ends the script with exit 3
# at the first step that fails:
#   1. checksums.txt carries a signature of a key in ALLOWED_SIGNERS (ssh-keygen -Y verify);
#   2. checksums.txt lists ARCHIVE exactly once, with the SHA-256 the file has;
#   3. where cosign is installed: the Sigstore bundle proves checksums.txt came from the release workflow;
#   4. where gh is installed and logged in: GitHub's build attestation for the archive.
verify_release() {
  vr_dir=$1
  vr_archive=$2
  need_key
  for vr_file in checksums.txt checksums.txt.sig "$vr_archive"; do
    [ -s "$vr_dir/$vr_file" ] || verify_fail "The download is incomplete: $vr_file is missing or empty."
  done

  printf '%s\n' "$ALLOWED_SIGNERS" >"$vr_dir/allowed_signers"
  vr_rc=0
  vr_out=$(ssh-keygen -Y verify -f "$vr_dir/allowed_signers" -I "$SIGN_IDENTITY" -n "$SIGN_NAMESPACE" \
    -s "$vr_dir/checksums.txt.sig" <"$vr_dir/checksums.txt" 2>&1) || vr_rc=$?
  if [ "$vr_rc" -ne 0 ]; then
    case $vr_out in
    *'option -- Y'* | *'unknown option'* | *'illegal option'*)
      die 4 'The OpenSSH on this system is older than 8.1 and cannot verify the download (no ssh-keygen -Y verify).' \
        'Nothing was installed or changed. Upgrade OpenSSH, or run isshoni with Docker:' "  $SITE_URL/install/docker"
      ;;
    esac
    verify_fail 'The signature of checksums.txt is not valid: this is not a release the isshoni project signed.' \
      "ssh-keygen says: $(printf '%s\n' "$vr_out" | sed -n 1p)"
  fi

  vr_want=$(checksum_for "$vr_archive" "$vr_dir/checksums.txt") ||
    verify_fail "checksums.txt does not list $vr_archive exactly once."
  vr_want=$(printf '%s' "$vr_want" | tr '[:upper:]' '[:lower:]')
  vr_got=$(sha256_of "$vr_dir/$vr_archive")
  [ -n "$vr_got" ] || verify_fail 'No tool that computes SHA-256 was found (sha256sum, shasum or openssl).' \
    'Install coreutils or openssl, then run this installer again.'
  [ "$vr_got" = "$vr_want" ] ||
    verify_fail "The checksum of $vr_archive does not match checksums.txt: the download is damaged or was changed." \
      "  expected $vr_want" "  got      $vr_got"

  if have cosign; then
    cosign verify-blob --bundle "$vr_dir/checksums.txt.sigstore.json" \
      --certificate-identity "$REPO_URL/.github/workflows/release.yml@refs/tags/v$TV" \
      --certificate-oidc-issuer https://token.actions.githubusercontent.com \
      "$vr_dir/checksums.txt" >/dev/null 2>&1 ||
      verify_fail 'cosign could not verify checksums.txt against the release workflow of the isshoni repository.'
  fi
  # gh needs a token for attestations, so this step runs only where gh is logged in.
  if have gh && gh auth status >/dev/null 2>&1; then
    gh attestation verify "$vr_dir/$vr_archive" --repo MoonWX/isshoni >/dev/null 2>&1 ||
      verify_fail "GitHub has no valid build attestation for $vr_archive."
  fi
}

# unpack_release DIR ARCHIVE unpacks the verified archive into DIR/x and checks that it holds what the installer
# installs.
unpack_release() {
  mkdir -p "$1/x"
  tar -xzf "$1/$2" -C "$1/x" --no-same-owner || die 1 "Could not unpack $2." \
    'Check the free space in the temporary directory, then run this installer again.'
  for ur_file in isshoni deploy/systemd/isshoni.service deploy/sysctl/60-isshoni.conf deploy/firewall/ufw-isshoni \
    deploy/firewall/firewalld-isshoni.xml; do
    [ -f "$1/x/$ur_file" ] || die 1 "The archive $2 has no $ur_file, which this installer needs." \
      "Use the installer of that release: $RELEASES_BASE/download/v$TV/install.sh"
  done
}

# ---------------------------------------------------------------------------------------------------------------
# Installing the files

# relabel restores the SELinux label of a path where the SELinux tools are installed.
relabel() {
  if have restorecon; then
    as_root restorecon "$@" 2>/dev/null || :
  fi
}

# make_dir DIR creates a system directory that is missing (root, 0755). One that exists keeps its owner and mode:
# /usr/local/bin and its like are not the installer's to change. Root looks whether it exists: an ordinary user
# can't see into every directory (/etc/firewalld is root's alone on Fedora).
make_dir() {
  as_root test -d "$1" || as_root install -d -m 0755 -o root -g root "$1"
}

# install_binary DIR puts the program from DIR/x in place. It is installed next to its final path and run there
# before it replaces the old one: /tmp may be mounted noexec, and `install` creates a new file, which gets the
# right SELinux label (a file moved out of /tmp would keep its own, and systemd could not run it).
install_binary() {
  ib_new=${BIN%/*}/.isshoni.new
  make_dir "${BIN%/*}"
  as_root install -m 0755 -o root -g root "$1/x/isshoni" "$ib_new" || die 1 "Could not write to ${BIN%/*}." \
    'Check that the file system is writable and has room, then run this installer again.'
  ib_got=$("$ib_new" version --short 2>/dev/null) || ib_got=''
  if [ "$ib_got" != "$TV" ]; then
    as_root rm -f "$ib_new"
    die 1 'The downloaded program does not run here.' \
      "It answered \"$ib_got\" for its version, not \"$TV\". The installed version, if any, was not changed." \
      "Please report this: $REPO_URL/issues"
  fi
  as_root mv -f "$ib_new" "$BIN"
  relabel "$BIN"
  BIN_CHANGED=1
}

# ensure_user creates the isshoni system group and user, and the configuration and data directories. systemd
# would create the directories at the first start; having them now lets `config init` write the config before it.
ensure_user() {
  if ! getent group isshoni >/dev/null 2>&1; then
    as_root groupadd --system isshoni || die 1 'Could not create the isshoni group.' \
      'Check that groupadd works (package "passwd" on Debian and Ubuntu, "shadow-utils" on Fedora).'
  fi
  if ! getent passwd isshoni >/dev/null 2>&1; then
    as_root useradd --system --gid isshoni --home-dir "$DATA_DIR" --no-create-home \
      --shell "$(command -v nologin || echo /bin/false)" --comment 'isshoni server' isshoni ||
      die 1 'Could not create the isshoni user.' \
        'Check that useradd works (package "passwd" on Debian and Ubuntu, "shadow-utils" on Fedora).'
  fi
  as_root install -d -m 0755 -o root -g root "$CONFIG_DIR"
  as_root install -d -m 0700 -o isshoni -g isshoni "$DATA_DIR"
}

# install_unit DIR installs the systemd unit from DIR/x, when it differs from the installed one.
install_unit() {
  if same_file "$1/x/deploy/systemd/isshoni.service" "$UNIT"; then
    return 0
  fi
  make_dir "$UNIT_DIR"
  as_root install -m 0644 -o root -g root "$1/x/deploy/systemd/isshoni.service" "$UNIT"
  relabel "$UNIT"
  UNIT_CHANGED=1
}

# sysctl_low KEY reports whether net.core.KEY is below what isshoni asks for. A value that can't be read counts
# as low.
sysctl_low() {
  sl=$(cat "/proc/sys/net/core/$1" 2>/dev/null) || sl=''
  case $sl in
  '' | *[!0-9]*) return 0 ;;
  esac
  [ "$sl" -lt "$BUFFER_BYTES" ]
}

# apply_sysctl DIR raises the UDP buffer limits for media: it installs the release's 60-isshoni.conf and applies
# it, with the keys whose running value is lower. A limit the admin has set higher is never lowered, now or at the
# next boot, so a key that is high enough is left out of the installed file; when both are, no file is written.
apply_sysctl() {
  as_keep=''
  for as_key in rmem_max wmem_max; do
    if sysctl_low "$as_key"; then
      as_keep="$as_keep $as_key"
    fi
  done
  [ -n "$as_keep" ] || return 0
  as_tmp=$1/60-isshoni.conf
  cp "$1/x/deploy/sysctl/60-isshoni.conf" "$as_tmp"
  for as_key in rmem_max wmem_max; do
    case $as_keep in
    *" $as_key"*) ;;
    *)
      sed "/^[[:space:]]*net\.core\.${as_key}[[:space:]]*=/d" "$as_tmp" >"$as_tmp.new"
      mv -f "$as_tmp.new" "$as_tmp"
      ;;
    esac
  done
  as_root install -m 0644 -o root -g root "$as_tmp" "$SYSCTL_FILE"
  # sysctl exits 0 even when the kernel refuses a value (in a container, for one), so the values are read back.
  as_root sysctl -p "$SYSCTL_FILE" >/dev/null 2>&1 || :
  if sysctl_low rmem_max || sysctl_low wmem_max; then
    warn "Could not apply $SYSCTL_FILE (larger UDP buffers for video)." \
      'In a container that is expected. "sudo isshoni doctor" shows the sizes in use.'
  fi
}

# install_fw_profiles DIR installs the named firewall profiles for the firewalls that are installed, active or
# not. offer_firewall uses them, and an admin who turns a firewall on later finds them there.
install_fw_profiles() {
  if have ufw && as_root test -d "${UFW_PROFILE%/*}"; then
    as_root install -m 0644 -o root -g root "$1/x/deploy/firewall/ufw-isshoni" "$UFW_PROFILE"
  fi
  if have firewall-cmd; then
    make_dir "${FIREWALLD_SERVICE%/*}"
    as_root install -m 0644 -o root -g root "$1/x/deploy/firewall/firewalld-isshoni.xml" "$FIREWALLD_SERVICE"
    relabel "$FIREWALLD_SERVICE"
  fi
}

# payload_complete reports whether everything the installer takes from the release archive is in place, so a run
# that changes no version can go on without downloading the archive again. Root looks for the files, as in
# make_dir: to an ordinary user a file in a directory that is root's alone would always be missing.
payload_complete() {
  as_root test -f "$UNIT" || return 1
  if sysctl_low rmem_max || sysctl_low wmem_max; then
    as_root test -f "$SYSCTL_FILE" || return 1
  fi
  if have ufw && as_root test -d "${UFW_PROFILE%/*}"; then
    as_root test -f "$UFW_PROFILE" || return 1
  fi
  if have firewall-cmd; then
    as_root test -f "$FIREWALLD_SERVICE" || return 1
  fi
  return 0
}

# ---------------------------------------------------------------------------------------------------------------
# The one question, and the config file

# ask_domain asks the installer's one question and sets wc_mode (auto or ip), wc_domain and wc_ip from the
# answer. With --yes the answer is the default, the IP address. Without a terminal there is nobody to ask (exit 2).
# An answer that is not a name is asked again, three times at most.
ask_domain() {
  if [ "$OPT_YES" = 1 ]; then
    wc_mode=ip
    return 0
  fi
  have_tty || die 2 'Non-interactive install: pass --domain NAME or --ip'
  ad_tries=0
  while :; do
    printf '%s\n' '' 'Domain name for this server (for example share.example.com).' >&2
    if ! ask "Press Enter to use this server's IP address instead:"; then
      die 8 'Cancelled.' 'Nothing was started. Run this installer again to go on.'
    fi
    if normalize_domain "$ANSWER"; then
      case $NORM_KIND in
      domain)
        wc_mode=auto
        wc_domain=$NORM_VALUE
        ;;
      ip)
        wc_mode=ip
        wc_ip=$NORM_VALUE
        ;;
      *) wc_mode=ip ;;
      esac
      return 0
    fi
    ad_tries=$((ad_tries + 1))
    [ "$ad_tries" -lt 3 ] || die 2 "\"$ANSWER\" is not a domain name." \
      'Run this installer again with --domain NAME (like share.example.com), or with --ip.'
    printf '%s\n' "\"$ANSWER\" is not a domain name. Write only the name, like share.example.com." >&2
  done
}

# config_init MODE DOMAIN IP writes the config file through `isshoni config init`, with flags only: the installer
# never writes TOML itself. It returns that command's exit code: 0 written, 78 the values are not valid and
# nothing was written.
config_init() {
  ci_mode=$1
  ci_domain=$2
  ci_ip=$3
  set -- config init --path "$CONFIG"
  case $ci_mode in
  auto) set -- "$@" --domain "$ci_domain" ;;
  ip) set -- "$@" --tls.mode ip ;;
  off)
    set -- "$@" --tls.mode off --public-url "$OPT_PUBLIC_URL"
    [ -z "$OPT_LISTEN_HTTP" ] || set -- "$@" --listen.http "$OPT_LISTEN_HTTP"
    ;;
  manual)
    set -- "$@" --tls.mode manual --tls.cert-file "$OPT_CERT_FILE" --tls.key-file "$OPT_KEY_FILE"
    [ -z "$ci_domain" ] || set -- "$@" --domain "$ci_domain"
    ;;
  esac
  [ -z "$ci_ip" ] || set -- "$@" --public-ip "$ci_ip"
  [ -z "$OPT_ACME_EMAIL" ] || set -- "$@" --tls.acme-email "$OPT_ACME_EMAIL"
  [ -z "$OPT_ACME_CA" ] || set -- "$@" --tls.acme-ca "$OPT_ACME_CA"
  [ -z "$OPT_ACME_CA_ROOT" ] || set -- "$@" --tls.acme-ca-root "$OPT_ACME_CA_ROOT"
  ci_rc=0
  as_root "$BIN" "$@" || ci_rc=$?
  return "$ci_rc"
}

# config_exists reports whether there is a config file. Root looks: the file belongs to root and the isshoni
# group, and an admin may have closed its directory to everybody else.
config_exists() {
  as_root test -e "$CONFIG"
}

# drop_pending_config deletes the config file this run wrote, while it has not passed the pre-check. A file that
# was there before the run is never touched: the installer writes one only when there is none.
drop_pending_config() {
  if [ "$CONFIG_PENDING" = 1 ]; then
    CONFIG_PENDING=0
    as_root rm -f "$CONFIG" || :
  fi
}

# doctor_precheck runs the checks that can save a failed first start: DNS, the public address, the clock, and
# the provider's firewall steps. doctor prints its own report. It returns 0 to go on and 5 when a check failed.
doctor_precheck() {
  say '' 'Checking this server before the first start:'
  dp_rc=0
  as_root "$BIN" doctor --config "$CONFIG" --only dns,public_ip,clock,firewall_hint || dp_rc=$?
  case $dp_rc in
  0 | 5) return "$dp_rc" ;;
  esac
  warn "The check could not run (isshoni doctor exited with $dp_rc); going on without it."
  return 0
}

# write_config makes /etc/isshoni/isshoni.toml on a machine that has none: it takes the TLS choice from the flags
# or asks the one question, lets `isshoni config init` write the file, and runs the pre-check. When the pre-check
# fails, the admin can enter the domain again, switch to the IP address or go on anyway; the first two delete the
# file this run wrote and start over, and a cancel deletes it too (exit 8).
write_config() {
  wc_mode=$OPT_TLS_MODE
  wc_domain=$OPT_DOMAIN
  wc_ip=$OPT_PUBLIC_IP
  wc_asked=0
  while :; do
    if [ -z "$wc_mode" ]; then
      ask_domain
      wc_asked=1
    fi
    # There is no config (do_install looked), and config init never overwrites one: from here until the pre-check
    # has passed, a file at that path is this run's, and an interrupted run deletes it.
    CONFIG_PENDING=1
    wc_rc=0
    config_init "$wc_mode" "$wc_domain" "$wc_ip" || wc_rc=$?
    case $wc_rc in
    0) ;;
    78)
      # config init said what is wrong and wrote nothing.
      CONFIG_PENDING=0
      if [ "$wc_asked" = 1 ] && [ "$OPT_YES" = 0 ] && have_tty; then
        wc_mode=''
        wc_domain=''
        wc_ip=$OPT_PUBLIC_IP
        continue
      fi
      die 2 'isshoni does not accept these settings (see above).' 'Correct them and run this installer again.'
      ;;
    7)
      # Somebody else wrote a config meanwhile: it is not this run's to delete.
      CONFIG_PENDING=0
      die 1 "$CONFIG appeared while the installer ran." 'Run this installer again: it keeps an existing config.'
      ;;
    *) die 1 "Could not write $CONFIG (isshoni config init exited with $wc_rc)." 'See the message above.' ;;
    esac
    as_root chown root:isshoni "$CONFIG"
    as_root chmod 0640 "$CONFIG"

    if doctor_precheck; then
      break
    fi
    if [ "$OPT_YES" = 1 ]; then
      warn 'A check failed (see above). Going on anyway, because of --yes.'
      break
    fi
    if ! have_tty; then
      drop_pending_config
      die 2 'A check failed (see above), and there is no terminal to ask what to do.' \
        'Fix the problem and run this installer again, or pass --yes to install anyway.'
    fi
    case $wc_mode in
    auto | ip)
      if ! ask '[R]e-enter the domain, use the [I]P instead, or [C]ontinue anyway?' R; then
        drop_pending_config
        die 8 'Cancelled.' 'Nothing was started. Run this installer again to go on.'
      fi
      ;;
    *)
      # off and manual were chosen with flags: there is no domain question to go back to.
      if confirm 'Continue anyway?' n; then
        ANSWER=C
      else
        drop_pending_config
        die 8 'Cancelled.' 'Nothing was started. Fix the problem above, then run this installer again.'
      fi
      ;;
    esac
    case $ANSWER in
    [Cc]*) break ;;
    [Ii]*)
      drop_pending_config
      wc_mode=ip
      wc_domain=''
      wc_ip=$OPT_PUBLIC_IP
      ;;
    *)
      drop_pending_config
      wc_mode=''
      wc_domain=''
      wc_ip=$OPT_PUBLIC_IP
      ;;
    esac
  done
  CONFIG_PENDING=0
}

# load_config reads from the config file what the steps after it need: the effective TLS mode, the listen
# addresses and the address friends open. The values come from `isshoni config print`, so the installer never
# parses TOML. deploy/test/testdata/config-print has samples of that output, which the unit tests read.
load_config() {
  lc_dump=$(as_root "$BIN" config print --config "$CONFIG" 2>/dev/null) || :
  CFG_TLS_MODE=$(printf '%s\n' "$lc_dump" | sed -n 's/^# effective tls\.mode: \([a-z]*\).*/\1/p' | sed -n 1p)
  CFG_DOMAIN=$(config_string domain)
  CFG_PUBLIC_URL=$(config_string public_url)
  CFG_HTTPS=$(config_string 'listen\.https')
  CFG_HTTP=$(config_string 'listen\.http')
  CFG_ICE_UDP=$(config_string 'listen\.ice_udp')
  CFG_ICE_TCP=$(config_string 'listen\.ice_tcp')
  if [ -z "$CFG_TLS_MODE" ]; then
    # `config print` failed, or printed something this script does not find the mode in: then the other values
    # are not to be trusted either. The server says what is wrong with a config when it starts; the port check
    # uses the default ports.
    warn "Could not read the settings in $CONFIG (\"isshoni config print\" did not show them)." \
      'The installer goes on and checks the default ports: 80, 443 and 7882.'
    CFG_HTTPS=:443
    CFG_HTTP=:80
    CFG_ICE_UDP=:7882
    CFG_ICE_TCP=:7882
  fi
}

# config_string KEY prints the string value of KEY (a regular expression) in the dump of load_config.
config_string() {
  printf '%s\n' "$lc_dump" | sed -n "s/^$1 = \"\\([^\"]*\\)\".*/\\1/p" | sed -n 1p
}

# port_of prints the port of a listen address: ":443", "127.0.0.1:8080" and "[::1]:8080" give 443, 8080 and 8080;
# an empty address (the listener is off) gives nothing.
port_of() {
  case $1 in
  *:*) printf '%s\n' "${1##*:}" ;;
  esac
}

# ---------------------------------------------------------------------------------------------------------------
# Ports and the firewall

# port_conflict PROTO PORTS reads `ss -Hltnp` or `ss -Hlunp` output on stdin and prints "PORT PROTO PID PROGRAM"
# for the first listener on one of the space-separated PORTS that is not isshoni itself. PID and PROGRAM are "-"
# when ss did not tell (it shows the programs of other users to root only).
port_conflict() {
  awk -v proto="$1" -v ports="$2" '
    BEGIN { n = split(ports, want, " ") }
    {
      port = ""
      for (i = 1; i <= NF; i++) {
        if ($i ~ /:[0-9]+$/) { port = $i; sub(/^.*:/, "", port); break }
      }
      if (port == "") next
      wanted = 0
      for (j = 1; j <= n; j++) if (want[j] == port) wanted = 1
      if (!wanted) next
      prog = ""; pid = "-"
      if (match($0, /users:\(\("[^"]*"/)) prog = substr($0, RSTART + 9, RLENGTH - 10)
      if (match($0, /pid=[0-9]+/)) pid = substr($0, RSTART + 4, RLENGTH - 4)
      if (prog == "isshoni") next
      if (prog == "") prog = "-"
      print port, proto, pid, prog
      exit
    }'
}

# check_ports TCP_PORTS UDP_PORTS ends the script with exit 7 when another program listens on a port isshoni
# needs. isshoni's own listeners (an upgrade, a restart) are fine.
check_ports() {
  if ! have ss; then
    warn 'The "ss" command is missing (package iproute2), so the installer cannot check that the ports are free.'
    return 0
  fi
  cp_hit=$(as_root ss -Hltnp 2>/dev/null | port_conflict tcp "$1") || cp_hit=''
  if [ -z "$cp_hit" ]; then
    cp_hit=$(as_root ss -Hlunp 2>/dev/null | port_conflict udp "$2") || cp_hit=''
  fi
  [ -n "$cp_hit" ] || return 0

  # shellcheck disable=SC2086 # the words of port_conflict's line; the program's name is last and may be several
  set -- $cp_hit
  cp_port=$1
  cp_proto=$2
  cp_pid=$3
  shift 3
  cp_prog=$*
  if [ "$cp_prog" = - ]; then
    cp_who='another program'
    cp_stop='that program'
  else
    cp_who=$cp_prog
    cp_stop=$cp_prog
    [ "$cp_pid" = - ] || cp_who="$cp_prog (pid $cp_pid)"
  fi
  case $cp_port in
  80 | 443)
    if [ "$CFG_TLS_MODE" != off ]; then
      die 7 "Port $cp_port is used by $cp_who. isshoni needs ports 80 and 443." \
        "Either stop $cp_stop, or run isshoni behind it:" \
        "  curl -fsSL $SITE_URL/install.sh | ISSHONI_PUBLIC_URL=https://share.example.com sh -s -- --tls-mode off" \
        "Guide: $SITE_URL/install/reverse-proxy" \
        'Then run this installer again.'
    fi
    ;;
  esac
  die 7 "Port $cp_port/$cp_proto is used by $cp_who. isshoni needs that port." \
    "Stop $cp_stop, or change the port in the listen section of $CONFIG." \
    'Then run this installer again.'
}

# ports_needed sets PORTS_TCP and PORTS_UDP from the config: 80, 443 and 7882, or with tls.mode off the HTTP port
# and 7882. A listener that the config turns off needs no port.
ports_needed() {
  PORTS_TCP=''
  if [ "$CFG_TLS_MODE" != off ]; then
    PORTS_TCP=$(port_of "$CFG_HTTPS")
  fi
  PORTS_TCP="$PORTS_TCP $(port_of "$CFG_HTTP") $(port_of "$CFG_ICE_TCP")"
  PORTS_UDP=$(port_of "$CFG_ICE_UDP")
}

# offer_firewall opens isshoni's ports in the firewall that is active on this machine, after asking (the default
# is yes, and --yes takes it). A firewall that is installed but not active is never turned on: that could lock the
# admin out of SSH. It ends with the reminder about the provider's own firewall, which no script can open.
offer_firewall() {
  of_ask='Open ports 80, 443 (TCP) and 7882 (TCP+UDP)'
  of_cmd='sudo firewall-cmd --reload && sudo firewall-cmd --permanent --add-service=isshoni && sudo firewall-cmd --reload'
  of_rc=0
  if [ "$OPT_NO_FIREWALL" = 1 ]; then
    say 'Firewall: not touched (--no-firewall). isshoni needs ports 80, 443 (TCP) and 7882 (TCP+UDP).'
  elif have ufw && as_root ufw status 2>/dev/null | grep -q '^Status: active'; then
    confirm "$of_ask in ufw?" y || of_rc=$?
    case $of_rc in
    0)
      if as_root ufw allow isshoni >/dev/null; then
        say 'Firewall: opened ports 80, 443 (TCP) and 7882 (TCP+UDP) in ufw.'
      else
        warn 'ufw did not open the ports.' 'Open them yourself: sudo ufw allow isshoni'
      fi
      ;;
    1) say 'Firewall: ufw not changed. To open the ports later: sudo ufw allow isshoni' ;;
    *) die 8 'Cancelled.' 'Nothing was started. Run this installer again to go on.' ;;
    esac
  elif firewalld_running; then
    confirm "$of_ask in firewalld?" y || of_rc=$?
    case $of_rc in
    0)
      # A running firewalld reads its service files when it starts and when it reloads, so it does not know the
      # isshoni.xml that install_fw_profiles put in place moments ago, and refuses --add-service=isshoni until a
      # reload. The second reload applies the permanent change to the running firewall.
      if as_root firewall-cmd --reload >/dev/null &&
        as_root firewall-cmd --permanent --add-service=isshoni >/dev/null &&
        as_root firewall-cmd --reload >/dev/null; then
        say 'Firewall: opened ports 80, 443 (TCP) and 7882 (TCP+UDP) in firewalld.'
      else
        warn 'firewalld did not open the ports.' "Open them yourself: $of_cmd"
      fi
      ;;
    1) say 'Firewall: firewalld not changed. To open the ports later:' "  $of_cmd" ;;
    *) die 8 'Cancelled.' 'Nothing was started. Run this installer again to go on.' ;;
    esac
  elif have netfilter-persistent && iptables_blocks iptables; then
    confirm "$of_ask in iptables?" y || of_rc=$?
    case $of_rc in
    0)
      if iptables_open iptables && { ! iptables_blocks ip6tables || iptables_open ip6tables; } &&
        as_root netfilter-persistent save >/dev/null 2>&1; then
        say 'Firewall: opened ports 80, 443 (TCP) and 7882 (TCP+UDP) in iptables, and saved the rules.'
      else
        warn 'Could not add or save the iptables rules.' \
          'Open TCP 80, 443 and 7882 and UDP 7882 yourself, before the first REJECT or DROP rule of the INPUT chain.'
      fi
      ;;
    1) say 'Firewall: iptables not changed. isshoni needs ports 80, 443 (TCP) and 7882 (TCP+UDP).' ;;
    *) die 8 'Cancelled.' 'Nothing was started. Run this installer again to go on.' ;;
    esac
  else
    say 'Firewall: no active firewall found on this machine, nothing to open here.'
  fi
  say 'If your provider has a cloud firewall (AWS, Google Cloud, Azure, Oracle, ...), open the same ports there:' \
    "  $SITE_URL/install/vps"
}

# firewalld_running reports whether firewalld runs. systemd is asked first: without a daemon to talk to,
# firewalld 2.4's firewall-cmd waits 10 seconds before it answers, on every machine where firewalld is installed
# and not running (Fedora's). The packaging scripts ask in the same order.
firewalld_running() {
  systemctl is-active --quiet firewalld.service 2>/dev/null &&
    [ "$(as_root firewall-cmd --state 2>/dev/null)" = running ]
}

# iptables_blocks CMD reports whether the INPUT chain of iptables or ip6tables rejects or drops traffic: by a
# rule (the pattern of Oracle Cloud's Ubuntu images) or by its policy.
iptables_blocks() {
  have "$1" || return 1
  as_root "$1" -S INPUT 2>/dev/null | grep -Eq -- '^-P INPUT DROP|-j (REJECT|DROP)( |$)'
}

# iptables_open CMD inserts isshoni's two accept rules before the first REJECT or DROP rule of the INPUT chain,
# or appends them when only the policy drops. The rules carry the comment "isshoni", which is how an uninstall
# finds them; a chain that has them already is left alone.
iptables_open() {
  io_rules=$(as_root "$1" -S INPUT 2>/dev/null) || return 1
  case $io_rules in
  *'--comment isshoni'*) return 0 ;;
  esac
  io_pos=$(printf '%s\n' "$io_rules" | awk '
    /^-A INPUT / { n++; if ($0 ~ /-j (REJECT|DROP)( |$)/) { print n; found = 1; exit } }
    END { if (!found) print 0 }')
  if [ "$io_pos" -gt 0 ]; then
    as_root "$1" -I INPUT "$io_pos" -p udp --dport 7882 -m comment --comment isshoni -j ACCEPT &&
      as_root "$1" -I INPUT "$io_pos" -p tcp -m multiport --dports 80,443,7882 -m comment --comment isshoni -j ACCEPT
  else
    as_root "$1" -A INPUT -p tcp -m multiport --dports 80,443,7882 -m comment --comment isshoni -j ACCEPT &&
      as_root "$1" -A INPUT -p udp --dport 7882 -m comment --comment isshoni -j ACCEPT
  fi
}

# ---------------------------------------------------------------------------------------------------------------
# Start, wait, setup link

# is_started reports whether the unit is enabled, which means an earlier run got as far as the start. It decides
# between a first start and an upgrade or repair; the config file and the installed version never do.
is_started() {
  systemctl is-enabled --quiet isshoni 2>/dev/null
}

# start_or_restart HOW starts the service: "first" enables and starts it, "restart" restarts it after an upgrade,
# and "ensure" starts it only when it is not running (a repair). A start that fails does not end the script here:
# wait_ready notices, and the diagnostics after it say why.
start_or_restart() {
  as_root systemctl daemon-reload
  case $1 in
  first)
    case $CFG_TLS_MODE in
    auto | ip)
      say '' "isshoni gets its certificate from Let's Encrypt. Using it means you accept the Let's Encrypt" \
        'Subscriber Agreement: https://letsencrypt.org/repository/'
      ;;
    esac
    say '' 'Starting isshoni'
    if ! as_root systemctl enable --now isshoni >/dev/null 2>&1; then
      is_started || die 1 'Could not enable the isshoni service.' \
        'See "systemctl status isshoni", fix the problem, then run this installer again.'
    fi
    ;;
  restart)
    say '' 'Restarting isshoni'
    as_root systemctl reset-failed isshoni >/dev/null 2>&1 || :
    as_root systemctl restart isshoni >/dev/null 2>&1 || :
    ;;
  *)
    if ! systemctl is-active --quiet isshoni 2>/dev/null; then
      say '' 'Starting isshoni'
      # A unit that exited 78, or ran into its start limit, starts again only after a reset.
      as_root systemctl reset-failed isshoni >/dev/null 2>&1 || :
      as_root systemctl start isshoni >/dev/null 2>&1 || :
    fi
    ;;
  esac
}

# wait_ready SECONDS polls `isshoni healthcheck --ready` every 2 seconds until the server is ready: database,
# certificate, media sockets and signaling. It returns 1 when SECONDS are over, or as soon as systemd has given up
# on the unit.
wait_ready() {
  wr_start=$(date +%s)
  wr_said=0
  wr_hint=0
  case $CFG_TLS_MODE in
  auto | ip) wr_what='the HTTPS certificate' ;;
  *) wr_what='isshoni to be ready' ;;
  esac
  while :; do
    if as_root "$BIN" healthcheck --ready >/dev/null 2>&1; then
      return 0
    fi
    wr_now=$(($(date +%s) - wr_start))
    [ "$wr_now" -lt "$1" ] || return 1
    if systemctl is-failed --quiet isshoni 2>/dev/null; then
      return 1
    fi
    if [ $((wr_now / 10)) -gt "$wr_said" ]; then
      wr_said=$((wr_now / 10))
      say "Waiting for $wr_what… ($((wr_said * 10)) s)"
      if [ "$wr_now" -ge 30 ] && [ "$wr_hint" = 0 ]; then
        wr_hint=1
        case $CFG_TLS_MODE in
        auto | ip) say "Let's Encrypt must reach this server on port 80 or 443. Check your provider's firewall." ;;
        esac
      fi
    fi
    sleep 2
  done
}

# newest_premigration_backup prints the newest database backup that the server wrote before a migration during
# this run (backups/pre-<schema>-<time>.db), or nothing. The directory belongs to isshoni with mode 0700, so the
# search runs with privilege. Backups of earlier upgrades are older than this run and are left out: restoring one
# of those would lose what was written since.
newest_premigration_backup() {
  # shellcheck disable=SC2016 # the variables are the inner shell's
  as_root sh -c 'find "$1" -name "pre-*.db" -newer "$2" -exec ls -t {} + 2>/dev/null | sed -n 1p' sh \
    "$DATA_DIR/backups" "$TMP/started" 2>/dev/null || :
}

# not_ready FROM ends a run whose server did not become ready (exit 5): the journal's last lines, doctor's report,
# where to read more, after an upgrade from version FROM the way back, and as the last line what to do next. The
# unit stays enabled, so the next run of the installer repairs, waits again and prints the setup link.
not_ready() {
  say '' "isshoni $TV is installed, but it did not become ready." '' \
    'The last lines of its log (journalctl -u isshoni):'
  as_root journalctl -u isshoni --no-pager -n 30 2>&1 || :
  say '' 'What "isshoni doctor" finds:'
  as_root "$BIN" doctor --config "$CONFIG" 2>&1 || :
  say '' "Troubleshooting: $SITE_URL/troubleshooting" "Provider firewalls: $SITE_URL/install/vps"
  if [ -n "$1" ]; then
    nr_backup=$(newest_premigration_backup)
    say '' "To go back to isshoni $1:"
    if [ -n "$nr_backup" ]; then
      say '  the upgrade changed the database, so first restore the backup the server made before it:' \
        "    sudo systemctl stop isshoni && sudo -u isshoni isshoni admin restore --offline $nr_backup" \
        "  then run this installer again with: --version $1 --allow-downgrade"
    else
      say "  run this installer again with: --version $1 --allow-downgrade"
    fi
  fi
  say '' 'Fix the problem above, then run this installer again.'
  PLANNED_EXIT=1
  exit 5
}

# status_field NAME prints a string field of the running server's status (`isshoni admin status --json`, one
# line of JSON), or nothing when no server answers. It takes the first field of that name, which is the top-level
# one for "version" and "origin": they come before every nested object.
status_field() {
  as_root "$BIN" admin status --json 2>/dev/null | tr ',' '\n' | sed -n "s/^{*\"$1\":\"\\(.*\\)\"\$/\\1/p" | sed -n 1p
}

# site_origin prints the address friends open, like https://share.example.com: from the running server, or else
# from the config.
site_origin() {
  so=$(status_field origin)
  if [ -z "$so" ]; then
    if [ -n "$CFG_PUBLIC_URL" ]; then
      so=$CFG_PUBLIC_URL
    elif [ -n "$CFG_DOMAIN" ]; then
      so=https://$CFG_DOMAIN
    else
      so="https://<this server's address>"
    fi
  fi
  printf '%s\n' "${so%/}"
}

# print_setup HEADLINE ends a successful run: the headline, then the one-time link that creates the admin account
# (with a QR code on a terminal), or the word that setup is done when an admin exists. The link goes from the
# server to stdout and nowhere else: it is never written to a file or a log.
print_setup() {
  ps_headline=$1
  set -- setup-url
  if [ -t 1 ]; then
    set -- "$@" --qr
  fi
  ps_rc=0
  # Off a terminal, setup-url prints the bare link as its first line and the QR code after it. Its own heading
  # and notes go to stderr; the text below replaces them.
  ps_out=$(as_root "$BIN" "$@" 2>/dev/null) || ps_rc=$?
  case $ps_rc in
  0)
    say '' "$C_BOLD$ps_headline$C_RESET" '' '  Create the admin account (link valid for 24 hours, works once):'
    printf '%s\n' "$ps_out" | sed 's/^/  /'
    say '' \
      '  Status   systemctl status isshoni        Logs     journalctl -u isshoni -f' \
      '  Check    sudo isshoni doctor             Upgrade  run this installer again'
    ;;
  7) say '' "$C_BOLD$ps_headline$C_RESET" "Setup is already done. Open $(site_origin)/" ;;
  *)
    # The server answered the readiness check a moment ago and is gone now.
    not_ready ''
    ;;
  esac
}

# ---------------------------------------------------------------------------------------------------------------
# Uninstall and purge (06 §4.11)

# exists PATH reports whether something is at PATH, a dangling link included. Root looks, as in make_dir.
exists() {
  as_root test -e "$1" || as_root test -L "$1"
}

# remove_path PATH WHAT deletes a file that the installer put there, and says so. One that is gone already is fine:
# an uninstall finishes what an earlier one left.
remove_path() {
  exists "$1" || return 0
  as_root rm -f "$1" || die 1 "Could not remove $1." 'Check the file system, then run this installer again.'
  say "Removed $2 ($1)."
  REMOVED=$((REMOVED + 1))
}

# stop_service stops and disables the service, then removes its unit (06 §4.11 steps 1 and 2). The admin's own
# drop-ins (`systemctl edit isshoni`) stay.
stop_service() {
  step 'stopping isshoni'
  if systemctl is-active --quiet isshoni 2>/dev/null; then
    say 'Stopping isshoni'
  fi
  as_root systemctl disable --now isshoni.service >/dev/null 2>&1 || :
  if systemctl is-active --quiet isshoni 2>/dev/null; then
    die 1 'Could not stop isshoni, so nothing was removed.' \
      'See "systemctl status isshoni", stop it, then run this installer again.'
  fi
  step 'removing the systemd unit'
  remove_path "$UNIT" 'the systemd unit'
  as_root systemctl daemon-reload >/dev/null 2>&1 || :
  # A unit that had failed (it exited 78, say) would stay listed as "not-found failed", and the system "degraded",
  # until the next boot.
  as_root systemctl reset-failed isshoni.service >/dev/null 2>&1 || :
}

# remove_program removes the program and the sysctl file (06 §4.11 step 3). The larger UDP buffers stay in effect
# until the next boot.
remove_program() {
  step 'removing the program'
  remove_path "$BIN" 'the program'
  remove_path "${BIN%/*}/.isshoni.new" 'an unfinished copy of the program'
  step 'removing the UDP buffer settings'
  remove_path "$SYSCTL_FILE" 'the UDP buffer settings, which stay in effect until the next boot'
}

# remove_ufw deletes the rule that the firewall offer added (`ufw allow isshoni`), then the profile. ufw needs the
# profile to delete the rule, so a rule ufw does not delete keeps its profile. A rule of the admin's that names the
# profile in another way keeps working without it.
remove_ufw() {
  exists "$UFW_PROFILE" || return 0
  if have ufw; then
    ru_rc=0
    ru_out=$(as_root ufw delete allow isshoni 2>&1) || ru_rc=$?
    if [ "$ru_rc" -ne 0 ]; then
      warn "ufw did not delete the isshoni rule, so its profile $UFW_PROFILE stays:" "$ru_out" \
        'Delete the rule yourself (sudo ufw delete allow isshoni), then the profile.'
      return 0
    fi
    case $ru_out in
    *'Rule deleted'*)
      say 'Firewall: removed the isshoni rule from ufw.'
      REMOVED=$((REMOVED + 1))
      ;;
    esac
  fi
  remove_path "$UFW_PROFILE" "ufw's isshoni profile"
}

# fw_perm runs a firewalld command on the permanent configuration: through the daemon when it runs (FW_ONLINE is
# 1), on the files with firewall-offline-cmd otherwise.
fw_perm() {
  if [ "$FW_ONLINE" = 1 ]; then
    as_root firewall-cmd --permanent "$@"
  else
    as_root firewall-offline-cmd "$@"
  fi
}

# fw_scrub LABEL SELECT REMOVE takes the isshoni service, and the rich rules that name it, out of one zone or policy
# of firewalld's permanent configuration. SELECT is the option that names the zone or policy (--zone=public),
# REMOVE the option that removes a service there. LABEL ("zone public") goes into FW_REMOVED, or into FW_FAILED
# when firewalld refuses a change. A removed rich rule is printed in full: it may hold addresses that are written
# down nowhere else, and the admin may want to add it again.
fw_scrub() {
  fs_found=0
  fs_ok=1
  if fw_perm "$2" --query-service=isshoni >/dev/null 2>&1; then
    fs_found=1
    fw_perm "$2" "$3=isshoni" >/dev/null 2>&1 || fs_ok=0
  fi
  fs_rules=$(fw_perm "$2" --list-rich-rules 2>/dev/null | grep -F 'service name="isshoni"') || fs_rules=''
  while IFS= read -r fs_rule; do
    [ -n "$fs_rule" ] || continue
    fs_found=1
    if fw_perm "$2" --remove-rich-rule="$fs_rule" >/dev/null 2>&1; then
      say "Firewall: removed this rule from firewalld's $1: $fs_rule"
    else
      fs_ok=0
    fi
  done <<EOF
$fs_rules
EOF
  [ "$fs_found" = 1 ] || return 0
  if [ "$fs_ok" = 1 ]; then
    FW_REMOVED="${FW_REMOVED:+$FW_REMOVED, }$1"
  else
    FW_FAILED="${FW_FAILED:+$FW_FAILED, }$1"
  fi
}

# remove_firewalld takes the isshoni service out of firewalld, then removes its file (06 §4.11). firewalld rejects
# its whole permanent configuration when a zone or a policy names a service it can't find: the next reload fails,
# and from its next start on it runs the stock configuration (state "failed"), without any of the admin's own
# rules. `--remove-service=isshoni` alone edits the default zone only, and an admin may have put the service into
# other zones or a policy, or written a rich rule that names it. So before isshoni.xml goes, the service leaves
# every zone and policy that has it, with every rich rule that names it: through the daemon when it runs, and then
# one reload, which closes the ports (it works because the daemon still knows the service), or on the files with
# firewall-offline-cmd. Nothing else in the firewall is touched. When firewalld refuses to change a zone or a
# policy, a warning names it and the file stays, so that firewalld goes on loading its configuration until the
# admin has taken the service out there by hand; the uninstall goes on.
remove_firewalld() {
  exists "$FIREWALLD_SERVICE" || return 0
  FW_REMOVED=''
  FW_FAILED=''
  # Only a zone or a policy that was changed on this machine can name the service, and firewalld keeps those in
  # files of their names below /etc/firewalld. Looking at the files first saves a dozen slow firewalld commands.
  if as_root grep -rqs isshoni "$FIREWALLD_DIR/zones" "$FIREWALLD_DIR/policies"; then
    if firewalld_running; then
      FW_ONLINE=1
      rf_zone_rm=--remove-service
      rf_policy_rm=--remove-service
    elif have firewall-offline-cmd; then
      FW_ONLINE=0
      # This tool's --remove-service is another option, which takes neither --zone nor --policy.
      rf_zone_rm=--remove-service-from-zone
      rf_policy_rm=--remove-service-from-policy
    else
      FW_ONLINE=''
      FW_FAILED='zones and policies (firewalld is not running, and firewall-offline-cmd is not installed)'
    fi
    if [ -n "$FW_ONLINE" ]; then
      for rf_zone in $(fw_perm --get-zones 2>/dev/null); do
        as_root grep -rqs isshoni "$FIREWALLD_DIR/zones/$rf_zone.xml" "$FIREWALLD_DIR/zones/$rf_zone" || continue
        fw_scrub "zone $rf_zone" "--zone=$rf_zone" "$rf_zone_rm"
      done
      for rf_policy in $(fw_perm --get-policies 2>/dev/null); do
        as_root grep -qs isshoni "$FIREWALLD_DIR/policies/$rf_policy.xml" || continue
        fw_scrub "policy $rf_policy" "--policy=$rf_policy" "$rf_policy_rm"
      done
    fi
    if [ -n "$FW_REMOVED" ]; then
      if [ "$FW_ONLINE" = 1 ] && ! as_root firewall-cmd --reload >/dev/null 2>&1; then
        warn 'firewalld did not reload, so the ports stay open until it does.' 'Reload it: sudo firewall-cmd --reload'
      fi
      say "Firewall: removed the isshoni service from firewalld ($FW_REMOVED)."
      REMOVED=$((REMOVED + 1))
    fi
  fi
  if [ -n "$FW_FAILED" ]; then
    warn "Could not take the isshoni service out of firewalld's $FW_FAILED." \
      "Remove it there by hand, then delete $FIREWALLD_SERVICE. The file stays until then: firewalld can't load" \
      'its configuration while a zone or a policy names a service that it has no file for.'
    return 0
  fi
  remove_path "$FIREWALLD_SERVICE" "firewalld's isshoni service"
}

# remove_iptables deletes the rules tagged "isshoni" from the INPUT chains of iptables and ip6tables (the firewall
# offer added them), and saves the rules where netfilter-persistent keeps them.
remove_iptables() {
  ri_deleted=0
  for ri_cmd in iptables ip6tables; do
    have "$ri_cmd" || continue
    ri_rules=$(as_root "$ri_cmd" -S INPUT 2>/dev/null) || continue
    ri_ours=$(printf '%s\n' "$ri_rules" | grep -E -- '^-A INPUT .*--comment isshoni( |$)') || ri_ours=''
    while IFS= read -r ri_rule; do
      [ -n "$ri_rule" ] || continue
      set -f
      # shellcheck disable=SC2086 # the words of the rule as iptables -S printed it, with -A turned into -D
      if as_root "$ri_cmd" -D ${ri_rule#-A } 2>/dev/null; then
        ri_deleted=$((ri_deleted + 1))
      else
        warn "$ri_cmd did not delete the rule: $ri_rule" "Delete it yourself: sudo $ri_cmd -D ${ri_rule#-A }"
      fi
      set +f
    done <<EOF
$ri_ours
EOF
  done
  [ "$ri_deleted" -gt 0 ] || return 0
  REMOVED=$((REMOVED + 1))
  if have netfilter-persistent; then
    if as_root netfilter-persistent save >/dev/null 2>&1; then
      say "Firewall: removed isshoni's iptables rules, and saved the rules."
    else
      warn "Removed isshoni's iptables rules, but netfilter-persistent did not save the rules." \
        'Save them yourself: sudo netfilter-persistent save'
    fi
  else
    say "Firewall: removed isshoni's iptables rules."
  fi
}

# uninstall_steps removes isshoni and keeps its configuration, its data and its user: the service, the unit, the
# program, the sysctl file and the firewall rules and profiles (06 §4.11 steps 1 to 4). REMOVED counts what it
# removed.
uninstall_steps() {
  REMOVED=0
  stop_service
  remove_program
  step 'removing the firewall rules'
  remove_ufw
  remove_firewalld
  remove_iptables
}

# say_dropins mentions the admin's own drop-ins, which an uninstall leaves where they are.
say_dropins() {
  exists "$DROPIN_DIR" || return 0
  say "Your own changes to the service (systemctl edit isshoni) are still in $DROPIN_DIR."
}

# do_uninstall is --uninstall: it removes isshoni and keeps its configuration, its data and its user, so that
# running the installer again brings it back as it was (06 §4.11). It ends with exit 0 also when there was nothing
# left to remove.
do_uninstall() {
  uninstall_steps
  if [ "$REMOVED" = 0 ]; then
    say '' 'isshoni is not installed here: there was nothing to remove.'
  else
    say '' 'isshoni is uninstalled.'
  fi
  if exists "$CONFIG_DIR" || exists "$DATA_DIR" || getent passwd isshoni >/dev/null 2>&1; then
    # It can offer no backup command for now: the service is stopped and the program gone, and an offline backup
    # refuses to run as root.
    say '' "Kept: $CONFIG_DIR (config), $DATA_DIR (database, certificates, backups) and the isshoni user." \
      'Remove them with --purge. Before --purge, back up with "sudo isshoni admin backup --out <file>" while' \
      'isshoni is installed and running (run this installer again to bring it back).'
  fi
  say_dropins
}

# confirm_purge asks for "purge" to be typed, before anything is changed; --yes skips the question. It comes while
# the server still runs, so the backup command it shows works. Any other answer is exit 8 with nothing changed.
confirm_purge() {
  [ "$OPT_YES" = 0 ] || return 0
  have_tty || die 2 '--purge deletes the configuration, the data and the isshoni user, and there is no terminal to confirm on.' \
    'Run it in a terminal, or pass --yes as well.'
  printf '%s\n' '' \
    "This removes isshoni and deletes its configuration ($CONFIG_DIR), its data ($DATA_DIR: the database, the" \
    'certificates and the backups) and the isshoni user. It makes no backup. To keep one, press Enter now and' \
    'back up first, while isshoni runs:' \
    '  sudo isshoni admin backup --out <file>' '' >&2
  if ! ask 'Type purge to delete everything:'; then
    die 8 'Cancelled.' 'Nothing was changed.'
  fi
  [ "$ANSWER" = purge ] || die 8 'Cancelled.' 'Nothing was changed.'
}

# do_purge is --purge: --uninstall, then it deletes the configuration, the data and the isshoni user (06 §4.11). It
# makes no backup.
do_purge() {
  confirm_purge
  uninstall_steps
  step 'deleting the configuration and the data'
  for dp_dir in "$CONFIG_DIR" "$DATA_DIR" "$RUN_DIR"; do
    exists "$dp_dir" || continue
    as_root rm -rf "$dp_dir" || die 1 "Could not delete $dp_dir." 'Check the file system, then run this installer again.'
    say "Deleted $dp_dir."
  done
  step 'deleting the isshoni user'
  if getent passwd isshoni >/dev/null 2>&1; then
    if quietly as_root userdel isshoni; then
      say 'Deleted the isshoni user.'
    else
      warn 'Could not delete the isshoni user (see above).' 'Delete it yourself: sudo userdel isshoni'
    fi
  fi
  # userdel deletes the group too where the system makes a group per user; elsewhere it stays.
  if getent group isshoni >/dev/null 2>&1; then
    if quietly as_root groupdel isshoni; then
      say 'Deleted the isshoni group.'
    else
      warn 'Could not delete the isshoni group (see above).' 'Delete it yourself: sudo groupdel isshoni'
    fi
  fi
  say '' 'isshoni is purged: its program, configuration, data and user are gone.'
  say_dropins
}

# ---------------------------------------------------------------------------------------------------------------
# Actions

# cleanup runs when the script ends, however it ends: it deletes a config file that this run wrote and that did
# not get past the pre-check, and the download directory. That includes a run that a signal ends: Ctrl-C, a kill,
# or a terminal that went away (set_traps). When the script ends because a command failed that no code here
# handles (set -e), it says at which step, and exits with 1: that command's own exit code could be read as one of
# the installer's.
cleanup() {
  cl_rc=$?
  drop_pending_config
  if [ -n "$TMP" ]; then
    rm -rf "$TMP"
    TMP=''
  fi
  if [ "$cl_rc" -ne 0 ] && [ "$PLANNED_EXIT" = 0 ]; then
    printf '%sError:%s The installer stopped at an unexpected error while %s (see the message above).\n' \
      "$E_RED" "$E_RESET" "$STEP" >&2
    printf '%s\n' 'Fix what it names, then run this installer again: it goes on where it stopped.' >&2
    exit 1
  fi
}

# set_traps makes the script end through cleanup when a signal ends it, too: Ctrl-C is a cancel (exit 8), a kill
# and a terminal that went away (HUP, which a dropped SSH session sends) are exit 1 (stopped_by_signal). Without a
# trap of its own, a signal ends dash and busybox sh at once, and the EXIT trap does not run.
set_traps() {
  trap cleanup EXIT
  trap 'PLANNED_EXIT=1; printf "\n%s\n" "Cancelled." >&2; exit 8' INT
  trap stopped_by_signal TERM HUP
}

# stopped_by_signal ends a run that a kill or a hangup stopped with exit 1, an unexpected error, whose message says
# at which step (06 §4.1). The message may have nowhere to go, the terminal of a dropped SSH session or a pipe whose
# reader is gone: neither a failed write nor a SIGPIPE may keep cleanup from running.
stopped_by_signal() {
  PLANNED_EXIT=1
  trap '' PIPE
  printf '\n%sError:%s Stopped by a signal while %s.\n%s\n' "$E_RED" "$E_RESET" "$STEP" \
    'Run this installer again: it goes on where it stopped.' >&2 || :
  exit 1
}

# running_other_version reports whether the running server is another version than the installed program: an
# earlier run replaced the program and did not get to the restart.
running_other_version() {
  ro=$(status_field version)
  [ -n "$ro" ] && [ "${ro%%+*}" != "$TV" ]
}

# fetch_and_verify DIR downloads the release of TV into DIR, verifies it and unpacks it. Nothing on the machine
# has changed when it fails (exit 3), apart from an OpenSSH client that had to be installed for the check.
fetch_and_verify() {
  # A copy of the installer without a key can't accept any release: say so before it downloads one.
  need_key
  downloader
  step 'installing ssh-keygen, which verifies the download'
  need_ssh_keygen
  step 'downloading the release'
  mkdir "$1"
  fetch_release "$1"
  step 'verifying the release'
  verify_release "$1" "$ARCHIVE"
  step 'unpacking the release'
  unpack_release "$1" "$ARCHIVE"
}

# first_start is the tail of a run on a machine where isshoni was never started: the port check, the firewall
# offer, the start, the wait and the setup link. The firewall comes before the start, so that Let's Encrypt reaches
# the server at its first try.
first_start() {
  step 'checking that the ports are free'
  check_ports "$PORTS_TCP" "$PORTS_UDP"
  step 'opening the firewall'
  say ''
  offer_firewall
  step 'starting isshoni'
  start_or_restart first
  wait_ready 180 || not_ready ''
  step 'making the setup link'
  print_setup "isshoni $TV is running."
}

# restart_or_repair FROM is the tail of a run on a machine where isshoni was started before: an upgrade or a
# downgrade from version FROM, or with an empty FROM a reinstall or a repair. The server is restarted when its
# program or its unit changed, and otherwise only started when it is down.
restart_or_repair() {
  if [ "$OPT_FIREWALL" = 1 ]; then
    step 'opening the firewall'
    say ''
    offer_firewall
  fi
  step 'starting isshoni'
  if [ "$BIN_CHANGED" = 1 ] || [ "$UNIT_CHANGED" = 1 ] || running_other_version; then
    check_ports "$PORTS_TCP" "$PORTS_UDP"
    start_or_restart restart
  else
    if ! systemctl is-active --quiet isshoni 2>/dev/null; then
      check_ports "$PORTS_TCP" "$PORTS_UDP"
    fi
    start_or_restart ensure
  fi
  wait_ready 120 || not_ready "$1"
  step 'making the setup link'
  if [ -z "$1" ]; then
    print_setup "isshoni $TV is running."
  elif [ "$(version_cmp "$1" "$TV")" = 1 ]; then
    print_setup "Downgraded $1 → $TV"
  else
    print_setup "Upgraded $1 → $TV"
  fi
}

# do_install installs, upgrades, repairs or finishes an installation (06 §4.2).
do_install() {
  # A machine without a config gets the one question. When no flag answers it and there is no terminal to ask
  # on, say so now, before anything is downloaded or installed.
  if ! config_exists && [ -z "$OPT_TLS_MODE" ] && [ "$OPT_YES" = 0 ] && ! have_tty; then
    die 2 'Non-interactive install: pass --domain NAME or --ip' \
      "Example: curl -fsSL $SITE_URL/install.sh | sh -s -- --domain share.example.com"
  fi

  step 'finding the version to install'
  installed_version
  resolve_version
  STARTED=0
  if is_started; then
    STARTED=1
  fi
  ARCHIVE=isshoni_${TV}_linux_$ARCH.tar.gz
  di_same=0
  if [ -n "$IV" ]; then
    if [ "$(version_cmp "$IV" "$TV")" = 1 ] && [ "$OPT_ALLOW_DOWNGRADE" = 0 ]; then
      die 6 "isshoni $IV is installed, which is newer than $TV. Nothing was changed." \
        'An older isshoni cannot read a database that a newer one has migrated: restore a backup that the older' \
        "version made first, then run this installer with --version $TV --allow-downgrade."
    fi
    if [ "${IV%%+*}" = "$TV" ]; then
      di_same=1
    fi
  fi

  TMP=$(mktemp -d) || die 1 'Could not create a temporary directory.' 'Check that /tmp is writable and has room.'
  : >"$TMP/started"

  # The archive is needed for another version, for --reinstall, and when a file that comes from it is missing.
  di_fetch=0
  if [ "$di_same" = 0 ] || [ "$OPT_REINSTALL" = 1 ] || ! payload_complete; then
    di_fetch=1
    fetch_and_verify "$TMP/dl"
    if [ "$di_same" = 0 ] || [ "$OPT_REINSTALL" = 1 ]; then
      step 'installing the program'
      say "Installing isshoni $TV"
      install_binary "$TMP/dl"
    fi
  elif [ "$STARTED" = 1 ]; then
    say "isshoni $TV is already installed."
  fi

  step 'creating the isshoni user and its directories'
  ensure_user
  if [ "$di_fetch" = 1 ]; then
    step 'installing the systemd unit'
    install_unit "$TMP/dl"
    step 'setting the UDP buffer sizes'
    apply_sysctl "$TMP/dl"
    step 'installing the firewall profiles'
    install_fw_profiles "$TMP/dl"
  fi

  if ! config_exists; then
    step 'writing the config'
    write_config
  fi
  step 'reading the config'
  load_config

  if [ "$OPT_NO_START" = 1 ]; then
    if [ "$STARTED" = 1 ]; then
      say '' "Installed isshoni $TV. The running server was not restarted (--no-start). To restart it:" \
        '  sudo systemctl daemon-reload && sudo systemctl restart isshoni'
    else
      say '' 'Installed. Run this installer again to start isshoni.'
    fi
    return 0
  fi

  ports_needed
  if [ "$STARTED" = 0 ]; then
    first_start
  elif [ "$di_same" = 0 ] && [ -n "$IV" ]; then
    restart_or_repair "$IV"
  else
    restart_or_repair ''
  fi
}

main() {
  # The installer reads what some tools print (ufw, ss), and root's tools live in sbin, which the PATH of an
  # ordinary user may lack.
  LC_ALL=C
  export LC_ALL
  PATH=$PATH:/usr/local/sbin:/usr/sbin:/sbin
  export PATH
  setup_colors
  parse_args "$@"
  scrub_env
  set_traps
  step 'checking this system'
  preflight
  case $OPT_ACTION in
  uninstall) do_uninstall ;;
  purge) do_purge ;;
  *) do_install ;;
  esac
}

[ "${ISSHONI_INSTALL_SOURCED:-0}" = 1 ] || main "$@"
