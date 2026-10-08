package publish

import (
	"context"
	"errors"
	"math"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/rtcp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/media/fake"
)

func loopbackSettings() webrtc.SettingEngine {
	var se webrtc.SettingEngine
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetIncludeLoopbackCandidate(true)
	se.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	return se
}

func newSource(t *testing.T) *fake.Source {
	t.Helper()
	src, err := fake.New(fake.Config{Audio: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	return src
}

func TestNewOptions(t *testing.T) {
	src := newSource(t)
	for _, tc := range []struct {
		name string
		o    Options
		want error
	}{
		{"defaults", Options{Source: src}, nil},
		{"audio only", Options{Source: src, Layers: []string{}, Audio: true}, nil},
		{"no source", Options{}, ErrInvalidOptions},
		{"nothing to send", Options{Source: src, Layers: []string{}}, ErrInvalidOptions},
		{"redundant RTX", Options{Source: src, RedundantRTX: true}, ErrNotImplemented},
		{"repeated layer", Options{Source: src, Layers: []string{"f", "f"}}, ErrInvalidOptions},
		{"empty layer", Options{Source: src, Layers: []string{""}}, ErrInvalidOptions},
		{"profile with level", Options{Source: src, Profile: "64001f"}, ErrInvalidOptions},
		{"upper-case profile", Options{Source: src, Profile: "42E0"}, ErrInvalidOptions},
		{"not hex", Options{Source: src, Profile: "xyzw"}, ErrInvalidOptions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := New(tc.o)
			if !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
				t.Fatalf("New = %v, want %v", err, tc.want)
			}
			if p != nil {
				if err := p.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// TestOffer checks the offer a Publisher makes: one sendonly video m-line with the rids f and q as simulcast, the
// one H.264 profile, mid/rid/transport-cc extensions, and a sendonly Opus m-line.
func TestOffer(t *testing.T) {
	p, err := New(Options{Source: newSource(t), Audio: true, Profile: "42e0", Settings: loopbackSettings()})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	offer, err := p.Offer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var sd sdp.SessionDescription
	if err := sd.Unmarshal([]byte(offer.SDP)); err != nil {
		t.Fatal(err)
	}
	if len(sd.MediaDescriptions) != 2 {
		t.Fatalf("%d m-lines", len(sd.MediaDescriptions))
	}
	video, audio := sd.MediaDescriptions[0], sd.MediaDescriptions[1]
	attr := func(m *sdp.MediaDescription, key string) []string {
		var out []string
		for _, a := range m.Attributes {
			if a.Key == key {
				out = append(out, a.Value)
			}
		}
		return out
	}
	has := func(m *sdp.MediaDescription, key, sub string) bool {
		for _, v := range attr(m, key) {
			if strings.Contains(v, sub) {
				return true
			}
		}
		return false
	}
	if video.MediaName.Media != "video" || audio.MediaName.Media != "audio" {
		t.Fatalf("m-lines %s, %s", video.MediaName.Media, audio.MediaName.Media)
	}
	for _, m := range []*sdp.MediaDescription{video, audio} {
		if _, ok := m.Attribute("sendonly"); !ok {
			t.Errorf("%s is not sendonly", m.MediaName.Media)
		}
		if !has(m, "extmap", sdp.TransportCCURI) {
			t.Errorf("%s has no transport-wide-cc extension", m.MediaName.Media)
		}
	}
	for _, want := range []struct{ key, sub string }{
		{"rtpmap", "H264/90000"}, {"fmtp", "packetization-mode=1;profile-level-id=42e01f"},
		{"rtcp-fb", "nack pli"}, {"rtcp-fb", "ccm fir"}, {"rtcp-fb", "transport-cc"},
		{"extmap", sdp.SDESMidURI}, {"extmap", sdp.SDESRTPStreamIDURI},
		{"rid", "f send"}, {"rid", "q send"}, {"simulcast", "send f;q"},
	} {
		if !has(video, want.key, want.sub) {
			t.Errorf("video has no a=%s with %q", want.key, want.sub)
		}
	}
	if len(attr(video, "rtpmap")) != 1 {
		t.Errorf("video offers %v, want only the one H.264 profile", attr(video, "rtpmap"))
	}
	if !has(audio, "rtpmap", "opus/48000/2") || !has(audio, "fmtp", "stereo=1") {
		t.Errorf("audio: %v %v", attr(audio, "rtpmap"), attr(audio, "fmtp"))
	}
	if !has(video, "candidate", "127.0.0.1") {
		t.Error("the offer holds no loopback candidate: gathering didn't complete")
	}
	tracks := p.Tracks()
	if len(tracks) != 3 || tracks[0].RID != "f" || tracks[1].RID != "q" || tracks[2].Kind != webrtc.RTPCodecTypeAudio {
		t.Fatalf("tracks %+v", tracks)
	}
	for _, tr := range tracks {
		if tr.MID == "" || tr.SSRC == 0 {
			t.Errorf("track %+v has no mid or SSRC", tr)
		}
	}
	if tracks[0].MID != tracks[1].MID || tracks[0].SSRC == tracks[1].SSRC {
		t.Errorf("simulcast encodings %+v and %+v", tracks[0], tracks[1])
	}
}

// TestSendOnlySimulcast: the copy of an offer that goes out has no receive side in its simulcast attributes, and is
// otherwise the offer, byte for byte.
func TestSendOnlySimulcast(t *testing.T) {
	const head = "v=0\r\no=- 1 1 IN IP4 0.0.0.0\r\ns=-\r\nt=0 0\r\nm=video 9 UDP/TLS/RTP/SAVPF 96\r\na=mid:0\r\n"
	for _, tc := range []struct{ name, in, want string }{
		{"a first offer", head + "a=rid:f send\r\na=rid:q send\r\na=simulcast:send f;q\r\na=sendonly\r\n",
			head + "a=rid:f send\r\na=rid:q send\r\na=simulcast:send f;q\r\na=sendonly\r\n"},
		{"Pion's re-offer", head + "a=rid:f recv\r\na=rid:q recv\r\na=simulcast:recv f;q\r\na=msid:s video\r\n" +
			"a=rid:f send\r\na=rid:q send\r\na=simulcast:send f;q\r\na=sendonly\r\n",
			head + "a=msid:s video\r\na=rid:f send\r\na=rid:q send\r\na=simulcast:send f;q\r\na=sendonly\r\n"},
		{"both directions on one line", head + "a=rid:f recv pt=96\r\na=rid:f send pt=96\r\na=simulcast:recv f send f;~q\r\n",
			head + "a=rid:f send pt=96\r\na=simulcast:send f;~q\r\n"},
		{"send first", head + "a=simulcast:send f;q recv f;q\r\n", head + "a=simulcast:send f;q\r\n"},
		{"no simulcast", head + "a=recvonly\r\na=rtcp-fb:96 nack\r\n", head + "a=recvonly\r\na=rtcp-fb:96 nack\r\n"},
		{"bare newlines, none at the end", "m=video 9 X 96\na=rid:q recv\na=rid:q send", "m=video 9 X 96\na=rid:q send"},
		{"empty", "", ""},
	} {
		if got := sendOnlySimulcast(tc.in); got != tc.want {
			t.Errorf("%s: sendOnlySimulcast = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// FuzzSendOnlySimulcast: whatever the description, the copy has no receive rid left, changes no other line, and is
// its own copy.
func FuzzSendOnlySimulcast(f *testing.F) {
	for _, seed := range []string{
		"", "v=0\r\n", "a=rid:f recv\r\na=rid:f send\r\na=simulcast:recv f;q send f;q\r\n", "a=simulcast:recv f\n",
		"a=simulcast:send\r\n", "a=rid:\na=rid:q\na=rid:q recv", "a=simulcast: recv  f  send q recv", "\n\r\n\r",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		out := sendOnlySimulcast(in)
		if again := sendOnlySimulcast(out); again != out {
			t.Fatalf("the copy of the copy differs: %q, then %q", out, again)
		}
		// others returns the lines of a description that aren't simulcast attributes of the receive side.
		others := func(desc string, copied bool) []string {
			var lines []string
			for line := range strings.SplitAfterSeq(desc, "\n") {
				text := strings.TrimRight(line, "\r\n")
				fields := strings.Fields(text)
				switch {
				case line == "":
				case strings.HasPrefix(text, "a=rid:") && len(fields) > 1 && fields[1] == "recv":
					if copied {
						t.Fatalf("the copy of %q has the receive rid %q", in, text)
					}
				case strings.HasPrefix(text, "a=simulcast:"):
					fields = strings.Fields(strings.TrimPrefix(text, "a=simulcast:"))
					for i := 0; copied && i+1 < len(fields); i += 2 {
						if fields[i] == "recv" {
							t.Fatalf("the copy of %q has the receive list %q", in, text)
						}
					}
				default:
					lines = append(lines, line)
				}
			}
			return lines
		}
		if was, is := others(in, false), others(out, true); !slices.Equal(was, is) {
			t.Fatalf("the copy of %q changed other lines: %q, were %q", in, is, was)
		}
	})
}

// TestReoffer: a second offer on the same PeerConnection, after a simulcast answer. Pion's local description then
// lists the rids of that answer as receive rids next to the ones it sends; the offer that Offer returns for sending
// has only the send side, like the first one, so an SFU that reads every a=rid line as a layer takes it (02 §8.4:
// rids ⊆ {f,q}, none twice). If the first assertion ever fails, Pion has stopped repeating them and sendOnlySimulcast
// can go.
func TestReoffer(t *testing.T) {
	p, err := New(Options{Source: newSource(t), Audio: true, Settings: loopbackSettings()})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	// The receiving side: a PeerConnection that takes the Publisher's H.264 and simulcast, as the SFU's pub PC does.
	m := &webrtc.MediaEngine{}
	for _, c := range []struct {
		params webrtc.RTPCodecParameters
		kind   webrtc.RTPCodecType
	}{
		{webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeH264, ClockRate: videoClock, SDPFmtpLine: h264FmtpPrefix + DefaultProfile + "1f",
			},
			PayloadType: videoPT,
		}, webrtc.RTPCodecTypeVideo},
		{webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: audioClock, Channels: 2},
			PayloadType:        opusPT,
		}, webrtc.RTPCodecTypeAudio},
	} {
		if err := m.RegisterCodec(c.params, c.kind); err != nil {
			t.Fatal(err)
		}
	}
	for _, uri := range []string{sdp.SDESMidURI, sdp.SDESRTPStreamIDURI, sdp.SDESRepairRTPStreamIDURI} {
		if err := m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: uri}, webrtc.RTPCodecTypeVideo); err != nil {
			t.Fatal(err)
		}
	}
	recv, err := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(loopbackSettings())).
		NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer recv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	answer := func(offer webrtc.SessionDescription) webrtc.SessionDescription {
		t.Helper()
		if err := recv.SetRemoteDescription(offer); err != nil {
			t.Fatalf("the receiver refused the offer: %v", err)
		}
		ans, err := recv.CreateAnswer(nil)
		if err != nil {
			t.Fatal(err)
		}
		gathered := webrtc.GatheringCompletePromise(recv)
		if err := recv.SetLocalDescription(ans); err != nil {
			t.Fatal(err)
		}
		select {
		case <-gathered:
		case <-ctx.Done():
			t.Fatal("the receiver's candidates were not gathered in time")
		}
		return *recv.LocalDescription()
	}
	rids := func(raw string) []string {
		var out []string
		for line := range strings.Lines(raw) {
			if line = strings.TrimSpace(line); strings.HasPrefix(line, "a=rid:") || strings.HasPrefix(line, "a=simulcast:") {
				out = append(out, line)
			}
		}
		return out
	}
	sendSide := []string{"a=rid:f send", "a=rid:q send", "a=simulcast:send f;q"}

	first, err := p.Offer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := rids(first.SDP); !slices.Equal(got, sendSide) || first.SDP != p.PC().LocalDescription().SDP {
		t.Fatalf("the first offer's rids = %v, want %v; it is the local description as it is", got, sendSide)
	}
	ans := answer(first)
	if got := rids(ans.SDP); !slices.Equal(got, []string{"a=rid:f recv", "a=rid:q recv", "a=simulcast:recv f;q"}) {
		t.Fatalf("the answer's rids = %v, want the receive side of f and q", got)
	}
	if err := p.SetAnswer(ans); err != nil {
		t.Fatal(err)
	}

	second, err := p.Offer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	local := p.PC().LocalDescription().SDP
	if got := rids(local); !slices.Contains(got, "a=rid:f recv") || !slices.Contains(got, "a=simulcast:recv f;q") {
		t.Errorf("the local description of a re-offer has the rids %v: Pion no longer repeats the answer's receive rids", got)
	}
	if got := rids(second.SDP); !slices.Equal(got, sendSide) {
		t.Errorf("the re-offer's rids = %v, want only the send side %v", got, sendSide)
	}
	if second.Type != webrtc.SDPTypeOffer || second.SDP != sendOnlySimulcast(local) || !strings.Contains(second.SDP, "a=candidate:") {
		t.Error("the re-offer isn't the local description without its receive rids, with its candidates")
	}
	// The receiver takes it, and answers as before; so does the Publisher.
	again := answer(second)
	if got := rids(again.SDP); !slices.Equal(got, []string{"a=rid:f recv", "a=rid:q recv", "a=simulcast:recv f;q"}) {
		t.Errorf("the second answer's rids = %v", got)
	}
	if err := p.SetAnswer(again); err != nil {
		t.Errorf("the second answer: %v", err)
	}
	if tracks := p.Tracks(); len(tracks) != 3 || tracks[0].MID != tracks[1].MID || tracks[0].MID == "" {
		t.Errorf("tracks after the re-offer = %+v", tracks)
	}
}

func TestLifecycle(t *testing.T) {
	p, err := New(Options{Source: newSource(t), Settings: loopbackSettings()})
	if err != nil {
		t.Fatal(err)
	}
	// Started but never connected: Close stops the loops that wait for the connection and for RTCP.
	if err := p.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := p.Start(t.Context()); err == nil {
		t.Error("second Start succeeded")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if err := p.Start(t.Context()); !errors.Is(err, ErrClosed) {
		t.Errorf("Start after Close = %v", err)
	}
	s := p.Stats()
	if len(s.Tracks) != 2 || s.Tracks[0].Packets != 0 || s.Dropped != 0 {
		t.Errorf("stats %+v", s)
	}
}

func TestRTPTicks(t *testing.T) {
	for _, tc := range []struct {
		d     int64
		clock uint32
		want  uint32
	}{
		{0, 90000, 0},
		{int64(time.Second) / 60, 90000, 1500}, // 16666666 ns: 1499.99994 ticks, rounded
		{int64(time.Second) * 7 / 60, 90000, 10500},
		{int64(20 * time.Millisecond), 48000, 960},
		{int64(time.Second) / 15, 90000, 6000},
		{-int64(time.Second) / 60, 90000, math.MaxUint32 - 1499},
		{int64(48 * time.Hour), 90000, uint32(uint64(48*3600*90000) % (1 << 32))}, // wraps like RTP time
	} {
		if got := rtpTicks(tc.d, tc.clock); got != tc.want {
			t.Errorf("rtpTicks(%d, %d) = %d, want %d", tc.d, tc.clock, got, tc.want)
		}
	}
	// Centuries of media time must not panic in the 128-bit division.
	_ = rtpTicks(math.MaxInt64, math.MaxUint32)
	_ = rtpTicks(math.MinInt64+1, math.MaxUint32)
}

func TestNTPTime(t *testing.T) {
	if got := ntpTime(time.Unix(0, 0)); got != ntpEpochOffset<<32 {
		t.Errorf("Unix epoch = %#x", got)
	}
	if got := ntpTime(time.Unix(1, int64(time.Second)/2)); got != (ntpEpochOffset+1)<<32|1<<31 {
		t.Errorf("1.5 s = %#x", got)
	}
}

func TestProfileKey(t *testing.T) {
	for fmtp, want := range map[string]string{
		"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=64001f": "6400",
		"profile-level-id=42E01F;packetization-mode=1":                           "42e0",
		"packetization-mode=0;profile-level-id=42e01f":                           "",
		"profile-level-id=42e01f":                                                "",
		"packetization-mode=1;profile-level-id=zz":                               "",
		"packetization-mode=1":                                                   "",
	} {
		if got := profileKey(fmtp); got != want {
			t.Errorf("profileKey(%q) = %q, want %q", fmtp, got, want)
		}
	}
}

// TestOnRTCP: PLI and FIR ask for a keyframe of the track's layer; other SSRCs, NACK and REMB don't.
func TestOnRTCP(t *testing.T) {
	tr := newTrack(webrtc.RTPCodecTypeVideo, "video", "s", "q", "q", "6400")
	tr.stats.SSRC = 42
	for _, tc := range []struct {
		pkt rtcp.Packet
		key bool
	}{
		{&rtcp.PictureLossIndication{MediaSSRC: 42}, true},
		{&rtcp.PictureLossIndication{MediaSSRC: 7}, false},
		{&rtcp.FullIntraRequest{FIR: []rtcp.FIREntry{{SSRC: 7}, {SSRC: 42}}}, true},
		{&rtcp.TransportLayerNack{MediaSSRC: 42}, false},
		{&rtcp.ReceiverEstimatedMaximumBitrate{Bitrate: 1e6, SSRCs: []uint32{1, 42}}, false},
	} {
		layer, key := tr.onRTCP(tc.pkt)
		if key != tc.key || (key && layer != "q") {
			t.Errorf("%T: keyframe %v for %q", tc.pkt, key, layer)
		}
	}
	if s := tr.stats; s.PLIs != 1 || s.FIRs != 1 || s.NACKs != 1 || s.LastREMB != 1e6 {
		t.Errorf("stats %+v", s)
	}
	audio := newTrack(webrtc.RTPCodecTypeAudio, "audio", "s", "", "", "")
	if _, key := audio.onRTCP(&rtcp.PictureLossIndication{}); key {
		t.Error("audio asked for a keyframe")
	}
}
