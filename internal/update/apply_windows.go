//go:build windows

package update

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Applying on Windows (06 §8.7, §2.3, §2.4): only a copy whose exe is in HKCU\Software\isshoni\InstallDir updates,
// and it runs the NSIS installer silently with /S /UPDATE /RELAUNCH, then waits until the installer holds its mutex
// Local\io.isshoni.desktop.installing, so that the app's running mutex and the installer's mutex overlap and no start
// in between can become a first instance.

const (
	installKey      = `Software\isshoni`
	installDirValue = "InstallDir"
	installingMutex = `Local\io.isshoni.desktop.installing`
)

func defaultSystem() system {
	return system{
		goos:      runtime.GOOS,
		goarch:    runtime.GOARCH,
		installed: installedCopy,
		freeSpace: freeSpace,
		launch:    launcher{start: startInstaller, exists: installingExists}.run,
		policy:    PolicyDisabled,
	}
}

// installedCopy reports whether the running exe's folder is HKCU\Software\isshoni\InstallDir (06 §8.7). A missing key
// or value means a copy that was never installed.
func installedCopy() (bool, error) {
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	dir, err := installDir(registry.CURRENT_USER, installKey)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return sameFolder(filepath.Dir(exe), dir), nil
}

// installDir reads the InstallDir value under root\path, expanding a REG_EXPAND_SZ.
func installDir(root registry.Key, path string) (string, error) {
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	defer func() { _ = k.Close() }()
	v, typ, err := k.GetStringValue(installDirValue)
	if err != nil {
		return "", err
	}
	if typ == registry.EXPAND_SZ {
		if v, err = registry.ExpandString(v); err != nil {
			return "", err
		}
	}
	if v == "" {
		return "", registry.ErrNotExist
	}
	return v, nil
}

// sameFolder compares two folder paths as Windows does: case-insensitively, without a trailing separator, and, when
// that differs, in their long forms (a short 8.3 name in one of them).
func sameFolder(a, b string) bool {
	clean := func(p string) string { return strings.TrimRight(filepath.Clean(p), `\/`) }
	if strings.EqualFold(clean(a), clean(b)) {
		return true
	}
	la, erra := longPath(a)
	lb, errb := longPath(b)
	return erra == nil && errb == nil && strings.EqualFold(clean(la), clean(lb))
}

// longPath returns the long form of an existing path (GetLongPathNameW).
func longPath(p string) (string, error) {
	in, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetLongPathName(in, &buf[0], uint32(len(buf))) //nolint:gosec // len(buf) is small
		if err != nil {
			return "", err
		}
		if int(n) <= len(buf) {
			return windows.UTF16ToString(buf[:n]), nil
		}
		buf = make([]uint16, n)
	}
}

// freeSpace returns the bytes free for this user on dir's volume (GetDiskFreeSpaceExW).
func freeSpace(dir string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var avail, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &totalFree); err != nil {
		return 0, err
	}
	return avail, nil
}

// launcher starts the installer and waits for its mutex; the tests replace start and exists.
type launcher struct {
	start  func(ctx context.Context, path string, args []string) error
	exists func() (bool, error)
}

// run starts the installer at path with args and returns once its mutex exists, or after 10 s with a WARN
// (waitInstalling). A failed start is CodeInstallFailed.
func (l launcher) run(ctx context.Context, path string, args []string, log *slog.Logger) error {
	if err := l.start(ctx, path, args); err != nil {
		return &Error{Code: CodeInstallFailed, Err: fmt.Errorf("start the installer: %w", err)}
	}
	return waitInstalling(ctx, l.exists, log)
}

// startInstaller starts the installer with exactly args (06 §8.7: hidden window, a new process group, no job object
// of the app's) and lets it go: the installer outlives the app, which quits right after. The end of ctx must never
// kill it, so the command's context is never done (which also means os/exec starts no goroutine to watch it).
//
// HideWindow is 06 §8.7's literal. It sets STARTUPINFO's SW_HIDE, which Windows may apply to the installer's first
// top-level window: in /S /UPDATE mode that is §6.4's "Updating isshoni to {v}…" banner. Whether the banner shows
// through Apply is not verified on Windows yet (an owner's-PC step); if it doesn't, 06 §8.7 drops HideWindow (an NSIS
// GUI exe opens no console, so it buys nothing).
func startInstaller(ctx context.Context, path string, args []string) error {
	//nolint:gosec // G204: the verified installer in the updates folder, with fixed arguments
	cmd := exec.CommandContext(context.WithoutCancel(ctx), path, args...)
	cmd.Dir = filepath.Dir(path)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// installingExists probes the installer's mutex with OpenMutexW(SYNCHRONIZE, FALSE, …) (06 §2.4).
func installingExists() (bool, error) { return mutexExists(installingMutex) }

func mutexExists(name string) (bool, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return false, err
	}
	h, err := windows.OpenMutex(windows.SYNCHRONIZE, false, p)
	switch {
	case err == nil:
		_ = windows.CloseHandle(h)
		return true, nil
	case errors.Is(err, windows.ERROR_FILE_NOT_FOUND):
		return false, nil
	case errors.Is(err, windows.ERROR_ACCESS_DENIED): // it exists, with a DACL that doesn't let us open it
		return true, nil
	}
	return false, err
}
