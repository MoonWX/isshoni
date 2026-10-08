package signal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// run is the client's goroutine: the state machine of 01 §10.1. It makes one connection attempt after the other,
// with a backoff wait in between, until Close or until an attempt ends in a way that reconnecting can't fix. It
// owns every socket and waits for each one's reader, so when it ends nothing of the client runs any more.
func (c *Client) run() {
	defer close(c.done)
	defer c.cancel()
	for {
		e := c.connection()
		p := c.plan(e)
		c.leave(p, e)
		if p.stop {
			break
		}
		if c.pause(p) {
			c.leave(plan{change: StateChange{State: StateStopped}, stop: true}, ending{closed: true})
			break
		}
	}
	close(c.events)
	close(c.states)
}

// ending is how one connection attempt ended.
type ending struct {
	closed   bool                     // Close was called
	err      error                    // what went wrong: Dial's error, and the log's
	srvErr   *protocol.Error          // the server's error that ended the socket
	hello    bool                     // srvErr answered hello
	code     websocket.StatusCode     // the server's close code when it closed without an error; 0 without one
	shutdown *protocol.ServerShutdown // the server announced a shutdown on this socket
	stable   bool                     // the connection was ready for BackoffReset
}

// plan is what follows an ending: the state to report, and for a backoff whether its wait is a rate-limit wait.
type plan struct {
	change  StateChange
	stop    bool
	limited bool
}

// connection makes one attempt: the bearer token if there is one, the upgrade, the handshake, and then the
// heartbeat for as long as the socket lives. It returns when the socket is gone and its reader has ended.
func (c *Client) connection() ending {
	c.set(StateChange{State: StateConnecting})
	ctx, cancel := context.WithTimeout(c.life, ConnectTimeout)
	defer cancel()

	hello := protocol.Hello{
		Protocol:    protocol.Version,
		MinProtocol: protocol.MinVersion,
		Features:    c.o.Features,
		Client:      c.o.Client,
		Role:        c.o.Role,
	}
	if c.o.Token != nil {
		tok, err := c.o.Token(ctx)
		if err != nil {
			return c.failed(fmt.Errorf("signal: bearer token: %w", err))
		}
		hello.Auth = &protocol.HelloAuth{Scheme: protocol.AuthSchemeBearer, Token: tok}
	}
	ws, err := c.upgrade(ctx)
	if err != nil {
		return c.failed(err)
	}
	ws.SetReadLimit(readLimit)

	s := newSocket(c, ws, strconv.FormatUint(c.ids.Add(1), 10))
	c.mu.Lock()
	hello.Caps = c.caps // the current ones: a resume replaces the connection's with them (01 §10.3)
	if c.welcomed {
		hello.ResumeToken = c.welcome.ResumeToken // 01 §10.3
	}
	c.sock = s
	c.setLocked(StateChange{State: StateHandshaking})
	c.mu.Unlock()
	go s.read()

	ready, local := c.handshake(s, hello)
	if local != nil && c.revoke() && errors.Is(local, errHandshakeTimeout) {
		ready, local = true, nil // the welcome arrived as the timeout fired
	}
	var readyAt time.Time
	if ready {
		readyAt = time.Now()
		local = c.heartbeat(s)
	}
	closing := errors.Is(local, errClosing)
	closeErr := c.drop(s, closing)

	// What ended the socket, in the order of who decides (01 §12.2): the server's error, then its close code, also
	// when a write of the client failed in the same moment, then the client's own reason.
	e := ending{shutdown: s.shutdown, stable: ready && time.Since(readyAt) >= BackoffReset}
	code := websocket.CloseStatus(s.readErr)
	switch {
	case closing:
		e.closed = true
		c.mu.Lock()
		c.closeErr = closeErr
		c.mu.Unlock()
	case s.srvErr != nil:
		e.srvErr, e.hello, e.err = s.srvErr, s.helloErr, s.srvErr
	case code != -1:
		e.code = code
		e.err = fmt.Errorf("%w: %w", errLost, s.readErr)
	case local != nil:
		e.err = local
	case s.readErr != nil:
		e.err = fmt.Errorf("%w: %w", errLost, s.readErr)
	default:
		e.err = errLost
	}
	return e
}

// failed is the ending of an attempt that got no socket.
func (c *Client) failed(err error) ending {
	select {
	case <-c.closing:
		return ending{closed: true}
	default:
		return ending{err: err}
	}
}

// upgrade opens the WebSocket (01 §3.1). When the server refuses the upgrade, the error names the HTTP status.
func (c *Client) upgrade(ctx context.Context) (*websocket.Conn, error) {
	ws, resp, err := websocket.Dial(ctx, c.o.URL, &websocket.DialOptions{
		HTTPClient: c.o.HTTPClient,
		HTTPHeader: c.o.Header,
	})
	if resp != nil && resp.Body != nil { // nil after a successful Dial; a copy of the start of the body otherwise
		_ = resp.Body.Close()
	}
	switch {
	case err == nil:
		return ws, nil
	case resp != nil:
		return nil, fmt.Errorf("signal: upgrade refused with %d: %w", resp.StatusCode, err)
	}
	return nil, fmt.Errorf("signal: connect: %w", err)
}

// handshake sends hello and waits for the welcome (01 §8.2). ready reports that the reader made the client ready;
// otherwise the reader has ended (local nil), or the client ends the socket for the reason local.
func (c *Client) handshake(s *socket, hello protocol.Hello) (ready bool, local error) {
	b, err := protocol.Marshal(protocol.MessageTypeHello, s.helloID, "", hello)
	if err != nil {
		return false, fmt.Errorf("signal: hello: %w", err)
	}
	timeout := time.NewTimer(HandshakeTimeout)
	defer timeout.Stop()
	if err := s.write(b, HandshakeTimeout); err != nil {
		return false, fmt.Errorf("signal: send hello: %w", err)
	}
	select {
	case <-s.ready:
		return true, nil
	case <-s.done:
		return false, nil
	case <-timeout.C:
		return false, errHandshakeTimeout
	case <-c.closing:
		return false, errClosing
	}
}

// revoke takes from the attempt's socket the right to make the client ready, before run drops it. It reports
// whether the socket has made the client ready already.
func (c *Client) revoke() (wasReady bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == StateReady {
		return true
	}
	c.sock = nil
	return false
}

// heartbeat runs while the client is ready on s (01 §3.4): a ping every limits.pingIntervalMs, and one at once for
// every Probe. A ping without a pong within its timeout ends the socket. It returns nil when the reader has ended,
// else the reason why the client ends the socket.
func (c *Client) heartbeat(s *socket) error {
	c.mu.Lock()
	interval := pingInterval(c.welcome.Limits.PingIntervalMs)
	c.mu.Unlock()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	deadline := time.NewTimer(PongTimeout)
	deadline.Stop()
	defer deadline.Stop()
	var due time.Time // when the pong for the pings out is due; zero when no ping is out

	// ping sends a ping; the pong is due at the earliest deadline of the pings out.
	ping := func(timeout time.Duration) error {
		b, err := protocol.Marshal(protocol.MessageTypePing, "", "", protocol.Ping{T: time.Now().UnixMilli()})
		if err != nil {
			return fmt.Errorf("signal: ping: %w", err)
		}
		if err := s.write(b, timeout); err != nil {
			return fmt.Errorf("signal: send ping: %w", err)
		}
		if d := time.Now().Add(timeout); due.IsZero() || d.Before(due) {
			due = d
			deadline.Reset(timeout)
		}
		return nil
	}
	for {
		var err error
		select {
		case <-s.done:
			return nil
		case <-c.closing:
			return errClosing
		case <-tick.C:
			err = ping(PongTimeout)
		case <-c.probe:
			err = ping(ProbeTimeout)
		case <-s.pong: // a pong answers every ping out
			due = time.Time{}
			deadline.Stop()
		case <-deadline.C:
			select {
			case <-s.pong: // it arrived as the deadline passed
				due = time.Time{}
			default:
				return errPongTimeout
			}
		}
		if err != nil {
			return err
		}
	}
}

// drop ends socket s and waits for its reader. A normal close (Close) sends close code 1000, which makes the server
// end the connection without its grace period (01 §4.2), and returns the handshake's error. Every other drop just
// closes the network connection: the server sees a socket that went away (1006) and keeps the connection for the
// grace period, so the next hello can resume it.
//
// Only a socket that is over has nobody to tell. A reader that has ended says nothing about that: halt itself ends
// one that waits for a caller who receives no more, on a socket that is alive, and its server must hear of the close.
func (c *Client) drop(s *socket, normal bool) error {
	s.halt()
	var err error
	if normal && !s.over.Load() {
		// net.ErrClosed: the server's close frame arrived in this moment and the reader answered it. That is a
		// closed socket, not a close without an answer.
		if cerr := s.ws.Close(websocket.StatusNormalClosure, ""); cerr != nil && !errors.Is(cerr, net.ErrClosed) {
			err = fmt.Errorf("signal: close: %w", cerr)
		}
	}
	_ = s.ws.CloseNow()
	<-s.done
	return err
}

// plan decides what follows an ending (01 §10.1, §10.2, §12.2). The server's error decides, and the close code is
// the fallback: scope session stops; scope connection, and whatever answers hello, stops unless it is retryable.
// Everything else backs off: for max(30 s, retryAfterMs) after a connection-scope rate limit, for exactly
// reconnectInMs after a server.shutdown, and otherwise for the next delay of the backoff sequence.
func (c *Client) plan(e ending) plan {
	if e.closed {
		return plan{change: StateChange{State: StateStopped}, stop: true}
	}
	if e.stable {
		c.attempt = 0
	}
	perr := e.srvErr
	if perr == nil {
		perr = errorForClose(e.code)
	}
	stop := c.o.NoReconnect
	var rateWait time.Duration
	if perr != nil {
		switch {
		case perr.Scope == protocol.ErrorScopeSession:
			stop = true
		case perr.Scope == protocol.ErrorScopeConnection || e.hello:
			stop = stop || !perr.Retryable
		}
		if perr.Code == protocol.ErrorCodeRateLimited && perr.Scope == protocol.ErrorScopeConnection {
			after, _ := serverWait(perr.RetryAfterMs)
			rateWait = max(RateLimitMinWait, after)
		}
	}
	if stop {
		return plan{change: StateChange{State: StateStopped, Err: perr}, stop: true}
	}
	p := plan{change: StateChange{State: StateBackoff, Err: perr}}
	shutdownWait, announced := time.Duration(0), false
	if e.shutdown != nil {
		shutdownWait, announced = serverWait(e.shutdown.ReconnectInMs)
	}
	switch {
	case rateWait > 0:
		p.change.Delay, p.limited = rateWait, true
		c.attempt++
	case announced:
		p.change.Delay = shutdownWait // this delay only; the sequence then goes on from where it was
	default:
		p.change.Delay = backoffDelay(c.attempt, jitter())
		c.attempt++
	}
	return p
}

// leave takes the client out of the attempt that ended with e and into the state of p, backoff or stopped: the
// requests that wait for a reply fail with ErrConnectionLost, and Request and Send fail until the next ready
// (01 §10.1).
func (c *Client) leave(p plan, e ending) {
	cause := e.err
	if p.change.Err != nil {
		cause = p.change.Err // also when a close code stands for it
	}
	c.mu.Lock()
	c.sock = nil
	pending := c.pending
	c.pending = make(map[string]chan reply)
	c.limited = p.limited
	if cause != nil {
		c.failure = cause
	}
	// A Probe or a RetryNow that came too late for the state it was meant for.
	select {
	case <-c.probe:
	default:
	}
	select {
	case <-c.retry:
	default:
	}
	c.setLocked(p.change)
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- reply{err: ErrConnectionLost}
	}
	if cause == nil {
		return
	}
	level := slog.LevelDebug
	switch {
	case e.code == websocket.StatusMessageTooBig, errors.Is(cause, websocket.ErrMessageTooBig),
		errors.Is(cause, errBadWelcome):
		level = slog.LevelWarn // 01 §12.2 "backoff, log": one side sent a message the other can't take
	case p.stop:
		level = slog.LevelInfo // the client gives up: worth a line
	}
	c.log.Log(c.base, level, "signaling connection ended", slog.Any("err", cause),
		slog.String("state", p.change.State.String()))
}

// pause is the backoff wait. It reports whether Close ended it.
func (c *Client) pause(p plan) (closed bool) {
	t := time.NewTimer(p.change.Delay)
	defer t.Stop()
	retry := c.retry
	if p.limited {
		retry = nil // nothing shortens a rate-limit wait (01 §10.2)
	}
	select {
	case <-t.C:
		return false
	case <-retry:
		return false
	case <-c.closing:
		return true
	}
}
