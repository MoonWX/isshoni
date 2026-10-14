package sfu_test

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/server/sfu"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// The acceptance tests of publisher-side control (README S88, 02 §11), through sfutest.Harness with real
// PeerConnections and fake media: the admin's cap as REMB on the pub PC every second and as quality hints, layer
// pausing with its resume, and sfu.pause_unwatched_layers = false. The unit tests are uplink_test.go's.

// hintsOf returns the QualityHintEvents a Conn's client got, in order.
func hintsOf(sig *sfutest.DirectSignaler) []sfu.QualityHintEvent {
	var out []sfu.QualityHintEvent
	for _, ev := range sig.Events() {
		if h, ok := ev.(sfu.QualityHintEvent); ok {
			out = append(out, h)
		}
	}
	return out
}

// nthHint waits until the client has got n QualityHintEvents, and returns the nth.
func nthHint(t *testing.T, sig *sfutest.DirectSignaler, n int) sfu.QualityHintEvent {
	t.Helper()
	eventually(t, func() bool { return len(hintsOf(sig)) >= n },
		func() string { return fmt.Sprintf("hints %+v, want %d", hintsOf(sig), n) })
	return hintsOf(sig)[n-1]
}

// wantHint fails unless h says reason, the cap and, for f and q, whether each is active, with the encodings of the
// Auto preset otherwise.
func wantHint(t *testing.T, h sfu.QualityHintEvent, share sfu.ShareID, reason string, capBps int, f, q bool) {
	t.Helper()
	full := 8_000_000
	if capBps > 0 && capBps < full {
		full = capBps
	}
	want := sfu.QualityHintEvent{Share: share, Reason: reason, MaxBitrate: capBps, Encodings: []sfu.EncodingParams{
		{RID: "f", Active: f, MaxBitrate: full, MaxFramerate: 60, MaxPixels: 1920 * 1080},
		{RID: "q", Active: q, MaxBitrate: 300_000, MaxFramerate: 15, MaxPixels: 640 * 360},
	}}
	if !reflect.DeepEqual(h, want) {
		t.Errorf("hint = %+v, want %+v", h, want)
	}
}

// publishWith starts a share of user on a new Conn and publishes fake media with a long GOP on it (simulcast f and q,
// with audio), and returns the Conn's signaler too: the publisher's quality hints arrive there.
func publishWith(t *testing.T, h *sfutest.Harness, user sfu.UserID, conn sfu.ConnID, share sfu.ShareID,
) (*sfu.Conn, *sfutest.DirectSignaler, *publish.Publisher) {
	t.Helper()
	ctx := testCtx(t)
	c, sig := join(t, h, user, conn, sfu.RoleFull)
	startShare(t, c, share, sfu.PresetAuto)
	pub := newPublisher(t, newLongGOPSource(t), sfutest.LoopbackSettings())
	if _, err := sfutest.Publish(ctx, c, pub, 1, 1, share); err != nil {
		t.Fatalf("publish negotiation of %s: %v", share, err)
	}
	if err := pub.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitLive(t, h, share)
	return c, sig, pub
}

// TestAdminCapREMB: with Limits.MaxShareKbps set, the pub PC gets a REMB of the cap every second, naming its video
// layers, and the share hears nothing more than its ShareParams said. A new cap reaches the sharer at once, as a hint
// with the reason admin and as the next REMB; when the cap goes, the hint says so and the pub PC gets three REMBs of
// no limit, then none.
func TestAdminCapREMB(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{Limits: sfu.Limits{MaxShareKbps: 2500}})
	const share = sfu.ShareID("s_cap")
	_, sig, pub := publishWith(t, h, "alice", "c-pub", share)
	f := func() publish.TrackStats { return layerStats(t, pub, "f") }
	q := func() publish.TrackStats { return layerStats(t, pub, "q") }

	eventually(t, func() bool { return f().REMBs >= 1 && q().REMBs >= 1 },
		func() string { return fmt.Sprintf("the publisher's stats are %+v", pub.Stats()) })
	if f().LastREMB != 2_500_000 || q().LastREMB != 2_500_000 {
		t.Errorf("the last REMBs say %d (f) and %d (q) bit/s, want the cap", f().LastREMB, q().LastREMB)
	}
	// Every second: three more REMBs take three seconds, within the ticker's quarter second either way.
	n0 := f().REMBs
	eventually(t, func() bool { return f().REMBs > n0 }, func() string { return "no REMB" })
	from, n1 := time.Now(), f().REMBs
	eventually(t, func() bool { return f().REMBs >= n1+3 }, func() string { return "no three more REMBs" })
	if took := time.Since(from); took < 2500*time.Millisecond || took > 3750*time.Millisecond {
		t.Errorf("three REMBs took %v, want one a second", took)
	}
	if a := layerStats(t, pub, "").REMBs; a != 0 {
		t.Errorf("%d REMBs name the audio", a)
	}
	if n := len(hintsOf(sig)); n != 0 {
		t.Errorf("%d hints while the cap stayed what StartShare said", n)
	}

	// A higher cap: a hint at once (the ticker's next second isn't waited for), and REMBs of the new cap.
	asked := time.Now()
	h.SFU.SetLimits(sfu.Limits{MaxShareKbps: 4000})
	wantHint(t, nthHint(t, sig, 1), share, sfu.HintReasonAdmin, 4_000_000, true, true)
	if took := time.Since(asked); took > 900*time.Millisecond {
		t.Errorf("the hint took %v", took)
	}
	eventually(t, func() bool { return f().LastREMB == 4_000_000 },
		func() string { return fmt.Sprintf("the last REMB says %d", f().LastREMB) })

	// No cap: a hint, three REMBs of no limit, then none.
	before := f().REMBs
	h.SFU.SetLimits(sfu.Limits{})
	wantHint(t, nthHint(t, sig, 2), share, sfu.HintReasonAdmin, 0, true, true)
	eventually(t, func() bool { return f().REMBs >= before+sfu.REMBLifts },
		func() string { return fmt.Sprintf("%d REMBs after the cap went", f().REMBs-before) })
	if f().LastREMB != sfu.REMBUncapped {
		t.Errorf("the last REMB says %d bit/s, want %d: no limit", f().LastREMB, sfu.REMBUncapped)
	}
	time.Sleep(2500 * time.Millisecond)
	if n := f().REMBs - before; n != sfu.REMBLifts {
		t.Errorf("%d REMBs after the cap went, want %d", n, sfu.REMBLifts)
	}
}

// TestLayerPausing: nobody asks for high for 10 s, and the sharer is told to pause the full layer, as a browser
// would apply it (sfutest.ApplyHint); the share stays live with both layers listed. A WebSocket resume brings the
// hint again. When a viewer asks for high, the layer resumes at once, with a PLI, and the viewer's stream goes on in
// the full layer from its keyframe. Back on the preview layer, 10 s later the full layer is paused again.
func TestLayerPausing(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{PauseUnwatchedLayers: true})
	const share = sfu.ShareID("s_pause")
	pubConn, sig, pub := publishWith(t, h, "alice", "c-pub", share)
	viewConn, viewSig := join(t, h, "bob", "c-view", sfu.RoleViewer)
	viewer := newViewer(t, sfutest.LoopbackSettings())
	viewSig.Attach(viewConn, viewer)
	subscribe(t, viewConn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityLow, Audio: true})
	video := streamTrack(t, viewer, share, webrtc.RTPCodecTypeVideo)
	waitFrames(t, video, 0, "q", 5)
	if n := len(hintsOf(sig)); n != 0 {
		t.Fatalf("%d hints within a few seconds of the share's start", n)
	}

	// Ten seconds on the policies' clock: the ticker's next look pauses f.
	h.SFU.AdvanceUplinkClock(sfu.PauseAfter)
	asked := time.Now()
	paused := nthHint(t, sig, 1)
	wantHint(t, paused, share, sfu.HintReasonViewers, 0, false, true)
	if took := time.Since(asked); took > 1500*time.Millisecond {
		t.Errorf("the pause hint came %v after the 10 s were up; the ticker looks once a second", took)
	}
	if err := sfutest.ApplyHint(pub, paused); err != nil {
		t.Fatal(err)
	}
	// f stops; the share stays live, and f stays listed (not active after 2 s without a packet).
	info := waitShare(t, h.SFU, share, func(i sfu.ShareInfo) bool {
		return len(i.Layers) == 2 && i.Layers[0].RID == "f" && !i.Layers[0].Active && i.Layers[1].Active
	})
	if info.State != sfu.ShareLive {
		t.Errorf("the share with its full layer paused = %+v", info)
	}
	for _, ev := range h.Events.Events() {
		if ev.Kind == sfutest.ShareUpdated && ev.Share.ID == share && ev.Share.State != sfu.ShareLive {
			t.Errorf("pausing changed the share's state: %+v", ev.Share)
		}
	}
	sent := layerStats(t, pub, "f").Packets
	time.Sleep(300 * time.Millisecond)
	if st := layerStats(t, pub, "f"); st.Packets != sent || st.Paused == 0 {
		t.Errorf("the paused layer sent %d packets (%d held back)", st.Packets-sent, st.Paused)
	}
	// A resume of the WebSocket: the hint again.
	pubConn.Resync()
	wantHint(t, nthHint(t, sig, 2), share, sfu.HintReasonViewers, 0, false, true)

	// Bob focuses: f resumes at once, with a PLI, and his stream switches to f on its keyframe.
	plis := layerStats(t, pub, "f").PLIs
	mark := len(video.Packets())
	asked = time.Now()
	subscribe(t, viewConn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityHigh, Audio: true})
	resumed := nthHint(t, sig, 3)
	wantHint(t, resumed, share, sfu.HintReasonViewers, 0, true, true)
	if took := time.Since(asked); took > 900*time.Millisecond {
		t.Errorf("the resume hint took %v", took)
	}
	eventually(t, func() bool { return layerStats(t, pub, "f").PLIs > plis },
		func() string { return "no PLI for the resumed layer" })
	if err := sfutest.ApplyHint(pub, resumed); err != nil {
		t.Fatal(err)
	}
	waitFrames(t, video, mark, "f", 10)
	waitSubState(t, viewSig, sfu.SubscriptionStateEvent{
		Share: share, Requested: sfu.QualityHigh, Forwarded: sfu.QualityHigh, Audio: true,
	})
	t.Logf("from the request for high to the full layer's first frame: %v",
		switchTime(t, video.Packets()[mark:], "f", asked))
	checkVideo(t, "video", video.Packets())

	// Back to low: ten seconds later f is paused again.
	subscribe(t, viewConn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityLow, Audio: true})
	waitFrames(t, video, len(video.Packets()), "q", 3)
	h.SFU.AdvanceUplinkClock(sfu.PauseAfter)
	wantHint(t, nthHint(t, sig, 4), share, sfu.HintReasonViewers, 0, false, true)
}

// TestLayerPausingOff: with sfu.pause_unwatched_layers = false, a share that nobody watches in high for an hour gets
// no hint, and its full layer keeps flowing.
func TestLayerPausingOff(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{PauseUnwatchedLayers: false})
	const share = sfu.ShareID("s_on")
	_, sig, pub := publishWith(t, h, "alice", "c-pub", share)
	viewConn, viewSig := join(t, h, "bob", "c-view", sfu.RoleViewer)
	viewer := newViewer(t, sfutest.LoopbackSettings())
	viewSig.Attach(viewConn, viewer)
	subscribe(t, viewConn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityLow, Audio: true})
	waitFrames(t, streamTrack(t, viewer, share, webrtc.RTPCodecTypeVideo), 0, "q", 5)

	h.SFU.AdvanceUplinkClock(time.Hour)
	sent := layerStats(t, pub, "f").Packets
	time.Sleep(2500 * time.Millisecond) // two looks of the ticker at least
	if hints := hintsOf(sig); len(hints) != 0 {
		t.Errorf("hints with layer pausing off: %+v", hints)
	}
	if st := layerStats(t, pub, "f"); st.Packets <= sent {
		t.Errorf("the full layer stopped: %d packets before, %d after", sent, st.Packets)
	}
	if info, _ := h.SFU.Share(share); len(info.Layers) != 2 || !info.Layers[0].Active {
		t.Errorf("the share = %+v, want its full layer active", info)
	}
}
