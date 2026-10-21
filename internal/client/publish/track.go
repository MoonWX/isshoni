package publish

import (
	"math/bits"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/media"
)

// track is the Publisher's webrtc.TrackLocal: one simulcast encoding, or the audio. Pion binds it when the answer is
// applied; the send loop packetizes into it, stamping the mid and rid header extensions itself, and writes the
// packets, or hands a video frame's packets to the pacer to write (Options.Pacing).
type track struct {
	kind           webrtc.RTPCodecType
	id, stream     string
	rid            string    // the encoding's rid; "" for audio and for a single layer
	layer          string    // the Source layer; "" for audio
	table          h264Table // video: the profiles the Publisher can send; set by New before the track is bound
	clock          uint32
	tr             *webrtc.RTPTransceiver // set once in addTracks, before any Bind
	seq            uint16                 // send loop only, like the fields below
	tsBase         uint32
	pay            codecs.H264Payloader
	midExt, ridExt []byte
	frames         int // frames packetized (video)

	// The test options, set by New before the track is bound: padEvery is Options.Padding (video), and repair is
	// there with Options.RedundantRTX (repair.go).
	padEvery int
	repair   *repair

	mu sync.Mutex // guards the fields below
	b  *binding
	// profile is the H.264 profile key the video goes out in: Options.Profile until Bind chooses, then what the
	// negotiation chose. next is the one it switches to at a coming keyframe (switchWhen), "" while it stays.
	profile, next string
	// nextIDC is, while a switch waits, the profile_idc its keyframe must have, or 0 when any keyframe will do. The
	// Publisher sets it when it asked its ProfileSource for another engine profile, whether or not SetProfile
	// succeeded (applyProfile): a keyframe that the old encoder made, still on its way or because the switch failed,
	// must not go out on the new payload type (switchesAt).
	nextIDC byte
	// inactive: SetActive turned the video layer off, and it sends nothing. resuming: it was turned on again and
	// sends nothing until its next keyframe (admit).
	inactive, resuming bool
	sent               bool
	lastTS             uint32
	lastNTP            uint64
	stats              TrackStats
	// The live numbers of TrackStats (stats.go): the wire bytes of each frame written, over rateWindow (Bitrate,
	// FPS); video's capture-to-send latencies, over latencyWindow; the last sender reports written (RTT).
	rate, latency window
	srs           []srSent
}

// binding is what Bind learned from the negotiation.
type binding struct {
	w            webrtc.TrackLocalWriter
	ssrc         uint32
	pt           uint8
	midID, ridID uint8 // header extension IDs; 0 when not negotiated
	// The RTX stream of the track's payload type: its SSRC and payload type, 0 when RTX isn't negotiated for it, and
	// the ID of the repaired-rtp-stream-id header extension, which names the rid an RTX packet repairs.
	rtxSSRC uint32
	rtxPT   uint8
	rridID  uint8
}

func newTrack(kind webrtc.RTPCodecType, id, stream, rid, layer, profile string) *track {
	t := &track{
		kind: kind, id: id, stream: stream, rid: rid, layer: layer, profile: profile, clock: videoClock,
		rate: newWindow(rateWindow), latency: newWindow(latencyWindow),
	}
	if profile != "" {
		t.table = h264Table{profile}
	}
	if kind == webrtc.RTPCodecTypeAudio {
		t.clock = audioClock
	}
	t.seq, t.tsBase = randomBase()
	t.stats.Kind, t.stats.RID, t.stats.Layer = kind, rid, layer
	return t
}

// ID, RID, StreamID and Kind implement webrtc.TrackLocal.
func (t *track) ID() string                { return t.id }
func (t *track) RID() string               { return t.rid }
func (t *track) StreamID() string          { return t.stream }
func (t *track) Kind() webrtc.RTPCodecType { return t.kind }

// Bind implements webrtc.TrackLocal: it picks the first negotiated codec the track can carry (an H.264 profile of
// the Publisher's with packetization-mode=1, or Opus), which is the answer's first one, as a browser picks it, and
// the mid and rid header extension IDs.
func (t *track) Bind(ctx webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	for _, c := range ctx.CodecParameters() {
		if !t.carries(c) {
			continue
		}
		b := &binding{w: ctx.WriteStream(), ssrc: uint32(ctx.SSRC()), pt: uint8(c.PayloadType)}
		for _, e := range ctx.HeaderExtensions() {
			switch e.URI {
			case sdp.SDESMidURI:
				b.midID = uint8(e.ID)
			case sdp.SDESRTPStreamIDURI:
				b.ridID = uint8(e.ID)
			case sdp.SDESRepairRTPStreamIDURI:
				b.rridID = uint8(e.ID)
			}
		}
		if pt, ok := rtxFor(c.PayloadType, ctx.CodecParameters()); ok && ctx.SSRCRetransmission() != 0 {
			b.rtxSSRC, b.rtxPT = uint32(ctx.SSRCRetransmission()), pt
		}
		t.mu.Lock()
		t.b = b
		t.stats.SSRC = b.ssrc
		if t.kind == webrtc.RTPCodecTypeVideo {
			t.profile, t.next, t.nextIDC = profileKey(c.SDPFmtpLine), "", 0
		}
		t.mu.Unlock()
		return c, nil
	}
	return webrtc.RTPCodecParameters{}, webrtc.ErrUnsupportedCodec
}

// switchWhen has the video track go on in another of the Publisher's profiles from a coming keyframe (packetize):
// its next one when idc is 0, else the first whose SPS has profile_idc idc (switchesAt). It reports whether it will
// switch: false when it sends that profile already, or isn't bound.
func (t *track) switchWhen(profile string, idc byte) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.b == nil || t.profile == profile {
		t.next, t.nextIDC = "", 0
		return false
	}
	t.next, t.nextIDC = profile, idc
	return true
}

// switchesAt reports whether a keyframe is the one a waiting switch happens at: any keyframe, unless the switch
// waits for the new encoder's (nextIDC). Then it is the first keyframe whose SPS has the new profile_idc. A keyframe
// without an SPS in front (which the media contract doesn't allow) switches when it is a Reinit one, the first after
// the encoder's re-initialization. A re-init that didn't change the profile (a CB-only encoder asked for High stays
// CB, 04 §9.6) leaves the layer on the payload type that names what it sends; the core then re-offers with the
// actual profile (§8.6). t.mu is held.
func (t *track) switchesAt(pkt media.Packet) bool {
	if t.nextIDC == 0 {
		return true
	}
	if idc, ok := spsProfileIDC(pkt.Data); ok {
		return idc == t.nextIDC
	}
	return pkt.Reinit
}

// switchLocked makes the profile that switchWhen set the one the track sends: a new binding with that profile's
// payload type, and its RTX payload type when the track has an RTX stream. Repairs of packets sent before go out
// with the new payload types too. t.mu is held.
func (t *track) switchLocked() {
	pt, rtx, ok := t.table.pts(t.next)
	if ok && t.b != nil {
		b := *t.b
		b.pt = pt
		if b.rtxSSRC != 0 {
			b.rtxPT = rtx
		}
		t.b, t.profile = &b, t.next
	}
	t.next, t.nextIDC = "", 0
}

// Unbind implements webrtc.TrackLocal. Packets are dropped until the next Bind.
func (t *track) Unbind(webrtc.TrackLocalContext) error {
	t.mu.Lock()
	t.b = nil
	t.mu.Unlock()
	return nil
}

func (t *track) carries(c webrtc.RTPCodecParameters) bool {
	if t.kind == webrtc.RTPCodecTypeAudio {
		return strings.EqualFold(c.MimeType, webrtc.MimeTypeOpus)
	}
	if !strings.EqualFold(c.MimeType, webrtc.MimeTypeH264) {
		return false
	}
	_, _, ok := t.table.pts(profileKey(c.SDPFmtpLine))
	return ok
}

func (t *track) info() TrackInfo {
	i := TrackInfo{Kind: t.kind, RID: t.rid, Layer: t.layer}
	if t.tr != nil {
		i.MID = t.tr.Mid()
	}
	t.mu.Lock()
	i.SSRC = t.stats.SSRC
	t.mu.Unlock()
	return i
}

// outKind is what one packet of an outFrame is.
type outKind uint8

const (
	outMedia   outKind = iota // a packet of the frame on the media stream
	outCopy                   // Options.RedundantRTX: the second copy of the frame's last packet, on the RTX stream
	outPadding                // Options.Padding: a padding-only packet on the media stream
)

// outPacket is one RTP packet of an outFrame.
type outPacket struct {
	kind    outKind
	hdr     rtp.Header // outCopy: only the sequence number of the packet to copy
	payload []byte
	size    int // its bytes on the wire, as far as the track knows them (the interceptors add the TWCC extension)
}

// outFrame is one Source packet as the RTP packets of its track. The send loop makes it (packetize); then the send
// loop or, for paced video, the pacer writes its packets in order (writePacket) and counts the frame (finish).
type outFrame struct {
	t        *track
	b        *binding // the binding when it was made: its packets carry its payload type and SSRC
	pkts     []outPacket
	media    int // pkts[:media] are the media packets, then come a copy and padding
	ts       uint32
	ntp      uint64
	capture  int64
	keyframe bool
	// What writing it did, filled in by its writer.
	errs           int
	copied, padded uint64
}

// packetize turns one Source packet into its RTP packets, with RTP timestamp tsBase + ticks and the sender-report NTP
// time of its capture: a video access unit through the H.264 payloader (the marker bit on its last packet), an Opus
// packet as it is. A keyframe that a waiting profile switch waits for switches it first, as an encoder's switch
// starts with a keyframe. It returns nil when the track isn't bound. Only the send loop calls it.
func (t *track) packetize(pkt media.Packet, ticks uint32, ntp uint64) *outFrame {
	t.mu.Lock()
	if t.next != "" && pkt.Keyframe && t.switchesAt(pkt) {
		t.switchLocked()
	}
	b := t.b
	t.mu.Unlock()
	if b == nil {
		return nil
	}
	if t.midExt == nil && t.tr != nil {
		if mid := t.tr.Mid(); mid != "" {
			t.midExt = []byte(mid)
		}
	}
	if t.ridExt == nil && t.rid != "" {
		t.ridExt = []byte(t.rid)
	}
	ts := t.tsBase + ticks
	video := t.kind == webrtc.RTPCodecTypeVideo
	payloads := [][]byte{pkt.Data}
	if video {
		payloads = t.pay.Payload(PayloadMTU, pkt.Data)
	}
	f := &outFrame{
		t: t, b: b, pkts: make([]outPacket, 0, len(payloads)+1), ts: ts, ntp: ntp, capture: pkt.CaptureNS,
		keyframe: pkt.Keyframe,
	}
	for i, payload := range payloads {
		hdr := rtp.Header{
			Version:        2,
			Marker:         video && i == len(payloads)-1,
			PayloadType:    b.pt,
			SequenceNumber: t.seq,
			Timestamp:      ts,
			SSRC:           b.ssrc,
		}
		t.seq++
		t.identify(&hdr, b)
		f.pkts = append(f.pkts, outPacket{hdr: hdr, payload: payload, size: hdr.MarshalSize() + len(payload)})
	}
	f.media = len(f.pkts)
	if video && len(payloads) > 0 {
		t.frames++
		// The second copy of the frame's last packet, on the RTX stream (Options.RedundantRTX).
		if t.repair != nil && b.rtxSSRC != 0 && t.frames > redundantAfter {
			last := f.pkts[f.media-1]
			f.pkts = append(f.pkts, outPacket{
				kind: outCopy, hdr: rtp.Header{SequenceNumber: last.hdr.SequenceNumber}, size: last.size + 2,
			})
		}
		if t.padEvery > 0 && t.frames%t.padEvery == 0 {
			t.pad(f)
		}
	}
	return f
}

// writePacket writes packet i of f. Only f's writer calls it: the send loop, or the pacer for paced video.
func (t *track) writePacket(f *outFrame, i int) {
	p := &f.pkts[i]
	switch p.kind {
	case outCopy:
		if t.resend(f.b, p.hdr.SequenceNumber) {
			f.copied++
		}
	case outPadding:
		if _, err := f.b.w.WriteRTP(&p.hdr, nil); err == nil {
			f.padded++
		}
	default:
		if t.repair != nil {
			t.repair.keep(p.hdr.SequenceNumber, p.hdr.Timestamp, p.hdr.Marker, p.payload)
		}
		if _, err := f.b.w.WriteRTP(&p.hdr, p.payload); err != nil {
			f.errs++
		}
	}
}

// writeFrame writes all of f's packets at once and counts the frame (the send loop, for audio and unpaced video).
func (t *track) writeFrame(f *outFrame, clock Clock) {
	for i := range f.pkts {
		t.writePacket(f, i)
	}
	t.finish(f, time.Now(), clock)
}

// finish counts a frame whose last packet was written at now: the counters, the frame for the next sender report,
// its wire bytes for Bitrate and FPS, and, for video with a Clock, its capture-to-send latency.
func (t *track) finish(f *outFrame, now time.Time, clock Clock) {
	var octets, wire uint64
	for _, p := range f.pkts[:f.media] {
		octets += uint64(len(p.payload))
		wire += uint64(p.size)
	}
	video := t.kind == webrtc.RTPCodecTypeVideo
	var age int64
	if video && clock != nil {
		age = clock.Now() - f.capture
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sent, t.lastTS, t.lastNTP = true, f.ts, f.ntp
	t.stats.Packets += uint64(f.media)
	t.stats.Octets += octets
	t.stats.WriteErrors += f.errs
	t.stats.Padding += f.padded
	t.stats.Redundant += f.copied
	t.stats.Frames++
	if f.keyframe {
		t.stats.Keyframes++
	}
	t.rate.add(now, int64(wire))
	if video && clock != nil {
		t.latency.add(now, age)
	}
}

// identify adds the mid and rid header extensions to a packet of the track's media stream, when they are
// negotiated. Only the send loop calls it.
func (t *track) identify(hdr *rtp.Header, b *binding) {
	if b.midID != 0 && t.midExt != nil {
		_ = hdr.SetExtension(b.midID, t.midExt)
	}
	if b.ridID != 0 && t.ridExt != nil {
		_ = hdr.SetExtension(b.ridID, t.ridExt)
	}
}

// pad adds padPackets padding-only packets to a frame (Options.Padding): the next sequence numbers of the media
// stream, the frame's timestamp, no payload, padSize bytes of padding. Only the send loop calls it.
func (t *track) pad(f *outFrame) {
	for range padPackets {
		hdr := rtp.Header{
			Version: 2, Padding: true, PaddingSize: padSize, PayloadType: f.b.pt, SequenceNumber: t.seq, Timestamp: f.ts,
			SSRC: f.b.ssrc,
		}
		t.seq++
		t.identify(&hdr, f.b)
		f.pkts = append(f.pkts, outPacket{kind: outPadding, hdr: hdr, size: hdr.MarshalSize() + padSize})
	}
}

// admit reports whether a Source packet of the track goes out (Publisher.SetActive): not while the video layer is
// off, and after it was turned on again not before its first keyframe, as an encoder that starts a layer again
// starts it with one. A packet that doesn't go out is counted in TrackStats.Paused. Only the send loop calls it.
func (t *track) admit(pkt media.Packet) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case t.inactive, t.resuming && !pkt.Keyframe:
		t.stats.Paused++
		return false
	}
	t.resuming = false
	return true
}

// setActive turns the video layer on or off, and reports whether it was off and is on now.
func (t *track) setActive(active bool) (resumed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	resumed = active && t.inactive
	t.inactive = !active
	if resumed {
		t.resuming = true
	}
	return resumed
}

// isActive reports whether SetActive left the video layer on.
func (t *track) isActive() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.inactive
}

// senderReport returns the SR for the last frame sent: NTP = wall clock at its capture, RTP = its timestamp, and the
// packet and octet counts (RFC 3550 §6.4.1). ok is false before the first packet, while unbound, and while the video
// layer is off (Publisher.SetActive): an encoding that sends nothing reports nothing.
func (t *track) senderReport() (*rtcp.SenderReport, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.sent || t.b == nil || t.inactive {
		return nil, false
	}
	return &rtcp.SenderReport{
		SSRC:        t.b.ssrc,
		NTPTime:     t.lastNTP,
		RTPTime:     t.lastTS,
		PacketCount: uint32(t.stats.Packets),
		OctetCount:  uint32(t.stats.Octets),
	}, true
}

// wroteSR counts a sender report written at at and remembers it for the RTT (onReceptionReport).
func (t *track) wroteSR(sr *rtcp.SenderReport, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stats.SRs++
	t.noteSRLocked(sr.NTPTime, at)
}

// onRTCP counts one RTCP packet addressed to this track, takes the RTT from a reception report about it, and reports
// whether the Source must make a keyframe for layer (PLI or FIR on a video track).
func (t *track) onRTCP(pkt rtcp.Packet) (layer string, keyframe bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ssrc := t.stats.SSRC
	video := t.kind == webrtc.RTPCodecTypeVideo
	switch pkt := pkt.(type) {
	case *rtcp.PictureLossIndication:
		if pkt.MediaSSRC == ssrc && video {
			t.stats.PLIs++
			return t.layer, true
		}
	case *rtcp.FullIntraRequest:
		for _, e := range pkt.FIR {
			if e.SSRC == ssrc && video {
				t.stats.FIRs++
				return t.layer, true
			}
		}
	case *rtcp.TransportLayerNack:
		if pkt.MediaSSRC == ssrc {
			t.stats.NACKs++
		}
	case *rtcp.ReceiverEstimatedMaximumBitrate:
		if slices.Contains(pkt.SSRCs, ssrc) {
			t.stats.REMBs++
			t.stats.LastREMB = uint64(pkt.Bitrate)
		}
	case *rtcp.ReceiverReport:
		t.onReceptionReports(pkt.Reports, ssrc)
	case *rtcp.SenderReport:
		t.onReceptionReports(pkt.Reports, ssrc)
	}
	return "", false
}

// onReceptionReports takes the RTT from the reports about ssrc, arrived now. t.mu is held.
func (t *track) onReceptionReports(reports []rtcp.ReceptionReport, ssrc uint32) {
	now := time.Now()
	for _, r := range reports {
		if r.SSRC == ssrc {
			t.onReceptionReport(r, now)
		}
	}
}

// rtpTicks converts a capture-time difference in ns to clock ticks, rounded to the nearest tick, modulo 2^32.
func rtpTicks(d int64, clock uint32) uint32 {
	neg := d < 0
	u := uint64(d)
	if neg {
		u = -u
	}
	hi, lo := bits.Mul64(u, uint64(clock))
	lo, carry := bits.Add64(lo, 5e8, 0)
	hi += carry
	if hi >= 1e9 { // the quotient doesn't fit 64 bits: centuries of media time
		hi %= 1e9
	}
	q, _ := bits.Div64(hi, lo, 1e9)
	if neg {
		return uint32(-q)
	}
	return uint32(q)
}

// ntpEpochOffset is the number of seconds from 1900-01-01 (NTP) to 1970-01-01 (Unix).
const ntpEpochOffset = 2208988800

// ntpTime converts a wall-clock time to the 64-bit NTP format of sender reports (32.32 fixed point since 1900).
func ntpTime(t time.Time) uint64 {
	secs := uint64(t.Unix() + ntpEpochOffset)
	frac := uint64(t.Nanosecond()) << 32 / uint64(time.Second)
	return secs<<32 | frac
}
