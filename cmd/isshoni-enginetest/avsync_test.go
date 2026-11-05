package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/native"
)

// The synthetic run of the pairing tests: flashes on a 1 s grid at phase 300 ms, clicks on another at phase 700 ms
// (the nearest click of each flash is 400 ms after it), the capture stamps each flash 16 ms after it became visible
// and each click 25 ms after its device-position time, so every pair's offset is 25 − 16 = 9 ms.
func syntheticRun(n int) (p, v, a, u []int64) {
	for k := range n {
		pk := int64(k)*int64(time.Second) + 300*ms
		ak := int64(k)*int64(time.Second) + 700*ms
		p = append(p, pk)
		v = append(v, pk+16*ms+int64(k%3-1)*ms) // ±1 ms of jitter
		a = append(a, ak)
		u = append(u, ak+25*ms)
	}
	return p, v, a, u
}

func offsetsOf(pairs []avPair) []float64 {
	out := make([]float64, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, p.OffsetMS)
	}
	return out
}

func TestPairAVSync(t *testing.T) {
	p, v, a, u := syntheticRun(20)
	pairs, unpaired := pairAVSync(p, v, a, u)
	if len(pairs) != 20 || len(unpaired) != 0 {
		t.Fatalf("%d pairs, unpaired %q: want 20 and none", len(pairs), unpaired)
	}
	for _, pr := range pairs {
		if pr.Flash != pr.Click {
			t.Errorf("flash %d paired with click %d: want the click 400 ms after it", pr.Flash, pr.Click)
		}
		if want := 25.0 - (16.0 + float64(pr.Flash%3-1)); math.Abs(pr.OffsetMS-want) > 1e-9 {
			t.Errorf("flash %d: offset %.3f ms, want %.3f", pr.Flash, pr.OffsetMS, want)
		}
		if pr.VideoMS <= 0 || pr.AudioMS != 25 {
			t.Errorf("flash %d: video %.1f ms, audio %.1f ms", pr.Flash, pr.VideoMS, pr.AudioMS)
		}
	}
	if m := quantile(offsetsOf(pairs), 0.5); m != 9 {
		t.Errorf("median %.3f ms, want 9", m)
	}
	if fails := checkAVSync(offsetsOf(pairs)); len(fails) != 0 {
		t.Errorf("failures %q", fails)
	}
}

// The epoch only associates events; an error in it (here 60 ms, more than a frame) changes no offset.
func TestPairAVSyncEpochError(t *testing.T) {
	p, v, a, u := syntheticRun(16)
	want, _ := pairAVSync(p, v, a, u)
	got, unpaired := pairAVSync(p, shift(v, 60*ms), a, shift(u, 60*ms))
	if len(got) != len(want) || len(unpaired) != 0 {
		t.Fatalf("%d pairs (unpaired %q), want %d", len(got), unpaired, len(want))
	}
	for i := range got {
		if got[i].OffsetMS != want[i].OffsetMS || got[i].Click != want[i].Click {
			t.Errorf("pair %d: %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestPairAVSyncMissingAndSpurious(t *testing.T) {
	p, v, a, u := syntheticRun(20)
	// Flash 3's frame was never seen, click 7 was never heard, flash 19 has no click any more (the click process
	// stopped early), and a door slammed 150 ms before click 10's onset.
	v = append(v[:3:3], v[4:]...)
	u = append(u[:7:7], u[8:]...)
	a = a[:19]
	u = append(u, a[10]-150*ms)
	pairs, unpaired := pairAVSync(p, v, a, u)
	if len(pairs) != 17 {
		t.Errorf("%d pairs, want 17", len(pairs))
	}
	want := []string{"flash 3: no frame", "flash 7: click 7 not found", "flash 19: no click"}
	if len(unpaired) != len(want) {
		t.Fatalf("unpaired %q, want %q", unpaired, want)
	}
	for i, w := range want {
		if !strings.HasPrefix(unpaired[i], w) {
			t.Errorf("unpaired[%d] = %q, want %q…", i, unpaired[i], w)
		}
	}
	for _, pr := range pairs {
		if pr.Flash == 10 && pr.AudioMS != 25 {
			t.Errorf("flash 10 took the spurious onset: audio %.1f ms", pr.AudioMS)
		}
	}
}

// Clicks on a grid that runs exactly between the flashes' (500 ms apart): each flash takes a click of its own; a
// flash beyond the clicks gets none.
func TestPairAVSyncEachClickOnce(t *testing.T) {
	var p, v, a, u []int64
	for k := range 4 {
		pk := int64(k) * int64(time.Second)
		p, v = append(p, pk), append(v, pk+10*ms)
	}
	for k := range 2 {
		ak := int64(k)*int64(time.Second) + 500*ms
		a, u = append(a, ak), append(u, ak+10*ms)
	}
	pairs, unpaired := pairAVSync(p, v, a, u)
	seen := map[int]bool{}
	for _, pr := range pairs {
		if seen[pr.Click] {
			t.Errorf("click %d paired twice", pr.Click)
		}
		seen[pr.Click] = true
	}
	if len(pairs) != 2 || len(unpaired) != 2 {
		t.Errorf("%d pairs, unpaired %q: want 2 and 2", len(pairs), unpaired)
	}
}

func TestCheckAVSync(t *testing.T) {
	many := func(n int, v float64) []float64 {
		out := make([]float64, n)
		for i := range out {
			out[i] = v
		}
		return out
	}
	for _, tc := range []struct {
		name    string
		offsets []float64
		fails   int
	}{
		{"15 pairs at 19 ms", many(15, 19), 0},
		{"15 pairs at -20 ms", many(15, -20), 0},
		{"14 pairs", many(14, 0), 1},
		{"20.5 ms", many(20, 20.5), 1},
		{"-21 ms", many(20, -21), 1},
		{"none", nil, 1},
		{"outliers don't move the median", append(many(16, 5), 900, -900, 400), 0},
	} {
		if fails := checkAVSync(tc.offsets); len(fails) != tc.fails {
			t.Errorf("%s: failures %q, want %d", tc.name, fails, tc.fails)
		}
	}
	if m := quantile([]float64{4, 1, 3, 2}, 0.5); m != 2.5 {
		t.Errorf("median of 1..4: %v", m)
	}
}

// pcmChunkWith is a PCM-tap chunk, silent except for the given frames (both channels at value).
func pcmChunkWith(value float32, frames ...int) []byte {
	b := make([]byte, pcmChunkBytes)
	for _, f := range frames {
		for c := range 2 {
			binary.LittleEndian.PutUint32(b[(f*2+c)*4:], math.Float32bits(value))
		}
	}
	return b
}

func TestOnsetDetector(t *testing.T) {
	d := newOnsetDetector()
	const chunk = 10 * ms
	base := 5 * int64(time.Second)
	feed := func(i int, data []byte) {
		t.Helper()
		if err := d.chunk(base+int64(i)*chunk, data); err != nil {
			t.Fatal(err)
		}
	}
	feed(0, pcmChunkWith(0))
	feed(1, pcmChunkWith(0.04, 10, 200)) // below the level
	// A click from frame 100 of chunk 2 to frame 99 of chunk 3: one onset, in block 2 (frames 96..143) of chunk 2.
	clickFrames := []int{}
	for f := 100; f < pcmChunkFrames; f++ {
		clickFrames = append(clickFrames, f)
	}
	feed(2, pcmChunkWith(0.5, clickFrames...))
	feed(3, pcmChunkWith(-0.5, 0, 50, 99))
	for i := 4; i < 100; i++ {
		feed(i, pcmChunkWith(0))
	}
	feed(100, pcmChunkWith(-0.06, 479)) // a negative sample counts too: block 9
	feed(101, pcmChunkWith(0.3, 0))     // 10 ms later: the same click, inside the refractory time
	want := []int64{base + 2*chunk + 2*ms, base + 100*chunk + 9*ms}
	if len(d.onsets) != len(want) {
		t.Fatalf("onsets %v, want %v", d.onsets, want)
	}
	for i := range want {
		if d.onsets[i] != want[i] {
			t.Errorf("onset %d at %d, want %d", i, d.onsets[i], want[i])
		}
	}
	if err := d.chunk(0, make([]byte, 100)); err == nil {
		t.Error("a short chunk was accepted")
	}
	// Only one channel loud is enough.
	one := make([]byte, pcmChunkBytes)
	binary.LittleEndian.PutUint32(one[(48*2+1)*4:], math.Float32bits(0.2))
	d2 := newOnsetDetector()
	_ = d2.chunk(0, one)
	if len(d2.onsets) != 1 || d2.onsets[0] != ms {
		t.Errorf("right channel only: onsets %v, want [1 ms]", d2.onsets)
	}
}

func TestLumaEdges(t *testing.T) {
	frames := []lumaSample{
		{0, 0.9},   // the capture started inside a flash: no edge
		{16, 0.95}, //
		{33, 0.1},
		{50, 0.5}, // not above 50 %
		{66, 0.51},
		{83, 0.9},
		{100, 0.05},
		{116, 0.8},
	}
	got := lumaEdges(frames, avsyncLumaLevel)
	if want := []int64{66, 116}; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("edges %v, want %v", got, want)
	}
}

func TestFlashTimes(t *testing.T) {
	const freq = 10_000_000 // 10 MHz, as Windows' QPC usually runs
	period := float64(freq) / 60
	base := int64(123_456_789)
	stat := func(n uint32) frameStat {
		// Present n was shown at refresh n + 1000; the statistics were sampled at that refresh.
		return frameStat{PresentCount: n, PresentRefreshCount: n + 1000, SyncRefreshCount: n + 1000,
			SyncQPC: base + int64(math.Round(float64(n+1000)*period))}
	}
	var stats []frameStat
	for n := uint32(1); n <= 120; n++ {
		if n == 61 || n == 62 { // two presents the statistics skipped
			continue
		}
		stats = append(stats, stat(n))
	}
	// One reading sampled two refreshes after its present: SyncQPC is then later than the present's display.
	late := stat(30)
	late.SyncRefreshCount += 2
	late.SyncQPC += int64(math.Round(2 * period))
	stats[29] = late
	r := flashTimes([]uint32{1, 30, 61, 200}, stats, freq)
	if r.Unresolved != 1 || r.Estimated != 1 || len(r.FlashesNS) != 3 {
		t.Fatalf("%+v: want 3 flashes (one extrapolated) and 1 unresolved", r)
	}
	if r.RefreshNS < 16_666_000 || r.RefreshNS > 16_667_000 {
		t.Errorf("refresh %d ns, want 16.67 ms", r.RefreshNS)
	}
	for i, n := range []uint32{1, 30, 61} {
		want := qpcTicksToNS(stat(n).SyncQPC, freq)
		if d := r.FlashesNS[i] - want; d < -1000 || d > 1000 {
			t.Errorf("present %d at %d ns, want %d (±1 µs)", n, r.FlashesNS[i], want)
		}
	}
	if q := qpcTicksToNS(3*freq+freq/2, freq); q != 3_500_000_000 {
		t.Errorf("qpcTicksToNS: %d", q)
	}
	if r := flashTimes([]uint32{1}, nil, freq); r.Unresolved != 1 {
		t.Errorf("without statistics: %+v", r)
	}
}

// ------------------------------------------------------------------------------------------- the run, with fakes

// avsyncFakeRun is a PC on which everything of the run is synthetic: the session's clock is the engine clock minus
// epoch; flashes and clicks are on the run's grids; the luma tap and the PCM tap show them with fixed delays.
type avsyncFakeRun struct {
	epoch      int64
	start      time.Time
	flashes    []int64 // P (engine clock)
	clicks     []int64 // A (engine clock)
	videoDelay int64
	audioDelay int64

	mu      sync.Mutex
	cfg     media.Config
	picked  bool
	stopped bool
	clickN  int
	flashOK bool
}

func (f *avsyncFakeRun) now() int64 { return int64(time.Since(f.start)) } // the session clock

func (f *avsyncFakeRun) Pick(context.Context, media.PickRequest) (media.Source, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.picked = true
	return media.Source{ID: "pick:1", Kind: media.SourceWindow}, nil
}

func (f *avsyncFakeRun) StartWith(_ context.Context, cfg media.Config, _ ...native.StartOption) (media.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfg = cfg
	s := &avsyncSession{run: f, stopped: make(chan struct{})}
	// The PCM tap: 10 ms chunks over the clicks' span, each click 5 ms long from A + audioDelay.
	if len(f.clicks) > 0 {
		first := f.clicks[0] - f.epoch - 100*ms
		last := f.clicks[len(f.clicks)-1] - f.epoch + 100*ms
		for t := first - first%(10*ms); t < last; t += 10 * ms {
			var frames []int
			for _, a := range f.clicks {
				on := a + f.audioDelay - f.epoch // the click's onset on the session clock
				for i := range pcmChunkFrames {
					ft := t + int64(i)*int64(time.Second)/pcmRate
					if ft >= on && ft < on+5*ms {
						frames = append(frames, i)
					}
				}
			}
			s.packets = append(s.packets, media.Packet{Kind: media.KindPCM, CaptureNS: t,
				Data: pcmChunkWith(0.5, frames...)})
		}
	}
	return s, nil
}

func (f *avsyncFakeRun) Clock() (native.ClockSample, error) {
	return native.ClockSample{MonoNS: f.now() + f.epoch}, nil
}

type avsyncSession struct {
	media.Session
	run     *avsyncFakeRun
	packets []media.Packet
	stopped chan struct{}
	once    sync.Once
}

func (s *avsyncSession) Next(ctx context.Context) (media.Packet, error) {
	if len(s.packets) > 0 {
		p := s.packets[0]
		s.packets = s.packets[1:]
		return p, nil
	}
	select {
	case <-ctx.Done():
		return media.Packet{}, ctx.Err()
	case <-s.stopped:
		return media.Packet{}, media.ErrStopped
	}
}

func (s *avsyncSession) Now() int64 { return s.run.now() }

func (s *avsyncSession) Stop() error {
	s.once.Do(func() {
		close(s.stopped)
		s.run.mu.Lock()
		s.run.stopped = true
		s.run.mu.Unlock()
	})
	return nil
}

type avsyncFakeHelpers struct{ run *avsyncFakeRun }

func (h avsyncFakeHelpers) StartFlash(context.Context) (flashProc, error) { return h, nil }
func (h avsyncFakeHelpers) Stop() (flashResult, error) {
	h.run.mu.Lock()
	h.run.flashOK = true
	h.run.mu.Unlock()
	return flashResult{FlashesNS: h.run.flashes, RefreshNS: 16_666_667}, nil
}

func (h avsyncFakeHelpers) StartClicks(_ context.Context, n int) (clickProc, error) {
	h.run.mu.Lock()
	h.run.clickN = n
	h.run.mu.Unlock()
	return h, nil
}

func (h avsyncFakeHelpers) Wait(context.Context) (clickResult, error) {
	return clickResult{OnsetsNS: h.run.clicks}, nil
}
func (avsyncFakeHelpers) Close() {}

// fakeLumaTap shows each flash as a white picture videoDelay after it, between black pictures every 16.7 ms.
type fakeLumaTap struct{ run *avsyncFakeRun }

func (fakeLumaTap) Options() []native.StartOption { return nil }

func (l fakeLumaTap) Samples() []lumaSample {
	r := l.run
	var out []lumaSample
	if len(r.flashes) == 0 {
		return nil
	}
	from := r.flashes[0] - r.epoch - 200*ms
	to := r.flashes[len(r.flashes)-1] - r.epoch + 300*ms
	for _, p := range r.flashes {
		v := p + r.videoDelay - r.epoch
		for t := from; t < v; t += 16_666_667 {
			out = append(out, lumaSample{CaptureNS: t, Mean: 0.06})
		}
		out = append(out, lumaSample{CaptureNS: v, Mean: 0.93})
		from = v + 100*ms
	}
	out = append(out, lumaSample{CaptureNS: to, Mean: 0.06})
	return out
}

// newAVSyncFakeRun has 18 flashes and 18 clicks 250 ms after each, seen videoDelay and heard audioDelay later.
func newAVSyncFakeRun(videoDelay, audioDelay int64) *avsyncFakeRun {
	run := &avsyncFakeRun{epoch: 987_654_321_000, start: time.Now(), videoDelay: videoDelay, audioDelay: audioDelay}
	for k := range 18 {
		run.flashes = append(run.flashes, run.epoch+int64(k)*int64(time.Second)+200*ms)
		run.clicks = append(run.clicks, run.epoch+int64(k)*int64(time.Second)+450*ms)
	}
	return run
}

func TestRunAVSyncWithFakes(t *testing.T) {
	run := newAVSyncFakeRun(12*ms, 30*ms)
	out := t.TempDir()
	o := avsyncOptions{source: "pick", seconds: 0.2, out: out}
	var stdout, stderr bytes.Buffer
	env := avsyncEnv{engine: run, helpers: avsyncFakeHelpers{run}, tap: fakeLumaTap{run}}
	code := runAVSync(context.Background(), env, o, true, &stdout, &stderr)
	var rep avsyncReport
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("exit %d: no report: %v\n%s", code, err, stderr.String())
	}
	if code != exitPass || !rep.Pass {
		t.Fatalf("exit %d, failures %q, unpaired %q", code, rep.Failures, rep.Unpaired)
	}
	if len(rep.Pairs) != 18 || math.Abs(rep.MedianMS-18) > 0.05 {
		// 30 ms − 12 ms; the PCM tap's 1 ms blocks round the onset down by less than a block.
		t.Errorf("%d pairs, median %.3f ms: want 18 and 18 ms", len(rep.Pairs), rep.MedianMS)
	}
	if rep.Edges != 18 || rep.Onsets != 18 || rep.Clicks != 18 || rep.Flashes != 18 {
		t.Errorf("counts: %+v", rep)
	}
	if !run.picked || !run.stopped || !run.flashOK {
		t.Errorf("picked %t, stopped %t, flash stopped %t", run.picked, run.stopped, run.flashOK)
	}
	if a := run.cfg.Audio; a == nil || !a.TapPCM || a.Mode != media.AudioExcludeApps || run.cfg.Listed ||
		run.cfg.Source != "pick:1" || run.cfg.Layers[0].MaxFPS != 60 || run.cfg.Layers[0].MaxPixels != 1920*1080 {
		t.Errorf("the session's config: %+v (audio %+v)", run.cfg, run.cfg.Audio)
	}
	file, err := os.ReadFile(filepath.Join(out, "avsync.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(file), bytes.TrimSpace(stdout.Bytes())) {
		t.Error("avsync.json differs from the -json output")
	}
	if strings.Contains(string(file), "pick:1") {
		t.Error("avsync.json names the source")
	}

	// The same run with the audio stamped 21 ms later than the video: it fails.
	run2 := newAVSyncFakeRun(12*ms, 33*ms)
	env = avsyncEnv{engine: run2, helpers: avsyncFakeHelpers{run2}, tap: fakeLumaTap{run2}}
	o.out = t.TempDir()
	stdout.Reset()
	if code := runAVSync(context.Background(), env, o, true, &stdout, &stderr); code != exitFail {
		t.Errorf("a 21 ms offset: exit %d, want %d", code, exitFail)
	}
}

func TestPairClicks(t *testing.T) {
	// Clicks every second; onsets 25 ms later, the third missing and a spurious one between the first two.
	a := []int64{1_000 * ms, 2_000 * ms, 3_000 * ms, 4_000 * ms}
	u := []int64{1_025 * ms, 1_600 * ms, 2_025 * ms, 4_025 * ms}
	pairs, unpaired := pairClicks(a, u)
	if len(pairs) != 3 || len(unpaired) != 1 || !strings.HasPrefix(unpaired[0], "click 2:") {
		t.Fatalf("pairs %+v, unpaired %q", pairs, unpaired)
	}
	for _, p := range pairs {
		if p.AudioMS != 25 || p.UNS-p.ANS != 25*ms {
			t.Errorf("pair %+v: want U − A = 25 ms", p)
		}
	}
	// Each onset at most once: two clicks near one onset take it once.
	if pairs, _ := pairClicks([]int64{0, 10 * ms}, []int64{5 * ms}); len(pairs) != 1 {
		t.Errorf("one onset paired %d times", len(pairs))
	}
}

func TestRunAVSyncNoLuma(t *testing.T) {
	run := newAVSyncFakeRun(12*ms, 30*ms)
	o := avsyncOptions{source: "pick", seconds: 0.2, out: t.TempDir(), noLuma: true}
	var stdout, stderr bytes.Buffer
	env := avsyncEnv{engine: run, helpers: avsyncFakeHelpers{run}}
	code := runAVSync(context.Background(), env, o, true, &stdout, &stderr)
	var rep avsyncReport
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("exit %d: no report: %v\n%s", code, err, stderr.String())
	}
	if code != exitPass || rep.Pass || rep.Diagnostic != "no-luma" || len(rep.Failures) != 0 {
		t.Fatalf("exit %d, pass %t, diagnostic %q, failures %q: want a clean run without a verdict", code, rep.Pass,
			rep.Diagnostic, rep.Failures)
	}
	if len(rep.FlashesNS) != 18 || len(rep.ClickPairs) != 18 || len(rep.Pairs) != 0 || len(rep.AudioMS) != 3 ||
		math.Abs(rep.AudioMS[1]-30) > 1 {
		t.Errorf("flashes %d, clicks %d, pairs %d, U − A %v: want 18, 18, 0 and a 30 ms median", len(rep.FlashesNS),
			len(rep.ClickPairs), len(rep.Pairs), rep.AudioMS)
	}
	var text bytes.Buffer
	printAVSync(&text, rep)
	if !strings.Contains(text.String(), "DIAGNOSTIC") || strings.Contains(text.String(), "PASS") {
		t.Errorf("the text report:\n%s", text.String())
	}

	// Nothing heard: a failure of the Windows half.
	silent := newAVSyncFakeRun(12*ms, 30*ms)
	silent.clicks = nil
	env = avsyncEnv{engine: silent, helpers: avsyncFakeHelpers{silent}}
	o.out = t.TempDir()
	stdout.Reset()
	if code := runAVSync(context.Background(), env, o, true, &stdout, &stderr); code != exitFail {
		t.Errorf("no clicks heard: exit %d, want %d", code, exitFail)
	}
}

func TestAVSyncUsage(t *testing.T) {
	for _, args := range [][]string{
		{"avsync", "-source", "test:pattern"},
		{"avsync", "-seconds", "19"},
		{"avsync", "-role", "bogus"},
		{"avsync", "-role", "flash"},
		{"avsync", "-role", "click", "-result", "x"},
		{"avsync", "-role", "click", "-result", "x", "-clicks", "601"},
		{"avsync", "extra"},
	} {
		if code, _, stderr := runArgs(t, args...); code != exitUsage {
			t.Errorf("%q: exit %d, want %d\n%s", args, code, exitUsage, stderr)
		}
	}
	// The luma tap isn't delivered by the binding yet: an environment failure before anything starts.
	code, _, stderr := runArgs(t, "-dll", t.TempDir(), "avsync", "-out", t.TempDir())
	if code != exitEnv || !strings.Contains(stderr, "luma tap") {
		t.Errorf("exit %d, want %d with the luma tap named\n%s", code, exitEnv, stderr)
	}
	// -no-luma gets past the tap to the engine (which this -dll doesn't have).
	code, _, stderr = runArgs(t, "-dll", t.TempDir(), "avsync", "-no-luma", "-out", t.TempDir())
	if code != exitEnv || strings.Contains(stderr, "luma tap") {
		t.Errorf("-no-luma: exit %d, want %d without the luma tap\n%s", code, exitEnv, stderr)
	}
	var me *media.Error
	if !errors.As(error(errLumaTap), &me) || envCode(errLumaTap) != exitEnv {
		t.Error("errLumaTap is not an environment error")
	}
	// The click process without an engine writes its result with the error and exits 2.
	res := filepath.Join(t.TempDir(), "click.json")
	if code, _, _ := runArgs(t, "-dll", t.TempDir(), "avsync", "-role", "click", "-clicks", "3", "-result", res); code != exitEnv {
		t.Errorf("click without an engine: exit %d, want %d", code, exitEnv)
	}
	var cr clickResult
	if err := readJSON(res, &cr); err != nil || cr.Error == "" || cr.OnsetsNS == nil {
		t.Errorf("click result %+v (%v): want an error and an empty list", cr, err)
	}
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the test's own folder
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
