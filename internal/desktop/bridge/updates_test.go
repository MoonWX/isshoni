package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/bridge/codes"
	"github.com/MoonWX/isshoni/internal/update"
)

// WA84: Updates.* over the app's updater (03 §6.2, §16.2) and the update.state event.

// fakeUpdates is the app's UpdateControl.
type fakeUpdates struct {
	mu         sync.Mutex
	st         update.Status
	checkErr   error
	installErr error
	installs   int
	fn         func(update.Status)
	cancelled  bool
}

func (f *fakeUpdates) UpdateStatus() (update.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st, nil
}

func (f *fakeUpdates) CheckUpdate(ctx context.Context) (update.Status, error) {
	if _, ok := ctx.Deadline(); !ok {
		return update.Status{}, errors.New("no deadline")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st, f.checkErr
}

func (f *fakeUpdates) InstallUpdate(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installs++
	return f.installErr
}

func (f *fakeUpdates) SubscribeUpdates(fn func(update.Status)) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fn = fn
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.cancelled = true
	}
}

// TestUpdatesService: Status maps 06's Status (checkedAt RFC 3339, the zero time before a check); Check returns the
// status it left or update.<code>; Install's refusals are busy, update.not_ready, update.not_installed and
// update.<code>; every status change goes out as update.state, until Close.
func TestUpdatesService(t *testing.T) {
	checked := time.Date(2026, 11, 5, 9, 12, 0, 0, time.UTC)
	u := &fakeUpdates{st: update.Status{State: update.StateReady, Current: "0.2.0", Available: "0.3.0", Required: true,
		NotesURL: "https://github.com/MoonWX/isshoni/releases/tag/v0.3.0", CheckedAt: checked}}
	var mu sync.Mutex
	var events []UpdateStatus
	var buf bytes.Buffer
	s, err := New(Deps{App: &fakeCore{}, Servers: &fakeServers{}, Updates: u, Log: testLog(&buf),
		Emit: func(name string, data any) {
			if v, ok := data.(UpdateStatus); ok && name == EventUpdateState {
				mu.Lock()
				events = append(events, v)
				mu.Unlock()
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	want := UpdateStatus{State: "ready", Current: "0.2.0", Available: "0.3.0", Required: true,
		NotesURL: "https://github.com/MoonWX/isshoni/releases/tag/v0.3.0", CheckedAt: "2026-11-05T09:12:00Z"}
	if st, err := s.Updates.Status(); err != nil || st != want {
		t.Fatalf("Status = %+v, %v", st, err)
	}
	if st, err := s.Updates.Check(context.Background()); err != nil || st != want {
		t.Fatalf("Check = %+v, %v", st, err)
	}
	u.fn(update.Status{State: update.StateIdle})
	mu.Lock()
	if len(events) != 1 || events[0].State != "idle" || events[0].CheckedAt != "0001-01-01T00:00:00Z" {
		t.Fatalf("update.state %+v", events)
	}
	mu.Unlock()

	u.checkErr = &update.Error{Code: update.CodeRateLimited, Err: errors.New("429")}
	if _, err := s.Updates.Check(context.Background()); codeOf(t, err) != codes.CodeUpdateRateLimited {
		t.Fatalf("Check failed: %v", err)
	}
	for _, tc := range []struct {
		err  error
		want codes.Code
	}{
		{nil, ""},
		{app.ErrBusy, codes.CodeBusy},
		{app.ErrUpdateNotReady, codes.CodeUpdateNotReady},
		{update.ErrNotInstalled, codes.CodeUpdateNotInstalled},
		{fmt.Errorf("apply: %w", &update.Error{Code: update.CodeHashMismatch}), codes.CodeUpdateHashMismatch},
		{&update.Error{Code: update.CodeInstallFailed}, codes.CodeUpdateInstallFailed},
		{update.ErrUnsupported, codes.CodeNotImplemented},
	} {
		u.installErr = tc.err
		err := s.Updates.Install(context.Background())
		if tc.want == "" {
			if err != nil {
				t.Errorf("Install: %v", err)
			}
			continue
		}
		if codeOf(t, err) != tc.want {
			t.Errorf("Install with %v: %v, want %s", tc.err, err, tc.want)
		}
	}
	s.Close()
	if !u.cancelled {
		t.Fatal("Close didn't unsubscribe from the updater")
	}
}

// TestUpdatesWithoutUpdater: the app without an updater (a second instance, the e2e harness) answers not_implemented
// with the method's name; the services find the app's UpdateControl on Deps.App.
func TestUpdatesWithoutUpdater(t *testing.T) {
	var buf bytes.Buffer
	s, err := New(Deps{App: updatesCore{&fakeCore{}}, Servers: &fakeServers{}, Log: testLog(&buf)})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for name, call := range map[string]func() error{
		"Updates.Status":  func() error { _, err := s.Updates.Status(); return err },
		"Updates.Check":   func() error { _, err := s.Updates.Check(context.Background()); return err },
		"Updates.Install": func() error { return s.Updates.Install(context.Background()) },
	} {
		err := call()
		var be *Error
		if !errors.As(err, &be) || be.Code != codes.CodeNotImplemented || be.Params["method"] != name {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// updatesCore is a Core whose UpdateControl has no updater, as *app.App's without one.
type updatesCore struct{ *fakeCore }

func (updatesCore) UpdateStatus() (update.Status, error) {
	return update.Status{}, app.ErrNotImplemented
}
func (updatesCore) CheckUpdate(context.Context) (update.Status, error) {
	return update.Status{}, app.ErrNotImplemented
}
func (updatesCore) InstallUpdate(context.Context) error                  { return app.ErrNotImplemented }
func (updatesCore) SubscribeUpdates(func(update.Status)) (cancel func()) { return func() {} }
