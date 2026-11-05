//go:build windows

package winos

import (
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
	rootEnv  = "ISSHONI_WINOS_TEST_ROOT" // the data root of the instance secret
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

// testSecretFiles are the instance secret's real files on Windows (singleinstance_secret_test.go).
func testSecretFiles(t *testing.T) secretFiles { return osSecretFiles(mustSID(t)) }

// newInstance is NewSingleInstance of id and root, failing the test on an error.
func newInstance(t *testing.T, id, root string, log *slog.Logger) *SingleInstance {
	t.Helper()
	si, err := NewSingleInstance(id, root, log)
	if err != nil {
		t.Fatal(err)
	}
	return si
}

// TestSingleInstanceNamesFromSecret: NewSingleInstance names the mutex and the pipe after the instance secret of its
// data root (03 §13.3; README §3 item 118), the mutex in the namespace every session shares; the serving pipe has that
// name below \\.\pipe\ while the mutex exists by its name. Two instances of one root see each other; of two roots
// they don't.
func TestSingleInstanceNamesFromSecret(t *testing.T) {
	id, root := randomID(t), t.TempDir()
	si := newInstance(t, id, root, nil)
	secret, err := os.ReadFile(filepath.Join(root, instanceSecretFile))
	if err != nil || !validSecret(string(secret)) {
		t.Fatalf("the secret's file: %q %v", secret, err)
	}
	mutex, pipe := si.Names()
	if want := `Global\` + id + ".v1." + string(secret); mutex != want {
		t.Errorf("mutex %q, want %q", mutex, want)
	}
	if want := `\\.\pipe\` + id + ".v1." + string(secret); pipe != want {
		t.Errorf("pipe %q, want %q", pipe, want)
	}
	if strings.Contains(mutex, mustSID(t)) {
		t.Errorf("the mutex's name holds the SID: %q", mutex)
	}
	if first, err := si.Acquire(); err != nil || !first {
		t.Fatalf("Acquire: %v %v", first, err)
	}
	t.Cleanup(func() { _ = si.Close() })
	if ok, err := MutexExists(mutex); !ok || err != nil {
		t.Errorf("MutexExists(%q) while held: %v %v", mutex, ok, err)
	}
	if err := si.Serve(func([]string) {}); err != nil {
		t.Fatal(err)
	}
	pattern, err := windows.UTF16PtrFromString(pipe)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var fd windows.Win32finddata
		h, err := windows.FindFirstFile(pattern, &fd)
		if err == nil {
			_ = windows.FindClose(h)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pipe %s while serving: %v", pipe, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Same root: the second instance. Another root (another data root of the same user, as an e2e build's): its own
	// first instance.
	if first, err := newInstance(t, id, root, nil).Acquire(); err != nil || first {
		t.Errorf("Acquire of the same root: %v %v, want a second instance", first, err)
	}
	other := newInstance(t, id, t.TempDir(), nil)
	if first, err := other.Acquire(); err != nil || !first {
		t.Errorf("Acquire of another root: %v %v, want a first instance", first, err)
	}
	_ = other.Close()
}

// TestSingleInstanceMutexOwner: the first instance's mutex and pipe are owned by this user's SID, set explicitly (an
// elevated process, as on CI, would otherwise give them BUILTIN\Administrators); the mutex is open to the user and
// SYSTEM only and the pipe to the user only. A second instance of the user reads the mutex's owner through a
// SYNCHRONIZE|READ_CONTROL handle and is a second instance; its Forward reads the pipe's owner and forwards (03 §13.3).
func TestSingleInstanceMutexOwner(t *testing.T) {
	got := make(chan []string, 4)
	a, b := serving(t, func(args []string) { got <- args })
	p, err := windows.UTF16PtrFromString(a.names.mutex)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.OpenMutex(windows.READ_CONTROL, false, p)
	if err != nil {
		t.Fatalf("OpenMutex(READ_CONTROL): %v", err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	checkSecurity(t, "the mutex", h, a.sid, []string{a.sid, "S-1-5-18"})
	if !a.ownedByUser(h) {
		t.Error("ownedByUser: false for this user's own mutex")
	}

	// The pipe, as Forward opens it (this connection is a client the server drops: it sends nothing).
	pp, err := windows.UTF16PtrFromString(a.names.pipe)
	if err != nil {
		t.Fatal(err)
	}
	var ph windows.Handle
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		ph, err = windows.CreateFile(pp, windows.GENERIC_WRITE|windows.READ_CONTROL, 0, nil, windows.OPEN_EXISTING,
			windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
		if err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		t.Fatalf("opening the pipe: %v", err)
	}
	checkSecurity(t, "the pipe", ph, a.sid, []string{a.sid})
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
	select {
	case g := <-got:
		if !slices.Equal(g, []string{"--autostart"}) {
			t.Errorf("received %q", g)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing received")
	}
}

// checkSecurity fails the test unless h's owner is owner and its DACL allows exactly allowed, in order.
func checkSecurity(t *testing.T, what string, h windows.Handle, owner string, allowed []string) {
	t.Helper()
	sd, err := windows.GetSecurityInfo(h, windows.SE_KERNEL_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	checkSD(t, what, sd, owner, allowed)
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

// TestInstanceSecretFileSecurity: the secret's file is owned by the user, its DACL protected and allowing the user and
// SYSTEM only, so another account can't read it and so can't learn the names (03 §13.3; README §3 item 118).
func TestInstanceSecretFileSecurity(t *testing.T) {
	root := t.TempDir()
	newInstance(t, randomID(t), root, nil)
	sid := mustSID(t)
	sd, err := windows.GetNamedSecurityInfo(filepath.Join(root, instanceSecretFile), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	checkSD(t, "the secret's file", sd, sid, []string{sid, "S-1-5-18"})
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Errorf("the secret's DACL isn't protected from inheritance: %#x %v", control, err)
	}
	// A regenerated secret (the temp file renamed over a malformed one) keeps that security.
	if err := os.WriteFile(filepath.Join(root, instanceSecretFile), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	newInstance(t, randomID(t), root, slog.New(slog.DiscardHandler))
	sd, err = windows.GetNamedSecurityInfo(filepath.Join(root, instanceSecretFile), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	checkSD(t, "the regenerated secret's file", sd, sid, []string{sid, "S-1-5-18"})
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

// squat takes name the way another account could take a name it knows (the SID-derived names of earlier builds, or a
// leaked secret's) before this user's launch, and returns the squatter's handle: "denied", a mutex of a DACL that lets
// this user read its owner but not SYNCHRONIZE; "event", an event of that name; "owner", a mutex open to everyone but
// owned by BUILTIN\Administrators, which only a process with that group as an owner can create (an elevated one, as
// on CI; the test is skipped otherwise).
func squat(t *testing.T, how, name string) windows.Handle {
	t.Helper()
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatal(err)
	}
	attrs := func(sddl string) *windows.SecurityAttributes {
		sd, err := windows.SecurityDescriptorFromString(sddl)
		if err != nil {
			t.Fatal(err)
		}
		sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
		sa.Length = uint32(unsafe.Sizeof(*sa))
		return sa
	}
	sid := mustSID(t)
	var h windows.Handle
	switch how {
	case "denied":
		h, err = windows.CreateMutexEx(attrs("D:P(A;;GA;;;SY)(A;;RC;;;"+sid+")"), p, 0, windows.READ_CONTROL)
	case "event":
		h, err = windows.CreateEvent(nil, 1, 0, p)
	case "owner":
		h, err = windows.CreateMutexEx(attrs("O:BAD:P(A;;GA;;;WD)"), p, 0, windows.SYNCHRONIZE)
		if errors.Is(err, windows.ERROR_INVALID_OWNER) {
			t.Skip("BUILTIN\\Administrators can't own this process's objects (not elevated)")
		}
	default:
		t.Fatalf("squat %q", how)
	}
	if err != nil {
		t.Fatalf("squatting %s (%s): %v", name, how, err)
	}
	t.Cleanup(func() { _ = windows.CloseHandle(h) })
	return h
}

func mustSID(t *testing.T) string {
	t.Helper()
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return u.User.Sid.String()
}

// TestSingleInstanceOldNamesIgnored: objects at the names of earlier builds, derived from the user's SID alone (README
// §3 items 115 and 117: Global\<id>.v1.<SID>, its per-session fallback Local\<id>.v1.<SID> and the pipe
// \\.\pipe\<id>.v1.<SID>), which another account could create, change nothing: the launch is the first instance and a
// second launch forwards to it.
func TestSingleInstanceOldNamesIgnored(t *testing.T) {
	id, sid := randomID(t), mustSID(t)
	for _, name := range []string{`Global\` + id + ".v1." + sid, `Local\` + id + ".v1." + sid} {
		squat(t, "event", name)
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
	root := t.TempDir()
	a := newInstance(t, id, root, nil)
	if first, err := a.Acquire(); err != nil || !first {
		t.Fatalf("Acquire beside the old names: %v %v", first, err)
	}
	t.Cleanup(func() { _ = a.Close() })
	got := make(chan []string, 1)
	if err := a.Serve(func(args []string) { got <- args }); err != nil {
		t.Fatal(err)
	}
	b := newInstance(t, id, root, nil)
	if first, err := b.Acquire(); err != nil || first {
		t.Fatalf("second Acquire: %v %v", first, err)
	}
	if err := b.Forward([]string{"--open-url", "isshoni://watch.example.com/"}); err != nil {
		t.Fatal(err)
	}
	select {
	case g := <-got:
		if !slices.Equal(g, []string{"--open-url", "isshoni://watch.example.com/"}) {
			t.Errorf("received %q", g)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing received")
	}
}

// TestSingleInstanceNotOurs: an object under the secret name that isn't this user's mutex (only a corrupted or leaked
// secret puts one there) never makes a launch a second instance: Acquire fails without a lock, so main starts as the
// first instance, and its error doesn't hold the name (03 §13.3).
func TestSingleInstanceNotOurs(t *testing.T) {
	for _, how := range []string{"denied", "event", "owner"} {
		t.Run(how, func(t *testing.T) {
			si := newInstance(t, randomID(t), t.TempDir(), slog.New(slog.DiscardHandler))
			squat(t, how, si.names.mutex)
			first, err := si.Acquire()
			if err == nil || first {
				t.Fatalf("Acquire with the name taken: %v %v, want an error", first, err)
			}
			if strings.Contains(err.Error(), si.names.mutex[len(`Global\`):]) {
				t.Errorf("the error holds the name: %v", err)
			}
			if err := si.Serve(func([]string) {}); !errors.Is(err, ErrNotFirst) {
				t.Errorf("Serve without the lock: %v", err)
			}
		})
	}
}

// TestSingleInstanceInProcess: the second holder of the lock forwards to the first, which receives the arguments
// unchanged; Close stops serving and frees the name.
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

func windowsPipeName(s *SingleInstance) (string, error) { return s.names.pipe, nil }

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

// TestForwardChecksTheServer: a second launch forwards nothing to a pipe that isn't this user's (only a corrupted or
// leaked secret lets another account create the name); the pipe's owner is compared by SID.
func TestForwardChecksTheServer(t *testing.T) {
	got := make(chan []string, 2)
	_, b := serving(t, func(args []string) { got <- args })
	own := b.sid
	b.sid = "S-1-5-18" // as if this launch ran as another account than the server
	if err := b.Forward([]string{"--open-url", "isshoni://watch.example.com/"}); err == nil {
		t.Error("forwarded to a server of another account")
	}
	b.sid = own
	if err := b.Forward([]string{"--autostart"}); err != nil {
		t.Fatal(err)
	}
	select {
	case g := <-got:
		if !slices.Equal(g, []string{"--autostart"}) {
			t.Errorf("received %q: the refused forward arrived", g)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing received")
	}
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
	release <- struct{}{} // the old server's goroutine ends and lets go of the name
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
	case <-time.After(firstPipeWait + 5*time.Second):
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
