package publish

import (
	"context"
	"errors"
	"math"
	"net"
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
