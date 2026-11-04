package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/native"
)

// fakePicker answers Pick from a script of outcomes and records what it was asked.
type fakePicker struct {
	outcomes []func() (media.Source, error)
	details  map[string]native.SourceDetail
	requests []media.PickRequest
	released []string
}

func (f *fakePicker) Pick(_ context.Context, req media.PickRequest) (media.Source, error) {
	f.requests = append(f.requests, req)
	next := f.outcomes[0]
	f.outcomes = f.outcomes[1:]
	return next()
}

func (f *fakePicker) SourceDetail(id string) (native.SourceDetail, bool) {
	d, ok := f.details[id]
	return d, ok
}

func (f *fakePicker) Release(id string) { f.released = append(f.released, id) }

func pickedWindow() (media.Source, error) {
	return media.Source{ID: "pick:2", Kind: media.SourceWindow, Name: "Secret Title", Width: 1280, Height: 720,
		Capture: "wgc", PID: 42}, nil
}

func pickedMonitor() (media.Source, error) {
	return media.Source{ID: "pick:3", Kind: media.SourceScreen, Name: "Monitor Name", Width: 2560, Height: 1440,
		Capture: "wgc"}, nil
}

func cancelledPick() (media.Source, error) {
	return media.Source{}, &media.Error{Code: media.CodeCancelled, Op: "pick", Detail: "the user closed the picker"}
}

// M-PICK's sequence: cancel, a window, a monitor. Names stay out unless -name.
func TestPickSequence(t *testing.T) {
	f := &fakePicker{
		outcomes: []func() (media.Source, error){cancelledPick, pickedWindow, pickedMonitor},
		details:  map[string]native.SourceDetail{"pick:2": {Window: 0x1234}},
	}
	var out, errOut bytes.Buffer
	code := runPicks(context.Background(), f, pickOptions{count: 3, timeout: 30 * time.Second}, false, &out, &errOut)
	if code != exitPass {
		t.Fatalf("exit %d, want %d\n%s", code, exitPass, errOut.String())
	}
	text := out.String()
	for _, want := range []string{
		"cancelled",
		"picked pick:2: window 1280x720, capture wgc, os border false, mapped false, identified true, pid 42",
		"picked pick:3: screen 2560x1440, capture wgc",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Secret Title") || strings.Contains(text, "Monitor Name") {
		t.Errorf("a name was printed without -name:\n%s", text)
	}
	if len(f.requests) != 3 || f.requests[0].Timeout != 30*time.Second || f.requests[0].OwnerWindow != 0 {
		t.Errorf("requests %+v: want 3 with the timeout and no owner window (the engine's own, 04 §7.2)", f.requests)
	}
	if strings.Join(f.released, ",") != "pick:2,pick:3" {
		t.Errorf("released %v, want both picks", f.released)
	}
}

func TestPickJSONWithName(t *testing.T) {
	f := &fakePicker{outcomes: []func() (media.Source, error){pickedWindow}}
	var out, errOut bytes.Buffer
	if code := runPicks(context.Background(), f, pickOptions{count: 1, name: true}, true, &out, &errOut); code != exitPass {
		t.Fatalf("exit %d\n%s", code, errOut.String())
	}
	var got []pickResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(got) != 1 || got[0].Outcome != "picked" || got[0].Name != "Secret Title" || got[0].Kind != "window" ||
		got[0].Identified {
		t.Errorf("got %+v", got)
	}
}

func TestPickExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		outcomes []func() (media.Source, error)
		want     int
	}{
		{"all cancelled", []func() (media.Source, error){cancelledPick, cancelledPick}, exitEnv},
		{"self window", []func() (media.Source, error){func() (media.Source, error) {
			return media.Source{}, &media.Error{Code: media.CodeInvalid, Op: "pick", Reason: "self_window"}
		}}, exitFail},
		{"no picker", []func() (media.Source, error){func() (media.Source, error) {
			return media.Source{}, &media.Error{Code: media.CodeUnsupportedOS, Op: "pick", Reason: "os_too_old"}
		}}, exitEnv},
		{"busy then picked", []func() (media.Source, error){func() (media.Source, error) {
			return media.Source{}, &media.Error{Code: media.CodeInternal, Op: "pick", Reason: "pick_busy"}
		}, pickedWindow}, exitFail},
	} {
		f := &fakePicker{outcomes: tc.outcomes}
		var out, errOut bytes.Buffer
		if code := runPicks(context.Background(), f, pickOptions{count: len(tc.outcomes)}, false, &out,
			&errOut); code != tc.want {
			t.Errorf("%s: exit %d, want %d\n%s%s", tc.name, code, tc.want, out.String(), errOut.String())
		}
	}
}

func TestPickUsage(t *testing.T) {
	for _, args := range [][]string{
		{"pick", "-count", "0"},
		{"pick", "-count", "21"},
		{"pick", "-timeout", "11m"},
		{"pick", "-timeout", "-1s"},
		{"pick", "extra"},
	} {
		if code, _, stderr := runArgs(t, args...); code != exitUsage {
			t.Errorf("%q: exit %d, want %d\n%s", args, code, exitUsage, stderr)
		}
	}
	// Without an engine DLL in the folder (or off Windows) pick is an environment failure.
	if code, _, stderr := runArgs(t, "-dll", t.TempDir(), "pick"); code != exitEnv {
		t.Errorf("no engine: exit %d, want %d\n%s", code, exitEnv, stderr)
	}
}
