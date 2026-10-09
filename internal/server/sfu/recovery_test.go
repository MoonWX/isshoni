package sfu_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/server/sfu"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// The acceptance tests of resilience and guards (README S57): integration 7, 8, 9, 11, 14 and 15 of 02 §17, and the
// rest of the recovery cases of 02 §5.3 and §6.5, through sfutest.Harness with real PeerConnections, fake media and
// clients whose UDP a FaultConn can take away. The handshake timeout runs in real time (02 §17); the tests of the
// 30 s grace and of the stalled state shorten the SFU's timers and its ICE timeouts.

// ctxFor returns a context for a test that takes longer than testCtx allows.
func ctxFor(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// faultySettings returns loopback settings for a client whose one UDP socket goes through a FaultConn, so that a
// test can take its network away, and give it back, without a word to anybody.
func faultySettings(t *testing.T) (webrtc.SettingEngine, *sfutest.FaultConn) {
	t.Helper()
	var lc net.ListenConfig
	sock, err := lc.ListenPacket(context.Background(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	wire := sfutest.NewFaultConn(sock)
	mux := webrtc.NewICEUDPMux(nil, wire)
	t.Cleanup(func() { _ = mux.Close() }) // after the client's own cleanup, which is registered later and runs first
	se := sfutest.LoopbackSettings()
	se.SetICEUDPMux(mux)
	return se, wire
}

// blackHole takes a client's UDP away (nothing in, nothing out) or gives it back.
func blackHole(wire *sfutest.FaultConn, on bool) {
	if on {
		wire.SetDrop(func(sfutest.Direction, net.Addr, []byte) bool { return true })
	} else {
		wire.SetDrop(nil)
	}
}

// iceUfrag returns the ICE user name of a description.
func iceUfrag(t *testing.T, raw string) string {
	t.Helper()
	desc := parse(t, raw)
	if v, ok := desc.Attribute("ice-ufrag"); ok {
		return v
	}
	for _, md := range desc.MediaDescriptions {
		if v, ok := md.Attribute("ice-ufrag"); ok {
			return v
		}
	}
	t.Fatal("the description has no ice-ufrag")
	return ""
}

// pcEvents returns the PCStateEvents a Conn's client got, in order.
func pcEvents(sig *sfutest.DirectSignaler) []sfu.PCStateEvent {
	var out []sfu.PCStateEvent
	for _, ev := range sig.Events() {
		if st, ok := ev.(sfu.PCStateEvent); ok {
			out = append(out, st)
		}
	}
	return out
}

// waitPCEvent waits for the n-th PCStateEvent a Conn's client gets (from 0) and returns it with the time it was seen.
func waitPCEvent(t *testing.T, ctx context.Context, sig *sfutest.DirectSignaler, n int) (sfu.PCStateEvent, time.Time) {
	t.Helper()
	for {
		if evs := pcEvents(sig); len(evs) > n {
			return evs[n], time.Now()
		}
		select {
		case <-ctx.Done():
			t.Fatalf("PC state event %d never came: the client got %+v", n, pcEvents(sig))
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// pcOf returns the state of a Conn's PeerConnection of a kind.
func pcOf(t *testing.T, c *sfu.Conn, kind sfu.PCKind) sfu.PCState {
	t.Helper()
	st, err := c.PC(testCtx(t), kind)
	if err != nil {
		t.Fatalf("PC(%s): %v", kind, err)
	}
	return st
}

// waitResumed waits until a video Recorder's stream has started again after its first skip packets: a packet that
// starts a keyframe, and n whole frames of layer rid from there on. It returns the packets from that keyframe start
// on, and how many came before it (what was still on its way when the stream broke off).
//
// "From there on" is the stream's order, not the order of arrival: a packet that arrives after the keyframe's start
// with a sequence number before it is no part of the stream that resumed. Those are packets that the outage
// swallowed, sent again: a publisher whose network is back answers the SFU's NACKs for them, and the SFU forwards a
// late packet in its own place in the viewer's stream (02 §9.4), where a decoder that starts on the keyframe has no
// use for it. Whether they come before or after the keyframe is a matter of milliseconds.
func waitResumed(t *testing.T, ctx context.Context, r *sfutest.Recorder, skip int, rid string, n int,
) (resumed []sfutest.Packet, before int) {
	t.Helper()
	since := func(pkts []sfutest.Packet) (resumed []sfutest.Packet, before int) {
		before = slices.IndexFunc(pkts, func(p sfutest.Packet) bool { return p.KeyStart })
		if before < 0 {
			return nil, len(pkts)
		}
		first := pkts[before].Seq
		for _, p := range pkts[before:] {
			if int16(p.Seq-first) >= 0 {
				resumed = append(resumed, p)
			}
		}
		return resumed, before
	}
	err := r.Wait(ctx, func(r *sfutest.Recorder) bool {
		pkts := r.Packets()
		if len(pkts) < skip {
			return false
		}
		resumed, _ := since(pkts[skip:])
		return len(resumed) > 0 && layerFrames(resumed, rid) >= n
	})
	if err != nil {
		t.Fatalf("the stream didn't start again with a keyframe and %d frames of layer %s: %v (%d packets since)", n, rid,
			err, len(r.Packets())-skip)
	}
	return since(r.Packets()[skip:])
}

// tracksOf counts the Viewer's tracks of a kind in a share's stream: one per PeerConnection that carried it.
func tracksOf(v *sfutest.Viewer, share sfu.ShareID, kind webrtc.RTPCodecType) int {
	n := 0
	for _, r := range v.Recorders() {
		if r.Kind() == kind && r.StreamID() == string(share) {
			n++
		}
	}
	return n
}

// nextTrack waits for a track of a kind in a share's stream other than the ones in old: the track of a rebuilt sub PC.
func nextTrack(t *testing.T, ctx context.Context, v *sfutest.Viewer, share sfu.ShareID, kind webrtc.RTPCodecType,
	old ...*sfutest.Recorder,
) *sfutest.Recorder {
	t.Helper()
	r, err := v.WaitRecorder(ctx, func(r *sfutest.Recorder) bool {
		return r.Kind() == kind && r.StreamID() == string(share) && !slices.Contains(old, r)
	})
	if err != nil {
		t.Fatalf("no new %s track of %s: %v", kind, share, err)
	}
	return r
}

// shareStates returns the states of a share in the ShareUpdated calls from the from-th room event on, each run of
// one state once: "live stalled live".
func shareStates(h *sfutest.Harness, share sfu.ShareID, from int) string {
	var runs []string
	for _, ev := range h.Events.Events()[from:] {
		if ev.Kind != sfutest.ShareUpdated || ev.Share.ID != share {
			continue
		}
		if s := ev.Share.State.String(); len(runs) == 0 || runs[len(runs)-1] != s {
			runs = append(runs, s)
		}
	}
	return strings.Join(runs, " ")
}

// keptLines is a log handler that keeps the lines whose message starts with prefix, each as "message: cause", and
// drops everything else: what a test needs to tell which of two requests the SFU acted on.
type keptLines struct {
	prefix string

	mu    sync.Mutex
	lines []string
}

func (k *keptLines) Enabled(context.Context, slog.Level) bool { return true }
func (k *keptLines) WithAttrs([]slog.Attr) slog.Handler       { return k }
func (k *keptLines) WithGroup(string) slog.Handler            { return k }

func (k *keptLines) Handle(_ context.Context, r slog.Record) error {
	if !strings.HasPrefix(r.Message, k.prefix) {
		return nil
	}
	line := r.Message
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "cause" {
			line += ": " + a.Value.String()
		}
		return true
	})
	k.mu.Lock()
	defer k.mu.Unlock()
	k.lines = append(k.lines, line)
	return nil
}

// take returns the lines kept since the last take.
func (k *keptLines) take() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := k.lines
	k.lines = nil
	return out
}

// startPublisher publishes fake media of src on conn's share as pub PC gen, on a new Publisher.
func startPublisher(t *testing.T, ctx context.Context, conn *sfu.Conn, src *fake.Source, se webrtc.SettingEngine,
	gen uint32, share sfu.ShareID,
) *publish.Publisher {
	t.Helper()
	pub := newPublisher(t, src, se)
	if _, err := sfutest.Publish(ctx, conn, pub, gen, 1, share); err != nil {
		t.Fatalf("publish negotiation of %s, gen %d: %v", share, gen, err)
	}
	if err := pub.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return pub
}

// TestSubICERestart is integration 7 of 02 §17. A viewer loses its UDP for 5 s and then asks for an ICE restart: the
// SFU offers again on the same sub PC with new ICE credentials, and the viewer's media is back within 3 s of the
// network, on the same tracks, starting with a keyframe. The share never leaves live: nothing happened to it.
//
// The resume case: the viewer's signaling is gone with its UDP. When both are back, the resume (Resync) and the
// viewer's own request ask for an ICE restart within moments, and exactly one offer goes out (02 §5.3).
func TestSubICERestart(t *testing.T) {
	restarts := &keptLines{prefix: "sub ICE restart"}
	h := newHarness(t, sfutest.HarnessOptions{Logger: slog.New(restarts)})
	ctx := ctxFor(t, 90*time.Second)
	const share = sfu.ShareID("s_ice")
	_, pub := publishShare(t, h, "alice", "c-pub", share, 57)
	waitLive(t, h, share)
	viewConn, viewSig := join(t, h, "bob", "c-view", sfu.RoleViewer)
	settings, wire := faultySettings(t)
	viewer := newViewer(t, settings)
	viewSig.Attach(viewConn, viewer)
	subscribe(t, viewConn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityHigh, Audio: true})
	video, audio := streamTrack(t, viewer, share, webrtc.RTPCodecTypeVideo), streamTrack(t, viewer, share, webrtc.RTPCodecTypeAudio)
	waitFrames(t, video, 0, "f", 30)
	first, err := viewSig.WaitOffer(ctx, func(o sfutest.Offer) bool { return o.Neg == 1 })
	if err != nil {
		t.Fatal(err)
	}
	roomEvents := len(h.Events.Events())

	// Requests that are about no PC of this Conn, or about an older one.
	wantCode(t, viewConn.RestartICE(ctx, sfu.PCPub, 1), sfu.CodeBadPC)
	wantCode(t, viewConn.RestartICE(ctx, sfu.PCSub, 2), sfu.CodeBadPC)
	wantCode(t, viewConn.RestartICE(ctx, 0, 1), sfu.CodeBadPC)
	if err := viewConn.RestartICE(ctx, sfu.PCSub, 0); err != nil {
		t.Errorf("RestartICE for an older gen = %v, want it ignored", err)
	}

	// restartAfterOutage is the client's side: its UDP is back, and it asks for the restart. It returns the ICE
	// restart's offer, and checks that the media came back.
	resume := func(what string, neg uint32, last sfutest.Offer, ask func()) sfutest.Offer {
		t.Helper()
		videoMark, audioMark := len(video.Packets()), audio.Stats().Packets
		keys := layerStats(t, pub, "f").Keyframes
		ask()
		blackHole(wire, false)
		unblocked := time.Now()
		offer, err := viewSig.WaitOffer(ctx, func(o sfutest.Offer) bool { return o.Neg == neg })
		if err != nil {
			t.Fatalf("%s: no ICE-restart offer: %v", what, err)
		}
		// The same PC and the same m-sections, with new ICE credentials: that is what makes it a restart for the client.
		if offer.Gen != 1 || !slices.Equal(offer.Tracks, last.Tracks) || !slices.Equal(sectionsOf(t, offer.SDP), sectionsOf(t, last.SDP)) {
			t.Errorf("%s: the restart's offer is gen %d with %v, want gen 1 with the m-sections %v of the offer before",
				what, offer.Gen, sectionsOf(t, offer.SDP), sectionsOf(t, last.SDP))
		}
		if a, b := iceUfrag(t, last.SDP), iceUfrag(t, offer.SDP); a == b {
			t.Errorf("%s: the restart's offer has the ICE user name of the offer before", what)
		}
		checkServerCandidates(t, h, what+": the restart's offer", offer.SDP)
		resumed, stragglers := waitResumed(t, ctx, video, videoMark, "f", 15)
		took := resumed[0].Arrival.Sub(unblocked)
		if took > 3*time.Second {
			t.Errorf("%s: the video came back %v after the network, want within 3 s", what, took)
		}
		if err := sfutest.CheckContinuous(sfutest.WithoutPartialTail(resumed)); err != nil {
			t.Errorf("%s: the video since the restart: %v", what, err)
		}
		if err := audio.Wait(ctx, func(r *sfutest.Recorder) bool { return r.Stats().Packets >= audioMark+25 }); err != nil {
			t.Errorf("%s: the audio didn't come back: %v", what, err)
		}
		// The same tracks: an ICE restart changes neither the senders nor their SSRCs, so the viewer has no new track.
		if v, a := tracksOf(viewer, share, webrtc.RTPCodecTypeVideo), tracksOf(viewer, share, webrtc.RTPCodecTypeAudio); v != 1 || a != 1 {
			t.Errorf("%s: the viewer has %d video and %d audio tracks of the share, want the one of each it had", what, v, a)
		}
		// The keyframe was asked for when the sub PC was connected again.
		if got := layerStats(t, pub, "f").Keyframes; got <= keys {
			t.Errorf("%s: the publisher made no keyframe for the viewer that came back", what)
		}
		waitSubState(t, viewSig, sfu.SubscriptionStateEvent{Share: share, Requested: sfu.QualityHigh, Forwarded: sfu.QualityHigh, Audio: true})
		t.Logf("%s: video back %v after the network (%d packets of the old stream arrived first)", what,
			took.Round(time.Millisecond), stragglers)
		return offer
	}

	// ---- 5 s without UDP, then the viewer asks for an ICE restart ----
	blackHole(wire, true)
	time.Sleep(5 * time.Second)
	second := resume("ICE restart", 2, first, func() {
		if err := viewConn.RestartICE(ctx, sfu.PCSub, 1); err != nil {
			t.Fatalf("RestartICE: %v", err)
		}
	})
	if got, want := restarts.take(), []string{"sub ICE restart: the client asked"}; !slices.Equal(got, want) {
		t.Errorf("the SFU's lines about the restart = %q, want %q", got, want)
	}

	// ---- the resume: signaling and UDP both gone, long enough for the SFU to see the sub PC disconnected ----
	viewSig.SetDown(true)
	blackHole(wire, true)
	gone := time.Now()
	eventually(t, func() bool { return pcOf(t, viewConn, sfu.PCSub).State == "disconnected" },
		func() string { return "the sub PC is " + pcOf(t, viewConn, sfu.PCSub).State + ", want disconnected" })
	if d := time.Since(gone); d < 3*time.Second {
		time.Sleep(3*time.Second - d)
	}
	resume("resume", 3, second, func() {
		viewSig.SetDown(false)
		viewConn.Resync()
		if err := viewConn.RestartICE(ctx, sfu.PCSub, 1); err != nil {
			t.Fatalf("RestartICE after Resync: %v", err)
		}
	})
	// The resume asked first and started the restart; the viewer's own request found it under way.
	if got, want := restarts.take(), []string{
		"sub ICE restart: the connection resumed", "sub ICE restart skipped: one is under way: the client asked",
	}; !slices.Equal(got, want) {
		t.Errorf("the SFU's lines about the restart = %q, want %q", got, want)
	}
	time.Sleep(4 * 50 * time.Millisecond) // four debounce periods
	if offers := viewSig.Offers(); len(offers) != 3 {
		t.Errorf("%d sub offers in all, want 3: the first, the ICE restart, and one more for the resume and the "+
			"viewer's request together", len(offers))
	}
	if lost := viewSig.Lost(); len(lost) != 0 {
		t.Errorf("%d offers were sent into the dead signaling, want none: nothing changed meanwhile", len(lost))
	}

	// Through all of it: one sub PC, never failed or closed; a share that stayed live; answers without errors.
	for _, ev := range pcEvents(viewSig) {
		if ev.PC != sfu.PCSub || ev.Gen != 1 || ev.Reason != "" || (ev.State != "connected" && ev.State != "disconnected") {
			t.Errorf("the viewer got %+v", ev)
		}
	}
	if st := pcOf(t, viewConn, sfu.PCSub); st.Gen != 1 || st.Closed || st.State != "connected" || st.HandshakeTimer || st.GraceTimer {
		t.Errorf("the sub PC = %+v, want gen 1, connected, no timer running", st)
	}
	if states := shareStates(h, share, roomEvents); states != "" && states != "live" {
		t.Errorf("the share went through %q while its viewer was away, want it live all along", states)
	}
	if info, _ := h.SFU.Share(share); info.State != sfu.ShareLive {
		t.Errorf("the share = %+v, want live", info)
	}
	if errs := viewSig.Errs(); len(errs) != 0 {
		t.Errorf("answering the sub offers: %v", errs)
	}
}

// TestPubPCRebuild is integration 8 of 02 §17. A publisher replaces its pub PC and offers gen + 1 with the same share
// id in its tracks. The share lives on under its id: stalled from the moment the old PC's tracks end, live again with
// the first keyframe of the new ones. Its viewers get no new sub offer; their tracks go on where they stopped, with a
// keyframe.
func TestPubPCRebuild(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx := testCtx(t)
	const share = sfu.ShareID("s_rebuild")
	pubConn, pubSig := join(t, h, "alice", "c-pub", sfu.RoleFull)
	startShare(t, pubConn, share, sfu.PresetAuto)
	src := newLongGOPSource(t)
	pub := startPublisher(t, ctx, pubConn, src, sfutest.LoopbackSettings(), 1, share)
	waitLive(t, h, share)

	// A viewer on the full layer with audio, and one on the preview layer.
	focusConn, focusSig := join(t, h, "bob", "c-focus", sfu.RoleViewer)
	thumbConn, thumbSig := join(t, h, "carol", "c-thumb", sfu.RoleViewer)
	focus, thumb := newViewer(t, sfutest.LoopbackSettings()), newViewer(t, sfutest.LoopbackSettings())
	focusSig.Attach(focusConn, focus)
	thumbSig.Attach(thumbConn, thumb)
	subscribe(t, focusConn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityHigh, Audio: true})
	subscribe(t, thumbConn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityLow})
	full, audio := streamTrack(t, focus, share, webrtc.RTPCodecTypeVideo), streamTrack(t, focus, share, webrtc.RTPCodecTypeAudio)
	small := streamTrack(t, thumb, share, webrtc.RTPCodecTypeVideo)
	waitFrames(t, full, 0, "f", 30)
	waitFrames(t, small, 0, "q", 15)
	if err := audio.Wait(ctx, func(r *sfutest.Recorder) bool { return r.Stats().Packets >= 50 }); err != nil {
		t.Fatal(err)
	}
	before, _ := h.SFU.Share(share)
	roomEvents := len(h.Events.Events())
	fullMark, smallMark, audioMark := len(full.Packets()), len(small.Packets()), audio.Stats().Packets

	// ---- the publisher replaces its pub PC: gen 2, neg 1, the same share id ----
	pub2 := newPublisher(t, src, sfutest.LoopbackSettings())
	if _, err := sfutest.Publish(ctx, pubConn, pub2, 2, 1, share); err != nil {
		t.Fatalf("gen 2: %v", err)
	}
	// The old PC's tracks ended with that offer: the share is stalled, at once, with nothing attached.
	if info, ok := h.SFU.Share(share); !ok || info.State != sfu.ShareStalled || len(info.Layers) != 0 || info.Audio {
		t.Errorf("the share right after its pub PC was replaced = %+v, %v; want stalled without layers", info, ok)
	}
	if err := pub.Close(); err != nil { // the Source has one consumer at a time
		t.Fatal(err)
	}
	if err := pub2.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pubSig.WaitPCState(ctx, sfu.PCPub, 2, "connected"); err != nil {
		t.Fatal(err)
	}
	after := waitShare(t, h.SFU, share, func(i sfu.ShareInfo) bool {
		return i.State == sfu.ShareLive && len(i.Layers) == 2 && i.Audio
	})
	if after.ID != share || !after.StartedAt.Equal(before.StartedAt) || !after.LiveAt.Equal(before.LiveAt) || after.Conn != "c-pub" {
		t.Errorf("the share after the rebuild = %+v, want the share of before (%+v): same id, same times", after, before)
	}
	// The room heard: stalled, then live again; the share never ended.
	eventually(t, func() bool { return shareStates(h, share, roomEvents) == "stalled live" },
		func() string { return "the share's states since the rebuild are " + shareStates(h, share, roomEvents) })
	for _, ev := range h.Events.Events() {
		if ev.Kind != sfutest.ShareUpdated || ev.Share.ID != share {
			t.Errorf("room event %+v, want only ShareUpdated: a pub PC rebuild doesn't end the share", ev)
		}
	}

	// ---- the viewers: the same tracks, on with a keyframe, without a new offer ----
	for _, v := range []struct {
		name string
		rec  *sfutest.Recorder
		mark int
		rid  string
		sig  *sfutest.DirectSignaler
		view *sfutest.Viewer
		want sfu.SubscriptionStateEvent
	}{
		{"the focused viewer", full, fullMark, "f", focusSig, focus,
			sfu.SubscriptionStateEvent{Share: share, Requested: sfu.QualityHigh, Forwarded: sfu.QualityHigh, Audio: true}},
		{"the thumbnail viewer", small, smallMark, "q", thumbSig, thumb,
			sfu.SubscriptionStateEvent{Share: share, Requested: sfu.QualityLow, Forwarded: sfu.QualityLow}},
	} {
		resumed, old := waitResumed(t, ctx, v.rec, v.mark, v.rid, 15)
		for _, f := range sfutest.Frames(sfutest.WithoutPartialTail(resumed)) {
			if !f.HasMark || f.Mark.RID != v.rid {
				t.Errorf("%s got a frame of layer %q after the rebuild, want %s", v.name, f.Mark.RID, v.rid)
				break
			}
		}
		// One stream through the rebuild: the new PC's packets follow the old one's without a gap in the viewer's
		// sequence numbers, and the timestamps go on.
		if err := sfutest.CheckContinuous(sfutest.WithoutPartialTail(v.rec.Packets())); err != nil {
			t.Errorf("%s's video through the rebuild: %v", v.name, err)
		}
		if n := tracksOf(v.view, share, webrtc.RTPCodecTypeVideo); n != 1 {
			t.Errorf("%s has %d video tracks of the share, want the one it had", v.name, n)
		}
		if n := len(v.sig.Offers()); n != 1 {
			t.Errorf("%s got %d sub offers, want only its first: a pub PC rebuild needs no renegotiation", v.name, n)
		}
		waitSubState(t, v.sig, v.want)
		if errs := v.sig.Errs(); len(errs) != 0 {
			t.Errorf("%s: answering the sub offer: %v", v.name, errs)
		}
		t.Logf("%s: %d packets of the old PC's stream after the mark, then the keyframe of the new one", v.name, old)
	}
	if err := audio.Wait(ctx, func(r *sfutest.Recorder) bool { return r.Stats().Packets >= audioMark+50 }); err != nil {
		t.Errorf("the audio didn't come back: %v", err)
	}
	if err := sfutest.CheckContinuous(audio.Packets()); err != nil {
		t.Errorf("the audio through the rebuild: %v", err)
	}
	// The replaced PC closes silently; the new one took its place before any of that could be reported.
	for _, ev := range pcEvents(pubSig) {
		if ev.State != "connected" || ev.Reason != "" {
			t.Errorf("the publisher got %+v, want only its two PCs connecting", ev)
		}
	}
	if st := pcOf(t, pubConn, sfu.PCPub); !st.Exists || st.Gen != 2 || st.Neg != 1 || st.HandshakeTimer {
		t.Errorf("the pub PC = %+v, want gen 2 with its handshake done", st)
	}
}

// TestSubPCRebuild is integration 9 of 02 §17. ResetPC replaces the sub PC: the new one, gen + 1, carries every
// subscription in its first offer, and the media comes back on it. The same happens from the closed state, whoever
// asks: a PC that the client closed is rebuilt by ResetPC, by RestartICE and by the next subscription change.
func TestSubPCRebuild(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx := ctxFor(t, 60*time.Second)
	const shareA, shareB = sfu.ShareID("s_a"), sfu.ShareID("s_b")
	publishShare(t, h, "alice", "c-pub-a", shareA, 1)
	publishShare(t, h, "carol", "c-pub-b", shareB, 2)
	waitLive(t, h, shareA)
	waitLive(t, h, shareB)
	viewConn, viewSig := join(t, h, "bob", "c-view", sfu.RoleViewer)
	viewer := newViewer(t, sfutest.LoopbackSettings())
	viewSig.Attach(viewConn, viewer)
	video, audio := webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio

	// Before any subscription there is no sub PC to reset.
	wantCode(t, viewConn.ResetPC(ctx, sfu.PCSub, 1), sfu.CodeBadPC)
	subscribe(t, viewConn,
		sfu.SubscriptionUpdate{Share: shareA, Video: sfu.QualityHigh, Audio: true},
		sfu.SubscriptionUpdate{Share: shareB, Video: sfu.QualityLow})
	tracks := map[sfu.ShareID][]*sfutest.Recorder{
		shareA: {streamTrack(t, viewer, shareA, video)}, shareB: {streamTrack(t, viewer, shareB, video)},
	}
	sound := []*sfutest.Recorder{streamTrack(t, viewer, shareA, audio)}
	waitFrames(t, tracks[shareA][0], 0, "f", 15)
	waitFrames(t, tracks[shareB][0], 0, "q", 8)
	wantCode(t, viewConn.ResetPC(ctx, sfu.PCPub, 1), sfu.CodeBadPC)
	wantCode(t, viewConn.ResetPC(ctx, sfu.PCSub, 2), sfu.CodeBadPC)
	wantCode(t, viewConn.ResetPC(ctx, 0, 1), sfu.CodeBadPC)

	// rebuilt checks the sub PC of a new gen: its first offer binds both subscriptions, the viewer answers it on a
	// new PeerConnection, and the media of both shares is back, each stream starting on a keyframe.
	wantBindings := []sfu.TrackBinding{
		{MID: "0", Share: shareA, Kind: video}, {MID: "1", Share: shareA, Kind: audio},
		{MID: "2", Share: shareB, Kind: video}, {MID: "3", Share: shareB, Kind: audio},
	}
	rebuilt := func(what string, gen uint32, clientPC *webrtc.PeerConnection) {
		t.Helper()
		offer, err := viewSig.WaitOffer(ctx, func(o sfutest.Offer) bool { return o.Gen == gen })
		if err != nil {
			t.Fatalf("%s: no offer of gen %d: %v", what, gen, err)
		}
		if offer.PC != sfu.PCSub || offer.Neg != 1 || !slices.Equal(offer.Tracks, wantBindings) {
			t.Errorf("%s: the new PC's offer is neg %d and binds %+v, want neg 1 with every subscription: %+v", what,
				offer.Neg, offer.Tracks, wantBindings)
		}
		for i, line := range sectionsOf(t, offer.SDP) {
			if !strings.HasSuffix(line, "sendonly "+string(wantBindings[i].Share)) {
				t.Errorf("%s: m-section %q of the new PC's offer, want it sendonly for %s", what, line, wantBindings[i].Share)
			}
		}
		checkServerCandidates(t, h, what+": the new PC's offer", offer.SDP)
		if err := viewSig.WaitPCState(ctx, sfu.PCSub, gen, "connected"); err != nil {
			t.Fatalf("%s: %v (answer errors %v)", what, err, viewSig.Errs())
		}
		if viewer.PC() == clientPC {
			t.Errorf("%s: the viewer answered gen %d on the PeerConnection it had", what, gen)
		}
		for share, rid := range map[sfu.ShareID]string{shareA: "f", shareB: "q"} {
			next := nextTrack(t, ctx, viewer, share, video, tracks[share]...)
			tracks[share] = append(tracks[share], next)
			waitFrames(t, next, 0, rid, 15)
			pkts := checkVideo(t, fmt.Sprintf("%s: video of %s", what, share), next.Packets())
			if runs := layerRuns(pkts); runs != rid {
				t.Errorf("%s: the viewer got the layers %q of %s, want %s as before", what, runs, share, rid)
			}
		}
		next := nextTrack(t, ctx, viewer, shareA, audio, sound...)
		sound = append(sound, next)
		if err := next.Wait(ctx, func(r *sfutest.Recorder) bool { return r.Stats().Packets >= 25 }); err != nil {
			t.Errorf("%s: the audio didn't come back: %v", what, err)
		}
		if tracksOf(viewer, shareB, audio) != 0 {
			t.Errorf("%s: the viewer got audio of the share it didn't ask audio for", what)
		}
		waitSubState(t, viewSig, sfu.SubscriptionStateEvent{Share: shareA, Requested: sfu.QualityHigh, Forwarded: sfu.QualityHigh, Audio: true})
		waitSubState(t, viewSig, sfu.SubscriptionStateEvent{Share: shareB, Requested: sfu.QualityLow, Forwarded: sfu.QualityLow})
		if st := pcOf(t, viewConn, sfu.PCSub); st.Gen != gen || st.Closed || st.Neg != 1 || st.Offering {
			t.Errorf("%s: the sub PC = %+v, want gen %d with its one offer answered", what, st, gen)
		}
	}
	// closedByClient has the viewer close its PeerConnection, and waits until the SFU has it as closed.
	closedByClient := func(gen uint32) {
		t.Helper()
		eventually(t, func() bool { return viewer.PC().ConnectionState() == webrtc.PeerConnectionStateConnected },
			func() string { return "the viewer's PC is " + viewer.PC().ConnectionState().String() })
		if err := viewer.PC().Close(); err != nil {
			t.Fatal(err)
		}
		if err := viewSig.WaitPCState(ctx, sfu.PCSub, gen, "closed"); err != nil {
			t.Fatalf("%v (events %+v)", err, pcEvents(viewSig))
		}
		if st := pcOf(t, viewConn, sfu.PCSub); st.Gen != gen || !st.Closed {
			t.Errorf("the sub PC the client closed = %+v, want gen %d, closed", st, gen)
		}
	}

	// ---- ResetPC on a connected PC ----
	clientPC := viewer.PC()
	if err := viewConn.ResetPC(ctx, sfu.PCSub, 1); err != nil {
		t.Fatalf("ResetPC: %v", err)
	}
	rebuilt("ResetPC", 2, clientPC)
	// The request again, as a client that hasn't seen the new offer yet may send it: gen 1 is over.
	if err := viewConn.ResetPC(ctx, sfu.PCSub, 1); err != nil {
		t.Errorf("ResetPC for the replaced gen = %v, want it ignored", err)
	}
	if err := viewConn.RestartICE(ctx, sfu.PCSub, 1); err != nil {
		t.Errorf("RestartICE for the replaced gen = %v, want it ignored", err)
	}
	if err := viewConn.ClosePC(ctx, sfu.PCSub, 1); err != nil {
		t.Errorf("ClosePC for the replaced gen = %v, want it ignored", err)
	}

	// ---- from closed: ResetPC ----
	closedByClient(2)
	wantCode(t, viewConn.HandleAnswer(ctx, sfu.PCSub, 2, 1, "v=0"), sfu.CodeBadPC)
	clientPC = viewer.PC()
	if err := viewConn.ResetPC(ctx, sfu.PCSub, 2); err != nil {
		t.Fatalf("ResetPC of a closed PC: %v", err)
	}
	rebuilt("ResetPC from closed", 3, clientPC)

	// ---- from closed: RestartICE, which has nothing to restart ----
	closedByClient(3)
	clientPC = viewer.PC()
	if err := viewConn.RestartICE(ctx, sfu.PCSub, 3); err != nil {
		t.Fatalf("RestartICE of a closed PC: %v", err)
	}
	rebuilt("RestartICE from closed", 4, clientPC)

	// ---- from closed: a subscription change. Here the SFU closed the PC, for the client (ClosePC) ----
	clientPC = viewer.PC()
	if err := viewConn.ClosePC(ctx, sfu.PCSub, 4); err != nil {
		t.Fatalf("ClosePC: %v", err)
	}
	if st := pcOf(t, viewConn, sfu.PCSub); st.Gen != 4 || !st.Closed {
		t.Errorf("the sub PC after ClosePC = %+v, want gen 4, closed", st)
	}
	if err := viewConn.ClosePC(ctx, sfu.PCSub, 4); err != nil {
		t.Errorf("ClosePC of a closed PC = %v, want nothing", err)
	}
	wantCode(t, viewConn.ClosePC(ctx, sfu.PCSub, 5), sfu.CodeBadPC)
	wantCode(t, viewConn.ClosePC(ctx, 0, 4), sfu.CodeBadPC)
	subscribe(t, viewConn, sfu.SubscriptionUpdate{Share: shareB, Video: sfu.QualityLow}) // as it was: still a request
	rebuilt("a subscription change on a closed PC", 5, clientPC)

	// ---- from closed: the resume ----
	closedByClient(5)
	clientPC = viewer.PC()
	viewConn.Resync()
	rebuilt("Resync with a closed PC", 6, clientPC)

	// Only the PCs that the client closed were reported closed: a PC that the SFU replaced, or closed because signal
	// said so, says nothing more.
	var got []string
	for _, ev := range pcEvents(viewSig) {
		if ev.PC != sfu.PCSub || ev.Reason != "" {
			t.Errorf("the viewer got %+v", ev)
		}
		got = append(got, fmt.Sprintf("%d:%s", ev.Gen, ev.State))
	}
	want := []string{"1:connected", "2:connected", "2:closed", "3:connected", "3:closed", "4:connected", "5:connected",
		"5:closed", "6:connected"}
	if !slices.Equal(got, want) {
		t.Errorf("the sub PCs' states = %v, want %v", got, want)
	}
	if errs := viewSig.Errs(); len(errs) != 0 {
		t.Errorf("answering the sub offers: %v", errs)
	}
	for _, ev := range viewSig.Events() {
		if e, ok := ev.(sfu.ErrorEvent); ok {
			t.Errorf("the viewer got %+v", e)
		}
	}
	subs, err := viewConn.Subscriptions(ctx)
	if err != nil || len(subs) != 2 || subs[0].Video != sfu.QualityHigh || !subs[0].Audio || subs[1].Video != sfu.QualityLow {
		t.Errorf("the subscriptions after five rebuilds = %+v, %v; want them as the viewer made them", subs, err)
	}
	if v, a, ok := h.SFU.FanOut(shareA); !ok || v != 1 || a != 1 {
		t.Errorf("FanOut(%s) = %d, %d, %v; want the viewer's two DownTracks, as before", shareA, v, a, ok)
	}
}

// TestSubPCResetWithoutSubscriptions: a client that asks for a new sub PC while it is subscribed to nothing gets
// none: the old one is closed, and the next subscription builds the successor.
func TestSubPCResetWithoutSubscriptions(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx := testCtx(t)
	const first, second = sfu.ShareID("s_first"), sfu.ShareID("s_second")
	pubConn, _ := publishShare(t, h, "alice", "c-pub", first, 9)
	waitLive(t, h, first)
	viewConn, viewSig := join(t, h, "bob", "c-view", sfu.RoleViewer)
	viewer := newViewer(t, sfutest.LoopbackSettings())
	viewSig.Attach(viewConn, viewer)
	subscribe(t, viewConn, sfu.SubscriptionUpdate{Share: first, Video: sfu.QualityHigh, Audio: true})
	waitFrames(t, streamTrack(t, viewer, first, webrtc.RTPCodecTypeVideo), 0, "f", 8)

	// The share ends: the sub PC stays, with two inactive m-sections and no subscription.
	if err := pubConn.StopShare(ctx, first, sfu.EndReasonStopped); err != nil {
		t.Fatal(err)
	}
	down, err := viewSig.WaitOffer(ctx, func(o sfutest.Offer) bool { return o.Neg == 2 })
	if err != nil {
		t.Fatal(err)
	}
	if err := viewSig.WaitAnswered(ctx, down); err != nil {
		t.Fatal(err)
	}
	if err := viewConn.ResetPC(ctx, sfu.PCSub, 1); err != nil {
		t.Fatalf("ResetPC without subscriptions: %v", err)
	}
	if st := pcOf(t, viewConn, sfu.PCSub); st.Gen != 1 || !st.Closed {
		t.Errorf("the sub PC = %+v, want gen 1, closed, and no successor yet", st)
	}
	// The other requests that would rebuild a closed PC have nothing to build it for either.
	if err := viewConn.RestartICE(ctx, sfu.PCSub, 1); err != nil {
		t.Errorf("RestartICE without subscriptions: %v", err)
	}
	viewConn.Resync()
	time.Sleep(4 * 50 * time.Millisecond)
	if n := len(viewSig.Offers()); n != 2 {
		t.Fatalf("%d sub offers, want the two of before: a Conn without subscriptions gets no sub PC", n)
	}

	// The next subscription: a new PC, gen 2, with only what it needs.
	startShare(t, pubConn, second, sfu.PresetAuto)
	subscribe(t, viewConn, sfu.SubscriptionUpdate{Share: second, Video: sfu.QualityLow})
	offer, err := viewSig.WaitOffer(ctx, func(o sfutest.Offer) bool { return o.Gen == 2 })
	if err != nil {
		t.Fatal(err)
	}
	wantSections := []string{"0 video sendonly " + string(second), "1 audio sendonly " + string(second)}
	if got := sectionsOf(t, offer.SDP); offer.Neg != 1 || !slices.Equal(got, wantSections) {
		t.Errorf("the new PC's offer: neg %d, m-sections %v; want neg 1 and %v", offer.Neg, got, wantSections)
	}
	if err := viewSig.WaitPCState(ctx, sfu.PCSub, 2, "connected"); err != nil {
		t.Fatalf("%v (answer errors %v)", err, viewSig.Errs())
	}
	for _, ev := range pcEvents(viewSig) {
		if ev.State != "connected" {
			t.Errorf("the viewer got %+v, want nothing about the PC that was closed for it", ev)
		}
	}
}

// TestResync is integration 11 of 02 §17. A sub offer is lost on the way (the viewer's WebSocket is down). After the
// resume, Resync sends the same offer again, with the same gen and neg, and the viewer's answer is applied. Resync
// also says again what the client may have missed: what each subscription gets, and for a publisher the codec to
// offer first.
func TestResync(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx := testCtx(t)
	const share = sfu.ShareID("s_resync")
	pubConn, pubSig := join(t, h, "alice", "c-pub", sfu.RoleFull)
	startShare(t, pubConn, share, sfu.PresetAuto)
	startPublisher(t, ctx, pubConn, newLongGOPSource(t), sfutest.LoopbackSettings(), 1, share)
	waitLive(t, h, share)
	viewConn, viewSig := join(t, h, "bob", "c-view", sfu.RoleViewer)
	viewer := newViewer(t, sfutest.LoopbackSettings())
	viewSig.Attach(viewConn, viewer)

	// ---- the offer is lost ----
	viewSig.SetDown(true)
	subscribe(t, viewConn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityHigh, Audio: true})
	eventually(t, func() bool { return len(viewSig.Lost()) == 1 }, func() string { return "no sub offer was sent" })
	lost := viewSig.Lost()[0]
	time.Sleep(4 * 50 * time.Millisecond)
	if st := pcOf(t, viewConn, sfu.PCSub); !st.Offering || st.Neg != 1 || len(viewSig.Offers()) != 0 || len(viewSig.Lost()) != 1 {
		t.Fatalf("the sub PC = %+v with %d offers arrived and %d lost; want one offer outstanding, lost", st,
			len(viewSig.Offers()), len(viewSig.Lost()))
	}

	// ---- the resume ----
	viewSig.SetDown(false)
	viewConn.Resync()
	again, err := viewSig.WaitOffer(ctx, func(sfutest.Offer) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if again.PC != sfu.PCSub || again.Gen != lost.Gen || again.Neg != lost.Neg || again.SDP != lost.SDP || !slices.Equal(again.Tracks, lost.Tracks) {
		t.Errorf("the offer after Resync is gen %d, neg %d (the same SDP: %v), want the lost offer again: gen %d, neg %d",
			again.Gen, again.Neg, again.SDP == lost.SDP, lost.Gen, lost.Neg)
	}
	if err := viewSig.WaitAnswered(ctx, again); err != nil {
		t.Fatal(err)
	}
	if err := viewSig.WaitPCState(ctx, sfu.PCSub, 1, "connected"); err != nil {
		t.Fatalf("%v (answer errors %v)", err, viewSig.Errs())
	}
	video := streamTrack(t, viewer, share, webrtc.RTPCodecTypeVideo)
	waitFrames(t, video, 0, "f", 15)
	checkVideo(t, "video", video.Packets())
	// What the subscription got at the time of the resume: nothing yet, and no reason for it but time. A client that
	// had been told something else while it was away knows better now.
	waiting := sfu.SubscriptionStateEvent{Share: share, Requested: sfu.QualityHigh}
	if evs := subEvents(viewSig, share); len(evs) == 0 || evs[0] != waiting {
		t.Errorf("the viewer's events about the share after the resume are %+v, want %+v first", evs, waiting)
	}
	waitSubState(t, viewSig, sfu.SubscriptionStateEvent{Share: share, Requested: sfu.QualityHigh, Forwarded: sfu.QualityHigh, Audio: true})

	// ---- a resume with nothing outstanding: the states again, no offer, no ICE restart of a connected PC ----
	states := len(subEvents(viewSig, share))
	viewConn.Resync()
	eventually(t, func() bool { return len(subEvents(viewSig, share)) > states },
		func() string { return "Resync didn't send the subscription's state again" })
	evs := subEvents(viewSig, share)
	if last := evs[len(evs)-1]; len(evs) != states+1 || last != evs[states-1] {
		t.Errorf("Resync sent %+v, want the state the client had last: %+v", evs[states:], evs[states-1])
	}
	time.Sleep(4 * 50 * time.Millisecond)
	if n := len(viewSig.Offers()); n != 1 {
		t.Errorf("%d sub offers after a resume with nothing outstanding, want only the first", n)
	}
	if len(viewSig.Lost()) != 1 {
		t.Errorf("%d offers lost, want only the one", len(viewSig.Lost()))
	}

	// ---- the publisher's resume: the codec to offer first, for each share it publishes ----
	const other = sfu.ShareID("s_other")
	startShare(t, pubConn, other, sfu.PresetAuto)
	pubConn.Resync()
	for _, id := range []sfu.ShareID{share, other} {
		want := sfu.Event(sfu.CodecPolicyEvent{Share: id, Profile: sfu.ProfileHigh})
		if _, err := pubSig.WaitEvent(ctx, func(ev sfu.Event) bool { return ev == want }); err != nil {
			t.Errorf("%v: the publisher's events after Resync are %+v, want %+v among them", err, pubSig.Events(), want)
		}
	}
	if n := len(pubSig.Offers()); n != 0 {
		t.Errorf("the publisher got %d sub offers", n)
	}
	for _, ev := range viewSig.Events() {
		if _, ok := ev.(sfu.CodecPolicyEvent); ok {
			t.Errorf("the viewer got %+v: it publishes nothing", ev)
		}
	}
	if errs := viewSig.Errs(); len(errs) != 0 {
		t.Errorf("answering the sub offer: %v", errs)
	}
}

// TestHandshakeTimeout is integration 14 of 02 §17, in real time. A client that never completes ICE: its new
// PeerConnection is closed 10 s (± 1 s) after its first answer and reported failed with the handshake_timeout
// reason, pub and sub alike. An ICE restart that never completes gets neither a timer nor an event: the client
// rebuilds after 15 s (01 §10.4).
func TestHandshakeTimeout(t *testing.T) {
	if d := sfu.DefaultTimings(); d.Handshake != 10*time.Second || d.Grace != 30*time.Second || d.OfferResend != 15*time.Second ||
		d.ICERestart != 5*time.Second || d.PCWindow != time.Minute || d.Stalled != 2*time.Second || sfu.MaxPCCreations != 10 {
		t.Errorf("the SFU's timings are %+v with %d PCs a window, want the numbers of 02 §5.3 and §12", d, sfu.MaxPCCreations)
	}
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx := ctxFor(t, 90*time.Second)
	video := webrtc.RTPCodecTypeVideo

	// A live share with a connected viewer: the one whose ICE restart will never complete.
	const live, never = sfu.ShareID("s_live"), sfu.ShareID("s_never")
	publishShare(t, h, "alice", "c-pub", live, 14)
	waitLive(t, h, live)
	okConn, okSig := join(t, h, "bob", "c-ok", sfu.RoleViewer)
	okSettings, okWire := faultySettings(t)
	okViewer := newViewer(t, okSettings)
	okSig.Attach(okConn, okViewer)
	subscribe(t, okConn, sfu.SubscriptionUpdate{Share: live, Video: sfu.QualityHigh, Audio: true})
	okVideo := streamTrack(t, okViewer, live, video)
	waitFrames(t, okVideo, 0, "f", 15)

	// ---- a publisher that never completes ICE: its UDP goes nowhere ----
	deafPubConn, deafPubSig := join(t, h, "carol", "c-deaf-pub", sfu.RoleFull)
	startShare(t, deafPubConn, never, sfu.PresetAuto)
	pubSettings, pubWire := faultySettings(t)
	blackHole(pubWire, true)
	src := newSource(t)
	deafPub := newPublisher(t, src, pubSettings)
	if _, err := sfutest.Publish(ctx, deafPubConn, deafPub, 1, 1, never); err != nil {
		t.Fatal(err)
	}
	pubAnswered := time.Now()
	if st := pcOf(t, deafPubConn, sfu.PCPub); !st.Exists || !st.HandshakeTimer {
		t.Errorf("the new pub PC = %+v, want its handshake timer running", st)
	}

	// ---- a viewer that never completes ICE ----
	deafViewConn, deafViewSig := join(t, h, "dave", "c-deaf-view", sfu.RoleViewer)
	viewSettings, viewWire := faultySettings(t)
	blackHole(viewWire, true)
	deafViewer := newViewer(t, viewSettings)
	deafViewSig.Attach(deafViewConn, deafViewer)
	subscribe(t, deafViewConn, sfu.SubscriptionUpdate{Share: live, Video: sfu.QualityLow})
	offer, err := deafViewSig.WaitOffer(ctx, func(sfutest.Offer) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if st := pcOf(t, deafViewConn, sfu.PCSub); st.HandshakeTimer && st.Offering {
		t.Errorf("the new sub PC = %+v: its handshake timer runs before the client has answered", st)
	}
	if err := deafViewSig.WaitAnswered(ctx, offer); err != nil {
		t.Fatal(err)
	}
	subAnswered := time.Now()
	if st := pcOf(t, deafViewConn, sfu.PCSub); !st.HandshakeTimer {
		t.Errorf("the answered sub PC = %+v, want its handshake timer running", st)
	}

	// ---- an ICE restart that never completes ----
	blackHole(okWire, true)
	if err := okConn.RestartICE(ctx, sfu.PCSub, 1); err != nil {
		t.Fatal(err)
	}
	restart, err := okSig.WaitOffer(ctx, func(o sfutest.Offer) bool { return o.Neg == 2 })
	if err != nil {
		t.Fatal(err)
	}
	if err := okSig.WaitAnswered(ctx, restart); err != nil {
		t.Fatal(err)
	}
	restarted := time.Now()
	okStates := len(pcEvents(okSig))

	// ---- 10 s later ----
	within := func(what string, since, at time.Time) {
		t.Helper()
		if d := at.Sub(since); d < 9*time.Second || d > 11*time.Second {
			t.Errorf("%s was closed %v after its first answer, want 10 s ± 1 s", what, d.Round(time.Millisecond))
		}
	}
	pubEv, pubAt := waitPCEvent(t, ctx, deafPubSig, 0)
	if want := (sfu.PCStateEvent{PC: sfu.PCPub, Gen: 1, State: "failed", Reason: sfu.PCReasonHandshakeTimeout}); pubEv != want {
		t.Errorf("the publisher that never connected got %+v, want %+v", pubEv, want)
	}
	within("the pub PC", pubAnswered, pubAt)
	subEv, subAt := waitPCEvent(t, ctx, deafViewSig, 0)
	if want := (sfu.PCStateEvent{PC: sfu.PCSub, Gen: 1, State: "failed", Reason: sfu.PCReasonHandshakeTimeout}); subEv != want {
		t.Errorf("the viewer that never connected got %+v, want %+v", subEv, want)
	}
	within("the sub PC", subAnswered, subAt)

	// The pub PC is gone and its gen is over; the share is as it was, pending, for the hub's own timeout to end.
	if st := pcOf(t, deafPubConn, sfu.PCPub); st.Exists || st.Gen != 1 {
		t.Errorf("the pub PC after its handshake timed out = %+v, want none, with gen 1 over", st)
	}
	reoffer, err := deafPub.Offer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = deafPubConn.HandleOffer(ctx, sfu.PCPub, 1, 2, reoffer.SDP, sfutest.Bindings(deafPub, never))
	wantCode(t, err, sfu.CodeBadPC)
	if info, ok := h.SFU.Share(never); !ok || info.State != sfu.SharePending {
		t.Errorf("the share of the publisher that never connected = %+v, %v; want it pending", info, ok)
	}
	// The sub PC is closed; the subscription waits for the PC that the client's rebuild request brings.
	if st := pcOf(t, deafViewConn, sfu.PCSub); !st.Exists || !st.Closed || st.Gen != 1 || st.HandshakeTimer {
		t.Errorf("the sub PC after its handshake timed out = %+v, want gen 1, closed", st)
	}
	if subs, err := deafViewConn.Subscriptions(ctx); err != nil || len(subs) != 1 {
		t.Errorf("the subscriptions of the viewer that never connected = %+v, %v; want the one kept", subs, err)
	}
	if m := h.SFU.Metrics().HandshakeTimeouts; m["pub"] != 1 || m["sub"] != 1 {
		t.Errorf("handshake timeouts counted: %v, want one pub and one sub", m)
	}

	// ---- the ICE restart: 11.5 s after it began, no timer has closed the PC and nothing was reported ----
	time.Sleep(time.Until(restarted.Add(11500 * time.Millisecond)))
	if evs := pcEvents(okSig); len(evs) != okStates {
		t.Errorf("the viewer whose ICE restart doesn't complete got %+v, want nothing: the client rebuilds by itself", evs[okStates:])
	}
	if st := pcOf(t, okConn, sfu.PCSub); st.Closed || st.Gen != 1 || st.State != "connecting" || st.HandshakeTimer || st.GraceTimer {
		t.Errorf("the sub PC whose ICE restart doesn't complete = %+v, want gen 1, connecting, without a timer", st)
	}
	// The PC is still good for another restart, now that its UDP is back.
	mark := len(okVideo.Packets())
	blackHole(okWire, false)
	if err := okConn.RestartICE(ctx, sfu.PCSub, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := okSig.WaitOffer(ctx, func(o sfutest.Offer) bool { return o.Neg == 3 }); err != nil {
		t.Fatalf("the second ICE restart, more than 5 s after the first one's offer: %v", err)
	}
	waitResumed(t, ctx, okVideo, mark, "f", 8)

	// ---- what the clients do next: the publisher offers gen 2, the viewer asks for a rebuild ----
	blackHole(pubWire, false)
	if err := deafPub.Close(); err != nil {
		t.Fatal(err)
	}
	startPublisher(t, ctx, deafPubConn, src, pubSettings, 2, never)
	if err := deafPubSig.WaitPCState(ctx, sfu.PCPub, 2, "connected"); err != nil {
		t.Fatal(err)
	}
	waitShare(t, h.SFU, never, func(i sfu.ShareInfo) bool { return i.State == sfu.ShareLive })
	blackHole(viewWire, false)
	if err := deafViewConn.ResetPC(ctx, sfu.PCSub, 1); err != nil {
		t.Fatal(err)
	}
	if err := deafViewSig.WaitPCState(ctx, sfu.PCSub, 2, "connected"); err != nil {
		t.Fatalf("%v (answer errors %v)", err, deafViewSig.Errs())
	}
	waitFrames(t, streamTrack(t, deafViewer, live, video), 0, "q", 8)
	for name, sig := range map[string]*sfutest.DirectSignaler{"publisher": deafPubSig, "viewer": deafViewSig} {
		if evs := pcEvents(sig); len(evs) != 2 || evs[1].Gen != 2 || evs[1].State != "connected" {
			t.Errorf("the %s that never connected got %+v, want the timeout and then its second PC connected", name, evs)
		}
	}
	if m := h.SFU.Metrics().HandshakeTimeouts; m["pub"] != 1 || m["sub"] != 1 {
		t.Errorf("handshake timeouts counted: %v, want one pub and one sub still", m)
	}
}

// TestGuards is integration 15 of 02 §17: what the guards of 02 §12 refuse, each with its code of 02 §6.3 and with
// nothing applied. A third PC, a fifth share, a rid the SFU doesn't take, a malformed tracks binding, a video
// m-section without H.264, a data channel, a stale pub offer, and one PeerConnection too many within a minute. And
// what is no error: a share stopped while its re-offer was in flight.
func TestGuards(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx := testCtx(t)
	conn, sig := join(t, h, "alice", "c-pub", sfu.RoleFull)
	video, audio := webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio

	// ---- a fifth share ----
	const gone, kept = sfu.ShareID("s_1"), sfu.ShareID("s_2")
	for _, id := range []sfu.ShareID{gone, kept, "s_3", "s_4"} {
		startShare(t, conn, id, sfu.PresetAuto)
	}
	_, err := conn.StartShare(ctx, sfu.StartShareParams{ID: "s_5", Preset: sfu.PresetAuto})
	wantCode(t, err, sfu.CodeTooManyShares)
	if n := len(h.SFU.Shares("lounge")); n != 4 {
		t.Fatalf("%d shares after a refused fifth, want 4", n)
	}

	// The publisher: one share's video and audio, and another share's video, on one pub PC.
	pub := newRawPublisher(t, video, video, audio)
	bind := func() []sfu.TrackBinding {
		return []sfu.TrackBinding{
			{MID: pub.mids[0], Share: gone, Kind: video}, {MID: pub.mids[1], Share: kept, Kind: video},
			{MID: pub.mids[2], Share: gone, Kind: audio},
		}
	}
	answer, err := conn.HandleOffer(ctx, sfu.PCPub, 1, 1, pub.offer(t, ctx), bind())
	if err != nil {
		t.Fatal(err)
	}
	pub.apply(t, answer)
	pub.send(t)
	if err := sig.WaitPCState(ctx, sfu.PCPub, 1, "connected"); err != nil {
		t.Fatal(err)
	}
	wantTracks := func(shares ...sfu.ShareID) []sfu.PubTrackState {
		t.Helper()
		var tracks []sfu.PubTrackState
		eventually(t, func() bool {
			var err error
			tracks, err = conn.PubTracks(ctx)
			if err != nil || len(tracks) != len(shares) {
				return false
			}
			for i, tr := range tracks {
				if tr.MID != pub.mids[i] || tr.Share != shares[i] || tr.Packets == 0 {
					return false
				}
			}
			return true
		}, func() string {
			return fmt.Sprintf("pub tracks = %+v, want them on mids %v feeding %q", tracks, pub.mids, shares)
		})
		return tracks
	}
	wantTracks(gone, kept, gone)
	if answer, err = conn.HandleOffer(ctx, sfu.PCPub, 1, 2, pub.offer(t, ctx), bind()); err != nil {
		t.Fatalf("the re-offer: %v", err)
	}
	pub.apply(t, answer)

	// snapshot is everything a refused offer must leave alone: the pub PC with its gen and neg, its tracks and what
	// they feed, the shares, and what the room and the client were told.
	snapshot := func() string {
		t.Helper()
		var b strings.Builder
		st := pcOf(t, conn, sfu.PCPub)
		fmt.Fprintf(&b, "pc %v gen %d neg %d %s;", st.Exists, st.Gen, st.Neg, st.State)
		tracks, err := conn.PubTracks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, tr := range tracks {
			fmt.Fprintf(&b, " track %s/%s %s→%s;", tr.MID, tr.RID, tr.Kind, tr.Share)
		}
		for _, info := range h.SFU.Shares("lounge") {
			fmt.Fprintf(&b, " share %s %s %v audio %v;", info.ID, info.State, layerRIDs(info), info.Audio)
		}
		fmt.Fprintf(&b, " %d room events, %d client events, %d offers", len(h.Events.Events()), len(sig.Events()), len(sig.Offers()))
		return b.String()
	}
	before := snapshot()
	// refused hands the SFU an offer that it must refuse with code, naming share in the error (02 §6.3: the share of
	// the failing m-section, for the codes that have one), and leave everything as it was.
	refused := func(what, code string, share sfu.ShareID, kind sfu.PCKind, gen, neg uint32, raw string, tracks []sfu.TrackBinding) {
		t.Helper()
		_, err := conn.HandleOffer(ctx, kind, gen, neg, raw, tracks)
		wantCode(t, err, code)
		var e *sfu.Error
		if errors.As(err, &e) && e.Share != share {
			t.Errorf("%s: the error names the share %q, want %q", what, e.Share, share)
		}
		if now := snapshot(); now != before {
			t.Errorf("%s changed something:\n before: %s\n after:  %s", what, before, now)
		}
	}
	good := pub.offer(t, ctx) // what neg 3 would be

	// ---- a third PC: a Conn has one pub and one sub PC ----
	refused("an offer for a third PC", sfu.CodePCLimit, "", sfu.PCKind(3), 1, 3, good, bind())
	refused("an offer for a fourth PC", sfu.CodePCLimit, "", sfu.PCKind(200), 1, 1, good, bind())
	// The sub PC is the SFU's to offer on, and the zero value is an adapter's bug: neither is a PC too many.
	refused("an offer for the sub PC", sfu.CodeBadPC, "", sfu.PCSub, 1, 3, good, bind())
	refused("an offer without a PC kind", sfu.CodeBadPC, "", 0, 1, 3, good, bind())

	// ---- bad rids: h is the M5 middle layer, and two rids are the most ----
	simulcast, err := newPublisher(t, newSource(t), sfutest.LoopbackSettings()).Offer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(simulcast.SDP, "a=rid:q send") || !strings.Contains(simulcast.SDP, "a=simulcast:send f;q") {
		t.Fatalf("the simulcast offer has no rids f and q:\n%s", simulcast.SDP)
	}
	var simVideo, simAudio string
	for _, md := range parse(t, simulcast.SDP).MediaDescriptions {
		mid, _ := md.Attribute(sdp.AttrKeyMID)
		if md.MediaName.Media == "video" {
			simVideo = mid
		} else {
			simAudio = mid
		}
	}
	simBind := []sfu.TrackBinding{{MID: simVideo, Share: "s_3", Kind: video}, {MID: simAudio, Share: "s_3", Kind: audio}}
	withH := strings.NewReplacer("a=rid:q send", "a=rid:h send", "a=simulcast:send f;q", "a=simulcast:send f;h").Replace(simulcast.SDP)
	refused("rid h", sfu.CodeBadRID, "s_3", sfu.PCPub, 1, 3, withH, simBind)
	three := strings.NewReplacer("a=rid:q send", "a=rid:q send\r\na=rid:h send", "a=simulcast:send f;q", "a=simulcast:send f;q;h").Replace(simulcast.SDP)
	refused("three rids", sfu.CodeBadRID, "s_3", sfu.PCPub, 1, 3, three, simBind)
	refused("a new gen with three rids", sfu.CodeBadRID, "s_3", sfu.PCPub, 2, 1, three, simBind)

	// ---- a malformed track binding: a video m-section bound as audio ----
	swapped := bind()
	swapped[0].Kind = audio
	refused("a video m-section bound as audio", sfu.CodeUnknownTrack, gone, sfu.PCPub, 1, 3, good, swapped)
	twice := append(bind(), sfu.TrackBinding{MID: pub.mids[1], Share: kept, Kind: video})
	refused("a mid bound twice", sfu.CodeUnknownTrack, kept, sfu.PCPub, 1, 3, good, twice)
	second := bind()
	second[1].Share = gone // the share's second video m-section
	refused("two video m-sections for one share", sfu.CodeUnknownTrack, gone, sfu.PCPub, 1, 3, good, second)

	// ---- no H.264 the SFU can forward ----
	noH264 := strings.ReplaceAll(good, "packetization-mode=1", "packetization-mode=0")
	refused("no H.264", sfu.CodeNoH264, gone, sfu.PCPub, 1, 3, noH264, bind())

	// ---- a data channel ----
	refused("a data channel", sfu.CodeBadSDP, "", sfu.PCPub, 1, 3, strings.Replace(good, "m=video", "m=application", 1), bind())
	refused("garbage", sfu.CodeBadSDP, "", sfu.PCPub, 1, 3, "v=nonsense", bind())

	// ---- stale pub offers ----
	refused("an older neg", sfu.CodeStaleOffer, "", sfu.PCPub, 1, 1, good, bind())
	refused("neg 0", sfu.CodeStaleOffer, "", sfu.PCPub, 1, 0, good, bind())
	refused("gen 0", sfu.CodeStaleOffer, "", sfu.PCPub, 0, 1, good, bind())

	// ---- no error: a share stopped while its re-offer was in flight ----
	// The offer still binds the share. The hub ends the share first and then hands the offer over.
	if err := conn.StopShare(ctx, gone, sfu.EndReasonStopped); err != nil {
		t.Fatal(err)
	}
	answer, err = conn.HandleOffer(ctx, sfu.PCPub, 1, 3, good, bind())
	if err != nil {
		t.Fatalf("the re-offer that still binds a stopped share: %v, want an answer", err)
	}
	ans := parse(t, answer)
	for i, want := range []string{sdp.AttrKeyInactive, sdp.AttrKeyRecvOnly, sdp.AttrKeyInactive} {
		if dir := directionOf(section(t, ans, pub.mids[i])); dir != want {
			t.Errorf("answer m-section %s is %s, want %s", pub.mids[i], dir, want)
		}
	}
	pub.apply(t, answer)
	// The publisher's other share keeps flowing: its track feeds it as before, and is read.
	flowing := wantTracks("", kept, "")
	eventually(t, func() bool {
		now, err := conn.PubTracks(ctx)
		return err == nil && len(now) == 3 && now[1].Share == kept && now[1].Packets > flowing[1].Packets+10
	}, func() string { return "the other share's track is no longer read" })
	if info, ok := h.SFU.Share(kept); !ok || !slices.Equal(layerRIDs(info), []string{"f"}) {
		t.Errorf("the other share = %+v, %v; want its layer kept", info, ok)
	}
	if _, ok := h.SFU.Share(gone); ok {
		t.Error("the stopped share is listed again")
	}
	var ends []string
	for _, ev := range h.Events.Events() {
		ends = append(ends, fmt.Sprintf("%s:%s", ev.Share.ID, ev.Reason))
	}
	if !slices.Equal(ends, []string{"s_1:stopped"}) {
		t.Errorf("room events = %v, want only the stopped share's end", ends)
	}
	for _, ev := range sig.Events() {
		if st, ok := ev.(sfu.PCStateEvent); !ok || st != (sfu.PCStateEvent{PC: sfu.PCPub, Gen: 1, State: "connected"}) {
			t.Errorf("the publisher got %+v, want only its pub PC connecting: every refusal was a call's error", ev)
		}
	}
}

// TestPCRateLimit: the guard on PeerConnection creations (02 §12). A client may make a Conn create 10 PCs of a kind
// within a minute: pub offers of a new gen, and sub PCs after rebuild requests (the Conn's first sub PC isn't one).
// One more is sfu.pc_rate_limited, retryable, with the time to wait, and changes nothing: the error of the call, or
// an ErrorEvent for the requests that have no error to put it in. When the oldest has left the window the next one
// passes.
func TestPCRateLimit(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{})
	const window = 3 * time.Second
	h.SFU.SetTimings(sfu.Timings{PCWindow: window})
	ctx := testCtx(t)
	video := webrtc.RTPCodecTypeVideo
	limited := func(what string, err error, started time.Time) {
		t.Helper()
		var e *sfu.Error
		if !errors.As(err, &e) || e.Code != sfu.CodePCRateLimited || !e.Retryable {
			t.Fatalf("%s = %v, want a retryable sfu.pc_rate_limited", what, err)
		}
		// The oldest of the ten was made just after started.
		if e.RetryAfter <= 0 || e.RetryAfter > window || e.RetryAfter < window-time.Since(started) {
			t.Errorf("%s: RetryAfter = %v, want the rest of the %v window that began %v ago", what, e.RetryAfter, window,
				time.Since(started).Round(time.Millisecond))
		}
	}

	// ---- pub: offers of a new gen ----
	pubConn, _ := join(t, h, "alice", "c-pub", sfu.RoleFull)
	const share = sfu.ShareID("s_rate")
	startShare(t, pubConn, share, sfu.PresetAuto)
	offerGen := func(gen uint32) error {
		t.Helper()
		p := newRawPublisher(t, video)
		raw := p.offer(t, ctx)
		_, err := pubConn.HandleOffer(ctx, sfu.PCPub, gen, 1, raw, []sfu.TrackBinding{{MID: p.mids[0], Share: share, Kind: video}})
		return err
	}
	pubStarted := time.Now()
	for gen := uint32(1); gen <= sfu.MaxPCCreations; gen++ {
		if err := offerGen(gen); err != nil {
			t.Fatalf("pub gen %d: %v", gen, err)
		}
	}
	// An offer the SFU's checks refuse makes no PC and counts for nothing; a re-offer makes none either.
	_, err := pubConn.HandleOffer(ctx, sfu.PCPub, 99, 1, "v=nonsense", nil)
	wantCode(t, err, sfu.CodeBadSDP)
	limited("an eleventh pub gen", offerGen(sfu.MaxPCCreations+1), pubStarted)
	if st := pcOf(t, pubConn, sfu.PCPub); !st.Exists || st.Gen != sfu.MaxPCCreations {
		t.Errorf("the pub PC after a refused gen = %+v, want gen %d kept", st, sfu.MaxPCCreations)
	}

	// ---- sub: ten rebuilds. The Conn's first sub PC isn't counted: its client can't cause another first one ----
	viewConn, viewSig := join(t, h, "bob", "c-view", sfu.RoleViewer) // nobody answers: a PC needs no answer to be rebuilt
	subscribe(t, viewConn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityLow})
	if _, err := viewSig.WaitOffer(ctx, func(o sfutest.Offer) bool { return o.Gen == 1 }); err != nil {
		t.Fatal(err)
	}
	const lastGen = sfu.MaxPCCreations + 1
	subStarted := time.Now()
	for gen := uint32(1); gen < lastGen; gen++ {
		if err := viewConn.ResetPC(ctx, sfu.PCSub, gen); err != nil {
			t.Fatalf("ResetPC of gen %d: %v", gen, err)
		}
	}
	limited("an eleventh rebuild", viewConn.ResetPC(ctx, sfu.PCSub, lastGen), subStarted)
	if st := pcOf(t, viewConn, sfu.PCSub); st.Gen != lastGen || st.Closed || !st.Offering {
		t.Errorf("the sub PC after a refused rebuild = %+v, want gen %d as it was", st, lastGen)
	}
	// The same for the requests that rebuild a closed PC. RestartICE returns the error. A subscription change and
	// Resync have no error to return it in: the client gets an ErrorEvent about its sub PC each time, so that it
	// knows to ask again, and how long to wait.
	if err := viewConn.ClosePC(ctx, sfu.PCSub, lastGen); err != nil {
		t.Fatal(err)
	}
	limited("RestartICE of a closed PC", viewConn.RestartICE(ctx, sfu.PCSub, lastGen), subStarted)
	errorEvents := func() (evs []sfu.ErrorEvent) {
		for _, ev := range viewSig.Events() {
			if e, ok := ev.(sfu.ErrorEvent); ok {
				evs = append(evs, e)
			}
		}
		return evs
	}
	if evs := errorEvents(); len(evs) != 0 {
		t.Errorf("error events after refusals that the calls returned: %+v", evs)
	}
	for i, request := range []struct {
		what string
		do   func()
	}{
		{"a subscription change", func() { subscribe(t, viewConn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityHigh}) }},
		{"Resync", viewConn.Resync},
	} {
		request.do()
		eventually(t, func() bool { return len(errorEvents()) > i },
			func() string {
				return fmt.Sprintf("no error event after %s past the limit: %+v", request.what, viewSig.Events())
			})
		evs := errorEvents()
		if len(evs) != i+1 || evs[i].Scope != sfu.ScopePCSub {
			t.Fatalf("after %s past the limit the client got %+v, want one more error about its sub PC", request.what, evs)
		}
		limited("the error event after "+request.what, evs[i].Err, subStarted)
	}
	if subs, err := viewConn.Subscriptions(ctx); err != nil || len(subs) != 1 || subs[0].Video != sfu.QualityHigh {
		t.Errorf("subscriptions = %+v, %v; want the change applied, though its PC wasn't built", subs, err)
	}
	time.Sleep(4 * 50 * time.Millisecond)
	if st := pcOf(t, viewConn, sfu.PCSub); st.Gen != lastGen || !st.Closed {
		t.Errorf("the closed sub PC after requests past the limit = %+v, want it still gen %d, closed", st, lastGen)
	}
	if n := len(viewSig.Offers()); n != lastGen {
		t.Errorf("%d sub offers, want one per PC: %d", n, lastGen)
	}

	// ---- the window moves on ----
	time.Sleep(time.Until(pubStarted.Add(window + 200*time.Millisecond)))
	if err := offerGen(sfu.MaxPCCreations + 1); err != nil {
		t.Errorf("a pub gen after the window: %v", err)
	}
	time.Sleep(time.Until(subStarted.Add(window + 200*time.Millisecond)))
	subscribe(t, viewConn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityLow})
	if _, err := viewSig.WaitOffer(ctx, func(o sfutest.Offer) bool { return o.Gen == lastGen+1 }); err != nil {
		t.Errorf("the closed sub PC wasn't rebuilt by a subscription change after the window: %v", err)
	}
	if evs := errorEvents(); len(evs) != 2 {
		t.Errorf("error events = %+v, want none for the rebuild after the window", evs)
	}
}

// TestPCGrace: a PeerConnection that fails is kept for 30 s (here 1.5 s) for an ICE restart or a rebuild, then closed
// and reported closed with the pc_grace_expired reason (02 §5.3, §12). An ICE restart within the grace keeps it. A
// failed pub PC's shares are stalled at once and stay so, for the hub's own timeout; the SFU never ends them.
func TestPCGrace(t *testing.T) {
	t.Cleanup(sfu.SetMediaICETimeouts(time.Second, time.Second, 200*time.Millisecond))
	h := newHarness(t, sfutest.HarnessOptions{})
	const grace = 1500 * time.Millisecond
	h.SFU.SetTimings(sfu.Timings{Grace: grace})
	ctx := ctxFor(t, 60*time.Second)
	video := webrtc.RTPCodecTypeVideo
	const steady, shaky = sfu.ShareID("s_steady"), sfu.ShareID("s_shaky")
	publishShare(t, h, "alice", "c-steady", steady, 5)
	waitLive(t, h, steady)

	// A publisher and two viewers, each with a network of its own to lose.
	shakyConn, shakySig := join(t, h, "carol", "c-shaky", sfu.RoleFull)
	startShare(t, shakyConn, shaky, sfu.PresetAuto)
	pubSettings, pubWire := faultySettings(t)
	src := newLongGOPSource(t)
	shakyPub := startPublisher(t, ctx, shakyConn, src, pubSettings, 1, shaky)
	waitLive(t, h, shaky)
	type client struct {
		conn   *sfu.Conn
		sig    *sfutest.DirectSignaler
		viewer *sfutest.Viewer
		wire   *sfutest.FaultConn
		video  *sfutest.Recorder
	}
	newClient := func(user sfu.UserID) *client {
		c := &client{}
		c.conn, c.sig = join(t, h, user, sfu.ConnID("c-"+user), sfu.RoleViewer)
		var settings webrtc.SettingEngine
		settings, c.wire = faultySettings(t)
		c.viewer = newViewer(t, settings)
		c.sig.Attach(c.conn, c.viewer)
		subscribe(t, c.conn, sfu.SubscriptionUpdate{Share: steady, Video: sfu.QualityHigh, Audio: true})
		c.video = streamTrack(t, c.viewer, steady, video)
		waitFrames(t, c.video, 0, "f", 15)
		return c
	}
	lost, saved := newClient("bob"), newClient("dave")
	roomEvents := len(h.Events.Events())

	failed := func(what string, sig *sfutest.DirectSignaler, kind sfu.PCKind) time.Time {
		t.Helper()
		ev, err := sig.WaitEvent(ctx, func(ev sfu.Event) bool {
			st, ok := ev.(sfu.PCStateEvent)
			return ok && st.State == "failed"
		})
		if err != nil {
			t.Fatalf("%s: %v (events %+v)", what, err, pcEvents(sig))
		}
		if want := (sfu.PCStateEvent{PC: kind, Gen: 1, State: "failed"}); ev != sfu.Event(want) {
			t.Errorf("%s got %+v, want %+v", what, ev, want)
		}
		return time.Now()
	}
	expired := func(what string, sig *sfutest.DirectSignaler, kind sfu.PCKind, failedAt time.Time) {
		t.Helper()
		want := sfu.PCStateEvent{PC: kind, Gen: 1, State: "closed", Reason: sfu.PCReasonGraceExpired}
		if _, err := sig.WaitEvent(ctx, func(ev sfu.Event) bool { return ev == sfu.Event(want) }); err != nil {
			t.Fatalf("%s: %v (events %+v)", what, err, pcEvents(sig))
		}
		// failedAt is when the test saw the failure, which is a little after it happened.
		if d := time.Since(failedAt); d < grace-500*time.Millisecond || d > grace+time.Second {
			t.Errorf("%s was closed %v after it failed, want the grace of %v", what, d.Round(time.Millisecond), grace)
		}
		evs := pcEvents(sig)
		if last := evs[len(evs)-1]; last != want {
			t.Errorf("%s: the last PC event is %+v, want the grace's end", what, last)
		}
	}

	// ---- a viewer that gets an ICE restart in time: its network is back when its PC has failed ----
	blackHole(saved.wire, true)
	savedAt := failed("the viewer to be saved", saved.sig, sfu.PCSub)
	if st := pcOf(t, saved.conn, sfu.PCSub); !st.GraceTimer {
		t.Errorf("the failed sub PC = %+v, want its grace running", st)
	}
	mark := len(saved.video.Packets())
	blackHole(saved.wire, false)
	if err := saved.conn.RestartICE(ctx, sfu.PCSub, 1); err != nil {
		t.Fatal(err)
	}
	waitResumed(t, ctx, saved.video, mark, "f", 8)
	// Past its grace, it was neither closed nor reported again.
	time.Sleep(time.Until(savedAt.Add(grace + 500*time.Millisecond)))
	if st := pcOf(t, saved.conn, sfu.PCSub); st.Closed || st.State != "connected" || st.GraceTimer {
		t.Errorf("the restarted sub PC after the grace = %+v, want it connected, without a timer", st)
	}
	for _, ev := range pcEvents(saved.sig) {
		if ev.Reason != "" || ev.State == "closed" {
			t.Errorf("the viewer whose ICE was restarted in time got %+v", ev)
		}
	}

	// ---- a publisher and a viewer that nobody restarts ----
	blackHole(pubWire, true)
	blackHole(lost.wire, true)
	// The publisher: failed, its share stalled at once; then the grace runs out.
	pubFailed := failed("the publisher", shakySig, sfu.PCPub)
	stalled, err := h.Events.Wait(ctx, func(ev sfutest.RoomEvent) bool {
		return ev.Kind == sfutest.ShareUpdated && ev.Share.ID == shaky && ev.Share.State == sfu.ShareStalled
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stalled.Share.Layers) != 2 || !stalled.Share.Audio {
		t.Errorf("the stalled share = %+v, want its tracks still attached: the PC failed, it didn't close", stalled.Share)
	}
	expired("the pub PC", shakySig, sfu.PCPub, pubFailed)
	if st := pcOf(t, shakyConn, sfu.PCPub); st.Exists || st.Gen != 1 {
		t.Errorf("the pub PC after its grace = %+v, want none", st)
	}
	if info, ok := h.SFU.Share(shaky); !ok || info.State != sfu.ShareStalled || len(info.Layers) != 0 {
		t.Errorf("the share after its pub PC's grace = %+v, %v; want it stalled, without tracks, and not ended", info, ok)
	}

	// The viewer: its sub PC failed at about the same time, and is closed the same way.
	failed("the lost viewer", lost.sig, sfu.PCSub)
	want := sfu.PCStateEvent{PC: sfu.PCSub, Gen: 1, State: "closed", Reason: sfu.PCReasonGraceExpired}
	if _, err := lost.sig.WaitEvent(ctx, func(ev sfu.Event) bool { return ev == sfu.Event(want) }); err != nil {
		t.Fatalf("the lost viewer's sub PC: %v (events %+v)", err, pcEvents(lost.sig))
	}
	if st := pcOf(t, lost.conn, sfu.PCSub); !st.Closed || st.GraceTimer {
		t.Errorf("the sub PC after its grace = %+v, want it closed", st)
	}
	if subs, err := lost.conn.Subscriptions(ctx); err != nil || len(subs) != 1 {
		t.Errorf("the lost viewer's subscriptions = %+v, %v; want the one kept for the rebuild", subs, err)
	}

	// ---- afterwards: the publisher offers gen 2, the lost viewer asks for a rebuild ----
	blackHole(pubWire, false)
	if err := shakyPub.Close(); err != nil {
		t.Fatal(err)
	}
	startPublisher(t, ctx, shakyConn, src, pubSettings, 2, shaky)
	waitShare(t, h.SFU, shaky, func(i sfu.ShareInfo) bool { return i.State == sfu.ShareLive && len(i.Layers) == 2 && i.Audio })
	eventually(t, func() bool { return shareStates(h, shaky, roomEvents) == "stalled live" },
		func() string { return "the shaky share's states are " + shareStates(h, shaky, roomEvents) })
	blackHole(lost.wire, false)
	if err := lost.conn.ResetPC(ctx, sfu.PCSub, 1); err != nil {
		t.Fatal(err)
	}
	waitFrames(t, nextTrack(t, ctx, lost.viewer, steady, video, lost.video), 0, "f", 8)
	// The share nobody touched was live all along, and nothing ended.
	if states := shareStates(h, steady, roomEvents); states != "" && states != "live" {
		t.Errorf("the steady share went through %q", states)
	}
	for _, ev := range h.Events.Events() {
		if ev.Kind == sfutest.ShareEnded {
			t.Errorf("room event %+v: the SFU never ends a share by itself", ev)
		}
	}
}

// TestPubICERestart: the pub side of 02 §6.5. A publisher loses its UDP: the SFU sees its pub PC disconnected, and
// 2 s later the share is stalled (02 §5.3). The publisher restarts ICE with an offer of its own, the same gen and the
// next neg: the same PC and tracks carry on, the share is live again with a keyframe, and its viewer needs nothing.
func TestPubICERestart(t *testing.T) {
	t.Cleanup(sfu.SetMediaICETimeouts(time.Second, 20*time.Second, 200*time.Millisecond))
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx := ctxFor(t, 60*time.Second)
	const share = sfu.ShareID("s_pubice")
	pubConn, pubSig := join(t, h, "alice", "c-pub", sfu.RoleFull)
	startShare(t, pubConn, share, sfu.PresetAuto)
	settings, wire := faultySettings(t)
	pub := newPublisher(t, newLongGOPSource(t), settings)
	first, err := sfutest.Publish(ctx, pubConn, pub, 1, 1, share)
	if err != nil {
		t.Fatal(err)
	}
	if err := pub.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitLive(t, h, share)
	viewConn, viewSig := join(t, h, "bob", "c-view", sfu.RoleViewer)
	viewer := newViewer(t, sfutest.LoopbackSettings())
	viewSig.Attach(viewConn, viewer)
	subscribe(t, viewConn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityHigh, Audio: true})
	video := streamTrack(t, viewer, share, webrtc.RTPCodecTypeVideo)
	waitFrames(t, video, 0, "f", 30)
	tracksBefore, err := pubConn.PubTracks(ctx)
	if err != nil || len(tracksBefore) != 3 {
		t.Fatalf("pub tracks = %+v, %v", tracksBefore, err)
	}
	before, _ := h.SFU.Share(share)
	roomEvents := len(h.Events.Events())

	// ---- the publisher's UDP is gone ----
	blackHole(wire, true)
	if err := pubSig.WaitPCState(ctx, sfu.PCPub, 1, "disconnected"); err != nil {
		t.Fatal(err)
	}
	disconnected := time.Now()
	if info, _ := h.SFU.Share(share); info.State != sfu.ShareLive {
		t.Errorf("the share right after its pub PC was disconnected = %+v, want it live for another 2 s", info)
	}
	stalled, err := h.Events.Wait(ctx, func(ev sfutest.RoomEvent) bool {
		return ev.Kind == sfutest.ShareUpdated && ev.Share.ID == share && ev.Share.State == sfu.ShareStalled
	})
	if err != nil {
		t.Fatal(err)
	}
	// The ticker looks once a second, so "2 s" is between 2 and 3.
	if d := stalled.At.Sub(disconnected); d < 1900*time.Millisecond || d > 3500*time.Millisecond {
		t.Errorf("the share was stalled %v after its pub PC was disconnected, want 2 s (and at most a ticker second more)",
			d.Round(time.Millisecond))
	}
	if len(stalled.Share.Layers) != 2 || !stalled.Share.Audio || !stalled.Share.LiveAt.Equal(before.LiveAt) {
		t.Errorf("the stalled share = %+v, want its tracks attached and its times kept", stalled.Share)
	}

	// ---- the publisher restarts ICE: gen 1, neg 2 ----
	mark := len(video.Packets())
	blackHole(wire, false)
	second, err := sfutest.PublishICERestart(ctx, pubConn, pub, 1, 2, share)
	if err != nil {
		t.Fatalf("the ICE-restart offer: %v", err)
	}
	if a, b := iceUfrag(t, first), iceUfrag(t, second); a == b {
		t.Error("the answer to an ICE-restart offer has the ICE user name of the answer before")
	}
	checkServerCandidates(t, h, "the answer to the ICE-restart offer", second)
	eventually(t, func() bool { return shareStates(h, share, roomEvents) == "stalled live" },
		func() string { return "the share's states since the outage are " + shareStates(h, share, roomEvents) })
	// The same PC, the same tracks: nothing was attached anew.
	if st := pcOf(t, pubConn, sfu.PCPub); !st.Exists || st.Gen != 1 || st.Neg != 2 || st.State != "connected" || st.GraceTimer || st.HandshakeTimer {
		t.Errorf("the pub PC after the ICE restart = %+v, want gen 1, neg 2, connected", st)
	}
	// The share is live again with the first keyframe on `f`: the readers of the other tracks may not have read their
	// next packet yet, so every track is given a moment to show that it is still read.
	var (
		tracksAfter []sfu.PubTrackState
		tracksErr   error
	)
	eventually(t, func() bool {
		tracksAfter, tracksErr = pubConn.PubTracks(ctx)
		if tracksErr != nil || len(tracksAfter) != len(tracksBefore) {
			return false
		}
		for i, tr := range tracksAfter {
			was := tracksBefore[i]
			if tr.MID != was.MID || tr.RID != was.RID || tr.Share != share || tr.Packets <= was.Packets {
				return false
			}
		}
		return true
	}, func() string {
		return fmt.Sprintf("pub tracks after the ICE restart = %+v (%v), were %+v: want the same tracks, still read",
			tracksAfter, tracksErr, tracksBefore)
	})
	for _, ev := range pcEvents(pubSig) {
		if ev.Gen != 1 || ev.Reason != "" || (ev.State != "connected" && ev.State != "disconnected") {
			t.Errorf("the publisher got %+v", ev)
		}
	}
	// The viewer: the same track, on with the keyframe the SFU asked for when the pub PC was connected again.
	resumed, _ := waitResumed(t, ctx, video, mark, "f", 15)
	if err := sfutest.CheckContinuous(sfutest.WithoutPartialTail(resumed)); err != nil {
		t.Errorf("the viewer's video since the ICE restart: %v", err)
	}
	if n := tracksOf(viewer, share, webrtc.RTPCodecTypeVideo); n != 1 || len(viewSig.Offers()) != 1 {
		t.Errorf("the viewer has %d video tracks and got %d sub offers, want 1 and 1", n, len(viewSig.Offers()))
	}
	if st := layerStats(t, pub, "f"); st.PLIs == 0 {
		t.Error("the publisher got no PLI when its pub PC was connected again")
	}
}

// TestClosePub: the pub side of ClosePC (01's pc.close). The PC is closed without a report, its gen is over, and a
// gen the SFU never saw can be closed too. The share is the hub's to end; left alone it is stalled.
func TestClosePub(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx := testCtx(t)
	const share = sfu.ShareID("s_close")
	conn, sig := join(t, h, "alice", "c-pub", sfu.RoleFull)
	startShare(t, conn, share, sfu.PresetAuto)
	src := newLongGOPSource(t)
	pub := startPublisher(t, ctx, conn, src, sfutest.LoopbackSettings(), 1, share)
	waitLive(t, h, share)
	roomEvents := len(h.Events.Events())

	if err := conn.ClosePC(ctx, sfu.PCPub, 0); err != nil {
		t.Errorf("ClosePC for an older gen = %v, want it ignored", err)
	}
	if st := pcOf(t, conn, sfu.PCPub); !st.Exists {
		t.Fatalf("the pub PC after ClosePC for an older gen = %+v", st)
	}
	wantCode(t, conn.ClosePC(ctx, sfu.PCKind(3), 1), sfu.CodeBadPC)
	wantCode(t, conn.ClosePC(ctx, sfu.PCSub, 1), sfu.CodeBadPC) // the Conn has no sub PC

	if err := conn.ClosePC(ctx, sfu.PCPub, 1); err != nil {
		t.Fatalf("ClosePC: %v", err)
	}
	if st := pcOf(t, conn, sfu.PCPub); st.Exists || st.Gen != 1 {
		t.Errorf("the pub PC after ClosePC = %+v, want none, with gen 1 over", st)
	}
	if info, ok := h.SFU.Share(share); !ok || info.State != sfu.ShareStalled || len(info.Layers) != 0 || info.Audio {
		t.Errorf("the share after its pub PC was closed = %+v, %v; want it stalled without tracks", info, ok)
	}
	if err := conn.ClosePC(ctx, sfu.PCPub, 1); err != nil {
		t.Errorf("ClosePC of a closed PC = %v, want nothing", err)
	}
	// The client's side closes too; Pion's goodbye finds no PC to report about.
	if err := pub.Close(); err != nil {
		t.Fatal(err)
	}
	offer, err := newPublisher(t, src, sfutest.LoopbackSettings()).Offer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = conn.HandleOffer(ctx, sfu.PCPub, 1, 2, offer.SDP, nil)
	wantCode(t, err, sfu.CodeBadPC)

	// The next gen is a new PC, and the share is live again.
	pub2 := startPublisher(t, ctx, conn, src, sfutest.LoopbackSettings(), 2, share)
	waitShare(t, h.SFU, share, func(i sfu.ShareInfo) bool { return i.State == sfu.ShareLive && len(i.Layers) == 2 })
	eventually(t, func() bool { return shareStates(h, share, roomEvents) == "stalled live" },
		func() string { return "the share's states are " + shareStates(h, share, roomEvents) })

	// The client names a gen the SFU never had: an offer of its gen 5 was refused, and it has closed that PC. The pub
	// PC the Conn still has, gen 2, is one the client left behind: it goes. No gen is used up by that.
	if err := conn.ClosePC(ctx, sfu.PCPub, 5); err != nil {
		t.Fatalf("ClosePC of a gen the SFU never had: %v", err)
	}
	if st := pcOf(t, conn, sfu.PCPub); st.Exists || st.Gen != 2 {
		t.Errorf("after ClosePC of gen 5: %+v, want no PC, and gen 2 the last one accepted", st)
	}
	waitShare(t, h.SFU, share, func(i sfu.ShareInfo) bool { return i.State == sfu.ShareStalled && len(i.Layers) == 0 })
	_, err = conn.HandleOffer(ctx, sfu.PCPub, 2, 2, offer.SDP, nil)
	wantCode(t, err, sfu.CodeBadPC)
	_, err = conn.HandleOffer(ctx, sfu.PCPub, 1, 3, offer.SDP, nil)
	wantCode(t, err, sfu.CodeStaleOffer)
	if err := pub2.Close(); err != nil {
		t.Fatal(err)
	}
	startPublisher(t, ctx, conn, src, sfutest.LoopbackSettings(), 3, share)
	waitShare(t, h.SFU, share, func(i sfu.ShareInfo) bool { return i.State == sfu.ShareLive && len(i.Layers) == 2 })

	time.Sleep(200 * time.Millisecond) // the closed PCs' own callbacks are through
	var got []string
	for _, ev := range pcEvents(sig) {
		got = append(got, fmt.Sprintf("%s/%d:%s%s", ev.PC, ev.Gen, ev.State, ev.Reason))
	}
	if want := []string{"pub/1:connected", "pub/2:connected", "pub/3:connected"}; !slices.Equal(got, want) {
		t.Errorf("the publisher's PC events = %v, want %v: a PC closed for the client reports nothing", got, want)
	}
	if states := shareStates(h, share, roomEvents); states != "stalled live stalled live" {
		t.Errorf("the share's states = %q, want it stalled while it had no pub PC, twice", states)
	}
}
