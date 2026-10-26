package main

import (
	"runtime"
	"strings"
	"testing"
)

func TestAnalyzerSelfCheck(t *testing.T) {
	var b strings.Builder
	if err := analyzerSelfCheck(&b); err != nil {
		t.Fatalf("analyzer self-check failed: %v", err)
	}
	if !strings.Contains(b.String(), "short-window rule ok") {
		t.Fatalf("self-check output missing a step:\n%s", b.String())
	}
}

func TestSelfcheckWithoutEngine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this build has the engine")
	}
	var b strings.Builder
	// Off Windows, selfcheck runs the analyzer and skips the engine part, returning success.
	if got := cmdSelfcheck(&b); got != exitAllPass {
		t.Fatalf("selfcheck off Windows: exit %d, want %d\n%s", got, exitAllPass, b.String())
	}
	if !strings.Contains(b.String(), "engine self-check skipped") {
		t.Fatalf("selfcheck should note the engine was skipped:\n%s", b.String())
	}
}

func TestRunWithoutEngine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this build has the engine")
	}
	var b strings.Builder
	// `run` needs the engine; off Windows it is a setup error (exit 4).
	if got := cmdRun(&b, []string{"-gate"}); got != exitSetup {
		t.Fatalf("run off Windows: exit %d, want %d\n%s", got, exitSetup, b.String())
	}
}

func TestSelectCases(t *testing.T) {
	all, err := selectCases(runOptions{})
	if err != nil || len(all) != len(cases()) {
		t.Fatalf("default selection: %d cases, err %v", len(all), err)
	}
	sel, err := selectCases(runOptions{cases: []string{"2", "fast-path"}})
	if err != nil || len(sel) != 2 || sel[0].num != 2 || sel[1].name != "fast-path" {
		t.Fatalf("explicit selection: %+v err %v", sel, err)
	}
	if _, err := selectCases(runOptions{cases: []string{"9"}}); err == nil {
		t.Fatal("case 9 (WA62b) should not resolve in this build")
	}
	if _, err := selectCases(runOptions{cases: []string{"nope"}}); err == nil {
		t.Fatal("an unknown case name should error")
	}
}
