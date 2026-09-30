package netx

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// This file is the 443 first-byte multiplexer of 04 §7.2 (README S26): one TCP port carries TLS (HTTPS, WSS, ACME
// tls-alpn-01) and RFC 4571 framed ICE-TCP. prefixconn.go holds the per-connection wrapper.

// PortMuxOptions configures ListenPortMux. A zero or negative limit or duration means its default (in the
// comments, 04 §7.8).
type PortMuxOptions struct {
	Addr             string        // ":443"
	ClassifyTimeout  time.Duration // 10 s: time allowed for the first byte
	MaxPending       int           // 1024 connections waiting for their first byte; when full, the oldest is closed
	MaxPendingPerIP  int           // 32 per IPKey
	MaxConnsPerIP    int           // limits.conns_per_ip (256): open TLS + ICE connections per IPKey
	MaxICEConnsPerIP int           // 64 per IPKey; one count across 443 and 7882/tcp (§7.3)
	QueueLen         int           // 128 per sub-listener; a full queue for 1 s drops the connection
	PublicHost       string        // for the plain-HTTP hint response
	Counter          *TransferCounter
	Logger           *slog.Logger

	// queueWait is how long a classified connection waits for room in a full sub-listener queue (1 s). Tests
	// shorten it.
	queueWait time.Duration
}

// Defaults of PortMuxOptions (04 §7.2, §7.8). The ICE default is DefaultMaxICEConnsPerIP.
const (
	defaultClassifyTimeout = 10 * time.Second
	defaultMaxPending      = 1024
	defaultMaxPendingPerIP = 32
	defaultMaxConnsPerIP   = 256
	defaultQueueLen        = 128
	defaultQueueWait       = time.Second
)

// The plain-HTTP hint: the mux answers, then reads and discards what the client sent (at most hintDrainBytes,
// within hintLinger for the whole exchange) before it closes. Closing with unread data would send a TCP reset,
// which can destroy the response before the client reads it.
const (
	hintLinger     = 2 * time.Second
	hintDrainBytes = 64 << 10
)

// PortMux splits one TCP port (443) by the first byte of each connection: TLS (0x16) to TLS(), RFC 4571 framed
// ICE (0x00–0x02) to ICE() (04 §7.2). A request in plain HTTP (a first byte A–Z) gets a 400 response that points
// to https://, and anything else is closed.
//
// The raw listener's accept loop never blocks on classification: every connection gets its own goroutine that
// waits up to ClassifyTimeout for the first byte. Every connection counts against MaxConnsPerIP from accept to
// close, a connection that waits for its first byte also against MaxPendingPerIP and MaxPending, and an ICE
// connection also against MaxICEConnsPerIP, a count shared with Transport's 7882/tcp listener. All per-IP counts
// are keyed by IPKey. Bytes are counted on Counter: TLS as PathWeb, ICE as PathMediaTCP.
type PortMux struct {
	ln       net.Listener // the raw listener
	opts     PortMuxOptions
	log      *slog.Logger
	counter  *TransferCounter
	hintHost string // PublicHost as it goes in a URL; "" = the connection's local address

	tls, ice *subListener
	open     *connLimiter // MaxConnsPerIP: every connection from accept to close
	// iceConns is the per-IPKey ICE-TCP count, shared with Transport's 7882/tcp listener (one count of
	// MaxICEConnsPerIP across both ports, 04 §7.3).
	iceConns *connLimiter

	mu           sync.Mutex
	closed       bool                     // Close has begun: accept nothing more
	live         map[*prefixConn]struct{} // accepted and not closed yet
	pending      list.List                // *prefixConn waiting for its first byte, oldest first
	pendingPerIP map[netip.Prefix]int

	// Outcomes the mux decides itself; TLS and ICE are counted by the sub-listeners when they hand a connection out.
	plainHTTP, garbage, timeout, limited atomic.Uint64

	done      chan struct{} // closed when Close begins
	wg        sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

// PortMuxStats counts accepted-connection outcomes: TLS and ICE count the connections handed out by TLS() and
// ICE(); PlainHTTP the ones answered with the hint; Garbage the ones closed for another first byte, or for none
// (the client closed or reset first); Timeout the ones that sent nothing within ClassifyTimeout; Limited the ones
// closed by a limit (a per-IP count, the pending pool, a full queue) and the 7882/tcp ICE connections the shared
// limit refused (04 §7.3). Connections closed because the mux closed are not counted.
type PortMuxStats struct {
	TLS, ICE, PlainHTTP, Garbage, Timeout, Limited uint64
}

// ListenPortMux binds opts.Addr and starts classifying connections; the caller serves TLS() and ICE() and calls
// Close. A port that can't be bound is a *ListenError (errors.Is(err, syscall.EADDRINUSE) tells a busy port).
func ListenPortMux(opts PortMuxOptions) (*PortMux, error) {
	if opts.Addr == "" {
		return nil, errors.New("netx: 443 multiplexer: no listen address")
	}
	if _, err := hintHostOf(opts.PublicHost); err != nil {
		return nil, err
	}
	var lc net.ListenConfig
	// Binding does not block, and ListenPortMux has no context in the API of 04 §7.2.
	ln, err := lc.Listen(context.Background(), "tcp", opts.Addr)
	if err != nil {
		return nil, &ListenError{Proto: "tcp", Addr: opts.Addr, Err: err}
	}
	return newPortMux(ln, opts), nil
}

// newPortMux runs a PortMux on ln (tests pass a fake listener) and starts its accept loop. opts.PublicHost must be
// valid.
func newPortMux(ln net.Listener, opts PortMuxOptions) *PortMux {
	opts.ClassifyTimeout = orDefault(opts.ClassifyTimeout, defaultClassifyTimeout)
	opts.MaxPending = orDefault(opts.MaxPending, defaultMaxPending)
	opts.MaxPendingPerIP = orDefault(opts.MaxPendingPerIP, defaultMaxPendingPerIP)
	opts.MaxConnsPerIP = orDefault(opts.MaxConnsPerIP, defaultMaxConnsPerIP)
	opts.MaxICEConnsPerIP = orDefault(opts.MaxICEConnsPerIP, DefaultMaxICEConnsPerIP)
	opts.QueueLen = orDefault(opts.QueueLen, defaultQueueLen)
	opts.queueWait = orDefault(opts.queueWait, defaultQueueWait)
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	host, _ := hintHostOf(opts.PublicHost)

	m := &PortMux{
		ln: ln, opts: opts, log: log.With("component", "netx"), counter: opts.Counter, hintHost: host,
		open:         newConnLimiter(opts.MaxConnsPerIP),
		iceConns:     newConnLimiter(opts.MaxICEConnsPerIP),
		live:         map[*prefixConn]struct{}{},
		pendingPerIP: map[netip.Prefix]int{},
		done:         make(chan struct{}),
	}
	m.tls = newSubListener(m, routeTLS, opts.QueueLen)
	m.ice = newSubListener(m, routeICE, opts.QueueLen)
	m.wg.Add(1)
	go m.acceptLoop()
	return m
}

func orDefault[T int | time.Duration](v, def T) T {
	if v <= 0 {
		return def
	}
	return v
}

// TLS returns the listener of connections whose first byte was 0x16; the byte is replayed. It goes to
// http.Server.ServeTLS. Its Close is idempotent and leaves the connections it handed out to their owner (so
// http.Server.Shutdown stays graceful).
func (m *PortMux) TLS() net.Listener { return m.tls }

// ICE returns the listener of connections that start with an RFC 4571 frame. Its Addr() is the *net.TCPAddr of
// :443, and its Close is idempotent (Transport.Close closes it, then PortMux.Close again). Close also closes every
// connection it handed out that is still open; pion's TCPMuxDefault.Close otherwise waits up to
// FirstStunBindTimeout. (Transport wraps it in an iceListener that does this as well.)
func (m *PortMux) ICE() net.Listener { return m.ice }

// Addr returns the raw listener's address.
func (m *PortMux) Addr() net.Addr { return m.ln.Addr() }

// Close closes the raw listener and both sub-listeners (their Accept then returns net.ErrClosed), then every
// connection that came through the mux and is still open: waiting for its first byte, queued, being answered, or
// handed out. It waits for the mux's goroutines. Call it after http.Server.Shutdown (04 §6.4): what is still open
// then is a hijacked connection or one that never finished its handshake. Later calls return the first result.
func (m *PortMux) Close() error {
	m.closeOnce.Do(func() {
		close(m.done)
		err := m.ln.Close()
		m.mu.Lock()
		m.closed = true
		m.mu.Unlock()
		_ = m.tls.Close()
		_ = m.ice.Close()
		m.closeConns(func(*prefixConn) bool { return true })
		m.wg.Wait()
		if err != nil && !errors.Is(err, net.ErrClosed) {
			m.closeErr = fmt.Errorf("netx: close 443 multiplexer: %w", err)
		}
	})
	return m.closeErr
}

// Stats returns the accepted-connection outcomes so far. Limited includes m.iceConns.refused: the 7882/tcp ICE
// connections that the shared ICE limit refused (04 §7.3), so they appear in
// isshoni_portmux_conns_total{result="limited"} whenever the limit is shared. A 443 refusal is counted once, either
// in m.iceConns.refused (the ICE limit) or in the mux's own count (every other limit).
func (m *PortMux) Stats() PortMuxStats {
	return PortMuxStats{
		TLS:       m.tls.handedOut.Load(),
		ICE:       m.ice.handedOut.Load(),
		PlainHTTP: m.plainHTTP.Load(),
		Garbage:   m.garbage.Load(),
		Timeout:   m.timeout.Load(),
		Limited:   m.limited.Load() + m.iceConns.refused.Load(),
	}
}

// iceLimiter returns the per-IPKey ICE connection count that Transport's 7882/tcp listener shares.
func (m *PortMux) iceLimiter() *connLimiter { return m.iceConns }

// acceptLoop accepts raw connections until the listener is closed. A transient error (EMFILE and the like) is
// retried after a pause, as net/http does, and never reaches the sub-listeners: http.Server and pion's
// TCPMuxDefault would stop serving on it.
func (m *PortMux) acceptLoop() {
	defer m.wg.Done()
	var delay time.Duration
	for {
		c, err := m.ln.Accept()
		if err != nil {
			select {
			case <-m.done:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				// Closed under the mux: end both sub-listeners too, so their servers stop instead of waiting.
				m.log.Error("443 listener closed unexpectedly", "err", err)
				_ = m.tls.Close()
				_ = m.ice.Close()
				return
			}
			delay = min(max(2*delay, acceptRetryMin), acceptRetryMax)
			m.log.Warn("443 accept failed, retrying", "err", err, "retry_in", delay)
			t := time.NewTimer(delay)
			select {
			case <-t.C:
			case <-m.done:
				t.Stop()
				return
			}
			continue
		}
		delay = 0
		m.admit(c)
	}
}

// admit applies the per-IP open limit, the per-IP pending limit and the pending pool to a new connection, then
// starts its classifier.
func (m *PortMux) admit(raw net.Conn) {
	key, ok := remoteKey(raw)
	if !ok { // not an IP connection: it can't be counted, so it isn't served
		m.limited.Add(1)
		_ = raw.Close()
		return
	}
	if !m.open.acquire(key) {
		m.limited.Add(1)
		_ = raw.Close()
		m.log.Debug("443 connection over the per-IP connection limit closed", "max", m.opts.MaxConnsPerIP)
		return
	}
	c := &prefixConn{Conn: raw, m: m, key: key, path: PathWeb}

	var evicted *prefixConn
	m.mu.Lock()
	switch {
	case m.closed:
		m.mu.Unlock()
		_ = c.Close()
		return
	case m.pendingPerIP[key] >= m.opts.MaxPendingPerIP:
		m.mu.Unlock()
		m.limited.Add(1)
		_ = c.Close()
		m.log.Debug("443 connection over the per-IP pending limit closed", "max", m.opts.MaxPendingPerIP)
		return
	}
	if m.pending.Len() >= m.opts.MaxPending {
		// Silent sockets push each other out instead of locking everyone out of 443.
		evicted = m.pending.Front().Value.(*prefixConn)
		m.unpend(evicted)
		evicted.dead = true
	}
	c.pending = m.pending.PushBack(c)
	m.pendingPerIP[key]++
	m.live[c] = struct{}{}
	m.wg.Add(1)
	m.mu.Unlock()

	if evicted != nil {
		m.limited.Add(1)
		_ = evicted.Close()
		m.log.Debug("443 pending pool full; the oldest waiting connection closed", "max", m.opts.MaxPending)
	}
	go m.classify(c)
}

// unpend removes c from the pending list. The caller holds m.mu.
func (m *PortMux) unpend(c *prefixConn) {
	if c.pending == nil {
		return
	}
	m.pending.Remove(c.pending)
	c.pending = nil
	if n := m.pendingPerIP[c.key]; n > 1 {
		m.pendingPerIP[c.key] = n - 1
	} else {
		delete(m.pendingPerIP, c.key)
	}
}

// release is prefixConn.Close's hook: it drops c from the mux's lists and gives back its per-IP slots.
func (m *PortMux) release(c *prefixConn) {
	m.mu.Lock()
	c.dead = true
	m.unpend(c)
	delete(m.live, c)
	ice := c.iceSlot
	c.iceSlot = false
	m.mu.Unlock()
	m.open.release(c.key)
	if ice {
		m.iceConns.release(c.key)
	}
}

// closeConns closes every live connection that match selects.
func (m *PortMux) closeConns(match func(*prefixConn) bool) {
	var cs []*prefixConn
	m.mu.Lock()
	for c := range m.live {
		if match(c) {
			c.dead = true
			cs = append(cs, c)
		}
	}
	m.mu.Unlock()
	for _, c := range cs {
		_ = c.Close()
	}
}

// classify waits for c's first byte and routes the connection (the table of 04 §7.2).
func (m *PortMux) classify(c *prefixConn) {
	defer m.wg.Done()
	var b [1]byte
	_ = c.SetReadDeadline(time.Now().Add(m.opts.ClassifyTimeout))
	n, err := io.ReadFull(c.Conn, b[:])
	m.mu.Lock()
	m.unpend(c)
	dead := c.dead
	m.mu.Unlock()
	switch {
	case dead: // evicted from the pending pool (counted there) or the mux is closing
		_ = c.Close()
		return
	case n == 0:
		if errors.Is(err, os.ErrDeadlineExceeded) {
			m.timeout.Add(1)
		} else {
			m.garbage.Add(1)
		}
		_ = c.Close()
		return
	}
	_ = c.SetReadDeadline(time.Time{})

	switch first := b[0]; {
	case first == 0x16: // TLS handshake record
		m.handOut(c, m.tls, first)
	case first <= 0x02: // RFC 4571 length prefix of a first frame of at most 0x0200 bytes (pion's 512-byte buffer)
		m.handOut(c, m.ice, first)
	case 'A' <= first && first <= 'Z': // an HTTP method: plain HTTP on the TLS port
		m.answerPlainHTTP(c)
	default:
		m.garbage.Add(1)
		m.counter.Add(PathWeb, false, 1)
		_ = c.Close()
	}
}

// handOut queues c on sub with its first byte to replay. An ICE connection first takes a slot of the shared ICE
// limit.
func (m *PortMux) handOut(c *prefixConn, sub *subListener, first byte) {
	if sub.route == routeICE {
		c.path = PathMediaTCP
	}
	c.first = first
	c.unread.Store(true)

	m.mu.Lock()
	if c.dead {
		m.mu.Unlock()
		_ = c.Close()
		return
	}
	if sub.route == routeICE {
		if !m.iceConns.acquire(c.key) {
			m.mu.Unlock()
			m.iceConns.refused.Add(1)
			_ = c.Close()
			m.log.Debug("443 ICE-TCP connection over the per-IP limit closed", "max", m.iceConns.max)
			return
		}
		c.iceSlot = true
	}
	c.route = sub.route
	m.mu.Unlock()

	switch sub.put(c, m.opts.queueWait) {
	case putOK:
	case putFull:
		m.limited.Add(1)
		_ = c.Close()
		m.log.Debug("443 sub-listener queue full; connection dropped", "queue", m.opts.QueueLen)
	case putClosed:
		_ = c.Close()
	}
}

// answerPlainHTTP tells a plain-HTTP client to use https:// and closes the connection.
func (m *PortMux) answerPlainHTTP(c *prefixConn) {
	m.plainHTTP.Add(1)
	m.counter.Add(PathWeb, false, 1) // the peeked byte
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(hintLinger))
	if _, err := c.Write(m.hintResponse(c.LocalAddr())); err != nil {
		return
	}
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(c, hintDrainBytes))
}

// hintResponse is the plain-HTTP answer: `This port speaks HTTPS. Use https://<host>/`, where host is PublicHost,
// or else the address the client connected to.
func (m *PortMux) hintResponse(local net.Addr) []byte {
	host := m.hintHost
	if host == "" {
		if ta, ok := local.(*net.TCPAddr); ok {
			host = urlHost(ta.AddrPort())
		}
	}
	body := "This port speaks HTTPS. Use https://" + host + "/\n"
	if host == "" {
		body = "This port speaks HTTPS. Use an https:// address.\n"
	}
	return []byte("HTTP/1.1 400 Bad Request\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n" +
		"Connection: close\r\n" +
		"\r\n" + body)
}

// urlHost formats a local address for an https URL: IPv6 in brackets, the port only when it is not 443.
func urlHost(ap netip.AddrPort) string {
	a := ap.Addr().Unmap().WithZone("")
	if !a.IsValid() || a.IsUnspecified() {
		return ""
	}
	s := a.String()
	if a.Is6() {
		s = "[" + s + "]"
	}
	if ap.Port() != 443 {
		s += ":" + strconv.Itoa(int(ap.Port()))
	}
	return s
}

// hintHostOf checks PublicHost and returns it as it goes in a URL (an IPv6 literal in brackets). It may carry a
// port ("example.com:8443", "[2001:db8::7]:8443").
func hintHostOf(publicHost string) (string, error) {
	if publicHost == "" {
		return "", nil
	}
	if a, err := netip.ParseAddr(publicHost); err == nil {
		a = a.Unmap().WithZone("")
		if a.Is6() {
			return "[" + a.String() + "]", nil
		}
		return a.String(), nil
	}
	if strings.ContainsFunc(publicHost, func(r rune) bool {
		return r <= ' ' || r == 0x7f || strings.ContainsRune(`/\?#@"<>`, r)
	}) {
		return "", fmt.Errorf("netx: 443 multiplexer: PublicHost %q is not a host name or IP address", publicHost)
	}
	return publicHost, nil
}

// putResult is the outcome of subListener.put.
type putResult uint8

const (
	putOK     putResult = iota
	putFull             // the queue stayed full for the whole wait
	putClosed           // the sub-listener is closed
)

// subListener is TLS() or ICE(): a queue of classified connections. Its Accept returns only a connection or
// net.ErrClosed, never a transient error.
type subListener struct {
	m         *PortMux
	route     route
	queue     chan *prefixConn
	handedOut atomic.Uint64

	closed    chan struct{}
	closeOnce sync.Once
	// sendMu orders put against Close: put holds it for reading while it may send, and Close takes it for
	// writing after closing closed (which wakes every waiting put), so nothing is queued after Close drains the
	// queue.
	sendMu sync.RWMutex
	shut   bool // guarded by sendMu
}

func newSubListener(m *PortMux, r route, queueLen int) *subListener {
	return &subListener{m: m, route: r, queue: make(chan *prefixConn, queueLen), closed: make(chan struct{})}
}

// put queues c, waiting up to wait while the queue is full.
func (s *subListener) put(c *prefixConn, wait time.Duration) putResult {
	s.sendMu.RLock()
	defer s.sendMu.RUnlock()
	if s.shut {
		return putClosed
	}
	select {
	case s.queue <- c:
		return putOK
	case <-s.closed:
		return putClosed
	default:
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case s.queue <- c:
		return putOK
	case <-s.closed:
		return putClosed
	case <-t.C:
		return putFull
	}
}

// Accept returns the next classified connection, or net.ErrClosed once the listener is closed.
func (s *subListener) Accept() (net.Conn, error) {
	select {
	case <-s.closed:
		return nil, s.errClosed()
	default:
	}
	select {
	case c := <-s.queue:
		s.handedOut.Add(1)
		return c, nil
	case <-s.closed:
		return nil, s.errClosed()
	}
}

func (s *subListener) errClosed() error {
	return &net.OpError{Op: "accept", Net: "tcp", Addr: s.Addr(), Err: net.ErrClosed}
}

// Addr returns the raw listener's address (a *net.TCPAddr).
func (s *subListener) Addr() net.Addr { return s.m.ln.Addr() }

// Close stops Accept and closes the queued connections; ICE() also closes the connections it handed out that are
// still open. It is idempotent and returns nil.
func (s *subListener) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		s.sendMu.Lock()
		s.shut = true
		s.sendMu.Unlock()
		for drained := false; !drained; {
			select {
			case c := <-s.queue:
				_ = c.Close()
			default:
				drained = true
			}
		}
		if s.route == routeICE {
			s.m.closeConns(func(c *prefixConn) bool { return c.route == routeICE })
		}
	})
	return nil
}

// IPKey is the key of every per-IP count in netx (pending, open and ICE, on 443, 80 and 7882/tcp): an IPv4 address
// (IPv4-mapped IPv6 is unmapped first) as its /32, an IPv6 address as its /64. It gives the same result as 03's
// limiter key (03 §7.3) and 01's pre-auth key (01 §3.1). An invalid address gives the zero Prefix.
func IPKey(a netip.Addr) netip.Prefix {
	a = a.Unmap().WithZone("")
	bits := 64
	if a.Is4() {
		bits = 32
	}
	p, err := a.Prefix(bits)
	if err != nil {
		return netip.Prefix{}
	}
	return p
}
