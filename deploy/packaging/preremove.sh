#!/bin/sh
# isshoni deb and rpm packages: runs before the package's files are removed (docs/m1/06-deploy-and-ci.md §5).
#
#   dpkg: prerm remove; prerm upgrade NEW-VERSION; prerm deconfigure …; prerm failed-upgrade OLD-VERSION
#   rpm:  %preun with $1 = 0 (the package is removed) or 1 (an upgrade removes the old copy)
#
# When the package is removed, it stops and disables the service, and takes the isshoni service out of firewalld's
# zones and policies, because the file that defines that service goes with the package. An upgrade changes nothing
# here: the new package's postinstall.sh restarts the service once the new files are in place.
set -eu

# fwd runs a firewalld command on the permanent configuration: through the daemon when it runs ($fw_online is 1),
# on the files otherwise.
fwd() {
  if [ "$fw_online" = 1 ]; then
    firewall-cmd --permanent "$@"
  else
    firewall-offline-cmd "$@"
  fi
}

# fw_scrub LABEL SELECT REMOVE takes the isshoni service, and the rich rules that name it, out of one zone or policy:
# SELECT is the option that names it, REMOVE the one that removes a service there. LABEL goes into $removed, or
# into $failed when firewalld refuses.
fw_scrub() {
  found=0
  ok=1
  if fwd "$2" --query-service=isshoni >/dev/null 2>&1; then
    found=1
    fwd "$2" "$3=isshoni" >/dev/null 2>&1 || ok=0
  fi
  rules=$(fwd "$2" --list-rich-rules 2>/dev/null | grep -F 'service name="isshoni"') || rules=
  while IFS= read -r rule; do
    [ -n "$rule" ] || continue
    found=1
    if fwd "$2" --remove-rich-rule="$rule" >/dev/null 2>&1; then
      # In full, so that the admin can add it again: it may hold addresses that are written down nowhere else.
      printf '%s\n' "isshoni: removed a firewalld rule from $1: $rule"
    else
      ok=0
    fi
  done <<RULES
$rules
RULES
  [ "$found" = 1 ] || return 0
  if [ "$ok" = 1 ]; then
    removed="${removed:+$removed, }$1"
  else
    failed="${failed:+$failed, }$1"
  fi
}

# clean_firewalld takes the isshoni service out of every zone and policy of firewalld's permanent configuration.
# firewalld rejects that whole configuration when one of them names a service it can't find: the next reload fails,
# and from its next start on firewalld runs the stock configuration (state "failed"), without any of the admin's
# own rules. The admin has put the service there (postinstall.sh prints the command). Nothing else in the firewall
# is touched, and ufw needs nothing: its rule keeps working without the profile.
clean_firewalld() {
  # With the admin's own definition of the service, the name stays valid when this package's file is gone.
  [ ! -e /etc/firewalld/services/isshoni.xml ] || return 0
  # Only a zone or a policy that was changed on this machine can name the service, and firewalld keeps those in
  # files of their names here. Looking at the files first saves a dozen slow firewalld commands.
  grep -rqs isshoni /etc/firewalld/zones /etc/firewalld/policies || return 0
  # systemd is asked first: without a daemon to talk to, firewalld 2.4's firewall-cmd waits 10 seconds.
  if systemctl is-active --quiet firewalld.service 2>/dev/null &&
    [ "$(firewall-cmd --state 2>/dev/null)" = running ]; then
    fw_online=1
    zone_remove=--remove-service
    policy_remove=--remove-service
  elif command -v firewall-offline-cmd >/dev/null 2>&1; then
    fw_online=0
    # This tool's --remove-service is another option, which takes neither --zone nor --policy.
    zone_remove=--remove-service-from-zone
    policy_remove=--remove-service-from-policy
  else
    return 0
  fi
  removed=
  failed=
  for zone in $(fwd --get-zones 2>/dev/null); do
    grep -rqs isshoni "/etc/firewalld/zones/$zone.xml" "/etc/firewalld/zones/$zone" || continue
    fw_scrub "zone $zone" "--zone=$zone" "$zone_remove"
  done
  for policy in $(fwd --get-policies 2>/dev/null); do
    grep -qs isshoni "/etc/firewalld/policies/$policy.xml" || continue
    fw_scrub "policy $policy" "--policy=$policy" "$policy_remove"
  done
  if [ -n "$removed" ]; then
    if [ "$fw_online" = 1 ]; then
      # The daemon still knows the service at this point, so this reload works. It closes the ports.
      firewall-cmd --reload >/dev/null 2>&1 || true
    fi
    printf '%s\n' "isshoni: removed the isshoni service from firewalld ($removed): this package defines it."
  fi
  if [ -n "$failed" ]; then
    printf '%s\n' "isshoni: could not remove the isshoni service from firewalld ($failed)." \
      'Remove it there by hand, or firewalld will reject its configuration at the next reload.' >&2
  fi
}

case ${1-} in
remove | 0) ;;
*) exit 0 ;;
esac

if [ -d /run/systemd/system ]; then
  systemctl disable --now isshoni.service || true
  # A unit that had failed (exit 78 after a configuration error, say) would stay listed as "not-found failed", and
  # the system "degraded", until the next boot.
  systemctl reset-failed isshoni.service 2>/dev/null || true
elif command -v systemctl >/dev/null 2>&1; then
  # No running systemd (a chroot, an image build): there is nothing to stop, only the enablement links to remove.
  systemctl disable isshoni.service || true
fi

clean_firewalld
