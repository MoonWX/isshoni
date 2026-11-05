package app

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/desktop/share"
	"github.com/MoonWX/isshoni/internal/update"
)

// The updater in the app (03 §15, §16.2; 06 §8.7; README WA84). 06's updater checks, downloads and applies; the app
// runs its schedule (Run), follows its status (the tray's "Restart and update to X", the ready toast and the bound
// Updates service's update.state event) and owns "Restart and update": never while a share is live (busy; the
// updater isn't told), else 06's Apply with the verified installer, then Quit. The start-time apply (03 §13.4 step 5)
// happens in cmd/isshoni-desktop before the app exists; at an --autostart start it applies nothing, so the update
// stays ready and the tray offers it (06 §8.7 (b)).

// ToastUpdateReady is the toast of a verified update (03 §15): "isshoni 0.3.0 is ready. Restart to update." with
// Restart now while no share is live.
const ToastUpdateReady = "update-ready"

// ErrUpdateNotReady is InstallUpdate's error when no verified installer waits (the bridge's update.not_ready).
var ErrUpdateNotReady = errors.New("app: no verified update is ready")

// installTimeout bounds "Restart and update": 06's Apply waits up to 10 s for the installer's mutex. The caller's
// context doesn't cut it short: a page that goes away must not leave an installer started and the app running.
const installTimeout = 15 * time.Second

// updateFollow is the app's view of the updater's status: who follows it, and the version whose toast went out.
type updateFollow struct {
	mu      sync.Mutex
	toasted string // the version the ready toast was shown for; "" while none is ready
	next    int
	subs    map[int]func(update.Status)

	// The gate between "Restart and update" and a share's start (03 §16.2: never while a share is live). Apply waits
	// up to 10 s for the installer's mutex, so a share the SPA starts meanwhile would be ended by the Quit that
	// follows: while installing is set, Shares().Start answers ErrBusy, and while a start runs (starting > 0),
	// InstallUpdate does.
	gate       sync.Mutex
	installing bool
	starting   int
}

// startUpdates follows the updater's status until the app ends (Run). Without an updater it does nothing.
func (a *App) startUpdates() {
	u := a.d.Updater
	if u == nil {
		return
	}
	ch, cancel := u.Subscribe()
	a.wg.Go(func() {
		defer cancel()
		for {
			select {
			case <-a.ctx.Done():
				return
			case st, ok := <-ch:
				if !ok {
					return
				}
				a.onUpdateStatus(st)
			}
		}
	})
}

// onUpdateStatus follows one status: the tray's offer while an update is ready, the ready toast once per version
// (Settings → App → "Show notifications" turns it off), the toast's settling once nothing is ready any more, and the
// SubscribeUpdates callbacks.
func (a *App) onUpdateStatus(st update.Status) {
	ready := ""
	if st.State == update.StateReady && st.Available != "" {
		ready = st.Available
	}
	a.SetUpdateReady(ready)
	a.upd.mu.Lock()
	prev := a.upd.toasted
	a.upd.toasted = ready
	subs := a.upd.callbacks()
	a.upd.mu.Unlock()
	switch {
	case ready != "" && ready != prev:
		a.updateToast(ready)
	case ready == "" && prev != "":
		a.toasts.dropKind(ToastUpdateReady)
		a.removeStaleToasts()
	}
	for _, fn := range subs {
		fn(st)
	}
}

// updateToast is "isshoni <version> is ready. Restart to update.", with Restart now while no share is live.
func (a *App) updateToast(version string) {
	if !a.d.Settings.Get().Notifications {
		return
	}
	body := a.d.I18n.T("desktop.native.toast.updateReady", "version", version)
	if _, live := a.sharing(); live {
		a.notify(ToastUpdateReady, body)
		return
	}
	a.notify(ToastUpdateReady, body,
		NotificationAction{ID: ToastRestart, Title: a.d.I18n.T("desktop.native.toast.restartNow")})
}

// callbacks returns the subscribers in the order they came; mu is held.
func (f *updateFollow) callbacks() []func(update.Status) {
	ids := make([]int, 0, len(f.subs))
	for id := range f.subs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]func(update.Status), 0, len(ids))
	for _, id := range ids {
		out = append(out, f.subs[id])
	}
	return out
}

// SubscribeUpdates calls fn with every status of the updater from Run on (the bound Updates service's update.state
// event, 03 §6.3); fn must not block. cancel stops the calls. Without an updater fn is never called.
func (a *App) SubscribeUpdates(fn func(update.Status)) (cancel func()) {
	f := &a.upd
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subs == nil {
		f.subs = map[int]func(update.Status){}
	}
	id := f.next
	f.next++
	f.subs[id] = fn
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.subs, id)
	}
}

// UpdateStatus is the updater's status (Updates.Status()); ErrNotImplemented without an updater (a second instance
// never builds one, nor does the e2e harness).
func (a *App) UpdateStatus() (update.Status, error) {
	if a.d.Updater == nil {
		return update.Status{}, ErrNotImplemented
	}
	return a.d.Updater.Status(), nil
}

// CheckUpdate is "Check now" (Updates.Check(), 03 §16.2): 06's CheckNow, which never waits for the wizard and honors
// the setting and the policy. Its error carries 06's code (update.CodeOf).
func (a *App) CheckUpdate(ctx context.Context) (update.Status, error) {
	if a.d.Updater == nil {
		return update.Status{}, ErrNotImplemented
	}
	return a.d.Updater.CheckNow(ctx)
}

// InstallUpdate is "Restart and update" (Settings → Updates, the required banner, the tray, the toast; 03 §16.2):
// ErrBusy while a share is live or starting, or another "Restart and update" runs (03 enforces it; the updater isn't
// told; a share the SPA starts meanwhile is refused with ErrBusy, beginInstall), ErrUpdateNotReady when no verified installer
// waits, else 06's Apply with relaunch, and once the installer runs, Quit. Apply's errors are returned as they are
// (update.ErrNotInstalled, *update.Error), and the app keeps running.
func (a *App) InstallUpdate(ctx context.Context) error {
	u := a.d.Updater
	if u == nil {
		return ErrNotImplemented
	}
	if !a.beginInstall() {
		return ErrBusy
	}
	p := u.Ready()
	if p == nil {
		a.endInstall()
		return ErrUpdateNotReady
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), installTimeout)
	defer cancel()
	if err := u.Apply(ctx, p, true); err != nil {
		a.endInstall()
		a.log.Warn("app: the update wasn't applied", "version", p.Version, "code", update.CodeOf(err))
		return err
	}
	if _, live := a.sharing(); live { // the gate rules it out; were it to happen, the end isn't the user's
		a.log.Warn("app: a share went live while the update's installer started; stopping it", "version", p.Version)
		a.quit(true, share.CauseSystem, true)
		return nil
	}
	a.log.Info("app: the update's installer runs; quitting", "version", p.Version)
	a.Quit(true)
	return nil
}

// beginInstall closes the gate for "Restart and update": false (busy) while a share is live or starting. The gate
// stays closed once the installer runs (the app quits); endInstall opens it again after a refusal or a failure.
func (a *App) beginInstall() bool {
	g := &a.upd
	g.gate.Lock()
	defer g.gate.Unlock()
	if g.installing || g.starting > 0 {
		return false
	}
	if _, live := a.sharing(); live {
		return false
	}
	g.installing = true
	return true
}

func (a *App) endInstall() {
	a.upd.gate.Lock()
	a.upd.installing = false
	a.upd.gate.Unlock()
}

// beginShareStart is the other side of the gate: false (busy) while "Restart and update" runs; else done must be
// called once the start returned.
func (a *App) beginShareStart() (done func(), ok bool) {
	g := &a.upd
	g.gate.Lock()
	defer g.gate.Unlock()
	if g.installing {
		return nil, false
	}
	g.starting++
	return func() {
		g.gate.Lock()
		g.starting--
		g.gate.Unlock()
	}, true
}

// gatedShares is the share controller as Shares() hands it out: Start answers ErrBusy while "Restart and update"
// runs (beginShareStart).
type gatedShares struct {
	*share.Controller
	a *App
}

func (g gatedShares) Start(ctx context.Context, r share.StartRequest) (share.View, error) {
	done, ok := g.a.beginShareStart()
	if !ok {
		return share.View{}, ErrBusy
	}
	defer done()
	return g.Controller.Start(ctx, r)
}

// restartForUpdate is the tray's "Restart and update to X" and the toast's Restart now: InstallUpdate, and on a
// refusal or a failure the window, whose Settings → Updates says why.
func (a *App) restartForUpdate(from string) {
	if err := a.InstallUpdate(a.ctx); err != nil {
		a.log.Info("app: Restart and update didn't restart", "from", from, "busy", errors.Is(err, ErrBusy))
		a.ShowWindow()
	}
}
