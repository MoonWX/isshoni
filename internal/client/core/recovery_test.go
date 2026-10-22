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
// republish on a new connection, share.start tried again after a drop, the pub PC's rebuilds (the server's
// pc.restart, a refused offer, unreachable) and the cleanup of a share that ended while signaling was down. The ICE
// restart and the rebuilds of a real outage are the itests' (internal/server/itest, D3–D6).

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
// {replaces: the old id}, and a new pub PC of gen 1 for the new id. (The pending offer goes again first, as §11.7
// orders it; the server ignores its tracks, which name a share it no longer has.)
func TestResumeLostShare(t *testing.T) {
	fs := newFakeServer(t)
	e := newEnv(t, fs, envOptions{tweak: fastTiming(nil)})
	sh := startedShare(t, e)
	fs.mu.Lock()
	fs.shares = nil
	fs.mu.Unlock()
	fs.kill()
	eventually(t, func() bool {
		offers := received[protocol.PCOffer](fs, protocol.MessageTypePCOffer)
		last := offers[len(offers)-1]
		return last.Gen == 1 && last.Neg == 1 && len(last.Tracks) > 0 && last.Tracks[0].ShareID == "s_2"
	}, func() string {
		return fmt.Sprintf("the server got the pub offers %s, want one of gen 1 for s_2", offerList(fs))
	})
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
	rebuild(1)
	waitOffers(t, fs, "1/1 2/1")
	rebuilt := time.Now()
	offers := received[protocol.PCOffer](fs, protocol.MessageTypePCOffer)
	if !slices.Equal(offers[1].Tracks, offers[0].Tracks) || offers[1].SDP.Reveal() == offers[0].SDP.Reveal() {
		t.Error("the rebuilt PC's offer has other tracks than the first, or the same SDP: want a new PC for the same share")
	}
	rebuild(1) // the old PC's: ignored
	rebuild(2)
	waitOffers(t, fs, "1/1 2/1 3/1")
	if took := time.Since(rebuilt); took < spacing-50*time.Millisecond {
		t.Errorf("the second rebuild came %v after the first, want the %v spacing", took, spacing)
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
