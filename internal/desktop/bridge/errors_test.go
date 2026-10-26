package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/MoonWX/isshoni/internal/client/compat"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/audio"
	"github.com/MoonWX/isshoni/internal/desktop/bridge/codes"
	"github.com/MoonWX/isshoni/internal/desktop/servers"
	"github.com/MoonWX/isshoni/internal/desktop/settings"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestErrorJSON: Error() is 03 §6.4's JSON exactly, which the page parses from the rejection's message (W12); the
// marshaled form is the same object, and params is always an object.
func TestErrorJSON(t *testing.T) {
	cases := []struct {
		err  *Error
		want string
	}{
		{newError(codes.CodeBusy, nil), `{"code":"busy","params":{}}`},
		{invalidArgument("url"), `{"code":"invalid_argument","params":{"field":"url"}}`},
		{notImplemented("Share.Pick"), `{"code":"not_implemented","params":{"method":"Share.Pick"}}`},
		{newError(codes.CodeEngineDependency, map[string]any{"reason": "abi"}),
			`{"code":"engine.dependency","params":{"reason":"abi"}}`},
		// encoding/json escapes <, > and & in the message too.
		{newError(codes.CodeInternal, map[string]any{"reason": "<&>"}),
			`{"code":"internal","params":{"reason":"` + "\\u003c\\u0026\\u003e" + `"}}`},
	}
	for _, c := range cases {
		if got := c.err.Error(); got != c.want {
			t.Errorf("Error() = %s, want %s", got, c.want)
		}
		b, err := json.Marshal(c.err)
		if err != nil || string(b) != c.want {
			t.Errorf("json.Marshal = %s, %v; want %s", b, err, c.want)
		}
		var back errorJSON
		if err := json.Unmarshal([]byte(c.err.Error()), &back); err != nil || back.Code != c.err.Code ||
			back.Params == nil {
			t.Errorf("%s doesn't read back: %+v %v", c.want, back, err)
		}
	}
	// A parameter that doesn't marshal keeps the code.
	if got := newError(codes.CodeInternal, map[string]any{"x": make(chan int)}).Error(); got !=
		`{"code":"internal","params":{}}` {
		t.Errorf("unmarshalable params: %s", got)
	}
}

// TestMapError is 03 §22.1's error mapping: 02's errors, media codes → engine.<code>, wire and REST codes passed
// through, the app's and 05's sentinels; anything else is internal with one WARN line.
func TestMapError(t *testing.T) {
	check := compat.Check{Verdict: compat.UpdateApp, Reason: compat.ReasonMinClientVersion, ServerVersion: "0.3.0",
		MinClientVersion: "0.3.0"}
	cases := []struct {
		name   string
		err    error
		code   codes.Code
		params map[string]any
	}{
		{"nil", nil, "", nil},
		{"bridge error", newError(codes.CodeBusy, nil), codes.CodeBusy, nil},
		{"wrapped bridge error", fmt.Errorf("x: %w", newError(codes.CodeNotFound, nil)), codes.CodeNotFound, nil},
		{"offline", fmt.Errorf("%w: dial", deviceauth.ErrOffline), codes.CodeServerUnreachable, nil},
		{"network", deviceauth.ErrNetwork, codes.CodeServerUnreachable, nil},
		{"bad response", deviceauth.ErrBadResponse, codes.CodeServerUnreachable, nil},
		{"deadline", context.DeadlineExceeded, codes.CodeServerUnreachable, nil},
		{"cancelled", context.Canceled, codes.CodeInternal, map[string]any{"reason": "cancelled"}},
		{"incompatible", deviceauth.ErrIncompatible, codes.CodeIncompatibleServer, nil},
		{"too many servers", deviceauth.ErrTooManyServers, codes.CodeTooManyServers, nil},
		{"not linked", deviceauth.ErrNotLinked, codes.CodeNotLinked, nil},
		{"media", &media.Error{Code: media.CodeDependency, Reason: "abi", Op: "start", Detail: "native text"},
			codes.CodeEngineDependency, map[string]any{"reason": "abi"}},
		{"media wrapped", fmt.Errorf("share: %w", &media.Error{Code: media.CodeNoEncoder}), codes.CodeEngineNoEncoder,
			map[string]any{"reason": ""}},
		{"media invalid self_window", &media.Error{Code: media.CodeInvalid, Reason: "self_window"},
			codes.CodeEngineInvalid, map[string]any{"reason": "self_window"}},
		{"media cancelled never surfaces", &media.Error{Code: media.CodeCancelled}, codes.CodeEngineInternal,
			map[string]any{"reason": ""}},
		{"rest rate limited", &api.Error{Code: "rate_limited", RetryAfter: 30}, "rate_limited",
			map[string]any{"retryAfter": 30}},
		{"rest validation", &api.Error{Code: "validation_failed", Fields: map[string]string{"username": "required"}},
			"validation_failed", map[string]any{"fields": map[string]string{"username": "required"}}},
		{"wire", &protocol.Error{Code: protocol.ErrorCodeShareLimit, Params: map[string]any{"limit": 2}},
			"share_limit", map[string]any{"limit": 2}},
		{"wire retry", &protocol.Error{Code: protocol.ErrorCodeRateLimited, RetryAfterMs: 500}, "rate_limited",
			map[string]any{"retryAfterMs": 500}},
		{"probe", &servers.Error{Code: servers.CodeTLSUntrusted, Err: errors.New("x509")}, codes.CodeTLSUntrusted, nil},
		{"probe incompatible", &servers.Error{Code: servers.CodeIncompatibleServer, Compat: &check},
			codes.CodeIncompatibleServer, map[string]any{"verdict": "update_app", "reason": "min_client_version",
				"serverVersion": "0.3.0", "minClientVersion": "0.3.0"}},
		{"app invalid", fmt.Errorf("%w: x", app.ErrInvalidArgument), codes.CodeInvalidArgument, nil},
		{"app busy", app.ErrBusy, codes.CodeBusy, nil},
		{"app not implemented", app.ErrNotImplemented, codes.CodeNotImplemented, nil},
		{"unknown prompt", fmt.Errorf("audio: %w", audio.ErrUnknownPrompt), codes.CodeNotFound, nil},
		{"locked app", audio.ErrLockedApp, codes.CodeInvalidArgument, map[string]any{"field": "appId"}},
		{"bad decision", audio.ErrInvalidDecision, codes.CodeInvalidArgument, map[string]any{"field": "decision"}},
		{"unknown tip", audio.ErrUnknownTip, codes.CodeInvalidArgument, map[string]any{"field": "code"}},
		{"settings field", fmt.Errorf("x: %w", &settings.FieldError{Field: "hotkeys.pause"}), codes.CodeInvalidArgument,
			map[string]any{"field": "hotkeys.pause"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			got := mapError(c.err, testLog(&buf))
			if c.err == nil {
				if got != nil {
					t.Fatalf("mapError(nil) = %v", got)
				}
				return
			}
			var be *Error
			if !errors.As(got, &be) {
				t.Fatalf("mapError = %T %v, not an *Error", got, got)
			}
			if be.Code != c.code {
				t.Fatalf("code %q, want %q", be.Code, c.code)
			}
			if c.params != nil {
				want, _ := json.Marshal(c.params)
				have, _ := json.Marshal(be.Params)
				if string(want) != string(have) {
					t.Fatalf("params %s, want %s", have, want)
				}
			}
			if buf.Len() != 0 {
				t.Errorf("a known error was logged: %s", buf.String())
			}
		})
	}

	var buf bytes.Buffer
	got := mapError(errors.New("something new"), testLog(&buf))
	var be *Error
	if !errors.As(got, &be) || be.Code != codes.CodeInternal || got.Error() != `{"code":"internal","params":{}}` {
		t.Fatalf("unknown error: %v", got)
	}
	if !strings.Contains(buf.String(), "something new") || !strings.Contains(buf.String(), `"level":"WARN"`) {
		t.Fatalf("an unknown error isn't logged once at WARN: %s", buf.String())
	}
	// The cause stays for errors.Is, and never reaches the JSON.
	if !strings.Contains(fmt.Sprint(errors.Unwrap(got)), "something new") || strings.Contains(got.Error(), "new") {
		t.Fatalf("cause: %v / %s", errors.Unwrap(got), got)
	}
}

// TestCodesAreSnakeCase: every code of bridge/codes is snake_case, optionally behind the engine. or update. prefix,
// and unique.
func TestCodesAreSnakeCase(t *testing.T) {
	all := allCodes(t)
	if len(all) < 40 {
		t.Fatalf("only %d codes found", len(all))
	}
	seen := map[string]bool{}
	for name, v := range all {
		rest := strings.TrimPrefix(strings.TrimPrefix(v, codes.EnginePrefix), codes.UpdatePrefix)
		for i, r := range rest {
			ok := r >= 'a' && r <= 'z' || r == '_' && i > 0 && i < len(rest)-1 || r >= '0' && r <= '9' && i > 0
			if !ok {
				t.Errorf("%s = %q is not snake_case", name, v)
				break
			}
		}
		if seen[v] {
			t.Errorf("%s = %q twice", name, v)
		}
		seen[v] = true
	}
	for _, c := range []media.ErrorCode{media.CodePermissionDenied, media.CodeEncoderLost, media.CodeSourceGone,
		media.CodeUnsupportedOS, media.CodeNoEncoder, media.CodeDependency, media.CodeAudioUnavailable,
		media.CodeAudioNotRunning, media.CodeStopped, media.CodeTimeout, media.CodeInternal, media.CodeInvalid,
		media.CodeBusy} {
		if !seen[codes.EnginePrefix+string(c)] {
			t.Errorf("no engine code for media's %q", c)
		}
	}
	if seen[codes.EnginePrefix+string(media.CodeCancelled)] {
		t.Error("engine.cancelled is a code, but it is never surfaced (03 §6.4)")
	}
}
