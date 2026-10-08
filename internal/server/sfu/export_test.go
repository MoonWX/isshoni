package sfu

import (
	"context"
	"slices"
	"strings"

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
	// VideoBound and AudioBound report whether Pion has bound the DownTrack to a negotiated sender.
	VideoBound, AudioBound bool
	// VideoPTs maps the H.264 profiles the viewer can receive to its payload types (the video binding's ptFor).
	VideoPTs map[ProfileKey]uint8
}

// Subscriptions returns the Conn's subscriptions, sorted by share id.
func (c *Conn) Subscriptions(ctx context.Context) ([]SubscriptionState, error) {
	var out []SubscriptionState
	err := c.do(ctx, func(context.Context) error {
		for id, sub := range c.subs {
			st := SubscriptionState{Share: id, Video: sub.reqVideo, Audio: sub.reqAudio}
			if b := sub.video.binding.Load(); b != nil {
				st.VideoBound, st.VideoPTs = true, b.ptFor
			}
			st.AudioBound = sub.audio.binding.Load() != nil
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
