package sfuplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/sfu"
	"github.com/MoonWX/isshoni/internal/server/signal"
)

// callTimeout bounds each call of the SFU (01 §15.4). A Conn's actor normally answers at once; when its command
// queue stays full, the SFU gives up after the same 5 s with sfu.busy (02 §5.4).
const callTimeout = 5 * time.Second

// callContext returns the context of one SFU call. The MediaPeer methods take no context (the hub's connection actor
// calls them and waits), so each bounds its own work with callTimeout.
func callContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), callTimeout)
}

// peer is the media side of one connection in one room: the hub's signal.MediaPeer and the SFU's sfu.Signaler of the
// same *sfu.Conn (01 §15.4). The hub calls the MediaPeer methods from the connection's actor only, never
// concurrently; the SFU calls the Signaler methods from the Conn's actor.
type peer struct {
	plane *Plane
	id    sfu.ConnID
	sink  signal.MediaSink
	log   *slog.Logger
	// conn is set by NewPeer once SFU.Join has returned. Only the MediaPeer methods read it.
	conn conn
	// media is the last state that the SFU reported of each share the Conn publishes (roomEvents.ShareUpdated).
	// Plane.mu guards it.
	media map[sfu.ShareID]sfu.ShareState
}

var (
	_ signal.MediaPeer = (*peer)(nil)
	_ sfu.Signaler     = (*peer)(nil)
)

// ---- calls: signal.MediaPeer → *sfu.Conn ----

// CreateShare implements signal.MediaPeer with Conn.StartShare: the share is created pending under the hub's id,
// and the reply tells the sharer how to encode it.
func (p *peer) CreateShare(shareID string, meta protocol.ShareStart) (protocol.ShareParams, error) {
	at := requestSite("StartShare")
	preset, ok := sfuPreset(meta.Preset)
	if !ok {
		return protocol.ShareParams{}, at.badRequest("preset")
	}
	source, ok := sfuSource(meta.Kind)
	if !ok {
		return protocol.ShareParams{}, at.badRequest("kind")
	}
	ctx, cancel := callContext()
	defer cancel()
	sp, err := p.conn.StartShare(ctx, sfu.StartShareParams{
		ID: sfu.ShareID(shareID), Preset: preset, Audio: meta.Audio, Source: source,
	})
	if err != nil {
		return protocol.ShareParams{}, p.failed(err, at)
	}
	return p.shareParams(shareID, sp), nil
}

// UpdateShare implements signal.MediaPeer with Conn.UpdateShare. Only a preset reaches the SFU: a label is the
// hub's, and an update without a preset returns the share's current ShareParams.
func (p *peer) UpdateShare(shareID string, meta protocol.ShareUpdate) (protocol.ShareParams, error) {
	at := requestSite("UpdateShare")
	var u sfu.ShareUpdate
	if meta.Preset != "" {
		preset, ok := sfuPreset(meta.Preset)
		if !ok {
			return protocol.ShareParams{}, at.badRequest("preset")
		}
		u.Preset = &preset
	}
	ctx, cancel := callContext()
	defer cancel()
	sp, err := p.conn.UpdateShare(ctx, sfu.ShareID(shareID), u)
	if err != nil {
		return protocol.ShareParams{}, p.failed(err, at)
	}
	return p.shareParams(shareID, sp), nil
}

// shareParams converts the SFU's ShareParams: the codec is "h264/" plus the profile, and each encoding's layer comes
// from its rid.
func (p *peer) shareParams(shareID string, sp sfu.ShareParams) protocol.ShareParams {
	codec, ok := codecKey(sp.Profile)
	if !ok || codec == "" {
		p.log.Error("share params without a usable H.264 profile", "share_id", shareID)
	}
	enc := wireEncodings(sp.Encodings)
	if len(enc) != len(sp.Encodings) {
		p.log.Error("share params with an unknown rid", "share_id", shareID)
	}
	return protocol.ShareParams{ShareID: shareID, Codec: codec, Encodings: enc, AudioBitrate: sp.AudioBitrate}
}

// EndShare implements signal.MediaPeer with Conn.StopShare. The end reasons are the same strings in both packages.
func (p *peer) EndShare(shareID string, reason protocol.EndReason) {
	ctx, cancel := callContext()
	defer cancel()
	err := p.conn.StopShare(ctx, sfu.ShareID(shareID), sfu.EndReason(reason))
	p.dropped("StopShare", err, "share_id", shareID)
}

// HandleOffer implements signal.MediaPeer with Conn.HandleOffer: a pub offer, answered synchronously with every
// server candidate in the SDP. Its errors have scope pc with the offer's pc, gen and neg; the one exception is
// codec_not_supported, which names the share that can't be published (scope share). The client waits for the
// answer, so every error is sent, sfu.closed too (not_in_room).
func (p *peer) HandleOffer(o protocol.PCOffer) (protocol.PCAnswer, error) {
	at := pcSite("HandleOffer", o.PC, o.Gen, o.Neg)
	at.reply = true
	kind, ok := sfuPCKind(o.PC)
	if !ok {
		return protocol.PCAnswer{}, at.badRequest("pc")
	}
	tracks := make([]sfu.TrackBinding, len(o.Tracks))
	for i, t := range o.Tracks {
		k, ok := sfuTrackKind(t.Kind)
		if !ok {
			return protocol.PCAnswer{}, at.badRequest("tracks")
		}
		tracks[i] = sfu.TrackBinding{MID: t.MID, Share: sfu.ShareID(t.ShareID), Kind: k}
	}
	ctx, cancel := callContext()
	defer cancel()
	answer, err := p.conn.HandleOffer(ctx, kind, o.Gen, o.Neg, o.SDP.Reveal(), tracks)
	if err != nil {
		return protocol.PCAnswer{}, p.failed(err, at)
	}
	return protocol.PCAnswer{PC: o.PC, Gen: o.Gen, Neg: o.Neg, SDP: protocol.SDP(answer)}, nil
}

// HandleAnswer implements signal.MediaPeer with Conn.HandleAnswer: the client's answer to a sub offer. A stale one
// (sfu.stale_answer) is dropped silently.
func (p *peer) HandleAnswer(a protocol.PCAnswer) error {
	at := pcSite("HandleAnswer", a.PC, a.Gen, a.Neg)
	kind, ok := sfuPCKind(a.PC)
	if !ok {
		return at.badRequest("pc")
	}
	ctx, cancel := callContext()
	defer cancel()
	return p.failed(p.conn.HandleAnswer(ctx, kind, a.Gen, a.Neg, a.SDP.Reveal()), at)
}

// AddICE implements signal.MediaPeer with Conn.AddICECandidate. The end-of-candidates marker, an absent or empty
// candidate, is ignored: the SFU needs none.
func (p *peer) AddICE(c protocol.PCICE) error {
	if c.Candidate == nil || c.Candidate.Candidate == "" {
		return nil
	}
	at := pcSite("AddICECandidate", c.PC, c.Gen, 0)
	kind, ok := sfuPCKind(c.PC)
	if !ok {
		return at.badRequest("pc")
	}
	ctx, cancel := callContext()
	defer cancel()
	return p.failed(p.conn.AddICECandidate(ctx, kind, c.Gen, webrtc.ICECandidateInit{
		Candidate:        c.Candidate.Candidate,
		SDPMid:           c.Candidate.SDPMid,
		SDPMLineIndex:    c.Candidate.SDPMLineIndex,
		UsernameFragment: c.Candidate.UsernameFragment,
	}), at)
}

// Restart implements signal.MediaPeer: the client asks for an ICE restart of its sub PC (Conn.RestartICE) or for a
// new one (Conn.ResetPC). The pub PC has no such call: its restarts are the client's own offers.
func (p *peer) Restart(r protocol.PCRestart) error {
	call := "RestartICE"
	if r.Mode == protocol.RestartModeRebuild {
		call = "ResetPC"
	}
	at := pcSite(call, r.PC, r.Gen, 0)
	kind, ok := sfuPCKind(r.PC)
	if !ok {
		return at.badRequest("pc")
	}
	ctx, cancel := callContext()
	defer cancel()
	switch r.Mode {
	case protocol.RestartModeICE:
		return p.failed(p.conn.RestartICE(ctx, kind, r.Gen), at)
	case protocol.RestartModeRebuild:
		return p.failed(p.conn.ResetPC(ctx, kind, r.Gen), at)
	}
	return at.badRequest("mode")
}

// ClosePC implements signal.MediaPeer with Conn.ClosePC: the client closed a PC on purpose. The hub calls it for
// pub only, after it has ended the shares of that PC.
func (p *peer) ClosePC(c protocol.PCClose) error {
	at := pcSite("ClosePC", c.PC, c.Gen, 0)
	kind, ok := sfuPCKind(c.PC)
	if !ok {
		return at.badRequest("pc")
	}
	ctx, cancel := callContext()
	defer cancel()
	return p.failed(p.conn.ClosePC(ctx, kind, c.Gen), at)
}

// Subscribe implements signal.MediaPeer with Conn.UpdateSubscriptions: the wants go to the SFU in one call, so it
// applies them in one step. An item that fails with sfu.share_not_found (its share just ended) is returned as
// ignored. Any other item error fails the whole request with its mapped code; the SFU has applied the other items
// all the same.
func (p *peer) Subscribe(wants []protocol.SubscriptionWant) (ignored []string, err error) {
	at := requestSite("UpdateSubscriptions")
	items := make([]sfu.SubscriptionUpdate, len(wants))
	for i, w := range wants {
		video, ok := sfuQuality(w.Video)
		if !ok {
			return nil, at.badRequest("subs")
		}
		items[i] = sfu.SubscriptionUpdate{
			Share: sfu.ShareID(w.ShareID), Video: video, Audio: w.Audio == protocol.AudioStateOn,
		}
	}
	ctx, cancel := callContext()
	defer cancel()
	errs, err := p.conn.UpdateSubscriptions(ctx, items)
	if err != nil {
		return nil, p.failed(err, at)
	}
	for i, itemErr := range errs {
		if itemErr == nil || i >= len(wants) {
			continue
		}
		var se *sfu.Error
		if errors.As(itemErr, &se) && se.Code == sfu.CodeShareNotFound {
			ignored = append(ignored, wants[i].ShareID)
			continue
		}
		return nil, p.failed(itemErr, at)
	}
	return ignored, nil
}

// SetCaps implements signal.MediaPeer with Conn.SetDecodeCaps: the H.264 profiles the client can decode now.
func (p *peer) SetCaps(c protocol.Caps) {
	ctx, cancel := callContext()
	defer cancel()
	p.dropped("SetDecodeCaps", p.conn.SetDecodeCaps(ctx, decodeCaps(c)))
}

// Resync implements signal.MediaPeer with Conn.Resync, after a WebSocket resume: the SFU sends an outstanding sub
// offer, the current subscription states and the hints again, all through the Signaler.
func (p *peer) Resync() { p.conn.Resync() }

// Stats implements signal.MediaPeer with Conn.Stats.
func (p *peer) Stats() protocol.ServerStats { return wireStats(p.conn.Stats()) }

// Close implements signal.MediaPeer with Conn.Close: the Conn leaves its room and closes both PCs. The hub has
// already ended the peer's shares with its own reason, so "left" reaches only the SFU's log.
func (p *peer) Close() {
	p.conn.Close(sfu.EndReason(protocol.EndReasonLeft))
	p.plane.forget(p)
}

// ---- events: sfu.Signaler → signal.MediaSink ----

// SendOffer implements sfu.Signaler: a sub offer goes to the sink with its tracks binding. The server offers on sub
// only (01 §9 rule 1).
func (p *peer) SendOffer(pc sfu.PCKind, gen, neg uint32, sdp string, tracks []sfu.TrackBinding) {
	if pc != sfu.PCSub {
		p.log.Error("offer dropped: the server offers on the sub PC only", "pc", pc.String())
		return
	}
	refs := wireTracks(tracks)
	if len(refs) != len(tracks) {
		p.log.Error("sub offer with a track that is neither video nor audio", "gen", gen, "neg", neg)
	}
	p.sink.Offer(protocol.PCOffer{PC: protocol.PCKindSub, Gen: gen, Neg: neg, SDP: protocol.SDP(sdp), Tracks: refs})
}

// SendEvent implements sfu.Signaler (01 §15.4 "Events"). The SFU decides what every event says; the peer only
// converts it.
func (p *peer) SendEvent(ev sfu.Event) {
	switch ev := ev.(type) {
	case sfu.SubscriptionStateEvent:
		p.subscriptionState(ev)
	case sfu.CodecPolicyEvent:
		// A profile switch doesn't change the encodings, so the hint carries only the codec.
		codec, ok := codecKey(ev.Profile)
		if !ok || codec == "" {
			p.log.Error("codec policy without a usable H.264 profile", "share_id", string(ev.Share))
			return
		}
		p.sink.QualityHint(protocol.QualityHint{
			ShareID: string(ev.Share), Reason: protocol.HintReasonCodec, Codec: codec,
		})
	case sfu.QualityHintEvent:
		enc := wireEncodings(ev.Encodings)
		if len(enc) != len(ev.Encodings) {
			p.log.Error("quality hint with an unknown rid", "share_id", string(ev.Share))
		}
		reason := protocol.HintReason(ev.Reason) // "admin" or "viewers": the same strings on the wire
		if !reason.Valid() {
			p.log.Warn("quality hint with an unknown reason", "share_id", string(ev.Share), "reason", ev.Reason)
		}
		p.sink.QualityHint(protocol.QualityHint{
			ShareID: string(ev.Share), Reason: reason, Encodings: enc, MaxBitrate: int64(ev.MaxBitrate),
		})
	case sfu.PCStateEvent:
		// The client drives every other recovery itself (01 §10.4): a failed pub PC, or one that missed its handshake,
		// is the one state the server acts on, by asking for a new pub PC. Sub PC states send nothing.
		if ev.PC == sfu.PCPub && ev.State == webrtc.PeerConnectionStateFailed.String() {
			p.sink.RestartRequest(protocol.PCRestart{
				PC: protocol.PCKindPub, Gen: ev.Gen,
				Mode: protocol.RestartModeRebuild, Reason: protocol.RestartReasonFailed,
			})
		}
	case sfu.ErrorEvent:
		p.errorEvent(ev)
	default:
		p.log.Error("unknown SFU event", "type", fmt.Sprintf("%T", ev))
	}
}

// subscriptionState turns a SubscriptionStateEvent into a subscribe.status entry.
func (p *peer) subscriptionState(ev sfu.SubscriptionStateEvent) {
	video, ok := wireLayer(ev.Forwarded)
	requested, ok2 := wireLayer(ev.Requested)
	if !ok || !ok2 {
		p.log.Error("subscription state with an unknown quality", "share_id", string(ev.Share))
		return
	}
	reason, known := statusReason(ev)
	if !known {
		p.log.Warn("subscription state with an unknown reason", "share_id", string(ev.Share),
			"reason", string(ev.Reason))
	}
	p.sink.SubscriptionStatus([]protocol.SubscriptionStatus{{
		ShareID:        string(ev.Share),
		Video:          video,
		Audio:          wireAudio(ev.Audio),
		RequestedVideo: requested,
		Reason:         reason,
	}})
}

// errorEvent turns an ErrorEvent, a failure that no call returns, into an error notification with the event's
// scope: pc with the PC it names, share or subscription with the share of the error.
func (p *peer) errorEvent(ev sfu.ErrorEvent) {
	if ev.Err == nil {
		p.log.Error("SFU error event without an error", "scope", ev.Scope)
		return
	}
	at, ok := eventSite(ev.Scope)
	if !ok {
		p.log.Error("SFU error event with an unknown scope", "scope", ev.Scope, "err", ev.Err)
		return
	}
	if e, sent := p.wireError(ev.Err, at); sent {
		p.sink.Error(e)
	}
}
