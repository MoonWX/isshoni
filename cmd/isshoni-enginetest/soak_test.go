package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// soakSession is a session that sends nothing until it is stopped, or ends with ended once endAfter passed.
type soakSession struct {
	media.Session // the methods soak doesn't call panic
	stopped       chan struct{}
	once          sync.Once
	ended         error
	endAfter      time.Duration
	stops         atomic.Int32
}

func newSoakSession() *soakSession { return &soakSession{stopped: make(chan struct{})} }

func (s *soakSession) Next(ctx context.Context) (media.Packet, error) {
	var end <-chan time.Time
	if s.ended != nil {
		end = time.After(s.endAfter)
	}
	select {
	case <-ctx.Done():
		return media.Packet{}, ctx.Err()
	case <-s.stopped:
		return media.Packet{}, media.ErrStopped
	case <-end:
		return media.Packet{}, s.ended
	}
}

func (s *soakSession) Stats() (media.EngineStats, error) {
	return media.EngineStats{Layers: []media.EngineLayerStats{{RID: "f", Encoder: "nvenc", FPSOut: 60}}}, nil
}

func (s *soakSession) Stop() error {
	s.stops.Add(1)
	s.once.Do(func() { close(s.stopped) })
	return nil
}

// countingSampler returns samples whose private bytes grow by step per call.
type countingSampler struct {
	mu    sync.Mutex
	calls uint64
	step  uint64
	gdi   uint64
}

func (c *countingSampler) sample() (procSample, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return procSample{PrivateBytes: 100_000_000 + c.calls*c.step, Handles: 400, GDIObjects: c.gdi, Threads: 30},
		nil
}

func shortSoak(t *testing.T) soakOptions {
	t.Helper()
	return soakOptions{hours: (400 * time.Millisecond).Hours(), every: 30 * time.Millisecond,
		warmup: 100 * time.Millisecond, source: "test:pattern", encoder: "auto", profile: "high", audio: "tone",
		out: t.TempDir()}
}

func runShortSoak(t *testing.T, env soakEnv, o soakOptions) (int, soakReport) {
	t.Helper()
	cfg, err := soakConfig(o)
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := runSoak(context.Background(), env, o, cfg, true, &out, &errOut)
	var rep soakReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("exit %d, stdout is no report: %v\n%s\n%s", code, err, out.String(), errOut.String())
	}
	file, err := os.ReadFile(filepath.Join(o.out, "soak.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(file), bytes.TrimSpace(out.Bytes())) {
		t.Error("soak.json differs from the -json output")
	}
	return code, rep
}

func TestSoakPasses(t *testing.T) {
	s := newSoakSession()
	var cfgSeen media.Config
	smp := &countingSampler{step: 1000} // 0.001 % per sample
	env := soakEnv{
		start: func(_ context.Context, cfg media.Config) (media.Session, error) {
			cfgSeen = cfg
			return s, nil
		},
		sample:    smp.sample,
		overflows: func() int64 { return 0 },
	}
	code, rep := runShortSoak(t, env, shortSoak(t))
	if code != exitPass || !rep.Pass {
		t.Fatalf("exit %d, failures %q", code, rep.Failures)
	}
	if s.stops.Load() == 0 {
		t.Error("the session was not stopped")
	}
	if cfgSeen.Source != "test:pattern" || cfgSeen.Audio == nil || cfgSeen.Audio.Mode != media.AudioTestTone ||
		len(cfgSeen.Layers) != 2 || cfgSeen.Layers[0].MaxFPS != 60 {
		t.Errorf("the session's config: %+v", cfgSeen)
	}
	if len(rep.Samples) < 3 {
		t.Errorf("%d samples, want the start, the ticks and the end", len(rep.Samples))
	}
	if len(rep.Growth) != 4 {
		t.Fatalf("growth %+v: want the four metrics", rep.Growth)
	}
	for _, g := range rep.Growth {
		if !g.Pass {
			t.Errorf("%s: %+v", g.Metric, g)
		}
	}
	if l := rep.Samples[0].Layers; len(l) != 1 || l[0].Encoder != "nvenc" {
		t.Errorf("the samples carry no stats: %+v", rep.Samples[0])
	}
}

func TestSoakFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		step      uint64
		gdi       uint64
		overflows int64
		ended     error
		want      string
	}{
		{name: "growth", step: 2_000_000, want: "privateBytes grew"},
		{name: "overflow", step: 0, overflows: 2, want: "RING_OVERFLOW 2 times"},
		{name: "ended", step: 0, ended: &media.Error{Code: media.CodeEncoderLost, Op: "next"}, want: "ENDED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSoakSession()
			s.ended, s.endAfter = tc.ended, 150*time.Millisecond
			smp := &countingSampler{step: tc.step, gdi: tc.gdi}
			var n atomic.Int64
			env := soakEnv{
				start: func(context.Context, media.Config) (media.Session, error) {
					n.Store(tc.overflows) // the overflows come after the start
					return s, nil
				},
				sample:    smp.sample,
				overflows: n.Load,
			}
			if tc.overflows > 0 {
				env.start = func(context.Context, media.Config) (media.Session, error) {
					go func() {
						time.Sleep(50 * time.Millisecond)
						n.Add(tc.overflows)
					}()
					return s, nil
				}
			}
			code, rep := runShortSoak(t, env, shortSoak(t))
			if code != exitFail || rep.Pass {
				t.Fatalf("exit %d, pass %t: want a failure", code, rep.Pass)
			}
			if !strings.Contains(strings.Join(rep.Failures, "\n"), tc.want) {
				t.Errorf("failures %q: want %q", rep.Failures, tc.want)
			}
		})
	}
}

func TestSoakStartFailureIsEnvironment(t *testing.T) {
	env := soakEnv{
		start: func(context.Context, media.Config) (media.Session, error) {
			return nil, &media.Error{Code: media.CodeNoEncoder, Op: "start"}
		},
		sample:    (&countingSampler{}).sample,
		overflows: func() int64 { return 0 },
	}
	o := shortSoak(t)
	cfg, _ := soakConfig(o)
	if code := runSoak(context.Background(), env, o, cfg, false, io.Discard, io.Discard); code != exitEnv {
		t.Errorf("exit %d, want %d", code, exitEnv)
	}
	env.sample = func() (procSample, error) { return procSample{}, errors.New("no such OS") }
	if code := runSoak(context.Background(), env, o, cfg, false, io.Discard, io.Discard); code != exitEnv {
		t.Errorf("a sampler that fails: exit %d, want %d", code, exitEnv)
	}
}

func TestEvalSoak(t *testing.T) {
	at := func(s float64, priv, handles, gdi, threads uint64) procSample {
		return procSample{AtS: s, PrivateBytes: priv, Handles: handles, GDIObjects: gdi, Threads: threads}
	}
	warm := 10 * time.Minute
	for _, tc := range []struct {
		name    string
		samples []procSample
		fail    []string // metrics that fail; "short" for a run that is too short
	}{
		{"steady after a warm-up spike", []procSample{
			at(0, 50, 100, 0, 20), at(300, 200, 300, 0, 40), at(600, 100, 200, 0, 30), at(3600, 104, 209, 0, 31),
			at(7200, 105, 210, 0, 31),
		}, nil},
		{"the end decides, not a peak", []procSample{
			at(600, 100, 200, 4, 30), at(3600, 120, 200, 4, 30), at(7200, 101, 200, 4, 30),
		}, nil},
		{"private bytes leak", []procSample{at(600, 100, 200, 4, 30), at(7200, 106, 200, 4, 30)},
			[]string{"privateBytes"}},
		{"handles and threads", []procSample{at(600, 100, 200, 4, 30), at(7200, 100, 211, 4, 32)},
			[]string{"handles", "threads"}},
		{"GDI objects from none", []procSample{at(600, 100, 200, 0, 30), at(7200, 100, 200, 1, 30)},
			[]string{"gdiObjects"}},
		{"exactly 5 %", []procSample{at(600, 100, 200, 20, 20), at(7200, 105, 210, 21, 21)}, nil},
		{"no sample after the warm-up", []procSample{at(0, 1, 1, 1, 1), at(599, 1, 1, 1, 1)}, []string{"short"}},
		{"only the baseline", []procSample{at(0, 1, 1, 1, 1), at(600, 1, 1, 1, 1)}, []string{"short"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			growth, fails := evalSoak(tc.samples, warm)
			got := map[string]bool{}
			for _, g := range growth {
				if !g.Pass {
					got[g.Metric] = true
				}
			}
			if len(growth) == 0 && len(fails) > 0 {
				got["short"] = true
			}
			if len(got) != len(tc.fail) {
				t.Fatalf("failing %v (%q), want %v", got, fails, tc.fail)
			}
			for _, m := range tc.fail {
				if !got[m] {
					t.Errorf("%s passed, want it to fail (%q)", m, fails)
				}
			}
			if len(fails) != len(tc.fail) {
				t.Errorf("failures %q, want one per failing metric %v", fails, tc.fail)
			}
		})
	}
}

func TestOverflowWatch(t *testing.T) {
	var n atomic.Int64
	var buf bytes.Buffer
	h := &overflowWatch{inner: slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}), n: &n}
	log := slog.New(h).With("component", "engine")
	log.Warn("engine ring overflow: the consumer fell behind", "stream", "f", "dropped", 12)
	log.WithGroup("g").Warn("engine ring overflow: the consumer fell behind")
	log.Warn("engine packet dropped")
	log.Info("engine ring overflow, at info") // the binding logs it at warn; below warn the logger never asks
	if got := n.Load(); got != 2 {
		t.Errorf("counted %d, want 2", got)
	}
	if buf.Len() != 0 {
		t.Errorf("the inner handler's level was not kept: %q", buf.String())
	}
}

func TestSoakUsage(t *testing.T) {
	for _, args := range [][]string{
		{"soak", "-hours", "0"},
		{"soak", "-hours", "100"},
		{"soak", "-hours", "0.1"}, // less than the 10-minute warm-up plus a sample
		{"soak", "-every", "0s"},
		{"soak", "-audio", "everything"},
		{"soak", "-source", "window:1"},
		{"soak", "-encoder", "x264"},
		{"soak", "-profile", "main"},
	} {
		if code, _, stderr := runArgs(t, args...); code != exitUsage {
			t.Errorf("%q: exit %d, want %d\n%s", args, code, exitUsage, stderr)
		}
	}
	if code, _, stderr := runArgs(t, "-dll", t.TempDir(), "soak", "-hours", "0.5", "-out", t.TempDir()); code != exitEnv {
		t.Errorf("without an engine: exit %d, want %d\n%s", code, exitEnv, stderr)
	}
}
