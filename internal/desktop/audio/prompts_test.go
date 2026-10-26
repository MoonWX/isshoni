package audio

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// TestPrompts drives 05 §14.6 with the fake's normal scenario: Steam uses the mic from the start (a mic prompt),
// Chrome from 5 s (a browser-mic prompt). Unanswered prompts keep their app out; Decide saves the answer; DismissPrompt
// keeps the app out, never asks again in this run and saves nothing.
func TestPrompts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		eng := newFake("normal")
		defer func() { _ = eng.Close() }()
		st := newStore(DefaultSettings())
		s := New(eng, st, testSelf, nil)
		rec := record(s)
		stop := runService(t, s)
		defer stop()
		if err := s.Watch(true); err != nil {
			t.Fatal(err)
		}
		settle(time.Second)

		fh := s.FriendsHear()
		if len(fh.Prompts) != 1 || fh.Prompts[0].AppID != "exe:steam.exe" || fh.Prompts[0].Kind != PromptMic ||
			fh.Prompts[0].Name != "Steam" || fh.Prompts[0].Since != time.Now().Add(-time.Second).UnixMilli() ||
			fh.Alert.Questions != 1 {
			t.Fatalf("prompts %+v, alert %+v", fh.Prompts, fh.Alert)
		}
		steam := fh.Prompts[0]
		if r, g := rowByID(fh, "exe:steam.exe"); g != "keptOut" || r.Reason != reasonMic || r.Decision != DecisionAuto ||
			r.Control != ControlToggle {
			t.Errorf("Steam %s %+v", g, r)
		}
		evs := rec.take()
		prompts := named(evs, EventPrompt)
		if len(prompts) != 1 || prompts[0].Payload.(Prompt) != steam {
			t.Errorf("audio.prompt %+v", prompts)
		}
		// The audio.report that lists the prompt comes first, so FriendsHear() already shows it.
		if i, j := slices.IndexFunc(evs, func(e Event) bool {
			return e.Name == EventReport && len(e.Payload.(FriendsHear).Prompts) == 1
		}), slices.IndexFunc(evs, func(e Event) bool { return e.Name == EventPrompt }); i < 0 || i > j {
			t.Errorf("event order %v", evs)
		}

		// 5 s: Chrome, a browser, starts using the mic.
		settle(5 * time.Second)
		fh = s.FriendsHear()
		if len(fh.Prompts) != 2 || fh.Prompts[0] != steam || fh.Prompts[1].AppID != "exe:chrome.exe" ||
			fh.Prompts[1].Kind != PromptBrowserMic || fh.Alert.Questions != 2 {
			t.Fatalf("prompts %+v", fh.Prompts)
		}
		chrome := fh.Prompts[1]
		if r, g := rowByID(fh, "exe:chrome.exe"); g != "keptOut" || r.Reason != reasonMic || !r.Browser {
			t.Errorf("Chrome %s %+v", g, r)
		}

		// Decide: saved per app, the rules rebuilt, the prompt answered.
		if err := s.Decide(steam.ID, DecisionAuto); !errors.Is(err, ErrInvalidDecision) {
			t.Errorf("Decide(auto): %v", err)
		}
		if err := s.Decide(steam.ID, DecisionKeepOut); err != nil {
			t.Fatal(err)
		}
		if saved, n := st.get(); saved.AppRules["exe:steam.exe"] != DecisionKeepOut || n != 1 {
			t.Errorf("saved %+v (%d saves)", saved.AppRules, n)
		}
		if err := s.Decide(steam.ID, DecisionKeepOut); !errors.Is(err, ErrUnknownPrompt) {
			t.Errorf("Decide twice: %v", err)
		}
		settle(time.Second)
		fh = s.FriendsHear()
		if r, _ := rowByID(fh, "exe:steam.exe"); r.Reason != "user" || r.Decision != DecisionKeepOut ||
			len(fh.Prompts) != 1 || fh.Prompts[0] != chrome {
			t.Errorf("after Decide: Steam %+v, prompts %+v", r, fh.Prompts)
		}
		if ex := s.Exclusions(); !hasRule(ex, media.RuleAppExclude, media.SourceUser, "steam.exe") {
			t.Error("no user rule for Steam")
		}

		// DismissPrompt: still kept out, not asked again, nothing saved.
		if err := s.DismissPrompt(chrome.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.DismissPrompt(chrome.ID); !errors.Is(err, ErrUnknownPrompt) {
			t.Errorf("DismissPrompt twice: %v", err)
		}
		if err := s.Decide(chrome.ID, DecisionInclude); !errors.Is(err, ErrUnknownPrompt) {
			t.Errorf("Decide after the dismissal: %v", err)
		}
		settle(10 * time.Second)
		fh = s.FriendsHear()
		if r, g := rowByID(fh, "exe:chrome.exe"); g != "keptOut" || r.Reason != reasonMic || len(fh.Prompts) != 0 ||
			fh.Alert.Questions != 0 {
			t.Errorf("after the dismissal: Chrome %s %+v, prompts %+v", g, r, fh.Prompts)
		}
		if saved, n := st.get(); len(saved.AppRules) != 1 || n != 1 {
			t.Errorf("the dismissal saved %+v (%d saves)", saved.AppRules, n)
		}
		if n := len(named(rec.take(), EventPrompt)); n != 1 {
			t.Errorf("%d more audio.prompt", n)
		}

		// Include on Chrome's row: heard, with included-uses-mic.
		if err := s.SetAppDecision("exe:chrome.exe", DecisionInclude); err != nil {
			t.Fatal(err)
		}
		settle(time.Second)
		if r, g := rowByID(s.FriendsHear(), "exe:chrome.exe"); g != "heard" || r.Warning != WarningIncludedUsesMic ||
			r.Decision != DecisionInclude {
			t.Errorf("included Chrome %s %+v", g, r)
		}
		if err := s.DismissPrompt("p99"); !errors.Is(err, ErrUnknownPrompt) {
			t.Errorf("DismissPrompt(unknown): %v", err)
		}
		stop()
	})
}

// TestPromptsKeepOutMicUsersOff: with "Keep out apps that use my microphone" off there are no prompts (pending ones
// go at once) and mic users are heard with included-uses-mic.
func TestPromptsKeepOutMicUsersOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		eng := newFake("normal")
		defer func() { _ = eng.Close() }()
		st := newStore(DefaultSettings())
		s := New(eng, st, testSelf, nil)
		stop := runService(t, s)
		defer stop()
		_ = s.Watch(true)
		settle(time.Second)
		if n := len(s.FriendsHear().Prompts); n != 1 {
			t.Fatalf("%d prompts", n)
		}
		if err := s.SetKeepOutMicUsers(false); err != nil {
			t.Fatal(err)
		}
		if fh := s.FriendsHear(); len(fh.Prompts) != 0 || fh.KeepOutMicUsers {
			t.Errorf("right after: %+v", fh)
		}
		if saved, _ := st.get(); saved.KeepOutMicUsers {
			t.Error("not saved")
		}
		settle(6 * time.Second) // Chrome's mic at 5 s
		fh := s.FriendsHear()
		if r, g := rowByID(fh, "exe:chrome.exe"); g != "heard" || r.Warning != WarningIncludedUsesMic ||
			len(fh.Prompts) != 0 {
			t.Errorf("Chrome %s %+v, prompts %+v", g, r, fh.Prompts)
		}
		if ex := s.Exclusions(); ex.KeepOutMicUsers {
			t.Error("the flag is still set")
		}
		stop()
	})
}

// TestPromptsOncePerRun: a prompt appears once per app id per run (also after its app went and came back), and a new
// run (a new Service) asks again, since a dismissal saved nothing. A pending prompt whose app is no longer kept out
// for its mic is gone. FriendsHear lists at most 3 prompts, oldest first.
func TestPromptsOncePerRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctl := newStub()
		kook := app("kook.exe", "KOOK", verdictKeptOut, reasonMic)
		kook.UsesMic, kook.Playing = true, true
		ctl.set(func(r *media.AudioReport) { r.Apps = []media.AudioApp{kook} })
		st := newStore(DefaultSettings())
		s := New(ctl, st, testSelf, nil)
		stop := runService(t, s)
		defer stop()
		_ = s.Watch(true)
		settle(time.Second)
		fh := s.FriendsHear()
		if len(fh.Prompts) != 1 || fh.Prompts[0].ID != "p1" {
			t.Fatalf("prompts %+v", fh.Prompts)
		}
		_ = s.DismissPrompt("p1")
		ctl.set(func(r *media.AudioReport) { r.Apps = nil }) // KOOK quits …
		settle(time.Second)
		ctl.set(func(r *media.AudioReport) { r.Apps = []media.AudioApp{kook} }) // … and comes back
		settle(time.Second)
		if n := len(s.FriendsHear().Prompts); n != 0 {
			t.Errorf("%d prompts after KOOK came back", n)
		}
		stop()

		// A new run asks again.
		s = New(ctl, st, testSelf, nil)
		stop2 := runService(t, s)
		defer stop2()
		_ = s.Watch(true)
		settle(time.Second)
		if fh := s.FriendsHear(); len(fh.Prompts) != 1 || fh.Prompts[0].AppID != "exe:kook.exe" {
			t.Errorf("the new run's prompts %+v", fh.Prompts)
		}

		// Gone: the row is no longer kept out for its mic (here a rule from elsewhere), so the prompt goes.
		ctl.set(func(r *media.AudioReport) { r.Apps[0].Reason = "user" })
		settle(time.Second)
		if n := len(s.FriendsHear().Prompts); n != 0 {
			t.Errorf("%d prompts after the row changed", n)
		}
		if err := s.Decide("p1", DecisionKeepOut); !errors.Is(err, ErrUnknownPrompt) {
			t.Errorf("Decide(gone): %v", err)
		}

		// At most 3, oldest first; answering one shows the next.
		ctl.set(func(r *media.AudioReport) {
			r.Apps = nil
			for i := range 5 {
				a := app(fmt.Sprintf("voice%d.exe", i), fmt.Sprintf("Voice %d", i), verdictKeptOut, reasonMic)
				a.UsesMic = true
				r.Apps = append(r.Apps, a)
			}
		})
		settle(time.Second)
		fh = s.FriendsHear()
		if len(fh.Prompts) != 3 || fh.Alert.Questions != 3 || fh.Prompts[0].AppID != "exe:voice0.exe" ||
			fh.Prompts[2].AppID != "exe:voice2.exe" {
			t.Fatalf("prompts %+v, alert %+v", fh.Prompts, fh.Alert)
		}
		if err := s.Decide(fh.Prompts[0].ID, DecisionInclude); err != nil {
			t.Fatal(err)
		}
		if fh = s.FriendsHear(); len(fh.Prompts) != 3 || fh.Prompts[2].AppID != "exe:voice3.exe" {
			t.Errorf("after an answer: %+v", fh.Prompts)
		}
		stop2()
	})
}

// TestPromptsSkipLocked: no prompt for an id that can't be decided (a locked app, an id that is no exe id) or that
// already has a decision.
func TestPromptsSkipLocked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctl := newStub()
		discord := media.AudioApp{ID: "discord", Name: "Discord", Exe: "discord.exe", Verdict: verdictKeptOut,
			Reason: reasonMic, UsesMic: true}
		odd := media.AudioApp{ID: "someapp", Name: "Some app", Exe: "some.exe", Verdict: verdictKeptOut,
			Reason: reasonMic, UsesMic: true}
		decided := app("decided.exe", "Decided", verdictKeptOut, reasonMic)
		ctl.set(func(r *media.AudioReport) { r.Apps = []media.AudioApp{discord, odd, decided} })
		st := DefaultSettings().withDecision("exe:decided.exe", DecisionKeepOut)
		s := New(ctl, newStore(st), testSelf, nil)
		stop := runService(t, s)
		defer stop()
		_ = s.Watch(true)
		settle(time.Second)
		if n := len(s.FriendsHear().Prompts); n != 0 {
			t.Errorf("%d prompts", n)
		}
		stop()
	})
}
