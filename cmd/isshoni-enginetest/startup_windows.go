//go:build windows

package main

import (
	"errors"

	"golang.org/x/sys/windows"
)

var (
	user32                            = windows.NewLazySystemDLL("user32.dll")
	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
)

// dpiAwarenessPerMonitorV2 is DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 ((DPI_AWARENESS_CONTEXT)-4).
const dpiAwarenessPerMonitorV2 = ^uintptr(3)

// processSetup is the tool's first step (04 §14): the process default of 04 §13.3 layer 2, so that no bare-name DLL
// load in this process (Go's, a vendor runtime FFmpeg loads, the default delay-load helper) searches the current
// directory or PATH; then per-monitor DPI awareness (v2), which Desktop Duplication needs (04 §7.4). Every step is
// tried; the errors are joined.
func processSetup() error {
	var errs []error
	errs = append(errs, restrictDLLSearch())
	if err := procSetProcessDpiAwarenessContext.Find(); err != nil {
		errs = append(errs, err)
	} else if r, _, e := procSetProcessDpiAwarenessContext.Call(dpiAwarenessPerMonitorV2); r == 0 {
		// ERROR_ACCESS_DENIED: already set (a manifest, or a second call); that is the state asked for or a
		// stronger one, so it is not an error.
		if !errors.Is(e, windows.ERROR_ACCESS_DENIED) {
			errs = append(errs, e)
		}
	}
	return errors.Join(errs...)
}

// restrictDLLSearch is 04 §13.3 layer 2: SetDefaultDllDirectories(LOAD_LIBRARY_SEARCH_SYSTEM32) and
// SetDllDirectoryW(""). The engine itself is loaded by full path (internal/media/native).
func restrictDLLSearch() error {
	return errors.Join(
		windows.SetDefaultDllDirectories(windows.LOAD_LIBRARY_SEARCH_SYSTEM32),
		windows.SetDllDirectory(""),
	)
}
