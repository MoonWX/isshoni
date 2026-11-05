package app

import (
	"errors"
	"fmt"
	"maps"
	"os"

	"github.com/MoonWX/isshoni/internal/desktop/hotkey"
	"github.com/MoonWX/isshoni/internal/desktop/settings"
)

// The settings' side effects (03 §14.2, §14.4, §16.1; README WA84): the bound Settings.Update saves a patch to
// settings.json and the app makes what changed take effect. Autostart goes to the OS first (SetAutostart: the Run
// value), so a value Windows refused is never saved; the rest follows the saved settings (SettingsChanged): the main
// window's capture exclusion, the registered hotkeys of a running share, the pill's mode and the log level. The
// notifications switch and the update settings are read where they are used (the toasts; 06's Enabled and Channel
// before every request).

// HotkeyTest is the id TestHotkey registers a combination under for the moment of its check (03 §11.2).
const HotkeyTest HotkeyID = "test"

// SetAutostart writes (on) or deletes (off) the autostart Run value for this exe (03 §16.1; OSHooks.SetAutostart):
// ErrNotImplemented where the OS has none (ErrNotSupported), ErrIO when the OS refused.
func (a *App) SetAutostart(on bool) error {
	exe := a.d.Exe
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return fmt.Errorf("%w: the exe's path: %w", ErrIO, err)
		}
	}
	err := a.d.OS.SetAutostart(on, exe)
	switch {
	case err == nil:
		a.log.Info("app: autostart changed", "on", on)
		return nil
	case errors.Is(err, ErrNotSupported):
		return ErrNotImplemented
	default:
		a.log.Warn("app: the autostart value wasn't changed", "on", on, "err", err)
		return fmt.Errorf("%w: %w", ErrIO, err)
	}
}

// SettingsChanged makes a saved change of settings.json take effect; prev is what the settings were before it. It
// is safe to call for any change, also one that changed nothing the app follows.
func (a *App) SettingsChanged(prev settings.Settings) {
	next := a.d.Settings.Get()
	if next.HideFromCapture != prev.HideFromCapture {
		a.applyCaptureExclusion()
	}
	if next.Hotkeys != prev.Hotkeys {
		a.reregisterHotkeys()
	}
	if next.Pill.Mode != prev.Pill.Mode {
		a.scheduleTray() // the pill's state goes out with the tray's (flushTray)
	}
	if next.Log.Level != prev.Log.Level && a.d.SetLogDebug != nil {
		a.d.SetLogDebug(next.Log.Level == settings.LogDebug)
	}
}

// reregisterHotkeys registers the hotkeys again with settings.json's combinations while a share has them registered
// (a change in Settings during a share); otherwise the next share registers the new ones.
func (a *App) reregisterHotkeys() {
	a.safe.mu.Lock()
	if !a.safe.registered {
		a.safe.mu.Unlock()
		return
	}
	for _, id := range hotkeyOrder {
		if _, ok := a.safe.combos[id]; ok {
			a.d.OS.UnregisterHotkey(id)
		}
	}
	a.registerHotkeysLocked()
	combos := maps.Clone(a.safe.combos)
	a.safe.mu.Unlock()
	a.SetTrayHotkeys(combos)
}

// TestHotkey checks a combination for the hotkey field (03 §6.2, §11.2): it registers it and at once unregisters it,
// and reports taken when another app has it, registered when it is one of the running share's own, idle otherwise
// (also where the OS has no global hotkeys); TypesChar when it types a character in the current keyboard layout. A
// combination that doesn't parse is ErrInvalidArgument.
func (a *App) TestHotkey(text string) (HotkeyStatus, error) {
	c, err := hotkey.Parse(text)
	if err != nil {
		return HotkeyStatus{}, fmt.Errorf("%w: %w", ErrInvalidArgument, err)
	}
	st := HotkeyStatus{ID: HotkeyTest, Combo: c.String(), State: HotkeyIdle}
	if ct, ok := a.d.OS.(charTyper); ok {
		st.TypesChar = ct.TypesChar(c)
	}
	a.safe.mu.Lock()
	defer a.safe.mu.Unlock()
	for _, combo := range a.safe.combos {
		if combo == st.Combo {
			st.State = HotkeyRegistered
			return st, nil
		}
	}
	switch err := a.d.OS.RegisterHotkey(HotkeyTest, c); {
	case err == nil:
		a.d.OS.UnregisterHotkey(HotkeyTest)
	case errors.Is(err, ErrHotkeyTaken):
		st.State = HotkeyTaken
	case errors.Is(err, ErrInvalidArgument):
		st.State = HotkeyInvalid
	default:
		a.log.Info("app: a hotkey couldn't be tried", "combo", st.Combo, "err", err)
	}
	return st, nil
}
