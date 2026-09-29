//go:build windows

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The self-test models the real situation with fake apps: a "game" plays a
// tone friends SHOULD hear; "voice apps" play tones they must NOT hear. Each
// fake app is a renamed copy of s1.exe, started under explorer.exe like a
// normal app. The recording is analysed per frequency (Goertzel), and the
// engine's own status is sampled every second to prove each fake app really
// played and was classified as the scenario expects (no "absent because it
// never played" passes).

type toneSpec struct {
	Exe   string  // renamed copy of s1.exe (may include a subfolder)
	Freq  float64 //
	At    float64 // start time (s) relative to recording start; <= 0 = before
	Dur   float64 // tone length (s); 0 = until after the scenario ends
	Child string  // if set, Exe launches Child, which plays the tone (helper-process model)
	Mic   bool    // also hold the microphone open
}

// player is the exe whose audio session carries the tone.
func (t toneSpec) player() string {
	if t.Child != "" {
		return strings.ToLower(filepath.Base(t.Child))
	}
	return strings.ToLower(filepath.Base(t.Exe))
}

type scenario struct {
	Name, What  string
	Mode        int
	Flags       uint32
	Apps        []string // exclusion rules on top of the built-in voice-app list
	Tones       []toneSpec
	SelfFreq    float64 // s1 itself plays this during the recording
	AddRuleAt   float64
	AddRule     string
	Duration    float64
	Checks      []check
	ClickWindow [2]float64 // assert no clicks in this window (0,0 = skip)
	NeedsMic    bool
	Info        bool // informational: never fails the run
}

// Test frequencies sit between musical notes, so background music is unlikely
// to land on the measured bins.
const (
	fGame  = 1031.0
	fGame2 = 683.0
	fVoice = 453.0
	fTS    = 571.0
	fSelf  = 907.0
)

func present(f float64, why string) check {
	return check{Freq: f, From: 1.5, Expect: "present", Why: why}
}
func absent(f float64, why string) check {
	return check{Freq: f, From: 1.5, Expect: "absent", Why: why}
}

func scenarios() []scenario {
	game := toneSpec{Exe: "game.exe", Freq: fGame}
	voice := toneSpec{Exe: "fakevoice.exe", Freq: fVoice}
	ts := toneSpec{Exe: "fakets.exe", Freq: fTS}
	return []scenario{
		{Name: "endpoint-baseline", What: "sanity: classic loopback hears every app (no exclusion)",
			Mode: modeEndpoint, Tones: []toneSpec{game, voice}, Duration: 6,
			Checks: []check{present(fGame, "game"), present(fVoice, "voice app (no exclusion in this mode)")}},
		{Name: "include-set", What: "two voice apps kept out at once, game kept in",
			Mode: modeIncludeSet, Apps: []string{"fakevoice.exe", "fakets.exe"}, Tones: []toneSpec{game, voice, ts}, Duration: 8,
			Checks:      []check{present(fGame, "game"), absent(fVoice, "voice app 1"), absent(fTS, "voice app 2")},
			ClickWindow: [2]float64{1.5, 8}},
		{Name: "exclude-one", What: "single EXCLUDE stream (fast path when only one app is excluded)",
			Mode: modeExcludeOne, Apps: []string{"fakevoice.exe"}, Tones: []toneSpec{game, voice}, Duration: 6,
			Checks: []check{present(fGame, "game"), absent(fVoice, "voice app")}},
		{Name: "exclude-one-limit", What: "why include-set exists: one EXCLUDE stream can't drop two apps", Info: true,
			Mode: modeExcludeOne, Apps: []string{"fakevoice.exe", "fakets.exe"}, Tones: []toneSpec{game, voice, ts}, Duration: 6,
			Checks: []check{present(fGame, "game"),
				{Freq: fVoice, From: 1.5, Expect: "info", Why: "voice app 1"}, {Freq: fTS, From: 1.5, Expect: "info", Why: "voice app 2"}}},
		{Name: "helper-process", What: "voice audio played by a child process of the voice app is kept out",
			Mode: modeIncludeSet, Apps: []string{"fakevoice.exe"},
			Tones:    []toneSpec{game, {Exe: "fakevoice.exe", Child: "voicehelper.exe", Freq: fVoice}},
			Duration: 6, Checks: []check{present(fGame, "game"), absent(fVoice, "voice app's helper process")}},
		{Name: "app-boundary", What: "an app launched BY a voice app (e.g. a browser opened from a Discord link) is still heard",
			Mode: modeIncludeSet, Apps: []string{"fakevoice.exe"},
			Tones: []toneSpec{{Exe: filepath.Join("voiceapp", "fakevoice.exe"),
				Child: filepath.Join("otherapp", "browser.exe"), Freq: fGame}},
			Duration: 6, Checks: []check{present(fGame, "browser started by the voice app")}},
		{Name: "self", What: "isshoni's own playback is never re-captured",
			Mode: modeIncludeSet, Tones: []toneSpec{game}, SelfFreq: fSelf, Duration: 6,
			Checks: []check{present(fGame, "game"), absent(fSelf, "isshoni itself")}},
		{Name: "late-start", What: "apps that start mid-share are classified within ~1 s; clean start/stop",
			Mode: modeIncludeSet, Apps: []string{"fakevoice.exe"},
			Tones: []toneSpec{game, {Exe: "fakevoice.exe", Freq: fVoice, At: 3},
				{Exe: "game2.exe", Freq: fGame2, At: 3, Dur: 3}},
			Duration: 9,
			Checks: []check{present(fGame, "game"), absent(fVoice, "voice app started mid-share"),
				{Freq: fGame2, From: 4.5, To: 5.8, Expect: "present", Why: "game started mid-share (picked up ≤1.5 s)"}},
			ClickWindow: [2]float64{1.5, 9}},
		{Name: "late-start-endpoint", Info: true,
			What: "control for late-start: same timeline through plain Windows loopback (no isshoni mixing); clicks here come from Windows/the test apps, not isshoni",
			Mode: modeEndpoint,
			Tones: []toneSpec{game, {Exe: "fakevoice.exe", Freq: fVoice, At: 3},
				{Exe: "game2.exe", Freq: fGame2, At: 3, Dur: 3}},
			Duration: 9,
			Checks: []check{present(fGame, "game"),
				{Freq: fGame2, From: 4.5, To: 5.8, Expect: "present", Why: "game started mid-share"}},
			ClickWindow: [2]float64{1.5, 9}},
		{Name: "runtime-rule", What: "adding an exclusion mid-share fades the app out without a click",
			Mode: modeIncludeSet, Tones: []toneSpec{game, voice}, AddRuleAt: 4, AddRule: "fakevoice.exe", Duration: 8,
			Checks: []check{present(fGame, "game"),
				{Freq: fVoice, From: 1.5, To: 3.8, Expect: "present", Why: "before the rule"},
				{Freq: fVoice, From: 5, To: 8, Expect: "absent", Why: "after the rule"}},
			ClickWindow: [2]float64{1.5, 8}},
		{Name: "mic-user", What: "an unknown app holding the microphone is kept out automatically", NeedsMic: true,
			Mode: modeIncludeSet, Flags: flagExcludeMic,
			Tones:    []toneSpec{game, {Exe: "fakevoip.exe", Freq: fVoice, Mic: true}},
			Duration: 7, Checks: []check{present(fGame, "game"), absent(fVoice, "app using the microphone")}},
	}
}

type scenarioReport struct {
	Name        string          `json:"name"`
	What        string          `json:"what"`
	Info        bool            `json:"info"`
	Result      string          `json:"result"` // PASS | FAIL | INFO | SKIP | ERROR (inconclusive)
	Checks      []checkResult   `json:"checks"`
	ClicksAtS   []float64       `json:"clicks_at_s"`
	Clicks      []clickDetail   `json:"click_details,omitempty"`
	Reparented  bool            `json:"tone_apps_under_explorer"`
	LostMs      int             `json:"audio_lost_ms,omitempty"`
	Foreign     []string        `json:"other_apps_heard,omitempty"`
	Problems    []string        `json:"problems,omitempty"`
	FinalStatus json.RawMessage `json:"final_status,omitempty"`
}

// clickDetail describes one detected click: the biggest sample-to-sample jump
// near it, a short waveform snippet (left channel), and engine events nearby.
type clickDetail struct {
	AtS     float64   `json:"at_s"`
	Step    float64   `json:"step"`
	Snippet []float64 `json:"snippet"`
	Events  []string  `json:"engine_events_nearby"`
}

func (r *scenarioReport) problem(format string, args ...any) {
	r.Problems = append(r.Problems, fmt.Sprintf(format, args...))
}

func cmdSelftest(e *engine, args []string) error {
	fs := flag.NewFlagSet("selftest", flag.ExitOnError)
	keep := fs.Bool("keep", false, "keep the recorded WAVs")
	only := fs.String("only", "", "run only these scenarios (comma-separated)")
	fs.Parse(args)

	probe, err := e.Probe()
	if err != nil {
		return err
	}
	stopOnInterrupt(e)
	fmt.Printf("isshoni S1 self-test · Windows build %d · per-app loopback: %v (format %s) · output %q · volume %.0f%%\n",
		probe.OSBuild, probe.ProcessLoopback, probe.Format, probe.Device, probe.MasterVolume*100)
	if probe.Error != "" {
		fmt.Println("  probe notes:", probe.Error)
	}
	if probe.MasterMuted || (probe.MasterVolume >= 0 && probe.MasterVolume < 0.3) {
		fmt.Println("  ⚠ Windows volume is muted or below 30% — raise it (turn the speakers down instead) for reliable levels.")
	}
	fmt.Println("You will hear test beeps for about 2 minutes. Please close/pause other apps that play sound.")
	work, err := os.MkdirTemp("", "isshoni-s1-")
	if err != nil {
		return err
	}
	defer removeAllRetry(work, *keep)
	self, _ := os.Executable()
	micOK := false
	if closeMic, err := openMic(); err == nil {
		closeMic()
		micOK = true
	}

	var reports []scenarioReport
	failed, inconclusive := 0, 0
	for _, sc := range scenarios() {
		if *only != "" && !strings.Contains(","+*only+",", ","+sc.Name+",") {
			continue
		}
		fmt.Printf("\n▶ %s: %s\n", sc.Name, sc.What)
		r := scenarioReport{Name: sc.Name, What: sc.What, Info: sc.Info}
		switch {
		case sc.NeedsMic && !micOK:
			r.Result = "SKIP"
			r.problem("no usable microphone (or mic access for desktop apps is off)")
		case !probe.ProcessLoopback && sc.Mode != modeEndpoint:
			r.Result = "SKIP"
			r.problem("per-app loopback unavailable on this build")
		default:
			runScenario(e, sc, self, work, *keep, &r)
		}
		switch r.Result {
		case "FAIL":
			failed++
		case "ERROR":
			inconclusive++
		}
		printScenario(r)
		reports = append(reports, r)
		time.Sleep(1500 * time.Millisecond) // let the previous apps' audio sessions expire
	}

	var lateClicks, controlClicks []float64
	for _, r := range reports {
		switch r.Name {
		case "late-start":
			lateClicks = r.ClicksAtS
		case "late-start-endpoint":
			controlClicks = r.ClicksAtS
		}
	}
	if len(lateClicks) > 0 && len(controlClicks) > 0 {
		fmt.Printf("\nnote: the late-start clicks %v also appear through plain Windows loopback %v: they come from Windows or the test apps, not from isshoni's mixer.\n", lateClicks, controlClicks)
	} else if len(lateClicks) > 0 {
		fmt.Printf("\nnote: the late-start clicks %v do NOT appear through plain Windows loopback: they come from isshoni's capture/mixing (see click_details in the report).\n", lateClicks)
	}

	report := map[string]any{"probe": probe, "scenarios": reports, "time": time.Now().Format(time.RFC3339)}
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		b = []byte(fmt.Sprintf(`{"error": %q}`, err.Error()))
	}
	reportPath := filepath.Join(filepath.Dir(self), "s1-report.json")
	if err := os.WriteFile(reportPath, b, 0o644); err != nil {
		return err
	}
	fmt.Printf("\n%d failed, %d inconclusive. Report: %s — please send this file back.\n", failed, inconclusive, reportPath)
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

// Fake-app processes started by the self-test, so Ctrl+C can clean them up.
var (
	startedMu sync.Mutex
	started   []*os.Process
)

func killStarted() {
	startedMu.Lock()
	defer startedMu.Unlock()
	for _, p := range started {
		exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(p.Pid)).Run()
	}
	started = nil
}

// sessionSeen accumulates what the engine reported about one exe during a run.
type sessionSeen struct {
	active, excluded, allowed bool
	reasons                   map[string]bool
}

func runScenario(e *engine, sc scenario, self, work string, keep bool, r *scenarioReport) {
	var mu sync.Mutex // guards r and exits (mid-share launches run in goroutines)
	r.Reparented = true
	type exitInfo struct {
		at   time.Time
		code int
	}
	exits := map[string]exitInfo{}
	defer func() {
		killStarted()
		stopSound()
	}()

	// Copy every fake app up front: a first-run antivirus scan of a fresh exe
	// must not happen inside the timed recording.
	exes := map[string]string{}
	for _, t := range sc.Tones {
		for _, name := range []string{t.Exe, t.Child} {
			if name == "" || exes[name] != "" {
				continue
			}
			path, err := copyAs(self, work, name)
			if err != nil {
				r.Result = "ERROR"
				r.problem("copy %s: %v", name, err)
				return
			}
			exes[name] = path
		}
	}
	recStart := time.Now().Add(time.Second) // recording starts after the 1 s settle below
	startTone := func(t toneSpec) {
		d := t.Dur
		if d == 0 {
			d = sc.Duration + 3
		}
		args := []string{"tone", "-f", fmt.Sprint(t.Freq), "-d", fmt.Sprint(d)}
		if t.Mic {
			args = append(args, "-mic")
		}
		if t.Child != "" {
			args = append([]string{"spawn", exes[t.Child]}, args...)
		}
		p, reparented, err := startUnderExplorer(exes[t.Exe], args...)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			r.problem("could not start %s: %v", t.Exe, err)
			return
		}
		r.Reparented = r.Reparented && reparented
		startedMu.Lock()
		started = append(started, p)
		startedMu.Unlock()
		go func() {
			st, _ := p.Wait()
			code := -1
			if st != nil {
				code = st.ExitCode()
			}
			mu.Lock()
			exits[t.player()] = exitInfo{time.Now(), code}
			mu.Unlock()
		}()
	}

	for _, t := range sc.Tones {
		if t.At <= 0 {
			startTone(t)
		}
	}
	if sc.SelfFreq > 0 {
		if err := playTone(sc.SelfFreq, 0.25, sc.Duration+2); err != nil {
			r.problem("s1 could not play its own tone: %v", err)
		}
	}
	time.Sleep(time.Until(recStart)) // let the audio sessions appear
	if err := e.SetRules(append(append([]string{}, defaultVoiceApps...), sc.Apps...), nil); err != nil {
		r.Result = "ERROR"
		r.problem("%v", err)
		return
	}
	if err := e.Start(sc.Mode, sc.Flags); err != nil {
		r.Result = "ERROR"
		r.problem("%v", err)
		return
	}

	fakes := map[string]bool{strings.ToLower(filepath.Base(self)): true}
	for _, t := range sc.Tones {
		fakes[strings.ToLower(filepath.Base(t.Exe))] = true
		fakes[t.player()] = true
	}
	seen := map[string]*sessionSeen{}
	foreign := map[string]bool{}
	sample := func() {
		st, err := e.Status()
		if err != nil {
			return
		}
		var v statusView
		if json.Unmarshal([]byte(st), &v) != nil {
			return
		}
		for _, x := range v.Sessions {
			exe := strings.ToLower(x.Exe)
			s := seen[exe]
			if s == nil {
				s = &sessionSeen{reasons: map[string]bool{}}
				seen[exe] = s
			}
			if x.State == "active" {
				s.active = true
				if x.Excluded {
					s.excluded = true
					s.reasons[x.Reason] = true
				} else {
					s.allowed = true
				}
			}
		}
		for _, st := range v.Streams {
			if st.Kind == "include" && st.MaxPeak > 1e-3 && !fakes[strings.ToLower(st.Label)] {
				foreign[st.Label] = true // another app really made sound in the recording
			}
		}
	}
	fired := map[int]bool{}
	ruleAdded := false
	rec, err := capture(e, sc.Duration, func(int) { sample() }, func(t float64) {
		for i, tone := range sc.Tones {
			if tone.At > 0 && !fired[i] && t >= tone.At {
				fired[i] = true
				go startTone(tone) // never block the read loop
			}
		}
		if sc.AddRule != "" && !ruleAdded && t >= sc.AddRuleAt {
			ruleAdded = true
			go e.AddAppRule(sc.AddRule)
		}
	})
	captureEnd := time.Now()
	sample()
	if st, err := e.Status(); err == nil {
		r.FinalStatus = json.RawMessage(st)
	}
	e.Stop()
	mu.Lock()
	defer mu.Unlock()
	samples, lostMs := rec.samples, rec.lostMs
	r.LostMs = lostMs
	if err != nil {
		r.Result = "ERROR"
		r.problem("%v", err)
		return
	}
	if keep {
		writeWAV(filepath.Join(work, sc.Name+".wav"), samples)
	}

	left, right := make([]float64, len(samples)/2), make([]float64, len(samples)/2)
	for i := range left {
		left[i], right[i] = float64(samples[2*i]), float64(samples[2*i+1])
	}
	r.Checks, _ = evaluate(left, right, sc.Checks)
	if w := sc.ClickWindow; w[1] > 0 {
		for _, t := range clickEvents(segment(left, sampleRate, w[0], w[1]), segment(right, sampleRate, w[0], w[1]), sampleRate, clickThreshold) {
			at := t + w[0]
			r.ClicksAtS = append(r.ClicksAtS, math.Round(at*100)/100)
			r.Clicks = append(r.Clicks, describeClick(left, at, rec.startNs, r.FinalStatus))
		}
	}
	r.Result = "PASS"
	for _, c := range r.Checks {
		if !c.Pass {
			r.Result = "FAIL"
		}
	}
	if len(r.ClicksAtS) > 0 {
		r.Result = "FAIL"
	}

	// Positive controls: every fake app must have played, and been classified
	// as the check for its frequency assumes.
	expect := map[float64]string{}
	for _, c := range sc.Checks {
		if c.Expect == "absent" || expect[c.Freq] == "" {
			expect[c.Freq] = c.Expect
		}
	}
	inconclusive := false
	checkApp := func(exe string, freq float64, plannedEnd time.Time) {
		s := seen[exe]
		if ex, ok := exits[exe]; ok && ex.at.Before(plannedEnd.Add(-500*time.Millisecond)) && ex.at.Before(captureEnd) {
			r.problem("%s exited early (code %d) — it may never have played", exe, ex.code)
			inconclusive = true
		}
		if s == nil || !s.active {
			r.problem("%s never showed up as a playing app", exe)
			inconclusive = true
			return
		}
		if sc.Mode == modeEndpoint || sc.AddRule != "" || sc.Info {
			return // classification doesn't matter here / changes mid-run
		}
		switch expect[freq] {
		case "absent":
			if !s.excluded || s.allowed {
				r.problem("%s was not consistently classified as excluded (excluded=%v allowed=%v)", exe, s.excluded, s.allowed)
				r.Result = "FAIL"
			}
		case "present":
			if s.excluded {
				r.problem("%s was wrongly excluded (%s)", exe, strings.Join(keys(s.reasons), "; "))
				r.Result = "FAIL"
			}
		}
	}
	for _, t := range sc.Tones {
		end := captureEnd.Add(time.Hour)
		if t.Dur > 0 {
			end = recStart.Add(time.Duration((math.Max(t.At, 0) + t.Dur) * float64(time.Second)))
		}
		checkApp(t.player(), t.Freq, end)
	}
	if sc.SelfFreq > 0 {
		checkApp(strings.ToLower(filepath.Base(self)), sc.SelfFreq, captureEnd.Add(time.Hour))
	}
	for f := range foreign {
		r.Foreign = append(r.Foreign, f)
	}
	sort.Strings(r.Foreign)

	switch {
	case r.Result == "FAIL" && lostMs > 0:
		r.Result = "ERROR"
		r.problem("this test program stalled and %d ms of audio were lost; re-run with -only %s", lostMs, sc.Name)
	case r.Result == "FAIL" && len(r.Foreign) > 0:
		r.Result = "ERROR"
		r.problem("other apps were playing and were captured (%s); close them and re-run with -only %s", strings.Join(r.Foreign, ", "), sc.Name)
	case !r.Reparented:
		r.Result = "ERROR"
		r.problem("test apps could not be started under explorer.exe, so they count as part of s1 ('isshoni itself')")
	case inconclusive && r.Result != "FAIL":
		r.Result = "ERROR"
	}
	if sc.Info {
		r.Result = "INFO"
	}
}

func describeClick(ch []float64, at float64, startNs int64, status json.RawMessage) clickDetail {
	d := clickDetail{AtS: math.Round(at*1000) / 1000}
	c := int(at * sampleRate)
	for i := max(c-24, 1); i < min(c+24, len(ch)); i++ {
		d.Snippet = append(d.Snippet, math.Round(ch[i]*10000)/10000)
		d.Step = math.Max(d.Step, math.Round(math.Abs(ch[i]-ch[i-1])*10000)/10000)
	}
	var v statusView
	if json.Unmarshal(status, &v) == nil {
		for _, ev := range v.Events {
			t := float64(ev.TNs-startNs) / 1e9
			if math.Abs(t-at) <= 0.3 {
				d.Events = append(d.Events, fmt.Sprintf("%+.3fs %s: %s", t-at, ev.Label, ev.Event))
			}
		}
	}
	return d
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func printScenario(r scenarioReport) {
	label := map[string]string{"ERROR": "INCONCLUSIVE"}[r.Result]
	if label == "" {
		label = r.Result
	}
	fmt.Printf("  %s\n", label)
	for _, c := range r.Checks {
		mark := "✓"
		if !c.Pass {
			mark = "✗"
		}
		if c.Expect == "info" {
			mark = "·"
		}
		fmt.Printf("    %s %g Hz %s %.1f dB (%s)\n", mark, c.Freq, c.Expect, c.LevelDB, c.Why)
	}
	if len(r.ClicksAtS) > 0 {
		mark := "✗"
		if r.Result == "INFO" {
			mark = "·"
		}
		fmt.Printf("    %s clicks at %v s\n", mark, r.ClicksAtS)
		for _, c := range r.Clicks {
			ev := "no isshoni engine event nearby"
			if len(c.Events) > 0 {
				ev = strings.Join(c.Events, " | ")
			}
			fmt.Printf("      %.3fs step %.3f — %s\n", c.AtS, c.Step, ev)
		}
	}
	var v statusView
	if len(r.FinalStatus) > 0 && json.Unmarshal(r.FinalStatus, &v) == nil {
		t := v.Totals
		if t.Discontinuities+t.Overflows+t.Underruns+t.Trims+t.DroppedChunks+t.MixerResyncs > 0 {
			fmt.Printf("    engine: discontinuities %d · overflows %d · underruns %d · trims %d · dropped %d · resyncs %d\n",
				t.Discontinuities, t.Overflows, t.Underruns, t.Trims, t.DroppedChunks, t.MixerResyncs)
		}
	}
	for _, p := range r.Problems {
		fmt.Printf("    ! %s\n", p)
	}
}

// copyAs copies s1.exe to work/name (fake apps are identified by exe name).
func copyAs(src, work, name string) (string, error) {
	dst := filepath.Join(work, name)
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return "", err
	}
	return dst, out.Close()
}

// removeAllRetry deletes the work folder; killed fake apps may keep their exe
// mapped for a moment, so retry briefly.
func removeAllRetry(dir string, keep bool) {
	if keep {
		return
	}
	for i := 0; i < 10; i++ {
		if os.RemoveAll(dir) == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}
