// Package credstore keeps a native app's refresh token per server (docs/m2/02-device-auth.md §11.5): one Credential
// per server origin, behind a small Store interface.
//
// Implementations:
//   - Windows (Open): Windows Credential Manager, one generic credential per server with the target name
//     Options.Prefix + origin ("isshoni:v1:https://watch.example.com"), persisted CRED_PERSIST_LOCAL_MACHINE so it
//     never roams to another PC. The credential blob is UTF-8 JSON, {"v":1,"refreshToken":…,"deviceId":…,
//     "savedAt":…}, and the user name goes into Credential Manager's own "User name" field.
//   - Other OSes in M2 (Open): a plaintext file, credentials.json in Options.Dir (mode 0600, written atomically), for
//     development and CI builds of the desktop app on macOS and Linux. Open logs one WARN saying so. M3 replaces it
//     with the macOS Keychain on darwin; M4's Linux agent keeps it as the headless fallback.
//   - NewMemory: a map, for tests and for the e2e harness.
//
// Every Store applies the same rules, so a test against the memory store holds for Credential Manager too: Put
// refuses a credential whose Credential Manager blob would exceed 2,560 bytes (CRED_MAX_CREDENTIAL_BLOB_SIZE), an
// empty or NUL-carrying origin, an empty refresh token and an over-long user name; SavedAt is kept in UTC to the
// millisecond; List returns origins sorted. All implementations are safe for concurrent use. Store methods take no
// context: each is a local call (Credential Manager, or one small file) that never waits on the network.
//
// A stored credential that can't be read back (a blob that doesn't parse, another "v", no refresh token) is
// reported as ErrNotFound with a WARN, so the app links again; List still names its origin, so Delete (and the
// uninstaller's ForgetAll) can remove it. ErrUnavailable means Windows has no user profile loaded for the process
// (ERROR_NO_SUCH_LOGON_SESSION).
//
// Threat model (docs/security.md): any process running as the same Windows user can read the credential (DPAPI
// binds it to the user's logon), as it can read browser cookies.
//
// No error or log line of this package holds a refresh token, a user name or a credential blob: Win32 errors are
// wrapped with their number only, and Credential redacts its secret fields in fmt, slog and encoding/json output.
package credstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
	"unicode/utf16"
)

// Credential is what a Store keeps for one server.
//
// Its RefreshToken and UserName never show in fmt output (every verb), slog or encoding/json: they print
// "[redacted]" (or nothing when empty). Go's own limit applies: fmt can't call methods on an unexported struct field,
// so a Credential in an unexported field prints raw; keep it out of printed structs.
type Credential struct {
	RefreshToken string    // redacted in fmt/slog
	DeviceID     string    // the server's device id (api.DeviceRef.ID)
	UserName     string    // display only (Credential Manager's "User name" column); redacted in fmt/slog
	SavedAt      time.Time // when the token was saved; stored in UTC to the millisecond
}

// Store keeps one Credential per server origin.
type Store interface {
	// Get returns the origin's credential, or ErrNotFound when there is none or it can't be read back.
	Get(origin string) (Credential, error)
	// Put stores the origin's credential, replacing the one it had.
	Put(origin string, c Credential) error
	// Delete removes the origin's credential; nil when it doesn't exist.
	Delete(origin string) error
	// List returns the origins that have a credential, sorted.
	List() ([]string, error)
}

var (
	// ErrNotFound is Get's answer for an origin without a (readable) credential.
	ErrNotFound = errors.New("credstore: not found")
	// ErrUnavailable means the store can't be used by this process: on Windows, no user profile is loaded
	// (ERROR_NO_SUCH_LOGON_SESSION).
	ErrUnavailable = errors.New("credstore: unavailable")
)

// Options configures Open.
type Options struct {
	// Prefix is put before the origin to make the credential's key (Credential Manager's target name). "" means
	// "isshoni:v1:"; tests use "isshoni-test:<random>:", so they never touch a user's credentials. It must not
	// contain "*" or NUL.
	Prefix string
	// Dir is the file store's directory (non-Windows, M2). "" means os.UserConfigDir()/isshoni-dev. Windows ignores
	// it.
	Dir string
	// Logger gets the store's WARNs (an unreadable credential, the plaintext file store). nil means slog.Default().
	Logger *slog.Logger
}

// defaultPrefix is the target-name prefix of the app's credentials (README §4 "Naming": isshoni:v1:<origin>).
const defaultPrefix = "isshoni:v1:"

// Credential Manager's limits (wincred.h), which every Store applies. Where the documentation leaves open whether a
// length counts the terminating NUL, one UTF-16 unit is kept free for it.
const (
	maxBlobBytes    = 2560  // CRED_MAX_CREDENTIAL_BLOB_SIZE (5*512)
	maxUserNameLen  = 512   // CRED_MAX_USERNAME_LENGTH is 513
	maxTargetLength = 32766 // CRED_MAX_GENERIC_TARGET_NAME_LENGTH is 32767
)

// blobVersion is the "v" of the credential blob.
const blobVersion = 1

// savedAtLayout writes SavedAt in UTC with milliseconds: "2026-11-02T19:05:01.004Z".
const savedAtLayout = "2006-01-02T15:04:05.000Z07:00"

var (
	// errInvalid is wrapped by the errors of a Put, Get or Delete called with arguments no store can hold.
	errInvalid = errors.New("credstore: invalid argument")
	// errBlobTooLarge is wrapped by Put's error for a credential whose blob exceeds maxBlobBytes.
	errBlobTooLarge = errors.New("credstore: credential too large")
)

// resolve applies Options' defaults and checks the prefix.
func (o Options) resolve() (prefix string, log *slog.Logger, err error) {
	prefix = o.Prefix
	if prefix == "" {
		prefix = defaultPrefix
	}
	if strings.ContainsAny(prefix, "*\x00") {
		return "", nil, fmt.Errorf("%w: the prefix must not contain \"*\" or NUL", errInvalid)
	}
	log = o.Logger
	if log == nil {
		log = slog.Default()
	}
	return prefix, log.With("component", "credstore"), nil
}

// checkOrigin refuses an origin that no store can key a credential by.
func checkOrigin(origin string) error {
	if origin == "" {
		return fmt.Errorf("%w: empty origin", errInvalid)
	}
	if strings.ContainsRune(origin, 0) {
		return fmt.Errorf("%w: the origin contains NUL", errInvalid)
	}
	return nil
}

// checkPut applies the rules every Store shares (Credential Manager's) and returns the Credential Manager blob of
// c, which holds everything but the user name.
func checkPut(prefix, origin string, c Credential) ([]byte, error) {
	if err := checkOrigin(origin); err != nil {
		return nil, err
	}
	if n := utf16Len(prefix + origin); n > maxTargetLength {
		return nil, fmt.Errorf("%w: the key is %d UTF-16 units, the limit is %d", errInvalid, n, maxTargetLength)
	}
	if c.RefreshToken == "" {
		return nil, fmt.Errorf("%w: empty refresh token", errInvalid)
	}
	if strings.ContainsRune(c.UserName, 0) {
		return nil, fmt.Errorf("%w: the user name contains NUL", errInvalid)
	}
	if n := utf16Len(c.UserName); n > maxUserNameLen {
		return nil, fmt.Errorf("%w: the user name is %d UTF-16 units, the limit is %d", errInvalid, n, maxUserNameLen)
	}
	if y := c.SavedAt.UTC().Year(); y < 0 || y > 9999 {
		return nil, fmt.Errorf("%w: SavedAt is outside the years 0 to 9999", errInvalid)
	}
	data := marshalBlob(c, false)
	if len(data) > maxBlobBytes {
		return nil, fmt.Errorf("%w: the blob is %d bytes, the limit is %d", errBlobTooLarge, len(data), maxBlobBytes)
	}
	return data, nil
}

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// normalize returns c as a Store gives it back: SavedAt in UTC, to the millisecond, without a monotonic reading.
func normalize(c Credential) Credential {
	c.SavedAt = c.SavedAt.UTC().Truncate(time.Millisecond)
	return c
}

// blob is the JSON form of a Credential: Credential Manager's blob, and each entry of the file store, which adds
// the user name.
type blob struct {
	V            int    `json:"v"`
	RefreshToken string `json:"refreshToken"`
	DeviceID     string `json:"deviceId"`
	SavedAt      string `json:"savedAt"`
	UserName     string `json:"userName,omitempty"`
}

// marshalBlob returns c's blob, with the user name only when withUser is set. Callers have run checkPut on c.
func marshalBlob(c Credential, withUser bool) []byte {
	b := blob{
		V:            blobVersion,
		RefreshToken: c.RefreshToken,
		DeviceID:     c.DeviceID,
		SavedAt:      c.SavedAt.UTC().Format(savedAtLayout),
	}
	if withUser {
		b.UserName = c.UserName
	}
	data, err := json.Marshal(b) //nolint:gosec // G117: this is the stored credential itself
	if err != nil {
		// Can't happen: every field is a string or an int.
		panic("credstore: marshal a credential blob: " + err.Error())
	}
	return data
}

// unmarshalBlob reads a blob. A blob the store can't use gives a non-empty reason for the WARN, never the blob's
// content.
func unmarshalBlob(data []byte) (c Credential, reason string) {
	var b blob
	if err := json.Unmarshal(data, &b); err != nil {
		return Credential{}, "the blob does not parse"
	}
	if b.V != blobVersion {
		return Credential{}, fmt.Sprintf("the blob has version %d", b.V)
	}
	if b.RefreshToken == "" {
		return Credential{}, "the blob has no refresh token"
	}
	savedAt, err := time.Parse(time.RFC3339Nano, b.SavedAt)
	if err != nil {
		return Credential{}, "the blob's savedAt does not parse"
	}
	return normalize(Credential{
		RefreshToken: b.RefreshToken,
		DeviceID:     b.DeviceID,
		UserName:     b.UserName,
		SavedAt:      savedAt,
	}), ""
}

// warnUnreadable logs a stored credential that Get reports as ErrNotFound.
func warnUnreadable(log *slog.Logger, origin, reason string) {
	log.Warn("credstore: ignoring a stored credential that can't be read; the app links again",
		"origin", origin, "reason", reason)
}

// redacted replaces a Credential's secret fields in fmt, slog and JSON output.
const redacted = "[redacted]"

func redact(s string) string {
	if s == "" {
		return ""
	}
	return redacted
}

// String returns c with RefreshToken and UserName redacted.
func (c Credential) String() string {
	return fmt.Sprintf("credstore.Credential{RefreshToken:%s DeviceID:%s UserName:%s SavedAt:%s}",
		redact(c.RefreshToken), c.DeviceID, redact(c.UserName), c.SavedAt.UTC().Format(savedAtLayout))
}

// GoString is String, for %#v.
func (c Credential) GoString() string { return c.String() }

// Format writes String for every verb.
func (c Credential) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }

// LogValue logs the device id and SavedAt; the refresh token is redacted and the user name left out (usernames are
// never logged).
func (c Credential) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("refresh_token", redact(c.RefreshToken)),
		slog.String("device_id", c.DeviceID),
		slog.Time("saved_at", c.SavedAt),
	)
}

// MarshalJSON writes c with RefreshToken and UserName redacted, so that a Credential serialized by accident (into
// a bridge reply, a settings file, a diagnostic) carries no secret. Stores use their own encoding.
func (c Credential) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		RefreshToken string    `json:"refreshToken"`
		DeviceID     string    `json:"deviceId"`
		UserName     string    `json:"userName"`
		SavedAt      time.Time `json:"savedAt"`
	}{redact(c.RefreshToken), c.DeviceID, redact(c.UserName), c.SavedAt})
}
