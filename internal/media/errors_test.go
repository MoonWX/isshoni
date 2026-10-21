package media_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/MoonWX/isshoni/internal/media"
)

// sentinels are every code sentinel of the contract, 05's ErrAudioNotRunning included.
var sentinels = []*media.Error{
	media.ErrPermissionDenied, media.ErrEncoderLost, media.ErrSourceGone, media.ErrUnsupportedOS, media.ErrNoEncoder,
	media.ErrDependency, media.ErrAudioUnavailable, media.ErrCancelled, media.ErrBusy, media.ErrStopped,
	media.ErrInvalid, media.ErrAudioNotRunning,
}

// TestErrorCodes pins the codes' strings: they cross into the app's bridge codes and are never renamed or reused.
func TestErrorCodes(t *testing.T) {
	for code, want := range map[media.ErrorCode]string{
		media.CodePermissionDenied: "permission_denied",
		media.CodeEncoderLost:      "encoder_lost",
		media.CodeSourceGone:       "source_gone",
		media.CodeUnsupportedOS:    "unsupported_os",
		media.CodeNoEncoder:        "no_encoder",
		media.CodeDependency:       "dependency",
		media.CodeAudioUnavailable: "audio_unavailable",
		media.CodeAudioNotRunning:  "audio_not_running",
		media.CodeCancelled:        "cancelled",
		media.CodeBusy:             "busy",
		media.CodeStopped:          "stopped",
		media.CodeTimeout:          "timeout",
		media.CodeInvalid:          "invalid",
		media.CodeInternal:         "internal",
	} {
		if string(code) != want {
			t.Errorf("code %q, want %q", code, want)
		}
	}
	for _, s := range sentinels {
		if s.Op != "" || s.Reason != "" || s.Detail != "" {
			t.Errorf("sentinel %#v has more than a code", s)
		}
	}
}

// TestErrorIs: errors.Is matches by code, whatever the Op, Reason and Detail, also through wrapping, and a code
// matches only its own sentinel.
func TestErrorIs(t *testing.T) {
	for _, s := range sentinels {
		err := &media.Error{Code: s.Code, Op: "start", Reason: "some_reason", Detail: "native text"}
		wrapped := fmt.Errorf("core: start share: %w", err)
		for _, other := range sentinels {
			want := other.Code == s.Code
			if got := errors.Is(err, other); got != want {
				t.Errorf("errors.Is(%v, %v) = %v, want %v", err, other, got, want)
			}
			if got := errors.Is(wrapped, other); got != want {
				t.Errorf("errors.Is(%v, %v) = %v, want %v", wrapped, other, got, want)
			}
		}
	}

	gone := &media.Error{Code: media.CodeSourceGone, Op: "next", Reason: "pick_expired"}
	for _, tc := range []struct {
		name   string
		err    error
		target error
		want   bool
	}{
		{"same code, bare", &media.Error{Code: media.CodeSourceGone}, media.ErrSourceGone, true},
		{"same code with a reason", gone, media.ErrSourceGone, true},
		{"the same pointer", gone, gone, true},
		{"a target with an Op", gone, &media.Error{Code: media.CodeSourceGone, Op: "next"}, false},
		{"a target with a Reason", gone, &media.Error{Code: media.CodeSourceGone, Reason: "pick_expired"}, false},
		{"a target with a Detail", gone, &media.Error{Code: media.CodeSourceGone, Detail: "x"}, false},
		{"another code", gone, media.ErrEncoderLost, false},
		{"a nil *Error target", gone, (*media.Error)(nil), false},
		{"another error type", gone, errors.New("source_gone"), false},
		{"audio not running", &media.Error{Code: media.CodeAudioNotRunning, Op: "audio_set_muted"},
			media.ErrAudioNotRunning, true},
		{"audio not running is not unavailable", &media.Error{Code: media.CodeAudioNotRunning},
			media.ErrAudioUnavailable, false},
		{"self pick", &media.Error{Code: media.CodeInvalid, Op: "pick", Reason: "self_window"}, media.ErrInvalid, true},
		{"abi", &media.Error{Code: media.CodeDependency, Op: "open", Reason: "abi"}, media.ErrDependency, true},
	} {
		if got := errors.Is(tc.err, tc.target); got != tc.want {
			t.Errorf("%s: errors.Is(%v, %v) = %v, want %v", tc.name, tc.err, tc.target, got, tc.want)
		}
	}

	var me *media.Error
	if !errors.As(fmt.Errorf("wrapped: %w", gone), &me) || me != gone {
		t.Errorf("errors.As = %v, want %v", me, gone)
	}
}

// TestErrorTimeout: a timeout error unwraps to context.DeadlineExceeded (04 §12.2), and no other code unwraps.
func TestErrorTimeout(t *testing.T) {
	timeout := &media.Error{Code: media.CodeTimeout, Op: "list_audio_apps"}
	if !errors.Is(timeout, context.DeadlineExceeded) || !errors.Is(fmt.Errorf("x: %w", timeout), context.DeadlineExceeded) {
		t.Error("a timeout error is not context.DeadlineExceeded")
	}
	if errors.Is(timeout, context.Canceled) {
		t.Error("a timeout error is context.Canceled")
	}
	if !errors.Is(timeout, &media.Error{Code: media.CodeTimeout}) {
		t.Error("a timeout error does not match its code")
	}
	for _, s := range sentinels {
		err := &media.Error{Code: s.Code, Op: "start"}
		if errors.Unwrap(err) != nil || errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%v unwraps to %v", err, errors.Unwrap(err))
		}
	}
}

func TestErrorString(t *testing.T) {
	for _, tc := range []struct {
		err  *media.Error
		want string
	}{
		{media.ErrSourceGone, "media: source_gone"},
		{&media.Error{Code: media.CodeSourceGone, Op: "next"}, "media: next: source_gone"},
		{&media.Error{Code: media.CodeDependency, Op: "start", Reason: "abi"}, "media: start: dependency (abi)"},
		{&media.Error{Code: media.CodeDependency, Op: "probe", Reason: "missing", Detail: "the codec library did not load"},
			"media: probe: dependency (missing): the codec library did not load"},
		{&media.Error{Code: media.CodeInternal, Op: "pick", Detail: "worker stalled"}, "media: pick: internal: worker stalled"},
		{&media.Error{Code: media.CodeInvalid, Reason: "self_window"}, "media: invalid (self_window)"},
		{nil, "media: <nil>"},
	} {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("Error() = %q, want %q", got, tc.want)
		}
	}
}
