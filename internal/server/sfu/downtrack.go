package sfu

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

// DownTrack is one subscriber's video or audio of one share: the webrtc.TrackLocal behind a sendonly transceiver of
// the subscriber's sub PC (02 §9.3). On the wire its msid stream id is the share id and its track id "v-" or "a-"
// plus the share id; viewers map it by the sub offer's tracks binding, the msid is a debugging aid (02 §5.2).
//
// Media reaches it from the share's Layers: a Layer's read loop puts every packet of a slot the DownTrack is
// interested in on its queue (enqueue, never blocking), and the DownTrack's writer goroutine takes them off, gives
// them their place in the viewer's stream (the munger) and writes them to the sub PC. A second goroutine reads the
// viewer's RTCP for the track. Which slot the DownTrack wants is its subscription's (setTarget).
//
// Later slices add the rest of 02 §9.3: the RTX queue, NACKs and forwarded sender reports (README S63), the pacer,
// REMB and receiver reports (S84), and the caps of 02 §10.1 with their reasons (S52, S69, S84).
type DownTrack struct {
	share *Share
	sub   *Subscription
	kind  webrtc.RTPCodecType

	// sender and transceiver are the actor's: set when the DownTrack joins a sub PC, replaced on a rebuild.
	sender      *webrtc.RTPSender
	transceiver *webrtc.RTPTransceiver
	// pc is the sub PC the sender belongs to (its DTLS-ready gate). The actor sets it before Pion can call Bind.
	pc atomic.Pointer[subPC]
	// binding is what the negotiation gave this track: nil until Bind and after Unbind.
	binding atomic.Pointer[binding]

	// interest holds the bits (1<<Slot) of the layers whose packets the writer wants: the target and, until a switch
	// to it is done, the layer forwarded now. It is 0 while the DownTrack is paused. Layer read loops test it per
	// packet, without a lock.
	interest atomic.Uint32
	queue    chan *packet  // video 1024, audio 256; a full queue drops (counted)
	done     chan struct{} // closed by stop: the writer ends
	stopOnce sync.Once

	mu sync.Mutex // guards m; the last lock in doc.go's order, never held across a call that takes another
	m  *munger

	// forwarding: the writer forwards a layer to the viewer now (ShareInfo.Viewers).
	forwarding atomic.Bool
	stats      dtStats
}

// dtStats are a DownTrack's counters. The writer counts; README S79 reads them.
type dtStats struct {
	packets     atomic.Uint64 // RTP packets written
	bytes       atomic.Uint64 // their payload bytes
	drops       atomic.Uint64 // packets a full queue dropped
	noCodec     atomic.Uint64 // packets in a profile the viewer negotiated no payload type for
	unsent      atomic.Uint64 // packets Pion took before it could send: the stream restarts on a keyframe
	writeErrors atomic.Uint64
	switches    atomic.Uint64 // epochs started: the first keyframe, layer switches, resumes
	keyRequests atomic.Uint64 // PLIs and FIRs from the viewer
}

// binding is one negotiated attachment of a DownTrack to a sub PC's sender (02 §9.3).
type binding struct {
	pc            *subPC // the generation: its ready flag is the DTLS gate
	writer        webrtc.TrackLocalWriter
	ssrc, rtxSSRC uint32
	// ptFor maps a stream's H.264 profile to the viewer's payload type for it (bestPT); a profile the viewer can't
	// decode has no entry. Audio has one entry, under "", for Opus.
	ptFor map[ProfileKey]uint8
	// rtxPTFor maps a viewer PT to its RTX PT, when RTX is negotiated.
	rtxPTFor      map[uint8]uint8
	absSendTimeID uint8 // 0 when not negotiated
	// unsupported: no codec of ptFor is negotiated (a viewer without H.264: 02 §8.5). The DownTrack then forwards
	// nothing.
	unsupported bool
}

// errNoCodec is Bind's error when the sub PC negotiated no codec of the track's kind at all.
var errNoCodec = errors.New("sfu: no codec negotiated for the track's kind")

// RTP clock rates of the two codecs the SFU forwards.
const (
	videoClockRate = 90000
	audioClockRate = 48000
)

// newDownTrack returns a paused DownTrack. It starts no goroutine: the Conn starts the writer (run) when the
// subscription is made, and the sub PC the RTCP reader when the DownTrack gets its sender.
func newDownTrack(sub *Subscription, kind webrtc.RTPCodecType) *DownTrack {
	d := &DownTrack{share: sub.share, sub: sub, kind: kind, done: make(chan struct{})}
	if kind == webrtc.RTPCodecTypeAudio {
		d.queue, d.m = make(chan *packet, audioQueueLen), newMunger(audioClockRate)
	} else {
		d.queue, d.m = make(chan *packet, videoQueueLen), newMunger(videoClockRate)
	}
	return d
}

// ID implements webrtc.TrackLocal: "v-" or "a-" plus the share id.
func (d *DownTrack) ID() string {
	if d.kind == webrtc.RTPCodecTypeAudio {
		return "a-" + string(d.share.id)
	}
	return "v-" + string(d.share.id)
}

// StreamID implements webrtc.TrackLocal: the share id.
func (d *DownTrack) StreamID() string { return string(d.share.id) }

// RID implements webrtc.TrackLocal: a DownTrack is never simulcast.
func (d *DownTrack) RID() string { return "" }

// Kind implements webrtc.TrackLocal.
func (d *DownTrack) Kind() webrtc.RTPCodecType { return d.kind }

// Bind implements webrtc.TrackLocal. Pion calls it from its own goroutine once the sender's m-section is negotiated;
// it only reads the negotiation and stores the result, so it never blocks. It maps each H.264 profile a publisher
// may send to the viewer's payload type for it (02 §8.2), finds the RTX payload types and the abs-send-time
// extension, and returns the codec the sender starts with. It never fails while a codec of its kind is negotiated:
// with video but no usable H.264 it returns the first video codec and marks the binding unsupported, so one viewer
// without a decoder doesn't fail the whole negotiation (02 §8.5). On a sub PC that is already connected it asks the
// publisher for a keyframe: the viewer can't decode anything before one (02 §9.7).
func (d *DownTrack) Bind(ctx webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	b := &binding{
		pc:       d.pc.Load(),
		writer:   ctx.WriteStream(),
		ssrc:     uint32(ctx.SSRC()),
		rtxSSRC:  uint32(ctx.SSRCRetransmission()),
		ptFor:    map[ProfileKey]uint8{},
		rtxPTFor: map[uint8]uint8{},
	}
	for _, ext := range ctx.HeaderExtensions() {
		if ext.URI == sdp.ABSSendTimeURI {
			b.absSendTimeID = uint8(ext.ID)
		}
	}
	codecs := ctx.CodecParameters()
	for media, rtx := range rtxPayloadTypes(codecs) {
		b.rtxPTFor[uint8(media)] = uint8(rtx)
	}

	var chosen *webrtc.RTPCodecParameters
	if d.kind == webrtc.RTPCodecTypeAudio {
		for i, c := range codecs {
			if strings.EqualFold(c.MimeType, webrtc.MimeTypeOpus) {
				b.ptFor[""] = uint8(c.PayloadType)
				chosen = &codecs[i]
				break
			}
		}
	} else {
		for _, profile := range profileRank {
			if pt, ok := bestPT(profile, codecs); ok {
				b.ptFor[profile] = uint8(pt)
			}
		}
		// The sender starts with the codec for the share's current profile, or else with the best one the viewer
		// takes; the writer picks the PT of each packet from ptFor.
		start := append([]ProfileKey{d.share.currentProfile()}, profileRank[:]...)
		for _, profile := range start {
			pt, ok := b.ptFor[profile]
			if !ok {
				continue
			}
			for i, c := range codecs {
				if uint8(c.PayloadType) == pt {
					chosen = &codecs[i]
					break
				}
			}
			break
		}
	}
	if chosen == nil {
		b.unsupported = true
		for i, c := range codecs {
			if !strings.EqualFold(c.MimeType, webrtc.MimeTypeRTX) {
				chosen = &codecs[i]
				break
			}
		}
	}
	if chosen == nil {
		return webrtc.RTPCodecParameters{}, errNoCodec
	}
	d.binding.Store(b)
	if b.sendable() {
		d.requestKeyframe()
	}
	return *chosen, nil
}

// Unbind implements webrtc.TrackLocal: the sender stopped (the subscription went away, or the PC closed). The writer
// drops packets until the next Bind.
func (d *DownTrack) Unbind(webrtc.TrackLocalContext) error {
	d.binding.Store(nil)
	d.setForwarding(false)
	return nil
}

// sendable reports whether a packet written through the binding can reach the viewer: the viewer negotiated a codec
// the SFU forwards, and the DTLS-ready gate of the binding's sub PC is open (02 §9.3, S4 finding 1: Pion drops RTP
// written before DTLS is up, without an error).
func (b *binding) sendable() bool {
	return b != nil && !b.unsupported && b.pc != nil && b.pc.ready.Load()
}

// ---- what the DownTrack forwards ----

// setTarget sets the slot the viewer should get and whether anything is forwarded at all; the Conn's actor calls it
// when the subscription changes. A pause is immediate. A new target keeps the layer forwarded now flowing until the
// new one's keyframe arrives, so the interest mask holds both until then, and the publisher is asked for that
// keyframe (02 §10.1) unless the viewer can't get it yet: then the sub PC's connected state, or Bind, asks.
func (d *DownTrack) setTarget(slot Slot, active bool) {
	d.mu.Lock()
	changed := d.m.setTarget(slot, active)
	waiting := d.m.waitingForKeyframe()
	d.interest.Store(d.interestLocked())
	_, forwarding := d.m.current()
	d.mu.Unlock()
	if !forwarding {
		d.setForwarding(false)
	}
	if changed && waiting && d.kind == webrtc.RTPCodecTypeVideo && d.binding.Load().sendable() {
		d.share.requestKeyframe(slot)
	}
}

// interestLocked returns the interest mask for the munger's state: the target's bit and the bit of the layer
// forwarded now; nothing while paused. d.mu is held.
func (d *DownTrack) interestLocked() uint32 {
	if !d.m.active {
		return 0
	}
	bits := uint32(1) << d.m.target
	if slot, ok := d.m.current(); ok {
		bits |= 1 << slot
	}
	return bits
}

// requestKeyframe asks the publisher for a keyframe of the layer this video DownTrack wants, unless it is paused
// (02 §9.7). Its callers are a Bind on a connected sub PC and the sub PC reaching connected; the request is
// throttled per layer, so many viewers connecting at once cost the publisher one keyframe.
func (d *DownTrack) requestKeyframe() {
	if d.kind != webrtc.RTPCodecTypeVideo {
		return
	}
	d.mu.Lock()
	slot, active := d.m.target, d.m.active
	d.mu.Unlock()
	if active {
		d.share.requestKeyframe(slot)
	}
}

// setForwarding records whether the DownTrack forwards now. It writes only on a change: the writer calls it per packet.
func (d *DownTrack) setForwarding(on bool) {
	if d.forwarding.Load() != on {
		d.forwarding.Store(on)
	}
}

// ---- the writer (02 §9.3) ----

// enqueue hands a packet (or a padding marker) of an interesting layer to the writer. It never blocks: a full queue
// drops the packet and counts it, so a writer that falls behind costs only its own viewer. Layer read loops call it.
func (d *DownTrack) enqueue(p *packet) {
	select {
	case d.queue <- p:
	default:
		d.stats.drops.Add(1)
	}
}

// run is the writer goroutine: it forwards what the Layers queue until stop, or until the Conn that owns the
// subscription closes (connClosed), whichever comes first. That Conn starts it and waits for it (Conn.writers).
func (d *DownTrack) run(connClosed <-chan struct{}) {
	var w rtpWriteState
	for {
		select {
		case <-d.done:
			return
		case <-connClosed:
			return
		case p := <-d.queue:
			d.write(&w, p)
		}
	}
}

// stop ends the writer. It is idempotent, and safe for a DownTrack whose writer was never started.
func (d *DownTrack) stop() {
	d.stopOnce.Do(func() { close(d.done) })
}

// rtpWriteState is what the writer reuses from packet to packet, so that forwarding allocates nothing: the header,
// whose extension list keeps its capacity, and the bytes of the abs-send-time extension.
type rtpWriteState struct {
	hdr         rtp.Header
	absSendTime [3]byte
}

// write forwards one queued packet, the steps of 02 §9.3:
//  1. the gate and the binding: nothing is written, and the munger isn't even asked, before the viewer negotiated
//     the track and its sub PC's DTLS is up, so the first packet a viewer gets is the keyframe start that the
//     munger's first epoch begins with;
//  2. the munger gives the packet its place in the viewer's stream, or drops it, or waits for a keyframe of the
//     target layer and has the publisher asked for one (throttled; every waiting packet asks again, which heals a
//     lost PLI);
//  3. a fresh header: the viewer's payload type for the packet's profile, the munger's seq and ts, the binding's
//     SSRC, the publisher's marker bit and nothing else of the publisher's header (no CSRCs, no padding, none of its
//     extensions, whose ids are the pub PC's); abs-send-time when the viewer negotiated it;
//  4. pacing is README S84's;
//  5. the write, which never blocks (02 §5.4).
//
// A padding marker only moves the munger (skipPadding).
func (d *DownTrack) write(w *rtpWriteState, p *packet) {
	b := d.binding.Load()
	if !b.sendable() {
		d.setForwarding(false)
		return
	}
	now := monoNow()
	if p.padding {
		d.mu.Lock()
		d.m.skipPadding(p, now)
		d.mu.Unlock()
		return
	}
	pt, ok := b.ptFor[p.profile]
	if !ok {
		// The viewer negotiated no payload type that carries this profile. README S69 reports it (codec_mismatch) and
		// has the room's codec policy fix it; until a packet in a profile the viewer takes arrives, nothing flows.
		d.stats.noCodec.Add(1)
		return
	}

	d.mu.Lock()
	seq, ts, v := d.m.process(p, now)
	if v == verdictNewEpoch {
		d.interest.Store(d.interestLocked()) // a switch is done: the old layer's packets are no longer wanted
	}
	_, forwarding := d.m.current()
	d.mu.Unlock()
	d.setForwarding(forwarding)
	switch v {
	case verdictWaitKeyframe:
		p.layer.requestKeyframe(now)
		return
	case verdictDrop:
		return
	case verdictNewEpoch:
		d.stats.switches.Add(1)
	case verdictForward:
	}

	w.hdr = rtp.Header{
		Version: 2, Marker: p.marker, PayloadType: pt, SequenceNumber: seq, Timestamp: ts, SSRC: b.ssrc,
		Extensions: w.hdr.Extensions[:0],
	}
	if b.absSendTimeID != 0 {
		putAbsSendTime(&w.absSendTime, time.Now())
		_ = w.hdr.SetExtension(b.absSendTimeID, w.absSendTime[:]) // 3 bytes always fit the one-byte form
	}
	n, err := b.writer.WriteRTP(&w.hdr, p.payload)
	switch {
	case err != nil:
		d.stats.writeErrors.Add(1) // the sender stopped or the PC is closing: Unbind follows
	case n == 0:
		// Pion took the packet and sent nothing: the sub PC reports connected a moment before its SRTP session is
		// usable, and a sender's write path opens a moment after Bind. The viewer never saw this packet, so the
		// stream starts again with the next keyframe, which the packets that now wait for it ask for.
		d.mu.Lock()
		d.m.restart()
		d.interest.Store(d.interestLocked())
		d.mu.Unlock()
		d.setForwarding(false)
		d.stats.unsent.Add(1)
	default:
		d.stats.packets.Add(1)
		d.stats.bytes.Add(uint64(len(p.payload)))
	}
}

// putAbsSendTime writes the abs-send-time header extension for t: the 24 bits of NTP time around the binary point,
// 6 of seconds and 18 of fraction (the REMB estimator of browsers reads it; the sub API has no transport-cc).
func putAbsSendTime(b *[3]byte, t time.Time) {
	ns := t.UnixNano()
	frac := (ns % int64(time.Second)) << 18 / int64(time.Second)
	v := uint32(ns/int64(time.Second))<<18 | uint32(frac) // the NTP epoch offset is a multiple of 64 s
	b[0], b[1], b[2] = byte(v>>16), byte(v>>8), byte(v)
}

// ---- the viewer's RTCP (02 §9.3) ----

// readRTCP reads the viewer's RTCP for this track from sender until the sender stops (the track left the PC, or the
// PC closed). The sub PC that made the sender starts it and waits for it (subPC.readers).
func (d *DownTrack) readRTCP(sender *webrtc.RTPSender) {
	buf := make([]byte, rtpReadBuffer)
	for {
		n, _, err := sender.Read(buf)
		if err != nil {
			return
		}
		pkts, err := rtcp.Unmarshal(buf[:n])
		if err != nil {
			continue
		}
		d.handleRTCP(pkts)
	}
}

// handleRTCP acts on one compound packet from the viewer. Pion delivers a compound packet to every SSRC it names,
// so each DownTrack looks only at what names its own SSRC. A PLI or FIR becomes a keyframe request to the publisher
// for the layer forwarded now (a FIR is translated to a PLI upstream, 02 §9.7). NACKs are README S63's; REMB and
// receiver reports S84's.
func (d *DownTrack) handleRTCP(pkts []rtcp.Packet) {
	b := d.binding.Load()
	if b == nil || d.kind != webrtc.RTPCodecTypeVideo {
		return
	}
	wanted := false
	for _, pkt := range pkts {
		switch pkt := pkt.(type) {
		case *rtcp.PictureLossIndication:
			wanted = wanted || pkt.MediaSSRC == b.ssrc
		case *rtcp.FullIntraRequest:
			for _, e := range pkt.FIR {
				wanted = wanted || e.SSRC == b.ssrc
			}
		}
	}
	if !wanted {
		return
	}
	d.stats.keyRequests.Add(1)
	d.mu.Lock()
	slot, ok := d.m.current()
	if !ok {
		slot, ok = d.m.target, d.m.active
	}
	d.mu.Unlock()
	if ok {
		d.share.requestKeyframe(slot)
	}
}
