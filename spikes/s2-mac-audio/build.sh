#!/bin/sh
# Builds dist/isshoni-s2.app plus the fake apps the self-test uses
# (dist/fixtures), all signed with one stable identity (see dev-signing.sh) and
# the hardened runtime, so macOS keeps the System Audio Recording grant across
# rebuilds. S2_BUILD=<n> sets the bundle version (for the update-persistence check).
set -eu
here=$(cd "$(dirname "$0")" && pwd)
out="$here/dist"
: "${SDKROOT:=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk}"
export SDKROOT CGO_ENABLED=1 MACOSX_DEPLOYMENT_TARGET=14.4
# Also for Go's own cgo objects (runtime/cgo), not just this package.
export CGO_CFLAGS="${CGO_CFLAGS:--O2 -g} -mmacosx-version-min=14.4" CGO_LDFLAGS="${CGO_LDFLAGS:--O2 -g} -mmacosx-version-min=14.4"
keychain=${ISSHONI_DEV_KEYCHAIN:-$HOME/.isshoni-dev/signing/isshoni-dev.keychain-db}
[ -f "$keychain" ] || { echo "no signing keychain at $keychain: run ./dev-signing.sh first" >&2; exit 1; }
security unlock-keychain -p "" "$keychain"
identity=$(security find-identity -p codesigning "$keychain" | awk '/"isshoni Dev Code Signing"/ {print $2; exit}')
[ -n "$identity" ] || { echo "identity not found in $keychain" >&2; exit 1; }

rm -rf "$out"
mkdir -p "$out/fixtures"
(cd "$here" && go build -trimpath -o "$out/s2.bin" .)

cat > "$out/audio-input.entitlements" <<'XML'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>com.apple.security.device.audio-input</key><true/></dict></plist>
XML

# mkapp <path.app> <executable> <bundle id> <name> [extra Info.plist XML]
mkapp() {
  mkdir -p "$1/Contents/MacOS"
  cp "$out/s2.bin" "$1/Contents/MacOS/$2"
  cat > "$1/Contents/Info.plist" <<XML
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleIdentifier</key><string>$3</string>
  <key>CFBundleExecutable</key><string>$2</string>
  <key>CFBundleName</key><string>$4</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>0.0.${S2_BUILD:-1}</string>
  <key>CFBundleVersion</key><string>${S2_BUILD:-1}</string>
  <key>LSMinimumSystemVersion</key><string>14.4</string>
  <key>LSUIElement</key><true/>
  ${5:-}
</dict></plist>
XML
}

# sign <path> [entitlements]
sign() {
  if [ $# -gt 1 ]; then
    codesign --force --options runtime --timestamp=none --keychain "$keychain" -s "$identity" --entitlements "$2" "$1"
  else
    codesign --force --options runtime --timestamp=none --keychain "$keychain" -s "$identity" "$1"
  fi
}

fx="$out/fixtures"
mkapp "$fx/FakeGame.app" FakeGame com.isshoni.test.game "Fake Game"
mkapp "$fx/FakeGame2.app" FakeGame2 com.isshoni.test.game2 "Fake Game 2"
mkapp "$fx/FakeVoice.app" FakeVoice com.isshoni.test.fakevoice "Fake Voice"
mkapp "$fx/FakeVoice.app/Contents/Frameworks/FakeVoice Helper.app" "FakeVoice Helper" com.isshoni.test.fakevoice.helper "FakeVoice Helper"
mkapp "$fx/FakeTS.app" FakeTS com.isshoni.test.faketeamspeak "Fake TeamSpeak"
mkapp "$fx/FakeBrowser.app" FakeBrowser com.isshoni.test.browser "Fake Browser"
mkapp "$fx/FakeVoIP.app" FakeVoIP com.isshoni.test.fakevoip "Fake VoIP" \
  '<key>NSMicrophoneUsageDescription</key><string>isshoni self-test: this fake voice app holds the microphone so isshoni can detect it.</string>'
cp "$out/s2.bin" "$fx/voice-cli-helper"
mkapp "$out/isshoni-s2.app" s2 io.isshoni.spike.s2 "isshoni S2" \
  '<key>NSAudioCaptureUsageDescription</key><string>isshoni shares your system audio with friends, minus voice apps. This test checks that.</string>
  <key>NSLocalNetworkUsageDescription</key><string>isshoni connects to servers and friends on your local network.</string>'
rm "$out/s2.bin"

# Inner code first.
sign "$fx/FakeVoice.app/Contents/Frameworks/FakeVoice Helper.app"
for a in FakeGame FakeGame2 FakeVoice FakeTS FakeBrowser; do sign "$fx/$a.app"; done
sign "$fx/FakeVoIP.app" "$out/audio-input.entitlements"
codesign --force --options runtime --timestamp=none --keychain "$keychain" -s "$identity" -i io.isshoni.test.voice-cli-helper "$fx/voice-cli-helper"
sign "$out/isshoni-s2.app" "$out/audio-input.entitlements"
codesign --verify --strict "$out/isshoni-s2.app"
echo "built $out/isshoni-s2.app (build ${S2_BUILD:-1})"
codesign -dr - "$out/isshoni-s2.app" 2>&1 | sed -n 's/^designated => /designated requirement: /p'
