package sfu

import (
	"math"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// Slot is a Layer's place in a share (02 §9.1): the simulcast layers q, h and f, and the audio layer. In DownTrack
// interest masks, 1<<slot is the slot's bit.
type Slot uint8

// Slots, in the order of their bits. SlotH is the M5 middle layer; rid "h" is rejected in M1 (02 §5.2).
const (
	SlotQ Slot = iota
	SlotH
	SlotF
	SlotAudio
)

// String returns the slot's rid, or "audio".
func (s Slot) String() string {
	switch s {
	case SlotQ:
		return ridPreview
	case SlotH:
		return "h"
	case SlotF:
		return ridFull
	case SlotAudio:
		return "audio"
	}
	return "slot?"
}

// quality returns what a viewer who gets the slot's layer gets (SubscriptionStateEvent.Forwarded): high for the full
// layer, low for the preview layer. The M5 middle layer counts as low until QualityMedium exists; the audio layer is
// either forwarded or not, which is QualityHigh or QualityOff.
func (s Slot) quality() Quality {
	if s == SlotQ || s == SlotH {
		return QualityLow
	}
	return QualityHigh
}

// monoBase anchors monoNow.
var monoBase = time.Now()

// monoNow returns the monotonic time in ns that the media path stamps packets, sender reports, epochs and keyframe
// requests with. Only differences mean anything.
func monoNow() int64 { return int64(time.Since(monoBase)) }

// rtcpWriter sends RTCP on a PeerConnection: a *webrtc.PeerConnection, or a recorder in tests. WriteRTCP is a media
// call: it never blocks, and any goroutine may make it (02 §5.4).
type rtcpWriter interface {
	WriteRTCP(pkts []rtcp.Packet) error
}

// Layer is one incoming RTP stream of a share: a simulcast layer or the audio track (02 §9.1). A publisher rebuild
// attaches new Layer instances to the same Share, so munger epochs compare Layers by pointer, never by rid.
//
// A pubTrack (pubpc.go) reads the track and hands every packet to the Layer it is attached to (handleRTP), and every
// sender report (handleSR): Pion delivers a track whether or not it carries a share, so the read loops are the
// track's and the Layer is what a share makes of them. The identity fields are set before the Layer is attached and
// never change; everything else is atomic, because the read loops, the DownTrack writers, the ticker and the readers
// of ShareInfo all look at a Layer without a lock.
type Layer struct {
	slot   Slot
	cache  *packetCache
	lastSR atomic.Pointer[srInfo] // the latest sender report for this layer's SSRC; nil until the first one

	share *Share
	rid   string // "f" | "q" for video (a track without a rid is "f"), "" for audio
	kind  webrtc.RTPCodecType
	track *webrtc.TrackRemote
	recv  *webrtc.RTPReceiver
	pubPC rtcpWriter // the pub PC, for PLI (02 §9.7) and REMB (02 §11)
	// rtcpSSRC is the sender SSRC of the RTCP the SFU writes about this layer: its pub PC's (pubPC.rtcpSSRC).
	rtcpSSRC uint32
	ssrc     uint32

	lastPLI atomic.Int64            // monoNow of the last keyframe request sent; the 500 ms throttle of 02 §9.7
	lastRTP atomic.Int64            // monoNow of the last packet
	sps     atomic.Pointer[spsInfo] // from the last SPS that parsed: size and profile, for LayerInfo
	stats   layerStats

	// The RTP loop's own state: only the track's reader goroutine touches it.
	seqStarted bool
	highSeq    uint16 // the highest sequence number seen (int16 order)
	tsStarted  bool
	highTS     uint32 // the highest timestamp of a cached packet (int32 order): a newer one is a new frame
}

// layerStats are a Layer's counters and rates (02 §9.1). The RTP loop counts; the ticker turns the counts into rates
// once a second (Layer.tick); ShareInfo and the DownTracks read the atomics.
type layerStats struct {
	packets    atomic.Uint64 // RTP packets read, whatever became of them
	bytes      atomic.Uint64 // their payload bytes
	duplicates atomic.Uint64 // copies of a packet the cache already held: not forwarded
	tooLate    atomic.Uint64 // packets a cache capacity or more behind: not forwarded
	badPT      atomic.Uint64 // video packets whose payload type is no H.264 the SFU forwards: dropped
	keyframes  atomic.Uint64 // packets that start a keyframe
	frames     atomic.Uint64 // distinct timestamps, counted as they move forward
	// expected is how many sequence numbers the stream has spanned (highest − first + 1), unique how many of them
	// arrived in time (not a duplicate, not too late). Their difference over an interval is the interval's loss.
	expected     atomic.Uint64
	unique       atomic.Uint64
	pliSent      atomic.Uint64 // keyframe requests written to the publisher
	pliThrottled atomic.Uint64 // requests that fell within 500 ms of the last one

	// Rates, written by the ticker.
	bitrate atomic.Int64  // bit/s of payload, 2 s moving average
	pps     atomic.Uint64 // packets per second, 2 s moving average (math.Float64bits)
	fps     atomic.Uint64 // frames per second, 2 s moving average (math.Float64bits)
	lossPct atomic.Uint64 // loss over the last two seconds, in percent (math.Float64bits)

	// The ticker's own state: only SFU.everySecond touches it.
	ticks  int         // how many ticks the Layer has seen, up to 2: the first is the baseline, the second the first rates
	prev   layerSample // the counters at the last tick
	window [2]lossSample
}

// layerSample is what the ticker read from a Layer's counters at one tick.
type layerSample struct {
	at                                       int64
	packets, bytes, frames, expected, unique uint64
}

// lossSample is one second of a Layer's sequence space: how many numbers it spanned and how many arrived.
type lossSample struct{ expected, received uint64 }

// newLayer returns a Layer for a slot with an empty packet cache sized for its kind.
func newLayer(slot Slot, kind webrtc.RTPCodecType) *Layer {
	l := &Layer{slot: slot, cache: newPacketCache(kind), kind: kind}
	l.lastPLI.Store(-int64(pliInterval)) // the first request is never throttled
	return l
}

// slotFor returns the slot of an incoming track: the audio slot, or the simulcast layer its rid names. A video track
// without a rid is the single full layer (02 §5.2). ok is false for a rid the SFU doesn't forward ("h" until M5).
func slotFor(kind webrtc.RTPCodecType, rid string) (slot Slot, ok bool) {
	if kind == webrtc.RTPCodecTypeAudio {
		return SlotAudio, rid == ""
	}
	switch rid {
	case "", ridFull:
		return SlotF, true
	case ridPreview:
		return SlotQ, true
	}
	return 0, false
}

// srInfo is a sender report of a Layer's SSRC as the SFU keeps it (02 §9.6).
type srInfo struct {
	ntp     uint64 // NTP timestamp, 32.32 fixed point
	rtp     uint32 // RTP timestamp at that NTP time
	arrival int64  // monotonic ns
}

// packet is one incoming RTP packet (02 §9.1). It is immutable after the Layer's RTP loop builds it, and shared by the
// packet cache and every DownTrack queue.
type packet struct {
	layer    *Layer
	seq      uint16
	ts       uint32
	marker   bool
	pt       uint8      // the publisher's payload type; viewers get their own (binding.ptFor)
	profile  ProfileKey // video: from the PT via the pub PC's negotiated codecs; "" for audio
	keyStart bool       // video: carries an SPS (isKeyframeStart); audio: always true
	padding  bool       // no payload (probe padding): never cached, used only to close sequence gaps
	payload  []byte     // exact-size copy, header extensions removed
	arrival  int64      // monotonic ns
}

// handleRTP is the RTP loop of 02 §9.1 for one packet of the Layer's track, already parsed: pkt's payload is without
// header extensions and padding, and aliases the reader's buffer. profile is the H.264 profile of pkt's payload type
// ("" for audio, and for a video payload type the SFU doesn't forward). Only the track's reader calls it.
//
// The packet is copied once, here, into an exact-size slice that the cache and every DownTrack queue share. A copy
// of a packet the cache already holds, and one a capacity or more late, is counted and goes no further: forwarding
// Chrome's redundant RTX copies would cost egress for every viewer and hide real loss from their receiver reports.
// Everything else goes to the DownTracks whose interest mask has this layer's bit. A padding-only packet is never
// cached; it goes out as a marker, so that the DownTracks forwarding this layer can close the gap its sequence
// number would leave.
func (l *Layer) handleRTP(pkt *rtp.Packet, profile ProfileKey, now int64) {
	l.lastRTP.Store(now)
	l.stats.packets.Add(1)
	l.stats.bytes.Add(uint64(len(pkt.Payload)))
	video := l.kind == webrtc.RTPCodecTypeVideo
	if video && profile == "" {
		l.stats.badPT.Add(1)
		return
	}
	l.span(pkt.SequenceNumber)
	p := &packet{
		layer: l, seq: pkt.SequenceNumber, ts: pkt.Timestamp, marker: pkt.Marker, pt: pkt.PayloadType,
		profile: profile, arrival: now,
	}
	if len(pkt.Payload) == 0 {
		p.padding = true
		l.stats.unique.Add(1)
		l.fanOut(p)
		return
	}
	p.payload = make([]byte, len(pkt.Payload))
	copy(p.payload, pkt.Payload)
	p.keyStart = !video || isKeyframeStart(p.payload)
	switch l.cache.insert(p) {
	case cacheDuplicate:
		l.stats.duplicates.Add(1)
		return
	case cacheTooLate:
		l.stats.tooLate.Add(1)
		return
	case cacheInserted:
	}
	l.stats.unique.Add(1)
	if !l.tsStarted || int32(p.ts-l.highTS) > 0 {
		l.tsStarted, l.highTS = true, p.ts
		l.stats.frames.Add(1)
	}
	if video {
		if p.keyStart {
			l.keyframe(p)
		} else if l.share.awaitsKeyframe.Load() && l.share.conn.pubUp.Load() {
			// The share is pending or stalled and this isn't the keyframe it waits for: its start was lost, the track
			// was already running when it was bound to the share, or the keyframe came before the Conn's actor had
			// seen the pub PC connected again. Ask, at most every 500 ms; not while the actor has the PC as away, when
			// a keyframe wouldn't end stalled and the actor asks anyway once the PC is back (Conn.onPCState).
			l.requestKeyframe(now)
		}
	}
	l.fanOut(p)
}

// span widens the Layer's sequence space to seq: the count of numbers between the first and the highest one seen.
func (l *Layer) span(seq uint16) {
	if !l.seqStarted {
		l.seqStarted, l.highSeq = true, seq
		l.stats.expected.Store(1)
		return
	}
	if ahead := int16(seq - l.highSeq); ahead > 0 {
		l.highSeq = seq
		l.stats.expected.Add(uint64(ahead))
	}
}

// keyframe notes a packet that starts a keyframe: the SPS gives the layer's size, and the share hears of it, because
// its first keyframe makes a pending share live (02 §5.3).
func (l *Layer) keyframe(p *packet) {
	l.stats.keyframes.Add(1)
	if info, err := parseSPS(spsNAL(p.payload)); err == nil {
		if cur := l.sps.Load(); cur == nil || *cur != info {
			l.sps.Store(&info)
		}
	}
	l.share.keyframe(l, p.profile)
}

// fanOut hands p to every DownTrack of the share that wants this layer's packets. The list is the share's
// copy-on-write fan-out list, read without a lock.
func (l *Layer) fanOut(p *packet) {
	list := l.share.fanOut(l.kind).Load()
	if list == nil {
		return
	}
	bit := uint32(1) << l.slot
	for _, dt := range *list {
		if dt.interest.Load()&bit != 0 {
			dt.enqueue(p)
		}
	}
}

// handleSR keeps a sender report of the Layer's SSRC (02 §9.1): the munger aligns timestamps through it when a
// DownTrack switches layers (02 §9.4). Forwarding it to the viewers is README S63's (02 §9.6).
func (l *Layer) handleSR(sr *rtcp.SenderReport, now int64) {
	if sr.SSRC != l.ssrc {
		return
	}
	l.lastSR.Store(&srInfo{ntp: sr.NTPTime, rtp: sr.RTPTime, arrival: now})
}

// requestKeyframe sends the publisher a PLI for this layer, unless one went out less than 500 ms ago (02 §9.7). The
// check is a compare-and-swap on lastPLI, so concurrent callers produce one PLI; the others are counted as
// throttled. It reports whether a PLI was written. It takes no lock and never blocks: DownTrack writers and RTCP
// readers, the layer's own read loop and any Conn's actor call it.
func (l *Layer) requestKeyframe(now int64) bool {
	if l.kind != webrtc.RTPCodecTypeVideo {
		return false
	}
	last := l.lastPLI.Load()
	if now-last < int64(pliInterval) || !l.lastPLI.CompareAndSwap(last, now) {
		l.stats.pliThrottled.Add(1)
		return false
	}
	if l.pubPC == nil {
		return false
	}
	pli := &rtcp.PictureLossIndication{SenderSSRC: l.rtcpSSRC, MediaSSRC: l.ssrc}
	if err := l.pubPC.WriteRTCP([]rtcp.Packet{pli}); err != nil {
		// The pub PC isn't connected, or is closing. Whoever still waits for the keyframe asks again in 500 ms.
		return false
	}
	l.stats.pliSent.Add(1)
	return true
}

// active reports whether a packet arrived within the last 2 s.
func (l *Layer) active(now int64) bool {
	return l.stats.packets.Load() > 0 && now-l.lastRTP.Load() <= int64(activeAfter)
}

// info returns the Layer's LayerInfo: the size from the last SPS and the rates of the last tick.
func (l *Layer) info(now int64) LayerInfo {
	li := LayerInfo{
		RID:     l.slot.String(),
		FPS:     math.Float64frombits(l.stats.fps.Load()),
		Bitrate: int(l.stats.bitrate.Load()),
		LossPct: math.Float64frombits(l.stats.lossPct.Load()),
		Active:  l.active(now),
	}
	if sps := l.sps.Load(); sps != nil {
		li.Width, li.Height = sps.width, sps.height
	}
	return li
}

// tick turns the Layer's counters into its rates and sizes its packet cache; the SFU's ticker calls it once a second
// (02 §9.1). Bitrate, packet rate and frame rate are 2 s moving averages. Loss is what the last two seconds of the
// sequence space are missing: a gap that a late or retransmitted packet fills within that time is no loss.
func (l *Layer) tick(now int64) {
	st := &l.stats
	cur := layerSample{
		at: now, packets: st.packets.Load(), bytes: st.bytes.Load(), frames: st.frames.Load(),
		expected: st.expected.Load(), unique: st.unique.Load(),
	}
	prev := st.prev
	st.prev = cur
	if st.ticks == 0 {
		st.ticks = 1
		return // the first tick only takes the baseline: the Layer may be a few ms old
	}
	dt := time.Duration(cur.at - prev.at)
	if dt <= 0 {
		return
	}
	per := func(n uint64) float64 { return float64(n) / dt.Seconds() }
	weight := 1 - math.Exp(-float64(dt)/float64(rateWindow))
	if st.ticks == 1 {
		st.ticks, weight = 2, 1 // the first measured second is the average so far
	}
	average := func(old, sample float64) float64 { return old + weight*(sample-old) }

	st.bitrate.Store(int64(math.Round(average(float64(st.bitrate.Load()), per(cur.bytes-prev.bytes)*8))))
	pps := average(math.Float64frombits(st.pps.Load()), per(cur.packets-prev.packets))
	st.pps.Store(math.Float64bits(pps))
	st.fps.Store(math.Float64bits(average(math.Float64frombits(st.fps.Load()), per(cur.frames-prev.frames))))

	st.window[0] = st.window[1]
	st.window[1] = lossSample{expected: cur.expected - prev.expected, received: cur.unique - prev.unique}
	expected := st.window[0].expected + st.window[1].expected
	received := st.window[0].received + st.window[1].received
	loss := 0.0
	if expected > received {
		loss = 100 * float64(expected-received) / float64(expected)
	}
	st.lossPct.Store(math.Float64bits(loss))

	l.cache.resize(pps, now)
}
