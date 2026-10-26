package main

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
)

// This file is the portable verdict logic of the audiotest (docs/m2/05-windows-audio.md §18.6). evaluate turns a
// recording and a case into a caseReport: it runs the analyzer's present/absent and short-window rules, the click
// check, the measured cut and onset, the session lag and fast-path exit, the positive controls and the engine and
// banner expectations, and decides PASS, FAIL, INCONCLUSIVE or SKIP. It takes already-recorded data, so it runs on
// every OS and is the subject of the analyzer and verdict tests (05 §19.1). The Windows driver (driver_windows.go)
// builds the recording through capture.go; the special cases 20, 21 and 27 are scored in special.go.

// recording is what one run produced, in a form the portable verdict can read. Times are seconds relative to the
// recording's start (the first tap chunk, about 1 s after the case's initial fixtures run, 05 §18.5). The capture
// maps engine-clock times (firstRenderNs, session_created, the recorder's SetExclusions stamps) to these seconds
// with the offset of 05 §13.2.
type recording struct {
	left, right []float64 // the PCM tap, 48 kHz, gaps zero-padded
	recordDur   float64   // the recording length in seconds (the "to the end" bound)
	tap0NS      int64     // the first tap chunk's capture time (session clock)

	reparented   bool            // every fixture was started under explorer.exe (05 §18.3)
	silenceFail  bool            // the silence pre-check failed (another app is playing, 05 §18.4)
	keptOutOther bool            // a non-fixture process was kept out (the fast-path cases' premise, 05 §18.4)
	lostGapMs    int             // the largest gap in PCM-tap capture times no new grid explains (05 §18.6)
	foreign      []foreignSample // non-fixture apps heard with peak > −60 dBFS (05 §18.6)

	roles       map[string]*roleTrack // per fixture role, from the sessions of its pid, sampled every 250 ms
	roleApp     map[string]string     // role → the app id of its sessions (events name apps by id)
	roleStart   map[string]float64    // role → when its process started
	engineAt    []engineSample        // the report's engine per sample
	bannerAt    []bannerSample        // the service's banner codes per sample
	promptAt    []promptSample        // the service's pending prompts per sample (cases 12, 13)
	events      []recEvent            // engine events at or after the case's watcher start, each once
	firstRender map[string]float64    // role → the fixture's logged first-render time
	setStamp    float64               // the recorder's SetExclusions stamp for the rule change (cases 13, 15's T_set)
	setStampOK  bool
	counters    map[string]int // the final report's mixer counters (fastpathLateCuts, …)
	opus        []opusPkt      // the Opus packets (case 21)
	micFail     bool           // a fixture could not open the microphone (05 §18.4: a mic case SKIPs)
}

// roleTrack is one fixture's history in the report samples.
type roleTrack struct {
	neverActive bool              // the fixture never appeared as an active session
	exitedEarly bool              // the fixture exited before its planned end (05 §18.6)
	samples     []roleSampleState // the report samples in which its pid had a session, oldest first
}

// roleSampleState is one report sample for one fixture role: its session (by pid) and the app row of that session's
// app id. The session is the role's render session; capture marks a role that had only a microphone session in
// that sample (its verdict says nothing about playback, its row is still the app's).
type roleSampleState struct {
	t       float64
	active  bool   // the session's state is "active"
	capture bool   // a microphone (capture-flow) session, not a render session
	verdict string // the session's: "heard" | "kept_out" | "not_captured" | "pending"
	reason  string
	note    string // "" | "covered" | "no_handle"

	row         bool // the report had a row for the session's app id
	rowVerdict  string
	rowReason   string
	rowDetail   string
	rowWarnings []string // the row's warnings ("shared_app_uses_mic", …)
	rowBrowser  bool
}

// promptSample is the pending FriendsHear prompts at one sample.
type promptSample struct {
	t       float64
	prompts []promptState
}

// engineSample is the report's engine at one sample.
type engineSample struct {
	t     float64
	value string
}

// bannerSample is the banner codes at one sample.
type bannerSample struct {
	t     float64
	codes []string
}

// recEvent is one engine event: its time in seconds, its code, and the app id it names ("" for engine-wide events).
type recEvent struct {
	t    float64
	code string
	app  string
}

// resolveBound resolves b to a time in seconds. isTo marks the "to" bound, whose zero value means the end of the
// recording. An event bound that never happened returns ok false (the case is INCONCLUSIVE, 05 §18.6).
func (rec recording) resolveBound(b bound, isTo bool) (float64, bool) {
	if b.event == "" {
		if isTo && b.abs == 0 {
			return rec.recordDur, true
		}
		return b.abs, true
	}
	switch {
	case b.event == eventTSet:
		if !rec.setStampOK {
			return 0, false
		}
		return rec.setStamp + b.offset, true
	case strings.HasPrefix(b.event, sessionPrefix):
		t, ok := rec.sessionCreated(strings.TrimPrefix(b.event, sessionPrefix))
		if !ok {
			return 0, false
		}
		return t + b.offset, true
	default:
		t, ok := rec.firstRender[b.event]
		if !ok {
			return 0, false
		}
		return t + b.offset, true
	}
}

// sessionCreated is the time of role's session_created event: the first one that names the role's app id at or
// after the role's process started (so an earlier fixture of the same app, or of an earlier case, never counts).
func (rec recording) sessionCreated(role string) (float64, bool) {
	app, ok := rec.roleApp[role]
	if !ok {
		return 0, false
	}
	from := math.Inf(-1)
	if s, ok := rec.roleStart[role]; ok {
		from = s
	} else if fr, ok := rec.firstRender[role]; ok {
		from = fr
	}
	for _, e := range rec.events {
		if e.code == "session_created" && e.app == app && e.t >= from {
			return e.t, true
		}
	}
	return 0, false
}

// newCaseReport is an empty report for cs with every list and map set, so the JSON has [] and {} rather than null.
func newCaseReport(cs caseSpec, cfg gateConfig) caseReport {
	return caseReport{
		Name: cs.name, Required: cs.requiredUnder(cfg), Checks: []checkReport{}, Measured: map[string]measure{},
		ClicksAt: []float64{}, Problems: []string{}, Engine: []string{}, Counters: map[string]int{},
		Events: []reportEvent{},
	}
}

// withResult sets cr's result as it counts for the gate (05 §18.10's effectiveResult).
func withResult(cs caseSpec, cfg gateConfig, cr caseReport, result string) caseReport {
	cr.Result = effectiveResult(cs, cfg, result)
	return cr
}

// inconclusive builds an INCONCLUSIVE caseReport with one problem.
func inconclusive(cs caseSpec, cfg gateConfig, format string, args ...any) caseReport {
	cr := newCaseReport(cs, cfg)
	cr.Problems = append(cr.Problems, fmt.Sprintf(format, args...))
	return withResult(cs, cfg, cr, resultInconclusive)
}

// skipped builds a SKIP caseReport with one problem (05 §18.4: a missing precondition).
func skipped(cs caseSpec, cfg gateConfig, problem string) caseReport {
	cr := newCaseReport(cs, cfg)
	cr.Problems = append(cr.Problems, problem)
	return withResult(cs, cfg, cr, resultSkip)
}

// premiseProblem is the first unproven premise of rec for cs, or "" (05 §18.6: an unproven premise is
// INCONCLUSIVE, not FAIL).
func premiseProblem(cs caseSpec, rec recording) string {
	switch {
	case !rec.reparented:
		return "fixtures weren't reparented under explorer.exe"
	case rec.silenceFail:
		return "the silence pre-check failed: other apps are playing"
	case rec.lostGapMs > 30:
		return fmt.Sprintf("the harness lost audio: a %d ms gap in PCM-tap capture", rec.lostGapMs)
	case cs.forceFastPath && rec.keptOutOther:
		return "a non-fixture process is kept out (named on the console): quit voice apps before the fast-path cases"
	}
	windowFrom := rec.expectFrom(cs)
	for _, f := range rec.foreign {
		if f.t >= windowFrom && f.t <= rec.recordDur {
			return "another app was heard during the window"
		}
	}
	for _, role := range checkedRoles(cs) {
		tr := rec.roles[role]
		switch {
		case tr == nil || tr.neverActive:
			return "a fixture never appeared as an active session"
		case tr.exitedEarly:
			return "a fixture exited early: it may never have played"
		}
	}
	return ""
}

// expectFrom is where cs's engine, banner and foreign-app checks start (zero = 1.5 s).
func (rec recording) expectFrom(cs caseSpec) float64 {
	if t, ok := rec.resolveBound(cs.expectFrom, false); ok && t > 0 {
		return t
	}
	return 1.5
}

// micSkip is the SKIP problem of a mic case whose fixture could not open the microphone (05 §18.4).
const micSkip = "a fixture could not open the microphone: connect one and allow desktop apps to use it " +
	"(Settings → Privacy & security → Microphone)"

// evaluate runs the portable verdict for a non-special case (05 §18.6). A FAIL is downgraded to INCONCLUSIVE only by
// the premises ("the premise couldn't be proven"), never by "it was close".
func evaluate(cs caseSpec, rec recording, cfg gateConfig) caseReport {
	if rec.micFail && cs.needsMic() {
		return skipped(cs, cfg, micSkip)
	}
	if p := premiseProblem(cs, rec); p != "" {
		return inconclusive(cs, cfg, "%s", p)
	}

	// Resolve every event-relative window and measurement first: an event that never happened is INCONCLUSIVE,
	// unless the case's prompt never appeared, which is why the decision that makes the event never came (case 13:
	// a FAIL, the mic user was not detected).
	unresolved := func(problem string) caseReport {
		if pp := rec.promptProblem(cs); pp != "" {
			cr := newCaseReport(cs, cfg)
			cr.Problems = append(cr.Problems, pp, problem)
			return withResult(cs, cfg, cr, resultFail)
		}
		return inconclusive(cs, cfg, "%s", problem)
	}
	type window struct{ from, to float64 }
	wins := make([]window, len(cs.checks))
	for i, c := range cs.checks {
		from, ok1 := rec.resolveBound(c.from, false)
		to, ok2 := rec.resolveBound(c.to, true)
		if !ok1 || !ok2 {
			return unresolved("an event-relative window never resolved: a fixture didn't start")
		}
		wins[i] = window{from, to}
	}
	for _, ms := range cs.measures {
		if !rec.measureEventsHappened(ms) {
			return unresolved(fmt.Sprintf("the event %s is measured from never happened: a fixture didn't start",
				ms.key))
		}
	}

	cr := newCaseReport(cs, cfg)
	cr.Counters = cloneCounters(rec.counters)
	cr.Engine = enginesSeen(rec)
	cr.Events = reportEvents(rec.events, rec.roleApp)

	m := mono(rec.left, rec.right)
	fail := false

	// P_min is the lowest present level, so compute every level first.
	levels := make([]float64, len(cs.checks))
	pMin := 0.0
	for i, c := range cs.checks {
		levels[i] = toneLevelDB(segment(m, sampleRate, wins[i].from, wins[i].to), sampleRate, c.freq)
		if c.expect == expectPresent && levels[i] < pMin {
			pMin = levels[i]
		}
	}
	for i, c := range cs.checks {
		w := wins[i]
		rep := checkReport{Freq: c.freq, From: round3(w.from), To: toReported(c.to, w.to), Expect: c.expect,
			LevelDB: roundDB(levels[i])}
		switch c.expect {
		case expectPresent:
			rep.Pass = levels[i] >= presentMinDB
		case expectAbsent:
			maxShort, atS := shortWindow(segment(m, sampleRate, w.from, w.to), sampleRate, c.freq)
			rep.MaxShortDB = roundDB(maxShort)
			rep.MaxShortAtS = round3(w.from + atS + shortWindowSec/2) // the window's centre
			rep.Pass = levels[i] <= absentMaxDB && levels[i] <= pMin-absentMarginDB && maxShort <= absentMaxDB
		default: // info
			rep.Pass = true
		}
		if !rep.Pass {
			fail = true
		}
		cr.Checks = append(cr.Checks, rep)

		// Positive control for the check's fixture role, over the check's window (05 §18.6).
		if problem := rec.controlProblem(c, w.from, w.to); problem != "" {
			cr.Problems = append(cr.Problems, problem)
			fail = true
		}
	}
	for _, rc := range cs.rowChecks {
		if problem := rec.rowCheckProblem(rc); problem != "" {
			cr.Problems = append(cr.Problems, problem)
			fail = true
		}
	}
	if problem := rec.promptProblem(cs); problem != "" {
		cr.Problems = append(cr.Problems, problem)
		fail = true
	}

	// Clicks (05 §18.6): default window [1.5 s, end] unless the case overrides it or disables the check.
	if !cs.noClick {
		cf, _ := rec.resolveBound(firstNonZero(cs.clickFrom, at(1.5)), false)
		ct, _ := rec.resolveBound(cs.clickTo, true)
		for _, t := range clickEvents(segment(rec.left, sampleRate, cf, ct), segment(rec.right, sampleRate, cf, ct),
			sampleRate, clickThreshold) {
			cr.ClicksAt = append(cr.ClicksAt, round3(cf+t))
		}
		if len(cr.ClicksAt) > 0 {
			fail = true
		}
	}

	// Engine and banner expectations in every sample of the window (05 §18.6).
	from := rec.expectFrom(cs)
	if cs.engineThroughout != "" {
		for _, e := range rec.engineAt {
			if e.t >= from && e.t <= rec.recordDur && e.value != cs.engineThroughout {
				cr.Problems = append(cr.Problems, fmt.Sprintf("engine was %q at %.2fs, want %q throughout", e.value,
					e.t, cs.engineThroughout))
				fail = true
				break
			}
		}
	}
	if cs.bannerThroughout != "" {
		for _, b := range rec.bannerAt {
			if b.t >= from && b.t <= rec.recordDur && !slices.Contains(b.codes, cs.bannerThroughout) {
				cr.Problems = append(cr.Problems, fmt.Sprintf("banner %q missing from the sample at %.2fs",
					cs.bannerThroughout, b.t))
				fail = true
				break
			}
		}
	}

	// Measured cut/onset, session lag and fast-path exit (05 §17, §18.6).
	for _, ms := range cs.measures {
		value, ok := rec.measureValue(ms, m)
		if !ok {
			cr.Problems = append(cr.Problems, fmt.Sprintf("%s could not be measured", ms.key))
			fail = true
			continue
		}
		cr.Measured[ms.key] = measure{ValueMs: value, BudgetMs: ms.budget}
		if value > ms.budget {
			cr.Problems = append(cr.Problems, fmt.Sprintf("%s %d ms exceeds the %d ms budget", ms.key, value,
				ms.budget))
			fail = true
		}
	}

	// Counters that must be zero (case 5's fastpathLateCuts) or stay under a ceiling.
	if problems := counterProblems(cs, rec.counters); len(problems) > 0 {
		cr.Problems = append(cr.Problems, problems...)
		fail = true
	}

	result := resultPass
	if fail {
		result = resultFail
	}
	return withResult(cs, cfg, cr, result)
}

// controlProblem checks the positive control a check implies about its fixture role over [from, to] (05 §18.6): a
// present check's fixture must not be kept_out in any sample where it is active; an absent check's must be kept_out
// (or the expected not_captured) with the expected reason in every such sample. It returns "" when the control holds,
// else a redacted problem.
func (rec recording) controlProblem(c checkSpec, from, to float64) string {
	want := c.ctl
	if want == "" {
		switch c.expect {
		case expectPresent:
			want = controlNotKeptOut
		case expectAbsent:
			want = controlKeptOut
		default:
			want = controlNone
		}
	}
	if c.role == "" || want == controlNone {
		return ""
	}
	tr := rec.roles[c.role]
	if tr == nil {
		return "a fixture never appeared in the report"
	}
	for _, s := range tr.samples {
		if s.t < from || s.t > to || !s.active || s.capture {
			continue // a microphone session says nothing about what the fixture plays
		}
		if want == controlNotKeptOut {
			if s.verdict == controlKeptOut {
				return fmt.Sprintf("a fixture was kept_out at %.2fs in a present window", s.t)
			}
			continue
		}
		if s.verdict != want {
			return fmt.Sprintf("a fixture was %q at %.2fs in its window, want %q", s.verdict, s.t, want)
		}
		if c.reason != "" && s.reason != c.reason {
			return fmt.Sprintf("a fixture's reason was %q at %.2fs, want %q", s.reason, s.t, c.reason)
		}
	}
	return ""
}

// rowCheckProblem checks a row or session expectation (05 §18.5) in every sample at or after its start where the
// role's session is active; at least one such sample must exist.
func (rec recording) rowCheckProblem(rc rowCheck) string {
	from, _ := rec.resolveBound(firstNonZero(rc.from, at(1.5)), false)
	tr := rec.roles[rc.role]
	if tr == nil {
		return "a fixture never appeared in the report"
	}
	seen := false
	for _, s := range tr.samples {
		if s.t < from || s.t > rec.recordDur || !s.active {
			continue
		}
		if rc.note != "" {
			if s.capture {
				continue // the note is a render session's
			}
			if s.note != rc.note {
				return fmt.Sprintf("a fixture's session note was %q at %.2fs, want %q", s.note, s.t, rc.note)
			}
		}
		seen = true
		if rc.verdict == "" && rc.reason == "" && rc.detail == "" && rc.warning == "" && !rc.browser {
			continue
		}
		switch {
		case !s.row:
			return fmt.Sprintf("a fixture's app row was missing at %.2fs", s.t)
		case rc.verdict != "" && s.rowVerdict != rc.verdict:
			return fmt.Sprintf("a fixture's app row was %q at %.2fs, want %q", s.rowVerdict, s.t, rc.verdict)
		case rc.reason != "" && s.rowReason != rc.reason:
			return fmt.Sprintf("a fixture's app row reason was %q at %.2fs, want %q", s.rowReason, s.t, rc.reason)
		case rc.detail != "" && !strings.EqualFold(s.rowDetail, rc.detail):
			return fmt.Sprintf("a fixture's app row detail at %.2fs is not the expected app", s.t)
		case rc.warning != "" && !slices.Contains(s.rowWarnings, rc.warning):
			return fmt.Sprintf("a fixture's app row lacked the warning %q at %.2fs", rc.warning, s.t)
		case rc.browser && !s.rowBrowser:
			return fmt.Sprintf("a fixture's app row was not a browser's at %.2fs", s.t)
		}
	}
	if !seen {
		return "a fixture's session was never active in its row check's window"
	}
	return ""
}

// promptProblem checks cs's prompt expectations (05 §18.5 cases 12, 13: "a browser-mic prompt in FriendsHear", "the
// mic prompt existed"): some sample inside each check's window shows a pending prompt of its kind for the app of its
// role. It returns "" when every one held.
func (rec recording) promptProblem(cs caseSpec) string {
	for _, pc := range cs.promptChecks {
		from, _ := rec.resolveBound(firstNonZero(pc.from, at(1.5)), false)
		to, ok := rec.resolveBound(pc.to, true)
		if !ok {
			to = rec.recordDur
		}
		app, known := rec.roleApp[pc.role]
		found := false
		for _, ps := range rec.promptAt {
			if ps.t < from || ps.t > to {
				continue
			}
			for _, p := range ps.prompts {
				if p.kind == pc.kind && known && p.appID == app {
					found = true
				}
			}
		}
		if !found {
			return fmt.Sprintf("no %q prompt for the fixture's app appeared in [%.2fs, %.2fs]", pc.kind, from, to)
		}
	}
	return ""
}

// counterProblems are cs's counter expectations that rec's final counters break: a counter that must stay 0, or one
// above its ceiling.
func counterProblems(cs caseSpec, counters map[string]int) []string {
	var out []string
	for _, key := range cs.counterZero {
		if counters[key] != 0 {
			out = append(out, fmt.Sprintf("counter %s is %d, want 0", key, counters[key]))
		}
	}
	keys := make([]string, 0, len(cs.counterMax))
	for key := range cs.counterMax {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if limit := cs.counterMax[key]; counters[key] > limit {
			out = append(out, fmt.Sprintf("counter %s is %d, want ≤ %d", key, counters[key], limit))
		}
	}
	return out
}

// measureEventsHappened reports whether the events ms is measured from exist (an absent one is INCONCLUSIVE).
func (rec recording) measureEventsHappened(ms measureSpec) bool {
	switch ms.key {
	case measureSessionLag:
		_, ok1 := rec.sessionCreated(ms.role)
		_, ok2 := rec.firstRender[ms.role]
		return ok1 && ok2
	case measureFastpathExit:
		_, ok := rec.sessionCreated(ms.role)
		return ok
	default:
		_, ok := rec.resolveBound(ms.event, false)
		return ok
	}
}

// measureValue is the measured value in ms for ms, and whether it could be measured (05 §18.6, §18.7).
func (rec recording) measureValue(ms measureSpec, m []float64) (int, bool) {
	switch ms.key {
	case measureSessionLag:
		// session lag = the voice app's session_created − its own logged firstRenderNs (05 §18.5 case 5).
		created, ok1 := rec.sessionCreated(ms.role)
		fr, ok2 := rec.firstRender[ms.role]
		if !ok1 || !ok2 {
			return 0, false
		}
		return int(math.Round((created - fr) * 1000)), true
	case measureFastpathExit:
		// The first fastpath_exit since the voice app started, minus its session_created (≤ 15 ms, §17).
		created, ok := rec.sessionCreated(ms.role)
		if !ok {
			return 0, false
		}
		from := created
		if s, ok := rec.roleStart[ms.role]; ok {
			from = s
		}
		for _, e := range rec.events {
			if e.code == "fastpath_exit" && e.t >= from {
				return int(math.Round((e.t - created) * 1000)), true
			}
		}
		return 0, false
	}
	ev, ok := rec.resolveBound(ms.event, false)
	if !ok {
		return 0, false
	}
	cutS, cutOK, onsetS, onsetOK := measuredCutOnset(m, sampleRate, ms.freq, ev)
	switch ms.key {
	case measureCut:
		if !cutOK {
			return 0, true // the tone was already silent at the event: a 0 ms cut
		}
		return int(math.Round((cutS - ev) * 1000)), true
	case measureOnset:
		if !onsetOK {
			return 0, false
		}
		return int(math.Round((onsetS - ev) * 1000)), true
	}
	return 0, false
}

// playingRoles are the fixture roles a case checks a tone of (a check with a role): each must appear active.
func playingRoles(cs caseSpec) []string {
	var out []string
	for _, c := range cs.checks {
		if c.role != "" && !slices.Contains(out, c.role) {
			out = append(out, c.role)
		}
	}
	return out
}

// checkedRoles are the playing roles plus the roles of the row checks: the positive controls cover all of them.
func checkedRoles(cs caseSpec) []string {
	out := playingRoles(cs)
	for _, rc := range cs.rowChecks {
		if !slices.Contains(out, rc.role) {
			out = append(out, rc.role)
		}
	}
	return out
}

// enginesSeen is the distinct engine values across the report samples, in first-seen order.
func enginesSeen(rec recording) []string {
	out := []string{}
	for _, e := range rec.engineAt {
		if e.value != "" && !slices.Contains(out, e.value) {
			out = append(out, e.value)
		}
	}
	return out
}

// reportEvents maps engine events to report events (05 §18.7): the time in ms, the code, and the app as
// "fixture:<role>" for a fixture's app id (roles sharing an app id show the first by name), "" for an engine-wide
// event, and any other app by its own id, which the report's redaction (redact.go) turns into "other" unless the run
// is -no-redact.
func reportEvents(events []recEvent, roleApp map[string]string) []reportEvent {
	roles := make([]string, 0, len(roleApp))
	for role := range roleApp {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	appRole := map[string]string{}
	for _, role := range roles {
		if _, ok := appRole[roleApp[role]]; !ok {
			appRole[roleApp[role]] = role
		}
	}
	out := make([]reportEvent, 0, len(events))
	for _, e := range events {
		app := e.app
		if role, ok := appRole[e.app]; ok && e.app != "" {
			app = fixturePrefix + role
		}
		out = append(out, reportEvent{TMs: int(math.Round(e.t * 1000)), Code: e.code, App: app})
	}
	return out
}

func cloneCounters(c map[string]int) map[string]int {
	out := make(map[string]int, len(c))
	for k, v := range c {
		out[k] = v
	}
	return out
}

// firstNonZero returns b when it is set, else def (used for the default window starts).
func firstNonZero(b, def bound) bound {
	if b.event == "" && b.abs == 0 {
		return def
	}
	return b
}

// toReported is the "to" value written into the report: 0 for the end of the recording (05 §18.7: "to: 0"), else
// the resolved time.
func toReported(b bound, resolved float64) float64 {
	if b.event == "" && b.abs == 0 {
		return 0
	}
	return round3(resolved)
}

func roundDB(v float64) float64 { return math.Round(v*10) / 10 }
func round3(v float64) float64  { return math.Round(v*1000) / 1000 }
