//go:build (windows || (darwin && cgo)) && !isshoni_hw

package main

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/desktop/winos"
)

// withRelaunch gives a test starter step 0's helper and step 2's probes, recording them.
func withRelaunch(s *starter, r *startRun, running bool, relaunched bool, err error) *starter {
	s.soundCheckHelper = func() int { r.rec = append(r.rec, "sound-check-helper"); return 4 }
	s.runningExists = func() (bool, error) { r.rec = append(r.rec, "running-probe"); return running, nil }
	s.shellRelaunch = func(args []string) (bool, error) {
		r.rec = append(r.rec, "relaunch:"+strings.Join(args, " "))
		return relaunched, err
	}
	return s
}

// TestSoundCheckHelperDispatch is 03 §13.4 step 0 (README WA77): --sound-check-helper first runs 05's helper and
// exits with its code, before every other step, the relaunch and the one-shot modes included; anywhere else it is no
// helper start.
func TestSoundCheckHelperDispatch(t *testing.T) {
	r := &startRun{installer: true}
	s := withRelaunch(newStarter(r), r, false, true, nil)
	if code := s.start([]string{"--sound-check-helper", "--version"}); code != 4 {
		t.Fatalf("exit %d, want the helper's 4", code)
	}
	if want := []string{"sound-check-helper"}; !slices.Equal(r.rec, want) {
		t.Errorf("steps %v, want %v", r.rec, want)
	}
	r = &startRun{}
	s = withRelaunch(newStarter(r), r, false, false, nil)
	_ = s.start([]string{"--updated-from=0.2.0", "--sound-check-helper"})
	if slices.Contains(r.rec, "sound-check-helper") {
		t.Errorf("a later --sound-check-helper ran the helper: %v", r.rec)
	}
}

// TestShellRelaunch is 03 §13.4 step 2: after the one-shot modes and before step 2a, a start relaunches under the
// shell with its arguments unchanged and exits 0 when that worked; --relaunched and a running instance skip it; a
// failure is logged and the start goes on in place.
func TestShellRelaunch(t *testing.T) {
	link := "isshoni://watch.example.com/r/lounge"

	r := &startRun{}
	s := withRelaunch(newStarter(r), r, false, true, nil)
	if code := s.start([]string{"--open-url", link}); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if want := []string{"running-probe", "relaunch:--open-url " + link}; !slices.Equal(r.rec, want) {
		t.Errorf("relaunched: steps %v, want %v", r.rec, want)
	}

	r = &startRun{}
	s = withRelaunch(newStarter(r), r, false, false, nil)
	_ = s.start(nil)
	if want := []string{"running-probe", "relaunch:", "installer-probe", "single-instance", "acquire", "serve",
		"running-mutex", "take-pending", "app"}; !slices.Equal(r.rec, want) {
		t.Errorf("no relaunch needed: steps %v, want %v", r.rec, want)
	}

	r = &startRun{}
	s = withRelaunch(newStarter(r), r, false, true, nil)
	_ = s.start([]string{winos.RelaunchedFlag, "--open-url", link})
	if slices.ContainsFunc(r.rec, func(step string) bool { return strings.HasPrefix(step, "relaunch") }) ||
		slices.Contains(r.rec, "running-probe") || !slices.Contains(r.rec, "app") {
		t.Errorf("--relaunched: steps %v", r.rec)
	}
	if r.gotArgs.Link != link {
		t.Errorf("--relaunched: the app got %+v", r.gotArgs)
	}

	r = &startRun{}
	s = withRelaunch(newStarter(r), r, true, true, nil)
	_ = s.start([]string{"--open-url", link})
	if slices.ContainsFunc(r.rec, func(step string) bool { return strings.HasPrefix(step, "relaunch") }) ||
		!slices.Contains(r.rec, "single-instance") {
		t.Errorf("a running instance: steps %v", r.rec)
	}

	r = &startRun{}
	s = withRelaunch(newStarter(r), r, false, false, winos.ErrNoShell)
	if code := s.start(nil); code != exitOK || !slices.Contains(r.rec, "app") {
		t.Errorf("a failed relaunch: exit %d, steps %v", code, r.rec)
	}
	if !strings.Contains(r.logs.String(), "not relaunched") {
		t.Errorf("a failed relaunch wasn't logged: %s", r.logs.String())
	}

	// The one-shot modes come first: they never relaunch.
	r = &startRun{}
	s = withRelaunch(newStarter(r), r, false, true, errors.New("unused"))
	_ = s.start([]string{"--version"})
	if slices.ContainsFunc(r.rec, func(step string) bool { return strings.HasPrefix(step, "relaunch") }) {
		t.Errorf("--version: steps %v", r.rec)
	}
}
