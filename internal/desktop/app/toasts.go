package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"slices"
	"strconv"
	"sync"

	"github.com/MoonWX/isshoni/internal/desktop/share"
)

// Toasts (03 §15): the app builds each Notification and the shell shows it through Wails' notifications service
// (W8). A toast with buttons carries a random 128-bit nonce; the app accepts a button only for a nonce of a toast it
// sent in this run and hasn't settled, so an action that arrives any other way (a toast of a run that is gone, at
// -Embedding; a protocol activation a web page made up) does nothing. Opening the window, the toast's body and its
// Open button, needs no nonce.

// The toasts' kinds (Notification.Kind): WA68's here; README WA77, WA80 and WA84 add the mic prompt, audio alert,
// pause, stopped and update toasts.
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

// toastBook holds the nonces of the toasts sent in this run that haven't been settled.
type toastBook struct {
	mu      sync.Mutex
	seq     int
	pending map[string]pendingToast // by nonce
	order   []string                // the nonces, oldest first
}

// pendingToast is a sent toast with buttons.
type pendingToast struct {
	kind    string
	actions []string
}

// notify shows a toast of kind with body; actions are its buttons, ids among the Toast* actions, with their titles
// (the catalog's desktop.native.toast.*). A failing shell is logged once per toast and never retried.
func (a *App) notify(kind, body string, actions ...NotificationAction) {
	n := Notification{Kind: kind, Title: toastTitle, Body: body, Actions: actions}
	b := &a.toasts
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
		b.pending[n.Nonce] = pendingToast{kind: kind, actions: ids}
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

// dropKind settles every pending toast of kind without an action: its question was answered another way (the
// tray, the panel, a later stop, the share's end), so its buttons, still in Action Center, no longer act.
func (b *toastBook) dropKind(kind string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.order = slices.DeleteFunc(b.order, func(n string) bool {
		if p, ok := b.pending[n]; ok && p.kind == kind {
			delete(b.pending, n)
			return true
		}
		return false
	})
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
	default:
		// keep-out, include (README WA77's mic prompt) and restart (WA84) have no toast in this build yet.
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
