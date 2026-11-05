package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// fakePatternWindow is the noise window: only its affinity matters to the fake capture.
type fakePatternWindow struct {
	mu       sync.Mutex
	affinity uint32
	visible  bool
	hidden   int // SetVisible(false) calls
	closed   bool
	monErr   error
}

func (w *fakePatternWindow) Handle() uintptr { return 0x5150 }
func (w *fakePatternWindow) MonitorID() (string, error) {
	if w.monErr != nil {
		return "", w.monErr
	}
	return `mon:\\.\DISPLAY1`, nil
}

func (w *fakePatternWindow) Affinity() (uint32, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.affinity, nil
}

func (w *fakePatternWindow) Unprotect() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.affinity = 0
	return nil
}

func (w *fakePatternWindow) SetVisible(v bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.visible = v
	if !v {
		w.hidden++
	}
}

func (w *fakePatternWindow) Close() { w.closed = true }

func (w *fakePatternWindow) protected() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.affinity == wdaExcludeFromCapture
}

// fakeProtectEngine captures the fake window: while it is protected (and the engine isn't leaky), its noise is left
// out and inter frames are tiny; otherwise each costs noisyBytes.
type fakeProtectEngine struct {
	w          *fakePatternWindow
	ignore     bool // ProtectWindow does nothing
	leaky      bool // the capture shows the protected window
	covered    bool // the window never reaches the capture
	pick       media.Source
	pickErr    error
	startErr   error
	mu         sync.Mutex
	protects   int
	picks      int
	released   []string
	configs    []media.Config
	sessionErr error // the session ends with it after its first packets
}

const noisyBytes = 20000 // × 200 frames/s × 8 = 32 Mbit/s

func (e *fakeProtectEngine) ProtectWindow(hwnd uintptr) error {
	e.mu.Lock()
	e.protects++
	e.mu.Unlock()
	if hwnd != e.w.Handle() {
		return &media.Error{Code: media.CodeInvalid, Op: "protect_window"}
	}
	if !e.ignore {
		e.w.mu.Lock()
		e.w.affinity = wdaExcludeFromCapture
		e.w.mu.Unlock()
	}
	return nil
}

func (e *fakeProtectEngine) Pick(context.Context, media.PickRequest) (media.Source, error) {
	e.mu.Lock()
	e.picks++
	e.mu.Unlock()
	return e.pick, e.pickErr
}

func (e *fakeProtectEngine) Release(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.released = append(e.released, id)
}

func (e *fakeProtectEngine) Start(_ context.Context, cfg media.Config) (media.Session, error) {
	e.mu.Lock()
	e.configs = append(e.configs, cfg)
	e.mu.Unlock()
	if e.startErr != nil {
		return nil, e.startErr
	}
	return &fakeProtectSession{e: e, t0: time.Now(), stopped: make(chan struct{})}, nil
}

type fakeProtectSession struct {
	fakeSession
	e       *fakeProtectEngine
	t0      time.Time
	n       int
	stopped chan struct{}
	once    sync.Once
}

// annexB is an access unit of the given NAL units, each a type and a payload size.
func annexB(units ...[2]int) []byte {
	var b []byte
	for _, u := range units {
		b = append(b, 0, 0, 0, 1, byte(u[0]))
		b = append(b, bytes.Repeat([]byte{0xAB}, u[1])...)
	}
	return b
}

func (s *fakeProtectSession) Next(ctx context.Context) (media.Packet, error) {
	if s.n == 0 {
		s.n++
		return media.Packet{Kind: media.KindEvent, Event: &media.Event{Code: media.EventLayer, Layer: "f",
			Encoder: "mf-sw"}}, nil
	}
	t := time.NewTimer(5 * time.Millisecond)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return media.Packet{}, ctx.Err()
	case <-s.stopped:
		return media.Packet{}, media.ErrStopped
	case <-t.C:
	}
	s.n++
	if s.e.sessionErr != nil && s.n > 5 {
		return media.Packet{}, s.e.sessionErr
	}
	p := media.Packet{Kind: media.KindVideo, Layer: "f", CaptureNS: s.Now()}
	switch {
	case s.n%20 == 0: // a periodic IDR: big, and never counted
		p.Keyframe = true
		p.Data = annexB([2]int{0x67, 10}, [2]int{0x68, 4}, [2]int{0x65, 200000})
	case s.e.covered || (s.e.w.protected() && !s.e.leaky):
		// a still desktop: a tiny slice, plus filler and SEI that don't count
		p.Data = annexB([2]int{0x06, 50000}, [2]int{0x41, 40}, [2]int{0x0C, 50000})
	default:
		p.Data = annexB([2]int{0x41, noisyBytes})
	}
	return p, nil
}

func (s *fakeProtectSession) Now() int64 { return int64(time.Since(s.t0)) + 1 }

func (s *fakeProtectSession) Stop() error {
	s.once.Do(func() { close(s.stopped) })
	return nil
}

func fastProtectOptions(wgc string) protectOptions {
	return protectOptions{seconds: 0.25, warmup: 60 * time.Millisecond, settle: 60 * time.Millisecond, wgc: wgc}
}

func runFakeProtect(t *testing.T, e *fakeProtectEngine, o protectOptions) (int, protectReport, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runProtectCheck(context.Background(), e, e.w, o, true, &out, &errb)
	var rep protectReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("the JSON report: %v\n%s\nstderr: %s", err, out.String(), errb.String())
	}
	return code, rep, out.String()
}

func newFakeProtect() *fakeProtectEngine {
	w := &fakePatternWindow{visible: true}
	return &fakeProtectEngine{w: w, pick: media.Source{ID: "pick:3", Kind: media.SourceScreen, Capture: "wgc",
		Name: "SECRET MONITOR NAME"}}
}

func TestProtectCheckPasses(t *testing.T) {
	e := newFakeProtect()
	code, rep, out := runFakeProtect(t, e, fastProtectOptions("pick"))
	if code != exitPass || !rep.Pass || len(rep.Paths) != 2 {
		t.Fatalf("code %d, report %s", code, out)
	}
	for i, want := range []string{"dd", "wgc"} {
		p := rep.Paths[i]
		if p.Path != want || p.Outcome != "pass" || p.Encoder != "mf-sw" || p.Protected == 0 || p.Shown == 0 ||
			p.ShownBps < 10*p.ProtectedBp {
			t.Errorf("path %d: %+v", i, p)
		}
	}
	if e.protects != 2 || e.picks != 1 || len(e.released) != 0 {
		t.Errorf("%d protects, %d picks, released %v", e.protects, e.picks, e.released)
	}
	if len(e.configs) != 2 || e.configs[0].Source != `mon:\\.\DISPLAY1` || !e.configs[0].Listed ||
		e.configs[1].Source != "pick:3" || e.configs[1].Listed {
		t.Errorf("configs %+v", e.configs)
	}
	for _, c := range e.configs {
		if len(c.Layers) != 1 || c.Layers[0].RID != "f" || c.Layers[0].Bitrate != protectBitrate || !c.Borderless {
			t.Errorf("layers %+v", c)
		}
	}
	if e.w.hidden != 1 || !e.w.visible {
		t.Errorf("the window was hidden %d times, visible %t at the end", e.w.hidden, e.w.visible)
	}
	if e.w.protected() {
		t.Error("the window is still protected after the run")
	}
	if strings.Contains(out, "SECRET") || strings.Contains(out, "DISPLAY") {
		t.Errorf("the report names a monitor: %s", out)
	}
}

func TestProtectCheckFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(e *fakeProtectEngine)
		code   int
		reason string
	}{
		{"affinity not set", func(e *fakeProtectEngine) { e.ignore = true }, exitFail, "affinity reads 0x0"},
		{"shown while protected", func(e *fakeProtectEngine) { e.leaky = true }, exitFail, "present while protected"},
		{"never shown", func(e *fakeProtectEngine) { e.covered = true }, exitFail, "not present after"},
		{"source gone", func(e *fakeProtectEngine) {
			e.startErr = &media.Error{Code: media.CodeSourceGone, Op: "start"}
		}, exitEnv, "start:"},
		{"session ends", func(e *fakeProtectEngine) {
			e.sessionErr = &media.Error{Code: media.CodeInternal, Op: "next", Reason: "device_removed"}
		}, exitFail, "the session ended"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFakeProtect()
			tc.setup(e)
			code, rep, out := runFakeProtect(t, e, fastProtectOptions("off"))
			if code != tc.code || rep.Pass || !strings.Contains(rep.Paths[0].Reason, tc.reason) {
				t.Errorf("code %d, want %d; report %s", code, tc.code, out)
			}
			if e.w.protected() && !e.ignore {
				t.Error("the window is still protected after the run")
			}
		})
	}
}

func TestProtectCheckWGC(t *testing.T) {
	// Windows 10: the monitor pick maps to DD, so WGC is not reachable; the token is released and the run passes.
	e := newFakeProtect()
	e.pick.Capture = "dd"
	code, rep, out := runFakeProtect(t, e, fastProtectOptions("pick"))
	if code != exitPass || rep.Paths[1].Outcome != "not_reachable" || rep.Paths[1].Capture != "dd" ||
		len(e.released) != 1 || e.released[0] != "pick:3" || len(e.configs) != 1 {
		t.Errorf("code %d, released %v, report %s", code, e.released, out)
	}

	// A cancelled pick: incomplete, exit 2.
	e = newFakeProtect()
	e.pickErr = media.ErrCancelled
	if code, rep, out = runFakeProtect(t, e, fastProtectOptions("pick")); code != exitEnv ||
		rep.Paths[1].Outcome != "cancelled" {
		t.Errorf("cancelled: code %d, report %s", code, out)
	}

	// A window pick is no monitor: released, and an error.
	e = newFakeProtect()
	e.pick.Kind = media.SourceWindow
	if code, rep, out = runFakeProtect(t, e, fastProtectOptions("pick")); code != exitEnv ||
		rep.Paths[1].Outcome != "error" || len(e.released) != 1 {
		t.Errorf("window pick: code %d, report %s", code, out)
	}

	// A failed start of a pick releases the token.
	e = newFakeProtect()
	e.startErr = &media.Error{Code: media.CodeNoEncoder, Op: "start"}
	if code, _, out = runFakeProtect(t, e, fastProtectOptions("pick")); code != exitEnv || len(e.released) != 1 {
		t.Errorf("start failure: code %d, released %v, report %s", code, e.released, out)
	}

	// -wgc off: no pick.
	e = newFakeProtect()
	if code, rep, out = runFakeProtect(t, e, fastProtectOptions("off")); code != exitPass || e.picks != 0 ||
		rep.Paths[1].Outcome != "off" {
		t.Errorf("off: code %d, %d picks, report %s", code, e.picks, out)
	}

	// No monitor for the window: the DD path is an environment error.
	e = newFakeProtect()
	e.w.monErr = context.DeadlineExceeded
	if code, rep, out = runFakeProtect(t, e, fastProtectOptions("off")); code != exitEnv ||
		rep.Paths[0].Outcome != "error" {
		t.Errorf("monitor: code %d, report %s", code, out)
	}
}

func TestProtectMeasures(t *testing.T) {
	au := annexB([2]int{0x09, 1}, [2]int{0x06, 30}, [2]int{0x41, 100}, [2]int{0x41, 50}, [2]int{0x0C, 999})
	if got := sliceBytes(au); got != 152 { // two slices, each with its header byte
		t.Errorf("sliceBytes = %d", got)
	}
	if got := sliceBytes(annexB([2]int{0x65, 10})); got != 11 {
		t.Errorf("an IDR slice: %d", got)
	}
	samples := []protectSample{{0, 1000}, {int64(time.Second / 2), 1000}, {int64(time.Second), 5000}}
	if bps, n := phaseRate(samples, 0, int64(time.Second)); bps != 16000 || n != 2 {
		t.Errorf("phaseRate = %v, %d", bps, n)
	}
	if bps, n := phaseRate(samples, 5, 5); bps != 0 || n != 0 {
		t.Errorf("an empty phase: %v, %d", bps, n)
	}
	for _, tc := range []struct {
		prot, shown float64
		ok          bool
	}{
		{0, protectMinShownBps, true},
		{protectMaxRatio * 4e6, 4e6, true},
		{protectMaxRatio*4e6 + 1, 4e6, false},
		{0, protectMinShownBps - 1, false},
	} {
		if ok, why := protectVerdict(tc.prot, tc.shown); ok != tc.ok {
			t.Errorf("protectVerdict(%v, %v) = %t (%s)", tc.prot, tc.shown, ok, why)
		}
	}
}

func TestProtectCheckFlags(t *testing.T) {
	for _, args := range [][]string{{"-seconds", "0.5"}, {"-seconds", "61"}, {"-wgc", "maybe"}, {"extra"}} {
		var out, errb bytes.Buffer
		if code := run(context.Background(), append([]string{"protect-check"}, args...), &out, &errb); code != exitUsage {
			t.Errorf("%v: code %d", args, code)
		}
	}
}
