package sfu_test

import (
	"slices"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/server/sfu"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// The acceptance test of downlink adaptation (README S84): integration 16 of 02 §17, through sfutest.Harness with
// real PeerConnections, fake media at the Auto preset's rates, a bottleneck in front of the viewer (sfutest.Link on
// the SFU's FaultConn) and REMBs from rembsim's model of that link (ViewerOptions.REMB). The allocator's unit tests,
// and the same cases in fake time, are allocator_test.go's; the pacer's are pacer_test.go's.

// TestDownlinkAdaptation is integration 16 of 02 §17. The viewer's REMB comes from rembsim, on a link of 20 Mbit/s.
//
//   - (a) Idle, then motion: the focused share's full layer runs at 0.2 Mbit/s for 30 s, then at 8 Mbit/s for 30 s.
//     The viewer's REMB stays far below what it gets (it may grow by 8 % a second only), and the viewer stays on high
//     without a bandwidth event: a jump from an idle screen to motion is never compared with REMB.
//   - (b) Throttle: the link narrows to 3 Mbit/s while the full layer runs at 8 Mbit/s. Within 3 s the share is
//     forwarded low with the reason bandwidth. After the 10 s backoff a trial upgrade brings the full layer back, and
//     fails: the link still can't carry it. The backoff is 20 s from then.
//   - (c) Recovery: the link has its 20 Mbit/s again. The viewer is back on high within one backoff period (20 s)
//     plus the 6 s probe, without anything from the test; the trial has passed, and the allocator is in normal
//     again, within those 26 s plus a second for the end of the backoff and one for the end of the trial, which the
//     ticker looks for once a second (and a little room for a loaded machine).
//
// 02 §10.2 expects a trial on a link that still can't carry the full layer to fail on loss more often than on a
// falling REMB, because Chrome's estimate is low by then. With rembsim as 02 §17 specifies it the estimate is still
// where the throttle left it (it never falls without overuse), and the REMB falls within a few hundred milliseconds of
// the trial, before a receiver report shows the loss: the test wants the trial to fail, for either reason, and logs
// which.
func TestDownlinkAdaptation(t *testing.T) {
	h := newHarness(t, sfutest.HarnessOptions{FaultyUDP: true})
	ctx := ctxFor(t, 4*time.Minute)
	const share = sfu.ShareID("s_downlink")

	// The sharer: the Auto preset's layers (f 1080p60 at 8 Mbit/s, q 360p15 at 0.3 Mbit/s) and audio, the full layer
	// idle to begin with. Its GOP is longer than the test: every keyframe after the first is one the SFU asked for.
	pubConn, _ := join(t, h, "alice", "c-pub", sfu.RoleFull)
	startShare(t, pubConn, share, sfu.PresetAuto)
	src, err := fake.New(fake.Config{GOP: 5 * time.Minute, Audio: true, Seed: 84})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	src.SetBitrate("f", 200_000)
	pub := newPublisher(t, src, sfutest.LoopbackSettings())
	if _, err := sfutest.Publish(ctx, pubConn, pub, 1, 1, share); err != nil {
		t.Fatalf("publish negotiation: %v", err)
	}
	if err := pub.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitLive(t, h, share)

	// The viewer, on a socket of its own, so that the SFU's socket can put the link in front of its address.
	settings, socket := faultySettings(t)
	link := sfutest.NewLink(20_000_000)
	h.Wire.SetLink(socket.LocalAddr(), link)
	viewConn, viewSig := join(t, h, "bob", "c-view", sfu.RoleViewer)
	viewer, err := sfutest.NewViewer(sfutest.ViewerOptions{Settings: settings, REMB: link})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := viewer.Close(); err != nil {
			t.Errorf("Viewer.Close: %v", err)
		}
	})
	viewSig.Attach(viewConn, viewer)
	subscribe(t, viewConn, sfu.SubscriptionUpdate{Share: share, Video: sfu.QualityHigh, Audio: true})
	video := streamTrack(t, viewer, share, webrtc.RTPCodecTypeVideo)
	waitFrames(t, video, 0, "f", 15)
	state := func(forwarded sfu.Quality, reason sfu.SubReason) sfu.SubscriptionStateEvent {
		return sfu.SubscriptionStateEvent{Share: share, Requested: sfu.QualityHigh, Forwarded: forwarded, Audio: true,
			Reason: reason}
	}
	high, capped := state(sfu.QualityHigh, ""), state(sfu.QualityLow, sfu.SubReasonBandwidth)
	waitSubState(t, viewSig, high)
	downlink := func() sfu.DownlinkState {
		t.Helper()
		st, err := viewConn.Downlink(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	// waitEvent waits until the client has heard want after its first from events, for at most d, and returns its
	// index among the events and when the test saw it.
	waitEvent := func(from int, want sfu.SubscriptionStateEvent, d time.Duration, what string) (int, time.Time) {
		t.Helper()
		deadline := time.Now().Add(d)
		for {
			evs := subEvents(viewSig, share)
			if i := slices.Index(evs[min(from, len(evs)):], want); i >= 0 {
				return from + i, time.Now()
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: no %+v within %v; the client heard %+v since, and the allocator is %+v", what, want, d,
					evs[min(from, len(evs)):], downlink())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	bandwidthEvents := func(from int) []sfu.SubscriptionStateEvent {
		var out []sfu.SubscriptionStateEvent
		for _, ev := range subEvents(viewSig, share)[from:] {
			if ev.Reason == sfu.SubReasonBandwidth {
				out = append(out, ev)
			}
		}
		return out
	}

	// ---- (a) idle, then motion ----
	mark := len(subEvents(viewSig, share))
	time.Sleep(30 * time.Second)
	src.SetBitrate("f", 8_000_000)
	time.Sleep(30 * time.Second)
	rembs := viewer.REMBs()
	if len(rembs) < 50 {
		t.Fatalf("(a): the viewer sent %d REMBs in a minute, want one a second", len(rembs))
	}
	last := rembs[len(rembs)-1].Bitrate
	stats := viewConn.Stats()
	t.Logf("(a): the last REMB %.0f bit/s, the viewer gets %d bit/s, downlink estimate %d", last,
		stats.Subscriptions[0].Video.Bitrate, stats.DownlinkEstimate)
	if last >= 8_000_000 {
		t.Errorf("(a): the REMB reached %.0f bit/s; the case is about a REMB below the forwarded rate", last)
	}
	if evs := bandwidthEvents(mark); len(evs) != 0 {
		t.Fatalf("(a): the viewer heard %+v; want no bandwidth event for a jump from an idle screen to motion", evs)
	}
	if st := downlink(); st.State != "normal" {
		t.Fatalf("(a): the allocator is %+v, want normal", st)
	}
	if got := subEvents(viewSig, share); got[len(got)-1] != high {
		t.Fatalf("(a): the client's last event is %+v, want %+v", got[len(got)-1], high)
	}
	if runs := layerRuns(sfutest.WithoutPartialTail(video.Packets())); runs != "f" {
		t.Errorf("(a): the viewer got the layers %q, want f alone", runs)
	}

	// ---- (b) throttle ----
	mark = len(subEvents(viewSig, share))
	link.SetCapacity(3_000_000)
	narrowed := time.Now()
	cappedAt, seen := waitEvent(mark, capped, 3*time.Second, "(b) the throttle")
	st := downlink()
	t.Logf("(b): forwarded low %v after the link narrowed: %+v", seen.Sub(narrowed).Round(time.Millisecond), st)
	if st.State != "limited" || st.Backoff != sfu.BackoffStart {
		t.Errorf("(b): the allocator is %+v, want limited with a backoff of %v", st, sfu.BackoffStart)
	}
	// The trial: the caps are lifted after the backoff, the full layer comes back, and the viewer goes back to low.
	trial, trialAt := waitEvent(cappedAt, high, sfu.BackoffStart+4*time.Second, "(b) the first trial")
	_, failedAt := waitEvent(trial, capped, 2*sfu.ProbeTime, "(b) the first trial's end")
	st = downlink()
	t.Logf("(b): the trial began %v after the cap and failed %v later, on %s: %+v", trialAt.Sub(seen).Round(time.Millisecond),
		failedAt.Sub(trialAt).Round(time.Millisecond), st.Cause, st)
	if st.State != "limited" || st.Backoff != 2*sfu.BackoffStart || (st.Cause != "loss" && st.Cause != "remb") {
		t.Fatalf("(b): after the trial the allocator is %+v, want limited with a backoff of 20 s, on loss or REMB", st)
	}

	// ---- (c) recovery ----
	mark = len(subEvents(viewSig, share))
	link.SetCapacity(20_000_000)
	restored := time.Now()
	_, backAt := waitEvent(mark, high, 2*sfu.BackoffStart+sfu.ProbeTime, "(c) the recovery")
	t.Logf("(c): back on high %v after the link recovered", backAt.Sub(restored).Round(time.Millisecond))
	deadline := restored.Add(2*sfu.BackoffStart + sfu.ProbeTime + 2*time.Second + 2*time.Second)
	for st = downlink(); st.State != "normal"; st = downlink() {
		if time.Now().After(deadline) {
			t.Fatalf("(c): %v after the link recovered the allocator is %+v, want normal", time.Since(restored), st)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("(c): the trial passed %v after the link recovered", time.Since(restored).Round(time.Millisecond))
	videoMark := len(video.Packets())
	time.Sleep(2 * time.Second)
	if evs := bandwidthEvents(mark); len(evs) != 0 {
		t.Errorf("(c): the viewer heard %+v after the link recovered", evs)
	}
	if got := subEvents(viewSig, share); got[len(got)-1] != high {
		t.Errorf("(c): the client's last event is %+v, want %+v", got[len(got)-1], high)
	}
	if runs := layerRuns(sfutest.WithoutPartialTail(video.Packets()[videoMark:])); runs != "f" {
		t.Errorf("(c): the viewer gets the layers %q after the trial passed, want f", runs)
	}
	if n := h.SFU.Metrics().Downgrades[string(sfu.SubReasonBandwidth)]; n < 2 {
		t.Errorf("isshoni_sfu_downgrades_total{bandwidth} = %d, want the cap and the failed trial", n)
	}
	sent, passed, lost := link.Counts()
	t.Logf("the link carried %d of %d bytes of RTP and lost %d datagrams; the viewer sent %d REMBs", passed, sent, lost,
		len(viewer.REMBs()))
}
