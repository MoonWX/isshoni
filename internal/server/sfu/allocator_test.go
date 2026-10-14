package sfu

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest/rembsim"
)

// The downlink allocator's unit tests (README S84, 02 §10.2, §17 allocator_test). They run on the allocator's own
// clock, which they move by hand: scripted REMBs and receiver reports drive the state machine; the allocation is
// checked on its own; the three cases of integration 16 run in fake time with REMBs from rembsim, and so does the
// recovery after the backoff has reached 120 s; and a viewer Conn with real DownTracks shows what its client hears.
// The test with real PeerConnections and a real bottleneck is TestDownlinkAdaptation (adaptation_test.go).

const ms = int64(time.Millisecond)

// TestAllocatorREMB: which REMBs fall (02 §10.2). A REMB falls when it is at least 3 % below the previous one, which
// came less than 5 s before; after a longer gap it is only a new baseline. A copy of the same REMB, which reaches
// every DownTrack it names, changes nothing. A fall is an overuse only below the rate forwarded to the viewer: a REMB
// below a target the viewer isn't receiving yet never downgrades.
func TestAllocatorREMB(t *testing.T) {
	var a allocator
	now := t0
	a.forwarded.Store(5_000_000)
	remb := func(bps int64, wantFall bool) {
		t.Helper()
		if fell := a.onREMB(bps, now); fell != wantFall {
			t.Fatalf("REMB %d after %d: fell %v, want %v", bps, a.remb, fell, wantFall)
		}
	}
	step := func() allocDecision {
		t.Helper()
		return a.step(now, a.forwarded.Load(), 0)
	}

	remb(10_000_000, false) // the first is a baseline
	now += 100 * ms
	remb(9_701_000, false) // 2.99 % below
	now += 100 * ms
	remb(9_701_000*97/100, true) // 3 % below: a fall, but above the 5 Mbit/s forwarded
	remb(9_701_000*97/100, false)
	remb(9_701_000*97/100, false) // copies
	if a.fall != 9_701_000*97/100 || a.overuse != 0 {
		t.Fatalf("fall %d, overuse %d after a fall above the forwarded rate", a.fall, a.overuse)
	}
	// The viewer asked for high and gets the preview layer: its REMB is far below the full layer, and that says nothing.
	if d := step(); d.to != allocNormal || d.recap || d.cause != "" {
		t.Fatalf("a fall above the forwarded rate: %+v, want normal and no cap", d)
	}
	if a.fall != 0 {
		t.Error("the step did not take the fall")
	}
	// Below the forwarded rate: an overuse.
	now += 100 * ms
	remb(4_000_000, true)
	remb(4_000_000, false)
	now += 50 * ms
	remb(3_000_000, true) // two falls before the actor looks: the lowest counts
	if a.overuse != 3_000_000 {
		t.Fatalf("overuse %d, want the lowest REMB below the forwarded rate", a.overuse)
	}
	if d := step(); d.to != allocLimited || !d.recap || d.cause != causeREMB || a.budget != 2_700_000 {
		t.Fatalf("an overuse REMB: %+v, budget %d; want limited at 0.9 × 3 Mbit/s", d, a.budget)
	}
	// After more than 5 s without one, a REMB is a baseline, however low.
	now += int64(rembFresh) + ms
	remb(100_000, false)
	// A REMB of nothing is ignored.
	remb(0, false)
	if a.remb != 100_000 {
		t.Errorf("latest REMB %d after a REMB of 0, want 100000", a.remb)
	}

	// So is one above 100 Gbit/s, on its way from the viewer's RTCP: the largest Pion parses (a mantissa of 2^18 - 1,
	// an exponent of 63) doesn't fit an int64, and must leave the latest REMB as it was on every architecture. Had it
	// replaced it, the next REMB of 6 Mbit/s would fall, below the 8 Mbit/s forwarded: an overuse.
	s, _ := newTicklessSFU(t)
	viewer, _ := join(t, s, "lounge", "bob", "c-b", RoleViewer)
	d, _ := testDownTrack(t, viewer, testShare(t, s, "s_1"), webrtc.RTPCodecTypeVideo)
	va := &viewer.alloc
	va.forwarded.Store(8_000_000)
	viewerREMB := func(bps float32) {
		t.Helper()
		d.handleRTCP([]rtcp.Packet{&rtcp.ReceiverEstimatedMaximumBitrate{Bitrate: bps, SSRCs: []uint32{0x7000}}})
		va.mu.Lock()
		latest, fall, overuse := va.remb, va.fall, va.overuse
		va.mu.Unlock()
		if latest != 6_000_000 || fall != 0 || overuse != 0 {
			t.Fatalf("after a REMB of %g: latest %d, fall %d, overuse %d; want 6000000 and no fall", bps, latest, fall,
				overuse)
		}
	}
	viewerREMB(6_000_000)
	viewerREMB(float32(math.Ldexp(1<<18-1, 63)))
	viewerREMB(float32(math.Inf(1)))
	viewerREMB(float32(math.NaN()))
	viewerREMB(6_000_000)
}

// TestAllocatorStates scripts the state machine of 02 §10.2 through every row of its table, with the budget each
// time, the backoff doubling up to 120 s and its reset, and the estimate ConnStats reports.
func TestAllocatorStates(t *testing.T) {
	var a allocator
	now := t0
	const fwd = 8_000_000
	step := func(want allocState, what string) allocDecision {
		t.Helper()
		d := a.step(now, fwd, 0)
		if d.to != want || a.state != want {
			t.Fatalf("%s: %+v, state %s; want %s", what, d, a.state, want)
		}
		return d
	}
	// A report covers the second before it.
	report := func(expected, lost int64) { a.onLoss(expected, lost, now-oneSecond, now) }
	a.forwarded.Store(fwd)
	a.onREMB(12_000_000, now)
	step(allocNormal, "a REMB above the forwarded rate")
	if got := a.estimate.Load(); got != 12_000_000 {
		t.Errorf("estimate in normal = %d, want the latest REMB", got)
	}

	// normal → limited on an overuse REMB: budget 0.9 × the REMB, backoff 10 s.
	now += 500 * ms
	a.onREMB(3_000_000, now)
	if d := step(allocLimited, "an overuse REMB"); !d.recap || d.cause != causeREMB || a.budget != 2_700_000 ||
		a.backoff != backoffStart {
		t.Fatalf("%+v, budget %d, backoff %v", d, a.budget, a.backoff)
	}
	if got := a.estimate.Load(); got != 2_700_000 {
		t.Errorf("estimate while limited = %d, want the budget", got)
	}
	limitedAt := now

	// A receiver report that covers time before a decision is about what was before it: no evidence.
	now += 500 * ms
	report(100, 60)
	step(allocLimited, "loss reported right after the cap")
	if a.since != limitedAt {
		t.Error("loss reported within a second of the decision restarted the backoff")
	}
	// limited, congestion again: the budget only goes down, and the backoff starts again.
	now += time.Second.Nanoseconds()
	a.onREMB(2_000_000, now)
	if d := step(allocLimited, "another overuse REMB"); !d.recap || a.budget != 1_800_000 || a.since != now {
		t.Fatalf("%+v, budget %d, since %d", d, a.budget, a.since)
	}
	limitedAt = now
	now += 2 * time.Second.Nanoseconds()
	a.onREMB(2_000_000, now) // no fall: a REMB goes out every second
	step(allocLimited, "a steady REMB")

	// Backoff elapsed, but 3 % loss over the last 3 s: no trial yet.
	now = limitedAt + int64(backoffStart) - ms
	step(allocLimited, "a millisecond before the backoff ends")
	now += ms
	report(100, 3)
	step(allocLimited, "the backoff has ended, with 3 % loss")
	// The report ages out of the 3 s: the trial.
	now += int64(recoverWindow) + ms
	if d := step(allocProbing, "the backoff has ended, without loss"); !d.lift {
		t.Fatalf("%+v: a trial lifts the caps", d)
	}

	// probing → limited on a fall of the REMB, even one above the forwarded rate: backoff 20 s, budget 0.9 × the REMB.
	trials := []time.Duration{20 * time.Second, 40 * time.Second, 80 * time.Second, 120 * time.Second, 120 * time.Second}
	for i, want := range trials {
		now += 500 * ms
		a.onREMB(10_000_000, now) // a baseline: more than 5 s since the last
		now += 500 * ms
		a.onREMB(9_000_000, now)
		d := step(allocLimited, fmt.Sprintf("trial %d with a falling REMB", i+1))
		if !d.recap || d.cause != causeREMB || a.backoff != want || a.budget != 8_100_000 {
			t.Fatalf("trial %d: %+v, backoff %v, budget %d; want backoff %v, budget 8100000", i+1, d, a.backoff,
				a.budget, want)
		}
		now += int64(a.backoff)
		step(allocProbing, fmt.Sprintf("after backoff %v", want))
	}
	// probing → limited on loss of 5 %, from what the reports said a second or more into the trial.
	trialAt := now
	now += 900 * ms
	report(1000, 400) // the report covers time before the trial: no evidence
	step(allocProbing, "loss reported right after the trial began")
	now = trialAt + oneSecond + 100*ms
	report(1000, 50)
	if d := step(allocLimited, "5 % loss in a trial"); d.cause != causeLoss || a.backoff != backoffMax {
		t.Fatalf("%+v, backoff %v", d, a.backoff)
	}
	if a.budget != fwd*85/100 { // the last REMB is too old to count
		t.Errorf("budget after a trial failed on loss = %d, want 0.85 × the forwarded rate", a.budget)
	}

	// probing for 6 s cleanly → normal, backoff 10 s again.
	now += int64(backoffMax)
	step(allocProbing, "after 120 s")
	now += int64(probeTime) - ms
	step(allocProbing, "a millisecond before the trial has passed")
	now += ms
	if d := step(allocNormal, "6 s without a fall, loss or drops"); d.recap || d.lift || a.backoff != backoffStart {
		t.Fatalf("%+v, backoff %v", d, a.backoff)
	}

	// normal → limited on loss ≥ 10 % over 2 s: min(0.9 × the latest REMB, 0.85 × the forwarded rate).
	now += 3 * time.Second.Nanoseconds()
	a.onREMB(6_000_000, now)
	report(15, 15) // too few packets for a verdict
	step(allocNormal, "15 packets lost")
	report(200, 20) // 35 lost of 215 over 2 s
	if d := step(allocLimited, "16 % loss"); d.cause != causeLoss || a.budget != 5_400_000 {
		t.Fatalf("%+v, budget %d; want 0.9 × the REMB, which is below 0.85 × the forwarded rate", d, a.budget)
	}

	// Without a REMB known: 0.85 × the forwarded rate. And drops: more than 50 a second.
	var b allocator
	now = t0
	b.step(now, fwd, 0)
	now += time.Second.Nanoseconds()
	if d := b.step(now, fwd, 50); d.to != allocNormal {
		t.Fatalf("50 drops in a second: %+v", d)
	}
	now += time.Second.Nanoseconds()
	if d := b.step(now, fwd, 50+51); d.to != allocLimited || d.cause != causeDrops || b.budget != fwd*85/100 {
		t.Fatalf("51 drops in a second: %+v, budget %d", d, b.budget)
	}
	if got := b.estimate.Load(); got != fwd*85/100 {
		t.Errorf("estimate = %d, want the budget", got)
	}
}

// TestAllocate: the budget is filled cheapest first (02 §10.2): the audio the viewer asks for, every thumbnail, then
// the high requests, the newest first, each at its full layer if that fits what is left and else at its preview
// layer, which counts in its place. A share without a smaller layer is never capped.
func TestAllocate(t *testing.T) {
	const (
		f     = 4_000_000
		q     = 300_000
		audio = 100_000
	)
	high := func(order uint64) allocItem {
		return allocItem{high: true, order: order, audio: audio, full: f, preview: q, cappable: true}
	}
	thumb := allocItem{preview: q}
	single := allocItem{high: true, order: 9, full: 6_000_000} // one layer, no audio
	for _, tc := range []struct {
		name   string
		budget int64
		items  []allocItem
		want   []bool
	}{
		{"one high that fits", f + audio, []allocItem{high(1)}, []bool{false}},
		{"one high that doesn't", f + audio - 1, []allocItem{high(1)}, []bool{true}},
		{"audio and thumbnails first", f + audio + 2*q - 1, []allocItem{thumb, high(1), thumb}, []bool{false, true, false}},
		{"thumbnails are kept, whatever the budget", 0, []allocItem{thumb, thumb}, []bool{false, false}},
		{"the newest high first", 2*f + q + 2*audio, []allocItem{high(1), high(3), high(2)}, []bool{true, false, false}},
		{"the oldest are capped first", f + 2*q + 3*audio, []allocItem{high(2), high(1), high(3)}, []bool{true, true, false}},
		{"a capped request's preview layer counts", 2*audio + 4_200_000,
			[]allocItem{high(1), {high: true, order: 2, audio: audio, full: 6_000_000, preview: q, cappable: true}},
			[]bool{true, true}},
		{"a single layer is never capped", 0, []allocItem{single, high(1)}, []bool{false, true}},
		{"nothing at all", 0, nil, []bool{}},
	} {
		if got := allocate(tc.budget, tc.items); !slices.Equal(got, tc.want) {
			t.Errorf("%s: capped %v, want %v", tc.name, got, tc.want)
		}
	}
}

// downlinkSim is one viewer of one share in fake time, for the cases of integration 16 without PeerConnections: its
// allocator, the link in front of it with a capacity the test scripts, and REMBs from rembsim's model of that link.
// The viewer asks for high and for the audio. What it gets follows the allocator's cap after a keyframe's wait; the
// link loses what exceeds its capacity, which the receiver reports say once a second and rembsim sees at once.
type downlinkSim struct {
	t        *testing.T
	a        allocator
	model    rembsim.Model
	start    int64
	now      int64
	capacity float64 // bit/s
	full     float64 // the full layer's bitrate now

	capped   bool   // the allocator's cap on the subscription
	layer    string // what the viewer gets: "f" or "q"
	switchAt int64  // when the layer the cap asks for arrives (its keyframe); 0: none waits

	fwd                float64 // the rate forwarded, a 2 s moving average updated once a second (DownTrack.tick)
	second             float64 // bits forwarded in the current second
	expected, lostPkts float64 // video packets in the current second
	trials             []trial // what each trial did
}

// trial is one trial upgrade: when it began, how it ended and why, and the backoff after it.
type trial struct {
	at, end int64
	passed  bool
	cause   string
	backoff time.Duration
}

const (
	simStep     = 50 * time.Millisecond
	simKeyframe = 100 * time.Millisecond // a layer switch waits this long for the new layer's keyframe
	simPreview  = 300_000
	simAudio    = 128_000
	simPacket   = 1200 * 8
)

func newDownlinkSim(t *testing.T, capacity, full float64) *downlinkSim {
	return &downlinkSim{t: t, start: t0, now: t0, capacity: capacity, full: full, layer: "f"}
}

// run advances the simulation by d, or until stop (if not nil) holds.
func (s *downlinkSim) run(d time.Duration, stop func() bool) {
	for end := s.now + int64(d); s.now < end; {
		s.step()
		if stop != nil && stop() {
			return
		}
	}
}

func (s *downlinkSim) step() {
	dt := simStep.Seconds()
	s.now += int64(simStep)
	if s.switchAt != 0 && s.now >= s.switchAt {
		s.layer, s.switchAt = map[bool]string{true: "q", false: "f"}[s.capped], 0
	}
	video := s.full
	if s.layer == "q" {
		video = simPreview
	}
	forwarded := (video + simAudio) * dt
	received := min(forwarded, s.capacity*dt)
	s.second += forwarded
	pkts := video * dt / simPacket
	s.expected += pkts
	s.lostPkts += pkts * (1 - received/forwarded)
	wake := false
	if bps, send := s.model.Step(time.Duration(s.now-s.start), forwarded, received); send {
		wake = s.a.onREMB(int64(bps), s.now) || wake
	}
	tick := (s.now-s.start)%int64(time.Second) == 0
	if tick {
		// The DownTracks' bitrate, as the ticker computes it, and the video's receiver report of the second.
		if s.fwd == 0 {
			s.fwd = s.second
		} else {
			s.fwd += (1 - math.Exp(-0.5)) * (s.second - s.fwd)
		}
		s.second = 0
		wake = s.a.onLoss(int64(math.Round(s.expected)), int64(math.Round(s.lostPkts)), s.now-oneSecond, s.now) || wake
		s.expected, s.lostPkts = 0, 0
	}
	if tick || wake {
		s.decide()
	}
}

// decide runs the allocator's step and applies it, as Conn.adapt does.
func (s *downlinkSim) decide() {
	d := s.a.step(s.now, int64(s.fwd), 0)
	if d.from == allocProbing && d.to != allocProbing {
		tr := &s.trials[len(s.trials)-1]
		tr.end, tr.passed, tr.cause, tr.backoff = s.now, d.to == allocNormal, d.cause, s.a.backoff
	}
	switch {
	case d.lift:
		s.trials = append(s.trials, trial{at: s.now})
		s.setCapped(false)
	case d.recap:
		item := allocItem{high: true, order: 1, audio: simAudio, full: int64(s.full), preview: simPreview, cappable: true}
		s.setCapped(allocate(s.a.budget, []allocItem{item})[0])
	}
}

func (s *downlinkSim) setCapped(c bool) {
	s.capped = c
	want := map[bool]string{true: "q", false: "f"}[c]
	switch {
	case want == s.layer:
		s.switchAt = 0
	case s.switchAt == 0:
		s.switchAt = s.now + int64(simKeyframe)
	}
}

func (s *downlinkSim) since(at int64) time.Duration { return time.Duration(s.now - at) }

// TestAllocatorRembsim runs the three cases of integration 16 (02 §17) in fake time, with REMBs from rembsim instead of
// scripted values, and then a long throttle, through which the backoff reaches 120 s, and its end.
func TestAllocatorRembsim(t *testing.T) {
	s := newDownlinkSim(t, 20_000_000, 200_000)

	// (a) Idle, then motion: the full layer at 0.2 Mbit/s for 30 s, then at 8 Mbit/s for 30 s. REMB stays far below
	// the forwarded rate (it may grow by 8 % a second only), and that is no reason to cap.
	s.run(30*time.Second, nil)
	s.full = 8_000_000
	s.run(30*time.Second, nil)
	if s.a.state != allocNormal || s.capped || s.layer != "f" || len(s.trials) != 0 {
		t.Fatalf("(a): state %s, capped %v, layer %s, trials %d; want normal on f throughout", s.a.state, s.capped,
			s.layer, len(s.trials))
	}
	if est := s.model.Estimate(); est >= 8_000_000 {
		t.Errorf("(a): the REMB reached %.0f; the case is about a REMB below the forwarded rate", est)
	}
	t.Logf("(a): REMB %.0f bit/s at the end, the forwarded rate %.0f", s.model.Estimate(), s.fwd)

	// (b) The link narrows to 3 Mbit/s: within 3 s the viewer gets the preview layer, and the first trial fails.
	s.capacity = 3_000_000
	narrowed := s.now
	s.run(3*time.Second, func() bool { return s.layer == "q" })
	if s.layer != "q" || s.a.state != allocLimited {
		t.Fatalf("(b): %v after the link narrowed: layer %s, state %s; want q, limited", s.since(narrowed), s.layer, s.a.state)
	}
	t.Logf("(b): on the preview layer %v after the link narrowed, budget %d", s.since(narrowed), s.a.budget)
	s.run(15*time.Second, func() bool { return len(s.trials) == 1 && s.trials[0].end != 0 })
	if len(s.trials) != 1 || s.trials[0].passed || s.a.backoff != 20*time.Second {
		t.Fatalf("(b): trials %+v, backoff %v; want one that failed, and a backoff of 20 s", s.trials, s.a.backoff)
	}
	first := s.trials[0]
	t.Logf("(b): the first trial began %v after the cap and failed after %v, on %s", time.Duration(first.at-narrowed),
		time.Duration(first.end-first.at), first.cause)

	// (c) The link has room again: back on high within one backoff (20 s) plus the 6 s of the trial.
	s.capacity = 20_000_000
	restored := s.now
	s.run(30*time.Second, func() bool { return s.a.state == allocNormal })
	// The ticker looks once a second: the end of the backoff and of the trial may each wait for its next look.
	if s.a.state != allocNormal || s.layer != "f" || s.since(restored) > 20*time.Second+probeTime+2*time.Second {
		t.Fatalf("(c): %v after the link recovered: state %s, layer %s; want normal on f within 26 s",
			s.since(restored), s.a.state, s.layer)
	}
	if last := s.trials[len(s.trials)-1]; !last.passed || len(s.trials) != 2 {
		t.Fatalf("(c): trials %+v, want the second to pass", s.trials)
	}
	t.Logf("(c): back on f %v after the link recovered", time.Duration(s.trials[1].at-restored))

	// A long throttle: every trial fails, the backoff doubles up to 120 s and stays there.
	s.capacity = 3_000_000
	s.run(10*time.Minute, nil)
	var backoffs []string
	for _, tr := range s.trials[2:] {
		if tr.passed {
			t.Fatalf("a trial passed on a link of 3 Mbit/s: %+v", tr)
		}
		backoffs = append(backoffs, tr.backoff.String())
	}
	if got := strings.Join(backoffs, " "); !strings.HasPrefix(got, "20s 40s 1m20s 2m0s 2m0s") {
		t.Fatalf("the backoffs after each failed trial: %s; want 20 s, 40 s, 80 s, then 120 s", got)
	}
	// It ends: back on high within the 120 s backoff plus the trial's 6 s.
	s.capacity = 20_000_000
	restored = s.now
	s.run(3*time.Minute, func() bool { return s.a.state == allocNormal })
	if s.a.state != allocNormal || s.layer != "f" || s.since(restored) > backoffMax+probeTime+2*time.Second {
		t.Fatalf("after the long throttle: %v, state %s, layer %s; want normal on f within 126 s",
			s.since(restored), s.a.state, s.layer)
	}
	if s.a.backoff != backoffStart {
		t.Errorf("backoff after a trial passed = %v, want %v", s.a.backoff, backoffStart)
	}
	t.Logf("after the long throttle: back on f %v after the link recovered", s.since(restored)-probeTime)
}

// TestAllocatorReports: what a video DownTrack's receiver reports give the allocator (DownTrack.noteReport). Loss is
// what the next report adds to the last one, as the link lost it: the sequence numbers the SFU never sent (a gap the
// writer passed over) are no loss of the link's, and a late number sent into such a gap counts again. The first report
// of a stream, the first of a new SSRC, and one that comes more than 2 s after the last only start the count.
func TestAllocatorReports(t *testing.T) {
	s, _ := newTicklessSFU(t)
	viewer, _ := join(t, s, "lounge", "bob", "c-b", RoleViewer)
	sh := testShare(t, s, "s_1")
	d, _ := testDownTrack(t, viewer, sh, webrtc.RTPCodecTypeVideo)
	a := &viewer.alloc
	var w rtpWriteState
	now := t0
	sent := func(seqs ...uint16) {
		for _, seq := range seqs {
			d.noteSeq(&w, seq)
		}
	}
	report := func(highest, lost uint32) {
		d.noteReport(rtcp.ReceptionReport{SSRC: 0x7000, LastSequenceNumber: highest, TotalLost: lost}, now)
	}
	samples := func() string {
		a.mu.Lock()
		defer a.mu.Unlock()
		var out []string
		for _, l := range a.losses {
			out = append(out, fmt.Sprintf("%d/%d", l.lost, l.expected))
		}
		a.losses = nil
		return strings.Join(out, " ")
	}
	check := func(what, want string) {
		t.Helper()
		if got := samples(); got != want {
			t.Errorf("%s: lost/expected %q, want %q", what, got, want)
		}
	}

	sent(100, 101, 102)
	report(102, 0)
	check("the first report", "")
	// 100 more, 5 of them lost on the way.
	for seq := uint16(103); seq <= 202; seq++ {
		sent(seq)
	}
	now += time.Second.Nanoseconds()
	report(202, 5)
	check("5 lost of 100", "5/100")
	// The writer passes over 10 numbers (an upstream gap): the viewer counts them lost, the link didn't lose them.
	sent(203, 214, 215)
	now += time.Second.Nanoseconds()
	report(215, 15)
	check("a gap the SFU left", "0/3")
	// Another gap (217 to 221), of which 219 is sent late after all; of the 4 numbers sent, the link lost 2. The
	// viewer counts 6 lost: the 4 numbers never sent and the 2 the link lost.
	sent(216, 222, 223, 219)
	now += time.Second.Nanoseconds()
	report(223, 15+6)
	check("a gap with a late number", "2/4")
	// A gap the report hasn't reached yet waits for the next one.
	for seq := uint16(224); seq <= 250; seq++ {
		sent(seq)
	}
	sent(300)
	now += time.Second.Nanoseconds()
	report(250, 21)
	check("a report before the gap", "0/27")
	now += time.Second.Nanoseconds()
	report(300, 21+49)
	check("the report after the gap", "0/1")
	// A report 2 s and more after the last: an outage, not congestion.
	sent(301, 400)
	now += 2*time.Second.Nanoseconds() + ms
	report(400, 70+98)
	check("a report after an outage", "")
	// A new SSRC: a rebuilt binding starts its count again.
	now += time.Second.Nanoseconds()
	d.noteReport(rtcp.ReceptionReport{SSRC: 0x7777, LastSequenceNumber: 500, TotalLost: 0}, now)
	check("a new SSRC", "")
	// Audio DownTracks don't count.
	da, _ := testDownTrack(t, viewer, sh, webrtc.RTPCodecTypeAudio)
	da.noteReport(rtcp.ReceptionReport{SSRC: 0x7001, LastSequenceNumber: 10, TotalLost: 0}, now)
	now += time.Second.Nanoseconds()
	da.noteReport(rtcp.ReceptionReport{SSRC: 0x7001, LastSequenceNumber: 110, TotalLost: 50}, now)
	check("audio", "")
}

// TestAllocatorReportFlood: a viewer that sends receiver reports as fast as it likes costs the allocator no more
// than one that sends five a second. A report less than 200 ms after the last one that counted is folded into the
// next, which covers its interval and the gaps in it: of 10 000 reports in a second, 5 are kept, and they still add
// up to what was sent and lost. And the allocator keeps at most maxLossReports reports, the newest.
func TestAllocatorReportFlood(t *testing.T) {
	s, _ := newTicklessSFU(t)
	viewer, _ := join(t, s, "lounge", "bob", "c-b", RoleViewer)
	d, _ := testDownTrack(t, viewer, testShare(t, s, "s_1"), webrtc.RTPCodecTypeVideo)
	a := &viewer.alloc
	var w rtpWriteState
	// Report i comes 100 µs after the one before, with one more packet sent, and one in 20 lost on the way. Halfway
	// through the writer passes over 10 sequence numbers (an upstream gap), which the viewer counts as lost too.
	const reports, gap = 10_000, 10
	seq := func(i int) uint32 {
		if i >= reports/2 {
			return uint32(1000 + i + gap)
		}
		return uint32(1000 + i)
	}
	for i := 0; i <= reports; i++ { // and one more, exactly a second after the first
		d.noteSeq(&w, uint16(seq(i)))
		lost := uint32(i / 20)
		if i >= reports/2 {
			lost += gap
		}
		at := t0 + int64(i)*int64(100*time.Microsecond)
		d.noteReport(rtcp.ReceptionReport{SSRC: 0x7000, LastSequenceNumber: seq(i), TotalLost: lost}, at)
	}
	a.mu.Lock()
	kept := slices.Clone(a.losses)
	a.mu.Unlock()
	var expected, lost int64
	for _, l := range kept {
		expected += l.expected
		lost += l.lost
	}
	if len(kept) > 5 || expected != reports || lost != reports/20 {
		t.Fatalf("after %d reports in a second: %d kept, %d packets expected and %d lost; want at most 5, %d and %d",
			reports+1, len(kept), expected, lost, reports, reports/20)
	}
	if from := kept[0].from; from != t0 {
		t.Errorf("the first report kept covers the time from %v on, want from the first report", time.Duration(from-t0))
	}

	// The allocator's own bound: past maxLossReports the oldest go, and what is older than 3 s goes first.
	var b allocator
	for i := range 2 * maxLossReports {
		b.onLoss(int64(100+i), 0, t0, t0+int64(i))
	}
	if n, oldest := len(b.losses), b.losses[0].expected; n != maxLossReports || oldest != 100+maxLossReports {
		t.Fatalf("after %d reports: %d kept, the oldest of %d packets; want %d, the newest", 2*maxLossReports, n,
			oldest, maxLossReports)
	}
	b.onLoss(1, 0, t0, t0+int64(recoverWindow)+2*maxLossReports)
	if len(b.losses) != 1 {
		t.Errorf("a report more than 3 s after the others: %d kept, want 1", len(b.losses))
	}
}

// TestAllocatorEvents runs the allocator on a viewer Conn with real DownTracks (02 §10.2, §6.2): the client hears each
// change of what it gets once, when it has happened, with the reason bandwidth while capped. A focus change while
// limited serves the newest request first; a trial lifts the caps, and one that fails caps again with a longer
// backoff; once a trial has passed the caps are gone for good. isshoni_sfu_downgrades_total{reason="bandwidth"} counts
// each time the client hears the reason anew, and ConnStats carries the estimate.
func TestAllocatorEvents(t *testing.T) {
	s, _ := newTicklessSFU(t)
	s.subEventEvery = time.Millisecond
	ctx := testCtx(t)
	viewer, sig := join(t, s, "lounge", "bob", "c-b", RoleViewer)
	const one, two = ShareID("s_1"), ShareID("s_2")
	sh1, sh2 := testShare(t, s, one), testShare(t, s, two)
	script := &eventScript{t: t, sig: sig}
	event := func(id ShareID, requested, forwarded Quality, reason SubReason) SubscriptionStateEvent {
		return SubscriptionStateEvent{Share: id, Requested: requested, Forwarded: forwarded, Audio: id == one, Reason: reason}
	}
	request := func(items ...SubscriptionUpdate) {
		t.Helper()
		if errs, err := viewer.UpdateSubscriptions(ctx, items); err != nil || errs[0] != nil {
			t.Fatalf("UpdateSubscriptions = %v, %v", errs, err)
		}
	}
	// onActor runs fn on the viewer's actor after everything posted before it.
	onActor := func(fn func()) {
		t.Helper()
		done := make(chan struct{})
		viewer.post(func() { fn(); close(done) })
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("the viewer's actor did not get to its events")
		}
	}
	key := keyPayload(t)
	type share struct {
		f, q, audio *feeder
	}
	layers := func(sh *Share) share {
		f, _ := testLayer(t, sh, SlotF, 0xf00)
		q, _ := testLayer(t, sh, SlotQ, 0xa00)
		a, _ := testLayer(t, sh, SlotAudio, 0xaa)
		f.stats.bitrate.Store(4_000_000)
		q.stats.bitrate.Store(300_000)
		a.stats.bitrate.Store(100_000)
		return share{f: &feeder{l: f}, q: &feeder{l: q}, audio: &feeder{l: a}}
	}
	l1, l2 := layers(sh1), layers(sh2)

	// share 1 focused (high, with its audio), share 2 a thumbnail.
	request(SubscriptionUpdate{Share: one, Video: QualityHigh, Audio: true}, SubscriptionUpdate{Share: two, Video: QualityLow})
	sub1, _ := connectedSubscription(t, viewer, one)
	sub2, _ := connectedSubscription(t, viewer, two)
	l1.audio.send([]byte{1})
	script.next("the audio", event(one, QualityHigh, QualityOff, ""))
	l1.f.send(key)
	script.next("the video", event(one, QualityHigh, QualityHigh, ""))
	l2.q.send(key)
	script.next("the thumbnail", event(two, QualityLow, QualityLow, ""))
	// What the viewer gets: 4.4 Mbit/s and 0.3 Mbit/s.
	sub1.video.stats.bitrate.Store(4_000_000)
	sub1.audio.stats.bitrate.Store(100_000)
	sub2.video.stats.bitrate.Store(300_000)
	remb := func(bps float32) {
		sub1.video.handleRTCP([]rtcp.Packet{&rtcp.ReceiverEstimatedMaximumBitrate{Bitrate: bps, SSRCs: []uint32{0x7000}}})
	}
	adapt := func() { onActor(viewer.adapt) }
	advance := func(d time.Duration) { s.allocSkew.Add(int64(d)) }
	state := func() allocState {
		var st allocState
		onActor(func() { st = viewer.alloc.state })
		return st
	}
	adapt()
	remb(10_000_000)
	adapt()
	script.same("a REMB in normal")
	if st := viewer.Stats(); st.DownlinkEstimate != 10_000_000 {
		t.Errorf("DownlinkEstimate = %d, want the REMB", st.DownlinkEstimate)
	}

	// The REMB falls below what the viewer gets: the high request doesn't fit 0.9 × 4 Mbit/s and is capped. Its client
	// hears it once the preview layer's keyframe has arrived, and the thumbnail is kept.
	advance(500 * time.Millisecond)
	remb(4_000_000)
	onActor(func() {})
	if got := state(); got != allocLimited {
		t.Fatalf("after an overuse REMB the allocator is %s, want limited", got)
	}
	l1.f.send(deltaPayload)
	l1.q.send(key)
	script.next("capped", event(one, QualityHigh, QualityLow, SubReasonBandwidth))
	if st := viewer.Stats(); st.DownlinkEstimate != 3_600_000 || st.Subscriptions[0].Reason != SubReasonBandwidth {
		t.Errorf("Stats while limited = %+v, want the budget as the estimate and the reason bandwidth", st)
	}
	for range 3 {
		advance(time.Second)
		adapt()
		l1.q.send(deltaPayload)
	}
	script.same("ticks while limited")

	// A focus change while limited: share 2 high, share 1 low. The newest high request is served at the full layer
	// when that fits the budget; share 1, a thumbnail now, has nothing left to cap.
	l2.f.l.stats.bitrate.Store(3_000_000)
	request(SubscriptionUpdate{Share: one, Video: QualityLow, Audio: true}, SubscriptionUpdate{Share: two, Video: QualityHigh})
	script.next("share 1 is a thumbnail now", event(one, QualityLow, QualityLow, ""))
	l2.q.send(deltaPayload)
	l2.f.send(key)
	script.next("share 2 focused, within the budget", event(two, QualityHigh, QualityHigh, ""))
	// And back: share 1 high again is the newest. 4 Mbit/s don't fit, so it is capped, and share 2 keeps its full
	// layer within what is left.
	request(SubscriptionUpdate{Share: one, Video: QualityHigh, Audio: true})
	script.next("share 1 high again, capped at once", event(one, QualityHigh, QualityLow, SubReasonBandwidth))
	if sub1.capReason != SubReasonBandwidth || sub2.capReason != SubReasonNone {
		t.Errorf("caps: share 1 %q, share 2 %q", sub1.capReason, sub2.capReason)
	}
	request(SubscriptionUpdate{Share: two, Video: QualityLow})
	l2.f.send(deltaPayload)
	l2.q.send(key)
	script.next("share 2 a thumbnail again", event(two, QualityLow, QualityLow, ""))

	// The backoff ends: a trial lifts the caps. The client hears at once that share 1 waits for its full layer (no
	// reason: only time stands between it and high), and then that it gets it.
	advance(backoffStart)
	adapt()
	if got := state(); got != allocProbing {
		t.Fatalf("after the backoff the allocator is %s, want probing", got)
	}
	script.next("the trial lifts the cap", event(one, QualityHigh, QualityLow, ""))
	l1.f.send(key)
	script.next("the trial", event(one, QualityHigh, QualityHigh, ""))
	// The REMB falls in the trial, even above what the viewer gets: capped again, and the next backoff is 20 s.
	remb(9_000_000) // a baseline: the last REMB is more than 5 s old
	advance(500 * time.Millisecond)
	remb(3_000_000)
	onActor(func() {})
	l1.q.send(key)
	script.next("the trial failed", event(one, QualityHigh, QualityLow, SubReasonBandwidth))
	var backoff time.Duration
	onActor(func() { backoff = viewer.alloc.backoff })
	if backoff != 20*time.Second {
		t.Errorf("backoff after a failed trial = %v, want 20 s", backoff)
	}

	// The next trial passes: 6 s without a fall, loss or drops, and the allocator is back in normal, where nothing is
	// capped.
	advance(20 * time.Second)
	adapt()
	script.next("the second trial lifts the cap", event(one, QualityHigh, QualityLow, ""))
	l1.f.send(key)
	script.next("the second trial", event(one, QualityHigh, QualityHigh, ""))
	advance(probeTime)
	remb(9_000_000)
	adapt()
	if got := state(); got != allocNormal {
		t.Fatalf("after 6 s of a clean trial the allocator is %s, want normal", got)
	}
	script.same("the trial passed")
	if n := s.Metrics().Downgrades[string(SubReasonBandwidth)]; n != 3 {
		t.Errorf("isshoni_sfu_downgrades_total{bandwidth} = %d, want 3: the cap, the focus change, the failed trial", n)
	}
}
