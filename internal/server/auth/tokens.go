package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
)

// Keys are the token-hashing keys from 04's secrets.json: 32 bytes each (03 §3.2). Session hashes session tokens
// (and, in M2, device codes, user codes and bearer tokens); Invite hashes setup, invite and password-reset tokens.
// Rotating one key invalidates exactly the tokens it protects (§4.6).
//
// Keys never print: every fmt verb, slog (a top-level attribute or a field of a logged struct, in the text and the
// JSON handler), encoding/json and every encoding.TextMarshaler user (TOML, XML) show a redacted placeholder.
type Keys struct{ Session, Invite []byte }

// keySize is the required length of each key in Keys.
const keySize = 32

// validate reports a key of the wrong length.
func (k Keys) validate() error {
	if len(k.Session) != keySize {
		return fmt.Errorf("auth: session key has %d bytes, want %d", len(k.Session), keySize)
	}
	if len(k.Invite) != keySize {
		return fmt.Errorf("auth: invite key has %d bytes, want %d", len(k.Invite), keySize)
	}
	return nil
}

// Format implements fmt.Formatter so that no verb (%v, %+v, %#v, %x, %s …) prints key bytes.
func (k Keys) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, "auth.Keys{REDACTED}") }

// LogValue implements slog.LogValuer so that a Keys value logged by mistake shows no key bytes.
func (k Keys) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON implements json.Marshaler. slog's JSON handler (the production log format) encodes a struct that holds
// Keys, such as the service's Options, with encoding/json, which would otherwise print both keys in base64.
func (k Keys) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText implements encoding.TextMarshaler for the encoders that use it, such as TOML and encoding/xml. (slog's
// text handler prints a struct that holds Keys with %+v, which Format covers.)
func (k Keys) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// redacted replaces key material wherever Keys is printed or encoded.
const redacted = "REDACTED"

// Random sizes of the M1 tokens, in bytes (03 §3.2). Every token is base64url without padding: 32 bytes are
// 43 characters, 24 bytes are 32 characters.
const (
	sessionTokenBytes = 32
	setupTokenBytes   = 32
	inviteTokenBytes  = 24
	resetTokenBytes   = 32
)

// newToken returns n bytes from crypto/rand as base64url without padding.
func newToken(n int) string {
	b := make([]byte, n)
	// crypto/rand.Read never returns an error since Go 1.24: it crashes the program instead.
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// tokenWellFormed reports whether tok has the exact shape of an n-byte token: canonical base64url without padding.
// Callers reject other input before any hashing or DB lookup.
func tokenWellFormed(tok string, n int) bool {
	if len(tok) != base64.RawURLEncoding.EncodedLen(n) {
		return false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(tok)
	return err == nil && len(b) == n
}

// keyedHash is the stored form of a token: HMAC-SHA-256(key, the token's ASCII bytes), 32 bytes (03 §3.2). A copied
// DB or backup yields no usable token without the key.
func keyedHash(key []byte, tok string) []byte {
	m := hmac.New(sha256.New, key)
	_, _ = io.WriteString(m, tok) // a hash.Hash never returns a write error
	return m.Sum(nil)
}

// sessionTokenHash hashes a session token with the session key.
func (k Keys) sessionTokenHash(tok string) []byte { return keyedHash(k.Session, tok) }

// inviteTokenHash hashes a setup, invite or password-reset token with the invite key.
func (k Keys) inviteTokenHash(tok string) []byte { return keyedHash(k.Invite, tok) }

// keyFingerprint is the value of the meta keys session_key_fp and invite_key_fp: the hex of the first 8 bytes of
// SHA-256(key) (03 §4.6). It identifies a key without revealing it.
func keyFingerprint(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:8])
}

// Names of the keys in the secrets.rotated audit detail ({keys: [...]}, 03 §4.6).
const (
	keyNameSession = "session"
	keyNameInvite  = "invite"
)

// rotatedKeys compares the fingerprints stored in meta with the current keys and names the keys that changed, in
// the order session, invite. An empty stored fingerprint means a first start (nothing stored yet), not a rotation.
// New purges the rows each changed key protects, audits secrets.rotated and raises the secrets_rotated alert.
func (k Keys) rotatedKeys(storedSessionFP, storedInviteFP string) []string {
	var changed []string
	if storedSessionFP != "" && storedSessionFP != keyFingerprint(k.Session) {
		changed = append(changed, keyNameSession)
	}
	if storedInviteFP != "" && storedInviteFP != keyFingerprint(k.Invite) {
		changed = append(changed, keyNameInvite)
	}
	return changed
}
