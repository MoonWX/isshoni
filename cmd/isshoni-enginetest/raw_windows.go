//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// rawProbe calls im_probe(flags) on the engine the binding loaded and initialized: LoadLibraryEx of the same full path
// returns the same module (one more reference, never released, like the binding's own). The thread is locked around
// the call and im_last_error, which reads this thread's detail (04 §12.4).
func rawProbe(dllDir string, flags uint32) (*rawCaps, error) {
	dir := dllDir
	if dir == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		dir = filepath.Dir(exe)
	}
	h, err := windows.LoadLibraryEx(filepath.Join(dir, "isshoni-media.dll"), 0,
		windows.LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR|windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
	if err != nil {
		return nil, fmt.Errorf("loading isshoni-media.dll: %w", err)
	}
	probe, err := windows.GetProcAddress(h, "im_probe")
	if err != nil {
		return nil, fmt.Errorf("im_probe: %w", err)
	}
	lastError, err := windows.GetProcAddress(h, "im_last_error")
	if err != nil {
		return nil, fmt.Errorf("im_last_error: %w", err)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	caps := &rawCaps{StructSize: uint32(unsafe.Sizeof(rawCaps{}))}
	r, _, _ := syscall.SyscallN(probe, uintptr(flags), uintptr(unsafe.Pointer(caps)))
	if st := int32(r); st != imOK { //nolint:gosec // G115: im_status is an int32 in a register
		buf := make([]byte, 1100)
		n, _, _ := syscall.SyscallN(lastError, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		text := ""
		if m := int(int32(n)); m > 0 { //nolint:gosec // G115: im_last_error returns an int32
			text = string(buf[:min(m, len(buf)-1)])
		}
		return nil, errors.New("im_probe: status " + fmt.Sprint(st) + ": " + text)
	}
	return caps, nil
}
