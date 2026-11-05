//go:build windows && !isshoni_hw

package main

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
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
	first := exec.CommandContext(ctx, exe, "--relaunched", "--autostart") //nolint:gosec // G204: the exe built above
	first.Env = env
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
			t.Fatalf("the --autostart start exited: %v (its log is under %s)", err, data)
		default:
		}
		if ok, _ := winos.MutexExists(winos.RunningMutex); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the app never held the running mutex")
		}
	}
	for end := time.Now().Add(8 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		select {
		case err := <-exited:
			t.Fatalf("the --autostart start exited: %v", err)
		default:
		}
		if n := visibleWindows(t, pid); n != 0 {
			t.Fatalf("%d visible windows after an --autostart start", n)
		}
	}

	// A second launch forwards to the first instance, which opens its window (03 §13.3).
	second := exec.CommandContext(ctx, exe, "--relaunched") //nolint:gosec // G204: the exe built above
	second.Env = env
	if err := second.Run(); err != nil {
		t.Fatalf("the second launch: %v", err)
	}
	for deadline := time.Now().Add(30 * time.Second); visibleWindows(t, pid) == 0; time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the window didn't open after a second launch")
		}
	}
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
