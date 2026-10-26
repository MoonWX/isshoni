package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// The recovery paths of docs/m2/01-client-core.md §11.6 and §11.7 against fakeServer, with the timers shortened:
// the hold, the resume (the pending offer sent again, a reconcile that stops a share the core doesn't have), the
// republish (on a new connection, on the same one at the next gen, after a resume into no room, and tried again
// after a retryable refusal), share.start tried again after a drop, the pub PC's rebuilds (the server's pc.restart,
// a refused offer, unreachable) and the cleanup of a share that ended while signaling was down. The ICE restarts and
// the rebuilds of a real outage are the itests' (internal/server/itest, D3–D6 and their neighbors).

// fastTiming is defaultTiming shortened for the tests that set it.
func fastTiming(change func(*timing)) func(*Options) {
	return func(o *Options) {
		t := defaultTiming
		t.pcPoll = 10 * time.Millisecond
		if change != nil {
			change(&t)
		}
		o.timing = &t
	}
}

// startedShare runs the env's Client, starts a share and waits for its first pub offer (the actor runs).
func startedShare(t *testing.T, e *testEnv) *Share {
	t.Helper()
	e.run()
	e.waitOnline()
	sh, err := e.start(ctxFor(t))
	if err != nil {
		t.Fatal(err)
	}
	waitOffers(t, e.fs, "1/1")
	return sh
}

// offerList renders the pub offers the server received as "gen/neg …".
func offerList(fs *fakeServer) string {
	var out []string
	for _, o := range received[protocol.PCOffer](fs, protocol.MessageTypePCOffer) {
		out = append(out, fmt.Sprintf("%d/%d", o.Gen, o.Neg))
	}
	return fmt.Sprint(out)
}

// waitOffers waits until the server has received exactly the pub offers want ("1/1 2/1").
func waitOffers(t *testing.T, fs *fakeServer, want string) {
	t.Helper()
	want = "[" + want + "]"
	eventually(t, func() bool { return offerList(fs) == want },
		func() string { return fmt.Sprintf("the server got the pub offers %s, want %s", offerList(fs), want) })
}

// refuseHellos has the server refuse every hello with a retryable error (it is restarting), so the core backs off.
func refuseHellos(fs *fakeServer) {
	fs.setHello(always(welcomeErr(protocol.ErrorCodeServerShutdown, protocol.ErrorScopeConnection, nil)))
}

// TestHoldExpiry: signaling down for the hold ends the share server-unreachable, with the engine stopped; the
// snapshot says reconnecting meanwhile.
func TestHoldExpiry(t *testing.T) {
	const hold = time.Second
	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{tweak: fastTiming(func(t *timing) { t.hold = hold })})
	sh := startedShare(t, e)
	refuseHellos(fs)
	fs.kill()
	eventually(t, func() bool { return sh.Snapshot().State == StateReconnecting },
		func() string { return fmt.Sprintf("the share is %+v, want reconnecting", sh.Snapshot()) })
	away := time.Now()
	select {
	case <-sh.Done():
	case <-time.After(waitTimeout):
		t.Fatal("the share didn't end")
	}
	if held := time.Since(away); held > hold+500*time.Millisecond {
		t.Errorf("the share ended %v after it went reconnecting, want about %v", held, hold)
	}
	if end := sh.Snapshot().End; end == nil || *end != (EndInfo{Reason: EndServerUnreachable}) {
		t.Errorf("the share ended with %+v, want server-unreachable", end)
	}
	if e.engine.Session() != nil || e.c.ActiveShare() != nil {
		t.Error("the engine session or the share outlived the hold")
	}
}

// TestResumeWithinHold: a socket that comes back within the hold resumes the share: the hold stops, the pending pub
// offer goes again with the same gen and neg, nothing is published again, and the room.state that follows the
// welcome reconciles the share (live).
func TestResumeWithinHold(t *testing.T) {
	const hold = 2 * time.Second
	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{tweak: fastTiming(func(t *timing) { t.hold = hold })})
	sh := startedShare(t, e)
	first := received[protocol.PCOffer](fs, protocol.MessageTypePCOffer)[0]
	fs.kill()
	waitOffers(t, fs, "1/1 1/1")
	again := received[protocol.PCOffer](fs, protocol.MessageTypePCOffer)[1]
	if again.SDP.Reveal() != first.SDP.Reveal() || !slices.Equal(again.Tracks, first.Tracks) {
		t.Error("the pending offer sent again after the resume differs from the first")
	}
	if st := e.c.Status(); st.ConnectionID != "c_1" {
		t.Errorf("the connection is %s after the resume, want c_1", st.ConnectionID)
	}
	eventually(t, func() bool { return sh.Snapshot().State == StateLive },
		func() string {
			return fmt.Sprintf("the share is %+v, want live (the resume's room.state lists it)", sh.Snapshot())
		})
	time.Sleep(hold + 500*time.Millisecond)
	select {
	case <-sh.Done():
		t.Fatalf("the share ended after a resume within the hold: %+v", sh.Snapshot().End)
	default:
	}
	if n := len(received[protocol.ShareStart](fs, protocol.MessageTypeShareStart)); n != 1 {
		t.Errorf("the server got %d share.start, want one: a resume publishes nothing again", n)
	}
}

// TestRepublishNewConnection: a welcome that isn't resumed publishes the share again (§11.7): room.join,
// share.start{replaces: the old id} with a new ref, a pub offer of gen 1 for the new id on a new Publisher, while
// the engine session goes on. The share keeps its local identity; its id and the connection's change.
func TestRepublishNewConnection(t *testing.T) {
	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{tweak: fastTiming(nil)})
	events, unsub := e.c.Subscribe()
	defer unsub()
	sh := startedShare(t, e)
	sess := e.engine.Session()
	fs.setHello(func(int, protocol.Hello) fakeAnswer { return fakeAnswer{resumed: false} })
	fs.kill()
	waitOffers(t, fs, "1/1 1/1")
	starts := received[protocol.ShareStart](fs, protocol.MessageTypeShareStart)
	if len(starts) != 2 || starts[1].Replaces != "s_1" || starts[1].Ref == starts[0].Ref || starts[0].Replaces != "" {
		t.Errorf("share.start requests %+v, want a second one replacing s_1 with a ref of its own", starts)
	}
	offers := received[protocol.PCOffer](fs, protocol.MessageTypePCOffer)
	for _, tr := range offers[1].Tracks {
		if tr.ShareID != "s_2" {
			t.Errorf("the new pub offer's tracks %+v, want the new share s_2's", offers[1].Tracks)
		}
	}
	once := []protocol.MessageType{protocol.MessageTypeRoomJoin, protocol.MessageTypeShareStart, protocol.MessageTypePCOffer}
	if got := fs.settledTypes(); !slices.Equal(got, slices.Concat(once, once)) {
		t.Errorf("the server got %v, want room.join, share.start and a pub offer on each connection", got)
	}
	if snap := sh.Snapshot(); snap.ID != "s_2" || snap.End != nil {
		t.Errorf("the share is %+v, want it running as s_2", snap)
	}
	if st := e.c.Status(); st.ConnectionID != "c_2" {
		t.Errorf("the connection is %s, want c_2", st.ConnectionID)
	}
	nextEvent(t, events, func(ev Event) bool { return ev.Kind == EventStatus && ev.Status.ConnectionID == "c_2" })
	nextEvent(t, events, func(ev Event) bool { return ev.Kind == EventShare && ev.Share.ID == "s_2" })
	if e.engine.Session() != sess || e.c.ActiveShare() != sh {
		t.Error("the engine session or the share changed: a republish keeps both")
	}
}

// TestResumeLostShare: a resumed welcome whose room.state lacks the share (the server ended it while the socket was
// away) publishes it again on the same connection: no room.join (the connection is in the room), share.start
// {replaces: the old id}, and a new pub PC for the new id at the next gen, neg 1: the server keeps the connection's
// pub gen (M1 01 §9 rule 2), so a gen-1 offer would be stale. (The pending offer goes again first, as §11.7 orders
// it; the server ignores its tracks, which name a share it no longer has.)
func TestResumeLostShare(t *testing.T) {
	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{tweak: fastTiming(nil)})
	sh := startedShare(t, e)
	fs.mu.Lock()
	fs.shares = nil
	fs.mu.Unlock()
	fs.kill()
	waitOffers(t, fs, "1/1 1/1 2/1")
	offers := received[protocol.PCOffer](fs, protocol.MessageTypePCOffer)
	if tracks := offers[2].Tracks; len(tracks) == 0 || tracks[0].ShareID != "s_2" {
		t.Errorf("the new PC's offer has the tracks %+v, want the new share s_2's", tracks)
	}
	starts := received[protocol.ShareStart](fs, protocol.MessageTypeShareStart)
	if len(starts) != 2 || starts[1].Replaces != "s_1" || starts[1].Ref == starts[0].Ref {
		t.Errorf("share.start requests %+v, want a second one replacing s_1 with a ref of its own", starts)
	}
	if n := len(received[protocol.RoomJoin](fs, protocol.MessageTypeRoomJoin)); n != 1 {
		t.Errorf("the server got %d room.join, want the first alone: the resumed connection is in the room", n)
	}
	if snap := sh.Snapshot(); snap.ID != "s_2" || snap.End != nil || e.c.Status().ConnectionID != "c_1" {
		t.Errorf("the share is %+v on %s, want it running as s_2 on c_1", snap, e.c.Status().ConnectionID)
	}
	if n := fs.staleOffers(); n != 0 {
		t.Errorf("the server answered %d pub offers with stale_negotiation, want none", n)
	}
}

// TestResumeWithoutRoom: a resumed welcome that names no room (the connection was taken out of the share's room
// while the socket was away, and the share with it) publishes the share again at once (M1 01 §10.5): room.join,
// share.start{replaces: the old id}, a new pub PC at the next gen; nothing goes to the old PC, whose offer would get
// not_in_room outside the room.
func TestResumeWithoutRoom(t *testing.T) {
	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{tweak: fastTiming(nil)})
	sh := startedShare(t, e)
	fs.mu.Lock()
	fs.shares = nil
	fs.conns[len(fs.conns)-1].room = ""
	fs.mu.Unlock()
	fs.kill()
	waitOffers(t, fs, "1/1 2/1")
	once := []protocol.MessageType{protocol.MessageTypeRoomJoin, protocol.MessageTypeShareStart, protocol.MessageTypePCOffer}
	if got := fs.settledTypes(); !slices.Equal(got, slices.Concat(once, once)) {
		t.Errorf("the server got %v, want room.join, share.start and a pub offer, then the same again", got)
	}
	starts := received[protocol.ShareStart](fs, protocol.MessageTypeShareStart)
	if len(starts) != 2 || starts[1].Replaces != "s_1" || starts[1].Ref == starts[0].Ref {
		t.Errorf("share.start requests %+v, want a second one replacing s_1 with a ref of its own", starts)
	}
	if joins := received[protocol.RoomJoin](fs, protocol.MessageTypeRoomJoin); len(joins) != 2 || joins[1].RoomID != "lounge" {
		t.Errorf("room.join requests %+v, want the share's room joined again", joins)
	}
	if snap := sh.Snapshot(); snap.ID != "s_2" || snap.End != nil || e.c.Status().ConnectionID != "c_1" {
		t.Errorf("the share is %+v on %s, want it running as s_2 on c_1", snap, e.c.Status().ConnectionID)
	}
}

// TestRepublishRefused: a republish that the server refuses (share_limit: the user shares elsewhere by now) ends the
// share with the server's code (§11.9's table: error, share_limit at republish) and leaves the room it joined.
func TestRepublishRefused(t *testing.T) {
	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{tweak: fastTiming(nil)})
	sh := startedShare(t, e)
	fs.mu.Lock()
	fs.reject = map[protocol.MessageType]protocol.Error{
		protocol.MessageTypeShareStart: protocol.NewError(protocol.ErrorCodeShareLimit, protocol.ErrorScopeRequest),
	}
	fs.mu.Unlock()
	fs.setHello(func(int, protocol.Hello) fakeAnswer { return fakeAnswer{resumed: false} })
	fs.kill()
	select {
	case <-sh.Done():
	case <-time.After(waitTimeout):
		t.Fatal("the share didn't end after its republish was refused")
	}
	if end := sh.Snapshot().End; end == nil || *end != (EndInfo{Reason: EndError, Protocol: protocol.ErrorCodeShareLimit}) {
		t.Errorf("the share ended with %+v, want error, share_limit", end)
	}
	once := []protocol.MessageType{protocol.MessageTypeRoomJoin, protocol.MessageTypeShareStart}
	want := slices.Concat(once, []protocol.MessageType{protocol.MessageTypePCOffer}, once,
		[]protocol.MessageType{protocol.MessageTypeRoomLeave})
	if got := fs.settledTypes(); !slices.Equal(got, want) {
		t.Errorf("the server got %v, want %v", got, want)
	}
	if e.engine.Session() != nil || e.c.ActiveShare() != nil {
		t.Error("the engine session or the share outlived the refused republish")
	}
}

// TestRepublishRetried: a republish that the server refuses with a retryable code goes again (M1 01 §12.1):
// rate_limited after its retryAfterMs, a retryable internal once after republishRetry. A second internal ends the
// share (error, internal).
func TestRepublishRetried(t *testing.T) {
	rateLimited := protocol.NewError(protocol.ErrorCodeRateLimited, protocol.ErrorScopeRequest)
	rateLimited.RetryAfterMs = 200
	internal := protocol.NewError(protocol.ErrorCodeInternal, protocol.ErrorScopeRequest)
	for _, tc := range []struct {
		name     string
		err      protocol.Error
		always   bool          // every share.start of the republish is refused, not only the first
		retry    time.Duration // the test's republishRetry
		min, max time.Duration // the wait between the refused share.start and the next
	}{
		// republishRetry is long here, so a retry that ignored retryAfterMs would come too late.
		{name: "rate_limited", err: rateLimited, retry: 10 * time.Second, min: 150 * time.Millisecond, max: 2 * time.Second},
		{name: "internal", err: internal, retry: 300 * time.Millisecond, min: 250 * time.Millisecond, max: 2 * time.Second},
		{name: "internal twice", err: internal, always: true, retry: 300 * time.Millisecond,
			min: 250 * time.Millisecond, max: 2 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeServer(t)
			e := newEnv(t, fs, envOptions{tweak: fastTiming(func(t *timing) { t.republishRetry = tc.retry })})
			sh := startedShare(t, e)
			fs.mu.Lock()
			if tc.always {
				fs.reject = map[protocol.MessageType]protocol.Error{protocol.MessageTypeShareStart: tc.err}
			} else {
				fs.rejectOnce = map[protocol.MessageType]protocol.Error{protocol.MessageTypeShareStart: tc.err}
			}
			fs.mu.Unlock()
			fs.setHello(func(int, protocol.Hello) fakeAnswer { return fakeAnswer{resumed: false} })
			fs.kill()
			starts := func() int { return len(received[protocol.ShareStart](fs, protocol.MessageTypeShareStart)) }
			eventually(t, func() bool { return starts() >= 2 }, func() string { return "no republish" })
			refused := time.Now()
			eventually(t, func() bool { return starts() >= 3 }, func() string {
				return fmt.Sprintf("the server got %v, want the refused share.start tried again", fs.types())
			})
			if took := time.Since(refused); took < tc.min || took > tc.max {
				t.Errorf("the refused share.start went again %v later, want %v to %v", took, tc.min, tc.max)
			}
			if tc.always {
				select {
				case <-sh.Done():
				case <-time.After(waitTimeout):
					t.Fatal("the share didn't end after its republish failed twice")
				}
				if end := sh.Snapshot().End; end == nil || *end != (EndInfo{Reason: EndError, Protocol: protocol.ErrorCodeInternal}) {
					t.Errorf("the share ended with %+v, want error, internal", end)
				}
				if n := starts(); n != 3 {
					t.Errorf("the server got %d share.start, want the first, the refused republish and one retry", n)
				}
				return
			}
			waitOffers(t, fs, "1/1 1/1")
			if snap := sh.Snapshot(); snap.ID != "s_2" || snap.End != nil {
				t.Errorf("the share is %+v, want it running as s_2", snap)
			}
			if n := len(received[protocol.RoomJoin](fs, protocol.MessageTypeRoomJoin)); n != 2 {
				t.Errorf("the server got %d room.join, want one on each connection: the retry is in the room", n)
			}
		})
	}
}

// TestBeginWithoutDownTime: an actor that begins while signaling isn't ready and has no time it went down holds the
// share from now; it doesn't end it at once (a hold from the zero time would be over).
func TestBeginWithoutDownTime(t *testing.T) {
	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{})
	s := newShare(e.c, ShareRequest{RoomID: "lounge"}, func() {})
	defer s.events.flush()
	if req := s.begin(); req != nil {
		t.Fatalf("begin = %+v, want the share held", req)
	}
	defer s.rec.stopHold()
	if s.rec.downAt.IsZero() || s.rec.holdC() == nil {
		t.Fatalf("after begin the hold runs from %v (timer %v), want from now", s.rec.downAt, s.rec.hold)
	}
	select {
	case <-s.rec.holdC():
		t.Error("the hold was over at once")
	case <-time.After(100 * time.Millisecond):
	}
	if st := s.Snapshot().State; st != StateReconnecting {
		t.Errorf("the share is %s, want reconnecting", st)
	}
}

// TestStartShareAfterDrop: a share.start that a drop cut short goes again once signaling is back: with the same ref
// on a resumed connection (the server answers with the share it may have made), from room.join with a new ref on a
// new one.
func TestStartShareAfterDrop(t *testing.T) {
	for _, resumed := range []bool{true, false} {
		t.Run(fmt.Sprintf("resumed %v", resumed), func(t *testing.T) {
			fs := newFakeServer(t)
			fs.silent = []protocol.MessageType{protocol.MessageTypeShareStart}
			e := newEnv(t, fs, envOptions{tweak: fastTiming(nil)})
			e.run()
			e.waitOnline()
			started := make(chan error, 1)
			go func() {
				_, err := e.start(ctxFor(t))
				started <- err
			}()
			eventually(t, func() bool { return len(received[protocol.ShareStart](fs, protocol.MessageTypeShareStart)) == 1 },
				func() string { return "no share.start" })
			fs.mu.Lock()
			fs.silent = nil
			fs.mu.Unlock()
			if !resumed {
				fs.setHello(func(int, protocol.Hello) fakeAnswer { return fakeAnswer{resumed: false} })
			}
			fs.kill()
			if err := <-started; err != nil {
				t.Fatalf("StartShare = %v, want the share", err)
			}
			starts := received[protocol.ShareStart](fs, protocol.MessageTypeShareStart)
			joins := len(received[protocol.RoomJoin](fs, protocol.MessageTypeRoomJoin))
			switch {
			case len(starts) != 2:
				t.Errorf("share.start requests %+v, want the first and its retry", starts)
			case resumed && (starts[1].Ref != starts[0].Ref || joins != 1):
				t.Errorf("share.start %+v after %d room.join, want the same ref without a new join", starts, joins)
			case !resumed && (starts[1].Ref == starts[0].Ref || joins != 2):
				t.Errorf("share.start %+v after %d room.join, want a new ref after a new join", starts, joins)
			}
			// The actor took over the share on the connection it is on: no republish.
			waitOffers(t, fs, "1/1")
			time.Sleep(100 * time.Millisecond)
			if n := len(received[protocol.ShareStart](fs, protocol.MessageTypeShareStart)); n != 2 {
				t.Errorf("the server got %d share.start, want 2", n)
			}
		})
	}
}

// TestServerRebuild: the server's pc.restart{pub, rebuild} rebuilds the pub PC (gen + 1, neg 1, the same share), one
// for an older gen is ignored, and a second rebuild waits for the spacing.
func TestServerRebuild(t *testing.T) {
	const spacing = 600 * time.Millisecond
	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{tweak: fastTiming(func(t *timing) { t.rebuildSpacing = spacing })})
	startedShare(t, e)
	rebuild := func(gen uint32) {
		fs.send(protocol.MessageTypePCRestart, protocol.PCRestart{PC: protocol.PCKindPub, Gen: gen,
			Mode: protocol.RestartModeRebuild, Reason: protocol.RestartReasonFailed})
	}
	// Timed from the first request, which the core can't act on earlier: its spacing runs from the first rebuild's
	// start, and the first rebuilt PC's offer arrives an ICE gathering later (on windows-go up to ~100 ms more than
	// the second's), so timing from that offer under-reads the spacing.
	asked := time.Now()
	rebuild(1)
	waitOffers(t, fs, "1/1 2/1")
	offers := received[protocol.PCOffer](fs, protocol.MessageTypePCOffer)
	if !slices.Equal(offers[1].Tracks, offers[0].Tracks) || offers[1].SDP.Reveal() == offers[0].SDP.Reveal() {
		t.Error("the rebuilt PC's offer has other tracks than the first, or the same SDP: want a new PC for the same share")
	}
	rebuild(1) // the old PC's: ignored
	rebuild(2)
	waitOffers(t, fs, "1/1 2/1 3/1")
	// 20 ms: Windows' clock ticks up to 15.6 ms. Without the spacing the third offer comes about two gatherings
	// after the first request (~200 ms).
	if took := time.Since(asked); took < spacing-20*time.Millisecond {
		t.Errorf("the second rebuild's offer came %v after the first rebuild was asked for, want at least the %v "+
			"spacing", took, spacing)
	}
	if got := fs.types(); slices.Contains(got, protocol.MessageTypePCClose) {
		t.Errorf("the server got %v: a rebuild sends no pc.close", got)
	}
}

// TestRefusedOffer: an offer the server refuses (sdp_invalid) rebuilds the PC once; a second refusal within 60 s
// ends the share (error, sdp_invalid) with share.stop.
func TestRefusedOffer(t *testing.T) {
	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{tweak: fastTiming(nil)})
	sh := startedShare(t, e)
	refuse := func(gen uint32) {
		pe := protocol.NewError(protocol.ErrorCodeSDPInvalid, protocol.ErrorScopePC)
		pe.PC, pe.Gen, pe.Neg = protocol.PCKindPub, gen, 1
		fs.send(protocol.MessageTypeError, pe)
	}
	refuse(1)
	waitOffers(t, fs, "1/1 2/1")
	refuse(2)
	select {
	case <-sh.Done():
	case <-time.After(waitTimeout):
		t.Fatal("the share didn't end after the second refusal")
	}
	if end := sh.Snapshot().End; end == nil || *end != (EndInfo{Reason: EndError, Protocol: protocol.ErrorCodeSDPInvalid}) {
		t.Errorf("the share ended with %+v, want error, sdp_invalid", end)
	}
	if !slices.Contains(fs.settledTypes(), protocol.MessageTypeShareStop) {
		t.Errorf("the server got %v, want a share.stop", fs.types())
	}
}

// TestUnreachable: rebuilt PCs that never connect are rebuilt again; after 5 rebuilds the share is unreachable, and
// the rebuilds go on at the slower spacing.
func TestUnreachable(t *testing.T) {
	const slow = 3 * time.Second
	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{tweak: fastTiming(func(t *timing) {
		t.connectWithin, t.rebuildSpacing, t.unreachableSpacing = 150*time.Millisecond, 100*time.Millisecond, slow
	})})
	sh := startedShare(t, e)
	fs.send(protocol.MessageTypePCRestart, protocol.PCRestart{PC: protocol.PCKindPub, Gen: 1,
		Mode: protocol.RestartModeRebuild, Reason: protocol.RestartReasonFailed})
	waitOffers(t, fs, "1/1 2/1 3/1 4/1 5/1 6/1")
	eventually(t, func() bool { return sh.Snapshot().State == StateUnreachable },
		func() string { return fmt.Sprintf("the share is %+v, want unreachable", sh.Snapshot()) })
	time.Sleep(slow / 2)
	if got := offerList(fs); got != "[1/1 2/1 3/1 4/1 5/1 6/1]" {
		t.Errorf("the server got the pub offers %s %v after the share became unreachable, want no rebuild yet", got, slow/2)
	}
	waitOffers(t, fs, "1/1 2/1 3/1 4/1 5/1 6/1 7/1")
}

// TestReconcileStopsOrphan: after a resume, a share of the core's connection that the core doesn't have (a lost
// share.start reply left it, say) is stopped with share.stop; the core's own share stays.
func TestReconcileStopsOrphan(t *testing.T) {
	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{tweak: fastTiming(nil)})
	sh := startedShare(t, e)
	fs.mu.Lock()
	fs.shares = append(fs.shares, fakeShare{id: "s_9", connID: "c_1", room: "lounge", ref: "orphaned"})
	fs.mu.Unlock()
	fs.kill()
	eventually(t, func() bool {
		return slices.ContainsFunc(received[protocol.ShareStop](fs, protocol.MessageTypeShareStop),
			func(s protocol.ShareStop) bool { return s.ShareID == "s_9" })
	}, func() string { return fmt.Sprintf("the server got %v, want share.stop of s_9", fs.types()) })
	time.Sleep(100 * time.Millisecond)
	if stops := received[protocol.ShareStop](fs, protocol.MessageTypeShareStop); len(stops) != 1 {
		t.Errorf("share.stop requests %+v, want s_9's alone", stops)
	}
	if snap := sh.Snapshot(); snap.End != nil || snap.ID != "s_1" {
		t.Errorf("the core's share is %+v, want it running as s_1", snap)
	}
}

// TestLeaveAfterResume: a share stopped while signaling was down ends locally at once; when the connection resumes
// into the room, the core stops the share the server kept and leaves the room (D12).
func TestLeaveAfterResume(t *testing.T) {
	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{tweak: fastTiming(nil)})
	sh := startedShare(t, e)
	refuseHellos(fs)
	fs.kill()
	eventually(t, func() bool { return e.c.Status().Conn != ConnOnline }, func() string { return "still online" })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := sh.Stop(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop while offline = %v", err)
	}
	if end := sh.Snapshot().End; end == nil || end.Reason != EndUser || e.engine.Session() != nil {
		t.Fatalf("the share after Stop is %+v (engine session %v), want it ended", sh.Snapshot(), e.engine.Session())
	}
	before := len(fs.types())
	fs.setHello(nil)
	eventually(t, func() bool {
		got := fs.types()[before:]
		return slices.Contains(got, protocol.MessageTypeShareStop) && slices.Contains(got, protocol.MessageTypeRoomLeave)
	}, func() string {
		return fmt.Sprintf("the server got %v after the resume, want share.stop and room.leave", fs.types()[before:])
	})
	if stops := received[protocol.ShareStop](fs, protocol.MessageTypeShareStop); len(stops) != 1 || stops[0].ShareID != "s_1" {
		t.Errorf("share.stop requests %+v, want s_1's", stops)
	}
	eventually(t, func() bool { return e.c.roomID() == "" }, func() string { return "the core is still in a room" })
}
