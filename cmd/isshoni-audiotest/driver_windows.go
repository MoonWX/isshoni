//go:build windows

package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/MoonWX/isshoni/internal/desktop/audio"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/native"
)

// This file is the Windows driver of the audiotest (docs/m2/05-windows-audio.md §18.1's driver_windows.go): it loads
// isshoni-media.dll through native.Open, wraps it in the SetExclusions recorder, drives it through the desktop audio
// policy service (internal/desktop/audio.Service, the app's own rules, prompts and FriendsHear), starts the
// fixtures as children of explorer.exe, starts an audio-only session itself and records the PCM tap. The analyzer,
// the capture bookkeeping, the verdict and the report are the portable core's; this file only produces their input.
//
// It builds on Windows only (the native engine exists only there, 04 §12.1). The Mac and CI ubuntu/macOS use
// driver_other.go and run the analyzer and verdict tests. This driver's cases run on the owner's PC: WA62a's right
// after group 6 merged, every case of 05 §18.5 (`run -gate`) and the `record`/`merge` of R1 after WA62b (group 7).
// The render endpoints come from mmdevice_windows.go.

const (
	reportEverySec = 0.25 // the report sample period (05 §18.6)
	silenceCapture = 2.0  // the silence pre-check session's length (s): the probe, then 1 s of endpoint loopback
	pidReuseLife   = 180  // case 27's fixture lifetime (s): the reuse loop can take a while
)

// winDriver is the Windows driver.
type winDriver struct {
	opt    driverOptions
	eng    *native.Engine
	hooks  *native.AudioTest
	log    *slog.Logger
	closed bool

	// What the report's redaction needs (05 §18.7): the work folders of the cases so far, and the app and exe names
	// the engine's reports showed.
	redactMu sync.Mutex
	workDirs []string
	names    map[string]bool
}

// noteRedaction remembers a work folder and names for the report's redaction.
func (d *winDriver) noteRedaction(workDir string, names ...string) {
	d.redactMu.Lock()
	defer d.redactMu.Unlock()
	if workDir != "" {
		d.workDirs = append(d.workDirs, workDir)
	}
	if d.names == nil {
		d.names = map[string]bool{}
	}
	for _, n := range names {
		if n != "" {
			d.names[n] = true
		}
	}
}

// redaction is what the report's redaction needs (05 §18.7): the work folders (and the temp folder they live in),
// the harness's folder, and the names seen.
func (d *winDriver) redaction() redactInfo {
	d.redactMu.Lock()
	defer d.redactMu.Unlock()
	info := redactInfo{workDirs: append([]string(nil), d.workDirs...)}
	if exe, err := os.Executable(); err == nil {
		info.harnessDir = filepath.Dir(exe)
	}
	for n := range d.names {
		info.names = append(info.names, n)
	}
	return info
}

// newDriver loads the engine (native.Open) and resolves its test hooks (05 §18.1). Any failure (the DLL missing, the
// wrong ABI, an export missing) is a setup error, never errUnsupportedOS.
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

// engineNow is the engine clock (im_clock().mono_ns) in ns.
func (d *winDriver) engineNow() int64 {
	ns, _ := d.hooks.EngineClockNS()
	return ns
}

// say writes a local note to the console (never into the report).
func (d *winDriver) say(format string, args ...any) {
	if d.opt.console != nil {
		fmt.Fprintf(d.opt.console, format+"\n", args...)
	}
}

// env is the report's environment (05 §18.7): the DLL version and SHA-256, the OS, and the machine. Names are
// redacted (no machine name, no absolute paths).
func (d *winDriver) env() (dllInfo, osInfo, machineInfo, error) {
	bi := d.eng.BuildInfo()
	dll := dllInfo{Version: bi.Engine}
	if sum, err := dllSHA256(""); err == nil {
		dll.SHA256 = sum
	}
	return dll, readOSInfo(), d.readMachine(), nil
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
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// readMachine is the report's machine (05 §18.4, recorded, not enforced). An empty RenderEndpoints list (none
// active, or no report) makes every case SKIP with exit 3 (05 §18.4, §18.8). The endpoints are {formFactor, bus}
// (05 §18.7: no friendly names), the default first, with the default's master volume and mute; Mic is a capture
// endpoint with desktop-app access (05 §18.4: the mic cases SKIP without one).
func (d *winDriver) readMachine() machineInfo {
	m := machineInfo{RenderEndpoints: []endpointInfo{}, CPU: readCPUVendor(), DuckingPreference: readDuckingPref()}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	r, err := d.eng.AudioReport(ctx)
	if err != nil {
		d.say("note: no audio report (%v): no render endpoint is known", err)
		return m
	}
	eps, err := listRenderEndpoints()
	switch {
	case err == nil && len(eps.endpoints) == r.Device.RenderEndpoints:
		for _, e := range eps.endpoints {
			m.RenderEndpoints = append(m.RenderEndpoints, endpointInfo{FormFactor: e.formFactor, Bus: e.bus})
		}
		m.MasterVolume, m.Muted = eps.volume, eps.muted
	default:
		if err != nil {
			d.say("note: the render endpoints could not be listed (%v): recorded as unknown", err)
		}
		for range r.Device.RenderEndpoints {
			m.RenderEndpoints = append(m.RenderEndpoints, endpointInfo{FormFactor: "unknown", Bus: "unknown"})
		}
	}
	if r.Device.CaptureEndpoints > 0 {
		access, why := micDesktopAccess()
		m.Mic = access
		if !access {
			d.say("note: %s: the mic cases will SKIP", why)
		}
	}
	return m
}

// The microphone privacy switches (Settings → Privacy & security → Microphone): "Microphone access" for the
// device, "Let apps access your microphone" for the user, and "Let desktop apps access your microphone".
const (
	consentStoreMic   = `SOFTWARE\Microsoft\Windows\CurrentVersion\CapabilityAccessManager\ConsentStore\microphone`
	consentStoreMicNP = consentStoreMic + `\NonPackaged`
)

// micDesktopAccess reports whether desktop apps may use the microphone (05 §18.4), from the consent store's three
// switches; a missing value is the default, allowed. why names the switch that is off.
func micDesktopAccess() (ok bool, why string) {
	denied := func(root registry.Key, path string) bool {
		k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
		if err != nil {
			return false
		}
		defer func() { _ = k.Close() }()
		v, _, err := k.GetStringValue("Value")
		return err == nil && strings.EqualFold(v, "Deny")
	}
	switch {
	case denied(registry.LOCAL_MACHINE, consentStoreMic):
		return false, "microphone access is off for this device"
	case denied(registry.CURRENT_USER, consentStoreMic):
		return false, "apps may not use the microphone"
	case denied(registry.CURRENT_USER, consentStoreMicNP):
		return false, "desktop apps may not use the microphone"
	}
	return true, ""
}

// selfCheck loads the DLL (done in newDriver), checks the probe, and starts and stops the watcher without sound
// (05 §18.2). native.Open already checked im_version and every export, the audio and im_test_* ones included.
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
		return errors.New("the engine is not an AudioController")
	}
	if err := ctl.WatchAudio(ctx, media.Exclusions{}); err != nil {
		return fmt.Errorf("watch: %w", err)
	}
	if err := ctl.UnwatchAudio(ctx); err != nil {
		return fmt.Errorf("unwatch: %w", err)
	}
	fmt.Fprintln(w, "  engine: watcher start/stop ok")
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
	fmt.Fprintf(w, "probe: result=%s osBuild=%d renderEndpoints=%d captureEndpoints=%d duckingPreference=%d\n",
		r.Probe.Result, r.Probe.OSBuild, r.Device.RenderEndpoints, r.Device.CaptureEndpoints, readDuckingPref())
	// The endpoints as the report names them (05 §18.7: {formFactor, bus}, no friendly names).
	eps, err := listRenderEndpoints()
	if err != nil {
		fmt.Fprintf(w, "render endpoints: not listed (%v)\n", err)
		return nil
	}
	for _, e := range eps.endpoints {
		def := ""
		if e.isDefault {
			def = " (default)"
		}
		fmt.Fprintf(w, "render endpoint: formFactor=%s bus=%s%s\n", e.formFactor, e.bus, def)
	}
	if access, why := micDesktopAccess(); !access {
		fmt.Fprintf(w, "microphone: %s\n", why)
	}
	return nil
}

// apps prints "What friends hear" with the built-in rules, as text on stdout only (05 §18.2): the app's name, its
// verdict, reason and detail. It is a local listing for the owner (WA35's check: Discord, a browser and a Steam game
// with the right verdicts), so it names apps; nothing is written to disk.
func (d *winDriver) apps(w io.Writer) error {
	svc, _, stop, err := d.newService(audio.DefaultSettings(), nil)
	if err != nil {
		return err
	}
	defer stop()
	if err := svc.Watch(true); err != nil {
		return err
	}
	time.Sleep(1500 * time.Millisecond) // let the watcher publish a report
	fh := svc.FriendsHear()
	fmt.Fprintf(w, "%-32s %-13s %-15s %s\n", "APP", "VERDICT", "REASON", "DETAIL")
	list := func(rows []audio.AppRow, verdict string) {
		for _, row := range rows {
			name := row.Name
			if name == "" {
				name = row.ID
			}
			fmt.Fprintf(w, "%-32s %-13s %-15s %s\n", name, verdict, row.Reason, row.Detail)
		}
	}
	list(fh.Heard, "heard")
	list(fh.KeptOut, "kept out")
	list(fh.NotCaptured, "not captured")
	return nil
}

// newService builds the desktop policy service over the recorder-wrapped engine with an in-memory settings store
// (05 §18.1: SelfInfo.Exe = ""), and runs it. stop stops the Service's Run loop.
func (d *winDriver) newService(settings audio.Settings, viewer *viewerCall) (*audio.Service, *recorder, func(), error) {
	ctl, ok := any(d.eng).(media.AudioController)
	if !ok {
		return nil, nil, nil, errors.New("the engine is not an AudioController")
	}
	rec := newRecorder(ctl, d.engineNow)
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

// caseSetup is what a case's preparation leaves for its recording: the service, the recorder, the work folder and
// the premises checked before any fixture started.
type caseSetup struct {
	svc         *audio.Service
	rec         *recorder
	work        string
	self        string
	watchNS     int64
	silenceFail bool
	keptOutPre  bool
	cleanup     []func()
}

func (s *caseSetup) close() {
	for i := len(s.cleanup) - 1; i >= 0; i-- {
		s.cleanup[i]()
	}
}

// prepare starts the service with the case's settings, copies every fixture exe (and extra) into a fresh work
// folder before any recording (05 §18.3), starts the watcher, and runs the preconditions of 05 §18.4: the kept-out
// check for the fast-path cases (named on the console) and the silence pre-check.
func (d *winDriver) prepare(cs caseSpec, cfg gateConfig, extra ...string) (*caseSetup, error) {
	settings := audio.DefaultSettings()
	settings.FastPath = cfg.FastPath || cs.forceFastPath
	for _, id := range cs.seedInclude {
		settings.AppRules[id] = audio.DecisionInclude
	}
	s := &caseSetup{}
	svc, rec, stop, err := d.newService(settings, cs.viewer)
	if err != nil {
		return nil, err
	}
	s.svc, s.rec = svc, rec
	s.cleanup = append(s.cleanup, stop)

	work, err := os.MkdirTemp("", "isshoni-audiotest-")
	if err != nil {
		s.close()
		return nil, err
	}
	s.work = work
	d.noteRedaction(work)
	s.cleanup = append(s.cleanup, func() { removeAllRetry(work) })
	if s.self, err = os.Executable(); err != nil {
		s.close()
		return nil, err
	}
	for _, rel := range append(copyList(cs), extra...) {
		if err := copyExe(s.self, filepath.Join(work, rel)); err != nil {
			s.close()
			return nil, err
		}
	}

	if err := svc.Watch(true); err != nil {
		s.close()
		return nil, err
	}
	s.watchNS = d.engineNow()

	// 05 §18.4: no non-fixture process may be kept out for the fast-path cases (no fixture runs yet).
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	r, err := rec.AudioReport(ctx)
	cancel()
	if err == nil {
		for _, a := range r.Apps {
			d.noteRedaction("", a.Name, a.Exe)
		}
		if others := keptOutNames(r); len(others) > 0 {
			s.keptOutPre = true
			if cs.forceFastPath {
				d.say("  %s: kept out, quit it for the fast-path cases: %s", cs.name, strings.Join(others, ", "))
			}
		}
	}

	if problem := d.silencePreCheck(svc, caseFreqs(cs)); problem != "" {
		s.silenceFail = true
		d.say("  %s: silence pre-check: %s", cs.name, problem)
	}
	return s, nil
}

// keptOutNames are the names of the apps and sessions a report keeps out for any reason but isshoni (05 §18.4's
// fast-path precondition), for the console.
func keptOutNames(r media.AudioReport) []string {
	var out []string
	seen := map[string]bool{}
	add := func(name string) {
		if name == "" {
			name = "(unnamed)"
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, a := range r.Apps {
		if a.Verdict == controlKeptOut && a.Reason != "isshoni" {
			add(a.Name)
		}
	}
	for _, s := range r.Sessions {
		if s.Verdict == controlKeptOut && s.Reason != "isshoni" {
			add(s.Exe)
		}
	}
	return out
}

// silencePreCheck captures endpoint loopback (an audio-only session with IM_AUDIOF_TEST_FAIL_PROBE and the tap)
// before a case and checks its last second (05 §18.4): every case frequency ≤ −70 dBFS and the RMS ≤ −60 dBFS. The
// first second covers the startup probe, whose own −60 dBFS 997 Hz tone plays before the engine falls back to the
// endpoint. It returns "" when quiet.
func (d *winDriver) silencePreCheck(svc *audio.Service, freqs []float64) string {
	ac := svc.AudioConfig()
	ac.TapPCM = true
	ac.TestFlags |= imAudiofTestFailProbe
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess, err := d.eng.Start(ctx, media.Config{Audio: &ac})
	if err != nil {
		return "the loopback session could not start: " + err.Error()
	}
	defer func() { _ = sess.Stop() }()
	c := newCapture(0, 0)
	deadline := time.Now().Add(secs(silenceCapture + 2))
	for c.now() < silenceCapture && time.Now().Before(deadline) {
		pctx, pcancel := context.WithTimeout(ctx, 60*time.Millisecond)
		pkt, err := sess.Next(pctx)
		pcancel()
		if err == nil {
			c.packet(pkt)
		}
	}
	return silenceProblem(segment(mono(c.left, c.right), sampleRate, silenceCapture-1, 0), freqs)
}

// imAudiofTestFailProbe is IM_AUDIOF_TEST_FAIL_PROBE (05 §4.1): the engine behaves as if the startup probe failed.
const imAudiofTestFailProbe = 0x80000000

// caseRun configures runCaseWith beyond the case itself.
type caseRun struct {
	dur      float64                     // the recording's length (s); 0 = caseDuration(cs)
	sink     func(left, right []float64) // non-nil: stream the timeline here and keep no audio (the soak)
	extra    []string                    // more exes to copy into the work folder before anything plays
	start    func(fx *fixtureRun, work string, dur float64)
	progress func(t float64) // called about once a second of the recording
}

// runCase runs one tone case and returns its recording (05 §18.5).
func (d *winDriver) runCase(cs caseSpec, cfg gateConfig) (recording, error) {
	return d.runCaseWith(cs, cfg, caseRun{})
}

// runCaseWith runs a case (05 §18.5): the preparation (copies, watcher, preconditions), the fixtures under explorer,
// the harness's own tone, a fixture the driver starts itself (cr.start: case 22's endpoint tone), the settle, the
// shared app's pid (case 14), an audio-only session with the tap, the late fixtures and mid-run calls timed from the
// recording's first tap chunk (case 13's Decide, case 15's SetAppDecision, case 21's bitrate), the report samples,
// and the recording.
func (d *winDriver) runCaseWith(cs caseSpec, cfg gateConfig, cr caseRun) (recording, error) {
	setup, err := d.prepare(cs, cfg, cr.extra...)
	if err != nil {
		return recording{}, err
	}
	defer setup.close()
	svc, rec := setup.svc, setup.rec

	caseDur := cr.dur
	if caseDur <= 0 {
		caseDur = caseDuration(cs)
	}
	fx := newFixtureRun(setup.work, d.engineNow)
	defer fx.killAll()
	if cs.selfTone > 0 {
		// isshoni's own playback (05 §18.5 cases 4 and 10): this process plays, from the settle to past the end.
		fx.addSelf()
		if err := playTone([]float64{cs.selfTone}, 0.125, caseDur+settleSec+1); err != nil {
			return recording{}, fmt.Errorf("the harness's own tone: %w", err)
		}
		defer stopSound()
	}
	for i := range cs.fixtures {
		if step := &cs.fixtures[i]; step.at <= 0 {
			fx.start(step, fixtureArgs(step, setup.work, caseDur))
		}
	}
	if cr.start != nil {
		cr.start(fx, setup.work, caseDur)
	}
	time.Sleep(secs(settleSec))

	// The shared app (case 14): its pid goes into the rules before the session starts, as 03's share controller
	// passes the pick's pid (05 §14.2).
	var sharedPID uint32
	if cs.sharedRole != "" {
		pid, ok := fx.pidOf(cs.sharedRole)
		if !ok {
			return recording{}, errors.New("the shared app's fixture did not start")
		}
		sharedPID = pid
		svc.SetSharedPID(pid)
	}

	// The audio-only session with the service's rules plus the tap and the test flags (05 §18.1).
	ac := svc.AudioConfig()
	ac.TapPCM = true
	if cs.failProbe {
		ac.TestFlags |= imAudiofTestFailProbe
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.OnShareStarted(sharedPID, true)
	defer svc.OnShareStopped()
	sess, err := d.eng.Start(ctx, media.Config{Audio: &ac})
	if err != nil {
		return recording{}, fmt.Errorf("start the audio session: %w", err)
	}
	defer func() { _ = sess.Stop() }()
	offset := d.clockOffset(sess)
	capt := newCapture(offset, setup.watchNS)
	if cr.sink != nil {
		capt = newStreamCapture(offset, setup.watchNS, cr.sink)
	}

	tSetIndex := int64(-1)
	lastProgress := 0.0
	d.pull(ctx, sess, capt, caseDur, func() {
		d.sample(capt, svc, rec, fx)
		if t := capt.now(); cr.progress != nil && t-lastProgress >= 1 {
			lastProgress = t
			cr.progress(t)
		}
	}, func(origin time.Time) {
		go fx.schedule(ctx, cs, origin, caseDur, svc, rec, sess, &tSetIndex)
	})
	d.sample(capt, svc, rec, fx)
	cancel()
	fx.killAll()

	end := fx.end(cs, caseDur)
	end.silenceFail, end.keptOutPre = setup.silenceFail, setup.keptOutPre
	if n := atomic.LoadInt64(&tSetIndex); n >= 0 {
		end.setStamp, end.setStampOK = rec.stampAt(int(n))
	}
	if others := capt.keptOutOthers(); len(others) > 0 && cs.forceFastPath {
		d.say("  %s: kept out during the case, quit it for the fast-path cases: %s", cs.name,
			strings.Join(others, ", "))
	}
	d.noteRedaction("", capt.seenNames()...)
	out := capt.finish(cs, end)
	if d.opt.keepWAV != "" && cr.sink == nil {
		if err := os.MkdirAll(d.opt.keepWAV, 0o750); err == nil {
			_ = writeWAV(filepath.Join(d.opt.keepWAV, cs.name+".wav"), interleave(out.left, out.right))
		}
	}
	return out, nil
}

// pull reads the session's packets into capt until the recording is dur seconds long, a wall-clock deadline
// passes or the session ends. From the first tap chunk on it calls onSample every 250 ms (05 §18.6); onStart, if
// set, runs once at the first tap chunk with the wall time of the recording's t = 0.
func (d *winDriver) pull(ctx context.Context, sess media.Session, capt *capture, dur float64, onSample func(),
	onStart func(origin time.Time)) {
	deadline := time.Now().Add(secs(dur + 5))
	var next time.Time
	for capt.now() < dur && time.Now().Before(deadline) {
		if capt.haveTap && !time.Now().Before(next) {
			onSample()
			if next = next.Add(secs(reportEverySec)); next.Before(time.Now()) {
				next = time.Now().Add(secs(reportEverySec)) // a slow sample: no burst of samples to catch up
			}
		}
		pctx, pcancel := context.WithTimeout(ctx, 60*time.Millisecond)
		pkt, err := sess.Next(pctx)
		pcancel()
		switch {
		case err == nil:
		case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
			continue // a pull timeout between packets
		default:
			return // the session ended, or the case was cancelled
		}
		hadTap := capt.haveTap
		capt.packet(pkt)
		if !hadTap && capt.haveTap {
			next = time.Now()
			if onStart != nil {
				// t = 0 is the first tap chunk's capture time; its wall time is now minus how long ago that was.
				lag := max(d.engineNow()-(capt.tap0+capt.offset), 0)
				onStart(time.Now().Add(-time.Duration(lag)))
			}
		}
	}
}

// sample takes one report sample: the engine report, FriendsHear's banners and prompts and the app levels, with
// the fixtures' pids as known now (05 §18.6).
func (d *winDriver) sample(capt *capture, svc *audio.Service, rec *recorder, fx *fixtureRun) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, err := rec.AudioReport(ctx)
	if err != nil {
		return
	}
	var levels *media.AudioLevels
	if l, err := rec.AudioLevels(ctx); err == nil {
		levels = &l
	}
	capt.sample(r, fhStateOf(svc.FriendsHear()), levels, fx.pidMap())
}

// fhStateOf is what a report sample reads from FriendsHear: the banner codes and the pending prompts.
func fhStateOf(fh audio.FriendsHear) fhState {
	st := fhState{banners: bannerCodesOf(fh)}
	for _, p := range fh.Prompts {
		st.prompts = append(st.prompts, promptState{kind: p.Kind, appID: p.AppID})
	}
	return st
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
	eng := d.engineNow()
	return eng - sess.Now()
}

// runSpecial runs a case the driver scores itself: timestamps (case 20), opus (case 21), multi-endpoint (case 22),
// soak (case 23) and pid-reuse (case 27).
func (d *winDriver) runSpecial(cs caseSpec, cfg gateConfig) (caseReport, error) {
	switch cs.special {
	case "timestamps":
		return d.runTimestamps(cs, cfg)
	case "opus":
		return d.runOpus(cs, cfg)
	case "multi-endpoint":
		return d.runMultiEndpoint(cs, cfg)
	case "soak":
		return d.runSoak(cs, cfg)
	case "pid-reuse":
		return d.runPIDReuse(cs, cfg)
	default:
		return caseReport{}, fmt.Errorf("unknown special case %q", cs.special)
	}
}

// runMultiEndpoint (case 22, INFO) records whether a tone on a second render endpoint is captured (05 §18.5): the
// game plays 1000 Hz on the default endpoint, and a reparented copy (`fixture endpoint-tone`, Games2\game2.exe)
// renders 683 Hz with im_test_render_tone on the first active render endpoint that isn't the default.
func (d *winDriver) runMultiEndpoint(cs caseSpec, cfg gateConfig) (caseReport, error) {
	eps, err := listRenderEndpoints()
	if err != nil {
		return caseReport{}, fmt.Errorf("list the render endpoints: %w", err)
	}
	other := ""
	for _, e := range eps.endpoints {
		if !e.isDefault {
			other = e.id
			break
		}
	}
	if other == "" {
		return skipped(cs, cfg, "no active render endpoint besides the default"), nil
	}
	exe, err := os.Executable()
	if err != nil {
		return caseReport{}, err
	}
	dllDir := filepath.Dir(exe)
	rec, err := d.runCaseWith(cs, cfg, caseRun{extra: []string{exeGame2},
		start: func(fx *fixtureRun, work string, dur float64) {
			fx.startExe("game2", filepath.Join(work, exeGame2), []string{"fixture", verbEndpointTone, "-role", "game2",
				"-dll", dllDir, "-endpoint", other, "-f", joinFreqs([]float64{freqLateGame}),
				"-d", fmtSec(dur + settleSec + 1)}, true)
		}})
	if err != nil {
		return caseReport{}, err
	}
	return scoreMultiEndpoint(cs, cfg, rec, len(eps.endpoints)), nil
}

// runSoak (case 23, `run -long`) records 30 minutes of the music-like fixture with the idle voice app holding the
// mic, streaming the tap into the soak analyzer (soak.go) instead of keeping it, and prints its progress each
// minute.
func (d *winDriver) runSoak(cs caseSpec, cfg gateConfig) (caseReport, error) {
	all, short := soakFreqs(cs)
	sa := newSoakAnalyzer(1.5, all, short)
	lastMin := 0
	rec, err := d.runCaseWith(cs, cfg, caseRun{dur: soakSec, sink: sa.feed, progress: func(t float64) {
		if m := int(t / 60); m > lastMin {
			lastMin = m
			d.say("  soak: %d of %d min", m, int(soakSec/60))
		}
	}})
	if err != nil {
		return caseReport{}, err
	}
	return scoreSoak(cs, cfg, rec, sa.result()), nil
}

// record is `record`'s recording (05 §18.2): the built-in rules plus -exclude's keep-out rules, a share-like audio
// session with the tap, every chunk to sink, and every second the apps friends hear and those kept out. Names are
// printed by the sink on the console only.
func (d *winDriver) record(ro recordOptions, sink recordSink) error {
	svc, _, stop, err := d.newService(audio.DefaultSettings(), nil)
	if err != nil {
		return err
	}
	defer stop()
	for _, exe := range ro.exclude {
		if err := svc.SetAppDecision("exe:"+exe, audio.DecisionKeepOut); err != nil {
			d.say("note: -exclude %s: %v", exe, err)
		}
	}
	if err := svc.Watch(true); err != nil {
		return err
	}
	ac := svc.AudioConfig()
	ac.TapPCM = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.OnShareStarted(0, true)
	defer svc.OnShareStopped()
	sess, err := d.eng.Start(ctx, media.Config{Audio: &ac})
	if err != nil {
		return fmt.Errorf("start the audio session: %w", err)
	}
	defer func() { _ = sess.Stop() }()
	var sinkErr error
	capt := newStreamCapture(d.clockOffset(sess), d.engineNow(), func(l, r []float64) {
		if sinkErr == nil {
			sinkErr = sink.pcm(l, r)
		}
	})
	lastStatus := -1.0
	d.pull(ctx, sess, capt, ro.dur, func() {
		if t := capt.now(); t-lastStatus >= 1 {
			lastStatus = t
			sink.status(hearStatusOf(t, svc.FriendsHear()))
		}
	}, nil)
	if capt.nSamples == 0 {
		return errors.New("no audio was captured")
	}
	return sinkErr
}

// hearStatusOf is one second of `record`'s status: the playing apps friends hear, and every app kept out.
func hearStatusOf(t float64, fh audio.FriendsHear) hearStatus {
	st := hearStatus{t: t}
	name := func(r audio.AppRow) string {
		if r.Name != "" {
			return r.Name
		}
		return r.ID
	}
	for _, r := range fh.Heard {
		if r.Playing {
			st.heard = append(st.heard, name(r))
		}
	}
	for _, r := range fh.KeptOut {
		st.keptOut = append(st.keptOut, namedReason{name: name(r), reason: r.Reason})
	}
	return st
}

// ---------------------------------------------------------------- fixtures

// trackedProc is one fixture process the harness holds a handle on: to see it exit (the "exited early" control) and
// to kill it at the end of the case without ever touching a reused pid.
type trackedProc struct {
	role string
	pid  uint32
	mu   sync.Mutex
	h    windows.Handle // 0 once the process exited and the handle was closed
	done chan struct{}
}

// fixtureRun tracks the fixture processes a case starts: their pids and roles, their logged times, and their exits.
type fixtureRun struct {
	work  string
	clock func() int64

	mu            sync.Mutex
	closed        bool
	allReparented bool
	pids          map[uint32]string
	procs         []*trackedProc
	byRole        map[string]*trackedProc
	cmds          []*exec.Cmd
	firstRender   map[string]int64
	started       map[string]int64
	exit          map[string]int64
	onsets        []int64
	micFail       bool // a fixture reported that it could not open the microphone
	wg            sync.WaitGroup
}

func newFixtureRun(work string, clock func() int64) *fixtureRun {
	return &fixtureRun{
		work: work, clock: clock, allReparented: true, pids: map[uint32]string{}, byRole: map[string]*trackedProc{},
		firstRender: map[string]int64{}, started: map[string]int64{}, exit: map[string]int64{},
	}
}

// addSelf registers the harness's own process as selfRole (it plays a tone itself, cases 4 and 10).
func (f *fixtureRun) addSelf() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pids[windows.GetCurrentProcessId()] = selfRole
}

// pidMap is a copy of the fixtures' pid → role map.
func (f *fixtureRun) pidMap() map[uint32]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[uint32]string, len(f.pids))
	for k, v := range f.pids {
		out[k] = v
	}
	return out
}

// start launches a top-level fixture step with args (fixtureArgs) now.
func (f *fixtureRun) start(step *fixtureStep, args []string) {
	f.startExe(step.role, filepath.Join(f.work, step.exe), args, step.underExplorer)
}

// startExe launches exe with args, reparented under explorer.exe when underExplorer is set (05 §18.3), registers its
// pid under role, and reads the JSON lines of it and its descendants from a pipe they all inherit.
func (f *fixtureRun) startExe(role, exe string, args []string, underExplorer bool) *trackedProc {
	f.mu.Lock()
	closed := f.closed
	f.mu.Unlock()
	if closed {
		return nil
	}
	cmd, reparented, stdout := startProcess(exe, args, underExplorer)
	f.mu.Lock()
	if underExplorer {
		f.allReparented = f.allReparented && reparented
	}
	f.mu.Unlock()
	if cmd == nil {
		return nil
	}
	tp := f.track(role, uint32(cmd.Process.Pid)) //nolint:gosec // G115: a PID fits in uint32
	f.mu.Lock()
	f.cmds = append(f.cmds, cmd)
	f.mu.Unlock()
	f.wg.Add(2)
	go func() { defer f.wg.Done(); _ = cmd.Wait() }() // reaps it and releases os/exec's handle when it exits
	go func() { defer f.wg.Done(); f.scan(stdout) }()
	return tp
}

// track registers pid under role and opens a handle on it, watched by a goroutine that records its exit.
func (f *fixtureRun) track(role string, pid uint32) *trackedProc {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pids[pid] = role
	for _, tp := range f.procs {
		if tp.pid == pid {
			return tp
		}
	}
	tp := &trackedProc{role: role, pid: pid, done: make(chan struct{})}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE|
		windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		close(tp.done)
		f.procs = append(f.procs, tp)
		return tp
	}
	tp.h = h
	f.procs = append(f.procs, tp)
	f.byRole[role] = tp
	if f.closed {
		_ = windows.TerminateProcess(h, 1) // announced after the case ended
	}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		_, _ = windows.WaitForSingleObject(h, windows.INFINITE)
		at := f.clock()
		tp.mu.Lock()
		_ = windows.CloseHandle(h)
		tp.h = 0
		tp.mu.Unlock()
		f.mu.Lock()
		if _, ok := f.exit[role]; !ok {
			f.exit[role] = at
		}
		f.mu.Unlock()
		close(tp.done)
	}()
	return tp
}

// scan reads the fixtures' JSON lines until every process holding the pipe exited: a hello registers the pid of a
// child the harness didn't start itself; a first render, a start, a microphone failure or the bursts' onsets are
// recorded.
func (f *fixtureRun) scan(r io.ReadCloser) {
	defer func() { _ = r.Close() }()
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		var l fixtureLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		if l.Role != "" && l.PID != 0 {
			f.track(l.Role, l.PID)
		}
		f.mu.Lock()
		if l.Role != "" && l.StartNS != 0 {
			f.started[l.Role] = l.StartNS
		}
		if l.Role != "" && l.FirstRenderNS != 0 {
			if _, ok := f.firstRender[l.Role]; !ok {
				f.firstRender[l.Role] = l.FirstRenderNS
			}
		}
		if len(l.OnsetNS) > 0 {
			f.onsets = append([]int64(nil), l.OnsetNS...)
		}
		if l.MicError != "" {
			f.micFail = true
		}
		f.mu.Unlock()
	}
}

// waitExit waits up to timeout for role's process to exit and its handle to close.
func (f *fixtureRun) waitExit(role string, timeout time.Duration) bool {
	f.mu.Lock()
	tp := f.byRole[role]
	f.mu.Unlock()
	if tp == nil {
		return false
	}
	select {
	case <-tp.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// pidOf is the pid of role's process, if known.
func (f *fixtureRun) pidOf(role string) (uint32, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tp := f.byRole[role]
	if tp == nil {
		return 0, false
	}
	return tp.pid, true
}

// schedule starts the late fixtures and makes the mid-run calls (05 §18.5), each at its time after origin, the wall
// time of the recording's t = 0: case 13's Decide at 5 s and case 15's SetAppDecision at 4 s (T_set is the first
// recorder stamp after the call), and case 21's bitrate change at 4 s. It registers them and returns; they end with
// ctx.
func (f *fixtureRun) schedule(ctx context.Context, cs caseSpec, origin time.Time, caseDur float64,
	svc *audio.Service, rec *recorder, sess media.Session, tSetIndex *int64) {
	at := func(s float64, fn func()) {
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			select {
			case <-ctx.Done():
			case <-time.After(time.Until(origin.Add(secs(s)))):
				fn()
			}
		}()
	}
	for i := range cs.fixtures {
		step := &cs.fixtures[i]
		if step.at > 0 {
			at(step.at, func() { f.start(step, fixtureArgs(step, f.work, caseDur)) })
		}
	}
	switch {
	case cs.name == "runtime-rule":
		at(4, func() {
			atomic.StoreInt64(tSetIndex, int64(rec.stampCount()))
			_ = svc.SetAppDecision("exe:fakevoice.exe", audio.DecisionKeepOut)
		})
	case cs.special == "opus":
		at(4, func() { _ = sess.SetBitrate(media.LayerAudio, 256000) })
	}
	if dc := cs.decide; dc != nil {
		at(dc.at, func() { f.decide(*dc, svc, rec, tSetIndex) })
	}
}

// decide answers the pending prompt of dc.role's app (case 13, 05 §18.5): the app id is that of the fixture's
// sessions in a fresh report; the recorder's stamp count is taken right before Service.Decide, so the next stamp is
// T_set. Without a pending prompt it does nothing: T_set never comes, and the verdict reports the missing prompt.
func (f *fixtureRun) decide(dc decideCall, svc *audio.Service, rec *recorder, tSetIndex *int64) {
	pid, ok := f.pidOf(dc.role)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r, err := rec.AudioReport(ctx)
	if err != nil {
		return
	}
	appID := ""
	for _, s := range r.Sessions {
		if s.PID == pid && s.AppID != "" {
			appID = s.AppID
			break
		}
	}
	for _, p := range svc.FriendsHear().Prompts {
		if appID != "" && p.AppID == appID {
			atomic.StoreInt64(tSetIndex, int64(rec.stampCount()))
			_ = svc.Decide(p.ID, audio.Decision(dc.decision))
			return
		}
	}
}

// end is what the fixture run knows for the capture's finish (05 §18.3, §18.6).
func (f *fixtureRun) end(cs caseSpec, caseDur float64) captureEnd {
	f.mu.Lock()
	defer f.mu.Unlock()
	return captureEnd{
		dur: caseDur, plan: planSpans(cs, caseDur), reparented: f.allReparented,
		firstRender: cloneNS(f.firstRender), start: cloneNS(f.started), exit: cloneNS(f.exit), micFail: f.micFail,
	}
}

func cloneNS(m map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// killAll ends every fixture of the case: each tracked process (orphans included) through the handle the harness
// holds, then each top-level tree with taskkill /T for anything untracked. It waits briefly for the watchers.
func (f *fixtureRun) killAll() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	procs := append([]*trackedProc(nil), f.procs...)
	cmds := append([]*exec.Cmd(nil), f.cmds...)
	f.mu.Unlock()
	for _, c := range cmds {
		_ = exec.CommandContext(context.Background(), "taskkill", "/F", "/T", "/PID", //nolint:gosec // G204: the harness kills its own fixtures
			strconv.Itoa(c.Process.Pid)).Run()
	}
	for _, tp := range procs {
		tp.mu.Lock()
		if tp.h != 0 {
			_ = windows.TerminateProcess(tp.h, 1)
		}
		tp.mu.Unlock()
	}
	done := make(chan struct{})
	go func() { f.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
}

// copyExe copies the harness binary to dst (a fixture copy), creating its folder.
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
	defer func() { _ = in.Close() }()
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

func bannerCodesOf(fh audio.FriendsHear) []string {
	out := make([]string, 0, len(fh.Banners))
	for _, b := range fh.Banners {
		out = append(out, b.Code)
	}
	return out
}

// ---------------------------------------------------------------- the special cases

// runTimestamps (case 20) compares the PCM-tap onset of each 2 kHz burst against im_test_render_bursts' predicted
// device-position time (05 §18.5, §13.2). The bursts are rendered by a reparented fixture with its own DLL, so they
// are not isshoni's tree. bursts.exe is copied before the session starts (05 §18.3) and killed with the case.
func (d *winDriver) runTimestamps(cs caseSpec, cfg gateConfig) (caseReport, error) {
	setup, err := d.prepare(cs, cfg, exeBursts)
	if err != nil {
		return caseReport{}, err
	}
	defer setup.close()
	const dur = 13.0
	fx := newFixtureRun(setup.work, d.engineNow)
	defer fx.killAll()

	ac := setup.svc.AudioConfig()
	ac.TapPCM = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess, err := d.eng.Start(ctx, media.Config{Audio: &ac})
	if err != nil {
		return caseReport{}, fmt.Errorf("start the audio session: %w", err)
	}
	defer func() { _ = sess.Stop() }()
	offset := d.clockOffset(sess)
	capt := newCapture(offset, setup.watchNS)
	fx.startExe("bursts", filepath.Join(setup.work, exeBursts),
		[]string{"fixture", "bursts", "-role", "bursts", "-dll", filepath.Dir(setup.self), "-n", "10"}, true)
	deadline := time.Now().Add(secs(dur + 5))
	nextReport := time.Now()
	for capt.now() < dur && time.Now().Before(deadline) {
		if capt.haveTap && !time.Now().Before(nextReport) {
			d.sample(capt, setup.svc, setup.rec, fx)
			nextReport = nextReport.Add(secs(reportEverySec))
		}
		pctx, pcancel := context.WithTimeout(ctx, 60*time.Millisecond)
		pkt, err := sess.Next(pctx)
		pcancel()
		if err == nil {
			capt.packet(pkt)
		}
	}
	// The fixture prints its onsets once the last burst has played.
	for wait := time.Now().Add(2 * time.Second); time.Now().Before(wait); time.Sleep(50 * time.Millisecond) {
		fx.mu.Lock()
		n := len(fx.onsets)
		fx.mu.Unlock()
		if n > 0 {
			break
		}
	}
	cancel()
	fx.killAll()
	end := fx.end(cs, dur)
	end.silenceFail, end.keptOutPre = setup.silenceFail, setup.keptOutPre
	rec := capt.finish(cs, end)
	fx.mu.Lock()
	predicted := make([]int64, 0, len(fx.onsets))
	for _, o := range fx.onsets {
		predicted = append(predicted, o-offset) // engine clock → session clock
	}
	fx.mu.Unlock()
	return scoreTimestamps(cs, cfg, rec, predicted), nil
}

// runOpus (case 21) records the case like a tone case (fixtures, controls, the bitrate change at t = 4 s), decodes
// every Opus packet with the DLL's libopus (im_test_opus_decode) and scores it (special.go).
func (d *winDriver) runOpus(cs caseSpec, cfg gateConfig) (caseReport, error) {
	rec, err := d.runCase(cs, cfg)
	if err != nil {
		return caseReport{}, err
	}
	decoded, decoded0, err := d.decodeOpus(rec.opus)
	if err != nil {
		return inconclusive(cs, cfg, "the Opus packets could not be decoded: %s", err), nil
	}
	return scoreOpus(cs, cfg, rec, decoded, decoded0), nil
}

// decodeOpus decodes every captured Opus packet with the DLL's libopus and returns the concatenated mono audio and
// the capture time of its first sample (the first packet's, session clock).
func (d *winDriver) decodeOpus(pkts []opusPkt) ([]float64, int64, error) {
	pcm := make([]float32, 2*opusMaxFramesDecode)
	var out []float64
	var first int64
	for i, p := range pkts {
		if i == 0 {
			first = p.captureNS
		}
		if len(p.data) == 0 {
			continue
		}
		frames, err := d.hooks.OpusDecode(p.data, pcm)
		if err != nil {
			return nil, 0, err
		}
		for f := range frames {
			out = append(out, (float64(pcm[2*f])+float64(pcm[2*f+1]))/2)
		}
	}
	return out, first, nil
}

// opusMaxFramesDecode is the longest Opus frame count per channel (120 ms at 48 kHz).
const opusMaxFramesDecode = 5760

// runPIDReuse (case 27, INFO) records whether Windows follows a reused PID into a heard app's tree (05 §5.2,
// §18.5): the game plays 1000; LauncherX (A) starts Discord (B, playing the chime partials) and exits at 2 s; the
// harness waits for A's exit and closes its handles on A, recreates A's pid as a suspended child of the game
// (`fixture reuse-pid`), and on a match runs AudioController.SoundCheck(<game's pid>, 3 s), an INCLUDE stream on the
// game's tree outside the policy whose "captured" means both chime partials were in it (§11.3). It records
// {matched, attempts, captured, gameReason}.
func (d *winDriver) runPIDReuse(cs caseSpec, cfg gateConfig) (caseReport, error) {
	setup, err := d.prepare(cs, cfg)
	if err != nil {
		return caseReport{}, err
	}
	defer setup.close()
	fx := newFixtureRun(setup.work, d.engineNow)
	defer fx.killAll()
	for i := range cs.fixtures {
		fx.start(&cs.fixtures[i], fixtureArgs(&cs.fixtures[i], setup.work, pidReuseLife))
	}
	cr := newCaseReport(cs, cfg)
	info := map[string]any{"matched": false, "attempts": 0, "captured": false, "gameReason": ""}
	cr.Info = info
	result := func(problem string) (caseReport, error) {
		if problem != "" {
			cr.Problems = append(cr.Problems, problem)
		}
		return withResult(cs, cfg, cr, resultInfo), nil
	}
	if !fx.reparented() {
		return result("fixtures weren't reparented under explorer.exe")
	}

	// Wait for A's exit (at 2 s) and for every handle the harness holds on A to close (os/exec's and the tracker's).
	if !fx.waitExit("launcherx", 15*time.Second) {
		return result("LauncherX (A) did not exit")
	}
	aPID, _ := fx.pidOf("launcherx")
	gamePID, ok := fx.pidOf("game")
	if !ok {
		return result("the game fixture did not start")
	}
	time.Sleep(500 * time.Millisecond) // os/exec's Wait releases its handle once A exits
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, setup.self, "fixture", "reuse-pid", //nolint:gosec // G204: this binary
		"-target", strconv.FormatUint(uint64(aPID), 10), "-parent", strconv.FormatUint(uint64(gamePID), 10)).Output()
	var l fixtureLine
	for _, line := range strings.Split(string(out), "\n") {
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &l) == nil && l.Matched != nil {
			break
		}
	}
	if l.Matched == nil {
		return result(fmt.Sprintf("reuse-pid printed no result (%v)", err))
	}
	info["matched"], info["attempts"] = *l.Matched, l.Attempts
	if !*l.Matched {
		return result("")
	}
	fx.track("reused", aPID) // killed with the case (it stays suspended until then)
	sc, err := setup.rec.SoundCheck(ctx, gamePID, 3*time.Second)
	if err != nil {
		return result("the sound check failed: " + err.Error())
	}
	info["captured"] = sc.Captured
	rctx, rcancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer rcancel()
	if r, err := setup.rec.AudioReport(rctx); err == nil {
		info["gameReason"] = gameRowReason(r, gamePID)
	}
	return result("")
}

// gameRowReason is the reason of the app row that the session of pid belongs to ("" if none).
func gameRowReason(r media.AudioReport, pid uint32) string {
	for _, s := range r.Sessions {
		if s.PID != pid {
			continue
		}
		for _, a := range r.Apps {
			if a.ID == s.AppID {
				return a.Reason
			}
		}
	}
	return ""
}

// reparented reports whether every reparented fixture so far was really started under explorer.exe.
func (f *fixtureRun) reparented() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.allReparented
}

// ---------------------------------------------------------------- Windows process helpers (S1's winapi)

// startProcess launches exe with args, reparented under explorer.exe when underExplorer is set (05 §18.3), with its
// stdout on a pipe that its descendants inherit. It returns the started command, whether it was reparented, and the
// pipe's read end (EOF once every process holding it exited). A failed reparented start is retried without
// reparenting (the case is then INCONCLUSIVE).
func startProcess(exe string, args []string, underExplorer bool) (*exec.Cmd, bool, io.ReadCloser) {
	try := func(reparent bool) (*exec.Cmd, bool, io.ReadCloser, error) {
		pr, pw, err := os.Pipe()
		if err != nil {
			return nil, false, nil, err
		}
		cmd := exec.CommandContext(context.Background(), exe, args...) //nolint:gosec // G204: the harness starts its own fixture copies
		cmd.Stdout = pw                                                // an *os.File: the child gets the handle itself, and Wait never closes the read end
		attr := &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		reparented := false
		var explorer windows.Handle
		if reparent {
			if h, err := openExplorer(); err == nil {
				explorer = h
				attr.ParentProcess = syscall.Handle(h)
				reparented = true
			}
		}
		cmd.SysProcAttr = attr
		err = cmd.Start()
		if explorer != 0 {
			_ = windows.CloseHandle(explorer) // only after CreateProcess has used it
		}
		_ = pw.Close()
		if err != nil {
			_ = pr.Close()
			return nil, false, nil, err
		}
		return cmd, reparented, pr, nil
	}
	cmd, reparented, out, err := try(underExplorer)
	if err != nil && underExplorer {
		cmd, reparented, out, err = try(false)
	}
	if err != nil {
		return nil, false, nil
	}
	return cmd, reparented, out
}

// openExplorer opens the user's explorer.exe in this session, for PROC_THREAD_ATTRIBUTE_PARENT_PROCESS (05 §18.3,
// S1's openExplorer). os/exec duplicates the standard handles into it, hence PROCESS_DUP_HANDLE.
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

// ntCurrentVersion is the registry key with the OS build's UBR and edition.
const ntCurrentVersion = `SOFTWARE\Microsoft\Windows NT\CurrentVersion`

// readOSInfo reads the Windows product, build, UBR and edition (05 §18.4; no machine identifiers).
func readOSInfo() osInfo {
	_, _, build := windows.RtlGetNtVersionNumbers()
	build &= 0xffff
	product := "win11"
	if build < 22000 {
		product = "win10"
	}
	o := osInfo{Product: product, Build: int(build)}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, ntCurrentVersion, registry.QUERY_VALUE)
	if err != nil {
		return o
	}
	defer func() { _ = k.Close() }()
	if ubr, _, err := k.GetIntegerValue("UBR"); err == nil && ubr <= 1<<31 {
		o.UBR = int(ubr)
	}
	if ed, _, err := k.GetStringValue("EditionID"); err == nil {
		o.Edition = ed
	}
	return o
}

// readCPUVendor is the first processor's vendor string ("GenuineIntel", "AuthenticAMD"; 05 §18.4).
func readCPUVendor() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DESCRIPTION\System\CentralProcessor\0`,
		registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer func() { _ = k.Close() }()
	v, _, err := k.GetStringValue("VendorIdentifier")
	if err != nil {
		return ""
	}
	return v
}

// readDuckingPref is HKCU\Software\Microsoft\Multimedia\Audio\UserDuckingPreference (0 mute, 1 −80 %, 2 −50 %,
// 3 do nothing; 05 §18.4 records it), or −1 when it is not set (Windows' default, which ducks) or unreadable.
func readDuckingPref() int {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Multimedia\Audio`, registry.QUERY_VALUE)
	if err != nil {
		return -1
	}
	defer func() { _ = k.Close() }()
	v, _, err := k.GetIntegerValue("UserDuckingPreference")
	if err != nil || v > 3 {
		return -1
	}
	return int(v)
}

// ---------------------------------------------------------------- the fixture subcommand (a fixture copy runs this)

// runFixture is what a renamed copy of the binary runs (05 §18.3): it plays a tone, idles, spawns a child, reuses a
// PID, renders bursts or an endpoint tone.
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
