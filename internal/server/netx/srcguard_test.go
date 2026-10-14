package netx

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/stun/v4"
	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/logx"
)

// floodSources is the number of distinct source addresses the acceptance tests send valid Binding requests from.
const floodSources = 1000

// The far side's ICE credentials in the agent tests (the server agent's remote ufrag and password).
const (
	peerUfrag = "peer"
	peerPwd   = "peerpasswordpeerpassword"
)

// testSource is source i of one address, 0 ≤ i < 50000.
func testSource(i int, tcp bool) iceSource {
	if i < 0 || i >= 50000 {
		panic("testSource: i out of range")
	}
	return iceSource{tcp: tcp, addr: netip.AddrPortFrom(netip.MustParseAddr("203.0.113.7"), 10000+uint16(i))}
}

// frameOf returns b as one RFC 4571 frame.
func frameOf(b []byte) []byte {
	if n := len(b); n <= 0xffff {
		return append(binary.BigEndian.AppendUint16(nil, uint16(n)), b...)
	}
	panic("frameOf: longer than a frame")
}

// TestSourceGuardCounts: a known ufrag admits 64 distinct sources, a source it has admitted costs nothing again,
// a UDP and a TCP source on one address and port are two, ufrags count apart, a ufrag the guard doesn't know
// passes, and forget starts the count over.
func TestSourceGuardCounts(t *testing.T) {
	g := newSourceGuard(maxSourcesPerUfrag, nil)
	ufragA, ufragB := []byte("ufragA"), []byte("ufragB")
	if !g.admit(ufragA, testSource(0, false)) {
		t.Fatal("STUN for a ufrag the guard doesn't know must pass")
	}
	if g.known() != 0 {
		t.Fatal("admit must not make a ufrag known")
	}

	g.open("ufragA")
	for i := range maxSourcesPerUfrag {
		if !g.admit(ufragA, testSource(i, i%2 == 1)) {
			t.Fatalf("source %d refused", i+1)
		}
	}
	if g.admit(ufragA, testSource(maxSourcesPerUfrag, false)) {
		t.Fatal("the 65th source was admitted")
	}
	if g.admit(ufragA, testSource(0, true)) {
		t.Error("a TCP source on a UDP source's address and port must be another source")
	}
	if !g.admit(ufragA, testSource(0, false)) || !g.admit(ufragA, testSource(1, true)) {
		t.Error("an admitted source must pass again")
	}
	mapped := iceSource{addr: canonicalSource(netip.MustParseAddrPort("[::ffff:203.0.113.7]:10000"))}
	if !g.admit(ufragA, mapped) {
		t.Error("an IPv4-mapped address must count as its IPv4 address")
	}
	g.open("ufragA") // another socket's GetConn for the same ufrag
	if n := g.sources("ufragA"); n != maxSourcesPerUfrag {
		t.Errorf("sources = %d after a second open, want %d", n, maxSourcesPerUfrag)
	}

	g.open("ufragB")
	if !g.admit(ufragB, testSource(maxSourcesPerUfrag, false)) {
		t.Error("another ufrag must have its own count")
	}
	if got := g.refused.Load(); got != 2 {
		t.Errorf("refused = %d, want 2", got)
	}

	g.forget("ufragA")
	if g.known() != 1 || g.sources("ufragA") != 0 {
		t.Fatalf("after forget: %d known, %d sources", g.known(), g.sources("ufragA"))
	}
	g.open("ufragA")
	for i := range maxSourcesPerUfrag {
		if !g.admit(ufragA, testSource(1000+i, false)) {
			t.Fatalf("after forget, source %d refused", i+1)
		}
	}
	if g.admit(ufragA, testSource(2000, false)) {
		t.Error("after forget, the 65th source was admitted")
	}
	g.forget("ufragA")
	g.forget("ufragB")
	g.forget("ufragB")
	if g.known() != 0 {
		t.Errorf("%d ufrags left", g.known())
	}
}

func TestCanonicalSource(t *testing.T) {
	for in, want := range map[string]string{
		"[::ffff:192.0.2.1]:5000":  "192.0.2.1:5000",
		"[2001:db8::1%eth0]:5000":  "[2001:db8::1]:5000",
		"[fe80::1%eth0]:5000":      "[fe80::1%eth0]:5000",
		"198.51.100.4:7882":        "198.51.100.4:7882",
		"[2001:db8::7]:443":        "[2001:db8::7]:443",
		"[::ffff:127.0.0.1]:65535": "127.0.0.1:65535",
	} {
		if got := canonicalSource(netip.MustParseAddrPort(in)); got != netip.MustParseAddrPort(want) {
			t.Errorf("canonicalSource(%s) = %s, want %s", in, got, want)
		}
	}
}

func buildSTUN(t *testing.T, setters ...stun.Setter) []byte {
	t.Helper()
	m, err := stun.Build(append([]stun.Setter{stun.TransactionID}, setters...)...)
	if err != nil {
		t.Fatal(err)
	}
	return m.Raw
}

func TestStunUfrag(t *testing.T) {
	full := buildSTUN(t, stun.BindingRequest, stun.NewUsername("server:client"), stun.NewShortTermIntegrity("pwd"),
		stun.Fingerprint)
	for _, tc := range []struct {
		name  string
		raw   []byte
		ufrag string // "" = not ok
	}{
		{"request", full, "server"},
		{"no colon", buildSTUN(t, stun.BindingRequest, stun.NewUsername("server")), "server"},
		{"several colons", buildSTUN(t, stun.BindingRequest, stun.NewUsername("a:b:c")), "a"},
		{"indication", buildSTUN(t, stun.NewType(stun.MethodBinding, stun.ClassIndication), stun.NewUsername("x:y")), "x"},
		{"no USERNAME", buildSTUN(t, stun.BindingSuccess), ""},
		{"truncated", full[:len(full)-8], ""},
		{"not STUN", []byte{0x80, 0x60, 0, 1, 0, 0, 0, 0, 0, 0, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}, ""},
		{"empty", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := stunUfrag(tc.raw)
			if ok != (tc.ufrag != "") || string(got) != tc.ufrag {
				t.Errorf("stunUfrag = %q, %v; want %q", got, ok, tc.ufrag)
			}
		})
	}
}

// TestAdmitUDP: only STUN with a USERNAME is judged; media and STUN without one pass whatever the count.
func TestAdmitUDP(t *testing.T) {
	g := newSourceGuard(1, nil)
	g.open("u")
	first, second := netip.MustParseAddrPort("192.0.2.1:1000"), netip.MustParseAddrPort("192.0.2.1:1001")
	req := buildSTUN(t, stun.BindingRequest, stun.NewUsername("u:peer"))
	if !g.admitUDP(req, first) {
		t.Fatal("the first source was refused")
	}
	if g.admitUDP(req, second) {
		t.Fatal("a second source was admitted with a limit of 1")
	}
	if !g.admitUDP(buildSTUN(t, stun.BindingSuccess), second) {
		t.Error("STUN without a USERNAME must pass")
	}
	if !g.admitUDP([]byte{0x80, 0x60, 0x00, 0x01, 1, 2, 3, 4, 5, 6, 7, 8}, second) {
		t.Error("RTP must pass")
	}
	if !g.admitUDP(buildSTUN(t, stun.BindingRequest, stun.NewUsername("other:peer")), second) {
		t.Error("STUN for a ufrag the guard doesn't know must pass")
	}
	var nilGuard *sourceGuard
	if !nilGuard.admitUDP(req, second) {
		t.Error("a nil guard must pass everything")
	}
}

// TestFirstFrameGate: the gate returns the first frame and what follows byte for byte, however pion reads it,
// unless the frame names a known ufrag beyond its limit or is too long.
func TestFirstFrameGate(t *testing.T) {
	req := frameOf(buildSTUN(t, stun.BindingRequest, stun.NewUsername("u:peer")))
	tail := []byte("\x00\x04rest")
	for _, tc := range []struct {
		name    string
		send    []byte
		full    bool // the guard's "u" already has its limit of sources
		wantErr error
	}{
		{"admitted", append(bytes.Clone(req), tail...), false, nil},
		{"over the limit", req, true, errICESourceLimit},
		{"unknown ufrag", frameOf(buildSTUN(t, stun.BindingRequest, stun.NewUsername("x:peer"))), true, nil},
		{"not STUN", append(frameOf([]byte("hello, not stun")), tail...), true, nil},
		{"too long", frameOf(make([]byte, maxFirstFrame+1)), false, errFirstFrameTooLarge},
		{"empty frame", append(frameOf(nil), tail...), true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newSourceGuard(1, nil)
			g.open("u")
			if tc.full {
				g.admit([]byte("u"), testSource(1, true))
			}
			server, client := pipeFrom(t, "203.0.113.7:10000")
			go func() {
				_, _ = client.Write(tc.send)
				_ = client.Close()
			}()
			gate := newFirstFrameGate(server, g)
			var got []byte
			buf := make([]byte, 3) // small reads, like pion's 2-byte header read
			for {
				n, err := gate.Read(buf)
				got = append(got, buf[:n]...)
				if err != nil {
					if tc.wantErr != nil {
						if !errors.Is(err, tc.wantErr) || len(got) != 0 {
							t.Fatalf("read %q, %v; want nothing and %v", got, err, tc.wantErr)
						}
						return
					}
					if !errors.Is(err, io.EOF) {
						t.Fatal(err)
					}
					break
				}
			}
			if tc.wantErr != nil {
				t.Fatalf("read %q without an error, want %v", got, tc.wantErr)
			}
			if !bytes.Equal(got, tc.send) {
				t.Errorf("read %q, want %q", got, tc.send)
			}
		})
	}
}

// bindingRequest is a connectivity check as an ICE peer sends it to the server agent: USERNAME "<server ufrag>:peer",
// PRIORITY, ICE-CONTROLLING, MESSAGE-INTEGRITY with the server's password, and FINGERPRINT.
func bindingRequest(t *testing.T, ufrag, pwd string) *stun.Message {
	t.Helper()
	m, err := stun.Build(stun.TransactionID, stun.BindingRequest, stun.NewUsername(ufrag+":"+peerUfrag),
		ice.PriorityAttr(0x6e0001ff), ice.AttrControlling(0x1234567890), stun.NewShortTermIntegrity(pwd),
		stun.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// isResponseTo reports whether b is the Binding success response to req.
func isResponseTo(b []byte, req *stun.Message) bool {
	m := &stun.Message{Raw: append([]byte(nil), b...)}
	return m.Decode() == nil && m.Type == stun.BindingSuccess && m.TransactionID == req.TransactionID
}

// writeSpy is a server UDP socket (TransportOptions.PacketConns) that records every address the server sends to.
// The UDP mux enters an address in its address map when it first sends to it, so these are the map's entries.
type writeSpy struct {
	*net.UDPConn
	mu  sync.Mutex
	dst map[netip.AddrPort]bool
}

func newWriteSpy(t *testing.T) *writeSpy {
	t.Helper()
	var lc net.ListenConfig
	pc, err := lc.ListenPacket(context.Background(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := pc.(*net.UDPConn)
	_ = c.SetReadBuffer(4 << 20)
	return &writeSpy{UDPConn: c, dst: map[netip.AddrPort]bool{}}
}

func (s *writeSpy) ReadFromAddrPort(b []byte) (int, netip.AddrPort, error) {
	return s.ReadFromUDPAddrPort(b)
}

func (s *writeSpy) WriteToAddrPort(b []byte, to netip.AddrPort) (int, error) {
	s.record(to)
	return s.WriteToUDPAddrPort(b, to)
}

func (s *writeSpy) WriteTo(b []byte, to net.Addr) (int, error) {
	s.record(packetSource(to))
	return s.UDPConn.WriteTo(b, to)
}

func (s *writeSpy) record(to netip.AddrPort) {
	s.mu.Lock()
	s.dst[canonicalSource(to)] = true
	s.mu.Unlock()
}

func (s *writeSpy) destinations() []netip.AddrPort {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]netip.AddrPort, 0, len(s.dst))
	for a := range s.dst {
		out = append(out, a)
	}
	return out
}

func (s *writeSpy) addr() netip.AddrPort { return udpAddrPortOf(s.UDPConn) }

// listenSources binds n UDP sockets on 127.0.0.1, each a distinct source address.
func listenSources(t *testing.T, n int) []*net.UDPConn {
	t.Helper()
	out := make([]*net.UDPConn, 0, n)
	for range n {
		out = append(out, listenUDP(t))
	}
	return out
}

// udpSend sends req from src to dst.
func udpSend(t *testing.T, src *net.UDPConn, dst netip.AddrPort, req *stun.Message) {
	t.Helper()
	if _, err := src.WriteToUDPAddrPort(req.Raw, dst); err != nil {
		t.Fatal(err)
	}
}

// awaitUDPResponse reads src until the response to req arrives (skipping the agent's own checks) or wait passes.
func awaitUDPResponse(src *net.UDPConn, req *stun.Message, wait time.Duration) bool {
	_ = src.SetReadDeadline(time.Now().Add(wait))
	defer func() { _ = src.SetReadDeadline(time.Time{}) }()
	buf := make([]byte, 1500)
	for {
		n, _, err := src.ReadFromUDPAddrPort(buf)
		if err != nil {
			return false
		}
		if isResponseTo(buf[:n], req) {
			return true
		}
	}
}

// udpExchange sends a fresh request from src to dst and waits for its response, trying a few times (loopback may
// drop a datagram under load). It is also the barrier of the UDP tests: one goroutine reads each server socket and
// one agent loop handles requests in order, so the response comes after everything src's socket was sent before.
func udpExchange(t *testing.T, src *net.UDPConn, dst netip.AddrPort, ufrag, pwd string) bool {
	t.Helper()
	for range 5 {
		req := bindingRequest(t, ufrag, pwd)
		udpSend(t, src, dst, req)
		if awaitUDPResponse(src, req, 2*time.Second) {
			return true
		}
	}
	return false
}

// expectSilent checks that none of srcs receives anything within a short wait.
func expectSilent(t *testing.T, srcs []*net.UDPConn, what string) {
	t.Helper()
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		heard int
	)
	for _, c := range srcs {
		wg.Go(func() {
			_ = c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			if _, _, err := c.ReadFromUDPAddrPort(make([]byte, 1500)); err == nil {
				mu.Lock()
				heard++
				mu.Unlock()
			}
			_ = c.SetReadDeadline(time.Time{})
		})
	}
	wg.Wait()
	if heard != 0 {
		t.Errorf("%d of %d %s got a packet from the server", heard, len(srcs), what)
	}
}

// startAgent starts a real pion ICE agent on the Transport's muxes (opts add them), gathers its host candidates and
// accepts as the controlled side with the peer's credentials, as the SFU's PCs do. It never fails or disconnects
// on its own, so a long test sees one ufrag until it restarts.
func startAgent(t *testing.T, opts ...ice.AgentOption) *ice.Agent {
	t.Helper()
	a, err := ice.NewAgentWithOptions(append([]ice.AgentOption{
		ice.WithCandidateTypes([]ice.CandidateType{ice.CandidateTypeHost}),
		ice.WithMulticastDNSMode(ice.MulticastDNSModeDisabled),
		ice.WithIncludeLoopback(),
		ice.WithDisconnectedTimeout(0),
		ice.WithFailedTimeout(0),
		ice.WithLoggerFactory(logx.NewPionLoggerFactory(slog.New(slog.DiscardHandler))),
	}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	gather(t, a)
	if _, err := a.StartAccept(peerUfrag, peerPwd); err != nil {
		t.Fatal(err)
	}
	return a
}

// gather gathers a's candidates and waits until gathering is complete.
func gather(t *testing.T, a *ice.Agent) {
	t.Helper()
	done := make(chan struct{})
	var once sync.Once
	if err := a.OnCandidate(func(c ice.Candidate) {
		if c == nil {
			once.Do(func() { close(done) })
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.GatherCandidates(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("gathering did not complete")
	}
}

func localCreds(t *testing.T, a *ice.Agent) (ufrag, pwd string) {
	t.Helper()
	ufrag, pwd, err := a.GetLocalUserCredentials()
	if err != nil {
		t.Fatal(err)
	}
	return ufrag, pwd
}

// prflxCount returns the number of a's peer-reflexive remote candidates.
func prflxCount(t *testing.T, a *ice.Agent) int {
	t.Helper()
	cands, err := a.GetRemoteCandidates()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range cands {
		if c.Type() == ice.CandidateTypePeerReflexive {
			n++
		}
	}
	return n
}

// closeAgent closes a and checks that the guard has forgotten its ufrag (pion calls RemoveConnByUfrag on close).
func closeAgent(t *testing.T, a *ice.Agent, g *sourceGuard) {
	t.Helper()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the guard forgets the closed agent's ufrag", func() bool { return g.known() == 0 })
}

// TestSourceGuardUDPAgent is the acceptance check over UDP. A real pion agent sits behind the Transport's UDP mux
// with two local sockets, and 1000 distinct source ports send it valid Binding requests, spread over both sockets.
// The first 64 sources get their Binding response; from the 65th on there is no response, no packet at all and no
// address-map entry, and the agent ends with 64 peer-reflexive candidates: one count for the ufrag across both
// sockets. After an ICE restart (a new ufrag) the count starts over, and after close the guard keeps nothing.
func TestSourceGuardUDPAgent(t *testing.T) {
	spies := []*writeSpy{newWriteSpy(t), newWriteSpy(t)}
	tr := newTestTransport(t, TransportOptions{
		PacketConns: []net.PacketConn{spies[0], spies[1]}, IncludeLoopback: true,
	})
	agent := startAgent(t, ice.WithUDPMux(tr.UDPMux), ice.WithNetworkTypes([]ice.NetworkType{ice.NetworkTypeUDP4}))
	if local, err := agent.GetLocalCandidates(); err != nil || len(local) != 2 {
		t.Fatalf("local candidates %v, %v; want one per socket", local, err)
	}
	ufrag, pwd := localCreds(t, agent)
	srcs := listenSources(t, floodSources)
	dst := func(i int) netip.AddrPort { return spies[i%2].addr() }

	// The first 64 sources each get their response before the next one sends.
	admitted := map[netip.AddrPort]bool{}
	for i := range maxSourcesPerUfrag {
		if !udpExchange(t, srcs[i], dst(i), ufrag, pwd) {
			t.Fatalf("source %d: no Binding response", i+1)
		}
		admitted[udpAddrPortOf(srcs[i])] = true
	}
	// The other 936 send in bursts of 100; after each burst one admitted source per socket waits for its answer,
	// so the burst has been read (and the sockets' buffers can't overflow).
	for start := maxSourcesPerUfrag; start < floodSources; start += 100 {
		for i := start; i < min(start+100, floodSources); i++ {
			udpSend(t, srcs[i], dst(i), bindingRequest(t, ufrag, pwd))
		}
		for i := range 2 {
			if !udpExchange(t, srcs[i], dst(i), ufrag, pwd) {
				t.Fatalf("admitted source %d: no Binding response after a burst", i+1)
			}
		}
	}

	expectSilent(t, srcs[maxSourcesPerUfrag:], "sources past the limit")
	var strays []netip.AddrPort
	for _, s := range spies {
		for _, a := range s.destinations() {
			if !admitted[a] {
				strays = append(strays, a)
			}
		}
	}
	if len(strays) != 0 {
		t.Errorf("the server sent to %d sources past the limit (address-map entries), e.g. %v", len(strays), strays[0])
	}
	if n := prflxCount(t, agent); n != maxSourcesPerUfrag {
		t.Errorf("%d peer-reflexive candidates, want %d", n, maxSourcesPerUfrag)
	}
	if n := tr.guard.sources(ufrag); n != maxSourcesPerUfrag {
		t.Errorf("the guard counts %d sources, want %d", n, maxSourcesPerUfrag)
	}
	if got, want := tr.guard.refused.Load(), uint64(floodSources-maxSourcesPerUfrag); got != want {
		t.Errorf("refused %d requests, want %d", got, want)
	}

	// An ICE restart: pion removes the old ufrag from the muxes and gathers with a new one, whose count starts
	// over: sources 65 to 128, refused so far, are answered now, and the 129th is not.
	if err := agent.Restart("", ""); err != nil {
		t.Fatal(err)
	}
	gather(t, agent)
	if err := agent.SetRemoteCredentials(peerUfrag, peerPwd); err != nil {
		t.Fatal(err)
	}
	ufrag2, pwd2 := localCreds(t, agent)
	if ufrag2 == ufrag {
		t.Fatal("the restart kept the ufrag")
	}
	if tr.guard.known() != 1 || tr.guard.sources(ufrag) != 0 {
		t.Fatalf("after the restart the guard knows %d ufrags, %d sources of the old one", tr.guard.known(),
			tr.guard.sources(ufrag))
	}
	for i := maxSourcesPerUfrag; i < 2*maxSourcesPerUfrag; i++ {
		if !udpExchange(t, srcs[i], dst(i), ufrag2, pwd2) {
			t.Fatalf("after the restart, source %d: no Binding response", i+1)
		}
	}
	next := 2 * maxSourcesPerUfrag
	udpSend(t, srcs[next], dst(next), bindingRequest(t, ufrag2, pwd2))
	if !udpExchange(t, srcs[next-2], dst(next), ufrag2, pwd2) { // the same server socket: a barrier
		t.Fatal("after the restart, an admitted source got no response")
	}
	expectSilent(t, srcs[next:next+1], "sources past the new ufrag's limit")
	if n := prflxCount(t, agent); n != maxSourcesPerUfrag {
		t.Errorf("after the restart, %d peer-reflexive candidates, want %d", n, maxSourcesPerUfrag)
	}

	closeAgent(t, agent, tr.guard)
}

// TestSourceGuardRemoveConnByUfrag: through the Transport's own muxes (no agent), a ufrag's connection receives the
// STUN of its first 64 sources only; after RemoveConnByUfrag and a new GetConn the count starts over. Both muxes
// tell the guard which ufrags they serve.
func TestSourceGuardRemoveConnByUfrag(t *testing.T) {
	tr := newTestTransport(t, TransportOptions{UDPAddr: "127.0.0.1:0", TCPAddr: "127.0.0.1:0", IncludeLoopback: true})
	local := tr.UDPMux.GetListenAddresses()[0]
	dst := local.(*net.UDPAddr).AddrPort()
	srcs := listenSources(t, maxSourcesPerUfrag+1)

	tc, err := tr.TCPMux.GetConnByUfrag("tcpufrag", false, net.IPv4(127, 0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tc.Close() }()
	if tr.guard.known() != 1 {
		t.Fatalf("GetConnByUfrag: the guard knows %d ufrags", tr.guard.known())
	}

	// receive sends one request from each of srcs and returns the sources the ufrag's connection got them from.
	receive := func(pc net.PacketConn) map[netip.AddrPort]bool {
		t.Helper()
		for _, s := range srcs {
			udpSend(t, s, dst, bindingRequest(t, "udpufrag", "unused"))
		}
		// srcs[0] again as the last: the socket's one reader routes in order, so it arrives after all the others.
		last := bindingRequest(t, "udpufrag", "unused")
		udpSend(t, srcs[0], dst, last)
		got := map[netip.AddrPort]bool{}
		buf := make([]byte, 1500)
		_ = pc.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				t.Fatalf("after %d sources: %v", len(got), err)
			}
			if bytes.Equal(buf[:n], last.Raw) {
				return got
			}
			got[from.(*net.UDPAddr).AddrPort()] = true
		}
	}

	// The connections stay open until the end: closing one makes the mux remove its ufrag later, from a goroutine
	// of its own, which could hit the next connection of the same ufrag. (Pion never reuses a ufrag.)
	pc, err := tr.UDPMux.GetConn("udpufrag", local)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	got := receive(pc)
	if len(got) != maxSourcesPerUfrag || got[udpAddrPortOf(srcs[maxSourcesPerUfrag])] {
		t.Fatalf("the connection got STUN from %d sources (the 65th: %v), want the first %d",
			len(got), got[udpAddrPortOf(srcs[maxSourcesPerUfrag])], maxSourcesPerUfrag)
	}

	tr.UDPMux.RemoveConnByUfrag("udpufrag")
	if tr.guard.known() != 1 || tr.guard.sources("udpufrag") != 0 {
		t.Fatalf("after RemoveConnByUfrag: %d known, %d sources", tr.guard.known(), tr.guard.sources("udpufrag"))
	}
	// The 65 sources again, in reverse order: the count started over, so the old 65th is admitted now.
	for i, j := 0, len(srcs)-1; i < j; i, j = i+1, j-1 {
		srcs[i], srcs[j] = srcs[j], srcs[i]
	}
	pc2, err := tr.UDPMux.GetConn("udpufrag", local)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc2.Close() }()
	got = receive(pc2)
	if len(got) != maxSourcesPerUfrag || !got[udpAddrPortOf(srcs[0])] {
		t.Errorf("after RemoveConnByUfrag the connection got STUN from %d sources (the old 65th: %v)",
			len(got), got[udpAddrPortOf(srcs[0])])
	}

	tr.TCPMux.RemoveConnByUfrag("tcpufrag")
	tr.UDPMux.RemoveConnByUfrag("udpufrag")
	if tr.guard.known() != 0 {
		t.Errorf("the guard knows %d ufrags after both were removed", tr.guard.known())
	}
}

// writeFrame writes b as one RFC 4571 frame.
func writeFrame(c net.Conn, b []byte) error {
	_, err := c.Write(frameOf(b))
	return err
}

// awaitTCPResponse reads frames from c until the response to req arrives (skipping the agent's own checks).
func awaitTCPResponse(c net.Conn, req *stun.Message) error {
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = c.SetReadDeadline(time.Time{}) }()
	var hdr [2]byte
	for {
		if _, err := io.ReadFull(c, hdr[:]); err != nil {
			return err
		}
		frame := make([]byte, binary.BigEndian.Uint16(hdr[:]))
		if _, err := io.ReadFull(c, frame); err != nil {
			return err
		}
		if isResponseTo(frame, req) {
			return nil
		}
	}
}

// TestSourceGuardTCPAgent is the acceptance check over ICE-TCP, on 7882, on 443 through the PortMux, and on both
// (one count for the ufrag across the two ports). 1000 connections from distinct source ports each send a valid
// Binding request as their first frame. The first 64 get their response; from the 65th on the server closes the
// connection without a byte, before pion's TCP mux attaches it to the ufrag, and the agent ends with 64
// peer-reflexive candidates. The per-IP ICE-TCP limit is raised so that only the guard refuses.
func TestSourceGuardTCPAgent(t *testing.T) {
	for _, tc := range []struct {
		name            string
		tcp443, tcp7882 bool
	}{
		{"7882", false, true},
		{"443", true, false},
		{"443 and 7882", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := TransportOptions{IncludeLoopback: true, Interfaces: &fakeIfaces{ifs: []Interface{loIface()}}}
			var pm *PortMux
			if tc.tcp443 {
				pm = newTestPortMux(t, PortMuxOptions{MaxICEConnsPerIP: 4 * floodSources})
				opts.PortMux = pm
			} else {
				opts.MaxICEConnsPerIP = 4 * floodSources
			}
			if tc.tcp7882 {
				opts.TCPAddr = "127.0.0.1:0"
			}
			tr := newTestTransport(t, opts)
			var ports []string
			if tc.tcp443 {
				ports = append(ports, pm.ICE().Addr().String())
			}
			if tc.tcp7882 {
				ports = append(ports, tr.tcpLn.Addr().String())
			}
			agent := startAgent(t, ice.WithTCPMux(tr.TCPMux), ice.WithNetworkTypes([]ice.NetworkType{ice.NetworkTypeTCP4}),
				ice.WithIPFilter(tr.IPFilter), ice.WithInterfaceFilter(tr.InterfaceFilter), ice.WithDisableActiveTCP())
			if local, err := agent.GetLocalCandidates(); err != nil || len(local) != len(ports) {
				t.Fatalf("local candidates %v, %v; want one per port", local, err)
			}
			ufrag, pwd := localCreds(t, agent)

			var d net.Dialer
			var open []net.Conn
			defer func() {
				for _, c := range open {
					_ = c.Close()
				}
			}()
			for i := range floodSources {
				c, err := d.DialContext(context.Background(), "tcp4", ports[i%len(ports)])
				if err != nil {
					t.Fatalf("source %d: %v", i+1, err)
				}
				req := bindingRequest(t, ufrag, pwd)
				if err := writeFrame(c, req.Raw); err != nil {
					_ = c.Close()
					t.Fatalf("source %d: %v", i+1, err)
				}
				if i < maxSourcesPerUfrag {
					open = append(open, c)
					if err := awaitTCPResponse(c, req); err != nil {
						t.Fatalf("source %d: no Binding response: %v", i+1, err)
					}
					continue
				}
				_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
				n, err := c.Read(make([]byte, 64))
				_ = c.Close()
				if n != 0 || err == nil || isTimeout(err) {
					t.Fatalf("source %d: read %d bytes, %v; want the connection closed without an answer", i+1, n, err)
				}
			}

			if n := prflxCount(t, agent); n != maxSourcesPerUfrag {
				t.Errorf("%d peer-reflexive candidates, want %d", n, maxSourcesPerUfrag)
			}
			if n := tr.guard.sources(ufrag); n != maxSourcesPerUfrag {
				t.Errorf("the guard counts %d sources, want %d", n, maxSourcesPerUfrag)
			}
			if got, want := tr.guard.refused.Load(), uint64(floodSources-maxSourcesPerUfrag); got != want {
				t.Errorf("refused %d connections, want %d", got, want)
			}
			closeAgent(t, agent, tr.guard)
		})
	}
}

// TestSourceGuardNormalClient: a client with host candidates on 127.0.0.1 and ::1 and a server-reflexive candidate
// in each family connects through the Transport as before, and the guard refuses nothing. When the PCs close, the
// guard keeps nothing.
func TestSourceGuardNormalClient(t *testing.T) {
	var lc net.ListenConfig
	probe, err := lc.ListenPacket(context.Background(), "udp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback here: %v", err)
	}
	_ = probe.Close()
	tr := newTestTransport(t, TransportOptions{
		UDPAddr: ":0", TCPAddr: ":0", IncludeLoopback: true, IPv6: true,
		Interfaces: &fakeIfaces{ifs: []Interface{loIface()}},
	})

	// The client's host candidates come from one socket per loopback address (pion skips ::1 when it scans the
	// interfaces itself), its srflx candidates from the rewrite rules: a socket of its own per family, as a NAT
	// would show it.
	var hostMuxes []ice.UDPMux
	for _, addr := range []string{"127.0.0.1:0", "[::1]:0"} {
		pc, err := lc.ListenPacket(context.Background(), "udp", addr)
		if err != nil {
			t.Fatal(err)
		}
		hostMuxes = append(hostMuxes, ice.NewUDPMuxDefault(ice.UDPMuxParams{
			UDPConn: pc, Logger: logx.NewPionLoggerFactory(slog.New(slog.DiscardHandler)).NewLogger("ice"),
		}))
	}
	clientMux := ice.NewMultiUDPMuxDefault(hostMuxes...)
	defer func() { _ = clientMux.Close() }()
	var se webrtc.SettingEngine
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})
	se.SetICEUDPMux(clientMux)
	se.SetIncludeLoopbackCandidate(true)
	se.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	if err := se.SetICEAddressRewriteRules(
		webrtc.ICEAddressRewriteRule{
			External: []string{"127.0.0.1"}, AsCandidateType: webrtc.ICECandidateTypeSrflx,
			Mode: webrtc.ICEAddressRewriteReplace, Networks: []webrtc.NetworkType{webrtc.NetworkTypeUDP4},
		},
		webrtc.ICEAddressRewriteRule{
			External: []string{"::1"}, AsCandidateType: webrtc.ICECandidateTypeSrflx,
			Mode: webrtc.ICEAddressRewriteReplace, Networks: []webrtc.NetworkType{webrtc.NetworkTypeUDP6},
		},
	); err != nil {
		t.Fatal(err)
	}
	local, clientSDP := connectPairSDP(t, tr, webrtc.NewAPI(webrtc.WithSettingEngine(se)))
	if local.Protocol != webrtc.ICEProtocolUDP {
		t.Errorf("selected local candidate %+v, want UDP", local)
	}

	seen := map[string]bool{}
	for _, c := range sdpCandidates(clientSDP) {
		family := "4"
		if c.addr.Addr().Is6() {
			family = "6"
		}
		if c.addr.Addr().IsLoopback() {
			seen[c.typ+family] = true
		}
	}
	if !seen["host4"] || !seen["host6"] || !seen["srflx4"] || !seen["srflx6"] {
		t.Errorf("client candidates %v; want host and srflx, IPv4 and IPv6:\n%s", seen, strings.TrimSpace(clientSDP))
	}
	if n := tr.guard.refused.Load(); n != 0 {
		t.Errorf("the guard refused %d packets of a normal client", n)
	}
	eventually(t, "the guard forgets the closed PC's ufrag", func() bool { return tr.guard.known() == 0 })
}
