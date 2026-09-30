package netx

import (
	"net"
	"sync"
	"sync/atomic"
)

// Path is where bytes are counted (04 §11.3). VPS providers bill packets on the wire, so bytes are counted at the
// socket layer, with SRTP, RTCP and STUN overhead, not from RTP payload counters.
type Path string

const (
	PathMediaUDP Path = "media_udp" // the UDP mux sockets (listen.ice_udp)
	PathMediaTCP Path = "media_tcp" // ICE-TCP connections on 7882 and through the 443 multiplexer
	PathWeb      Path = "web"       // HTTPS and HTTP connections
)

// paths lists every Path in a fixed order; the index is the slot in TransferCounter.
var paths = [...]Path{PathMediaUDP, PathMediaTCP, PathWeb}

// TransferCounter keeps process-wide byte totals per Path and direction. It is safe for concurrent use, its zero
// value is ready, and a nil *TransferCounter counts nothing, so options can leave it out. ops.Transfer reads
// Totals every 60 s and stores the deltas per calendar month (04 §11.3).
type TransferCounter struct {
	egress, ingress [len(paths)]atomic.Uint64
}

// Add counts n bytes on path p: egress for bytes the server sent, ingress for bytes it received. An unknown path,
// n ≤ 0 and a nil counter are ignored.
func (c *TransferCounter) Add(p Path, egress bool, n int) {
	if c == nil || n <= 0 {
		return
	}
	i := pathIndex(p)
	if i < 0 {
		return
	}
	if egress {
		c.egress[i].Add(uint64(n))
	} else {
		c.ingress[i].Add(uint64(n))
	}
}

// Totals returns the totals since the counter was created, with an entry for every Path (zero when nothing was
// counted). A nil counter returns the zero entries.
func (c *TransferCounter) Totals() map[Path]struct{ Egress, Ingress uint64 } {
	out := make(map[Path]struct{ Egress, Ingress uint64 }, len(paths))
	for i, p := range paths {
		var t struct{ Egress, Ingress uint64 }
		if c != nil {
			t.Egress, t.Ingress = c.egress[i].Load(), c.ingress[i].Load()
		}
		out[p] = t
	}
	return out
}

func pathIndex(p Path) int {
	for i, q := range paths {
		if q == p {
			return i
		}
	}
	return -1
}

// countingConn counts a stream connection's bytes on one Path and runs onClose once when it is closed. LocalAddr
// and RemoteAddr pass through, so pion still sees a *net.TCPAddr.
type countingConn struct {
	net.Conn
	counter *TransferCounter
	path    Path

	closeOnce sync.Once
	onClose   func()
	closeErr  error
}

func newCountingConn(c net.Conn, counter *TransferCounter, path Path, onClose func()) *countingConn {
	return &countingConn{Conn: c, counter: counter, path: path, onClose: onClose}
}

func (c *countingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.counter.Add(c.path, false, n)
	return n, err
}

func (c *countingConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.counter.Add(c.path, true, n)
	return n, err
}

// Close closes the connection once; later calls return the first result.
func (c *countingConn) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = c.Conn.Close()
		if c.onClose != nil {
			c.onClose()
		}
	})
	return c.closeErr
}
