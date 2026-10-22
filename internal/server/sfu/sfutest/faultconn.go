package sfutest

import (
	"container/heap"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Direction is the way a datagram goes through a FaultConn.
type Direction uint8

// The directions.
const (
	Outgoing Direction = iota + 1 // WriteTo
	Incoming                      // ReadFrom
)

// maxDatagram is the read buffer of a FaultConn's reader goroutine: the largest UDP payload.
const maxDatagram = 65535

// FaultConn wraps a UDP socket and loses datagrams when a test says so: the SFU's socket, injected through 04's
// netx.TransportOptions.PacketConns, or a Go client's, through webrtc.NewICEUDPMux. Lost datagrams vanish like on a
// bad network: WriteTo still reports them as written, and ReadFrom waits for the next datagram. A Link in front of a
// peer gives the datagrams sent to it a capacity (SetLink, a policer), and one behind a peer the datagrams that come
// from it (SetInboundLink, a policer or a queueing Link).
//
// From the first queueing Link on, FaultConn reads the socket on a goroutine of its own until Close, and ReadFrom
// returns each datagram at its departure time from the Link it came through, or at once when it came through none.
// Read deadlines are then kept by FaultConn itself.
type FaultConn struct {
	net.PacketConn

	mu      sync.Mutex // guards the fields below
	drop    func(dir Direction, peer net.Addr, b []byte) bool
	blocked map[string]bool  // peer address → black-holed both ways
	links   map[string]*Link // peer address → the bottleneck of what is sent to it (a policer)
	inbound map[string]*Link // peer address → the bottleneck of what comes from it (either kind)
	// rd is the reader goroutine and its delay line, nil until the first queueing Link. deadline is the read deadline
	// FaultConn keeps while rd runs.
	rd       *delayReader
	deadline time.Time
	dropped  atomic.Int64
}

// NewFaultConn wraps pc. Nothing is lost until SetDrop, BlackHole, SetLink or SetInboundLink.
func NewFaultConn(pc net.PacketConn) *FaultConn {
	return &FaultConn{
		PacketConn: pc, blocked: map[string]bool{}, links: map[string]*Link{}, inbound: map[string]*Link{},
	}
}

// SetLink puts l in front of peer: what is written to peer goes through it, at its capacity (Link). nil removes it.
// l must be a policer (NewLink): SetLink panics with a queueing Link, which works only inbound.
func (c *FaultConn) SetLink(peer net.Addr, l *Link) {
	if l != nil && l.queue {
		panic("sfutest: SetLink takes a policer (NewLink); a queueing Link works only with SetInboundLink")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if l == nil {
		delete(c.links, peer.String())
	} else {
		c.links[peer.String()] = l
	}
}

// SetInboundLink puts l behind peer: what is read from peer goes through it, the uplink twin of SetLink (M2 01
// §10.1). l is either kind: a policer (NewLink) loses the RTP that comes faster than its capacity beyond its bucket;
// a queueing Link (NewQueueLink) delays each datagram to its departure time and loses the RTP whose wait would exceed
// its limit. nil removes it. The first queueing Link starts the reader goroutine.
func (c *FaultConn) SetInboundLink(peer net.Addr, l *Link) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if l == nil {
		delete(c.inbound, peer.String())
		return
	}
	c.inbound[peer.String()] = l
	if l.queue && c.rd == nil {
		c.startReaderLocked()
	}
}

// SetDrop installs a filter that loses every datagram it returns true for (nil removes it). It runs on the socket's
// read and write paths, so it must be quick and must not block. RTPHeader tells RTP from the rest (STUN, DTLS,
// RTCP), which is how a test drops "chosen packets" of one stream.
func (c *FaultConn) SetDrop(f func(dir Direction, peer net.Addr, b []byte) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drop = f
}

// BlackHole loses every datagram to and from peer while on is true.
func (c *FaultConn) BlackHole(peer net.Addr, on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if on {
		c.blocked[peer.String()] = true
	} else {
		delete(c.blocked, peer.String())
	}
}

// Dropped returns the number of datagrams lost so far.
func (c *FaultConn) Dropped() int64 { return c.dropped.Load() }

// lose decides a datagram's fate at now: lost (the filter, a black hole, or a policer's bucket), or through, and then
// the queueing Link it waits in, if any.
func (c *FaultConn) lose(dir Direction, peer net.Addr, b []byte, now time.Time) (lost bool, queue *Link) {
	c.mu.Lock()
	drop, blocked := c.drop, len(c.blocked) > 0 && peer != nil && c.blocked[peer.String()]
	var link *Link
	switch {
	case peer == nil:
	case dir == Outgoing && len(c.links) > 0:
		link = c.links[peer.String()]
	case dir == Incoming && len(c.inbound) > 0:
		link = c.inbound[peer.String()]
	}
	c.mu.Unlock()
	if blocked || (drop != nil && drop(dir, peer, b)) {
		c.dropped.Add(1)
		return true, nil
	}
	if link == nil {
		return false, nil
	}
	if link.queue {
		return false, link
	}
	_, _, _, rtp := RTPHeader(b)
	if !link.pass(len(b), rtp, now) {
		c.dropped.Add(1)
		return true, nil
	}
	return false, nil
}

// WriteTo implements net.PacketConn.
func (c *FaultConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	if lost, _ := c.lose(Outgoing, addr, b, time.Now()); lost {
		return len(b), nil
	}
	return c.PacketConn.WriteTo(b, addr)
}

// ReadFrom implements net.PacketConn. A datagram that comes through a queueing Link is returned at its departure
// time (delayReader).
func (c *FaultConn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		if rd := c.reader(); rd != nil {
			return c.readDelayed(rd, b)
		}
		n, addr, err := c.PacketConn.ReadFrom(b)
		if err != nil {
			return n, addr, err
		}
		now := time.Now()
		if rd := c.reader(); rd != nil {
			// The reader goroutine started while this read waited: the datagram joins its delay line like the others.
			c.admit(rd, b[:n], addr, now)
			continue
		}
		if lost, _ := c.lose(Incoming, addr, b[:n], now); !lost {
			return n, addr, nil
		}
	}
}

// SetReadDeadline implements net.PacketConn. While the reader goroutine runs, FaultConn keeps the deadline itself;
// before, it is the socket's.
func (c *FaultConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	rd := c.rd
	c.mu.Unlock()
	if rd != nil {
		rd.notify()
		return nil
	}
	return c.PacketConn.SetReadDeadline(t)
}

// SetDeadline implements net.PacketConn: the read deadline as SetReadDeadline keeps it, and the socket's write
// deadline.
func (c *FaultConn) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}

// Close implements net.PacketConn: it closes the socket and waits for the reader goroutine, if one runs, to end.
func (c *FaultConn) Close() error {
	err := c.PacketConn.Close()
	if rd := c.reader(); rd != nil {
		rd.wg.Wait()
	}
	return err
}

// ---- the delay line of queueing Links ----

// delayReader is the reader goroutine of a FaultConn with a queueing Link, and the datagrams it read: a heap ordered
// by departure time (a queueing Link's are in FIFO order, the others depart when they come). FaultConn.mu guards its
// fields.
type delayReader struct {
	ready arrivals
	seq   uint64 // the order datagrams came in, for equal departure times
	err   error  // the socket's error that ended the goroutine
	// wake (capacity 1) tells a waiting ReadFrom that a datagram came or the deadline changed; done is closed when the
	// goroutine has ended, which every waiting ReadFrom sees.
	wake chan struct{}
	done chan struct{}
	wg   sync.WaitGroup
}

// arrival is one datagram in a delayReader's heap.
type arrival struct {
	due  time.Time
	seq  uint64
	b    []byte
	addr net.Addr
}

// arrivals is a min-heap of arrivals by departure time, then by the order they came in.
type arrivals []arrival

func (a arrivals) Len() int { return len(a) }
func (a arrivals) Less(i, j int) bool {
	if !a[i].due.Equal(a[j].due) {
		return a[i].due.Before(a[j].due)
	}
	return a[i].seq < a[j].seq
}
func (a arrivals) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a *arrivals) Push(x any)   { *a = append(*a, x.(arrival)) }
func (a *arrivals) Pop() any {
	old := *a
	x := old[len(old)-1]
	*a = old[:len(old)-1]
	return x
}

// notify wakes a ReadFrom that waits for the delay line. It never blocks.
func (rd *delayReader) notify() {
	select {
	case rd.wake <- struct{}{}:
	default:
	}
}

// reader returns the reader goroutine's delay line, nil while none runs.
func (c *FaultConn) reader() *delayReader {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rd
}

// startReaderLocked starts the reader goroutine; c.mu is held. The socket's read deadline is cleared, since FaultConn
// keeps it from now on (a deadline set before carries over).
func (c *FaultConn) startReaderLocked() {
	rd := &delayReader{wake: make(chan struct{}, 1), done: make(chan struct{})}
	c.rd = rd
	_ = c.PacketConn.SetReadDeadline(time.Time{})
	rd.wg.Go(func() { c.readLoop(rd) })
}

// readLoop is the reader goroutine: it reads the socket until it fails (Close) and puts each datagram into the delay
// line. A timeout (a deadline set on the socket by a caller that raced the start) is cleared and passed over.
func (c *FaultConn) readLoop(rd *delayReader) {
	defer close(rd.done)
	buf := make([]byte, maxDatagram)
	for {
		n, addr, err := c.PacketConn.ReadFrom(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				_ = c.PacketConn.SetReadDeadline(time.Time{})
				continue
			}
			c.mu.Lock()
			rd.err = err
			c.mu.Unlock()
			return
		}
		c.admit(rd, buf[:n], addr, time.Now())
	}
}

// admit decides the fate of a datagram read at now and puts what isn't lost into the delay line, with its departure
// time: from its queueing Link, or now.
func (c *FaultConn) admit(rd *delayReader, b []byte, addr net.Addr, now time.Time) {
	lost, queue := c.lose(Incoming, addr, b, now)
	if lost {
		return
	}
	due := now
	if queue != nil {
		_, _, _, rtp := RTPHeader(b)
		var ok bool
		if due, ok = queue.enqueue(len(b), rtp, now); !ok {
			c.dropped.Add(1)
			return
		}
	}
	c.mu.Lock()
	rd.seq++
	heap.Push(&rd.ready, arrival{due: due, seq: rd.seq, b: append([]byte(nil), b...), addr: addr})
	c.mu.Unlock()
	rd.notify()
}

// readDelayed is ReadFrom while the reader goroutine runs: it returns the datagram that departs first once it has
// departed, the socket's error once the goroutine has ended, or a timeout at the read deadline.
func (c *FaultConn) readDelayed(rd *delayReader, b []byte) (int, net.Addr, error) {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		c.mu.Lock()
		now := time.Now()
		if len(rd.ready) > 0 && !rd.ready[0].due.After(now) {
			a := heap.Pop(&rd.ready).(arrival)
			c.mu.Unlock()
			return copy(b, a.b), a.addr, nil
		}
		if rd.err != nil {
			err := rd.err
			c.mu.Unlock()
			return 0, nil, err
		}
		deadline := c.deadline
		if !deadline.IsZero() && !now.Before(deadline) {
			c.mu.Unlock()
			return 0, nil, &net.OpError{
				Op: "read", Net: c.LocalAddr().Network(), Addr: c.LocalAddr(), Err: os.ErrDeadlineExceeded,
			}
		}
		wait := time.Duration(-1)
		if len(rd.ready) > 0 {
			wait = rd.ready[0].due.Sub(now)
		}
		if !deadline.IsZero() && (wait < 0 || deadline.Sub(now) < wait) {
			wait = deadline.Sub(now)
		}
		c.mu.Unlock()
		if wait < 0 {
			select {
			case <-rd.wake:
			case <-rd.done:
			}
			continue
		}
		timer.Reset(wait)
		select {
		case <-rd.wake:
		case <-rd.done:
		case <-timer.C:
		}
	}
}

// RTPHeader reads the clear-text header of an RTP or SRTP datagram: its SSRC, sequence number and payload type. ok
// is false for anything else on the port: STUN, DTLS, RTCP and SRTCP (RFC 7983 and RFC 5761 demultiplexing).
func RTPHeader(b []byte) (ssrc uint32, seq uint16, pt uint8, ok bool) {
	if len(b) < 12 || b[0] < 128 || b[0] > 191 {
		return 0, 0, 0, false
	}
	if t := b[1] & 0x7f; t >= 64 && t <= 95 { // RTCP packet types 192–223
		return 0, 0, 0, false
	}
	return binary.BigEndian.Uint32(b[8:12]), binary.BigEndian.Uint16(b[2:4]), b[1] & 0x7f, true
}
