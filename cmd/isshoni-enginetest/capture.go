package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

func init() {
	register(command{name: "capture", summary: "share a source for -seconds, write f.h264, q.h264, audio.ogg and report.json, check 04 §14",
		run: cmdCapture})
	register(command{name: "bench", summary: "capture without files (-seconds 30): encode and capture-to-packet percentiles, CPU",
		run: cmdBench})
}

// captureOptions are capture's and bench's flags (04 §14).
type captureOptions struct {
	source    string
	seconds   float64
	preset    string
	encoder   string
	profile   string
	audio     string
	keyframes time.Duration
	steps     []int
	dumpAUs   int
	out       string
	files     bool // capture writes files; bench doesn't
}

// The layers of a run: f at 1080p60 and q at 360p15, as a share of 01 asks for (01 §8.4's defaults).
func runLayers(fBps int) []media.LayerConfig {
	return []media.LayerConfig{
		{RID: "f", MaxPixels: 1920 * 1080, MaxFPS: 60, Bitrate: fBps},
		{RID: "q", MaxPixels: 640 * 360, MaxFPS: 15, Bitrate: 300_000},
	}
}

const (
	defaultFBps = 8_000_000
	// With -bitrate-steps f starts here, so that a first step to a common value (8M) is a real change.
	stepsBaseBps   = 4_000_000
	stepsFirstAt   = time.Second
	stepsSpacing   = 2500 * time.Millisecond
	keyframesFirst = time.Second
	defaultGOPMS   = 4000 // the engine's default for f (04 §9.4)
)

// parseBitrate reads "8M", "2.5M", "500k" or "800000".
func parseBitrate(s string) (int, error) {
	s = strings.TrimSpace(s)
	mult := 1.0
	switch {
	case strings.HasSuffix(s, "M"), strings.HasSuffix(s, "m"):
		mult, s = 1e6, s[:len(s)-1]
	case strings.HasSuffix(s, "k"), strings.HasSuffix(s, "K"):
		mult, s = 1e3, s[:len(s)-1]
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 || v*mult > 50e6 || v*mult < 100e3 {
		return 0, fmt.Errorf("bitrate %q: want 100k..50M", s)
	}
	return int(v * mult), nil
}

func parseSteps(s string) ([]int, error) {
	if s == "" {
		return nil, nil
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		v, err := parseBitrate(part)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// profileKey maps -profile to the engine's key.
func profileKey(p string) (string, bool) {
	switch p {
	case "high":
		return "6400", true
	case "cb":
		return "42e0", true
	}
	return "", false
}

func captureFlags(g *globals, name string, o *captureOptions, defSeconds float64, args []string) (int, bool) {
	fs := newFlags(g, name, "[flags]")
	fs.StringVar(&o.source, "source", "test:pattern", "`pick`, mon:<id> or test:pattern")
	fs.Float64Var(&o.seconds, "seconds", defSeconds, "how long to capture")
	fs.StringVar(&o.preset, "preset", "auto", "auto, game, movie or text")
	fs.StringVar(&o.encoder, "encoder", "auto", "auto, nvenc, qsv, amf, mf-hw, mf-sw or openh264 (04 §5.6's tokens)")
	fs.StringVar(&o.profile, "profile", "high", "high or cb")
	fs.StringVar(&o.audio, "audio", "off", "off, tone (the test tone) or mix (exclude-apps)")
	fs.DurationVar(&o.keyframes, "keyframes", 0, "request a keyframe on every layer this often (e.g. 500ms)")
	steps := fs.String("bitrate-steps", "", "f's bitrate targets, applied 2.5 s apart from 1 s after Start (e.g. 8M,2M,8M)")
	if o.files {
		fs.IntVar(&o.dumpAUs, "dump-aus", 0, "write q's first `N` access units to DIR/aus/<encoder>/ (annexb_test's fixtures)")
		fs.StringVar(&o.out, "out", "enginetest-capture", "write the files into `DIR`")
	}
	if code, ok := parseFlags(fs, args); !ok {
		return code, false
	}
	var err error
	if o.steps, err = parseSteps(*steps); err != nil {
		fmt.Fprintln(g.stderr, name+":", err)
		return exitUsage, false
	}
	switch {
	case o.seconds <= 0 || o.seconds > 24*3600:
		fmt.Fprintln(g.stderr, name+": -seconds out of range")
		return exitUsage, false
	case o.keyframes != 0 && len(o.steps) > 0:
		fmt.Fprintln(g.stderr, name+": -keyframes and -bitrate-steps don't combine (04 §14)")
		return exitUsage, false
	case o.keyframes < 0:
		fmt.Fprintln(g.stderr, name+": -keyframes must be positive")
		return exitUsage, false
	case o.audio != "off" && o.audio != "tone" && o.audio != "mix":
		fmt.Fprintln(g.stderr, name+": -audio is off, tone or mix")
		return exitUsage, false
	case o.dumpAUs < 0:
		fmt.Fprintln(g.stderr, name+": -dump-aus must be ≥ 0")
		return exitUsage, false
	}
	if _, ok := profileKey(o.profile); !ok {
		fmt.Fprintln(g.stderr, name+": -profile is high or cb")
		return exitUsage, false
	}
	switch o.preset {
	case "auto", "game", "movie", "text":
	default:
		fmt.Fprintln(g.stderr, name+": -preset is auto, game, movie or text")
		return exitUsage, false
	}
	if _, ok := encoderKinds[o.encoder]; !ok {
		fmt.Fprintln(g.stderr, name+": unknown -encoder", o.encoder)
		return exitUsage, false
	}
	return exitPass, true
}

var encoderKinds = map[string]bool{"auto": true, "nvenc": true, "qsv": true, "amf": true, "mf-hw": true,
	"mf-sw": true, "openh264": true, "fake": true}

func cmdCapture(ctx context.Context, g *globals, args []string) int {
	o := captureOptions{files: true}
	if code, ok := captureFlags(g, "capture", &o, 5, args); !ok {
		return code
	}
	return captureRun(ctx, g, o)
}

func cmdBench(ctx context.Context, g *globals, args []string) int {
	o := captureOptions{}
	if code, ok := captureFlags(g, "bench", &o, 30, args); !ok {
		return code
	}
	return captureRun(ctx, g, o)
}

// sink writes a run's files: the two layers' Annex B streams, the Ogg Opus audio and q's first AUs.
type sink struct {
	dir     string
	video   map[string]*os.File
	ogg     *oggWriter
	oggFile *os.File
	dumpN   int
	dumped  int
	dumpDir string
	err     error
}

func newSink(dir string, dumpN int) (*sink, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return &sink{dir: dir, video: map[string]*os.File{}, dumpN: dumpN}, nil
}

func (s *sink) setErr(err error) {
	if err != nil && s.err == nil {
		s.err = err
	}
}

func (s *sink) videoPacket(layer string, data []byte, encoder string) {
	f := s.video[layer]
	if f == nil {
		var err error
		f, err = os.Create(filepath.Join(s.dir, layer+".h264")) //nolint:gosec // G304: the user's -out folder
		if err != nil {
			s.setErr(err)
			return
		}
		s.video[layer] = f
	}
	_, err := f.Write(data)
	s.setErr(err)
	if layer == "q" && s.dumped < s.dumpN {
		if s.dumpDir == "" {
			s.dumpDir = filepath.Join(s.dir, "aus", encoder)
			s.setErr(os.MkdirAll(s.dumpDir, 0o750))
		}
		s.setErr(os.WriteFile(filepath.Join(s.dumpDir, fmt.Sprintf("%04d.h264", s.dumped)), data, 0o600))
		s.dumped++
	}
}

func (s *sink) audioPacket(data []byte) {
	if s.ogg == nil {
		f, err := os.Create(filepath.Join(s.dir, "audio.ogg")) //nolint:gosec // G304: the user's -out folder
		if err != nil {
			s.setErr(err)
			return
		}
		s.oggFile = f
		s.ogg = newOggOpus(f, 2)
	}
	s.setErr(s.ogg.WritePacket(data))
}

func (s *sink) close() error {
	for _, f := range s.video {
		s.setErr(f.Close())
	}
	if s.ogg != nil {
		s.setErr(s.ogg.Close())
		s.setErr(s.oggFile.Close())
	}
	return s.err
}

// latencies collects capture → packet times per layer (Session.Now() when Next returned − CaptureNS).
type latencies map[string][]time.Duration

func percentile(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(p*float64(len(s))+0.5) - 1
	return s[max(0, min(i, len(s)-1))]
}

// report is report.json (capture) and bench's -json output. It names no machine, user or path.
type report struct {
	Command      string             `json:"command"`
	Source       string             `json:"source"` // "pick", "mon" or "test:pattern": never a monitor or window name
	Seconds      float64            `json:"seconds"`
	Profile      string             `json:"profile"`
	Preset       string             `json:"preset"`
	Encoder      string             `json:"encoderAsked"`
	Audio        string             `json:"audio"`
	Layers       []reportLayer      `json:"layers"`
	Audios       int                `json:"audioPackets"`
	Actions      []action           `json:"actions"`
	Stats        *media.EngineStats `json:"stats,omitempty"`
	EncodeP95Max map[string]string  `json:"encodeP95Max,omitempty"`
	CPUPercent   float64            `json:"cpuPercentOfOneCore"`
	Failures     []string           `json:"failures"`
	Pass         bool               `json:"pass"`
}

type reportLayer struct {
	RID         string  `json:"rid"`
	Encoder     string  `json:"encoder"`
	HW          bool    `json:"hw"`
	LiveBitrate bool    `json:"liveBitrate"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	Packets     int     `json:"packets"`
	Keyframes   int     `json:"keyframes"`
	Reinits     int     `json:"reinits"`
	Bytes       int     `json:"bytes"`
	FirstMS     float64 `json:"firstPacketMs"`
	LatencyP50  string  `json:"captureToPacketP50"`
	LatencyP95  string  `json:"captureToPacketP95"`
	LatencyP99  string  `json:"captureToPacketP99"`
}

// captureRun is capture and bench: start a session, read it for -seconds while the control goroutine requests
// keyframes or steps the bitrate, stop, then check 04 §14's assertions (capture) or print the numbers (bench).
func captureRun(ctx context.Context, g *globals, o captureOptions) int {
	name := "bench"
	if o.files {
		name = "capture"
	}
	e, code, err := g.openEngine(o.audio != "mix")
	if err != nil {
		fmt.Fprintf(g.stderr, "%s: the engine didn't load: %v\n", name, err)
		return code
	}
	defer func() { _ = e.Close() }()

	profile, _ := profileKey(o.profile)
	fBps := defaultFBps
	if len(o.steps) > 0 {
		fBps = stepsBaseBps
	}
	cfg := media.Config{
		Source: o.source, Listed: true, Profile: profile, Preset: o.preset, Layers: runLayers(fBps),
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
	sourceKind := o.source
	switch {
	case o.source == "pick":
		src, err := e.Pick(ctx, media.PickRequest{})
		if err != nil {
			fmt.Fprintf(g.stderr, "%s: pick: %v\n", name, err)
			return envCode(err)
		}
		cfg.Source, cfg.Listed = src.ID, false
	case strings.HasPrefix(o.source, "mon:"):
		sourceKind = "mon"
	}

	var sk *sink
	if o.files {
		if sk, err = newSink(o.out, o.dumpAUs); err != nil {
			fmt.Fprintf(g.stderr, "%s: %v\n", name, err)
			return exitEnv
		}
	}
	cpu0 := processCPU()
	wall0 := time.Now()
	s, err := e.Start(ctx, cfg)
	if err != nil {
		fmt.Fprintf(g.stderr, "%s: start: %v\n", name, err)
		if sk != nil {
			_ = sk.close()
		}
		return envCode(err)
	}
	l := &runLog{
		StartNS: s.Now(), Layers: cfg.Layers, Profile: profile, Test: strings.HasPrefix(cfg.Source, "test:"),
		Tone: o.audio == "tone", GOPMS: defaultGOPMS, StepsBase: fBps, Steps: o.steps,
	}

	// The control goroutine: keyframe requests or bitrate steps, at their times on the session clock.
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(o.seconds*float64(time.Second)))
	defer cancel()
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		control(runCtx, s, o, l.StartNS, func(a action) {
			mu.Lock()
			l.Actions = append(l.Actions, a)
			mu.Unlock()
		}, g.stderr)
	}()

	encoders := map[string]*media.Event{}
	lat := latencies{}
	var statsMax = map[string]time.Duration{}
	nextStats := time.Now().Add(time.Second)
	var endErr error
	for {
		p, err := s.Next(runCtx)
		if err != nil {
			if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
				endErr = err
			}
			break
		}
		now := s.Now()
		r := rec{Kind: p.Kind, Layer: p.Layer, Key: p.Keyframe, Reinit: p.Reinit, CaptureNS: p.CaptureNS, RecvNS: now,
			Size: len(p.Data), Event: p.Event}
		switch p.Kind {
		case media.KindVideo:
			r.Types = nalTypes(p.Data)
			r.SPSIDC, r.SPSFlags, _ = spsProfile(p.Data)
			lat[p.Layer] = append(lat[p.Layer], time.Duration(now-p.CaptureNS))
			if sk != nil {
				enc := "unknown"
				if ev := encoders[p.Layer]; ev != nil {
					enc = ev.Encoder
				}
				sk.videoPacket(p.Layer, p.Data, enc)
			}
		case media.KindAudio:
			if sk != nil {
				sk.audioPacket(p.Data)
			}
		case media.KindEvent:
			if p.Event != nil && p.Event.Code == media.EventLayer {
				ev := *p.Event
				encoders[ev.Layer] = &ev
			}
		}
		l.Recs = append(l.Recs, r)
		if time.Now().After(nextStats) {
			nextStats = time.Now().Add(time.Second)
			if st, err := s.Stats(); err == nil {
				l.Stats = &st
				for _, ls := range st.Layers {
					statsMax[ls.RID] = max(statsMax[ls.RID], ls.EncodeP95)
				}
			}
		}
	}
	l.EndNS = s.Now()
	if st, err := s.Stats(); err == nil {
		l.Stats = &st
		for _, ls := range st.Layers {
			statsMax[ls.RID] = max(statsMax[ls.RID], ls.EncodeP95)
		}
	}
	cancel()
	wg.Wait()
	_ = s.Stop()
	wall := time.Since(wall0)
	cpuPct := 0.0
	if wall > 0 {
		cpuPct = 100 * (processCPU() - cpu0).Seconds() / wall.Seconds()
	}

	rep := report{
		Command: name, Source: sourceKind, Seconds: o.seconds, Profile: profile, Preset: o.preset, Encoder: o.encoder,
		Audio: o.audio, Actions: l.Actions, Stats: l.Stats, CPUPercent: cpuPct, EncodeP95Max: map[string]string{},
		Failures: []string{},
	}
	for rid, d := range statsMax {
		rep.EncodeP95Max[rid] = d.String()
	}
	for _, lc := range cfg.Layers {
		rl := reportLayer{RID: lc.RID}
		if ev := l.layerEvent(lc.RID); ev != nil {
			rl.Encoder, rl.HW, rl.LiveBitrate, rl.Width, rl.Height = ev.Encoder, ev.HW, ev.LiveBitrate, ev.Width, ev.Height
		}
		first := true
		for _, r := range l.Recs {
			if r.Kind != media.KindVideo || r.Layer != lc.RID {
				continue
			}
			if first {
				rl.FirstMS = float64(r.RecvNS-l.StartNS) / 1e6
				first = false
			}
			rl.Packets++
			rl.Bytes += r.Size
			if r.Key {
				rl.Keyframes++
			}
			if r.Reinit {
				rl.Reinits++
			}
		}
		d := lat[lc.RID]
		rl.LatencyP50, rl.LatencyP95, rl.LatencyP99 = percentile(d, 0.50).String(), percentile(d, 0.95).String(),
			percentile(d, 0.99).String()
		rep.Layers = append(rep.Layers, rl)
	}
	for _, r := range l.Recs {
		if r.Kind == media.KindAudio {
			rep.Audios++
		}
	}
	if endErr != nil {
		rep.Failures = append(rep.Failures, fmt.Sprintf("the session ended: %v", endErr))
	}
	if o.files {
		// bench prints numbers only (04 §14); capture asserts.
		rep.Failures = append(rep.Failures, checkRun(l)...)
		if err := sk.close(); err != nil {
			rep.Failures = append(rep.Failures, "writing the files: "+err.Error())
		}
	}
	rep.Pass = len(rep.Failures) == 0
	data, _ := json.MarshalIndent(rep, "", "  ")
	if o.files {
		if err := os.WriteFile(filepath.Join(o.out, "report.json"), append(data, '\n'), 0o600); err != nil {
			fmt.Fprintf(g.stderr, "%s: %v\n", name, err)
			return exitEnv
		}
	}
	if g.json {
		_, _ = g.stdout.Write(append(data, '\n'))
	} else {
		printReport(g.stdout, rep)
	}
	if endErr != nil {
		return envCode(endErr)
	}
	if !rep.Pass {
		return exitFail
	}
	return exitPass
}

// control requests keyframes (-keyframes) or steps f's bitrate (-bitrate-steps) at their times, recording each call.
func control(ctx context.Context, s media.Session, o captureOptions, startNS int64, record func(action), stderr io.Writer) {
	at := func(d time.Duration) bool {
		wait := time.Duration(startNS+int64(d)-s.Now()) * time.Nanosecond
		if wait <= 0 {
			return ctx.Err() == nil
		}
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		}
	}
	switch {
	case len(o.steps) > 0:
		for i, bps := range o.steps {
			if !at(stepsFirstAt + time.Duration(i)*stepsSpacing) {
				return
			}
			record(action{Kind: "bitrate", Layer: "f", AtNS: s.Now(), Bps: bps})
			if err := s.SetBitrate("f", bps); err != nil {
				fmt.Fprintln(stderr, "SetBitrate:", err)
			}
		}
	case o.keyframes > 0:
		for d := keyframesFirst; ; d += o.keyframes {
			if !at(d) {
				return
			}
			for _, layer := range []string{"f", "q"} {
				record(action{Kind: "keyframe", Layer: layer, AtNS: s.Now()})
				s.RequestKeyframe(layer)
			}
		}
	}
}

func printReport(w io.Writer, r report) {
	fmt.Fprintf(w, "%s: %s, %.0f s, profile %s, preset %s, encoder %s, audio %s (%d packets)\n", r.Command, r.Source,
		r.Seconds, r.Profile, r.Preset, r.Encoder, r.Audio, r.Audios)
	for _, l := range r.Layers {
		fmt.Fprintf(w, "layer %s: %s (hw %t, live bitrate %t) %dx%d, %d packets (%d keyframes, %d reinits, %d bytes), first %.0f ms\n",
			l.RID, l.Encoder, l.HW, l.LiveBitrate, l.Width, l.Height, l.Packets, l.Keyframes, l.Reinits, l.Bytes, l.FirstMS)
		fmt.Fprintf(w, "  capture → packet p50 %s p95 %s p99 %s; engine encode p95 (max of 1 s samples) %s\n",
			l.LatencyP50, l.LatencyP95, l.LatencyP99, r.EncodeP95Max[l.RID])
	}
	if r.Stats != nil {
		for _, s := range r.Stats.Layers {
			fmt.Fprintf(w, "stats %s: fpsIn %.1f fpsOut %.1f bitrate %d/%d limit %s drops %d\n", s.RID, s.FPSIn, s.FPSOut,
				s.BitrateBps, s.TargetBps, s.Limit, s.Drops)
		}
	}
	fmt.Fprintf(w, "process CPU: %.0f %% of one core\n", r.CPUPercent)
	for _, f := range r.Failures {
		fmt.Fprintln(w, "FAIL:", f)
	}
	if r.Pass {
		fmt.Fprintln(w, "PASS")
	}
}
