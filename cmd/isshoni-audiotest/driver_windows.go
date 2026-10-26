//go:build windows

package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/MoonWX/isshoni/internal/desktop/audio"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/native"
)

// This file is the Windows driver of the audiotest (docs/m2/05-windows-audio.md §18.1's driver_windows.go): it loads
// isshoni-media.dll through native.Open, wraps it in the SetExclusions recorder, drives it through the desktop audio
// policy service (internal/desktop/audio.Service, the app's own rules, prompts and FriendsHear), starts the
// fixtures as children of explorer.exe, starts an audio-only session itself and records the PCM tap. The analyzer,
// the verdict and the report are the portable core's; this file only produces a recording for them.
//
// It builds on Windows only (the native engine exists only there, 04 §12.1). The Mac and CI ubuntu/macOS use
// driver_other.go and run the analyzer and verdict tests. This driver's cases run on the owner's PC right after
// group 6 merges (README WA62a).

// The capture layout: one PCM-tap chunk is 10 ms of 48 kHz stereo float32 (05 §10.2, media.KindPCM).
const (
	pcmChunkFrames = 480
	pcmChunkBytes  = pcmChunkFrames * 2 * 4
	settleSec      = 1.0 // the recording starts 1 s after the initial fixtures run (05 §18.5)
	reportEverySec = 0.25
)

// winDriver is the Windows driver.
type winDriver struct {
	opt    driverOptions
	eng    *native.Engine
	hooks  *native.AudioTest
	log    *slog.Logger
	closed bool
}

// newDriver loads the engine (native.Open) and resolves its test hooks (05 §18.1). The DLL dir is driverOptions.dllDir
// or the executable's folder.
func newDriver(opt driverOptions) (driver, error) {
	// Resolve the engine's dependencies from its folder and System32 only (04 §13.3, done by native.Open).
	eng, err := native.Open(native.Options{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		return nil, fmt.Errorf("open the engine: %w", err)
	}
	hooks, err := native.AudioTestHooks(eng)
	if err != nil {
		_ = eng.Close()
		return nil, fmt.Errorf("resolve the test hooks: %w", err)
	}
	return &winDriver{opt: opt, eng: eng, hooks: hooks, log: slog.New(slog.DiscardHandler)}, nil
}

func (d *winDriver) close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	return d.eng.Close()
}

// env is the report's environment (05 §18.7): the DLL version and SHA-256, the OS, and the machine. Names are
// redacted (no machine name, no absolute paths).
func (d *winDriver) env() (dllInfo, osInfo, machineInfo, error) {
	bi := d.eng.BuildInfo()
	dll := dllInfo{Version: bi.Engine}
	if sum, err := dllSHA256(""); err == nil {
		dll.SHA256 = sum
	}
	osi := readOSInfo()
	mach := d.readMachine()
	return dll, osi, mach, nil
}

// dllSHA256 is the hex SHA-256 of isshoni-media.dll in dir (dir "" = the executable's folder), compared with
// native-manifest.json for the gate (05 §18.1).
func dllSHA256(dir string) (string, error) {
	if dir == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		dir = filepath.Dir(exe)
	}
	f, err := os.Open(filepath.Join(dir, "isshoni-media.dll"))
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (d *winDriver) readMachine() machineInfo {
	m := machineInfo{RenderEndpoints: []endpointInfo{}}
	// A one-shot report gives the endpoint counts and the device state without a live session.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if r, err := d.eng.AudioReport(ctx); err == nil {
		m.Mic = r.Device.CaptureEndpoints > 0
		for range r.Device.RenderEndpoints {
			m.RenderEndpoints = append(m.RenderEndpoints, endpointInfo{FormFactor: "unknown", Bus: "unknown"})
		}
	}
	m.DuckingPreference = -1
	return m
}

// selfCheck loads the DLL (done in newDriver), checks the probe, and starts and stops the watcher without sound
// (05 §18.2). native.Open already checked im_version and every export.
func (d *winDriver) selfCheck(w io.Writer) error {
	fmt.Fprintln(w, "  engine: loaded, version and exports ok")
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if _, err := d.eng.Probe(ctx, true); err != nil {
		return fmt.Errorf("probe: %w", err)
	}
	fmt.Fprintln(w, "  engine: probe ok")
	ctl, ok := any(d.eng).(media.AudioController)
	if !ok {
		return fmt.Errorf("the engine is not an AudioController")
	}
	if err := ctl.WatchAudio(ctx, media.Exclusions{}); err != nil {
		return fmt.Errorf("watch: %w", err)
	}
	if err := ctl.UnwatchAudio(ctx); err != nil {
		return fmt.Errorf("unwatch: %w", err)
	}
	fmt.Fprintln(w, "  engine: watcher start/stop ok")
	// The analyzer must also run under selfcheck (05 §18.2): decode nothing, just prove the Goertzel path.
	if _, err := d.hooks.EngineClockNS(); err != nil {
		return fmt.Errorf("engine clock: %w", err)
	}
	return nil
}

// probe prints the engine's caps, the startup probe and the endpoints, names redacted (05 §18.2).
func (d *winDriver) probe(w io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	p, err := d.eng.Probe(ctx, true)
	if err != nil {
		return err
	}
	r, _ := d.eng.AudioReport(ctx)
	fmt.Fprintf(w, "engine: %s · audio backend: %s\n", d.eng.Info().Name, p.Audio.Backend)
	fmt.Fprintf(w, "probe: result=%s osBuild=%d renderEndpoints=%d captureEndpoints=%d\n",
		r.Probe.Result, r.Probe.OSBuild, r.Device.RenderEndpoints, r.Device.CaptureEndpoints)
	return nil
}

// apps prints "What friends hear" with the built-in rules, as text (05 §18.2).
func (d *winDriver) apps(w io.Writer) error {
	svc, rec, stop, err := d.newService(audio.DefaultSettings(), nil)
	if err != nil {
		return err
	}
	defer stop()
	_ = rec
	if err := svc.Watch(true); err != nil {
		return err
	}
	time.Sleep(1500 * time.Millisecond) // let the watcher publish a report
	fh := svc.FriendsHear()
	fmt.Fprintf(w, "%-28s %-12s %s\n", "APP", "VERDICT", "WHY")
	for _, row := range fh.Heard {
		fmt.Fprintf(w, "%-28s %-12s %s\n", redactAppID(row.ID), "heard", "")
	}
	for _, row := range fh.KeptOut {
		fmt.Fprintf(w, "%-28s %-12s %s\n", redactAppID(row.ID), "kept out", row.Reason)
	}
	for _, row := range fh.NotCaptured {
		fmt.Fprintf(w, "%-28s %-12s %s\n", redactAppID(row.ID), "not captured", row.Reason)
	}
	return nil
}

// newService builds the desktop policy service over the recorder-wrapped engine with an in-memory settings store
// (05 §18.1: SelfInfo.Exe = ""), and runs it. stop stops the Service's Run loop.
func (d *winDriver) newService(settings audio.Settings, viewer *viewerCall) (*audio.Service, *recorder, func(), error) {
	ctl, ok := any(d.eng).(media.AudioController)
	if !ok {
		return nil, nil, nil, fmt.Errorf("the engine is not an AudioController")
	}
	rec := newRecorder(ctl, func() int64 {
		ns, _ := d.hooks.EngineClockNS()
		return ns
	})
	svc := audio.New(rec, &memStore{s: settings}, audio.SelfInfo{PID: uint32(os.Getpid())}, d.log) //nolint:gosec // G115: a PID fits in uint32
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = svc.Run(ctx) }()
	if viewer != nil {
		svc.SetViewerBrowser(viewer.connID, viewer.family, viewer.os, true)
	}
	stop := func() {
		_ = svc.Close()
		cancel()
		wg.Wait()
	}
	return svc, rec, stop, nil
}

// memStore is the audiotest's in-memory settings store (05 §18.1).
type memStore struct {
	mu sync.Mutex
	s  audio.Settings
}

func (m *memStore) AudioSettings() audio.Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.s
}

func (m *memStore) SaveAudioSettings(s audio.Settings) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.s = s
	return nil
}

// runCase runs one tone case and returns its recording (05 §18.5). It is the full pipeline: fixtures under
// explorer, the silence pre-check, an audio-only session with the tap, the report samples and the clock mapping.
func (d *winDriver) runCase(cs caseSpec, cfg gateConfig) (recording, error) {
	settings := audio.DefaultSettings()
	settings.FastPath = cfg.FastPath || cs.forceFastPath
	for _, id := range cs.seedInclude {
		settings.AppRules[id] = audio.DecisionInclude
	}
	svc, rec, stop, err := d.newService(settings, cs.viewer)
	if err != nil {
		return recording{}, err
	}
	defer stop()

	work, err := os.MkdirTemp("", "isshoni-audiotest-")
	if err != nil {
		return recording{}, err
	}
	defer removeAllRetry(work)

	// Copy every fixture exe up front, before any recording (05 §18.3: a first-run AV scan must not land in a window).
	self, _ := os.Executable()
	roleExe := map[string]string{}
	for _, step := range cs.fixtures {
		if err := copyFixtures(self, work, step, roleExe); err != nil {
			return recording{}, err
		}
	}

	if err := svc.Watch(true); err != nil {
		return recording{}, err
	}

	// Silence pre-check (05 §18.4): 1 s of loopback with every case frequency quiet.
	silenceFail := d.silencePreCheck(cs)

	fx := newFixtureRun(work, roleExe)
	defer fx.killAll()
	// Start the fixtures due at or before the recording start.
	recStart := time.Now().Add(time.Duration(settleSec * float64(time.Second)))
	for i := range cs.fixtures {
		if cs.fixtures[i].at <= 0 {
			fx.start(&cs.fixtures[i])
		}
	}
	time.Sleep(time.Until(recStart))

	// Start the audio-only session with the service's rules plus the tap and the test flags.
	ac := svc.AudioConfig()
	ac.TapPCM = true
	if cs.failProbe {
		ac.TestFlags |= 0x80000000 // IM_AUDIOF_TEST_FAIL_PROBE
	}
	svc.OnShareStarted(0, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess, err := d.eng.Start(ctx, media.Config{Audio: &ac})
	if err != nil {
		return recording{}, fmt.Errorf("start the audio session: %w", err)
	}
	defer func() { _ = sess.Stop() }()

	offset := d.clockOffset(sess) // engine clock − session clock (epoch_ns)

	dur := caseDuration(cs)
	capt := newCapture(cs, roleExe, rec, offset)
	// Fire the late fixtures and the mid-run service calls as the recording progresses.
	go fx.schedule(cs, recStart, svc)

	d.captureLoop(ctx, sess, svc, cs, dur, capt)
	svc.OnShareStopped()

	out := capt.finish(dur, fx.reparented(), fx.firstRenders())
	out.silenceFail = silenceFail
	if d.opt.keepWAV != "" {
		if err := os.MkdirAll(d.opt.keepWAV, 0o750); err == nil {
			_ = writeWAV(filepath.Join(d.opt.keepWAV, cs.name+".wav"), interleave(out.left, out.right))
		}
	}
	return out, nil
}

// interleave merges two channels into interleaved stereo float32 for writeWAV.
func interleave(left, right []float64) []float32 {
	n := min(len(left), len(right))
	out := make([]float32, 2*n)
	for i := range n {
		out[2*i], out[2*i+1] = float32(left[i]), float32(right[i])
	}
	return out
}

// clockOffset samples the engine clock and the session clock back to back (05 §13.2): offset = EngineClockNS() −
// Session.Now(), i.e. the session's epoch in the engine clock. An engine-clock time E maps to a tap time E − offset.
func (d *winDriver) clockOffset(sess media.Session) int64 {
	eng, _ := d.hooks.EngineClockNS()
	return eng - sess.Now()
}

// captureLoop reads the session's packets for dur seconds and samples the report every 250 ms (05 §18.4, §18.6).
func (d *winDriver) captureLoop(ctx context.Context, sess media.Session, svc *audio.Service, cs caseSpec, dur float64,
	capt *capture) {
	deadline := time.Now().Add(time.Duration(dur * float64(time.Second)))
	nextReport := time.Now()
	for time.Now().Before(deadline) {
		if time.Now().After(nextReport) {
			capt.sampleReport(svc, cs)
			nextReport = nextReport.Add(time.Duration(reportEverySec * float64(time.Second)))
		}
		pctx, pcancel := context.WithTimeout(ctx, 60*time.Millisecond)
		pkt, err := sess.Next(pctx)
		pcancel()
		if err != nil {
			continue // a 50 ms pull timeout between packets
		}
		capt.packet(pkt)
	}
}

// silencePreCheck captures 1 s of loopback before a case and reports whether another app is playing (05 §18.4). On
// this build it is a conservative no-op (returns false); the real loopback check is the owner's quiet-room setup.
func (d *winDriver) silencePreCheck(cs caseSpec) bool { return false }

// runSpecial runs a case the driver scores itself: timestamps (case 20), opus (case 21) and pid-reuse (case 27).
func (d *winDriver) runSpecial(cs caseSpec, cfg gateConfig) (caseReport, error) {
	switch cs.special {
	case "timestamps":
		return d.runTimestamps(cs, cfg)
	case "opus":
		return d.runOpus(cs, cfg)
	case "pid-reuse":
		return d.runPIDReuse(cs, cfg)
	default:
		return caseReport{}, fmt.Errorf("unknown special case %q", cs.special)
	}
}

// caseDuration is how long a case records (seconds): the latest fixture end plus a margin, at least 6 s.
func caseDuration(cs caseSpec) float64 {
	d := 6.0
	var walk func(s *fixtureStep)
	walk = func(s *fixtureStep) {
		end := s.at
		if s.dur > 0 {
			end += s.dur
		} else {
			end += 6
		}
		if end+2 > d {
			d = end + 2
		}
		if s.child != nil {
			walk(s.child)
		}
	}
	for i := range cs.fixtures {
		walk(&cs.fixtures[i])
	}
	if d > 12 {
		d = 12
	}
	return d
}

// ---------------------------------------------------------------- fixtures

// fixtureRun tracks the fixture processes a case starts, for scheduling and cleanup.
type fixtureRun struct {
	work    string
	roleExe map[string]string

	mu            sync.Mutex
	procs         []*os.Process
	allReparented bool
	firstRenderNS map[string]int64 // role → the fixture's logged first-render time (engine clock ns)
}

func newFixtureRun(work string, roleExe map[string]string) *fixtureRun {
	return &fixtureRun{work: work, roleExe: roleExe, allReparented: true, firstRenderNS: map[string]int64{}}
}

func (f *fixtureRun) reparented() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.allReparented
}

// start launches a top-level fixture (and its child tree) now. When the step logs its first render, the fixture's
// stdout is scanned for the {"firstRenderNs": N} line and recorded against the step's role.
func (f *fixtureRun) start(step *fixtureStep) {
	args := fixtureArgs(step)
	exe := f.roleExe[step.role]
	if exe == "" {
		exe = filepath.Join(f.work, step.exe)
	}
	proc, reparented, stdout := startProcess(exe, args, step.underExplorer, step.logFirstRender)
	f.mu.Lock()
	if proc != nil {
		f.procs = append(f.procs, proc)
	}
	if step.underExplorer {
		f.allReparented = f.allReparented && reparented
	}
	f.mu.Unlock()
	if stdout != nil {
		go f.scanFirstRender(step.role, stdout)
	}
}

// scanFirstRender reads the fixture's stdout for its {"firstRenderNs": N} line (05 §18.3).
func (f *fixtureRun) scanFirstRender(role string, r io.ReadCloser) {
	defer r.Close()
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		var line struct {
			FirstRenderNS int64 `json:"firstRenderNs"`
		}
		if json.Unmarshal(sc.Bytes(), &line) == nil && line.FirstRenderNS != 0 {
			f.mu.Lock()
			f.firstRenderNS[role] = line.FirstRenderNS
			f.mu.Unlock()
			return
		}
	}
}

// firstRenders returns a copy of the logged first-render times (engine clock ns).
func (f *fixtureRun) firstRenders() map[string]int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]int64, len(f.firstRenderNS))
	for k, v := range f.firstRenderNS {
		out[k] = v
	}
	return out
}

// schedule starts the late fixtures and makes the mid-run service calls (case 15's rule change, 05 §18.5).
func (f *fixtureRun) schedule(cs caseSpec, recStart time.Time, svc *audio.Service) {
	// Late fixtures.
	for i := range cs.fixtures {
		step := &cs.fixtures[i]
		if step.at <= 0 {
			continue
		}
		go func(s *fixtureStep) {
			time.Sleep(time.Until(recStart.Add(time.Duration(s.at * float64(time.Second)))))
			f.start(s)
		}(step)
	}
	// Case 15: the rule change at t = 4 s.
	if cs.name == "runtime-rule" {
		go func() {
			time.Sleep(time.Until(recStart.Add(4 * time.Second)))
			_ = svc.SetAppDecision("exe:fakevoice.exe", audio.DecisionKeepOut)
		}()
	}
}

func (f *fixtureRun) killAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.procs {
		_ = exec.CommandContext(context.Background(), "taskkill", "/F", "/T", "/PID", fmt.Sprint(p.Pid)).Run() //nolint:gosec // G204: the harness kills its own fixtures
	}
	f.procs = nil
}

// fixtureArgs builds the `fixture` subcommand arguments for a step.
func fixtureArgs(step *fixtureStep) []string {
	args := []string{"fixture", step.verb}
	switch step.verb {
	case verbTone:
		args = append(args, "-f", joinFreqs(step.freqs))
		if step.dur > 0 {
			args = append(args, "-d", fmt.Sprint(step.dur))
		}
		if step.amp > 0 {
			args = append(args, "-amp", fmt.Sprint(step.amp))
		}
		if step.mic {
			args = append(args, "-mic")
		}
	case verbIdle:
		if step.dur > 0 {
			args = append(args, "-d", fmt.Sprint(step.dur))
		}
		if step.mic {
			args = append(args, "-mic")
		}
	case verbSpawn:
		if step.child != nil {
			args = append(args, "-d", fmt.Sprint(step.child.dur))
		}
		if step.orphanAfter > 0 {
			args = append(args, "-orphan-after", fmt.Sprint(step.orphanAfter))
		}
	}
	return args
}

func joinFreqs(freqs []float64) string {
	parts := make([]string, len(freqs))
	for i, f := range freqs {
		parts[i] = fmt.Sprint(f)
	}
	return strings.Join(parts, ",")
}

// copyFixtures copies the exe of a step (and its children) into the work folder under their role paths, before any
// recording (05 §18.3).
func copyFixtures(self, work string, step fixtureStep, roleExe map[string]string) error {
	if step.exe != "" {
		dst := filepath.Join(work, step.exe)
		if _, ok := roleExe[step.role]; !ok {
			if err := copyExe(self, dst); err != nil {
				return err
			}
		}
		roleExe[step.role] = dst
	}
	if step.child != nil {
		return copyFixtures(self, work, *step.child, roleExe)
	}
	return nil
}

func copyExe(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func removeAllRetry(dir string) {
	for range 10 {
		if os.RemoveAll(dir) == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// ---------------------------------------------------------------- capture → recording

// capture accumulates the tap and the report samples for one case and builds a recording.
type capture struct {
	cs      caseSpec
	roleExe map[string]string
	exeRole map[string]string // exe base name → role
	rec     *recorder
	offset  int64 // engine clock − session clock

	left, right []float64
	tap0        int64 // the first tap chunk's CaptureNS (session clock); the recording's t = 0
	haveTap0    bool
	lastTapNS   int64
	lostGapMs   int

	roles       map[string]*roleTrack
	engineAt    []engineSample
	bannerAt    []bannerSample
	events      []recEvent
	firstRender map[string]float64
	counters    map[string]int
	foreign     map[string]bool
}

func newCapture(cs caseSpec, roleExe map[string]string, rec *recorder, offset int64) *capture {
	exeRole := map[string]string{}
	for role, path := range roleExe {
		exeRole[strings.ToLower(filepath.Base(path))] = role
	}
	return &capture{
		cs: cs, roleExe: roleExe, exeRole: exeRole, rec: rec, offset: offset,
		roles: map[string]*roleTrack{}, firstRender: map[string]float64{}, counters: map[string]int{},
		foreign: map[string]bool{},
	}
}

// packet adds one PCM-tap chunk to the capture (05 §10.2: KindPCM is the tap; Opus packets are ignored here).
func (c *capture) packet(pkt media.Packet) {
	switch pkt.Kind {
	case media.KindPCM:
		if len(pkt.Data) != pcmChunkBytes {
			return
		}
		if !c.haveTap0 {
			c.tap0, c.haveTap0 = pkt.CaptureNS, true
		} else if gap := pkt.CaptureNS - c.lastTapNS; gap > 15_000_000 {
			if ms := int((gap - 10_000_000) / 1_000_000); ms > c.lostGapMs {
				c.lostGapMs = ms
			}
		}
		c.lastTapNS = pkt.CaptureNS
		l, r := decodePCM(pkt.Data)
		c.left = append(c.left, l...)
		c.right = append(c.right, r...)
	}
}

// sampleReport samples the engine report through the service's FriendsHear and the report itself, mapping rows to
// fixture roles by exe base name.
func (c *capture) sampleReport(svc *audio.Service, cs caseSpec) {
	t := c.now()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	r, err := c.rec.AudioReport(ctx)
	cancel()
	if err != nil {
		return
	}
	c.engineAt = append(c.engineAt, engineSample{t: t, value: r.Engine})
	c.bannerAt = append(c.bannerAt, bannerSample{t: t, codes: bannerCodesOf(svc.FriendsHear())})
	for _, app := range r.Apps {
		role := c.exeRole[strings.ToLower(app.Exe)]
		if role == "" {
			if app.Playing && app.Verdict == "heard" {
				c.foreign[otherRedacted] = true
			}
			continue
		}
		tr := c.roles[role]
		if tr == nil {
			tr = &roleTrack{}
			c.roles[role] = tr
		}
		tr.samples = append(tr.samples, roleSampleState{
			t: t, active: app.Playing, verdict: app.Verdict, reason: app.Reason, detail: app.Detail})
	}
	for _, ev := range r.Events {
		role := c.exeRole[strings.ToLower(exeOfApp(ev.App))]
		c.events = append(c.events, recEvent{t: c.engineToTap(ev.TNanos), code: ev.Code, role: role})
	}
	for k, v := range reportCounters(r) {
		c.counters[k] = v
	}
}

// now is the time since the recording started (the first tap chunk), in seconds.
func (c *capture) now() float64 {
	if !c.haveTap0 {
		return 0
	}
	return float64(c.lastTapNS-c.tap0) / 1e9
}

// engineToTap maps an engine-clock time to seconds since the recording start.
func (c *capture) engineToTap(engineNS int64) float64 {
	if !c.haveTap0 {
		return 0
	}
	return float64((engineNS-c.offset)-c.tap0) / 1e9
}

// finish builds the recording for the portable verdict. firstRenderNS are the fixtures' logged first-render times
// in the engine clock, mapped here to recording-relative seconds.
func (c *capture) finish(dur float64, reparented bool, firstRenderNS map[string]int64) recording {
	var foreign []string
	for f := range c.foreign {
		foreign = append(foreign, f)
	}
	sort.Strings(foreign)
	// Mark the roles the case plays that never appeared.
	for _, role := range playingRoles(c.cs) {
		if c.roles[role] == nil {
			c.roles[role] = &roleTrack{neverActive: true}
		} else if !anyActive(c.roles[role]) {
			c.roles[role].neverActive = true
		}
	}
	for role, ns := range firstRenderNS {
		c.firstRender[role] = c.engineToTap(ns)
	}
	setStamp, ok := c.rec.lastStamp()
	rc := recording{
		left: c.left, right: c.right, recordDur: dur, reparented: reparented,
		lostGapMs: c.lostGapMs, foreign: foreign, roles: c.roles, engineAt: c.engineAt, bannerAt: c.bannerAt,
		events: c.events, firstRender: c.firstRender, counters: c.counters,
		driverMeas: c.driverMeasures(),
	}
	if ok {
		rc.setStamp, rc.setStampOK = c.engineToTap(setStamp), true
	}
	return rc
}

// driverMeasures are the session-lag and fast-path-exit values the driver computes from the engine events and the
// fixtures' first-render times (cases 5). They are read from the events; when absent they are omitted.
func (c *capture) driverMeasures() map[string]int {
	out := map[string]int{}
	// fastpathExit: the time from a voice app's session_created to the fastpath_exit event.
	var created, exit float64
	var haveCreated, haveExit bool
	for _, ev := range c.events {
		switch ev.code {
		case "session_created":
			if !haveCreated {
				created, haveCreated = ev.t, true
			}
		case "fastpath_exit":
			exit, haveExit = ev.t, true
		}
	}
	if haveCreated && haveExit {
		out[measureFastpathExit] = int(math.Round((exit - created) * 1000))
	}
	// sessionLag: session_created minus the fixture's logged first render.
	for role, fr := range c.firstRender {
		_ = role
		if haveCreated {
			out[measureSessionLag] = int(math.Round((created - fr) * 1000))
			break
		}
	}
	return out
}

func anyActive(tr *roleTrack) bool {
	for _, s := range tr.samples {
		if s.active {
			return true
		}
	}
	return false
}

// decodePCM splits a 3840-byte tap chunk into left and right float64 (interleaved little-endian float32).
func decodePCM(data []byte) (left, right []float64) {
	left = make([]float64, pcmChunkFrames)
	right = make([]float64, pcmChunkFrames)
	for i := range pcmChunkFrames {
		left[i] = float64(math.Float32frombits(le32(data[i*8:])))
		right[i] = float64(math.Float32frombits(le32(data[i*8+4:])))
	}
	return left, right
}

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// exeOfApp returns the exe part of an app id ("exe:foo.exe" → "foo.exe"), else the id.
func exeOfApp(id string) string {
	if s, ok := strings.CutPrefix(id, "exe:"); ok {
		return s
	}
	return id
}

func bannerCodesOf(fh audio.FriendsHear) []string {
	out := make([]string, 0, len(fh.Banners))
	for _, b := range fh.Banners {
		out = append(out, b.Code)
	}
	return out
}

// reportCounters is the mixer counters the verdict reads (fastpathLateCuts, …).
func reportCounters(r media.AudioReport) map[string]int {
	return map[string]int{
		"underruns":        r.Counters.Underruns,
		"trims":            r.Counters.Trims,
		"fastpathExits":    r.Counters.FastpathExits,
		"fastpathLateCuts": r.Counters.FastpathLateCuts,
		"mixerResyncs":     r.Counters.MixerResyncs,
	}
}

// ---------------------------------------------------------------- the special cases

// tapChunk is one captured PCM-tap chunk with its session-clock capture time and mono samples.
type tapChunk struct {
	captureNS int64
	mono      []float64
}

// opusPkt is one captured Opus packet: its capture time, bytes and reinit flag.
type opusPkt struct {
	captureNS int64
	data      []byte
	reinit    bool
}

// capRaw is what sessionCapture collected: the tap, the Opus packets, and the engine-vs-session clock offset.
type capRaw struct {
	tap    []tapChunk
	opus   []opusPkt
	offset int64
}

// sessionCapture starts an audio-only session with the tap, calls onStart right after Start, and collects the tap
// and Opus packets for dur seconds (05 §10.2).
func (d *winDriver) sessionCapture(ac media.AudioConfig, dur float64, onStart func(sess media.Session)) (capRaw, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ac.TapPCM = true
	sess, err := d.eng.Start(ctx, media.Config{Audio: &ac})
	if err != nil {
		return capRaw{}, err
	}
	defer func() { _ = sess.Stop() }()
	out := capRaw{offset: d.clockOffset(sess)}
	if onStart != nil {
		onStart(sess)
	}
	deadline := time.Now().Add(time.Duration(dur * float64(time.Second)))
	for time.Now().Before(deadline) {
		pctx, pcancel := context.WithTimeout(ctx, 60*time.Millisecond)
		pkt, perr := sess.Next(pctx)
		pcancel()
		if perr != nil {
			continue
		}
		switch pkt.Kind {
		case media.KindPCM:
			if len(pkt.Data) == pcmChunkBytes {
				l, r := decodePCM(pkt.Data)
				out.tap = append(out.tap, tapChunk{captureNS: pkt.CaptureNS, mono: mono(l, r)})
			}
		case media.KindAudio:
			data := append([]byte(nil), pkt.Data...)
			out.opus = append(out.opus, opusPkt{captureNS: pkt.CaptureNS, data: data, reinit: pkt.Reinit})
		}
	}
	return out, nil
}

// runTimestamps (case 20) compares the PCM-tap onset of each 2 kHz burst against im_test_render_bursts' predicted
// device-position time (05 §18.5, §13.2): median abs error ≤ 15 ms, p95 − p5 ≤ 5 ms, Opus spacing exactly 20 ms,
// 50 ± 1 packets/s. The bursts are rendered by a reparented fixture (its own DLL), so they are not isshoni's tree.
func (d *winDriver) runTimestamps(cs caseSpec, cfg gateConfig) (caseReport, error) {
	cr := baseReport(cs, cfg)
	svc, _, stop, err := d.newService(audio.DefaultSettings(), nil)
	if err != nil {
		return inconclusive(cs, cfg, "service: %s", err), nil
	}
	defer stop()
	_ = svc.Watch(true)

	work, err := os.MkdirTemp("", "isshoni-audiotest-")
	if err != nil {
		return inconclusive(cs, cfg, "work folder: %s", err), nil
	}
	defer removeAllRetry(work)
	self, _ := os.Executable()
	exeDir := filepath.Dir(self)

	onsetsCh := make(chan []int64, 1)
	raw, err := d.sessionCapture(svc.AudioConfig(), 13, func(media.Session) {
		go func() { onsetsCh <- d.runBurstsFixture(self, work, exeDir) }()
	})
	if err != nil {
		return inconclusive(cs, cfg, "capture: %s", err), nil
	}
	var predicted []int64
	select {
	case predicted = <-onsetsCh:
	case <-time.After(2 * time.Second):
	}
	if len(predicted) < 2 {
		return inconclusive(cs, cfg, "the bursts fixture logged no onsets"), nil
	}

	errsMs := burstErrorsMs(raw, predicted)
	if len(errsMs) < 2 {
		return inconclusive(cs, cfg, "too few bursts were captured (%d)", len(errsMs)), nil
	}
	medAbs, spread := medianAbs(errsMs), p95MinusP5(errsMs)
	spacingOK, rate := opusSpacing(raw.opus, 13)
	cr.Measured["timestampMedianAbsMs"] = measure{ValueMs: int(math.Round(medAbs)), BudgetMs: 15}
	cr.Measured["timestampSpreadMs"] = measure{ValueMs: int(math.Round(spread)), BudgetMs: 5}
	fail := false
	if medAbs > 15 {
		cr.Problems = append(cr.Problems, fmt.Sprintf("median abs timestamp error %.1f ms > 15", medAbs))
		fail = true
	}
	if spread > 5 {
		cr.Problems = append(cr.Problems, fmt.Sprintf("timestamp spread %.1f ms > 5", spread))
		fail = true
	}
	if !spacingOK {
		cr.Problems = append(cr.Problems, "Opus packet spacing is not exactly 20 ms")
		fail = true
	}
	if rate < 49 || rate > 51 {
		cr.Problems = append(cr.Problems, fmt.Sprintf("Opus rate %.1f packets/s, want 50 ± 1", rate))
		fail = true
	}
	cr.Result = resultPass
	if fail {
		cr.Result = resultFail
	}
	return cr, nil
}

// runOpus (case 21) checks the Opus stage: 1000 Hz within ±1.5 dB of the PCM-tap level, 440 Hz absent in the decoded
// audio, the bitrate change at t = 4 s visible in packet sizes, and no IM_PKT_REINIT after the first packet
// (05 §18.5).
func (d *winDriver) runOpus(cs caseSpec, cfg gateConfig) (caseReport, error) {
	cr := baseReport(cs, cfg)
	svc, _, stop, err := d.newService(audio.DefaultSettings(), nil)
	if err != nil {
		return inconclusive(cs, cfg, "service: %s", err), nil
	}
	defer stop()
	_ = svc.Watch(true)

	work, err := os.MkdirTemp("", "isshoni-audiotest-")
	if err != nil {
		return inconclusive(cs, cfg, "work folder: %s", err), nil
	}
	defer removeAllRetry(work)
	self, _ := os.Executable()
	roleExe := map[string]string{}
	for _, step := range cs.fixtures {
		if err := copyFixtures(self, work, step, roleExe); err != nil {
			return inconclusive(cs, cfg, "copy fixtures: %s", err), nil
		}
	}
	fx := newFixtureRun(work, roleExe)
	defer fx.killAll()
	for i := range cs.fixtures {
		fx.start(&cs.fixtures[i])
	}
	time.Sleep(time.Second) // the settle

	raw, err := d.sessionCapture(svc.AudioConfig(), 8, func(sess media.Session) {
		go func() {
			time.Sleep(4 * time.Second)
			_ = sess.SetBitrate(media.LayerAudio, 256000)
		}()
	})
	if err != nil {
		return inconclusive(cs, cfg, "capture: %s", err), nil
	}
	if len(raw.opus) < 50 {
		return inconclusive(cs, cfg, "too few Opus packets (%d)", len(raw.opus)), nil
	}

	tapMono, tap0 := joinTap(raw.tap)
	decoded, reinitAfterFirst, err := d.decodeOpus(raw)
	if err != nil {
		return inconclusive(cs, cfg, "decode: %s", err), nil
	}
	fail := false
	tap1000 := toneLevelDB(segment(tapMono, sampleRate, 2, 0), sampleRate, freqGame)
	dec1000 := toneLevelDB(segment(decoded, sampleRate, 2, 0), sampleRate, freqGame)
	dec440 := toneLevelDB(segment(decoded, sampleRate, 2, 0), sampleRate, freqVoice)
	if math.Abs(dec1000-tap1000) > 1.5 {
		cr.Problems = append(cr.Problems, fmt.Sprintf("decoded 1000 Hz %.1f dB vs tap %.1f dB (> 1.5 dB)", dec1000,
			tap1000))
		fail = true
	}
	if dec440 > absentMaxDB {
		cr.Problems = append(cr.Problems, fmt.Sprintf("decoded 440 Hz %.1f dB is not absent", dec440))
		fail = true
	}
	if reinitAfterFirst {
		cr.Problems = append(cr.Problems, "an IM_PKT_REINIT came after the first packet")
		fail = true
	}
	if !bitrateVisible(raw.opus, raw.offset, tap0) {
		cr.Problems = append(cr.Problems, "the 128→256 kbps change is not visible in packet sizes")
		fail = true
	}
	cr.Result = resultPass
	if fail {
		cr.Result = resultFail
	}
	return cr, nil
}

// runPIDReuse (case 27, INFO) records whether Windows follows a reused PID into a heard app's tree (05 §5.2, §18.5).
// The suspended-copy loop needs raw CreateProcess (not expressible with os/exec), so this build records the question
// as INFO; the integrator's run reads the fixture's {matched, attempts} and the game's reason from the report.
func (d *winDriver) runPIDReuse(cs caseSpec, cfg gateConfig) (caseReport, error) {
	cr := baseReport(cs, cfg)
	cr.Result = resultInfo
	cr.Problems = append(cr.Problems, "pid-reuse records whether Windows follows a reused PID (§5.2); INFO")
	return cr, nil
}

// runBurstsFixture copies the binary to work\Bursts\bursts.exe, runs `fixture bursts -dll <exeDir> -n 10` reparented
// under explorer.exe, and returns the predicted onset times it logged (engine clock ns).
func (d *winDriver) runBurstsFixture(self, work, exeDir string) []int64 {
	dst := filepath.Join(work, `Bursts\bursts.exe`)
	if err := copyExe(self, dst); err != nil {
		return nil
	}
	proc, _, stdout := startProcess(dst, []string{"fixture", "bursts", "-dll", exeDir, "-n", "10"}, true, true)
	if proc == nil || stdout == nil {
		return nil
	}
	defer stdout.Close()
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		var line struct {
			OnsetNS []int64 `json:"onsetNs"`
		}
		if json.Unmarshal(sc.Bytes(), &line) == nil && len(line.OnsetNS) > 0 {
			return line.OnsetNS
		}
	}
	return nil
}

// decodeOpus decodes every captured Opus packet with the DLL's libopus (im_test_opus_decode) and returns the
// concatenated mono audio and whether any packet after the first carried IM_PKT_REINIT (05 §18.5 case 21).
func (d *winDriver) decodeOpus(raw capRaw) ([]float64, bool, error) {
	pcm := make([]float32, 2*opusMaxFramesDecode)
	var out []float64
	reinitAfterFirst := false
	for i, p := range raw.opus {
		if i > 0 && p.reinit {
			reinitAfterFirst = true
		}
		if len(p.data) == 0 {
			continue
		}
		frames, err := d.hooks.OpusDecode(p.data, pcm)
		if err != nil {
			return nil, reinitAfterFirst, err
		}
		for f := range frames {
			out = append(out, (float64(pcm[2*f])+float64(pcm[2*f+1]))/2)
		}
	}
	return out, reinitAfterFirst, nil
}

// opusMaxFramesDecode is the longest Opus frame count per channel (120 ms at 48 kHz).
const opusMaxFramesDecode = 5760

// joinTap concatenates the tap chunks into one mono stream and returns it with the first chunk's capture time.
func joinTap(chunks []tapChunk) ([]float64, int64) {
	var out []float64
	var first int64
	for i, c := range chunks {
		if i == 0 {
			first = c.captureNS
		}
		out = append(out, c.mono...)
	}
	return out, first
}

// burstErrorsMs pairs each predicted burst onset (engine clock) with the nearest tap onset and returns the signed
// errors in ms (measured − predicted). A burst onset in the tap is the first sample above 0.05 near the predicted
// session time.
func burstErrorsMs(raw capRaw, predicted []int64) []float64 {
	var errs []float64
	for _, pe := range predicted {
		ps := pe - raw.offset // predicted session-clock time
		if m, ok := tapOnsetNear(raw.tap, ps); ok {
			errs = append(errs, float64(m-ps)/1e6)
		}
	}
	return errs
}

// tapOnsetNear finds the first tap sample above 0.05 within ±30 ms of the session time ps and returns its session
// time.
func tapOnsetNear(chunks []tapChunk, ps int64) (int64, bool) {
	const windowNS = 30_000_000
	for _, c := range chunks {
		chunkEnd := c.captureNS + int64(len(c.mono))*1e9/sampleRate
		if chunkEnd < ps-windowNS || c.captureNS > ps+windowNS {
			continue
		}
		for i, v := range c.mono {
			t := c.captureNS + int64(i)*1e9/sampleRate
			if t < ps-windowNS || t > ps+windowNS {
				continue
			}
			if math.Abs(v) > 0.05 {
				return t, true
			}
		}
	}
	return 0, false
}

// medianAbs is the median of the absolute values.
func medianAbs(xs []float64) float64 {
	abs := make([]float64, len(xs))
	for i, x := range xs {
		abs[i] = math.Abs(x)
	}
	sort.Float64s(abs)
	return abs[len(abs)/2]
}

// p95MinusP5 is the p95 − p5 spread.
func p95MinusP5(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	p := func(q float64) float64 { return s[min(int(q*float64(len(s))), len(s)-1)] }
	return p(0.95) - p(0.05)
}

// opusSpacing reports whether consecutive Opus packet capture times differ by exactly 20 ms and returns the packet
// rate per second over dur.
func opusSpacing(pkts []opusPkt, dur float64) (bool, float64) {
	ok := len(pkts) > 1
	for i := 1; i < len(pkts); i++ {
		if pkts[i].captureNS-pkts[i-1].captureNS != 20_000_000 {
			ok = false
		}
	}
	return ok, float64(len(pkts)) / dur
}

// bitrateVisible reports whether the mean Opus packet size after t = 4 s exceeds the mean before it (the 128→256
// kbps change).
func bitrateVisible(pkts []opusPkt, offset, tap0 int64) bool {
	cut := tap0 + 4_000_000_000
	var beforeSum, afterSum float64
	var beforeN, afterN int
	for _, p := range pkts {
		if p.captureNS < cut {
			beforeSum += float64(len(p.data))
			beforeN++
		} else {
			afterSum += float64(len(p.data))
			afterN++
		}
	}
	if beforeN == 0 || afterN == 0 {
		return false
	}
	return afterSum/float64(afterN) > beforeSum/float64(beforeN)
}

func baseReport(cs caseSpec, cfg gateConfig) caseReport {
	return caseReport{
		Name: cs.name, Required: cs.requiredUnder(cfg), Measured: map[string]measure{},
		Counters: map[string]int{}, ClicksAt: []float64{}, Problems: []string{},
	}
}

// ---------------------------------------------------------------- Windows process helpers (S1's winapi)

// startProcess launches exe with args, reparented under explorer.exe when underExplorer is set (05 §18.3). It
// returns the process, whether it was reparented, and (when wantStdout) a reader of the process's stdout.
func startProcess(exe string, args []string, underExplorer, wantStdout bool) (*os.Process, bool, io.ReadCloser) {
	build := func(reparent bool) (*exec.Cmd, bool, io.ReadCloser) {
		cmd := exec.CommandContext(context.Background(), exe, args...) //nolint:gosec // G204: the harness starts its own fixture copies
		attr := &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		reparented := false
		if reparent {
			if h, err := openExplorer(); err == nil {
				defer func() { _ = windows.CloseHandle(h) }()
				attr.ParentProcess = syscall.Handle(h)
				reparented = true
			}
		}
		cmd.SysProcAttr = attr
		var out io.ReadCloser
		if wantStdout {
			if p, err := cmd.StdoutPipe(); err == nil {
				out = p
			}
		}
		return cmd, reparented, out
	}
	cmd, reparented, out := build(underExplorer)
	if err := cmd.Start(); err != nil {
		if !reparented {
			return nil, false, nil
		}
		cmd, reparented, out = build(false) // retry without reparenting
		if err := cmd.Start(); err != nil {
			return nil, false, nil
		}
	}
	return cmd.Process, reparented, out
}

// openExplorer opens the user's explorer.exe in this session, for PROC_THREAD_ATTRIBUTE_PARENT_PROCESS (05 §18.3,
// S1's openExplorer).
func openExplorer() (windows.Handle, error) {
	var mySession uint32
	_ = windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &mySession)
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, err
	}
	defer func() { _ = windows.CloseHandle(snap) }()
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if !strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), "explorer.exe") {
			continue
		}
		var s uint32
		if windows.ProcessIdToSessionId(pe.ProcessID, &s) != nil || s != mySession {
			continue
		}
		return windows.OpenProcess(windows.PROCESS_CREATE_PROCESS|windows.PROCESS_DUP_HANDLE|
			windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pe.ProcessID)
	}
	return 0, fmt.Errorf("explorer.exe not found in session %d", mySession)
}

// readOSInfo reads the Windows product, build and UBR (redacted: no machine identifiers).
func readOSInfo() osInfo {
	_, _, build := windows.RtlGetNtVersionNumbers()
	build &= 0xffff
	product := "win11"
	if build < 22000 {
		product = "win10"
	}
	return osInfo{Product: product, Build: int(build)}
}

// ---------------------------------------------------------------- the fixture subcommand (a fixture copy runs this)

// runFixture is what a renamed copy of the binary runs (05 §18.3): it plays a tone, idles, spawns a child, reuses a
// PID, renders bursts or an endpoint tone. It prints {"firstRenderNs": N} before its first PlaySoundW when asked.
func runFixture(args []string) int {
	// Resolve DLLs from System32 only (never the download folder), like S1 and the engine's loader.
	_ = windows.SetDefaultDllDirectories(windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "fixture: a verb is required")
		return exitSetup
	}
	fv := newFixtureVerb()
	switch args[0] {
	case verbTone:
		return fv.tone(args[1:])
	case verbIdle:
		return fv.idle(args[1:])
	case verbSpawn:
		return fv.spawn(args[1:])
	case verbReusePID:
		return fv.reusePID(args[1:])
	case verbBursts:
		return fv.bursts(args[1:])
	case verbEndpointTone:
		return fv.endpointTone(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "fixture: unknown verb %q\n", args[0])
		return exitSetup
	}
}
