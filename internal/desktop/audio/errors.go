package audio

import (
	"context"
	"errors"
	"log/slog"

	"github.com/MoonWX/isshoni/internal/media"
)

// The service's errors (05 §14.2). 03's bridge maps ErrUnknownPrompt and ErrUnknownTip to not_found, and the others
// to invalid_argument with params.field appId (ErrLockedApp, ErrInvalidAppID) or decision (ErrInvalidDecision).
// Every other error is the settings store's or the engine's, wrapped (errors.Is finds media's sentinels).
var (
	ErrUnknownPrompt   = errors.New("audio: unknown prompt")   // Decide, DismissPrompt: answered, dismissed or gone
	ErrLockedApp       = errors.New("audio: locked app")       // SetAppDecision: a built-in voice app or isshoni
	ErrInvalidAppID    = errors.New("audio: invalid app id")   // SetAppDecision: not "exe:<lower-case exe name>"
	ErrInvalidDecision = errors.New("audio: invalid decision") // Decide, SetAppDecision
	ErrUnknownTip      = errors.New("audio: unknown tip")      // DismissTip
	// ErrClosed is the error of a call that needs the engine after Close.
	ErrClosed = errors.New("audio: service closed")
)

// errAttr is the log attribute of err, without anything that could name an app, a path or a pid: for a media error
// its code, op and reason (never its detail), for a context error its text, and "error" for anything else (a
// settings store's or an exec error can carry a path with the user's name, 05 §16).
func errAttr(err error) slog.Attr {
	var me *media.Error
	switch {
	case errors.As(err, &me):
		s := string(me.Code)
		if me.Op != "" {
			s = me.Op + ": " + s
		}
		if me.Reason != "" {
			s += " (" + me.Reason + ")"
		}
		return slog.String("err", s)
	case errors.Is(err, context.Canceled):
		return slog.String("err", context.Canceled.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return slog.String("err", context.DeadlineExceeded.Error())
	case errors.Is(err, ErrClosed):
		return slog.String("err", ErrClosed.Error())
	}
	return slog.String("err", "error")
}
