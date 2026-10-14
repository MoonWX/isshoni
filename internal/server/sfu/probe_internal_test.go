package sfu

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/server/netx"
)

// The unit tests of the connection-test probe (probe.go, 02 §7.6); integration 18 is probe_test.go.

// probeOfferSDP is a browser's probe offer as Chrome sends it: one data channel, no candidates.
const probeOfferSDP = "v=0\r\n" +
	"o=- 4611731400430051336 2 IN IP4 127.0.0.1\r\n" +
	"s=-\r\n" +
	"t=0 0\r\n" +
	"a=group:BUNDLE 0\r\n" +
	"a=extmap-allow-mixed\r\n" +
	"a=msid-semantic: WMS\r\n" +
	"m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n" +
	"c=IN IP4 0.0.0.0\r\n" +
	"a=ice-ufrag:Hk9a\r\n" +
	"a=ice-pwd:kQ2x7mP1vB8nC4tR6yW0zL3s\r\n" +
	"a=ice-options:trickle\r\n" +
	"a=fingerprint:sha-256 4A:AD:B9:B1:3F:82:18:3B:54:02:12:DF:3E:5D:49:6B:19:E5:7C:AB:4E:9A:1F:AB:EE:6C:3F:76:1F:F3:56:39\r\n" +
	"a=setup:actpass\r\n" +
	"a=mid:0\r\n" +
	"a=sctp-port:5000\r\n" +
	"a=max-message-size:262144\r\n"

func TestCheckProbeOffer(t *testing.T) {
	if err := checkProbeOffer(probeOfferSDP); err != nil {
		t.Fatalf("a browser's probe offer: %v", err)
	}
	withCandidate := probeOfferSDP + "a=candidate:1 1 udp 2122260223 192.0.2.10 54400 typ host\r\n"
	if err := checkProbeOffer(withCandidate); err != nil {
		t.Errorf("a probe offer with a candidate: %v", err)
	}
	session := probeOfferSDP[:strings.Index(probeOfferSDP, "m=")]
	media := probeOfferSDP[strings.Index(probeOfferSDP, "m="):]
	audio := "m=audio 9 UDP/TLS/RTP/SAVPF 111\r\nc=IN IP4 0.0.0.0\r\na=mid:1\r\na=rtpmap:111 opus/48000/2\r\n"
	for name, raw := range map[string]string{
		"empty":                "",
		"blank":                " \r\n",
		"unparsable":           "v=0\r\nnonsense\r\n",
		"no m-section":         session,
		"audio only":           session + audio,
		"audio and a channel":  probeOfferSDP + audio,
		"two data channels":    probeOfferSDP + media,
		"video, not a channel": session + strings.Replace(media, "m=application 9 UDP/DTLS/SCTP webrtc-datachannel", "m=video 9 UDP/TLS/RTP/SAVPF 96", 1),
		"larger than 64 KiB":   probeOfferSDP + strings.Repeat("a=x:"+strings.Repeat("y", 1020)+"\r\n", 64),
	} {
		err := checkProbeOffer(raw)
		if !errors.Is(err, &Error{Code: CodeBadSDP}) {
			t.Errorf("%s: %v, want sfu.bad_sdp", name, err)
			continue
		}
		wantErr(t, err, CodeBadSDP)
	}
	if n := len(probeOfferSDP + strings.Repeat("a=x:"+strings.Repeat("y", 1020)+"\r\n", 64)); n <= maxProbeOfferBytes {
		t.Fatalf("the large offer has %d bytes", n)
	}
}

func TestProbeTransportValid(t *testing.T) {
	for _, pt := range []ProbeTransport{ProbeUDP, ProbeTCP443, ProbeTCP7882} {
		if !pt.valid() {
			t.Errorf("%s is not valid", pt)
		}
	}
	for _, pt := range []ProbeTransport{"", "UDP", "tcp", "tcp9999", "tls"} {
		if pt.valid() {
			t.Errorf("%q is valid", pt)
		}
	}
}

// TestPairLabel: ProbeResult.Selected is the transport label of the server's candidate and its address and port, as
// advertised (after the rewrite rules); a TCP candidate that matches no advertised address is plain "tcp".
func TestPairLabel(t *testing.T) {
	a := &apis{advertised: []netx.AdvertisedAddr{
		{Proto: "udp", Addr: netip.MustParseAddrPort("203.0.113.5:7882"), Via: netx.ViaUDP},
		{Proto: "tcp", Addr: netip.MustParseAddrPort("203.0.113.5:443"), Via: netx.ViaTCP443},
		{Proto: "tcp", Addr: netip.MustParseAddrPort("203.0.113.5:7882"), Via: netx.ViaTCP7882},
		{Proto: "udp", Addr: netip.MustParseAddrPort("[2001:db8::5]:7882"), Via: netx.ViaUDP},
	}, log: slog.New(slog.DiscardHandler)}
	for _, tc := range []struct {
		c    webrtc.ICECandidate
		want string
	}{
		{webrtc.ICECandidate{Protocol: webrtc.ICEProtocolUDP, Address: "203.0.113.5", Port: 7882}, "udp 203.0.113.5:7882"},
		{webrtc.ICECandidate{Protocol: webrtc.ICEProtocolTCP, Address: "203.0.113.5", Port: 443}, "tcp443 203.0.113.5:443"},
		{webrtc.ICECandidate{Protocol: webrtc.ICEProtocolTCP, Address: "203.0.113.5", Port: 7882}, "tcp7882 203.0.113.5:7882"},
		{webrtc.ICECandidate{Protocol: webrtc.ICEProtocolUDP, Address: "2001:db8::5", Port: 7882}, "udp [2001:db8::5]:7882"},
		{webrtc.ICECandidate{Protocol: webrtc.ICEProtocolUDP, Address: "::ffff:203.0.113.5", Port: 7882}, "udp 203.0.113.5:7882"},
		{webrtc.ICECandidate{Protocol: webrtc.ICEProtocolTCP, Address: "198.51.100.9", Port: 8443}, "tcp 198.51.100.9:8443"},
		{webrtc.ICECandidate{Protocol: webrtc.ICEProtocolUDP, Address: "host.invalid", Port: 7882}, "udp host.invalid:7882"},
	} {
		if got := a.pairLabel(tc.c); got != tc.want {
			t.Errorf("pairLabel(%s %s:%d) = %q, want %q", tc.c.Protocol, tc.c.Address, tc.c.Port, got, tc.want)
		}
	}
}

// TestProbeSlots: a user's new probe of a transport takes the old one's slot; the 21st probe is sfu.probe_limit with
// RetryAfter until the oldest probe's life ends, and at least a second; a freed slot can be taken again; after Close
// every probe is sfu.closed.
func TestProbeSlots(t *testing.T) {
	s, _ := newTestSFU(t)
	if s.timing.probeLife != 20*time.Second || maxProbes != 20 || probeRTTAfter != time.Second {
		t.Errorf("probe life %v, %d probes, RTT after %v: want the 20 s, 20 and 1 s of 02 §7.6",
			s.timing.probeLife, maxProbes, probeRTTAfter)
	}
	if probeICETimeouts.disconnected != 3*time.Second || probeICETimeouts.failed != 8*time.Second {
		t.Errorf("probe ICE timeouts %+v, want 3 s and 8 s (02 §7.6)", probeICETimeouts)
	}
	var probes []*probe
	for i := range maxProbes {
		p := s.newProbe(UserID("u"+string(rune('a'+i))), ProbeUDP)
		if i == 0 {
			p.deadline = time.Now().Add(5 * time.Second) // the oldest
		}
		old, err := s.addProbe(p)
		if err != nil || old != nil {
			t.Fatalf("probe %d: %v, %v", i+1, old, err)
		}
		probes = append(probes, p)
	}
	_, err := s.addProbe(s.newProbe("late", ProbeUDP))
	e := errOf(t, err, CodeProbeLimit)
	if !e.Retryable || e.RetryAfter > 5*time.Second || e.RetryAfter < 4*time.Second {
		t.Errorf("probe_limit %+v, want retryable with RetryAfter until the oldest probe's life ends", e)
	}
	// A user's new probe of the same transport takes the slot; of another transport it needs its own.
	again := s.newProbe("ua", ProbeUDP)
	if old, err := s.addProbe(again); err != nil || old != probes[0] {
		t.Fatalf("ua's second udp probe: %v, %v", old, err)
	}
	_, err = s.addProbe(s.newProbe("ua", ProbeTCP7882))
	wantErr(t, err, CodeProbeLimit)
	// The replaced probe frees nothing; the new one does. A probe whose life is over still holds its slot until
	// it is gone, and the wait is then at least a second.
	s.removeProbe(probes[0])
	_, err = s.addProbe(s.newProbe("late", ProbeUDP))
	wantErr(t, err, CodeProbeLimit)
	probes[1].deadline = time.Now().Add(-time.Second)
	_, err = s.addProbe(s.newProbe("late", ProbeUDP))
	if e := errOf(t, err, CodeProbeLimit); e.RetryAfter != time.Second {
		t.Errorf("RetryAfter = %v after the oldest life ended, want 1 s", e.RetryAfter)
	}
	s.removeProbe(again)
	if _, err := s.addProbe(s.newProbe("late", ProbeUDP)); err != nil {
		t.Errorf("a freed slot: %v", err)
	}
	if slots, running := len(s.probes), len(s.probing); slots != maxProbes || running != maxProbes {
		t.Errorf("%d slots, %d running", slots, running)
	}

	// None of these has a goroutine; Close must not wait for them.
	s.mu.Lock()
	clear(s.probing)
	clear(s.probes)
	s.mu.Unlock()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = s.addProbe(s.newProbe("late", ProbeUDP))
	wantErr(t, err, CodeClosed)
}

// TestFilterProbeOffer: a probe offer's candidates go through the remote-candidate filter and the cap of 64 (02 §12):
// of 100 public host candidates the first 64 stay and 36 are dropped. A candidate that the filter drops uses none of
// the 64, and one that was kept stays when it comes again.
func TestFilterProbeOffer(t *testing.T) {
	cand := func(port int) string {
		return fmt.Sprintf("a=candidate:%d 1 udp 2122260223 203.0.113.7 %d typ host\r\n", port, port)
	}
	var many, first64 strings.Builder
	for i := range 100 {
		many.WriteString(cand(50000 + i))
		if i < maxRemoteCandidates {
			first64.WriteString(cand(50000 + i))
		}
	}
	filter := candidateFilter{}
	out, dropped, err := filter.filterProbeOffer(probeOfferSDP + many.String())
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 36 {
		t.Errorf("dropped %d of 100 candidates, want 36", dropped)
	}
	if got, want := probeCandidates(t, out), probeCandidates(t, probeOfferSDP+first64.String()); !slices.Equal(got, want) {
		t.Errorf("kept %d candidates %q..., want the first 64", len(got), got[:min(len(got), 3)])
	}

	// A loopback candidate ahead of them is dropped by the filter and takes no place; the first one again, after
	// the 100, was kept already.
	loopback := "a=candidate:1 1 udp 2122260223 127.0.0.1 9 typ host\r\n"
	out, dropped, err = filter.filterProbeOffer(probeOfferSDP + loopback + many.String() + cand(50000))
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 37 {
		t.Errorf("dropped %d candidates, want the loopback one and 36", dropped)
	}
	want := probeCandidates(t, probeOfferSDP+first64.String()+cand(50000))
	if got := probeCandidates(t, out); !slices.Equal(got, want) {
		t.Errorf("kept %d candidates, want the first 64 and the repeat", len(got))
	}

	// Each offer has a count of its own: a probe offer of 64 is whole.
	if out, dropped, err := filter.filterProbeOffer(probeOfferSDP + first64.String()); err != nil || dropped != 0 ||
		out != probeOfferSDP+first64.String() {
		t.Errorf("64 candidates: %d dropped, %v", dropped, err)
	}
}

// probeCandidates returns the values of an SDP's candidate attributes, in order.
func probeCandidates(t *testing.T, raw string) []string {
	t.Helper()
	desc, err := parseSDP(raw)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, md := range desc.MediaDescriptions {
		for _, a := range md.Attributes {
			if a.IsICECandidate() {
				out = append(out, a.Value)
			}
		}
	}
	return out
}

// FuzzProbeOffer: whatever a client posts as a probe offer, the SDP guard answers it with nil or sfu.bad_sdp; an offer
// it takes has one m-section, a data channel, and still has it after the remote-candidate filter and its cap, which
// never fail on it with anything but sfu.bad_sdp either and leave at most 64 candidates.
func FuzzProbeOffer(f *testing.F) {
	f.Add(probeOfferSDP)
	f.Add(probeOfferSDP + "a=candidate:1 1 udp 2122260223 192.0.2.10 54400 typ host\r\n")
	f.Add(probeOfferSDP + "a=candidate:2 1 udp 2122260223 127.0.0.1 54401 typ host\r\na=end-of-candidates\r\n")
	f.Add(strings.Replace(probeOfferSDP, "m=application 9 UDP/DTLS/SCTP webrtc-datachannel", "m=audio 9 UDP/TLS/RTP/SAVPF 111", 1))
	f.Add(probeOfferSDP + probeOfferSDP[strings.Index(probeOfferSDP, "m="):])
	f.Add("v=0\r\n")
	f.Add("")
	filter := candidateFilter{private: true}
	f.Fuzz(func(t *testing.T, raw string) {
		if err := checkProbeOffer(raw); err != nil {
			if !errors.Is(err, &Error{Code: CodeBadSDP}) {
				t.Fatalf("checkProbeOffer: %v, want nil or sfu.bad_sdp", err)
			}
			return
		}
		out, _, err := filter.filterProbeOffer(raw)
		if err != nil {
			if !errors.Is(err, &Error{Code: CodeBadSDP}) {
				t.Fatalf("filterProbeOffer: %v, want nil or sfu.bad_sdp", err)
			}
			return
		}
		desc, err := parseSDP(out)
		if err != nil {
			t.Fatalf("the filtered offer doesn't parse: %v", err)
		}
		if len(desc.MediaDescriptions) != 1 || desc.MediaDescriptions[0].MediaName.Media != "application" {
			t.Fatalf("an accepted probe offer has %d m-sections", len(desc.MediaDescriptions))
		}
		keys := map[candidateKey]struct{}{}
		attrs := slices.Concat(desc.Attributes, desc.MediaDescriptions[0].Attributes)
		for _, a := range attrs {
			if !a.IsICECandidate() {
				continue
			}
			key, reason := filter.judge(a.Value)
			if reason != keepCandidate {
				t.Fatalf("the filtered offer has a candidate the filter drops (%s)", reason)
			}
			keys[key] = struct{}{}
		}
		if len(keys) > maxRemoteCandidates {
			t.Fatalf("the filtered offer has %d candidates", len(keys))
		}
	})
}
