//go:build windows && isshoni_hw

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// checklistDataDir is the WebView2 profile of the checklist run: a temporary folder, never the user's.
func checklistDataDir() (string, error) { return os.MkdirTemp("", "isshoni-checklist-webview2-") }

var procIsWindow = windows.NewLazySystemDLL("user32.dll").NewProc("IsWindow")

// isNativeWindow reports whether h is a window handle (W10).
func isNativeWindow(h uintptr) bool {
	if h == 0 {
		return false
	}
	r, _, _ := procIsWindow.Call(h)
	return r != 0
}

// pipeExists reports whether the named pipe \\.\pipe\<name> exists (W19).
func pipeExists(name string) bool {
	_, err := os.Stat(`\\.\pipe\` + name) //nolint:gosec // G703: the pipe name the caller set
	return err == nil
}
