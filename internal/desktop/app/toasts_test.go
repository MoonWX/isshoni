package app

import (
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/desktop/share"
	"github.com/MoonWX/isshoni/internal/desktop/tray"
)

// WF14: a toast whose question was answered another way leaves Action Center (03 §15) through the OS hooks'
// toastRemover, serialized with Shell.Notify and never under App.mu (winos RemoveToasts' contract; WF13's review R1
// and R3).

// removerOS is recOS with a recording toastRemover. Each call records its IDs, how many toasts the shell had shown
// by then, and whether App.mu was free; inFlight lets the shell check that no toast is shown during a removal.
type removerOS struct {
	*recOS
	app      *App
	shell    *sysShell
	err      error // what RemoveToasts answers
	hold     time.Duration
	inFlight atomic.Bool

	mu    sync.Mutex
	calls []removal
}

type removal struct {
	ids     []string
	shown   int  // toasts the shell had shown when the removal began
	muBusy  bool // App.mu stayed held for the whole call: the caller held it
	overlap bool // another removal or a Notify ran at the same time
}

func (o *removerOS) RemoveToasts(ids ...string) (int, error) {
	overlap := !o.inFlight.CompareAndSwap(false, true)
	defer o.inFlight.Store(false)
	o.shell.mu.Lock()
	shown := len(o.shell.toasts)
	o.shell.mu.Unlock()
	// A caller holding App.mu would keep this goroutine from taking it until the call returns.
	got := make(chan struct{})
	// With App.mu held by the caller, this ends only after the call returned.
	go func() { o.app.mu.Lock(); o.app.mu.Unlock(); close(got) }() //nolint:staticcheck // SA2001: a probe of App.mu
	muBusy := false
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		muBusy = true
	}
	if o.hold > 0 {
		time.Sleep(o.hold)
	}
	o.mu.Lock()
	o.calls = append(o.calls, removal{ids: slices.Clone(ids), shown: shown, muBusy: muBusy, overlap: overlap})
	o.mu.Unlock()
	if o.err != nil {
		return 0, o.err
	}
	return len(ids), nil
}

func (o *removerOS) removals() []removal {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.calls)
}

// removedIDs is every ID removed so far, in order.
func (o *removerOS) removedIDs() []string {
	var out []string
	for _, c := range o.removals() {
		out = append(out, c.ids...)
	}
	return out
}

// waitRemoved waits until the removed IDs are want.
func (o *removerOS) waitRemoved(t *testing.T, want ...string) []removal {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		if got := o.removedIDs(); slices.Equal(got, want) {
			calls := o.removals()
			for _, c := range calls {
				if c.muBusy {
					t.Errorf("RemoveToasts(%v) was called with App.mu held", c.ids)
				}
				if c.overlap {
					t.Errorf("RemoveToasts(%v) overlapped another toast call", c.ids)
				}
			}
			return calls
		}
		if time.Now().After(deadline) {
			t.Fatalf("removed toasts %v, want %v", o.removedIDs(), want)
		}
	}
}

// orderedShell is sysShell whose Notify notes when it runs during a removal.
type orderedShell struct {
	*sysShell
	os      *removerOS
	overlap atomic.Bool
}

func (s *orderedShell) Notify(n Notification) error {
	if s.os.inFlight.Load() {
		s.overlap.Store(true)
	}
	return s.sysShell.Notify(n)
}

// withRemover gives s's app OS hooks with a toast removal, and a shell that checks the toast path's order.
func withRemover(s *sysEnv) (*removerOS, *orderedShell) {
	o := &removerOS{recOS: s.os, app: s.app, shell: s.shell}
	sh := &orderedShell{sysShell: s.shell, os: o}
	s.app.d.OS, s.app.d.Shell = o, sh
	return o, sh
}

// staleIDs is the book's stale IDs without taking them.
func (b *toastBook) staleIDs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.stale)
}

// TestStaleToastRemovedOnResume (WF13-R3): "Resume sharing?" answered by the tray's Resume leaves Action Center,
// once, by its Notification ID, without App.mu held; its buttons no longer act.
func TestStaleToastRemovedOnResume(t *testing.T) {
	s := newSysEnv(t)
	o, _ := withRemover(s)
	n := s.lockLate(t)
	if got := o.removedIDs(); len(got) != 0 {
		t.Fatalf("toasts removed before the question was answered: %v", got)
	}
	s.app.TrayAction(tray.ActionResume)
	s.waitState(t, share.StateLive)
	s.waitRemoved(t, o, n.ID)
	if s.app.ToastAction(n.Nonce, ToastStop) {
		t.Fatal("the removed toast's Stop ran")
	}
	// Later share events find nothing more to remove.
	if err := s.app.shares.SetPaused(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	s.waitState(t, share.StatePaused)
	time.Sleep(5 * share.StateEvery)
	if got := o.removedIDs(); !slices.Equal(got, []string{n.ID}) {
		t.Fatalf("removed %v, want only %s", got, n.ID)
	}
}

// waitRemoved is removerOS.waitRemoved for a sysEnv.
func (s *sysEnv) waitRemoved(t *testing.T, o *removerOS, want ...string) []removal {
	t.Helper()
	return o.waitRemoved(t, want...)
}

// TestStaleToastRemovedOnOutsideResume: a resume through the controller (the panel's), which reaches the app as a
// share event, removes the toast too (desktop-system.spec's case).
func TestStaleToastRemovedOnOutsideResume(t *testing.T) {
	s := newSysEnv(t)
	o, _ := withRemover(s)
	n := s.lockLate(t)
	if err := s.app.shares.SetPaused(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	s.waitState(t, share.StateLive)
	s.waitLockCleared(t)
	s.waitRemoved(t, o, n.ID)
}

// TestStaleToastRemovedOnEnd: the share's end (the tray's Stop) answers the question too.
func TestStaleToastRemovedOnEnd(t *testing.T) {
	s := newSysEnv(t)
	o, _ := withRemover(s)
	n := s.lockLate(t)
	s.app.TrayAction(tray.ActionStop)
	s.waitState(t, share.StateEnded)
	s.waitRemoved(t, o, n.ID)
}

// TestStaleToastRemovedBeforeStoppedLock (WF13-R1): at the second bound the "Resume sharing?" toast leaves Action
// Center before "Sharing stopped while your PC was locked" is shown, and never while it is shown, so the clear and
// deliver-again path of an untagged toast can't take the new toast with it.
func TestStaleToastRemovedBeforeStoppedLock(t *testing.T) {
	s := newSysEnv(t)
	o, sh := withRemover(s)
	o.hold = 20 * time.Millisecond // a slow WinRT round trip widens any overlap
	n := s.lockLate(t)
	s.clock.Advance(24 * time.Minute)
	if !s.timers.fire() {
		t.Fatal("no 30 min timer")
	}
	s.waitState(t, share.StateEnded)
	calls := s.waitRemoved(t, o, n.ID)
	stopped := s.shell.toastsOf(ToastStoppedLock)
	if len(stopped) != 1 {
		t.Fatalf("%d stopped-locked toasts", len(stopped))
	}
	if calls[0].shown != 1 { // only the lock-paused toast had been shown
		t.Errorf("the removal began after %d toasts were shown, want 1 (before stopped-locked)", calls[0].shown)
	}
	if sh.overlap.Load() {
		t.Error("a toast was shown during a removal")
	}
	if slices.Contains(o.removedIDs(), stopped[0].ID) {
		t.Error("the stopped-locked toast was removed")
	}
}

// TestToastPathSerialized: removals and new toasts racing from several goroutines never overlap, and every dropped
// toast is removed exactly once.
func TestToastPathSerialized(t *testing.T) {
	s := newSysEnv(t)
	o, sh := withRemover(s)
	o.hold = time.Millisecond
	const rounds = 20
	var wg sync.WaitGroup
	for range rounds {
		wg.Go(func() {
			s.app.notify(ToastLockPaused, "x", NotificationAction{ID: ToastStop, Title: "Stop"})
			s.app.mu.Lock()
			s.app.toasts.dropKind(ToastLockPaused)
			s.app.mu.Unlock()
			s.app.removeStaleToasts()
		})
	}
	wg.Wait()
	if sh.overlap.Load() {
		t.Error("a toast was shown during a removal")
	}
	var want []string
	for _, n := range s.shell.toastsOf(ToastLockPaused) {
		want = append(want, n.ID)
	}
	got := o.removedIDs()
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("removed %v, want each of %v once", got, want)
	}
	for _, c := range o.removals() {
		if c.overlap || c.muBusy {
			t.Fatalf("removal %+v", c)
		}
	}
}

// TestStaleToastsWithoutRemover: OS hooks without the removal (darwin, linux, a test fake) only settle the toast,
// and the book doesn't keep its ID.
func TestStaleToastsWithoutRemover(t *testing.T) {
	s := newSysEnv(t)
	n := s.lockLate(t)
	s.app.TrayAction(tray.ActionResume)
	s.waitState(t, share.StateLive)
	s.waitLockCleared(t)
	if got := s.app.toasts.staleIDs(); len(got) != 0 {
		t.Fatalf("stale IDs kept without a removal: %v", got)
	}
	if s.app.ToastAction(n.Nonce, ToastStop) {
		t.Fatal("the stale toast's Stop ran")
	}
}

// TestStaleToastRemovalFails: a failing removal is logged and not retried; the toast's buttons stay settled.
func TestStaleToastRemovalFails(t *testing.T) {
	s := newSysEnv(t)
	o, _ := withRemover(s)
	o.err = errors.New("winos: the toast history isn't available")
	n := s.lockLate(t)
	s.app.TrayAction(tray.ActionResume)
	s.waitState(t, share.StateLive)
	s.waitRemoved(t, o, n.ID)
	s.app.notify(ToastStillSharing, "x")
	s.app.removeStaleToasts()
	if got := o.removedIDs(); !slices.Equal(got, []string{n.ID}) {
		t.Fatalf("removals %v: a failed one was retried", got)
	}
	if s.app.ToastAction(n.Nonce, ToastResume) {
		t.Fatal("the stale toast's Resume ran")
	}
}

// TestDropKindKeepsIDs: dropKind marks only the dropped kind's pending toasts stale, by their Notification ID; a
// toast without buttons is never pending, so never stale.
func TestDropKindKeepsIDs(t *testing.T) {
	s := newSysEnv(t)
	s.app.notify(ToastStillSharing, "x")
	s.app.notify(ToastLockPaused, "x", NotificationAction{ID: ToastStop, Title: "Stop"})
	s.app.notify(ToastStoppedLock, "x", NotificationAction{ID: ToastOpen, Title: "Open"})
	s.app.toasts.dropKind(ToastLockPaused)
	lp := s.shell.toastsOf(ToastLockPaused)
	if got := s.app.toasts.staleIDs(); len(lp) != 1 || !slices.Equal(got, []string{lp[0].ID}) {
		t.Fatalf("stale %v, want the lock-paused toast's ID", got)
	}
	if got := s.app.toasts.takeStale(); len(got) != 1 || len(s.app.toasts.staleIDs()) != 0 {
		t.Fatalf("takeStale %v left %v", got, s.app.toasts.staleIDs())
	}
}
