//go:build windows

package winos

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows/registry"
)

// TestAutostartRunValue is the autostart round trip on a test key (03 §16.1; README WA84; the app's own value under
// HKCU\...\Run is never touched): on writes "<exe>" --autostart as REG_SZ and reads back as there, off deletes it,
// off again and a missing key are no error, and a value of another type doesn't count.
func TestAutostartRunValue(t *testing.T) {
	if appRun.key != `Software\Microsoft\Windows\CurrentVersion\Run` || appRun.name != "isshoni" {
		t.Fatalf("the app's Run value is %+v", appRun)
	}
	r := runEntry{key: `Software\isshoni-test-` + randomID(t), name: RunValue}
	t.Cleanup(func() { _ = registry.DeleteKey(registry.CURRENT_USER, r.key) })
	if _, ok, err := r.get(); ok || err != nil {
		t.Fatalf("no key: %v %v", ok, err)
	}
	if err := r.set(false, ""); err != nil {
		t.Fatalf("off without a key: %v", err)
	}
	exe := `C:\Program Files\isshoni\isshoni.exe`
	if err := r.set(true, exe); err != nil {
		t.Fatal(err)
	}
	data, ok, err := r.get()
	if err != nil || !ok || data != `"C:\Program Files\isshoni\isshoni.exe" --autostart` {
		t.Fatalf("on: %q %v %v", data, ok, err)
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, r.key, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = k.Close() }()
	if _, typ, err := k.GetStringValue(r.name); err != nil || typ != registry.SZ {
		t.Fatalf("the value's type %d, %v", typ, err)
	}
	if err := r.set(false, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := r.get(); ok || err != nil {
		t.Fatalf("off: %v %v", ok, err)
	}
	if err := r.set(false, ""); err != nil {
		t.Fatalf("off again: %v", err)
	}
	if err := k.SetDWordValue(r.name, 1); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := r.get(); ok || err != nil {
		t.Fatalf("a DWORD: %v %v", ok, err)
	}
	if err := r.set(true, "isshoni.exe"); !errors.Is(err, errBadExe) {
		t.Fatalf("a relative exe: %v", err)
	}
}
