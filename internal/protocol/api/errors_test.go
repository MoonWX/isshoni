package api

import (
	"errors"
	"fmt"
	"go/ast"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// wantStatus is 03 §12.2's code table (with 04's rows), written out a second time on purpose: StatusOf must agree
// with the spec, not only with itself.
var wantStatus = map[string]int{
	"bad_request":            400,
	"validation_failed":      422,
	"unsupported_media_type": 415,
	"payload_too_large":      413,
	"method_not_allowed":     405,
	"unauthenticated":        401,
	"invalid_credentials":    401,
	"invalid_token":          401,
	"wrong_password":         403,
	"forbidden":              403,
	"csrf_failed":            403,
	"account_pending":        403,
	"account_disabled":       403,
	"registration_closed":    403,
	"invite_required":        403,
	"not_found":              404,
	"user_not_found":         404,
	"room_not_found":         404,
	"invite_invalid":         404,
	"invite_expired":         410,
	"invite_used_up":         410,
	"invite_revoked":         410,
	"setup_unavailable":      404,
	"setup_token_invalid":    404,
	"reset_token_invalid":    404,
	"username_taken":         409,
	"room_name_taken":        409,
	"last_admin":             409,
	"self_action_forbidden":  409,
	"room_is_default":        409,
	"limit_reached":          409,
	"setting_locked":         409,
	"push_endpoint_rejected": 422,
	"push_unavailable":       503,
	"rate_limited":           429,
	"server_busy":            503,
	"internal":               500,
	"bad_sdp":                400,
	"transport_disabled":     409,
	"doctor_busy":            429,
	"not_ready":              503,
	"server_shutdown":        503,
	"backup_invalid":         400,
	"backup_newer":           409,
	"restore_in_progress":    409,
	"insufficient_storage":   507,
	"authorization_pending":  400,
	"slow_down":              400,
	"access_denied":          400,
	"expired_token":          400,
	"invalid_grant":          400,
	"device_code_invalid":    404,
}

// sharedWithProtocol is 03 §12.2's list of codes that 01's WebSocket errors use with the same meaning.
var sharedWithProtocol = []string{
	"account_disabled", "bad_request", "forbidden", "internal", "rate_limited", "room_not_found", "server_shutdown",
	"unauthenticated",
}

var snakeCase = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)

// codeConsts returns the package's Code* constants, by name.
func codeConsts(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, c := range stringConsts(t, parseSources(t, ".")) {
		if strings.HasPrefix(c.Name, "Code") {
			if c.Type != "" {
				t.Errorf("%s is typed %s; error codes are untyped string constants", c.Name, c.Type)
			}
			out[c.Name] = c.Value
		}
	}
	if len(out) == 0 {
		t.Fatal("found no Code* constants")
	}
	return out
}

func TestStatusOfMatchesSpec(t *testing.T) {
	for code, want := range wantStatus {
		if got := StatusOf(code); got != want {
			t.Errorf("StatusOf(%q) = %d, want %d", code, got, want)
		}
	}
	if got, want := Codes(), slices.Sorted(maps.Keys(wantStatus)); !slices.Equal(got, want) {
		t.Errorf("Codes() = %v\nwant %v", got, want)
	}
}

// TestEveryCodeHasStatus checks that each Code* constant has its own row in the status table and that the table has
// no row without a constant.
func TestEveryCodeHasStatus(t *testing.T) {
	consts := codeConsts(t)
	byValue := map[string]string{}
	for name, v := range consts {
		if prev, dup := byValue[v]; dup {
			t.Errorf("%s and %s both have the value %q", prev, name, v)
		}
		byValue[v] = name
		if !snakeCase.MatchString(v) {
			t.Errorf("%s = %q is not snake_case", name, v)
		}
		s, ok := statusByCode[v]
		if !ok {
			t.Errorf("%s = %q has no status in StatusOf's table", name, v)
			continue
		}
		if s < 400 || s > 599 {
			t.Errorf("%s: status %d is not an error status", name, s)
		}
	}
	for code := range statusByCode {
		if _, ok := byValue[code]; !ok {
			t.Errorf("the status table has %q but no Code* constant declares it", code)
		}
	}
}

func TestStatusOfUnknownCode(t *testing.T) {
	for _, code := range []string{"", "no_such_code", "BAD_REQUEST"} {
		if got := StatusOf(code); got != 500 {
			t.Errorf("StatusOf(%q) = %d, want 500", code, got)
		}
	}
}

func TestErrorValue(t *testing.T) {
	e := NewError(CodeRoomNotFound)
	if got := e.Error(); got != "api: room_not_found" {
		t.Errorf("Error() = %q", got)
	}
	var err error = e
	wrapped := fmt.Errorf("rename room: %w", err)
	if !IsCode(wrapped, CodeRoomNotFound) {
		t.Error("IsCode(wrapped, room_not_found) = false")
	}
	if IsCode(wrapped, CodeNotFound) {
		t.Error("IsCode(wrapped, not_found) = true")
	}
	if IsCode(errors.New("plain"), CodeInternal) || IsCode(nil, CodeInternal) {
		t.Error("IsCode matched a non-*Error")
	}
	var target *Error
	if !errors.As(wrapped, &target) || target != e {
		t.Error("errors.As did not find the *Error")
	}
}

// TestSharedCodesWithProtocol is 03 §12.2's check: the codes present both here and among 01's protocol.ErrorCode*
// constants are exactly sharedWithProtocol. It reads internal/protocol's source rather than importing it, so this
// package's tests don't depend on the protocol package's Go API. It skips only while internal/protocol has no Go
// files (before S03 is merged); TestSharedCodesCheck runs the same check on 01 §12.1's table meanwhile.
func TestSharedCodesWithProtocol(t *testing.T) {
	files := parseSources(t, "..")
	if len(files) == 0 {
		t.Skip("internal/protocol has no Go files yet (S03)")
	}
	checkSharedCodes(t, files, sharedWithProtocol)
}

// protocolCodesSrc is a copy of 01 §12.1's code table as protocol Go source.
const protocolCodesSrc = `package protocol

type ErrorCode string

const (
	ErrorCodeBadMessage          ErrorCode = "bad_message"
	ErrorCodeHelloRequired       ErrorCode = "hello_required"
	ErrorCodeHelloTimeout        ErrorCode = "hello_timeout"
	ErrorCodeIdleTimeout         ErrorCode = "idle_timeout"
	ErrorCodeProtocolUnsupported ErrorCode = "protocol_unsupported"
	ErrorCodeClientOutdated      ErrorCode = "client_outdated"
	ErrorCodeUnauthenticated     ErrorCode = "unauthenticated"
	ErrorCodeSessionRevoked      ErrorCode = "session_revoked"
	ErrorCodeAccountDisabled     ErrorCode = "account_disabled"
	ErrorCodeTooManyConnections  ErrorCode = "too_many_connections"
	ErrorCodeRateLimited         ErrorCode = "rate_limited"
	ErrorCodeSlowConnection      ErrorCode = "slow_connection"
	ErrorCodeReplaced            ErrorCode = "replaced"
	ErrorCodeServerShutdown      ErrorCode = "server_shutdown"
	ErrorCodeInternal            ErrorCode = "internal"
	ErrorCodeBadRequest          ErrorCode = "bad_request"
	ErrorCodeUnknownType         ErrorCode = "unknown_type"
	ErrorCodeMessageTooLarge     ErrorCode = "message_too_large"
	ErrorCodeForbidden           ErrorCode = "forbidden"
	ErrorCodeFeatureDisabled     ErrorCode = "feature_disabled"
	ErrorCodeNotInRoom           ErrorCode = "not_in_room"
	ErrorCodeRoomNotFound        ErrorCode = "room_not_found"
	ErrorCodeRoomFull            ErrorCode = "room_full"
	ErrorCodeKicked              ErrorCode = "kicked"
	ErrorCodeRoomClosed          ErrorCode = "room_closed"
	ErrorCodeShareNotFound       ErrorCode = "share_not_found"
	ErrorCodeShareLimit          ErrorCode = "share_limit"
	ErrorCodeCodecNotSupported   ErrorCode = "codec_not_supported"
	ErrorCodeSDPInvalid          ErrorCode = "sdp_invalid"
	ErrorCodeStaleNegotiation    ErrorCode = "stale_negotiation"
	ErrorCodeAgentTargetNotFound ErrorCode = "agent_target_not_found"
)
`

// TestSharedCodesCheck runs checkSharedCodes on 01 §12.1's table (so the check works before S03 lands), and on the
// same table plus a code that collides with a REST code, which the check must report.
func TestSharedCodesCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "errors.go")
	if err := os.WriteFile(path, []byte(protocolCodesSrc), 0o600); err != nil {
		t.Fatal(err)
	}
	checkSharedCodes(t, parseSources(t, dir), sharedWithProtocol)

	collide := strings.Replace(protocolCodesSrc, "\n)\n", "\n\tErrorCodeNotFound ErrorCode = \"not_found\"\n)\n", 1)
	if err := os.WriteFile(path, []byte(collide), 0o600); err != nil {
		t.Fatal(err)
	}
	want := append(slices.Clone(sharedWithProtocol), "not_found")
	slices.Sort(want)
	checkSharedCodes(t, parseSources(t, dir), want)
}

// checkSharedCodes asserts that the codes present both in the status table and among the ErrorCode* string
// constants of files are exactly want (sorted).
func checkSharedCodes(t *testing.T, files []*ast.File, want []string) {
	t.Helper()
	protoCodes := map[string]bool{}
	for _, c := range stringConsts(t, files) {
		if strings.HasPrefix(c.Name, "ErrorCode") {
			protoCodes[c.Value] = true
		}
	}
	if len(protoCodes) == 0 {
		t.Fatal("internal/protocol has Go files but no ErrorCode* string constants (01 §12.1)")
	}
	var both []string
	for code := range statusByCode {
		if protoCodes[code] {
			both = append(both, code)
		}
	}
	slices.Sort(both)
	if !slices.Equal(both, want) {
		t.Errorf("codes in both api and protocol = %v\nwant %v (03 §12.2: a shared code must mean the same thing on "+
			"both sides, and a new code must not reuse the other side's code)", both, want)
	}
}
