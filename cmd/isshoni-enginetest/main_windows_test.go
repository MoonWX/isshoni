//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// The child processes of the Windows tests: the test binary run again with childEnv set (TestMain).
const childEnv = "ISSHONI_ENGINETEST_TEST_CHILD"

// The child's exit codes for "loaded" and "not loaded".
const (
	exitLoaded    = 10
	exitNotLoaded = 11
)

func TestMain(m *testing.M) {
	if role := os.Getenv(childEnv); role != "" {
		os.Exit(testChild(role))
	}
	os.Exit(m.Run())
}

func testChild(role string) int {
	switch role {
	case "dll-control", "dll-startup":
		if role == "dll-startup" {
			// The tool's first step, as main runs it (04 §14). Its DPI step may fail in a service session; only the
			// DLL search order matters here.
			_ = processSetup()
		}
		h, err := windows.LoadLibraryEx("im-marker.dll", 0, 0)
		if err != nil {
			return exitNotLoaded
		}
		_ = windows.FreeLibrary(h)
		return exitLoaded
	}
	return 2
}

func exitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("the child didn't run: %v", err)
	}
	return ee.ExitCode()
}

// withoutEnv drops a variable, its name compared as Windows compares names (without regard to case).
func withoutEnv(env []string, name string) []string {
	out := env[:0:0]
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if !strings.EqualFold(k, name) {
			out = append(out, kv)
		}
	}
	return out
}

// TestDLLSearchMarker is the process default of 04 §13.3 layer 2 for isshoni-enginetest (04 §14, §18.2): with
// im-marker.dll in the current directory, or in a PATH entry, a child that ran the tool's startup step can't load it
// by its bare name and the marker's DllMain never runs; the control child, without the step, loads it.
func TestDLLSearchMarker(t *testing.T) {
	gcc, err := exec.LookPath("gcc")
	if err != nil {
		if os.Getenv("CI") == "true" {
			// The acceptance's "Go process-default check in CI" must not pass by skipping (ffmpeg_load_test's rule).
			t.Fatal("no gcc on PATH in CI: the Windows jobs put MinGW-w64's there to build the marker DLL")
		}
		t.Skip("no gcc to build the marker DLL: windows-native puts MinGW-w64's on PATH; elsewhere this test can't run")
	}
	dll := filepath.Join(t.TempDir(), "im-marker.dll")
	if out, err := exec.CommandContext(t.Context(), gcc, "-shared", "-O2", "-static-libgcc", "-o", dll,
		filepath.Join("testdata", "marker", "marker.c")).CombinedOutput(); err != nil {
		t.Fatalf("building the marker DLL: %v\n%s", err, out)
	}
	marker, err := os.ReadFile(dll)
	if err != nil {
		t.Fatal(err)
	}
	for _, where := range []string{"current directory", "PATH"} {
		t.Run(where, func(t *testing.T) {
			cwd, pathDir := t.TempDir(), t.TempDir()
			planted := cwd
			if where == "PATH" {
				planted = pathDir
			}
			//nolint:gosec // G703: t.TempDir()
			if err := os.WriteFile(filepath.Join(planted, "im-marker.dll"), marker, 0o600); err != nil {
				t.Fatal(err)
			}
			runChild := func(role string) int {
				t.Helper()
				cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$") //nolint:gosec // G204: this test binary
				cmd.Dir = cwd
				cmd.Env = append(withoutEnv(os.Environ(), "PATH"), "PATH="+pathDir+";"+os.Getenv("PATH"),
					childEnv+"="+role)
				out, err := cmd.CombinedOutput()
				code := exitCode(t, err)
				if code != exitLoaded && code != exitNotLoaded {
					t.Fatalf("%s exited %d:\n%s", role, code, out)
				}
				return code
			}
			loaded := func() []string {
				var m []string
				for _, dir := range []string{cwd, pathDir} {
					found, _ := filepath.Glob(filepath.Join(dir, "*.loaded"))
					m = append(m, found...)
				}
				return m
			}
			if code := runChild("dll-control"); code != exitLoaded {
				t.Fatalf("the control couldn't load the planted DLL from the %s: the test proves nothing", where)
			}
			m := loaded()
			if len(m) != 1 || filepath.Dir(m[0]) != planted {
				t.Fatalf("the control's markers: %v", m)
			}
			for _, f := range m {
				_ = os.Remove(f)
			}
			if code := runChild("dll-startup"); code != exitNotLoaded {
				t.Errorf("after the tool's startup im-marker.dll loaded from the %s", where)
			}
			if m := loaded(); len(m) != 0 {
				t.Errorf("the marker's DllMain ran: %v", m)
			}
		})
	}
}

// engineDir is the folder of the DLL the windows-native job tests (ISSHONI_TEST_ENGINE_DLL), or the test skips.
func engineDir(t *testing.T) string {
	t.Helper()
	dll := os.Getenv("ISSHONI_TEST_ENGINE_DLL")
	if dll == "" {
		t.Skip("no engine DLL to test against (04 §18.2: CI windows-native sets ISSHONI_TEST_ENGINE_DLL)")
	}
	return filepath.Dir(dll)
}

// TestProbeWithDLL runs the tool's probe, full and quick, against the win-x64-ci DLL (with its FFmpeg DLLs next to
// it): both pass 04 §14's assertions and write probe.json with every encoder.
func TestProbeWithDLL(t *testing.T) {
	dir := engineDir(t)
	for _, quick := range []bool{false, true} {
		out := t.TempDir()
		args := []string{"-dll", dir, "probe", "-out", out}
		if quick {
			args = append(args, "-quick")
		}
		code, stdout, stderr := runArgs(t, args...)
		if code != exitPass {
			t.Fatalf("%q: exit %d\n%s\n%s", args, code, stdout, stderr)
		}
		data, err := os.ReadFile(filepath.Join(out, "probe.json"))
		if err != nil {
			t.Fatal(err)
		}
		var d probeDoc
		if err := json.Unmarshal(data, &d); err != nil {
			t.Fatal(err)
		}
		names := map[string]bool{}
		for _, e := range d.Encoders {
			names[e.Name] = true
		}
		if !names["mf-sw"] || !names["openh264"] || !names["fake"] {
			t.Errorf("quick %t: encoders %v", quick, names)
		}
		if d.Full == quick {
			t.Errorf("quick %t: full %t", quick, d.Full)
		}
		t.Logf("quick %t:\n%s", quick, stdout)
	}
}

// TestCaptureWithDLL is a short capture of test:pattern with the test hooks' IM_ENC_FAKE and the test tone through
// the binding: 04 §14's assertions pass, the files are there, and -dump-aus writes q's first AUs.
func TestCaptureWithDLL(t *testing.T) {
	dir := engineDir(t)
	out := t.TempDir()
	args := []string{"-dll", dir, "capture", "-encoder", "fake", "-seconds", "3", "-audio", "tone", "-dump-aus", "4",
		"-out", out}
	code, stdout, stderr := runArgs(t, args...)
	if code != exitPass {
		t.Fatalf("%q: exit %d\n%s\n%s", args, code, stdout, stderr)
	}
	for _, f := range []string{"f.h264", "q.h264", "audio.ogg", "report.json"} {
		if st, err := os.Stat(filepath.Join(out, f)); err != nil || st.Size() == 0 {
			t.Errorf("%s: %v", f, err)
		}
	}
	aus, _ := filepath.Glob(filepath.Join(out, "aus", "fake", "*.h264"))
	if len(aus) != 4 {
		t.Errorf("-dump-aus 4 wrote %v", aus)
	}
	t.Log(stdout)
}
