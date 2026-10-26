package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSummaryLine(t *testing.T) {
	r := report{
		Harness: "v0.2.0-14-g1a2b3c4",
		DLL:     dllInfo{Version: "0.2.0", SHA256: "3f9a1c0b7d2e9f00112233"},
		Gate:    true,
		Summary: summary{Pass: 21, Info: 2, Exit: 0},
		OS:      osInfo{Product: "win11", Build: 26200, UBR: 6899, Edition: "Professional"},
	}
	got := summaryLine(r)
	for _, want := range []string{
		"isshoni-audiotest v0.2.0-14-g1a2b3c4",
		"DLL 0.2.0 sha256:3f9a1c0b7d2e", // shortened to 12 hex
		"Windows 11 Pro 26200.6899",
		"21/21 PASS", "0 FAIL", "2 INFO", "gate=PASS", // INFO is not in the total (05 §18.7's 24/24 with 2 INFO)
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary line %q missing %q", got, want)
		}
	}

	// A failing gate reads gate=FAIL; a non-Windows diagnostic run has no DLL clause.
	r.Summary = summary{Pass: 20, Fail: 1, Exit: 1}
	if !strings.Contains(summaryLine(r), "gate=FAIL") {
		t.Error("a failing gate should read gate=FAIL")
	}
	r2 := report{Harness: "dev", OS: osInfo{Product: ""}, Summary: summary{Pass: 1}}
	if strings.Contains(summaryLine(r2), "DLL ") {
		t.Error("a run without a DLL should not print a DLL clause")
	}
}

func TestReportName(t *testing.T) {
	if got := reportName("win10", 19045, "20260101-1230"); got != "audiotest-win10-19045-20260101-1230.json" {
		t.Fatalf("report name %q", got)
	}
}

func TestRedactAppID(t *testing.T) {
	if got := redactAppID("fixture:game"); got != "fixture:game" {
		t.Errorf("a fixture id must stay, got %q", got)
	}
	if got := redactAppID("exe:discord.exe"); got != otherRedacted {
		t.Errorf("a non-fixture id must become %q, got %q", otherRedacted, got)
	}
}

func TestSummarize(t *testing.T) {
	s := summarize([]caseReport{
		{Result: resultPass}, {Result: resultPass}, {Result: resultFail}, {Result: resultInconclusive},
		{Result: resultSkip}, {Result: resultInfo},
	}, 1)
	if s.Pass != 2 || s.Fail != 1 || s.Inconclusive != 1 || s.Skip != 1 || s.Info != 1 || s.Exit != 1 {
		t.Fatalf("summary %+v", s)
	}
}

// TestReportJSONShape checks the report marshals with 05 §18.7's field names and that redacted events carry a
// fixture id.
func TestReportJSONShape(t *testing.T) {
	cr := caseReport{
		Name: "two-voice-apps", Required: true, Result: resultPass,
		Checks: []checkReport{{Freq: 440, From: 1.5, To: 0, Expect: "absent", LevelDB: -141.7,
			MaxShortDB: -128.4, MaxShortAtS: 6.31, Pass: true}},
		Measured: map[string]measure{measureCut: {ValueMs: 58, BudgetMs: 100}},
		ClicksAt: []float64{}, Counters: map[string]int{"underruns": 0},
		Events: redactEvents([]recEvent{{t: 1.21, code: "stream_running", app: "exe:game.exe"}},
			map[string]string{"game": "exe:game.exe"}),
	}
	r := report{V: 1, Harness: "dev", Gate: true, GateCfg: gateConfig{FastPath: true},
		Summary: summarize([]caseReport{cr}, 0), Cases: []caseReport{cr}}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	for _, want := range []string{
		`"v":1`, `"gateConfig":{"fastPath":true}`, `"maxShortDb":-128.4`, `"maxShortAtS":6.31`,
		`"app":"fixture:game"`, `"valueMs":58`, `"budgetMs":100`, `"to":0`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("report JSON missing %q\n%s", want, js)
		}
	}
}
