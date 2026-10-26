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

// TestCaseTableCoverage checks the WA62a case table holds exactly the cases this slice owns.
func TestCaseTableCoverage(t *testing.T) {
	want := []int{1, 2, 3, 4, 5, 6, 7, 8, 10, 11, 15, 16, 17, 18, 19, 20, 21, 24, 25, 26, 27}
	got := cases()
	if len(got) != len(want) {
		t.Fatalf("case table has %d cases, want %d (WA62b adds 9, 12–14, 22, 23)", len(got), len(want))
	}
	for i, cs := range got {
		if cs.num != want[i] {
			t.Fatalf("case %d is %d, want %d (table must be in number order)", i, cs.num, want[i])
		}
		if cs.name == "" {
			t.Fatalf("case %d has no name", cs.num)
		}
	}
}
