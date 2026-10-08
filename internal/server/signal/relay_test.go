package signal_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/signal"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// The same-user relay (01 §8.14, slice P12): agent.send of one connection reaches other connections of the same
// user as agent.recv, and nobody else's. Its limits are 16 KiB of payload and 10 messages per second per connection.

// relayHello is a web client's hello with the role that asks for the agent.relay feature.
func relayHello(role protocol.Role) protocol.Hello {
	h := signaltest.DefaultHello()
	h.Role = role
	h.Features = []protocol.Feature{protocol.FeatureAgentRelay}
	return h
}

// relayConn connects with relayHello, checks that the welcome turns the feature on, and returns the client and its
// connection id.
func (e *env) relayConn(cookie string, role protocol.Role) (*signaltest.Client, string) {
	e.t.Helper()
	c, w := e.connect(cookie, relayHello(role))
	if !slices.Equal(w.Features, []protocol.Feature{protocol.FeatureAgentRelay}) {
		e.t.Fatalf("welcome features %v, want agent.relay", w.Features)
	}
	return c, w.ConnectionID
}

// toRole and toConn are agent.sends with a small payload.
func toRole(role protocol.Role, kind string) protocol.AgentSend {
	return protocol.AgentSend{ToRole: role, Kind: kind, Payload: json.RawMessage(`{"preset":"game"}`)}
}

func toConn(id, kind string) protocol.AgentSend {
	return protocol.AgentSend{To: id, Kind: kind, Payload: json.RawMessage(`{"preset":"game"}`)}
}

// relayed sends an agent.send, reads its ok and returns the delivered count.
func relayed(t *testing.T, c *signaltest.Client, v protocol.AgentSend) int {
	t.Helper()
	id := request(t, c, protocol.MessageTypeAgentSend, v)
	env := expectType(t, c, protocol.MessageTypeOK)
	res, err := protocol.Decode[protocol.AgentSendResult](env)
	if err != nil || env.Re != id {
		t.Fatalf("agent.send reply re %q (want %q) %s, %v", env.Re, id, env.Data, err)
	}
	return res.Delivered
}

// notRelayed sends an agent.send and reads its reply, which must be error{code, scope request}.
func notRelayed(t *testing.T, c *signaltest.Client, v protocol.AgentSend, code protocol.ErrorCode) protocol.Error {
	t.Helper()
	id := request(t, c, protocol.MessageTypeAgentSend, v)
	pe, re := expectError(t, c, code, protocol.ErrorScopeRequest)
	if re != id {
		t.Fatalf("error re %q, want %q", re, id)
	}
	return pe
}

// expectRecv reads the next message, an agent.recv notification from connection from with the kind, and returns
// its payload.
func expectRecv(t *testing.T, c *signaltest.Client, from, kind string) json.RawMessage {
	t.Helper()
	env := expectType(t, c, protocol.MessageTypeAgentRecv)
	v, err := protocol.Decode[protocol.AgentRecv](env)
	if err != nil {
		t.Fatalf("decode agent.recv: %v", err)
	}
	if env.ID != "" || env.Re != "" || v.From != from || v.Kind != kind {
		t.Fatalf("agent.recv id %q re %q %s, want a notification from %s with kind %s", env.ID, env.Re, env.Data, from, kind)
	}
	return v.Payload
}

// nothingFor checks that the clients are open and have nothing queued once every goroutine has come to rest.
func nothingFor(t *testing.T, cs ...*signaltest.Client) {
	t.Helper()
	synctest.Wait()
	for _, c := range cs {
		expectOpen(t, c)
	}
}

// Two connections of one user exchange messages (01 §8.14): the page asks the user's agent by role, and the agent
// answers the page by the connection id that agent.recv gave it. The wire format is the spec's example.
func TestRelayExchange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, id := e.user(false)
		page, pageID := e.relayConn(cookie, protocol.RoleFull)
		// The agent has a session of its own (a device token from M4 on): the relay goes by user.
		agent, agentID := e.relayConn(e.session(id, "agent-session"), protocol.RoleAgent)

		rid := request(t, page, protocol.MessageTypeAgentSend, protocol.AgentSend{ToRole: protocol.RoleAgent,
			Kind: "share.request", Payload: json.RawMessage(`{"preset":"game"}`)})
		if env := expectType(t, page, protocol.MessageTypeOK); env.Re != rid || string(env.Data) != `{"delivered":1}` {
			t.Errorf("reply re %q (want %q) %s, want delivered 1", env.Re, rid, env.Data)
		}
		got := expectType(t, agent, protocol.MessageTypeAgentRecv)
		want := fmt.Sprintf(`{"from":%q,"kind":"share.request","payload":{"preset":"game"}}`, pageID)
		if got.ID != "" || got.Re != "" || string(got.Data) != want {
			t.Errorf("agent.recv id %q re %q %s, want a notification with %s", got.ID, got.Re, got.Data, want)
		}

		if n := relayed(t, agent, protocol.AgentSend{To: pageID, Kind: "share.status",
			Payload: json.RawMessage(`{"state":"started","shareId":"s_k3v9q2m7xw4pa8d1"}`)}); n != 1 {
			t.Errorf("delivered %d, want 1", n)
		}
		if p := expectRecv(t, page, agentID, "share.status"); string(p) != `{"state":"started","shareId":"s_k3v9q2m7xw4pa8d1"}` {
			t.Errorf("payload %s", p)
		}
		nothingFor(t, page, agent)

		for _, m := range []struct {
			typ, dir string
			want     float64
		}{{"agent.send", "in", 2}, {"agent.recv", "out", 2}} {
			if got := e.metric("isshoni_ws_messages_total", "type", m.typ, "dir", m.dir); got != m.want {
				t.Errorf("isshoni_ws_messages_total{type=%s,dir=%s} = %v, want %v", m.typ, m.dir, got, m.want)
			}
		}
		if e.logs.count("level=DEBUG", "msg=relay", "conn_id="+pageID, "kind=share.request", "delivered=1") != 1 {
			t.Errorf("no relay log line for the page's message:\n%s", e.logs)
		}
	})
}

// The relay only reaches the sender's own user (01 §8.14, §17): another user's connection, by its id or by its
// role, gets nothing, and the sender gets agent_target_not_found, the very answer that an unknown id gets, so the
// relay does not tell which connection ids exist.
func TestRelayOtherUser(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, _ := e.user(false)
		cookieB, _ := e.user(false)
		a, aID := e.relayConn(cookieA, protocol.RoleFull)
		b, bID := e.relayConn(cookieB, protocol.RoleAgent)

		// reply returns the data of the error that answers v.
		reply := func(c *signaltest.Client, v protocol.AgentSend) string {
			t.Helper()
			id := request(t, c, protocol.MessageTypeAgentSend, v)
			env := expectType(t, c, protocol.MessageTypeError)
			pe, err := protocol.Decode[protocol.Error](env)
			if err != nil || env.Re != id || pe.Code != protocol.ErrorCodeAgentTargetNotFound ||
				pe.Scope != protocol.ErrorScopeRequest || pe.Retryable {
				t.Fatalf("reply re %q (want %q) %s, %v; want agent_target_not_found, scope request, not retryable",
					env.Re, id, env.Data, err)
			}
			return string(env.Data)
		}
		other := reply(a, toConn(bID, "share.request"))
		unknown := reply(a, toConn("c_0000000000000000", "share.request"))
		if other != unknown {
			t.Errorf("another user's connection is answered with %s, an unknown one with %s; want the same", other, unknown)
		}
		reply(a, toRole(protocol.RoleAgent, "share.request")) // only the other user has an agent
		reply(b, toConn(aID, "share.status"))
		reply(b, toRole(protocol.RoleFull, "share.status"))
		nothingFor(t, a, b)

		if got := e.metric("isshoni_ws_errors_total", "code", "agent_target_not_found"); got != 5 {
			t.Errorf("isshoni_ws_errors_total{code=agent_target_not_found} = %v, want 5", got)
		}
		if got := e.metric("isshoni_ws_messages_total", "type", "agent.recv", "dir", "out"); got > 0 {
			t.Errorf("%v agent.recv sent, want none", got)
		}
	})
}

// sizedPayload is a JSON object of exactly n bytes.
func sizedPayload(n int) json.RawMessage {
	const wrap = `{"p":""}`
	return json.RawMessage(`{"p":"` + strings.Repeat("x", n-len(wrap)) + `"}`)
}

// A payload of 17 KiB gets message_too_large (01 §13: at most 16 KiB), and nothing is relayed. The limit is 16 KiB
// to the byte, and the connection stays open.
func TestRelayPayloadTooLarge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		page, pageID := e.relayConn(cookie, protocol.RoleFull)
		agent, _ := e.relayConn(cookie, protocol.RoleAgent)
		send := func(n int) protocol.AgentSend {
			return protocol.AgentSend{ToRole: protocol.RoleAgent, Kind: "share.request", Payload: sizedPayload(n)}
		}

		if pe := notRelayed(t, page, send(17<<10), protocol.ErrorCodeMessageTooLarge); pe.Retryable {
			t.Errorf("message_too_large is retryable")
		}
		notRelayed(t, page, send(16<<10+1), protocol.ErrorCodeMessageTooLarge)
		// Too large comes before the target: an oversized message to nobody is too large, too.
		big := send(17 << 10)
		big.ToRole = protocol.RolePublisher
		notRelayed(t, page, big, protocol.ErrorCodeMessageTooLarge)
		nothingFor(t, agent)

		if n := relayed(t, page, send(16<<10)); n != 1 {
			t.Errorf("delivered %d, want 1", n)
		}
		if p := expectRecv(t, agent, pageID, "share.request"); len(p) != 16<<10 {
			t.Errorf("payload of %d bytes, want %d", len(p), 16<<10)
		}
		nothingFor(t, page, agent)
	})
}

// agent.send is limited to 10 per second per connection (01 §13): of 11 at once the 11th gets rate_limited with
// retryAfterMs, the time until the next one may pass. The user's other connection has a bucket of its own.
func TestRelayRateLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		page, pageID := e.relayConn(cookie, protocol.RoleFull)
		agent, agentID := e.relayConn(cookie, protocol.RoleAgent)
		send := toRole(protocol.RoleAgent, "share.request")

		var ids []string
		for range 11 {
			ids = append(ids, request(t, page, protocol.MessageTypeAgentSend, send))
		}
		for i := range 10 {
			if env := expectType(t, page, protocol.MessageTypeOK); env.Re != ids[i] {
				t.Fatalf("reply %d: re %q, want %q", i+1, env.Re, ids[i])
			}
		}
		pe, re := expectError(t, page, protocol.ErrorCodeRateLimited, protocol.ErrorScopeRequest)
		if re != ids[10] || pe.RetryAfterMs != 100 || !pe.Retryable {
			t.Errorf("re %q, retryAfterMs %d, retryable %v; want %s, 100 (one message per 100 ms), true", re,
				pe.RetryAfterMs, pe.Retryable, ids[10])
		}
		for range 10 {
			expectRecv(t, agent, pageID, "share.request")
		}
		nothingFor(t, page, agent) // the 11th was not relayed

		if n := relayed(t, agent, toConn(pageID, "share.status")); n != 1 {
			t.Errorf("the agent's own message: delivered %d, want 1", n)
		}
		expectRecv(t, page, agentID, "share.status")

		// 100 ms later one more passes, and only one.
		time.Sleep(100 * time.Millisecond)
		if n := relayed(t, page, send); n != 1 {
			t.Errorf("after 100 ms: delivered %d, want 1", n)
		}
		expectRecv(t, agent, pageID, "share.request")
		notRelayed(t, page, send, protocol.ErrorCodeRateLimited)

		// A second later the bucket is full again. A message that finds no target, or is too large, costs a token
		// like any other: after ten of them the next message is refused, whatever it is.
		time.Sleep(time.Second)
		big := send
		big.Payload = sizedPayload(17 << 10)
		for range 5 {
			notRelayed(t, page, toConn("c_0000000000000000", "share.request"), protocol.ErrorCodeAgentTargetNotFound)
			notRelayed(t, page, big, protocol.ErrorCodeMessageTooLarge)
		}
		notRelayed(t, page, send, protocol.ErrorCodeRateLimited)
		nothingFor(t, page, agent)
	})
}

// A steady 11 messages per second (01 §13: 10 per second): the bucket's ten tokens make up for the missing one per
// second for about 9 s, so the first message refused is about the 100th; from then on ten per second pass, 309 of
// the 330 in 30 s. A steady ten per second is never refused.
func TestRelayRateLimitSteady(t *testing.T) {
	for _, tc := range []struct {
		perSecond            int
		minPassed, maxPassed int
		minFirst, maxFirst   int // the first message refused, counted from 1; 0 and 0: none is
	}{
		{10, 300, 300, 0, 0},
		{11, 307, 311, 95, 105},
	} {
		t.Run(fmt.Sprintf("%d per second", tc.perSecond), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t)
				defer e.close()
				cookie, _ := e.user(false)
				page, pageID := e.relayConn(cookie, protocol.RoleFull)
				agent, _ := e.relayConn(cookie, protocol.RoleAgent)
				const seconds = 30
				sent := tc.perSecond * seconds
				for range sent {
					request(t, page, protocol.MessageTypeAgentSend, toRole(protocol.RoleAgent, "share.request"))
					time.Sleep(time.Second / time.Duration(tc.perSecond))
				}
				passed, first := 0, 0
				for i := 1; i <= sent; i++ {
					env := recv(t, page)
					switch env.Type {
					case protocol.MessageTypeOK:
						passed++
					case protocol.MessageTypeError:
						if pe, err := protocol.Decode[protocol.Error](env); err != nil || pe.Code != protocol.ErrorCodeRateLimited ||
							pe.RetryAfterMs <= 0 {
							t.Fatalf("message %d: error %s, %v; want rate_limited with retryAfterMs", i, env.Data, err)
						}
						if first == 0 {
							first = i
						}
					default:
						t.Fatalf("message %d: got %s %s", i, env.Type, env.Data)
					}
				}
				if passed < tc.minPassed || passed > tc.maxPassed {
					t.Errorf("%d of %d passed, want %d to %d", passed, sent, tc.minPassed, tc.maxPassed)
				}
				if first < tc.minFirst || first > tc.maxFirst {
					t.Errorf("the first message refused is number %d, want %d to %d (0: none)", first, tc.minFirst, tc.maxFirst)
				}
				for range passed {
					expectRecv(t, agent, pageID, "share.request")
				}
				nothingFor(t, page, agent)
			})
		})
	}
}

// Who is a target (01 §8.14): by role, every other connection of the user with that role; by id, that one
// connection. Never the sender itself, never a connection without the agent.relay feature, and never one that is
// detached or gone, since what the hub sends a detached connection is dropped: delivered counts the connections that
// got the message, and without one the answer is agent_target_not_found.
func TestRelayTargets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, id := e.user(false)
		cookie2 := e.session(id, "second-session")
		cookieO, _ := e.user(false)
		page, pageID := e.relayConn(cookie, protocol.RoleFull)
		agent1, agent1ID := e.relayConn(cookie, protocol.RoleAgent)
		agent2, w2 := e.connect(cookie2, relayHello(protocol.RoleAgent))
		agent2ID := w2.ConnectionID
		// A viewer of the user that did not ask for the feature, and another user's agent.
		h := signaltest.DefaultHello()
		h.Role = protocol.RoleViewer
		plain, wp := e.connect(cookie, h)
		other, _ := e.relayConn(cookieO, protocol.RoleAgent)
		const kind = "share.request"

		if n := relayed(t, page, toRole(protocol.RoleAgent, kind)); n != 2 {
			t.Errorf("by role: delivered %d, want 2 (both agents of the user)", n)
		}
		expectRecv(t, agent1, pageID, kind)
		expectRecv(t, agent2, pageID, kind)
		if n := relayed(t, page, toConn(agent2ID, kind)); n != 1 {
			t.Errorf("by id: delivered %d, want 1", n)
		}
		expectRecv(t, agent2, pageID, kind)

		// The sender is not its own target.
		if n := relayed(t, agent1, toRole(protocol.RoleAgent, kind)); n != 1 {
			t.Errorf("an agent to the agents: delivered %d, want 1 (the other one)", n)
		}
		expectRecv(t, agent2, agent1ID, kind)
		notRelayed(t, page, toRole(protocol.RoleFull, kind), protocol.ErrorCodeAgentTargetNotFound)
		notRelayed(t, page, toConn(pageID, kind), protocol.ErrorCodeAgentTargetNotFound)

		// A connection without the feature gets no agent.recv, and its own agent.send gets feature_disabled.
		notRelayed(t, page, toConn(wp.ConnectionID, kind), protocol.ErrorCodeAgentTargetNotFound)
		notRelayed(t, page, toRole(protocol.RoleViewer, kind), protocol.ErrorCodeAgentTargetNotFound)
		notRelayed(t, plain, toRole(protocol.RoleAgent, kind), protocol.ErrorCodeFeatureDisabled)
		nothingFor(t, page, agent1, agent2, plain, other)

		// agent2 loses its socket: detached, it is no target, and the message is not kept for it. (The second that
		// passes first refills the page's ten messages per second.)
		time.Sleep(time.Second)
		agent2.Close()
		synctest.Wait()
		if n := relayed(t, page, toRole(protocol.RoleAgent, kind)); n != 1 {
			t.Errorf("with one agent detached: delivered %d, want 1", n)
		}
		expectRecv(t, agent1, pageID, kind)
		notRelayed(t, page, toConn(agent2ID, kind), protocol.ErrorCodeAgentTargetNotFound)

		// It resumes: a target again, under its connection id, with the feature it had.
		rh := relayHello(protocol.RoleAgent)
		rh.ResumeToken = w2.ResumeToken
		agent2, w2 = e.connect(cookie2, rh)
		if !w2.Resumed || w2.ConnectionID != agent2ID || !slices.Contains(w2.Features, protocol.FeatureAgentRelay) {
			t.Fatalf("resumed %v as %s with features %v; want %s back with agent.relay", w2.Resumed, w2.ConnectionID,
				w2.Features, agent2ID)
		}
		if n := relayed(t, page, toConn(agent2ID, kind)); n != 1 {
			t.Errorf("after the resume: delivered %d, want 1", n)
		}
		expectRecv(t, agent2, pageID, kind)

		// agent1 leaves for good: its id names nobody any more.
		if err := agent1.CloseWith(websocket.StatusNormalClosure); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		notRelayed(t, page, toConn(agent1ID, kind), protocol.ErrorCodeAgentTargetNotFound)
		if n := relayed(t, page, toRole(protocol.RoleAgent, kind)); n != 1 {
			t.Errorf("with one agent gone: delivered %d, want 1", n)
		}
		expectRecv(t, agent2, pageID, kind)
		nothingFor(t, page, agent2, plain, other)
	})
}

// A connection whose socket the hub is closing is no target either. Here the agent's session is revoked and its
// client has not read the error yet: the connection is still there, with its socket, and takes nothing more.
func TestRelayClosingTarget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, id := e.user(false)
		page, _ := e.relayConn(cookie, protocol.RoleFull)
		agent, agentID := e.relayConn(e.session(id, "agent-session"), protocol.RoleAgent)

		agent.Pause() // the client reads the error at most: the close of its socket, which the hub starts, can't finish
		if n := e.hub.CloseConnections(signal.ConnSelector{UserID: id.UserID, SessionID: "agent-session"},
			protocol.ErrorCodeSessionRevoked); n != 1 {
			t.Fatalf("closed %d connections, want 1", n)
		}
		synctest.Wait()
		if conns, socks, _, _ := signal.Counts(e.hub, id.UserID); conns != 2 || socks != 2 {
			t.Fatalf("conns %d, sockets %d while the agent's socket closes; want 2, 2", conns, socks)
		}
		notRelayed(t, page, toConn(agentID, "share.request"), protocol.ErrorCodeAgentTargetNotFound)
		notRelayed(t, page, toRole(protocol.RoleAgent, "share.request"), protocol.ErrorCodeAgentTargetNotFound)

		agent.Resume()
		expectFail(t, agent, protocol.ErrorCodeSessionRevoked, protocol.ErrorScopeSession)
		nothingFor(t, page)
	})
}

// Every role may send agent.send (01 §6.3), and a connection of any role is a target by that role. One user has a
// connection of each role; each sends to each role, its own included, where it finds nobody: the sender is not its
// own target.
func TestRelayRoles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		roles := []protocol.Role{protocol.RoleFull, protocol.RoleViewer, protocol.RolePublisher, protocol.RoleAgent}
		clients, ids := map[protocol.Role]*signaltest.Client{}, map[protocol.Role]string{}
		for _, r := range roles {
			clients[r], ids[r] = e.relayConn(cookie, r)
		}
		for _, from := range roles {
			if !protocol.Allowed(from, protocol.MessageTypeAgentSend, "") {
				t.Errorf("the registry does not allow agent.send for %s", from)
			}
			for _, to := range roles {
				v := toRole(to, "playback.stop")
				if to == from {
					notRelayed(t, clients[from], v, protocol.ErrorCodeAgentTargetNotFound)
					continue
				}
				if n := relayed(t, clients[from], v); n != 1 {
					t.Errorf("%s to %s: delivered %d, want 1", from, to, n)
				}
				expectRecv(t, clients[to], ids[from], "playback.stop")
			}
		}
		for _, r := range roles {
			nothingFor(t, clients[r])
		}
	})
}

// An agent.send that is not valid gets bad_request with the field (01 §8.14, §12.1), and nothing is relayed.
func TestRelayBadRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		page, _ := e.relayConn(cookie, protocol.RoleFull)
		agent, agentID := e.relayConn(cookie, protocol.RoleAgent)
		for _, tc := range []struct{ data, field, reason string }{
			{`{"kind":"share.request","payload":{}}`, "to", "required"},
			{`{"to":"` + agentID + `","toRole":"agent","kind":"share.request","payload":{}}`, "toRole", "invalid"},
			{`{"toRole":"robot","kind":"share.request","payload":{}}`, "toRole", "invalid"},
			{`{"to":"not an id","kind":"share.request","payload":{}}`, "to", "invalid"},
			{`{"toRole":"agent","payload":{}}`, "kind", "required"},
			{`{"toRole":"agent","kind":"Share.Request","payload":{}}`, "kind", "invalid"},
			{`{"toRole":"agent","kind":"` + strings.Repeat("a", 33) + `","payload":{}}`, "kind", "too_long"},
			{`{"toRole":"agent","kind":"share.request"}`, "payload", "required"},
		} {
			id := page.NextID()
			if err := page.SendRaw([]byte(`{"type":"agent.send","id":"` + id + `","data":` + tc.data + `}`)); err != nil {
				t.Fatal(err)
			}
			pe, re := expectError(t, page, protocol.ErrorCodeBadRequest, protocol.ErrorScopeRequest)
			if re != id || pe.Params["field"] != tc.field || pe.Params["reason"] != tc.reason {
				t.Errorf("%s: re %q (want %q), params %v; want field %s, reason %s", tc.data, re, id, pe.Params,
					tc.field, tc.reason)
			}
		}
		nothingFor(t, page, agent)
	})
}

// The payload is opaque to the hub (01 §8.14): any JSON value goes through as that value, whatever is in it, and it
// is never logged (01 §17).
func TestRelayPayloadOpaque(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		page, pageID := e.relayConn(cookie, protocol.RoleFull)
		agent, _ := e.relayConn(cookie, protocol.RoleAgent)
		const canary = "relay-canary-7f3a"
		for _, payload := range []string{
			`{}`,
			`{"note":"` + canary + `","nested":{"list":[1,2.5,-3e2,true,false,null,"x"],"deep":{"a":{"b":{"c":[]}}}}}`,
			`{"html":"<b>a&b</b>","escaped":"\u003c\u2028\"\\","text":"héllo 日本語 🎬"}`,
			`{ "spaced" : [ 1 , 2 ] ,` + "\n\t" + `"type" : "ok" , "from" : "c_0000000000000000" }`,
			`[1,"two",{"three":3}]`,
			`"` + canary + `"`,
			`42`,
			`true`,
		} {
			id := page.NextID()
			if err := page.SendRaw([]byte(`{"type":"agent.send","id":"` + id +
				`","data":{"toRole":"agent","kind":"exclusions.set","payload":` + payload + `}}`)); err != nil {
				t.Fatal(err)
			}
			if env := expectType(t, page, protocol.MessageTypeOK); env.Re != id {
				t.Fatalf("%s: reply re %q, want %q", payload, env.Re, id)
			}
			got := expectRecv(t, agent, pageID, "exclusions.set")
			var want, have any
			if err := json.Unmarshal([]byte(payload), &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(got, &have); err != nil {
				t.Fatalf("%s: relayed as %s: %v", payload, got, err)
			}
			if !reflect.DeepEqual(have, want) {
				t.Errorf("payload %s was relayed as %s", payload, got)
			}
		}
		nothingFor(t, page, agent)
		if logs := e.logs.String(); strings.Contains(logs, canary) || strings.Contains(logs, "héllo") {
			t.Errorf("a payload is in the log:\n%s", logs)
		}
		if n := e.logs.count("level=DEBUG", "msg=relay", "kind=exclusions.set"); n != 8 {
			t.Errorf("%d relay log lines, want 8", n)
		}
	})
}

// The sender never waits for a target (01 §15.2: no actor waits for another one). While the agent's actor is busy
// the page gets its ok at once, and the agent gets the messages, in order, when its actor is back. Two connections
// whose actors are both busy when they relay to each other don't block each other either.
func TestRelayNeverWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		page, pageID := e.relayConn(cookie, protocol.RoleFull)
		agent, agentID := e.relayConn(cookie, protocol.RoleAgent)

		release := signal.Stall(e.hub, agentID)
		if release == nil {
			t.Fatal("no such connection")
		}
		defer release()
		for _, kind := range []string{"exclusions.get", "exclusions.set", "share.request"} {
			if n := relayed(t, page, toRole(protocol.RoleAgent, kind)); n != 1 {
				t.Errorf("%s: delivered %d, want 1", kind, n)
			}
		}
		nothingFor(t, agent)
		release()
		for _, kind := range []string{"exclusions.get", "exclusions.set", "share.request"} {
			expectRecv(t, agent, pageID, kind)
		}

		// Both actors are held while each has a message for the other in its inbox.
		releasePage, releaseAgent := signal.Stall(e.hub, pageID), signal.Stall(e.hub, agentID)
		defer releasePage()
		defer releaseAgent()
		toAgent := request(t, page, protocol.MessageTypeAgentSend, toConn(agentID, "share.request"))
		toPage := request(t, agent, protocol.MessageTypeAgentSend, toConn(pageID, "share.status"))
		synctest.Wait()
		releasePage()
		releaseAgent()
		// Each gets its ok and the other's message, in either order.
		for _, x := range []struct {
			c        *signaltest.Client
			re, from string
		}{{page, toAgent, agentID}, {agent, toPage, pageID}} {
			var ok, got bool
			for range 2 {
				switch env := recv(t, x.c); env.Type {
				case protocol.MessageTypeOK:
					ok = env.Re == x.re
				case protocol.MessageTypeAgentRecv:
					v, err := protocol.Decode[protocol.AgentRecv](env)
					got = err == nil && v.From == x.from
				default:
					t.Fatalf("got %s %s", env.Type, env.Data)
				}
			}
			if !ok || !got {
				t.Errorf("ok %v, agent.recv from %s %v; want both", ok, x.from, got)
			}
		}
		nothingFor(t, page, agent)
	})
}
