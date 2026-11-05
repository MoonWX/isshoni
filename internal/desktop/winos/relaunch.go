package winos

import (
	"errors"
	"strings"

	"github.com/MoonWX/isshoni/internal/media/audiorules"
)

// The shell relaunch (03 D16, §13.4 step 2): isshoni.exe runs as a child of the user's shell. Started by anything
// that is not Explorer or a launcher of 05 §5.4 (a browser that opened an isshoni:// link, the installer), it starts
// itself again under explorer.exe and exits, since isshoni's process tree is always kept out of shares, and inside a
// browser's tree it would keep that browser (Firefox plays from its parent process) out for the whole session
// (05 §6.2, §22).

// RelaunchedFlag is the argument the relaunch puts first: the new process doesn't relaunch again (03 §13.4 step 2).
// The CDP e2e build is started with it, so Playwright keeps the process it launched (03 §22.4).
const RelaunchedFlag = "--relaunched"

// ErrNoShell is RelaunchUnderShell's answer when there is no shell window (GetShellWindow returns 0: a service
// session, a CI runner without Explorer): the start goes on in place.
var ErrNoShell = errors.New("winos: no shell window")

// needsRelaunch reports whether a parent whose exe file name is parentExe needs the relaunch: anything but
// explorer.exe and 05's launchers and shells (audiorules.Launchers, compared in lower case, as Windows names are).
// "" (no parent, or a parent that is gone or whose PID was reused) needs none.
func needsRelaunch(parentExe string) bool {
	exe := strings.ToLower(parentExe)
	if exe == "" || exe == "explorer.exe" {
		return false
	}
	return !audiorules.Launchers().Contains("windows", exe)
}

// relaunchCommandLine is the new process's command line: exe, then RelaunchedFlag, then args unchanged, each quoted
// as syscall.EscapeArg (Windows' rules for CommandLineToArgvW) quotes it.
func relaunchCommandLine(exe string, args []string, escape func(string) string) string {
	parts := make([]string, 0, len(args)+2)
	parts = append(parts, escape(exe), RelaunchedFlag)
	for _, a := range args {
		parts = append(parts, escape(a))
	}
	return strings.Join(parts, " ")
}
