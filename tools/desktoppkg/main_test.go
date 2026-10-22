package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the tests when a goroutine outlives them: ffmpeg-fetch's HTTP connections or a test server.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// runCmd runs desktoppkg with args and returns its exit code, stdout and stderr.
func runCmd(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestUsage(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name string
		args []string
		code int
		err  string
	}{
		{"no command", nil, 2, "usage: desktoppkg"},
		{"unknown command", []string{"-root", root, "frobnicate"}, 2, `unknown command "frobnicate"`},
		{"bad global flag", []string{"-nope"}, 2, "flag provided but not defined"},
		{"help", []string{"-h"}, 0, "commands:"},
		{"command help", []string{"-root", root, "ffmpeg-inputs", "-h"}, 0, "-check"},
		{"bad command flag", []string{"-root", root, "ffmpeg-fetch", "-nope"}, 2, "flag provided but not defined"},
		{"extra argument", []string{"-root", root, "ffmpeg-inputs", "x"}, 2, "takes no arguments"},
		{"http base", []string{"-root", root, "ffmpeg-fetch", "-base", "http://example.com"}, 2, "-base must be an https URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := runCmd(t, tc.args...)
			if code != tc.code || !strings.Contains(stderr, tc.err) {
				t.Errorf("exit %d, stderr %q; want exit %d and %q", code, stderr, tc.code, tc.err)
			}
		})
	}
}

// TestUsageListsEveryCommand: the usage names the whole command set of 06 §6.1.
func TestUsageListsEveryCommand(t *testing.T) {
	_, _, stderr := runCmd(t, "-h")
	for _, name := range []string{"winver", "stage", "nsis", "check", "manifest", "ffmpeg-inputs", "ffmpeg-fetch", "dll-check"} {
		if !strings.Contains(stderr, "  "+name+" ") {
			t.Errorf("usage lacks %s:\n%s", name, stderr)
		}
	}
}

// TestNotImplemented: the commands of later slices fail with exit 1 and name their slice.
func TestNotImplemented(t *testing.T) {
	root := t.TempDir()
	for cmd, slice := range map[string]string{
		"winver": "WA67", "stage": "WA67", "nsis": "WA67", "check": "WA67",
	} {
		code, _, stderr := runCmd(t, "-root", root, cmd)
		want := "desktoppkg " + cmd + ": not implemented yet (slice " + slice + ")"
		if code != 1 || !strings.Contains(stderr, want) {
			t.Errorf("%s: exit %d, stderr %q; want exit 1 and %q", cmd, code, stderr, want)
		}
	}
}

// TestFindRoot: without -root the repository root is the nearest directory whose go.mod is the main module, so
// `go -C tools run ./desktoppkg` works from tools/.
func TestFindRoot(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module "+modulePath+"\n\ngo 1.26\n")
	writeFile(t, filepath.Join(dir, "tools", "go.mod"), "module "+modulePath+"/tools\n\ngo 1.26\n")
	t.Chdir(filepath.Join(dir, "tools"))
	got, err := findRoot("")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(dir)
	if gotReal, _ := filepath.EvalSymlinks(got); gotReal != want {
		t.Errorf("findRoot = %s, want %s", got, dir)
	}

	other := t.TempDir()
	t.Chdir(other)
	if _, err := findRoot(""); err == nil || !strings.Contains(err.Error(), "pass -root") {
		t.Errorf("outside the repository: %v, want a pass -root error", err)
	}
	if got, err := findRoot("x"); err != nil || !filepath.IsAbs(got) {
		t.Errorf("findRoot(x) = %q, %v; want an absolute path", got, err)
	}
}

func writeFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0o600); err != nil { //nolint:gosec // G703: a path under t.TempDir()
		t.Fatal(err)
	}
}
