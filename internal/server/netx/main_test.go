package netx

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestMain fails the run when goroutines outlive the tests. Pion's goroutines get 2 s to settle after Close;
// goleak's own retries stop after about 0.5 s.
func TestMain(m *testing.M) {
	code := m.Run()
	if code == 0 {
		err := goleak.Find()
		for deadline := time.Now().Add(2 * time.Second); err != nil && time.Now().Before(deadline); {
			time.Sleep(100 * time.Millisecond)
			err = goleak.Find()
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "goleak: %v\n", err)
			code = 1
		}
	}
	os.Exit(code)
}

// fakeIfaces is an InterfaceLister with fixed interfaces and route sources.
type fakeIfaces struct {
	ifs    []Interface
	routes map[netip.Addr]netip.Addr // probe destination → source; missing = no route
	err    error
}

func (f *fakeIfaces) Interfaces() ([]Interface, error) { return f.ifs, f.err }

func (f *fakeIfaces) RouteSource(_ context.Context, dst netip.Addr) (netip.Addr, error) {
	if a, ok := f.routes[dst]; ok {
		return a, nil
	}
	return netip.Addr{}, fmt.Errorf("no route to %s", dst)
}

// iface builds an up interface; flags may add net.FlagLoopback. Addresses are parsed; a trailing "~" marks a
// temporary IPv6 address.
func iface(name string, flags net.Flags, addrs ...string) Interface {
	it := Interface{Name: name, Flags: net.FlagUp | flags}
	for _, s := range addrs {
		tmp := false
		if s[len(s)-1] == '~' {
			tmp, s = true, s[:len(s)-1]
		}
		it.Addrs = append(it.Addrs, InterfaceAddr{Addr: netip.MustParseAddr(s), Temporary: tmp})
	}
	return it
}

func loIface() Interface { return iface("lo", net.FlagLoopback, "127.0.0.1", "::1") }

// memPacketConn is an in-memory net.PacketConn with a chosen local address: ReadFrom blocks until Close (or a
// deadline), WriteTo discards and counts. It has no AddrPort methods.
type memPacketConn struct {
	local *net.UDPAddr

	mu       sync.Mutex
	closed   chan struct{}
	deadline time.Time
	wake     chan struct{}
	in       chan memPacket
	written  int
}

type memPacket struct {
	b    []byte
	from net.Addr
}

func newMemPacketConn(local string) *memPacketConn {
	return &memPacketConn{
		local:  net.UDPAddrFromAddrPort(netip.MustParseAddrPort(local)),
		closed: make(chan struct{}),
		wake:   make(chan struct{}, 1),
		in:     make(chan memPacket, 16),
	}
}

func (c *memPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		if n, from, done, err := c.readOnce(b); done {
			return n, from, err
		}
	}
}

// readOnce waits for a packet, Close, the deadline or a deadline change (done false: wait again).
func (c *memPacketConn) readOnce(b []byte) (n int, from net.Addr, done bool, err error) {
	c.mu.Lock()
	d := c.deadline
	c.mu.Unlock()
	var timeout <-chan time.Time
	if !d.IsZero() {
		if !time.Now().Before(d) {
			return 0, nil, true, timeoutError{}
		}
		t := time.NewTimer(time.Until(d))
		defer t.Stop()
		timeout = t.C
	}
	select {
	case p := <-c.in:
		return copy(b, p.b), p.from, true, nil
	case <-c.closed:
		return 0, nil, true, net.ErrClosed
	case <-timeout:
		return 0, nil, true, timeoutError{}
	case <-c.wake:
		return 0, nil, false, nil
	}
}

func (c *memPacketConn) WriteTo(b []byte, _ net.Addr) (int, error) {
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	default:
	}
	c.mu.Lock()
	c.written += len(b)
	c.mu.Unlock()
	return len(b), nil
}

func (c *memPacketConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.closed:
		return net.ErrClosed
	default:
		close(c.closed)
	}
	return nil
}

func (c *memPacketConn) LocalAddr() net.Addr { return c.local }

func (c *memPacketConn) SetDeadline(t time.Time) error { return c.SetReadDeadline(t) }

func (c *memPacketConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return nil
}

func (c *memPacketConn) SetWriteDeadline(time.Time) error { return nil }

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
