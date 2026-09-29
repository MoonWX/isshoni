#!/bin/sh
# Runs the self-test as isshoni-s2.app through LaunchServices, so macOS asks
# for (and remembers) the audio permission for isshoni S2, not your terminal.
# Arguments go to `s2 selftest` (e.g. -only exclude,self -mic -keep).
set -eu
here=$(cd "$(dirname "$0")" && pwd)
app="$here/dist/isshoni-s2.app"
[ -d "$app" ] || { echo "build first: ./build.sh" >&2; exit 1; }
log="$here/dist/selftest.log"
err="$here/dist/selftest.err"
: > "$log"
: > "$err"
open -W -n -g --stdout "$log" --stderr "$err" "$app" --args selftest -report "$here/dist/s2-report.json" "$@" &
openpid=$!
tail -n +1 -f "$log" &
tailpid=$!
wait "$openpid" || true
sleep 0.5
kill "$tailpid" 2>/dev/null || true
if [ -s "$err" ]; then
  echo "--- stderr:"
  cat "$err"
fi
