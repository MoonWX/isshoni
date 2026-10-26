package itest

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/core"
	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// Cases D1, D2, D14 and D15 of docs/m2/01-client-core.md §14: the client core shares, is one Participant with its
// user's other connection, ends a share whose source went away, and stops another share of its user. D17 (no leaks)
// is every case's: noLeaks, and desktopApp.close's checks.

// appEngine is the fake engine of these cases: a 1280×720 window, so the full layer (1080p budget) runs at
// 1280×720 and the preview (640×360 budget) at 640×360, with a flash and a beep every 200 ms for the A/V check.
func appEngine(seed uint64) desktopOptions {
	return desktopOptions{engine: fake.EngineConfig{
		SourceSize: [2]int{1280, 720}, FlashEvery: 200 * time.Millisecond, Seed: seed,
	}}
}

// TestDesktopShareAndWatch is D1: StartShare → live within 3 s; a viewer gets both layers and the audio, as one
// continuous stream that starts on an SPS; flash and beep, captured together, are at most 10 ms apart on the
// viewer's clocks (the plan's target is 45 ms end to end).
func TestDesktopShareAndWatch(t *testing.T) {
	noLeaks(t)
	w := newWorld(t, servertest.Options{})
	app := w.desktop(w.member("alice"), appEngine(1))
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	viewer := bob.watch(sfutest.LoopbackSettings(), 0)

	began := time.Now()
	sh := app.start(defaultRoom(bob))
	if st := sh.Snapshot().State; st != core.StateStarting && st != core.StateLive {
		t.Errorf("the share right after StartShare is %s, want starting (or live already)", st)
	}
	snap := app.waitState(sh, core.StateLive)
	live := time.Since(began)
	t.Logf("live %v after StartShare", live.Round(time.Millisecond))
	if live > 3*time.Second {
		t.Errorf("the share went live %v after StartShare, want at most 3 s", live)
	}
	info := bob.waitLive(snap.ID) // both layers, High, audio
	if info.ConnectionID != app.core.Status().ConnectionID || info.Kind != protocol.ShareKindWindow ||
		info.Preset != protocol.PresetAuto {
		t.Errorf("the share in room.state = %+v, want a window share of the core's connection %s", info,
			app.core.Status().ConnectionID)
	}

	// ---- both layers and the audio ----
	bob.subscribe(want(snap.ID, true))
	video, audio := track(t, viewer, snap.ID, webrtc.RTPCodecTypeVideo), track(t, viewer, snap.ID, webrtc.RTPCodecTypeAudio)
	waitFrames(t, video, 0, "f", 120) // 2 s: each stream gets sender reports
	waitPackets(t, audio, 100)
	bob.waitStatus(want(snap.ID, true))
	low := protocol.SubscriptionWant{ShareID: snap.ID, Video: protocol.VideoLayerLow, Audio: protocol.AudioStateOn}
	mark := len(video.Packets())
	bob.subscribe(low)
	waitFrames(t, video, mark, "q", 23) // 1.5 s at 15 fps
	waitPackets(t, audio, 50)
	bob.waitStatus(low)

	vp := checkVideo(t, "video", video.Packets()) // continuous, starts on an SPS, whole frames
	if first := vp[0]; !first.KeyStart {
		t.Errorf("the first packet the viewer got (seq %d, NAL type %d) is no SPS", first.Seq, first.NAL)
	}
	if runs := layerRuns(vp); runs != "f q" {
		t.Errorf("the viewer got the layers %q, want the full layer, then the preview", runs)
	}
	checkSwitchTimestamps(t, vp)
	ap := audio.Packets()
	for _, check := range []func([]sfutest.Packet) error{sfutest.CheckContinuous, sfutest.CheckAudioMarkers} {
		if err := check(ap); err != nil {
			t.Errorf("audio: %v", err)
		}
	}
	if vs, as := video.Stats(), audio.Stats(); vs.Gaps != 0 || vs.Lost != 0 || as.Gaps != 0 || as.Lost != 0 {
		t.Errorf("gaps on loopback: video %+v, audio %+v", vs, as)
	}

	// ---- A/V ----
	samples, err := viewer.AVOffset(snap.ID)
	if err != nil {
		t.Fatal(err)
	}
	perLayer, worst := map[string]int{}, time.Duration(0)
	for _, s := range samples {
		perLayer[s.Layer]++
		worst = max(worst, s.Offset.Abs())
		if s.Offset.Abs() > 10*time.Millisecond {
			t.Errorf("A/V offset %v at frame %d of layer %s, want at most 10 ms", s.Offset, s.Frame, s.Layer)
		}
	}
	t.Logf("A/V: pairs per layer %v, worst offset %v", perLayer, worst)
	if perLayer["f"] < 5 || perLayer["q"] < 3 {
		t.Errorf("flash and beep pairs per layer: %v, want at least 5 of f and 3 of q", perLayer)
	}

	// ---- what the core says about it ----
	snap = sh.Snapshot()
	f, q := layerOf(snap, "f"), layerOf(snap, "q")
	if f.Width != 1280 || f.Height != 720 || q.Width != 640 || q.Height != 360 || f.Encoder != fake.EncoderName ||
		f.Profile != "6400" || !f.Active || !q.Active || snap.Encoder != fake.EncoderName || !snap.NoOSIndicator ||
		!snap.Audio || snap.End != nil {
		t.Errorf("the share's snapshot = %+v, want f 1280×720 and q 640×360 on the fake encoder in High, borderless",
			snap)
	}
	if f.SentBps <= 0 || f.FPS <= 0 || snap.AudioSentBps <= 0 || f.CaptureToSend.Samples == 0 {
		t.Errorf("the share's live counters = %+v (audio %d bit/s), want them running", f, snap.AudioSentBps)
	}
	if evs := app.shareEvents(); len(evs) == 0 || !slices.ContainsFunc(evs, func(s core.ShareSnapshot) bool {
		return s.State == core.StateLive
	}) {
		t.Errorf("EventShare snapshots %+v, want one of the live share", evs)
	}
	// The live counters fire no EventShare (§11.10): over a second of media, each EventShare that comes changed a field
	// that isn't one. The ramp is still stepping the full layer's target every RampEvery until 8 Mbit/s (01 §9.3), and
	// its third step, 8 s after live, falls in this second.
	time.Sleep(500 * time.Millisecond) // the coalescing of the last watcher change
	quiet := len(app.shareEvents())
	time.Sleep(time.Second)
	evs := app.shareEvents()
	for i := max(quiet, 1); i < len(evs); i++ {
		if reflect.DeepEqual(withoutCounters(evs[i]), withoutCounters(evs[i-1])) {
			t.Errorf("EventShare fired in a second of media alone, for live counters only: %+v after %+v", evs[i],
				evs[i-1])
		}
	}
	bob.check()
}

// withoutCounters returns snap without its live counters (01 §11.10), which fire no EventShare.
func withoutCounters(snap core.ShareSnapshot) core.ShareSnapshot {
	snap.AudioSentBps, snap.Server = 0, nil
	snap.Layers = slices.Clone(snap.Layers)
	for i := range snap.Layers {
		l := &snap.Layers[i]
		l.SentBps, l.FPS, l.FPSIn = 0, 0, 0
		l.CaptureToSend, l.EngineLatency = publish.LatencyStats{}, publish.LatencyStats{}
	}
	return snap
}

// layerOf returns a snapshot's layer rid (a zero one when it has none).
func layerOf(snap core.ShareSnapshot, rid string) core.LayerSnapshot {
	for _, l := range snap.Layers {
		if l.RID == rid {
			return l
		}
	}
	return core.LayerSnapshot{}
}

// participantsOf returns the participants of a user in a room.state.
func participantsOf(st protocol.RoomState, userID string) []protocol.ParticipantInfo {
	var out []protocol.ParticipantInfo
	for _, p := range st.Participants {
		if p.UserID == userID {
			out = append(out, p)
		}
	}
	return out
}

// connIDs returns the connection ids of a participant.
func connIDs(p protocol.ParticipantInfo) []string {
	var out []string
	for _, c := range p.Connections {
		out = append(out, c.ID)
	}
	slices.Sort(out)
	return out
}

// TestDesktopOneParticipant is D2: the core's publisher connection and the viewer connection of the same user (the
// in-app SPA's) are one Participant with two connections while the core shares; the user's own viewer is no
// watcher; and the core is in no room before and after the share.
func TestDesktopOneParticipant(t *testing.T) {
	noLeaks(t)
	w := newWorld(t, servertest.Options{})
	alice := w.member("alice")
	spa := w.connect(alice, protocol.RoleViewer)
	app := w.desktop(alice, appEngine(2))
	coreID := app.core.Status().ConnectionID
	room := defaultRoom(spa)

	// Before: the core joined nothing.
	alone := func() bool {
		ps := participantsOf(spa.roomState(), spa.userID)
		return len(ps) == 1 && slices.Equal(connIDs(ps[0]), []string{spa.connID()})
	}
	eventually(t, alone, func() string {
		return fmt.Sprintf("before the share, room.state = %+v, want alice with the SPA's connection only", spa.roomState())
	})

	// During: one participant, two connections.
	sh := app.start(room)
	snap := app.waitState(sh, core.StateLive)
	both := []string{spa.connID(), coreID}
	slices.Sort(both)
	eventually(t, func() bool {
		ps := participantsOf(spa.roomState(), spa.userID)
		return len(ps) == 1 && slices.Equal(connIDs(ps[0]), both)
	}, func() string {
		return fmt.Sprintf("during the share, room.state = %+v, want alice as one participant with %v", spa.roomState(), both)
	})
	for _, c := range participantsOf(spa.roomState(), spa.userID)[0].Connections {
		if c.ID == coreID && (c.Kind != protocol.ClientKindDesktop || c.Role != protocol.RolePublisher) {
			t.Errorf("the core's connection in room.state = %+v, want kind desktop, role publisher", c)
		}
	}

	// The SPA watches its own share: no watcher. Another member watching is one.
	spaViewer := spa.watch(sfutest.LoopbackSettings(), 0)
	spa.subscribe(want(snap.ID, true))
	waitFrames(t, track(t, spaViewer, snap.ID, webrtc.RTPCodecTypeVideo), 0, "f", 15)
	spa.waitStatus(want(snap.ID, true))
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	bobViewer := bob.watch(sfutest.LoopbackSettings(), 0)
	bob.waitLive(snap.ID)
	bob.subscribe(want(snap.ID, true))
	waitFrames(t, track(t, bobViewer, snap.ID, webrtc.RTPCodecTypeVideo), 0, "f", 15)
	watchers := []protocol.Watcher{{UserID: bob.userID, Video: protocol.VideoLayerHigh, Audio: protocol.AudioStateOn}}
	eventually(t, func() bool {
		s, ok := spa.shareIn(snap.ID)
		return ok && slices.Equal(s.Watchers, watchers) && slices.Equal(sh.Snapshot().Watchers, watchers)
	}, func() string {
		s, _ := spa.shareIn(snap.ID)
		return fmt.Sprintf("watchers in room.state %+v, in the core's snapshot %+v; want bob alone", s.Watchers,
			sh.Snapshot().Watchers)
	})

	// After: the core leaves the room with its share.
	if err := sh.Stop(waitCtx(t)); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if end := app.waitDone(sh); end != (core.EndInfo{Reason: core.EndUser, ByThisApp: true}) {
		t.Errorf("the share ended with %+v, want user, by this app", end)
	}
	if app.core.ActiveShare() != nil {
		t.Error("ActiveShare after Stop is not nil")
	}
	eventually(t, func() bool {
		_, shared := spa.shareIn(snap.ID)
		return alone() && !shared
	}, func() string {
		return fmt.Sprintf("after the share, room.state = %+v, want alice with the SPA's connection only", spa.roomState())
	})
	// Never two participants for one user, and the core's connection only while it shared.
	for _, st := range spa.roomStates() {
		if n := len(participantsOf(st, spa.userID)); n > 1 {
			t.Errorf("room.state rev %d has alice %d times", st.Rev, n)
		}
	}
	spa.check()
	bob.check()
}

// TestDesktopSourceGone is D14: the engine ends the session with source_gone; the core sends share.stop (the share
// ends "stopped", not "left"), the share ends source-gone, and the core leaves the room.
func TestDesktopSourceGone(t *testing.T) {
	noLeaks(t)
	w := newWorld(t, servertest.Options{})
	app := w.desktop(w.member("alice"), appEngine(3))
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	coreID := app.core.Status().ConnectionID
	sh := app.start(defaultRoom(bob))
	snap := app.waitState(sh, core.StateLive)
	bob.waitLive(snap.ID)

	sess := app.engine.Session()
	if sess == nil {
		t.Fatal("no engine session while the share is live")
	}
	sess.Fail(&media.Error{Code: media.CodeSourceGone, Op: "next"})
	if end := app.waitDone(sh); end != (core.EndInfo{Reason: core.EndSourceGone, Media: media.CodeSourceGone}) {
		t.Errorf("the share ended with %+v, want source-gone", end)
	}
	eventually(t, func() bool {
		return slices.ContainsFunc(bob.roomEvents(), func(ev protocol.RoomEvent) bool {
			return ev.Kind == protocol.RoomEventKindShareStopped && ev.ShareID == snap.ID
		})
	}, func() string {
		return fmt.Sprintf("bob's room events %+v, want share.stopped of %s", bob.roomEvents(), snap.ID)
	})
	for _, ev := range bob.roomEvents() {
		if ev.Kind == protocol.RoomEventKindShareStopped && ev.ShareID == snap.ID && ev.Reason != protocol.EndReasonStopped {
			t.Errorf("share.stopped %+v, want reason stopped: the core sends share.stop", ev)
		}
	}
	eventually(t, func() bool {
		st := bob.roomState()
		_, shared := bob.shareIn(snap.ID)
		return !shared && !slices.ContainsFunc(st.Participants, func(p protocol.ParticipantInfo) bool {
			return slices.Contains(connIDs(p), coreID)
		})
	}, func() string {
		return fmt.Sprintf("bob's room.state %+v, want the core's connection gone", bob.roomState())
	})
	if app.core.ActiveShare() != nil || app.engine.Session() != nil {
		t.Errorf("after the end: active share %v, engine session %v; want neither", app.core.ActiveShare(),
			app.engine.Session())
	}
	if evs := app.shareEvents(); len(evs) == 0 || evs[len(evs)-1].End == nil ||
		evs[len(evs)-1].End.Reason != core.EndSourceGone {
		t.Errorf("EventShare snapshots %+v, want the last one ended source-gone", evs)
	}
	bob.check()
}

// TestDesktopStopOther is D15: a full-role connection of the user shares in the lounge; the core, in no room, stops
// it with StopOther, which returns once room.state no longer lists the share. The core is in no room afterwards, and
// presence never showed the user joining or leaving. With the core sharing in another room, StopOther is ErrBusy.
func TestDesktopStopOther(t *testing.T) {
	noLeaks(t)
	w := newWorld(t, servertest.Options{})
	alice := w.member("alice")
	tab := w.connect(alice, protocol.RoleFull) // a web tab of alice's
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	room := defaultRoom(bob)
	app := w.desktop(alice, appEngine(4))
	coreID := app.core.Status().ConnectionID
	sh := tab.publish(41, sfutest.LoopbackSettings())
	bob.waitLive(sh.id)
	eventsBefore := len(bob.roomEvents())
	statesBefore := len(bob.roomStates())

	if err := app.core.StopOther(waitCtx(t), room, sh.id); err != nil {
		t.Fatalf("StopOther: %v", err)
	}
	// StopOther returned once the core's room.state dropped the share; bob's follows.
	eventually(t, func() bool {
		_, shared := bob.shareIn(sh.id)
		return !shared
	}, func() string { return fmt.Sprintf("bob's room.state %+v, want %s gone", bob.roomState(), sh.id) })
	eventually(t, func() bool {
		return !slices.ContainsFunc(bob.roomState().Participants, func(p protocol.ParticipantInfo) bool {
			return slices.Contains(connIDs(p), coreID)
		})
	}, func() string {
		return fmt.Sprintf("bob's room.state %+v, want the core out of the room", bob.roomState())
	})
	for _, ev := range bob.roomEvents()[eventsBefore:] {
		if ev.UserID == tab.userID && ev.Kind != protocol.RoomEventKindShareStopped {
			t.Errorf("room.event %+v: alice's presence changed", ev)
		}
		if ev.Kind == protocol.RoomEventKindShareStopped && ev.ShareID == sh.id && ev.Reason != protocol.EndReasonStopped {
			t.Errorf("share.stopped %+v, want reason stopped", ev)
		}
	}
	for _, st := range bob.roomStates()[statesBefore:] {
		if n := len(participantsOf(st, tab.userID)); n != 1 {
			t.Errorf("room.state rev %d has alice %d times, want once", st.Rev, n)
		}
	}

	// Sharing in another room, the core can't reach the lounge.
	cinema := w.createRoom("Cinema")
	mine := app.start(cinema)
	app.waitState(mine, core.StateLive)
	if err := app.core.StopOther(waitCtx(t), room, "s_whatever"); !errors.Is(err, core.ErrBusy) {
		t.Errorf("StopOther while sharing in another room = %v, want ErrBusy", err)
	}
	if mine.Snapshot().State != core.StateLive {
		t.Errorf("the core's own share after the refused StopOther is %+v, want it live", mine.Snapshot())
	}
	bob.check()
}
