package main

import (
	"strings"
	"testing"
)

// These tests are 05 §19.1's verdict tests on synthetic recordings: the positive controls, every INCONCLUSIVE
// reason, case 5's session-lag rule, and the gate configuration. They build a recording directly (no engine), so
// they run on every OS.

// mustCase returns the case by name or fails.
func mustCase(t *testing.T, name string) caseSpec {
	t.Helper()
	cs, ok := caseByName(name)
	if !ok {
		t.Fatalf("no case %q in the table", name)
	}
	return cs
}

// recForCase2 builds a clean, passing recording for the builtin-voice case: 1 kHz heard, 440 Hz kept out.
func recForCase2(dur float64) recording {
	n := int(dur * sampleRate)
	left := fadedTone(freqGame, dbToAmp(-18), dur, 0.02) // the game heard
	right := left
	roles := map[string]*roleTrack{
		"game":  trackActive("heard", "", "", dur),
		"voice": trackActive("kept_out", "builtin", "", dur),
	}
	return recording{
		left: left[:n], right: right[:n], recordDur: dur, reparented: true,
		roles:       roles,
		engineAt:    enginesEvery("include-set", dur),
		firstRender: map[string]float64{},
		driverMeas:  map[string]int{},
		counters:    map[string]int{},
	}
}

// trackActive is a role track that is active with the same verdict/reason throughout a dur-second recording, sampled
// every 250 ms.
func trackActive(verdict, reason, detail string, dur float64) *roleTrack {
	tr := &roleTrack{}
	for t := 0.0; t <= dur+1e-9; t += 0.25 {
		tr.samples = append(tr.samples, roleSampleState{t: t, active: true, verdict: verdict, reason: reason,
			detail: detail})
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

func TestEvaluatePasses(t *testing.T) {
	cs := mustCase(t, "builtin-voice")
	cr := evaluate(cs, recForCase2(8), gateConfig{FastPath: true})
	if cr.Result != resultPass {
		t.Fatalf("want PASS, got %s: %v", cr.Result, cr.Problems)
	}
	if len(cr.Checks) != 2 {
		t.Fatalf("want 2 checks, got %d", len(cr.Checks))
	}
}

// TestPositiveControls is 05 §18.6's controls: a fixture that never appeared active, and one that exited early, are
// INCONCLUSIVE; a fixture classified against its check's expectation (kept out when it should be) passes, and the
// wrong classification FAILs.
func TestPositiveControls(t *testing.T) {
	cs := mustCase(t, "builtin-voice")

	never := recForCase2(8)
	never.roles["voice"].neverActive = true
	if cr := evaluate(cs, never, gateConfig{FastPath: true}); cr.Result != resultInconclusive {
		t.Fatalf("never-active fixture: want INCONCLUSIVE, got %s", cr.Result)
	}

	early := recForCase2(8)
	early.roles["game"].exitedEarly = true
	if cr := evaluate(cs, early, gateConfig{FastPath: true}); cr.Result != resultInconclusive {
		t.Fatalf("early-exit fixture: want INCONCLUSIVE, got %s", cr.Result)
	}

	// The voice app must be kept out; if a sample shows it heard, the control FAILs.
	wrong := recForCase2(8)
	wrong.roles["voice"].samples[8].verdict = "heard"
	if cr := evaluate(cs, wrong, gateConfig{FastPath: true}); cr.Result != resultFail {
		t.Fatalf("voice heard in a sample: want FAIL, got %s", cr.Result)
	}
	// The wrong reason also FAILs.
	badReason := recForCase2(8)
	badReason.roles["voice"].samples[8].reason = "mic"
	if cr := evaluate(cs, badReason, gateConfig{FastPath: true}); cr.Result != resultFail {
		t.Fatalf("voice reason mic (want builtin): want FAIL, got %s", cr.Result)
	}
}

// TestInconclusiveReasons is 05 §18.6's INCONCLUSIVE causes: fixtures not reparented, silence pre-check failed, lost
// audio, another app heard, and an event-relative window that never resolved.
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
		{"another app heard", func(r *recording) { r.foreign = []string{otherRedacted} }, "another app"},
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

	// An event-relative window (case 15's T_set) that never resolved is INCONCLUSIVE.
	rc := mustCase(t, "runtime-rule")
	r := recForRuntimeRule(8, false /* no SetExclusions stamp */)
	if cr := evaluate(rc, r, cfg); cr.Result != resultInconclusive {
		t.Fatalf("missing T_set: want INCONCLUSIVE, got %s: %v", cr.Result, cr.Problems)
	}
}

// recForRuntimeRule builds a case-15 recording: 440 Hz present until ~4 s, then kept out after T_set (~4.05 s).
func recForRuntimeRule(dur float64, withStamp bool) recording {
	n := int(dur * sampleRate)
	game := fadedTone(freqGame, dbToAmp(-18), dur, 0.02)
	// fakevoice: a −18 dBFS 440 Hz tone that stops (10 ms fade) at 4.1 s.
	voice := make([]float64, n)
	copyInto(voice, fadedTone(freqVoice, dbToAmp(-18), 4.1, 0.02), 0)
	m := mixTones(game[:n], voice)

	// fakevoice is heard until 4.0 s, kept out after.
	vt := &roleTrack{}
	for t := 0.0; t <= dur+1e-9; t += 0.25 {
		v := "heard"
		if t >= 4.1 {
			v = "kept_out"
		}
		vt.samples = append(vt.samples, roleSampleState{t: t, active: true, verdict: v})
	}
	r := recording{
		left: m, right: m, recordDur: dur, reparented: true,
		roles: map[string]*roleTrack{
			"game":      trackActive("heard", "", "", dur),
			"fakevoice": vt,
		},
		engineAt:    enginesEvery("include-set", dur),
		firstRender: map[string]float64{},
		driverMeas:  map[string]int{},
		counters:    map[string]int{},
	}
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
}

// recForCase5 builds a fast-path-late-voice recording: 1 kHz heard throughout, 440 kept out, with driver-provided
// session-lag and fast-path-exit values.
func recForCase5(sessionLagMs, fastpathExitMs, lateCuts int) recording {
	dur := 8.0
	n := int(dur * sampleRate)
	game := fadedTone(freqGame, dbToAmp(-18), dur, 0.02)[:n]
	voiceTrack := &roleTrack{}
	for t := 0.0; t <= dur+1e-9; t += 0.25 {
		active := t >= 3.0
		voiceTrack.samples = append(voiceTrack.samples, roleSampleState{t: t, active: active, verdict: "kept_out"})
	}
	return recording{
		left: game, right: game, recordDur: dur, reparented: true,
		roles: map[string]*roleTrack{
			"game":  trackActive("heard", "", "", dur),
			"voice": voiceTrack,
		},
		engineAt:    enginesEvery("exclude-self", dur),
		firstRender: map[string]float64{"voice": 3.0},
		driverMeas:  map[string]int{measureSessionLag: sessionLagMs, measureFastpathExit: fastpathExitMs},
		counters:    map[string]int{"fastpathLateCuts": lateCuts},
	}
}

// TestCase5SessionLag is 05 §19.1's "case 5's session-lag rule (21 ms → FAIL)".
func TestCase5SessionLag(t *testing.T) {
	cs := mustCase(t, "fast-path-late-voice")
	cfg := gateConfig{FastPath: true}

	if cr := evaluate(cs, recForCase5(21, 10, 0), cfg); cr.Result != resultFail {
		t.Fatalf("21 ms session lag: want FAIL, got %s: %v", cr.Result, cr.Problems)
	}
	if cr := evaluate(cs, recForCase5(19, 10, 0), cfg); cr.Result != resultPass {
		t.Fatalf("19 ms session lag: want PASS, got %s: %v", cr.Result, cr.Problems)
	}
	// fastpathLateCuts must be 0.
	if cr := evaluate(cs, recForCase5(10, 10, 1), cfg); cr.Result != resultFail {
		t.Fatalf("fastpathLateCuts 1: want FAIL, got %s", cr.Result)
	}
	// Over the 15 ms fast-path-exit budget FAILs.
	if cr := evaluate(cs, recForCase5(10, 16, 0), cfg); cr.Result != resultFail {
		t.Fatalf("16 ms fastpath exit: want FAIL, got %s", cr.Result)
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

	// With fastPath true the same result FAILs and the exit is 1.
	crOn := evaluate(cs, recForCase5(21, 10, 0), gateConfig{FastPath: true})
	if crOn.Result != resultFail || !crOn.Required {
		t.Fatalf("fastPath true: want required FAIL, got required=%v %s", crOn.Required, crOn.Result)
	}
	if got := exitCode([]caseReport{crOn}); got != exitFail {
		t.Fatalf("fastPath true: exit %d, want %d", got, exitFail)
	}
}
