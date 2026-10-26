package sfu

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// The unit tests of M2's server counterparts in the SFU (M2 01 §10.1, §10.3; 01 §14's sfu row): the native uplink
// estimator's rows on fixed stats sequences, the raw loss and the queueing delay a Layer measures at fixed instants,
// the spacing of congestion hints, which client kinds get which policy, and the sharer's pause. The tests with real
// PeerConnections are congestion_test.go's.

// joinAs joins a Conn of a client kind; the participant id is the user id.
func joinAs(t *testing.T, s *SFU, user UserID, conn ConnID, role Role, client ClientKind) (*Conn, *recSignaler) {
	t.Helper()
	sig := &recSignaler{}
	c, err := s.Join(JoinParams{
		Room: "lounge", Participant: ParticipantID(user), User: user, Conn: conn, Role: role, Client: client,
		Decode: DecodeCaps{H264: []ProfileKey{ProfileHigh, ProfileConstrainedBaseline}}, Signaler: sig,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, sig
}

// layerStat is one video layer's uplink stats with what the native estimator reads, at a steady rate: what arrived in
// the last second is the 2 s average.
func layerStat(rid string, bps int, loss float64, delay time.Duration) LayerUplinkStats {
	return LayerUplinkStats{RID: rid, Bitrate: bps, RawLossPct: loss, QueueDelay: delay, arrived: bps}
}

// nativeRig is a native policy on a fake clock that a test ticks a second at a time, each tick a new generation of
// stats as the ticker's seconds are (SFU.uplinkGen).
type nativeRig struct {
	t     *testing.T
	p     *nativeUplink
	sh    *Share
	clock int64
	gen   uint64
}

func newNativeRig(t *testing.T, s *SFU, id ShareID) *nativeRig {
	t.Helper()
	r := &nativeRig{t: t, sh: testShare(t, s, id), clock: int64(time.Hour)}
	r.p = newNativeUplink(s, r.sh, func() int64 { return r.clock })
	r.gen = r.p.gen
	return r
}

// next moves the clock on a second and runs the policy on st as the ticker's next second.
func (r *nativeRig) next(st ShareUplinkStats) UplinkDecision {
	r.clock += int64(time.Second)
	r.gen++
	st.gen = r.gen
	return r.p.Tick(st)
}

// tick moves the clock on a second and runs the policy on layers.
func (r *nativeRig) tick(layers ...LayerUplinkStats) UplinkDecision {
	return r.next(ShareUplinkStats{Client: ClientDesktop, Layers: layers})
}

// again runs the policy on layers as a look between the ticker's seconds does (a viewer's request for high, the
// admin's cap): the clock moves on by d, the generation stays.
func (r *nativeRig) again(d time.Duration, layers ...LayerUplinkStats) UplinkDecision {
	r.clock += int64(d)
	return r.p.Tick(ShareUplinkStats{Client: ClientDesktop, Layers: layers, gen: r.gen})
}

// want ticks once and fails unless the decision's cap and reason are as given (and no layer changes).
func (r *nativeRig) want(wantCap int, wantReason string, layers ...LayerUplinkStats) {
	r.t.Helper()
	d := r.tick(layers...)
	if d.MaxBitrate != wantCap || d.Reason != wantReason || d.Active != nil {
		r.t.Fatalf("tick %+v = %+v, want cap %d, reason %q", layers, d, wantCap, wantReason)
	}
}

// TestNativeUplinkRows drives nativeUplink through each row of 01 §10.1 with fixed stats sequences.
func TestNativeUplinkRows(t *testing.T) {
	s, _ := newTicklessSFU(t)
	const unknown = time.Duration(-1)
	f := func(bps int, loss float64, delay time.Duration) LayerUplinkStats {
		return layerStat("f", bps, loss, delay)
	}
	q := func(bps int, loss float64, delay time.Duration) LayerUplinkStats {
		return layerStat("q", bps, loss, delay)
	}
	cong := HintReasonCongestion

	t.Run("too few packets", func(t *testing.T) {
		r := newNativeRig(t, s, "s_rate")
		for range 3 {
			r.want(0, "", f(249_999, 50, 100*time.Millisecond))
		}
		// The low ticks had no verdict: one tick of 3 % loss after them is the first of a streak, not the fourth.
		r.want(0, "", f(4_000_000, 3, unknown))
		r.want(3_200_000, cong, f(4_000_000, 3, unknown))
	})

	t.Run("sharer paused", func(t *testing.T) {
		r := newNativeRig(t, s, "s_paused")
		r.sh.sharerPaused.Store(true)
		for range 3 {
			r.want(0, "", f(4_000_000, 50, 100*time.Millisecond))
		}
		r.sh.sharerPaused.Store(false)
		// Its end starts a resume grace like a layer's (setSharerPaused stores the time): 3 s without a verdict.
		r.sh.sharerResumedAt.Store(r.clock)
		r.want(0, "", f(4_000_000, 0, 50*time.Millisecond))
		r.want(0, "", f(4_000_000, 0, 50*time.Millisecond))
		// 3 s after the resume: the paused ticks and the grace counted nothing, so this is the first of a streak.
		r.want(0, "", f(4_000_000, 0, 50*time.Millisecond))
		r.want(3_400_000, cong, f(4_000_000, 0, 50*time.Millisecond))
	})

	t.Run("loss", func(t *testing.T) {
		r := newNativeRig(t, s, "s_loss")
		// The rate is what arrived, so the row's value is 0.8 × the rate, whatever the loss (TestNativeUplinkLossOnce).
		r.want(3_200_000, cong, f(4_000_000, 10, unknown)) // ≥ 10 % on one tick: 0.8 × 4 Mbit/s
		r.want(3_200_000, cong, f(4_000_000, 10, unknown))
		r.want(3_200_000, cong, f(5_000_000, 10, unknown)) // 4 Mbit/s would raise it: a backoff never does
		r.want(2_400_000, cong, f(3_000_000, 10, unknown))
		r.want(nativeMinCap, cong, f(600_000, 50, unknown)) // 480 kbit/s: never below the floor

		r = newNativeRig(t, s, "s_loss3")
		r.want(0, "", f(4_000_000, 3, unknown)) // 3 % on one tick: nothing
		r.want(0, "", f(4_000_000, 2.9, unknown))
		r.want(0, "", f(4_000_000, 3, unknown))
		r.want(3_200_000, cong, f(4_000_000, 3, unknown)) // on two consecutive ticks: 0.8 × 4 Mbit/s

		// Loss is weighted by bitrate: 10 % on 3 Mbit/s and none on 300 kbit/s is 9.09 %, below the one-tick row; the
		// rate is both layers'.
		r = newNativeRig(t, s, "s_weighted")
		r.want(0, "", f(3_000_000, 10, unknown), q(300_000, 0, unknown))
		r.want(2_640_000, cong, f(3_000_000, 10, unknown), q(300_000, 0, unknown)) // 0.8 × 3.3 Mbit/s
	})

	t.Run("delay", func(t *testing.T) {
		r := newNativeRig(t, s, "s_delay")
		r.want(0, "", f(4_000_000, 0, 40*time.Millisecond)) // ≥ 40 ms on one tick: nothing
		r.want(3_400_000, cong, f(4_000_000, 0, 40*time.Millisecond))
		r.want(3_400_000, cong, f(5_000_000, 0, 50*time.Millisecond)) // 4.25 Mbit/s: never above the estimate
		r.want(2_550_000, cong, f(3_000_000, 0, 45*time.Millisecond))

		r = newNativeRig(t, s, "s_delay_low")
		r.want(0, "", f(4_000_000, 0, 39*time.Millisecond))
		r.want(0, "", f(4_000_000, 0, 39*time.Millisecond))
		r.want(0, "", f(4_000_000, 0, 40*time.Millisecond))
		r.want(0, "", f(4_000_000, 0, unknown)) // unknown breaks the streak and is never high
		r.want(0, "", f(4_000_000, 0, 40*time.Millisecond))

		// The loss row comes first.
		r = newNativeRig(t, s, "s_delay_loss")
		r.want(0, "", f(4_000_000, 3, 40*time.Millisecond))
		r.want(3_200_000, cong, f(4_000_000, 3, 40*time.Millisecond)) // 0.8 × the rate, not the delay row's 0.85

		// The delay is the full layer's, even unknown, and the preview layer's only without a full layer.
		r = newNativeRig(t, s, "s_delay_q")
		for range 3 {
			r.want(0, "", f(4_000_000, 0, unknown), q(300_000, 0, 50*time.Millisecond))
		}
		r = newNativeRig(t, s, "s_delay_qonly")
		r.want(0, "", q(300_000, 0, 50*time.Millisecond))
		r.want(nativeMinCap, cong, q(300_000, 0, 50*time.Millisecond)) // 255 kbit/s: the floor
	})

	t.Run("growth", func(t *testing.T) {
		r := newNativeRig(t, s, "s_growth")
		est := 3_200_000
		r.want(est, cong, f(4_000_000, 10, unknown))
		// Held for 10 s, however good the ticks.
		for range 9 {
			r.want(est, cong, f(est, 0, 5*time.Millisecond))
		}
		grow := func() int { return int(math.Round(float64(est) * nativeGrowth)) }
		low := 5 * time.Millisecond
		// growAt ticks with a rate of the estimate, expecting a growth step.
		growAt := func(delay time.Duration) {
			t.Helper()
			rate := est
			est = grow()
			r.want(est, cong, f(rate, 0, delay))
		}
		growAt(low)                       // 10 s after the backoff
		r.want(est, cong, f(est, 0, low)) // 1 s after the last growth
		growAt(low)
		// A queue of 20 ms blocks it (and isn't a backoff); so does one good tick after it.
		r.want(est, cong, f(est, 0, low))
		r.want(est, cong, f(est, 0, 20*time.Millisecond))
		r.want(est, cong, f(est, 0, low))
		growAt(low)
		// An unknown delay counts as low.
		r.want(est, cong, f(est, 0, 20*time.Millisecond))
		r.want(est, cong, f(est, 0, unknown))
		growAt(unknown)
		// Loss of 1 % or more blocks it, for that tick and the next.
		r.want(est, cong, f(est, 1, unknown))
		r.want(est, cong, f(est, 1, unknown))
		r.want(est, cong, f(est, 0, unknown))
		growAt(unknown)
		// A rate below 75 % of the estimate says nothing about more (README WF5; §10.1 had 85 %), and 75 % is enough.
		r.want(est, cong, f(est, 0, unknown))
		r.want(est, cong, f(est*74/100, 0, unknown))
		threeQuarters := int(math.Ceil(0.75 * float64(est)))
		est = grow()
		r.want(est, cong, f(threeQuarters, 0, unknown))
		// Up to the preset's ceilings × 1.1 (8.3 Mbit/s × 1.1), where the estimate is cleared.
		ceiling := nativeCeilingSlack * float64(fullMaxBitrate+previewMaxBitrate)
		for est > 0 {
			r.want(est, cong, f(est, 0, unknown))
			rate := est
			wantReason := cong
			if est = grow(); float64(est) >= ceiling {
				est, wantReason = 0, ""
			}
			r.want(est, wantReason, f(rate, 0, unknown))
		}
		// A cleared estimate stays clear on good ticks, and the next congestion sets it again.
		r.want(0, "", f(9_000_000, 0, unknown))
		r.want(4_000_000, cong, f(5_000_000, 20, unknown))
	})

	t.Run("looks between ticks", func(t *testing.T) {
		// The policy is asked between the ticker's seconds too (askedForHigh, SetLimits). Such a look runs no row:
		// one second of 3 % loss or of a 50 ms queue stays one tick, however many looks see it.
		for _, tc := range []struct {
			name    string
			stats   LayerUplinkStats
			wantCap int
		}{
			{"loss", f(4_000_000, 3, unknown), 3_200_000},
			{"delay", f(4_000_000, 0, 50*time.Millisecond), 3_400_000},
		} {
			r := newNativeRig(t, s, ShareID("s_between_"+tc.name))
			r.want(0, "", tc.stats)
			for range 3 {
				if d := r.again(100*time.Millisecond, tc.stats); d.MaxBitrate != 0 || d.Reason != "" {
					t.Fatalf("%s: a look between ticks = %+v, want no cap", tc.name, d)
				}
			}
			r.want(tc.wantCap, cong, tc.stats) // the second tick of the streak
			// The looks between ticks apply the estimate as it is, and change it no further.
			for range 3 {
				if d := r.again(100*time.Millisecond, f(4_000_000, 50, 100*time.Millisecond)); d.MaxBitrate != tc.wantCap ||
					d.Reason != cong {
					t.Fatalf("%s: a look between ticks after the backoff = %+v, want the cap %d", tc.name, d, tc.wantCap)
				}
			}
		}
	})

	t.Run("admin cap", func(t *testing.T) {
		r := newNativeRig(t, s, "s_admin")
		s.SetLimits(Limits{MaxShareKbps: 2000})
		t.Cleanup(func() { s.SetLimits(Limits{}) })
		r.want(2_000_000, "", f(4_000_000, 10, unknown)) // the estimate (3.2 Mbit/s) is above the admin's cap
		s.SetLimits(Limits{MaxShareKbps: 5000})
		r.want(3_200_000, cong, f(4_000_000, 0, unknown)) // now it binds
		// The ceilings are under the admin's cap: (5 + 0.3) Mbit/s × 1.1 = 5.83 Mbit/s clears the estimate.
		est := 3_200_000
		for range 8 { // held until 10 s after the backoff
			r.want(est, cong, f(est, 0, unknown))
		}
		for est < 5_000_000 {
			est = int(math.Round(float64(est) * nativeGrowth))
			wantCap, wantReason := est, cong
			if est >= 5_000_000 {
				wantCap, wantReason = 5_000_000, "" // above the admin's cap, which binds again
			}
			r.want(wantCap, wantReason, f(est, 0, unknown))
			r.want(wantCap, wantReason, f(est, 0, unknown))
		}
		// The estimate is 5.62 Mbit/s now, and its next step (6.46 Mbit/s) passes 5.83 Mbit/s: none.
		for range 4 {
			r.want(5_000_000, "", f(5_000_000, 0, unknown))
		}
		if r.p.estimate != 0 {
			t.Errorf("the estimate is %d, want none above the ceilings × 1.1", r.p.estimate)
		}
	})
}

// TestNativeUplinkLossOnce pins the loss row's value (README WF5): max(nativeMinCap, 0.8 × arrived), whatever the
// loss, with arrived what arrived of the active layers in the ticker's last second. The lost packets are out of it
// already, so 01 §10.1's row as first written, 0.8 × rate × (1 − loss/100), counted the loss a second time; and the
// 2 s average (Bitrate) still carries what the sharer sent before the loss began.
func TestNativeUplinkLossOnce(t *testing.T) {
	s, _ := newTicklessSFU(t)
	const unknown = time.Duration(-1)

	// The policy: 5 Mbit/s that arrived backs off to 4 Mbit/s at any loss that backs off (counted twice: 3.2 Mbit/s
	// at 20 %, 500 kbit/s at 90 %), and what arrived of two layers is the sum of both, at their weighted loss.
	for _, loss := range []float64{3, 10, 20, 50, 90} {
		r := newNativeRig(t, s, ShareID(fmt.Sprintf("s_once_%g", loss)))
		r.tick(layerStat("f", 5_000_000, loss, unknown)) // 3 %: the first tick of its streak
		r.want(4_000_000, HintReasonCongestion, layerStat("f", 5_000_000, loss, unknown))
	}
	r := newNativeRig(t, s, "s_once_two")
	two := []LayerUplinkStats{layerStat("f", 4_000_000, 50, unknown), layerStat("q", 300_000, 0, unknown)}
	r.want(3_440_000, HintReasonCongestion, two...) // 46.5 % of 4.3 Mbit/s; counted twice: 1.84 Mbit/s
	// The last second's: 3 Mbit/s that arrived in it, while the 2 s average is still 5 Mbit/s, is 2.4 Mbit/s.
	r = newNativeRig(t, s, "s_once_last")
	last := layerStat("f", 5_000_000, 20, unknown)
	last.arrived = 3_000_000
	r.want(2_400_000, HintReasonCongestion, last)

	// The Layer: a second of 400 packets of 1000 bytes (3.2 Mbit/s) all arrive, then of the next second's 400 every
	// fourth is lost. Its Bitrate's 2 s average is then about 2.89 Mbit/s, what arrived in the last second 2.4 Mbit/s,
	// its raw loss 12.5 % over the two seconds, and the sharer is told 0.8 × 2.4 Mbit/s: not that × 0.875 (the loss
	// counted again), nor 0.8 × the average (2.31 Mbit/s).
	s, _ = newTicklessSFU(t)
	ctx := testCtx(t)
	pub, pubSig := joinAs(t, s, "alice", "c-pub", RolePublisher, ClientDesktop)
	startShare(t, pub, "s_once_layer")
	sh := s.lookupShare("s_once_layer")
	l, _ := testLayer(t, sh, SlotF, 0xf0)
	payload := make([]byte, 1000)
	payload[0] = 0x41 // a non-IDR slice
	l.tick(t0)        // the baseline
	// second sends the packets of second n (from 0), skipping those lost, and has the Conn look as the ticker does.
	second := func(n int, lost func(i int) bool) LayerUplinkStats {
		t.Helper()
		for i := range 400 {
			if lost(i) {
				continue
			}
			k := 400*n + i
			l.handleRTP(&rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(100 + k), Timestamp: 3000 * uint32(k/10),
				PayloadType: 96}, Payload: payload}, ProfileHigh, at(float64(k)*2.5))
		}
		l.tick(t0 + int64(n+1)*int64(time.Second))
		s.uplinkGen.Add(1) // the ticker's next second
		if err := pub.CheckUplink(ctx); err != nil {
			t.Fatal(err)
		}
		return sh.uplinkStats(ClientDesktop).Layers[0]
	}
	if st := second(0, func(int) bool { return false }); st.Bitrate != 3_200_000 || st.arrived != 3_200_000 ||
		st.RawLossPct != 0 {
		t.Fatalf("the layer's stats after a second without loss = %+v, want 3.2 Mbit/s and no loss", st)
	}
	if got := pubSig.hints(); len(got) != 0 {
		t.Fatalf("hints after a second without loss = %+v, want none", got)
	}
	// Lost: every fourth; the last packet arrives, so the second's sequence space is all 400.
	st := second(1, func(i int) bool { return i%4 == 1 })
	wantAvg := int(math.Round(3_200_000 - (1-math.Exp(-float64(time.Second)/float64(rateWindow)))*800_000))
	if st.Bitrate != wantAvg || st.arrived != 2_400_000 || st.RawLossPct != 12.5 {
		t.Fatalf("the layer's stats = %+v, want a 2 s average of %d bit/s, 2.4 Mbit/s that arrived in the last "+
			"second and 12.5 %% raw loss", st, wantAvg)
	}
	if got := pubSig.hints(); len(got) != 1 || got[0].Reason != HintReasonCongestion || got[0].MaxBitrate != 1_920_000 {
		t.Fatalf("hints = %+v, want one congestion hint of 1.92 Mbit/s (0.8 × what arrived in the last second)", got)
	}
}

// TestNativeUplinkLayerPausing: the native policy keeps M1's layer pausing (it embeds m1Uplink). A layer it paused
// is no active layer, so its stale 2 s average doesn't count; a layer it resumes leaves the estimate as it is for 3 s.
func TestNativeUplinkLayerPausing(t *testing.T) {
	s, _ := newTicklessSFU(t)
	s.pauseUnwatched = true // before any Join: no actor reads it yet
	r := newNativeRig(t, s, "s_np")
	both := func(fLoss, qLoss float64) []LayerUplinkStats {
		return []LayerUplinkStats{layerStat("f", 4_000_000, fLoss, -1), layerStat("q", 300_000, qLoss, -1)}
	}
	r.clock += int64(pauseAfter) - int64(time.Second)
	if d := r.tick(both(0, 0)...); !reflect.DeepEqual(d.Active, map[string]bool{"f": false}) || d.MaxBitrate != 0 {
		t.Fatalf("10 s without a request for high: %+v, want f paused", d)
	}
	// Only q counts now: 50 % loss on its 300 kbit/s is the floor; with f's 4 Mbit/s it would be 3.44 Mbit/s.
	r.want(nativeMinCap, HintReasonCongestion, both(50, 50)...)
	// A request for high resumes f: no verdict for 3 s, then the estimate follows the stats again.
	st := ShareUplinkStats{Client: ClientDesktop, HighRequests: 1, Layers: both(0, 0)}
	if d := r.next(st); !reflect.DeepEqual(d.Active, map[string]bool{"f": true}) || d.MaxBitrate != nativeMinCap {
		t.Fatalf("the resume: %+v", d)
	}
	r.want(nativeMinCap, HintReasonCongestion, both(50, 50)...)
	r.want(nativeMinCap, HintReasonCongestion, both(50, 50)...)
	r.want(nativeMinCap, HintReasonCongestion, both(50, 50)...) // 3 s after the resume: a verdict, the floor still
	if r.p.heldUntil != r.clock+int64(nativeHold) {
		t.Errorf("the backoff 3 s after the resume didn't hold growth")
	}
}

// TestUplinkPolicyKinds: a share gets the native estimator when its publishing Conn is a desktop or mobile client,
// M1's policy otherwise: the web client, and the tools, which signal maps to the web kind (sfuplane's convert test).
func TestUplinkPolicyKinds(t *testing.T) {
	s, _ := newTicklessSFU(t)
	ctx := testCtx(t)
	for i, tc := range []struct {
		client ClientKind
		native bool
	}{{ClientWeb, false}, {ClientDesktop, true}, {ClientMobile, true}} {
		id := ShareID("s_kind" + string(rune('a'+i)))
		c, _ := joinAs(t, s, UserID("u"+string(id)), ConnID("c"+string(id)), RolePublisher, tc.client)
		startShare(t, c, id)
		var policy UplinkPolicy
		if err := c.do(ctx, func(context.Context) error {
			policy = s.lookupShare(id).up.policy
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		_, native := policy.(*nativeUplink)
		_, m1 := policy.(*m1Uplink)
		if native != tc.native || m1 == tc.native {
			t.Errorf("%s: the share's policy is %T", tc.client, policy)
		}
	}
}

// scriptedPolicy decides what the test set last (on the actor, through setDecision).
type scriptedPolicy struct{ d UplinkDecision }

func (p *scriptedPolicy) Tick(ShareUplinkStats) UplinkDecision { return p.d }

// TestCongestionHints: a cap that only the native estimator moved goes to the sharer as a hint with the reason
// congestion when it differs from the cap the sharer was told by 15 % or more (or between none and some), and 2 s or
// more after the share's last hint; one that doesn't qualify waits, and the next look compares again. Admin and pause
// changes go out at once, as in M1. Every hint's encodings are under the admin's cap, its MaxBitrate the total.
func TestCongestionHints(t *testing.T) {
	s, _ := newTicklessSFU(t)
	ctx := testCtx(t)
	pub, pubSig := joinAs(t, s, "alice", "c-pub", RolePublisher, ClientDesktop)
	startShare(t, pub, "s_1")
	sh := s.lookupShare("s_1")
	policy := &scriptedPolicy{}
	setDecision := func(d UplinkDecision) {
		t.Helper()
		if err := pub.do(ctx, func(context.Context) error {
			policy.d = d
			sh.up.policy = policy
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	look := func() {
		t.Helper()
		if err := pub.CheckUplink(ctx); err != nil {
			t.Fatal(err)
		}
	}
	decide := func(capBps int, reason string, active map[string]bool) {
		t.Helper()
		setDecision(UplinkDecision{MaxBitrate: capBps, Reason: reason, Active: active})
		look()
	}
	wantHints := func(n int, last QualityHintEvent) {
		t.Helper()
		got := pubSig.hints()
		if len(got) != n || (n > 0 && !reflect.DeepEqual(got[n-1], last)) {
			t.Fatalf("hints = %+v, want %d with the last %+v", got, n, last)
		}
	}
	hint := func(reason string, capBps, adminBps int, paused uint32) QualityHintEvent {
		return QualityHintEvent{
			Share: "s_1", Reason: reason, MaxBitrate: capBps, Encodings: presetEncodings(PresetAuto, adminBps, paused),
		}
	}
	cong := HintReasonCongestion
	fOff := map[string]bool{"f": false}

	decide(3_000_000, cong, nil) // from none to some, the share's first hint: at once
	wantHints(1, hint(cong, 3_000_000, 0, 0))
	s.AdvanceUplinkClock(time.Second)
	decide(2_000_000, cong, nil) // a third lower, but 1 s after the last hint: waits
	wantHints(1, hint(cong, 3_000_000, 0, 0))
	s.AdvanceUplinkClock(time.Second)
	look() // 2 s: due
	wantHints(2, hint(cong, 2_000_000, 0, 0))
	s.AdvanceUplinkClock(2 * time.Second)
	decide(1_800_000, cong, nil) // 10 %: waits however long
	s.AdvanceUplinkClock(10 * time.Second)
	look()
	wantHints(2, hint(cong, 2_000_000, 0, 0))
	decide(1_700_000, cong, nil) // exactly 15 %: due
	wantHints(3, hint(cong, 1_700_000, 0, 0))
	if st, _, _ := pub.Uplink(ctx, "s_1"); st.MaxBitrate != 1_700_000 || st.Reason != cong {
		t.Errorf("Uplink = %+v", st)
	}

	// The admin's cap changes 0.5 s later, below the estimate: at once, the reason admin.
	s.AdvanceUplinkClock(500 * time.Millisecond)
	setDecision(UplinkDecision{MaxBitrate: 1_000_000})
	s.SetLimits(Limits{MaxShareKbps: 1000})
	settle(pub)
	wantHints(4, hint(HintReasonAdmin, 1_000_000, 1_000_000, 0))
	// A pause change right after: at once, the reason viewers.
	decide(1_000_000, "", fOff)
	wantHints(5, hint(HintReasonViewers, 1_000_000, 1_000_000, 1<<SlotF))
	// The estimate binds again 2 s later: the encodings stay under the admin's cap.
	s.AdvanceUplinkClock(2 * time.Second)
	decide(800_000, cong, fOff)
	wantHints(6, hint(cong, 800_000, 1_000_000, 1<<SlotF))
	// The estimate is cleared 2 s later: the admin's cap is the total again, and the estimator moved it.
	s.AdvanceUplinkClock(2 * time.Second)
	decide(1_000_000, "", fOff)
	wantHints(7, hint(cong, 1_000_000, 1_000_000, 1<<SlotF))
	// The admin's cap changes while an estimate binds: at once, the total stays the estimate.
	s.AdvanceUplinkClock(2 * time.Second)
	decide(700_000, cong, fOff)
	wantHints(8, hint(cong, 700_000, 1_000_000, 1<<SlotF))
	setDecision(UplinkDecision{MaxBitrate: 700_000, Reason: cong, Active: fOff})
	s.SetLimits(Limits{MaxShareKbps: 3000})
	settle(pub)
	wantHints(9, hint(HintReasonAdmin, 700_000, 3_000_000, 1<<SlotF))
	// A resume of the WebSocket: the last hint again.
	pub.Resync()
	if got := pubSig.waitHints(t, 10); !reflect.DeepEqual(got[9], got[8]) {
		t.Errorf("Resync sent %+v, want the last hint %+v", got[9], got[8])
	}
	s.SetLimits(Limits{})
	settle(pub)
}

// TestRawLoss: a Layer counts each gap at the new highest sequence number, before any repair; padding counts as
// received. A gap that a retransmission fills is no loss (LossPct) but is raw loss (RawLossPct); both are over the
// last two ticks, and reach the uplink stats.
func TestRawLoss(t *testing.T) {
	s, _ := newTicklessSFU(t)
	sh := testShare(t, s, "s_raw")
	l, _ := testLayer(t, sh, SlotF, 0xf0)
	now := t0
	send := func(seq uint16, payload []byte) {
		pkt := &rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: uint32(seq) * 3000, PayloadType: 96},
			Payload: payload}
		l.handleRTP(pkt, ProfileHigh, now)
		now += int64(time.Millisecond)
	}
	media := []byte{0x41, 1, 2, 3}
	l.tick(t0) // the baseline
	for seq := range uint16(40) {
		switch seq {
		case 10, 11, 30:
			// lost for now
		case 20:
			send(seq, nil) // padding: no payload, but received
		default:
			send(seq, media)
		}
	}
	send(10, media) // the retransmissions of 10 and 11; 30 stays lost
	send(11, media)
	l.tick(t0 + int64(time.Second))
	raw := math.Float64frombits(l.stats.rawLossPct.Load())
	loss := math.Float64frombits(l.stats.lossPct.Load())
	if raw != 100*3.0/40 || loss != 100*1.0/40 {
		t.Errorf("RawLossPct %.3f, LossPct %.3f; want %.3f and %.3f", raw, loss, 100*3.0/40, 100*1.0/40)
	}
	st := sh.uplinkStats(ClientDesktop)
	if len(st.Layers) != 1 || st.Layers[0].RawLossPct != raw || st.Layers[0].LossPct != loss {
		t.Errorf("uplink stats = %+v", st)
	}
	// The next second has no gap; the window still holds the first.
	for seq := uint16(40); seq < 80; seq++ {
		send(seq, media)
	}
	l.tick(t0 + 2*int64(time.Second))
	if raw := math.Float64frombits(l.stats.rawLossPct.Load()); raw != 100*3.0/80 {
		t.Errorf("RawLossPct over two seconds = %.3f, want %.3f", raw, 100*3.0/80)
	}
	for seq := uint16(80); seq < 120; seq++ {
		send(seq, media)
	}
	l.tick(t0 + 3*int64(time.Second))
	if raw := math.Float64frombits(l.stats.rawLossPct.Load()); raw != 0 {
		t.Errorf("RawLossPct once the gaps are 2 s old = %.3f", raw)
	}
}

// at returns the instant ms milliseconds after t0, the tests' arrival clock.
func at(ms float64) int64 { return t0 + int64(ms*float64(time.Millisecond)) }

// frameMS is a 30 fps frame interval: 3000 ticks of 90 kHz.
const frameMS = 1000.0 / 30

// trendFeed feeds a delayTrend frames of a 30 fps stream whose sender clock runs slow by ppm (its timestamps fall
// behind real time), and ticks it once a second on the arrival clock, from t0 + 1 s.
type trendFeed struct {
	d     delayTrend
	ts0   uint32
	k     int // the next frame
	ppm   float64
	next  int64 // the next tick
	ticks []time.Duration
}

func newTrendFeed(ts0 uint32, ppm float64) *trendFeed {
	return &trendFeed{ts0: ts0, ppm: ppm, next: at(1000)}
}

// arrival is when frame k arrives without a queue.
func (f *trendFeed) arrival(k int) int64 {
	return at(float64(k) * frameMS * (1 + f.ppm*1e-6))
}

// frames sends n frames, every every-th frame of the stream (1: all), each arriving extra late, with the ticks that
// fall between them.
func (f *trendFeed) frames(n, every int, extra time.Duration) {
	for range n {
		t := f.arrival(f.k) + int64(extra)
		f.until(t)
		f.d.sample(f.ts0+uint32(3000*f.k), t)
		f.k += every
	}
}

// until runs the ticks up to t, and returns the QueueDelay of the last one.
func (f *trendFeed) until(t int64) time.Duration {
	for f.next <= t {
		f.ticks = append(f.ticks, f.d.tick(f.next))
		f.next += int64(time.Second)
	}
	if len(f.ticks) == 0 {
		return -2
	}
	return f.ticks[len(f.ticks)-1]
}

// near reports whether d is within 1 µs of want (the ns conversions round).
func near(d, want time.Duration) bool {
	return d >= want-time.Microsecond && d <= want+time.Microsecond
}

// TestDelayTrend is the queueing delay of 01 §10.1 at fixed instants: unknown in the first 2 s and with fewer than 3
// samples in the window; the minimum of the last 500 ms above the baseline; a baseline that follows a sender drift of
// ±300 ppm (QueueDelay stays below 5 ms), absorbs a standing queue at no more than 0.5 ms per s, and drops at once when
// the queue drains; the 32-bit timestamp unwrapped.
func TestDelayTrend(t *testing.T) {
	ms := time.Millisecond
	t.Run("warm-up and few samples", func(t *testing.T) {
		f := newTrendFeed(1000, 0)
		f.frames(30, 1, 0) // frames 0 to 29, to 967 ms
		if d := f.until(at(1000)); d != -1 {
			t.Errorf("QueueDelay 1 s after the first sample = %v, want -1", d)
		}
		f.frames(30, 1, 0)
		if d := f.until(at(2000)); !near(d, 0) {
			t.Errorf("QueueDelay 2 s after the first sample = %v, want 0", d)
		}
		// A static screen: a frame every 300 ms (from 2 s) leaves at most 2 samples in the 500 ms window.
		f.frames(10, 9, 0)
		for i, d := range f.ticks[2:] {
			if d != -1 {
				t.Errorf("tick %d s with a frame every 300 ms: QueueDelay %v, want -1", i+3, d)
			}
		}
		f.k = 6 * 30 // 30 fps again from 6 s
		f.frames(30, 1, 0)
		if d := f.until(at(7000)); !near(d, 0) {
			t.Errorf("QueueDelay back at 30 fps = %v", d)
		}
	})

	t.Run("500 ms window", func(t *testing.T) {
		f := newTrendFeed(1000, 0)
		f.frames(90, 1, 0)
		f.until(at(3000))
		f.frames(30, 1, 30*ms) // from 3 s, a second with 30 ms of queue
		// The baseline rose 0.5 ms in the second without a sample below it.
		if d := f.until(at(4000)); !near(d, 29500*time.Microsecond) {
			t.Errorf("QueueDelay in a 30 ms queue = %v, want 29.5 ms", d)
		}
		f.frames(18, 1, 10*ms) // 600 ms of 10 ms: the 30 ms samples have left the window
		// The baseline rose 0.5 ms per second since 3 s: 0.805 ms at 4.61 s.
		if d := f.d.tick(at(4610)); !near(d, 10*ms-805*time.Microsecond) {
			t.Errorf("QueueDelay 600 ms into a 10 ms queue = %v, want 10 ms less the baseline's 0.805 ms", d)
		}
	})

	for _, ppm := range []float64{300, -300} {
		t.Run(fmt.Sprintf("drift %+.0f ppm", ppm), func(t *testing.T) {
			f := newTrendFeed(1000, ppm)
			f.frames(30*120, 1, 0) // two minutes
			worst := time.Duration(0)
			for _, d := range f.ticks[1:] {
				worst = max(worst, d)
			}
			if worst >= 5*ms {
				t.Errorf("%+.0f ppm: QueueDelay reached %v, want below 5 ms", ppm, worst)
			}
			t.Logf("%+.0f ppm over 120 s: QueueDelay at most %v", ppm, worst)
		})
	}

	t.Run("standing queue", func(t *testing.T) {
		f := newTrendFeed(1000, 0)
		f.frames(90, 1, 0)
		f.until(at(3000))
		from := len(f.ticks) // the tick at 4 s is the first in the queue
		f.frames(30*100, 1, 40*ms)
		f.until(at(103_000)) // 100 s of a 40 ms queue
		for i, d := range f.ticks[from:] {
			// The baseline rises at most 0.5 ms per second: after n s the queue shows at least 40 − 0.5 n ms.
			floor := 40*ms - time.Duration(i+1)*500*time.Microsecond - time.Microsecond
			if d < max(floor, 0) {
				t.Fatalf("%d s into the queue QueueDelay = %v, want at least %v", i+1, d, floor)
			}
		}
		if d := f.ticks[from+19]; !near(d, 30*ms) {
			t.Errorf("20 s into the queue QueueDelay = %v, want 30 ms", d)
		}
		if d := f.ticks[len(f.ticks)-1]; d > time.Microsecond {
			t.Errorf("after 100 s the queue is still %v above the baseline, want it absorbed", d)
		}
		// The queue drains: the baseline drops at once, so a new 40 ms queue shows in full (less one tick's rise).
		f.frames(30, 1, 0)
		if d := f.until(at(104_000)); !near(d, 0) {
			t.Errorf("QueueDelay once the queue drained = %v", d)
		}
		f.frames(30, 1, 40*ms)
		if d := f.until(at(105_000)); !near(d, 39500*time.Microsecond) {
			t.Errorf("QueueDelay of a new 40 ms queue = %v, want 39.5 ms", d)
		}
	})

	t.Run("timestamp wrap", func(t *testing.T) {
		f := newTrendFeed(math.MaxUint32-3000*45, 0) // wraps 1.5 s in
		f.frames(30*5, 1, 0)
		f.until(at(5000))
		f.frames(30, 1, 20*ms)
		d := f.until(at(6000))
		for i, d := range f.ticks[1:5] {
			if !near(d, 0) {
				t.Errorf("tick %d s across the wrap: QueueDelay %v", i+2, d)
			}
		}
		if !near(d, 19500*time.Microsecond) {
			t.Errorf("a 20 ms queue after the wrap = %v, want 19.5 ms", d)
		}
	})
}

// FuzzDelayTrend feeds a delayTrend arbitrary samples (any timestamp step, wraps and reordering included, arrivals a
// little before or after the ticker's clock) and ticks: it never panics, its window stays within its rings, and
// QueueDelay is −1 or not negative.
func FuzzDelayTrend(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0x0b, 0xb8, 1, 8, 0, 0, 0, 0x0b, 0xb8, 1, 8, 1, 0, 0, 0, 0, 0x40, 0})
	f.Add([]byte{0, 0xff, 0xff, 0xff, 0xff, 0, 1, 0, 0x80, 0, 0, 0, 0, 1, 1, 0, 0, 0, 0, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		var d delayTrend
		now := t0
		var ts uint32
		for ; len(data) >= 7; data = data[7:] {
			op := data[0]
			step := uint32(data[1])<<24 | uint32(data[2])<<16 | uint32(data[3])<<8 | uint32(data[4])
			now += int64(uint16(data[5])<<8|uint16(data[6])) * int64(time.Millisecond) / 8 // up to 8 s on
			switch op % 3 {
			case 0, 1:
				ts += step
				skew := int64(op>>2) * int64(time.Millisecond) // the RTP loop's clock a little ahead
				d.sample(ts, now+skew)
			default:
				if q := d.tick(now); q < -1 {
					t.Fatalf("QueueDelay %v", q)
				}
			}
			if d.minN < 0 || d.minN > nativeDelaySlots || d.tN < 0 || d.tN > nativeDelaySlots {
				t.Fatalf("window rings hold %d and %d entries", d.minN, d.tN)
			}
		}
	})
}

// TestLayerQueueDelay: the Layer's RTP loop samples only a frame's first packet: neither the rest of the frame, nor a
// retransmission (an older sequence number), padding or a reordered older timestamp. The ticker's QueueDelay reaches
// the uplink stats, and its delay rows: a desktop share whose queue grows gets a congestion hint.
func TestLayerQueueDelay(t *testing.T) {
	s, _ := newTicklessSFU(t)
	ctx := testCtx(t)
	pub, pubSig := joinAs(t, s, "alice", "c-pub", RolePublisher, ClientDesktop)
	startShare(t, pub, "s_qd")
	sh := s.lookupShare("s_qd")
	l, _ := testLayer(t, sh, SlotF, 0xf0)
	var seq uint16 = 100
	tsOf := func(i int) uint32 { return 7000 + 3000*uint32(i) } // frame i's RTP timestamp
	payload := make([]byte, 1100)
	payload[0] = 0x41
	send := func(seq uint16, ts uint32, t int64, payload []byte) {
		l.handleRTP(&rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: ts, PayloadType: 96},
			Payload: payload}, ProfileHigh, t)
	}
	// frame sends frame i of the stream (due at i/30 s) as n packets 0.1 ms apart, the first extra late.
	frame := func(i, n int, extra time.Duration) {
		first := at(float64(i)*frameMS) + int64(extra)
		for k := range n {
			send(seq, tsOf(i), first+int64(k)*int64(100*time.Microsecond), payload)
			seq++
		}
	}
	delay := func(tickAt int64) time.Duration {
		l.tick(tickAt)
		return sh.uplinkStats(ClientDesktop).Layers[0].QueueDelay
	}

	for i := range 90 { // 3 s at 30 fps, ticks at 1 s (still warming up) and 2 s
		if i == 30 || i == 60 {
			if d := delay(at(float64(i) * frameMS)); (i == 30) != (d == -1) {
				t.Fatalf("QueueDelay at %d s = %v", i/30, d)
			}
		}
		frame(i, 3, 0)
	}
	if d := delay(at(3000)); !near(d, 0) {
		t.Fatalf("QueueDelay of a steady stream = %v", d)
	}
	// Nothing for 1.5 s (the window empties), then two frames of ten packets at 4.6 and 4.7 s, a packet between them
	// lost; then the junk: the lost packet's retransmission (not the newest) and padding, both with a newer
	// timestamp, and a newest packet with an older one. Two samples in the window: unknown.
	frame(138, 10, 0)
	lost := seq
	seq++
	frame(141, 10, 0)
	send(lost, tsOf(171), at(4750), payload)
	send(seq, tsOf(171), at(4751), nil)
	send(seq+1, tsOf(139), at(4752), payload)
	seq += 2
	if d := delay(at(5000)); d != -1 {
		t.Fatalf("QueueDelay with two frames in the window = %v, want -1: something else was sampled", d)
	}
	for _, i := range []int{168, 171, 174} {
		frame(i, 2, 0)
	}
	if d := delay(at(6000)); !near(d, 0) {
		t.Fatalf("QueueDelay with three frames in the window = %v, want 0", d)
	}

	// A 60 ms queue for two ticks: the delay row backs off to 0.85 × the rate, and the sharer hears of it. Each tick
	// is a new second for the uplink policy (as SFU.everySecond has it) and its look, as the ticker's.
	look := func() {
		t.Helper()
		s.uplinkGen.Add(1)
		if err := pub.CheckUplink(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for i := 180; i < 210; i++ {
		frame(i, 2, 60*time.Millisecond)
	}
	if d := delay(at(7000)); !near(d, 59500*time.Microsecond) {
		t.Fatalf("QueueDelay in a 60 ms queue = %v, want 59.5 ms (the baseline rose 0.5 ms)", d)
	}
	look()
	if got := pubSig.hints(); len(got) != 0 {
		t.Fatalf("hints after one tick of a 60 ms queue = %+v, want none", got)
	}
	for i := 210; i < 240; i++ {
		frame(i, 2, 60*time.Millisecond)
	}
	if d := delay(at(8000)); !near(d, 59*time.Millisecond) {
		t.Fatalf("QueueDelay a second later = %v, want 59 ms", d)
	}
	look()
	got := pubSig.hints()
	rate := int(l.stats.bitrate.Load())
	if len(got) != 1 || got[0].Reason != HintReasonCongestion ||
		got[0].MaxBitrate != max(nativeMinCap, int(math.Round(nativeDelayBackoff*float64(rate)))) {
		t.Fatalf("hints = %+v, want one congestion hint of 0.85 × %d", got, rate)
	}
}

// TestNativeUplinkLooksBetweenTicks: the publishing Conn's actor also looks at the uplink between the ticker's
// seconds, on the layer stats of its last one: at once when a viewer's request becomes high (askedForHigh, whenever
// layers may be paused, which is the default), and when the admin's cap may have changed (SetLimits). Such a look
// never counts that second again: one second of 3 % raw loss, or of a 50 ms queue, with the ticker's look and those
// looks in it, sends no hint; the next second with the same stats sends the backoff.
func TestNativeUplinkLooksBetweenTicks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		loss    float64
		delay   time.Duration
		wantCap int
	}{
		{"loss", 3, -1, 3_200_000},
		{"delay", 0, 50 * time.Millisecond, 3_400_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTicklessSFU(t)
			s.pauseUnwatched = true // before any Join: no actor reads it yet
			ctx := testCtx(t)
			pub, pubSig := joinAs(t, s, "alice", "c-pub", RolePublisher, ClientDesktop)
			startShare(t, pub, "s_between")
			sh := s.lookupShare("s_between")
			l, _ := testLayer(t, sh, SlotF, 0xf0)
			// second gives the layer the stats of the ticker's next second, and has the Conn look as the ticker does.
			second := func() {
				t.Helper()
				l.stats.bitrate.Store(4_000_000)
				l.stats.arrived.Store(4_000_000)
				l.stats.rawLossPct.Store(math.Float64bits(tc.loss))
				l.stats.queueDelay.Store(int64(tc.delay))
				s.uplinkGen.Add(1)
				s.AdvanceUplinkClock(time.Second)
				if err := pub.CheckUplink(ctx); err != nil {
					t.Fatal(err)
				}
			}
			second()
			for range 2 {
				s.AdvanceUplinkClock(100 * time.Millisecond)
				sh.askedForHigh()
				settle(pub)
			}
			s.SetLimits(Limits{})
			settle(pub)
			if got := pubSig.hints(); len(got) != 0 {
				t.Fatalf("hints after one second of stats = %+v, want none", got)
			}
			second()
			if got := pubSig.hints(); len(got) != 1 || got[0].Reason != HintReasonCongestion ||
				got[0].MaxBitrate != tc.wantCap {
				t.Fatalf("hints after two seconds = %+v, want one congestion hint of %d", got, tc.wantCap)
			}
		})
	}
}

// TestSharerPause: share.update{paused} (01 §10.3). While the sharer has paused the share, a keyframe request for
// any of its layers writes nothing and spends no throttle; the unpause asks once for the keyframe of each video layer
// that has viewers, and none of the others. The params UpdateShare returns don't change. Like a layer pause and
// resume (02 §11), the downlink allocators weigh the video layers at their maximum while the share is paused and for
// rateWindow after the unpause, while their 2 s averages catch up (allocRate).
func TestSharerPause(t *testing.T) {
	s, _ := newTicklessSFU(t)
	ctx := testCtx(t)
	pub, _ := joinAs(t, s, "alice", "c-pub", RolePublisher, ClientDesktop)
	params := startShare(t, pub, "s_p")
	sh := s.lookupShare("s_p")
	f, fPub := testLayer(t, sh, SlotF, 0xf00)
	q, qPub := testLayer(t, sh, SlotQ, 0xa00)
	viewer, _ := join(t, s, "lounge", "bob", "c-view", RoleViewer)
	dt, _ := testDownTrack(t, viewer, sh, webrtc.RTPCodecTypeVideo)
	dt.interest.Store(1 << SlotF) // the viewer watches f only
	update := func(paused bool) {
		t.Helper()
		p, err := pub.UpdateShare(ctx, "s_p", ShareUpdate{Paused: &paused})
		if err != nil || !reflect.DeepEqual(p.Encodings, params.Encodings) || p.AudioBitrate != params.AudioBitrate {
			t.Fatalf("UpdateShare(paused %v) = %+v, %v; want the params of StartShare %+v", paused, p, err, params)
		}
	}

	// allocRates fails unless the allocators weigh f and q at fRate and qRate.
	allocRates := func(when string, fRate, qRate int64) {
		t.Helper()
		if got, gotQ := sh.allocRate(f), sh.allocRate(q); got != fRate || gotQ != qRate {
			t.Errorf("%s: allocRate f %d, q %d; want %d and %d", when, got, gotQ, fRate, qRate)
		}
	}
	fMax, qMax := sh.maxBitrate(SlotF), sh.maxBitrate(SlotQ)
	f.stats.bitrate.Store(3_000_000)
	q.stats.bitrate.Store(200_000)
	allocRates("before the pause", 3_000_000, 200_000)

	update(true)
	if !sh.sharerPaused.Load() {
		t.Fatal("the share is not paused")
	}
	allocRates("paused, as the layers still measure their last rates", fMax, qMax)
	f.stats.bitrate.Store(0) // within 2 s the paused share measures nothing
	q.stats.bitrate.Store(0)
	allocRates("paused", fMax, qMax)
	now := monoNow()
	if f.requestKeyframe(now) || q.requestKeyframe(now) {
		t.Error("a keyframe request went out for a paused share")
	}
	sh.requestKeyframe(SlotF)
	if len(fPub.plis()) != 0 || len(qPub.plis()) != 0 || f.stats.pliThrottled.Load()+q.stats.pliThrottled.Load() != 0 {
		t.Errorf("while paused: %d and %d PLIs, %d throttled", len(fPub.plis()), len(qPub.plis()),
			f.stats.pliThrottled.Load()+q.stats.pliThrottled.Load())
	}
	update(true) // again: nothing changes
	if sh.sharerResumedAt.Load() != 0 {
		t.Error("a pause noted a resume")
	}

	update(false)
	if sh.sharerPaused.Load() || sh.sharerResumedAt.Load() == 0 {
		t.Fatalf("after the unpause: paused %v, resumed at %d", sh.sharerPaused.Load(), sh.sharerResumedAt.Load())
	}
	if at := sh.sharerResumedAt.Load(); sh.resumedAt[SlotF].Load() != at || sh.resumedAt[SlotQ].Load() != at {
		t.Errorf("the layers' resumes at %d and %d, want the unpause's %d", sh.resumedAt[SlotF].Load(),
			sh.resumedAt[SlotQ].Load(), at)
	}
	f.stats.bitrate.Store(1_000_000) // the 2 s averages catching up
	q.stats.bitrate.Store(100_000)
	allocRates("just after the unpause", fMax, qMax)
	s.AdvanceUplinkClock(rateWindow - time.Millisecond)
	allocRates("just within rateWindow of the unpause", fMax, qMax)
	s.AdvanceUplinkClock(time.Millisecond)
	allocRates("rateWindow after the unpause", 1_000_000, 100_000)
	if p := fPub.plis(); len(p) != 1 || p[0].MediaSSRC != 0xf00 {
		t.Errorf("the watched layer's PLIs after the unpause = %+v, want one", p)
	}
	if n := len(qPub.plis()); n != 0 {
		t.Errorf("the unwatched layer got %d PLIs", n)
	}
	update(false) // again: no second request
	if n := len(fPub.plis()); n != 1 {
		t.Errorf("%d PLIs after a second unpause, want 1", n)
	}
	// Requests work again (the throttle wasn't spent while paused: the unpause's request went out).
	if !q.requestKeyframe(monoNow()) {
		t.Error("no PLI for the preview layer after the unpause")
	}
}
