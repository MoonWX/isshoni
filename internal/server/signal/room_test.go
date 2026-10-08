package signal_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/signal"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// Rooms and participants (01 §4, §8.4–8.6, §19 "Rooms"): room.join and room.leave, participants that merge a user's
// connections, coalesced byte-identical room.state, room.event, CloseRoom, the room_full policy and the live
// snapshot. Shares and subscriptions come with share.start and subscribe.update (README S40); until then the tests
// put them in place with the AddShare and SetWants hooks.

var room1 = protocol.RoomInfo{ID: "room1", Name: "Movie night"}

// join sends room.join and reads its ok, which must name the room, and the room.state that must follow at once
// (01 §7). It returns the state.
func join(t *testing.T, c *signaltest.Client, roomID string) protocol.RoomState {
	t.Helper()
	id := request(t, c, protocol.MessageTypeRoomJoin, protocol.RoomJoin{RoomID: roomID})
	env := expectType(t, c, protocol.MessageTypeOK)
	res, err := protocol.Decode[protocol.RoomJoinResult](env)
	if err != nil || env.Re != id || res.Room.ID != roomID {
		t.Fatalf("room.join reply re %q (want %q) %s, %v", env.Re, id, env.Data, err)
	}
	return expectState(t, c, roomID)
}

// leave sends room.leave and reads its ok.
func leave(t *testing.T, c *signaltest.Client) {
	t.Helper()
	id := request(t, c, protocol.MessageTypeRoomLeave, protocol.Empty{})
	if env := expectType(t, c, protocol.MessageTypeOK); env.Re != id {
		t.Fatalf("room.leave reply re %q, want %q", env.Re, id)
	}
}

// expectState reads the next message, a room.state of roomID.
func expectState(t *testing.T, c *signaltest.Client, roomID string) protocol.RoomState {
	t.Helper()
	st, err := protocol.Decode[protocol.RoomState](expectType(t, c, protocol.MessageTypeRoomState))
	if err != nil {
		t.Fatalf("decode room.state: %v", err)
	}
	if st.RoomID != roomID {
		t.Fatalf("room.state of %q, want %q", st.RoomID, roomID)
	}
	return st
}

// expectEvent reads the next message, a room.event of the kind about userID.
func expectEvent(t *testing.T, c *signaltest.Client, kind protocol.RoomEventKind, userID string) protocol.RoomEvent {
	t.Helper()
	ev, err := protocol.Decode[protocol.RoomEvent](expectType(t, c, protocol.MessageTypeRoomEvent))
	if err != nil {
		t.Fatalf("decode room.event: %v", err)
	}
	if ev.Kind != kind || ev.UserID != userID {
		t.Fatalf("room.event %s about %q, want %s about %q", ev.Kind, ev.UserID, kind, userID)
	}
	return ev
}

// settle lets fake time pass beyond the coalescing window and waits until every goroutine of the bubble is blocked:
// every scheduled room.state has gone out and reached the clients.
func settle() {
	time.Sleep(time.Second)
	synctest.Wait()
}

// drain reads every queued message, which must be room.events and room.states, and returns the events in order and
// the last state (nil when none).
func drain(t *testing.T, c *signaltest.Client) ([]protocol.RoomEvent, *protocol.RoomState) {
	t.Helper()
	var evs []protocol.RoomEvent
	var last *protocol.RoomState
	for c.Pending() > 0 {
		env := recv(t, c)
		switch env.Type {
		case protocol.MessageTypeRoomEvent:
			ev, err := protocol.Decode[protocol.RoomEvent](env)
			if err != nil {
				t.Fatal(err)
			}
			evs = append(evs, ev)
		case protocol.MessageTypeRoomState:
			st, err := protocol.Decode[protocol.RoomState](env)
			if err != nil {
				t.Fatal(err)
			}
			last = &st
		default:
			t.Fatalf("got %s %s, want room traffic", env.Type, env.Data)
		}
	}
	return evs, last
}

// userIDs returns the participants' user ids in order.
func userIDs(st protocol.RoomState) []string {
	var ids []string
	for _, p := range st.Participants {
		ids = append(ids, p.UserID)
	}
	return ids
}

// connIDs returns a participant's connection ids in order.
func connIDs(p protocol.ParticipantInfo) []string {
	var ids []string
	for _, c := range p.Connections {
		ids = append(ids, c.ID)
	}
	return ids
}

// room.join and room.leave (01 §8.4): the ok and the room.state that follows at once, the MediaPeer, joining the
// same room again, and the errors.
func TestRoomJoinLeave(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, a := e.user(false)
		c, w := e.connect(cookie, signaltest.DefaultHello())

		t0 := time.Now()
		st := join(t, c, "lounge")
		if st.Rev == 0 || len(st.Shares) != 0 || len(st.Participants) != 1 {
			t.Fatalf("state %+v", st)
		}
		p := st.Participants[0]
		want := protocol.ConnectionInfo{ID: w.ConnectionID, Kind: protocol.ClientKindWeb, Role: protocol.RoleFull,
			Status: protocol.ConnectionStatusOnline}
		if p.UserID != a.UserID || p.Name != a.Name || p.Status != protocol.ParticipantStatusPresent ||
			!p.JoinedAt.Equal(t0) || !slices.Equal(p.Connections, []protocol.ConnectionInfo{want}) {
			t.Errorf("participant %+v", p)
		}
		peers := e.media.Peers()
		if len(peers) != 1 {
			t.Fatalf("%d peers, want 1", len(peers))
		}
		h := signaltest.DefaultHello()
		wantParams := signal.PeerParams{ConnectionID: w.ConnectionID, UserID: a.UserID, RoomID: "lounge",
			Role: protocol.RoleFull, Caps: h.Caps, Client: h.Client}
		if got := peers[0].Params; got.ConnectionID != wantParams.ConnectionID || got.UserID != wantParams.UserID ||
			got.RoomID != wantParams.RoomID || got.Role != wantParams.Role || got.Client != wantParams.Client ||
			!slices.Equal(got.Caps.Decode, wantParams.Caps.Decode) {
			t.Errorf("peer params %+v, want %+v", got, wantParams)
		}
		synctest.Wait()
		if r, n := e.metric("isshoni_rooms"), e.metric("isshoni_participants"); r != 1 || n != 1 {
			t.Errorf("rooms %v, participants %v; want 1, 1", r, n)
		}

		// The same room again: ok and room.state again, with nothing new (the rev too), and no new MediaPeer.
		if again := join(t, c, "lounge"); again.Rev != st.Rev || len(again.Participants) != 1 {
			t.Errorf("state after joining again %+v", again)
		}
		settle()
		expectOpen(t, c)
		if n := len(e.media.Peers()); n != 1 {
			t.Errorf("%d peers after joining the same room, want 1", n)
		}

		leave(t, c)
		if !peers[0].Closed() {
			t.Error("the MediaPeer was not closed on room.leave")
		}
		synctest.Wait()
		if r, n := e.metric("isshoni_rooms"), e.metric("isshoni_participants"); r != 0 || n != 0 {
			t.Errorf("rooms %v, participants %v; want 0, 0", r, n)
		}
		if snap := e.hub.Snapshot(); len(snap.Rooms) != 0 {
			t.Errorf("snapshot %+v, want no rooms", snap)
		}
		leave(t, c) // in no room: ok as well

		// Errors: an unknown room, CanJoin refusing, the directory failing, the MediaPlane failing.
		id := request(t, c, protocol.MessageTypeRoomJoin, protocol.RoomJoin{RoomID: "nowhere"})
		if _, re := expectError(t, c, protocol.ErrorCodeRoomNotFound, protocol.ErrorScopeRequest); re != id {
			t.Errorf("re %q", re)
		}
		e.rooms.SetCanJoin(func(signal.Identity, string) error { return errors.New("locked") })
		id = request(t, c, protocol.MessageTypeRoomJoin, protocol.RoomJoin{RoomID: "lounge"})
		if _, re := expectError(t, c, protocol.ErrorCodeForbidden, protocol.ErrorScopeRequest); re != id {
			t.Errorf("re %q", re)
		}
		e.rooms.SetCanJoin(nil)
		e.rooms.FailGetRoom(errors.New("database is locked"))
		request(t, c, protocol.MessageTypeRoomJoin, protocol.RoomJoin{RoomID: "lounge"})
		pe, _ := expectError(t, c, protocol.ErrorCodeInternal, protocol.ErrorScopeRequest)
		if ref, _ := pe.Params["ref"].(string); e.logs.count("level=ERROR", "ref="+ref, "get room") != 1 {
			t.Errorf("no ERROR line for ref %s", ref)
		}
		e.rooms.FailGetRoom(nil)
		e.media.FailNewPeer(errors.New("sfu closed"))
		request(t, c, protocol.MessageTypeRoomJoin, protocol.RoomJoin{RoomID: "lounge"})
		pe, _ = expectError(t, c, protocol.ErrorCodeInternal, protocol.ErrorScopeRequest)
		if ref, _ := pe.Params["ref"].(string); e.logs.count("level=ERROR", "ref="+ref, "new media peer") != 1 {
			t.Errorf("no ERROR line for ref %s", ref)
		}
		settle()
		expectOpen(t, c)
		if snap := e.hub.Snapshot(); len(snap.Rooms) != 0 {
			t.Errorf("snapshot %+v after a failed NewPeer, want no rooms", snap)
		}
		if r, n := e.metric("isshoni_rooms"), e.metric("isshoni_participants"); r != 0 || n != 0 {
			t.Errorf("rooms %v, participants %v after a failed NewPeer; want 0, 0", r, n)
		}
		// In no room, room messages still get not_in_room.
		request(t, c, protocol.MessageTypeShareStop, protocol.ShareStop{ShareID: "s_x"})
		expectError(t, c, protocol.ErrorCodeNotInRoom, protocol.ErrorScopeRequest)
	})
}

// Participants (01 §4.1, §8.6, §19): two connections of one user are one participant with two connections; the
// others learn about a new participant from participant.joined, and about one that leaves with its last connection
// from participant.left; the joining connection gets no events at all, only its ok and room.state. Participants are
// sorted by joinedAt, then userId.
func TestRoomParticipants(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieB, b := e.user(false) // user01: joins last, sorts first by id
		cookieA, a := e.user(false) // user02
		a1, wa1 := e.connect(cookieA, signaltest.DefaultHello())
		a2, wa2 := e.connect(cookieA, signaltest.DefaultHello())
		cb, wb := e.connect(cookieB, signaltest.DefaultHello())

		join(t, a1, "lounge")
		time.Sleep(time.Second)
		st := join(t, cb, "lounge")
		if !slices.Equal(userIDs(st), []string{a.UserID, b.UserID}) {
			t.Errorf("participants %v, want A then B (by joinedAt)", userIDs(st))
		}
		ev := expectEvent(t, a1, protocol.RoomEventKindParticipantJoined, b.UserID)
		if ev.RoomID != "lounge" || ev.Name != b.Name || !ev.At.Equal(time.Now()) || ev.Reason != "" {
			t.Errorf("participant.joined %+v", ev)
		}
		settle()
		if got := expectState(t, a1, "lounge"); got.Rev != st.Rev {
			t.Errorf("A got rev %d, B's join state has %d", got.Rev, st.Rev)
		}
		expectOpen(t, a1)
		expectOpen(t, cb) // no events on join: nothing about A, nothing about itself

		// A's second connection: no event, one participant with both connections.
		time.Sleep(time.Second)
		st = join(t, a2, "lounge")
		if len(st.Participants) != 2 || !slices.Equal(connIDs(st.Participants[0]),
			[]string{wa1.ConnectionID, wa2.ConnectionID}) {
			t.Errorf("state %+v", st)
		}
		settle()
		for _, c := range []*signaltest.Client{a1, cb} {
			if got := expectState(t, c, "lounge"); got.Rev != st.Rev {
				t.Errorf("rev %d, want %d", got.Rev, st.Rev)
			}
			expectOpen(t, c)
		}
		expectOpen(t, a2)

		// One of A's connections leaves: A stays, with one connection; no event.
		leave(t, a1)
		settle()
		for _, c := range []*signaltest.Client{a2, cb} {
			got := expectState(t, c, "lounge")
			if len(got.Participants) != 2 || !slices.Equal(connIDs(got.Participants[0]), []string{wa2.ConnectionID}) {
				t.Errorf("state %+v", got)
			}
			expectOpen(t, c)
		}
		expectOpen(t, a1)

		// A's last connection leaves: participant.left{left} to B only.
		leave(t, a2)
		ev = expectEvent(t, cb, protocol.RoomEventKindParticipantLeft, a.UserID)
		if ev.Reason != protocol.EndReasonLeft || ev.Name != a.Name {
			t.Errorf("participant.left %+v", ev)
		}
		settle()
		if got := expectState(t, cb, "lounge"); !slices.Equal(userIDs(got), []string{b.UserID}) {
			t.Errorf("participants %v", userIDs(got))
		}
		expectOpen(t, cb)
		expectOpen(t, a2)
		if snap := e.hub.Snapshot(); len(snap.Rooms) != 1 || len(snap.Rooms[0].Participants) != 1 ||
			snap.Rooms[0].Participants[0].Connections[0].ID != wb.ConnectionID {
			t.Errorf("snapshot %+v", snap)
		}
	})
}

// Watchers (01 §4.1, §19): from the desired subscriptions, merged per participant (the maximum video, audio on if
// any connection has it on); the owner is never a watcher of its own share, and {off, off} is not watching. Shares
// are sorted by startedAt, then id. When the owner's connection goes, its shares end: share.stopped with the reason,
// then EndShare and Close at its MediaPeer.
func TestRoomWatchers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		cookieB, b := e.user(false)
		cookieC, c := e.user(false)
		a1, wa1 := e.connect(cookieA, signaltest.DefaultHello())
		a2, wa2 := e.connect(cookieA, signaltest.DefaultHello())
		cb, wb := e.connect(cookieB, signaltest.DefaultHello())
		cc, wc := e.connect(cookieC, signaltest.DefaultHello())
		for _, cl := range []*signaltest.Client{a1, a2, cb, cc} {
			join(t, cl, "lounge")
		}
		settle()
		for _, cl := range []*signaltest.Client{a1, a2, cb, cc} {
			for cl.Pending() > 0 {
				recv(t, cl) // the joins' events and states
			}
		}

		start := time.Now()
		shareB := protocol.ShareInfo{ID: "s_bbbbbbbbbbbbbbbb", UserID: b.UserID, ConnectionID: wb.ConnectionID,
			Kind: protocol.ShareKindScreen, Preset: protocol.PresetMovie, Audio: true, Status: protocol.ShareStatusLive,
			Layers: []protocol.VideoLayer{protocol.VideoLayerHigh, protocol.VideoLayerLow},
			Codec:  protocol.CodecH264High, StartedAt: start}
		shareA := protocol.ShareInfo{ID: "s_zzzzzzzzzzzzzzzz", UserID: a.UserID, ConnectionID: wa1.ConnectionID,
			Kind: protocol.ShareKindWindow, Preset: protocol.PresetAuto, Status: protocol.ShareStatusStarting,
			StartedAt: start.Add(-time.Second)}
		shareA2 := shareA
		shareA2.ID = "s_aaaaaaaaaaaaaaaa" // same startedAt: sorted by id
		for _, s := range []protocol.ShareInfo{shareB, shareA, shareA2} {
			if !signal.AddShare(e.hub, "lounge", s) {
				t.Fatalf("AddShare %s", s.ID)
			}
		}
		wants := []struct {
			conn string
			w    protocol.SubscriptionWant
		}{
			{wa1.ConnectionID, protocol.SubscriptionWant{ShareID: shareB.ID, Video: protocol.VideoLayerLow, Audio: protocol.AudioStateOff}},
			{wa2.ConnectionID, protocol.SubscriptionWant{ShareID: shareB.ID, Video: protocol.VideoLayerOff, Audio: protocol.AudioStateOn}},
			{wc.ConnectionID, protocol.SubscriptionWant{ShareID: shareB.ID, Video: protocol.VideoLayerOff, Audio: protocol.AudioStateOff}},
			{wb.ConnectionID, protocol.SubscriptionWant{ShareID: shareB.ID, Video: protocol.VideoLayerHigh, Audio: protocol.AudioStateOn}},
			{wa2.ConnectionID, protocol.SubscriptionWant{ShareID: shareA.ID, Video: protocol.VideoLayerHigh, Audio: protocol.AudioStateOn}},
			{wb.ConnectionID, protocol.SubscriptionWant{ShareID: shareA.ID, Video: protocol.VideoLayerHigh, Audio: protocol.AudioStateOff}},
			{wc.ConnectionID, protocol.SubscriptionWant{ShareID: shareA.ID, Video: protocol.VideoLayerLow, Audio: protocol.AudioStateOn}},
		}
		for _, w := range wants {
			if !signal.SetWants(e.hub, w.conn, w.w) {
				t.Fatalf("SetWants %s", w.conn)
			}
		}
		settle()
		var st protocol.RoomState
		for cc.Pending() > 0 {
			st = expectState(t, cc, "lounge")
		}
		var ids []string
		for _, s := range st.Shares {
			ids = append(ids, s.ID)
		}
		if !slices.Equal(ids, []string{shareA2.ID, shareA.ID, shareB.ID}) {
			t.Fatalf("shares %v, want by startedAt then id", ids)
		}
		wantWatchers := map[string][]protocol.Watcher{
			shareA2.ID: {},
			shareA.ID: {
				{UserID: b.UserID, Video: protocol.VideoLayerHigh, Audio: protocol.AudioStateOff},
				{UserID: c.UserID, Video: protocol.VideoLayerLow, Audio: protocol.AudioStateOn},
			},
			shareB.ID: {{UserID: a.UserID, Video: protocol.VideoLayerLow, Audio: protocol.AudioStateOn}},
		}
		for _, s := range st.Shares {
			if !slices.Equal(s.Watchers, wantWatchers[s.ID]) {
				t.Errorf("%s watchers %+v, want %+v", s.ID, s.Watchers, wantWatchers[s.ID])
			}
		}
		if got := st.Shares[2]; got.Kind != shareB.Kind || got.Preset != shareB.Preset || !got.Audio ||
			got.Status != shareB.Status || !slices.Equal(got.Layers, shareB.Layers) || got.Codec != shareB.Codec ||
			!got.StartedAt.Equal(start) || got.ConnectionID != wb.ConnectionID {
			t.Errorf("share %+v", got)
		}

		// B's connection drops and nothing resumes it: after the grace its share ends with disconnected, then B leaves
		// (TestResume* have the grace itself).
		for _, cl := range []*signaltest.Client{a1, a2} {
			drain(t, cl)
		}
		cb.Close()
		time.Sleep(grace)
		settle()
		for _, cl := range []*signaltest.Client{a1, a2, cc} {
			evs, last := drain(t, cl)
			if len(evs) != 2 || evs[0].Kind != protocol.RoomEventKindShareStopped || evs[0].ShareID != shareB.ID ||
				evs[0].Reason != protocol.EndReasonDisconnected || evs[0].UserID != b.UserID || evs[0].Name != b.Name ||
				evs[1].Kind != protocol.RoomEventKindParticipantLeft || evs[1].UserID != b.UserID ||
				evs[1].Reason != protocol.EndReasonDisconnected {
				t.Errorf("events %+v, want share.stopped of B's share, then participant.left", evs)
			}
			if last == nil || len(last.Shares) != 2 || !slices.Equal(last.Shares[1].Watchers, []protocol.Watcher{
				{UserID: c.UserID, Video: protocol.VideoLayerLow, Audio: protocol.AudioStateOn}}) {
				t.Errorf("state after B left %+v", last)
			}
		}
		peerB := e.media.Peer(wb.ConnectionID)
		calls := peerB.Calls()
		if len(calls) != 2 || calls[0].Method != "EndShare" || calls[0].Args[0] != shareB.ID ||
			calls[0].Args[1] != protocol.EndReasonDisconnected || calls[1].Method != "Close" {
			t.Errorf("B's peer calls %+v, want EndShare(%s, disconnected), Close", calls, shareB.ID)
		}
	})
}

// room.state coalescing (01 §8.5, §19): 50 changes in 100 ms make at most 2 snapshots, the first at once (nothing
// was sent in the last 200 ms), the second at 200 ms with the final state, with a higher rev; every connection in
// the room gets byte-identical snapshots.
func TestRoomStateCoalescing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, _ := e.user(false)
		cookieB, b := e.user(false)
		cookieC, _ := e.user(false)
		ca, _ := e.connect(cookieA, signaltest.DefaultHello())
		cb, _ := e.connect(cookieB, signaltest.DefaultHello())
		cc, _ := e.connect(cookieC, signaltest.DefaultHello())
		clients := []*signaltest.Client{ca, cb, cc}
		for _, c := range clients {
			join(t, c, "lounge")
		}
		settle()
		for _, c := range clients {
			for c.Pending() > 0 {
				recv(t, c)
			}
		}

		// 50 renames of B, 2 ms apart.
		start := time.Now()
		e.hub.UpdateUser(b.UserID, "bea00", false)
		synctest.Wait()
		first := make([][]byte, len(clients))
		var rev1 uint64
		for i, c := range clients {
			f, err := c.RecvFrame(ctxT(t))
			if err != nil || f.Envelope.Type != protocol.MessageTypeRoomState {
				t.Fatalf("client %d: %s, %v; want room.state at once", i, f.Envelope.Type, err)
			}
			if time.Since(start) != 0 {
				t.Errorf("first snapshot after %v, want at once", time.Since(start))
			}
			first[i] = f.Raw
			st, _ := protocol.Decode[protocol.RoomState](f.Envelope)
			rev1 = st.Rev
			if st.Participants[1].Name != "bea00" {
				t.Errorf("first snapshot names %+v", st.Participants)
			}
		}
		for i := 1; i < 50; i++ {
			time.Sleep(2 * time.Millisecond)
			e.hub.UpdateUser(b.UserID, fmt.Sprintf("bea%02d", i), false)
		}
		time.Sleep(199*time.Millisecond - time.Since(start))
		synctest.Wait()
		for _, c := range clients {
			expectOpen(t, c) // nothing before 200 ms
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		second := make([][]byte, len(clients))
		for i, c := range clients {
			f, err := c.RecvFrame(ctxT(t))
			if err != nil || f.Envelope.Type != protocol.MessageTypeRoomState {
				t.Fatalf("client %d: %s, %v; want room.state at 200 ms", i, f.Envelope.Type, err)
			}
			second[i] = f.Raw
			st, _ := protocol.Decode[protocol.RoomState](f.Envelope)
			if st.Rev <= rev1 || st.Participants[1].Name != "bea49" {
				t.Errorf("second snapshot rev %d (first %d), names %+v", st.Rev, rev1, st.Participants)
			}
		}
		settle()
		for i, c := range clients {
			expectOpen(t, c) // at most 2 snapshots
			if !bytes.Equal(first[i], first[0]) || !bytes.Equal(second[i], second[0]) {
				t.Errorf("client %d got other bytes than client 0", i)
			}
		}

		// Byte-identical for a change by a join too.
		cookieD, _ := e.user(false)
		cd, _ := e.connect(cookieD, signaltest.DefaultHello())
		join(t, cd, "lounge")
		settle()
		var raws [][]byte
		for _, c := range clients {
			expectType(t, c, protocol.MessageTypeRoomEvent)
			f, err := c.RecvFrame(ctxT(t))
			if err != nil || f.Envelope.Type != protocol.MessageTypeRoomState {
				t.Fatalf("%s, %v; want room.state", f.Envelope.Type, err)
			}
			raws = append(raws, f.Raw)
		}
		for _, r := range raws[1:] {
			if !bytes.Equal(r, raws[0]) {
				t.Errorf("snapshots differ:\n%s\n%s", raws[0], r)
			}
		}
	})
}

// Renames and role changes reach room.state (01 §3.2, §15.2): UpdateUser and a Revalidate result with a new name.
func TestRoomRename(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		cookieB, b := e.user(false)
		ca, _ := e.connect(cookieA, signaltest.DefaultHello())
		cb, _ := e.connect(cookieB, signaltest.DefaultHello())
		join(t, ca, "lounge")
		join(t, cb, "lounge")
		settle()
		expectEvent(t, ca, protocol.RoomEventKindParticipantJoined, b.UserID)
		expectState(t, ca, "lounge")

		e.hub.UpdateUser(a.UserID, "alex", true)
		settle()
		for _, c := range []*signaltest.Client{ca, cb} {
			if st := expectState(t, c, "lounge"); st.Participants[0].Name != "alex" {
				t.Errorf("names %+v", st.Participants)
			}
			expectOpen(t, c)
		}
		// Only the admin flag: room.state doesn't show it, so nothing is sent.
		e.hub.UpdateUser(a.UserID, "alex", false)
		settle()
		expectOpen(t, ca)
		expectOpen(t, cb)

		e.auth.SetUser(b.UserID, "bea", false)
		time.Sleep(5 * time.Minute) // the revalidation tick
		settle()
		for _, c := range []*signaltest.Client{ca, cb} {
			if st := expectState(t, c, "lounge"); st.Participants[1].Name != "bea" {
				t.Errorf("names %+v", st.Participants)
			}
		}
	})
}

// Why a participant left (01 §4.2, §8.6): a deliberate close (1000, 1001) and a revocation are left, at once; a
// dropped socket and a close by the hub (idle timeout) are disconnected, once the grace has expired.
func TestRoomLeaveReasons(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieO, _ := e.user(false)
		obs, _ := e.connect(cookieO, signaltest.DefaultHello()) // answers the hub's ping frames: never idle
		join(t, obs, "lounge")
		for _, tc := range []struct {
			name   string
			noPong bool
			close  func(*signaltest.Client, signal.Identity)
			code   protocol.CloseCode // the close code the client sees; 0: not checked
			want   protocol.EndReason
		}{
			{"1000", false, func(c *signaltest.Client, _ signal.Identity) { _ = c.CloseWith(websocket.StatusNormalClosure) },
				0, protocol.EndReasonLeft},
			{"1001", false, func(c *signaltest.Client, _ signal.Identity) { _ = c.CloseWith(websocket.StatusGoingAway) },
				0, protocol.EndReasonLeft},
			{"dropped", false, func(c *signaltest.Client, _ signal.Identity) {
				c.Close()
				time.Sleep(grace)
			}, 0, protocol.EndReasonDisconnected},
			{"revoked", false, func(_ *signaltest.Client, id signal.Identity) {
				e.hub.CloseConnections(signal.ConnSelector{UserID: id.UserID}, protocol.ErrorCodeSessionRevoked)
			}, protocol.CloseCodeUnauthenticated, protocol.EndReasonLeft},
			{"account disabled", false, func(_ *signaltest.Client, id signal.Identity) {
				e.hub.CloseConnections(signal.ConnSelector{UserID: id.UserID}, protocol.ErrorCodeAccountDisabled)
			}, protocol.CloseCodeForbidden, protocol.EndReasonLeft},
			{"idle", true, func(*signaltest.Client, signal.Identity) { time.Sleep(46*time.Second + grace) },
				protocol.CloseCodeTimeout, protocol.EndReasonDisconnected},
		} {
			cookie, id := e.user(false)
			c, err := e.dial(signaltest.DialOptions{Header: headers(cookie, testOrigin, ""), NoPong: tc.noPong})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Hello(ctxT(t), signaltest.DefaultHello()); err != nil {
				t.Fatal(err)
			}
			join(t, c, "lounge")
			settle()
			expectEvent(t, obs, protocol.RoomEventKindParticipantJoined, id.UserID)
			expectState(t, obs, "lounge")

			tc.close(c, id)
			settle()
			if tc.code != 0 {
				if got, _ := c.CloseStatus(ctxT(t)); got != websocket.StatusCode(tc.code) {
					t.Errorf("%s: closed with %d, want %d", tc.name, got, tc.code)
				}
			}
			evs, last := drain(t, obs)
			if len(evs) != 1 || evs[0].Kind != protocol.RoomEventKindParticipantLeft || evs[0].UserID != id.UserID ||
				evs[0].Reason != tc.want {
				t.Errorf("%s: events %+v, want participant.left %s", tc.name, evs, tc.want)
			}
			if last == nil || len(last.Participants) != 1 {
				t.Errorf("%s: state %+v, want the observer alone", tc.name, last)
			}
		}
	})
}

// Shutdown with people in a room (01 §4.2, 04 §6.4): each connection's shares end with server_shutdown at its
// MediaPeer, which closes; the room goes with its last participant, and a room.state that is still scheduled doesn't
// hold Shutdown up.
func TestRoomShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		cookieB, b := e.user(false)
		ca, wa := e.connect(cookieA, signaltest.DefaultHello())
		cb, wb := e.connect(cookieB, signaltest.DefaultHello())
		join(t, ca, "lounge")
		join(t, cb, "lounge")
		settle()
		shareA := protocol.ShareInfo{ID: "s_aaaaaaaaaaaaaaaa", UserID: a.UserID, ConnectionID: wa.ConnectionID,
			Kind: protocol.ShareKindScreen, Preset: protocol.PresetAuto, Status: protocol.ShareStatusLive,
			StartedAt: time.Now()}
		signal.AddShare(e.hub, "lounge", shareA) // its snapshot goes out at once
		e.hub.UpdateUser(b.UserID, "bea", false) // this one is scheduled 200 ms later
		synctest.Wait()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		start := time.Now()
		if err := e.hub.Shutdown(ctx, protocol.ShutdownReasonRestart); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		if d := time.Since(start); d != 0 {
			t.Errorf("Shutdown took %v, want no wait for the scheduled room.state", d)
		}
		calls := e.media.Peer(wa.ConnectionID).Calls()
		if len(calls) != 2 || calls[0].Method != "EndShare" || calls[0].Args[0] != shareA.ID ||
			calls[0].Args[1] != protocol.EndReasonServerShutdown || calls[1].Method != "Close" {
			t.Errorf("A's peer calls %+v, want EndShare(%s, server_shutdown), Close", calls, shareA.ID)
		}
		if !e.media.Peer(wb.ConnectionID).Closed() {
			t.Error("B's peer not closed")
		}
		if snap := e.hub.Snapshot(); len(snap.Rooms) != 0 {
			t.Errorf("snapshot %+v, want no rooms", snap)
		}
		if r, n := e.metric("isshoni_rooms"), e.metric("isshoni_participants"); r != 0 || n != 0 {
			t.Errorf("rooms %v, participants %v; want 0, 0", r, n)
		}
		for _, c := range []*signaltest.Client{ca, cb} {
			expectClose(t, c, protocol.CloseCodeServiceRestart)
		}
	})
}

// CloseRoom (01 §15.2, §19): room_closed (scope room, with the room id) to that room's connections only; their
// shares end with room_closed and their MediaPeers close; no room.state or room.event follows; the room is gone from
// the snapshot, and joining it again gets room_not_found for the rest of the process.
func TestCloseRoom(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		e.rooms.Add(room1)
		cookieA, _ := e.user(false)
		cookieB, b := e.user(false)
		cookieC, _ := e.user(false)
		ca, wa := e.connect(cookieA, signaltest.DefaultHello())
		cb, wb := e.connect(cookieB, signaltest.DefaultHello())
		cc, _ := e.connect(cookieC, signaltest.DefaultHello())
		join(t, ca, room1.ID)
		join(t, cb, room1.ID)
		join(t, cc, "lounge")
		shareB := protocol.ShareInfo{ID: "s_bbbbbbbbbbbbbbbb", UserID: b.UserID, ConnectionID: wb.ConnectionID,
			Kind: protocol.ShareKindTab, Preset: protocol.PresetAuto, Status: protocol.ShareStatusLive,
			StartedAt: time.Now()}
		signal.AddShare(e.hub, room1.ID, shareB)
		settle()
		for _, c := range []*signaltest.Client{ca, cb} {
			for c.Pending() > 0 {
				recv(t, c)
			}
		}
		if snap := e.hub.Snapshot(); len(snap.Rooms) != 2 || snap.Rooms[1].ID != room1.ID || snap.Rooms[1].Name != room1.Name {
			t.Fatalf("snapshot %+v", snap)
		}

		// Two renames: the first snapshot goes out at once, the second is still scheduled when CloseRoom comes, and
		// never goes out.
		e.hub.UpdateUser(b.UserID, "bea1", false)
		e.hub.UpdateUser(b.UserID, "bea2", false)
		synctest.Wait()
		e.hub.CloseRoom(room1.ID)
		for _, c := range []*signaltest.Client{ca, cb} {
			if st := expectState(t, c, room1.ID); st.Participants[1].Name != "bea1" {
				t.Errorf("names %+v, want the first rename", st.Participants)
			}
			pe, re := expectError(t, c, protocol.ErrorCodeRoomClosed, protocol.ErrorScopeRoom)
			if pe.RoomID != room1.ID || re != "" || pe.Retryable {
				t.Errorf("room_closed %+v re %q", pe, re)
			}
		}
		settle()
		for _, c := range []*signaltest.Client{ca, cb, cc} {
			expectOpen(t, c) // no room.state, no room.event; the socket stays open
		}
		calls := e.media.Peer(wb.ConnectionID).Calls()
		if len(calls) != 2 || calls[0].Method != "EndShare" || calls[0].Args[1] != protocol.EndReasonRoomClosed ||
			calls[1].Method != "Close" {
			t.Errorf("B's peer calls %+v", calls)
		}
		if !e.media.Peer(wa.ConnectionID).Closed() {
			t.Error("A's peer not closed")
		}
		if snap := e.hub.Snapshot(); len(snap.Rooms) != 1 || snap.Rooms[0].ID != "lounge" {
			t.Errorf("snapshot %+v", snap)
		}
		if r, n := e.metric("isshoni_rooms"), e.metric("isshoni_participants"); r != 1 || n != 1 {
			t.Errorf("rooms %v, participants %v; want 1, 1", r, n)
		}
		if e.logs.count("level=INFO", "room closed", "room_id=room1", "connections=2") != 1 {
			t.Errorf("no room closed line:\n%s", e.logs)
		}

		// The directory still has the room (its delete may lag), but the hub remembers the close.
		request(t, ca, protocol.MessageTypeRoomJoin, protocol.RoomJoin{RoomID: room1.ID})
		expectError(t, ca, protocol.ErrorCodeRoomNotFound, protocol.ErrorScopeRequest)
		join(t, ca, "lounge")
		// CloseRoom of a room nobody is in only records the id.
		e.hub.CloseRoom("lounge2")
		ping(t, cb)
	})
}

// A room.join whose GetRoom returns before CloseRoom but attaches after it gets room_not_found, and no in-memory
// room is left (01 §8.4, §19).
func TestCloseRoomDuringJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		e.rooms.Add(room1)
		cookie, _ := e.user(false)
		c, _ := e.connect(cookie, signaltest.DefaultHello())

		release := e.rooms.HoldGetRoom()
		id := request(t, c, protocol.MessageTypeRoomJoin, protocol.RoomJoin{RoomID: room1.ID})
		synctest.Wait()
		if n := e.rooms.GetRoomCalls(); n != 1 {
			t.Fatalf("%d GetRoom calls, want the join's", n)
		}
		e.hub.CloseRoom(room1.ID)
		release()
		if _, re := expectError(t, c, protocol.ErrorCodeRoomNotFound, protocol.ErrorScopeRequest); re != id {
			t.Errorf("re %q", re)
		}
		synctest.Wait()
		if snap := e.hub.Snapshot(); len(snap.Rooms) != 0 {
			t.Errorf("snapshot %+v, want no rooms", snap)
		}
		if n := len(e.media.Peers()); n != 0 {
			t.Errorf("%d MediaPeers, want none", n)
		}
		if r := e.metric("isshoni_rooms"); r > 0 {
			t.Errorf("isshoni_rooms %v", r)
		}
		ping(t, c)
	})
}

// room_full (01 §8.4, §19): when Policy().MaxRoomParticipants is reached a new participant is refused with
// params.limit, a known user's further connection is not; a raised limit applies to the next join.
func TestRoomFull(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		e.policy.Set(signal.Policy{MaxRoomParticipants: 2})
		cookieA, _ := e.user(false)
		cookieB, _ := e.user(false)
		cookieC, _ := e.user(false)
		a1, _ := e.connect(cookieA, signaltest.DefaultHello())
		a2, _ := e.connect(cookieA, signaltest.DefaultHello())
		cb, _ := e.connect(cookieB, signaltest.DefaultHello())
		cc, _ := e.connect(cookieC, signaltest.DefaultHello())
		join(t, a1, "lounge")
		join(t, cb, "lounge")

		id := request(t, cc, protocol.MessageTypeRoomJoin, protocol.RoomJoin{RoomID: "lounge"})
		pe, re := expectError(t, cc, protocol.ErrorCodeRoomFull, protocol.ErrorScopeRequest)
		if re != id || !pe.Retryable || pe.Params["limit"] != float64(2) {
			t.Errorf("room_full %+v re %q", pe, re)
		}
		if st := join(t, a2, "lounge"); len(st.Participants) != 2 {
			t.Errorf("participants %v", userIDs(st))
		}
		e.policy.Set(signal.Policy{MaxRoomParticipants: 3})
		if st := join(t, cc, "lounge"); len(st.Participants) != 3 {
			t.Errorf("participants %v", userIDs(st))
		}
	})
}

// Joining another room leaves the current one (01 §4.1, §8.4): participant.left{left} there, the old MediaPeer
// closes before the new one is created, and the old room's traffic stops. The MediaSink of the current peer reaches
// the client; a previous peer's events are dropped.
func TestRoomSwitch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		e.rooms.Add(room1)
		cookieA, a := e.user(false)
		cookieB, b := e.user(false)
		ca, wa := e.connect(cookieA, signaltest.DefaultHello())
		cb, _ := e.connect(cookieB, signaltest.DefaultHello())
		join(t, ca, "lounge")
		join(t, cb, "lounge")
		settle()
		expectEvent(t, ca, protocol.RoomEventKindParticipantJoined, b.UserID)
		expectState(t, ca, "lounge")
		old := e.media.Peer(wa.ConnectionID)

		st := join(t, ca, room1.ID)
		if !slices.Equal(userIDs(st), []string{a.UserID}) {
			t.Errorf("room1 participants %v", userIDs(st))
		}
		if !old.Closed() {
			t.Error("the lounge MediaPeer is still open")
		}
		peers := e.media.Peers()
		cur := peers[len(peers)-1]
		if cur == old || cur.Params.RoomID != room1.ID || cur.Params.ConnectionID != wa.ConnectionID {
			t.Errorf("new peer %+v", cur.Params)
		}
		ev := expectEvent(t, cb, protocol.RoomEventKindParticipantLeft, a.UserID)
		if ev.Reason != protocol.EndReasonLeft || ev.RoomID != "lounge" {
			t.Errorf("participant.left %+v", ev)
		}
		e.hub.UpdateUser(b.UserID, "bea", false) // a lounge change: A no longer hears about it
		settle()
		expectState(t, cb, "lounge")
		expectOpen(t, ca)

		// Events of the previous peer are dropped; the current peer's go out.
		old.Sink().Offer(protocol.PCOffer{PC: protocol.PCKindSub, Gen: 1, Neg: 1, SDP: signaltest.FakeSDP})
		old.Sink().Error(protocol.NewError(protocol.ErrorCodeSDPInvalid, protocol.ErrorScopePC))
		synctest.Wait()
		expectOpen(t, ca)
		// The SFU may reuse its slices, maps and pointers once a call returns: what goes out is the value of the call.
		sink := cur.Sink()
		tracks := []protocol.TrackRef{{MID: "0", ShareID: "s_x", Kind: protocol.TrackKindVideo}}
		sink.Offer(protocol.PCOffer{PC: protocol.PCKindSub, Gen: 1, Neg: 1, SDP: signaltest.FakeSDP, Tracks: tracks})
		tracks[0].ShareID = "s_reused"
		cand := &protocol.ICECandidate{Candidate: "candidate:x"}
		sink.ICE(protocol.PCICE{PC: protocol.PCKindSub, Gen: 1, Candidate: cand})
		cand.Candidate = "reused"
		sink.RestartRequest(protocol.PCRestart{PC: protocol.PCKindPub, Gen: 1, Mode: protocol.RestartModeRebuild,
			Reason: protocol.RestartReasonFailed})
		subs := []protocol.SubscriptionStatus{{ShareID: "s_x", Video: protocol.VideoLayerLow,
			Audio: protocol.AudioStateOn, RequestedVideo: protocol.VideoLayerHigh, Reason: protocol.StatusReasonBandwidth}}
		sink.SubscriptionStatus(subs)
		subs[0].ShareID = "s_reused"
		encs := []protocol.Encoding{{RID: protocol.RIDHigh, Layer: protocol.VideoLayerHigh, Active: true}}
		sink.QualityHint(protocol.QualityHint{ShareID: "s_x", Reason: protocol.HintReasonViewers, Encodings: encs})
		encs[0].Active = false
		pcErr := protocol.NewError(protocol.ErrorCodeSDPInvalid, protocol.ErrorScopePC)
		pcErr.Params = map[string]any{"reason": "x"}
		sink.Error(pcErr)
		pcErr.Params["reason"] = "reused"
		sink.ShareMedia("s_x", signal.ShareMediaEvent{Kind: signal.ShareMediaLive})
		var offer protocol.PCOffer
		var ice protocol.PCICE
		var status protocol.SubscribeStatus
		var hint protocol.QualityHint
		var gotErr protocol.Error
		for _, m := range []struct {
			typ protocol.MessageType
			v   any // nil: not checked
		}{
			{protocol.MessageTypePCOffer, &offer}, {protocol.MessageTypePCICE, &ice}, {protocol.MessageTypePCRestart, nil},
			{protocol.MessageTypeSubscribeStatus, &status}, {protocol.MessageTypeQualityHint, &hint},
			{protocol.MessageTypeError, &gotErr},
		} {
			env := expectType(t, ca, m.typ)
			if m.v != nil {
				if err := json.Unmarshal(env.Data, m.v); err != nil {
					t.Fatalf("%s: %v", m.typ, err)
				}
			}
		}
		if len(offer.Tracks) != 1 || offer.Tracks[0].ShareID != "s_x" || ice.Candidate == nil ||
			ice.Candidate.Candidate == "reused" || len(status.Subs) != 1 || status.Subs[0].ShareID != "s_x" ||
			len(hint.Encodings) != 1 || !hint.Encodings[0].Active || gotErr.Params["reason"] != "x" {
			t.Errorf("sent %+v, %+v, %+v, %+v, %+v: want the values at the time of the calls",
				offer.Tracks, ice.Candidate, status.Subs, hint.Encodings, gotErr.Params)
		}
		synctest.Wait()
		expectOpen(t, ca)
		if got := e.metric("isshoni_ws_errors_total", "code", string(protocol.ErrorCodeSDPInvalid)); got != 1 {
			t.Errorf("errors sent (sdp_invalid) %v, want 1", got)
		}
		if e.logs.count("level=WARN", "share media event not handled", "share_id=s_x") != 1 {
			t.Errorf("no WARN line for ShareMedia:\n%s", e.logs)
		}

		// After room.leave, the peer that was current is a previous one too.
		leave(t, ca)
		sink.Offer(protocol.PCOffer{PC: protocol.PCKindSub, Gen: 2, Neg: 1, SDP: signaltest.FakeSDP})
		synctest.Wait()
		expectOpen(t, ca)
	})
}

// Notify with a room target reaches that room's connections only (01 §8.12).
func TestNotifyRoom(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		e.rooms.Add(room1)
		cookieA, _ := e.user(false)
		cookieB, _ := e.user(false)
		ca, _ := e.connect(cookieA, signaltest.DefaultHello())
		cb, _ := e.connect(cookieB, signaltest.DefaultHello())
		cn, _ := e.connect(cookieB, signaltest.DefaultHello()) // in no room
		join(t, ca, "lounge")
		join(t, cb, room1.ID)
		settle()

		e.hub.Notify(signal.Target{RoomID: room1.ID}, protocol.TopicRooms)
		expectInvalidate(t, cb, protocol.TopicRooms)
		ping(t, ca)
		ping(t, cn)

		// A room target and a user target: either matches.
		e.hub.Notify(signal.Target{RoomID: "lounge", UserID: "nobody"}, protocol.TopicMe)
		expectInvalidate(t, ca, protocol.TopicMe)
		ping(t, cb)

		leave(t, ca)
		e.hub.Notify(signal.Target{RoomID: "lounge"}, protocol.TopicRooms)
		ping(t, ca)
	})
}

// The live snapshot (01 §15.2): rooms by id, participants and shares in room.state order, and per connection what
// only admins see (OS, browser, version), its status, since when it is connected and its last stats.
func TestSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		e.rooms.Add(room1)
		cookieA, a := e.user(true)
		cookieB, b := e.user(false)
		since := time.Now()
		a1, wa1 := e.connect(cookieA, signaltest.DefaultHello())
		time.Sleep(time.Second)
		phone := signaltest.DefaultHello()
		phone.Role = protocol.RoleViewer
		phone.Client = protocol.ClientInfo{Kind: protocol.ClientKindWeb, Version: "0.1.0", OS: protocol.ClientOSIOS,
			Browser: "safari"}
		a2, wa2 := e.connect(cookieA, phone)
		cb, _ := e.connect(cookieB, signaltest.DefaultHello())
		join(t, a1, "lounge")
		join(t, a2, "lounge")
		join(t, cb, room1.ID)
		stats := protocol.ClientStats{IntervalMs: 10000}
		if err := a2.Send(protocol.MessageTypeStats, "", stats); err != nil {
			t.Fatal(err)
		}
		shareA := protocol.ShareInfo{ID: "s_aaaaaaaaaaaaaaaa", UserID: a.UserID, ConnectionID: wa1.ConnectionID,
			Kind: protocol.ShareKindScreen, Preset: protocol.PresetGame, Status: protocol.ShareStatusLive,
			StartedAt: time.Now()}
		signal.AddShare(e.hub, "lounge", shareA)
		signal.SetWants(e.hub, wa2.ConnectionID, protocol.SubscriptionWant{ShareID: shareA.ID,
			Video: protocol.VideoLayerHigh, Audio: protocol.AudioStateOn}) // A's own: not a watcher
		synctest.Wait()

		snap := e.hub.Snapshot()
		if len(snap.Rooms) != 2 || snap.Rooms[0].ID != "lounge" || snap.Rooms[0].Name != "Lounge" ||
			snap.Rooms[1].ID != room1.ID || snap.Rooms[1].Name != room1.Name {
			t.Fatalf("rooms %+v", snap.Rooms)
		}
		lounge := snap.Rooms[0]
		if len(lounge.Participants) != 1 || lounge.Participants[0].UserID != a.UserID ||
			lounge.Participants[0].Name != a.Name {
			t.Fatalf("lounge participants %+v", lounge.Participants)
		}
		conns := lounge.Participants[0].Connections
		if len(conns) != 2 {
			t.Fatalf("connections %+v", conns)
		}
		c1, c2 := conns[0], conns[1]
		if c1.ID != wa1.ConnectionID || c1.Kind != protocol.ClientKindWeb || c1.Role != protocol.RoleFull ||
			c1.OS != protocol.ClientOSMacOS || c1.Browser != "chrome" || c1.Version != "0.1.0" ||
			c1.Status != protocol.ConnectionStatusOnline || !c1.Since.Equal(since) || c1.LastStats != nil {
			t.Errorf("connection 1 %+v", c1)
		}
		if c2.ID != wa2.ConnectionID || c2.Role != protocol.RoleViewer || c2.OS != protocol.ClientOSIOS ||
			c2.Browser != "safari" || !c2.Since.Equal(since.Add(time.Second)) || c2.LastStats == nil ||
			c2.LastStats.IntervalMs != 10000 {
			t.Errorf("connection 2 %+v", c2)
		}
		// shareA has no layers yet: still [], not nil.
		if len(lounge.Shares) != 1 || lounge.Shares[0].Info.ID != shareA.ID || lounge.Shares[0].Info.Watchers == nil ||
			len(lounge.Shares[0].Info.Watchers) != 0 || lounge.Shares[0].Info.Layers == nil ||
			len(lounge.Shares[0].Info.Layers) != 0 || lounge.Shares[0].Layers == nil {
			t.Errorf("lounge shares %+v", lounge.Shares)
		}
		if r := snap.Rooms[1]; len(r.Participants) != 1 || r.Participants[0].UserID != b.UserID || r.Shares == nil {
			t.Errorf("room1 %+v", r)
		}

		// A rename in the directory reaches the snapshot with the next room.join, the same room's included.
		renamed := protocol.RoomInfo{ID: room1.ID, Name: "Film night"}
		e.rooms.Add(renamed)
		join(t, cb, room1.ID)
		if snap := e.hub.Snapshot(); len(snap.Rooms) != 2 || snap.Rooms[1].Name != renamed.Name {
			t.Errorf("rooms %+v after a re-join, want room1 named %q", snap.Rooms, renamed.Name)
		}
	})
}
