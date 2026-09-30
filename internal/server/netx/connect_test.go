package netx

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// sdpCandidate is one a=candidate line of an SDP.
type sdpCandidate struct {
	proto string
	addr  netip.AddrPort
	typ   string
}

// sdpCandidates returns the component-1 candidates of an SDP (Pion repeats each one as component 2).
func sdpCandidates(sdp string) []sdpCandidate {
	var out []sdpCandidate
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "a=candidate:") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 8 || f[1] != "1" {
			continue
		}
		ip, err1 := netip.ParseAddr(f[4])
		port, err2 := strconv.ParseUint(f[5], 10, 16)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, sdpCandidate{proto: strings.ToLower(f[2]), addr: netip.AddrPortFrom(ip.Unmap(), uint16(port)), typ: f[7]})
	}
	return out
}

// gatherOffer creates a PeerConnection with one data channel on api, gathers all its candidates and returns the
// offer SDP (non-trickle).
func gatherOffer(t *testing.T, api *webrtc.API) string {
	t.Helper()
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	if _, err := pc.CreateDataChannel("probe", nil); err != nil {
		t.Fatal(err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gathered:
	case <-time.After(10 * time.Second):
		t.Fatal("gathering did not complete")
	}
	return pc.LocalDescription().SDP
}

// clientAPI is the far side: a plain Pion peer on loopback that only uses the given network type (for TCP it
// dials the server's passive candidates, active ICE-TCP).
func clientAPI(nt webrtc.NetworkType) *webrtc.API {
	var se webrtc.SettingEngine
	se.SetNetworkTypes([]webrtc.NetworkType{nt})
	se.SetIncludeLoopbackCandidate(true)
	se.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	return webrtc.NewAPI(webrtc.WithSettingEngine(se))
}

// connectPair connects a server PeerConnection built on tr to a Pion client over loopback: the server offers a
// data channel (non-trickle, like 02's sub PC), the client echoes, and the server gets its message back. It
// returns the server's selected local candidate.
func connectPair(t *testing.T, tr *Transport, client *webrtc.API) webrtc.ICECandidate {
	t.Helper()
	var se webrtc.SettingEngine
	if err := tr.Apply(&se); err != nil {
		t.Fatal(err)
	}
	server, err := webrtc.NewAPI(webrtc.WithSettingEngine(se)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()
	peer, err := client.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = peer.Close() }()

	peer.OnDataChannel(func(dc *webrtc.DataChannel) {
		dc.OnMessage(func(m webrtc.DataChannelMessage) { _ = dc.Send(m.Data) })
	})
	dc, err := server.CreateDataChannel("probe", nil)
	if err != nil {
		t.Fatal(err)
	}
	echo := make(chan []byte, 1)
	payload := bytes.Repeat([]byte("isshoni "), 64)
	dc.OnOpen(func() { _ = dc.Send(payload) })
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		select {
		case echo <- m.Data:
		default:
		}
	})

	offer, err := server.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(server)
	if err := server.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gathered
	offerSDP := server.LocalDescription().SDP
	checkAdvertised(t, tr, offerSDP)

	if err := peer.SetRemoteDescription(*server.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	answer, err := peer.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	peerGathered := webrtc.GatheringCompletePromise(peer)
	if err := peer.SetLocalDescription(answer); err != nil {
		t.Fatal(err)
	}
	<-peerGathered
	if err := server.SetRemoteDescription(*peer.LocalDescription()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	select {
	case got := <-echo:
		if !bytes.Equal(got, payload) {
			t.Fatalf("echo differs: %d bytes", len(got))
		}
	case <-ctx.Done():
		t.Fatalf("no data channel echo: server ICE %s, peer ICE %s",
			server.ICEConnectionState(), peer.ICEConnectionState())
	}
	pair, err := server.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil || pair == nil {
		t.Fatalf("no selected pair: %v", err)
	}
	return *pair.Local
}

// checkAdvertised checks that every candidate of the server's SDP is one of tr.Advertised.
func checkAdvertised(t *testing.T, tr *Transport, sdp string) {
	t.Helper()
	cands := sdpCandidates(sdp)
	if len(cands) == 0 {
		t.Fatalf("the server offer has no candidates:\n%s", sdp)
	}
	for _, c := range cands {
		if c.typ != "host" {
			t.Errorf("candidate %+v is not a host candidate", c)
		}
		if viaOf(tr, c.proto, c.addr) == "" {
			t.Errorf("candidate %s %v is not in Advertised %+v", c.proto, c.addr, tr.Advertised)
		}
	}
}

// viaOf labels a local candidate from Advertised, as 02 does for selected pairs.
func viaOf(tr *Transport, proto string, addr netip.AddrPort) string {
	for _, a := range tr.Advertised {
		if a.Proto == proto && a.Addr == addr {
			return a.Via
		}
	}
	return ""
}

func candidateAddr(t *testing.T, c webrtc.ICECandidate) netip.AddrPort {
	t.Helper()
	ip, err := netip.ParseAddr(c.Address)
	if err != nil {
		t.Fatal(err)
	}
	return netip.AddrPortFrom(ip.Unmap(), c.Port)
}

// TestConnectUDP is the acceptance check: a Pion PeerConnection pair connects through the Transport's UDP mux on
// loopback, and the TransferCounter counts the media_udp bytes both ways.
func TestConnectUDP(t *testing.T) {
	var counter TransferCounter
	tr := newTestTransport(t, TransportOptions{
		UDPAddr: "127.0.0.1:0", TCPAddr: "127.0.0.1:0", IncludeLoopback: true, UDPBufferBytes: 1 << 20,
		Counter: &counter,
	})
	local := connectPair(t, tr, clientAPI(webrtc.NetworkTypeUDP4))
	if local.Protocol != webrtc.ICEProtocolUDP {
		t.Fatalf("selected local candidate %+v, want UDP", local)
	}
	if via := viaOf(tr, "udp", candidateAddr(t, local)); via != ViaUDP {
		t.Errorf("selected pair labeled %q, want %q", via, ViaUDP)
	}
	totals := counter.Totals()
	if u := totals[PathMediaUDP]; u.Egress == 0 || u.Ingress == 0 {
		t.Errorf("media_udp = %+v, want bytes both ways", u)
	}
	if tcp := totals[PathMediaTCP]; tcp.Egress != 0 || tcp.Ingress != 0 {
		t.Errorf("media_tcp = %+v, want nothing on a UDP connection", tcp)
	}
}

// TestConnectTCP7882 is the acceptance check over ICE-TCP: the client dials the passive 7882/tcp candidate, and
// the TransferCounter counts the media_tcp bytes both ways.
func TestConnectTCP7882(t *testing.T) {
	var counter TransferCounter
	tr := newTestTransport(t, TransportOptions{
		UDPAddr: "127.0.0.1:0", TCPAddr: "127.0.0.1:0", IncludeLoopback: true, Counter: &counter,
	})
	local := connectPair(t, tr, clientAPI(webrtc.NetworkTypeTCP4))
	if local.Protocol != webrtc.ICEProtocolTCP {
		t.Fatalf("selected local candidate %+v, want TCP", local)
	}
	if via := viaOf(tr, "tcp", candidateAddr(t, local)); via != ViaTCP7882 {
		t.Errorf("selected pair labeled %q, want %q", via, ViaTCP7882)
	}
	if local.Port != tcpPortOf(tr.tcpLn) {
		t.Errorf("selected local port %d, want the 7882/tcp listener's %d", local.Port, tcpPortOf(tr.tcpLn))
	}
	totals := counter.Totals()
	if c := totals[PathMediaTCP]; c.Egress == 0 || c.Ingress == 0 {
		t.Errorf("media_tcp = %+v, want bytes both ways", c)
	}
}

// TestConnectTCP443 connects through the 443 multiplexer's ICE side (TCPMux443): the client dials the passive
// candidate on the mux's port, the mux routes its RFC 4571 frames to ICE() and counts the media_tcp bytes both ways
// (the Transport does not count them again).
func TestConnectTCP443(t *testing.T) {
	var counter TransferCounter
	pm := newTestPortMux(t, PortMuxOptions{Counter: &counter})
	tr := newTestTransport(t, TransportOptions{PortMux: pm, IncludeLoopback: true, Counter: &counter})
	if tr.UDPMux != nil || tr.TCPMux7882 != nil || tr.TCPMux443 == nil {
		t.Fatalf("muxes: udp %v, 7882 %v, 443 %v", tr.UDPMux, tr.TCPMux7882, tr.TCPMux443)
	}
	local := connectPair(t, tr, clientAPI(webrtc.NetworkTypeTCP4))
	if via := viaOf(tr, "tcp", candidateAddr(t, local)); via != ViaTCP443 {
		t.Errorf("selected pair labeled %q, want %q", via, ViaTCP443)
	}
	if local.Port != tcpPortOf(pm.ICE()) {
		t.Errorf("selected local port %d, want the mux's %d", local.Port, tcpPortOf(pm.ICE()))
	}
	st := pm.Stats()
	if st.ICE == 0 || st.TLS != 0 || st.Garbage != 0 || st.Limited != 0 {
		t.Errorf("Stats = %+v, want only ICE connections", st)
	}
	totals := counter.Totals()
	if c := totals[PathMediaTCP]; c.Egress == 0 || c.Ingress == 0 {
		t.Errorf("media_tcp = %+v, want bytes both ways", c)
	}
	if w := totals[PathWeb]; w.Egress != 0 || w.Ingress != 0 {
		t.Errorf("web = %+v, want nothing on an ICE connection", w)
	}
}

// TestAdvertisedMatchesPion: with a rewrite rule (a container behind 1:1 NAT, loopback standing in for the
// bridge address), the candidates Pion puts in an offer are exactly Transport.Advertised.
func TestAdvertisedMatchesPion(t *testing.T) {
	tr := newTestTransport(t, TransportOptions{
		UDPAddr: "127.0.0.1:0", TCPAddr: "127.0.0.1:0", IncludeLoopback: true, InContainer: true,
		Public: PublicAddrs{V4: netip.MustParseAddr("198.51.100.9"), V4Method: MethodSTUN,
			LocalV4: netip.MustParseAddr("127.0.0.1"), NAT: api.NATKindOneToOne},
		Interfaces: &fakeIfaces{ifs: []Interface{loIface()}},
	})
	if len(tr.RewriteRules) != 1 || tr.RewriteRules[0].Mode != webrtc.ICEAddressRewriteReplace {
		t.Fatalf("RewriteRules = %+v, want one Replace rule", tr.RewriteRules)
	}
	var se webrtc.SettingEngine
	if err := tr.Apply(&se); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range sdpCandidates(gatherOffer(t, webrtc.NewAPI(webrtc.WithSettingEngine(se)))) {
		got = append(got, c.proto+" "+c.addr.String())
	}
	var want []string
	for _, a := range tr.Advertised {
		if a.Addr.Addr() != netip.MustParseAddr("198.51.100.9") {
			t.Errorf("Advertised %+v: want only the public address", a)
		}
		want = append(want, a.Proto+" "+a.Addr.String())
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("offer candidates %v, Advertised %v", got, want)
	}
}

// TestCloseWithSilentTCPClient: a client that connects to 7882/tcp and never sends its first STUN request does not
// hold Transport.Close for pion's 10 s first-request timeout.
func TestCloseWithSilentTCPClient(t *testing.T) {
	tr, err := NewTransport(context.Background(), TransportOptions{TCPAddr: "127.0.0.1:0", IncludeLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	closeWithSilentClient(t, tr, tr.tcpLn, nil)
}

// TestCloseWithSilent443Client: the same on 443. The client sends only the first byte of an RFC 4571 header, so
// the mux hands it to pion, which then waits for the rest of its first frame.
func TestCloseWithSilent443Client(t *testing.T) {
	pm := newTestPortMux(t, PortMuxOptions{})
	tr, err := NewTransport(context.Background(), TransportOptions{
		PortMux: pm, IncludeLoopback: true, Interfaces: &fakeIfaces{ifs: []Interface{loIface()}},
	})
	if err != nil {
		t.Fatal(err)
	}
	closeWithSilentClient(t, tr, tr.ln443, []byte{0x00})
}

// closeWithSilentClient connects a client to ln that sends first (it may be nil) and then nothing, waits until ln
// has handed it to pion, then checks that Transport.Close returns quickly and closes the client's connection.
func closeWithSilentClient(t *testing.T, tr *Transport, ln *iceListener, first []byte) {
	t.Helper()
	var d net.Dialer
	c, err := d.DialContext(context.Background(), "tcp4", ln.Addr().String())
	if err != nil {
		_ = tr.Close()
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Write(first); err != nil {
		_ = tr.Close()
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ln.handedOut() == 0; {
		if time.Now().After(deadline) {
			_ = tr.Close()
			t.Fatal("the connection was not accepted")
		}
		time.Sleep(5 * time.Millisecond)
	}
	start := time.Now()
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Close took %v with a silent ICE-TCP client", d)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil || isTimeout(err) {
		t.Errorf("the silent client read %v, want the connection closed", err)
	}
}
