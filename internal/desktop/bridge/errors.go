package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"

	"github.com/MoonWX/isshoni/internal/client/compat"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/audio"
	"github.com/MoonWX/isshoni/internal/desktop/bridge/codes"
	"github.com/MoonWX/isshoni/internal/desktop/servers"
	"github.com/MoonWX/isshoni/internal/desktop/settings"
	"github.com/MoonWX/isshoni/internal/desktop/share"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// Error is a bound method's error (03 §6.4): its Error() is the JSON {"code":"…","params":{…}}, which the pinned
// Wails hands to the page verbatim as the rejection's message (W12), and which the SPA parses into BridgeError. Its
// MarshalJSON writes the same object, so Wails' error marshaling puts it in the rejection's cause too.
//
// Params hold codes and numbers only (03 §6.4: params.field, params.reason, params.retryAfter, …), never English, a
// token, a path or a name: the page may log them.
type Error struct {
	Code   codes.Code
	Params map[string]any
	cause  error // for the log line of an internal error only; never serialized
}

// errorJSON is Error's wire shape. params is always an object, so the SPA never reads a missing one.
type errorJSON struct {
	Code   codes.Code     `json:"code"`
	Params map[string]any `json:"params"`
}

// Error returns the JSON object.
func (e *Error) Error() string {
	b, err := e.MarshalJSON()
	if err != nil { // a parameter that doesn't marshal: keep the code
		b, _ = json.Marshal(errorJSON{Code: e.Code, Params: map[string]any{}})
	}
	return string(b)
}

// MarshalJSON writes {"code", "params"}.
func (e *Error) MarshalJSON() ([]byte, error) {
	p := e.Params
	if p == nil {
		p = map[string]any{}
	}
	return json.Marshal(errorJSON{Code: e.Code, Params: p})
}

// Unwrap returns the cause, if any.
func (e *Error) Unwrap() error { return e.cause }

// newError returns an *Error with code and params (nil: none).
func newError(code codes.Code, params map[string]any) *Error {
	return &Error{Code: code, Params: params}
}

// invalidArgument is invalid_argument for the named argument.
func invalidArgument(field string) *Error {
	return newError(codes.CodeInvalidArgument, map[string]any{"field": field})
}

// notImplemented is the error of a method whose slice hasn't landed (README §4 "Interfaces first"). method is the
// bound name, "Share.Pick".
func notImplemented(method string) *Error {
	return newError(codes.CodeNotImplemented, map[string]any{"method": method})
}

// compatParams are incompatible_server's params: 02 §13.2's compat.Check (03 §6.4).
func compatParams(c compat.Check) map[string]any {
	v := c.Verdict
	if v == "" {
		v = compat.UpdateServer // an incompatible verdict was never given: the server's side, as 02's default
	}
	return map[string]any{
		"verdict":          string(v),
		"reason":           string(c.Reason),
		"serverVersion":    c.ServerVersion,
		"minClientVersion": c.MinClientVersion,
	}
}

// mapError turns any Go error a service got into an *Error (03 §6.4); nil stays nil. log gets one WARN line for an
// error that no rule knows (it becomes internal), with the cause's text: the packages below never put a token, a
// password, a name or a path into an error's text.
//
// A deadline that ran out is internal with params.reason "timeout": a method's own bound (Audio.SoundCheck's 10 s, a
// local file) is no server's fault. Only Auth, whose deadlines bound calls to a server, maps it to server_unreachable
// (Auth.authError).
func mapError(err error, log *slog.Logger) error {
	if err == nil {
		return nil
	}
	var be *Error
	if errors.As(err, &be) {
		return be
	}
	if e := knownError(err); e != nil {
		return e
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return newError(codes.CodeInternal, map[string]any{"reason": "timeout"})
	}
	log.Warn("bridge: an unexpected error", "err", err)
	return &Error{Code: codes.CodeInternal, cause: err}
}

// isTimeout reports an error that only a deadline explains: context.DeadlineExceeded that no bridge error and no rule
// of knownError claims first (a server's own code wrapped around it keeps that code).
func isTimeout(err error) bool {
	var be *Error
	return errors.Is(err, context.DeadlineExceeded) && !errors.As(err, &be) && knownError(err) == nil
}

// knownError is mapError's table; nil for an error it doesn't know.
func knownError(err error) *Error {
	if e := shareError(err); e != nil { // first: the controller's errors may wrap a wire or media error
		return e
	}
	var (
		srvErr   *servers.Error
		apiErr   *api.Error
		wireErr  *protocol.Error
		mediaErr *media.Error
		fieldErr *settings.FieldError
	)
	switch {
	case errors.As(err, &srvErr):
		e := newError(codes.Code(srvErr.Code), nil)
		if srvErr.Code == servers.CodeIncompatibleServer && srvErr.Compat != nil {
			e.Params = compatParams(*srvErr.Compat)
		}
		return e
	case errors.As(err, &mediaErr):
		return engineError(mediaErr)
	case errors.As(err, &apiErr):
		return restError(apiErr)
	case errors.As(err, &wireErr):
		p := maps.Clone(wireErr.Params)
		if wireErr.RetryAfterMs > 0 {
			if p == nil {
				p = map[string]any{}
			}
			p["retryAfterMs"] = wireErr.RetryAfterMs
		}
		return newError(codes.Code(wireErr.Code), p)
	case errors.As(err, &fieldErr):
		return invalidArgument(fieldErr.Field)
	}
	switch {
	case errors.Is(err, app.ErrInvalidArgument):
		return newError(codes.CodeInvalidArgument, nil)
	case errors.Is(err, app.ErrBusy):
		return newError(codes.CodeBusy, nil)
	case errors.Is(err, app.ErrNotImplemented):
		return newError(codes.CodeNotImplemented, nil)

	case errors.Is(err, context.Canceled):
		// The page cancelled the call (W11): nobody reads the answer.
		return newError(codes.CodeInternal, map[string]any{"reason": "cancelled"})
	case errors.Is(err, deviceauth.ErrOffline), errors.Is(err, deviceauth.ErrNetwork),
		errors.Is(err, deviceauth.ErrBadResponse):
		return newError(codes.CodeServerUnreachable, nil)
	case errors.Is(err, deviceauth.ErrIncompatible):
		return newError(codes.CodeIncompatibleServer, compatParams(compat.Check{}))
	case errors.Is(err, deviceauth.ErrTooManyServers):
		return newError(codes.CodeTooManyServers, nil)
	case errors.Is(err, deviceauth.ErrNotLinked):
		return newError(codes.CodeNotLinked, nil)

	case errors.Is(err, audio.ErrUnknownPrompt):
		return newError(codes.CodeNotFound, nil)
	case errors.Is(err, audio.ErrInvalidAppID), errors.Is(err, audio.ErrLockedApp):
		return invalidArgument("appId")
	case errors.Is(err, audio.ErrInvalidDecision):
		return invalidArgument("decision")
	case errors.Is(err, audio.ErrUnknownTip):
		return invalidArgument("code")
	case errors.Is(err, settings.ErrInvalid):
		return newError(codes.CodeInvalidArgument, nil)
	}
	return nil
}

// shareError is the share controller's part of the table (03 §6.4, §10.5); nil for another error. The controller's
// own pick_expired carries params.reason pick_expired like the engine's (engine.source_gone with that reason), so
// the SPA's reason row (Pick again) matches both.
func shareError(err error) *Error {
	switch {
	case errors.Is(err, share.ErrPickExpired):
		return newError(codes.CodePickExpired, map[string]any{"reason": "pick_expired"})
	case errors.Is(err, share.ErrAlreadySharing):
		return newError(codes.CodeAlreadySharing, nil)
	case errors.Is(err, share.ErrNotSharing):
		return newError(codes.CodeNotSharing, nil)
	case errors.Is(err, share.ErrPublisherOffline):
		return newError(codes.CodePublisherOffline, nil)
	case errors.Is(err, share.ErrFeatureDisabled):
		return newError(codes.CodeFeatureDisabled, map[string]any{"feature": string(protocol.FeatureSharePause)})
	case errors.Is(err, share.ErrBusy):
		return newError(codes.CodeBusy, nil)
	case errors.Is(err, share.ErrInvalid):
		return newError(codes.CodeInvalidArgument, nil)
	}
	return nil
}

// engineError is "engine." + the media code with params.reason (03 §6.4). media's cancelled never reaches the page
// (a cancelled Pick is null): it maps to engine.internal here, where a caller forgot that rule.
func engineError(e *media.Error) *Error {
	code := e.Code
	if code == media.CodeCancelled || code == "" {
		code = media.CodeInternal
	}
	return newError(codes.Code(codes.EnginePrefix+string(code)), map[string]any{"reason": e.Reason})
}

// restError passes an M1 REST error through (03 §6.4): its code, params, retryAfter and validation_failed's fields.
func restError(e *api.Error) *Error {
	p := maps.Clone(e.Params)
	if p == nil {
		p = map[string]any{}
	}
	if e.RetryAfter > 0 {
		p["retryAfter"] = e.RetryAfter
	}
	if len(e.Fields) > 0 {
		p["fields"] = maps.Clone(e.Fields)
	}
	return newError(codes.Code(e.Code), p)
}
