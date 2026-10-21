//go:build windows && isshoni_hw

package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// checklistDataDir is the WebView2 profile of the checklist run: a temporary folder, never the user's.
func checklistDataDir() (string, error) { return os.MkdirTemp("", "isshoni-checklist-webview2-") }

var procIsWindow = windows.NewLazySystemDLL("user32.dll").NewProc("IsWindow")

// isNativeWindow reports whether h is a window handle (W10).
func isNativeWindow(h uintptr) bool {
	if h == 0 {
		return false
	}
	r, _, _ := procIsWindow.Call(h)
	return r != 0
}

// pipeExists reports whether the named pipe \\.\pipe\<name> exists (W19).
func pipeExists(name string) bool {
	_, err := os.Stat(`\\.\pipe\` + name) //nolint:gosec // G703: the pipe name the caller set
	return err == nil
}

// webviewCommandLines returns the command lines of the msedgewebview2.exe processes below this one (W19): the
// WebView2 browser process is this process's child, and its renderer, GPU and utility processes are its children.
func webviewCommandLines() ([]string, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = windows.CloseHandle(snap) }()
	type proc struct {
		parent uint32
		exe    string
	}
	procs := map[uint32]proc{}
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		procs[e.ProcessID] = proc{e.ParentProcessID, windows.UTF16ToString(e.ExeFile[:])}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, err
	}
	self := uint32(os.Getpid()) //nolint:gosec // G115: a Windows PID is a DWORD
	below := func(pid uint32) bool {
		for range 16 { // a reused PID can make a cycle
			p, ok := procs[pid]
			if !ok || p.parent == 0 || p.parent == pid {
				return false
			}
			if p.parent == self {
				return true
			}
			pid = p.parent
		}
		return false
	}
	var out []string
	for pid, p := range procs {
		if !strings.EqualFold(p.exe, "msedgewebview2.exe") || !below(pid) {
			continue
		}
		cl, err := processCommandLine(pid)
		if err != nil {
			return out, fmt.Errorf("msedgewebview2.exe %d: %w", pid, err)
		}
		out = append(out, cl)
	}
	return out, nil
}

// processCommandLine reads a process's command line (NtQueryInformationProcess, ProcessCommandLineInformation:
// Windows 8.1 and later).
func processCommandLine(pid uint32) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	buf := make([]byte, 4096)
	for {
		var n uint32
		err := windows.NtQueryInformationProcess(h, windows.ProcessCommandLineInformation, unsafe.Pointer(&buf[0]),
			uint32(len(buf)), &n) //nolint:gosec // G115: a few KiB
		if err == nil {
			break
		}
		if int(n) > len(buf) {
			buf = make([]byte, n)
			continue
		}
		return "", err
	}
	return (*windows.NTUnicodeString)(unsafe.Pointer(&buf[0])).String(), nil
}
