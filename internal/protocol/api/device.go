package api

import (
	"strings"
	"time"
)

// The device authorization flow and device tokens (M2: docs/m2/02-device-auth.md §5, §6.2, §7, §14): the DTOs of
// POST /api/v1/device/code (#44), /device/token (#45), /device/lookup (#46), /device/decide (#47),
// /device/password (#48) and PATCH /api/v1/me/devices/{id} (#52), and the user-code helpers. POST /device/revoke (#49)
// takes and returns Empty. Device codes, access and refresh tokens, exchange ids and the password of the link form
// are redacted in fmt and log/slog (secret.go). The user code and the device name are not secrets, but they are never
// logged either (README §4 Logging): the DTOs that redact a secret hide them too (DeviceCodeResponse,
// DeviceTokenResponse, DevicePasswordRequest), and callers leave the others out of log lines.

// UserCodeAlphabet is the alphabet of user codes (M2 02 §3, RFC 8628 §6.1): 20 consonants, no vowels (so no words) and
// no digits (so no 0/O or 1/I mix-ups). The server draws 8 symbols by rejection sampling over random bytes.
const UserCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ"

// UserCodeLength is the number of letters of a user code: 8 of 20 symbols, about 34.6 bits.
const UserCodeLength = 8

// DeviceCodeRequest is POST /api/v1/device/code (#44, M2 02 §5.2): an app asks to be linked.
type DeviceCodeRequest struct {
	ClientKind string `json:"clientKind"` // desktop | mobile (01's ClientKind)
	DeviceName string `json:"deviceName"` // the app's proposal, e.g. the PC's host name; the approver may change it
	OS         string `json:"os"`         // windows | macos | linux | ios | android
	AppVersion string `json:"appVersion"` // 1–32 bytes of [0-9A-Za-z.+-]; not necessarily SemVer (dev builds)
}

// DeviceCodeResponse is the reply to POST /api/v1/device/code (#44, M2 02 §5.2). The app shows UserCode and a QR code
// of VerificationURIComplete, and polls /device/token with DeviceCode every Interval seconds.
type DeviceCodeResponse struct {
	DeviceCode              string `json:"deviceCode"`              // 43 base64url characters; redacted in fmt/slog
	UserCode                string `json:"userCode"`                // formatted, "WDJB-MJHT"; redacted in fmt/slog
	VerificationURI         string `json:"verificationUri"`         // "<origin>/link"
	VerificationURIComplete string `json:"verificationUriComplete"` // "<origin>/link?code=WDJB-MJHT"; redacted
	ExpiresIn               int    `json:"expiresIn"`               // seconds (600)
	Interval                int    `json:"interval"`                // seconds between polls (5)
}

// GrantType is DeviceTokenRequest.GrantType.
type GrantType string

const (
	GrantTypeDeviceCode   GrantType = "device_code"   // RFC 8628 polling (M2 02 §5.4)
	GrantTypeRefreshToken GrantType = "refresh_token" // rotation (M2 02 §6.2)
)

// DeviceTokenRequest is POST /api/v1/device/token (#45): a device-code poll (M2 02 §5.4) or a refresh (M2 02 §6.2).
// Only the app's Go code sends it, never its webview.
type DeviceTokenRequest struct {
	GrantType    GrantType `json:"grantType"`
	DeviceCode   string    `json:"deviceCode,omitempty"`   // device_code grant; redacted
	RefreshToken string    `json:"refreshToken,omitempty"` // refresh_token grant, "isr_" + 43; redacted
	ExchangeID   string    `json:"exchangeId,omitempty"`   // refresh_token grant, optional, kept by retries; redacted
	AppVersion   string    `json:"appVersion,omitempty"`   // refresh_token grant, optional: updates the device row
}

// DeviceTokenResponse is the reply of both grants of POST /api/v1/device/token (#45) and of POST
// /api/v1/device/password (#48): a new access and refresh token pair.
type DeviceTokenResponse struct {
	AccessToken      string    `json:"accessToken"`      // "isa_" + 43; redacted
	TokenType        string    `json:"tokenType"`        // "Bearer"
	ExpiresIn        int       `json:"expiresIn"`        // the access token's lifetime in seconds (900)
	RefreshToken     string    `json:"refreshToken"`     // "isr_" + 43; redacted
	RefreshExpiresIn int       `json:"refreshExpiresIn"` // the refresh token's lifetime in seconds (7776000, 90 days)
	Device           DeviceRef `json:"device"`           // its name redacted in fmt/slog
	User             User      `json:"user"`             // {id, username, role}
}

// DeviceRef names a device: DeviceTokenResponse.Device.
type DeviceRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// DeviceLookupRequest is POST /api/v1/device/lookup (#46, M2 02 §5.3), sent by the approval page /link with a cookie
// session.
type DeviceLookupRequest struct {
	UserCode string `json:"userCode"` // as typed or from ?code=; the server normalizes it (NormalizeUserCode)
}

// NetworkMatch is DeviceLookup.Network: whether the app asked for its code from the approving browser's own network
// (M2 02 §5.3). Every value but same requires the approver's "I started this myself" tick.
type NetworkMatch string

const (
	NetworkMatchSame      NetworkMatch = "same"
	NetworkMatchDifferent NetworkMatch = "different"
	NetworkMatchUnknown   NetworkMatch = "unknown" // e.g. the app and the browser used different address families
)

// DeviceLookup is the reply to POST /api/v1/device/lookup (#46, M2 02 §5.3): what the approval page shows about the
// request before the approver decides.
type DeviceLookup struct {
	UserCode    string       `json:"userCode"` // formatted, "WDJB-MJHT"
	ClientKind  string       `json:"clientKind"`
	OS          string       `json:"os"`
	DeviceName  string       `json:"deviceName"`
	AppVersion  string       `json:"appVersion"`
	RequestIP   string       `json:"requestIp"` // the address the app asked for the code from
	Network     NetworkMatch `json:"network"`
	RequestedAt time.Time    `json:"requestedAt"`
	ExpiresAt   time.Time    `json:"expiresAt"`
}

// DeviceDecideRequest is POST /api/v1/device/decide (#47, M2 02 §5.3); the reply is 204.
type DeviceDecideRequest struct {
	UserCode   string  `json:"userCode"`
	Approve    bool    `json:"approve"`
	DeviceName *string `json:"deviceName,omitempty"` // absent = keep the app's name
}

// DevicePasswordRequest is POST /api/v1/device/password (#48, M2 02 §14; M1 03 §12.4.7's sketch): the in-app
// username/password fallback, which links a device without a browser. Sent by the app's Go code, never by its webview.
// The reply is DeviceTokenResponse.
type DevicePasswordRequest struct {
	Username   string `json:"username"`
	Password   string `json:"password"`   // redacted in fmt/slog
	ClientKind string `json:"clientKind"` // as DeviceCodeRequest
	DeviceName string `json:"deviceName"` // redacted in fmt/slog
	OS         string `json:"os"`
	AppVersion string `json:"appVersion"`
}

// RenameDeviceRequest is PATCH /api/v1/me/devices/{id} (#52, M2 02 §7.2); the reply is DeviceResponse.
type RenameDeviceRequest struct {
	Name string `json:"name"`
}

// DeviceResponse is {device}: the reply to PATCH /api/v1/me/devices/{id} (#52).
type DeviceResponse struct {
	Device DeviceInfo `json:"device"`
}

// NormalizeUserCode returns the canonical form of a user code as a person typed it (M2 02 §3): ASCII spaces, tabs and
// "-" are dropped and ASCII a–z are upper-cased; the result must be exactly UserCodeLength letters of
// UserCodeAlphabet, else ok is false. Nothing else is folded: an en dash, full-width letters or a vowel make the code
// invalid. "wdjb mjht" → "WDJBMJHT". The TypeScript copy is web/src/link/userCode.ts; both test suites use M2 02 §3's
// table.
func NormalizeUserCode(s string) (normalized string, ok bool) {
	var b [UserCodeLength]byte
	n := 0
	for i := range len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '-':
			continue
		case c >= 'a' && c <= 'z':
			c -= 'a' - 'A'
		}
		if n == UserCodeLength || strings.IndexByte(UserCodeAlphabet, c) < 0 {
			return "", false
		}
		b[n] = c
		n++
	}
	if n != UserCodeLength {
		return "", false
	}
	return string(b[:]), true
}

// FormatUserCode returns a normalized user code as it is shown, with a "-" after the fourth letter: "WDJBMJHT" →
// "WDJB-MJHT". Any other input, including a code that isn't normalized yet, is returned unchanged.
func FormatUserCode(normalized string) string {
	if n, ok := NormalizeUserCode(normalized); !ok || n != normalized {
		return normalized
	}
	return normalized[:UserCodeLength/2] + "-" + normalized[UserCodeLength/2:]
}
