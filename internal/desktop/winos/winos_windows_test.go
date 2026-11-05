//go:build windows

package winos

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/MoonWX/isshoni/internal/desktop/app"
)

// The child processes of these tests: the test binary run again with childEnv set.
const (
	childEnv = "ISSHONI_WINOS_TEST_CHILD" // "first", "second" or "mutex"
	idEnv    = "ISSHONI_WINOS_TEST_ID"
	rootEnv  = "ISSHONI_WINOS_TEST_ROOT" // the data root, whose instance.lock is the lock
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
	id, root, out := os.Getenv(idEnv), os.Getenv(rootEnv), os.Getenv(outEnv)
	switch role {
	case "first": // hold the lock, serve, write the first forwarded command line to out, exit
		si, err := NewSingleInstance(id, root, nil)
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
		si, err := NewSingleInstance(id, root, nil)
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

func childCmd(t *testing.T, role, id, root, out string, args ...string) *exec.Cmd {
	//nolint:gosec // G204: this test binary itself
	cmd := exec.CommandContext(t.Context(), os.Args[0], append([]string{"-test.run=^$", "--"}, args...)...)
	cmd.Env = append(os.Environ(), childEnv+"="+role, idEnv+"="+id, rootEnv+"="+root, outEnv+"="+out)
	return cmd
}

// newInstance is NewSingleInstance of id and root, failing the test on an error.
func newInstance(t *testing.T, id, root string, log *slog.Logger) *SingleInstance {
	t.Helper()
	si, err := NewSingleInstance(id, root, log)
	if err != nil {
		t.Fatal(err)
	}
	return si
}

// waitForName waits until si's pipe name is in instance.lock and returns it.
func waitForName(t *testing.T, si *SingleInstance) string {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if name, ok := si.readPipeName(); ok {
			return name
		}
		if time.Now().After(deadline) {
			t.Fatal("no pipe name in instance.lock while serving")
		}
	}
}

// receiveArgs waits for one command line on got and compares it with want.
func receiveArgs(t *testing.T, got <-chan []string, want ...string) {
	t.Helper()
	select {
	case g := <-got:
		if !slices.Equal(g, want) {
			t.Errorf("received %q, want %q", g, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%q wasn't received", want)
	}
}

// TestSingleInstanceLock: the lock is instance.lock in the data root (03 §13.3; README §3 item 119). Of one root the
// second Acquire is a second instance, of another root a first one; the lock is a byte range past the content, so the
// pipe's name stays readable while it is held; LockHeld sees it; Close clears the name and frees the lock.
func TestSingleInstanceLock(t *testing.T) {
	id, root := randomID(t), t.TempDir()
	if held, err := LockHeld(root); held || err != nil {
		t.Fatalf("LockHeld without a file: %v %v", held, err)
	}
	a := newInstance(t, id, root, nil)
	if first, err := a.Acquire(); err != nil || !first {
		t.Fatalf("Acquire: %v %v", first, err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if held, err := LockHeld(root); !held || err != nil {
		t.Errorf("LockHeld while held: %v %v", held, err)
	}
	if err := a.Serve(func([]string) {}); err != nil {
		t.Fatal(err)
	}
	name := waitForName(t, a)
	if !strings.HasPrefix(name, pipePrefix(id)) {
		t.Errorf("the pipe's name doesn't start with %q", pipePrefix(id))
	}
	data, err := os.ReadFile(filepath.Join(root, lockFile)) // a reader isn't blocked by the held range
	if err != nil || string(data) != name {
		t.Errorf("instance.lock holds %q (%v), want the pipe's name", data, err)
	}
	if first, err := newInstance(t, id, root, nil).Acquire(); err != nil || first {
		t.Errorf("Acquire of the same root: %v %v, want a second instance", first, err)
	}
	other := newInstance(t, id, t.TempDir(), nil)
	if first, err := other.Acquire(); err != nil || !first {
		t.Errorf("Acquire of another root: %v %v, want a first instance", first, err)
	}
	_ = other.Close()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if held, err := LockHeld(root); held || err != nil {
		t.Errorf("LockHeld after Close: %v %v", held, err)
	}
	if data, err := os.ReadFile(filepath.Join(root, lockFile)); err != nil || len(data) != 0 {
		t.Errorf("instance.lock after Close holds %d bytes (%v)", len(data), err)
	}
}

// TestSingleInstanceSecurity: instance.lock and the pipe are owned by this user's SID, set explicitly (an elevated
// process, as on CI, would otherwise give them BUILTIN\Administrators); the file is open to the user and SYSTEM only,
// its DACL protected, and the pipe to the user only. checkServer accepts the pipe by its owner, which works whether or
// not the server runs elevated (03 §13.3).
func TestSingleInstanceSecurity(t *testing.T) {
	got := make(chan []string, 4)
	a, b := serving(t, func(args []string) { got <- args })
	sd, err := windows.GetNamedSecurityInfo(filepath.Join(a.root, lockFile), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	checkSD(t, "instance.lock", sd, a.sid, []string{a.sid, "S-1-5-18"})
	if control, _, err := sd.Control(); err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Errorf("instance.lock's DACL isn't protected from inheritance: %#x %v", control, err)
	}

	// The pipe, as Forward opens it (this connection is a client the server drops: it sends nothing).
	pp, err := windows.UTF16PtrFromString(waitForName(t, a))
	if err != nil {
		t.Fatal(err)
	}
	ph, err := windows.CreateFile(pp, windows.GENERIC_WRITE|windows.READ_CONTROL, 0, nil, windows.OPEN_EXISTING,
		windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
	if err != nil {
		t.Fatalf("opening the pipe: %v", err)
	}
	psd, err := windows.GetSecurityInfo(ph, windows.SE_KERNEL_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	checkSD(t, "the pipe", psd, a.sid, []string{a.sid})
	if err := a.checkServer(ph); err != nil {
		t.Errorf("checkServer of this user's pipe: %v", err)
	}
	_ = windows.CloseHandle(ph)

	if first, err := b.Acquire(); err != nil || first {
		t.Fatalf("second Acquire: %v %v", first, err)
	}
	if err := b.Forward([]string{"--autostart"}); err != nil {
		t.Fatalf("Forward to this user's pipe: %v", err)
	}
	receiveArgs(t, got, "--autostart")
}

func checkSD(t *testing.T, what string, sd *windows.SECURITY_DESCRIPTOR, owner string, allowed []string) {
	t.Helper()
	o, _, err := sd.Owner()
	if err != nil || o == nil || o.String() != owner {
		t.Errorf("%s's owner is %v (%v), want %s", what, o, err, owner)
	}
	if got := daclSIDs(t, sd); !slices.Equal(got, allowed) {
		t.Errorf("%s's DACL allows %q, want %q", what, got, allowed)
	}
}

// daclSIDs lists the SIDs of sd's DACL's access-allowed ACEs, in order; it fails the test on any other kind of ACE.
func daclSIDs(t *testing.T, sd *windows.SECURITY_DESCRIPTOR) []string {
	t.Helper()
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("no DACL: %v", err)
	}
	var sids []string
	for i := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Fatalf("ACE %d is of type %d", i, ace.Header.AceType)
		}
		sids = append(sids, (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String())
	}
	return sids
}

func attrs(t *testing.T, sddl string) *windows.SecurityAttributes {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	return sa
}

func mustSID(t *testing.T) string {
	t.Helper()
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return u.User.Sid.String()
}

// TestSingleInstanceLockNotOurs: an instance.lock that isn't this user's plain writable file (one a copy, a restore or
// a migration of the data root left) is replaced: its name is deleted, never what it links to, and the launch takes
// the lock on a new file of its own, which a second launch of the same root sees and forwards to (03 §13.3). "owner":
// a file owned by BUILTIN\Administrators, which only a process with that group as an owner can create (an elevated
// one, as on CI; skipped otherwise); "symlink": a symbolic link to a file of the user (skipped without the right to
// create one); "hardlink": a second name of a file elsewhere; "directory": an empty folder at the name; "read-only":
// the user's own file with FILE_ATTRIBUTE_READONLY.
func TestSingleInstanceLockNotOurs(t *testing.T) {
	for _, how := range []string{"owner", "symlink", "hardlink", "directory", "read-only"} {
		t.Run(how, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, lockFile)
			target := filepath.Join(t.TempDir(), "elsewhere")
			if err := os.WriteFile(target, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			switch how {
			case "owner":
				_ = windows.CloseHandle(adminOwned(t, path, false))
			case "symlink":
				if err := os.Symlink(target, path); err != nil {
					t.Skipf("no symbolic link: %v", err)
				}
			case "hardlink":
				if err := os.Link(target, path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "read-only":
				if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o400); err != nil { // FILE_ATTRIBUTE_READONLY
					t.Fatal(err)
				}
			}
			var logs bytes.Buffer
			id := randomID(t)
			a := newInstance(t, id, root, slog.New(slog.NewTextHandler(&logs, nil)))
			if first, err := a.Acquire(); err != nil || !first {
				t.Fatalf("Acquire at %s: %v %v, want the repaired lock", how, first, err)
			}
			t.Cleanup(func() { _ = a.Close() })
			if !strings.Contains(logs.String(), "lock file was replaced") {
				t.Errorf("no warning that the lock file was replaced: %s", logs.String())
			}
			if strings.Contains(logs.String(), root) {
				t.Errorf("the log holds the path: %s", logs.String())
			}
			// The new file is a plain one of the user's, open to the user and SYSTEM only.
			fi, err := os.Lstat(path)
			if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o200 == 0 {
				t.Fatalf("instance.lock after the repair: %v %v", fi, err)
			}
			sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
				windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
			if err != nil {
				t.Fatal(err)
			}
			checkSD(t, "the new instance.lock", sd, a.sid, []string{a.sid, "S-1-5-18"})
			if data, err := os.ReadFile(target); err != nil || len(data) != 0 {
				t.Errorf("the linked file was written or deleted: %q %v", data, err)
			}
			if held, err := LockHeld(root); !held || err != nil {
				t.Errorf("LockHeld after the repair: %v %v", held, err)
			}
			got := make(chan []string, 1)
			if err := a.Serve(func(args []string) { got <- args }); err != nil {
				t.Fatal(err)
			}
			b := newInstance(t, id, root, nil)
			if first, err := b.Acquire(); err != nil || first {
				t.Fatalf("a second Acquire of the repaired root: %v %v, want a second instance", first, err)
			}
			if err := b.Forward([]string{"--autostart"}); err != nil {
				t.Fatal(err)
			}
			receiveArgs(t, got, "--autostart")
		})
	}
}

// TestSingleInstanceLockNotRepaired: an instance.lock that can't be replaced never makes a launch a second instance:
// Acquire fails without a lock, so main starts as the first instance (with a warning), and Serve then refuses (03
// §13.3). "owner, held": a file of BUILTIN\Administrators (elevated only, as on CI) that another handle keeps open,
// its range locked; "full directory": a folder at the name that holds a file.
func TestSingleInstanceLockNotRepaired(t *testing.T) {
	for _, how := range []string{"owner, held", "full directory"} {
		t.Run(how, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, lockFile)
			switch how {
			case "owner, held":
				h := adminOwned(t, path, true)
				t.Cleanup(func() { _ = windows.CloseHandle(h) })
			case "full directory":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "kept"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var logs bytes.Buffer
			si := newInstance(t, randomID(t), root, slog.New(slog.NewTextHandler(&logs, nil)))
			first, err := si.Acquire()
			if err == nil || first {
				t.Fatalf("Acquire at %s: %v %v, want an error", how, first, err)
			}
			if strings.Contains(err.Error(), root) || strings.Contains(logs.String(), root) {
				t.Errorf("the error or the log holds the path: %v %s", err, logs.String())
			}
			if err := si.Serve(func([]string) {}); !errors.Is(err, ErrNotFirst) {
				t.Errorf("Serve without the lock: %v", err)
			}
			if how == "full directory" {
				if _, err := os.Stat(filepath.Join(path, "kept")); err != nil {
					t.Errorf("the directory's file is gone: %v", err)
				}
			}
		})
	}
}

// adminOwned creates path as a file owned by BUILTIN\Administrators and open to everyone, and returns its handle;
// with lock it holds the lock's range. It skips the test when this process can't set that owner (not elevated).
func adminOwned(t *testing.T, path string, lock bool) windows.Handle {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, attrs(t, "O:BAD:P(A;;GA;;;WD)"),
		windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if errors.Is(err, windows.ERROR_INVALID_OWNER) {
		t.Skip("BUILTIN\\Administrators can't own this process's files (not elevated)")
	}
	if err != nil {
		t.Fatal(err)
	}
	if lock {
		// Another holder of the range: it must still not make this launch a second instance.
		if err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0,
			lockRange()); err != nil {
			_ = windows.CloseHandle(h)
			t.Fatal(err)
		}
	}
	return h
}

// TestSingleInstanceSharingViolation: another process that has instance.lock open for a moment without sharing writes
// (a backup, a scanner) delays Acquire, which retries the open, instead of failing it.
func TestSingleInstanceSharingViolation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, lockFile)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(200 * time.Millisecond)
		_ = windows.CloseHandle(h)
	}()
	t.Cleanup(func() { <-released })
	a := newInstance(t, randomID(t), root, nil)
	start := time.Now()
	first, err := a.Acquire()
	if err != nil || !first {
		t.Fatalf("Acquire while the file is open without sharing writes for 200 ms: %v %v", first, err)
	}
	defer func() { _ = a.Close() }()
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Errorf("Acquire returned after %v, before the file was released", d)
	}
	if !a.writable {
		t.Error("the lock file wasn't opened for writing")
	}
}

// TestSingleInstanceOldNamesIgnored: objects at the names of earlier builds (README §3 items 115, 117 and 118: the
// mutexes Global\<id>.v1.<SID> and Local\<id>.v1.<SID>, the pipe \\.\pipe\<id>.v1.<SID>), which another account could
// create, change nothing: the launch is the first instance and a second launch forwards to it.
func TestSingleInstanceOldNamesIgnored(t *testing.T) {
	id, sid := randomID(t), mustSID(t)
	for _, name := range []string{`Global\` + id + ".v1." + sid, `Local\` + id + ".v1." + sid} {
		p, err := windows.UTF16PtrFromString(name)
		if err != nil {
			t.Fatal(err)
		}
		h, err := windows.CreateEvent(nil, 1, 0, p)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = windows.CloseHandle(h) })
	}
	oldPipe, err := windows.UTF16PtrFromString(`\\.\pipe\` + id + ".v1." + sid)
	if err != nil {
		t.Fatal(err)
	}
	ph, err := windows.CreateNamedPipe(oldPipe, windows.PIPE_ACCESS_INBOUND, windows.PIPE_TYPE_BYTE, 1, 0, 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = windows.CloseHandle(ph) })
	got := make(chan []string, 1)
	_, b := serving(t, func(args []string) { got <- args })
	if first, err := b.Acquire(); err != nil || first {
		t.Fatalf("second Acquire: %v %v", first, err)
	}
	if err := b.Forward([]string{"--open-url", "isshoni://watch.example.com/"}); err != nil {
		t.Fatal(err)
	}
	receiveArgs(t, got, "--open-url", "isshoni://watch.example.com/")
}

// TestForwardStaleName: a name in instance.lock that no pipe serves (the last run's, while this one starts) is
// retried until ForwardTimeout, then Forward fails cleanly, with an error that doesn't hold the name.
func TestForwardStaleName(t *testing.T) {
	id, root := randomID(t), t.TempDir()
	a := newInstance(t, id, root, nil)
	if first, err := a.Acquire(); err != nil || !first {
		t.Fatalf("Acquire: %v %v", first, err)
	}
	t.Cleanup(func() { _ = a.Close() })
	stale, err := newPipeName(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.publish(stale); err != nil {
		t.Fatal(err)
	}
	b := newInstance(t, id, root, nil)
	if first, err := b.Acquire(); err != nil || first {
		t.Fatalf("second Acquire: %v %v", first, err)
	}
	start := time.Now()
	err = b.Forward([]string{"x"})
	if err == nil {
		t.Fatal("Forward to a stale name succeeded")
	}
	if d := time.Since(start); d < ForwardTimeout || d > ForwardTimeout+time.Second {
		t.Errorf("Forward gave up after %v, want about %v", d, ForwardTimeout)
	}
	if strings.Contains(err.Error(), stale[len(pipePrefix(id)):]) {
		t.Errorf("the error holds the name: %v", err)
	}
}

// TestForwardSquattedName: a pipe that another account created at the name in instance.lock (a stale name it listed)
// gets nothing: its owner isn't the user's SID, so Forward refuses it until ForwardTimeout (errServerNotUser). The
// squatter's pipe is owned by BUILTIN\Administrators, which only an elevated process can set (as on CI; skipped
// otherwise); TestForwardChecksTheServer covers the check itself everywhere.
func TestForwardSquattedName(t *testing.T) {
	id, root := randomID(t), t.TempDir()
	a := newInstance(t, id, root, nil)
	if first, err := a.Acquire(); err != nil || !first {
		t.Fatalf("Acquire: %v %v", first, err)
	}
	t.Cleanup(func() { _ = a.Close() })
	stale, err := newPipeName(id)
	if err != nil {
		t.Fatal(err)
	}
	p, err := windows.UTF16PtrFromString(stale)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateNamedPipe(p, windows.PIPE_ACCESS_INBOUND, windows.PIPE_TYPE_BYTE,
		windows.PIPE_UNLIMITED_INSTANCES, 0, pipeBuffer, 0, attrs(t, "O:BAD:P(A;;GA;;;WD)"))
	if errors.Is(err, windows.ERROR_INVALID_OWNER) {
		t.Skip("BUILTIN\\Administrators can't own this process's pipes (not elevated)")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = windows.CloseHandle(h) })
	if err := a.publish(stale); err != nil {
		t.Fatal(err)
	}
	b := newInstance(t, id, root, nil)
	err = b.Forward([]string{"--open-url", "isshoni://watch.example.com/r/lounge"})
	if !errors.Is(err, errServerNotUser) {
		t.Fatalf("Forward to a squatted pipe: %v, want errServerNotUser", err)
	}
	if strings.Contains(err.Error(), stale[len(pipePrefix(id)):]) {
		t.Errorf("the error holds the name: %v", err)
	}
}

// TestSingleInstanceInProcess: the second holder of the lock forwards to the first, which receives the arguments
// unchanged; Close stops serving and frees the lock.
func TestSingleInstanceInProcess(t *testing.T) {
	id, root := randomID(t), t.TempDir()
	a := newInstance(t, id, root, nil)
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
	b := newInstance(t, id, root, nil)
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
		receiveArgs(t, got, args...)
	}
	// A client that connects and sends garbage is dropped; the server keeps serving.
	if f, err := os.OpenFile(waitForName(t, a), os.O_WRONLY, 0); err == nil {
		_, _ = f.Write([]byte("not json"))
		_ = f.Close()
	}
	if err := b.Forward([]string{"after"}); err != nil {
		t.Fatal(err)
	}
	receiveArgs(t, got, "after")
	_ = b.Close()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	// The lock is free again, and nobody serves: Forward gives up within its bound.
	c := newInstance(t, id, root, nil)
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

// serving returns a first instance of a new id and data root that serves onArgs, and a second instance of the same
// id and root.
func serving(t *testing.T, onArgs func([]string)) (a, b *SingleInstance) {
	t.Helper()
	id, root := randomID(t), t.TempDir()
	a = newInstance(t, id, root, nil)
	if first, err := a.Acquire(); err != nil || !first {
		t.Fatalf("first Acquire: %v %v", first, err)
	}
	if err := a.Serve(onArgs); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, newInstance(t, id, root, nil)
}

// TestForwardChecksTheServer: a second launch forwards nothing to a pipe that isn't this user's; the pipe's owner is
// compared by SID. The other SID is set for dial only: with it, instance.lock (the user's) wouldn't be read either.
func TestForwardChecksTheServer(t *testing.T) {
	got := make(chan []string, 2)
	a, b := serving(t, func(args []string) { got <- args })
	name := waitForName(t, a)
	own := b.sid
	b.sid = "S-1-5-18" // as if this launch ran as another account than the server
	h, err := b.dial(name)
	b.sid = own
	if !errors.Is(err, errServerNotUser) {
		if err == nil {
			_ = windows.CloseHandle(h)
		}
		t.Errorf("dialled a server of another account: %v", err)
	}
	if err := b.Forward([]string{"--autostart"}); err != nil {
		t.Fatal(err)
	}
	receiveArgs(t, got, "--autostart")
}

// TestServesWhileHandling: while onArgs handles one command line, the pipe's name stays served (the next instance
// exists before the last one closes), so another launch's forward succeeds at once, while the first is still being
// handled, and arrives afterwards: the client wrote and closed its end before the server accepted it (ERROR_NO_DATA).
func TestServesWhileHandling(t *testing.T) {
	// Only the first call is held: a later one that took the same path would wait for a release nobody sends.
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	got := make(chan []string, 2)
	_, b := serving(t, func(args []string) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		got <- args
	})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	if err := b.Forward([]string{"one"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("onArgs wasn't called")
	}
	start := time.Now()
	if err := b.Forward([]string{"two"}); err != nil {
		t.Fatalf("a forward while onArgs runs: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("the forward waited %v for a pipe instance", d)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("onArgs was called %d times while the first call ran", n)
	}
	unblock()
	for _, want := range []string{"one", "two"} {
		select {
		case g := <-got:
			if !slices.Equal(g, []string{want}) {
				t.Errorf("received %q, want %q", g, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s wasn't received", want)
		}
	}
}

// TestCloseDoesntWaitForOnArgs: Close releases the lock within its bound even while onArgs blocks (the caller's
// code: a window that can't be shown any more), and a new first instance can take the lock and serve.
func TestCloseDoesntWaitForOnArgs(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})
	defer close(release)
	a, b := serving(t, func([]string) {
		entered <- struct{}{}
		<-release
	})
	if err := b.Forward([]string{"--autostart"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("onArgs wasn't called")
	}
	start := time.Now()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > closeWait+time.Second {
		t.Errorf("Close took %v", d)
	}
	if first, err := b.Acquire(); err != nil || !first {
		t.Fatalf("Acquire after Close: %v %v", first, err)
	}
	got := make(chan []string, 1)
	if err := b.Serve(func(args []string) { got <- args }); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	release <- struct{}{} // the old server's goroutine ends and closes its pipe
	select {
	case <-a.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the old server's goroutine didn't end once onArgs returned")
	}
	if err := b.Forward([]string{"after"}); err != nil { // b, now the first instance, forwards to itself
		t.Fatal(err)
	}
	select {
	case g := <-got:
		if !slices.Equal(g, []string{"after"}) {
			t.Errorf("received %q", g)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the new first instance doesn't serve")
	}
}

// TestSingleInstanceAcrossProcesses (W6, 03 §22.2): a second process forwards an --open-url link and exits 0 within
// 2 s; the first receives it.
func TestSingleInstanceAcrossProcesses(t *testing.T) {
	id, root := randomID(t), t.TempDir()
	out := filepath.Join(t.TempDir(), "received.json")
	first := childCmd(t, "first", id, root, out)
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
	second := childCmd(t, "second", id, root, out, link...)
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
		err := childCmd(t, "mutex", name, "", "").Run()
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
	defer func() { _ = h.Close() }()
	if h.OSBuild() < 10240 {
		t.Errorf("OSBuild = %d", h.OSBuild())
	}
	if err := SetAppUserModelID("io.isshoni.test"); err != nil {
		t.Error(err)
	}
	// The Run value's own round trip is TestAutostartRunValue (on a test key); here only a path it can't hold.
	if err := h.SetAutostart(true, `relative\isshoni.exe`); !errors.Is(err, errBadExe) {
		t.Errorf("SetAutostart with a relative path: %v", err)
	}
	if h.Pill() == nil || h.Hotkeys() == nil || h.Events() == nil {
		t.Error("Pill, Hotkeys or Events")
	}
}
