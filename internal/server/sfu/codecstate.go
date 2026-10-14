package sfu

import (
	"context"
	"errors"
	"maps"
	"slices"

	"github.com/pion/webrtc/v4"
)

// Viewers without H.264 (02 §8.5). A fresh Firefox profile has no H.264 decoder until it has fetched OpenH264, which
// takes a minute or so; while it waits it lists no H.264 in its caps, or, worse, lists some and still refuses it in
// its answers. Two things keep such a viewer watching what it can and get it video as soon as it can decode it, and
// both are the server's: clients only poll their capabilities and send caps.update, never pc.restart for a codec
// (01 §11.7).
//
//   - Caps-driven: a Conn whose decode caps list no H.264 gets no video. Its video DownTracks stay off the sub PC, its
//     audio flows, and each subscription that asks for video reports decoder_unavailable. When its caps gain H.264
//     (SetDecodeCaps), the SFU rebuilds the sub PC with every DownTrack: a fresh PeerConnection picks up the new
//     decoder, which a renegotiated one may not.
//   - The guard, for caps that are wrong or stale: an answer that refuses H.264 on a video m-section with a DownTrack
//     doesn't fail the negotiation (DownTrack.Bind binds it as unsupported, and the audio flows), but that sub PC can
//     carry no video any more: Pion settles a PC's video codecs with its first video answer. The subscriptions report
//     decoder_unavailable, and the SFU rebuilds the sub PC every 20 s, at most 9 times (3 min, 05's "Reload the page"
//     hint). After that the reason is decoder_failed, until the next SetDecodeCaps starts the retries again.
//
// These rebuilds are the SFU's own: the limit on the PeerConnections a client makes a Conn create doesn't count them
// (02 §12). On the wire all three internal reasons, codec_mismatch included, are `codec` (01 §15.4).

// decoderState is what a Conn's actor knows of its viewer's H.264 decoders beyond its caps.
type decoderState struct {
	// refused: an answer of the viewer refused H.264 on a video m-section that carries a DownTrack, although its caps
	// list some. The sub PC that took it can carry no video (subPC.refusedVideo). It holds across the codec rebuilds
	// that follow, until an answer of a sub PC that never refused binds video with H.264.
	refused bool
	// retries counts the codec rebuilds since the viewer's caps last changed or its video was last taken; failed
	// says that all codecRetries of them were done and the last answer still refused H.264: decoder_failed.
	retries int
	failed  bool
	// retry runs the next codec rebuild while refused holds and failed doesn't.
	retry actorTimer
	// stale: the viewer gained H.264 while its sub PC, negotiated before the decoder existed, carried no
	// subscription. The next subscription rebuilds that PC instead of renegotiating it.
	stale bool
}

// SetDecodeCaps replaces what the viewer can decode (01's caps.update): the H.264 profiles of caps, none while it has
// no H.264 decoder (yet). The room's codec policy follows at once (02 §8.3). For the Conn's own video (02 §8.5):
//   - without H.264 its subscriptions get no video: the video DownTracks of new ones stay off the sub PC, and each
//     subscription that asks for video reports decoder_unavailable;
//   - a viewer that gains H.264, or whose video is held by the codec now (no decoder, a refused answer,
//     decoder_failed, or a stream in a profile its sub PC negotiated no payload type for), gets a new sub PC with
//     every DownTrack at once. It is the SFU's own rebuild, which the limit on PC creations doesn't count. A Conn
//     without subscriptions keeps its sub PC until the next subscription, which then rebuilds it. A sub PC that is
//     closed stays closed until the client's next request (02 §5.3), whose successor counts toward the limit
//     (02 §12) and carries the video;
//   - the guard's retries start again: decoder_failed holds only until the next SetDecodeCaps.
//
// The same profiles again, in whatever order, change nothing: a caps.update about something else than decoding
// starts no rebuild and leaves the guard as it is. The caps are kept as given. A profile key the SFU doesn't know
// counts as H.264 and decodes only its own profile.
func (c *Conn) SetDecodeCaps(ctx context.Context, caps DecodeCaps) error {
	return c.do(ctx, func(context.Context) error {
		c.setDecodeCaps(caps)
		return nil
	})
}

func (c *Conn) setDecodeCaps(caps DecodeCaps) {
	if slices.Equal(slices.Sorted(slices.Values(c.decode.H264)), slices.Sorted(slices.Values(caps.H264))) {
		return
	}
	had, held := c.decodesH264(), c.codecHeld()
	c.room.mu.Lock()
	c.decode = DecodeCaps{H264: slices.Clone(caps.H264)}
	c.room.mu.Unlock()
	has := c.decodesH264()
	if has != had {
		c.log.Info("decode caps changed", "h264", has)
	}
	c.sfu.updateCodecPolicy(c.room)

	c.codec.retry.stop()
	c.codec.retries, c.codec.failed = 0, false
	if has && (!had || held) {
		c.codecRebuild("the viewer's decoders changed")
	}
	c.refreshCodec()
}

// decodesH264 reports whether the viewer's caps list some H.264 profile.
func (c *Conn) decodesH264() bool { return len(c.decode.H264) > 0 }

// carries reports whether dt belongs on the Conn's sub PC: audio always, video only for a viewer whose caps list
// some H.264 (02 §8.5).
func (c *Conn) carries(dt *DownTrack) bool {
	return dt.kind != webrtc.RTPCodecTypeVideo || c.decodesH264()
}

// decoderReason is why the viewer gets no video of any share, as its decoders stand: decoder_unavailable while its
// caps list no H.264 or its sub PC refused it, decoder_failed once the guard's retries are spent; "" when nothing
// about its decoders holds the video back.
func (c *Conn) decoderReason() SubReason {
	switch {
	case !c.decodesH264():
		return SubReasonDecoderUnavailable
	case c.codec.failed:
		return SubReasonDecoderFailed
	case c.codec.refused:
		return SubReasonDecoderUnavailable
	}
	return SubReasonNone
}

// codecHeld reports whether the codec holds back the video of one of the Conn's subscriptions: its decoders
// (Subscription.codec), or a stream in a profile the viewer has no payload type for (codec_mismatch).
func (c *Conn) codecHeld() bool {
	for _, sub := range c.subs {
		if sub.codec != SubReasonNone || sub.video.mismatched() {
			return true
		}
	}
	return false
}

// refreshCodec applies the viewer's decoder state to every subscription and tells the client at once what changed
// (a cap the server sets, 02 §6.2), codec_mismatch included: the actor calls it whenever the decoder state or the
// bindings may have changed (SetDecodeCaps, an applied answer).
func (c *Conn) refreshCodec() {
	reason := c.decoderReason()
	for _, id := range slices.Sorted(maps.Keys(c.subs)) {
		sub := c.subs[id]
		if sub.codec != reason {
			sub.codec = reason
			sub.apply()
		}
		c.reportSubscription(sub)
	}
}

// codecAnswered runs after Pion has taken an answer of sub PC s, which has bound the DownTracks new in it (Bind is
// synchronous in SetRemoteDescription when the SFU offers). A video DownTrack bound as unsupported means that the
// viewer refused H.264 although its caps list some (02 §8.5, the guard): s can't carry video from now on, and a
// codec rebuild follows in 20 s, until the retries are spent. A sub PC that never refused and bound video with H.264
// ends the guard. An answer that binds no video changes nothing.
func (c *Conn) codecAnswered(s *subPC) {
	video := false
	for _, sub := range c.subs {
		if b := sub.video.binding.Load(); b != nil && b.pc == s {
			video = true
			if b.unsupported {
				s.refusedVideo = true
			}
		}
	}
	switch {
	case s.refusedVideo:
		if !c.codec.refused {
			c.log.Info("the viewer refused H.264 although its caps list some: its video waits for a codec rebuild",
				"gen", s.gen)
		}
		c.codec.refused = true
		c.scheduleCodecRetry()
	case video && c.codec.refused:
		c.log.Info("the viewer takes H.264 now", "gen", s.gen, "rebuilds", c.codec.retries)
		c.codec.retry.stop()
		c.codec.refused, c.codec.retries, c.codec.failed = false, 0, false
	}
	c.refreshCodec()
}

// scheduleCodecRetry sets the timer of the next codec rebuild, unless it runs already. Once codecRetries rebuilds
// have been answered with a refusal, there is none: the viewer's video is decoder_failed.
func (c *Conn) scheduleCodecRetry() {
	switch {
	case c.codec.failed, c.codec.retry.pending():
	case c.codec.retries >= codecRetries:
		c.codec.failed = true
		c.log.Info("the viewer's H.264 decoder failed: no more codec rebuilds until its caps change",
			"rebuilds", c.codec.retries)
	default:
		c.after(&c.codec.retry, c.sfu.timing.codecRetry, c.codecRetryDue)
	}
}

// codecRetryDue rebuilds the sub PC of a viewer whose answers refuse H.264, if they still do. A sub PC that is
// closed waits for the client's own request (02 §5.3), and a Conn without subscriptions for its next one; the answer
// of whatever PC follows decides whether the guard goes on.
func (c *Conn) codecRetryDue() {
	if !c.codec.refused || c.codec.failed || c.sub == nil || c.sub.closed || len(c.subs) == 0 {
		return
	}
	c.codec.retries++
	c.log.Info("codec rebuild of the sub PC", "rebuild", c.codec.retries, "of", codecRetries)
	c.codecRebuild("the viewer refused H.264")
}

// codecRebuild gives the Conn's open sub PC a successor for the viewer's decoders (rebuildSub): the SFU's own
// rebuild, which isn't counted toward the PC creation limit (02 §12). A closed sub PC stays closed until the client's
// next request (02 §5.3): its successor is one the client caused, which rebuildClosedSub counts, and it carries the
// video as the caps stand then (carries). Without subscriptions there is nothing to offer; the open sub PC is then
// rebuilt by the next subscription (stale). A failure reaches the client as an ErrorEvent about the sub PC, like any
// rebuild that no call returns.
func (c *Conn) codecRebuild(cause string) {
	switch {
	case c.sub == nil, c.sub.closed:
		return
	case len(c.subs) == 0:
		c.codec.stale = true
		return
	}
	if err := c.rebuildSub(cause); err != nil {
		c.log.Warn("sub PC not rebuilt", "cause", cause, "err", err)
		var e *Error
		if errors.As(err, &e) {
			c.sig.SendEvent(ErrorEvent{Err: e, Scope: ScopePCSub})
		}
	}
}
