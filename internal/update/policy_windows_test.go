//go:build windows

package update

import (
	"testing"

	"golang.org/x/sys/windows/registry"
)

// The machine policy value is read (06 §8.8, §16): a test key under HKCU stands in for HKLM\SOFTWARE\Policies\isshoni.
func TestPolicyDisabled(t *testing.T) {
	path := testKeyPath(t)
	if policyDisabled(registry.CURRENT_USER, path) {
		t.Error("a missing key turns updates off")
	}
	k := createTestKey(t, path)
	if policyDisabled(registry.CURRENT_USER, path) {
		t.Error("a missing value turns updates off")
	}
	for _, tt := range []struct {
		name string
		set  func() error
		off  bool
	}{
		{"DWORD 1", func() error { return k.SetDWordValue(policyValue, 1) }, true},
		{"DWORD 0", func() error { return k.SetDWordValue(policyValue, 0) }, false},
		{"DWORD 2", func() error { return k.SetDWordValue(policyValue, 2) }, false},
		{"QWORD 1", func() error { return k.SetQWordValue(policyValue, 1) }, false},
		{"string 1", func() error { return k.SetStringValue(policyValue, "1") }, false},
		{"DWORD 1 again", func() error { return k.SetDWordValue(policyValue, 1) }, true},
		{"deleted", func() error { return k.DeleteValue(policyValue) }, false},
	} {
		if err := tt.set(); err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if got := policyDisabled(registry.CURRENT_USER, path); got != tt.off {
			t.Errorf("%s: policyDisabled = %t, want %t", tt.name, got, tt.off)
		}
	}
	// The real value is read from HKLM; a CI machine and a developer's PC have none, but whatever it says, it must
	// not fail.
	_ = PolicyDisabled()
}
