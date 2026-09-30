package sfu

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/interceptor/pkg/report"
	"github.com/pion/interceptor/pkg/twcc"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

func TestParseH264Fmtp(t *testing.T) {
	cases := []struct {
		fmtp  string
		key   ProfileKey
		mode1 bool
	}{
		{"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f", "42e0", true},
		{"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640034", "6400", true}, // Chrome's High
		{"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640c1f", "640c", true},
		{"packetization-mode=1;profile-level-id=4d0032", "4d00", true},
		{"PROFILE-LEVEL-ID=42E01F;Packetization-Mode=1", "42e0", true}, // case
		// Pion trims each item as a whole, not the name or the value on their own (TestOfferedProfilesMatchPion).
		{" packetization-mode=1 ; profile-level-id=64001F ", "6400", true},
		{"profile-level-id=64001F; packetization-mode = 1 ", "6400", false},
		{"packetization-mode =1;profile-level-id=64001f", "6400", false},
		{" packetization-mode=1 ; profile-level-id= 42001f", "", true},
		{"packetization-mode= 1;profile-level-id=42001f", "4200", false},
		{"profile-level-id=42e01f", "42e0", false},                      // packetization-mode missing: mode 0
		{"packetization-mode=0;profile-level-id=42e01f", "42e0", false}, // mode 0
		{"packetization-mode=2;profile-level-id=42e01f", "42e0", false}, // mode 2 isn't supported
		{"packetization-mode=01;profile-level-id=42e01f", "42e0", false},
		{"level-asymmetry-allowed=1;packetization-mode=1", "", true}, // profile-level-id missing
		{"packetization-mode=1;profile-level-id=42e", "", true},      // too short
		{"packetization-mode=1;profile-level-id=zz001f", "", true},   // not hex
		{"packetization-mode=1;profile-level-id=4-e01f", "", true},   // not hex
		{"packetization-mode=1;profile-level-id=42e0zz", "", true},   // Pion decodes the whole value
		{"packetization-mode=1;profile-level-id=42e01", "", true},    // odd length
		{"packetization-mode=1;profile-level-id=42e0", "42e0", true}, // no level: still a key
		{"packetization-mode=1;profile-level-id=42e01f00", "42e0", true},
		{"packetization-mode=1;profile-level-id", "", true}, // no value
		// A repeated name keeps its last value, as in Pion.
		{"packetization-mode=1;profile-level-id=42e01f;packetization-mode=0", "42e0", false},
		{"packetization-mode=1;profile-level-id=zz;profile-level-id=64001f", "6400", true},
		{"packetization-mode=1;profile-level-id=64001f;profile-level-id=zz", "", true},
		{"", "", false},
		{";;;", "", false},
		{"apt=96", "", false},
	}
	for _, c := range cases {
		key, mode1 := parseH264Fmtp(c.fmtp)
		if key != c.key || mode1 != c.mode1 {
			t.Errorf("parseH264Fmtp(%q) = %q, %v; want %q, %v", c.fmtp, key, mode1, c.key, c.mode1)
		}
	}
}

func h264Params(pt webrtc.PayloadType, fmtp string) webrtc.RTPCodecParameters {
	return webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: fmtp},
		PayloadType:        pt,
	}
}

func rtxParams(pt, apt webrtc.PayloadType) webrtc.RTPCodecParameters {
	return webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeRTX, ClockRate: 90000, SDPFmtpLine: "apt=" + strconv.Itoa(int(apt)),
		},
		PayloadType: pt,
	}
}

func h264(pt webrtc.PayloadType, plid string) webrtc.RTPCodecParameters {
	return h264Params(pt, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id="+plid)
}

func TestCodecProfile(t *testing.T) {
	cases := []struct {
		c    webrtc.RTPCodecParameters
		key  ProfileKey
		isOK bool
	}{
		{h264(96, "42e01f"), "42e0", true},
		{h264Params(96, "packetization-mode=1;profile-level-id=640c34"), "640c", true},
		{func() webrtc.RTPCodecParameters { c := h264(96, "42e01f"); c.MimeType = "video/h264"; return c }(), "42e0", true},
		{h264Params(97, "packetization-mode=0;profile-level-id=42e01f"), "", false},
		{h264Params(98, "packetization-mode=1"), "", false},
		{rtxParams(97, 96), "", false},
		{webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}}, "", false},
	}
	for i, c := range cases {
		key, ok := codecProfile(c.c)
		if key != c.key || ok != c.isOK {
			t.Errorf("case %d (%s %q): got %q, %v; want %q, %v", i, c.c.MimeType, c.c.SDPFmtpLine, key, ok, c.key, c.isOK)
		}
	}
}

// TestCompatibilityMatrix checks every cell of 02 §8.2. Rows: what the viewer advertises; columns: the stream.
func TestCompatibilityMatrix(t *testing.T) {
	cols := []ProfileKey{"42e0", "4200", "4d00", "640c", "6400"}
	matrix := []struct {
		viewer ProfileKey
		cells  string // one mark per column: y = can receive, - = can't
	}{
		{"42e0", "yy---"},
		{"4200", "yy---"},
		{"4d00", "yyy--"},
		{"640c", "yyyyy"},
		{"6400", "yyyyy"},
	}
	for _, row := range matrix {
		for i, stream := range cols {
			want := row.cells[i] == 'y'
			if got := compatible(row.viewer, stream); got != want {
				t.Errorf("compatible(viewer %s, stream %s) = %v, want %v", row.viewer, stream, got, want)
			}
		}
	}
	if !compatible("f400", "f400") {
		t.Error("an exact match must be compatible even for a profile outside the table")
	}
	for _, k := range cols {
		if compatible("f400", k) || compatible(k, "f400") || compatible("", k) || compatible(k, "") {
			t.Errorf("unknown or empty keys must not match %s", k)
		}
	}
	if compatible("", "") {
		t.Error("empty keys must not match")
	}
	if got := slices.Sorted(maps.Keys(decodable)); !slices.Equal(got, slices.Sorted(slices.Values(profileRank[:]))) {
		t.Errorf("matrix rows %v and profileRank %v differ", got, profileRank)
	}
	for _, k := range profileRank {
		if !k.known() {
			t.Errorf("%s not known", k)
		}
	}
	if ProfileKey("f400").known() || ProfileKey("").known() {
		t.Error("unknown keys reported as known")
	}
}

func TestBestPT(t *testing.T) {
	// Chrome 154's publish offer PTs (testdata/sdp/chrome154-pub-offer.sdp), plus packetization-mode 0 entries.
	chrome := []webrtc.RTPCodecParameters{
		h264(118, "64001f"), rtxParams(119, 118),
		h264(102, "42001f"), rtxParams(103, 102),
		h264(108, "42e01f"), rtxParams(109, 108),
		h264(116, "4d001f"), rtxParams(117, 116),
	}
	pm0 := h264Params(99, "level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=640c1f")
	cases := []struct {
		name       string
		stream     ProfileKey
		negotiated []webrtc.RTPCodecParameters
		pt         webrtc.PayloadType
		ok         bool
	}{
		{"exact High", "6400", chrome, 118, true},
		{"exact CB", "42e0", chrome, 108, true},
		{"exact Main", "4d00", chrome, 116, true},
		{"exact first among equals", "42e0", []webrtc.RTPCodecParameters{h264(96, "42e01f"), h264(127, "42e034")}, 96, true},
		{"exact before compatible", "42e0", []webrtc.RTPCodecParameters{h264(104, "64001f"), h264(96, "42e01f")}, 96, true},
		{"CHigh stream on a High viewer", "640c", chrome, 118, true},
		{"CB stream, High preferred over Baseline", "42e0", []webrtc.RTPCodecParameters{h264(98, "42001f"), h264(104, "64001f")}, 104, true},
		{"CB stream, CHigh preferred over Main", "42e0", []webrtc.RTPCodecParameters{h264(100, "4d001f"), h264(102, "640c1f")}, 102, true},
		{"CB stream, Main preferred over Baseline", "42e0", []webrtc.RTPCodecParameters{h264(98, "42001f"), h264(100, "4d001f")}, 100, true},
		{"Baseline stream on a CB-only viewer", "4200", []webrtc.RTPCodecParameters{h264(96, "42e01f")}, 96, true},
		{"High stream, CB-only viewer (Firefox)", "6400", []webrtc.RTPCodecParameters{h264(126, "42e01f"), h264(97, "42001f")}, 0, false},
		{"Main stream, CB-only viewer", "4d00", []webrtc.RTPCodecParameters{h264(126, "42e01f")}, 0, false},
		{"packetization-mode 0 ignored", "640c", []webrtc.RTPCodecParameters{pm0, h264(96, "42e01f")}, 0, false},
		{"RTX ignored", "6400", []webrtc.RTPCodecParameters{rtxParams(105, 104)}, 0, false},
		{"no codecs", "6400", nil, 0, false},
		{"unknown stream profile", "f400", chrome, 0, false},
		{"empty stream profile", "", chrome, 0, false},
	}
	for _, c := range cases {
		pt, ok := bestPT(c.stream, c.negotiated)
		if pt != c.pt || ok != c.ok {
			t.Errorf("%s: bestPT(%s) = %d, %v; want %d, %v", c.name, c.stream, pt, ok, c.pt, c.ok)
		}
	}
}

func TestRTXPayloadTypes(t *testing.T) {
	negotiated := []webrtc.RTPCodecParameters{
		h264(118, "64001f"), rtxParams(119, 118),
		h264(108, "42e01f"), rtxParams(109, 108),
		rtxParams(120, 118), // a second RTX for 118: the first one wins
		rtxParams(121, 55),  // apt names a PT that isn't negotiated
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeRTX, SDPFmtpLine: "apt=x"}, PayloadType: 122},
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeRTX, SDPFmtpLine: "apt=300"}, PayloadType: 123},
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeRTX}, PayloadType: 124},
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: "video/RTX", SDPFmtpLine: " APT = 116"}, PayloadType: 117},
		h264(116, "4d001f"),
	}
	want := map[webrtc.PayloadType]webrtc.PayloadType{118: 119, 108: 109, 116: 117}
	if got := rtxPayloadTypes(negotiated); !maps.Equal(got, want) {
		t.Errorf("rtxPayloadTypes = %v, want %v", got, want)
	}
	if got := rtxPayloadTypes(nil); len(got) != 0 {
		t.Errorf("rtxPayloadTypes(nil) = %v", got)
	}
}

func TestPolicyFor(t *testing.T) {
	cases := []struct {
		name    string
		viewers [][]ProfileKey
		want    ProfileKey
	}{
		{"no viewers", nil, ProfileHigh},
		{"Chrome viewers", [][]ProfileKey{{"4200", "42e0", "4d00", "6400"}, {"42e0", "6400"}}, ProfileHigh},
		{"CHigh counts as high", [][]ProfileKey{{"42e0", "640c"}}, ProfileHigh},
		{"a CB-only viewer (Firefox)", [][]ProfileKey{{"42e0", "6400"}, {"42e0", "4200"}}, ProfileConstrainedBaseline},
		{"Main is not enough", [][]ProfileKey{{"4d00"}}, ProfileConstrainedBaseline},
		{"viewers without H.264 don't count", [][]ProfileKey{{}, {"6400"}, nil}, ProfileHigh},
		{"only viewers without H.264", [][]ProfileKey{{}, nil}, ProfileHigh},
		{"unknown keys only", [][]ProfileKey{{"f400"}}, ProfileConstrainedBaseline},
	}
	for _, c := range cases {
		if got := policyFor(c.viewers); got != c.want {
			t.Errorf("%s: policyFor = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestPolicyAllows(t *testing.T) {
	for _, k := range profileRank {
		if !policyAllows(ProfileHigh, k) {
			t.Errorf("high must allow %s", k)
		}
		want := k == "42e0" || k == "4200"
		if got := policyAllows(ProfileConstrainedBaseline, k); got != want {
			t.Errorf("cb allows %s = %v, want %v", k, got, want)
		}
	}
	if policyAllows(ProfileHigh, "f400") || policyAllows(ProfileHigh, "") || policyAllows(ProfileConstrainedBaseline, "") {
		t.Error("unknown profiles must never be allowed")
	}
}

func TestFilterByPolicy(t *testing.T) {
	vp8 := webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}, PayloadType: 96,
	}
	offer := []webrtc.RTPCodecParameters{
		vp8, rtxParams(97, 96),
		h264(118, "64001f"), rtxParams(119, 118),
		h264(102, "42001f"), rtxParams(103, 102),
		h264Params(106, "level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42e01f"), rtxParams(107, 106),
		h264(108, "42e01f"), rtxParams(109, 108),
		h264(116, "4d001f"), rtxParams(117, 116),
		h264(39, "640c1f"),
		rtxParams(40, 39),
	}
	pts := func(cs []webrtc.RTPCodecParameters) []webrtc.PayloadType {
		out := make([]webrtc.PayloadType, len(cs))
		for i, c := range cs {
			out[i] = c.PayloadType
		}
		return out
	}
	if got, want := pts(filterByPolicy(offer, ProfileHigh)), []webrtc.PayloadType{118, 119, 102, 103, 108, 109, 116, 117, 39, 40}; !slices.Equal(got, want) {
		t.Errorf("high keeps %v, want %v", got, want)
	}
	if got, want := pts(filterByPolicy(offer, ProfileConstrainedBaseline)), []webrtc.PayloadType{102, 103, 108, 109}; !slices.Equal(got, want) {
		t.Errorf("cb keeps %v, want %v", got, want)
	}
	if got := filterByPolicy([]webrtc.RTPCodecParameters{vp8}, ProfileHigh); len(got) != 0 {
		t.Errorf("no H.264: got %v", got)
	}
}

// ---- MediaEngines (02 §8.1) ----

// spec81 is 02 §8.1's payload-type table, written out independently of codec.go.
var spec81 = []struct {
	pt, rtx int
	plid    string
}{
	{96, 97, "42e01f"},
	{98, 99, "42001f"},
	{100, 101, "4d001f"},
	{102, 103, "640c1f"},
	{104, 105, "64001f"},
}

// engineWant is one column of 02 §8.1's feedback and header-extension table.
type engineWant struct {
	videoFB, audioFB   []string
	opusFmtp           string
	videoExt, audioExt []string
}

var (
	pubWant = engineWant{
		videoFB:  []string{"goog-remb", "ccm fir", "nack", "nack pli", "transport-cc"},
		audioFB:  []string{"nack", "transport-cc"},
		opusFmtp: "minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1;maxaveragebitrate=128000",
		videoExt: []string{
			"urn:ietf:params:rtp-hdrext:sdes:mid",
			"urn:ietf:params:rtp-hdrext:sdes:rtp-stream-id",
			"urn:ietf:params:rtp-hdrext:sdes:repaired-rtp-stream-id",
			"http://www.ietf.org/id/draft-holmer-rmcat-transport-wide-cc-extensions-01",
		},
		audioExt: []string{"http://www.ietf.org/id/draft-holmer-rmcat-transport-wide-cc-extensions-01"},
	}
	subWant = engineWant{
		videoFB:  []string{"goog-remb", "ccm fir", "nack", "nack pli"},
		audioFB:  []string{"nack"},
		opusFmtp: "minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1;maxaveragebitrate=510000",
		videoExt: []string{"http://www.webrtc.org/experiments/rtp-hdrext/abs-send-time"},
	}
)

// testAPI builds a webrtc.API on an engine with a hermetic SettingEngine: no mDNS (no multicast socket, no macOS
// Local Network prompt) and no interfaces, so creating offers and answers never touches the network.
func testAPI(m *webrtc.MediaEngine, ir *interceptor.Registry) *webrtc.API {
	var se webrtc.SettingEngine
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetInterfaceFilter(func(string) bool { return false })
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se))
}

func newTestPC(t *testing.T, api *webrtc.API) *webrtc.PeerConnection {
	t.Helper()
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pc.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return pc
}

// engineOffer returns an offer with one video and one audio transceiver from an engine.
func engineOffer(t *testing.T, build func() (*webrtc.MediaEngine, *interceptor.Registry, error),
	dir webrtc.RTPTransceiverDirection) string {
	t.Helper()
	m, ir, err := build()
	if err != nil {
		t.Fatal(err)
	}
	pc := newTestPC(t, testAPI(m, ir))
	for _, kind := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
		if _, err := pc.AddTransceiverFromKind(kind, webrtc.RTPTransceiverInit{Direction: dir}); err != nil {
			t.Fatal(err)
		}
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	return offer.SDP
}

// fragmentKeys are the attributes sdpFragments keeps: the deterministic, codec-related ones.
var fragmentKeys = map[string]bool{
	"mid": true, "extmap": true, "sendrecv": true, "sendonly": true, "recvonly": true, "inactive": true,
	"rtpmap": true, "fmtp": true, "rtcp-fb": true, "rid": true, "simulcast": true, "rtcp-mux": true,
}

// sdpFragments reduces an SDP to its m-lines and codec-related attributes (fragmentKeys), in a canonical order:
// formats sorted by PT, then the other kept attributes as written, then each PT's rtpmap, fmtp and rtcp-fb lines by
// PT, then extmap lines by ID. Pion writes extmaps and an answer's RTX codecs in map order, so the written order
// isn't stable; the order that matters is checked by the tests themselves. ICE credentials, fingerprints, msids and
// SSRCs are left out.
func sdpFragments(t *testing.T, raw string) string {
	t.Helper()
	var desc sdp.SessionDescription
	if err := desc.UnmarshalString(raw); err != nil {
		t.Fatal(err)
	}
	num := func(s string) int {
		n, _ := strconv.Atoi(strings.Fields(s + " x")[0])
		return n
	}
	var b strings.Builder
	for _, md := range desc.MediaDescriptions {
		formats := slices.Clone(md.MediaName.Formats)
		slices.SortStableFunc(formats, func(x, y string) int { return num(x) - num(y) })
		fmt.Fprintf(&b, "m=%s %s %s %s\n", md.MediaName.Media, md.MediaName.Port.String(),
			strings.Join(md.MediaName.Protos, "/"), strings.Join(formats, " "))
		var codec, ext []sdp.Attribute
		for _, a := range md.Attributes {
			switch {
			case a.Key == "extmap":
				ext = append(ext, a)
			case a.Key == "rtpmap" || a.Key == "fmtp" || a.Key == "rtcp-fb":
				codec = append(codec, a)
			case fragmentKeys[a.Key]:
				b.WriteString(a.String() + "\n")
			}
		}
		rank := map[string]int{"rtpmap": 0, "fmtp": 1, "rtcp-fb": 2}
		slices.SortStableFunc(codec, func(x, y sdp.Attribute) int {
			if d := num(x.Value) - num(y.Value); d != 0 {
				return d
			}
			return rank[x.Key] - rank[y.Key]
		})
		slices.SortStableFunc(ext, func(x, y sdp.Attribute) int { return num(x.Value) - num(y.Value) })
		for _, a := range slices.Concat(codec, ext) {
			b.WriteString(a.String() + "\n")
		}
	}
	return b.String()
}

// attrValues returns the values of every attribute key of an m-section that start with prefix, prefix removed.
func attrValues(md *sdp.MediaDescription, key, prefix string) []string {
	var out []string
	for _, a := range md.Attributes {
		if a.Key == key && strings.HasPrefix(a.Value, prefix) {
			out = append(out, strings.TrimPrefix(a.Value, prefix))
		}
	}
	return out
}

// extURIs returns the header extension URIs of an m-section, sorted.
func extURIs(md *sdp.MediaDescription) []string {
	var out []string
	for _, v := range attrValues(md, "extmap", "") {
		f := strings.Fields(v)
		if len(f) >= 2 {
			out = append(out, f[1])
		}
	}
	slices.Sort(out)
	return out
}

// checkSpec81 checks an engine's offer against 02 §8.1: PTs and their order, rtpmap, fmtp, feedback and header
// extensions per kind.
func checkSpec81(t *testing.T, raw string, want engineWant) {
	t.Helper()
	var desc sdp.SessionDescription
	if err := desc.UnmarshalString(raw); err != nil {
		t.Fatal(err)
	}
	if len(desc.MediaDescriptions) != 2 {
		t.Fatalf("want 2 m-sections, got %d", len(desc.MediaDescriptions))
	}
	video, audio := desc.MediaDescriptions[0], desc.MediaDescriptions[1]

	var wantFormats []string
	for _, r := range spec81 {
		wantFormats = append(wantFormats, strconv.Itoa(r.pt), strconv.Itoa(r.rtx))
	}
	if !slices.Equal(video.MediaName.Formats, wantFormats) {
		t.Errorf("video PTs %v, want %v", video.MediaName.Formats, wantFormats)
	}
	for _, r := range spec81 {
		pt, rtx := strconv.Itoa(r.pt)+" ", strconv.Itoa(r.rtx)+" "
		checkAttr(t, video, "rtpmap", pt, []string{"H264/90000"})
		checkAttr(t, video, "fmtp", pt, []string{"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=" + r.plid})
		checkAttr(t, video, "rtcp-fb", pt, want.videoFB)
		checkAttr(t, video, "rtpmap", rtx, []string{"rtx/90000"})
		checkAttr(t, video, "fmtp", rtx, []string{"apt=" + strconv.Itoa(r.pt)})
		checkAttr(t, video, "rtcp-fb", rtx, nil)
	}
	if !slices.Equal(audio.MediaName.Formats, []string{"111"}) {
		t.Errorf("audio PTs %v, want [111]", audio.MediaName.Formats)
	}
	checkAttr(t, audio, "rtpmap", "111 ", []string{"opus/48000/2"})
	checkAttr(t, audio, "fmtp", "111 ", []string{want.opusFmtp})
	checkAttr(t, audio, "rtcp-fb", "111 ", want.audioFB)

	if got, w := extURIs(video), slices.Sorted(slices.Values(want.videoExt)); !slices.Equal(got, w) {
		t.Errorf("video header extensions %v, want %v", got, w)
	}
	if got, w := extURIs(audio), slices.Sorted(slices.Values(want.audioExt)); !slices.Equal(got, w) {
		t.Errorf("audio header extensions %v, want %v", got, w)
	}
}

func checkAttr(t *testing.T, md *sdp.MediaDescription, key, prefix string, want []string) {
	t.Helper()
	if got := attrValues(md, key, prefix); !slices.Equal(got, want) {
		t.Errorf("%s a=%s:%s= %q, want %q", md.MediaName.Media, key, prefix, got, want)
	}
}

func TestPubEngineGolden(t *testing.T) {
	offer := engineOffer(t, newPubEngine, webrtc.RTPTransceiverDirectionRecvonly)
	checkSpec81(t, offer, pubWant)
	golden(t, "pub-engine-offer.sdp", sdpFragments(t, offer))
}

func TestSubEngineGolden(t *testing.T) {
	offer := engineOffer(t, newSubEngine, webrtc.RTPTransceiverDirectionSendonly)
	checkSpec81(t, offer, subWant)
	if strings.Contains(offer, "transport-cc") || strings.Contains(offer, "transport-wide-cc") {
		t.Error("the sub engine must not offer transport-cc (02 §8.1)")
	}
	golden(t, "sub-engine-offer.sdp", sdpFragments(t, offer))
}

// TestPubEngineAnswersChromeOffer answers a real Chrome 154 simulcast offer (testdata/sdp/chrome154-pub-offer.sdp,
// captured from headless Chrome on macOS: video with rids f and q and High first, plus Opus) with the pub engine.
// Pion keeps the offerer's PTs, matches H.264 profiles by fmtp, and keeps only the extensions and feedback of §8.1.
func TestPubEngineAnswersChromeOffer(t *testing.T) {
	m, ir, err := newPubEngine()
	if err != nil {
		t.Fatal(err)
	}
	pc := newTestPC(t, testAPI(m, ir))
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer,
		SDP: readSDP(t, "sdp/chrome154-pub-offer.sdp")}); err != nil {
		t.Fatal(err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	var desc sdp.SessionDescription
	if err := desc.UnmarshalString(answer.SDP); err != nil {
		t.Fatal(err)
	}
	video, audio := desc.MediaDescriptions[0], desc.MediaDescriptions[1]
	if got, want := slices.Sorted(slices.Values(video.MediaName.Formats)),
		[]string{"102", "103", "108", "109", "116", "117", "118", "119"}; !slices.Equal(got, want) {
		t.Errorf("answer video PTs %v, want Chrome's %v", got, want)
	}
	var h264Order []string
	for _, f := range video.MediaName.Formats {
		if slices.Contains(attrValues(video, "rtpmap", f+" "), "H264/90000") {
			h264Order = append(h264Order, f)
		}
	}
	if want := []string{"118", "102", "108", "116"}; !slices.Equal(h264Order, want) {
		t.Errorf("answer H.264 order %v, want the offer's %v", h264Order, want)
	}
	checkAttr(t, video, "fmtp", "118 ", []string{"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=64001f"})
	// Feedback is the intersection, in the offer's order.
	if got, want := slices.Sorted(slices.Values(attrValues(video, "rtcp-fb", "118 "))),
		slices.Sorted(slices.Values(pubWant.videoFB)); !slices.Equal(got, want) {
		t.Errorf("answer feedback for 118 %q, want %q", got, want)
	}
	checkAttr(t, video, "fmtp", "119 ", []string{"apt=118"})
	if got := attrValues(video, "simulcast", ""); !slices.Equal(got, []string{"recv f;q"}) {
		t.Errorf("simulcast %q, want recv f;q", got)
	}
	if got := attrValues(video, "rid", ""); !slices.Equal(got, []string{"f recv", "q recv"}) {
		t.Errorf("rids %q", got)
	}
	if got, want := extURIs(video), slices.Sorted(slices.Values(pubWant.videoExt)); !slices.Equal(got, want) {
		t.Errorf("video header extensions %v, want %v", got, want)
	}
	if got := attrValues(video, "extmap", "9 "); !slices.Equal(got, []string{"urn:ietf:params:rtp-hdrext:sdes:mid"}) {
		t.Errorf("the answer must keep Chrome's extension IDs: mid = %q", got)
	}
	if !slices.Equal(audio.MediaName.Formats, []string{"111"}) {
		t.Errorf("answer audio PTs %v, want [111]", audio.MediaName.Formats)
	}
	if got, want := extURIs(audio), pubWant.audioExt; !slices.Equal(got, want) {
		t.Errorf("audio header extensions %v, want %v", got, want)
	}
	golden(t, "pub-answer-chrome154.sdp", sdpFragments(t, answer.SDP))
}

func TestInterceptors(t *testing.T) {
	factories, err := pubInterceptors()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range factories {
		got = append(got, reflect.TypeOf(f).String())
	}
	want := []string{
		reflect.TypeFor[*nack.GeneratorInterceptorFactory]().String(),
		reflect.TypeFor[*report.ReceiverInterceptorFactory]().String(),
		reflect.TypeFor[*twcc.SenderInterceptorFactory]().String(),
	}
	if !slices.Equal(got, want) {
		t.Errorf("pub interceptors %v, want %v (no NACK responder, SR generator or stats)", got, want)
	}
	_, subIR, err := newSubEngine()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(subIR, &interceptor.Registry{}) {
		t.Error("the sub registry must be empty (02 §8.1)")
	}
}

func TestEngineTablesMatch(t *testing.T) {
	if len(h264Codecs) != len(spec81) {
		t.Fatalf("h264Codecs has %d rows, 02 §8.1 has %d", len(h264Codecs), len(spec81))
	}
	for i, r := range spec81 {
		c := h264Codecs[i]
		if int(c.pt) != r.pt || int(c.rtx) != r.rtx || c.profileLevelID != r.plid {
			t.Errorf("row %d = %+v, want %+v", i, c, r)
		}
		if got := profileKeyOf(c.profileLevelID); !got.known() {
			t.Errorf("row %d profile %s is not in the matrix", i, got)
		}
	}
}
