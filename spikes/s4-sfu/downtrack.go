package main

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

type fwdPacket struct {
	rid      string
	pkt      *rtp.Packet // shared between DownTracks: read-only
	keyframe bool
	at       time.Time
}

// DownTrack forwards one share's video (or audio) to one subscriber. It is a
// webrtc.TrackLocal with its own queue and goroutine so a slow subscriber can't
// stall the publisher's read loop or other subscribers.
type DownTrack struct {
	id, streamID string
	kind         webrtc.RTPCodecType
	codec        webrtc.RTPCodecCapability // what the publisher sends
	share        *Share
	subscriber   *Peer
	sender       *webrtc.RTPSender

	queue     chan fwdPacket
	done      chan struct{}
	closeOnce sync.Once

	mu      sync.Mutex
	writer  webrtc.TrackLocalWriter
	ssrc    webrtc.SSRC
	pt      webrtc.PayloadType
	match   string // exact | partial (profile differs) | none
	fmtp    string // negotiated with the subscriber
	quality string // video: high|low|off; audio: on|off
	m       munger

	sent, dropped atomic.Uint64
}

func newDownTrack(share *Share, subscriber *Peer, kind webrtc.RTPCodecType, codec webrtc.RTPCodecCapability) *DownTrack {
	prefix := "v-"
	if kind == webrtc.RTPCodecTypeAudio {
		prefix = "a-"
	}
	d := &DownTrack{
		id:         prefix + share.id,
		streamID:   share.id,
		kind:       kind,
		codec:      codec,
		share:      share,
		subscriber: subscriber,
		queue:      make(chan fwdPacket, 1024),
		done:       make(chan struct{}),
		m:          munger{clockRate: codec.ClockRate},
	}
	go d.run()
	return d
}

// Bind implements webrtc.TrackLocal.
func (d *DownTrack) Bind(ctx webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	codec, match := matchCodec(d.codec, ctx.CodecParameters())
	if match == "none" {
		return webrtc.RTPCodecParameters{}, webrtc.ErrUnsupportedCodec
	}
	d.mu.Lock()
	d.writer, d.ssrc, d.pt, d.match, d.fmtp = ctx.WriteStream(), ctx.SSRC(), codec.PayloadType, match, codec.SDPFmtpLine
	d.mu.Unlock()
	if d.kind == webrtc.RTPCodecTypeVideo {
		d.share.requestKeyframe(d.targetRID())
	}
	return codec, nil
}

// Unbind implements webrtc.TrackLocal.
func (d *DownTrack) Unbind(webrtc.TrackLocalContext) error {
	d.mu.Lock()
	d.writer = nil
	d.mu.Unlock()
	return nil
}

func (d *DownTrack) ID() string                { return d.id }
func (d *DownTrack) RID() string               { return "" }
func (d *DownTrack) StreamID() string          { return d.streamID }
func (d *DownTrack) Kind() webrtc.RTPCodecType { return d.kind }

func (d *DownTrack) targetRID() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.m.target
}

// SetQuality sets what the subscriber wants: high|low|off for video, on|off for audio.
func (d *DownTrack) SetQuality(q string) {
	d.mu.Lock()
	d.quality = q
	d.mu.Unlock()
	d.refresh()
}

// refresh re-resolves the target layer, e.g. after the publisher's layers changed.
func (d *DownTrack) refresh() {
	d.mu.Lock()
	rid, active := d.share.resolve(d.kind, d.quality)
	changed := d.m.setTarget(rid, active)
	waiting := d.m.waitingForKeyframe()
	d.mu.Unlock()
	if changed && waiting && d.kind == webrtc.RTPCodecTypeVideo {
		d.share.requestKeyframe(rid)
	}
}

func (d *DownTrack) enqueue(p fwdPacket) {
	select {
	case d.queue <- p:
	default:
		d.dropped.Add(1)
	}
}

func (d *DownTrack) run() {
	for {
		select {
		case <-d.done:
			return
		case p := <-d.queue:
			d.write(p)
		}
	}
}

func (d *DownTrack) write(p fwdPacket) {
	if !d.subscriber.subReady.Load() {
		return
	}
	d.mu.Lock()
	w := d.writer
	if w == nil {
		d.mu.Unlock()
		return
	}
	seq, ts, ok := d.m.process(p.rid, p.pkt.SequenceNumber, p.pkt.Timestamp, p.keyframe, p.at)
	waitingOnThis := !ok && d.m.waitingForKeyframe() && p.rid == d.m.target
	ssrc, pt := d.ssrc, d.pt
	d.mu.Unlock()

	if waitingOnThis && d.kind == webrtc.RTPCodecTypeVideo {
		d.share.requestKeyframe(p.rid) // throttled; self-heals a lost keyframe request
	}
	if !ok {
		return
	}
	// Publisher header extensions carry the publisher's negotiated IDs (mid, rid,
	// transport-cc…); they must never leak to the subscriber, so start clean.
	hdr := rtp.Header{
		Version:        2,
		Marker:         p.pkt.Marker,
		PayloadType:    uint8(pt),
		SequenceNumber: seq,
		Timestamp:      ts,
		SSRC:           uint32(ssrc),
	}
	if _, err := w.WriteRTP(&hdr, p.pkt.Payload); err == nil {
		d.sent.Add(1)
	}
}

// readRTCP handles feedback from the subscriber for this track.
func (d *DownTrack) readRTCP() {
	for {
		pkts, _, err := d.sender.ReadRTCP()
		if err != nil {
			return
		}
		for _, pkt := range pkts {
			switch pkt.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				d.mu.Lock()
				rid, ok := d.m.current, d.m.hasCurrent
				d.mu.Unlock()
				if ok {
					d.share.requestKeyframe(rid)
				}
			}
		}
	}
}

func (d *DownTrack) close() {
	d.closeOnce.Do(func() { close(d.done) })
}

type downTrackStats struct {
	Share    string `json:"share"`
	Kind     string `json:"kind"`
	Quality  string `json:"quality"`
	Target   string `json:"target"`
	Current  string `json:"current"`
	Switches int    `json:"switches"`
	Sent     uint64 `json:"sent"`
	Dropped  uint64 `json:"dropped"`
	Match    string `json:"match"`
	Fmtp     string `json:"fmtp"`
	Profile  string `json:"profile,omitempty"`
}

func (d *DownTrack) stats() downTrackStats {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := downTrackStats{
		Share: d.share.id, Kind: d.kind.String(), Quality: d.quality,
		Target: d.m.target, Switches: d.m.switches,
		Sent: d.sent.Load(), Dropped: d.dropped.Load(), Match: d.match, Fmtp: d.fmtp,
	}
	if d.m.hasCurrent {
		s.Current = d.m.current
	}
	if d.kind == webrtc.RTPCodecTypeVideo {
		s.Profile = h264ProfileName(d.fmtp)
	}
	return s
}

// matchCodec finds the subscriber-negotiated codec to send the publisher's
// codec as. "partial" means same codec but a different H.264 profile: the spike
// forwards anyway and reports it, to learn whether decoders tolerate it.
func matchCodec(want webrtc.RTPCodecCapability, have []webrtc.RTPCodecParameters) (webrtc.RTPCodecParameters, string) {
	var partial *webrtc.RTPCodecParameters
	for i, c := range have {
		if !strings.EqualFold(c.MimeType, want.MimeType) {
			continue
		}
		if !strings.EqualFold(want.MimeType, webrtc.MimeTypeH264) {
			return c, "exact"
		}
		wantProfile, wantMode := h264FmtpKey(want.SDPFmtpLine)
		haveProfile, haveMode := h264FmtpKey(c.SDPFmtpLine)
		if wantMode != haveMode {
			continue
		}
		if wantProfile == haveProfile {
			return c, "exact"
		}
		if partial == nil {
			partial = &have[i]
		}
	}
	if partial != nil {
		return *partial, "partial"
	}
	return webrtc.RTPCodecParameters{}, "none"
}
