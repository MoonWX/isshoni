//go:build windows

package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/windows"
)

// restrictDLLSearch is main's first action, before step 0 and in every mode (03 §13.4; 04 §13.3 layer 2): the
// process default for DLL loads becomes System32 only (SetDefaultDllDirectories(LOAD_LIBRARY_SEARCH_SYSTEM32)), and
// the current directory leaves the legacy search order (SetDllDirectoryW("")). So no later bare-name load, by Go,
// Wails, WebView2's loader or a system DLL, searches the current directory (a browser's download folder on a link
// launch) or PATH. The app folder isn't added: the pinned Wails loads WebView2 through its own Go loader by full
// path, and isshoni-media.dll is loaded by full path (04 §13.3), so nothing needs a bare-name load from it.
//
// Both calls exist on every supported Windows build; a failure is logged at WARN once logging exists, and the start
// goes on.
func restrictDLLSearch() error {
	return errors.Join(
		windows.SetDefaultDllDirectories(windows.LOAD_LIBRARY_SEARCH_SYSTEM32),
		windows.SetDllDirectory(""),
	)
}

var (
	kernel32          = windows.NewLazySystemDLL("kernel32.dll")
	procAttachConsole = kernel32.NewProc("AttachConsole")
)

const attachParentProcess = ^uint32(0) // ATTACH_PARENT_PROCESS

// versionOutput is where --version writes (03 §13.4 step 1): a GUI-subsystem process writes to the stdout handle it
// inherited (a pipe or a file when the caller redirected it); without one it attaches to its parent's console
// (AttachConsole(ATTACH_PARENT_PROCESS)) and writes there. With neither, the line goes nowhere.
func versionOutput() io.Writer {
	if h, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE); err == nil && h != 0 && h != windows.InvalidHandle {
		if t, err := windows.GetFileType(h); err == nil && t != windows.FILE_TYPE_UNKNOWN {
			return os.NewFile(uintptr(h), "stdout")
		}
	}
	if r, _, _ := procAttachConsole.Call(uintptr(attachParentProcess)); r == 0 {
		return io.Discard
	}
	f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0)
	if err != nil {
		return io.Discard
	}
	return f
}

// firstActionError formats the first action's failure for its WARN line.
func firstActionError(err error) error {
	return fmt.Errorf("the DLL search order wasn't restricted: %w", err)
}
