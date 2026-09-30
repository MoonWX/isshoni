package netx

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestConnLimiter(t *testing.T) {
	l := newConnLimiter(0)
	if l.max != DefaultMaxICEConnsPerIP {
		t.Fatalf("max = %d, want the default %d", l.max, DefaultMaxICEConnsPerIP)
	}
	k := IPKey(netip.MustParseAddr("203.0.113.9"))
	other := IPKey(netip.MustParseAddr("203.0.113.10"))
	for i := range DefaultMaxICEConnsPerIP {
		if !l.acquire(k) {
			t.Fatalf("acquire %d refused", i+1)
		}
	}
	if l.acquire(k) {
		t.Fatal("the 65th connection of one key was admitted")
	}
	if !l.acquire(other) {
		t.Fatal("another key must not share the count")
	}
	l.release(k)
	if !l.acquire(k) {
		t.Fatal("a released slot was not reusable")
	}
	for range DefaultMaxICEConnsPerIP {
		l.release(k)
	}
	l.release(other)
	if len(l.counts) != 0 {
		t.Errorf("counts not cleaned up: %v", l.counts)
	}
}

// fakeListener hands out prepared connections, then blocks until Close. errs are returned first, one per Accept.
type fakeListener struct {
	conns  chan net.Conn
	errs   chan error
	closed chan struct{}
	once   sync.Once
}

func newFakeListener() *fakeListener {
	return &fakeListener{conns: make(chan net.Conn, 256), errs: make(chan error, 8), closed: make(chan struct{})}
}

func (l *fakeListener) Accept() (net.Conn, error) {
	select {
	case err := <-l.errs:
		return nil, err
	default:
	}
	select {
	case c := <-l.conns:
		return c, nil
	case err := <-l.errs:
		return nil, err
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *fakeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *fakeListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv6unspecified, Port: 7882} }

// addrConn is one end of a net.Pipe with a chosen remote address.
type addrConn struct {
	net.Conn
	remote net.Addr
}

func (c addrConn) RemoteAddr() net.Addr { return c.remote }

// pipeFrom returns a server-side conn that appears to come from remote, and its client end.
func pipeFrom(t *testing.T, remote string) (server net.Conn, client net.Conn) {
	t.Helper()
	s, c := net.Pipe()
	t.Cleanup(func() { _ = s.Close(); _ = c.Close() })
	return addrConn{Conn: s, remote: net.TCPAddrFromAddrPort(netip.MustParseAddrPort(remote))}, c
}

// TestICEListenerLimitPerSlash64 checks the per-IPKey limit when the connections come from different addresses of
// one IPv6 /64, and that a connection from the neighbouring /64 is not affected.
func TestICEListenerLimitPerSlash64(t *testing.T) {
	fl := newFakeListener()
	lim := newConnLimiter(3)
	l := newICEListener(fl, lim, nil, discardLog())
	defer func() { _ = l.Close() }()

	var clients []net.Conn
	for _, remote := range []string{"[2001:db8:1:2::1]:1000", "[2001:db8:1:2::2]:1000", "[2001:db8:1:2:ffff::9]:1000",
		"[2001:db8:1:2::4]:1000", "[2001:db8:1:3::1]:1000"} {
		s, c := pipeFrom(t, remote)
		fl.conns <- s
		clients = append(clients, c)
	}
	var accepted []net.Conn
	for range 4 { // the 4th connection of 2001:db8:1:2::/64 is skipped, the /64 next door accepted
		c, err := l.Accept()
		if err != nil {
			t.Fatal(err)
		}
		accepted = append(accepted, c)
	}
	if got := lim.refused.Load(); got != 1 {
		t.Errorf("refused = %d, want 1", got)
	}
	// The refused connection was closed: its client end reads EOF.
	_ = clients[3].SetReadDeadline(time.Now().Add(time.Second))
	if _, err := clients[3].Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("the 4th client read %v, want EOF", err)
	}
	key := IPKey(netip.MustParseAddr("2001:db8:1:2::1"))
	if got := lim.open(key); got != 3 {
		t.Errorf("open(%v) = %d, want 3", key, got)
	}
	_ = accepted[0].Close()
	_ = accepted[0].Close() // releases once
	if got := lim.open(key); got != 2 {
		t.Errorf("after Close open(%v) = %d, want 2", key, got)
	}
	for _, c := range accepted[1:] {
		_ = c.Close()
	}
	if len(lim.counts) != 0 {
		t.Errorf("counts after closing everything: %v", lim.counts)
	}
}

// TestICEListenerRefusesUnknownRemote: a connection without an IP remote address can't be keyed, so it is refused.
func TestICEListenerRefusesUnknownRemote(t *testing.T) {
	fl := newFakeListener()
	lim := newConnLimiter(0)
	l := newICEListener(fl, lim, nil, discardLog())
	defer func() { _ = l.Close() }()
	s, c := net.Pipe() // RemoteAddr is "pipe"
	defer func() { _ = c.Close() }()
	s2, c2 := net.Pipe()
	defer func() { _ = c2.Close() }()
	fl.conns <- s
	fl.conns <- addrConn{Conn: s2, remote: &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 5}} // not TCP, but an IP
	got, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if got := lim.refused.Load(); got != 1 {
		t.Errorf("refused = %d, want 1", got)
	}
	if k, ok := remoteKey(got); !ok || k != netip.MustParsePrefix("192.0.2.1/32") {
		t.Errorf("remoteKey = %v, %v", k, ok)
	}
	_ = got.Close()
}

// TestICEListenerNoLimit: with a nil limiter (PortMux's 443 ICE sub-listener, which PortMux limits itself) every
// connection is handed out, even one without an IP remote address, and Close still closes the ones handed out.
func TestICEListenerNoLimit(t *testing.T) {
	fl := newFakeListener()
	l := newICEListener(fl, nil, nil, discardLog())
	var clients []net.Conn
	for range 3 {
		s, c := net.Pipe() // RemoteAddr is "pipe"
		t.Cleanup(func() { _ = s.Close(); _ = c.Close() })
		fl.conns <- s
		clients = append(clients, c)
	}
	for i := range clients {
		if _, err := l.Accept(); err != nil {
			t.Fatalf("Accept %d: %v", i+1, err)
		}
	}
	if got := l.handedOut(); got != len(clients) {
		t.Errorf("handedOut = %d, want %d", got, len(clients))
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	for i, c := range clients {
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("client %d read %v after Close, want EOF", i+1, err)
		}
	}
}

func TestICEListenerRetriesTransientErrors(t *testing.T) {
	fl := newFakeListener()
	l := newICEListener(fl, newConnLimiter(1), nil, discardLog())
	defer func() { _ = l.Close() }()
	fl.errs <- errors.New("accept: too many open files")
	s, _ := pipeFrom(t, "192.0.2.7:1000")
	fl.conns <- s
	c, err := l.Accept()
	if err != nil {
		t.Fatalf("Accept after a transient error = %v, want the next connection", err)
	}
	_ = c.Close()

	// Close during the backoff ends Accept with net.ErrClosed.
	fl.errs <- errors.New("accept: transient")
	done := make(chan error, 1)
	go func() {
		_, err := l.Accept()
		done <- err
	}()
	time.Sleep(time.Millisecond)
	_ = l.Close()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("Accept after Close = %v, want net.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Accept did not return after Close")
	}
	if err := l.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

func TestICEListenerCountsBytes(t *testing.T) {
	var lc net.ListenConfig
	raw, err := lc.Listen(context.Background(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var counter TransferCounter
	l := newICEListener(raw, newConnLimiter(0), &counter, discardLog())
	defer func() { _ = l.Close() }()

	var d net.Dialer
	client, err := d.DialContext(context.Background(), "tcp4", raw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	server, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()
	if _, ok := server.LocalAddr().(*net.TCPAddr); !ok {
		t.Errorf("LocalAddr is %T, want *net.TCPAddr (pion needs it)", server.LocalAddr())
	}
	if _, err := client.Write([]byte("ping!")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(server, make([]byte, 5)); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Write([]byte("pong")); err != nil {
		t.Fatal(err)
	}
	if got := counter.Totals()[PathMediaTCP]; got.Egress != 4 || got.Ingress != 5 {
		t.Errorf("Totals()[media_tcp] = %+v, want {4 5}", got)
	}
}
