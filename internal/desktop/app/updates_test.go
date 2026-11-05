package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/desktop/settings"
	"github.com/MoonWX/isshoni/internal/desktop/share"
	"github.com/MoonWX/isshoni/internal/desktop/tray"
	"github.com/MoonWX/isshoni/internal/update"
)

// WA84: the updater in the app (03 §15, §16.2; 06 §8.7): the status the tray, the toast and the bound service
// follow, and "Restart and update", refused while a share is live.

// fakeUpdater is 06's updater as the app sees it: a status the test sets, a ready installer, and the applies.
type fakeUpdater struct {
	ran atomic.Bool

	mu       sync.Mutex
	st       update.Status
	ready    *update.Pending
	applies  []string // "<version> relaunch=<bool>"
	applyErr error
	checkErr error
	subs     []chan update.Status
}

func newFakeUpdater(st update.Status) *fakeUpdater { return &fakeUpdater{st: st} }

func (f *fakeUpdater) Run(ctx context.Context) error {
	f.ran.Store(true)
	<-ctx.Done()
	return nil
}

func (f *fakeUpdater) Status() update.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}

func (f *fakeUpdater) Subscribe() (<-chan update.Status, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan update.Status, 1)
	ch <- f.st
	f.subs = append(f.subs, ch)
	return ch, func() {}
}

// set publishes st, ready while it is StateReady (as 06's set does).
func (f *fakeUpdater) set(st update.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st = st
	f.ready = nil
	if st.State == update.StateReady {
		f.ready = &update.Pending{Version: st.Available, Path: "installer.exe", Required: st.Required}
	}
	for _, ch := range f.subs {
		select {
		case <-ch:
		default:
		}
		ch <- st
	}
}

func (f *fakeUpdater) CheckNow(context.Context) (update.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st, f.checkErr
}

func (f *fakeUpdater) Ready() *update.Pending {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ready == nil {
		return nil
	}
	p := *f.ready
	return &p
}

func (f *fakeUpdater) Apply(ctx context.Context, p *update.Pending, relaunch bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	f.applies = append(f.applies, p.Version+map[bool]string{true: " relaunch=true", false: " relaunch=false"}[relaunch])
	return f.applyErr
}

func (f *fakeUpdater) applied() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.applies)
}

// newUpdateEnv is newSysEnv with a fake updater that the app follows (as Run does).
func newUpdateEnv(t *testing.T) (*sysEnv, *fakeUpdater) {
	t.Helper()
	s := newSysEnv(t)
	u := newFakeUpdater(update.Status{State: update.StateIdle, Current: "0.2.0"})
	s.app.d.Updater = u
	s.app.startUpdates()
	return s, u
}

func readyStatus(v string) update.Status {
	return update.Status{State: update.StateReady, Current: "0.2.0", Available: v}
}

// TestUpdateReadyFollowed: a ready update puts "Restart and update to X" in the tray's Idle menu, shows the toast
// once per version with Restart now, and reaches SubscribeUpdates; once nothing is ready the menu item goes.
func TestUpdateReadyFollowed(t *testing.T) {
	s, u := newUpdateEnv(t)
	var mu sync.Mutex
	var seen []update.State
	cancel := s.app.SubscribeUpdates(func(st update.Status) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, st.State)
	})
	defer cancel()

	u.set(readyStatus("0.3.0"))
	s.waitTray(t, func(v tray.View) bool {
		return slices.ContainsFunc(v.Menu, func(m tray.MenuItem) bool {
			return m.Action == tray.ActionRestart && m.Label == "Restart and update to 0.3.0"
		})
	}, "the tray offers the ready update")
	waitUntil(t, func() bool { return len(s.shell.toastsOf(ToastUpdateReady)) == 1 }, "the ready toast")
	n := s.shell.toastsOf(ToastUpdateReady)[0]
	if n.Body != "isshoni 0.3.0 is ready. Restart to update." || len(n.Actions) != 1 ||
		n.Actions[0].ID != ToastRestart || n.Nonce == "" {
		t.Fatalf("toast %+v", n)
	}
	// The same version again: no second toast.
	u.set(readyStatus("0.3.0"))
	u.set(update.Status{State: update.StateIdle})
	s.waitTray(t, func(v tray.View) bool {
		return !slices.ContainsFunc(v.Menu, func(m tray.MenuItem) bool { return m.Action == tray.ActionRestart })
	}, "the offer goes once nothing is ready")
	if got := len(s.shell.toastsOf(ToastUpdateReady)); got != 1 {
		t.Fatalf("%d ready toasts", got)
	}
	waitUntil(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) > 0 && seen[len(seen)-1] == update.StateIdle && slices.Contains(seen, update.StateReady)
	}, "the subscriber saw ready and idle")
	// A dropped toast's Restart now does nothing.
	if s.app.ToastAction(n.Nonce, ToastRestart) {
		t.Fatal("a settled toast's Restart ran")
	}
	if len(u.applied()) != 0 {
		t.Fatal("applied")
	}
}

// TestUpdateToastNotifications: "Show notifications" off: no ready toast, the tray still offers it; while a share is
// live the toast has no Restart now.
func TestUpdateToastNotifications(t *testing.T) {
	s, u := newUpdateEnv(t)
	if err := s.app.d.Settings.Update(func(st *settings.Settings) { st.Notifications = false }); err != nil {
		t.Fatal(err)
	}
	u.set(readyStatus("0.3.0"))
	s.waitTray(t, func(v tray.View) bool { return v.Menu != nil }, "a tray")
	time.Sleep(20 * time.Millisecond)
	if got := len(s.shell.toastsOf(ToastUpdateReady)); got != 0 {
		t.Fatalf("%d toasts with notifications off", got)
	}
	if err := s.app.d.Settings.Update(func(st *settings.Settings) { st.Notifications = true }); err != nil {
		t.Fatal(err)
	}
	s.goLive(t)
	u.set(readyStatus("0.3.1"))
	waitUntil(t, func() bool { return len(s.shell.toastsOf(ToastUpdateReady)) == 1 }, "the ready toast")
	if n := s.shell.toastsOf(ToastUpdateReady)[0]; len(n.Actions) != 0 {
		t.Fatalf("a live share's toast has buttons: %+v", n)
	}
}

// TestInstallUpdateRefusedWhileLive (03 §16.2): Restart and update answers busy while a share is live and the updater
// isn't told; once the share ended it applies the ready installer with relaunch and quits.
func TestInstallUpdateRefusedWhileLive(t *testing.T) {
	s, u := newUpdateEnv(t)
	u.set(readyStatus("0.3.0"))
	s.goLive(t)
	if err := s.app.InstallUpdate(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("InstallUpdate while live: %v", err)
	}
	s.app.TrayAction(tray.ActionRestart) // the tray's item (Idle only, but a stale menu may call it): refused too
	if len(u.applied()) != 0 || s.shell.count("quit") != 0 {
		t.Fatalf("applied while live: %v", u.applied())
	}
	s.app.TrayAction(tray.ActionStop)
	s.waitState(t, share.StateEnded)

	// A page that goes away doesn't cut the install short.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.app.InstallUpdate(ctx); err != nil {
		t.Fatal(err)
	}
	if got := u.applied(); !slices.Equal(got, []string{"0.3.0 relaunch=true"}) {
		t.Fatalf("applies %v", got)
	}
	if s.shell.count("quit") != 1 {
		t.Fatal("the app didn't quit after the installer started")
	}
}

// TestInstallUpdateErrors: no updater (not_implemented), nothing ready, and an Apply that failed: the app keeps
// running.
func TestInstallUpdateErrors(t *testing.T) {
	e := newEnv(t, nil, nil)
	if err := e.app.InstallUpdate(context.Background()); !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("no updater: %v", err)
	}
	if _, err := e.app.UpdateStatus(); !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("no updater: %v", err)
	}
	if _, err := e.app.CheckUpdate(context.Background()); !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("no updater: %v", err)
	}

	s, u := newUpdateEnv(t)
	if err := s.app.InstallUpdate(context.Background()); !errors.Is(err, ErrUpdateNotReady) {
		t.Fatalf("nothing ready: %v", err)
	}
	u.set(readyStatus("0.3.0"))
	u.applyErr = update.ErrNotInstalled
	if err := s.app.InstallUpdate(context.Background()); !errors.Is(err, update.ErrNotInstalled) {
		t.Fatalf("apply failed: %v", err)
	}
	if s.shell.count("quit") != 0 {
		t.Fatal("quit after a failed apply")
	}
	// The tray's item then opens the window, where Settings → Updates says why.
	s.shell.reset()
	s.app.TrayAction(tray.ActionRestart)
	if s.shell.count("show(true)") != 1 {
		t.Fatalf("calls %v", s.shell.calls)
	}
	u.checkErr = &update.Error{Code: update.CodeNetwork}
	if _, err := s.app.CheckUpdate(context.Background()); update.CodeOf(err) != update.CodeNetwork {
		t.Fatalf("CheckUpdate: %v", err)
	}
	if st, err := s.app.UpdateStatus(); err != nil || st.State != update.StateReady {
		t.Fatalf("UpdateStatus = %+v, %v", st, err)
	}
}

// TestUpdateToastRestart: the ready toast's Restart now applies and quits.
func TestUpdateToastRestart(t *testing.T) {
	s, u := newUpdateEnv(t)
	u.set(readyStatus("0.3.0"))
	waitUntil(t, func() bool { return len(s.shell.toastsOf(ToastUpdateReady)) == 1 }, "the ready toast")
	n := s.shell.toastsOf(ToastUpdateReady)[0]
	if !s.app.ToastAction(n.Nonce, ToastRestart) {
		t.Fatal("Restart now didn't run")
	}
	if got := u.applied(); len(got) != 1 || !strings.HasPrefix(got[0], "0.3.0") || s.shell.count("quit") != 1 {
		t.Fatalf("applies %v", got)
	}
}

func waitUntil(t *testing.T, ok func() bool, what string) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); !ok(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
	}
}
