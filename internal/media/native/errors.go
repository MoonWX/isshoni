package native

import (
	"fmt"
	"strings"

	"github.com/MoonWX/isshoni/internal/media"
)

// The detail tokens of 04 §5.7 (fixed in v1: never renamed or reused; new ones are additive). The engine puts one in
// front of im_last_error's text ("<token>: detail"), into im_encoder_info.reason and in front of the ENDED event's
// detail; the binding splits it off into media.Error.Reason and media.EncoderInfo.Reason. abi_test.go checks this
// list against native/common's token table.
const (
	tokenMissing           = "missing"
	tokenVersion           = "version"
	tokenBlockedByPolicy   = "blocked_by_policy"
	tokenDriverTooOld      = "driver_too_old"
	tokenDisabled          = "disabled"
	tokenNoMediaFoundation = "no_media_foundation"
	tokenSlowOpen          = "slow_open"
	tokenOpenFailed        = "open_failed"
	tokenTestEncodeFailed  = "test_encode_failed" //nolint:gosec // G101: a detail token of 04 §5.7, not a credential
	tokenLayerFailed       = "layer_failed"
	tokenForcedFailed      = "forced_failed"
	tokenDeviceRemoved     = "device_removed"
	tokenSessionLimit      = "session_limit"
	tokenPickBusy          = "pick_busy"
	tokenPickExpired       = "pick_expired"
	tokenSelfWindow        = "self_window" // Go only: Pick's answer to an IM_SRCF_SELF pick
	tokenOSTooOld          = "os_too_old"
	tokenCaptureDenied     = "capture_denied"
	tokenDPIUnaware        = "dpi_unaware"
	tokenEngineTimeout     = "engine_timeout"
	tokenNotInitialized    = "not_initialized"
	tokenNotWatching       = "not_watching"
	tokenABI               = "abi"
)

// detailTokens is every token of 04 §5.7, in its order.
var detailTokens = [...]string{
	tokenMissing, tokenVersion, tokenBlockedByPolicy, tokenDriverTooOld, tokenDisabled, tokenNoMediaFoundation,
	tokenSlowOpen, tokenOpenFailed, tokenTestEncodeFailed, tokenLayerFailed, tokenForcedFailed, tokenDeviceRemoved,
	tokenSessionLimit, tokenPickBusy, tokenPickExpired, tokenSelfWindow, tokenOSTooOld, tokenCaptureDenied,
	tokenDPIUnaware, tokenEngineTimeout, tokenNotInitialized, tokenNotWatching, tokenABI,
}

// reasonNotImplemented is the Reason of the binding's "not implemented" errors: a part that a later slice of
// docs/m2/README.md §5 fills, or IM_ERR_NOT_IMPLEMENTED from the DLL. Like internal/media's ParseAudioReport stub,
// they are CodeInternal (01 §5.2 maps IM_ERR_NOT_IMPLEMENTED to internal).
const reasonNotImplemented = "not_implemented"

// isDetailToken reports whether s is one of the tokens of 04 §5.7.
func isDetailToken(s string) bool {
	for _, t := range detailTokens {
		if t == s {
			return true
		}
	}
	return false
}

// splitDetail splits a native detail ("<token>: rest", from im_last_error or an ENDED event) into its token and the
// rest. Only the tokens of 04 §5.7 count, so a detail such as "avcodec_open2: -22" keeps its first word.
func splitDetail(s string) (token, rest string) {
	if i := strings.Index(s, ": "); i > 0 && isDetailToken(s[:i]) {
		return s[:i], s[i+2:]
	}
	if isDetailToken(s) {
		return s, ""
	}
	return "", s
}

// statusCode maps a native status to the contract's code: 01 §5.2's table, which README §3 item 17 makes the rule
// (04 §12.2 follows it). IM_ERR_BAD_STATE from an audio call is the audio exception (CodeAudioNotRunning), which 05's
// audio.go applies (05 §13.1); here, from every other call, it is internal. IM_TIMEOUT (1) outside im_next_packet is
// a timeout like IM_ERR_TIMEOUT: only Next loops on it.
func statusCode(st int32) media.ErrorCode {
	switch st {
	case imErrInvalidArg:
		return media.CodeInvalid
	case imErrABI:
		return media.CodeDependency
	case imErrUnsupportedOS:
		return media.CodeUnsupportedOS
	case imErrPermissionDenied:
		return media.CodePermissionDenied
	case imErrSourceGone:
		return media.CodeSourceGone
	case imErrEncoderLost:
		return media.CodeEncoderLost
	case imErrNoEncoder:
		return media.CodeNoEncoder
	case imErrCancelled:
		return media.CodeCancelled
	case imErrLimit:
		return media.CodeBusy
	case imErrDependency:
		return media.CodeDependency
	case imErrAudioUnavailable:
		return media.CodeAudioUnavailable
	case imErrTimeout, imTimeout:
		return media.CodeTimeout
	default: // IM_ERR_BAD_STATE, IM_ERR_AUDIO_DEVICE_CHANGED, IM_ERR_NOT_IMPLEMENTED, IM_ERR_INTERNAL, IM_ENDED, others
		return media.CodeInternal
	}
}

// statusError is the *media.Error of a failed call: op is the Go method ("start", "pick", "probe", or for the audio
// calls the export's name without im_, 05 §13.1), export the C function, st its non-zero status and lastErr
// im_last_error's text read on the same OS thread. The Reason is the text's token (04 §5.7); IM_ERR_ABI always has
// Reason "abi" and IM_ERR_NOT_IMPLEMENTED "not_implemented" when the text names none. The Detail, for logs only,
// names the export and keeps the rest of the text (or the status's name when the text is empty).
func statusError(op, export string, st int32, lastErr string) *media.Error {
	token, rest := splitDetail(strings.TrimSpace(lastErr))
	switch {
	case st == imErrABI:
		token = tokenABI
	case st == imErrNotImplemented && token == "":
		token = reasonNotImplemented
	}
	if rest == "" {
		rest = statusName(st)
	}
	return &media.Error{Code: statusCode(st), Op: op, Reason: token, Detail: export + ": " + rest}
}

// codeError is a *media.Error for a failure the binding itself finds (no native status): a bad argument, a JSON
// document it can't read, the ABI check.
func codeError(code media.ErrorCode, op, reason, format string, args ...any) *media.Error {
	return &media.Error{Code: code, Op: op, Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// notImplemented is the error of a part of the API that a later slice fills (docs/m2/README.md §4, "Interfaces
// first").
func notImplemented(op string) *media.Error {
	return &media.Error{Code: media.CodeInternal, Op: op, Reason: reasonNotImplemented,
		Detail: "internal/media/native: not implemented yet"}
}

// errClosed is the error of a call after Engine.Close: the engine's not_initialized, without a DLL call.
func errClosed(op string) *media.Error {
	return &media.Error{Code: media.CodeInternal, Op: op, Reason: tokenNotInitialized, Detail: "the engine is closed"}
}

// statusName is the header's name of a status ("IM_ERR_DEPENDENCY"), for details in logs.
func statusName(st int32) string {
	switch st {
	case imOK:
		return "IM_OK"
	case imTimeout:
		return "IM_TIMEOUT"
	case imEnded:
		return "IM_ENDED"
	case imErrInvalidArg:
		return "IM_ERR_INVALID_ARG"
	case imErrBadState:
		return "IM_ERR_BAD_STATE"
	case imErrABI:
		return "IM_ERR_ABI"
	case imErrUnsupportedOS:
		return "IM_ERR_UNSUPPORTED_OS"
	case imErrPermissionDenied:
		return "IM_ERR_PERMISSION_DENIED"
	case imErrAudioDeviceChanged:
		return "IM_ERR_AUDIO_DEVICE_CHANGED"
	case imErrSourceGone:
		return "IM_ERR_SOURCE_GONE"
	case imErrEncoderLost:
		return "IM_ERR_ENCODER_LOST"
	case imErrNoEncoder:
		return "IM_ERR_NO_ENCODER"
	case imErrCancelled:
		return "IM_ERR_CANCELLED"
	case imErrLimit:
		return "IM_ERR_LIMIT"
	case imErrDependency:
		return "IM_ERR_DEPENDENCY"
	case imErrAudioUnavailable:
		return "IM_ERR_AUDIO_UNAVAILABLE"
	case imErrNotImplemented:
		return "IM_ERR_NOT_IMPLEMENTED"
	case imErrTimeout:
		return "IM_ERR_TIMEOUT"
	case imErrInternal:
		return "IM_ERR_INTERNAL"
	}
	return fmt.Sprintf("status %d", st)
}

// Win32 errors of a failed LoadLibraryEx (winerror.h), for loadErrorReason. They are plain numbers here so that the
// mapping is tested on every OS.
const (
	winErrorFileNotFound           = 2
	winErrorPathNotFound           = 3
	winErrorModNotFound            = 126
	winErrorBadExeFormat           = 193
	winErrorInvalidImageHash       = 577  // STATUS_INVALID_IMAGE_HASH: Smart App Control, WDAC
	winErrorAccessDisabledByPolicy = 1260 // software restriction or AppLocker policy
)

// loadErrorReason is the Reason of a failed load of isshoni-media.dll (Code dependency, 03's "Reinstall isshoni"):
// missing when the DLL or one of its imports is not there, blocked_by_policy when a code-integrity policy refused
// it (03 gives Smart App Control's text, 04 §17), and "" for anything else (the error number is in the detail).
func loadErrorReason(winErr uint32) string {
	switch winErr {
	case winErrorFileNotFound, winErrorPathNotFound, winErrorModNotFound:
		return tokenMissing
	case winErrorInvalidImageHash, winErrorAccessDisabledByPolicy:
		return tokenBlockedByPolicy
	}
	return ""
}
