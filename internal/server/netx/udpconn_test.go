package netx

import (
	"context"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/stun/v4"
)

func listenUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	var lc net.ListenConfig
	pc, err := lc.ListenPacket(context.Background(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	return pc.(*net.UDPConn)
}

func udpAddrPortOf(c net.PacketConn) netip.AddrPort {
	return c.LocalAddr().(*net.UDPAddr).AddrPort()
}

func TestCountingPacketConnCounts(t *testing.T) {
	a, b := listenUDP(t), listenUDP(t)
	var c TransferCounter
	w := newCountingPacketConn(a, &c)
	if _, ok := net.PacketConn(w).(ice.AddrPortReaderWriter); !ok {
		t.Fatal("the counting wrapper does not implement ice.AddrPortReaderWriter")
	}
	if w.ap == nil {
		t.Fatal("a *net.UDPConn must keep its AddrPort methods through the wrapper")
	}
	_ = b.SetReadDeadline(time.Now().Add(5 * time.Second))
	_ = a.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)

	if n, err := w.WriteToAddrPort([]byte("12345"), udpAddrPortOf(b)); n != 5 || err != nil {
		t.Fatalf("WriteToAddrPort = %d, %v", n, err)
	}
	if n, err := w.WriteTo([]byte("123"), b.LocalAddr()); n != 3 || err != nil {
		t.Fatalf("WriteTo = %d, %v", n, err)
	}
	for range 2 {
		if _, _, err := b.ReadFromUDPAddrPort(buf); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.WriteToUDPAddrPort([]byte("abcd"), udpAddrPortOf(a)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.WriteToUDPAddrPort([]byte("ab"), udpAddrPortOf(a)); err != nil {
		t.Fatal(err)
	}
	n, from, err := w.ReadFromAddrPort(buf)
	if err != nil || n != 4 || from != udpAddrPortOf(b) {
		t.Fatalf("ReadFromAddrPort = %d, %v, %v", n, from, err)
	}
	if n, _, err := w.ReadFrom(buf); err != nil || n != 2 {
		t.Fatalf("ReadFrom = %d, %v", n, err)
	}
	if got := c.Totals()[PathMediaUDP]; got.Egress != 8 || got.Ingress != 6 {
		t.Errorf("Totals()[media_udp] = %+v, want {8 6}", got)
	}
}

func TestCountingPacketConnFallback(t *testing.T) {
	m := newMemPacketConn("127.0.0.1:7882")
	defer func() { _ = m.Close() }()
	var c TransferCounter
	w := newCountingPacketConn(m, &c)
	if w.ap != nil {
		t.Fatal("a socket without AddrPort methods must use the fallback")
	}
	if n, err := w.WriteToAddrPort([]byte("abc"), netip.MustParseAddrPort("127.0.0.1:9")); n != 3 || err != nil {
		t.Fatalf("WriteToAddrPort = %d, %v", n, err)
	}
	from := netip.MustParseAddrPort("192.0.2.5:4000")
	m.in <- memPacket{b: []byte("hello"), from: net.UDPAddrFromAddrPort(from)}
	n, got, err := w.ReadFromAddrPort(make([]byte, 16))
	if err != nil || n != 5 || got != from {
		t.Fatalf("ReadFromAddrPort = %d, %v, %v", n, got, err)
	}
	m.in <- memPacket{b: []byte("x"), from: &net.TCPAddr{}}
	if _, _, err := w.ReadFromAddrPort(make([]byte, 16)); err == nil {
		t.Error("a non-UDP source address must be an error")
	}
	if got := c.Totals()[PathMediaUDP]; got.Egress != 3 || got.Ingress != 6 {
		t.Errorf("Totals()[media_udp] = %+v, want {3 6}", got)
	}
}

// addrPortSpy is a socket with its own AddrPort methods that records which read path the UDP mux uses.
type addrPortSpy struct {
	*net.UDPConn
	addrPortReads, plainReads atomic.Int64
}

func (s *addrPortSpy) ReadFromAddrPort(b []byte) (int, netip.AddrPort, error) {
	s.addrPortReads.Add(1)
	return s.ReadFromUDPAddrPort(b)
}

func (s *addrPortSpy) WriteToAddrPort(b []byte, to netip.AddrPort) (int, error) {
	return s.WriteToUDPAddrPort(b, to)
}

func (s *addrPortSpy) ReadFrom(b []byte) (int, net.Addr, error) {
	s.plainReads.Add(1)
	return s.UDPConn.ReadFrom(b)
}

// TestUDPMuxUsesAddrPortPath checks that ice.UDPMuxDefault reads through the wrapper's AddrPort method, i.e. that
// the wrapper does not push pion onto its allocating net.Addr path.
func TestUDPMuxUsesAddrPortPath(t *testing.T) {
	spy := &addrPortSpy{UDPConn: listenUDP(t)}
	var c TransferCounter
	mux := ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: newCountingPacketConn(spy, &c)})
	defer func() { _ = mux.Close() }()

	peer := listenUDP(t)
	msg := stun.MustBuild(stun.TransactionID, stun.BindingRequest, stun.NewUsername("ufrag:peer"))
	if _, err := peer.WriteToUDPAddrPort(msg.Raw, udpAddrPortOf(spy.UDPConn)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for c.Totals()[PathMediaUDP].Ingress == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := c.Totals()[PathMediaUDP].Ingress; got != uint64(len(msg.Raw)) {
		t.Errorf("ingress = %d, want %d", got, len(msg.Raw))
	}
	if spy.addrPortReads.Load() == 0 || spy.plainReads.Load() != 0 {
		t.Errorf("reads: AddrPort %d, ReadFrom %d; want only AddrPort reads",
			spy.addrPortReads.Load(), spy.plainReads.Load())
	}
}
