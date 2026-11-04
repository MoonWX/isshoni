package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/native"
)

// fakeSystem is the PC of the sessions tests: NVENC with a per-system limit shared by every process (M-NVENC-LIMIT:
// a layer past it gets MF software, 04 §9.7), the starts that fail, and how many sessions ran at once.
type fakeSystem struct {
	mu        sync.Mutex
	nvenc     int // NVENC sessions left
	starts    int
	failAt    map[int]error // start number (from 1) → its error
	live      int
	maxLive   int
	lastStops int
}

func (f *fakeSystem) StartExtra(_ context.Context, cfg media.Config, _ ...native.StartOption) (media.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	if err := f.failAt[f.starts]; err != nil {
		return nil, err
	}
	if cfg.Source != "test:pattern" || len(cfg.Layers) != 2 || cfg.Layers[0].MaxFPS != 60 ||
		cfg.Layers[0].MaxPixels != 1920*1080 || cfg.Audio != nil {
		return nil, &media.Error{Code: media.CodeInvalid, Op: "start_extra", Detail: "not sessions' config"}
	}
	s := &fakeSession{sys: f, stopped: make(chan struct{})}
	for _, l := range cfg.Layers {
		enc := "mf-sw"
		if f.nvenc > 0 {
			f.nvenc--
			enc = "nvenc"
		}
		s.events = append(s.events, &media.Event{Code: media.EventLayer, Layer: l.RID, Encoder: enc, HW: enc == "nvenc",
			Profile: "6400", Width: 1920, Height: 1080, FPS: l.MaxFPS})
	}
	f.live++
	f.maxLive = max(f.maxLive, f.live)
	return s, nil
}

type fakeSession struct {
	sys     *fakeSystem
	mu      sync.Mutex
	events  []*media.Event
	stopped chan struct{}
	once    sync.Once
}

func (s *fakeSession) Next(ctx context.Context) (media.Packet, error) {
	s.mu.Lock()
	if len(s.events) > 0 {
		ev := s.events[0]
		s.events = s.events[1:]
		s.mu.Unlock()
		return media.Packet{Kind: media.KindEvent, Event: ev}, nil
	}
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return media.Packet{}, ctx.Err()
	case <-s.stopped:
		return media.Packet{}, media.ErrStopped
	}
}

func (s *fakeSession) Now() int64                        { return 0 }
func (s *fakeSession) RequestKeyframe(string)            {}
func (s *fakeSession) SetBitrate(string, int) error      { return nil }
func (s *fakeSession) SetLayerActive(string, bool) error { return nil }
func (s *fakeSession) SetPaused(bool) error              { return nil }
func (s *fakeSession) Reconfigure(media.Reconfig) error  { return nil }
func (s *fakeSession) Stats() (media.EngineStats, error) { return media.EngineStats{}, nil }
func (s *fakeSession) Stop() error {
	s.once.Do(func() {
		close(s.stopped)
		s.sys.mu.Lock()
		s.sys.live--
		s.sys.lastStops++
		s.sys.mu.Unlock()
	})
	return nil
}

// inProcessHelper runs a helper's sessions in this test process on the same fake PC: it parses the helper's arguments
// as the real one would and answers with its JSON report.
func inProcessHelper(t *testing.T, sys *fakeSystem, calls *[][]string, mu *sync.Mutex) helperRunner {
	return func(ctx context.Context, args []string) ([]byte, int, error) {
		mu.Lock()
		*calls = append(*calls, args)
		mu.Unlock()
		if len(args) < 2 || args[0] != "-json" || args[1] != "sessions" {
			t.Errorf("helper args %q: want -json sessions …", args)
			return nil, 0, errors.New("bad args")
		}
		fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		o := sessionsOptions{}
		fs.IntVar(&o.count, "count", 0, "")
		fs.Float64Var(&o.seconds, "seconds", 0, "")
		fs.StringVar(&o.encoder, "encoder", "", "")
		fs.StringVar(&o.profile, "profile", "", "")
		fs.BoolVar(&o.child, "child", false, "")
		fs.IntVar(&o.first, "first", 0, "")
		if err := fs.Parse(args[2:]); err != nil {
			return nil, 0, err
		}
		if !o.child {
			t.Errorf("helper args %q: no -child", args)
		}
		if _, ok := checkSessionsOptions(io.Discard, o); !ok {
			t.Errorf("helper args %q: the helper would refuse them", args)
		}
		var out bytes.Buffer
		code := runSessionsCommand(ctx, sys, o, nil, true, &out, io.Discard)
		return out.Bytes(), code, nil
	}
}

func decodeReport(t *testing.T, data []byte) sessionsReport {
	t.Helper()
	var rep sessionsReport
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatalf("report: %v\n%s", err, data)
	}
	return rep
}

// M-NVENC-LIMIT on a fake PC with 7 NVENC sessions left (OBS holds one of 8): 5 sessions of two layers, 4 here and
// 1 in a helper process, all running at once; every session starts, 7 layers report NVENC and 3 MF software.
func TestSessionsNvencLimit(t *testing.T) {
	sys := &fakeSystem{nvenc: 7}
	var calls [][]string
	var mu sync.Mutex
	var out bytes.Buffer
	o := sessionsOptions{count: 5, seconds: 0.2, encoder: "auto", profile: "high"}
	code := runSessionsCommand(context.Background(), sys, o, inProcessHelper(t, sys, &calls, &mu), true, &out,
		io.Discard)
	if code != exitPass {
		t.Fatalf("exit %d, want %d\n%s", code, exitPass, out.String())
	}
	rep := decodeReport(t, out.Bytes())
	if !rep.Pass || rep.Started != 5 || len(rep.Sessions) != 5 {
		t.Fatalf("report %+v: want 5 started", rep)
	}
	if rep.Encoders["nvenc"] != 7 || rep.Encoders["mf-sw"] != 3 {
		t.Errorf("encoders %v: want nvenc 7, mf-sw 3", rep.Encoders)
	}
	for i, r := range rep.Sessions {
		wantProcess := 0
		if i >= maxSessionsPerProcess {
			wantProcess = 1
		}
		if r.Index != i || r.Process != wantProcess || len(r.Layers) != 2 || r.Layers[0].RID != "f" {
			t.Errorf("session %d: %+v, want index %d in process %d with f and q", i, r, i, wantProcess)
		}
	}
	if len(calls) != 1 || !strings.Contains(strings.Join(calls[0], " "), "-child -first 4 -count 1 -seconds 0.2") {
		t.Errorf("helpers %q: want one for session 4", calls)
	}
	if sys.maxLive != 5 {
		t.Errorf("at most %d sessions ran at once, want all 5", sys.maxLive)
	}
	if sys.live != 0 {
		t.Errorf("%d sessions still run", sys.live)
	}
}

// 9 sessions: 4 here, then helpers of 4 and 1, started together; the text output lists every session.
func TestSessionsSpreadOverHelpers(t *testing.T) {
	sys := &fakeSystem{nvenc: 100}
	var calls [][]string
	var mu sync.Mutex
	var out bytes.Buffer
	o := sessionsOptions{count: 9, seconds: 0.2, encoder: "auto", profile: "high"}
	code := runSessionsCommand(context.Background(), sys, o, inProcessHelper(t, sys, &calls, &mu), false, &out,
		io.Discard)
	if code != exitPass {
		t.Fatalf("exit %d\n%s", code, out.String())
	}
	if len(calls) != 2 {
		t.Fatalf("helpers %q: want 2", calls)
	}
	if sys.maxLive != 9 {
		t.Errorf("at most %d sessions ran at once, want 9", sys.maxLive)
	}
	text := out.String()
	for _, want := range []string{"session 0 (process 0): f nvenc, q nvenc", "session 4 (process 1)",
		"session 8 (process 2)", "9 of 9 sessions started; layers per encoder: nvenc 18", "PASS"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
}

// A session that doesn't start fails the run (exit 1); when none starts, the error's environment code decides.
func TestSessionsStartFailures(t *testing.T) {
	limit := &media.Error{Code: media.CodeBusy, Op: "start_extra", Detail: "session_limit"}
	sys := &fakeSystem{nvenc: 8, failAt: map[int]error{2: limit}}
	var out bytes.Buffer
	o := sessionsOptions{count: 3, seconds: 0.1, encoder: "auto", profile: "high"}
	if code := runSessionsCommand(context.Background(), sys, o, nil, true, &out, io.Discard); code != exitFail {
		t.Fatalf("exit %d, want %d", code, exitFail)
	}
	rep := decodeReport(t, out.Bytes())
	if rep.Pass || rep.Started != 2 || rep.Sessions[1].Started || rep.Sessions[1].Code != string(media.CodeBusy) {
		t.Errorf("report %+v: want session 1 not started (busy)", rep)
	}

	noEnc := &media.Error{Code: media.CodeNoEncoder, Op: "start_extra"}
	sys = &fakeSystem{failAt: map[int]error{1: noEnc, 2: noEnc}}
	out.Reset()
	o.count = 2
	if code := runSessionsCommand(context.Background(), sys, o, nil, true, &out, io.Discard); code != exitEnv {
		t.Fatalf("no session started for want of an encoder: exit %d, want %d", code, exitEnv)
	}
}

// A helper that doesn't run or answers no report fails the run; its sessions count as not started.
func TestSessionsHelperFailure(t *testing.T) {
	sys := &fakeSystem{nvenc: 100}
	var out bytes.Buffer
	o := sessionsOptions{count: 6, seconds: 0.1, encoder: "auto", profile: "high"}
	broken := func(context.Context, []string) ([]byte, int, error) { return []byte("not json"), exitFail, nil }
	if code := runSessionsCommand(context.Background(), sys, o, broken, true, &out, io.Discard); code != exitFail {
		t.Fatalf("exit %d, want %d", code, exitFail)
	}
	rep := decodeReport(t, out.Bytes())
	if rep.Started != 4 || len(rep.Sessions) != 6 || rep.Sessions[5].Started || rep.Sessions[5].Index != 5 {
		t.Errorf("report %+v: want sessions 4 and 5 not started", rep)
	}
	if len(rep.Failures) == 0 || !strings.Contains(strings.Join(rep.Failures, "\n"), "helper process 1") {
		t.Errorf("failures %q: want the helper's", rep.Failures)
	}
}

func TestSessionsUsage(t *testing.T) {
	for _, args := range [][]string{
		{"sessions", "-count", "0"},
		{"sessions", "-count", "17"},
		{"sessions", "-child", "-count", "5"},
		{"sessions", "-seconds", "0"},
		{"sessions", "-encoder", "x264"},
		{"sessions", "-profile", "main"},
		{"sessions", "extra"},
	} {
		if code, _, stderr := runArgs(t, args...); code != exitUsage {
			t.Errorf("%q: exit %d, want %d\n%s", args, code, exitUsage, stderr)
		}
	}
	if _, _, stderr := runArgs(t); !strings.Contains(stderr, "sessions") {
		t.Errorf("the usage text lists no sessions:\n%s", stderr)
	}
	if code, _, stderr := runArgs(t, "-dll", t.TempDir(), "sessions", "-seconds", "0.1"); code != exitEnv {
		t.Errorf("without a DLL: exit %d, want %d\n%s", code, exitEnv, stderr)
	}
}
