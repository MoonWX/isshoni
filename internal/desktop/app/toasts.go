package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"slices"
	"strconv"
	"sync"

	"github.com/MoonWX/isshoni/internal/desktop/audio"
	"github.com/MoonWX/isshoni/internal/desktop/share"
)

// Toasts (03 §15): the app builds each Notification and the shell shows it through Wails' notifications service
// (W8). A toast with buttons carries a random 128-bit nonce; the app accepts a button only for a nonce of a toast it
// sent in this run and hasn't settled, so an action that arrives any other way (a toast of a run that is gone, at
// -Embedding; a protocol activation a web page made up) does nothing. Opening the window, the toast's body and its
// Open button, needs no nonce.

// The toasts' kinds (Notification.Kind): WA68's here, the mic prompt's in audio.go (ToastMicPrompt), WA80's audio
// alert and pause in safety.go, and the update's in updates.go (ToastUpdateReady, README WA84).
const (
	ToastStillSharing = "still-sharing"  // the first close to the tray while live
	ToastLockPaused   = "lock-paused"    // an unlock later than LockResume after the lock: Resume · Stop
	ToastStoppedLock  = "stopped-locked" // a lock pause reached LockStop: Open
	ToastStoppedSleep = "stopped-sleep"  // a suspend stopped the share; shown after the resume: Open
)

// The toasts' actions (NotificationAction.ID). ToastOpen is also what the shell reports for a click on a toast's
// body (Wails' default action).
const (
	ToastOpen    = "open"
	ToastResume  = "resume"
	ToastStop    = "stop"
	ToastKeepOut = "keep-out"
	ToastInclude = "include"
	ToastRestart = "restart"
)

// toastTitle is every toast's title: the app's name (the body says what happened).
const toastTitle = "isshoni"

// maxToasts bounds the toasts whose buttons are still accepted: an older one's buttons are dropped.
const maxToasts = 32

// toastRemover is the OS hooks' removal from Action Center (winos.Hooks.RemoveToasts; e2ehost's recorder): it takes
// the toasts whose Notification.ID is among ids out of Action Center and reports how many went. The app finds it by
// a type assertion on Deps.OS, like the other optional hooks; without it a stale toast stays in Action Center, its
// buttons settled (they no longer act).
//
// Its contract (winos): calls are serialized with Shell.Notify and never made with App.mu held. The app keeps both
// through toastBook.out: notify and removeStaleToasts are the only callers of Shell.Notify and RemoveToasts, both
// hold out, and neither runs under App.mu.
type toastRemover interface {
	RemoveToasts(ids ...string) (int, error)
}

// toastBook holds the nonces of the toasts sent in this run that haven't been settled, and the IDs of the toasts
// that were dropped (their question answered another way) and still have to leave Action Center.
type toastBook struct {
	// out serializes the app's toast output: Shell.Notify and toastRemover.RemoveToasts (RemoveToasts' contract: an
	// untagged toast's removal clears the app's toasts and delivers the others again, so a toast shown meanwhile
	// could be lost). It is taken before mu, never under App.mu.
	out sync.Mutex

	mu      sync.Mutex
	seq     int
	pending map[string]pendingToast // by nonce
	order   []string                // the nonces, oldest first
	stale   []string                // Notification IDs dropKind settled, not yet removed from Action Center
}

// pendingToast is a sent toast with buttons.
type pendingToast struct {
	id      string // the Notification's ID: what Action Center's removal finds it by
	kind    string
	ref     string // what the toast asks about: a mic prompt's id; "" for the others
	actions []string
}

// notify shows a toast of kind with body; actions are its buttons, ids among the Toast* actions, with their titles
// (the catalog's desktop.native.toast.*). A failing shell is logged once per toast and never retried. Stale toasts
// leave Action Center first, on the same serialized path (toastRemover's contract), so the new toast is never
// shown while they are being removed. It must not be called with a.mu held.
func (a *App) notify(kind, body string, actions ...NotificationAction) {
	a.notifyRef(kind, "", body, actions...)
}

// notifyRef is notify for a toast about one thing, ref (a mic prompt's id): its buttons act on ref, and dropRefsExcept
// settles it once ref is gone.
func (a *App) notifyRef(kind, ref, body string, actions ...NotificationAction) {
	n := Notification{Kind: kind, Title: toastTitle, Body: body, Actions: actions}
	b := &a.toasts
	b.out.Lock()
	defer b.out.Unlock()
	a.removeStaleLocked()
	b.mu.Lock()
	b.seq++
	n.ID = kind + "-" + strconv.Itoa(b.seq)
	if len(actions) > 0 {
		n.Nonce = newNonce()
		if b.pending == nil {
			b.pending = map[string]pendingToast{}
		}
		ids := make([]string, len(actions))
		for i, act := range actions {
			ids[i] = act.ID
		}
		b.pending[n.Nonce] = pendingToast{id: n.ID, kind: kind, ref: ref, actions: ids}
		b.order = append(b.order, n.Nonce)
		for len(b.order) > maxToasts {
			delete(b.pending, b.order[0])
			b.order = b.order[1:]
		}
	}
	b.mu.Unlock()
	if err := a.d.Shell.Notify(n); err != nil {
		a.log.Warn("app: a toast wasn't shown", "kind", kind, "err", err)
	}
}

// newNonce is 128 random bits in hex (32 characters).
func newNonce() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails (Go 1.24+)
	return hex.EncodeToString(b[:])
}

// settle takes the pending toast of nonce when action is one of its buttons; ok false otherwise (unknown, settled
// already, or not one of its buttons). A toast is settled by its first accepted action.
func (b *toastBook) settle(nonce, action string) (pendingToast, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.pending[nonce]
	if !ok || !slices.Contains(p.actions, action) {
		return pendingToast{}, false
	}
	delete(b.pending, nonce)
	b.order = slices.DeleteFunc(b.order, func(n string) bool { return n == nonce })
	return p, true
}

// take settles the pending toast of nonce whatever its buttons are (a dismissal); ok false for an unknown nonce.
func (b *toastBook) take(nonce string) (pendingToast, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.pending[nonce]
	if !ok {
		return pendingToast{}, false
	}
	delete(b.pending, nonce)
	b.order = slices.DeleteFunc(b.order, func(n string) bool { return n == nonce })
	return p, true
}

// dropRefsExcept settles every pending toast of kind whose ref isn't in keep: what it asks about was answered
// another way, or is gone. Like dropKind, it keeps their IDs as stale; they leave Action Center at the next notify
// or removeStaleToasts (it only touches the book, so it makes no WinRT round trip on the caller's thread).
func (b *toastBook) dropRefsExcept(kind string, keep []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.order = slices.DeleteFunc(b.order, func(n string) bool {
		if p, ok := b.pending[n]; ok && p.kind == kind && !slices.Contains(keep, p.ref) {
			delete(b.pending, n)
			b.stale = append(b.stale, p.id)
			return true
		}
		return false
	})
}

// dropKind settles every pending toast of kind without an action: its question was answered another way (the
// tray, the panel, a later stop, the share's end), so its buttons, still in Action Center, no longer act. Their IDs
// are kept as stale: the toasts themselves leave Action Center at the next removeStaleToasts or notify (03 §15). It
// only touches the book, so callers may hold a.mu; they call removeStaleToasts once they released it.
func (b *toastBook) dropKind(kind string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.order = slices.DeleteFunc(b.order, func(n string) bool {
		if p, ok := b.pending[n]; ok && p.kind == kind {
			delete(b.pending, n)
			b.stale = append(b.stale, p.id)
			return true
		}
		return false
	})
}

// takeStale is the stale toasts' IDs, which it forgets.
func (b *toastBook) takeStale() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	ids := b.stale
	b.stale = nil
	return ids
}

// hasStale reports stale IDs waiting.
func (b *toastBook) hasStale() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.stale) > 0
}

// removeStaleToasts takes the toasts dropKind settled out of Action Center (03 §15; WF13's review R3) through the
// OS hooks' toastRemover, serialized with notify. It must not be called with a.mu held: the removal makes WinRT
// round trips. With nothing to remove it returns at once, without waiting for a toast on its way out (the share
// events call it for every state); a toast dropped later is removed by its own dropper's call.
func (a *App) removeStaleToasts() {
	if !a.toasts.hasStale() {
		return
	}
	a.toasts.out.Lock()
	defer a.toasts.out.Unlock()
	a.removeStaleLocked()
}

// removeStaleLocked is removeStaleToasts with toastBook.out held. A failed removal is logged and not retried: the
// toasts' buttons are settled already, so a toast left behind only looks stale.
func (a *App) removeStaleLocked() {
	ids := a.toasts.takeStale()
	if len(ids) == 0 {
		return
	}
	r, ok := a.d.OS.(toastRemover)
	if !ok {
		return
	}
	if _, err := r.RemoveToasts(ids...); err != nil {
		a.log.Warn("app: stale toasts weren't removed from Action Center", "count", len(ids), errAttr(err))
	}
}

// ToastAction is the shell's report of a toast activation: the nonce the toast carried ("" for none) and the
// action ("" or ToastOpen for its body). Open always opens the window (and settles the toast when its nonce is
// known); any other action runs only for a pending toast of this run that has that button, once. It reports whether
// the action ran.
func (a *App) ToastAction(nonce, action string) bool {
	if action == "" || action == ToastOpen {
		if nonce != "" {
			a.toasts.settle(nonce, ToastOpen)
		}
		a.ShowWindow()
		return true
	}
	p, ok := a.toasts.settle(nonce, action)
	if !ok {
		a.log.Info("app: a toast action without a pending toast was dropped", "action", safeAction(action))
		return false
	}
	switch action {
	case ToastResume:
		a.resume()
	case ToastStop:
		ctx, cancel := context.WithTimeout(a.ctx, QuitTimeout)
		defer cancel()
		a.shares.Stop(ctx, share.CauseUser)
	case ToastKeepOut, ToastInclude:
		if p.kind != ToastMicPrompt {
			return false
		}
		d := audio.DecisionKeepOut
		if action == ToastInclude {
			d = audio.DecisionInclude
		}
		return a.answerPrompt(p.ref, d)
	case ToastRestart:
		if p.kind != ToastUpdateReady {
			return false
		}
		a.restartForUpdate("toast")
	default:
		a.log.Info("app: a toast action this build doesn't handle", "kind", p.kind, "action", action)
		return false
	}
	return true
}

// safeAction is an action id for the log: one of the known ids, else "other" (an activation's text is the
// outside world's).
func safeAction(action string) string {
	switch action {
	case ToastOpen, ToastResume, ToastStop, ToastKeepOut, ToastInclude, ToastRestart:
		return action
	}
	return "other"
}
