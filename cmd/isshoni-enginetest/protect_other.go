//go:build !windows

package main

import "errors"

// createPatternWindow: protect-check's noise window is a Win32 window (protect_windows.go).
func createPatternWindow() (patternWindow, error) {
	return nil, errors.New("protect-check runs on Windows only")
}
