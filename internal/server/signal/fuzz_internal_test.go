package signal

import (
	"bytes"
	"strings"
	"testing"
)

// FuzzNormalizeOrigin: never panics, and a normalized origin is a fixed point (so the allowlist compare is exact).
func FuzzNormalizeOrigin(f *testing.F) {
	for _, s := range []string{
		"https://watch.example.com", "HTTPS://Watch.Example.com:443", "http://[2001:db8::1]:80",
		"wails://wails.localhost", "https://192.0.2.1:8443", "null", "", "https://a/b", "https://%41",
		"https://[::ffff:192.0.2.1]", "https://x:0", "a://b@c", "https://xn--bcher-kva.example",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, httpOnly := range []bool{false, true} {
			n, err := normalizeOrigin(s, httpOnly)
			if err != nil {
				continue
			}
			n2, err := normalizeOrigin(n, httpOnly)
			if err != nil || n2 != n {
				t.Fatalf("normalizeOrigin(%q, %v) = %q, which normalizes to %q, %v", s, httpOnly, n, n2, err)
			}
			if httpOnly && !strings.HasPrefix(n, "http://") && !strings.HasPrefix(n, "https://") {
				t.Fatalf("normalizeOrigin(%q, true) = %q", s, n)
			}
		}
	})
}

// FuzzParseResumeToken: never panics; only tokens minted with the key parse, and they name the right connection.
func FuzzParseResumeToken(f *testing.F) {
	key := bytes.Repeat([]byte{3}, resumeKeyLen)
	raw := [connIDRawLen]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	tok, _ := mintResumeToken(key, raw)
	for _, s := range []string{tok.Reveal(), "", "r1.", "r1." + strings.Repeat("A", 56), "r2." + tok.Reveal()[3:]} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, ok := parseResumeToken(key, s)
		if !ok {
			return
		}
		if len(s) != len(resumeTokenPrefix)+56 || !strings.HasPrefix(s, resumeTokenPrefix) {
			t.Fatalf("parsed a token of the wrong shape: %q", s)
		}
		if got != raw && s != tok.Reveal() {
			// Any other token that parses would be an HMAC forgery.
			t.Fatalf("forged token %q parsed as %v", s, got)
		}
	})
}
