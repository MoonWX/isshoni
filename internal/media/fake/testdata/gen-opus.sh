#!/bin/sh
# gen-opus.sh regenerates tone.opus, the Opus asset of the fake audio (docs/m1/02-sfu.md §15.1). Its output is
# committed, so CI never runs it and never needs opus-tools.
#
# It writes two seconds of a periodic test tone (gentone.go: a 440 Hz bed and a 1 kHz beep in the first 100 ms of
# each second) and encodes them with opusenc (opus-tools and libopus, both BSD) at 64 kbit/s in 20 ms packets. The
# fake uses the second second (packets 50–99), whose encoder state has a whole period behind it, as a
# seamless one-second loop (opus.go).
#
# Needs Go and opusenc. Without a local opus-tools, set OPUSENC_IMAGE to a Debian or Ubuntu image (for example
# OPUSENC_IMAGE=ubuntu:24.04) to run opusenc in a throwaway container with Docker.
set -eu

# PRESKIP must equal libopus's pre-skip at 48 kHz (opus.go's assetPreSkip; TestOpusAsset checks the file's).
PRESKIP=312

cd "$(dirname "$0")"

encode() {
  if [ -n "${OPUSENC_IMAGE:-}" ]; then
    docker run --rm -i "$OPUSENC_IMAGE" sh -c '
      apt-get -qq update >&2 &&
        DEBIAN_FRONTEND=noninteractive apt-get -qq install -y opus-tools >&2 &&
        opusenc "$@"' opusenc "$@"
  else
    opusenc "$@"
  fi
}

go run gentone.go -seconds 2 -shift "$PRESKIP" |
  encode --quiet --raw --raw-bits 16 --raw-rate 48000 --raw-chan 2 --raw-endianness 0 \
    --bitrate 64 --vbr --framesize 20 --comp 10 --serial 1 --padding 0 - - >tone.opus.tmp
mv tone.opus.tmp tone.opus
echo "gen-opus.sh: wrote $(wc -c <tone.opus | tr -d ' ') bytes to tone.opus"
