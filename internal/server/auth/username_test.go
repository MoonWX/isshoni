package auth

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/text/secure/precis"
)

func TestNormalizeUsername(t *testing.T) {
	tests := []struct {
		name, in     string
		display, key string // when code == ""
		code         string
	}{
		// 03 §7.1 examples.
		{name: "fullwidth", in: "Ａｌｅｘ", display: "Alex", key: "alex"},
		{name: "kanji", in: "太郎", display: "太郎", key: "太郎"},
		{name: "cyrillic", in: "Ёжик", display: "Ёжик", key: "ёжик"},
		{name: "punctuation inside", in: "sam_k.99", display: "sam_k.99", key: "sam_k.99"},
		{name: "one rune", in: "a", code: FieldTooShort},
		{name: "leading hyphen", in: "-sam", code: FieldInvalid},
		{name: "double dot", in: "sam..k", code: FieldInvalid},

		// Case and width.
		{name: "case preserved, key lowered", in: "SamK", display: "SamK", key: "samk"},
		{name: "surrounding spaces trimmed", in: "  Alex \t", display: "Alex", key: "alex"},
		{name: "fullwidth digits", in: "ｋ９", display: "k9", key: "k9"},
		{name: "decomposed accent composed", in: "Rene\u0301", display: "Ren\u00e9", key: "ren\u00e9"},
		{name: "german sharp s kept", in: "Straße", display: "Straße", key: "straße"},

		// Scripts.
		{name: "devanagari ending in a vowel sign", in: "राजे", display: "राजे", key: "राजे"},
		{name: "devanagari with virama", in: "नमस्ते", display: "नमस्ते", key: "नमस्ते"},
		{name: "thai", in: "สมชาย", display: "สมชาย", key: "สมชาย"},
		{name: "hebrew", in: "שלום", display: "שלום", key: "שלום"},
		{name: "arabic", in: "مريم", display: "مريم", key: "مريم"},
		{name: "arabic-indic digits", in: "علي٤٢", display: "علي٤٢", key: "علي٤٢"},
		{name: "bidi rule: a digit before hebrew", in: "1שלום", code: FieldInvalid},

		// Refused runes.
		{name: "emoji", in: "sam😀", code: FieldInvalid},
		{name: "only emoji", in: "😀😀", code: FieldInvalid},
		{name: "inner space", in: "sam k", code: FieldInvalid},
		{name: "symbol", in: "sam!k", code: FieldInvalid},
		{name: "at sign", in: "sam@k", code: FieldInvalid},
		{name: "plus", in: "sam+k", code: FieldInvalid},
		{name: "control", in: "sam\x01k", code: FieldInvalid},
		{name: "zero-width joiner", in: "sa\u200dm", code: FieldInvalid},
		{name: "letter-like number", in: "Ⅻx", code: FieldInvalid},
		{name: "ligature", in: "ﬁne", code: FieldInvalid},
		{name: "leading combining mark", in: "\u0301sam", code: FieldInvalid},
		{name: "mark after punctuation", in: "sa-\u0301m", code: FieldInvalid},

		// Edge punctuation.
		{name: "trailing dot", in: "sam.", code: FieldInvalid},
		{name: "trailing underscore", in: "sam_", code: FieldInvalid},
		{name: "leading underscore", in: "_sam", code: FieldInvalid},
		{name: "leading dot", in: ".sam", code: FieldInvalid},
		{name: "hyphen then dot", in: "sa-.m", code: FieldInvalid},
		{name: "underscore then hyphen", in: "sa_-m", code: FieldInvalid},
		{name: "separated punctuation", in: "a.b-c_d", display: "a.b-c_d", key: "a.b-c_d"},
		{name: "only punctuation", in: "--", code: FieldInvalid},
		{name: "digits only", in: "42", display: "42", key: "42"},

		// Length counts runes.
		{name: "two kanji", in: "山田", display: "山田", key: "山田"},
		{name: "32 runes", in: strings.Repeat("\u00e9", 32), display: strings.Repeat("\u00e9", 32), key: strings.Repeat("\u00e9", 32)},
		{name: "33 runes", in: strings.Repeat("\u00e9", 33), code: FieldTooLong},
		{name: "32 kanji (96 bytes)", in: strings.Repeat("山", 32), display: strings.Repeat("山", 32), key: strings.Repeat("山", 32)},
		{name: "over 128 bytes", in: strings.Repeat("a", 129), code: FieldTooLong},
		{name: "128 bytes of spaces around a name", in: strings.Repeat(" ", 62) + "Alex" + strings.Repeat(" ", 62), display: "Alex", key: "alex"},
		{name: "129 bytes with spaces", in: strings.Repeat(" ", 63) + "Alex" + strings.Repeat(" ", 62), code: FieldTooLong},
		{name: "empty", in: "", code: FieldRequired},
		{name: "only spaces", in: "   ", code: FieldRequired},

		// Reserved keys, whatever the case or width.
		{name: "reserved isshoni", in: "isshoni", code: FieldReserved},
		{name: "reserved System", in: "System", code: FieldReserved},
		{name: "reserved EVERYONE", in: "EVERYONE", code: FieldReserved},
		{name: "reserved fullwidth here", in: "ｈｅｒｅ", code: FieldReserved},
		{name: "admin allowed", in: "admin", display: "admin", key: "admin"},
		{name: "Admin allowed", in: "Admin", display: "Admin", key: "admin"},
		{name: "reserved as a prefix is fine", in: "systemd", display: "systemd", key: "systemd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			display, key, err := NormalizeUsername(tt.in)
			if tt.code != "" {
				var fe *FieldError
				if !errors.As(err, &fe) || fe.Code != tt.code || fe.Field != "username" {
					t.Fatalf("NormalizeUsername(%q) = %q, %q, %v; want FieldError username/%s", tt.in, display, key, err, tt.code)
				}
				if display != "" || key != "" {
					t.Fatalf("NormalizeUsername(%q) returned %q, %q with an error", tt.in, display, key)
				}
				return
			}
			if err != nil || display != tt.display || key != tt.key {
				t.Fatalf("NormalizeUsername(%q) = %q, %q, %v; want %q, %q", tt.in, display, key, err, tt.display, tt.key)
			}
		})
	}
}

func TestNormalizeUsernameSameKey(t *testing.T) {
	// Spellings of one name share a key, so they can't both register.
	_, want, err := NormalizeUsername("alex")
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{"Alex", "ALEX", "Ａｌｅｘ", "aLeX", " alex "} {
		if _, key, err := NormalizeUsername(in); err != nil || key != want {
			t.Errorf("NormalizeUsername(%q) key = %q, %v; want %q", in, key, err, want)
		}
	}
}

func FuzzNormalizeUsername(f *testing.F) {
	for _, s := range []string{
		"Ａｌｅｘ", "太郎", "Ёжик", "sam_k.99", "a", "-sam", "sam..k", "राजे", "שלום1", "1שלום", "İstanbul",
		"Straße", "sam😀", "Rene\u0301", "ǅemal", "ΣΊΣΥΦΟΣ", "\u0301a", "a\u200db",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		display, key, err := NormalizeUsername(in)
		if err != nil {
			var fe *FieldError
			if !errors.As(err, &fe) || fe.Field != "username" {
				t.Fatalf("NormalizeUsername(%q) error %v is not a username *FieldError", in, err)
			}
			return
		}
		if n := utf8.RuneCountInString(display); n < UsernameMinRunes || n > UsernameMaxRunes {
			t.Fatalf("NormalizeUsername(%q) display %q has %d runes", in, display, n)
		}
		if !utf8.ValidString(display) || !utf8.ValidString(key) || key == "" {
			t.Fatalf("NormalizeUsername(%q) = %q, %q: invalid output", in, display, key)
		}
		// Idempotent: the stored display name normalizes to itself and to the same key.
		d2, k2, err := NormalizeUsername(display)
		if err != nil || d2 != display || k2 != key {
			t.Fatalf("NormalizeUsername(%q) = %q, %q, but NormalizeUsername(%q) = %q, %q, %v",
				in, display, key, display, d2, k2, err)
		}
		// The key is stable: it is its own comparison key, so lookups by key never drift.
		if k3, err := precis.UsernameCaseMapped.CompareKey(key); err != nil || k3 != key {
			t.Fatalf("CompareKey(key %q) = %q, %v", key, k3, err)
		}
		if reservedUsernames[key] {
			t.Fatalf("NormalizeUsername(%q) accepted the reserved key %q", in, key)
		}
	})
}
