package update

import (
	"bytes"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"
)

// The verifier of the manifest's signature (06 §8.3): the armored SSHSIG that
//
//	ssh-keygen -Y sign -f <key> -n isshoni-update isshoni-desktop-update.json
//
// writes, checked as OpenSSH's PROTOCOL.sshsig describes it, with the subset that the update keys need: namespace
// isshoni-update, hash sha512, and a public key that is one of the trusted keys.

const (
	// Namespace is the SSHSIG namespace of update manifests, and also the principal of deploy/keys/update_signers
	// (06 §8.3, §11.5).
	Namespace = "isshoni-update"

	// MaxSignatureSize bounds the armored signature, as the download does (06 §8.3, §8.6).
	MaxSignatureSize = 8 << 10
)

const (
	sshsigMagic   = "SSHSIG"
	sshsigVersion = 1
	sshsigHash    = "sha512"
	armorBegin    = "-----BEGIN SSH SIGNATURE-----"
	armorEnd      = "-----END SSH SIGNATURE-----"
)

// fingerprintRE matches a SHA256 key fingerprint as `ssh-keygen -l` prints it: "SHA256:" and the unpadded base64 of
// the 32-byte SHA-256 of the key's wire form.
var fingerprintRE = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)

// TrustedKey is a public key that verifies update manifests.
type TrustedKey struct {
	Name   string        // the key's comment, such as isshoni-update-1
	Backup bool          // the offline backup key, whose manifests may revoke keys (06 §8.1)
	Key    ssh.PublicKey // an ssh-ed25519 key
}

// Fingerprint returns the key's SHA256 fingerprint as `ssh-keygen -l` prints it (SHA256:…), the form of revokedKeys.
func (k TrustedKey) Fingerprint() string { return ssh.FingerprintSHA256(k.Key) }

// Trusted returns keys minus the keys whose fingerprints revoked lists, the persisted revocations of state.json
// (06 §8.3 step 3). An entry that names a backup key is ignored: the backup changes only through an app release that
// embeds a new one (06 §8.1, §8.12).
func Trusted(keys []TrustedKey, revoked []string) []TrustedKey {
	gone := make(map[string]bool, len(revoked))
	for _, fp := range revoked {
		gone[fp] = true
	}
	out := make([]TrustedKey, 0, len(keys))
	for _, k := range keys {
		if k.Backup || !gone[k.Fingerprint()] {
			out = append(out, k)
		}
	}
	return out
}

// VerifySSHSIG checks that armored, an armored SSHSIG in the namespace isshoni-update, is a signature of message by
// one of keys, which are already the trusted set (Trusted). It returns the key that made the signature; whether it
// is the backup key decides whether the manifest's revokedKeys is honoured (06 §8.1). Every failure is an *Error with
// CodeBadSignature.
func VerifySSHSIG(message, armored []byte, keys []TrustedKey) (TrustedKey, error) {
	k, err := verifySSHSIG(message, armored, Namespace, keys)
	if err != nil {
		return TrustedKey{}, &Error{Code: CodeBadSignature, Err: err}
	}
	return k, nil
}

// verifySSHSIG is VerifySSHSIG for any namespace, with plain errors.
func verifySSHSIG(message, armored []byte, namespace string, keys []TrustedKey) (TrustedKey, error) {
	blob, err := dearmor(armored)
	if err != nil {
		return TrustedKey{}, err
	}
	sig, err := parseSSHSIG(blob)
	if err != nil {
		return TrustedKey{}, err
	}
	if sig.namespace != namespace {
		return TrustedKey{}, fmt.Errorf("sshsig: namespace %q, want %q", truncate(sig.namespace), namespace)
	}
	if sig.hashAlg != sshsigHash {
		return TrustedKey{}, fmt.Errorf("sshsig: hash algorithm %q, want %q", truncate(sig.hashAlg), sshsigHash)
	}
	key, ok := matchKey(sig.publicKey, keys)
	if !ok {
		return TrustedKey{}, errors.New("sshsig: signed by a key that is not trusted")
	}
	var s struct {
		Format string
		Blob   []byte
		Rest   []byte `ssh:"rest"`
	}
	if err := ssh.Unmarshal(sig.signature, &s); err != nil {
		return TrustedKey{}, fmt.Errorf("sshsig: signature: %w", err)
	}
	if len(s.Rest) != 0 {
		return TrustedKey{}, errors.New("sshsig: trailing bytes after the signature")
	}
	if err := key.Key.Verify(signedData(sig, message), &ssh.Signature{Format: s.Format, Blob: s.Blob}); err != nil {
		return TrustedKey{}, fmt.Errorf("sshsig: %w", err)
	}
	return key, nil
}

// matchKey returns the key of keys whose wire form is publicKey.
func matchKey(publicKey []byte, keys []TrustedKey) (TrustedKey, bool) {
	for _, k := range keys {
		if bytes.Equal(k.Key.Marshal(), publicKey) {
			return k, true
		}
	}
	return TrustedKey{}, false
}

// sshsig is a parsed SSHSIG blob (PROTOCOL.sshsig, "Blob format").
type sshsig struct {
	publicKey []byte // the signer's public key, wire form
	namespace string
	reserved  []byte
	hashAlg   string
	signature []byte // an SSH signature: string format, string blob
}

// signedData returns what the signature covers (PROTOCOL.sshsig, "Signed Data"): the magic, the namespace, the
// reserved field, the hash algorithm and the hash of the message, each but the magic as an SSH string.
func signedData(sig *sshsig, message []byte) []byte {
	sum := sha512.Sum512(message)
	var b []byte
	b = append(b, sshsigMagic...)
	b = appendString(b, []byte(sig.namespace))
	b = appendString(b, sig.reserved)
	b = appendString(b, []byte(sig.hashAlg))
	b = appendString(b, sum[:])
	return b
}

// marshal returns the blob form of sig; parseSSHSIG(sig.marshal()) gives sig again.
func (sig *sshsig) marshal() []byte {
	var b []byte
	b = append(b, sshsigMagic...)
	b = binary.BigEndian.AppendUint32(b, sshsigVersion)
	b = appendString(b, sig.publicKey)
	b = appendString(b, []byte(sig.namespace))
	b = appendString(b, sig.reserved)
	b = appendString(b, []byte(sig.hashAlg))
	b = appendString(b, sig.signature)
	return b
}

func appendString(b, s []byte) []byte {
	b = binary.BigEndian.AppendUint32(b, uint32(len(s))) //nolint:gosec // every caller's s is far below 4 GiB
	return append(b, s...)
}

// parseSSHSIG parses an SSHSIG blob: the magic SSHSIG, uint32 version 1, then the strings public key, namespace,
// reserved, hash algorithm and signature, and no trailing bytes (06 §8.3 step 2).
func parseSSHSIG(blob []byte) (*sshsig, error) {
	rest, ok := bytes.CutPrefix(blob, []byte(sshsigMagic))
	if !ok {
		return nil, errors.New("sshsig: no SSHSIG magic")
	}
	if len(rest) < 4 {
		return nil, errors.New("sshsig: truncated before the version")
	}
	if v := binary.BigEndian.Uint32(rest); v != sshsigVersion {
		return nil, fmt.Errorf("sshsig: version %d, want %d", v, sshsigVersion)
	}
	rest = rest[4:]
	var fields [5][]byte
	for i, name := range [...]string{"public key", "namespace", "reserved", "hash algorithm", "signature"} {
		var err error
		if fields[i], rest, err = readString(rest); err != nil {
			return nil, fmt.Errorf("sshsig: %s: %w", name, err)
		}
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("sshsig: %d trailing bytes", len(rest))
	}
	return &sshsig{
		publicKey: fields[0],
		namespace: string(fields[1]),
		reserved:  fields[2],
		hashAlg:   string(fields[3]),
		signature: fields[4],
	}, nil
}

// readString reads an SSH string (uint32 length, then the bytes) from the start of b.
func readString(b []byte) (s, rest []byte, err error) {
	if len(b) < 4 {
		return nil, nil, errors.New("truncated length")
	}
	n, rest := binary.BigEndian.Uint32(b), b[4:]
	if int(n) > len(rest) || int(n) < 0 {
		return nil, nil, fmt.Errorf("length %d past the end", n)
	}
	return rest[:n], rest[n:], nil
}

// dearmor returns the blob of an armored SSHSIG (06 §8.3 step 1): the BEGIN line, the base64 lines, the END line,
// optionally followed by line ends. CRLF line ends are accepted; anything else before BEGIN or after END, a character
// outside base64 in the body, or input above MaxSignatureSize is refused.
func dearmor(armored []byte) ([]byte, error) {
	if len(armored) > MaxSignatureSize {
		return nil, fmt.Errorf("sshsig: signature of %d bytes is above the %d-byte limit", len(armored), MaxSignatureSize)
	}
	text := string(bytes.TrimRight(armored, "\r\n"))
	body, ok := cutLine(text, armorBegin)
	if !ok {
		return nil, fmt.Errorf("sshsig: no %q line at the start", armorBegin)
	}
	i := len(body) - len(armorEnd)
	if i < 1 || body[i:] != armorEnd || body[i-1] != '\n' {
		return nil, fmt.Errorf("sshsig: no %q line at the end", armorEnd)
	}
	body = body[:i]
	b64 := make([]byte, 0, len(body))
	for j := range len(body) {
		switch c := body[j]; {
		case c == '\n' || c == '\r':
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '+', c == '/', c == '=':
			b64 = append(b64, c)
		default:
			return nil, fmt.Errorf("sshsig: byte 0x%02x in the base64 body", c)
		}
	}
	blob, err := base64.StdEncoding.Strict().DecodeString(string(b64))
	if err != nil {
		return nil, fmt.Errorf("sshsig: base64: %w", err)
	}
	return blob, nil
}

// cutLine returns what follows the first line of text when that line is exactly line (with an LF or CRLF end).
func cutLine(text, line string) (string, bool) {
	rest, ok := strings.CutPrefix(text, line)
	if !ok {
		return "", false
	}
	if r, ok := strings.CutPrefix(rest, "\r\n"); ok {
		return r, true
	}
	return strings.CutPrefix(rest, "\n")
}

// truncate shortens a string from a signature for an error message.
func truncate(s string) string {
	const limit = 64
	if len(s) > limit {
		return s[:limit] + "…"
	}
	return s
}
