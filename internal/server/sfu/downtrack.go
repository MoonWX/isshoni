package sfu

import (
	"math"
	"math/rand/v2"
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
// viewer's RTCP for the track.
//
// Which layer it forwards is 02 §10.1's choice: its subscription says what it wants (setWant), and the DownTrack
// takes the best layer for that among the ones the share has (selectSlot), again whenever the share's layers change
// (Share.retargetLocked); a stream of a layer that has ended is over, and goes on with a keyframe of the layer
// chosen then. Whenever what the viewer gets changes, the subscriber's Conn hears of it (Subscription.changed) and
// tells the client (SubscriptionStateEvent).
//
// Repair and sync (02 §9.5, §9.6): what the viewer's NACKs ask for comes from the Layers' packet caches, through a
// second queue that the writer serves first (rtx.go); and the sender reports of the layer forwarded now go on to the
// viewer with their RTP time moved to the viewer's timeline, which is what keeps a share's audio and video in sync
// across layer switches and pauses (sendSR).
//
// What it counts, its bitrate and the loss its viewer reports are what Conn.Stats, Snapshot and Metrics show of it
// (02 §13, stats.go). Later slices add the rest of 02 §9.3: the pacer, REMB and the downlink allocator's use of the
// receiver reports (README S84).
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
	// queue holds what the Layers hand over, in their order: packets, padding markers and sender report notices
	// (packet). Video 1024, audio 256; a full queue drops (counted).
	queue chan *packet
	// rtxQueue holds the retransmissions the viewer's NACKs asked for (02 §9.5); the writer sends them before new
	// media. 256; a full queue drops (counted), and the viewer asks again.
	rtxQueue chan rtxItem
	nack     nackState     // the RTCP reader's: what was retransmitted when, and the budget
	done     chan struct{} // closed by stop: the writer ends
	stopOnce sync.Once

	// mu guards m, want and noted. It is the last lock in doc.go's order, never held across a call that takes another.
	mu sync.Mutex
	m  *munger
	// want is what the subscription asks of the track (setWant): for video the quality to forward, the client's
	// request under the caps of 02 §10.1; for audio QualityHigh while the client wants the share's audio, else
	// QualityOff. Which layer serves it depends on the layers the share has, so every change of want or of those
	// layers chooses again (retargetLocked), with the share's lock held: it keeps the choice in step with the layers.
	want Quality
	// noted is the munger's state (current) as the subscriber's Conn last heard of it: 0 while nothing is forwarded,
	// else 1 + the slot of the layer forwarded (noteLocked).
	noted uint8

	// forwarding: the writer forwards a layer to the viewer now (ShareInfo.Viewers). It is the munger's state
	// (current), written only while mu is held so that the actor and the writer can't leave it stale, and read
	// without a lock.
	forwarding atomic.Bool
	stats      dtStats
	// totals are the SFU's (counters.go), which the tallies of stats count into, and the layer switches; nil for a
	// DownTrack that a unit test builds without a Conn.
	totals *counters
}

// dtStats are a DownTrack's counters and rates (02 §13). The writer and the RTCP reader count; the ticker turns the
// bytes into the bitrate once a second (DownTrack.tick); Conn.Stats, Snapshot and Metrics read them. The tallies also
// count into the SFU's totals, which outlive the DownTrack (countInto).
type dtStats struct {
	packets     tally         // RTP packets written, retransmissions not included
	bytes       tally         // their payload bytes
	drops       tally         // packets a full queue dropped
	noCodec     atomic.Uint64 // packets in a profile the viewer negotiated no payload type for
	unsent      atomic.Uint64 // packets Pion took without sending them (WriteRTP returned 0 and no error)
	writeErrors atomic.Uint64
	switches    atomic.Uint64 // epochs started: the first keyframe, layer switches, resumes
	keyRequests atomic.Uint64 // PLIs and FIRs from the viewer
	srs         atomic.Uint64 // sender reports forwarded to the viewer

	// Repair (02 §9.5, §13). Every sequence number a NACK names is received, and then served, missed, skipped or
	// dropped by the full RTX queue.
	nackReceived tally // sequence numbers the viewer's NACKs asked for
	nackServed   tally // of those, retransmitted
	nackMissed   tally // not in a cache any more, or never sent
	rtxSkipped   tally // not retransmitted: asked for again too soon or too often, over budget, or too many at once
	rtxPackets   tally // retransmissions written, as RTX or plain (egress kind "rtx")
	rtxBytes     tally // their payload bytes, with the two of RTX's original sequence number
	rtxDrops     tally // retransmissions the full RTX queue dropped

	// lossPct is the fraction the viewer's last receiver report about the track says it lost, in percent
	// (math.Float64bits); 0 before the first one.
	lossPct atomic.Uint64
	// bitrate is what the DownTrack sends its viewer, media and retransmissions, in bit/s of payload: a 2 s moving
	// average that the ticker updates once a second (tick).
	bitrate atomic.Int64
	// The ticker's own state: only SFU.everySecond touches it.
	ticks     int    // ticks seen, up to 2: the first is the baseline
	tickAt    int64  // monoNow of the last tick
	tickBytes uint64 // bytes + rtxBytes at the last tick
}

// binding is one negotiated attachment of a DownTrack to a sub PC's sender (02 §9.3).
type binding struct {
	// id is the id of the sender's TrackLocalContext, one per sender: Unbind ends the binding only when Pion ends
	// that sender, not when the stop of an older sender (of the sub PC before a rebuild) arrives late.
	id     string
	pc     *subPC // the generation: its ready flag is the DTLS gate
	writer webrtc.TrackLocalWriter
	// rtcp sends the track's sender reports (02 §9.6): the sub PC's PeerConnection. nil when there is none to send on.
	rtcp          rtcpWriter
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

// RTP clock rates of the two codecs the SFU forwards.
const (
	videoClockRate = 90000
	audioClockRate = 48000
)

// newDownTrack returns a paused DownTrack. It starts no goroutine: the Conn starts the writer (run) when the
// subscription is made, and the sub PC the RTCP reader when the DownTrack gets its sender.
func newDownTrack(sub *Subscription, kind webrtc.RTPCodecType) *DownTrack {
	d := &DownTrack{
		share: sub.share, sub: sub, kind: kind, done: make(chan struct{}), rtxQueue: make(chan rtxItem, rtxQueueLen),
	}
	if kind == webrtc.RTPCodecTypeAudio {
		d.queue, d.m = make(chan *packet, audioQueueLen), newMunger(audioClockRate)
	} else {
		d.queue, d.m = make(chan *packet, videoQueueLen), newMunger(videoClockRate)
	}
	if sub.conn != nil {
		d.countInto(&sub.conn.sfu.counters)
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

// Bind implements webrtc.TrackLocal. Pion calls it once the sender's m-section is negotiated, inside the
// SetRemoteDescription of the viewer's answer; it only reads the negotiation and stores the result, so it never
// blocks. It maps each H.264 profile a publisher may send to the viewer's payload type for it (02 §8.2), finds the
// RTX payload types and the abs-send-time extension, and returns the codec the sender starts with. It never fails
// (02 §8.5): with video but no usable H.264 it returns the first video codec, and with no codec at all, which is how
// Pion binds the sender of an m-section the viewer rejected (a browser without H.264 answers its video with port 0),
// a zero codec, which Pion takes as it is. Either binding is unsupported: nothing is written through it. So one
// viewer without a decoder doesn't fail the whole negotiation, its audio flows, and the SFU's guard takes over
// (Conn.codecAnswered). On a sub PC that is already connected it asks the publisher for a keyframe: the viewer can't
// decode anything before one (02 §9.7).
func (d *DownTrack) Bind(ctx webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	b := &binding{
		id:       ctx.ID(),
		pc:       d.pc.Load(),
		writer:   ctx.WriteStream(),
		ssrc:     uint32(ctx.SSRC()),
		rtxSSRC:  uint32(ctx.SSRCRetransmission()),
		ptFor:    map[ProfileKey]uint8{},
		rtxPTFor: map[uint8]uint8{},
	}
	if b.pc != nil && b.pc.pc != nil {
		b.rtcp = b.pc.pc
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
	d.binding.Store(b)
	if chosen == nil {
		return webrtc.RTPCodecParameters{}, nil // a rejected m-section: see above
	}
	if b.sendable() {
		d.requestKeyframe()
	}
	return *chosen, nil
}

// mismatched reports whether the viewer negotiated no payload type for the profile the share's video is in now
// (02 §8.3, codec_mismatch): a viewer that decodes only Constrained Baseline, say, while the sharer still sends High.
// The DownTrack then forwards nothing, and goes on with the first keyframe in a profile the viewer takes, which the
// room's codec policy has the sharer send. It is false for audio, before the share's first keyframe, without a
// binding, and for a binding without H.264, which is the decoders' matter (Subscription.codec).
func (d *DownTrack) mismatched() bool {
	if d.kind != webrtc.RTPCodecTypeVideo {
		return false
	}
	b := d.binding.Load()
	if b == nil || b.unsupported {
		return false
	}
	profile := d.share.currentProfile()
	if profile == "" {
		return false
	}
	_, ok := b.ptFor[profile]
	return !ok
}

// Unbind implements webrtc.TrackLocal: the sender stopped (the subscription went away, or the PC closed). The writer
// drops packets until the next Bind, and what the viewer was getting is over: after a Bind the stream starts on a
// keyframe again. Like Bind it only stores, under the DownTrack's own lock, and never blocks.
//
// It ends the binding of the sender that stopped and no other. A sub PC that Pion closes by itself (the client's
// DTLS close_notify) stops its senders from a goroutine of Pion's, some time after the PC counts as closed: by then
// a rebuilt sub PC may have bound the DownTrack to its own sender, and that binding stays.
func (d *DownTrack) Unbind(ctx webrtc.TrackLocalContext) error {
	d.mu.Lock()
	changed := false
	if b := d.binding.Load(); b != nil && b.id == ctx.ID() && d.binding.CompareAndSwap(b, nil) {
		changed = d.restartLocked()
	}
	d.mu.Unlock()
	if changed {
		d.sub.changed()
	}
	return nil
}

// sendable reports whether a packet written through the binding can reach the viewer: the viewer negotiated a codec
// the SFU forwards, and the DTLS-ready gate of the binding's sub PC is open (02 §9.3, S4 finding 1: Pion drops RTP
// written before DTLS is up, without an error). The gate follows the PC: it is open while the PC is connected (or
// disconnected, which may pass) and closed again once it has failed or closed, when Pion drops everything the same
// silent way.
func (b *binding) sendable() bool {
	return b != nil && !b.unsupported && b.pc != nil && b.pc.ready.Load()
}

// ---- what the DownTrack forwards ----

// setWant sets what the subscription asks of the track, and has the DownTrack choose its layer for it among the ones
// the share has now (02 §10.1); the Conn's actor calls it when the subscription changes (Subscription.apply).
// QualityOff, or a request that no layer of the share can serve, pauses the track at once. A new layer keeps the one
// forwarded now flowing until its own keyframe arrives, so the interest mask holds both until then, and the publisher
// is asked for that keyframe unless the viewer can't get it yet: then the sub PC's connected state, or Bind, asks.
func (d *DownTrack) setWant(q Quality) {
	sh := d.share
	var ask *Layer
	sh.mu.Lock()
	d.mu.Lock()
	d.want = q
	slot, changed := d.retargetLocked(sh.presentLocked())
	if changed && d.kind == webrtc.RTPCodecTypeVideo && d.m.waitingForKeyframe() {
		ask = sh.layers[slot]
	}
	d.mu.Unlock()
	sh.mu.Unlock()
	if ask != nil && d.binding.Load().sendable() {
		ask.requestKeyframe(monoNow())
	}
}

// retargetLocked chooses the layer for what the subscription wants among the layers a share has (present holds their
// slot bits) and makes it the munger's target; without such a layer the track is paused until the share has one. It
// returns the slot chosen, which means nothing when none was, and whether the target changed. d.mu is held, and so is
// the share's lock, by both callers: setWant on the subscriber's actor, and Share.retargetLocked wherever a layer of
// the share comes or goes. The second one is why a viewer misses nothing of a layer that arrives: its track's first
// packet, the start of a keyframe, is read only after the DownTracks that want the layer have it as their target.
func (d *DownTrack) retargetLocked(present uint32) (slot Slot, changed bool) {
	slot, ok := selectSlot(d.kind, d.want, present)
	changed = d.m.setTarget(slot, ok)
	d.interest.Store(d.interestLocked())
	d.noteLocked()
	return slot, changed
}

// forwarded returns the quality of the layer the viewer gets now: QualityOff while it gets none, and for audio
// QualityHigh while it flows.
func (d *DownTrack) forwarded() Quality {
	d.mu.Lock()
	defer d.mu.Unlock()
	if slot, ok := d.m.current(); ok {
		return slot.quality()
	}
	return QualityOff
}

// restartLocked ends what the DownTrack forwards now without pausing it: the munger's epoch is over, the interest
// mask is the target's alone, and the viewer no longer counts as one. The stream goes on with the next keyframe of
// the target (munger.restart). It reports whether that changed what the viewer gets (noteLocked). d.mu is held.
func (d *DownTrack) restartLocked() bool {
	d.m.restart()
	d.interest.Store(d.interestLocked())
	return d.noteLocked()
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

// noteLocked brings the forwarding flag in line with the munger and reports whether what the viewer gets changed since
// the last call: forwarding began or ended, or went to another layer. Whoever gets true tells the subscriber's Conn
// once it has released the lock (Subscription.changed); the actor's own callers need not, they look at the
// subscription themselves. It writes only on a change: the writer calls it per packet. d.mu is held, so the flag is
// always the munger's state as the last holder left it: without the lock, a writer that stored "forwarding" just
// after the actor paused the track would leave a paused viewer listed for good.
func (d *DownTrack) noteLocked() bool {
	state := uint8(0)
	if slot, ok := d.m.current(); ok {
		state = 1 + uint8(slot)
	}
	if state == d.noted {
		return false
	}
	d.noted = state
	d.forwarding.Store(state != 0)
	return true
}

// ---- the writer (02 §9.3) ----

// enqueue hands a packet, a padding marker or a sender report notice of an interesting layer to the writer. It never
// blocks: a full queue drops the packet and counts it, so a writer that falls behind costs only its own viewer. (A
// notice that finds no room is not a packet lost: the next report comes in a second.) Layer read loops call it.
func (d *DownTrack) enqueue(p *packet) {
	select {
	case d.queue <- p:
	default:
		if !p.report {
			d.stats.drops.Add(1)
		}
	}
}

// run is the writer goroutine: it sends the retransmissions the viewer asked for and forwards what the Layers queue,
// until stop, or until the Conn that owns the subscription closes (connClosed), whichever comes first. That Conn
// starts it and waits for it (Conn.writers).
//
// Retransmissions come first (02 §9.3): a viewer that waits for one can't show anything newer, so a repair that
// queued behind new media would only add its delay to every frame after it.
func (d *DownTrack) run(connClosed <-chan struct{}) {
	// Not a secret: RFC 3550 asks for a random first sequence number, and the RTX stream is a stream of its own.
	w := rtpWriteState{rtxSeq: uint16(rand.Uint32())} //nolint:gosec // see above
	for {
		select {
		case it := <-d.rtxQueue:
			d.retransmit(&w, it)
			continue
		default:
		}
		select {
		case <-d.done:
			return
		case <-connClosed:
			return
		case it := <-d.rtxQueue:
			d.retransmit(&w, it)
		case p := <-d.queue:
			d.write(&w, p)
		}
	}
}

// stop ends the writer. It is idempotent, and safe for a DownTrack whose writer was never started.
func (d *DownTrack) stop() {
	d.stopOnce.Do(func() { close(d.done) })
}

// rtpWriteState is the writer's own state, one per DownTrack. Part of it is reused from packet to packet so that
// forwarding allocates nothing: the header, whose extension list keeps its capacity, and the bytes of the
// abs-send-time extension. The rest follows how the stream's start goes on the current binding.
type rtpWriteState struct {
	hdr         rtp.Header
	absSendTime [3]byte

	// binding is the binding the fields below are about: a new one starts them again.
	binding *binding
	// sent: a packet has left through binding since its gate last opened. Until then a packet that Pion takes
	// without sending is a start the viewer never saw, and the stream starts again; after it, such a packet is an
	// ordinary lost packet.
	sent bool
	// unsentStarts counts the starts in a row that never left, up to unsentStartLimit. From there on the DownTrack
	// has a keyframe asked for only every unsentStartBackoff; askedAt is the monoNow it last did, or reached the
	// limit.
	unsentStarts int
	askedAt      int64
	// packets and octets are what has left through binding on the track's media SSRC: the counts of the sender
	// reports the viewer gets (02 §9.6).
	packets, octets uint32

	// rtxSeq is the next sequence number of the DownTrack's RTX stream (02 §9.5).
	rtxSeq uint16
}

// bind makes b the binding the writer's state is about: with another one than before, the stream's start and its
// counts begin again.
func (w *rtpWriteState) bind(b *binding) {
	if b != w.binding {
		w.binding, w.sent, w.unsentStarts, w.packets, w.octets = b, false, 0, 0, 0
	}
}

// count notes a packet with n payload bytes that left on the media SSRC.
func (w *rtpWriteState) count(n int) {
	w.packets++
	w.octets += uint32(n)
}

// write forwards one queued packet, the steps of 02 §9.3:
//  1. the gate and the binding: nothing is written, and the munger isn't asked for a place, before the viewer
//     negotiated the track and its sub PC's DTLS is up, so the first packet a viewer gets is the keyframe start that
//     the munger's first epoch begins with. A gate that closes again (the PC failed or closed) or an Unbind ends
//     what was forwarded, so what follows a reopened gate or the next Bind starts on a keyframe too;
//  2. the munger gives the packet its place in the viewer's stream, or drops it, or waits for a keyframe of the
//     target layer and has the publisher asked for one (throttled; every waiting packet asks again, which heals a
//     lost PLI);
//  3. a fresh header: the viewer's payload type for the packet's profile, the munger's seq and ts, the binding's
//     SSRC, the publisher's marker bit and nothing else of the publisher's header (no CSRCs, no padding, none of its
//     extensions, whose ids are the pub PC's); abs-send-time when the viewer negotiated it;
//  4. pacing is README S84's;
//  5. the write, which never blocks (02 §5.4). A packet that Pion takes without sending it is unsent's. The first
//     packet of an epoch is followed by the layer's latest sender report (sendSR), so the viewer knows the new
//     layer's place in time at once: after a switch, and when audio is turned on.
//
// A padding marker only moves the munger (skipPadding), and a sender report notice sends the layer's report on
// (sendSR).
//
// When the packet changed what the viewer gets (its stream began or ended, or went on in another layer), the
// subscriber's Conn hears of it, after the write: a start that Pion took without sending has by then been taken back,
// so the client isn't told of a stream it never saw.
func (d *DownTrack) write(w *rtpWriteState, p *packet) {
	if d.forward(w, p) {
		d.sub.changed()
	}
}

// forward is write without the notice: it reports whether what the viewer gets changed.
func (d *DownTrack) forward(w *rtpWriteState, p *packet) (changed bool) {
	b := d.binding.Load()
	w.bind(b)
	if !b.sendable() {
		// The viewer gets nothing now, and missed what it was getting: the stream it had is over.
		w.sent, w.unsentStarts = false, 0
		d.mu.Lock()
		if _, forwarding := d.m.current(); forwarding {
			changed = d.restartLocked()
		}
		d.mu.Unlock()
		return changed
	}
	now := monoNow()
	if p.report {
		d.sendSR(w, b, p.layer, now)
		return false
	}
	if p.padding {
		d.mu.Lock()
		d.m.skipPadding(p, now)
		d.mu.Unlock()
		return false
	}
	pt, ok := b.ptFor[p.profile]
	if !ok {
		// The viewer negotiated no payload type that carries this profile: its subscriber's actor reports
		// codec_mismatch (mismatched), and the room's codec policy has the sharer switch to a profile every viewer
		// takes (02 §8.3). Until a packet in such a profile arrives, nothing flows. If the layer forwarded now has
		// moved to this profile, what the viewer was getting is over (02 §9.4). Nobody is asked for a keyframe: the
		// viewer couldn't decode it.
		d.stats.noCodec.Add(1)
		d.mu.Lock()
		if d.m.unforwardable(p) {
			d.interest.Store(d.interestLocked())
			changed = d.noteLocked()
		}
		d.mu.Unlock()
		return changed
	}

	d.mu.Lock()
	var last *Layer // the Layer of the stream so far: its newest epoch's
	if d.m.n > 0 {
		last = d.m.newest().layer
	}
	seq, ts, v := d.m.process(p, now)
	if v == verdictNewEpoch {
		d.interest.Store(d.interestLocked()) // a switch is done: the old layer's packets are no longer wanted
	}
	changed = d.noteLocked()
	d.mu.Unlock()
	switch v {
	case verdictWaitKeyframe:
		if w.unsentStarts >= unsentStartLimit {
			// Nothing has left through this binding yet, start after start: the next try can wait.
			if now-w.askedAt < int64(unsentStartBackoff) {
				return changed
			}
			w.askedAt = now
		}
		p.layer.requestKeyframe(now)
		return changed
	case verdictDrop:
		return changed
	case verdictNewEpoch:
		d.stats.switches.Add(1)
		if last != nil && last.slot != p.layer.slot && d.totals != nil {
			// A layer switch (isshoni_sfu_layer_switches_total): the viewer's stream goes on in another layer than
			// it was in, at once or after a pause. A resume in the same layer, or a publisher's new track for it,
			// is none.
			d.totals.layerSwitches.Add(1)
		}
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
		if d.unsent(w, now) {
			changed = true
		}
	default:
		w.sent, w.unsentStarts = true, 0
		w.count(len(p.payload))
		d.stats.packets.Add(1)
		d.stats.bytes.Add(uint64(len(p.payload)))
		if v == verdictNewEpoch {
			d.sendSR(w, b, p.layer, now)
		}
	}
	return changed
}

// sendSR sends the viewer the latest sender report of layer l, as a report about the DownTrack's own stream
// (02 §9.6): the publisher's NTP time as it is, its RTP time moved by the current epoch's timestamp offset, and the
// DownTrack's own counts. A browser reads from it which wall-clock instant an RTP timestamp of the track stands for,
// and plays the audio and the video of a share, which have the same msid stream, by that. So the SFU adds no error
// of its own while the publisher's reports are fresh: both tracks stay on the publisher's capture clock, through
// layer switches, pauses and publisher rebuilds.
//
// Nothing is sent unless the DownTrack forwards l now (another layer's timeline isn't the viewer's), for a report
// older than 10 s, or without a PeerConnection to send on. The writer calls it: for every report that arrives while
// it forwards the layer (the Layer's notice), and right after the first packet of a new epoch.
func (d *DownTrack) sendSR(w *rtpWriteState, b *binding, l *Layer, now int64) {
	sr := l.lastSR.Load()
	if sr == nil || now-sr.arrival > int64(srMaxAge) || b.rtcp == nil {
		return
	}
	d.mu.Lock()
	ts, ok := d.m.translateSR(l, sr)
	d.mu.Unlock()
	if !ok {
		return
	}
	report := &rtcp.SenderReport{SSRC: b.ssrc, NTPTime: sr.ntp, RTPTime: ts, PacketCount: w.packets, OctetCount: w.octets}
	if err := b.rtcp.WriteRTCP([]rtcp.Packet{report}); err != nil {
		return // the PC is closing: Unbind follows
	}
	d.stats.srs.Add(1)
}

// unsent handles a packet that Pion took without sending it and without an error. Pion does that in two situations
// that have nothing in common:
//
//   - Before anything has left through the binding: a sub PC reports connected a moment before its SRTP session is
//     usable (S4 finding 1), and a sender's write path opens a moment after Bind. The viewer never saw the packet, a
//     keyframe start, so the stream must not go on from it: it starts again with the next keyframe, which the packets
//     that now wait for it ask for. A binding that never gets a packet out would keep that up at the layer's two
//     keyframes a second, for every viewer of the layer, so from the second start in a row on the DownTrack asks only
//     once per unsentStartBackoff.
//   - In mid-stream, whenever the ICE transport can't send right now: no selected candidate pair (a PC that has
//     just failed, until its gate closes), or a full write buffer on ICE-TCP (a slow viewer). That is an ordinary
//     lost packet, like one lost on the network: the viewer sees the gap and asks for what it needs (a NACK, or a
//     PLI). Restarting here would turn every such packet into a keyframe for all viewers of the layer.
//
// It reports whether it changed what the viewer gets (the first case).
func (d *DownTrack) unsent(w *rtpWriteState, now int64) bool {
	d.stats.unsent.Add(1)
	if w.sent {
		return false
	}
	if w.unsentStarts < unsentStartLimit {
		if w.unsentStarts++; w.unsentStarts == unsentStartLimit {
			w.askedAt = now
		}
	}
	d.mu.Lock()
	changed := d.restartLocked()
	d.mu.Unlock()
	return changed
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
// PC closed). The sub PC that made the sender, pc, starts it and waits for it (subPC.readers). A PC that is heard
// from has its round-trip time read now and then (subPC.sampleRTT), before what the viewer sent is acted on: the
// handling of a NACK goes by it.
func (d *DownTrack) readRTCP(pc *subPC, sender *webrtc.RTPSender) {
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
		pc.sampleRTT(sender, monoNow())
		d.handleRTCP(pkts)
	}
}

// handleRTCP acts on one compound packet from the viewer. Pion delivers a compound packet to every SSRC it names,
// so each DownTrack looks only at what names its own SSRC. A NACK is answered from the packet cache (02 §9.5,
// handleNACK), for audio as for video. A PLI or FIR about a video track becomes a keyframe request to the publisher
// for the layer forwarded now (a FIR is translated to a PLI upstream, 02 §9.7). A receiver report about the track
// gives its loss (DownTrackStats.LossPct, 02 §13). REMB, and what the downlink allocator makes of the receiver reports,
// are README S84's.
func (d *DownTrack) handleRTCP(pkts []rtcp.Packet) {
	b := d.binding.Load()
	if b == nil {
		return
	}
	wanted := false
	left := nackBatch
	for _, pkt := range pkts {
		switch pkt := pkt.(type) {
		case *rtcp.ReceiverReport:
			for _, r := range pkt.Reports {
				if r.SSRC == b.ssrc {
					d.stats.lossPct.Store(math.Float64bits(100 * float64(r.FractionLost) / 256))
				}
			}
		case *rtcp.TransportLayerNack:
			if pkt.MediaSSRC == b.ssrc {
				d.handleNACK(b, pkt.Nacks, &left, monoNow())
			}
		case *rtcp.PictureLossIndication:
			wanted = wanted || pkt.MediaSSRC == b.ssrc
		case *rtcp.FullIntraRequest:
			for _, e := range pkt.FIR {
				wanted = wanted || e.SSRC == b.ssrc
			}
		}
	}
	if !wanted || d.kind != webrtc.RTPCodecTypeVideo {
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
