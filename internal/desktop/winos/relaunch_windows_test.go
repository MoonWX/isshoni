//go:build windows

package winos

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

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

// readWhenWritten waits up to 20 s for the helper's file; diag adds what the failure needs to be read.
func readWhenWritten(t *testing.T, path string, diag func() string) string {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return string(b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the helper wrote no %s (%s)", filepath.Base(path), diag())
		}
	}
}

// processesNamed describes the running processes whose exe file name is name: PID, parent PID and session.
func processesNamed(name string) string {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return "no process snapshot: " + err.Error()
	}
	defer func() { _ = windows.CloseHandle(snap) }()
	var found []string
	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if strings.EqualFold(windows.UTF16ToString(e.ExeFile[:]), name) {
			var s uint32
			_ = windows.ProcessIdToSessionId(e.ProcessID, &s)
			found = append(found, fmt.Sprintf("pid %d parent %d session %d", e.ProcessID, e.ParentProcessID, s))
		}
	}
	if len(found) == 0 {
		return "no " + name + " is running"
	}
	return name + " running: " + strings.Join(found, "; ")
}

// processName is the exe file name of the process pid, "?" when it isn't found.
func processName(pid uint32) string {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return "?"
	}
	defer func() { _ = windows.CloseHandle(snap) }()
	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if e.ProcessID == pid {
			return windows.UTF16ToString(e.ExeFile[:])
		}
	}
	return "?"
}

// TestRelaunchUnderShell is 03 §22.2's shell relaunch test. Started with --relaunched the helper stays in place,
// under this test. Started from this test process (not a launcher) it relaunches under the shell, and the process
// that stays sees explorer.exe as its parent. When the shell window's process is in another session or runs as
// another user than this test, or this test is elevated and the shell is not (a CI runner's elevated job), the helper
// must refuse the relaunch instead (exit 3, the start goes on in place), and the explorer-parent half is skipped
// with the reason; it runs on the owner's PC. Skipped when there is no shell window.
func TestRelaunchUnderShell(t *testing.T) {
	exe := buildRelaunchee(t)
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	stay := filepath.Join(dir, "stayed.txt")
	if err := exec.CommandContext(t.Context(), exe, RelaunchedFlag, stay).Run(); err != nil {
		t.Fatalf("relaunchee --relaunched: %v", err)
	}
	if got := readWhenWritten(t, stay, func() string { return "--relaunched" }); !strings.EqualFold(got,
		filepath.Base(self)) {
		t.Fatalf("with --relaunched the helper's parent is %q, want this test (%s)", got, filepath.Base(self))
	}

	shellPID, err := shellProcessID()
	if errors.Is(err, ErrNoShell) {
		t.Skip("no shell window (GetShellWindow returns 0): the relaunch has nothing to start under")
	}
	if err != nil {
		t.Fatal(err)
	}
	var mine, theirs uint32
	_ = windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &mine)
	_ = windows.ProcessIdToSessionId(shellPID, &theirs)
	where := fmt.Sprintf("the shell window's process %d (%s) in session %d; this test in session %d", shellPID,
		processName(shellPID), theirs, mine)
	t.Log(where)

	if err := shellIsOurs(shellPID); err != nil {
		if !errors.Is(err, errShellElsewhere) {
			t.Fatal(err)
		}
		out := filepath.Join(dir, "refused.txt")
		runErr := exec.CommandContext(t.Context(), exe, out).Run()
		var exit *exec.ExitError
		if !errors.As(runErr, &exit) || exit.ExitCode() != 3 {
			t.Fatalf("with the shell elsewhere (%v) relaunchee returned %v, want exit 3 (refused, in place)", err, runErr)
		}
		if got := readWhenWritten(t, out, func() string { return where }); !strings.HasPrefix(got, "error: ") ||
			!strings.Contains(got, errShellElsewhere.Error()) {
			t.Fatalf("helper wrote %q, want the errShellElsewhere refusal", got)
		}
		t.Skipf("the relaunch was refused as it must be; the explorer-parent half needs a shell in this test's "+
			"session and user: %v", err)
	}

	out := filepath.Join(dir, "relaunched.txt")
	if err := exec.CommandContext(t.Context(), exe, out).Run(); err != nil {
		b, _ := os.ReadFile(out) //nolint:gosec // G304: the helper's file in this test's temp dir
		var exit *exec.ExitError
		died := strings.HasPrefix(string(b), "error: "+errRelaunchDied.Error())
		if errors.As(err, &exit) && exit.ExitCode() == 3 && died && os.Getenv("RUNNER_ENVIRONMENT") == "github-hosted" {
			// The hosted image (UAC off, one full admin token) starts the child under its Explorer, which then dies
			// before its main: the relaunch saw it and went on in place, as it must. The explorer-parent half runs on a
			// real desktop.
			t.Skipf("the relaunched process died at once on the hosted runner and the start went on in place: %s (%s)",
				b, where)
		}
		t.Fatalf("relaunchee: %v, wrote %q (%s)", err, b, where)
	}
	got := readWhenWritten(t, out, func() string {
		fb := filepath.Join(os.TempDir(), "relaunchee-"+filepath.Base(dir)+".txt")
		b, err := os.ReadFile(fb) //nolint:gosec // G304: the helper's report in the temp root
		if err != nil {
			b = []byte("no report in the temp root: " + err.Error())
		}
		return fmt.Sprintf("%s; %s; elevated %v; the helper's report: %q", where, processesNamed(filepath.Base(exe)),
			windows.GetCurrentProcessToken().IsElevated(), b)
	})
	if got = strings.ToLower(got); got != "explorer.exe" {
		t.Fatalf("the relaunched helper's parent is %q, want explorer.exe", got)
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
