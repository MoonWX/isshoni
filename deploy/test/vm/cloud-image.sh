#!/bin/sh
# Downloads the official cloud image of a supported distro for the VM scenarios (docs/m1/06-deploy-and-ci.md §4.13,
# "06" below), and checks it against the checksum file the distro publishes next to it.
#
#   sh deploy/test/vm/cloud-image.sh DISTRO FILE
#
# DISTRO is one of the supported distros (06 §4.13; debian-12, debian-13, ubuntu-22.04, ubuntu-24.04, ubuntu-26.04,
# fedora-44 today), named as distro-test.sh names them: Debian 12 or 13, any Ubuntu release, any Fedora release, so
# that a Fedora bump needs no change here. The image is for this machine's CPU (x86_64 or aarch64). Debian's and
# Ubuntu's are the current build of the release, Fedora's is the release's image.
# FILE gets the image (qcow2) once its checksum matches, and not before. vm-test.sh calls this when its VM
# directory has no image for the distro yet; the nightly's distro-vm job calls it in a step of its own.
#
# Everything goes over HTTPS. The checksum comes from the distro's own server (for Fedora the project's, and the
# image itself from any mirror). A "latest" directory can change between the two downloads, so a mismatch is tried
# once more with both read again.
#
# It needs curl, awk, and sha256sum or sha512sum (GNU coreutils).
set -eu

ci_die() {
  printf 'cloud-image: %s\n' "$@" >&2
  [ "${GITHUB_ACTIONS-}" != true ] || printf '::error::cloud-image: %s\n' "$1"
  exit 1
}

[ $# -eq 2 ] || ci_die 'usage: cloud-image.sh DISTRO FILE'
ci_distro=$1
ci_file=$2

case $(uname -m) in
x86_64) ci_deb_arch=amd64 ci_rpm_arch=x86_64 ;;
aarch64 | arm64) ci_deb_arch=arm64 ci_rpm_arch=aarch64 ;;
*) ci_die "no cloud images for $(uname -m) (x86_64 and aarch64 only)" ;;
esac

case $ci_distro in
debian-12 | debian-13)
  ci_v=${ci_distro#debian-}
  ci_code=bookworm
  [ "$ci_v" = 12 ] || ci_code=trixie
  ci_base=https://cloud.debian.org/images/cloud/$ci_code/latest
  ci_sums_url=$ci_base/SHA512SUMS ci_algo=sha512
  ci_image=debian-$ci_v-genericcloud-$ci_deb_arch.qcow2
  ;;
ubuntu-[0-9]*)
  ci_v=${ci_distro#ubuntu-}
  ci_base=https://cloud-images.ubuntu.com/releases/$ci_v/release
  ci_sums_url=$ci_base/SHA256SUMS ci_algo=sha256
  ci_image=ubuntu-$ci_v-server-cloudimg-$ci_deb_arch.img
  ;;
fedora-[0-9]*)
  ci_v=${ci_distro#fedora-}
  ci_listing=https://dl.fedoraproject.org/pub/fedora/linux/releases/$ci_v/Cloud/$ci_rpm_arch/images
  ci_sums=$(curl -fsSL --retry 3 "$ci_listing/" | grep -o "Fedora-Cloud-$ci_v-[0-9.]*-$ci_rpm_arch-CHECKSUM" |
    sed -n 1p) || ci_sums=''
  [ -n "$ci_sums" ] || ci_die "no CHECKSUM file in $ci_listing/"
  ci_sums_url=$ci_listing/$ci_sums ci_algo=sha256
  # Any mirror: the checksum from the Fedora project's own server decides.
  ci_base=https://download.fedoraproject.org/pub/fedora/linux/releases/$ci_v/Cloud/$ci_rpm_arch/images
  ci_image=''
  ;;
*) ci_die "no cloud image for \"$ci_distro\"" 'known: debian-12 debian-13 ubuntu-22.04 ubuntu-24.04 ubuntu-26.04 fedora-44' ;;
esac
command -v "${ci_algo}sum" >/dev/null 2>&1 || ci_die "\"${ci_algo}sum\" is needed and not installed"

ci_dir=$(dirname -- "$ci_file")
mkdir -p "$ci_dir"
ci_tmp=$ci_file.part
ci_sums_file=$ci_file.sums
trap 'rm -f "$ci_tmp" "$ci_sums_file"' EXIT
trap 'exit 1' INT TERM

for ci_attempt in 1 2; do
  curl -fsSL --retry 3 -o "$ci_sums_file" "$ci_sums_url" || ci_die "could not download $ci_sums_url"
  if [ -z "$ci_image" ]; then
    # "SHA256 (Fedora-Cloud-Base-Generic-44-1.7.x86_64.qcow2) = <hex>"
    ci_image=$(awk -v v="$ci_v" -v a="$ci_rpm_arch" '
      $1 == "SHA256" && $2 ~ "^\\(Fedora-Cloud-Base-Generic-" v "-[0-9.]+\\." a "\\.qcow2\\)$" {
        gsub(/[()]/, "", $2); print $2; exit
      }' "$ci_sums_file")
  fi
  ci_want=$(awk -v f="$ci_image" '
    ($2 == f || $2 == "*" f) && $1 ~ /^[0-9a-f]+$/ { print $1; exit }
    $1 == "SHA256" && $2 == "(" f ")" { print $4; exit }' "$ci_sums_file")
  if [ -z "$ci_image" ] || [ -z "$ci_want" ]; then
    ci_die "$ci_sums_url lists no checksum for the image (${ci_image:-not found})"
  fi
  printf 'cloud-image: downloading %s\n' "$ci_base/$ci_image"
  curl -fsSL --retry 3 -o "$ci_tmp" "$ci_base/$ci_image" || ci_die "could not download $ci_base/$ci_image"
  if echo "$ci_want  $ci_tmp" | "${ci_algo}sum" -c --quiet -; then
    break
  fi
  [ "$ci_attempt" = 1 ] || ci_die "$ci_image does not match its $ci_algo checksum in $ci_sums_url"
  printf 'cloud-image: %s does not match its checksum, trying again\n' "$ci_image"
  # Fedora's image name comes from the checksum file (the release's respin), so it is read again too.
  [ -z "${ci_listing-}" ] || ci_image=''
done
mv "$ci_tmp" "$ci_file"

printf 'cloud-image: %s is %s (%s %s)\n' "$ci_file" "$ci_image" "$ci_algo" "$ci_want"
if [ -n "${GITHUB_STEP_SUMMARY-}" ]; then
  printf 'Cloud image of %s: %s (%s %s)\n' "$ci_distro" "$ci_image" "$ci_algo" "$ci_want" >>"$GITHUB_STEP_SUMMARY"
fi
