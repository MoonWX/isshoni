//go:build windows && !isshoni_hw

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/winos"
)

// The child processes of the Windows tests: the test binary run again with childEnv set (TestMain).
const (
	childEnv   = "ISSHONI_DESKTOP_TEST_CHILD"
	idEnv      = "ISSHONI_DESKTOP_TEST_ID"      // the single-instance id
	runningEnv = "ISSHONI_DESKTOP_TEST_RUNNING" // the running mutex's name
	outEnv     = "ISSHONI_DESKTOP_TEST_OUT"     // a file the child writes
)

// The child's exit codes for "loaded" and "not loaded".
const (
	exitLoaded    = 10
	exitNotLoaded = 11
	exitMutexSeen = 12
)

func TestMain(m *testing.M) {
	if role := os.Getenv(childEnv); role != "" {
		os.Exit(testChild(role))
	}
	os.Exit(m.Run())
}

// testChild runs one child role and returns its exit code. The starters use the test's own names: a test never
// touches the app's real mutexes, pipe or registry values.
func testChild(role string) int {
	switch role {
	case "dll-control", "dll-hardened":
		if role == "dll-hardened" {
			// main's first action, as main runs it.
			if err := restrictDLLSearch(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 4
			}
		}
		h, err := windows.LoadLibraryEx("im-marker.dll", 0, 0)
		if err != nil {
			return exitNotLoaded
		}
		_ = windows.FreeLibrary(h)
		return exitLoaded
	case "first", "second", "oneshot":
		return childStart(role)
	}
	return 2
}

// childStart runs 03 §13.4's order with the real single instance and running mutex under test names.
func childStart(role string) int {
	id, running, out := os.Getenv(idEnv), os.Getenv(runningEnv), os.Getenv(outEnv)
	s := &starter{
		log:    slog.New(slog.NewTextHandler(os.Stderr, nil)),
		stdout: func() io.Writer { f, _ := os.Create(out); return f }, //nolint:gosec // G703: the parent test's temp file
		singleInstance: func() (instance, error) {
			si, err := winos.NewSingleInstance(id, nil)
			if err != nil {
				return nil, err
			}
			return si, nil
		},
		holdRunning: func() error { return winos.HoldRunningMutex(running) },
		takePending: func() (app.Pending, error) { return app.Pending{}, nil },
	}
	switch role {
	case "first": // the app: take forwarded command lines (start serves them) and write the first one to out
		s.runApp = func(_ app.Args, _ []string, _ app.Pending, fwd *forwarded, inst instance) int {
			if inst == nil {
				return 5
			}
			got := make(chan []string, 1)
			fwd.start(func(args []string) {
				select {
				case got <- args:
				default:
				}
			})
			if err := os.WriteFile(out+".ready", nil, 0o600); err != nil { //nolint:gosec // G703: the test's temp file
				return 6
			}
			select {
			case args := <-got:
				data, _ := json.Marshal(args)
				if err := os.WriteFile(out, data, 0o600); err != nil { //nolint:gosec // G703: the test's temp file
					return 7
				}
				return 0
			case <-time.After(60 * time.Second):
				return 8
			}
		}
	default: // a second launch or a one-shot mode: never the app
		s.runApp = func(app.Args, []string, app.Pending, *forwarded, instance) int { return 9 }
	}
	i := slices.Index(os.Args, "--")
	code := s.start(os.Args[i+1:])
	if role == "oneshot" {
		if ok, _ := winos.MutexExists(running); ok {
			return exitMutexSeen
		}
	}
	return code
}

func randomName(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "io.isshoni.test." + hex.EncodeToString(b)
}

// childCmd runs the test binary as a child in role with extra environment; args go after "--".
func childCmd(t *testing.T, role string, env []string, args ...string) *exec.Cmd {
	//nolint:gosec // G204: this test binary itself
	cmd := exec.CommandContext(t.Context(), os.Args[0], append([]string{"-test.run=^$", "--"}, args...)...)
	cmd.Env = append(append(os.Environ(), childEnv+"="+role), env...)
	return cmd
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

// TestSingleInstanceAndRunningMutex is W6 and the running mutex on Windows (03 §22.2, README WA41): the first
// instance holds the running mutex; a one-shot mode never creates it; a second launch forwards its --open-url
// link, exits 0 within the bound, and the first instance receives it unchanged.
func TestSingleInstanceAndRunningMutex(t *testing.T) {
	id := randomName(t)
	running := `Local\` + randomName(t) + ".running"
	out := filepath.Join(t.TempDir(), "received.json")
	env := []string{idEnv + "=" + id, runningEnv + "=" + running, outEnv + "=" + out}

	first := childCmd(t, "first", env)
	var firstErr bytes.Buffer
	first.Stderr = &firstErr
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Process.Kill(); _ = first.Wait() })
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(out + ".ready"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the first instance didn't start:\n%s", firstErr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ok, err := winos.MutexExists(running); !ok || err != nil {
		t.Fatalf("the first instance doesn't hold the running mutex: %v %v", ok, err)
	}

	// A one-shot mode (its own names, so its mutex can't be the first's) creates no running mutex.
	oneShotRunning := `Local\` + randomName(t) + ".running"
	versionOut := filepath.Join(t.TempDir(), "version.json")
	oneShot := childCmd(t, "oneshot", []string{idEnv + "=" + randomName(t), runningEnv + "=" + oneShotRunning,
		outEnv + "=" + versionOut}, "--version")
	if code := exitCode(t, oneShot.Run()); code != 0 {
		t.Fatalf("--version exited %d (%d: it created the running mutex)", code, exitMutexSeen)
	}
	data, err := os.ReadFile(versionOut)
	if err != nil {
		t.Fatal(err)
	}
	checkVersionLine(t, data)

	// The second launch.
	link := []string{"--open-url", "isshoni://watch.example.com/r/lounge"}
	second := childCmd(t, "second", env, link...)
	start := time.Now()
	output, err := second.CombinedOutput()
	if code := exitCode(t, err); code != 0 {
		t.Fatalf("the second launch exited %d:\n%s", code, output)
	}
	// 03 §13.3's 2 s are the forward's; the margin is the runner starting a process.
	if d := time.Since(start); d > winos.ForwardTimeout+3*time.Second {
		t.Errorf("the second launch took %v", d)
	}
	if err := first.Wait(); err != nil {
		t.Fatalf("the first instance: %v\n%s", err, firstErr.String())
	}
	data, err = os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := json.Unmarshal(data, &got); err != nil || !slices.Equal(got, link) {
		t.Errorf("the first instance received %s (%v), want %q", data, err, link)
	}
}

// TestDLLSearchMarker is 03 §22.2's marker test (main's first action, 04 §13.3 layer 2): with im-marker.dll in the
// current directory, or in a PATH entry, a child process that ran main's first action can't load it by its bare
// name, and the marker's DllMain never runs; without the first action (the control) the same child loads it.
func TestDLLSearchMarker(t *testing.T) {
	gcc, err := exec.LookPath("gcc")
	if err != nil {
		t.Skip("no gcc to build the marker DLL: windows-go puts MinGW-w64's on PATH; elsewhere this test can't run")
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
			run := func(role string) int {
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
			if code := run("dll-control"); code != exitLoaded {
				t.Fatalf("the control couldn't load the planted DLL from the %s: the test proves nothing", where)
			}
			m := loaded()
			if len(m) != 1 || filepath.Dir(m[0]) != planted {
				t.Fatalf("the control's markers: %v", m)
			}
			for _, f := range m {
				_ = os.Remove(f)
			}
			if code := run("dll-hardened"); code != exitNotLoaded {
				t.Errorf("after main's first action im-marker.dll loaded from the %s", where)
			}
			if m := loaded(); len(m) != 0 {
				t.Errorf("the marker's DllMain ran: %v", m)
			}
		})
	}
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

// TestVersionGUISubsystem: the real isshoni.exe, built as the release is (-H windowsgui, no console), prints one JSON
// line to a redirected stdout and exits 0 at once (03 §13.4 step 1, §22.2).
func TestVersionGUISubsystem(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the app")
	}
	if raceEnabled {
		t.Skip("the race pass would build every package again; the CGO_ENABLED=0 pass of windows-go runs this")
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command on PATH")
	}
	exe := filepath.Join(t.TempDir(), "isshoni.exe")
	// -buildvcs=false: a test binary carries no VCS stamp, so the exe's --version must not either for
	// checkVersionLine's comparison with version.Version() and version.Commit() (0.0.0-dev, no commit).
	build := exec.CommandContext(t.Context(), goTool, "build", "-buildvcs=false", "-tags", "production",
		"-ldflags=-H=windowsgui", "-o", exe, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "--version") //nolint:gosec // G204: the exe built above
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatalf("isshoni.exe --version: %v\n%s", err, stderr.String())
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("--version took %v", d)
	}
	checkVersionLine(t, stdout.Bytes())
}
