package publish

import (
	"math/bits"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/media/fake"
)

// track is the Publisher's webrtc.TrackLocal: one simulcast encoding, or the audio. Pion binds it when the answer is
// applied; the send loop writes to it, stamping the mid and rid header extensions itself.
type track struct {
	kind           webrtc.RTPCodecType
	id, stream     string
	rid            string // the encoding's rid; "" for audio and for a single layer
	layer          string // the Source layer; "" for audio
	profile        string // H.264 profile key (video)
	clock          uint32
	tr             *webrtc.RTPTransceiver // set once in addTracks, before any Bind
	seq            uint16                 // send loop only, like the fields below
	tsBase         uint32
	pay            codecs.H264Payloader
	midExt, ridExt []byte

	mu      sync.Mutex // guards the fields below
	b       *binding
	sent    bool
	lastTS  uint32
	lastNTP uint64
	stats   TrackStats
}

// binding is what Bind learned from the negotiation.
type binding struct {
	w            webrtc.TrackLocalWriter
	ssrc         uint32
	pt           uint8
	midID, ridID uint8 // header extension IDs; 0 when not negotiated
}

func newTrack(kind webrtc.RTPCodecType, id, stream, rid, layer, profile string) *track {
	t := &track{kind: kind, id: id, stream: stream, rid: rid, layer: layer, profile: profile, clock: videoClock}
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

// Bind implements webrtc.TrackLocal: it picks the negotiated codec that carries what the Source sends (the H.264
// profile with packetization-mode=1, or Opus) and the mid and rid header extension IDs.
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
			}
		}
		t.mu.Lock()
		t.b = b
		t.stats.SSRC = b.ssrc
		t.mu.Unlock()
		return c, nil
	}
	return webrtc.RTPCodecParameters{}, webrtc.ErrUnsupportedCodec
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
	return strings.EqualFold(c.MimeType, webrtc.MimeTypeH264) && profileKey(c.SDPFmtpLine) == t.profile
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

// write packetizes and writes one Source packet with RTP timestamp tsBase + ticks and the sender-report NTP time of
// its capture. It returns false when the track isn't bound. Only the send loop calls it.
func (t *track) write(pkt fake.Packet, ticks uint32, ntp uint64) bool {
	t.mu.Lock()
	b := t.b
	t.mu.Unlock()
	if b == nil {
		return false
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
	payloads := [][]byte{pkt.Data}
	if t.kind == webrtc.RTPCodecTypeVideo {
		payloads = t.pay.Payload(PayloadMTU, pkt.Data)
	}
	var octets uint64
	errs := 0
	for i, payload := range payloads {
		hdr := rtp.Header{
			Version:        2,
			Marker:         t.kind == webrtc.RTPCodecTypeVideo && i == len(payloads)-1,
			PayloadType:    b.pt,
			SequenceNumber: t.seq,
			Timestamp:      ts,
			SSRC:           b.ssrc,
		}
		t.seq++
		if b.midID != 0 && t.midExt != nil {
			_ = hdr.SetExtension(b.midID, t.midExt)
		}
		if b.ridID != 0 && t.ridExt != nil {
			_ = hdr.SetExtension(b.ridID, t.ridExt)
		}
		if _, err := b.w.WriteRTP(&hdr, payload); err != nil {
			errs++
		}
		octets += uint64(len(payload))
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sent, t.lastTS, t.lastNTP = true, ts, ntp
	t.stats.Packets += uint64(len(payloads))
	t.stats.Octets += octets
	t.stats.WriteErrors += errs
	t.stats.Frames++
	if pkt.Keyframe {
		t.stats.Keyframes++
	}
	return true
}

// senderReport returns the SR for the last frame sent: NTP = wall clock at its capture, RTP = its timestamp, and the
// packet and octet counts (RFC 3550 §6.4.1). ok is false before the first packet or while unbound.
func (t *track) senderReport() (*rtcp.SenderReport, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.sent || t.b == nil {
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

// onRTCP counts one RTCP packet addressed to this track and reports whether the Source must make a keyframe for
// layer (PLI or FIR on a video track).
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
		for _, s := range pkt.SSRCs {
			if s == ssrc {
				t.stats.LastREMB = uint64(pkt.Bitrate)
				break
			}
		}
	}
	return "", false
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
