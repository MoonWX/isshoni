package signal

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strings"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// Resume tokens (01 §10.3):
//
//	"r1." + base64url(connIdRaw[10] ‖ nonce[16] ‖ HMAC-SHA256(resumeKey, "isshoni-resume-v1" ‖ connIdRaw ‖ nonce)[:16])
//
// The HMAC lets the hub drop forged tokens without a lookup, and tells "valid but from a previous process" apart from
// garbage (both are answered resumed: false). A connection keeps the SHA-256 of its current token and rotates it on
// every welcome. Resuming (the lookup, the identity check, the rotation and grace) comes with README S28; this file
// holds the format.
const (
	resumeTokenPrefix = "r1."
	resumeHMACLabel   = "isshoni-resume-v1"
	resumeNonceLen    = 16
	resumeMACLen      = 16
	resumeTokenRawLen = connIDRawLen + resumeNonceLen + resumeMACLen
)

// mintResumeToken returns a new resume token for the connection whose id has the random bytes raw, and the token's
// SHA-256, which the connection keeps as its current token.
func mintResumeToken(key []byte, raw [connIDRawLen]byte) (protocol.Secret, [sha256.Size]byte) {
	var nonce [resumeNonceLen]byte
	_, _ = rand.Read(nonce[:]) // crypto/rand.Read never fails (it crashes the program instead)
	b := make([]byte, 0, resumeTokenRawLen)
	b = append(b, raw[:]...)
	b = append(b, nonce[:]...)
	b = append(b, resumeMAC(key, raw[:], nonce[:])...)
	tok := resumeTokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	return protocol.Secret(tok), sha256.Sum256([]byte(tok))
}

// parseResumeToken checks a token's HMAC under key and returns the random bytes of the connection id it names. ok
// is false for anything that is not a token of this key: garbage, a forgery, or a token from before a key rotation.
func parseResumeToken(key []byte, tok string) (raw [connIDRawLen]byte, ok bool) {
	rest, found := strings.CutPrefix(tok, resumeTokenPrefix)
	if !found || len(rest) != base64.RawURLEncoding.EncodedLen(resumeTokenRawLen) {
		return raw, false
	}
	b, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil || len(b) != resumeTokenRawLen {
		return raw, false
	}
	id, nonce, mac := b[:connIDRawLen], b[connIDRawLen:connIDRawLen+resumeNonceLen], b[connIDRawLen+resumeNonceLen:]
	if !hmac.Equal(mac, resumeMAC(key, id, nonce)) {
		return raw, false
	}
	copy(raw[:], id)
	return raw, true
}

// resumeMAC is the token's truncated HMAC.
func resumeMAC(key, id, nonce []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(resumeHMACLabel))
	m.Write(id)
	m.Write(nonce)
	return m.Sum(nil)[:resumeMACLen]
}
