//go:build windows

package native

import (
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// QueryPerformanceCounter and QueryPerformanceFrequency, from System32's kernel32 (x/sys/windows has no wrapper for
// them).
var (
	kernel32                      = windows.NewLazySystemDLL("kernel32.dll")
	procQueryPerformanceCounter   = kernel32.NewProc("QueryPerformanceCounter")
	procQueryPerformanceFrequency = kernel32.NewProc("QueryPerformanceFrequency")
)

// qpcFrequency is QueryPerformanceFrequency, fixed at boot: read once.
var qpcFrequency = sync.OnceValue(func() int64 {
	var f int64
	if r, _, _ := procQueryPerformanceFrequency.Call(uintptr(unsafe.Pointer(&f))); r == 0 || f <= 0 {
		// Never on Windows XP or later; a frequency of 1 GHz keeps qpcNS defined, and the clock cross-check fails Open.
		return 1e9
	}
	return f
})

// qpcNS is QueryPerformanceCounter in nanoseconds with the engine's formula (qpcToNS, 04 §12.3), so Go's and the
// engine's readings agree to well under 1 µs.
func qpcNS() int64 {
	var q int64
	_, _, _ = procQueryPerformanceCounter.Call(uintptr(unsafe.Pointer(&q))) // never fails since Windows XP
	return qpcToNS(q, qpcFrequency())
}
