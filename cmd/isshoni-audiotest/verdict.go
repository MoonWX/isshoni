package main

import (
	"fmt"
	"math"
	"slices"
	"strings"
)

// This file is the portable verdict logic of the audiotest (docs/m2/05-windows-audio.md §18.6). evaluate turns a
// recording and a case into a caseReport: it runs the analyzer's present/absent and short-window rules, the click
// check, the measured cut and onset, the positive controls and the engine and banner expectations, and decides
// PASS, FAIL, INCONCLUSIVE or SKIP. It takes already-recorded data, so it runs on every OS and is the subject of the
// analyzer and verdict tests (05 §19.1). The Windows driver (driver_windows.go) builds the recording; the special
// cases 20, 21 and 27 are built by the driver itself (they decode Opus or measure timestamps) and never reach here.

// recording is what one run produced, in a form the portable verdict can read. Times are seconds relative to the
// recording's start (1 s after the case's initial fixtures run, 05 §18.5). The Windows driver maps engine-clock
// events (firstRenderNs, session_created, the recorder's SetExclusions stamps) to these seconds with the offset of
// 05 §13.2 before filling this struct.
type recording struct {
	left, right []float64 // the PCM tap, 48 kHz
	recordDur   float64   // the recording length in seconds (the "to the end" bound)

	reparented  bool     // every fixture was started under explorer.exe (05 §18.3)
	silenceFail bool     // the silence pre-check failed (another app is playing, 05 §18.4)
	lostGapMs   int      // the largest gap in PCM-tap capture times beyond one 10 ms chunk (05 §18.6: > 30 ms loses)
	foreign     []string // non-fixture apps heard with peak > −60 dBFS during a window (redacted; 05 §18.6)

	roles       map[string]*roleTrack // per fixture role, sampled every 250 ms
	engineAt    []engineSample        // the report's engine per sample
	bannerAt    []bannerSample        // the report's banner codes per sample
	events      []recEvent            // engine events (session_created, …), redacted
	firstRender map[string]float64    // role → the fixture's logged first-render time
	setStamp    float64               // the recorder's SetExclusions stamp for the rule change (case 15's T_set)
	setStampOK  bool
	driverMeas  map[string]int // the driver's measurements in ms (sessionLagMs, fastpathExitMs)
	counters    map[string]int // the final report's mixer counters (fastpathLateCuts, …)
}

// roleTrack is one fixture's history in the report samples.
type roleTrack struct {
	neverActive bool              // the fixture never appeared as an active session
	exitedEarly bool              // the fixture exited before its planned end (05 §18.6)
	samples     []roleSampleState // the report samples, oldest first
}

// roleSampleState is one report sample for one fixture role.
type roleSampleState struct {
	t       float64
	active  bool
	verdict string // "heard" | "kept_out" | "not_captured" | "pending" | "" (absent from the report)
	reason  string
	detail  string
}

// engineSample is the report's engine at one sample.
type engineSample struct {
	t     float64
	value string
}

// bannerSample is the report's banner codes at one sample.
type bannerSample struct {
	t     float64
	codes []string
}

// recEvent is one engine event, redacted: its time in seconds, its code, and the fixture role it names (or "").
type recEvent struct {
	t    float64
	code string
	role string
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

// sessionCreated is the time of the first session_created event that names role.
func (rec recording) sessionCreated(role string) (float64, bool) {
	for _, e := range rec.events {
		if e.code == "session_created" && e.role == role {
			return e.t, true
		}
	}
	return 0, false
}

// inconclusive builds an INCONCLUSIVE caseReport with one problem.
func inconclusive(cs caseSpec, cfg gateConfig, format string, args ...any) caseReport {
	return caseReport{
		Name: cs.name, Required: cs.requiredUnder(cfg), Result: resultInconclusive,
		Problems: []string{fmt.Sprintf(format, args...)}, Measured: map[string]measure{},
		Counters: map[string]int{}, ClicksAt: []float64{},
	}
}

// evaluate runs the portable verdict for a non-special case (05 §18.6). A FAIL is downgraded to INCONCLUSIVE only by
// the premises below ("the premise couldn't be proven"), never by "it was close".
func evaluate(cs caseSpec, rec recording, cfg gateConfig) caseReport {
	// Premises: an unproven premise is INCONCLUSIVE, not FAIL (05 §18.6).
	switch {
	case !rec.reparented:
		return inconclusive(cs, cfg, "fixtures weren't reparented under explorer.exe")
	case rec.silenceFail:
		return inconclusive(cs, cfg, "the silence pre-check failed: another app is playing")
	case rec.lostGapMs > 30:
		return inconclusive(cs, cfg, "the harness lost audio: a %d ms gap in PCM-tap capture", rec.lostGapMs)
	case len(rec.foreign) > 0:
		return inconclusive(cs, cfg, "another app was heard during a window")
	}

	// Every fixture that plays a tone (a check with a role) must have appeared active and not exited early.
	for _, role := range playingRoles(cs) {
		tr := rec.roles[role]
		switch {
		case tr == nil || tr.neverActive:
			return inconclusive(cs, cfg, "a fixture never appeared as an active playing app")
		case tr.exitedEarly:
			return inconclusive(cs, cfg, "a fixture exited early — it may never have played")
		}
	}

	cr := caseReport{
		Name: cs.name, Required: cs.requiredUnder(cfg), Measured: map[string]measure{},
		Counters: cloneCounters(rec.counters), ClicksAt: []float64{}, Problems: []string{},
		Engine: enginesSeen(rec), Events: redactEvents(rec.events),
	}

	m := mono(rec.left, rec.right)
	fail := false

	// Resolve and evaluate the checks. P_min is the lowest present level, so compute levels first.
	type pending struct {
		spec  checkSpec
		from  float64
		to    float64
		level float64
	}
	var pend []pending
	pMin := 0.0
	for _, c := range cs.checks {
		from, ok1 := rec.resolveBound(c.from, false)
		to, ok2 := rec.resolveBound(c.to, true)
		if !ok1 || !ok2 {
			return inconclusive(cs, cfg, "an event-relative window never resolved: a fixture didn't start")
		}
		level := toneLevelDB(segment(m, sampleRate, from, to), sampleRate, c.freq)
		if c.expect == expectPresent && level < pMin {
			pMin = level
		}
		pend = append(pend, pending{spec: c, from: from, to: to, level: level})
	}
	for _, p := range pend {
		rep := checkReport{Freq: p.spec.freq, From: p.from, To: toReported(p.spec.to, p.to), Expect: p.spec.expect,
			LevelDB: roundDB(p.level)}
		switch p.spec.expect {
		case expectPresent:
			rep.Pass = p.level >= presentMinDB
		case expectAbsent:
			maxShort, atS := shortWindow(segment(m, sampleRate, p.from, p.to), sampleRate, p.spec.freq)
			rep.MaxShortDB = roundDB(maxShort)
			rep.MaxShortAtS = round3(p.from + atS)
			rep.Pass = p.level <= absentMaxDB && p.level <= pMin-absentMarginDB && maxShort <= absentMaxDB
		default: // info
			rep.Pass = true
		}
		if !rep.Pass {
			fail = true
		}
		cr.Checks = append(cr.Checks, rep)

		// Positive control for the check's fixture role, over the check's window (05 §18.6).
		if problem := rec.controlProblem(p.spec, p.from, p.to); problem != "" {
			cr.Problems = append(cr.Problems, problem)
			fail = true
		}
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

	// Engine and banner expectations in every sample (05 §18.6).
	if cs.engineThroughout != "" {
		for _, e := range rec.engineAt {
			if e.value != cs.engineThroughout {
				cr.Problems = append(cr.Problems, fmt.Sprintf("engine was %q at %.2fs, want %q throughout", e.value,
					e.t, cs.engineThroughout))
				fail = true
				break
			}
		}
	}
	if cs.bannerThroughout != "" {
		for _, b := range rec.bannerAt {
			if !slices.Contains(b.codes, cs.bannerThroughout) {
				cr.Problems = append(cr.Problems, fmt.Sprintf("banner %q missing from the sample at %.2fs",
					cs.bannerThroughout, b.t))
				fail = true
				break
			}
		}
	}

	// Measured cut/onset and the driver's session-lag and fast-path-exit values (05 §17, §18.6).
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

	// Counters that must be zero (case 5's fastpathLateCuts).
	for _, key := range cs.counterZero {
		if rec.counters[key] != 0 {
			cr.Problems = append(cr.Problems, fmt.Sprintf("counter %s is %d, want 0", key, rec.counters[key]))
			fail = true
		}
	}

	cr.Result = resultPass
	if fail {
		cr.Result = resultFail
	}
	cr.Result = effectiveResult(cs, cfg, cr.Result)
	return cr
}

// controlProblem checks the positive control a check implies about its fixture role over [from, to] (05 §18.6):
// every sample where the role is active must have the expected verdict (and reason and detail when given). It
// returns "" when the control holds, else a redacted problem.
func (rec recording) controlProblem(c checkSpec, from, to float64) string {
	if c.role == "" {
		return "" // a harness-played frequency has no fixture row
	}
	want := c.ctl
	if want == "" {
		switch c.expect {
		case expectPresent:
			want = controlHeard
		case expectAbsent:
			want = controlKeptOut
		default:
			return "" // an info check without an explicit control asserts nothing about the row
		}
	}
	tr := rec.roles[c.role]
	if tr == nil {
		return "a fixture never appeared in the report"
	}
	for _, s := range tr.samples {
		if s.t < from || (to > 0 && s.t > to) || !s.active {
			continue
		}
		if s.verdict != want {
			return fmt.Sprintf("a fixture was %q in its window, want %q", s.verdict, want)
		}
		if c.reason != "" && s.reason != c.reason {
			return fmt.Sprintf("a fixture's reason was %q, want %q", s.reason, c.reason)
		}
		if c.detail != "" && !strings.EqualFold(s.detail, c.detail) {
			return fmt.Sprintf("a fixture's detail was %q, want %q", s.detail, c.detail)
		}
	}
	return ""
}

// measureValue is the measured value in ms for ms, and whether it could be measured (05 §18.6).
func (rec recording) measureValue(ms measureSpec, m []float64) (int, bool) {
	switch ms.kind {
	case measureFromDriver:
		v, ok := rec.driverMeas[ms.key]
		return v, ok
	default: // measureFromAudio
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
}

// playingRoles are the fixture roles a case plays a tone from (a check with a role): each must appear active.
func playingRoles(cs caseSpec) []string {
	var out []string
	for _, c := range cs.checks {
		if c.role != "" && !slices.Contains(out, c.role) {
			out = append(out, c.role)
		}
	}
	return out
}

// enginesSeen is the distinct engine values across the report samples, in first-seen order.
func enginesSeen(rec recording) []string {
	var out []string
	for _, e := range rec.engineAt {
		if e.value != "" && !slices.Contains(out, e.value) {
			out = append(out, e.value)
		}
	}
	return out
}

// redactEvents maps engine events to redacted report events (05 §18.7): the time in ms, the code, and the fixture
// role (already redacted to "fixture:<role>" by the driver, or "").
func redactEvents(events []recEvent) []reportEvent {
	out := make([]reportEvent, 0, len(events))
	for _, e := range events {
		app := ""
		if e.role != "" {
			app = fixturePrefix + e.role
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

// firstNonZero returns b when it is set, else def (used for the click window's default start).
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
