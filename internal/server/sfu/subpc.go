package sfu

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

// subPC is a Conn's subscribe PeerConnection of one generation (02 §5.3): the SFU always offers, so there is never
// glare. Its negotiation is the state machine of 02 §5.3: idle or offering, with changes made while an offer is
// outstanding folded into one follow-up offer. Everything but ready belongs to the Conn's actor.
//
// So far it has the states idle and offering, the 50 ms debounce, the answer, and the reuse of the m-sections of
// ended shares. README S57 adds the rest of the table: the 15 s re-send, ICE restarts, ResetPC and the rebuild from
// closed; S69 the codec rebuilds.
type subPC struct {
	gen uint32
	pc  *webrtc.PeerConnection
	// ready is the DTLS-ready gate (02 §9.3): true while this PC can send (follow). Its DownTracks forward nothing
	// while it is false, because Pion drops RTP written before DTLS is up (S4 finding 1), and RTP written to a PC that
	// has failed or closed, without an error. A rebuilt sub PC is a new subPC with its own gate, so a stale "ready"
	// can never leak.
	ready atomic.Bool
	// readers are the RTCP readers of the PC's DownTracks, one per sender; each ends when its sender stops, all of
	// them when the PC closes.
	readers sync.WaitGroup

	// closed: the PeerConnection is closed, by Pion because the client closed its side, or by the SFU after a fatal
	// error (subFatal). Nothing is offered on it any more, and no offer is outstanding or waiting (markClosed); README
	// S57 replaces it with a new gen when the Conn needs a sub PC again (02 §5.3, the closed row).
	closed   bool
	neg      uint32      // the last offer's neg; 0 before the first
	offering bool        // an offer is outstanding: neg is waiting for its answer
	dirty    bool        // something changed while offering: offer again after the answer
	debounce *time.Timer // a change is waiting for its offer; nil when none is
	// offer is the last offer sent, as sent: what Resync and the 15 s re-send repeat while offering (README S57).
	offer subOffer
	// lastState is the last connection state onPCState handled, so that each state is reported once.
	lastState webrtc.PeerConnectionState

	// spares are the transceivers whose DownTrack has left (its share ended): their m-sections stay in the SDP for
	// good, and the next DownTrack of the kind takes one over instead of adding a new one, so the SDP doesn't grow
	// with every share of a long session (02 §9.3).
	spares []*spare
	// dtls is the PC's DTLS transport, kept from its first sender: the sender of a DownTrack that takes over a spare
	// transceiver is made on it.
	dtls *webrtc.DTLSTransport
}

// spare is a transceiver of a sub PC without a DownTrack: RemoveTrack took its sender and made it inactive. It can
// carry another DownTrack once the viewer knows its m-section as inactive, which is when the viewer has answered an
// offer made after the DownTrack left. A new DownTrack on it before that would look to the viewer like the old
// track with another SSRC and msid, without the inactive step that makes a browser fire `track` again (02 §18).
type spare struct {
	tr *webrtc.RTPTransceiver
	// neg is the neg of the first offer made since the DownTrack left; 0 until that offer.
	neg uint32
	// free: the viewer answered that offer.
	free bool
}

// subOffer is a sub offer as the Signaler got it.
type subOffer struct {
	neg    uint32
	sdp    string
	tracks []TrackBinding
}

// ensureSubPC returns the Conn's sub PC, creating the first one (gen 1) when a subscription needs it.
func (c *Conn) ensureSubPC() (*subPC, error) {
	if c.sub != nil {
		return c.sub, nil
	}
	pc, err := c.sfu.apis.sub.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		c.log.Warn("sub PC not created", "err", err)
		return nil, newError(CodeInternal, "the sub PC could not be created")
	}
	c.subGen++
	s := &subPC{gen: c.subGen, pc: pc}
	gen := s.gen
	pc.OnConnectionStateChange(func(webrtc.PeerConnectionState) {
		// Only "something changed": Pion runs each callback in a goroutine of its own, so their order means nothing.
		c.post(func() { c.onPCState(PCSub, gen, pc) })
	})
	c.sub = s
	c.resetCandidates(PCSub, gen)
	return s, nil
}

// addTrack puts dt on the sub PC: on the free transceiver of an ended share when there is one of its kind
// (takeSpare), else on a new sendonly transceiver (viewers never send media on the sub PC). So the m-sections of a
// sub PC are the most subscriptions its Conn ever had at once, not all it ever had (02 §9.3). It starts the reader
// of the viewer's RTCP for the track, which runs until the sender stops. api is the SFU's subscribe API.
func (s *subPC) addTrack(api *webrtc.API, dt *DownTrack, log *slog.Logger) error {
	dt.pc.Store(s)
	tr, sender := s.takeSpare(api, dt, log)
	if tr == nil {
		var err error
		tr, err = s.pc.AddTransceiverFromTrack(dt,
			webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
		if err != nil {
			return err
		}
		sender = tr.Sender()
		if s.dtls == nil {
			s.dtls = sender.Transport()
		}
	}
	dt.transceiver, dt.sender = tr, sender
	s.readers.Go(func() { dt.readRTCP(sender) })
	return nil
}

// takeSpare gives dt a free spare transceiver of its kind, with a new sender: the m-section of an ended share then
// carries dt, sendonly again, under dt's msid and a new SSRC. It returns nil when no spare is free.
//
// This is what Pion's AddTrack does when it reuses a transceiver, made here on the transceiver the sub PC chose:
// AddTrack picks by Pion's own view of the last answer, and when it finds none it adds a sendrecv transceiver, which
// a sub PC must never have. A spare that isn't as RemoveTrack left it, or that takes no sender, is given up: it
// stays an inactive m-section.
func (s *subPC) takeSpare(api *webrtc.API, dt *DownTrack, log *slog.Logger) (*webrtc.RTPTransceiver, *webrtc.RTPSender) {
	if s.closed {
		return nil, nil // nothing joins a closed PC: adding a transceiver fails, and so must this
	}
	for i := 0; i < len(s.spares); {
		sp := s.spares[i]
		if !sp.free || sp.tr.Kind() != dt.kind {
			i++
			continue
		}
		s.spares = slices.Delete(s.spares, i, i+1)
		if sp.tr.Sender() != nil || sp.tr.Direction() != webrtc.RTPTransceiverDirectionInactive {
			log.Warn("spare sub transceiver given up: not inactive", "mid", sp.tr.Mid())
			continue
		}
		sender, err := api.NewRTPSender(dt, s.dtls)
		if err == nil {
			if err = sp.tr.SetSender(sender, dt); err != nil {
				// Pion left the sender on the transceiver; RemoveTrack takes it off again.
				_ = s.pc.RemoveTrack(sender)
			}
		}
		if err != nil {
			log.Warn("spare sub transceiver given up", "mid", sp.tr.Mid(), "err", err)
			continue
		}
		return sp.tr, sender
	}
	return nil, nil
}

// removeTrack stops dt's sender, which makes its m-section inactive in the next offer, and keeps the transceiver as
// a spare for a later DownTrack.
func (s *subPC) removeTrack(dt *DownTrack, log *slog.Logger) {
	if dt.sender == nil {
		return
	}
	if err := s.pc.RemoveTrack(dt.sender); err != nil {
		log.Debug("sub track not removed", "err", err) // the PC is closed, or the sender didn't stop: no spare
	} else if dt.transceiver != nil {
		s.spares = append(s.spares, &spare{tr: dt.transceiver})
	}
	dt.sender, dt.transceiver = nil, nil
}

// sparesOffered notes that offer neg was made: it shows the viewer every spare of the PC as inactive.
func (s *subPC) sparesOffered(neg uint32) {
	for _, sp := range s.spares {
		if sp.neg == 0 {
			sp.neg = neg
		}
	}
}

// sparesAnswered notes that the viewer answered offer neg: the spares that offer, or an earlier one, showed as
// inactive are free.
func (s *subPC) sparesAnswered(neg uint32) {
	for _, sp := range s.spares {
		if sp.neg != 0 && sp.neg <= neg {
			sp.free = true
		}
	}
}

// bindings returns the tracks binding of the offer just set: one entry per m-section that carries a share, in
// m-section order (01 §9 rule 4). It is never nil: an offer without shares has an empty binding.
func (s *subPC) bindings() []TrackBinding {
	out := []TrackBinding{}
	for _, tr := range s.pc.GetTransceivers() {
		sender := tr.Sender()
		if sender == nil || tr.Mid() == "" {
			continue
		}
		if dt, ok := sender.Track().(*DownTrack); ok {
			out = append(out, TrackBinding{MID: tr.Mid(), Share: dt.share.id, Kind: dt.kind})
		}
	}
	return out
}

// follow makes the DTLS-ready gate follow the PC's connection state; the actor calls it for every state it handles
// (onPCState). The gate opens when the PC is connected. It stays as it is while the PC is disconnected: the selected
// pair is still there, packets may get through, and ICE often recovers by itself. In every other state it is closed:
// a failed or closed PC sends nothing, and Pion takes what is written to it without an error for as long as it stays
// that way, as it does before DTLS is up; a PC that is connecting again (an ICE restart, README S57) opens the gate
// anew when it is connected, with a keyframe request for each video DownTrack.
func (s *subPC) follow(state webrtc.PeerConnectionState) {
	switch state {
	case webrtc.PeerConnectionStateConnected:
		s.ready.Store(true)
	case webrtc.PeerConnectionStateDisconnected:
	default:
		s.ready.Store(false)
	}
}

// markClosed puts s into the closed state of 02 §5.3: no offer is outstanding or waiting, and none follows. Its
// gate is shut for good.
func (s *subPC) markClosed() {
	s.closed = true
	s.ready.Store(false)
	s.offering, s.dirty = false, false
	if s.debounce != nil {
		s.debounce.Stop()
		s.debounce = nil
	}
}

// close stops the debounce timer, closes the PeerConnection and waits for the RTCP readers of its DownTracks, which
// end when Pion stops their senders.
func (s *subPC) close(log *slog.Logger) {
	s.markClosed()
	if err := s.pc.Close(); err != nil {
		log.Debug("sub PC close", "gen", s.gen, "err", err)
	}
	s.readers.Wait()
}

// subFatal closes sub PC s after an error it can't negotiate past: an offer Pion couldn't make or set, or an answer
// Pion refused (it keeps the answer it refused, has no rollback, and so takes no other one). This is the "fatal
// error" of 02 §5.3's closed row: s stays the Conn's sub PC, closed, with the Conn's subscriptions intact, and README
// S57 builds its successor (gen + 1) on ResetPC, RestartICE, Resync or the next subscription change. The caller
// tells the client; Pion's closed state follows as a PCStateEvent.
func (c *Conn) subFatal(s *subPC) {
	s.close(c.log)
}

// subChanged notes that the sub PC's tracks changed (02 §5.3): while idle it starts the 50 ms debounce, so one user
// action that changes several subscriptions makes one offer; while an offer is outstanding it marks the PC dirty.
func (c *Conn) subChanged() {
	s := c.sub
	switch {
	case s == nil || s.closed:
	case s.offering:
		s.dirty = true
	case s.debounce == nil:
		s.debounce = time.AfterFunc(subOfferDebounce, func() {
			c.post(func() {
				if c.sub == s && s.debounce != nil {
					s.debounce = nil
					c.offerSub(s)
				}
			})
		})
	}
}

// offerSub creates the next offer of s, sets it with every candidate in it (the SFU never trickles) and sends it
// with its tracks binding. A failure is fatal for s (subFatal): an idle PC whose change was never offered would
// leave the viewer without those tracks until something else changed. The client hears of it as an ErrorEvent with
// sfu.internal, which is 01's retryable `internal`, not sdp_invalid: no rule of 01 has the client ask for a rebuild
// on it, so the tracks come back with the sub PC that README S57 builds from the closed state.
func (c *Conn) offerSub(s *subPC) {
	if s.closed {
		return
	}
	fail := func(what string, err error) {
		c.log.Warn("sub offer failed; the sub PC is closed", "gen", s.gen, "step", what, "err", err)
		c.subFatal(s)
		c.sig.SendEvent(ErrorEvent{Err: newError(CodeInternal, "the sub offer could not be "+what), Scope: ScopePCSub})
	}
	offer, err := s.pc.CreateOffer(nil)
	if err != nil {
		fail("created", err)
		return
	}
	local, complete, err := setLocalComplete(c.ctx, s.pc, offer)
	if err != nil {
		if c.ctx.Err() == nil { // otherwise the Conn is closing, and its teardown closes the PC
			fail("applied", err)
		}
		return
	}
	if !complete {
		c.log.Warn("ICE gathering did not finish in time; the sub offer may lack candidates", "gen", s.gen)
	}
	s.neg++
	s.offering, s.dirty = true, false
	s.sparesOffered(s.neg)
	s.offer = subOffer{neg: s.neg, sdp: local, tracks: s.bindings()}
	c.sig.SendOffer(PCSub, s.gen, s.neg, s.offer.sdp, s.offer.tracks)
}

// HandleAnswer applies the client's answer to the outstanding sub offer. Its gen and neg must be that offer's:
// anything else is sfu.stale_answer, which 01's sfuplane drops silently. If something changed while the offer was
// outstanding, the next offer follows at once. Answers are only valid for PCSub.
//
// An answer the SFU's own checks refuse (sfu.bad_sdp) leaves the offer outstanding. One that Pion refuses is
// sfu.bad_sdp too, and closes the sub PC (subFatal): the client asks for a rebuild after 01's sdp_invalid anyway
// (01 §9 rule 8), and every later answer for this gen is sfu.bad_pc.
func (c *Conn) HandleAnswer(ctx context.Context, pc PCKind, gen, neg uint32, sdp string) error {
	return c.do(ctx, func(context.Context) error { return c.handleAnswer(pc, gen, neg, sdp) })
}

func (c *Conn) handleAnswer(kind PCKind, gen, neg uint32, raw string) error {
	s := c.sub
	switch {
	case kind != PCSub:
		return newError(CodeBadPC, "an answer is only valid for the sub PC")
	case s == nil:
		return newError(CodeBadPC, "there is no sub PC")
	case s.closed && gen == s.gen:
		return newError(CodeBadPC, "the sub PC is closed")
	case gen != s.gen || !s.offering || neg != s.neg:
		return newError(CodeStaleAnswer, "the answer doesn't match the outstanding sub offer")
	}
	// The video m-lines without H.264 that this returns are README S69's: their DownTracks bind as unsupported.
	if _, err := checkSubAnswer(raw); err != nil {
		return err
	}
	filtered, dropped, err := c.sfu.apis.filter.filterSDP(raw)
	if err != nil {
		return err
	}
	if dropped > 0 {
		c.log.Debug("remote candidates dropped from a sub answer", "count", dropped)
	}
	remote := webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: filtered}
	if err := s.pc.SetRemoteDescription(remote); err != nil {
		// Pion takes the answer (signaling state stable) before it checks the rest of it, and has no rollback: the
		// offer is no longer outstanding in Pion, which would refuse the right answer too, also after a re-send of
		// the offer. The PC can't negotiate any more.
		c.log.Debug("sub answer refused by SetRemoteDescription", "gen", gen, "neg", neg, "err", err)
		c.log.Info("sub PC closed: Pion refused its answer", "gen", gen, "neg", neg) // no err: it may quote the SDP
		c.subFatal(s)
		return newError(CodeBadSDP, "the sub answer can't be applied")
	}
	c.flushCandidates(PCSub, gen, s.pc)
	s.offering = false
	s.sparesAnswered(neg)
	if s.dirty {
		c.offerSub(s)
	}
	return nil
}
