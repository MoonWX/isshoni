package netx

import (
	"io"
	"net"
	"sync"
	"testing"
)

func TestTransferCounterTotals(t *testing.T) {
	var c TransferCounter
	c.Add(PathMediaUDP, true, 100)
	c.Add(PathMediaUDP, false, 7)
	c.Add(PathMediaTCP, true, 5)
	c.Add(PathWeb, false, 3)
	c.Add(PathWeb, false, 0)      // ignored
	c.Add(PathWeb, true, -4)      // ignored
	c.Add(Path("other"), true, 9) // ignored

	got := c.Totals()
	want := map[Path]struct{ Egress, Ingress uint64 }{
		PathMediaUDP: {100, 7},
		PathMediaTCP: {5, 0},
		PathWeb:      {0, 3},
	}
	if len(got) != len(want) {
		t.Fatalf("Totals has %d paths, want %d: %v", len(got), len(want), got)
	}
	for p, w := range want {
		if got[p] != w {
			t.Errorf("Totals()[%s] = %+v, want %+v", p, got[p], w)
		}
	}
}

func TestTransferCounterNil(t *testing.T) {
	var c *TransferCounter
	c.Add(PathWeb, true, 10) // must not panic
	got := c.Totals()
	for _, p := range []Path{PathMediaUDP, PathMediaTCP, PathWeb} {
		if v, ok := got[p]; !ok || v.Egress != 0 || v.Ingress != 0 {
			t.Errorf("nil Totals()[%s] = %+v, %v; want a zero entry", p, v, ok)
		}
	}
}

func TestTransferCounterConcurrent(t *testing.T) {
	var c TransferCounter
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 1000 {
				c.Add(PathMediaTCP, true, 2)
				c.Add(PathMediaTCP, false, 1)
			}
		})
	}
	wg.Wait()
	if got := c.Totals()[PathMediaTCP]; got.Egress != 16000 || got.Ingress != 8000 {
		t.Errorf("Totals()[media_tcp] = %+v, want {16000 8000}", got)
	}
}

func TestCountingConn(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = b.Close() }()
	var c TransferCounter
	closed := 0
	cc := newCountingConn(a, &c, PathMediaTCP, func() { closed++ })

	go func() {
		buf := make([]byte, 5)
		_, _ = io.ReadFull(b, buf)
		_, _ = b.Write([]byte("abc"))
	}()
	if n, err := cc.Write([]byte("hello")); n != 5 || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	buf := make([]byte, 3)
	if _, err := io.ReadFull(cc, buf); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := c.Totals()[PathMediaTCP]; got.Egress != 5 || got.Ingress != 3 {
		t.Errorf("Totals()[media_tcp] = %+v, want {5 3}", got)
	}
	if err := cc.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	_ = cc.Close()
	if closed != 1 {
		t.Errorf("onClose ran %d times, want 1", closed)
	}
}
