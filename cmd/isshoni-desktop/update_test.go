//go:build (windows || (darwin && cgo)) && !isshoni_hw

package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/desktop/settings"
	"github.com/MoonWX/isshoni/internal/update"
)

// WA84: 03 §13.4 step 5 (the start-time update with the Pending values), --update-now (06 §8.11) and the logger the
// early parts keep.

// fakeStartUpdater is 06's updater at step 5 and in --update-now.
type fakeStartUpdater struct {
	rec        *[]string
	pending    *update.Pending // what PendingAtStartup answers when not autostart
	pendingErr error
	applyErr   error
	check      update.Status
	checkErr   error
	autostart  []bool
}

func (f *fakeStartUpdater) PendingAtStartup(autostart bool) (*update.Pending, error) {
	*f.rec = append(*f.rec, "pending-at-startup")
	f.autostart = append(f.autostart, autostart)
	if autostart {
		return nil, f.pendingErr // 06: never at an --autostart start
	}
	return f.pending, f.pendingErr
}

func (f *fakeStartUpdater) Apply(ctx context.Context, p *update.Pending, relaunch bool) error {
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("no deadline")
	}
	step := "apply " + p.Version
	if relaunch {
		step += " relaunch"
	}
	*f.rec = append(*f.rec, step)
	return f.applyErr
}

func (f *fakeStartUpdater) CheckNow(context.Context) (update.Status, error) {
	*f.rec = append(*f.rec, "check-now")
	return f.check, f.checkErr
}

func (f *fakeStartUpdater) Ready() *update.Pending {
	if f.check.State != update.StateReady {
		return nil
	}
	return &update.Pending{Version: f.check.Available}
}

func withUpdater(r *startRun, u *fakeStartUpdater) *starter {
	s := newStarter(r)
	u.rec = &r.rec
	s.openUpdater = func() (startUpdater, error) {
		r.rec = append(r.rec, "open-updater")
		return u, nil
	}
	return s
}

// TestStartTimeUpdate (03 §13.4 step 5, 06 §8.7 (b)): with a pending update the first instance writes its link to
// PendingServer and PendingRoom, applies it with relaunch and exits 0 before the Pending values are read or anything
// of the app runs; no window, no connection.
func TestStartTimeUpdate(t *testing.T) {
	r := &startRun{}
	u := &fakeStartUpdater{pending: &update.Pending{Version: "0.3.0"}}
	code := withUpdater(r, u).start([]string{"--open-url", "isshoni://watch.example.com/r/lounge"})
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	want := []string{"installer-probe", "single-instance", "acquire", "serve", "running-mutex", "open-updater",
		"pending-at-startup", "write-pending", "apply 0.3.0 relaunch"}
	if !slices.Equal(r.rec, want) {
		t.Fatalf("steps %v, want %v", r.rec, want)
	}
	if r.registry["PendingServer"] != "https://watch.example.com" || r.registry["PendingRoom"] != "lounge" {
		t.Fatalf("Pending values %v", r.registry)
	}
	if strings.Contains(r.logs.String(), "lounge") {
		t.Fatal("the link's room was logged")
	}
}

// TestStartTimeUpdateForwardedLink: a link a second launch forwarded before step 5 is the newest: it goes to the
// Pending values instead of this start's own (the newest link wins, 03 §13.4 step 2a's rule).
func TestStartTimeUpdateForwardedLink(t *testing.T) {
	r := &startRun{}
	u := &fakeStartUpdater{pending: &update.Pending{Version: "0.3.0"}}
	s := withUpdater(r, u)
	s.holdRunning = func() error {
		r.rec = append(r.rec, "running-mutex")
		r.inst.onArgs([]string{"--open-url", "isshoni://b.example.com/r/later"})
		return nil
	}
	if code := s.start([]string{"--open-url", "isshoni://watch.example.com/r/lounge"}); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if r.registry["PendingServer"] != "https://b.example.com" || r.registry["PendingRoom"] != "later" {
		t.Fatalf("Pending values %v", r.registry)
	}
}

// TestStartTimeUpdateNotApplied: at an --autostart start nothing is applied (the update stays ready for the tray);
// without a pending update, or when Apply fails, the start goes on with the Pending values (those just written
// included) and the app.
func TestStartTimeUpdateNotApplied(t *testing.T) {
	r := &startRun{}
	u := &fakeStartUpdater{pending: &update.Pending{Version: "0.3.0"}}
	if code := withUpdater(r, u).start([]string{"--autostart"}); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if !slices.Equal(u.autostart, []bool{true}) || !slices.Contains(r.rec, "app") ||
		slices.ContainsFunc(r.rec, func(s string) bool { return strings.HasPrefix(s, "apply") }) {
		t.Fatalf("autostart: steps %v, PendingAtStartup(%v)", r.rec, u.autostart)
	}
	if !r.gotArgs.Autostart {
		t.Fatal("the app didn't get --autostart")
	}

	r = &startRun{}
	u = &fakeStartUpdater{}
	if code := withUpdater(r, u).start(nil); code != exitOK || !slices.Contains(r.rec, "app") {
		t.Fatalf("nothing pending: exit %d, steps %v", code, r.rec)
	}

	r = &startRun{}
	u = &fakeStartUpdater{pending: &update.Pending{Version: "0.3.0"}, applyErr: update.ErrNotInstalled,
		pendingErr: &update.Error{Code: update.CodeHashMismatch}}
	if code := withUpdater(r, u).start([]string{"--open-url", "isshoni://watch.example.com/r/lounge"}); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	want := []string{"installer-probe", "single-instance", "acquire", "serve", "running-mutex", "open-updater",
		"pending-at-startup", "write-pending", "apply 0.3.0 relaunch", "take-pending", "app"}
	if !slices.Equal(r.rec, want) {
		t.Fatalf("a failed apply: steps %v, want %v", r.rec, want)
	}

	r = &startRun{}
	s := newStarter(r)
	s.openUpdater = func() (startUpdater, error) { return nil, errors.New("no data folder") }
	if code := s.start(nil); code != exitOK || !slices.Contains(r.rec, "app") {
		t.Fatalf("no updater: exit %d, steps %v", code, r.rec)
	}
}

// TestStartTimeUpdateSecondInstance: a second instance forwards and never builds the updater (06 §8.7).
func TestStartTimeUpdateSecondInstance(t *testing.T) {
	r := &startRun{inst: &fakeInstance{first: false}}
	u := &fakeStartUpdater{pending: &update.Pending{Version: "0.3.0"}}
	if code := withUpdater(r, u).start(nil); code != exitOK || slices.Contains(r.rec, "open-updater") {
		t.Fatalf("exit %d, steps %v", code, r.rec)
	}
}

// TestUpdateNow (06 §8.11): up to date exits 0; a ready update is applied with relaunch (0); a failed check, a
// failed apply or nothing to apply exits 1; without the updatetest tag the mode exits 2 (TestOneShotModesSkipTheStart).
func TestUpdateNow(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	for _, tc := range []struct {
		name  string
		u     fakeStartUpdater
		want  int
		steps []string
	}{
		{"up to date", fakeStartUpdater{check: update.Status{State: update.StateUpToDate}}, exitOK,
			[]string{"check-now"}},
		{"ready", fakeStartUpdater{check: update.Status{State: update.StateReady, Available: "0.0.1-ci.2"}}, exitOK,
			[]string{"check-now", "apply 0.0.1-ci.2 relaunch"}},
		{"apply failed", fakeStartUpdater{check: update.Status{State: update.StateReady, Available: "0.0.1-ci.2"},
			applyErr: &update.Error{Code: update.CodeHashMismatch}}, exitFailed,
			[]string{"check-now", "apply 0.0.1-ci.2 relaunch"}},
		{"check failed", fakeStartUpdater{checkErr: &update.Error{Code: update.CodeNetwork}}, exitFailed,
			[]string{"check-now"}},
		{"disabled", fakeStartUpdater{check: update.Status{State: update.StateDisabled,
			ErrorCode: update.CodeNotRelease}}, exitFailed, []string{"check-now"}},
	} {
		var rec []string
		u := tc.u
		u.rec = &rec
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		code := updateNow(ctx, log, func() (nowUpdater, error) { return &u, nil })
		cancel()
		if code != tc.want || !slices.Equal(rec, tc.steps) {
			t.Errorf("%s: exit %d, steps %v", tc.name, code, rec)
		}
	}
	if code := updateNow(context.Background(), log, func() (nowUpdater, error) {
		return nil, errors.New("no data folder")
	}); code != exitFailed {
		t.Errorf("no updater: exit %d", code)
	}

	// The starter runs the mode it has, as a one-shot mode: nothing of the start.
	r := &startRun{}
	s := newStarter(r)
	s.updateNow = func() int { r.rec = append(r.rec, "update-now"); return exitOK }
	if code := s.start([]string{"--update-now"}); code != exitOK || !slices.Equal(r.rec, []string{"update-now"}) {
		t.Fatalf("--update-now: exit %d, steps %v", code, r.rec)
	}
}

// TestLateLogger: a logger made before the default changes writes to the default of the moment, with its
// attributes and groups.
func TestLateLogger(t *testing.T) {
	prev := slog.Default()
	defer slog.SetDefault(prev)
	log := lateLogger().With("component", "update").WithGroup("g")
	var first, second bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&first, nil)))
	log.Info("one", "k", 1)
	slog.SetDefault(slog.New(slog.NewTextHandler(&second, nil)))
	log.Info("two", "k", 2)
	if !strings.Contains(first.String(), "msg=one component=update g.k=1") ||
		!strings.Contains(second.String(), "msg=two component=update g.k=2") || strings.Contains(first.String(), "two") {
		t.Fatalf("first %q, second %q", first.String(), second.String())
	}
	if lateLogger().Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug enabled at the default's info level")
	}
}

// TestOpenSettingsFromStep5: step 7 goes on with step 5's settings.json, dropping the preferences of servers that
// servers.json doesn't list (Open's KnownServer rule, which step 5 couldn't apply).
func TestOpenSettingsFromStep5(t *testing.T) {
	st, err := settings.Open(settings.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.Update(func(s *settings.Settings) {
		s.ServerPrefs["https://a.example.com"] = settings.ServerPrefs{Preset: "game"}
		s.ServerPrefs["https://gone.example.com"] = settings.ServerPrefs{Preset: "movie"}
	}); err != nil {
		t.Fatal(err)
	}
	got, err := openSettings("", &earlyParts{settings: st}, func(o string) bool { return o == "https://a.example.com" })
	if err != nil || got != st {
		t.Fatalf("openSettings = %p, %v", got, err)
	}
	if prefs := st.Get().ServerPrefs; len(prefs) != 1 || prefs["https://a.example.com"].Preset != "game" {
		t.Fatalf("prefs %v", prefs)
	}
}
