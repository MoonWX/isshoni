package signal_test

import (
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// Negotiation plumbing (01 §8.8–8.10, §9) against the fake MediaPlane: pc.* routing with its role, direction and
// ownership checks (the SFU checks gen and neg), the pub offer's track rules, pc.close, subscribe.update and
// subscribe.status, quality.hint and caps.update.

const (
	pub = protocol.PCKindPub
	sub = protocol.PCKindSub
)

const offerSDP protocol.SDP = "v=0\r\no=- 1 1 IN IP4 192.0.2.7\r\ns=-\r\nt=0 0\r\n"

func videoTrack(mid, shareID string) protocol.TrackRef {
	return protocol.TrackRef{MID: mid, ShareID: shareID, Kind: protocol.TrackKindVideo}
}

func audioTrack(mid, shareID string) protocol.TrackRef {
	return protocol.TrackRef{MID: mid, ShareID: shareID, Kind: protocol.TrackKindAudio}
}

// notify sends a notification.
func notify(t *testing.T, c *signaltest.Client, typ protocol.MessageType, data any) {
	t.Helper()
	if err := c.Send(typ, "", data); err != nil {
		t.Fatalf("send %s: %v", typ, err)
	}
}

// pubOffer sends a pub offer with the tracks.
func pubOffer(t *testing.T, c *signaltest.Client, gen, neg uint32, tracks ...protocol.TrackRef) {
	t.Helper()
	notify(t, c, protocol.MessageTypePCOffer, protocol.PCOffer{PC: pub, Gen: gen, Neg: neg, SDP: offerSDP, Tracks: tracks})
}

// next reads the next message that is not room traffic.
func next(t *testing.T, c *signaltest.Client) protocol.Envelope {
	t.Helper()
	for {
		env := recv(t, c)
		if env.Type != protocol.MessageTypeRoomState && env.Type != protocol.MessageTypeRoomEvent {
			return env
		}
	}
}

// expectAnswer reads the pc.answer of pub offer (gen, neg), skipping room traffic.
func expectAnswer(t *testing.T, c *signaltest.Client, gen, neg uint32) {
	t.Helper()
	env := next(t, c)
	a, err := protocol.Decode[protocol.PCAnswer](env)
	if env.Type != protocol.MessageTypePCAnswer || err != nil || env.Re != "" ||
		a != (protocol.PCAnswer{PC: pub, Gen: gen, Neg: neg, SDP: signaltest.FakeSDP}) {
		t.Fatalf("got %s %s (%v), want the pc.answer of pub offer gen %d neg %d", env.Type, env.Data, err, gen, neg)
	}
}

// expectPCError reads the next message that is not room traffic: error{code, scope pc}, a notification.
func expectPCError(t *testing.T, c *signaltest.Client, code protocol.ErrorCode) protocol.Error {
	t.Helper()
	env := next(t, c)
	pe, err := protocol.Decode[protocol.Error](env)
	if env.Type != protocol.MessageTypeError || err != nil || env.Re != "" || pe.Code != code ||
		pe.Scope != protocol.ErrorScopePC {
		t.Fatalf("got %s %s (%v), want error %s with scope pc", env.Type, env.Data, err, code)
	}
	return pe
}

// offers returns the pub offers that the MediaPeer was given.
func offers(p *signaltest.Peer) []protocol.PCOffer {
	var out []protocol.PCOffer
	for _, c := range p.CallsTo("HandleOffer") {
		out = append(out, c.Args[0].(protocol.PCOffer))
	}
	return out
}

// methods returns the names of the calls.
func methods(calls []signaltest.Call) []string {
	var out []string
	for _, c := range calls {
		out = append(out, c.Method)
	}
	return out
}

// A pub offer's tracks (01 §8.7, §9 rule 4, §19): only the TrackRefs of shares that this connection publishes reach
// the MediaPeer, and the server still answers; a share that an earlier offer bound and that the new offer omits ends
// with stopped before the offer is applied, whatever the gen; a starting share that no offer has bound is left
// alone. pc.close ends the shares that the pub PC carried.
func TestPCOfferTracks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		cookieB, _ := e.user(false)
		ca, wa := e.connect(cookieA, signaltest.DefaultHello())
		cb, _ := e.connect(cookieB, signaltest.DefaultHello())
		join(t, ca, "lounge")
		join(t, cb, "lounge")
		peer := e.media.Peer(wa.ConnectionID)
		s1 := startShare(t, ca, screen("r1")).ShareID
		s2 := startShare(t, ca, screen("r2")).ShareID
		sB := startShare(t, cb, screen("r1")).ShareID
		quiet(t, ca, cb)

		// Somebody else's share and an unknown one (it ended in a race) are dropped from the tracks.
		pubOffer(t, ca, 1, 1, videoTrack("0", s1), audioTrack("1", s1), videoTrack("2", sB), videoTrack("3", "s_0000000000000000"))
		expectAnswer(t, ca, 1, 1)
		got := offers(peer)
		if len(got) != 1 || got[0].PC != pub || got[0].Gen != 1 || got[0].Neg != 1 || got[0].SDP != offerSDP ||
			!slices.Equal(got[0].Tracks, []protocol.TrackRef{videoTrack("0", s1), audioTrack("1", s1)}) {
			t.Fatalf("HandleOffer got %+v, want the offer with the tracks of the connection's own share", got)
		}
		// The second share was never bound: an offer without it leaves it alone.
		pubOffer(t, ca, 1, 2, videoTrack("0", s1), audioTrack("1", s1))
		expectAnswer(t, ca, 1, 2)
		settle()
		if evs, _ := drain(t, cb); len(evs) != 0 {
			t.Fatalf("events %+v, want none: nothing ended", evs)
		}

		// Now the second share is bound, and the first one omitted: it ends with stopped before the MediaPeer sees
		// the offer.
		pubOffer(t, ca, 1, 3, videoTrack("4", s2))
		expectAnswer(t, ca, 1, 3)
		if ev := expectEvent(t, cb, protocol.RoomEventKindShareStopped, a.UserID); ev.ShareID != s1 ||
			ev.Reason != protocol.EndReasonStopped {
			t.Errorf("share.stopped %+v, want the first share, stopped", ev)
		}
		calls := peer.MediaCalls()
		n := len(calls)
		if calls[n-2].Method != "EndShare" || calls[n-2].Args[0] != s1 || calls[n-2].Args[1] != protocol.EndReasonStopped ||
			calls[n-1].Method != "HandleOffer" || calls[n-1].Args[0].(protocol.PCOffer).Neg != 3 {
			t.Errorf("MediaPeer calls %v, want EndShare(%s, stopped) before HandleOffer", methods(calls), s1)
		}

		// A rebuilt pub PC (a new gen) that no longer lists the second share: the same rule.
		quiet(t, ca, cb)
		pubOffer(t, ca, 2, 1)
		expectAnswer(t, ca, 2, 1)
		if ev := expectEvent(t, cb, protocol.RoomEventKindShareStopped, a.UserID); ev.ShareID != s2 ||
			ev.Reason != protocol.EndReasonStopped {
			t.Errorf("share.stopped %+v, want the second share, stopped", ev)
		}
		calls = peer.MediaCalls()
		n = len(calls)
		if calls[n-2].Method != "EndShare" || calls[n-2].Args[0] != s2 || calls[n-1].Method != "HandleOffer" ||
			len(calls[n-1].Args[0].(protocol.PCOffer).Tracks) != 0 {
			t.Errorf("MediaPeer calls %v, want EndShare(%s) before the HandleOffer of gen 2", methods(calls), s2)
		}

		// pc.close: the share that the pub PC carried ends with stopped, then the MediaPeer closes the PC. A share
		// that no offer has bound yet stays, until its start timeout.
		s3 := startShare(t, ca, screen("r3")).ShareID
		pubOffer(t, ca, 2, 2, videoTrack("0", s3))
		expectAnswer(t, ca, 2, 2)
		time.Sleep(10 * time.Second)
		s4 := startShare(t, ca, screen("r4")).ShareID
		quiet(t, ca, cb)
		notify(t, ca, protocol.MessageTypePCClose, protocol.PCClose{PC: pub, Gen: 2})
		settle()
		evs, st := drain(t, cb)
		if len(evs) != 1 || evs[0].ShareID != s3 || evs[0].Reason != protocol.EndReasonStopped {
			t.Errorf("events after pc.close %+v, want share.stopped{stopped} of the bound share", evs)
		}
		if st == nil || len(st.Shares) != 2 || shareIn(st, s4) == nil || shareIn(st, sB) == nil {
			t.Errorf("state after pc.close %+v, want the unbound share and B's", st)
		}
		calls = peer.MediaCalls()
		n = len(calls)
		if calls[n-2].Method != "EndShare" || calls[n-2].Args[0] != s3 || calls[n-1].Method != "ClosePC" ||
			calls[n-1].Args[0] != (protocol.PCClose{PC: pub, Gen: 2}) {
			t.Errorf("MediaPeer calls %v, want EndShare(%s), then ClosePC", methods(calls), s3)
		}
		// B's share was untouched by all of this, and A's unbound one times out 30 s after its start.
		time.Sleep(30 * time.Second)
		synctest.Wait()
		drain(t, ca)
		evs, _ = drain(t, cb)
		if st := stopped(evs); len(st) != 2 || st[0].ShareID != sB || st[0].Reason != protocol.EndReasonMediaTimeout ||
			st[1].ShareID != s4 || st[1].Reason != protocol.EndReasonMediaTimeout {
			t.Errorf("events %+v, want media_timeout for B's share, then for A's unbound one", evs)
		}
	})
}

// Errors of a pub offer (01 §9 rules 4, 7 and 8, §12.1): the MediaPeer's errors go out with scope pc and the
// offer's pc, gen and neg, and nothing is bound; codec_not_supported has scope share and ends its share; a malformed
// tracks array is bad_request with field tracks and never reaches the MediaPeer.
func TestPCOfferErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		ca, wa := e.connect(cookieA, signaltest.DefaultHello())
		join(t, ca, "lounge")
		peer := e.media.Peer(wa.ConnectionID)
		s1 := startShare(t, ca, screen("r1")).ShareID
		quiet(t, ca)

		// The SDP can't be applied. The adapter's error has the code; the hub makes it a pc error of this offer.
		sdpInvalid := protocol.NewError(protocol.ErrorCodeSDPInvalid, protocol.ErrorScopeRequest)
		peer.Fail("HandleOffer", &sdpInvalid)
		pubOffer(t, ca, 1, 1, videoTrack("0", s1))
		if pe := expectPCError(t, ca, protocol.ErrorCodeSDPInvalid); pe.PC != pub || pe.Gen != 1 || pe.Neg != 1 || pe.Retryable {
			t.Errorf("sdp_invalid %+v, want pc pub, gen 1, neg 1", pe)
		}
		stale := protocol.NewError(protocol.ErrorCodeStaleNegotiation, protocol.ErrorScopePC)
		peer.Fail("HandleOffer", &stale)
		pubOffer(t, ca, 1, 2, videoTrack("0", s1))
		if pe := expectPCError(t, ca, protocol.ErrorCodeStaleNegotiation); pe.Gen != 1 || pe.Neg != 2 {
			t.Errorf("stale_negotiation %+v, want gen 1, neg 2", pe)
		}
		limited := protocol.NewError(protocol.ErrorCodeRateLimited, protocol.ErrorScopePC)
		limited.RetryAfterMs = 4200
		peer.Fail("HandleOffer", &limited)
		pubOffer(t, ca, 1, 3, videoTrack("0", s1))
		if pe := expectPCError(t, ca, protocol.ErrorCodeRateLimited); pe.RetryAfterMs != 4200 || !pe.Retryable {
			t.Errorf("rate_limited %+v, want retryAfterMs 4200", pe)
		}
		peer.Fail("HandleOffer", errors.New("pion: boom"))
		pubOffer(t, ca, 1, 4, videoTrack("0", s1))
		pe := expectPCError(t, ca, protocol.ErrorCodeInternal)
		if ref, _ := pe.Params["ref"].(string); pe.PC != pub || pe.Neg != 4 || len(ref) != 8 ||
			e.logs.count("level=ERROR", "ref="+ref, "handle offer", "pion: boom") != 1 {
			t.Errorf("internal %+v:\n%s", pe, e.logs)
		}
		// None of these offers was applied, so none bound the share: an offer without it leaves it alone.
		peer.Fail("HandleOffer", nil)
		pubOffer(t, ca, 1, 5)
		expectAnswer(t, ca, 1, 5)
		settle()
		if evs, _ := drain(t, ca); len(evs) != 0 {
			t.Fatalf("events %+v, want none: the share was never bound", evs)
		}
		if n := len(peer.CallsTo("EndShare")); n != 0 {
			t.Fatalf("%d EndShare calls, want none", n)
		}

		// No usable H.264 (01 §9 rule 7): codec_not_supported with scope share, then the share ends with stopped.
		codec := protocol.NewError(protocol.ErrorCodeCodecNotSupported, protocol.ErrorScopeShare)
		codec.ShareID = s1
		peer.Fail("HandleOffer", &codec)
		pubOffer(t, ca, 1, 6, videoTrack("0", s1))
		env := next(t, ca)
		ce, err := protocol.Decode[protocol.Error](env)
		if env.Type != protocol.MessageTypeError || err != nil || env.Re != "" || ce.Code != protocol.ErrorCodeCodecNotSupported ||
			ce.Scope != protocol.ErrorScopeShare || ce.ShareID != s1 || ce.PC != "" {
			t.Fatalf("got %s %s, want error codec_not_supported with scope share and the share id", env.Type, env.Data)
		}
		if ev := expectEvent(t, ca, protocol.RoomEventKindShareStopped, a.UserID); ev.ShareID != s1 ||
			ev.Reason != protocol.EndReasonStopped {
			t.Errorf("share.stopped %+v", ev)
		}
		if calls := peer.CallsTo("EndShare"); len(calls) != 1 || calls[0].Args[0] != s1 || calls[0].Args[1] != protocol.EndReasonStopped {
			t.Errorf("EndShare calls %+v", calls)
		}
		peer.Fail("HandleOffer", nil)
		quiet(t, ca)

		// Malformed tracks: bad_request, scope pc, field tracks (01 §9 rule 4).
		before := len(peer.CallsTo("HandleOffer"))
		nine := make([]protocol.TrackRef, 9)
		for i := range nine {
			nine[i] = videoTrack(string(rune('a'+i)), "s_000000000000000"+string(rune('a'+i)))
		}
		for _, tc := range []struct {
			name   string
			tracks []protocol.TrackRef
			reason string
		}{
			{"duplicate mids", []protocol.TrackRef{videoTrack("0", s1), audioTrack("0", s1)}, "duplicate"},
			{"two video m-sections of one share", []protocol.TrackRef{videoTrack("0", s1), videoTrack("1", s1)}, "duplicate"},
			{"9 entries", nine, "too_many"},
			{"bad kind", []protocol.TrackRef{{MID: "0", ShareID: s1, Kind: "data"}}, "invalid"},
			{"no mid", []protocol.TrackRef{{ShareID: s1, Kind: protocol.TrackKindVideo}}, "required"},
		} {
			pubOffer(t, ca, 1, 7, tc.tracks...)
			pe := expectPCError(t, ca, protocol.ErrorCodeBadRequest)
			if pe.PC != pub || pe.Gen != 1 || pe.Neg != 7 || pe.Params["field"] != "tracks" || pe.Params["reason"] != tc.reason {
				t.Errorf("%s: bad_request %+v, want field tracks, reason %s", tc.name, pe, tc.reason)
			}
		}
		// Messages that go the wrong way (01 §9 rule 1): the client offers on pub only, answers on sub only, and asks
		// for a restart of sub only.
		for _, m := range []struct {
			typ  protocol.MessageType
			data any
		}{
			{protocol.MessageTypePCOffer, protocol.PCOffer{PC: sub, Gen: 1, Neg: 1, SDP: offerSDP}},
			{protocol.MessageTypePCAnswer, protocol.PCAnswer{PC: pub, Gen: 1, Neg: 1, SDP: offerSDP}},
			{protocol.MessageTypePCRestart, protocol.PCRestart{PC: pub, Gen: 1, Mode: protocol.RestartModeRebuild,
				Reason: protocol.RestartReasonFailed}},
		} {
			notify(t, ca, m.typ, m.data)
			pe := expectPCError(t, ca, protocol.ErrorCodeBadRequest)
			if pe.Gen != 1 || pe.Params["field"] != "pc" || pe.Params["reason"] != "invalid" {
				t.Errorf("%s the wrong way: bad_request %+v, want field pc", m.typ, pe)
			}
		}
		ping(t, ca)
		if calls := peer.MediaCalls(); len(peer.CallsTo("HandleOffer")) != before || slices.Contains(methods(calls), "HandleAnswer") ||
			slices.Contains(methods(calls), "Restart") {
			t.Errorf("MediaPeer calls %v, want none for the malformed messages", methods(calls))
		}
	})
}

// Pub offers with a new gen, each of which makes the SFU build a PeerConnection, are limited to 6 per minute per
// connection (01 §13); renegotiations of a gen are not counted, and whether a gen is stale is the SFU's to say.
func TestPCOfferGenRate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		c, w := e.connect(cookie, signaltest.DefaultHello())
		join(t, c, "lounge")
		peer := e.media.Peer(w.ConnectionID)
		for gen := uint32(1); gen <= 6; gen++ {
			pubOffer(t, c, gen, 1)
			expectAnswer(t, c, gen, 1)
			pubOffer(t, c, gen, 2) // the same gen again: a renegotiation
			expectAnswer(t, c, gen, 2)
		}
		pubOffer(t, c, 7, 1)
		pe := expectPCError(t, c, protocol.ErrorCodeRateLimited)
		if pe.PC != pub || pe.Gen != 7 || pe.Neg != 1 || pe.RetryAfterMs != 10000 || !pe.Retryable {
			t.Errorf("rate_limited %+v, want pc pub, gen 7, neg 1, retryAfterMs 10000", pe)
		}
		// The current gen and an older one still reach the MediaPeer, which decides what is stale.
		pubOffer(t, c, 6, 3)
		expectAnswer(t, c, 6, 3)
		pubOffer(t, c, 2, 9)
		expectAnswer(t, c, 2, 9)
		if n := len(offers(peer)); n != 14 {
			t.Errorf("%d HandleOffer calls, want 14: the refused offer never reached the MediaPeer", n)
		}
		time.Sleep(10 * time.Second)
		pubOffer(t, c, 7, 1)
		expectAnswer(t, c, 7, 1)

		// A new MediaPeer (the next room.join) starts over: its first offer makes a PeerConnection, whatever the gen.
		leave(t, c)
		join(t, c, "lounge")
		pubOffer(t, c, 1, 1)
		if pe := expectPCError(t, c, protocol.ErrorCodeRateLimited); pe.Gen != 1 {
			t.Errorf("rate_limited %+v, want gen 1", pe)
		}
		time.Sleep(10 * time.Second)
		pubOffer(t, c, 1, 1)
		expectAnswer(t, c, 1, 1)
		if n := len(offers(e.media.Peer(w.ConnectionID))); n != 1 {
			t.Errorf("%d HandleOffer calls at the new MediaPeer, want 1", n)
		}
	})
}

// pc.answer, pc.ice, pc.restart and pc.close reach the connection's MediaPeer as they are (01 §8.8, §15.4), and its
// errors come back with scope pc and the message's pc, gen and neg. Outside a room they get not_in_room.
func TestPCRouting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		c, w := e.connect(cookie, signaltest.DefaultHello())
		join(t, c, "lounge")
		peer := e.media.Peer(w.ConnectionID)

		answer := protocol.PCAnswer{PC: sub, Gen: 3, Neg: 2, SDP: offerSDP}
		mid, idx, ufrag := "0", uint16(0), "a1b2"
		cand := protocol.PCICE{PC: sub, Gen: 3, Candidate: &protocol.ICECandidate{
			Candidate: "candidate:1 1 udp 2122260223 198.51.100.23 54321 typ host", SDPMid: &mid, SDPMLineIndex: &idx,
			UsernameFragment: &ufrag}}
		end := protocol.PCICE{PC: pub, Gen: 1} // end of candidates: the adapter ignores it
		restart := protocol.PCRestart{PC: sub, Gen: 3, Mode: protocol.RestartModeICE, Reason: protocol.RestartReasonDisconnected}
		rebuild := protocol.PCRestart{PC: sub, Gen: 3, Mode: protocol.RestartModeRebuild, Reason: protocol.RestartReasonFailed}
		closePub := protocol.PCClose{PC: pub, Gen: 1}
		notify(t, c, protocol.MessageTypePCAnswer, answer)
		notify(t, c, protocol.MessageTypePCICE, cand)
		notify(t, c, protocol.MessageTypePCICE, end)
		notify(t, c, protocol.MessageTypePCRestart, restart)
		notify(t, c, protocol.MessageTypePCRestart, rebuild)
		notify(t, c, protocol.MessageTypePCClose, closePub)
		notify(t, c, protocol.MessageTypePCClose, protocol.PCClose{PC: sub, Gen: 3}) // ignored (01 §8.8)
		ping(t, c)
		calls := peer.MediaCalls()
		if !slices.Equal(methods(calls), []string{"HandleAnswer", "AddICE", "AddICE", "Restart", "Restart", "ClosePC"}) {
			t.Fatalf("MediaPeer calls %v", methods(calls))
		}
		gotCand := calls[1].Args[0].(protocol.PCICE)
		if calls[0].Args[0] != answer || gotCand.PC != sub || gotCand.Gen != 3 || gotCand.Candidate == nil ||
			jsonOf(t, gotCand) != jsonOf(t, cand) || jsonOf(t, calls[2].Args[0]) != jsonOf(t, end) ||
			calls[3].Args[0] != restart || calls[4].Args[0] != rebuild || calls[5].Args[0] != closePub {
			t.Errorf("MediaPeer calls %+v, want the messages as they were sent", calls)
		}
		if e.logs.count("198.51.100.23") != 0 || e.logs.count("IN IP4") != 0 {
			t.Errorf("a log line has a candidate or an SDP:\n%s", e.logs)
		}

		// The MediaPeer's errors: scope pc with the message's pc, gen and neg.
		sdpInvalid := protocol.NewError(protocol.ErrorCodeSDPInvalid, protocol.ErrorScopePC)
		peer.Fail("HandleAnswer", &sdpInvalid)
		notify(t, c, protocol.MessageTypePCAnswer, answer)
		if pe := expectPCError(t, c, protocol.ErrorCodeSDPInvalid); pe.PC != sub || pe.Gen != 3 || pe.Neg != 2 {
			t.Errorf("sdp_invalid %+v, want pc sub, gen 3, neg 2", pe)
		}
		badPC := protocol.NewError(protocol.ErrorCodeBadRequest, protocol.ErrorScopeRequest)
		peer.Fail("AddICE", &badPC)
		notify(t, c, protocol.MessageTypePCICE, cand)
		if pe := expectPCError(t, c, protocol.ErrorCodeBadRequest); pe.PC != sub || pe.Gen != 3 || pe.Neg != 0 {
			t.Errorf("bad_request %+v, want pc sub, gen 3", pe)
		}
		limited := protocol.NewError(protocol.ErrorCodeRateLimited, protocol.ErrorScopePC)
		limited.RetryAfterMs = 1500
		peer.Fail("Restart", &limited)
		notify(t, c, protocol.MessageTypePCRestart, rebuild)
		if pe := expectPCError(t, c, protocol.ErrorCodeRateLimited); pe.PC != sub || pe.Gen != 3 || pe.RetryAfterMs != 1500 {
			t.Errorf("rate_limited %+v, want pc sub, gen 3, retryAfterMs 1500", pe)
		}
		peer.Fail("ClosePC", errors.New("pion: boom"))
		notify(t, c, protocol.MessageTypePCClose, closePub)
		pe := expectPCError(t, c, protocol.ErrorCodeInternal)
		if ref, _ := pe.Params["ref"].(string); pe.PC != pub || pe.Gen != 1 ||
			e.logs.count("level=ERROR", "ref="+ref, "close pc", "pion: boom") != 1 {
			t.Errorf("internal %+v:\n%s", pe, e.logs)
		}

		// Outside a room there is no MediaPeer: not_in_room, scope pc.
		leave(t, c)
		before := len(peer.MediaCalls())
		for typ, data := range map[protocol.MessageType]any{
			protocol.MessageTypePCOffer:   protocol.PCOffer{PC: pub, Gen: 1, Neg: 1, SDP: offerSDP},
			protocol.MessageTypePCAnswer:  answer,
			protocol.MessageTypePCICE:     cand,
			protocol.MessageTypePCRestart: restart,
			protocol.MessageTypePCClose:   closePub,
		} {
			notify(t, c, typ, data)
			if pe := expectPCError(t, c, protocol.ErrorCodeNotInRoom); pe.PC == "" || pe.Gen == 0 {
				t.Errorf("%s: not_in_room %+v, want the message's pc and gen", typ, pe)
			}
		}
		if n := len(peer.MediaCalls()); n != before {
			t.Errorf("%d MediaPeer calls after room.leave, want %d", n, before)
		}
	})
}

// subscribe.update (01 §8.9, §19): desired state per share, merged into the connection's wants; the watchers of
// room.state follow them; shares that are not in the room are ignored and listed, never an error; the MediaPeer gets
// all of a request's wants in one call, and its errors fail the request. subscribe.status and quality.hint come from
// the MediaPeer.
func TestSubscribeUpdate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, _ := e.user(false)
		cookieB, b := e.user(false)
		ca, wa := e.connect(cookieA, signaltest.DefaultHello())
		viewer := signaltest.DefaultHello()
		viewer.Role = protocol.RoleViewer
		b1, wb1 := e.connect(cookieB, viewer)
		b2, wb2 := e.connect(cookieB, signaltest.DefaultHello())
		for _, c := range []*signaltest.Client{ca, b1, b2} {
			join(t, c, "lounge")
		}
		s1 := startShare(t, ca, screen("r1")).ShareID
		time.Sleep(time.Second)
		s2 := startShare(t, ca, screen("r2")).ShareID
		goLive(e, wa.ConnectionID, s1)
		goLive(e, wa.ConnectionID, s2)
		quiet(t, ca, b1, b2)
		peer1, peer2 := e.media.Peer(wb1.ConnectionID), e.media.Peer(wb2.ConnectionID)
		const unknown = "s_0000000000000000"
		// watchers returns the watchers of both shares as A sees them once the room has settled.
		watchers := func() (w1, w2 []protocol.Watcher) {
			t.Helper()
			settle()
			_, st := drain(t, ca)
			return mustShare(t, st, s1).Watchers, mustShare(t, st, s2).Watchers
		}

		// One request, three items: the unknown share is ignored, the others go to the MediaPeer in one call.
		wants := []protocol.SubscriptionWant{want(s1, high, audioOn), want(unknown, low, audioOff), want(s2, low, audioOff)}
		if ignored := subscribe(t, b1, wants...); !slices.Equal(ignored, []string{unknown}) {
			t.Errorf("ignored %v, want the unknown share", ignored)
		}
		calls := peer1.CallsTo("Subscribe")
		if len(calls) != 1 || !slices.Equal(calls[0].Args[0].([]protocol.SubscriptionWant),
			[]protocol.SubscriptionWant{want(s1, high, audioOn), want(s2, low, audioOff)}) {
			t.Fatalf("Subscribe calls %+v, want one with the two known shares", calls)
		}
		w1, w2 := watchers()
		if !slices.Equal(w1, []protocol.Watcher{{UserID: b.UserID, Video: high, Audio: audioOn}}) ||
			!slices.Equal(w2, []protocol.Watcher{{UserID: b.UserID, Video: low, Audio: audioOff}}) {
			t.Errorf("watchers %+v and %+v", w1, w2)
		}

		// Wants are merged: the next request names one share, the other keeps its want. The user's second connection
		// merges into the same watcher (01 §4.1).
		subscribe(t, b1, want(s2, videoOff, audioOff))
		subscribe(t, b2, want(s1, low, audioOff), want(s2, videoOff, audioOn))
		w1, w2 = watchers()
		if !slices.Equal(w1, []protocol.Watcher{{UserID: b.UserID, Video: high, Audio: audioOn}}) ||
			!slices.Equal(w2, []protocol.Watcher{{UserID: b.UserID, Video: videoOff, Audio: audioOn}}) {
			t.Errorf("merged watchers %+v and %+v", w1, w2)
		}
		// {off, off} pauses a subscription: not watching.
		subscribe(t, b2, want(s2, videoOff, audioOff))
		if _, w2 = watchers(); len(w2) != 0 {
			t.Errorf("watchers %+v, want none for the paused subscription", w2)
		}
		// The same wants again change nothing in the room.
		subscribe(t, b2, want(s2, videoOff, audioOff))
		settle()
		expectOpen(t, ca)
		// The owner may subscribe to its own share (a second screen); it is never a watcher.
		subscribe(t, ca, want(s2, high, audioOn))
		if _, w2 = watchers(); len(w2) != 0 {
			t.Errorf("watchers %+v, want none: the owner doesn't count", w2)
		}

		// The MediaPeer doesn't know a share (it ended in a race): ignored as well, in the request's order.
		peer2.SetIgnored([]string{s2})
		if ignored := subscribe(t, b2, want(s2, low, audioOff), want(unknown, low, audioOff), want(s1, low, audioOff)); !slices.Equal(ignored, []string{s2, unknown}) {
			t.Errorf("ignored %v, want the MediaPeer's and the hub's", ignored)
		}
		peer2.SetIgnored(nil)
		// Only unknown shares: nothing for the MediaPeer.
		n := len(peer2.CallsTo("Subscribe"))
		if ignored := subscribe(t, b2, want(unknown, high, audioOn)); !slices.Equal(ignored, []string{unknown}) {
			t.Errorf("ignored %v", ignored)
		}
		if got := len(peer2.CallsTo("Subscribe")); got != n {
			t.Errorf("%d Subscribe calls, want %d: nothing to subscribe to", got, n)
		}

		// The MediaPeer refuses an item for another reason: the request fails with its error (01 §15.4).
		tooMany := protocol.NewError(protocol.ErrorCodeBadRequest, protocol.ErrorScopeRequest)
		tooMany.Params = map[string]any{"field": "subs", "reason": "too_many"}
		peer2.Fail("Subscribe", &tooMany)
		pe := refused(t, b2, protocol.MessageTypeSubscribeUpdate, protocol.SubscribeUpdate{
			Subs: []protocol.SubscriptionWant{want(s2, high, audioOn)}}, protocol.ErrorCodeBadRequest)
		if pe.Params["field"] != "subs" || pe.Params["reason"] != "too_many" {
			t.Errorf("bad_request %+v", pe)
		}
		peer2.Fail("Subscribe", errors.New("sfu: boom"))
		refused(t, b2, protocol.MessageTypeSubscribeUpdate, protocol.SubscribeUpdate{
			Subs: []protocol.SubscriptionWant{want(s2, high, audioOn)}}, protocol.ErrorCodeInternal)
		peer2.Fail("Subscribe", nil)
		// The want stands as the client stated it: watchers may over-count, never under-count (01 §4.1).
		if _, w2 = watchers(); !slices.Equal(w2, []protocol.Watcher{{UserID: b.UserID, Video: high, Audio: audioOn}}) {
			t.Errorf("watchers %+v after a refused request, want the stated want", w2)
		}
		// A malformed request never reaches the room.
		pe = refused(t, b2, protocol.MessageTypeSubscribeUpdate, protocol.SubscribeUpdate{
			Subs: []protocol.SubscriptionWant{want(s1, high, audioOn), want(s1, low, audioOn)}}, protocol.ErrorCodeBadRequest)
		if pe.Params["field"] != "subs[1].shareId" || pe.Params["reason"] != "duplicate" {
			t.Errorf("bad_request %+v", pe)
		}

		// What the SFU forwards differs from the request: subscribe.status; a hint for the sharer: quality.hint.
		quiet(t, ca, b1, b2)
		status := []protocol.SubscriptionStatus{{ShareID: s1, Video: low, Audio: audioOn, RequestedVideo: high,
			Reason: protocol.StatusReasonBandwidth}}
		peer1.Sink().SubscriptionStatus(status)
		got, err := protocol.Decode[protocol.SubscribeStatus](expectType(t, b1, protocol.MessageTypeSubscribeStatus))
		if err != nil || !slices.Equal(got.Subs, status) {
			t.Errorf("subscribe.status %+v (%v), want %+v", got, err, status)
		}
		hint := protocol.QualityHint{ShareID: s1, Reason: protocol.HintReasonCodec, Codec: protocol.CodecH264ConstrainedBaseline}
		e.media.Peer(wa.ConnectionID).Sink().QualityHint(hint)
		gotHint, err := protocol.Decode[protocol.QualityHint](expectType(t, ca, protocol.MessageTypeQualityHint))
		if err != nil || jsonOf(t, gotHint) != jsonOf(t, hint) {
			t.Errorf("quality.hint %s (%v), want %s", jsonOf(t, gotHint), err, jsonOf(t, hint))
		}

		// A connection's wants go when it leaves the room.
		leave(t, b2)
		okReply(t, b1, request(t, b1, protocol.MessageTypeRoomLeave, protocol.Empty{}))
		join(t, b1, "lounge")
		if w1, w2 = watchers(); len(w1) != 0 || len(w2) != 0 {
			t.Errorf("watchers %+v and %+v after B left and came back, want none", w1, w2)
		}
	})
}

// caps.update (01 §8.10, §11.7): the connection's MediaPeer gets the new capabilities at once, and the next
// MediaPeer is created with them. At most 12 per minute; more are dropped silently.
func TestCapsUpdate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		h := signaltest.DefaultHello()
		h.Caps = protocol.Caps{Decode: []protocol.CodecKey{protocol.CodecOpus}}
		c, w := e.connect(cookie, h)

		// Outside a room the connection keeps them for its MediaPeer.
		first := protocol.Caps{Decode: []protocol.CodecKey{protocol.CodecH264Baseline, protocol.CodecOpus}}
		notify(t, c, protocol.MessageTypeCapsUpdate, protocol.CapsUpdate{Caps: first})
		join(t, c, "lounge")
		peer := e.media.Peer(w.ConnectionID)
		if !slices.Equal(peer.Params.Caps.Decode, first.Decode) {
			t.Errorf("the MediaPeer was created with caps %+v, want %+v", peer.Params.Caps, first)
		}
		if n := len(peer.CallsTo("SetCaps")); n != 0 {
			t.Errorf("%d SetCaps calls, want none yet", n)
		}
		// In a room the MediaPeer gets them: the SFU updates the room's codec safe set.
		second := protocol.Caps{Decode: []protocol.CodecKey{protocol.CodecH264ConstrainedBaseline, protocol.CodecH264Baseline,
			protocol.CodecOpus}, Simulcast: true}
		notify(t, c, protocol.MessageTypeCapsUpdate, protocol.CapsUpdate{Caps: second})
		ping(t, c)
		calls := peer.CallsTo("SetCaps")
		if len(calls) != 1 || jsonOf(t, calls[0].Args[0]) != jsonOf(t, second) {
			t.Fatalf("SetCaps calls %+v, want one with %+v", calls, second)
		}
		// 12 per minute (two are spent): the 13th is dropped, without an error.
		for range 11 {
			notify(t, c, protocol.MessageTypeCapsUpdate, protocol.CapsUpdate{Caps: second})
		}
		ping(t, c)
		if n := len(peer.CallsTo("SetCaps")); n != 11 {
			t.Errorf("%d SetCaps calls, want 11: the 13th caps.update of the minute is dropped", n)
		}
		// The next MediaPeer starts with the latest caps.
		leave(t, c)
		time.Sleep(time.Minute)
		notify(t, c, protocol.MessageTypeCapsUpdate, protocol.CapsUpdate{Caps: first})
		join(t, c, "lounge")
		if p := e.media.Peer(w.ConnectionID); p == peer || !slices.Equal(p.Params.Caps.Decode, first.Decode) || p.Params.Caps.Simulcast {
			t.Errorf("the new MediaPeer has caps %+v, want %+v", p.Params.Caps, first)
		}
	})
}

// The MediaPeer's sink (01 §15.2) with a real share: events of the connection's MediaPeer reach its client while it
// is attached, and the share's state follows the media facts also while it is detached.
func TestShareMediaWhileDetached(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		cookieB, _ := e.user(false)
		ca, wa := e.connect(cookieA, signaltest.DefaultHello())
		cb, _ := e.connect(cookieB, signaltest.DefaultHello())
		join(t, ca, "lounge")
		join(t, cb, "lounge")
		p := startShare(t, ca, screen("r1"))
		pubOffer(t, ca, 1, 1, videoTrack("0", p.ShareID))
		expectAnswer(t, ca, 1, 1)
		quiet(t, ca, cb)
		peer := e.media.Peer(wa.ConnectionID)

		// The socket drops before the first keyframe; the keyframe arrives during the grace.
		ca.Close()
		settle()
		drain(t, cb)
		goLive(e, wa.ConnectionID, p.ShareID)
		if ev := expectEvent(t, cb, protocol.RoomEventKindShareStarted, a.UserID); ev.ShareID != p.ShareID {
			t.Errorf("share.started %+v", ev)
		}
		settle()
		if _, st := drain(t, cb); mustShare(t, st, p.ShareID).Status != protocol.ShareStatusLive {
			t.Errorf("state %+v, want the share live", st)
		}
		if n := len(e.push.Events()); n != 1 {
			t.Errorf("%d push events, want 1", n)
		}
		// The client resumes: the share is as the room knows it, and the pub offer it sends again changes nothing.
		c2, w2 := e.resume(cookieA, wa.ResumeToken)
		if !w2.Resumed {
			t.Fatal("not resumed")
		}
		st := expectState(t, c2, "lounge")
		if s := mustShare(t, &st, p.ShareID); s.Status != protocol.ShareStatusLive || s.ConnectionID != wa.ConnectionID {
			t.Errorf("share after the resume %+v", s)
		}
		pubOffer(t, c2, 1, 1, videoTrack("0", p.ShareID))
		expectAnswer(t, c2, 1, 1)
		settle()
		if evs, _ := drain(t, cb); len(evs) != 0 {
			t.Errorf("events after the resume %+v, want none", evs)
		}
		if got := methods(peer.MediaCalls()); !slices.Equal(got, []string{"CreateShare", "HandleOffer", "Resync", "HandleOffer"}) {
			t.Errorf("MediaPeer calls %v", got)
		}
	})
}
