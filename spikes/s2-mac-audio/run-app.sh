#!/bin/sh
# Runs `s2 <args>` as isshoni-s2.app through LaunchServices (so macOS
# attributes permissions to isshoni S2) and prints its output.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
app="$here/dist/isshoni-s2.app"
[ -d "$app" ] || { echo "build first: ./build.sh" >&2; exit 1; }
out="$here/dist/run-app.log"
: > "$out"
open -W -n -g --stdout "$out" --stderr "$out.err" "$app" --args "$@"
cat "$out"
[ -s "$out.err" ] && cat "$out.err" || true
