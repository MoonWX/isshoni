//go:build windows || (darwin && cgo)

package main

// webview2Min is the oldest WebView2 runtime the app starts with: Evergreen 120.0 (03 §5.4). The installer's
// WEBVIEW2_MIN (06 §6.5, common.nsh) must equal it, so a PC the installer accepts never gets this app's message box.
// The check itself is Windows-only (webview2_windows.go).
const webview2Min = "120.0.0.0"
