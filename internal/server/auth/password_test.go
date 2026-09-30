package auth

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

var seclistsPath = flag.String("seclists", "",
	"rebuild data/common-passwords.txt.gz from this copy of SecLists' Passwords/Common-Credentials/10k-most-common.txt")

// buildCommonPasswords turns the SecLists source into the embedded list format: entries of at least
// PasswordMinRunes runes, lowercased, deduplicated, sorted, one per line, gzipped with a zero header time.
func buildCommonPasswords(t *testing.T, src []byte) []byte {
	t.Helper()
	set := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(src))
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if utf8.RuneCountInString(line) >= PasswordMinRunes {
			set[strings.ToLower(line)] = true
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	list := make([]string, 0, len(set))
	for k := range set {
		list = append(list, k)
	}
	slices.Sort(list)
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range list {
		_, _ = zw.Write([]byte(p + "\n"))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestCommonPasswordsFile checks the embedded list's format. With -seclists=<path> it first rebuilds the
// file from the SecLists source (see commonPasswordsGz for the pinned commit); run the test again afterwards so the
// new file is embedded.
func TestCommonPasswordsFile(t *testing.T) {
	gz := commonPasswordsGz
	if *seclistsPath != "" {
		src, err := os.ReadFile(*seclistsPath)
		if err != nil {
			t.Fatal(err)
		}
		gz = buildCommonPasswords(t, src)
		if err := os.WriteFile(filepath.Join("data", "common-passwords.txt.gz"), gz, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	sc := bufio.NewScanner(zr)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(lines) < 2000 {
		t.Fatalf("the list has %d entries, want the ~2,087 of the pinned SecLists commit", len(lines))
	}
	for i, l := range lines {
		if n := utf8.RuneCountInString(l); n < PasswordMinRunes {
			t.Errorf("line %d %q: %d runes, shorter than the minimum length", i+1, l, n)
		}
		if l != strings.ToLower(l) || strings.TrimSpace(l) != l {
			t.Errorf("line %d %q: not lowercased and trimmed", i+1, l)
		}
		if i > 0 && lines[i-1] >= l {
			t.Errorf("line %d %q: not sorted or not unique", i+1, l)
		}
	}
	if len(gz) > 32<<10 {
		t.Errorf("the gzipped list is %d bytes, over the ~25 KB budget of 03 §2.2", len(gz))
	}
	if *seclistsPath == "" { // after a rebuild, the old file is still the embedded one
		if got := len(commonPasswords()); got != len(lines) {
			t.Errorf("commonPasswords() has %d entries, the file %d", got, len(lines))
		}
	}
}

func TestParseCommonPasswordsRejectsGarbage(t *testing.T) {
	if _, err := parseCommonPasswords([]byte("not gzip")); err == nil {
		t.Fatal("parseCommonPasswords(garbage) = nil error")
	}
}

func TestCheckPassword(t *testing.T) {
	long := strings.Repeat("\u00e9", PasswordMaxRunes) // 128 runes, 256 bytes
	tests := []struct {
		name, in, userKey string
		want              string // normalized, when code == ""
		code              string
	}{
		{name: "ok", in: "correct horse battery", want: "correct horse battery"},
		{name: "exactly 8 runes", in: "tr0ub4dr", want: "tr0ub4dr"},
		{name: "7 runes", in: "tr0ub4d", code: FieldTooShort},
		{name: "length counts runes, not bytes", in: "ñandú12", code: FieldTooShort}, // 7 runes, 9 bytes
		{name: "8 multibyte runes", in: "日本語のパスワー", want: "日本語のパスワー"},
		{name: "128 runes", in: long, want: long},
		{name: "129 runes", in: long + "x", code: FieldTooLong},
		{name: "over 1024 bytes", in: strings.Repeat("a", passwordMaxBytes+1), code: FieldTooLong},
		{name: "empty", in: "", code: FieldRequired},
		{name: "no-break space becomes a space", in: "my\u00a0secret\u00a0words", want: "my secret words"},
		{name: "em space becomes a space", in: "my\u2003secret\u2003words", want: "my secret words"},
		{name: "decomposed accent is composed", in: "cafe\u0301 au lait", want: "caf\u00e9 au lait"},
		{name: "leading space kept", in: "  spaced out", want: "  spaced out"},
		{name: "control rejected", in: "pass\x00word1", code: FieldInvalid},
		{name: "tab rejected", in: "pass\tword1", code: FieldInvalid},
		{name: "common", in: "password1", code: FieldTooCommon},
		{name: "common in upper case", in: "PASSWORD1", code: FieldTooCommon},
		{name: "common mixed case", in: "QwErTyUiOp", code: FieldTooCommon},
		{name: "common digits", in: "12345678", code: FieldTooCommon},
		{name: "same as username", in: "tarou.yamada", userKey: "tarou.yamada", code: FieldSameAsUsername},
		{name: "same as username, other case", in: "Tarou.Yamada", userKey: "tarou.yamada", code: FieldSameAsUsername},
		{name: "same as username, fullwidth", in: "ｔａｒｏｕ．ｙａｍａｄａ", userKey: "tarou.yamada", code: FieldSameAsUsername},
		{name: "contains the username", in: "tarou.yamada!", userKey: "tarou.yamada", want: "tarou.yamada!"},
		{name: "no username key", in: "tarou.yamada", want: "tarou.yamada"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CheckPassword(tt.in, tt.userKey)
			if tt.code != "" {
				var fe *FieldError
				if !errors.As(err, &fe) || fe.Code != tt.code || fe.Field != "password" {
					t.Fatalf("CheckPassword(%q) = %q, %v; want FieldError password/%s", tt.in, got, err, tt.code)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("CheckPassword(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
			}
		})
	}
}

func TestCommonPasswordLookup(t *testing.T) {
	for _, pw := range []string{"password", "iloveyou1", "Password", "ILOVEYOU"} {
		if !isCommonPassword(pw) {
			t.Errorf("isCommonPassword(%q) = false", pw)
		}
	}
	for _, pw := range []string{"Watch together at eight", "zq8#Lm2!pX"} {
		if isCommonPassword(pw) {
			t.Errorf("isCommonPassword(%q) = true", pw)
		}
	}
}

func FuzzCheckPassword(f *testing.F) {
	for _, s := range []string{"correct horse", "pass\u00a0word", "cafe\u0301 latte", "PASSWORD1", "\x00", "ａｂｃｄｅｆｇｈ", ""} {
		f.Add(s, "alex")
	}
	f.Fuzz(func(t *testing.T, pw, userKey string) {
		got, err := CheckPassword(pw, userKey)
		if err != nil {
			var fe *FieldError
			if !errors.As(err, &fe) {
				t.Fatalf("CheckPassword(%q) error %v is not a *FieldError", pw, err)
			}
			return
		}
		if n := utf8.RuneCountInString(got); n < PasswordMinRunes || n > PasswordMaxRunes {
			t.Fatalf("CheckPassword(%q) = %q with %d runes", pw, got, n)
		}
		// Idempotent: the stored form passes the rules unchanged.
		again, err := CheckPassword(got, userKey)
		if err != nil || again != got {
			t.Fatalf("CheckPassword(%q) = %q, but CheckPassword of that = %q, %v", pw, got, again, err)
		}
	})
}
