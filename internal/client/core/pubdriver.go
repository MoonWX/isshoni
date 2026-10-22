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

// The pub PC's negotiation (docs/m2/01-client-core.md §11.6, 01 §9): the share's actor offers, the server answers.
// This file has the first offer of a share and what answers it. The recovery rows of §11.6 (ICE restart, rebuild,
// unreachable) and §11.7's resend after a resume are README WA38's.

// offerTimeout bounds the gathering of one offer's candidates.
const offerTimeout = 10 * time.Second

// pubDriver is the negotiation state of the share's pub PC.
type pubDriver struct {
	gen, neg uint32            // of the last offer; gen 0 before the first
	pending  *protocol.PCOffer // the offer that waits for its answer, or nil (01 §9 rule 3: one at a time)
	started  bool              // Publisher.Start ran (after the first answer)
}

// offer makes the next offer of the current Publisher (gen 1 for the first) and sends it with the share's tracks.
// An offer that can't be sent now stays pending.
func (s *Share) offer() error {
	ctx, cancel := context.WithTimeout(s.ctx, offerTimeout)
	defer cancel()
	s.mu.Lock()
	pub := s.pub
	s.mu.Unlock()
	sd, err := pub.Offer(ctx)
	if err != nil {
		return err
	}
	if s.drv.gen == 0 {
		s.drv.gen = 1
	}
	s.drv.neg++
	o := protocol.PCOffer{
		PC: protocol.PCKindPub, Gen: s.drv.gen, Neg: s.drv.neg, SDP: protocol.SDP(sd.SDP),
		Tracks: trackRefs(pub.Tracks(), s.params.ShareID),
	}
	s.drv.pending = &o
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

// onAnswer applies the answer to the pending offer; the first one starts the Publisher. An answer to anything else
// is stale and dropped. A Publisher that refuses the answer ends the share.
func (s *Share) onAnswer(a protocol.PCAnswer) *EndInfo {
	p := s.drv.pending
	if p == nil || a.Gen != p.Gen || a.Neg != p.Neg {
		s.log.Debug("stale pub answer", slog.Uint64("gen", uint64(a.Gen)), slog.Uint64("neg", uint64(a.Neg)))
		return nil
	}
	s.drv.pending = nil
	s.mu.Lock()
	pub := s.pub
	s.mu.Unlock()
	if err := pub.SetAnswer(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: a.SDP.Reveal()}); err != nil {
		s.log.Warn("pub answer refused", slog.Uint64("gen", uint64(a.Gen)), logx.Err(err))
		return &EndInfo{Reason: EndError, Protocol: protocol.ErrorCodeSDPInvalid}
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

// onPCError handles an error in scope pc about the pending offer: stale_negotiation is ignored, rate_limited sends
// the offer again after retryAfterMs (1 s when absent); any other code ends the share (README WA38 makes the first
// sdp_invalid or bad_request a rebuild, §11.6).
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
	}
	s.log.Warn("pub offer failed", slog.String("code", string(e.Code)), slog.Uint64("gen", uint64(e.Gen)))
	return &EndInfo{Reason: EndError, Protocol: e.Code}
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

// onRestart handles the server's pc.restart{pub, rebuild}. Rebuilding the pub PC is README WA38's (§11.6).
func (s *Share) onRestart(r protocol.PCRestart) {
	s.log.Debug("pub PC restart asked", slog.String("mode", string(r.Mode)), slog.Uint64("gen", uint64(r.Gen)))
}
