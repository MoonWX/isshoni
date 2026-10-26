package bridge

import (
	"context"
	"log/slog"
	"sync"

	"github.com/MoonWX/isshoni/internal/desktop/audio"
	"github.com/MoonWX/isshoni/internal/desktop/bridge/codes"
	"github.com/MoonWX/isshoni/internal/media"
)

// Audio is the bound Audio service (03 §6.2): "What friends hear" over 05's policy service, one to one, plus the
// reference counts of the pages that watch (03 §6.2: the share sheet, the first-share sheet, the wizard's voice step
// and Settings → Audio call Watch on mount and unmount; level meters call WatchLevels). There is no sound-mode
// method: M2 has no "All sound" mode (README §3 item 15), and sound is switched per share by Share.SetAudioEnabled.
//
// main hands it 05's service with README WA77; until then every method is not_implemented (the counts still work).
type Audio struct {
	core   Core
	policy AudioPolicy // nil before WA77
	log    *slog.Logger

	mu     sync.Mutex
	watch  int // Audio.Watch's count
	levels int // Audio.WatchLevels' count
}

// FriendsHear is 05's "What friends hear" (05 §14.5): Audio.FriendsHear() and the audio.report event.
type FriendsHear = audio.FriendsHear

// AppRule is one remembered per-app decision (05 §14.2).
type AppRule = audio.AppRule

// SoundCheckResult is 01's media.SoundCheckResult: captured, levelDb, ms, backend and reason.
type SoundCheckResult = media.SoundCheckResult

// FriendsHear returns the current "What friends hear"; the audio.report event pushes its changes.
func (s *Audio) FriendsHear() (FriendsHear, error) {
	if s.policy == nil {
		return FriendsHear{}, notImplemented("Audio.FriendsHear")
	}
	return s.policy.FriendsHear(), nil
}

// Watch counts the pages that show the app list: 05's watcher runs while the count is above 0 (05 §14.2).
// Unbalanced offs never take it below 0.
func (s *Audio) Watch(on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.watch
	s.watch = count(s.watch, on)
	if s.policy == nil {
		return notImplemented("Audio.Watch")
	}
	if (prev > 0) == (s.watch > 0) {
		return nil
	}
	if err := s.policy.Watch(s.watch > 0); err != nil {
		s.watch = prev // the watcher didn't change: the next call tries again
		return mapError(err, s.log)
	}
	return nil
}

// WatchLevels counts the level meters on screen: 05 emits audio.levels at 10 Hz while the count is above 0.
func (s *Audio) WatchLevels(on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.levels
	s.levels = count(s.levels, on)
	if s.policy == nil {
		return notImplemented("Audio.WatchLevels")
	}
	if (prev > 0) != (s.levels > 0) {
		s.policy.WatchLevels(s.levels > 0)
	}
	return nil
}

// SetAppDecision sets an app's decision from a list row or Settings → Audio: keep-out, include, or auto (forget the
// saved one). invalid_argument for a locked row, a malformed app id or another decision.
func (s *Audio) SetAppDecision(appID, decision string) error {
	switch audio.Decision(decision) {
	case audio.DecisionKeepOut, audio.DecisionInclude, audio.DecisionAuto:
	default:
		return invalidArgument("decision")
	}
	if s.policy == nil {
		return notImplemented("Audio.SetAppDecision")
	}
	return mapError(s.policy.SetAppDecision(appID, audio.Decision(decision)), s.log)
}

// SetKeepOutMicUsers turns "Keep apps that use the microphone out" on or off.
func (s *Audio) SetKeepOutMicUsers(on bool) error {
	if s.policy == nil {
		return notImplemented("Audio.SetKeepOutMicUsers")
	}
	return mapError(s.policy.SetKeepOutMicUsers(on), s.log)
}

// Decide answers a mic prompt with keep-out or include, saved per app (05 §14.6). invalid_argument
// (params.field: decision) for anything else, auto included (a dismissal is DismissPrompt); not_found when the
// prompt was already answered, dismissed or is gone.
func (s *Audio) Decide(promptID, decision string) error {
	switch audio.Decision(decision) {
	case audio.DecisionKeepOut, audio.DecisionInclude:
	default:
		return invalidArgument("decision")
	}
	if s.policy == nil {
		return notImplemented("Audio.Decide")
	}
	return mapError(s.policy.Decide(promptID, audio.Decision(decision)), s.log)
}

// DismissPrompt closes a mic prompt without an answer: the app stays kept out for this run and nothing is saved
// (05 §14.6). not_found (05's ErrUnknownPrompt).
func (s *Audio) DismissPrompt(promptID string) error {
	if s.policy == nil {
		return notImplemented("Audio.DismissPrompt")
	}
	return mapError(s.policy.DismissPrompt(promptID), s.log)
}

// DismissTip hides a tip for good (05's tips: ducking). invalid_argument for an unknown tip.
func (s *Audio) DismissTip(code string) error {
	if s.policy == nil {
		return notImplemented("Audio.DismissTip")
	}
	return mapError(s.policy.DismissTip(code), s.log)
}

// AppRules returns the remembered per-app decisions with names, for Settings → Audio.
func (s *Audio) AppRules() ([]AppRule, error) {
	if s.policy == nil {
		return nil, notImplemented("Audio.AppRules")
	}
	rules := s.policy.AppRules()
	if rules == nil {
		rules = []AppRule{}
	}
	return rules, nil
}

// SoundCheck runs the wizard's sound check (05 §11.3, about 3 s): busy while a share is live.
func (s *Audio) SoundCheck(ctx context.Context) (SoundCheckResult, error) {
	if _, live := s.core.Live(); live {
		return SoundCheckResult{}, newError(codes.CodeBusy, nil)
	}
	if s.policy == nil {
		return SoundCheckResult{}, notImplemented("Audio.SoundCheck")
	}
	ctx, cancel := withTimeout(ctx, ioTimeout)
	defer cancel()
	res, err := s.policy.SoundCheck(ctx)
	if err != nil {
		return SoundCheckResult{}, mapError(err, s.log)
	}
	return res, nil
}
