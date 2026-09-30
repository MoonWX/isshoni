package sfutest

import (
	"encoding/binary"
	"net"
	"sync"
	"sync/atomic"
)

// Direction is the way a datagram goes through a FaultConn.
type Direction uint8

// The directions.
const (
	Outgoing Direction = iota + 1 // WriteTo
	Incoming                      // ReadFrom
)

// FaultConn wraps a UDP socket and loses datagrams when a test says so: the SFU's socket, injected through 04's
// netx.TransportOptions.PacketConns, or a Go client's, through webrtc.NewICEUDPMux. Lost datagrams vanish like on a
// bad network: WriteTo still reports them as written, and ReadFrom waits for the next datagram.
type FaultConn struct {
	net.PacketConn

	mu      sync.Mutex // guards drop and blocked
	drop    func(dir Direction, peer net.Addr, b []byte) bool
	blocked map[string]bool // peer address → black-holed both ways
	dropped atomic.Int64
}

// NewFaultConn wraps pc. Nothing is lost until SetDrop or BlackHole.
func NewFaultConn(pc net.PacketConn) *FaultConn {
	return &FaultConn{PacketConn: pc, blocked: map[string]bool{}}
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

func (c *FaultConn) lose(dir Direction, peer net.Addr, b []byte) bool {
	c.mu.Lock()
	drop, blocked := c.drop, len(c.blocked) > 0 && peer != nil && c.blocked[peer.String()]
	c.mu.Unlock()
	if blocked || (drop != nil && drop(dir, peer, b)) {
		c.dropped.Add(1)
		return true
	}
	return false
}

// WriteTo implements net.PacketConn.
func (c *FaultConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	if c.lose(Outgoing, addr, b) {
		return len(b), nil
	}
	return c.PacketConn.WriteTo(b, addr)
}

// ReadFrom implements net.PacketConn.
func (c *FaultConn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		n, addr, err := c.PacketConn.ReadFrom(b)
		if err != nil || !c.lose(Incoming, addr, b[:n]) {
			return n, addr, err
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
