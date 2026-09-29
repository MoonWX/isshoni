//go:build darwin

// Command vtbench measures VideoToolbox H.264 hardware encoding for a screen-sharing
// sender (isshoni spike S2, macOS). See README.md.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"time"
)

type sizeSpec struct {
	Name    string
	W, H    int
	Bitrate int64
}

var sizeTable = map[string]sizeSpec{
	"1080p": {"1080p", 1920, 1080, 8_000_000},
	"1440p": {"1440p", 2560, 1440, 16_000_000},
	"4k":    {"4k", 3840, 2160, 30_000_000},
}

var formatTable = map[string]int{"bgra": fmtBGRA, "nv12": fmtNV12}

// The S2 pass criterion from docs/PLAN.md.
const (
	criterionSize = "1440p"
	criterionMs   = 16.0
)

type options struct {
	sizes, formats, llrc, suites, twoLayerSizes []string
	fps                                         int
	duration, warmup, tputDuration, gop         time.Duration
	tputInflight                                int
	dataRate                                    float64
	speed, realTime, forceKF, decode, psnr      bool
	ringMax, ringMB                             int
	previewW, previewH, previewFPS              int
	previewBps                                  int64
	bitrates                                    map[string]int64
	gapMs                                       int
	repeat                                      int
	csvDir                                      string
}

type Results struct {
	Spike               string              `json:"spike"`
	Date                string              `json:"date"`
	System              json.RawMessage     `json:"system"`
	SystemEnd           json.RawMessage     `json:"system_end"`
	Tool                map[string]any      `json:"tool"`
	Settings            map[string]any      `json:"settings"`
	Criterion           string              `json:"criterion"`
	Encoders            json.RawMessage     `json:"encoders"`
	SupportedProperties map[string][]string `json:"supported_properties"`
	SessionExamples     map[string]any      `json:"session_examples"` // every property set + readback errors, first run per encoder
	Summary             []SummaryRow        `json:"summary"`
	Runs                []*Run              `json:"runs"`
}

type Run struct {
	Name        string         `json:"name"`
	Kind        string         `json:"kind"` // paced | throughput | two-layer | preview-only | variant
	Variant     string         `json:"variant,omitempty"`
	Size        string         `json:"size"`
	Width       int            `json:"width"`
	Height      int            `json:"height"`
	Format      string         `json:"format"`
	LLRC        bool           `json:"low_latency_rc"`
	FPS         int            `json:"fps"`
	RingFrames  int            `json:"ring_frames"`
	Status      int32          `json:"status"`
	StatusName  string         `json:"status_name,omitempty"`
	Verdict     string         `json:"verdict"`
	Layers      []*LayerResult `json:"layers"`
	ComparedTo  string         `json:"compared_to,omitempty"`
	Interaction map[string]any `json:"interaction,omitempty"`
}

type FrameCounts struct {
	Submitted int `json:"submitted"`
	Encoded   int `json:"encoded"`
	Dropped   int `json:"dropped"`
	Failed    int `json:"failed"`
	Missing   int `json:"missing"`
	Truncated int `json:"truncated,omitempty"`
}

type Warmup struct {
	Frames       int     `json:"frames"`
	FirstFrameMs float64 `json:"first_frame_ms"`
	First10MaxMs float64 `json:"first_10_max_ms"`
	Latency      Dist    `json:"latency_ms"`
	FirstCycle   *Dist   `json:"first_ring_cycle_ms,omitempty"`  // first use of every IOSurface
	SecondCycle  *Dist   `json:"second_ring_cycle_ms,omitempty"` // same surfaces again
	Dropped      int     `json:"dropped"`
}

type ForcedKF struct {
	Frame       int     `json:"frame"`
	Keyframe    bool    `json:"came_out_as_keyframe"`
	LatencyMs   float64 `json:"latency_ms"`
	Bytes       int     `json:"bytes"`
	MedianBytes int     `json:"median_frame_bytes"`
}

type LayerResult struct {
	Role            string         `json:"role"`
	Width           int            `json:"width"`
	Height          int            `json:"height"`
	FPS             int            `json:"fps"`
	TargetBps       int64          `json:"target_bps"`
	EncoderID       string         `json:"encoder_id"`
	Hardware        any            `json:"hardware"`
	Rejected        []string       `json:"rejected_properties"`
	Session         map[string]any `json:"session"`
	Frames          FrameCounts    `json:"frames"`
	Warmup          *Warmup        `json:"warmup,omitempty"`
	LatencyMs       Dist           `json:"latency_ms"`         // capture tick -> output callback (measured phase)
	EncoderMs       Dist           `json:"encoder_latency_ms"` // EncodeFrame call -> output callback
	OverCriterion   int            `json:"frames_over_16ms"`
	OverInterval    int            `json:"frames_over_frame_interval"`
	PFrameLatencyMs Dist           `json:"p_frame_latency_ms"`
	KeyLatencyMs    Dist           `json:"keyframe_latency_ms"`
	SubmitCallMs    Dist           `json:"submit_call_ms"`
	PacingLateMs    *Dist          `json:"submit_late_ms,omitempty"` // EncodeFrame call - capture tick (grows when the previous call blocked)
	DownscaleMs     *Dist          `json:"downscale_ms,omitempty"`
	OutputFPS       float64        `json:"output_fps"`
	ProcessCPUPct   float64        `json:"process_cpu_pct,omitempty"` // whole process, % of one core (full layer)
	ThroughputFPS   float64        `json:"throughput_fps,omitempty"`
	BitrateBps      float64        `json:"bitrate_bps"`
	Max1sBitrateBps float64        `json:"max_1s_bitrate_bps"`
	Keyframes       int            `json:"keyframes"`
	ForcedKeyframe  *ForcedKF      `json:"forced_keyframe,omitempty"`
	Stream          *StreamReport  `json:"stream,omitempty"`
	Decode          *DecodeResult  `json:"decode,omitempty"`
	Sustains        bool           `json:"sustains_rate"`       // no drops/failures, >= 98% of the frame rate, p99 < 2 frame intervals
	WithinInterval  bool           `json:"p99_within_interval"` // p99 latency <= one frame interval
}

type SummaryRow struct {
	Run       string  `json:"run"`
	Layer     string  `json:"layer"`
	EncoderID string  `json:"encoder_id"`
	P50       float64 `json:"p50_ms"`
	P95       float64 `json:"p95_ms"`
	P99       float64 `json:"p99_ms"`
	Max       float64 `json:"max_ms"`
	FPS       float64 `json:"fps"`
	Dropped   int     `json:"dropped"`
	Mbps      float64 `json:"mbps"`
	Keyframes int     `json:"keyframes"`
	PSNR      float64 `json:"psnr_y_db,omitempty"`
	Verdict   string  `json:"verdict"`
}

var vtStatusNames = map[int32]string{
	-12900: "kVTPropertyNotSupportedErr", -12901: "kVTPropertyReadOnlyErr", -12902: "kVTParameterErr",
	-12903: "kVTInvalidSessionErr", -12904: "kVTAllocationFailedErr", -12905: "kVTPixelTransferNotSupportedErr",
	-12906: "kVTCouldNotFindVideoDecoderErr", -12907: "kVTCouldNotCreateInstanceErr",
	-12908: "kVTCouldNotFindVideoEncoderErr", -12909: "kVTVideoDecoderBadDataErr",
	-12910: "kVTVideoDecoderUnsupportedDataFormatErr", -12911: "kVTVideoDecoderMalfunctionErr",
	-12912: "kVTVideoEncoderMalfunctionErr", -12913: "kVTVideoDecoderNotAvailableNowErr",
	-12915: "kVTVideoEncoderNotAvailableNowErr", -12916: "kVTFormatDescriptionChangeNotSupportedErr",
	-12917: "kVTInsufficientSourceColorDataErr", -12918: "kVTCouldNotCreateColorCorrectionDataErr",
	-12919: "kVTColorSyncTransformConvertFailedErr", -17691: "kVTSessionMalfunctionErr",
	-17694: "kVTVideoDecoderReferenceMissingErr",
}

func statusName(st int32) string {
	if st == 0 {
		return "noErr"
	}
	if n, ok := vtStatusNames[st]; ok {
		return n
	}
	return fmt.Sprintf("OSStatus %d", st)
}

func main() {
	var (
		sizes         = flag.String("sizes", "1080p,1440p,4k", "sizes to run: 1080p, 1440p, 4k")
		formats       = flag.String("formats", "nv12,bgra", "input pixel formats: nv12, bgra")
		llrc          = flag.String("llrc", "on,off", "EnableLowLatencyRateControl variants: on, off")
		suites        = flag.String("suites", "paced,tput,twolayer,variants", "suites: paced, tput, twolayer (two layers + preview-only baseline), variants (one-property changes)")
		variantSize   = flag.String("variant-sizes", "1440p,4k", "sizes for the variants suite")
		variantFormat = flag.String("variant-format", "nv12", "input format for the variants suite")
		twoSizes      = flag.String("twolayer-sizes", "1080p,1440p,4k", "sizes for the two-layer suite")
		fps           = flag.Int("fps", 60, "full-layer frame rate")
		duration      = flag.Duration("duration", 10*time.Second, "measured duration per paced run")
		warmup        = flag.Duration("warmup", 3*time.Second, "paced warm-up before measuring (same session)")
		tputDur       = flag.Duration("tput-duration", 4*time.Second, "throughput run length")
		inflight      = flag.Int("tput-inflight", 8, "throughput mode: max frames in flight")
		gop           = flag.Duration("gop", 2*time.Second, "MaxKeyFrameIntervalDuration (0 = don't set)")
		dataRate      = flag.Float64("datarate", 1.5, "DataRateLimits = bitrate*X per 1 s (0 = don't set)")
		speed         = flag.Bool("speed", true, "set PrioritizeEncodingSpeedOverQuality")
		realTime      = flag.Bool("realtime", true, "set RealTime")
		forceKF       = flag.Bool("force-kf", true, "force one keyframe mid-run (PLI response)")
		decode        = flag.Bool("decode", true, "decode every stream with VTDecompressionSession")
		psnr          = flag.Bool("psnr", true, "luma PSNR against the source (NV12 inputs only)")
		ringMax       = flag.Int("ring", 90, "max frames in the pre-rendered ring")
		ringMB        = flag.Int("ring-mb", 2048, "max ring memory (MiB); caps the ring at 4K BGRA")
		prevSize      = flag.String("preview", "640x360@15", "preview layer WxH@fps")
		prevBps       = flag.Int64("preview-bps", 300_000, "preview layer bitrate")
		bitrates      = flag.String("bitrates", "1080p=8000000,1440p=16000000,4k=30000000", "full-layer bitrate per size")
		gapMs         = flag.Int("gap-ms", 500, "idle gap between runs")
		jsonOut       = flag.String("json", "", "write machine-readable results to this file")
		dumpDir       = flag.String("dump", "", "write each layer's Annex B stream (.h264) into this directory")
		listEncs      = flag.Bool("list-encoders", false, "print the H.264 encoder list and exit")
		repeat        = flag.Int("repeat", 1, "repeat the per-size suites (paced, tput, twolayer) N times")
		activity      = flag.String("activity", "latency-critical", "NSProcessInfo activity held during the runs: latency-critical, user-initiated, none")
		csvDir        = flag.String("csv", "", "write per-frame records (.csv) into this directory")
	)
	flag.Parse()

	if *listEncs {
		var v any
		_ = json.Unmarshal(EncodersJSON(), &v)
		b, _ := json.MarshalIndent(v, "", "  ")
		fmt.Println(string(b))
		return
	}

	o := options{
		sizes: splitList(*sizes), formats: splitList(*formats), llrc: splitList(*llrc),
		suites: splitList(*suites), twoLayerSizes: splitList(*twoSizes),
		fps: *fps, duration: *duration, warmup: *warmup, tputDuration: *tputDur, gop: *gop,
		tputInflight: *inflight, dataRate: *dataRate, speed: *speed, realTime: *realTime, forceKF: *forceKF,
		decode: *decode, psnr: *psnr, ringMax: *ringMax, ringMB: *ringMB, previewBps: *prevBps,
		bitrates: map[string]int64{}, gapMs: *gapMs, repeat: max(1, *repeat),
	}
	if _, err := fmt.Sscanf(*prevSize, "%dx%d@%d", &o.previewW, &o.previewH, &o.previewFPS); err != nil {
		fatalf("bad -preview %q: %v", *prevSize, err)
	}
	for _, kv := range splitList(*bitrates) {
		var name string
		var bps int64
		k, v, ok := strings.Cut(kv, "=")
		if _, err := fmt.Sscan(v, &bps); !ok || err != nil {
			fatalf("bad -bitrates entry %q", kv)
		}
		name = k
		o.bitrates[name] = bps
	}
	for _, s := range o.sizes {
		if _, ok := sizeTable[s]; !ok {
			fatalf("unknown size %q", s)
		}
	}
	for _, f := range o.formats {
		if _, ok := formatTable[f]; !ok {
			fatalf("unknown format %q", f)
		}
	}
	if *dumpDir != "" {
		if err := os.MkdirAll(*dumpDir, 0o755); err != nil {
			fatalf("%v", err)
		}
	}

	switch *activity {
	case "latency-critical":
		defer BeginActivity(true)()
	case "user-initiated":
		defer BeginActivity(false)()
	case "none":
	default:
		fatalf("bad -activity %q", *activity)
	}
	if *csvDir != "" {
		if err := os.MkdirAll(*csvDir, 0o755); err != nil {
			fatalf("%v", err)
		}
	}
	o.csvDir = *csvDir

	res := &Results{
		Spike:  "S2 macOS: VideoToolbox H.264 encode",
		Date:   time.Now().Format("2006-01-02"),
		System: SystemJSON(),
		Tool: map[string]any{
			"go": runtime.Version(), "arch": runtime.GOARCH, "sdk": "MacOSX 26.2 (Xcode)", "min_macos": "14.4",
		},
		Settings: map[string]any{
			"sizes": o.sizes, "formats": o.formats, "llrc": o.llrc, "suites": o.suites,
			"twolayer_sizes": o.twoLayerSizes, "fps": o.fps, "duration_s": o.duration.Seconds(),
			"warmup_s": o.warmup.Seconds(), "tput_duration_s": o.tputDuration.Seconds(),
			"tput_inflight": o.tputInflight, "gop_s": o.gop.Seconds(), "data_rate_factor": o.dataRate,
			"prioritize_speed": o.speed, "real_time": o.realTime, "force_keyframe": o.forceKF,
			"ring_max": o.ringMax, "ring_mb": o.ringMB, "preview": *prevSize, "preview_bps": o.previewBps,
			"bitrates": o.bitrates, "require_hardware": true, "profile": "High (AutoLevel)",
			"allow_frame_reordering": false, "activity": *activity, "repeat": max(1, *repeat),
		},
		Criterion: fmt.Sprintf("%s%d paced: every frame (max) capture tick -> output callback <= %.0f ms, no dropped/failed frames, output fps >= 98%% of target; 'PASS at p99' if only p99 meets it",
			criterionSize, o.fps, criterionMs),
		Encoders:            EncodersJSON(),
		SupportedProperties: map[string][]string{},
		SessionExamples:     map[string]any{},
	}
	fmt.Printf("vtbench: %s\n", string(res.System))

	for _, sz := range o.sizes {
		spec := sizeTable[sz]
		if b, ok := o.bitrates[sz]; ok {
			spec.Bitrate = b
		}
		if !slices.Contains(o.suites, "paced") && !slices.Contains(o.suites, "tput") &&
			!(slices.Contains(o.suites, "twolayer") && slices.Contains(o.twoLayerSizes, sz)) {
			continue
		}
		for _, fname := range o.formats {
			bench, err := OpenBench(spec.W, spec.H, formatTable[fname], o.ringMax, int64(o.ringMB)<<20)
			if err != nil {
				fatalf("prerender %s %s: %v", sz, fname, err)
			}
			fmt.Printf("\n== %s %s: ring of %d frames pre-rendered in %.0f ms\n", sz, fname, bench.RingLen, bench.PrerenderMs)
			for rep := 1; rep <= o.repeat; rep++ {
				for _, ll := range o.llrc {
					on := ll == "on"
					enc := EncConfig{Width: spec.W, Height: spec.H, FPS: o.fps, ExpectedFPS: o.fps, Bitrate: spec.Bitrate,
						DataRateFactor: o.dataRate, KeyintSec: o.gop.Seconds(), LLRC: on, Speed: o.speed,
						RealTime: o.realTime, RequireHW: true}
					base := fmt.Sprintf("%s%d %s llrc=%s", sz, o.fps, fname, ll)
					if o.repeat > 1 {
						base = fmt.Sprintf("#%d %s", rep, base)
					}
					var paced *Run
					if slices.Contains(o.suites, "paced") {
						paced = runPaced(res, bench, spec, fname, enc, o, base+" paced", nil, true, *dumpDir)
					}
					if slices.Contains(o.suites, "tput") {
						runThroughput(res, bench, spec, fname, enc, o, base+" throughput")
					}
					if slices.Contains(o.suites, "twolayer") && slices.Contains(o.twoLayerSizes, sz) {
						prev := EncConfig{Width: o.previewW, Height: o.previewH, FPS: o.previewFPS,
							ExpectedFPS: o.previewFPS, Bitrate: o.previewBps,
							DataRateFactor: o.dataRate, KeyintSec: o.gop.Seconds(), LLRC: on, Speed: o.speed,
							RealTime: o.realTime, RequireHW: true}
						two := runPaced(res, bench, spec, fname, enc, o, base+" two-layer", &prev, true, *dumpDir)
						po := runPaced(res, bench, spec, fname, enc, o, base+" preview-only", &prev, false, *dumpDir)
						compareLayers(two, paced, po)
					}
				}
			}
			bench.Close()
		}
	}
	if slices.Contains(o.suites, "variants") {
		for _, sz := range splitList(*variantSize) {
			runVariants(res, o, sz, *variantFormat, *dumpDir)
		}
	}
	res.SystemEnd = SystemJSON()
	res.Summary = summarize(res.Runs)
	printSummary(res.Summary)

	if *jsonOut != "" {
		b, err := marshalResults(res)
		if err != nil {
			fatalf("json: %v", err)
		}
		if err := os.WriteFile(*jsonOut, b, 0o644); err != nil {
			fatalf("%v", err)
		}
		fmt.Printf("\nwrote %s\n", *jsonOut)
	}
}

func splitList(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(strings.ToLower(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "vtbench: "+format+"\n", a...)
	os.Exit(1)
}

func gap(o options) { time.Sleep(time.Duration(o.gapMs) * time.Millisecond) }

func newRun(name, kind string, spec sizeSpec, fname string, enc EncConfig, ring int) *Run {
	return &Run{Name: name, Kind: kind, Size: spec.Name, Width: spec.W, Height: spec.H, Format: fname,
		LLRC: enc.LLRC, FPS: enc.FPS, RingFrames: ring}
}

// runPaced runs a real-time 60 fps run: single layer (preview == nil), two layers, or
// the preview layer alone (encodeFull == false).
func runPaced(res *Results, b *Bench, spec sizeSpec, fname string, enc EncConfig, o options, name string,
	preview *EncConfig, encodeFull bool, dumpDir string) *Run {
	gap(o)
	kind := "paced"
	if preview != nil {
		kind = "two-layer"
		if !encodeFull {
			kind = "preview-only"
		}
	}
	warm := int(o.warmup.Seconds() * float64(enc.FPS))
	meas := int(o.duration.Seconds() * float64(enc.FPS))
	rc := RunConfig{Mode: modePaced, Frames: warm + meas, ForceKF: -1, EncodeFull: encodeFull, Preview: preview,
		StreamCap: 512 << 20}
	if o.forceKF && encodeFull {
		rc.ForceKF = warm + meas/2 + 37 // away from the periodic keyframes
	}
	run := newRun(name, kind, spec, fname, enc, b.RingLen)
	st, layers := b.Run(enc, rc)
	run.Status, run.StatusName = st, statusName(st)
	for i, lr := range layers {
		cfg, w := enc, warm
		if i == 1 {
			cfg = *preview
			w = warm / max(1, enc.FPS/preview.FPS)
		}
		if lr.Info["skipped"] == true {
			continue
		}
		L := analyzeLayer(res, lr, cfg, w, b.RingLen, modePaced)
		if st == 0 && o.decode && L.Stream != nil {
			aus := auRefs(lr.Frames)
			if len(aus) > 0 {
				d, err := b.Decode(lr.Stream, aus, o.psnr && i == 0)
				if err == nil {
					L.Decode = &d
				}
			}
		}
		writeCSV(o.csvDir, name, L.Role, lr.Frames)
		if dumpDir != "" && len(lr.Stream) > 0 {
			fn := filepath.Join(dumpDir, strings.NewReplacer(" ", "_", "=", "-").Replace(name)+"-"+L.Role+".h264")
			_ = os.WriteFile(fn, lr.Stream, 0o644)
		}
		run.Layers = append(run.Layers, L)
	}
	run.Verdict = verdict(run, o)
	res.Runs = append(res.Runs, run)
	printRun(run)
	return run
}

func runThroughput(res *Results, b *Bench, spec sizeSpec, fname string, enc EncConfig, o options, name string) *Run {
	gap(o)
	rc := RunConfig{Mode: modeThroughput, Frames: 20000, MaxSeconds: o.tputDuration.Seconds(), Inflight: o.tputInflight,
		ForceKF: -1, EncodeFull: true, StreamCap: 512 << 20}
	run := newRun(name, "throughput", spec, fname, enc, b.RingLen)
	st, layers := b.Run(enc, rc)
	run.Status, run.StatusName = st, statusName(st)
	for _, lr := range layers {
		L := analyzeLayer(res, lr, enc, 0, b.RingLen, modeThroughput)
		writeCSV(o.csvDir, name, L.Role, lr.Frames)
		run.Layers = append(run.Layers, L)
	}
	run.Verdict = verdict(run, o)
	res.Runs = append(res.Runs, run)
	printRun(run)
	return run
}

func auRefs(frames []Frame) []AURef {
	var aus []AURef
	for _, f := range frames {
		if f.StreamOff >= 0 && f.StreamLen > 0 {
			aus = append(aus, AURef{Off: f.StreamOff, Len: f.StreamLen, Frame: f.FrameIndex, Key: f.has(flagKeyframe)})
		}
	}
	return aus
}

func ms(ns int64) float64 { return float64(ns) / 1e6 }

func analyzeLayer(res *Results, lr LayerRaw, cfg EncConfig, warm, ring, mode int) *LayerResult {
	L := &LayerResult{Role: fmt.Sprint(lr.Info["role"]), Width: cfg.Width, Height: cfg.Height, FPS: cfg.FPS,
		TargetBps: cfg.Bitrate, Session: lr.Info, Rejected: []string{}}
	if rb, ok := lr.Info["readback"].(map[string]any); ok {
		L.EncoderID = fmt.Sprint(rb["EncoderID"])
		L.Hardware = rb["UsingHardwareAcceleratedVideoEncoder"]
	}
	if set, ok := lr.Info["set"].([]any); ok {
		for _, e := range set {
			m, _ := e.(map[string]any)
			if s, _ := m["status"].(float64); s != 0 {
				L.Rejected = append(L.Rejected, fmt.Sprintf("%v: %s", m["name"], statusName(int32(s))))
			}
		}
	}
	if sup, ok := lr.Info["supported_properties"].([]any); ok {
		key := fmt.Sprintf("%s llrc=%v", L.EncoderID, cfg.LLRC)
		if _, seen := res.SupportedProperties[key]; !seen {
			names := make([]string, 0, len(sup))
			for _, s := range sup {
				names = append(names, fmt.Sprint(s))
			}
			res.SupportedProperties[key] = names
		}
		delete(lr.Info, "supported_properties")
	}
	if set, ok := lr.Info["set"]; ok {
		key := fmt.Sprintf("%s llrc=%v role=%s", L.EncoderID, cfg.LLRC, L.Role)
		if _, seen := res.SessionExamples[key]; !seen {
			res.SessionExamples[key] = map[string]any{"set": set, "readback_status": lr.Info["readback_status"]}
		}
		delete(lr.Info, "set")
		delete(lr.Info, "readback_status")
	}

	frames := lr.Frames
	if warm > len(frames) {
		warm = len(frames)
	}
	// Paced runs measure from the frame's scheduled capture tick (what an SCK callback
	// would see), so time spent waiting behind a blocking EncodeFrame counts. For an
	// on-time submit this equals submit -> callback. Throughput runs use submit -> callback.
	lat := func(f Frame) (float64, bool) {
		if !f.has(flagCallback) || f.has(flagNoOutput) || f.DoneNs == 0 {
			return 0, false
		}
		if mode == modePaced {
			return ms(f.DoneNs - f.TargetNs), true
		}
		return ms(f.DoneNs - f.SubmitNs), true
	}

	if warm > 0 && mode == modePaced {
		wf := frames[:warm]
		W := &Warmup{Frames: warm}
		var all []float64
		for i, f := range wf {
			if f.has(flagDropped) {
				W.Dropped++
			}
			v, ok := lat(f)
			if !ok {
				continue
			}
			all = append(all, v)
			if i == 0 {
				W.FirstFrameMs = round3(v)
			}
			if i < 10 {
				W.First10MaxMs = max(W.First10MaxMs, round3(v))
			}
		}
		W.Latency = NewDist(all)
		if ring > 0 && L.Role == "full" && warm >= 2*ring {
			var c1, c2 []float64
			for i, f := range wf[:2*ring] {
				if v, ok := lat(f); ok {
					if i < ring {
						c1 = append(c1, v)
					} else {
						c2 = append(c2, v)
					}
				}
			}
			d1, d2 := NewDist(c1), NewDist(c2)
			W.FirstCycle, W.SecondCycle = &d1, &d2
		}
		L.Warmup = W
	}

	meas := frames[warm:]
	var lats, encs, plats, klats, calls, late, xfer, sizes []float64
	var bytesTotal int64
	var firstDone, lastDone int64 = -1, -1
	skip := 0
	if mode == modeThroughput {
		skip = min(30, len(meas)/4)
	}
	for i, f := range meas {
		L.Frames.Submitted++
		if f.has(flagDropped) {
			L.Frames.Dropped++
		}
		if f.SubmitStatus != 0 || f.CbStatus != 0 {
			L.Frames.Failed++
		}
		if !f.has(flagCallback) {
			L.Frames.Missing++
		}
		if f.has(flagTruncated) {
			L.Frames.Truncated++
		}
		calls = append(calls, ms(f.ReturnNs-f.SubmitNs))
		if mode == modePaced {
			late = append(late, ms(f.SubmitNs-f.TargetNs))
		}
		if f.XferNs > 0 {
			xfer = append(xfer, ms(f.XferNs))
		}
		v, ok := lat(f)
		if !ok || f.Bytes == 0 {
			continue
		}
		L.Frames.Encoded++
		bytesTotal += int64(f.Bytes)
		sizes = append(sizes, float64(f.Bytes))
		if i >= skip {
			lats = append(lats, v)
			encs = append(encs, ms(f.DoneNs-f.SubmitNs))
			if v > criterionMs {
				L.OverCriterion++
			}
			if v > 1000/float64(cfg.FPS) {
				L.OverInterval++
			}
			if f.has(flagKeyframe) {
				klats = append(klats, v)
			} else {
				plats = append(plats, v)
			}
			if firstDone < 0 {
				firstDone = f.DoneNs
			}
			lastDone = f.DoneNs
		}
		if f.has(flagKeyframe) {
			L.Keyframes++
		}
	}
	L.LatencyMs = NewDist(lats)
	L.PFrameLatencyMs = NewDist(plats)
	L.KeyLatencyMs = NewDist(klats)
	L.SubmitCallMs = NewDist(calls)
	L.EncoderMs = NewDist(encs)
	if mode == modePaced && len(late) > 0 {
		d := NewDist(late)
		L.PacingLateMs = &d
	}
	if len(xfer) > 0 {
		d := NewDist(xfer)
		L.DownscaleMs = &d
	}
	if len(lats) > 1 && lastDone > firstDone {
		rate := float64(len(lats)-1) / (float64(lastDone-firstDone) / 1e9)
		if mode == modeThroughput {
			L.ThroughputFPS = round1(rate)
		}
		L.OutputFPS = round1(rate)
	}
	if n := len(meas); n > 0 && cfg.FPS > 0 {
		L.BitrateBps = float64(int64(float64(bytesTotal*8) / (float64(n) / float64(cfg.FPS))))
		win := cfg.FPS
		var sum int64
		for i, f := range meas {
			sum += int64(f.Bytes)
			if i >= win {
				sum -= int64(meas[i-win].Bytes)
			}
			if i >= win-1 {
				L.Max1sBitrateBps = max(L.Max1sBitrateBps, float64(sum*8))
			}
		}
	}
	for _, f := range frames {
		if f.has(flagForcedKF) {
			v, _ := lat(f)
			L.ForcedKeyframe = &ForcedKF{Frame: int(f.FrameIndex), Keyframe: f.has(flagKeyframe), LatencyMs: round3(v),
				Bytes: int(f.Bytes), MedianBytes: int(NewDist(sizes).P50)}
		}
	}
	if len(lr.Stream) > 0 {
		rep := AnalyzeStream(lr.Stream, auRefs(frames), 100)
		L.Stream = &rep
	}
	if cpu, ok := lr.Info["process_cpu_ms"].(float64); ok {
		if end, ok := lr.Info["end_ns"].(float64); ok && end > 0 {
			L.ProcessCPUPct = round1(cpu / (end / 1e6) * 100)
		}
	}
	L.Sustains = L.Frames.Dropped == 0 && L.Frames.Failed == 0 && L.Frames.Missing == 0 && L.Frames.Submitted > 0
	if mode == modePaced {
		interval := 1000 / float64(cfg.FPS)
		L.Sustains = L.Sustains && L.OutputFPS >= 0.98*float64(cfg.FPS) && L.LatencyMs.P99 < 2*interval
		L.WithinInterval = L.LatencyMs.P99 <= interval
	}
	return L
}

func verdict(r *Run, o options) string {
	if r.Status != 0 {
		return "session failed: " + r.StatusName
	}
	if len(r.Layers) == 0 {
		return "no layers"
	}
	var parts []string
	for _, L := range r.Layers {
		v := ""
		switch {
		case r.Kind == "throughput":
			v = fmt.Sprintf("%.0f fps sustained", L.ThroughputFPS)
		case L.Role == "full" && r.Size == criterionSize && r.FPS == 60:
			switch {
			case L.Sustains && L.LatencyMs.Max <= criterionMs:
				v = "PASS"
			case L.Sustains && L.LatencyMs.P99 <= criterionMs:
				v = fmt.Sprintf("PASS at p99 (%d of %d frames > %.0f ms, max %.1f ms)", L.OverCriterion,
					L.LatencyMs.N, criterionMs, L.LatencyMs.Max)
			default:
				v = fmt.Sprintf("FAIL (p99 %.1f ms, max %.1f ms, dropped %d, %.1f fps)", L.LatencyMs.P99,
					L.LatencyMs.Max, L.Frames.Dropped, L.OutputFPS)
			}
		case L.Sustains && L.WithinInterval:
			v = fmt.Sprintf("sustains %d fps, p99 within one frame interval", L.FPS)
		case L.Sustains:
			v = fmt.Sprintf("sustains %d fps, but p99 %.1f ms > one frame interval", L.FPS, L.LatencyMs.P99)
		default:
			v = fmt.Sprintf("does not sustain %d fps (%.1f fps, p99 %.1f ms, dropped %d)", L.FPS, L.OutputFPS,
				L.LatencyMs.P99, L.Frames.Dropped)
		}
		if L.Stream != nil && !L.Stream.OK {
			v += "; stream check FAILED"
		}
		if L.Decode != nil && !L.Decode.OK {
			v += "; decode check FAILED"
		}
		if len(r.Layers) > 1 {
			v = L.Role + ": " + v
		}
		parts = append(parts, v)
	}
	return strings.Join(parts, "; ")
}

// compareLayers records how much the second session and the downscale cost the full layer.
func compareLayers(two, single, previewOnly *Run) {
	if two == nil || len(two.Layers) < 2 {
		return
	}
	in := map[string]any{}
	full, prev := two.Layers[0], two.Layers[1]
	if single != nil && len(single.Layers) > 0 {
		s := single.Layers[0]
		two.ComparedTo = single.Name
		in["full_p50_delta_ms"] = round3(full.LatencyMs.P50 - s.LatencyMs.P50)
		in["full_p99_delta_ms"] = round3(full.LatencyMs.P99 - s.LatencyMs.P99)
		in["full_max_delta_ms"] = round3(full.LatencyMs.Max - s.LatencyMs.Max)
	}
	if previewOnly != nil && len(previewOnly.Layers) > 0 {
		p := previewOnly.Layers[0]
		in["preview_p50_delta_ms"] = round3(prev.LatencyMs.P50 - p.LatencyMs.P50)
		in["preview_p99_delta_ms"] = round3(prev.LatencyMs.P99 - p.LatencyMs.P99)
		if prev.DownscaleMs != nil && p.DownscaleMs != nil {
			in["downscale_p50_delta_ms"] = round3(prev.DownscaleMs.P50 - p.DownscaleMs.P50)
		}
	}
	two.Interaction = in
}

func summarize(runs []*Run) []SummaryRow {
	var rows []SummaryRow
	for _, r := range runs {
		if len(r.Layers) == 0 {
			rows = append(rows, SummaryRow{Run: r.Name, Verdict: r.Verdict})
			continue
		}
		for _, L := range r.Layers {
			row := SummaryRow{Run: r.Name, Layer: L.Role, EncoderID: L.EncoderID, P50: L.LatencyMs.P50,
				P95: L.LatencyMs.P95, P99: L.LatencyMs.P99, Max: L.LatencyMs.Max, FPS: L.OutputFPS,
				Dropped: L.Frames.Dropped, Mbps: round3(L.BitrateBps / 1e6), Keyframes: L.Keyframes, Verdict: r.Verdict}
			if L.Decode != nil {
				row.PSNR = round3(L.Decode.PSNRYMean)
			}
			rows = append(rows, row)
		}
	}
	return rows
}

func printRun(r *Run) {
	fmt.Printf("%-40s ", r.Name)
	if r.Status != 0 {
		fmt.Printf("session failed: %s\n", r.StatusName)
		return
	}
	for i, L := range r.Layers {
		if i > 0 {
			fmt.Printf("%-40s ", "  └ "+L.Role)
		}
		fmt.Printf("lat p50 %5.2f p95 %5.2f p99 %5.2f max %6.2f ms | %5.1f fps | drop %d fail %d miss %d | %6.2f Mbps (1s max %6.2f) | kf %d",
			L.LatencyMs.P50, L.LatencyMs.P95, L.LatencyMs.P99, L.LatencyMs.Max, L.OutputFPS,
			L.Frames.Dropped, L.Frames.Failed, L.Frames.Missing, L.BitrateBps/1e6, L.Max1sBitrateBps/1e6, L.Keyframes)
		fmt.Printf(" | enc p50 %.2f call p99 %.2f", L.EncoderMs.P50, L.SubmitCallMs.P99)
		if L.Warmup != nil {
			fmt.Printf(" | first %.2f ms", L.Warmup.FirstFrameMs)
		}
		if L.DownscaleMs != nil {
			fmt.Printf(" | downscale p50 %.2f max %.2f ms", L.DownscaleMs.P50, L.DownscaleMs.Max)
		}
		if L.Decode != nil {
			fmt.Printf(" | dec %d/%d", L.Decode.Decoded, L.Decode.AccessUnits)
			if L.Decode.PSNRFrames > 0 {
				fmt.Printf(" %.1f dB", L.Decode.PSNRYMean)
			}
		}
		if L.Stream != nil && L.Stream.SPS != nil {
			fmt.Printf(" | %s", L.Stream.SPS.ProfileLevelID)
		}
		fmt.Printf(" | %s", L.EncoderID)
		if len(L.Rejected) > 0 {
			fmt.Printf(" | rejected: %s", strings.Join(L.Rejected, ", "))
		}
		fmt.Println()
	}
	fmt.Printf("%-40s => %s\n", "", r.Verdict)
}

func printSummary(rows []SummaryRow) {
	fmt.Println("\n| run | layer | p50 | p95 | p99 | max (ms) | fps | dropped | Mbps | kf | verdict |")
	fmt.Println("|---|---|---|---|---|---|---|---|---|---|---|")
	for _, r := range rows {
		fmt.Printf("| %s | %s | %.2f | %.2f | %.2f | %.2f | %.1f | %d | %.2f | %d | %s |\n", r.Run, r.Layer, r.P50, r.P95,
			r.P99, r.Max, r.FPS, r.Dropped, r.Mbps, r.Keyframes, r.Verdict)
	}
}

func writeCSV(dir, name, role string, frames []Frame) {
	if dir == "" {
		return
	}
	var sb strings.Builder
	sb.WriteString("i,frame,target_ms,submit_ms,return_ms,done_ms,latency_ms,xfer_ms,bytes,key,dropped,submit_status,cb_status,flags\n")
	for i, f := range frames {
		l := 0.0
		if f.DoneNs > 0 {
			l = ms(f.DoneNs - f.SubmitNs)
		}
		fmt.Fprintf(&sb, "%d,%d,%.3f,%.3f,%.3f,%.3f,%.3f,%.3f,%d,%v,%v,%d,%d,%d\n", i, f.FrameIndex, ms(f.TargetNs),
			ms(f.SubmitNs), ms(f.ReturnNs), ms(f.DoneNs), l, ms(f.XferNs), f.Bytes, f.has(flagKeyframe),
			f.has(flagDropped), f.SubmitStatus, f.CbStatus, f.Flags)
	}
	fn := filepath.Join(dir, strings.NewReplacer(" ", "_", "=", "-").Replace(name)+"-"+role+".csv")
	_ = os.WriteFile(fn, []byte(sb.String()), 0o644)
}

// variant changes one thing relative to the paced baseline (NV12, 1440p60 by default).
type variant struct {
	name    string
	mod     func(e *EncConfig)
	ring    int                // 0: default ring
	prevMod func(p *EncConfig) // non-nil: two layers, preview config changed by prevMod
}

var variants = []variant{
	{"llrc=off realtime=off", func(e *EncConfig) { e.LLRC, e.RealTime = false, false }, 0, nil},
	{"llrc=off expected-fps=unset", func(e *EncConfig) { e.LLRC, e.ExpectedFPS = false, 0 }, 0, nil},
	{"llrc=off expected-fps=120", func(e *EncConfig) { e.LLRC, e.ExpectedFPS = false, 120 }, 0, nil},
	{"llrc=off speed=off", func(e *EncConfig) { e.LLRC, e.Speed = false, false }, 0, nil},
	{"llrc=on datarate=off", func(e *EncConfig) { e.LLRC, e.DataRateFactor = true, 0 }, 0, nil},
	{"llrc=on expected-fps=unset", func(e *EncConfig) { e.LLRC, e.ExpectedFPS = true, 0 }, 0, nil},
	{"llrc=on ring=8", func(e *EncConfig) { e.LLRC = true }, 8, nil},
	{"llrc=off ring=8", func(e *EncConfig) { e.LLRC = false }, 8, nil},
	{"two-layer llrc=on, preview on the software encoder", func(e *EncConfig) { e.LLRC = true }, 0,
		func(p *EncConfig) { p.LLRC, p.RequireHW, p.Software = false, false, true }},
	{"two-layer llrc=on at 30 fps", func(e *EncConfig) { e.LLRC, e.FPS, e.ExpectedFPS = true, 30, 30 }, 0,
		func(p *EncConfig) {}},
}

func runVariants(res *Results, o options, sz, fname, dumpDir string) {
	spec, ok := sizeTable[sz]
	if !ok {
		fatalf("unknown -variant-size %q", sz)
	}
	if b, ok := o.bitrates[sz]; ok {
		spec.Bitrate = b
	}
	benches := map[int]*Bench{}
	defer func() {
		for _, b := range benches {
			b.Close()
		}
	}()
	fmt.Printf("\n== variants at %s %s\n", sz, fname)
	for _, v := range variants {
		ring := o.ringMax
		if v.ring > 0 {
			ring = v.ring
		}
		b := benches[ring]
		if b == nil {
			var err error
			if b, err = OpenBench(spec.W, spec.H, formatTable[fname], ring, int64(o.ringMB)<<20); err != nil {
				fatalf("prerender: %v", err)
			}
			benches[ring] = b
		}
		enc := EncConfig{Width: spec.W, Height: spec.H, FPS: o.fps, ExpectedFPS: o.fps, Bitrate: spec.Bitrate,
			DataRateFactor: o.dataRate, KeyintSec: o.gop.Seconds(), Speed: o.speed, RealTime: o.realTime,
			RequireHW: true}
		v.mod(&enc)
		name := fmt.Sprintf("%s%d %s variant: %s", sz, enc.FPS, fname, v.name)
		var prev *EncConfig
		if v.prevMod != nil {
			prev = &EncConfig{Width: o.previewW, Height: o.previewH, FPS: o.previewFPS, ExpectedFPS: o.previewFPS,
				Bitrate: o.previewBps, DataRateFactor: o.dataRate, KeyintSec: o.gop.Seconds(), LLRC: enc.LLRC,
				Speed: o.speed, RealTime: o.realTime, RequireHW: true}
			v.prevMod(prev)
		}
		r := runPaced(res, b, spec, fname, enc, o, name, prev, true, dumpDir)
		r.Kind, r.Variant = "variant", v.name
	}
}

// marshalResults indents the top level but writes each summary row and each run on one
// line, which keeps the file small and one run per line for diffs and grep/jq.
func marshalResults(res *Results) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("{\n")
	type field struct {
		key  string
		val  any
		list bool
	}
	fields := []field{
		{"spike", res.Spike, false}, {"date", res.Date, false}, {"system", res.System, false},
		{"system_end", res.SystemEnd, false}, {"tool", res.Tool, false}, {"settings", res.Settings, false},
		{"criterion", res.Criterion, false}, {"encoders", res.Encoders, false},
		{"supported_properties", res.SupportedProperties, false}, {"session_examples", res.SessionExamples, false},
		{"summary", res.Summary, true}, {"runs", res.Runs, true},
	}
	for i, f := range fields {
		fmt.Fprintf(&buf, "  %q: ", f.key)
		if f.list {
			rv := reflect.ValueOf(f.val)
			buf.WriteString("[\n")
			for k := 0; k < rv.Len(); k++ {
				b, err := json.Marshal(rv.Index(k).Interface())
				if err != nil {
					return nil, err
				}
				buf.WriteString("    ")
				buf.Write(b)
				if k < rv.Len()-1 {
					buf.WriteByte(',')
				}
				buf.WriteByte('\n')
			}
			buf.WriteString("  ]")
		} else {
			b, err := json.MarshalIndent(f.val, "  ", "  ")
			if err != nil {
				return nil, err
			}
			buf.Write(b)
		}
		if i < len(fields)-1 {
			buf.WriteByte(',')
		}
		buf.WriteByte('\n')
	}
	buf.WriteString("}\n")
	if !json.Valid(buf.Bytes()) {
		return nil, fmt.Errorf("generated JSON is invalid")
	}
	return buf.Bytes(), nil
}
