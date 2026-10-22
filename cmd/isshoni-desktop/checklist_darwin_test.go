//go:build darwin && cgo && isshoni_hw

package main

// checklistDataDir is "" on macOS: WKWebView has no WebView2 profile.
func checklistDataDir() (string, error) { return "", nil }

// isNativeWindow reports whether h is a window handle (W10): an NSWindow pointer here.
func isNativeWindow(h uintptr) bool { return h != 0 }

// pipeExists is false on macOS: WebView2's script-debugger pipe is Windows-only (W19).
func pipeExists(string) bool { return false }

// webviewCommandLines is empty on macOS: there are no WebView2 processes (W19).
func webviewCommandLines() ([]string, error) { return nil, nil }
