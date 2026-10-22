package native

import (
	"context"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// This file is 05's (docs/m2/05-windows-audio.md §13.2; README WA35 fills it): media.AudioController on *Engine over
// the caller, the single mode mapping (05 §13.1), buildExclusions, level and report decoding, the audio status table
// (IM_ERR_BAD_STATE from an audio call is CodeAudioNotRunning) and the im_test_* wrappers. README WA10 declares the
// surface (docs/m2/README.md §4, "Interfaces first"): until WA35, every call returns a not-implemented *media.Error.
// The C mirrors of the audio structs (imRule, imExclusions, imAudioLevel, imAudioLevels) are in abi.go, beside the
// other mirrors that layout_test.go checks (04 §12.1).

// WatchAudio starts the watcher without capture (im_audio_watch; the share sheet before going live). Not implemented
// yet (README WA35).
func (e *Engine) WatchAudio(ctx context.Context, ex media.Exclusions) error {
	return notImplemented("audio_watch")
}

// UnwatchAudio stops the watcher (im_audio_unwatch). Not implemented yet (README WA35).
func (e *Engine) UnwatchAudio(ctx context.Context) error {
	return notImplemented("audio_unwatch")
}

// SetExclusions replaces the rules of the watcher and of a running share (im_set_exclusions). Not implemented yet
// (README WA35).
func (e *Engine) SetExclusions(ctx context.Context, ex media.Exclusions) error {
	return notImplemented("set_exclusions")
}

// SetAudioMode switches a live share's sound off or on (im_audio_set_mode; AudioEverything and AudioTestTone are
// CodeInvalid without a call, README §3 item 15). Not implemented yet (README WA35).
func (e *Engine) SetAudioMode(ctx context.Context, m media.AudioMode) error {
	return notImplemented("audio_set_mode")
}

// SetAudioMuted is the pause mute 01's core sets (im_audio_set_muted). Not implemented yet (README WA35).
func (e *Engine) SetAudioMuted(ctx context.Context, muted bool) error {
	return notImplemented("audio_set_muted")
}

// AudioReport is "What friends hear" (im_list_audio_apps, 05 §12.1). Not implemented yet (README WA35).
func (e *Engine) AudioReport(ctx context.Context) (media.AudioReport, error) {
	return media.AudioReport{}, notImplemented("list_audio_apps")
}

// AudioLevels is im_audio_levels_get (05 §12.2). Not implemented yet (README WA35).
func (e *Engine) AudioLevels(ctx context.Context) (media.AudioLevels, error) {
	return media.AudioLevels{}, notImplemented("audio_levels_get")
}

// SoundCheck is the first-run sound check (im_audio_sound_check, 05 §11.3). Not implemented yet (README WA35).
func (e *Engine) SoundCheck(ctx context.Context, helperPID uint32,
	timeout time.Duration) (media.SoundCheckResult, error) {
	return media.SoundCheckResult{}, notImplemented("audio_sound_check")
}

// AudioApps returns the apps of 05's report (04 §12.2). Not implemented yet (README WA35).
func (e *Engine) AudioApps(ctx context.Context) ([]media.AudioApp, error) {
	return nil, notImplemented("audio_apps")
}

// AudioTest is the shipped im_test_* exports of an engine (05 §13.2), for cmd/isshoni-audiotest's driver only: they
// render or decode test signals and capture nothing.
type AudioTest struct {
	e *Engine
}

// AudioTestHooks returns the im_test_* exports of the engine native.Open returned; for any other media.Engine an
// error wrapping media.ErrUnsupportedOS (05 §13.2).
func AudioTestHooks(e media.Engine) (*AudioTest, error) {
	ne, ok := e.(*Engine)
	if !ok || ne == nil {
		return nil, &media.Error{Code: media.CodeUnsupportedOS, Op: "audio_test_hooks",
			Detail: "not an engine from native.Open"}
	}
	return &AudioTest{e: ne}, nil
}

// RenderTone is im_test_render_tone. Not implemented yet (README WA35).
func (t *AudioTest) RenderTone(endpointID string, hz, amp float32, d time.Duration) error {
	return notImplemented("test_render_tone")
}

// RenderBursts is im_test_render_bursts: the predicted onset of each burst on the engine clock. Not implemented yet
// (README WA35).
func (t *AudioTest) RenderBursts(endpointID string, hz, amp float32, burst, period time.Duration,
	n int) (onsetEngineNS []int64, err error) {
	return nil, notImplemented("test_render_bursts")
}

// OpusDecode is im_test_opus_decode: pcm is interleaved stereo with len ≥ 2×5760. Not implemented yet
// (README WA35).
func (t *AudioTest) OpusDecode(pkt []byte, pcm []float32) (frames int, err error) {
	return 0, notImplemented("test_opus_decode")
}

// EngineClockNS is im_clock().mono_ns, the engine clock that RenderBursts' onsets use.
func (t *AudioTest) EngineClockNS() (int64, error) {
	s, err := t.e.Clock()
	return s.MonoNS, err
}
