package sfuplane

import (
	"math"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/sfu"
)

// This file holds the value mappings of 01 §15.4. Every enum maps by name: the SFU's numbers are its own and may
// change (02 §6.1). A function that maps a value the hub validates returns ok = false for anything else, and its
// caller refuses the call instead of passing a zero value on.

// sfuRole maps a connection's role (01 §6.3).
func sfuRole(r protocol.Role) (sfu.Role, bool) {
	switch r {
	case protocol.RoleFull:
		return sfu.RoleFull, true
	case protocol.RoleViewer:
		return sfu.RoleViewer, true
	case protocol.RolePublisher:
		return sfu.RolePublisher, true
	case protocol.RoleAgent:
		return sfu.RoleAgent, true
	}
	return 0, false
}

// sfuClientKind maps a client kind. The SFU knows web, desktop and mobile; tool (the load test and Go tests) and a
// kind this build doesn't know are web, as the hub treats them (01 §8.2).
func sfuClientKind(k protocol.ClientKind) sfu.ClientKind {
	switch k {
	case protocol.ClientKindDesktop:
		return sfu.ClientDesktop
	case protocol.ClientKindMobile:
		return sfu.ClientMobile
	}
	return sfu.ClientWeb
}

func sfuPCKind(k protocol.PCKind) (sfu.PCKind, bool) {
	switch k {
	case protocol.PCKindPub:
		return sfu.PCPub, true
	case protocol.PCKindSub:
		return sfu.PCSub, true
	}
	return 0, false
}

func wirePCKind(k sfu.PCKind) (protocol.PCKind, bool) {
	switch k {
	case sfu.PCPub:
		return protocol.PCKindPub, true
	case sfu.PCSub:
		return protocol.PCKindSub, true
	}
	return "", false
}

// sfuQuality maps a requested video layer: high is the focused share, low a thumbnail, off a hidden one.
func sfuQuality(l protocol.VideoLayer) (sfu.Quality, bool) {
	switch l {
	case protocol.VideoLayerHigh:
		return sfu.QualityHigh, true
	case protocol.VideoLayerLow:
		return sfu.QualityLow, true
	case protocol.VideoLayerOff:
		return sfu.QualityOff, true
	}
	return 0, false
}

func wireLayer(q sfu.Quality) (protocol.VideoLayer, bool) {
	switch q {
	case sfu.QualityHigh:
		return protocol.VideoLayerHigh, true
	case sfu.QualityLow:
		return protocol.VideoLayerLow, true
	case sfu.QualityOff:
		return protocol.VideoLayerOff, true
	}
	return "", false
}

// qualityRank orders the qualities by name, cheapest first: off, low, high.
func qualityRank(q sfu.Quality) int {
	switch q {
	case sfu.QualityLow:
		return 1
	case sfu.QualityHigh:
		return 2
	}
	return 0
}

// layerOfRID maps a simulcast rid to its layer: "f" is high and "q" is low (01 §20). Any other rid has no layer on
// the wire (the "h" layer is M5's).
func layerOfRID(rid string) (protocol.VideoLayer, bool) {
	switch rid {
	case protocol.RIDHigh:
		return protocol.VideoLayerHigh, true
	case protocol.RIDLow:
		return protocol.VideoLayerLow, true
	}
	return "", false
}

func wireAudio(on bool) protocol.AudioState {
	if on {
		return protocol.AudioStateOn
	}
	return protocol.AudioStateOff
}

func sfuPreset(p protocol.Preset) (sfu.Preset, bool) {
	switch p {
	case protocol.PresetAuto:
		return sfu.PresetAuto, true
	case protocol.PresetGame:
		return sfu.PresetGame, true
	case protocol.PresetMovie:
		return sfu.PresetMovie, true
	case protocol.PresetText:
		return sfu.PresetText, true
	}
	return 0, false
}

func sfuSource(k protocol.ShareKind) (sfu.SourceKind, bool) {
	switch k {
	case protocol.ShareKindScreen:
		return sfu.SourceScreen, true
	case protocol.ShareKindWindow:
		return sfu.SourceWindow, true
	case protocol.ShareKindTab:
		return sfu.SourceTab, true
	}
	return 0, false
}

func sfuTrackKind(k protocol.TrackKind) (webrtc.RTPCodecType, bool) {
	switch k {
	case protocol.TrackKindVideo:
		return webrtc.RTPCodecTypeVideo, true
	case protocol.TrackKindAudio:
		return webrtc.RTPCodecTypeAudio, true
	}
	return 0, false
}

func wireTrackKind(k webrtc.RTPCodecType) (protocol.TrackKind, bool) {
	switch k {
	case webrtc.RTPCodecTypeVideo:
		return protocol.TrackKindVideo, true
	case webrtc.RTPCodecTypeAudio:
		return protocol.TrackKindAudio, true
	}
	return "", false
}

// codecKey returns the wire's CodecKey of an H.264 profile key: "6400" is "h264/6400". An empty profile (a share
// whose profile isn't known yet) is an empty key; ok is false for a profile that isn't 4 hex digits.
func codecKey(p sfu.ProfileKey) (key protocol.CodecKey, ok bool) {
	if p == "" {
		return "", true
	}
	key = protocol.H264CodecKey(string(p))
	return key, key != ""
}

// decodeCaps returns the H.264 profiles of a client's decode capabilities, without the "h264/" prefix. Other keys
// (opus, codecs this build doesn't know, malformed ones) are left out: the SFU forwards only H.264 video.
func decodeCaps(c protocol.Caps) sfu.DecodeCaps {
	var out sfu.DecodeCaps
	for _, k := range c.Decode {
		if p := k.H264Profile(); p != "" {
			out.H264 = append(out.H264, sfu.ProfileKey(p))
		}
	}
	return out
}

// statusReason maps why a subscription gets less video than it asked for to the wire's four reasons. No reason while
// less is forwarded than requested means that the sub PC isn't connected yet or the layer's keyframe hasn't arrived:
// waiting. A subscription that gets what it asked for has no reason on the wire, whatever the SFU says. known is
// false for a reason this package has no mapping for, which goes out as unavailable.
func statusReason(ev sfu.SubscriptionStateEvent) (reason protocol.StatusReason, known bool) {
	if qualityRank(ev.Forwarded) >= qualityRank(ev.Requested) {
		return "", true
	}
	switch ev.Reason {
	case sfu.SubReasonNone:
		return protocol.StatusReasonWaiting, true
	case sfu.SubReasonBandwidth, sfu.SubReasonServerLimit:
		return protocol.StatusReasonBandwidth, true
	case sfu.SubReasonCodecMismatch, sfu.SubReasonDecoderUnavailable, sfu.SubReasonDecoderFailed:
		return protocol.StatusReasonCodec, true
	case sfu.SubReasonNoPreviewLayer, sfu.SubReasonNoLayer:
		return protocol.StatusReasonUnavailable, true
	}
	return protocol.StatusReasonUnavailable, false
}

// wireEncodings converts encodings one to one, in the SFU's order (f, then q), with each layer taken from its rid.
// An encoding whose rid has no layer on the wire is left out; the caller sees that from the lengths.
func wireEncodings(in []sfu.EncodingParams) []protocol.Encoding {
	out := make([]protocol.Encoding, 0, len(in))
	for _, e := range in {
		layer, ok := layerOfRID(e.RID)
		if !ok {
			continue
		}
		out = append(out, protocol.Encoding{
			RID:          e.RID,
			Layer:        layer,
			Active:       e.Active,
			MaxBitrate:   int64(e.MaxBitrate),
			MaxFramerate: e.MaxFramerate,
			MaxPixels:    e.MaxPixels,
		})
	}
	return out
}

// wireLayers returns the layers of a share that the SFU receives: every layer whose track is attached, paused ones
// included, so LayerInfo.Active is ignored (01 §15.4). The SFU sorts them f, then q, which is high, then low.
func wireLayers(in []sfu.LayerInfo) []protocol.VideoLayer {
	out := make([]protocol.VideoLayer, 0, len(in))
	for _, l := range in {
		if layer, ok := layerOfRID(l.RID); ok {
			out = append(out, layer)
		}
	}
	return out
}

// wireTracks converts a sub offer's bindings to TrackRefs, leaving out a binding whose kind is neither video nor
// audio; the caller sees that from the lengths.
func wireTracks(in []sfu.TrackBinding) []protocol.TrackRef {
	out := make([]protocol.TrackRef, 0, len(in))
	for _, b := range in {
		if kind, ok := wireTrackKind(b.Kind); ok {
			out = append(out, protocol.TrackRef{MID: b.MID, ShareID: string(b.Share), Kind: kind})
		}
	}
	return out
}

// wireStats converts a Conn's stats to the stats message (01 §8.11). Subs lists the tracks that are forwarded now:
// for each subscription its video, with the layer of the rid it carries (or of the forwarded quality, for a rid
// without a layer on the wire), and its audio. Layers lists what the SFU receives of the connection's own shares:
// each video layer and the audio.
func wireStats(st sfu.ConnStats) protocol.ServerStats {
	out := protocol.ServerStats{
		DownlinkEstimate: st.DownlinkEstimate,
		Subs:             []protocol.ServerSubStats{},
		Layers:           []protocol.ServerLayerStats{},
	}
	for _, s := range st.Subscriptions {
		if layer, ok := forwardedLayer(s); ok {
			out.Subs = append(out.Subs, protocol.ServerSubStats{
				ShareID: string(s.Share), Kind: protocol.TrackKindVideo, Layer: layer,
				Bitrate: int64(s.Video.Bitrate), LossPct: s.Video.LossPct, Dropped: clampInt64(s.Video.Drops),
			})
		}
		if s.AudioForwarded {
			out.Subs = append(out.Subs, protocol.ServerSubStats{
				ShareID: string(s.Share), Kind: protocol.TrackKindAudio,
				Bitrate: int64(s.Audio.Bitrate), LossPct: s.Audio.LossPct, Dropped: clampInt64(s.Audio.Drops),
			})
		}
	}
	for _, sh := range st.Shares {
		for _, l := range sh.Layers {
			out.Layers = append(out.Layers, protocol.ServerLayerStats{
				ShareID: string(sh.Share), Kind: protocol.TrackKindVideo, RID: l.RID,
				Bitrate: int64(l.Bitrate), LossPct: l.LossPct,
			})
		}
		if sh.Audio {
			out.Layers = append(out.Layers, protocol.ServerLayerStats{
				ShareID: string(sh.Share), Kind: protocol.TrackKindAudio,
				Bitrate: int64(sh.AudioBitrate), LossPct: sh.AudioLossPct,
			})
		}
	}
	return out
}

// forwardedLayer returns the layer of the video that a subscription forwards now; ok is false when it forwards none.
func forwardedLayer(s sfu.SubscriptionStats) (protocol.VideoLayer, bool) {
	if s.Forwarded == sfu.QualityOff {
		return "", false
	}
	if layer, ok := layerOfRID(s.Layer); ok {
		return layer, true
	}
	return wireLayer(s.Forwarded)
}

// clampInt64 converts a counter to the wire's int64.
func clampInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}
