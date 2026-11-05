//go:build windows

package native

import (
	"runtime"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/MoonWX/isshoni/internal/media"
)

var (
	testUser32                    = windows.NewLazySystemDLL("user32.dll")
	procTestCreateWindowExW       = testUser32.NewProc("CreateWindowExW")
	procTestDestroyWindow         = testUser32.NewProc("DestroyWindow")
	procTestGetWindowDispAffinity = testUser32.NewProc("GetWindowDisplayAffinity")
	procTestGetDesktopWindow      = testUser32.NewProc("GetDesktopWindow")
)

const (
	testWDAExcludeFromCapture    = 0x11                    // WDA_EXCLUDEFROMCAPTURE
	testWSPopup                  = 0x80000000              // WS_POPUP
	testWSExToolWindowNoActivate = 0x00000080 | 0x08000000 // WS_EX_TOOLWINDOW | WS_EX_NOACTIVATE
)

// TestDLLProtectWindow (README WA70, 04 §11): against the real engine, ProtectWindow sets WDA_EXCLUDEFROMCAPTURE on a
// window of this process (GetWindowDisplayAffinity reads it back) and refuses another process's window (the desktop
// window, csrss's) as invalid. The window is a hidden "STATIC" popup: the affinity needs no visible window.
func TestDLLProtectWindow(t *testing.T) {
	e := openDLL(t)
	if b := windowsBuild(); b != 0 && b < 19041 {
		wantError(t, e.ProtectWindow(1), media.CodeUnsupportedOS, tokenOSTooOld, "protect_window")
		return
	}
	// The window lives on this goroutine's OS thread, which owns it until DestroyWindow.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	class, err := windows.UTF16PtrFromString("STATIC")
	if err != nil {
		t.Fatal(err)
	}
	title, err := windows.UTF16PtrFromString("")
	if err != nil {
		t.Fatal(err)
	}
	hwnd, _, err := procTestCreateWindowExW.Call(testWSExToolWindowNoActivate,
		uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(title)), testWSPopup, 0, 0, 64, 64, 0, 0, 0, 0)
	if hwnd == 0 {
		t.Fatalf("CreateWindowExW: %v", err)
	}
	defer func() { _, _, _ = procTestDestroyWindow.Call(hwnd) }()

	if err := e.ProtectWindow(hwnd); err != nil {
		t.Fatalf("ProtectWindow: %v", err)
	}
	var affinity uint32
	if r, _, err := procTestGetWindowDispAffinity.Call(hwnd, uintptr(unsafe.Pointer(&affinity))); r == 0 {
		t.Fatalf("GetWindowDisplayAffinity: %v", err)
	}
	if affinity != testWDAExcludeFromCapture {
		t.Errorf("affinity %#x, want WDA_EXCLUDEFROMCAPTURE", affinity)
	}
	if err := e.ProtectWindow(hwnd); err != nil {
		t.Errorf("a second ProtectWindow: %v", err)
	}

	desktop, _, _ := procTestGetDesktopWindow.Call()
	wantError(t, e.ProtectWindow(desktop), media.CodeInvalid, "", "protect_window")
}

// windowsBuild is RtlGetVersion's build number.
func windowsBuild() uint32 {
	return windows.RtlGetVersion().BuildNumber
}
