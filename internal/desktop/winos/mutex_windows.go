//go:build windows

package winos

import (
	"errors"
	"fmt"
	"sync"

	"golang.org/x/sys/windows"
)

// running keeps the running mutex's handle for the life of the process: it is never closed (03 §13.4 step 4).
var running struct {
	sync.Mutex
	h windows.Handle
}

// HoldRunningMutex creates the named mutex (RunningMutex; tests pass their own) with CreateMutexW(nil, FALSE, name)
// and keeps the handle until the process exits: the installer and uninstaller wait for the name to disappear
// (06 §2.4, §6.7). Only the first instance calls it, after single instance; one-shot modes never do. A second call
// does nothing.
func HoldRunningMutex(name string) error {
	running.Lock()
	defer running.Unlock()
	if running.h != 0 {
		return nil
	}
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	h, err := windows.CreateMutex(nil, false, p)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return fmt.Errorf("winos: the running mutex: %w", err)
	}
	running.h = h
	return nil
}

// MutexExists reports whether a named mutex exists now (OpenMutexW with SYNCHRONIZE): the installer's
// Local\io.isshoni.desktop.installing at 03 §13.4 step 2a (README WA59), and the tests of the running mutex.
func MutexExists(name string) (bool, error) {
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
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		return true, nil // it exists; we may not open it
	default:
		return false, fmt.Errorf("winos: %w", err)
	}
}
