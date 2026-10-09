package sfu

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/ice/v4"
	"github.com/pion/logging"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/server/netx"
)

// This file builds the SFU's webrtc.APIs on 04's netx.Transport (02 §7.1, §7.3, §7.6): the publish and subscribe
// APIs every Conn's PeerConnections use, and one connection-test probe API per ICE transport. It also holds the
// remote-candidate filter, the selected-pair label and the complete-SDP helper (the server never trickles).

// ProbeTransport names one ICE transport of the connection test (02 §7.6). The values are netx's Via labels, the
// same set used everywhere (dashboard, metrics, the selected-pair label). probe.go has ProbeResult and SFU.Probe,
// which README S76 implements on the probe APIs built here.
type ProbeTransport string

// The three probe transports. Each is unavailable when its mux is nil: udp with listen.ice_udp = "", tcp443 in
// tls.mode=off, tcp7882 with listen.ice_tcp = "".
const (
	ProbeUDP     ProbeTransport = "udp"
	ProbeTCP443  ProbeTransport = "tcp443"
	ProbeTCP7882 ProbeTransport = "tcp7882"
)

// ICE and DTLS settings (02 §7.3, §7.6, §12).
const (
	// The client drives restarts after 3 s of ICE disconnected; the server only cleans up.
	iceDisconnectedTimeout = 5 * time.Second
	iceFailedTimeout       = 15 * time.Second
	iceKeepaliveInterval   = 2 * time.Second
	// A probe PC lives at most 20 s, so it gives up sooner.
	probeDisconnectedTimeout = 3 * time.Second
	probeFailedTimeout       = 8 * time.Second
	// When the SFU is controlling (the sub PC), browser pairs are prflx. Pion's default 1 s wait delays the first
	// frame; 300 ms still lets a UDP pair win over TCP.
	prflxAcceptanceMinWait = 300 * time.Millisecond
	// The plan's guard on the DTLS handshake.
	dtlsConnectTimeout = 10 * time.Second
	// gatherTimeout caps the wait for ICE gathering before a local description goes out (02 §5.3). Gathering is
	// instant with the muxes.
	gatherTimeout = 2 * time.Second
)

// srtpProfiles puts AES-GCM first: egress encryption is the main per-packet CPU cost, and GCM uses AES-NI or ARMv8
// AES (02 §2). AES128_CM_HMAC_SHA1_80 is the fallback every browser supports.
var srtpProfiles = []dtls.SRTPProtectionProfile{dtls.SRTP_AEAD_AES_128_GCM, dtls.SRTP_AES128_CM_HMAC_SHA1_80}

// iceTimeouts are the three arguments of SettingEngine.SetICETimeouts.
type iceTimeouts struct {
	disconnected, failed, keepalive time.Duration
}

var (
	mediaICETimeouts = iceTimeouts{iceDisconnectedTimeout, iceFailedTimeout, iceKeepaliveInterval}
	probeICETimeouts = iceTimeouts{probeDisconnectedTimeout, probeFailedTimeout, iceKeepaliveInterval}
)

// apis are the webrtc.APIs of one SFU, built once in New on 04's netx.Transport and shared by every PeerConnection.
// They hold no sockets: the Transport owns them, and the wiring closes it after SFU.Close. A MediaEngine is copied
// into each PeerConnection and an interceptor registry builds new interceptors for each, so sharing is safe.
type apis struct {
	pub *webrtc.API // publish PCs (the client offers): pub MediaEngine and interceptors (02 §8.1)
	sub *webrtc.API // subscribe PCs (the SFU offers): sub MediaEngine, no interceptors
	// probe holds one API per transport whose mux isn't nil (02 §7.6): an empty MediaEngine, SCTP on, only that
	// transport's candidates.
	probe      map[ProbeTransport]*webrtc.API
	filter     candidateFilter
	advertised []netx.AdvertisedAddr // Transport.Advertised, for the selected-pair label
	log        *slog.Logger
}

// newAPIs builds the publish, subscribe and probe APIs on tr (02 §7.1): each SettingEngine gets tr.Apply first and
// then the settings of 02 §7.3. log carries component=sfu; one Pion logger factory on it serves every API, so the
// per-scope line limit of logx holds for the whole SFU.
func newAPIs(tr *netx.Transport, log *slog.Logger) (*apis, error) {
	if tr == nil {
		return nil, errors.New("sfu: no ICE transport (Config.Transport is nil)")
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	lf := logx.NewPionLoggerFactory(log)
	a := &apis{
		probe:      map[ProbeTransport]*webrtc.API{},
		filter:     newCandidateFilter(tr),
		advertised: slices.Clone(tr.Advertised),
		log:        log,
	}

	se, err := settingEngine(tr, lf, mediaICETimeouts)
	if err != nil {
		return nil, err
	}
	pubME, pubIR, err := newPubEngine()
	if err != nil {
		return nil, err
	}
	a.pub = webrtc.NewAPI(webrtc.WithMediaEngine(pubME), webrtc.WithInterceptorRegistry(pubIR),
		webrtc.WithSettingEngine(se))
	subME, subIR, err := newSubEngine()
	if err != nil {
		return nil, err
	}
	a.sub = webrtc.NewAPI(webrtc.WithMediaEngine(subME), webrtc.WithInterceptorRegistry(subIR),
		webrtc.WithSettingEngine(se))

	for _, pt := range []ProbeTransport{ProbeUDP, ProbeTCP443, ProbeTCP7882} {
		api, err := probeAPI(tr, lf, pt)
		if err != nil {
			return nil, err
		}
		if api != nil {
			a.probe[pt] = api
		}
	}
	return a, nil
}

// settingEngine returns the SettingEngine of 02 §7.3. 04's Transport.Apply comes first: the UDP and TCP muxes,
// network types, interface and IP filters, the address rewrite rules, loopback (development) and mDNS off. Then:
// no active ICE-TCP (the server never dials out), full ICE (S4 tested it), the ICE timeouts, the candidate
// acceptance waits, the DTLS handshake timeout and the SRTP profiles. The receive MTU stays Pion's default (1460).
func settingEngine(tr *netx.Transport, lf logging.LoggerFactory, t iceTimeouts) (webrtc.SettingEngine, error) {
	se := webrtc.SettingEngine{LoggerFactory: lf}
	if err := tr.Apply(&se); err != nil {
		return webrtc.SettingEngine{}, fmt.Errorf("sfu: apply the ICE transport: %w", err)
	}
	se.DisableActiveTCP(true)
	se.SetLite(false)
	se.SetICETimeouts(t.disconnected, t.failed, t.keepalive)
	se.SetPrflxAcceptanceMinWait(prflxAcceptanceMinWait)
	se.SetSrflxAcceptanceMinWait(0)
	se.SetHostAcceptanceMinWait(0)
	// Pion calls the maker for each handshake it starts; the handshake is its own top-level operation, so its
	// context is rooted here (Pion's default does the same with 30 s).
	se.SetDTLSConnectContextMaker(func() (context.Context, func()) {
		return context.WithTimeout(context.Background(), dtlsConnectTimeout)
	})
	se.SetSRTPProtectionProfiles(srtpProfiles...)
	return se, nil
}

// probeAPI builds the connection-test API of one transport (02 §7.6), or returns nil when the transport has no mux
// (SFU.Probe then answers sfu.transport_disabled). Its SettingEngine is the one of 02 §7.3 with the probe's ICE
// timeouts, narrowed to the transport: SetNetworkTypes with the udp or tcp subset of Transport.NetworkTypes
// (probeNetworkTypes), and only that transport's mux: for tcp443 and tcp7882 SetICETCPMux with that listener's mux
// alone. 04 guarantees that Apply only calls setters, so the later call wins. The MediaEngine is empty; SCTP is on
// (Pion's default).
//
// Unlike 02 §7.6, which leaves the UDP mux in place for a TCP probe, the other protocol's mux is removed: Pion (ice
// v4.4) gathers a host candidate on every address of a UDP mux whatever the network types, so a TCP probe would
// advertise UDP candidates too. For the same reason an IPv4-only UDP probe gets the Transport's UDP mux narrowed to
// its IPv4 sockets (ipv4UDPMux).
func probeAPI(tr *netx.Transport, lf logging.LoggerFactory, pt ProbeTransport) (*webrtc.API, error) {
	var (
		tcpMux ice.TCPMux
		udp    bool
	)
	switch pt {
	case ProbeUDP:
		if tr.UDPMux == nil {
			return nil, nil
		}
		udp = true
	case ProbeTCP443:
		tcpMux = tr.TCPMux443
	case ProbeTCP7882:
		tcpMux = tr.TCPMux7882
	default:
		return nil, fmt.Errorf("sfu: unknown probe transport %q", pt)
	}
	if !udp && tcpMux == nil {
		return nil, nil
	}
	types := probeNetworkTypes(tr, udp)
	if len(types) == 0 {
		// No network type of that protocol, though its mux exists: an empty list would mean all of Pion's.
		return nil, nil
	}
	se, err := settingEngine(tr, lf, probeICETimeouts)
	if err != nil {
		return nil, err
	}
	se.SetNetworkTypes(types)
	switch {
	case !udp:
		se.SetICEUDPMux(nil)
		se.SetICETCPMux(tcpMux)
	case probeIPv4Only(tr):
		se.SetICETCPMux(nil)
		se.SetICEUDPMux(ipv4UDPMux{tr.UDPMux})
	default:
		se.SetICETCPMux(nil)
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(&webrtc.MediaEngine{}), webrtc.WithSettingEngine(se)), nil
}

// probeIPv4Only reports whether the probe APIs use IPv4 only: the server has a public IPv4 address (04's Public.V4,
// seen here as an IPv4 entry of Transport.Advertised that isn't a LAN address), so a passing probe reflects the path
// most friends use. IPv6 is probed only on servers without one (02 §7.6).
func probeIPv4Only(tr *netx.Transport) bool {
	return slices.ContainsFunc(tr.Advertised, func(a netx.AdvertisedAddr) bool {
		return a.Addr.Addr().Unmap().Is4() && !a.LAN
	})
}

// probeNetworkTypes returns the udp (or tcp) subset of the Transport's network types for a probe API, only udp4/tcp4
// when the probe is IPv4-only (probeIPv4Only).
func probeNetworkTypes(tr *netx.Transport, udp bool) []webrtc.NetworkType {
	v4Only := probeIPv4Only(tr)
	var out []webrtc.NetworkType
	for _, nt := range tr.NetworkTypes {
		var isUDP, is4 bool
		switch nt {
		case webrtc.NetworkTypeUDP4:
			isUDP, is4 = true, true
		case webrtc.NetworkTypeUDP6:
			isUDP = true
		case webrtc.NetworkTypeTCP4:
			is4 = true
		case webrtc.NetworkTypeTCP6:
		default:
			continue
		}
		if isUDP == udp && (is4 || !v4Only) {
			out = append(out, nt)
		}
	}
	return out
}

// ipv4UDPMux narrows a UDP mux to its IPv4 sockets, for the IPv4-only UDP probe: Pion gathers a host candidate on
// every listen address of a UDP mux, whatever the network types. Everything else goes to the Transport's mux.
type ipv4UDPMux struct{ ice.UDPMux }

// GetListenAddresses returns the mux's IPv4 addresses only.
func (m ipv4UDPMux) GetListenAddresses() []net.Addr {
	var out []net.Addr
	for _, a := range m.UDPMux.GetListenAddresses() {
		if ua, ok := a.(*net.UDPAddr); ok && ua.IP.To4() != nil {
			out = append(out, a)
		}
	}
	return out
}

// Close does nothing: the Transport owns the mux and closes it (Pion never closes a SettingEngine's mux either).
func (ipv4UDPMux) Close() error { return nil }

// via labels the local candidate of a selected candidate pair with the transport it runs on (02 §7.1): the Via
// ("udp" | "tcp443" | "tcp7882") of the Transport.Advertised entry with the same protocol and address:port after
// the rewrite rules, so port numbers are never hard-coded. Stats, Snapshot and Metrics all use this label (§13). A
// UDP candidate that matches no entry is still "udp", since the UDP mux is the only UDP path; a TCP one gets "" and a
// debug line, since 443 and 7882 can't be told apart without a match.
func (a *apis) via(c webrtc.ICECandidate) string {
	proto := strings.ToLower(c.Protocol.String())
	if ip, err := netip.ParseAddr(c.Address); err == nil {
		ap := netip.AddrPortFrom(ip.Unmap().WithZone(""), c.Port)
		for _, adv := range a.advertised {
			if adv.Proto == proto && adv.Addr == ap {
				return adv.Via
			}
		}
	}
	if c.Protocol == webrtc.ICEProtocolUDP {
		return netx.ViaUDP
	}
	a.log.Debug("no advertised address matches the selected local candidate", "protocol", proto)
	return ""
}

// setLocalComplete sets desc as pc's local description, waits until ICE gathering completes (gatherTimeout at most,
// or until ctx ends) and returns pc's local description with every candidate gathered by then. The SFU sends
// complete SDP for sub offers and pub answers and never trickles (02 §2, 01 §9 rule 5): it never sets an
// OnICECandidate handler, so candidates can't overtake their SDP. Gathering is instant with the muxes; complete is
// false when it didn't finish in time, and the SDP then holds the candidates gathered so far. For an ICE restart,
// CreateOffer has already restarted gathering, so the wait covers the new candidates.
func setLocalComplete(ctx context.Context, pc *webrtc.PeerConnection, desc webrtc.SessionDescription,
) (local string, complete bool, err error) {
	gathered := webrtc.GatheringCompletePromise(pc) // before SetLocalDescription, which starts gathering
	if err := pc.SetLocalDescription(desc); err != nil {
		return "", false, err
	}
	timer := time.NewTimer(gatherTimeout)
	defer timer.Stop()
	select {
	case <-gathered:
		complete = true
	case <-timer.C:
	case <-ctx.Done():
		return "", false, ctx.Err()
	}
	ld := pc.LocalDescription()
	if ld == nil {
		return "", false, errors.New("sfu: no local description after SetLocalDescription")
	}
	return ld.SDP, complete, nil
}

// ---- remote-candidate filter (02 §7.3, 01 §17) ----

// dropReason tells why the remote-candidate filter drops a candidate; keepCandidate ("") keeps it. Reasons go to
// debug logs and tests; the address itself is never logged.
type dropReason string

const (
	keepCandidate   dropReason = ""
	dropUnparsable  dropReason = "unparsable"  // not a candidate Pion can parse
	dropHostname    dropReason = "hostname"    // an mDNS name (.local): mDNS is off, and a name could point anywhere
	dropUnspecified dropReason = "unspecified" // 0.0.0.0/8 or ::
	dropBroadcast   dropReason = "broadcast"   // 255.255.255.255
	dropMulticast   dropReason = "multicast"   // 224.0.0.0/4, ff00::/8
	dropLinkLocal   dropReason = "link_local"  // 169.254.0.0/16, fe80::/10
	dropLoopback    dropReason = "loopback"    // 127.0.0.0/8, ::1, unless network.include_loopback
	dropPrivate     dropReason = "private"     // RFC 1918, CGNAT, ULA or site-local, unless the server is on a LAN
	dropIPv4Compat  dropReason = "ipv4_compat" // ::/96 other than :: and ::1 (IPv4-compatible IPv6, deprecated)
	dropOwnAddr     dropReason = "own_address" // an address of Transport.Advertised, unless network.include_loopback
)

var (
	cgnatPrefix      = netip.MustParsePrefix("100.64.0.0/10") // RFC 6598 shared address space
	thisNetwork      = netip.MustParsePrefix("0.0.0.0/8")     // RFC 1122 "this network": never a destination
	siteLocalV6      = netip.MustParsePrefix("fec0::/10")     // deprecated site-local (RFC 3879): private like ULA
	nat64WellKnown   = netip.MustParsePrefix("64:ff9b::/96")  // RFC 6052: a NAT64 gateway forwards to the IPv4 inside
	ipv4Compatible   = netip.MustParsePrefix("::/96")         // RFC 4291 §2.5.5.1, deprecated; RFC 8445 §5.1.1.1
	limitedBroadcast = netip.MustParseAddr("255.255.255.255")
)

// candidateFilter is the remote-candidate filter of 02 §7.3. With full ICE the server sends connectivity checks to
// every remote candidate a client signals, so without it a client could make the server probe its own loopback or
// internal networks (01 §17). It applies to everything a client signals: trickled candidates (trickled, in
// Conn.AddICECandidate) and the candidates of its SDP (filterSDP, before SetRemoteDescription: a browser's pub offer,
// sub answer or re-offer can carry the candidates it has gathered, and so can a connection-test probe offer, SFU.Probe
// of 02 §7.6, which any signed-in user may send). It never applies to peer-reflexive candidates: those are the
// source addresses of checks that already reached the server, and the server learns every client address it needs
// that way. That is also why Pion's SettingEngine.SetRemoteIPFilter isn't used: Pion applies it to peer-reflexive
// candidates too. Each candidate is read with Pion's own parser (ice.UnmarshalCandidate) from exactly the string Pion
// will read, so the filter and Pion never disagree on a candidate's address.
//
// Always dropped: unparsable candidates, host names (mDNS or not), unspecified, broadcast, multicast, link-local and
// IPv4-compatible IPv6 (::/96) addresses. Loopback, and any address the server advertises itself
// (Transport.Advertised, on any port), are kept only with Transport.IncludeLoopback (development). RFC 1918, CGNAT
// (100.64.0.0/10), ULA (fc00::/7) and site-local IPv6 addresses are kept only when the server itself advertises a
// private address (04's LAN/Append case: LAN friends connect to it directly). IPv4-mapped IPv6 addresses are judged
// as IPv4, and a NAT64 address (64:ff9b::/96) by the IPv4 address inside it.
//
// The IPv4-compatible and own-address drops add to the table of 02 §7.3, for the reason of its loopback rule.
// IPv4-compatible addresses: RFC 8445 §5.1.1.1 rules them out as candidates, and a Linux host with the sit module
// loaded tunnels a check to one to the IPv4 address inside it, which may be internal or loopback. The server's own
// addresses: a check to one is delivered locally (or hairpinned by Docker's bridge) and reaches whatever listens on
// that port, as loopback would. No client needs either; a client on the server's host or behind its NAT is still
// learned as peer-reflexive.
type candidateFilter struct {
	loopback bool         // Transport.IncludeLoopback (network.include_loopback)
	private  bool         // any Transport.Advertised[].LAN
	own      []netip.Addr // the addresses of Transport.Advertised, unmapped, without repeats
}

// newCandidateFilter returns the filter for the server's own addresses.
func newCandidateFilter(tr *netx.Transport) candidateFilter {
	f := candidateFilter{
		loopback: tr.IncludeLoopback,
		private:  slices.ContainsFunc(tr.Advertised, func(a netx.AdvertisedAddr) bool { return a.LAN }),
	}
	for _, adv := range tr.Advertised {
		if a := adv.Addr.Addr().Unmap().WithZone(""); a.IsValid() && !slices.Contains(f.own, a) {
			f.own = append(f.own, a)
		}
	}
	return f
}

// trickled judges a trickled candidate the way Pion's PeerConnection.AddICECandidate reads it: the candidate field
// without its "candidate:" prefix. The end-of-candidates marker (an empty candidate) is kept: Pion ignores it.
func (f candidateFilter) trickled(init webrtc.ICECandidateInit) dropReason {
	_, reason := f.judgeTrickled(init)
	return reason
}

// judgeTrickled is trickled with the key of a candidate it keeps, which is what the cap on remote candidates counts
// (02 §12, remoteCandidates).
func (f candidateFilter) judgeTrickled(init webrtc.ICECandidateInit) (candidateKey, dropReason) {
	v := strings.TrimPrefix(init.Candidate, "candidate:")
	if v == "" {
		return candidateKey{}, keepCandidate
	}
	return f.judge(v)
}

// candidate judges one candidate string as ice.UnmarshalCandidate reads it (the value of a candidate attribute, or a
// trickled candidate as trickled passes it). A value with a CR or LF in it is dropped as unparsable: no valid
// candidate has one, and pion/sdp wouldn't read it back unchanged after filterSDP re-marshals the SDP.
func (f candidateFilter) candidate(value string) dropReason {
	_, reason := f.judge(value)
	return reason
}

// candidateKey is what makes a remote candidate one of its own to Pion: everything ice.Candidate.Equal compares.
// Pion's ICE agent keeps every remote candidate it has no equal of, pairs it with each local candidate and sends
// its checks to it, and has no cap of its own. So two candidates on one transport address that differ in their type
// or their related address are two candidates, and the cap on remote candidates (02 §12) counts keys, not addresses.
//
// The fields are Pion's, read from the candidate as Pion parsed it, so the SFU and Pion never disagree on whether two
// candidates are the same: two with one key are equal to Pion. (Pion keeps the address as the client spelled it, so
// two spellings of one IPv6 address are two candidates, here as there.)
type candidateKey struct {
	network ice.NetworkType // udp4, udp6, tcp4 or tcp6
	address string          // as the candidate spells it, without a zone
	port    int
	tcpType ice.TCPType
	typ     ice.CandidateType
	// The related address (raddr and rport). A host candidate has none; the other types always have one, empty when
	// the candidate names none.
	related bool
	raddr   string
	rport   int
}

// keyOf returns the key of a candidate that Pion parsed.
func keyOf(c ice.Candidate) candidateKey {
	k := candidateKey{network: c.NetworkType(), address: c.Address(), port: c.Port(), tcpType: c.TCPType(), typ: c.Type()}
	if rel := c.RelatedAddress(); rel != nil {
		k.related, k.raddr, k.rport = true, rel.Address, rel.Port
	}
	return k
}

// judge is candidate with the key of a candidate it keeps.
func (f candidateFilter) judge(value string) (candidateKey, dropReason) {
	if strings.ContainsAny(value, "\r\n") {
		return candidateKey{}, dropUnparsable
	}
	c, err := ice.UnmarshalCandidate(value)
	if err != nil {
		return candidateKey{}, dropUnparsable
	}
	ip, err := netip.ParseAddr(c.Address())
	if err != nil {
		return candidateKey{}, dropHostname
	}
	if reason := f.addr(ip); reason != keepCandidate {
		return candidateKey{}, reason
	}
	return keyOf(c), keepCandidate
}

// addr judges a candidate's connection address.
func (f candidateFilter) addr(a netip.Addr) dropReason {
	a = a.Unmap().WithZone("")
	if nat64WellKnown.Contains(a) {
		b := a.As16()
		a = netip.AddrFrom4([4]byte(b[12:]))
	}
	switch {
	case !a.IsValid():
		return dropUnparsable
	case a.IsUnspecified(), thisNetwork.Contains(a):
		return dropUnspecified
	case a == limitedBroadcast:
		return dropBroadcast
	case a.IsMulticast():
		return dropMulticast
	case a.IsLinkLocalUnicast():
		return dropLinkLocal
	case a.IsLoopback():
		if !f.loopback {
			return dropLoopback
		}
	case ipv4Compatible.Contains(a): // after :: and ::1, which keep their own reasons
		return dropIPv4Compat
	case a.IsPrivate(), cgnatPrefix.Contains(a), siteLocalV6.Contains(a):
		if !f.private {
			return dropPrivate
		}
	}
	if !f.loopback && slices.Contains(f.own, a) {
		return dropOwnAddr
	}
	return keepCandidate
}

// filterSDP returns a remote description (a pub offer, a sub answer or a probe offer: SFU.Probe, 02 §7.6) without
// the candidate attributes the filter drops, at session or media level, and how many it dropped.
//
// It reads the SDP with pion/sdp, the parser of Pion's SetRemoteDescription, and judges each candidate attribute by
// its value, exactly the string Pion's parser reads (webrtc's extractICEDetails → ice.UnmarshalCandidate), so every
// attribute Pion takes for a candidate is judged however the lines are broken (pion/sdp also ends a line at a lone
// CR in places, and skips CRs before a line's type). An SDP without a dropped candidate comes back unchanged. Otherwise
// it is re-marshaled, and since pion/sdp doesn't always read back what it wrote for a malformed line (it cuts one more
// character off a line for each CR inside it), the result is read again the same way: a candidate the filter drops
// in it makes the SDP sfu.bad_sdp, as does an SDP pion/sdp can't parse (SetRemoteDescription would refuse it too).
// Browser and Pion descriptions never hit either case.
func (f candidateFilter) filterSDP(raw string) (string, int, error) {
	return f.limitSDP(raw, nil)
}

// limitSDP is filterSDP for the remote description of a Conn's PC, whose candidates count toward the PC's cap on
// remote candidates like trickled ones (02 §12): a candidate that the filter keeps but admit refuses is dropped too.
// admit gets the key of each kept candidate, in the SDP's order, and must give the same answer when it is asked
// about a candidate again (remoteCandidates.admit does: a candidate it admitted stays admitted), because the SDP
// that comes out is read once more.
func (f candidateFilter) limitSDP(raw string, admit func(candidateKey) bool) (string, int, error) {
	desc, err := parseSDP(raw)
	if err != nil {
		return "", 0, err
	}
	dropped := f.dropCandidates(desc, admit)
	if dropped == 0 {
		return raw, 0, nil
	}
	b, err := desc.Marshal()
	if err != nil {
		return "", 0, newError(CodeBadSDP, "SDP can't be written back after dropping candidates")
	}
	out := string(b)
	if again, err := parseSDP(out); err != nil || f.dropCandidates(again, admit) != 0 {
		return "", 0, newError(CodeBadSDP, "malformed lines around candidates")
	}
	return out, dropped, nil
}

// dropCandidates removes the candidate attributes the filter drops from desc, at session and media level, and those
// that admit refuses when there is one, and returns how many it removed.
func (f candidateFilter) dropCandidates(desc *sdp.SessionDescription, admit func(candidateKey) bool) int {
	dropped := 0
	drop := func(a sdp.Attribute) bool {
		if !a.IsICECandidate() {
			return false
		}
		key, reason := f.judge(a.Value)
		if reason != keepCandidate || (admit != nil && !admit(key)) {
			dropped++
			return true
		}
		return false
	}
	desc.Attributes = slices.DeleteFunc(desc.Attributes, drop)
	for _, md := range desc.MediaDescriptions {
		md.Attributes = slices.DeleteFunc(md.Attributes, drop)
	}
	return dropped
}
