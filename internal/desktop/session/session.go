// Package session runs C2, the desktop app's publisher connection (docs/m2/03-desktop-app.md, "03" below, §9): one
// 01 core client (internal/client/core) for the active, linked server, authenticated as this device through the
// core.Auth adapter over that server's 02 Manager (internal/client/deviceauth).
//
// The session builds the core client when the active server is linked, and builds a new one after a server switch,
// a relink, and a terminal stop that 02 cured (a session_revoked or account_disabled stop after which the manager's
// refresh kept it linked). It closes the core before it builds one for another server, on an incompatible verdict,
// and when the active server is unlinked. The adapter is bound to the Manager of the origin its core was built for,
// never to "whichever server is active now", so a core still connected to the old server during a switch can't get
// the new server's token (03 §5.8).
//
// What the app calls: Run (the app's part), Ready (the share controller's wait for C2, 03 §10.1), ConnectionID
// (App.Info's publisherConnectionId), OnAuthState (02's OnState: every link change of any server) and Refresh (after
// the active server changed: 02's SetActive tells nobody). Nothing it logs holds a token, a user or device name or a
// window title.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MoonWX/isshoni/internal/client/core"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// RebuildEvery is the shortest gap between two core clients the session builds, so a manager whose state flips
// can't make it dial in a loop.
const RebuildEvery = time.Second

// Servers is what the session reads of 02's server list; ServersOf adapts *deviceauth.Servers.
type Servers interface {
	// Active returns the active server's manager, nil when no server is listed.
	Active() Manager
}

// Manager is what the session uses of one server's 02 Manager; *deviceauth.Manager implements it.
type Manager interface {
	State() deviceauth.State
	AccessToken(ctx context.Context, forceRefresh bool) (protocol.Secret, error)
	SessionEnded(ctx context.Context, code protocol.ErrorCode) deviceauth.State
	HelloRefused(e *protocol.Error)
}

var _ Manager = (*deviceauth.Manager)(nil)

// ServersOf adapts 02's *deviceauth.Servers to Servers.
func ServersOf(s *deviceauth.Servers) Servers { return servers{s} }

type servers struct{ s *deviceauth.Servers }

func (l servers) Active() Manager {
	m := l.s.Active()
	if m == nil {
		return nil // no typed nil inside the interface
	}
	return m
}

// Client is one core client as the session runs it and the app uses it; *core.Client implements it.
type Client interface {
	Run(ctx context.Context) error
	Status() core.Status
	Subscribe() (<-chan core.Event, func())
	StartShare(ctx context.Context, r core.ShareRequest) (*core.Share, error)
	StopOther(ctx context.Context, roomID, shareID string) error
	WatchServerStats(on bool)
	Close() error
}

var _ Client = (*core.Client)(nil)

// Options configure a Session.
type Options struct {
	// Servers is 02's server list (ServersOf); required.
	Servers Servers
	// Engine is the media engine every core client gets (01's Options.Engine); required.
	Engine media.Engine
	// Client goes into C2's hello: {Kind: desktop, Version, OS}; zero fields take the core's defaults.
	Client protocol.ClientInfo
	// AudioConfig is 05's Service.AudioConfig (01's Options.AudioConfig); nil is the core's default.
	AudioConfig func() media.AudioConfig
	// HTTPClient makes the WebSocket upgrade requests; nil is the core's default.
	HTTPClient *http.Client
	// Log gets the session's lines (component=session) and the core's (component=core); nil means slog.Default().
	Log *slog.Logger
	// NewClient builds a core client; nil means core.New. Tests pass fakes.
	NewClient func(core.Options) (Client, error)
}

// Session is C2. Its methods are safe for concurrent use.
type Session struct {
	o         Options
	log       *slog.Logger
	coreLog   *slog.Logger
	newClient func(core.Options) (Client, error)
	kick      chan struct{} // one slot: something changed, look again

	mu      sync.Mutex // guards the fields below
	ran     bool
	cur     *conn         // the core client that runs, nil when none
	changed chan struct{} // closed and replaced at every change Ready waits for
}

// conn is one core client and what the session knows of it.
type conn struct {
	client   Client
	origin   string
	auth     *auth
	done     chan struct{} // closed when Run returned
	err      error         // Run's error; read after done
	closeReq atomic.Bool   // the adapter saw ErrIncompatible: close this core
}

// New checks o and returns a Session. It starts nothing (Run does).
func New(o Options) (*Session, error) {
	switch {
	case o.Servers == nil:
		return nil, errors.New("session: Options.Servers is required")
	case o.Engine == nil:
		return nil, errors.New("session: Options.Engine is required")
	}
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	nc := o.NewClient
	if nc == nil {
		nc = func(co core.Options) (Client, error) {
			c, err := core.New(co)
			if err != nil {
				return nil, err // no typed nil inside the interface
			}
			return c, nil
		}
	}
	return &Session{o: o, log: log.With("component", "session"), coreLog: log.With("component", "core"),
		newClient: nc, kick: make(chan struct{}, 1), changed: make(chan struct{})}, nil
}

// Run runs C2 until ctx ends: a core client for the active server while it is linked, rebuilt and closed as the
// package comment says. It returns nil when ctx ended, after the core client it ran has returned (01's Close: at
// most 3 s plus the close handshake). Run may be called once.
func (s *Session) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.ran {
		s.mu.Unlock()
		return errors.New("session: Run called twice")
	}
	s.ran = true
	s.mu.Unlock()

	var (
		cur     *conn
		blocked bool      // a terminal stop that wasn't cured: build nothing until something changes
		built   time.Time // when the last core client was built
		retry   *time.Timer
	)
	defer func() {
		if retry != nil {
			retry.Stop()
		}
		if cur != nil {
			s.closeConn(cur)
		}
		s.setCur(nil)
	}()
	for {
		m := s.o.Servers.Active()
		var st deviceauth.State
		if m != nil {
			st = m.State()
		}
		if cur != nil && (m == nil || st.Origin != cur.origin || ended(st) || cur.closeReq.Load()) {
			if cur.closeReq.Load() {
				s.log.Info("session: the server and the app are incompatible; the publisher connection closes")
				blocked = true
			}
			s.closeConn(cur)
			s.setCur(nil)
			cur = nil
		}
		var retryC <-chan time.Time
		if cur == nil && !blocked && m != nil && runnable(st) {
			if wait := RebuildEvery - time.Since(built); !built.IsZero() && wait > 0 {
				if retry == nil {
					retry = time.NewTimer(wait)
				}
				retryC = retry.C
			} else {
				if retry != nil {
					retry.Stop()
					retry = nil
				}
				built = time.Now()
				c, err := s.start(ctx, m, st.Origin)
				if err != nil {
					s.log.Error("session: the publisher connection can't be built", "origin", st.Origin, "err", err)
					blocked = true
				} else {
					cur = c
					s.setCur(c)
				}
			}
		}
		var done <-chan struct{}
		if cur != nil {
			done = cur.done
		}
		select {
		case <-ctx.Done():
			return nil
		case <-s.kick:
			blocked = false
		case <-retryC:
			retry = nil
		case <-done:
			blocked = s.stopped(cur)
			s.setCur(nil)
			cur = nil
		}
	}
}

// runnable reports whether a core client should run for a server in state st: linked or offline (02 retries the
// refresh; the core treats ErrOffline as transient), or linking again while a credential exists.
func runnable(st deviceauth.State) bool {
	switch st.Status {
	case deviceauth.StatusLinked, deviceauth.StatusOffline:
		return true
	case deviceauth.StatusLinking:
		return st.User != nil
	}
	return false
}

// ended reports a state in which a running core client is closed: unlinked (signed out, revoked) or incompatible.
func ended(st deviceauth.State) bool {
	return st.Status == deviceauth.StatusUnlinked || st.Status == deviceauth.StatusIncompatible
}

// start builds a core client for origin over m's adapter and runs it.
func (s *Session) start(ctx context.Context, m Manager, origin string) (*conn, error) {
	c := &conn{origin: origin, done: make(chan struct{})}
	c.auth = &auth{m: m, onIncompatible: func() {
		if c.closeReq.CompareAndSwap(false, true) {
			s.wake()
		}
	}}
	client, err := s.newClient(core.Options{
		ServerURL:   origin,
		Auth:        c.auth,
		Client:      s.o.Client,
		Engine:      s.o.Engine,
		AudioConfig: s.o.AudioConfig,
		HTTPClient:  s.o.HTTPClient,
		Logger:      s.coreLog,
	})
	if err != nil {
		return nil, err
	}
	c.client = client
	events, unsubscribe := client.Subscribe()
	go func() { // ends when Run's goroutine unsubscribes, which closes the channel
		for ev := range events {
			if ev.Kind == core.EventStatus || ev.Kind == core.EventResync {
				s.notify()
			}
		}
	}()
	go func() {
		defer close(c.done)
		defer unsubscribe()
		c.err = client.Run(ctx)
	}()
	s.log.Info("session: the publisher connection starts", "origin", origin)
	return c, nil
}

// closeConn closes c's core client and waits until its Run returned.
func (s *Session) closeConn(c *conn) {
	_ = c.client.Close()
	<-c.done
}

// stopped handles a core client whose Run returned by itself (03 §9) and reports whether the session must wait for a
// change before it builds another: a stop that 02 cured (session_revoked or account_disabled, after which the
// manager stayed linked) builds a new client at once; update_required hands the server's refusal to 02
// (Manager.HelloRefused: compat.FromError, then the version screen through auth.state) and waits; the other stops
// (too_many_connections, replaced, bad_message, unlinked without a cure) wait.
func (s *Session) stopped(c *conn) bool {
	var se *core.StopError
	if !errors.As(c.err, &se) {
		if c.err != nil {
			s.log.Warn("session: the publisher connection stopped", "err", "error")
			return true
		}
		return false // closed: the next look decides
	}
	s.log.Info("session: the publisher connection stopped", "err", se.Error())
	switch se.Reason {
	case core.StopUnlinked, core.StopDisabled:
		st := c.auth.m.State()
		if e, ok := c.auth.endedState(); ok {
			st = e
		}
		return st.Status != deviceauth.StatusLinked
	case core.StopUpdateRequired:
		if se.Err != nil {
			c.auth.m.HelloRefused(se.Err)
		}
	}
	return true
}

// setCur makes c the running core client (nil: none) and wakes Ready's waiters.
func (s *Session) setCur(c *conn) {
	s.mu.Lock()
	s.cur = c
	s.mu.Unlock()
	s.notify()
}

// notify wakes Ready's waiters.
func (s *Session) notify() {
	s.mu.Lock()
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}

// wake makes Run look again, and wakes Ready's waiters.
func (s *Session) wake() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
	s.notify()
}

// OnAuthState is 02's OnState (through the app): any server's link changed. Run looks again: it builds a core
// client after a relink, and closes it when the active server was unlinked or became incompatible. It never blocks.
func (s *Session) OnAuthState(deviceauth.State) { s.wake() }

// Refresh makes Run look again after the active server changed (02's Servers.Add, SetActive and Remove tell nobody):
// the old core client is closed and one for the new active server is built. It never blocks.
func (s *Session) Refresh() { s.wake() }

// Ready returns C2's core client once it is online, and the origin of its server, waiting until ctx ends (then
// ctx's error). It fails at once with an error wrapping 02's deviceauth.ErrNotLinked when no server is listed or
// the active one isn't linked, and deviceauth.ErrIncompatible while it is incompatible.
func (s *Session) Ready(ctx context.Context) (Client, string, error) {
	for {
		s.mu.Lock()
		c, changed := s.cur, s.changed
		s.mu.Unlock()
		m := s.o.Servers.Active()
		if m == nil {
			return nil, "", fmt.Errorf("session: no server: %w", deviceauth.ErrNotLinked)
		}
		st := m.State()
		switch {
		case st.Status == deviceauth.StatusIncompatible:
			return nil, "", fmt.Errorf("session: %w", deviceauth.ErrIncompatible)
		case !runnable(st):
			return nil, "", fmt.Errorf("session: %w", deviceauth.ErrNotLinked)
		}
		if c != nil && c.origin == st.Origin && c.client.Status().Conn == core.ConnOnline {
			return c.client, c.origin, nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
}

// ConnectionID is C2's connection id while it is online (App.Info's publisherConnectionId), else "".
func (s *Session) ConnectionID() string {
	s.mu.Lock()
	c := s.cur
	s.mu.Unlock()
	if c == nil {
		return ""
	}
	if st := c.client.Status(); st.Conn == core.ConnOnline {
		return st.ConnectionID
	}
	return ""
}
