package sfutest

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"
)

// The uplink bottlenecks of M2 01 §10.1 (01 §14's sfutest row): a queueing Link at fixed instants, and both kinds of
// Link behind a peer of a FaultConn (SetInboundLink). M1's Link tests (link_test.go) are unchanged.

// stunDatagram returns a datagram of n bytes that looks like STUN (first byte 0), which a Link never loses.
func stunDatagram(n int) []byte {
	b := make([]byte, n)
	b[0], b[1] = 0, 1
	return b
}

// TestQueueLink: a queueing Link departs datagrams at its capacity in FIFO order; its wait (Queued) grows by
// size × 8 / capacity per datagram; an RTP datagram whose wait would exceed the limit is lost and takes no place in
// the line, while STUN, DTLS and RTCP are never lost; an empty queue departs a datagram after its own serialization;
// without a capacity everything departs at once. Counts works as for a policer.
func TestQueueLink(t *testing.T) {
	const ms = time.Millisecond
	l := NewQueueLink(800_000, 50*ms) // 1000 bytes take 10 ms
	t0 := time.Now()
	if q := l.queuedAt(t0); q != 0 {
		t.Fatalf("Queued of a new link = %v", q)
	}
	for i := range 5 {
		due, ok := l.enqueue(1000, true, t0)
		if want := t0.Add(time.Duration(i+1) * 10 * ms); !ok || !due.Equal(want) {
			t.Fatalf("RTP datagram %d departs at %v (ok %v), want %v", i, due.Sub(t0), ok, want.Sub(t0))
		}
		if q, want := l.queuedAt(t0), time.Duration(i+1)*10*ms; q != want {
			t.Fatalf("Queued after %d datagrams = %v, want %v", i+1, q, want)
		}
	}
	// The sixth would wait 60 ms, beyond the 50 ms limit: lost, and the line is as it was.
	if _, ok := l.enqueue(1000, true, t0); ok {
		t.Fatal("an RTP datagram that would wait 60 ms passed a link with a 50 ms limit")
	}
	if q := l.queuedAt(t0); q != 50*ms {
		t.Errorf("Queued after a lost datagram = %v, want 50 ms", q)
	}
	// STUN (and DTLS, RTCP) queue too but are never lost.
	due, ok := l.enqueue(1000, false, t0)
	if !ok || !due.Equal(t0.Add(60*ms)) {
		t.Fatalf("a STUN datagram beyond the limit departs at %v (ok %v), want 60 ms", due.Sub(t0), ok)
	}
	// 25 ms later the queue holds 35 ms: a small RTP datagram fits behind it.
	t1 := t0.Add(25 * ms)
	if q := l.queuedAt(t1); q != 35*ms {
		t.Errorf("Queued 25 ms later = %v, want 35 ms", q)
	}
	if due, ok := l.enqueue(500, true, t1); !ok || !due.Equal(t0.Add(65*ms)) {
		t.Errorf("a 500-byte datagram 25 ms later departs at %v (ok %v), want 65 ms", due.Sub(t0), ok)
	}
	// Once the queue has drained, a datagram waits only for its own serialization.
	t2 := t0.Add(time.Second)
	if q := l.queuedAt(t2); q != 0 {
		t.Errorf("Queued of a drained link = %v", q)
	}
	if due, ok := l.enqueue(1000, true, t2); !ok || !due.Equal(t2.Add(10*ms)) {
		t.Errorf("into an empty queue: departs at %v (ok %v), want 10 ms later", due.Sub(t2), ok)
	}
	if sent, passed, lost := l.Counts(); sent != 7500 || passed != 6500 || lost != 1 {
		t.Errorf("Counts = %d sent, %d passed, %d lost; want 7500, 6500, 1", sent, passed, lost)
	}
	// A narrower capacity applies to what comes next; no capacity departs everything at once.
	l.SetCapacity(80_000)
	if l.Capacity() != 80_000 {
		t.Errorf("Capacity = %d", l.Capacity())
	}
	if due, ok := l.enqueue(100, true, t2.Add(time.Second)); !ok || !due.Equal(t2.Add(time.Second+10*ms)) {
		t.Errorf("100 bytes at 80 kbit/s depart after %v (ok %v), want 10 ms", due.Sub(t2.Add(time.Second)), ok)
	}
	l.SetCapacity(0)
	t3 := t2.Add(2 * time.Second)
	for range 100 {
		if due, ok := l.enqueue(1500, true, t3); !ok || !due.Equal(t3) {
			t.Fatalf("a link without a capacity delays to %v (ok %v)", due.Sub(t3), ok)
		}
	}
	if q := NewLink(1_000_000).Queued(); q != 0 {
		t.Errorf("a policer's Queued = %v", q)
	}
}

// TestSetLinkQueuePanics: SetLink keeps M1's policer only.
func TestSetLinkQueuePanics(t *testing.T) {
	a, err := listenUDP(t)
	if err != nil {
		t.Fatal(err)
	}
	fc := NewFaultConn(a)
	defer fc.Close()
	defer func() {
		if recover() == nil {
			t.Error("SetLink took a queueing Link")
		}
	}()
	fc.SetLink(a.LocalAddr(), NewQueueLink(1_000_000, time.Second))
}

// readAll reads from pc until nothing has come for quiet, and returns each datagram's first byte and when it came.
func readAll(t *testing.T, pc net.PacketConn, quiet time.Duration) (firsts []byte, at []time.Time) {
	t.Helper()
	buf := make([]byte, 2000)
	for {
		_ = pc.SetReadDeadline(time.Now().Add(quiet))
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("ReadFrom: %v", err)
			}
			return firsts, at
		}
		if n > 0 {
			firsts, at = append(firsts, buf[0]), append(at, time.Now())
		}
	}
}

// TestFaultConnQueueLink: datagrams from a peer behind a queueing Link come out of ReadFrom at their departure times,
// in the order they were sent, while another peer's pass at once; the RTP that would wait beyond the limit is lost,
// a STUN datagram never; Dropped counts the loss. The read deadline still works, and Close ends the reader
// goroutine (TestMain's goleak check).
func TestFaultConnQueueLink(t *testing.T) {
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

	// A deadline with nothing to read: a timeout, from FaultConn's own deadline once the reader runs.
	link := NewQueueLink(400_000, 150*time.Millisecond) // 1000 bytes take 20 ms
	fc.SetInboundLink(b.LocalAddr(), link)
	if firsts, _ := readAll(t, fc, 50*time.Millisecond); len(firsts) != 0 {
		t.Fatalf("%d datagrams from nowhere", len(firsts))
	}

	// Ten RTP datagrams of 1000 bytes from b at once, numbered in their first byte's low bits (the CSRC count: still
	// RTP of version 2), then three from c, then a STUN datagram from b.
	sent := time.Now()
	for i := range 10 {
		d := rtpDatagram(1000)
		d[0] = 0x80 | byte(i)
		if _, err := b.WriteTo(d, fc.LocalAddr()); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		if _, err := c.WriteTo([]byte{'c', 1, 2, 3}, fc.LocalAddr()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.WriteTo(stunDatagram(1000), fc.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	firsts, at := readAll(t, fc, 300*time.Millisecond)

	// c's three come first, at once; then b's that fit the 150 ms (the first seven: 20 to 140 ms) in their order, the
	// eighth to tenth lost; then the STUN datagram, which waits behind the seven and is never lost.
	want := []byte{'c', 'c', 'c', 0x80, 0x81, 0x82, 0x83, 0x84, 0x85, 0x86, 0}
	if string(firsts) != string(want) {
		t.Fatalf("datagrams came in the order %x, want %x", firsts, want)
	}
	if d := at[2].Sub(sent); d > 15*time.Millisecond {
		t.Errorf("the other peer's datagrams took %v, want at once", d)
	}
	for i := 3; i < 10; i++ {
		// The k-th of b's departs k × 20 ms after it was sent; a datagram never comes before its departure.
		due := time.Duration(i-2) * 20 * time.Millisecond
		if d := at[i].Sub(sent); d < due-2*time.Millisecond || d > due+40*time.Millisecond {
			t.Errorf("b's datagram %d came after %v, want %v", i-3, d, due)
		}
	}
	if d := at[10].Sub(sent); d < 158*time.Millisecond {
		t.Errorf("the STUN datagram came after %v, want 160 ms", d)
	}
	if sent, passed, lost := link.Counts(); sent != 10_000 || passed != 7000 || lost != 3 {
		t.Errorf("Counts = %d sent, %d passed, %d lost; want 10000, 7000, 3", sent, passed, lost)
	}
	if fc.Dropped() != 3 {
		t.Errorf("Dropped = %d, want 3", fc.Dropped())
	}
	if q := link.Queued(); q != 0 {
		t.Errorf("Queued after the queue drained = %v", q)
	}

	// The link removed, b's datagrams pass at once again; the reader goroutine still runs until Close.
	fc.SetInboundLink(b.LocalAddr(), nil)
	sent = time.Now()
	for range 5 {
		if _, err := b.WriteTo(rtpDatagram(1000), fc.LocalAddr()); err != nil {
			t.Fatal(err)
		}
	}
	firsts, at = readAll(t, fc, 100*time.Millisecond)
	if len(firsts) != 5 || at[4].Sub(sent) > 15*time.Millisecond {
		t.Errorf("without the link %d of 5 datagrams came, the last after %v", len(firsts), at[len(at)-1].Sub(sent))
	}
	if err := fc.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fc.ReadFrom(make([]byte, 10)); !errors.Is(err, net.ErrClosed) {
		t.Errorf("ReadFrom after Close = %v, want net.ErrClosed", err)
	}
}

// TestFaultConnInboundPolicer: M1's policer behind a peer loses the RTP from it beyond its bucket, lets what isn't
// RTP through, and leaves the other peers and the outgoing direction alone; it needs no reader goroutine.
func TestFaultConnInboundPolicer(t *testing.T) {
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
	link := NewLink(80_000) // a bucket of 8 kbit: one datagram of 1000 bytes
	fc.SetInboundLink(b.LocalAddr(), link)
	for range 5 {
		for _, from := range []net.PacketConn{b, c} {
			if _, err := from.WriteTo(rtpDatagram(1000), fc.LocalAddr()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := b.WriteTo(stunDatagram(100), fc.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	if firsts, _ := readAll(t, fc, 100*time.Millisecond); len(firsts) != 7 {
		t.Errorf("%d datagrams came, want 7: one RTP and the STUN from b, five from c", len(firsts))
	}
	if _, _, lost := link.Counts(); lost != 4 || fc.Dropped() != 4 {
		t.Errorf("lost %d, Dropped %d; want 4", lost, fc.Dropped())
	}
	if fc.reader() != nil {
		t.Error("a policer started the reader goroutine")
	}
	// What is written to b doesn't go through its inbound link.
	for range 3 {
		if _, err := fc.WriteTo(rtpDatagram(1000), b.LocalAddr()); err != nil {
			t.Fatal(err)
		}
	}
	if firsts, _ := readAll(t, b, 100*time.Millisecond); len(firsts) != 3 {
		t.Errorf("%d of 3 datagrams to the peer arrived", len(firsts))
	}
}
