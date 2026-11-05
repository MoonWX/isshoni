package winos

import (
	"errors"
	"path/filepath"
	"strings"
)

// Autostart (03 §14.1, §16.1; README WA84): Settings → App → "Start isshoni when I sign in to Windows" writes the
// value isshoni = "<exe>" --autostart under HKCU\Software\Microsoft\Windows\CurrentVersion\Run, and turning it off
// deletes it; the uninstaller deletes it too (06 §2.3). A start with --autostart opens no window (cmd/isshoni-desktop).
const (
	// RunKey is the per-user Run key under HKEY_CURRENT_USER.
	RunKey = `Software\Microsoft\Windows\CurrentVersion\Run`
	// RunValue is the app's value in it.
	RunValue = "isshoni"
	// AutostartFlag is the argument the Run value starts the app with (internal/desktop/app's ParseArgs).
	AutostartFlag = "--autostart"
)

// errBadExe is SetAutostart's error for an exe path the Run value can't hold: not absolute, or with a quote.
var errBadExe = errors.New("winos: the exe path can't go into the Run value")

// RunCommand is the Run value's data for exe: the quoted path and --autostart (`"C:\…\isshoni.exe" --autostart`).
// exe must be an absolute path without a double quote (a Windows path can't have one).
func RunCommand(exe string) (string, error) {
	if exe == "" || !filepath.IsAbs(exe) || strings.ContainsAny(exe, "\"\x00") {
		return "", errBadExe
	}
	return `"` + exe + `" ` + AutostartFlag, nil
}
