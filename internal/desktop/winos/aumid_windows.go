//go:build windows

package winos

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procSetAppID = windows.NewLazySystemDLL("shell32.dll").NewProc("SetCurrentProcessExplicitAppUserModelID")

// SetAppUserModelID sets the process's AppUserModelID (SetCurrentProcessExplicitAppUserModelID), InstanceID in the
// app, so its toasts, its taskbar button and the installer's Start-menu shortcut agree (03 §13.4 step 7, §15).
func SetAppUserModelID(id string) error {
	p, err := windows.UTF16PtrFromString(id)
	if err != nil {
		return err
	}
	if err := procSetAppID.Find(); err != nil {
		return fmt.Errorf("winos: %w", err)
	}
	if hr, _, _ := procSetAppID.Call(uintptr(unsafe.Pointer(p))); hr != 0 {
		return fmt.Errorf("winos: SetCurrentProcessExplicitAppUserModelID: HRESULT %#x", uint32(hr)) //nolint:gosec // G115: an HRESULT is 32 bits
	}
	return nil
}
