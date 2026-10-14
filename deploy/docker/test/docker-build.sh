#!/bin/sh
# Builds the isshoni image from this working tree, for tests (docs/m1/06-deploy-and-ci.md §6.1, §11.3).
#
# A release image is built by goreleaser's dockers_v2. This script builds the same image without goreleaser: it
# cross-compiles the server for linux/<arch>, lays out a build context like goreleaser's (linux/<arch>/isshoni,
# deploy/docker/rootfs/, LICENSE, NOTICE, THIRD_PARTY_NOTICES) and runs `docker buildx build --load` with
# deploy/docker/Dockerfile. `task docker:smoke` uses it through docker-smoke.sh.
#
# Usage: docker-build.sh [-a ARCH] [-t IMAGE] [-v VERSION]
#   -a ARCH     amd64 or arm64 (default: the architecture of the Docker engine)
#   -t IMAGE    name of the image to load into Docker (default: isshoni:local)
#   -v VERSION  version of the binary (default: the version web/dist was built for, so binary and web app agree)
#
# Needs go, git and docker with buildx, the built web app in web/dist (task build:web) and THIRD_PARTY_NOTICES
# (task notices). POSIX sh, as every script under deploy/.

set -eu

VERSION_PKG=github.com/MoonWX/isshoni/internal/version

say() { printf '%s\n' "$*"; }

die() {
  printf 'docker-build: %s\n' "$*" >&2
  exit 1
}

usage() {
  sed -n '/^# Usage:/,/^#$/p' "$0" | sed -e '$d' -e 's/^# \{0,1\}//'
}

# repo_root prints the repository's root: the first directory above this script that has a go.mod and a Taskfile.yml.
repo_root() {
  dir=$(cd "$(dirname "$0")" && pwd)
  while [ "$dir" != / ]; do
    if [ -f "$dir/go.mod" ] && [ -f "$dir/Taskfile.yml" ]; then
      printf '%s\n' "$dir"
      return 0
    fi
    dir=$(dirname "$dir")
  done
  return 1
}

# engine_arch prints the Docker engine's architecture in Go's spelling.
engine_arch() {
  machine=$(docker info --format '{{.Architecture}}') || die "can't reach the Docker engine: is Docker running?"
  case $machine in
  x86_64 | amd64) say amd64 ;;
  aarch64 | arm64) say arm64 ;;
  *) die "the Docker engine runs on $machine; isshoni images exist for amd64 and arm64 only (pass -a)" ;;
  esac
}

# web_version prints the version that web/dist was built for (web/dist/version.json), read without Node.
web_version() {
  sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' web/dist/version.json 2>/dev/null
}

cleanup() {
  [ -z "${context:-}" ] || rm -rf "$context"
}

main() {
  arch='' image=isshoni:local version=''
  while getopts a:t:v:h opt; do
    case $opt in
    a) arch=$OPTARG ;;
    t) image=$OPTARG ;;
    v) version=$OPTARG ;;
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

  root=$(repo_root) || die "can't find the repository root above $0"
  cd "$root"
  for tool in go docker; do
    command -v "$tool" >/dev/null || die "$tool is not installed"
  done
  docker buildx version >/dev/null 2>&1 || die "docker buildx is not installed"

  [ -n "$arch" ] || arch=$(engine_arch)
  case $arch in
  amd64 | arm64) ;;
  *) die "-a $arch: want amd64 or arm64" ;;
  esac
  [ -f web/dist/index.html ] ||
    die 'web/dist/index.html is missing, and the binary embeds the web app: run "task build:web" first'
  [ -f THIRD_PARTY_NOTICES ] ||
    die 'THIRD_PARTY_NOTICES is missing, and the image ships it: run "task notices" first'
  [ -n "$version" ] || version=$(web_version)
  [ -n "$version" ] || die "web/dist/version.json has no version: pass -v VERSION"

  context=$(mktemp -d "${TMPDIR:-/tmp}/isshoni-image.XXXXXX")
  trap cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  # The flags of `task build:go` (06 §7.2). Outside a git checkout the commit and its date stay empty.
  ldflags="-s -w -X $VERSION_PKG.version=$version"
  if commit=$(git rev-parse HEAD 2>/dev/null); then
    date=$(TZ=UTC0 git log -1 --date=format-local:%Y-%m-%dT%H:%M:%SZ --format=%cd)
    ldflags="$ldflags -X $VERSION_PKG.commit=$commit -X $VERSION_PKG.date=$date"
  fi
  say "docker-build: go build for linux/$arch, version $version"
  mkdir -p "$context/linux/$arch" "$context/deploy/docker"
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags "$ldflags" \
    -o "$context/linux/$arch/isshoni" ./cmd/isshoni
  cp -R deploy/docker/rootfs "$context/deploy/docker/rootfs"
  cp LICENSE NOTICE THIRD_PARTY_NOTICES "$context/"
  # The Dockerfile sets the modes of the binary and of the two directories. These three files keep the mode they
  # have in the context, and that must not be this shell's umask: everybody may read a license.
  chmod 0644 "$context/LICENSE" "$context/NOTICE" "$context/THIRD_PARTY_NOTICES"

  say "docker-build: docker buildx build --platform linux/$arch -> $image"
  # No attestations: with them the result is an image index, which not every image store can --load.
  docker buildx build --platform "linux/$arch" --load --provenance=false --sbom=false \
    -f deploy/docker/Dockerfile -t "$image" "$context"
}

main "$@"
