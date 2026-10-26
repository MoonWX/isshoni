package itest

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/core"
	"github.com/MoonWX/isshoni/internal/client/publish"
	"github.com/MoonWX/isshoni/internal/client/signal"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// Cases D7, D8 and D11–D13 of docs/m2/01-client-core.md §14: the client core follows the server's quality hints (the
// codec switch, layer pausing), pauses and resumes (by itself and from a web tab of its user), takes a preset
// changed elsewhere, and shows the engine's encoder fallback. Plus README WA38's hand-over to WA46: a share published
// again after a server restart keeps its preset and its pause.

// Bounds of these cases.
const (
	// pauseQuiet: no media reaches a viewer later than this after SetPaused(true) is called (D11).
	pauseQuiet = 50 * time.Millisecond
	// resumeKeyframe: a viewer gets a keyframe within this of SetPaused(false) (D11).
	resumeKeyframe = 500 * time.Millisecond
	// layerResume: a full layer that was paused for want of viewers is back at the viewer within this of its
	// request for high (D8).
	layerResume = time.Second
	// codecHold is how long the room's codec policy stays Constrained Baseline after the last viewer that needs it
	// left (the SFU's codecPolicyHold, 02 §8.3), and codecSlack what the SFU's once-a-second look and the re-offer
	// add (D7).
	codecHold  = time.Minute
	codecSlack = 3 * time.Second
)

// mediaPackets returns the packets a viewer got that carry media of the share's publisher: no repair, no
// duplicate, no empty payload.
func mediaPackets(r *sfutest.Recorder) []sfutest.Packet {
	var out []sfutest.Packet
	for _, p := range r.Packets() {
		if !p.RTX && !p.Dup && p.Size > 0 {
			out = append(out, p)
		}
	}
	return out
}

// lastArrival returns when the newest media packet of a recorder arrived.
func lastArrival(r *sfutest.Recorder) time.Time {
	var last time.Time
	for _, p := range mediaPackets(r) {
		if p.Arrival.After(last) {
			last = p.Arrival
		}
	}
	return last
}

// callsAfter returns the engine session's calls after the first n whose op is one of ops.
func callsAfter(sess *fake.Session, n int, ops ...string) []fake.Call {
	var out []fake.Call
	for _, c := range sess.Calls()[n:] {
		if slices.Contains(ops, c.Op) {
			out = append(out, c)
		}
	}
	return out
}

// renderCalls renders engine calls as "op layer value, …".
func renderCalls(calls []fake.Call) string {
	var out []string
	for _, c := range calls {
		switch c.Op {
		case "bitrate":
			out = append(out, fmt.Sprintf("bitrate %s %d", c.Layer, c.Bps))
		case "active":
			out = append(out, fmt.Sprintf("active %s %t", c.Layer, c.On))
		case "paused", "audio_muted":
			out = append(out, fmt.Sprintf("%s %t", c.Op, c.On))
		case "reconfigure":
			out = append(out, fmt.Sprintf("reconfigure profile=%q preset=%q", c.Reconf.Profile, c.Reconf.Preset))
		default:
			out = append(out, c.Op)
		}
	}
	return strings.Join(out, ", ")
}

// waitPauseCalls waits until the session's pause and mute calls after the first n render as want.
func waitPauseCalls(t *testing.T, sess *fake.Session, n int, want string) {
	t.Helper()
	eventually(t, func() bool { return renderCalls(callsAfter(sess, n, "paused", "audio_muted")) == want },
		func() string {
			return fmt.Sprintf("the engine's pause calls are %q, want %q",
				renderCalls(callsAfter(sess, n, "paused", "audio_muted")), want)
		})
}

// waitShareStatus waits until the client's room.state shows the share with a status.
func waitShareStatus(c *client, shareID string, status protocol.ShareStatus) protocol.ShareInfo {
	c.t.Helper()
	var info protocol.ShareInfo
	eventually(c.t, func() bool {
		var ok bool
		info, ok = c.shareIn(shareID)
		return ok && info.Status == status
	}, func() string {
		return fmt.Sprintf("%s: share %s in room.state is %+v, want it %s", c.name, shareID, info, status)
	})
	return info
}

// waitLong is eventually with a timeout of its own, for D7's minute.
func waitLong(t *testing.T, timeout time.Duration, cond func() bool, what func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v: %s", timeout, what())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// notices returns the app's EventNotice notices so far.
func (d *desktopApp) notices() []core.Notice {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []core.Notice
	for _, ev := range d.events {
		if ev.Kind == core.EventNotice && ev.Notice != nil {
			out = append(out, *ev.Notice)
		}
	}
	return out
}

// webTab is a web tab of a member's: a full-role connection in the default room that asks for share.pause, as 03's
// SPA does, and sends share.update for its user's shares. It only drains its notifications.
type webTab struct {
	t    *testing.T
	sig  *signal.Client
	done chan struct{}
}

// webTab opens a web tab for m and joins the default room. It closes when the test ends.
func (w *world) webTab(m *member) *webTab {
	w.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	sig, err := signal.Dial(ctx, signal.Options{
		URL:        w.srv.WSURL,
		HTTPClient: &http.Client{Jar: m.http.Jar, Transport: w.srv.Client.Transport},
		Client:     protocol.ClientInfo{Kind: protocol.ClientKindWeb, Version: "0.2.0", OS: protocol.ClientOSLinux},
		Role:       protocol.RoleFull,
		Caps:       caps,
		Features:   []protocol.Feature{protocol.FeatureSharePause},
	})
	if err != nil {
		w.t.Fatalf("%s's tab: dial: %v", m.name, err)
	}
	tab := &webTab{t: w.t, sig: sig, done: make(chan struct{})}
	go func() {
		defer close(tab.done)
		for range sig.Events() { // a signaling client whose Events nobody drains stops reading its socket
		}
	}()
	w.t.Cleanup(func() {
		_ = sig.Close()
		<-tab.done
	})
	if !slices.Contains(sig.Welcome().Features, protocol.FeatureSharePause) {
		w.t.Fatalf("%s's tab: the welcome offers %v, want share.pause", m.name, sig.Welcome().Features)
	}
	var joined protocol.RoomJoinResult
	tab.request(protocol.MessageTypeRoomJoin, protocol.RoomJoin{RoomID: sig.Welcome().DefaultRoomID}, &joined)
	return tab
}

// request sends a request and decodes its reply; an error fails the test.
func (tab *webTab) request(typ protocol.MessageType, data, result any) {
	tab.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	if err := tab.sig.Request(ctx, typ, data, result); err != nil {
		tab.t.Fatalf("the tab's %s: %v", typ, err)
	}
}

// watchCB gives the client a Viewer that decodes Constrained Baseline (and Baseline) only, as Firefox with
// OpenH264 does, with the sub PC's recovery of watchWith.
func (c *client) watchCB() *sfutest.Viewer {
	c.t.Helper()
	v, err := sfutest.NewViewer(sfutest.ViewerOptions{Settings: sfutest.LoopbackSettings(),
		H264: []string{"42e01f", "42001f"}})
	if err != nil {
		c.t.Fatal(err)
	}
	stop, stopped := make(chan struct{}), make(chan struct{})
	c.t.Cleanup(func() {
		close(stop)
		<-stopped
		if err := v.Close(); err != nil {
			c.t.Errorf("%s: Viewer.Close: %v", c.name, err)
		}
	})
	c.mu.Lock()
	c.viewer = v
	c.mu.Unlock()
	go func() {
		defer close(stopped)
		c.subDriver(stop)
	}()
	return v
}

// watchedShare starts a share of the app in the default room and has bob watch it, the full layer in focus with its
// audio, until both arrive.
func watchedShare(t *testing.T, app *desktopApp, bob *client, viewer *sfutest.Viewer) (
	sh *core.Share, snap core.ShareSnapshot, video, audio *sfutest.Recorder) {
	t.Helper()
	sh = app.start(defaultRoom(bob))
	snap = app.waitState(sh, core.StateLive)
	bob.waitLive(snap.ID)
	bob.subscribe(want(snap.ID, true))
	video, audio = track(t, viewer, snap.ID, webrtc.RTPCodecTypeVideo), track(t, viewer, snap.ID, webrtc.RTPCodecTypeAudio)
	waitFrames(t, video, 0, "f", 30)
	waitPackets(t, audio, 50)
	bob.waitStatus(want(snap.ID, true))
	return sh, snap, video, audio
}

// TestDesktopCodecSwitch is D7: a viewer that decodes only Constrained Baseline joins the room; the SFU's codec
// policy drops to CB and the core gets quality.hint{codec: h264/42e0}: the Publisher offers again with CB first
// (gen 1, neg 2), and on the answer the engine re-initializes in CB (Session.Reconfigure{Profile: 42e0}). The viewer
// gets the CB video from the re-init's keyframe, whose SPS is Constrained Baseline (profile_idc 66, constraint_set1),
// not only its payload type. A minute after it left the policy is High again, and the core switches back the same
// way: the first keyframe after that re-init has a High SPS. It takes over a minute, so -short skips it.
func TestDesktopCodecSwitch(t *testing.T) {
	if testing.Short() {
		t.Skip("D7 waits for the SFU's minute of codec hold")
	}
	noLeaks(t)
	w := newWorld(t, servertest.Options{})
	app := w.desktop(w.member("alice"), appEngine(71))
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	bobView := bob.watch(sfutest.LoopbackSettings(), 0)
	sh, snap, _, _ := watchedShare(t, app, bob, bobView)
	sess := app.engine.Session()
	n := len(sess.Calls())

	// ---- a CB-only viewer joins ----
	cbCaps := protocol.Caps{Decode: []protocol.CodecKey{protocol.CodecH264ConstrainedBaseline, protocol.CodecOpus}}
	dan := w.connectWith(w.member("dan"), protocol.RoleViewer, connOptions{caps: &cbCaps})
	danView := dan.watchCB()
	var reconf fake.Call
	eventually(t, func() bool {
		calls := callsAfter(sess, n, "reconfigure")
		if len(calls) == 0 {
			return false
		}
		reconf = calls[0]
		return true
	}, func() string { return "the engine was never reconfigured after the CB-only viewer joined" })
	if reconf.Reconf.Profile != "42e0" || reconf.Reconf.Preset != "" || len(reconf.Reconf.Layers) != 0 {
		t.Errorf("the engine's reconfigure is %+v, want Profile 42e0 alone", reconf.Reconf)
	}
	if !strings.Contains(app.logs.String(), `msg="pub offer" gen=1 neg=2 ice_restart=false`) {
		t.Error("the core's log has no re-offer (gen 1, neg 2) for the codec switch")
	}
	eventually(t, func() bool { return layerOf(sh.Snapshot(), "f").Profile == "42e0" },
		func() string { return fmt.Sprintf("the share's snapshot is %+v, want f in 42e0", sh.Snapshot()) })
	dan.waitLiveIn(snap.ID, protocol.CodecH264ConstrainedBaseline)

	// ---- the CB viewer watches the CB video, from the re-init's keyframe ----
	dan.subscribe(want(snap.ID, true))
	cb := track(t, danView, snap.ID, webrtc.RTPCodecTypeVideo)
	waitFrames(t, cb, 0, "f", 30)
	if fmtp := cb.Codec().SDPFmtpLine; !strings.Contains(fmtp, "profile-level-id=42e01f") {
		t.Errorf("the CB viewer's video is %q, want Constrained Baseline", fmtp)
	}
	vp := checkVideo(t, "the CB viewer's video", cb.Packets())
	frames := sfutest.Frames(vp)
	if len(frames) == 0 || !frames[0].KeyStart || !frames[0].HasMark {
		t.Fatalf("the CB viewer's first frame is %+v, want a keyframe", frames[0])
	}
	if captured := sess.Origin().Add(time.Duration(frames[0].Mark.CaptureNS)); captured.Before(reconf.At.Add(-100 * time.Millisecond)) {
		t.Errorf("the CB viewer's first frame was captured %v before the engine's re-init: it isn't the re-init's",
			reconf.At.Sub(captured))
	}
	// The bitstream is CB too, not only the payload type: the keyframe's SPS says Constrained Baseline.
	full := layerOf(sh.Snapshot(), "f")
	if sps := keyframeSPS(t, vp, frames[0], full.Width, full.Height); sps[1] != spsBaseline || sps[2]&spsConstraintSet1 == 0 {
		t.Errorf("the CB viewer's first keyframe has the SPS % x, want profile_idc %d with constraint_set1", sps,
			spsBaseline)
	}
	dan.waitStatus(want(snap.ID, true))
	// Out of the room, dan's sub PC recovery asks for what a room member may (not_in_room): checked before it leaves.
	dan.check()

	// ---- it leaves: High again a minute later ----
	m := len(sess.Calls())
	dan.request(protocol.MessageTypeRoomLeave, protocol.Empty{}, nil)
	left := time.Now()
	waitLong(t, codecHold+codecSlack+10*time.Second, func() bool { return len(callsAfter(sess, m, "reconfigure")) > 0 },
		func() string { return "the engine was never reconfigured back to High" })
	back := callsAfter(sess, m, "reconfigure")[0]
	took := back.At.Sub(left)
	t.Logf("back to High %v after the CB viewer left", took.Round(time.Millisecond))
	if back.Reconf.Profile != "6400" || took < codecHold-time.Second || took > codecHold+codecSlack {
		t.Errorf("the engine's reconfigure %+v came %v after the CB viewer left, want Profile 6400 after %v (+%v)",
			back.Reconf, took.Round(time.Millisecond), codecHold, codecSlack)
	}
	if !strings.Contains(app.logs.String(), `msg="pub offer" gen=1 neg=3 ice_restart=false`) {
		t.Error("the core's log has no re-offer (gen 1, neg 3) for the switch back")
	}
	eventually(t, func() bool { return layerOf(sh.Snapshot(), "f").Profile == "6400" },
		func() string { return fmt.Sprintf("the share's snapshot is %+v, want f in 6400", sh.Snapshot()) })
	bob.waitLiveIn(snap.ID, protocol.CodecH264High)
	if got := sh.Snapshot().ID; got != snap.ID {
		t.Errorf("the share's id is %s, want %s: a codec switch keeps the share", got, snap.ID)
	}
	// bob's first keyframe of f captured after the re-init (every frame of the old encoder was captured before it)
	// carries a High SPS again.
	var key sfutest.Frame
	var keyPkts []sfutest.Packet
	eventually(t, func() bool {
		key, keyPkts = firstKeyframeAfter(bobView, snap.ID, sess.Origin(), back.At)
		return keyPkts != nil
	}, func() string { return "bob got no keyframe of f captured after the switch back" })
	if sps := keyframeSPS(t, keyPkts, key, full.Width, full.Height); sps[1] != spsHigh {
		t.Errorf("bob's first keyframe after the switch back has the SPS % x, want profile_idc %d", sps, spsHigh)
	}
	// bob watched all along (on whichever sub PC the SFU gave each profile), and still does.
	before := shareFrames(bobView, snap.ID)
	eventually(t, func() bool { return shareFrames(bobView, snap.ID) >= before+30 },
		func() string {
			return fmt.Sprintf("bob has %d frames of f, want 30 more than %d", shareFrames(bobView, snap.ID), before)
		})
	bob.waitStatus(want(snap.ID, true))
	bob.check()
}

// What D7 reads from an SPS (H.264 7.3.2.1.1; RFC 6184 for the RTP payload).
const (
	nalSPS            = 7
	nalSTAPA          = 24
	spsBaseline       = 66   // profile_idc of (Constrained) Baseline
	spsHigh           = 100  // profile_idc of High
	spsConstraintSet1 = 0x40 // constraint_set1_flag: Constrained Baseline with profile_idc 66
)

// keyframeSPS returns the SPS that a viewer's keyframe carried (its NAL header byte first, then profile_idc and the
// constraint flags): key is the keyframe, pkts the packets of its track, and width and height the layer's encoded
// size. A Recorder keeps a hash of each payload, not the payload, so the keyframe's STAP-A (its SPS and PPS) is
// matched against the ones the fake engine makes for a layer of that size and of the track's frame rate (which sets
// the level) in each engine profile, packetized as the Publisher does (pion's H264Payloader at publish.PayloadMTU);
// the SPS is read from the STAP-A that matches. It fails the test when none does.
func keyframeSPS(t *testing.T, pkts []sfutest.Packet, key sfutest.Frame, width, height int) []byte {
	t.Helper()
	i := slices.IndexFunc(pkts, func(p sfutest.Packet) bool { return p.TS == key.TS && p.KeyStart && !p.Dup })
	if i < 0 {
		t.Fatalf("the keyframe %+v has no packet with its SPS", key)
	}
	fps := frameRate(pkts, key.Mark.RID)
	for _, profile := range []string{publish.ConstrainedBaselineProfile, publish.HighProfile} {
		src, err := fake.New(fake.Config{Profile: profile, FlashEvery: -1, Layers: []fake.VideoLayer{
			{RID: key.Mark.RID, Width: width, Height: height, FPS: fps, Bitrate: 1_000_000}}})
		if err != nil {
			t.Fatalf("a fake layer %dx%d@%d in %s: %v", width, height, fps, profile, err)
		}
		au, err := src.Next(context.Background()) // the first frame, due at once, is a keyframe
		_ = src.Close()
		if err != nil || !au.Keyframe {
			t.Fatalf("the fake layer's first frame: %+v, %v", au, err)
		}
		stap := (&codecs.H264Payloader{}).Payload(publish.PayloadMTU, au.Data)[0]
		if sfutest.PayloadHash(stap) != pkts[i].Hash {
			continue
		}
		if sps := stapNAL(stap, nalSPS); len(sps) >= 3 {
			return sps
		}
		t.Fatalf("the fake's STAP-A % x has no SPS", stap)
	}
	t.Fatalf("the keyframe's STAP-A (%d bytes) is neither the CB nor the High one of a %dx%d@%d layer",
		pkts[i].Size, width, height, fps)
	return nil
}

// frameRate returns the frame rate of a track's layer rid, from the capture times of two consecutive frames; 0 when
// it has none.
func frameRate(pkts []sfutest.Packet, rid string) int {
	var prev *sfutest.Frame
	frames := sfutest.Frames(pkts)
	for i := range frames {
		f := &frames[i]
		if !f.HasMark || f.Mark.RID != rid {
			continue
		}
		if prev != nil && f.Mark.Frame == prev.Mark.Frame+1 && f.Mark.CaptureNS > prev.Mark.CaptureNS {
			return int(math.Round(float64(time.Second) / float64(f.Mark.CaptureNS-prev.Mark.CaptureNS)))
		}
		prev = f
	}
	return 0
}

// stapNAL returns the NAL unit of type typ in a STAP-A payload (or the payload itself when it is that NAL unit), nil
// when it has none.
func stapNAL(payload []byte, typ byte) []byte {
	switch {
	case len(payload) == 0:
		return nil
	case payload[0]&0x1f == typ:
		return payload
	case payload[0]&0x1f != nalSTAPA:
		return nil
	}
	for i := 1; i+2 <= len(payload); {
		size := int(payload[i])<<8 | int(payload[i+1])
		i += 2
		if size == 0 || i+size > len(payload) {
			return nil
		}
		if payload[i]&0x1f == typ {
			return payload[i : i+size]
		}
		i += size
	}
	return nil
}

// firstKeyframeAfter returns the earliest captured keyframe of f, over a Viewer's video tracks of a share, whose
// capture (the engine session's origin plus its Marker's CaptureNS) is not before at, with the packets of its track;
// nil packets when there is none.
func firstKeyframeAfter(v *sfutest.Viewer, shareID string, origin, at time.Time) (sfutest.Frame, []sfutest.Packet) {
	var (
		key     sfutest.Frame
		keyPkts []sfutest.Packet
	)
	for _, r := range v.Recorders() {
		if r.Kind() != webrtc.RTPCodecTypeVideo || r.StreamID() != shareID {
			continue
		}
		pkts := r.Packets()
		for _, f := range sfutest.Frames(sfutest.WithoutPartialTail(pkts)) {
			if !f.KeyStart || !f.HasMark || f.Mark.RID != "f" || origin.Add(time.Duration(f.Mark.CaptureNS)).Before(at) {
				continue
			}
			if keyPkts == nil || f.Mark.CaptureNS < key.Mark.CaptureNS {
				key, keyPkts = f, pkts
			}
		}
	}
	return key, keyPkts
}

// shareFrames counts the whole frames of f a Viewer got of a share, over all its video tracks of it.
func shareFrames(v *sfutest.Viewer, shareID string) int {
	n := 0
	for _, r := range v.Recorders() {
		if r.Kind() == webrtc.RTPCodecTypeVideo && r.StreamID() == shareID {
			n += layerFrames(r.Packets(), "f")
		}
	}
	return n
}

// TestDesktopLayerPausing is D8: nobody asks for the full layer for 10 s, so the SFU pauses it: the core turns it off
// (Session.SetLayerActive(f, false)) and nothing of it is sent. A viewer's request for high resumes it, with its
// target set in the same step (SetBitrate(f, target), SetLayerActive(f, true)), and the viewer gets its keyframe
// within a second.
func TestDesktopLayerPausing(t *testing.T) {
	noLeaks(t)
	w := newWorld(t, servertest.Options{})
	app := w.desktop(w.member("alice"), appEngine(72))
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	viewer := bob.watch(sfutest.LoopbackSettings(), 0)
	sh := app.start(defaultRoom(bob))
	snap := app.waitState(sh, core.StateLive)
	bob.waitLive(snap.ID)
	bob.subscribe(want(snap.ID, false)) // the preview alone: nobody asks for high
	video := track(t, viewer, snap.ID, webrtc.RTPCodecTypeVideo)
	waitFrames(t, video, 0, "q", 15)
	sess := app.engine.Session()

	// ---- f paused ----
	eventually(t, func() bool {
		return slices.ContainsFunc(callsAfter(sess, 0, "active"), func(c fake.Call) bool { return c.Layer == "f" && !c.On })
	}, func() string {
		return fmt.Sprintf("the engine's calls are %q, want f turned off", renderCalls(callsAfter(sess, 0, "active", "bitrate")))
	})
	time.Sleep(2500 * time.Millisecond) // the Publisher's counters cover 2 s
	if f := layerOf(sh.Snapshot(), "f"); f.Active || f.SentBps != 0 || f.FPS != 0 {
		t.Errorf("the paused full layer is %+v, want it inactive and sending nothing", f)
	}
	if q := layerOf(sh.Snapshot(), "q"); !q.Active || q.SentBps == 0 {
		t.Errorf("the preview is %+v, want it sending", q)
	}

	// ---- a request for high resumes it ----
	n := len(sess.Calls())
	mark := len(video.Packets())
	asked := time.Now()
	bob.subscribe(want(snap.ID, true))
	waitFrames(t, video, mark, "f", 1)
	var first sfutest.Frame
	for _, fr := range sfutest.Frames(video.Packets()[mark:]) {
		if fr.HasMark && fr.Mark.RID == "f" {
			first = fr
			break
		}
	}
	took := first.First.Sub(asked)
	t.Logf("the full layer was back at the viewer %v after its request for high", took.Round(time.Millisecond))
	if !first.KeyStart || took > layerResume {
		t.Errorf("the full layer's first frame came %v after the request (keyframe %t), want a keyframe within %v",
			took.Round(time.Millisecond), first.KeyStart, layerResume)
	}
	target := layerOf(sh.Snapshot(), "f").TargetBps
	calls := callsAfter(sess, n, "active", "bitrate")
	i := slices.IndexFunc(calls, func(c fake.Call) bool { return c.Op == "active" && c.Layer == "f" && c.On })
	if i < 1 || calls[i-1].Op != "bitrate" || calls[i-1].Layer != "f" || calls[i-1].Bps != target ||
		calls[i].At.Sub(calls[i-1].At) > 50*time.Millisecond {
		t.Errorf("the engine's calls after the request are %q, want SetBitrate(f, %d) and SetLayerActive(f, true) "+
			"in one step", renderCalls(calls), target)
	}
	if f := layerOf(sh.Snapshot(), "f"); !f.Active {
		t.Errorf("the full layer is %+v after the resume, want it active", f)
	}
	bob.waitStatus(want(snap.ID, true))
	bob.check()
}

// TestDesktopPause is D11: SetPaused(true) stops the media at once (nothing reaches the viewer 50 ms after the
// call), pauses and mutes the engine and shows the share paused; viewers' keyframe requests go nowhere and a viewer
// who joins meanwhile gets no frame. SetPaused(false) unmutes, then un-pauses, and a keyframe reaches the viewers
// within 500 ms. A pause and a resume from a web tab of the user are reconciled with the same calls.
func TestDesktopPause(t *testing.T) {
	noLeaks(t)
	w := newWorld(t, servertest.Options{})
	alice := w.member("alice")
	app := w.desktop(alice, appEngine(73))
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	bobView := bob.watch(sfutest.LoopbackSettings(), 0)
	sh, snap, video, audio := watchedShare(t, app, bob, bobView)
	sess := app.engine.Session()
	n := len(sess.Calls())

	// ---- pause ----
	pausedAt := time.Now()
	if err := sh.SetPaused(waitCtx(t), true); err != nil {
		t.Fatalf("SetPaused(true): %v", err)
	}
	waitShareStatus(bob, snap.ID, protocol.ShareStatusPaused)
	waitPauseCalls(t, sess, n, "paused true, audio_muted true")
	if !sh.Snapshot().Paused {
		t.Errorf("the share is %+v, want it paused", sh.Snapshot())
	}
	time.Sleep(500 * time.Millisecond) // what was in flight has landed
	for _, r := range []*sfutest.Recorder{video, audio} {
		if late := lastArrival(r).Sub(pausedAt); late > pauseQuiet {
			t.Errorf("the viewer's %s went on %v after SetPaused(true), want at most %v", r.Kind(),
				late.Round(time.Millisecond), pauseQuiet)
		}
	}

	// ---- a viewer who joins meanwhile gets nothing; keyframe requests go nowhere ----
	k := len(sess.Calls())
	carol := w.connect(w.member("carol"), protocol.RoleViewer)
	carolView := carol.watch(sfutest.LoopbackSettings(), 0)
	waitShareStatus(carol, snap.ID, protocol.ShareStatusPaused)
	carol.subscribe(want(snap.ID, true))
	time.Sleep(1500 * time.Millisecond)
	if hasTrack(carolView, snap.ID, webrtc.RTPCodecTypeVideo) {
		t.Error("a viewer who joined the paused share got video")
	}
	if keys := callsAfter(sess, k, "keyframe"); len(keys) != 0 {
		t.Errorf("keyframe requests reached the paused engine: %q", renderCalls(keys))
	}
	if late := lastArrival(video).Sub(pausedAt); late > pauseQuiet {
		t.Errorf("the viewer's video went on %v after SetPaused(true)", late.Round(time.Millisecond))
	}

	// ---- resume ----
	mark := len(video.Packets())
	resumedAt := time.Now()
	if err := sh.SetPaused(waitCtx(t), false); err != nil {
		t.Fatalf("SetPaused(false): %v", err)
	}
	waitPauseCalls(t, sess, n, "paused true, audio_muted true, audio_muted false, paused false")
	carolVideo := track(t, carolView, snap.ID, webrtc.RTPCodecTypeVideo)
	waitFrames(t, carolVideo, 0, "f", 5)
	waitFrames(t, video, mark, "f", 5)
	for _, v := range []struct {
		who  string
		pkts []sfutest.Packet
	}{{"the late viewer", carolVideo.Packets()}, {"the viewer", video.Packets()[mark:]}} {
		frames := sfutest.Frames(v.pkts)
		took := frames[0].First.Sub(resumedAt)
		t.Logf("%s's first frame after the resume came %v after SetPaused(false)", v.who, took.Round(time.Millisecond))
		if !frames[0].KeyStart || took > resumeKeyframe {
			t.Errorf("%s's first frame after the resume came %v after SetPaused(false) (keyframe %t), want a keyframe "+
				"within %v", v.who, took.Round(time.Millisecond), frames[0].KeyStart, resumeKeyframe)
		}
	}
	waitShareStatus(bob, snap.ID, protocol.ShareStatusLive)
	if err := sfutest.CheckContinuous(sfutest.WithoutPartialTail(video.Packets())); err != nil {
		t.Errorf("the viewer's video across the pause: %v", err)
	}
	waitPackets(t, audio, 25)

	// ---- a pause and a resume from a web tab ----
	tab := w.webTab(alice)
	m := len(sess.Calls())
	paused, resumed := true, false
	var params protocol.ShareParams
	tab.request(protocol.MessageTypeShareUpdate, protocol.ShareUpdate{ShareID: snap.ID, Paused: &paused}, &params)
	tabPausedAt := time.Now()
	waitPauseCalls(t, sess, m, "paused true, audio_muted true")
	waitShareStatus(bob, snap.ID, protocol.ShareStatusPaused)
	eventually(t, func() bool { return sh.Snapshot().Paused },
		func() string { return fmt.Sprintf("the share is %+v, want it paused from the tab", sh.Snapshot()) })
	time.Sleep(500 * time.Millisecond)
	// The core paused after the tab's room.state reached it: allow that delay on top of the pump's 50 ms.
	pauseCall := callsAfter(sess, m, "paused")[0]
	for _, r := range []*sfutest.Recorder{video, audio} {
		if late := lastArrival(r).Sub(pauseCall.At); late > pauseQuiet {
			t.Errorf("the viewer's %s went on %v after the core paused for the tab, want at most %v", r.Kind(),
				late.Round(time.Millisecond), pauseQuiet)
		}
	}
	t.Logf("the core paused %v after the tab's share.update was answered", pauseCall.At.Sub(tabPausedAt).Round(time.Millisecond))
	mark = len(video.Packets())
	tab.request(protocol.MessageTypeShareUpdate, protocol.ShareUpdate{ShareID: snap.ID, Paused: &resumed}, &params)
	waitPauseCalls(t, sess, m, "paused true, audio_muted true, audio_muted false, paused false")
	waitShareStatus(bob, snap.ID, protocol.ShareStatusLive)
	waitFrames(t, video, mark, "f", 5)
	if frames := sfutest.Frames(video.Packets()[mark:]); !frames[0].KeyStart {
		t.Errorf("the viewer's first frame after the tab's resume is %+v, want a keyframe", frames[0])
	}
	if sh.Snapshot().Paused {
		t.Errorf("the share is %+v, want it resumed from the tab", sh.Snapshot())
	}
	bob.check()
	carol.check()
}

// TestDesktopPresetElsewhere is D12: a web tab of the user sets the core's share to Movie; the core reconciles from
// room.state: share.update with the same preset, then Movie's ShareParams: the Opus target of 256 kbit/s
// (SetBitrate(audio, 256000)) and the preset in Session.Reconfigure.
func TestDesktopPresetElsewhere(t *testing.T) {
	noLeaks(t)
	w := newWorld(t, servertest.Options{})
	alice := w.member("alice")
	app := w.desktop(alice, appEngine(74))
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	bobView := bob.watch(sfutest.LoopbackSettings(), 0)
	sh, snap, video, audio := watchedShare(t, app, bob, bobView)
	sess := app.engine.Session()
	n := len(sess.Calls())

	tab := w.webTab(alice)
	var params protocol.ShareParams
	tab.request(protocol.MessageTypeShareUpdate, protocol.ShareUpdate{ShareID: snap.ID, Preset: protocol.PresetMovie}, &params)
	if params.AudioBitrate != 256_000 {
		t.Errorf("the tab's share.update answered %+v, want Movie's Opus target", params)
	}
	eventually(t, func() bool {
		calls := callsAfter(sess, n, "bitrate", "reconfigure")
		return slices.ContainsFunc(calls, func(c fake.Call) bool {
			return c.Op == "bitrate" && c.Layer == media.LayerAudio && c.Bps == 256_000
		}) && slices.ContainsFunc(calls, func(c fake.Call) bool {
			return c.Op == "reconfigure" && c.Reconf.Preset == string(protocol.PresetMovie)
		})
	}, func() string {
		return fmt.Sprintf("the engine's calls are %q, want the Opus target 256000 and the preset movie",
			renderCalls(callsAfter(sess, n, "bitrate", "reconfigure")))
	})
	if logs := app.logs.String(); !strings.Contains(logs, `msg="share update" share_id=`+snap.ID+` preset=movie`) {
		t.Error("the core's log has no share.update with the preset movie")
	}
	eventually(t, func() bool { return sh.Snapshot().Preset == protocol.PresetMovie },
		func() string { return fmt.Sprintf("the share is %+v, want preset movie", sh.Snapshot()) })
	if info, _ := bob.shareIn(snap.ID); info.Preset != protocol.PresetMovie {
		t.Errorf("the share in room.state is %+v, want preset movie", info)
	}
	// The media goes on: the re-init of a new preset starts each layer on a keyframe.
	mark := len(video.Packets())
	waitFrames(t, video, mark, "f", 30)
	waitPackets(t, audio, 50)
	if err := sfutest.CheckContinuous(sfutest.WithoutPartialTail(video.Packets())); err != nil {
		t.Errorf("the viewer's video across the preset change: %v", err)
	}
	bob.check()
}

// TestDesktopEncoderFallback is D13: the engine falls back to another encoder for f (EventEncoderChanged, then the
// layer's EventLayer): the app gets EventNotice{encoder_fallback}, the snapshot's encoder of f changes, the share
// keeps its id and the viewer's media goes on. Then the engine runs out of encoders (encoder_lost) and the share ends
// with error/encoder_lost.
func TestDesktopEncoderFallback(t *testing.T) {
	noLeaks(t)
	w := newWorld(t, servertest.Options{})
	app := w.desktop(w.member("alice"), appEngine(75))
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	bobView := bob.watch(sfutest.LoopbackSettings(), 0)
	sh, snap, video, audio := watchedShare(t, app, bob, bobView)
	sess := app.engine.Session()

	f := layerOf(sh.Snapshot(), "f")
	sess.Emit(media.Event{Code: media.EventEncoderChanged, Layer: "f", Encoder: "mf-sw"})
	sess.Emit(media.Event{Code: media.EventLayer, Layer: "f", Width: f.Width, Height: f.Height, FPS: 60,
		Encoder: "mf-sw", Profile: f.Profile, LiveBitrate: true, NoOSIndicator: true})
	fallback := core.Notice{Code: core.NoticeEncoderFallback, Params: map[string]string{"layer": "f", "encoder": "mf-sw"}}
	eventually(t, func() bool {
		return slices.ContainsFunc(app.notices(), func(n core.Notice) bool {
			return n.Code == fallback.Code && n.Params["layer"] == "f" && n.Params["encoder"] == "mf-sw"
		})
	}, func() string { return fmt.Sprintf("the app's notices are %+v, want %+v", app.notices(), fallback) })
	eventually(t, func() bool {
		s := sh.Snapshot()
		return layerOf(s, "f").Encoder == "mf-sw" && s.Encoder == "mf-sw" && !layerOf(s, "f").HW
	}, func() string { return fmt.Sprintf("the share is %+v, want f on mf-sw", sh.Snapshot()) })
	if s := sh.Snapshot(); s.ID != snap.ID || layerOf(s, "q").Encoder != fake.EncoderName || s.State != core.StateLive {
		t.Errorf("the share is %+v, want it live as %s with q on its encoder", s, snap.ID)
	}
	mark := len(video.Packets())
	waitFrames(t, video, mark, "f", 60)
	waitPackets(t, audio, 50)
	checkVideo(t, "the viewer's video across the fallback", video.Packets())

	// ---- no encoder left ----
	sess.Fail(&media.Error{Code: media.CodeEncoderLost, Op: "next"})
	if end := app.waitDone(sh); end != (core.EndInfo{Reason: core.EndError, Media: media.CodeEncoderLost}) {
		t.Errorf("the share ended with %+v, want error/encoder_lost", end)
	}
	eventually(t, func() bool { _, shared := bob.shareIn(snap.ID); return !shared },
		func() string { return fmt.Sprintf("bob's room.state %+v, want the share gone", bob.roomState()) })
	bob.check()
}

// TestDesktopRepublishKeepsPauseAndPreset is README WA38's hand-over: the server restarts while the share is paused
// and set to Text. The core publishes it again with share.start{replaces, preset: text} and sends
// share.update{paused: true} for the new share, so the server has the pause (the hub logs it; a paused share sends
// no keyframe, so the new share shows paused while starting, its start timeout suspended, WF3), while the engine stays
// paused. The resume brings it live, and the viewer follows it by replaces. TestDesktopPausedAcrossRestart keeps it
// paused past the start timeout.
func TestDesktopRepublishKeepsPauseAndPreset(t *testing.T) {
	noLeaks(t)
	udp, tcp := freePort(t, "udp"), freePort(t, "tcp")
	w := newWorld(t, servertest.Options{Flags: []string{
		"--listen.ice-udp=127.0.0.1:" + strconv.Itoa(udp), "--listen.ice-tcp=127.0.0.1:" + strconv.Itoa(tcp),
	}})
	app := w.desktop(w.member("alice"), appEngine(76))
	bob := w.connect(w.member("bob"), protocol.RoleViewer)
	bobView := bob.watch(sfutest.LoopbackSettings(), 0)
	sh, snap, _, _ := watchedShare(t, app, bob, bobView)
	sess := app.engine.Session()
	if err := sh.SetPreset(waitCtx(t), protocol.PresetText); err != nil {
		t.Fatalf("SetPreset(text): %v", err)
	}
	if err := sh.SetPaused(waitCtx(t), true); err != nil {
		t.Fatalf("SetPaused(true): %v", err)
	}
	if info := waitShareStatus(bob, snap.ID, protocol.ShareStatusPaused); info.Preset != protocol.PresetText {
		t.Errorf("the share in room.state is %+v, want preset text", info)
	}
	n := len(sess.Calls())

	// ---- the restart ----
	w.srv.Restart(t)
	var next string
	eventually(t, func() bool {
		next = sh.Snapshot().ID
		return next != snap.ID && next != ""
	}, func() string { return fmt.Sprintf("the share is %+v, want a new id", sh.Snapshot()) })
	var info protocol.ShareInfo
	eventually(t, func() bool {
		var ok bool
		info, ok = bob.shareIn(next)
		return ok
	}, func() string {
		return fmt.Sprintf("bob's room.state %+v, want the new share %s", bob.roomState(), next)
	})
	// It is starting until its share.update{paused} lands, then paused; room.state's coalescing may show either first.
	if info.Replaces != snap.ID || info.Preset != protocol.PresetText ||
		(info.Status != protocol.ShareStatusStarting && info.Status != protocol.ShareStatusPaused) {
		t.Errorf("the new share is %+v, want it replacing %s, preset text, starting or paused (it isn't live yet)",
			info, snap.ID)
	}
	waitShareStatus(bob, next, protocol.ShareStatusPaused)
	eventually(t, func() bool {
		for line := range strings.SplitSeq(w.srv.Logs(), "\n") {
			if strings.Contains(line, "share paused") && strings.Contains(line, next) {
				return true
			}
		}
		return false
	}, func() string { return "the server's log has no pause of the new share" })
	if calls := callsAfter(sess, n, "paused", "audio_muted", "reconfigure"); len(calls) != 0 {
		t.Errorf("the republish changed the engine: %q", renderCalls(calls))
	}
	if s := sh.Snapshot(); !s.Paused || s.Preset != protocol.PresetText {
		t.Errorf("the share is %+v, want it paused with preset text", s)
	}

	// ---- the resume: live, and the viewer follows ----
	if err := sh.SetPaused(waitCtx(t), false); err != nil {
		t.Fatalf("SetPaused(false): %v", err)
	}
	waitShareStatus(bob, next, protocol.ShareStatusLive)
	app.waitState(sh, core.StateLive)
	video := track(t, bobView, next, webrtc.RTPCodecTypeVideo)
	waitFrames(t, video, 0, "f", 15)
	if s := sh.Snapshot(); s.Paused || s.Preset != protocol.PresetText || s.ID != next {
		t.Errorf("the share is %+v, want %s resumed with preset text", s, next)
	}
	bob.check()
}
