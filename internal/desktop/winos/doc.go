// Package winos is the desktop app's Windows layer (docs/m2/03-desktop-app.md, "03" below, §4.3, §4.5): the OSHooks
// of internal/desktop/app (the winos thread with hotkeys, the Sharing pill, WDA, autostart, power and session
// events, keep-awake and the OS settings pages), single instance, the running mutex, the registry values the
// installer leaves (PendingServer, PendingRoom, UpdateChecks), the process AppUserModelID and the shell relaunch
// (relaunch*.go: RelaunchUnderShell, 03 D16, §13.4 step 2; README WA77). darwin and linux get stubs (oshooks_other.go and the *_other.go files), so the package builds and its
// tests run on every OS.
//
// What is real so far: single instance (03 §13.3; the mutex and pipe of 03 §3.2's W6 fallback), the running mutex
// (§13.4 step 4), the registry values (§13.2, §14.2), the AppUserModelID and OSBuild (README WA41); the winos
// thread with its hidden window (§4.3: hotkey registration and WM_HOTKEY, lock and unlock, suspend and resume, the
// end of the session with its 2 s handshake, display changes), keep-awake on that thread, WDA_EXCLUDEFROMCAPTURE
// and its reset, the OS settings pages and OpenPath (README WA68); Action Center (toasts.go: RemoveToasts takes a
// toast that is no longer relevant out of it, 03 §15; WF13); the Sharing pill on the winos thread (pill_windows.go, with
// its geometry in pill.go) and the keyboard layout check of the hotkeys (TypesChar; README WA80). Autostart (WA84)
// returns "not implemented" on Windows until its slice.
//
// Names (README §4 "Naming"): the single-instance id io.isshoni.desktop, the running mutex
// Local\io.isshoni.desktop.running, the registry key HKCU\Software\isshoni.
package winos

import "github.com/MoonWX/isshoni/internal/desktop/app"

// The names of README §4 "Naming".
const (
	InstanceID   = "io.isshoni.desktop"               // single instance (03 §13.3) and AppUserModelID (§15)
	RunningMutex = `Local\io.isshoni.desktop.running` // held by the first instance until it exits (03 §13.4)
	RegistryKey  = `Software\isshoni`                 // under HKEY_CURRENT_USER (06 §2.3)
)

var (
	_ app.OSHooks      = (*Hooks)(nil)
	_ app.PendingStore = (*Registry)(nil)
	// The app finds the toast removal by a type assertion, like the other optional hooks.
	_ interface {
		RemoveToasts(ids ...string) (int, error)
	} = (*Hooks)(nil)
)

// nopPill is the pill of the OSes without one (and of Windows when the winos thread didn't start): Set does nothing
// and Actions never delivers.
type nopPill struct{}

func (nopPill) Set(app.PillState)              {}
func (nopPill) Actions() <-chan app.PillAction { return nil }
