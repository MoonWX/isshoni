//go:build windows

package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// The fake installer is this test binary, copied into the updates folder as the pending installer: started with
// fakeInstallerEnv set, TestMain runs runFakeInstaller instead of the tests. It records its arguments, waits a
// moment, creates Local\io.isshoni.desktop.installing as installer.nsi's .onInit does, and holds it until the test
// releases it.
const (
	fakeOutEnv     = "UPDATE_TEST_FAKE_OUT"     // where it writes its arguments, as JSON
	fakeReleaseEnv = "UPDATE_TEST_FAKE_RELEASE" // a file whose existence ends it
	fakeDelayEnv   = "UPDATE_TEST_FAKE_DELAY"   // how long it waits before it creates the mutex
)

func init() { fakeInstallerMain = runFakeInstaller }

func runFakeInstaller() int {
	data, err := json.Marshal(os.Args[1:])
	if err != nil {
		return 2
	}
	out := os.Getenv(fakeOutEnv)
	if err := os.WriteFile(out+".tmp", data, 0o600); err != nil { //nolint:gosec // G703: the test's temp folder
		return 2
	}
	if err := os.Rename(out+".tmp", out); err != nil { //nolint:gosec // G703: the test's temp folder
		return 2
	}
	if d, err := time.ParseDuration(os.Getenv(fakeDelayEnv)); err == nil {
		time.Sleep(d)
	}
	name, err := windows.UTF16PtrFromString(installingMutex)
	if err != nil {
		return 2
	}
	h, err := windows.CreateMutex(nil, false, name)
	if h == 0 {
		return 2
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return 8 // another setup runs (06 §6.4)
	}
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(os.Getenv(fakeReleaseEnv)); err == nil { //nolint:gosec // G703: the test's temp folder
			return 0
		}
	}
	return 0
}

// Apply starts the installer with exactly /S /UPDATE /RELAUNCH and returns only once the installer has created its
// mutex (06 §8.7, §16).
func TestApplyStartsInstaller(t *testing.T) {
	if ok, err := installingExists(); ok || err != nil {
		t.Skipf("Local\\io.isshoni.desktop.installing exists already (%v): an isshoni setup runs on this machine", err)
	}
	data := t.TempDir()
	dir := filepath.Join(data, "updates")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The pending installer: a copy of this test binary.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	name := "isshoni-desktop_0.2.1_windows_amd64_setup.exe"
	size, sum := copyFile(t, self, filepath.Join(dir, name))
	st := emptyState()
	st.Pending = &pendingRec{Version: "0.2.1", File: name, Size: size, SHA256: sum,
		ExpiresAt: wireTime{time.Now().Add(24 * time.Hour)}}
	stateData, err := marshalState(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateName), stateData, 0o600); err != nil {
		t.Fatal(err)
	}

	out, release := filepath.Join(data, "args.json"), filepath.Join(data, "release")
	t.Setenv(fakeInstallerEnv, "1")
	t.Setenv(fakeOutEnv, out)
	t.Setenv(fakeReleaseEnv, release)
	t.Setenv(fakeDelayEnv, "500ms")
	defer func() { // let the fake installer go, and wait until its mutex is gone
		_ = os.WriteFile(release, nil, 0o600)
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if ok, _ := installingExists(); !ok {
				return
			}
		}
		t.Error("the fake installer still holds its mutex")
	}()

	sys := defaultSystem()
	sys.rename = os.Rename
	sys.installed = func() (bool, error) { return true, nil } // this test binary is not in InstallDir
	logs := &logRecorder{}
	u, err := newUpdater(Options{CurrentVersion: "0.2.0", DataDir: data, Logger: logs.logger(),
		Enabled: func() bool { return true }}, env{
		signers: func() ([]TrustedKey, error) { return keys(newTestSigner(t, "isshoni-update-1")), nil },
		ep:      defaultEndpoints(), sys: sys,
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := u.PendingAtStartup(false)
	if err != nil || p == nil {
		t.Fatalf("PendingAtStartup = %+v, %v", p, err)
	}
	start := time.Now()
	if err := u.Apply(t.Context(), p, true); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	took := time.Since(start)
	if ok, err := installingExists(); !ok || err != nil {
		t.Errorf("Apply returned before the installer's mutex existed (%v)", err)
	}
	if took < 400*time.Millisecond || took > installingWait {
		t.Errorf("Apply took %v; the fake installer creates the mutex after 500 ms", took)
	}
	var args []string
	if err := json.Unmarshal(mustRead(t, out), &args); err != nil {
		t.Fatal(err)
	}
	if want := []string{"/S", "/UPDATE", "/RELAUNCH"}; !slices.Equal(args, want) {
		t.Errorf("the installer got %q, want exactly %q", args, want)
	}
	if a := u.state().Pending.Attempts; a != 1 {
		t.Errorf("attempts %d, want 1", a)
	}
	if n := logs.count(slog.LevelWarn); n != 0 {
		t.Errorf("WARN lines: %v", logs.lines())
	}
}

func copyFile(t *testing.T, from, to string) (size int64, sum string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	size, err = io.Copy(io.MultiWriter(out, h), in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	return size, hex.EncodeToString(h.Sum(nil))
}

// Without the mutex, the launcher returns nil with one WARN after 10 s (an injected mutex probe, the bubble's clock).
func TestLauncherTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := &logRecorder{}
		var started []string
		l := launcher{
			start: func(_ context.Context, path string, args []string) error {
				started = append([]string{path}, args...)
				return nil
			},
			exists: func() (bool, error) { return false, nil },
		}
		start := time.Now()
		if err := l.run(t.Context(), `C:\x\setup.exe`, []string{"/S", "/UPDATE", "/RELAUNCH"}, logs.logger()); err != nil {
			t.Fatalf("run = %v, want nil", err)
		}
		if d := time.Since(start); d != installingWait {
			t.Errorf("run returned after %v, want %v", d, installingWait)
		}
		if n := logs.count(slog.LevelWarn); n != 1 {
			t.Errorf("%d WARN lines, want 1: %v", n, logs.lines())
		}
		if want := []string{`C:\x\setup.exe`, "/S", "/UPDATE", "/RELAUNCH"}; !slices.Equal(started, want) {
			t.Errorf("started %q", started)
		}
		l.start = func(context.Context, string, []string) error { return errors.New("access denied") }
		if err := l.run(t.Context(), `C:\x\setup.exe`, nil, logs.logger()); CodeOf(err) != CodeInstallFailed {
			t.Errorf("run with a failing start = %v, want install_failed", err)
		}
	})
}

func TestMutexExists(t *testing.T) {
	name := fmt.Sprintf(`Local\isshoni-update-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	if ok, err := mutexExists(name); ok || err != nil {
		t.Fatalf("mutexExists before = %t, %v", ok, err)
	}
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateMutex(nil, false, p)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := mutexExists(name); !ok || err != nil {
		t.Errorf("mutexExists while held = %t, %v", ok, err)
	}
	if err := windows.CloseHandle(h); err != nil {
		t.Fatal(err)
	}
	if ok, err := mutexExists(name); ok || err != nil {
		t.Errorf("mutexExists after = %t, %v", ok, err)
	}
}

func TestInstalledCopy(t *testing.T) {
	// This test binary never runs from an install folder.
	if ok, err := installedCopy(); ok || err != nil {
		t.Errorf("installedCopy = %t, %v", ok, err)
	}
	for _, tt := range []struct {
		a, b string
		same bool
	}{
		{`C:\Users\a\AppData\Local\Programs\isshoni`, `c:\users\A\appdata\local\programs\ISSHONI`, true},
		{`C:\Users\a\AppData\Local\Programs\isshoni`, `C:\Users\a\AppData\Local\Programs\isshoni\`, true},
		{`C:\Users\a\AppData\Local\Programs\isshoni`, `C:\Users\a\AppData\Local\Programs\isshoni\..\isshoni`, true},
		{`C:\Users\a\AppData\Local\Programs\isshoni`, `C:\Users\a\AppData\Local\Programs\isshoni2`, false},
		{`C:\Users\a\Desktop\isshoni`, `C:\Users\a\AppData\Local\Programs\isshoni`, false},
	} {
		if got := sameFolder(tt.a, tt.b); got != tt.same {
			t.Errorf("sameFolder(%q, %q) = %t", tt.a, tt.b, got)
		}
	}
	// A short (8.3) name of an existing folder matches its long name, where the volume makes short names.
	long := t.TempDir()
	if short, err := shortPath(long); err == nil && !strings.EqualFold(short, long) && !sameFolder(short, long) {
		t.Errorf("sameFolder(%q, %q) = false", short, long)
	}
}

func shortPath(p string) (string, error) {
	in, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	buf := make([]uint16, windows.MAX_PATH)
	n, err := windows.GetShortPathName(in, &buf[0], windows.MAX_PATH)
	if err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:n]), nil
}

// InstallDir is read from a test key under HKCU, as SZ and EXPAND_SZ.
func TestInstallDir(t *testing.T) {
	path := testKeyPath(t)
	if _, err := installDir(registry.CURRENT_USER, path); !errors.Is(err, registry.ErrNotExist) {
		t.Errorf("installDir of a missing key = %v", err)
	}
	k := createTestKey(t, path)
	if _, err := installDir(registry.CURRENT_USER, path); !errors.Is(err, registry.ErrNotExist) {
		t.Errorf("installDir of a missing value = %v", err)
	}
	if err := k.SetStringValue(installDirValue, `C:\Programs\isshoni`); err != nil {
		t.Fatal(err)
	}
	if got, err := installDir(registry.CURRENT_USER, path); err != nil || got != `C:\Programs\isshoni` {
		t.Errorf("installDir = %q, %v", got, err)
	}
	if err := k.SetExpandStringValue(installDirValue, `%LOCALAPPDATA%\Programs\isshoni`); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "isshoni")
	if got, err := installDir(registry.CURRENT_USER, path); err != nil || !strings.EqualFold(got, want) {
		t.Errorf("installDir of an EXPAND_SZ = %q, %v; want %q", got, err, want)
	}
	if err := k.SetStringValue(installDirValue, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := installDir(registry.CURRENT_USER, path); !errors.Is(err, registry.ErrNotExist) {
		t.Errorf("installDir of an empty value = %v", err)
	}
}

func TestFreeSpace(t *testing.T) {
	free, err := freeSpace(t.TempDir())
	if err != nil || free == 0 {
		t.Errorf("freeSpace = %d, %v", free, err)
	}
	if _, err := freeSpace(filepath.Join(t.TempDir(), "missing", "dir")); err == nil {
		t.Error("freeSpace of a missing folder succeeded")
	}
}

// testKeyPath returns a key path under HKCU that no one else uses; the key is deleted when the test ends.
func testKeyPath(t *testing.T) string {
	t.Helper()
	root := fmt.Sprintf(`Software\isshoni-update-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { deleteKeyTree(t, registry.CURRENT_USER, root) })
	return root + `\Policies\isshoni`
}

func createTestKey(t *testing.T, path string) registry.Key {
	t.Helper()
	k, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.ALL_ACCESS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k.Close() })
	return k
}

// deleteKeyTree deletes path and its subkeys.
func deleteKeyTree(t *testing.T, root registry.Key, path string) {
	k, err := registry.OpenKey(root, path, registry.ENUMERATE_SUB_KEYS)
	if errors.Is(err, registry.ErrNotExist) {
		return
	}
	if err != nil {
		t.Errorf("open %s: %v", path, err)
		return
	}
	subs, err := k.ReadSubKeyNames(-1)
	_ = k.Close()
	if err != nil {
		t.Errorf("list %s: %v", path, err)
	}
	for _, s := range subs {
		deleteKeyTree(t, root, path+`\`+s)
	}
	if err := registry.DeleteKey(root, path); err != nil {
		t.Errorf("delete %s: %v", path, err)
	}
}
