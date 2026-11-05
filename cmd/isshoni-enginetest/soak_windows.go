//go:build windows

package main

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32                    = windows.NewLazySystemDLL("kernel32.dll")
	procK32GetProcessMemoryInfo = kernel32.NewProc("K32GetProcessMemoryInfo")
	procGetProcessHandleCount   = kernel32.NewProc("GetProcessHandleCount")
	procGetGuiResources         = user32.NewProc("GetGuiResources")
)

// processMemoryCountersEx is PROCESS_MEMORY_COUNTERS_EX (psapi.h).
type processMemoryCountersEx struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
	PrivateUsage               uintptr
}

const grGDIObjects = 0 // GetGuiResources' GR_GDIOBJECTS

// sampleProcess reads this process's private bytes (PROCESS_MEMORY_COUNTERS_EX.PrivateUsage), handle count, GDI
// objects (GetGuiResources) and thread count (the Toolhelp process entry's cntThreads).
func sampleProcess() (procSample, error) {
	var s procSample
	self := windows.CurrentProcess()
	mc := processMemoryCountersEx{}
	mc.CB = uint32(unsafe.Sizeof(mc))
	if r, _, e := procK32GetProcessMemoryInfo.Call(uintptr(self), uintptr(unsafe.Pointer(&mc)), uintptr(mc.CB)); r == 0 {
		return s, fmt.Errorf("K32GetProcessMemoryInfo: %w", e)
	}
	s.PrivateBytes = uint64(mc.PrivateUsage)
	var handles uint32
	if r, _, e := procGetProcessHandleCount.Call(uintptr(self), uintptr(unsafe.Pointer(&handles))); r == 0 {
		return s, fmt.Errorf("GetProcessHandleCount: %w", e)
	}
	s.Handles = uint64(handles)
	// GetGuiResources returns 0 both for none and for a failure, and doesn't reset the last error on success, so a 0
	// can't be told apart: it counts as none (the pseudo handle has every access right, so the call doesn't fail).
	if err := procGetGuiResources.Find(); err != nil {
		return s, fmt.Errorf("GetGuiResources: %w", err)
	}
	r, _, _ := procGetGuiResources.Call(uintptr(self), grGDIObjects)
	s.GDIObjects = uint64(r)
	threads, err := ownThreadCount()
	if err != nil {
		return s, err
	}
	s.Threads = uint64(threads)
	return s, nil
}

// ownThreadCount is this process's cntThreads from a Toolhelp process snapshot.
func ownThreadCount() (uint32, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, fmt.Errorf("CreateToolhelp32Snapshot: %w", err)
	}
	defer func() { _ = windows.CloseHandle(snap) }()
	pid := windows.GetCurrentProcessId()
	pe := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if pe.ProcessID == pid {
			return pe.Threads, nil
		}
	}
	return 0, fmt.Errorf("Process32Next: this process is not in the snapshot: %w", err)
}
