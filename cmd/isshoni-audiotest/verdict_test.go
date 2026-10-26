package main

import (
	"math"
	"strings"
	"testing"
)

// These tests are 05 §19.1's verdict tests on synthetic recordings: the positive controls, every INCONCLUSIVE
// reason, case 5's session-lag rule, event-relative windows, the window of the engine and banner expectations, and
// the gate configuration. They build a recording directly (no engine), so they run on every OS.

// mustCase returns the case by name or fails.
func mustCase(t *testing.T, name string) caseSpec {
	t.Helper()
	cs, ok := caseByName(name)
	if !ok {
		t.Fatalf("no case %q in the table", name)
	}
	return cs
}

// trackActive is a role track whose session is active with the same verdict and reason throughout a dur-second
// recording (its app row the same), sampled every 250 ms.
func trackActive(verdict, reason string, dur float64) *roleTrack {
	tr := &roleTrack{}
	for t := 0.0; t <= dur+1e-9; t += 0.25 {
		tr.samples = append(tr.samples, roleSampleState{t: t, active: true, verdict: verdict, reason: reason,
			row: true, rowVerdict: verdict, rowReason: reason})
	}
	return tr
}

func enginesEvery(value string, dur float64) []engineSample {
	var out []engineSample
	for t := 0.0; t <= dur+1e-9; t += 0.25 {
		out = append(out, engineSample{t: t, value: value})
	}
	return out
}

// baseRec is a clean recording of dur seconds with the given mono signal and role tracks.
func baseRec(sig []float64, dur float64, roles map[string]*roleTrack, engine string) recording {
	n := int(dur * sampleRate)
	sig = sig[:n]
	return recording{
		left: sig, right: sig, recordDur: dur, reparented: true,
		roles: roles, roleApp: map[string]string{}, roleStart: map[string]float64{},
		engineAt:    enginesEvery(engine, dur),
		firstRender: map[string]float64{},
		counters:    map[string]int{},
	}
}

// recForCase2 builds a clean, passing recording for the builtin-voice case: 1 kHz heard, 440 Hz kept out.
func recForCase2(dur float64) recording {
	return baseRec(fadedTone(freqGame, dbToAmp(-18), dur, 0.02), dur, map[string]*roleTrack{
		"game":  trackActive("heard", "", dur),
		"voice": trackActive("kept_out", "builtin", dur),
	}, "include-set")
}

func TestEvaluatePasses(t *testing.T) {
	cs := mustCase(t, "builtin-voice")
	cr := evaluate(cs, recForCase2(8), gateConfig{FastPath: true})
	if cr.Result != resultPass {
		t.Fatalf("want PASS, got %s: %v", cr.Result, cr.Problems)
	}
	if len(cr.Checks) != 2 {
		t.Fatalf("want 2 checks, got %d", len(cr.Checks))
	}
	// maxShortAtS is the centre of the loudest 50 ms window (05 §18.6), so never before the window's start + 25 ms.
	if c := cr.Checks[1]; c.MaxShortAtS < c.From+shortWindowSec/2 {
		t.Fatalf("maxShortAtS %.3f is not a window centre (from %.3f)", c.MaxShortAtS, c.From)
	}
}

// TestPositiveControls is 05 §18.6's controls: a fixture that never appeared active, and one that exited early, are
// INCONCLUSIVE; a fixture classified against its check's expectation FAILs; a present fixture may be pending or
// heard but never kept out; the app row's reason is checked too.
func TestPositiveControls(t *testing.T) {
	cs := mustCase(t, "builtin-voice")
	cfg := gateConfig{FastPath: true}

	never := recForCase2(8)
	never.roles["voice"].neverActive = true
	if cr := evaluate(cs, never, cfg); cr.Result != resultInconclusive {
		t.Fatalf("never-active fixture: want INCONCLUSIVE, got %s", cr.Result)
	}

	early := recForCase2(8)
	early.roles["game"].exitedEarly = true
	if cr := evaluate(cs, early, cfg); cr.Result != resultInconclusive {
		t.Fatalf("early-exit fixture: want INCONCLUSIVE, got %s", cr.Result)
	}

	// The voice app must be kept out; if a sample in the window shows it heard, the control FAILs.
	wrong := recForCase2(8)
	wrong.roles["voice"].samples[8].verdict = "heard"
	if cr := evaluate(cs, wrong, cfg); cr.Result != resultFail {
		t.Fatalf("voice heard in a sample: want FAIL, got %s", cr.Result)
	}
	// The same sample before the window (t = 1.0 s) doesn't count.
	before := recForCase2(8)
	before.roles["voice"].samples[4].verdict = "heard"
	if cr := evaluate(cs, before, cfg); cr.Result != resultPass {
		t.Fatalf("voice heard at 1.0 s (before the window): want PASS, got %s: %v", cr.Result, cr.Problems)
	}
	// The wrong session reason FAILs.
	badReason := recForCase2(8)
	badReason.roles["voice"].samples[8].reason = "mic"
	if cr := evaluate(cs, badReason, cfg); cr.Result != resultFail {
		t.Fatalf("voice reason mic (want builtin): want FAIL, got %s", cr.Result)
	}
	// The Discord row must be kept_out/builtin (05 §18.5 case 2's row check).
	badRow := recForCase2(8)
	badRow.roles["voice"].samples[10].rowReason = "user"
	if cr := evaluate(cs, badRow, cfg); cr.Result != resultFail || !strings.Contains(cr.Problems[0], "row") {
		t.Fatalf("Discord row reason user: want FAIL naming the row, got %s: %v", cr.Result, cr.Problems)
	}
	// A present fixture that is pending in one sample is not kept out: the control holds (05 §18.6).
	pending := recForCase2(8)
	pending.roles["game"].samples[9].verdict = "pending"
	if cr := evaluate(cs, pending, cfg); cr.Result != resultPass {
		t.Fatalf("game pending in one sample: want PASS, got %s: %v", cr.Result, cr.Problems)
	}
	keptGame := recForCase2(8)
	keptGame.roles["game"].samples[9].verdict = "kept_out"
	if cr := evaluate(cs, keptGame, cfg); cr.Result != resultFail {
		t.Fatalf("game kept out in a present window: want FAIL, got %s", cr.Result)
	}
	// An inactive sample asserts nothing.
	idle := recForCase2(8)
	idle.roles["voice"].samples[9].active, idle.roles["voice"].samples[9].verdict = false, "heard"
	if cr := evaluate(cs, idle, cfg); cr.Result != resultPass {
		t.Fatalf("an inactive voice sample: want PASS, got %s: %v", cr.Result, cr.Problems)
	}
}

// TestInconclusiveReasons is 05 §18.6's INCONCLUSIVE causes: fixtures not reparented, silence pre-check failed, lost
// audio, another app heard during the window, a kept-out non-fixture process in a fast-path case (05 §18.4), and an
// event-relative window or measured event that never happened.
func TestInconclusiveReasons(t *testing.T) {
	cs := mustCase(t, "builtin-voice")
	cfg := gateConfig{FastPath: true}

	cases := []struct {
		name string
		mod  func(r *recording)
		want string
	}{
		{"not reparented", func(r *recording) { r.reparented = false }, "reparented"},
		{"silence pre-check", func(r *recording) { r.silenceFail = true }, "silence"},
		{"lost audio", func(r *recording) { r.lostGapMs = 45 }, "lost audio"},
		{"another app heard", func(r *recording) { r.foreign = []foreignSample{{t: 3, app: otherRedacted}} },
			"another app"},
	}
	for _, tc := range cases {
		r := recForCase2(8)
		tc.mod(&r)
		cr := evaluate(cs, r, cfg)
		if cr.Result != resultInconclusive {
			t.Fatalf("%s: want INCONCLUSIVE, got %s", tc.name, cr.Result)
		}
		if len(cr.Problems) == 0 || !strings.Contains(cr.Problems[0], tc.want) {
			t.Fatalf("%s: problem %q doesn't mention %q", tc.name, cr.Problems, tc.want)
		}
	}

	// Another app heard before the window (t = 1.0 s) is not a premise failure.
	r := recForCase2(8)
	r.foreign = []foreignSample{{t: 1.0, app: otherRedacted}}
	if cr := evaluate(cs, r, cfg); cr.Result != resultPass {
		t.Fatalf("an app heard before the window: want PASS, got %s: %v", cr.Result, cr.Problems)
	}
	// A kept-out real voice app matters only for the fast-path cases.
	r = recForCase2(8)
	r.keptOutOther = true
	if cr := evaluate(cs, r, cfg); cr.Result != resultPass {
		t.Fatalf("a kept-out non-fixture in case 2: want PASS, got %s", cr.Result)
	}
	fp := recForCase5(12, 5, 0)
	fp.keptOutOther = true
	if cr := evaluate(mustCase(t, "fast-path-late-voice"), fp, cfg); cr.Result != resultInconclusive {
		t.Fatalf("a kept-out non-fixture in case 5: want INCONCLUSIVE, got %s", cr.Result)
	}

	// An event-relative window (case 15's T_set) that never resolved is INCONCLUSIVE.
	rc := mustCase(t, "runtime-rule")
	if cr := evaluate(rc, recForRuntimeRule(8, false /* no SetExclusions stamp */), cfg); cr.Result != resultInconclusive {
		t.Fatalf("missing T_set: want INCONCLUSIVE, got %s: %v", cr.Result, cr.Problems)
	}
	// Case 5 without the voice app's session_created can't measure the lag: INCONCLUSIVE ("fixture didn't start").
	noSession := recForCase5(12, 5, 0)
	noSession.events = nil
	if cr := evaluate(mustCase(t, "fast-path-late-voice"), noSession, cfg); cr.Result != resultInconclusive {
		t.Fatalf("no session_created: want INCONCLUSIVE, got %s: %v", cr.Result, cr.Problems)
	}
}

// recForRuntimeRule builds a case-15 recording: 440 Hz present until ~4.1 s, then kept out after T_set (~4.05 s).
func recForRuntimeRule(dur float64, withStamp bool) recording {
	n := int(dur * sampleRate)
	game := fadedTone(freqGame, dbToAmp(-18), dur, 0.02)
	voice := make([]float64, n)
	copyInto(voice, fadedTone(freqVoice, dbToAmp(-18), 4.1, 0.01), 0)
	vt := &roleTrack{}
	for t := 0.0; t <= dur+1e-9; t += 0.25 {
		v := "heard"
		if t >= 4.1 {
			v = "kept_out"
		}
		vt.samples = append(vt.samples, roleSampleState{t: t, active: true, verdict: v})
	}
	r := baseRec(mixTones(game[:n], voice), dur, map[string]*roleTrack{
		"game":      trackActive("heard", "", dur),
		"fakevoice": vt,
	}, "include-set")
	if withStamp {
		r.setStamp, r.setStampOK = 4.05, true
	}
	return r
}

func TestRuntimeRulePasses(t *testing.T) {
	cs := mustCase(t, "runtime-rule")
	cr := evaluate(cs, recForRuntimeRule(8, true), gateConfig{FastPath: true})
	if cr.Result != resultPass {
		t.Fatalf("want PASS, got %s: %v", cr.Result, cr.Problems)
	}
	if m, ok := cr.Measured[measureCut]; !ok || m.BudgetMs != budgetCutMs || m.ValueMs > budgetCutMs {
		t.Fatalf("cut measure %+v (ok=%v), want ≤ %d ms", m, ok, budgetCutMs)
	}
	// The absent window starts at the stamp + 100 ms (an event-relative window from the recorder's stamp).
	if got := cr.Checks[2].From; got != 4.15 {
		t.Fatalf("the absent window starts at %.3f, want T_set + 0.1 = 4.150", got)
	}
}

// recForCase5 builds a fast-path-late-voice recording: 1 kHz heard throughout, 440 kept out, the voice app (app id
// "discord") started at 2.98 s, logging its first render at 3.0 s, with its session_created sessionLagMs later and
// a fastpath_exit fastpathExitMs after that. The engine events also hold what a real report repeats: an older
// session_created of the same app id (an earlier case's, before the voice started) and the game's.
func recForCase5(sessionLagMs, fastpathExitMs, lateCuts int) recording {
	dur := 8.0
	voiceTrack := &roleTrack{}
	for t := 0.0; t <= dur+1e-9; t += 0.25 {
		voiceTrack.samples = append(voiceTrack.samples, roleSampleState{t: t, active: t >= 3.25, verdict: "kept_out"})
	}
	r := baseRec(fadedTone(freqGame, dbToAmp(-18), dur, 0.02), dur, map[string]*roleTrack{
		"game":  trackActive("heard", "", dur),
		"voice": voiceTrack,
	}, "exclude-self")
	created := 3.0 + float64(sessionLagMs)/1000
	r.roleApp = map[string]string{"game": "exe:game.exe", "voice": "discord"}
	r.roleStart = map[string]float64{"game": -1.0, "voice": 2.98}
	r.firstRender = map[string]float64{"voice": 3.0}
	r.events = []recEvent{
		{t: -0.9, code: "session_created", app: "exe:game.exe"},
		{t: -0.5, code: "session_created", app: "discord"},
		{t: -0.4, code: "fastpath_exit"},
		{t: created, code: "session_created", app: "discord"},
		{t: created + float64(fastpathExitMs)/1000, code: "fastpath_exit"},
	}
	r.counters = map[string]int{"fastpathLateCuts": lateCuts}
	return r
}

// TestCase5SessionLag is 05 §19.1's "case 5's session-lag rule (21 ms → FAIL)", measured from the voice app's own
// session_created (not an earlier one of the same app id, nor the game's) and its logged firstRenderNs.
func TestCase5SessionLag(t *testing.T) {
	cs := mustCase(t, "fast-path-late-voice")
	cfg := gateConfig{FastPath: true}

	cr := evaluate(cs, recForCase5(21, 10, 0), cfg)
	if cr.Result != resultFail || cr.Measured[measureSessionLag].ValueMs != 21 {
		t.Fatalf("21 ms session lag: want FAIL with 21 ms, got %s %+v: %v", cr.Result, cr.Measured, cr.Problems)
	}
	cr = evaluate(cs, recForCase5(19, 10, 0), cfg)
	if cr.Result != resultPass {
		t.Fatalf("19 ms session lag: want PASS, got %s: %v", cr.Result, cr.Problems)
	}
	if m := cr.Measured; m[measureSessionLag].ValueMs != 19 || m[measureFastpathExit].ValueMs != 10 {
		t.Fatalf("measured %+v, want sessionLag 19 and fastpathExit 10", m)
	}
	// fastpathLateCuts must be 0.
	if cr := evaluate(cs, recForCase5(10, 10, 1), cfg); cr.Result != resultFail {
		t.Fatalf("fastpathLateCuts 1: want FAIL, got %s", cr.Result)
	}
	// Over the 15 ms fast-path-exit budget FAILs.
	if cr := evaluate(cs, recForCase5(10, 16, 0), cfg); cr.Result != resultFail {
		t.Fatalf("16 ms fastpath exit: want FAIL, got %s", cr.Result)
	}
	// No fastpath_exit after the voice app started: the fast path never exited, a FAIL.
	noExit := recForCase5(10, 10, 0)
	noExit.events = noExit.events[:4]
	if cr := evaluate(cs, noExit, cfg); cr.Result != resultFail {
		t.Fatalf("no fastpath_exit: want FAIL, got %s: %v", cr.Result, cr.Problems)
	}
}

// TestGateConfigFastPathFalse is 05 §19.1's "shipped fastPath false → cases 4, 5, 26 INFO, exit code unaffected by
// their result".
func TestGateConfigFastPathFalse(t *testing.T) {
	cs := mustCase(t, "fast-path-late-voice")

	// A result that would FAIL (21 ms session lag) becomes INFO under a gate that ships fastPath false, and the case
	// does not count toward the exit code.
	crOff := evaluate(cs, recForCase5(21, 10, 0), gateConfig{FastPath: false})
	if crOff.Result != resultInfo {
		t.Fatalf("fastPath false: want INFO, got %s", crOff.Result)
	}
	if crOff.Required {
		t.Fatalf("fastPath false: case 5 must not be required")
	}
	if got := exitCode([]caseReport{crOff}); got != exitAllPass {
		t.Fatalf("fastPath false: exit %d, want %d (unaffected)", got, exitAllPass)
	}
	// An INCONCLUSIVE premise is INFO too (the summary must not count it as inconclusive).
	lost := recForCase5(10, 10, 0)
	lost.lostGapMs = 60
	if cr := evaluate(cs, lost, gateConfig{FastPath: false}); cr.Result != resultInfo {
		t.Fatalf("fastPath false, lost audio: want INFO, got %s", cr.Result)
	}

	// With fastPath true the same result FAILs and the exit is 1.
	crOn := evaluate(cs, recForCase5(21, 10, 0), gateConfig{FastPath: true})
	if crOn.Result != resultFail || !crOn.Required {
		t.Fatalf("fastPath true: want required FAIL, got required=%v %s", crOn.Required, crOn.Result)
	}
	if got := exitCode([]caseReport{crOn}); got != exitFail {
		t.Fatalf("fastPath true: exit %d, want %d", got, exitFail)
	}
}

// recForEndpoint builds a case 1 / case 19 recording on a correct endpoint engine: both tones heard, while the
// planner keeps Discord's policy verdict (kept_out/builtin, audiopolicy_planner_test's "the policy's verdict; the
// engine says endpoint"). The engine reads "none" at t = 0 and the include set at 0.25 s (the probe still running,
// 05 §6.1), "endpoint" from 0.5 s; the banner appears at 0.5 s.
func recForEndpoint(dur float64) recording {
	sig := mixTones(fadedTone(freqGame, dbToAmp(-18), dur, 0.02), fadedTone(freqVoice, dbToAmp(-18), dur, 0.02))
	r := baseRec(sig, dur, map[string]*roleTrack{
		"game":    trackActive("heard", "", dur),
		"voice":   trackActive("kept_out", "builtin", dur),
		"discord": trackActive("kept_out", "builtin", dur),
	}, "endpoint")
	r.engineAt[0].value, r.engineAt[1].value = "none", "include-set"
	for t := 0.0; t <= dur+1e-9; t += 0.25 {
		var codes []string
		if t >= 0.5 {
			codes = []string{"exclusion-unavailable"}
		}
		r.bannerAt = append(r.bannerAt, bannerSample{t: t, codes: codes})
	}
	return r
}

// TestEndpointCases: cases 1 and 19 PASS on a correct endpoint engine though Discord's row stays kept_out (the
// engine and the banner say "endpoint", the sound proves it), and the engine and banner expectations hold only
// inside the window [1.5 s, end] (05 §18.6), not right after Start.
func TestEndpointCases(t *testing.T) {
	cfg := gateConfig{FastPath: true}
	for _, name := range []string{"baseline-endpoint", "probe-fallback"} {
		cs := mustCase(t, name)
		if cr := evaluate(cs, recForEndpoint(8), cfg); cr.Result != resultPass {
			t.Fatalf("%s: want PASS, got %s: %v", name, cr.Result, cr.Problems)
		}
		// An engine other than endpoint inside the window FAILs.
		r := recForEndpoint(8)
		r.engineAt[10].value = "include-set" // t = 2.5 s
		if cr := evaluate(cs, r, cfg); cr.Result != resultFail {
			t.Fatalf("%s: include-set at 2.5 s: want FAIL, got %s", name, cr.Result)
		}
	}
	// Case 19's banner must hold in every sample of the window.
	r := recForEndpoint(8)
	r.bannerAt[12].codes = nil // t = 3.0 s
	if cr := evaluate(mustCase(t, "probe-fallback"), r, cfg); cr.Result != resultFail {
		t.Fatalf("banner missing at 3.0 s: want FAIL, got %s", cr.Result)
	}
}

// TestEventRelativeBounds is 05 §19.1's "event-relative windows resolve from firstRenderNs and recorder stamps": a
// bound naming a role resolves from its logged first render, one naming T_set from the recorder's stamp, one naming
// "session:<role>" from the role's own session_created; a missing event doesn't resolve.
func TestEventRelativeBounds(t *testing.T) {
	r := recForCase5(12, 5, 0)
	r.setStamp, r.setStampOK = 4.05, true
	if got, ok := r.resolveBound(bound{event: "voice", offset: 0.1}, false); !ok || math.Abs(got-3.1) > 1e-9 {
		t.Fatalf("firstRender bound: %v %v, want 3.1", got, ok)
	}
	if got, ok := r.resolveBound(afterTSet(0.1), false); !ok || got < 4.149 || got > 4.151 {
		t.Fatalf("T_set bound: %v %v, want 4.15", got, ok)
	}
	if got, ok := r.resolveBound(bound{event: sessionPrefix + "voice"}, false); !ok || math.Abs(got-3.012) > 1e-9 {
		t.Fatalf("session bound: %v %v, want 3.012 (the voice app's own session_created)", got, ok)
	}
	if _, ok := r.resolveBound(bound{event: "helper"}, false); ok {
		t.Fatal("a role that never logged a first render resolved")
	}
	// A check whose window starts at a first render that never happened is INCONCLUSIVE.
	cs := mustCase(t, "builtin-voice")
	cs.checks[1].from = bound{event: "voice"}
	rec := recForCase2(8)
	if cr := evaluate(cs, rec, gateConfig{FastPath: true}); cr.Result != resultInconclusive {
		t.Fatalf("no firstRender: want INCONCLUSIVE, got %s", cr.Result)
	}
	rec.firstRender["voice"] = 2.0
	cr := evaluate(cs, rec, gateConfig{FastPath: true})
	if cr.Result != resultPass || cr.Checks[1].From != 2.0 {
		t.Fatalf("firstRender at 2.0 s: want PASS from 2.000, got %s from %.3f: %v", cr.Result, cr.Checks[1].From,
			cr.Problems)
	}
}

// TestRowChecks: case 17's launcher row must be not_captured/tree naming Discord, and case 25's late child must be
// covered by the game's stream with the game's row heard (05 §18.5).
func TestRowChecks(t *testing.T) {
	cfg := gateConfig{FastPath: true}
	dur := 8.0
	sig := fadedTone(freqGame, dbToAmp(-18), dur, 0.02)

	launcher := trackActive("not_captured", "tree", dur)
	for i := range launcher.samples {
		launcher.samples[i].rowDetail = "Discord"
	}
	r17 := baseRec(sig, dur, map[string]*roleTrack{
		"game": trackActive("heard", "", dur), "discord": trackActive("kept_out", "builtin", dur),
		"launcherx": launcher,
	}, "include-set")
	cs17 := mustCase(t, "tree-contains-excluded")
	if cr := evaluate(cs17, r17, cfg); cr.Result != resultPass {
		t.Fatalf("case 17: want PASS, got %s: %v", cr.Result, cr.Problems)
	}
	launcher.samples[12].rowDetail = "Steam"
	if cr := evaluate(cs17, r17, cfg); cr.Result != resultFail {
		t.Fatalf("case 17 with the wrong detail: want FAIL, got %s", cr.Result)
	}

	sig25 := make([]float64, int(dur*sampleRate))
	copyInto(sig25, fadedTone(freqGame, dbToAmp(-18), dur, 0.02), 0)
	copyInto(sig25, fadedTone(freqLateGame, dbToAmp(-18), dur-3.2, 0.02), int(3.2*sampleRate))
	child := &roleTrack{}
	for t := 3.25; t <= dur+1e-9; t += 0.25 {
		child.samples = append(child.samples, roleSampleState{t: t, active: true, verdict: "heard", note: "covered",
			row: true, rowVerdict: "heard"})
	}
	r25 := baseRec(sig25, dur, map[string]*roleTrack{"game": trackActive("heard", "", dur), "gameChild": child},
		"include-set")
	r25.roleApp = map[string]string{"game": "exe:game.exe", "gameChild": "exe:game.exe"}
	r25.roleStart = map[string]float64{"game": -1, "gameChild": 3.0}
	r25.events = []recEvent{
		{t: -0.9, code: "session_created", app: "exe:game.exe"},
		{t: 3.21, code: "session_created", app: "exe:game.exe"},
	}
	cs25 := mustCase(t, "late-child-heard")
	cr := evaluate(cs25, r25, cfg)
	if cr.Result != resultPass {
		t.Fatalf("case 25: want PASS, got %s: %v", cr.Result, cr.Problems)
	}
	if v := cr.Measured[measureOnset].ValueMs; v < -20 || v > 100 {
		t.Fatalf("case 25 onset %d ms from the child's own session_created, want near 0", v)
	}
	child.samples[5].note = ""
	if cr := evaluate(cs25, r25, cfg); cr.Result != resultFail {
		t.Fatalf("case 25 without the covered note: want FAIL, got %s", cr.Result)
	}
}

// TestRedactedEvents: events name fixtures by role through their app ids, any other app as "other", and an
// engine-wide event with no app (05 §18.7).
func TestRedactedEvents(t *testing.T) {
	got := redactEvents([]recEvent{
		{t: 1.21, code: "stream_running", app: "exe:game.exe"},
		{t: 1.5, code: "session_created", app: "exe:spotify.exe"},
		{t: 2, code: "fastpath_enter"},
	}, map[string]string{"game": "exe:game.exe", "gameChild": "exe:game.exe"})
	want := []reportEvent{
		{TMs: 1210, Code: "stream_running", App: "fixture:game"},
		{TMs: 1500, Code: "session_created", App: otherRedacted},
		{TMs: 2000, Code: "fastpath_enter", App: ""},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d: %+v, want %+v", i, got[i], want[i])
		}
	}
}
