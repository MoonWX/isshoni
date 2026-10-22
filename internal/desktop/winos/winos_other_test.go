//go:build !windows

package winos

import (
	"errors"
	"testing"

	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/hotkey"
)

// TestStubs: darwin and linux get no-op stubs (03 §6.1, §4.5).
func TestStubs(t *testing.T) {
	h := New()
	if h.OSBuild() != 0 || h.Hotkeys() != nil || h.Events() != nil {
		t.Error("OSBuild, Hotkeys or Events")
	}
	if err := h.RegisterHotkey(app.HotkeyPause, hotkey.DefaultPause); err != nil {
		t.Error(err)
	}
	h.UnregisterHotkey(app.HotkeyPause)
	h.KeepAwake(true)
	h.Pill().Set(app.PillState{Visible: true})
	if h.Pill().Actions() != nil {
		t.Error("the stub pill delivers actions")
	}
	if err := h.ExcludeFromCapture(1); err != nil {
		t.Error(err)
	}
	if err := h.SetAutostart(true, "x"); !errors.Is(err, app.ErrNotSupported) {
		t.Errorf("SetAutostart: %v", err)
	}
	if on, err := h.Autostart(); on || err != nil {
		t.Errorf("Autostart: %v %v", on, err)
	}
	if err := h.OpenSettingsPage(app.PageSound); !errors.Is(err, app.ErrNotSupported) {
		t.Errorf("OpenSettingsPage: %v", err)
	}

	si, err := NewSingleInstance(InstanceID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first, err := si.Acquire(); !first || err != nil {
		t.Errorf("Acquire: %v %v", first, err)
	}
	if err := si.Serve(func([]string) {}); err != nil {
		t.Error(err)
	}
	if err := si.Forward(nil); err == nil {
		t.Error("Forward succeeded")
	}
	if err := si.Close(); err != nil {
		t.Error(err)
	}

	if err := HoldRunningMutex(RunningMutex); err != nil {
		t.Error(err)
	}
	if ok, err := MutexExists(RunningMutex); ok || err != nil {
		t.Errorf("MutexExists: %v %v", ok, err)
	}
	r := NewRegistry(RegistryKey)
	if p, err := r.TakePending(); p != (app.Pending{}) || err != nil {
		t.Errorf("TakePending: %+v %v", p, err)
	}
	if err := r.WritePending(app.Pending{Server: "x"}); !errors.Is(err, app.ErrNotSupported) {
		t.Errorf("WritePending: %v", err)
	}
	if _, ok := r.UpdateChecks(); ok {
		t.Error("UpdateChecks")
	}
	if err := SetAppUserModelID(InstanceID); err != nil {
		t.Error(err)
	}
}
