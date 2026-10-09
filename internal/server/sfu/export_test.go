package sfu

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/pion/webrtc/v4"
)

// This file gives the tests in package sfu_test (the ones that use sfutest.Harness, which imports this package) a
// look at the state a Conn's actor owns.

// PubTrackState describes one incoming track of a Conn's pub PC.
type PubTrackState struct {
	MID, RID string
	Kind     webrtc.RTPCodecType
	Share    ShareID // "" while the track feeds no share
	Packets  uint64  // RTP packets read so far
}

// PubTracks returns the tracks Pion has delivered on the Conn's current pub PC, sorted by mid and rid.
func (c *Conn) PubTracks(ctx context.Context) ([]PubTrackState, error) {
	var out []PubTrackState
	err := c.do(ctx, func(context.Context) error {
		if c.pub == nil {
			return nil
		}
		for key, t := range c.pub.tracks {
			st := PubTrackState{MID: key.mid, RID: key.rid, Kind: t.kind, Packets: t.packets.Load()}
			if l := t.layer.Load(); l != nil {
				st.Share = l.share.id
			}
			out = append(out, st)
		}
		return nil
	})
	slices.SortFunc(out, func(a, b PubTrackState) int {
		return strings.Compare(a.MID+"/"+a.RID, b.MID+"/"+b.RID)
	})
	return out, err
}

// PubStuck reports whether the Conn's current pub PC is stuck: an offer failed inside Pion after Pion had taken it,
// so the PC takes no other one. It is false without a pub PC.
func (c *Conn) PubStuck(ctx context.Context) (bool, error) {
	var stuck bool
	err := c.do(ctx, func(context.Context) error {
		stuck = c.pub != nil && c.pub.stuck
		return nil
	})
	return stuck, err
}

// SubscriptionState describes one subscription of a Conn.
type SubscriptionState struct {
	Share ShareID
	Video Quality
	Audio bool
	// Forwarded, AudioForwarded and Reason are what the subscription gets now, as a SubscriptionStateEvent says it;
	// Layer is the rid of the video layer forwarded, "" when none is.
	Forwarded      Quality
	AudioForwarded bool
	Reason         SubReason
	Layer          string
	// VideoBound and AudioBound report whether Pion has bound the DownTrack to a negotiated sender.
	VideoBound, AudioBound bool
	// VideoPTs maps the H.264 profiles the viewer can receive to its payload types (the video binding's ptFor).
	VideoPTs map[ProfileKey]uint8
	// VideoSent and AudioSent count the RTP packets written to the viewer; VideoDrops and AudioDrops those a full
	// DownTrack queue dropped.
	VideoSent, AudioSent, VideoDrops, AudioDrops uint64
}

// Subscriptions returns the Conn's subscriptions, sorted by share id.
func (c *Conn) Subscriptions(ctx context.Context) ([]SubscriptionState, error) {
	var out []SubscriptionState
	err := c.do(ctx, func(context.Context) error {
		for id, sub := range c.subs {
			st := SubscriptionState{Share: id, Video: sub.reqVideo, Audio: sub.reqAudio}
			now := sub.state()
			st.Forwarded, st.AudioForwarded, st.Reason = now.forwarded, now.audio, now.reason
			sub.video.mu.Lock()
			if slot, ok := sub.video.m.current(); ok {
				st.Layer = slot.String()
			}
			sub.video.mu.Unlock()
			if b := sub.video.binding.Load(); b != nil {
				st.VideoBound, st.VideoPTs = true, b.ptFor
			}
			st.AudioBound = sub.audio.binding.Load() != nil
			st.VideoSent, st.VideoDrops = sub.video.stats.packets.Load(), sub.video.stats.drops.Load()
			st.AudioSent, st.AudioDrops = sub.audio.stats.packets.Load(), sub.audio.stats.drops.Load()
			out = append(out, st)
		}
		return nil
	})
	slices.SortFunc(out, func(a, b SubscriptionState) int { return strings.Compare(string(a.Share), string(b.Share)) })
	return out, err
}

// FanOut returns how many video and audio DownTracks a share fans out to; ok is false for an unknown or ended share.
func (s *SFU) FanOut(id ShareID) (video, audio int, ok bool) {
	sh := s.lookupShare(id)
	if sh == nil {
		return 0, 0, false
	}
	return len(sh.downTracks(webrtc.RTPCodecTypeVideo)), len(sh.downTracks(webrtc.RTPCodecTypeAudio)), true
}

// PCState describes one of a Conn's two PeerConnections as its actor has it.
type PCState struct {
	Exists bool   // the Conn has a PC of the kind (a closed sub PC exists; a closed pub PC is gone)
	Gen    uint32 // of that PC; for pub without a PC, the highest gen accepted
	// State is the PeerConnection's connection state as Pion has it now ("" without a PC).
	State string
	// Neg is the neg of the last offer: the last one answered on the pub PC, the last one sent on the sub PC. Closed
	// and Offering are the sub PC's negotiation state: closed for good, an offer outstanding.
	Neg              uint32
	Closed, Offering bool
	// HandshakeTimer and GraceTimer report whether the PC's handshake timeout and its grace after failed are running.
	HandshakeTimer, GraceTimer bool
}

// PC returns the state of the Conn's PeerConnection of a kind.
func (c *Conn) PC(ctx context.Context, kind PCKind) (PCState, error) {
	var st PCState
	err := c.do(ctx, func(context.Context) error {
		switch {
		case kind == PCPub && c.pub != nil:
			p := c.pub
			st = PCState{
				Exists: true, Gen: p.gen, State: p.pc.ConnectionState().String(), Neg: p.neg,
				HandshakeTimer: p.handshake.pending(), GraceTimer: p.grace.pending(),
			}
		case kind == PCPub:
			st.Gen = c.pubGen
		case kind == PCSub && c.sub != nil:
			s := c.sub
			st = PCState{
				Exists: true, Gen: s.gen, State: s.pc.ConnectionState().String(), Closed: s.closed, Offering: s.offering,
				Neg: s.neg, HandshakeTimer: s.handshake.pending(), GraceTimer: s.grace.pending(),
			}
		}
		return nil
	})
	return st, err
}

// Timings are the durations of PeerConnection recovery that a test may shorten (config.go): the handshake timeout
// (10 s), the grace after failed (30 s), the sub offer re-send (15 s), the ICE restart spacing (5 s), the window of
// the PC creation limit (1 min) and the stalled threshold (2 s).
type Timings struct {
	Handshake, Grace, OfferResend, ICERestart, PCWindow, Stalled time.Duration
}

// SetTimings replaces the durations of t that aren't zero. Call it before the SFU's first Join.
func (s *SFU) SetTimings(t Timings) {
	for _, d := range []struct {
		to   *time.Duration
		from time.Duration
	}{
		{&s.timing.handshake, t.Handshake}, {&s.timing.grace, t.Grace}, {&s.timing.offerResend, t.OfferResend},
		{&s.timing.iceRestart, t.ICERestart}, {&s.timing.pcWindow, t.PCWindow}, {&s.timing.stalled, t.Stalled},
	} {
		if d.from != 0 {
			*d.to = d.from
		}
	}
}

// DefaultTimings returns the durations an SFU uses unless a test shortens them: the numbers of 02 §12.
func DefaultTimings() Timings {
	d := defaultTimings()
	return Timings{
		Handshake: d.handshake, Grace: d.grace, OfferResend: d.offerResend, ICERestart: d.iceRestart,
		PCWindow: d.pcWindow, Stalled: d.stalled,
	}
}

// MaxPCCreations is the number of PeerConnections of one kind a client may make a Conn create within a minute.
const MaxPCCreations = maxPCCreations

// SetMediaICETimeouts replaces the ICE timeouts of the pub and sub PCs of every SFU built from now on, and returns
// the function that puts the real ones back (02 §7.3: 5 s to disconnected, 15 s more to failed). A test that needs a
// PC to fail can't wait that long.
func SetMediaICETimeouts(disconnected, failed, keepalive time.Duration) (restore func()) {
	old := mediaICETimeouts
	mediaICETimeouts = iceTimeouts{disconnected: disconnected, failed: failed, keepalive: keepalive}
	return func() { mediaICETimeouts = old }
}
