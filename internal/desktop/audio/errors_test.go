package audio

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/audiorules"
)

// TestSetAppDecisionErrors is 05 §14.2's error table for SetAppDecision: locked ids, invalid ids and decisions.
func TestSetAppDecisionErrors(t *testing.T) {
	s := New(newStub(), newStore(DefaultSettings()), testSelf, nil)
	defer func() { _ = s.Close() }()
	locked := []string{"isshoni", "exe:isshoni.exe"}
	for _, va := range audiorules.VoiceApps() {
		locked = append(locked, va.ID)
		for _, exe := range va.Windows {
			locked = append(locked, "exe:"+exe)
		}
	}
	for _, id := range locked {
		if err := s.SetAppDecision(id, DecisionInclude); !errors.Is(err, ErrLockedApp) {
			t.Errorf("SetAppDecision(%q): %v, want ErrLockedApp", id, err)
		}
	}
	invalid := []string{"", "kook.exe", "exe:", "exe:KOOK.exe", "exe:Kook.exe", "exe:.exe", "exe:a.ex", "exe:a.exe.bak",
		"exe:" + strings.Repeat("a", 252) + ".exe", "exe:a/b.exe", `exe:a\b.exe`, "exe:a:b.exe", "exe:a*b.exe",
		"exe:a?b.exe", `exe:a"b.exe`, "exe:a<b.exe", "exe:a>b.exe", "exe:a|b.exe", "exe:a\x01b.exe", "exe:a\x7fb.exe",
		"exe:a\u0085b.exe", "exe:\xff.exe", "spotify", "EXE:a.exe"}
	for _, id := range invalid {
		if err := s.SetAppDecision(id, DecisionInclude); !errors.Is(err, ErrInvalidAppID) {
			t.Errorf("SetAppDecision(%q): %v, want ErrInvalidAppID", id, err)
		}
	}
	valid := []string{"exe:a.exe", "exe:" + strings.Repeat("a", 251) + ".exe", "exe:battle.net launcher.exe",
		"exe:qq音乐.exe", "exe:kook.exe"}
	for _, id := range valid {
		if err := s.SetAppDecision(id, DecisionKeepOut); err != nil {
			t.Errorf("SetAppDecision(%q): %v", id, err)
		}
	}
	for _, d := range []Decision{"", "maybe", "Keep-Out", "exclude"} {
		if err := s.SetAppDecision("exe:kook.exe", d); !errors.Is(err, ErrInvalidDecision) {
			t.Errorf("SetAppDecision(%q): %v, want ErrInvalidDecision", d, err)
		}
	}
	// auto forgets.
	if err := s.SetAppDecision("exe:kook.exe", DecisionAuto); err != nil {
		t.Fatal(err)
	}
	for _, r := range s.AppRules() {
		if r.ID == "exe:kook.exe" {
			t.Error("auto kept the decision")
		}
	}
}

// TestDecideAndTipErrors: Decide refuses every decision but keep-out and include, Decide and DismissPrompt an unknown
// prompt, DismissTip an unknown tip.
func TestDecideAndTipErrors(t *testing.T) {
	st := newStore(DefaultSettings())
	s := New(newStub(), st, testSelf, nil)
	defer func() { _ = s.Close() }()
	for _, d := range []Decision{DecisionAuto, "", "maybe"} {
		if err := s.Decide("p1", d); !errors.Is(err, ErrInvalidDecision) {
			t.Errorf("Decide(%q): %v, want ErrInvalidDecision", d, err)
		}
	}
	for _, d := range []Decision{DecisionKeepOut, DecisionInclude} {
		if err := s.Decide("p1", d); !errors.Is(err, ErrUnknownPrompt) {
			t.Errorf("Decide(%q): %v, want ErrUnknownPrompt", d, err)
		}
	}
	if err := s.DismissPrompt("p1"); !errors.Is(err, ErrUnknownPrompt) {
		t.Errorf("DismissPrompt: %v", err)
	}
	for _, code := range []string{"", "Ducking", "volume"} {
		if err := s.DismissTip(code); !errors.Is(err, ErrUnknownTip) {
			t.Errorf("DismissTip(%q): %v, want ErrUnknownTip", code, err)
		}
	}
	if err := s.DismissTip(TipDucking); err != nil {
		t.Fatal(err)
	}
	if err := s.DismissTip(TipDucking); err != nil {
		t.Fatal(err)
	}
	if saved, n := st.get(); n != 1 || len(saved.DismissedTips) != 1 || saved.DismissedTips[0] != TipDucking {
		t.Errorf("saved %+v (%d saves)", saved.DismissedTips, n)
	}
}

// TestStoreAndEngineErrors: the settings store's and the engine's errors come back wrapped, and a failed save
// changes nothing.
func TestStoreAndEngineErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		storeErr := errors.New("disk full")
		st := newStore(DefaultSettings())
		st.fail = storeErr
		ctl := newStub()
		kook := app("kook.exe", "KOOK", verdictKeptOut, reasonMic)
		ctl.set(func(r *media.AudioReport) { r.Apps = []media.AudioApp{kook} })
		s := New(ctl, st, testSelf, nil)
		stop := runService(t, s)
		defer stop()
		_ = s.Watch(true)
		settle(time.Second)
		for name, err := range map[string]error{
			"SetAppDecision":     s.SetAppDecision("exe:a.exe", DecisionKeepOut),
			"SetKeepOutMicUsers": s.SetKeepOutMicUsers(false),
			"DismissTip":         s.DismissTip(TipDucking),
			"Decide":             s.Decide("p1", DecisionKeepOut),
		} {
			if !errors.Is(err, storeErr) {
				t.Errorf("%s: %v, want the store's error", name, err)
			}
		}
		if fh := s.FriendsHear(); len(fh.Prompts) != 1 || !fh.KeepOutMicUsers || len(s.AppRules()) != 0 {
			t.Errorf("a failed save changed the state: %+v", fh)
		}

		// The engine's errors, wrapped.
		ctl.fail("watch", &media.Error{Code: media.CodeInvalid, Op: "audio_watch", Detail: "rule 3: empty value"})
		_ = s.Watch(false)
		if err := s.Watch(true); !errors.Is(err, media.ErrInvalid) {
			t.Errorf("Watch: %v", err)
		}
		ctl.fail("watch", nil)
		_ = s.Watch(true)
		s.OnShareStarted(0, true)
		ctl.fail("mode", &media.Error{Code: media.CodeInternal, Op: "audio_set_mode"})
		if err := s.SetShareSound(false); err == nil || !errors.Is(err, &media.Error{Code: media.CodeInternal}) {
			t.Errorf("SetShareSound: %v", err)
		}
		if fh := s.FriendsHear(); fh.Mode != ModeExcludeApps {
			t.Errorf("a failed SetShareSound changed the mode: %q", fh.Mode)
		}
		ctl.fail("soundcheck", &media.Error{Code: media.CodeTimeout, Op: "audio_sound_check"})
		if _, err := s.SoundCheck(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("SoundCheck: %v", err)
		}
		s.OnShareStopped()

		// After Close.
		stop()
		if err := s.Watch(true); !errors.Is(err, ErrClosed) {
			t.Errorf("Watch after Close: %v", err)
		}
		if _, err := s.SoundCheck(t.Context()); !errors.Is(err, ErrClosed) {
			t.Errorf("SoundCheck after Close: %v", err)
		}
		if err := s.SetShareSound(true); !errors.Is(err, ErrClosed) {
			t.Errorf("SetShareSound after Close: %v", err)
		}
		if err := s.Watch(false); err != nil {
			t.Errorf("Watch(false) after Close: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Errorf("Close twice: %v", err)
		}
	})
}
