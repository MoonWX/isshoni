package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/native"
)

func init() {
	register(command{name: "soak", summary: "one session for -hours, sampling private bytes, handles, GDI objects and threads every minute (≤ 5 % growth after 10 min, no RING_OVERFLOW, no ENDED)",
		run: cmdSoak})
}

// 04 §14's soak -hours H: one session; the process's private bytes, handles, GDI objects and threads are sampled
// every 60 s; after the first 10 minutes none of them grows by more than 5 %, the engine never reports RING_OVERFLOW
// and the session never ends (ENDED). README WA76; the owner runs `soak -hours 2` on the PC (04 §18.4), separately
// from hwcheck.ps1.
//
// Growth is measured from the first sample taken at or after the warm-up (-warmup, 10 min) to the last sample (taken
// when the run ends): (last − baseline) / baseline ≤ 5 % for every metric. A metric that is 0 at the baseline (GDI
// objects of a process without windows) must still be 0 at the end. The report also lists each metric's largest
// value after the baseline, for the reader; only the end value decides.
const (
	soakGrowthPercent    = 5
	defaultSoakEvery     = time.Minute
	defaultSoakWarmup    = 10 * time.Minute
	soakMaxHours         = 72.0
	soakStopTimeout      = 5 * time.Second
	ringOverflowLogBegin = "engine ring overflow" // internal/media/native logs RING_OVERFLOW at warn with this message
)

// soakOptions are soak's flags.
type soakOptions struct {
	hours   float64
	every   time.Duration
	warmup  time.Duration
	source  string
	encoder string
	profile string
	audio   string
	out     string
}

func (o soakOptions) duration() time.Duration { return time.Duration(o.hours * float64(time.Hour)) }

// procSample is one sample of the process (this one: the engine runs in it).
type procSample struct {
	AtS          float64     `json:"atS"` // seconds since the session started
	PrivateBytes uint64      `json:"privateBytes"`
	Handles      uint64      `json:"handles"`
	GDIObjects   uint64      `json:"gdiObjects"`
	Threads      uint64      `json:"threads"`
	Layers       []soakLayer `json:"layers,omitempty"` // the engine's stats at the sample, when it answered
}

// soakLayer is one layer of the engine's stats at a sample: enough to see a share that stalled or fell back.
type soakLayer struct {
	RID     string  `json:"rid"`
	Encoder string  `json:"encoder"`
	FPSOut  float64 `json:"fpsOut"`
	Drops   int     `json:"drops"`
}

// soakGrowth is one metric's verdict.
type soakGrowth struct {
	Metric    string  `json:"metric"`
	Baseline  uint64  `json:"baseline"`
	End       uint64  `json:"end"`
	Max       uint64  `json:"max"`
	Growth    float64 `json:"growth"`    // (end − baseline) / baseline; 0 when both are 0
	MaxGrowth float64 `json:"maxGrowth"` // (max − baseline) / baseline, for the reader
	Pass      bool    `json:"pass"`
}

// soakReport is soak.json and soak's -json output. It names no machine, user or path.
type soakReport struct {
	Command       string       `json:"command"`
	Hours         float64      `json:"hours"`
	EveryS        float64      `json:"everyS"`
	WarmupS       float64      `json:"warmupS"`
	Source        string       `json:"source"` // "test:pattern", "mon" or "pick": never a monitor or window name
	Encoder       string       `json:"encoderAsked"`
	Audio         string       `json:"audio"`
	RanS          float64      `json:"ranS"`
	Samples       []procSample `json:"samples"`
	Growth        []soakGrowth `json:"growth"`
	RingOverflows int64        `json:"ringOverflows"`
	Ended         string       `json:"ended,omitempty"`
	Failures      []string     `json:"failures"`
	Pass          bool         `json:"pass"`
}

// soakStarter starts the run's one session (*native.Engine's StartWith; tests fake it).
type soakStarter func(ctx context.Context, cfg media.Config) (media.Session, error)

// soakEnv is what runSoak needs: the session, the process sampler and the RING_OVERFLOW counter.
type soakEnv struct {
	start     soakStarter
	sample    func() (procSample, error)
	overflows func() int64
}

func cmdSoak(ctx context.Context, g *globals, args []string) int {
	fs := newFlags(g, "soak", "[-hours H] [-source test:pattern|mon:<id>|pick] [-encoder auto] [-audio tone] [-out DIR]")
	o := soakOptions{}
	fs.Float64Var(&o.hours, "hours", 2, "how long the session runs (04 §18.4: 2)")
	fs.DurationVar(&o.every, "every", defaultSoakEvery, "the sampling interval")
	fs.DurationVar(&o.warmup, "warmup", defaultSoakWarmup, "growth counts from the first sample after this")
	fs.StringVar(&o.source, "source", "test:pattern", "`pick`, mon:<id> or test:pattern")
	fs.StringVar(&o.encoder, "encoder", "auto", "auto, or a kind to force (04 §5.6's tokens)")
	fs.StringVar(&o.profile, "profile", "high", "high or cb")
	fs.StringVar(&o.audio, "audio", "tone", "off, tone (the test tone) or mix (exclude-apps)")
	fs.StringVar(&o.out, "out", ".", "write soak.json into `DIR`")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if code, ok := checkSoakOptions(g.stderr, o); !ok {
		return code
	}
	var overflows atomic.Int64
	e, code, err := openEngineWatched(g, o.audio != "mix", &overflows)
	if err != nil {
		fmt.Fprintln(g.stderr, "soak: the engine didn't load:", err)
		return code
	}
	defer func() { _ = e.Close() }()
	cfg, err := soakConfig(o)
	if err != nil {
		fmt.Fprintln(g.stderr, "soak:", err)
		return exitUsage
	}
	if o.source == "pick" {
		src, err := e.Pick(ctx, media.PickRequest{})
		if err != nil {
			fmt.Fprintln(g.stderr, "soak: pick:", err)
			return envCode(err)
		}
		cfg.Source, cfg.Listed = src.ID, false
	}
	env := soakEnv{
		start: func(ctx context.Context, cfg media.Config) (media.Session, error) {
			return e.StartWith(ctx, cfg)
		},
		sample:    sampleProcess,
		overflows: overflows.Load,
	}
	return runSoak(ctx, env, o, cfg, g.json, g.stdout, g.stderr)
}

func checkSoakOptions(stderr io.Writer, o soakOptions) (int, bool) {
	switch {
	case o.hours <= 0 || o.hours > soakMaxHours:
		fmt.Fprintf(stderr, "soak: -hours must be in (0, %g]\n", soakMaxHours)
		return exitUsage, false
	case o.every <= 0:
		fmt.Fprintln(stderr, "soak: -every must be positive")
		return exitUsage, false
	case o.warmup < 0 || o.warmup+o.every > o.duration():
		// At least two samples from the baseline on: the baseline and one later.
		fmt.Fprintln(stderr, "soak: -hours must cover -warmup plus one -every (growth needs two samples after the warm-up)")
		return exitUsage, false
	case o.audio != "off" && o.audio != "tone" && o.audio != "mix":
		fmt.Fprintln(stderr, "soak: -audio is off, tone or mix")
		return exitUsage, false
	case o.source != "pick" && o.source != "test:pattern" && !strings.HasPrefix(o.source, "mon:"):
		fmt.Fprintln(stderr, "soak: -source is pick, mon:<id> or test:pattern")
		return exitUsage, false
	}
	if _, ok := profileKey(o.profile); !ok {
		fmt.Fprintln(stderr, "soak: -profile is high or cb")
		return exitUsage, false
	}
	if _, ok := encoderKinds[o.encoder]; !ok {
		fmt.Fprintln(stderr, "soak: unknown -encoder", o.encoder)
		return exitUsage, false
	}
	return exitPass, true
}

// soakConfig is the session: capture's layers (f 1080p60, q 360p15) on the asked source, with the asked audio.
func soakConfig(o soakOptions) (media.Config, error) {
	profile, ok := profileKey(o.profile)
	if !ok {
		return media.Config{}, fmt.Errorf("-profile %q", o.profile)
	}
	cfg := media.Config{
		Source: o.source, Listed: true, Profile: profile, Preset: "auto", Layers: runLayers(defaultFBps),
		Borderless: true,
	}
	if o.encoder != "auto" {
		cfg.Encoder = o.encoder
	}
	switch o.audio {
	case "tone":
		cfg.Audio = &media.AudioConfig{Mode: media.AudioTestTone}
	case "mix":
		cfg.Audio = &media.AudioConfig{Mode: media.AudioExcludeApps}
	}
	return cfg, nil
}

// openEngineWatched is globals.openEngine with a logger that also counts the binding's RING_OVERFLOW warnings (the
// binding logs RING_OVERFLOW and passes nothing through Next, 04 §12.3).
func openEngineWatched(g *globals, noAudio bool, overflows *atomic.Int64) (*native.Engine, int, error) {
	level := slog.LevelWarn
	if g.verbose {
		level = slog.LevelDebug
	}
	inner := slog.NewTextHandler(g.stderr, &slog.HandlerOptions{Level: level})
	logger := slog.New(&overflowWatch{inner: inner, n: overflows})
	e, err := g.open(native.Options{Dir: g.dll, Logger: logger, LogLevel: level, NoAudio: noAudio})
	if err != nil {
		return nil, exitEnv, err
	}
	return e, exitPass, nil
}

// overflowWatch counts the records whose message starts with ringOverflowLog and passes every record on to inner.
type overflowWatch struct {
	inner slog.Handler
	n     *atomic.Int64
}

func (h *overflowWatch) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelWarn || h.inner.Enabled(ctx, l)
}

func (h *overflowWatch) Handle(ctx context.Context, r slog.Record) error {
	if strings.HasPrefix(r.Message, ringOverflowLogBegin) {
		h.n.Add(1)
	}
	if h.inner.Enabled(ctx, r.Level) {
		return h.inner.Handle(ctx, r)
	}
	return nil
}

func (h *overflowWatch) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &overflowWatch{inner: h.inner.WithAttrs(attrs), n: h.n}
}

func (h *overflowWatch) WithGroup(name string) slog.Handler {
	return &overflowWatch{inner: h.inner.WithGroup(name), n: h.n}
}

// runSoak runs the session for o.hours, sampling the process at the start, every o.every and at the end, then
// checks and prints the report and writes soak.json into o.out. Exit 0 pass, 1 a check failed, 2 when the session
// didn't start for an environment reason or the process can't be sampled.
func runSoak(ctx context.Context, env soakEnv, o soakOptions, cfg media.Config, asJSON bool, stdout,
	stderr io.Writer,
) int {
	sourceKind := o.source
	if strings.HasPrefix(o.source, "mon:") {
		sourceKind = "mon"
	}
	rep := soakReport{
		Command: "soak", Hours: o.hours, EveryS: o.every.Seconds(), WarmupS: o.warmup.Seconds(), Source: sourceKind,
		Encoder: o.encoder, Audio: o.audio, Samples: []procSample{}, Growth: []soakGrowth{}, Failures: []string{},
	}
	// A sample before Start proves the sampler works on this OS (an environment failure otherwise); the run's
	// samples start with the session.
	if _, err := env.sample(); err != nil {
		fmt.Fprintln(stderr, "soak: sampling the process:", err)
		return exitEnv
	}

	s, err := env.start(ctx, cfg)
	if err != nil {
		fmt.Fprintln(stderr, "soak: start:", err)
		return envCode(err)
	}
	started := time.Now()
	overflows0 := env.overflows()

	// The reader drains the session (the packets are dropped: the soak watches the process, not the stream) until
	// it is stopped or ends.
	readCtx, cancelRead := context.WithCancel(ctx)
	var ended error
	var endedMu sync.Mutex
	endedCh := make(chan struct{})
	go func() {
		defer close(endedCh)
		for {
			if _, err := s.Next(readCtx); err != nil {
				if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) &&
					!errors.Is(err, media.ErrStopped) {
					endedMu.Lock()
					ended = err
					endedMu.Unlock()
				}
				return
			}
		}
	}()

	take := func() {
		smp, err := env.sample()
		if err != nil {
			rep.Failures = append(rep.Failures, fmt.Sprintf("sampling at %.0f s: %v", time.Since(started).Seconds(), err))
			return
		}
		smp.AtS = time.Since(started).Seconds()
		if st, err := s.Stats(); err == nil {
			for _, l := range st.Layers {
				smp.Layers = append(smp.Layers, soakLayer{RID: l.RID, Encoder: l.Encoder, FPSOut: l.FPSOut, Drops: l.Drops})
			}
		}
		rep.Samples = append(rep.Samples, smp)
	}
	take()
	deadline := time.NewTimer(o.duration())
	defer deadline.Stop()
	tick := time.NewTicker(o.every)
	defer tick.Stop()
	interrupted := false
loop:
	for {
		select {
		case <-ctx.Done():
			interrupted = true
			break loop
		case <-endedCh:
			break loop
		case <-deadline.C:
			break loop
		case <-tick.C:
			take()
		}
	}
	take()
	rep.RanS = time.Since(started).Seconds()
	stopDone := make(chan struct{})
	go func() {
		_ = s.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(soakStopTimeout):
		rep.Failures = append(rep.Failures, fmt.Sprintf("Stop took more than %v", soakStopTimeout))
	}
	cancelRead()
	<-endedCh
	endedMu.Lock()
	if ended != nil {
		rep.Ended = ended.Error()
		rep.Failures = append(rep.Failures, "the session ended (ENDED): "+ended.Error())
	}
	endedMu.Unlock()
	rep.RingOverflows = env.overflows() - overflows0
	if rep.RingOverflows > 0 {
		rep.Failures = append(rep.Failures, fmt.Sprintf("the engine reported RING_OVERFLOW %d times", rep.RingOverflows))
	}
	if interrupted {
		rep.Failures = append(rep.Failures, fmt.Sprintf("interrupted after %.0f s of %.0f", rep.RanS,
			o.duration().Seconds()))
	}
	growth, fails := evalSoak(rep.Samples, o.warmup)
	rep.Growth = growth
	rep.Failures = append(rep.Failures, fails...)
	rep.Pass = len(rep.Failures) == 0

	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, "soak:", err)
		return exitFail
	}
	if err := os.MkdirAll(o.out, 0o750); err != nil {
		fmt.Fprintln(stderr, "soak:", err)
		return exitEnv
	}
	if err := os.WriteFile(filepath.Join(o.out, "soak.json"), append(data, '\n'), 0o600); err != nil {
		fmt.Fprintln(stderr, "soak:", err)
		return exitEnv
	}
	if asJSON {
		_, _ = stdout.Write(append(data, '\n'))
	} else {
		printSoak(stdout, rep)
	}
	if rep.Pass {
		return exitPass
	}
	return exitFail
}

// soakMetrics are the sampled metrics in the report's order.
var soakMetrics = []struct {
	name string
	get  func(procSample) uint64
}{
	{"privateBytes", func(s procSample) uint64 { return s.PrivateBytes }},
	{"handles", func(s procSample) uint64 { return s.Handles }},
	{"gdiObjects", func(s procSample) uint64 { return s.GDIObjects }},
	{"threads", func(s procSample) uint64 { return s.Threads }},
}

// evalSoak is the growth check: the baseline is the first sample at or after warmup, the end the last sample; each
// metric may grow by at most soakGrowthPercent % from the baseline to the end. It needs a sample after the baseline.
func evalSoak(samples []procSample, warmup time.Duration) ([]soakGrowth, []string) {
	base := -1
	for i, s := range samples {
		if s.AtS >= warmup.Seconds() {
			base = i
			break
		}
	}
	if base < 0 || base == len(samples)-1 {
		return []soakGrowth{}, []string{fmt.Sprintf("no two samples at or after the warm-up of %v: the run was too short",
			warmup)}
	}
	after := samples[base:]
	out := make([]soakGrowth, 0, len(soakMetrics))
	var fails []string
	for _, m := range soakMetrics {
		g := soakGrowth{Metric: m.name, Baseline: m.get(after[0]), End: m.get(after[len(after)-1])}
		for _, s := range after {
			g.Max = max(g.Max, m.get(s))
		}
		if g.Baseline == 0 {
			// Any growth from 0 is unbounded; JSON has no infinity, so it is reported as -1 with pass false.
			g.Pass = g.End == 0
			if !g.Pass {
				g.Growth, g.MaxGrowth = -1, -1
			}
		} else {
			g.Growth = float64(g.End)/float64(g.Baseline) - 1
			g.MaxGrowth = float64(g.Max)/float64(g.Baseline) - 1
			// In integers, so that exactly 5 % passes: end × 100 ≤ baseline × 105.
			g.Pass = g.End*100 <= g.Baseline*(100+soakGrowthPercent)
		}
		if !g.Pass {
			fails = append(fails, fmt.Sprintf("%s grew from %d to %d after the warm-up (limit %.0f %%)", m.name,
				g.Baseline, g.End, float64(soakGrowthPercent)))
		}
		out = append(out, g)
	}
	return out, fails
}

func printSoak(w io.Writer, r soakReport) {
	fmt.Fprintf(w, "soak: %s, %.2f h asked, ran %.0f s, encoder %s, audio %s, %d samples every %.0f s\n", r.Source,
		r.Hours, r.RanS, r.Encoder, r.Audio, len(r.Samples), r.EveryS)
	if n := len(r.Samples); n > 0 {
		last := r.Samples[n-1]
		for _, l := range last.Layers {
			fmt.Fprintf(w, "layer %s at the end: %s, fpsOut %.1f, drops %d\n", l.RID, l.Encoder, l.FPSOut, l.Drops)
		}
	}
	for _, g := range r.Growth {
		fmt.Fprintf(w, "%-12s baseline %d, end %d (%+.1f %%), max %d\n", g.Metric, g.Baseline, g.End, 100*g.Growth,
			g.Max)
	}
	fmt.Fprintf(w, "RING_OVERFLOW: %d\n", r.RingOverflows)
	for _, f := range r.Failures {
		fmt.Fprintln(w, "FAIL:", f)
	}
	if r.Pass {
		fmt.Fprintln(w, "PASS")
	}
}
