// Command relaunchee is the shell relaunch's test helper (docs/m2/03-desktop-app.md §22.2): started by the winos
// tests (a parent that is neither Explorer nor a launcher), it calls winos.RelaunchUnderShell with its arguments and
// exits 0 when it relaunched; the process that stays writes its parent's exe file name to the file named by its last
// argument. "relaunchee [--relaunched] <out>". Exit codes: 0 written (or relaunched), 2 bad usage, 3 the relaunch
// failed (the error goes to the file as "error: ..."; "noshell" for winos.ErrNoShell).
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/MoonWX/isshoni/internal/desktop/winos"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: relaunchee [--relaunched] <out>")
		return 2
	}
	out := args[len(args)-1]
	watchChild(out)
	relaunched, err := winos.RelaunchUnderShell(args)
	switch {
	case relaunched:
		return 0
	case errors.Is(err, winos.ErrNoShell):
		write(out, "noshell")
		return 3
	case err != nil:
		write(out, "error: "+err.Error())
		return 3
	}
	parent, err := winos.ParentExe()
	if err != nil {
		write(out, "error: "+err.Error())
		return 3
	}
	if !write(out, parent) {
		return 3
	}
	return 0
}

// write writes text to the test's file; false when it couldn't. A failed write is reported next to the temp root
// (FallbackPath), which the test reads to say why its file never appeared.
func write(path, text string) bool {
	err := os.WriteFile(path, []byte(text), 0o600) //nolint:gosec // G703: the file the test named, in its temp dir
	if err != nil {
		_ = os.WriteFile(FallbackPath(path), []byte(text+"\nwrite: "+err.Error()), 0o600) //nolint:gosec // G703: as above
	}
	return err == nil
}

// FallbackPath is where write reports a failed write of out: in the temp root, named after out's folder.
func FallbackPath(out string) string {
	return filepath.Join(os.TempDir(), "relaunchee-"+filepath.Base(filepath.Dir(out))+".txt")
}

// childReport is where the helper records how the process it relaunched ended (its PID and exit code), for the
// test's failure message.
func childReport(out string) string { return out + ".child" }

// watchChild makes RelaunchUnderShell's relaunched process report itself: its PID, then, if it ends within 10 s, its
// exit code (0xC0000142 would mean it could not start on the desktop).
func watchChild(out string) {
	setAfterRelaunch(func(pid uint32, wait func(time.Duration) (uint32, bool)) {
		text := fmt.Sprintf("pid %d", pid)
		if code, ended := wait(10 * time.Second); ended {
			text += fmt.Sprintf(", exited 0x%08X", code)
		} else {
			text += ", still running after 10 s"
		}
		_ = os.WriteFile(childReport(out), []byte(text), 0o600) //nolint:gosec // G703: next to the test's file
	})
}
