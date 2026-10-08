package signal

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// Shares (01 §4.4, §8.7) and desired subscriptions (01 §8.9).
//
// The hub owns the share lifecycle: share.start creates a share in starting, the SFU's media facts
// (MediaSink.ShareMedia) move it between live and stalled, and it ends on share.stop, pc.close, a pub offer that no
// longer lists its tracks, either 30 s timeout, or when its connection leaves the room. The SFU never ends a share on
// its own (02 §5.3). A share is published through one connection, whose MediaPeer holds its media, and belongs to
// that connection's user: any connection of the user in the room may update or stop it.
//
// Share records live in their room, under the room's lock. Everything that calls a MediaPeer for a share runs on the
// actor of the share's publishing connection, which is the only goroutine that may call that peer: a request from
// another connection of the user is handed to that actor (endAtOwner, updateShareOn).

// newShareID returns a new share id, "s_" + 16 base32 characters (80 random bits, 01 §4.1). Ids are random, not
// sequential, so a stale id from before a server restart never matches a new share.
func newShareID() string {
	var raw [connIDRawLen]byte
	_, _ = rand.Read(raw[:]) // crypto/rand.Read never fails (it crashes the program instead)
	return "s_" + idEncoding.EncodeToString(raw[:])
}

// share is one share in a room (01 §4.4), guarded by the room's lock. info holds what room.state shows, except
// Watchers, which each snapshot computes from the members' desired subscriptions.
type share struct {
	info protocol.ShareInfo
	// ref is share.start's idempotency key on the publishing connection, and params the ShareParams of the latest
	// share.start or share.update: a share.start with the same ref gets them again (01 §8.7).
	ref    string
	params protocol.ShareParams
	// peerSeq is the publishing connection's peerSeq when the share was created: a call that another connection's
	// actor hands to the publishing one is dropped when that connection has another MediaPeer by then.
	peerSeq uint64
	// bound: a pub offer that listed the share's tracks was applied. A later pub offer without them, or a pc.close,
	// ends the share; a share that no offer has bound yet is left to its start timeout (01 §8.7).
	bound bool
	// announced: the share went live once, and room.event share.started went out (once per share, 01 §8.6).
	announced bool
	// deadline is when the share ends with media_timeout: StartedAt + ShareStartTimeout while starting, the moment
	// it stalled + StalledTimeout while stalled, zero while live (01 §4.4). The publishing connection's actor keeps
	// its share timer at the earliest deadline of its shares (armShareTimer).
	deadline time.Time
}

// applyMedia records what a media event says the SFU receives for the share (01 §8.5): the layers, the codec (an
// event without one keeps the last one) and whether an audio track arrives. It reports whether anything changed.
func (s *share) applyMedia(ev ShareMediaEvent) bool {
	changed := false
	if !slices.Equal(s.info.Layers, ev.Layers) {
		s.info.Layers = ev.Layers
		changed = true
	}
	if ev.Codec != "" && s.info.Codec != ev.Codec {
		s.info.Codec = ev.Codec
		changed = true
	}
	if s.info.Audio != ev.Audio {
		s.info.Audio = ev.Audio
		changed = true
	}
	return changed
}

// setStatusLocked changes the share's status and its count in isshoni_shares{status}.
func (r *room) setStatusLocked(s *share, st protocol.ShareStatus) {
	r.h.metrics.shareStatus(s.info.Status, st)
	s.info.Status = st
}

// connLocked returns the connection connID of user userID in the room, or nil.
func (r *room) connLocked(userID, connID string) *conn {
	if p := r.participants[userID]; p != nil {
		for _, m := range p.members {
			if m.c.id == connID {
				return m.c
			}
		}
	}
	return nil
}

// userSharesLocked counts the user's shares in the room, whatever their status.
func (r *room) userSharesLocked(userID string) int {
	n := 0
	for _, s := range r.shares {
		if s.info.UserID == userID {
			n++
		}
	}
	return n
}

// endShares ends, with reason, the shares of the room that pick selects (in room.state order): they leave room.state,
// and everyone in the room gets room.event share.stopped. It returns the ended shares, which the caller ends at
// their publishing connection's MediaPeer.
func (r *room) endShares(reason protocol.EndReason, pick func(*share) bool) []*share {
	now := time.Now()
	var o outbox
	var ended []*share
	r.mu.Lock()
	for _, s := range r.sortedSharesLocked() {
		if pick(s) {
			r.endShareLocked(&o, s, reason, now)
			ended = append(ended, s)
		}
	}
	if len(ended) > 0 {
		r.changedLocked(&o)
	}
	r.mu.Unlock()
	o.send()
	for _, s := range ended {
		r.h.logShareEnded(r, s, reason)
	}
	return ended
}

// logShareEnded writes the INFO line of a share's end (01 §18). s has left the room: nothing changes it any more.
func (h *Hub) logShareEnded(r *room, s *share, reason protocol.EndReason) {
	h.log.Info("share ended", slog.String("share_id", s.info.ID), slog.String("room_id", r.id),
		slog.String("conn_id", s.info.ConnectionID), slog.String("user_id", s.info.UserID),
		slog.String("reason", string(reason)))
}

// shareLimits are the limits a new share is checked against (01 §8.7).
type shareLimits struct {
	user int // the user's shares across all rooms (Limits.MaxSharesPerUser)
	room int // the room's shares; 0 = none (Policy.MaxRoomShares)
}

// admitShare checks whether userID may start another share in room r (01 §8.7): the user's shares across all rooms,
// whatever their status, are below lim.user (else share_limit with params {limit, per: "user"}), and the room's
// shares are below lim.room when that is set (per: "room"). When s is not nil and the checks pass, it adds s to the
// room in the same step, under the hub's and the room's locks, so two share.starts never pass the same limit.
// A room that CloseRoom has closed takes no more shares: not_in_room, as the connection is about to learn.
func (h *Hub) admitShare(r *room, userID string, lim shareLimits, s *share) *protocol.Error {
	var o outbox
	var e *protocol.Error
	h.mu.Lock()
	n := 0
	for _, other := range h.rooms {
		if other != r {
			other.mu.Lock()
			n += other.userSharesLocked(userID)
			other.mu.Unlock()
		}
	}
	r.mu.Lock()
	n += r.userSharesLocked(userID)
	switch {
	case r.closed:
		pe := protocol.NewError(protocol.ErrorCodeNotInRoom, protocol.ErrorScopeRequest)
		e = &pe
	case n >= lim.user:
		e = shareLimit(lim.user, "user")
	case lim.room > 0 && len(r.shares) >= lim.room:
		e = shareLimit(lim.room, "room")
	case s != nil:
		r.shares[s.info.ID] = s
		h.metrics.shareStatus("", s.info.Status)
		r.changedLocked(&o)
	}
	r.mu.Unlock()
	h.mu.Unlock()
	o.send()
	return e
}

func shareLimit(limit int, per string) *protocol.Error {
	e := protocol.NewError(protocol.ErrorCodeShareLimit, protocol.ErrorScopeRequest)
	e.Params = map[string]any{"limit": limit, "per": per}
	return &e
}

// rejectMedia answers request env with the error of a MediaPeer call: the *protocol.Error the adapter mapped
// (01 §15.4), or error{internal} with a logged ref for anything else.
func (c *conn) rejectMedia(env protocol.Envelope, call string, err error) {
	var pe *protocol.Error
	if errors.As(err, &pe) {
		c.reject(env, *pe)
		return
	}
	c.reject(env, c.h.internalError(fmt.Errorf("%s: %w", call, err), protocol.ErrorScopeRequest,
		slog.String("conn_id", c.id), slog.String("type", string(env.Type))))
}

// onShareStart handles share.start (01 §8.7). The connection must be in a room (not_in_room; dispatch has checked
// the publishing role). A ref that a share of this connection already has returns that share's ShareParams again,
// so a reply lost in a reconnect can be retried. Otherwise the limits are checked (share_limit), the MediaPeer
// creates the share and returns how to encode it, and the share joins the room in starting: room.state shows it, and
// it ends with media_timeout unless its first keyframe arrives within ShareStartTimeout. The ok goes out before any
// room.state with the new share (01 §7): the snapshot reaches this connection through its inbox.
func (c *conn) onShareStart(env protocol.Envelope) {
	v, ok := decode[protocol.ShareStart](c, env)
	if !ok || !c.requireRoom(env) {
		return
	}
	r := c.room
	r.mu.Lock()
	var again *protocol.ShareParams
	for _, s := range r.shares {
		if s.info.ConnectionID == c.id && s.ref == v.Ref {
			p := s.params
			again = &p
			break
		}
	}
	r.mu.Unlock()
	if again != nil {
		c.reply(env, *again)
		return
	}
	lim := shareLimits{user: c.h.cfg.Limits.MaxSharesPerUser, room: c.h.policy().MaxRoomShares}
	if e := c.h.admitShare(r, c.userID, lim, nil); e != nil {
		c.reject(env, *e)
		return
	}
	id := newShareID()
	params, err := c.peer.CreateShare(id, v)
	if err != nil {
		c.rejectMedia(env, "create share", err)
		return
	}
	params.ShareID = id
	now := time.Now()
	s := &share{
		info: protocol.ShareInfo{
			ID:           id,
			UserID:       c.userID,
			ConnectionID: c.id,
			Kind:         v.Kind,
			Label:        v.Label,
			Preset:       v.Preset,
			Audio:        v.Audio,
			Status:       protocol.ShareStatusStarting,
			StartedAt:    now,
			Replaces:     v.Replaces,
		},
		ref:      v.Ref,
		params:   params,
		peerSeq:  c.peerSeq,
		deadline: now.Add(c.h.cfg.ShareStartTimeout),
	}
	// Another connection of the user, or of the room, may have taken the last slot while the MediaPeer worked.
	if e := c.h.admitShare(r, c.userID, lim, s); e != nil {
		c.peer.EndShare(id, protocol.EndReasonStopped)
		c.reject(env, *e)
		return
	}
	c.reply(env, params)
	c.armShareTimer()
	c.armStats()
	c.h.log.Info("share started", slog.String("share_id", id), slog.String("room_id", r.id),
		slog.String("conn_id", c.id), slog.String("user_id", c.userID), slog.String("kind", string(v.Kind)),
		slog.String("preset", string(v.Preset)), slog.Bool("replaces", v.Replaces != ""))
}

// onShareUpdate handles share.update (01 §8.7): it changes the label or the preset of a share of the connection's
// user in its room (share_not_found; forbidden for another user's) and replies with the new ShareParams, which the
// share's publishing connection's MediaPeer computes.
func (c *conn) onShareUpdate(env protocol.Envelope) {
	v, ok := decode[protocol.ShareUpdate](c, env)
	if !ok || !c.requireRoom(env) {
		return
	}
	r := c.room
	var owner *conn
	var seq uint64
	code := protocol.ErrorCode("")
	r.mu.Lock()
	switch s := r.shares[v.ShareID]; {
	case s == nil:
		code = protocol.ErrorCodeShareNotFound
	case s.info.UserID != c.userID:
		code = protocol.ErrorCodeForbidden
	default:
		owner, seq = r.connLocked(s.info.UserID, s.info.ConnectionID), s.peerSeq
	}
	r.mu.Unlock()
	if code != "" {
		c.reject(env, protocol.NewError(code, protocol.ErrorScopeRequest))
		return
	}
	params, err := c.updateShareOn(owner, r, seq, v)
	if err != nil {
		c.rejectMedia(env, "update share", err)
		return
	}
	c.reply(env, params)
}

// errShareGone is share.update's error when the share ended, or its publishing connection left, before the update
// reached the MediaPeer.
func errShareGone() error {
	e := protocol.NewError(protocol.ErrorCodeShareNotFound, protocol.ErrorScopeRequest)
	return &e
}

// errOwnerBusy is share.update's error when the share's publishing connection did not get to it in time.
var errOwnerBusy = errors.New("signal: the share's connection did not answer in time")

// updateShareOn applies share.update v on the actor of owner, the share's publishing connection, and returns the new
// ShareParams. For the connection's own share that is this actor. For a share that another connection of the user
// publishes, this actor hands the update over and waits for the result, so the connection's messages stay in order
// and the request gets exactly one reply. It does not wait forever: when the owner's actor has ended or the hub
// shuts down (share_not_found: the share ends with its connection), or when the owner does not get to the update
// within depTimeout (internal: it is busy, or it waits for this actor in turn, two connections updating each
// other's shares at once), the update is given up, and the owner drops it if it gets there later.
func (c *conn) updateShareOn(owner *conn, r *room, seq uint64, v protocol.ShareUpdate) (protocol.ShareParams, error) {
	if owner == c {
		return c.updateShare(r, v)
	}
	if owner == nil {
		return protocol.ShareParams{}, errShareGone()
	}
	type result struct {
		params protocol.ShareParams
		err    error
	}
	res := make(chan result, 1)
	var gaveUp atomic.Bool
	if !owner.post(func() {
		switch {
		case gaveUp.Load():
		case !owner.currentPeer(seq):
			res <- result{err: errShareGone()}
		default:
			p, err := owner.updateShare(r, v)
			res <- result{params: p, err: err}
		}
	}) {
		return protocol.ShareParams{}, errShareGone() // the owner is closing: its shares end with it
	}
	t := time.NewTimer(depTimeout)
	defer t.Stop()
	select {
	case x := <-res:
		return x.params, x.err
	case <-owner.done:
		select {
		case x := <-res: // it ran the update before it ended
			return x.params, x.err
		default:
			return protocol.ShareParams{}, errShareGone()
		}
	case <-c.h.down: // Shutdown has begun: it must not wait for this
		gaveUp.Store(true)
		return protocol.ShareParams{}, errShareGone()
	case <-t.C:
		gaveUp.Store(true)
		return protocol.ShareParams{}, errOwnerBusy
	}
}

// updateShare applies share.update v to a share that this connection publishes in room r: the MediaPeer returns the
// new ShareParams, then the label and the preset change in room.state. It runs on the connection's actor, with r its
// room.
func (c *conn) updateShare(r *room, v protocol.ShareUpdate) (protocol.ShareParams, error) {
	r.mu.Lock()
	s := r.shares[v.ShareID]
	mine := s != nil && s.info.ConnectionID == c.id
	r.mu.Unlock()
	if !mine {
		return protocol.ShareParams{}, errShareGone()
	}
	params, err := c.peer.UpdateShare(v.ShareID, v)
	if err != nil {
		return protocol.ShareParams{}, err
	}
	params.ShareID = v.ShareID
	var o outbox
	r.mu.Lock()
	if s := r.shares[v.ShareID]; s != nil {
		changed := false
		if v.Label != nil && s.info.Label != *v.Label {
			s.info.Label = *v.Label
			changed = true
		}
		if v.Preset != "" && s.info.Preset != v.Preset {
			s.info.Preset = v.Preset
			changed = true
		}
		s.params = params
		if changed {
			r.changedLocked(&o)
		}
	}
	r.mu.Unlock()
	o.send()
	return params, nil
}

// onShareStop handles share.stop (01 §8.7). It is idempotent: an unknown or already ended share replies ok. Stopping
// another user's share is forbidden; any connection of the share's user may stop it. The share ends with stopped,
// also at the MediaPeer of its publishing connection.
func (c *conn) onShareStop(env protocol.Envelope) {
	v, ok := decode[protocol.ShareStop](c, env)
	if !ok || !c.requireRoom(env) {
		return
	}
	r := c.room
	const reason = protocol.EndReasonStopped
	var owner *conn
	var o outbox
	r.mu.Lock()
	s := r.shares[v.ShareID]
	if s != nil && s.info.UserID != c.userID {
		r.mu.Unlock()
		c.reject(env, protocol.NewError(protocol.ErrorCodeForbidden, protocol.ErrorScopeRequest))
		return
	}
	if s != nil {
		owner = r.connLocked(s.info.UserID, s.info.ConnectionID)
		r.endShareLocked(&o, s, reason, time.Now())
		r.changedLocked(&o)
	}
	r.mu.Unlock()
	o.send()
	if s != nil {
		c.h.logShareEnded(r, s, reason)
		c.endAtOwner(owner, s, reason)
	}
	c.reply(env, nil)
}

// endAtOwner ends share s, which has left its room, at the MediaPeer of owner, its publishing connection: on this
// actor for the connection's own share, else on the owner's. The owner drops the call when it has left the room
// since; its MediaPeer has closed then, which ends the peer's shares.
func (c *conn) endAtOwner(owner *conn, s *share, reason protocol.EndReason) {
	id, seq := s.info.ID, s.peerSeq
	switch owner {
	case nil:
	case c:
		c.peer.EndShare(id, reason)
		c.armShareTimer()
	default:
		owner.post(func() {
			if owner.currentPeer(seq) {
				owner.peer.EndShare(id, reason)
				owner.armShareTimer()
			}
		})
	}
}

// endOwn ends, with reason, the shares that this connection publishes in its room and that pick selects: in the
// room, then at the connection's MediaPeer. It returns how many it ended.
func (c *conn) endOwn(reason protocol.EndReason, pick func(*share) bool) int {
	ended := c.room.endShares(reason, func(s *share) bool { return s.info.ConnectionID == c.id && pick(s) })
	for _, s := range ended {
		c.peer.EndShare(s.info.ID, reason)
	}
	if len(ended) > 0 {
		c.armShareTimer()
	}
	return len(ended)
}

// armShareTimer sets the connection's share timer to the earliest deadline of the shares it publishes, or stops it
// when none of them has one. It runs after every change of those shares or their deadlines.
func (c *conn) armShareTimer() {
	r := c.room
	if r == nil {
		c.shareTimer.Stop()
		return
	}
	var next time.Time
	r.mu.Lock()
	for _, s := range r.shares {
		if s.info.ConnectionID == c.id && !s.deadline.IsZero() && (next.IsZero() || s.deadline.Before(next)) {
			next = s.deadline
		}
	}
	r.mu.Unlock()
	if next.IsZero() {
		c.shareTimer.Stop()
		return
	}
	c.shareTimer.Reset(time.Until(next))
}

// shareTimeouts ends the connection's shares whose deadline has passed with media_timeout (01 §4.4): no first
// keyframe within ShareStartTimeout of share.start, or stalled for StalledTimeout. The timers run on while the
// connection is detached: a share's media does not depend on the WebSocket.
func (c *conn) shareTimeouts() {
	if c.room == nil {
		return
	}
	now := time.Now()
	c.endOwn(protocol.EndReasonMediaTimeout, func(s *share) bool {
		return !s.deadline.IsZero() && !now.Before(s.deadline)
	})
	c.armShareTimer()
}

// onShareMedia handles a media fact that the connection's MediaPeer reports about share id (01 §4.4):
//
//   - live: the first keyframe arrived. A starting share goes live, room.event share.started goes out (once per
//     share) and, unless the share replaces another one, Web Push is told (once per share). A stalled share is live
//     again, without an event;
//   - stalled: the publisher's pub PC is not connected. A live share is stalled, and ends with media_timeout unless
//     it is live again within StalledTimeout. A starting share stays starting: its start timeout covers it;
//   - changed: the layers, the codec or the audio changed;
//   - gone (reserved, 01 §15.2): the share's tracks are gone for good; it ends with stopped.
//
// Each event also carries what the SFU receives now, which room.state shows once the share has been live. Events
// about a share that has ended, or that another connection publishes, are dropped.
func (c *conn) onShareMedia(id string, ev ShareMediaEvent) {
	r := c.room
	now := time.Now()
	var o outbox
	var push *PushShareStarted
	changed, known := false, false
	r.mu.Lock()
	if s := r.shares[id]; s != nil && s.info.ConnectionID == c.id {
		known = true
		prev := s.info.Status
		switch ev.Kind {
		case ShareMediaLive:
			if prev != protocol.ShareStatusLive {
				r.setStatusLocked(s, protocol.ShareStatusLive)
				s.deadline = time.Time{}
				changed = true
			}
			changed = s.applyMedia(ev) || changed
			if !s.announced {
				s.announced = true
				push = r.announceLocked(&o, s, now)
			}
		case ShareMediaStalled:
			if prev == protocol.ShareStatusLive {
				r.setStatusLocked(s, protocol.ShareStatusStalled)
				s.deadline = now.Add(c.h.cfg.StalledTimeout)
				changed = true
			}
			if prev != protocol.ShareStatusStarting {
				changed = s.applyMedia(ev) || changed
			}
		case ShareMediaChanged:
			if prev != protocol.ShareStatusStarting {
				changed = s.applyMedia(ev)
			}
		}
		if changed {
			r.changedLocked(&o)
		}
	}
	r.mu.Unlock()
	o.send()
	if !known {
		c.h.log.Debug("share media event dropped", slog.String("conn_id", c.id), slog.String("share_id", id),
			slog.String("kind", ev.Kind.String()))
		return
	}
	if ev.Kind == ShareMediaGone {
		c.endOwn(protocol.EndReasonStopped, func(s *share) bool { return s.info.ID == id })
		return
	}
	if changed {
		c.h.log.Debug("share media", slog.String("conn_id", c.id), slog.String("share_id", id),
			slog.String("kind", ev.Kind.String()))
	}
	c.armShareTimer()
	if push != nil {
		c.pushShareStarted(*push)
	}
}

// announceLocked sends room.event share.started for share s, which just went live for the first time, to everyone in
// the room, the user's own connections included (01 §8.6). It returns the Web Push event for it, without the room's
// name, or nil for a share that replaces another one: a re-publish after a restart is no news (01 §10.6).
func (r *room) announceLocked(o *outbox, s *share, now time.Time) *PushShareStarted {
	name := ""
	if p := r.participants[s.info.UserID]; p != nil {
		name = p.name
	}
	r.eventLocked(o, protocol.RoomEvent{
		Kind:     protocol.RoomEventKindShareStarted,
		RoomID:   r.id,
		UserID:   s.info.UserID,
		Name:     name,
		ShareID:  s.info.ID,
		Replaces: s.info.Replaces,
		At:       now,
	}, nil)
	if s.info.Replaces != "" {
		return nil
	}
	ev := &PushShareStarted{RoomID: r.id, ShareID: s.info.ID, UserID: s.info.UserID, UserName: name, At: now}
	for _, p := range r.sortedParticipantsLocked() {
		ev.PresentUserIDs = append(ev.PresentUserIDs, p.userID)
	}
	return ev
}

// pushShareStarted tells Web Push that a share went live (01 §8.6, 04 §14). The room's name is read with GetRoom
// now: the hub never caches room names, so a rename needs no hook. That read is a store call, so it runs on its own
// goroutine and never holds up the connection's messages. Without the name (the room is gone, or the store fails)
// there is no notification.
func (c *conn) pushShareStarted(ev PushShareStarted) {
	h := c.h
	if h.deps.Push == nil {
		return
	}
	h.wg.Add(1) // the actor is counted, so the WaitGroup is above zero here
	go func() {
		defer h.wg.Done()
		ctx, cancel := context.WithTimeout(h.ctx, depTimeout)
		info, err := h.deps.Rooms.GetRoom(ctx, ev.RoomID)
		cancel()
		if err != nil {
			h.log.Warn("share started: no push, the room's name can't be read", slog.String("room_id", ev.RoomID),
				slog.String("share_id", ev.ShareID), slog.Any("err", err))
			return
		}
		ev.RoomName = info.Name
		h.deps.Push.ShareStarted(h.ctx, ev)
	}()
}

// shareError sends an error with scope share that the connection's MediaPeer reported, and acts on it:
// codec_not_supported means the publisher offered no usable H.264 for the share, which then ends with stopped
// (01 §9 rule 7). It is the one case where the sharer gets an error besides share.stopped (01 §8.6), and the error
// goes out first, so the client knows why before it sees the share end.
func (c *conn) shareError(e protocol.Error) {
	c.sendError(e, "")
	if e.Code == protocol.ErrorCodeCodecNotSupported && e.ShareID != "" {
		c.endOwn(protocol.EndReasonStopped, func(s *share) bool { return s.info.ID == e.ShareID })
	}
}

// onSubscribeUpdate handles subscribe.update (01 §8.9): desired state per share, merged into the connection's
// wants. Items for shares that are not in the room (any more) are ignored and listed in the reply, never an error.
// The others become the connection's desired subscriptions, from which room.state computes the watchers, and go to
// the MediaPeer in one call, so the SFU applies them in one step (audio follows focus without an overlap). An item
// that the MediaPeer refuses for another reason than an unknown share fails the request with its error; the wants
// stay as the client stated them, so watchers may over-count but never under-count (01 §4.1).
func (c *conn) onSubscribeUpdate(env protocol.Envelope) {
	v, ok := decode[protocol.SubscribeUpdate](c, env)
	if !ok || !c.requireRoom(env) {
		return
	}
	r := c.room
	unknown := make(map[string]bool)
	wants := make([]protocol.SubscriptionWant, 0, len(v.Subs))
	var o outbox
	r.mu.Lock()
	m := r.memberLocked(c)
	changed := false
	for _, w := range v.Subs {
		if _, ok := r.shares[w.ShareID]; !ok || m == nil {
			unknown[w.ShareID] = true
			continue
		}
		wants = append(wants, w)
		if m.subs[w.ShareID] != w {
			m.subs[w.ShareID] = w
			changed = true
		}
	}
	if changed {
		r.changedLocked(&o)
	}
	r.mu.Unlock()
	o.send()
	if len(wants) > 0 {
		ignored, err := c.peer.Subscribe(wants)
		if err != nil {
			c.rejectMedia(env, "subscribe", err)
			return
		}
		for _, id := range ignored {
			unknown[id] = true
		}
		c.armStats()
	}
	res := protocol.SubscribeResult{Ignored: []string{}}
	for _, w := range v.Subs { // in the request's order
		if unknown[w.ShareID] {
			res.Ignored = append(res.Ignored, w.ShareID)
		}
	}
	c.reply(env, res)
}
