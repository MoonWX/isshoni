package sfutest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/interceptor/pkg/report"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

// ViewerOptions configure a Viewer.
type ViewerOptions struct {
	// Settings configure Pion's networking and logging; LoopbackSettings suits in-process tests. The zero value is
	// Pion's default.
	Settings   webrtc.SettingEngine
	ICEServers []webrtc.ICEServer
	// H264 lists the profile-level-ids the viewer decodes, in preference order. nil means DefaultH264, the five
	// profiles of 02 §8.1, like Chrome.
	H264 []string
	// NoH264 registers no H.264 at all, like a fresh Firefox profile before its decoder download (02 §8.5).
	NoH264 bool
	// NoRTX registers no RTX codecs, so repairs come as plain retransmissions.
	NoRTX bool
	// KeepPackets bounds the packets each Recorder keeps (the newest are kept; the counters cover all). 0 means
	// DefaultKeepPackets; negative keeps none.
	KeepPackets int
	// Logger gets the Viewer's own logs (default slog.Default()).
	Logger *slog.Logger
}

// DefaultH264 are the profile-level-ids a Viewer decodes by default: the SFU's static table (02 §8.1).
var DefaultH264 = []string{"42e01f", "42001f", "4d001f", "640c1f", "64001f"}

// Viewer defaults.
const (
	DefaultKeepPackets = 1 << 17
	// FinalLossAfter is how long a missing sequence number may stay missing before it counts as lost.
	FinalLossAfter = time.Second
	pollInterval   = 10 * time.Millisecond
	opusPT         = 111
)

// Viewer is a Pion subscriber that records what it receives. NewViewer creates its PeerConnection; tracks are
// recorded from the first packet on, until Close. Rebuild replaces the PeerConnection with a new one, as a client
// does for a sub offer of a new gen (01 §9 rule 2); the Recorders of the old one stay, and stop growing.
type Viewer struct {
	api  *webrtc.API
	cfg  webrtc.Configuration
	log  *slog.Logger
	keep int

	mu       sync.Mutex // guards the fields below
	pc       *webrtc.PeerConnection
	recs     []*Recorder
	rtcpRead map[rtcpKey]bool
	closed   bool
	wg       sync.WaitGroup
}

// rtcpKey names one RTCP reader: a receiver and, for simulcast, a rid.
type rtcpKey struct {
	recv *webrtc.RTPReceiver
	rid  string
}

// NewViewer builds the Viewer's API (a browser-like MediaEngine, a NACK generator and receiver reports every 1 s)
// and its PeerConnection.
func NewViewer(o ViewerOptions) (*Viewer, error) {
	m, err := viewerEngine(o)
	if err != nil {
		return nil, err
	}
	gen, err := nack.NewGeneratorInterceptor(nack.GeneratorSize(2048))
	if err != nil {
		return nil, fmt.Errorf("sfutest: nack generator: %w", err)
	}
	rr, err := report.NewReceiverInterceptor(report.ReceiverInterval(time.Second))
	if err != nil {
		return nil, fmt.Errorf("sfutest: receiver reports: %w", err)
	}
	ir := &interceptor.Registry{}
	ir.Add(gen)
	ir.Add(rr)
	v := &Viewer{
		api: webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir),
			webrtc.WithSettingEngine(o.Settings)),
		cfg: webrtc.Configuration{ICEServers: o.ICEServers},
		log: o.Logger, keep: o.KeepPackets, rtcpRead: map[rtcpKey]bool{},
	}
	if v.log == nil {
		v.log = slog.Default()
	}
	if v.keep == 0 {
		v.keep = DefaultKeepPackets
	}
	if v.pc, err = v.newPC(); err != nil {
		return nil, err
	}
	return v, nil
}

// newPC creates a PeerConnection of the Viewer's API whose tracks the Viewer records.
func (v *Viewer) newPC() (*webrtc.PeerConnection, error) {
	pc, err := v.api.NewPeerConnection(v.cfg)
	if err != nil {
		return nil, fmt.Errorf("sfutest: new PeerConnection: %w", err)
	}
	pc.OnTrack(func(tr *webrtc.TrackRemote, recv *webrtc.RTPReceiver) { v.onTrack(pc, tr, recv) })
	return pc, nil
}

// Rebuild closes the Viewer's PeerConnection and gives it a new one with the same settings: what a client does when
// the SFU offers a sub PC of a new gen (01 §9 rule 2). The tracks of the new PeerConnection get new Recorders.
func (v *Viewer) Rebuild() error {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return errors.New("sfutest: rebuild of a closed viewer")
	}
	pc, err := v.newPC()
	if err != nil {
		v.mu.Unlock()
		return err
	}
	old := v.pc
	v.pc = pc
	v.mu.Unlock()
	if err := old.Close(); err != nil {
		return fmt.Errorf("sfutest: close the replaced PeerConnection: %w", err)
	}
	return nil
}

// viewerEngine registers H.264 (with RTX unless NoRTX) and Opus with a browser's feedback, and the header extensions
// a browser offers: sdes:mid everywhere, rtp-stream-id and repaired-rtp-stream-id (receiving simulcast from a
// publisher) and abs-send-time (the SFU's REMB needs it) on video.
func viewerEngine(o ViewerOptions) (*webrtc.MediaEngine, error) {
	m := &webrtc.MediaEngine{}
	profiles := o.H264
	if profiles == nil {
		profiles = DefaultH264
	}
	if o.NoH264 {
		profiles = nil
	}
	videoFB := []webrtc.RTCPFeedback{
		{Type: webrtc.TypeRTCPFBGoogREMB}, {Type: webrtc.TypeRTCPFBCCM, Parameter: "fir"},
		{Type: webrtc.TypeRTCPFBNACK}, {Type: webrtc.TypeRTCPFBNACK, Parameter: "pli"},
	}
	for i, plid := range profiles {
		pt := webrtc.PayloadType(96 + 2*i)
		if pt >= opusPT-1 {
			return nil, fmt.Errorf("sfutest: too many H.264 profiles (%d)", len(profiles))
		}
		codecs := []webrtc.RTPCodecParameters{{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
				SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=" + plid,
				RTCPFeedback: videoFB,
			},
			PayloadType: pt,
		}}
		if !o.NoRTX {
			codecs = append(codecs, webrtc.RTPCodecParameters{
				RTPCodecCapability: webrtc.RTPCodecCapability{
					MimeType: webrtc.MimeTypeRTX, ClockRate: 90000, SDPFmtpLine: "apt=" + strconv.Itoa(int(pt)),
				},
				PayloadType: pt + 1,
			})
		}
		for _, c := range codecs {
			if err := m.RegisterCodec(c, webrtc.RTPCodecTypeVideo); err != nil {
				return nil, fmt.Errorf("sfutest: register PT %d: %w", c.PayloadType, err)
			}
		}
	}
	opus := webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
			SDPFmtpLine: "minptime=10;useinbandfec=1;stereo=1", RTCPFeedback: []webrtc.RTCPFeedback{{Type: "nack"}},
		},
		PayloadType: opusPT,
	}
	if err := m.RegisterCodec(opus, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("sfutest: register Opus: %w", err)
	}
	for _, e := range []struct {
		uri  string
		kind webrtc.RTPCodecType
	}{
		{sdp.SDESMidURI, webrtc.RTPCodecTypeVideo}, {sdp.SDESRTPStreamIDURI, webrtc.RTPCodecTypeVideo},
		{sdp.SDESRepairRTPStreamIDURI, webrtc.RTPCodecTypeVideo}, {sdp.ABSSendTimeURI, webrtc.RTPCodecTypeVideo},
		{sdp.SDESMidURI, webrtc.RTPCodecTypeAudio},
	} {
		if err := m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: e.uri}, e.kind); err != nil {
			return nil, fmt.Errorf("sfutest: register header extension %s: %w", e.uri, err)
		}
	}
	return m, nil
}

// LoopbackSettings returns Pion settings for in-process tests: UDP4 host candidates on loopback only, no mDNS.
func LoopbackSettings() webrtc.SettingEngine {
	var se webrtc.SettingEngine
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetIncludeLoopbackCandidate(true)
	se.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	return se
}

// PC returns the Viewer's PeerConnection, for signaling and state callbacks: the current one, which Rebuild
// replaces. Close closes it.
func (v *Viewer) PC() *webrtc.PeerConnection {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.pc
}

// Answer applies a remote offer and returns the answer, set as the local description, with every ICE candidate in it
// (no trickle). An offer with new ICE credentials restarts ICE, as on any WebRTC client.
func (v *Viewer) Answer(ctx context.Context, offer webrtc.SessionDescription) (webrtc.SessionDescription, error) {
	pc := v.PC()
	if err := pc.SetRemoteDescription(offer); err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("sfutest: set remote description: %w", err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("sfutest: create answer: %w", err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("sfutest: set local description: %w", err)
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		return webrtc.SessionDescription{}, ctx.Err()
	}
	return *pc.LocalDescription(), nil
}

// onTrack starts recording a new track of pc: an RTP reader for the track and, once per receiver and rid, an RTCP
// reader.
func (v *Viewer) onTrack(pc *webrtc.PeerConnection, tr *webrtc.TrackRemote, recv *webrtc.RTPReceiver) {
	mid := ""
	for _, t := range pc.GetTransceivers() {
		if t.Receiver() == recv {
			mid = t.Mid()
			break
		}
	}
	rec := newRecorder(tr, mid, v.keep)
	v.log.Debug("sfutest: new track", "kind", rec.kind.String(), "mid", mid, "rid", rec.rid, "ssrc", rec.ssrc)
	key := rtcpKey{recv, tr.RID()}
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return
	}
	v.recs = append(v.recs, rec)
	v.wg.Add(1)
	go v.readRTP(rec, tr)
	if !v.rtcpRead[key] {
		v.rtcpRead[key] = true
		v.wg.Add(1)
		go v.readRTCP(key)
	}
	v.mu.Unlock()
}

// readRTP records the track's packets until it ends. Packets that Pion unwrapped from RTX carry its RTX attributes.
func (v *Viewer) readRTP(rec *Recorder, tr *webrtc.TrackRemote) {
	defer v.wg.Done()
	buf := make([]byte, 1500)
	for {
		n, attrs, err := tr.Read(buf)
		if err != nil {
			return
		}
		var pkt rtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}
		rtx := attrs != nil && attrs.Get(webrtc.AttributeRtxSsrc) != nil
		rec.record(&pkt, time.Now(), rtx)
	}
}

// readRTCP reads one receiver's RTCP (per rid for simulcast, S4 finding 2), which also runs the receiver-report
// interceptor, and records sender reports on the Recorder of their SSRC.
func (v *Viewer) readRTCP(key rtcpKey) {
	defer v.wg.Done()
	buf := make([]byte, 1500)
	for {
		var n int
		var err error
		if key.rid != "" {
			n, _, err = key.recv.ReadSimulcast(buf, key.rid)
		} else {
			n, _, err = key.recv.Read(buf)
		}
		if err != nil {
			return
		}
		pkts, err := rtcp.Unmarshal(buf[:n])
		if err != nil {
			continue
		}
		now := time.Now()
		for _, p := range pkts {
			sr, ok := p.(*rtcp.SenderReport)
			if !ok {
				continue
			}
			if rec := v.bySSRC(sr.SSRC); rec != nil {
				rec.recordSR(sr, now)
			}
		}
	}
}

// bySSRC returns the newest Recorder of an SSRC.
func (v *Viewer) bySSRC(ssrc uint32) *Recorder {
	v.mu.Lock()
	defer v.mu.Unlock()
	for i := len(v.recs) - 1; i >= 0; i-- {
		if v.recs[i].ssrc == ssrc {
			return v.recs[i]
		}
	}
	return nil
}

// Recorders returns the Recorders in the order their tracks arrived.
func (v *Viewer) Recorders() []*Recorder {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.recs)
}

// WaitRecorder waits for a Recorder that match accepts and returns the first one.
func (v *Viewer) WaitRecorder(ctx context.Context, match func(*Recorder) bool) (*Recorder, error) {
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		for _, r := range v.Recorders() {
			if match(r) {
				return r, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("sfutest: no matching track: %w", ctx.Err())
		case <-tick.C:
		}
	}
}

// RequestKeyframe sends a PLI for the Recorder's SSRC.
func (v *Viewer) RequestKeyframe(r *Recorder) error {
	if err := v.PC().WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: r.ssrc}}); err != nil {
		return fmt.Errorf("sfutest: PLI: %w", err)
	}
	r.mu.Lock()
	r.stats.PLIs++
	r.mu.Unlock()
	return nil
}

// SendREMB sends one REMB with a fixed estimate (bps) naming every video SSRC received so far.
func (v *Viewer) SendREMB(bps float32) error {
	var ssrcs []uint32
	for _, r := range v.Recorders() {
		if r.kind == webrtc.RTPCodecTypeVideo && !slices.Contains(ssrcs, r.ssrc) {
			ssrcs = append(ssrcs, r.ssrc)
		}
	}
	if len(ssrcs) == 0 {
		return errors.New("sfutest: REMB: no video track yet")
	}
	remb := &rtcp.ReceiverEstimatedMaximumBitrate{Bitrate: bps, SSRCs: ssrcs}
	if err := v.PC().WriteRTCP([]rtcp.Packet{remb}); err != nil {
		return fmt.Errorf("sfutest: REMB: %w", err)
	}
	return nil
}

// Close closes the PeerConnection and waits for the readers. It is idempotent.
func (v *Viewer) Close() error {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return nil
	}
	v.closed = true
	pc := v.pc
	v.mu.Unlock()
	err := pc.Close()
	v.wg.Wait()
	if err != nil {
		return fmt.Errorf("sfutest: close viewer: %w", err)
	}
	return nil
}
