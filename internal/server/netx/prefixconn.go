package netx

import (
	"container/list"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
)

// route is where PortMux sends a connection after its first byte.
type route uint8

const (
	routeNone route = iota // not classified yet, or answered by the mux itself (plain-HTTP hint, garbage)
	routeTLS
	routeICE
)

// prefixConn is one TCP connection accepted on PortMux's raw listener, for its whole life (04 §7.2). The mux reads
// the first byte from the underlying connection to classify it; once handed out, Read returns that byte first and
// then reads the connection. LocalAddr and RemoteAddr pass through, so pion still gets its *net.TCPAddr.
//
// Bytes read and written through it are counted on its Path (web for TLS, media_tcp for ICE). Close closes the
// connection once and releases what it holds in the mux: its per-IP open slot, its per-IP ICE slot, its place in
// the pending list and in the set of live connections (the "wrapper's Close hook" of 04 §7.2).
type prefixConn struct {
	net.Conn // the accepted TCP connection
	m        *PortMux
	key      netip.Prefix // IPKey of the remote address

	// Written by the classifier goroutine before the connection is handed out or answered; read-only afterwards.
	path   Path
	first  byte
	unread atomic.Bool // first has not been returned by Read yet

	// Guarded by m.mu.
	route   route
	pending *list.Element // the entry in m.pending while the connection waits for its first byte
	dead    bool          // closed, or chosen to be closed, by the mux: the classifier must not hand it out
	iceSlot bool          // holds one of m.iceConns' slots for key

	closeOnce sync.Once
	closeErr  error
}

// Read returns the peeked first byte on its own, then reads the connection.
func (c *prefixConn) Read(b []byte) (int, error) {
	if len(b) > 0 && c.unread.CompareAndSwap(true, false) {
		b[0] = c.first
		c.m.counter.Add(c.path, false, 1)
		return 1, nil
	}
	n, err := c.Conn.Read(b)
	c.m.counter.Add(c.path, false, n)
	return n, err
}

func (c *prefixConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.m.counter.Add(c.path, true, n)
	return n, err
}

// Close closes the connection and releases its slots once; later calls return the first result.
func (c *prefixConn) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = c.Conn.Close()
		c.m.release(c)
	})
	return c.closeErr
}
