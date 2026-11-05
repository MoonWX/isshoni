package winos

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestForwardMessage(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{},
		{"--open-url", "isshoni://watch.example.com/r/lounge"},
		{"--server", "https://watch.example.com", "--autostart", "", "ünïcode"},
	} {
		data, err := encodeArgs(args)
		if err != nil {
			t.Fatalf("encode %q: %v", args, err)
		}
		got, err := decodeArgs(data)
		if err != nil {
			t.Fatalf("decode %s: %v", data, err)
		}
		if len(got) != len(args) || strings.Join(got, "\x00") != strings.Join(args, "\x00") {
			t.Errorf("round trip %q → %q", args, got)
		}
	}
	long := strings.Repeat("x", maxArgBytes+1)
	many := make([]string, maxForwardArgs+1)
	for _, args := range [][]string{{long}, many} {
		if _, err := encodeArgs(args); err == nil {
			t.Errorf("encode of %d args (%d bytes) succeeded", len(args), len(args[0]))
		}
	}
	for _, data := range []string{
		``,
		`[]`,
		`{"v":2,"args":[]}`,
		`{"v":1}`,
		`{"v":1,"args":"x"}`,
		`{"v":1,"args":[1]}`,
		`{"v":1,"args":["` + long + `"]}`,
		strings.Repeat(" ", maxForwardBytes+1),
	} {
		if args, err := decodeArgs([]byte(data)); err == nil {
			t.Errorf("decode %.40q = %q, want an error", data, args)
		}
	}
}

// fakePipe is a pipe's name as serveLoop sees Windows' named pipes (byte mode): a client connects to any instance that
// no client holds yet, whether or not the server waits in accept on it, and what it wrote stays readable after it
// closed its end; with no such instance a client finds the name busy, and Forward retries.
type fakePipe struct {
	stop     chan struct{}
	stopOnce sync.Once

	mu       sync.Mutex
	free     []*fakeInstance // instances that no client holds yet, oldest first
	open     int             // instances not closed
	nextErr  error           // what create(false) returns, if not nil
	freed    int             // closes that left no instance before Close: the name was free for anyone (03 §13.3)
	twice    int             // closes of a closed instance
	accepted int             // accept calls
}

// fakeMessage is what a client sends: data, or, as the instance's accept returns it, an error (a client that sent
// nothing in time).
type fakeMessage struct {
	data []byte
	err  error
}

type fakeInstance struct {
	p      *fakePipe
	msg    chan fakeMessage // the client's message, once it closed its end
	closed bool             // under p.mu
}

func newFakePipe() *fakePipe { return &fakePipe{stop: make(chan struct{})} }

func (p *fakePipe) create(first bool) (pipeInstance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case first && p.open > 0:
		return nil, errors.New("fake: another process holds the name")
	case !first && p.nextErr != nil:
		return nil, p.nextErr
	}
	in := &fakeInstance{p: p, msg: make(chan fakeMessage, 1)}
	p.free = append(p.free, in)
	p.open++
	return in, nil
}

func (p *fakePipe) stopped() bool {
	select {
	case <-p.stop:
		return true
	default:
		return false
	}
}

// halt is Close.
func (p *fakePipe) halt() { p.stopOnce.Do(func() { close(p.stop) }) }

// send is a second launch: it connects to an instance that no client holds, writes and closes its end, at once. It
// reports false when no instance is free.
func (p *fakePipe) send(m fakeMessage) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.free) == 0 {
		return false
	}
	in := p.free[0]
	p.free = p.free[1:]
	in.msg <- m
	return true
}

func (in *fakeInstance) accept() ([]byte, error) {
	in.p.mu.Lock()
	in.p.accepted++
	in.p.mu.Unlock()
	// A message already waiting comes back first, as on Windows, where accept doesn't look at the stop event when a
	// client is already connected: serveLoop's own stopped check then decides.
	select {
	case m := <-in.msg:
		return m.data, m.err
	default:
	}
	select {
	case m := <-in.msg:
		return m.data, m.err
	case <-in.p.stop:
		return nil, errStopped
	}
}

func (in *fakeInstance) close() {
	p := in.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if in.closed {
		p.twice++
		return
	}
	in.closed = true
	p.open--
	p.free = slices.DeleteFunc(p.free, func(f *fakeInstance) bool { return f == in })
	if p.open == 0 && !p.stopped() {
		p.freed++
	}
}

// runLoop runs serveLoop on p until the test ends. done is closed when serveLoop returns; read log after that.
func runLoop(t *testing.T, p *fakePipe, onArgs func([]string)) (done chan struct{}, log *bytes.Buffer) {
	t.Helper()
	done, log = make(chan struct{}), &bytes.Buffer{}
	go func() {
		defer close(done)
		serveLoop(p, onArgs, slog.New(slog.NewTextHandler(log, nil)))
	}()
	t.Cleanup(func() {
		p.halt()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("serveLoop didn't return after Close")
		}
	})
	return done, log
}

func message(t *testing.T, args ...string) fakeMessage {
	t.Helper()
	data, err := encodeArgs(args)
	if err != nil {
		t.Fatal(err)
	}
	return fakeMessage{data: data}
}

// sendSoon sends m once an instance is free, as Forward retries a busy name, within 5 s.
func sendSoon(t *testing.T, p *fakePipe, m fakeMessage) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !p.send(m) {
		if time.Now().After(deadline) {
			t.Fatal("no instance of the pipe was free within 5 s")
		}
		time.Sleep(time.Millisecond)
	}
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal(what)
	}
}

func receive(t *testing.T, got <-chan []string, want ...string) {
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

// ciPipePattern is the PowerShell -like pattern by which the windows-desktop jobs' cleanup (ci.yml) and windows-full's
// (nightly.yml) find the single-instance pipes below \\.\pipe\; TestPipeNames reads it from both files.
const ciPipePattern = `*io.isshoni.desktop.*`

// ciPipeLike matches the cleanup's pipe filter: GetFiles('\\.\pipe\') piped into Where-Object { $_ -like '<pattern>' }.
var ciPipeLike = regexp.MustCompile(`GetFiles\('\\\\\.\\pipe\\'\) \| Where-Object \{ \$_ -like '([^']*)' \}`)

// ciPipesUse is every use of the cleanup's $pipes: only its assignment and its count may appear, so the pipes' names
// are never printed (03 §13.3: neither the pipe's name nor the lock file's content is logged).
var ciPipesUse = regexp.MustCompile(`\$pipes\b(\s*=\s*@\(|\.Count\b)?`)

// TestPipeNames: the pipe's name is \\.\pipe\<id>.v1.<32 hex digits>, new each run; instance.lock's content gives it
// back only when it is exactly such a name of the same id (03 §13.3; README §3 item 119). The workflows' cleanup
// pattern matches it, and the cleanup prints the leftover pipes' count, never their names.
func TestPipeNames(t *testing.T) {
	a, err := newPipeName(InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := newPipeName(InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Errorf("two runs share a pipe name: %q", a)
	}
	if !regexp.MustCompile(`^\\\\\.\\pipe\\io\.isshoni\.desktop\.v1\.[0-9a-f]{32}$`).MatchString(a) {
		t.Errorf("pipe name %q", a)
	}
	for _, data := range []string{a, a + "\r\n", " " + a + "\n"} {
		if got, ok := pipeFromLock(InstanceID, []byte(data)); !ok || got != a {
			t.Errorf("pipeFromLock(%q) = %q %v, want %q", data, got, ok, a)
		}
	}
	token := a[len(pipePrefix(InstanceID)):]
	for _, data := range []string{
		"",
		"   ",
		a[:len(a)-1],       // half written
		a + "0",            // too long
		strings.ToUpper(a), // not as written
		pipePrefix(InstanceID) + strings.Repeat("g", 32),
		pipePrefix("io.isshoni.test-1") + token, // another id's
		`\\.\pipe\..\` + token,
		`C:\Users\x\file.v1.` + token,
		a + "\n" + a,
	} {
		if got, ok := pipeFromLock(InstanceID, []byte(data)); ok {
			t.Errorf("pipeFromLock(%q) = %q, want none", data, got)
		}
	}

	// The cleanup's pattern, as PowerShell's -like applies it to GetFiles' full names (no '/' in them, so path.Match's
	// '*' is -like's): it must be ciPipePattern in both workflows and match the pipe.
	for _, wf := range []string{"../../../.github/workflows/ci.yml", "../../../.github/workflows/nightly.yml"} {
		data, err := os.ReadFile(wf)
		if err != nil {
			t.Fatal(err)
		}
		m := ciPipeLike.FindAllStringSubmatch(string(data), -1)
		if len(m) != 1 {
			t.Errorf("%s: %d single-instance pipe filters, want 1", wf, len(m))
			continue
		}
		if m[0][1] != ciPipePattern {
			t.Errorf("%s: the pipe filter is %q, want %q", wf, m[0][1], ciPipePattern)
		}
		uses := ciPipesUse.FindAllStringSubmatch(string(data), -1)
		if len(uses) < 2 {
			t.Errorf("%s: $pipes is used %d times, want its assignment and its count", wf, len(uses))
		}
		for _, u := range uses {
			if u[1] == "" {
				t.Errorf("%s: $pipes is used other than by its count (its names would be printed)", wf)
			}
		}
	}
	for _, p := range []string{a, b} {
		if ok, err := path.Match(ciPipePattern, p); !ok || err != nil {
			t.Errorf("%q doesn't match the cleanup's %q: %v", p, ciPipePattern, err)
		}
	}
}

// TestLockState: only a plain instance.lock this user owns is the lock; on it, a held range is a first instance and
// another handle's range a second one; a link, a file of another owner (or one whose owner can't be read) and any
// other lock error start the launch without the lock, never as a second instance (03 §13.3).
func TestLockState(t *testing.T) {
	errViolation := errors.New("lock violation")
	errOther := errors.New("something else")
	for _, c := range []struct {
		name  string
		f     lockFacts
		first bool
		err   bool
	}{
		{"held", lockFacts{ownerIsUser: true}, true, false},
		{"held elsewhere", lockFacts{ownerIsUser: true, lockErr: errViolation, violation: true}, false, false},
		{"another lock error", lockFacts{ownerIsUser: true, lockErr: errOther}, false, true},
		{"a link", lockFacts{reparse: true, ownerIsUser: true}, false, true},
		{"a link, held elsewhere", lockFacts{reparse: true, ownerIsUser: true, lockErr: errViolation, violation: true},
			false, true},
		{"another owner", lockFacts{}, false, true},
		{"another owner, held elsewhere", lockFacts{lockErr: errViolation, violation: true}, false, true},
	} {
		first, err := lockState(c.f)
		if first != c.first || (err != nil) != c.err {
			t.Errorf("%s: %v %v, want first %v, error %v", c.name, first, err, c.first, c.err)
		}
	}
	if _, err := lockState(lockFacts{reparse: true}); !errors.Is(err, errLockNotOurs) {
		t.Errorf("a link: %v isn't errLockNotOurs", err)
	}
}

// TestServerCheck: Forward trusts the pipe when its owner is the user's SID, whatever the server's token says (an
// elevated server's can't be opened from a non-elevated client); it compares the token only when the owner can't be
// read, and a token that can't be opened then leaves the server unproven (03 §13.3).
func TestServerCheck(t *testing.T) {
	const user, other = "S-1-5-21-1-2-3-1001", "S-1-5-21-1-2-3-1002"
	errDenied := errors.New("access denied")
	errUnread := errors.New("the owner can't be read")
	token := func(sid string, err error) func() (string, error) {
		return func() (string, error) { return sid, err }
	}
	for _, c := range []struct {
		name     string
		owner    string
		ownerErr error
		token    func() (string, error)
		ok       bool
	}{
		{"owner is the user, token denied (elevated server)", user, nil, token("", errDenied), true},
		{"owner is the user, token of the user", user, nil, token(user, nil), true},
		{"owner is another account", other, nil, token(user, nil), false},
		{"owner unread, token of the user", "", errUnread, token(user, nil), true},
		{"owner unread, token of another account", "", errUnread, token(other, nil), false},
		{"owner unread, token denied", "", errUnread, token("", errDenied), false},
	} {
		err := serverCheck(user, c.owner, c.ownerErr, c.token)
		if (err == nil) != c.ok {
			t.Errorf("%s: %v, want ok %v", c.name, err, c.ok)
		}
		if err != nil && !errors.Is(err, errServerNotUser) {
			t.Errorf("%s: %v isn't errServerNotUser", c.name, err)
		}
	}
}

// TestRetryOpen: Forward's loop returns at the first success; on the timeout a refused server is its error even when
// later attempts failed otherwise (a refused pipe that stays leaves its instance busy), and without a refusal the
// last attempt's error is.
func TestRetryOpen(t *testing.T) {
	errBusy, errGone := errors.New("busy"), errors.New("gone")
	const timeout, pause = 100 * time.Millisecond, time.Millisecond
	seq := func(errs ...error) func() error {
		n := 0
		return func() error {
			err := errs[min(n, len(errs)-1)]
			n++
			return err
		}
	}
	refused := fmt.Errorf("%w (its owner: x)", errServerNotUser)
	if err := retryOpen(timeout, pause, seq(errBusy, refused, nil)); err != nil {
		t.Errorf("a success after failures: %v", err)
	}
	start := time.Now()
	err := retryOpen(timeout, pause, seq(refused, errBusy))
	if !errors.Is(err, errServerNotUser) {
		t.Errorf("refused once, then busy: %v, want errServerNotUser", err)
	}
	if d := time.Since(start); d < timeout {
		t.Errorf("gave up after %v, before the timeout", d)
	}
	if err := retryOpen(timeout, pause, seq(errBusy, errGone)); !errors.Is(err, errGone) ||
		errors.Is(err, errServerNotUser) {
		t.Errorf("never refused: %v, want the last error", err)
	}
}

// heldFirst returns an onArgs that holds its first call until release is closed, and closes entered when that call
// starts; later calls aren't held. Each call's arguments go to got.
func heldFirst(calls *atomic.Int32, entered, release chan struct{}, got chan<- []string) func([]string) {
	return func(args []string) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		got <- args
	}
}

// TestServeLoop: Serve's loop apart from the pipe (03 §13.3). While onArgs handles one command line, the next
// instance already exists, so another launch connects at once, and it is served after onArgs returns, in order; a
// message that doesn't parse or doesn't come in time is dropped and serving goes on; the name is never free while it
// serves, and Close ends the loop with every instance closed.
func TestServeLoop(t *testing.T) {
	p := newFakePipe()
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	got := make(chan []string, 8)
	done, log := runLoop(t, p, heldFirst(&calls, entered, release, got))

	sendSoon(t, p, message(t, "one"))
	waitFor(t, entered, "onArgs wasn't called")
	if !p.send(message(t, "two")) {
		t.Fatal("no instance of the pipe is free while onArgs runs")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("onArgs was called %d times while the first call ran", n)
	}
	unblock()
	receive(t, got, "one")
	receive(t, got, "two")

	sendSoon(t, p, fakeMessage{data: []byte("not json")})
	sendSoon(t, p, fakeMessage{err: errors.New("winos: a second launch sent nothing in time")})
	sendSoon(t, p, message(t)) // no arguments at all: still a command line
	receive(t, got)
	sendSoon(t, p, message(t, "--open-url", "isshoni://watch.example.com/r/lounge"))
	receive(t, got, "--open-url", "isshoni://watch.example.com/r/lounge")

	// Close while the loop waits for the next launch.
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.mu.Lock()
		waiting := p.accepted == 7 && len(p.free) == 1
		p.mu.Unlock()
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("serveLoop doesn't wait for the next launch")
		}
		time.Sleep(time.Millisecond)
	}
	p.halt()
	waitFor(t, done, "serveLoop didn't return after Close")
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.open != 0 || p.freed != 0 || p.twice != 0 {
		t.Errorf("instances left open %d, closes that freed the name while serving %d, double closes %d",
			p.open, p.freed, p.twice)
	}
	if n := strings.Count(log.String(), "message was dropped"); n != 2 {
		t.Errorf("%d messages dropped, want 2; log:\n%s", n, log)
	}
	if n := calls.Load(); n != 4 {
		t.Errorf("onArgs was called %d times, want 4", n)
	}
	// Each delivered command line is logged by its count, never its arguments.
	if n := strings.Count(log.String(), forwardArrivedMsg); n != 4 {
		t.Errorf("%d command lines logged as arrived, want 4; log:\n%s", n, log)
	}
	if strings.Contains(log.String(), "lounge") {
		t.Errorf("a forwarded argument was logged; log:\n%s", log)
	}
}

// TestServeLoopCloseWhileHandling: Close while onArgs runs ends the loop once onArgs returns: a launch that connected
// meanwhile isn't handled any more, and every instance is closed.
func TestServeLoopCloseWhileHandling(t *testing.T) {
	p := newFakePipe()
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	got := make(chan []string, 2)
	done, _ := runLoop(t, p, heldFirst(&calls, entered, release, got))
	sendSoon(t, p, message(t, "one"))
	waitFor(t, entered, "onArgs wasn't called")
	if !p.send(message(t, "two")) {
		t.Fatal("no instance of the pipe is free while onArgs runs")
	}
	p.halt()
	unblock()
	waitFor(t, done, "serveLoop didn't return after onArgs, once Close ran")
	receive(t, got, "one")
	p.mu.Lock()
	defer p.mu.Unlock()
	if n := calls.Load(); n != 1 {
		t.Errorf("onArgs was called %d times after Close", n)
	}
	if p.open != 0 || p.twice != 0 {
		t.Errorf("instances left open %d, double closes %d", p.open, p.twice)
	}
}

// TestServeLoopCreateFails: with another holder of the name nothing is served (a warning); when the next instance
// can't be created, the command line already read is still handled, then the loop ends, with a warning unless Close
// ended the retries.
func TestServeLoopCreateFails(t *testing.T) {
	p := newFakePipe()
	if _, err := p.create(false); err != nil { // another process's instance
		t.Fatal(err)
	}
	done, log := runLoop(t, p, func([]string) { t.Error("onArgs was called") })
	waitFor(t, done, "serveLoop didn't return")
	if !strings.Contains(log.String(), "can't be created") {
		t.Errorf("no warning; log:\n%s", log)
	}

	for _, tc := range []struct {
		err  error
		warn bool
	}{
		{errors.New("fake: no more instances"), true},
		{errStopped, false},
	} {
		p := newFakePipe()
		p.nextErr = tc.err
		got := make(chan []string, 1)
		done, log := runLoop(t, p, func(args []string) { got <- args })
		sendSoon(t, p, message(t, "one"))
		receive(t, got, "one")
		waitFor(t, done, "serveLoop didn't return without a next instance")
		if warned := strings.Contains(log.String(), "can't be created"); warned != tc.warn {
			t.Errorf("%v: warned %v, want %v; log:\n%s", tc.err, warned, tc.warn, log)
		}
		p.mu.Lock()
		if p.open != 0 || p.twice != 0 {
			t.Errorf("%v: instances left open %d, double closes %d", tc.err, p.open, p.twice)
		}
		p.mu.Unlock()
	}
}
