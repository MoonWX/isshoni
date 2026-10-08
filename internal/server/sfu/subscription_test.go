package sfu

import (
	"context"
	"fmt"
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

func (f *feeder) send(payload []byte) {
	f.seq++
	f.ts += 3000
	profile := ProfileHigh
	if f.l.kind == webrtc.RTPCodecTypeAudio {
		profile = ""
	}
	f.l.handleRTP(rtpPkt(f.seq, f.ts, true, payload), profile, monoNow())
}

// TestSubscriptionStateEvents: what a subscription forwards as the share's layers come and go and the client changes
// its mind (02 §10.1), and what the client hears of it (02 §6.2): a SubscriptionStateEvent whenever the forwarded
// video, the audio or the reason changes, and never otherwise. A switch is reported when the new layer's keyframe has
// arrived, a pause at once, and a request alone not at all.
func TestSubscriptionStateEvents(t *testing.T) {
	s, _ := newTicklessSFU(t)
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

// TestSubTransceiverReuse: a sub PC's m-sections outlive their shares (02 §9.3). When a share ends, its two
// m-sections go inactive; once the viewer has answered that, the next subscription takes them over, sendonly again
// under its own msid and a new SSRC, instead of adding two more. A subscription that arrives before the viewer knows
// the m-sections as inactive gets new ones, and the old pair serves the one after it. No m-section is ever anything
// but sendonly or inactive: viewers can't send media on the sub PC.
func TestSubTransceiverReuse(t *testing.T) {
	s, _ := newTestSFU(t)
	ctx := testCtx(t)
	viewer, sig := join(t, s, "lounge", "bob", "c-b", RoleViewer)
	for _, id := range []ShareID{"s_1", "s_2", "s_3", "s_4", "s_5"} {
		testShare(t, s, id)
	}
	subscribe := func(ids ...ShareID) {
		t.Helper()
		var items []SubscriptionUpdate
		for _, id := range ids {
			items = append(items, SubscriptionUpdate{Share: id, Video: QualityHigh, Audio: true})
		}
		errs, err := viewer.UpdateSubscriptions(ctx, items)
		if err != nil || slices.ContainsFunc(errs, func(e error) bool { return e != nil }) {
			t.Fatalf("UpdateSubscriptions(%v) = %v, %v", ids, errs, err)
		}
	}
	client := newTestPC(t, loopbackClientAPI(t, webrtc.NetworkTypeUDP4))
	// offer waits for sub offer neg and checks its m-sections and its tracks binding.
	offer := func(neg uint32, sections ...string) subOffer {
		t.Helper()
		o := sig.waitOffers(t, int(neg))[neg-1]
		got := sectionLines(t, o.sdp)
		if !slices.Equal(got, sections) {
			t.Fatalf("sub offer %d has the m-sections\n  %s\nwant\n  %s", neg, strings.Join(got, "\n  "), strings.Join(sections, "\n  "))
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
			t.Errorf("sub offer %d binds %v, want exactly its sending m-sections %v", neg, bound, sending)
		}
		return o
	}
	answer := func(o subOffer) {
		t.Helper()
		if err := client.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: o.sdp}); err != nil {
			t.Fatal(err)
		}
		ans, err := client.CreateAnswer(nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := viewer.HandleAnswer(ctx, PCSub, 1, o.neg, completeDescription(t, client, ans)); err != nil {
			t.Fatalf("HandleAnswer(neg %d): %v", o.neg, err)
		}
	}
	end := func(id ShareID) {
		t.Helper()
		if err := s.StopShare(id, EndReasonStopped); err != nil {
			t.Fatal(err)
		}
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
	subscribe("s_1", "s_2")
	first := offer(1, "0 video sendonly s_1", "1 audio sendonly s_1", "2 video sendonly s_2", "3 audio sendonly s_2")
	answer(first)
	sig.waitPCStates(t, "sub/1:connected")

	// s_1 ends: its m-sections go inactive, and stay in the SDP.
	end("s_1")
	answer(offer(2, "0 video inactive", "1 audio inactive", "2 video sendonly s_2", "3 audio sendonly s_2"))

	// The next subscription takes them over: the same mids, sendonly, another msid and other SSRCs.
	subscribe("s_3")
	third := offer(3, "0 video sendonly s_3", "1 audio sendonly s_3", "2 video sendonly s_2", "3 audio sendonly s_2")
	for _, mid := range []string{"0", "1"} {
		if was, is := ssrcOf(first, mid), ssrcOf(third, mid); was == "" || is == "" || was == is {
			t.Errorf("m-section %s: SSRC %q for s_1 and %q for s_3, want a new one for the new track", mid, was, is)
		}
	}
	answer(third)
	// The viewer's PC took the m-sections back: Pion bound the new DownTracks, on the connected PC.
	var subs []SubscriptionState
	eventuallyTrue(t, func() bool {
		var err error
		subs, err = viewer.Subscriptions(ctx)
		return err == nil && len(subs) == 2 && subs[1].Share == "s_3" && subs[1].VideoBound && subs[1].AudioBound
	}, "the DownTracks of s_3 are bound on the reused transceivers")

	// s_2 ends, and before the viewer has answered that, the client subscribes to s_4: the m-sections of s_2 aren't
	// free yet (the viewer still knows them as s_2's), so s_4 gets two new ones, in the follow-up offer.
	end("s_2")
	fourth := offer(4, "0 video sendonly s_3", "1 audio sendonly s_3", "2 video inactive", "3 audio inactive")
	subscribe("s_4")
	answer(fourth)
	answer(offer(5, "0 video sendonly s_3", "1 audio sendonly s_3", "2 video inactive", "3 audio inactive",
		"4 video sendonly s_4", "5 audio sendonly s_4"))
	// They serve the subscription after it.
	subscribe("s_5")
	answer(offer(6, "0 video sendonly s_3", "1 audio sendonly s_3", "2 video sendonly s_5", "3 audio sendonly s_5",
		"4 video sendonly s_4", "5 audio sendonly s_4"))

	// Everything ends: six inactive m-sections, all of them spares that the viewer's answer frees.
	for _, id := range []ShareID{"s_3", "s_4", "s_5"} {
		end(id)
	}
	// The three ends may share an offer or not (the 50 ms debounce): the last one shows all six inactive.
	all := []string{"0 video inactive", "1 audio inactive", "2 video inactive", "3 audio inactive", "4 video inactive", "5 audio inactive"}
	var last subOffer
	for neg := uint32(7); ; neg++ {
		last = sig.waitOffers(t, int(neg))[neg-1]
		answer(last)
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
	pc := subPCOf(t, viewer)
	var free, video int
	if err := viewer.do(ctx, func(context.Context) error {
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
	if n := len(sig.errorEvents()); n != 0 {
		t.Errorf("%d ErrorEvents: %+v", n, sig.errorEvents())
	}
}
