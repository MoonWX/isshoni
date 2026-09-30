package signal

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"log/slog"
	mrand "math/rand/v2"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/MoonWX/isshoni/internal/protocol"
)

const (
	writeTimeout = 10 * time.Second // one WebSocket write (01 §3.3)
	pongTimeout  = 10 * time.Second // the wait for the pong of the hub's ping frame (01 §3.4)
	inboxSize    = 1024             // the connection actor's inbox (01 §15.2)
)

// Reconnect spread of server.shutdown (01 §8.12): uniform in [minReconnectMs, maxReconnectMs].
const (
	minReconnectMs = 500
	maxReconnectMs = 3000
)

// errNotImplemented marks the parts of the hub that later slices of README §5 fill in. The client gets
// error{internal} with a ref; the log line says what is missing.
var errNotImplemented = errors.New("signal: not implemented yet")

// idEncoding is lowercase Crockford base32 without padding (README "Naming"): 10 random bytes make 16 characters.
var idEncoding = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

// connIDRawLen is the number of random bytes behind a connection id (80 bits, 01 §4.1).
const connIDRawLen = 10

// newConnID returns a new connection id, "c_" + 16 base32 characters, and its random bytes.
func newConnID() (string, [connIDRawLen]byte) {
	var raw [connIDRawLen]byte
	_, _ = rand.Read(raw[:]) // crypto/rand.Read never fails (it crashes the program instead)
	return connIDFromRaw(raw), raw
}

func connIDFromRaw(raw [connIDRawLen]byte) string { return "c_" + idEncoding.EncodeToString(raw[:]) }

// newRef returns the 8-character reference of an internal error, which is sent to the client and logged (01 §12.1).
func newRef() string {
	var raw [5]byte
	_, _ = rand.Read(raw[:])
	return idEncoding.EncodeToString(raw[:])
}

// internalError returns error{internal} with a fresh ref (01 §12.1) and logs err with that ref, so a user can
// report it. errNotImplemented is logged at WARN, anything else at ERROR.
func (h *Hub) internalError(err error, scope protocol.ErrorScope, attrs ...any) protocol.Error {
	ref := newRef()
	e := protocol.NewError(protocol.ErrorCodeInternal, scope)
	e.Params = map[string]any{"ref": ref}
	level := slog.LevelError
	if errors.Is(err, errNotImplemented) {
		level = slog.LevelWarn
	}
	h.log.Log(h.ctx, level, "internal error", append(attrs, slog.String("ref", ref), slog.Any("err", err))...)
	return e
}

// socket is one accepted WebSocket, from Accept until its reader ends. A connection uses one socket at a time; from
// README S28 on it survives socket reconnects.
//
// Goroutines: the reader (ServeHTTP's goroutine: the hello handshake, then every frame for the connection's actor),
// the writer (drains the send queue, then closes), and, after the handshake, the pinger. The reader waits for the
// other two before ServeHTTP returns.
type socket struct {
	h       *Hub
	ip      netip.Addr
	cookie  bool     // authenticated by cookie at the upgrade
	ident   Identity // the cookie's identity; zero for pre-auth sockets
	tooMany bool     // upgrade step 5 failed: the socket only gets too_many_connections

	ws           *websocket.Conn // set by serve before the writer, the pinger or an actor uses the socket
	q            sendQueue
	lastActivity atomic.Int64         // unix nanoseconds of the last frame received: a message, a ping or a pong
	conn         atomic.Pointer[conn] // the connection this socket serves, once registered

	rawMu  sync.Mutex
	raw    net.Conn // the hijacked connection
	forced bool     // forceClose was called

	dead       chan struct{} // closed when the reader has ended: the writer and the pinger stop
	writerDone chan struct{}
	pingerDone chan struct{} // nil until the pinger starts; set and read by the reader goroutine only

	// Guarded by h.mu.
	preAuth  bool   // counted in h.preAuth
	slotUser string // the user whose per-user slot this socket holds ("" = none)
}

func newSocket(h *Hub, ip netip.Addr, cookie bool, id Identity) *socket {
	s := &socket{h: h, ip: ip, cookie: cookie, ident: id, dead: make(chan struct{}), writerDone: make(chan struct{})}
	s.q.init(h.cfg.Limits.SendQueueMessages, h.cfg.Limits.SendQueueBytes)
	return s
}

// serve runs an accepted socket until it closes. It runs on ServeHTTP's goroutine.
func (s *socket) serve(ws *websocket.Conn, raw net.Conn) {
	s.ws = ws
	s.setRaw(raw)
	ws.SetReadLimit(protocol.MaxMessageBytes) // 64 KiB until welcome (01 §3.3)
	s.touch()
	s.h.wg.Add(1)
	go s.writeLoop()
	if s.tooMany {
		s.fail(protocol.NewError(protocol.ErrorCodeTooManyConnections, protocol.ErrorScopeConnection), "")
	}
	c, err := s.readLoop()
	s.end(c, err)
}

// readLoop reads frames until the socket fails or closes: the first one must be hello within HelloTimeout, the
// others go to the connection's actor in order. Frames that arrive while the hub closes the socket are discarded.
func (s *socket) readLoop() (*conn, error) {
	hello := time.AfterFunc(s.h.cfg.HelloTimeout, func() {
		s.fail(protocol.NewError(protocol.ErrorCodeHelloTimeout, protocol.ErrorScopeConnection), "")
	})
	defer hello.Stop()
	var c *conn
	for {
		typ, b, err := s.ws.Read(s.h.ctx)
		if err != nil {
			return c, err
		}
		s.touch()
		switch {
		case s.q.closing():
			// The hub is closing this socket: discard until the close handshake ends.
		case typ != websocket.MessageText:
			s.closeWith(websocket.StatusUnsupportedData, "") // 01 §3.3; close reasons are codes, never prose
		case c != nil:
			if !c.postWait(func() { c.handleFrame(s, b) }) {
				return c, errActorEnded
			}
		case hello.Stop(): // false: the hello timer fired, and the socket is closing
			c = s.handshake(b)
		}
	}
}

// errActorEnded ends a reader whose connection's actor has ended.
var errActorEnded = errors.New("signal: connection closed")

// end finishes the socket after its reader has ended: it tells the connection, stops the writer and the pinger, and
// releases the WebSocket.
func (s *socket) end(c *conn, err error) {
	code := s.endCode(err)
	if websocket.CloseStatus(err) == -1 {
		// No close frame arrived: the network connection failed, or the read limit was hit (coder/websocket has
		// sent 1009). Release it at once; that also ends a close handshake the writer may be waiting in.
		s.forceClose()
	}
	s.q.finish(0, "") // nothing more is queued
	close(s.dead)
	if c != nil {
		c.postWait(func() { c.socketEnded(s, code) })
	}
	<-s.writerDone
	if s.pingerDone != nil {
		<-s.pingerDone
	}
	_ = s.ws.CloseNow()
	s.h.log.Debug("websocket closed", slog.Int("code", code))
}

// endCode is the socket's close code for logs and metrics: the code the hub closed with, else the peer's, else
// 1009 for a message over the read limit, else 1006 (no close frame).
func (s *socket) endCode(err error) int {
	if code := s.q.closeCode(); code != 0 {
		return int(code)
	}
	if code := websocket.CloseStatus(err); code != -1 {
		return int(code)
	}
	if errors.Is(err, websocket.ErrMessageTooBig) {
		return int(protocol.CloseCodeMessageTooBig)
	}
	return int(protocol.CloseCodeAbnormal)
}

func (s *socket) setRaw(c net.Conn) {
	s.rawMu.Lock()
	defer s.rawMu.Unlock()
	s.raw = c
	if s.forced && c != nil {
		_ = c.Close()
	}
}

// forceClose closes the socket's network connection without a close handshake. It unblocks the reader, the writer
// and the pinger at once.
func (s *socket) forceClose() {
	s.rawMu.Lock()
	defer s.rawMu.Unlock()
	s.forced = true
	if s.raw != nil {
		_ = s.raw.Close()
	}
}

func (s *socket) touch() { s.lastActivity.Store(time.Now().UnixNano()) }

func (s *socket) lastActive() time.Time { return time.Unix(0, s.lastActivity.Load()) }

// writeLoop writes the queued messages, one at a time with writeTimeout each, and closes the WebSocket when the
// queue asks for it.
func (s *socket) writeLoop() {
	defer s.h.wg.Done()
	defer close(s.writerDone)
	for {
		select {
		case <-s.q.wake:
		case <-s.dead:
			return
		}
		for {
			msgs, fin := s.q.take()
			for _, m := range msgs {
				if s.q.aborted() { // slow_connection: nothing more is sent
					break
				}
				if err := s.write(m); err != nil {
					s.h.log.Debug("websocket write", slog.Any("err", err))
					s.forceClose()
					return
				}
			}
			if fin.set {
				if fin.code != 0 {
					if err := s.ws.Close(fin.code, fin.reason); err != nil {
						s.h.log.Debug("websocket close", slog.Any("err", err))
					}
				}
				return
			}
			if len(msgs) == 0 {
				break
			}
		}
	}
}

func (s *socket) write(b []byte) error {
	ctx, cancel := context.WithTimeout(s.h.ctx, writeTimeout)
	defer cancel()
	return s.ws.Write(ctx, websocket.MessageText, b)
}

// startPinger starts the goroutine that sends a WebSocket ping frame every PingInterval (01 §3.4). Browsers answer
// ping frames even in hidden tabs; a missed pong does not close the socket by itself, the idle timeout does.
func (s *socket) startPinger() {
	s.pingerDone = make(chan struct{})
	s.h.wg.Add(1)
	go func() {
		defer s.h.wg.Done()
		defer close(s.pingerDone)
		t := time.NewTicker(s.h.cfg.PingInterval)
		defer t.Stop()
		for {
			select {
			case <-s.dead:
				return
			case <-t.C:
			}
			ctx, cancel := context.WithTimeout(s.h.ctx, pongTimeout)
			err := s.ws.Ping(ctx)
			cancel()
			if err != nil {
				s.h.log.Debug("websocket ping", slog.Any("err", err))
			}
		}
	}()
}

// send encodes and queues one message; re is the id of the request it answers.
func (s *socket) send(t protocol.MessageType, re string, data any) bool {
	b, err := protocol.Marshal(t, "", re, data)
	if err != nil {
		s.h.log.Error("encode message", slog.String("type", string(t)), slog.Any("err", err))
		return false
	}
	return s.sendEncoded(t, b)
}

// sendEncoded queues an encoded message of type t. A full queue closes the socket as slow_connection (01 §3.3).
func (s *socket) sendEncoded(t protocol.MessageType, b []byte) bool {
	switch s.q.push(b) {
	case pushed:
		s.h.metrics.message(t, dirOut)
		s.h.log.Debug("send", slog.String("type", string(t)))
		return true
	case pushFull:
		s.overflow()
	}
	return false
}

// sendError queues error e, the reply to request re when re is set.
func (s *socket) sendError(e protocol.Error, re string) bool {
	if !s.send(protocol.MessageTypeError, re, e) {
		return false
	}
	s.h.metrics.errorSent(e.Code)
	return true
}

// fail sends error e (the reply to request re when re is set) as the socket's last message, then closes the socket
// with the close code that follows e (01 §12.1).
func (s *socket) fail(e protocol.Error, re string) {
	code := protocol.CloseCodeFor(e.Code, e.Scope)
	if code == 0 {
		code = protocol.CloseCodeInternalError
	}
	b, err := protocol.Marshal(protocol.MessageTypeError, "", re, e)
	if err != nil {
		s.h.log.Error("encode error", slog.String("code", string(e.Code)), slog.Any("err", err))
		b = nil
	}
	var msgs [][]byte
	if b != nil {
		msgs = append(msgs, b)
	}
	if s.q.finish(websocket.StatusCode(code), string(e.Code), msgs...) && b != nil {
		s.h.metrics.message(protocol.MessageTypeError, dirOut)
		s.h.metrics.errorSent(e.Code)
		s.h.log.Debug("send", slog.String("type", string(protocol.MessageTypeError)),
			slog.String("code", string(e.Code)))
	}
}

// failInternal logs err with a new ref and fails the socket with error{internal, scope connection} (close 1011).
func (s *socket) failInternal(err error, re string) {
	s.fail(s.h.internalError(err, protocol.ErrorScopeConnection), re)
}

// closeWith closes the socket with code after the queued messages, without an error message.
func (s *socket) closeWith(code websocket.StatusCode, reason string) { s.q.finish(code, reason) }

// shutdown sends server.shutdown{reason, reconnectInMs} and error{server_shutdown} and closes the socket with 1012
// (01 §8.12, 04 §6.4).
func (s *socket) shutdown(reason protocol.ShutdownReason) {
	notice, err := protocol.Marshal(protocol.MessageTypeServerShutdown, "", "", protocol.ServerShutdown{
		Reason:        reason,
		ReconnectInMs: minReconnectMs + mrand.IntN(maxReconnectMs-minReconnectMs+1), //nolint:gosec // G404: jitter, not a secret
	})
	if err != nil {
		s.h.log.Error("encode server.shutdown", slog.Any("err", err))
		s.fail(protocol.NewError(protocol.ErrorCodeServerShutdown, protocol.ErrorScopeConnection), "")
		return
	}
	e := protocol.NewError(protocol.ErrorCodeServerShutdown, protocol.ErrorScopeConnection)
	eb, err := protocol.Marshal(protocol.MessageTypeError, "", "", e)
	if err != nil {
		s.h.log.Error("encode error", slog.Any("err", err))
		return
	}
	if s.q.finish(websocket.StatusCode(protocol.CloseCodeServiceRestart), string(e.Code), notice, eb) {
		s.h.metrics.message(protocol.MessageTypeServerShutdown, dirOut)
		s.h.metrics.message(protocol.MessageTypeError, dirOut)
		s.h.metrics.errorSent(e.Code)
	}
}

// overflow closes the socket as slow_connection: the queued messages are dropped, nothing more is sent, and the
// writer closes with 4503 once its current write returns (01 §3.3).
func (s *socket) overflow() {
	if s.q.abort(websocket.StatusCode(protocol.CloseCodeSlowConnection), string(protocol.ErrorCodeSlowConnection)) {
		s.h.log.Debug("send queue full", slog.Int("limit_messages", s.q.maxMsgs), slog.Int("limit_bytes", s.q.maxBytes))
	}
}

// sendQueue is a socket's bounded send queue (01 §3.3): at most maxMsgs messages and maxBytes bytes. It ends with a
// close request (finish), after which nothing more is queued, or with an abort that drops what is queued.
type sendQueue struct {
	maxMsgs, maxBytes int
	wake              chan struct{} // one slot: the writer has something to do

	mu      sync.Mutex
	msgs    [][]byte
	bytes   int
	fin     closeRequest
	dropped bool // aborted: the queued messages were dropped
}

// closeRequest asks the writer to close the WebSocket with code after the queued messages. code 0 means the socket
// is already gone: the writer just stops.
type closeRequest struct {
	set    bool
	code   websocket.StatusCode
	reason string
}

type pushResult uint8

const (
	pushed   pushResult = iota + 1
	pushGone            // the queue is closing: the message is dropped
	pushFull            // the queue is full: the caller closes the socket as slow_connection
)

func (q *sendQueue) init(maxMsgs, maxBytes int) {
	q.maxMsgs, q.maxBytes = maxMsgs, maxBytes
	q.wake = make(chan struct{}, 1)
}

func (q *sendQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *sendQueue) push(b []byte) pushResult {
	q.mu.Lock()
	switch {
	case q.fin.set:
		q.mu.Unlock()
		return pushGone
	case len(q.msgs)+1 > q.maxMsgs || q.bytes+len(b) > q.maxBytes:
		q.mu.Unlock()
		return pushFull
	}
	q.msgs = append(q.msgs, b)
	q.bytes += len(b)
	q.mu.Unlock()
	q.signal()
	return pushed
}

// finish queues msgs (beyond the limits: they are the last ones) and asks the writer to close with code after
// them. Only the first finish or abort counts; finish reports whether it was the first.
func (q *sendQueue) finish(code websocket.StatusCode, reason string, msgs ...[]byte) bool {
	q.mu.Lock()
	if q.fin.set {
		q.mu.Unlock()
		return false
	}
	for _, m := range msgs {
		q.msgs = append(q.msgs, m)
		q.bytes += len(m)
	}
	q.fin = closeRequest{set: true, code: code, reason: reason}
	q.mu.Unlock()
	q.signal()
	return true
}

// abort drops the queued messages and asks the writer to close with code after the write in progress. It reports
// whether it was the first finish or abort.
func (q *sendQueue) abort(code websocket.StatusCode, reason string) bool {
	q.mu.Lock()
	if q.fin.set {
		q.mu.Unlock()
		return false
	}
	q.msgs, q.bytes = nil, 0
	q.dropped = true
	q.fin = closeRequest{set: true, code: code, reason: reason}
	q.mu.Unlock()
	q.signal()
	return true
}

// take returns every queued message, and the close request once the queue has nothing left before it.
func (q *sendQueue) take() ([][]byte, closeRequest) {
	q.mu.Lock()
	defer q.mu.Unlock()
	msgs := q.msgs
	q.msgs, q.bytes = nil, 0
	return msgs, q.fin
}

func (q *sendQueue) closing() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.fin.set
}

func (q *sendQueue) aborted() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.dropped
}

func (q *sendQueue) closeCode() websocket.StatusCode {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.fin.code
}

// conn is a Connection (01 §4.1): one client instance, from welcome until it closes. Its actor goroutine owns its
// state and handles, in order, the socket's messages, its timers and the hub's control calls (01 §15.2). Other
// goroutines reach it only through post and postWait.
type conn struct {
	h         *Hub
	id        string
	idRaw     [connIDRawLen]byte
	userID    string
	sessionID string
	deviceID  string
	client    protocol.ClientInfo
	role      protocol.Role
	version   int                // the protocol version chosen at hello
	features  []protocol.Feature // active features: hello's list ∩ the server's
	since     time.Time

	inbox chan func()
	done  chan struct{} // closed when the actor has ended

	ident     atomic.Pointer[Identity] // Name and Admin change; the ids never do
	sockP     atomic.Pointer[socket]   // the current socket, for other goroutines (overflow)
	lastStats atomic.Pointer[protocol.ClientStats]

	// Guarded by h.mu.
	slotUser string // the user whose per-user slot the connection holds
	roomID   string // the room the connection is in; "" = none (rooms come with README S19)

	// Owned by the actor.
	sock       *socket // nil while detached (README S28)
	ip         netip.Addr
	caps       protocol.Caps
	limits     connLimits
	statsWatch bool
	tokenHash  [sha256.Size]byte // SHA-256 of the current resume token (01 §10.3)
	idle       *time.Timer
	closed     bool
}

func newConn(h *Hub, s *socket, id Identity, hello *protocol.Hello, version int) *conn {
	cid, raw := newConnID()
	now := time.Now()
	c := &conn{
		h:         h,
		id:        cid,
		idRaw:     raw,
		userID:    id.UserID,
		sessionID: id.SessionID,
		deviceID:  id.DeviceID,
		client:    hello.Client,
		role:      hello.Role,
		version:   version,
		features:  activeFeatures(hello.Features, h.features),
		since:     now,
		inbox:     make(chan func(), inboxSize),
		done:      make(chan struct{}),
		ip:        s.ip,
		caps:      hello.Caps,
		limits:    newConnLimits(h.cfg.Limits, now),
	}
	c.ident.Store(&id)
	return c
}

// activeFeatures is the intersection of the client's features and the server's, in the server's order (01 §6.2).
func activeFeatures(client, server []protocol.Feature) []protocol.Feature {
	out := []protocol.Feature{}
	for _, f := range server {
		if slices.Contains(client, f) {
			out = append(out, f)
		}
	}
	return out
}

func (c *conn) identity() Identity { return *c.ident.Load() }

// run is the actor. first runs before anything else (attach, which sends welcome).
func (c *conn) run(first func()) {
	defer c.h.wg.Done()
	defer close(c.done)
	c.idle = time.NewTimer(c.h.cfg.IdleTimeout)
	defer c.idle.Stop()
	reval := time.NewTicker(c.h.cfg.RevalidateEvery)
	defer reval.Stop()
	first()
	for !c.closed {
		select {
		case f := <-c.inbox:
			f()
		case <-c.idle.C:
			c.checkIdle()
		case <-reval.C:
			c.revalidate()
		}
	}
	c.h.removeConn(c)
}

// post queues f for the actor without blocking. When the inbox is full the connection can't keep up and is closed
// as slow_connection (01 §15.2). post returns false when f was not queued.
func (c *conn) post(f func()) bool {
	select {
	case <-c.done:
		return false
	default:
	}
	select {
	case c.inbox <- f:
		return true
	default:
		if s := c.sockP.Load(); s != nil {
			s.overflow()
		}
		return false
	}
}

// postWait queues f for the actor, waiting while the inbox is full. The socket reader uses it, so a client that
// outpaces its actor is slowed down by TCP backpressure. It returns false once the actor has ended.
func (c *conn) postWait(f func()) bool {
	select {
	case c.inbox <- f:
		return true
	case <-c.done:
		return false
	}
}

// attach makes s the connection's socket and sends welcome, the reply to hello request re (01 §8.2 step 6).
func (c *conn) attach(s *socket, re, defaultRoom string, pol Policy) {
	c.sock = s
	c.sockP.Store(s)
	c.ip = s.ip
	c.idle.Reset(c.h.cfg.IdleTimeout)
	token, hash := mintResumeToken(c.h.cfg.ResumeKey, c.idRaw)
	c.tokenHash = hash
	id := c.identity()
	cfg := &c.h.cfg
	c.send(protocol.MessageTypeWelcome, re, protocol.Welcome{
		Protocol:         c.version,
		ServerVersion:    cfg.ServerVersion,
		MinClientVersion: pol.MinClientVersion,
		Features:         c.features,
		Limits: protocol.Limits{
			MaxMessageBytes:     protocol.MaxMessageBytes,
			MaxSDPBytes:         protocol.MaxSDPBytes,
			MaxSharesPerUser:    cfg.Limits.MaxSharesPerUser,
			MaxRoomParticipants: pol.MaxRoomParticipants,
			MaxRoomShares:       pol.MaxRoomShares,
			MaxVideoBitrate:     pol.MaxVideoBitrate,
			MessagesPerSecond:   cfg.Limits.MessagesPerSecond,
			MessageBurst:        cfg.Limits.MessageBurst,
			PingIntervalMs:      int(cfg.PingInterval.Milliseconds()),
			IdleTimeoutMs:       int(cfg.IdleTimeout.Milliseconds()),
			GraceMs:             int(cfg.Grace.Milliseconds()),
		},
		ICEServers:    cfg.ICEServers,
		ConnectionID:  c.id,
		ResumeToken:   token,
		Resumed:       false,
		DefaultRoomID: defaultRoom,
		User:          protocol.UserInfo{ID: id.UserID, Name: id.Name, Admin: id.Admin},
		ServerTime:    time.Now(),
	})
	c.h.log.Info("connection opened", slog.String("conn_id", c.id), slog.String("user_id", c.userID),
		slog.Bool("resumed", false), slog.String("kind", string(c.client.Kind)), slog.String("role", string(c.role)))
}

// socketEnded handles the end of socket s. The connection closes with its socket; README S28 keeps it detached for
// Config.Grace instead, unless the client closed on purpose (1000, 1001) or the hub closed it for good.
func (c *conn) socketEnded(s *socket, code int) {
	if s != c.sock {
		return // an earlier socket
	}
	c.sock = nil
	c.sockP.Store(nil)
	c.close(code)
}

// close ends the connection after its socket has ended; the actor then exits and the hub forgets it. code is the
// socket's close code. With rooms and shares (README S19, S40) it also leaves the room, ends the connection's
// shares and closes its MediaPeer.
func (c *conn) close(code int) {
	if c.closed {
		return
	}
	c.closed = true
	c.h.metrics.connClosed(c.client.Kind, c.role, code)
	c.h.log.Info("connection closed", slog.String("conn_id", c.id), slog.String("user_id", c.userID),
		slog.Int("code", code))
}

// send queues a message on the current socket; while detached, messages are dropped (resync replaces them).
func (c *conn) send(t protocol.MessageType, re string, data any) bool {
	if c.sock == nil {
		return false
	}
	return c.sock.send(t, re, data)
}

func (c *conn) sendEncoded(t protocol.MessageType, b []byte) bool {
	if c.sock == nil {
		return false
	}
	return c.sock.sendEncoded(t, b)
}

func (c *conn) sendError(e protocol.Error, re string) bool {
	if c.sock == nil {
		return false
	}
	return c.sock.sendError(e, re)
}

// fail sends error e and closes the socket with the matching close code (01 §12.1).
func (c *conn) fail(e protocol.Error) {
	if c.sock != nil {
		c.sock.fail(e, "")
	}
}

// revoke closes the connection for a revocation (01 §3.2): error{session_revoked|account_disabled, scope session}
// and 4401/4403. From README S28 on it also skips grace, and the connection's shares end with left.
func (c *conn) revoke(e protocol.Error) {
	c.h.log.Info("connection revoked", slog.String("conn_id", c.id), slog.String("user_id", c.userID),
		slog.String("code", string(e.Code)))
	c.fail(e)
}

// shutdown sends the shutdown notice and closes the socket with 1012 (04 §6.4). With shares (README S40) it also ends
// them with server_shutdown.
func (c *conn) shutdown(reason protocol.ShutdownReason) {
	if c.sock != nil {
		c.sock.shutdown(reason)
	}
}

// setUser applies a rename or role change. README S19 also refreshes room.state.
func (c *conn) setUser(name string, admin bool) {
	id := c.identity()
	id.Name, id.Admin = name, admin
	c.ident.Store(&id)
}

// checkIdle closes a socket from which no frame (message, ping or pong) arrived for IdleTimeout with
// error{idle_timeout} and 4408 (01 §3.4), and otherwise re-arms the idle timer.
func (c *conn) checkIdle() {
	s := c.sock
	if s == nil {
		return
	}
	idle := time.Since(s.lastActive())
	if idle >= c.h.cfg.IdleTimeout {
		c.fail(protocol.NewError(protocol.ErrorCodeIdleTimeout, protocol.ErrorScopeConnection))
		return
	}
	c.idle.Reset(c.h.cfg.IdleTimeout - idle)
}

// revalidate asks the Authenticator whether the connection's session or device is still valid (01 §3.2). Only
// ErrInvalid closes the connection (session_revoked, 4401); any other error keeps it, and the next tick retries. A
// changed name or admin flag is applied like UpdateUser.
func (c *conn) revalidate() {
	id := c.identity()
	ctx, cancel := context.WithTimeout(c.h.ctx, depTimeout)
	nid, err := c.h.deps.Auth.Revalidate(ctx, id, c.ip)
	cancel()
	switch {
	case errors.Is(err, ErrInvalid):
		c.revoke(protocol.NewError(protocol.ErrorCodeSessionRevoked, protocol.ErrorScopeSession))
	case err != nil:
		c.h.log.Warn("revalidate", slog.String("conn_id", c.id), slog.String("user_id", c.userID),
			slog.Any("err", err))
	case nid.Name != id.Name || nid.Admin != id.Admin:
		c.setUser(nid.Name, nid.Admin)
	}
}
