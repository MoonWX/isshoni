package signal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// The same-user relay (01 §8.14, feature agent.relay).
//
// A user's connections reach each other through the hub: the web page drives the Linux agent (M4) and hands
// playback off to the desktop app (M2). Protocol v1 defines the transport only. A connection sends
// agent.send{to | toRole, kind, payload}. Every target gets agent.recv{from, kind, payload}, where from is the
// sender's connection id, which a target answers to with an agent.send{to} of its own. The sender gets
// ok{delivered}, the number of targets.
//
// The hub never looks into the payload: it goes out as the JSON value that came in, in no more bytes than came in
// (encodeAgentRecv). The one value it takes no message for is null, which is no payload at all: such an agent.send is
// refused like one without a payload and reaches nobody, so no agent.recv ever has a null for its payload (01 §5).
// The kinds are a registry of the M2 and M4 docs; the hub checks their form only.
//
// Targets are connections of the sender's own user and of nobody else, whatever their session or device:
//
//   - to names one connection by its id. An id that is unknown, or another user's, is answered exactly like a
//     connection of the user that can't be reached, with agent_target_not_found: the relay never confirms that
//     another user's connection id exists;
//   - toRole names every connection of the user with that role;
//   - the sender is never its own target, neither by its role nor by its id;
//   - a target has the agent.relay feature active (01 §6.2: a client that did not ask for it does not know
//     agent.recv);
//   - a target is online. A detached connection (01 §4.2) is none: what the hub sends it is dropped and never
//     replayed (01 §10.5), so the sender had better hear that nobody got the message.
//
// Without a target the answer is agent_target_not_found, so an ok always has delivered >= 1.
//
// delivered counts the connections whose actor was handed the message while their socket was open. It is not an
// acknowledgment: a socket can still fail before the message is written, or the connection can detach before its
// actor gets to it. Kinds that need an answer define one (share.request and share.status, M4).
//
// A target whose actor's inbox is full is not handed the message and is not counted. It is closed for good as
// slow_connection, without grace (conn.post, 01 §15.2), like any connection that cannot keep up with what the hub
// has for it: its shares end, and no resume brings it back.
//
// What comes before the handler (dispatch.go): the frame is at most 64 KiB, the connection has the agent.relay
// feature (else feature_disabled), and its per-type bucket has a token (typeRates: 10 per second per connection,
// else rate_limited with retryAfterMs). Every role may send (01 §6.3). The bucket is charged whatever becomes of the
// message, so a connection gets ten answers per second about connection ids, too. Then the payload is validated
// (protocol.AgentSend.Validate): exactly one of to and toRole, the form of kind, and a payload that is there (not
// missing, not null) of at most 16 KiB (message_too_large above it, bad_request for the rest). An agent.send that
// fails here never gets as far as its targets.

// onAgentSend handles agent.send (01 §8.14): it relays the message as agent.recv to its targets and replies
// ok{delivered}, or agent_target_not_found when there is none.
//
// The message is encoded once and posted to each target's actor, which queues it on its socket in order with the
// connection's other messages. The sender's actor never waits for a target's: a target that is busy, or stuck, costs
// the sender nothing, and two connections may relay to each other at once.
func (c *conn) onAgentSend(env protocol.Envelope) {
	v, ok := decode[protocol.AgentSend](c, env)
	if !ok {
		return
	}
	delivered := 0
	if targets := c.h.relayTargets(c, v.To, v.ToRole); len(targets) > 0 {
		msg, err := encodeAgentRecv(c.id, v.Kind, v.Payload)
		if err != nil {
			c.reject(env, c.h.internalError(fmt.Errorf("encode agent.recv: %w", err), protocol.ErrorScopeRequest,
				slog.String("conn_id", c.id), slog.String("type", string(env.Type))))
			return
		}
		for _, t := range targets {
			if t.post(func() { t.sendEncoded(protocol.MessageTypeAgentRecv, msg) }) {
				delivered++
			}
		}
	}
	// The kind is one of a few fixed names ([a-z.]{1,32}); the payload is never logged (01 §17).
	c.h.log.Debug("relay", slog.String("conn_id", c.id), slog.String("user_id", c.userID),
		slog.String("kind", v.Kind), slog.Int("delivered", delivered))
	if delivered == 0 {
		c.reject(env, protocol.NewError(protocol.ErrorCodeAgentTargetNotFound, protocol.ErrorScopeRequest))
		return
	}
	c.reply(env, protocol.AgentSendResult{Delivered: delivered})
}

// encodeAgentRecv encodes the agent.recv that takes a relayed message to its targets: from is the sender's
// connection id, kind and payload are the agent.send's.
//
// The payload goes out as it came in, but for the white space between its tokens, which is dropped (json.Compact).
// So the message is never more than the payload plus an envelope of about a hundred bytes: what a sender is charged
// for (16 KiB of payload, 10 per second) is the most that each target is sent, and agent.recv stays far below the
// 64 KiB of a message (protocol.MaxMessageBytes).
//
// That is why the payload does not go through protocol.Marshal, as the rest of the message does. encoding/json
// escapes what it marshals for JSON inside a web page, which a WebSocket message is not: every <, >, &, U+2028 and
// U+2029 of the payload would go out as a \u escape of six bytes, up to six times what came in. For a payload
// without those characters the message is the very one that Marshal makes (TestRelayEncodeMatchesProtocol).
//
// A payload that passed protocol.ParseEnvelope and AgentSend.Validate always encodes, and the result is as deeply
// nested as the agent.send was, so it passes the receiving side's ParseEnvelope too (FuzzRelayEncode).
func encodeAgentRecv(from, kind string, payload json.RawMessage) ([]byte, error) {
	// The message as the protocol encodes it with a null for its payload, which is the last thing in it. The payload
	// takes the null's place.
	const placeholder, tail = "null", "}}"
	msg, err := protocol.Marshal(protocol.MessageTypeAgentRecv, "", "",
		protocol.AgentRecv{From: from, Kind: kind, Payload: json.RawMessage(placeholder)})
	if err != nil {
		return nil, err
	}
	head, ok := bytes.CutSuffix(msg, []byte(placeholder+tail))
	if !ok {
		return nil, fmt.Errorf("agent.recv does not end with its payload: %s", msg)
	}
	var b bytes.Buffer
	b.Grow(len(head) + len(payload) + len(tail))
	b.Write(head)
	if err := json.Compact(&b, payload); err != nil {
		// Without the error's text, which quotes a character of the payload: the payload is never logged (01 §17).
		return nil, errors.New("the payload is not JSON")
	}
	b.WriteString(tail)
	return b.Bytes(), nil
}

// relayTargets returns the connections that an agent.send of connection from reaches right now (see the top of this
// file): the one that to names, or, without to, every one with the role. Any goroutine may call it.
//
// A send by role walks all of the hub's connections under the hub lock, as UpdateUser and CloseConnections do: the
// hub keeps no index by user, and a send costs its connection a token of its 10 per second.
func (h *Hub) relayTargets(from *conn, to string, role protocol.Role) []*conn {
	var targets []*conn
	h.mu.Lock()
	if to != "" {
		if t := h.conns[to]; t != nil && from.relaysTo(t) {
			targets = []*conn{t}
		}
	} else {
		for _, t := range h.conns {
			if t.role == role && from.relaysTo(t) {
				targets = append(targets, t)
			}
		}
	}
	h.mu.Unlock()
	return slices.DeleteFunc(targets, func(t *conn) bool { return !t.relayOnline() })
}

// relaysTo reports whether the relay may take a message of connection c to connection t: t is another connection
// of the same user, and it has the agent.relay feature. A connection's user, role and features never change, so any
// goroutine may call it.
func (c *conn) relaysTo(t *conn) bool {
	return t != c && t.userID == c.userID && slices.Contains(t.features, protocol.FeatureAgentRelay)
}

// relayOnline reports whether the connection has a socket that still takes messages: it is not detached, and the hub
// is not closing its socket. Any goroutine may call it; the answer can be out of date as soon as it is given.
func (c *conn) relayOnline() bool {
	s := c.sockP.Load()
	return s != nil && !s.q.closing()
}
