package sfu

import (
	"context"

	"github.com/pion/webrtc/v4"
)

// Subscription is one Conn's subscription to one share: a video and an audio DownTrack on the Conn's sub PC, and
// what the client last asked for (02 §5.1). It is created by the first UpdateSubscriptions item that names the
// share and removed only when the share ends or the Conn closes: off/off pauses it but keeps its transceivers, so
// toggling a tile needs no renegotiation (01 §8.9). It belongs to the Conn's actor.
type Subscription struct {
	conn         *Conn
	share        *Share
	video, audio *DownTrack
	// What the client asked for; apply turns it into what the DownTracks forward.
	reqVideo Quality
	reqAudio bool
}

// tracks returns the subscription's DownTracks, video first.
func (s *Subscription) tracks() [2]*DownTrack { return [2]*DownTrack{s.video, s.audio} }

// apply makes the DownTracks follow what the client asked for. In this slice a request names its layer directly:
// high is the full layer f, low the preview layer q, off nothing, and audio flows while Audio is set. A layer the
// publisher doesn't send simply forwards nothing. The fallbacks of 02 §10.1 (high from q when there is no f), the
// reasons and the SubscriptionStateEvents are README S52's; the caps of the allocator and the codec state S84's and
// S69's.
func (s *Subscription) apply() {
	switch s.reqVideo {
	case QualityHigh:
		s.video.setTarget(SlotF, true)
	case QualityLow:
		s.video.setTarget(SlotQ, true)
	default:
		s.video.setTarget(SlotF, false)
	}
	s.audio.setTarget(SlotAudio, s.reqAudio)
}

// detach takes the subscription's DownTracks off the share's fan-out lists and ends their writers.
func (s *Subscription) detach() {
	for _, dt := range s.tracks() {
		s.share.removeDownTrack(dt)
		dt.stop()
	}
}

// UpdateSubscriptions applies items in order: the desired state per share, merged into what the Conn already has
// (01 §8.9). The first result holds one error per item, nil when applied: sfu.share_not_found for a share that isn't
// in the Conn's room (it may just have ended), sfu.too_many_subscriptions past 256 subscriptions; the items before a
// failed one stay applied. The second is a Conn-level error (role, closed, busy, more than 64 items), with nothing
// applied. Creating subscriptions triggers one debounced sub offer for the whole batch; changing quality never
// renegotiates.
func (c *Conn) UpdateSubscriptions(ctx context.Context, items []SubscriptionUpdate) ([]error, error) {
	var errs []error
	err := c.do(ctx, func(context.Context) error {
		var err error
		errs, err = c.updateSubscriptions(items)
		return err
	})
	if err != nil {
		return nil, err
	}
	return errs, nil
}

func (c *Conn) updateSubscriptions(items []SubscriptionUpdate) ([]error, error) {
	switch {
	case !c.role.canSubscribe():
		return nil, newError(CodeRoleForbidden, "the role can't subscribe")
	case len(items) > maxSubscriptionsPerCall:
		return nil, newError(CodeTooManySubscriptions, "more than 64 items in one call")
	}
	errs := make([]error, len(items))
	created := false
	for i, it := range items {
		if !it.Video.valid() {
			errs[i] = errCaller("UpdateSubscriptions: unknown quality")
			continue
		}
		sub, made, err := c.subscription(it.Share)
		if err != nil {
			errs[i] = err
			continue
		}
		created = created || made
		sub.reqVideo, sub.reqAudio = it.Video, it.Audio
		sub.apply()
	}
	if created {
		c.subChanged()
	}
	return errs, nil
}

// subscription returns the Conn's subscription to a share, creating it (and the sub PC) when it is new. A new
// subscription's DownTracks are paused, on the share's fan-out lists and on the sub PC, with their writers running.
func (c *Conn) subscription(id ShareID) (sub *Subscription, created bool, err error) {
	if sub := c.subs[id]; sub != nil {
		return sub, false, nil
	}
	sh := c.sfu.lookupShare(id)
	if sh == nil || sh.room != c.room {
		return nil, false, newError(CodeShareNotFound, "no such share in the room")
	}
	if len(c.subs) >= maxSubscriptions {
		return nil, false, newError(CodeTooManySubscriptions, "the connection already has 256 subscriptions")
	}
	pc, err := c.ensureSubPC()
	if err != nil {
		return nil, false, err
	}
	sub = &Subscription{conn: c, share: sh}
	sub.video = newDownTrack(sub, webrtc.RTPCodecTypeVideo)
	sub.audio = newDownTrack(sub, webrtc.RTPCodecTypeAudio)
	// The share's lists first: they refuse a share that ended since the lookup, before the PC gets transceivers.
	for _, dt := range sub.tracks() {
		if !sh.addDownTrack(dt) {
			sub.detach()
			return nil, false, newError(CodeShareNotFound, "the share ended")
		}
	}
	for _, dt := range sub.tracks() {
		if err := pc.addTrack(dt); err != nil {
			c.log.Warn("DownTrack not added to the sub PC", "share_id", string(id), "err", err)
			sub.detach()
			for _, added := range sub.tracks() {
				pc.removeTrack(added, c.log)
			}
			return nil, false, newError(CodeInternal, "the sub PC refused a track")
		}
	}
	for _, dt := range sub.tracks() {
		c.writers.Go(func() { dt.run(c.stop) })
	}
	c.subs[id] = sub
	return sub, true, nil
}

// subscribedShareEnded runs on a subscriber's actor after a share it subscribed to ended: the subscription goes, its
// DownTracks leave the sub PC, and the PC renegotiates so the viewer's m-sections go inactive (02 §5.4).
func (c *Conn) subscribedShareEnded(sh *Share) {
	sub := c.subs[sh.id]
	if sub == nil || sub.share != sh {
		return
	}
	delete(c.subs, sh.id)
	sub.detach()
	if c.sub == nil {
		return
	}
	for _, dt := range sub.tracks() {
		c.sub.removeTrack(dt, c.log)
	}
	c.subChanged()
}
