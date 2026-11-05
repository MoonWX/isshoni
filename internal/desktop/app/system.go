package app

import (
	"context"
	"errors"
	"time"

	"github.com/MoonWX/isshoni/internal/desktop/share"
)

// What the system does (03 §11.6): the OS's session and power events (winos' thread on Windows), keep-awake while a
// share runs, the lock pause with its two bounds, and the heartbeat watchdog of W17.

// The bounds of 03 §11.6 and §20.
const (
	// LockResume: an unlock within this long of the lock resumes a share the lock paused.
	LockResume = 5 * time.Minute
	// LockStop: a lock pause that lasts this long (counted from the lock, locked or not, until any resume) stops
	// the share with the end reason system.
	LockStop = 30 * time.Minute
	// SystemStopTimeout bounds the stop of a share at a suspend and at the end of the session: Windows gives an
	// app about 2 s (03 §11.6's end-of-session row).
	SystemStopTimeout = 2 * time.Second
	// pauseTimeout bounds a lock's pause and an unlock's resume.
	pauseTimeout = 10 * time.Second
	// EndSessionQuitTimeout bounds the wait at WM_ENDSESSION for the app's parts (C1 and C2 closing with 1000) once
	// the quit's own steps ran; the OS hooks wait a little longer for the acknowledgement (winos.EndSessionQuitWait).
	EndSessionQuitTimeout = 2500 * time.Millisecond
)

// The watchdog of W17's fallback (03 §3.2, §20): the SPA calls App.Heartbeat every HeartbeatEvery; a visible window
// without one for HeartbeatTimeout is reloaded.
const (
	HeartbeatEvery   = 5 * time.Second
	HeartbeatTimeout = 15 * time.Second
)

// stopper is a timer that can be stopped (*time.Timer).
type stopper interface{ Stop() bool }

func afterFunc(d time.Duration, f func()) stopper { return time.AfterFunc(d, f) }

// systemState is the app's part of 03 §11.6, guarded by App.mu.
type systemState struct {
	locked bool // between a lock and the next unlock
	// lockPaused: the share was paused by a lock (never one the user paused) and nothing has resumed it.
	lockPaused bool
	lockedAt   time.Time // the lock that paused it
	prompt     bool      // an unlock came later than LockResume: "Resume sharing?" waits for an answer
	gen        int       // the lock pause's number, so a stale timer does nothing
	stopTimer  stopper   // LockStop after lockedAt
	// suspendStopped: a suspend stopped the share; the resume shows the toast.
	suspendStopped bool
	closeToastSent bool // window.go: the close-to-tray toast of this run
}

// watchdog is W17's heartbeat state, guarded by App.mu.
type watchdog struct {
	last       time.Time // the last beat, or a grace that began then
	ever       bool      // the page has beaten since the start
	wasVisible bool
}

// endSessionAcker is the OS hooks' end-of-session handshake (winos): WM_QUERYENDSESSION waits, at most 2 s, until the
// app has stopped its share; WM_ENDSESSION waits, at most about 3 s, until the app has quit.
type endSessionAcker interface {
	EndSessionHandled()
}

// systemEvents handles the OS's events until the app ends.
func (a *App) systemEvents(ch <-chan SystemEvent) {
	for {
		select {
		case <-a.ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			a.handleSystemEvent(ev)
		}
	}
}

// handleSystemEvent is 03 §11.6's table for one event.
func (a *App) handleSystemEvent(ev SystemEvent) {
	a.log.Info("app: system event", "event", string(ev))
	switch ev {
	case EventLock:
		a.onLock()
	case EventUnlock:
		a.onUnlock()
	case EventSuspend:
		a.onSuspend()
	case EventResume:
		a.onResume()
	case EventEndSession:
		a.onEndSession()
	case EventEndSessionConfirmed:
		a.onEndSessionConfirmed()
	case EventDisplayChange:
		// The pill re-clamps its position (README WA80).
	}
}

// pausable reports a share a lock pauses: live, reconnecting or unreachable. A share the user paused stays paused
// and isn't a lock pause; one still starting isn't paused.
func pausable(v *share.View) bool {
	if v == nil {
		return false
	}
	switch v.State {
	case share.StateLive, share.StateReconnecting, share.StateUnreachable:
		return true
	}
	return false
}

// onLock pauses a live share through the controller (as every Pause does, 03 §11.3) and releases keep-awake, so
// the power plan applies again; a server without share.pause gets no pause (the share goes on, as in a browser).
func (a *App) onLock() {
	a.mu.Lock()
	a.sys.locked = true
	already := a.sys.lockPaused
	a.mu.Unlock()
	a.syncAwake()
	v := a.shares.Current()
	if already || !pausable(v) {
		a.scheduleTray()
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, pauseTimeout)
	err := a.shares.SetPaused(ctx, true)
	cancel()
	switch {
	case errors.Is(err, share.ErrFeatureDisabled):
		a.log.Info("app: the server has no share.pause; a lock doesn't pause the share")
	case err != nil:
		a.log.Warn("app: the lock didn't pause the share", errAttr(err))
	default:
		a.mu.Lock()
		if a.sys.stopTimer != nil {
			a.sys.stopTimer.Stop()
		}
		a.sys.gen++
		gen := a.sys.gen
		a.sys.lockPaused, a.sys.lockedAt, a.sys.prompt = true, a.clock(), false
		a.sys.stopTimer = a.after(a.d.LockStop, func() { a.lockStopReached(gen) })
		a.mu.Unlock()
	}
	a.syncAwake()
	a.scheduleTray()
}

// onUnlock resumes a share the lock paused when the unlock comes within LockResume of the lock; later, the share
// stays paused with "Resume sharing?" in the tray (and the panel and pill, README WA80) and a toast with Resume and
// Stop. Keep-awake comes back with a running share that isn't waiting for that answer.
func (a *App) onUnlock() {
	a.mu.Lock()
	a.sys.locked = false
	lockPaused, at, gen := a.sys.lockPaused, a.sys.lockedAt, a.sys.gen
	a.mu.Unlock()
	if lockPaused {
		if a.clock().Sub(at) <= a.d.LockResume {
			ctx, cancel := context.WithTimeout(a.ctx, pauseTimeout)
			err := a.shares.SetPaused(ctx, false)
			cancel()
			if err == nil {
				a.endLockPause(gen)
			} else {
				a.log.Warn("app: the unlock didn't resume the share", errAttr(err))
				a.askResume(gen)
			}
		} else {
			a.askResume(gen)
		}
	}
	a.syncAwake()
	a.scheduleTray()
}

// askResume is the late unlock's question: the tray's Paused menu with Resume first, and the toast.
func (a *App) askResume(gen int) {
	a.mu.Lock()
	if !a.sys.lockPaused || a.sys.gen != gen {
		a.mu.Unlock()
		return
	}
	a.sys.prompt = true
	a.mu.Unlock()
	t := a.d.I18n
	a.notify(ToastLockPaused, t.T("desktop.native.toast.lockPaused"),
		NotificationAction{ID: ToastResume, Title: t.T("desktop.native.toast.resume")},
		NotificationAction{ID: ToastStop, Title: t.T("desktop.native.toast.stop")})
	a.mu.Lock()
	if !a.sys.lockPaused || a.sys.gen != gen { // answered while the toast went out
		a.toasts.dropKind(ToastLockPaused)
	}
	a.mu.Unlock()
	a.removeStaleToasts()
}

// endLockPause ends the lock pause gen (a resume from anywhere).
func (a *App) endLockPause(gen int) {
	a.mu.Lock()
	if a.sys.gen == gen {
		a.clearLockLocked()
	}
	a.mu.Unlock()
	a.removeStaleToasts()
}

// clearLockLocked forgets the lock pause; a.mu is held. Its question is answered: a "Resume sharing?" toast still
// in Action Center no longer acts (03 §15: its buttons are for that question only) and is marked stale; the caller
// takes it out of Action Center with removeStaleToasts (or the next notify) once a.mu is released.
func (a *App) clearLockLocked() {
	if a.sys.stopTimer != nil {
		a.sys.stopTimer.Stop()
		a.sys.stopTimer = nil
	}
	a.sys.lockPaused, a.sys.prompt = false, false
	a.toasts.dropKind(ToastLockPaused)
}

// lockStopReached is LockStop after the lock that paused the share: still paused by it, the share stops with the end
// reason system and the toast "Sharing stopped while your PC was locked" (with Open). A share that is live again
// was resumed (its live event may still be on its way): it only forgets the lock pause.
func (a *App) lockStopReached(gen int) {
	v := a.shares.Current()
	a.mu.Lock()
	if !a.sys.lockPaused || a.sys.gen != gen {
		a.mu.Unlock()
		return
	}
	a.sys.stopTimer = nil
	a.clearLockLocked()
	a.mu.Unlock()
	if v == nil || v.State == share.StateLive || v.State == share.StateEnded ||
		v.State == share.StateFailed {
		a.removeStaleToasts()
		a.syncAwake()
		a.scheduleTray()
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, QuitTimeout)
	a.shares.Stop(ctx, share.CauseSystem)
	cancel()
	t := a.d.I18n
	// notify removes the stale "Resume sharing?" toast first, on the same serialized path, then shows this one.
	a.notify(ToastStoppedLock, t.T("desktop.native.toast.stoppedLocked"),
		NotificationAction{ID: ToastOpen, Title: t.T("desktop.native.toast.open")})
	a.syncAwake()
	a.scheduleTray()
}

// resume resumes the share (the tray's, a toast's or the pill's Resume): it ends a lock pause, and keep-awake comes
// back.
func (a *App) resume() {
	ctx, cancel := context.WithTimeout(a.ctx, pauseTimeout)
	err := a.shares.SetPaused(ctx, false)
	cancel()
	if err != nil {
		a.log.Info("app: the share wasn't resumed", errAttr(err))
		return
	}
	a.mu.Lock()
	a.clearLockLocked()
	a.mu.Unlock()
	a.removeStaleToasts()
	a.syncAwake()
	a.scheduleTray()
}

// onSuspend stops a running share (end reason system); the resume shows "Sharing stopped because your PC went to
// sleep". C1 and C2 reconnect after the resume by themselves (01).
func (a *App) onSuspend() {
	if _, live := a.Live(); !live {
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, SystemStopTimeout)
	a.shares.Stop(ctx, share.CauseSystem)
	cancel()
	a.mu.Lock()
	a.sys.suspendStopped = true
	a.clearLockLocked()
	a.mu.Unlock()
	a.removeStaleToasts()
	a.syncAwake()
	a.scheduleTray()
}

// onResume shows the toast of a share the suspend stopped.
func (a *App) onResume() {
	a.mu.Lock()
	stopped := a.sys.suspendStopped
	a.sys.suspendStopped = false
	a.mu.Unlock()
	if stopped {
		t := a.d.I18n
		a.notify(ToastStoppedSleep, t.T("desktop.native.toast.stoppedSleep"),
			NotificationAction{ID: ToastOpen, Title: t.T("desktop.native.toast.open")})
	}
}

// onEndSession is WM_QUERYENDSESSION: a running share stops within 2 s (share.stop, C2's close 1000) and the OS
// hooks are told to let the session end. The app keeps running: the end may still be cancelled
// (onEndSessionConfirmed quits).
func (a *App) onEndSession() {
	if _, live := a.Live(); live {
		ctx, cancel := context.WithTimeout(a.ctx, SystemStopTimeout)
		a.shares.Stop(ctx, share.CauseSystem)
		cancel()
	}
	a.ackEndSession()
}

// onEndSessionConfirmed is WM_ENDSESSION with the session ending: the app quits (C1 and C2 close with 1000, the
// audio service closes, settings are flushed) and then tells the OS hooks, which hold WM_ENDSESSION until then, so
// Windows doesn't end the process first. It runs off the events loop: the loop is one of the parts the quit waits
// for.
func (a *App) onEndSessionConfirmed() {
	go func() {
		defer a.ackEndSession()
		a.quit(true, share.CauseSystem, true)
		parts := make(chan struct{})
		go func() { a.wg.Wait(); close(parts) }()
		select {
		case <-parts:
		case <-time.After(EndSessionQuitTimeout):
			a.log.Warn("app: the app's parts didn't stop before the end of the session")
		}
	}()
}

// ackEndSession lets the OS hooks' waiting end-of-session message return.
func (a *App) ackEndSession() {
	if ack, ok := a.d.OS.(endSessionAcker); ok {
		ack.EndSessionHandled()
	}
}

// onShareEvent follows the share controller (its subscriber: it only reads the view): a lock pause that anything
// resumed ends, an ended share forgets its lock pause, keep-awake follows, and the tray is redrawn.
func (a *App) onShareEvent(ev share.Event) {
	if ev.Kind != share.EventState {
		return
	}
	v := ev.View
	// A live event resumes a lock pause only when the share is live now: a live event read before the lock's pause
	// may be delivered after it (the controller reads the view at the delivery, and the pause's own event may
	// already have gone out inside SetPaused, before onLock recorded the lock pause).
	resumed := v != nil && v.State == share.StateLive
	if resumed {
		cur := a.shares.Current()
		resumed = cur != nil && cur.State == share.StateLive
	}
	a.mu.Lock()
	switch {
	case v == nil || v.State == share.StateEnded || v.State == share.StateFailed:
		a.clearLockLocked()
	case a.sys.lockPaused && resumed:
		a.clearLockLocked() // resumed: the panel, the hotkey, the tray, the pill or a toast
	}
	a.mu.Unlock()
	a.removeStaleToasts()
	a.syncAwake()
	a.scheduleTray()
}

// awakeWanted reports a share that keeps the PC awake: from live to its end (03 §11.6's last row).
func awakeWanted(v *share.View) bool {
	if v == nil || v.LiveSince.IsZero() {
		return false
	}
	switch v.State {
	case share.StateLive, share.StatePaused, share.StateReconnecting, share.StateUnreachable:
		return true
	}
	return false
}

// syncAwake sets keep-awake by the share and the lock: on from live to the end, off while the PC is locked and while
// a lock pause waits for its "Resume sharing?" answer. KeepAwake is called only when that changes.
func (a *App) syncAwake() {
	v := a.shares.Current()
	a.awakeMu.Lock()
	defer a.awakeMu.Unlock()
	a.mu.Lock()
	want := awakeWanted(v) && !a.sys.locked && !a.sys.prompt
	a.mu.Unlock()
	if want == a.awake {
		return
	}
	a.awake = want
	a.d.OS.KeepAwake(want)
}

// stopTimers stops the tray's debounce and the lock pause's timer: the app quits.
func (a *App) stopTimers() {
	a.tray.mu.Lock()
	a.tray.stopped = true
	if a.tray.timer != nil {
		a.tray.timer.Stop()
		a.tray.timer = nil
	}
	a.tray.mu.Unlock()
	a.mu.Lock()
	if a.sys.stopTimer != nil {
		a.sys.stopTimer.Stop()
		a.sys.stopTimer = nil
	}
	a.mu.Unlock()
}

// Heartbeat is the SPA's watchdog beat (W17's fallback, 03 §20): every 5 s.
func (a *App) Heartbeat() {
	a.mu.Lock()
	a.beat.last, a.beat.ever = a.clock(), true
	a.mu.Unlock()
}

// runWatchdog checks the heartbeat every HeartbeatEvery until the app ends.
func (a *App) runWatchdog() {
	t := time.NewTicker(HeartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-t.C:
			a.watchdogTick()
		}
	}
}

// watchdogTick reloads the page when a visible window had no heartbeat for HeartbeatTimeout. It arms only once the
// page has beaten (a page that never calls Heartbeat is never reloaded); a hidden window isn't watched (WebView2
// throttles a hidden page's timers), and one that was just shown gets a fresh HeartbeatTimeout. After a reload the
// next one waits another HeartbeatTimeout.
func (a *App) watchdogTick() {
	visible := a.d.Shell.WindowVisible()
	now := a.clock()
	a.mu.Lock()
	w := &a.beat
	shown := visible && !w.wasVisible
	w.wasVisible = visible
	switch {
	case !w.ever || !visible:
		a.mu.Unlock()
		return
	case shown:
		w.last = now
		a.mu.Unlock()
		return
	case now.Sub(w.last) <= HeartbeatTimeout:
		a.mu.Unlock()
		return
	}
	w.last = now
	a.mu.Unlock()
	a.log.Warn("app: the page sent no heartbeat; reloading it", "after", HeartbeatTimeout.String())
	a.d.Shell.Reload()
}
