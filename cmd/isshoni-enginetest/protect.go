package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/native"
)

func init() {
	register(command{name: "protect-check",
		summary: "a protected pattern window is absent from DD and WGC monitor captures, and present once unprotected",
		run:     cmdProtectCheck})
}

// protect-check is 04 §14's (README WA70, 04 E12, §11): it creates a window with a known pattern, protects it through
// the engine (ProtectWindow, im_protect_window), captures its monitor with Desktop Duplication (the listed mon: source)
// and with WGC (the monitor picked in the OS picker), then unprotects it and captures again; the pattern must be absent
// while protected and present after.
//
// The pattern is noise: a topmost, never-activated window over a quarter of the monitor shows new random blocks about
// 60 times a second. The tool reads the engine's H.264, not pixels (the shipped DLL has no luma tap), so presence is
// measured by what the noise costs the encoder: the slice bytes of layer f's non-keyframe access units per second,
// counted by capture time. Shown, the noise makes every frame expensive (the encoder spends about its whole target);
// left out, the monitor behind it is still, and inter frames cost next to nothing. A path passes when the unprotected
// rate is at least protectMinShownBps and the protected one at most protectMaxRatio of it. Periodic IDRs and re-inits
// are left out of both, and SEI or filler NAL units are not counted. The desktop behind the window must be still
// during the run (no video playing, no animation): the output says so before it starts.
//
// One session per path: the window is protected before Start and unprotected half-way, so a pick token (single use)
// serves both halves. The unprotect is SetWindowDisplayAffinity(WDA_NONE) by the tool itself: the binding has no
// IM_PROTECT_OFF call (04 §12.2 lists ProtectWindow only), and the C++ tests cover the flag.
//
// WGC: on Windows 11 a picked monitor is captured with WGC. On Windows 10 a picked monitor maps to Desktop
// Duplication (04 §7.5), and WGC captures it only when the mapping fails; such a run reports WGC as "not_reachable"
// (the DD path covers the monitor). -wgc off skips the pick (unattended runs). The pattern window is hidden while
// the picker is open, so it never covers the picker.
//
// WGC is also "not_reachable" when a WGC start fell back to Desktop Duplication (04 §7.1, §7.5): the verdict follows
// the path the session runs on (im_session_info.capture_path), not the pick's planned one. A picked monitor other than
// the noise window's is an operator error (exit 2), not a failure of the engine.
//
// Exit codes: 0 when DD passed and WGC passed, was not reachable or was off; 1 when a path failed (the pattern showed
// while protected, did not show after, or the affinity did not read back); 2 for the environment (the engine, the
// window, an OS below build 19041, a source that can't start, a cancelled pick, the wrong monitor picked, an
// interrupted run).

// The thresholds of a path's verdict.
const (
	// protectMinShownBps: the unprotected window's noise must cost layer f at least this much, else the pattern is
	// not "present" (the window is covered or off the captured monitor).
	protectMinShownBps = 500_000
	// protectMaxRatio: the protected rate may be at most this share of the unprotected one.
	protectMaxRatio = 0.2
	// protectBitrate is layer f's target: well above protectMinShownBps, so the noise can show it.
	protectBitrate = 6_000_000
	// wdaExcludeFromCapture is WDA_EXCLUDEFROMCAPTURE, the affinity ProtectWindow sets.
	wdaExcludeFromCapture = 0x11
)

// protectOptions are protect-check's flags.
type protectOptions struct {
	seconds float64       // the length of each half (protected, then unprotected)
	warmup  time.Duration // from Start to the protected half: the first IDR and the encoder's ramp
	settle  time.Duration // from the unprotect to the unprotected half
	wgc     string        // "pick" or "off"
}

// patternWindow is the noise window (protect_windows.go; tests fake it).
type patternWindow interface {
	// Handle is the window's HWND.
	Handle() uintptr
	// MonitorID is the listed id of the monitor the window is on: "mon:" + its DeviceName (04 §5.6).
	MonitorID() (string, error)
	// Affinity is GetWindowDisplayAffinity's value.
	Affinity() (uint32, error)
	// Unprotect is SetWindowDisplayAffinity(WDA_NONE).
	Unprotect() error
	// SetVisible shows the window (without activating it) or hides it.
	SetVisible(visible bool)
	Close()
}

// sessionCapturePath is the capture path a started session runs on (native.CapturePath; tests fake it).
var sessionCapturePath = native.CapturePath

// newPatternWindow creates the shown noise window on the primary monitor (protect_windows.go); off Windows it fails.
var newPatternWindow = createPatternWindow

// protectEngine is what protect-check needs of the engine (*native.Engine; tests fake it).
type protectEngine interface {
	ProtectWindow(hwnd uintptr) error
	Pick(ctx context.Context, req media.PickRequest) (media.Source, error)
	Release(id string)
	Start(ctx context.Context, cfg media.Config) (media.Session, error)
}

// protectPath is one capture path's result. It names no monitor or window (output may go to a public PR).
type protectPath struct {
	Path        string  `json:"path"`              // "dd" or "wgc": the path asked for
	Capture     string  `json:"capture,omitempty"` // the source's capture path ("dd", "wgc")
	Outcome     string  `json:"outcome"`           // "pass", "fail", "error", "not_reachable", "cancelled", "off"
	Reason      string  `json:"reason,omitempty"`
	Encoder     string  `json:"encoder,omitempty"`
	ProtectedBp float64 `json:"protectedBps"`
	ShownBps    float64 `json:"unprotectedBps"`
	Protected   int     `json:"protectedFrames"`
	Shown       int     `json:"unprotectedFrames"`
	err         error
}

// protectReport is protect-check's -json output.
type protectReport struct {
	Paths []protectPath `json:"paths"`
	Pass  bool          `json:"pass"`
}

func cmdProtectCheck(ctx context.Context, g *globals, args []string) int {
	fs := newFlags(g, "protect-check", "[-seconds S] [-wgc pick|off]")
	o := protectOptions{warmup: 1500 * time.Millisecond, settle: 700 * time.Millisecond}
	fs.Float64Var(&o.seconds, "seconds", 3, "the length of each half (protected, then unprotected), 1 to 60")
	fs.StringVar(&o.wgc, "wgc", "pick", "pick: pick the pattern's monitor in the OS picker for the WGC path; off: skip it")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if o.seconds < 1 || o.seconds > 60 {
		fmt.Fprintln(g.stderr, "protect-check: -seconds must be 1..60")
		return exitUsage
	}
	if o.wgc != "pick" && o.wgc != "off" {
		fmt.Fprintln(g.stderr, "protect-check: -wgc is pick or off")
		return exitUsage
	}
	e, code, err := g.openEngine(true)
	if err != nil {
		fmt.Fprintln(g.stderr, "protect-check: the engine didn't load:", err)
		return code
	}
	defer func() { _ = e.Close() }()
	w, err := newPatternWindow()
	if err != nil {
		fmt.Fprintln(g.stderr, "protect-check: the pattern window:", err)
		return exitEnv
	}
	defer w.Close()
	return runProtectCheck(ctx, e, w, o, g.json, g.stdout, g.stderr)
}

// runProtectCheck runs the DD path, then the WGC path, and prints the report.
func runProtectCheck(ctx context.Context, e protectEngine, w patternWindow, o protectOptions, asJSON bool,
	stdout, stderr io.Writer) int {
	if !asJSON {
		fmt.Fprintln(stdout, "protect-check: keep the desktop behind the noise window still (no video, no animation)")
	}
	rep := protectReport{}
	mon, err := w.MonitorID()
	if err != nil {
		// The environment (exit 2): no monitor to capture.
		rep.Paths = append(rep.Paths, protectPath{Path: "dd", Outcome: "error", Reason: "monitor: " + err.Error()})
	} else {
		rep.Paths = append(rep.Paths, protectRun(ctx, e, w, "dd", "dd", mon, true, o))
	}
	if ctx.Err() == nil {
		rep.Paths = append(rep.Paths, protectWGC(ctx, e, w, mon, o, stdout, asJSON))
	}

	code := exitPass
	for _, p := range rep.Paths {
		switch p.Outcome {
		case "fail":
			code = exitFail
		case "error":
			if code == exitPass {
				code = envCode(p.err)
				// An interrupt (Ctrl-C) is no failed assertion: 1 is only for those (04 §14).
				if p.err == nil || errors.Is(p.err, context.Canceled) || ctx.Err() != nil {
					code = exitEnv
				}
			}
		case "cancelled":
			if code == exitPass {
				code = exitEnv
			}
		}
	}
	if ctx.Err() != nil && code == exitPass {
		code = exitEnv
	}
	rep.Pass = code == exitPass
	if asJSON {
		data, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			fmt.Fprintln(stderr, "protect-check:", err)
			return exitFail
		}
		_, _ = stdout.Write(append(data, '\n'))
	} else {
		for _, p := range rep.Paths {
			printProtectPath(stdout, p)
		}
		if rep.Pass {
			fmt.Fprintln(stdout, "protect-check: pass")
		} else {
			fmt.Fprintln(stdout, "protect-check: FAIL")
		}
	}
	return code
}

// protectWGC is the WGC path: the user picks the pattern's monitor with the window hidden; a pick the engine captures
// with Desktop Duplication (Windows 10's mapping) makes WGC not reachable. mon is the noise window's monitor id ("" if
// unknown): a pick of another monitor (media.Source.MonitorID, filled for a mapped monitor pick) is an operator error.
func protectWGC(ctx context.Context, e protectEngine, w patternWindow, mon string, o protectOptions,
	stdout io.Writer, asJSON bool) protectPath {
	if o.wgc == "off" {
		return protectPath{Path: "wgc", Outcome: "off"}
	}
	if !asJSON {
		fmt.Fprintln(stdout, "protect-check: pick the monitor the noise window was on (the window is hidden meanwhile)")
	}
	w.SetVisible(false)
	src, err := e.Pick(ctx, media.PickRequest{})
	w.SetVisible(true)
	switch {
	case errors.Is(err, media.ErrCancelled) || errors.Is(err, context.Canceled):
		return protectPath{Path: "wgc", Outcome: "cancelled", Reason: "the pick was cancelled"}
	case err != nil:
		return protectPath{Path: "wgc", Outcome: "error", Reason: "pick: " + err.Error(), err: err}
	case src.Kind != media.SourceScreen:
		e.Release(src.ID)
		return protectPath{Path: "wgc", Capture: src.Capture, Outcome: "error",
			Reason: "the pick is a window, not a monitor: pick the monitor the noise window is on"}
	case mon != "" && src.MonitorID != "" && src.MonitorID != mon:
		e.Release(src.ID)
		return protectPath{Path: "wgc", Capture: src.Capture, Outcome: "error",
			Reason: "the picked monitor is not the one the noise window is on"}
	case src.Capture != "wgc":
		e.Release(src.ID)
		return protectPath{Path: "wgc", Capture: src.Capture, Outcome: "not_reachable",
			Reason: "the picked monitor is captured with " + src.Capture +
				" (Windows 10 maps a monitor pick to Desktop Duplication, 04 §7.5): WGC isn't reachable for it"}
	}
	return protectRun(ctx, e, w, "wgc", src.Capture, src.ID, false, o)
}

// protectSample is one of layer f's non-keyframe access units: its capture time and its slice bytes.
type protectSample struct {
	captureNS int64
	bytes     int
}

// protectRun protects the window, starts a session on source, measures the protected half, unprotects the window,
// measures the unprotected half and stops the session.
func protectRun(ctx context.Context, e protectEngine, w patternWindow, path, capture, source string, listed bool,
	o protectOptions) protectPath {
	r := protectPath{Path: path, Capture: capture}
	fail := func(outcome, reason string, err error) protectPath {
		r.Outcome, r.Reason, r.err = outcome, reason, err
		return r
	}
	if err := e.ProtectWindow(w.Handle()); err != nil {
		return fail("error", "ProtectWindow: "+err.Error(), err)
	}
	if a, err := w.Affinity(); err != nil {
		return fail("error", "GetWindowDisplayAffinity: "+err.Error(), err)
	} else if a != wdaExcludeFromCapture {
		return fail("fail", fmt.Sprintf("the affinity reads %#x after ProtectWindow, want WDA_EXCLUDEFROMCAPTURE", a), nil)
	}
	cfg := media.Config{
		Source: source, Listed: listed, Profile: "6400", Preset: "auto", Borderless: true,
		Layers: []media.LayerConfig{{RID: "f", MaxPixels: 1920 * 1080, MaxFPS: 30, Bitrate: protectBitrate}},
	}
	s, err := e.Start(ctx, cfg)
	if err != nil {
		if !listed {
			e.Release(source)
		}
		_ = w.Unprotect()
		return fail("error", "start: "+err.Error(), err)
	}
	if path == "wgc" {
		if running := sessionCapturePath(s); running != "wgc" {
			// A WGC start that fails falls back to DD on the same output (04 §7.1, §7.5): that is no WGC run.
			_ = s.Stop()
			_ = w.Unprotect()
			r.Capture = running
			return fail("not_reachable", "WGC fell back to DD at start", nil)
		}
	}

	half := time.Duration(o.seconds * float64(time.Second))
	readCtx, cancel := context.WithCancel(ctx)
	var (
		mu      sync.Mutex
		samples []protectSample
		endErr  error
		wg      sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			p, err := s.Next(readCtx)
			if err != nil {
				if !errors.Is(err, context.Canceled) && !errors.Is(err, media.ErrStopped) {
					mu.Lock()
					endErr = err
					mu.Unlock()
				}
				return
			}
			switch {
			case p.Kind == media.KindEvent && p.Event != nil && p.Event.Code == media.EventLayer && p.Event.Layer == "f":
				mu.Lock()
				r.Encoder = p.Event.Encoder
				mu.Unlock()
			case p.Kind == media.KindVideo && p.Layer == "f" && !p.Keyframe && !p.Reinit:
				mu.Lock()
				samples = append(samples, protectSample{captureNS: p.CaptureNS, bytes: sliceBytes(p.Data)})
				mu.Unlock()
			}
		}
	}()

	start := s.Now()
	protFrom := start + int64(o.warmup)
	waitCtx(ctx, o.warmup+half)
	protTo := s.Now()
	unErr := w.Unprotect()
	shownFrom := s.Now() + int64(o.settle)
	waitCtx(ctx, o.settle+half)
	shownTo := s.Now()
	cancel()
	_ = s.Stop()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	switch {
	case ctx.Err() != nil:
		return fail("error", "interrupted", ctx.Err())
	case unErr != nil:
		return fail("error", "SetWindowDisplayAffinity(WDA_NONE): "+unErr.Error(), unErr)
	case endErr != nil:
		return fail("error", "the session ended: "+endErr.Error(), endErr)
	}
	r.ProtectedBp, r.Protected = phaseRate(samples, protFrom, protTo)
	r.ShownBps, r.Shown = phaseRate(samples, shownFrom, shownTo)
	if ok, why := protectVerdict(r.ProtectedBp, r.ShownBps); !ok {
		return fail("fail", why, nil)
	}
	r.Outcome = "pass"
	return r
}

// waitCtx sleeps for d or until ctx ends.
func waitCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// phaseRate is the slice bit rate of the samples captured in [from, to) and their count.
func phaseRate(samples []protectSample, from, to int64) (bps float64, frames int) {
	if to <= from {
		return 0, 0
	}
	total := 0
	for _, s := range samples {
		if s.captureNS >= from && s.captureNS < to {
			total += s.bytes
			frames++
		}
	}
	return float64(total) * 8 / (float64(to-from) / float64(time.Second)), frames
}

// protectVerdict: the pattern is present after (shown ≥ protectMinShownBps) and absent while protected (protected ≤
// protectMaxRatio × shown).
func protectVerdict(protectedBps, shownBps float64) (ok bool, why string) {
	switch {
	case shownBps < protectMinShownBps:
		return false, fmt.Sprintf("the pattern is not present after unprotecting: %.0f bit/s, want ≥ %d "+
			"(is the noise window covered, or not on the captured monitor?)", shownBps, protectMinShownBps)
	case protectedBps > protectMaxRatio*shownBps:
		return false, fmt.Sprintf("the pattern is present while protected: %.0f bit/s protected against %.0f "+
			"unprotected, want ≤ %.0f%% (or the desktop behind it moved)", protectedBps, shownBps, protectMaxRatio*100)
	}
	return true, ""
}

// sliceBytes is the size of an access unit's coded slices (NAL types 1–5): SEI, parameter sets, delimiters and
// filler data, which an encoder may add whatever the picture, don't count.
func sliceBytes(au []byte) int {
	n := 0
	for _, u := range nalUnits(au) {
		if t := u[0] & 0x1F; t >= nalSlice && t <= nalIDR {
			n += len(u)
		}
	}
	return n
}

func printProtectPath(w io.Writer, p protectPath) {
	fmt.Fprintf(w, "%s: %s", p.Path, p.Outcome)
	if p.Capture != "" && p.Capture != p.Path {
		fmt.Fprintf(w, " (captured with %s)", p.Capture)
	}
	if p.Outcome == "pass" || p.Outcome == "fail" {
		fmt.Fprintf(w, ", protected %.0f bit/s over %d frames, unprotected %.0f bit/s over %d frames", p.ProtectedBp,
			p.Protected, p.ShownBps, p.Shown)
		if p.Encoder != "" {
			fmt.Fprintf(w, ", encoder %s", p.Encoder)
		}
	}
	if p.Reason != "" {
		fmt.Fprintf(w, ": %s", p.Reason)
	}
	fmt.Fprintln(w)
}
