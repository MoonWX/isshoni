package publish

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/interceptor/pkg/twcc"
	"github.com/pion/rtcp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/media/fake"
)

// Source is the media a Publisher sends: *fake.Source in M1, the native engine in M2.
type Source interface {
	// Next blocks until the next packet is due and returns it; packets come in capture order.
	Next(ctx context.Context) (fake.Packet, error)
	// RequestKeyframe makes the next frame of a layer a keyframe.
	RequestKeyframe(layer string)
}

// ProfileSource is a Source that can encode another H.264 profile (in M2 the native engine re-initializes its
// encoder). When an answer has the Publisher send another profile (SetProfile, 02 §8.3), it switches the Source with
// SetProfile, then asks it for a keyframe of every layer, and sends that keyframe on the new profile's payload type.
// A Source that isn't a ProfileSource (a fake.Source has one profile) keeps its SPS: only the payload type changes,
// which is what an SFU goes by, but a decoder would see through it.
type ProfileSource interface {
	Source
	SetProfile(profile string) error
}

// Options configure a Publisher.
type Options struct {
	Source Source
	// Settings configure Pion's networking (network types, interface and IP filters, muxes) and its logging. The
	// zero value is Pion's default.
	Settings   webrtc.SettingEngine
	ICEServers []webrtc.ICEServer
	// Layers are the Source layers to send, one simulcast encoding each, in this order; a layer's name is its rid.
	// nil means {"f", "q"}; an empty, non-nil slice means no video (then Audio must be on). A single layer goes out
	// as a plain track without a rid, like a browser without simulcast: Pion signals rids only for two or more
	// encodings (02 §8.4: rids ⊆ {f,q}, or none).
	Layers []string
	// Profile is the H.264 profile key the video codec is offered with first: DefaultProfile ("6400"), "42e0", or
	// another 4-hex-digit key. It must match the Source's SPS.
	Profile string
	// Profiles are the other H.264 profiles the Publisher can send, offered after Profile in this order, the way a
	// browser offers every profile its encoder has: at most 4, each a 4-hex-digit key other than Profile. The
	// Publisher sends the first of its profiles that an answer lists (as a browser does), so a codec policy that
	// allows only some of them (02 §8.4) chooses; SetProfile puts one first for the next offer. Profile i (Profile
	// being 0) has payload type 96 + 2i, and its RTX 97 + 2i.
	Profiles []string
	Audio    bool
	// StreamID is the msid stream id of the tracks (default DefaultStreamID). The SFU maps tracks by mid.
	StreamID string
	// RedundantRTX is a test option (02 §17 test 19) that makes the Publisher repair like a browser. It negotiates
	// RTX for the video and answers NACKs itself: video on the RTX stream (RFC 4588), with the
	// repaired-rtp-stream-id extension when the video is simulcast, audio as the packet again. And it sends, on the
	// RTX stream, a second copy of the last packet of every video frame right after the frame, the way Chrome fills
	// its padding and bandwidth probes with payloads it already delivered. A receiver must drop those copies.
	RedundantRTX bool
	// Padding is a test option (02 §17 test 6): after every Padding-th frame of each video layer the Publisher
	// sends two padding-only packets on the layer's media stream, as a browser without RTX pads and probes. They
	// take sequence numbers and carry no payload, so a receiver that forwards the stream must close the gaps they
	// would leave. 0 sends none.
	Padding int
	// Logger gets the Publisher's own logs (default slog.Default()). Pion logs through Settings.LoggerFactory.
	Logger *slog.Logger
}

// Defaults and fixed values.
const (
	DefaultProfile  = "6400"
	DefaultStreamID = "isshoni"
	// PayloadMTU is the H.264 payloader's MTU: the largest RTP payload.
	PayloadMTU = 1200
	// SRInterval is how often each SSRC gets a sender report.
	SRInterval = time.Second

	videoPT        = 96 // the first profile's (Options.Profile); profile i has videoPT + 2i
	rtxPT          = 97 // RedundantRTX: the first profile's RTX payload type; profile i has rtxPT + 2i
	maxProfiles    = 5  // Profile and 4 more: the profiles of the SFU's table (02 §8.1)
	opusPT         = 111
	h264FmtpPrefix = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id="
	opusFmtp       = "minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1"
	videoClock     = 90000
	audioClock     = 48000
	connectPoll    = 5 * time.Millisecond
)

var (
	// ErrInvalidOptions is wrapped by New's errors about Options.
	ErrInvalidOptions = errors.New("publish: invalid options")
	// ErrClosed is returned by Start after Close.
	ErrClosed = errors.New("publish: closed")
)

// TrackInfo identifies one sent RTP stream: a simulcast encoding or the audio.
type TrackInfo struct {
	Kind  webrtc.RTPCodecType
	RID   string // the encoding's rid; "" for audio and for a single layer
	Layer string // the Source layer; "" for audio
	MID   string // "" until the offer is created
	SSRC  uint32
}

// TrackStats are one track's counters.
type TrackStats struct {
	TrackInfo
	Packets, Octets   uint64 // RTP packets and payload bytes written; padding and repairs are counted apart
	Frames, Keyframes int    // access units (audio: packets) and keyframes written
	WriteErrors       int
	PLIs, FIRs, NACKs int    // received for this SSRC
	SRs               int    // sender reports written
	LastREMB          uint64 // bps of the last REMB naming this SSRC; 0 before the first
	// Padding counts the padding-only packets written (Options.Padding). Redundant counts the second copies sent on
	// the RTX stream, and Retransmitted the packets sent again for a NACK (both Options.RedundantRTX; without it
	// Pion's NACK responder answers, and nothing counts here).
	Padding, Redundant, Retransmitted uint64
	// Profile is the H.264 profile key the video goes out in now, and PT its payload type (0 before the track is
	// bound). Audio has neither.
	Profile string
	PT      uint8
}

// Stats are a Publisher's counters.
type Stats struct {
	Tracks []TrackStats
	// Dropped counts Source packets that no track sent: an unknown layer, or a track not bound yet.
	Dropped int64
}

// Publisher sends a Source's media on its own PeerConnection. New sets the PeerConnection up and starts nothing; the
// caller negotiates (Offer, then SetAnswer), then calls Start, and finally Close.
type Publisher struct {
	src     Source
	pc      *webrtc.PeerConnection
	log     *slog.Logger
	video   []*track // in encoding order
	audio   *track   // nil without audio
	vsender *webrtc.RTPSender
	asender *webrtc.RTPSender
	dropped atomic.Int64

	// The capture-to-wall-clock anchor, set from the first packet sent (send loop only).
	anchored bool
	cap0     int64
	wall0    time.Time

	// h264 are the H.264 profiles the Publisher can send, Options.Profile first: their payload types (h264Table).
	h264 h264Table

	mu      sync.Mutex // guards the fields below
	started bool
	closed  bool
	cancel  context.CancelFunc
	// chosen is the profile the last answer chose (followAnswer), "" once the send loop has switched to it
	// (applyProfile); encoding is the profile the Source encodes: Options.Profile, then the last one switched to.
	chosen, encoding string
	wg               sync.WaitGroup
}

// New checks o, builds the PeerConnection with its MediaEngine and interceptors, and adds sendonly transceivers: one
// video transceiver with one encoding per layer, and one audio transceiver.
func New(o Options) (*Publisher, error) {
	if o.Source == nil {
		return nil, fmt.Errorf("%w: no Source", ErrInvalidOptions)
	}
	if o.Padding < 0 {
		return nil, fmt.Errorf("%w: Padding %d is negative", ErrInvalidOptions, o.Padding)
	}
	layers := o.Layers
	if layers == nil {
		layers = []string{"f", "q"}
	}
	if len(layers) == 0 && !o.Audio {
		return nil, fmt.Errorf("%w: no layers and no audio", ErrInvalidOptions)
	}
	for i, l := range layers {
		if l == "" || slices.Contains(layers[:i], l) {
			return nil, fmt.Errorf("%w: layer %q is empty or repeated", ErrInvalidOptions, l)
		}
	}
	profile := o.Profile
	if profile == "" {
		profile = DefaultProfile
	}
	table := append(h264Table{profile}, o.Profiles...)
	for i, k := range table {
		if profileKey(h264FmtpPrefix+k) != k || len(k) != 4 {
			return nil, fmt.Errorf("%w: profile %q is not 4 lowercase hex digits", ErrInvalidOptions, k)
		}
		if slices.Contains(table[:i], k) {
			return nil, fmt.Errorf("%w: profile %q is repeated", ErrInvalidOptions, k)
		}
	}
	if len(table) > maxProfiles {
		return nil, fmt.Errorf("%w: %d profiles (at most %d)", ErrInvalidOptions, len(table), maxProfiles)
	}
	stream := o.StreamID
	if stream == "" {
		stream = DefaultStreamID
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}

	api, err := newAPI(o.Settings, table, o.RedundantRTX)
	if err != nil {
		return nil, err
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers:   o.ICEServers,
		BundlePolicy: webrtc.BundlePolicyMaxBundle,
	})
	if err != nil {
		return nil, fmt.Errorf("publish: new PeerConnection: %w", err)
	}
	p := &Publisher{src: o.Source, pc: pc, log: log, h264: table, encoding: profile}
	if err := p.addTracks(layers, o.Audio, profile, stream); err != nil {
		_ = pc.Close()
		return nil, err
	}
	for _, t := range p.tracks() {
		if o.RedundantRTX {
			t.repair = newRepair()
		}
		if t.kind == webrtc.RTPCodecTypeVideo {
			t.padEvery, t.table = o.Padding, table
		}
	}
	return p, nil
}

// h264Table is a Publisher's H.264 profiles, in the order it offers them at first: profile i has payload type
// videoPT + 2i, and RTX payload type rtxPT + 2i when RTX is negotiated (Options.RedundantRTX).
type h264Table []string

// pts returns a profile's payload types; ok is false for a profile that isn't in the table.
func (h h264Table) pts(profile string) (pt, rtx uint8, ok bool) {
	i := slices.Index(h, profile)
	if i < 0 {
		return 0, 0, false
	}
	return uint8(videoPT + 2*i), uint8(rtxPT + 2*i), true
}

// codecs returns the table's codecs with their feedback, the given profile first and the others in table order,
// each followed by its RTX codec when rtx is set: the order of the codec preferences of SetProfile, and, with the
// first profile first, the order of registration.
func (h h264Table) codecs(first string, rtx bool) []webrtc.RTPCodecParameters {
	order := append([]string{first}, slices.DeleteFunc(slices.Clone(h), func(k string) bool { return k == first })...)
	var out []webrtc.RTPCodecParameters
	for _, k := range order {
		pt, rpt, ok := h.pts(k)
		if !ok {
			continue
		}
		out = append(out, webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeH264, ClockRate: videoClock, SDPFmtpLine: h264FmtpPrefix + k + "1f",
				RTCPFeedback: slices.Clone(videoFeedback),
			},
			PayloadType: webrtc.PayloadType(pt),
		})
		if rtx {
			out = append(out, webrtc.RTPCodecParameters{
				RTPCodecCapability: webrtc.RTPCodecCapability{
					MimeType: webrtc.MimeTypeRTX, ClockRate: videoClock, SDPFmtpLine: "apt=" + strconv.Itoa(int(pt)),
				},
				PayloadType: webrtc.PayloadType(rpt),
			})
		}
	}
	return out
}

// videoFeedback is the RTCP feedback a browser offers for its video codecs.
var videoFeedback = []webrtc.RTCPFeedback{
	{Type: webrtc.TypeRTCPFBGoogREMB}, {Type: webrtc.TypeRTCPFBCCM, Parameter: "fir"},
	{Type: webrtc.TypeRTCPFBNACK}, {Type: webrtc.TypeRTCPFBNACK, Parameter: "pli"},
	{Type: webrtc.TypeRTCPFBTransportCC},
}

// newAPI builds a browser-like publishing API: the H.264 profiles the Publisher can send (h264, in that order) and
// Opus, simulcast and transport-wide-cc header extensions, a NACK responder and the TWCC header-extension
// interceptor. No sender-report interceptor: the Publisher writes its own SRs. With rtx (Options.RedundantRTX) each
// profile has an RTX payload type and there is no NACK responder: the tracks answer NACKs themselves (repair.go),
// because the RTX stream has one sequence of numbers, which the second copies they send on it must share with the
// repairs.
func newAPI(se webrtc.SettingEngine, h264 h264Table, rtx bool) (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	for _, c := range h264.codecs(h264[0], rtx) {
		if err := m.RegisterCodec(c, webrtc.RTPCodecTypeVideo); err != nil {
			return nil, fmt.Errorf("publish: register %s %d: %w", c.MimeType, c.PayloadType, err)
		}
	}
	opus := webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeOpus, ClockRate: audioClock, Channels: 2, SDPFmtpLine: opusFmtp,
			RTCPFeedback: []webrtc.RTCPFeedback{{Type: webrtc.TypeRTCPFBNACK}, {Type: webrtc.TypeRTCPFBTransportCC}},
		},
		PayloadType: opusPT,
	}
	if err := m.RegisterCodec(opus, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("publish: register %s: %w", opus.MimeType, err)
	}
	for _, e := range []struct {
		uri  string
		kind webrtc.RTPCodecType
	}{
		{sdp.SDESMidURI, webrtc.RTPCodecTypeVideo}, {sdp.SDESRTPStreamIDURI, webrtc.RTPCodecTypeVideo},
		{sdp.SDESRepairRTPStreamIDURI, webrtc.RTPCodecTypeVideo}, {sdp.TransportCCURI, webrtc.RTPCodecTypeVideo},
		{sdp.SDESMidURI, webrtc.RTPCodecTypeAudio}, {sdp.TransportCCURI, webrtc.RTPCodecTypeAudio},
	} {
		if err := m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: e.uri}, e.kind); err != nil {
			return nil, fmt.Errorf("publish: register header extension %s: %w", e.uri, err)
		}
	}
	ir := &interceptor.Registry{}
	if !rtx {
		responder, err := nack.NewResponderInterceptor()
		if err != nil {
			return nil, fmt.Errorf("publish: nack responder: %w", err)
		}
		ir.Add(responder)
	}
	tcc, err := twcc.NewHeaderExtensionInterceptor()
	if err != nil {
		return nil, fmt.Errorf("publish: twcc header extension: %w", err)
	}
	ir.Add(tcc)
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir),
		webrtc.WithSettingEngine(se)), nil
}

// addTracks adds the video transceiver (one encoding per layer) and the audio transceiver, both sendonly.
func (p *Publisher) addTracks(layers []string, audio bool, profile, stream string) error {
	sendonly := webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly}
	for _, l := range layers {
		rid := l
		if len(layers) == 1 {
			rid = ""
		}
		p.video = append(p.video, newTrack(webrtc.RTPCodecTypeVideo, "video", stream, rid, l, profile))
	}
	if len(p.video) > 0 {
		tr, err := p.pc.AddTransceiverFromTrack(p.video[0], sendonly)
		if err != nil {
			return fmt.Errorf("publish: add video transceiver: %w", err)
		}
		for _, t := range p.video[1:] {
			if err := tr.Sender().AddEncoding(t); err != nil {
				return fmt.Errorf("publish: add encoding %q: %w", t.rid, err)
			}
		}
		p.vsender = tr.Sender()
		encodings := p.vsender.GetParameters().Encodings
		for i, t := range p.video {
			t.tr = tr
			if i < len(encodings) {
				t.stats.SSRC = uint32(encodings[i].SSRC)
			}
		}
	}
	if audio {
		p.audio = newTrack(webrtc.RTPCodecTypeAudio, "audio", stream, "", "", "")
		tr, err := p.pc.AddTransceiverFromTrack(p.audio, sendonly)
		if err != nil {
			return fmt.Errorf("publish: add audio transceiver: %w", err)
		}
		p.audio.tr = tr
		p.asender = tr.Sender()
		if encodings := p.asender.GetParameters().Encodings; len(encodings) > 0 {
			p.audio.stats.SSRC = uint32(encodings[0].SSRC)
		}
	}
	return nil
}

// PC returns the Publisher's PeerConnection, for signaling and state callbacks. Close closes it.
func (p *Publisher) PC() *webrtc.PeerConnection { return p.pc }

// Offer creates the offer, sets it as the local description and returns it with every ICE candidate in it (no
// trickle), which is what the SFU expects. Call it again for a re-offer on the same PeerConnection (the next neg of
// the same gen).
//
// What it returns is the copy to send, which differs from the local description in a re-offer of simulcast video:
// see sendOnlySimulcast.
func (p *Publisher) Offer(ctx context.Context) (webrtc.SessionDescription, error) {
	return p.offer(ctx, nil)
}

// OfferICERestart is Offer for an ICE restart: the offer has new ICE credentials and newly gathered candidates, and
// the SFU's answer brings its own new ones. It is how a client recovers a publish PeerConnection whose ICE has been
// disconnected for 3 s (01 §10.4): the next neg of the same gen, with the tracks and their SSRCs as they are. Pion
// drops the selected candidate pair when it makes the offer, so nothing is sent until ICE is connected again; the
// send loop goes on pulling from the Source meanwhile, and the SFU asks for a keyframe when it sees the
// PeerConnection connected.
func (p *Publisher) OfferICERestart(ctx context.Context) (webrtc.SessionDescription, error) {
	return p.offer(ctx, &webrtc.OfferOptions{ICERestart: true})
}

func (p *Publisher) offer(ctx context.Context, opts *webrtc.OfferOptions) (webrtc.SessionDescription, error) {
	offer, err := p.pc.CreateOffer(opts)
	if err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("publish: create offer: %w", err)
	}
	gathered := webrtc.GatheringCompletePromise(p.pc)
	if err := p.pc.SetLocalDescription(offer); err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("publish: set local description: %w", err)
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		return webrtc.SessionDescription{}, ctx.Err()
	}
	local := *p.pc.LocalDescription()
	local.SDP = sendOnlySimulcast(local.SDP)
	return local, nil
}

// sendOnlySimulcast returns an offer without the receive side of its simulcast attributes: the "a=rid:<id> recv"
// lines, and the recv list of "a=simulcast" (the whole line when it has no send list).
//
// Once a simulcast m-section has been answered, Pion's next offer repeats the rids of that answer as its own
// receive rids ("a=rid:f recv", "a=simulcast:recv f;q") next to the ones it sends ("a=rid:f send",
// "a=simulcast:send f;q"). A Publisher receives nothing, and no browser offers that. The SFU reads every a=rid line
// of an offer as a layer the publisher may send (as Pion's own answerer does), so it refuses a rid that is listed
// twice as sfu.bad_rid. These attributes only tell the remote side what to do, so the local description keeps its
// own and the copy that goes out loses them.
func sendOnlySimulcast(offer string) string {
	var out strings.Builder
	out.Grow(len(offer))
	for line := range strings.SplitAfterSeq(offer, "\n") {
		text := strings.TrimRight(line, "\r\n")
		eol := line[len(text):]
		switch {
		case strings.HasPrefix(text, "a=rid:"):
			if fields := strings.Fields(text); len(fields) > 1 && fields[1] == "recv" {
				continue
			}
		case strings.HasPrefix(text, "a=simulcast:"):
			// RFC 8853 §5.1: one or two "<direction> <list>" pairs.
			fields := strings.Fields(strings.TrimPrefix(text, "a=simulcast:"))
			var kept []string
			recv := false
			for i := 0; i+1 < len(fields); i += 2 {
				if fields[i] == "recv" {
					recv = true
					continue
				}
				kept = append(kept, fields[i], fields[i+1])
			}
			switch {
			case !recv: // nothing to drop: the line stays as it is
			case len(kept) == 0:
				continue
			default:
				text = "a=simulcast:" + strings.Join(kept, " ")
			}
		}
		out.WriteString(text)
		out.WriteString(eol)
	}
	return out.String()
}

// SetAnswer applies the remote answer. Pion binds the tracks here, the first time. The video goes on in the first
// H.264 profile of the answer's video m-section that the Publisher can send, as a browser's does: after a re-offer
// that changed the answer's choice (SetProfile, or a codec policy that allows fewer profiles), each layer switches
// to that profile's payload type with its next keyframe, which the send loop asks the Source for right away. A
// ProfileSource is switched to the profile first.
func (p *Publisher) SetAnswer(answer webrtc.SessionDescription) error {
	if err := p.pc.SetRemoteDescription(answer); err != nil {
		return fmt.Errorf("publish: set remote description: %w", err)
	}
	p.followAnswer(answer.SDP)
	return nil
}

// SetProfile puts an H.264 profile of the Publisher first in its video codec preferences, as a browser sharer does
// on a quality.hint with a codec (01 §8.10): the next Offer lists it first, and the Publisher sends it once an answer
// puts it first (SetAnswer). It returns an error that wraps ErrInvalidOptions for a profile the Publisher can't send
// (Options.Profile and Options.Profiles), and for a Publisher without video.
func (p *Publisher) SetProfile(profile string) error {
	if len(p.video) == 0 {
		return fmt.Errorf("%w: SetProfile without video", ErrInvalidOptions)
	}
	if !slices.Contains(p.h264, profile) {
		return fmt.Errorf("%w: profile %q is not one the Publisher can send", ErrInvalidOptions, profile)
	}
	rtx := p.video[0].repair != nil
	if err := p.video[0].tr.SetCodecPreferences(p.h264.codecs(profile, rtx)); err != nil {
		return fmt.Errorf("publish: codec preferences: %w", err)
	}
	return nil
}

// followAnswer notes an answer's choice of profile, the first H.264 codec of the video m-section with a profile the
// Publisher can send, for the send loop to switch to (applyProfile). An answer without one changes nothing.
func (p *Publisher) followAnswer(answer string) {
	if len(p.video) == 0 {
		return
	}
	profile := answerProfile(answer, p.video[0].tr.Mid(), p.h264)
	if profile == "" {
		return
	}
	p.mu.Lock()
	p.chosen = profile // the send loop switches (applyProfile); before Start, its first round does
	p.mu.Unlock()
}

// answerProfile returns the profile of the first H.264 codec with packetization-mode=1 in an answer's m-section mid
// that h264 has, or "".
func answerProfile(answer, mid string, h264 h264Table) string {
	var desc sdp.SessionDescription
	if mid == "" || desc.UnmarshalString(answer) != nil {
		return ""
	}
	for _, md := range desc.MediaDescriptions {
		if m, _ := md.Attribute(sdp.AttrKeyMID); m != mid || md.MediaName.Port.Value == 0 {
			continue
		}
		section := sdp.SessionDescription{MediaDescriptions: []*sdp.MediaDescription{md}} // its codecs alone
		for _, f := range md.MediaName.Formats {
			pt, err := strconv.ParseUint(f, 10, 8)
			if err != nil {
				continue
			}
			codec, err := section.GetCodecForPayloadType(uint8(pt))
			if err != nil || !strings.EqualFold(codec.Name, "h264") {
				continue
			}
			if k := profileKey(codec.Fmtp); slices.Contains(h264, k) {
				return k
			}
		}
	}
	return ""
}

// Tracks describes the sent RTP streams, video encodings first. MIDs are known once Offer has run.
func (p *Publisher) Tracks() []TrackInfo {
	var out []TrackInfo
	for _, t := range p.tracks() {
		out = append(out, t.info())
	}
	return out
}

// Stats returns a snapshot of the counters.
func (p *Publisher) Stats() Stats {
	s := Stats{Dropped: p.dropped.Load()}
	for _, t := range p.tracks() {
		t.mu.Lock()
		ts := t.stats
		if t.kind == webrtc.RTPCodecTypeVideo {
			ts.Profile = t.profile
			if t.b != nil {
				ts.PT = t.b.pt
			}
		}
		t.mu.Unlock()
		ts.TrackInfo = t.info()
		s.Tracks = append(s.Tracks, ts)
	}
	return s
}

func (p *Publisher) tracks() []*track {
	out := slices.Clone(p.video)
	if p.audio != nil {
		out = append(out, p.audio)
	}
	return out
}

// Start starts sending: the send loop pulls from the Source once the PeerConnection is connected (so the first frame,
// a keyframe, isn't lost to the DTLS handshake), one RTCP reader per track handles PLI, FIR, NACK and REMB, and a
// ticker writes the sender reports. The send loop and the SR ticker stop when ctx ends; the RTCP readers, which
// block in Pion reads that only closing the PeerConnection ends, stop at Close, which waits for every goroutine.
func (p *Publisher) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	if p.started {
		return errors.New("publish: already started")
	}
	p.started = true
	ctx, p.cancel = context.WithCancel(ctx)
	p.wg.Add(2)
	go p.sendLoop(ctx)
	go p.srLoop(ctx)
	for _, t := range p.video {
		read := func(b []byte) (int, error) { n, _, err := p.vsender.Read(b); return n, err }
		if t.rid != "" {
			rid := t.rid
			read = func(b []byte) (int, error) { n, _, err := p.vsender.ReadSimulcast(b, rid); return n, err }
		}
		p.wg.Add(1)
		go p.rtcpLoop(t, read)
	}
	if p.audio != nil {
		p.wg.Add(1)
		go p.rtcpLoop(p.audio, func(b []byte) (int, error) { n, _, err := p.asender.Read(b); return n, err })
	}
	return nil
}

// Close stops the loops, closes the PeerConnection and waits for every goroutine. The Source stays open, so a new
// Publisher can take it over (a pub PC rebuild); a fake.Source then resumes at most 200 ms behind real time instead
// of handing out, in one burst, all the media due while nobody pulled. Close is idempotent.
func (p *Publisher) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	cancel := p.cancel
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	err := p.pc.Close()
	p.wg.Wait()
	if err != nil {
		return fmt.Errorf("publish: close: %w", err)
	}
	return nil
}

// sendLoop pulls packets from the Source and writes them, until ctx ends or the Source fails.
func (p *Publisher) sendLoop(ctx context.Context) {
	defer p.wg.Done()
	if err := p.waitConnected(ctx); err != nil {
		p.log.Debug("publish: not sending", "err", err)
		return
	}
	for {
		p.applyProfile()
		pkt, err := p.src.Next(ctx)
		if err != nil {
			if ctx.Err() == nil {
				p.log.Warn("publish: source stopped", "err", err)
			}
			return
		}
		p.send(pkt)
	}
}

// applyProfile switches the video to the profile the last answer chose, if it hasn't yet (followAnswer). The send
// loop does it between two packets, so that no packet the Source made before the switch goes out after it: a
// ProfileSource is switched first, then each layer is set to go on in the new profile's payload type from its next
// keyframe, which the Source is asked for.
func (p *Publisher) applyProfile() {
	p.mu.Lock()
	profile, encoding := p.chosen, p.encoding
	p.chosen = ""
	if profile != "" {
		p.encoding = profile
	}
	p.mu.Unlock()
	if profile == "" {
		return
	}
	if ps, ok := p.src.(ProfileSource); ok && profile != encoding {
		if err := ps.SetProfile(profile); err != nil {
			p.log.Warn("publish: the source did not switch profiles", "profile", profile, "err", err)
		}
	}
	switched := false
	for _, t := range p.video {
		switched = t.switchTo(profile) || switched
	}
	if switched {
		for _, t := range p.video {
			p.src.RequestKeyframe(t.layer)
		}
	}
}

// waitConnected polls the connection state, so that the caller keeps the PeerConnection's state callback.
func (p *Publisher) waitConnected(ctx context.Context) error {
	tick := time.NewTicker(connectPoll)
	defer tick.Stop()
	for {
		switch s := p.pc.ConnectionState(); s {
		case webrtc.PeerConnectionStateConnected:
			return nil
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			return fmt.Errorf("publish: PeerConnection %s", s)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// send writes one Source packet on its track.
func (p *Publisher) send(pkt fake.Packet) {
	t := p.trackFor(pkt)
	if t == nil {
		p.dropped.Add(1)
		return
	}
	if !p.anchored {
		// The Source returns a packet when it is due, so its capture instant is now.
		p.anchored, p.cap0, p.wall0 = true, pkt.CaptureNS, time.Now()
	}
	d := pkt.CaptureNS - p.cap0
	if !t.write(pkt, rtpTicks(d, t.clock), ntpTime(p.wall0.Add(time.Duration(d)))) {
		p.dropped.Add(1)
	}
}

func (p *Publisher) trackFor(pkt fake.Packet) *track {
	switch pkt.Kind {
	case fake.Video:
		for _, t := range p.video {
			if t.layer == pkt.Layer {
				return t
			}
		}
	case fake.Audio:
		return p.audio
	}
	return nil
}

// srLoop writes a sender report for every track that has sent something, every SRInterval.
func (p *Publisher) srLoop(ctx context.Context) {
	defer p.wg.Done()
	tick := time.NewTicker(SRInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		for _, t := range p.tracks() {
			sr, ok := t.senderReport()
			if !ok {
				continue
			}
			if err := p.pc.WriteRTCP([]rtcp.Packet{sr}); err != nil {
				p.log.Debug("publish: write SR", "err", err)
				continue
			}
			t.mu.Lock()
			t.stats.SRs++
			t.mu.Unlock()
		}
	}
}

// rtcpLoop reads one track's RTCP until the sender stops. Reading also runs the interceptors' RTCP handling (the
// NACK responder); with Options.RedundantRTX the track answers the NACKs here.
func (p *Publisher) rtcpLoop(t *track, read func([]byte) (int, error)) {
	defer p.wg.Done()
	buf := make([]byte, 1500)
	for {
		n, err := read(buf)
		if err != nil {
			return
		}
		pkts, err := rtcp.Unmarshal(buf[:n])
		if err != nil {
			continue
		}
		for _, pkt := range pkts {
			if layer, key := t.onRTCP(pkt); key {
				p.src.RequestKeyframe(layer)
			}
			if nack, ok := pkt.(*rtcp.TransportLayerNack); ok {
				t.answerNACK(nack)
			}
		}
	}
}

// profileKey returns the lowercased first 4 hex digits of an H.264 fmtp line's profile-level-id when it asks for
// packetization-mode=1, else "".
func profileKey(fmtp string) string {
	var key string
	mode1 := false
	for param := range strings.SplitSeq(fmtp, ";") {
		name, value, _ := strings.Cut(strings.TrimSpace(param), "=")
		switch strings.ToLower(name) {
		case "profile-level-id":
			if _, err := strconv.ParseUint(value, 16, 32); err == nil && len(value) >= 4 {
				key = strings.ToLower(value[:4])
			}
		case "packetization-mode":
			mode1 = value == "1"
		}
	}
	if !mode1 {
		return ""
	}
	return key
}

// randomBase returns the random first sequence number and timestamp of a track. RFC 3550 §5.1 wants them
// unpredictable, against known-plaintext attacks on the encryption.
func randomBase() (uint16, uint32) {
	var b [6]byte
	_, _ = rand.Read(b[:]) // never fails (crypto/rand)
	return binary.BigEndian.Uint16(b[:2]), binary.BigEndian.Uint32(b[2:])
}
