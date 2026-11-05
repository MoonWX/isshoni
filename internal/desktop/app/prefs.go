package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"time"

	"github.com/MoonWX/isshoni/internal/desktop/hotkey"
	"github.com/MoonWX/isshoni/internal/desktop/settings"
	"github.com/MoonWX/isshoni/internal/update"
)

// The settings' side effects (03 §14.2, §14.4, §16.1; README WA84): the bound Settings.Update saves a patch to
// settings.json and the app makes what changed take effect. Autostart goes to the OS first (SetAutostart: the Run
// value), so a value Windows refused is never saved; the rest follows the saved settings (SettingsChanged): the main
// window's capture exclusion, the registered hotkeys of a running share, the pill's mode, the log level, and a change
// of updates.check or updates.channel: 06's Enabled and Channel are read before every request, but the status the
// UI and the tray show only changes with a check, so the app runs one at once (off: StateOff, no request). The
// notifications switch is read where it is used (the toasts).

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

// syncAutostart makes settings.json's autostart say what the OS does (Run's start): the Run value can go without the
// app (06's uninstaller deletes it and a keep-data reinstall keeps settings.json; another tool may remove it), and
// Settings → App would then show the switch on while Windows doesn't start the app, and turning it on would change
// nothing. The OS is the truth; the app never writes the Run value behind the user's back. Where the OS has no
// autostart (Autostart's error) nothing changes.
func (a *App) syncAutostart() {
	on, err := a.d.OS.Autostart()
	if err != nil {
		if !errors.Is(err, ErrNotSupported) {
			a.log.Info("app: the autostart value couldn't be read", "err", err)
		}
		return
	}
	if on == a.d.Settings.Get().Autostart {
		return
	}
	if err := a.d.Settings.Update(func(s *settings.Settings) { s.Autostart = on }); err != nil {
		a.log.Warn("app: settings.json's autostart wasn't brought in line with the OS", "on", on, "err", err)
		return
	}
	a.log.Info("app: settings.json's autostart follows the OS's Run value", "on", on)
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
	if next.Updates.Check != prev.Updates.Check || next.Updates.Channel != prev.Updates.Channel {
		a.recheckUpdates()
	}
}

// updatesRecheckTimeout bounds the check a change of the update settings runs (a download included).
const updatesRecheckTimeout = 45 * time.Second

// recheckUpdates runs 06's CheckNow in the background after a change of the update settings: turned off, it
// publishes StateOff (the ready offer and the tray's item go); on, or another channel, it checks now.
func (a *App) recheckUpdates() {
	u := a.d.Updater
	if u == nil || a.ctx.Err() != nil {
		return
	}
	a.wg.Go(func() {
		ctx, cancel := context.WithTimeout(a.ctx, updatesRecheckTimeout)
		defer cancel()
		if _, err := u.CheckNow(ctx); err != nil && a.ctx.Err() == nil {
			a.log.Info("app: the check after a change of the update settings failed", "code", update.CodeOf(err))
		}
	})
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
