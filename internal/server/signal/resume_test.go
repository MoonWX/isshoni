package signal_test

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/signal"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// Resume and grace (01 §4.2, §10.3, §10.5, §19 "Resume"): a connection outlives its socket for the 30 s grace, a
// hello with its resume token moves it to a new socket, and nobody sees a participant.left or participant.joined
// for it. The tests run the real 30 s on synctest's fake clock.

// grace is the resume grace of DefaultConfig (30 s).
var grace = signal.DefaultConfig().Grace

// resumeHello is a web client's hello with a resume token.
func resumeHello(tok protocol.Secret) protocol.Hello {
	h := signaltest.DefaultHello()
	h.ResumeToken = tok
	return h
}

// resume opens a new socket with the cookie and says hello with the resume token. It returns the welcome, resumed
// or not.
func (e *env) resume(cookie string, tok protocol.Secret) (*signaltest.Client, protocol.Welcome) {
	e.t.Helper()
	return e.connect(cookie, resumeHello(tok))
}

// liveShare is a live share of userID, published through connID.
func liveShare(id, userID, connID string) protocol.ShareInfo {
	return protocol.ShareInfo{ID: id, UserID: userID, ConnectionID: connID, Kind: protocol.ShareKindScreen,
		Preset: protocol.PresetMovie, Audio: true, Status: protocol.ShareStatusLive,
		Layers: []protocol.VideoLayer{protocol.VideoLayerHigh, protocol.VideoLayerLow},
		Codec:  protocol.CodecH264High, StartedAt: time.Now()}
}

// participantOf returns userID's participant in st.
func participantOf(t *testing.T, st *protocol.RoomState, userID string) protocol.ParticipantInfo {
	t.Helper()
	if st == nil {
		t.Fatalf("no room.state, want one with user %s", userID)
	}
	for _, p := range st.Participants {
		if p.UserID == userID {
			return p
		}
	}
	t.Fatalf("user %s is not in room.state: %+v", userID, st.Participants)
	return protocol.ParticipantInfo{}
}

// statuses returns a participant's status and the statuses of its connections, in order.
func statuses(p protocol.ParticipantInfo) (protocol.ParticipantStatus, []protocol.ConnectionStatus) {
	var cs []protocol.ConnectionStatus
	for _, c := range p.Connections {
		cs = append(cs, c.Status)
	}
	return p.Status, cs
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const (
	online       = protocol.ConnectionStatusOnline
	reconnecting = protocol.ConnectionStatusReconnecting
)

// The socket is killed and the client comes back within the grace (01 §19): the connection is detached, then
// resumed: true with the same connectionId and a new token; its shares are unchanged; its participant is
// reconnecting, then present; nobody gets a leave or join event; Resync is called once.
func TestResumeWithinGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		cookieO, o := e.user(false)
		ca, wa := e.connect(cookieA, signaltest.DefaultHello())
		obs, wo := e.connect(cookieO, signaltest.DefaultHello())
		join(t, ca, "lounge")
		join(t, obs, "lounge")
		share := liveShare("s_aaaaaaaaaaaaaaaa", a.UserID, wa.ConnectionID)
		signal.AddShare(e.hub, "lounge", share)
		signal.SetWants(e.hub, wo.ConnectionID, protocol.SubscriptionWant{ShareID: share.ID,
			Video: protocol.VideoLayerHigh, Audio: protocol.AudioStateOn})
		settle()
		drain(t, ca)
		_, before := drain(t, obs)
		if before == nil || len(before.Shares) != 1 || len(before.Shares[0].Watchers) != 1 {
			t.Fatalf("state before the drop %+v, want A's share with one watcher", before)
		}
		peer := e.media.Peer(wa.ConnectionID)

		// The socket is killed: the connection is detached and keeps everything.
		ca.Close()
		settle()
		evs, st := drain(t, obs)
		if len(evs) != 0 {
			t.Errorf("events after the drop %+v, want none", evs)
		}
		if ps, cs := statuses(participantOf(t, st, a.UserID)); ps != protocol.ParticipantStatusReconnecting ||
			!slices.Equal(cs, []protocol.ConnectionStatus{reconnecting}) {
			t.Errorf("A is %s with connections %v, want reconnecting", ps, cs)
		}
		if ps, _ := statuses(participantOf(t, st, o.UserID)); ps != protocol.ParticipantStatusPresent {
			t.Errorf("the observer is %s", ps)
		}
		if st.Rev <= before.Rev || jsonOf(t, st.Shares) != jsonOf(t, before.Shares) {
			t.Errorf("state after the drop: rev %d (before %d), shares %s; want a new rev and the shares unchanged",
				st.Rev, before.Rev, jsonOf(t, st.Shares))
		}
		snap := e.hub.Snapshot()
		if got := snap.Rooms[0].Participants[0].Connections[0]; got.ID != wa.ConnectionID || got.Status != reconnecting {
			t.Errorf("snapshot connection %+v, want A's, reconnecting", got)
		}
		if conns, socks, _, slots := signal.Counts(e.hub, a.UserID); conns != 2 || socks != 1 || slots != 1 {
			t.Errorf("conns %d, sockets %d, A's slots %d while detached; want 2, 1, 1", conns, socks, slots)
		}
		if got := e.metric("isshoni_ws_connections", "kind", "web", "role", "full"); got != 2 {
			t.Errorf("isshoni_ws_connections %v while detached, want 2", got)
		}
		if e.logs.count("level=INFO", "connection detached", wa.ConnectionID, "user_id="+a.UserID, "code=1006") != 1 {
			t.Errorf("no detach line:\n%s", e.logs)
		}
		if calls := peer.Calls(); len(calls) != 0 {
			t.Errorf("MediaPeer calls while detached %+v, want none", calls)
		}

		// Late in the grace the client is back, on a new socket and from another address.
		time.Sleep(grace - time.Second - time.Millisecond)
		c2 := e.mustDial(headers(cookieA, testOrigin, "198.51.100.7"))
		w2, err := c2.Hello(ctxT(t), resumeHello(wa.ResumeToken))
		if err != nil {
			t.Fatalf("hello: %v", err)
		}
		if !w2.Resumed || w2.ConnectionID != wa.ConnectionID || w2.RoomID != "lounge" {
			t.Fatalf("welcome resumed %v, connectionId %s (want %s), roomId %q", w2.Resumed, w2.ConnectionID,
				wa.ConnectionID, w2.RoomID)
		}
		if w2.ResumeToken == wa.ResumeToken || !resumeTokenRE.MatchString(w2.ResumeToken.Reveal()) {
			t.Error("the resumed welcome has no new resume token")
		}
		if w2.User != (protocol.UserInfo{ID: a.UserID, Name: a.Name}) || w2.DefaultRoomID != "lounge" ||
			!w2.ServerTime.Equal(time.Now()) || w2.Limits.GraceMs != 30000 {
			t.Errorf("welcome %+v", w2)
		}
		// room.state follows the welcome at once, for this connection alone (01 §10.5).
		own := expectState(t, c2, "lounge")
		if ps, cs := statuses(participantOf(t, &own, a.UserID)); ps != protocol.ParticipantStatusPresent ||
			!slices.Equal(cs, []protocol.ConnectionStatus{online}) {
			t.Errorf("A is %s with connections %v after the resume, want present", ps, cs)
		}
		if jsonOf(t, own.Shares) != jsonOf(t, before.Shares) {
			t.Errorf("shares after the resume %s, want them unchanged: %s", jsonOf(t, own.Shares), jsonOf(t, before.Shares))
		}
		settle()
		expectOpen(t, c2) // nothing else: no events, and no second copy of the state
		evs, st = drain(t, obs)
		if len(evs) != 0 {
			t.Errorf("events after the resume %+v, want none", evs)
		}
		if ps, cs := statuses(participantOf(t, st, a.UserID)); ps != protocol.ParticipantStatusPresent ||
			!slices.Equal(cs, []protocol.ConnectionStatus{online}) || st.Rev != own.Rev {
			t.Errorf("the observer sees A %s with %v at rev %d, want present at rev %d", ps, cs, st.Rev, own.Rev)
		}

		// The same MediaPeer, resynced once; nothing ended, nothing created.
		calls := peer.Calls()
		if len(calls) != 1 || calls[0].Method != "Resync" {
			t.Errorf("MediaPeer calls %+v, want one Resync", calls)
		}
		if n := len(e.media.Peers()); n != 2 || peer.Closed() {
			t.Errorf("%d MediaPeers (closed %v), want the two from the joins", n, peer.Closed())
		}
		if conns, socks, _, slots := signal.Counts(e.hub, a.UserID); conns != 2 || socks != 2 || slots != 1 {
			t.Errorf("conns %d, sockets %d, A's slots %d after the resume; want 2, 2, 1", conns, socks, slots)
		}
		if r, n := e.metric("isshoni_ws_resume_total", "result", "resumed"),
			e.metric("isshoni_ws_resume_total", "result", "not_resumed"); r != 1 || n > 0 {
			t.Errorf("isshoni_ws_resume_total resumed %v, not_resumed %v; want 1 and none", r, n)
		}
		if e.logs.count("level=INFO", "connection resumed", wa.ConnectionID, "user_id="+a.UserID, "resumed=true") != 1 {
			t.Errorf("no resume line:\n%s", e.logs)
		}
		if e.logs.count("198.51.100.7") != 0 {
			t.Errorf("a log line has the client IP:\n%s", e.logs)
		}
		if e.logs.count(wa.ResumeToken.Reveal()) != 0 || e.logs.count(w2.ResumeToken.Reveal()) != 0 {
			t.Errorf("a log line has a resume token:\n%s", e.logs)
		}

		// The connection lives on past the old deadline, and its peer's events reach the new socket.
		time.Sleep(time.Minute)
		synctest.Wait()
		expectOpen(t, obs)
		peer.Sink().Offer(protocol.PCOffer{PC: protocol.PCKindSub, Gen: 1, Neg: 1, SDP: signaltest.FakeSDP})
		expectType(t, c2, protocol.MessageTypePCOffer)
		ping(t, c2)
	})
}

// Nothing resumes the connection (01 §19): the grace lasts exactly 30 s, then the connection closes: share.stopped
// and participant.left with disconnected, EndShare and Close at its MediaPeer. Its token then resumes nothing.
func TestResumeGraceExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		cookieO, o := e.user(false)
		ca, wa := e.connect(cookieA, signaltest.DefaultHello())
		obs, _ := e.connect(cookieO, signaltest.DefaultHello())
		join(t, ca, "lounge")
		join(t, obs, "lounge")
		share := liveShare("s_aaaaaaaaaaaaaaaa", a.UserID, wa.ConnectionID)
		signal.AddShare(e.hub, "lounge", share)
		settle()
		drain(t, obs)
		peer := e.media.Peer(wa.ConnectionID)

		ca.Close()
		synctest.Wait()
		time.Sleep(grace - time.Millisecond)
		synctest.Wait()
		evs, st := drain(t, obs)
		if ps, _ := statuses(participantOf(t, st, a.UserID)); len(evs) != 0 || ps != protocol.ParticipantStatusReconnecting ||
			len(st.Shares) != 1 {
			t.Errorf("1 ms before the grace ends: events %+v, A %s, %d shares; want A reconnecting with its share",
				evs, ps, len(st.Shares))
		}
		if calls := peer.Calls(); len(calls) != 0 {
			t.Errorf("MediaPeer calls within the grace %+v", calls)
		}

		time.Sleep(time.Millisecond)
		synctest.Wait()
		evs, st = drain(t, obs)
		if len(evs) != 2 || evs[0].Kind != protocol.RoomEventKindShareStopped || evs[0].ShareID != share.ID ||
			evs[0].UserID != a.UserID || evs[0].Name != a.Name || evs[0].Reason != protocol.EndReasonDisconnected ||
			evs[1].Kind != protocol.RoomEventKindParticipantLeft || evs[1].UserID != a.UserID ||
			evs[1].Reason != protocol.EndReasonDisconnected || !evs[1].At.Equal(time.Now()) {
			t.Errorf("events at the end of the grace %+v, want share.stopped and participant.left, both disconnected", evs)
		}
		if st == nil || !slices.Equal(userIDs(*st), []string{o.UserID}) || len(st.Shares) != 0 {
			t.Errorf("state after the grace %+v, want the observer alone", st)
		}
		calls := peer.Calls()
		if len(calls) != 2 || calls[0].Method != "EndShare" || calls[0].Args[0] != share.ID ||
			calls[0].Args[1] != protocol.EndReasonDisconnected || calls[1].Method != "Close" {
			t.Errorf("MediaPeer calls %+v, want EndShare(%s, disconnected), Close", calls, share.ID)
		}
		if conns, socks, _, slots := signal.Counts(e.hub, a.UserID); conns != 1 || socks != 1 || slots != 0 {
			t.Errorf("conns %d, sockets %d, A's slots %d after the grace; want 1, 1, 0", conns, socks, slots)
		}
		if e.logs.count("level=INFO", "connection closed", wa.ConnectionID, "code=1006", "reason=disconnected") != 1 {
			t.Errorf("no close line:\n%s", e.logs)
		}
		if c, n := e.metric("isshoni_ws_connections", "kind", "web", "role", "full"),
			e.metric("isshoni_ws_close_total", "code", "1006"); c != 1 || n != 1 {
			t.Errorf("isshoni_ws_connections %v, isshoni_ws_close_total{1006} %v; want 1 and 1", c, n)
		}

		// Too late: the token resumes nothing. That is no error: a new connection, in no room.
		c2, w2 := e.resume(cookieA, wa.ResumeToken)
		if w2.Resumed || w2.ConnectionID == wa.ConnectionID || w2.RoomID != "" {
			t.Errorf("welcome after the grace: resumed %v, same id %v, roomId %q", w2.Resumed,
				w2.ConnectionID == wa.ConnectionID, w2.RoomID)
		}
		if got := e.metric("isshoni_ws_resume_total", "result", "not_resumed"); got != 1 {
			t.Errorf("isshoni_ws_resume_total{not_resumed} %v, want 1", got)
		}
		if e.logs.count("level=DEBUG", "resume token not resumed", "user_id="+a.UserID, "reason=unknown_connection") != 1 ||
			e.logs.count(wa.ResumeToken.Reveal()) != 0 {
			t.Errorf("want one DEBUG line with the reason and without the token:\n%s", e.logs)
		}
		ping(t, c2) // no room.state before the pong
		if e.logs.count("level=INFO", "connection opened", w2.ConnectionID, "resumed=false") != 1 {
			t.Errorf("no open line for the new connection:\n%s", e.logs)
		}
	})
}

// Every welcome rotates the token (01 §10.3, §19): an old token of the same connection resumes nothing, the current
// one does.
func TestResumeTokenRotation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		c1, w1 := e.connect(cookie, signaltest.DefaultHello())
		c1.Close()
		synctest.Wait()
		c2, w2 := e.resume(cookie, w1.ResumeToken)
		if !w2.Resumed || w2.ConnectionID != w1.ConnectionID || w2.ResumeToken == w1.ResumeToken {
			t.Fatalf("first resume: resumed %v, connectionId %s (want %s), same token %v", w2.Resumed,
				w2.ConnectionID, w1.ConnectionID, w2.ResumeToken == w1.ResumeToken)
		}
		c2.Close()
		synctest.Wait()

		// The first token again: the resumed welcome rotated it.
		c3, w3 := e.resume(cookie, w1.ResumeToken)
		if w3.Resumed || w3.ConnectionID == w1.ConnectionID {
			t.Errorf("the old token: resumed %v, same connection %v; want a new connection", w3.Resumed,
				w3.ConnectionID == w1.ConnectionID)
		}
		if e.logs.count("level=DEBUG", "resume token not resumed", "reason=rotated_token") != 1 {
			t.Errorf("no DEBUG line with reason=rotated_token:\n%s", e.logs)
		}
		// That did not touch the connection: it is still detached, and its current token resumes it.
		c4, w4 := e.resume(cookie, w2.ResumeToken)
		if !w4.Resumed || w4.ConnectionID != w1.ConnectionID || w4.ResumeToken == w2.ResumeToken {
			t.Errorf("the current token: resumed %v, connectionId %s (want %s)", w4.Resumed, w4.ConnectionID,
				w1.ConnectionID)
		}
		ping(t, c3)
		ping(t, c4)
		if r, n := e.metric("isshoni_ws_resume_total", "result", "resumed"),
			e.metric("isshoni_ws_resume_total", "result", "not_resumed"); r != 2 || n != 1 {
			t.Errorf("isshoni_ws_resume_total resumed %v, not_resumed %v; want 2 and 1", r, n)
		}
	})
}

// A token is not a credential (01 §3.2, §10.3, §19): another user's token resumes nothing, nor does the same user's
// from another session or device. The attempts don't touch the connection.
func TestResumeIdentity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, withWails)
		defer e.close()
		cookieA, a := e.user(false)
		cookieA2 := e.session(a, "sessionA2")
		cookieB, b := e.user(false)
		dev := a
		dev.SessionID, dev.DeviceID = "", "device01"
		e.auth.AddBearer("token01", dev)
		bearer := func(tok protocol.Secret) protocol.Hello {
			h := resumeHello(tok)
			h.Auth = &protocol.HelloAuth{Scheme: protocol.AuthSchemeBearer, Token: "token01"}
			return h
		}

		ca, wa := e.connect(cookieA, signaltest.DefaultHello())
		ca.Close()
		synctest.Wait()
		for _, tc := range []struct {
			name   string
			cookie string
			hello  protocol.Hello
			user   string
		}{
			{"another user", cookieB, resumeHello(wa.ResumeToken), b.UserID},
			{"the user's other session", cookieA2, resumeHello(wa.ResumeToken), a.UserID},
			{"the user's device", "", bearer(wa.ResumeToken), a.UserID},
		} {
			c := e.mustDial(headers(tc.cookie, "", ""))
			w, err := c.Hello(ctxT(t), tc.hello)
			if err != nil {
				t.Fatalf("%s: hello: %v", tc.name, err)
			}
			if w.Resumed || w.ConnectionID == wa.ConnectionID || w.User.ID != tc.user {
				t.Errorf("%s: resumed %v, A's connection %v, user %s; want a connection of its own", tc.name,
					w.Resumed, w.ConnectionID == wa.ConnectionID, w.User.ID)
			}
		}
		// The session that owns the connection resumes it, still with the same token.
		c, w := e.resume(cookieA, wa.ResumeToken)
		if !w.Resumed || w.ConnectionID != wa.ConnectionID {
			t.Errorf("the owner: resumed %v, connectionId %s; want %s resumed", w.Resumed, w.ConnectionID, wa.ConnectionID)
		}
		ping(t, c)

		// A bearer connection belongs to its device: the user's cookie session does not resume it, the device does.
		cd := e.mustDial(headers("", wailsOrigin, ""))
		wd, err := cd.Hello(ctxT(t), bearer(""))
		if err != nil {
			t.Fatalf("bearer hello: %v", err)
		}
		cd.Close()
		synctest.Wait()
		if _, w := e.resume(cookieA, wd.ResumeToken); w.Resumed || w.ConnectionID == wd.ConnectionID {
			t.Errorf("a cookie session resumed the device's connection")
		}
		cd2 := e.mustDial(headers("", wailsOrigin, ""))
		wd2, err := cd2.Hello(ctxT(t), bearer(wd.ResumeToken))
		if err != nil || !wd2.Resumed || wd2.ConnectionID != wd.ConnectionID {
			t.Errorf("the device: %v, resumed %v, connectionId %s; want %s resumed", err, wd2.Resumed,
				wd2.ConnectionID, wd.ConnectionID)
		}
		if n := e.logs.count("level=DEBUG", "resume token not resumed", "reason=other_identity"); n != 4 {
			t.Errorf("%d DEBUG lines with reason=other_identity, want 4:\n%s", n, e.logs)
		}
	})
}

// Resume while the old socket is still attached (01 §10.3, §19): the old socket gets replaced and 4409, the new one
// has the connection, and the room sees nothing at all, because the connection was never detached.
func TestResumeReplaced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		cookieO, _ := e.user(false)
		old, wa := e.connect(cookieA, signaltest.DefaultHello())
		obs, _ := e.connect(cookieO, signaltest.DefaultHello())
		join(t, old, "lounge")
		join(t, obs, "lounge")
		settle()
		drain(t, old)
		drain(t, obs)
		peer := e.media.Peer(wa.ConnectionID)

		c2, w2 := e.resume(cookieA, wa.ResumeToken)
		if !w2.Resumed || w2.ConnectionID != wa.ConnectionID || w2.RoomID != "lounge" {
			t.Fatalf("welcome resumed %v, connectionId %s (want %s), roomId %q", w2.Resumed, w2.ConnectionID,
				wa.ConnectionID, w2.RoomID)
		}
		st := expectState(t, c2, "lounge")
		if ps, cs := statuses(participantOf(t, &st, a.UserID)); ps != protocol.ParticipantStatusPresent ||
			!slices.Equal(cs, []protocol.ConnectionStatus{online}) {
			t.Errorf("A is %s with connections %v, want present", ps, cs)
		}
		pe := expectFail(t, old, protocol.ErrorCodeReplaced, protocol.ErrorScopeConnection)
		if pe.Retryable {
			t.Error("replaced is retryable")
		}
		settle()
		expectOpen(t, obs)
		expectOpen(t, c2)
		if calls := peer.Calls(); len(calls) != 1 || calls[0].Method != "Resync" {
			t.Errorf("MediaPeer calls %+v, want one Resync", calls)
		}
		if conns, socks, _, slots := signal.Counts(e.hub, a.UserID); conns != 2 || socks != 2 || slots != 1 {
			t.Errorf("conns %d, sockets %d, A's slots %d; want 2, 2, 1", conns, socks, slots)
		}
		if c, n := e.metric("isshoni_ws_close_total", "code", "4409"),
			e.metric("isshoni_ws_errors_total", "code", string(protocol.ErrorCodeReplaced)); c != 1 || n != 1 {
			t.Errorf("isshoni_ws_close_total{4409} %v, isshoni_ws_errors_total{replaced} %v; want 1 and 1", c, n)
		}

		// The end of the old socket is not the end of the connection, and starts no grace.
		time.Sleep(grace + time.Second)
		synctest.Wait()
		expectOpen(t, obs)
		ping(t, c2)
		if e.logs.count("connection detached") != 0 || e.logs.count("connection closed") != 0 {
			t.Errorf("the connection was detached or closed:\n%s", e.logs)
		}
	})
}

// A half-open old socket, whose client is gone and reads nothing, doesn't hold a resume up: the new socket has the
// connection at once, and the old one is given up when its close handshake times out.
func TestResumeReplacedHalfOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, id := e.user(false)
		old, w := e.connect(cookie, signaltest.DefaultHello())
		old.Pause()

		start := time.Now()
		c2, w2 := e.resume(cookie, w.ResumeToken)
		if !w2.Resumed || w2.ConnectionID != w.ConnectionID || time.Since(start) != 0 {
			t.Fatalf("resumed %v as %s after %v; want %s at once", w2.Resumed, w2.ConnectionID, time.Since(start),
				w.ConnectionID)
		}
		ping(t, c2)
		time.Sleep(11 * time.Second)
		synctest.Wait()
		if conns, socks, _, slots := signal.Counts(e.hub, id.UserID); conns != 1 || socks != 1 || slots != 1 {
			t.Errorf("conns %d, sockets %d, slots %d; want 1, 1, 1 (the old socket is gone)", conns, socks, slots)
		}
		ping(t, c2)
	})
}

// The deliberate-close fast path (01 §4.2, §19): a client that closes with 1000 or 1001 left on purpose. There is no
// grace: its share and its participant go at once, with left, and its token resumes nothing. Any other close code
// from the client keeps the grace, the web client's 4000 for a socket it replaces included.
func TestResumeDeliberateClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieO, _ := e.user(false)
		obs, _ := e.connect(cookieO, signaltest.DefaultHello())
		join(t, obs, "lounge")
		for _, code := range []websocket.StatusCode{websocket.StatusNormalClosure, websocket.StatusGoingAway, 4000} {
			cookie, id := e.user(false)
			c, w := e.connect(cookie, signaltest.DefaultHello())
			join(t, c, "lounge")
			share := liveShare("s_aaaaaaaaaaaaaaaa", id.UserID, w.ConnectionID)
			signal.AddShare(e.hub, "lounge", share)
			settle()
			drain(t, obs)
			peer := e.media.Peer(w.ConnectionID)

			closed := time.Now()
			if err := c.CloseWith(code); err != nil {
				t.Fatalf("%d: close: %v", code, err)
			}
			settle()
			evs, st := drain(t, obs)
			if code == 4000 {
				if ps, _ := statuses(participantOf(t, st, id.UserID)); len(evs) != 0 ||
					ps != protocol.ParticipantStatusReconnecting {
					t.Errorf("%d: events %+v, participant %s; want the grace", code, evs, ps)
				}
				c2, w2 := e.resume(cookie, w.ResumeToken)
				if !w2.Resumed || w2.ConnectionID != w.ConnectionID {
					t.Errorf("%d: resumed %v, want the connection back", code, w2.Resumed)
				}
				_ = c2.CloseWith(websocket.StatusNormalClosure)
				settle()
				drain(t, obs)
				continue
			}
			if len(evs) != 2 || evs[0].Kind != protocol.RoomEventKindShareStopped ||
				evs[0].Reason != protocol.EndReasonLeft || evs[1].Kind != protocol.RoomEventKindParticipantLeft ||
				evs[1].UserID != id.UserID || evs[1].Reason != protocol.EndReasonLeft || !evs[1].At.Equal(closed) {
				t.Errorf("%d: events %+v, want share.stopped and participant.left with left, at once", code, evs)
			}
			if len(st.Participants) != 1 {
				t.Errorf("%d: participants %v, want the observer alone", code, userIDs(*st))
			}
			calls := peer.Calls()
			if len(calls) != 2 || calls[0].Method != "EndShare" || calls[0].Args[1] != protocol.EndReasonLeft ||
				calls[1].Method != "Close" {
				t.Errorf("%d: MediaPeer calls %+v, want EndShare(left), Close", code, calls)
			}
			if conns, _, _, slots := signal.Counts(e.hub, id.UserID); conns != 1 || slots != 0 {
				t.Errorf("%d: conns %d, slots %d; want 1, 0", code, conns, slots)
			}
			if e.logs.count("connection detached", w.ConnectionID) != 0 ||
				e.logs.count("connection closed", w.ConnectionID, "reason=left") != 1 {
				t.Errorf("%d: want a close line with reason=left and no detach line:\n%s", code, e.logs)
			}
			c2, w2 := e.resume(cookie, w.ResumeToken)
			if w2.Resumed || w2.ConnectionID == w.ConnectionID {
				t.Errorf("%d: the token resumed after a deliberate close", code)
			}
			_ = c2.CloseWith(websocket.StatusNormalClosure)
			synctest.Wait()
		}
	})
}

// Which closes by the hub keep the grace (01 §4.2, §12.2): a retryable one does (the idle timeout), and the client
// resumes; after an error that makes the client stop (bad_message, a second hello, a binary frame) the connection
// ends at once.
func TestResumeAfterHubClose(t *testing.T) {
	for _, tc := range []struct {
		name   string
		noPong bool
		end    func(t *testing.T, c *signaltest.Client)
		grace  bool
	}{
		{"idle timeout", true, func(t *testing.T, c *signaltest.Client) {
			time.Sleep(45 * time.Second)
			expectFail(t, c, protocol.ErrorCodeIdleTimeout, protocol.ErrorScopeConnection)
		}, true},
		{"bad message", false, func(t *testing.T, c *signaltest.Client) {
			if err := c.SendRaw([]byte("nope")); err != nil {
				t.Fatal(err)
			}
			expectFail(t, c, protocol.ErrorCodeBadMessage, protocol.ErrorScopeConnection)
		}, false},
		{"second hello", false, func(t *testing.T, c *signaltest.Client) {
			request(t, c, protocol.MessageTypeHello, signaltest.DefaultHello())
			expectFail(t, c, protocol.ErrorCodeBadRequest, protocol.ErrorScopeConnection)
		}, false},
		{"binary frame", false, func(t *testing.T, c *signaltest.Client) {
			if err := c.SendBinary([]byte("{}")); err != nil {
				t.Fatal(err)
			}
			expectClose(t, c, protocol.CloseCodeUnsupportedData)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t)
				defer e.close()
				cookieO, _ := e.user(false)
				obs, _ := e.connect(cookieO, signaltest.DefaultHello())
				join(t, obs, "lounge")
				cookie, id := e.user(false)
				c, err := e.dial(signaltest.DialOptions{Header: headers(cookie, testOrigin, ""), NoPong: tc.noPong})
				if err != nil {
					t.Fatal(err)
				}
				w, err := c.Hello(ctxT(t), signaltest.DefaultHello())
				if err != nil {
					t.Fatal(err)
				}
				join(t, c, "lounge")
				settle()
				drain(t, obs)

				tc.end(t, c)
				ended := time.Now()
				settle()
				evs, st := drain(t, obs)
				c2, w2 := e.resume(cookie, w.ResumeToken)
				if !tc.grace {
					if len(evs) != 1 || evs[0].Kind != protocol.RoomEventKindParticipantLeft ||
						evs[0].UserID != id.UserID || evs[0].Reason != protocol.EndReasonDisconnected ||
						!evs[0].At.Equal(ended) {
						t.Errorf("events %+v, want participant.left{disconnected} at once", evs)
					}
					if w2.Resumed {
						t.Error("the token resumed a connection that the hub closed for good")
					}
					return
				}
				if ps, _ := statuses(participantOf(t, st, id.UserID)); len(evs) != 0 ||
					ps != protocol.ParticipantStatusReconnecting {
					t.Errorf("events %+v, participant %s; want the grace", evs, ps)
				}
				if !w2.Resumed || w2.ConnectionID != w.ConnectionID {
					t.Errorf("resumed %v, want the connection back", w2.Resumed)
				}
				expectState(t, c2, "lounge")
				ping(t, c2)
			})
		})
	}
}

// The idle timeout runs again on the socket a connection resumed on.
func TestResumeIdleAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		silent := signaltest.DialOptions{Header: headers(cookie, testOrigin, ""), NoPong: true}
		c, err := e.dial(silent)
		if err != nil {
			t.Fatal(err)
		}
		w, err := c.Hello(ctxT(t), signaltest.DefaultHello())
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(45 * time.Second)
		expectFail(t, c, protocol.ErrorCodeIdleTimeout, protocol.ErrorScopeConnection)
		time.Sleep(10 * time.Second)

		c2, err := e.dial(silent)
		if err != nil {
			t.Fatal(err)
		}
		w2, err := c2.Hello(ctxT(t), resumeHello(w.ResumeToken))
		if err != nil || !w2.Resumed {
			t.Fatalf("resume: %v, resumed %v", err, w2.Resumed)
		}
		time.Sleep(45*time.Second - time.Millisecond)
		synctest.Wait()
		expectOpen(t, c2)
		time.Sleep(time.Millisecond)
		expectFail(t, c2, protocol.ErrorCodeIdleTimeout, protocol.ErrorScopeConnection)
	})
}

// Resume-key rotation (01 §10.3): tokens are bound to Config.ResumeKey, so `isshoni admin rotate-secrets`, which
// restarts the server with a new key, invalidates all of them. A token from before a restart with the same key is
// genuine but names a connection that is gone. Garbage and forgeries are dropped the same way. Each hello gets
// resumed: false and a connection of its own.
func TestResumeKeyRotation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		withKey := func(b byte) func(*signal.Config) {
			return func(c *signal.Config) { c.ResumeKey = bytes.Repeat([]byte{b}, 32) }
		}
		first := newEnv(t, withKey(1))
		cookie, _ := first.user(false)
		_, w1 := first.connect(cookie, signaltest.DefaultHello())
		first.close()

		for _, tc := range []struct {
			name string
			key  byte
			why  string // the DEBUG line's reason for the first process's token
		}{
			{"restart with the same key", 1, "reason=unknown_connection"},
			{"restart with a rotated key", 2, "reason=invalid_token"},
		} {
			e := newEnv(t, withKey(tc.key))
			cookie, _ := e.user(false) // the same user and session as in the first process
			c, w := e.resume(cookie, w1.ResumeToken)
			if w.Resumed || w.ConnectionID == w1.ConnectionID || w.ResumeToken == w1.ResumeToken {
				t.Errorf("%s: resumed %v, same connection %v; want a new connection", tc.name, w.Resumed,
					w.ConnectionID == w1.ConnectionID)
			}
			if e.logs.count("level=DEBUG", "resume token not resumed", tc.why) != 1 {
				t.Errorf("%s: no DEBUG line with %s:\n%s", tc.name, tc.why, e.logs)
			}
			ping(t, c)
			// The new process's own tokens work, under either key.
			c.Close()
			synctest.Wait()
			if _, w2 := e.resume(cookie, w.ResumeToken); !w2.Resumed || w2.ConnectionID != w.ConnectionID {
				t.Errorf("%s: its own token: resumed %v", tc.name, w2.Resumed)
			}
			for _, tok := range []protocol.Secret{"garbage", "r1.", "r1." + protocol.Secret(bytes.Repeat([]byte{'A'}, 56)),
				w.ResumeToken + "x"} {
				if _, wg := e.resume(cookie, tok); wg.Resumed {
					t.Errorf("%s: token %q resumed", tc.name, tok.Reveal())
				}
			}
			if r, n := e.metric("isshoni_ws_resume_total", "result", "resumed"),
				e.metric("isshoni_ws_resume_total", "result", "not_resumed"); r != 1 || n != 5 {
				t.Errorf("%s: isshoni_ws_resume_total resumed %v, not_resumed %v; want 1 and 5", tc.name, r, n)
			}
			e.close()
		}
	})
}

// A revocation skips grace for a detached connection too (01 §4.2): CloseConnections closes it at once, with left.
func TestResumeRevokedWhileDetached(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		cookieO, _ := e.user(false)
		ca, wa := e.connect(cookieA, signaltest.DefaultHello())
		obs, _ := e.connect(cookieO, signaltest.DefaultHello())
		join(t, ca, "lounge")
		join(t, obs, "lounge")
		share := liveShare("s_aaaaaaaaaaaaaaaa", a.UserID, wa.ConnectionID)
		signal.AddShare(e.hub, "lounge", share)
		settle()
		ca.Close()
		settle()
		drain(t, obs)

		revoked := time.Now()
		if n := e.hub.CloseConnections(signal.ConnSelector{UserID: a.UserID, SessionID: a.SessionID},
			protocol.ErrorCodeSessionRevoked); n != 1 {
			t.Errorf("closed %d, want the detached connection", n)
		}
		settle()
		evs, st := drain(t, obs)
		if len(evs) != 2 || evs[0].Kind != protocol.RoomEventKindShareStopped || evs[0].Reason != protocol.EndReasonLeft ||
			evs[1].Kind != protocol.RoomEventKindParticipantLeft || evs[1].Reason != protocol.EndReasonLeft ||
			!evs[1].At.Equal(revoked) {
			t.Errorf("events %+v, want share.stopped and participant.left with left, at once", evs)
		}
		if len(st.Participants) != 1 {
			t.Errorf("participants %v, want the observer alone", userIDs(*st))
		}
		calls := e.media.Peer(wa.ConnectionID).Calls()
		if len(calls) != 2 || calls[0].Method != "EndShare" || calls[0].Args[1] != protocol.EndReasonLeft ||
			calls[1].Method != "Close" {
			t.Errorf("MediaPeer calls %+v, want EndShare(left), Close", calls)
		}
		if conns, _, _, slots := signal.Counts(e.hub, a.UserID); conns != 1 || slots != 0 {
			t.Errorf("conns %d, A's slots %d; want 1, 0", conns, slots)
		}
		if _, w := e.resume(cookieA, wa.ResumeToken); w.Resumed {
			t.Error("the token resumed a revoked connection")
		}
	})
}

// A connection that the hub is closing for good resumes nothing, even while its socket lingers: here it was revoked,
// and its client doesn't finish the close handshake.
func TestResumeWhileRevoked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, id := e.user(false)
		c, w := e.connect(cookie, signaltest.DefaultHello())
		c.Pause()
		if n := e.hub.CloseConnections(signal.ConnSelector{UserID: id.UserID}, protocol.ErrorCodeSessionRevoked); n != 1 {
			t.Fatalf("closed %d, want 1", n)
		}
		synctest.Wait()
		if conns, socks, _, _ := signal.Counts(e.hub, id.UserID); conns != 1 || socks != 1 {
			t.Fatalf("conns %d, sockets %d; want the revoked connection still closing its socket", conns, socks)
		}
		// The hub alone was told (the fake's session is still valid), so the hello gets as far as the token.
		c2, w2 := e.resume(cookie, w.ResumeToken)
		if w2.Resumed || w2.ConnectionID == w.ConnectionID {
			t.Errorf("resumed %v: a revoked connection was resumed", w2.Resumed)
		}
		if e.logs.count("level=DEBUG", "resume token not resumed", "reason=closing") != 1 {
			t.Errorf("no DEBUG line with reason=closing:\n%s", e.logs)
		}
		ping(t, c2)
		time.Sleep(11 * time.Second) // the old socket's close handshake times out
		synctest.Wait()
		if conns, socks, _, slots := signal.Counts(e.hub, id.UserID); conns != 1 || socks != 1 || slots != 1 {
			t.Errorf("conns %d, sockets %d, slots %d; want only the new connection", conns, socks, slots)
		}
		if e.logs.count("connection closed", w.ConnectionID, "reason=left") != 1 {
			t.Errorf("no close line with reason=left for the revoked connection:\n%s", e.logs)
		}
	})
}

// A hello with a resume token that loses to the connection's close: the client left on purpose a moment before, and
// the actor, busy with a slow room lookup, gets to the close first. The hello then opens a new connection.
func TestResumeLosesToClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, id := e.user(false)
		c, w := e.connect(cookie, signaltest.DefaultHello())
		release := e.rooms.HoldGetRoom()
		defer release()
		request(t, c, protocol.MessageTypeRoomJoin, protocol.RoomJoin{RoomID: "lounge"})
		synctest.Wait() // the actor waits for GetRoom
		if err := c.CloseWith(websocket.StatusNormalClosure); err != nil {
			t.Fatal(err)
		}
		c2 := e.mustDial(headers(cookie, testOrigin, ""))
		reqID := request(t, c2, protocol.MessageTypeHello, resumeHello(w.ResumeToken))
		synctest.Wait() // the hello waits for the actor, behind the socket's end
		expectOpen(t, c2)

		release()
		env := expectType(t, c2, protocol.MessageTypeWelcome)
		w2, err := protocol.Decode[protocol.Welcome](env)
		if err != nil || env.Re != reqID || w2.Resumed || w2.ConnectionID == w.ConnectionID {
			t.Errorf("welcome %v, re %q, resumed %v, same connection %v; want a new connection", err, env.Re,
				w2.Resumed, w2.ConnectionID == w.ConnectionID)
		}
		ping(t, c2)
		synctest.Wait()
		if conns, socks, _, slots := signal.Counts(e.hub, id.UserID); conns != 1 || socks != 1 || slots != 1 {
			t.Errorf("conns %d, sockets %d, slots %d; want only the new connection", conns, socks, slots)
		}
		// The join that was under way went through and was undone by the close.
		if peers := e.media.Peers(); len(peers) != 1 || !peers[0].Closed() {
			t.Errorf("%d MediaPeers, want the join's one, closed", len(peers))
		}
		if snap := e.hub.Snapshot(); len(snap.Rooms) != 0 {
			t.Errorf("snapshot %+v, want no rooms", snap)
		}
	})
}

// Shutdown doesn't wait for a grace (04 §6.4): a detached connection closes at once, and its shares end with
// server_shutdown.
func TestResumeShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		cookieB, _ := e.user(false)
		ca, wa := e.connect(cookieA, signaltest.DefaultHello())
		cb, wb := e.connect(cookieB, signaltest.DefaultHello())
		join(t, ca, "lounge")
		join(t, cb, "lounge")
		share := liveShare("s_aaaaaaaaaaaaaaaa", a.UserID, wa.ConnectionID)
		signal.AddShare(e.hub, "lounge", share)
		settle()
		ca.Close()
		settle()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		start := time.Now()
		if err := e.hub.Shutdown(ctx, protocol.ShutdownReasonRestart); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		if d := time.Since(start); d != 0 {
			t.Errorf("Shutdown took %v with a detached connection, want no wait", d)
		}
		calls := e.media.Peer(wa.ConnectionID).Calls()
		if len(calls) != 2 || calls[0].Method != "EndShare" || calls[0].Args[1] != protocol.EndReasonServerShutdown ||
			calls[1].Method != "Close" {
			t.Errorf("A's MediaPeer calls %+v, want EndShare(server_shutdown), Close", calls)
		}
		if !e.media.Peer(wb.ConnectionID).Closed() {
			t.Error("B's MediaPeer is not closed")
		}
		if conns, socks, _, slots := signal.Counts(e.hub, a.UserID); conns+socks+slots != 0 {
			t.Errorf("conns %d, sockets %d, A's slots %d after Shutdown; want none", conns, socks, slots)
		}
		for cb.Pending() > 0 {
			if env := recv(t, cb); env.Type == protocol.MessageTypeServerShutdown {
				break
			}
		}
		expectFail(t, cb, protocol.ErrorCodeServerShutdown, protocol.ErrorScopeConnection)
	})
}

// Nothing resumes once Shutdown has begun: a hello with a resume token that was under way then gets the shutdown
// notice like every other socket, also when its connection is still there, closing a socket that doesn't answer.
func TestResumeDuringShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, id := e.user(false)
		old, w := e.connect(cookie, signaltest.DefaultHello())
		old.Pause()
		release := e.auth.HoldRevalidate()
		defer release()
		c2 := e.mustDial(headers(cookie, testOrigin, ""))
		request(t, c2, protocol.MessageTypeHello, resumeHello(w.ResumeToken))
		synctest.Wait() // the hello waits for its Revalidate

		done := make(chan error, 1)
		go func() { done <- e.hub.Shutdown(ctxT(t), protocol.ShutdownReasonRestart) }()
		synctest.Wait()
		if conns, _, _, _ := signal.Counts(e.hub, id.UserID); conns != 1 {
			t.Fatalf("%d connections, want the old one, still closing its socket", conns)
		}
		release()
		expectShutdown(t, c2, protocol.ShutdownReasonRestart)
		if err := <-done; err != nil {
			t.Errorf("Shutdown: %v", err)
		}
		if e.logs.count("level=DEBUG", "resume token not resumed", "reason=closing") != 1 {
			t.Errorf("no DEBUG line with reason=closing:\n%s", e.logs)
		}
		if e.logs.count("connection resumed") != 0 || e.logs.count("connection opened") != 1 {
			t.Errorf("a connection was resumed or opened during Shutdown:\n%s", e.logs)
		}
		if conns, socks, _, slots := signal.Counts(e.hub, id.UserID); conns+socks+slots != 0 {
			t.Errorf("conns %d, sockets %d, slots %d after Shutdown; want none", conns, socks, slots)
		}
	})
}

// A resume is a resync, not a replay (01 §10.5): what the hub sent while the connection was detached is gone, and
// the room.state after the welcome is current. The hello's caps and the revalidated name and role are the client's
// current ones. The order is welcome, room.state, then Resync and what the peer emits in it (its pending sub offer).
func TestResumeResync(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		cookieO, o := e.user(false)
		ca, wa := e.connect(cookieA, signaltest.DefaultHello())
		obs, _ := e.connect(cookieO, signaltest.DefaultHello())
		join(t, ca, "lounge")
		join(t, obs, "lounge")
		settle()
		peer := e.media.Peer(wa.ConnectionID)
		ca.Close()
		settle()
		drain(t, obs)

		// While A is away: the room changes, A is notified, A's peer has events, and A is renamed and made an admin.
		e.hub.UpdateUser(o.UserID, "olga", false)
		e.hub.Notify(signal.Target{UserID: a.UserID}, protocol.TopicMe)
		peer.Sink().Offer(protocol.PCOffer{PC: protocol.PCKindSub, Gen: 1, Neg: 1, SDP: signaltest.FakeSDP})
		peer.Sink().SubscriptionStatus([]protocol.SubscriptionStatus{{ShareID: "s_x", Video: protocol.VideoLayerLow,
			Audio: protocol.AudioStateOn, RequestedVideo: protocol.VideoLayerHigh, Reason: protocol.StatusReasonBandwidth}})
		e.auth.SetUser(a.UserID, "alex", true)
		settle()
		drain(t, obs)
		// Like the SFU, the peer sends its pending sub offer again when it is resynced.
		peer.OnResync(func() {
			peer.Sink().Offer(protocol.PCOffer{PC: protocol.PCKindSub, Gen: 1, Neg: 7, SDP: signaltest.FakeSDP})
		})

		h := resumeHello(wa.ResumeToken)
		h.Caps.Decode = []protocol.CodecKey{protocol.CodecOpus}
		c2, w2 := e.connect(cookieA, h)
		if !w2.Resumed || w2.User != (protocol.UserInfo{ID: a.UserID, Name: "alex", Admin: true}) {
			t.Fatalf("welcome resumed %v, user %+v; want alex, an admin", w2.Resumed, w2.User)
		}
		st := expectState(t, c2, "lounge")
		if pa, po := participantOf(t, &st, a.UserID), participantOf(t, &st, o.UserID); pa.Name != "alex" ||
			pa.Status != protocol.ParticipantStatusPresent || po.Name != "olga" {
			t.Errorf("state after the resume: A %+v, the observer %+v; want alex (present) and olga", pa, po)
		}
		// After the state comes the resync's offer, not the one from while the connection was away. Nothing else was
		// kept: the pong is the next message.
		offer, err := protocol.Decode[protocol.PCOffer](expectType(t, c2, protocol.MessageTypePCOffer))
		if err != nil || offer.Neg != 7 {
			t.Errorf("pc.offer %+v, %v after the resume; want the resync's (neg 7)", offer, err)
		}
		ping(t, c2)
		peer.OnResync(nil)
		calls := peer.Calls()
		if len(calls) != 2 || calls[0].Method != "SetCaps" || calls[1].Method != "Resync" {
			t.Fatalf("MediaPeer calls %+v, want SetCaps, Resync", calls)
		}
		if caps, _ := calls[0].Args[0].(protocol.Caps); !slices.Equal(caps.Decode, []protocol.CodecKey{protocol.CodecOpus}) {
			t.Errorf("SetCaps(%+v), want the hello's caps", calls[0].Args[0])
		}

		// From here on the connection hears everything again.
		peer.Sink().Offer(protocol.PCOffer{PC: protocol.PCKindSub, Gen: 1, Neg: 2, SDP: signaltest.FakeSDP})
		expectType(t, c2, protocol.MessageTypePCOffer)
		e.hub.Notify(signal.Target{Admins: true}, protocol.TopicAdminUsers)
		expectInvalidate(t, c2, protocol.TopicAdminUsers)
		settle()
		if _, st := drain(t, obs); participantOf(t, st, a.UserID).Name != "alex" ||
			participantOf(t, st, a.UserID).Status != protocol.ParticipantStatusPresent {
			t.Errorf("the observer's state %+v, want alex present", st)
		}
		expectOpen(t, c2)

		// A resume with the same caps tells the peer nothing but Resync.
		h.ResumeToken = w2.ResumeToken
		c3, w3 := e.connect(cookieA, h)
		if !w3.Resumed {
			t.Fatal("the second resume failed")
		}
		expectState(t, c3, "lounge")
		ping(t, c3)
		if calls := peer.Calls(); len(calls) != 3 || calls[2].Method != "Resync" {
			t.Errorf("MediaPeer calls %+v, want one more Resync", calls)
		}
	})
}

// A participant merges its connections (01 §4.3): it is reconnecting only while all of them are detached, and it
// stays when one of them is not resumed, without any event.
func TestResumeTwoConnections(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		cookieO, _ := e.user(false)
		a1, _ := e.connect(cookieA, signaltest.DefaultHello())
		a2, wa2 := e.connect(cookieA, signaltest.DefaultHello())
		obs, _ := e.connect(cookieO, signaltest.DefaultHello())
		for _, c := range []*signaltest.Client{a1, a2, obs} {
			join(t, c, "lounge")
		}
		settle()
		drain(t, obs)
		check := func(when string, wantP protocol.ParticipantStatus, wantC ...protocol.ConnectionStatus) {
			t.Helper()
			settle()
			evs, st := drain(t, obs)
			if ps, cs := statuses(participantOf(t, st, a.UserID)); len(evs) != 0 || ps != wantP || !slices.Equal(cs, wantC) {
				t.Errorf("%s: events %+v, A %s with connections %v; want no events, %s with %v", when, evs, ps, cs,
					wantP, wantC)
			}
		}

		a1.Close()
		check("one connection detached", protocol.ParticipantStatusPresent, reconnecting, online)
		a2.Close()
		check("both detached", protocol.ParticipantStatusReconnecting, reconnecting, reconnecting)
		c, w := e.resume(cookieA, wa2.ResumeToken)
		if !w.Resumed || w.ConnectionID != wa2.ConnectionID {
			t.Fatalf("resumed %v", w.Resumed)
		}
		expectState(t, c, "lounge")
		check("one resumed", protocol.ParticipantStatusPresent, reconnecting, online)

		// The other connection's grace ends: it goes, the participant stays, and nobody gets an event.
		time.Sleep(grace)
		check("the other one expired", protocol.ParticipantStatusPresent, online)
		if evs, _ := drain(t, c); len(evs) != 0 {
			t.Errorf("events for the user's own connection %+v, want none", evs)
		}
	})
}

// A connection that resumes without a room, or whose room was closed while it was away, gets a welcome without
// roomId, no room.state and no Resync (01 §8.2 step 7).
func TestResumeWithoutRoom(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		e.rooms.Add(room1)
		cookie, _ := e.user(false)
		c, w := e.connect(cookie, signaltest.DefaultHello())
		c.Close()
		synctest.Wait()
		c, w2 := e.resume(cookie, w.ResumeToken)
		if !w2.Resumed || w2.ConnectionID != w.ConnectionID || w2.RoomID != "" {
			t.Fatalf("resumed %v, roomId %q; want resumed, in no room", w2.Resumed, w2.RoomID)
		}
		ping(t, c)
		if n := len(e.media.Peers()); n != 0 {
			t.Errorf("%d MediaPeers, want none", n)
		}

		join(t, c, room1.ID)
		peer := e.media.Peer(w.ConnectionID)
		c.Close()
		synctest.Wait()
		e.hub.CloseRoom(room1.ID)
		synctest.Wait()
		c, w3 := e.resume(cookie, w2.ResumeToken)
		if !w3.Resumed || w3.RoomID != "" {
			t.Fatalf("after CloseRoom: resumed %v, roomId %q; want resumed, in no room", w3.Resumed, w3.RoomID)
		}
		ping(t, c)
		if calls := peer.Calls(); len(calls) != 1 || calls[0].Method != "Close" {
			t.Errorf("MediaPeer calls %+v, want only Close (by CloseRoom)", calls)
		}
		join(t, c, "lounge")
	})
}

// A connection keeps its role: a hello that asks for another one is another client, and opens a connection of its
// own.
func TestResumeOtherRole(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		c, w := e.connect(cookie, signaltest.DefaultHello())
		c.Close()
		synctest.Wait()
		h := resumeHello(w.ResumeToken)
		h.Role = protocol.RoleViewer
		if _, w2 := e.connect(cookie, h); w2.Resumed || w2.ConnectionID == w.ConnectionID {
			t.Errorf("a viewer hello resumed a full connection")
		}
		if e.logs.count("level=DEBUG", "resume token not resumed", "reason=other_client") != 1 {
			t.Errorf("no DEBUG line with reason=other_client:\n%s", e.logs)
		}
		if _, w3 := e.resume(cookie, w.ResumeToken); !w3.Resumed || w3.ConnectionID != w.ConnectionID {
			t.Errorf("the connection's own role: resumed %v", w3.Resumed)
		}
	})
}

// Two sockets that present the same token at once: one resumes the connection, the other finds the token rotated
// and opens a new connection.
func TestResumeSameTokenTwice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, id := e.user(false)
		c, w := e.connect(cookie, signaltest.DefaultHello())
		c.Close()
		synctest.Wait()
		s1 := e.mustDial(headers(cookie, testOrigin, ""))
		s2 := e.mustDial(headers(cookie, testOrigin, ""))
		for _, s := range []*signaltest.Client{s1, s2} {
			if err := s.Send(protocol.MessageTypeHello, "h", resumeHello(w.ResumeToken)); err != nil {
				t.Fatal(err)
			}
		}
		resumed := 0
		for _, s := range []*signaltest.Client{s1, s2} {
			wl, err := protocol.Decode[protocol.Welcome](expectType(t, s, protocol.MessageTypeWelcome))
			if err != nil {
				t.Fatal(err)
			}
			if wl.Resumed != (wl.ConnectionID == w.ConnectionID) {
				t.Errorf("resumed %v with connectionId %s (the token's is %s)", wl.Resumed, wl.ConnectionID, w.ConnectionID)
			}
			if wl.Resumed {
				resumed++
			}
			ping(t, s)
		}
		if resumed != 1 {
			t.Errorf("%d sockets resumed the connection, want exactly one", resumed)
		}
		synctest.Wait()
		if conns, socks, _, slots := signal.Counts(e.hub, id.UserID); conns != 2 || socks != 2 || slots != 2 {
			t.Errorf("conns %d, sockets %d, slots %d; want 2, 2, 2", conns, socks, slots)
		}
	})
}

// The per-user cap never stops a resume (01 §3.1 step 5). The user's connections hold their slots while detached,
// so the socket a client opens to resume one can be over the cap: it waits for its hello, and a hello that resumes
// needs no slot.
func TestResumeAtTheCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, id := e.user(false)
		limit := signal.DefaultConfig().Limits.MaxConnectionsPerUser
		clients := make([]*signaltest.Client, limit)
		welcomes := make([]protocol.Welcome, limit)
		for i := range limit {
			clients[i], welcomes[i] = e.connect(cookie, signaltest.DefaultHello())
		}
		counts := func(when string, wantConns, wantSocks, wantSlots, wantOver int) {
			t.Helper()
			synctest.Wait()
			conns, socks, _, slots := signal.Counts(e.hub, id.UserID)
			if over := signal.OverCap(e.hub, id.UserID); conns != wantConns || socks != wantSocks || slots != wantSlots ||
				over != wantOver {
				t.Errorf("%s: conns %d, sockets %d, slots %d, waiting at the cap %d; want %d, %d, %d, %d", when,
					conns, socks, slots, over, wantConns, wantSocks, wantSlots, wantOver)
			}
		}

		// One socket drops. Its connection keeps its slot, so the client's new socket is the user's 17th.
		first := welcomes[0].ResumeToken
		clients[0].Close()
		counts("one detached", limit, limit-1, limit, 0)
		c, w := e.resume(cookie, first)
		if !w.Resumed || w.ConnectionID != welcomes[0].ConnectionID {
			t.Fatalf("at the cap: resumed %v, want the connection back", w.Resumed)
		}
		clients[0], welcomes[0] = c, w
		counts("resumed at the cap", limit, limit, limit, 0)

		// The same with a half-open old socket.
		c, w = e.resume(cookie, welcomes[1].ResumeToken)
		if !w.Resumed || w.ConnectionID != welcomes[1].ConnectionID {
			t.Fatalf("at the cap, half-open: resumed %v", w.Resumed)
		}
		expectFail(t, clients[1], protocol.ErrorCodeReplaced, protocol.ErrorScopeConnection)
		clients[1], welcomes[1] = c, w
		counts("replaced at the cap", limit, limit, limit, 0)

		// The network drops every socket, and every client opens its new one before any of them says hello.
		e.srv.Net.Sever()
		counts("all detached", limit, 0, limit, 0)
		for _, c := range clients {
			if code, err := c.CloseStatus(ctxT(t)); err != nil || code != -1 {
				t.Fatalf("a severed socket ended with %d, %v; want no close frame", code, err)
			}
		}
		for i := range limit {
			clients[i] = e.mustDial(headers(cookie, testOrigin, ""))
		}
		counts("all waiting at the cap", limit, limit, limit, limit)
		for _, c := range clients {
			expectOpen(t, c)
		}
		// There are as many places at the cap as slots. One socket more gets the error at once.
		expectFail(t, e.mustDial(headers(cookie, testOrigin, "")), protocol.ErrorCodeTooManyConnections,
			protocol.ErrorScopeConnection)
		for i, c := range clients {
			w, err := c.Hello(ctxT(t), resumeHello(welcomes[i].ResumeToken))
			if err != nil || !w.Resumed || w.ConnectionID != welcomes[i].ConnectionID {
				t.Fatalf("connection %d: %v, resumed %v", i, err, w.Resumed)
			}
			welcomes[i] = w
		}
		counts("all resumed", limit, limit, limit, 0)

		// At the cap, a hello that resumes nothing gets too_many_connections as its reply: without a token, and with
		// a token that was rotated.
		for _, h := range []protocol.Hello{signaltest.DefaultHello(), resumeHello(first)} {
			c := e.mustDial(headers(cookie, testOrigin, ""))
			reqID := request(t, c, protocol.MessageTypeHello, h)
			if pe, re := expectError(t, c, protocol.ErrorCodeTooManyConnections, protocol.ErrorScopeConnection); re != reqID ||
				pe.Retryable {
				t.Errorf("too_many_connections re %q (want %q), retryable %v", re, reqID, pe.Retryable)
			}
			expectClose(t, c, protocol.CloseCodeRateLimited)
		}
		counts("after two refused hellos", limit, limit, limit, 0)

		// A socket that waits at the cap and says nothing gets hello_timeout like any other, and gives its place back.
		quiet := e.mustDial(headers(cookie, testOrigin, ""))
		counts("one waiting", limit, limit+1, limit, 1)
		time.Sleep(10 * time.Second)
		expectFail(t, quiet, protocol.ErrorCodeHelloTimeout, protocol.ErrorScopeConnection)
		counts("after the hello timeout", limit, limit, limit, 0)

		// A slot that became free while a socket waited is the socket's when its hello opens a new connection.
		waiting := e.mustDial(headers(cookie, testOrigin, ""))
		counts("one waiting again", limit, limit+1, limit, 1)
		if err := clients[2].CloseWith(websocket.StatusNormalClosure); err != nil {
			t.Fatal(err)
		}
		counts("one connection closed", limit-1, limit, limit-1, 1)
		if w, err := waiting.Hello(ctxT(t), signaltest.DefaultHello()); err != nil || w.Resumed {
			t.Fatalf("hello after a slot was freed: %v, resumed %v", err, w.Resumed)
		}
		counts("the waiting socket connected", limit, limit, limit, 0)
	})
}
