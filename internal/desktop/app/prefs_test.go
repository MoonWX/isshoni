package app

import (
	"errors"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/desktop/settings"
	"github.com/MoonWX/isshoni/internal/desktop/tray"
	"github.com/MoonWX/isshoni/internal/update"
)

// WA84: autostart and the side effects of a saved settings change (03 §11.2, §11.5, §14.2, §16.1).

// autostartOS records SetAutostart.
type autostartOS struct {
	*safetyOS
	mu    sync.Mutex
	calls []string
	err   error
}

func (o *autostartOS) SetAutostart(on bool, exe string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err != nil {
		return o.err
	}
	state := "off"
	if on {
		state = "on " + exe
	}
	o.calls = append(o.calls, state)
	return nil
}

func TestSetAutostart(t *testing.T) {
	s := newSafetyEnv(t)
	o := &autostartOS{safetyOS: s.sos}
	s.app.d.OS = o
	s.app.d.Exe = `C:\Program Files\isshoni\isshoni.exe`
	if err := s.app.SetAutostart(true); err != nil {
		t.Fatal(err)
	}
	if err := s.app.SetAutostart(false); err != nil {
		t.Fatal(err)
	}
	s.app.d.Exe = ""
	if err := s.app.SetAutostart(true); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`on C:\Program Files\isshoni\isshoni.exe`, "off", "on " + exe}
	if !slices.Equal(o.calls, want) {
		t.Fatalf("calls %q, want %q", o.calls, want)
	}
	o.err = ErrNotSupported
	if err := s.app.SetAutostart(true); !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("an OS without autostart: %v", err)
	}
	o.err = errors.New("access denied")
	if err := s.app.SetAutostart(true); !errors.Is(err, ErrIO) {
		t.Fatalf("a refused write: %v", err)
	}
}

// TestSettingsChangedCapture: hideFromCapture off resets the main window's affinity, on sets it again.
func TestSettingsChangedCapture(t *testing.T) {
	s := newSysEnv(t)
	change := func(fn func(*settings.Settings)) {
		t.Helper()
		prev := s.app.d.Settings.Get()
		if err := s.app.d.Settings.Update(fn); err != nil {
			t.Fatal(err)
		}
		s.app.SettingsChanged(prev)
	}
	change(func(st *settings.Settings) { st.HideFromCapture = false })
	change(func(st *settings.Settings) { st.HideFromCapture = true })
	change(func(st *settings.Settings) { st.CloseToTray = true }) // nothing to apply
	s.os.mu.Lock()
	inc, exc := slices.Clone(s.os.included), slices.Clone(s.os.excluded)
	s.os.mu.Unlock()
	if !slices.Equal(inc, []uintptr{0x1234}) || !slices.Equal(exc, []uintptr{0x1234}) {
		t.Fatalf("included %v, excluded %v", inc, exc)
	}
}

// TestSettingsChangedHotkeys: a hotkey changed during a share is registered again with the new combination (the
// tray's menu names it); before a share nothing registers. The log level follows log.level.
func TestSettingsChangedHotkeys(t *testing.T) {
	s := newSafetyEnv(t)
	var levels []bool
	s.app.d.SetLogDebug = func(on bool) { levels = append(levels, on) }
	change := func(fn func(*settings.Settings)) {
		t.Helper()
		prev := s.app.d.Settings.Get()
		if err := s.app.d.Settings.Update(fn); err != nil {
			t.Fatal(err)
		}
		s.app.SettingsChanged(prev)
	}
	change(func(st *settings.Settings) { st.Hotkeys.Stop = "Ctrl+Alt+F11" })
	if got := s.sos.registrations(); len(got) != 0 {
		t.Fatalf("registered before a share: %q", got)
	}
	s.goLive(t)
	s.waitRegs(t, "+pause Ctrl+Alt+Shift+F9", "+stop Ctrl+Alt+F11")
	change(func(st *settings.Settings) { st.Hotkeys.Pause = "Ctrl+Shift+F8" })
	s.waitRegs(t, "+pause Ctrl+Alt+Shift+F9", "+stop Ctrl+Alt+F11", "-pause", "-stop",
		"+pause Ctrl+Shift+F8", "+stop Ctrl+Alt+F11")
	if got := statesOf(s.app.Hotkeys()); got != "pause=Ctrl+Shift+F8:registered stop=Ctrl+Alt+F11:registered" {
		t.Fatalf("hotkeys %s", got)
	}
	change(func(st *settings.Settings) { st.Log.Level = settings.LogDebug })
	change(func(st *settings.Settings) { st.Log.Level = settings.LogInfo })
	if !slices.Equal(levels, []bool{true, false}) {
		t.Fatalf("log levels %v", levels)
	}
}

// TestTestHotkey (03 §11.2): a free combination is idle and left unregistered; one another app has is taken; one the
// running share has is registered; typesChar from the layout check; one that doesn't parse is invalid argument.
func TestTestHotkey(t *testing.T) {
	s := newSafetyEnv(t)
	st, err := s.app.TestHotkey("ctrl+alt+f7")
	if err != nil || st.State != HotkeyIdle || st.Combo != "Ctrl+Alt+F7" || st.ID != HotkeyTest || st.TypesChar {
		t.Fatalf("free: %+v, %v", st, err)
	}
	if got := s.sos.registrations(); !slices.Equal(got, []string{"+test Ctrl+Alt+F7", "-test"}) {
		t.Fatalf("registrations %q", got)
	}
	s.sos.mu.Lock()
	s.sos.taken[HotkeyTest] = true
	s.sos.mu.Unlock()
	if st, err := s.app.TestHotkey("Ctrl+Alt+F7"); err != nil || st.State != HotkeyTaken {
		t.Fatalf("taken: %+v, %v", st, err)
	}
	s.sos.mu.Lock()
	s.sos.taken[HotkeyTest] = false
	s.sos.typesChar = true
	s.sos.mu.Unlock()
	if st, err := s.app.TestHotkey("Ctrl+Alt+Q"); err != nil || !st.TypesChar {
		t.Fatalf("typesChar: %+v, %v", st, err)
	}
	if _, err := s.app.TestHotkey("Win+X"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("Win+X: %v", err)
	}
	s.goLive(t)
	s.waitRegs(t, "+test Ctrl+Alt+F7", "-test", "+test Ctrl+Alt+Q", "-test",
		"+pause Ctrl+Alt+Shift+F9", "+stop Ctrl+Alt+Shift+F10")
	if st, err := s.app.TestHotkey("Ctrl+Alt+Shift+F10"); err != nil || st.State != HotkeyRegistered {
		t.Fatalf("the share's own: %+v, %v", st, err)
	}
}

// TestSettingsChangedUpdates (03 §16.2): updates.check off publishes StateOff at once (the ready offer and the
// tray's item go, Settings → Updates says "Turned off") without a request; on again, or another channel, checks now.
// A change of anything else checks nothing.
func TestSettingsChangedUpdates(t *testing.T) {
	s, u := newUpdateEnv(t)
	st := s.app.d.Settings
	u.mu.Lock()
	u.enabled = func() bool { return st.Updates().Check }
	u.mu.Unlock()
	change := func(fn func(*settings.Settings)) {
		t.Helper()
		prev := st.Get()
		if err := st.Update(fn); err != nil {
			t.Fatal(err)
		}
		s.app.SettingsChanged(prev)
	}
	u.set(readyStatus("0.3.0"))
	s.waitTray(t, func(v tray.View) bool {
		return slices.ContainsFunc(v.Menu, func(m tray.MenuItem) bool { return m.Action == tray.ActionRestart })
	}, "the tray offers the ready update")

	change(func(st *settings.Settings) { st.Updates.Check = false })
	waitUntil(t, func() bool { return u.Status().State == update.StateOff }, "StateOff after the switch went off")
	s.waitTray(t, func(v tray.View) bool {
		return !slices.ContainsFunc(v.Menu, func(m tray.MenuItem) bool { return m.Action == tray.ActionRestart })
	}, "the tray's offer goes once updates are off")
	if got := u.checked(); got != 1 {
		t.Fatalf("%d checks after off", got)
	}

	change(func(st *settings.Settings) { st.Updates.Check = true })
	waitUntil(t, func() bool { return u.checked() == 2 }, "a check after the switch went on")
	change(func(st *settings.Settings) { st.Updates.Channel = settings.ChannelBeta })
	waitUntil(t, func() bool { return u.checked() == 3 }, "a check after the channel changed")
	change(func(st *settings.Settings) { st.CloseToTray = !st.CloseToTray })
	time.Sleep(20 * time.Millisecond)
	if got := u.checked(); got != 3 {
		t.Fatalf("%d checks after a change of something else", got)
	}
}

// statusOS reports a Run value (Autostart) and records SetAutostart.
type statusOS struct {
	*autostartOS
	on  bool
	err error
}

func (o *statusOS) Autostart() (bool, error) { return o.on, o.err }

// TestSyncAutostart (03 §16.1): at Run's start settings.json's autostart follows the Run value (gone after a
// keep-data reinstall, say), without writing the Run value; an OS without autostart changes nothing.
func TestSyncAutostart(t *testing.T) {
	s := newSafetyEnv(t)
	o := &statusOS{autostartOS: &autostartOS{safetyOS: s.sos}}
	s.app.d.OS = o
	st := s.app.d.Settings
	if err := st.Update(func(st *settings.Settings) { st.Autostart = true }); err != nil {
		t.Fatal(err)
	}
	s.app.syncAutostart()
	if st.Get().Autostart {
		t.Fatal("settings say autostart while the Run value is gone")
	}
	o.on = true
	s.app.syncAutostart()
	if !st.Get().Autostart {
		t.Fatal("settings don't follow a Run value that is there")
	}
	o.on, o.err = false, ErrNotSupported
	s.app.syncAutostart()
	if !st.Get().Autostart {
		t.Fatal("an OS without autostart changed the setting")
	}
	if len(o.calls) != 0 {
		t.Fatalf("the Run value was written: %q", o.calls)
	}
}
