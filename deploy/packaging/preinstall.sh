#!/bin/sh
# isshoni deb and rpm packages: runs before the package's files are unpacked (docs/m1/06-deploy-and-ci.md §5).
#
#   dpkg: preinst install|upgrade [OLD-VERSION]; preinst abort-upgrade NEW-VERSION when an upgrade is undone
#   rpm:  %pre with $1 = 1 (first install) or 2 (upgrade)
#
# It refuses to go on next to an install.sh installation, and creates the isshoni system user and group with the
# commands install.sh uses (06 §4.4), so both ways of installing give the same account.
set -eu

case ${1-} in
abort-*) exit 0 ;; # dpkg is undoing a failed upgrade: nothing to check, nothing to create
esac

# systemd looks in /usr/local before /usr, for the program and for the unit alike: the service would keep running
# install.sh's copy, and this package's files would never be used.
for f in /usr/local/bin/isshoni /usr/local/lib/systemd/system/isshoni.service; do
  if [ -e "$f" ] || [ -L "$f" ]; then
    {
      printf '%s\n' "isshoni: $f exists." \
        'isshoni was installed with install.sh; run it with --uninstall first:' \
        '  curl -fsSL https://moonwx.github.io/isshoni/install.sh | sh -s -- --uninstall' \
        'That keeps the configuration (/etc/isshoni), the data (/var/lib/isshoni) and the isshoni user.'
    } >&2
    exit 1
  fi
done

if ! getent group isshoni >/dev/null || ! getent passwd isshoni >/dev/null; then
  for tool in groupadd useradd; do
    if ! command -v "$tool" >/dev/null 2>&1; then
      printf '%s\n' "isshoni: $tool is missing, so the isshoni user can't be created." \
        'Install it (package "passwd" on Debian and Ubuntu, "shadow-utils" on Fedora), then install isshoni again.' >&2
      exit 1
    fi
  done
fi

if ! getent group isshoni >/dev/null; then
  groupadd --system isshoni
fi
if ! getent passwd isshoni >/dev/null; then
  useradd --system --gid isshoni --home-dir /var/lib/isshoni --no-create-home \
    --shell "$(command -v nologin || echo /bin/false)" --comment "isshoni server" isshoni
fi
