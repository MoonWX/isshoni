package sfu

import (
	"sync/atomic"

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

// Layer is one incoming RTP stream of a share: a simulcast layer or the audio track (02 §9.1). A publisher rebuild
// attaches new Layer instances to the same Share, so munger epochs compare Layers by pointer, never by rid.
//
// This slice (S12) declares what the munger and the packet cache use. The core slice (S29) adds the rest of 02 §9.1:
// the share, the track, receiver and pub PC, the RTP and RTCP read loops, keyframe throttling, stats and SPS info.
type Layer struct {
	slot   Slot
	cache  *packetCache
	lastSR atomic.Pointer[srInfo] // the latest sender report for this layer's SSRC; nil until the first one
}

// newLayer returns a Layer for a slot with an empty packet cache sized for its kind.
func newLayer(slot Slot, kind webrtc.RTPCodecType) *Layer {
	return &Layer{slot: slot, cache: newPacketCache(kind)}
}

// srInfo is a sender report of a Layer's SSRC as the SFU keeps it (02 §9.6).
type srInfo struct {
	ntp     uint64 // NTP timestamp, 32.32 fixed point
	rtp     uint32 // RTP timestamp at that NTP time
	arrival int64  // monotonic ns
}

// packet is one incoming RTP packet (02 §9.1). It is immutable after the Layer's RTP loop builds it, and shared by the
// packet cache and every DownTrack queue. The core slice (S29) adds the header fields the writer needs (marker, PT).
type packet struct {
	layer    *Layer
	seq      uint16
	ts       uint32
	profile  ProfileKey // video: from the PT via the pub PC's negotiated codecs; "" for audio
	keyStart bool       // video: carries an SPS (isKeyframeStart); audio: always true
	padding  bool       // no payload (probe padding): never cached, used only to close sequence gaps
	payload  []byte     // exact-size copy, header extensions removed
	arrival  int64      // monotonic ns
}
