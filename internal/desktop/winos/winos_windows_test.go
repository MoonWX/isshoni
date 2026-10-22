//go:build windows

package winos

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"

	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/hotkey"
)

// The child processes of these tests: the test binary run again with childEnv set.
const (
	childEnv = "ISSHONI_WINOS_TEST_CHILD" // "first", "second" or "mutex"
	idEnv    = "ISSHONI_WINOS_TEST_ID"
	outEnv   = "ISSHONI_WINOS_TEST_OUT"
)

func TestMain(m *testing.M) {
	if role := os.Getenv(childEnv); role != "" {
		os.Exit(child(role))
	}
	os.Exit(m.Run())
}

// child runs one side of a cross-process test and returns its exit code.
func child(role string) int {
	id, out := os.Getenv(idEnv), os.Getenv(outEnv)
	switch role {
	case "first": // hold the lock, serve, write the first forwarded command line to out, exit
		si, err := NewSingleInstance(id, nil)
		if err != nil {
			return 3
		}
		first, err := si.Acquire()
		if err != nil || !first {
			return 4
		}
		got := make(chan []string, 1)
		if err := si.Serve(func(args []string) { got <- args }); err != nil {
			return 5
		}
		if err := os.WriteFile(out+".ready", nil, 0o600); err != nil { //nolint:gosec // G703: the test's temp file
			return 6
		}
		select {
		case args := <-got:
			data, _ := json.Marshal(args)
			if err := os.WriteFile(out, data, 0o600); err != nil { //nolint:gosec // G703: the test's temp file
				return 7
			}
		case <-time.After(30 * time.Second):
			return 8
		}
		_ = si.Close()
		return 0
	case "second": // a second launch: forward os.Args after "--" and exit 0, as main's step 3 does
		si, err := NewSingleInstance(id, nil)
		if err != nil {
			return 3
		}
		first, err := si.Acquire()
		if err != nil || first {
			return 4
		}
		i := slices.Index(os.Args, "--")
		if err := si.Forward(os.Args[i+1:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 5
		}
		return 0
	case "mutex": // report whether the named mutex exists: exit 10 yes, 11 no
		ok, err := MutexExists(id)
		switch {
		case err != nil:
			return 3
		case ok:
			return 10
		default:
			return 11
		}
	}
	return 2
}

func randomID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "io.isshoni.test." + hex.EncodeToString(b)
}

func childCmd(t *testing.T, role, id, out string, args ...string) *exec.Cmd {
	//nolint:gosec // G204: this test binary itself
	cmd := exec.CommandContext(t.Context(), os.Args[0], append([]string{"-test.run=^$", "--"}, args...)...)
	cmd.Env = append(os.Environ(), childEnv+"="+role, idEnv+"="+id, outEnv+"="+out)
	return cmd
}

// TestSingleInstanceInProcess: the second holder of the lock forwards to the first, which receives the arguments
// unchanged; Close stops serving and frees the name.
func TestSingleInstanceInProcess(t *testing.T) {
	id := randomID(t)
	a, err := NewSingleInstance(id, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := a.Acquire()
	if err != nil || !first {
		t.Fatalf("first Acquire: %v %v", first, err)
	}
	got := make(chan []string, 4)
	if err := a.Serve(func(args []string) { got <- args }); err != nil {
		t.Fatal(err)
	}
	if err := a.Serve(func([]string) {}); err == nil {
		t.Error("Serve twice")
	}
	b, err := NewSingleInstance(id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first, err := b.Acquire(); err != nil || first {
		t.Fatalf("second Acquire: %v %v", first, err)
	}
	if err := b.Serve(func([]string) {}); !errors.Is(err, ErrNotFirst) {
		t.Errorf("a second instance served: %v", err)
	}
	for _, args := range [][]string{
		{"--open-url", "isshoni://watch.example.com/r/lounge"},
		{},
		{"--server", "https://watch.example.com", "ünïcode"},
	} {
		if err := b.Forward(args); err != nil {
			t.Fatal(err)
		}
		select {
		case g := <-got:
			if !slices.Equal(g, args) {
				t.Errorf("received %q, want %q", g, args)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("nothing received")
		}
	}
	// A client that connects and sends garbage is dropped; the server keeps serving.
	name, _ := windowsPipeName(a)
	if f, err := os.OpenFile(name, os.O_WRONLY, 0); err == nil {
		_, _ = f.Write([]byte("not json"))
		_ = f.Close()
	}
	if err := b.Forward([]string{"after"}); err != nil {
		t.Fatal(err)
	}
	select {
	case g := <-got:
		if !slices.Equal(g, []string{"after"}) {
			t.Errorf("received %q", g)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the server stopped after a bad client")
	}
	_ = b.Close()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	// The name is free again, and nobody serves: Forward gives up within its bound.
	c, _ := NewSingleInstance(id, nil)
	start := time.Now()
	if err := c.Forward([]string{"x"}); err == nil {
		t.Error("Forward to nobody succeeded")
	}
	if d := time.Since(start); d > ForwardTimeout+time.Second {
		t.Errorf("Forward took %v", d)
	}
	if first, err := c.Acquire(); err != nil || !first {
		t.Errorf("Acquire after Close: %v %v", first, err)
	}
	_ = c.Close()
}

func windowsPipeName(s *SingleInstance) (string, error) { return s.pipeName, nil }

// TestSingleInstanceAcrossProcesses (W6, 03 §22.2): a second process forwards an --open-url link and exits 0 within
// 2 s; the first receives it.
func TestSingleInstanceAcrossProcesses(t *testing.T) {
	id := randomID(t)
	out := filepath.Join(t.TempDir(), "received.json")
	first := childCmd(t, "first", id, out)
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Process.Kill(); _ = first.Wait() }()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(out + ".ready"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first instance didn't start serving")
		}
		time.Sleep(20 * time.Millisecond)
	}
	link := []string{"--open-url", "isshoni://watch.example.com/r/lounge"}
	second := childCmd(t, "second", id, out, link...)
	start := time.Now()
	if output, err := second.CombinedOutput(); err != nil {
		t.Fatalf("the second instance: %v\n%s", err, output)
	}
	// The process start itself is not the forward's: allow the runner a margin over the 2 s of 03 §13.3.
	if d := time.Since(start); d > ForwardTimeout+3*time.Second {
		t.Errorf("the second instance took %v", d)
	}
	if err := first.Wait(); err != nil {
		t.Fatalf("the first instance: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := json.Unmarshal(data, &got); err != nil || !slices.Equal(got, link) {
		t.Errorf("received %s (%v), want %q", data, err, link)
	}
}

// TestRunningMutex: HoldRunningMutex makes the named mutex exist for other processes while this one runs.
func TestRunningMutex(t *testing.T) {
	name := `Local\` + randomID(t) + ".running"
	exists := func() bool {
		t.Helper()
		err := childCmd(t, "mutex", name, "").Run()
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("the mutex child: %v", err)
		}
		switch ee.ExitCode() {
		case 10:
			return true
		case 11:
			return false
		}
		t.Fatalf("the mutex child exited %d", ee.ExitCode())
		return false
	}
	if exists() {
		t.Fatal("the mutex exists before it was created")
	}
	if ok, err := MutexExists(name); ok || err != nil {
		t.Fatalf("MutexExists before: %v %v", ok, err)
	}
	if err := HoldRunningMutex(name); err != nil {
		t.Fatal(err)
	}
	if err := HoldRunningMutex(name); err != nil {
		t.Fatal(err)
	}
	if !exists() {
		t.Error("another process doesn't see the running mutex")
	}
	if ok, err := MutexExists(name); !ok || err != nil {
		t.Errorf("MutexExists after: %v %v", ok, err)
	}
}

// TestRegistryPending: TakePending reads and deletes both values, whatever they hold; UpdateChecks reads a DWORD.
func TestRegistryPending(t *testing.T) {
	path := `Software\isshoni-test-` + randomID(t)
	t.Cleanup(func() { _ = registry.DeleteKey(registry.CURRENT_USER, path) })
	r := NewRegistry(path)
	if p, err := r.TakePending(); err != nil || p != (app.Pending{}) {
		t.Fatalf("no key: %+v %v", p, err)
	}
	if _, ok := r.UpdateChecks(); ok {
		t.Error("UpdateChecks without a key")
	}
	want := app.Pending{Server: "https://watch.example.com", Room: "lounge"}
	if err := r.WritePending(want); err != nil {
		t.Fatal(err)
	}
	if p, err := r.TakePending(); err != nil || p != want {
		t.Fatalf("TakePending = %+v, %v", p, err)
	}
	if p, err := r.TakePending(); err != nil || p != (app.Pending{}) {
		t.Errorf("the values weren't deleted: %+v %v", p, err)
	}
	if err := r.WritePending(app.Pending{Server: "https://a.example.com", Room: "r"}); err != nil {
		t.Fatal(err)
	}
	if err := r.WritePending(app.Pending{Server: "https://b.example.com"}); err != nil {
		t.Fatal(err)
	}
	if p, _ := r.TakePending(); p != (app.Pending{Server: "https://b.example.com"}) {
		t.Errorf("the newest link should win and drop the old room: %+v", p)
	}

	k, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = k.Close() }()
	// Values of the wrong type read as "" and are deleted all the same.
	if err := k.SetDWordValue(valuePendingServer, 7); err != nil {
		t.Fatal(err)
	}
	if err := k.SetExpandStringValue(valuePendingRoom, "%TEMP%"); err != nil {
		t.Fatal(err)
	}
	if p, err := r.TakePending(); err != nil || p != (app.Pending{}) {
		t.Errorf("wrong types: %+v %v", p, err)
	}
	q, _ := registry.OpenKey(registry.CURRENT_USER, path, registry.QUERY_VALUE)
	names, _ := q.ReadValueNames(0)
	_ = q.Close()
	if len(names) != 0 {
		t.Errorf("values left: %v", names)
	}

	for _, tc := range []struct {
		set  func() error
		v    uint32
		want bool
	}{
		{func() error { return k.SetDWordValue(valueUpdateChecks, 0) }, 0, true},
		{func() error { return k.SetDWordValue(valueUpdateChecks, 1) }, 1, true},
		{func() error { return k.SetStringValue(valueUpdateChecks, "0") }, 0, false},
		{func() error { return k.DeleteValue(valueUpdateChecks) }, 0, false},
	} {
		if err := tc.set(); err != nil {
			t.Fatal(err)
		}
		if v, ok := r.UpdateChecks(); v != tc.v || ok != tc.want {
			t.Errorf("UpdateChecks = %d %v, want %d %v", v, ok, tc.v, tc.want)
		}
	}
}

func TestHooks(t *testing.T) {
	h := New()
	if h.OSBuild() < 10240 {
		t.Errorf("OSBuild = %d", h.OSBuild())
	}
	if err := SetAppUserModelID("io.isshoni.test"); err != nil {
		t.Error(err)
	}
	// Not there yet (README WA68, WA80, WA84): a clear error, never a silent success.
	if err := h.RegisterHotkey(app.HotkeyPause, hotkey.DefaultPause); !errors.Is(err, errNotImplemented) {
		t.Errorf("RegisterHotkey: %v", err)
	}
	if err := h.SetAutostart(true, `C:\x.exe`); !errors.Is(err, errNotImplemented) {
		t.Errorf("SetAutostart: %v", err)
	}
	if h.Pill() == nil || h.Hotkeys() != nil || h.Events() != nil {
		t.Error("Pill, Hotkeys or Events")
	}
}
