package winos

import (
	"strings"
	"testing"
)

// TestNeedsRelaunch is 03 §13.4 step 2's parent rule: Explorer and 05's launchers and shells (in any case) keep the
// start in place; anything else, a browser or the installer, relaunches; no parent never does.
func TestNeedsRelaunch(t *testing.T) {
	for exe, want := range map[string]bool{
		"":                      false,
		"explorer.exe":          false,
		"EXPLORER.EXE":          false,
		"steam.exe":             false,
		"cmd.exe":               false,
		"WindowsTerminal.exe":   false,
		"firefox.exe":           true,
		"chrome.exe":            true,
		"isshoni-setup.exe":     true,
		"winos.test.exe":        true,
		"explorer.exe.evil.exe": true,
	} {
		if got := needsRelaunch(exe); got != want {
			t.Errorf("needsRelaunch(%q) = %v, want %v", exe, got, want)
		}
	}
}

// TestRelaunchCommandLine: the exe, then --relaunched, then the arguments unchanged, each through the escaper.
func TestRelaunchCommandLine(t *testing.T) {
	quote := func(s string) string {
		if s == "" || strings.ContainsAny(s, " \t\"") {
			return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
		}
		return s
	}
	got := relaunchCommandLine(`C:\Program Files\isshoni\isshoni.exe`,
		[]string{"--open-url", "isshoni://watch.example.com/r/lounge", "a b", ""}, quote)
	want := `"C:\Program Files\isshoni\isshoni.exe" --relaunched --open-url isshoni://watch.example.com/r/lounge ` +
		`"a b" ""`
	if got != want {
		t.Errorf("command line\n got %s\nwant %s", got, want)
	}
}

// TestRelaunchedSkips: a start that already carries --relaunched first never relaunches, on every OS.
func TestRelaunchedSkips(t *testing.T) {
	if relaunched, err := RelaunchUnderShell([]string{RelaunchedFlag, "--open-url", "x"}); relaunched || err != nil {
		t.Fatalf("RelaunchUnderShell(--relaunched …) = %v, %v", relaunched, err)
	}
}
