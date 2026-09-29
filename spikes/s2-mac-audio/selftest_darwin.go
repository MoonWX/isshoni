package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The self-test models the real situation with fake apps (dist/fixtures, built
// by build.sh): a "game" plays a tone friends SHOULD hear; "voice apps" play
// tones they must NOT hear. Every fake app is a copy of s2 in its own signed
// .app bundle with its own bundle ID, started through LaunchServices like a
// normal app. The recording is analysed per frequency (Goertzel), and the
// engine's status is sampled every second to prove each fake app really played
// and was classified as expected (no "absent because it never played" passes).

type toneSpec struct {
	App string // fixture app (fixtures/<App>.app)
	// Who plays: "" = App itself; "helper" = App's bundled helper app, started
	// with posix_spawn (Electron model); "cli" = a plain executable outside any
	// bundle, started by App (matched only via the responsible process);
	// "open:<Other>" = App launches Other through LaunchServices, Other plays.
	Via     string
	Freq    float64
	At      float64 // start (s) relative to recording start; <= 0 = before the share starts
	Dur     float64 // 0 = until the scenario ends
	Mic     bool
	KeptOut bool // expected classification of the playing process
}

var fixtureIDs = map[string]string{
	"FakeGame": "com.isshoni.test.game", "FakeGame2": "com.isshoni.test.game2",
	"FakeVoice": "com.isshoni.test.fakevoice", "FakeTS": "com.isshoni.test.faketeamspeak",
	"FakeBrowser": "com.isshoni.test.browser", "FakeVoIP": "com.isshoni.test.fakevoip",
}

// player is how the playing process shows up in the engine's app list.
func (t toneSpec) player() string {
	switch {
	case t.Via == "helper":
		return fixtureIDs[t.App] + ".helper"
	case t.Via == "cli":
		return "voice-cli-helper"
	case strings.HasPrefix(t.Via, "open:"):
		return fixtureIDs[strings.TrimPrefix(t.Via, "open:")]
	}
	return fixtureIDs[t.App]
}

type scenario struct {
	Name, What   string
	Mode         int
	Flags        uint32
	Apps         []string // exclusion rules on top of the built-in voice-app list
	Tones        []toneSpec
	SelfFreq     float64 // isshoni itself plays this (AudioQueue) during the recording
	WebFreq      float64 // isshoni plays this through a WKWebView (WebKit's GPU process)
	AddRuleAt    float64
	AddRule      string
	RemoveRuleAt float64 // drop sc.Apps (keep the built-in list) at this time
	Duration     float64
	Checks       []check
	ClickWindow  [2]float64 // assert no clicks in this window (0,0 = skip)
	NeedsMic     bool
	Info         bool // informational: never fails the run
}

// Test frequencies sit between musical notes (same as S1).
const (
	fGame   = 1031.0
	fGame2  = 683.0
	fVoice  = 453.0
	fTS     = 571.0
	fSelf   = 907.0
	toneAmp = 0.1
)

func present(f float64, why string) check {
	return check{Freq: f, From: 1.5, Expect: "present", Why: why}
}
func absent(f float64, why string) check {
	return check{Freq: f, From: 1.5, Expect: "absent", Why: why}
}

var testRules = []string{"com.isshoni.test.fakevoice", "com.isshoni.test.faketeamspeak"}

func scenarios() []scenario {
	game := toneSpec{App: "FakeGame", Freq: fGame}
	voice := toneSpec{App: "FakeVoice", Freq: fVoice, KeptOut: true}
	ts := toneSpec{App: "FakeTS", Freq: fTS, KeptOut: true}
	lateStart := func(name, what string, flags uint32, info bool) scenario {
		return scenario{Name: name, What: what, Info: info, Mode: modeIncludeSet, Flags: flags, Apps: testRules,
			Tones: []toneSpec{game, {App: "FakeVoice", Via: "helper", Freq: fVoice, At: 3, KeptOut: true},
				{App: "FakeGame2", Freq: fGame2, At: 5, Dur: 3}},
			Duration: 9,
			Checks: []check{present(fGame, "game"), absent(fVoice, "voice app's helper started mid-share"),
				{Freq: fGame2, From: 6, To: 7.8, Expect: "present", Why: "game started mid-share"}},
			ClickWindow: [2]float64{1.5, 9}}
	}
	runtimeRule := func(name, what string, flags uint32, info bool) scenario {
		return scenario{Name: name, What: what, Info: info, Mode: modeIncludeSet, Flags: flags,
			Tones: []toneSpec{game, {App: "FakeTS", Freq: fTS, KeptOut: true}}, AddRuleAt: 4,
			AddRule: "com.isshoni.test.faketeamspeak", Duration: 8,
			Checks: []check{present(fGame, "game"),
				{Freq: fTS, From: 1.5, To: 3.8, Expect: "present", Why: "before the rule"},
				{Freq: fTS, From: 4.8, To: 8, Expect: "absent", Why: "after the rule"}},
			ClickWindow: [2]float64{1.5, 8}}
	}
	return []scenario{
		{Name: "endpoint-baseline", What: "sanity: a tap without exclusions hears every app",
			Mode: modeEndpoint, Tones: []toneSpec{game, {App: "FakeVoice", Freq: fVoice}}, Duration: 6,
			Checks: []check{present(fGame, "game"), present(fVoice, "voice app (no exclusion in this mode)")}},
		{Name: "exclude", What: "two voice apps kept out at once, game kept in",
			Mode: modeIncludeSet, Apps: testRules, Tones: []toneSpec{game, voice, ts}, Duration: 8,
			Checks:      []check{present(fGame, "game"), absent(fVoice, "voice app 1"), absent(fTS, "voice app 2")},
			ClickWindow: [2]float64{1.5, 8}},
		{Name: "helper-process", What: "voice audio played by the voice app's helper app (Electron model) is kept out",
			Mode: modeIncludeSet, Apps: testRules,
			Tones:    []toneSpec{game, {App: "FakeVoice", Via: "helper", Freq: fVoice, KeptOut: true}},
			Duration: 6, Checks: []check{present(fGame, "game"), absent(fVoice, "voice app's helper app")}},
		{Name: "helper-responsible", What: "a plain helper executable (no bundle ID) is kept out via its responsible app",
			Mode: modeIncludeSet, Apps: testRules,
			Tones:    []toneSpec{game, {App: "FakeVoice", Via: "cli", Freq: fVoice, KeptOut: true}},
			Duration: 6, Checks: []check{present(fGame, "game"), absent(fVoice, "voice app's CLI helper")}},
		{Name: "app-boundary", What: "an app opened BY a voice app (e.g. a browser from a Discord link) is still heard",
			Mode: modeIncludeSet, Apps: testRules,
			Tones:    []toneSpec{{App: "FakeVoice", Via: "open:FakeBrowser", Freq: fGame}},
			Duration: 6, Checks: []check{present(fGame, "browser opened by the voice app")}},
		{Name: "self", What: "isshoni's own playback is never re-captured",
			Mode: modeIncludeSet, Tones: []toneSpec{game}, SelfFreq: fSelf, Duration: 6,
			Checks: []check{present(fGame, "game"), absent(fSelf, "isshoni itself")}},
		{Name: "self-webview", What: "isshoni's in-app viewer (WKWebView; audio plays in WebKit's GPU process) is never re-captured",
			Mode: modeIncludeSet, Tones: []toneSpec{game}, WebFreq: fSelf, Duration: 6,
			Checks: []check{present(fGame, "game"), absent(fSelf, "isshoni's WKWebView")}},
		{Name: "late-start", What: "a voice app that starts mid-share is kept out; a game that starts mid-share is heard",
			Mode: modeIncludeSet, Apps: testRules,
			Tones: []toneSpec{game, {App: "FakeVoice", Freq: fVoice, At: 3, KeptOut: true},
				{App: "FakeGame2", Freq: fGame2, At: 5, Dur: 3}},
			Duration: 9,
			Checks: []check{present(fGame, "game"), absent(fVoice, "voice app started mid-share"),
				{Freq: fGame2, From: 6, To: 7.8, Expect: "present", Why: "game started mid-share"}},
			ClickWindow: [2]float64{1.5, 9}},
		lateStart("late-start-helper", "a voice app whose helper (never seen before) starts playing mid-share", 0, false),
		lateStart("late-start-helper-no-bundle-ids", "control: the same without CATapDescription.bundleIDs (the macOS 14–15 path)",
			flagNoBundleID, true),
		runtimeRule("runtime-rule", "adding an exclusion mid-share removes the playing app smoothly (crossfade to a new tap)", 0, false),
		runtimeRule("runtime-rule-hard-cut", "control: the same change applied to the live tap in place", flagNoXfade, true),
		{Name: "runtime-rule-removed", What: "removing an exclusion mid-share brings the playing app back smoothly",
			Mode: modeIncludeSet, Apps: testRules, Tones: []toneSpec{game, {App: "FakeTS", Freq: fTS, KeptOut: true}},
			RemoveRuleAt: 4, Duration: 8,
			Checks: []check{present(fGame, "game"),
				{Freq: fTS, From: 1.5, To: 3.8, Expect: "absent", Why: "while excluded"},
				{Freq: fTS, From: 4.8, To: 8, Expect: "present", Why: "after the rule is removed"}},
			ClickWindow: [2]float64{1.5, 8}},
		{Name: "aggregate-with-output", What: "variant: aggregate device that also contains the output device (clock source)",
			Info: true, Mode: modeIncludeSet, Flags: flagAggOutput, Apps: testRules, Tones: []toneSpec{game, voice}, Duration: 6,
			Checks: []check{present(fGame, "game"), absent(fVoice, "voice app")}},
		{Name: "mic-user", What: "an unknown app holding the microphone is kept out automatically", NeedsMic: true,
			Mode: modeIncludeSet, Flags: flagExcludeMic,
			Tones:    []toneSpec{game, {App: "FakeVoIP", Freq: fVoice, Mic: true, KeptOut: true}},
			Duration: 7, Checks: []check{present(fGame, "game"), absent(fVoice, "app using the microphone")}},
	}
}

type scenarioReport struct {
	Name      string        `json:"name"`
	What      string        `json:"what"`
	Info      bool          `json:"info"`
	Result    string        `json:"result"` // PASS | FAIL | INFO | SKIP | ERROR (inconclusive)
	Checks    []checkResult `json:"checks"`
	ClicksAtS []float64     `json:"clicks_at_s"`
	LostMs    int           `json:"audio_lost_ms,omitempty"`
	Seen      []string      `json:"apps_seen"`
	Events    []string      `json:"engine_events"`
	WebView   string        `json:"webview_audiocontext,omitempty"`
	Problems  []string      `json:"problems,omitempty"`
	Final     statusSummary `json:"final_status"`
}

type statusSummary struct {
	Aggregate   string   `json:"aggregate"`
	TapRate     float64  `json:"tap_rate"`
	BundleIDs   []string `json:"bundle_ids"`
	Updates     int      `json:"tap_updates"`
	UpdateErr   int      `json:"tap_update_errors"`
	Rebuilds    int      `json:"rebuilds"`
	Crossfades  int      `json:"crossfades"`
	XfadeAborts int      `json:"crossfade_aborts"`
	MaxUpdateMs float64  `json:"max_update_ms"`
	Overflow    int      `json:"overflow_frames"`
	Warnings    []string `json:"warnings"`
}

func (r *scenarioReport) problem(format string, args ...any) {
	r.Problems = append(r.Problems, fmt.Sprintf(format, args...))
}

// Fake-app processes are tracked through pidfiles in the work directory.
var (
	workMu  sync.Mutex
	workDir string
	pidSeq  int
)

func nextPidfile() string {
	workMu.Lock()
	defer workMu.Unlock()
	pidSeq++
	return filepath.Join(workDir, fmt.Sprintf("p%d.pid", pidSeq))
}

func killStarted() {
	workMu.Lock()
	dir := workDir
	workMu.Unlock()
	files, _ := filepath.Glob(filepath.Join(dir, "*.pid"))
	var pids []int
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 {
			pids = append(pids, pid)
			syscall.Kill(pid, syscall.SIGTERM)
		}
		os.Remove(f)
	}
	deadline := time.Now().Add(1500 * time.Millisecond)
	for _, pid := range pids {
		for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if syscall.Kill(pid, 0) == nil {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// launch starts a fake app (and whatever it starts) playing t for dur seconds.
func launch(fixtures string, t toneSpec, dur float64) error {
	app := filepath.Join(fixtures, t.App+".app")
	tone := []string{"tone", "-f", ftoa(t.Freq), "-d", ftoa(dur), "-amp", ftoa(toneAmp), "-pidfile", nextPidfile()}
	if t.Mic {
		tone = append(tone, "-mic")
	}
	args := tone
	switch {
	case t.Via == "helper":
		helper := filepath.Join(app, "Contents", "Frameworks", t.App+" Helper.app", "Contents", "MacOS", t.App+" Helper")
		args = append([]string{"spawn", "-pidfile", nextPidfile(), helper}, tone...)
	case t.Via == "cli":
		args = append([]string{"spawn", "-pidfile", nextPidfile(), filepath.Join(fixtures, "voice-cli-helper")}, tone...)
	case strings.HasPrefix(t.Via, "open:"):
		other := filepath.Join(fixtures, strings.TrimPrefix(t.Via, "open:")+".app")
		args = append([]string{"openapp", "-pidfile", nextPidfile(), "-d", ftoa(dur), other}, tone...)
	}
	return openApp(app, args...)
}

// fixturesDir finds dist/fixtures next to isshoni-s2.app (or next to a bare s2).
func fixturesDir() (dir string, bundled bool, err error) {
	exe, err := os.Executable()
	if err != nil {
		return "", false, err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	base := filepath.Dir(exe)
	if i := strings.Index(exe, ".app/Contents/MacOS/"); i >= 0 {
		base = filepath.Dir(exe[:i+4])
		bundled = true
	}
	dir = filepath.Join(base, "fixtures")
	if _, err := os.Stat(filepath.Join(dir, "FakeGame.app")); err != nil {
		return "", bundled, fmt.Errorf("fake apps not found in %s: build with ./build.sh", dir)
	}
	return dir, bundled, nil
}

func cmdSelftest(e *engine, args []string) error {
	fs := flag.NewFlagSet("selftest", flag.ExitOnError)
	keep := fs.Bool("keep", false, "keep the recorded WAVs")
	only := fs.String("only", "", "run only these scenarios (comma-separated)")
	mic := fs.Bool("mic", false, "also run mic-user (macOS asks once to let \"Fake VoIP\" use the microphone)")
	reportPath := fs.String("report", "", "write the JSON report here (default: next to the app)")
	fs.Parse(args)

	fixtures, bundled, err := fixturesDir()
	if err != nil {
		return err
	}
	if !bundled {
		fmt.Println("⚠ not running as isshoni-s2.app: macOS will attribute the audio permission to your terminal. Use ./run-selftest.sh.")
	}
	if *reportPath == "" {
		*reportPath = filepath.Join(filepath.Dir(fixtures), "s2-report.json")
	}
	probe, err := e.Probe()
	if err != nil {
		return err
	}
	fmt.Printf("isshoni S2 self-test · macOS %s · process taps: %v · bundleIDs: %v · output %q (%.0f Hz) · permission preflight %d\n",
		probe.OS, probe.ProcessTap, probe.BundleIDs, probe.Device, probe.DeviceRate, probe.TCC)
	if !probe.ProcessTap {
		return fmt.Errorf("this macOS has no process taps (needs 14.2+)")
	}
	work, err := os.MkdirTemp("", "isshoni-s2-")
	if err != nil {
		return err
	}
	workMu.Lock()
	workDir = work
	workMu.Unlock()
	defer func() {
		killStarted()
		if !*keep {
			os.RemoveAll(work)
		}
	}()
	stopOnInterrupt(e)

	warmUp(fixtures)
	if err := waitForPermission(e, fixtures); err != nil {
		return err
	}
	fmt.Println("You will hear test beeps for about 2 minutes. Please pause other apps that play sound.")

	var reports []scenarioReport
	failed, inconclusive := 0, 0
	for _, sc := range scenarios() {
		if *only != "" && !strings.Contains(","+*only+",", ","+sc.Name+",") {
			continue
		}
		fmt.Printf("\n▶ %s: %s\n", sc.Name, sc.What)
		r := scenarioReport{Name: sc.Name, What: sc.What, Info: sc.Info}
		if sc.NeedsMic && !*mic {
			r.Result = "SKIP"
			r.problem("needs -mic (macOS will ask to let \"Fake VoIP\" use the microphone)")
		} else {
			runScenario(e, sc, fixtures, work, *keep, &r)
		}
		switch r.Result {
		case "FAIL":
			failed++
		case "ERROR":
			inconclusive++
		}
		printScenario(r)
		reports = append(reports, r)
		time.Sleep(1 * time.Second)
	}

	report := map[string]any{"probe": probe, "scenarios": reports, "time": time.Now().Format(time.RFC3339)}
	b, _ := json.MarshalIndent(report, "", "  ")
	if err := writeFile(*reportPath, b); err != nil {
		return err
	}
	fmt.Printf("\n%d failed, %d inconclusive. Report: %s\n", failed, inconclusive, *reportPath)
	if *keep {
		fmt.Println("WAVs kept in", work)
	}
	if failed > 0 {
		return exitCode(1)
	}
	if inconclusive > 0 {
		return exitCode(2)
	}
	return nil
}

// warmUp launches every fake app once (silently): the first launch of a newly
// signed app is slow (code-signature checks), and must not land in a timed test.
func warmUp(fixtures string) {
	for _, app := range []string{"FakeGame", "FakeGame2", "FakeVoice", "FakeTS", "FakeBrowser", "FakeVoIP"} {
		launch(fixtures, toneSpec{App: app}, 0.3)
	}
	launch(fixtures, toneSpec{App: "FakeVoice", Via: "helper"}, 0.3)
	launch(fixtures, toneSpec{App: "FakeVoice", Via: "cli"}, 0.3)
	time.Sleep(2 * time.Second)
	killStarted()
}

// waitForPermission captures (no exclusions) while the fake game plays, until
// real audio arrives. The first time, macOS asks to allow "isshoni S2" to
// record system audio; until then the tap delivers only zeros.
func waitForPermission(e *engine, fixtures string) error {
	if err := launch(fixtures, toneSpec{App: "FakeGame", Freq: fGame}, 90); err != nil {
		return err
	}
	defer killStarted()
	e.SetRules(nil, nil)
	fmt.Println("Checking the System Audio Recording permission. If macOS asks, click Allow.")
	buf := make([]float32, chunkFrames*2)
	start := time.Now()
	for attempt := 0; time.Since(start) < 80*time.Second; attempt++ {
		if err := e.Start(modeEndpoint, 0); err != nil {
			return err
		}
		heard := 0
		for t0 := time.Now(); time.Since(t0) < 5*time.Second; {
			n, _, err := e.Read(buf, 1000)
			if err != nil {
				if isTimeout(err) {
					continue
				}
				e.Stop()
				return err
			}
			peak := 0.0
			for _, v := range buf[:n*2] {
				peak = math.Max(peak, math.Abs(float64(v)))
			}
			if peak > 0.01 {
				heard++
			}
			if heard >= 20 {
				e.Stop()
				fmt.Printf("  permission OK (audio captured after %.1f s)\n", time.Since(start).Seconds())
				return nil
			}
		}
		e.Stop()
		p, _ := e.Probe()
		fmt.Printf("  still silent after %.0f s (permission preflight %d); retrying…\n", time.Since(start).Seconds(), p.TCC)
	}
	return fmt.Errorf("no audio captured: allow isshoni S2 in System Settings → Privacy & Security → Screen & System Audio Recording → System Audio Recording Only, then run again")
}

// appSeen accumulates what the engine reported about one app during a run.
type appSeen struct {
	output, excluded, allowed bool
	reasons                   map[string]bool
}

func runScenario(e *engine, sc scenario, fixtures, work string, keep bool, r *scenarioReport) {
	var mu sync.Mutex // guards r (timed launches run in goroutines)
	defer killStarted()
	rules := append(append([]string{}, defaultVoiceApps...), sc.Apps...)
	if err := e.SetRules(rules, nil); err != nil {
		r.Result = "ERROR"
		r.problem("rules: %v", err)
		return
	}
	full := sc.Duration + 3
	for _, t := range sc.Tones {
		if t.At <= 0 {
			if err := launch(fixtures, t, orDefault(t.Dur, full)); err != nil {
				r.Result = "ERROR"
				r.problem("%v", err)
				return
			}
		}
	}
	if sc.SelfFreq > 0 {
		go playTone(sc.SelfFreq, toneAmp, full, false)
	}
	webDone := make(chan string, 1)
	if sc.WebFreq > 0 {
		go onMain(func() {
			state, err := playWebViewTone(sc.WebFreq, full)
			if err != nil {
				state = "error: " + err.Error()
			}
			webDone <- state
		})
	}
	time.Sleep(1500 * time.Millisecond) // the "already running" apps are playing before the share starts

	if err := e.Start(sc.Mode, sc.Flags); err != nil {
		r.Result = "ERROR"
		r.problem("start: %v", err)
		return
	}
	seen := map[string]*appSeen{}
	sample := func() {
		s, err := e.Status()
		if err != nil {
			return
		}
		var v statusView
		if json.Unmarshal([]byte(s), &v) != nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		for _, a := range v.Apps {
			k := a.label()
			if seen[k] == nil {
				seen[k] = &appSeen{reasons: map[string]bool{}}
			}
			x := seen[k]
			x.output = x.output || a.Output
			if a.Excluded {
				x.excluded = true
				x.reasons[a.Reason] = true
			} else if a.Output {
				x.allowed = true
			}
		}
	}
	launched := map[int]bool{}
	ruleAdded, ruleRemoved := false, false
	res, err := capture(e, sc.Duration, func(int) { go sample() }, func(t float64) {
		for i, tn := range sc.Tones {
			if tn.At > 0 && t >= tn.At && !launched[i] {
				launched[i] = true
				go func(tn toneSpec) {
					if err := launch(fixtures, tn, orDefault(tn.Dur, sc.Duration-tn.At+2)); err != nil {
						mu.Lock()
						r.problem("%v", err)
						mu.Unlock()
					}
				}(tn)
			}
		}
		if sc.AddRuleAt > 0 && t >= sc.AddRuleAt && !ruleAdded {
			ruleAdded = true
			go e.AddAppRule(sc.AddRule)
		}
		if sc.RemoveRuleAt > 0 && t >= sc.RemoveRuleAt && !ruleRemoved {
			ruleRemoved = true
			go e.SetRules(defaultVoiceApps, nil)
		}
	})
	sample()
	statusJSON, _ := e.Status()
	e.Stop()
	killStarted()
	if sc.WebFreq > 0 {
		select {
		case r.WebView = <-webDone:
		case <-time.After(time.Duration(full+5) * time.Second):
			r.WebView = "(still running)"
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if err != nil {
		r.Result = "ERROR"
		r.problem("capture: %v", err)
		return
	}
	r.LostMs = res.lostMs

	var final statusView
	json.Unmarshal([]byte(statusJSON), &final)
	r.Final = statusSummary{Aggregate: final.Aggregate, TapRate: final.TapRate, BundleIDs: final.BundleIDs,
		Updates: final.Updates, UpdateErr: final.UpdateErrors, Rebuilds: final.Rebuilds, Crossfades: final.Crossfades,
		XfadeAborts: final.XfadeAborts, MaxUpdateMs: final.MaxUpdateMs,
		Overflow: final.Overflow, Warnings: final.Warnings}
	for _, ev := range final.Events {
		r.Events = append(r.Events, fmt.Sprintf("%+.3fs %s", float64(ev.TNs-res.startNs)/1e9, redactEvent(ev.Msg)))
	}

	left := make([]float64, len(res.samples)/2)
	right := make([]float64, len(res.samples)/2)
	for i := range left {
		left[i], right[i] = float64(res.samples[2*i]), float64(res.samples[2*i+1])
	}
	if keep {
		writeWAV(filepath.Join(work, sc.Name+".wav"), res.samples)
	}
	checks, _ := evaluate(left, right, sc.Checks)
	r.Checks = checks
	if w := sc.ClickWindow; w[1] > 0 {
		r.ClicksAtS = clickEvents(segment(left, sampleRate, w[0], w[1]), segment(right, sampleRate, w[0], w[1]), sampleRate, clickThreshold)
		for i := range r.ClicksAtS {
			r.ClicksAtS[i] = math.Round((r.ClicksAtS[i]+w[0])*1000) / 1000
		}
	}

	// Every fake app must have played and been classified as expected. The
	// report names only the test's own apps (it may be shared publicly).
	others := 0
	for k, x := range seen {
		ours := false
		for w := range x.reasons {
			ours = ours || strings.Contains(w, "isshoni")
		}
		if !isTestApp(k) && !ours {
			if x.output || x.excluded {
				others++
			}
			continue
		}
		var why []string
		for w := range x.reasons {
			why = append(why, w)
		}
		sort.Strings(why)
		state := "heard"
		if x.excluded {
			state = "kept out: " + strings.Join(why, "; ")
		}
		if x.output || x.excluded {
			r.Seen = append(r.Seen, fmt.Sprintf("%s (%s)", k, state))
		}
	}
	sort.Strings(r.Seen)
	if others > 0 {
		r.Seen = append(r.Seen, fmt.Sprintf("%d other apps (not named in this report)", others))
	}
	for _, t := range sc.Tones {
		x := seen[t.player()]
		switch {
		case x == nil || !x.output:
			r.problem("%s never showed up playing audio: the test could not exercise it", t.player())
		case t.KeptOut && !x.excluded && sc.Mode != modeEndpoint:
			r.problem("%s was never classified as kept out", t.player())
		case !t.KeptOut && x.excluded:
			r.problem("%s was classified as kept out but should be heard", t.player())
		}
	}
	if sc.WebFreq > 0 && !strings.Contains(r.WebView, "running") {
		r.problem("the WKWebView's AudioContext was %q, not running: the test could not exercise it", r.WebView)
	}

	failedCheck := false
	for _, c := range checks {
		failedCheck = failedCheck || !c.Pass
	}
	switch {
	case sc.Info:
		r.Result = "INFO"
	case failedCheck || len(r.ClicksAtS) > 0:
		r.Result = "FAIL"
	case len(r.Problems) > 0:
		r.Result = "ERROR"
	default:
		r.Result = "PASS"
	}
}

// isTestApp: the self-test's fake apps and isshoni itself (incl. its WebKit
// GPU process). Anything else is the owner's own software.
func isTestApp(label string) bool {
	return strings.HasPrefix(label, "com.isshoni.") || strings.HasPrefix(label, "io.isshoni.") ||
		label == "voice-cli-helper" || label == "s2"
}

// redactEvent hides other apps' names in "keep out pid N <app> (…)" events.
func redactEvent(msg string) string {
	const p = "keep out pid "
	if !strings.HasPrefix(msg, p) {
		return msg
	}
	f := strings.SplitN(msg[len(p):], " ", 3)
	if len(f) == 3 && !isTestApp(f[1]) && !strings.Contains(f[2], "isshoni") {
		return p + "(other app) " + f[2]
	}
	return msg
}

func orDefault(v, d float64) float64 {
	if v > 0 {
		return v
	}
	return d
}

func printScenario(r scenarioReport) {
	fmt.Printf("  %s\n", r.Result)
	for _, c := range r.Checks {
		mark := "✓"
		if !c.Pass {
			mark = "✗"
		}
		leak := ""
		if c.Expect == "absent" && c.LeakMs > 0 {
			leak = fmt.Sprintf(", audible for %d ms from %.2f s", c.LeakMs, c.LeakFirst)
		}
		fmt.Printf("    %s %-7s %6.0f Hz %7.1f dB  %s%s\n", mark, c.Expect, c.Freq, c.LevelDB, c.Why, leak)
	}
	if len(r.ClicksAtS) > 0 {
		fmt.Printf("    clicks at %v s\n", r.ClicksAtS)
	}
	for _, s := range r.Seen {
		fmt.Printf("    seen: %s\n", s)
	}
	if r.WebView != "" {
		fmt.Printf("    webview AudioContext: %s\n", r.WebView)
	}
	for _, p := range r.Problems {
		fmt.Printf("    ! %s\n", p)
	}
}
