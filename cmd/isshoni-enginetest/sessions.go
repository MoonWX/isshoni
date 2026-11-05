package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/native"
)

func init() {
	register(command{name: "sessions", summary: "run -count sessions of test:pattern (1080p60, f and q) at once and report each layer's encoder (M-NVENC-LIMIT)",
		run: cmdSessions})
}

// 04 §14's sessions -count N: N sessions of test:pattern at 1080p60 with two layers, through StartExtra, all running
// at once; it reports each layer's encoder and passes when every session started and ran to the end (fallbacks
// allowed: NVENC's limit is per system, shared with OBS and others, and a layer past it takes MF, 04 §9.7; a session
// that ENDED mid-run, e.g. a layer out of candidates, isn't one that works at the limit). The DLL runs at most 4 sessions
// per process (04 §6.4), so sessions beyond the first 4 run in helper processes, copies of this program with the same
// -dll (`sessions -child`, 4 sessions each at most), started once this process's sessions run and holding theirs for
// the same time; this process keeps its sessions until every helper is done, so all N run together.
const (
	maxSessionsPerProcess = 4 // 04 §6.4
	maxSessionsCount      = 16
	// How long a started session may take to report both layers (EventLayer). Its first packets come within 500 ms of
	// Start (04 §6.3); this leaves room for a loaded PC.
	sessionLayersWait = 3 * time.Second
)

// sessionsOptions are sessions' flags. child and first are for the helper processes only.
type sessionsOptions struct {
	count   int
	seconds float64
	encoder string
	profile string
	child   bool
	first   int // the index of this process's first session
}

// sessionLayer is one layer of one session as the report shows it: its encoder from the EventLayer events (the last
// one, after any fallback) and the encoders it fell back to during the run.
type sessionLayer struct {
	RID     string   `json:"rid"`
	Encoder string   `json:"encoder"` // "" when the layer reported nothing within sessionLayersWait
	HW      bool     `json:"hw"`
	Profile string   `json:"profile,omitempty"`
	Width   int      `json:"width,omitempty"`
	Height  int      `json:"height,omitempty"`
	FPS     int      `json:"fps,omitempty"`
	Changes []string `json:"changes,omitempty"` // ENCODER_CHANGED's new encoders, in order
}

// sessionResult is one session. Process 0 is this process, 1.. the helpers.
type sessionResult struct {
	Index   int            `json:"index"`
	Process int            `json:"process"`
	Started bool           `json:"started"`
	Error   string         `json:"error,omitempty"`
	Code    string         `json:"code,omitempty"`  // media.Error's code when the start failed
	Ended   string         `json:"ended,omitempty"` // the session ended during the run
	Layers  []sessionLayer `json:"layers,omitempty"`
}

// sessionsReport is what sessions prints (with -json, also each helper's answer to its parent).
type sessionsReport struct {
	Command  string          `json:"command"`
	Count    int             `json:"count"`
	Seconds  float64         `json:"seconds"`
	Encoder  string          `json:"encoder"`
	Profile  string          `json:"profile"`
	Started  int             `json:"started"`
	Sessions []sessionResult `json:"sessions"`
	// Encoders counts the layers per encoder token (the last one of each layer).
	Encoders map[string]int `json:"encoders"`
	Failures []string       `json:"failures"`
	Pass     bool           `json:"pass"`
}

// extraStarter is what sessions needs of the engine (*native.Engine; tests fake it).
type extraStarter interface {
	StartExtra(ctx context.Context, cfg media.Config, opts ...native.StartOption) (media.Session, error)
}

// helperRunner runs one helper process with args (the global flags, then the command) and returns its stdout and exit
// code; err is set when it couldn't run or didn't answer.
type helperRunner func(ctx context.Context, args []string) (stdout []byte, code int, err error)

func cmdSessions(ctx context.Context, g *globals, args []string) int {
	fs := newFlags(g, "sessions", "[-count N] [-seconds S] [-encoder auto] [-profile high]")
	o := sessionsOptions{}
	fs.IntVar(&o.count, "count", 5, "sessions at once (1.."+strconv.Itoa(maxSessionsCount)+"; M-NVENC-LIMIT: 5)")
	fs.Float64Var(&o.seconds, "seconds", 5, "how long every session runs")
	fs.StringVar(&o.encoder, "encoder", "auto", "auto, or a kind to force (04 §5.6's tokens)")
	fs.StringVar(&o.profile, "profile", "high", "high or cb")
	fs.BoolVar(&o.child, "child", false, "a helper process of another sessions run (at most 4 sessions)")
	fs.IntVar(&o.first, "first", 0, "the helper's first session index")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if code, ok := checkSessionsOptions(g.stderr, o); !ok {
		return code
	}
	e, code, err := g.openEngine(true) // video only: StartExtra runs no audio
	if err != nil {
		fmt.Fprintln(g.stderr, "sessions: the engine didn't load:", err)
		return code
	}
	defer func() { _ = e.Close() }()
	return runSessionsCommand(ctx, e, o, g.helperRunner(), g.json, g.stdout, g.stderr)
}

func checkSessionsOptions(stderr io.Writer, o sessionsOptions) (int, bool) {
	limit := maxSessionsCount
	if o.child {
		limit = maxSessionsPerProcess
	}
	switch {
	case o.count < 1 || o.count > limit:
		fmt.Fprintf(stderr, "sessions: -count must be 1..%d\n", limit)
		return exitUsage, false
	case o.seconds <= 0 || o.seconds > 3600:
		fmt.Fprintln(stderr, "sessions: -seconds must be in (0, 3600]")
		return exitUsage, false
	case o.first < 0 || o.first > maxSessionsCount:
		fmt.Fprintln(stderr, "sessions: -first out of range")
		return exitUsage, false
	}
	if _, ok := profileKey(o.profile); !ok {
		fmt.Fprintln(stderr, "sessions: -profile is high or cb")
		return exitUsage, false
	}
	if _, ok := encoderKinds[o.encoder]; !ok {
		fmt.Fprintln(stderr, "sessions: unknown -encoder", o.encoder)
		return exitUsage, false
	}
	return exitPass, true
}

// helperRunner starts copies of this executable with this run's -dll and -v (the helpers' stderr is this process's).
func (g *globals) helperRunner() helperRunner {
	return func(ctx context.Context, args []string) ([]byte, int, error) {
		exe, err := os.Executable()
		if err != nil {
			return nil, 0, err
		}
		full := []string{}
		if g.dll != "" {
			full = append(full, "-dll", g.dll)
		}
		if g.verbose {
			full = append(full, "-v")
		}
		cmd := exec.CommandContext(ctx, exe, append(full, args...)...) //nolint:gosec // G204: this program itself
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = g.stderr
		err = cmd.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out.Bytes(), ee.ExitCode(), nil
		}
		if err != nil {
			return nil, 0, err
		}
		return out.Bytes(), exitPass, nil
	}
}

// sessionsConfig is one session's media.Config: test:pattern, f at 1080p60 and q at 360p15 (capture's layers).
func sessionsConfig(o sessionsOptions) media.Config {
	profile, _ := profileKey(o.profile)
	cfg := media.Config{
		Source: "test:pattern", Listed: true, Profile: profile, Preset: "auto", Layers: runLayers(defaultFBps),
		Borderless: true,
	}
	if o.encoder != "auto" {
		cfg.Encoder = o.encoder
	}
	return cfg
}

// runSessionsCommand runs this process's sessions (the first 4, or a helper's share), the helpers for the rest, and
// prints the report. Exit 0 when every session started and none ended during the run, 1 otherwise (or when a helper
// failed), the environment's
// code when none did (no encoder, a DLL without the test pattern).
func runSessionsCommand(ctx context.Context, st extraStarter, o sessionsOptions, helper helperRunner, asJSON bool,
	stdout, stderr io.Writer,
) int {
	own := min(o.count, maxSessionsPerProcess)
	hold := time.Duration(o.seconds * float64(time.Second))
	run := startSessions(ctx, st, o, own)

	var helpers []sessionsReport
	var failures []string
	if !o.child && o.count > own {
		helpers, failures = runHelpers(ctx, o, own, helper)
	}
	// This process's sessions run for -seconds, and at least until every helper is done (they hold theirs as long).
	select {
	case <-ctx.Done():
	case <-time.After(time.Until(run.started.Add(hold))):
	}
	results := run.stop()

	rep := sessionsReport{
		Command: "sessions", Count: o.count, Seconds: o.seconds, Encoder: o.encoder, Profile: o.profile,
		Encoders: map[string]int{}, Failures: failures,
	}
	rep.Sessions = append(rep.Sessions, results...)
	for i, h := range helpers {
		for _, r := range h.Sessions {
			r.Process = i + 1
			rep.Sessions = append(rep.Sessions, r)
		}
		rep.Failures = append(rep.Failures, h.Failures...)
	}
	sort.SliceStable(rep.Sessions, func(a, b int) bool { return rep.Sessions[a].Index < rep.Sessions[b].Index })
	var firstErr error
	for _, r := range rep.Sessions {
		if r.Started {
			rep.Started++
			if r.Ended != "" {
				rep.Failures = append(rep.Failures, fmt.Sprintf("session %d ended during the run: %s", r.Index, r.Ended))
			}
		} else {
			rep.Failures = append(rep.Failures, fmt.Sprintf("session %d didn't start: %s", r.Index, r.Error))
		}
		for _, l := range r.Layers {
			if l.Encoder != "" {
				rep.Encoders[l.Encoder]++
			} else {
				rep.Failures = append(rep.Failures, fmt.Sprintf("session %d layer %s reported no encoder within %v",
					r.Index, l.RID, sessionLayersWait))
			}
		}
	}
	if len(rep.Sessions) != o.count {
		rep.Failures = append(rep.Failures, fmt.Sprintf("%d of %d sessions reported", len(rep.Sessions), o.count))
	}
	for _, r := range results {
		if !r.Started && firstErr == nil {
			firstErr = run.errs[r.Index]
		}
	}
	if rep.Failures == nil {
		rep.Failures = []string{}
	}
	rep.Pass = len(rep.Failures) == 0
	if asJSON {
		data, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			fmt.Fprintln(stderr, "sessions:", err)
			return exitFail
		}
		_, _ = stdout.Write(append(data, '\n'))
	} else {
		printSessions(stdout, rep)
	}
	switch {
	case rep.Pass:
		return exitPass
	case rep.Started == 0 && firstErr != nil:
		return envCode(firstErr)
	}
	return exitFail
}

// sessionsRun is this process's running sessions.
type sessionsRun struct {
	started time.Time
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	mu      sync.Mutex
	results []sessionResult // by position; guarded by mu while the readers run
	errs    map[int]error   // the start errors by session index
	live    []media.Session
}

// startSessions starts n sessions one after the other (im_start runs on the engine's one worker anyway), each read by
// its own goroutine, and waits until each started one reported both layers or sessionLayersWait passed.
func startSessions(ctx context.Context, st extraStarter, o sessionsOptions, n int) *sessionsRun {
	readCtx, cancel := context.WithCancel(ctx)
	run := &sessionsRun{cancel: cancel, errs: map[int]error{}}
	cfg := sessionsConfig(o)
	for i := 0; i < n; i++ {
		r := sessionResult{Index: o.first + i}
		s, err := st.StartExtra(ctx, cfg)
		if err != nil {
			r.Error = err.Error()
			var me *media.Error
			if errors.As(err, &me) {
				r.Code = string(me.Code)
			}
			run.errs[r.Index] = err
			run.add(r)
			continue
		}
		r.Started = true
		for _, lc := range cfg.Layers {
			r.Layers = append(r.Layers, sessionLayer{RID: lc.RID})
		}
		pos := run.add(r)
		run.live = append(run.live, s)
		run.wg.Add(1)
		go func() {
			defer run.wg.Done()
			run.read(readCtx, s, pos)
		}()
	}
	run.started = time.Now()
	deadline := time.Now().Add(sessionLayersWait)
	for time.Now().Before(deadline) && ctx.Err() == nil && !run.allLayersKnown() {
		time.Sleep(20 * time.Millisecond)
	}
	return run
}

// add appends a result (the readers of the sessions started before it run) and returns its position.
func (run *sessionsRun) add(r sessionResult) int {
	run.mu.Lock()
	defer run.mu.Unlock()
	run.results = append(run.results, r)
	return len(run.results) - 1
}

// read drains one session, recording its layers' EventLayer and ENCODER_CHANGED events, until ctx ends or the session
// ends.
func (run *sessionsRun) read(ctx context.Context, s media.Session, pos int) {
	for {
		p, err := s.Next(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) &&
				!errors.Is(err, media.ErrStopped) {
				run.mu.Lock()
				run.results[pos].Ended = err.Error()
				run.mu.Unlock()
			}
			return
		}
		if p.Kind != media.KindEvent || p.Event == nil {
			continue
		}
		ev := p.Event
		run.mu.Lock()
		for i := range run.results[pos].Layers {
			l := &run.results[pos].Layers[i]
			if l.RID != ev.Layer {
				continue
			}
			switch ev.Code {
			case media.EventLayer:
				l.Encoder, l.HW, l.Profile, l.Width, l.Height, l.FPS = ev.Encoder, ev.HW, ev.Profile, ev.Width,
					ev.Height, ev.FPS
			case media.EventEncoderChanged:
				l.Changes = append(l.Changes, ev.Encoder)
			}
		}
		run.mu.Unlock()
	}
}

func (run *sessionsRun) allLayersKnown() bool {
	run.mu.Lock()
	defer run.mu.Unlock()
	for _, r := range run.results {
		for _, l := range r.Layers {
			if l.Encoder == "" {
				return false
			}
		}
	}
	return true
}

// stop stops every session, ends the readers and returns the results.
func (run *sessionsRun) stop() []sessionResult {
	for _, s := range run.live {
		_ = s.Stop()
	}
	run.cancel()
	run.wg.Wait()
	run.mu.Lock()
	defer run.mu.Unlock()
	return append([]sessionResult(nil), run.results...)
}

// runHelpers runs the sessions after this process's own in helper processes, 4 each, at the same time, and returns
// their reports and what went wrong running them.
func runHelpers(ctx context.Context, o sessionsOptions, own int, helper helperRunner) ([]sessionsReport, []string) {
	type answer struct {
		rep    sessionsReport
		parsed bool // rep is the helper's whole report
		err    string
	}
	var starts []int
	for first := own; first < o.count; first += maxSessionsPerProcess {
		starts = append(starts, first)
	}
	answers := make([]answer, len(starts))
	var wg sync.WaitGroup
	for i, first := range starts {
		count := min(maxSessionsPerProcess, o.count-first)
		args := []string{"-json", "sessions", "-child", "-first", strconv.Itoa(first), "-count", strconv.Itoa(count),
			"-seconds", strconv.FormatFloat(o.seconds, 'f', -1, 64), "-encoder", o.encoder, "-profile", o.profile}
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, code, err := helper(ctx, args)
			if err != nil {
				answers[i].err = fmt.Sprintf("helper process %d didn't run: %v", i+1, err)
				return
			}
			var rep sessionsReport
			if jerr := json.Unmarshal(out, &rep); jerr != nil {
				// A partly decoded report is no report: rep is dropped, its sessions get placeholders below.
				answers[i].err = fmt.Sprintf("helper process %d (exit %d) gave no report: %v", i+1, code, jerr)
				return
			}
			answers[i].rep = rep
			answers[i].parsed = true
			if code != exitPass && rep.Pass {
				answers[i].err = fmt.Sprintf("helper process %d exited %d", i+1, code)
			}
		}()
	}
	wg.Wait()
	var reps []sessionsReport
	var failures []string
	for i, a := range answers {
		if a.err != "" {
			failures = append(failures, a.err)
		}
		if !a.parsed {
			// Its sessions are unknown: they count as not started. (A report that parsed keeps its sessions even when
			// the exit code disagrees with it; that disagreement is the failure above.)
			a.rep = sessionsReport{}
			for first, n := starts[i], min(maxSessionsPerProcess, o.count-starts[i]); n > 0; first, n = first+1, n-1 {
				a.rep.Sessions = append(a.rep.Sessions, sessionResult{Index: first, Error: "its helper process failed"})
			}
		}
		reps = append(reps, a.rep)
	}
	return reps, failures
}

func printSessions(w io.Writer, rep sessionsReport) {
	for _, r := range rep.Sessions {
		if !r.Started {
			fmt.Fprintf(w, "session %d (process %d): not started: %s\n", r.Index, r.Process, r.Error)
			continue
		}
		parts := make([]string, 0, len(r.Layers))
		for _, l := range r.Layers {
			enc := l.Encoder
			if enc == "" {
				enc = "(no report)"
			}
			s := fmt.Sprintf("%s %s", l.RID, enc)
			if len(l.Changes) > 0 {
				s += " (fell back to " + strings.Join(l.Changes, ", ") + ")"
			}
			parts = append(parts, s)
		}
		fmt.Fprintf(w, "session %d (process %d): %s", r.Index, r.Process, strings.Join(parts, ", "))
		if r.Ended != "" {
			fmt.Fprintf(w, "; ended: %s", r.Ended)
		}
		fmt.Fprintln(w)
	}
	tokens := make([]string, 0, len(rep.Encoders))
	for t := range rep.Encoders {
		tokens = append(tokens, t)
	}
	sort.Strings(tokens)
	counts := make([]string, 0, len(tokens))
	for _, t := range tokens {
		counts = append(counts, fmt.Sprintf("%s %d", t, rep.Encoders[t]))
	}
	fmt.Fprintf(w, "%d of %d sessions started; layers per encoder: %s\n", rep.Started, rep.Count,
		strings.Join(counts, ", "))
	for _, f := range rep.Failures {
		fmt.Fprintln(w, "FAIL:", f)
	}
	if rep.Pass {
		fmt.Fprintln(w, "PASS")
	}
}
