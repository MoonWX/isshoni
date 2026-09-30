package netx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/stun/v4"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// stunServer is a local STUN responder on 127.0.0.1 built on pion/stun: it answers Binding requests with a chosen
// mapped address (or the real source address), an error response, only MAPPED-ADDRESS, or nothing.
type stunServer struct {
	conn     *net.UDPConn
	mapped   netip.AddrPort // zero: the request's source
	mode     string         // "" | "error" | "plain" | "silent"
	requests atomic.Int64
	done     chan struct{}
}

func startSTUNServer(t *testing.T, mapped, mode string) *stunServer {
	t.Helper()
	s := &stunServer{conn: listenUDP(t), mode: mode, done: make(chan struct{})}
	if mapped != "" {
		s.mapped = netip.MustParseAddrPort(mapped)
	}
	go s.serve()
	t.Cleanup(func() {
		_ = s.conn.Close()
		<-s.done
	})
	return s
}

func (s *stunServer) addr() string { return s.conn.LocalAddr().String() }

func (s *stunServer) serve() {
	defer close(s.done)
	buf := make([]byte, 1500)
	for {
		n, from, err := s.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		req := &stun.Message{Raw: append([]byte(nil), buf[:n]...)}
		if req.Decode() != nil || req.Type != stun.BindingRequest {
			continue
		}
		s.requests.Add(1)
		mapped := s.mapped
		if !mapped.IsValid() {
			mapped = from
		}
		ip, port := net.IP(mapped.Addr().AsSlice()), int(mapped.Port())
		var setters []stun.Setter
		switch s.mode {
		case "silent":
			continue
		case "error":
			setters = []stun.Setter{req, stun.BindingError, stun.CodeServerError}
		case "plain":
			setters = []stun.Setter{req, stun.BindingSuccess, &stun.MappedAddress{IP: ip, Port: port}}
		default:
			setters = []stun.Setter{req, stun.BindingSuccess, &stun.XORMappedAddress{IP: ip, Port: port}, stun.Fingerprint}
		}
		resp, err := stun.Build(setters...)
		if err != nil {
			continue
		}
		_, _ = s.conn.WriteToUDPAddrPort(resp.Raw, from)
	}
}

// fakeResolver resolves names from a table.
type fakeResolver struct {
	names map[string][]netip.Addr
	calls atomic.Int64
}

func (r *fakeResolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	r.calls.Add(1)
	if a, ok := r.names[host]; ok {
		return a, nil
	}
	return nil, fmt.Errorf("lookup %s %s: no such host", network, host)
}

func TestSTUNClientMapped(t *testing.T) {
	srv := startSTUNServer(t, "", "")
	sock := listenUDP(t)
	c := NewSTUNClient(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := c.Mapped(ctx, sock, srv.addr())
	if err != nil {
		t.Fatal(err)
	}
	if want := udpAddrPortOf(sock); got != want {
		t.Errorf("Mapped = %v, want the socket's own address %v", got, want)
	}
	// The socket is usable afterwards: no read deadline is left behind for its owner.
	if got, err := c.Mapped(ctx, sock, srv.addr()); err != nil || got != udpAddrPortOf(sock) {
		t.Errorf("second Mapped = %v, %v", got, err)
	}
}

// TestSTUNClientParallelOneSocket: two servers are asked at once from one socket, and each call gets its own
// server's answer even though one reader receives both.
func TestSTUNClientParallelOneSocket(t *testing.T) {
	a := startSTUNServer(t, "198.51.100.9:40000", "")
	b := startSTUNServer(t, "198.51.100.9:51234", "")
	r := &fakeResolver{names: map[string][]netip.Addr{
		"stun-a.test": {netip.MustParseAddr("2001:db8::1"), netip.MustParseAddr("127.0.0.1")}, // IPv4 is picked
	}}
	_, aPort, _ := net.SplitHostPort(a.addr())
	servers := []string{"stun-a.test:" + aPort, b.addr()}
	sock := listenUDP(t)
	c := NewSTUNClient(r)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for range 5 {
		results := make([]netip.AddrPort, len(servers))
		errs := make([]error, len(servers))
		var wg sync.WaitGroup
		for i, s := range servers {
			wg.Go(func() { results[i], errs[i] = c.Mapped(ctx, sock, s) })
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			t.Fatal(err)
		}
		if results[0] != netip.MustParseAddrPort("198.51.100.9:40000") ||
			results[1] != netip.MustParseAddrPort("198.51.100.9:51234") {
			t.Fatalf("results = %v", results)
		}
	}
	if r.calls.Load() == 0 {
		t.Error("the injected resolver was not used")
	}
}

func TestSTUNClientErrors(t *testing.T) {
	sock := listenUDP(t)
	c := NewSTUNClient(&fakeResolver{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errSrv := startSTUNServer(t, "", "error")
	if _, err := c.Mapped(ctx, sock, errSrv.addr()); !errors.Is(err, errSTUNFailed) {
		t.Errorf("error response: %v, want errSTUNFailed", err)
	}
	plain := startSTUNServer(t, "203.0.113.7:3478", "plain")
	if got, err := c.Mapped(ctx, sock, plain.addr()); err != nil || got != netip.MustParseAddrPort("203.0.113.7:3478") {
		t.Errorf("MAPPED-ADDRESS only: %v, %v", got, err)
	}
	if _, err := c.Mapped(ctx, sock, "unknown.test:3478"); err == nil {
		t.Error("an unresolvable server must be an error")
	}
	if _, err := c.Mapped(ctx, sock, "127.0.0.1:99999"); err == nil {
		t.Error("a bad port must be an error")
	}

	silent := startSTUNServer(t, "", "silent")
	short, cancelShort := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancelShort()
	if _, err := c.Mapped(short, sock, silent.addr()); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("no answer: %v, want context.DeadlineExceeded", err)
	}
	if silent.requests.Load() != 1 {
		t.Errorf("the silent server got %d requests, want 1", silent.requests.Load())
	}
	// After a timeout the next call on the same socket still works.
	ok := startSTUNServer(t, "", "")
	if _, err := c.Mapped(ctx, sock, ok.addr()); err != nil {
		t.Errorf("Mapped after a timeout: %v", err)
	}
}

// TestDetectWithRealSTUNClient runs DetectPublicAddrs end to end with the real client against two local servers
// that report different mapped ports: the NAT is symmetric.
func TestDetectWithRealSTUNClient(t *testing.T) {
	a := startSTUNServer(t, "198.51.100.9:40000", "")
	b := startSTUNServer(t, "198.51.100.9:51234", "")
	lister := &fakeIfaces{
		ifs:    []Interface{iface("eth0", 0, "192.168.1.20")},
		routes: map[netip.Addr]netip.Addr{v4Probe: netip.MustParseAddr("192.168.1.20")},
	}
	got, err := DetectPublicAddrs(context.Background(), DetectOptions{
		STUNServers: []string{a.addr(), b.addr()}, Timeout: 2 * time.Second, Interfaces: lister,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.V4 != netip.MustParseAddr("198.51.100.9") || got.V4Method != MethodSTUN || got.NAT != api.NATKindSymmetric {
		t.Errorf("got V4 %v (%s) NAT %s, want 198.51.100.9 (stun) symmetric", got.V4, got.V4Method, got.NAT)
	}
}
