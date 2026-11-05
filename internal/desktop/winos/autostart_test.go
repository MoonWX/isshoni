package winos

import (
	"errors"
	"path/filepath"
	"runtime"
	"testing"
)

// TestRunCommand: the Run value's data is the quoted exe and --autostart (03 §14.1); a path it can't hold is refused.
func TestRunCommand(t *testing.T) {
	exe := filepath.Join(string(filepath.Separator)+"Program Files", "isshoni", "isshoni.exe")
	if runtime.GOOS == "windows" {
		exe = `C:\Program Files\isshoni\isshoni.exe`
	}
	got, err := RunCommand(exe)
	if err != nil || got != `"`+exe+`" --autostart` {
		t.Fatalf("RunCommand = %q, %v", got, err)
	}
	for _, bad := range []string{"", "isshoni.exe", filepath.Join("relative", "isshoni.exe"), exe + `"`} {
		if _, err := RunCommand(bad); !errors.Is(err, errBadExe) {
			t.Errorf("RunCommand(%q): %v", bad, err)
		}
	}
}
