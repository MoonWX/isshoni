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
// failure (ErrNoShell without a shell window, errShellElsewhere for a shell in another session or of another user,
// access denied, CreateProcessW, errRelaunchDied for a relaunched process that exited at once with a nonzero code) is
// false with the error, and the start goes on in place (the caller logs a WARN).
// The caller skips it while the running mutex exists (this start only forwards). Nothing names an exe in its errors
// or logs (README §4 "Logging").
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
// (PROC_THREAD_ATTRIBUTE_PARENT_PROCESS), and lets it take the foreground. The shell's process must be in this
// process's session and run as this process's user (shellIsOurs).
func startUnderShell(args []string) error {
	shellPID, err := shellProcessID()
	if err != nil {
		return err
	}
	if err := shellIsOurs(shellPID); err != nil {
		return err
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
	// The shell's desktop, not this process's: the child takes the shell's token, and with lpDesktop unset it would
	// open this process's window station and desktop, which that token may not use (a CI runner's job: the child
	// died with STATUS_DLL_INIT_FAILED, 0xC0000142, before main).
	desktop, err := windows.UTF16PtrFromString(`winsta0\default`)
	if err != nil {
		return err
	}
	si := &windows.StartupInfoEx{
		StartupInfo:             windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{})), Desktop: desktop},
		ProcThreadAttributeList: attrs.List(),
	}
	var pi windows.ProcessInformation
	err = windows.CreateProcess(exe16, cmd16, nil, nil, false,
		windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_UNICODE_ENVIRONMENT, &env[0], dir16, &si.StartupInfo, &pi)
	if err != nil {
		return fmt.Errorf("winos: CreateProcessW: %w", err)
	}
	_ = windows.CloseHandle(pi.Thread)
	defer func() { _ = windows.CloseHandle(pi.Process) }()
	// The new process shows the window; this one, which the user started, may hand it the foreground.
	_, _, _ = procAllowSetForegroundWindow.Call(uintptr(pi.ProcessId))
	if f := AfterRelaunchForTest; f != nil {
		f(pi.ProcessId, pi.Process)
	}
	return relaunchedLives(pi.Process)
}

// relaunchDeathWindowMs is how long startUnderShell watches the relaunched process before this start exits.
const relaunchDeathWindowMs = 1500

// errRelaunchDied: the relaunched process exited with a nonzero code within relaunchDeathWindowMs, so it will never
// show the app; the start goes on in place. Exit code 0 is a normal end (it forwarded to a running instance).
var errRelaunchDied = errors.New("winos: the relaunched process exited at once")

// relaunchedLives waits up to relaunchDeathWindowMs for the relaunched process: nil while it runs or when it ended
// with 0, errRelaunchDied (with its exit code) when it ended with another.
// A process that can't be watched or read was started all the same, so that is nil too.
func relaunchedLives(process windows.Handle) error {
	ev, waitErr := windows.WaitForSingleObject(process, relaunchDeathWindowMs)
	if waitErr != nil || ev != windows.WAIT_OBJECT_0 {
		return nil //nolint:nilerr // still running (WAIT_TIMEOUT), or it can't be watched: it was started
	}
	var code uint32
	if err := windows.GetExitCodeProcess(process, &code); err != nil {
		return nil //nolint:nilerr // its exit code can't be read: it was started
	}
	if code == 0 {
		return nil
	}
	return fmt.Errorf("%w (0x%08x)", errRelaunchDied, code)
}

// AfterRelaunchForTest, when set (the relaunch test's helper only), gets the relaunched process's PID and handle
// before the handle is closed, so the test can report how that process ended.
var AfterRelaunchForTest func(pid uint32, process windows.Handle)

// errShellElsewhere: the shell window's process is in another session or runs as another user. The child would
// inherit its token (PROC_THREAD_ATTRIBUTE_PARENT_PROCESS: the parent's token, so its user and session), so the app
// would start out of sight or as someone else while this start exits 0. The start goes on in place instead. (An
// elevated start under a shell that isn't elevated relaunches de-elevated, as the shell's token runs it.)
var errShellElsewhere = errors.New("winos: the shell runs in another session or as another user")

// shellProcessID is the PID owning GetShellWindow's window; ErrNoShell without one.
func shellProcessID() (uint32, error) {
	hwnd := windows.GetShellWindow()
	if hwnd == 0 {
		return 0, ErrNoShell
	}
	var pid uint32
	if _, err := windows.GetWindowThreadProcessId(hwnd, &pid); err != nil || pid == 0 {
		return 0, fmt.Errorf("winos: the shell's process: %w", errors.Join(err, ErrNoShell))
	}
	return pid, nil
}

// shellIsOurs is nil when the process pid is in this process's session and its token's user is this process's
// user; errShellElsewhere (wrapped) when it isn't or its token can't be read.
func shellIsOurs(pid uint32) error {
	var mine, theirs uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &mine); err != nil {
		return fmt.Errorf("winos: this process's session: %w", err)
	}
	if err := windows.ProcessIdToSessionId(pid, &theirs); err != nil {
		return fmt.Errorf("winos: the shell's session: %w", err)
	}
	if mine != theirs {
		return fmt.Errorf("%w (session %d, this process %d)", errShellElsewhere, theirs, mine)
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return fmt.Errorf("winos: open the shell's process: %w", err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var tok windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok); err != nil {
		return fmt.Errorf("%w (its token can't be read: %w)", errShellElsewhere, err)
	}
	defer func() { _ = tok.Close() }()
	theirUser, err := tok.GetTokenUser()
	if err != nil {
		return fmt.Errorf("winos: the shell's user: %w", err)
	}
	myUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("winos: this process's user: %w", err)
	}
	if !theirUser.User.Sid.Equals(myUser.User.Sid) {
		return fmt.Errorf("%w (another user)", errShellElsewhere)
	}
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
