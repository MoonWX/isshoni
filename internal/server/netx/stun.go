package netx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/pion/stun/v4"
)

// STUNClient asks a STUN server for this host's public address (04 §7.4). NewSTUNClient is the real one; tests
// and doctor inject fakes, so detection runs offline.
type STUNClient interface {
	// Mapped sends a Binding request from one local socket to server and returns XOR-MAPPED-ADDRESS.
	Mapped(ctx context.Context, localSocket net.PacketConn, server string) (netip.AddrPort, error)
}

// Resolver resolves host names. *net.Resolver implements it (net.DefaultResolver is the real one); tests and
// doctor inject fakes.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// defaultSTUNPort is used when a STUN server is given without a port.
const defaultSTUNPort = 3478

// errSTUNFailed is wrapped by every error of a STUN server's answer (an error response, no mapped address).
var errSTUNFailed = errors.New("netx: STUN Binding failed")

// NewSTUNClient returns the real STUNClient on pion/stun: it resolves server names with r (nil means
// net.DefaultResolver) and sends one Binding request per Mapped call, without retransmission (DetectPublicAddrs
// retries). Mapped may be called concurrently on one socket: the client reads the socket while calls are
// running and hands each response to the call with the matching transaction ID, which is how 04 §7.4 queries
// both servers in parallel from one socket.
//
// Mapped owns the socket's read deadline while it runs: it clears any deadline the owner set and, when the last
// call on the socket returns, leaves it cleared (no deadline). The owner must not read the socket meanwhile, and
// sets its own deadline again afterwards if it needs one.
func NewSTUNClient(r Resolver) STUNClient {
	if r == nil {
		r = net.DefaultResolver
	}
	return &stunClient{resolver: r, socks: map[net.PacketConn]*stunSocket{}}
}

type stunClient struct {
	resolver Resolver

	mu    sync.Mutex
	socks map[net.PacketConn]*stunSocket
}

// stunSocket is the reader of one local socket while Mapped calls use it. Its fields are guarded by
// stunClient.mu.
type stunSocket struct {
	conn     net.PacketConn
	waiters  map[[stun.TransactionIDSize]byte]chan *stun.Message // buffered, capacity 1
	stopping bool                                                // the last call left; the reader exits
	done     chan struct{}                                       // closed when the reader has exited
}

func (c *stunClient) Mapped(ctx context.Context, localSocket net.PacketConn, server string) (netip.AddrPort, error) {
	to, err := c.resolve(ctx, localSocket, server)
	if err != nil {
		return netip.AddrPort{}, err
	}
	req, err := stun.Build(stun.TransactionID, stun.BindingRequest, stun.Fingerprint)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("netx: STUN request: %w", err)
	}
	answer := make(chan *stun.Message, 1)
	s, err := c.acquire(ctx, localSocket, req.TransactionID, answer)
	if err != nil {
		return netip.AddrPort{}, err
	}
	defer c.release(s, req.TransactionID)

	if _, err := localSocket.WriteTo(req.Raw, net.UDPAddrFromAddrPort(to)); err != nil {
		return netip.AddrPort{}, fmt.Errorf("netx: STUN %s: %w", server, err)
	}
	select {
	case <-ctx.Done():
		return netip.AddrPort{}, fmt.Errorf("netx: STUN %s: %w", server, ctx.Err())
	case m := <-answer:
		return mappedAddr(m, server)
	}
}

// resolve turns "host:port" (or "host", port 3478) into one address of the socket's family.
func (c *stunClient) resolve(ctx context.Context, sock net.PacketConn, server string) (netip.AddrPort, error) {
	host, portStr, err := net.SplitHostPort(server)
	if err != nil {
		host, portStr = server, strconv.Itoa(defaultSTUNPort)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || port == 0 {
		return netip.AddrPort{}, fmt.Errorf("netx: STUN server %q: bad port", server)
	}
	network := "ip4"
	if ua, ok := sock.LocalAddr().(*net.UDPAddr); ok && ua.IP.To4() == nil && !ua.IP.IsUnspecified() {
		network = "ip6"
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return netip.AddrPortFrom(ip.Unmap(), uint16(port)), nil
	}
	ips, err := c.resolver.LookupNetIP(ctx, network, host)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("netx: STUN server %q: %w", server, err)
	}
	for _, ip := range ips {
		ip = ip.Unmap()
		if (network == "ip4") == ip.Is4() {
			return netip.AddrPortFrom(ip, uint16(port)), nil
		}
	}
	return netip.AddrPort{}, fmt.Errorf("netx: STUN server %q: no %s address", server, network)
}

// mappedAddr reads XOR-MAPPED-ADDRESS (or, from an old server, MAPPED-ADDRESS) from a Binding response.
func mappedAddr(m *stun.Message, server string) (netip.AddrPort, error) {
	if m.Type != stun.BindingSuccess {
		return netip.AddrPort{}, fmt.Errorf("%w: %s answered %s", errSTUNFailed, server, m.Type)
	}
	var ip net.IP
	var port int
	var xor stun.XORMappedAddress
	if err := xor.GetFrom(m); err == nil {
		ip, port = xor.IP, xor.Port
	} else {
		var plain stun.MappedAddress
		if err := plain.GetFrom(m); err != nil {
			return netip.AddrPort{}, fmt.Errorf("%w: %s sent no mapped address", errSTUNFailed, server)
		}
		ip, port = plain.IP, plain.Port
	}
	a, ok := netip.AddrFromSlice(ip)
	if !ok || port <= 0 || port > 0xffff {
		return netip.AddrPort{}, fmt.Errorf("%w: %s sent a bad mapped address", errSTUNFailed, server)
	}
	return netip.AddrPortFrom(a.Unmap(), uint16(port)), nil
}

// acquire registers a waiter for id on conn and starts the socket's reader if none runs. A reader that is still
// stopping is waited for first, so two readers never share a socket or its read deadline.
func (c *stunClient) acquire(ctx context.Context, conn net.PacketConn, id [stun.TransactionIDSize]byte,
	answer chan *stun.Message,
) (*stunSocket, error) {
	for {
		c.mu.Lock()
		s := c.socks[conn]
		if s != nil && s.stopping {
			done := s.done
			c.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if s == nil {
			s = &stunSocket{
				conn:    conn,
				waiters: map[[stun.TransactionIDSize]byte]chan *stun.Message{},
				done:    make(chan struct{}),
			}
			c.socks[conn] = s
			_ = conn.SetReadDeadline(time.Time{})
			go c.read(s)
		}
		s.waiters[id] = answer
		c.mu.Unlock()
		return s, nil
	}
}

// release removes the waiter for id. The last one stops the reader (a read deadline in the past wakes it; the reader
// clears it again on its way out) and waits until it has exited, so every reader has an owner that waits for it. A
// reader that already gave up on a failing socket is no longer registered; its deadline is left alone, since a newer
// reader may own the socket.
func (c *stunClient) release(s *stunSocket, id [stun.TransactionIDSize]byte) {
	c.mu.Lock()
	delete(s.waiters, id)
	last := len(s.waiters) == 0 && !s.stopping
	if last {
		s.stopping = true
		if c.socks[s.conn] == s {
			_ = s.conn.SetReadDeadline(time.Now())
		}
	}
	c.mu.Unlock()
	if last {
		<-s.done
	}
}

// maxReadErrors ends a reader whose socket keeps failing with errors other than a timeout; the waiting calls then
// end with their contexts.
const maxReadErrors = 100

// read reads Binding responses from s.conn and hands each to the waiter with its transaction ID. Anything else
// (other traffic, unknown IDs, late answers) is dropped. On exit a still-registered reader clears the read deadline
// that release set to wake it, so none is left behind for the socket's owner; a new reader can register only after
// the delete, under the same lock, so it never loses its deadline to this one.
func (c *stunClient) read(s *stunSocket) {
	defer close(s.done)
	buf := make([]byte, 1500)
	errs := 0
	for {
		n, _, err := s.conn.ReadFrom(buf)
		c.mu.Lock()
		if s.stopping || errors.Is(err, net.ErrClosed) || errs >= maxReadErrors {
			if c.socks[s.conn] == s {
				_ = s.conn.SetReadDeadline(time.Time{})
				delete(c.socks, s.conn)
			}
			c.mu.Unlock()
			return
		}
		if err != nil {
			errs++
			c.mu.Unlock()
			continue
		}
		errs = 0
		if !stun.IsMessage(buf[:n]) {
			c.mu.Unlock()
			continue
		}
		m := &stun.Message{Raw: append([]byte(nil), buf[:n]...)}
		if m.Decode() == nil {
			if w, ok := s.waiters[m.TransactionID]; ok {
				select {
				case w <- m:
				default: // a duplicate answer; the first one counts
				}
			}
		}
		c.mu.Unlock()
	}
}
