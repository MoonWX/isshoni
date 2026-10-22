package auth

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/secure/precis"
)

// Device names (M2 02 §5.6).

// fieldDeviceName is the JSON name of a device's name in POST /device/code, /device/decide and /device/password, the
// default Field of NormalizeDeviceName's errors. PATCH /me/devices/{id} calls the field "name" and keeps only the
// code.
const fieldDeviceName = "deviceName"

// deviceNameMaxRunes is the longest device name, in code points after normalization (M2 02 §5.6, §15).
const deviceNameMaxRunes = 64

// NormalizeDeviceName returns a device name as it is stored and shown (M2 02 §5.6): the name an app proposes (the
// PC's host name), the one an approver types at /device/decide, and (WA39) a rename on the Devices page. The rules,
// in order:
//
//  1. valid UTF-8 without U+FFFD, else invalid: encoding/json turns bad bytes of a request body into U+FFFD, which
//     no name needs (room names and the serverName setting have the same rule);
//  2. spaces trimmed; nothing left (also only spaces, of any kind) is required;
//  3. PRECIS Nickname (RFC 8266), the profile of room names (03 §8): non-ASCII spaces become U+0020, inner runs
//     collapse to one, NFKC. Runes the profile refuses (control characters, a line break among them) are invalid,
//     and so is a result that would not map to itself again (the stored name must normalize unchanged);
//  4. at most 64 code points, else too_long.
//
// Names need not be unique. A name is user content and is never logged (README §4 Logging).
//
// The error is a *FieldError with Field "deviceName" and the code FieldRequired, FieldTooLong or FieldInvalid.
func NormalizeDeviceName(in string) (string, error) {
	if !utf8.ValidString(in) || strings.ContainsRune(in, utf8.RuneError) {
		return "", fieldErr(fieldDeviceName, FieldInvalid)
	}
	trimmed := strings.TrimSpace(in)
	if trimmed == "" {
		return "", fieldErr(fieldDeviceName, FieldRequired)
	}
	name, err := precis.Nickname.String(trimmed)
	if err != nil {
		return "", fieldErr(fieldDeviceName, FieldInvalid)
	}
	if name == "" {
		// Only spaces the profile maps to U+0020 (an ideographic space, a no-break space): nothing is left.
		return "", fieldErr(fieldDeviceName, FieldRequired)
	}
	if again, err := precis.Nickname.String(name); err != nil || again != name {
		return "", fieldErr(fieldDeviceName, FieldInvalid)
	}
	if utf8.RuneCountInString(name) > deviceNameMaxRunes {
		return "", fieldErr(fieldDeviceName, FieldTooLong)
	}
	return name, nil
}
