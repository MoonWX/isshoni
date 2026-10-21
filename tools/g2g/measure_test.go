package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// fakeScreen is a capturer that draws the screen at the time of each grab: the clock's panel with the time's
// value, and a copy of the panel at half size whose value lags by lag(i) ms (no copy when ok is false).
type fakeScreen struct {
	origin  time.Time
	offset  int // the clock's value at origin
	lag     func(i int) (ms int, ok bool)
	copyAt  func(i int) (x, y float64)
	jumpAt  int // from this screenshot on, the clock is jump ms ahead (a reload); 0: never
	jump    int
	refresh int

	mu sync.Mutex
	n  int
}

func (s *fakeScreen) grab() (*Frame, error) {
	s.mu.Lock()
	i := s.n
	s.n++
	s.mu.Unlock()
	now := int(time.Since(s.origin).Milliseconds()) + s.offset
	if s.jumpAt > 0 && i >= s.jumpAt {
		now += s.jump
	}
	f := newFrame(1000, 320)
	f.fill(f.bounds(), 32, 33, 36)
	paintPanel(f, 0, 0, v16(now))
	if lag, ok := s.lag(i); ok {
		x, y := 300.3, 180.6
		if s.copyAt != nil {
			x, y = s.copyAt(i)
		}
		paintPanelScaled(f, x, y, 0.5, v16(now-lag))
	}
	return f, nil
}

func (s *fakeScreen) refreshHz() int { return s.refresh }
func (s *fakeScreen) close() error   { return nil }

// useScreen makes measure capture s.
func useScreen(t *testing.T, s *fakeScreen) {
	t.Helper()
	old := openCapturer
	openCapturer = func() (capturer, error) { return s, nil }
	t.Cleanup(func() { openCapturer = old })
}

// measureJSON runs `g2g measure args… -o file` and returns its exit status and report.
func measureJSON(t *testing.T, args ...string) (int, report, string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "result.json")
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), append(append([]string{"measure"}, args...), "-o", out), &stdout, &stderr)
	var rep report
	if b, err := os.ReadFile(out); err == nil { //nolint:gosec // G304: the test's own file
		if err := json.Unmarshal(b, &rep); err != nil {
			t.Fatalf("the JSON: %v\n%s", err, b)
		}
		if !json.Valid(b) {
			t.Fatal("invalid JSON")
		}
	}
	return code, rep, stderr.String()
}

// TestMeasureResults: the exit status and the report of each kind of run, on a simulated screen with virtual
// time (2 s of calibration, then 20 screenshots a second).
func TestMeasureResults(t *testing.T) {
	jitter := func(base, spread int) func(int) (int, bool) {
		rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: the same jitter in every run, not a secret
		return func(int) (int, bool) { return base + rng.IntN(spread+1), true }
	}
	for _, tc := range []struct {
		name     string
		run      string
		lag      func(int) (int, bool)
		code     int
		result   string
		minP95   int
		maxP95   int
		failures int // at least
	}{
		{"pass", "gate", jitter(120, 60), exitOK, resultPass, 120, 180, 0},
		{"fail", "nosound", jitter(230, 40), exitLatency, resultFail, 230, 270, 0},
		{"just under", "nocuts", func(int) (int, bool) { return 199, true }, exitOK, resultPass, 199, 199, 0},
		{"at the limit", "gate", func(int) (int, bool) { return 200, true }, exitLatency, resultFail, 200, 200, 0},
		{
			"too few decoded", "gate",
			func(i int) (int, bool) { return 150, i < 40 || i%8 != 0 }, // the copy is gone in every 8th screenshot
			exitInconclusive, resultInconclusive, 150, 150, 7,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				useScreen(t, &fakeScreen{origin: time.Now(), offset: 61000, lag: tc.lag, refresh: 60})
				code, rep, stderr := measureJSON(t, "-d", "3s", "-run", tc.run)
				if code != tc.code || rep.Result != tc.result || rep.ExitCode != code {
					t.Fatalf("exit %d, result %q (exitCode %d), want %d %q\n%s", code, rep.Result, rep.ExitCode, tc.code,
						tc.result, stderr)
				}
				if rep.Run != tc.run || rep.Tool != "g2g" {
					t.Errorf("run %q tool %q, want %q g2g", rep.Run, rep.Tool, tc.run)
				}
				if rep.Screenshots != 60 || rep.IntervalMs != 50 || rep.DurationMs != 3000 || rep.RefreshHz != 60 {
					t.Errorf("screenshots %d, interval %d, duration %d, refresh %d; want 60, 50, 3000, 60",
						rep.Screenshots, rep.IntervalMs, rep.DurationMs, rep.RefreshHz)
				}
				if rep.LatencyMs == nil || rep.LatencyMs.P95 < tc.minP95 || rep.LatencyMs.P95 > tc.maxP95 {
					t.Errorf("latency %+v, want p95 in [%d, %d]", rep.LatencyMs, tc.minP95, tc.maxP95)
				}
				if rep.Failures.Total < tc.failures || rep.Failures.Total != rep.Screenshots-rep.Samples {
					t.Errorf("failures %+v of %d screenshots with %d samples, want at least %d", rep.Failures,
						rep.Screenshots, rep.Samples, tc.failures)
				}
				if rep.Copy.Scale < 0.49 || rep.Copy.Scale > 0.51 || rep.Screen.Width != 1000 {
					t.Errorf("copy %+v, screen %+v", rep.Copy, rep.Screen)
				}
				if len(rep.Series) != rep.Screenshots || rep.Criterion.P95BelowMs != 200 || rep.Criterion.MinDecodedPct != 90 {
					t.Errorf("series of %d, criterion %+v", len(rep.Series), rep.Criterion)
				}
			})
		})
	}
}

// TestMeasureTracksAndFilters: a viewer that moves is followed, a misread copy and a jumping clock count as
// implausible, and a lasting change of the clock is followed.
func TestMeasureTracksAndFilters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &fakeScreen{
			origin: time.Now(), offset: 100,
			lag: func(i int) (int, bool) {
				if i == 50 || i == 51 {
					return 150 + 20000, true // a misread: seconds off
				}
				return 150, true
			},
			copyAt: func(i int) (float64, float64) {
				if i >= 70 {
					return 330.5, 200.2 // the viewer moved
				}
				return 300.3, 180.6
			},
			jumpAt: 100, jump: 5000, // the page was reloaded
		}
		useScreen(t, s)
		code, rep, stderr := measureJSON(t, "-d", "12s") // 240 screenshots, so 22 failures still pass
		if rep.CopyMoves != 1 {
			t.Errorf("copyMoves %d, want 1\n%s", rep.CopyMoves, stderr)
		}
		// 2 misreads, and the first 20 screenshots after the jump, until the clock is followed.
		if rep.Failures.Implausible != 2+recoverAfter || rep.Failures.Copy != 0 || rep.Failures.Clock != 0 {
			t.Errorf("failures %+v, want %d implausible and nothing else\n%s", rep.Failures, 2+recoverAfter, stderr)
		}
		if !strings.Contains(stderr, "the clock jumped by 5000 ms") {
			t.Errorf("stderr does not report the jump:\n%s", stderr)
		}
		if rep.LatencyMs == nil || rep.LatencyMs.Max != 150 || code != exitOK {
			t.Errorf("exit %d, latency %+v; want 0 and 150 ms", code, rep.LatencyMs)
		}
	})
}

func TestMeasureCalibrationFails(t *testing.T) {
	for _, tc := range []struct {
		name   string
		screen capturer
		want   string
	}{
		{"no copy", &fakeScreen{lag: func(int) (int, bool) { return 0, false }}, "no copy of the clock's strip"},
		{"no clock", staticCapturer{newFrame(400, 300)}, "the clock's strip is not on the screen"},
		{"no screenshot", errCapturer{}, "no screenshot could be taken"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				if fs, ok := tc.screen.(*fakeScreen); ok {
					fs.origin = time.Now()
				}
				old := openCapturer
				openCapturer = func() (capturer, error) { return tc.screen, nil }
				defer func() { openCapturer = old }()
				code, _, stderr := measureJSON(t)
				if code != exitInconclusive || !strings.Contains(stderr, tc.want) {
					t.Errorf("exit %d, stderr %q; want 2 and %q", code, stderr, tc.want)
				}
			})
		})
	}
}

type errCapturer struct{}

func (errCapturer) grab() (*Frame, error) { return nil, errors.New("the desktop is locked") }
func (errCapturer) refreshHz() int        { return 0 }
func (errCapturer) close() error          { return nil }

// TestMeasureInterrupted: Ctrl+C during the measurement writes what was measured, and the result is inconclusive.
func TestMeasureInterrupted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		useScreen(t, &fakeScreen{origin: time.Now(), lag: func(int) (int, bool) { return 100, true }})
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second) // calibration 2 s, then 1 s of 60
		defer cancel()
		out := filepath.Join(t.TempDir(), "r.json")
		var stderr bytes.Buffer
		code := run(ctx, []string{"measure", "-o", out}, &bytes.Buffer{}, &stderr)
		b, err := os.ReadFile(out) //nolint:gosec // G304: the test's own file
		if err != nil {
			t.Fatal(err)
		}
		var rep report
		if err := json.Unmarshal(b, &rep); err != nil {
			t.Fatal(err)
		}
		if code != exitInconclusive || !rep.Interrupted || rep.Result != resultInconclusive || rep.Screenshots < 15 {
			t.Errorf("exit %d, report %+v", code, rep)
		}
	})
}

// TestMeasureServer: with -server, the RTT and the server's version are in the JSON, and the server's address is
// not.
func TestMeasureServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/info" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"server":{"name":"friends","version":"0.1.0","publicUrl":"https://friends.example"}}`))
	}))
	defer srv.Close()
	useScreen(t, &fakeScreen{origin: time.Now(), lag: func(int) (int, bool) { return 90, true }})
	code, rep, stderr := measureJSON(t, "-d", "1s", "-server", srv.URL+"/")
	if code != exitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if rep.RTTMs == nil || len(rep.RTTSamplesMs) != rttConnects || rep.Versions.Server != "0.1.0" {
		t.Errorf("rtt %v %v, server version %q", rep.RTTMs, rep.RTTSamplesMs, rep.Versions.Server)
	}
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	for _, secret := range []string{host, "127.0.0.1", "friends"} {
		if bytes.Contains(b, []byte(secret)) {
			t.Errorf("the JSON contains %q", secret)
		}
	}
}

func TestServerChecks(t *testing.T) {
	for _, bad := range []string{
		"isshoni.example", "ftp://isshoni.example", "https://", "https://u:p@isshoni.example", "%",
	} {
		if _, err := parseServer(bad); err == nil {
			t.Errorf("parseServer(%q) accepted it", bad)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a/api/v1/info":
			_, _ = w.Write([]byte(`{"server":{"version":"1.2.3"}}`))
		case "/b/api/v1/info":
			_, _ = w.Write([]byte(`{"server":{}}`))
		default:
			http.Error(w, "no", http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	for path, want := range map[string]string{"/a": "1.2.3", "/b": "", "/c": ""} {
		u, err := parseServer(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := serverVersion(ctx, u)
		if got != want || (want == "") != (err != nil) {
			t.Errorf("serverVersion(%s) = %q, %v; want %q", path, got, err, want)
		}
	}
	u, _ := parseServer(srv.URL)
	ms, err := measureRTT(ctx, u)
	if err != nil || len(ms) != rttConnects {
		t.Fatalf("measureRTT = %v, %v", ms, err)
	}
	srv.Close()
	if _, err := measureRTT(ctx, u); err == nil {
		t.Error("measureRTT to a closed port succeeded")
	}
}

func TestDump(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		useScreen(t, &fakeScreen{origin: time.Now(), lag: func(i int) (int, bool) { return 100, i < 45 }})
		dir := filepath.Join(t.TempDir(), "dump")
		code, _, stderr := measureJSON(t, "-d", "1s", "-dump", dir)
		if code != exitInconclusive {
			t.Fatalf("exit %d, want 2 (the copy is gone after the calibration)\n%s", code, stderr)
		}
		files, err := filepath.Glob(filepath.Join(dir, "*.png"))
		if err != nil {
			t.Fatal(err)
		}
		// The calibration's two crops and two for each failed screenshot, up to maxDumps files in all.
		if len(files) < maxDumps || len(files) > maxDumps+1 {
			t.Errorf("dumped %d files: %v", len(files), files)
		}
		f := readPNG(t, filepath.Join(dir, "calibration-clock.png"))
		if f.W > panelW || f.H > 2*cellPx {
			t.Errorf("the clock's crop is %d × %d: more than the strip", f.W, f.H)
		}
	})
}
