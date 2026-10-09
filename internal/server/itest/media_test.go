package itest

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// Cases 1 to 3 of 01 §19 "Integration": one server in off mode, Go clients on loopback UDP.

// TestJoinAndWatch is case 1: a member shares, another joins later and watches. Media arrives, and the first
// packet the server forwards to the viewer is an SPS: nothing leaves before the viewer's sub PC is ready, and then
// the stream starts where a decoder can (the DTLS-ready gate, S4 finding 1). What both clients are told on their
// sockets matches: the share's parameters, its state in room.state, who watches it, and what the viewer gets.
func TestJoinAndWatch(t *testing.T) {
	w := newWorld(t, servertest.Options{})
	alice := w.connect(w.member("alice"), protocol.RoleFull)
	sh := alice.publish(1, sfutest.LoopbackSettings())
	alice.waitLive(sh.id)
	// The share has been live for a while when the viewer comes: the stream's own first keyframe is long past, and
	// the next one is a minute away unless someone asks.
	eventually(t, func() bool { return layerStats(t, sh.pub, "f").Frames >= 30 },
		func() string { return fmt.Sprintf("the publisher's stats are %+v", sh.pub.Stats()) })
	before := layerStats(t, sh.pub, "f")

	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	viewer := bob.watch(sfutest.LoopbackSettings(), 0)
	// A new connection learns of the share from the room.state that follows its join.
	info := bob.waitLive(sh.id)
	if info.UserID != alice.userID || info.ConnectionID != alice.connectionID || info.Kind != protocol.ShareKindScreen ||
		info.Preset != protocol.PresetAuto || len(info.Watchers) != 0 {
		t.Errorf("the share in the viewer's room.state = %+v, want alice's screen share without watchers", info)
	}
	bob.subscribe(want(sh.id, true))
	video, audio := track(t, viewer, sh.id, webrtc.RTPCodecTypeVideo), track(t, viewer, sh.id, webrtc.RTPCodecTypeAudio)
	waitFrames(t, video, 0, "f", 30)
	waitPackets(t, audio, 50)
	bob.waitStatus(want(sh.id, true))

	// ---- the media ----
	vp := checkVideo(t, "video", video.Packets())
	if first := vp[0]; !first.KeyStart {
		t.Errorf("the first packet the viewer got (seq %d, NAL type %d) is no SPS", first.Seq, first.NAL)
	}
	if runs := layerRuns(vp); runs != "f" {
		t.Errorf("the viewer got the layers %q, want the full layer alone", runs)
	}
	if st := video.Stats(); st.Gaps != 0 || st.Lost != 0 || st.Duplicates != 0 {
		t.Errorf("video stats on loopback = %+v, want no gap", st)
	}
	ap := audio.Packets()
	for _, check := range []func([]sfutest.Packet) error{sfutest.CheckContinuous, sfutest.CheckAudioMarkers} {
		if err := check(ap); err != nil {
			t.Errorf("audio: %v", err)
		}
	}
	// The keyframe the viewer started on is one the SFU asked the publisher for: it did not wait for the GOP to end.
	if after := layerStats(t, sh.pub, "f"); after.PLIs+after.FIRs <= before.PLIs+before.FIRs || after.Keyframes <= before.Keyframes {
		t.Errorf("the publisher's full layer: %d keyframe requests and %d keyframes before the viewer, %d and %d after; want one more of each",
			before.PLIs+before.FIRs, before.Keyframes, after.PLIs+after.FIRs, after.Keyframes)
	}
	if st := layerStats(t, sh.pub, "q"); st.PLIs+st.FIRs != 0 {
		t.Errorf("the preview layer got %d keyframe requests, and nobody watches it", st.PLIs+st.FIRs)
	}

	// ---- the signaling ----
	// One sub offer made the subscription: the share's video and audio, bound by its tracks.
	offers := bob.subOffers()
	if len(offers) != 1 || offers[0].Gen != 1 || offers[0].Neg != 1 || len(offers[0].Tracks) != 2 {
		t.Fatalf("sub offers = %+v, want one (gen 1, neg 1) with two tracks", offers)
	}
	kinds := map[protocol.TrackKind]string{}
	for _, tr := range offers[0].Tracks {
		if tr.ShareID != sh.id {
			t.Errorf("the sub offer binds %+v, want tracks of %s", tr, sh.id)
		}
		kinds[tr.Kind] = tr.MID
	}
	if kinds[protocol.TrackKindVideo] != video.MID() || kinds[protocol.TrackKindAudio] != audio.MID() {
		t.Errorf("the sub offer's tracks %v, the viewer's video is on mid %q and its audio on %q", kinds, video.MID(), audio.MID())
	}
	// Everyone in the room sees who watches, the sharer included.
	watching := []protocol.Watcher{{UserID: bob.userID, Video: protocol.VideoLayerHigh, Audio: protocol.AudioStateOn}}
	for _, c := range []*client{alice, bob} {
		eventually(t, func() bool {
			s, ok := c.shareIn(sh.id)
			return ok && slices.Equal(s.Watchers, watching)
		}, func() string {
			s, _ := c.shareIn(sh.id)
			return fmt.Sprintf("%s sees the watchers %+v, want %+v", c.name, s.Watchers, watching)
		})
	}
	// The sharer was in the room when its share went live, the viewer was not: room.event is for those present.
	started := func(c *client) int {
		return len(slices.DeleteFunc(c.roomEvents(), func(ev protocol.RoomEvent) bool {
			return ev.Kind != protocol.RoomEventKindShareStarted || ev.ShareID != sh.id || ev.UserID != alice.userID
		}))
	}
	if started(alice) != 1 || started(bob) != 0 {
		t.Errorf("share.started events: %d for the sharer, %d for the later viewer; want 1 and 0", started(alice), started(bob))
	}
	// What the viewer was told about its subscription never claimed more than it got, and gave no reason but
	// "waiting" on the way there.
	for _, st := range bob.statuses(sh.id) {
		if st.RequestedVideo != protocol.VideoLayerHigh || (st.Reason != "" && st.Reason != protocol.StatusReasonWaiting) {
			t.Errorf("subscribe.status %+v", st)
		}
	}
	alice.check()
	bob.check()
}

// switchTime checks the packets a viewer got since it asked for another layer at asked: the first frame of that
// layer starts on an SPS and nothing of another layer follows it. It returns how long the switch took.
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
	return frames[first].First.Sub(asked)
}

// TestLayers is case 2: subscribe.update high → low → high. Each switch happens on a keyframe of the new layer,
// which the SFU asks the publisher for, and the viewer's stream stays one continuous sequence through both:
// sequence numbers without a gap, timestamps on the capture timeline. A change of quality never renegotiates, and
// the audio never notices.
func TestLayers(t *testing.T) {
	w := newWorld(t, servertest.Options{})
	alice := w.connect(w.member("alice"), protocol.RoleFull)
	sh := alice.publish(2, sfutest.LoopbackSettings())
	alice.waitLive(sh.id)
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	viewer := bob.watch(sfutest.LoopbackSettings(), 0)
	bob.waitLive(sh.id)
	high := want(sh.id, true)
	low := protocol.SubscriptionWant{ShareID: sh.id, Video: protocol.VideoLayerLow, Audio: protocol.AudioStateOn}

	// ---- high ----
	bob.subscribe(high)
	video, audio := track(t, viewer, sh.id, webrtc.RTPCodecTypeVideo), track(t, viewer, sh.id, webrtc.RTPCodecTypeAudio)
	// A second and a half: by then the publisher has sent a sender report for each layer, which a switch aligns
	// the timestamps with.
	waitFrames(t, video, 0, "f", 45)
	eventually(t, func() bool { return layerStats(t, sh.pub, "f").SRs >= 1 && layerStats(t, sh.pub, "q").SRs >= 1 },
		func() string { return fmt.Sprintf("the publisher's stats are %+v", sh.pub.Stats()) })
	bob.waitStatus(high)
	if st := layerStats(t, sh.pub, "q"); st.PLIs+st.FIRs != 0 || st.Keyframes != 1 {
		t.Errorf("the preview layer before anybody asked for it: %d keyframe requests, %d keyframes", st.PLIs+st.FIRs, st.Keyframes)
	}

	// ---- high → low ----
	mark, asked := len(video.Packets()), time.Now()
	bob.subscribe(low)
	waitFrames(t, video, mark, "q", 8)
	bob.waitStatus(low)
	toLow := switchTime(t, video.Packets()[mark:], "q", asked)
	if st := layerStats(t, sh.pub, "q"); st.PLIs+st.FIRs < 1 || st.Keyframes < 2 {
		t.Errorf("the preview layer after the switch: %d keyframe requests, %d keyframes; want the request and its keyframe",
			st.PLIs+st.FIRs, st.Keyframes)
	}

	// ---- low → high ----
	keysBefore := layerStats(t, sh.pub, "f").Keyframes
	mark, asked = len(video.Packets()), time.Now()
	bob.subscribe(high)
	waitFrames(t, video, mark, "f", 15)
	bob.waitStatus(high)
	toHigh := switchTime(t, video.Packets()[mark:], "f", asked)
	if keys := layerStats(t, sh.pub, "f").Keyframes; keys <= keysBefore {
		t.Errorf("the full layer made no keyframe for the switch back (%d before, %d after)", keysBefore, keys)
	}
	t.Logf("high → low took %v, low → high %v (S4: 42–106 ms)", toLow.Round(time.Millisecond), toHigh.Round(time.Millisecond))
	// The publisher's GOP is a minute: a switch that waited for it would take that long. A second is what the SFU's
	// own test allows; the hub's part of the way is one message.
	for name, took := range map[string]time.Duration{"high → low": toLow, "low → high": toHigh} {
		if took > 2*time.Second {
			t.Errorf("%s took %v, want well under 2 s", name, took)
		}
	}

	// ---- the whole stream ----
	vp := checkVideo(t, "video", video.Packets())
	if runs := layerRuns(vp); runs != "f q f" {
		t.Errorf("the viewer got the layers %q, want f, then q, then f again", runs)
	}
	checkSwitchTimestamps(t, vp)
	if st := video.Stats(); st.Gaps != 0 || st.Lost != 0 || st.Duplicates != 0 {
		t.Errorf("video stats on loopback = %+v, want no gap", st)
	}
	ap := audio.Packets()
	if err := sfutest.CheckContinuous(ap); err != nil {
		t.Errorf("audio: %v", err)
	}
	if runs := audioRuns(t, "audio", ap); len(runs) != 1 {
		t.Errorf("the audio has %d runs, want one: a change of the video layer is none of its business", len(runs))
	}
	// Quality never renegotiates.
	if n := len(bob.subOffers()); n != 1 {
		t.Errorf("%d sub offers, want 1", n)
	}
	// The viewer heard of each change once it had happened: high, low, high, with "waiting" as the only reason on
	// the way.
	var heard []protocol.VideoLayer
	for _, st := range bob.statuses(sh.id) {
		if st.Reason != "" && st.Reason != protocol.StatusReasonWaiting {
			t.Errorf("subscribe.status %+v has a reason other than waiting", st)
		}
		if st.Video == protocol.VideoLayerOff {
			continue // before the first frame
		}
		if len(heard) == 0 || heard[len(heard)-1] != st.Video {
			heard = append(heard, st.Video)
		}
	}
	if !slices.Equal(heard, []protocol.VideoLayer{protocol.VideoLayerHigh, protocol.VideoLayerLow, protocol.VideoLayerHigh}) {
		t.Errorf("the viewer heard the forwarded layer go %v, want high, low, high (statuses %+v)", heard, bob.statuses(sh.id))
	}
	alice.check()
	bob.check()
}

// TestAudioFollowsFocus is case 3: a viewer of two shares with the focus on A gets A's audio and none of B's.
// One subscribe.update moves the focus, and the audio with it: A's stops and B's starts in the same step, with no
// time in which both run or neither does.
//
// The spec asks for that "sampled every 20 ms". A client can sample only what arrives: a stream runs from its
// first packet of a run to 20 ms after the last one, which is the audio that packet carries. Two free-running
// publishers are up to one packet interval apart, so a perfect switch shows an overlap or a gap of up to 20 ms
// either way, and a busy machine adds its own. The test moves the focus five times. Every move must be within a
// quarter of a second, which no switch that waits for a negotiation or a keyframe could do, and the median within
// 60 ms, three samples.
func TestAudioFollowsFocus(t *testing.T) {
	const (
		moves     = 5
		worst     = 250 * time.Millisecond
		typical   = 60 * time.Millisecond
		packetLen = 20 * time.Millisecond
	)
	w := newWorld(t, servertest.Options{})
	alice := w.connect(w.member("alice"), protocol.RoleFull)
	carol := w.connect(w.member("carol"), protocol.RoleFull)
	a, b := alice.publish(31, sfutest.LoopbackSettings()), carol.publish(32, sfutest.LoopbackSettings())
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	viewer := bob.watch(sfutest.LoopbackSettings(), 0)
	bob.waitLive(a.id)
	bob.waitLive(b.id)
	focus := func(onA bool) {
		t.Helper()
		bob.subscribe(want(a.id, onA), want(b.id, !onA))
		bob.waitStatus(want(a.id, onA))
		bob.waitStatus(want(b.id, !onA))
	}

	// ---- the focus is on A: A's audio, and only A's ----
	focus(true)
	videoA, videoB := track(t, viewer, a.id, webrtc.RTPCodecTypeVideo), track(t, viewer, b.id, webrtc.RTPCodecTypeVideo)
	audioA := track(t, viewer, a.id, webrtc.RTPCodecTypeAudio)
	waitFrames(t, videoA, 0, "f", 30)
	waitFrames(t, videoB, 0, "q", 15)
	waitPackets(t, audioA, 50)
	time.Sleep(400 * time.Millisecond)
	if hasTrack(viewer, b.id, webrtc.RTPCodecTypeAudio) {
		t.Errorf("the viewer got audio of %s while the focus was on %s", b.id, a.id)
	}

	// ---- the focus moves, one subscribe.update each time: B, A, B, A, B ----
	onA := true
	var audioB *sfutest.Recorder
	for range moves {
		onA = !onA
		focus(onA)
		if onA {
			waitPackets(t, audioA, 50)
			continue
		}
		if audioB == nil {
			audioB = track(t, viewer, b.id, webrtc.RTPCodecTypeAudio)
		}
		waitPackets(t, audioB, 50)
	}
	time.Sleep(100 * time.Millisecond) // what was on its way has arrived

	// ---- what the viewer got ----
	// Each audio is one stream whose sequence numbers go on across its pauses, in three runs: A's before the
	// first, third and fifth move, B's after them.
	runsA := audioRuns(t, "audio of "+a.id, audioA.Packets())
	runsB := audioRuns(t, "audio of "+b.id, audioB.Packets())
	if len(runsA) != 3 || len(runsB) != 3 {
		t.Fatalf("the audio of A has %d runs and that of B %d; want 3 each for %d moves of the focus", len(runsA), len(runsB), moves)
	}
	for name, r := range map[string]*sfutest.Recorder{a.id: audioA, b.id: audioB} {
		if err := sfutest.CheckContinuous(r.Packets()); err != nil {
			t.Errorf("audio of %s: %v", name, err)
		}
		if st := r.Stats(); st.Gaps != 0 || st.Duplicates != 0 {
			t.Errorf("audio of %s: stats %+v", name, st)
		}
	}
	// The runs take turns: A, B, A, B, A, B. At each change, how long did both run (positive) or neither
	// (negative)?
	turns := []audioRun{runsA[0], runsB[0], runsA[1], runsB[1], runsA[2], runsB[2]}
	var overlaps, sizes []time.Duration
	for i := 1; i < len(turns); i++ {
		overlap := turns[i-1].last.Add(packetLen).Sub(turns[i].first)
		overlaps = append(overlaps, overlap)
		sizes = append(sizes, overlap.Abs())
		if overlap.Abs() > worst {
			t.Errorf("move %d of the focus: the two audio streams overlapped by %v (negative: a gap), want within ±%v", i, overlap, worst)
		}
	}
	slices.Sort(sizes)
	if median := sizes[len(sizes)/2]; median > typical {
		t.Errorf("the audio streams overlapped or left a gap of %v at the median move (all: %v), want within %v", median, overlaps, typical)
	}
	// The same on the spec's grid: every 20 ms, which of the two runs?
	running := func(runs []audioRun, at time.Time) bool {
		return slices.ContainsFunc(runs, func(r audioRun) bool { return !at.Before(r.first) && at.Before(r.last.Add(packetLen)) })
	}
	var samples, both, neither int
	for at := turns[0].first; at.Before(turns[len(turns)-1].last); at = at.Add(packetLen) {
		ra, rb := running(runsA, at), running(runsB, at)
		samples++
		switch {
		case ra && rb:
			both++
		case !ra && !rb:
			neither++
		}
	}
	t.Logf("%d moves of the focus: overlap at each %v; of %d samples 20 ms apart, both audio streams ran in %d and neither in %d",
		moves, overlaps, samples, both, neither)
	if limit := moves * int(worst/packetLen); both+neither > limit {
		t.Errorf("both or neither audio stream ran in %d of %d samples, want at most %d around the %d moves", both+neither, samples, limit, moves)
	}

	// The video followed the focus too, each stream continuous through its switches.
	for name, tc := range map[string]struct {
		r    *sfutest.Recorder
		runs string
	}{a.id: {videoA, "f q f q f q"}, b.id: {videoB, "q f q f q f"}} {
		vp := checkVideo(t, "video of "+name, tc.r.Packets())
		if runs := layerRuns(vp); runs != tc.runs {
			t.Errorf("video of %s: layers %q, want %q", name, runs, tc.runs)
		}
		checkSwitchTimestamps(t, vp)
	}
	// Moving the focus never renegotiates: the one offer that made the two subscriptions is all there was, and
	// the viewer has the four tracks of it.
	if n := len(bob.subOffers()); n != 1 {
		t.Errorf("%d sub offers, want 1", n)
	}
	if n := len(viewer.Recorders()); n != 4 {
		t.Errorf("the viewer has %d tracks, want two video and two audio", n)
	}
	// The viewer was told of every change of the audio, and never that B's ran while the focus was on A.
	on, off := protocol.AudioStateOn, protocol.AudioStateOff
	for name, wantAudio := range map[string][]protocol.AudioState{
		a.id: {on, off, on, off, on, off},
		b.id: {off, on, off, on, off, on},
	} {
		var heard []protocol.AudioState
		for _, st := range bob.statuses(name) {
			if st.Reason != "" && st.Reason != protocol.StatusReasonWaiting {
				t.Errorf("subscribe.status %+v has a reason other than waiting", st)
			}
			if len(heard) == 0 || heard[len(heard)-1] != st.Audio {
				heard = append(heard, st.Audio)
			}
		}
		if len(heard) > 0 && heard[0] == off && wantAudio[0] == on {
			heard = heard[1:] // the video started before the audio
		}
		if !slices.Equal(heard, wantAudio) {
			t.Errorf("the viewer heard the audio of %s go %v, want %v (statuses %+v)", name, heard, wantAudio, bob.statuses(name))
		}
	}
	// And so was everyone in the room: the watchers of each share say who has the audio now.
	eventually(t, func() bool {
		sa, okA := alice.shareIn(a.id)
		sb, okB := alice.shareIn(b.id)
		return okA && okB &&
			slices.Equal(sa.Watchers, []protocol.Watcher{{UserID: bob.userID, Video: protocol.VideoLayerLow, Audio: off}}) &&
			slices.Equal(sb.Watchers, []protocol.Watcher{{UserID: bob.userID, Video: protocol.VideoLayerHigh, Audio: on}})
	}, func() string { return fmt.Sprintf("the sharer's room.state is %+v", alice.roomState().Shares) })
	for _, c := range []*client{alice, carol, bob} {
		c.check()
	}
}
