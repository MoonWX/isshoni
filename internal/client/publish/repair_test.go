package publish

import (
	"encoding/binary"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/media/fake"
)

// recWriter is a track's webrtc.TrackLocalWriter that records what the track writes.
type recWriter struct {
	mu       sync.Mutex
	hdrs     []rtp.Header
	payloads [][]byte
	err      error
}

func (w *recWriter) WriteRTP(h *rtp.Header, payload []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	w.hdrs = append(w.hdrs, h.Clone())
	w.payloads = append(w.payloads, slices.Clone(payload))
	return h.MarshalSize() + len(payload), nil
}

func (w *recWriter) Write(b []byte) (int, error) { return len(b), nil }

// frame returns an access unit of one slice NAL unit that the payloader splits into three packets. Its bytes depend
// on n and are never 0, so no start code hides in them.
func frame(n int) fake.Packet {
	au := make([]byte, 4+3000)
	au[3], au[4] = 1, 0x41
	for i := 5; i < len(au); i++ {
		au[i] = byte(n%200) + 1
	}
	return fake.Packet{Kind: fake.Video, Layer: "f", Data: au}
}

// TestRedundantRTX: with Options.RedundantRTX a video track sends, after its first frames, a second copy of each
// frame's last packet on the RTX stream, and answers NACKs there too: RFC 4588 packets with the RTX SSRC and payload
// type, one sequence of numbers for both, the original's timestamp and marker bit, the original sequence number in
// front of the original payload, and the repaired-rtp-stream-id of the layer.
func TestRedundantRTX(t *testing.T) {
	w := &recWriter{}
	tr := newTrack(webrtc.RTPCodecTypeVideo, "video", "s", "f", "f", "6400")
	tr.repair = newRepair()
	tr.repair.seq = 7000
	b := &binding{w: w, ssrc: 11, pt: videoPT, midID: 1, ridID: 2, rridID: 3, rtxSSRC: 22, rtxPT: rtxPT}
	tr.b = b
	seq0 := tr.seq
	for i := range redundantAfter {
		if !tr.write(frame(i), uint32(i)*3000, 0) {
			t.Fatal("write on a bound track failed")
		}
	}
	if len(w.hdrs) != 3*redundantAfter {
		t.Fatalf("%d packets for the first %d frames, want three each and no second copy yet", len(w.hdrs), redundantAfter)
	}
	for i, h := range w.hdrs {
		if h.SSRC != 11 || h.PayloadType != videoPT || h.SequenceNumber != seq0+uint16(i) || h.Marker != (i%3 == 2) ||
			string(h.GetExtension(2)) != "f" || h.GetExtension(3) != nil {
			t.Fatalf("media packet %d = %+v", i, h)
		}
	}

	// The next frame is followed by the copy of its last packet.
	tr.write(frame(99), uint32(redundantAfter)*3000, 0)
	if len(w.hdrs) != 3*redundantAfter+4 {
		t.Fatalf("%d packets after frame %d, want its three and one copy", len(w.hdrs), redundantAfter)
	}
	last, lastPayload := w.hdrs[len(w.hdrs)-2], w.payloads[len(w.payloads)-2]
	rtx, payload := w.hdrs[len(w.hdrs)-1], w.payloads[len(w.payloads)-1]
	if rtx.SSRC != 22 || rtx.PayloadType != rtxPT || rtx.SequenceNumber != 7000 || rtx.Timestamp != last.Timestamp ||
		!rtx.Marker || !last.Marker {
		t.Errorf("the copy's header = %+v, want the RTX SSRC and payload type, RTX seq 7000, and the timestamp and marker "+
			"bit of %+v", rtx, last)
	}
	if string(rtx.GetExtension(3)) != "f" || rtx.GetExtension(2) != nil {
		t.Errorf("the copy's extensions: rid %q, repaired rid %q; want only the repaired rid f", rtx.GetExtension(2), rtx.GetExtension(3))
	}
	if len(payload) != len(lastPayload)+2 || binary.BigEndian.Uint16(payload) != last.SequenceNumber ||
		!slices.Equal(payload[2:], lastPayload) {
		t.Errorf("the copy's payload: %d bytes, original seq %d; want seq %d and the %d original bytes", len(payload),
			binary.BigEndian.Uint16(payload), last.SequenceNumber, len(lastPayload))
	}
	if s := tr.stats; s.Redundant != 1 || s.Packets != uint64(3*redundantAfter+3) || s.Frames != redundantAfter+1 {
		t.Errorf("stats %+v", s)
	}

	// A NACK for two packets of the second frame: two more packets of the RTX stream, in its sequence.
	before := len(w.hdrs)
	tr.answerNACK(&rtcp.TransportLayerNack{MediaSSRC: 11, Nacks: []rtcp.NackPair{{PacketID: seq0 + 3, LostPackets: 0b10}}})
	if len(w.hdrs) != before+2 {
		t.Fatalf("%d packets for a NACK of two, want 2", len(w.hdrs)-before)
	}
	for i, want := range []uint16{seq0 + 3, seq0 + 5} {
		h, p := w.hdrs[before+i], w.payloads[before+i]
		if h.SSRC != 22 || h.SequenceNumber != 7001+uint16(i) || h.Timestamp != tr.tsBase+3000 || h.Marker != (i == 1) ||
			binary.BigEndian.Uint16(p) != want || !slices.Equal(p[2:], w.payloads[int(want-seq0)]) {
			t.Errorf("retransmission %d = %+v with original seq %d, want RTX seq %d and original seq %d", i, h,
				binary.BigEndian.Uint16(p), 7001+i, want)
		}
	}
	// A NACK about another stream, or for a packet the track never sent, gets nothing.
	tr.answerNACK(&rtcp.TransportLayerNack{MediaSSRC: 12, Nacks: []rtcp.NackPair{{PacketID: seq0}}})
	tr.answerNACK(&rtcp.TransportLayerNack{MediaSSRC: 11, Nacks: []rtcp.NackPair{{PacketID: seq0 - 5}}})
	if len(w.hdrs) != before+2 || tr.stats.Retransmitted != 2 {
		t.Errorf("%d packets, %d counted, after NACKs that ask for nothing the track has", len(w.hdrs)-before, tr.stats.Retransmitted)
	}
	// The ring holds the last 1024 packets: older ones are gone.
	for i := range 400 {
		tr.write(frame(i), 0, 0)
	}
	before = len(w.hdrs)
	tr.answerNACK(&rtcp.TransportLayerNack{MediaSSRC: 11, Nacks: []rtcp.NackPair{{PacketID: seq0 + 3}, {PacketID: tr.seq - 1}}})
	if len(w.hdrs) != before+1 || binary.BigEndian.Uint16(w.payloads[before]) != tr.seq-1 {
		t.Errorf("%d packets for a NACK of an old and a new packet, want the new one alone", len(w.hdrs)-before)
	}
	// A write that fails isn't counted.
	w.err = errors.New("closed")
	tr.answerNACK(&rtcp.TransportLayerNack{MediaSSRC: 11, Nacks: []rtcp.NackPair{{PacketID: tr.seq - 2}}})
	if tr.stats.Retransmitted != 3 {
		t.Errorf("%d retransmissions counted, want 3", tr.stats.Retransmitted)
	}

	// Without RTX negotiated the track sends no second copies, and a NACK gets the packet itself again, as audio
	// always does.
	w = &recWriter{}
	audio := newTrack(webrtc.RTPCodecTypeAudio, "audio", "s", "", "", "")
	audio.repair = newRepair()
	audio.b = &binding{w: w, ssrc: 33, pt: opusPT, midID: 1}
	a0 := audio.seq
	for i := range 20 {
		audio.write(fake.Packet{Kind: fake.Audio, Data: []byte{0xfc, byte(i)}}, uint32(i)*960, 0)
	}
	audio.answerNACK(&rtcp.TransportLayerNack{MediaSSRC: 33, Nacks: []rtcp.NackPair{{PacketID: a0 + 7}}})
	if len(w.hdrs) != 21 {
		t.Fatalf("%d audio packets written, want 20 and one retransmission", len(w.hdrs))
	}
	if h := w.hdrs[20]; h.SSRC != 33 || h.PayloadType != opusPT || h.SequenceNumber != a0+7 || h.Timestamp != audio.tsBase+7*960 ||
		!slices.Equal(w.payloads[20], []byte{0xfc, 7}) {
		t.Errorf("audio retransmission = %+v %x, want packet 7 as it was", h, w.payloads[20])
	}
	if audio.stats.Redundant != 0 || audio.stats.Retransmitted != 1 {
		t.Errorf("audio stats %+v", audio.stats)
	}

	// And without the option a track answers no NACK: Pion's responder does.
	plain := newTrack(webrtc.RTPCodecTypeVideo, "video", "s", "", "f", "6400")
	plain.b = &binding{w: w, ssrc: 44, pt: videoPT}
	plain.write(frame(1), 0, 0)
	n := len(w.hdrs)
	plain.answerNACK(&rtcp.TransportLayerNack{MediaSSRC: 44, Nacks: []rtcp.NackPair{{PacketID: plain.seq - 1}}})
	if len(w.hdrs) != n {
		t.Error("a track without RedundantRTX answered a NACK itself")
	}
}

// TestPadding: with Options.Padding a video track follows every n-th frame with two padding-only packets on its
// media stream: the next sequence numbers, the frame's timestamp, no payload.
func TestPadding(t *testing.T) {
	w := &recWriter{}
	tr := newTrack(webrtc.RTPCodecTypeVideo, "video", "s", "q", "q", "6400")
	tr.padEvery = 2
	tr.b = &binding{w: w, ssrc: 11, pt: videoPT, ridID: 2}
	seq0 := tr.seq
	for i := range 4 {
		tr.write(frame(i), uint32(i)*3000, 0)
	}
	// Frames of three packets; after the second and the fourth, two of padding.
	want := []bool{false, false, false, false, false, false, true, true, false, false, false, false, false, false, true, true}
	if len(w.hdrs) != len(want) {
		t.Fatalf("%d packets, want %d", len(w.hdrs), len(want))
	}
	for i, h := range w.hdrs {
		if h.SequenceNumber != seq0+uint16(i) || h.SSRC != 11 || h.PayloadType != videoPT || string(h.GetExtension(2)) != "q" {
			t.Errorf("packet %d = %+v, want the media stream's next sequence number", i, h)
		}
		if h.Padding != want[i] {
			t.Errorf("packet %d: padding %v, want %v", i, h.Padding, want[i])
		}
		if !want[i] {
			continue
		}
		if h.PaddingSize != padSize || len(w.payloads[i]) != 0 || h.Marker || h.Timestamp != w.hdrs[i-1].Timestamp {
			t.Errorf("padding packet %d = %+v with %d payload bytes, want %d bytes of padding, no payload and the "+
				"frame's timestamp", i, h, len(w.payloads[i]), padSize)
		}
		// On the wire it is an RTP packet of nothing but its header and the padding.
		raw, err := (&rtp.Packet{Header: h}).Marshal()
		var back rtp.Packet
		if err != nil || back.Unmarshal(raw) != nil || len(back.Payload) != 0 || len(raw) != h.MarshalSize()+padSize {
			t.Errorf("padding packet %d marshals to %d bytes (%v) with %d payload bytes", i, len(raw), err, len(back.Payload))
		}
	}
	if s := tr.stats; s.Padding != 4 || s.Packets != 12 || s.Frames != 4 {
		t.Errorf("stats %+v, want 4 padding packets next to the 12 of the frames", s)
	}
	// Audio is never padded.
	audio := newTrack(webrtc.RTPCodecTypeAudio, "audio", "s", "", "", "")
	audio.padEvery = 1
	audio.b = &binding{w: &recWriter{}, ssrc: 33, pt: opusPT}
	audio.write(fake.Packet{Kind: fake.Audio, Data: []byte{0xfc}}, 0, 0)
	if audio.stats.Padding != 0 {
		t.Error("an audio track sent padding")
	}
}

func TestRTXFor(t *testing.T) {
	codec := func(mime, fmtp string, pt webrtc.PayloadType) webrtc.RTPCodecParameters {
		return webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: mime, SDPFmtpLine: fmtp}, PayloadType: pt,
		}
	}
	negotiated := []webrtc.RTPCodecParameters{
		codec(webrtc.MimeTypeH264, h264FmtpPrefix+"64001f", 96), codec("video/RTX", "apt=98", 99),
		codec("video/rtx", "x=1; APT=96", 97), codec(webrtc.MimeTypeRTX, "apt=abc", 101), codec(webrtc.MimeTypeRTX, "", 103),
		codec(webrtc.MimeTypeH264, "apt=104", 105),
	}
	for pt, want := range map[webrtc.PayloadType]uint8{96: 97, 98: 99, 100: 0, 104: 0} {
		if got, ok := rtxFor(pt, negotiated); got != want || ok != (want != 0) {
			t.Errorf("rtxFor(%d) = %d, %v; want %d", pt, got, ok, want)
		}
	}
}
