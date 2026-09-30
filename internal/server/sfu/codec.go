package sfu

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/interceptor/pkg/report"
	"github.com/pion/interceptor/pkg/twcc"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

// ProfileKey names an H.264 profile the way SDP negotiation compares it: the first 4 hex digits of profile-level-id
// (profile_idc and the constraint-flags byte), lowercased. The level is ignored (S4 finding 5; Pion's own H.264 fmtp
// match compares the same two bytes). It is 01's CodecKey without the "h264/" prefix.
type ProfileKey string

// The five profiles both MediaEngines register (02 §8.1). ProfileHigh and ProfileConstrainedBaseline are also the two
// values of a room's codec policy ("high" and "cb", 02 §8.3).
const (
	ProfileConstrainedBaseline ProfileKey = "42e0"
	ProfileBaseline            ProfileKey = "4200"
	ProfileMain                ProfileKey = "4d00"
	ProfileConstrainedHigh     ProfileKey = "640c"
	ProfileHigh                ProfileKey = "6400"
)

// profileRank is the order in which bestPT tries compatible (not exact) profiles, and the set of known profiles.
var profileRank = [...]ProfileKey{
	ProfileHigh, ProfileConstrainedHigh, ProfileMain, ProfileConstrainedBaseline, ProfileBaseline,
}

// decodable is the compatibility matrix of 02 §8.2: a viewer that advertises the key can receive a stream in each
// listed profile. Baseline streams from WebRTC encoders use no FMO, ASO or redundant slices, so they are
// Constrained Baseline in practice; WebRTC encoders never emit B-frames, so High and Main streams decode as
// Constrained High. Every compatible cell must pass the real-decoder e2e checks (05) before a release; a failing
// cell is removed here.
var decodable = map[ProfileKey][]ProfileKey{
	ProfileConstrainedBaseline: {ProfileConstrainedBaseline, ProfileBaseline},
	ProfileBaseline:            {ProfileConstrainedBaseline, ProfileBaseline},
	ProfileMain:                {ProfileConstrainedBaseline, ProfileBaseline, ProfileMain},
	ProfileConstrainedHigh: {
		ProfileConstrainedBaseline, ProfileBaseline, ProfileMain, ProfileConstrainedHigh, ProfileHigh,
	},
	ProfileHigh: {ProfileConstrainedBaseline, ProfileBaseline, ProfileMain, ProfileConstrainedHigh, ProfileHigh},
}

// known reports whether k is one of the five profiles the SFU registers.
func (k ProfileKey) known() bool {
	_, ok := decodable[k]
	return ok
}

// compatible reports whether a viewer that advertises viewer can decode a stream in profile stream. An exact match
// is always compatible.
func compatible(viewer, stream ProfileKey) bool {
	if viewer == stream && viewer != "" {
		return true
	}
	for _, k := range decodable[viewer] {
		if k == stream {
			return true
		}
	}
	return false
}

// parseH264Fmtp returns the ProfileKey of an H.264 fmtp line and whether it asks for packetization-mode=1. It reads
// the line the way Pion's H.264 fmtp matcher does (webrtc/v4 internal/fmtp), so that what counts as H.264 mode 1 here
// is what Pion negotiates: each ";" item is trimmed and cut at its first "=", the name is lowercased, the value is
// taken as it is, and a repeated name keeps its last value. mode1 needs the value "1" exactly. The key is "" when
// profile-level-id is missing or its value isn't hex of even length with at least 4 digits (profileKeyOf). RFC 6184
// would infer Baseline level 1.0 for a missing profile-level-id, but Pion never matches such a codec, so it can't be
// negotiated here.
func parseH264Fmtp(fmtp string) (key ProfileKey, mode1 bool) {
	for param := range strings.SplitSeq(fmtp, ";") {
		name, value, _ := strings.Cut(strings.TrimSpace(param), "=")
		switch strings.ToLower(name) {
		case "profile-level-id":
			key = profileKeyOf(value)
		case "packetization-mode":
			mode1 = value == "1"
		}
	}
	return key, mode1
}

// profileKeyOf returns the lowercased first 4 hex digits of a profile-level-id value, or "" unless the whole value
// is hex of even length with at least 4 digits: Pion hex-decodes the value and compares its first two bytes.
func profileKeyOf(plid string) ProfileKey {
	if len(plid) < 4 || len(plid)%2 != 0 {
		return ""
	}
	for i := range len(plid) {
		switch c := plid[i]; {
		case '0' <= c && c <= '9', 'a' <= c && c <= 'f', 'A' <= c && c <= 'F':
		default:
			return ""
		}
	}
	return ProfileKey(strings.ToLower(plid[:4]))
}

// codecProfile returns the ProfileKey of an H.264 codec with packetization-mode=1, and false for anything else
// (other codecs, RTX, packetization-mode 0, a missing or malformed profile-level-id).
func codecProfile(c webrtc.RTPCodecParameters) (ProfileKey, bool) {
	if !strings.EqualFold(c.MimeType, webrtc.MimeTypeH264) {
		return "", false
	}
	key, mode1 := parseH264Fmtp(c.SDPFmtpLine)
	if !mode1 || key == "" {
		return "", false
	}
	return key, true
}

// bestPT picks the payload type a viewer receives a stream in profile stream on, from the codecs negotiated on its
// transceiver (02 §8.2): an exact profile match first, then a compatible one in the order 6400, 640c, 4d00, 42e0,
// 4200. The first codec of the negotiated list wins among equals. ok is false when no negotiated codec can carry the
// stream (the DownTrack then reports codec_mismatch, 02 §8.3).
func bestPT(stream ProfileKey, negotiated []webrtc.RTPCodecParameters) (webrtc.PayloadType, bool) {
	if stream == "" {
		return 0, false
	}
	for _, c := range negotiated {
		if k, ok := codecProfile(c); ok && k == stream {
			return c.PayloadType, true
		}
	}
	for _, want := range profileRank {
		if !compatible(want, stream) {
			continue
		}
		for _, c := range negotiated {
			if k, ok := codecProfile(c); ok && k == want {
				return c.PayloadType, true
			}
		}
	}
	return 0, false
}

// rtxPayloadTypes maps each negotiated media PT to its RTX PT, from the RTX codecs' apt= parameter (02 §9.3,
// binding.rtxPTFor). A malformed apt, or one naming a PT that isn't negotiated, is skipped.
func rtxPayloadTypes(negotiated []webrtc.RTPCodecParameters) map[webrtc.PayloadType]webrtc.PayloadType {
	media := make(map[webrtc.PayloadType]bool, len(negotiated))
	for _, c := range negotiated {
		if !strings.EqualFold(c.MimeType, webrtc.MimeTypeRTX) {
			media[c.PayloadType] = true
		}
	}
	out := make(map[webrtc.PayloadType]webrtc.PayloadType)
	for _, c := range negotiated {
		if !strings.EqualFold(c.MimeType, webrtc.MimeTypeRTX) {
			continue
		}
		apt, ok := fmtpParam(c.SDPFmtpLine, "apt")
		if !ok {
			continue
		}
		pt, err := strconv.ParseUint(apt, 10, 7)
		if err != nil || !media[webrtc.PayloadType(pt)] {
			continue
		}
		if _, dup := out[webrtc.PayloadType(pt)]; !dup {
			out[webrtc.PayloadType(pt)] = c.PayloadType
		}
	}
	return out
}

// fmtpParam returns the value of one fmtp parameter (name matched without case).
func fmtpParam(fmtp, name string) (string, bool) {
	for param := range strings.SplitSeq(fmtp, ";") {
		k, v, _ := strings.Cut(strings.TrimSpace(param), "=")
		if strings.EqualFold(strings.TrimSpace(k), name) {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

// ---- codec policy helpers (02 §8.3–8.4; the room policy with its hysteresis lives in room.go) ----

// policyFor returns the codec policy for a room's viewer set: ProfileHigh when every viewer can receive High (its
// caps hold 6400 or 640c), otherwise ProfileConstrainedBaseline. Each element is one viewer's DecodeCaps.H264; a
// viewer without any H.264 is waiting for a decoder and doesn't count. No viewers means high.
func policyFor(viewers [][]ProfileKey) ProfileKey {
	for _, caps := range viewers {
		if len(caps) == 0 {
			continue
		}
		high := false
		for _, k := range caps {
			if compatible(k, ProfileHigh) {
				high = true
				break
			}
		}
		if !high {
			return ProfileConstrainedBaseline
		}
	}
	return ProfileHigh
}

// policyAllows reports whether a publisher may send profile under policy (02 §8.4 step 3): high allows all five
// profiles, cb only Constrained Baseline and Baseline.
func policyAllows(policy, profile ProfileKey) bool {
	if policy == ProfileHigh {
		return profile.known()
	}
	return profile == ProfileConstrainedBaseline || profile == ProfileBaseline
}

// filterByPolicy keeps, in their order, the H.264 codecs (packetization-mode=1) that policy allows plus the RTX
// codecs that repair them. Everything else is dropped. The pub PC passes the result to SetCodecPreferences on each
// video transceiver (02 §8.4 step 3).
func filterByPolicy(codecs []webrtc.RTPCodecParameters, policy ProfileKey) []webrtc.RTPCodecParameters {
	kept := make(map[webrtc.PayloadType]bool)
	for _, c := range codecs {
		if k, ok := codecProfile(c); ok && policyAllows(policy, k) {
			kept[c.PayloadType] = true
		}
	}
	out := make([]webrtc.RTPCodecParameters, 0, len(codecs))
	for _, c := range codecs {
		if kept[c.PayloadType] {
			out = append(out, c)
			continue
		}
		if !strings.EqualFold(c.MimeType, webrtc.MimeTypeRTX) {
			continue
		}
		if apt, ok := fmtpParam(c.SDPFmtpLine, "apt"); ok {
			if pt, err := strconv.ParseUint(apt, 10, 7); err == nil && kept[webrtc.PayloadType(pt)] {
				out = append(out, c)
			}
		}
	}
	return out
}

// ---- MediaEngines and interceptors (02 §8.1) ----

// h264Codec is one row of the static payload-type table. Both MediaEngines register the same PTs; Pion matches remote
// PTs by fmtp and answers with the offerer's, so a browser's own PT numbers work on the publish side.
type h264Codec struct {
	pt, rtx        webrtc.PayloadType
	profileLevelID string
}

// h264Codecs is the H.264 part of 02 §8.1, in registration (and offer) order.
var h264Codecs = [...]h264Codec{
	{96, 97, "42e01f"},   // Constrained Baseline
	{98, 99, "42001f"},   // Baseline
	{100, 101, "4d001f"}, // Main
	{102, 103, "640c1f"}, // Constrained High
	{104, 105, "64001f"}, // High
}

const (
	ptOpus webrtc.PayloadType = 111

	// h264FmtpPrefix is followed by the profile-level-id of each row.
	h264FmtpPrefix = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id="
	// opusFmtpBase is the Opus fmtp of both engines without maxaveragebitrate. It is what the sub offer carries. A pub
	// answer carries the offerer's Opus fmtp instead (Pion answers with the remote fmtp), so the copy sent to the
	// client gets stereo and its share's bitrate from setOpusAnswerParams (02 §8.4 step 5).
	opusFmtpBase = "minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1"
	opusFmtpPub  = opusFmtpBase + ";maxaveragebitrate=128000"
	opusFmtpSub  = opusFmtpBase + ";maxaveragebitrate=510000"

	// Publish-side interceptor settings (02 §8.1).
	pubNACKSize     = 2048
	pubNACKInterval = 50 * time.Millisecond
	pubRRInterval   = time.Second
)

// RTCP feedback per side and kind (02 §8.1). The subscribe side has no transport-cc in v1: downlink adaptation uses
// REMB and loss.
var (
	fbGoogREMB     = webrtc.RTCPFeedback{Type: webrtc.TypeRTCPFBGoogREMB}
	fbCCMFIR       = webrtc.RTCPFeedback{Type: webrtc.TypeRTCPFBCCM, Parameter: "fir"}
	fbNACK         = webrtc.RTCPFeedback{Type: webrtc.TypeRTCPFBNACK}
	fbNACKPLI      = webrtc.RTCPFeedback{Type: webrtc.TypeRTCPFBNACK, Parameter: "pli"}
	fbTransportCC  = webrtc.RTCPFeedback{Type: webrtc.TypeRTCPFBTransportCC}
	pubVideoFB     = []webrtc.RTCPFeedback{fbGoogREMB, fbCCMFIR, fbNACK, fbNACKPLI, fbTransportCC}
	pubAudioFB     = []webrtc.RTCPFeedback{fbNACK, fbTransportCC}
	subVideoFB     = []webrtc.RTCPFeedback{fbGoogREMB, fbCCMFIR, fbNACK, fbNACKPLI}
	subAudioFB     = []webrtc.RTCPFeedback{fbNACK}
	pubVideoHdrExt = []string{sdp.SDESMidURI, sdp.SDESRTPStreamIDURI, sdp.SDESRepairRTPStreamIDURI, sdp.TransportCCURI}
	pubAudioHdrExt = []string{sdp.TransportCCURI}
	// abs-send-time: REMB needs it when there is no transport-cc.
	subVideoHdrExt = []string{sdp.ABSSendTimeURI}
)

// engineSpec describes one MediaEngine of 02 §8.1.
type engineSpec struct {
	videoFB, audioFB         []webrtc.RTCPFeedback
	opusFmtp                 string
	videoHdrExt, audioHdrExt []string
}

var (
	pubEngineSpec = engineSpec{pubVideoFB, pubAudioFB, opusFmtpPub, pubVideoHdrExt, pubAudioHdrExt}
	subEngineSpec = engineSpec{subVideoFB, subAudioFB, opusFmtpSub, subVideoHdrExt, nil}
)

// newPubEngine builds the publish side's MediaEngine (browser or native → SFU) and its interceptors: a NACK
// generator, receiver reports and transport-wide CC feedback. No NACK responder, sender reports or stats
// interceptor: the SFU never sends media on a pub PC and counts packets itself. Built by hand rather than with
// webrtc.RegisterDefaultInterceptors, so nothing else slips in.
func newPubEngine() (*webrtc.MediaEngine, *interceptor.Registry, error) {
	m, err := newMediaEngine(pubEngineSpec)
	if err != nil {
		return nil, nil, err
	}
	factories, err := pubInterceptors()
	if err != nil {
		return nil, nil, err
	}
	ir := &interceptor.Registry{}
	for _, f := range factories {
		ir.Add(f)
	}
	return m, ir, nil
}

// newSubEngine builds the subscribe side's MediaEngine (SFU → viewer) and its interceptor registry, which stays
// empty: the shared packet cache answers NACKs, publishers' sender reports are forwarded, and the SFU keeps its own
// counters (02 §8.1, §9.5–9.6).
func newSubEngine() (*webrtc.MediaEngine, *interceptor.Registry, error) {
	m, err := newMediaEngine(subEngineSpec)
	if err != nil {
		return nil, nil, err
	}
	return m, &interceptor.Registry{}, nil
}

// pubInterceptors returns the publish side's interceptor factories in registration order.
func pubInterceptors() ([]interceptor.Factory, error) {
	gen, err := nack.NewGeneratorInterceptor(nack.GeneratorSize(pubNACKSize), nack.GeneratorInterval(pubNACKInterval))
	if err != nil {
		return nil, fmt.Errorf("sfu: nack generator: %w", err)
	}
	rr, err := report.NewReceiverInterceptor(report.ReceiverInterval(pubRRInterval))
	if err != nil {
		return nil, fmt.Errorf("sfu: receiver reports: %w", err)
	}
	tcc, err := twcc.NewSenderInterceptor()
	if err != nil {
		return nil, fmt.Errorf("sfu: twcc feedback: %w", err)
	}
	return []interceptor.Factory{gen, rr, tcc}, nil
}

// newMediaEngine registers the static PT table of 02 §8.1 with one side's feedback, Opus fmtp and header extensions.
func newMediaEngine(spec engineSpec) (*webrtc.MediaEngine, error) {
	m := &webrtc.MediaEngine{}
	for _, c := range h264Codecs {
		h264 := webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     webrtc.MimeTypeH264,
				ClockRate:    90000,
				SDPFmtpLine:  h264FmtpPrefix + c.profileLevelID,
				RTCPFeedback: slices.Clone(spec.videoFB),
			},
			PayloadType: c.pt,
		}
		rtx := webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:    webrtc.MimeTypeRTX,
				ClockRate:   90000,
				SDPFmtpLine: "apt=" + strconv.Itoa(int(c.pt)),
			},
			PayloadType: c.rtx,
		}
		for _, codec := range []webrtc.RTPCodecParameters{h264, rtx} {
			if err := m.RegisterCodec(codec, webrtc.RTPCodecTypeVideo); err != nil {
				return nil, fmt.Errorf("sfu: register PT %d: %w", codec.PayloadType, err)
			}
		}
	}
	opus := webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:     webrtc.MimeTypeOpus,
			ClockRate:    48000,
			Channels:     2,
			SDPFmtpLine:  spec.opusFmtp,
			RTCPFeedback: slices.Clone(spec.audioFB),
		},
		PayloadType: ptOpus,
	}
	if err := m.RegisterCodec(opus, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("sfu: register PT %d: %w", ptOpus, err)
	}
	for _, e := range []struct {
		uris []string
		kind webrtc.RTPCodecType
	}{{spec.videoHdrExt, webrtc.RTPCodecTypeVideo}, {spec.audioHdrExt, webrtc.RTPCodecTypeAudio}} {
		for _, uri := range e.uris {
			ext := webrtc.RTPHeaderExtensionCapability{URI: uri}
			if err := m.RegisterHeaderExtension(ext, e.kind); err != nil {
				return nil, fmt.Errorf("sfu: register header extension %s: %w", uri, err)
			}
		}
	}
	return m, nil
}
