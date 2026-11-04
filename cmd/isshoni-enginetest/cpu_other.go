//go:build !windows

package main

import (
	"syscall"
	"time"
)

// processCPU is the process's user + system time so far (getrusage).
func processCPU() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
