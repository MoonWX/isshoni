package signal_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/signal"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// Every hub test runs inside a testing/synctest bubble on a signaltest.PipeNet: time is fake, so the tests use the
// real timeouts of DefaultConfig (10 s hello, 45 s idle, 5 min revalidation) and still run instantly, and a
// deadlock or a leaked goroutine fails the test.

const (
	testOrigin    = "https://watch.example.com"
	testVersion   = "0.1.0"
	wailsOrigin   = "wails://wails.localhost"
	recvTimeout   = time.Minute // fake time
	serverVersion = testVersion
)

// env is a hub with fakes, served on an in-memory network.
type env struct {
	t      *testing.T
	hub    *signal.Hub
	auth   *signaltest.Auth
	rooms  *signaltest.Rooms
	media  *signaltest.Media
	push   *signaltest.Push
	policy *signaltest.Policy
	reg    *prometheus.Registry
	logs   *syncBuffer
	srv    *signaltest.Server
	hc     *http.Client

	mu      sync.Mutex
	clients []*signaltest.Client
	users   int
}

// newEnv starts a hub. Call it inside a synctest bubble and defer close.
func newEnv(t *testing.T, opts ...func(*signal.Config)) *env {
	t.Helper()
	cfg := signal.DefaultConfig()
	cfg.PublicOrigin = testOrigin
	cfg.ServerVersion = serverVersion
	cfg.ResumeKey = bytes.Repeat([]byte{7}, 32)
	for _, o := range opts {
		o(&cfg)
	}
	e := &env{
		t:      t,
		auth:   signaltest.NewAuth(),
		rooms:  signaltest.NewRooms(),
		media:  signaltest.NewMedia(),
		push:   &signaltest.Push{},
		policy: &signaltest.Policy{},
		reg:    prometheus.NewRegistry(),
		logs:   &syncBuffer{},
	}
	hub, err := signal.New(cfg, signal.Deps{
		Auth:     e.auth,
		Rooms:    e.rooms,
		Media:    e.media,
		Policy:   e.policy.Get,
		Push:     e.push,
		ClientIP: signaltest.ClientIP,
		Log:      slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Metrics:  e.reg,
	})
	if err != nil {
		t.Fatalf("signal.New: %v", err)
	}
	e.hub = hub
	e.srv = signaltest.StartServer(hub)
	e.hc = e.srv.Net.HTTPClient()
	return e
}

// close closes every client, shuts the hub down and stops the server.
func (e *env) close() {
	e.mu.Lock()
	clients := e.clients
	e.mu.Unlock()
	for _, c := range clients {
		c.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := e.hub.Shutdown(ctx, protocol.ShutdownReasonStop); err != nil {
		e.t.Errorf("Shutdown: %v", err)
	}
	e.srv.Close()
}

// user registers a new user with one cookie session and returns the cookie value and the identity.
func (e *env) user(admin bool) (string, signal.Identity) {
	e.mu.Lock()
	e.users++
	n := e.users
	e.mu.Unlock()
	id := signal.Identity{
		UserID:    fmt.Sprintf("user%02d", n),
		Name:      fmt.Sprintf("name%02d", n),
		Admin:     admin,
		SessionID: fmt.Sprintf("session%02d", n),
	}
	cookie := fmt.Sprintf("cookie%02d", n)
	e.auth.AddSession(cookie, id)
	return cookie, id
}

// session adds another cookie session for an existing user.
func (e *env) session(id signal.Identity, sessionID string) string {
	id.SessionID = sessionID
	cookie := "cookie-" + sessionID
	e.auth.AddSession(cookie, id)
	return cookie
}

// headers builds upgrade headers: the cookie (if any), the Origin (if any) and the client IP (if any).
func headers(cookie, origin, ip string) http.Header {
	h := http.Header{}
	if cookie != "" {
		h = signaltest.CookieHeader(cookie)
	}
	if origin != "" {
		h.Set("Origin", origin)
	}
	if ip != "" {
		h.Set(signaltest.ClientIPHeader, ip)
	}
	return h
}

// dial opens a WebSocket; the env closes it at the end.
func (e *env) dial(o signaltest.DialOptions) (*signaltest.Client, error) {
	c, err := signaltest.Dial(context.Background(), e.hc, e.srv.URL("/ws"), o)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.clients = append(e.clients, c)
	e.mu.Unlock()
	return c, nil
}

// mustDial dials with headers and fails the test when the upgrade fails.
func (e *env) mustDial(h http.Header) *signaltest.Client {
	e.t.Helper()
	c, err := e.dial(signaltest.DialOptions{Header: h})
	if err != nil {
		e.t.Fatalf("dial: %v", err)
	}
	return c
}

// refused dials and returns the refusal, failing the test when the upgrade succeeds.
func (e *env) refused(h http.Header) *signaltest.RefusedError {
	e.t.Helper()
	c, err := e.dial(signaltest.DialOptions{Header: h})
	if err == nil {
		c.Close()
		e.t.Fatalf("dial succeeded, want a refusal")
	}
	var re *signaltest.RefusedError
	if !errors.As(err, &re) {
		e.t.Fatalf("dial: %v, want a refusal", err)
	}
	return re
}

// connect dials with the cookie from the browser origin and completes the handshake with hello.
func (e *env) connect(cookie string, hello protocol.Hello) (*signaltest.Client, protocol.Welcome) {
	e.t.Helper()
	c := e.mustDial(headers(cookie, testOrigin, ""))
	w, err := c.Hello(ctxT(e.t), hello)
	if err != nil {
		e.t.Fatalf("hello: %v", err)
	}
	return c, w
}

// ctxT is a context for one wait in a test (fake time), canceled at the latest when the test ends.
func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), recvTimeout)
	t.Cleanup(cancel)
	return ctx
}

// recv returns the next message.
func recv(t *testing.T, c *signaltest.Client) protocol.Envelope {
	t.Helper()
	env, err := c.Recv(ctxT(t))
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	return env
}

// expectType returns the next message and checks its type.
func expectType(t *testing.T, c *signaltest.Client, typ protocol.MessageType) protocol.Envelope {
	t.Helper()
	env := recv(t, c)
	if env.Type != typ {
		t.Fatalf("got %s %s, want %s", env.Type, env.Data, typ)
	}
	return env
}

// expectError reads the next message, an error with the code and scope, and returns it and its re.
func expectError(t *testing.T, c *signaltest.Client, code protocol.ErrorCode, scope protocol.ErrorScope) (protocol.Error, string) {
	t.Helper()
	env := expectType(t, c, protocol.MessageTypeError)
	e, err := protocol.Decode[protocol.Error](env)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != code || e.Scope != scope {
		t.Fatalf("got error %s (scope %s) %s, want %s (scope %s)", e.Code, e.Scope, env.Data, code, scope)
	}
	return e, env.Re
}

// expectClose waits for the socket to close and checks the close code.
func expectClose(t *testing.T, c *signaltest.Client, code protocol.CloseCode) {
	t.Helper()
	got, err := c.CloseStatus(ctxT(t))
	if err != nil {
		t.Fatalf("close status: %v", err)
	}
	if got != websocket.StatusCode(code) {
		t.Fatalf("closed with %d (%v), want %d", got, c.Err(), code)
	}
}

// expectFail reads error{code, scope} and then the close code that follows it (01 §12.1).
func expectFail(t *testing.T, c *signaltest.Client, code protocol.ErrorCode, scope protocol.ErrorScope) protocol.Error {
	t.Helper()
	e, _ := expectError(t, c, code, scope)
	expectClose(t, c, protocol.CloseCodeFor(code, scope))
	return e
}

// expectOpen checks that the socket is open and that nothing is queued.
func expectOpen(t *testing.T, c *signaltest.Client) {
	t.Helper()
	select {
	case <-c.Closed():
		t.Fatalf("socket closed: %v", c.Err())
	default:
	}
	if n := c.Pending(); n != 0 {
		t.Fatalf("%d unexpected messages queued", n)
	}
}

// ping sends a ping notification and waits for its pong: the actor has handled every earlier message.
func ping(t *testing.T, c *signaltest.Client) {
	t.Helper()
	if err := c.Send(protocol.MessageTypePing, "", protocol.Ping{T: 42}); err != nil {
		t.Fatalf("send ping: %v", err)
	}
	env := expectType(t, c, protocol.MessageTypePong)
	if p, _ := protocol.Decode[protocol.Pong](env); p.T != 42 {
		t.Fatalf("pong %s, want t 42", env.Data)
	}
}

// request sends a request with a fresh id and returns the id.
func request(t *testing.T, c *signaltest.Client, typ protocol.MessageType, data any) string {
	t.Helper()
	id := c.NextID()
	if err := c.Send(typ, id, data); err != nil {
		t.Fatalf("send %s: %v", typ, err)
	}
	return id
}

// syncBuffer is a log sink that tests can read while the hub writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// count returns the number of log lines that contain every one of subs.
func (s *syncBuffer) count(subs ...string) int {
	n := 0
	for line := range strings.Lines(s.String()) {
		ok := true
		for _, sub := range subs {
			if !strings.Contains(line, sub) {
				ok = false
				break
			}
		}
		if ok {
			n++
		}
	}
	return n
}

// metric returns the value of the series name with the given labels (name=value pairs), or -1 when absent.
func (e *env) metric(name string, labels ...string) float64 {
	e.t.Helper()
	mfs, err := e.reg.Gather()
	if err != nil {
		e.t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	next:
		for _, m := range mf.GetMetric() {
			have := map[string]string{}
			for _, lp := range m.GetLabel() {
				have[lp.GetName()] = lp.GetValue()
			}
			for i := 0; i+1 < len(labels); i += 2 {
				if have[labels[i]] != labels[i+1] {
					continue next
				}
			}
			switch {
			case m.GetCounter() != nil:
				return m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				return m.GetGauge().GetValue()
			}
		}
	}
	return -1
}
