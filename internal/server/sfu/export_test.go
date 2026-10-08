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

// SetMediaICETimeouts replaces the ICE timeouts of the pub and sub PCs of every SFU built from now on, and returns
// the function that puts the real ones back (02 §7.3: 5 s to disconnected, 15 s more to failed). A test that needs a
// PC to fail can't wait that long.
func SetMediaICETimeouts(disconnected, failed, keepalive time.Duration) (restore func()) {
	old := mediaICETimeouts
	mediaICETimeouts = iceTimeouts{disconnected: disconnected, failed: failed, keepalive: keepalive}
	return func() { mediaICETimeouts = old }
}
