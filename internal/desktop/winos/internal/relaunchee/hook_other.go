//go:build !windows

package main

import "time"

// setAfterRelaunch: no shell relaunch outside Windows.
func setAfterRelaunch(func(pid uint32, wait func(time.Duration) (uint32, bool))) {}
