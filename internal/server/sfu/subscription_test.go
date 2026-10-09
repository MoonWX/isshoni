package sfu

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

// Unit tests of layer selection (README S52; 02 §10.1): the layer a request gets, the reasons, the
// SubscriptionStateEvents, and the reuse of a sub PC's m-sections (02 §9.3). The tests with real media through
// sfutest.Harness are in simulcast_test.go.

// Slot bits, as Share.presentLocked returns them.
const (
	hasF     = uint32(1) << SlotF
	hasH     = uint32(1) << SlotH
	hasQ     = uint32(1) << SlotQ
	hasAudio = uint32(1) << SlotAudio
)

// TestSelectSlot is the table of 02 §10.1.
func TestSelectSlot(t *testing.T) {
	video, audio := webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio
	for _, tc := range []struct {
		kind    webrtc.RTPCodecType
		want    Quality
		present uint32
		slot    Slot
		ok      bool
	}{
		// high: f, else h, else q.
		{video, QualityHigh, hasF | hasH | hasQ | hasAudio, SlotF, true},
		{video, QualityHigh, hasF | hasQ, SlotF, true},
		{video, QualityHigh, hasH | hasQ, SlotH, true},
		{video, QualityHigh, hasQ | hasAudio, SlotQ, true},
		{video, QualityHigh, hasAudio, 0, false},
		{video, QualityHigh, 0, 0, false},
		// low: q, else h; never the full layer as a thumbnail.
		{video, QualityLow, hasF | hasH | hasQ, SlotQ, true},
		{video, QualityLow, hasF | hasH, SlotH, true},
		{video, QualityLow, hasF | hasAudio, 0, false},
		{video, QualityLow, 0, 0, false},
		// off: none.
		{video, QualityOff, hasF | hasQ, 0, false},
		// audio: the audio layer, when the client wants it.
		{audio, QualityHigh, hasF | hasQ | hasAudio, SlotAudio, true},
		{audio, QualityHigh, hasF | hasQ, 0, false},
		{audio, QualityOff, hasAudio, 0, false},
	} {
		slot, ok := selectSlot(tc.kind, tc.want, tc.present)
		if ok != tc.ok || (ok && slot != tc.slot) {
			t.Errorf("selectSlot(%s, %s, %04b) = %s, %v; want %s, %v", tc.kind, tc.want, tc.present, slot, ok, tc.slot, tc.ok)
		}
	}
	for slot, want := range map[Slot]Quality{SlotF: QualityHigh, SlotH: QualityLow, SlotQ: QualityLow, SlotAudio: QualityHigh} {
		if got := slot.quality(); got != want {
			t.Errorf("a viewer of layer %s gets %s, want %s", slot, got, want)
		}
	}
}

// TestShortfall: why a subscription gets less video than the client asked for, for good (02 §10.1).
func TestShortfall(t *testing.T) {
	for _, tc := range []struct {
		requested, allowed Quality
		capReason          SubReason
		present            uint32
		want               SubReason
	}{
		// A layer that serves the request exists: whatever is missing is on its way.
		{QualityHigh, QualityHigh, "", hasF | hasQ, ""},
		{QualityHigh, QualityHigh, "", hasF, ""},
		{QualityLow, QualityLow, "", hasF | hasQ, ""},
		{QualityLow, QualityLow, "", hasH, ""},
		// high from a lower layer, or from none (the share isn't live yet).
		{QualityHigh, QualityHigh, "", hasQ, SubReasonNoLayer},
		{QualityHigh, QualityHigh, "", hasH | hasQ, SubReasonNoLayer},
		{QualityHigh, QualityHigh, "", hasAudio, SubReasonNoLayer},
		{QualityHigh, QualityHigh, "", 0, SubReasonNoLayer},
		// low without a preview layer.
		{QualityLow, QualityLow, "", hasF, SubReasonNoPreviewLayer},
		{QualityLow, QualityLow, "", 0, SubReasonNoPreviewLayer},
		// The server's cap comes first, whatever the layers.
		{QualityHigh, QualityLow, SubReasonBandwidth, hasF | hasQ, SubReasonBandwidth},
		{QualityHigh, QualityLow, SubReasonBandwidth, hasF, SubReasonBandwidth},
		{QualityHigh, QualityOff, SubReasonDecoderUnavailable, hasF | hasQ, SubReasonDecoderUnavailable},
		{QualityLow, QualityOff, SubReasonCodecMismatch, 0, SubReasonCodecMismatch},
		// A cap that isn't below the request explains nothing.
		{QualityLow, QualityLow, SubReasonBandwidth, hasF | hasQ, ""},
		{QualityLow, QualityLow, SubReasonBandwidth, hasF, SubReasonNoPreviewLayer},
	} {
		if got := shortfall(tc.requested, tc.allowed, tc.capReason, tc.present); got != tc.want {
			t.Errorf("shortfall(%s, %s, %q, %04b) = %q, want %q", tc.requested, tc.allowed, tc.capReason, tc.present, got, tc.want)
		}
	}
}

// subEvents returns the SubscriptionStateEvents the Signaler got, in order.
func (r *recSignaler) subEvents() []SubscriptionStateEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []SubscriptionStateEvent
	for _, ev := range r.events {
		if e, ok := ev.(SubscriptionStateEvent); ok {
			out = append(out, e)
		}
	}
	return out
}

// eventScript checks the SubscriptionStateEvents of one Signaler against the ones a test expects, in order.
type eventScript struct {
	t    *testing.T
	sig  *recSignaler
	want []SubscriptionStateEvent
}

// next waits until the Signaler has got evs, in this order, after everything expected before, and nothing else.
func (e *eventScript) next(what string, evs ...SubscriptionStateEvent) {
	e.t.Helper()
	e.want = append(e.want, evs...)
	deadline := time.Now().Add(10 * time.Second)
	for len(e.sig.subEvents()) < len(e.want) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	e.same(what)
}

// same checks that the Signaler has got nothing but what was expected so far.
func (e *eventScript) same(what string) {
	e.t.Helper()
	if got := e.sig.subEvents(); !slices.Equal(got, e.want) {
		e.t.Fatalf("%s: the client heard\n%s\nwant\n%s", what, eventLines(got), eventLines(e.want))
	}
}

func eventLines(evs []SubscriptionStateEvent) string {
	var b strings.Builder
	for _, ev := range evs {
		fmt.Fprintf(&b, "  %s: requested %s, forwarded %s, audio %v, reason %q\n", ev.Share, ev.Requested, ev.Forwarded, ev.Audio, ev.Reason)
	}
	return b.String()
}

// connectedSubscription returns viewer's subscription to a share with both DownTracks bound as Pion binds them on a
// connected sub PC, to a writer that sends nowhere; gate is that PC's DTLS-ready gate. (The real sub PC of the
// subscription is never answered, so Pion binds nothing itself.)
func connectedSubscription(t *testing.T, viewer *Conn, id ShareID) (sub *Subscription, gate *subPC) {
	t.Helper()
	if err := viewer.do(testCtx(t), func(context.Context) error { sub = viewer.subs[id]; return nil }); err != nil || sub == nil {
		t.Fatalf("no subscription to %s (%v)", id, err)
	}
	gate = &subPC{gen: 1}
	gate.ready.Store(true)
	sub.video.binding.Store(&binding{pc: gate, writer: discardWriter{}, ssrc: 0x7000, ptFor: chromePTs})
	sub.audio.binding.Store(&binding{pc: gate, writer: discardWriter{}, ssrc: 0x7001, ptFor: map[ProfileKey]uint8{"": 111}})
	return sub, gate
}

// feeder sends packets on a Layer as its track's reader would, with sequence numbers and timestamps that go on.
type feeder struct {
	l   *Layer
	seq uint16
	ts  uint32
}

// send sends a packet in the High profile, or an audio packet.
func (f *feeder) send(payload []byte) {
	profile := ProfileHigh
	if f.l.kind == webrtc.RTPCodecTypeAudio {
		profile = ""
	}
	f.sendAs(profile, payload)
}

// sendAs sends a packet in a payload type of the given profile.
func (f *feeder) sendAs(profile ProfileKey, payload []byte) {
	f.seq++
	f.ts += 3000
	f.l.handleRTP(rtpPkt(f.seq, f.ts, true, payload), profile, monoNow())
}

// TestSubscriptionStateEvents: what a subscription forwards as the share's layers come and go and the client changes
// its mind (02 §10.1), and what the client hears of it (02 §6.2): a SubscriptionStateEvent whenever the forwarded
// video, the audio or the reason changes, and never otherwise. A switch is reported when the new layer's keyframe has
// arrived, a pause at once, and a request alone not at all. (The script waits for each event before its next step,
// so it runs with a short interval between the events the media path causes; TestSubscriptionEventsPaced has the
// real one.)
func TestSubscriptionStateEvents(t *testing.T) {
	s, _ := newTicklessSFU(t)
	s.subEventEvery = time.Millisecond
	ctx := testCtx(t)
	viewer, sig := join(t, s, "lounge", "bob", "c-b", RoleViewer)
	const id = ShareID("s_1")
	sh := testShare(t, s, id)
	script := &eventScript{t: t, sig: sig}
	request := func(video Quality, audio bool) {
		t.Helper()
		errs, err := viewer.UpdateSubscriptions(ctx, []SubscriptionUpdate{{Share: id, Video: video, Audio: audio}})
		if err != nil || errs[0] != nil {
			t.Fatalf("UpdateSubscriptions(%s, %v) = %v, %v", video, audio, errs, err)
		}
	}
	event := func(requested, forwarded Quality, audio bool, reason SubReason) SubscriptionStateEvent {
		return SubscriptionStateEvent{Share: id, Requested: requested, Forwarded: forwarded, Audio: audio, Reason: reason}
	}
	key := keyPayload(t)

	// The share is pending and has no track yet: nothing can serve the request, and the client hears that at once.
	request(QualityHigh, true)
	script.next("high on a share without layers", event(QualityHigh, QualityOff, false, SubReasonNoLayer))
	sub, gate := connectedSubscription(t, viewer, id)
	if got := sub.video.interest.Load() | sub.audio.interest.Load(); got != 0 {
		t.Errorf("interest of a subscription to a share without layers = %04b", got)
	}

	// The preview layer's track arrives first. Before attach returns, the DownTrack wants it: the track's first
	// packet, the start of its first keyframe, is forwarded, and nobody has to ask the publisher for another.
	q, pubQ := testLayer(t, sh, SlotQ, 0xa00)
	if got := sub.video.interest.Load(); got != hasQ {
		t.Fatalf("interest right after the preview layer arrived = %04b, want q: high falls back to it", got)
	}
	feedQ := &feeder{l: q}
	feedQ.send(key)
	script.next("high served from the preview layer", event(QualityHigh, QualityLow, false, SubReasonNoLayer))
	if n := len(pubQ.plis()); n != 0 || sub.video.stats.packets.Load() != 1 {
		t.Errorf("the first packet of the new layer: %d forwarded, %d PLIs; want it forwarded without a request",
			sub.video.stats.packets.Load(), n)
	}

	// The full layer arrives: it is the target at once, and the preview layer flows until the full layer's keyframe.
	// The client hears that what it lacks is now only a matter of time (no reason: 01's "waiting"), and of the switch
	// when it has happened.
	f, pubF := testLayer(t, sh, SlotF, 0xf00)
	if got := sub.video.interest.Load(); got != hasF|hasQ {
		t.Fatalf("interest right after the full layer arrived = %04b, want f and q until the switch", got)
	}
	script.next("the full layer exists, its keyframe hasn't come", event(QualityHigh, QualityLow, false, ""))
	feedF := &feeder{l: f}
	feedQ.send(deltaPayload)
	feedF.send(key)
	script.next("the full layer's keyframe", event(QualityHigh, QualityHigh, false, ""))
	if n := len(pubF.plis()); n != 0 {
		t.Errorf("%d PLIs for a layer whose first packet started a keyframe", n)
	}
	eventuallyTrue(t, func() bool { return sub.video.interest.Load() == hasF }, "the interest is f alone after the switch")

	// The client asked for audio all along; the share had none. Now its audio track arrives.
	a, _ := testLayer(t, sh, SlotAudio, 0xaa)
	if got := sub.audio.interest.Load(); got != hasAudio {
		t.Fatalf("audio interest right after the audio track arrived = %04b", got)
	}
	feedA := &feeder{l: a}
	feedA.send([]byte{1})
	script.next("the audio flows", event(QualityHigh, QualityHigh, true, ""))

	// The full layer's track ends while the viewer gets it, and the preview layer is still (no packet of it comes).
	// The viewer's stream is over at once, not when the preview layer's next packet says so, and the publisher is
	// asked for a keyframe of the preview layer right away. Until it comes, the viewer gets nothing.
	sh.detach(f)
	if n := len(pubQ.plis()); n != 1 {
		t.Errorf("%d PLIs for q when the layer forwarded ended, want one at once", n)
	}
	if got := sub.video.interest.Load(); got != hasQ || sub.video.forwarded() != QualityOff {
		t.Errorf("after the layer forwarded ended: interest %04b, forwarded %s; want q alone and nothing forwarded", got, sub.video.forwarded())
	}
	// The audio still flows, so bob still counts as a viewer of the share.
	if info, _ := s.Share(id); len(info.Layers) != 1 || !slices.Contains(info.Viewers, ParticipantID("bob")) {
		t.Errorf("ShareInfo after the full layer ended = %+v, want one layer and bob listening", info)
	}
	script.next("the layer forwarded ended", event(QualityHigh, QualityOff, true, SubReasonNoLayer))
	feedQ.send(key)
	script.next("high from the preview layer again", event(QualityHigh, QualityLow, true, SubReasonNoLayer))
	// It comes back, as a new track.
	f, pubF = testLayer(t, sh, SlotF, 0xf01)
	feedF = &feeder{l: f, seq: 20000}
	script.next("the full layer is back, its keyframe isn't", event(QualityHigh, QualityLow, true, ""))
	feedF.send(key)
	script.next("high again after the full layer came back", event(QualityHigh, QualityHigh, true, ""))
	if n := len(pubF.plis()); n != 0 {
		t.Errorf("%d PLIs for the new full layer, whose first packet started a keyframe", n)
	}
	q.lastPLI.Store(-int64(pliInterval)) // the next step counts its own request
	pubQ.forget()

	// A request for low: nothing is reported while the full layer still flows, and the same request again changes
	// nothing; the event comes with the preview layer's keyframe.
	request(QualityLow, true)
	request(QualityLow, true)
	if n := len(pubQ.plis()); n != 1 {
		t.Errorf("%d PLIs for q after two requests for low, want 1", n)
	}
	feedF.send(deltaPayload)
	feedQ.send(deltaPayload)
	feedQ.send(key)
	script.next("the switch to low", event(QualityLow, QualityLow, true, ""))

	// The preview layer's track ends: a thumbnail never gets the full layer, so it pauses, and says why.
	sh.detach(q)
	script.next("low without a preview layer", event(QualityLow, QualityOff, true, SubReasonNoPreviewLayer))
	if got := sub.video.interest.Load(); got != 0 {
		t.Errorf("interest of a thumbnail without a preview layer = %04b, want none (never f)", got)
	}
	// It comes back (a new track): the viewer waits for its keyframe, which is no reason, and then gets it.
	q, _ = testLayer(t, sh, SlotQ, 0xa01)
	feedQ = &feeder{l: q, seq: 9000}
	script.next("the preview layer is back, its keyframe isn't", event(QualityLow, QualityOff, true, ""))
	feedQ.send(key)
	script.next("low again", event(QualityLow, QualityLow, true, ""))

	// The server's cap (02 §10.1: effective = min(requested, cap)). A cap that isn't below the request changes
	// nothing; below it, the client hears the cap's reason as soon as it gets less than it asked for.
	setCap := func(q Quality, reason SubReason) {
		t.Helper()
		if err := viewer.do(ctx, func(context.Context) error { viewer.capSubscription(sub, q, reason); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	setCap(QualityLow, SubReasonBandwidth)
	script.same("a cap at low on a request for low")
	request(QualityHigh, true)
	script.next("high under a cap at low", event(QualityHigh, QualityLow, true, SubReasonBandwidth))
	if got := sub.video.interest.Load(); got != hasQ {
		t.Errorf("interest under a cap at low = %04b, want q", got)
	}
	setCap(QualityOff, SubReasonDecoderUnavailable)
	script.next("a cap at off", event(QualityHigh, QualityOff, true, SubReasonDecoderUnavailable))
	// The cap is lifted: the full layer is the target, and until its keyframe the viewer waits, without a reason.
	setCap(QualityHigh, "")
	script.next("the cap is lifted", event(QualityHigh, QualityOff, true, ""))
	feedF.send(key)
	script.next("high again", event(QualityHigh, QualityHigh, true, ""))

	// Audio follows the request at once, in both directions, without touching the video.
	request(QualityHigh, false)
	script.next("audio off", event(QualityHigh, QualityHigh, false, ""))
	feedA.send([]byte{2})
	request(QualityHigh, true)
	feedA.send([]byte{3})
	script.next("audio on", event(QualityHigh, QualityHigh, true, ""))

	// Off is a pause, at once.
	request(QualityOff, false)
	script.next("off", event(QualityOff, QualityOff, false, ""))
	sent := sub.video.stats.packets.Load() + sub.audio.stats.packets.Load()
	feedF.send(key)
	feedQ.send(key)
	feedA.send([]byte{4})
	if got := sub.video.interest.Load() | sub.audio.interest.Load(); got != 0 || len(sub.video.queue)+len(sub.audio.queue) != 0 {
		t.Errorf("a paused subscription: interest %04b, %d packets queued", got, len(sub.video.queue)+len(sub.audio.queue))
	}

	// On again, and then the sub PC goes away under it (the DTLS-ready gate closes): the viewer gets nothing, for no
	// reason of the share's, and the client hears that with the next packet of each track.
	request(QualityHigh, true)
	script.same("a request that nothing serves yet")
	feedF.send(key)
	script.next("video on again", event(QualityHigh, QualityHigh, false, ""))
	feedA.send([]byte{5})
	script.next("audio on again", event(QualityHigh, QualityHigh, true, ""))
	if got := sub.video.stats.packets.Load() + sub.audio.stats.packets.Load(); got != sent+2 {
		t.Errorf("%d packets forwarded while paused", got-sent-2)
	}
	gate.ready.Store(false)
	feedF.send(deltaPayload)
	script.next("the gate closed: video", event(QualityHigh, QualityOff, true, ""))
	feedA.send([]byte{6})
	script.next("the gate closed: audio", event(QualityHigh, QualityOff, false, ""))

	// The share ends: the subscription goes without another word (the hub tells the clients about the share).
	if err := s.StopShare(id, EndReasonStopped); err != nil {
		t.Fatal(err)
	}
	eventuallyTrue(t, func() bool {
		n := -1
		_ = viewer.do(ctx, func(context.Context) error { n = len(viewer.subs); return nil })
		return n == 0
	}, "the subscription is gone")
	sub.changed() // a writer that was still on its way
	// The actor handles its internal events in order: once this one has run, so has everything posted before it.
	handled := make(chan struct{})
	viewer.post(func() { close(handled) })
	select {
	case <-handled:
	case <-ctx.Done():
		t.Fatal("the viewer's actor did not get to its events")
	}
	script.same("after the share ended")
}

// TestSubscriptionEventsPaced: a publisher decides how often what its viewers get changes, but not how many events
// they are sent (02 §6.2). Its stream here alternates between two profiles, so the viewer's stream begins with every
// keyframe and ends with the packet after it (02 §9.4): hundreds of changes in a few milliseconds. The DownTrack's
// writer keeps one notice in the actor's queue however many there are, and the client hears of them at most once per
// subEventInterval, the latest state each time.
func TestSubscriptionEventsPaced(t *testing.T) {
	s, _ := newTicklessSFU(t)
	ctx := testCtx(t)
	if s.subEventEvery != subEventInterval || subEventInterval != 250*time.Millisecond {
		t.Fatalf("subEventEvery = %v, subEventInterval = %v; want 250 ms", s.subEventEvery, subEventInterval)
	}
	viewer, sig := join(t, s, "lounge", "bob", "c-b", RoleViewer)
	const id = ShareID("s_1")
	sh := testShare(t, s, id)
	f, _ := testLayer(t, sh, SlotF, 0xf00)
	if errs, err := viewer.UpdateSubscriptions(ctx, []SubscriptionUpdate{{Share: id, Video: QualityHigh}}); err != nil || errs[0] != nil {
		t.Fatalf("UpdateSubscriptions = %v, %v", errs, err)
	}
	sub, _ := connectedSubscription(t, viewer, id)
	script := &eventScript{t: t, sig: sig}
	event := func(forwarded Quality) SubscriptionStateEvent {
		return SubscriptionStateEvent{Share: id, Requested: QualityHigh, Forwarded: forwarded}
	}

	// While the actor is busy, any number of changes leave one notice in its queue.
	busy, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- viewer.do(ctx, func(context.Context) error {
			close(busy)
			<-release
			return nil
		})
	}()
	<-busy
	for range 1000 {
		sub.changed()
	}
	viewer.events.mu.Lock()
	queued := len(viewer.events.items)
	viewer.events.mu.Unlock()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Errorf("%d notices in the actor's queue after 1000 changes, want 1", queued)
	}
	script.same("notices without a change")

	// The stream begins: the first change in a long while is reported at once.
	feed := &feeder{l: f}
	key := keyPayload(t)
	began := time.Now()
	feed.send(key)
	script.next("the stream begins", event(QualityHigh))
	// Then it ends and begins again 400 times, and ends. (801 packets: the DownTrack's queue holds them all.)
	const flips = 400
	for range flips {
		feed.sendAs(ProfileConstrainedBaseline, deltaPayload)
		feed.send(key)
	}
	feed.sendAs(ProfileConstrainedBaseline, deltaPayload)
	eventuallyTrue(t, func() bool { return len(sub.video.queue) == 0 && sub.video.forwarded() == QualityOff }, "the writer is through")
	flood := time.Since(began)
	if got, drops := sub.video.stats.switches.Load(), sub.video.stats.drops.Load(); got != flips+1 || drops != 0 {
		t.Fatalf("the viewer's stream began %d times (%d packets dropped), want %d", got, drops, flips+1)
	}
	// The client hears of all that once, when the interval since the first event is over: how it ended.
	ended := func() bool {
		evs := sig.subEvents()
		return evs[len(evs)-1] == event(QualityOff)
	}
	eventuallyTrue(t, ended, "the client hears that the stream ended")
	heard := time.Since(began)
	time.Sleep(subEventInterval + 50*time.Millisecond) // nothing follows
	evs := sig.subEvents()
	// Two events, unless the machine was so slow that the changes took longer than the interval.
	if most := 2 + int(flood/subEventInterval); len(evs) > most || !ended() {
		t.Errorf("%d changes in %v made %d events, want at most %d and the last to say that nothing is forwarded:\n%s",
			2*flips+2, flood, len(evs), most, eventLines(evs))
	}
	if len(evs) == 2 && heard < subEventInterval {
		t.Errorf("the second event came %v after the stream began, want it no sooner than %v after the first", heard, subEventInterval)
	}
	script.want = evs

	// What the client asks for itself is answered at once, however recent the last event: a pause.
	feed.send(key)
	script.next("the stream begins again", event(QualityHigh))
	if errs, err := viewer.UpdateSubscriptions(ctx, []SubscriptionUpdate{{Share: id, Video: QualityOff}}); err != nil || errs[0] != nil {
		t.Fatalf("UpdateSubscriptions = %v, %v", errs, err)
	}
	script.want = append(script.want, SubscriptionStateEvent{Share: id, Requested: QualityOff})
	script.same("a pause right after another event: it is reported before UpdateSubscriptions returns")
}

// TestForwardedLayerEnds: a layer ends while viewers get it and the share has another one (02 §10.1, §9.7). For
// each of those viewers the stream is over at once: it forwards nothing, counts as no viewer and hears so, until the
// keyframe of the layer it falls back to, which the publisher is asked for right away, once however many viewers
// wait for it and not at all for a viewer whose sub PC can't send. Viewers of another layer notice nothing. A layer
// that only gives way to a new track in its slot (the same stream with another SSRC) ends nobody's stream.
func TestForwardedLayerEnds(t *testing.T) {
	s, _ := newTicklessSFU(t)
	s.subEventEvery = time.Millisecond
	ctx := testCtx(t)
	const id, lonely = ShareID("s_1"), ShareID("s_2")
	sh, sh2 := testShare(t, s, id), testShare(t, s, lonely)
	f, _ := testLayer(t, sh, SlotF, 0xf00)
	q, pubQ := testLayer(t, sh, SlotQ, 0xa00)
	key := keyPayload(t)

	type client struct {
		conn   *Conn
		sub    *Subscription
		script *eventScript
	}
	request := func(c *Conn, share ShareID, video Quality) {
		t.Helper()
		if errs, err := c.UpdateSubscriptions(ctx, []SubscriptionUpdate{{Share: share, Video: video}}); err != nil || errs[0] != nil {
			t.Fatalf("UpdateSubscriptions(%s, %s) = %v, %v", share, video, errs, err)
		}
	}
	view := func(user UserID, share ShareID, video Quality, connected bool) client {
		t.Helper()
		conn, sig := join(t, s, "lounge", user, ConnID("c-"+user), RoleViewer)
		request(conn, share, video)
		sub, gate := connectedSubscription(t, conn, share)
		gate.ready.Store(connected)
		return client{conn, sub, &eventScript{t: t, sig: sig}}
	}
	event := func(share ShareID, requested, forwarded Quality, reason SubReason) SubscriptionStateEvent {
		return SubscriptionStateEvent{Share: share, Requested: requested, Forwarded: forwarded, Reason: reason}
	}
	viewers := func() []ParticipantID {
		info, _ := s.Share(id)
		return info.Viewers
	}

	// Two viewers of the full layer, one of the preview layer, and one who is on the way from the full layer to the
	// preview layer: eve has asked for low and still gets f, until q's keyframe.
	ann, bob := view("ann", id, QualityHigh, true), view("bob", id, QualityHigh, true)
	dan, eve := view("dan", id, QualityLow, true), view("eve", id, QualityHigh, true)
	feedF, feedQ := &feeder{l: f}, &feeder{l: q}
	feedF.send(key)
	feedQ.send(key)
	for _, c := range []client{ann, bob, eve} {
		c.script.next("the full layer", event(id, QualityHigh, QualityHigh, ""))
	}
	dan.script.next("the preview layer", event(id, QualityLow, QualityLow, ""))
	request(eve.conn, id, QualityLow)
	if got := eve.sub.video.interest.Load(); got != hasF|hasQ || len(pubQ.plis()) != 1 {
		t.Fatalf("on the way from f to q: interest %04b, %d PLIs for q; want both layers and one request", got, len(pubQ.plis()))
	}
	q.lastPLI.Store(-int64(pliInterval)) // what follows counts its own requests
	pubQ.forget()

	// ---- the full layer ends ----
	sh.detach(f)
	if n, throttled := len(pubQ.plis()), q.stats.pliThrottled.Load(); n != 1 || throttled != 0 {
		t.Errorf("%d PLIs for q and %d requests throttled when the full layer ended; want one request for its three "+
			"viewers, at once", n, throttled)
	}
	for name, c := range map[string]client{"ann": ann, "bob": bob, "eve": eve} {
		if got := c.sub.video.interest.Load(); got != hasQ || c.sub.video.forwarded() != QualityOff {
			t.Errorf("%s after the full layer ended: interest %04b, forwarded %s; want q alone, and nothing", name, got, c.sub.video.forwarded())
		}
	}
	if got := viewers(); !slices.Equal(got, []ParticipantID{"dan"}) {
		t.Errorf("the share's viewers = %v, want only dan, whose layer goes on", got)
	}
	ann.script.next("ann's layer ended", event(id, QualityHigh, QualityOff, SubReasonNoLayer))
	bob.script.next("bob's layer ended", event(id, QualityHigh, QualityOff, SubReasonNoLayer))
	eve.script.next("the layer eve was leaving ended", event(id, QualityLow, QualityOff, ""))
	if dan.sub.video.forwarded() != QualityLow || dan.sub.video.stats.switches.Load() != 1 {
		t.Errorf("dan, who never got the full layer: forwarded %s, %d stream starts", dan.sub.video.forwarded(), dan.sub.video.stats.switches.Load())
	}
	// The preview layer's keyframe: they go on with it.
	feedQ.send(key)
	ann.script.next("ann falls back", event(id, QualityHigh, QualityLow, SubReasonNoLayer))
	bob.script.next("bob falls back", event(id, QualityHigh, QualityLow, SubReasonNoLayer))
	eve.script.next("eve arrives", event(id, QualityLow, QualityLow, ""))
	dan.script.same("dan heard nothing")
	if got := viewers(); !slices.Equal(got, []ParticipantID{"ann", "bob", "dan", "eve"}) {
		t.Errorf("the share's viewers = %v, want all four again", got)
	}

	// ---- a viewer whose sub PC can't send: nobody is asked for a keyframe that viewer couldn't get ----
	f2, _ := testLayer(t, sh2, SlotF, 0xf02)
	_, pubQ2 := testLayer(t, sh2, SlotQ, 0xa02)
	cat := view("cat", lonely, QualityHigh, false)
	sh2.detach(f2)
	cat.script.next("the layer cat waited for ended", event(lonely, QualityHigh, QualityOff, SubReasonNoLayer))
	if n := len(pubQ2.plis()); n != 0 || cat.sub.video.interest.Load() != hasQ {
		t.Errorf("%d PLIs for a viewer who isn't connected (interest %04b), want none, and q as the layer", n, cat.sub.video.interest.Load())
	}

	// ---- a track that comes again with another SSRC takes its slot over: nothing ends ----
	f, _ = testLayer(t, sh, SlotF, 0xf03)
	ann.script.next("the full layer is back", event(id, QualityHigh, QualityLow, ""))
	feedF = &feeder{l: f, seq: 30000}
	feedF.send(key)
	ann.script.next("ann gets the full layer again", event(id, QualityHigh, QualityHigh, ""))
	q.lastPLI.Store(-int64(pliInterval))
	pubQ.forget()
	again, pubAgain := testLayer(t, sh, SlotF, 0xf04) // attached in f's place, as Conn.onPubTrack does it
	sh.detach(f)                                      // and only then is the old track's Layer taken off
	if n := len(pubQ.plis()); n != 0 || ann.sub.video.interest.Load() != hasF || ann.sub.video.forwarded() != QualityHigh {
		t.Errorf("after the full layer's track was replaced: %d PLIs for q, ann's interest %04b, forwarded %s; want ann "+
			"to wait for the new track", n, ann.sub.video.interest.Load(), ann.sub.video.forwarded())
	}
	(&feeder{l: again, seq: 40000}).send(key)
	eventuallyTrue(t, func() bool { return ann.sub.video.stats.switches.Load() == 4 }, "ann's stream goes on with the new track")
	if n := len(pubAgain.plis()); n != 0 {
		t.Errorf("%d PLIs for a track whose first packet started a keyframe", n)
	}
	ann.script.same("the full layer's track was replaced")
}

// eventuallyTrue polls cond for up to 10 s.
func eventuallyTrue(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestSubscriptionOfPendingShare: the reasons a new subscription to a share without tracks gets, by request, and
// that a share whose sharer sends no audio leaves Audio false in the event however long the client asks for it.
func TestSubscriptionOfPendingShare(t *testing.T) {
	s, _ := newTicklessSFU(t)
	ctx := testCtx(t)
	viewer, sig := join(t, s, "lounge", "bob", "c-b", RoleViewer)
	testShare(t, s, "s_1")
	testShare(t, s, "s_2")
	sh3 := testShare(t, s, "s_3")
	errs, err := viewer.UpdateSubscriptions(ctx, []SubscriptionUpdate{
		{Share: "s_1", Video: QualityLow, Audio: true}, {Share: "s_2", Video: QualityOff}, {Share: "s_3", Video: QualityHigh, Audio: true},
	})
	if err != nil || slices.ContainsFunc(errs, func(e error) bool { return e != nil }) {
		t.Fatalf("UpdateSubscriptions = %v, %v", errs, err)
	}
	script := &eventScript{t: t, sig: sig}
	// Low and high each have their reason. Off on any share is what a new subscription is: nothing to say.
	script.next("subscriptions to shares without layers",
		SubscriptionStateEvent{Share: "s_1", Requested: QualityLow, Reason: SubReasonNoPreviewLayer},
		SubscriptionStateEvent{Share: "s_3", Requested: QualityHigh, Reason: SubReasonNoLayer})

	// s_3 gets its video, and no audio track ever.
	sub, _ := connectedSubscription(t, viewer, "s_3")
	f, _ := testLayer(t, sh3, SlotF, 0xf00)
	script.next("the layer exists, its keyframe hasn't come", SubscriptionStateEvent{Share: "s_3", Requested: QualityHigh})
	(&feeder{l: f}).send(keyPayload(t))
	script.next("the video of a share without audio",
		SubscriptionStateEvent{Share: "s_3", Requested: QualityHigh, Forwarded: QualityHigh})
	if got := sub.audio.interest.Load(); got != 0 || sub.audio.forwarded() != QualityOff {
		t.Errorf("the audio DownTrack of a share without audio: interest %04b, forwarded %s", got, sub.audio.forwarded())
	}
	if info, _ := s.Share("s_3"); info.Audio || !slices.Equal(info.Viewers, []ParticipantID{"bob"}) {
		t.Errorf("ShareInfo = %+v, want no audio and bob watching", info)
	}
}

// subPCOf returns the sub PC of a Conn, read on its actor.
func subPCOf(t *testing.T, c *Conn) *subPC {
	t.Helper()
	var s *subPC
	if err := c.do(testCtx(t), func(context.Context) error { s = c.sub; return nil }); err != nil || s == nil {
		t.Fatalf("no sub PC (%v)", err)
	}
	return s
}

// sectionLines describes the m-sections of a sub offer, each as "0 video sendonly s_1" or "1 audio inactive".
func sectionLines(t *testing.T, raw string) []string {
	t.Helper()
	desc := &sdp.SessionDescription{}
	if err := desc.UnmarshalString(raw); err != nil {
		t.Fatalf("unparsable sub offer: %v", err)
	}
	var out []string
	for _, md := range desc.MediaDescriptions {
		mid, _ := md.Attribute(sdp.AttrKeyMID)
		line := mid + " " + md.MediaName.Media
		for _, dir := range []string{sdp.AttrKeySendOnly, sdp.AttrKeyInactive, sdp.AttrKeySendRecv, sdp.AttrKeyRecvOnly} {
			if _, ok := md.Attribute(dir); ok {
				line += " " + dir
			}
		}
		if msid, ok := md.Attribute("msid"); ok {
			stream, _, _ := strings.Cut(msid, " ")
			line += " " + stream
		}
		out = append(out, line)
	}
	return out
}

// subRig negotiates a viewer's sub PC with a real client PeerConnection on loopback, one offer at a time.
type subRig struct {
	t      *testing.T
	s      *SFU
	viewer *Conn
	sig    *recSignaler
	client *webrtc.PeerConnection
	// edit, when set, changes the client's answers on their way to the SFU.
	edit func(string) string
}

// newSubRig joins a viewer to a new SFU; its client's PeerConnection comes from api.
func newSubRig(t *testing.T, api *webrtc.API) *subRig {
	t.Helper()
	s, _ := newTestSFU(t)
	viewer, sig := join(t, s, "lounge", "bob", "c-b", RoleViewer)
	return &subRig{t: t, s: s, viewer: viewer, sig: sig, client: newTestPC(t, api)}
}

// subscribe asks for the shares, high with audio.
func (r *subRig) subscribe(ids ...ShareID) {
	r.t.Helper()
	var items []SubscriptionUpdate
	for _, id := range ids {
		items = append(items, SubscriptionUpdate{Share: id, Video: QualityHigh, Audio: true})
	}
	errs, err := r.viewer.UpdateSubscriptions(testCtx(r.t), items)
	if err != nil || slices.ContainsFunc(errs, func(e error) bool { return e != nil }) {
		r.t.Fatalf("UpdateSubscriptions(%v) = %v, %v", ids, errs, err)
	}
}

// offer waits for sub offer neg and checks its m-sections (sectionLines) and that its tracks binding names exactly
// the sending ones.
func (r *subRig) offer(neg uint32, sections ...string) subOffer {
	r.t.Helper()
	o := r.sig.waitOffers(r.t, int(neg))[neg-1]
	got := sectionLines(r.t, o.sdp)
	if !slices.Equal(got, sections) {
		r.t.Fatalf("sub offer %d has the m-sections\n  %s\nwant\n  %s", neg, strings.Join(got, "\n  "), strings.Join(sections, "\n  "))
	}
	var bound []string
	for _, b := range o.tracks {
		bound = append(bound, fmt.Sprintf("%s %s sendonly %s", b.MID, b.Kind, b.Share))
	}
	var sending []string
	for _, line := range sections {
		if strings.Contains(line, " sendonly ") {
			sending = append(sending, line)
		}
	}
	if !slices.Equal(bound, sending) {
		r.t.Errorf("sub offer %d binds %v, want exactly its sending m-sections %v", neg, bound, sending)
	}
	return o
}

// answer has the client answer a sub offer.
func (r *subRig) answer(o subOffer) {
	r.t.Helper()
	if err := r.client.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: o.sdp}); err != nil {
		r.t.Fatal(err)
	}
	ans, err := r.client.CreateAnswer(nil)
	if err != nil {
		r.t.Fatal(err)
	}
	raw := completeDescription(r.t, r.client, ans)
	if r.edit != nil {
		raw = r.edit(raw)
	}
	if err := r.viewer.HandleAnswer(testCtx(r.t), PCSub, 1, o.neg, raw); err != nil {
		r.t.Fatalf("HandleAnswer(neg %d): %v", o.neg, err)
	}
}

// end stops a share and waits until the viewer's actor has dealt with that: its subscription to the share is gone.
func (r *subRig) end(id ShareID) {
	r.t.Helper()
	if err := r.s.StopShare(id, EndReasonStopped); err != nil {
		r.t.Fatal(err)
	}
	eventuallyTrue(r.t, func() bool {
		gone := false
		err := r.viewer.do(testCtx(r.t), func(context.Context) error { gone = r.viewer.subs[id] == nil; return nil })
		return err == nil && gone
	}, "the subscription to "+string(id)+" is gone")
}

// bindings waits until Pion has bound both DownTracks of the viewer's subscription to a share, and returns what it
// bound them to.
func (r *subRig) bindings(id ShareID) (video, audio *binding) {
	r.t.Helper()
	eventuallyTrue(r.t, func() bool {
		err := r.viewer.do(testCtx(r.t), func(context.Context) error {
			if sub := r.viewer.subs[id]; sub != nil {
				video, audio = sub.video.binding.Load(), sub.audio.binding.Load()
			}
			return nil
		})
		return err == nil && video != nil && audio != nil
	}, "the DownTracks of "+string(id)+" are bound")
	return video, audio
}

// TestSubTransceiverReuse: a sub PC's m-sections outlive their shares (02 §9.3). When a share ends, its two
// m-sections go inactive; once the viewer has answered that, the next subscription takes them over, sendonly again
// under its own msid and a new SSRC, instead of adding two more. A subscription that arrives before the viewer knows
// the m-sections as inactive gets new ones, and the old pair serves the one after it. No m-section is ever anything
// but sendonly or inactive: viewers can't send media on the sub PC.
func TestSubTransceiverReuse(t *testing.T) {
	r := newSubRig(t, loopbackClientAPI(t, webrtc.NetworkTypeUDP4))
	ctx := testCtx(t)
	for _, id := range []ShareID{"s_1", "s_2", "s_3", "s_4", "s_5"} {
		testShare(t, r.s, id)
	}
	ssrcOf := func(o subOffer, mid string) string {
		t.Helper()
		desc := &sdp.SessionDescription{}
		if err := desc.UnmarshalString(o.sdp); err != nil {
			t.Fatal(err)
		}
		for _, md := range desc.MediaDescriptions {
			if got, _ := md.Attribute(sdp.AttrKeyMID); got == mid {
				ssrc, _ := md.Attribute(sdp.AttrKeySSRC)
				first, _, _ := strings.Cut(ssrc, " ")
				return first
			}
		}
		t.Fatalf("sub offer %d has no m-section %s", o.neg, mid)
		return ""
	}

	// Two shares: four m-sections.
	r.subscribe("s_1", "s_2")
	first := r.offer(1, "0 video sendonly s_1", "1 audio sendonly s_1", "2 video sendonly s_2", "3 audio sendonly s_2")
	r.answer(first)
	r.sig.waitPCStates(t, "sub/1:connected")

	// s_1 ends: its m-sections go inactive, and stay in the SDP.
	r.end("s_1")
	r.answer(r.offer(2, "0 video inactive", "1 audio inactive", "2 video sendonly s_2", "3 audio sendonly s_2"))

	// The next subscription takes them over: the same mids, sendonly, another msid and other SSRCs.
	r.subscribe("s_3")
	third := r.offer(3, "0 video sendonly s_3", "1 audio sendonly s_3", "2 video sendonly s_2", "3 audio sendonly s_2")
	for _, mid := range []string{"0", "1"} {
		if was, is := ssrcOf(first, mid), ssrcOf(third, mid); was == "" || is == "" || was == is {
			t.Errorf("m-section %s: SSRC %q for s_1 and %q for s_3, want a new one for the new track", mid, was, is)
		}
	}
	r.answer(third)
	// The viewer's PC took the m-sections back: Pion bound the new DownTracks, on the connected PC.
	var subs []SubscriptionState
	eventuallyTrue(t, func() bool {
		var err error
		subs, err = r.viewer.Subscriptions(ctx)
		return err == nil && len(subs) == 2 && subs[1].Share == "s_3" && subs[1].VideoBound && subs[1].AudioBound
	}, "the DownTracks of s_3 are bound on the reused transceivers")

	// s_2 ends, and before the viewer has answered that, the client subscribes to s_4: the m-sections of s_2 aren't
	// free yet (the viewer still knows them as s_2's), so s_4 gets two new ones, in the follow-up offer.
	r.end("s_2")
	fourth := r.offer(4, "0 video sendonly s_3", "1 audio sendonly s_3", "2 video inactive", "3 audio inactive")
	r.subscribe("s_4")
	r.answer(fourth)
	r.answer(r.offer(5, "0 video sendonly s_3", "1 audio sendonly s_3", "2 video inactive", "3 audio inactive",
		"4 video sendonly s_4", "5 audio sendonly s_4"))
	// They serve the subscription after it.
	r.subscribe("s_5")
	r.answer(r.offer(6, "0 video sendonly s_3", "1 audio sendonly s_3", "2 video sendonly s_5", "3 audio sendonly s_5",
		"4 video sendonly s_4", "5 audio sendonly s_4"))

	// Everything ends: six inactive m-sections, all of them spares that the viewer's answer frees.
	for _, id := range []ShareID{"s_3", "s_4", "s_5"} {
		r.end(id)
	}
	// The three ends may share an offer or not (the 50 ms debounce): the last one shows all six inactive.
	all := []string{"0 video inactive", "1 audio inactive", "2 video inactive", "3 audio inactive", "4 video inactive", "5 audio inactive"}
	var last subOffer
	for neg := uint32(7); ; neg++ {
		last = r.sig.waitOffers(t, int(neg))[neg-1]
		r.answer(last)
		if slices.Equal(sectionLines(t, last.sdp), all) {
			break
		}
		if neg > 10 {
			t.Fatalf("no sub offer with every m-section inactive; the last has %v", sectionLines(t, last.sdp))
		}
	}
	if len(last.tracks) != 0 {
		t.Errorf("the offer without shares binds %+v", last.tracks)
	}
	pc := subPCOf(t, r.viewer)
	var free, video int
	if err := r.viewer.do(ctx, func(context.Context) error {
		for _, sp := range pc.spares {
			if sp.free {
				free++
			}
			if sp.tr.Kind() == webrtc.RTPCodecTypeVideo {
				video++
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if free != 6 || video != 3 {
		t.Errorf("%d free spares, %d of them video; want 6 and 3", free, video)
	}
	if n := len(r.sig.errorEvents()); n != 0 {
		t.Errorf("%d ErrorEvents: %+v", n, r.sig.errorEvents())
	}
}

// TestSubTransceiverReuseBinding: a DownTrack that takes over the m-section of an ended share is bound to what its
// viewer negotiated, exactly as one on a new m-section is (02 §9.3). The viewer here takes less than the SFU offers:
// one H.264 profile, no RTX, no abs-send-time. A sender of the SFU's subscribe API instead of the PeerConnection's
// would bind the second share's DownTracks to everything the SFU offers: payload types the viewer never accepted,
// an RTX stream and a header extension it doesn't know.
func TestSubTransceiverReuseBinding(t *testing.T) {
	m := &webrtc.MediaEngine{}
	for kind, codec := range map[webrtc.RTPCodecType]webrtc.RTPCodecParameters{
		webrtc.RTPCodecTypeVideo: {
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: h264FmtpPrefix + "42e01f",
			},
			PayloadType: 102,
		},
		webrtc.RTPCodecTypeAudio: {
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
			PayloadType:        111,
		},
	} {
		if err := m.RegisterCodec(codec, kind); err != nil {
			t.Fatal(err)
		}
	}
	r := newSubRig(t, loopbackClientAPIWith(t, m, webrtc.NetworkTypeUDP4))
	testShare(t, r.s, "s_1")
	testShare(t, r.s, "s_2")

	r.subscribe("s_1")
	r.answer(r.offer(1, "0 video sendonly s_1", "1 audio sendonly s_1"))
	r.sig.waitPCStates(t, "sub/1:connected")
	video1, audio1 := r.bindings("s_1")
	// What the viewer negotiated: the SFU's payload type for Constrained Baseline, which also carries Baseline.
	if want := map[ProfileKey]uint8{ProfileConstrainedBaseline: 96, "4200": 96}; !maps.Equal(video1.ptFor, want) ||
		len(video1.rtxPTFor) != 0 || video1.absSendTimeID != 0 || video1.rtxSSRC != 0 || video1.unsupported {
		t.Fatalf("the video binding on a new m-section = %+v, want the payload types %v, no RTX, no abs-send-time", video1, want)
	}

	r.end("s_1")
	r.answer(r.offer(2, "0 video inactive", "1 audio inactive"))
	r.subscribe("s_2")
	r.answer(r.offer(3, "0 video sendonly s_2", "1 audio sendonly s_2"))
	video2, audio2 := r.bindings("s_2")
	for _, b := range []struct {
		kind       string
		new, reuse *binding
	}{{"video", video1, video2}, {"audio", audio1, audio2}} {
		if b.reuse == b.new || b.reuse.ssrc == b.new.ssrc {
			t.Errorf("%s: the binding on the reused m-section is the first share's (SSRC %d and %d)", b.kind, b.new.ssrc, b.reuse.ssrc)
		}
		if !maps.Equal(b.reuse.ptFor, b.new.ptFor) || !maps.Equal(b.reuse.rtxPTFor, b.new.rtxPTFor) ||
			b.reuse.absSendTimeID != b.new.absSendTimeID || b.reuse.rtxSSRC != b.new.rtxSSRC || b.reuse.unsupported != b.new.unsupported {
			t.Errorf("%s: the binding on the reused m-section = %+v, on the new one %+v; want the same negotiation", b.kind, b.reuse, b.new)
		}
	}
	if n := len(r.sig.errorEvents()); n != 0 {
		t.Errorf("%d ErrorEvents: %+v", n, r.sig.errorEvents())
	}
}

// TestSubSpareNotFreeYet: Pion's AddTrack goes by the viewer's last answer alone, so it would also reuse an m-section
// that had a DownTrack for a moment since then: one that was taken over and lost its share again before the viewer
// answered. The viewer may know that m-section as the ended share's by now, so it isn't free (02 §9.3), and the
// subscription that Pion would have put there gets new m-sections instead, although older free ones exist: Pion
// picks the oldest it can use, and only that one can be checked.
func TestSubSpareNotFreeYet(t *testing.T) {
	r := newSubRig(t, loopbackClientAPI(t, webrtc.NetworkTypeUDP4))
	for _, id := range []ShareID{"s_1", "s_2", "s_3", "s_4", "s_5"} {
		testShare(t, r.s, id)
	}
	r.subscribe("s_1", "s_2")
	r.answer(r.offer(1, "0 video sendonly s_1", "1 audio sendonly s_1", "2 video sendonly s_2", "3 audio sendonly s_2"))
	r.sig.waitPCStates(t, "sub/1:connected")
	r.end("s_1")
	r.answer(r.offer(2, "0 video inactive", "1 audio inactive", "2 video sendonly s_2", "3 audio sendonly s_2"))
	r.end("s_2")
	r.answer(r.offer(3, "0 video inactive", "1 audio inactive", "2 video inactive", "3 audio inactive"))

	// s_3 takes the first pair over and ends before the viewer has answered the offer that says so.
	r.subscribe("s_3")
	fourth := r.offer(4, "0 video sendonly s_3", "1 audio sendonly s_3", "2 video inactive", "3 audio inactive")
	r.end("s_3")
	// For Pion that pair is still as the last answer left it, inactive, and the oldest to reuse. s_4 must not get it.
	r.subscribe("s_4")
	r.answer(fourth)
	r.answer(r.offer(5, "0 video inactive", "1 audio inactive", "2 video inactive", "3 audio inactive",
		"4 video sendonly s_4", "5 audio sendonly s_4"))
	r.bindings("s_4")
	// Now the viewer knows every m-section but s_4's as inactive: the oldest pair is the next to be taken.
	r.subscribe("s_5")
	r.answer(r.offer(6, "0 video sendonly s_5", "1 audio sendonly s_5", "2 video inactive", "3 audio inactive",
		"4 video sendonly s_4", "5 audio sendonly s_4"))
	r.bindings("s_5")
	if n := len(r.sig.errorEvents()); n != 0 {
		t.Errorf("%d ErrorEvents: %+v", n, r.sig.errorEvents())
	}
}

// TestSubSpareNotInactive: a spare transceiver is reused only once the viewer's answer has left its m-section
// inactive, which is also when Pion's AddTrack takes it (02 §9.3). A viewer that answers the inactive m-sections of
// an ended share with a direction, against JSEP, keeps them from being reused: the next subscription gets new
// sendonly m-sections. Reusing them anyway would end with Pion adding a sendrecv transceiver, and a viewer must never
// be able to send media on the sub PC. If the SFU's list and Pion ever disagree all the same, the transceiver Pion
// adds is stopped before an offer shows it.
func TestSubSpareNotInactive(t *testing.T) {
	r := newSubRig(t, loopbackClientAPI(t, webrtc.NetworkTypeUDP4))
	ctx := testCtx(t)
	for _, id := range []ShareID{"s_1", "s_2", "s_3", "s_4"} {
		testShare(t, r.s, id)
	}
	spares := func() (free, all int) {
		t.Helper()
		pc := subPCOf(t, r.viewer)
		if err := r.viewer.do(ctx, func(context.Context) error {
			for _, sp := range pc.spares {
				if all++; sp.free {
					free++
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return free, all
	}

	r.subscribe("s_1")
	r.answer(r.offer(1, "0 video sendonly s_1", "1 audio sendonly s_1"))
	r.sig.waitPCStates(t, "sub/1:connected")

	// s_1 ends, and the viewer answers its inactive m-sections as if it still received on them.
	r.end("s_1")
	r.edit = func(raw string) string { return strings.ReplaceAll(raw, "a=inactive", "a=recvonly") }
	r.answer(r.offer(2, "0 video inactive", "1 audio inactive"))
	r.edit = nil
	if free, all := spares(); free != 0 || all != 2 {
		t.Fatalf("%d of %d spares are free after an answer that left them receiving, want none of 2", free, all)
	}
	// The next subscription gets its own m-sections.
	r.subscribe("s_2")
	r.answer(r.offer(3, "0 video inactive", "1 audio inactive", "2 video sendonly s_2", "3 audio sendonly s_2"))
	r.bindings("s_2")
	// That answer left the first two inactive, as it should: they are free after all.
	if free, all := spares(); free != 2 || all != 2 {
		t.Fatalf("%d of %d spares are free after a proper answer, want both", free, all)
	}
	r.subscribe("s_3")
	r.answer(r.offer(4, "0 video sendonly s_3", "1 audio sendonly s_3", "2 video sendonly s_2", "3 audio sendonly s_2"))
	r.bindings("s_3")

	// The SFU's list is wrong for once: it calls the m-sections of s_3 free before the viewer has answered that they
	// are inactive. Pion finds nothing to reuse and adds a transceiver for each DownTrack of s_4; they are stopped, and
	// s_4 gets sendonly m-sections like any other subscription.
	r.end("s_3")
	fifth := r.offer(5, "0 video inactive", "1 audio inactive", "2 video sendonly s_2", "3 audio sendonly s_2")
	pc := subPCOf(t, r.viewer)
	if err := r.viewer.do(ctx, func(context.Context) error {
		for _, sp := range pc.spares {
			sp.free = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.subscribe("s_4")
	r.answer(fifth)
	r.answer(r.offer(6, "0 video inactive", "1 audio inactive", "2 video sendonly s_2", "3 audio sendonly s_2",
		"4 video inactive", "5 video sendonly s_4", "6 audio inactive", "7 audio sendonly s_4"))
	r.bindings("s_4")
	if free, all := spares(); free != 4 || all != 4 {
		t.Errorf("%d of %d spares are free, want the two of s_3 and the two Pion added", free, all)
	}
	if n := len(r.sig.errorEvents()); n != 0 {
		t.Errorf("%d ErrorEvents: %+v", n, r.sig.errorEvents())
	}
}
