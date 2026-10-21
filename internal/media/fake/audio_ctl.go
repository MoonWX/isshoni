package fake

import (
	"context"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// This file is 05's (docs/m2/05-windows-audio.md §13.3): *Engine's media.AudioController. Until slice WA26 brings the
// scripted scenarios (EngineConfig.AudioScenario), it is a stub: the watcher and the exclusions are no-ops (an
// SetExclusions made while a session runs is recorded in its Calls), SetAudioMode and SetAudioMuted follow 05 §13.1's
// rules (a running session with audio, the M2 modes) and SetAudioMuted silences the session's audio, and the report,
// the levels and the sound check return a not-implemented error.

var _ media.AudioController = (*Engine)(nil)

// WatchAudio implements media.AudioController. The stub does nothing.
func (e *Engine) WatchAudio(ctx context.Context, ex media.Exclusions) error {
	return ctx.Err()
}

// UnwatchAudio implements media.AudioController. The stub does nothing.
func (e *Engine) UnwatchAudio(ctx context.Context) error {
	return ctx.Err()
}

// SetExclusions implements media.AudioController. The stub records the call in the running session's Calls
// ("exclusions") and does nothing else.
func (e *Engine) SetExclusions(ctx context.Context, ex media.Exclusions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s := e.Session(); s != nil {
		s.recordExclusions()
	}
	return nil
}

// SetAudioMode implements media.AudioController: it needs a running session with audio (else ErrAudioNotRunning)
// and accepts only media.AudioOff and media.AudioExcludeApps (else CodeInvalid, 05 §13.1). The stub changes nothing
// else.
func (e *Engine) SetAudioMode(ctx context.Context, m media.AudioMode) error {
	const op = "audio_set_mode"
	if err := ctx.Err(); err != nil {
		return err
	}
	if s := e.Session(); s == nil || !s.liveAudio() {
		return &media.Error{Code: media.CodeAudioNotRunning, Op: op}
	}
	switch m {
	case media.AudioOff, media.AudioExcludeApps:
		return nil
	}
	return errorf(media.CodeInvalid, op, "", "audio mode %q", m)
}

// SetAudioMuted implements media.AudioController: it needs a running session with audio (else ErrAudioNotRunning);
// muted, that session's audio packets carry Opus silence instead of the tone until it is unmuted (01 §6.2), and the
// call is recorded in its Calls ("audio_muted", On = muted).
func (e *Engine) SetAudioMuted(ctx context.Context, muted bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s := e.Session()
	if s == nil {
		return &media.Error{Code: media.CodeAudioNotRunning, Op: "audio_set_muted"}
	}
	return s.setAudioMuted(muted)
}

// AudioReport implements media.AudioController. Not implemented until WA26: an internal error with Reason
// "not_implemented".
func (e *Engine) AudioReport(ctx context.Context) (media.AudioReport, error) {
	return media.AudioReport{}, notImplemented("list_audio_apps")
}

// AudioLevels implements media.AudioController. Not implemented until WA26.
func (e *Engine) AudioLevels(ctx context.Context) (media.AudioLevels, error) {
	return media.AudioLevels{}, notImplemented("audio_levels_get")
}

// SoundCheck implements media.AudioController. Not implemented until WA26.
func (e *Engine) SoundCheck(ctx context.Context, helperPID uint32, timeout time.Duration) (media.SoundCheckResult, error) {
	return media.SoundCheckResult{}, notImplemented("audio_sound_check")
}

// notImplemented is the error of a part that a later slice fills in, as the engine answers IM_ERR_NOT_IMPLEMENTED
// (01 §5.2).
func notImplemented(op string) *media.Error {
	return &media.Error{Code: media.CodeInternal, Op: op, Reason: "not_implemented"}
}
