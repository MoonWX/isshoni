//go:build windows

package main

import (
	"time"

	"golang.org/x/sys/windows"
)

// processCPU is the process's user + kernel time so far (GetProcessTimes).
func processCPU() time.Duration {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &creation, &exit, &kernel, &user); err != nil {
		return 0
	}
	ticks := func(f windows.Filetime) int64 { return int64(f.HighDateTime)<<32 | int64(f.LowDateTime) }
	return time.Duration(ticks(kernel)+ticks(user)) * 100 // FILETIME counts 100 ns
}
