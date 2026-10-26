//go:build !windows

package winos

import (
	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/hotkey"
)

// Hooks is app.OSHooks on darwin and linux in M2: no-op stubs (03 §6.1). Hotkeys register without ever firing, the
// pill never shows, WDA is a no-op; autostart and the settings pages answer app.ErrNotSupported.
type Hooks struct{}

// New returns the stub hooks.
func New() *Hooks { return &Hooks{} }

func (*Hooks) OSBuild() int                                    { return 0 }
func (*Hooks) RegisterHotkey(app.HotkeyID, hotkey.Combo) error { return nil }
func (*Hooks) UnregisterHotkey(app.HotkeyID)                   {}
func (*Hooks) Hotkeys() <-chan app.HotkeyID                    { return nil }
func (*Hooks) Pill() app.Pill                                  { return nopPill{} }
func (*Hooks) ExcludeFromCapture(uintptr) error                { return nil }
func (*Hooks) SetAutostart(bool, string) error                 { return app.ErrNotSupported }
func (*Hooks) Autostart() (bool, error)                        { return false, nil }
func (*Hooks) OpenSettingsPage(app.OSPage) error               { return app.ErrNotSupported }
func (*Hooks) KeepAwake(bool)                                  {}
func (*Hooks) Events() <-chan app.SystemEvent                  { return nil }
