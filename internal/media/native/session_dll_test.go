package native

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/mediatest"
	"github.com/MoonWX/isshoni/internal/server/sfu"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// The session tests against a real engine (04 §18.2's "Go on Windows", README WA23): the win-x64-ci DLL that
// ISSHONI_TEST_ENGINE_DLL names (session_windows_test.go opens it), with test:pattern, IM_ENC_FAKE and the test tone,
// under goleak (main_test.go). Without an engine they skip.

// sessionEngine opens the engine of these tests and closes it when the test ends, or skips the test.
var sessionEngine = func(t *testing.T) *Engine {
	t.Helper()
	t.Skip("no engine DLL to test against on this OS (04 §18.2: CI windows with ISSHONI_TEST_ENGINE_DLL)")
	return nil
}

// testPattern is the listed test source of the win-x64-ci DLL.
var testPattern = media.Source{ID: "test:pattern", Kind: media.SourceScreen, Capture: "test"}

// probeFallback is the engine Conformance runs: until README WA44's probe exists, im_probe answers
// not_implemented, and the fake encoder the session core uses (04 §18.2) is what a test:pattern session gets with
// IM_ENC_AUTO, so check 1 is given the probe the win-x64-ci DLL will report for it: IM_ENC_FAKE, tested, live
// bitrate, both profiles. Everything else is the engine's.
type probeFallback struct{ *Engine }

func (p probeFallback) Probe(ctx context.Context, refresh bool) (media.Probe, error) {
	pr, err := p.Engine.Probe(ctx, refresh)
	var me *media.Error
	if !errors.As(err, &me) || me.Reason != reasonNotImplemented {
		return pr, err
	}
	return media.Probe{
		OS: media.OSInfo{Name: "windows", Supported: true},
		Encoders: []media.EncoderInfo{{Name: "fake", Adapter: -1, Tested: true, LiveBitrate: true,
			Profiles: []string{profileHigh, profileCB}}},
		Capture: media.CaptureCaps{MaxFPS: 60},
		Audio:   media.AudioCapsFromFlags(0),
	}, nil
}

// TestDLLConformance is 01 §6.4's suite on the native engine: test:pattern, {Listed, TestTone, Encoder: "fake"},
// checks 5 (the fake's bitrate follows its target) and 7 (Constrained Baseline) included.
func TestDLLConformance(t *testing.T) {
	mediatest.Conformance(t, func(t *testing.T) media.Engine { return probeFallback{sessionEngine(t)} }, testPattern,
		mediatest.Options{Listed: true, TestTone: true, Encoder: "fake"})
}

// nativeConfig is a share of the test pattern: f 1080p60 at 4 Mbit/s, q 360p15 at 300 kbit/s, the test tone.
func nativeConfig() media.Config {
	return media.Config{
		Source: testPattern.ID, Listed: true, Encoder: "fake", Profile: profileHigh, Preset: "auto",
		Layers: []media.LayerConfig{
			{RID: "f", MaxPixels: 1920 * 1080, MaxFPS: 60, Bitrate: 4_000_000},
			{RID: "q", MaxPixels: 640 * 360, MaxFPS: 15, Bitrate: 300_000},
		},
		Audio: &media.AudioConfig{Mode: media.AudioTestTone},
	}
}

func startNative(t *testing.T, e *Engine, cfg media.Config) *session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ms, err := e.Start(ctx, cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	s := ms.(*session)
	t.Cleanup(func() { _ = s.Stop() })
	return s
}

// TestDLLSession: the binding over the real session core: one EventLayer per layer before any media, each layer's
// first packet a Reinit keyframe within 500 ms, PCM chunks through Next (TapPCM, 05 §13.2), an EventLayer of the new
// profile before the Reinit packets of Reconfigure{Profile}, Stats, and ErrStopped after Stop.
func TestDLLSession(t *testing.T) {
	e := sessionEngine(t)
	cfg := nativeConfig()
	cfg.Audio.TapPCM = true
	started := time.Now()
	s := startNative(t, e, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	next := func() media.Packet {
		t.Helper()
		p, err := s.Next(ctx)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		return p
	}
	for _, rid := range []string{"f", "q"} {
		p := next()
		if p.Kind != media.KindEvent || p.Event.Code != media.EventLayer || p.Event.Layer != rid ||
			p.Event.Encoder != "fake" || p.Event.Profile != profileHigh || !p.Event.NoOSIndicator {
			t.Fatalf("before any media: %+v %+v, want layer %s's EventLayer", p, p.Event, rid)
		}
	}
	first := map[string]media.Packet{}
	gotPCM := false
	checkPCM := func(p media.Packet) {
		t.Helper()
		if p.Kind != media.KindPCM {
			return
		}
		gotPCM = true
		if len(p.Data) != pcmChunkBytes || p.CaptureNS < 0 {
			t.Errorf("PCM chunk of %d bytes at %d", len(p.Data), p.CaptureNS)
		}
	}
	for len(first) < 2 {
		p := next()
		checkPCM(p)
		if p.Kind == media.KindVideo {
			if _, ok := first[p.Layer]; !ok {
				first[p.Layer] = p
				if !p.Keyframe || !p.Reinit {
					t.Errorf("layer %s: first packet Keyframe %v Reinit %v", p.Layer, p.Keyframe, p.Reinit)
				}
			}
		}
	}
	if d := time.Since(started); d > 600*time.Millisecond {
		t.Errorf("first packets %v after Start", d)
	}
	// PCM comes through im_next_packet like everything else, and Next returns it as KindPCM (TapPCM).
	for deadline := time.Now().Add(time.Second); !gotPCM && time.Now().Before(deadline); {
		checkPCM(next())
	}
	if !gotPCM {
		t.Error("no PCM chunk with TapPCM")
	}

	if err := s.Reconfigure(media.Reconfig{Profile: profileCB}); err != nil {
		t.Fatal(err)
	}
	pending := map[string]bool{"f": true, "q": true}
	sawEvent := map[string]bool{}
	for len(pending) > 0 {
		p := next()
		switch {
		case p.Kind == media.KindEvent && p.Event.Code == media.EventLayer:
			if p.Event.Profile != profileCB {
				t.Errorf("EventLayer %+v after the switch", p.Event)
			}
			sawEvent[p.Event.Layer] = true
		case p.Kind == media.KindVideo && p.Reinit:
			if !sawEvent[p.Layer] {
				t.Errorf("layer %s: a Reinit packet without its EventLayer first", p.Layer)
			}
			delete(pending, p.Layer)
		}
	}
	st, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Layers) != 2 || st.Layers[0].RID != "f" || st.Layers[0].Encoder != "fake" {
		t.Errorf("Stats %+v", st)
	}
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Next(ctx); !errors.Is(err, media.ErrStopped) {
		t.Errorf("Next after Stop: %v", err)
	}
}

// TestDLLStartErrors: statuses of the real engine map to their codes and tokens.
func TestDLLStartErrors(t *testing.T) {
	e := sessionEngine(t)
	ctx := context.Background()
	cfg := nativeConfig()
	cfg.Source = "pick:999999"
	cfg.Listed = false
	_, err := e.Start(ctx, cfg)
	wantError(t, err, media.CodeSourceGone, tokenPickExpired, "start")

	cfg = nativeConfig()
	cfg.Listed = false
	_, err = e.Start(ctx, cfg)
	wantError(t, err, media.CodeInvalid, "", "start")

	cfg = nativeConfig()
	cfg.Encoder = "nvenc" // forced, and the win-x64-ci DLL has no NVENC session on the runner
	_, err = e.Start(ctx, cfg)
	wantError(t, err, media.CodeNoEncoder, tokenForcedFailed, "start")

	// The DLL's 4 sessions: Start's one and 3 StartExtra, then busy (session_limit).
	startNative(t, e, nativeConfig())
	video := nativeConfig()
	video.Audio = nil
	for range 3 {
		ms, err := e.StartExtra(ctx, video)
		if err != nil {
			t.Fatalf("StartExtra: %v", err)
		}
		t.Cleanup(func() { _ = ms.Stop() })
	}
	_, err = e.StartExtra(ctx, video)
	wantError(t, err, media.CodeBusy, tokenSessionLimit, "start_extra")
}

// TestNativeIntoPublisher is 04 §18.2's milestone (README §5): a native session (IM_ENC_FAKE and the test tone, both
// carrying M1's Markers) → publish.Publisher → M1's in-process SFU harness → sfutest.Viewer. Frames arrive on both
// layers, the next encoded frame answers a PLI, and the flash/beep A/V offset read from the video and audio
// Markers through the sender reports is under 45 ms (M1 integration 5's check, now on native timing). The offset is
// asserted on f, which carries every frame of the 60 Hz pattern, so its flash frame is the beep's instant; q thins
// the pattern to 15 fps on the pacer's schedule (04 §8.4), so its flash is its first frame at or after the beep (04
// §18.2's rule), up to one q interval (67 ms) later: logged, and bounded by that interval.
func TestNativeIntoPublisher(t *testing.T) {
	e := sessionEngine(t)
	s := startNative(t, e, nativeConfig())
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	h, err := sfutest.NewHarness(ctx, sfutest.HarnessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Errorf("Harness.Close: %v", err)
		}
	})
	const share = sfu.ShareID("s_native")
	join := func(user sfu.UserID, conn sfu.ConnID, role sfu.Role) (*sfu.Conn, *sfutest.DirectSignaler) {
		t.Helper()
		c, sig, err := h.Join(sfu.JoinParams{
			Room: "lounge", Participant: sfu.ParticipantID(user), User: user, Conn: conn, Role: role,
			Decode: sfu.DecodeCaps{H264: []sfu.ProfileKey{sfu.ProfileHigh, sfu.ProfileConstrainedBaseline}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return c, sig
	}
	pubConn, _ := join("alice", "c-pub", sfu.RoleFull)
	if _, err := pubConn.StartShare(ctx, sfu.StartShareParams{ID: share, Preset: sfu.PresetAuto, Audio: true,
		Source: sfu.SourceScreen}); err != nil {
		t.Fatal(err)
	}
	pub, err := publish.New(publish.Options{Source: s, Settings: sfutest.LoopbackSettings(), Audio: true,
		Profile: profileHigh})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pub.Close(); err != nil {
			t.Errorf("Publisher.Close: %v", err)
		}
	})
	if _, err := sfutest.Publish(ctx, pubConn, pub, 1, 1, share); err != nil {
		t.Fatalf("publish negotiation: %v", err)
	}
	if err := pub.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 15*time.Second, func() bool {
		i, ok := h.SFU.Share(share)
		return ok && i.State == sfu.ShareLive && len(i.Layers) == 2 && i.Audio
	}, "the share goes live with two layers and audio")

	viewConn, viewSig := join("bob", "c-view", sfu.RoleViewer)
	viewer, err := sfutest.NewViewer(sfutest.ViewerOptions{Settings: sfutest.LoopbackSettings()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := viewer.Close(); err != nil {
			t.Errorf("Viewer.Close: %v", err)
		}
	})
	viewSig.Attach(viewConn, viewer)
	subscribe := func(q sfu.Quality) {
		t.Helper()
		errs, err := viewConn.UpdateSubscriptions(ctx, []sfu.SubscriptionUpdate{{Share: share, Video: q, Audio: true}})
		if err != nil || slices.ContainsFunc(errs, func(e error) bool { return e != nil }) {
			t.Fatalf("UpdateSubscriptions(%v) = %v, %v", q, errs, err)
		}
	}
	subscribe(sfu.QualityHigh)
	match := func(kind webrtc.RTPCodecType) func(*sfutest.Recorder) bool {
		return func(r *sfutest.Recorder) bool { return r.Kind() == kind && r.StreamID() == string(share) }
	}
	videoRec, err := viewer.WaitRecorder(ctx, match(webrtc.RTPCodecTypeVideo))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := viewer.WaitRecorder(ctx, match(webrtc.RTPCodecTypeAudio)); err != nil {
		t.Fatal(err)
	}
	frames := func(rid string) int {
		n := 0
		for _, f := range sfutest.Frames(sfutest.WithoutPartialTail(videoRec.Packets())) {
			if f.HasMark && f.Mark.RID == rid {
				n++
			}
		}
		return n
	}
	// Four seconds of f: four flashes and beeps, and sender reports on both tracks.
	waitUntil(t, 20*time.Second, func() bool { return frames("f") >= 240 }, "240 frames of f")

	// A PLI, away from any IDR: f's GOP (4 s) restarts at every keyframe, so its GOP IDR comes 240 frames after the
	// subscription's keyframe, just as the wait above ends, and the engine holds a request back until 250 ms after an
	// IDR (04 §6.4). The PLI goes 30 frames (500 ms) or more after f's newest keyframe and 40 or more before the next
	// GOP IDR, and the next encoded frame must answer it: its Marker frame at most 3 past the newest one the viewer had
	// (the frames on their way), its first packet within 100 ms (at 60 fps a frame is 16.7 ms).
	fMarks := func(fs []sfutest.Frame) (newest, key uint32, ok bool) {
		for _, f := range fs {
			if f.HasMark && f.Mark.RID == "f" {
				newest, ok = max(newest, f.Mark.Frame), true
				if f.Mark.Keyframe {
					key = max(key, f.Mark.Frame)
				}
			}
		}
		return newest, key, ok
	}
	var askedFrame uint32
	var before int
	waitUntil(t, 10*time.Second, func() bool {
		fs := sfutest.Frames(videoRec.Packets())
		newest, key, ok := fMarks(fs)
		if !ok || newest < key+30 || newest > key+200 {
			return false
		}
		askedFrame, before = newest, len(fs)
		return true
	}, "f 30 to 200 frames past its newest keyframe")
	asked := time.Now()
	if err := viewer.RequestKeyframe(videoRec); err != nil {
		t.Fatal(err)
	}
	var answer sfutest.Frame
	waitUntil(t, 5*time.Second, func() bool {
		fs := sfutest.Frames(videoRec.Packets())
		for _, f := range fs[min(before, len(fs)):] {
			if f.HasMark && f.Mark.RID == "f" && f.Mark.Keyframe {
				answer = f
				return true
			}
		}
		return false
	}, "a keyframe of f after the PLI")
	t.Logf("a PLI at frame %d of f: keyframe at frame %d, %v later", askedFrame, answer.Mark.Frame,
		answer.First.Sub(asked))
	if answer.Mark.Frame > askedFrame+3 {
		t.Errorf("the PLI's keyframe is frame %d of f, asked at frame %d: want the next encoded frame (at most +3)",
			answer.Mark.Frame, askedFrame)
	}
	if d := answer.First.Sub(asked); d > 100*time.Millisecond {
		t.Errorf("the PLI's keyframe came %v after it, want within 100 ms", d)
	}

	// q: frames arrive on the low layer too.
	subscribe(sfu.QualityLow)
	waitUntil(t, 20*time.Second, func() bool { return frames("q") >= 45 }, "45 frames of q")

	samples, err := viewer.AVOffset(string(share))
	if err != nil {
		t.Fatal(err)
	}
	perLayer := map[string]int{}
	for _, smp := range samples {
		perLayer[smp.Layer]++
		bound := 45 * time.Millisecond
		if smp.Layer != "f" {
			bound = time.Second/15 + 5*time.Millisecond
			t.Logf("layer %s: flash/beep offset %v at frame %d", smp.Layer, smp.Offset, smp.Frame)
		}
		if smp.Offset.Abs() > bound {
			t.Errorf("layer %s: flash/beep offset %v at frame %d, want at most %v", smp.Layer, smp.Offset, smp.Frame,
				bound)
		}
	}
	if perLayer["f"] < 3 {
		t.Errorf("flash/beep pairs per layer %v, want at least 3 on f", perLayer)
	}
}

// waitUntil polls cond every 20 ms until it holds, or fails the test after d.
func waitUntil(t *testing.T, d time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
