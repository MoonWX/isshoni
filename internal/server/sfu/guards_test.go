package sfu

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// The unit tests of resilience and guards (README S57): the parts of 02 §5.3 and §12 that need no media and no
// client that loses its network. The acceptance tests, with real PeerConnections, are in recovery_test.go.

// TestPCWindow: the window of the PC creation guard (02 §12): 10 within the window pass, the next one doesn't and
// counts for nothing, and the wait is the time until the oldest leaves.
func TestPCWindow(t *testing.T) {
	const window = time.Minute
	at := func(d time.Duration) int64 { return int64(time.Hour + d) }
	var w pcWindow
	for i := range maxPCCreations {
		if wait, ok := w.admit(at(time.Duration(i)*time.Second), window); !ok || wait != 0 {
			t.Fatalf("creation %d: admitted %v, wait %v", i+1, ok, wait)
		}
	}
	// The ten were made at 0 s … 9 s. At 30 s the oldest has 30 s left; a refused one doesn't push the window on.
	for range 3 {
		if wait, ok := w.admit(at(30*time.Second), window); ok || wait != 30*time.Second {
			t.Fatalf("an eleventh creation at 30 s: admitted %v, wait %v; want it refused for 30 s", ok, wait)
		}
	}
	if wait, ok := w.admit(at(window-time.Nanosecond), window); ok || wait != time.Nanosecond {
		t.Errorf("just before the oldest leaves: admitted %v, wait %v", ok, wait)
	}
	// At 60 s the first one has left: one more passes, and the next waits for the one made at 1 s.
	if _, ok := w.admit(at(window), window); !ok {
		t.Error("a creation a full window after the oldest was refused")
	}
	if wait, ok := w.admit(at(window), window); ok || wait != time.Second {
		t.Errorf("the next one: admitted %v, wait %v; want it refused for 1 s", ok, wait)
	}
	// Each second from then on frees one place, in the order they were taken.
	for i := 1; i < maxPCCreations; i++ {
		if _, ok := w.admit(at(window+time.Duration(i)*time.Second), window); !ok {
			t.Fatalf("the creation at 60 s + %d s was refused", i)
		}
	}
	if wait, ok := w.admit(at(window+9*time.Second), window); ok || wait != 51*time.Second {
		t.Errorf("after ten more: admitted %v, wait %v; want it refused until the one made at 60 s leaves", ok, wait)
	}
}

// TestActorTimer: a timer of the actor runs its work on the actor, once; set again it runs only the new work; and a
// stopped one never runs, not even when its time has already passed.
func TestActorTimer(t *testing.T) {
	s, _ := newTestSFU(t)
	c, _ := join(t, s, "lounge", "alice", "c-a", RoleFull)
	ctx := testCtx(t)
	var (
		timer actorTimer
		ran   []string // the actor's
	)
	onActor := func(fn func()) {
		t.Helper()
		if err := c.do(ctx, func(context.Context) error { fn(); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	runs := func() []string {
		var out []string
		onActor(func() { out = slices.Clone(ran) })
		return out
	}
	waitRuns := func(want ...string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !slices.Equal(runs(), want) {
			if time.Now().After(deadline) {
				t.Fatalf("the timer's work ran as %v, want %v", runs(), want)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}

	onActor(func() {
		c.after(&timer, 20*time.Millisecond, func() { ran = append(ran, "first") })
		if !timer.pending() {
			t.Error("a timer that was just set isn't pending")
		}
	})
	waitRuns("first")
	onActor(func() {
		if timer.pending() {
			t.Error("a timer that ran is still pending")
		}
		// Set, then set again before it runs: only the second work runs.
		c.after(&timer, 20*time.Millisecond, func() { ran = append(ran, "replaced") })
		c.after(&timer, 30*time.Millisecond, func() { ran = append(ran, "second") })
	})
	waitRuns("first", "second")

	// Stopped after its time.Timer fired but before the actor got to it: the actor is busy until then.
	onActor(func() {
		c.after(&timer, time.Millisecond, func() { ran = append(ran, "stopped") })
		time.Sleep(50 * time.Millisecond) // the post is in the queue by now
		timer.stop()
		if timer.pending() {
			t.Error("a stopped timer is pending")
		}
	})
	// Work that sets the timer again, as the re-send of a sub offer does.
	onActor(func() {
		c.after(&timer, 10*time.Millisecond, func() {
			ran = append(ran, "third")
			c.after(&timer, 10*time.Millisecond, func() { ran = append(ran, "fourth") })
		})
	})
	waitRuns("first", "second", "third", "fourth")

	// A timer of a closed Conn never runs.
	onActor(func() { c.after(&timer, 20*time.Millisecond, func() { t.Error("a timer ran on a closed Conn") }) })
	c.Close(EndReasonLeft)
	waitDone(t, c)
	time.Sleep(60 * time.Millisecond)
}

// TestRestartUnderWay: one sub ICE restart at a time (02 §5.3). A restart is under way while it waits for its offer,
// while its offer has no answer, and for 5 s after its offer went out, unless ICE has connected or failed since
// (which clears restartAt).
func TestRestartUnderWay(t *testing.T) {
	const spacing = iceRestartSpacing
	now := int64(time.Hour)
	ago := func(d time.Duration) int64 { return now - int64(d) }
	for name, tc := range map[string]struct {
		pc   *subPC
		want bool
	}{
		"none so far":                         {&subPC{}, false},
		"waiting for its offer":               {&subPC{restartQueued: true}, true},
		"its offer is outstanding":            {&subPC{offering: true, offer: subOffer{restart: true}, restartAt: ago(time.Minute)}, true},
		"another offer is outstanding":        {&subPC{offering: true}, false},
		"offered 1 s ago":                     {&subPC{restartAt: ago(time.Second)}, true},
		"offered just under 5 s ago":          {&subPC{restartAt: ago(spacing - time.Millisecond)}, true},
		"offered 5 s ago":                     {&subPC{restartAt: ago(spacing)}, false},
		"offered long ago, answered":          {&subPC{restartAt: ago(time.Minute), offer: subOffer{restart: true}}, false},
		"connected or failed since its offer": {&subPC{restartAt: 0, offer: subOffer{restart: true}}, false},
	} {
		if got := tc.pc.restartUnderWay(now, spacing); got != tc.want {
			t.Errorf("%s: under way = %v, want %v", name, got, tc.want)
		}
	}
}

// answerSub has a Pion client answer the newest sub offer of a Conn, and gives the answer to the Conn.
func answerSub(t *testing.T, viewer *Conn, client *webrtc.PeerConnection, gen uint32, offer subOffer) {
	t.Helper()
	if err := client.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.sdp}); err != nil {
		t.Fatal(err)
	}
	answer, err := client.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := viewer.HandleAnswer(testCtx(t), PCSub, gen, offer.neg, completeDescription(t, client, answer)); err != nil {
		t.Fatalf("answer to sub offer gen %d neg %d: %v", gen, offer.neg, err)
	}
}

// subNegotiation is a sub PC's negotiation state, as negotiation reads it.
type subNegotiation struct {
	gen, neg                          uint32
	offering, dirty, closed, answered bool
	queued                            bool  // an ICE restart waits for its offer
	restart                           bool  // the last offer was an ICE restart
	restartAt                         int64 // when it went out; 0 once ICE connected or failed since
	// Which timers are set.
	debounce, resend, handshake, grace bool
}

// negotiation reads the negotiation state of a Conn's sub PC on the actor.
func negotiation(t *testing.T, c *Conn) (st subNegotiation) {
	t.Helper()
	if err := c.do(testCtx(t), func(context.Context) error {
		s := c.sub
		if s == nil {
			return fmt.Errorf("no sub PC")
		}
		st = subNegotiation{
			gen: s.gen, neg: s.neg, offering: s.offering, dirty: s.dirty, closed: s.closed, answered: s.answered,
			queued: s.restartQueued, restart: s.offer.restart, restartAt: s.restartAt,
			debounce: s.debounce != nil, resend: s.resend.pending(), handshake: s.handshake.pending(),
			grace: s.grace.pending(),
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return st
}

// TestSubOfferResend: a sub offer without an answer is sent again every 15 s (here 100 ms), with the same gen, neg,
// SDP and tracks, until the answer comes (02 §5.3).
func TestSubOfferResend(t *testing.T) {
	s, _ := newTestSFU(t)
	if s.timing != defaultTimings() || s.timing.offerResend != 15*time.Second {
		t.Errorf("the SFU's timings = %+v, want the defaults of config.go", s.timing)
	}
	s.timing.offerResend = 100 * time.Millisecond
	ctx := testCtx(t)
	pub, _ := join(t, s, "lounge", "alice", "c-a", RoleFull)
	viewer, sig := join(t, s, "lounge", "bob", "c-b", RoleViewer)
	startShare(t, pub, "s_1")
	start := time.Now()
	if _, err := viewer.UpdateSubscriptions(ctx, []SubscriptionUpdate{{Share: "s_1", Video: QualityLow}}); err != nil {
		t.Fatal(err)
	}
	offers := sig.waitOffers(t, 4)
	if took := time.Since(start); took < 300*time.Millisecond {
		t.Errorf("four sends of the offer took %v, want the first and three more, 100 ms apart", took)
	}
	sig.mu.Lock()
	gens := slices.Clone(sig.gens)
	sig.mu.Unlock()
	for i, o := range offers[:4] {
		if gens[i] != 1 || o.neg != 1 || o.sdp != offers[0].sdp || !slices.Equal(o.tracks, offers[0].tracks) {
			t.Errorf("send %d of the offer: gen %d, neg %d, the same SDP %v; want gen 1, neg 1, as first sent", i+1, gens[i],
				o.neg, o.sdp == offers[0].sdp)
		}
	}
	if st := negotiation(t, viewer); !st.offering || st.neg != 1 || !st.resend || st.handshake {
		t.Errorf("the sub PC while its offer is out = %+v, want it offering neg 1 with the re-send set and no handshake timer", st)
	}

	// The answer ends it, and starts the PC's handshake timer.
	client := newTestPC(t, loopbackClientAPI(t, webrtc.NetworkTypeUDP4))
	answerSub(t, viewer, client, 1, offers[0])
	if st := negotiation(t, viewer); st.offering || st.resend || !st.answered {
		t.Errorf("the sub PC after the answer = %+v, want it idle without a re-send", st)
	}
	sent := len(sig.sent())
	time.Sleep(350 * time.Millisecond)
	if n := len(sig.sent()); n != sent {
		t.Errorf("%d more sends of the offer after its answer", n-sent)
	}
	sig.waitPCStates(t, "sub/1:connected")
	if st := negotiation(t, viewer); st.handshake || st.grace {
		t.Errorf("the connected sub PC = %+v, want no timer running", st)
	}
}

// TestSubICERestartRules: when a request for a sub ICE restart starts one (02 §5.3). Not before the PC's first
// answer; at once on an idle PC, as the next neg; behind the answer when an offer is outstanding; and not while one
// is under way.
func TestSubICERestartRules(t *testing.T) {
	s, _ := newTestSFU(t)
	ctx := testCtx(t)
	pub, _ := join(t, s, "lounge", "alice", "c-a", RoleFull)
	viewer, sig := join(t, s, "lounge", "bob", "c-b", RoleViewer)
	startShare(t, pub, "s_1")
	startShare(t, pub, "s_2")
	subscribe := func(id ShareID) {
		t.Helper()
		errs, err := viewer.UpdateSubscriptions(ctx, []SubscriptionUpdate{{Share: id, Video: QualityLow}})
		if err != nil || errs[0] != nil {
			t.Fatalf("subscribe %s: %v, %v", id, errs, err)
		}
	}
	ufrag := func(o subOffer) string {
		t.Helper()
		desc, err := parseSDP(o.sdp)
		if err != nil {
			t.Fatal(err)
		}
		v, _ := desc.MediaDescriptions[0].Attribute("ice-ufrag")
		return v
	}
	restart := func() {
		t.Helper()
		if err := viewer.RestartICE(ctx, PCSub, 1); err != nil {
			t.Fatalf("RestartICE: %v", err)
		}
	}

	// Before the first answer there is no ICE to restart: the offer that is out starts it.
	subscribe("s_1")
	first := sig.waitOffers(t, 1)[0]
	restart()
	viewer.Resync() // sends the offer again, and restarts nothing either
	second := sig.waitOffers(t, 2)[1]
	if st := negotiation(t, viewer); st.queued || st.dirty || st.neg != 1 || second.neg != 1 || second.sdp != first.sdp {
		t.Errorf("after requests before the first answer: %+v, and the offer sent again is neg %d; want nothing queued", st, second.neg)
	}
	client := newTestPC(t, loopbackClientAPI(t, webrtc.NetworkTypeUDP4))
	answerSub(t, viewer, client, 1, first)
	sig.waitPCStates(t, "sub/1:connected")
	time.Sleep(4 * subOfferDebounce)
	if n := len(sig.sent()); n != 2 {
		t.Fatalf("%d sends after the first answer, want the offer and its repeat", n)
	}

	// A connected PC: Resync restarts nothing; the client's request does, as the next neg with new credentials.
	viewer.Resync()
	time.Sleep(4 * subOfferDebounce)
	if n := len(sig.sent()); n != 2 {
		t.Fatalf("Resync restarted the ICE of a connected sub PC: %d sends", n)
	}
	restart()
	if st := negotiation(t, viewer); (!st.queued || !st.debounce) && st.neg != 2 { // or the 50 ms are over already
		t.Errorf("after RestartICE on an idle PC: %+v, want the restart waiting behind the debounce", st)
	}
	restart() // again while it waits: still one
	third := sig.waitOffers(t, 3)[2]
	if third.neg != 2 || ufrag(third) == ufrag(first) || !slices.Equal(third.tracks, first.tracks) {
		t.Errorf("the restart's offer: neg %d, a new ICE user name %v, tracks %+v; want neg 2 with new credentials and "+
			"the tracks as they were", third.neg, ufrag(third) != ufrag(first), third.tracks)
	}
	if st := negotiation(t, viewer); st.queued || !st.offering || !st.restart || st.restartAt == 0 || st.handshake {
		t.Errorf("after the restart's offer: %+v, want it outstanding, with its time kept and no handshake timer", st)
	}
	// While its offer is out, and for 5 s after its answer unless ICE connects first, further requests start nothing.
	restart()
	viewer.Resync() // the offer again: the PC isn't connected, and the restart is under way
	fourth := sig.waitOffers(t, 4)[3]
	if fourth.neg != 2 || fourth.sdp != third.sdp {
		t.Errorf("Resync sent neg %d, want the restart's offer again", fourth.neg)
	}
	if st := negotiation(t, viewer); st.queued || st.dirty {
		t.Errorf("requests while the restart's offer is out queued something: %+v", st)
	}
	answerSub(t, viewer, client, 1, third)
	sig.waitPCStates(t, "sub/1:connected", "sub/1:connected")
	if st := negotiation(t, viewer); st.restartAt != 0 || st.offering {
		t.Errorf("after ICE connected again: %+v, want the restart over", st)
	}
	if n := len(sig.sent()); n != 4 {
		t.Fatalf("%d sends, want 4", n)
	}

	// A restart asked for while another offer is outstanding follows that offer's answer, as one more offer.
	subscribe("s_2")
	fifth := sig.waitOffers(t, 5)[4]
	if fifth.neg != 3 || ufrag(fifth) != ufrag(third) {
		t.Fatalf("the offer for the new subscription: neg %d, new ICE credentials %v; want neg 3 and no restart", fifth.neg,
			ufrag(fifth) != ufrag(third))
	}
	restart()
	if st := negotiation(t, viewer); !st.queued || !st.dirty || !st.offering || st.neg != 3 {
		t.Errorf("a restart asked for while an offer is out: %+v, want it queued behind the answer", st)
	}
	time.Sleep(4 * subOfferDebounce)
	if n := len(sig.sent()); n != 5 {
		t.Fatalf("%d sends: the restart didn't wait for the outstanding answer", n)
	}
	answerSub(t, viewer, client, 1, fifth)
	sixth := sig.waitOffers(t, 6)[5]
	if sixth.neg != 4 || ufrag(sixth) == ufrag(third) || len(sixth.tracks) != 4 {
		t.Errorf("the restart after the answer: neg %d, a new ICE user name %v, %d tracks; want neg 4, new credentials and "+
			"all four tracks", sixth.neg, ufrag(sixth) != ufrag(third), len(sixth.tracks))
	}
	answerSub(t, viewer, client, 1, sixth)
	sig.waitPCStates(t, "sub/1:connected", "sub/1:connected", "sub/1:connected")
	if errs := sig.errorEvents(); len(errs) != 0 {
		t.Errorf("error events: %+v", errs)
	}
}

// TestShareStalled: the media states of 02 §5.3 past the first keyframe. A live share is stalled when its pub PC
// hasn't been connected for 2 s (the ticker's check), at once when that PC failed, and at once when its last video
// track ended. It is live again with the first keyframe that arrives while the pub PC is connected. A pending share
// is never stalled, and the first time a share went live stays its LiveAt.
func TestShareStalled(t *testing.T) {
	s, ev := newTicklessSFU(t)
	sh := testShare(t, s, "s_1")
	pending := testShare(t, s, "s_2")
	c := sh.conn
	now := t0
	tick := func(wait time.Duration) []string {
		now += int64(wait)
		s.everySecond(now)
		s.reportShares(now)
		return ev.take()
	}
	// down is what the actor does when it sees the pub PC leave connected; up, when it is connected again.
	down := func() {
		c.pubUp.Store(true)
		c.pubDownAt.Store(now)
		c.pubUp.Store(false)
	}
	up := func() { c.pubUp.Store(true) }
	wantCalls := func(what string, got []string, want ...string) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Errorf("%s: room events = %v, want %v", what, got, want)
		}
	}
	wantPLIs := func(what string, rec *rtcpRecorder, want int) {
		t.Helper()
		if n := len(rec.plis()); n != want {
			t.Errorf("%s: %d PLIs, want %d", what, n, want)
		}
		rec.forget()
	}

	f, pubF := testLayer(t, sh, SlotF, 0xf00)
	q, _ := testLayer(t, sh, SlotQ, 0xa00)
	a, _ := testLayer(t, sh, SlotAudio, 0xaa)
	testLayer(t, pending, SlotF, 0xf02)
	pending.conn.pubDownAt.Store(1) // its pub PC has been away for ever
	pending.conn.pubUp.Store(false)
	sh.keyframe(f, ProfileHigh)
	wantCalls("the first keyframe", tick(0), "updated s_1 live f+q+audio")
	liveAt := sh.liveAt
	if c.pubAway(now) != 0 {
		t.Errorf("pubAway with a connected pub PC = %v", c.pubAway(now))
	}

	// ---- the pub PC is disconnected: live for 2 s more, then stalled ----
	down()
	wantCalls("1 s after the pub PC went away", tick(time.Second))
	wantCalls("just under 2 s", tick(stalledAfter-time.Second-time.Millisecond))
	wantCalls("2 s", tick(time.Millisecond), "updated s_1 stalled f+q+audio")
	wantCalls("stalled already", tick(time.Second))
	if st, _ := sh.liveState(); st != ShareStalled || !sh.awaitsKeyframe.Load() {
		t.Errorf("the share = %v, awaiting a keyframe %v; want stalled and waiting", st, sh.awaitsKeyframe.Load())
	}
	// A keyframe while the actor has the PC as away changes nothing, and nobody is asked for another one.
	pubF.forget()
	if sh.keyframe(f, ProfileHigh) {
		t.Error("a keyframe made the share live while its pub PC is away")
	}
	f.handleRTP(rtpPkt(10, 10, false, deltaPayload), ProfileHigh, now)
	wantPLIs("a packet of a stalled share whose pub PC is away", pubF, 0)
	wantCalls("still stalled", tick(time.Second))
	// The PC is back: a packet that is no keyframe asks for one, and the keyframe makes the share live.
	up()
	f.handleRTP(rtpPkt(11, 11, false, deltaPayload), ProfileHigh, now)
	wantPLIs("a packet of a stalled share whose pub PC is back", pubF, 1)
	f.handleRTP(rtpPkt(12, 12, false, keyPayload(t)), ProfileConstrainedBaseline, now)
	wantCalls("the keyframe after the PC came back", tick(0), "updated s_1 live f+q+audio")
	if !sh.liveAt.Equal(liveAt) || sh.awaitsKeyframe.Load() {
		t.Errorf("live again: LiveAt moved (%v), or the share still asks for keyframes (%v)", !sh.liveAt.Equal(liveAt),
			sh.awaitsKeyframe.Load())
	}
	// A short absence, under 2 s, is none.
	down()
	wantCalls("away for 1 s", tick(time.Second))
	up()
	wantCalls("back", tick(time.Second))

	// ---- the pub PC failed: stalled at once ----
	down()
	c.stallShares("the pub PC failed")
	wantCalls("the pub PC failed", tick(0), "updated s_1 stalled f+q+audio")
	c.stallShares("again")
	wantCalls("stalled already", tick(3*time.Second))
	up()
	sh.keyframe(q, ProfileConstrainedBaseline) // any video layer's keyframe will do
	wantCalls("a keyframe on the preview layer", tick(0), "updated s_1 live f+q+audio")

	// ---- the tracks end: stalled when the last video track is gone, and not before ----
	sh.detach(a)
	sh.detach(q)
	wantCalls("audio and one video layer gone", tick(time.Second), "updated s_1 live f")
	sh.detach(f)
	wantCalls("the last video layer gone", tick(0), "updated s_1 stalled ")
	if sh.keyframe(f, ProfileHigh) {
		t.Error("a keyframe of a track that has ended made the share live")
	}
	// The next pub PC's tracks attach to the same share; changes of a stalled share aren't reported one by one, the
	// keyframe's report has them all.
	f2, _ := testLayer(t, sh, SlotF, 0xf10)
	testLayer(t, sh, SlotAudio, 0xab)
	wantCalls("new tracks, no keyframe yet", tick(time.Second))
	sh.keyframe(f2, ProfileHigh)
	wantCalls("the new track's keyframe", tick(0), "updated s_1 live f+audio")

	// ---- a share that never went live is never stalled, and an ended one neither ----
	wantCalls("the pending share", tick(time.Hour))
	pending.stall("test")
	pending.conn.stallShares("test")
	if st, _ := pending.liveState(); st != SharePending {
		t.Errorf("the pending share = %v", st)
	}
	if err := s.StopShare("s_1", EndReasonStopped); err != nil {
		t.Fatal(err)
	}
	ev.take()
	sh.stall("after its end")
	sh.detach(f2)
	down()
	wantCalls("after the end", tick(time.Hour))
	if m := s.Metrics().Shares; m[SharePending.String()] != 1 || m[ShareLive.String()] != 0 || m[ShareStalled.String()] != 0 {
		t.Errorf("shares by state = %v, want the one pending share", m)
	}
}

// idContext is a webrtc.TrackLocalContext with an id of its own, as each RTPSender has.
type idContext struct {
	fakeTrackContext
	id string
}

func (c idContext) ID() string { return c.id }

// TestUnbindOfAnOlderSender: Unbind ends the binding of the sender that stopped and no other. When Pion closes a sub
// PC by itself, its senders stop from a goroutine of Pion's, after the PC counts as closed: the rebuilt sub PC may
// have bound the DownTrack by then, and that binding must stay.
func TestUnbindOfAnOlderSender(t *testing.T) {
	s, _ := newTestSFU(t)
	viewer, _ := join(t, s, "lounge", "bob", "c-b", RoleViewer)
	sh := testShare(t, s, "s_1")
	d := newDownTrack(&Subscription{conn: viewer, share: sh}, webrtc.RTPCodecTypeVideo)
	codecs := []webrtc.RTPCodecParameters{h264Param(96, "42e01f")}
	oldPC, newPC := &subPC{gen: 1}, &subPC{gen: 2}
	oldCtx, newCtx := idContext{fakeTrackContext{codecs: codecs}, "old"}, idContext{fakeTrackContext{codecs: codecs}, "new"}

	d.pc.Store(oldPC)
	if _, err := d.Bind(oldCtx); err != nil {
		t.Fatal(err)
	}
	// The rebuild: the DownTrack joins the new PC and is bound there, before the old sender's stop arrives.
	d.pc.Store(newPC)
	if _, err := d.Bind(newCtx); err != nil {
		t.Fatal(err)
	}
	if err := d.Unbind(oldCtx); err != nil {
		t.Fatal(err)
	}
	if b := d.binding.Load(); b == nil || b.pc != newPC || b.id != "new" {
		t.Fatalf("after the old sender's Unbind the binding is %+v, want the new PC's kept", b)
	}
	// The same Unbind again, and one of a sender that was never bound: nothing.
	_ = d.Unbind(oldCtx)
	_ = d.Unbind(idContext{id: "other"})
	if d.binding.Load() == nil {
		t.Fatal("a stray Unbind ended the binding")
	}
	if err := d.Unbind(newCtx); err != nil || d.binding.Load() != nil {
		t.Errorf("the current sender's Unbind: %v, still bound %v", err, d.binding.Load() != nil)
	}
}

// TestCloseStopsTimers: Close with every timer of a Conn's PCs running. The timers stop, nothing of them runs
// afterwards, and the Conn's actor ends.
func TestCloseStopsTimers(t *testing.T) {
	s, _ := newTestSFU(t)
	s.timing.handshake, s.timing.offerResend = 150*time.Millisecond, 150*time.Millisecond
	ctx := testCtx(t)
	c, sig := join(t, s, "lounge", "alice", "c-a", RoleFull)
	other, _ := join(t, s, "lounge", "bob", "c-b", RoleFull)
	startShare(t, c, "s_1")
	startShare(t, other, "s_2")
	// A pub PC whose handshake timer runs (nobody completes ICE), and a sub PC whose offer nobody answers.
	_, raw, tracks := pubOfferFrom(t, "s_1")
	if _, err := c.HandleOffer(ctx, PCPub, 1, 1, raw, tracks); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpdateSubscriptions(ctx, []SubscriptionUpdate{{Share: "s_2", Video: QualityLow}}); err != nil {
		t.Fatal(err)
	}
	sig.waitOffers(t, 1)
	var pub *pubPC
	var sub *subPC
	if err := c.do(ctx, func(context.Context) error {
		pub, sub = c.pub, c.sub
		if !pub.handshake.pending() || !sub.resend.pending() || sub.handshake.pending() {
			return fmt.Errorf("timers before Close: pub handshake %v, sub re-send %v, sub handshake %v",
				pub.handshake.pending(), sub.resend.pending(), sub.handshake.pending())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c.Close(EndReasonLeft)
	waitDone(t, c)
	if pub.handshake.pending() || pub.grace.pending() || sub.resend.pending() || sub.handshake.pending() || sub.debounce != nil {
		t.Error("a timer of a closed Conn's PCs is still set")
	}
	sent, states := len(sig.sent()), len(sig.pcStates())
	time.Sleep(400 * time.Millisecond)
	if n, m := len(sig.sent()), len(sig.pcStates()); n != sent || m != states {
		t.Errorf("after Close: %d more offers, PC events %v", n-sent, sig.pcStates())
	}
	if m := s.Metrics().HandshakeTimeouts; m["pub"] != 0 || m["sub"] != 0 {
		t.Errorf("handshake timeouts = %v, want none: the Conn closed first", m)
	}
}

// TestHandshakeTimeoutShort: the handshake timeout with a short timer (TestHandshakeTimeout has it in real time).
// The pub PC of a client that never starts ICE is closed and reported once, as failed with the reason; its gen is
// over; the next gen gets a timer of its own.
func TestHandshakeTimeoutShort(t *testing.T) {
	s, _ := newTestSFU(t)
	s.timing.handshake = 200 * time.Millisecond
	ctx := testCtx(t)
	c, sig := join(t, s, "lounge", "alice", "c-a", RoleFull)
	startShare(t, c, "s_1")
	for gen := uint32(1); gen <= 2; gen++ {
		_, raw, tracks := pubOfferFrom(t, "s_1")
		start := time.Now()
		if _, err := c.HandleOffer(ctx, PCPub, gen, 1, raw, tracks); err != nil {
			t.Fatal(err)
		}
		// A re-offer on the same PC doesn't start the timer again.
		time.Sleep(100 * time.Millisecond)
		if _, err := c.HandleOffer(ctx, PCPub, gen, 1, raw, tracks); err != nil {
			t.Fatal(err)
		}
		var want []string
		for g := uint32(1); g <= gen; g++ {
			want = append(want, fmt.Sprintf("pub/%d:failed", g))
		}
		sig.waitPCStates(t, want...)
		if took := time.Since(start); took < 200*time.Millisecond || took > 2*time.Second {
			t.Errorf("gen %d was closed after %v, want the 200 ms of its handshake timeout", gen, took)
		}
		if g, exists := pubState(t, c); g != gen || exists {
			t.Errorf("after the timeout of gen %d: pub gen %d, PC exists %v", gen, g, exists)
		}
		_, err := c.HandleOffer(ctx, PCPub, gen, 2, raw, tracks)
		wantErr(t, err, CodeBadPC)
	}
	sig.mu.Lock()
	events := slices.Clone(sig.events)
	sig.mu.Unlock()
	for _, ev := range events {
		if st, ok := ev.(PCStateEvent); !ok || st.State != "failed" || st.Reason != PCReasonHandshakeTimeout {
			t.Errorf("the client got %+v, want only the two timeouts", ev)
		}
	}
	if m := s.Metrics().HandshakeTimeouts; m["pub"] != 2 || m["sub"] != 0 {
		t.Errorf("handshake timeouts = %v, want two for pub", m)
	}
}
