package winos

import (
	"bytes"
	"errors"
	"log/slog"
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
