package media

import (
	"context"
	"strings"
)

// ErrorCode is the kind of an engine failure (01 §5.2). Codes are never renamed or reused.
type ErrorCode string

// The error codes. The native binding maps the C ABI's statuses to them (01 §5.2's table, 04 §12.2); 05's audio
// calls use CodeAudioUnavailable, CodeAudioNotRunning and CodeTimeout (05 §13.1).
const (
	CodePermissionDenied ErrorCode = "permission_denied"
	CodeEncoderLost      ErrorCode = "encoder_lost"
	CodeSourceGone       ErrorCode = "source_gone"
	CodeUnsupportedOS    ErrorCode = "unsupported_os"
	CodeNoEncoder        ErrorCode = "no_encoder"
	// CodeDependency: FFmpeg or OpenH264 missing, wrong version or blocked; an ABI mismatch (Reason "abi").
	CodeDependency ErrorCode = "dependency"
	// CodeAudioUnavailable: no audio capture path works (AudioController calls). Never from Start or Next.
	CodeAudioUnavailable ErrorCode = "audio_unavailable"
	// CodeAudioNotRunning: an audio call that needs a watcher or a live share (the header's not-started status).
	CodeAudioNotRunning ErrorCode = "audio_not_running"
	CodeCancelled       ErrorCode = "cancelled"
	CodeBusy            ErrorCode = "busy"
	CodeStopped         ErrorCode = "stopped"
	// CodeTimeout: a blocking native call hit its limit (never from Next: Next loops on the timeout status). An Error
	// with this code unwraps to context.DeadlineExceeded.
	CodeTimeout  ErrorCode = "timeout"
	CodeInvalid  ErrorCode = "invalid"
	CodeInternal ErrorCode = "internal"
)

// Error is an engine error. Only adapters translate it: the native binding makes it from im_status, the core maps it
// to a share's stop reason and notices, and the app's bridge to its error codes (docs/m2/README.md §4).
type Error struct {
	Code ErrorCode
	Op   string // "start", "next", "pick", "probe", …
	// Reason is a short machine token from 04 (missing, version, blocked_by_policy, driver_too_old, dpi_unaware,
	// slow_open, abi, self_window, …) that 03 may map to a text; "" when none.
	Reason string
	// Detail is the native message (im_last_error), for logs only: never shown, never sent.
	Detail string
}

// Error formats e as "media: <op>: <code>", plus " (<reason>)" and ": <detail>" when they are set. Without an Op it
// is "media: <code>".
func (e *Error) Error() string {
	if e == nil {
		return "media: <nil>"
	}
	var b strings.Builder
	b.WriteString("media: ")
	if e.Op != "" {
		b.WriteString(e.Op)
		b.WriteString(": ")
	}
	b.WriteString(string(e.Code))
	if e.Reason != "" {
		b.WriteString(" (")
		b.WriteString(e.Reason)
		b.WriteString(")")
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	return b.String()
}

// Is reports whether target is a code sentinel of e: a *Error with the same Code and an empty Op, Reason and
// Detail. So errors.Is(err, ErrSourceGone) holds for every source_gone error, whatever its Op, Reason and Detail, and
// errors.Is(err, ErrAudioNotRunning) for every audio_not_running one.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok || t == nil || e == nil {
		return false
	}
	return t.Code == e.Code && t.Op == "" && t.Reason == "" && t.Detail == ""
}

// Unwrap returns context.DeadlineExceeded for a CodeTimeout error, so errors.Is(err, context.DeadlineExceeded) holds
// for it (04 §12.2), and nil for every other code.
func (e *Error) Unwrap() error {
	if e != nil && e.Code == CodeTimeout {
		return context.DeadlineExceeded
	}
	return nil
}

// The code sentinels, for errors.Is. 05's ErrAudioNotRunning is declared in audio.go.
var (
	ErrPermissionDenied = &Error{Code: CodePermissionDenied}
	ErrEncoderLost      = &Error{Code: CodeEncoderLost}
	ErrSourceGone       = &Error{Code: CodeSourceGone}
	ErrUnsupportedOS    = &Error{Code: CodeUnsupportedOS}
	ErrNoEncoder        = &Error{Code: CodeNoEncoder}
	ErrDependency       = &Error{Code: CodeDependency}
	ErrAudioUnavailable = &Error{Code: CodeAudioUnavailable}
	ErrCancelled        = &Error{Code: CodeCancelled}
	ErrBusy             = &Error{Code: CodeBusy}
	ErrStopped          = &Error{Code: CodeStopped}
	ErrInvalid          = &Error{Code: CodeInvalid}
)
