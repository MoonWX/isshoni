package sfu

import (
	"errors"
	"strings"
	"sync/atomic"

	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

// DownTrack is one subscriber's video or audio of one share: the webrtc.TrackLocal behind a sendonly transceiver of
// the subscriber's sub PC (02 §9.3). On the wire its msid stream id is the share id and its track id "v-" or "a-"
// plus the share id; viewers map it by the sub offer's tracks binding, the msid is a debugging aid (02 §5.2).
//
// The core slice (README S29) gives it its place in the model and its binding to the PC. The media path (S41 and
// later) adds the rest of 02 §9.3: the queues, the writer and its munger, the RTCP reader, the interest mask and the
// requested and capped quality.
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

func newDownTrack(sub *Subscription, kind webrtc.RTPCodecType) *DownTrack {
	return &DownTrack{share: sub.share, sub: sub, kind: kind}
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
// without a decoder doesn't fail the whole negotiation (02 §8.5).
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
		// takes; the writer picks the PT of each packet from ptFor (README S41).
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
	return *chosen, nil
}

// Unbind implements webrtc.TrackLocal: the sender stopped (the subscription went away, or the PC closed).
func (d *DownTrack) Unbind(webrtc.TrackLocalContext) error {
	d.binding.Store(nil)
	return nil
}
