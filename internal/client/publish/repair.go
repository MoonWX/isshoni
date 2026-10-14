package publish

import (
	"encoding/binary"
	"strconv"
	"strings"
	"sync"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// This file is Options.RedundantRTX: the track repairs its stream itself, the way a browser does, in place of
// Pion's NACK responder. It keeps the packets it sent, answers NACKs from them, and sends second copies of packets
// that were delivered already. A browser does both on one RTX stream (RFC 4588), so both take their sequence
// numbers from one counter here.

// The test options' numbers.
const (
	// repairRing is how many sent packets a track keeps for repairs.
	repairRing = 1024
	// redundantAfter is the number of frames a video track sends before its first second copy. A receiver can place
	// an RTX stream only once it knows the stream it repairs: Pion maps the RTX stream of a simulcast layer to the
	// layer's track when its first packet arrives, and gives the RTX stream up for good when it has no such track yet.
	redundantAfter = 15
	// A padding burst (Options.Padding): padPackets packets with padSize bytes of padding each.
	padPackets = 2
	padSize    = 200
)

// repair is what a track keeps to repair its stream.
type repair struct {
	// mu guards the fields below. It is held across a write on the RTX stream, so that the stream's packets leave in
	// the order of their sequence numbers, whichever goroutine sends them: the send loop (second copies) or the
	// track's RTCP reader (NACKs). Writes never block.
	mu   sync.Mutex
	seq  uint16 // the RTX stream's next sequence number
	ring [repairRing]sentPacket
}

// sentPacket is a packet of the media stream as the ring keeps it.
type sentPacket struct {
	seq     uint16
	ts      uint32
	marker  bool
	sent    bool   // false: the slot is empty
	payload []byte // the Source's or the payloader's: never changed after it was sent
}

func newRepair() *repair {
	seq, _ := randomBase()
	return &repair{seq: seq}
}

// keep remembers a packet of the media stream.
func (r *repair) keep(seq uint16, ts uint32, marker bool, payload []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ring[seq%repairRing] = sentPacket{seq: seq, ts: ts, marker: marker, sent: true, payload: payload}
}

// answerNACK sends again what a NACK about the track's stream asks for, as far as the track still has it. Without
// Options.RedundantRTX it does nothing: Pion's NACK responder answers. The track's RTCP reader calls it.
func (t *track) answerNACK(nack *rtcp.TransportLayerNack) {
	if t.repair == nil {
		return
	}
	t.mu.Lock()
	b := t.b
	t.mu.Unlock()
	if b == nil || nack.MediaSSRC != b.ssrc {
		return
	}
	var sent uint64
	for _, pair := range nack.Nacks {
		for _, seq := range pair.PacketList() {
			if t.resend(b, seq) {
				sent++
			}
		}
	}
	t.mu.Lock()
	t.stats.Retransmitted += sent
	t.mu.Unlock()
}

// resend sends the packet of the media stream with sequence number seq again, if the track still has it, and reports
// whether it did. With RTX negotiated for the track's payload type it goes out on the RTX stream: the RTX SSRC and
// payload type, the stream's next sequence number, the original's timestamp and marker bit, and the original sequence
// number in front of the payload; a simulcast layer's RTX packets name the layer with the repaired-rtp-stream-id
// extension, as its media packets do with rtp-stream-id. Without RTX (audio) it is the original packet again.
func (t *track) resend(b *binding, seq uint16) bool {
	r := t.repair
	r.mu.Lock()
	defer r.mu.Unlock()
	sp := r.ring[seq%repairRing]
	if !sp.sent || sp.seq != seq {
		return false
	}
	hdr := rtp.Header{
		Version: 2, Marker: sp.marker, PayloadType: b.pt, SequenceNumber: sp.seq, Timestamp: sp.ts, SSRC: b.ssrc,
	}
	payload, ridID := sp.payload, b.ridID
	if b.rtxSSRC != 0 {
		hdr.PayloadType, hdr.SequenceNumber, hdr.SSRC = b.rtxPT, r.seq, b.rtxSSRC
		r.seq++
		payload = make([]byte, 2+len(sp.payload))
		binary.BigEndian.PutUint16(payload, sp.seq)
		copy(payload[2:], sp.payload)
		ridID = b.rridID
	}
	// The mid is read here, not taken from the send loop's copy: the RTCP reader calls this too.
	if b.midID != 0 && t.tr != nil {
		if mid := t.tr.Mid(); mid != "" {
			_ = hdr.SetExtension(b.midID, []byte(mid))
		}
	}
	if ridID != 0 && t.rid != "" {
		_ = hdr.SetExtension(ridID, []byte(t.rid))
	}
	_, err := b.w.WriteRTP(&hdr, payload)
	return err == nil
}

// rtxFor returns the RTX payload type that the negotiated codecs give payload type pt (the RTX codec whose apt
// parameter names it).
func rtxFor(pt webrtc.PayloadType, negotiated []webrtc.RTPCodecParameters) (uint8, bool) {
	for _, c := range negotiated {
		if !strings.EqualFold(c.MimeType, webrtc.MimeTypeRTX) {
			continue
		}
		for param := range strings.SplitSeq(c.SDPFmtpLine, ";") {
			name, value, _ := strings.Cut(strings.TrimSpace(param), "=")
			if apt, err := strconv.ParseUint(value, 10, 8); strings.EqualFold(name, "apt") && err == nil &&
				webrtc.PayloadType(apt) == pt {
				return uint8(c.PayloadType), true
			}
		}
	}
	return 0, false
}
