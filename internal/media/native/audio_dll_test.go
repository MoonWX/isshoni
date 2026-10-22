//go:build windows

package native

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// The audio controller against a real engine (05 §19.2, README WA35): the win-x64-ci DLL that ISSHONI_TEST_ENGINE_DLL
// names, with and without IM_INIT_NO_AUDIO. They need no audio device: on a machine without one the watcher reports
// no-device. Without the DLL they skip.

// openAudioDLL is openDLL with audio on.
func openAudioDLL(t *testing.T) *Engine {
	t.Helper()
	dir := engineDLLDir(t)
	e, err := Open(Options{Dir: dir, LogLevel: slog.LevelDebug,
		Logger: slog.New(slog.NewTextHandler(testLog{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

// dllTestRules is a share sheet's kind of rule set plus a built-in rule on a process every Windows runs, so the
// report has a kept-out row without any audio session.
func dllTestRules() media.Exclusions {
	return media.Exclusions{KeepOutMicUsers: true, FastPath: true, Rules: []media.Rule{
		{Kind: media.RuleAppExclude, Source: media.SourceBuiltin, Value: "svchost.exe", AppID: "test-voice",
			Name: "Test Voice"},
		{Kind: media.RuleStop, Source: media.SourceData, Value: "explorer.exe"},
		{Kind: media.RuleSharedRuntime, Source: media.SourceData, Value: "msedgewebview2.exe"},
	}}
}

// TestDLLAudioWithoutAudio: after IM_INIT_NO_AUDIO the watcher's calls are media.ErrAudioNotRunning (05 §13.1's
// IM_ERR_BAD_STATE row); SetExclusions keeps the rules and UnwatchAudio does nothing.
func TestDLLAudioWithoutAudio(t *testing.T) {
	e := openDLL(t) // NoAudio
	ctx := context.Background()
	for op, err := range map[string]error{
		"audio_watch":       e.WatchAudio(ctx, dllTestRules()),
		"list_audio_apps":   second(e.AudioReport(ctx)),
		"audio_levels_get":  second(e.AudioLevels(ctx)),
		"audio_sound_check": second(e.SoundCheck(ctx, 0, time.Second)),
		"audio_set_mode":    e.SetAudioMode(ctx, media.AudioOff),
		"audio_set_muted":   e.SetAudioMuted(ctx, true),
	} {
		if !errors.Is(err, media.ErrAudioNotRunning) {
			t.Errorf("%s: %v, want ErrAudioNotRunning", op, err)
		}
	}
	if err := e.SetExclusions(ctx, dllTestRules()); err != nil {
		t.Errorf("SetExclusions: %v", err)
	}
	if err := e.UnwatchAudio(ctx); err != nil {
		t.Errorf("UnwatchAudio: %v", err)
	}
}

// TestDLLAudioWatch: the watcher's life from Go: a one-shot report before watching, then the watcher's report with
// the built-in row and the levels, the modes refused without a live share, and the levels refused again after
// unwatching.
func TestDLLAudioWatch(t *testing.T) {
	e := openAudioDLL(t)
	ctx := context.Background()
	if _, err := e.AudioLevels(ctx); !errors.Is(err, media.ErrAudioNotRunning) {
		t.Errorf("AudioLevels before WatchAudio: %v", err)
	}
	r, err := e.AudioReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != "stopped" || r.Engine != "none" || r.Live {
		t.Errorf("one-shot report: state %q, engine %q, live %v", r.State, r.Engine, r.Live)
	}

	if err := e.WatchAudio(ctx, dllTestRules()); err != nil {
		t.Fatal(err)
	}
	r, err = e.AudioReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if (r.State != "ok" && r.State != "no-device") || r.Engine != "none" || r.Live || r.Mode != media.AudioExcludeApps {
		t.Errorf("watching: state %q, engine %q, live %v, mode %q", r.State, r.Engine, r.Live, r.Mode)
	}
	if r.Device.Present != (r.State == "ok") || r.Device.Present != (r.Device.RenderEndpoints > 0) {
		t.Errorf("state %q with device %+v", r.State, r.Device)
	}
	found := false
	for _, a := range r.Apps {
		if a.ID == "test-voice" {
			found = true
			if a.Verdict != "kept_out" || a.Reason != "builtin" || a.Name != "Test Voice" || a.PIDs < 1 {
				t.Errorf("the built-in row %+v", a)
			}
		}
	}
	if !found {
		t.Error("no row for a running process matched by a built-in rule (05 §12.1)")
	}
	lv, err := e.AudioLevels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The levels carry the seq of the report of their sample (every 100 ms, so they may lag a newer report).
	r2, err := e.AudioReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if lv.ReportSeq < 1 || lv.ReportSeq > r2.Seq || lv.MixPeakDB != -120 || len(lv.Apps) > imMaxLevelApps {
		t.Errorf("levels %+v against report seq %d", lv, r2.Seq)
	}
	for _, a := range lv.Apps {
		if a.Captured {
			t.Errorf("a captured level %+v while not live", a)
		}
	}
	if err := e.SetAudioMode(ctx, media.AudioOff); !errors.Is(err, media.ErrAudioNotRunning) {
		t.Errorf("SetAudioMode without a share: %v", err)
	}
	if err := e.SetAudioMode(ctx, media.AudioEverything); !errors.Is(err, media.ErrInvalid) {
		t.Errorf("SetAudioMode(everything): %v", err)
	}
	if err := e.UnwatchAudio(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AudioLevels(ctx); !errors.Is(err, media.ErrAudioNotRunning) {
		t.Errorf("AudioLevels after UnwatchAudio: %v", err)
	}
}
