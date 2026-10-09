#!/bin/sh
# isshoni deb and rpm packages: runs after the package's files are removed (docs/m1/06-deploy-and-ci.md §5).
#
#   dpkg: postrm remove; postrm purge; postrm upgrade NEW-VERSION; and the abort-* and failed-upgrade cases
#   rpm:  %postun with $1 = 0 (the package is removed) or 1 (an upgrade removed the old copy)
#
# The data directory is never deleted here: it holds the accounts, the certificates and the backups, and only an
# explicit action of the admin removes it. `apt purge` removes the configuration; rpm has no purge, so there the
# configuration stays as well. Both say what they kept.
set -eu

if [ -d /run/systemd/system ]; then
  systemctl daemon-reload || true
fi

case ${1-} in
purge)
  # A purge can come long after the remove. If install.sh has installed isshoni in between, /etc/isshoni is the
  # configuration of that installation.
  if [ -e /usr/local/bin/isshoni ]; then
    printf '%s\n' 'isshoni: kept /etc/isshoni: the isshoni in /usr/local/bin (installed with install.sh) uses it.'
    exit 0
  fi
  rm -rf /etc/isshoni
  ;;
0) ;;
*) exit 0 ;; # deb remove (the configuration stays until a purge), and every upgrade and abort case
esac

kept=
dirs=
if [ -d /etc/isshoni ]; then
  kept='/etc/isshoni (configuration)'
  dirs=' /etc/isshoni'
fi
if [ -d /var/lib/isshoni ]; then
  kept="${kept:+$kept, }/var/lib/isshoni (database, certificates, backups)"
  dirs="$dirs /var/lib/isshoni"
fi
if [ -n "$dirs" ]; then
  printf '%s\n' "isshoni: kept $kept and the isshoni user." \
    "To delete them too: sudo rm -rf$dirs && sudo userdel isshoni"
fi
