package signal

import (
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

// handlers handle the client→server messages other than hello and pc.* (handlePC), after dispatch's checks.
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

// inRoom reports whether the connection is in a room. The actor is the only writer of roomID, so it reads it
// without the hub lock.
func (c *conn) inRoom() bool { return c.roomID != "" }

// requireRoom answers not_in_room outside a room (01 §12.1); inside one, the handling comes with README S19/S40.
func (c *conn) requireRoom(env protocol.Envelope) {
	if !c.inRoom() {
		c.reject(env, protocol.NewError(protocol.ErrorCodeNotInRoom, protocol.ErrorScopeRequest))
		return
	}
	c.notImplemented(env)
}

// onPing answers ping with pong, echoing t (01 §8.3).
func (c *conn) onPing(env protocol.Envelope) {
	p, ok := decode[protocol.Ping](c, env)
	if !ok {
		return
	}
	c.send(protocol.MessageTypePong, "", protocol.Pong{T: p.T, ServerTimeMs: time.Now().UnixMilli()})
}

// onRoomJoin handles room.join (01 §8.4). A malformed room id gets room_not_found; joining comes with README S19.
func (c *conn) onRoomJoin(env protocol.Envelope) {
	if _, ok := decode[protocol.RoomJoin](c, env); !ok {
		return
	}
	c.notImplemented(env)
}

// onRoomLeave handles room.leave: it replies ok, also when the connection is in no room (01 §8.4). Leaving a room
// comes with README S19.
func (c *conn) onRoomLeave(env protocol.Envelope) {
	if _, ok := decode[protocol.Empty](c, env); !ok {
		return
	}
	if c.inRoom() {
		c.notImplemented(env)
		return
	}
	c.reply(env, nil)
}

// onShareStart handles share.start (01 §8.7; README S40).
func (c *conn) onShareStart(env protocol.Envelope) {
	if _, ok := decode[protocol.ShareStart](c, env); ok {
		c.requireRoom(env)
	}
}

// onShareUpdate handles share.update (01 §8.7; README S40).
func (c *conn) onShareUpdate(env protocol.Envelope) {
	if _, ok := decode[protocol.ShareUpdate](c, env); ok {
		c.requireRoom(env)
	}
}

// onShareStop handles share.stop (01 §8.7; README S40).
func (c *conn) onShareStop(env protocol.Envelope) {
	if _, ok := decode[protocol.ShareStop](c, env); ok {
		c.requireRoom(env)
	}
}

// onSubscribeUpdate handles subscribe.update (01 §8.9; README S40).
func (c *conn) onSubscribeUpdate(env protocol.Envelope) {
	if _, ok := decode[protocol.SubscribeUpdate](c, env); ok {
		c.requireRoom(env)
	}
}

// onCapsUpdate records the client's new capabilities (01 §8.10). README S40 passes them to the MediaPeer.
func (c *conn) onCapsUpdate(env protocol.Envelope) {
	v, ok := decode[protocol.CapsUpdate](c, env)
	if !ok {
		return
	}
	c.caps = v.Caps
}

// onStats keeps the client's last stats report for the live snapshot (01 §8.11). README S40 adds the client
// metrics from its counters.
func (c *conn) onStats(env protocol.Envelope) {
	v, ok := decode[protocol.ClientStats](c, env)
	if !ok {
		return
	}
	c.lastStats.Store(&v)
}

// onStatsWatch records whether the client watches the server's stats and replies ok (01 §8.11). README S40 sends
// the ServerStats every 2 s while watched.
func (c *conn) onStatsWatch(env protocol.Envelope) {
	v, ok := decode[protocol.StatsWatch](c, env)
	if !ok {
		return
	}
	c.statsWatch = v.On
	c.reply(env, nil)
}

// onAgentSend handles agent.send (01 §8.14), which the same-user relay (README S51) brings together with the
// agent.relay feature. Until then the feature is off and dispatch answers feature_disabled.
func (c *conn) onAgentSend(env protocol.Envelope) {
	if _, ok := decode[protocol.AgentSend](c, env); ok {
		c.notImplemented(env)
	}
}

// handlePC handles the pc.* notifications (01 §8.8): validation, the PC-kind role check (01 §6.3), the pc.restart
// limit (01 §13), then the room. Routing to the MediaPeer comes with README S40. Every error has scope pc.
func (c *conn) handlePC(env protocol.Envelope, now time.Time) {
	var pc protocol.PCKind
	var err error
	switch env.Type {
	case protocol.MessageTypePCOffer:
		var v protocol.PCOffer
		v, err = protocol.Decode[protocol.PCOffer](env)
		pc = v.PC
	case protocol.MessageTypePCAnswer:
		var v protocol.PCAnswer
		v, err = protocol.Decode[protocol.PCAnswer](env)
		pc = v.PC
	case protocol.MessageTypePCICE:
		var v protocol.PCICE
		v, err = protocol.Decode[protocol.PCICE](env)
		pc = v.PC
	case protocol.MessageTypePCRestart:
		var v protocol.PCRestart
		v, err = protocol.Decode[protocol.PCRestart](env)
		pc = v.PC
	case protocol.MessageTypePCClose:
		var v protocol.PCClose
		v, err = protocol.Decode[protocol.PCClose](env)
		pc = v.PC
	}
	if err != nil {
		c.rejectDecode(env, err)
		return
	}
	if !protocol.Allowed(c.role, env.Type, pc) {
		c.reject(env, protocol.NewError(protocol.ErrorCodeForbidden, protocol.ErrorScopePC))
		return
	}
	switch {
	case env.Type == protocol.MessageTypePCRestart:
		if ok, wait := c.limits.typed(now, string(env.Type)+"/"+string(pc), pcRestartRate); !ok {
			c.reject(env, rateLimited(wait))
			return
		}
	case env.Type == protocol.MessageTypePCClose && pc == protocol.PCKindSub:
		return // M1 clients send pc.close only for pub; the hub ignores sub (01 §8.8)
	}
	c.requireRoom(env)
}
