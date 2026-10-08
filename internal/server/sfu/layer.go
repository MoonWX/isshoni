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
// The munger and the packet cache use slot, cache and lastSR (README S12). The core slice (S29) adds where the Layer
// comes from: its share, track, receiver and pub PC; a pubTrack (pubpc.go) reads the track and feeds the Layer it is
// attached to. The media path (S41) adds the rest of 02 §9.1: what the read loops do with a packet, keyframe
// throttling, stats and SPS info. The fields are set before the Layer is attached and never change.
type Layer struct {
	slot   Slot
	cache  *packetCache
	lastSR atomic.Pointer[srInfo] // the latest sender report for this layer's SSRC; nil until the first one

	share *Share
	rid   string // "f" | "q" for video (a track without a rid is "f"), "" for audio
	kind  webrtc.RTPCodecType
	track *webrtc.TrackRemote
	recv  *webrtc.RTPReceiver
	pubPC *webrtc.PeerConnection // for PLI and REMB (02 §9.7, §11)
	ssrc  uint32
}

// newLayer returns a Layer for a slot with an empty packet cache sized for its kind.
func newLayer(slot Slot, kind webrtc.RTPCodecType) *Layer {
	return &Layer{slot: slot, cache: newPacketCache(kind), kind: kind}
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
// packet cache and every DownTrack queue. The media path (README S41) adds the header fields the DownTrack writer
// needs (marker, PT).
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
