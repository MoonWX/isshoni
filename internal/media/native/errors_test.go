package native

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/media"
)

// statusCodes is 01 §5.2's status table (README §3 item 17; 04 §12.2 follows it), for every status of the header.
var statusCodes = map[int32]media.ErrorCode{
	imErrInvalidArg:         media.CodeInvalid,
	imErrBadState:           media.CodeInternal, // from an audio call: CodeAudioNotRunning, in 05's audio.go
	imErrABI:                media.CodeDependency,
	imErrUnsupportedOS:      media.CodeUnsupportedOS,
	imErrPermissionDenied:   media.CodePermissionDenied,
	imErrAudioDeviceChanged: media.CodeInternal,
	imErrSourceGone:         media.CodeSourceGone,
	imErrEncoderLost:        media.CodeEncoderLost,
	imErrNoEncoder:          media.CodeNoEncoder,
	imErrCancelled:          media.CodeCancelled,
	imErrLimit:              media.CodeBusy,
	imErrDependency:         media.CodeDependency,
	imErrAudioUnavailable:   media.CodeAudioUnavailable,
	imErrNotImplemented:     media.CodeInternal,
	imErrTimeout:            media.CodeTimeout,
	imErrInternal:           media.CodeInternal,
	imTimeout:               media.CodeTimeout, // outside im_next_packet
	imEnded:                 media.CodeInternal,
	-42:                     media.CodeInternal, // a status the header doesn't define
}

// TestStatusErrors: every status, with every token of 04 §5.7 and with none, becomes a *media.Error with the
// table's code, the token as Reason, the Go method as Op and the rest of the text as the Detail.
func TestStatusErrors(t *testing.T) {
	tokens := append([]string{""}, detailTokens[:]...)
	for st, code := range statusCodes {
		for _, token := range tokens {
			lastErr := "avcodec-62.dll: LoadLibraryExW failed (0x4EC)"
			if token != "" {
				lastErr = token + ": " + lastErr
			}
			err := statusError("start", "im_start", st, lastErr)
			if err.Code != code {
				t.Errorf("%s with %q: code %q, want %q", statusName(st), lastErr, err.Code, code)
			}
			if err.Op != "start" {
				t.Errorf("%s: op %q", statusName(st), err.Op)
			}
			wantReason := token
			switch {
			case st == imErrABI:
				wantReason = tokenABI
			case st == imErrNotImplemented && token == "":
				wantReason = reasonNotImplemented
			}
			if err.Reason != wantReason {
				t.Errorf("%s with %q: reason %q, want %q", statusName(st), lastErr, err.Reason, wantReason)
			}
			if want := "im_start: avcodec-62.dll: LoadLibraryExW failed (0x4EC)"; err.Detail != want {
				t.Errorf("%s with %q: detail %q, want %q", statusName(st), lastErr, err.Detail, want)
			}
			if !errors.Is(err, &media.Error{Code: code}) {
				t.Errorf("%s: errors.Is with its code's sentinel is false", statusName(st))
			}
		}
	}
}

// TestStatusErrorSentinels: 03 maps with errors.Is on 01's sentinels and reads Reason (04 §5.7); CodeTimeout
// unwraps to context.DeadlineExceeded (04 §12.2: there is no native.ErrTimeout).
func TestStatusErrorSentinels(t *testing.T) {
	for _, c := range []struct {
		st       int32
		lastErr  string
		sentinel error
		reason   string
	}{
		{imErrDependency, "blocked_by_policy: avcodec-62.dll: LoadLibraryExW failed (0x4EC)", media.ErrDependency,
			tokenBlockedByPolicy},
		{imErrDependency, "missing: avutil-60.dll", media.ErrDependency, tokenMissing},
		{imErrDependency, "version: OpenH264 2.5.0, want 2.6.0", media.ErrDependency, tokenVersion},
		{imErrNoEncoder, "layer_failed: q", media.ErrNoEncoder, tokenLayerFailed},
		{imErrNoEncoder, "forced_failed: nvenc", media.ErrNoEncoder, tokenForcedFailed},
		{imErrSourceGone, "pick_expired: pick:7", media.ErrSourceGone, tokenPickExpired},
		{imErrLimit, "session_limit: 4 sessions", media.ErrBusy, tokenSessionLimit},
		{imErrLimit, "pick_busy: IM_ERR_LIMIT", media.ErrBusy, tokenPickBusy},
		{imErrPermissionDenied, "capture_denied: GraphicsCaptureAccess", media.ErrPermissionDenied, tokenCaptureDenied},
		{imErrUnsupportedOS, "os_too_old: 19044", media.ErrUnsupportedOS, tokenOSTooOld},
		{imErrCancelled, "", media.ErrCancelled, ""},
		{imErrABI, "im_start_config.struct_size 200 below v1.0", media.ErrDependency, tokenABI},
		{imErrTimeout, "engine_timeout: im_probe", context.DeadlineExceeded, tokenEngineTimeout},
		{imErrBadState, "not_initialized: IM_ERR_BAD_STATE", &media.Error{Code: media.CodeInternal},
			tokenNotInitialized},
	} {
		err := error(statusError("probe", "im_probe", c.st, c.lastErr))
		if !errors.Is(err, c.sentinel) {
			t.Errorf("%s %q: errors.Is(%v, %v) is false", statusName(c.st), c.lastErr, err, c.sentinel)
		}
		var me *media.Error
		if !errors.As(err, &me) || me.Reason != c.reason {
			t.Errorf("%s %q: reason %q, want %q", statusName(c.st), c.lastErr, me.Reason, c.reason)
		}
	}
}

func TestSplitDetail(t *testing.T) {
	for _, c := range []struct{ in, token, rest string }{
		{"", "", ""},
		{"missing: avcodec-62.dll", "missing", "avcodec-62.dll"},
		{"missing", "missing", ""},
		{"missing:", "", "missing:"},
		{"not_initialized: IM_ERR_BAD_STATE", "not_initialized", "IM_ERR_BAD_STATE"},
		{"avcodec_open2: -22", "", "avcodec_open2: -22"},           // a word that is not a token stays in the detail
		{"Missing: avcodec-62.dll", "", "Missing: avcodec-62.dll"}, // tokens are lower case
		{"im_start: layer_failed: q", "", "im_start: layer_failed: q"},
		{"encode_error: x", "", "encode_error: x"}, // ENCODER_CHANGED's reason, not a detail token
		{"layer_failed: q: more: text", "layer_failed", "q: more: text"},
	} {
		token, rest := splitDetail(c.in)
		if token != c.token || rest != c.rest {
			t.Errorf("splitDetail(%q) = %q, %q; want %q, %q", c.in, token, rest, c.token, c.rest)
		}
	}
}

// TestStatusErrorDetail: an empty text gets the status's name, so a log line always says what failed.
func TestStatusErrorDetail(t *testing.T) {
	if got := statusError("open", "im_init", imErrBadState, "").Detail; got != "im_init: IM_ERR_BAD_STATE" {
		t.Errorf("detail %q", got)
	}
	if got := statusError("open", "im_init", -42, "").Detail; got != "im_init: status -42" {
		t.Errorf("detail %q", got)
	}
	err := statusError("open", "im_init", imErrBadState, "not_initialized")
	if err.Reason != tokenNotInitialized || err.Detail != "im_init: IM_ERR_BAD_STATE" {
		t.Errorf("a bare token: %#v", err)
	}
	if s := err.Error(); !strings.Contains(s, "open: internal (not_initialized)") {
		t.Errorf("Error() = %q", s)
	}
}

func TestStatusNames(t *testing.T) {
	for st := range statusCodes {
		name := statusName(st)
		if st == -42 {
			if name != "status -42" {
				t.Errorf("statusName(-42) = %q", name)
			}
			continue
		}
		if v, ok := abiConstants[name]; !ok || v != int64(st) {
			t.Errorf("statusName(%d) = %q, which is not that status's #define", st, name)
		}
	}
	if statusName(imOK) != "IM_OK" {
		t.Error("IM_OK")
	}
}

// TestNotImplemented: the binding's stubs answer like the DLL's IM_ERR_NOT_IMPLEMENTED.
func TestNotImplemented(t *testing.T) {
	err := notImplemented("probe")
	native := statusError("probe", "im_probe", imErrNotImplemented, "im_probe: not implemented")
	if err.Code != native.Code || err.Reason != native.Reason || err.Op != "probe" {
		t.Errorf("stub %#v, DLL %#v", err, native)
	}
	if c := errClosed("clock"); c.Code != media.CodeInternal || c.Reason != tokenNotInitialized || c.Op != "clock" {
		t.Errorf("errClosed: %#v", c)
	}
}

func TestLoadErrorReason(t *testing.T) {
	for code, want := range map[uint32]string{
		winErrorFileNotFound:           tokenMissing,
		winErrorPathNotFound:           tokenMissing,
		winErrorModNotFound:            tokenMissing,
		winErrorInvalidImageHash:       tokenBlockedByPolicy,
		winErrorAccessDisabledByPolicy: tokenBlockedByPolicy,
		winErrorBadExeFormat:           "",
		0:                              "",
	} {
		if got := loadErrorReason(code); got != want {
			t.Errorf("loadErrorReason(%d) = %q, want %q", code, got, want)
		}
	}
}
