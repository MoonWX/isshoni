package main

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/pion/webrtc/v4"
)

// message is the spike's signaling envelope (JSON over WebSocket).
type message struct {
	Type      string                   `json:"type"`
	ID        string                   `json:"id,omitempty"`
	Name      string                   `json:"name,omitempty"`
	SDP       string                   `json:"sdp,omitempty"`
	PC        string                   `json:"pc,omitempty"` // "pub" | "sub"
	Candidate *webrtc.ICECandidateInit `json:"candidate,omitempty"`
	Share     string                   `json:"share,omitempty"`
	Video     string                   `json:"video,omitempty"` // high | low | off
	Audio     *bool                    `json:"audio,omitempty"`
	State     *roomState               `json:"state,omitempty"`
	Stats     []downTrackStats         `json:"stats,omitempty"`
	Layers    []layerStats             `json:"layers,omitempty"`
	Error     string                   `json:"error,omitempty"`
}

type subscription struct {
	share        *Share
	video, audio *DownTrack
}

// Peer is one connected client. It has up to two PeerConnections: "pub" (the
// client offers; carries its share) and "sub" (the server offers; carries the
// shares it watches).
type Peer struct {
	id, name string
	sfu      *SFU
	conn     *websocket.Conn
	ctx      context.Context
	cancel   context.CancelFunc
	writeMu  sync.Mutex

	// subReady is set once the subscribe PC's DTLS is up. Pion silently drops
	// RTP written before that, so DownTracks must not start (and consume a
	// keyframe) until then.
	subReady atomic.Bool

	mu          sync.Mutex
	pub, sub    *webrtc.PeerConnection
	share       *Share
	subs        map[string]*subscription
	pendingICE  map[string][]webrtc.ICECandidateInit
	negotiating bool
	renegotiate bool
	closed      bool
}

func newPeer(s *SFU, c *websocket.Conn, parent context.Context) *Peer {
	ctx, cancel := context.WithCancel(parent)
	return &Peer{
		id: s.newID("p"), sfu: s, conn: c, ctx: ctx, cancel: cancel,
		subs: map[string]*subscription{}, pendingICE: map[string][]webrtc.ICECandidateInit{},
	}
}

func (p *Peer) send(m message) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, p.conn, m); err != nil {
		p.cancel()
	}
}

func (p *Peer) sendError(err string) { p.send(message{Type: "error", Error: err}) }

func (p *Peer) handle(m message) {
	switch m.Type {
	case "join":
		p.name = m.Name
		p.sfu.addPeer(p)
		p.send(message{Type: "welcome", ID: p.id})
		p.sfu.broadcastState()
	case "pub.offer":
		p.onPubOffer(m.SDP)
	case "sub.answer":
		p.onSubAnswer(m.SDP)
	case "ice":
		if m.Candidate != nil {
			p.addCandidate(m.PC, *m.Candidate)
		}
	case "subscribe":
		p.subscribe(m.Share, m.Video, m.Audio)
	case "unshare":
		p.mu.Lock()
		sh := p.share
		p.mu.Unlock()
		if sh != nil {
			p.endShare(sh)
		}
	default:
		p.sendError("unknown message type " + m.Type)
	}
}

func (p *Peer) newPC(api *webrtc.API, name string) (*webrtc.PeerConnection, error) {
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, err
	}
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			init := c.ToJSON()
			p.send(message{Type: "ice", PC: name, Candidate: &init})
		}
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		slog.Info("pc state", "peer", p.id, "pc", name, "state", s)
	})
	return pc, nil
}

func (p *Peer) pubPC() *webrtc.PeerConnection {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pub
}

func (p *Peer) onPubOffer(sdp string) {
	p.mu.Lock()
	pc := p.pub
	p.mu.Unlock()
	if pc == nil {
		var err error
		if pc, err = p.newPC(p.sfu.pubAPI, "pub"); err != nil {
			p.sendError(err.Error())
			return
		}
		pc.OnTrack(func(track *webrtc.TrackRemote, recv *webrtc.RTPReceiver) {
			go func() { // keep RTCP interceptors (reports, NACK) running
				for {
					var err error
					if rid := track.RID(); rid != "" {
						_, _, err = recv.ReadSimulcastRTCP(rid)
					} else {
						_, _, err = recv.ReadRTCP()
					}
					if err != nil {
						return
					}
				}
			}()
			p.ensureShare().addLayer(track)
			p.sfu.broadcastState()
		})
		p.mu.Lock()
		p.pub = pc
		p.mu.Unlock()
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
		p.sendError("pub offer: " + err.Error())
		return
	}
	p.flushICE("pub", pc)
	answer, err := pc.CreateAnswer(nil)
	if err == nil {
		err = pc.SetLocalDescription(answer)
	}
	if err != nil {
		p.sendError("pub answer: " + err.Error())
		return
	}
	p.send(message{Type: "pub.answer", SDP: answer.SDP})
}

func (p *Peer) ensureShare() *Share {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.share == nil {
		p.share = newShare(p.sfu.newID("s"), p)
		p.sfu.addShare(p.share)
	}
	return p.share
}

// endShare stops this peer's share (client asked, or its video ended).
func (p *Peer) endShare(sh *Share) {
	p.mu.Lock()
	if p.share != sh {
		p.mu.Unlock()
		return
	}
	p.share = nil
	pub := p.pub
	p.pub = nil
	delete(p.pendingICE, "pub")
	p.mu.Unlock()
	if pub != nil {
		_ = pub.Close()
	}
	p.sfu.removeShare(sh)
}

func (p *Peer) ensureSub() (*webrtc.PeerConnection, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sub != nil {
		return p.sub, nil
	}
	pc, err := p.newPC(p.sfu.subAPI, "sub")
	if err != nil {
		return nil, err
	}
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		slog.Info("pc state", "peer", p.id, "pc", "sub", "state", s)
		ready := s == webrtc.PeerConnectionStateConnected
		if p.subReady.Swap(ready) == ready || !ready {
			return
		}
		p.mu.Lock()
		var videos []*DownTrack
		for _, sub := range p.subs {
			videos = append(videos, sub.video)
		}
		p.mu.Unlock()
		for _, d := range videos {
			d.share.requestKeyframe(d.targetRID())
		}
	})
	p.sub = pc
	return pc, nil
}

func (p *Peer) subscribe(shareID, video string, audio *bool) {
	sh := p.sfu.getShare(shareID)
	if sh == nil {
		p.sendError("no such share " + shareID)
		return
	}
	pc, err := p.ensureSub()
	if err != nil {
		p.sendError(err.Error())
		return
	}
	sh.mu.RLock()
	videoCodec := sh.videoCodec
	sh.mu.RUnlock()

	p.mu.Lock()
	sub := p.subs[shareID]
	created := sub == nil
	if created {
		sub = &subscription{
			share: sh,
			video: newDownTrack(sh, p, webrtc.RTPCodecTypeVideo, videoCodec),
			audio: newDownTrack(sh, p, webrtc.RTPCodecTypeAudio, opusCapability),
		}
		p.subs[shareID] = sub
	}
	p.mu.Unlock()

	if video == "" && created {
		video = "low"
	}
	if video != "" {
		sub.video.SetQuality(video)
	}
	if audio != nil {
		sub.audio.SetQuality(map[bool]string{true: "on", false: "off"}[*audio])
	} else if created {
		sub.audio.SetQuality("off")
	}
	if !created {
		return
	}
	for _, d := range []*DownTrack{sub.video, sub.audio} {
		sender, err := pc.AddTrack(d)
		if err != nil {
			p.sendError("add track: " + err.Error())
			return
		}
		d.sender = sender
		go d.readRTCP()
		sh.addDownTrack(d)
	}
	p.negotiateSub()
}

// unsubscribe drops this peer's subscription to a share (the share ended).
func (p *Peer) unsubscribe(shareID string) {
	p.mu.Lock()
	sub := p.subs[shareID]
	delete(p.subs, shareID)
	pc := p.sub
	closed := p.closed
	p.mu.Unlock()
	if sub == nil {
		return
	}
	for _, d := range []*DownTrack{sub.video, sub.audio} {
		sub.share.removeDownTrack(d)
		if pc != nil && d.sender != nil && !closed {
			_ = pc.RemoveTrack(d.sender)
		}
	}
	p.negotiateSub()
}

// negotiateSub sends a fresh server offer on the subscribe PC; changes made
// while an offer is outstanding are folded into one follow-up offer.
func (p *Peer) negotiateSub() {
	p.mu.Lock()
	if p.closed || p.sub == nil {
		p.mu.Unlock()
		return
	}
	if p.negotiating {
		p.renegotiate = true
		p.mu.Unlock()
		return
	}
	p.negotiating = true
	pc := p.sub
	p.mu.Unlock()

	offer, err := pc.CreateOffer(nil)
	if err == nil {
		err = pc.SetLocalDescription(offer)
	}
	if err != nil {
		slog.Error("sub offer", "peer", p.id, "err", err)
		p.mu.Lock()
		p.negotiating = false
		p.mu.Unlock()
		return
	}
	p.send(message{Type: "sub.offer", SDP: offer.SDP})
}

func (p *Peer) onSubAnswer(sdp string) {
	p.mu.Lock()
	pc := p.sub
	p.mu.Unlock()
	if pc == nil {
		return
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp}); err != nil {
		p.sendError("sub answer: " + err.Error())
	}
	p.flushICE("sub", pc)
	p.mu.Lock()
	p.negotiating = false
	again := p.renegotiate
	p.renegotiate = false
	p.mu.Unlock()
	if again {
		p.negotiateSub()
	}
}

// addCandidate adds a trickled candidate, buffering it until the remote
// description exists (candidates can overtake the SDP they belong to).
func (p *Peer) addCandidate(name string, c webrtc.ICECandidateInit) {
	p.mu.Lock()
	pc := p.pub
	if name == "sub" {
		pc = p.sub
	}
	if pc == nil || pc.RemoteDescription() == nil {
		p.pendingICE[name] = append(p.pendingICE[name], c)
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	if err := pc.AddICECandidate(c); err != nil {
		slog.Warn("add candidate", "peer", p.id, "pc", name, "err", err)
	}
}

func (p *Peer) flushICE(name string, pc *webrtc.PeerConnection) {
	p.mu.Lock()
	pending := p.pendingICE[name]
	delete(p.pendingICE, name)
	p.mu.Unlock()
	for _, c := range pending {
		_ = pc.AddICECandidate(c)
	}
}

func (p *Peer) statsLoop() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-t.C:
		}
		p.mu.Lock()
		var st []downTrackStats
		for _, sub := range p.subs {
			st = append(st, sub.video.stats(), sub.audio.stats())
		}
		sh := p.share
		p.mu.Unlock()
		m := message{Type: "stats", Stats: st}
		if sh != nil {
			m.Layers = sh.layerStats()
		}
		p.send(m)
	}
}

func (p *Peer) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	sh, sub := p.share, p.sub
	subs := p.subs
	p.subs = map[string]*subscription{}
	p.mu.Unlock()

	p.cancel()
	for _, s := range subs {
		s.share.removeDownTrack(s.video)
		s.share.removeDownTrack(s.audio)
	}
	if sh != nil {
		p.endShare(sh)
	}
	if sub != nil {
		_ = sub.Close()
	}
	p.sfu.removePeer(p)
	p.sfu.broadcastState()
	_ = p.conn.CloseNow()
}
