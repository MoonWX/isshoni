//go:build windows && !isshoni_hw

package main

import (
	"bytes"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/MoonWX/isshoni/internal/client/credstore"
	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/winos"
)

// These tests run the one-shot modes and step 2a against the real Credential Manager, mutexes and registry, always
// under the test's own names: a random "isshoni-test:<random>:" credential prefix, random mutex names and a random
// key under HKCU\Software. They never touch the app's credentials, mutexes or registry values.

// windowsStarter is a starter with the real running mutex under a test name and the given credential store, data
// root and installer probe; single instance, the Pending values and the app fail the test when reached.
func windowsStarter(t *testing.T, running string, store credstore.Store, root string) (*starter, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	s := &starter{
		log:              slog.New(slog.NewTextHandler(&logs, nil)),
		installerRunning: func() (bool, error) { return false, nil },
		openStore:        func() (credstore.Store, error) { return store, nil },
		openServers:      realServers(root),
		dataRoot:         func() (string, error) { return root, nil },
		removeAll:        os.RemoveAll,
		singleInstance: func() (instance, error) {
			t.Error("single instance was reached")
			return nil, errors.New("not in this test")
		},
		holdRunning: func() error { return winos.HoldRunningMutex(running) },
		takePending: func() (app.Pending, error) {
			t.Error("the Pending values were taken")
			return app.Pending{}, nil
		},
		runApp: func(app.Args, []string, app.Pending, *forwarded, instance) int {
			t.Error("the app (its window) was started")
			return exitFailed
		},
	}
	return s, &logs
}

// testCredStore opens Credential Manager under a random test prefix and deletes what is left under it at cleanup.
func testCredStore(t *testing.T) credstore.Store {
	t.Helper()
	store, err := credstore.Open(credstore.Options{Prefix: "isshoni-test:" + randomName(t) + ":"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = deleteAllCredentials(store) })
	return store
}

// TestForgetAndPurgeCredentialManager is README WA59's windows-go acceptance: with a test credential prefix,
// --forget-credentials and --purge-data leave no credential and exit 0 while offline (the one linked server
// refuses at once), --purge-data also removes the data root (here with a file still locked for a moment, as
// WebView2's exiting children keep one), and neither creates the running mutex or reaches single instance, the
// Pending values or the window.
func TestForgetAndPurgeCredentialManager(t *testing.T) {
	for _, mode := range []string{"--forget-credentials", "--purge-data"} {
		t.Run(mode, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "isshoni")
			if err := os.MkdirAll(filepath.Join(root, "WebView2"), 0o700); err != nil {
				t.Fatal(err)
			}
			origin := offlineServers(t, root)
			store := testCredStore(t)
			for _, o := range []string{origin, "https://stray.example.com"} {
				if err := store.Put(o, testCredential()); err != nil {
					t.Fatal(err)
				}
			}
			running := `Local\` + randomName(t) + ".running"
			s, logs := windowsStarter(t, running, store, root)
			// The lock is released 500 ms after the first delete attempt (not before start: ForgetAll's time must
			// not count against it), and the attempts are counted, so the run proves the retry.
			var removes atomic.Int32
			if mode == "--purge-data" {
				release := lockFile(t, filepath.Join(root, "WebView2", "lockfile"))
				var arm sync.Once
				s.removeAll = func(p string) error {
					removes.Add(1)
					arm.Do(func() { time.AfterFunc(500*time.Millisecond, release) })
					return os.RemoveAll(p)
				}
			}
			start := time.Now()
			if code := s.start([]string{mode}); code != exitOK {
				t.Fatalf("exit %d\n%s", code, logs.String())
			}
			// ForgetAll's one 5 s budget, the purge's 10 s retries at most, and a margin for a loaded runner.
			if d := time.Since(start); d > 20*time.Second {
				t.Errorf("took %v", d)
			}
			if left, err := store.List(); err != nil || len(left) != 0 {
				t.Errorf("credentials left: %v %v", left, err)
			}
			if ok, err := winos.MutexExists(running); ok || err != nil {
				t.Errorf("the running mutex exists: %v %v", ok, err)
			}
			_, err := os.Stat(root)
			if mode == "--purge-data" && !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the data root is still there: %v\n%s", err, logs.String())
			}
			if n := removes.Load(); mode == "--purge-data" && n < 2 {
				t.Errorf("%d delete attempts: the locked file didn't make the first one fail", n)
			}
			if mode == "--forget-credentials" && err != nil {
				t.Errorf("--forget-credentials touched the data root: %v", err)
			}
		})
	}
}

// lockFile creates path and keeps it open without FILE_SHARE_DELETE, so nothing can delete it (or its folder)
// until the returned function closes it.
func lockFile(t *testing.T, path string) func() {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ, nil,
		windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err == nil {
		t.Fatal("a locked file could be deleted: the test proves nothing")
	}
	var once sync.Once
	release := func() { once.Do(func() { _ = windows.CloseHandle(h) }) }
	t.Cleanup(release)
	return release
}

// TestMutexOpens: step 2a's probe opens a mutex that exists and reports "not found" as absent.
func TestMutexOpens(t *testing.T) {
	name := `Local\` + randomName(t) + ".installing"
	if ok, err := mutexOpens(name); ok || err != nil {
		t.Fatalf("before: %v %v", ok, err)
	}
	release := createMutex(t, name)
	if ok, err := mutexOpens(name); !ok || err != nil {
		t.Errorf("held: %v %v", ok, err)
	}
	release()
	if ok, err := mutexOpens(name); ok || err != nil {
		t.Errorf("after: %v %v", ok, err)
	}
}

// createMutex creates a named mutex, as the installer does, and returns the function that closes it (also run at
// cleanup; closing twice closes once).
func createMutex(t *testing.T, name string) func() {
	t.Helper()
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateMutex(nil, false, p)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(func() { _ = windows.CloseHandle(h) }) }
	t.Cleanup(release)
	return release
}

// TestInstallerHandoverWindows is 03 §22.1's step 2a row with the real probe and registry: while a mutex named as
// the installer's (a test name) exists, a link start writes PendingServer and PendingRoom into the key (replacing
// older values) and exits 0 without single instance, the running mutex or the app; once it is gone, the start
// reaches single instance.
func TestInstallerHandoverWindows(t *testing.T) {
	installing := `Local\` + randomName(t) + ".installing"
	running := `Local\` + randomName(t) + ".running"
	key := `Software\isshoni-test-` + randomName(t)
	t.Cleanup(func() { _ = registry.DeleteKey(registry.CURRENT_USER, key) })
	reg := winos.NewRegistry(key)
	if err := reg.WritePending(app.Pending{Server: "https://old.example.com", Room: "old"}); err != nil {
		t.Fatal(err)
	}
	release := createMutex(t, installing)

	s, logs := windowsStarter(t, running, nil, "")
	s.installerRunning = func() (bool, error) { return mutexOpens(installing) }
	s.writePending = reg.WritePending
	if code := s.start([]string{"--open-url", "isshoni://watch.example.com/r/lounge"}); code != exitOK {
		t.Fatalf("exit %d\n%s", code, logs.String())
	}
	if ok, err := winos.MutexExists(running); ok || err != nil {
		t.Errorf("the running mutex exists: %v %v", ok, err)
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, key, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	server, _, errServer := k.GetStringValue("PendingServer")
	room, _, errRoom := k.GetStringValue("PendingRoom")
	_ = k.Close()
	if server != "https://watch.example.com" || room != "lounge" || errServer != nil || errRoom != nil {
		t.Errorf("PendingServer %q (%v), PendingRoom %q (%v)", server, errServer, room, errRoom)
	}

	// The installer is gone: the next start goes on to single instance.
	release()
	reached := false
	s.singleInstance = func() (instance, error) { reached = true; return nil, errors.New("stop here") }
	s.holdRunning = func() error { return nil } // the real one would hold a test name for the rest of the process
	s.takePending = func() (app.Pending, error) { return app.Pending{}, nil }
	s.runApp = func(app.Args, []string, app.Pending, *forwarded, instance) int { return exitOK }
	if code := s.start(nil); code != exitOK || !reached {
		t.Errorf("exit %d, single instance reached %v", code, reached)
	}
}

// TestPurgeWhileRunning: a running first instance keeps its data root's instance.lock open without delete sharing, so
// --purge-data's removeDataRoot can't delete that file while it runs: it deletes everything else, retries until its
// budget and reports the failure (never a success with the app still running), and leaves instance.lock and the
// lock held. Once the instance is closed, the purge succeeds (03 §13.3, §13.4).
func TestPurgeWhileRunning(t *testing.T) {
	root := filepath.Join(t.TempDir(), "isshoni")
	si, err := winos.NewSingleInstance(winos.InstanceID+".test", root, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if first, err := si.Acquire(); err != nil || !first {
		t.Fatalf("Acquire: %v %v", first, err)
	}
	t.Cleanup(func() { _ = si.Close() })
	if err := os.WriteFile(filepath.Join(root, "settings.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeDataRoot(t.Context(), root, 300*time.Millisecond, 20*time.Millisecond, os.RemoveAll); err == nil {
		t.Fatal("the purge reported success while an instance holds the lock")
	}
	if _, err := os.Stat(filepath.Join(root, "settings.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("settings.json is still there: %v", err)
	}
	if held, err := winos.LockHeld(root); !held || err != nil {
		t.Errorf("the lock after the failed purge: held %v, %v", held, err)
	}
	if err := si.Close(); err != nil {
		t.Fatal(err)
	}
	if err := removeDataRoot(t.Context(), root, 5*time.Second, 20*time.Millisecond, os.RemoveAll); err != nil {
		t.Fatalf("the purge after Close: %v", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the data root is still there: %v", err)
	}
}
