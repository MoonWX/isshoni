// Package winos is the desktop app's Windows layer (docs/m2/03-desktop-app.md, "03" below, §4.3, §4.5): the OSHooks
// of internal/desktop/app (the winos thread with hotkeys, the Sharing pill, WDA, autostart, power and session
// events, keep-awake and the OS settings pages), single instance, the running mutex, the registry values the
// installer leaves (PendingServer, PendingRoom, UpdateChecks), the process AppUserModelID and, later, the shell
// relaunch. darwin and linux get stubs (oshooks_other.go and the *_other.go files), so the package builds and its
// tests run on every OS.
//
// What is real so far (README WA41): single instance (03 §13.3; the mutex and pipe of 03 §3.2's W6 fallback),
// the running mutex (§13.4 step 4), the registry values (§13.2, §14.2), the AppUserModelID and OSBuild. The other
// OSHooks methods return "not implemented" on Windows until their slices (WA68, WA80, WA84) fill them.
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
)

// nopPill is the pill of the OSes without one (and of Windows until README WA80): Set does nothing and Actions
// never delivers.
type nopPill struct{}

func (nopPill) Set(app.PillState)              {}
func (nopPill) Actions() <-chan app.PillAction { return nil }
