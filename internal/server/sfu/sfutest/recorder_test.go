package sfutest

import (
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/media/fake"
)

// fakeMedia returns d of fake media from a small two-layer source with audio, made at once in a synctest bubble.
func fakeMedia(t *testing.T, d time.Duration) []fake.Packet {
	t.Helper()
	var out []fake.Packet
	synctest.Test(t, func(t *testing.T) {
		src, err := fake.New(fake.Config{Layers: []fake.VideoLayer{
			{RID: "f", Width: 320, Height: 180, FPS: 30, Bitrate: 400_000},
			{RID: "q", Width: 160, Height: 90, FPS: 15, Bitrate: 100_000},
		}, GOP: 500 * time.Millisecond, Audio: true})
		if err != nil {
			t.Fatal(err)
		}
		defer src.Close()
		for {
			p, err := src.Next(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if p.CaptureNS >= int64(d) {
				return
			}
			out = append(out, p)
		}
	})
	return out
}

// rtpizer turns fake packets of one layer (or the audio) into RTP packets like the Publisher: payload MTU 200, so
// frames fragment, and the marker bit on each frame's last packet.
type rtpizer struct {
	seq uint16
	pay codecs.H264Payloader
}

func (z *rtpizer) packets(p fake.Packet) []*rtp.Packet {
	ts := uint32(p.CaptureNS * 90000 / int64(time.Second))
	payloads := [][]byte{p.Data}
	if p.Kind == fake.Audio {
		ts = uint32(p.CaptureNS * 48000 / int64(time.Second))
	} else {
		payloads = z.pay.Payload(200, p.Data)
	}
	var out []*rtp.Packet
	for i, pl := range payloads {
		out = append(out, &rtp.Packet{
			Header: rtp.Header{Version: 2, SequenceNumber: z.seq, Timestamp: ts,
				Marker: p.Kind == fake.Video && i == len(payloads)-1},
			Payload: pl,
		})
		z.seq++
	}
	return out
}

// recordLayer records one layer's packets on a new Recorder and returns it.
func recordLayer(media []fake.Packet, kind fake.Kind, layer string, seq0 uint16) *Recorder {
	r := &Recorder{kind: webrtc.RTPCodecTypeVideo, keep: DefaultKeepPackets}
	if kind == fake.Audio {
		r.kind = webrtc.RTPCodecTypeAudio
	}
	z := &rtpizer{seq: seq0}
	at := time.Now()
	for _, p := range media {
		if p.Kind != kind || p.Layer != layer {
			continue
		}
		for _, pkt := range z.packets(p) {
			r.record(pkt, at, false)
			at = at.Add(time.Millisecond)
		}
	}
	return r
}

func TestRecorderChecks(t *testing.T) {
	media := fakeMedia(t, 2*time.Second)
	f := recordLayer(media, fake.Video, "f", 65500) // wraps
	q := recordLayer(media, fake.Video, "q", 0)
	a := recordLayer(media, fake.Audio, "", 65530)
	for name, r := range map[string]*Recorder{"f": f, "q": q} {
		pkts := r.Packets()
		for _, check := range []func([]Packet) error{CheckContinuous, CheckStartsOnSPS, CheckVideoMarkers} {
			if err := check(pkts); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
		frames := Frames(pkts)
		if len(frames) != map[string]int{"f": 60, "q": 30}[name] {
			t.Errorf("%s: %d frames", name, len(frames))
		}
		if bps, err := GOPBitrate(frames); err != nil || bps < 0.95*map[string]float64{"f": 4e5, "q": 1e5}[name] {
			t.Errorf("%s: GOP bitrate %.0f, %v", name, bps, err)
		}
		if s := r.Stats(); s.Expected != s.Packets || s.Gaps != 0 || s.Keyframes != 4 {
			t.Errorf("%s: stats %+v", name, s)
		}
	}
	if err := CheckAudioMarkers(a.Packets()); err != nil {
		t.Error(err)
	}
	if err := CheckContinuous(a.Packets()); err != nil {
		t.Error(err)
	}
	if s := a.Stats(); s.Packets != 100 || s.Expected != 100 {
		t.Errorf("audio stats %+v", s)
	}

	// Breakages the checks must see.
	pkts := f.Packets()
	for _, tc := range []struct {
		name  string
		check func([]Packet) error
		edit  func([]Packet) []Packet
		want  string
	}{
		{"gap", CheckContinuous, func(p []Packet) []Packet { return append(p[:10:10], p[11:]...) }, "missing"},
		{"duplicate", CheckContinuous, func(p []Packet) []Packet { p[5].Dup = true; return p }, "twice"},
		{"timestamp back", CheckContinuous, func(p []Packet) []Packet { p[20].TS -= 90000; return p }, "goes back"},
		{"starts mid-GOP", CheckStartsOnSPS, func(p []Packet) []Packet { return p[firstDelta(p):] }, "without an SPS"},
		{"switch without SPS", CheckStartsOnSPS, func(p []Packet) []Packet {
			for i := firstDelta(p); i < len(p); i++ {
				if p[i].HasMark {
					p[i].Mark.RID = "q"
					break
				}
			}
			return p
		}, "switch f → q"},
		{"two markers", CheckVideoMarkers, func(p []Packet) []Packet {
			i := firstDelta(p)
			p[i+1].HasMark, p[i+1].Mark = true, p[i].Mark
			return p
		}, "2 markers"},
		{"lost marker bit", CheckVideoMarkers, func(p []Packet) []Packet {
			for i := range p {
				if p[i].Marker {
					p[i].Marker = false
					break
				}
			}
			return p
		}, "RTP marker bits"},
		{"frame skipped", CheckVideoMarkers, func(p []Packet) []Packet {
			for i := firstDelta(p); i < len(p); i++ {
				if p[i].HasMark {
					p[i].Mark.Frame += 2
					break
				}
			}
			return p
		}, "follows frame"},
		{"wrong keyframe flag", CheckVideoMarkers, func(p []Packet) []Packet {
			p[firstDelta(p)].Mark.Keyframe = true
			return p
		}, "keyframe flag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.check(tc.edit(append([]Packet(nil), pkts...)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %v, want %q", err, tc.want)
			}
		})
	}
	ap := a.Packets()
	ap[3].HasMark = false
	if err := CheckAudioMarkers(ap); err == nil {
		t.Error("audio without a marker passed")
	}
}

// TestWithoutCutFrames: a layer switch may cut the old layer's last frame short. WithoutCutFrames leaves out exactly
// such a frame, with or without its fake marker, so that the checks of whole frames pass; a frame that lost a packet
// anywhere else stays, and still fails them.
func TestWithoutCutFrames(t *testing.T) {
	media := fakeMedia(t, 2*time.Second)
	f, q := recordLayer(media, fake.Video, "f", 0).Packets(), recordLayer(media, fake.Video, "q", 0).Packets()
	// keyStarts returns the indices of the packets that start a keyframe (GOP 500 ms: at 0, 0.5, 1 and 1.5 s).
	keyStarts := func(p []Packet) []int {
		var out []int
		for i := range p {
			if p[i].KeyStart {
				out = append(out, i)
			}
		}
		return out
	}
	fKeys, qKeys := keyStarts(f), keyStarts(q)
	if len(fKeys) != 4 || len(qKeys) != 4 {
		t.Fatalf("%d and %d keyframes, want 4 each", len(fKeys), len(qKeys))
	}
	// switched is what a viewer gets when the SFU goes from f to q's keyframe at 1.5 s after the first n packets of
	// f: one stream, renumbered.
	switched := func(n int) []Packet {
		out := append(append([]Packet(nil), f[:n]...), q[qKeys[3]:]...)
		for i := range out {
			out[i].Seq = uint16(65000 + i)
		}
		return out
	}
	// midFrame: f up to the first packet of a delta frame of several packets, which carries the frame's marker.
	midFrame := -1
	for i := fKeys[1]; i+1 < fKeys[2]; i++ {
		if f[i].HasMark && !f[i].Mark.Keyframe && !f[i].Marker {
			midFrame = i + 1
			break
		}
	}
	if midFrame < 0 {
		t.Fatal("no delta frame of more than one packet")
	}
	for name, pkts := range map[string][]Packet{
		"cut in a delta frame":                     switched(midFrame),
		"cut in a keyframe, before its marker":     switched(fKeys[2] + 1),
		"a clean switch":                           switched(fKeys[2]),
		"a stream of one layer":                    f,
		"a stream whose last frame is still short": f[:midFrame],
	} {
		if err := CheckContinuous(pkts); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		whole := WithoutCutFrames(pkts)
		cut := strings.HasPrefix(name, "cut")
		if got := len(pkts) - len(whole); (got > 0) != cut {
			t.Errorf("%s: %d packets left out", name, got)
		}
		if err := CheckVideoMarkers(pkts); (err != nil) != (cut || strings.Contains(name, "short")) {
			t.Errorf("%s: CheckVideoMarkers = %v before the cut frame is left out", name, err)
		}
		if strings.Contains(name, "short") {
			continue // WithoutPartialTail's
		}
		for _, check := range []func([]Packet) error{CheckVideoMarkers, CheckStartsOnSPS} {
			if err := check(whole); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
	}
	// A frame of the old layer that lost its last packet earlier is no cut frame.
	lossy := switched(fKeys[2])
	lossy = append(lossy[:midFrame:midFrame], lossy[midFrame+1:]...)
	for lossy[midFrame-1].TS == lossy[midFrame].TS {
		lossy = append(lossy[:midFrame:midFrame], lossy[midFrame+1:]...)
	}
	if got := WithoutCutFrames(lossy); len(got) != len(lossy) || CheckVideoMarkers(got) == nil {
		t.Errorf("a frame that lost packets in mid-stream: %d of %d packets kept, CheckVideoMarkers = %v", len(got),
			len(lossy), CheckVideoMarkers(got))
	}
}

// firstDelta returns the index of the first packet of the first delta frame that carries a marker.
func firstDelta(p []Packet) int {
	for i := range p {
		if p[i].HasMark && !p[i].Mark.Keyframe {
			return i
		}
	}
	return -1
}

// TestSeqTracker: gaps, reordering, RTX recovery, duplicates, wraparound and the final loss after 1 s.
func TestSeqTracker(t *testing.T) {
	r := &Recorder{kind: webrtc.RTPCodecTypeAudio, keep: 3}
	at := time.Now() // Stats expires against the real clock
	rec := func(seq uint16, rtx bool) {
		r.record(&rtp.Packet{Header: rtp.Header{SequenceNumber: seq}}, at, rtx)
	}
	rec(65534, false)
	rec(65535, false)
	rec(2, false) // 0 and 1 missing
	rec(1, true)  // recovered through RTX
	rec(1, false) // duplicate
	rec(5, false) // 3 and 4 missing
	at = at.Add(500 * time.Millisecond)
	rec(4, false) // late, reordered
	s := r.Stats()
	if s.Gaps != 4 || s.Recovered != 2 || s.RecoveredRTX != 1 || s.Duplicates != 1 || s.Expected != 8 ||
		s.RTX != 1 || s.Packets != 7 {
		t.Errorf("stats %+v", s)
	}
	if n := len(r.Packets()); n > 3 {
		t.Errorf("kept %d packets, want at most 3", n)
	}
	// 0 and 3 are still missing; a packet 1 s later makes them final.
	at = at.Add(FinalLossAfter)
	rec(6, false)
	if s := r.Stats(); s.Lost != 2 {
		t.Errorf("lost %d, want 2", s.Lost)
	}
	// A huge jump counts at most maxMissing as pending, the rest lost at once.
	rec(6+maxMissing+11, false)
	if s := r.Stats(); s.Lost != 2+10 || s.Gaps != 4+maxMissing+10 {
		t.Errorf("after a jump: %+v", s)
	}
}

func TestClassifyH264(t *testing.T) {
	sps := []byte{0x67, 0x42, 0xe0, 0x1f}
	for _, tc := range []struct {
		name    string
		payload []byte
		nal     uint8
		key     bool
	}{
		{"empty", nil, 0, false},
		{"SPS", sps, 7, true},
		{"STAP-A with SPS", append([]byte{0x78, 0, 4}, sps...), 24, true},
		{"STAP-A with PPS only", []byte{0x78, 0, 2, 0x68, 0xce}, 24, false},
		{"STAP-A too short", []byte{0x78, 0, 9, 0x67}, 24, false},
		{"STAP-A zero size", []byte{0x78, 0, 0, 0x67, 0x42}, 24, false},
		{"FU-A middle", []byte{0x7c, 0x05, 'I', 'S', 'H', 'N'}, 28, false},
		{"slice without marker", []byte{0x41, 1, 2, 3}, 1, false},
	} {
		var p Packet
		classifyH264(&p, tc.payload)
		if p.NAL != tc.nal || p.KeyStart != tc.key || p.HasMark {
			t.Errorf("%s: %+v", tc.name, p)
		}
	}
}

func TestNTP(t *testing.T) {
	sr := SenderReport{NTP: (ntpEpochOffset+10)<<32 | 1<<31}
	if got := sr.Time(); !got.Equal(time.Unix(10, 5e8)) {
		t.Errorf("Time = %v", got)
	}
	base := time.Unix(100, 0)
	srs := []SenderReport{
		{Arrival: base, NTP: ntpEpochOffset << 32, RTP: 1000},
		{Arrival: base.Add(time.Second), NTP: (ntpEpochOffset + 1) << 32, RTP: 91000},
	}
	start := int64(ntpEpochOffset) * int64(time.Second)
	for _, tc := range []struct {
		ts   uint32
		at   time.Time
		want int64
	}{
		{1000 + 45000, base.Add(500 * time.Millisecond), start + int64(500*time.Millisecond)},   // first SR
		{91000 + 9000, base.Add(1100 * time.Millisecond), start + int64(1100*time.Millisecond)}, // second SR
		{1000 + (1<<32 - 90000), base.Add(-time.Second), start - int64(time.Second)},            // before any SR
	} {
		if got, ok := mapNTP(srs, tc.ts, tc.at, 90000); !ok || got != tc.want {
			t.Errorf("mapNTP(%d) = %d, want %d", tc.ts, got, tc.want)
		}
	}
	if _, ok := mapNTP(nil, 0, base, 90000); ok {
		t.Error("mapNTP without SRs")
	}
}

// TestRecordSR checks that SRs are kept with their arrival time.
func TestRecordSR(t *testing.T) {
	r := &Recorder{kind: webrtc.RTPCodecTypeVideo}
	at := time.Now()
	r.recordSR(&rtcp.SenderReport{SSRC: 1, NTPTime: 7, RTPTime: 8, PacketCount: 9, OctetCount: 10}, at)
	if srs := r.SRs(); len(srs) != 1 || srs[0] != (SenderReport{Arrival: at, NTP: 7, RTP: 8, Packets: 9, Octets: 10}) {
		t.Errorf("SRs %+v", srs)
	}
	if r.Stats().SRs != 1 {
		t.Error("SR not counted")
	}
}

func FuzzClassifyH264(f *testing.F) {
	f.Add([]byte{0x78, 0, 4, 0x67, 0x42, 0xe0, 0x1f})
	f.Add([]byte{0x7c, 0x85, 'I', 'S', 'H', 'N', 'f', 0, 0, 0, 3, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{0x41, 'I', 'S', 'H', 'N'})
	f.Fuzz(func(t *testing.T, payload []byte) {
		var p Packet
		classifyH264(&p, payload)
		if p.KeyStart && p.NAL != nalSPS && p.NAL != nalSTAPA {
			t.Fatalf("keyframe start in a NAL unit of type %d", p.NAL)
		}
		if p.HasMark && p.NAL != nalSlice && p.NAL != nalIDR && p.NAL != nalFUA {
			t.Fatalf("marker in a NAL unit of type %d", p.NAL)
		}
	})
}
