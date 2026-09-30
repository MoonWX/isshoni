package netx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// newTestTransport builds a Transport and closes it when the test ends.
func newTestTransport(t *testing.T, opts TransportOptions) *Transport {
	t.Helper()
	tr, err := NewTransport(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := tr.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return tr
}

// newTestPortMux binds a PortMux on 127.0.0.1:0 (unless opts.Addr says otherwise) and closes it when the test ends.
func newTestPortMux(t *testing.T, opts PortMuxOptions) *PortMux {
	t.Helper()
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:0"
	}
	m, err := ListenPortMux(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Errorf("PortMux.Close: %v", err)
		}
	})
	return m
}

func tcpPortOf(l net.Listener) uint16 { return l.Addr().(*net.TCPAddr).AddrPort().Port() }

// TestTransportAdvertised checks the advertised addresses after the rewrite rules, with Via for every transport,
// on a home LAN server (Append mode) with fake interfaces and an injected UDP socket.
func TestTransportAdvertised(t *testing.T) {
	lister := &fakeIfaces{ifs: []Interface{
		loIface(),
		iface("eth0", 0, "192.168.1.20", "2001:db8::20"),
		iface("docker0", 0, "172.17.0.1"),
	}}
	pm := newTestPortMux(t, PortMuxOptions{})
	udp := newMemPacketConn("192.168.1.20:7882")
	var counter TransferCounter
	tr := newTestTransport(t, TransportOptions{
		TCPAddr:           "0.0.0.0:0",
		PortMux:           pm,
		ExcludeInterfaces: []string{"docker*"},
		IPv6:              false,
		Public: PublicAddrs{V4: netip.MustParseAddr("198.51.100.9"), V4Method: MethodSTUN,
			LocalV4: netip.MustParseAddr("192.168.1.20"), NAT: api.NATKindPortForward},
		CloudProvider: api.CloudProviderUnknown,
		Counter:       &counter,
		PacketConns:   []net.PacketConn{udp},
		Interfaces:    lister,
	})

	p443, p7882 := tcpPortOf(pm.ICE()), tcpPortOf(tr.tcpLn)
	ap := func(a string, port uint16) netip.AddrPort { return netip.AddrPortFrom(netip.MustParseAddr(a), port) }
	want := []AdvertisedAddr{
		{Proto: "udp", Addr: ap("192.168.1.20", 7882), Via: ViaUDP, LAN: true},
		{Proto: "udp", Addr: ap("198.51.100.9", 7882), Via: ViaUDP},
		{Proto: "tcp", Addr: ap("192.168.1.20", p443), Via: ViaTCP443, LAN: true},
		{Proto: "tcp", Addr: ap("198.51.100.9", p443), Via: ViaTCP443},
		{Proto: "tcp", Addr: ap("192.168.1.20", p7882), Via: ViaTCP7882, LAN: true},
		{Proto: "tcp", Addr: ap("198.51.100.9", p7882), Via: ViaTCP7882},
	}
	if !slices.Equal(tr.Advertised, want) {
		t.Errorf("Advertised =\n%+v\nwant\n%+v", tr.Advertised, want)
	}
	wantTypes := []webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeTCP4}
	if !slices.Equal(tr.NetworkTypes, wantTypes) {
		t.Errorf("NetworkTypes = %v, want %v", tr.NetworkTypes, wantTypes)
	}
	if len(tr.RewriteRules) != 1 || tr.RewriteRules[0].Mode != webrtc.ICEAddressRewriteAppend {
		t.Errorf("RewriteRules = %+v, want one Append rule", tr.RewriteRules)
	}
	for ip, keep := range map[string]bool{
		"192.168.1.20": true, "127.0.0.1": false, "172.17.0.1": false, "2001:db8::20": false, "10.1.1.1": false,
	} {
		if got := tr.IPFilter(net.ParseIP(ip)); got != keep {
			t.Errorf("IPFilter(%s) = %v, want %v", ip, got, keep)
		}
	}
	if tr.InterfaceFilter("docker0") || !tr.InterfaceFilter("eth0") {
		t.Error("InterfaceFilter does not apply exclude_interfaces")
	}
	if tr.UDPMux == nil || tr.TCPMux == nil || tr.TCPMux443 == nil || tr.TCPMux7882 == nil {
		t.Fatalf("muxes missing: %+v", tr)
	}
	if _, ok := tr.UDPMux.(*ice.MultiUDPMuxDefault); !ok {
		t.Errorf("UDPMux is %T", tr.UDPMux)
	}
	if _, ok := tr.TCPMux.(*ice.MultiTCPMuxDefault); !ok {
		t.Errorf("TCPMux is %T", tr.TCPMux)
	}
}

// TestTransportSharedICELimit: the 7882/tcp listener shares the per-IP ICE count with the 443 multiplexer, so the
// 65th ICE-TCP connection across both ports is closed.
func TestTransportSharedICELimit(t *testing.T) {
	pm := newTestPortMux(t, PortMuxOptions{})
	key := IPKey(netip.MustParseAddr("127.0.0.1"))
	for range DefaultMaxICEConnsPerIP - 1 { // 63 connections open on 443
		if !pm.iceConns.acquire(key) {
			t.Fatal("acquire refused")
		}
	}
	tr := newTestTransport(t, TransportOptions{
		TCPAddr: "127.0.0.1:0", PortMux: pm, IncludeLoopback: true, Interfaces: &fakeIfaces{ifs: []Interface{loIface()}},
	})
	if tr.tcpLn.limiter != pm.iceConns {
		t.Fatal("the 7882/tcp listener does not share PortMux's ICE count")
	}
	var d net.Dialer
	ctx := context.Background()
	first, err := d.DialContext(ctx, "tcp4", tr.tcpLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := d.DialContext(ctx, "tcp4", tr.tcpLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()

	// The 65th connection is closed by the server at once; the 64th waits for its first STUN request.
	_ = second.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := second.Read(make([]byte, 1)); err == nil {
		t.Error("the 65th connection was not closed")
	}
	if got := pm.iceConns.refused.Load(); got != 1 {
		t.Errorf("refused = %d, want 1", got)
	}
	if got := pm.Stats().Limited; got != 1 {
		t.Errorf("PortMux.Stats().Limited = %d, want 1 (the 7882/tcp refusal of the shared limit)", got)
	}
	if got := pm.iceConns.open(key); got != DefaultMaxICEConnsPerIP {
		t.Errorf("open = %d, want %d", got, DefaultMaxICEConnsPerIP)
	}
	_ = first.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := first.Read(make([]byte, 1)); !isTimeout(err) {
		t.Errorf("the 64th connection: read %v, want a timeout (still open)", err)
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// TestTransportPortZeroReuse: with port 0, the first address gets an ephemeral port and the others reuse it.
func TestTransportPortZeroReuse(t *testing.T) {
	var lc net.ListenConfig
	probe, err := lc.ListenPacket(context.Background(), "udp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback here: %v", err)
	}
	_ = probe.Close()
	tr := newTestTransport(t, TransportOptions{
		UDPAddr: ":0", IncludeLoopback: true, IPv6: true,
		Interfaces: &fakeIfaces{ifs: []Interface{loIface()}},
	})
	var got []netip.AddrPort
	for _, a := range tr.Advertised {
		got = append(got, a.Addr)
	}
	if len(got) != 2 || got[0].Addr() != netip.MustParseAddr("127.0.0.1") || got[1].Addr() != netip.MustParseAddr("::1") {
		t.Fatalf("Advertised = %v, want 127.0.0.1 and ::1", got)
	}
	if got[0].Port() == 0 || got[0].Port() != got[1].Port() {
		t.Errorf("ports %d and %d, want one ephemeral port for both", got[0].Port(), got[1].Port())
	}
	wantTypes := []webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6}
	if !slices.Equal(tr.NetworkTypes, wantTypes) {
		t.Errorf("NetworkTypes = %v, want %v", tr.NetworkTypes, wantTypes)
	}
	if tr.TCPMux != nil || tr.TCPMux443 != nil || tr.TCPMux7882 != nil {
		t.Error("TCP muxes exist without listen.ice_tcp and a PortMux")
	}
}

// warnLines returns the JSON log lines at level WARN.
func warnLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		if m["level"] == "WARN" {
			out = append(out, m)
		}
	}
	return out
}

// TestTransportBufferWarning: buffers that read back below network.udp_buffer_bytes give one warn line naming the
// sysctl fix; buffers that fit give none.
func TestTransportBufferWarning(t *testing.T) {
	lo := &fakeIfaces{ifs: []Interface{loIface()}}
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	const huge = 1 << 30 // more than any default net.core.rmem_max
	tr := newTestTransport(t, TransportOptions{
		UDPAddr: ":0", IncludeLoopback: true, UDPBufferBytes: huge, Interfaces: lo, Logger: log,
	})
	if tr.RcvBuf <= 0 || tr.SndBuf <= 0 || tr.RcvBuf >= huge {
		t.Fatalf("RcvBuf %d, SndBuf %d: want the effective sizes read back", tr.RcvBuf, tr.SndBuf)
	}
	warns := warnLines(t, &buf)
	if len(warns) != 1 {
		t.Fatalf("%d warn lines, want exactly one:\n%s", len(warns), buf.String())
	}
	w := warns[0]
	if w["component"] != "netx" || !strings.Contains(w["fix"].(string), "net.core.rmem_max") {
		t.Errorf("warn line %v: want component=netx and the sysctl fix", w)
	}

	buf.Reset()
	small := newTestTransport(t, TransportOptions{
		UDPAddr: "127.0.0.1:0", UDPBufferBytes: 1024, Interfaces: lo, Logger: log,
	})
	if small.RcvBuf < 1024 || small.SndBuf < 1024 {
		t.Errorf("RcvBuf %d, SndBuf %d, want ≥ 1024", small.RcvBuf, small.SndBuf)
	}
	if warns := warnLines(t, &buf); len(warns) != 0 {
		t.Errorf("warn lines for buffers that fit: %v", warns)
	}
}

func TestTransportErrors(t *testing.T) {
	lo := &fakeIfaces{ifs: []Interface{loIface()}}
	ctx := context.Background()

	if _, err := NewTransport(ctx, TransportOptions{Interfaces: lo}); !errors.Is(err, ErrNoTransport) {
		t.Errorf("nothing enabled: %v, want ErrNoTransport", err)
	}
	// Only loopback addresses and include_loopback off: no UDP address.
	if _, err := NewTransport(ctx, TransportOptions{UDPAddr: ":0", Interfaces: lo}); !errors.Is(err, ErrNoTransport) {
		t.Errorf("no usable address: %v, want ErrNoTransport", err)
	}
	if _, err := NewTransport(ctx, TransportOptions{UDPAddr: "localhost:7882", Interfaces: lo}); err == nil {
		t.Error("a host name in listen.ice_udp must be an error")
	}
	if _, err := NewTransport(ctx, TransportOptions{UDPAddr: "127.0.0.1", Interfaces: lo}); err == nil {
		t.Error("a listen address without a port must be an error")
	}
	if _, err := NewTransport(ctx, TransportOptions{UDPAddr: "127.0.0.1:0", ExcludeInterfaces: []string{"["},
		Interfaces: lo}); err == nil {
		t.Error("a malformed exclude glob must be an error")
	}
	broken := &fakeIfaces{err: errors.New("no interfaces for you")}
	if _, err := NewTransport(ctx, TransportOptions{UDPAddr: "127.0.0.1:0", Interfaces: broken}); err == nil {
		t.Error("an interface listing error must be returned")
	}

	// Busy ports are *ListenError wrapping EADDRINUSE; the UDP socket bound before the failure is released.
	busyUDP := listenUDP(t)
	_, err := NewTransport(ctx, TransportOptions{UDPAddr: busyUDP.LocalAddr().String(), Interfaces: lo})
	var le *ListenError
	if !errors.As(err, &le) || le.Proto != "udp" || !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("busy UDP port: %v, want a udp *ListenError with EADDRINUSE", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "listen udp "+busyUDP.LocalAddr().String()) {
		t.Errorf("error text %q does not name the protocol and address", msg)
	}
	var lc net.ListenConfig
	busyTCP, err := lc.Listen(ctx, "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busyTCP.Close() }()
	_, err = NewTransport(ctx, TransportOptions{UDPAddr: "127.0.0.1:0", TCPAddr: busyTCP.Addr().String(), Interfaces: lo})
	if !errors.As(err, &le) || le.Proto != "tcp" || !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("busy TCP port: %v, want a tcp *ListenError with EADDRINUSE", err)
	}
}

func TestTransportCloseIdempotent(t *testing.T) {
	pm := newTestPortMux(t, PortMuxOptions{})
	tr, err := NewTransport(context.Background(), TransportOptions{
		UDPAddr: "127.0.0.1:0", TCPAddr: "127.0.0.1:0", PortMux: pm, IncludeLoopback: true,
		Interfaces: &fakeIfaces{ifs: []Interface{loIface()}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// PortMux.Close after Transport.Close closed its ICE sub-listener is harmless.
	if err := pm.Close(); err != nil {
		t.Errorf("PortMux.Close after Transport.Close: %v", err)
	}
	// The ports are free again.
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp4", tr.tcpLn.Addr().String())
	if err != nil {
		t.Errorf("7882/tcp still bound after Close: %v", err)
	} else {
		_ = ln.Close()
	}
}

// TestQuietLogger: the muxes' per-connection warnings become debug lines; errors keep their level.
func TestQuietLogger(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	q := quietLogger{logx.NewPionLoggerFactory(log).NewLogger("ice")}
	q.Warnf("Error reading first packet from %s: %s", "192.0.2.1:5000", "EOF")
	q.Warn("Not a STUN message")
	q.Errorf("Failed to read UDP packet: %v", "boom")
	var levels []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		levels = append(levels, m["level"].(string))
	}
	if want := []string{"DEBUG", "DEBUG", "ERROR"}; !slices.Equal(levels, want) {
		t.Errorf("levels = %v, want %v", levels, want)
	}
}

// TestApplySetterAfterApplyWins: Apply only calls setters, so a later setter overrides it (02's probe APIs).
func TestApplySetterAfterApplyWins(t *testing.T) {
	tr := newTestTransport(t, TransportOptions{
		UDPAddr: "127.0.0.1:0", TCPAddr: "127.0.0.1:0", IncludeLoopback: true,
		Interfaces: &fakeIfaces{ifs: []Interface{loIface()}},
	})
	candidates := func(override func(*webrtc.SettingEngine)) (udp, tcp int) {
		var se webrtc.SettingEngine
		if err := tr.Apply(&se); err != nil {
			t.Fatal(err)
		}
		if override != nil {
			override(&se)
		}
		sdp := gatherOffer(t, webrtc.NewAPI(webrtc.WithSettingEngine(se)))
		for _, c := range sdpCandidates(sdp) {
			switch c.proto {
			case "udp":
				udp++
			case "tcp":
				tcp++
			}
		}
		return udp, tcp
	}
	if udp, tcp := candidates(nil); udp != 1 || tcp != 1 {
		t.Errorf("after Apply: %d UDP and %d TCP candidates, want 1 and 1", udp, tcp)
	}
	udpOnly := func(se *webrtc.SettingEngine) {
		se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
		se.SetICETCPMux(nil)
	}
	if udp, tcp := candidates(udpOnly); udp != 1 || tcp != 0 {
		t.Errorf("with a later setter: %d UDP and %d TCP candidates, want 1 and 0", udp, tcp)
	}
}
