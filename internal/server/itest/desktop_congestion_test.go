package itest

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/client/core"
	"github.com/MoonWX/isshoni/internal/client/ratectl"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// Cases D9, D10 and D19 of docs/m2/01-client-core.md §14: the native sharer's congestion control end to end. The
// client core shares fake.Engine's 1080p60 through the test's server from behind a bottleneck on its uplink; the
// SFU's native estimator (01 §10.1) answers with congestion hints, and the core's rate control (01 §9) applies them
// within its guards and ramps back once the uplink allows it.
//
//   - D9: M1's policer (sfutest.NewLink) of 3 Mbit/s appears once the full layer has ramped to 8 Mbit/s: the loss
//     before repair backs the estimate off and the full layer comes down (the estimate probes upward again after its
//     hold, and backs off again at the policer), the viewer loses next to nothing for good, and the app is told the
//     upload allows about 540p; once the policer allows 20 Mbit/s, the full layer ramps back to its 8 Mbit/s and the
//     upload hint clears.
//   - D10: D9's script with an encoder that re-initializes for every new bitrate (fake's probe without LiveBitrate):
//     every change is 30 % or more and 10 s or more after the last one, and each starts with the new encoder's
//     keyframe.
//   - D19: a queueing link of 6 Mbit/s with a 1 s buffer (sfutest.NewQueueLink), like a home router's, is there from
//     the start: the ramp crosses its capacity, and the queueing delay backs the estimate off before the buffer
//     overflows, so the queue drains within 6 s and no datagram is ever lost at it.
//
// Where the bottleneck sits. 01 §10.1 puts the Link on the SFU's socket, in front of the publisher's address
// (FaultConn.SetInboundLink). The in-process server of these tests binds its ICE socket itself (servertest has no seam
// for netx.TransportOptions.PacketConns), so the Link sits at the other end of the same path instead: the core's pub
// PC sends through an uplink, which hands every datagram to an sfutest.FaultConn as if read from one peer
// (uplinkPeer), and FaultConn.SetInboundLink puts the Link there, with the semantics 01 §10.1 gives it: a policer
// loses the RTP beyond its bucket, a queueing Link delays each datagram to its departure time and loses the RTP
// whose wait would exceed its limit, and neither ever loses STUN, DTLS or RTCP. What reaches the SFU, and when, is
// the same as with the Link on the SFU's socket; only the way back to the core is unlimited, as an uplink test
// wants.
//
// These cases take a minute and more each, so -short skips them.

// The scripts and bounds of D9 and D10.
const (
	// steadyFor is how long the share runs at its ceiling before the uplink drops: longer than the live guard's
	// interval (ratectl.MinInterval), so the core takes the first hint at once, as a share that ramped a while ago
	// does.
	steadyFor = 5 * time.Second
	// policedCapacity is D9's uplink, and liftedCapacity the same policer once the limit lifts.
	policedCapacity = 3_000_000
	liftedCapacity  = 20_000_000
	// congestionHintWithin: the first congestion hint reaches the core within this of the limit (D9).
	congestionHintWithin = 6 * time.Second
	// settleWithin and settledF: the full layer's target comes down to settledF or below within settleWithin of the
	// limit (D9).
	settleWithin = 15 * time.Second
	settledF     = 2_300_000
	// policedFor is how long the limit lasts: the 15 s of settling and as long again, over which the viewer loses at
	// most maxFinalLossPct of its video's sequence space for good after the full layer settled (D9).
	policedFor      = 30 * time.Second
	maxFinalLossPct = 2.0
	// recoverWithin: once the limit lifts, the full layer is back at its ceiling, the upload hint off and the estimate
	// gone within this (D9). After a backoff the estimate is held for 10 s, then grows by 15.1 % a step, and the full
	// layer follows each step with one of its ramp's, RampEvery apart (a hint every 3–4 s). Under -race on the Mac the
	// way back took 30–57 s: least when the estimate was growing at the lift, most after a backoff right at it (the
	// estimate's probes can hit the policer about every 25 s), with the hold, some 13 steps from about 1.4 Mbit/s and
	// the upload hint's UploadOff. A second backoff on the next tick that lowers the estimate by less than a hint's
	// 15 % restarts the hold without one, and the first growth step after it is no hint either: about 4 s more. So
	// the bound has little margin in that case.
	recoverWithin = 60 * time.Second
	// ceilingF is the Auto preset's full layer (M1 02 §8.6), where the ramp ends.
	ceilingF = 8_000_000
	// rampWithin bounds the wait for the ramp to reach ceilingF from live: RampDelay and four RampEvery (01 §9.3),
	// or the one step OneStepDelay after live (re-init), with every step on a Tick and the waits the guards add.
	rampWithin = ratectl.RampDelay + 4*ratectl.RampEvery + 5*time.Second
	// reinitKeyBefore and reinitKeyAfter: each change of a re-init encoder's layer is followed by a keyframe of it
	// captured from reinitKeyBefore before the change to reinitKeyAfter after it (D10). The new encoder's first frame
	// is the old one's next capture instant (fake's swap): the next frame's, or one up to the 200 ms the pump may
	// lag behind the engine's clock before its lag cap skips.
	reinitKeyBefore = 250 * time.Millisecond
	reinitKeyAfter  = 100 * time.Millisecond
	// reinitSeenWithin: a change can be seen at a viewer when one got frames of the layer captured within this of it.
	// While the uplink is congested the SFU may forward another layer to every viewer of the full one (its layers
	// switch on keyframes, 02 §9), and a change then is seen at none.
	reinitSeenWithin = time.Second
	// A keyframe near a change is the new encoder's only when nothing else made it (sightOf): not the GOP, which
	// makes a keyframe a whole GOP (gopFull, gopOther) after the layer's last one (fake.Session's GOPs, 04 §9.4's;
	// a GOP is a whole number of frames, so within half a frame of it: gopSlack is half a frame at 5 fps), and not a
	// keyframe request of the layer (a PLI or the SFU's layer switch: fake's "keyframe" call), whose keyframe is
	// captured from keyRequestBefore before the request (the pump's lag) to keyRequestAfter after it (the 250 ms
	// spacing of forced keyframes, 04 §6.4, and two frames).
	gopFull          = 4 * time.Second
	gopOther         = 2 * time.Second
	gopSlack         = 100 * time.Millisecond
	keyRequestBefore = 250 * time.Millisecond
	keyRequestAfter  = 500 * time.Millisecond
	// callSlack is how much closer together than ReinitMinInterval two engine calls (fake.Call.At) of a layer may
	// be: D10 asserts ReinitMinInterval − callSlack between them, 9 s, not the 10 s of its acceptance. ratectl keeps
	// its guards on the instants its Ticks carry, and the share's actor (internal/client/core's share.go) passes it
	// the rate ticker's value, which since Go 1.23 is when the tick was due, not when the actor took it. A Tick taken
	// late is recorded as a change up to that lag (0.54 s seen under -race) before the engine call, while a hint's
	// change is recorded at time.Now(): so the next change can reach the engine less than 10 s after the last one.
	// The native engine itself spaces bitrate re-inits 10 s apart (04 §6.4, latest value wins), so what users get
	// keeps the 10 s; the fake applies each call at once. Once the actor passes time.Now() to rateTick, callSlack is
	// Windows' clock step, 20 ms.
	callSlack = time.Second
)

// The script and bounds of D19.
const (
	queueCapacity = 6_000_000
	queueLimit    = time.Second
	// queueSampleEvery: Queued() is sampled this often (D19).
	queueSampleEvery = 100 * time.Millisecond
	// queueDrained and queueDrainWithin: the queue is below queueDrained queueDrainWithin after the ramp crossed the
	// capacity (D19).
	queueDrained     = 150 * time.Millisecond
	queueDrainWithin = 6 * time.Second
	// queueWatch: no RTP datagram is lost at the queue from the start until this long after the crossing (D19).
	queueWatch = 60 * time.Second
	// autoAudioBps is the Auto preset's Opus target (M1 02 §8.4), the audio part of D19's crossing.
	autoAudioBps = 128_000
)

// congestionEngine is the fake engine of these cases: a 1920×1080 screen, so the full layer's target height is 1080
// and a 540p upload hint is a limit, with the default probe's encoder, or one without live bitrate when reinit.
func congestionEngine(seed uint64, reinit bool) fake.EngineConfig {
	cfg := fake.EngineConfig{SourceSize: [2]int{1920, 1080}, Seed: seed}
	if reinit {
		cfg.Probe = media.Probe{Encoders: []media.EncoderInfo{{
			Name: fake.EncoderName, Adapter: -1, Tested: true, LiveBitrate: false,
			Profiles: []string{"6400", fake.DecodableProfile},
		}}}
	}
	return cfg
}

// TestDesktopUplinkCongestion is D9: a 3 Mbit/s policer on the uplink of a share whose full layer runs at 8 Mbit/s.
// The core gets a congestion hint within 6 s, and the full layer's target comes down to 2.3 Mbit/s or less within
// 15 s ("settles"); from then on, while the limit lasts, the viewer loses less than 2 % of the video for good (what
// neither the core's nor the SFU's retransmissions brought back), although the estimate probes upward again after
// its hold; and the app's share shows the upload hint at about 540p or 720p. Once the policer allows 20 Mbit/s, the
// full layer is back at 8 Mbit/s within 60 s, with the upload hint cleared and a last congestion hint that lifts the
// cap.
func TestDesktopUplinkCongestion(t *testing.T) {
	if testing.Short() {
		t.Skip("D9 runs for about two minutes")
	}
	noLeaks(t)
	run := startCongestion(t, congestionEngine(91, false), nil)
	run.waitCeiling()
	tr := run.police()

	// ---- the hint, the settled full layer, the final loss, the upload hint ----
	if tr.hint.at.IsZero() || tr.hint.at.Sub(tr.limitedAt) > congestionHintWithin {
		t.Errorf("the first congestion hint came %s after the limit (%+v), want within %v",
			sinceOrNever(tr.limitedAt, tr.hint.at), tr.hint, congestionHintWithin)
	}
	if tr.settled.IsZero() || tr.settled.Sub(tr.limitedAt) > settleWithin {
		t.Errorf("the full layer's target came down to %d bit/s or less %s after the limit, want within %v",
			settledF, sinceOrNever(tr.limitedAt, tr.settled), settleWithin)
	}
	if tr.finalLossPct >= maxFinalLossPct {
		t.Errorf("the viewer lost %.2f %% of the video for good after the full layer settled, want less than %.0f %%",
			tr.finalLossPct, maxFinalLossPct)
	}
	if !slices.ContainsFunc(tr.limitedUploads, func(u ratectl.UploadHint) bool {
		return u.Limited && (u.ApproxHeight == 540 || u.ApproxHeight == 720)
	}) {
		t.Errorf("the share's upload hints while the uplink was limited are %+v, want one at 540p or 720p",
			tr.limitedUploads)
	}

	// ---- once the limit lifts ----
	if tr.recovered.IsZero() {
		last, _ := lastCongestion(run.hints(), tr.limitedAt)
		t.Errorf("%v after the limit lifted the full layer is %+v, the upload hint %+v and the last congestion hint "+
			"%+v, want 8 Mbit/s, none and one that lifts the cap", recoverWithin, layerOf(run.sh.Snapshot(), "f"),
			run.sh.Snapshot().Upload, last)
	}
	run.bob.check()
}

// TestDesktopReinitCongestion is D10: D9's script with an encoder that has no live bitrate. The full layer ramps in
// one step and comes down when the hints say so, but no layer's target changes less than 10 s after its last change
// or by less than 30 %, and each change is applied as a re-init: the core's rate action says so (ratectl's
// Action.Reinit), the fake re-initializes the layer for the engine call (its EventLayer, then a Reinit keyframe), and
// a viewer of the layer gets that keyframe, captured at the change (reinitKeyBefore, reinitKeyAfter) and made by
// nothing else (sightOf). The intervals are measured on the engine's calls, which ratectl's guards don't bound to
// the 10 s (callSlack). A change while the SFU forwards another layer is seen at no viewer; the full layer's ramp step
// and its way back once the limit lifts are, and so is the preview's, which carol watches.
func TestDesktopReinitCongestion(t *testing.T) {
	if testing.Short() {
		t.Skip("D10 runs for about two minutes")
	}
	noLeaks(t)
	run := startCongestion(t, congestionEngine(92, true), nil)
	// carol watches the preview, so that a change of q is seen at a viewer too.
	carol := run.w.connect(run.w.member("carol"), protocol.RoleViewer)
	preview := carol.watch(sfutest.LoopbackSettings(), 0)
	carol.waitLive(run.snap.ID)
	carol.subscribe(want(run.snap.ID, false))
	waitFrames(t, track(t, preview, run.snap.ID, webrtc.RTPCodecTypeVideo), 0, "q", 15)
	carol.waitStatus(want(run.snap.ID, false))
	run.waitCeiling()
	tr := run.police()

	changes := map[string][]fake.Call{}
	requests := map[string][]time.Time{} // each layer's keyframe requests: PLIs and the SFU's layer switches
	for _, c := range run.sess.Calls() {
		switch {
		case c.Op == "bitrate" && c.Layer != media.LayerAudio:
			changes[c.Layer] = append(changes[c.Layer], c)
		case c.Op == "keyframe":
			requests[c.Layer] = append(requests[c.Layer], c.At)
		}
	}
	if len(changes["f"]) < 2 {
		t.Errorf("the full layer's target changed %d times, want the ramp's step and a backoff at least",
			len(changes["f"]))
	}
	initial := map[string]int{}
	for _, l := range run.snap.Layers {
		initial[l.RID] = l.TargetBps
	}
	logs := run.app.logs.String()
	viewers := []*sfutest.Viewer{run.viewer, preview}
	seen, unsure := map[string]int{}, map[string]int{}
	for rid, calls := range changes {
		prev, prevAt := initial[rid], time.Time{}
		for i, c := range calls {
			step := 100 * float64(c.Bps-prev) / float64(prev)
			t.Logf("%s: %d → %d bit/s (%+.1f %%) at %v", rid, prev, c.Bps, step,
				c.At.Sub(run.liveAt).Round(time.Millisecond))
			if step > -ratectl.ReinitMinChange && step < ratectl.ReinitMinChange {
				t.Errorf("%s's target changed by %.1f %% (%d → %d bit/s), want at least %d %%", rid, step, prev, c.Bps,
					ratectl.ReinitMinChange)
			}
			if i > 0 && c.At.Sub(prevAt) < ratectl.ReinitMinInterval-callSlack {
				t.Errorf("%s's target changed %v after its last change, want at least %v (ReinitMinInterval less "+
					"callSlack)", rid, c.At.Sub(prevAt).Round(time.Millisecond), ratectl.ReinitMinInterval-callSlack)
			}
			action := fmt.Sprintf(`msg="rate action" op=bitrate layer=%s bps=%d active=false reinit=true`, rid, c.Bps)
			if !strings.Contains(logs, action) {
				t.Errorf("the core's log has no re-init action for %s's %d bit/s", rid, c.Bps)
			}
			switch sight := sightOf(viewers, run.snap.ID, run.origins.at, rid, c.At, requests[rid]); sight {
			case sightNone:
				t.Logf("  no viewer got %s within %v of this change: its keyframe can't be seen", rid, reinitSeenWithin)
			case sightNoKey:
				t.Errorf("the viewers got no keyframe of %s captured from %v before to %v after its change to %d bit/s",
					rid, reinitKeyBefore, reinitKeyAfter, c.Bps)
			case sightUnsure:
				t.Logf("  the viewers' keyframes of %s at this change may be the GOP's or a keyframe request's", rid)
				unsure[rid]++
			case sightReinit:
				seen[rid]++
			}
			prev, prevAt = c.Bps, c.At
		}
	}
	if seen["f"] < 2 {
		t.Errorf("the viewers saw the new encoder's keyframe at %d of the full layer's %d changes, want 2 at least",
			seen["f"], len(changes["f"]))
	}
	// The preview's keyframes are often the GOP's or a keyframe request's as well (carol joins q, the 4 s GOP): a
	// change whose keyframe can't be told apart from those doesn't count against it (a missing keyframe already
	// failed above, sightNoKey); one that came and is certainly the new encoder's is wanted only when none was unsure.
	if len(changes["q"]) > 0 && seen["q"] == 0 && unsure["q"] == 0 {
		t.Errorf("the viewers saw the new encoder's keyframe at none of the preview's %d changes, want 1 at least",
			len(changes["q"]))
	}
	skips, skipped := run.origins.skips()
	t.Logf("D10: the first congestion hint %s after the limit; the full layer settled %s after it; the viewer lost "+
		"%.2f %% for good after that; the engine's clock skipped %d times, %v in all",
		sinceOrNever(tr.limitedAt, tr.hint.at), sinceOrNever(tr.limitedAt, tr.settled), tr.finalLossPct, skips, skipped)
	run.bob.check()
	carol.check()
}

// TestDesktopBufferbloat is D19: a queueing link of 6 Mbit/s with a 1 s buffer on the uplink from the start. The core
// ramps from 4 Mbit/s with a live encoder; the crossing is the first applied ramp step whose targets of f, q and the
// audio add up to more than the link's capacity. The queue (Queued(), sampled every 100 ms) is below 150 ms 6 s after
// the crossing, a congestion hint has come, and no RTP datagram is lost at the queue from the start until 60 s after
// the crossing: the delay rule, not loss, did the backoff.
func TestDesktopBufferbloat(t *testing.T) {
	if testing.Short() {
		t.Skip("D19 runs for over a minute")
	}
	noLeaks(t)
	link := sfutest.NewQueueLink(queueCapacity, queueLimit)
	run := startCongestion(t, congestionEngine(93, false), link)
	samples := run.sampleQueue(link)

	// ---- the crossing ----
	var crossing fake.Call
	eventually(t, func() bool {
		var ok bool
		crossing, ok = run.crossing(queueCapacity)
		return ok
	}, func() string {
		return fmt.Sprintf("the ramp never crossed %d bit/s: the engine's calls are %q", queueCapacity,
			renderCalls(callsAfter(run.sess, 0, "bitrate")))
	})
	t.Logf("the ramp crossed the link's capacity %v after live: f at %d bit/s",
		crossing.At.Sub(run.liveAt).Round(time.Millisecond), crossing.Bps)

	// ---- 60 s from the crossing ----
	time.Sleep(time.Until(crossing.At.Add(queueWatch)))
	q := samples.stop()
	_, _, lost := link.Counts()

	var (
		peak, atDrain time.Duration
		peakAt        time.Time
		drainedAt     time.Time
		drainSample   bool
	)
	for _, s := range q {
		if s.at.Before(crossing.At) {
			continue
		}
		if !s.at.After(crossing.At.Add(queueDrainWithin)) {
			atDrain, drainSample = s.queued, true
			if s.queued > peak {
				peak, peakAt, drainedAt = s.queued, s.at, time.Time{}
			}
		}
		if s.at.After(peakAt) && s.queued < queueDrained && drainedAt.IsZero() {
			drainedAt = s.at
		}
	}
	run.logScript(crossing.At, crossing.At.Add(-3*time.Second), crossing.At.Add(15*time.Second))
	t.Logf("D19: the queue peaked at %v %v after the crossing and was below %v again %v after it; %v queued %v after "+
		"it; %d datagrams lost at the queue",
		peak.Round(time.Millisecond), peakAt.Sub(crossing.At).Round(time.Millisecond),
		queueDrained, drainedAt.Sub(crossing.At).Round(time.Millisecond), atDrain.Round(time.Millisecond),
		queueDrainWithin, lost)
	if !drainSample || atDrain >= queueDrained {
		t.Errorf("the queue is %v %v after the crossing (peak %v), want below %v", atDrain.Round(time.Millisecond),
			queueDrainWithin, peak.Round(time.Millisecond), queueDrained)
	}
	hints := run.hints()
	if !slices.ContainsFunc(hints, func(h loggedHint) bool {
		return h.reason == string(protocol.HintReasonCongestion) && h.at.After(crossing.At) && h.maxBitrate > 0
	}) {
		t.Errorf("no congestion hint after the crossing; the core's hints are %+v", hints)
	}
	if lost != 0 {
		t.Errorf("%d RTP datagrams were lost at the queue, want none", lost)
	}
	if t.Failed() { // the queue from 3 s before the crossing to 12 s after it, every 300 ms
		for i, s := range q {
			if d := s.at.Sub(crossing.At); d >= -3*time.Second && d <= 12*time.Second && i%3 == 0 {
				t.Logf("  queue %v at %v", s.queued.Round(time.Millisecond), d.Round(time.Millisecond))
			}
		}
	}
	run.bob.check()
}

// ---- a congested share ----

// congestionRun is one of these cases: the member's app sharing from behind an uplink, and a viewer watching the full
// layer with its audio.
type congestionRun struct {
	t      *testing.T
	w      *world
	app    *recoveryApp
	up     *uplink
	bob    *client
	viewer *sfutest.Viewer
	sh     *core.Share
	snap   core.ShareSnapshot // at live
	sess   *fake.Session
	video  *sfutest.Recorder
	liveAt time.Time
	// origins follows the session's Origin from live on.
	origins *originTrace
}

// startCongestion starts a server, alice's app on engine behind an uplink with link on it (nil: none yet), and bob
// watching her share; it returns once the share is live and bob gets its full layer and audio.
func startCongestion(t *testing.T, engine fake.EngineConfig, link *sfutest.Link) *congestionRun {
	t.Helper()
	w := newWorld(t, servertest.Options{})
	r := &congestionRun{t: t, w: w, up: newUplink(t)}
	if link != nil {
		r.up.SetLink(link)
	}
	mux := webrtc.NewICEUDPMux(nil, r.up)
	t.Cleanup(func() { _ = mux.Close() }) // after the app's close, which is registered later and runs first
	se := sfutest.LoopbackSettings()
	se.SetICEUDPMux(mux)
	r.bob = w.connect(w.member("bob"), protocol.RoleViewer)
	r.viewer = r.bob.watch(sfutest.LoopbackSettings(), 0)
	r.app = w.recoveringDesktop(w.member("alice"), engine, &se)
	r.sh = r.app.start(defaultRoom(r.bob))
	r.snap = r.app.waitState(r.sh, core.StateLive)
	r.liveAt = time.Now()
	if r.sess = r.app.engine.Session(); r.sess == nil {
		t.Fatal("no engine session while the share is live")
	}
	r.origins = r.traceOrigin()
	r.bob.waitLive(r.snap.ID)
	r.bob.subscribe(want(r.snap.ID, true))
	r.video = track(t, r.viewer, r.snap.ID, webrtc.RTPCodecTypeVideo)
	audio := track(t, r.viewer, r.snap.ID, webrtc.RTPCodecTypeAudio)
	waitFrames(t, r.video, 0, "f", 30)
	waitPackets(t, audio, 50)
	r.bob.waitStatus(want(r.snap.ID, true))
	return r
}

// waitCeiling waits until the ramp has taken the full layer to its ceiling.
func (r *congestionRun) waitCeiling() {
	r.t.Helper()
	waitLong(r.t, rampWithin, func() bool { return layerOf(r.sh.Snapshot(), "f").TargetBps >= ceilingF },
		func() string {
			return fmt.Sprintf("%v after live the full layer is %+v, want it at %d bit/s: the engine's calls are %q",
				time.Since(r.liveAt).Round(time.Millisecond), layerOf(r.sh.Snapshot(), "f"), ceilingF,
				renderCalls(callsAfter(r.sess, 0, "bitrate")))
		})
	r.t.Logf("the full layer reached %d bit/s %v after live", ceilingF, time.Since(r.liveAt).Round(time.Millisecond))
}

// policeTrace is what police saw of D9's script.
type policeTrace struct {
	limitedAt, liftedAt time.Time
	hint                loggedHint // the first congestion hint after the limit
	// settled is when the full layer's target first came down to settledF or less (the engine's call), zero if it
	// never did while the limit lasted; finalLossPct is what the viewer lost of its video for good from then until
	// the lift, in percent of its sequence space.
	settled        time.Time
	finalLossPct   float64
	limitedUploads []ratectl.UploadHint // the share's upload hints in EventShare while the limit lasted
	// recovered is when the full layer was at its ceiling with no upload hint and a last congestion hint that lifted
	// the cap; zero if never.
	recovered time.Time
}

// police runs D9's script on a share whose full layer has reached its ceiling: steadyFor at it, then a policer of
// policedCapacity on the uplink for policedFor, then liftedCapacity until the full layer is back at its ceiling with
// no upload hint, for at most recoverWithin. It logs what happened.
func (r *congestionRun) police() *policeTrace {
	t := r.t
	t.Helper()
	time.Sleep(steadyFor)
	tr := &policeTrace{}
	callMark, eventMark := len(r.sess.Calls()), len(r.app.shareEvents())
	link := sfutest.NewLink(policedCapacity)
	r.up.SetLink(link)
	tr.limitedAt = time.Now()

	// The final loss after settling: Lost counts a sequence number FinalLossAfter after it went missing, so the span
	// from the settling to the lift is Lost from FinalLossAfter after the one to FinalLossAfter after the other, and
	// its sequence space Expected from the one to the other.
	var (
		seenAt               time.Time // when the test saw the settled target
		settledExp, liftExp  int
		settledLost, lostEnd int
		haveSettledLost      bool
	)
	for until := tr.limitedAt.Add(policedFor); time.Now().Before(until); time.Sleep(20 * time.Millisecond) {
		switch {
		case seenAt.IsZero() && layerOf(r.sh.Snapshot(), "f").TargetBps <= settledF:
			seenAt, settledExp = time.Now(), r.video.Stats().Expected
		case !seenAt.IsZero() && !haveSettledLost && time.Since(seenAt) >= sfutest.FinalLossAfter:
			settledLost, haveSettledLost = r.video.Stats().Lost, true
		}
	}
	link.SetCapacity(liftedCapacity)
	tr.liftedAt = time.Now()
	liftExp = r.video.Stats().Expected
	events := r.app.shareEvents()
	for _, ev := range events[eventMark:] {
		tr.limitedUploads = append(tr.limitedUploads, ev.Upload)
	}
	time.Sleep(sfutest.FinalLossAfter + 100*time.Millisecond)
	lostEnd = r.video.Stats().Lost
	if haveSettledLost && liftExp > settledExp {
		tr.finalLossPct = 100 * float64(lostEnd-settledLost) / float64(liftExp-settledExp)
	}

	// Recovered: f at its ceiling, the upload hint off, and the estimate gone. The estimate is cleared 1–3 growth
	// steps after f reaches its ceiling (a cap from 8.3 to 9.1 Mbit/s already gives f its 8 Mbit/s) and the upload
	// hint 10 s after f passed about 6.2 Mbit/s (ratectl.UploadOff), so either can come last.
	for deadline := tr.liftedAt.Add(recoverWithin); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		snap := r.sh.Snapshot()
		if layerOf(snap, "f").TargetBps < ceilingF || snap.Upload.Limited {
			continue
		}
		if last, ok := lastCongestion(r.hints(), tr.limitedAt); ok && last.maxBitrate == 0 {
			tr.recovered = time.Now()
			break
		}
	}
	for _, c := range callsAfter(r.sess, callMark, "bitrate") {
		if c.Layer == "f" && c.Bps <= settledF && c.At.Before(tr.liftedAt) {
			tr.settled = c.At
			break
		}
	}
	for _, h := range r.hints() {
		if h.reason == string(protocol.HintReasonCongestion) && !h.at.Before(tr.limitedAt) {
			tr.hint = h
			break
		}
	}
	sent, passed, lost := link.Counts()
	t.Logf("D9 script: a %d bit/s policer from %v after the ceiling: first congestion hint %s after the limit "+
		"(%d bit/s); f settled at %d bit/s or less %s after it; the viewer lost %.2f %% for good from then until the "+
		"lift (%d of %d); %d datagrams lost at the policer (%d of %d bytes passed); back at %d bit/s without an "+
		"upload hint or a cap %s after the lift",
		policedCapacity, steadyFor, sinceOrNever(tr.limitedAt, tr.hint.at), tr.hint.maxBitrate, settledF,
		sinceOrNever(tr.limitedAt, tr.settled), tr.finalLossPct, lostEnd-settledLost, liftExp-settledExp, lost,
		passed, sent, ceilingF, sinceOrNever(tr.liftedAt, tr.recovered))
	r.logScript(tr.limitedAt, tr.limitedAt, time.Now())
	var last ratectl.UploadHint
	for _, ev := range events[eventMark:] {
		if ev.Upload != last {
			t.Logf("  upload hint %+v", ev.Upload)
			last = ev.Upload
		}
	}
	return tr
}

// sinceOrNever returns how long after from at was, to the ms, or "never" for a zero at.
func sinceOrNever(from, at time.Time) string {
	if at.IsZero() {
		return "never"
	}
	return at.Sub(from).Round(time.Millisecond).String()
}

// lastCongestion returns the last congestion hint of hints that came at since or later, and whether there is one.
func lastCongestion(hints []loggedHint, since time.Time) (loggedHint, bool) {
	for i := len(hints) - 1; i >= 0; i-- {
		if h := hints[i]; h.reason == string(protocol.HintReasonCongestion) && !h.at.Before(since) {
			return h, true
		}
	}
	return loggedHint{}, false
}

// logScript logs the engine's bitrate calls and the core's quality hints between from and to, timed from zero.
func (r *congestionRun) logScript(zero, from, to time.Time) {
	for _, c := range callsAfter(r.sess, 0, "bitrate") {
		if !c.At.Before(from) && !c.At.After(to) {
			r.t.Logf("  %s → %d bit/s at %v", c.Layer, c.Bps, c.At.Sub(zero).Round(time.Millisecond))
		}
	}
	for _, h := range r.hints() {
		if !h.at.Before(from) && !h.at.After(to) {
			r.t.Logf("  hint %s %d bit/s at %v", h.reason, h.maxBitrate, h.at.Sub(zero).Round(time.Millisecond))
		}
	}
}

// crossing returns the first bitrate call of f that took the targets of the video layers and the audio above
// capacity, and whether there is one: f and q start at their targets at live, the audio at the Auto preset's.
func (r *congestionRun) crossing(capacity int) (fake.Call, bool) {
	targets := map[string]int{media.LayerAudio: autoAudioBps}
	for _, l := range r.snap.Layers {
		targets[l.RID] = l.TargetBps
	}
	for _, c := range r.sess.Calls() {
		if c.Op != "bitrate" {
			continue
		}
		up := c.Bps > targets[c.Layer]
		targets[c.Layer] = c.Bps
		sum := 0
		for _, bps := range targets {
			sum += bps
		}
		if c.Layer == "f" && up && sum > capacity {
			return c, true
		}
	}
	return fake.Call{}, false
}

// changeSight is what the viewers saw of a change of a layer (sightOf).
type changeSight int

const (
	sightNone   changeSight = iota // no frame of the layer captured within reinitSeenWithin of the change
	sightNoKey                     // frames, but no keyframe captured in the change's window (reinitKeyBefore/After)
	sightUnsure                    // keyframes there, but each may be the GOP's or a keyframe request's
	sightReinit                    // a keyframe there that only the change's re-init explains
)

// sightOf looks at the frames of layer rid of the share that the viewers got around a change of the layer at at,
// given the layer's keyframe requests. A frame's capture instant is its Marker's CaptureNS from the engine session's
// Origin at the time it arrived (originAt). A keyframe captured from reinitKeyBefore before the change to
// reinitKeyAfter after it is the re-init's when it is forced, not the GOP's (the viewers also got a keyframe of the
// layer less than a GOP before it: the GOP's comes a whole GOP after the layer's last keyframe, whatever made that
// one, and CaptureNS keeps its frame grid across re-inits and skips), and no keyframe request of the layer explains
// it (keyRequestBefore, keyRequestAfter).
func sightOf(viewers []*sfutest.Viewer, shareID string, originAt func(time.Time) time.Time, rid string,
	at time.Time, requests []time.Time) changeSight {
	gop := gopOther
	if rid == protocol.RIDHigh {
		gop = gopFull
	}
	type key struct {
		frame uint32
		ns    int64     // its Marker's CaptureNS
		at    time.Time // its capture instant
	}
	var (
		got        bool
		keys, near []key // the layer's keyframes the viewers got; those in the change's window
	)
	for _, v := range viewers {
		for _, rec := range v.Recorders() {
			if rec.Kind() != webrtc.RTPCodecTypeVideo || rec.StreamID() != shareID {
				continue
			}
			for _, f := range sfutest.Frames(rec.Packets()) {
				if !f.HasMark || f.Mark.RID != rid {
					continue
				}
				captured := originAt(f.First).Add(time.Duration(f.Mark.CaptureNS))
				d := captured.Sub(at)
				got = got || (d >= -reinitSeenWithin && d <= reinitSeenWithin)
				if !f.KeyStart && !f.Mark.Keyframe {
					continue
				}
				k := key{frame: f.Mark.Frame, ns: f.Mark.CaptureNS, at: captured}
				keys = append(keys, k)
				if d >= -reinitKeyBefore && d <= reinitKeyAfter {
					near = append(near, k)
				}
			}
		}
	}
	switch {
	case len(near) == 0 && !got:
		return sightNone
	case len(near) == 0:
		return sightNoKey
	}
	for _, k := range near {
		forced := slices.ContainsFunc(keys, func(p key) bool {
			return p.frame < k.frame && time.Duration(k.ns-p.ns) < gop-gopSlack
		})
		requested := slices.ContainsFunc(requests, func(r time.Time) bool {
			d := k.at.Sub(r)
			return d >= -keyRequestBefore && d <= keyRequestAfter
		})
		if forced && !requested {
			return sightReinit
		}
	}
	return sightUnsure
}

// hints returns the quality hints in the core's log.
func (r *congestionRun) hints() []loggedHint {
	return parseHints(r.app.logs.String())
}

// loggedHint is a quality.hint for the share as the core's debug log has it ("quality hint").
type loggedHint struct {
	at         time.Time
	reason     string
	maxBitrate int64
}

// parseHints returns the "quality hint" lines of a core's text log, in order.
func parseHints(logs string) []loggedHint {
	var out []loggedHint
	for line := range strings.SplitSeq(logs, "\n") {
		if !strings.Contains(line, `msg="quality hint"`) {
			continue
		}
		var h loggedHint
		for field := range strings.FieldsSeq(line) {
			k, v, _ := strings.Cut(field, "=")
			switch k {
			case "time":
				h.at, _ = time.Parse(time.RFC3339Nano, v)
			case "reason":
				h.reason = v
			case "max_bitrate":
				h.maxBitrate, _ = strconv.ParseInt(v, 10, 64)
			}
		}
		out = append(out, h)
	}
	return out
}

// ---- the engine's clock ----

// originTrace follows an engine session's Origin, which moves when the fake's lag cap skips (the pump fell 200 ms
// behind, fake.Source.Next): a frame's capture instant is the Origin of its time plus its Marker's CaptureNS.
type originTrace struct {
	mu    sync.Mutex
	moves []originMove // in time order; the first is the Origin when the trace started
	quit  chan struct{}
	done  chan struct{}
	once  sync.Once
}

// originMove is an Origin and when the trace first saw it.
type originMove struct{ seen, origin time.Time }

// originEvery is how often an originTrace looks at the Origin.
const originEvery = 20 * time.Millisecond

// traceOrigin starts following the run's session's Origin, until the test ends.
func (r *congestionRun) traceOrigin() *originTrace {
	o := &originTrace{
		moves: []originMove{{seen: time.Now(), origin: r.sess.Origin()}},
		quit:  make(chan struct{}), done: make(chan struct{}),
	}
	go func() {
		defer close(o.done)
		tick := time.NewTicker(originEvery)
		defer tick.Stop()
		for {
			select {
			case <-o.quit:
				return
			case now := <-tick.C:
				origin := r.sess.Origin()
				o.mu.Lock()
				if !origin.Equal(o.moves[len(o.moves)-1].origin) {
					o.moves = append(o.moves, originMove{seen: now, origin: origin})
				}
				o.mu.Unlock()
			}
		}
	}()
	r.t.Cleanup(func() {
		o.once.Do(func() { close(o.quit) })
		<-o.done
	})
	return o
}

// at returns the Origin at t: the last one seen at or before t (the first one for an earlier t).
func (o *originTrace) at(t time.Time) time.Time {
	o.mu.Lock()
	defer o.mu.Unlock()
	origin := o.moves[0].origin
	for _, m := range o.moves[1:] {
		if m.seen.After(t) {
			break
		}
		origin = m.origin
	}
	return origin
}

// skips returns how often the Origin moved, and by how much in all.
func (o *originTrace) skips() (n int, by time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.moves) - 1, o.moves[len(o.moves)-1].origin.Sub(o.moves[0].origin)
}

// ---- the queue's samples ----

// queueSample is one sample of a queueing Link's wait.
type queueSample struct {
	at     time.Time
	queued time.Duration
}

// queueSampler samples a queueing Link's Queued() every queueSampleEvery, from its start until stop.
type queueSampler struct {
	mu      sync.Mutex
	samples []queueSample
	quit    chan struct{}
	done    chan struct{}
	once    sync.Once
}

// sampleQueue starts sampling link. The samples end at stop, or when the test ends.
func (r *congestionRun) sampleQueue(link *sfutest.Link) *queueSampler {
	s := &queueSampler{quit: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		tick := time.NewTicker(queueSampleEvery)
		defer tick.Stop()
		for {
			select {
			case <-s.quit:
				return
			case now := <-tick.C:
				q := link.Queued()
				s.mu.Lock()
				s.samples = append(s.samples, queueSample{at: now, queued: q})
				s.mu.Unlock()
			}
		}
	}()
	r.t.Cleanup(func() { s.stop() })
	return s
}

// stop ends the sampling and returns the samples.
func (s *queueSampler) stop() []queueSample {
	s.once.Do(func() { close(s.quit) })
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.samples)
}

// ---- the uplink ----

// uplinkPeer is the peer under which an uplink's FaultConn sees every datagram the core sends, whatever its
// destination: SetInboundLink(uplinkPeer, l) puts l on the whole uplink.
var uplinkPeer net.Addr = uplinkDest{}

// uplinkDest is a datagram's destination as an uplink's FaultConn sees it: the address, under the one name of
// uplinkPeer.
type uplinkDest struct{ to net.Addr }

func (uplinkDest) Network() string { return "udp" }
func (uplinkDest) String() string  { return "uplink" }

// uplink is the core's UDP socket with a bottleneck on what it sends: a net.PacketConn for webrtc.NewICEUDPMux. What
// the core writes goes into a pipe, which an sfutest.FaultConn reads as datagrams from uplinkPeer; a goroutine takes
// what the FaultConn lets through, when its Link lets it go, and writes it to the socket. What the core reads comes
// straight from the socket. Close closes everything and waits for the goroutines.
type uplink struct {
	sock net.PacketConn
	in   *datagramPipe
	wire *sfutest.FaultConn
	done chan struct{} // closed when the forwarder has ended

	closeOnce sync.Once
	closeErr  error
}

// newUplink binds a loopback UDP socket and starts the forwarder. The ICE mux that the uplink is given closes it.
func newUplink(t *testing.T) *uplink {
	t.Helper()
	var lc net.ListenConfig
	sock, err := lc.ListenPacket(context.Background(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	in := newDatagramPipe(sock.LocalAddr())
	u := &uplink{sock: sock, in: in, wire: sfutest.NewFaultConn(in), done: make(chan struct{})}
	go u.forward()
	return u
}

// SetLink puts l on the uplink, either kind (nil: none).
func (u *uplink) SetLink(l *sfutest.Link) { u.wire.SetInboundLink(uplinkPeer, l) }

// forward writes what leaves the FaultConn to its destination, until the FaultConn is closed.
func (u *uplink) forward() {
	defer close(u.done)
	buf := make([]byte, 65535)
	for {
		n, addr, err := u.wire.ReadFrom(buf)
		if err != nil {
			return
		}
		if d, ok := addr.(uplinkDest); ok && d.to != nil {
			_, _ = u.sock.WriteTo(buf[:n], d.to) // a datagram the socket refuses is lost, as on a network
		}
	}
}

// ReadFrom implements net.PacketConn: straight from the socket.
func (u *uplink) ReadFrom(b []byte) (int, net.Addr, error) { return u.sock.ReadFrom(b) }

// WriteTo implements net.PacketConn: into the bottleneck. It never blocks.
func (u *uplink) WriteTo(b []byte, addr net.Addr) (int, error) { return u.in.push(b, addr) }

// Close implements net.PacketConn.
func (u *uplink) Close() error {
	u.closeOnce.Do(func() {
		_ = u.wire.Close() // closes the pipe: the FaultConn's reader and the forwarder end
		<-u.done
		u.closeErr = u.sock.Close()
	})
	return u.closeErr
}

// LocalAddr implements net.PacketConn.
func (u *uplink) LocalAddr() net.Addr { return u.sock.LocalAddr() }

// SetDeadline implements net.PacketConn: the read deadline (writes never block).
func (u *uplink) SetDeadline(t time.Time) error { return u.sock.SetReadDeadline(t) }

// SetReadDeadline implements net.PacketConn.
func (u *uplink) SetReadDeadline(t time.Time) error { return u.sock.SetReadDeadline(t) }

// SetWriteDeadline implements net.PacketConn. Writes never block, so there is nothing to abort; a deadline on the
// socket would only fail the forwarder's writes.
func (u *uplink) SetWriteDeadline(time.Time) error { return nil }

// datagram is one datagram in a datagramPipe, with its destination.
type datagram struct {
	b  []byte
	to net.Addr
}

// datagramPipe is the read side of an uplink for its FaultConn: a net.PacketConn whose ReadFrom returns the datagrams
// pushed into it, in order, each from its destination as an uplinkDest. It never loses or blocks a push; it has no
// deadlines (the FaultConn sets none on it but the zero one).
type datagramPipe struct {
	local  net.Addr
	wake   chan struct{} // capacity 1: a datagram came
	closed chan struct{}

	mu     sync.Mutex
	queue  []datagram
	isShut bool
}

func newDatagramPipe(local net.Addr) *datagramPipe {
	return &datagramPipe{local: local, wake: make(chan struct{}, 1), closed: make(chan struct{})}
}

// push queues a copy of b for to.
func (p *datagramPipe) push(b []byte, to net.Addr) (int, error) {
	p.mu.Lock()
	if p.isShut {
		p.mu.Unlock()
		return 0, net.ErrClosed
	}
	p.queue = append(p.queue, datagram{b: bytes.Clone(b), to: to})
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
	return len(b), nil
}

// ReadFrom implements net.PacketConn: the oldest datagram, once there is one; net.ErrClosed after Close.
func (p *datagramPipe) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		p.mu.Lock()
		if len(p.queue) > 0 {
			d := p.queue[0]
			p.queue[0] = datagram{}
			p.queue = p.queue[1:]
			p.mu.Unlock()
			return copy(b, d.b), uplinkDest{to: d.to}, nil
		}
		shut := p.isShut
		p.mu.Unlock()
		if shut {
			return 0, nil, net.ErrClosed
		}
		select {
		case <-p.wake:
		case <-p.closed:
		}
	}
}

// WriteTo implements net.PacketConn; nothing writes to the read side.
func (p *datagramPipe) WriteTo([]byte, net.Addr) (int, error) {
	return 0, fmt.Errorf("itest: the uplink's pipe is read only")
}

// Close implements net.PacketConn: ReadFrom returns net.ErrClosed from now on, and push refuses.
func (p *datagramPipe) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.isShut {
		p.isShut = true
		close(p.closed)
	}
	return nil
}

func (p *datagramPipe) LocalAddr() net.Addr              { return p.local }
func (p *datagramPipe) SetDeadline(time.Time) error      { return nil }
func (p *datagramPipe) SetReadDeadline(time.Time) error  { return nil }
func (p *datagramPipe) SetWriteDeadline(time.Time) error { return nil }
