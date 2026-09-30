package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

func testKeys() Keys {
	return Keys{Session: bytes.Repeat([]byte{0x11}, 32), Invite: bytes.Repeat([]byte{0x22}, 32)}
}

func TestNewTokenShape(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bytes int
		chars int
	}{
		{"session", sessionTokenBytes, 43},
		{"setup", setupTokenBytes, 43},
		{"invite", inviteTokenBytes, 32},
		{"reset", resetTokenBytes, 43},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := map[string]bool{}
			for range 100 {
				tok := newToken(tc.bytes)
				if len(tok) != tc.chars {
					t.Fatalf("newToken(%d) = %q: %d chars, want %d", tc.bytes, tok, len(tok), tc.chars)
				}
				if strings.ContainsAny(tok, "+/=") {
					t.Fatalf("newToken(%d) = %q: not base64url without padding", tc.bytes, tok)
				}
				if !tokenWellFormed(tok, tc.bytes) {
					t.Fatalf("tokenWellFormed(%q, %d) = false", tok, tc.bytes)
				}
				if seen[tok] {
					t.Fatalf("newToken(%d) repeated %q", tc.bytes, tok)
				}
				seen[tok] = true
			}
		})
	}
}

func TestTokenWellFormed(t *testing.T) {
	good := newToken(32)
	for _, tc := range []struct {
		tok  string
		n    int
		want bool
	}{
		{good, 32, true},
		{good, 24, false},
		{good[:42], 32, false},
		{good + "A", 32, false},
		{"", 32, false},
		{strings.Repeat("A", 42) + "+", 32, false}, // standard alphabet
		{strings.Repeat("A", 42) + "=", 32, false}, // padding
		{strings.Repeat("A", 42) + "B", 32, false}, // non-canonical: the last char's unused bits are set
		{strings.Repeat("A", 42) + "E", 32, true},
		{strings.Repeat("_", 32), 24, true},
	} {
		if got := tokenWellFormed(tc.tok, tc.n); got != tc.want {
			t.Errorf("tokenWellFormed(%q, %d) = %v, want %v", tc.tok, tc.n, got, tc.want)
		}
	}
}

func TestKeyedHash(t *testing.T) {
	k := testKeys()
	tok := "dGhpcyBpcyBhIHRlc3QgdG9rZW4gb2YgMzIgYnl0ZXM"

	// HMAC-SHA-256 over the token's ASCII bytes, with the key of the token's kind.
	want := hmac.New(sha256.New, k.Session)
	want.Write([]byte(tok))
	got := k.sessionTokenHash(tok)
	if !bytes.Equal(got, want.Sum(nil)) || len(got) != 32 {
		t.Fatalf("sessionTokenHash = %x, want %x", got, want.Sum(nil))
	}
	if bytes.Equal(k.inviteTokenHash(tok), got) {
		t.Fatal("the invite key and the session key give the same hash")
	}
	if !bytes.Equal(keyedHash(k.Invite, tok), k.inviteTokenHash(tok)) {
		t.Fatal("inviteTokenHash does not use the invite key")
	}
	// Deterministic, and sensitive to every byte of the token and the key.
	if !bytes.Equal(k.sessionTokenHash(tok), got) {
		t.Fatal("sessionTokenHash is not deterministic")
	}
	if bytes.Equal(k.sessionTokenHash(tok[:len(tok)-1]+"x"), got) {
		t.Fatal("a changed token gives the same hash")
	}
	other := k
	other.Session = bytes.Repeat([]byte{0x12}, 32)
	if bytes.Equal(other.sessionTokenHash(tok), got) {
		t.Fatal("a rotated key gives the same hash")
	}
	// A published HMAC-SHA-256 test vector, so the stored format cannot drift.
	if h := hex.EncodeToString(keyedHash([]byte("key"), "The quick brown fox jumps over the lazy dog")); h !=
		"f7bc83f430538424b13298e6aa6fb143ef4d59a14946175997479dbc2d1a3cd8" {
		t.Fatalf("HMAC-SHA-256 known answer = %s", h)
	}
}

func TestKeyFingerprint(t *testing.T) {
	k := testKeys()
	fp := keyFingerprint(k.Session)
	sum := sha256.Sum256(k.Session)
	if fp != hex.EncodeToString(sum[:8]) || len(fp) != 16 {
		t.Fatalf("keyFingerprint = %q, want the hex of the first 8 bytes of SHA-256", fp)
	}
	if keyFingerprint(k.Invite) == fp {
		t.Fatal("two keys share a fingerprint")
	}
}

func TestRotatedKeys(t *testing.T) {
	k := testKeys()
	sfp, ifp := keyFingerprint(k.Session), keyFingerprint(k.Invite)
	for _, tc := range []struct {
		name                     string
		storedSession, storedInv string
		want                     []string
	}{
		{"first start", "", "", nil},
		{"unchanged", sfp, ifp, nil},
		{"session rotated", "0000000000000000", ifp, []string{"session"}},
		{"invite rotated", sfp, "0000000000000000", []string{"invite"}},
		{"both rotated", "0000000000000000", "1111111111111111", []string{"session", "invite"}},
		{"session stored only", sfp, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := k.rotatedKeys(tc.storedSession, tc.storedInv); !slices.Equal(got, tc.want) {
				t.Fatalf("rotatedKeys = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestKeysValidate(t *testing.T) {
	if err := testKeys().validate(); err != nil {
		t.Fatalf("validate(32-byte keys) = %v", err)
	}
	for _, k := range []Keys{
		{},
		{Session: make([]byte, 32)},
		{Session: make([]byte, 31), Invite: make([]byte, 32)},
		{Session: make([]byte, 32), Invite: make([]byte, 33)},
	} {
		if err := k.validate(); err == nil {
			t.Errorf("validate(%d, %d bytes) = nil", len(k.Session), len(k.Invite))
		}
	}
}

func TestKeysNeverPrint(t *testing.T) {
	k := testKeys()
	hexKey := hex.EncodeToString(k.Session)
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x", "%X", "%q", "%d"} {
		out := fmt.Sprintf(verb, k)
		if strings.Contains(out, hexKey) || strings.Contains(strings.ToLower(out), "1111") ||
			strings.Contains(out, "17 17") || strings.Contains(out, "\x11") {
			t.Errorf("fmt %s printed key material: %q", verb, out)
		}
	}
	for _, verb := range []string{"%v", "%+v"} {
		out := fmt.Sprintf(verb, struct{ K Keys }{k})
		if strings.Contains(out, "17") || strings.Contains(out, "1111") {
			t.Errorf("fmt %s of a struct holding Keys printed key material: %q", verb, out)
		}
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "keys", k)
	if !strings.Contains(buf.String(), `"keys":"REDACTED"`) {
		t.Errorf("slog output %q does not redact Keys", buf.String())
	}

	// Keys as a field of a logged struct (the service's Options holds one): slog's JSON handler encodes the struct
	// with encoding/json and its text handler with encoding.TextMarshaler or %+v, never through LogValue.
	b64Session := base64.StdEncoding.EncodeToString(k.Session)[:8] // "ERERERER"
	b64Invite := base64.StdEncoding.EncodeToString(k.Invite)[:8]   // "IiIiIiIi"
	leaks := func(out string) bool {
		return strings.Contains(out, b64Session) || strings.Contains(out, b64Invite) ||
			strings.Contains(out, hexKey) || strings.Contains(out, "17,17")
	}
	type options struct {
		Name string
		Keys Keys
	}
	opts := options{Name: "isshoni", Keys: k}
	outputs := map[string]string{}
	for name, h := range map[string]func(*bytes.Buffer) slog.Handler{
		"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
		"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
	} {
		var b bytes.Buffer
		slog.New(h(&b)).Info("x", "opts", opts, "ptr", &opts, "group", slog.GroupValue(slog.Any("keys", k)))
		outputs["slog "+name+" handler"] = b.String()
	}
	for name, v := range map[string]any{"Keys": k, "*Keys": &k, "struct": opts, "*struct": &opts} {
		j, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("json.Marshal(%s): %v", name, err)
		}
		outputs["json.Marshal "+name] = string(j)
	}
	if j, _ := json.Marshal(opts); string(j) != `{"Name":"isshoni","Keys":"REDACTED"}` {
		t.Errorf("json.Marshal of a struct holding Keys = %s", j)
	}
	if txt, err := k.MarshalText(); err != nil || string(txt) != "REDACTED" {
		t.Errorf("MarshalText = %q, %v", txt, err)
	}
	for name, out := range outputs {
		if leaks(out) || !strings.Contains(out, "REDACTED") {
			t.Errorf("%s printed key material or no placeholder: %s", name, out)
		}
	}
}
