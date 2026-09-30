package auth

import (
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed" // the common-password list
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/text/secure/precis"
)

// Password limits (03 §7.2; the minimum of 8 is the owner's decision, 03 §19). /api/v1/info serves the rune limits
// as accountRules.
const (
	PasswordMinRunes = 8
	PasswordMaxRunes = 128
	passwordMaxBytes = 1024 // raw input, checked before any other work
)

// commonPasswordsGz is the common-password blocklist: one lowercased password per line, sorted, unique, each at
// least PasswordMinRunes long, gzipped.
//
// Source: SecLists (https://github.com/danielmiessler/SecLists), the file
// Passwords/Common-Credentials/10k-most-common.txt at commit 913b327317496d062bcc7cace524aaad8a693be2 (10,001
// entries; SHA-256 68782d6a4a19a4768d5f15dd66bd534e7a33055cc755411e33f16d18c50fdcce). The 2,087 entries of 8 or
// more characters were kept, lowercased, deduplicated and sorted; TestCommonPasswordsFile rebuilds this file from
// that source with -seclists=<path>.
//
// License: MIT, "Copyright (c) 2018 Daniel Miessler"; data/common-passwords.txt.gz.license holds the full text. The
// attribution belongs in deploy/notices/extra.txt, so that it reaches THIRD_PARTY_NOTICES (03 §17, 06 §8.4).
//
//go:embed data/common-passwords.txt.gz
var commonPasswordsGz []byte

// commonPasswords decompresses the embedded list once, on first use.
var commonPasswords = sync.OnceValue(func() map[string]struct{} {
	set, err := parseCommonPasswords(commonPasswordsGz)
	if err != nil {
		// The file is embedded at build time and checked by tests; a bad file is a build defect.
		panic(err)
	}
	return set
})

// parseCommonPasswords reads the gzipped list format of commonPasswordsGz.
func parseCommonPasswords(gz []byte) (map[string]struct{}, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, fmt.Errorf("auth: common-password list: %w", err)
	}
	defer func() { _ = zr.Close() }()
	set := make(map[string]struct{}, 2100)
	sc := bufio.NewScanner(zr)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			set[line] = struct{}{}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("auth: common-password list: %w", err)
	}
	return set, nil
}

// isCommonPassword reports whether the lowercased password is on the embedded list.
func isCommonPassword(pw string) bool {
	_, ok := commonPasswords()[strings.ToLower(pw)]
	return ok
}

// CheckPassword applies the password rules of 03 §7.2 to a new password and returns the normalized form, which is
// what gets hashed. usernameKey is the account's comparison key from NormalizeUsername ("" skips rule 5). A failed
// rule returns a *FieldError for the field "password" with the code of the first rule that failed:
//
//  1. at most 1024 bytes of input, else too_long, before any other work;
//  2. PRECIS OpaqueString: NFC, and non-ASCII spaces become U+0020; controls are refused, else invalid. Empty input
//     is required;
//  3. 8–128 runes, else too_short or too_long;
//  4. the lowercased password is not on the embedded common list, else too_common;
//  5. it is not the username (compared as username keys, so case and width do not matter), else same_as_username.
//
// There are no composition rules. Logins verify against the stored hash and do not apply these rules; they normalize
// the typed password with normalizePassword first.
func CheckPassword(pw, usernameKey string) (normalized string, err error) {
	const field = "password"
	if len(pw) > passwordMaxBytes {
		return "", fieldErr(field, FieldTooLong)
	}
	if pw == "" {
		return "", fieldErr(field, FieldRequired)
	}
	normalized, err = normalizePassword(pw)
	if err != nil {
		return "", fieldErr(field, FieldInvalid)
	}
	switch n := utf8.RuneCountInString(normalized); {
	case n < PasswordMinRunes:
		return "", fieldErr(field, FieldTooShort)
	case n > PasswordMaxRunes:
		return "", fieldErr(field, FieldTooLong)
	}
	if isCommonPassword(normalized) {
		return "", fieldErr(field, FieldTooCommon)
	}
	if usernameKey != "" {
		if k, err := precis.UsernameCaseMapped.CompareKey(normalized); err == nil && k == usernameKey {
			return "", fieldErr(field, FieldSameAsUsername)
		}
	}
	return normalized, nil
}

// normalizePassword applies PRECIS OpaqueString (03 §7.2 rule 2) to a password, without the length and list rules.
// Login normalizes the typed password this way before verifying it, so a password set on one device matches when
// typed on another that sends a different space or a decomposed accent.
func normalizePassword(pw string) (string, error) {
	if len(pw) > passwordMaxBytes {
		return "", fieldErr("password", FieldTooLong)
	}
	out, err := precis.OpaqueString.String(pw)
	if err != nil {
		return "", fmt.Errorf("auth: password: %w", err)
	}
	return out, nil
}
