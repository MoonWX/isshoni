package api

import (
	"strings"
	"testing"
)

// userCodeCases is M2 02 §3's table, which web/src/link/userCode.ts's tests share: input → normalized form, "" for
// invalid.
var userCodeCases = []struct{ in, want string }{
	{"WDJB-MJHT", "WDJBMJHT"},
	{"wdjb mjht", "WDJBMJHT"},
	{" wdjbmjht ", "WDJBMJHT"},
	{"WDJB-MJHT-", "WDJBMJHT"},
	{"WDJB-MJHA", ""},  // A is a vowel
	{"WDJB-MJH", ""},   // 7 letters
	{"WDJB-MJHTX", ""}, // 9 letters
	{"WDJB–MJHT", ""},  // en dash
	{"ＷＤＪＢＭＪＨＴ", ""},   // full width
	{"", ""},
}

func TestNormalizeUserCode(t *testing.T) {
	cases := append([]struct{ in, want string }{}, userCodeCases...)
	cases = append(cases, []struct{ in, want string }{
		// More separators, mixed case and every letter of the alphabet.
		{"\tWDJB\tMJHT\t", "WDJBMJHT"},
		{"W-D-J-B M-J-H-T", "WDJBMJHT"},
		{"wDjB-mJhT", "WDJBMJHT"},
		{"--WDJBMJHT--", "WDJBMJHT"},
		{"bcdf-ghjk", "BCDFGHJK"},
		{"LMNP-QRST", "LMNPQRST"},
		{"VWXZ-BCDF", "VWXZBCDF"},
		// Only ASCII space, tab and "-" are dropped; nothing else is folded.
		{"WDJB_MJHT", ""},
		{"WDJB.MJHT", ""},
		{"WDJB\nMJHT", ""},
		{"WDJB\rMJHT", ""},
		{"WDJB MJHT", ""}, // no-break space
		{"WDJB‐MJHT", ""}, // Unicode hyphen
		{"WDJB0MJH", ""},  // digits
		{"WDJBMJHY", ""},  // Y is not in the alphabet
		{"EIOU-WDJB", ""}, // vowels
		{"WDJBMJHT\x00", ""},
		{"WDJBMJH\xff", ""},
		{"   ", ""},
		{"--------", ""},
		{"WDJBMJHTWDJBMJHT", ""},
		{"WDJB-MJHT WDJB-MJHT", ""},
	}...)
	for _, c := range cases {
		got, ok := NormalizeUserCode(c.in)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("NormalizeUserCode(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.want != "")
		}
	}
}

func TestFormatUserCode(t *testing.T) {
	if got := FormatUserCode("WDJBMJHT"); got != "WDJB-MJHT" {
		t.Errorf(`FormatUserCode("WDJBMJHT") = %q, want "WDJB-MJHT"`, got)
	}
	// Normalize and format round-trip for every valid input of the table.
	for _, c := range userCodeCases {
		if c.want == "" {
			continue
		}
		n, _ := NormalizeUserCode(c.in)
		f := FormatUserCode(n)
		if f != "WDJB-MJHT" {
			t.Errorf("FormatUserCode(NormalizeUserCode(%q)) = %q", c.in, f)
		}
		if back, ok := NormalizeUserCode(f); !ok || back != n {
			t.Errorf("NormalizeUserCode(%q) = %q, %v; want %q", f, back, ok, n)
		}
	}
	// Anything that isn't a normalized code comes back unchanged.
	for _, in := range []string{"", "WDJB-MJHT", "wdjbmjht", "WDJBMJH", "WDJBMJHA", "WDJBMJHTX", "WDJB MJH"} {
		if got := FormatUserCode(in); got != in {
			t.Errorf("FormatUserCode(%q) = %q, want it unchanged", in, got)
		}
	}
}

// TestUserCodeAlphabet: 20 distinct upper-case consonants (RFC 8628 §6.1), no vowels and no Y, and 8 symbols.
func TestUserCodeAlphabet(t *testing.T) {
	if len(UserCodeAlphabet) != 20 || UserCodeLength != 8 {
		t.Fatalf("alphabet of %d symbols and codes of %d; want 20 and 8", len(UserCodeAlphabet), UserCodeLength)
	}
	seen := map[rune]bool{}
	for _, r := range UserCodeAlphabet {
		if r < 'A' || r > 'Z' || strings.ContainsRune("AEIOUY", r) || seen[r] {
			t.Errorf("symbol %q", r)
		}
		seen[r] = true
	}
}

// FuzzNormalizeUserCode: never panics; the output is always 8 alphabet letters or invalid; a valid result is the
// input upper-cased without its spaces, tabs and dashes, normalizes to itself and survives FormatUserCode.
func FuzzNormalizeUserCode(f *testing.F) {
	for _, c := range userCodeCases {
		f.Add(c.in)
	}
	f.Add("\tbcdf-ghjk ")
	f.Add("WDJB MJHT")
	f.Fuzz(func(t *testing.T, s string) {
		n, ok := NormalizeUserCode(s)
		if !ok {
			if n != "" {
				t.Fatalf("NormalizeUserCode(%q) = %q with ok false", s, n)
			}
			return
		}
		if len(n) != UserCodeLength || strings.Trim(n, UserCodeAlphabet) != "" {
			t.Fatalf("NormalizeUserCode(%q) = %q: not %d alphabet letters", s, n, UserCodeLength)
		}
		stripped := strings.Map(func(r rune) rune {
			if r == ' ' || r == '\t' || r == '-' {
				return -1
			}
			if r >= 'a' && r <= 'z' {
				return r - ('a' - 'A')
			}
			return r
		}, s)
		if stripped != n {
			t.Fatalf("NormalizeUserCode(%q) = %q, want %q", s, n, stripped)
		}
		if again, ok := NormalizeUserCode(n); !ok || again != n {
			t.Fatalf("NormalizeUserCode(%q) = %q, %v: not idempotent", n, again, ok)
		}
		formatted := FormatUserCode(n)
		if len(formatted) != UserCodeLength+1 || formatted[4] != '-' || formatted[:4]+formatted[5:] != n {
			t.Fatalf("FormatUserCode(%q) = %q", n, formatted)
		}
	})
}
