package netx

import (
	"net"
	"net/netip"

	"github.com/pion/ice/v4"
)

// countingPacketConn counts a UDP socket's bytes on PathMediaUDP and applies the ICE source guard to what it reads
// (srcguard.go): a STUN packet that the guard refuses is counted and dropped, and the read goes on with the next
// packet, so the UDP mux never sees it. It implements ice.AddrPortReaderWriter, so ice.UDPMuxDefault keeps its
// allocation-free netip.AddrPort path through the wrapper (pion only adapts a bare *net.UDPConn by itself).
// Deadlines, LocalAddr and Close pass through to the socket.
type countingPacketConn struct {
	net.PacketConn
	ap      ice.AddrPortReaderWriter // the socket's own AddrPort methods; nil when it has none
	counter *TransferCounter
	guard   *sourceGuard // nil: every packet passes
}

var _ ice.AddrPortReaderWriter = (*countingPacketConn)(nil)

func newCountingPacketConn(pc net.PacketConn, counter *TransferCounter, guard *sourceGuard) *countingPacketConn {
	c := &countingPacketConn{PacketConn: pc, counter: counter, guard: guard}
	switch s := pc.(type) {
	case *net.UDPConn:
		c.ap = udpAddrPort{s}
	case ice.AddrPortReaderWriter:
		c.ap = s
	}
	return c
}

func (c *countingPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		n, addr, err := c.PacketConn.ReadFrom(b)
		c.counter.Add(PathMediaUDP, false, n)
		if err != nil || c.guard.admitUDP(b[:n], packetSource(addr)) {
			return n, addr, err
		}
	}
}

func (c *countingPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	n, err := c.PacketConn.WriteTo(b, addr)
	c.counter.Add(PathMediaUDP, true, n)
	return n, err
}

// ReadFromAddrPort reads one packet. Without AddrPort methods on the socket it falls back to ReadFrom, whose
// address must then be a *net.UDPAddr.
func (c *countingPacketConn) ReadFromAddrPort(b []byte) (int, netip.AddrPort, error) {
	if c.ap == nil {
		n, addr, err := c.ReadFrom(b)
		if err != nil {
			return n, netip.AddrPort{}, err
		}
		ua, ok := addr.(*net.UDPAddr)
		if !ok {
			return n, netip.AddrPort{}, &net.AddrError{Err: "not a UDP address", Addr: addr.String()}
		}
		return n, ua.AddrPort(), nil
	}
	for {
		n, from, err := c.ap.ReadFromAddrPort(b)
		c.counter.Add(PathMediaUDP, false, n)
		if err != nil || c.guard.admitUDP(b[:n], from) {
			return n, from, err
		}
	}
}

// WriteToAddrPort writes one packet, through WriteTo when the socket has no AddrPort methods.
func (c *countingPacketConn) WriteToAddrPort(b []byte, to netip.AddrPort) (int, error) {
	if c.ap != nil {
		n, err := c.ap.WriteToAddrPort(b, to)
		c.counter.Add(PathMediaUDP, true, n)
		return n, err
	}
	return c.WriteTo(b, net.UDPAddrFromAddrPort(to))
}

// packetSource returns a packet's source address for the guard: the zero AddrPort when it isn't a UDP address,
// which the mux rejects anyway.
func packetSource(a net.Addr) netip.AddrPort {
	if ua, ok := a.(*net.UDPAddr); ok {
		return ua.AddrPort()
	}
	return netip.AddrPort{}
}

// udpAddrPort adapts *net.UDPConn's allocation-free methods to ice.AddrPortReaderWriter.
type udpAddrPort struct{ c *net.UDPConn }

func (u udpAddrPort) ReadFromAddrPort(b []byte) (int, netip.AddrPort, error) {
	return u.c.ReadFromUDPAddrPort(b)
}

func (u udpAddrPort) WriteToAddrPort(b []byte, to netip.AddrPort) (int, error) {
	return u.c.WriteToUDPAddrPort(b, to)
}
