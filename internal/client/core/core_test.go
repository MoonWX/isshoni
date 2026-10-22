package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// The core's unit rows of docs/m2/01-client-core.md §14 against fakeServer: the stop table of §11.3, Run once,
// Close within 3 s, ErrNotReady, ctx cancel aborting a start, Status.ConnectionID and its EventStatus, EventShare's
// coalescing and the subscribers' overflow.

const waitTimeout = 20 * time.Second

// testEnv is one test's Client on a fake server and a fake engine.
type testEnv struct {
	t      *testing.T
	fs     *fakeServer
	auth   *fakeAuth
	engine *fake.Engine
	c      *Client
	logs   *logBuffer
}

// envOptions configure a testEnv.
type envOptions struct {
	auth  *fakeAuth // nil: cookie auth through Options.Header
	tweak func(*Options)
}

func newEnv(t *testing.T, fs *fakeServer, o envOptions) *testEnv {
	t.Helper()
	e := &testEnv{t: t, fs: fs, auth: o.auth, engine: fake.NewEngine(fake.EngineConfig{SourceSize: [2]int{640, 360}}),
		logs: &logBuffer{}}
	se := sfutest.LoopbackSettings()
	opts := Options{
		ServerURL: fs.url(),
		Engine:    e.engine,
		Settings:  &se,
		Logger:    slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	if o.auth != nil {
		opts.Auth = o.auth
	} else {
		opts.Header = http.Header{"Cookie": {"isshoni_session=test"}}
	}
	if o.tweak != nil {
		o.tweak(&opts)
	}
	c, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	e.c = c
	t.Cleanup(func() {
		_ = c.Close()
		_ = e.engine.Close()
		if t.Failed() {
			t.Logf("the core's log:\n%s", e.logs.String())
		}
	})
	return e
}

// run runs the Client until the test ends; the runner tells how Run returned.
func (e *testEnv) run() *runner {
	r := &runner{done: make(chan struct{})}
	go func() {
		r.err = e.c.Run(context.Background())
		r.at = time.Now()
		close(r.done)
	}()
	e.t.Cleanup(func() {
		_ = e.c.Close()
		<-r.done
	})
	return r
}

// runner is a running Run.
type runner struct {
	done chan struct{}
	err  error
	at   time.Time
}

// wait waits for Run's return.
func (r *runner) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-r.done:
		return r.err
	case <-time.After(waitTimeout):
		t.Fatal("Run did not return")
	}
	return nil
}

// waitOnline waits until the connection is online.
func (e *testEnv) waitOnline() {
	e.t.Helper()
	eventually(e.t, func() bool { return e.c.Status().Conn == ConnOnline },
		func() string { return fmt.Sprintf("status %+v, want online", e.c.Status()) })
}

// start picks a window on the fake engine and starts sharing it in the lounge.
func (e *testEnv) start(ctx context.Context) (*Share, error) {
	e.t.Helper()
	src, err := e.engine.Pick(ctx, media.PickRequest{})
	if err != nil {
		e.t.Fatal(err)
	}
	return e.c.StartShare(ctx, ShareRequest{RoomID: "lounge", Source: src, Audio: true})
}

func eventually(t *testing.T, cond func() bool, what func() string) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func ctxFor(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	t.Cleanup(cancel)
	return ctx
}

// nextEvent returns the next event that match accepts, failing the test when none comes.
func nextEvent(t *testing.T, events <-chan Event, match func(Event) bool) Event {
	t.Helper()
	timeout := time.After(waitTimeout)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("the event channel closed")
			}
			if match(ev) {
				return ev
			}
		case <-timeout:
			t.Fatal("no such event")
		}
	}
}

// logBuffer collects a Client's log for a failing test.
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestNewChecksOptions(t *testing.T) {
	eng := fake.NewEngine(fake.EngineConfig{})
	cookie := http.Header{"Cookie": {"isshoni_session=test"}}
	bad := map[string]Options{
		"no scheme":               {ServerURL: "watch.example.com", Header: cookie, Engine: eng},
		"http on a public host":   {ServerURL: "http://watch.example.com", Header: cookie, Engine: eng},
		"a path":                  {ServerURL: "https://watch.example.com/ws", Header: cookie, Engine: eng},
		"a websocket URL":         {ServerURL: "wss://watch.example.com", Header: cookie, Engine: eng},
		"neither Auth nor Header": {ServerURL: "https://watch.example.com", Engine: eng},
		"no engine":               {ServerURL: "https://watch.example.com", Header: cookie},
	}
	for name, o := range bad {
		if _, err := New(o); err == nil {
			t.Errorf("%s: New = nil error", name)
		}
	}
	for _, u := range []string{"https://watch.example.com", "https://watch.example.com/", "http://localhost:8080",
		"http://127.0.0.1:8080", "http://[::1]:8080"} {
		c, err := New(Options{ServerURL: u, Header: cookie, Engine: eng})
		if err != nil {
			t.Errorf("New(%s) = %v", u, err)
			continue
		}
		if c.wsURL != strings.TrimSuffix(u, "/")+"/ws" || c.o.Client.Kind != protocol.ClientKindDesktop ||
			c.o.Client.Version == "" || c.o.Client.OS == "" {
			t.Errorf("New(%s): ws URL %q, client %+v", u, c.wsURL, c.o.Client)
		}
		if st := c.Status(); st.Conn != ConnConnecting || st.ConnectionID != "" || st.Stop != nil {
			t.Errorf("New(%s): status %+v before Run", u, st)
		}
	}
}

// TestRunOnce: a Client runs once. Run after Run returned, or after Close, is ErrClosed; Close is idempotent.
func TestRunOnce(t *testing.T) {
	e := newEnv(t, newFakeServer(t), envOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.c.Run(ctx) }()
	e.waitOnline()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run after its ctx ended = %v, want nil", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Run did not return when its ctx ended")
	}
	if st := e.c.Status(); st.Conn != ConnStopped || st.Stop != nil {
		t.Errorf("status after Run = %+v, want stopped without a stop error", st)
	}
	if err := e.c.Run(context.Background()); !errors.Is(err, ErrClosed) {
		t.Errorf("Run again = %v, want ErrClosed", err)
	}
	for range 2 {
		if err := e.c.Close(); err != nil {
			t.Errorf("Close = %v", err)
		}
	}

	closed := newEnv(t, newFakeServer(t), envOptions{})
	if err := closed.c.Close(); err != nil {
		t.Errorf("Close before Run = %v", err)
	}
	if err := closed.c.Run(context.Background()); !errors.Is(err, ErrClosed) {
		t.Errorf("Run after Close = %v, want ErrClosed", err)
	}
	if st := closed.c.Status(); st.Conn != ConnStopped {
		t.Errorf("status after Close without Run = %+v, want stopped", st)
	}
	if closed.fs.helloCount() != 0 {
		t.Errorf("a closed Client sent %d hellos", closed.fs.helloCount())
	}
}

// TestHello: the Go connection's hello (§11.1): role publisher, kind desktop, the features agent.relay and
// share.pause, caps from the probe, and the bearer token with Auth.
func TestHello(t *testing.T) {
	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{auth: &fakeAuth{tokens: []tokenAnswer{{tok: "t1"}}}})
	e.run()
	e.waitOnline()
	h := fs.helloAt(0)
	wantEncode := []protocol.CodecKey{protocol.CodecH264High, protocol.CodecH264ConstrainedHigh,
		protocol.CodecH264ConstrainedBaseline, protocol.CodecOpus}
	if h.Role != protocol.RolePublisher || h.Client.Kind != protocol.ClientKindDesktop ||
		!slices.Equal(h.Features, []protocol.Feature{protocol.FeatureAgentRelay, protocol.FeatureSharePause}) ||
		h.Caps.Decode == nil || len(h.Caps.Decode) != 0 || !slices.Equal(h.Caps.Encode, wantEncode) ||
		!h.Caps.Simulcast || !h.Caps.DisplayCapture {
		t.Errorf("hello = %+v, want §11.1's", h)
	}
	if h.Auth == nil || h.Auth.Scheme != protocol.AuthSchemeBearer || h.Auth.Token.Reveal() != "t1" {
		t.Errorf("hello.auth = %+v, want the bearer token", h.Auth)
	}

	// A CB-only engine announces no High.
	fs2 := newFakeServer(t)
	cb := newEnv(t, fs2, envOptions{tweak: func(o *Options) {
		o.Engine = fake.NewEngine(fake.EngineConfig{Probe: media.Probe{Encoders: []media.EncoderInfo{
			{Name: "nvenc", Tested: false, Profiles: []string{"6400"}},
			{Name: "openh264", Tested: true, Profiles: []string{"42e0"}},
		}}})
	}})
	cb.run()
	cb.waitOnline()
	if got := fs2.helloAt(0); got.Auth != nil ||
		!slices.Equal(got.Caps.Encode, []protocol.CodecKey{protocol.CodecH264ConstrainedBaseline, protocol.CodecOpus}) {
		t.Errorf("a cookie client with a CB-only encoder sent hello %+v", got)
	}
}

// TestConnectionIDAndEventStatus: Status.ConnectionID is the welcome's, EventStatus fires when it changes, and a
// reconnect that isn't resumed gives the connection a new id (§11.10).
func TestConnectionIDAndEventStatus(t *testing.T) {
	fs := newFakeServer(t)
	fs.hello = func(int, protocol.Hello) fakeAnswer { return fakeAnswer{resumed: false} } // every welcome is new
	e := newEnv(t, fs, envOptions{})
	events, unsub := e.c.Subscribe()
	defer unsub()
	e.run()
	first := nextEvent(t, events, func(ev Event) bool { return ev.Kind == EventStatus && ev.Status.Conn == ConnOnline })
	if first.Status.ConnectionID != "c_1" || first.Status.ServerVersion != "0.2.0-test" ||
		!slices.Equal(first.Status.Features, []protocol.Feature{protocol.FeatureAgentRelay}) {
		t.Errorf("EventStatus = %+v, want online as c_1 with the welcome's version and features", first.Status)
	}
	if st := e.c.Status(); st.ConnectionID != "c_1" {
		t.Errorf("Status().ConnectionID = %q, want c_1", st.ConnectionID)
	}

	fs.kill()
	away := nextEvent(t, events, func(ev Event) bool { return ev.Kind == EventStatus && ev.Status.Conn != ConnOnline })
	if away.Status.Conn != ConnReconnecting && away.Status.Conn != ConnConnecting {
		t.Errorf("EventStatus after the drop = %+v, want reconnecting or connecting", away.Status)
	}
	back := nextEvent(t, events, func(ev Event) bool { return ev.Kind == EventStatus && ev.Status.Conn == ConnOnline })
	if back.Status.ConnectionID != "c_2" {
		t.Errorf("EventStatus after the reconnect = %+v, want online as c_2", back.Status)
	}
	if h := fs.helloAt(1); h.ResumeToken.Reveal() != "rt-c_1" {
		t.Errorf("the second hello's resume token is %q, want the first welcome's", h.ResumeToken.Reveal())
	}
	if st := e.c.Status(); st.ConnectionID != "c_2" || st.Conn != ConnOnline {
		t.Errorf("Status() = %+v, want online as c_2", st)
	}
}

// stopCase is one row of the stop table (§11.3).
type stopCase struct {
	name string
	auth *fakeAuth // nil: cookie auth
	// hello answers the n-th hello; nil welcomes. mid, when set, ends the first connection after its welcome.
	hello func(n int, h protocol.Hello) fakeAnswer
	mid   *protocol.Error
	want  StopError // Err is compared by code only, nil meaning none
	// ended are the SessionEnded calls, forced the forced refreshes, hellos the hellos the server saw.
	ended  []protocol.ErrorCode
	forced int
	hellos int
}

func always(a fakeAnswer) func(int, protocol.Hello) fakeAnswer {
	return func(int, protocol.Hello) fakeAnswer { return a }
}

func TestStopTable(t *testing.T) {
	unauth := welcomeErr(protocol.ErrorCodeUnauthenticated, protocol.ErrorScopeSession, nil)
	versions := map[string]any{"serverMin": float64(1), "serverMax": float64(1), "serverVersion": "0.9.0"}
	serverErr := func(code protocol.ErrorCode) *protocol.Error { return &protocol.Error{Code: code} }
	cases := []stopCase{
		{name: "session_revoked", auth: &fakeAuth{}, mid: serverErr(protocol.ErrorCodeSessionRevoked),
			want:  StopError{Reason: StopUnlinked, Code: protocol.ErrorCodeSessionRevoked, Err: serverErr(protocol.ErrorCodeSessionRevoked)},
			ended: []protocol.ErrorCode{protocol.ErrorCodeSessionRevoked}, hellos: 1},
		{name: "account_disabled", auth: &fakeAuth{},
			hello: always(welcomeErr(protocol.ErrorCodeAccountDisabled, protocol.ErrorScopeSession, nil)),
			want:  StopError{Reason: StopDisabled, Code: protocol.ErrorCodeAccountDisabled, Err: serverErr(protocol.ErrorCodeAccountDisabled)},
			ended: []protocol.ErrorCode{protocol.ErrorCodeAccountDisabled}, hellos: 1},
		{name: "a second unauthenticated after the forced refresh",
			auth:  &fakeAuth{tokens: []tokenAnswer{{tok: "t1"}}, forced: []tokenAnswer{{tok: "t2"}}},
			hello: always(unauth),
			want:  StopError{Reason: StopUnlinked, Code: protocol.ErrorCodeUnauthenticated, Err: serverErr(protocol.ErrorCodeUnauthenticated)},
			ended: []protocol.ErrorCode{protocol.ErrorCodeUnauthenticated}, forced: 1, hellos: 2},
		{name: "RelinkRequired at the dial", auth: &fakeAuth{tokens: []tokenAnswer{{err: relinkError{}}}},
			want: StopError{Reason: StopUnlinked}},
		{name: "RelinkRequired in a joined error", auth: &fakeAuth{tokens: []tokenAnswer{{err: errors.Join(errTransient, relinkError{})}}},
			want: StopError{Reason: StopUnlinked}},
		{name: "UpdateRequired at the dial", auth: &fakeAuth{tokens: []tokenAnswer{{err: fmt.Errorf("refresh: %w", updateError{})}}},
			want: StopError{Reason: StopUpdateRequired}},
		{name: "RelinkRequired at the forced refresh",
			auth: &fakeAuth{forced: []tokenAnswer{{err: relinkError{}}}}, hello: always(unauth),
			want: StopError{Reason: StopUnlinked}, forced: 1, hellos: 1},
		{name: "UpdateRequired at the forced refresh",
			auth: &fakeAuth{forced: []tokenAnswer{{err: updateError{}}}}, hello: always(unauth),
			want: StopError{Reason: StopUpdateRequired}, forced: 1, hellos: 1},
		{name: "protocol_unsupported", auth: &fakeAuth{},
			hello: always(welcomeErr(protocol.ErrorCodeProtocolUnsupported, protocol.ErrorScopeConnection, versions)),
			want: StopError{Reason: StopUpdateRequired, Code: protocol.ErrorCodeProtocolUnsupported,
				Err: &protocol.Error{Code: protocol.ErrorCodeProtocolUnsupported, Params: versions}}, hellos: 1},
		{name: "client_outdated", auth: &fakeAuth{},
			hello:  always(welcomeErr(protocol.ErrorCodeClientOutdated, protocol.ErrorScopeConnection, nil)),
			want:   StopError{Reason: StopUpdateRequired, Code: protocol.ErrorCodeClientOutdated, Err: serverErr(protocol.ErrorCodeClientOutdated)},
			hellos: 1},
		{name: "too_many_connections", auth: &fakeAuth{},
			hello:  always(welcomeErr(protocol.ErrorCodeTooManyConnections, protocol.ErrorScopeConnection, nil)),
			want:   StopError{Reason: StopTooMany, Code: protocol.ErrorCodeTooManyConnections, Err: serverErr(protocol.ErrorCodeTooManyConnections)},
			hellos: 1},
		{name: "replaced", auth: &fakeAuth{}, mid: &protocol.Error{Code: protocol.ErrorCodeReplaced, Scope: protocol.ErrorScopeConnection},
			want:   StopError{Reason: StopReplaced, Code: protocol.ErrorCodeReplaced, Err: serverErr(protocol.ErrorCodeReplaced)},
			hellos: 1},
		{name: "bad_message", auth: &fakeAuth{},
			hello:  always(welcomeErr(protocol.ErrorCodeBadMessage, protocol.ErrorScopeConnection, nil)),
			want:   StopError{Reason: StopProtocol, Code: protocol.ErrorCodeBadMessage, Err: serverErr(protocol.ErrorCodeBadMessage)},
			hellos: 1},
		{name: "cookie auth: unauthenticated", hello: always(unauth),
			want:   StopError{Reason: StopUnlinked, Code: protocol.ErrorCodeUnauthenticated, Err: serverErr(protocol.ErrorCodeUnauthenticated)},
			hellos: 1},
		{name: "cookie auth: session_revoked", mid: serverErr(protocol.ErrorCodeSessionRevoked),
			want:   StopError{Reason: StopUnlinked, Code: protocol.ErrorCodeSessionRevoked, Err: serverErr(protocol.ErrorCodeSessionRevoked)},
			hellos: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { testStop(t, tc) })
	}
}

func testStop(t *testing.T, tc stopCase) {
	fs := newFakeServer(t)
	fs.hello = tc.hello
	if tc.auth != nil {
		tc.auth.endedDelay = 100 * time.Millisecond // SessionEnded must finish before Run returns
	}
	e := newEnv(t, fs, envOptions{auth: tc.auth})
	events, unsub := e.c.Subscribe()
	defer unsub()
	r := e.run()
	if tc.mid != nil {
		e.waitOnline()
		scope := tc.mid.Scope
		if scope == "" {
			scope = protocol.ErrorScopeSession
		}
		fs.endSession(tc.mid.Code, scope)
	}
	err := r.wait(t)
	var se *StopError
	if !errors.As(err, &se) {
		t.Fatalf("Run = %v, want a *StopError", err)
	}
	if se.Reason != tc.want.Reason || se.Code != tc.want.Code {
		t.Errorf("StopError = %+v, want reason %s, code %q", se, tc.want.Reason, tc.want.Code)
	}
	switch {
	case tc.want.Err == nil && se.Err != nil:
		t.Errorf("StopError.Err = %+v, want nil (a local stop)", se.Err)
	case tc.want.Err != nil && (se.Err == nil || se.Err.Code != tc.want.Err.Code):
		t.Errorf("StopError.Err = %+v, want the server's %s", se.Err, tc.want.Err.Code)
	case tc.want.Err != nil && tc.want.Err.Params != nil && fmt.Sprint(se.Err.Params) != fmt.Sprint(tc.want.Err.Params):
		t.Errorf("StopError.Err.Params = %v, want %v", se.Err.Params, tc.want.Err.Params)
	}
	if st := e.c.Status(); st.Conn != ConnStopped || st.Stop == nil || st.Stop.Reason != tc.want.Reason {
		t.Errorf("status after the stop = %+v, want stopped with %s", st, tc.want.Reason)
	}
	ev := nextEvent(t, events, func(ev Event) bool { return ev.Kind == EventStatus && ev.Status.Stop != nil })
	if ev.Status.Conn != ConnStopped || ev.Status.Stop.Reason != tc.want.Reason {
		t.Errorf("EventStatus of the stop = %+v", ev.Status)
	}
	if got := fs.helloCount(); got != tc.hellos {
		t.Errorf("the server saw %d hellos, want %d", got, tc.hellos)
	}
	if tc.auth == nil {
		return
	}
	_, forced, ended, left, at := tc.auth.snapshot()
	if forced != tc.forced || !slices.Equal(ended, tc.ended) {
		t.Errorf("Auth: %d forced refreshes, SessionEnded %v; want %d and %v", forced, ended, tc.forced, tc.ended)
	}
	if len(ended) > 0 {
		if left <= 14*time.Second || left > sessionEndedTimeout {
			t.Errorf("SessionEnded's ctx had %v left, want it bounded at 15 s", left)
		}
		if r.at.Before(at) {
			t.Errorf("Run returned %v before SessionEnded did", at.Sub(r.at))
		}
	}
}

// TestForcedRefresh: an unauthenticated hello makes the core refresh the token once (forced) and dial anew with
// it; a transient error of that refresh is retried with the backoff.
func TestForcedRefresh(t *testing.T) {
	fs := newFakeServer(t)
	fs.hello = func(_ int, h protocol.Hello) fakeAnswer {
		if h.Auth == nil || h.Auth.Token.Reveal() != "t2" {
			return welcomeErr(protocol.ErrorCodeUnauthenticated, protocol.ErrorScopeSession, nil)
		}
		return fakeAnswer{}
	}
	auth := &fakeAuth{tokens: []tokenAnswer{{tok: "t1"}}, forced: []tokenAnswer{{err: errTransient}, {tok: "t2"}}}
	e := newEnv(t, fs, envOptions{auth: auth})
	e.run()
	e.waitOnline()
	calls, forced, ended, _, _ := auth.snapshot()
	if forced != 2 || len(ended) != 0 || calls != 1 {
		t.Errorf("Auth: %d calls, %d forced, SessionEnded %v; want 1 call, 2 forced (one retried), no SessionEnded",
			calls, forced, ended)
	}
	if n := fs.helloCount(); n != 2 || fs.helloAt(1).ResumeToken != "" {
		t.Errorf("hellos %d, the second's resume token %q; want 2, the second a new connection", n,
			fs.helloAt(1).ResumeToken.Reveal())
	}
}

// TestTransientTokenError: a transient AccessToken error fails one attempt; the signaling client tries again.
func TestTransientTokenError(t *testing.T) {
	fs := newFakeServer(t)
	auth := &fakeAuth{tokens: []tokenAnswer{{err: errTransient}, {tok: "t1"}}}
	e := newEnv(t, fs, envOptions{auth: auth})
	e.run()
	e.waitOnline()
	if calls, forced, _, _, _ := auth.snapshot(); calls != 2 || forced != 0 {
		t.Errorf("Auth: %d calls, %d forced; want 2 and 0", calls, forced)
	}
}

// TestStopEndsShare: a terminal stop ends the share locally (unlinked, the stop's code) and stops the engine
// before Auth.SessionEnded runs (the manager's refresh may take up to 15 s; capture must not wait for it), and Run
// returns after SessionEnded.
func TestStopEndsShare(t *testing.T) {
	fs := newFakeServer(t)
	auth := &fakeAuth{endedDelay: 500 * time.Millisecond}
	e := newEnv(t, fs, envOptions{auth: auth})
	r := e.run()
	e.waitOnline()
	sh, err := e.start(ctxFor(t))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var atEnded []string // what was still running when SessionEnded was called
	auth.setEndedHook(func() {
		mu.Lock()
		defer mu.Unlock()
		select {
		case <-sh.Done():
		default:
			atEnded = append(atEnded, "the share")
		}
		if e.engine.Session() != nil {
			atEnded = append(atEnded, "the engine session")
		}
		if e.c.ActiveShare() != nil {
			atEnded = append(atEnded, "ActiveShare")
		}
	})
	fs.endSession(protocol.ErrorCodeSessionRevoked, protocol.ErrorScopeSession)
	var se *StopError
	if err := r.wait(t); !errors.As(err, &se) || se.Reason != StopUnlinked {
		t.Fatalf("Run = %v, want unlinked", err)
	}
	_, _, ended, _, endedAt := auth.snapshot()
	mu.Lock()
	if len(ended) != 1 || len(atEnded) > 0 {
		t.Errorf("SessionEnded calls %v; still running when it was called: %v (want none)", ended, atEnded)
	}
	mu.Unlock()
	if r.at.Before(endedAt) {
		t.Errorf("Run returned %v before SessionEnded did", endedAt.Sub(r.at))
	}
	select {
	case <-sh.Done():
	default:
		t.Fatal("the share runs on after Run returned")
	}
	if end := sh.Snapshot().End; end == nil ||
		*end != (EndInfo{Reason: EndUnlinked, Protocol: protocol.ErrorCodeSessionRevoked}) {
		t.Errorf("the share ended with %+v, want unlinked, session_revoked", end)
	}
	if e.engine.Session() != nil || e.c.ActiveShare() != nil {
		t.Error("the engine session or the share outlived the stop")
	}
}

// TestStartShareNotReady: StartShare is ErrNotReady at once when the connection isn't online: before Run, while
// reconnecting, after Close.
func TestStartShareNotReady(t *testing.T) {
	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{})
	notReady := func(when string) {
		t.Helper()
		began := time.Now()
		if _, err := e.start(ctxFor(t)); !errors.Is(err, ErrNotReady) {
			t.Errorf("StartShare %s = %v, want ErrNotReady", when, err)
		}
		if took := time.Since(began); took > 100*time.Millisecond {
			t.Errorf("StartShare %s took %v, want an answer at once", when, took)
		}
		if err := e.c.StopOther(ctxFor(t), "lounge", "s_1"); !errors.Is(err, ErrNotReady) {
			t.Errorf("StopOther %s = %v, want ErrNotReady", when, err)
		}
	}
	notReady("before Run")
	e.run()
	e.waitOnline()
	// The server goes away: every hello is refused with a retryable error, so the client backs off.
	fs.setHello(always(welcomeErr(protocol.ErrorCodeServerShutdown, protocol.ErrorScopeConnection, nil)))
	fs.kill()
	eventually(t, func() bool { return e.c.Status().Conn != ConnOnline }, func() string { return "still online" })
	notReady("while reconnecting")
	_ = e.c.Close()
	notReady("after Close")
	if got := fs.types(); slices.Contains(got, protocol.MessageTypeRoomJoin) {
		t.Errorf("the server got %v: a refused start sent something", got)
	}
}

// TestStartShareBusy: one share at a time.
func TestStartShareBusy(t *testing.T) {
	e := newEnv(t, newFakeServer(t), envOptions{})
	e.run()
	e.waitOnline()
	sh, err := e.start(ctxFor(t))
	if err != nil {
		t.Fatal(err)
	}
	if e.c.ActiveShare() != sh || e.c.Status().Share == nil || e.c.Status().Share.ID != "s_1" {
		t.Errorf("ActiveShare %v, Status().Share %+v; want the share", e.c.ActiveShare(), e.c.Status().Share)
	}
	if _, err := e.start(ctxFor(t)); !errors.Is(err, ErrBusy) {
		t.Errorf("a second StartShare = %v, want ErrBusy", err)
	}
	// The share's actor offers the pub PC once the engine's first EventLayer is in.
	eventually(t, func() bool { return slices.Contains(e.fs.types(), protocol.MessageTypePCOffer) },
		func() string { return fmt.Sprintf("the server got %v, want a pub offer", e.fs.types()) })
	if err := sh.Stop(ctxFor(t)); err != nil {
		t.Fatal(err)
	}
	if end := sh.Snapshot().End; end == nil || *end != (EndInfo{Reason: EndUser, ByThisApp: true}) {
		t.Errorf("the share ended with %+v, want user, by this app", end)
	}
	want := []protocol.MessageType{protocol.MessageTypeRoomJoin, protocol.MessageTypeShareStart,
		protocol.MessageTypePCOffer, protocol.MessageTypeShareStop, protocol.MessageTypePCClose, protocol.MessageTypeRoomLeave}
	if got := e.fs.types(); !slices.Equal(got, want) {
		t.Errorf("the server got %v, want %v", got, want)
	}
}

// TestStartShareCancel: cancelling StartShare's ctx aborts the start cleanly (§11.2): share.stop once share.start
// succeeded, the engine stopped, room.leave, and no share remains. Also when the room.join's reply never came.
func TestStartShareCancel(t *testing.T) {
	t.Run("at Engine.Start", func(t *testing.T) {
		fs := newFakeServer(t)
		var cancel context.CancelFunc
		e := newEnv(t, fs, envOptions{tweak: func(o *Options) {
			// AudioConfig is read right before Engine.Start (§11.4 step 3): the user gives up just then.
			o.AudioConfig = func() media.AudioConfig { cancel(); return media.AudioConfig{} }
		}})
		events, unsub := e.c.Subscribe()
		defer unsub()
		e.run()
		e.waitOnline()
		var ctx context.Context
		ctx, cancel = context.WithCancel(context.Background())
		defer cancel()
		if _, err := e.start(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("StartShare = %v, want context.Canceled", err)
		}
		want := []protocol.MessageType{protocol.MessageTypeRoomJoin, protocol.MessageTypeShareStart,
			protocol.MessageTypeShareStop, protocol.MessageTypeRoomLeave}
		if got := fs.types(); !slices.Equal(got, want) {
			t.Errorf("the server got %v, want %v", got, want)
		}
		if e.c.ActiveShare() != nil || e.engine.Session() != nil {
			t.Error("a share or an engine session remains")
		}
		ev := nextEvent(t, events, func(ev Event) bool {
			return ev.Kind == EventShare && ev.Share.State == StateEnded
		})
		if ev.Share.End == nil || ev.Share.End.Reason != EndUser {
			t.Errorf("the aborted share's last EventShare = %+v", ev.Share)
		}
		// Nothing is left behind: a new share starts.
		sh, err := e.start(ctxFor(t))
		if err != nil {
			t.Fatalf("StartShare after the abort = %v", err)
		}
		_ = sh.Stop(ctxFor(t))
	})
	t.Run("at room.join", func(t *testing.T) {
		fs := newFakeServer(t)
		fs.silent = []protocol.MessageType{protocol.MessageTypeRoomJoin}
		e := newEnv(t, fs, envOptions{})
		e.run()
		e.waitOnline()
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		if _, err := e.start(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("StartShare = %v, want context.DeadlineExceeded", err)
		}
		// The join may have reached the server: the abort leaves.
		if got := fs.types(); !slices.Equal(got, []protocol.MessageType{protocol.MessageTypeRoomJoin, protocol.MessageTypeRoomLeave}) {
			t.Errorf("the server got %v, want room.join, room.leave", got)
		}
		if e.c.ActiveShare() != nil {
			t.Error("a share remains")
		}
	})
}

// TestStartShareErrors: a server refusal is the *protocol.Error, an engine refusal the *media.Error, and both undo
// the steps done.
func TestStartShareErrors(t *testing.T) {
	t.Run("share_limit", func(t *testing.T) {
		fs := newFakeServer(t)
		fs.reject = map[protocol.MessageType]protocol.Error{
			protocol.MessageTypeShareStart: protocol.NewError(protocol.ErrorCodeShareLimit, protocol.ErrorScopeRequest),
		}
		e := newEnv(t, fs, envOptions{})
		e.run()
		e.waitOnline()
		_, err := e.start(ctxFor(t))
		var perr *protocol.Error
		if !errors.As(err, &perr) || perr.Code != protocol.ErrorCodeShareLimit {
			t.Fatalf("StartShare = %v, want share_limit", err)
		}
		if got := fs.types(); !slices.Equal(got, []protocol.MessageType{protocol.MessageTypeRoomJoin,
			protocol.MessageTypeShareStart, protocol.MessageTypeRoomLeave}) {
			t.Errorf("the server got %v", got)
		}
	})
	t.Run("pick gone", func(t *testing.T) {
		fs := newFakeServer(t)
		e := newEnv(t, fs, envOptions{})
		e.run()
		e.waitOnline()
		src, err := e.engine.Pick(ctxFor(t), media.PickRequest{})
		if err != nil {
			t.Fatal(err)
		}
		e.engine.Release(src.ID) // the user backed out of the sheet, and something started it anyway
		_, err = e.c.StartShare(ctxFor(t), ShareRequest{RoomID: "lounge", Source: src})
		if !errors.Is(err, media.ErrSourceGone) {
			t.Fatalf("StartShare = %v, want source_gone", err)
		}
		if got := fs.types(); !slices.Equal(got, []protocol.MessageType{protocol.MessageTypeRoomJoin,
			protocol.MessageTypeShareStart, protocol.MessageTypeShareStop, protocol.MessageTypeRoomLeave}) {
			t.Errorf("the server got %v", got)
		}
		if e.c.ActiveShare() != nil {
			t.Error("a share remains")
		}
	})
}

// TestCloseBound: Close sends share.stop and stops the engine session at once, waits at most 3 s for the reply (here
// it never comes), then closes the socket (§11.2).
func TestCloseBound(t *testing.T) {
	fs := newFakeServer(t)
	fs.silent = []protocol.MessageType{protocol.MessageTypeShareStop}
	e := newEnv(t, fs, envOptions{})
	r := e.run()
	e.waitOnline()
	sh, err := e.start(ctxFor(t))
	if err != nil {
		t.Fatal(err)
	}
	if e.engine.Session() == nil {
		t.Fatal("no engine session after StartShare")
	}
	began := time.Now()
	closed := make(chan error, 1)
	go func() { closed <- e.c.Close() }()
	// Capture ends at once (within one frame interval, §11.2), not after the wait for the reply.
	if took := engineStopped(t, e.engine); took > engineStopBound {
		t.Errorf("the engine session stopped %v after Close was called, want within %v", took, engineStopBound)
	}
	if err := <-closed; err != nil {
		t.Errorf("Close = %v", err)
	}
	took := time.Since(began)
	t.Logf("Close took %v", took.Round(time.Millisecond))
	if took < closeTimeout-100*time.Millisecond || took > closeTimeout+time.Second {
		t.Errorf("Close took %v, want the 3 s wait for the share.stop reply and the close handshake", took)
	}
	if err := r.wait(t); err != nil {
		t.Errorf("Run after Close = %v, want nil", err)
	}
	if !slices.Contains(fs.types(), protocol.MessageTypeShareStop) {
		t.Errorf("the server got %v, want a share.stop", fs.types())
	}
	if end := sh.Snapshot().End; end == nil || *end != (EndInfo{Reason: EndUser, ByThisApp: true}) {
		t.Errorf("the share ended with %+v, want user, by this app", end)
	}
	if e.engine.Session() != nil {
		t.Error("the engine session outlived Close")
	}
}

// engineStopBound is how soon a share's end must stop the engine session: far less than the waits for the replies
// (3 s in Close, the caller's ctx in Stop), with the race detector's slack.
const engineStopBound = 300 * time.Millisecond

// engineStopped polls the fake engine until its session has stopped and returns how long that took (at most 5 s).
func engineStopped(t *testing.T, eng *fake.Engine) time.Duration {
	t.Helper()
	began := time.Now()
	for eng.Session() != nil {
		if time.Since(began) > 5*time.Second {
			t.Fatal("the engine session did not stop")
		}
		time.Sleep(time.Millisecond)
	}
	return time.Since(began)
}

// TestStopWithoutReply: Share.Stop stops the engine at once while it waits for the share.stop reply (here it never
// comes), keeps share.stop → pc.close on the wire, and returns ctx's error.
func TestStopWithoutReply(t *testing.T) {
	fs := newFakeServer(t)
	fs.silent = []protocol.MessageType{protocol.MessageTypeShareStop}
	e := newEnv(t, fs, envOptions{})
	e.run()
	e.waitOnline()
	sh, err := e.start(ctxFor(t))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return slices.Contains(fs.types(), protocol.MessageTypePCOffer) },
		func() string { return fmt.Sprintf("the server got %v, want a pub offer", fs.types()) })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- sh.Stop(ctx) }()
	if took := engineStopped(t, e.engine); took > engineStopBound {
		t.Errorf("the engine session stopped %v after Stop was called, want within %v", took, engineStopBound)
	}
	select {
	case <-sh.Done():
		t.Error("the share ended before the wait for the share.stop reply did")
	default:
	}
	if err := <-stopped; !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Stop = %v, want context.DeadlineExceeded", err)
	}
	got := fs.types()
	stop, pcClose := slices.Index(got, protocol.MessageTypeShareStop), slices.Index(got, protocol.MessageTypePCClose)
	if stop < 0 || pcClose < stop {
		t.Errorf("the server got %v, want share.stop, then pc.close", got)
	}
	if end := sh.Snapshot().End; end == nil || *end != (EndInfo{Reason: EndUser, ByThisApp: true}) {
		t.Errorf("the share ended with %+v, want user, by this app", end)
	}
}

// TestEndCutsOfferShort: Close ends a share whose actor waits for an offer's ICE gathering (here a STUN server that
// never answers holds it for Pion's 5 s): the gathering is cut short, Close keeps its bound, and the share ends as
// Close asked (user, by this app).
func TestEndCutsOfferShort(t *testing.T) {
	stun, err := (&net.ListenConfig{}).ListenPacket(ctxFor(t), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stun.Close() })
	go func() { // reads the binding requests and never answers
		b := make([]byte, 1500)
		for {
			if _, _, err := stun.ReadFrom(b); err != nil {
				return
			}
		}
	}()
	fs := newFakeServer(t)
	fs.iceServers = []protocol.ICEServer{{URLs: []string{"stun:" + stun.LocalAddr().String()}}}
	e := newEnv(t, fs, envOptions{})
	r := e.run()
	e.waitOnline()
	sh, err := e.start(ctxFor(t))
	if err != nil {
		t.Fatal(err)
	}
	// The first EventLayer fills in the layer's encoder right before the actor makes the Publisher and its offer.
	eventually(t, func() bool { return layerEncoder(sh.Snapshot()) != "" },
		func() string { return "no EventLayer reached the share" })
	time.Sleep(200 * time.Millisecond)
	began := time.Now()
	if err := e.c.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
	took := time.Since(began)
	if took > time.Second {
		t.Errorf("Close took %v while an offer gathered, want well within its 3 s bound", took)
	}
	if err := r.wait(t); err != nil {
		t.Errorf("Run after Close = %v, want nil", err)
	}
	got := fs.types()
	if slices.Contains(got, protocol.MessageTypePCOffer) || !slices.Contains(got, protocol.MessageTypeShareStop) {
		t.Errorf("the server got %v, want a share.stop and no pub offer (the gathering was cut short)", got)
	}
	if end := sh.Snapshot().End; end == nil || *end != (EndInfo{Reason: EndUser, ByThisApp: true}) {
		t.Errorf("the share ended with %+v, want user, by this app", end)
	}
}

// layerEncoder returns the encoder of the snapshot's first layer that has one.
func layerEncoder(snap ShareSnapshot) string {
	for _, l := range snap.Layers {
		if l.Encoder != "" {
			return l.Encoder
		}
	}
	return ""
}

// TestStopOtherJoinLost: StopOther from no room leaves again when its room.join's reply never came (the join may
// have put the connection in the room), and doesn't when the server refused the join.
func TestStopOtherJoinLost(t *testing.T) {
	t.Run("no reply", func(t *testing.T) {
		fs := newFakeServer(t)
		fs.silent = []protocol.MessageType{protocol.MessageTypeRoomJoin}
		e := newEnv(t, fs, envOptions{})
		e.run()
		e.waitOnline()
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		if err := e.c.StopOther(ctx, "lounge", "s_9"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("StopOther = %v, want context.DeadlineExceeded", err)
		}
		if got := fs.types(); !slices.Equal(got, []protocol.MessageType{protocol.MessageTypeRoomJoin, protocol.MessageTypeRoomLeave}) {
			t.Errorf("the server got %v, want room.join, room.leave", got)
		}
	})
	t.Run("refused", func(t *testing.T) {
		fs := newFakeServer(t)
		fs.reject = map[protocol.MessageType]protocol.Error{
			protocol.MessageTypeRoomJoin: protocol.NewError(protocol.ErrorCodeRoomNotFound, protocol.ErrorScopeRequest),
		}
		e := newEnv(t, fs, envOptions{})
		e.run()
		e.waitOnline()
		err := e.c.StopOther(ctxFor(t), "lounge", "s_9")
		var perr *protocol.Error
		if !errors.As(err, &perr) || perr.Code != protocol.ErrorCodeRoomNotFound {
			t.Fatalf("StopOther = %v, want room_not_found", err)
		}
		if got := fs.types(); !slices.Equal(got, []protocol.MessageType{protocol.MessageTypeRoomJoin}) {
			t.Errorf("the server got %v, want room.join only", got)
		}
	})
}

// TestSubscriberOverflow: a subscriber that falls behind loses its oldest events and gets an EventResync; the
// newest event is kept, and ending the subscription closes the channel.
func TestSubscriberOverflow(t *testing.T) {
	var subs subscribers
	ch, unsub := subs.add()
	for i := range 100 {
		id := fmt.Sprint(i)
		subs.publish(func() Event { return Event{Kind: EventStatus, Status: &Status{ConnectionID: id}} })
	}
	unsub()
	var got []Event
	for ev := range ch {
		got = append(got, ev)
	}
	if len(got) > subscriberDepth {
		t.Errorf("%d events queued, want at most %d", len(got), subscriberDepth)
	}
	if !slices.ContainsFunc(got, func(ev Event) bool { return ev.Kind == EventResync }) {
		t.Error("no EventResync after the overflow")
	}
	if last := got[len(got)-1]; last.Kind != EventStatus || last.Status.ConnectionID != "99" {
		t.Errorf("the last event is %+v, want the newest", last)
	}
	unsub() // idempotent
}

// TestShareEventsCoalesced: EventShare fires at most 4 times per second (§11.10): a burst of changes is one event
// now and one at the end of the interval; flush emits at once and stops.
func TestShareEventsCoalesced(t *testing.T) {
	var mu sync.Mutex
	var emits []time.Time
	k := &coalescer{emit: func() {
		mu.Lock()
		defer mu.Unlock()
		emits = append(emits, time.Now())
	}}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(emits)
	}
	for range 50 {
		k.changed()
	}
	if n := count(); n != 1 {
		t.Errorf("%d emits right after a burst, want 1", n)
	}
	eventually(t, func() bool { return count() == 2 }, func() string { return fmt.Sprintf("%d emits", count()) })
	mu.Lock()
	gap := emits[1].Sub(emits[0])
	mu.Unlock()
	if gap < shareEventEvery-10*time.Millisecond {
		t.Errorf("two emits %v apart, want at least %v", gap, shareEventEvery)
	}
	time.Sleep(2 * shareEventEvery)
	if n := count(); n != 2 {
		t.Errorf("%d emits without changes, want 2", n)
	}
	k.changed()
	k.flush()
	k.changed()
	time.Sleep(2 * shareEventEvery)
	if n := count(); n != 4 {
		t.Errorf("%d emits after the last change and flush, want 4 (nothing after the flush)", n)
	}
}

// TestShareEventsLastIsFlush: a timer's emit that took its snapshot before the share ended can't publish after
// flush's: flush waits for it, so the ended snapshot is always the last one out.
func TestShareEventsLastIsFlush(t *testing.T) {
	var mu sync.Mutex
	state := "live"
	var out []string
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	block := false
	k := &coalescer{}
	k.emit = func() {
		mu.Lock()
		snap, wait := state, block
		block = false
		mu.Unlock()
		if wait { // the snapshot is taken; the publish waits
			entered <- struct{}{}
			<-release
		}
		mu.Lock()
		out = append(out, snap)
		mu.Unlock()
	}
	k.changed() // emits at once
	mu.Lock()
	block = true
	mu.Unlock()
	k.changed() // emits at the end of the interval: fire
	select {
	case <-entered:
	case <-time.After(waitTimeout):
		t.Fatal("the timer's emit never ran")
	}
	mu.Lock()
	state = "ended"
	mu.Unlock()
	flushed := make(chan struct{})
	go func() {
		k.flush()
		close(flushed)
	}()
	time.Sleep(50 * time.Millisecond) // flush runs into the emit in progress
	close(release)
	select {
	case <-flushed:
	case <-time.After(waitTimeout):
		t.Fatal("flush never returned")
	}
	emitted := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(out)
	}
	eventually(t, func() bool { return emitted() >= 3 }, func() string { return fmt.Sprintf("%d emits, want 3", emitted()) })
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"live", "live", "ended"}; !slices.Equal(out, want) {
		t.Errorf("emitted %v, want %v (the ended snapshot last)", out, want)
	}
}

// TestStopReasonError: StopError's text names the reason and the code.
func TestStopReasonError(t *testing.T) {
	for _, tc := range []struct {
		e    StopError
		want string
	}{
		{StopError{Reason: StopUnlinked, Code: protocol.ErrorCodeSessionRevoked}, "core: stopped: unlinked (session_revoked)"},
		{StopError{Reason: StopUpdateRequired}, "core: stopped: update_required"},
	} {
		if got := tc.e.Error(); got != tc.want {
			t.Errorf("Error() = %q, want %q", got, tc.want)
		}
	}
}
