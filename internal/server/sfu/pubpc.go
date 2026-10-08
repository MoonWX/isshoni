package sfu

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/pion/webrtc/v4"
)

// pubPC is a Conn's publish PeerConnection of one generation (02 §5.3): the client offers, the SFU answers. Its
// incoming tracks are mapped to the Conn's shares by the tracks binding of the latest offer, never by msid (01 §9
// rule 4). Everything but the pubTracks' own fields belongs to the Conn's actor.
type pubPC struct {
	gen uint32
	pc  *webrtc.PeerConnection
	// neg is the last negotiation answered in this gen and answer what it got: a repeated neg gets answer again, a
	// lower one is stale (01 §9 rule 3).
	neg    uint32
	answer string
	// bound maps the mids of the latest offer's sending m-sections that carry a share to that share and kind. A
	// track on any other mid feeds nothing.
	bound  map[string]TrackBinding
	tracks map[trackKey]*pubTrack
	// readers are the tracks' read goroutines; they end when the PC closes.
	readers sync.WaitGroup
}

// trackKey names one incoming RTP stream of a pub PC: an m-section and, for simulcast, a rid.
type trackKey struct{ mid, rid string }

// pubTrack is one incoming RTP stream of a pub PC with its two readers (RTP and RTCP). The readers run for as long
// as Pion delivers the track, whether or not it carries a share: a track that nobody reads fills Pion's buffers.
// What they read goes to the Layer the track is attached to. The media path (README S41) adds what a Layer does
// with a packet (02 §9.1); until then, and whenever layer is nil (the m-section carries no share: 02 §8.4 step 1),
// the packets are counted and discarded.
type pubTrack struct {
	key   trackKey
	kind  webrtc.RTPCodecType
	track *webrtc.TrackRemote
	recv  *webrtc.RTPReceiver

	layer   atomic.Pointer[Layer] // set by the actor; nil while the track carries no share
	packets atomic.Uint64         // RTP packets read
}

// HandleOffer applies a pub offer and returns the answer synchronously, with every candidate in it (gathering is
// instant with the muxes, capped at 2 s; the SFU never trickles). tracks binds each sending m-section to a share of
// this Conn; an m-section it doesn't bind, or binds to a share that has ended, is answered a=inactive and carries
// nothing (02 §8.4). A higher gen replaces the pub PC: the client rebuilt it. A lower gen, or a lower neg in the
// current gen, returns sfu.stale_offer and changes nothing; a repeated neg gets the stored answer again (02 §5.3).
// Offers are only valid for PCPub.
func (c *Conn) HandleOffer(ctx context.Context, pc PCKind, gen, neg uint32, sdp string, tracks []TrackBinding,
) (answerSDP string, err error) {
	err = c.do(ctx, func(ctx context.Context) error {
		var err error
		answerSDP, err = c.handleOffer(ctx, pc, gen, neg, sdp, tracks)
		return err
	})
	if err != nil {
		return "", err
	}
	return answerSDP, nil
}

// handleOffer is 02 §8.4 on the actor. Everything that can refuse the offer for what it says (steps 0–1) runs before
// anything changes.
func (c *Conn) handleOffer(ctx context.Context, kind PCKind, gen, neg uint32, raw string, tracks []TrackBinding,
) (string, error) {
	switch {
	case kind != PCPub:
		return "", newError(CodeBadPC, "an offer is only valid for the pub PC")
	case !c.role.canPublish():
		return "", newError(CodeRoleForbidden, "the role can't publish")
	}
	// Step 0: gen and neg (02 §5.3). Both count from 1, so 0 is below every valid value.
	cur := c.pub
	switch {
	case gen == 0 || neg == 0 || gen < c.pubGen:
		return "", newError(CodeStaleOffer, "a pub offer of an older gen, or with a counter of 0")
	case gen == c.pubGen && cur == nil:
		return "", newError(CodeBadPC, "the pub PC of this gen is closed")
	case gen == c.pubGen && neg < cur.neg:
		return "", newError(CodeStaleOffer, "a pub offer with an older neg")
	case gen == c.pubGen && neg == cur.neg:
		return cur.answer, nil
	}

	// Step 1: validate, and drop the candidates the server must not probe (02 §7.3).
	offer, err := checkPubOffer(raw, tracks, c.ownsShare)
	if err != nil {
		return "", err
	}
	filtered, dropped, err := c.sfu.apis.filter.filterSDP(raw)
	if err != nil {
		return "", err
	}
	if dropped > 0 {
		c.log.Debug("remote candidates dropped from a pub offer", "count", dropped)
	}

	// Step 2: the PC of this gen. A new one replaces the old one only once it has taken the offer, so an offer Pion
	// refuses changes nothing either.
	p := cur
	if gen > c.pubGen {
		if p, err = c.newPubPC(gen); err != nil {
			c.log.Warn("pub PC not created", "err", err)
			return "", newError(CodeInternal, "the pub PC could not be created")
		}
	}
	remote := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: filtered}
	if err := p.pc.SetRemoteDescription(remote); err != nil {
		c.log.Debug("pub offer refused by SetRemoteDescription", "gen", gen, "neg", neg, "err", err)
		if p != cur {
			p.close(c.log)
		}
		return "", newError(CodeBadSDP, "the pub offer can't be applied")
	}
	if p != cur {
		if cur != nil {
			c.log.Info("pub PC replaced", "gen", gen, "old_gen", cur.gen)
			c.pub = nil
			cur.close(c.log)
		}
		c.pub, c.pubGen = p, gen
		c.resetCandidates(PCPub, gen)
	}
	p.bound = make(map[string]TrackBinding, len(offer.sections))
	for _, sec := range offer.sections {
		p.bound[sec.mid] = TrackBinding{MID: sec.mid, Share: sec.share, Kind: sec.kind}
	}
	c.rebindTracks(p)
	c.flushCandidates(PCPub, gen, p.pc)

	// Step 3, the codec filter by room policy (SetCodecPreferences on each video transceiver), is README S69's: until
	// then the policy is always high, which allows all five profiles.

	// Step 4: the answer, with every candidate in it.
	answer, err := p.pc.CreateAnswer(nil)
	if err != nil {
		c.log.Warn("pub answer not created", "gen", gen, "neg", neg, "err", err)
		return "", newError(CodeInternal, "the pub answer could not be created")
	}
	local, complete, err := setLocalComplete(ctx, p.pc, answer)
	if err != nil {
		c.log.Warn("pub answer not applied", "gen", gen, "neg", neg, "err", err)
		return "", newError(CodeInternal, "the pub answer could not be applied")
	}
	if !complete {
		c.log.Warn("ICE gathering did not finish in time; the pub answer may lack candidates", "gen", gen, "neg", neg)
	}

	// Step 5: the copy for the client. Each audio m-section that carries a share asks for stereo at its share's
	// bitrate; each sending m-section without a share is inactive, so the client stops sending on it.
	bitrates := map[string]int{}
	for _, sec := range offer.sections {
		if sec.kind != webrtc.RTPCodecTypeAudio {
			continue
		}
		if sh := c.sfu.lookupShare(sec.share); sh != nil {
			bitrates[sec.mid] = sh.audioTarget()
		}
	}
	out, err := setOpusAnswerParams(local, bitrates)
	if err == nil {
		out, err = setAnswerInactive(out, offer.unbound)
	}
	if err != nil {
		c.log.Warn("pub answer not edited", "gen", gen, "neg", neg, "err", err)
		return "", newError(CodeInternal, "the pub answer could not be edited")
	}
	p.neg, p.answer = neg, out
	return out, nil
}

// ownsShare reports whether id is a share this Conn publishes that hasn't ended (pending, live or stalled): what a
// pub offer's tracks may bind (02 §5.2).
func (c *Conn) ownsShare(id ShareID) bool {
	sh := c.sfu.lookupShare(id)
	return sh != nil && sh.conn == c
}

// newPubPC creates the pub PC of a gen. Its Pion callbacks only post to the actor (02 §5.4).
func (c *Conn) newPubPC(gen uint32) (*pubPC, error) {
	pc, err := c.sfu.apis.pub.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, err
	}
	p := &pubPC{gen: gen, pc: pc, tracks: map[trackKey]*pubTrack{}}
	pc.OnTrack(func(track *webrtc.TrackRemote, recv *webrtc.RTPReceiver) {
		// Pion hands the track over once: if the Conn is closing and drops the post, the PC is closing too.
		c.post(func() { c.onPubTrack(p, track, recv) })
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		c.post(func() { c.onPCState(PCPub, gen, pc, state) })
	})
	return p, nil
}

// close detaches the PC's tracks from their shares, closes the PeerConnection and waits for the track readers, which
// end when their reads fail.
func (p *pubPC) close(log *slog.Logger) {
	for _, t := range p.tracks {
		t.detach()
	}
	if err := p.pc.Close(); err != nil {
		log.Debug("pub PC close", "gen", p.gen, "err", err)
	}
	p.readers.Wait()
}

// onPubTrack takes a track Pion delivers on pub PC p (posted by OnTrack): it starts the track's readers and attaches
// it to the share its m-section is bound to, if any.
func (c *Conn) onPubTrack(p *pubPC, track *webrtc.TrackRemote, recv *webrtc.RTPReceiver) {
	if c.pub != p {
		return // p was replaced or closed: its tracks have ended
	}
	mid := ""
	for _, tr := range p.pc.GetTransceivers() {
		if tr.Receiver() == recv {
			mid = tr.Mid()
			break
		}
	}
	t := &pubTrack{key: trackKey{mid, track.RID()}, kind: track.Kind(), track: track, recv: recv}
	if old := p.tracks[t.key]; old != nil {
		// The same m-section and rid with a new SSRC: Pion stopped the old receiver, whose readers end by themselves.
		old.detach()
	}
	p.tracks[t.key] = t
	p.readers.Add(2)
	go func() {
		defer p.readers.Done()
		t.readRTP()
	}()
	go func() {
		defer p.readers.Done()
		t.readRTCP()
	}()
	c.log.Debug("pub track", "gen", p.gen, "kind", t.kind.String(), "rid", t.key.rid)
	c.bindTrack(p, t)
}

// rebindTracks maps every known track of p by the binding of the offer just applied: the mapping is read afresh on
// every offer (02 §5.2), and a client reuses an m-section for its next share.
func (c *Conn) rebindTracks(p *pubPC) {
	for _, t := range p.tracks {
		c.bindTrack(p, t)
	}
}

// bindTrack attaches t to the share its m-section is bound to, as a new Layer, and detaches it from any other. A
// track whose m-section carries no share (no binding, another kind than bound, a share that ended, or a rid the SFU
// doesn't forward) stays detached: its readers discard what arrives.
func (c *Conn) bindTrack(p *pubPC, t *pubTrack) {
	var target *Share
	slot, ok := slotFor(t.kind, t.key.rid)
	if b, bound := p.bound[t.key.mid]; ok && bound && b.Kind == t.kind {
		if sh := c.sfu.lookupShare(b.Share); sh != nil && sh.conn == c {
			target = sh
		}
	}
	if cur := t.layer.Load(); cur != nil {
		if cur.share == target {
			return
		}
		t.detach()
	}
	if target == nil {
		return
	}
	l := newLayer(slot, t.kind)
	l.share, l.rid, l.track, l.recv, l.pubPC, l.ssrc = target, t.key.rid, t.track, t.recv, p.pc, uint32(t.track.SSRC())
	if l.kind == webrtc.RTPCodecTypeVideo && l.rid == "" {
		l.rid = ridFull
	}
	replaced, ok := target.attach(l)
	if !ok {
		return // the share ended meanwhile
	}
	if replaced != nil {
		// Another track of this PC fed the slot (checkPubOffer allows one m-section per share and kind, so this is a
		// track Pion re-delivered): it feeds nothing from now on.
		for _, other := range p.tracks {
			other.layer.CompareAndSwap(replaced, nil)
		}
	}
	t.layer.Store(l)
	c.log.Debug("layer attached", "share_id", string(target.id), "layer", slot.String())
}

// detachShare runs on the publishing Conn's actor after one of its shares ended: its tracks stop feeding the share.
func (c *Conn) detachShare(sh *Share) {
	if c.pub == nil {
		return
	}
	for _, t := range c.pub.tracks {
		if l := t.layer.Load(); l != nil && l.share == sh {
			t.detach()
		}
	}
}

// detach takes the track off the share it feeds, if any.
func (t *pubTrack) detach() {
	if l := t.layer.Swap(nil); l != nil {
		l.share.detach(l)
	}
}

// readRTP reads the track's RTP until the track ends (the PC closed, or Pion stopped the receiver).
func (t *pubTrack) readRTP() {
	buf := make([]byte, rtpReadBuffer)
	for {
		if _, _, err := t.track.Read(buf); err != nil {
			return
		}
		t.packets.Add(1)
		// README S41: hand the packet to t.layer (parse, cache, fan-out: 02 §9.1). Without a Layer it is discarded.
	}
}

// readRTCP reads the RTCP of the track's stream until it ends: per rid for simulcast (S4 finding 2). Reading is what
// keeps the publish side's interceptors (NACK generator, receiver reports, TWCC) running. README S41 adds the sender
// reports (02 §9.1).
func (t *pubTrack) readRTCP() {
	buf := make([]byte, rtpReadBuffer)
	for {
		var err error
		if t.key.rid != "" {
			_, _, err = t.recv.ReadSimulcast(buf, t.key.rid)
		} else {
			_, _, err = t.recv.Read(buf)
		}
		if err != nil {
			return
		}
	}
}
