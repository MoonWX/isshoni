package sfu_test

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/server/sfu"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// The acceptance tests of simulcast and layer selection (README S52): integration 2, 3, 10 and 17 of 02 §17, through
// sfutest.Harness with real PeerConnections and fake media.

// subscribe sets a Conn's subscriptions and fails the test on any error.
func subscribe(t *testing.T, c *sfu.Conn, items ...sfu.SubscriptionUpdate) {
	t.Helper()
	errs, err := c.UpdateSubscriptions(testCtx(t), items)
	if err != nil || slices.ContainsFunc(errs, func(e error) bool { return e != nil }) {
		t.Fatalf("UpdateSubscriptions(%+v) = %v, %v", items, errs, err)
	}
}

// subEvents returns the SubscriptionStateEvents a Conn's client got about a share, in order.
func subEvents(sig *sfutest.DirectSignaler, share sfu.ShareID) []sfu.SubscriptionStateEvent {
	var out []sfu.SubscriptionStateEvent
	for _, ev := range sig.Events() {
		if st, ok := ev.(sfu.SubscriptionStateEvent); ok && st.Share == share {
			out = append(out, st)
		}
	}
	return out
}

// waitSubState waits until the last SubscriptionStateEvent the client got about a share is want.
func waitSubState(t *testing.T, sig *sfutest.DirectSignaler, want sfu.SubscriptionStateEvent) {
	t.Helper()
	eventually(t, func() bool {
		evs := subEvents(sig, want.Share)
		return len(evs) > 0 && evs[len(evs)-1] == want
	}, func() string {
		return fmt.Sprintf("the client's events about %s are %+v, want the last to be %+v", want.Share, subEvents(sig, want.Share), want)
	})
}

// streamTrack waits for the Viewer's newest track of a kind in a share's stream (the msid stream is the share id).
func streamTrack(t *testing.T, v *sfutest.Viewer, share sfu.ShareID, kind webrtc.RTPCodecType) *sfutest.Recorder {
	t.Helper()
	match := func(r *sfutest.Recorder) bool { return r.Kind() == kind && r.StreamID() == string(share) }
	if _, err := v.WaitRecorder(testCtx(t), match); err != nil {
		t.Fatalf("the viewer got no %s of %s: %v", kind, share, err)
	}
	recs := v.Recorders()
	for i := len(recs) - 1; i >= 0; i-- {
		if match(recs[i]) {
			return recs[i]
		}
	}
	return nil
}

// hasTrack reports whether the Viewer ever got a track of a kind in a share's stream. A Viewer learns of a track with
// its first packet.
func hasTrack(v *sfutest.Viewer, share sfu.ShareID, kind webrtc.RTPCodecType) bool {
	return slices.ContainsFunc(v.Recorders(), func(r *sfutest.Recorder) bool {
		return r.Kind() == kind && r.StreamID() == string(share)
	})
}

// layerFrames counts the whole frames of a layer among pkts.
func layerFrames(pkts []sfutest.Packet, rid string) int {
	n := 0
	for _, f := range sfutest.Frames(sfutest.WithoutPartialTail(pkts)) {
		if f.HasMark && f.Mark.RID == rid {
			n++
		}
	}
	return n
}

// waitFrames waits until the Recorder has n whole frames of a layer after its first skip packets.
func waitFrames(t *testing.T, r *sfutest.Recorder, skip int, rid string, n int) {
	t.Helper()
	err := r.Wait(testCtx(t), func(r *sfutest.Recorder) bool {
		pkts := r.Packets()
		return len(pkts) >= skip && layerFrames(pkts[skip:], rid) >= n
	})
	if err != nil {
		t.Fatalf("%d frames of layer %s: %v", n, rid, err)
	}
}

// checkVideo checks the video a viewer got, up to its last whole frame: one continuous stream (no sequence number
// missing or twice, timestamps that never go back) that starts on an SPS, as does every change of layer in it, of
// whole frames with consecutive frame indices within each layer. The one frame that may be short is the old layer's
// last before a switch (sfutest.WithoutCutFrames). It returns the packets it checked.
func checkVideo(t *testing.T, what string, pkts []sfutest.Packet) []sfutest.Packet {
	t.Helper()
	pkts = sfutest.WithoutPartialTail(pkts)
	for _, check := range []func([]sfutest.Packet) error{sfutest.CheckContinuous, sfutest.CheckStartsOnSPS} {
		if err := check(pkts); err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}
	if err := sfutest.CheckVideoMarkers(sfutest.WithoutCutFrames(pkts)); err != nil {
		t.Errorf("%s: %v", what, err)
	}
	return pkts
}

// layerRuns returns the layers of the frames in order, each run of one layer once: "f q f".
func layerRuns(pkts []sfutest.Packet) string {
	var runs []string
	for _, f := range sfutest.Frames(pkts) {
		if f.HasMark && (len(runs) == 0 || runs[len(runs)-1] != f.Mark.RID) {
			runs = append(runs, f.Mark.RID)
		}
	}
	return strings.Join(runs, " ")
}

// checkSwitchTimestamps checks that the viewer's timeline stays the capture timeline across layer switches and
// pauses: between the last frame of one layer and the first of the next, the RTP timestamp advances by their capture
// time difference (02 §9.4: aligned through both layers' sender reports, else by the wall clock). The tolerance is
// what the wall-clock rule may add on a busy machine; a wrong offset is off by a random 32-bit number.
func checkSwitchTimestamps(t *testing.T, pkts []sfutest.Packet) {
	t.Helper()
	const tolerance = 50 * time.Millisecond
	frames := sfutest.Frames(pkts)
	for i := 1; i < len(frames); i++ {
		a, b := frames[i-1], frames[i]
		if !a.HasMark || !b.HasMark || a.Mark.RID == b.Mark.RID {
			continue
		}
		got := time.Duration(int32(b.TS-a.TS)) * time.Second / 90000
		want := time.Duration(b.Mark.CaptureNS - a.Mark.CaptureNS)
		if d := got - want; d < -tolerance || d > tolerance {
			t.Errorf("switch %s → %s at seq %d: the timestamp advances by %v, the capture time by %v", a.Mark.RID,
				b.Mark.RID, b.FirstSeq, got, want)
		}
	}
}

// publishShare starts a share on a new Conn of user and publishes fake media on it: simulcast f and q with audio,
// with a GOP far longer than a test, so that every keyframe after a layer's first is one somebody asked for.
func publishShare(t *testing.T, h *sfutest.Harness, user sfu.UserID, conn sfu.ConnID, share sfu.ShareID, seed uint64,
) (*sfu.Conn, *publish.Publisher) {
	t.Helper()
	ctx := testCtx(t)
	c, _ := join(t, h, user, conn, sfu.RoleFull)
	startShare(t, c, share, sfu.PresetAuto)
	src, err := fake.New(fake.Config{
		Layers: []fake.VideoLayer{
			{RID: "f", Width: 640, Height: 360, FPS: 30, Bitrate: 400_000},
			{RID: "q", Width: 320, Height: 180, FPS: 15, Bitrate: 100_000},
		},
		GOP: time.Minute, Audio: true, Seed: seed,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	pub := newPublisher(t, src, sfutest.LoopbackSettings())
	if _, err := sfutest.Publish(ctx, c, pub, 1, 1, share); err != nil {
		t.Fatalf("publish negotiation of %s: %v", share, err)
	}
	if err := pub.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return c, pub
}

// waitLive waits until a share is live with both simulcast layers and its audio attached.
func waitLive(t *testing.T, h *sfutest.Harness, share sfu.ShareID) {
	t.Helper()
	waitShare(t, h.SFU, share, func(i sfu.ShareInfo) bool {
		return i.State == sfu.ShareLive && len(i.Layers) == 2 && i.Audio
	})
}

// TestLayerSwitchOnKeyframe is integration 2 of 02 §17 (the S4 port): a viewer goes high → low → off → high. Each
// switch takes less than a second, because the publisher is asked for a keyframe and doesn't wait for its GOP to
// end; the viewer's stream is one continuous sequence through all of it; every change of layer starts on an SPS;
// nothing flows while off; the quality changes never renegotiate; and the client hears of each change when it has
// happened.
func TestLayerSwitchOnKeyframe(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx := testCtx(t)
	const share = sfu.ShareID("s_switch")
	_, pub := publishShare(t, h, "alice", "c-pub", share, 52)
	waitLive(t, h, share)
	viewConn, viewSig := join(t, h, "bob", "c-view", sfu.RoleViewer)
	viewer := newViewer(t, sfutest.LoopbackSettings())
	viewSig.Attach(viewConn, viewer)
	state := func(requested, forwarded sfu.Quality, audio bool) sfu.SubscriptionStateEvent {
		return sfu.SubscriptionStateEvent{Share: share, Requested: requested, Forwarded: forwarded, Audio: audio}
	}

	// ---- high ----
	subscribe(t, viewConn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityHigh, Audio: true})
	video, audio := streamTrack(t, viewer, share, webrtc.RTPCodecTypeVideo), streamTrack(t, viewer, share, webrtc.RTPCodecTypeAudio)
	// A second and a half: by then the publisher has sent a sender report for each layer, which a switch aligns the
	// timestamps with.
	waitFrames(t, video, 0, "f", 45)
	if err := audio.Wait(ctx, func(r *sfutest.Recorder) bool { return r.Stats().Packets >= 50 }); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return layerStats(t, pub, "f").SRs >= 1 && layerStats(t, pub, "q").SRs >= 1 },
		func() string { return fmt.Sprintf("the publisher's stats are %+v", pub.Stats()) })
	waitSubState(t, viewSig, state(sfu.QualityHigh, sfu.QualityHigh, true))
	if st := layerStats(t, pub, "q"); st.PLIs != 0 || st.Keyframes != 1 {
		t.Errorf("layer q before anybody asked for low: %d PLIs, %d keyframes", st.PLIs, st.Keyframes)
	}

	// ---- high → low: within a second, on the preview layer's keyframe ----
	mark := len(video.Packets())
	asked := time.Now()
	subscribe(t, viewConn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityLow, Audio: true})
	waitFrames(t, video, mark, "q", 8)
	waitSubState(t, viewSig, state(sfu.QualityLow, sfu.QualityLow, true))
	toLow := switchTime(t, video.Packets()[mark:], "q", asked)
	if st := layerStats(t, pub, "q"); st.PLIs < 1 || st.PLIs > 2 || st.Keyframes < 2 {
		t.Errorf("layer q after the switch: %d PLIs, %d keyframes; want the one request and its keyframe", st.PLIs, st.Keyframes)
	}

	// ---- low → off: nothing flows, video or audio ----
	subscribe(t, viewConn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityOff})
	waitSubState(t, viewSig, state(sfu.QualityOff, sfu.QualityOff, false))
	time.Sleep(100 * time.Millisecond) // what was on its way has arrived
	pausedVideo, pausedAudio := video.Stats().Packets, audio.Stats().Packets
	keysBefore := layerStats(t, pub, "f").Keyframes + layerStats(t, pub, "q").Keyframes
	time.Sleep(500 * time.Millisecond)
	if v, a := video.Stats().Packets, audio.Stats().Packets; v != pausedVideo || a != pausedAudio {
		t.Errorf("while off for 500 ms the viewer got %d video and %d audio packets, want none", v-pausedVideo, a-pausedAudio)
	}
	if keys := layerStats(t, pub, "f").Keyframes + layerStats(t, pub, "q").Keyframes; keys != keysBefore {
		t.Errorf("the publisher made %d keyframes while nobody watched", keys-keysBefore)
	}
	if info, _ := h.SFU.Share(share); len(info.Viewers) != 0 || info.State != sfu.ShareLive {
		t.Errorf("the share while its only viewer is off = %+v, want live and unwatched", info)
	}

	// ---- off → high: again within a second, on a keyframe ----
	mark = len(video.Packets())
	asked = time.Now()
	subscribe(t, viewConn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityHigh, Audio: true})
	waitFrames(t, video, mark, "f", 15)
	waitSubState(t, viewSig, state(sfu.QualityHigh, sfu.QualityHigh, true))
	toHigh := switchTime(t, video.Packets()[mark:], "f", asked)
	t.Logf("high → low took %v, off → high %v (S4: 42–106 ms); publisher PLIs: f %d, q %d", toLow.Round(time.Millisecond),
		toHigh.Round(time.Millisecond), layerStats(t, pub, "f").PLIs, layerStats(t, pub, "q").PLIs)

	// ---- the whole stream ----
	vp := checkVideo(t, "video", video.Packets())
	if runs := layerRuns(vp); runs != "f q f" {
		t.Errorf("the viewer got the layers %q, want f, then q, then f again", runs)
	}
	checkSwitchTimestamps(t, vp)
	if st := video.Stats(); st.Gaps != 0 || st.Lost != 0 || st.Duplicates != 0 {
		t.Errorf("video stats on loopback = %+v, want no gap", st)
	}
	// Audio paused and resumed with the video: its sequence numbers go on without a gap, its timestamps by the time
	// that passed.
	ap := audio.Packets()
	if err := sfutest.CheckContinuous(ap); err != nil {
		t.Errorf("audio: %v", err)
	}
	if pauses := checkAudioRuns(t, ap); pauses != 1 {
		t.Errorf("the audio has %d pauses, want the one while off", pauses)
	}
	// Quality never renegotiates, and the client heard only what changed: nothing with a reason (every layer was
	// there all along), nothing twice.
	if n := len(viewSig.Offers()); n != 1 {
		t.Errorf("%d sub offers, want 1: changing quality never renegotiates", n)
	}
	evs := subEvents(viewSig, share)
	for i, ev := range evs {
		if ev.Reason != "" || ev.Forwarded > ev.Requested || (i > 0 && ev == evs[i-1]) {
			t.Errorf("event %d of %+v: want no reason, nothing above the request, nothing twice", i, evs)
		}
	}
	if len(evs) > 6 {
		t.Errorf("%d SubscriptionStateEvents for four requests: %+v", len(evs), evs)
	}
	if errs := viewSig.Errs(); len(errs) != 0 {
		t.Errorf("answering the sub offer: %v", errs)
	}
}

// switchTime checks the packets a viewer got since it asked for another layer at asked: the first frame of that
// layer starts on an SPS, nothing of another layer follows it, and it came within a second. It returns how long it
// took.
func switchTime(t *testing.T, pkts []sfutest.Packet, rid string, asked time.Time) time.Duration {
	t.Helper()
	frames := sfutest.Frames(sfutest.WithoutPartialTail(pkts))
	first := slices.IndexFunc(frames, func(f sfutest.Frame) bool { return f.HasMark && f.Mark.RID == rid })
	if first < 0 {
		t.Fatalf("no frame of layer %s after the request", rid)
	}
	if !frames[first].KeyStart {
		t.Errorf("the first frame of layer %s (seq %d) doesn't start on an SPS", rid, frames[first].FirstSeq)
	}
	for _, f := range frames[first:] {
		if !f.HasMark || f.Mark.RID != rid {
			t.Errorf("a frame of layer %q after the switch to %s (seq %d)", f.Mark.RID, rid, f.FirstSeq)
			break
		}
	}
	took := frames[first].First.Sub(asked)
	if took > time.Second {
		t.Errorf("the switch to layer %s took %v, want less than 1 s", rid, took)
	}
	return took
}

// checkAudioRuns checks fake audio that was paused and resumed: every packet has its marker, the packet indices only
// go forward, and across a pause the timestamp advances by exactly the media time that was left out (20 ms, 960
// ticks, per packet): a resumed audio stream is still on its publisher's timeline (02 §9.4). It returns the number of
// pauses.
func checkAudioRuns(t *testing.T, pkts []sfutest.Packet) (pauses int) {
	t.Helper()
	for i, p := range pkts {
		if !p.HasMark {
			t.Errorf("audio seq %d has no marker", p.Seq)
			return pauses
		}
		if i == 0 {
			continue
		}
		prev := pkts[i-1]
		step := int64(p.Mark.Frame) - int64(prev.Mark.Frame)
		if step < 1 || int64(int32(p.TS-prev.TS)) != step*960 || p.Seq != prev.Seq+1 {
			t.Errorf("audio seq %d → %d: packet %d → %d, timestamp +%d; want the next seq and 960 ticks per packet",
				prev.Seq, p.Seq, prev.Mark.Frame, p.Mark.Frame, int32(p.TS-prev.TS))
			return pauses
		}
		if step > 1 {
			pauses++
		}
	}
	return pauses
}

// TestAudioFollowsFocus is integration 3 of 02 §17: a viewer of two shares gets audio only for the one whose
// subscription has Audio set, normally the focused one. Moving the focus stops one audio and starts the other in the
// same step; toggling needs no renegotiation; and an audio stream that resumes goes on with the next sequence number,
// on its publisher's timeline.
func TestAudioFollowsFocus(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx := testCtx(t)
	const shareA, shareB = sfu.ShareID("s_a"), sfu.ShareID("s_b")
	publishShare(t, h, "alice", "c-a", shareA, 1)
	publishShare(t, h, "carol", "c-b", shareB, 2)
	waitLive(t, h, shareA)
	waitLive(t, h, shareB)
	viewConn, viewSig := join(t, h, "bob", "c-view", sfu.RoleViewer)
	viewer := newViewer(t, sfutest.LoopbackSettings())
	viewSig.Attach(viewConn, viewer)
	focus := func(share sfu.ShareID, on bool) sfu.SubscriptionUpdate {
		if on {
			return sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityHigh, Audio: true}
		}
		return sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityLow}
	}
	focused := func(share sfu.ShareID, on bool) sfu.SubscriptionStateEvent {
		u := focus(share, on)
		return sfu.SubscriptionStateEvent{Share: share, Requested: u.Video, Forwarded: u.Video, Audio: u.Audio}
	}
	audioSent := func(share sfu.ShareID) uint64 {
		t.Helper()
		subs, err := viewConn.Subscriptions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range subs {
			if s.Share == share {
				return s.AudioSent
			}
		}
		t.Fatalf("no subscription to %s", share)
		return 0
	}
	// silent checks that the SFU sends no audio of a share for 400 ms.
	silent := func(share sfu.ShareID) {
		t.Helper()
		time.Sleep(50 * time.Millisecond)
		before := audioSent(share)
		time.Sleep(400 * time.Millisecond)
		if n := audioSent(share) - before; n != 0 {
			t.Errorf("the SFU sent %d audio packets of %s while the viewer doesn't want its audio", n, share)
		}
	}

	// ---- the viewer focuses A: A's audio, and only A's ----
	subscribe(t, viewConn, focus(shareA, true), focus(shareB, false))
	videoA, videoB := streamTrack(t, viewer, shareA, webrtc.RTPCodecTypeVideo), streamTrack(t, viewer, shareB, webrtc.RTPCodecTypeVideo)
	audioA := streamTrack(t, viewer, shareA, webrtc.RTPCodecTypeAudio)
	waitFrames(t, videoA, 0, "f", 30)
	waitFrames(t, videoB, 0, "q", 15)
	if err := audioA.Wait(ctx, func(r *sfutest.Recorder) bool { return r.Stats().Packets >= 50 }); err != nil {
		t.Fatal(err)
	}
	waitSubState(t, viewSig, focused(shareA, true))
	waitSubState(t, viewSig, focused(shareB, false))
	if hasTrack(viewer, shareB, webrtc.RTPCodecTypeAudio) || audioSent(shareB) != 0 {
		t.Errorf("the viewer got audio of %s (%d packets sent), which it didn't ask for", shareB, audioSent(shareB))
	}
	silent(shareB)

	// ---- the focus moves to B, in one call: A's audio stops, B's starts ----
	subscribe(t, viewConn, focus(shareA, false), focus(shareB, true))
	waitSubState(t, viewSig, focused(shareA, false))
	waitSubState(t, viewSig, focused(shareB, true))
	audioB := streamTrack(t, viewer, shareB, webrtc.RTPCodecTypeAudio)
	if err := audioB.Wait(ctx, func(r *sfutest.Recorder) bool { return r.Stats().Packets >= 50 }); err != nil {
		t.Fatal(err)
	}
	silent(shareA)

	// ---- and back; then A's audio alone goes off and on again, as a speaker button does ----
	subscribe(t, viewConn, focus(shareA, true), focus(shareB, false))
	waitSubState(t, viewSig, focused(shareA, true))
	waitSubState(t, viewSig, focused(shareB, false))
	resumed := audioA.Stats().Packets
	if err := audioA.Wait(ctx, func(r *sfutest.Recorder) bool { return r.Stats().Packets >= resumed+50 }); err != nil {
		t.Fatalf("the audio of %s did not resume: %v", shareA, err)
	}
	silent(shareB)
	muted := sfu.SubscriptionUpdate{Share: shareA, Video: sfu.QualityHigh}
	subscribe(t, viewConn, muted)
	waitSubState(t, viewSig, sfu.SubscriptionStateEvent{Share: shareA, Requested: sfu.QualityHigh, Forwarded: sfu.QualityHigh})
	silent(shareA)
	subscribe(t, viewConn, focus(shareA, true))
	waitSubState(t, viewSig, focused(shareA, true))
	resumed = audioA.Stats().Packets
	if err := audioA.Wait(ctx, func(r *sfutest.Recorder) bool { return r.Stats().Packets >= resumed+25 }); err != nil {
		t.Fatalf("the audio of %s did not resume: %v", shareA, err)
	}

	// ---- what the viewer got ----
	// A's audio: one stream, paused twice. Its sequence numbers are continuous across the pauses.
	ap := audioA.Packets()
	if err := sfutest.CheckContinuous(ap); err != nil {
		t.Errorf("audio of %s: %v", shareA, err)
	}
	if pauses := checkAudioRuns(t, ap); pauses != 2 {
		t.Errorf("the audio of %s has %d pauses, want 2", shareA, pauses)
	}
	// B's audio: one run, whole.
	bp := audioB.Packets()
	if err := sfutest.CheckContinuous(bp); err != nil {
		t.Errorf("audio of %s: %v", shareB, err)
	}
	if err := sfutest.CheckAudioMarkers(bp); err != nil {
		t.Errorf("audio of %s: %v", shareB, err)
	}
	for share, r := range map[sfu.ShareID]*sfutest.Recorder{shareA: audioA, shareB: audioB} {
		if st := r.Stats(); st.Gaps != 0 || st.Duplicates != 0 || r.TrackID() != "a-"+string(share) {
			t.Errorf("audio of %s: stats %+v, track id %q", share, st, r.TrackID())
		}
	}
	// The video followed the focus too, each stream continuous through its switches.
	for share, tc := range map[sfu.ShareID]struct {
		r    *sfutest.Recorder
		runs string
	}{shareA: {videoA, "f q f"}, shareB: {videoB, "q f q"}} {
		vp := checkVideo(t, "video of "+string(share), tc.r.Packets())
		if runs := layerRuns(vp); runs != tc.runs {
			t.Errorf("video of %s: layers %q, want %q", share, runs, tc.runs)
		}
		checkSwitchTimestamps(t, vp)
	}
	// Toggling works without renegotiation: the one offer that created the two subscriptions is all there was, and
	// the viewer has the four tracks it got then.
	if n := len(viewSig.Offers()); n != 1 {
		t.Errorf("%d sub offers, want 1", n)
	}
	if n := len(viewer.Recorders()); n != 4 {
		t.Errorf("the viewer has %d tracks, want two video and two audio", n)
	}
	// The client heard of every change of the audio, and never that B's audio flowed while A was focused.
	for share, want := range map[sfu.ShareID][]bool{shareA: {true, false, true, false, true}, shareB: {false, true, false}} {
		var got []bool
		for _, ev := range subEvents(viewSig, share) {
			if ev.Reason != "" {
				t.Errorf("event %+v has a reason", ev)
			}
			if len(got) == 0 || got[len(got)-1] != ev.Audio {
				got = append(got, ev.Audio)
			}
		}
		if len(got) > 0 && !got[0] && want[0] {
			got = got[1:] // the video started before the audio
		}
		if !slices.Equal(got, want) {
			t.Errorf("the client heard the audio of %s go %v, want %v (events %+v)", share, got, want, subEvents(viewSig, share))
		}
	}
}

// TestSimulcastFromTheFirstPacket: viewers who are subscribed and connected before a simulcast publisher sends
// anything get each layer from its very first packet, whichever of the publisher's tracks the SFU learns of first:
// the layer choice is made again when a layer arrives, before that track's first packet is read (02 §10.1). So the
// publisher is never asked for a keyframe, and makes none but each layer's first. The viewer who asked for high may
// get the preview layer for a moment, while it is the only one there; the thumbnail viewer never gets the full layer.
func TestSimulcastFromTheFirstPacket(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx := testCtx(t)
	const share = sfu.ShareID("s_start")
	pubConn, _ := join(t, h, "alice", "c-pub", sfu.RoleFull)
	startShare(t, pubConn, share, sfu.PresetAuto)
	pub := newPublisher(t, newLongGOPSource(t), sfutest.LoopbackSettings())
	if _, err := sfutest.Publish(ctx, pubConn, pub, 1, 1, share); err != nil {
		t.Fatalf("publish negotiation: %v", err)
	}
	type client struct {
		conn   *sfu.Conn
		sig    *sfutest.DirectSignaler
		viewer *sfutest.Viewer
	}
	connect := func(user sfu.UserID, u sfu.SubscriptionUpdate) client {
		t.Helper()
		conn, sig := join(t, h, user, sfu.ConnID("c-"+user), sfu.RoleViewer)
		v := newViewer(t, sfutest.LoopbackSettings())
		sig.Attach(conn, v)
		subscribe(t, conn, u)
		if err := sig.WaitPCState(ctx, sfu.PCSub, 1, "connected"); err != nil {
			t.Fatalf("%s's sub PC: %v (answer errors %v)", user, err, sig.Errs())
		}
		eventually(t, func() bool {
			subs, err := conn.Subscriptions(ctx)
			return err == nil && len(subs) == 1 && subs[0].VideoBound && subs[0].AudioBound &&
				v.PC().ConnectionState() == webrtc.PeerConnectionStateConnected
		}, func() string { return string(user) + "'s DownTracks aren't bound" })
		return client{conn, sig, v}
	}
	focused := connect("bob", sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityHigh, Audio: true})
	thumb := connect("carol", sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityLow})
	// The share has no track yet: both were told that nothing can serve them.
	waitSubState(t, focused.sig, sfu.SubscriptionStateEvent{Share: share, Requested: sfu.QualityHigh, Reason: sfu.SubReasonNoLayer})
	waitSubState(t, thumb.sig, sfu.SubscriptionStateEvent{Share: share, Requested: sfu.QualityLow, Reason: sfu.SubReasonNoPreviewLayer})

	if err := pub.Start(ctx); err != nil {
		t.Fatal(err)
	}
	full, small := streamTrack(t, focused.viewer, share, webrtc.RTPCodecTypeVideo), streamTrack(t, thumb.viewer, share, webrtc.RTPCodecTypeVideo)
	audio := streamTrack(t, focused.viewer, share, webrtc.RTPCodecTypeAudio)
	waitFrames(t, full, 0, "f", 30)
	waitFrames(t, small, 0, "q", 15)
	if err := audio.Wait(ctx, func(r *sfutest.Recorder) bool { return r.Stats().Packets >= 50 }); err != nil {
		t.Fatal(err)
	}
	waitSubState(t, focused.sig, sfu.SubscriptionStateEvent{Share: share, Requested: sfu.QualityHigh, Forwarded: sfu.QualityHigh, Audio: true})
	waitSubState(t, thumb.sig, sfu.SubscriptionStateEvent{Share: share, Requested: sfu.QualityLow, Forwarded: sfu.QualityLow})

	// Nobody asked the publisher for anything: each layer made its first keyframe and no other.
	for _, layer := range []string{"f", "q"} {
		if st := layerStats(t, pub, layer); st.Keyframes != 1 || st.PLIs != 0 {
			t.Errorf("layer %s: %d keyframes, %d PLIs; want its first keyframe and no request", layer, st.Keyframes, st.PLIs)
		}
	}
	// The focused viewer: the full layer from its frame 0, after at most a moment of the preview layer from its
	// frame 0.
	fp := checkVideo(t, "the focused viewer's video", full.Packets())
	runs := layerRuns(fp)
	if runs != "f" && runs != "q f" {
		t.Errorf("the focused viewer got the layers %q, want f, or q until f arrived", runs)
	}
	seen := map[string]bool{}
	for _, f := range sfutest.Frames(sfutest.WithoutCutFrames(fp)) {
		if !f.HasMark || seen[f.Mark.RID] {
			continue
		}
		seen[f.Mark.RID] = true
		if f.Mark.Frame != 0 || !f.Mark.Keyframe {
			t.Errorf("the focused viewer's first frame of layer %s is %+v, want the publisher's frame 0", f.Mark.RID, f.Mark)
		}
	}
	// The thumbnail viewer: the preview layer from its frame 0, and nothing else, not even audio.
	sp := checkVideo(t, "the thumbnail viewer's video", small.Packets())
	if first := sfutest.Frames(sp)[0]; layerRuns(sp) != "q" || first.Mark.Frame != 0 || !first.Mark.Keyframe {
		t.Errorf("the thumbnail viewer got the layers %q, first frame %+v; want q from the publisher's frame 0", layerRuns(sp), first.Mark)
	}
	if n := len(thumb.viewer.Recorders()); n != 1 {
		t.Errorf("the thumbnail viewer has %d tracks, want only video", n)
	}
	ap := audio.Packets()
	if err := sfutest.CheckAudioMarkers(ap); err != nil || !ap[0].HasMark || ap[0].Mark.Frame != 0 {
		t.Errorf("the focused viewer's audio: %v, first packet %+v; want it whole from the publisher's packet 0", err, ap[0].Mark)
	}
	t.Logf("the focused viewer got the layers %q", runs)
}

// sectionsOf describes the m-sections of a sub offer: "0 video sendonly s_1", "1 audio inactive".
func sectionsOf(t *testing.T, raw string) []string {
	t.Helper()
	var out []string
	for _, md := range parse(t, raw).MediaDescriptions {
		mid, _ := md.Attribute(sdp.AttrKeyMID)
		line := mid + " " + md.MediaName.Media + " " + directionOf(md)
		if stream, _, ok := strings.Cut(attr(md, "msid", ""), " "); ok {
			line += " " + stream
		}
		out = append(out, line)
	}
	return out
}

// TestConnCloseEndsShares is integration 10 of 02 §17: when a publisher's Conn closes, its share ends with the
// reason Close was given, and the viewers' m-sections for it go inactive. The next share takes those m-sections
// over (02 §9.3): over five shares in a row, each from a new Conn, the viewer's sub PC never has more than the two
// m-sections of its first subscription, and every share's media reaches the viewer through them.
func TestConnCloseEndsShares(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx := testCtx(t)
	viewConn, viewSig := join(t, h, "bob", "c-view", sfu.RoleViewer)
	viewer := newViewer(t, sfutest.LoopbackSettings())
	viewSig.Attach(viewConn, viewer)
	offerNeg := func(neg uint32) sfutest.Offer {
		t.Helper()
		o, err := viewSig.WaitOffer(ctx, func(o sfutest.Offer) bool { return o.Neg == neg })
		if err != nil {
			t.Fatalf("%v (answer errors %v)", err, viewSig.Errs())
		}
		if o.Gen != 1 {
			t.Fatalf("sub offer neg %d has gen %d: the sub PC was replaced", neg, o.Gen)
		}
		return o
	}

	const cycles = 5
	for i := range cycles {
		share := sfu.ShareID(fmt.Sprintf("s_cycle%d", i))
		pubConn, pub := publishShare(t, h, "alice", sfu.ConnID(fmt.Sprintf("c-pub%d", i)), share, uint64(i))
		waitLive(t, h, share)
		subscribe(t, viewConn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityHigh, Audio: true})

		// The share's offer: the same two m-sections every time, sendonly under the share's msid.
		up := offerNeg(uint32(2*i + 1))
		wantUp := []string{"0 video sendonly " + string(share), "1 audio sendonly " + string(share)}
		if got := sectionsOf(t, up.SDP); !slices.Equal(got, wantUp) {
			t.Fatalf("share %d: the sub offer has the m-sections %v, want %v: the m-sections of the share before are "+
				"taken over, not added to", i, got, wantUp)
		}
		wantTracks := []sfu.TrackBinding{
			{MID: "0", Share: share, Kind: webrtc.RTPCodecTypeVideo}, {MID: "1", Share: share, Kind: webrtc.RTPCodecTypeAudio},
		}
		if !slices.Equal(up.Tracks, wantTracks) {
			t.Errorf("share %d: the sub offer binds %+v, want %+v", i, up.Tracks, wantTracks)
		}
		// Its media arrives through them: a new track for the viewer each time, from its first keyframe on.
		video, audio := streamTrack(t, viewer, share, webrtc.RTPCodecTypeVideo), streamTrack(t, viewer, share, webrtc.RTPCodecTypeAudio)
		waitFrames(t, video, 0, "f", 15)
		if err := audio.Wait(ctx, func(r *sfutest.Recorder) bool { return r.Stats().Packets >= 25 }); err != nil {
			t.Fatalf("share %d: %v", i, err)
		}
		if video.MID() != "0" || audio.MID() != "1" {
			t.Errorf("share %d arrived on the m-sections %s and %s, want 0 and 1", i, video.MID(), audio.MID())
		}
		waitSubState(t, viewSig, sfu.SubscriptionStateEvent{Share: share, Requested: sfu.QualityHigh, Forwarded: sfu.QualityHigh, Audio: true})

		// The publisher's connection is gone (the hub closes the Conn after its grace period).
		pubConn.Close(sfu.EndReasonDisconnected)
		ended, err := h.Events.Wait(ctx, func(ev sfutest.RoomEvent) bool { return ev.Kind == sfutest.ShareEnded && ev.Share.ID == share })
		if err != nil {
			t.Fatal(err)
		}
		if ended.Reason != sfu.EndReasonDisconnected || ended.Share.State != sfu.ShareLive || ended.Share.Conn != pubConn.ID() {
			t.Errorf("share %d: ShareEnded = %+v, want the reason the Conn closed with", i, ended)
		}
		if _, ok := h.SFU.Share(share); ok {
			t.Errorf("share %d is still listed after its Conn closed", i)
		}
		// The viewer's m-sections go inactive, with nothing bound; the subscription is gone.
		down := offerNeg(uint32(2*i + 2))
		if got := sectionsOf(t, down.SDP); !slices.Equal(got, []string{"0 video inactive", "1 audio inactive"}) || len(down.Tracks) != 0 {
			t.Fatalf("share %d ended: the sub offer has the m-sections %v and binds %+v, want both inactive", i, got, down.Tracks)
		}
		if err := viewSig.WaitAnswered(ctx, down); err != nil {
			t.Fatal(err)
		}
		if subs, err := viewConn.Subscriptions(ctx); err != nil || len(subs) != 0 {
			t.Errorf("share %d ended: subscriptions = %+v, %v", i, subs, err)
		}
		select {
		case <-pubConn.Done():
		case <-ctx.Done():
			t.Fatalf("share %d: the publisher's Conn did not finish closing", i)
		}
		if err := pub.Close(); err != nil {
			t.Errorf("Publisher.Close: %v", err)
		}
		// What the viewer got of the share: a stream that started on an SPS and had no gap until it ended.
		checkVideo(t, fmt.Sprintf("share %d: video", i), video.Packets())
		if err := sfutest.CheckContinuous(audio.Packets()); err != nil {
			t.Errorf("share %d: audio: %v", i, err)
		}
	}

	// One sub PC all along, connected once; two offers per share, each answered; ten tracks on two m-sections.
	if n := len(viewSig.Offers()); n != 2*cycles {
		t.Errorf("%d sub offers for %d shares, want two each", n, cycles)
	}
	if errs := viewSig.Errs(); len(errs) != 0 {
		t.Errorf("answering the sub offers: %v", errs)
	}
	var states []string
	for _, ev := range viewSig.Events() {
		if st, ok := ev.(sfu.PCStateEvent); ok {
			states = append(states, st.State)
		}
	}
	if !slices.Equal(states, []string{"connected"}) || viewer.PC().ConnectionState() != webrtc.PeerConnectionStateConnected {
		t.Errorf("the viewer's sub PC went through %v and is %s, want connected once and still", states, viewer.PC().ConnectionState())
	}
	if n := len(viewer.Recorders()); n != 2*cycles {
		t.Errorf("the viewer got %d tracks, want a video and an audio track per share", n)
	}
	if n := len(viewer.PC().GetTransceivers()); n != 2 {
		t.Errorf("the viewer's PeerConnection has %d transceivers, want 2", n)
	}
}

// TestSimulcastReoffer: a publisher that sends simulcast offers again on the same pub PC (the next neg of its gen),
// as a client does to add a share, to change its codec order or to restart ICE. The SFU answers as before, and
// nothing changes for the tracks that flow: no layer is attached anew, no keyframe is asked for, the viewer's stream
// has no gap.
func TestSimulcastReoffer(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx := testCtx(t)
	const share = sfu.ShareID("s_reoffer")
	pubConn, pub := publishShare(t, h, "alice", "c-pub", share, 3)
	waitLive(t, h, share)
	viewConn, viewSig := join(t, h, "bob", "c-view", sfu.RoleViewer)
	viewer := newViewer(t, sfutest.LoopbackSettings())
	viewSig.Attach(viewConn, viewer)
	subscribe(t, viewConn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityHigh, Audio: true})
	video := streamTrack(t, viewer, share, webrtc.RTPCodecTypeVideo)
	waitFrames(t, video, 0, "f", 30)
	before := pub.Stats()

	for neg := uint32(2); neg <= 3; neg++ {
		answer, err := sfutest.Publish(ctx, pubConn, pub, 1, neg, share)
		if err != nil {
			t.Fatalf("re-offer neg %d: %v", neg, err)
		}
		bindings := sfutest.Bindings(pub, share)
		if len(bindings) != 2 {
			t.Fatalf("the publisher's bindings = %+v", bindings)
		}
		if sim := attr(section(t, parse(t, answer), bindings[0].MID), "simulcast", ""); sim != "recv f;q" {
			t.Errorf("re-offer neg %d: answer a=simulcast:%s, want recv f;q", neg, sim)
		}
	}
	// A repeated neg gets the stored answer; an older one is stale.
	offer, err := pub.Offer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pubConn.HandleOffer(ctx, sfu.PCPub, 1, 3, offer.SDP, sfutest.Bindings(pub, share)); err != nil {
		t.Errorf("a repeated neg: %v", err)
	}
	_, err = pubConn.HandleOffer(ctx, sfu.PCPub, 1, 2, offer.SDP, sfutest.Bindings(pub, share))
	wantCode(t, err, sfu.CodeStaleOffer)
	// The local description the Publisher keeps would not have passed: it lists the rids twice.
	_, err = pubConn.HandleOffer(ctx, sfu.PCPub, 1, 4, pub.PC().LocalDescription().SDP, sfutest.Bindings(pub, share))
	wantCode(t, err, sfu.CodeBadRID)

	mark := len(video.Packets())
	waitFrames(t, video, mark, "f", 30)
	tracks, err := pubConn.PubTracks(ctx)
	if err != nil || len(tracks) != 3 {
		t.Fatalf("pub tracks after the re-offers = %+v, %v", tracks, err)
	}
	for _, tr := range tracks {
		if tr.Share != share {
			t.Errorf("pub track %+v feeds %q after the re-offers", tr, tr.Share)
		}
	}
	after := pub.Stats()
	for i, tr := range after.Tracks {
		if tr.Keyframes != before.Tracks[i].Keyframes || tr.PLIs != before.Tracks[i].PLIs || tr.WriteErrors != 0 {
			t.Errorf("track %s/%q: %d keyframes and %d PLIs after the re-offers, %d and %d before; %d write errors",
				tr.Kind, tr.RID, tr.Keyframes, tr.PLIs, before.Tracks[i].Keyframes, before.Tracks[i].PLIs, tr.WriteErrors)
		}
	}
	if runs := layerRuns(checkVideo(t, "video through the re-offers", video.Packets())); runs != "f" {
		t.Errorf("the viewer got the layers %q, want only f", runs)
	}
	if info, _ := h.SFU.Share(share); info.State != sfu.ShareLive || len(info.Layers) != 2 || !info.Audio {
		t.Errorf("the share after the re-offers = %+v", info)
	}
}

// heapInUse returns the bytes of live heap objects after a collection.
func heapInUse() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// TestTenByTen is integration 17 of 02 §17 (the S4 port; skipped with -short): ten publishers, each with a simulcast
// share with audio, and ten viewers who each focus another share (high, with audio) and watch the other nine as
// thumbnails (low, without). All hundred subscriptions get their layer within 5 s; every stream is continuous, starts
// on an SPS and carries only the layer asked for; audio flows only for the focused shares; a publisher gets at most
// one PLI per 500 ms and layer however many viewers arrive; and a DownTrack costs well under 1 MB of heap.
func TestTenByTen(t *testing.T) {
	if testing.Short() {
		t.Skip("10×10 runs outside -short")
	}
	const n = 10
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	shares := make([]sfu.ShareID, n)
	pubs := make([]*publish.Publisher, n)
	for i := range n {
		shares[i] = sfu.ShareID(fmt.Sprintf("s_ten%d", i))
		_, pubs[i] = publishShare(t, h, sfu.UserID(fmt.Sprintf("pub%d", i)), sfu.ConnID(fmt.Sprintf("c-pub%d", i)), shares[i], uint64(100+i))
	}
	for _, share := range shares {
		waitLive(t, h, share)
	}
	type client struct {
		conn   *sfu.Conn
		sig    *sfutest.DirectSignaler
		viewer *sfutest.Viewer
	}
	viewers := make([]client, n)
	for j := range viewers {
		conn, sig := join(t, h, sfu.UserID(fmt.Sprintf("view%d", j)), sfu.ConnID(fmt.Sprintf("c-view%d", j)), sfu.RoleViewer)
		v, err := sfutest.NewViewer(sfutest.ViewerOptions{Settings: sfutest.LoopbackSettings(), KeepPackets: 1 << 13})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = v.Close() })
		sig.Attach(conn, v)
		viewers[j] = client{conn, sig, v}
	}
	wanted := func(j, i int) (sfu.SubscriptionUpdate, string) {
		if i == j {
			return sfu.SubscriptionUpdate{Share: shares[i], Video: sfu.QualityHigh, Audio: true}, "f"
		}
		return sfu.SubscriptionUpdate{Share: shares[i], Video: sfu.QualityLow}, "q"
	}
	// The publishers have run for a moment: what they got asked so far is behind them.
	pliBefore := make([]publish.Stats, n)
	for i, pub := range pubs {
		pliBefore[i] = pub.Stats()
	}
	heapBefore := heapInUse()

	// ---- every viewer subscribes to every share, in one call each ----
	start := time.Now()
	for j, c := range viewers {
		items := make([]sfu.SubscriptionUpdate, n)
		for i := range n {
			items[i], _ = wanted(j, i)
		}
		errs, err := c.conn.UpdateSubscriptions(ctx, items)
		if err != nil || slices.ContainsFunc(errs, func(e error) bool { return e != nil }) {
			t.Fatalf("viewer %d: UpdateSubscriptions = %v, %v", j, errs, err)
		}
	}
	// ---- all hundred get their layer ----
	const frames = 10
	recs := make([][]*sfutest.Recorder, n)
	var slowest time.Duration
	for j, c := range viewers {
		recs[j] = make([]*sfutest.Recorder, n)
		for i := range n {
			_, rid := wanted(j, i)
			r, err := c.viewer.WaitRecorder(ctx, func(r *sfutest.Recorder) bool {
				return r.Kind() == webrtc.RTPCodecTypeVideo && r.StreamID() == string(shares[i])
			})
			if err != nil {
				t.Fatalf("viewer %d got no video of share %d: %v (answer errors %v)", j, i, err, c.sig.Errs())
			}
			if err := r.Wait(ctx, func(r *sfutest.Recorder) bool { return layerFrames(r.Packets(), rid) >= frames }); err != nil {
				t.Fatalf("viewer %d, share %d: %d frames of layer %s: %v", j, i, frames, rid, err)
			}
			recs[j][i] = r
			slowest = max(slowest, r.Stats().First.Sub(start))
		}
	}
	if slowest > 5*time.Second {
		t.Errorf("the last of the %d subscriptions got its first packet %v after the viewers subscribed, want within 5 s", n*n, slowest)
	}
	// Let the streams run, then look at all of them.
	time.Sleep(time.Second)
	heapAfter := heapInUse()
	elapsed := time.Since(start)

	for j, c := range viewers {
		subs, err := c.conn.Subscriptions(ctx)
		if err != nil || len(subs) != n {
			t.Fatalf("viewer %d: subscriptions = %+v, %v", j, subs, err)
		}
		for i := range n {
			u, rid := wanted(j, i)
			what := fmt.Sprintf("viewer %d, share %d", j, i)
			vp := checkVideo(t, what+": video", recs[j][i].Packets())
			if runs := layerRuns(vp); runs != rid {
				t.Errorf("%s: the viewer got the layers %q, want only %s", what, runs, rid)
			}
			if st := recs[j][i].Stats(); st.Gaps != 0 || st.Duplicates != 0 {
				t.Errorf("%s: video stats on loopback = %+v", what, st)
			}
			// Audio only on the focused share.
			st := subs[i] // sorted by share id, as shares is
			if st.Share != shares[i] || st.Forwarded != u.Video || st.AudioForwarded != u.Audio || st.Layer != rid ||
				st.Reason != "" || st.VideoDrops != 0 || st.AudioDrops != 0 || (st.AudioSent > 0) != u.Audio {
				t.Errorf("%s: subscription = %+v, want layer %s, audio %v, nothing dropped", what, st, rid, u.Audio)
			}
			if got := hasTrack(c.viewer, shares[i], webrtc.RTPCodecTypeAudio); got != u.Audio {
				t.Errorf("%s: the viewer has audio: %v, want %v", what, got, u.Audio)
			}
			if u.Audio {
				audio := streamTrack(t, c.viewer, shares[i], webrtc.RTPCodecTypeAudio)
				if err := audio.Wait(ctx, func(r *sfutest.Recorder) bool { return r.Stats().Packets >= 25 }); err != nil {
					t.Errorf("%s: audio: %v", what, err)
					continue
				}
				ap := audio.Packets()
				if err := sfutest.CheckContinuous(ap); err != nil {
					t.Errorf("%s: audio: %v", what, err)
				}
				if err := sfutest.CheckAudioMarkers(ap); err != nil {
					t.Errorf("%s: audio: %v", what, err)
				}
			}
			// The client heard what it gets: all it asked for.
			evs := subEvents(c.sig, shares[i])
			want := sfu.SubscriptionStateEvent{Share: shares[i], Requested: u.Video, Forwarded: u.Video, Audio: u.Audio}
			if len(evs) == 0 || evs[len(evs)-1] != want {
				t.Errorf("%s: the client's events are %+v, want the last to be %+v", what, evs, want)
			}
		}
		// One debounced offer for the whole batch, answered.
		if offers, errs := c.sig.Offers(), c.sig.Errs(); len(offers) != 1 || len(offers[0].Tracks) != 2*n || len(errs) != 0 {
			t.Errorf("viewer %d: %d sub offers (answer errors %v), want one that binds %d m-sections", j, len(offers), errs, 2*n)
		}
	}

	// ---- what it cost ----
	// The PLI throttle (02 §9.7): nine viewers arrived for each preview layer, one for each full layer, and a layer
	// got at most one PLI per 500 ms. Each of them needed at least one.
	most := 1 + int(elapsed/(500*time.Millisecond))
	var plis, keyframes int
	for i, pub := range pubs {
		for k, tr := range pub.Stats().Tracks {
			if tr.Kind != webrtc.RTPCodecTypeVideo {
				continue
			}
			got := tr.PLIs - pliBefore[i].Tracks[k].PLIs
			plis += got
			keyframes += tr.Keyframes - pliBefore[i].Tracks[k].Keyframes
			if got < 1 || got > most {
				t.Errorf("publisher %d, layer %s: %d PLIs in %v, want at least 1 and at most 1 per 500 ms (%d)", i, tr.RID, got,
					elapsed.Round(time.Millisecond), most)
			}
		}
	}
	// Heap per DownTrack: the SFU's (queue, munger, sender) and, in this process, the viewer's side of it too.
	downTracks := 2 * n * n
	var perDownTrack uint64
	if heapAfter > heapBefore {
		perDownTrack = (heapAfter - heapBefore) / uint64(downTracks)
	}
	if perDownTrack > 1<<20 {
		t.Errorf("the heap grew by %d bytes per DownTrack, want less than 1 MB", perDownTrack)
	}
	for i, share := range shares {
		info, ok := h.SFU.Share(share)
		if !ok || info.State != sfu.ShareLive || len(info.Viewers) != n {
			t.Errorf("share %d = %+v, want live with %d viewers", i, info, n)
		}
		if v, a, _ := h.SFU.FanOut(share); v != n || a != n {
			t.Errorf("share %d fans out to %d video and %d audio DownTracks, want %d each", i, v, a, n)
		}
	}
	t.Logf("%d×%d: every subscription had its first packet after %v; %d PLIs and %d keyframes for %d layers in %v; heap +%d KiB "+
		"per DownTrack (%d DownTracks)", n, n, slowest.Round(time.Millisecond), plis, keyframes, 2*n, elapsed.Round(time.Millisecond),
		perDownTrack>>10, downTracks)
}
