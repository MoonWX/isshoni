package main

import (
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

// pliInterval throttles keyframe requests sent to a publisher, per layer.
const pliInterval = 500 * time.Millisecond

// Layer is one incoming RTP stream of a share: a simulcast video layer or the audio.
type Layer struct {
	rid   string
	kind  webrtc.RTPCodecType
	track *webrtc.TrackRemote
	ssrc  uint32

	lastPLI atomic.Int64 // unix nanos
	packets atomic.Uint64
	bytes   atomic.Uint64
}

// Share is one participant's screen share: simulcast video layers + optional audio.
type Share struct {
	id    string
	owner *Peer

	mu         sync.RWMutex
	layers     map[string]*Layer // video, by rid
	audio      *Layer
	videoCodec webrtc.RTPCodecCapability
	downtracks map[*DownTrack]struct{}
	closed     bool
}

func newShare(id string, owner *Peer) *Share {
	return &Share{id: id, owner: owner, layers: map[string]*Layer{}, downtracks: map[*DownTrack]struct{}{}}
}

func (s *Share) addLayer(track *webrtc.TrackRemote) {
	l := &Layer{rid: track.RID(), kind: track.Kind(), track: track, ssrc: uint32(track.SSRC())}
	s.mu.Lock()
	if l.kind == webrtc.RTPCodecTypeAudio {
		s.audio = l
	} else {
		s.layers[l.rid] = l
		s.videoCodec = track.Codec().RTPCodecCapability
	}
	dts := s.downTrackList()
	s.mu.Unlock()

	slog.Info("layer added", "share", s.id, "kind", l.kind, "rid", l.rid, "codec", track.Codec().MimeType,
		"fmtp", track.Codec().SDPFmtpLine)
	if l.kind == webrtc.RTPCodecTypeVideo && !strings.EqualFold(track.Codec().MimeType, webrtc.MimeTypeH264) {
		slog.Warn("publisher did not send H.264; the spike can't switch layers for it", "share", s.id)
	}
	for _, d := range dts {
		d.refresh()
	}
	go s.readLoop(l)
}

func (s *Share) readLoop(l *Layer) {
	isVideo := l.kind == webrtc.RTPCodecTypeVideo
	for {
		pkt, _, err := l.track.ReadRTP()
		if err != nil {
			s.removeLayer(l)
			return
		}
		if len(pkt.Payload) == 0 {
			continue // padding-only (bandwidth probing)
		}
		l.packets.Add(1)
		l.bytes.Add(uint64(len(pkt.Payload)))
		fp := fwdPacket{rid: l.rid, pkt: pkt, keyframe: !isVideo || isH264KeyframeStart(pkt.Payload), at: time.Now()}
		s.mu.RLock()
		for d := range s.downtracks {
			if d.kind == l.kind {
				d.enqueue(fp)
			}
		}
		s.mu.RUnlock()
	}
}

func (s *Share) removeLayer(l *Layer) {
	s.mu.Lock()
	if l.kind == webrtc.RTPCodecTypeAudio {
		if s.audio == l {
			s.audio = nil
		}
	} else if s.layers[l.rid] == l {
		delete(s.layers, l.rid)
	}
	noVideo := len(s.layers) == 0
	s.mu.Unlock()
	if noVideo {
		s.owner.endShare(s)
	}
}

// resolve maps a subscriber's quality wish to a concrete layer rid.
func (s *Share) resolve(kind webrtc.RTPCodecType, quality string) (rid string, active bool) {
	if kind == webrtc.RTPCodecTypeAudio {
		return "", quality != "off"
	}
	if quality == "off" {
		return "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.layers) == 0 {
		return "", false
	}
	prefs := []string{"f", "h", "", "q"} // high
	if quality == "low" {
		prefs = []string{"q", "h", "", "f"}
	}
	for _, p := range prefs {
		if _, ok := s.layers[p]; ok {
			return p, true
		}
	}
	for rid := range s.layers {
		return rid, true
	}
	return "", false
}

// requestKeyframe asks the publisher for a keyframe on one layer, at most once per pliInterval.
func (s *Share) requestKeyframe(rid string) {
	s.mu.RLock()
	l := s.layers[rid]
	s.mu.RUnlock()
	if l == nil {
		return
	}
	now := time.Now().UnixNano()
	last := l.lastPLI.Load()
	if now-last < int64(pliInterval) || !l.lastPLI.CompareAndSwap(last, now) {
		return
	}
	if pc := s.owner.pubPC(); pc != nil {
		_ = pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: l.ssrc}})
	}
}

func (s *Share) addDownTrack(d *DownTrack) {
	s.mu.Lock()
	s.downtracks[d] = struct{}{}
	s.mu.Unlock()
}

func (s *Share) removeDownTrack(d *DownTrack) {
	s.mu.Lock()
	delete(s.downtracks, d)
	s.mu.Unlock()
	d.close()
}

// downTrackList must be called with s.mu held.
func (s *Share) downTrackList() []*DownTrack {
	out := make([]*DownTrack, 0, len(s.downtracks))
	for d := range s.downtracks {
		out = append(out, d)
	}
	return out
}

type shareInfo struct {
	ID      string   `json:"id"`
	PeerID  string   `json:"peerId"`
	Name    string   `json:"name"`
	Layers  []string `json:"layers"`
	Audio   bool     `json:"audio"`
	Codec   string   `json:"codec"`
	Profile string   `json:"profile"`
}

func (s *Share) info() shareInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	layers := make([]string, 0, len(s.layers))
	for rid := range s.layers {
		layers = append(layers, rid)
	}
	slices.Sort(layers)
	return shareInfo{
		ID: s.id, PeerID: s.owner.id, Name: s.owner.name, Layers: layers, Audio: s.audio != nil,
		Codec: s.videoCodec.MimeType + " " + s.videoCodec.SDPFmtpLine, Profile: h264ProfileName(s.videoCodec.SDPFmtpLine),
	}
}

type layerStats struct {
	RID     string `json:"rid"`
	Kind    string `json:"kind"`
	Packets uint64 `json:"packets"`
	Bytes   uint64 `json:"bytes"`
}

func (s *Share) layerStats() []layerStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []layerStats
	add := func(l *Layer) {
		out = append(out, layerStats{RID: l.rid, Kind: l.kind.String(), Packets: l.packets.Load(), Bytes: l.bytes.Load()})
	}
	for _, l := range s.layers {
		add(l)
	}
	if s.audio != nil {
		add(s.audio)
	}
	return out
}
