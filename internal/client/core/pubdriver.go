package core

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// The pub PC's driver (docs/m2/01-client-core.md §11.6: M1 01 §9 and the pub column of M1 01 §10.4), the production
// version of the itest's share driver. The share's actor offers and the server answers, one offer at a time; the
// driver recovers the PC from the states it observes, polling it every timing.pcPoll:
//
//   - ICE disconnected: signal.Probe at once (is signaling down too?), and after 3 s an ICE restart (the same gen,
//     neg + 1, new ICE credentials), at most one per 5 s;
//   - an ICE restart (or the new PC of a rebuild or a republish) that isn't connected within 15 s, a PC that failed
//     or was closed under the core, the server's pc.restart{pub, rebuild}, an offer the server refused (sdp_invalid,
//     bad_request) or an answer the Publisher refused: a rebuild, at most one per 10 s. The Publisher is closed and a
//     new one offers gen + 1 with the same share id; the media pump re-attaches with keyframe requests;
//   - a second refused offer or answer within 60 s ends the share (error, sdp_invalid);
//   - 5 rebuilds without connecting: the share is unreachable (03: "Can't reach the server's media port"), and the
//     rebuilds go on every 30 s.
//
// A codec switch (hints.go, §8.6) asks for one more offer on the current PC, which goes out once no offer is pending
// and signaling is ready (01 §9 rule 3). While signaling isn't ready the driver only records; what follows a welcome
// (the pending offer sent again, an ICE restart, a republish) is recovery.go's. The local candidates go in the offer
// itself: publish.Publisher.Offer waits for their gathering (host candidates only while the server sends no ICE
// servers), so nothing is trickled.

// offerTimeout bounds the gathering of one offer's candidates.
const offerTimeout = 10 * time.Second

// unreachableAfter is how many rebuilds without connecting make the share unreachable (§11.6).
const unreachableAfter = 5

// pubDriver is the negotiation and recovery state of the share's pub PC. It is the actor's alone.
type pubDriver struct {
	gen, neg uint32            // of the last offer; gen 0 before the first (and after a republish dropped the PC)
	pending  *protocol.PCOffer // the offer that waits for its answer, or nil (01 §9 rule 3: one at a time)
	started  bool              // Publisher.Start ran on the current Publisher (after its first answer)
	// reoffer: a change of the Publisher's codec preferences (hints.go, §8.6) waits for the next offer, which goes
	// out once no offer is pending (01 §9 rule 3: changes fold into one follow-up).
	reoffer bool

	connected     bool      // the current PC's ICE is connected
	everConnected bool      // a PC of this share on this connection has been connected: one that isn't is recovering
	down          time.Time // when ICE was first seen disconnected; zero while it isn't
	probed        bool      // signal.Probe went out for this disconnection
	connectBy     time.Time // an ICE restart or a rebuilt or republished PC not connected by then is rebuilt; zero once up
	icedAt        time.Time // the last ICE restart
	rebuiltAt     time.Time // the last rebuild
	owed          string    // why a rebuild is owed (the server asked, a refused offer), until the spacing lets it
	rebuilds      int       // rebuilds since a PC was last connected
	unreachable   bool      // unreachableAfter rebuilds without connecting
	refusedAt     time.Time // the last refused offer or answer
}

// publisher returns the share's current Publisher, nil before the first EventLayer and while it is replaced.
func (s *Share) publisher() *publish.Publisher {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pub
}

// offer makes the next offer of the current Publisher, with new ICE credentials when restart, and sends it with the
// share's tracks: gen 1 for the first PC (or the current gen), neg + 1. An offer that can't be sent now stays
// pending: the resumed welcome sends it again (§11.7).
func (s *Share) offer(restart bool) error {
	ctx, cancel := context.WithTimeout(s.ctx, offerTimeout)
	defer cancel()
	pub := s.publisher()
	var (
		sd  webrtc.SessionDescription
		err error
	)
	if restart {
		sd, err = pub.OfferICERestart(ctx)
	} else {
		sd, err = pub.Offer(ctx)
	}
	if err != nil {
		return err
	}
	if s.drv.gen == 0 {
		s.drv.gen = 1
	}
	s.drv.neg++
	s.drv.reoffer = false // this offer carries the Publisher's codec preferences as they are
	o := protocol.PCOffer{
		PC: protocol.PCKindPub, Gen: s.drv.gen, Neg: s.drv.neg, SDP: protocol.SDP(sd.SDP),
		Tracks: trackRefs(pub.Tracks(), s.params.ShareID),
	}
	s.drv.pending = &o
	s.log.Debug("pub offer", slog.Uint64("gen", uint64(o.Gen)), slog.Uint64("neg", uint64(o.Neg)),
		slog.Bool("ice_restart", restart))
	if err := s.c.send(protocol.MessageTypePCOffer, o); err != nil {
		s.log.Info("pub offer not sent", slog.Uint64("gen", uint64(o.Gen)), logx.Err(err))
	}
	return nil
}

// trackRefs are a pub offer's tracks: one per m-section (the simulcast encodings share the video's mid), all of
// them the share's.
func trackRefs(tracks []publish.TrackInfo, shareID string) []protocol.TrackRef {
	var refs []protocol.TrackRef
	for _, tr := range tracks {
		ref := protocol.TrackRef{MID: tr.MID, ShareID: shareID, Kind: protocol.TrackKindVideo}
		if tr.Kind == webrtc.RTPCodecTypeAudio {
			ref.Kind = protocol.TrackKindAudio
		}
		if !slices.Contains(refs, ref) {
			refs = append(refs, ref)
		}
	}
	return refs
}

// onAnswer applies the answer to the pending offer; the first one of a Publisher starts it. An answer to anything
// else is stale and dropped. An answer the Publisher refuses counts as a refused offer (refused).
func (s *Share) onAnswer(a protocol.PCAnswer) *EndInfo {
	p := s.drv.pending
	if p == nil || a.Gen != p.Gen || a.Neg != p.Neg {
		s.log.Debug("stale pub answer", slog.Uint64("gen", uint64(a.Gen)), slog.Uint64("neg", uint64(a.Neg)))
		return nil
	}
	s.drv.pending = nil
	pub := s.publisher()
	if err := pub.SetAnswer(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: a.SDP.Reveal()}); err != nil {
		s.log.Warn("pub answer refused", slog.Uint64("gen", uint64(a.Gen)), logx.Err(err))
		return s.refused(protocol.ErrorCodeSDPInvalid)
	}
	if !s.drv.started {
		if err := pub.Start(s.ctx); err != nil {
			s.log.Warn("publisher start failed", logx.Err(err))
			return &EndInfo{Reason: EndError, Protocol: protocol.ErrorCodeInternal}
		}
		s.drv.started = true
	}
	return nil
}

// offerOrEnd makes an offer; one that fails for another reason than the share's end ends the share.
func (s *Share) offerOrEnd(restart bool) *EndInfo {
	if err := s.offer(restart); err != nil && s.ctx.Err() == nil {
		s.log.Warn("pub offer failed", logx.Err(err))
		return &EndInfo{Reason: EndError, Protocol: protocol.ErrorCodeInternal}
	}
	return nil
}

// onPCError handles an error in scope pc about the pending offer: stale_negotiation is ignored, rate_limited sends
// the offer again after retryAfterMs (1 s when absent), sdp_invalid and bad_request rebuild the PC once (refused);
// any other code ends the share.
func (s *Share) onPCError(e protocol.Error) *EndInfo {
	p := s.drv.pending
	if e.PC != protocol.PCKindPub || p == nil || e.Gen != p.Gen || e.Neg != p.Neg {
		return nil
	}
	switch e.Code {
	case protocol.ErrorCodeStaleNegotiation:
		return nil
	case protocol.ErrorCodeRateLimited:
		wait := time.Second
		if e.RetryAfterMs > 0 {
			wait = time.Duration(e.RetryAfterMs) * time.Millisecond
		}
		gen, neg := p.Gen, p.Neg
		time.AfterFunc(wait, func() { s.post(msgResend{gen: gen, neg: neg}) })
		return nil
	case protocol.ErrorCodeSDPInvalid, protocol.ErrorCodeBadRequest:
		s.log.Warn("pub offer refused", slog.String("code", string(e.Code)), slog.Uint64("gen", uint64(e.Gen)),
			slog.Uint64("neg", uint64(e.Neg)))
		s.drv.pending = nil
		return s.refused(e.Code)
	}
	s.log.Warn("pub offer failed", slog.String("code", string(e.Code)), slog.Uint64("gen", uint64(e.Gen)))
	return &EndInfo{Reason: EndError, Protocol: e.Code}
}

// refused handles an offer the server couldn't apply, or an answer the Publisher couldn't (01 §9 rule 8): the first
// rebuilds the PC; a second within 60 s ends the share (error, sdp_invalid).
func (s *Share) refused(code protocol.ErrorCode) *EndInfo {
	now := time.Now()
	if !s.drv.refusedAt.IsZero() && now.Sub(s.drv.refusedAt) < s.c.timing.refusedWindow {
		return &EndInfo{Reason: EndError, Protocol: protocol.ErrorCodeSDPInvalid}
	}
	s.drv.refusedAt = now
	s.drv.owed = "refused (" + string(code) + ")"
	return s.drive(now)
}

// resend sends the pending offer again, if it is still the one of gen and neg.
func (s *Share) resend(gen, neg uint32) {
	p := s.drv.pending
	if p == nil || p.Gen != gen || p.Neg != neg {
		return
	}
	if err := s.c.send(protocol.MessageTypePCOffer, *p); err != nil {
		s.log.Info("pub offer not sent again", logx.Err(err))
	}
}

// onRestart handles the server's pc.restart{pub, rebuild} for the current PC (01 §10.4): a rebuild, within its
// spacing. One for an older gen is ignored, and the server asks for nothing else on a pub PC.
func (s *Share) onRestart(r protocol.PCRestart) *EndInfo {
	if r.Mode != protocol.RestartModeRebuild || r.Gen < s.drv.gen || s.publisher() == nil {
		s.log.Debug("pub PC restart ignored", slog.String("mode", string(r.Mode)), slog.Uint64("gen", uint64(r.Gen)))
		return nil
	}
	s.drv.owed = "asked (" + string(r.Reason) + ")"
	return s.drive(time.Now())
}

// iceUp reports whether a PeerConnection's ICE is connected.
func iceUp(pc *webrtc.PeerConnection) bool {
	switch pc.ICEConnectionState() {
	case webrtc.ICEConnectionStateConnected, webrtc.ICEConnectionStateCompleted:
		return true
	}
	return false
}

// drive looks at the pub PC once (§11.6's table). It records ICE's state, and while signaling is ready it acts:
// signal.Probe when ICE has just become disconnected, an ICE restart after 3 s of it, a rebuild when the PC failed
// or closed, when an ICE restart or a rebuilt (or republished) PC hasn't connected within 15 s, or when one is owed.
// Each action keeps its spacing; one that has to wait is taken by a later look. The EndInfo is set when the share
// must end.
func (s *Share) drive(now time.Time) *EndInfo {
	pub := s.publisher()
	if pub == nil || s.rec.republish {
		return nil // no PC yet, or none the server knows
	}
	d := &s.drv
	pc := pub.PC()
	ice, state := pc.ICEConnectionState(), pc.ConnectionState()
	was := d.connected
	switch ice {
	case webrtc.ICEConnectionStateConnected, webrtc.ICEConnectionStateCompleted:
		d.connected, d.everConnected = true, true
		d.down, d.probed, d.connectBy = time.Time{}, false, time.Time{}
		d.rebuilds, d.unreachable = 0, false
	case webrtc.ICEConnectionStateDisconnected:
		d.connected = false
		if d.down.IsZero() {
			d.down = now
		}
	default:
		d.connected = false
		d.down, d.probed = time.Time{}, false
	}
	if d.connected != was {
		s.log.Info("pub PC", slog.Bool("connected", d.connected), slog.Uint64("gen", uint64(d.gen)),
			slog.String("ice", ice.String()))
		s.syncState()
	}
	if !s.rec.online {
		return nil // recorded; acting waits for ready (§11.6)
	}
	if !d.down.IsZero() && !d.probed {
		d.probed = true
		s.c.probe()
	}
	t := s.c.timing
	var broken string
	switch {
	case state == webrtc.PeerConnectionStateClosed:
		broken = "closed"
	case ice == webrtc.ICEConnectionStateFailed || state == webrtc.PeerConnectionStateFailed:
		broken = "failed"
	case d.owed != "":
		broken = d.owed
	case !d.connectBy.IsZero() && !now.Before(d.connectBy):
		broken = "not connected in time"
	}
	switch {
	case broken != "":
		return s.rebuild(now, broken)
	case !d.down.IsZero() && now.Sub(d.down) >= t.iceRestartAfter:
		return s.restartICE(now)
	}
	return s.maybeReoffer() // a codec switch that waited for an answer or for signaling (hints.go)
}

// restartICE offers an ICE restart on the current PC (01 §10.4): the same gen, the next neg, new ICE credentials.
// Not while an offer is pending (the 15 s rule rebuilds a PC whose restart never got its answer), and at most one
// per 5 s.
func (s *Share) restartICE(now time.Time) *EndInfo {
	d := &s.drv
	if d.pending != nil || now.Sub(d.icedAt) < s.c.timing.restartSpacing {
		return nil
	}
	d.icedAt, d.connectBy = now, now.Add(s.c.timing.connectWithin)
	d.down, d.probed = time.Time{}, false
	s.log.Info("pub PC ICE restart", slog.Uint64("gen", uint64(d.gen)), slog.Uint64("neg", uint64(d.neg+1)))
	return s.offerOrEnd(true)
}

// rebuild replaces the pub PC (01 §10.4): the Publisher closes (no pc.close: the next gen's offer replaces the PC
// at the server), and a new one of the same session and share offers gen + 1. At most one per 10 s, or per 30 s
// once the share is unreachable; a rebuild that has to wait for its spacing is owed.
func (s *Share) rebuild(now time.Time, why string) *EndInfo {
	d := &s.drv
	t := s.c.timing
	spacing := t.rebuildSpacing
	if d.unreachable {
		spacing = t.unreachableSpacing
	}
	if now.Sub(d.rebuiltAt) < spacing {
		if d.owed == "" {
			d.owed = why
		}
		return nil
	}
	d.owed, d.rebuiltAt = "", now
	d.rebuilds++
	if d.rebuilds >= unreachableAfter && !d.unreachable {
		d.unreachable = true
		s.log.Warn("the server's media port is unreachable", slog.Int("rebuilds", d.rebuilds))
	}
	s.log.Info("pub PC rebuild", slog.Uint64("gen", uint64(d.gen+1)), slog.String("why", why),
		slog.Int("rebuilds", d.rebuilds))
	s.closePublisher(false)
	d.gen++
	d.neg, d.pending, d.started = 0, nil, false
	d.connected, d.down, d.probed = false, time.Time{}, false
	d.connectBy = now.Add(t.connectWithin)
	s.syncState()
	if err := s.startPublisher(); err != nil {
		if s.ctx.Err() != nil {
			return nil // the share is ending: the actor takes that request next
		}
		s.log.Warn("pub PC rebuild failed", logx.Err(err))
		return &EndInfo{Reason: EndError, Protocol: protocol.ErrorCodeInternal}
	}
	return nil
}

// recovering reports whether the share's pub PC, which has been connected, isn't now (§11.4's reconnecting).
func (d *pubDriver) recovering() bool {
	return d.everConnected && !d.connected
}
