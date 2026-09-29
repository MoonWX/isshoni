package main

import (
	"fmt"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
)

// SFU is a single-room screen-share forwarder. Publishers and subscribers use
// separate webrtc.API instances (different codecs/interceptors) that share one
// ICE UDP mux, so all media uses a single UDP port.
type SFU struct {
	pubAPI, subAPI *webrtc.API
	nextID         atomic.Uint64

	mu     sync.Mutex
	peers  map[string]*Peer
	shares map[string]*Share
}

var opusCapability = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}

// NewSFU builds the publish and subscribe APIs on top of one UDP mux.
func NewSFU(mux ice.UDPMux, includeLoopback bool) (*SFU, error) {
	var se webrtc.SettingEngine
	se.SetICEUDPMux(mux)
	se.SetIncludeLoopbackCandidate(includeLoopback)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})

	// Publish side: the answer's maxaveragebitrate is the Opus target the browser encodes at.
	pubME, err := newMediaEngine("minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1;maxaveragebitrate=128000")
	if err != nil {
		return nil, err
	}
	pubIR := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(pubME, pubIR); err != nil { // NACK gen, reports, simulcast exts, TWCC
		return nil, err
	}

	// Subscribe side: no transport-cc (v1 downlink adaptation will use REMB/loss).
	subME, err := newMediaEngine("minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1;maxaveragebitrate=510000")
	if err != nil {
		return nil, err
	}
	subIR := &interceptor.Registry{}
	if err := webrtc.ConfigureNack(subME, subIR); err != nil {
		return nil, err
	}
	if err := webrtc.ConfigureRTCPReports(subIR); err != nil {
		return nil, err
	}

	return &SFU{
		pubAPI: webrtc.NewAPI(webrtc.WithMediaEngine(pubME), webrtc.WithInterceptorRegistry(pubIR), webrtc.WithSettingEngine(se)),
		subAPI: webrtc.NewAPI(webrtc.WithMediaEngine(subME), webrtc.WithInterceptorRegistry(subIR), webrtc.WithSettingEngine(se)),
		peers:  map[string]*Peer{},
		shares: map[string]*Share{},
	}, nil
}

// h264Variants are the H.264 flavours offered, so the spike can observe which
// profiles each browser encodes and decodes. Payload type pairs: (codec, rtx).
var h264Variants = []struct {
	pt, rtx webrtc.PayloadType
	plid    string
}{
	{96, 97, "42e01f"},   // constrained baseline
	{98, 99, "42001f"},   // baseline
	{100, 101, "4d001f"}, // main
	{102, 103, "640c1f"}, // constrained high
	{104, 105, "64001f"}, // high
}

func newMediaEngine(opusFmtp string) (*webrtc.MediaEngine, error) {
	m := &webrtc.MediaEngine{}
	videoFB := []webrtc.RTCPFeedback{
		{Type: "goog-remb"}, {Type: "ccm", Parameter: "fir"}, {Type: "nack"}, {Type: "nack", Parameter: "pli"},
	}
	for _, v := range h264Variants {
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
				SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=" + v.plid,
				RTCPFeedback: videoFB,
			},
			PayloadType: v.pt,
		}, webrtc.RTPCodecTypeVideo); err != nil {
			return nil, err
		}
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeRTX, ClockRate: 90000, SDPFmtpLine: fmt.Sprintf("apt=%d", v.pt),
			},
			PayloadType: v.rtx,
		}, webrtc.RTPCodecTypeVideo); err != nil {
			return nil, err
		}
	}
	opus := opusCapability
	opus.SDPFmtpLine = opusFmtp
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: opus, PayloadType: 111},
		webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	return m, nil
}

func (s *SFU) newID(prefix string) string {
	return fmt.Sprintf("%s%d", prefix, s.nextID.Add(1))
}

// ServeWS runs the signaling session for one client.
func (s *SFU) ServeWS(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil) // same-origin only
	if err != nil {
		return
	}
	c.SetReadLimit(4 << 20)
	p := newPeer(s, c, r.Context())
	defer p.close()
	go p.statsLoop()
	for {
		var m message
		if err := wsjson.Read(p.ctx, c, &m); err != nil {
			return
		}
		p.handle(m)
	}
}

func (s *SFU) addPeer(p *Peer) {
	s.mu.Lock()
	s.peers[p.id] = p
	s.mu.Unlock()
}

func (s *SFU) removePeer(p *Peer) {
	s.mu.Lock()
	delete(s.peers, p.id)
	s.mu.Unlock()
}

func (s *SFU) addShare(sh *Share) {
	s.mu.Lock()
	s.shares[sh.id] = sh
	s.mu.Unlock()
}

func (s *SFU) getShare(id string) *Share {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shares[id]
}

// removeShare unregisters a share and tears down every subscription to it.
func (s *SFU) removeShare(sh *Share) {
	s.mu.Lock()
	delete(s.shares, sh.id)
	s.mu.Unlock()

	sh.mu.Lock()
	sh.closed = true
	dts := sh.downTrackList()
	sh.mu.Unlock()

	seen := map[*Peer]bool{}
	for _, d := range dts {
		if !seen[d.subscriber] {
			seen[d.subscriber] = true
			d.subscriber.unsubscribe(sh.id)
		}
	}
	s.broadcastState()
}

type peerInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type roomState struct {
	Peers  []peerInfo  `json:"peers"`
	Shares []shareInfo `json:"shares"`
}

func (s *SFU) state() roomState {
	s.mu.Lock()
	peers := make([]*Peer, 0, len(s.peers))
	for _, p := range s.peers {
		peers = append(peers, p)
	}
	shares := make([]*Share, 0, len(s.shares))
	for _, sh := range s.shares {
		shares = append(shares, sh)
	}
	s.mu.Unlock()

	st := roomState{Peers: []peerInfo{}, Shares: []shareInfo{}}
	for _, p := range peers {
		st.Peers = append(st.Peers, peerInfo{ID: p.id, Name: p.name})
	}
	for _, sh := range shares {
		if info := sh.info(); len(info.Layers) > 0 { // announce once video is flowing
			st.Shares = append(st.Shares, info)
		}
	}
	slices.SortFunc(st.Peers, func(a, b peerInfo) int { return compareIDs(a.ID, b.ID) })
	slices.SortFunc(st.Shares, func(a, b shareInfo) int { return compareIDs(a.ID, b.ID) })
	return st
}

func compareIDs(a, b string) int {
	if len(a) != len(b) {
		return len(a) - len(b)
	}
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func (s *SFU) broadcastState() {
	st := s.state()
	s.mu.Lock()
	peers := make([]*Peer, 0, len(s.peers))
	for _, p := range s.peers {
		peers = append(peers, p)
	}
	s.mu.Unlock()
	for _, p := range peers {
		p.send(message{Type: "state", State: &st})
	}
}
