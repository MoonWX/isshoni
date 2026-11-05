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

// TestSingleInstanceNamesOfThisUser: NewSingleInstance names the mutex and the pipe after this process's user (03
// §13.3; README WF19), the mutex in the namespace every session shares, and the serving pipe has that name below
// \\.\pipe\ while the mutex exists by its name.
func TestSingleInstanceNamesOfThisUser(t *testing.T) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	id := randomID(t)
	si, err := NewSingleInstance(id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := `Global\` + id + ".v1." + u.User.Sid.String(); si.mutexName != want {
		t.Errorf("mutex %q, want %q", si.mutexName, want)
	}
	want := `\\.\pipe\` + id + ".v1." + u.User.Sid.String()
	if si.pipeName != want {
		t.Errorf("pipe %q, want %q", si.pipeName, want)
	}
	if first, err := si.Acquire(); err != nil || !first {
		t.Fatalf("Acquire: %v %v", first, err)
	}
	t.Cleanup(func() { _ = si.Close() })
	if ok, err := MutexExists(si.mutexName); !ok || err != nil {
		t.Errorf("MutexExists(%q) while held: %v %v", si.mutexName, ok, err)
	}
	if err := si.Serve(func([]string) {}); err != nil {
		t.Fatal(err)
	}
	pattern, err := windows.UTF16PtrFromString(want)
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
			t.Fatalf("no pipe %s while serving: %v", want, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSingleInstanceMutexOwner: the first instance's mutex is owned by this user's SID, set explicitly (an elevated
// process, as on CI, would otherwise give it BUILTIN\Administrators), and open to the user and SYSTEM only; a second
// instance of the user reads that owner through a SYNCHRONIZE|READ_CONTROL handle and is a second instance of the
// per-user pair (03 §13.3; README §3 item 117).
func TestSingleInstanceMutexOwner(t *testing.T) {
	a, b := serving(t, func([]string) {})
	p, err := windows.UTF16PtrFromString(a.userNames.mutex)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.OpenMutex(windows.READ_CONTROL, false, p)
	if err != nil {
		t.Fatalf("OpenMutex(READ_CONTROL): %v", err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	sd, err := windows.GetSecurityInfo(h, windows.SE_KERNEL_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || owner.String() != a.sid {
		t.Errorf("the mutex's owner is %v (%v), want the user's SID", owner, err)
	}
	if got, want := daclSIDs(t, sd), []string{a.sid, "S-1-5-18"}; !slices.Equal(got, want) {
		t.Errorf("the mutex's DACL allows %q, want %q (the user and SYSTEM)", got, want)
	}
	if !a.ownedByUser(h) {
		t.Error("ownedByUser: false for this user's own mutex")
	}
	if first, err := b.Acquire(); err != nil || first {
		t.Fatalf("second Acquire: %v %v", first, err)
	}
	if b.mutexName != a.userNames.mutex || b.pipeName != a.userNames.pipe {
		t.Errorf("the second instance took %s and %s, want the per-user pair", b.mutexName, b.pipeName)
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

// lockedBuffer is a log destination for a logger that more than one goroutine writes to.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// squat takes name the way another account could, before this user's first launch, and returns the squatter's
// handle: "denied", a mutex of a DACL that lets this user read its owner but not SYNCHRONIZE; "event", an event of
// that name; "owner", a mutex open to everyone but owned by BUILTIN\Administrators, which only a process with that
// group as an owner can create (an elevated one, as on CI; the test is skipped otherwise).
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

// TestSingleInstanceSquatted: another account that holds the per-user mutex's name (README §3 item 117) doesn't keep
// this user's app from starting: the launch logs a warning that names the squatted name without the SID, falls back
// to the per-session pair, and a second launch in the session forwards to it there. With the per-session name taken
// too, Acquire fails, so main starts without a lock.
func TestSingleInstanceSquatted(t *testing.T) {
	for _, how := range []string{"denied", "event", "owner"} {
		t.Run(how, func(t *testing.T) {
			id := randomID(t)
			var log lockedBuffer
			a, err := NewSingleInstance(id, slog.New(slog.NewTextHandler(&log, nil)))
			if err != nil {
				t.Fatal(err)
			}
			squat(t, how, a.userNames.mutex)
			first, err := a.Acquire()
			if err != nil || !first {
				t.Fatalf("Acquire with the per-user name squatted: %v %v", first, err)
			}
			t.Cleanup(func() { _ = a.Close() })
			if a.mutexName != a.sessionNames.mutex || a.pipeName != a.sessionNames.pipe {
				t.Errorf("took %s and %s, want the per-session pair %+v", a.mutexName, a.pipeName, a.sessionNames)
			}
			out := log.String() // the text handler quotes the name: its backslash is doubled
			if !strings.Contains(out, "another account holds this user's single-instance mutex name") ||
				!strings.Contains(out, "level=WARN") || !strings.Contains(out, `Global\\`+id+".v1.<user SID>") {
				t.Errorf("no warning: %s", out)
			}
			if strings.Contains(out, a.sid) {
				t.Errorf("the warning holds the SID: %s", out)
			}
			got := make(chan []string, 1)
			if err := a.Serve(func(args []string) { got <- args }); err != nil {
				t.Fatal(err)
			}
			b, err := NewSingleInstance(id, nil)
			if err != nil {
				t.Fatal(err)
			}
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
				t.Fatal("nothing received on the per-session pipe")
			}
		})
	}
	t.Run("both", func(t *testing.T) {
		si, err := NewSingleInstance(randomID(t), slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		squat(t, "event", si.userNames.mutex)
		squat(t, "event", si.sessionNames.mutex)
		if first, err := si.Acquire(); err == nil {
			t.Errorf("Acquire with both names squatted: %v, want an error", first)
		}
		if err := si.Serve(func([]string) {}); !errors.Is(err, ErrNotFirst) {
			t.Errorf("Serve without the lock: %v", err)
		}
	})
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

// serving returns a first instance of a new id that serves onArgs, and a second instance of the same id.
func serving(t *testing.T, onArgs func([]string)) (a, b *SingleInstance) {
	t.Helper()
	id := randomID(t)
	a, err := NewSingleInstance(id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first, err := a.Acquire(); err != nil || !first {
		t.Fatalf("first Acquire: %v %v", first, err)
	}
	if err := a.Serve(onArgs); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	b, err = NewSingleInstance(id, nil)
	if err != nil {
		t.Fatal(err)
	}
	return a, b
}

// TestForwardChecksTheServer: a second launch forwards nothing to a pipe whose server runs as another account (one
// that created the name while no first instance held it); the account is compared by SID.
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
