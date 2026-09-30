package signal_test

import (
	"fmt"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// Read limits after welcome (01 §3.3, §19): pc.offer/pc.answer up to 256 KiB, everything else up to 64 KiB, stats up
// to 16 KiB; a larger message gets message_too_large and the socket stays open.
func TestMessageSizeAfterWelcome(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		c, _ := e.connect(cookie, signaltest.DefaultHello())

		// A 200 KiB sub answer passes the size checks; outside a room it gets not_in_room (scope pc). (No line
		// breaks: JSON escapes each CR and LF into two bytes.)
		sdp := strings.Repeat("a", 200*1024)
		if err := c.Send(protocol.MessageTypePCAnswer, "", protocol.PCAnswer{PC: protocol.PCKindSub, Gen: 1, Neg: 2,
			SDP: protocol.SDP(sdp)}); err != nil {
			t.Fatal(err)
		}
		pe, _ := expectError(t, c, protocol.ErrorCodeNotInRoom, protocol.ErrorScopePC)
		if pe.PC != protocol.PCKindSub || pe.Gen != 1 || pe.Neg != 2 {
			t.Errorf("error pc %q gen %d neg %d", pe.PC, pe.Gen, pe.Neg)
		}

		// A 70 KiB stats report is dropped with message_too_large (without re: stats has no id).
		big := protocol.ClientStats{IntervalMs: 10000, PCs: []protocol.PCStats{{PC: protocol.PCKindSub, Gen: 1,
			State: strings.Repeat("x", 70*1024)}}}
		if err := c.Send(protocol.MessageTypeStats, "", big); err != nil {
			t.Fatal(err)
		}
		if _, re := expectError(t, c, protocol.ErrorCodeMessageTooLarge, protocol.ErrorScopeRequest); re != "" {
			t.Errorf("re %q, want none", re)
		}

		// A 70 KiB request gets message_too_large as its reply.
		id := request(t, c, protocol.MessageTypeStatsWatch, map[string]any{"on": true, "pad": strings.Repeat("x", 70*1024)})
		if _, re := expectError(t, c, protocol.ErrorCodeMessageTooLarge, protocol.ErrorScopeRequest); re != id {
			t.Errorf("re %q, want %q", re, id)
		}
		// 17 KiB of stats is over the 16 KiB stats limit.
		big.PCs[0].State = strings.Repeat("x", 17*1024)
		if err := c.Send(protocol.MessageTypeStats, "", big); err != nil {
			t.Fatal(err)
		}
		expectError(t, c, protocol.ErrorCodeMessageTooLarge, protocol.ErrorScopeRequest)
		ping(t, c)

		// Over the 256 KiB socket limit: 1009.
		huge := protocol.PCAnswer{PC: protocol.PCKindSub, Gen: 1, Neg: 3, SDP: protocol.SDP(strings.Repeat("x", 257*1024))}
		if err := c.Send(protocol.MessageTypePCAnswer, "", huge); err != nil {
			t.Fatal(err)
		}
		expectClose(t, c, protocol.CloseCodeMessageTooBig)
	})
}

// Protocol-level rules for messages after welcome (01 §5, §8.13, §12.1).
func TestMessageRules(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		c, _ := e.connect(cookie, signaltest.DefaultHello())

		// ping → pong with t echoed and the server's clock.
		if err := c.Send(protocol.MessageTypePing, "", protocol.Ping{T: 1760295845123}); err != nil {
			t.Fatal(err)
		}
		pong, err := protocol.Decode[protocol.Pong](expectType(t, c, protocol.MessageTypePong))
		if err != nil || pong.T != 1760295845123 || pong.ServerTimeMs == 0 {
			t.Errorf("pong %+v, %v", pong, err)
		}

		// Unknown types: unknown_type with an id, ignored without one; so is a server→client type.
		if err := c.SendRaw([]byte(`{"type":"future.thing","data":{}}`)); err != nil {
			t.Fatal(err)
		}
		if err := c.SendRaw([]byte(`{"type":"future.thing","id":"u1","data":{"x":[1,2]}}`)); err != nil {
			t.Fatal(err)
		}
		if _, re := expectError(t, c, protocol.ErrorCodeUnknownType, protocol.ErrorScopeRequest); re != "u1" {
			t.Errorf("re %q", re)
		}
		if err := c.SendRaw([]byte(`{"type":"welcome","id":"u2"}`)); err != nil {
			t.Fatal(err)
		}
		expectError(t, c, protocol.ErrorCodeUnknownType, protocol.ErrorScopeRequest)

		// Requests.
		id := request(t, c, protocol.MessageTypeRoomLeave, protocol.Empty{})
		if env := expectType(t, c, protocol.MessageTypeOK); env.Re != id || env.Data != nil {
			t.Errorf("room.leave reply re %q data %s", env.Re, env.Data)
		}
		id = request(t, c, protocol.MessageTypeStatsWatch, protocol.StatsWatch{On: true})
		if env := expectType(t, c, protocol.MessageTypeOK); env.Re != id {
			t.Errorf("stats.watch reply re %q", env.Re)
		}
		id = request(t, c, protocol.MessageTypeRoomJoin, protocol.RoomJoin{RoomID: "bad room!"})
		if _, re := expectError(t, c, protocol.ErrorCodeRoomNotFound, protocol.ErrorScopeRequest); re != id {
			t.Errorf("re %q", re)
		}
		id = request(t, c, protocol.MessageTypeShareStart, protocol.ShareStart{Kind: protocol.ShareKindScreen,
			Preset: protocol.PresetAuto, Ref: "r1"})
		if _, re := expectError(t, c, protocol.ErrorCodeNotInRoom, protocol.ErrorScopeRequest); re != id {
			t.Errorf("re %q", re)
		}
		id = request(t, c, protocol.MessageTypeShareStart, protocol.ShareStart{Kind: "hologram",
			Preset: protocol.PresetAuto, Ref: "r2"})
		pe, re := expectError(t, c, protocol.ErrorCodeBadRequest, protocol.ErrorScopeRequest)
		if re != id || pe.Params["field"] != "kind" || pe.Params["reason"] != "invalid" {
			t.Errorf("re %q, params %v", re, pe.Params)
		}
		id = request(t, c, protocol.MessageTypeAgentSend, protocol.AgentSend{ToRole: protocol.RoleAgent,
			Kind: "share.request", Payload: []byte(`{}`)})
		if _, re := expectError(t, c, protocol.ErrorCodeFeatureDisabled, protocol.ErrorScopeRequest); re != id {
			t.Errorf("re %q", re)
		}

		// share.start in a room is the shares slice's (README S40): error{internal} with a ref, logged at WARN.
		join(t, c, "lounge")
		id = request(t, c, protocol.MessageTypeShareStart, protocol.ShareStart{Kind: protocol.ShareKindScreen,
			Preset: protocol.PresetAuto, Ref: "r3"})
		pe, re = expectError(t, c, protocol.ErrorCodeInternal, protocol.ErrorScopeRequest)
		ref, _ := pe.Params["ref"].(string)
		if re != id || len(ref) != 8 || !pe.Retryable {
			t.Errorf("re %q, ref %q, retryable %v", re, ref, pe.Retryable)
		}
		if e.logs.count("level=WARN", "not implemented yet", "ref="+ref, "type=share.start") != 1 {
			t.Errorf("no WARN line for ref %s:\n%s", ref, e.logs)
		}

		// Notifications with bad payloads are dropped; a pc.* notification gets bad_request with scope pc.
		if err := c.SendRaw([]byte(`{"type":"caps.update","data":{"caps":{"decode":"opus"}}}`)); err != nil {
			t.Fatal(err)
		}
		if err := c.SendRaw([]byte(`{"type":"pc.ice","data":{"pc":"sub","gen":0}}`)); err != nil {
			t.Fatal(err)
		}
		pe, _ = expectError(t, c, protocol.ErrorCodeBadRequest, protocol.ErrorScopePC)
		if pe.PC != protocol.PCKindSub || pe.Params["field"] != "gen" {
			t.Errorf("pc error %+v", pe)
		}
		// pc.close for sub is ignored (01 §8.8).
		if err := c.Send(protocol.MessageTypePCClose, "", protocol.PCClose{PC: protocol.PCKindSub, Gen: 1}); err != nil {
			t.Fatal(err)
		}
		ping(t, c)

		// A request without an id is a bad envelope: bad_message and 4400.
		if err := c.SendRaw([]byte(`{"type":"room.leave","data":{}}`)); err != nil {
			t.Fatal(err)
		}
		expectFail(t, c, protocol.ErrorCodeBadMessage, protocol.ErrorScopeConnection)
	})
}

// A frame that isn't a JSON envelope after welcome: bad_message and 4400.
func TestMessageBadEnvelope(t *testing.T) {
	for _, frame := range []string{`[1,2]`, `{"type":""}`, `{"type":"ping","id":"a b"}`, `{"type":"ping","data":[1]}`, "\xff"} {
		t.Run(fmt.Sprintf("%q", frame), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t)
				defer e.close()
				cookie, _ := e.user(false)
				c, _ := e.connect(cookie, signaltest.DefaultHello())
				if err := c.SendRaw([]byte(frame)); err != nil {
					t.Fatal(err)
				}
				expectFail(t, c, protocol.ErrorCodeBadMessage, protocol.ErrorScopeConnection)
			})
		})
	}
}

// Every client message × every role against 01 §6.3: a disallowed message gets forbidden (scope request, or pc for
// pc.*); an allowed one never does.
func TestRoleMatrix(t *testing.T) {
	type msg struct {
		typ  protocol.MessageType
		pc   protocol.PCKind // pc.* only
		data any
	}
	sdp := protocol.SDP("v=0\r\n")
	msgs := []msg{
		{typ: protocol.MessageTypePing, data: protocol.Ping{T: 1}},
		{typ: protocol.MessageTypeRoomJoin, data: protocol.RoomJoin{RoomID: "lounge"}},
		{typ: protocol.MessageTypeRoomLeave, data: protocol.Empty{}},
		{typ: protocol.MessageTypeShareStart, data: protocol.ShareStart{Kind: protocol.ShareKindScreen,
			Preset: protocol.PresetAuto, Ref: "r1"}},
		{typ: protocol.MessageTypeShareUpdate, data: protocol.ShareUpdate{ShareID: "s_x", Preset: protocol.PresetMovie}},
		{typ: protocol.MessageTypeShareStop, data: protocol.ShareStop{ShareID: "s_x"}},
		{typ: protocol.MessageTypeSubscribeUpdate, data: protocol.SubscribeUpdate{Subs: []protocol.SubscriptionWant{
			{ShareID: "s_x", Video: protocol.VideoLayerHigh, Audio: protocol.AudioStateOn}}}},
		{typ: protocol.MessageTypeCapsUpdate, data: protocol.CapsUpdate{Caps: protocol.Caps{
			Decode: []protocol.CodecKey{protocol.CodecOpus}}}},
		{typ: protocol.MessageTypeStats, data: protocol.ClientStats{IntervalMs: 10000}},
		{typ: protocol.MessageTypeStatsWatch, data: protocol.StatsWatch{On: true}},
	}
	for _, pc := range []protocol.PCKind{protocol.PCKindPub, protocol.PCKindSub} {
		msgs = append(msgs,
			msg{protocol.MessageTypePCOffer, pc, protocol.PCOffer{PC: pc, Gen: 1, Neg: 1, SDP: sdp}},
			msg{protocol.MessageTypePCAnswer, pc, protocol.PCAnswer{PC: pc, Gen: 1, Neg: 1, SDP: sdp}},
			msg{protocol.MessageTypePCICE, pc, protocol.PCICE{PC: pc, Gen: 1}},
			msg{protocol.MessageTypePCRestart, pc, protocol.PCRestart{PC: pc, Gen: 1, Mode: protocol.RestartModeICE,
				Reason: protocol.RestartReasonDisconnected}},
			msg{protocol.MessageTypePCClose, pc, protocol.PCClose{PC: pc, Gen: 1}},
		)
	}
	// Every client→server type of the registry is covered (agent.send is behind a feature that is off).
	covered := map[protocol.MessageType]bool{protocol.MessageTypeHello: true, protocol.MessageTypeAgentSend: true}
	for _, m := range msgs {
		covered[m.typ] = true
	}
	for _, s := range protocol.Registry {
		if s.Dir == protocol.DirClientToServer && !covered[s.Type] {
			t.Errorf("%s is not in the matrix", s.Type)
		}
	}

	for _, role := range []protocol.Role{protocol.RoleFull, protocol.RoleViewer, protocol.RolePublisher, protocol.RoleAgent} {
		t.Run(string(role), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t)
				defer e.close()
				cookie, _ := e.user(false)
				h := signaltest.DefaultHello()
				h.Role = role
				c, _ := e.connect(cookie, h)
				for _, m := range msgs {
					spec, _ := protocol.Lookup(m.typ, protocol.DirClientToServer)
					id := ""
					if spec.Kind == protocol.KindRequest {
						id = c.NextID()
					}
					if err := c.Send(m.typ, id, m.data); err != nil {
						t.Fatal(err)
					}
					allowed := protocol.Allowed(role, m.typ, m.pc)
					if !allowed {
						scope := protocol.ErrorScopeRequest
						if m.pc != "" {
							scope = protocol.ErrorScopePC
						}
						pe, re := expectError(t, c, protocol.ErrorCodeForbidden, scope)
						if re != id || pe.PC != m.pc {
							t.Errorf("%s %s: re %q, pc %q", m.typ, m.pc, re, pe.PC)
						}
						continue
					}
					// Whatever the answer (ok, pong, not_in_room, internal, nothing), it is not forbidden.
					if err := c.Send(protocol.MessageTypePing, "", protocol.Ping{T: 99}); err != nil {
						t.Fatal(err)
					}
					for {
						env := recv(t, c)
						if env.Type == protocol.MessageTypePong {
							if p, _ := protocol.Decode[protocol.Pong](env); p.T == 99 {
								break
							}
							continue
						}
						if pe, _ := protocol.Decode[protocol.Error](env); pe.Code == protocol.ErrorCodeForbidden {
							t.Errorf("%s %s: forbidden for %s", m.typ, m.pc, role)
						}
					}
				}
			})
		})
	}
}
