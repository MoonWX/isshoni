package sfu

import (
	"encoding/binary"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// This file is the repair path of 02 §9.5. A viewer that misses a packet sends a NACK for the sequence number it
// expected: one of the DownTrack's own. The munger's epochs say which packet of which Layer that was, the Layer's
// packet cache still has it for 1.5 s, and the DownTrack's writer sends it again before new media: wrapped as RTX
// (RFC 4588) when the viewer negotiated RTX for the payload type, else as it was. Nothing is kept per viewer but the
// bookkeeping of what was retransmitted when; the payloads are the cache's.
//
// The publish side needs nothing of this: Pion's NACK generator asks the publisher, and what the publisher sends
// again reaches the cache, and every DownTrack that forwards the layer, as a late packet (02 §9.1).

// rtxItem is one retransmission on its way from the DownTrack's RTCP reader, which decided on it, to the DownTrack's
// writer.
type rtxItem struct {
	p   *packet  // the cached packet; its payload is the cache's, shared and never changed
	b   *binding // the binding the NACK came through: what one sender was asked for, no other sender answers
	seq uint16   // the own sequence number the viewer asked for
	ts  uint32   // the own timestamp the packet went out with
}

// nackState is what a DownTrack remembers of its viewer's NACKs: what it retransmitted when, and the budget. Its
// lock is a leaf: nothing is called while it is held.
type nackState struct {
	mu sync.Mutex
	// records is the ring of retransmissions by own sequence number. It is made with the first one: most DownTracks
	// never see a NACK.
	records *[nackRecords]nackRecord
	// The budget is a token bucket in bits: it fills at the budget's rate, up to rtxBurst of it, and was last filled
	// at filledAt. It starts full, with the first NACK.
	bits     int64
	filledAt int64
	filled   bool
}

// nackRecord says how often one own sequence number was retransmitted, and when last.
type nackRecord struct {
	at  int64
	seq uint16
	n   uint8 // 0: the slot is empty
}

// nackHit is one sequence number of a NACK on its way through serveNACKs.
type nackHit struct {
	seq    uint16
	layer  *Layer // of the epoch that sent seq; nil when the munger knows none
	seqOff uint16 // that epoch's offsets
	tsOff  uint32
	p      *packet // from the layer's cache; nil when it is no longer there
	send   bool
}

// nackChunk is how many sequence numbers serveNACKs handles in one go: its scratch space is on the stack.
const nackChunk = 64

// rtxPool holds the buffers that RTX payloads are built in: the original sequence number followed by the cached
// payload. A buffer goes back as soon as Pion has taken the packet, which it encrypts into a buffer of its own.
var rtxPool = sync.Pool{New: func() any { return new([rtpReadBuffer]byte) }}

// handleNACK answers the NACK items of one RTCP packet from the viewer (02 §9.5). Each item names up to 17 own
// sequence numbers. left is how many more of them this compound packet may have looked at (nackBatch): the rest are
// counted as skipped, and the viewer asks again for what it still misses. now is monoNow. The DownTrack's RTCP reader
// calls it.
func (d *DownTrack) handleNACK(b *binding, pairs []rtcp.NackPair, left *int, now int64) {
	var hits [nackChunk]nackHit
	n := 0
	for _, pair := range pairs {
		for bit := -1; bit < 16; bit++ {
			if bit >= 0 && uint16(pair.LostPackets)&(1<<bit) == 0 {
				continue
			}
			d.stats.nackReceived.Add(1)
			if *left <= 0 {
				d.stats.rtxSkipped.Add(1)
				continue
			}
			*left--
			hits[n] = nackHit{seq: pair.PacketID + uint16(bit+1)}
			if n++; n == len(hits) {
				d.serveNACKs(b, hits[:n], now)
				n = 0
			}
		}
	}
	if n > 0 {
		d.serveNACKs(b, hits[:n], now)
	}
}

// serveNACKs decides for each sequence number the viewer asked for whether it is sent again, and queues those that
// are for the writer. The steps of 02 §9.5:
//
//  1. The munger's epochs give the Layer and the upstream sequence number that own sequence number stood for, and
//     the Layer's cache the packet, if it arrived less than 1.5 s ago. Without either it is a miss: the viewer asks
//     for a keyframe when it needs one.
//  2. A sequence number that was retransmitted within the last max(20 ms, RTT/2) is skipped: that NACK was sent
//     before the answer to the last one could arrive. So is one that was retransmitted three times: whatever keeps
//     it from arriving, a fourth copy won't help. (This check comes first: a viewer that goes on asking for what it
//     already got, as Pion's own NACK generator does for packets that arrive as RTX, is skipped, not counted as a
//     miss once the cache has let go of the packet.)
//  3. The budget: retransmissions may use a quarter of what the layer carries or may carry, at least 200 kbit/s,
//     with a second of that in reserve. What goes beyond is skipped: a viewer on a link that loses that much needs
//     a lower layer, not more load (the downlink allocator, README S84).
//
// The locks are taken one after the other, never inside each other: the DownTrack's for the epochs, each cache's own
// for the packet, and the NACK state's for the records and the budget.
func (d *DownTrack) serveNACKs(b *binding, hits []nackHit, now int64) {
	var newest *Layer
	d.mu.Lock()
	if d.m.n > 0 {
		newest = d.m.newest().layer
	}
	for i := range hits {
		if e, ok := d.m.lookup(hits[i].seq, now); ok {
			hits[i].layer, hits[i].seqOff, hits[i].tsOff = e.layer, e.seqOff, e.tsOff
		}
	}
	d.mu.Unlock()
	for i := range hits {
		if h := &hits[i]; h.layer != nil {
			h.p = h.layer.cache.get(h.seq-h.seqOff, now)
		}
	}

	rate := d.rtxRate(newest)
	gap := int64(nackMinGap)
	if b.pc != nil {
		gap = max(gap, b.pc.rtt.Load()/2)
	}
	var missed, skipped uint64
	ns := &d.nack
	ns.mu.Lock()
	ns.fill(rate, now)
	for i := range hits {
		h := &hits[i]
		switch rec := ns.record(h.seq); {
		case rec != nil && (now-rec.at < gap || rec.n >= nackMaxSends):
			skipped++
		case h.p == nil:
			missed++
		case ns.bits < rtxCost(h.p):
			skipped++
		default:
			ns.bits -= rtxCost(h.p)
			ns.sent(h.seq, now)
			h.send = true
		}
	}
	ns.mu.Unlock()

	for i := range hits {
		h := &hits[i]
		if !h.send {
			continue
		}
		select {
		case d.rtxQueue <- rtxItem{p: h.p, b: b, seq: h.seq, ts: h.p.ts + h.tsOff}:
		default:
			d.stats.rtxDrops.Add(1)
		}
	}
	d.stats.nackMissed.Add(missed)
	d.stats.rtxSkipped.Add(skipped)
}

// rtxCost is what one retransmission takes from the budget, in bits: the payload.
func rtxCost(p *packet) int64 { return int64(len(p.payload)) * 8 }

// rtxRate returns the rate of the DownTrack's retransmission budget in bit/s (02 §9.5): a quarter of what the layer
// it forwards carries or is meant to carry at most, whichever is more, and at least 200 kbit/s. l is the layer of
// the DownTrack's newest epoch, nil before the first. The layer's own measured bitrate stands for "the DownTrack's
// forwarded bitrate": a DownTrack forwards all of a layer or nothing. The maximum counts because screen video idles
// far below it and then starts with a frame of hundreds of packets, when the measured rate is still the idle one.
func (d *DownTrack) rtxRate(l *Layer) int64 {
	var bps int64
	if l != nil {
		bps = max(l.stats.bitrate.Load(), int64(d.share.maxBitrate(l.slot)))
	}
	return max(bps/rtxShare, rtxMinRate)
}

// fill brings the budget up to now at rate bit/s. A lower rate than last time (the DownTrack went to a smaller
// layer) also lowers what the bucket can hold. n.mu is held.
func (n *nackState) fill(rate, now int64) {
	depth := rate * int64(rtxBurst) / int64(time.Second)
	if !n.filled {
		n.filled, n.bits, n.filledAt = true, depth, now
	} else if add := min(now-n.filledAt, int64(rtxBurst)) * rate / int64(time.Second); add > 0 {
		// Only when something was added: a NACK every few microseconds must not round the budget's income away.
		n.bits, n.filledAt = n.bits+add, now
	}
	n.bits = min(n.bits, depth)
}

// record returns what is known of earlier retransmissions of own sequence number seq, or nil when there were none
// (or the record has made way for another sequence number's). n.mu is held.
func (n *nackState) record(seq uint16) *nackRecord {
	if n.records == nil {
		return nil
	}
	if rec := &n.records[seq%nackRecords]; rec.n > 0 && rec.seq == seq {
		return rec
	}
	return nil
}

// sent notes one more retransmission of own sequence number seq. n.mu is held.
func (n *nackState) sent(seq uint16, now int64) {
	if n.records == nil {
		n.records = new([nackRecords]nackRecord)
	}
	rec := &n.records[seq%nackRecords]
	if rec.n == 0 || rec.seq != seq {
		*rec = nackRecord{seq: seq}
	}
	rec.n++
	rec.at = now
}

// retransmit sends one queued retransmission (02 §9.5, steps 4 and 5): the cached payload under the own timestamp
// and marker bit it first went out with. When the viewer negotiated RTX for the packet's payload type, it goes out
// on the RTX stream (RFC 4588): the RTX SSRC and payload type, the next number of the DownTrack's RTX sequence, and
// the original sequence number in front of the payload. Otherwise, which is always so for audio, it is the original
// packet again: the same SSRC, payload type and sequence number. Abs-send-time is the time of this sending.
//
// A retransmission that was asked for on another binding than the DownTrack has now (the sub PC was rebuilt
// meanwhile), or that the viewer can no longer get or decode (the gate closed, the payload type is gone), is
// dropped and counted as skipped. The writer calls it.
func (d *DownTrack) retransmit(w *rtpWriteState, it rtxItem) {
	b := d.binding.Load()
	w.bind(b)
	pt, ok := b.payloadType(it.p)
	if b != it.b || !ok || !b.sendable() {
		d.stats.rtxSkipped.Add(1)
		return
	}
	w.hdr = rtp.Header{
		Version: 2, Marker: it.p.marker, PayloadType: pt, SequenceNumber: it.seq, Timestamp: it.ts, SSRC: b.ssrc,
		Extensions: w.hdr.Extensions[:0],
	}
	payload := it.p.payload
	var buf *[rtpReadBuffer]byte
	if rtxPT, ok := b.rtxPTFor[pt]; ok && b.rtxSSRC != 0 && len(payload)+2 <= rtpReadBuffer {
		buf, _ = rtxPool.Get().(*[rtpReadBuffer]byte)
		binary.BigEndian.PutUint16(buf[:2], it.seq)
		payload = buf[:2+copy(buf[2:], payload)]
		w.hdr.PayloadType, w.hdr.SSRC, w.hdr.SequenceNumber = rtxPT, b.rtxSSRC, w.rtxSeq
		w.rtxSeq++
	}
	if b.absSendTimeID != 0 {
		putAbsSendTime(&w.absSendTime, time.Now())
		_ = w.hdr.SetExtension(b.absSendTimeID, w.absSendTime[:]) // 3 bytes always fit the one-byte form
	}
	n, err := b.writer.WriteRTP(&w.hdr, payload)
	if buf != nil {
		rtxPool.Put(buf)
	}
	switch {
	case err != nil:
		d.stats.writeErrors.Add(1)
	case n == 0:
		d.stats.unsent.Add(1) // lost like any packet the transport can't take right now: the viewer asks again
	default:
		d.stats.nackServed.Add(1)
		d.stats.rtxPackets.Add(1)
		d.stats.rtxBytes.Add(uint64(len(payload)))
		if buf == nil {
			w.count(len(payload)) // on the media SSRC: the next sender report counts it
		}
	}
}

// payloadType returns the viewer's payload type for a cached packet: the one of its profile, or of Opus.
func (b *binding) payloadType(p *packet) (uint8, bool) {
	if b == nil {
		return 0, false
	}
	pt, ok := b.ptFor[p.profile]
	return pt, ok
}

// sampleRTT reads the round-trip time of the sub PC's selected candidate pair, at most every rttEvery (02 §9.3):
// ICE measures it with its connectivity checks and keepalives. The RTCP readers of the PC's DownTracks call it with
// their sender when the viewer's RTCP arrives, so a PC that is heard from is measured, and whichever reader comes
// first after the interval does it. It asks Pion's ICE agent and may wait for it a moment, which a reader may and a
// writer may not. A PC that isn't connected has no pair to ask about.
func (s *subPC) sampleRTT(sender *webrtc.RTPSender, now int64) {
	last := s.rttAt.Load()
	if now-last < int64(rttEvery) || !s.ready.Load() || !s.rttAt.CompareAndSwap(last, now) {
		return
	}
	// Only while DTLS is up: a PC that the viewer has just closed still counts as ready until the actor has seen it,
	// and Pion logs an error when a closed ICE agent is asked for its statistics.
	dtls := sender.Transport()
	if dtls == nil || dtls.State() != webrtc.DTLSTransportStateConnected {
		return
	}
	ice := dtls.ICETransport()
	if ice == nil {
		return
	}
	if pair, ok := ice.GetSelectedCandidatePairStats(); ok && pair.CurrentRoundTripTime > 0 {
		s.rtt.Store(int64(pair.CurrentRoundTripTime * float64(time.Second)))
	}
}
