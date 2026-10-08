package sfu

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/ice/v4"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/server/netx"
)

// The api_test of 02 §17 on a loopback netx.Transport (04): the sub offer and the pub answer are complete (every
// advertised candidate, UDP and passive TCP 7882, and end-of-candidates: no trickle), PCs on both APIs connect over
// UDP and over TCP 7882, the probe APIs each advertise only their transport, and the remote-candidate filter drops
// what 02 §7.3 lists. The TCP 443 candidates come from 04's real 443 multiplexer (netx.ListenPortMux on 127.0.0.1:0,
// TestCandidatesWithTCP443); a whole Conn negotiating and connecting through it is in negotiate_test.go.

// loopbackIfaces is a netx.InterfaceLister with only a loopback interface (127.0.0.1, and ::1 with v6), so the
// Transport's address plan doesn't depend on the host. (Pion still enumerates the host's interfaces itself; the
// Transport's IP filter keeps only the loopback addresses.)
type loopbackIfaces struct{ v6 bool }

func (l loopbackIfaces) Interfaces() ([]netx.Interface, error) {
	addrs := []netx.InterfaceAddr{{Addr: netip.MustParseAddr("127.0.0.1")}}
	if l.v6 {
		addrs = append(addrs, netx.InterfaceAddr{Addr: netip.MustParseAddr("::1")})
	}
	return []netx.Interface{{Name: "lo", Flags: net.FlagUp | net.FlagLoopback, Addrs: addrs}}, nil
}

func (loopbackIfaces) RouteSource(context.Context, netip.Addr) (netip.Addr, error) {
	return netip.Addr{}, errors.New("no route")
}

// loopbackTransport builds a netx.Transport on 127.0.0.1 with UDP and, with tcp, the 7882/tcp listener, each on an
// ephemeral port (the tests' stand-in for 7882), and closes it when the test ends (after its PCs, which are
// registered later).
func loopbackTransport(t *testing.T, tcp bool) *netx.Transport {
	t.Helper()
	opts := netx.TransportOptions{UDPAddr: "127.0.0.1:0", IncludeLoopback: true, Interfaces: loopbackIfaces{}}
	if tcp {
		opts.TCPAddr = "127.0.0.1:0"
	}
	tr, err := netx.NewTransport(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := tr.Close(); err != nil {
			t.Errorf("close transport: %v", err)
		}
	})
	return tr
}

func newTestAPIs(t *testing.T, tr *netx.Transport) *apis {
	t.Helper()
	a, err := newAPIs(tr, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// loopbackClientAPI is the far side: a plain Pion peer on loopback with Pion's default codecs that uses only network
// type nt (for TCP it dials the server's passive candidates: active ICE-TCP). profiles, if any, replace Pion's SRTP
// protection profiles.
func loopbackClientAPI(t *testing.T, nt webrtc.NetworkType, profiles ...dtls.SRTPProtectionProfile) *webrtc.API {
	t.Helper()
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	var se webrtc.SettingEngine
	se.SetNetworkTypes([]webrtc.NetworkType{nt})
	se.SetIncludeLoopbackCandidate(true)
	se.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	if len(profiles) > 0 {
		se.SetSRTPProtectionProfiles(profiles...)
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(se))
}

// sdpCand is one candidate of an SDP.
type sdpCand struct {
	proto   string // "udp" | "tcp"
	addr    netip.AddrPort
	typ     string // "host", "srflx", …
	tcpType string // "passive", "active" or ""
}

// String formats c for test failures ("tcp 127.0.0.1:7882 host passive"); fmt can't print the unexported fields'
// addresses itself.
func (c sdpCand) String() string {
	s := c.proto + " " + c.addr.String() + " " + c.typ
	if c.tcpType != "" {
		s += " " + c.tcpType
	}
	return s
}

// sdpCandidatesOf returns the component-1 candidates of an SDP without repeats (Pion lists them in every bundled
// m-section).
func sdpCandidatesOf(t *testing.T, raw string) []sdpCand {
	t.Helper()
	var out []sdpCand
	for line := range strings.Lines(raw) {
		v, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "a=candidate:")
		if !ok {
			continue
		}
		c, err := ice.UnmarshalCandidate(v)
		if err != nil {
			t.Fatalf("unparsable candidate in the SDP: %v", err)
		}
		if c.Component() != 1 {
			continue
		}
		ip, err := netip.ParseAddr(c.Address())
		if err != nil {
			t.Fatalf("candidate address %q: %v", c.Address(), err)
		}
		sc := sdpCand{
			proto:   c.NetworkType().NetworkShort(),
			addr:    netip.AddrPortFrom(ip.Unmap(), uint16(c.Port())),
			typ:     c.Type().String(),
			tcpType: c.TCPType().String(),
		}
		if sc.tcpType == ice.TCPTypeUnspecified.String() {
			sc.tcpType = ""
		}
		if !slices.Contains(out, sc) {
			out = append(out, sc)
		}
	}
	return out
}

// viaOfCand labels an SDP candidate the way the SFU labels a selected pair's local candidate.
func viaOfCand(a *apis, c sdpCand) string {
	proto := webrtc.ICEProtocolUDP
	if c.proto == "tcp" {
		proto = webrtc.ICEProtocolTCP
	}
	return a.via(webrtc.ICECandidate{Protocol: proto, Address: c.addr.Addr().String(), Port: c.addr.Port()})
}

// checkCompleteSDP checks a server description: it ends its candidates (gathering was complete, nothing is left to
// trickle), every candidate is a host candidate of tr.Advertised with a Via in wantVias (TCP ones passive: the server
// never dials out), and every advertised address of those transports is there.
func checkCompleteSDP(t *testing.T, a *apis, tr *netx.Transport, raw string, wantVias ...string) {
	t.Helper()
	if !strings.Contains(raw, "a=end-of-candidates") {
		t.Errorf("the description has no a=end-of-candidates:\n%s", raw)
	}
	cands := sdpCandidatesOf(t, raw)
	gotVias := map[string]bool{}
	for _, c := range cands {
		via := viaOfCand(a, c)
		switch {
		case c.typ != "host":
			t.Errorf("candidate %v is not a host candidate", c)
		case c.proto == "tcp" && c.tcpType != "passive":
			t.Errorf("TCP candidate %v is not passive", c)
		case !slices.ContainsFunc(tr.Advertised, func(adv netx.AdvertisedAddr) bool {
			return adv.Proto == c.proto && adv.Addr == c.addr
		}):
			t.Errorf("candidate %v is not in Advertised %+v", c, tr.Advertised)
		case !slices.Contains(wantVias, via):
			t.Errorf("candidate %v is on transport %q, want only %v", c, via, wantVias)
		}
		gotVias[via] = true
	}
	for _, adv := range tr.Advertised {
		if !slices.Contains(wantVias, adv.Via) {
			continue
		}
		if !slices.ContainsFunc(cands, func(c sdpCand) bool { return c.proto == adv.Proto && c.addr == adv.Addr }) {
			t.Errorf("advertised %s %v (%s) is missing from the description", adv.Proto, adv.Addr, adv.Via)
		}
	}
	for _, via := range wantVias {
		if !gotVias[via] {
			t.Errorf("no %s candidate in the description:\n%s", via, raw)
		}
	}
}

// completeDescription applies desc on pc with setLocalComplete and checks that gathering completed at once.
func completeDescription(t *testing.T, pc *webrtc.PeerConnection, desc webrtc.SessionDescription) string {
	t.Helper()
	start := time.Now()
	sdp, complete, err := setLocalComplete(context.Background(), pc, desc)
	if err != nil {
		t.Fatal(err)
	}
	if !complete {
		t.Errorf("gathering did not complete within %v", gatherTimeout)
	}
	if d := time.Since(start); d > gatherTimeout/2 {
		t.Errorf("gathering took %v; with the muxes it is instant", d)
	}
	return sdp
}

func opusTrack(t *testing.T, id string) *webrtc.TrackLocalStaticRTP {
	t.Helper()
	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, id, id)
	if err != nil {
		t.Fatal(err)
	}
	return track
}

func h264Track(t *testing.T, id string) *webrtc.TrackLocalStaticRTP {
	t.Helper()
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: h264FmtpPrefix + "42e01f",
	}, "v-"+id, id)
	if err != nil {
		t.Fatal(err)
	}
	return track
}

func addSendonly(t *testing.T, pc *webrtc.PeerConnection, tracks ...webrtc.TrackLocal) {
	t.Helper()
	for _, track := range tracks {
		init := webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly}
		if _, err := pc.AddTransceiverFromTrack(track, init); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSubOfferCandidates: a sub offer (the SFU offers) holds the UDP and passive TCP 7882 candidates of the
// Transport and ends its candidates, with gathering done at once: the SFU never trickles.
func TestSubOfferCandidates(t *testing.T) {
	tr := loopbackTransport(t, true)
	a := newTestAPIs(t, tr)
	pc := newTestPC(t, a.sub)
	addSendonly(t, pc, h264Track(t, "s_sub"), opusTrack(t, "a-s_sub"))
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	sdp := completeDescription(t, pc, offer)
	checkCompleteSDP(t, a, tr, sdp, netx.ViaUDP, netx.ViaTCP7882)
	if n := strings.Count(sdp, "\r\nm="); n != 2 {
		t.Errorf("%d m-sections in the sub offer, want 2", n)
	}
}

// TestPubAnswerCandidates: a pub answer (the client offers) is complete in the same way.
func TestPubAnswerCandidates(t *testing.T) {
	tr := loopbackTransport(t, true)
	a := newTestAPIs(t, tr)
	client := newTestPC(t, loopbackClientAPI(t, webrtc.NetworkTypeUDP4))
	addSendonly(t, client, h264Track(t, "s_pub"), opusTrack(t, "a-s_pub"))
	offer, err := client.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	offerSDP := completeDescription(t, client, offer)

	pc := newTestPC(t, a.pub)
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offerSDP}); err != nil {
		t.Fatal(err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	checkCompleteSDP(t, a, tr, completeDescription(t, pc, answer), netx.ViaUDP, netx.ViaTCP7882)
}

// TestConnectThroughTransport connects PCs of both APIs to a Pion client over loopback, over UDP and over TCP 7882
// (the client dials the passive candidate), with complete SDP both ways and the server's remote description passed
// through the candidate filter as a Conn does. Media flows (SRTP) and the selected pair carries the right label.
func TestConnectThroughTransport(t *testing.T) {
	tr := loopbackTransport(t, true)
	a := newTestAPIs(t, tr)
	host, prflx := webrtc.ICECandidateTypeHost, webrtc.ICECandidateTypePrflx
	for _, tc := range []struct {
		name         string
		serverOffers bool // sub PC; otherwise pub PC
		nt           webrtc.NetworkType
		profiles     []dtls.SRTPProtectionProfile
		filter       candidateFilter
		wantVia      string
		wantRemote   webrtc.ICECandidateType // 0: not checked (a TCP remote is always learned from the connection)
	}{
		{
			name: "sub/udp", serverOffers: true, nt: webrtc.NetworkTypeUDP4, filter: a.filter,
			wantVia: netx.ViaUDP, wantRemote: host,
		},
		{name: "sub/tcp7882", serverOffers: true, nt: webrtc.NetworkTypeTCP4, filter: a.filter, wantVia: netx.ViaTCP7882},
		{name: "pub/udp", nt: webrtc.NetworkTypeUDP4, filter: a.filter, wantVia: netx.ViaUDP, wantRemote: host},
		{name: "pub/tcp7882", nt: webrtc.NetworkTypeTCP4, filter: a.filter, wantVia: netx.ViaTCP7882},
		// The server's first SRTP choice is AES-GCM; a client that only has the fallback still connects.
		{
			name: "sub/udp/srtp-fallback", serverOffers: true, nt: webrtc.NetworkTypeUDP4, filter: a.filter,
			profiles: []dtls.SRTPProtectionProfile{dtls.SRTP_AES128_CM_HMAC_SHA1_80}, wantVia: netx.ViaUDP,
		},
		// A client with AES-128-GCM only connects too: on sub the SFU is the DTLS server (it answers the client's
		// profile list), on pub the DTLS client (it offers its own).
		{
			name: "sub/udp/srtp-gcm-only", serverOffers: true, nt: webrtc.NetworkTypeUDP4, filter: a.filter,
			profiles: []dtls.SRTPProtectionProfile{dtls.SRTP_AEAD_AES_128_GCM}, wantVia: netx.ViaUDP,
		},
		{
			name: "pub/udp/srtp-gcm-only", nt: webrtc.NetworkTypeUDP4, filter: a.filter,
			profiles: []dtls.SRTPProtectionProfile{dtls.SRTP_AEAD_AES_128_GCM}, wantVia: netx.ViaUDP,
		},
		// Every client candidate is filtered out (loopback without include_loopback): the server still learns the
		// client's address as a peer-reflexive candidate from its checks, and connects.
		{
			name: "sub/udp/all-filtered", serverOffers: true, nt: webrtc.NetworkTypeUDP4,
			wantVia: netx.ViaUDP, wantRemote: prflx,
		},
		{name: "pub/udp/all-filtered", nt: webrtc.NetworkTypeUDP4, wantVia: netx.ViaUDP, wantRemote: prflx},
		{name: "pub/tcp7882/all-filtered", nt: webrtc.NetworkTypeTCP4, wantVia: netx.ViaTCP7882},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pair := connectToClient(t, a, tc.serverOffers, loopbackClientAPI(t, tc.nt, tc.profiles...), tc.filter)
			if got := a.via(*pair.Local); got != tc.wantVia {
				t.Errorf("selected local candidate %s %s:%d labeled %q, want %q",
					pair.Local.Protocol, pair.Local.Address, pair.Local.Port, got, tc.wantVia)
			}
			if tc.wantRemote != 0 && pair.Remote.Typ != tc.wantRemote {
				t.Errorf("selected remote candidate is %s, want %s", pair.Remote.Typ, tc.wantRemote)
			}
		})
	}
}

// TestSRTPProfilesRestricted: a client that only offers AES-256-GCM (Pion's own first choice, not in the SFU's list)
// can't complete DTLS with the SFU.
func TestSRTPProfilesRestricted(t *testing.T) {
	tr := loopbackTransport(t, false)
	a := newTestAPIs(t, tr)
	server := newTestPC(t, a.sub)
	client := newTestPC(t, loopbackClientAPI(t, webrtc.NetworkTypeUDP4, dtls.SRTP_AEAD_AES_256_GCM))
	addSendonly(t, server, opusTrack(t, "a-s_srtp"))
	iceConnected := make(chan struct{})
	var once sync.Once
	server.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		if s == webrtc.ICEConnectionStateConnected {
			once.Do(func() { close(iceConnected) })
		}
	})
	// A failed handshake ends in failed, or in closed when the peer's alert closes the DTLS transport (Pion then
	// closes the PC).
	state := firstState(server, webrtc.PeerConnectionStateConnected, webrtc.PeerConnectionStateFailed,
		webrtc.PeerConnectionStateClosed)
	negotiate(t, server, client, true, a.filter)
	select {
	case s := <-state:
		if s == webrtc.PeerConnectionStateConnected {
			t.Fatal("the SFU connected with a client that has no SRTP profile of its list")
		}
		// Pion runs every state callback in a goroutine of its own, so on a busy machine the one for ICE connected
		// can still be on its way when the one for the failure has arrived.
		select {
		case <-iceConnected: // ICE worked; DTLS-SRTP is what failed
		case <-time.After(2 * time.Second):
			t.Error("the PC failed before ICE connected")
		}
	case <-time.After(dtlsConnectTimeout + 5*time.Second):
		t.Fatalf("no failure: server %s", server.ConnectionState())
	}
}

// firstState returns a channel that receives the first of states pc reaches. Set it before negotiating; it
// replaces pc's OnConnectionStateChange handler.
func firstState(pc *webrtc.PeerConnection, states ...webrtc.PeerConnectionState) <-chan webrtc.PeerConnectionState {
	ch := make(chan webrtc.PeerConnectionState, 1)
	var once sync.Once
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if slices.Contains(states, s) {
			once.Do(func() { ch <- s })
		}
	})
	return ch
}

// negotiate runs one offer/answer between the server PC and the client with complete SDP both ways. The server's
// remote description goes through filter first, as a Conn does (filterSDP before SetRemoteDescription).
func negotiate(t *testing.T, server, client *webrtc.PeerConnection, serverOffers bool, filter candidateFilter) {
	t.Helper()
	offerer, answerer := client, server
	if serverOffers {
		offerer, answerer = server, client
	}
	offer, err := offerer.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	offerSDP, _, err := setLocalComplete(context.Background(), offerer, offer)
	if err != nil {
		t.Fatal(err)
	}
	if !serverOffers {
		if offerSDP, _, err = filter.filterSDP(offerSDP); err != nil {
			t.Fatal(err)
		}
	}
	if err := answerer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offerSDP}); err != nil {
		t.Fatal(err)
	}
	answer, err := answerer.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	answerSDP, _, err := setLocalComplete(context.Background(), answerer, answer)
	if err != nil {
		t.Fatal(err)
	}
	if serverOffers {
		if answerSDP, _, err = filter.filterSDP(answerSDP); err != nil {
			t.Fatal(err)
		}
	}
	if err := offerer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answerSDP}); err != nil {
		t.Fatal(err)
	}
}

// connectToClient connects a server PC (sub if serverOffers, else pub) to a client PC on clientAPI, sends Opus RTP
// from the sending side (the server on sub, the client on pub) until the other side receives a packet, and returns
// the server's selected candidate pair.
func connectToClient(t *testing.T, a *apis, serverOffers bool, clientAPI *webrtc.API, filter candidateFilter,
) *webrtc.ICECandidatePair {
	t.Helper()
	serverAPI := a.pub
	if serverOffers {
		serverAPI = a.sub
	}
	server := newTestPC(t, serverAPI)
	client := newTestPC(t, clientAPI)
	sender, receiver := client, server
	if serverOffers {
		sender, receiver = server, client
	}
	track := opusTrack(t, "a-s_conn")
	addSendonly(t, sender, track)
	received := make(chan struct{})
	var once sync.Once
	receiver.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			if _, _, err := tr.ReadRTP(); err != nil {
				return
			}
			once.Do(func() { close(received) })
		}
	})
	state := firstState(server, webrtc.PeerConnectionStateConnected, webrtc.PeerConnectionStateFailed)
	negotiate(t, server, client, serverOffers, filter)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	wg.Go(func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		p := &rtp.Packet{Header: rtp.Header{Version: 2}, Payload: []byte{0xf8, 0xff, 0xfe}}
		for {
			select {
			case <-ctx.Done():
				return
			case <-received:
				return
			case <-tick.C:
			}
			p.SequenceNumber++
			p.Timestamp += 960
			_ = track.WriteRTP(p) // errors before DTLS is up are expected
		}
	})
	select {
	case s := <-state:
		if s != webrtc.PeerConnectionStateConnected {
			t.Fatalf("server PC %s (ICE %s), client %s (ICE %s)", s, server.ICEConnectionState(),
				client.ConnectionState(), client.ICEConnectionState())
		}
	case <-ctx.Done():
		t.Fatalf("not connected: server %s (ICE %s), client %s (ICE %s)", server.ConnectionState(),
			server.ICEConnectionState(), client.ConnectionState(), client.ICEConnectionState())
	}
	select {
	case <-received:
	case <-ctx.Done():
		t.Fatal("no media arrived")
	}
	pair, err := server.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil || pair == nil {
		t.Fatalf("no selected pair: %v", err)
	}
	return pair
}

// TestProbeAPIs: one probe API per transport that has a mux (no 443 without a PortMux), each with only its own
// transport's candidates, and a probe PC answering a data-channel offer connects on it.
func TestProbeAPIs(t *testing.T) {
	tr := loopbackTransport(t, true)
	a := newTestAPIs(t, tr)
	if got := slices.Sorted(maps.Keys(a.probe)); !slices.Equal(got, []ProbeTransport{ProbeTCP7882, ProbeUDP}) {
		t.Fatalf("probe APIs %v, want udp and tcp7882", got)
	}
	for _, tc := range []struct {
		pt ProbeTransport
		nt webrtc.NetworkType
	}{{ProbeUDP, webrtc.NetworkTypeUDP4}, {ProbeTCP7882, webrtc.NetworkTypeTCP4}} {
		t.Run(string(tc.pt), func(t *testing.T) {
			api := a.probe[tc.pt]
			// The probe's own offer (a data channel) holds only its transport's candidates.
			pc := newTestPC(t, api)
			if _, err := pc.CreateDataChannel("probe", nil); err != nil {
				t.Fatal(err)
			}
			offer, err := pc.CreateOffer(nil)
			if err != nil {
				t.Fatal(err)
			}
			checkCompleteSDP(t, a, tr, completeDescription(t, pc, offer), string(tc.pt))

			// A browser offers a data channel; the probe PC answers with complete SDP and connects.
			server := newTestPC(t, api)
			client := newTestPC(t, loopbackClientAPI(t, tc.nt))
			if _, err := client.CreateDataChannel("probe", nil); err != nil {
				t.Fatal(err)
			}
			state := firstState(server, webrtc.PeerConnectionStateConnected, webrtc.PeerConnectionStateFailed)
			offer, err = client.CreateOffer(nil)
			if err != nil {
				t.Fatal(err)
			}
			offerSDP := completeDescription(t, client, offer)
			if err := server.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offerSDP}); err != nil {
				t.Fatal(err)
			}
			answer, err := server.CreateAnswer(nil)
			if err != nil {
				t.Fatal(err)
			}
			answerSDP := completeDescription(t, server, answer)
			checkCompleteSDP(t, a, tr, answerSDP, string(tc.pt))
			if err := client.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answerSDP}); err != nil {
				t.Fatal(err)
			}
			select {
			case s := <-state:
				if s != webrtc.PeerConnectionStateConnected {
					t.Fatalf("probe PC %s (ICE %s)", s, server.ICEConnectionState())
				}
			case <-time.After(20 * time.Second):
				t.Fatalf("probe not connected: %s (ICE %s)", server.ConnectionState(), server.ICEConnectionState())
			}
			pair, err := server.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
			if err != nil || pair == nil {
				t.Fatalf("no selected pair: %v", err)
			}
			if got := a.via(*pair.Local); got != string(tc.pt) {
				t.Errorf("selected pair labeled %q, want %q", got, tc.pt)
			}
		})
	}
}

// TestCandidatesWithTCP443: on a Transport that has 04's 443 multiplexer (netx.ListenPortMux on 127.0.0.1:0, as
// tls.mode != off gives it), a sub offer and a pub answer hold the UDP candidate and passive TCP candidates on 443 and
// on 7882, complete and without trickle, and there is a third probe API whose offer holds only the 443 candidate.
func TestCandidatesWithTCP443(t *testing.T) {
	pm, err := netx.ListenPortMux(netx.PortMuxOptions{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pm.Close(); err != nil {
			t.Errorf("close the 443 multiplexer: %v", err)
		}
	})
	tr, err := netx.NewTransport(context.Background(), netx.TransportOptions{
		UDPAddr: "127.0.0.1:0", TCPAddr: "127.0.0.1:0", PortMux: pm, IncludeLoopback: true, Interfaces: loopbackIfaces{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := tr.Close(); err != nil {
			t.Errorf("close transport: %v", err)
		}
	})
	a := newTestAPIs(t, tr)
	all := []string{netx.ViaUDP, netx.ViaTCP443, netx.ViaTCP7882}

	sub := newTestPC(t, a.sub)
	addSendonly(t, sub, h264Track(t, "s_443"), opusTrack(t, "a-s_443"))
	offer, err := sub.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	subOffer := completeDescription(t, sub, offer)
	checkCompleteSDP(t, a, tr, subOffer, all...)
	port443 := pm.Addr().(*net.TCPAddr).AddrPort().Port()
	want := sdpCand{
		proto: "tcp", addr: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port443), typ: "host", tcpType: "passive",
	}
	if !slices.Contains(sdpCandidatesOf(t, subOffer), want) {
		t.Errorf("the sub offer has no candidate %v on the multiplexer's port:\n%v", want, sdpCandidatesOf(t, subOffer))
	}

	client := newTestPC(t, loopbackClientAPI(t, webrtc.NetworkTypeUDP4))
	addSendonly(t, client, h264Track(t, "s_pub443"), opusTrack(t, "a-s_pub443"))
	offer, err = client.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	pub := newTestPC(t, a.pub)
	remote := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: completeDescription(t, client, offer)}
	if err := pub.SetRemoteDescription(remote); err != nil {
		t.Fatal(err)
	}
	answer, err := pub.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	checkCompleteSDP(t, a, tr, completeDescription(t, pub, answer), all...)

	if got := slices.Sorted(maps.Keys(a.probe)); !slices.Equal(got, []ProbeTransport{ProbeTCP443, ProbeTCP7882, ProbeUDP}) {
		t.Fatalf("probe APIs %v, want all three", got)
	}
	for _, pt := range []ProbeTransport{ProbeUDP, ProbeTCP443, ProbeTCP7882} {
		pc := newTestPC(t, a.probe[pt])
		if _, err := pc.CreateDataChannel("probe", nil); err != nil {
			t.Fatal(err)
		}
		offer, err := pc.CreateOffer(nil)
		if err != nil {
			t.Fatal(err)
		}
		checkCompleteSDP(t, a, tr, completeDescription(t, pc, offer), string(pt))
	}
}

// TestProbeAPIsIPv4Only: on a dual-stack Transport whose IPv4 address isn't a LAN one (here 127.0.0.1 and ::1),
// the sub API advertises both families, and each probe API only its IPv4 candidates, the UDP one included (Pion
// gathers on every address of a UDP mux, whatever the network types). Pion gathers no TCP candidate on ::1 (it takes
// it for an IPv4-compatible address), so IPv6 shows here on UDP only.
func TestProbeAPIsIPv4Only(t *testing.T) {
	tr, err := netx.NewTransport(context.Background(), netx.TransportOptions{
		UDPAddr: ":0", TCPAddr: ":0", IncludeLoopback: true, IPv6: true, Interfaces: loopbackIfaces{v6: true},
	})
	if err != nil {
		t.Skipf("no dual-stack loopback here: %v", err)
	}
	t.Cleanup(func() {
		if err := tr.Close(); err != nil {
			t.Errorf("close transport: %v", err)
		}
	})
	var v4, v6 []sdpCand
	for _, adv := range tr.Advertised {
		c := sdpCand{proto: adv.Proto, addr: adv.Addr}
		if adv.Addr.Addr().Is4() {
			v4 = append(v4, c)
		} else {
			v6 = append(v6, c)
		}
	}
	if len(v4) != 2 || len(v6) != 1 {
		t.Skipf("no dual-stack loopback here: Advertised %+v", tr.Advertised)
	}
	a := newTestAPIs(t, tr)

	pc := newTestPC(t, a.sub)
	addSendonly(t, pc, opusTrack(t, "a-s_dual"))
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	subCands := sdpCandidatesOf(t, completeDescription(t, pc, offer))
	for _, want := range append(slices.Clone(v4), v6[slices.IndexFunc(v6, func(c sdpCand) bool { return c.proto == "udp" })]) {
		if !slices.ContainsFunc(subCands, func(c sdpCand) bool { return c.proto == want.proto && c.addr == want.addr }) {
			t.Errorf("sub offer: no %s %v candidate in %v", want.proto, want.addr, subCands)
		}
	}

	for _, pt := range []ProbeTransport{ProbeUDP, ProbeTCP7882} {
		pc := newTestPC(t, a.probe[pt])
		if _, err := pc.CreateDataChannel("probe", nil); err != nil {
			t.Fatal(err)
		}
		offer, err := pc.CreateOffer(nil)
		if err != nil {
			t.Fatal(err)
		}
		sdp := completeDescription(t, pc, offer)
		var got []string
		for _, c := range sdpCandidatesOf(t, sdp) {
			got = append(got, c.proto+" "+c.addr.String())
		}
		var want []string
		for _, c := range v4 {
			if (c.proto == "udp") == (pt == ProbeUDP) {
				want = append(want, c.proto+" "+c.addr.String())
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s probe candidates %v, want %v", pt, got, want)
		}
	}
}

// listenUDPMux is a UDP mux with fixed listen addresses that records Close.
type listenUDPMux struct {
	ice.UDPMux
	addrs  []net.Addr
	closed bool
}

func (m *listenUDPMux) GetListenAddresses() []net.Addr { return m.addrs }
func (m *listenUDPMux) Close() error                   { m.closed = true; return nil }

// TestIPv4UDPMux: the IPv4-only probe's mux lists only the IPv4 sockets and never closes the Transport's mux.
func TestIPv4UDPMux(t *testing.T) {
	v4 := &net.UDPAddr{IP: net.ParseIP("203.0.113.7"), Port: 7882}
	inner := &listenUDPMux{addrs: []net.Addr{
		v4,
		&net.UDPAddr{IP: net.ParseIP("2001:db8::7"), Port: 7882},
		&net.TCPAddr{IP: net.ParseIP("198.51.100.9"), Port: 7882},
	}}
	m := ipv4UDPMux{inner}
	if got := m.GetListenAddresses(); len(got) != 1 || got[0] != v4 {
		t.Errorf("GetListenAddresses = %v, want only %v", got, v4)
	}
	if err := m.Close(); err != nil || inner.closed {
		t.Errorf("Close: %v; the Transport's mux closed: %v", err, inner.closed)
	}
}

// TestProbeAPIsFollowMuxes: a transport without a mux gets no probe API (SFU.Probe answers transport_disabled).
func TestProbeAPIsFollowMuxes(t *testing.T) {
	a := newTestAPIs(t, loopbackTransport(t, false))
	if got := slices.Sorted(maps.Keys(a.probe)); !slices.Equal(got, []ProbeTransport{ProbeUDP}) {
		t.Fatalf("probe APIs %v without 7882/tcp, want only udp", got)
	}
	if _, err := probeAPI(&netx.Transport{}, nil, "tcp9999"); err == nil {
		t.Error("an unknown probe transport must be an error")
	}
	for _, pt := range []ProbeTransport{ProbeUDP, ProbeTCP443, ProbeTCP7882} {
		if api, err := probeAPI(&netx.Transport{}, nil, pt); api != nil || err != nil {
			t.Errorf("%s without a mux: %v, %v; want no API", pt, api, err)
		}
	}
}

func TestProbeNetworkTypes(t *testing.T) {
	all := []webrtc.NetworkType{
		webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6, webrtc.NetworkTypeTCP4, webrtc.NetworkTypeTCP6,
	}
	adv := func(addr string, lan bool) netx.AdvertisedAddr {
		return netx.AdvertisedAddr{Proto: "udp", Addr: netip.MustParseAddrPort(addr), Via: netx.ViaUDP, LAN: lan}
	}
	nt := func(s ...webrtc.NetworkType) []webrtc.NetworkType { return s }
	for _, tc := range []struct {
		name     string
		types    []webrtc.NetworkType
		adv      []netx.AdvertisedAddr
		udp, tcp []webrtc.NetworkType
	}{
		{
			name: "public IPv4: IPv4 only", types: all,
			adv: []netx.AdvertisedAddr{adv("203.0.113.7:7882", false), adv("[2001:db8::7]:7882", false)},
			udp: nt(webrtc.NetworkTypeUDP4), tcp: nt(webrtc.NetworkTypeTCP4),
		},
		{
			name: "home server (Append): the public IPv4 counts", types: all,
			adv: []netx.AdvertisedAddr{adv("192.168.1.20:7882", true), adv("198.51.100.9:7882", false)},
			udp: nt(webrtc.NetworkTypeUDP4), tcp: nt(webrtc.NetworkTypeTCP4),
		},
		{
			name: "LAN-only IPv4 and global IPv6: both families", types: all,
			adv: []netx.AdvertisedAddr{adv("192.168.1.20:7882", true), adv("[2001:db8::7]:7882", false)},
			udp: nt(webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6), tcp: nt(webrtc.NetworkTypeTCP4, webrtc.NetworkTypeTCP6),
		},
		{
			name: "IPv6-only server", types: nt(webrtc.NetworkTypeUDP6, webrtc.NetworkTypeTCP6),
			adv: []netx.AdvertisedAddr{adv("[2001:db8::7]:7882", false)},
			udp: nt(webrtc.NetworkTypeUDP6), tcp: nt(webrtc.NetworkTypeTCP6),
		},
		{
			name: "IPv4-mapped advertised address counts as IPv4", types: all,
			adv: []netx.AdvertisedAddr{adv("[::ffff:203.0.113.7]:7882", false)},
			udp: nt(webrtc.NetworkTypeUDP4), tcp: nt(webrtc.NetworkTypeTCP4),
		},
		{
			name:  "public IPv4 but only IPv6 types: none (no probe API)",
			types: nt(webrtc.NetworkTypeUDP6, webrtc.NetworkTypeTCP6),
			adv:   []netx.AdvertisedAddr{adv("203.0.113.7:7882", false)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := &netx.Transport{NetworkTypes: tc.types, Advertised: tc.adv}
			if got := probeNetworkTypes(tr, true); !slices.Equal(got, tc.udp) {
				t.Errorf("udp: %v, want %v", got, tc.udp)
			}
			if got := probeNetworkTypes(tr, false); !slices.Equal(got, tc.tcp) {
				t.Errorf("tcp: %v, want %v", got, tc.tcp)
			}
		})
	}
}

func TestVia(t *testing.T) {
	a := &apis{
		advertised: []netx.AdvertisedAddr{
			{Proto: "udp", Addr: netip.MustParseAddrPort("203.0.113.7:7882"), Via: netx.ViaUDP},
			{Proto: "tcp", Addr: netip.MustParseAddrPort("203.0.113.7:443"), Via: netx.ViaTCP443},
			{Proto: "tcp", Addr: netip.MustParseAddrPort("203.0.113.7:7882"), Via: netx.ViaTCP7882},
			{Proto: "udp", Addr: netip.MustParseAddrPort("[2001:db8::7]:7882"), Via: netx.ViaUDP},
		},
		log: slog.New(slog.DiscardHandler),
	}
	udp, tcp := webrtc.ICEProtocolUDP, webrtc.ICEProtocolTCP
	for _, tc := range []struct {
		proto webrtc.ICEProtocol
		addr  string
		port  uint16
		want  string
	}{
		{udp, "203.0.113.7", 7882, netx.ViaUDP},
		{tcp, "203.0.113.7", 443, netx.ViaTCP443},
		{tcp, "203.0.113.7", 7882, netx.ViaTCP7882},
		{udp, "2001:db8::7", 7882, netx.ViaUDP},
		{tcp, "::ffff:203.0.113.7", 443, netx.ViaTCP443}, // IPv4-mapped
		{udp, "198.51.100.1", 5000, netx.ViaUDP},         // no match: UDP has one path
		{tcp, "198.51.100.1", 443, ""},                   // no match: 443 or 7882 can't be told
		{tcp, "203.0.113.7", 9, ""},
		{tcp, "not-an-ip", 443, ""},
	} {
		c := webrtc.ICECandidate{Protocol: tc.proto, Address: tc.addr, Port: tc.port}
		if got := a.via(c); got != tc.want {
			t.Errorf("via(%s %s:%d) = %q, want %q", tc.proto, tc.addr, tc.port, got, tc.want)
		}
	}
}

func TestProbeTransportValues(t *testing.T) {
	// ProbeTransport values are netx's Via labels (and so 04's api.Transport values).
	for pt, via := range map[ProbeTransport]string{
		ProbeUDP: netx.ViaUDP, ProbeTCP443: netx.ViaTCP443, ProbeTCP7882: netx.ViaTCP7882,
	} {
		if string(pt) != via {
			t.Errorf("ProbeTransport %q != Via %q", pt, via)
		}
	}
}

func TestNewAPIsNeedsTransport(t *testing.T) {
	if _, err := newAPIs(nil, nil); err == nil {
		t.Fatal("newAPIs without a Transport must fail")
	}
}

// TestCandidateFilterAddrs is the remote-candidate filter table of 02 §7.3 (01 §17), for the three server setups
// that change it: the default (a public server), network.include_loopback (development) and a server that
// advertises a private address itself (04's LAN/Append case). Each advertises the addresses in own.
func TestCandidateFilterAddrs(t *testing.T) {
	own := []netip.Addr{
		netip.MustParseAddr("198.51.100.9"), netip.MustParseAddr("2001:db8::9"), netip.MustParseAddr("192.168.1.30"),
	}
	var (
		public   = candidateFilter{own: own}
		loopback = candidateFilter{loopback: true, own: own}
		lan      = candidateFilter{private: true, own: own}
	)
	for _, tc := range []struct {
		addr                  string
		public, loopback, lan dropReason
	}{
		// Public addresses: always kept.
		{"203.0.113.7", keepCandidate, keepCandidate, keepCandidate},
		{"2001:db8::7", keepCandidate, keepCandidate, keepCandidate},
		{"100.128.0.1", keepCandidate, keepCandidate, keepCandidate}, // just above CGNAT
		{"172.32.0.1", keepCandidate, keepCandidate, keepCandidate},  // just above 172.16.0.0/12
		{"::ffff:203.0.113.7", keepCandidate, keepCandidate, keepCandidate},
		{"64:ff9b::cb00:7107", keepCandidate, keepCandidate, keepCandidate}, // NAT64 of 203.0.113.7
		// Loopback: only with include_loopback.
		{"127.0.0.1", dropLoopback, keepCandidate, dropLoopback},
		{"127.8.9.10", dropLoopback, keepCandidate, dropLoopback},
		{"::1", dropLoopback, keepCandidate, dropLoopback},
		{"::ffff:127.0.0.1", dropLoopback, keepCandidate, dropLoopback},
		{"64:ff9b::7f00:1", dropLoopback, keepCandidate, dropLoopback},
		// RFC 1918, CGNAT, ULA and site-local: only when the server is on a LAN itself.
		{"10.1.2.3", dropPrivate, dropPrivate, keepCandidate},
		{"172.16.0.1", dropPrivate, dropPrivate, keepCandidate},
		{"172.31.255.254", dropPrivate, dropPrivate, keepCandidate},
		{"192.168.1.20", dropPrivate, dropPrivate, keepCandidate},
		{"100.64.0.1", dropPrivate, dropPrivate, keepCandidate},
		{"100.127.255.254", dropPrivate, dropPrivate, keepCandidate},
		{"fd12:3456::1", dropPrivate, dropPrivate, keepCandidate},
		{"fc00::1", dropPrivate, dropPrivate, keepCandidate},
		{"fec0::1", dropPrivate, dropPrivate, keepCandidate},
		{"::ffff:10.0.0.1", dropPrivate, dropPrivate, keepCandidate},
		{"64:ff9b::a00:1", dropPrivate, dropPrivate, keepCandidate}, // NAT64 of 10.0.0.1
		// The server's own addresses, on any port: only with include_loopback (checks to them stay on the host).
		{"198.51.100.9", dropOwnAddr, keepCandidate, dropOwnAddr},
		{"::ffff:198.51.100.9", dropOwnAddr, keepCandidate, dropOwnAddr},
		{"64:ff9b::c633:6409", dropOwnAddr, keepCandidate, dropOwnAddr}, // NAT64 of 198.51.100.9
		{"2001:db8::9", dropOwnAddr, keepCandidate, dropOwnAddr},
		{"192.168.1.30", dropPrivate, dropPrivate, dropOwnAddr},
		{"198.51.100.10", keepCandidate, keepCandidate, keepCandidate},
		{"2001:db8::a", keepCandidate, keepCandidate, keepCandidate},
		// Always dropped.
		{"169.254.1.1", dropLinkLocal, dropLinkLocal, dropLinkLocal},
		{"fe80::1", dropLinkLocal, dropLinkLocal, dropLinkLocal},
		{"fe80::1%eth0", dropLinkLocal, dropLinkLocal, dropLinkLocal},
		{"224.0.0.1", dropMulticast, dropMulticast, dropMulticast},
		{"239.255.255.250", dropMulticast, dropMulticast, dropMulticast},
		{"ff02::1", dropMulticast, dropMulticast, dropMulticast},
		{"ff0e::1", dropMulticast, dropMulticast, dropMulticast},
		{"0.0.0.0", dropUnspecified, dropUnspecified, dropUnspecified},
		{"0.1.2.3", dropUnspecified, dropUnspecified, dropUnspecified},
		{"::", dropUnspecified, dropUnspecified, dropUnspecified},
		{"255.255.255.255", dropBroadcast, dropBroadcast, dropBroadcast},
		// IPv4-compatible IPv6 (::/96, deprecated): never, whatever IPv4 address is inside (a sit tunnel reaches it).
		{"::a00:1", dropIPv4Compat, dropIPv4Compat, dropIPv4Compat},     // ::10.0.0.1
		{"::c0a8:114", dropIPv4Compat, dropIPv4Compat, dropIPv4Compat},  // ::192.168.1.20
		{"::cb00:7107", dropIPv4Compat, dropIPv4Compat, dropIPv4Compat}, // ::203.0.113.7
		{"::7f00:1", dropIPv4Compat, dropIPv4Compat, dropIPv4Compat},    // ::127.0.0.1
		{"::c633:6409", dropIPv4Compat, dropIPv4Compat, dropIPv4Compat}, // ::198.51.100.9 (own)
		{"::2", dropIPv4Compat, dropIPv4Compat, dropIPv4Compat},
		{"::1:0:0", keepCandidate, keepCandidate, keepCandidate}, // just above ::/96 (and not ::ffff:0:0/96)
	} {
		a := netip.MustParseAddr(tc.addr)
		for _, f := range []struct {
			name string
			f    candidateFilter
			want dropReason
		}{{"public", public, tc.public}, {"include_loopback", loopback, tc.loopback}, {"lan", lan, tc.lan}} {
			if got := f.f.addr(a); got != f.want {
				t.Errorf("%s server, %s: %q, want %q", f.name, tc.addr, got, f.want)
			}
		}
	}
	if got := (candidateFilter{loopback: true, private: true}).addr(netip.Addr{}); got != dropUnparsable {
		t.Errorf("the zero Addr: %q, want %q", got, dropUnparsable)
	}
}

// TestCandidateFilterCandidates: the filter reads a candidate as Pion does (with or without the "candidate:" prefix)
// and judges only its connection address.
func TestCandidateFilterCandidates(t *testing.T) {
	f := candidateFilter{own: []netip.Addr{netip.MustParseAddr("192.0.2.10")}} // a public server on 192.0.2.10
	for _, tc := range []struct {
		cand string
		want dropReason
	}{
		{"candidate:1 1 udp 2122260223 203.0.113.7 54321 typ host", keepCandidate},
		{"1 1 udp 2122260223 203.0.113.7 54321 typ host generation 0 ufrag abcd", keepCandidate},
		{"candidate:1 1 UDP 2122260223 2001:db8::7 54321 typ host", keepCandidate},
		{"candidate:2 1 udp 1686052607 203.0.113.7 54321 typ srflx raddr 192.168.1.20 rport 54321", keepCandidate},
		{"candidate:3 1 udp 41885439 198.51.100.9 3478 typ relay raddr 203.0.113.7 rport 54321", keepCandidate},
		{"candidate:4 1 tcp 1518280447 203.0.113.7 9 typ host tcptype active", keepCandidate},
		{"candidate:5 1 tcp 1518280447 203.0.113.7 443 typ host tcptype passive", keepCandidate},
		{"candidate:6 1 udp 2122260223 192.168.1.20 54321 typ host", dropPrivate},
		{"candidate:7 1 tcp 1518280447 10.0.0.5 9 typ host tcptype active", dropPrivate},
		{"candidate:8 1 udp 1686052607 100.64.1.2 54321 typ srflx raddr 0.0.0.0 rport 0", dropPrivate},
		{"candidate:9 1 udp 2122260223 127.0.0.1 54321 typ host", dropLoopback},
		{"candidate:10 1 udp 2122260223 fe80::1%en0 54321 typ host", dropLinkLocal},
		{"candidate:11 1 udp 2122260223 169.254.3.4 54321 typ host", dropLinkLocal},
		{"candidate:12 1 udp 2122260223 239.1.2.3 54321 typ host", dropMulticast},
		{"candidate:13 1 udp 2122260223 0.0.0.0 54321 typ host", dropUnspecified},
		{"candidate:14 1 udp 2122260223 5b4e0f3a-8b2c-4a0e-9d1a-0c6a2e1f7b3d.local 54321 typ host", dropHostname},
		{"candidate:15 1 udp 2122260223 media.example.com 54321 typ host", dropUnparsable},
		{"candidate:16 1 udp 2122260223 203.0.113.7 54321 typ bogus", dropUnparsable},
		{"candidate:17 1 udp 1 192.0.2.10 11211 typ host", dropOwnAddr},
		{"candidate:18 1 tcp 1518280447 192.0.2.10 9 typ host tcptype active", dropOwnAddr},
		{"candidate:19 1 udp 2122260223 ::a00:1 54321 typ host", dropIPv4Compat},
		{"candidate:garbage", dropUnparsable},
		{"", dropUnparsable},
	} {
		if got := f.candidate(tc.cand); got != tc.want {
			t.Errorf("%q: %q, want %q", tc.cand, got, tc.want)
		}
	}
}

func TestNewCandidateFilter(t *testing.T) {
	lanAdv := []netx.AdvertisedAddr{
		{Proto: "udp", Addr: netip.MustParseAddrPort("192.168.1.20:7882"), Via: netx.ViaUDP, LAN: true},
		{Proto: "udp", Addr: netip.MustParseAddrPort("198.51.100.9:7882"), Via: netx.ViaUDP},
		{Proto: "tcp", Addr: netip.MustParseAddrPort("192.168.1.20:7882"), Via: netx.ViaTCP7882, LAN: true},
	}
	publicAdv := []netx.AdvertisedAddr{
		{Proto: "udp", Addr: netip.MustParseAddrPort("203.0.113.7:7882"), Via: netx.ViaUDP},
		{Proto: "tcp", Addr: netip.MustParseAddrPort("[::ffff:203.0.113.7]:443"), Via: netx.ViaTCP443},
		{Proto: "udp", Addr: netip.MustParseAddrPort("[2001:db8::7]:7882"), Via: netx.ViaUDP},
	}
	lanOwn := []netip.Addr{netip.MustParseAddr("192.168.1.20"), netip.MustParseAddr("198.51.100.9")}
	publicOwn := []netip.Addr{netip.MustParseAddr("203.0.113.7"), netip.MustParseAddr("2001:db8::7")}
	for _, tc := range []struct {
		tr   *netx.Transport
		want candidateFilter
	}{
		{&netx.Transport{Advertised: publicAdv}, candidateFilter{own: publicOwn}},
		{
			&netx.Transport{Advertised: publicAdv, IncludeLoopback: true},
			candidateFilter{loopback: true, own: publicOwn},
		},
		{&netx.Transport{Advertised: lanAdv}, candidateFilter{private: true, own: lanOwn}},
		{
			&netx.Transport{Advertised: lanAdv, IncludeLoopback: true},
			candidateFilter{loopback: true, private: true, own: lanOwn},
		},
		{&netx.Transport{}, candidateFilter{}},
	} {
		got := newCandidateFilter(tc.tr)
		if got.loopback != tc.want.loopback || got.private != tc.want.private || !slices.Equal(got.own, tc.want.own) {
			t.Errorf("Transport %+v: %+v, want %+v", tc.tr.Advertised, got, tc.want)
		}
	}
}

// TestFilterSDP: a client's SDP loses exactly the candidates the filter drops, at session and media level, read as
// Pion's SDP parser reads them.
func TestFilterSDP(t *testing.T) {
	const in = "v=0\r\n" +
		"o=- 1 2 IN IP4 127.0.0.1\r\n" +
		"s=-\r\n" +
		"t=0 0\r\n" +
		"a=candidate:0 1 udp 2122260223 10.9.8.7 5000 typ host\r\n" +
		"a=group:BUNDLE 0\r\n" +
		"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
		"c=IN IP4 0.0.0.0\r\n" +
		"a=mid:0\r\n" +
		"a=candidate:1 1 udp 2122260223 203.0.113.7 54321 typ host\r\n" +
		"a=candidate:2 1 udp 2122260223 192.168.1.20 54322 typ host\r\n" +
		"a=candidate:3 1 tcp 1518280447 127.0.0.1 9 typ host tcptype active\r\n" +
		"a=candidate:4 1 udp 1686052607 198.51.100.4 54323 typ srflx raddr 192.168.1.20 rport 54322\r\n" +
		"a=candidate:5 1 udp 2122260223 abc.local 54324 typ host\r\n" +
		"a=end-of-candidates\r\n" +
		"a=rtpmap:111 opus/48000/2\r\n"
	const want = "v=0\r\n" +
		"o=- 1 2 IN IP4 127.0.0.1\r\n" +
		"s=-\r\n" +
		"t=0 0\r\n" +
		"a=group:BUNDLE 0\r\n" +
		"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
		"c=IN IP4 0.0.0.0\r\n" +
		"a=mid:0\r\n" +
		"a=candidate:1 1 udp 2122260223 203.0.113.7 54321 typ host\r\n" +
		"a=candidate:4 1 udp 1686052607 198.51.100.4 54323 typ srflx raddr 192.168.1.20 rport 54322\r\n" +
		"a=end-of-candidates\r\n" +
		"a=rtpmap:111 opus/48000/2\r\n"
	public := candidateFilter{}
	check := func(name, in, want string, wantDropped int) {
		t.Helper()
		got, n, err := public.filterSDP(in)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			return
		}
		if got != want || n != wantDropped {
			t.Errorf("%s: dropped %d:\n%q\nwant %d:\n%q", name, n, got, wantDropped, want)
		}
	}
	check("CRLF", in, want, 4)
	// A changed SDP is re-marshaled (CRLF).
	lf := func(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
	check("LF", lf(in), want, 4)

	// Candidates hidden in odd line breaks, as pion/sdp reads them: after CRs before a line's type, and after a lone
	// CR that ends a t= line.
	const endOfCands = "a=end-of-candidates\r\n"
	const private6 = "a=candidate:6 1 udp 2122260223 10.0.0.6 5006 typ host\r\n"
	const public6 = "a=candidate:6 1 udp 2122260223 203.0.113.6 5006 typ host\r\n"
	check("CRs before the type", strings.Replace(in, endOfCands, "\r\r"+private6+"\r\n"+endOfCands, 1), want, 5)
	check("CRs before the type, kept", strings.Replace(in, endOfCands, "\r"+public6+endOfCands, 1),
		strings.Replace(want, endOfCands, public6+endOfCands, 1), 4)
	check("lone CR after t=", strings.Replace(in, "t=0 0\r\n", "t=0 0\r"+private6, 1), want, 5)
	// A CR inside a line: pion/sdp reads the value "…generation 0\r" (it cuts one more character for each CR in a
	// line), which no valid candidate has.
	const cand4 = "a=candidate:4 1 udp 1686052607 198.51.100.4 54323 typ srflx raddr 192.168.1.20 rport 54322\r\n"
	check("CR inside", strings.Replace(in, cand4, strings.TrimSuffix(cand4, "\r\n")+" generation 0\rX\r\n", 1),
		strings.Replace(want, cand4, "", 1), 5)
	// Two CRs at the end: pion/sdp reads the clean value, which is kept.
	check("CR CR LF", strings.Replace(in, cand4, strings.TrimSuffix(cand4, "\n")+"\r\n", 1), want, 4)
	// Attributes pion/sdp reads as a candidate without a value: dropped. One that only starts like one: kept.
	for _, odd := range []string{"a=candidate\r\n", "a=candidate\r\r\n", "a=candidate:\r\n"} {
		check(fmt.Sprintf("odd %q", odd), strings.Replace(in, endOfCands, odd+endOfCands, 1), want, 5)
	}
	const candidates = "a=candidates:1 1 udp 1 10.0.0.1 1 typ host\r\n"
	check("a=candidates", strings.Replace(in, endOfCands, candidates+endOfCands, 1),
		strings.Replace(want, endOfCands, candidates+endOfCands, 1), 4)

	// Nothing dropped: the same string back, even with LF line endings.
	lan := candidateFilter{loopback: true, private: true}
	onlyKept := lf(strings.ReplaceAll(in, "a=candidate:5 1 udp 2122260223 abc.local 54324 typ host\r\n", ""))
	if got, n, err := lan.filterSDP(onlyKept); err != nil || got != onlyKept || n != 0 {
		t.Errorf("a LAN server with include_loopback dropped %d (%v):\n%s", n, err, got)
	}
	const noCands = "v=0\r\no=- 1 2 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\n"
	if got, n, err := public.filterSDP(noCands); err != nil || got != noCands || n != 0 {
		t.Errorf("an SDP without candidates: %q, %d, %v", got, n, err)
	}

	// sfu.bad_sdp: what pion/sdp can't parse (SetRemoteDescription would refuse it too), and a malformed line that
	// pion/sdp would read back as a candidate after the re-marshaling ("candidate\r" is no candidate; written back as
	// "a=candidate\r\r\n", it is one).
	for _, bad := range []string{
		"", "garbage", "a=end-of-candidates\r\n", "v=0\r\ns=-\r\n",
		strings.Replace(in, endOfCands, "a=candidate\r0\r\n"+endOfCands, 1),
	} {
		_, _, err := public.filterSDP(bad)
		wantCode(t, fmt.Sprintf("filterSDP(%.40q)", bad), err, CodeBadSDP, "")
	}
}

// TestFilterSDPOnPionDescriptions: a real client offer filtered for a public server loses all its (loopback)
// candidates and Pion still applies it.
func TestFilterSDPOnPionDescriptions(t *testing.T) {
	a := newTestAPIs(t, loopbackTransport(t, true))
	client := newTestPC(t, loopbackClientAPI(t, webrtc.NetworkTypeUDP4))
	addSendonly(t, client, h264Track(t, "s_pion"), opusTrack(t, "a-s_pion"))
	offer, err := client.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	offerSDP := completeDescription(t, client, offer)
	if len(sdpCandidatesOf(t, offerSDP)) == 0 {
		t.Fatal("the client offer has no candidates")
	}
	filtered, n, err := candidateFilter{}.filterSDP(offerSDP)
	if err != nil || n == 0 {
		t.Fatalf("filterSDP: %d dropped, %v", n, err)
	}
	if strings.Contains(filtered, "a=candidate:") {
		t.Errorf("a loopback candidate is left:\n%s", filtered)
	}
	pc := newTestPC(t, a.pub)
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: filtered}); err != nil {
		t.Fatalf("Pion refuses the filtered offer: %v", err)
	}
}

// FuzzFilterSDP: whatever a client sends, either it is sfu.bad_sdp, or Pion's SDP parser reads the filtered SDP and
// finds no candidate in it that the filter would drop and none that the server must not probe, and filtering again
// changes nothing.
func FuzzFilterSDP(f *testing.F) {
	f.Add("v=0\r\no=- 1 2 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\nc=IN IP4 0.0.0.0\r\n" +
		"a=mid:0\r\na=candidate:1 1 udp 2122260223 203.0.113.7 54321 typ host\r\n" +
		"a=candidate:2 1 udp 2122260223 192.168.1.20 54322 typ host\r\n\r\ra=candidate:3 1 udp 1 127.0.0.1 1 typ host\r\n")
	f.Add("v=0\no=- 1 2 IN IP4 0.0.0.0\ns=-\nt=0 0\na=candidate:0 1 tcp 1 10.0.0.1 9 typ host tcptype active\n" +
		"m=video 9 UDP/TLS/RTP/SAVPF 96\na=mid:v\na=candidate:1 1 udp 1 fe80::1%en0 1 typ host generation 0\rX\n")
	f.Add("v=0\r\ns=-\r\n")
	f.Fuzz(func(t *testing.T, raw string) {
		for _, filter := range []candidateFilter{{}, {loopback: true}, {private: true}} {
			out, n, err := filter.filterSDP(raw)
			if err != nil {
				var e *Error
				if !errors.As(err, &e) || e.Code != CodeBadSDP {
					t.Fatalf("error %v is not sfu.bad_sdp", err)
				}
				continue
			}
			if n == 0 && out != raw {
				t.Fatal("nothing dropped, but the SDP changed")
			}
			desc := &sdp.SessionDescription{}
			if err := desc.UnmarshalString(out); err != nil {
				t.Fatalf("pion/sdp can't read the filtered SDP: %v", err)
			}
			attrs := slices.Clone(desc.Attributes)
			for _, md := range desc.MediaDescriptions {
				attrs = append(attrs, md.Attributes...)
			}
			for _, a := range attrs {
				if !a.IsICECandidate() {
					continue
				}
				if r := filter.candidate(a.Value); r != keepCandidate {
					t.Fatalf("a candidate the filter drops (%s) is left", r)
				}
				c, err := ice.UnmarshalCandidate(a.Value)
				if err != nil {
					t.Fatalf("a kept candidate doesn't parse: %v", err)
				}
				ip, err := netip.ParseAddr(c.Address())
				if err != nil {
					t.Fatalf("a kept candidate has no IP address: %v", err)
				}
				ip = ip.Unmap()
				if ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() ||
					(ip.IsLoopback() && !filter.loopback) || (ip.IsPrivate() && !filter.private) ||
					(ip.Is6() && netip.MustParsePrefix("::/96").Contains(ip) && !ip.IsLoopback()) {
					t.Fatalf("a kept candidate has the address %v (filter %+v)", ip, filter)
				}
			}
			if again, n2, err := filter.filterSDP(out); err != nil || n2 != 0 || again != out {
				t.Fatalf("filtering twice changed the SDP: %d dropped, %v", n2, err)
			}
		}
	})
}

// TestCandidateFilterTrickled: a trickled candidate is read the way Pion's AddICECandidate reads it.
func TestCandidateFilterTrickled(t *testing.T) {
	f := candidateFilter{}
	for _, tc := range []struct {
		cand string
		want dropReason
	}{
		{"candidate:1 1 udp 2122260223 203.0.113.7 54321 typ host", keepCandidate},
		{"candidate:1 1 udp 2122260223 10.0.0.1 54321 typ host", dropPrivate},
		// Pion strips "candidate:" twice (AddICECandidate, then UnmarshalCandidate).
		{"candidate:candidate:1 1 udp 2122260223 10.0.0.1 54321 typ host", dropPrivate},
		{"1 1 udp 2122260223 127.0.0.1 54321 typ host", dropLoopback},
		{"candidate:1 1 udp 2122260223 203.0.113.7 54321 typ host\r", dropUnparsable},
		// The end-of-candidates marker: Pion ignores it.
		{"", keepCandidate},
		{"candidate:", keepCandidate},
	} {
		init := webrtc.ICECandidateInit{Candidate: tc.cand}
		if got := f.trickled(init); got != tc.want {
			t.Errorf("%q: %q, want %q", tc.cand, got, tc.want)
		}
	}
}

// FuzzCandidateFilter: a trickled candidate the filter keeps for a public server is one Pion parses, with an IP
// address a public server may probe (not its own).
func FuzzCandidateFilter(f *testing.F) {
	for _, s := range []string{
		"candidate:1 1 udp 2122260223 203.0.113.7 54321 typ host",
		"candidate:2 1 udp 1686052607 203.0.113.7 54321 typ srflx raddr 192.168.1.20 rport 54321",
		"candidate:3 1 tcp 1518280447 10.0.0.5 9 typ host tcptype active",
		"candidate:4 1 udp 2122260223 64:ff9b::a00:1 54321 typ host",
		"candidate:5 1 udp 2122260223 abc.local 54321 typ host",
		"candidate:candidate:6 1 udp 2122260223 ::ffff:127.0.0.1 1 typ host",
		"candidate:7 1 udp 2122260223 ::a00:1 54321 typ host",
		"candidate:8 1 udp 1 198.51.100.9 11211 typ host",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		own := netip.MustParseAddr("198.51.100.9")
		public := candidateFilter{own: []netip.Addr{own}}
		v := strings.TrimPrefix(s, "candidate:")
		if public.trickled(webrtc.ICECandidateInit{Candidate: s}) != keepCandidate || v == "" {
			return
		}
		c, err := ice.UnmarshalCandidate(v) // as Pion's AddICECandidate
		if err != nil {
			t.Fatalf("kept, but Pion can't parse it: %v", err)
		}
		ip, err := netip.ParseAddr(c.Address())
		if err != nil {
			t.Fatalf("kept, but %q is no IP address", c.Address())
		}
		ip = ip.Unmap()
		if ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLoopback() || ip.IsPrivate() ||
			cgnatPrefix.Contains(ip) || (ip.Is6() && netip.MustParsePrefix("::/96").Contains(ip)) || ip == own {
			t.Fatalf("kept the address %v", ip)
		}
	})
}
