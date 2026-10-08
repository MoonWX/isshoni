package signal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// handleFrame handles one text frame of the connection's socket s: it parses the envelope, charges the global rate
// limit (01 §13) and dispatches the message. It runs on the actor.
func (c *conn) handleFrame(s *socket, b []byte) {
	if s != c.sock || s.q.closing() {
		return // an earlier socket's frame, or the socket is closing
	}
	now := time.Now()
	env, err := protocol.ParseEnvelope(b)
	if err != nil {
		c.fail(protocol.NewError(protocol.ErrorCodeBadMessage, protocol.ErrorScopeConnection))
		return
	}
	c.h.metrics.message(env.Type, dirIn)
	c.h.log.Debug("receive", slog.String("conn_id", c.id), slog.String("type", string(env.Type)))
	if ok, wait, flood := c.limits.global(now, len(b)); !ok {
		if flood {
			e := protocol.NewError(protocol.ErrorCodeRateLimited, protocol.ErrorScopeConnection)
			e.RetryAfterMs = retryAfterMs(c.limits.refillTime())
			c.h.log.Info("flooding connection closed", slog.String("conn_id", c.id), slog.String("user_id", c.userID))
			c.fail(e)
			return
		}
		c.reject(env, rateLimited(wait))
		return
	}
	c.dispatch(env, len(b), now)
}

// dispatch checks a message against the registry, its size limit, the connection's role and features and the
// per-type rate limits, then runs its handler.
func (c *conn) dispatch(env protocol.Envelope, size int, now time.Time) {
	if size > protocol.MaxMessageSize(env.Type) {
		c.tooLarge(env)
		return
	}
	spec, known := protocol.Lookup(env.Type, protocol.DirClientToServer)
	switch {
	case !known: // unknown_type if it had an id, else ignored (01 §8.13)
		if env.ID != "" {
			c.sendError(protocol.NewError(protocol.ErrorCodeUnknownType, protocol.ErrorScopeRequest), env.ID)
		}
		return
	case env.Type == protocol.MessageTypeHello: // a second hello (01 §8.2 step 1)
		if c.sock != nil {
			c.sock.fail(protocol.NewError(protocol.ErrorCodeBadRequest, protocol.ErrorScopeConnection), env.ID)
		}
		return
	case spec.Kind == protocol.KindRequest && env.ID == "": // a request without an id can't be answered
		c.fail(protocol.NewError(protocol.ErrorCodeBadMessage, protocol.ErrorScopeConnection))
		return
	case spec.Feature != "" && !slices.Contains(c.features, spec.Feature):
		c.reject(env, protocol.NewError(protocol.ErrorCodeFeatureDisabled, protocol.ErrorScopeRequest))
		return
	case isPC(env.Type):
		c.handlePC(env, now)
		return
	case !spec.Allows(c.role):
		c.reject(env, protocol.NewError(protocol.ErrorCodeForbidden, protocol.ErrorScopeRequest))
		return
	}
	if r, ok := typeRates[env.Type]; ok {
		if ok, wait := c.limits.typed(now, string(env.Type), r); !ok {
			if env.Type == protocol.MessageTypeStats {
				return // at most one per 5 s; the others are dropped silently (01 §8.11)
			}
			c.reject(env, rateLimited(wait))
			return
		}
	}
	handle, ok := handlers[env.Type]
	if !ok {
		c.notImplemented(env)
		return
	}
	handle(c, env)
}

// handlers handle the client→server messages other than hello and pc.* (handlePC), after dispatch's checks. Those of
// shares and subscriptions are in share.go, the relay's (agent.send) is in relay.go.
var handlers = map[protocol.MessageType]func(*conn, protocol.Envelope){
	protocol.MessageTypePing:            (*conn).onPing,
	protocol.MessageTypeRoomJoin:        (*conn).onRoomJoin,
	protocol.MessageTypeRoomLeave:       (*conn).onRoomLeave,
	protocol.MessageTypeShareStart:      (*conn).onShareStart,
	protocol.MessageTypeShareUpdate:     (*conn).onShareUpdate,
	protocol.MessageTypeShareStop:       (*conn).onShareStop,
	protocol.MessageTypeSubscribeUpdate: (*conn).onSubscribeUpdate,
	protocol.MessageTypeCapsUpdate:      (*conn).onCapsUpdate,
	protocol.MessageTypeStats:           (*conn).onStats,
	protocol.MessageTypeStatsWatch:      (*conn).onStatsWatch,
	protocol.MessageTypeAgentSend:       (*conn).onAgentSend,
}

func isPC(t protocol.MessageType) bool {
	switch t {
	case protocol.MessageTypePCOffer, protocol.MessageTypePCAnswer, protocol.MessageTypePCICE,
		protocol.MessageTypePCRestart, protocol.MessageTypePCClose:
		return true
	}
	return false
}

func rateLimited(wait time.Duration) protocol.Error {
	e := protocol.NewError(protocol.ErrorCodeRateLimited, protocol.ErrorScopeRequest)
	e.RetryAfterMs = retryAfterMs(wait)
	return e
}

// reject answers a message with error e: with scope pc (and the message's pc, gen and neg) for pc.* notifications,
// as the reply (scope request) to a message with an id. Errors about other notifications are not sent: scope request
// is only for messages with an id (01 §12.1).
func (c *conn) reject(env protocol.Envelope, e protocol.Error) {
	switch {
	case isPC(env.Type):
		e.Scope = protocol.ErrorScopePC
		e.PC, e.Gen, e.Neg = pcFields(env)
		c.sendError(e, "")
	case env.ID != "":
		e.Scope = protocol.ErrorScopeRequest
		c.sendError(e, env.ID)
	default:
		c.h.log.Debug("notification dropped", slog.String("conn_id", c.id), slog.String("type", string(env.Type)),
			slog.String("code", string(e.Code)))
	}
}

// tooLarge answers a message over its size limit with message_too_large and drops it (01 §3.3). Unlike other errors
// about notifications, this one is sent for a notification too (without re), so the client learns that its message,
// a stats report for example, was dropped.
func (c *conn) tooLarge(env protocol.Envelope) {
	e := protocol.NewError(protocol.ErrorCodeMessageTooLarge, protocol.ErrorScopeRequest)
	if isPC(env.Type) || env.ID != "" {
		c.reject(env, e)
		return
	}
	c.sendError(e, "")
}

// pcFields reads the pc, gen and neg fields of a pc.* payload for its errors (01 §12.1), ignoring everything else. A
// pc value that is not a known kind is left out.
func pcFields(env protocol.Envelope) (protocol.PCKind, uint32, uint32) {
	var f struct {
		PC  protocol.PCKind `json:"pc"`
		Gen uint32          `json:"gen"`
		Neg uint32          `json:"neg"`
	}
	_ = json.Unmarshal(env.Data, &f) // best effort: the fields only annotate the error
	if !f.PC.Valid() {
		f.PC = ""
	}
	return f.PC, f.Gen, f.Neg
}

// decode unmarshals and validates env's payload. On failure it rejects the message, with bad_request and params
// {field, reason}, or with the *protocol.Error that validation chose (room_not_found, message_too_large).
func decode[T any](c *conn, env protocol.Envelope) (T, bool) {
	v, err := protocol.Decode[T](env)
	if err != nil {
		c.rejectDecode(env, err)
		return v, false
	}
	return v, true
}

func (c *conn) rejectDecode(env protocol.Envelope, err error) {
	var pe *protocol.Error
	var fe *protocol.FieldError
	switch {
	case errors.As(err, &pe):
		c.reject(env, *pe)
	case errors.As(err, &fe):
		c.reject(env, fe.BadRequest(protocol.ErrorScopeRequest))
	default:
		c.reject(env, c.h.internalError(err, protocol.ErrorScopeRequest,
			slog.String("conn_id", c.id), slog.String("type", string(env.Type))))
	}
}

// reply sends ok for request env; data nil sends no data.
func (c *conn) reply(env protocol.Envelope, data any) {
	c.send(protocol.MessageTypeOK, env.ID, data)
}

// notImplemented answers a message whose handling a later slice brings (README §5) with error{internal} and logs
// which one it was.
func (c *conn) notImplemented(env protocol.Envelope) {
	c.reject(env, c.h.internalError(fmt.Errorf("%s: %w", env.Type, errNotImplemented), protocol.ErrorScopeRequest,
		slog.String("conn_id", c.id), slog.String("type", string(env.Type))))
}

// requireRoom reports whether the connection is in a room. Outside one it answers a share, subscription or pc.*
// message with not_in_room (01 §12.1).
func (c *conn) requireRoom(env protocol.Envelope) bool {
	if c.room != nil {
		return true
	}
	c.reject(env, protocol.NewError(protocol.ErrorCodeNotInRoom, protocol.ErrorScopeRequest))
	return false
}

// onPing answers ping with pong, echoing t (01 §8.3).
func (c *conn) onPing(env protocol.Envelope) {
	p, ok := decode[protocol.Ping](c, env)
	if !ok {
		return
	}
	c.send(protocol.MessageTypePong, "", protocol.Pong{T: p.T, ServerTimeMs: time.Now().UnixMilli()})
}

// onRoomJoin handles room.join (01 §8.4). The room must exist (GetRoom; a malformed id also gets room_not_found)
// and the user may join it (CanJoin, else forbidden). Joining the connection's own room again replies ok and resends
// room.state, and the room's name for Snapshot becomes the one GetRoom just read. Otherwise the connection attaches
// to the room's participant for its user (room_full when a new participant would pass Policy().MaxRoomParticipants;
// room_not_found when CloseRoom got there first), leaves its previous room (its shares there end with left) and gets
// a new MediaPeer. The ok carries the room and is followed at once by a room.state for this connection alone.
func (c *conn) onRoomJoin(env protocol.Envelope) {
	v, ok := decode[protocol.RoomJoin](c, env)
	if !ok {
		return
	}
	info, e := c.lookupRoom(v.RoomID)
	if e != nil {
		c.reject(env, *e)
		return
	}
	res := protocol.RoomJoinResult{Room: protocol.RoomInfo{ID: v.RoomID, Name: info.Name}}
	if r := c.room; r != nil && r.id == v.RoomID {
		r.setName(info.Name)
		c.reply(env, res)
		c.sendStateNow()
		return
	}
	r, e := c.h.attach(c, res.Room, c.h.policy().MaxRoomParticipants)
	if e != nil {
		c.reject(env, *e)
		return
	}
	c.leaveRoom(protocol.EndReasonLeft) // the previous room; the SFU sees one peer per connection at a time
	c.room = r
	c.peerSeq++
	peer, err := c.h.deps.Media.NewPeer(PeerParams{
		ConnectionID: c.id,
		UserID:       c.userID,
		RoomID:       r.id,
		Role:         c.role,
		Caps:         c.caps,
		Client:       c.client,
	}, &peerSink{c: c, seq: c.peerSeq})
	if err != nil {
		c.room = nil
		c.h.detach(c, r, protocol.EndReasonLeft) // no shares yet
		c.reject(env, c.h.internalError(fmt.Errorf("new media peer: %w", err), protocol.ErrorScopeRequest,
			slog.String("conn_id", c.id), slog.String("room_id", r.id)))
		return
	}
	c.peer = peer
	c.reply(env, res)
	c.sendStateNow()
	if c.statsWatch {
		c.armStats() // a client that watches the server's stats gets those of its new MediaPeer
	}
	c.h.log.Debug("joined room", slog.String("conn_id", c.id), slog.String("user_id", c.userID),
		slog.String("room_id", r.id))
}

// lookupRoom reads the room for a room.join from the RoomDirectory: room_not_found for an unknown room, forbidden
// when CanJoin refuses, internal (with a logged ref) when the directory fails. It runs on the actor, which handles
// the connection's messages in order; the call is bounded by depTimeout.
func (c *conn) lookupRoom(roomID string) (protocol.RoomInfo, *protocol.Error) {
	ctx, cancel := context.WithTimeout(c.h.ctx, depTimeout)
	defer cancel()
	info, err := c.h.deps.Rooms.GetRoom(ctx, roomID)
	switch {
	case errors.Is(err, ErrNotFound):
		e := protocol.NewError(protocol.ErrorCodeRoomNotFound, protocol.ErrorScopeRequest)
		return info, &e
	case err != nil:
		e := c.h.internalError(fmt.Errorf("get room: %w", err), protocol.ErrorScopeRequest,
			slog.String("conn_id", c.id), slog.String("room_id", roomID))
		return info, &e
	}
	if err := c.h.deps.Rooms.CanJoin(ctx, c.identity(), roomID); err != nil {
		c.h.log.Debug("room.join refused", slog.String("conn_id", c.id), slog.String("room_id", roomID),
			slog.Any("err", err))
		e := protocol.NewError(protocol.ErrorCodeForbidden, protocol.ErrorScopeRequest)
		return info, &e
	}
	return info, nil
}

// onRoomLeave handles room.leave (01 §8.4): the connection's shares end with left, its MediaPeer closes and it
// leaves its participant. It replies ok, also when the connection is in no room.
func (c *conn) onRoomLeave(env protocol.Envelope) {
	if _, ok := decode[protocol.Empty](c, env); !ok {
		return
	}
	c.leaveRoom(protocol.EndReasonLeft)
	c.reply(env, nil)
}

// onCapsUpdate handles caps.update (01 §8.10): the client's capabilities changed (a codec became available,
// 01 §11.7). The connection keeps them for its next MediaPeer, and its current one gets them at once: the SFU
// updates the room's codec safe set and retries the subscriptions that the codec blocked.
func (c *conn) onCapsUpdate(env protocol.Envelope) {
	v, ok := decode[protocol.CapsUpdate](c, env)
	if !ok {
		return
	}
	c.caps = v.Caps
	if c.peer != nil {
		c.peer.SetCaps(v.Caps)
	}
}

// onStats handles a client's stats report (01 §8.11): the hub keeps the last one for the live snapshot and counts
// the growth of its inbound counters in the isshoni_client_* metrics.
func (c *conn) onStats(env protocol.Envelope) {
	v, ok := decode[protocol.ClientStats](c, env)
	if !ok {
		return
	}
	c.lastStats.Store(&v)
	c.countClientStats(&v)
}

// onStatsWatch handles stats.watch (01 §8.11): while on, the connection gets the server's stats of its MediaPeer
// every statsInterval (statsTick), until {on: false} or the connection closes. It replies ok. Outside a room there
// is nothing to send; the stats start with the next room.join.
func (c *conn) onStatsWatch(env protocol.Envelope) {
	v, ok := decode[protocol.StatsWatch](c, env)
	if !ok {
		return
	}
	c.statsWatch = v.On
	c.reply(env, nil)
	if v.On {
		c.armStats()
	}
}

// handlePC handles the pc.* notifications (01 §8.8, §9) and routes them to the connection's MediaPeer. Each one
// passes pcCheck, the rate limits of 01 §13 and the room check (not_in_room) first. Every error has scope pc, with
// the message's pc, gen and neg.
//
// The hub checks the role and the ownership of what a message names; it keeps no PeerConnection state. Whether a
// gen or a neg is current is the SFU's to say (01 §9 rule 2): its answers come back through the MediaPeer.
func (c *conn) handlePC(env protocol.Envelope, now time.Time) {
	switch env.Type {
	case protocol.MessageTypePCOffer: // the client offers on pub only
		v, err := protocol.Decode[protocol.PCOffer](env)
		if c.pcCheck(env, err, v.PC, protocol.PCKindPub) && c.requireRoom(env) {
			c.onPCOffer(env, v, now)
		}
	case protocol.MessageTypePCAnswer: // and answers on sub only
		v, err := protocol.Decode[protocol.PCAnswer](env)
		if c.pcCheck(env, err, v.PC, protocol.PCKindSub) && c.requireRoom(env) {
			c.pcResult(env, "handle answer", c.peer.HandleAnswer(v))
		}
	case protocol.MessageTypePCICE:
		v, err := protocol.Decode[protocol.PCICE](env)
		if c.pcCheck(env, err, v.PC, "") && c.requireRoom(env) {
			c.pcResult(env, "add ice candidate", c.peer.AddICE(v))
		}
	case protocol.MessageTypePCRestart: // sub only: the client restarts its pub PC with an offer of its own
		v, err := protocol.Decode[protocol.PCRestart](env)
		if !c.pcCheck(env, err, v.PC, protocol.PCKindSub) {
			return
		}
		if ok, wait := c.limits.typed(now, string(env.Type)+"/"+string(v.PC), pcRestartRate); !ok {
			c.reject(env, rateLimited(wait))
			return
		}
		if c.requireRoom(env) {
			c.pcResult(env, "restart", c.peer.Restart(v))
		}
	case protocol.MessageTypePCClose:
		v, err := protocol.Decode[protocol.PCClose](env)
		if !c.pcCheck(env, err, v.PC, "") || v.PC == protocol.PCKindSub {
			return // M1 clients send pc.close only for pub; the hub ignores sub (01 §8.8)
		}
		if c.requireRoom(env) {
			c.onPCClose(env, v)
		}
	}
}

// pcCheck runs the checks that every pc.* notification passes before anything else: its payload is valid
// (bad_request with the field), the connection's role may use that PeerConnection (forbidden, 01 §6.3), and the
// message goes the way the protocol fixes for it (01 §9 rule 1). only is the one PC kind that a client may send the
// message for, "" for either: an offer, an answer or a restart request for the other kind is bad_request with
// params {field: "pc", reason: "invalid"}. err is the payload's decode error. It reports whether the message passed.
func (c *conn) pcCheck(env protocol.Envelope, err error, pc, only protocol.PCKind) bool {
	switch {
	case err != nil:
		c.rejectDecode(env, err)
	case !protocol.Allowed(c.role, env.Type, pc):
		c.reject(env, protocol.NewError(protocol.ErrorCodeForbidden, protocol.ErrorScopePC))
	case only != "" && pc != only:
		fe := protocol.FieldError{Field: "pc", Reason: protocol.FieldInvalid}
		c.reject(env, fe.BadRequest(protocol.ErrorScopePC))
	default:
		return true
	}
	return false
}

// pcResult answers a pc.* notification whose MediaPeer call failed. The adapter's *protocol.Error (01 §15.4) goes
// out with scope pc and the message's pc, gen and neg, whichever row its code comes from (01 §12.1). The exception
// is an error with scope share, codec_not_supported: it names a share, which then ends (shareError). Any other error
// is internal, with a logged ref.
func (c *conn) pcResult(env protocol.Envelope, call string, err error) {
	if err == nil {
		return
	}
	var pe *protocol.Error
	switch {
	case !errors.As(err, &pe):
		c.reject(env, c.h.internalError(fmt.Errorf("%s: %w", call, err), protocol.ErrorScopePC,
			slog.String("conn_id", c.id), slog.String("type", string(env.Type))))
	case pe.Scope == protocol.ErrorScopeShare:
		c.shareError(*pe)
	default:
		c.reject(env, *pe)
	}
}

// onPCOffer handles a pub offer (01 §8.8, §9 rules 4 and 7):
//
//   - an offer with a new gen asks for a new PeerConnection, which is limited to pubGenRate (rate_limited, scope pc);
//   - of its tracks, only those of shares that this connection publishes go on: a TrackRef of any other share (one
//     that ended in a race, or somebody else's) is dropped, and the server still answers;
//   - each share of this connection that an earlier pub offer bound and that this one no longer lists ends with
//     stopped, before the MediaPeer sees the offer (01 §8.7). A starting share that no offer has bound yet is left
//     alone: its start timeout covers it;
//   - the MediaPeer's answer goes back as pc.answer, and the shares that the offer lists are bound from now on. When
//     the MediaPeer refuses the offer, nothing is bound.
func (c *conn) onPCOffer(env protocol.Envelope, o protocol.PCOffer, now time.Time) {
	if o.Gen > c.pubGen {
		if ok, wait := c.limits.typed(now, pubGenKey, pubGenRate); !ok {
			c.reject(env, rateLimited(wait))
			return
		}
		c.pubGen = o.Gen
	}
	r := c.room
	listed := make(map[string]bool, len(o.Tracks))
	var tracks []protocol.TrackRef
	r.mu.Lock()
	for _, t := range o.Tracks {
		if s := r.shares[t.ShareID]; s != nil && s.info.ConnectionID == c.id {
			tracks = append(tracks, t)
			listed[t.ShareID] = true
		}
	}
	r.mu.Unlock()
	o.Tracks = tracks
	c.endOwn(protocol.EndReasonStopped, func(s *share) bool { return s.bound && !listed[s.info.ID] })
	answer, err := c.peer.HandleOffer(o)
	if err != nil {
		c.pcResult(env, "handle offer", err)
		return
	}
	r.mu.Lock()
	for id := range listed {
		if s := r.shares[id]; s != nil && s.info.ConnectionID == c.id {
			s.bound = true
		}
	}
	r.mu.Unlock()
	c.send(protocol.MessageTypePCAnswer, "", answer)
}

// onPCClose handles pc.close for the pub PC (01 §8.8, §9 rule 10): the client closed it on purpose. The shares it
// carried, those that a pub offer bound, end with stopped, then the MediaPeer closes its side. A starting share that
// no offer has bound yet was never on that PeerConnection and is left to its start timeout, as in onPCOffer.
func (c *conn) onPCClose(env protocol.Envelope, v protocol.PCClose) {
	c.endOwn(protocol.EndReasonStopped, func(s *share) bool { return s.bound })
	c.pcResult(env, "close pc", c.peer.ClosePC(v))
}
