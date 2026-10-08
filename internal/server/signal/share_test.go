package signal_test

import (
	"bytes"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/signal"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// Shares (01 §4.4, §8.7, §19 "Shares") against the fake MediaPlane: share.start with its limits, ref and replaces,
// the lifecycle starting → live → stalled → live with both 30 s timeouts, share.update, share.stop and the Web Push
// call. The tests run the real 30 s on synctest's fake clock.

var shareIDRE = regexp.MustCompile(`^s_[0-9a-hjkmnp-tv-z]{16}$`)

const (
	high     = protocol.VideoLayerHigh
	low      = protocol.VideoLayerLow
	videoOff = protocol.VideoLayerOff
	audioOn  = protocol.AudioStateOn
	audioOff = protocol.AudioStateOff
)

// screen is a share.start of a whole screen with the auto preset and no audio.
func screen(ref string) protocol.ShareStart {
	return protocol.ShareStart{Kind: protocol.ShareKindScreen, Preset: protocol.PresetAuto, Ref: ref}
}

// replyTo reads the reply (ok or error) to request id. The room traffic queued before it (room.state and room.event
// of what the test did earlier) is skipped; anything else fails the test.
func replyTo(t *testing.T, c *signaltest.Client, id string) protocol.Envelope {
	t.Helper()
	for {
		env := recv(t, c)
		switch env.Type {
		case protocol.MessageTypeRoomState, protocol.MessageTypeRoomEvent:
			continue
		case protocol.MessageTypeOK, protocol.MessageTypeError:
			if env.Re == id {
				return env
			}
		}
		t.Fatalf("got %s (re %q) %s, want the reply to request %s", env.Type, env.Re, env.Data, id)
	}
}

// okReply reads the ok that answers request id, skipping queued room traffic.
func okReply(t *testing.T, c *signaltest.Client, id string) protocol.Envelope {
	t.Helper()
	env := replyTo(t, c, id)
	if env.Type != protocol.MessageTypeOK {
		t.Fatalf("request %s got %s %s, want ok", id, env.Type, env.Data)
	}
	return env
}

// refused sends a request and reads its reply, which must be error{code, scope request}, skipping queued room
// traffic.
func refused(t *testing.T, c *signaltest.Client, typ protocol.MessageType, data any, code protocol.ErrorCode) protocol.Error {
	t.Helper()
	id := request(t, c, typ, data)
	env := replyTo(t, c, id)
	pe, err := protocol.Decode[protocol.Error](env)
	if err != nil || env.Type != protocol.MessageTypeError || pe.Code != code || pe.Scope != protocol.ErrorScopeRequest {
		t.Fatalf("%s got %s %s (%v), want error %s with scope request", typ, env.Type, env.Data, err, code)
	}
	return pe
}

// startShare sends share.start, reads its ok and returns the ShareParams.
func startShare(t *testing.T, c *signaltest.Client, v protocol.ShareStart) protocol.ShareParams {
	t.Helper()
	env := okReply(t, c, request(t, c, protocol.MessageTypeShareStart, v))
	p, err := protocol.Decode[protocol.ShareParams](env)
	if err != nil || !shareIDRE.MatchString(p.ShareID) {
		t.Fatalf("share.start reply %s, %v", env.Data, err)
	}
	return p
}

// stopShare sends share.stop and reads its ok.
func stopShare(t *testing.T, c *signaltest.Client, shareID string) {
	t.Helper()
	if env := okReply(t, c, request(t, c, protocol.MessageTypeShareStop, protocol.ShareStop{ShareID: shareID})); env.Data != nil {
		t.Fatalf("share.stop reply %s, want no data", env.Data)
	}
}

// subscribe sends subscribe.update, reads its ok and returns the ignored share ids.
func subscribe(t *testing.T, c *signaltest.Client, wants ...protocol.SubscriptionWant) []string {
	t.Helper()
	env := okReply(t, c, request(t, c, protocol.MessageTypeSubscribeUpdate, protocol.SubscribeUpdate{Subs: wants}))
	res, err := protocol.Decode[protocol.SubscribeResult](env)
	if err != nil || res.Ignored == nil {
		t.Fatalf("subscribe.update reply %s, %v; want ignored, never null", env.Data, err)
	}
	return res.Ignored
}

// want is a desired subscription.
func want(shareID string, video protocol.VideoLayer, audio protocol.AudioState) protocol.SubscriptionWant {
	return protocol.SubscriptionWant{ShareID: shareID, Video: video, Audio: audio}
}

// quiet lets the room settle and discards what the clients have queued.
func quiet(t *testing.T, cs ...*signaltest.Client) {
	t.Helper()
	settle()
	for _, c := range cs {
		for c.Pending() > 0 {
			recv(t, c)
		}
	}
}

// shareIn returns share id of st, or nil.
func shareIn(st *protocol.RoomState, id string) *protocol.ShareInfo {
	if st == nil {
		return nil
	}
	for i := range st.Shares {
		if st.Shares[i].ID == id {
			return &st.Shares[i]
		}
	}
	return nil
}

// mustShare returns share id of st and fails the test when st doesn't have it.
func mustShare(t *testing.T, st *protocol.RoomState, id string) protocol.ShareInfo {
	t.Helper()
	s := shareIn(st, id)
	if s == nil {
		t.Fatalf("room.state %+v has no share %s", st, id)
	}
	return *s
}

// liveEvent is the media fact of a first keyframe: both layers, High profile, no audio.
func liveEvent() signal.ShareMediaEvent {
	return signal.ShareMediaEvent{Kind: signal.ShareMediaLive, Layers: []protocol.VideoLayer{high, low},
		Codec: protocol.CodecH264High}
}

// goLive reports the first keyframe of a share at its publishing connection's MediaPeer.
func goLive(e *env, connID, shareID string) {
	e.media.Peer(connID).Sink().ShareMedia(shareID, liveEvent())
	synctest.Wait()
}

// stopped returns the share.stopped events among evs.
func stopped(evs []protocol.RoomEvent) []protocol.RoomEvent {
	var out []protocol.RoomEvent
	for _, ev := range evs {
		if ev.Kind == protocol.RoomEventKindShareStopped {
			out = append(out, ev)
		}
	}
	return out
}

// share.start (01 §8.7): the ok carries the MediaPeer's ShareParams and comes before any room.state with the new
// share; the share is starting, visible to the room, with no event yet; the errors of the MediaPeer and of the
// payload.
func TestShareStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		cookieB, _ := e.user(false)
		ca, wa := e.connect(cookieA, signaltest.DefaultHello())
		cb, _ := e.connect(cookieB, signaltest.DefaultHello())
		join(t, ca, "lounge")
		join(t, cb, "lounge")
		quiet(t, ca, cb)
		peer := e.media.Peer(wa.ConnectionID)

		t0 := time.Now()
		meta := protocol.ShareStart{Kind: protocol.ShareKindWindow, Label: "  Movie night ", Preset: protocol.PresetMovie,
			Audio: true, Ref: "r1"}
		params := startShare(t, ca, meta)
		if want := signaltest.ShareParamsFor(params.ShareID); jsonOf(t, params) != jsonOf(t, want) {
			t.Errorf("ShareParams %s, want the MediaPeer's %s", jsonOf(t, params), jsonOf(t, want))
		}
		// The ok came first (01 §7); the room.state with the share follows, for everyone, and no room.event.
		info := protocol.ShareInfo{ID: params.ShareID, UserID: a.UserID, ConnectionID: wa.ConnectionID,
			Kind: protocol.ShareKindWindow, Label: "Movie night", Preset: protocol.PresetMovie, Audio: true,
			Status: protocol.ShareStatusStarting, StartedAt: t0}
		for _, c := range []*signaltest.Client{ca, cb} {
			f, err := c.RecvFrame(ctxT(t))
			if err != nil || f.Envelope.Type != protocol.MessageTypeRoomState {
				t.Fatalf("got %s, %v; want room.state", f.Envelope.Type, err)
			}
			if !bytes.Contains(f.Raw, []byte(`"layers":[]`)) || !bytes.Contains(f.Raw, []byte(`"watchers":[]`)) ||
				bytes.Contains(f.Raw, []byte(`"codec"`)) || bytes.Contains(f.Raw, []byte(`"replaces"`)) {
				t.Errorf("room.state %s: want empty layers and watchers, no codec, no replaces", f.Raw)
			}
			st, err := protocol.Decode[protocol.RoomState](f.Envelope)
			if err != nil || len(st.Shares) != 1 || jsonOf(t, st.Shares[0]) != jsonOf(t, info) {
				t.Errorf("shares %s (%v), want %s", jsonOf(t, st.Shares), err, jsonOf(t, info))
			}
		}
		settle()
		expectOpen(t, ca)
		expectOpen(t, cb)
		trimmed := meta
		trimmed.Label = "Movie night"
		calls := peer.MediaCalls()
		if len(calls) != 1 || calls[0].Method != "CreateShare" || calls[0].Args[0] != params.ShareID ||
			calls[0].Args[1] != trimmed {
			t.Errorf("MediaPeer calls %+v, want CreateShare(%s, %+v)", calls, params.ShareID, trimmed)
		}
		if got := e.metric("isshoni_shares", "status", "starting"); got != 1 {
			t.Errorf("isshoni_shares{starting} %v, want 1", got)
		}
		if e.logs.count("level=INFO", "share started", "share_id="+params.ShareID, "conn_id="+wa.ConnectionID,
			"user_id="+a.UserID, "room_id=lounge", "kind=window") != 1 {
			t.Errorf("no share started line:\n%s", e.logs)
		}
		if e.logs.count("Movie night") != 0 {
			t.Errorf("a log line has the share's label:\n%s", e.logs)
		}

		// The MediaPeer refuses: its mapped error is the reply (01 §15.4); anything else is internal, with a ref.
		tooMany := protocol.NewError(protocol.ErrorCodeShareLimit, protocol.ErrorScopeRequest)
		tooMany.Params = map[string]any{"limit": 4, "per": "user"}
		peer.Fail("CreateShare", &tooMany)
		id := request(t, ca, protocol.MessageTypeShareStart, screen("r2"))
		pe, re := expectError(t, ca, protocol.ErrorCodeShareLimit, protocol.ErrorScopeRequest)
		if re != id || pe.Params["limit"] != float64(4) || pe.Params["per"] != "user" || pe.Retryable {
			t.Errorf("share_limit %+v re %q", pe, re)
		}
		gone := protocol.NewError(protocol.ErrorCodeNotInRoom, protocol.ErrorScopePC) // the scope is the request's
		peer.Fail("CreateShare", &gone)
		id = request(t, ca, protocol.MessageTypeShareStart, screen("r2"))
		if _, re := expectError(t, ca, protocol.ErrorCodeNotInRoom, protocol.ErrorScopeRequest); re != id {
			t.Errorf("re %q, want %q", re, id)
		}
		peer.Fail("CreateShare", errors.New("sfu: boom"))
		id = request(t, ca, protocol.MessageTypeShareStart, screen("r2"))
		pe, re = expectError(t, ca, protocol.ErrorCodeInternal, protocol.ErrorScopeRequest)
		ref, _ := pe.Params["ref"].(string)
		if re != id || len(ref) != 8 || e.logs.count("level=ERROR", "ref="+ref, "create share", "sfu: boom") != 1 {
			t.Errorf("internal %+v re %q:\n%s", pe, re, e.logs)
		}
		peer.Fail("CreateShare", nil)

		// The payload's rules (01 §5): the label, the kind, the ref.
		for _, tc := range []struct {
			name          string
			mut           func(*protocol.ShareStart)
			field, reason string
		}{
			{"control character", func(s *protocol.ShareStart) { s.Label = "a\u0007b" }, "label", "invalid"},
			{"41 code points", func(s *protocol.ShareStart) { s.Label = strings.Repeat("é", 41) }, "label", "too_long"},
			{"no ref", func(s *protocol.ShareStart) { s.Ref = "" }, "ref", "required"},
			{"unknown preset", func(s *protocol.ShareStart) { s.Preset = "cinema" }, "preset", "invalid"},
		} {
			v := screen("r3")
			tc.mut(&v)
			id := request(t, ca, protocol.MessageTypeShareStart, v)
			pe, re := expectError(t, ca, protocol.ErrorCodeBadRequest, protocol.ErrorScopeRequest)
			if re != id || pe.Params["field"] != tc.field || pe.Params["reason"] != tc.reason {
				t.Errorf("%s: re %q, params %v", tc.name, re, pe.Params)
			}
		}
		settle()
		expectOpen(t, ca)
		expectOpen(t, cb) // nothing of this reached the room
		if n := len(peer.CallsTo("CreateShare")); n != 4 {
			t.Errorf("%d CreateShare calls, want 4 (bad payloads never reach the MediaPeer)", n)
		}
		if snap := e.hub.Snapshot(); len(snap.Rooms[0].Shares) != 1 {
			t.Errorf("snapshot shares %+v, want the one share", snap.Rooms[0].Shares)
		}
	})
}

// The share limits (01 §8.7, §19): 4 shares per user across all rooms, whatever their status, and the room's soft
// limit from Policy, which applies to the next share.start; a share that ends frees its slot.
func TestShareLimits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		e.rooms.Add(room1)
		cookieA, _ := e.user(false)
		cookieB, _ := e.user(false)
		a1, wa1 := e.connect(cookieA, signaltest.DefaultHello())
		a2, wa2 := e.connect(cookieA, signaltest.DefaultHello())
		cb, wb := e.connect(cookieB, signaltest.DefaultHello())
		if wa1.Limits.MaxSharesPerUser != 4 {
			t.Fatalf("welcome.limits.maxSharesPerUser %d, want 4", wa1.Limits.MaxSharesPerUser)
		}
		join(t, a1, "lounge")
		join(t, a2, room1.ID)
		join(t, cb, room1.ID)
		quiet(t, a1, a2, cb)

		// Two shares of A in each room, in three different states.
		s1 := startShare(t, a1, screen("r1"))
		startShare(t, a1, screen("r2"))
		s3 := startShare(t, a2, screen("r1"))
		startShare(t, a2, screen("r2"))
		goLive(e, wa1.ConnectionID, s1.ShareID)
		goLive(e, wa2.ConnectionID, s3.ShareID)
		e.media.Peer(wa2.ConnectionID).Sink().ShareMedia(s3.ShareID, signal.ShareMediaEvent{Kind: signal.ShareMediaStalled})
		quiet(t, a1, a2, cb)
		for name, c := range map[string]*signaltest.Client{"lounge": a1, "room1": a2} {
			pe := refused(t, c, protocol.MessageTypeShareStart, screen("r3"), protocol.ErrorCodeShareLimit)
			if pe.Params["limit"] != float64(4) || pe.Params["per"] != "user" || pe.Retryable {
				t.Errorf("%s: share_limit %+v, want limit 4 per user", name, pe)
			}
		}
		for _, w := range []protocol.Welcome{wa1, wa2} {
			if n := len(e.media.Peer(w.ConnectionID).CallsTo("CreateShare")); n != 2 {
				t.Errorf("%d CreateShare calls, want 2: a refused share never reaches the MediaPeer", n)
			}
		}

		// The room's soft limit: room1 has A's two shares.
		e.policy.Set(signal.Policy{MaxRoomShares: 2})
		pe := refused(t, cb, protocol.MessageTypeShareStart, screen("r1"), protocol.ErrorCodeShareLimit)
		if pe.Params["limit"] != float64(2) || pe.Params["per"] != "room" {
			t.Errorf("share_limit %+v, want limit 2 per room", pe)
		}
		if n := len(e.media.Peer(wb.ConnectionID).CallsTo("CreateShare")); n != 0 {
			t.Errorf("%d CreateShare calls for B, want none", n)
		}
		e.policy.Set(signal.Policy{MaxRoomShares: 3}) // a raised limit applies to the next share.start
		startShare(t, cb, screen("r1"))
		pe = refused(t, cb, protocol.MessageTypeShareStart, screen("r2"), protocol.ErrorCodeShareLimit)
		if pe.Params["limit"] != float64(3) || pe.Params["per"] != "room" {
			t.Errorf("share_limit %+v, want limit 3 per room", pe)
		}

		// A share that ends frees its slot: A's user limit, in the other room.
		stopShare(t, a1, s1.ShareID)
		startShare(t, a1, screen("r3"))
		if pe := refused(t, a1, protocol.MessageTypeShareStart, screen("r4"), protocol.ErrorCodeShareLimit); pe.Params["per"] != "user" {
			t.Errorf("share_limit %+v, want per user", pe)
		}
		synctest.Wait()
		if s, l, st := e.metric("isshoni_shares", "status", "starting"), e.metric("isshoni_shares", "status", "live"),
			e.metric("isshoni_shares", "status", "stalled"); s != 4 || l != 0 || st != 1 {
			t.Errorf("isshoni_shares starting %v, live %v, stalled %v; want 4, 0, 1", s, l, st)
		}
	})
}

// Two share.starts that race for the last slot (01 §8.7): the limits are checked again, in one step with adding the
// share, once the MediaPeer has created it; the loser is ended at its MediaPeer and gets share_limit.
func TestShareLimitRace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		ca, wa := e.connect(cookieA, signaltest.DefaultHello())
		join(t, ca, "lounge")
		for _, ref := range []string{"r1", "r2", "r3"} {
			startShare(t, ca, screen(ref))
		}
		quiet(t, ca)
		peer := e.media.Peer(wa.ConnectionID)
		// While the MediaPeer creates the fourth share, another share of A takes the last slot.
		raced := liveShare("s_raceraceracerace", a.UserID, wa.ConnectionID)
		peer.OnCall("CreateShare", func() { signal.AddShare(e.hub, "lounge", raced) })
		id := request(t, ca, protocol.MessageTypeShareStart, screen("r4"))
		pe, re := expectError(t, ca, protocol.ErrorCodeShareLimit, protocol.ErrorScopeRequest)
		if re != id || pe.Params["per"] != "user" {
			t.Errorf("share_limit %+v re %q", pe, re)
		}
		peer.OnCall("CreateShare", nil)
		calls := peer.MediaCalls()
		n := len(calls)
		if n != 5 || calls[n-2].Method != "CreateShare" || calls[n-1].Method != "EndShare" ||
			calls[n-1].Args[0] != calls[n-2].Args[0] || calls[n-1].Args[1] != protocol.EndReasonStopped {
			t.Errorf("MediaPeer calls %+v, want the fourth CreateShare undone by EndShare(stopped)", calls)
		}
		settle()
		_, st := drain(t, ca)
		if st == nil || len(st.Shares) != 4 || shareIn(st, raced.ID) == nil {
			t.Errorf("state %+v, want the three shares and the one that won the race", st)
		}
	})
}

// A share.start that meets CloseRoom (01 §15.2): the room takes no more shares once it is closed, whether the close
// comes before the request is handled or while the MediaPeer creates the share. The request gets not_in_room, the
// MediaPeer is left without the share, and the connection then learns room_closed.
func TestShareStartInClosedRoom(t *testing.T) {
	for _, during := range []bool{false, true} {
		name := "before the request is handled"
		if during {
			name = "while the MediaPeer creates the share"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t)
				defer e.close()
				e.rooms.Add(room1)
				cookie, _ := e.user(false)
				c, w := e.connect(cookie, signaltest.DefaultHello())
				join(t, c, room1.ID)
				peer := e.media.Peer(w.ConnectionID)

				var id string
				if during {
					peer.OnCall("CreateShare", func() { e.hub.CloseRoom(room1.ID) })
					id = request(t, c, protocol.MessageTypeShareStart, screen("r1"))
				} else {
					release := signal.Stall(e.hub, w.ConnectionID) // the actor is busy
					id = request(t, c, protocol.MessageTypeShareStart, screen("r1"))
					synctest.Wait()
					e.hub.CloseRoom(room1.ID) // its part for this connection waits behind the share.start
					release()
				}
				if _, re := expectError(t, c, protocol.ErrorCodeNotInRoom, protocol.ErrorScopeRequest); re != id {
					t.Errorf("re %q, want %q", re, id)
				}
				if pe, _ := expectError(t, c, protocol.ErrorCodeRoomClosed, protocol.ErrorScopeRoom); pe.RoomID != room1.ID {
					t.Errorf("room_closed %+v", pe)
				}
				settle()
				expectOpen(t, c)
				want := []string{"Close"}
				if during {
					want = []string{"CreateShare", "EndShare", "Close"}
				}
				if got := methods(peer.MediaCalls()); !slices.Equal(got, want) {
					t.Errorf("MediaPeer calls %v, want %v", got, want)
				}
				if got := e.metric("isshoni_shares", "status", "starting"); got > 0 {
					t.Errorf("isshoni_shares{starting} %v, want none", got)
				}
			})
		})
	}
}

// The ref of share.start is an idempotency key per connection (01 §8.7, §19): the same ref returns the same share,
// so a reply lost in a reconnect can be retried, also at the limit and after a resume.
func TestShareRef(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, _ := e.user(false)
		a1, wa1 := e.connect(cookieA, signaltest.DefaultHello())
		a2, _ := e.connect(cookieA, signaltest.DefaultHello())
		join(t, a1, "lounge")
		join(t, a2, "lounge")
		quiet(t, a1, a2)
		peer := e.media.Peer(wa1.ConnectionID)

		p1 := startShare(t, a1, screen("r1"))
		quiet(t, a1, a2)
		again := protocol.ShareStart{Kind: protocol.ShareKindTab, Preset: protocol.PresetGame, Audio: true, Ref: "r1"}
		if p := startShare(t, a1, again); jsonOf(t, p) != jsonOf(t, p1) {
			t.Errorf("the same ref returned %s, want the first share's %s", jsonOf(t, p), jsonOf(t, p1))
		}
		settle()
		expectOpen(t, a1) // nothing changed: no room.state
		if n := len(peer.CallsTo("CreateShare")); n != 1 {
			t.Errorf("%d CreateShare calls, want 1", n)
		}
		// The ref is per connection: the same one on another connection of the user is another share.
		if p := startShare(t, a2, screen("r1")); p.ShareID == p1.ShareID {
			t.Error("another connection's share.start with the same ref returned the first connection's share")
		}

		// At the user's limit a retry still gets its share, and after a resume too.
		startShare(t, a1, screen("r2"))
		startShare(t, a2, screen("r2"))
		quiet(t, a1, a2)
		request(t, a1, protocol.MessageTypeShareStart, screen("r5"))
		expectError(t, a1, protocol.ErrorCodeShareLimit, protocol.ErrorScopeRequest)
		a1.Close()
		settle()
		c2, w2 := e.resume(cookieA, wa1.ResumeToken)
		if !w2.Resumed {
			t.Fatal("not resumed")
		}
		expectState(t, c2, "lounge")
		if p := startShare(t, c2, screen("r1")); p.ShareID != p1.ShareID {
			t.Errorf("after the resume the ref returned %s, want %s", p.ShareID, p1.ShareID)
		}

		// Once the share has ended, its ref starts a new one.
		stopShare(t, c2, p1.ShareID)
		quiet(t, c2, a2)
		if p := startShare(t, c2, screen("r1")); p.ShareID == p1.ShareID {
			t.Error("the ref of an ended share returned the ended share")
		}
		if n := len(peer.CallsTo("CreateShare")); n != 3 {
			t.Errorf("%d CreateShare calls, want 3", n)
		}
	})
}

// starting → live → stalled → live (01 §4.4, §8.6, §19): the first keyframe makes the share live, with one
// room.event share.started for everyone in the room, the owner's connections included, and one Web Push call with
// the room's current name; stalled and live again change room.state only; the media facts (layers, codec, audio)
// reach room.state; nothing is broadcast when nothing changed.
func TestShareLifecycle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		cookieB, b := e.user(false)
		a1, wa1 := e.connect(cookieA, signaltest.DefaultHello())
		a2, _ := e.connect(cookieA, signaltest.DefaultHello())
		cb, _ := e.connect(cookieB, signaltest.DefaultHello())
		join(t, a1, "lounge")
		join(t, a2, "lounge")
		time.Sleep(time.Second)
		join(t, cb, "lounge")
		quiet(t, a1, a2, cb)
		all := []*signaltest.Client{a1, a2, cb}

		v := screen("r1")
		v.Audio = true
		p := startShare(t, a1, v)
		quiet(t, all...)
		sink := e.media.Peer(wa1.ConnectionID).Sink()
		e.rooms.Add(protocol.RoomInfo{ID: "lounge", Name: "Den"}) // renamed since the joins: Push gets the name of now
		time.Sleep(10 * time.Second)

		// The first keyframe.
		t1 := time.Now()
		layers := []protocol.VideoLayer{high, low}
		sink.ShareMedia(p.ShareID, signal.ShareMediaEvent{Kind: signal.ShareMediaLive, Layers: layers,
			Codec: protocol.CodecH264High})
		layers[0] = protocol.VideoLayerOff // the SFU may reuse its slice once the call returns
		for _, c := range all {
			ev := expectEvent(t, c, protocol.RoomEventKindShareStarted, a.UserID)
			if ev.ShareID != p.ShareID || ev.RoomID != "lounge" || ev.Name != a.Name || !ev.At.Equal(t1) ||
				ev.Replaces != "" || ev.Reason != "" {
				t.Errorf("share.started %+v", ev)
			}
			st := expectState(t, c, "lounge")
			if s := mustShare(t, &st, p.ShareID); s.Status != protocol.ShareStatusLive ||
				!slices.Equal(s.Layers, []protocol.VideoLayer{high, low}) || s.Codec != protocol.CodecH264High || s.Audio {
				t.Errorf("share after the first keyframe %+v, want live, both layers, High, no audio track", s)
			}
		}
		synctest.Wait()
		pushed := e.push.Events()
		if len(pushed) != 1 {
			t.Fatalf("%d push events, want 1", len(pushed))
		}
		if ev := pushed[0]; ev.RoomID != "lounge" || ev.RoomName != "Den" || ev.ShareID != p.ShareID ||
			ev.UserID != a.UserID || ev.UserName != a.Name || !ev.At.Equal(t1) ||
			!slices.Equal(ev.PresentUserIDs, []string{a.UserID, b.UserID}) {
			t.Errorf("push event %+v", ev)
		}
		if s, l := e.metric("isshoni_shares", "status", "starting"), e.metric("isshoni_shares", "status", "live"); s != 0 || l != 1 {
			t.Errorf("isshoni_shares starting %v, live %v; want 0, 1", s, l)
		}

		// state returns the share as every client sees it after the room has settled, and checks that no event came.
		state := func(what string) protocol.ShareInfo {
			t.Helper()
			settle()
			var s protocol.ShareInfo
			for _, c := range all {
				evs, st := drain(t, c)
				if len(evs) != 0 {
					t.Errorf("%s: events %+v, want none", what, evs)
				}
				s = mustShare(t, st, p.ShareID)
			}
			return s
		}
		// The pub PC is not connected: stalled. The tracks are gone; the codec stays the last one known.
		sink.ShareMedia(p.ShareID, signal.ShareMediaEvent{Kind: signal.ShareMediaStalled})
		if s := state("stalled"); s.Status != protocol.ShareStatusStalled || len(s.Layers) != 0 ||
			s.Codec != protocol.CodecH264High || s.Audio {
			t.Errorf("stalled share %+v", s)
		}
		if st := e.metric("isshoni_shares", "status", "stalled"); st != 1 {
			t.Errorf("isshoni_shares{stalled} %v, want 1", st)
		}
		// A keyframe again: live, without a second share.started or push.
		sink.ShareMedia(p.ShareID, signal.ShareMediaEvent{Kind: signal.ShareMediaLive,
			Layers: []protocol.VideoLayer{high}, Codec: protocol.CodecH264ConstrainedBaseline, Audio: true})
		if s := state("live again"); s.Status != protocol.ShareStatusLive ||
			!slices.Equal(s.Layers, []protocol.VideoLayer{high}) || s.Codec != protocol.CodecH264ConstrainedBaseline || !s.Audio {
			t.Errorf("share live again %+v", s)
		}
		// The layers changed while live.
		changed := signal.ShareMediaEvent{Kind: signal.ShareMediaChanged, Layers: []protocol.VideoLayer{high, low}, Audio: true}
		sink.ShareMedia(p.ShareID, changed)
		if s := state("changed"); s.Status != protocol.ShareStatusLive ||
			!slices.Equal(s.Layers, []protocol.VideoLayer{high, low}) || s.Codec != protocol.CodecH264ConstrainedBaseline {
			t.Errorf("changed share %+v", s)
		}
		// Facts that change nothing are not broadcast.
		sink.ShareMedia(p.ShareID, changed)
		sink.ShareMedia(p.ShareID, signal.ShareMediaEvent{Kind: signal.ShareMediaLive, Layers: []protocol.VideoLayer{high, low},
			Codec: protocol.CodecH264ConstrainedBaseline, Audio: true})
		sink.ShareMedia(p.ShareID, signal.ShareMediaEvent{Kind: 99})
		// A live share has no timeout.
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		for _, c := range all {
			expectOpen(t, c)
		}
		if n := len(e.push.Events()); n != 1 {
			t.Errorf("%d push events, want still 1", n)
		}
		if s, l, st := e.metric("isshoni_shares", "status", "starting"), e.metric("isshoni_shares", "status", "live"),
			e.metric("isshoni_shares", "status", "stalled"); s != 0 || l != 1 || st != 0 {
			t.Errorf("isshoni_shares starting %v, live %v, stalled %v; want 0, 1, 0", s, l, st)
		}
	})
}

// The starting timeout (01 §4.4, §19): without a first keyframe the share ends with media_timeout exactly
// ShareStartTimeout (30 s) after share.start, in the room and at the MediaPeer; other media facts don't start it;
// a share that went live in time stays.
func TestShareStartTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		cookieB, _ := e.user(false)
		ca, wa := e.connect(cookieA, signaltest.DefaultHello())
		cb, _ := e.connect(cookieB, signaltest.DefaultHello())
		join(t, ca, "lounge")
		join(t, cb, "lounge")
		quiet(t, ca, cb)
		peer := e.media.Peer(wa.ConnectionID)

		t0 := time.Now()
		p1 := startShare(t, ca, screen("r1"))
		time.Sleep(10 * time.Second)
		p2 := startShare(t, ca, screen("r2"))
		// Not connected, layers changed: neither is a first keyframe. They change nothing about a starting share, and
		// they don't move its deadline.
		peer.Sink().ShareMedia(p1.ShareID, signal.ShareMediaEvent{Kind: signal.ShareMediaStalled,
			Layers: []protocol.VideoLayer{low}, Codec: protocol.CodecH264ConstrainedBaseline, Audio: true})
		peer.Sink().ShareMedia(p1.ShareID, signal.ShareMediaEvent{Kind: signal.ShareMediaChanged,
			Layers: []protocol.VideoLayer{high}, Codec: protocol.CodecH264High, Audio: true})
		time.Sleep(20*time.Second - time.Millisecond)
		synctest.Wait()
		evs, st := drain(t, cb)
		if len(evs) != 0 || st == nil || len(st.Shares) != 2 {
			t.Fatalf("1 ms before the timeout: events %+v, state %+v; want both shares and no event", evs, st)
		}
		if s := mustShare(t, st, p1.ShareID); s.Status != protocol.ShareStatusStarting || len(s.Layers) != 0 || s.Codec != "" || s.Audio {
			t.Errorf("share %+v, want it still starting as it was declared", s)
		}

		time.Sleep(time.Millisecond)
		synctest.Wait()
		for _, c := range []*signaltest.Client{ca, cb} {
			evs, st := drain(t, c)
			if len(evs) != 1 || evs[0].Kind != protocol.RoomEventKindShareStopped || evs[0].ShareID != p1.ShareID ||
				evs[0].Reason != protocol.EndReasonMediaTimeout || evs[0].UserID != a.UserID || evs[0].Name != a.Name ||
				!evs[0].At.Equal(t0.Add(30*time.Second)) {
				t.Errorf("events at the timeout %+v, want share.stopped{media_timeout} of the first share", evs)
			}
			if st == nil || len(st.Shares) != 1 || st.Shares[0].ID != p2.ShareID {
				t.Errorf("state at the timeout %+v, want the second share only", st)
			}
		}
		calls := peer.MediaCalls()
		if last := calls[len(calls)-1]; len(calls) != 3 || last.Method != "EndShare" || last.Args[0] != p1.ShareID ||
			last.Args[1] != protocol.EndReasonMediaTimeout {
			t.Errorf("MediaPeer calls %+v, want EndShare(%s, media_timeout) last", calls, p1.ShareID)
		}
		if e.logs.count("level=INFO", "share ended", "share_id="+p1.ShareID, "reason=media_timeout", "room_id=lounge") != 1 {
			t.Errorf("no share ended line:\n%s", e.logs)
		}

		// The second share, started 10 s later, has its own 30 s.
		time.Sleep(10*time.Second - time.Millisecond)
		synctest.Wait()
		if evs, _ := drain(t, cb); len(evs) != 0 {
			t.Errorf("events before the second timeout %+v", evs)
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if evs, _ := drain(t, cb); len(evs) != 1 || evs[0].ShareID != p2.ShareID || evs[0].Reason != protocol.EndReasonMediaTimeout {
			t.Errorf("events at the second timeout %+v", evs)
		}
		synctest.Wait()
		if got := e.metric("isshoni_shares", "status", "starting"); got != 0 {
			t.Errorf("isshoni_shares{starting} %v, want 0", got)
		}

		// A first keyframe just in time: the share stays.
		quiet(t, ca, cb)
		p3 := startShare(t, ca, screen("r3"))
		time.Sleep(30*time.Second - time.Millisecond)
		goLive(e, wa.ConnectionID, p3.ShareID)
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		evs, st = drain(t, cb)
		if len(stopped(evs)) != 0 || mustShare(t, st, p3.ShareID).Status != protocol.ShareStatusLive {
			t.Errorf("events %+v, state %+v; want the third share live", evs, st)
		}
	})
}

// The stalled timeout (01 §4.4, §19): a share that stays stalled for StalledTimeout (30 s) ends with media_timeout;
// more stalled facts don't extend it; live again cancels it, and the next stall has a full 30 s. The timers run on
// while the publisher's socket is gone: a share's media does not depend on the WebSocket.
func TestShareStalledTimeout(t *testing.T) {
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
		sink := peer.Sink()
		stall := func(id string) {
			sink.ShareMedia(id, signal.ShareMediaEvent{Kind: signal.ShareMediaStalled})
			synctest.Wait()
		}
		// ends reports whether the share's media_timeout reaches B within the next millisecond, and not before.
		ends := func(id string) bool {
			t.Helper()
			synctest.Wait()
			if evs, _ := drain(t, cb); len(stopped(evs)) != 0 {
				t.Errorf("share.stopped %+v before the timeout", stopped(evs))
			}
			time.Sleep(time.Millisecond)
			synctest.Wait()
			evs, _ := drain(t, cb)
			st := stopped(evs)
			return len(st) == 1 && st[0].ShareID == id && st[0].Reason == protocol.EndReasonMediaTimeout
		}

		p1 := startShare(t, ca, screen("r1"))
		goLive(e, wa.ConnectionID, p1.ShareID)
		quiet(t, ca, cb)
		stall(p1.ShareID)
		time.Sleep(20 * time.Second)
		stall(p1.ShareID) // still not connected: the deadline stays
		sink.ShareMedia(p1.ShareID, signal.ShareMediaEvent{Kind: signal.ShareMediaChanged})
		time.Sleep(10*time.Second - time.Millisecond)
		if !ends(p1.ShareID) {
			t.Error("the stalled share did not end with media_timeout exactly 30 s after it stalled")
		}
		calls := peer.MediaCalls()
		if last := calls[len(calls)-1]; last.Method != "EndShare" || last.Args[0] != p1.ShareID ||
			last.Args[1] != protocol.EndReasonMediaTimeout {
			t.Errorf("MediaPeer calls %+v, want EndShare(%s, media_timeout) last", calls, p1.ShareID)
		}

		// Live again after 29 s, then stalled again: 30 s from the second stall.
		p2 := startShare(t, ca, screen("r2"))
		goLive(e, wa.ConnectionID, p2.ShareID)
		stall(p2.ShareID)
		time.Sleep(29 * time.Second)
		goLive(e, wa.ConnectionID, p2.ShareID)
		time.Sleep(29 * time.Second)
		stall(p2.ShareID)
		time.Sleep(30*time.Second - time.Millisecond)
		if !ends(p2.ShareID) {
			t.Error("the share did not end 30 s after it stalled for the second time")
		}

		// The publisher's socket drops 5 s after its share stalled: the share ends with media_timeout 30 s after the
		// stall, while its connection is still in its grace, and the connection goes 5 s later.
		p3 := startShare(t, ca, screen("r3"))
		goLive(e, wa.ConnectionID, p3.ShareID)
		quiet(t, ca, cb)
		stall(p3.ShareID)
		time.Sleep(5 * time.Second)
		ca.Close()
		time.Sleep(25*time.Second - time.Millisecond)
		if !ends(p3.ShareID) {
			t.Error("the detached publisher's stalled share did not end with media_timeout 30 s after it stalled")
		}
		if peer.Closed() {
			t.Error("the MediaPeer closed with the share, want it kept for the grace")
		}
		time.Sleep(5 * time.Second)
		synctest.Wait()
		evs, _ := drain(t, cb)
		if len(evs) != 1 || evs[0].Kind != protocol.RoomEventKindParticipantLeft || evs[0].UserID != a.UserID ||
			evs[0].Reason != protocol.EndReasonDisconnected {
			t.Errorf("events when the grace expired %+v, want participant.left{disconnected} alone", evs)
		}
		calls = peer.MediaCalls()
		n := len(calls)
		if calls[n-1].Method != "Close" || calls[n-2].Method != "EndShare" || calls[n-2].Args[0] != p3.ShareID ||
			calls[n-2].Args[1] != protocol.EndReasonMediaTimeout {
			t.Errorf("MediaPeer calls %+v, want EndShare(%s, media_timeout), then Close", calls, p3.ShareID)
		}
		if l, st := e.metric("isshoni_shares", "status", "live"), e.metric("isshoni_shares", "status", "stalled"); l != 0 || st != 0 {
			t.Errorf("isshoni_shares live %v, stalled %v; want 0, 0", l, st)
		}
	})
}

// share.stop (01 §8.7, §19): idempotent; another user's share is forbidden; any connection of the share's user may
// stop it, and the share then ends at the MediaPeer of the connection that publishes it.
func TestShareStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		cookieB, b := e.user(false)
		a1, wa1 := e.connect(cookieA, signaltest.DefaultHello())
		a2, wa2 := e.connect(cookieA, signaltest.DefaultHello())
		cb, wb := e.connect(cookieB, signaltest.DefaultHello())
		for _, c := range []*signaltest.Client{a1, a2, cb} {
			join(t, c, "lounge")
		}
		quiet(t, a1, a2, cb)
		all := []*signaltest.Client{a1, a2, cb}
		peer1, peer2 := e.media.Peer(wa1.ConnectionID), e.media.Peer(wa2.ConnectionID)

		p := startShare(t, a1, screen("r1"))
		goLive(e, wa1.ConnectionID, p.ShareID)
		subscribe(t, cb, want(p.ShareID, high, audioOn))
		quiet(t, all...)

		// Another user's share: forbidden, and nothing happens.
		id := request(t, cb, protocol.MessageTypeShareStop, protocol.ShareStop{ShareID: p.ShareID})
		if pe, re := expectError(t, cb, protocol.ErrorCodeForbidden, protocol.ErrorScopeRequest); re != id || pe.Retryable {
			t.Errorf("forbidden %+v re %q", pe, re)
		}
		// An unknown share: ok.
		stopShare(t, cb, "s_0000000000000000")
		stopShare(t, a1, "s_0000000000000000")
		settle()
		for _, c := range all {
			expectOpen(t, c)
		}
		if n := len(peer1.CallsTo("EndShare")); n != 0 {
			t.Errorf("%d EndShare calls, want none yet", n)
		}

		// The user's other connection stops it: it ends at the publishing connection's MediaPeer.
		t1 := time.Now()
		stopShare(t, a2, p.ShareID)
		settle()
		for _, c := range all {
			evs, st := drain(t, c)
			if len(evs) != 1 || evs[0].Kind != protocol.RoomEventKindShareStopped || evs[0].ShareID != p.ShareID ||
				evs[0].Reason != protocol.EndReasonStopped || evs[0].UserID != a.UserID || evs[0].Name != a.Name ||
				!evs[0].At.Equal(t1) {
				t.Errorf("events %+v, want share.stopped{stopped}", evs)
			}
			if st == nil || len(st.Shares) != 0 {
				t.Errorf("state %+v, want no shares", st)
			}
		}
		if calls := peer1.CallsTo("EndShare"); len(calls) != 1 || calls[0].Args[0] != p.ShareID ||
			calls[0].Args[1] != protocol.EndReasonStopped {
			t.Errorf("the publishing connection's EndShare calls %+v, want EndShare(%s, stopped)", calls, p.ShareID)
		}
		if calls := peer2.MediaCalls(); len(calls) != 0 {
			t.Errorf("the stopping connection's MediaPeer calls %+v, want none", calls)
		}
		if got := e.metric("isshoni_shares", "status", "live"); got != 0 {
			t.Errorf("isshoni_shares{live} %v, want 0", got)
		}
		if e.logs.count("level=INFO", "share ended", "share_id="+p.ShareID, "reason=stopped", "conn_id="+wa1.ConnectionID) != 1 {
			t.Errorf("no share ended line:\n%s", e.logs)
		}

		// Stopping it again is ok and does nothing.
		stopShare(t, a2, p.ShareID)
		stopShare(t, a1, p.ShareID)
		settle()
		for _, c := range all {
			expectOpen(t, c)
		}
		if n := len(peer1.CallsTo("EndShare")); n != 1 {
			t.Errorf("%d EndShare calls after stopping again, want 1", n)
		}

		// A connection stops its own share; the subscription to it goes with it.
		p2 := startShare(t, cb, screen("r1"))
		goLive(e, wb.ConnectionID, p2.ShareID)
		subscribe(t, a1, want(p2.ShareID, low, audioOff))
		settle()
		if _, st := drain(t, a2); len(mustShare(t, st, p2.ShareID).Watchers) != 1 {
			t.Fatalf("state %+v, want A watching B's share", st)
		}
		stopShare(t, cb, p2.ShareID)
		if calls := e.media.Peer(wb.ConnectionID).CallsTo("EndShare"); len(calls) != 1 || calls[0].Args[0] != p2.ShareID {
			t.Errorf("B's EndShare calls %+v", calls)
		}
		// The share's id starts a share of B again (it can't, ids are random; the hook can): A's old want is gone.
		quiet(t, all...)
		signal.AddShare(e.hub, "lounge", liveShare(p2.ShareID, b.UserID, wb.ConnectionID))
		settle()
		if _, st := drain(t, a2); len(mustShare(t, st, p2.ShareID).Watchers) != 0 {
			t.Errorf("state %+v, want no watchers: the subscription went with the share", st)
		}
	})
}

// share.update (01 §8.7): the label and the preset of a share of the same user change in room.state, and the reply
// carries the ShareParams that the publishing connection's MediaPeer returns, also when another connection of the
// user asks; share_not_found, forbidden, and the MediaPeer's errors.
func TestShareUpdate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, _ := e.user(false)
		cookieB, _ := e.user(false)
		a1, wa1 := e.connect(cookieA, signaltest.DefaultHello())
		a2, wa2 := e.connect(cookieA, signaltest.DefaultHello())
		cb, _ := e.connect(cookieB, signaltest.DefaultHello())
		for _, c := range []*signaltest.Client{a1, a2, cb} {
			join(t, c, "lounge")
		}
		peer1, peer2 := e.media.Peer(wa1.ConnectionID), e.media.Peer(wa2.ConnectionID)
		v := screen("r1")
		v.Label = "Old"
		p := startShare(t, a1, v)
		quiet(t, a1, a2, cb)

		// update sends share.update on c and returns the reply's ShareParams. The publishing connection gets its ok
		// before the room.state with the change. Another connection's ok comes when the publishing one hands the
		// outcome back, which may be after that room.state.
		update := func(c *signaltest.Client, u protocol.ShareUpdate) protocol.ShareParams {
			t.Helper()
			id := request(t, c, protocol.MessageTypeShareUpdate, u)
			var env protocol.Envelope
			if c == a1 {
				env = expectType(t, c, protocol.MessageTypeOK)
			} else {
				env = okReply(t, c, id)
			}
			got, err := protocol.Decode[protocol.ShareParams](env)
			if err != nil || env.Re != id {
				t.Fatalf("share.update reply re %q (want %q) %s, %v", env.Re, id, env.Data, err)
			}
			return got
		}
		// The MediaPeer computes the new parameters: here the movie preset's.
		movie := signaltest.ShareParamsFor(p.ShareID)
		movie.Encodings[0].MaxBitrate = 8_000_000
		movie.AudioBitrate = 192_000
		peer1.SetShareParams(func(string) protocol.ShareParams { return movie })
		label := "  New label "
		if got := update(a1, protocol.ShareUpdate{ShareID: p.ShareID, Label: &label, Preset: protocol.PresetMovie}); jsonOf(t, got) != jsonOf(t, movie) {
			t.Errorf("ShareParams %s, want the MediaPeer's %s", jsonOf(t, got), jsonOf(t, movie))
		}
		calls := peer1.CallsTo("UpdateShare")
		if len(calls) != 1 || calls[0].Args[0] != p.ShareID {
			t.Fatalf("UpdateShare calls %+v", calls)
		}
		if u := calls[0].Args[1].(protocol.ShareUpdate); u.Label == nil || *u.Label != "New label" || u.Preset != protocol.PresetMovie {
			t.Errorf("UpdateShare got %+v, want the trimmed label and the preset", u)
		}
		settle()
		for _, c := range []*signaltest.Client{a1, a2, cb} {
			evs, st := drain(t, c)
			if s := mustShare(t, st, p.ShareID); len(evs) != 0 || s.Label != "New label" || s.Preset != protocol.PresetMovie {
				t.Errorf("events %+v, share %+v; want the new label and preset and no event", evs, s)
			}
		}
		// A retried share.start now gets the parameters of the update.
		if got := startShare(t, a1, screen("r1")); jsonOf(t, got) != jsonOf(t, movie) {
			t.Errorf("share.start with the share's ref returned %s, want the updated %s", jsonOf(t, got), jsonOf(t, movie))
		}

		// Another connection of the user: the publishing connection's MediaPeer is asked, and the label is cleared.
		empty := "   "
		if got := update(a2, protocol.ShareUpdate{ShareID: p.ShareID, Label: &empty}); jsonOf(t, got) != jsonOf(t, movie) {
			t.Errorf("ShareParams for the other connection %s, want %s", jsonOf(t, got), jsonOf(t, movie))
		}
		if n, m := len(peer1.CallsTo("UpdateShare")), len(peer2.MediaCalls()); n != 2 || m != 0 {
			t.Errorf("%d UpdateShare calls at the publishing connection, %d calls at the other; want 2, 0", n, m)
		}
		settle()
		if _, st := drain(t, cb); mustShare(t, st, p.ShareID).Label != "" || mustShare(t, st, p.ShareID).Preset != protocol.PresetMovie {
			t.Errorf("state %+v, want the label cleared and the preset kept", st)
		}
		drain(t, a1)
		drain(t, a2)
		// Nothing changes: the reply still comes, and no room.state.
		update(a1, protocol.ShareUpdate{ShareID: p.ShareID, Preset: protocol.PresetMovie})
		settle()
		expectOpen(t, cb)

		// Errors.
		id := request(t, a1, protocol.MessageTypeShareUpdate, protocol.ShareUpdate{ShareID: "s_0000000000000000", Preset: protocol.PresetGame})
		if _, re := expectError(t, a1, protocol.ErrorCodeShareNotFound, protocol.ErrorScopeRequest); re != id {
			t.Errorf("re %q", re)
		}
		id = request(t, cb, protocol.MessageTypeShareUpdate, protocol.ShareUpdate{ShareID: p.ShareID, Preset: protocol.PresetGame})
		if _, re := expectError(t, cb, protocol.ErrorCodeForbidden, protocol.ErrorScopeRequest); re != id {
			t.Errorf("re %q", re)
		}
		request(t, a1, protocol.MessageTypeShareUpdate, protocol.ShareUpdate{ShareID: p.ShareID, Preset: "cinema"})
		if pe, _ := expectError(t, a1, protocol.ErrorCodeBadRequest, protocol.ErrorScopeRequest); pe.Params["field"] != "preset" {
			t.Errorf("bad_request %+v", pe)
		}
		notFound := protocol.NewError(protocol.ErrorCodeShareNotFound, protocol.ErrorScopeRequest)
		peer1.Fail("UpdateShare", &notFound)
		for _, c := range []*signaltest.Client{a1, a2} {
			request(t, c, protocol.MessageTypeShareUpdate, protocol.ShareUpdate{ShareID: p.ShareID, Preset: protocol.PresetGame})
			expectError(t, c, protocol.ErrorCodeShareNotFound, protocol.ErrorScopeRequest)
		}
		peer1.Fail("UpdateShare", errors.New("sfu: boom"))
		request(t, a2, protocol.MessageTypeShareUpdate, protocol.ShareUpdate{ShareID: p.ShareID, Preset: protocol.PresetGame})
		pe, _ := expectError(t, a2, protocol.ErrorCodeInternal, protocol.ErrorScopeRequest)
		if ref, _ := pe.Params["ref"].(string); e.logs.count("level=ERROR", "ref="+ref, "update share", "sfu: boom") != 1 {
			t.Errorf("no ERROR line for ref %s:\n%s", ref, e.logs)
		}
		peer1.Fail("UpdateShare", nil)
		settle()
		if _, st := drain(t, cb); st != nil {
			t.Errorf("state %+v after failed updates, want none", st)
		}
		if n := len(peer1.CallsTo("UpdateShare")); n != 6 {
			t.Errorf("%d UpdateShare calls, want 6", n)
		}

		// The publishing connection's actor is busy (a slow dependency call): the other connection's request waits
		// for it, gives up after 10 s with internal, and the update is not applied later. The requesting connection
		// is not held up meanwhile: it answers a ping at once.
		release := signal.Stall(e.hub, wa1.ConnectionID)
		start := time.Now()
		id = request(t, a2, protocol.MessageTypeShareUpdate, protocol.ShareUpdate{ShareID: p.ShareID, Preset: protocol.PresetText})
		ping(t, a2)
		if d := time.Since(start); d != 0 {
			t.Errorf("the pong came after %v, want at once: the connection doesn't wait for the other one", d)
		}
		pe, re := expectError(t, a2, protocol.ErrorCodeInternal, protocol.ErrorScopeRequest)
		if d := time.Since(start); d != 10*time.Second || !pe.Retryable || re != id {
			t.Errorf("gave up after %v (retryable %v, re %q); want 10 s, for request %s", d, pe.Retryable, re, id)
		}
		release()
		ping(t, a1)
		ping(t, a2)
		settle()
		if n := len(peer1.CallsTo("UpdateShare")); n != 6 {
			t.Errorf("%d UpdateShare calls, want 6: an update that was given up is not applied", n)
		}
		if snap := e.hub.Snapshot(); snap.Rooms[0].Shares[0].Info.Preset != protocol.PresetMovie {
			t.Errorf("preset %s, want movie", snap.Rooms[0].Shares[0].Info.Preset)
		}
	})
}

// Two connections of a user update each other's shares at the same moment (01 §4.1: a share belongs to its user, so
// each of the user's connections may update it). Neither actor waits for the other one: both updates are applied,
// and both requests get their ok at once.
func TestShareUpdateCrossed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		a1, w1 := e.connect(cookie, signaltest.DefaultHello())
		a2, w2 := e.connect(cookie, signaltest.DefaultHello())
		join(t, a1, "lounge")
		join(t, a2, "lounge")
		s1 := startShare(t, a1, screen("r1")).ShareID
		time.Sleep(time.Second) // shares are in startedAt order
		s2 := startShare(t, a2, screen("r1")).ShareID
		quiet(t, a1, a2)
		peer1, peer2 := e.media.Peer(w1.ConnectionID), e.media.Peer(w2.ConnectionID)

		// Both actors are held until each has its request in its inbox: each then hands its update to the other one
		// before it gets to the update that the other one handed to it.
		release1, release2 := signal.Stall(e.hub, w1.ConnectionID), signal.Stall(e.hub, w2.ConnectionID)
		start := time.Now()
		id1 := request(t, a1, protocol.MessageTypeShareUpdate, protocol.ShareUpdate{ShareID: s2, Preset: protocol.PresetMovie})
		id2 := request(t, a2, protocol.MessageTypeShareUpdate, protocol.ShareUpdate{ShareID: s1, Preset: protocol.PresetGame})
		synctest.Wait()
		release1()
		release2()
		for _, tc := range []struct {
			c       *signaltest.Client
			id      string
			shareID string
		}{{a1, id1, s2}, {a2, id2, s1}} {
			env := okReply(t, tc.c, tc.id)
			if got, err := protocol.Decode[protocol.ShareParams](env); err != nil || got.ShareID != tc.shareID {
				t.Errorf("share.update reply %s (%v), want the ShareParams of %s", env.Data, err, tc.shareID)
			}
		}
		if d := time.Since(start); d != 0 {
			t.Errorf("the replies came after %v, want at once", d)
		}
		for _, tc := range []struct {
			peer    *signaltest.Peer
			shareID string
			preset  protocol.Preset
		}{{peer1, s1, protocol.PresetGame}, {peer2, s2, protocol.PresetMovie}} {
			calls := tc.peer.CallsTo("UpdateShare")
			if len(calls) != 1 || calls[0].Args[0] != tc.shareID || calls[0].Args[1].(protocol.ShareUpdate).Preset != tc.preset {
				t.Errorf("UpdateShare calls %+v, want one for %s with preset %s", calls, tc.shareID, tc.preset)
			}
		}
		settle()
		for _, c := range []*signaltest.Client{a1, a2} {
			_, st := drain(t, c)
			if p1, p2 := mustShare(t, st, s1).Preset, mustShare(t, st, s2).Preset; p1 != protocol.PresetGame || p2 != protocol.PresetMovie {
				t.Errorf("presets %s and %s, want game and movie", p1, p2)
			}
		}
	})
}

// The reply to a share.update that went to another connection's actor is for the socket that the request came on.
// A connection that has resumed on a new socket by then gets no reply: the client gave the request up with the old
// socket, and may use its id again on the new one. The update itself is applied.
func TestShareUpdateReplyAfterResume(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		a1, w1 := e.connect(cookie, signaltest.DefaultHello())
		a2, w2 := e.connect(cookie, signaltest.DefaultHello())
		join(t, a1, "lounge")
		join(t, a2, "lounge")
		p := startShare(t, a1, screen("r1"))
		quiet(t, a1, a2)

		release := signal.Stall(e.hub, w1.ConnectionID)
		request(t, a2, protocol.MessageTypeShareUpdate, protocol.ShareUpdate{ShareID: p.ShareID, Preset: protocol.PresetMovie})
		ping(t, a2) // the update is with the publishing connection
		a2.Close()
		synctest.Wait()
		a3, w3 := e.resume(cookie, w2.ResumeToken)
		if !w3.Resumed || w3.ConnectionID != w2.ConnectionID {
			t.Fatalf("welcome resumed %v, connectionId %s (want %s)", w3.Resumed, w3.ConnectionID, w2.ConnectionID)
		}
		expectState(t, a3, "lounge")
		release()
		settle()
		// drain fails on anything but room traffic: no ok reaches the new socket.
		if _, st := drain(t, a3); mustShare(t, st, p.ShareID).Preset != protocol.PresetMovie {
			t.Errorf("state %+v, want the preset of the update", st)
		}
		if n := len(e.media.Peer(w1.ConnectionID).CallsTo("UpdateShare")); n != 1 {
			t.Errorf("%d UpdateShare calls, want 1", n)
		}
		if e.logs.count("level=DEBUG", "share.update reply dropped", "conn_id="+w2.ConnectionID) != 1 {
			t.Errorf("no DEBUG line for the dropped reply:\n%s", e.logs)
		}
		ping(t, a3)
	})
}

// replaces (01 §10.6, §19): a re-publish after a restart carries the old share id, which is copied to room.state
// and to share.started, and Web Push is not called for it. Push is called once for each other share that goes live,
// and never for one that doesn't.
func TestShareReplaces(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		cookieB, _ := e.user(false)
		ca, wa := e.connect(cookieA, signaltest.DefaultHello())
		cb, _ := e.connect(cookieB, signaltest.DefaultHello())
		join(t, ca, "lounge")
		join(t, cb, "lounge")
		quiet(t, ca, cb)

		const old = "s_q7m2x9c4v8b1n5k3" // from before the restart: the server can't know it
		v := screen("r1")
		v.Replaces = old
		p := startShare(t, ca, v)
		if st := expectState(t, cb, "lounge"); mustShare(t, &st, p.ShareID).Replaces != old {
			t.Errorf("state %+v, want replaces %s", st, old)
		}
		goLive(e, wa.ConnectionID, p.ShareID)
		if ev := expectEvent(t, cb, protocol.RoomEventKindShareStarted, a.UserID); ev.ShareID != p.ShareID || ev.Replaces != old {
			t.Errorf("share.started %+v, want replaces %s", ev, old)
		}
		settle()
		if n := len(e.push.Events()); n != 0 {
			t.Errorf("%d push events for a share with replaces, want none", n)
		}

		// A share that never goes live makes no push; each one that does makes one.
		startShare(t, ca, screen("r2"))
		p3 := startShare(t, ca, screen("r3"))
		p4 := startShare(t, ca, screen("r4"))
		goLive(e, wa.ConnectionID, p3.ShareID)
		goLive(e, wa.ConnectionID, p4.ShareID)
		goLive(e, wa.ConnectionID, p3.ShareID)
		time.Sleep(time.Minute) // the second share has timed out
		synctest.Wait()
		pushed := e.push.Events()
		if len(pushed) != 2 || pushed[0].ShareID != p3.ShareID || pushed[1].ShareID != p4.ShareID {
			t.Errorf("push events %+v, want one for each of the two shares that went live", pushed)
		}

		// Without the room's name there is no notification: the room is gone, or the store fails.
		quiet(t, ca, cb)
		stopShare(t, ca, p3.ShareID)
		p5 := startShare(t, ca, screen("r5"))
		e.rooms.FailGetRoom(errors.New("database is locked"))
		goLive(e, wa.ConnectionID, p5.ShareID)
		settle()
		if n := len(e.push.Events()); n != 2 {
			t.Errorf("%d push events, want still 2", n)
		}
		if e.logs.count("level=WARN", "share started: no push", "share_id="+p5.ShareID, "database is locked") != 1 {
			t.Errorf("no WARN line for the push that was not sent:\n%s", e.logs)
		}
		e.rooms.FailGetRoom(nil)
	})
}

// A flood close keeps the grace (01 §4.2; decided at integration): a sharer whose connection the hub closes with
// rate_limited and 4429 is detached, not gone. Its share stays as it is, a resume within the 30 s gets the connection
// back with it, and without one the share ends with disconnected when the grace expires.
func TestShareFloodKeepsGrace(t *testing.T) {
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
		goLive(e, wa.ConnectionID, p.ShareID)
		quiet(t, ca, cb)
		peer := e.media.Peer(wa.ConnectionID)
		// flood sends 100 messages per second, against a refill of 20, until the hub closes the socket.
		flood := func(c *signaltest.Client) {
			t.Helper()
			for start := time.Now(); time.Since(start) < time.Minute; time.Sleep(10 * time.Millisecond) {
				if err := c.Send(protocol.MessageTypePing, "", protocol.Ping{T: 1}); err != nil {
					break
				}
			}
			expectClose(t, c, protocol.CloseCodeRateLimited)
		}
		// detached checks that B saw A's connection go reconnecting with its share untouched, and nothing end.
		detached := func() {
			t.Helper()
			synctest.Wait()
			evs, st := drain(t, cb)
			if len(evs) != 0 {
				t.Errorf("events after the flood close %+v, want none", evs)
			}
			if ps, _ := statuses(participantOf(t, st, a.UserID)); ps != protocol.ParticipantStatusReconnecting ||
				mustShare(t, st, p.ShareID).Status != protocol.ShareStatusLive {
				t.Errorf("state after the flood close %+v, want A reconnecting and its share live", st)
			}
		}

		flood(ca)
		closed := time.Now()
		detached()
		time.Sleep(grace - time.Second)
		c2, w2 := e.resume(cookieA, wa.ResumeToken)
		if !w2.Resumed || w2.ConnectionID != wa.ConnectionID {
			t.Fatalf("welcome resumed %v after %v, want the connection back within the grace", w2.Resumed, time.Since(closed))
		}
		if st := expectState(t, c2, "lounge"); mustShare(t, &st, p.ShareID).Status != protocol.ShareStatusLive {
			t.Errorf("state after the resume %+v", st)
		}
		if n := len(peer.CallsTo("EndShare")); n != 0 || peer.Closed() {
			t.Errorf("%d EndShare calls, closed %v; want the MediaPeer untouched", n, peer.Closed())
		}

		// Again, and nobody resumes: the share ends with disconnected when the grace expires. (The client sees the
		// close up to one 10 ms step of its loop after the hub closed the socket.)
		quiet(t, cb)
		flood(c2) // the resumed connection kept its rate-limit buckets, and they have refilled
		closed = time.Now()
		detached()
		time.Sleep(grace - time.Second)
		synctest.Wait()
		if evs, _ := drain(t, cb); len(evs) != 0 {
			t.Errorf("events 1 s before the grace expires %+v, want none", evs)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		evs, _ := drain(t, cb)
		if len(evs) != 2 || evs[0].Kind != protocol.RoomEventKindShareStopped || evs[0].ShareID != p.ShareID ||
			evs[0].Reason != protocol.EndReasonDisconnected || evs[1].Kind != protocol.RoomEventKindParticipantLeft ||
			evs[1].Reason != protocol.EndReasonDisconnected {
			t.Fatalf("events when the grace expired %+v, want share.stopped and participant.left, disconnected", evs)
		}
		if d := evs[0].At.Sub(closed); d <= grace-20*time.Millisecond || d > grace {
			t.Errorf("the share ended %v after the client saw the close, want the 30 s grace", d)
		}
		if calls := peer.CallsTo("EndShare"); len(calls) != 1 || calls[0].Args[1] != protocol.EndReasonDisconnected || !peer.Closed() {
			t.Errorf("EndShare calls %+v, closed %v", calls, peer.Closed())
		}
	})
}

// A share ends when its connection leaves the room (01 §4.2, §8.4): share.stopped{left} for the room and EndShare at
// the MediaPeer before it closes. A media fact or an error from the MediaPeer ends it too: codec_not_supported (01 §9
// rule 7) and the reserved "gone". Facts about shares that the connection doesn't publish are dropped.
func TestShareEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		e.rooms.Add(room1)
		cookieA, a := e.user(false)
		cookieB, _ := e.user(false)
		ca, wa := e.connect(cookieA, signaltest.DefaultHello())
		cb, _ := e.connect(cookieB, signaltest.DefaultHello())
		join(t, ca, "lounge")
		join(t, cb, "lounge")
		peerA := e.media.Peer(wa.ConnectionID)

		// codec_not_supported from the MediaPeer: the error reaches the sharer first, then the share ends with stopped.
		p1 := startShare(t, ca, screen("r1"))
		quiet(t, ca, cb)
		codec := protocol.NewError(protocol.ErrorCodeCodecNotSupported, protocol.ErrorScopeShare)
		codec.ShareID = p1.ShareID
		peerA.Sink().Error(codec)
		pe, re := expectError(t, ca, protocol.ErrorCodeCodecNotSupported, protocol.ErrorScopeShare)
		if re != "" || pe.ShareID != p1.ShareID || pe.Retryable {
			t.Errorf("codec_not_supported %+v re %q", pe, re)
		}
		if ev := expectEvent(t, ca, protocol.RoomEventKindShareStopped, a.UserID); ev.ShareID != p1.ShareID ||
			ev.Reason != protocol.EndReasonStopped {
			t.Errorf("share.stopped %+v", ev)
		}
		if ev := expectEvent(t, cb, protocol.RoomEventKindShareStopped, a.UserID); ev.Reason != protocol.EndReasonStopped {
			t.Errorf("share.stopped %+v", ev)
		}
		if calls := peerA.CallsTo("EndShare"); len(calls) != 1 || calls[0].Args[0] != p1.ShareID ||
			calls[0].Args[1] != protocol.EndReasonStopped {
			t.Errorf("EndShare calls %+v", calls)
		}
		if got := e.metric("isshoni_ws_errors_total", "code", string(protocol.ErrorCodeCodecNotSupported)); got != 1 {
			t.Errorf("errors sent (codec_not_supported) %v, want 1", got)
		}
		// The same error about a share that the connection doesn't publish is only forwarded.
		pB := startShare(t, cb, screen("r1"))
		quiet(t, ca, cb)
		codec.ShareID = pB.ShareID
		peerA.Sink().Error(codec)
		expectError(t, ca, protocol.ErrorCodeCodecNotSupported, protocol.ErrorScopeShare)
		// So are media facts about it, or about a share that has ended.
		peerA.Sink().ShareMedia(pB.ShareID, liveEvent())
		peerA.Sink().ShareMedia(p1.ShareID, liveEvent())
		settle()
		expectOpen(t, ca)
		expectOpen(t, cb)
		if n := len(peerA.CallsTo("EndShare")); n != 1 {
			t.Errorf("%d EndShare calls, want still 1", n)
		}
		if n := len(e.push.Events()); n != 0 {
			t.Errorf("%d push events, want none", n)
		}

		// The reserved "gone": the share's tracks are gone for good.
		p2 := startShare(t, ca, screen("r2"))
		goLive(e, wa.ConnectionID, p2.ShareID)
		quiet(t, ca, cb)
		peerA.Sink().ShareMedia(p2.ShareID, signal.ShareMediaEvent{Kind: signal.ShareMediaGone})
		if ev := expectEvent(t, cb, protocol.RoomEventKindShareStopped, a.UserID); ev.ShareID != p2.ShareID ||
			ev.Reason != protocol.EndReasonStopped {
			t.Errorf("share.stopped %+v", ev)
		}
		synctest.Wait() // the room is told first; A's actor then ends the share at its MediaPeer
		if calls := peerA.CallsTo("EndShare"); len(calls) != 2 || calls[1].Args[0] != p2.ShareID {
			t.Errorf("EndShare calls %+v", calls)
		}

		// The connection leaves the room: its shares end with left, at the MediaPeer before it closes.
		p3 := startShare(t, ca, screen("r3"))
		time.Sleep(time.Second) // shares are in startedAt order
		p4 := startShare(t, ca, screen("r4"))
		goLive(e, wa.ConnectionID, p4.ShareID)
		quiet(t, ca, cb)
		join(t, ca, room1.ID) // leaves lounge
		settle()
		evs, st := drain(t, cb)
		if len(evs) != 3 || evs[0].Kind != protocol.RoomEventKindShareStopped || evs[0].ShareID != p3.ShareID ||
			evs[0].Reason != protocol.EndReasonLeft || evs[1].ShareID != p4.ShareID || evs[1].Reason != protocol.EndReasonLeft ||
			evs[2].Kind != protocol.RoomEventKindParticipantLeft {
			t.Errorf("events %+v, want share.stopped{left} for both shares, then participant.left", evs)
		}
		if st == nil || len(st.Shares) != 1 || st.Shares[0].ID != pB.ShareID {
			t.Errorf("state %+v, want B's share only", st)
		}
		calls := peerA.MediaCalls()
		n := len(calls)
		if calls[n-1].Method != "Close" || calls[n-2].Method != "EndShare" || calls[n-2].Args[0] != p4.ShareID ||
			calls[n-2].Args[1] != protocol.EndReasonLeft || calls[n-3].Method != "EndShare" || calls[n-3].Args[0] != p3.ShareID {
			t.Errorf("MediaPeer calls %+v, want EndShare(left) for both shares, then Close", calls)
		}
		if s, l := e.metric("isshoni_shares", "status", "starting"), e.metric("isshoni_shares", "status", "live"); s != 1 || l != 0 {
			t.Errorf("isshoni_shares starting %v, live %v; want B's starting share only", s, l)
		}
		// Events of the peer that the connection left behind are dropped, a late first keyframe included.
		peerA.Sink().ShareMedia(p3.ShareID, liveEvent())
		// The share timer went with the room: nothing fires for the shares of the old room.
		time.Sleep(time.Minute)
		synctest.Wait()
		expectOpen(t, ca)
		if n := len(e.media.Peer(wa.ConnectionID).MediaCalls()); n != 0 {
			t.Errorf("%d calls at the new MediaPeer, want none", n)
		}
	})
}
