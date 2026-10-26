//go:build windows

package winos

import (
	"errors"

	"golang.org/x/sys/windows"

	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/hotkey"
)

// errNotImplemented is returned by the OSHooks methods whose slices haven't landed yet.
var errNotImplemented = errors.New("winos: not implemented yet")

// Hooks is app.OSHooks on Windows. So far it answers OSBuild; the hotkeys and system events (README WA80, WA68),
// the pill (WA80), WDA (WA68), autostart (WA84) and the settings pages (WA68) return errNotImplemented, and
// KeepAwake does nothing.
type Hooks struct{}

// New returns the Windows hooks. It starts nothing yet: the winos thread comes with the hotkeys (README WA80).
func New() *Hooks { return &Hooks{} }

// OSBuild is the Windows build number, such as 26100 (RtlGetVersion, which no compatibility shim changes).
func (*Hooks) OSBuild() int { return int(windows.RtlGetVersion().BuildNumber) }

func (*Hooks) RegisterHotkey(app.HotkeyID, hotkey.Combo) error { return errNotImplemented }
func (*Hooks) UnregisterHotkey(app.HotkeyID)                   {}
func (*Hooks) Hotkeys() <-chan app.HotkeyID                    { return nil }
func (*Hooks) Pill() app.Pill                                  { return nopPill{} }
func (*Hooks) ExcludeFromCapture(uintptr) error                { return errNotImplemented }
func (*Hooks) SetAutostart(bool, string) error                 { return errNotImplemented }
func (*Hooks) Autostart() (bool, error)                        { return false, errNotImplemented }
func (*Hooks) OpenSettingsPage(app.OSPage) error               { return errNotImplemented }
func (*Hooks) KeepAwake(bool)                                  {}
func (*Hooks) Events() <-chan app.SystemEvent                  { return nil }
