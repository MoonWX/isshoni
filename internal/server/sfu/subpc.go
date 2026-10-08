package sfu

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/sdp/v3"
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
}

// spare is a transceiver of a sub PC without a DownTrack: RemoveTrack took its sender and made it inactive. It can
// carry another DownTrack once the viewer knows its m-section as inactive, which is when the viewer has answered an
// offer made after the DownTrack left, with the m-section inactive. A new DownTrack on it before that would look to
// the viewer like the old track with another SSRC and msid, without the inactive step that makes a browser fire
// `track` again (02 §18). (02 §9.3 would also reuse a transceiver whose current direction is recvonly. On a sub PC
// that is an m-section the viewer answered as one it sends on, which no viewer may: it is left alone.)
type spare struct {
	tr *webrtc.RTPTransceiver
	// neg is the neg of the first offer made since the DownTrack left; 0 until that offer.
	neg uint32
	// free: the viewer's last answer to that offer, or to a later one, left the m-section inactive. That is also
	// what Pion goes by when it looks for a transceiver to reuse (takeSpare).
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
// of the viewer's RTCP for the track, which runs until the sender stops.
func (s *subPC) addTrack(dt *DownTrack, log *slog.Logger) error {
	dt.pc.Store(s)
	tr, sender := s.takeSpare(dt, log)
	if tr == nil {
		var err error
		tr, err = s.pc.AddTransceiverFromTrack(dt,
			webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
		if err != nil {
			return err
		}
		sender = tr.Sender()
	}
	dt.transceiver, dt.sender = tr, sender
	s.readers.Go(func() { dt.readRTCP(sender) })
	return nil
}

// takeSpare gives dt a free spare transceiver of its kind: the m-section of an ended share then carries dt, sendonly
// again, under dt's msid and a new SSRC. It returns nil when the sub PC has none to give.
//
// The reuse is Pion's AddTrack (02 §9.3), so the new sender is the PeerConnection's own, like the sender of a new
// transceiver: Pion binds dt to the codecs and header extensions this viewer negotiated. (A sender made with the
// SFU's subscribe API would bind it to everything the SFU offers, whatever the viewer answered.)
//
// AddTrack chooses the transceiver itself: the oldest one of dt's kind that has no sender and that the last answer
// didn't leave sending. Only when it finds none does it add a transceiver, a sendrecv one, which a sub PC must never
// have. A free spare is one it can take (spare.free), so it is called only while there is one, and what it took is
// checked against the sub PC's own list:
//   - a free spare, sendonly now: the reuse.
//   - a spare that isn't free. Pion goes by the last answer alone: it also takes a transceiver that was reused and
//     lost its DownTrack again before the viewer answered an offer with that DownTrack in it, or one whose m-section
//     the viewer had refused. The transceiver goes back to what it was and dt gets a new one (addTrack): rare, and it
//     costs one m-section.
//   - a transceiver that Pion added. That can't happen while free means what it says. If it does, the transceiver is
//     stopped, which makes it inactive before any offer shows it, and is a spare from now on; the spares Pion didn't
//     take aren't free.
func (s *subPC) takeSpare(dt *DownTrack, log *slog.Logger) (*webrtc.RTPTransceiver, *webrtc.RTPSender) {
	if s.closed {
		return nil, nil // nothing joins a closed PC: adding a transceiver fails, and so must this
	}
	free := func(sp *spare) bool {
		return sp.free && sp.tr.Kind() == dt.kind && sp.tr.Sender() == nil &&
			sp.tr.Direction() == webrtc.RTPTransceiverDirectionInactive
	}
	if !slices.ContainsFunc(s.spares, free) {
		return nil, nil
	}
	sender, err := s.pc.AddTrack(dt)
	if err != nil {
		log.Warn("spare sub transceiver not taken", "err", err)
		return nil, nil
	}
	var tr *webrtc.RTPTransceiver
	for _, t := range s.pc.GetTransceivers() {
		if t.Sender() == sender {
			tr = t
			break
		}
	}
	i := slices.IndexFunc(s.spares, func(sp *spare) bool { return sp.tr == tr })
	if i >= 0 && s.spares[i].free && tr.Direction() == webrtc.RTPTransceiverDirectionSendonly {
		s.spares = slices.Delete(s.spares, i, i+1)
		return tr, sender
	}

	// Pion took something else: undo it.
	if err := s.pc.RemoveTrack(sender); err != nil {
		log.Warn("sub track not taken off the transceiver Pion chose", "err", err)
		return nil, nil // the PC is closing: a new transceiver fails too
	}
	if i >= 0 {
		log.Debug("spare sub transceiver not taken: Pion chose one that isn't free yet", "mid", tr.Mid())
		return nil, nil
	}
	// RemoveTrack found the sender on a transceiver, so tr is that one, and no spare.
	log.Error("Pion added a sub transceiver instead of reusing a free one; it stays inactive", "kind", dt.kind.String())
	if err := tr.Stop(); err != nil {
		log.Warn("sub transceiver not stopped", "err", err)
	}
	for _, sp := range s.spares {
		if free(sp) {
			sp.free = false
		}
	}
	s.spares = append(s.spares, &spare{tr: tr})
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

// sparesAnswered notes that the viewer answered offer neg with answer: a spare which that offer, or an earlier one,
// showed as inactive is free if the answer leaves its m-section inactive, as JSEP has it. A viewer that answers such
// an m-section with a direction keeps the spare from being used again, until an answer of its puts that right. The
// direction is read as Pion reads it (getPeerDirection), because Pion decides by it too.
func (s *subPC) sparesAnswered(neg uint32, answer string) {
	if !slices.ContainsFunc(s.spares, func(sp *spare) bool { return sp.neg != 0 && sp.neg <= neg }) {
		return
	}
	desc, err := parseSDP(answer)
	if err != nil {
		return // Pion has just taken it, so this can't be
	}
	inactive := map[string]bool{}
	for _, md := range desc.MediaDescriptions {
		dir := direction(md.Attributes)
		if dir == "" {
			dir = direction(desc.Attributes)
		}
		if mid, ok := md.Attribute(sdp.AttrKeyMID); ok && dir == sdp.AttrKeyInactive {
			inactive[mid] = true
		}
	}
	for _, sp := range s.spares {
		if sp.neg != 0 && sp.neg <= neg {
			sp.free = inactive[sp.tr.Mid()]
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
	s.sparesAnswered(neg, filtered)
	if s.dirty {
		c.offerSub(s)
	}
	return nil
}
