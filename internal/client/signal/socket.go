package signal

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// readLimit is the largest server message the client reads: above every message a server sends (a sub offer stays
// well under 256 KiB, 01 §13), and a bound on what a broken server can make the client hold.
const readLimit = 1 << 20

// socket is one WebSocket of the client, from its hello until its reader has ended. Its reader goroutine handles
// every message: it routes replies to their requests, makes the client ready on the welcome, and delivers
// notifications to Events. The run goroutine owns the socket's life: it starts the reader, and it always waits for
// it (drop) before it goes on.
type socket struct {
	c       *Client
	ws      *websocket.Conn
	helloID string

	ready chan struct{} // closed by the reader when this socket's welcome made the client ready
	pong  chan struct{} // one slot: a pong arrived
	stop  chan struct{} // closed by halt: the reader stops delivering and ends
	done  chan struct{} // closed when the reader has ended
	once  sync.Once
	// over is set by the reader when the socket itself is over: Read failed, or the server's error ended it. A reader
	// also ends on a socket that lives (halt, a welcome it can't use), so done does not say this.
	over atomic.Bool

	welcomed bool // the welcome arrived; the reader's own

	// Why the reader ended. It writes them before it closes done; run reads them after.
	readErr  error                    // the socket failed or closed: Read's error; errBadWelcome
	srvErr   *protocol.Error          // the server's error that ended the socket (01 §12.2: the error decides)
	helloErr bool                     // srvErr answered hello
	shutdown *protocol.ServerShutdown // the server.shutdown that arrived on this socket
}

func newSocket(c *Client, ws *websocket.Conn, helloID string) *socket {
	return &socket{
		c:       c,
		ws:      ws,
		helloID: helloID,
		ready:   make(chan struct{}),
		pong:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// halt makes the reader stop after the message it is handling. A reader that waits in Read ends when the socket
// closes.
func (s *socket) halt() { s.once.Do(func() { close(s.stop) }) }

// write sends one text frame. A write that fails or times out closes the socket (coder/websocket), which ends the
// reader. The context is the client's base, never a caller's: a canceled write would take the socket with it.
func (s *socket) write(b []byte, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(s.c.base, timeout)
	defer cancel()
	return s.ws.Write(ctx, websocket.MessageText, b)
}

// read is the socket's reader goroutine. It ends when the socket fails or closes, when a server error ends the
// socket, or after halt.
func (s *socket) read() {
	defer close(s.done)
	for {
		typ, b, err := s.ws.Read(s.c.base)
		if err != nil {
			s.readErr = err
			s.over.Store(true)
			return
		}
		if typ != websocket.MessageText {
			continue // the server sends text frames only (01 §3.3)
		}
		env, err := protocol.ParseEnvelope(b)
		if err != nil {
			s.c.log.Warn("dropped a malformed server message", slog.Int("bytes", len(b)))
			continue
		}
		if !s.handle(env) {
			if s.srvErr != nil {
				s.over.Store(true) // the server closes the socket after its error (01 §12.2)
			}
			return
		}
	}
}

// handle handles one server message and reports whether the reader goes on.
func (s *socket) handle(env protocol.Envelope) bool {
	switch env.Type {
	case protocol.MessageTypeWelcome:
		if s.welcomed || env.Re == "" || env.Re != s.helloID {
			return true // not the answer to this socket's hello
		}
		return s.onWelcome(env)
	case protocol.MessageTypeOK:
		if env.Re != "" {
			s.c.resolve(env.Re, reply{env: env})
		}
		return true
	case protocol.MessageTypeError:
		return s.onError(env)
	case protocol.MessageTypePong:
		select {
		case s.pong <- struct{}{}:
		default:
		}
		return true
	case protocol.MessageTypeServerShutdown:
		// The next backoff waits exactly reconnectInMs (01 §10.2). The message is a notification too.
		if sd, err := protocol.Decode[protocol.ServerShutdown](env); err == nil {
			s.shutdown = &sd
		}
	}
	if _, ok := protocol.Lookup(env.Type, protocol.DirServerToClient); !ok {
		s.c.log.Debug("dropped a message of an unknown type", slog.String("type", string(env.Type)))
		return true // from a newer server: ignored (01 §8.13)
	}
	return s.deliver(env)
}

// onWelcome makes the client ready on this socket (01 §8.2), unless run has taken the socket's right to (a
// handshake timeout, Close). A welcome without a connection id or a resume token is a server bug: the attempt fails.
func (s *socket) onWelcome(env protocol.Envelope) bool {
	w, err := protocol.Decode[protocol.Welcome](env)
	if err != nil || w.ConnectionID == "" || w.ResumeToken == "" {
		s.readErr = errBadWelcome
		return false
	}
	c := s.c
	c.mu.Lock()
	if c.sock != s || c.state != StateHandshaking {
		c.mu.Unlock()
		return false
	}
	if !w.Resumed {
		// A new connection: what the old one's socket delivered and nobody received yet belongs to shares and
		// PeerConnections that are gone. Only this reader sends to Events (run waited for the last socket's), so
		// after this the channel holds nothing older than this welcome.
		c.discardEvents()
	}
	c.welcome = w
	if !c.welcomed {
		c.welcomed = true
		close(c.first)
	}
	s.welcomed = true
	c.setLocked(StateChange{State: StateReady, Resumed: w.Resumed})
	c.mu.Unlock()
	close(s.ready)
	return true
}

// onError handles an error message (01 §8.13, §12). The error that answers hello ends the socket whatever its scope.
// Any other goes to the request it answers, if any; in scope connection or session it then ends the socket at once,
// without waiting for the close frame that follows (§12.2), and in the other scopes it is a notification when it
// answers no request.
func (s *socket) onError(env protocol.Envelope) bool {
	e := decodeError(env)
	if !s.welcomed && env.Re != "" && env.Re == s.helloID {
		s.srvErr, s.helloErr = e, true
		return false
	}
	if env.Re != "" {
		s.c.resolve(env.Re, reply{err: e})
	}
	if e.Scope == protocol.ErrorScopeConnection || e.Scope == protocol.ErrorScopeSession {
		s.srvErr = e
		return false
	}
	if env.Re != "" {
		return true
	}
	return s.deliver(env)
}

// deliver hands a notification to Events. It waits while the channel is full, until the caller receives or run
// halts the socket.
func (s *socket) deliver(env protocol.Envelope) bool {
	select {
	case s.c.events <- env:
		return true
	case <-s.stop:
		return false
	}
}

// discardEvents drops the notifications that nobody has received yet. The caller is the only sender.
func (c *Client) discardEvents() {
	for {
		select {
		case <-c.events:
		default:
			return
		}
	}
}
