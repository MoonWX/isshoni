package main

import (
	"errors"
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

// TestSelfcheckFailsWhenTheDLLDoesntLoad is 05 §18.2: selfcheck loads the DLL and exits 0 or 1; only an OS without
// the engine skips the engine part. A missing DLL, a wrong ABI or a missing export fails it (CI windows runs it).
func TestSelfcheckFailsWhenTheDLLDoesntLoad(t *testing.T) {
	var b strings.Builder
	open := func(driverOptions) (driver, error) { return nil, errors.New("open the engine: the DLL is missing") }
	if got := selfcheckWith(&b, open); got != exitFail {
		t.Fatalf("a DLL that doesn't load: exit %d, want %d\n%s", got, exitFail, b.String())
	}
	b.Reset()
	open = func(driverOptions) (driver, error) { return nil, errUnsupportedOS }
	if got := selfcheckWith(&b, open); got != exitAllPass || !strings.Contains(b.String(), "skipped") {
		t.Fatalf("no engine on this OS: exit %d, want %d with a skip note\n%s", got, exitAllPass, b.String())
	}
	b.Reset()
	open = func(driverOptions) (driver, error) { return &stubDriver{endpoints: 1}, nil }
	if got := selfcheckWith(&b, open); got != exitAllPass || !strings.Contains(b.String(), "selfcheck: ok") {
		t.Fatalf("a working engine: exit %d\n%s", got, b.String())
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
	all, err := selectCases(runOptions{long: true})
	if err != nil || len(all) != len(cases()) {
		t.Fatalf("default selection with -long: %d cases, err %v", len(all), err)
	}
	sel, err := selectCases(runOptions{cases: []string{"2", "fast-path", "9"}})
	if err != nil || len(sel) != 3 || sel[0].num != 2 || sel[1].name != "fast-path" || sel[2].name != "steam-game" {
		t.Fatalf("explicit selection: %+v err %v", sel, err)
	}
	if _, err := selectCases(runOptions{cases: []string{"28"}}); err == nil {
		t.Fatal("case 28 doesn't exist and should not resolve")
	}
	if _, err := selectCases(runOptions{cases: []string{"nope"}}); err == nil {
		t.Fatal("an unknown case name should error")
	}
}

func TestRecordAndMergeUsage(t *testing.T) {
	var b strings.Builder
	if got := cmdMerge(&b, []string{"only-a-report.json"}); got != exitSetup || !strings.Contains(b.String(), "usage") {
		t.Fatalf("merge with one argument: exit %d\n%s", got, b.String())
	}
	b.Reset()
	if got := cmdRecord(&b, strings.NewReader(""), []string{"-d", "5"}); got != exitSetup ||
		!strings.Contains(b.String(), "-label") {
		t.Fatalf("record without -label: exit %d\n%s", got, b.String())
	}
}
