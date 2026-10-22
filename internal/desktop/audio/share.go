package audio

import (
	"errors"
	"fmt"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// This file holds the methods that 03's App and share controller call (05 §14.2): the start config, the shared app,
// and the share's life, pause and Share sound.

// AudioConfig is the audio start config for a share with sound (core.Options.AudioConfig, 01): Mode
// media.AudioExcludeApps and the current rules with SharedPID. While the watcher runs or a share is live it first
// flushes the 50 ms debounce (a pending change reaches SetExclusions at once), so the engine and the start config
// agree. BitrateBPS stays 0: the core fills it from ShareParams.AudioBitrate.
func (s *Service) AudioConfig() media.AudioConfig {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	ex := s.exclusionsLocked()
	target := s.activeLocked()
	apply := target && !sameExclusions(ex, s.applied)
	if target {
		s.dirty = false
	}
	s.mu.Unlock()
	if apply {
		ctx, cancel := s.callCtx()
		err := s.eng.SetExclusions(ctx, ex)
		cancel()
		if err != nil {
			s.log.Warn("audio rules not applied", errAttr(err))
		} else {
			s.mu.Lock()
			s.applied = ex
			s.mu.Unlock()
		}
	}
	return media.AudioConfig{Mode: media.AudioExcludeApps, Exclusions: cloneExclusions(ex)}
}

// Exclusions is AudioConfig().Exclusions.
func (s *Service) Exclusions() media.Exclusions { return s.AudioConfig().Exclusions }

// SetSharedPID sets the process of the window being shared (Exclusions.SharedPID, 05 §5.8; 0 for a monitor or none):
// 03 calls it after Share.Pick with Source.PID, and with 0 on Release or pick expiry, so the first-share sheet shows
// the shared app first, with its warnings, before the share starts. The rules are rebuilt (debounced 50 ms).
func (s *Service) SetSharedPID(pid uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setSharedPIDLocked(pid, time.Now())
}

func (s *Service) setSharedPIDLocked(pid uint32, now time.Time) {
	if s.sharedPID != pid {
		s.sharedPID = pid
		s.markDirtyLocked(now)
	}
}

// OnShareStarted is called by 03's share controller right before core.StartShare (05 §14.2): it sets SharedPID, Share
// sound to withAudio (a share started without sound has no track to turn on) and not paused, keeps out again the
// browsers of the viewer tabs that still play (05 §5.9), and starts the watcher if the sheet hasn't. From here until
// OnShareStopped the share is live: polling and rule changes go on even if the watcher didn't start (the engine
// replaces a running share's rules, 05 §4.3), and FriendsHear.live, the sound-off banner, and audio.alert for an
// error banner (also one that shows as the share starts).
func (s *Service) OnShareStarted(sharedPID uint32, withAudio bool) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	now := time.Now()
	if !s.watcherOn {
		// syncWatcher below starts the watcher and then polls at once. A wake of Run before that (a rule change
		// kicks it) must not poll the engine, which watches nothing yet: the share would start with two polls.
		s.lastPoll = now
	}
	s.live, s.withAudio, s.soundOn, s.paused, s.modePending = true, withAudio, withAudio, false, false
	s.alerted = map[string]time.Time{}
	s.setSharedPIDLocked(sharedPID, now)
	s.restartViewersLocked(now)
	s.refreshLocked(now)
	s.mu.Unlock()
	s.deliver()
	s.log.Info("audio share started", "audio", withAudio)
	_ = s.syncWatcher() // logged; the share goes on without the watcher
	s.mu.Lock()
	s.kickLocked() // live polling starts, also when the watcher didn't
	s.mu.Unlock()
}

// OnShareStopped is called when the share ends, whoever ended it (03: when Share.Done() closes, and when StartShare
// fails): it drops the viewer rules (05 §5.9), sets SharedPID to 0 and stops the watcher unless Watch(true) holds
// it. The per-run viewer overrides stay until the app quits, and so do the tabs that play (SetViewerBrowser): a tab
// reports only when its playback starts or stops (03 §12.2), so the next share keeps out a browser whose tab still
// plays.
func (s *Service) OnShareStopped() {
	s.mu.Lock()
	now := time.Now()
	wasLive := s.live
	s.live, s.withAudio, s.soundOn, s.paused, s.modePending = false, false, false, false, false
	s.alerted = nil
	s.dropViewersLocked(now)
	s.setSharedPIDLocked(0, now)
	s.refreshLocked(now)
	s.mu.Unlock()
	s.deliver()
	if wasLive {
		s.log.Info("audio share stopped")
	}
	_ = s.syncWatcher()
	s.mu.Lock()
	s.kickLocked() // Run re-plans: polling and the rules continue only if the watcher still runs
	s.mu.Unlock()
}

// SetPaused records the share's pause (05 §14.2): 03 calls it on every pause and resume, including screen lock and a
// pause from another device. It is idempotent and makes no engine call (01's core pauses and mutes the session); it
// only updates FriendsHear at once (paused, and the sound-off banner with cause paused). Outside a share it does
// nothing.
func (s *Service) SetPaused(paused bool) {
	defer s.deliver()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.live || s.paused == paused {
		return
	}
	s.paused = paused
	s.refreshLocked(time.Now())
}

// SetShareSound is the per-share Share sound switch (05 §14.2, §6.5). During a live share with sound it calls
// SetAudioMode(AudioOff) for off and SetAudioMode(AudioExcludeApps) for on; pause and sound are separate engine
// flags, so friends hear silence while paused or switched off, and a resume keeps a switched-off share silent. When
// the share's audio session isn't running yet (ErrAudioNotRunning) the switch is applied with the first live report.
// During a share started without sound it only records the value (FriendsHear.mode stays off); outside a share it
// does nothing. Errors are the engine's, wrapped (the switch keeps its value); after Close, ErrClosed.
func (s *Service) SetShareSound(on bool) error {
	defer s.deliver()
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	switch {
	case s.closed:
		s.mu.Unlock()
		return ErrClosed
	case !s.live:
		s.mu.Unlock()
		return nil
	case !s.withAudio || (s.soundOn == on && !s.modePending):
		s.soundOn = on
		s.refreshLocked(time.Now())
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	ctx, cancel := s.callCtx()
	defer cancel()
	err := s.eng.SetAudioMode(ctx, modeFor(on))
	pending := errors.Is(err, media.ErrAudioNotRunning)
	if err != nil && !pending {
		s.log.Warn("audio sound mode not applied", errAttr(err))
		return fmt.Errorf("audio: share sound: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.soundOn, s.modePending = on, pending
	s.refreshLocked(time.Now())
	return nil
}
