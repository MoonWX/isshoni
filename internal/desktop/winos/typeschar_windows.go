//go:build windows

package winos

import (
	"unsafe"

	"github.com/MoonWX/isshoni/internal/desktop/hotkey"
)

var (
	procToUnicodeEx              = modUser32.NewProc("ToUnicodeEx")
	procMapVirtualKeyExW         = modUser32.NewProc("MapVirtualKeyExW")
	procGetKeyboardLayout        = modUser32.NewProc("GetKeyboardLayout")
	procGetForegroundWindow      = modUser32.NewProc("GetForegroundWindow")
	procGetWindowThreadProcessId = modUser32.NewProc("GetWindowThreadProcessId")
)

// The virtual keys of the modifiers, and ToUnicodeEx's flag that leaves the keyboard state alone (Windows 10 1607+).
const (
	vkShift          = 0x10
	vkControl        = 0x11
	vkMenu           = 0x12
	keyDown          = 0x80
	mapvkVKToVSC     = 0
	toUnicodeNoState = 0x4
)

// TypesChar reports whether typing k produces a character in the keyboard layout of the foreground window's thread
// (03 §11.2: the hotkey field's warning, and the info line when the hotkeys are registered): ToUnicodeEx with the
// modifiers held, which leaves the dead-key state alone. The character itself is never returned or logged.
func (*Hooks) TypesChar(k hotkey.Combo) bool {
	if k.Validate() != nil {
		return false
	}
	fg, _, _ := procGetForegroundWindow.Call()
	tid := uintptr(0)
	if fg != 0 {
		tid, _, _ = procGetWindowThreadProcessId.Call(fg, 0)
	}
	layout, _, _ := procGetKeyboardLayout.Call(tid)
	vk := uintptr(k.VirtualKey())
	scan, _, _ := procMapVirtualKeyExW.Call(vk, mapvkVKToVSC, layout)
	var state [256]byte
	if k.Ctrl {
		state[vkControl] = keyDown
	}
	if k.Alt {
		state[vkMenu] = keyDown
	}
	if k.Shift {
		state[vkShift] = keyDown
	}
	var buf [8]uint16
	n, _, _ := procToUnicodeEx.Call(vk, scan, uintptr(unsafe.Pointer(&state[0])), uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)), toUnicodeNoState, layout)
	// A control character (Ctrl+A is U+0001) types nothing.
	return int32(n) > 0 && buf[0] >= 0x20 && buf[0] != 0x7F //nolint:gosec // G115: ToUnicodeEx returns an int
}
