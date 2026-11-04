package main

import (
	"strings"
	"testing"
)

// TestWinver is 06 §5.3's table, plus the edges: 65535 is the largest part, N wraps at 65000, any other
// prerelease is 0, build metadata doesn't count, and what isn't SemVer is refused.
func TestWinver(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"0.2.0", "0.2.0.65000"},
		{"0.2.0-rc.2", "0.2.0.2"},
		{"0.0.1-ci.1234", "0.0.1.1234"},
		{"0.0.0-dev+1a2b3c", "0.0.0.0"},
		{"0.0.0-dev+1a2b3c4d5e6f-dirty", "0.0.0.0"},
		{"1.2.3+build.7", "1.2.3.65000"},
		{"65535.65535.65535", "65535.65535.65535.65000"},
		{"0.0.0-ci.65000", "0.0.0.0"},
		{"0.0.0-ci.65001", "0.0.0.1"},
		{"0.0.0-ci.130123", "0.0.0.123"},
		{"0.0.0-ci.99999999999999999999999", "0.0.0.34999"}, // past uint64: (10^23-1) mod 65000
		{"0.2.0-rc.2.beta", "0.2.0.0"},
		{"0.2.0-smoke.1", "0.2.0.1"},
		{"0.2.0-alpha", "0.2.0.0"},
		{"0.0.1-smoke.1", "0.0.1.1"},
	} {
		got, err := winver(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("winver(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "1.2", "v1.2.3", "1.2.3-", "1.2.3+", "01.2.3", "1.2.3-01", "65536.0.0", "0.65536.0",
		"0.0.65536", "1.2.3.4", "1.2.3-rc..1", " 1.2.3"} {
		if got, err := winver(bad); err == nil {
			t.Errorf("winver(%q) = %q; want an error", bad, got)
		}
	}
}

func TestWinverCommand(t *testing.T) {
	root := t.TempDir()
	code, stdout, stderr := runCmd(t, "-root", root, "winver", "0.2.0-rc.2")
	if code != 0 || stdout != "0.2.0.2\n" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if code, _, stderr := runCmd(t, "-root", root, "winver"); code != 2 || !strings.Contains(stderr, "one SemVer") {
		t.Errorf("no argument: exit %d, stderr %q", code, stderr)
	}
	if code, _, stderr := runCmd(t, "-root", root, "winver", "65536.0.0"); code != 1 || !strings.Contains(stderr, "above 65535") {
		t.Errorf("65536: exit %d, stderr %q", code, stderr)
	}
}
