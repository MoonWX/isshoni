package sfutest

import (
	"net"
	"testing"
	"time"
)

// rtpDatagram returns an RTP-looking datagram of n bytes (payload type 96).
func rtpDatagram(n int) []byte {
	b := make([]byte, n)
	b[0], b[1] = 0x80, 96
	return b
}

// TestLink: a Link passes RTP up to its buffer (100 ms of its capacity) and then at its capacity, and loses the rest;
// what isn't RTP always passes and takes its share; without a capacity everything passes. It counts what the SFU sent
// through it and what passed.
func TestLink(t *testing.T) {
	l := NewLink(1_000_000) // a buffer of 100 kbit
	t0 := time.Now()
	pkt := rtpDatagram(1250) // 10 kbit
	for i := range 10 {
		if !l.pass(len(pkt), true, t0) {
			t.Fatalf("RTP datagram %d of the buffer's 100 kbit was lost", i)
		}
	}
	if l.pass(len(pkt), true, t0) {
		t.Fatal("an RTP datagram beyond the buffer passed")
	}
	if !l.pass(1000, false, t0) { // 8 kbit of RTCP: in debt now
		t.Fatal("a datagram that isn't RTP was lost")
	}
	// 18 ms later the capacity has brought 18 kbit: 10 kbit after the debt, one datagram.
	t1 := t0.Add(18 * time.Millisecond)
	if !l.pass(len(pkt), true, t1) || l.pass(len(pkt), true, t1) {
		t.Fatal("after 18 ms at 1 Mbit/s, want one more RTP datagram through and the next lost")
	}
	if sent, passed, lost := l.Counts(); sent != 13*1250 || passed != 11*1250 || lost != 2 {
		t.Errorf("Counts = %d sent, %d passed, %d lost; want %d, %d, 2", sent, passed, lost, 13*1250, 11*1250)
	}
	// A narrower link holds less: what the buffer had beyond its new size is gone.
	l.SetCapacity(100_000)
	if l.Capacity() != 100_000 {
		t.Errorf("Capacity = %d", l.Capacity())
	}
	t2 := t1.Add(time.Second) // a full buffer of 10 kbit
	if !l.pass(len(pkt), true, t2) || l.pass(len(pkt), true, t2) {
		t.Fatal("at 100 kbit/s, want one 10 kbit datagram from a full buffer and not two")
	}
	l.SetCapacity(0)
	for range 100 {
		if !l.pass(len(pkt), true, t2) {
			t.Fatal("a Link without a capacity lost a datagram")
		}
	}
}

// TestFaultConnLink: a Link on a FaultConn applies to what is written to its peer, and only to that.
func TestFaultConnLink(t *testing.T) {
	a, err := listenUDP(t)
	if err != nil {
		t.Fatal(err)
	}
	fc := NewFaultConn(a)
	defer fc.Close()
	b, err := listenUDP(t)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	c, err := listenUDP(t)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	link := NewLink(80_000) // a buffer of 8 kbit: one datagram of 1000 bytes
	fc.SetLink(b.LocalAddr(), link)
	count := func(pc net.PacketConn) int {
		n := 0
		buf := make([]byte, 2000)
		for {
			_ = pc.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			if _, _, err := pc.ReadFrom(buf); err != nil {
				return n
			}
			n++
		}
	}
	for range 5 {
		for _, to := range []net.PacketConn{b, c} {
			if _, err := fc.WriteTo(rtpDatagram(1000), to.LocalAddr()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := fc.WriteTo([]byte("not RTP"), b.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	if got, other := count(b), count(c); got != 2 || other != 5 {
		t.Errorf("through the link %d datagrams arrived, beside it %d; want 2 (one RTP, the other one) and 5", got, other)
	}
	// What comes in from the peer doesn't go through it.
	for range 3 {
		if _, err := b.WriteTo(rtpDatagram(1000), fc.LocalAddr()); err != nil {
			t.Fatal(err)
		}
	}
	if got := count(fc); got != 3 {
		t.Errorf("%d of 3 datagrams from the peer arrived", got)
	}
	if fc.Dropped() != 4 {
		t.Errorf("Dropped = %d, want the 4 RTP datagrams the link lost", fc.Dropped())
	}
	fc.SetLink(b.LocalAddr(), nil)
	for range 5 {
		if _, err := fc.WriteTo(rtpDatagram(1000), b.LocalAddr()); err != nil {
			t.Fatal(err)
		}
	}
	if got := count(b); got != 5 {
		t.Errorf("%d of 5 datagrams arrived without the link", got)
	}
}
