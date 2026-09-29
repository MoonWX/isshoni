#!/bin/sh
# Creates the stable self-signed code-signing identity used for local builds
# (plan: one certificate that never changes, so macOS keeps TCC grants across
# rebuilds and updates). Everything stays outside the repository, in its own
# keychain; no system trust settings are changed. codesign accepts the
# untrusted identity when it is selected by its SHA-1 hash.
set -eu
dir=${ISSHONI_DEV_SIGNING_DIR:-$HOME/.isshoni-dev/signing}
kc="$dir/isshoni-dev.keychain-db"
mkdir -p "$dir"
chmod 700 "$dir"
cd "$dir"
if [ ! -f cert.pem ]; then
  # RSA: `security import` on macOS 27 crashes on EC keys from LibreSSL's PKCS#12.
  /usr/bin/openssl req -x509 -newkey rsa:3072 -nodes -keyout key.pem -out cert.pem -days 10950 \
    -subj "/CN=isshoni Dev Code Signing/O=isshoni dev" \
    -addext "basicConstraints=critical,CA:false" -addext "keyUsage=critical,digitalSignature" \
    -addext "extendedKeyUsage=critical,codeSigning" 2>/dev/null
  chmod 600 key.pem
  /usr/bin/openssl pkcs12 -export -inkey key.pem -in cert.pem -name "isshoni Dev Code Signing" -out id.p12 -passout pass:isshoni-dev
fi
if [ ! -f "$kc" ]; then
  security create-keychain -p "" "$kc" # not added to the search list
  security set-keychain-settings "$kc" # no auto-lock
fi
security unlock-keychain -p "" "$kc"
if ! security find-identity -p codesigning "$kc" | grep -q "isshoni Dev Code Signing"; then
  security import id.p12 -k "$kc" -P isshoni-dev -T /usr/bin/codesign -f pkcs12
  security set-key-partition-list -S apple-tool:,apple:,codesign: -s -k "" "$kc" >/dev/null
fi
security find-identity -p codesigning "$kc" | sed -n '/Matching/,/found/p'
