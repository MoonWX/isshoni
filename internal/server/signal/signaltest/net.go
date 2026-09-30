package signaltest

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

// ClientIPHeader is the request header that ClientIP reads, so a test can give each upgrade its own client address.
const ClientIPHeader = "X-Signaltest-Client-Ip"

// DefaultClientIP is the address ClientIP returns for a request without ClientIPHeader whose RemoteAddr is not an
// IP address (a PipeNet connection). It is from the documentation range 192.0.2.0/24 (RFC 5737).
var DefaultClientIP = netip.MustParseAddr("192.0.2.1")

// ClientIP is a Deps.ClientIP for tests: the address in ClientIPHeader, else RemoteAddr's, else DefaultClientIP.
func ClientIP(r *http.Request) netip.Addr {
	if v := r.Header.Get(ClientIPHeader); v != "" {
		if a, err := netip.ParseAddr(v); err == nil {
			return a
		}
	}
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		return ap.Addr()
	}
	return DefaultClientIP
}

// PipeNet is an in-memory network: it is a net.Listener that accepts the server ends of the net.Pipe connections
// that Dial and HTTPClient open. Operations on a pipe block until the other end reads or writes, and inside a
// testing/synctest bubble they are durably blocking, so the bubble's fake clock runs. Create and use a PipeNet inside
// one bubble.
type PipeNet struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

// NewPipeNet returns an open PipeNet.
func NewPipeNet() *PipeNet {
	return &PipeNet{conns: make(chan net.Conn), closed: make(chan struct{})}
}

// Accept implements net.Listener.
func (n *PipeNet) Accept() (net.Conn, error) {
	select {
	case c := <-n.conns:
		return c, nil
	case <-n.closed:
		return nil, net.ErrClosed
	}
}

// Close implements net.Listener. Dial fails afterwards; open connections stay open.
func (n *PipeNet) Close() error {
	n.once.Do(func() { close(n.closed) })
	return nil
}

// Addr implements net.Listener.
func (n *PipeNet) Addr() net.Addr { return pipeAddr{} }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

// Dial opens a connection to the listener; network and address are ignored.
func (n *PipeNet) Dial(ctx context.Context, _, _ string) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case n.conns <- server:
		return client, nil
	case <-n.closed:
	case <-ctx.Done():
	}
	_ = client.Close()
	_ = server.Close()
	return nil, errors.New("signaltest: pipe network closed or dial canceled")
}

// HTTPClient returns an HTTP client whose requests go to the listener, whatever the URL's host. Keep-alives are off,
// so no idle connection outlives a request.
func (n *PipeNet) HTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: n.Dial, DisableKeepAlives: true}}
}

// Server is an http.Server on a PipeNet.
type Server struct {
	Net  *PipeNet
	srv  *http.Server
	done chan struct{}
}

// StartServer serves h on a new PipeNet until Close.
func StartServer(h http.Handler) *Server {
	s := &Server{Net: NewPipeNet(), srv: &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second},
		done: make(chan struct{})}
	go func() {
		defer close(s.done)
		_ = s.srv.Serve(s.Net)
	}()
	return s
}

// URL is the WebSocket URL of path on the server.
func (s *Server) URL(path string) string { return "ws://signaltest" + path }

// Close stops the server and waits for it. Hijacked connections (WebSockets) are not the server's: close them
// through the hub first.
func (s *Server) Close() {
	_ = s.srv.Close()
	<-s.done
}
