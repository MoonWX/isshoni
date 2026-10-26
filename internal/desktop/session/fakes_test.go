package session

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"

	"github.com/MoonWX/isshoni/internal/client/core"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/protocol"
)

const (
	originA = "https://a.example.com"
	originB = "https://b.example.com"
)

// fakeManager is one server's 02 Manager.
type fakeManager struct {
	mu         sync.Mutex
	state      deviceauth.State
	token      protocol.Secret
	tokenErr   error
	tokenCalls []bool               // each AccessToken's forceRefresh
	sessions   []protocol.ErrorCode // each SessionEnded's code
	afterEnd   *deviceauth.State    // the state SessionEnded leaves (nil: unchanged)
	hellos     []*protocol.Error
}

func newManager(origin string, status deviceauth.Status) *fakeManager {
	return &fakeManager{state: deviceauth.State{Origin: origin, Status: status},
		token: protocol.Secret("isa_" + origin)}
}

func (m *fakeManager) State() deviceauth.State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *fakeManager) AccessToken(_ context.Context, force bool) (protocol.Secret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokenCalls = append(m.tokenCalls, force)
	if m.tokenErr != nil {
		return "", m.tokenErr
	}
	return m.token, nil
}

func (m *fakeManager) SessionEnded(_ context.Context, code protocol.ErrorCode) deviceauth.State {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions = append(m.sessions, code)
	if m.afterEnd != nil {
		m.state = *m.afterEnd
	}
	return m.state
}

func (m *fakeManager) HelloRefused(e *protocol.Error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hellos = append(m.hellos, e)
}

func (m *fakeManager) set(fn func(m *fakeManager)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(m)
}

func (m *fakeManager) calls() ([]bool, []protocol.ErrorCode, []*protocol.Error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.tokenCalls), slices.Clone(m.sessions), slices.Clone(m.hellos)
}

// fakeServers is 02's server list with a switchable active server.
type fakeServers struct {
	mu     sync.Mutex
	active *fakeManager
}

func (s *fakeServers) Active() Manager {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil {
		return nil
	}
	return s.active
}

func (s *fakeServers) setActive(m *fakeManager) {
	s.mu.Lock()
	s.active = m
	s.mu.Unlock()
}

// fakeClient is a core client. Its Run is run's script, by default: online until ctx ends or Close.
type fakeClient struct {
	opts core.Options
	run  func(ctx context.Context, c *fakeClient) error

	closed    chan struct{}
	closeOnce sync.Once

	mu      sync.Mutex
	status  core.Status
	subs    map[int]chan core.Event
	nextSub int
	closes  int
}

func (c *fakeClient) Run(ctx context.Context) error {
	if c.run != nil {
		return c.run(ctx, c)
	}
	c.goOnline()
	c.waitClosed(ctx)
	return nil
}

// goOnline makes the client online with a connection id and tells its subscribers.
func (c *fakeClient) goOnline() {
	c.setStatus(core.Status{Conn: core.ConnOnline, ConnectionID: "c_" + string(c.opts.ServerURL[8])})
}

// waitClosed waits until ctx ends or Close.
func (c *fakeClient) waitClosed(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-c.closed:
	}
	c.setStatus(core.Status{Conn: core.ConnStopped})
}

func (c *fakeClient) setStatus(st core.Status) {
	c.mu.Lock()
	c.status = st
	subs := make([]chan core.Event, 0, len(c.subs))
	for _, ch := range c.subs {
		subs = append(subs, ch)
	}
	for _, ch := range subs {
		select {
		case ch <- core.Event{Kind: core.EventStatus, Status: &st}:
		default:
		}
	}
	c.mu.Unlock()
}

func (c *fakeClient) Status() core.Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

func (c *fakeClient) Subscribe() (<-chan core.Event, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextSub
	c.nextSub++
	ch := make(chan core.Event, 64)
	c.subs[id] = ch
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			c.mu.Lock()
			delete(c.subs, id)
			c.mu.Unlock()
			close(ch)
		})
	}
}

func (c *fakeClient) StartShare(context.Context, core.ShareRequest) (*core.Share, error) {
	return nil, errors.New("fake: no shares")
}
func (c *fakeClient) StopOther(context.Context, string, string) error { return nil }
func (c *fakeClient) WatchServerStats(bool)                           {}

func (c *fakeClient) Close() error {
	c.mu.Lock()
	c.closes++
	c.mu.Unlock()
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *fakeClient) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

// factory builds fake clients: script(n) is the n-th client's Run script (nil: the default).
type factory struct {
	script func(n int) func(ctx context.Context, c *fakeClient) error

	mu      sync.Mutex
	clients []*fakeClient
}

func (f *factory) new(o core.Options) (Client, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := &fakeClient{opts: o, closed: make(chan struct{}), subs: map[int]chan core.Event{},
		status: core.Status{Conn: core.ConnConnecting}}
	if f.script != nil {
		c.run = f.script(len(f.clients))
	}
	f.clients = append(f.clients, c)
	return c, nil
}

func (f *factory) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.clients)
}

func (f *factory) get(i int) *fakeClient {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clients[i]
}

// env is a session over fakes, running until the test ends.
type env struct {
	srv  *fakeServers
	fac  *factory
	eng  media.Engine
	sess *Session
}

func newEnv(t *testing.T, active *fakeManager, script func(n int) func(context.Context, *fakeClient) error) *env {
	t.Helper()
	e := &env{srv: &fakeServers{active: active}, fac: &factory{script: script}, eng: fake.NewEngine(fake.EngineConfig{})}
	t.Cleanup(func() { _ = e.eng.Close() })
	s, err := New(Options{Servers: e.srv, Engine: e.eng, Client: protocol.ClientInfo{Kind: protocol.ClientKindDesktop,
		Version: "0.2.0"}, Log: slog.New(slog.DiscardHandler), NewClient: e.fac.new})
	if err != nil {
		t.Fatal(err)
	}
	e.sess = s
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	return e
}
