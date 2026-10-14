package sfu

import (
	"encoding/binary"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// The unit tests of repair and sync (README S63; 02 §17 rtx_test): what a viewer's NACKs get from the packet cache,
// the layout of a retransmission, the limits per sequence number and the budget (02 §9.5); and the sender reports a
// DownTrack forwards (02 §9.6). Like media_test.go they build Layers and DownTracks by hand on a real SFU; the tests
// with real PeerConnections are repair_test.go's (integration 4, 5, 6 and 19).

// copyWriter is a DownTrack's webrtc.TrackLocalWriter that keeps a copy of every payload: an RTX payload is built
// in a pooled buffer, which the DownTrack takes back after the write. It is safe for a writer goroutine.
type copyWriter struct {
	mu       sync.Mutex
	hdrs     []rtp.Header
	payloads [][]byte
}

func (w *copyWriter) WriteRTP(h *rtp.Header, payload []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.hdrs = append(w.hdrs, h.Clone())
	w.payloads = append(w.payloads, slices.Clone(payload))
	return h.MarshalSize() + len(payload), nil
}

func (w *copyWriter) Write(b []byte) (int, error) { return len(b), nil }

// written returns how many packets were written, and the header and payload of packet i when there is one.
func (w *copyWriter) written() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.hdrs)
}

func (w *copyWriter) packet(i int) (rtp.Header, []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.hdrs[i], w.payloads[i]
}

// The test viewer's RTX stream, next to testDownTrack's media SSRC 0x7000: its SSRC, and the RTX payload type of
// each of chromePTs' media payload types.
const rtxTestSSRC = 0x7100

var chromeRTXPTs = map[uint8]uint8{96: 97, 98: 99, 100: 101, 102: 103, 104: 105}

// rtxRig is one share with a full layer and one viewer's video DownTrack that forwards it, with RTX negotiated. Its
// clock starts at the real monoNow, which the writer's munger uses, and moves by hand.
type rtxRig struct {
	t     *testing.T
	s     *SFU
	sh    *Share
	f     *Layer
	d     *DownTrack
	b     *binding
	w     *copyWriter
	rtcp  *rtcpRecorder // the sub PC, as far as sender reports go
	state rtpWriteState
	now   int64
	pace  int64  // how far apart the publisher's packets arrive: 10 ms unless a test sends hundreds
	seq   uint16 // the publisher's next sequence number and timestamp on f
	ts    uint32
}

func newRTXRig(t *testing.T) *rtxRig {
	t.Helper()
	s, _ := newTicklessSFU(t)
	viewer, _ := join(t, s, "lounge", "bob", "c-b", RoleViewer)
	sh := testShare(t, s, "s_1")
	f, _ := testLayer(t, sh, SlotF, 0xf00)
	sh.keyframe(f, ProfileHigh)
	d, _ := testDownTrack(t, viewer, sh, webrtc.RTPCodecTypeVideo)
	r := &rtxRig{
		t: t, s: s, sh: sh, f: f, d: d, w: &copyWriter{}, rtcp: &rtcpRecorder{}, now: monoNow(), pace: tick, seq: 500,
		ts: 90000,
	}
	r.b = d.binding.Load()
	r.b.writer, r.b.rtcp, r.b.rtxSSRC, r.b.rtxPTFor = r.w, r.rtcp, rtxTestSSRC, chromeRTXPTs
	d.setWant(QualityHigh)
	return r
}

// send feeds one packet through layer f and the writer, a tick after the last, and returns its upstream sequence
// number.
func (r *rtxRig) send(payload []byte, marker bool) uint16 {
	r.t.Helper()
	r.now += r.pace
	seq := r.seq
	r.f.handleRTP(rtpPkt(seq, r.ts, marker, payload), ProfileHigh, r.now)
	r.seq++
	r.ts += 3000
	drain(r.d, &r.state)
	return seq
}

// start sends the keyframe start and n delta packets, and returns the own sequence number and timestamp of the first:
// the stream's own numbers go on from there one by one, 3000 ticks per packet.
func (r *rtxRig) start(n int) (seq0 uint16, ts0 uint32) {
	r.t.Helper()
	r.send(keyPayload(r.t), false)
	for range n {
		r.send(deltaPayload, false)
	}
	if got := r.w.written(); got != n+1 {
		r.t.Fatalf("%d packets forwarded, want %d", got, n+1)
	}
	h, _ := r.w.packet(0)
	return h.SequenceNumber, h.Timestamp
}

// nack has the viewer ask for own sequence numbers, as one NACK without a limit on their number, and runs the writer
// over what that queued. It returns how many packets were written for it.
func (r *rtxRig) nack(seqs ...uint16) int {
	r.t.Helper()
	before := r.w.written()
	left := len(seqs)
	r.d.handleNACK(r.d.binding.Load(), rtcp.NackPairsFromSequenceNumbers(seqs), &left, r.now)
	r.drainRTX()
	return r.w.written() - before
}

func (r *rtxRig) drainRTX() {
	for {
		select {
		case it := <-r.d.rtxQueue:
			r.d.retransmit(&r.state, it)
		default:
			return
		}
	}
}

// counts returns the DownTrack's repair counters as a comparable value.
func (r *rtxRig) counts() RepairState {
	st := r.d.repairState()
	st.SRs = 0
	return st
}

// TestRTXLayout: a retransmission on the RTX stream is the packet of RFC 4588: the RTX SSRC and the RTX payload type
// of the original's payload type, a sequence number of the RTX stream's own, the original's timestamp and marker
// bit, and a payload that starts with the original sequence number and goes on with the original payload, byte for
// byte from the cache. Without RTX negotiated it is the original packet again.
func TestRTXLayout(t *testing.T) {
	r := newRTXRig(t)
	seq0, ts0 := r.start(2)
	big := make([]byte, 1100)
	for i := range big {
		big[i] = byte(i)
	}
	big[0] = 0x41
	r.send(big, true) // own seq0+3, with the marker bit
	r.send(deltaPayload, false)
	before := r.w.written()

	if n := r.nack(seq0 + 3); n != 1 {
		t.Fatalf("%d packets for one NACK, want 1", n)
	}
	h, payload := r.w.packet(before)
	if h.SSRC != rtxTestSSRC || h.PayloadType != 105 || h.SequenceNumber != 0 || h.Timestamp != ts0+9000 || !h.Marker ||
		h.Version != 2 || h.Padding || len(h.CSRC) != 0 {
		t.Errorf("RTX header = %+v, want SSRC %#x, PT 105, the RTX stream's seq 0, ts %d and the marker bit", h, rtxTestSSRC, ts0+9000)
	}
	if ids := h.GetExtensionIDs(); !slices.Equal(ids, []uint8{3}) || len(h.GetExtension(3)) != 3 {
		t.Errorf("RTX header extensions = %v, want abs-send-time alone", ids)
	}
	if len(payload) != len(big)+2 || binary.BigEndian.Uint16(payload) != seq0+3 || !slices.Equal(payload[2:], big) {
		t.Errorf("RTX payload: %d bytes starting %x, want the original seq %d and the %d original bytes", len(payload),
			payload[:min(4, len(payload))], seq0+3, len(big))
	}
	if got, want := r.counts(), (RepairState{NACKs: 1, Served: 1}); got != want {
		t.Errorf("counters = %+v, want %+v", got, want)
	}
	if p, b := r.d.stats.rtxPackets.Load(), r.d.stats.rtxBytes.Load(); p != 1 || b != uint64(len(big)+2) {
		t.Errorf("egress as rtx: %d packets, %d bytes; want 1 and %d", p, b, len(big)+2)
	}
	if got := r.d.stats.packets.Load(); got != 5 {
		t.Errorf("%d media packets counted, want the 5 forwarded: a retransmission is none", got)
	}

	// The next one, of another packet, has the RTX stream's next sequence number and that packet's own timestamp.
	// A keyframe start is retransmitted like any packet.
	r.nack(seq0)
	h, payload = r.w.packet(before + 1)
	if h.SequenceNumber != 1 || h.SSRC != rtxTestSSRC || h.Timestamp != ts0 || h.Marker ||
		binary.BigEndian.Uint16(payload) != seq0 || !isKeyframeStart(payload[2:]) {
		t.Errorf("second RTX packet = %+v", h)
	}

	// One NACK item names up to 17 sequence numbers: all of them are answered, in order.
	before = r.w.written()
	left := nackBatch
	r.d.handleNACK(r.b, []rtcp.NackPair{{PacketID: seq0 + 1, LostPackets: 0b101}}, &left, r.now)
	r.drainRTX()
	var osn []uint16
	for i := before; i < r.w.written(); i++ {
		_, payload := r.w.packet(i)
		osn = append(osn, binary.BigEndian.Uint16(payload))
	}
	if !slices.Equal(osn, []uint16{seq0 + 1, seq0 + 2, seq0 + 4}) || left != nackBatch-3 {
		t.Errorf("a NACK for %d with bitmask 101 retransmitted %v (%d left of the batch)", seq0+1, osn, left)
	}

	// Without RTX for the payload type (a viewer that negotiated none): the original packet again, with the same
	// payload slice as the cache.
	r.b.rtxPTFor = map[uint8]uint8{}
	plain := &captureWriter{}
	r.b.writer = plain
	r.now += int64(nackMinGap)
	r.nack(seq0 + 3)
	if len(plain.hdrs) != 1 {
		t.Fatalf("%d plain retransmissions, want 1", len(plain.hdrs))
	}
	if h := plain.hdrs[0]; h.SSRC != 0x7000 || h.PayloadType != 104 || h.SequenceNumber != seq0+3 || h.Timestamp != ts0+9000 || !h.Marker {
		t.Errorf("plain retransmission = %+v, want the original SSRC, PT 104, seq %d and ts %d", h, seq0+3, ts0+9000)
	}
	if &plain.payloads[0][0] != &r.f.cache.get(503, r.now).payload[0] {
		t.Error("a plain retransmission copies the payload, want the cache's slice")
	}
	// And with an RTX payload type but no RTX SSRC (Pion disabled RTX on the sender): plain too.
	r.b.rtxPTFor, r.b.rtxSSRC = chromeRTXPTs, 0
	r.now += int64(nackMinGap)
	r.nack(seq0 + 3)
	if len(plain.hdrs) != 2 || plain.hdrs[1].SSRC != 0x7000 || plain.hdrs[1].SequenceNumber != seq0+3 {
		t.Errorf("without an RTX SSRC: %+v", plain.hdrs)
	}

	// A retransmission that Pion takes without sending it (the transport can't send right now), or refuses (the
	// sender stopped), is counted as that and not as served: the viewer asks again.
	served := r.d.stats.nackServed.Load()
	plain.taken = 1
	r.now += int64(nackMinGap)
	r.nack(seq0 + 4)
	plain.err = errors.New("closed")
	r.now += int64(nackMinGap)
	r.nack(seq0 + 4)
	if st := &r.d.stats; st.unsent.Load() != 1 || st.writeErrors.Load() != 1 || st.nackServed.Load() != served || len(plain.hdrs) != 2 {
		t.Errorf("after a retransmission that wasn't sent and one that failed: %d unsent, %d write errors, %d more served",
			st.unsent.Load(), st.writeErrors.Load(), st.nackServed.Load()-served)
	}
}

// TestRTXAudio: audio has no RTX stream. A NACK for an audio DownTrack's packet gets the packet itself again, and
// the sender report that follows counts it, since it left on the media SSRC.
func TestRTXAudio(t *testing.T) {
	s, _ := newTicklessSFU(t)
	viewer, _ := join(t, s, "lounge", "bob", "c-b", RoleViewer)
	sh := testShare(t, s, "s_1")
	a, _ := testLayer(t, sh, SlotAudio, 0xaa)
	d, w := testDownTrack(t, viewer, sh, webrtc.RTPCodecTypeAudio)
	sent := &rtcpRecorder{}
	d.binding.Load().rtcp = sent
	d.setWant(QualityHigh)
	var state rtpWriteState
	now := monoNow()
	for i := range 10 {
		a.handleRTP(rtpPkt(uint16(100+i), uint32(i)*960, false, []byte{0xfc, byte(i)}), "", now)
		now += tick
	}
	drain(d, &state)
	if len(w.hdrs) != 10 {
		t.Fatalf("%d audio packets forwarded", len(w.hdrs))
	}
	lost := w.hdrs[4]
	d.handleRTCP([]rtcp.Packet{&rtcp.TransportLayerNack{MediaSSRC: 0x7001, Nacks: []rtcp.NackPair{{PacketID: lost.SequenceNumber}}}})
	if len(d.rtxQueue) != 1 {
		t.Fatalf("%d retransmissions queued for an audio NACK, want 1", len(d.rtxQueue))
	}
	d.retransmit(&state, <-d.rtxQueue)
	if len(w.hdrs) != 11 {
		t.Fatalf("%d packets written after the NACK, want 11", len(w.hdrs))
	}
	if h := w.hdrs[10]; h.SSRC != 0x7001 || h.PayloadType != 111 || h.SequenceNumber != lost.SequenceNumber ||
		h.Timestamp != lost.Timestamp || !slices.Equal(w.payloads[10], []byte{0xfc, 4}) {
		t.Errorf("audio retransmission = %+v %x, want packet %+v again", h, w.payloads[10], lost)
	}
	a.handleSR(&rtcp.SenderReport{SSRC: 0xaa, NTPTime: 77 << 32, RTPTime: 960}, now)
	drain(d, &state)
	srs := sent.srs()
	if len(srs) != 1 || srs[0].PacketCount != 11 || srs[0].OctetCount != 22 {
		t.Errorf("sender reports after the retransmission = %+v, want one that counts 11 packets and 22 bytes", srs)
	}
}

// TestNACKLimits: the limits per sequence number of 02 §9.5. A NACK that repeats within 20 ms, or within half the
// round-trip time when that is longer, is not answered again: it was sent before the first answer could arrive. And
// no sequence number is retransmitted more than three times.
func TestNACKLimits(t *testing.T) {
	r := newRTXRig(t)
	seq0, _ := r.start(5)
	step := func(d time.Duration, seq uint16, want int, why string) {
		t.Helper()
		r.now += int64(d)
		if n := r.nack(seq); n != want {
			t.Fatalf("%s: %d retransmissions, want %d", why, n, want)
		}
	}
	step(0, seq0+1, 1, "the first NACK")
	step(0, seq0+1, 0, "the same NACK again at once")
	step(19*time.Millisecond, seq0+1, 0, "again 19 ms later")
	step(time.Millisecond, seq0+1, 1, "again 20 ms after the retransmission")
	step(20*time.Millisecond, seq0+1, 1, "a third time")
	step(20*time.Millisecond, seq0+1, 0, "a fourth time")
	step(time.Second, seq0+1, 0, "and a second later, with the packet still in the cache")
	if got, want := r.counts(), (RepairState{NACKs: 7, Served: 3, Skipped: 4}); got != want {
		t.Errorf("counters = %+v, want %+v", got, want)
	}
	// Another sequence number has its own count, and one NACK may ask for both.
	r.send(deltaPayload, false) // own seq0+6, in the cache now
	if n := r.nack(seq0+1, seq0+6); n != 1 {
		t.Errorf("a NACK for a spent and a new sequence number: %d retransmissions, want 1", n)
	}

	// With a round-trip time of 100 ms the same NACK is answered again only after 50 ms.
	r.b.pc.rtt.Store(int64(100 * time.Millisecond))
	seq := seq0 + 7
	r.send(deltaPayload, false)
	step(0, seq, 1, "RTT 100 ms: the first NACK")
	step(20*time.Millisecond, seq, 0, "RTT 100 ms: again after 20 ms")
	step(29*time.Millisecond, seq, 0, "RTT 100 ms: again after 49 ms")
	step(time.Millisecond, seq, 1, "RTT 100 ms: again after 50 ms")
	// A short round-trip time doesn't lower the 20 ms.
	r.b.pc.rtt.Store(int64(2 * time.Millisecond))
	step(19*time.Millisecond, seq, 0, "RTT 2 ms: again after 19 ms")
	step(time.Millisecond, seq, 1, "RTT 2 ms: again after 20 ms")
}

// TestNACKRecordAge: what a DownTrack remembers of a retransmission counts for nackRecordAge and no longer. Until
// then a viewer that goes on asking for a packet it was sent three times is skipped, also when the cache has let go
// of the packet; after that its NACK is a miss, like any for a packet that old. And the record is long gone when the
// 16-bit sequence number comes around: the packet that has it 65536 packets later is another packet, and is
// retransmitted three times like any, whatever was sent under that number before.
func TestNACKRecordAge(t *testing.T) {
	r := newRTXRig(t)
	seq0, _ := r.start(5)
	spent, once := seq0+1, seq0+2
	for i := range nackMaxSends {
		if n := r.nack(spent); n != 1 {
			t.Fatalf("NACK %d: %d retransmissions, want 1", i+1, n)
		}
		r.now += int64(nackMinGap)
	}
	if n := r.nack(once); n != 1 {
		t.Fatalf("%d retransmissions of another sequence number, want 1", n)
	}
	// One nanosecond short of nackRecordAge after the third retransmission, and then at it.
	r.now += int64(nackRecordAge-nackMinGap) - 1
	if n := r.nack(spent); n != 0 {
		t.Fatalf("%d retransmissions long after the cache let go of the packet", n)
	}
	if got, want := r.counts(), (RepairState{NACKs: 5, Served: 4, Skipped: 1}); got != want {
		t.Errorf("just before the record expires: counters = %+v, want %+v", got, want)
	}
	r.now++
	if n := r.nack(spent); n != 0 {
		t.Fatalf("%d retransmissions long after the cache let go of the packet", n)
	}
	if got, want := r.counts(), (RepairState{NACKs: 6, Served: 4, Skipped: 1, Missed: 1}); got != want {
		t.Errorf("once the record has expired: counters = %+v, want %+v", got, want)
	}

	// 65536 packets later, at 900 packets a second: 73 s. The packets that have the two own sequence numbers now are
	// told apart from the first by their payloads.
	r.pace = int64(time.Second) / 900
	for range 1<<16 - 5 {
		r.send(deltaPayload, false) // own seq0+6 … seq0+65536, which is seq0 again
	}
	again := map[uint16][]byte{spent: {0x41, 0x01}, once: {0x41, 0x02}}
	r.send(again[spent], false)
	r.send(again[once], true)
	for range 3 {
		r.send(deltaPayload, false)
	}
	for round := range nackMaxSends {
		before := r.w.written()
		if n := r.nack(spent, once); n != 2 {
			t.Fatalf("a turn of the sequence number later, NACK %d: %d retransmissions, want 2 (counters %+v)", round+1, n,
				r.counts())
		}
		for i := before; i < before+2; i++ {
			_, payload := r.w.packet(i)
			if osn := binary.BigEndian.Uint16(payload); !slices.Equal(payload[2:], again[osn]) {
				t.Errorf("own seq %d retransmitted with payload %x, want the packet that has that number now, %x", osn,
					payload[2:], again[osn])
			}
		}
		r.now += int64(nackMinGap)
	}
	if n := r.nack(spent, once); n != 0 {
		t.Errorf("%d retransmissions after three of each packet", n)
	}
	if got, want := r.counts(), (RepairState{NACKs: 14, Served: 10, Skipped: 3, Missed: 1}); got != want {
		t.Errorf("counters = %+v, want %+v", got, want)
	}
}

// TestNACKMisses: what the cache no longer has, or never had, is a miss: a sequence number the DownTrack hasn't sent
// yet, one whose packet arrived more than 1.5 s ago, and one the munger has no epoch for. A viewer that keeps asking
// for a packet it got three times is skipped, not missed, also when the cache has let go of the packet.
func TestNACKMisses(t *testing.T) {
	r := newRTXRig(t)
	seq0, _ := r.start(5)
	if n := r.nack(seq0+6, seq0+100, seq0-1); n != 0 {
		t.Fatalf("%d retransmissions of sequence numbers that were never sent", n)
	}
	if got, want := r.counts(), (RepairState{NACKs: 3, Missed: 3}); got != want {
		t.Errorf("counters = %+v, want %+v", got, want)
	}
	for range 3 {
		if n := r.nack(seq0 + 2); n != 1 {
			t.Fatalf("%d retransmissions, want 1", n)
		}
		r.now += int64(nackMinGap)
	}
	r.now += int64(cacheMaxAge)
	if n := r.nack(seq0+2, seq0+3); n != 0 {
		t.Fatalf("%d retransmissions of packets that arrived more than 1.5 s ago", n)
	}
	if got, want := r.counts(), (RepairState{NACKs: 8, Served: 3, Missed: 4, Skipped: 1}); got != want {
		t.Errorf("counters = %+v, want %+v: the packet sent three times is skipped, the other is a miss", got, want)
	}
	// A DownTrack that never forwarded anything has nothing to retransmit.
	fresh, _ := testDownTrack(t, r.d.sub.conn, r.sh, webrtc.RTPCodecTypeVideo)
	fresh.handleRTCP([]rtcp.Packet{&rtcp.TransportLayerNack{MediaSSRC: 0x7000, Nacks: []rtcp.NackPair{{PacketID: 1, LostPackets: 0xffff}}}})
	if st := fresh.repairState(); st.NACKs != 17 || st.Missed != 17 || len(fresh.rtxQueue) != 0 {
		t.Errorf("a NACK to a DownTrack that never sent: %+v", st)
	}
}

// TestNACKAcrossEpochs: the sequence map (02 §9.4). A NACK for a packet of the layer the viewer got before a switch
// is answered from that layer's cache, with the timestamp that packet had on the viewer's timeline, and so is one
// for a packet from before a padding gap, whose own sequence number is no longer the upstream one plus the epoch's
// first offset.
func TestNACKAcrossEpochs(t *testing.T) {
	r := newRTXRig(t)
	seq0, ts0 := r.start(3) // f: upstream 500–503, own seq0–seq0+3
	// Padding upstream (504), then two more packets (505, 506): own seq0+4 and seq0+5, without a gap.
	r.now += tick
	r.f.handleRTP(rtpPkt(r.seq, r.ts, false, nil), ProfileHigh, r.now)
	r.seq++
	drain(r.d, &r.state)
	marked := []byte{0x41, 0xaa, 0xbb}
	r.send(marked, false)
	r.send(deltaPayload, true)
	// The viewer goes to the preview layer: upstream 9000 on, own seq0+6 on.
	q, _ := testLayer(t, r.sh, SlotQ, 0xf01)
	r.d.setWant(QualityLow)
	r.now += tick
	q.handleRTP(rtpPkt(9000, 7_000_000, false, keyPayload(t)), ProfileHigh, r.now)
	q.handleRTP(rtpPkt(9001, 7_003_000, true, []byte{0x41, 0x51}), ProfileHigh, r.now)
	drain(r.d, &r.state)
	if got := r.w.written(); got != 8 {
		t.Fatalf("%d packets forwarded, want 8", got)
	}
	want := map[uint16]struct {
		ts      uint32
		payload []byte
	}{
		seq0 + 2: {ts0 + 6000, deltaPayload},  // f, before the padding
		seq0 + 4: {ts0 + 12000, marked},       // f, after the padding: upstream 505
		seq0 + 7: {0, []byte{0x41, 0x51}},     // q; its timestamp is the q epoch's
		seq0 + 6: {0, keyPayload(t)},          // q's keyframe start
		seq0 + 5: {ts0 + 15000, deltaPayload}, // f's last packet
	}
	hq, _ := r.w.packet(6)
	before := r.w.written()
	if n := r.nack(seq0+2, seq0+4, seq0+5, seq0+6, seq0+7); n != 5 {
		t.Fatalf("%d retransmissions, want 5 (counters %+v)", n, r.counts())
	}
	for i := before; i < r.w.written(); i++ {
		h, payload := r.w.packet(i)
		osn := binary.BigEndian.Uint16(payload)
		w, ok := want[osn]
		if !ok {
			t.Fatalf("a retransmission of own seq %d, which nobody asked for", osn)
		}
		if w.ts == 0 {
			w.ts = hq.Timestamp + uint32(osn-(seq0+6))*3000
		}
		if h.Timestamp != w.ts || !slices.Equal(payload[2:], w.payload) || h.PayloadType != 105 {
			t.Errorf("own seq %d retransmitted with ts %d and payload %x, want ts %d and %x", osn, h.Timestamp, payload[2:],
				w.ts, w.payload)
		}
		delete(want, osn)
	}
}

// TestRTXBudget: the retransmission budget of 02 §9.5 is a token bucket that fills at a quarter of what the layer
// may carry: 2 Mbit/s for an 8 Mbit/s full layer, with 250 kB in reserve. What goes beyond is skipped. After 10 s of
// an idle layer, NACKs for 30 packets of one large frame are all served: the reserve is there for exactly that, when
// the measured bitrate is still the idle one.
func TestRTXBudget(t *testing.T) {
	r := newRTXRig(t)
	seq0, _ := r.start(0)
	r.pace = int64(100 * time.Microsecond)
	payload := make([]byte, 1200)
	payload[0] = 0x41
	var seqs []uint16
	for i := range 250 {
		r.send(payload, false)
		seqs = append(seqs, seq0+1+uint16(i))
	}
	// 250 kB of budget hold 208 packets of 1200 bytes.
	const depth = fullMaxBitrate / rtxShare / 8 // bytes
	if n := r.nack(seqs...); n != depth/1200 {
		t.Fatalf("%d of 250 retransmissions fit the full budget, want %d", n, depth/1200)
	}
	if got, want := r.counts(), (RepairState{NACKs: 250, Served: 208, Skipped: 42}); got != want {
		t.Errorf("counters = %+v, want %+v", got, want)
	}
	// 100 ms later the bucket has 25 kB more: with the 400 bytes that were left, 21 more packets.
	r.now += int64(100 * time.Millisecond)
	if n := r.nack(seqs[208:]...); n != 21 {
		t.Errorf("%d retransmissions 100 ms later, want 21", n)
	}
	// A burst of NACK packets a microsecond apart doesn't lose the budget's income to rounding: 2 bits each time.
	r.d.nack.mu.Lock()
	r.d.nack.bits = 0
	r.d.nack.mu.Unlock()
	for range 1000 {
		r.now += int64(time.Microsecond)
		r.nack(seq0 + 300) // never sent: a miss, but the bucket is filled
	}
	r.d.nack.mu.Lock()
	bits := r.d.nack.bits
	r.d.nack.mu.Unlock()
	if bits != 2000 {
		t.Errorf("after 1 ms in 1000 steps the budget holds %d bits, want 2000", bits)
	}

	// Ten idle seconds: the layer's measured bitrate is next to nothing, and the bucket is full again, no fuller.
	r.f.stats.bitrate.Store(50_000)
	r.now += 10 * oneSecond
	r.d.nack.mu.Lock()
	r.d.nack.fill(r.d.rtxRate(r.f), r.now)
	bits = r.d.nack.bits
	r.d.nack.mu.Unlock()
	if bits != depth*8 {
		t.Errorf("after 10 idle seconds the budget holds %d bits, want %d", bits, depth*8)
	}
	// The first change on the screen: one frame of 30 packets, of which the viewer lost every one.
	first := seq0 + 251
	var frame []uint16
	for i := range 30 {
		r.send(payload, i == 29)
		frame = append(frame, first+uint16(i))
	}
	r.now += int64(200 * time.Millisecond)
	if n := r.nack(frame...); n != 30 {
		t.Errorf("%d of the 30 packets of a large frame after an idle time were retransmitted, want all", n)
	}

	// The admin's cap on the layer lowers the budget with it, at once: 1 Mbit/s leaves 250 kbit/s, 31 250 bytes.
	// The viewer asks for the whole frame once more.
	r.s.SetLimits(Limits{MaxShareKbps: 1000})
	r.now += int64(nackMinGap)
	if n := r.nack(frame...); n != 26 {
		t.Errorf("%d retransmissions under a budget of 250 kbit/s, want 26", n)
	}
}

// TestRTXRate: the budget's rate (02 §9.5) is a quarter of the layer's maximum after the admin cap, or of what the
// layer carries when that is more, and never less than 200 kbit/s.
func TestRTXRate(t *testing.T) {
	r := newRTXRig(t)
	q, _ := testLayer(t, r.sh, SlotQ, 0xf01)
	a, _ := testLayer(t, r.sh, SlotAudio, 0xaa)
	for _, c := range []struct {
		name     string
		layer    *Layer
		capKbps  int
		measured int64
		want     int64
	}{
		{"the full layer", r.f, 0, 300_000, 2_000_000},
		{"the full layer under an admin cap", r.f, 2000, 0, 500_000},
		{"a cap that leaves less than the floor", r.f, 400, 0, 200_000},
		{"a publisher that sends more than its maximum", r.f, 0, 12_000_000, 3_000_000},
		{"the preview layer", q, 0, 250_000, 200_000},
		{"audio", a, 0, 128_000, 200_000},
		{"before anything was forwarded", nil, 0, 0, 200_000},
	} {
		r.s.SetLimits(Limits{MaxShareKbps: c.capKbps})
		if c.layer != nil {
			c.layer.stats.bitrate.Store(c.measured)
		}
		if got := r.d.rtxRate(c.layer); got != c.want {
			t.Errorf("%s: %d bit/s, want %d", c.name, got, c.want)
		}
	}
	// The maxima the rate follows are the share's, as SetLimits and UpdateShare leave them: the full layer's under
	// the cap, the preview layer's, and the Opus bitrate of the preset.
	r.s.SetLimits(Limits{MaxShareKbps: 2500})
	if !r.sh.setPreset(PresetMovie) {
		t.Fatal("the share took no preset")
	}
	for _, c := range []struct {
		slot Slot
		want int64
	}{{SlotF, 2_500_000}, {SlotH, 2_500_000}, {SlotQ, previewMaxBitrate}, {SlotAudio, audioBitrateMovie}} {
		if got := r.sh.maxBitrate(c.slot); got != c.want {
			t.Errorf("the most layer %s may carry: %d bit/s, want %d", c.slot, got, c.want)
		}
	}
}

// TestPubNACKFilter: the NACKs that Pion's generator sends a publisher go through the pub PC's NACK filter, which
// takes out what the cache of the track's Layer holds by then (02 §9.5, pubnack.go): the generator never learns of a
// packet that came back through RTX, and would ask for it nine more times. A NACK left with nothing is not sent.
// Other RTCP, NACKs about an SSRC the filter has no track of, and a track that feeds no Layer, pass unchanged.
func TestPubNACKFilter(t *testing.T) {
	r := newRTXRig(t)
	r.start(0)
	// Upstream 500 to 504 arrive, 505 to 507 are lost, 508 and 509 arrive.
	for range 4 {
		r.send(deltaPayload, false)
	}
	r.seq += 3
	r.send(deltaPayload, false)
	r.send(deltaPayload, false)
	track := &pubTrack{ssrc: r.f.ssrc, kind: webrtc.RTPCodecTypeVideo}
	track.layer.Store(r.f)
	f := &nackFilter{}
	f.add(track)

	var written [][]rtcp.Packet
	w := f.BindRTCPWriter(interceptor.RTCPWriterFunc(func(pkts []rtcp.Packet, _ interceptor.Attributes) (int, error) {
		written = append(written, pkts)
		return len(pkts), nil
	}))
	nackOf := func(ssrc uint32, seqs ...uint16) *rtcp.TransportLayerNack {
		return &rtcp.TransportLayerNack{SenderSSRC: 0x5e, MediaSSRC: ssrc, Nacks: rtcp.NackPairsFromSequenceNumbers(seqs)}
	}
	asked := func(p rtcp.Packet) (ssrc uint32, seqs []uint16) {
		nack, ok := p.(*rtcp.TransportLayerNack)
		if !ok {
			return 0, nil
		}
		for _, pair := range nack.Nacks {
			seqs = append(seqs, pair.PacketList()...)
		}
		return nack.MediaSSRC, seqs
	}
	write := func(pkts ...rtcp.Packet) []rtcp.Packet {
		t.Helper()
		before := len(written)
		if _, err := w.Write(pkts, nil); err != nil {
			t.Fatal(err)
		}
		if len(written) == before {
			return nil
		}
		return written[len(written)-1]
	}

	// What arrived is taken out; the sender SSRC stays.
	out := write(nackOf(r.f.ssrc, 503, 505, 506, 507, 509))
	if len(out) != 1 {
		t.Fatalf("%d packets written, want one NACK", len(out))
	}
	if ssrc, seqs := asked(out[0]); ssrc != r.f.ssrc || !slices.Equal(seqs, []uint16{505, 506, 507}) ||
		out[0].(*rtcp.TransportLayerNack).SenderSSRC != 0x5e {
		t.Errorf("the NACK asks for %v of %#x, want 505 to 507 of %#x", seqs, ssrc, r.f.ssrc)
	}
	// A NACK for nothing but what arrived is not sent at all.
	if out := write(nackOf(r.f.ssrc, 500, 504, 508)); out != nil {
		t.Errorf("a NACK for packets in the cache went out: %v", out)
	}
	// 506 comes back on the RTX stream, late: the requests for it stop, those for the others go on.
	r.f.handleRTP(rtpPkt(506, 90000+3000*6, false, deltaPayload), ProfileHigh, r.now)
	pli := &rtcp.PictureLossIndication{MediaSSRC: r.f.ssrc}
	rr := &rtcp.ReceiverReport{SSRC: 0x5e}
	out = write(rr, nackOf(r.f.ssrc, 505, 506, 507), nackOf(r.f.ssrc, 506), pli)
	if len(out) != 3 || out[0] != rr || out[2] != pli {
		t.Fatalf("written %v, want the receiver report, one NACK and the PLI, in that order", out)
	}
	if _, seqs := asked(out[1]); !slices.Equal(seqs, []uint16{505, 507}) {
		t.Errorf("after 506 came back the NACK asks for %v, want 505 and 507", seqs)
	}
	// Unchanged: a batch without NACKs, a NACK of an SSRC the PC has no track of, and one of a track that feeds no
	// Layer (its m-section carries no share).
	for _, batch := range [][]rtcp.Packet{
		{pli, rr},
		{nackOf(0x9999, 500, 501)},
	} {
		if out := write(batch...); !slices.Equal(out, batch) {
			t.Errorf("written %v, want %v as it was", out, batch)
		}
	}
	track.layer.Store(nil)
	if _, seqs := asked(write(nackOf(r.f.ssrc, 500, 505))[0]); !slices.Equal(seqs, []uint16{500, 505}) {
		t.Errorf("a track without a Layer: the NACK asks for %v, want 500 and 505", seqs)
	}
	track.layer.Store(r.f)

	// The table follows the PC's tracks: a track that ended leaves it, unless a newer one has its SSRC by then.
	newer := &pubTrack{ssrc: r.f.ssrc, kind: webrtc.RTPCodecTypeVideo}
	newer.layer.Store(r.f)
	f.add(newer)
	f.remove(track)
	if _, seqs := asked(write(nackOf(r.f.ssrc, 500, 505))[0]); !slices.Equal(seqs, []uint16{505}) {
		t.Errorf("after an older track of the SSRC ended the NACK asks for %v, want 505", seqs)
	}
	f.remove(newer)
	if _, seqs := asked(write(nackOf(r.f.ssrc, 500, 505))[0]); !slices.Equal(seqs, []uint16{500, 505}) {
		t.Errorf("after the track ended the NACK asks for %v, want 500 and 505", seqs)
	}
}

// TestNACKBatch: one compound packet from a viewer can ask for thousands of sequence numbers. The first 256 are
// looked at, which is as many as the RTX queue holds; the rest are skipped, and a viewer that still misses them asks
// again. NACKs about another SSRC are not this DownTrack's.
func TestNACKBatch(t *testing.T) {
	r := newRTXRig(t)
	seq0, _ := r.start(0)
	r.pace = int64(100 * time.Microsecond)
	var seqs []uint16
	for i := range 300 {
		r.send(deltaPayload, false)
		seqs = append(seqs, seq0+1+uint16(i))
	}
	pairs := rtcp.NackPairsFromSequenceNumbers(seqs)
	r.d.handleRTCP([]rtcp.Packet{
		&rtcp.TransportLayerNack{MediaSSRC: 0x9999, Nacks: pairs},
		&rtcp.TransportLayerNack{MediaSSRC: rtxTestSSRC, Nacks: pairs},
		&rtcp.TransportLayerNack{MediaSSRC: 0x7000, Nacks: pairs[:5]},
		&rtcp.ReceiverReport{},
		&rtcp.TransportLayerNack{MediaSSRC: 0x7000, Nacks: pairs[5:]},
	})
	if len(r.d.rtxQueue) != nackBatch {
		t.Fatalf("%d retransmissions queued for a NACK of 300, want %d", len(r.d.rtxQueue), nackBatch)
	}
	r.drainRTX()
	if got, want := r.counts(), (RepairState{NACKs: 300, Served: 256, Skipped: 44}); got != want {
		t.Errorf("counters = %+v, want %+v", got, want)
	}
	// The next packet starts a new batch.
	r.d.handleRTCP([]rtcp.Packet{&rtcp.TransportLayerNack{MediaSSRC: 0x7000, Nacks: rtcp.NackPairsFromSequenceNumbers(seqs[256:])}})
	if len(r.d.rtxQueue) != 44 {
		t.Errorf("%d retransmissions queued for the rest, want 44", len(r.d.rtxQueue))
	}
}

// TestRTXQueue: the RTX queue holds 256 retransmissions; more are dropped and counted, never waited for. One that
// was asked for through another binding than the DownTrack has when its turn comes (the sub PC was rebuilt), or that
// meets a closed gate or a viewer without a payload type for it, is not sent.
func TestRTXQueue(t *testing.T) {
	r := newRTXRig(t)
	seq0, _ := r.start(0)
	r.pace = int64(100 * time.Microsecond)
	var seqs []uint16
	for i := range rtxQueueLen + 10 {
		r.send(deltaPayload, false)
		seqs = append(seqs, seq0+1+uint16(i))
	}
	left := len(seqs)
	r.d.handleNACK(r.b, rtcp.NackPairsFromSequenceNumbers(seqs), &left, r.now)
	if got, want := r.counts(), (RepairState{NACKs: uint64(len(seqs)), Dropped: 10}); got != want || len(r.d.rtxQueue) != rtxQueueLen {
		t.Fatalf("counters = %+v with %d queued, want %+v and a full queue", got, len(r.d.rtxQueue), want)
	}
	before := r.w.written()
	// Three wait for the writer when the binding changes: none of them is for the new sender.
	for len(r.d.rtxQueue) > 3 {
		r.d.retransmit(&r.state, <-r.d.rtxQueue)
	}
	sent := r.w.written() - before
	rebound := *r.b
	r.d.binding.Store(&rebound)
	r.drainRTX()
	if got := r.w.written() - before; got != sent || r.counts().Skipped != 3 {
		t.Errorf("%d retransmissions written on another binding than they were asked on (skipped %d)", got-sent, r.counts().Skipped)
	}
	// Asked for on the new binding, with the gate closed when the writer gets to it; then without a payload type.
	r.now += oneSecond
	r.send(deltaPayload, false)
	before, sent = r.w.written(), 0
	last := seqs[len(seqs)-1] + 1
	rebound.pc.ready.Store(false)
	if n := r.nack(last); n != 0 {
		t.Errorf("%d retransmissions through a closed gate", n)
	}
	rebound.pc.ready.Store(true)
	rebound.ptFor = map[ProfileKey]uint8{ProfileConstrainedBaseline: 96}
	r.now += oneSecond
	if n := r.nack(last); n != 0 {
		t.Errorf("%d retransmissions in a profile the viewer has no payload type for", n)
	}
	if got := r.counts().Skipped; got != 5 {
		t.Errorf("%d skipped, want 5", got)
	}
	// Unbound: nothing to retransmit on, and no crash.
	r.d.binding.Store(nil)
	r.d.handleRTCP([]rtcp.Packet{&rtcp.TransportLayerNack{MediaSSRC: 0x7000, Nacks: []rtcp.NackPair{{PacketID: last}}}})
	r.d.retransmit(&r.state, rtxItem{p: r.f.cache.get(r.seq-1, r.now), b: &rebound, seq: last})
	if got := r.w.written() - before; got != sent {
		t.Errorf("%d retransmissions written without a binding", got-sent)
	}
}

// TestWriterSendsRTXFirst: the writer goroutine takes what the viewer waits for before new media (02 §9.3).
func TestWriterSendsRTXFirst(t *testing.T) {
	r := newRTXRig(t)
	seq0, _ := r.start(4)
	before := r.w.written()
	// Five new packets are queued, and then the viewer's NACK for two old ones arrives. The writer isn't running yet.
	for range 5 {
		r.now += tick
		r.f.handleRTP(rtpPkt(r.seq, r.ts, false, deltaPayload), ProfileHigh, r.now)
		r.seq++
		r.ts += 3000
	}
	left := nackBatch
	r.d.handleNACK(r.b, rtcp.NackPairsFromSequenceNumbers([]uint16{seq0 + 1, seq0 + 2}), &left, r.now)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.d.run(nil)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for r.w.written() < before+7 {
		if time.Now().After(deadline) {
			t.Fatalf("the writer wrote %d of 7 packets", r.w.written()-before)
		}
		time.Sleep(time.Millisecond)
	}
	r.d.stop()
	<-done
	var ssrcs []uint32
	for i := before; i < before+7; i++ {
		h, _ := r.w.packet(i)
		ssrcs = append(ssrcs, h.SSRC)
	}
	if !slices.Equal(ssrcs, []uint32{rtxTestSSRC, rtxTestSSRC, 0x7000, 0x7000, 0x7000, 0x7000, 0x7000}) {
		t.Errorf("the writer sent SSRCs %x, want the two retransmissions before the five new packets", ssrcs)
	}
	// The RTX stream of a writer goroutine starts at a random sequence number and counts up from there.
	h0, _ := r.w.packet(before)
	h1, _ := r.w.packet(before + 1)
	if h1.SequenceNumber != h0.SequenceNumber+1 {
		t.Errorf("RTX sequence numbers %d and %d, want consecutive", h0.SequenceNumber, h1.SequenceNumber)
	}
}

// srs returns the sender reports written so far.
func (r *rtcpRecorder) srs() []rtcp.SenderReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []rtcp.SenderReport
	for _, p := range r.pkts {
		if sr, ok := p.(*rtcp.SenderReport); ok {
			out = append(out, *sr)
		}
	}
	return out
}

// TestSenderReportForwarding: the sender reports of the layer a DownTrack forwards go on to the viewer as reports
// about the DownTrack's own stream (02 §9.6): the publisher's NTP time unchanged, its RTP time moved by the same
// offset as the layer's packets, the DownTrack's SSRC and its own counts. A report of a layer the viewer isn't
// getting, a stale one, and one for a DownTrack that can't send, go nowhere.
func TestSenderReportForwarding(t *testing.T) {
	r := newRTXRig(t)
	// Nothing is forwarded yet: the DownTrack wants the layer, gets the notice, and sends nothing.
	const ntp = uint64(3_900_000_000)<<32 | 0x8000_0000
	r.f.handleSR(&rtcp.SenderReport{SSRC: 0xf00, NTPTime: ntp, RTPTime: 80_000}, r.now)
	if len(r.d.queue) != 1 {
		t.Fatalf("%d items queued for a sender report of the wanted layer, want the notice", len(r.d.queue))
	}
	drain(r.d, &r.state)
	if n := len(r.rtcp.srs()); n != 0 {
		t.Fatalf("%d sender reports before the stream began", n)
	}

	// The stream begins: the first packet of the epoch is followed by the layer's latest report, at once.
	_, ts0 := r.start(0) // upstream ts 90000
	offset := ts0 - 90000
	srs := r.rtcp.srs()
	if len(srs) != 1 {
		t.Fatalf("%d sender reports after the first packet of the stream, want 1", len(srs))
	}
	if sr := srs[0]; sr.SSRC != 0x7000 || sr.NTPTime != ntp || sr.RTPTime != 80_000+offset || sr.PacketCount != 1 ||
		sr.OctetCount != uint32(len(keyPayload(t))) || len(sr.Reports) != 0 {
		t.Errorf("the first sender report = %+v, want SSRC 0x7000, the publisher's NTP time, RTP %d and the counts of one packet",
			sr, 80_000+offset)
	}

	// Every report that arrives while the layer is forwarded goes on, in its place among the packets.
	r.send(deltaPayload, false)
	r.send(deltaPayload, true)
	r.now += tick
	r.f.handleSR(&rtcp.SenderReport{SSRC: 0xf00, NTPTime: ntp + 1<<32, RTPTime: 170_000}, r.now)
	r.f.handleSR(&rtcp.SenderReport{SSRC: 0xbad, NTPTime: 1, RTPTime: 2}, r.now) // not this layer's stream
	drain(r.d, &r.state)
	srs = r.rtcp.srs()
	if len(srs) != 2 {
		t.Fatalf("%d sender reports, want 2", len(srs))
	}
	if sr := srs[1]; sr.SSRC != 0x7000 || sr.NTPTime != ntp+1<<32 || sr.RTPTime != 170_000+offset || sr.PacketCount != 3 ||
		sr.OctetCount != uint32(len(keyPayload(t))+2*len(deltaPayload)) {
		t.Errorf("the second sender report = %+v, want RTP %d and the counts of three packets", sr, 170_000+offset)
	}
	if got := r.d.stats.srs.Load(); got != 2 {
		t.Errorf("%d sender reports counted, want 2", got)
	}

	// A report of the layer the viewer is switching to isn't on its timeline yet: nothing goes out for it, until the
	// switch is done. Then the new layer's report follows the first packet, with the new epoch's offset.
	q, _ := testLayer(t, r.sh, SlotQ, 0xf01)
	r.d.setWant(QualityLow)
	q.handleSR(&rtcp.SenderReport{SSRC: 0xf01, NTPTime: ntp + 2<<32, RTPTime: 5_000_000}, r.now)
	drain(r.d, &r.state)
	if n := len(r.rtcp.srs()); n != 2 {
		t.Fatalf("%d sender reports while the viewer waits for the new layer's keyframe, want still 2", n)
	}
	r.now += tick
	q.handleRTP(rtpPkt(9000, 5_090_000, false, keyPayload(t)), ProfileHigh, r.now)
	drain(r.d, &r.state)
	hq, _ := r.w.packet(r.w.written() - 1)
	srs = r.rtcp.srs()
	if len(srs) != 3 || srs[2].NTPTime != ntp+2<<32 || srs[2].RTPTime != 5_000_000+(hq.Timestamp-5_090_000) || srs[2].PacketCount != 4 {
		t.Fatalf("after the switch: sender reports %+v, want a third with the new layer's NTP time and RTP %d", srs,
			5_000_000+(hq.Timestamp-5_090_000))
	}
	// The old layer's reports are no longer the viewer's.
	r.f.handleSR(&rtcp.SenderReport{SSRC: 0xf00, NTPTime: ntp + 3<<32, RTPTime: 350_000}, r.now)
	drain(r.d, &r.state)
	if n := len(r.rtcp.srs()); n != 3 {
		t.Errorf("%d sender reports after one of the layer the viewer left, want still 3", n)
	}

	// A stale report (10 s) isn't sent after a new epoch: the viewer waits for the next one.
	r.d.setWant(QualityOff)
	r.d.setWant(QualityLow)
	stale := *q.lastSR.Load()
	stale.arrival = monoNow() - int64(srMaxAge) - int64(time.Second)
	q.lastSR.Store(&stale)
	r.now += tick
	q.handleRTP(rtpPkt(9001, 5_093_000, false, keyPayload(t)), ProfileHigh, r.now)
	drain(r.d, &r.state)
	if n := len(r.rtcp.srs()); n != 3 || r.w.written() != 5 {
		t.Errorf("%d sender reports and %d packets after a resume with a stale report, want 3 and 5", n, r.w.written())
	}

	// A paused DownTrack isn't told of reports at all, and a full queue drops the notice without counting a packet.
	r.d.setWant(QualityOff)
	q.handleSR(&rtcp.SenderReport{SSRC: 0xf01, NTPTime: ntp + 4<<32, RTPTime: 5_500_000}, r.now)
	if len(r.d.queue) != 0 {
		t.Errorf("%d items queued for a paused DownTrack", len(r.d.queue))
	}
	r.d.setWant(QualityLow)
	for len(r.d.queue) < cap(r.d.queue) {
		r.d.queue <- q.srNotice
	}
	q.handleSR(&rtcp.SenderReport{SSRC: 0xf01, NTPTime: ntp + 5<<32, RTPTime: 5_600_000}, r.now)
	if got := r.d.stats.drops.Load(); got != 0 {
		t.Errorf("%d packets counted as dropped for a sender report notice", got)
	}
	// A binding without a PeerConnection to send on (none in these tests' other DownTracks): nothing, and no crash.
	r.b.rtcp = nil
	drain(r.d, &r.state)
	if n := len(r.rtcp.srs()); n != 3 {
		t.Errorf("%d sender reports, want 3", n)
	}
}
