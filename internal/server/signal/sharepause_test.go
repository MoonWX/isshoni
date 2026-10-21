package signal_test

import (
	"bytes"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/signal"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// share.pause (M2 01 §10.3, slice WA27): share.update{paused} from a connection with the feature sets the share's
// pause bit, which room.state shows as the status paused while the share would be live; stalled wins; a starting
// share shows paused once it goes live; any connection of the user may pause; without the feature paused is ignored.

const pausedStatus = protocol.ShareStatusPaused

// pauseHello is a web client's hello (role full) with the feature share.pause.
func pauseHello() protocol.Hello {
	h := signaltest.DefaultHello()
	h.Features = []protocol.Feature{protocol.FeatureSharePause}
	return h
}

// pauseConn connects with pauseHello and checks that the welcome turns the feature on.
func (e *env) pauseConn(cookie string) (*signaltest.Client, protocol.Welcome) {
	e.t.Helper()
	c, w := e.connect(cookie, pauseHello())
	if !slices.Equal(w.Features, []protocol.Feature{protocol.FeatureSharePause}) {
		e.t.Fatalf("welcome features %v, want share.pause", w.Features)
	}
	return c, w
}

func boolPtr(b bool) *bool { return &b }

// setPaused sends share.update{shareId, paused} on c and returns the ShareParams of its ok, skipping queued room
// traffic.
func setPaused(t *testing.T, c *signaltest.Client, shareID string, paused bool) protocol.ShareParams {
	t.Helper()
	env := okReply(t, c, request(t, c, protocol.MessageTypeShareUpdate,
		protocol.ShareUpdate{ShareID: shareID, Paused: &paused}))
	p, err := protocol.Decode[protocol.ShareParams](env)
	if err != nil || p.ShareID != shareID {
		t.Fatalf("share.update reply %s, %v; want the ShareParams of %s", env.Data, err, shareID)
	}
	return p
}

// lastUpdate returns the ShareUpdate of the peer's latest UpdateShare call.
func lastUpdate(t *testing.T, p *signaltest.Peer) protocol.ShareUpdate {
	t.Helper()
	calls := p.CallsTo("UpdateShare")
	if len(calls) == 0 {
		t.Fatal("no UpdateShare call")
	}
	return calls[len(calls)-1].Args[1].(protocol.ShareUpdate)
}

// statusAfter lets the room settle, reads what c has queued, which must be room traffic without events, and returns
// the share's status in the last room.state, or "" when no room.state came.
func statusAfter(t *testing.T, c *signaltest.Client, shareID string) protocol.ShareStatus {
	t.Helper()
	settle()
	evs, st := drain(t, c)
	if len(evs) != 0 {
		t.Errorf("room.events %+v, want none", evs)
	}
	if st == nil {
		return ""
	}
	return mustShare(t, st, shareID).Status
}

// shareCounts returns isshoni_shares by status: starting, live, stalled, paused.
func (e *env) shareCounts() [4]float64 {
	var out [4]float64
	for i, st := range []string{"starting", "live", "stalled", "paused"} {
		out[i] = max(e.metric("isshoni_shares", "status", st), 0)
	}
	return out
}

// Pause and resume a live share from its publishing connection: the ok comes first, then room.state shows paused to
// everyone, with no room.event; the MediaPeer gets the bit; stalled wins over paused; no timeout applies while paused.
func TestSharePause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		cookieB, _ := e.user(false)
		ca, wa := e.pauseConn(cookieA)
		cb, _ := e.connect(cookieB, signaltest.DefaultHello())
		join(t, ca, "lounge")
		join(t, cb, "lounge")
		id := startShare(t, ca, screen("r1")).ShareID
		goLive(e, wa.ConnectionID, id)
		quiet(t, ca, cb)
		peer := e.media.Peer(wa.ConnectionID)

		// Pause: the reply is the MediaPeer's ShareParams and goes out before the room.state with the change.
		rid := request(t, ca, protocol.MessageTypeShareUpdate, protocol.ShareUpdate{ShareID: id, Paused: boolPtr(true)})
		env := expectType(t, ca, protocol.MessageTypeOK)
		if p, err := protocol.Decode[protocol.ShareParams](env); err != nil || env.Re != rid ||
			jsonOf(t, p) != jsonOf(t, signaltest.ShareParamsFor(id)) {
			t.Errorf("share.update reply re %q (want %q) %s, %v", env.Re, rid, env.Data, err)
		}
		if u := lastUpdate(t, peer); u.Paused == nil || !*u.Paused || u.ShareID != id {
			t.Errorf("UpdateShare got %+v, want paused true", u)
		}
		for _, c := range []*signaltest.Client{ca, cb} {
			f, err := c.RecvFrame(ctxT(t))
			if err != nil || f.Envelope.Type != protocol.MessageTypeRoomState {
				t.Fatalf("got %s, %v; want room.state", f.Envelope.Type, err)
			}
			if !bytes.Contains(f.Raw, []byte(`"status":"paused"`)) {
				t.Errorf("room.state %s, want the share paused", f.Raw)
			}
			if st := statusAfter(t, c, id); st != "" {
				t.Errorf("another room.state with %s, want none", st)
			}
		}
		if got := e.shareCounts(); got != [4]float64{0, 0, 0, 1} {
			t.Errorf("isshoni_shares starting/live/stalled/paused %v, want only paused", got)
		}
		if e.logs.count("level=INFO", "msg=\"share paused\"", "share_id="+id, "conn_id="+wa.ConnectionID,
			"user_id="+a.UserID, "room_id=lounge") != 1 {
			t.Errorf("no share paused line:\n%s", e.logs)
		}
		if snap := e.hub.Snapshot(); snap.Rooms[0].Shares[0].Info.Status != pausedStatus {
			t.Errorf("snapshot status %s, want paused", snap.Rooms[0].Shares[0].Info.Status)
		}

		// Pausing again changes nothing: the reply still comes, no room.state.
		setPaused(t, ca, id, true)
		if st := statusAfter(t, cb, id); st != "" {
			t.Errorf("room.state with %s after a second pause, want none", st)
		}

		// Stalled wins over paused, and the stall timeout runs; live again shows paused.
		peer.Sink().ShareMedia(id, signal.ShareMediaEvent{Kind: signal.ShareMediaStalled,
			Layers: []protocol.VideoLayer{high, low}, Codec: protocol.CodecH264High})
		if st := statusAfter(t, cb, id); st != protocol.ShareStatusStalled {
			t.Errorf("status %q after the pub PC went down, want stalled", st)
		}
		if got := e.shareCounts(); got != [4]float64{0, 0, 1, 0} {
			t.Errorf("isshoni_shares %v while stalled, want only stalled", got)
		}
		goLive(e, wa.ConnectionID, id)
		if st := statusAfter(t, cb, id); st != pausedStatus {
			t.Errorf("status %q after the pub PC came back, want paused", st)
		}

		// No timeout applies to paused: two minutes later the share is still there, paused.
		time.Sleep(2 * time.Minute)
		if st := statusAfter(t, cb, id); st != "" {
			t.Errorf("room.state with %s while paused, want none", st)
		}
		if snap := e.hub.Snapshot(); len(snap.Rooms[0].Shares) != 1 {
			t.Fatalf("shares %+v after two minutes paused, want the share", snap.Rooms[0].Shares)
		}

		// Resume: live again for everyone.
		setPaused(t, ca, id, false)
		if u := lastUpdate(t, peer); u.Paused == nil || *u.Paused {
			t.Errorf("UpdateShare got %+v, want paused false", u)
		}
		if st := statusAfter(t, cb, id); st != protocol.ShareStatusLive {
			t.Errorf("status %q after the resume, want live", st)
		}
		if got := e.shareCounts(); got != [4]float64{0, 1, 0, 0} {
			t.Errorf("isshoni_shares %v after the resume, want only live", got)
		}
		if e.logs.count("level=INFO", "msg=\"share resumed\"", "share_id="+id) != 1 {
			t.Errorf("no share resumed line:\n%s", e.logs)
		}

		// A paused share that ends leaves the count: the gauge stays right.
		setPaused(t, ca, id, true)
		stopShare(t, ca, id)
		settle()
		if got := e.shareCounts(); got != [4]float64{} {
			t.Errorf("isshoni_shares %v after the stop, want none", got)
		}
	})
}

// Pausing a starting share stores the bit: the share stays starting, and shows paused once it would be live. The
// first keyframe still announces it. The start timeout still applies while it is starting.
func TestSharePauseStarting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		cookieB, _ := e.user(false)
		ca, wa := e.pauseConn(cookieA)
		cb, _ := e.connect(cookieB, signaltest.DefaultHello())
		join(t, ca, "lounge")
		join(t, cb, "lounge")
		id := startShare(t, ca, screen("r1")).ShareID
		quiet(t, ca, cb)
		peer := e.media.Peer(wa.ConnectionID)

		setPaused(t, ca, id, true)
		if u := lastUpdate(t, peer); u.Paused == nil || !*u.Paused {
			t.Errorf("UpdateShare got %+v, want paused true", u)
		}
		if st := statusAfter(t, cb, id); st != "" {
			t.Errorf("room.state with %s after pausing a starting share, want none: it stays starting", st)
		}
		if got := e.shareCounts(); got != [4]float64{1, 0, 0, 0} {
			t.Errorf("isshoni_shares %v, want only starting", got)
		}

		// The first keyframe: paused, not live, and share.started goes out as for any share.
		peer.Sink().ShareMedia(id, liveEvent())
		settle()
		evs, st := drain(t, cb)
		if len(evs) != 1 || evs[0].Kind != protocol.RoomEventKindShareStarted || evs[0].UserID != a.UserID {
			t.Errorf("events %+v, want share.started", evs)
		}
		if s := mustShare(t, st, id); s.Status != pausedStatus || !slices.Equal(s.Layers, []protocol.VideoLayer{high, low}) {
			t.Errorf("share %+v, want paused with both layers", s)
		}
		if got := e.shareCounts(); got != [4]float64{0, 0, 0, 1} {
			t.Errorf("isshoni_shares %v, want only paused", got)
		}
		setPaused(t, ca, id, false)
		if st := statusAfter(t, cb, id); st != protocol.ShareStatusLive {
			t.Errorf("status %q after the resume, want live", st)
		}

		// A share paused while starting that never goes live still ends at its start timeout.
		id2 := startShare(t, ca, screen("r2")).ShareID
		setPaused(t, ca, id2, true)
		quiet(t, ca, cb)
		time.Sleep(signal.DefaultConfig().ShareStartTimeout)
		settle()
		evs, st = drain(t, cb)
		if len(stopped(evs)) != 1 || evs[0].ShareID != id2 || evs[0].Reason != protocol.EndReasonMediaTimeout ||
			shareIn(st, id2) != nil {
			t.Errorf("events %+v, state %+v; want %s ended with media_timeout", evs, st, id2)
		}
	})
}

// Any connection of the share's user may pause or resume it (01 §4.1): another connection with the feature hands it
// to the publishing connection's MediaPeer; another user's connection is forbidden.
func TestSharePauseFromOtherConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		cookieB, _ := e.user(false)
		a1, w1 := e.pauseConn(cookieA)
		a2, w2 := e.pauseConn(e.session(a, "sessionA2"))
		cb, _ := e.pauseConn(cookieB)
		for _, c := range []*signaltest.Client{a1, a2, cb} {
			join(t, c, "lounge")
		}
		id := startShare(t, a1, screen("r1")).ShareID
		goLive(e, w1.ConnectionID, id)
		quiet(t, a1, a2, cb)
		peer1, peer2 := e.media.Peer(w1.ConnectionID), e.media.Peer(w2.ConnectionID)

		if p := setPaused(t, a2, id, true); jsonOf(t, p) != jsonOf(t, signaltest.ShareParamsFor(id)) {
			t.Errorf("ShareParams %s, want the publishing connection's", jsonOf(t, p))
		}
		if u := lastUpdate(t, peer1); u.Paused == nil || !*u.Paused {
			t.Errorf("UpdateShare at the publishing connection got %+v, want paused true", u)
		}
		if calls := peer2.MediaCalls(); len(calls) != 0 {
			t.Errorf("MediaPeer calls at the other connection %+v, want none", calls)
		}
		// (a2's room.state may have come before its ok, which setPaused skipped.)
		for _, c := range []*signaltest.Client{a1, cb} {
			if st := statusAfter(t, c, id); st != pausedStatus {
				t.Errorf("status %q, want paused", st)
			}
		}
		drain(t, a2)
		if e.logs.count("level=INFO", "msg=\"share paused\"", "share_id="+id, "conn_id="+w1.ConnectionID) != 1 {
			t.Errorf("no share paused line from the publishing connection:\n%s", e.logs)
		}

		// Another user may not, feature or not.
		refused(t, cb, protocol.MessageTypeShareUpdate, protocol.ShareUpdate{ShareID: id, Paused: boolPtr(false)},
			protocol.ErrorCodeForbidden)
		if st := statusAfter(t, a1, id); st != "" {
			t.Errorf("room.state with %s after another user's attempt, want none", st)
		}

		setPaused(t, a2, id, false)
		if st := statusAfter(t, cb, id); st != protocol.ShareStatusLive {
			t.Errorf("status %q after the other connection's resume, want live", st)
		}
	})
}

// paused is read only from a connection with the feature, and only for a share whose publishing connection has it
// too: otherwise it is dropped before the MediaPeer sees the update, without an error, and the rest of the update
// applies.
func TestSharePauseWithoutFeature(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, a := e.user(false)
		cookieB, _ := e.user(false)
		plain, wp := e.connect(cookieA, signaltest.DefaultHello()) // no share.pause
		pauser, _ := e.pauseConn(e.session(a, "sessionA2"))
		cb, _ := e.connect(cookieB, signaltest.DefaultHello())
		for _, c := range []*signaltest.Client{plain, pauser, cb} {
			join(t, c, "lounge")
		}
		id := startShare(t, plain, screen("r1")).ShareID
		goLive(e, wp.ConnectionID, id)
		quiet(t, plain, pauser, cb)
		peer := e.media.Peer(wp.ConnectionID)

		// The publishing connection without the feature: ignored, the reply is the usual one.
		setPaused(t, plain, id, true)
		if u := lastUpdate(t, peer); u.Paused != nil {
			t.Errorf("UpdateShare got paused %v, want none", *u.Paused)
		}
		if st := statusAfter(t, cb, id); st != "" {
			t.Errorf("room.state with %s, want none", st)
		}
		// The rest of an update applies.
		label := "Notes"
		okReply(t, plain, request(t, plain, protocol.MessageTypeShareUpdate,
			protocol.ShareUpdate{ShareID: id, Label: &label, Paused: boolPtr(true)}))
		if u := lastUpdate(t, peer); u.Paused != nil || u.Label == nil || *u.Label != label {
			t.Errorf("UpdateShare got %+v, want the label and no paused", u)
		}
		settle()
		if _, st := drain(t, cb); mustShare(t, st, id).Label != label || mustShare(t, st, id).Status != protocol.ShareStatusLive {
			t.Errorf("state %+v, want the label and the share live", st)
		}

		// A connection with the feature, but the share's publisher doesn't know it: ignored too.
		setPaused(t, pauser, id, true)
		if u := lastUpdate(t, peer); u.Paused != nil {
			t.Errorf("UpdateShare got paused %v for a publisher without the feature, want none", *u.Paused)
		}
		if st := statusAfter(t, cb, id); st != "" {
			t.Errorf("room.state with %s, want none", st)
		}
		if e.logs.count("level=DEBUG", "paused ignored without the feature", "share_id="+id) != 3 {
			t.Errorf("want 3 debug lines for the ignored pauses:\n%s", e.logs)
		}
		if got := e.shareCounts(); got != [4]float64{0, 1, 0, 0} {
			t.Errorf("isshoni_shares %v, want only live", got)
		}
	})
}

// The welcome lists the server's M2 features that the hello asks for, in the server's order (01 §6.2).
func TestWelcomeM2Features(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		h := signaltest.DefaultHello()
		h.Features = []protocol.Feature{protocol.FeatureSharePause, protocol.FeatureUserConnections,
			protocol.FeatureLayerMid, protocol.FeatureAgentRelay}
		_, w := e.connect(cookie, h)
		want := []protocol.Feature{protocol.FeatureAgentRelay, protocol.FeatureUserConnections,
			protocol.FeatureSharePause}
		if !slices.Equal(w.Features, want) {
			t.Errorf("features %v, want %v", w.Features, want)
		}
	})
}
