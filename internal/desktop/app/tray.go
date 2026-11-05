package app

import (
	"context"
	"maps"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/desktop/share"
	"github.com/MoonWX/isshoni/internal/desktop/tray"
)

// The tray (03 §11.1): the app keeps internal/desktop/tray's State from the share controller's events and the
// other slices' inputs, renders it, and gives the view to the shell's TrayShell at most every 250 ms. The shell
// reports a left click (TrayClicked) and a chosen item (TrayAction).

// TrayDebounce is the tray's debounce: a burst of watcher changes rebuilds the menu once (03 §11.1).
const TrayDebounce = 250 * time.Millisecond

// trayActionTimeout bounds what a tray item asks of the share (a pause or a stop).
const trayActionTimeout = 10 * time.Second

// trayModel is the tray's inputs beside the share's view, and its debounce.
type trayModel struct {
	mu          sync.Mutex
	timer       stopper   // the debounce that is due; nil when none
	last        tray.View // the view the shell has
	drawn       bool      // last was given to the shell
	stopped     bool      // the app quits: nothing more is drawn
	updateReady string    // SetUpdateReady
	hotkeys     map[tray.HotkeyID]string
	voiceHeard  bool
	questions   int
}

// SetUpdateReady shows "Restart and update to <version>" in the Idle menu; "" takes it away (06's update.state
// ready; README WA84 calls it).
func (a *App) SetUpdateReady(version string) {
	a.tray.mu.Lock()
	a.tray.updateReady = version
	a.tray.mu.Unlock()
	a.scheduleTray()
}

// SetTrayHotkeys gives the menu the registered hotkeys' combinations, by id; nil or empty while none is registered
// (safety.go registers them while a share is live).
func (a *App) SetTrayHotkeys(combos map[HotkeyID]string) {
	m := make(map[tray.HotkeyID]string, len(combos))
	for id, c := range combos {
		m[tray.HotkeyID(id)] = c
	}
	a.tray.mu.Lock()
	a.tray.hotkeys = m
	a.tray.mu.Unlock()
	a.scheduleTray()
}

// SetAudioAlert gives the tray 05's FriendsHear.alert (05 §14.7 "Outside the window"): voiceAppsHeard and the
// number of pending mic prompts; the app calls it on every audio.report (audio.go).
func (a *App) SetAudioAlert(voiceAppsHeard bool, questions int) {
	a.tray.mu.Lock()
	a.tray.voiceHeard, a.tray.questions = voiceAppsHeard, max(questions, 0)
	a.tray.mu.Unlock()
	a.scheduleTray()
}

// trayState is the tray's State now: the share controller's view, the lock prompt, whether the share's server has
// share.pause, and the inputs above.
func (a *App) trayState() tray.State {
	v := a.shares.Current()
	noPause := a.clients.pauseUnavailable()
	a.mu.Lock()
	prompt := a.sys.prompt
	a.mu.Unlock()
	a.tray.mu.Lock()
	defer a.tray.mu.Unlock()
	s := tray.State{
		Mode:        trayMode(v),
		Hotkeys:     maps.Clone(a.tray.hotkeys),
		UpdateReady: a.tray.updateReady,
	}
	if s.Mode != tray.Idle {
		s.Source, s.Watching = v.Label, v.Watching
		s.LockPrompt = prompt && s.Mode == tray.Paused
		s.VoiceAppsHeard, s.Questions = a.tray.voiceHeard, a.tray.questions
		s.PauseUnavailable = noPause
	}
	return s
}

// trayMode is the tray's mode for the controller's view: a share being started counts as live (its Stop and Quit
// rows are what the user needs then), a share being stopped as idle.
func trayMode(v *share.View) tray.Mode {
	if v == nil {
		return tray.Idle
	}
	switch v.State {
	case share.StateStarting, share.StateLive:
		return tray.Live
	case share.StatePaused:
		return tray.Paused
	case share.StateReconnecting, share.StateUnreachable:
		return tray.Reconnecting
	}
	return tray.Idle
}

// scheduleTray redraws the tray once TrayDebounce has passed, unless a redraw is already due.
func (a *App) scheduleTray() {
	a.tray.mu.Lock()
	defer a.tray.mu.Unlock()
	if a.tray.timer != nil || a.tray.stopped {
		return
	}
	a.tray.timer = afterFunc(a.trayEvery, func() {
		a.tray.mu.Lock()
		a.tray.timer = nil
		a.tray.mu.Unlock()
		a.flushTray()
	})
}

// flushTray renders the tray and gives the view to the shell when it changed; the pill gets its state with it
// (safety.go), and the share's view its lock prompt (syncLockPrompt).
func (a *App) flushTray() {
	a.syncLockPrompt()
	a.flushPill()
	ts, ok := a.d.Shell.(TrayShell)
	if !ok {
		return
	}
	v := tray.Render(a.trayState(), a.d.I18n)
	a.tray.mu.Lock()
	if a.tray.stopped || (a.tray.drawn && a.tray.last.Equal(v)) {
		a.tray.mu.Unlock()
		return
	}
	a.tray.last, a.tray.drawn = v, true
	a.tray.mu.Unlock()
	ts.SetTray(v)
}

// TrayClicked is a left click on the tray icon: the window opens and takes the focus (03 §11.1).
func (a *App) TrayClicked() { a.ShowWindow() }

// TrayAction carries out a tray menu item (03 §11.1, §11.6). Pause and Resume go through the share controller as
// every control does (03 §11.3); Stop ends the share with the end reason tray; "Stop sharing and quit" and Quit quit
// with the share stopped first. "Restart and update" opens the window until README WA84 wires Updates.Install, and
// "N questions waiting" opens it on the mic prompt card. An unknown action does nothing.
func (a *App) TrayAction(act tray.Action) {
	switch act {
	case tray.ActionOpen, tray.ActionQuestions:
		a.ShowWindow()
	case tray.ActionPause:
		ctx, cancel := context.WithTimeout(a.ctx, trayActionTimeout)
		defer cancel()
		if err := a.shares.SetPaused(ctx, true); err != nil {
			a.log.Info("app: the tray's pause didn't pause", errAttr(err))
		}
	case tray.ActionResume:
		a.resume()
	case tray.ActionStop:
		ctx, cancel := context.WithTimeout(a.ctx, QuitTimeout)
		defer cancel()
		a.shares.Stop(ctx, share.CauseTray)
	case tray.ActionStopAndQuit, tray.ActionQuit:
		a.quit(true, share.CauseTray, true)
	case tray.ActionRestart:
		a.log.Info("app: Restart and update isn't wired yet; opening the window")
		a.ShowWindow()
	default:
		a.log.Debug("app: unknown tray action", "action", string(act))
	}
}

// syncLockPrompt gives the share controller the lock pause's "Resume sharing?" (03 §11.6), so the share's view
// (ShareView.lockPrompt) shows the panel's bar "Sharing is still paused since you locked your PC. [Resume sharing]
// [Stop]" while the tray and the pill ask too. Every change of the prompt schedules the tray, so it runs from
// flushTray, never under a.mu or from the controller's subscriber; the controller sends a share.state only when the
// value changed.
func (a *App) syncLockPrompt() {
	a.mu.Lock()
	prompt := a.sys.prompt
	a.mu.Unlock()
	a.shares.SetLockPrompt(prompt)
}
