package main

import (
	"time"

	"golang.org/x/sys/windows"

	"github.com/MoonWX/isshoni/internal/desktop/winos"
)

// setAfterRelaunch installs f as winos.AfterRelaunchForTest; wait reports the process's exit code if it ends in time.
func setAfterRelaunch(f func(pid uint32, wait func(time.Duration) (uint32, bool))) {
	winos.AfterRelaunchForTest = func(pid uint32, process windows.Handle) {
		f(pid, func(d time.Duration) (uint32, bool) {
			ev, err := windows.WaitForSingleObject(process, uint32(min(d.Milliseconds(), 60_000))) //nolint:gosec // G115: capped at 60 s
			if err != nil || ev != windows.WAIT_OBJECT_0 {
				return 0, false
			}
			var code uint32
			if windows.GetExitCodeProcess(process, &code) != nil {
				return 0, false
			}
			return code, true
		})
	}
}
