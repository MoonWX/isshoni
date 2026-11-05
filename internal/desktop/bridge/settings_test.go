package bridge

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/bridge/codes"
	"github.com/MoonWX/isshoni/internal/desktop/settings"
	"github.com/MoonWX/isshoni/internal/media"
)

// WA84: Settings.Update with every field of 03 §14.2's SettingsPatch, Settings.Hotkeys and TestHotkey over the app,
// and the OpenH264 opt-in.

// settingsCore is fakeCore with the app's settings side: autostart, the side effects and the hotkeys.
type settingsCore struct {
	*fakeCore
	mu           sync.Mutex
	autostart    []bool
	autostartErr error
	changed      []settings.Settings // prev of each SettingsChanged
	tested       []string
	testErr      error
}

func (c *settingsCore) SetAutostart(on bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.autostartErr != nil {
		return c.autostartErr
	}
	c.autostart = append(c.autostart, on)
	return nil
}

func (c *settingsCore) SettingsChanged(prev settings.Settings) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.changed = append(c.changed, prev)
}

func (c *settingsCore) Hotkeys() []app.HotkeyStatus {
	return []app.HotkeyStatus{{ID: app.HotkeyPause, Combo: "Ctrl+Alt+Shift+F9", State: app.HotkeyRegistered},
		{ID: app.HotkeyStop, Combo: "Ctrl+Alt+Shift+F10", State: app.HotkeyTaken}}
}

func (c *settingsCore) TestHotkey(combo string) (app.HotkeyStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tested = append(c.tested, combo)
	if c.testErr != nil {
		return app.HotkeyStatus{}, c.testErr
	}
	return app.HotkeyStatus{ID: app.HotkeyTest, Combo: "Ctrl+Alt+Q", State: app.HotkeyIdle, TypesChar: true}, nil
}

type settingsEnv struct {
	st     *settings.Store
	core   *settingsCore
	s      *Services
	shell  *fakeShell
	mu     sync.Mutex
	events []SettingsView
}

func newSettingsEnv(t *testing.T, h264 OpenH264Codec) *settingsEnv {
	t.Helper()
	st, err := settings.Open(settings.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e := &settingsEnv{st: st, core: &settingsCore{fakeCore: &fakeCore{}}, shell: &fakeShell{}}
	srv := &fakeServers{entries: []deviceauth.ServerEntry{{Origin: originA, Name: "A"}}}
	var buf bytes.Buffer
	d := Deps{App: e.core, Servers: srv, Settings: st, Shell: e.shell, Log: testLog(&buf),
		Emit: func(name string, data any) {
			if v, ok := data.(SettingsView); ok && name == EventSettingsChanged {
				e.mu.Lock()
				e.events = append(e.events, v)
				e.mu.Unlock()
			}
		}}
	if h264 != nil {
		d.OpenH264 = h264
	}
	s, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	e.s = s
	return e
}

func ptr[T any](v T) *T { return &v }

// TestSettingsUpdate: every field of the patch is saved and comes back in the view and in settings.changed; the app
// is told what the settings were before; autostart goes to Windows first.
func TestSettingsUpdate(t *testing.T) {
	e := newSettingsEnv(t, nil)
	v, err := e.s.Settings.Update(SettingsPatch{
		CloseToTray:     ptr(true),
		Autostart:       ptr(true),
		HideFromCapture: ptr(false),
		Notifications:   ptr(false),
		Hotkeys:         &HotkeysPatch{Pause: ptr("ctrl+shift+f8")},
		Pill:            &PillPatch{Mode: ptr("always")},
		Updates:         &UpdatesPatch{Check: ptr(false), Channel: ptr("beta")},
		Tips:            &TipsPatch{FirstShareShown: ptr(true), DNDShown: ptr(true), TrayNoticeShown: ptr(true)},
		Log:             &LogPatch{Level: ptr("debug")},
		ServerPrefs:     map[string]ServerPrefsPatchItem{originA: {Preset: ptr("game")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !v.CloseToTray || !v.Autostart || v.HideFromCapture || v.Notifications || v.Hotkeys.Pause != "Ctrl+Shift+F8" ||
		v.Hotkeys.Stop != "Ctrl+Alt+Shift+F10" || v.Pill.Mode != "always" || v.Updates.Check ||
		v.Updates.Channel != "beta" || !v.Tips.FirstShareShown || !v.Tips.DNDShown || !v.Tips.TrayNoticeShown ||
		v.Log.Level != "debug" || v.ServerPrefs[originA].Preset != "game" {
		t.Fatalf("view %+v", v)
	}
	if got := e.st.Get(); got.Hotkeys.Pause != "Ctrl+Shift+F8" || !got.Autostart || got.Updates.Channel != "beta" {
		t.Fatalf("saved %+v", got)
	}
	if !slices.Equal(e.core.autostart, []bool{true}) {
		t.Fatalf("autostart calls %v", e.core.autostart)
	}
	if len(e.core.changed) != 1 || !e.core.changed[0].HideFromCapture || e.core.changed[0].Autostart {
		t.Fatalf("SettingsChanged got %+v", e.core.changed)
	}
	if len(e.events) != 1 || e.events[0].Pill.Mode != "always" {
		t.Fatalf("settings.changed %+v", e.events)
	}
	// A tip can be shown again; autostart that doesn't change isn't written again.
	if v, err := e.s.Settings.Update(SettingsPatch{Tips: &TipsPatch{FirstShareShown: ptr(false)},
		Autostart: ptr(true)}); err != nil || v.Tips.FirstShareShown || !v.Tips.DNDShown {
		t.Fatalf("tips again: %+v %v", v, err)
	}
	if len(e.core.autostart) != 1 {
		t.Fatalf("autostart written again: %v", e.core.autostart)
	}
	// An empty patch changes nothing and answers the settings.
	if v, err := e.s.Settings.Update(SettingsPatch{}); err != nil || v.Pill.Mode != "always" {
		t.Fatalf("empty patch: %+v %v", v, err)
	}
}

// TestSettingsChangedFromStore: a change made through the store itself, not Settings.Update (the app's tips, the
// pill's position), sends settings.changed too (03 §6.3, WA84-R6); an Update sends it once; a write that changes
// nothing and a closed bridge send none.
func TestSettingsChangedFromStore(t *testing.T) {
	e := newSettingsEnv(t, nil)
	count := func() int {
		e.mu.Lock()
		defer e.mu.Unlock()
		return len(e.events)
	}
	if err := e.st.Update(func(st *settings.Settings) { st.Tips.DNDShown = true }); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	if len(e.events) != 1 || !e.events[0].Tips.DNDShown {
		e.mu.Unlock()
		t.Fatalf("settings.changed after a store change %+v", e.events)
	}
	e.mu.Unlock()
	if _, err := e.s.Settings.Update(SettingsPatch{Pill: &PillPatch{Mode: ptr("always")}}); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 2 {
		t.Fatalf("settings.changed after an Update: %d events, want 2", n)
	}
	if err := e.st.Update(func(st *settings.Settings) { st.Tips.DNDShown = true }); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 2 {
		t.Fatalf("settings.changed after a write that changed nothing: %d events", n)
	}
	e.s.Close()
	if err := e.st.Update(func(st *settings.Settings) { st.Tips.DNDShown = false }); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 2 {
		t.Fatalf("settings.changed after Close: %d events", n)
	}
}

// TestSettingsUpdateInvalid: each bad value is invalid_argument with its field, and nothing is saved, autostart
// included (written to Windows and set back when the rest was refused).
func TestSettingsUpdateInvalid(t *testing.T) {
	e := newSettingsEnv(t, nil)
	before := e.st.Get()
	for _, tc := range []struct {
		name  string
		patch SettingsPatch
		field string
	}{
		{"wizardDone false", SettingsPatch{WizardDone: ptr(false)}, "wizardDone"},
		{"pill mode", SettingsPatch{Pill: &PillPatch{Mode: ptr("never")}}, "pill.mode"},
		{"channel", SettingsPatch{Updates: &UpdatesPatch{Channel: ptr("nightly")}}, "updates.channel"},
		{"log level", SettingsPatch{Log: &LogPatch{Level: ptr("trace")}}, "log.level"},
		{"hotkey", SettingsPatch{Hotkeys: &HotkeysPatch{Stop: ptr("Win+X")}}, "hotkeys.stop"},
		{"same hotkeys", SettingsPatch{Hotkeys: &HotkeysPatch{Stop: ptr("ctrl+alt+shift+f9")}}, "hotkeys.stop"},
		{"same hotkeys (pause)", SettingsPatch{Hotkeys: &HotkeysPatch{Pause: ptr("Ctrl+Alt+Shift+F10")}},
			"hotkeys.pause"},
		{"unknown server", SettingsPatch{ServerPrefs: map[string]ServerPrefsPatchItem{
			"https://other.example.com": {Preset: ptr("game")}}}, "serverPrefs"},
		{"preset", SettingsPatch{ServerPrefs: map[string]ServerPrefsPatchItem{originA: {Preset: ptr("fast")}}},
			"serverPrefs.preset"},
		{"autostart with a bad pill mode", SettingsPatch{Autostart: ptr(true), Pill: &PillPatch{Mode: ptr("x")}},
			"pill.mode"},
	} {
		_, err := e.s.Settings.Update(tc.patch)
		var be *Error
		if !errors.As(err, &be) || be.Code != codes.CodeInvalidArgument || be.Params["field"] != tc.field {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	after := e.st.Get()
	if after.Pill.Mode != before.Pill.Mode || after.Autostart ||
		after.Hotkeys != before.Hotkeys || after.Updates != before.Updates || after.Log != before.Log {
		t.Fatalf("saved after refusals: %+v", after)
	}
	// The refused patch with autostart wrote it and set it back.
	if !slices.Equal(e.core.autostart, []bool{true, false}) {
		t.Fatalf("autostart calls %v", e.core.autostart)
	}
	if len(e.core.changed) != 0 || len(e.events) != 0 {
		t.Fatal("a refused patch was applied or announced")
	}
}

// TestSettingsUpdateAutostartRefused: Windows refused the Run value: io_error and nothing saved; an OS without
// autostart: not_implemented.
func TestSettingsUpdateAutostartRefused(t *testing.T) {
	e := newSettingsEnv(t, nil)
	e.core.autostartErr = app.ErrIO
	if _, err := e.s.Settings.Update(SettingsPatch{Autostart: ptr(true), CloseToTray: ptr(true)}); codeOf(t,
		err) != codes.CodeIOError {
		t.Fatalf("refused: %v", err)
	}
	if st := e.st.Get(); st.Autostart || st.CloseToTray {
		t.Fatalf("saved %+v", st)
	}
	e.core.autostartErr = app.ErrNotImplemented
	if _, err := e.s.Settings.Update(SettingsPatch{Autostart: ptr(true)}); codeOf(t, err) != codes.CodeNotImplemented {
		t.Fatalf("no autostart: %v", err)
	}
}

// TestSettingsHotkeys: Hotkeys maps the app's states; TestHotkey its answer with typesChar, and invalid_argument
// (field combo) for one that doesn't parse.
func TestSettingsHotkeys(t *testing.T) {
	e := newSettingsEnv(t, nil)
	hs, err := e.s.Settings.Hotkeys()
	if err != nil || len(hs) != 2 || hs[0] != (HotkeyStatus{ID: "pause", Combo: "Ctrl+Alt+Shift+F9",
		State: "registered"}) || hs[1].State != "taken" {
		t.Fatalf("Hotkeys = %+v, %v", hs, err)
	}
	h, err := e.s.Settings.TestHotkey("ctrl+alt+q")
	if err != nil || h != (HotkeyStatus{ID: "test", Combo: "Ctrl+Alt+Q", State: "idle", TypesChar: true}) {
		t.Fatalf("TestHotkey = %+v, %v", h, err)
	}
	e.core.testErr = app.ErrInvalidArgument
	_, err = e.s.Settings.TestHotkey("Win+X")
	var be *Error
	if !errors.As(err, &be) || be.Code != codes.CodeInvalidArgument || be.Params["field"] != "combo" {
		t.Fatalf("TestHotkey invalid: %v", err)
	}
}

// fakeH264 is the OpenH264 opt-in over a flag.
type fakeH264 struct {
	mu        sync.Mutex
	installed bool
	enableErr error
	calls     []string
}

func (f *fakeH264) Version() string { return "2.7.0" }
func (f *fakeH264) Installed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.installed
}
func (f *fakeH264) Enable(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "enable")
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("no deadline")
	}
	if f.enableErr != nil {
		return f.enableErr
	}
	f.installed = true
	return nil
}
func (f *fakeH264) Disable() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "disable")
	f.installed = false
	return nil
}

// TestOpenH264OptIn (03 §14.4, 04 §9.9): unavailable without a codec; off, on after Enable, off after Disable; a
// download that failed is engine.dependency with its reason; Disable is busy while a share is live.
func TestOpenH264OptIn(t *testing.T) {
	if st, err := newSettingsEnv(t, nil).s.Settings.OpenH264(); err != nil ||
		st != (OpenH264Status{State: "unavailable"}) {
		t.Fatalf("without a codec: %+v %v", st, err)
	}
	h := &fakeH264{}
	e := newSettingsEnv(t, h)
	if st, err := e.s.Settings.OpenH264(); err != nil || st != (OpenH264Status{State: "off", Version: "2.7.0"}) {
		t.Fatalf("off: %+v %v", st, err)
	}
	h.enableErr = &media.Error{Code: media.CodeDependency, Reason: "download", Op: "install_openh264"}
	_, err := e.s.Settings.EnableOpenH264(context.Background())
	var be *Error
	if !errors.As(err, &be) || be.Code != "engine.dependency" || be.Params["reason"] != "download" {
		t.Fatalf("a failed download: %v", err)
	}
	h.enableErr = nil
	if st, err := e.s.Settings.EnableOpenH264(context.Background()); err != nil || st.State != "on" {
		t.Fatalf("enable: %+v %v", st, err)
	}
	e.core.fakeCore.mu.Lock()
	e.core.live = true
	e.core.fakeCore.mu.Unlock()
	if _, err := e.s.Settings.DisableOpenH264(); codeOf(t, err) != codes.CodeBusy {
		t.Fatalf("disable while live: %v", err)
	}
	e.core.fakeCore.mu.Lock()
	e.core.live = false
	e.core.fakeCore.mu.Unlock()
	if st, err := e.s.Settings.DisableOpenH264(); err != nil || st.State != "off" {
		t.Fatalf("disable: %+v %v", st, err)
	}
	if !slices.Equal(h.calls, []string{"enable", "enable", "disable"}) {
		t.Fatalf("calls %v", h.calls)
	}
}
