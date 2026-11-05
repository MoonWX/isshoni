//go:build windows

package winos

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// buildRelaunchee builds the helper (internal/relaunchee) into a temporary folder.
func buildRelaunchee(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "relaunchee.exe")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-buildvcs=false", "-o", exe, "./internal/relaunchee")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build relaunchee: %v\n%s", err, out)
	}
	return exe
}

// readWhenWritten waits up to 20 s for the helper's file.
func readWhenWritten(t *testing.T, path string) string {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return string(b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the helper wrote no %s", filepath.Base(path))
		}
	}
}

// TestRelaunchUnderShell is 03 §22.2's shell relaunch test: the helper, started from this test process (not a
// launcher), relaunches under the shell, and the process that stays sees explorer.exe as its parent; started with
// --relaunched it stays in place, under this test. Skipped when there is no shell window (a CI runner without an
// interactive desktop); it runs on the owner's PC runner.
func TestRelaunchUnderShell(t *testing.T) {
	if windows.GetShellWindow() == 0 {
		t.Skip("no shell window (GetShellWindow returns 0): the relaunch has nothing to start under")
	}
	exe := buildRelaunchee(t)
	dir := t.TempDir()

	out := filepath.Join(dir, "relaunched.txt")
	cmd := exec.CommandContext(t.Context(), exe, out)
	if err := cmd.Run(); err != nil {
		t.Fatalf("relaunchee: %v", err)
	}
	if got := strings.ToLower(readWhenWritten(t, out)); got != "explorer.exe" {
		t.Fatalf("the relaunched helper's parent is %q, want explorer.exe", got)
	}

	stay := filepath.Join(dir, "stayed.txt")
	cmd = exec.CommandContext(t.Context(), exe, RelaunchedFlag, stay)
	if err := cmd.Run(); err != nil {
		t.Fatalf("relaunchee --relaunched: %v", err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if got := readWhenWritten(t, stay); !strings.EqualFold(got, filepath.Base(self)) {
		t.Fatalf("with --relaunched the helper's parent is %q, want this test (%s)", got, filepath.Base(self))
	}
}

// TestEnvBlock: CreateProcessW's block is the entries NUL-ended plus one NUL; an entry with a NUL is refused.
func TestEnvBlock(t *testing.T) {
	b, err := envBlock([]string{"A=1", "B=ü"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(utf16Runes(b)); got != "A=1\x00B=ü\x00\x00" {
		t.Errorf("block %q", got)
	}
	if b, err := envBlock(nil); err != nil || len(b) != 2 {
		t.Errorf("empty block %v %v", b, err)
	}
	if _, err := envBlock([]string{"A=\x00"}); err == nil {
		t.Error("an entry with a NUL was taken")
	}
}

func utf16Runes(b []uint16) []rune {
	out := make([]rune, 0, len(b))
	for _, u := range b {
		out = append(out, rune(u))
	}
	return out
}
