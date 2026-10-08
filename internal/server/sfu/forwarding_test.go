package sfu_test

import (
	"fmt"
	"net"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"go.uber.org/goleak"

	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/server/sfu"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// The media path's acceptance test (README S41): integration 1 of 02 §17, through sfutest.Harness.

// newLongGOPSource returns fake media like newSource, but with a GOP far longer than a test: after each layer's
// first keyframe the only keyframes are the ones somebody asks for, so a test can tell what the SFU's PLIs did.
func newLongGOPSource(t *testing.T) *fake.Source {
	t.Helper()
	src, err := fake.New(fake.Config{
		Layers: []fake.VideoLayer{
			{RID: "f", Width: 640, Height: 360, FPS: 30, Bitrate: 400_000},
			{RID: "q", Width: 320, Height: 180, FPS: 15, Bitrate: 100_000},
		},
		GOP: time.Minute, Audio: true, Seed: 41,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	return src
}

// layerStats returns the Publisher's counters for a Source layer ("f", "q") or, with "", for its audio.
func layerStats(t *testing.T, pub *publish.Publisher, layer string) publish.TrackStats {
	t.Helper()
	for _, ts := range pub.Stats().Tracks {
		if ts.Layer == layer && (layer != "" || ts.Kind == webrtc.RTPCodecTypeAudio) {
			return ts
		}
	}
	t.Fatalf("the publisher has no track for layer %q", layer)
	return publish.TrackStats{}
}

// waitTrack waits for the Viewer's track of a kind.
func waitTrack(t *testing.T, v *sfutest.Viewer, kind webrtc.RTPCodecType) *sfutest.Recorder {
	t.Helper()
	r, err := v.WaitRecorder(testCtx(t), func(r *sfutest.Recorder) bool { return r.Kind() == kind })
	if err != nil {
		t.Fatalf("the viewer got no %s: %v", kind, err)
	}
	return r
}

// sfuGoroutines returns the stacks of the goroutines that run in package sfu or were started by it: the Conn
// actors, the track readers, the DownTrack writers and RTCP readers, and the ticker. The tests themselves are in
// package sfu_test and the harness in sfutest, so neither counts; the main goroutine, which runs package sfu's
// TestMain, is left out.
func sfuGoroutines() []string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	var out []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "/internal/server/sfu.") && !strings.Contains(g, "/internal/server/sfu.TestMain(") {
			out = append(out, g)
		}
	}
	return out
}

// TestForwardingFromTheFirstPacket: a viewer who is subscribed and connected before the publisher sends anything gets
// the stream from its very first packet: frame 0, the keyframe that also makes the share live, and audio packet 0.
// Nothing is lost between Pion delivering a track and the SFU reading it, and so nobody has to ask the publisher
// for a second keyframe.
func TestForwardingFromTheFirstPacket(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx := testCtx(t)
	pubConn, _ := join(t, h, "alice", "c-pub", sfu.RoleFull)
	viewConn, viewSig := join(t, h, "bob", "c-view", sfu.RoleViewer)
	const share = sfu.ShareID("s_first")
	startShare(t, pubConn, share, sfu.PresetAuto)
	pub := newPublisher(t, newLongGOPSource(t), sfutest.LoopbackSettings(), "f")
	if _, err := sfutest.Publish(ctx, pubConn, pub, 1, 1, share); err != nil {
		t.Fatalf("publish negotiation: %v", err)
	}

	// The viewer subscribes to the pending share and connects: its sub PC has nothing to carry yet.
	viewer := newViewer(t, sfutest.LoopbackSettings())
	viewSig.Attach(viewConn, viewer)
	errs, err := viewConn.UpdateSubscriptions(ctx, []sfu.SubscriptionUpdate{{Share: share, Video: sfu.QualityHigh, Audio: true}})
	if err != nil || errs[0] != nil {
		t.Fatalf("UpdateSubscriptions = %v, %v", errs, err)
	}
	if err := viewSig.WaitPCState(ctx, sfu.PCSub, 1, "connected"); err != nil {
		t.Fatalf("sub PC: %v (answer errors %v)", err, viewSig.Errs())
	}
	eventually(t, func() bool { return viewer.PC().ConnectionState() == webrtc.PeerConnectionStateConnected },
		func() string { return "the viewer's PC is " + viewer.PC().ConnectionState().String() })
	var subs []sfu.SubscriptionState
	eventually(t, func() bool {
		subs, err = viewConn.Subscriptions(ctx)
		return err == nil && len(subs) == 1 && subs[0].VideoBound && subs[0].AudioBound
	}, func() string { return fmt.Sprintf("subscriptions = %+v, %v", subs, err) })
	if info, _ := h.SFU.Share(share); info.State != sfu.SharePending || len(info.Viewers) != 0 {
		t.Errorf("the share before its media = %+v, want pending and unwatched", info)
	}

	if err := pub.Start(ctx); err != nil {
		t.Fatal(err)
	}
	video, audio := waitTrack(t, viewer, webrtc.RTPCodecTypeVideo), waitTrack(t, viewer, webrtc.RTPCodecTypeAudio)
	if err := video.Wait(ctx, func(r *sfutest.Recorder) bool { return len(sfutest.Frames(r.Packets())) >= 30 }); err != nil {
		t.Fatal(err)
	}
	if err := audio.Wait(ctx, func(r *sfutest.Recorder) bool { return r.Stats().Packets >= 50 }); err != nil {
		t.Fatal(err)
	}
	vp := sfutest.WithoutPartialTail(video.Packets())
	for _, check := range []func([]sfutest.Packet) error{sfutest.CheckStartsOnSPS, sfutest.CheckContinuous, sfutest.CheckVideoMarkers} {
		if err := check(vp); err != nil {
			t.Errorf("video: %v", err)
		}
	}
	if first := sfutest.Frames(vp)[0]; !first.HasMark || first.Mark.Frame != 0 || !first.Mark.Keyframe {
		t.Errorf("the viewer's first video frame is %+v, want the publisher's frame 0, its first keyframe", first.Mark)
	}
	ap := audio.Packets()
	if err := sfutest.CheckAudioMarkers(ap); err != nil {
		t.Errorf("audio: %v", err)
	}
	if !ap[0].HasMark || ap[0].Mark.Frame != 0 {
		t.Errorf("the viewer's first audio packet is %+v, want the publisher's packet 0", ap[0].Mark)
	}
	if st := layerStats(t, pub, "f"); st.Keyframes != 1 || st.PLIs != 0 {
		t.Errorf("the publisher made %d keyframes and got %d PLIs, want its first keyframe and no request", st.Keyframes, st.PLIs)
	}
	info := waitShare(t, h.SFU, share, func(i sfu.ShareInfo) bool {
		return i.State == sfu.ShareLive && slices.Equal(i.Viewers, []sfu.ParticipantID{"bob"})
	})
	if info.Profile != sfu.ProfileHigh || len(info.Layers) != 1 || !info.Audio {
		t.Errorf("ShareInfo = %+v", info)
	}
}

// TestBasicForwarding is integration 1 of 02 §17: a share goes pending → live with its first keyframe, a viewer who
// subscribes while the media already flows gets video that starts on an SPS (the DTLS-ready gate and the PLI on
// connect of S4 finding 1) and audio, both continuous and intact; and SFU.Close, with all of that running, returns
// within 1 s and leaves no goroutine behind. Once with a publisher that sends a single video layer, as the slice's
// name says, and once with simulcast, where each viewer still gets exactly one layer.
func TestBasicForwarding(t *testing.T) {
	t.Run("one layer", func(t *testing.T) { basicForwarding(t, "f") })
	t.Run("simulcast", func(t *testing.T) { basicForwarding(t) })
}

func basicForwarding(t *testing.T, layers ...string) {
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx := testCtx(t)
	pubConn, _ := join(t, h, "alice", "c-pub", sfu.RoleFull)
	viewConn, viewSig := join(t, h, "bob", "c-view", sfu.RoleViewer)
	const share = sfu.ShareID("s_fwd")
	startShare(t, pubConn, share, sfu.PresetAuto)
	simulcast := len(layers) == 0
	wantLayers := []string{"f"}
	if simulcast {
		wantLayers = []string{"f", "q"}
	}

	// ---- pending → live ----
	pub := newPublisher(t, newLongGOPSource(t), sfutest.LoopbackSettings(), layers...)
	if _, err := sfutest.Publish(ctx, pubConn, pub, 1, 1, share); err != nil {
		t.Fatalf("publish negotiation: %v", err)
	}
	if info, ok := h.SFU.Share(share); !ok || info.State != sfu.SharePending || !info.LiveAt.IsZero() || info.Profile != "" {
		t.Errorf("the share before its media = %+v, %v; want pending", info, ok)
	}
	if evs := h.Events.Events(); len(evs) != 0 {
		t.Errorf("room events before any media: %+v", evs)
	}
	if err := pub.Start(ctx); err != nil {
		t.Fatal(err)
	}
	went, err := h.Events.Wait(ctx, func(ev sfutest.RoomEvent) bool { return ev.Share.ID == share })
	if err != nil {
		t.Fatalf("the share did not go live: %v", err)
	}
	// The first thing the room hears is the state change, with the layer whose keyframe made it.
	if went.Kind != sfutest.ShareUpdated || went.Room != "lounge" || went.Share.State != sfu.ShareLive ||
		went.Share.LiveAt.IsZero() || went.Share.LiveAt.Before(went.Share.StartedAt) || went.Share.Profile != sfu.ProfileHigh {
		t.Errorf("the first room event = %+v, want ShareUpdated with the share live in profile 6400", went)
	}
	if !slices.ContainsFunc(went.Share.Layers, func(l sfu.LayerInfo) bool { return l.Width > 0 && l.Height > 0 }) {
		t.Errorf("the live share's layers = %+v, want the layer of the first keyframe with its size", went.Share.Layers)
	}
	// Half a second into the stream. The publisher has made one keyframe per layer, its first, and nobody asked for
	// another: no packet of the pending share was missed.
	eventually(t, func() bool { return layerStats(t, pub, "f").Frames >= 15 },
		func() string { return fmt.Sprintf("the publisher's stats are %+v", pub.Stats()) })
	for _, layer := range wantLayers {
		if st := layerStats(t, pub, layer); st.Keyframes != 1 || st.PLIs != 0 {
			t.Errorf("layer %s before any viewer: %d keyframes, %d PLIs; want its first keyframe and no request", layer,
				st.Keyframes, st.PLIs)
		}
	}

	// ---- a viewer subscribes while the media flows ----
	viewer := newViewer(t, sfutest.LoopbackSettings())
	viewSig.Attach(viewConn, viewer)
	subscribed := time.Now()
	errs, err := viewConn.UpdateSubscriptions(ctx, []sfu.SubscriptionUpdate{{Share: share, Video: sfu.QualityHigh, Audio: true}})
	if err != nil || errs[0] != nil {
		t.Fatalf("UpdateSubscriptions = %v, %v", errs, err)
	}
	video, audio := waitTrack(t, viewer, webrtc.RTPCodecTypeVideo), waitTrack(t, viewer, webrtc.RTPCodecTypeAudio)
	// 2.5 s of video and 2 s of audio: long enough for the ticker to have measured the layers.
	if err := video.Wait(ctx, func(r *sfutest.Recorder) bool { return len(sfutest.Frames(r.Packets())) >= 75 }); err != nil {
		t.Fatalf("%v (answer errors %v)", err, viewSig.Errs())
	}
	if err := audio.Wait(ctx, func(r *sfutest.Recorder) bool { return r.Stats().Packets >= 100 }); err != nil {
		t.Fatal(err)
	}
	if errs := viewSig.Errs(); len(errs) != 0 {
		t.Errorf("answering the sub offer: %v", errs)
	}

	// The DTLS-ready gate: the first video packet the viewer received starts a keyframe. The GOP is a minute, so that
	// keyframe is the one the SFU asked for when the sub PC connected, and it asked once.
	vp := video.Packets()
	if !vp[0].KeyStart {
		t.Errorf("the viewer's first video packet (seq %d, NAL type %d) carries no SPS", vp[0].Seq, vp[0].NAL)
	}
	if err := sfutest.CheckStartsOnSPS(vp); err != nil {
		t.Error(err)
	}
	if wait := video.Stats().First.Sub(subscribed); wait > 5*time.Second {
		t.Errorf("the first video packet came %v after the subscription", wait)
	}
	full := layerStats(t, pub, "f")
	if full.PLIs < 1 || full.Keyframes < 2 || full.Keyframes > 3 {
		t.Errorf("layer f with one viewer: %d PLIs, %d keyframes; want one request answered by one keyframe (two when "+
			"Pion wasn't ready for the first)", full.PLIs, full.Keyframes)
	}
	if simulcast {
		// Nobody watches the preview layer: it is neither forwarded nor asked for a keyframe.
		if st := layerStats(t, pub, "q"); st.PLIs != 0 || st.Keyframes != 1 {
			t.Errorf("layer q, which nobody watches: %d PLIs, %d keyframes; want none and its first", st.PLIs, st.Keyframes)
		}
	}

	// Video: one continuous stream of whole frames of the full layer, in the viewer's payload type for High, under
	// the msid the viewer maps it by.
	vp = sfutest.WithoutPartialTail(vp)
	if err := sfutest.CheckContinuous(vp); err != nil {
		t.Errorf("video: %v", err)
	}
	if err := sfutest.CheckVideoMarkers(vp); err != nil {
		t.Errorf("video: %v", err)
	}
	for _, f := range sfutest.Frames(vp) {
		if !f.HasMark || f.Mark.RID != "f" {
			t.Fatalf("the viewer asked for high and got a frame of layer %q (marker %v)", f.Mark.RID, f.HasMark)
		}
	}
	for _, p := range vp {
		if p.PT != 104 || p.RTX || p.Dup {
			t.Fatalf("video packet %+v, want PT 104 (the viewer's 64001f), not repeated", p)
		}
	}
	if video.StreamID() != string(share) || video.TrackID() != "v-"+string(share) {
		t.Errorf("video msid %q %q", video.StreamID(), video.TrackID())
	}
	if st := video.Stats(); st.Gaps != 0 || st.Lost != 0 || st.Duplicates != 0 {
		t.Errorf("video stats on loopback = %+v, want no gap", st)
	}

	// Audio flows: continuous, intact, at the publisher's 50 packets per second.
	ap := audio.Packets()
	if err := sfutest.CheckContinuous(ap); err != nil {
		t.Errorf("audio: %v", err)
	}
	if err := sfutest.CheckAudioMarkers(ap); err != nil {
		t.Errorf("audio: %v", err)
	}
	for _, p := range ap {
		if p.PT != 111 || p.Dup {
			t.Fatalf("audio packet %+v, want Opus on PT 111, not repeated", p)
		}
	}
	if audio.StreamID() != string(share) || audio.TrackID() != "a-"+string(share) {
		t.Errorf("audio msid %q %q", audio.StreamID(), audio.TrackID())
	}
	as := audio.Stats()
	if rate := float64(as.Packets-1) / as.Last.Sub(as.First).Seconds(); rate < 45 || rate > 55 || as.Gaps != 0 {
		t.Errorf("audio: %.1f packets per second with %d gaps, want 50 and none", rate, as.Gaps)
	}

	// What the SFU says about the share: live, watched by the viewer, with what it measured of each layer.
	info := waitShare(t, h.SFU, share, func(i sfu.ShareInfo) bool {
		return slices.Equal(i.Viewers, []sfu.ParticipantID{"bob"}) && len(i.Layers) == len(wantLayers) &&
			!slices.ContainsFunc(i.Layers, func(l sfu.LayerInfo) bool { return l.Bitrate == 0 || l.FPS == 0 })
	})
	if info.State != sfu.ShareLive || !info.Audio || info.Profile != sfu.ProfileHigh || !slices.Equal(layerRIDs(info), wantLayers) {
		t.Errorf("ShareInfo = %+v", info)
	}
	wantInfo := map[string]struct {
		w, h    int
		fps     float64
		bitrate int
	}{"f": {640, 360, 30, 400_000}, "q": {320, 180, 15, 100_000}}
	for _, l := range info.Layers {
		want := wantInfo[l.RID]
		if l.Width != want.w || l.Height != want.h || !l.Active || l.LossPct != 0 ||
			l.FPS < want.fps*0.7 || l.FPS > want.fps*1.3 || l.Bitrate < want.bitrate/2 || l.Bitrate > want.bitrate*2 {
			t.Errorf("layer %+v, want %dx%d at about %.0f fps and %d bit/s, active, without loss", l, want.w, want.h,
				want.fps, want.bitrate)
		}
	}
	// The room heard of the share only through ShareUpdated in state live: once at the state change, and then at
	// most every 250 ms as its tracks arrived. The latest report has every layer and the audio.
	var last sfutest.RoomEvent
	eventually(t, func() bool {
		evs := h.Events.Events()
		last = evs[len(evs)-1]
		return len(last.Share.Layers) == len(wantLayers) && last.Share.Audio
	}, func() string { return fmt.Sprintf("the last room event is %+v", last) })
	evs := h.Events.Events()
	for i, ev := range evs {
		if ev.Kind != sfutest.ShareUpdated || ev.Share.ID != share || ev.Share.State != sfu.ShareLive {
			t.Errorf("room event %d = %+v, want ShareUpdated for the live share", i, ev)
		}
		if i > 0 && ev.At.Sub(evs[i-1].At) < 200*time.Millisecond {
			t.Errorf("room events %d and %d are %v apart, want at least 250 ms: only state changes aren't debounced",
				i-1, i, ev.At.Sub(evs[i-1].At))
		}
	}

	// ---- with simulcast, a second viewer takes the preview layer, without audio ----
	var clients []interface{ Close() error } // the clients to close at the end, besides viewer and pub
	if simulcast {
		thumbConn, thumbSig := join(t, h, "carol", "c-thumb", sfu.RoleViewer)
		thumb := newViewer(t, sfutest.LoopbackSettings())
		clients = append(clients, thumb)
		thumbSig.Attach(thumbConn, thumb)
		errs, err := thumbConn.UpdateSubscriptions(ctx, []sfu.SubscriptionUpdate{{Share: share, Video: sfu.QualityLow}})
		if err != nil || errs[0] != nil {
			t.Fatalf("UpdateSubscriptions = %v, %v", errs, err)
		}
		small := waitTrack(t, thumb, webrtc.RTPCodecTypeVideo)
		if err := small.Wait(ctx, func(r *sfutest.Recorder) bool { return len(sfutest.Frames(r.Packets())) >= 15 }); err != nil {
			t.Fatalf("%v (answer errors %v)", err, thumbSig.Errs())
		}
		sp := sfutest.WithoutPartialTail(small.Packets())
		for _, check := range []func([]sfutest.Packet) error{
			sfutest.CheckStartsOnSPS, sfutest.CheckContinuous, sfutest.CheckVideoMarkers,
		} {
			if err := check(sp); err != nil {
				t.Errorf("the preview viewer's video: %v", err)
			}
		}
		for _, f := range sfutest.Frames(sp) {
			if !f.HasMark || f.Mark.RID != "q" {
				t.Fatalf("the viewer asked for low and got a frame of layer %q", f.Mark.RID)
			}
		}
		if st := layerStats(t, pub, "q"); st.PLIs < 1 || st.Keyframes < 2 || st.Keyframes > 3 {
			t.Errorf("layer q with one viewer: %d PLIs, %d keyframes", st.PLIs, st.Keyframes)
		}
		if n := len(thumb.Recorders()); n != 1 {
			t.Errorf("the preview viewer has %d tracks, want only video: it didn't ask for audio", n)
		}
		waitShare(t, h.SFU, share, func(i sfu.ShareInfo) bool {
			return slices.Equal(i.Viewers, []sfu.ParticipantID{"bob", "carol"})
		})
		// The first viewer never noticed.
		if st := layerStats(t, pub, "f"); st.Keyframes != full.Keyframes {
			t.Errorf("layer f made %d keyframes, %d before the preview viewer came", st.Keyframes, full.Keyframes)
		}
		if err := sfutest.CheckContinuous(sfutest.WithoutPartialTail(video.Packets())); err != nil {
			t.Errorf("the first viewer's video after the second one joined: %v", err)
		}
	}
	subs, err := viewConn.Subscriptions(ctx)
	if err != nil || len(subs) != 1 || subs[0].VideoSent == 0 || subs[0].AudioSent == 0 || subs[0].VideoDrops != 0 ||
		subs[0].AudioDrops != 0 {
		t.Errorf("the viewer's subscription = %+v, %v; want packets sent and none dropped", subs, err)
	}

	// ---- Close, with all of it running ----
	start := time.Now()
	if err := h.SFU.Close(); err != nil {
		t.Errorf("SFU.Close: %v", err)
	}
	took := time.Since(start)
	if took > time.Second {
		t.Errorf("SFU.Close took %v, want at most 1 s", took)
	}
	t.Logf("first video packet %v after the subscription; layer f: %d PLIs, %d keyframes; layers %+v; SFU.Close took %v",
		video.Stats().First.Sub(subscribed).Round(time.Millisecond), full.PLIs, full.Keyframes, info.Layers,
		took.Round(time.Microsecond))
	for _, c := range []*sfu.Conn{pubConn, viewConn} {
		select {
		case <-c.Done():
		default:
			t.Errorf("Conn %s was still closing its PeerConnections when SFU.Close returned", c.ID())
		}
	}
	// The share ended with the shutdown, and that is the last the room heard of it.
	evs = h.Events.Events()
	if end := evs[len(evs)-1]; end.Kind != sfutest.ShareEnded || end.Share.ID != share ||
		end.Reason != sfu.EndReasonServerShutdown || end.Share.State != sfu.ShareLive {
		t.Errorf("the last room event = %+v, want the share ended by the shutdown", end)
	}
	if n := len(evs) - 1; slices.ContainsFunc(evs[:n], func(ev sfutest.RoomEvent) bool { return ev.Kind == sfutest.ShareEnded }) {
		t.Errorf("ShareEnded more than once: %+v", evs)
	}
	// None of the SFU's goroutines is left: actors, track readers, DownTrack writers and RTCP readers, the ticker.
	// (A Pion callback that was already on its way may still be passing through; it has nothing to wait for, so it
	// gets a moment and no more.)
	left := sfuGoroutines()
	for deadline := time.Now().Add(200 * time.Millisecond); len(left) != 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
		left = sfuGoroutines()
	}
	if len(left) != 0 {
		t.Errorf("%d goroutines of the SFU are left after Close:\n%s", len(left), strings.Join(left, "\n\n"))
	}
	// And once the clients and the Transport are closed too, no goroutine at all (Pion's get 2 s to settle, 02 §17).
	for _, c := range append(clients, viewer, pub, h) {
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}
	leaks := goleak.Find()
	for deadline := time.Now().Add(2 * time.Second); leaks != nil && time.Now().Before(deadline); {
		time.Sleep(100 * time.Millisecond)
		leaks = goleak.Find()
	}
	if leaks != nil {
		t.Errorf("goroutines left after everything closed: %v", leaks)
	}
}

// TestFailedViewerCostsNoKeyframes: a viewer that vanishes without a word (its network is gone) must cost the others
// nothing. Its sub PC fails after the ICE timeouts, and Pion then takes every packet written to it without an error
// and sends none. The DTLS-ready gate follows the PC: the viewer's DownTracks stop, the viewer leaves the share's
// list, and nobody asks the publisher for anything on its behalf. (Before, each packet that went nowhere restarted
// the viewer's stream, and the packets waiting for its keyframe asked the publisher for one every 500 ms, which every
// other viewer of the layer got.)
func TestFailedViewerCostsNoKeyframes(t *testing.T) {
	t.Cleanup(sfu.SetMediaICETimeouts(time.Second, time.Second, 200*time.Millisecond))
	h := newHarness(t, sfutest.HarnessOptions{})
	ctx := testCtx(t)
	pubConn, pubSig := join(t, h, "alice", "c-pub", sfu.RoleFull)
	const share = sfu.ShareID("s_gone")
	startShare(t, pubConn, share, sfu.PresetAuto)
	pub := newPublisher(t, newLongGOPSource(t), sfutest.LoopbackSettings(), "f")
	if _, err := sfutest.Publish(ctx, pubConn, pub, 1, 1, share); err != nil {
		t.Fatalf("publish negotiation: %v", err)
	}
	if err := pub.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// Two viewers of the one layer. The second one's only socket goes through a FaultConn.
	var lc net.ListenConfig
	sock, err := lc.ListenPacket(ctx, "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	wire := sfutest.NewFaultConn(sock)
	mux := webrtc.NewICEUDPMux(nil, wire)
	t.Cleanup(func() { _ = mux.Close() }) // after the viewer's cleanup, which runs first
	lossy := sfutest.LoopbackSettings()
	lossy.SetICEUDPMux(mux)
	stayConn, staySig := join(t, h, "bob", "c-stay", sfu.RoleViewer)
	goneConn, goneSig := join(t, h, "carol", "c-gone", sfu.RoleViewer)
	stay, gone := newViewer(t, sfutest.LoopbackSettings()), newViewer(t, lossy)
	staySig.Attach(stayConn, stay)
	goneSig.Attach(goneConn, gone)
	for _, c := range []*sfu.Conn{stayConn, goneConn} {
		errs, err := c.UpdateSubscriptions(ctx, []sfu.SubscriptionUpdate{{Share: share, Video: sfu.QualityHigh, Audio: true}})
		if err != nil || errs[0] != nil {
			t.Fatalf("UpdateSubscriptions = %v, %v", errs, err)
		}
	}
	stayVideo, goneVideo := waitTrack(t, stay, webrtc.RTPCodecTypeVideo), waitTrack(t, gone, webrtc.RTPCodecTypeVideo)
	for _, r := range []*sfutest.Recorder{stayVideo, goneVideo} {
		if err := r.Wait(ctx, func(r *sfutest.Recorder) bool { return len(sfutest.Frames(r.Packets())) >= 30 }); err != nil {
			t.Fatalf("%v (answer errors %v, %v)", err, staySig.Errs(), goneSig.Errs())
		}
	}
	waitShare(t, h.SFU, share, func(i sfu.ShareInfo) bool {
		return slices.Equal(i.Viewers, []sfu.ParticipantID{"bob", "carol"})
	})
	// Both streams have run for a second since their keyframes: whatever was asked for to start them is done.
	before, stayKeys := layerStats(t, pub, "f"), stayVideo.Stats().Keyframes

	// The network of the second viewer goes away: nothing in, nothing out, no goodbye.
	wire.SetDrop(func(sfutest.Direction, net.Addr, []byte) bool { return true })
	if err := goneSig.WaitPCState(ctx, sfu.PCSub, 1, "failed"); err != nil {
		t.Fatalf("the vanished viewer's sub PC: %v", err)
	}
	failedAt := time.Now()
	// The gate closed with the PC: the viewer no longer counts as one.
	waitShare(t, h.SFU, share, func(i sfu.ShareInfo) bool {
		return slices.Equal(i.Viewers, []sfu.ParticipantID{"bob"})
	})
	subs, err := goneConn.Subscriptions(ctx)
	if err != nil || len(subs) != 1 {
		t.Fatalf("the vanished viewer's subscriptions = %+v, %v", subs, err)
	}
	// Four of the layer's throttle intervals with the PC failed.
	time.Sleep(time.Until(failedAt.Add(2 * time.Second)))

	// A PC of the others that dropped out for a moment (a machine too busy for 1 s ICE timeouts) asks for a keyframe
	// when it is back, rightly: then this run says nothing about the failed viewer.
	for name, sig := range map[string]*sfutest.DirectSignaler{"publisher": pubSig, "staying viewer": staySig} {
		for _, ev := range sig.Events() {
			if st, ok := ev.(sfu.PCStateEvent); ok && st.State != "connected" {
				t.Skipf("the %s's PC went %s during the test: the machine is too slow for its ICE timeouts", name, st.State)
			}
		}
	}
	after := layerStats(t, pub, "f")
	if after.PLIs != before.PLIs || after.Keyframes != before.Keyframes {
		t.Errorf("while a viewer's sub PC was failed for 2 s the publisher got %d PLIs and made %d keyframes, want none: "+
			"its packets go nowhere, and a keyframe wouldn't change that", after.PLIs-before.PLIs,
			after.Keyframes-before.Keyframes)
	}
	if info, _ := h.SFU.Share(share); !slices.Equal(info.Viewers, []sfu.ParticipantID{"bob"}) || info.State != sfu.ShareLive {
		t.Errorf("the share = %+v, want it live and watched by bob alone", info)
	}
	later, err := goneConn.Subscriptions(ctx)
	if err != nil || len(later) != 1 || later[0].VideoSent != subs[0].VideoSent || later[0].AudioSent != subs[0].AudioSent {
		t.Errorf("the vanished viewer's subscription went from %+v to %+v (%v), want nothing more sent to it", subs, later, err)
	}
	// The viewer that stayed never noticed.
	vp := sfutest.WithoutPartialTail(stayVideo.Packets())
	if err := sfutest.CheckContinuous(vp); err != nil {
		t.Errorf("the staying viewer's video: %v", err)
	}
	if st := stayVideo.Stats(); st.Gaps != 0 || st.Lost != 0 || st.Keyframes != stayKeys || time.Since(st.Last) > time.Second {
		t.Errorf("the staying viewer's video stats = %+v, want a stream that still runs, without a gap and with the %d "+
			"keyframes it had before the other viewer vanished", st, stayKeys)
	}
}
