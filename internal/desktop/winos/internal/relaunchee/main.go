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

	"github.com/MoonWX/isshoni/internal/desktop/winos"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: relaunchee [--relaunched] <out>")
		return 2
	}
	out := args[len(args)-1]
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
