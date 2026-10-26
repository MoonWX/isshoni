package audio

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// This file holds the methods that 03's bridge.Audio wraps one to one for the SPA (05 §14.2, 03 §6.2).

// FriendsHear returns "What friends hear" as of the last engine report and the service's state (05 §14.5). Before a
// share it lists what plays without capturing it; with nothing watched it is the last known state.
func (s *Service) FriendsHear() FriendsHear {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneFriendsHear(s.fh)
}

// AppRules returns the remembered decisions, including those of apps that aren't running, sorted by name (03's
// Settings → Audio). A name is the display name seen in this run, else the exe name.
func (s *Service) AppRules() []AppRule {
	s.mu.Lock()
	rules := make([]AppRule, 0, len(s.settings.AppRules))
	for id, d := range s.settings.AppRules {
		name := s.names[id]
		if name == "" {
			name, _ = exeOf(id)
		}
		rules = append(rules, AppRule{ID: id, Name: name, Decision: d})
	}
	s.mu.Unlock()
	slices.SortFunc(rules, func(a, b AppRule) int {
		return cmp.Or(strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), strings.Compare(a.ID, b.ID))
	})
	return rules
}

// Watch starts (true) or stops (false) watching for the share sheet, the first-share sheet, the wizard's voice step
// and Settings → Audio; 03's bridge counts its callers and passes count > 0. Watch(true) starts the engine's watcher
// (WatchAudio with the current rules) and polling; Watch(false) stops them (UnwatchAudio) unless a share is live.
// Errors are the engine's, wrapped; after Close, Watch(true) is ErrClosed.
func (s *Service) Watch(on bool) error {
	s.mu.Lock()
	if on && s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	s.sheet = on
	s.mu.Unlock()
	return s.syncWatcher()
}

// WatchLevels adds (true) or removes (false) one level meter on screen; the count never goes below 0. While it is
// above 0 and the watcher runs, the service polls every 100 ms and emits audio.levels at 10 Hz (05 §14.4).
func (s *Service) WatchLevels(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case on:
		s.levelsCount++
	case s.levelsCount > 0:
		s.levelsCount--
	}
	s.kickLocked()
}

// SetVisible records whether the app's window is visible (03 §4.4). 05 gives the window's visibility no effect on
// the policy in M2: hidden, the service keeps polling for what the SPA watches, and 03 decides which prompts become
// toasts (audio.prompt). It makes no engine call.
func (s *Service) SetVisible(visible bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.visible = visible
}

// Decide answers a mic prompt with keep-out or include (05 §14.6): the decision is saved in appRules (remembered per
// app) and the rules are rebuilt. ErrInvalidDecision for any other decision (auto included: a dismissal is
// DismissPrompt), ErrUnknownPrompt when no prompt with that id is pending, else the settings store's error, wrapped.
func (s *Service) Decide(promptID string, d Decision) error {
	if !validDecision(d) {
		return ErrInvalidDecision
	}
	defer s.deliver()
	s.setMu.Lock()
	defer s.setMu.Unlock()
	s.mu.Lock()
	i := slices.IndexFunc(s.prompts, func(p Prompt) bool { return p.ID == promptID })
	if i < 0 {
		s.mu.Unlock()
		return ErrUnknownPrompt
	}
	appID := s.prompts[i].AppID
	next := s.settings.withDecision(appID, d)
	s.mu.Unlock()
	if err := s.save(next); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings = next
	s.prompts = slices.DeleteFunc(s.prompts, func(p Prompt) bool { return p.AppID == appID })
	now := time.Now()
	s.markDirtyLocked(now)
	s.refreshLocked(now)
	return nil
}

// DismissPrompt closes a mic prompt without an answer (03's card close button, a dismissed toast): the app stays kept
// out, isn't asked about again in this run, and nothing is saved, so the next run asks again (05 §14.6).
// ErrUnknownPrompt when no prompt with that id is pending.
func (s *Service) DismissPrompt(promptID string) error {
	defer s.deliver()
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.prompts, func(p Prompt) bool { return p.ID == promptID })
	if i < 0 {
		return ErrUnknownPrompt
	}
	s.prompts = slices.Delete(s.prompts, i, i+1)
	s.refreshLocked(time.Now())
	return nil
}

// SetAppDecision changes an app's decision from 03's lists (a row's toggle, Settings → Audio): keep-out or include is
// saved in appRules, auto forgets the saved one; a pending prompt for the app is answered by it. "Include" on a row
// kept out for a viewer (reason viewer) is the per-run viewer override instead (05 §5.9 step 4): it removes that
// exe's viewer rules until the app quits, saves nothing, and mic keep-out still applies to the browser.
//
// ErrLockedApp for a built-in voice app's id or one of its exe ids, or isshoni's; ErrInvalidAppID for anything but
// "exe:" + a lower-case exe file name of 5–255 bytes ending in ".exe", without \ / : * ? " < > | or control
// characters; ErrInvalidDecision for anything but keep-out, include or auto; else the settings store's error,
// wrapped.
func (s *Service) SetAppDecision(appID string, d Decision) error {
	switch {
	case lockedApp(appID):
		return ErrLockedApp
	case !validAppID(appID):
		return ErrInvalidAppID
	case !validDecision(d) && d != DecisionAuto:
		return ErrInvalidDecision
	}
	defer s.deliver()
	s.setMu.Lock()
	defer s.setMu.Unlock()
	s.mu.Lock()
	if d == DecisionInclude && s.viewerRowLocked(appID) {
		exe, _ := exeOf(appID)
		s.overrides[exe] = true
		now := time.Now()
		s.markDirtyLocked(now)
		s.refreshLocked(now)
		s.mu.Unlock()
		return nil
	}
	if s.settings.decision(appID) == d {
		s.mu.Unlock()
		return nil
	}
	next := s.settings.withDecision(appID, d)
	s.mu.Unlock()
	if err := s.save(next); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings = next
	if d != DecisionAuto {
		s.prompts = slices.DeleteFunc(s.prompts, func(p Prompt) bool { return p.AppID == appID })
	}
	now := time.Now()
	s.markDirtyLocked(now)
	s.refreshLocked(now)
	return nil
}

// SetKeepOutMicUsers changes "Keep out apps that use my microphone" (05 §14.1). Off, there are no prompts and mic
// users are heard (with included-uses-mic). Errors are the settings store's, wrapped.
func (s *Service) SetKeepOutMicUsers(on bool) error {
	defer s.deliver()
	s.setMu.Lock()
	defer s.setMu.Unlock()
	s.mu.Lock()
	if s.settings.KeepOutMicUsers == on {
		s.mu.Unlock()
		return nil
	}
	next := s.settings.clone()
	next.KeepOutMicUsers = on
	s.mu.Unlock()
	if err := s.save(next); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings = next
	if !on {
		s.prompts = nil
	}
	now := time.Now()
	s.markDirtyLocked(now)
	s.refreshLocked(now)
	return nil
}

// DismissTip dismisses a tip for good (05 §14.7). ErrUnknownTip for anything but a tip code ("ducking"); else the
// settings store's error, wrapped.
func (s *Service) DismissTip(code string) error {
	if !knownTip(code) {
		return ErrUnknownTip
	}
	defer s.deliver()
	s.setMu.Lock()
	defer s.setMu.Unlock()
	s.mu.Lock()
	if slices.Contains(s.settings.DismissedTips, code) {
		s.mu.Unlock()
		return nil
	}
	next := s.settings.clone()
	next.DismissedTips = append(next.DismissedTips, code)
	s.mu.Unlock()
	if err := s.save(next); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings = next
	s.refreshLocked(time.Now())
	return nil
}

// save writes the settings through the store.
func (s *Service) save(next Settings) error {
	if err := s.st.SaveAudioSettings(next.clone()); err != nil {
		s.log.Warn("audio settings not saved", errAttr(err))
		return fmt.Errorf("audio: save the settings: %w", err)
	}
	return nil
}

// levelsOf is the audio.levels payload of the engine's levels: dB rounded to 0.01, non-finite values as silence, the
// first entry of an app id only.
func levelsOf(lv media.AudioLevels, seq uint64) Levels {
	l := Levels{Seq: seq, MixPeakDB: dB(lv.MixPeakDB), MixRMSDB: dB(lv.MixRMSDB), Apps: map[string]AppLevel{}}
	for _, a := range lv.Apps {
		if _, dup := l.Apps[a.AppID]; dup {
			continue
		}
		src := "activity"
		if a.Captured {
			src = "captured"
		}
		l.Apps[a.AppID] = AppLevel{PeakDB: dB(a.PeakDB), Source: src}
	}
	return l
}

// silenceDB is the level of silence in Levels (the engine's floor).
const silenceDB = -120

// dB rounds a level to 0.01 dB; NaN and infinities (which JSON can't carry) are silence.
func dB(v float32) float64 {
	f := float64(v)
	if math.IsNaN(f) || math.IsInf(f, 0) || f < silenceDB {
		return silenceDB
	}
	return math.Round(f*100) / 100
}
