//go:build windows && !isshoni_hw

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/MoonWX/isshoni/internal/desktop/winos"
	"github.com/MoonWX/isshoni/web"
)

// TestAutostartRoundTrip is 03 §16.1's round trip with the real exe (README WA84): the e2e build started as the Run
// value starts it (--autostart; --relaunched as the CDP build is, 03 §13.4) runs on with no visible window, and a
// second launch, as a click on the shortcut, then shows one. The Run value's own round trip is winos'
// TestAutostartRunValue. It needs the built SPA in web/dist (task build:web: otherwise the app stops at its asset
// handler) and a WebView2 runtime, and skips under -race (the CGO_ENABLED=0 pass runs it) and while an isshoni runs:
// the build uses the real single-instance id and running mutex.
//
// With ISSHONI_REQUIRE_AUTOSTART_E2E=1 (CI's windows-desktop job, which has web/dist) a missing prerequisite fails
// the test instead of skipping it, so the acceptance can't turn into a silent skip.
//
// Every link of the chain is checked on its own, so a failure names the one that broke (the first windows-desktop
// run of PR #56 only said "the window didn't open"): no other process holds the single-instance mutex or pipe before
// the start; the first instance's pipe was created; the second launch forwarded (a failed forward still exits 0,
// 03 §13.3); the first instance logged the forward as arrived (winos' serveLoop) or not, when the window doesn't open;
// the first instance is still running while the window is awaited. A failure prints both starts' stderr (the e2e
// build mirrors its log there) and the first instance's log files.
func TestAutostartRoundTrip(t *testing.T) {
	skip := t.Skip
	if os.Getenv("ISSHONI_REQUIRE_AUTOSTART_E2E") == "1" {
		skip = t.Fatal
	}
	switch {
	case testing.Short():
		skip("builds and runs the app")
	case raceEnabled:
		skip("the race pass would build every package again; the CGO_ENABLED=0 pass runs this")
	}
	if _, err := fs.Stat(web.Dist(), "index.html"); err != nil {
		skip("the SPA isn't built (task build:web)")
	}
	if running, _ := winos.MutexExists(winos.RunningMutex); running {
		skip("an isshoni app runs on this PC")
	}
	// The build uses the real single-instance names: anything that still holds them (a process an earlier step left
	// in this session or another one; the pipe's name is per user, not per session) would take the second launch's
	// forward, or keep the first instance from serving.
	if held, _ := winos.MutexExists(`Local\` + winos.InstanceID + ".v1"); held {
		skip("another process holds the single-instance mutex " + winos.InstanceID + ".v1")
	}
	if pipe := singleInstancePipe(t); pipeExists(t, pipe) {
		skip(`another process serves the single-instance pipe \\.\pipe\` + pipe)
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		skip("no go command on PATH")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "isshoni.exe")
	build := exec.CommandContext(t.Context(), goTool, "build", "-buildvcs=false", "-tags", "e2e",
		"-ldflags=-H=windowsgui", "-o", exe, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	data := filepath.Join(dir, "data")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "ISSHONI_E2E_DATA_DIR="+data)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var firstErr, secondErr lockedBuffer
	// fail reports a failure with what both starts wrote and the first instance's log files.
	fail := func(format string, args ...any) {
		t.Helper()
		t.Fatalf("%s\n---- the --autostart start's stderr\n%s\n---- the second launch's stderr\n%s\n%s",
			fmt.Sprintf(format, args...), firstErr.String(), secondErr.String(),
			logFiles(filepath.Join(data, "isshoni", "logs")))
	}
	first := exec.CommandContext(ctx, exe, "--relaunched", "--autostart") //nolint:gosec // G204: the exe built above
	first.Env = env
	first.Stderr = &firstErr
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- first.Wait() }()
	t.Cleanup(func() {
		_ = first.Process.Kill()
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
		}
	})
	pid := uint32(first.Process.Pid) //nolint:gosec // G115: a PID fits

	// The running mutex shows the app got to step 4; from then on, for 8 s, no window of the process is visible.
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		select {
		case err := <-exited:
			fail("the --autostart start exited: %v", err)
		default:
		}
		if ok, _ := winos.MutexExists(winos.RunningMutex); ok {
			break
		}
		if time.Now().After(deadline) {
			fail("the app never held the running mutex")
		}
	}
	for end := time.Now().Add(8 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		select {
		case err := <-exited:
			fail("the --autostart start exited: %v", err)
		default:
		}
		if n := visibleWindows(t, pid); n != 0 {
			fail("%d visible windows after an --autostart start", n)
		}
	}
	// serveLoop gave up on the pipe's name after firstPipeWait (5 s), well inside the 8 s above.
	if out := firstErr.String() + logFiles(filepath.Join(data, "isshoni", "logs")); strings.Contains(out,
		"second launches can't reach this one") {
		fail("the first instance serves no single-instance pipe (another process holds its name?)")
	}

	// A second launch forwards to the first instance, which opens its window (03 §13.3).
	second := exec.CommandContext(ctx, exe, "--relaunched") //nolint:gosec // G204: the exe built above
	second.Env = env
	second.Stderr = &secondErr
	if err := second.Run(); err != nil {
		fail("the second launch: %v", err)
	}
	// Step 3 logs a forward that failed (or a lock it couldn't take) and still exits 0.
	// Only step 2a's handover lines mean no forward; "couldn't tell whether an installer runs" goes on to forward.
	for _, msg := range []string{"didn't take this start's arguments", "single instance unavailable",
		"desktop: an installer runs;", "desktop: an installer runs and"} {
		if strings.Contains(secondErr.String(), msg) {
			fail("the second launch didn't forward to the first instance (%q)", msg)
		}
	}
	for deadline := time.Now().Add(30 * time.Second); visibleWindows(t, pid) == 0; time.Sleep(200 * time.Millisecond) {
		select {
		case err := <-exited:
			fail("the first instance exited after the second launch: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			// winos' serveLoop logs each command line it delivers: which side of the pipe the chain broke on.
			arrived := "the first instance never logged the forward as arrived"
			if strings.Contains(firstErr.String()+logFiles(filepath.Join(data, "isshoni", "logs")),
				"a second launch's command line arrived") {
				arrived = "the first instance logged the forward as arrived"
			}
			fail("the window didn't open after a second launch (%s)", arrived)
		}
	}
}

// lockedBuffer is a child's stderr: exec's copying goroutine writes while the test reads.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// logFiles returns the last 16 KiB of each file in dir, for a failure's report.
func logFiles(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Sprintf("---- no log files (%v)\n", err)
	}
	var b strings.Builder
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // G304: the test's own data folder
		if err != nil {
			fmt.Fprintf(&b, "---- %s: %v\n", e.Name(), err)
			continue
		}
		const tail = 16 << 10
		if len(data) > tail {
			data = data[len(data)-tail:]
		}
		fmt.Fprintf(&b, "---- %s\n%s\n", e.Name(), data)
	}
	return b.String()
}

// singleInstancePipe is the name, below \\.\pipe\, of the single-instance pipe of this user (winos.NewSingleInstance's).
func singleInstancePipe(t *testing.T) string {
	t.Helper()
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("the user's SID: %v", err)
	}
	return winos.InstanceID + ".v1." + u.User.Sid.String()
}

// pipeExists reports whether a pipe of that name exists, by listing \\.\pipe\ (opening it would take an instance).
func pipeExists(t *testing.T, name string) bool {
	t.Helper()
	pattern, err := windows.UTF16PtrFromString(`\\.\pipe\` + name)
	if err != nil {
		t.Fatal(err)
	}
	var fd windows.Win32finddata
	h, err := windows.FindFirstFile(pattern, &fd)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return false
	}
	if err != nil {
		t.Fatalf("listing the pipe %s: %v", name, err)
	}
	_ = windows.FindClose(h)
	return true
}

// visibleWindows counts the visible top-level windows of the process pid.
func visibleWindows(t *testing.T, pid uint32) int {
	t.Helper()
	n := 0
	cb := syscall.NewCallback(func(hwnd windows.HWND, _ uintptr) uintptr {
		var owner uint32
		if _, err := windows.GetWindowThreadProcessId(hwnd, &owner); err == nil && owner == pid &&
			windows.IsWindowVisible(hwnd) {
			n++
		}
		return 1 // go on
	})
	if err := windows.EnumWindows(cb, unsafe.Pointer(nil)); err != nil {
		t.Fatalf("EnumWindows: %v", err)
	}
	return n
}
