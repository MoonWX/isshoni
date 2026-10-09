#!/bin/sh
# isshoni deb and rpm packages: runs after the package's files are unpacked (docs/m1/06-deploy-and-ci.md §5).
#
#   dpkg: postinst configure [OLD-VERSION] (no OLD-VERSION on a first install); postinst abort-upgrade|abort-remove|
#         abort-deconfigure … when a later step failed
#   rpm:  %post with $1 = 1 (first install) or 2 (upgrade)
#
# It makes systemd read the unit, applies the UDP buffer sizes, and restarts a running service after an upgrade.
# It never enables or starts the service: a fresh install has no configuration yet. It prints the steps instead.
set -eu

CONFIG=/etc/isshoni/isshoni.toml
BUFFER_BYTES=8388608 # both values of 60-isshoni.conf

say() {
  printf '%s\n' "$@"
}

warn() {
  printf 'isshoni: %s\n' "$*" >&2
}

# apply_sysctl applies /usr/lib/sysctl.d/60-isshoni.conf now, as the next boot will (an /etc/sysctl.d file of the
# same name wins, as at boot). Like install.sh (06 §4.6) it does so only while a running value is below the file's:
# a larger value from another sysctl.d file would otherwise be lowered until the next boot. Where the values can't
# be read or written (most containers), nothing happens.
apply_sysctl() {
  low=0
  for key in rmem_max wmem_max; do
    cur=$(cat "/proc/sys/net/core/$key" 2>/dev/null) || cur=
    case $cur in
    '' | *[!0-9]*) ;;
    *) [ "$cur" -ge "$BUFFER_BYTES" ] || low=1 ;;
    esac
  done
  [ "$low" = 1 ] || return 0
  for bin in /usr/lib/systemd/systemd-sysctl /lib/systemd/systemd-sysctl; do
    [ -x "$bin" ] || continue
    "$bin" 60-isshoni.conf ||
      warn 'could not apply 60-isshoni.conf (UDP buffer sizes); "isshoni doctor" shows the sizes in use'
    return 0
  done
}

# Everything but a first install counts as an upgrade, dpkg's abort-* cases too: the files of a running service may
# have changed.
upgrade=1
case ${1-} in
configure) [ -n "${2-}" ] || upgrade=0 ;;
1) upgrade=0 ;;
esac

if [ -d /run/systemd/system ]; then
  systemctl daemon-reload || true
  apply_sysctl
  if [ "$upgrade" = 1 ]; then
    # Restarts only a running service; clients reconnect on their own.
    systemctl try-restart isshoni.service ||
      warn 'the service did not restart; see "systemctl status isshoni" and "journalctl -u isshoni"'
  fi
fi

# Enabled means the admin has started it before (the last step below): there is nothing left to say.
if systemctl is-enabled --quiet isshoni.service 2>/dev/null; then
  exit 0
fi

say '' 'isshoni is installed. It is not enabled or started yet.' ''
step=1
if [ ! -e "$CONFIG" ]; then
  say "$step. Write the configuration. With a domain name that points to this server:" \
    "     sudo isshoni config init --path $CONFIG --domain share.example.com" \
    "   or, without a domain, for this server's public IP address:" \
    "     sudo isshoni config init --path $CONFIG --tls.mode ip" \
    '   Then let the service read it:' \
    "     sudo chown root:isshoni $CONFIG"
  step=$((step + 1))
fi
say "$step. Open ports 80 and 443 (TCP) and 7882 (TCP and UDP), here and in your provider's cloud firewall."
# The commands for a firewall that is active on this machine (an inactive one blocks nothing). Both use the
# profile this package installs.
if LC_ALL=C ufw status 2>/dev/null | grep -q '^Status: active'; then
  say '     sudo ufw allow isshoni'
fi
if [ "$(firewall-cmd --state 2>/dev/null)" = running ]; then
  # A running firewalld knows a new service file only after a reload.
  say '     sudo firewall-cmd --reload' \
    '     sudo firewall-cmd --permanent --add-service=isshoni && sudo firewall-cmd --reload'
fi
step=$((step + 1))
say "$step. Start isshoni, and print the link that creates the admin account:" \
  '     sudo systemctl enable --now isshoni' \
  '     sudo isshoni setup-url --wait 180s' \
  '' \
  'Guide: https://moonwx.github.io/isshoni/install/' \
  ''
