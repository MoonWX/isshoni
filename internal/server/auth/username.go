package auth

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/secure/precis"
)

// Username limits (03 §7.1). /api/v1/info serves the rune limits as accountRules.
const (
	UsernameMinRunes = 2
	UsernameMaxRunes = 32
	usernameMaxBytes = 128 // raw input, checked before any other work
)

// reservedUsernames are comparison keys nobody may register. "admin" is allowed on purpose: the first admin often
// picks it.
var reservedUsernames = map[string]bool{
	"isshoni":  true,
	"system":   true,
	"everyone": true,
	"here":     true,
}

// NormalizeUsername applies the username rules of 03 §7.1 and returns the display form (shown as typed, case
// preserved) and the comparison key (used for uniqueness, login and the reserved check). A failed rule returns a
// *FieldError for the field "username" with the code of the first rule that failed:
//
//  1. at most 128 bytes of input, else too_long;
//  2. display = PRECIS UsernameCasePreserved(TrimSpace(input)): fullwidth to halfwidth, NFC; spaces, controls,
//     symbols and emoji are refused, else invalid. Empty input (after trimming) is required;
//  3. 2–32 runes, else too_short or too_long;
//  4. only letters (L*), marks (M*), decimal digits (Nd), '_', '-' and '.'; it starts with a letter or digit and ends
//     with a letter or digit, where combining marks count as part of the letter or digit they follow (so names in
//     scripts that end in a vowel sign, like "राजे", are valid); a mark never follows punctuation; two of '_', '-',
//     '.' never touch; else invalid;
//  5. key = PRECIS UsernameCaseMapped.CompareKey(display);
//  6. the keys "isshoni", "system", "everyone" and "here" are reserved.
//
// Both outputs are stable: NormalizeUsername(display) returns the same display and key.
func NormalizeUsername(in string) (display, key string, err error) {
	const field = "username"
	if len(in) > usernameMaxBytes {
		return "", "", fieldErr(field, FieldTooLong)
	}
	trimmed := strings.TrimSpace(in)
	if trimmed == "" {
		return "", "", fieldErr(field, FieldRequired)
	}
	display, err = precis.UsernameCasePreserved.String(trimmed)
	if err != nil || display == "" {
		return "", "", fieldErr(field, FieldInvalid)
	}
	// The profile must be a fixed point, so that a stored display name normalizes to itself.
	if again, err := precis.UsernameCasePreserved.String(display); err != nil || again != display {
		return "", "", fieldErr(field, FieldInvalid)
	}
	switch n := utf8.RuneCountInString(display); {
	case n < UsernameMinRunes:
		return "", "", fieldErr(field, FieldTooShort)
	case n > UsernameMaxRunes:
		return "", "", fieldErr(field, FieldTooLong)
	}
	if !usernameShapeOK(display) {
		return "", "", fieldErr(field, FieldInvalid)
	}
	key, err = precis.UsernameCaseMapped.CompareKey(display)
	if err != nil || key == "" {
		return "", "", fieldErr(field, FieldInvalid)
	}
	if reservedUsernames[key] {
		return "", "", fieldErr(field, FieldReserved)
	}
	return display, key, nil
}

// isUsernamePunct reports the three punctuation runes a username may contain.
func isUsernamePunct(r rune) bool { return r == '_' || r == '-' || r == '.' }

// usernameShapeOK checks rule 4 of NormalizeUsername on the display form.
func usernameShapeOK(s string) bool {
	// lastBase is the class of the last rune that was not a mark: 'a' for a letter or digit, 'p' for punctuation,
	// 0 before the first rune.
	var lastBase byte
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.Is(unicode.Nd, r):
			lastBase = 'a'
		case unicode.IsMark(r):
			// A mark belongs to the letter or digit before it. At the start, or after punctuation, it has none.
			if lastBase != 'a' {
				return false
			}
		case isUsernamePunct(r):
			// Not first, and never next to another punctuation rune.
			if lastBase != 'a' {
				return false
			}
			lastBase = 'p'
		default:
			return false
		}
	}
	return lastBase == 'a'
}
