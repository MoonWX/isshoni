package sfuplane

import (
	"math"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/sfu"
)

// The "Values" rows of 01 §15.4. Each enum maps by name, and the SFU's own String methods give the names
// (02 §6.1), so the tables check the mapping against what the SFU calls its values, not against its numbers.

func TestRoles(t *testing.T) {
	for _, r := range []protocol.Role{protocol.RoleFull, protocol.RoleViewer, protocol.RolePublisher, protocol.RoleAgent} {
		got, ok := sfuRole(r)
		if !ok || got.String() != string(r) {
			t.Errorf("sfuRole(%q) = %v (%q), %v", r, got, got, ok)
		}
	}
	for _, r := range []protocol.Role{"", "admin", "FULL"} {
		if got, ok := sfuRole(r); ok {
			t.Errorf("sfuRole(%q) = %v, want no mapping", r, got)
		}
	}
}

func TestClientKinds(t *testing.T) {
	for _, tc := range []struct {
		kind protocol.ClientKind
		want sfu.ClientKind
	}{
		{protocol.ClientKindWeb, sfu.ClientWeb},
		{protocol.ClientKindDesktop, sfu.ClientDesktop},
		{protocol.ClientKindMobile, sfu.ClientMobile},
		// The SFU has no kind for these: the hub treats them like web (01 §8.2).
		{protocol.ClientKindTool, sfu.ClientWeb},
		{"kiosk", sfu.ClientWeb},
		{"", sfu.ClientWeb},
	} {
		got := sfuClientKind(tc.kind)
		if got != tc.want {
			t.Errorf("sfuClientKind(%q) = %v, want %v", tc.kind, got, tc.want)
		}
		if tc.kind.Valid() && tc.kind != protocol.ClientKindTool && got.String() != string(tc.kind) {
			t.Errorf("sfuClientKind(%q) is the SFU's %q", tc.kind, got)
		}
	}
}

func TestPCKinds(t *testing.T) {
	for _, k := range []protocol.PCKind{protocol.PCKindPub, protocol.PCKindSub} {
		got, ok := sfuPCKind(k)
		if !ok || got.String() != string(k) {
			t.Errorf("sfuPCKind(%q) = %v (%q), %v", k, got, got, ok)
		}
		back, ok := wirePCKind(got)
		if !ok || back != k {
			t.Errorf("wirePCKind(%v) = %q, %v; want %q", got, back, ok, k)
		}
	}
	if got, ok := sfuPCKind("data"); ok {
		t.Errorf("sfuPCKind(data) = %v, want no mapping", got)
	}
	for _, k := range []sfu.PCKind{0, sfu.PCSub + 1} {
		if got, ok := wirePCKind(k); ok {
			t.Errorf("wirePCKind(%d) = %q, want no mapping", k, got)
		}
	}
}

func TestVideoLayers(t *testing.T) {
	for _, l := range []protocol.VideoLayer{protocol.VideoLayerHigh, protocol.VideoLayerLow, protocol.VideoLayerOff} {
		got, ok := sfuQuality(l)
		if !ok || got.String() != string(l) {
			t.Errorf("sfuQuality(%q) = %v (%q), %v", l, got, got, ok)
		}
		back, ok := wireLayer(got)
		if !ok || back != l {
			t.Errorf("wireLayer(%v) = %q, %v; want %q", got, back, ok, l)
		}
	}
	if got, ok := sfuQuality("mid"); ok {
		t.Errorf("sfuQuality(mid) = %v, want no mapping (M5)", got)
	}
	if got, ok := wireLayer(sfu.QualityHigh + 1); ok {
		t.Errorf("wireLayer(unknown) = %q, want no mapping", got)
	}
	// The order that "forwarded less than requested" is decided by, taken from the names.
	if qualityRank(sfu.QualityOff) >= qualityRank(sfu.QualityLow) || qualityRank(sfu.QualityLow) >= qualityRank(sfu.QualityHigh) {
		t.Error("qualityRank does not order off < low < high")
	}
}

func TestRIDs(t *testing.T) {
	for _, tc := range []struct {
		rid  string
		want protocol.VideoLayer
		ok   bool
	}{
		{protocol.RIDHigh, protocol.VideoLayerHigh, true},
		{protocol.RIDLow, protocol.VideoLayerLow, true},
		{"f", protocol.VideoLayerHigh, true},
		{"q", protocol.VideoLayerLow, true},
		{"h", "", false}, // M5
		{"", "", false},
	} {
		if got, ok := layerOfRID(tc.rid); got != tc.want || ok != tc.ok {
			t.Errorf("layerOfRID(%q) = %q, %v; want %q, %v", tc.rid, got, ok, tc.want, tc.ok)
		}
	}
}

func TestAudioStates(t *testing.T) {
	if wireAudio(true) != protocol.AudioStateOn || wireAudio(false) != protocol.AudioStateOff {
		t.Errorf("wireAudio = %q, %q", wireAudio(true), wireAudio(false))
	}
}

func TestPresets(t *testing.T) {
	for _, p := range []protocol.Preset{protocol.PresetAuto, protocol.PresetGame, protocol.PresetMovie, protocol.PresetText} {
		got, ok := sfuPreset(p)
		if !ok || got.String() != string(p) {
			t.Errorf("sfuPreset(%q) = %v (%q), %v", p, got, got, ok)
		}
	}
	for _, p := range []protocol.Preset{"", "cinema"} {
		if got, ok := sfuPreset(p); ok {
			t.Errorf("sfuPreset(%q) = %v, want no mapping", p, got)
		}
	}
}

func TestShareKinds(t *testing.T) {
	for _, k := range []protocol.ShareKind{protocol.ShareKindScreen, protocol.ShareKindWindow, protocol.ShareKindTab} {
		got, ok := sfuSource(k)
		if !ok || got.String() != string(k) {
			t.Errorf("sfuSource(%q) = %v (%q), %v", k, got, got, ok)
		}
	}
	for _, k := range []protocol.ShareKind{"", "camera"} {
		if got, ok := sfuSource(k); ok {
			t.Errorf("sfuSource(%q) = %v, want no mapping", k, got)
		}
	}
}

func TestTrackKinds(t *testing.T) {
	for _, k := range []protocol.TrackKind{protocol.TrackKindVideo, protocol.TrackKindAudio} {
		got, ok := sfuTrackKind(k)
		if !ok || got.String() != string(k) {
			t.Errorf("sfuTrackKind(%q) = %v, %v", k, got, ok)
		}
		back, ok := wireTrackKind(got)
		if !ok || back != k {
			t.Errorf("wireTrackKind(%v) = %q, %v; want %q", got, back, ok, k)
		}
	}
	if got, ok := sfuTrackKind("data"); ok {
		t.Errorf("sfuTrackKind(data) = %v, want no mapping", got)
	}
	if got, ok := wireTrackKind(webrtc.RTPCodecType(0)); ok {
		t.Errorf("wireTrackKind(unknown) = %q, want no mapping", got)
	}
}

// TestEndReasons: the end reasons are the same strings in both packages, so the adapter converts them as they are
// (01 §15.4). Every reason the SFU declares is one of the wire's, and the other way round.
func TestEndReasons(t *testing.T) {
	declared := sfuConstsOf(t, "EndReason")
	for _, v := range declared {
		if !protocol.EndReason(v).Valid() {
			t.Errorf("the SFU's end reason %q is not one of the wire's", v)
		}
	}
	for _, r := range []protocol.EndReason{
		protocol.EndReasonStopped, protocol.EndReasonLeft, protocol.EndReasonDisconnected,
		protocol.EndReasonMediaTimeout, protocol.EndReasonKicked, protocol.EndReasonRoomClosed,
		protocol.EndReasonServerShutdown,
	} {
		if !slices.Contains(declared, string(r)) {
			t.Errorf("the wire's end reason %q is not one of the SFU's", r)
		}
	}
	if sfu.EndReason(protocol.EndReasonLeft) != sfu.EndReasonLeft ||
		sfu.EndReason(protocol.EndReasonMediaTimeout) != sfu.EndReasonMediaTimeout {
		t.Error("an end reason changes when converted")
	}
}

// TestCodecKeys: a profile key is the CodecKey without its "h264/" prefix, in both directions, for every profile
// the SFU declares.
func TestCodecKeys(t *testing.T) {
	want := map[sfu.ProfileKey]protocol.CodecKey{
		sfu.ProfileConstrainedBaseline: protocol.CodecH264ConstrainedBaseline,
		sfu.ProfileBaseline:            protocol.CodecH264Baseline,
		sfu.ProfileMain:                protocol.CodecH264Main,
		sfu.ProfileConstrainedHigh:     protocol.CodecH264ConstrainedHigh,
		sfu.ProfileHigh:                protocol.CodecH264High,
	}
	declared := sfuConstsOf(t, "ProfileKey")
	if len(declared) != len(want) {
		t.Errorf("the SFU declares the profiles %v, the table has %d", declared, len(want))
	}
	for _, v := range declared {
		p := sfu.ProfileKey(v)
		key, ok := codecKey(p)
		if !ok || key != want[p] || key != protocol.CodecKey("h264/"+v) || !key.IsH264() {
			t.Errorf("codecKey(%q) = %q, %v; want %q", p, key, ok, want[p])
		}
		caps := decodeCaps(protocol.Caps{Decode: []protocol.CodecKey{key}})
		if !slices.Equal(caps.H264, []sfu.ProfileKey{p}) {
			t.Errorf("decodeCaps(%q) = %v, want %q", key, caps.H264, p)
		}
	}
	if key, ok := codecKey(""); !ok || key != "" {
		t.Errorf("codecKey of an unknown profile = %q, %v; want an empty key", key, ok)
	}
	for _, p := range []sfu.ProfileKey{"64", "64001f", "zzzz", "h264/6400"} {
		if key, ok := codecKey(p); ok || key != "" {
			t.Errorf("codecKey(%q) = %q, %v; want no key", p, key, ok)
		}
	}
}

// TestDecodeCaps is the SetCaps row: "h264/6400" → "6400", and every key that isn't H.264 is dropped.
func TestDecodeCaps(t *testing.T) {
	for _, tc := range []struct {
		name   string
		decode []protocol.CodecKey
		want   []sfu.ProfileKey
	}{
		{"chrome", []protocol.CodecKey{"opus", "h264/42e0", "h264/4d00", "h264/6400"}, []sfu.ProfileKey{"42e0", "4d00", "6400"}},
		{"order kept", []protocol.CodecKey{"h264/6400", "h264/42e0"}, []sfu.ProfileKey{"6400", "42e0"}},
		{"no H.264 yet (fresh Firefox)", []protocol.CodecKey{"opus"}, nil},
		{"nothing", nil, nil},
		{"other codecs and malformed keys", []protocol.CodecKey{"vp8", "av1/0", "h264/", "h264/64", "h264/6400ff", "H264/6400", "h264/64AB", "h264/640c"}, []sfu.ProfileKey{"640c"}},
	} {
		got := decodeCaps(protocol.Caps{Decode: tc.decode, Encode: []protocol.CodecKey{"h264/4200"}})
		if !slices.Equal(got.H264, tc.want) {
			t.Errorf("%s: decodeCaps = %v, want %v", tc.name, got.H264, tc.want)
		}
	}
}

// TestStatusReasons is the "Subscription reasons" table of 01 §15.4, for every reason the SFU declares.
func TestStatusReasons(t *testing.T) {
	want := map[sfu.SubReason]protocol.StatusReason{
		sfu.SubReasonNone:               protocol.StatusReasonWaiting,
		sfu.SubReasonBandwidth:          protocol.StatusReasonBandwidth,
		sfu.SubReasonServerLimit:        protocol.StatusReasonBandwidth,
		sfu.SubReasonCodecMismatch:      protocol.StatusReasonCodec,
		sfu.SubReasonDecoderUnavailable: protocol.StatusReasonCodec,
		sfu.SubReasonDecoderFailed:      protocol.StatusReasonCodec,
		sfu.SubReasonNoPreviewLayer:     protocol.StatusReasonUnavailable,
		sfu.SubReasonNoLayer:            protocol.StatusReasonUnavailable,
	}
	declared := sfuConstsOf(t, "SubReason")
	if len(declared) != len(want) {
		t.Errorf("the SFU declares the reasons %q, the table has %d", declared, len(want))
	}
	for _, v := range declared {
		r := sfu.SubReason(v)
		wire, inTable := want[r]
		if !inTable {
			t.Errorf("the SFU's reason %q has no row", r)
			continue
		}
		// Less forwarded than requested: the reason's row.
		for _, q := range [][2]sfu.Quality{
			{sfu.QualityHigh, sfu.QualityLow}, {sfu.QualityHigh, sfu.QualityOff}, {sfu.QualityLow, sfu.QualityOff},
		} {
			ev := sfu.SubscriptionStateEvent{Share: "s_1", Requested: q[0], Forwarded: q[1], Reason: r}
			got, known := statusReason(ev)
			if got != wire || !known || !got.Valid() {
				t.Errorf("statusReason(%q, %v → %v) = %q, %v; want %q", r, q[0], q[1], got, known, wire)
			}
		}
		// What was asked for is forwarded: no reason on the wire.
		for _, q := range []sfu.Quality{sfu.QualityHigh, sfu.QualityLow, sfu.QualityOff} {
			ev := sfu.SubscriptionStateEvent{Share: "s_1", Requested: q, Forwarded: q, Reason: r}
			if got, known := statusReason(ev); got != "" || !known {
				t.Errorf("statusReason(%q, %v forwarded as asked) = %q, %v; want none", r, q, got, known)
			}
		}
	}
	ev := sfu.SubscriptionStateEvent{Requested: sfu.QualityHigh, Forwarded: sfu.QualityLow, Reason: "moon_phase"}
	if got, known := statusReason(ev); got != protocol.StatusReasonUnavailable || known {
		t.Errorf("statusReason of an unknown reason = %q, %v; want unavailable, flagged", got, known)
	}
}

// TestEncodings: encodings convert one to one, in the SFU's order, with the layer of each rid.
func TestEncodings(t *testing.T) {
	in := []sfu.EncodingParams{
		{RID: "f", Active: true, MaxBitrate: 8_000_000, MaxFramerate: 60, MaxPixels: 2_073_600},
		{RID: "h", Active: true, MaxBitrate: 1_500_000, MaxFramerate: 30, MaxPixels: 921_600}, // M5: no layer on the wire yet
		{RID: "q", Active: false, MaxBitrate: 300_000, MaxFramerate: 15, MaxPixels: 230_400},
	}
	want := []protocol.Encoding{
		{RID: "f", Layer: protocol.VideoLayerHigh, Active: true, MaxBitrate: 8_000_000, MaxFramerate: 60, MaxPixels: 2_073_600},
		{RID: "q", Layer: protocol.VideoLayerLow, Active: false, MaxBitrate: 300_000, MaxFramerate: 15, MaxPixels: 230_400},
	}
	if got := wireEncodings(in); !slices.Equal(got, want) {
		t.Errorf("wireEncodings = %+v\nwant %+v", got, want)
	}
	if got := wireEncodings(nil); got == nil || len(got) != 0 {
		t.Errorf("wireEncodings(nil) = %#v, want an empty list", got)
	}
}

// TestLayers: a share's layers are every attached layer, paused ones included (LayerInfo.Active is ignored).
func TestLayers(t *testing.T) {
	high, low := protocol.VideoLayerHigh, protocol.VideoLayerLow
	for _, tc := range []struct {
		name string
		in   []sfu.LayerInfo
		want []protocol.VideoLayer
	}{
		{"simulcast", []sfu.LayerInfo{{RID: "f", Active: true}, {RID: "q", Active: true}}, []protocol.VideoLayer{high, low}},
		{"paused layers count", []sfu.LayerInfo{{RID: "f"}, {RID: "q", Active: true}}, []protocol.VideoLayer{high, low}},
		{"one layer", []sfu.LayerInfo{{RID: "f", Active: true}}, []protocol.VideoLayer{high}},
		{"preview only", []sfu.LayerInfo{{RID: "q"}}, []protocol.VideoLayer{low}},
		{"a layer of M5", []sfu.LayerInfo{{RID: "f"}, {RID: "h"}, {RID: "q"}}, []protocol.VideoLayer{high, low}},
		{"none", nil, []protocol.VideoLayer{}},
	} {
		got := wireLayers(tc.in)
		if got == nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s: wireLayers = %#v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestTracks(t *testing.T) {
	in := []sfu.TrackBinding{
		{MID: "0", Share: "s_a", Kind: webrtc.RTPCodecTypeVideo},
		{MID: "1", Share: "s_a", Kind: webrtc.RTPCodecTypeAudio},
		{MID: "2", Share: "s_b", Kind: webrtc.RTPCodecType(0)},
	}
	want := []protocol.TrackRef{
		{MID: "0", ShareID: "s_a", Kind: protocol.TrackKindVideo},
		{MID: "1", ShareID: "s_a", Kind: protocol.TrackKindAudio},
	}
	if got := wireTracks(in); !slices.Equal(got, want) {
		t.Errorf("wireTracks = %+v, want %+v", got, want)
	}
	if got := wireTracks(nil); got == nil || len(got) != 0 {
		t.Errorf("wireTracks(nil) = %#v, want an empty list", got)
	}
}

// TestStats is the Stats row: a Conn's stats as the stats message of 01 §8.11.
func TestStats(t *testing.T) {
	in := sfu.ConnStats{
		PCs: []sfu.PCStats{{Kind: sfu.PCSub, Gen: 2, State: "connected", Transport: "udp"}}, // not on the wire
		Shares: []sfu.OwnShareStats{
			{
				Share: "s_mine", State: sfu.ShareLive, Audio: true, AudioBitrate: 127_000, AudioLossPct: 0.5,
				Layers: []sfu.LayerInfo{
					{RID: "f", Width: 1920, Height: 1080, FPS: 60, Bitrate: 7_400_000, LossPct: 0.1, Active: true},
					{RID: "q", Width: 640, Height: 360, FPS: 15, Bitrate: 280_000, Active: false},
				},
			},
			{Share: "s_silent", State: sfu.SharePending},
		},
		Subscriptions: []sfu.SubscriptionStats{
			{
				Share: "s_a", Requested: sfu.QualityHigh, Forwarded: sfu.QualityHigh, Layer: "f", Profile: "6400",
				AudioRequested: true, AudioForwarded: true,
				Video: sfu.DownTrackStats{Bitrate: 7_600_000, LossPct: 0.2, NACKsServed: 9, Drops: 3},
				Audio: sfu.DownTrackStats{Bitrate: 128_000, LossPct: 1.5, Drops: 1},
			},
			// Asked for high, capped: the preview layer is what flows.
			{
				Share: "s_b", Requested: sfu.QualityHigh, Forwarded: sfu.QualityLow, Reason: sfu.SubReasonBandwidth,
				Layer: "q", Video: sfu.DownTrackStats{Bitrate: 290_000},
			},
			// Nothing forwarded: no entry.
			{Share: "s_c", Requested: sfu.QualityLow, Forwarded: sfu.QualityOff, Reason: sfu.SubReasonNoPreviewLayer},
			// A layer without a name on the wire is listed by the forwarded quality.
			{Share: "s_d", Requested: sfu.QualityHigh, Forwarded: sfu.QualityHigh, Layer: "h", Video: sfu.DownTrackStats{Drops: math.MaxUint64}},
			// Audio only.
			{Share: "s_e", Requested: sfu.QualityOff, Forwarded: sfu.QualityOff, AudioRequested: true, AudioForwarded: true},
			// Audio that is asked for but not forwarded: no entry.
			{Share: "s_f", Requested: sfu.QualityOff, Forwarded: sfu.QualityOff, AudioRequested: true},
			// The rid names the layer, also where the forwarded quality says otherwise.
			{Share: "s_g", Requested: sfu.QualityHigh, Forwarded: sfu.QualityHigh, Layer: "q", Video: sfu.DownTrackStats{Bitrate: 1}},
		},
		DownlinkEstimate: 24_000_000,
	}
	video, audio := protocol.TrackKindVideo, protocol.TrackKindAudio
	want := protocol.ServerStats{
		DownlinkEstimate: 24_000_000,
		Subs: []protocol.ServerSubStats{
			{ShareID: "s_a", Kind: video, Layer: protocol.VideoLayerHigh, Bitrate: 7_600_000, LossPct: 0.2, Dropped: 3},
			{ShareID: "s_a", Kind: audio, Bitrate: 128_000, LossPct: 1.5, Dropped: 1},
			{ShareID: "s_b", Kind: video, Layer: protocol.VideoLayerLow, Bitrate: 290_000},
			{ShareID: "s_d", Kind: video, Layer: protocol.VideoLayerHigh, Dropped: math.MaxInt64},
			{ShareID: "s_e", Kind: audio},
			{ShareID: "s_g", Kind: video, Layer: protocol.VideoLayerLow, Bitrate: 1},
		},
		Layers: []protocol.ServerLayerStats{
			{ShareID: "s_mine", Kind: video, RID: "f", Bitrate: 7_400_000, LossPct: 0.1},
			{ShareID: "s_mine", Kind: video, RID: "q", Bitrate: 280_000},
			{ShareID: "s_mine", Kind: audio, Bitrate: 127_000, LossPct: 0.5},
		},
	}
	if got := wireStats(in); !reflect.DeepEqual(got, want) {
		t.Errorf("wireStats = %+v\nwant %+v", got, want)
	}
	// Nothing yet: the lists are empty, never nil.
	if got := wireStats(sfu.ConnStats{}); got.Subs == nil || got.Layers == nil || len(got.Subs)+len(got.Layers) != 0 {
		t.Errorf("wireStats of nothing = %#v, want empty lists", got)
	}
}

func TestRetryAfterMs(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want int
	}{
		{time.Second, 1000},
		{1500 * time.Millisecond, 1500},
		{time.Nanosecond, 1}, // rounded up
		{time.Millisecond + time.Nanosecond, 2},
		{0, 1},
	} {
		if got := retryAfterMs(tc.d); got != tc.want {
			t.Errorf("retryAfterMs(%v) = %d, want %d", tc.d, got, tc.want)
		}
	}
}
