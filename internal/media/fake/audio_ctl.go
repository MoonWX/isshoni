package fake

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// This file is 05's (docs/m2/05-windows-audio.md §13.3): *Engine's media.AudioController with scripted scenarios,
// so 03 builds the share sheet on the Mac and internal/desktop/audio is tested without Windows.
//
// A scenario (EngineConfig.AudioScenario) scripts what the native watcher would find on a PC: the apps with their
// facts (exe, pids, sessions, playing, mic use, the shared app, the level over time) and when each appears, the
// probe and the endpoints. Verdicts are not scripted: every report classifies the apps against the current
// Exclusions with a small port of 05 §5.3's precedence (classify), so the Exclusions of WatchAudio, SetExclusions and
// a Start with audio change the verdicts as they would natively. With the Exclusions the desktop app builds (the
// built-in voice apps, the browsers, "Keep out apps that use my microphone"), the scenarios show what 05 §13.3's
// table says.
//
// The script's clock is the watcher's: it starts when the watcher starts (WatchAudio, or a session with audio while
// not watching) and stops when neither runs. Time is time.Now, so tests drive the scripts with testing/synctest.
// Without a watcher, AudioReport is a one-shot snapshot of the script's start (state "stopped", engine "none") and
// AudioLevels is ErrAudioNotRunning, as natively.

var _ media.AudioController = (*Engine)(nil)

// The scenarios' times and the native limits the fake follows (05 §4.1, §6.2, §11.3, §12.1, §12.2).
const (
	chromeMicAfter    = 5 * time.Second         // normal, degrade-live: Chrome starts using the mic
	discordAfter      = 8 * time.Second         // fast-path: Discord starts
	degradeAfter      = 5 * time.Second         // degrade-live: after each session with audio starts
	soundCheckAfter   = 1500 * time.Millisecond // SoundCheck's answer
	minSoundCheck     = time.Second             // im_audio_sound_check's timeout_ms: 1000..10000
	maxSoundCheck     = 10 * time.Second
	maxIncludeStreams = 48 // the planner's cap (05 §6.2 step 4); the rest are not captured ("limit")
	maxReportEvents   = 300
	maxLevelApps      = 64
	maxRuleValue      = 520 // IM_MAX_RULE_VALUE: bytes with the NUL
	silenceDB         = -120.0
	rmsBelowPeakDB    = 3.0                // a captured level's RMS: a sine's peak minus 3 dB
	filetimeBase      = 133725183442731234 // the creation FILETIME of the scripted processes, plus 1 s per pid
)

// The report's values (05 §12.1).
const (
	verdictHeard       = "heard"
	verdictKeptOut     = "kept_out"
	verdictNotCaptured = "not_captured"

	engineIncludeSet  = "include-set"
	engineExcludeSelf = "exclude-self"
	engineEndpoint    = "endpoint"
	engineNone        = "none"

	stateStopped = "stopped"
)

// scriptApp is one app of a scenario: what the native watcher would find. Its verdict is derived (classify).
type scriptApp struct {
	exe, name string // the row id without a rule is "exe:" + exe; name is the exe's FileDescription
	pid       uint32 // the first pid of its tree; its sessions' pids follow 4 apart
	pids      int    // the processes of its tree
	sessions  int    // its render sessions; 0 = a running process without one
	playing   bool   // a session active within the last 2 s
	usesMic   bool   // a mic root (05 §5.6)
	shared    bool   // the shared app (05 §5.8) whatever Exclusions.SharedPID says
	// from is when it appears, after the watcher started (0: from the start).
	from time.Duration
	// level is its activity level in dBFS at script time t while it plays; nil is −120 (silence).
	level func(t time.Duration) float64
}

// hasPID reports whether pid is one of a's processes.
func (a scriptApp) hasPID(pid uint64) bool {
	return pid >= uint64(a.pid) && pid < uint64(a.pid)+uint64(4*max(a.pids, 1)) && (pid-uint64(a.pid))%4 == 0
}

// audioScenario is one of 05 §13.3's scenarios.
type audioScenario struct {
	apps        []scriptApp
	probe       media.AudioProbeResult
	device      media.AudioDeviceState
	endpoint    bool // the probe failed or is unsupported: the endpoint engine, everything heard (05 §6.1)
	noDevice    bool // no endpoint: state no-device, engine none
	degradeLive bool // process loopback fails 5 s into each session with audio (05 §6.5)
	soundCheck  media.SoundCheckResult
}

var (
	probeOK     = media.AudioProbeResult{Result: "ok", OSBuild: 26100, Format: "float32", LevelDB: -60.1, Millis: 231}
	probeFailed = media.AudioProbeResult{Result: "failed", OSBuild: 26100, Format: "float32", LevelDB: -97.3,
		Millis: 231}
	deviceOK = media.AudioDeviceState{Present: true, RenderEndpoints: 2, CaptureEndpoints: 1}

	soundCheckLoopback = media.SoundCheckResult{Captured: true, LevelDB: -14, Millis: 1500, Backend: "process-loopback"}
	soundCheckEndpoint = media.SoundCheckResult{Captured: true, LevelDB: -14, Millis: 1500, Backend: "endpoint"}
	soundCheckNoDevice = media.SoundCheckResult{Captured: false, Reason: "no_device", Backend: "none"}
)

// spotifyLevel is −18 dB ± 6 dB, a sine at 0.5 Hz.
func spotifyLevel(t time.Duration) float64 {
	return -18 + 6*math.Sin(math.Pi*t.Seconds())
}

// steadyLevel is a constant level.
func steadyLevel(db float64) func(time.Duration) float64 {
	return func(time.Duration) float64 { return db }
}

// The scripted apps.
func spotify() scriptApp {
	return scriptApp{exe: "spotify.exe", name: "Spotify", pid: 4120, pids: 4, sessions: 1, playing: true,
		level: spotifyLevel}
}

func discord() scriptApp {
	return scriptApp{exe: "discord.exe", name: "Discord", pid: 6604, pids: 6, sessions: 2, playing: true, usesMic: true,
		level: steadyLevel(-24)}
}

func steam() scriptApp { // a mic session, silent
	return scriptApp{exe: "steam.exe", name: "Steam", pid: 2280, pids: 3, sessions: 1, usesMic: true}
}

func chrome() scriptApp { // a call in the browser (Discord Web, Meet)
	return scriptApp{exe: "chrome.exe", name: "Google Chrome", pid: 11240, pids: 14, sessions: 1, playing: true,
		usesMic: true, from: chromeMicAfter, level: steadyLevel(-30)}
}

// silentRow is a, running without a session: no endpoint, so nothing plays and nothing uses a mic.
func silentRow(a scriptApp) scriptApp {
	a.sessions, a.playing, a.usesMic, a.level = 0, false, false, nil
	return a
}

// audioScenarios are 05 §13.3's scenarios by name.
var audioScenarios = map[string]func() *audioScenario{
	// t0: Spotify heard, Discord kept out (built-in), Steam kept out (mic, silent); t5: Chrome starts using the mic.
	"normal": func() *audioScenario {
		return &audioScenario{apps: []scriptApp{spotify(), discord(), steam(), chrome()}, probe: probeOK,
			device: deviceOK, soundCheck: soundCheckLoopback}
	},
	// The probe failed: engine endpoint, everything heard, Discord included.
	"degraded": func() *audioScenario {
		return &audioScenario{apps: []scriptApp{spotify(), discord(), steam()}, probe: probeFailed, device: deviceOK,
			endpoint: true, soundCheck: soundCheckEndpoint}
	},
	// As normal until 5 s after a session with audio starts; then process loopback fails twice in 10 s.
	"degrade-live": func() *audioScenario {
		return &audioScenario{apps: []scriptApp{spotify(), discord(), steam(), chrome()}, probe: probeOK,
			device: deviceOK, degradeLive: true, soundCheck: soundCheckLoopback}
	},
	// Process loopback is unsupported (Windows 10 1909): engine endpoint, everything heard.
	"old-windows": func() *audioScenario {
		return &audioScenario{apps: []scriptApp{spotify(), discord(), steam()},
			probe:  media.AudioProbeResult{Result: "unsupported", OSBuild: 18363, LevelDB: silenceDB},
			device: deviceOK, endpoint: true, soundCheck: soundCheckEndpoint}
	},
	// No endpoint: nothing plays; only the running built-in voice app has a row (05 §12.1's row rule).
	"no-device": func() *audioScenario {
		return &audioScenario{apps: []scriptApp{silentRow(spotify()), silentRow(discord()), silentRow(steam())},
			probe: media.AudioProbeResult{Result: "skipped", OSBuild: 26100, LevelDB: silenceDB}, noDevice: true,
			soundCheck: soundCheckNoDevice}
	},
	// Only Spotify and a game: the fast path; t8: Discord starts, so the include set takes over.
	"fast-path": func() *audioScenario {
		game := scriptApp{exe: "eldenring.exe", name: "ELDEN RING", pid: 9932, pids: 1, sessions: 1, playing: true,
			level: steadyLevel(-20)}
		d := discord()
		d.from = discordAfter
		return &audioScenario{apps: []scriptApp{spotify(), game, d}, probe: probeOK, device: deviceOK,
			soundCheck: soundCheckLoopback}
	},
	// The shared game uses the mic and is heard, with shared_app_uses_mic.
	"shared-mic": func() *audioScenario {
		game := scriptApp{exe: "valorant.exe", name: "VALORANT", pid: 8812, pids: 3, sessions: 1, playing: true,
			usesMic: true, shared: true, level: steadyLevel(-16)}
		return &audioScenario{apps: []scriptApp{game, spotify(), discord()}, probe: probeOK, device: deviceOK,
			soundCheck: soundCheckLoopback}
	},
	// 60 apps: the share sheet's scrolling. Every ninth uses the mic, every thirteenth is silent. The apps are 16 pids
	// apart, so an app's up to 4 processes (4 apart) never reach the next app's first pid.
	"many": func() *audioScenario {
		apps := []scriptApp{spotify(), discord(), steam()}
		for i := 1; len(apps) < 60; i++ {
			apps = append(apps, scriptApp{
				exe: fmt.Sprintf("app%02d.exe", i), name: fmt.Sprintf("App %02d", i), pid: 20000 + 16*uint32(i),
				pids: 1 + i%4, sessions: 1, playing: i%13 != 0, usesMic: i%9 == 0, level: steadyLevel(-20 - float64(i%20)),
			})
		}
		return &audioScenario{apps: apps, probe: probeOK, device: deviceOK, soundCheck: soundCheckLoopback}
	},
}

// audioState is the AudioController's script and state, guarded by its own mutex. Lock order: audioState.mu, then a
// Session's mu, then Engine.mu; no Session method takes audioState.mu. A Start with audio holds it while it creates
// the session, so the session is never running without the controller knowing it.
type audioState struct {
	name    string
	sc      *audioScenario // nil: an unknown scenario
	created time.Time      // the engine clock's 0 (QPCNanos, AudioEvent.TNanos)

	mu       sync.Mutex
	ex       media.Exclusions // the rule set: the last WatchAudio, SetExclusions or Start with audio
	watching bool
	runSince time.Time          // when the watcher started; meaningful while it runs
	sess     *Session           // the last session with audio
	override *media.AudioReport // SetAudioStatus
	seq      uint32
	started  bool              // prev is set
	at       time.Time         // the instant prev describes
	prev     media.AudioReport // the report at `at`, without Seq, QPCNanos and Events
	prevApps []scriptApp       // the scripted app of each of prev's rows
	events   []media.AudioEvent
}

func (a *audioState) init(name string) {
	a.name, a.created = name, time.Now()
	if name == "" {
		name = "normal"
	}
	if mk, ok := audioScenarios[name]; ok {
		a.sc = mk()
	}
}

// info returns the last audio session's audioInfo; ok is false before the first.
func (a *audioState) info() (info audioInfo, ok bool) {
	if a.sess == nil {
		return audioInfo{}, false
	}
	return a.sess.audioInfo(), true
}

// clock is t on the engine clock.
func (a *audioState) clock(t time.Time) int64 {
	return t.Sub(a.created).Nanoseconds()
}

// advanceLocked brings the report up to now: it evaluates the script at every instant since the last one where
// something happened (an app appears, the session with audio ends, degrade-live's failure), so the events carry
// those instants, and then at now.
func (a *audioState) advanceLocked(now time.Time) {
	info, hasSess := a.info()
	if a.started {
		var ts []time.Time
		add := func(t time.Time) {
			if t.After(a.at) && t.Before(now) {
				ts = append(ts, t)
			}
		}
		for _, app := range a.sc.apps {
			if app.from > 0 {
				add(a.runSince.Add(app.from))
			}
		}
		if hasSess {
			add(info.start)
			if a.sc.degradeLive {
				add(info.start.Add(degradeAfter))
			}
			if !info.end.IsZero() {
				add(info.end)
			}
		}
		slices.SortFunc(ts, time.Time.Compare)
		ts = slices.CompactFunc(ts, time.Time.Equal)
		for _, t := range ts {
			a.stepLocked(t, info, hasSess)
		}
	}
	a.stepLocked(now, info, hasSess)
}

// stepLocked evaluates the script at t: Seq goes up when the report changed (anything but the counters), and the
// change becomes events.
func (a *audioState) stepLocked(t time.Time, info audioInfo, hasSess bool) {
	cur, apps := a.stateAt(t, info, hasSess)
	prev := a.prev
	if !a.started {
		prev = media.AudioReport{State: stateStopped}
	}
	if !a.started || !sameReport(prev, cur) {
		a.seq++
	}
	a.addEvents(t, prev, cur)
	a.prev, a.prevApps, a.at, a.started = cur, apps, t, true
}

// sameReport reports whether x and y differ only in what Seq ignores (05 §12.1): counters, qpcNs, events.
func sameReport(x, y media.AudioReport) bool {
	x.Counters, y.Counters = media.AudioCounters{}, media.AudioCounters{}
	return reflect.DeepEqual(x, y)
}

// stateAt is the scripted report at t (without Seq, QPCNanos and Events) and the scripted app of each row.
func (a *audioState) stateAt(t time.Time, info audioInfo, hasSess bool) (media.AudioReport, []scriptApp) {
	sc := a.sc
	live := hasSess && info.liveAt(t)
	running := a.watching || live
	var st time.Duration // the script's time
	if running {
		st = t.Sub(a.runSince)
	}
	failAt := info.start.Add(degradeAfter) // degrade-live's failure, if the session still ran then
	failed := sc.degradeLive && hasSess && !t.Before(failAt) && (info.end.IsZero() || !info.end.Before(failAt))
	endpoint := sc.endpoint || failed
	off := live && info.off

	r := media.AudioReport{
		V: 1, Mode: media.AudioExcludeApps, Live: live, Probe: sc.probe, Device: sc.device,
		Apps: []media.AudioApp{}, Sessions: []media.AudioSession{}, Warnings: []media.AudioWarning{},
	}
	if failed {
		r.Probe = probeFailed
	}
	if live {
		r.Muted, r.Paused = info.muted, info.paused
	}
	if off {
		r.Mode = media.AudioOff
	}
	switch {
	case !running:
		r.State = stateStopped
	case sc.noDevice:
		r.State = "no-device"
	case endpoint:
		r.State = "degraded"
	default:
		r.State = "ok"
	}

	var rows []classified
	keptOut := false
	for _, app := range sc.apps {
		if app.from > st {
			continue
		}
		c := classify(app, a.ex, endpoint)
		if c.row.Sessions == 0 && !c.builtin && !c.row.Shared {
			continue // a row is an app with a session, a running built-in voice app or the shared app
		}
		keptOut = keptOut || c.row.Verdict == verdictKeptOut
		rows = append(rows, c)
	}
	switch {
	case !running || sc.noDevice || off:
		r.Engine = engineNone
	case endpoint:
		r.Engine = engineEndpoint
	case a.ex.FastPath && !keptOut:
		r.Engine = engineExcludeSelf
	default:
		r.Engine = engineIncludeSet
	}

	capture := live && r.Engine != engineNone
	apps := make([]scriptApp, 0, len(rows))
	for _, c := range rows {
		row := c.row
		if row.Verdict == verdictHeard && r.Engine == engineIncludeSet {
			switch {
			case c.launcher:
				row.Verdict, row.Reason = verdictNotCaptured, "launcher"
			case capture && row.Playing && r.Counters.Streams < maxIncludeStreams:
				row.Streams = 1
				r.Counters.Streams++
			case capture && row.Playing:
				row.Verdict, row.Reason = verdictNotCaptured, "limit"
			}
		}
		switch {
		case row.Shared && row.UsesMic && row.Verdict == verdictHeard:
			row.Warnings = append(row.Warnings, "shared_app_uses_mic")
		case row.Shared && row.Verdict == verdictKeptOut && row.Reason != "mic":
			row.Warnings = append(row.Warnings, "shared_app_kept_out")
		case row.UsesMic && row.Verdict == verdictHeard && !endpoint && (c.allowed || !a.ex.KeepOutMicUsers):
			row.Warnings = append(row.Warnings, "included_uses_mic")
		}
		r.Apps = append(r.Apps, row)
		apps = append(apps, c.app)
		for j := range row.Sessions {
			pid := c.app.pid + 4*uint32(j)
			s := media.AudioSession{
				PID: pid, Created: strconv.FormatUint(filetimeBase+uint64(pid)*10_000_000, 10), Exe: row.Exe,
				AppID: row.ID, Flow: "render", State: "inactive", Verdict: row.Verdict, Reason: row.Reason,
				Stream: "none", Format: "float32",
			}
			if row.Playing && j == 0 {
				s.State = "active"
			}
			if row.Streams > 0 && j == 0 {
				s.Stream = "running"
			}
			r.Sessions = append(r.Sessions, s)
		}
	}
	if capture && r.Engine != engineIncludeSet {
		r.Counters.Streams = 1 // the EXCLUDE or the endpoint stream
	}
	return r, apps
}

// classified is a scripted app's row before the engine's part (launchers, the stream limit, warnings).
type classified struct {
	app      scriptApp
	row      media.AudioApp
	allowed  bool // a row-5 allow rule decided
	builtin  bool // a built-in rule matched: the row shows without a session
	launcher bool // an IM_RULE_STOP names it
}

// precedence is 05 §5.3's rows 1–5: the rule that matches first decides; reason "" is heard (row 5).
var precedence = []struct {
	kind   media.RuleKind
	source media.RuleSource
	any    bool // any source
	reason string
}{
	{media.RuleInstance, 0, true, "isshoni"},                   // row 1: in the tree of an instance pid
	{media.RuleAppExclude, media.SourceSelf, false, "isshoni"}, // row 1: the walk hits isshoni.exe
	{media.RuleAppExclude, media.SourceBuiltin, false, "builtin"},
	{media.RuleAppExclude, media.SourceUser, false, "user"},
	{media.RuleAppExclude, media.SourceViewer, false, "viewer"},
	{media.RuleAppAllow, 0, true, ""},
}

// classify applies 05 §5.3 to a scripted app (its walk is the app itself; rules compare exe names
// case-insensitively). In the endpoint engine everything is heard, but the row keeps the matched rule's id and name.
func classify(app scriptApp, ex media.Exclusions, endpoint bool) classified {
	shared := app.shared || (ex.SharedPID != 0 && app.hasPID(uint64(ex.SharedPID)))
	c := classified{app: app, row: media.AudioApp{
		ID: "exe:" + app.exe, Name: app.name, Exe: app.exe, Verdict: verdictHeard, Playing: app.playing,
		UsesMic: app.usesMic, Browser: findRule(ex, media.RuleBrowser, 0, true, app) != nil, Shared: shared,
		PIDs: app.pids, Sessions: app.sessions, Warnings: []string{},
	}}
	c.launcher = findRule(ex, media.RuleStop, 0, true, app) != nil
	c.builtin = findRule(ex, media.RuleAppExclude, media.SourceBuiltin, false, app) != nil
	decided := false
	for _, p := range precedence {
		r := findRule(ex, p.kind, p.source, p.any, app)
		if r == nil {
			continue
		}
		if r.AppID != "" {
			c.row.ID = r.AppID
		}
		if r.Name != "" {
			c.row.Name = r.Name
		}
		if p.reason == "" {
			c.allowed = true
		} else {
			c.row.Verdict, c.row.Reason = verdictKeptOut, p.reason
		}
		decided = true
		break
	}
	if !decided && ex.KeepOutMicUsers && app.usesMic && !shared { // row 6
		c.row.Verdict, c.row.Reason = verdictKeptOut, "mic"
	}
	if endpoint {
		c.row.Verdict, c.row.Reason = verdictHeard, ""
	}
	return c
}

// findRule returns the first rule of ex of kind (and source, unless anySource) that names app: an instance rule by
// one of its pids, the others by its exe name.
func findRule(ex media.Exclusions, kind media.RuleKind, source media.RuleSource, anySource bool,
	app scriptApp) *media.Rule {
	for i := range ex.Rules {
		r := &ex.Rules[i]
		if r.Kind != kind || (!anySource && r.Source != source) {
			continue
		}
		if kind == media.RuleInstance {
			if pid, err := strconv.ParseUint(r.Value, 10, 32); err == nil && app.hasPID(pid) {
				return r
			}
			continue
		}
		if strings.EqualFold(r.Value, app.exe) {
			return r
		}
	}
	return nil
}

// addEvents turns the change from prev to cur at t into report events (05 §12.1), keeping the last 300.
func (a *audioState) addEvents(t time.Time, prev, cur media.AudioReport) {
	ev := func(code, app, detail string) {
		a.events = append(a.events, media.AudioEvent{TNanos: a.clock(t), Code: code, App: app, Detail: detail})
	}
	if prev.Probe.Result == "ok" && cur.Probe.Result == "failed" { // degrade-live: loopback failed twice
		app := ""
		if i := slices.IndexFunc(prev.Apps, func(r media.AudioApp) bool { return r.Streams > 0 }); i >= 0 {
			app = prev.Apps[i].ID
		}
		ev("stream_failed", app, "")
		ev("stream_failed", app, "")
		ev("probe_failed", "", "")
	}
	// Rows by their scripted app (its exe, unique in a scenario), so a rule that renames a row (an app id) is no session
	// or stream event; the session events follow a row's sessions, so a session-less row (a silent built-in voice app,
	// the shared app) appearing or going is none. A stopped report (a one-shot snapshot) has no sessions and no streams.
	rows := func(r media.AudioReport) map[string]media.AudioApp {
		m := map[string]media.AudioApp{}
		if r.State != stateStopped {
			for _, a := range r.Apps {
				m[a.Exe] = a
			}
		}
		return m
	}
	before, after := rows(prev), rows(cur)
	for _, r := range cur.Apps {
		if _, ok := after[r.Exe]; !ok {
			break // stopped
		}
		p, ok := before[r.Exe]
		if r.Sessions > 0 && (!ok || p.Sessions == 0) {
			ev("session_created", r.ID, "")
		}
		if p.Streams == 0 && r.Streams > 0 {
			ev("stream_opened", r.ID, "")
			ev("stream_running", r.ID, "")
		}
	}
	for _, p := range prev.Apps {
		if _, ok := before[p.Exe]; !ok {
			break // stopped
		}
		r, ok := after[p.Exe]
		if p.Streams > 0 && r.Streams == 0 {
			ev("stream_removed", p.ID, "")
		}
		if p.Sessions > 0 && (!ok || r.Sessions == 0) && cur.State != stateStopped {
			ev("session_expired", p.ID, "")
		}
	}
	if prev.State != stateStopped && cur.State != stateStopped {
		switch {
		case prev.Engine == engineIncludeSet && cur.Engine == engineExcludeSelf:
			ev("fastpath_enter", "", "")
		case prev.Engine == engineExcludeSelf && cur.Engine == engineIncludeSet:
			ev("fastpath_exit", "", "")
		}
	}
	if prev.Live && cur.Live {
		if prev.Mode != cur.Mode {
			ev("mode_changed", "", string(cur.Mode))
		}
		switch {
		case !prev.Muted && cur.Muted:
			ev("muted", "", "")
		case prev.Muted && !cur.Muted:
			ev("unmuted", "", "")
		}
	}
	if n := len(a.events) - maxReportEvents; n > 0 {
		a.events = slices.Delete(a.events, 0, n)
	}
}

// setRulesLocked replaces the rule set (a copy) at now; a change is a rules_changed event.
func (a *audioState) setRulesLocked(now time.Time, ex media.Exclusions) {
	ex.Rules = slices.Clone(ex.Rules)
	if len(ex.Rules) == 0 {
		ex.Rules = nil
	}
	if reflect.DeepEqual(ex, a.ex) {
		return
	}
	a.ex = ex
	a.events = append(a.events, media.AudioEvent{TNanos: a.clock(now), Code: "rules_changed"})
}

// levelsLocked is AudioLevels from the report at now (05 §12.2): one entry per row with a session or a stream,
// captured (post-fade, so silent while muted, paused or off) for rows with a running stream and activity otherwise;
// the mix sums what friends hear.
func (a *audioState) levelsLocked(now time.Time) media.AudioLevels {
	r := a.prev
	st := now.Sub(a.runSince)
	silenced := r.Muted || r.Paused || r.Mode == media.AudioOff
	lv := media.AudioLevels{ReportSeq: a.seq, QPCNanos: a.clock(now), MixPeakDB: silenceDB, MixRMSDB: silenceDB}
	var playing, quiet []media.AudioLevel
	mix := 0.0 // power
	for i, row := range r.Apps {
		if row.Sessions == 0 && row.Streams == 0 {
			continue
		}
		peak := silenceDB
		if app := a.prevApps[i]; row.Playing && app.level != nil {
			peak = app.level(st)
		}
		inMix := r.Live && r.Engine != engineNone && row.Playing &&
			(row.Streams > 0 || (r.Engine != engineIncludeSet && row.Verdict == verdictHeard))
		if inMix && !silenced {
			mix += math.Pow(10, peak/10)
		}
		l := media.AudioLevel{AppID: row.ID, PeakDB: float32(peak), RMSDB: float32(peak), Captured: row.Streams > 0}
		if l.Captured {
			if silenced {
				l.PeakDB, l.RMSDB = silenceDB, silenceDB
			} else {
				l.RMSDB = float32(peak - rmsBelowPeakDB)
			}
		}
		if row.Playing {
			playing = append(playing, l)
		} else {
			quiet = append(quiet, l)
		}
	}
	lv.Apps = append(playing, quiet...) // most recently active first
	if len(lv.Apps) > maxLevelApps {
		lv.Apps = lv.Apps[:maxLevelApps]
	}
	if mix > 0 {
		lv.MixPeakDB = float32(10 * math.Log10(mix))
		lv.MixRMSDB = lv.MixPeakDB - rmsBelowPeakDB
	}
	return lv
}

// cloneReport returns a deep copy of r.
func cloneReport(r media.AudioReport) media.AudioReport {
	r.Apps = slices.Clone(r.Apps)
	for i := range r.Apps {
		r.Apps[i].Warnings = slices.Clone(r.Apps[i].Warnings)
	}
	r.Sessions = slices.Clone(r.Sessions)
	r.Warnings = slices.Clone(r.Warnings)
	r.Events = slices.Clone(r.Events)
	return r
}

// checkAudioCall is the start of every AudioController call: ctx, a closed Engine (stopped, Reason "closed") and an
// unknown scenario (invalid).
func (e *Engine) checkAudioCall(ctx context.Context, op string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := e.checkOpen(op); err != nil {
		return err
	}
	if e.audio.sc == nil {
		return errorf(media.CodeInvalid, op, "", "unknown fake audio scenario %q", e.audio.name)
	}
	return nil
}

// checkExclusions checks ex as the binding and the DLL together do (media.AudioController's doc): im_audio_watch,
// im_set_exclusions and im_start refuse more than 512 rules, unknown kinds and sources, empty values, malformed pids
// and app ids outside [a-z0-9:._-]{1,63} (05 §4.3); the binding refuses, before any call, values of 520 bytes or more
// and a NUL byte in a value, app id or name, which a C string can't carry. Details name the rule's index, never its
// strings.
func checkExclusions(op string, ex media.Exclusions) error {
	if len(ex.Rules) > maxRules {
		return errorf(media.CodeInvalid, op, "", "%d rules, at most %d", len(ex.Rules), maxRules)
	}
	for i, r := range ex.Rules {
		var bad string
		switch {
		case r.Kind > media.RuleWebViewDataDir:
			bad = fmt.Sprintf("unknown kind %d", r.Kind)
		case r.Source > media.SourceData:
			bad = fmt.Sprintf("unknown source %d", r.Source)
		case r.Value == "":
			bad = "empty value"
		case len(r.Value) >= maxRuleValue:
			bad = fmt.Sprintf("a value of %d bytes, at most %d", len(r.Value), maxRuleValue-1)
		case strings.ContainsRune(r.Value, 0) || strings.ContainsRune(r.AppID, 0) || strings.ContainsRune(r.Name, 0):
			bad = "a NUL byte"
		case r.Kind == media.RuleInstance && !isDecimalPID(r.Value):
			bad = "a malformed pid"
		case r.AppID != "" && !isAppID(r.AppID):
			bad = "an app id outside [a-z0-9:._-]{1,63}"
		default:
			continue
		}
		return errorf(media.CodeInvalid, op, "", "rule %d: %s", i, bad)
	}
	return nil
}

// isDecimalPID reports whether s is a pid in decimal digits.
func isDecimalPID(s string) bool {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return false
	}
	_, err := strconv.ParseUint(s, 10, 32)
	return err == nil
}

// isAppID reports whether s matches [a-z0-9:._-]{1,63} (05 §4.1's app_id).
func isAppID(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	return strings.Trim(s, "abcdefghijklmnopqrstuvwxyz0123456789:._-") == ""
}

// WatchAudio implements media.AudioController: it starts the watcher, and the script's clock if neither the watcher
// nor a session with audio ran, and replaces the rule set with ex. Idempotent. Before any Start, as the share sheet
// needs.
func (e *Engine) WatchAudio(ctx context.Context, ex media.Exclusions) error {
	const op = "audio_watch"
	if err := e.checkAudioCall(ctx, op); err != nil {
		return err
	}
	if err := checkExclusions(op, ex); err != nil {
		return err
	}
	a := &e.audio
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	a.advanceLocked(now)
	if info, ok := a.info(); !a.watching && (!ok || !info.liveAt(now)) { // the watcher starts now
		a.runSince = now
	}
	a.watching = true
	a.setRulesLocked(now, ex)
	info, ok := a.info()
	a.stepLocked(now, info, ok)
	return nil
}

// UnwatchAudio implements media.AudioController: it stops the watcher; a session with audio keeps it running until it
// ends (05 §4.1).
func (e *Engine) UnwatchAudio(ctx context.Context) error {
	if err := e.checkAudioCall(ctx, "audio_unwatch"); err != nil {
		return err
	}
	a := &e.audio
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	a.advanceLocked(now)
	a.watching = false
	info, ok := a.info()
	a.stepLocked(now, info, ok)
	return nil
}

// SetExclusions implements media.AudioController: it replaces the rule set, which re-derives the verdicts (05 §5.3),
// also without a watcher (the rules wait for the next WatchAudio, as natively), and records the call in the running
// session's Calls ("exclusions").
func (e *Engine) SetExclusions(ctx context.Context, ex media.Exclusions) error {
	const op = "set_exclusions"
	if err := e.checkAudioCall(ctx, op); err != nil {
		return err
	}
	if err := checkExclusions(op, ex); err != nil {
		return err
	}
	a := &e.audio
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	a.advanceLocked(now)
	a.setRulesLocked(now, ex)
	if s := e.Session(); s != nil {
		s.recordExclusions()
	}
	info, ok := a.info()
	a.stepLocked(now, info, ok)
	return nil
}

// SetAudioMode implements media.AudioController: it accepts only media.AudioOff and media.AudioExcludeApps (else
// CodeInvalid, checked first, as 05 §13.1's binding does in Go without a call) and needs a running session with audio
// (else ErrAudioNotRunning). AudioOff makes the report's mode "off" (engine "none", no streams) and the session's
// audio Opus silence until AudioExcludeApps; the call is recorded in the session's Calls ("audio_mode", Mode).
func (e *Engine) SetAudioMode(ctx context.Context, m media.AudioMode) error {
	const op = "audio_set_mode"
	if err := e.checkAudioCall(ctx, op); err != nil {
		return err
	}
	switch m {
	case media.AudioOff, media.AudioExcludeApps:
	default:
		return errorf(media.CodeInvalid, op, "", "audio mode %q", m)
	}
	a := &e.audio
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	a.advanceLocked(now)
	s := e.Session()
	if s == nil {
		return &media.Error{Code: media.CodeAudioNotRunning, Op: op}
	}
	if err := s.setAudioMode(m); err != nil {
		return err
	}
	info, ok := a.info()
	a.stepLocked(now, info, ok)
	return nil
}

// SetAudioMuted implements media.AudioController: it needs a running session with audio (else ErrAudioNotRunning);
// muted, that session's audio packets carry Opus silence instead of the tone until it is unmuted (01 §6.2), the
// report says "muted", and the call is recorded in its Calls ("audio_muted", Muted and On).
func (e *Engine) SetAudioMuted(ctx context.Context, muted bool) error {
	const op = "audio_set_muted"
	if err := e.checkAudioCall(ctx, op); err != nil {
		return err
	}
	a := &e.audio
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	a.advanceLocked(now)
	s := e.Session()
	if s == nil {
		return &media.Error{Code: media.CodeAudioNotRunning, Op: op}
	}
	if err := s.setAudioMuted(muted); err != nil {
		return err
	}
	info, ok := a.info()
	a.stepLocked(now, info, ok)
	return nil
}

// AudioReport implements media.AudioController: the scenario's report now (a copy), or SetAudioStatus's. Without a
// watcher it is a one-shot snapshot of the script's start with state "stopped".
func (e *Engine) AudioReport(ctx context.Context) (media.AudioReport, error) {
	if err := e.checkAudioCall(ctx, "list_audio_apps"); err != nil {
		return media.AudioReport{}, err
	}
	a := &e.audio
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.override != nil {
		return cloneReport(*a.override), nil
	}
	now := time.Now()
	a.advanceLocked(now)
	r := cloneReport(a.prev)
	r.Seq, r.QPCNanos, r.Events = a.seq, a.clock(now), slices.Clone(a.events)
	if r.Events == nil {
		r.Events = []media.AudioEvent{}
	}
	return r, nil
}

// AudioLevels implements media.AudioController: the scripted levels now (05 §12.2; Spotify's −18 dB ± 6 dB sine,
// Discord's −24 dB activity, …), with ReportSeq the report's Seq. Neither watching nor live, it is
// ErrAudioNotRunning.
func (e *Engine) AudioLevels(ctx context.Context) (media.AudioLevels, error) {
	const op = "audio_levels_get"
	if err := e.checkAudioCall(ctx, op); err != nil {
		return media.AudioLevels{}, err
	}
	a := &e.audio
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	a.advanceLocked(now)
	if a.prev.State == stateStopped {
		return media.AudioLevels{}, &media.Error{Code: media.CodeAudioNotRunning, Op: op}
	}
	lv := a.levelsLocked(now)
	if a.override != nil {
		lv.ReportSeq = a.override.Seq
	}
	return lv, nil
}

// SoundCheck implements media.AudioController: after 1.5 s it answers {captured, levelDb −14, ms 1500, backend
// "process-loopback"}; "degraded" and "old-windows" say backend "endpoint", and "no-device" {captured false, reason
// "no_device", backend "none"}. A timeout outside 1–10 s is CodeInvalid; one under 1.5 s gives {captured false,
// reason "timeout"} when it ends. helperPID is not used. It needs no watcher; ctx and Close end it.
func (e *Engine) SoundCheck(ctx context.Context, helperPID uint32, timeout time.Duration) (media.SoundCheckResult,
	error) {
	const op = "audio_sound_check"
	if err := e.checkAudioCall(ctx, op); err != nil {
		return media.SoundCheckResult{}, err
	}
	if timeout < minSoundCheck || timeout > maxSoundCheck {
		return media.SoundCheckResult{}, errorf(media.CodeInvalid, op, "", "timeout %v not in %v–%v", timeout,
			minSoundCheck, maxSoundCheck)
	}
	res, wait := e.audio.sc.soundCheck, soundCheckAfter
	if timeout < wait {
		res, wait = media.SoundCheckResult{Reason: "timeout", Backend: res.Backend}, timeout
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-t.C:
		return res, nil
	case <-ctx.Done():
		return media.SoundCheckResult{}, ctx.Err()
	case <-e.done:
		return media.SoundCheckResult{}, errClosed(op)
	}
}

// SetAudioStatus makes AudioReport return a copy of r, and AudioLevels' ReportSeq r.Seq, instead of the script's,
// for a test that needs a report no scenario makes; nil goes back to the script. (05 §13.3 calls it the override of
// Session.AudioStatus; the contract's report is AudioController.AudioReport, so it is the Engine's.)
func (e *Engine) SetAudioStatus(r *media.AudioStatus) {
	e.audio.mu.Lock()
	defer e.audio.mu.Unlock()
	if r == nil {
		e.audio.override = nil
		return
	}
	c := cloneReport(*r)
	e.audio.override = &c
}

// audioSessionStartedLocked is Start's part for a session with audio, under audioState.mu, which Start took before
// the session existed: the session's Exclusions replace the rule set (as im_start's do, 05 §4.2), and the watcher
// runs while the session lives (the script's clock starts with the session when no watcher ran). Only for a known
// scenario.
func (e *Engine) audioSessionStartedLocked(s *Session, ex media.Exclusions) {
	a := &e.audio
	now := time.Now()
	a.advanceLocked(now)
	info := s.audioInfo()
	if !a.watching {
		a.runSince = info.start
	}
	a.sess = s
	a.setRulesLocked(now, ex)
	a.stepLocked(now, info, true)
}
