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
	// Profile is the H.264 profile key the video codec is offered with: DefaultProfile ("6400"), "42e0", or another
	// 4-hex-digit key. It must match the Source's SPS.
	Profile string
	Audio   bool
	// StreamID is the msid stream id of the tracks (default DefaultStreamID). The SFU maps tracks by mid.
	StreamID string
	// RedundantRTX (02 §17 test 19) negotiates RTX and resends already-delivered packets on the RTX stream with the
	// repaired-rtp-stream-id extension, like Chrome's padding and probes. Not implemented yet (README S63): New
	// returns ErrNotImplemented.
	RedundantRTX bool
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

	videoPT        = 96
	opusPT         = 111
	h264FmtpPrefix = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id="
	opusFmtp       = "minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1"
	videoClock     = 90000
	audioClock     = 48000
	connectPoll    = 5 * time.Millisecond
)

var (
	// ErrNotImplemented is returned by New for an option that a later slice adds.
	ErrNotImplemented = errors.New("publish: not implemented yet")
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
	Packets, Octets   uint64 // RTP packets and payload bytes written
	Frames, Keyframes int    // access units (audio: packets) and keyframes written
	WriteErrors       int
	PLIs, FIRs, NACKs int    // received for this SSRC
	SRs               int    // sender reports written
	LastREMB          uint64 // bps of the last REMB naming this SSRC; 0 before the first
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

	mu      sync.Mutex // guards started, closed and cancel
	started bool
	closed  bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// New checks o, builds the PeerConnection with its MediaEngine and interceptors, and adds sendonly transceivers: one
// video transceiver with one encoding per layer, and one audio transceiver.
func New(o Options) (*Publisher, error) {
	if o.Source == nil {
		return nil, fmt.Errorf("%w: no Source", ErrInvalidOptions)
	}
	if o.RedundantRTX {
		return nil, fmt.Errorf("%w: RedundantRTX (README S63)", ErrNotImplemented)
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
	if profileKey(h264FmtpPrefix+profile) != profile || len(profile) != 4 {
		return nil, fmt.Errorf("%w: Profile %q is not 4 lowercase hex digits", ErrInvalidOptions, profile)
	}
	stream := o.StreamID
	if stream == "" {
		stream = DefaultStreamID
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}

	api, err := newAPI(o.Settings, profile)
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
	p := &Publisher{src: o.Source, pc: pc, log: log}
	if err := p.addTracks(layers, o.Audio, profile, stream); err != nil {
		_ = pc.Close()
		return nil, err
	}
	return p, nil
}

// newAPI builds a browser-like publishing API: the one H.264 profile the Source sends and Opus, simulcast and
// transport-wide-cc header extensions, a NACK responder and the TWCC header-extension interceptor. No sender-report
// interceptor: the Publisher writes its own SRs.
func newAPI(se webrtc.SettingEngine, profile string) (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	videoFB := []webrtc.RTCPFeedback{
		{Type: webrtc.TypeRTCPFBGoogREMB}, {Type: webrtc.TypeRTCPFBCCM, Parameter: "fir"},
		{Type: webrtc.TypeRTCPFBNACK}, {Type: webrtc.TypeRTCPFBNACK, Parameter: "pli"},
		{Type: webrtc.TypeRTCPFBTransportCC},
	}
	audioFB := []webrtc.RTCPFeedback{{Type: webrtc.TypeRTCPFBNACK}, {Type: webrtc.TypeRTCPFBTransportCC}}
	for _, c := range []struct {
		params webrtc.RTPCodecParameters
		kind   webrtc.RTPCodecType
	}{
		{webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeH264, ClockRate: videoClock, SDPFmtpLine: h264FmtpPrefix + profile + "1f",
				RTCPFeedback: videoFB,
			},
			PayloadType: videoPT,
		}, webrtc.RTPCodecTypeVideo},
		{webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeOpus, ClockRate: audioClock, Channels: 2, SDPFmtpLine: opusFmtp,
				RTCPFeedback: audioFB,
			},
			PayloadType: opusPT,
		}, webrtc.RTPCodecTypeAudio},
	} {
		if err := m.RegisterCodec(c.params, c.kind); err != nil {
			return nil, fmt.Errorf("publish: register %s: %w", c.params.MimeType, err)
		}
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
	responder, err := nack.NewResponderInterceptor()
	if err != nil {
		return nil, fmt.Errorf("publish: nack responder: %w", err)
	}
	tcc, err := twcc.NewHeaderExtensionInterceptor()
	if err != nil {
		return nil, fmt.Errorf("publish: twcc header extension: %w", err)
	}
	ir := &interceptor.Registry{}
	ir.Add(responder)
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
// trickle), which is what the SFU expects.
func (p *Publisher) Offer(ctx context.Context) (webrtc.SessionDescription, error) {
	offer, err := p.pc.CreateOffer(nil)
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
	return *p.pc.LocalDescription(), nil
}

// SetAnswer applies the remote answer. Pion binds the tracks here.
func (p *Publisher) SetAnswer(answer webrtc.SessionDescription) error {
	if err := p.pc.SetRemoteDescription(answer); err != nil {
		return fmt.Errorf("publish: set remote description: %w", err)
	}
	return nil
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
// NACK responder).
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
