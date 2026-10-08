package signal_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"

	"github.com/MoonWX/isshoni/internal/client/signal"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// Every test runs inside a testing/synctest bubble on a signaltest.PipeNet: time is fake, so the tests use the
// client's real timeouts (10 s, 15 s, 30 s) and measure its backoff delays exactly, and a goroutine that outlives
// its test fails it.

// wait bounds one wait of a test, in fake time: longer than the longest wait of the client (5 min).
const wait = 10 * time.Minute

// ctxT is a context for one wait in a test, canceled at the latest when the test ends.
func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	t.Cleanup(cancel)
	return ctx
}

// options are a test client's options for url through hc.
func options(url string, hc *http.Client) signal.Options {
	return signal.Options{
		URL:        url,
		HTTPClient: hc,
		Client:     protocol.ClientInfo{Kind: protocol.ClientKindTool, Version: "0.1.0", OS: protocol.ClientOSLinux},
		Role:       protocol.RoleFull,
		Caps: protocol.Caps{
			Decode: []protocol.CodecKey{protocol.CodecH264High, protocol.CodecOpus},
			Encode: []protocol.CodecKey{protocol.CodecH264High, protocol.CodecOpus},
		},
	}
}

// nextState returns the client's next state change.
func nextState(t *testing.T, c *signal.Client) signal.StateChange {
	t.Helper()
	select {
	case sc, ok := <-c.States():
		if !ok {
			t.Fatal("States is closed, want a state change")
		}
		return sc
	case <-time.After(wait):
		t.Fatal("no state change")
	}
	return signal.StateChange{}
}

// expectStates reads one state change per wanted state and returns the last one.
func expectStates(t *testing.T, c *signal.Client, want ...signal.State) signal.StateChange {
	t.Helper()
	var sc signal.StateChange
	for i, w := range want {
		if sc = nextState(t, c); sc.State != w {
			t.Fatalf("state change %d is %s (%+v), want %s of %v", i, sc.State, sc, w, want)
		}
	}
	return sc
}

// expectBackoff reads the next state change, a backoff, and then waits for the attempt that follows it. It checks
// that the client waited exactly the delay it reported, and returns the backoff.
func expectBackoff(t *testing.T, c *signal.Client) signal.StateChange {
	t.Helper()
	sc := expectStates(t, c, signal.StateBackoff)
	start := time.Now()
	expectStates(t, c, signal.StateConnecting)
	if got := time.Since(start); got != sc.Delay {
		t.Fatalf("the client waited %v, its backoff said %v", got, sc.Delay)
	}
	return sc
}

// noStates checks that no state change is waiting.
func noStates(t *testing.T, c *signal.Client) {
	t.Helper()
	synctest.Wait()
	select {
	case sc, ok := <-c.States():
		t.Fatalf("unexpected state change %+v (open %v)", sc, ok)
	default:
	}
}

// nextEvent returns the next notification of the client.
func nextEvent(t *testing.T, c *signal.Client) protocol.Envelope {
	t.Helper()
	select {
	case env, ok := <-c.Events():
		if !ok {
			t.Fatal("Events is closed, want a notification")
		}
		return env
	case <-time.After(wait):
		t.Fatal("no notification")
	}
	return protocol.Envelope{}
}

// expectEvent returns the next notification, which must have type typ.
func expectEvent(t *testing.T, c *signal.Client, typ protocol.MessageType) protocol.Envelope {
	t.Helper()
	env := nextEvent(t, c)
	if env.Type != typ {
		t.Fatalf("got notification %s %s, want %s", env.Type, env.Data, typ)
	}
	return env
}

// noEvents checks that no notification is waiting.
func noEvents(t *testing.T, c *signal.Client) {
	t.Helper()
	synctest.Wait()
	select {
	case env, ok := <-c.Events():
		t.Fatalf("unexpected notification %s %s (open %v)", env.Type, env.Data, ok)
	default:
	}
}

// expectStopped checks that the client has stopped: both channels are closed (after the changes still waiting in
// States, which it returns), and requests and notifications fail with ErrNotReady.
func expectStopped(t *testing.T, c *signal.Client) []signal.StateChange {
	t.Helper()
	synctest.Wait()
	var rest []signal.StateChange
	for sc := range c.States() { // ends: the channel is closed
		rest = append(rest, sc)
	}
	for range c.Events() { // ends: the channel is closed too
	}
	if err := c.Request(ctxT(t), protocol.MessageTypeRoomLeave, protocol.Empty{}, nil); !errors.Is(err, signal.ErrNotReady) {
		t.Errorf("Request on a stopped client: %v, want ErrNotReady", err)
	}
	if err := c.Send(protocol.MessageTypeStats, protocol.ClientStats{}); !errors.Is(err, signal.ErrNotReady) {
		t.Errorf("Send on a stopped client: %v, want ErrNotReady", err)
	}
	return rest
}

// inBackoffBounds checks delay against 01 §10.2 for attempt n: min(10 s, 0.5 s × 2ⁿ) × U(0.8, 1.2), clamped to
// [0.5 s, 10 s].
func inBackoffBounds(t *testing.T, n int, delay time.Duration) {
	t.Helper()
	base := signal.BackoffMax
	if n < 5 {
		base = min(signal.BackoffMax, signal.BackoffMin<<n)
	}
	lo := max(signal.BackoffMin, time.Duration(float64(base)*0.8))
	hi := min(signal.BackoffMax, time.Duration(float64(base)*1.2))
	if delay < lo || delay > hi {
		t.Errorf("backoff delay %d is %v, want it in [%v, %v]", n, delay, lo, hi)
	}
}

// cutNet dials a fake server's network through connections that a test can break. After cut, what the client
// writes vanishes and nothing arrives, without an error on either side, like a network path that has gone black.
// After broken, the client's writes fail.
type cutNet struct {
	net    *signaltest.PipeNet
	cut    atomic.Bool
	broken atomic.Bool
}

func (n *cutNet) client() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := n.net.Dial(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &cutConn{Conn: c, net: n}, nil
		}}}
}

type cutConn struct {
	net.Conn
	net *cutNet
}

func (c *cutConn) Write(p []byte) (int, error) {
	switch {
	case c.net.broken.Load():
		return 0, errors.New("write: broken pipe")
	case c.net.cut.Load():
		return len(p), nil
	}
	return c.Conn.Write(p)
}

// ---- A scripted server ----

// fakeServer is a signaling server that the test scripts: it accepts WebSockets on an in-memory network and hands
// each one to the test as a fakeConn. By default it answers every hello with a welcome and every ping with a pong.
type fakeServer struct {
	t   *testing.T
	srv *signaltest.Server
	hc  *http.Client

	conns chan *fakeConn

	status   atomic.Int32 // not 0: refuse upgrades with this HTTP status
	hang     atomic.Bool  // don't answer upgrades at all
	slam     atomic.Bool  // accept the WebSocket and drop it at once
	manual   atomic.Bool  // don't answer hello: the test does
	noPong   atomic.Bool  // don't answer pings
	noResume atomic.Bool  // answer a hello that has a resume token with resumed: false and a new connection
	pingMs   atomic.Int32 // limits.pingIntervalMs of the welcomes (0: 15000)
	upgrades atomic.Int32 // upgrade requests seen
	welcomes atomic.Int32 // welcomes sent

	mu  sync.Mutex
	all []*fakeConn
}

func newFakeServer(t *testing.T) *fakeServer {
	f := &fakeServer{t: t, conns: make(chan *fakeConn, 64)}
	f.srv = signaltest.StartServer(f)
	f.hc = f.srv.Net.HTTPClient()
	return f
}

// close closes every socket and stops the server.
func (f *fakeServer) close() {
	f.mu.Lock()
	all := f.all
	f.mu.Unlock()
	for _, fc := range all {
		fc.kill()
	}
	f.srv.Close()
}

func (f *fakeServer) options() signal.Options { return options(f.srv.URL("/ws"), f.hc) }

// dial connects a client and returns it with its server side.
func (f *fakeServer) dial(o signal.Options) (*signal.Client, *fakeConn) {
	f.t.Helper()
	c, err := signal.Dial(ctxT(f.t), o)
	if err != nil {
		f.t.Fatalf("Dial: %v", err)
	}
	return c, f.conn()
}

// conn returns the next accepted socket.
func (f *fakeServer) conn() *fakeConn {
	f.t.Helper()
	select {
	case fc := <-f.conns:
		return fc
	case <-time.After(wait):
		f.t.Fatal("no connection to the fake server")
	}
	return nil
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.upgrades.Add(1)
	if f.hang.Load() {
		<-r.Context().Done()
		return
	}
	if st := f.status.Load(); st != 0 {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(int(st))
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	if f.slam.Load() {
		_ = ws.CloseNow()
		return
	}
	ws.SetReadLimit(1 << 20)
	fc := &fakeConn{f: f, ws: ws, header: r.Header.Clone(), msgs: make(chan protocol.Envelope, 1024),
		done: make(chan struct{})}
	f.mu.Lock()
	f.all = append(f.all, fc)
	f.mu.Unlock()
	go fc.read(context.WithoutCancel(r.Context())) // the socket outlives its upgrade request
	f.conns <- fc
}

// welcome is the fake server's welcome number n.
func (f *fakeServer) welcome(n int32) protocol.Welcome {
	ping := int(f.pingMs.Load())
	if ping == 0 {
		ping = 15000
	}
	return protocol.Welcome{
		Protocol:      protocol.Version,
		ServerVersion: "0.1.0",
		Limits: protocol.Limits{MaxMessageBytes: protocol.MaxMessageBytes, MaxSDPBytes: protocol.MaxSDPBytes,
			MaxSharesPerUser: 4, MessagesPerSecond: 20, MessageBurst: 100, PingIntervalMs: ping,
			IdleTimeoutMs: 45000, GraceMs: 30000},
		ConnectionID:  "c_aaaaaaaaaaaaaaaa",
		ResumeToken:   protocol.Secret(fmt.Sprintf("r1.token%d", n)),
		DefaultRoomID: "lounge",
		User:          protocol.UserInfo{ID: "user01", Name: "alex"},
		ServerTime:    time.Now(),
	}
}

// fakeConn is the server side of one of the client's sockets. A reader goroutine queues the client's messages (and
// answers hello and ping unless the server is told not to); the test reads them with next.
type fakeConn struct {
	f      *fakeServer
	ws     *websocket.Conn
	header http.Header // of the upgrade request

	msgs  chan protocol.Envelope
	done  chan struct{}
	err   error        // the reader's final error; read it after done is closed
	pings atomic.Int32 // pings answered
}

func (fc *fakeConn) read(ctx context.Context) {
	defer close(fc.done)
	for {
		typ, b, err := fc.ws.Read(ctx)
		if err != nil {
			fc.err = err
			return
		}
		if typ != websocket.MessageText {
			fc.err = errors.New("the client sent a binary frame")
			return
		}
		env, err := protocol.ParseEnvelope(b)
		if err != nil {
			fc.err = fmt.Errorf("bad message from the client: %w", err)
			return
		}
		switch {
		case env.Type == protocol.MessageTypeHello && !fc.f.manual.Load():
			fc.answerHello(env)
		case env.Type == protocol.MessageTypePing && !fc.f.noPong.Load():
			p, _ := protocol.Decode[protocol.Ping](env)
			fc.pings.Add(1)
			_ = fc.write(protocol.MessageTypePong, "", protocol.Pong{T: p.T, ServerTimeMs: time.Now().UnixMilli()})
			continue
		}
		fc.msgs <- env
	}
}

// answerHello sends the welcome for a hello: resumed when it carries a resume token, unless the server has "lost"
// its connections.
func (fc *fakeConn) answerHello(env protocol.Envelope) {
	h, _ := protocol.Decode[protocol.Hello](env)
	n := fc.f.welcomes.Add(1)
	w := fc.f.welcome(n)
	switch {
	case h.ResumeToken != "" && !fc.f.noResume.Load():
		w.Resumed = true
	case n > 1:
		w.ConnectionID = fmt.Sprintf("c_bbbbbbbbbbbbbb%02d", n)
	}
	_ = fc.write(protocol.MessageTypeWelcome, env.ID, w)
}

func (fc *fakeConn) write(typ protocol.MessageType, re string, data any) error {
	b, err := protocol.Marshal(typ, "", re, data)
	if err != nil {
		return err
	}
	return fc.ws.Write(context.Background(), websocket.MessageText, b)
}

// send sends one server message; re is the id of the request it answers.
func (fc *fakeConn) send(typ protocol.MessageType, re string, data any) {
	fc.f.t.Helper()
	if err := fc.write(typ, re, data); err != nil {
		fc.f.t.Fatalf("fake server: send %s: %v", typ, err)
	}
}

// sendRaw sends one text frame as is.
func (fc *fakeConn) sendRaw(frame string) {
	fc.f.t.Helper()
	if err := fc.ws.Write(context.Background(), websocket.MessageText, []byte(frame)); err != nil {
		fc.f.t.Fatalf("fake server: send: %v", err)
	}
}

// fail sends error e and closes with the close code that follows it (01 §12.1), as the hub does.
func (fc *fakeConn) fail(e protocol.Error, re string) {
	fc.f.t.Helper()
	fc.send(protocol.MessageTypeError, re, e)
	fc.closeWith(protocol.CloseCodeFor(e.Code, e.Scope))
}

// closeWith closes the socket with a close frame.
func (fc *fakeConn) closeWith(code protocol.CloseCode) {
	_ = fc.ws.Close(websocket.StatusCode(code), "")
	<-fc.done
}

// kill closes the socket without a close frame and waits for the reader.
func (fc *fakeConn) kill() {
	_ = fc.ws.CloseNow()
	<-fc.done
}

// next returns the client's next message (pings and hellos that the server answered by itself come too, except
// answered pings).
func (fc *fakeConn) next() protocol.Envelope {
	fc.f.t.Helper()
	select {
	case env := <-fc.msgs:
		return env
	case <-fc.done:
		select {
		case env := <-fc.msgs:
			return env
		default:
		}
		fc.f.t.Fatalf("the client's socket ended (%v), want a message", fc.err)
	case <-time.After(wait):
		fc.f.t.Fatal("no message from the client")
	}
	return protocol.Envelope{}
}

// expect returns the client's next message, which must have type typ.
func (fc *fakeConn) expect(typ protocol.MessageType) protocol.Envelope {
	fc.f.t.Helper()
	env := fc.next()
	if env.Type != typ {
		fc.f.t.Fatalf("the client sent %s %s, want %s", env.Type, env.Data, typ)
	}
	return env
}

// hello returns the hello the client opened this socket with.
func (fc *fakeConn) hello() protocol.Hello {
	fc.f.t.Helper()
	env := fc.expect(protocol.MessageTypeHello)
	if env.ID == "" {
		fc.f.t.Fatal("hello has no id")
	}
	h, err := protocol.Decode[protocol.Hello](env)
	if err != nil {
		fc.f.t.Fatalf("hello: %v", err)
	}
	return h
}

// closed waits for the client to end the socket and returns its close code, -1 when it sent no close frame.
func (fc *fakeConn) closed() websocket.StatusCode {
	fc.f.t.Helper()
	select {
	case <-fc.done:
		return websocket.CloseStatus(fc.err)
	case <-time.After(wait):
		fc.f.t.Fatal("the client did not end the socket")
	}
	return 0
}

// open checks that the client has not ended the socket.
func (fc *fakeConn) open() {
	fc.f.t.Helper()
	synctest.Wait()
	select {
	case <-fc.done:
		fc.f.t.Fatalf("the client's socket ended: %v", fc.err)
	default:
	}
}
