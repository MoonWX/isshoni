package main

import "testing"

func TestRequiredUnder(t *testing.T) {
	fast, _ := caseByName("fast-path")
	yes, _ := caseByName("two-voice-apps")
	info, _ := caseByName("pid-reuse-tree")

	if !yes.requiredUnder(gateConfig{FastPath: false}) {
		t.Fatal("a reqYes case must always be required")
	}
	if fast.requiredUnder(gateConfig{FastPath: false}) {
		t.Fatal("a fast-path case must not be required when fastPath is off")
	}
	if !fast.requiredUnder(gateConfig{FastPath: true}) {
		t.Fatal("a fast-path case must be required when fastPath is on")
	}
	if info.requiredUnder(gateConfig{FastPath: true}) {
		t.Fatal("an INFO case is never required")
	}
}

func TestExitCode(t *testing.T) {
	tests := []struct {
		name string
		in   []caseReport
		want int
	}{
		{"all pass", []caseReport{{Required: true, Result: resultPass}}, exitAllPass},
		{"a fail wins", []caseReport{
			{Required: true, Result: resultInconclusive}, {Required: true, Result: resultFail},
		}, exitFail},
		{"a skip over inconclusive", []caseReport{
			{Required: true, Result: resultInconclusive}, {Required: true, Result: resultSkip},
		}, exitSkipped},
		{"inconclusive", []caseReport{{Required: true, Result: resultInconclusive}}, exitInconclusive},
		{"non-required ignored", []caseReport{
			{Required: false, Result: resultFail}, {Required: true, Result: resultPass},
		}, exitAllPass},
		{"info ignored", []caseReport{{Required: false, Result: resultInfo}}, exitAllPass},
	}
	for _, tc := range tests {
		if got := exitCode(tc.in); got != tc.want {
			t.Errorf("%s: exit %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestEffectiveResult(t *testing.T) {
	fast, _ := caseByName("fast-path")
	yes, _ := caseByName("two-voice-apps")

	// A fast-path case the gate doesn't require is reported as INFO whatever it measured.
	if got := effectiveResult(fast, gateConfig{FastPath: false}, resultFail); got != resultInfo {
		t.Fatalf("fast-path FAIL under fastPath off: got %s, want INFO", got)
	}
	if got := effectiveResult(fast, gateConfig{FastPath: true}, resultFail); got != resultFail {
		t.Fatalf("fast-path FAIL under fastPath on: got %s, want FAIL", got)
	}
	if got := effectiveResult(yes, gateConfig{FastPath: false}, resultFail); got != resultFail {
		t.Fatalf("reqYes FAIL: got %s, want FAIL", got)
	}
}

// TestShippedFastPathDefault confirms the gate follows the shipped default of audio.fastPath (05 §18.10): the
// built-in default is on.
func TestShippedFastPathDefault(t *testing.T) {
	if !shippedFastPath() {
		t.Fatal("the shipped audio.fastPath default is expected to be true in M2")
	}
}

// TestCaseTableCoverage checks the case table holds every case of 05 §18.5, in number order, with §18.5's name and
// "Required" column: 24 required (three of them with the fast path, four needing a mic), two INFO, the soak a
// minor-release tier case. With the shipped fastPath on, the 24 count toward the exit code, as §18.7's example
// summary (pass 24, info 2) says.
func TestCaseTableCoverage(t *testing.T) {
	want := []struct {
		name string
		req  requirement
	}{
		{"baseline-endpoint", reqYes}, {"builtin-voice", reqYes}, {"two-voice-apps", reqYes},
		{"fast-path", reqFastPath}, {"fast-path-late-voice", reqFastPath}, {"helper-tree", reqYes},
		{"late-helper", reqYes}, {"late-app", reqYes}, {"steam-game", reqMic}, {"isshoni-self", reqYes},
		{"viewer-browser", reqYes}, {"browser-mic", reqMic}, {"mic-user", reqMic}, {"shared-app-mic", reqMic},
		{"runtime-rule", reqYes}, {"app-boundary", reqYes}, {"tree-contains-excluded", reqYes}, {"orphan", reqYes},
		{"probe-fallback", reqYes}, {"timestamps", reqYes}, {"opus", reqYes}, {"multi-endpoint", reqInfo},
		{"soak", reqTierMinor}, {"viewer-browser-included", reqYes}, {"late-child-heard", reqYes},
		{"fast-path-late-child", reqFastPath}, {"pid-reuse-tree", reqInfo},
	}
	got := cases()
	if len(got) != len(want) {
		t.Fatalf("case table has %d cases, want %d", len(got), len(want))
	}
	required := 0
	for i, cs := range got {
		if cs.num != i+1 || cs.name != want[i].name || cs.req != want[i].req {
			t.Fatalf("case at %d is %d %q (req %d), want %d %q (req %d)", i, cs.num, cs.name, cs.req, i+1,
				want[i].name, want[i].req)
		}
		if cs.requiredUnder(gateConfig{FastPath: true}) {
			required++
		}
	}
	if required != 24 {
		t.Fatalf("%d required cases with the fast path on, want 24", required)
	}
}
