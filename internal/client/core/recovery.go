package core

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/MoonWX/isshoni/internal/client/signal"
	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// Reconnect, resync and the 30 s hold (docs/m2/01-client-core.md §11.7, M1 01 §10.5, §10.6):
//
//   - The hold: when signaling leaves ready, a share's 30 s timer starts. The engine keeps capturing and the pump
//     keeps draining; a pub PC that stays connected keeps sending (a WebSocket drop is not a media drop). Ready
//     again within 30 s stops it; else the share ends server-unreachable.
//   - A resumed welcome: the pending pub offer goes again (the same neg), a pub PC that isn't connected restarts
//     ICE, and the first room.state is reconciled: a share the server lost is published again; the connection's
//     shares that the core doesn't have are stopped. A resumed welcome without the share's room (the connection
//     was taken out of it meanwhile) publishes the share again at once, from room.join (M1 01 §10.5).
//   - A new connection (a server restart, the grace over, a forced token refresh): the pub PC is dropped without a
//     message, room.join, share.start{replaces: the last id} with a new ref, a new Publisher at gen 1. The engine
//     session goes on and the pump asks for keyframes when the new Publisher attaches. The share keeps its local
//     identity; its id changes, and so does the connection's (Status.ConnectionID). A share published again on the
//     connection it was on gets its new Publisher at the next gen instead: the server keeps a connection's pub gen
//     (M1 01 §9 rule 2 resets it only after welcome{resumed: false}).
//   - A request that a drop cut short goes again once ready: share.start with the same ref on the same connection,
//     and a share.stop as the reconcile of the next resumed welcome (leaveAfterResume, stopOrphans).
//
// The connection actor tells the share's actor of every change of signaling (signalReady, signalDown); each message
// carries the count of readies, so the actor, which may begin after some of them (StartShare's steps run first),
// takes each change once.

// timing holds the timers of the core's recovery (§13). The package's tests shorten them (Options.timing).
type timing struct {
	hold               time.Duration // signaling not ready this long ends the share (server-unreachable)
	pcPoll             time.Duration // how often a share's actor looks at its pub PC
	iceRestartAfter    time.Duration // ICE disconnected this long: an ICE restart
	connectWithin      time.Duration // an ICE restart or a rebuilt or republished PC not connected this long: a rebuild
	restartSpacing     time.Duration // at most one ICE restart per this
	rebuildSpacing     time.Duration // at most one rebuild per this
	unreachableSpacing time.Duration // the rebuilds of an unreachable share
	refusedWindow      time.Duration // a second refused offer within this ends the share
	// republishRetry: a republish that failed for want of signaling or a reply, or that the server refused with a
	// retryable code and no retryAfterMs, goes again after this.
	republishRetry time.Duration
}

// defaultTiming are §13's values.
var defaultTiming = timing{
	hold:               30 * time.Second,
	pcPoll:             100 * time.Millisecond,
	iceRestartAfter:    3 * time.Second,
	connectWithin:      15 * time.Second,
	restartSpacing:     5 * time.Second,
	rebuildSpacing:     10 * time.Second,
	unreachableSpacing: 30 * time.Second,
	refusedWindow:      60 * time.Second,
	republishRetry:     time.Second,
}

// resumeStateWait bounds the wait for the room.state that follows a resumed welcome (leaveAfterResume).
const resumeStateWait = time.Second

// recovery is a share's view of signaling and what it owes the server (§11.7). It is the actor's alone.
type recovery struct {
	online  bool                // signaling is ready, as far as the actor knows
	seq     uint64              // the last ready the actor knows of
	conn    string              // that ready's connection id (takeSignal)
	downAt  time.Time           // when signaling left ready, while it isn't (takeSignal)
	hold    *time.Timer         // runs while signaling isn't ready
	resync  bool                // after a resumed welcome: the next room.state is reconciled
	offline *protocol.RoomState // the newest room.state that came while signaling was down

	republish    bool            // the server has no share of ours: publish it again once ready
	republishing bool            // a republish began, until its share is live
	ref, refConn string          // the ref of a republish's share.start that may have reached the server, and its connection
	retryAt      time.Time       // a republish cut short goes again from then on
	retried      bool            // the republish was tried again after a retryable internal error (once, M1 01 §12.1)
	orphans      map[string]bool // the connection's shares that the core stopped
}

// holdC is the hold timer's channel, nil while it doesn't run.
func (r *recovery) holdC() <-chan time.Time {
	if r.hold == nil {
		return nil
	}
	return r.hold.C
}

// startHold starts the hold, from when signaling left ready.
func (r *recovery) startHold(from time.Time, hold time.Duration) {
	if r.hold == nil {
		r.hold = time.NewTimer(max(0, time.Until(from.Add(hold))))
	}
}

// stopHold stops the hold, if it runs.
func (r *recovery) stopHold() {
	if r.hold != nil {
		r.hold.Stop()
		r.hold = nil
	}
}

// takeSignal takes signaling's state as the actor begins: what came before is in it, and the messages about it in
// the inbox are dropped by their count.
func (s *Share) takeSignal() {
	c := s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	s.rec.online, s.rec.seq, s.rec.conn, s.rec.downAt = c.sigReady, c.readySeq, c.connID, c.downAt
}

// begin is the actor's first step: the hold when signaling is down, and a republish when signaling is ready on
// another connection than the one share.start reached (it changed while StartShare's steps ran). Signaling down
// without a time it went down (never ready, which StartShare's isOnline rules out) holds from now.
func (s *Share) begin() *endRequest {
	if !s.rec.online {
		if s.rec.downAt.IsZero() {
			s.rec.downAt = time.Now()
		}
		s.rec.startHold(s.rec.downAt, s.c.timing.hold)
		s.syncState()
		return nil
	}
	if s.connID == "" || s.connID != s.rec.conn {
		s.log.Info("the share's connection is gone: publishing it again", slog.String("share_id", s.params.ShareID))
		s.rec.republish = true
		return s.republish()
	}
	return nil
}

// tick is the actor's regular look: a republish that is due, else the pub PC (pubdriver.go). Nothing once end has
// asked the actor to end the share: its request is next.
func (s *Share) tick(now time.Time) *endRequest {
	if s.ctx.Err() != nil {
		return nil
	}
	if s.rec.republish {
		return s.republish()
	}
	if info := s.drive(now); info != nil {
		return &endRequest{info: *info, stop: true, leave: true}
	}
	return nil
}

// onOffline starts the hold when signaling leaves the ready the actor knows of.
func (s *Share) onOffline(m msgOffline) {
	if m.seq < s.rec.seq || !s.rec.online {
		return // a ready the actor never knew of, or one it knows has ended
	}
	s.rec.online, s.rec.offline = false, nil
	s.rec.startHold(m.at, s.c.timing.hold)
	s.log.Info("signaling is down: the share is held", slog.Duration("hold", s.c.timing.hold))
	s.syncState()
}

// holdOver ends the share when the hold ran out (server-unreachable), unless signaling is ready by now: its welcome
// is then in the inbox.
func (s *Share) holdOver() *endRequest {
	s.rec.hold = nil
	if s.c.sigReadyNow() {
		return nil
	}
	s.log.Info("signaling was down too long: the share ends", slog.Duration("hold", s.c.timing.hold))
	return &endRequest{info: EndInfo{Reason: EndServerUnreachable}, stop: true, leave: true}
}

// onWelcome handles signaling's return: the hold stops, and the share resumes on its connection or is published
// again on a new one.
func (s *Share) onWelcome(m msgWelcome) *endRequest {
	if m.seq <= s.rec.seq {
		return nil // takeSignal had it
	}
	s.rec.seq, s.rec.conn, s.rec.online = m.seq, m.connID, true
	s.rec.stopHold()
	s.rec.retryAt = time.Time{}
	offline := s.rec.offline
	s.rec.offline = nil
	if !m.resumed || m.connID != s.connID {
		// A new connection: the server forgot the room, the share and the pub PC.
		s.log.Info("a new connection: publishing the share again", slog.String("share_id", s.params.ShareID))
		s.rec.republish = true
		return s.republish()
	}
	if m.roomID != s.req.RoomID {
		// The connection was taken out of the share's room while it was away (kicked, the room closed: the hub names
		// the room of a resumed welcome only while the connection is in it), and the share and the pub PC went with
		// it. M1 01 §10.5: join the room again, then publish the share again. Nothing goes to the old pub PC: an offer
		// outside the room would get not_in_room.
		s.log.Info("the connection left the share's room: publishing the share again",
			slog.String("share_id", s.params.ShareID))
		s.rec.republish = true
		return s.republish()
	}
	s.syncState()
	if s.rec.republish {
		return s.republish() // one that a drop cut short, on the same connection: the same ref
	}
	if info := s.onResumed(); info != nil {
		return &endRequest{info: *info, stop: true, leave: true}
	}
	if offline != nil {
		// That room.state came after the drop, possibly the one that follows this welcome: reconciled now, and the
		// next one too (resync).
		return s.reconcile(*offline, true)
	}
	return nil
}

// onResumed is a resumed welcome's part of the pub PC (M1 01 §10.4, §10.5): the pending offer goes again with the
// same neg (the server answers a repeated one with its stored answer), or a PC that isn't connected restarts ICE.
// The next room.state is reconciled.
func (s *Share) onResumed() *EndInfo {
	s.rec.resync = true
	pub := s.publisher()
	switch {
	case pub == nil:
	case s.drv.pending != nil:
		s.resend(s.drv.pending.Gen, s.drv.pending.Neg)
	case s.drv.started && !iceUp(pub.PC()):
		return s.restartICE(time.Now())
	}
	return nil
}

// republish publishes the share again for a server that has no share of ours (§11.7): the pub PC is dropped without
// a message; room.join unless the connection is in the share's room; share.start{replaces: the last id}; a new
// Publisher once the session has its profile (else its first EventLayer makes one). On a new connection the new
// Publisher offers gen 1; on the connection the share was on it offers the next gen with neg 1, as a rebuild does,
// since the server keeps the connection's pub gen (M1 01 §9 rule 2; a gen-1 offer there would be stale, or answered
// for the old PC). A republish's PC that isn't connected within 15 s is rebuilt, as a rebuilt one is. A request that
// signaling's drop or a missing reply cut short goes again later (tick, onWelcome), and so does one the server
// refused with a retryable code (republishFailed); any other refusal (room_not_found, room_full, forbidden,
// share_limit, …) ends the share with its code.
func (s *Share) republish() *endRequest {
	if !s.rec.online || time.Now().Before(s.rec.retryAt) {
		return nil
	}
	s.rec.republish, s.rec.republishing = true, true
	s.rec.resync = false
	switch {
	case s.connID == "" || s.connID != s.rec.conn:
		// A new connection: the server has no pub PC of ours, and gen starts at 1 again.
		if s.publisher() != nil || s.drv.gen != 0 {
			s.closePublisher(false)
			s.drv = pubDriver{}
		}
	case s.publisher() != nil:
		// The same connection: the next gen, taken once (a retry finds no Publisher to close, and keeps it).
		gen := s.drv.gen
		s.closePublisher(false)
		s.drv = pubDriver{gen: gen + 1}
	}
	s.syncState()
	params, conn, err := s.startAgain()
	if err != nil {
		return s.republishFailed(err)
	}
	old := s.params.ShareID
	s.params, s.connID = params, conn
	s.rec.republish, s.rec.ref, s.rec.refConn, s.rec.orphans, s.rec.retried = false, "", "", nil, false
	s.inState = false
	s.update(func(snap *ShareSnapshot) { snap.ID = params.ShareID })
	s.log.Info("share published again", slog.String("share_id", params.ShareID), slog.String("replaces", old))
	if len(s.seen) == 0 {
		return nil
	}
	s.drv.connectBy = time.Now().Add(s.c.timing.connectWithin)
	if err := s.startPublisher(); err != nil {
		if s.ctx.Err() != nil {
			return nil
		}
		s.log.Warn("pub PC start failed", logx.Err(err))
		return &endRequest{info: EndInfo{Reason: EndError, Protocol: protocol.ErrorCodeInternal}, stop: true, leave: true}
	}
	return nil
}

// startAgain is the server's half of a republish, with the room operations serialized (c.roomMu): room.join unless
// the connection is in the share's room, then share.start{replaces}. It returns the new ShareParams and the
// connection they are on ("" when signaling changed connections meanwhile, which the next welcome sorts out).
func (s *Share) startAgain() (protocol.ShareParams, string, error) {
	c := s.c
	c.roomMu.Lock()
	defer c.roomMu.Unlock()
	conn := c.sigConnID()
	if c.roomID() != s.req.RoomID {
		if err := c.join(s.ctx, s.req.RoomID); err != nil {
			return protocol.ShareParams{}, "", err
		}
	}
	if s.rec.ref == "" || s.rec.refConn != conn {
		s.rec.ref, s.rec.refConn = newRef(), conn
	}
	s.mu.Lock()
	preset := s.snap.Preset
	s.mu.Unlock()
	start := protocol.ShareStart{Kind: shareKind(s.req.Source.Kind), Label: s.req.Label, Preset: preset,
		Audio: s.req.Audio, Ref: s.rec.ref, Replaces: s.params.ShareID}
	var params protocol.ShareParams
	if err := c.request(s.ctx, protocol.MessageTypeShareStart, start, &params); err != nil {
		return params, "", err
	}
	if c.sigConnID() != conn {
		conn = ""
	}
	return params, conn, nil
}

// republishFailed handles a republish's failed request: the share's end lets it be; anything but the server's
// refusal (signaling down, no reply) is tried again. Of the server's refusals (M1 01 §12.1), not_in_room only
// forgets the room, so the next try joins; rate_limited (room.join and share.start have their buckets) is tried
// again after its retryAfterMs (1 s when absent); a retryable internal is tried again once; any other code ends the
// share with it, room_full included, which the user has to be told of.
func (s *Share) republishFailed(err error) *endRequest {
	if s.ctx.Err() != nil {
		return nil // the share is ending: the actor takes that request next
	}
	var perr *protocol.Error
	switch {
	case errors.As(err, &perr) && perr.Code == protocol.ErrorCodeNotInRoom:
		s.c.forgetRoom(s.req.RoomID)
	case errors.As(err, &perr) && perr.Code == protocol.ErrorCodeRateLimited:
		wait := s.c.timing.republishRetry
		if perr.RetryAfterMs > 0 {
			wait = time.Duration(perr.RetryAfterMs) * time.Millisecond
		}
		s.rec.retryAt = time.Now().Add(wait)
		s.log.Info("publishing the share again: rate limited", slog.Duration("retry_after", wait))
	case errors.As(err, &perr) && perr.Code == protocol.ErrorCodeInternal && perr.Retryable && !s.rec.retried:
		s.rec.retried = true
		s.rec.retryAt = time.Now().Add(s.c.timing.republishRetry)
		s.log.Info("publishing the share again: the server failed, trying once more")
	case errors.As(err, &perr):
		s.log.Warn("publishing the share again was refused", slog.String("code", string(perr.Code)))
		return &endRequest{info: EndInfo{Reason: EndError, Protocol: perr.Code}, leave: true}
	default:
		s.rec.retryAt = time.Now().Add(s.c.timing.republishRetry)
		s.log.Info("publishing the share again: not yet", logx.Err(err))
	}
	return nil
}

// stopOrphans stops the shares of the share's connection that the core doesn't have (§11.7): one that a lost
// share.start reply left, for example. A republish that may have reached the server keeps them: its share.start
// with the same ref takes that share over.
func (s *Share) stopOrphans(st protocol.RoomState) {
	if s.connID == "" || s.rec.republish {
		return
	}
	for _, si := range st.Shares {
		if si.ConnectionID != s.connID || si.ID == s.params.ShareID || s.rec.orphans[si.ID] {
			continue
		}
		if s.rec.orphans == nil {
			s.rec.orphans = map[string]bool{}
		}
		s.rec.orphans[si.ID] = true
		s.log.Info("stopping a share the core doesn't have", slog.String("share_id", si.ID))
		ctx, cancel := context.WithTimeout(s.ctx, cleanupTimeout)
		if err := s.c.request(ctx, protocol.MessageTypeShareStop, protocol.ShareStop{ShareID: si.ID}, nil); err != nil {
			s.log.Debug("share.stop", logx.Err(err))
		}
		cancel()
	}
}

// syncState sets the snapshot's state from what the actor knows (§11.4, 03 §10.1): reconnecting while signaling is
// down (the hold), while a republish hasn't gone live and while the pub PC, connected before, isn't; unreachable
// after 5 rebuilds without connecting; else live once the share has been, starting before.
func (s *Share) syncState() {
	state := StateStarting
	switch {
	case !s.rec.online:
		state = StateReconnecting
	case s.drv.unreachable:
		state = StateUnreachable
	case s.rec.republishing || s.drv.recovering():
		state = StateReconnecting
	case s.live:
		state = StateLive
	}
	s.update(func(snap *ShareSnapshot) {
		if snap.State != StateEnded {
			snap.State = state
		}
	})
}

// ---- the connection's side ----

// signalReady records that signaling is ready on connection connID, resumed or not and in room roomID (the
// welcome's), and tells the share. The connection actor calls it before Status says online, so a StartShare that
// sees online finds the ready recorded.
func (c *Client) signalReady(connID string, resumed bool, roomID string) {
	c.mu.Lock()
	c.readySeq++
	c.sigReady, c.connID = true, connID
	seq, sh := c.readySeq, c.share
	c.wakeReadyLocked()
	c.mu.Unlock()
	if sh != nil {
		sh.post(msgWelcome{seq: seq, connID: connID, resumed: resumed, roomID: roomID})
	}
}

// signalDown records that signaling left ready and tells the share; it does nothing while signaling isn't ready.
func (c *Client) signalDown() {
	now := time.Now()
	c.mu.Lock()
	if !c.sigReady {
		c.mu.Unlock()
		return
	}
	c.sigReady, c.downAt = false, now
	seq, sh := c.readySeq, c.share
	c.wakeReadyLocked()
	c.mu.Unlock()
	if sh != nil {
		sh.post(msgOffline{seq: seq, at: now})
	}
}

func (c *Client) wakeReadyLocked() {
	close(c.readyCh)
	c.readyCh = make(chan struct{})
}

// readySeqNow returns the count of signaling's readies so far.
func (c *Client) readySeqNow() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readySeq
}

// sigReadyNow reports whether signaling is ready, as the connection actor has seen it.
func (c *Client) sigReadyNow() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sigReady
}

// waitReady waits until signaling is ready on a ready after the after-th: the reconnect after a drop.
func (c *Client) waitReady(ctx context.Context, after uint64) error {
	for {
		c.mu.Lock()
		ok, ch := c.sigReady && c.readySeq > after, c.readyCh
		c.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// lostSignaling reports whether a request failed because signaling went down (or wasn't ready): it can go again
// once ready.
func lostSignaling(err error) bool {
	return errors.Is(err, signal.ErrConnectionLost)
}

// sigConnID returns the connection id of the signaling client's last welcome, "" without a client. The signaling
// client takes it as the welcome arrives, so a request that succeeds between two equal answers went out on it.
func (c *Client) sigConnID() string {
	c.mu.Lock()
	sig := c.sig
	c.mu.Unlock()
	if sig == nil {
		return ""
	}
	return sig.Welcome().ConnectionID
}

// probe has the signaling client ping at once (the pub PC's ICE went disconnected: is signaling down too?).
func (c *Client) probe() {
	c.mu.Lock()
	sig := c.sig
	c.mu.Unlock()
	if sig != nil {
		sig.Probe()
	}
}

// forgetRoom puts the room view back to no room, when it is id: the server said the connection isn't in it.
func (c *Client) forgetRoom(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.room.id == id {
		c.room.reset("")
	}
}

// leaveAfterResume takes the connection out of the room that a resumed welcome kept it in, when it has no share any
// more (D12): a share that ended while signaling was down could neither stop on the server nor leave (§11.7). The
// connection's shares that the room.state after the welcome lists are stopped with share.stop (seq is the room
// view's count of states at the welcome), then room.leave ends whatever is left. It runs beside the connection
// actor, as a StartShare does, and gives way to one.
func (c *Client) leaveAfterResume(roomID string, seq uint64) {
	if !c.startOp() {
		return
	}
	go func() {
		defer c.ops.Done()
		c.roomMu.Lock()
		defer c.roomMu.Unlock()
		if c.ActiveShare() != nil || c.roomID() != roomID {
			return
		}
		ctx, cancel := context.WithTimeout(c.life, cleanupTimeout)
		defer cancel()
		wctx, wcancel := context.WithTimeout(ctx, resumeStateWait)
		_ = c.waitRoom(wctx, roomID, func(_ protocol.RoomState, n uint64) bool { return n > seq })
		wcancel()
		c.mu.Lock()
		st, me := c.room.state, c.connID
		c.mu.Unlock()
		for _, si := range st.Shares {
			if si.ConnectionID != me || ctx.Err() != nil {
				continue
			}
			c.log.Info("stopping a share the core doesn't have", slog.String("share_id", si.ID))
			if err := c.request(ctx, protocol.MessageTypeShareStop, protocol.ShareStop{ShareID: si.ID}, nil); err != nil {
				c.log.Debug("share.stop after the resume", logx.Err(err))
			}
		}
		if err := c.leave(ctx); err != nil {
			c.log.Debug("room.leave after the resume", logx.Err(err))
		}
	}()
}
