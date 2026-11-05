package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/native"
)

func init() {
	register(command{name: "avsync", summary: "05 §19.3's real-share A/V offset: a flash window and a click process, captured with the luma tap and the PCM tap (|median| ≤ 20 ms over ≥ 15 pairs)",
		run: cmdAVSync})
}

// 04 §14's avsync, whose steps and pass rule are 05 §19.3's (README WA76):
//
//  1. Two helper processes start as children of explorer.exe (so that neither is in this process's tree, which an
//     exclude-apps share leaves out): the flash window (this program with -role flash: a borderless 1280×720
//     flip-model swap chain, black, white for 100 ms once a second; for each white flash it records the QPC time
//     its first white frame became visible, P_k, from IDXGISwapChain::GetFrameStatistics' SyncQPCTime), and, once
//     the session runs, the click process (-role click: im_test_render_bursts(NULL, 2000, 0.5, 5, 1000, n), 5 ms
//     clicks once a second; it returns each click's predicted device-position time A_k).
//  2. One session captures for -seconds (25) with real capture and real audio: -source pick (pick the flash window;
//     WGC) on Windows 11, -source mon:<id> (DD) on Windows 10; layer f at 1080p60; audio exclude-apps with TapPCM.
//  3. Video: V_k = the capture time of the first frame after each flash whose mean luma in the luma tap exceeds
//     50 % (a rising edge through 50 %). Audio: U_k = each click's PCM-tap onset, the first 1 ms block (48 frames
//     of the chunk) with |x| > 0.05 in either channel, at chunk time + offset / 48 kHz.
//  4. Each flash is paired with the click nearest to it (both run on 1 s grids that need not coincide, because each
//     pair's true offset A_k − P_k is known). Pass: over ≥ 15 pairs, |median((U_k − V_k) − (A_k − P_k))| ≤ 20 ms.
//     avsync.json lists every pair.
//
// The luma tap is an IM_TEST_HOOKS feature: -dll points at the win-x64-ci DLL (06 §5.1's isshoni-native-tools carries
// it in ci\). Capture times are on the session's clock (Packet.CaptureNS = capture_ts_ns − epoch_ns); P_k and A_k are
// QPC nanoseconds (04 §12.3's formula, the engine clock). The tool converts V_k and U_k to the engine clock with the
// session's epoch (engine clock − Session.Now, read back to back) only to associate each event with its partner; the
// offset itself, (U_k − A_k) − (V_k − P_k), doesn't depend on the epoch.
const (
	avsyncMinPairs        = 15
	avsyncMaxOffset       = 20 * time.Millisecond
	avsyncDefaultSeconds  = 25
	avsyncMinSeconds      = 20 // the clicks are -seconds − 5, and ≥ 15 pairs need ≥ 15 clicks
	avsyncClickMargin     = 5  // seconds of the capture without clicks: the click process's start and the tail
	avsyncClickHz         = 2000
	avsyncClickAmp        = 0.5
	avsyncClickLen        = 5 * time.Millisecond
	avsyncPeriod          = time.Second
	avsyncOnsetLevel      = 0.05
	avsyncLumaLevel       = 0.5
	avsyncOnsetRefractory = 500 * time.Millisecond // one onset per click: the clicks are 1 s apart, 5 ms long
	avsyncMatchWindow     = 400 * time.Millisecond // an event's own detection lies within ± this of it
	avsyncPairWindow      = 600 * time.Millisecond // the nearest click on a 1 s grid is within 500 ms of a flash
	avsyncFlashLifetime   = 10 * time.Minute       // the flash window's own limit; the run stops it much earlier
	avsyncClickWait       = 15 * time.Second       // after the capture: the click process's last burst has played by then

	pcmRate         = 48_000
	pcmChunkFrames  = 480
	pcmChunkBytes   = pcmChunkFrames * 2 * 4 // stereo float32
	pcmBlockFrames  = pcmRate / 1000         // 1 ms
	pcmBlockNS      = int64(time.Millisecond)
	avsyncFlashName = "isshoni-enginetest avsync flash"
)

// An open point for whoever wires the tap: with -source mon:<id> the picture is the whole monitor, of which the
// 1280×720 flash window covers 44 % at 1080p, so a whole-picture mean crosses 50 % only over a bright desktop; the
// tap's mean must then be over the flash window's rectangle (centered on the primary monitor), or the window must
// cover the monitor.
//
// errLumaTap is the luma tap's state in this build: the win-x64-ci DLL's luma tap (IM_PACKET_DEBUG, 04 §12.3) has no
// producer in the session yet and internal/media/native drops IM_PACKET_DEBUG packets ("nobody listens yet"), so
// avsync can't read V_k. The tap's Go side is lumaTap below; newLumaTap returns it once the binding passes the
// packets on (a binding change outside README WA76's paths).
var errLumaTap = &media.Error{Code: media.CodeDependency, Op: "avsync",
	Detail: "not implemented: the luma tap (IM_PACKET_DEBUG of an IM_TEST_HOOKS DLL) is not delivered by internal/media/native yet"}

// lumaSample is one encoded picture as the luma tap shows it: its capture time (session clock) and its mean luma,
// 0 (black) to 1 (white).
type lumaSample struct {
	CaptureNS int64
	Mean      float64
}

// lumaTap is the luma tap of one session: the start options that turn it on, and the pictures it saw (in capture
// order) once the session stopped.
type lumaTap interface {
	Options() []native.StartOption
	Samples() []lumaSample
}

// newLumaTap returns a session's luma tap (errLumaTap until the binding delivers it).
var newLumaTap = func() (lumaTap, error) { return nil, errLumaTap }

// avsyncOptions are avsync's flags. role, result, stop, ready and clicks are the helper processes'.
type avsyncOptions struct {
	source  string
	seconds float64
	out     string
	role    string // "" (the run), "flash" or "click"
	result  string // a helper's result file
	stop    string // the flash window stops when this file exists
	ready   string // the flash window creates this file once its first frame is on screen
	clicks  int    // the click process's bursts
	life    time.Duration
}

func cmdAVSync(ctx context.Context, g *globals, args []string) int {
	fs := newFlags(g, "avsync", "[-source pick|mon:<id>] [-seconds 25] [-out DIR]")
	o := avsyncOptions{}
	fs.StringVar(&o.source, "source", "pick", "`pick` (the flash window, WGC; Windows 11) or mon:<id> (DD; Windows 10: the primary monitor, where the flash window opens)")
	fs.Float64Var(&o.seconds, "seconds", avsyncDefaultSeconds, "how long to capture")
	fs.StringVar(&o.out, "out", ".", "write avsync.json into `DIR`")
	fs.StringVar(&o.role, "role", "", "helper processes only: flash or click")
	fs.StringVar(&o.result, "result", "", "helper processes only: the result file")
	fs.StringVar(&o.stop, "stop", "", "the flash window only: stop when this file exists")
	fs.StringVar(&o.ready, "ready", "", "the flash window only: created once the first frame is on screen")
	fs.IntVar(&o.clicks, "clicks", 0, "the click process only: how many clicks")
	fs.DurationVar(&o.life, "life", avsyncFlashLifetime, "the flash window only: the longest it stays")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if code, ok := checkAVSyncOptions(g.stderr, o); !ok {
		return code
	}
	switch o.role {
	case "flash":
		return runFlashRole(ctx, o, g.stderr)
	case "click":
		return runClickRole(ctx, g, o)
	}
	tap, err := newLumaTap()
	if err != nil {
		fmt.Fprintln(g.stderr, "avsync:", err)
		return envCode(err)
	}
	e, code, err := g.openEngine(false)
	if err != nil {
		fmt.Fprintln(g.stderr, "avsync: the engine didn't load:", err)
		return code
	}
	defer func() { _ = e.Close() }()
	helpers, err := newAVSyncHelpers(g)
	if err != nil {
		fmt.Fprintln(g.stderr, "avsync:", err)
		return exitEnv
	}
	defer helpers.Close()
	return runAVSync(ctx, avsyncEnv{engine: e, helpers: helpers, tap: tap}, o, g.json, g.stdout, g.stderr)
}

func checkAVSyncOptions(stderr io.Writer, o avsyncOptions) (int, bool) {
	switch o.role {
	case "":
		switch {
		case o.source != "pick" && !strings.HasPrefix(o.source, "mon:"):
			fmt.Fprintln(stderr, "avsync: -source is pick or mon:<id>")
			return exitUsage, false
		case o.seconds < avsyncMinSeconds || o.seconds > 600:
			fmt.Fprintf(stderr, "avsync: -seconds must be in [%d, 600] (≥ %d pairs need ≥ %d clicks)\n", avsyncMinSeconds,
				avsyncMinPairs, avsyncMinPairs)
			return exitUsage, false
		}
	case "flash":
		if o.result == "" || o.stop == "" || o.ready == "" || o.life <= 0 {
			fmt.Fprintln(stderr, "avsync: -role flash needs -result, -stop, -ready and a positive -life")
			return exitUsage, false
		}
	case "click":
		if o.result == "" || o.clicks < 1 || o.clicks > 600 {
			fmt.Fprintln(stderr, "avsync: -role click needs -result and -clicks 1..600")
			return exitUsage, false
		}
	default:
		fmt.Fprintln(stderr, "avsync: -role is flash or click")
		return exitUsage, false
	}
	return exitPass, true
}

// ---------------------------------------------------------------------------------------------- the helper processes

// flashResult is the flash window's result file.
type flashResult struct {
	FlashesNS  []int64 `json:"flashesNs"` // P_k: when each flash's first white frame became visible (QPC ns)
	Presents   int     `json:"presents"`
	Unresolved int     `json:"unresolved"` // flashes whose display time the frame statistics didn't give
	Estimated  int     `json:"estimated"`  // flashes whose display time was extrapolated over later presents
	RefreshNS  int64   `json:"refreshNs"`  // the display's refresh period as the statistics show it
	Error      string  `json:"error,omitempty"`
}

// clickResult is the click process's result file.
type clickResult struct {
	OnsetsNS []int64 `json:"onsetsNs"` // A_k: each click's predicted device-position time (engine clock, QPC ns)
	Error    string  `json:"error,omitempty"`
}

// flashProc is a running flash window.
type flashProc interface {
	// Stop ends the flash window and returns what it recorded.
	Stop() (flashResult, error)
}

// clickProc is a running click process.
type clickProc interface {
	// Wait waits for the click process (its last click has played) and returns the clicks' times.
	Wait(ctx context.Context) (clickResult, error)
}

// avsyncHelpers starts the helper processes (on Windows as children of explorer.exe).
type avsyncHelpers interface {
	StartFlash(ctx context.Context) (flashProc, error)
	StartClicks(ctx context.Context, n int) (clickProc, error)
	Close()
}

// runClickRole is the click process: im_test_render_bursts on the default render endpoint, then the predicted
// onsets into -result.
func runClickRole(ctx context.Context, g *globals, o avsyncOptions) int {
	res := clickResult{OnsetsNS: []int64{}}
	code := exitPass
	func() {
		e, c, err := g.openEngine(false)
		if err != nil {
			res.Error, code = "the engine didn't load: "+err.Error(), c
			return
		}
		defer func() { _ = e.Close() }()
		hooks, err := native.AudioTestHooks(e)
		if err != nil {
			res.Error, code = err.Error(), envCode(err)
			return
		}
		if ctx.Err() != nil {
			res.Error, code = ctx.Err().Error(), exitFail
			return
		}
		onsets, err := hooks.RenderBursts("", avsyncClickHz, avsyncClickAmp, avsyncClickLen, avsyncPeriod, o.clicks)
		if err != nil {
			res.Error, code = err.Error(), envCode(err)
			return
		}
		res.OnsetsNS = append(res.OnsetsNS, onsets...)
	}()
	if err := writeJSONFile(o.result, res); err != nil {
		fmt.Fprintln(g.stderr, "avsync click:", err)
		return exitEnv
	}
	if res.Error != "" {
		fmt.Fprintln(g.stderr, "avsync click:", res.Error)
	}
	return code
}

func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// ---------------------------------------------------------------------------------------------------------- the run

// avsyncEngine is what the run needs of the engine (*native.Engine; tests fake it).
type avsyncEngine interface {
	Pick(ctx context.Context, req media.PickRequest) (media.Source, error)
	StartWith(ctx context.Context, cfg media.Config, opts ...native.StartOption) (media.Session, error)
	Clock() (native.ClockSample, error)
}

type avsyncEnv struct {
	engine  avsyncEngine
	helpers avsyncHelpers
	tap     lumaTap
}

// avPair is one flash paired with its click. P, V, A and U are on the engine clock (QPC ns); V and U came from
// capture times plus the session's epoch.
type avPair struct {
	Flash    int     `json:"flash"` // the flash's index in the flash window's list
	Click    int     `json:"click"` // the click's index in the click process's list
	PNS      int64   `json:"pNs"`
	VNS      int64   `json:"vNs"`
	ANS      int64   `json:"aNs"`
	UNS      int64   `json:"uNs"`
	VideoMS  float64 `json:"videoMs"`  // V − P: the frame's capture time after the flash became visible
	AudioMS  float64 `json:"audioMs"`  // U − A: the click's chunk time after its device-position time
	OffsetMS float64 `json:"offsetMs"` // (U − V) − (A − P) = (U − A) − (V − P)
}

// avsyncReport is avsync.json and avsync's -json output. It names no machine, user, window or path.
type avsyncReport struct {
	Command      string     `json:"command"`
	Source       string     `json:"source"` // "pick" or "mon"
	Seconds      float64    `json:"seconds"`
	Flashes      int        `json:"flashes"`
	FlashEst     int        `json:"flashesEstimated"`
	FlashMissing int        `json:"flashesUnresolved"`
	RefreshMS    float64    `json:"refreshMs"`
	Clicks       int        `json:"clicks"`
	Frames       int        `json:"frames"`    // pictures the luma tap saw
	Edges        int        `json:"lumaEdges"` // rising edges through 50 %
	Onsets       int        `json:"pcmOnsets"` // click onsets found in the PCM tap
	PCMChunks    int        `json:"pcmChunks"` // PCM-tap chunks read
	Pairs        []avPair   `json:"pairs"`     // every pair
	Unpaired     []string   `json:"unpaired"`  // flashes without a pair, and why
	MedianMS     float64    `json:"medianMs"`  // median of the pairs' offsets
	SpreadMS     [2]float64 `json:"p5p95Ms"`   // the offsets' 5th and 95th percentiles, for the reader
	Failures     []string   `json:"failures"`
	Pass         bool       `json:"pass"`
}

// runAVSync is the run of 05 §19.3 on env: flash window, source, session with the taps, clicks, capture, analysis.
func runAVSync(ctx context.Context, env avsyncEnv, o avsyncOptions, asJSON bool, stdout, stderr io.Writer) int {
	sourceKind := "pick"
	if strings.HasPrefix(o.source, "mon:") {
		sourceKind = "mon"
	}
	rep := avsyncReport{Command: "avsync", Source: sourceKind, Seconds: o.seconds, Pairs: []avPair{},
		Unpaired: []string{}, Failures: []string{}}

	flash, err := env.helpers.StartFlash(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "avsync: the flash window:", err)
		return exitEnv
	}
	flashStopped := false
	stopFlash := func() (flashResult, error) {
		flashStopped = true
		return flash.Stop()
	}
	defer func() {
		if !flashStopped {
			_, _ = flash.Stop()
		}
	}()

	cfg := media.Config{
		Source: o.source, Listed: true, Profile: "6400", Preset: "auto", Layers: runLayers(defaultFBps),
		Borderless: true, Audio: &media.AudioConfig{Mode: media.AudioExcludeApps, TapPCM: true},
	}
	if o.source == "pick" {
		fmt.Fprintf(stderr, "avsync: pick the window %q (the black window that flashes white)\n", avsyncFlashName)
		src, err := env.engine.Pick(ctx, media.PickRequest{})
		if err != nil {
			fmt.Fprintln(stderr, "avsync: pick:", err)
			return envCode(err)
		}
		cfg.Source, cfg.Listed = src.ID, false
	}
	s, err := env.engine.StartWith(ctx, cfg, env.tap.Options()...)
	if err != nil {
		fmt.Fprintln(stderr, "avsync: start:", err)
		return envCode(err)
	}
	epoch, err := sessionEpoch(env.engine, s)
	if err != nil {
		_ = s.Stop()
		fmt.Fprintln(stderr, "avsync: the engine clock:", err)
		return exitFail
	}

	nClicks := max(1, int(o.seconds)-avsyncClickMargin)
	clicks, err := env.helpers.StartClicks(ctx, nClicks)
	if err != nil {
		_ = s.Stop()
		fmt.Fprintln(stderr, "avsync: the click process:", err)
		return exitEnv
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(o.seconds*float64(time.Second)))
	defer cancel()
	det := newOnsetDetector()
	var endErr error
	for {
		p, err := s.Next(runCtx)
		if err != nil {
			if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
				endErr = err
			}
			break
		}
		if p.Kind == media.KindPCM {
			rep.PCMChunks++
			if err := det.chunk(p.CaptureNS, p.Data); err != nil {
				rep.Failures = append(rep.Failures, err.Error())
			}
		}
	}
	cancel()
	_ = s.Stop()
	if endErr != nil {
		rep.Failures = append(rep.Failures, "the session ended: "+endErr.Error())
	}

	waitCtx, cancelWait := context.WithTimeout(ctx, avsyncClickWait)
	cr, err := clicks.Wait(waitCtx)
	cancelWait()
	if err != nil {
		rep.Failures = append(rep.Failures, "the click process: "+err.Error())
	}
	fr, err := stopFlash()
	if err != nil {
		rep.Failures = append(rep.Failures, "the flash window: "+err.Error())
	}
	frames := env.tap.Samples()
	edges := lumaEdges(frames, avsyncLumaLevel)
	rep.Flashes, rep.FlashEst, rep.FlashMissing = len(fr.FlashesNS), fr.Estimated, fr.Unresolved
	rep.RefreshMS = float64(fr.RefreshNS) / 1e6
	rep.Clicks, rep.Frames, rep.Edges, rep.Onsets = len(cr.OnsetsNS), len(frames), len(edges), len(det.onsets)

	v := shift(edges, epoch)
	u := shift(det.onsets, epoch)
	rep.Pairs, rep.Unpaired = pairAVSync(fr.FlashesNS, v, cr.OnsetsNS, u)
	offsets := make([]float64, 0, len(rep.Pairs))
	for _, p := range rep.Pairs {
		offsets = append(offsets, p.OffsetMS)
	}
	rep.MedianMS = quantile(offsets, 0.5)
	rep.SpreadMS = [2]float64{quantile(offsets, 0.05), quantile(offsets, 0.95)}
	rep.Failures = append(rep.Failures, checkAVSync(offsets)...)
	rep.Pass = len(rep.Failures) == 0

	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, "avsync:", err)
		return exitFail
	}
	if err := os.MkdirAll(o.out, 0o750); err != nil {
		fmt.Fprintln(stderr, "avsync:", err)
		return exitEnv
	}
	if err := os.WriteFile(filepath.Join(o.out, "avsync.json"), append(data, '\n'), 0o600); err != nil {
		fmt.Fprintln(stderr, "avsync:", err)
		return exitEnv
	}
	if asJSON {
		_, _ = stdout.Write(append(data, '\n'))
	} else {
		printAVSync(stdout, rep)
	}
	switch {
	case endErr != nil && len(rep.Pairs) == 0:
		return envCode(endErr)
	case !rep.Pass:
		return exitFail
	}
	return exitPass
}

// sessionEpoch is the session's epoch_ns on the engine clock: the engine clock (QPC ns) minus Session.Now, read back
// to back; of five tries the one with the shortest bracket.
func sessionEpoch(e avsyncEngine, s media.Session) (int64, error) {
	best, bestWidth := int64(0), int64(math.MaxInt64)
	for range 5 {
		n0 := s.Now()
		c, err := e.Clock()
		if err != nil {
			return 0, err
		}
		n1 := s.Now()
		if w := n1 - n0; w < bestWidth {
			best, bestWidth = c.MonoNS-(n0+w/2), w
		}
	}
	return best, nil
}

func shift(ts []int64, by int64) []int64 {
	out := make([]int64, len(ts))
	for i, t := range ts {
		out[i] = t + by
	}
	return out
}

// checkAVSync is the pass rule: at least avsyncMinPairs pairs and |median offset| ≤ avsyncMaxOffset.
func checkAVSync(offsetsMS []float64) []string {
	var fails []string
	if len(offsetsMS) < avsyncMinPairs {
		fails = append(fails, fmt.Sprintf("%d pairs, at least %d needed (05 §19.3)", len(offsetsMS), avsyncMinPairs))
	}
	if len(offsetsMS) > 0 {
		if m := quantile(offsetsMS, 0.5); math.Abs(m) > float64(avsyncMaxOffset)/1e6 {
			fails = append(fails, fmt.Sprintf("|median A/V offset| %.1f ms > %v (05 §19.3)", math.Abs(m), avsyncMaxOffset))
		}
	}
	return fails
}

// quantile is the q-quantile of v with linear interpolation between order statistics (q 0.5: the median, the mean
// of the two middle values for an even count); 0 for no values.
func quantile(v []float64, q float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	pos := q * float64(len(s)-1)
	i := int(pos)
	if i >= len(s)-1 {
		return s[len(s)-1]
	}
	return s[i] + (pos-float64(i))*(s[i+1]-s[i])
}

// ----------------------------------------------------------------------------------------- detection and pairing

// onsetDetector finds the clicks in the PCM tap: an onset is the first 1 ms block of a chunk (48 frames) with a
// sample of either channel above avsyncOnsetLevel in magnitude, at least avsyncOnsetRefractory after the previous
// loud block. Its time is the chunk's capture time plus the block's offset (offset / 48 kHz).
type onsetDetector struct {
	onsets   []int64
	lastLoud int64
	haveLoud bool
}

func newOnsetDetector() *onsetDetector { return &onsetDetector{} }

// chunk takes one PCM-tap chunk: 480 stereo float32 frames, little endian, interleaved.
func (d *onsetDetector) chunk(captureNS int64, data []byte) error {
	if len(data) != pcmChunkBytes {
		return fmt.Errorf("a PCM-tap chunk of %d bytes, not %d", len(data), pcmChunkBytes)
	}
	for b := range pcmChunkFrames / pcmBlockFrames {
		loud := false
		for f := b * pcmBlockFrames; f < (b+1)*pcmBlockFrames && !loud; f++ {
			for c := range 2 {
				x := math.Float32frombits(binary.LittleEndian.Uint32(data[(f*2+c)*4:]))
				if math.Abs(float64(x)) > avsyncOnsetLevel {
					loud = true
					break
				}
			}
		}
		if !loud {
			continue
		}
		t := captureNS + int64(b)*pcmBlockNS
		if !d.haveLoud || t-d.lastLoud >= int64(avsyncOnsetRefractory) {
			d.onsets = append(d.onsets, t)
		}
		d.lastLoud, d.haveLoud = t, true
	}
	return nil
}

// lumaEdges returns the capture times of the pictures whose mean luma exceeds level while the picture before it
// didn't (the first frame after each flash). The first picture is never an edge: a capture that starts inside a
// flash doesn't see its start. The samples are in capture order.
func lumaEdges(frames []lumaSample, level float64) []int64 {
	var out []int64
	for i := 1; i < len(frames); i++ {
		if frames[i].Mean > level && frames[i-1].Mean <= level {
			out = append(out, frames[i].CaptureNS)
		}
	}
	return out
}

// nearest is the index of the value of ts nearest to t within ±window that used doesn't mark, or -1.
func nearest(ts []int64, t, window int64, used map[int]bool) int {
	best, bestD := -1, window+1
	for i, x := range ts {
		if used[i] {
			continue
		}
		d := x - t
		if d < 0 {
			d = -d
		}
		if d < bestD {
			best, bestD = i, d
		}
	}
	return best
}

// pairAVSync pairs each flash P_k with its frame V (the rising edge nearest to it within avsyncMatchWindow), the
// click A nearest to it (within avsyncPairWindow, each click at most once) and that click's onset U (nearest to A
// within avsyncMatchWindow). All times are on one clock. A flash that misses any part is listed in unpaired.
func pairAVSync(p, v, a, u []int64) ([]avPair, []string) {
	pairs := []avPair{}
	unpaired := []string{}
	usedV, usedA, usedU := map[int]bool{}, map[int]bool{}, map[int]bool{}
	for k, pk := range p {
		vi := nearest(v, pk, int64(avsyncMatchWindow), usedV)
		if vi < 0 {
			unpaired = append(unpaired, fmt.Sprintf("flash %d: no frame above 50 %% luma within %v", k, avsyncMatchWindow))
			continue
		}
		ai := nearest(a, pk, int64(avsyncPairWindow), usedA)
		if ai < 0 {
			unpaired = append(unpaired, fmt.Sprintf("flash %d: no click within %v", k, avsyncPairWindow))
			continue
		}
		ui := nearest(u, a[ai], int64(avsyncMatchWindow), usedU)
		if ui < 0 {
			unpaired = append(unpaired, fmt.Sprintf("flash %d: click %d not found in the PCM tap within %v", k, ai,
				avsyncMatchWindow))
			continue
		}
		usedV[vi], usedA[ai], usedU[ui] = true, true, true
		video, audio := v[vi]-pk, u[ui]-a[ai]
		pairs = append(pairs, avPair{
			Flash: k, Click: ai, PNS: pk, VNS: v[vi], ANS: a[ai], UNS: u[ui],
			VideoMS: float64(video) / 1e6, AudioMS: float64(audio) / 1e6, OffsetMS: float64(audio-video) / 1e6,
		})
	}
	return pairs, unpaired
}

// ------------------------------------------------------------------------------------ the flash window's statistics

// frameStat is one DXGI_FRAME_STATISTICS reading.
type frameStat struct {
	PresentCount        uint32
	PresentRefreshCount uint32
	SyncRefreshCount    uint32
	SyncQPC             int64
}

// refreshPeriodQPC is the display's refresh period in QPC ticks: the median, over consecutive readings whose
// SyncRefreshCount advanced, of ΔSyncQPC / ΔSyncRefreshCount; 0 without two such readings.
func refreshPeriodQPC(stats []frameStat) float64 {
	var per []float64
	for i := 1; i < len(stats); i++ {
		dr := int64(stats[i].SyncRefreshCount) - int64(stats[i-1].SyncRefreshCount)
		dq := stats[i].SyncQPC - stats[i-1].SyncQPC
		if dr > 0 && dq > 0 {
			per = append(per, float64(dq)/float64(dr))
		}
	}
	return quantile(per, 0.5)
}

// presentQPC is when present n became visible, in QPC ticks. A reading whose PresentCount is n gives it directly:
// SyncQPC + (PresentRefreshCount − SyncRefreshCount) × period. Without one, the first reading of a later present
// (at most maxAhead later) extrapolates back, one refresh per present (the window presents with sync interval 1
// and keeps up): estimated is then true. ok is false when neither exists or the period is unknown.
func presentQPC(n uint32, stats []frameStat, period float64) (qpc int64, estimated, ok bool) {
	const maxAhead = 8
	if period <= 0 {
		return 0, false, false
	}
	at := func(s frameStat, refresh int64) int64 {
		return s.SyncQPC + int64(math.Round(float64(refresh-int64(s.SyncRefreshCount))*period))
	}
	for _, s := range stats {
		if s.PresentCount == n {
			return at(s, int64(s.PresentRefreshCount)), false, true
		}
	}
	for _, s := range stats {
		if ahead := int64(s.PresentCount) - int64(n); ahead > 0 && ahead <= maxAhead {
			return at(s, int64(s.PresentRefreshCount)-ahead), true, true
		}
	}
	return 0, false, false
}

// qpcTicksToNS is 04 §12.3's formula (internal/media/native's qpcToNS), so P_k is on the engine clock.
func qpcTicksToNS(q, f int64) int64 { return q/f*1e9 + q%f*1e9/f }

// flashTimes resolves the flash window's white flashes: firstWhite lists each flash's first white present (its
// PresentCount); the result has each flash's display time in QPC ns, in order, and the counts of extrapolated and
// unresolved flashes.
func flashTimes(firstWhite []uint32, stats []frameStat, freq int64) flashResult {
	r := flashResult{FlashesNS: []int64{}}
	period := refreshPeriodQPC(stats)
	if freq > 0 {
		r.RefreshNS = int64(period * 1e9 / float64(freq))
	}
	for _, n := range firstWhite {
		q, est, ok := presentQPC(n, stats, period)
		switch {
		case !ok || freq <= 0:
			r.Unresolved++
		default:
			if est {
				r.Estimated++
			}
			r.FlashesNS = append(r.FlashesNS, qpcTicksToNS(q, freq))
		}
	}
	return r
}

func printAVSync(w io.Writer, r avsyncReport) {
	fmt.Fprintf(w, "avsync: %s, %.0f s; %d flashes (%d extrapolated, %d unresolved, refresh %.2f ms), %d clicks\n",
		r.Source, r.Seconds, r.Flashes, r.FlashEst, r.FlashMissing, r.RefreshMS, r.Clicks)
	fmt.Fprintf(w, "luma tap: %d pictures, %d flashes seen; PCM tap: %d chunks, %d clicks heard\n", r.Frames, r.Edges,
		r.PCMChunks, r.Onsets)
	for _, p := range r.Pairs {
		fmt.Fprintf(w, "flash %2d / click %2d: video %+7.1f ms, audio %+7.1f ms, offset %+7.1f ms\n", p.Flash, p.Click,
			p.VideoMS, p.AudioMS, p.OffsetMS)
	}
	for _, u := range r.Unpaired {
		fmt.Fprintln(w, "unpaired:", u)
	}
	fmt.Fprintf(w, "%d pairs: median offset %+.1f ms (p5 %+.1f, p95 %+.1f)\n", len(r.Pairs), r.MedianMS, r.SpreadMS[0],
		r.SpreadMS[1])
	for _, f := range r.Failures {
		fmt.Fprintln(w, "FAIL:", f)
	}
	if r.Pass {
		fmt.Fprintln(w, "PASS")
	}
}
