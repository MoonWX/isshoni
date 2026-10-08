package sfu

import (
	"context"
	"sync/atomic"
	"time"

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
	// capQ and capReason are the server's cap on the video (02 §10.1), set by capSubscription: while capReason isn't
	// "", the subscription gets min(reqVideo, capQ), and capReason is why when that is less than the client asked
	// for. Nothing caps yet: the downlink allocator (README S84) will with SubReasonBandwidth, the codec state (S69)
	// with SubReasonCodecMismatch and SubReasonDecoderUnavailable.
	capQ      Quality
	capReason SubReason
	// told is what the client last heard of the subscription (report). A new subscription forwards nothing, without
	// a reason, and the client knows that without being told.
	told subState
	// quiet is the monoNow until which the media path's changes are held back: subEventInterval after the last
	// SubscriptionStateEvent. held is the timer that reports them then, nil when nothing waits.
	quiet int64
	held  *time.Timer

	// noticed: a notice that the subscription may have changed is in the Conn's event queue and hasn't been looked at
	// (changed). It is the one field that goroutines other than the actor touch.
	noticed atomic.Bool
}

// subState is what a SubscriptionStateEvent says about a subscription besides the request (02 §6.2).
type subState struct {
	forwarded Quality   // the video the viewer gets now
	audio     bool      // the viewer gets the share's audio now
	reason    SubReason // why forwarded is less than the request, when something other than time will change that
}

// Layer preferences of 02 §10.1, best first. A full layer is never sent as a thumbnail: one 4K layer per thumbnail
// would multiply the server's egress (the plan's "4K60 alone" case).
var (
	slotsForHigh  = [...]Slot{SlotF, SlotH, SlotQ}
	slotsForLow   = [...]Slot{SlotQ, SlotH}
	slotsForAudio = [...]Slot{SlotAudio}
)

// selectSlot is the layer choice of 02 §10.1: the slot that serves quality want of a DownTrack of a kind, among the
// layers a share has (present holds their slot bits). High is the full layer, else the middle one, else the preview
// layer; low is the preview layer, else the middle one; audio (any want but off) is the audio layer. ok is false when
// want is off or no such layer exists: then nothing is forwarded.
func selectSlot(kind webrtc.RTPCodecType, want Quality, present uint32) (slot Slot, ok bool) {
	var order []Slot
	switch {
	case want == QualityOff:
		return 0, false
	case kind == webrtc.RTPCodecTypeAudio:
		order = slotsForAudio[:]
	case want == QualityHigh:
		order = slotsForHigh[:]
	default:
		order = slotsForLow[:]
	}
	for _, s := range order {
		if present&(1<<s) != 0 {
			return s, true
		}
	}
	return 0, false
}

// shortfall returns why a subscription can't get the video quality the client asked for (02 §10.1), or "" when it
// can: then a layer that serves the request exists, and whatever the viewer still lacks is on its way (the sub PC's
// connection, the layer's keyframe; 01's "waiting"). allowed is the request under the server's cap, and the cap comes
// first, with its own reason. Without one, a request for high that only a lower layer can serve, or none, is no_layer
// (the share isn't live yet, or its sharer doesn't send the full layer now), and a request for low without a preview
// layer is no_preview_layer.
func shortfall(requested, allowed Quality, capReason SubReason, present uint32) SubReason {
	if allowed < requested {
		return capReason
	}
	if slot, ok := selectSlot(webrtc.RTPCodecTypeVideo, requested, present); ok && slot.quality() >= requested {
		return SubReasonNone
	}
	if requested == QualityLow {
		return SubReasonNoPreviewLayer
	}
	return SubReasonNoLayer
}

// tracks returns the subscription's DownTracks, video first.
func (s *Subscription) tracks() [2]*DownTrack { return [2]*DownTrack{s.video, s.audio} }

// allowed returns the video quality the subscription may get: the client's request under the server's cap
// (02 §10.1: effective = min(requested, cap)).
func (s *Subscription) allowed() Quality {
	if s.capReason != SubReasonNone {
		return min(s.reqVideo, s.capQ)
	}
	return s.reqVideo
}

// apply makes the DownTracks follow what the client asked for (02 §10.1): the video DownTrack gets the requested
// quality under the server's cap, the audio DownTrack the share's audio while Audio is set (audio follows focus: the
// client sets it for the share it focuses, and the SFU forces nothing). Each DownTrack then takes the best layer the
// share has for that, and pauses when there is none.
func (s *Subscription) apply() {
	s.video.setWant(s.allowed())
	on := QualityOff
	if s.reqAudio {
		on = QualityHigh
	}
	s.audio.setWant(on)
}

// state returns what the subscription gets now: the quality of the video layer forwarded, whether audio flows, and,
// while the video is less than the client asked for, the reason (shortfall).
func (s *Subscription) state() subState {
	st := subState{forwarded: s.video.forwarded(), audio: s.audio.forwarded() != QualityOff}
	if st.forwarded < s.reqVideo {
		st.reason = shortfall(s.reqVideo, s.allowed(), s.capReason, s.share.present())
	}
	return st
}

// capSubscription sets the server's cap on a subscription's video, or lifts it (reason ""), and applies it
// (02 §10.1): the client's request stays, the subscription gets at most q, and the client hears why once it gets less
// than it asked for. It runs on the Conn's actor. Its callers come with the downlink allocator (README S84) and the
// codec state (S69).
func (c *Conn) capSubscription(sub *Subscription, q Quality, reason SubReason) {
	sub.capQ, sub.capReason = q, reason
	sub.apply()
	c.reportSubscription(sub)
}

// changed tells the subscriber's Conn that what the subscription gets may have changed. It never blocks: the Conn's
// actor looks at the subscription when it gets to it (report). The DownTrack writers call it, Pion's Unbind, and
// whoever changes the share's layers (02 §5.4: work for another Conn is posted to it).
//
// At most one notice per subscription is in the actor's queue, however often what a writer forwards changes: the
// actor takes the notice back before it looks, so a change that finds one queued is seen by that look, and one that
// comes later queues the next.
func (s *Subscription) changed() {
	if !s.noticed.CompareAndSwap(false, true) {
		return
	}
	c := s.conn
	c.post(func() {
		s.noticed.Store(false)
		c.report(s, true)
	})
}

// reportSubscription tells the client at once what a subscription gets, if that differs from what it last heard: the
// answer to something the client asked for (UpdateSubscriptions), or to a cap the server set (capSubscription). It
// runs on the Conn's actor.
func (c *Conn) reportSubscription(sub *Subscription) { c.report(sub, false) }

// report sends the client a SubscriptionStateEvent when what a subscription gets differs from what the client last
// heard (02 §6.2, §10.1): only when the forwarded video, the audio or the reason changed, never for a request alone.
// So a switch is reported when the new layer's keyframe has arrived, a pause at once, and a wait for the sub PC or
// for a keyframe not at all. It runs on the Conn's actor. A subscription that is gone, or whose share has ended,
// reports nothing more: the hub tells the clients about the share.
//
// paced marks a change that came from the media path (changed): a layer came or went, a writer began or ended a
// stream. Such changes are a publisher's doing, as often as it likes (a stream that alternates between two profiles
// begins and ends with every frame), so they are reported at most once per subEventInterval and subscription: one
// that comes sooner after the last event waits for the rest of the interval, and what is sent then is the state at
// that time. So the client always ends up knowing the latest state, and never hears more than four of them a second
// that it didn't ask for.
func (c *Conn) report(sub *Subscription, paced bool) {
	if c.subs[sub.share.id] != sub {
		return
	}
	if _, live := sub.share.liveState(); !live {
		return
	}
	st := sub.state()
	if st == sub.told {
		return
	}
	now := monoNow()
	if wait := sub.quiet - now; paced && wait > 0 {
		if sub.held == nil {
			sub.held = time.AfterFunc(time.Duration(wait), func() {
				c.post(func() {
					sub.held = nil
					c.report(sub, true)
				})
			})
		}
		return
	}
	sub.told, sub.quiet = st, now+int64(c.sfu.subEventEvery)
	c.sig.SendEvent(SubscriptionStateEvent{
		Share: sub.share.id, Requested: sub.reqVideo, Forwarded: st.forwarded, Audio: st.audio, Reason: st.reason,
	})
}

// detach takes the subscription's DownTracks off the share's fan-out lists and ends their writers. A report that
// was held back is dropped. The Conn's actor calls it when the subscription goes.
func (s *Subscription) detach() {
	if s.held != nil {
		s.held.Stop()
		s.held = nil
	}
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
//
// What each subscription then gets follows 02 §10.1, and the client hears of it through SubscriptionStateEvents: a
// pause, and a request that the share's layers can't serve, before UpdateSubscriptions returns; everything else when
// it happens, or up to 250 ms later when it follows another event closely (report).
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
		c.reportSubscription(sub)
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
		if err := pc.addTrack(dt, c.log); err != nil {
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
// DownTracks leave the sub PC, and the PC renegotiates so the viewer's m-sections go inactive (02 §5.4). Once the
// viewer has answered that, the m-sections are free for the Conn's next subscription (subPC.spares).
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
