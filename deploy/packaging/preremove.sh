#!/bin/sh
# isshoni deb and rpm packages: runs before the package's files are removed (docs/m1/06-deploy-and-ci.md §5).
#
#   dpkg: prerm remove; prerm upgrade NEW-VERSION; prerm deconfigure …; prerm failed-upgrade OLD-VERSION
#   rpm:  %preun with $1 = 0 (the package is removed) or 1 (an upgrade removes the old copy)
#
# It stops and disables the service when the package is removed. An upgrade leaves the service running: the new
# package's postinstall.sh restarts it once the new files are in place.
set -eu

case ${1-} in
remove | 0) ;;
*) exit 0 ;;
esac

if [ -d /run/systemd/system ]; then
  systemctl disable --now isshoni.service || true
elif command -v systemctl >/dev/null 2>&1; then
  # No running systemd (a chroot, an image build): there is nothing to stop, only the enablement links to remove.
  systemctl disable isshoni.service || true
fi
