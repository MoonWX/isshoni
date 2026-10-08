package signal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// Options configure Dial.
type Options struct {
	// URL is the server's signaling endpoint: "wss://example.com/ws" (ws: against a dev server).
	URL string
	// Header is sent with every upgrade request. A tool or test client puts its session cookie here (01 §3.2); a
	// bearer client leaves it empty. Without an Origin header the server takes the client for a non-browser one.
	Header http.Header
	// Token returns the bearer access token for hello.auth (native apps, M2+); nil means cookie auth. It is called
	// before every hello, also the ones that resume, with a context that ends after ConnectTimeout: the place to
	// refresh a token that is about to expire (01 §3.2). An error fails that attempt like a failed connect. The
	// token goes nowhere but into hello.auth.
	//
	// An unauthenticated answer stops the client like every error in scope session. The "refresh once and retry"
	// of 01 §3.2 is then the app's: it dials again, as a new connection. The M1 server answers every bearer hello
	// with unauthenticated.
	Token func(ctx context.Context) (protocol.Secret, error)
	// Client, Role, Caps and Features go into every hello (01 §8.2). A resume keeps the connection's client info,
	// role and features and takes the caps anew (01 §10.3).
	Client   protocol.ClientInfo
	Role     protocol.Role
	Caps     protocol.Caps
	Features []protocol.Feature
	// HTTPClient makes the upgrade requests; nil means http.DefaultClient.
	HTTPClient *http.Client
	// Logger gets the client's log; nil discards it.
	Logger *slog.Logger
	// NoReconnect makes the client stop where it would back off (tests): Dial returns the first attempt's error, and
	// a connection that drops later is reported as StateStopped.
	NoReconnect bool
}

func (o *Options) validate() error {
	u, err := url.Parse(o.URL)
	if err != nil {
		return fmt.Errorf("signal: options: URL: %w", err)
	}
	switch u.Scheme {
	case "ws", "wss", "http", "https": // coder/websocket reads http and https as ws and wss
	default:
		return fmt.Errorf("signal: options: URL scheme %q, want ws or wss", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("signal: options: URL has no host")
	}
	return nil
}

// State is a state of the signaling client (01 §10.1).
type State uint8

const (
	// StateConnecting: an upgrade request is out (after Dial, or after a backoff wait).
	StateConnecting State = iota + 1
	// StateHandshaking: the socket is open and hello is sent; the client waits for welcome.
	StateHandshaking
	// StateReady: welcome arrived. Only now do Request and Send reach the server.
	StateReady
	// StateBackoff: the connection is down and the client waits before the next attempt.
	StateBackoff
	// StateStopped: the client is done, by Close or because reconnecting can't help (01 §10.1). It is final.
	StateStopped
)

// String returns the state's name as 01 §10.1 has it.
func (s State) String() string {
	switch s {
	case StateConnecting:
		return "connecting"
	case StateHandshaking:
		return "handshaking"
	case StateReady:
		return "ready"
	case StateBackoff:
		return "backoff"
	case StateStopped:
		return "stopped"
	}
	return "state(" + strconv.Itoa(int(s)) + ")"
}

// StateChange is one step of the state machine, as States reports it.
type StateChange struct {
	State State
	// Resumed is welcome.resumed (StateReady). False means a new connection: the first one, or the one that
	// replaces a connection the server no longer had; the caller then starts over (01 §10.5).
	Resumed bool
	// Err says why the connection went down, when the server said so (StateBackoff, StateStopped): its error in
	// scope connection or session, the error that answered hello, or the error its close code stands for (01 §12.2).
	// Nil for a plain drop, a timeout of the client's own, and Close.
	Err *protocol.Error
	// Delay is the wait before the next attempt (StateBackoff).
	Delay time.Duration
}

const (
	eventBuffer = 256 // notifications Events holds before the client stops reading its socket
	stateBuffer = 64  // changes States holds; the oldest are dropped when nobody receives
)

// Client is one signaling connection that reconnects by itself (01 §10.1–§10.3). Its methods are safe for
// concurrent use. Close it when done: until then it holds a goroutine, and its socket while it has one.
type Client struct {
	o   Options
	log *slog.Logger

	// base is Dial's context without its cancellation: socket reads and writes run under it. life ends with Close:
	// it bounds the upgrade requests and token calls.
	base   context.Context
	life   context.Context
	cancel context.CancelFunc

	events chan protocol.Envelope
	states chan StateChange
	first  chan struct{} // closed with the first welcome
	done   chan struct{} // closed when run has ended: the client is stopped and both channels are closed

	closing   chan struct{} // closed by Close
	closeOnce sync.Once
	retry     chan struct{} // one slot: RetryNow, during a backoff wait
	probe     chan struct{} // one slot: Probe, while ready
	ids       atomic.Uint64

	mu       sync.Mutex
	state    State
	sock     *socket // the socket of the current attempt: it may make the client ready, or the client is ready on it
	welcome  protocol.Welcome
	welcomed bool                  // welcome is set: its resume token goes into the next hello
	pending  map[string]chan reply // requests waiting for their reply, by id
	limited  bool                  // the current backoff is a rate-limit wait, which RetryNow doesn't shorten
	failure  error                 // why the last attempt failed, for Dial's error
	closeErr error                 // the failed close handshake of Close

	// attempt is n of 01 §10.2: the number of delays taken since the connection was last ready for BackoffReset.
	// Owned by run.
	attempt int
}

// Dial connects, sends hello, and returns after the first welcome; afterwards the client reconnects in the
// background until Close.
//
// A first attempt that fails is retried with the backoff of 01 §10.2 like any later one, until ctx ends: Dial then
// stops the client and returns ctx's error together with the last attempt's. When the server answers with an error
// that reconnecting can't fix (unauthenticated, protocol_unsupported, too_many_connections, …), Dial returns it at
// once as a *protocol.Error. With Options.NoReconnect the first failure of any kind is returned.
//
// ctx bounds Dial only: the client keeps ctx's values and outlives its cancellation.
func Dial(ctx context.Context, o Options) (*Client, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("signal: dial: %w", context.Cause(ctx))
	}
	c := newClient(ctx, o)
	go c.run()
	select {
	case <-c.first:
		return c, nil
	case <-c.done:
		select {
		case <-c.first: // welcomed and stopped right away (a revocation): States tells
			return c, nil
		default:
		}
		err := c.lastFailure()
		if err == nil {
			err = errLost
		}
		return nil, fmt.Errorf("signal: dial: %w", err)
	case <-ctx.Done():
		_ = c.Close()
		if err := c.lastFailure(); err != nil {
			return nil, fmt.Errorf("signal: dial: %w (last attempt: %w)", context.Cause(ctx), err)
		}
		return nil, fmt.Errorf("signal: dial: %w", context.Cause(ctx))
	}
}

func newClient(ctx context.Context, o Options) *Client {
	// The options are read at every hello: later changes by the caller to what it passed don't reach the client.
	o.Header = o.Header.Clone()
	o.Features = slices.Clone(o.Features)
	o.Caps.Decode = slices.Clone(o.Caps.Decode)
	o.Caps.Encode = slices.Clone(o.Caps.Encode)
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	c := &Client{
		o:       o,
		log:     log,
		base:    context.WithoutCancel(ctx),
		events:  make(chan protocol.Envelope, eventBuffer),
		states:  make(chan StateChange, stateBuffer),
		first:   make(chan struct{}),
		done:    make(chan struct{}),
		closing: make(chan struct{}),
		retry:   make(chan struct{}, 1),
		probe:   make(chan struct{}, 1),
		pending: make(map[string]chan reply),
	}
	c.life, c.cancel = context.WithCancel(c.base)
	return c
}

// lastFailure returns why the last failed attempt failed, or nil when none has.
func (c *Client) lastFailure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failure
}

// Welcome returns the last welcome: the connection id, the user, the limits, the features that are active, and the
// room the server resumed the connection into. It is kept through drops and after the client has stopped, until the
// next welcome replaces it. Its slices are shared: don't modify them.
func (c *Client) Welcome() protocol.Welcome {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.welcome
}

// Request sends request t with payload data and waits for its reply. The payload of the ok is decoded into result,
// a pointer to a zero value of t's reply type (protocol.Registry has it; nil drops the payload). The client sends
// hello itself; every other request goes through here (01 §7).
//
// When the server answers with an error message, the error is that *protocol.Error. Otherwise it wraps
// ErrConnectionLost (the client is not ready, or its socket went away before the reply), ErrRequestTimeout (no
// reply within RequestTimeout), ErrNotReady (the client is stopped) or ctx's error; ctx ends the wait, never the
// socket. Request never retries: after ErrConnectionLost and ErrRequestTimeout the caller can't tell whether the
// server acted, and applies 01 §10.5 after the next welcome.
func (c *Client) Request(ctx context.Context, t protocol.MessageType, data, result any) error {
	if t == protocol.MessageTypeHello {
		return errors.New("signal: request: the client sends hello itself")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("signal: request %s: %w", t, context.Cause(ctx))
	}
	id := strconv.FormatUint(c.ids.Add(1), 10)
	b, err := protocol.Marshal(t, id, "", data)
	if err != nil {
		return fmt.Errorf("signal: request: %w", err)
	}
	ch := make(chan reply, 1)
	c.mu.Lock()
	s, err := c.readyLocked()
	if err == nil {
		c.pending[id] = ch
	}
	c.mu.Unlock()
	if err != nil {
		return fmt.Errorf("signal: request %s: %w", t, err)
	}
	timeout := time.NewTimer(RequestTimeout)
	defer timeout.Stop()
	if err := s.write(b, writeTimeout); err != nil {
		c.forget(id)
		return fmt.Errorf("signal: request %s: %w: %w", t, ErrConnectionLost, err)
	}
	c.log.Debug("request", slog.String("type", string(t)), slog.String("id", id))
	select {
	case r := <-ch:
		if r.err != nil {
			var pe *protocol.Error
			if errors.As(r.err, &pe) {
				return pe
			}
			return fmt.Errorf("signal: request %s: %w", t, r.err)
		}
		if result == nil {
			return nil
		}
		if err := protocol.DecodeInto(r.env, result); err != nil {
			return fmt.Errorf("signal: request %s: decode the reply: %w", t, err)
		}
		return nil
	case <-timeout.C:
		c.forget(id)
		return fmt.Errorf("signal: request %s: %w", t, ErrRequestTimeout)
	case <-ctx.Done():
		c.forget(id)
		return fmt.Errorf("signal: request %s: %w", t, context.Cause(ctx))
	}
}

// Send sends notification t with payload data: no id, no reply (01 §7). The error wraps ErrConnectionLost while the
// client is not ready (nothing is queued for later) and ErrNotReady once it is stopped.
func (c *Client) Send(t protocol.MessageType, data any) error {
	if t == protocol.MessageTypeHello {
		return errors.New("signal: send: the client sends hello itself")
	}
	b, err := protocol.Marshal(t, "", "", data)
	if err != nil {
		return fmt.Errorf("signal: send: %w", err)
	}
	c.mu.Lock()
	s, err := c.readyLocked()
	c.mu.Unlock()
	if err != nil {
		return fmt.Errorf("signal: send %s: %w", t, err)
	}
	if err := s.write(b, writeTimeout); err != nil {
		return fmt.Errorf("signal: send %s: %w: %w", t, ErrConnectionLost, err)
	}
	c.log.Debug("send", slog.String("type", string(t)))
	return nil
}

// Events returns the server's notifications, in the order they arrived: room.state, room.event, pc.*,
// subscribe.status, quality.hint, stats, invalidate, server.shutdown, agent.recv, and the error messages that answer
// no request (scopes share, pc and room, and scopes this build doesn't know). Decode them with protocol.Decode.
// Replies never come here, nor do the errors in scope connection or session, which States reports, nor message
// types this build doesn't know (01 §8.13).
//
// The channel must be received from: see the package documentation. It is closed once the client has stopped.
func (c *Client) Events() <-chan protocol.Envelope { return c.events }

// States returns the client's state changes in order, from the first connecting on: when Dial returns, the changes
// up to the first ready are waiting in it. It never blocks the client: it holds 64 changes, and when nobody
// receives, the oldest are dropped. It is closed after StateStopped.
func (c *Client) States() <-chan StateChange { return c.states }

// Probe sends a ping at once, with the 3 s ProbeTimeout: without a pong the client drops the socket and reconnects
// (01 §3.4). A caller whose PeerConnection became disconnected calls it to find out quickly whether signaling is
// down too. It does nothing unless the client is ready.
func (c *Client) Probe() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == StateReady {
		select {
		case c.probe <- struct{}{}:
		default:
		}
	}
}

// RetryNow skips the current backoff wait once (01 §10.2): for a caller that knows the network is back, or a user
// who asks. A rate-limit wait is not shortened. It does nothing unless the client is backing off.
func (c *Client) RetryNow() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == StateBackoff && !c.limited {
		select {
		case c.retry <- struct{}{}:
		default:
		}
	}
}

// Close closes the socket with 1000, so the server skips the grace period and ends the connection at once
// (01 §4.2): its shares stop and its participant leaves. Requests that wait for a reply fail with ErrConnectionLost.
// Close returns when the client's goroutines have ended and Events and States are closed; later calls return the
// same result. Without a socket (connecting, backing off, stopped) there is nothing to tell the server, and Close
// returns at once.
//
// The close handshake has coder/websocket's bounds: 5 s to send the close frame and 5 s for the server's answer.
// The error is non-nil when no answer came, which is what a network that has gone away looks like; the server then
// keeps the connection for its grace period. A nil error is not a proof of the opposite.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.closing)
		c.cancel()
	})
	<-c.done
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeErr
}

// readyLocked returns the socket to write to, or why there is none (01 §10.1, §12.4). c.mu is held.
func (c *Client) readyLocked() (*socket, error) {
	switch c.state {
	case StateReady:
		return c.sock, nil
	case StateStopped:
		return nil, ErrNotReady
	}
	return nil, ErrConnectionLost
}

// reply is the answer to a request: the ok message, or the error that ends the wait.
type reply struct {
	env protocol.Envelope
	err error
}

// resolve hands r to the request with this id. A reply to a request that has gone (timed out, canceled) is dropped.
func (c *Client) resolve(id string, r reply) {
	c.mu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ch != nil {
		ch <- r // one slot, and the request was just taken out: nothing else sends
	}
}

func (c *Client) forget(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// setLocked moves the client to sc.State and reports the change. It never blocks: when States is full the oldest
// change is dropped. c.mu is held, which also puts the changes in order.
func (c *Client) setLocked(sc StateChange) {
	c.state = sc.State
	attrs := []any{slog.String("state", sc.State.String())}
	switch {
	case sc.State == StateReady:
		attrs = append(attrs, slog.String("conn_id", c.welcome.ConnectionID), slog.Bool("resumed", sc.Resumed))
	case sc.Err != nil:
		attrs = append(attrs, slog.String("code", string(sc.Err.Code)))
	}
	if sc.State == StateBackoff {
		attrs = append(attrs, slog.Duration("delay", sc.Delay))
	}
	c.log.Debug("signaling state", attrs...)
	for {
		select {
		case c.states <- sc:
			return
		default:
		}
		select {
		case <-c.states:
		default:
		}
	}
}

func (c *Client) set(sc StateChange) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setLocked(sc)
}
