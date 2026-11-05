//go:build windows

package winos

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// RelaunchUnderShell is 03 §13.4 step 2 (D16): when this process's parent is neither explorer.exe nor one of 05's
// launchers, it starts the same exe again as a child of the user's shell, with RelaunchedFlag first and then args
// (os.Args[1:]) unchanged, in the same environment and working directory, lets it take the foreground, and reports
// true: the caller exits 0. It reports false, nil when no relaunch is needed: args start with RelaunchedFlag, the
// parent is gone or its PID was reused (it was created after this process), or it is Explorer or a launcher. Any
// failure (ErrNoShell without a shell window, access denied, CreateProcessW) is false with the error, and the start
// goes on in place (the caller logs a WARN). The caller skips it while the running mutex exists (this start only
// forwards). Nothing names an exe in its errors or logs (README §4 "Logging").
func RelaunchUnderShell(args []string) (bool, error) {
	if len(args) > 0 && args[0] == RelaunchedFlag {
		return false, nil
	}
	parent, err := ParentExe()
	if err != nil {
		return false, err
	}
	if !needsRelaunch(parent) {
		return false, nil
	}
	if err := startUnderShell(args); err != nil {
		return false, err
	}
	return true, nil
}

// ParentExe is the exe file name of this process's parent (a Toolhelp snapshot's th32ParentProcessID and its entry's
// szExeFile), "" when the parent is gone or was created after this process (a reused PID).
func ParentExe() (string, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return "", fmt.Errorf("winos: the process snapshot: %w", err)
	}
	defer func() { _ = windows.CloseHandle(snap) }()
	self := windows.GetCurrentProcessId()
	names := map[uint32]string{}
	var parentPID uint32
	found := false
	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		names[e.ProcessID] = windows.UTF16ToString(e.ExeFile[:])
		if e.ProcessID == self {
			parentPID, found = e.ParentProcessID, true
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return "", fmt.Errorf("winos: the process snapshot: %w", err)
	}
	name, alive := names[parentPID]
	if !found || parentPID == 0 || !alive {
		return "", nil
	}
	// A newer parent is a reused PID: the real one is gone. One that can't be opened to compare is taken by its name.
	if newer, err := createdAfterSelf(parentPID); err == nil && newer {
		return "", nil
	}
	return name, nil
}

// createdAfterSelf reports whether the process pid was created after this one (GetProcessTimes): its PID was reused
// after the real parent exited. An error when it can't be opened or read.
func createdAfterSelf(pid uint32) (bool, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return false, err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var created, selfCreated, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exit, &kernel, &user); err != nil {
		return false, err
	}
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &selfCreated, &exit, &kernel, &user); err != nil {
		return false, err
	}
	return created.Nanoseconds() > selfCreated.Nanoseconds(), nil
}

// startUnderShell starts this exe with RelaunchedFlag and args as a child of the shell's process
// (PROC_THREAD_ATTRIBUTE_PARENT_PROCESS), and lets it take the foreground.
func startUnderShell(args []string) error {
	hwnd := windows.GetShellWindow()
	if hwnd == 0 {
		return ErrNoShell
	}
	var shellPID uint32
	if _, err := windows.GetWindowThreadProcessId(hwnd, &shellPID); err != nil || shellPID == 0 {
		return fmt.Errorf("winos: the shell's process: %w", errors.Join(err, ErrNoShell))
	}
	shell, err := windows.OpenProcess(windows.PROCESS_CREATE_PROCESS, false, shellPID)
	if err != nil {
		return fmt.Errorf("winos: open the shell's process: %w", err)
	}
	defer func() { _ = windows.CloseHandle(shell) }()

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("winos: this exe: %w", err)
	}
	dir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("winos: the working directory: %w", err)
	}
	exe16, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	cmd16, err := windows.UTF16PtrFromString(relaunchCommandLine(exe, args, syscall.EscapeArg))
	if err != nil {
		return err
	}
	dir16, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	env, err := envBlock(os.Environ())
	if err != nil {
		return err
	}

	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return fmt.Errorf("winos: the attribute list: %w", err)
	}
	defer attrs.Delete()
	parent := shell // Update keeps a pointer to it: it must live until CreateProcessW returned
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PARENT_PROCESS, unsafe.Pointer(&parent),
		unsafe.Sizeof(parent)); err != nil {
		return fmt.Errorf("winos: the parent attribute: %w", err)
	}
	si := &windows.StartupInfoEx{
		StartupInfo:             windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{}))},
		ProcThreadAttributeList: attrs.List(),
	}
	var pi windows.ProcessInformation
	err = windows.CreateProcess(exe16, cmd16, nil, nil, false,
		windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_UNICODE_ENVIRONMENT, &env[0], dir16, &si.StartupInfo, &pi)
	if err != nil {
		return fmt.Errorf("winos: CreateProcessW: %w", err)
	}
	_ = windows.CloseHandle(pi.Thread)
	_ = windows.CloseHandle(pi.Process)
	// The new process shows the window; this one, which the user started, may hand it the foreground.
	_, _, _ = procAllowSetForegroundWindow.Call(uintptr(pi.ProcessId))
	return nil
}

// envBlock is environ as CreateProcessW's Unicode environment block: "name=value" strings, each ended by a NUL,
// and one more NUL at the end.
func envBlock(environ []string) ([]uint16, error) {
	var b []uint16
	for _, kv := range environ {
		for _, r := range kv {
			if r == 0 {
				return nil, fmt.Errorf("winos: an environment entry holds a NUL: %w", windows.ERROR_INVALID_PARAMETER)
			}
		}
		b = append(b, utf16.Encode([]rune(kv))...)
		b = append(b, 0)
	}
	if len(b) == 0 {
		b = append(b, 0)
	}
	return append(b, 0), nil
}
