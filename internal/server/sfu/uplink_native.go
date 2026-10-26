package sfu

import (
	"math"
	"sync"
	"time"
)

// The native uplink estimator (M2 01 §10.1, M1 02 §11's "Later (M2), native publishers"): a share whose publishing
// Conn is a desktop or mobile client gets nativeUplink, which embeds the M1 policy (the admin's cap, layer pausing)
// and adds an estimate of what the sharer's uplink carries. A native publisher has no congestion control of its own
// (Pion GCC stays off, M2 01 D9): it ramps toward its ceilings and applies the estimate the hints carry. The estimate
// comes from two signals of each video layer: the loss before any repair (LayerUplinkStats.RawLossPct) and the
// queueing-delay trend (LayerUplinkStats.QueueDelay, delayTrend). A browser keeps m1Uplink: its own GCC over TWCC
// already does this job.

// The estimator's constants (M2 01 §10.1). They are tuned only with M2 01 §14's integration cases D9, D10 and D19
// passing (internal/server/itest's desktop_congestion_test.go). README WA54 tuned one of them: nativeGrowth is 1.151,
// not §10.1's 1.10. A step of just over 15 % is a hint of its own (nativeHintMinChange) that the sharer's ramp
// follows in one of its steps (ratectl.RampFactor, 115); steps of 10 % went out in pairs, +21 % about every 5 s, and
// after a backoff right at the end of D9's 3 Mbit/s limit the way back to 8 Mbit/s took more than D9's 60 s. The
// extra 0.1 % keeps every step a hint whatever the rounding: with exactly 1.15, 2.3 % of the estimates (some of those
// ≡ 10 mod 20 bit/s) grew by round(1.15 e) − e, one bit/s short of congestionDue's round(0.15 e) (655 370 → 753 675
// is 98 305 against 98 306), and that step waited for the next one.
const (
	nativeLossHigh        = 10.0                   // percent of raw loss that backs off on one tick
	nativeLossMid         = 3.0                    // percent of raw loss that backs off on two consecutive ticks
	nativeLossLow         = 1.0                    // growth needs less raw loss than this on two consecutive ticks
	nativeBackoff         = 0.8                    // after loss: the estimate is 80 % of what got through
	nativeDelayHigh       = 40 * time.Millisecond  // a queue this deep on two consecutive ticks backs off
	nativeDelayLow        = 15 * time.Millisecond  // growth needs a queue below this (or none known) on two ticks
	nativeDelayBackoff    = 0.85                   // after a queue: the estimate is 85 % of the rate
	nativeDelayWindow     = 500 * time.Millisecond // QueueDelay is the minimum of the samples of this window
	nativeBaselineRise    = 500 * time.Microsecond // per second (500 ppm): how fast the delay baseline may rise
	nativeDelayMinSamples = 3                      // fewer samples in the window: QueueDelay unknown
	nativeGrowth          = 1.151                  // the estimate's growth step (WA54: §10.1 has 1.10)
	nativeHold            = 10 * time.Second       // growth is held this long after a backoff
	nativeMinCap          = 500_000                // bit/s: no backoff goes below this
	nativeMinRate         = 250_000                // bit/s: below this a tick has too few packets for a verdict
	nativeHintMinChange   = 0.15                   // a congestion hint changes the cap the sharer was told by this
	nativeHintMinGap      = 2 * time.Second        // a congestion hint comes this long after the share's last hint
)

// More numbers of M2 01 §10.1 that its constant list doesn't name.
const (
	// nativeResumeGrace: for this long after a layer resumes the rates of the layers say nothing (the resumed one
	// measures less than it sends while its 2 s average catches up), and the estimate stays as it is.
	nativeResumeGrace = 3 * time.Second
	// nativeGrowthEvery is the least time between two growth steps, and nativeGrowthRate the share of the estimate
	// that must arrive for one: an estimate that the sharer doesn't fill says nothing about more.
	nativeGrowthEvery = 2 * time.Second
	nativeGrowthRate  = 0.85
	// nativeCeilingSlack: the estimate is cleared once a growth step takes it to the sum of the preset's ceilings
	// times this.
	nativeCeilingSlack = 1.1
	// nativeDelayWarmup: during a Layer's first 2 s (from its first sample) QueueDelay is unknown, so the baseline
	// has settled before anything is decided by it.
	nativeDelayWarmup = 2 * time.Second
	// nativeDelaySlots is the capacity of the delay window's min-deque and sample ring: 240 fps × 0.5 s, with room.
	nativeDelaySlots = 256
	// videoTickNS converts the 90 kHz RTP clock to ns: 10⁹ / 90 000 = 100 000 / 9.
	videoTickNS, videoTickDiv = 100_000, 9
)

// HintReasonCongestion is the reason of a QualityHintEvent whose cap the native uplink estimator moved (M2 01 §10.1),
// 01's protocol.HintReasonCongestion. UplinkDecision.Reason has it when the estimate is the binding cap.
const HintReasonCongestion = "congestion"

// nativeUplink is the uplink policy of a native publisher's share (M2 01 §10.1): the M1 policy, so the admin's cap and
// layer pausing behave exactly as in M1, and an estimate of the sharer's uplink from each tick's stats. The decision's
// cap is the smaller of the admin's cap and the estimate (0 = none for either), with Reason HintReasonCongestion
// when the estimate binds. Like every UplinkPolicy it runs on the publishing Conn's actor only.
//
// A tick is one second of the ticker's stats (ShareUplinkStats.gen): the policy is also asked between the ticker's
// seconds (a viewer's new request for high, the admin's cap), on the same layer stats, and those looks get M1's
// decision and the cap of the current estimate but run no row, so a streak of consecutive ticks counts each second
// once and one second of loss or delay is never two.
//
// Each tick looks at the active video layers (the ones the policy hasn't paused): rate is the sum of their bitrates,
// loss their raw loss weighted by bitrate, delay the full layer's QueueDelay (else the preview layer's; −1 when
// neither is active or known). The rows of M2 01 §10.1 are checked top-down and the first that matches decides:
//
//   - rate below nativeMinRate, the sharer's own pause (Share.sharerPaused), or within nativeResumeGrace of a layer
//     resume: unchanged. Such a tick has no verdict, so it also ends every streak of consecutive ticks below.
//   - loss ≥ nativeLossHigh on this tick, or ≥ nativeLossMid on two consecutive ticks: max(nativeMinCap,
//     nativeBackoff × rate × (1 − loss/100)), growth held for nativeHold.
//   - delay ≥ nativeDelayHigh on two consecutive ticks: max(nativeMinCap, nativeDelayBackoff × rate), growth held.
//   - an estimate set, loss < nativeLossLow and delay < nativeDelayLow (or unknown) on two consecutive ticks each,
//     rate ≥ nativeGrowthRate × the estimate, nativeGrowthEvery since the last growth and not held: the estimate ×
//     nativeGrowth; once that reaches the sum of the preset's ceilings (under the admin's cap) × nativeCeilingSlack,
//     none.
//
// A backoff row never raises a set estimate: the new estimate is the smaller of the row's value and the current one.
// Since a row's value comes from what arrived, not from the estimate, a congestion that lasts lowers the estimate
// only as fast as the sharer lowers what it sends.
type nativeUplink struct {
	m1Uplink
	share *Share // the sharer's pause (sharerPaused, sharerResumedAt) and the preset's ceilings

	estimate  int    // bit/s; 0 = none
	heldUntil int64  // growth is held until then (the uplink clock)
	grewAt    int64  // the last growth step; 0 = none yet
	resumedAt int64  // the last layer resume this policy decided; 0 = none
	gen       uint64 // the generation of the stats the rows last ran on (SFU.uplinkGen)
	// The streaks: how many consecutive ticks with a verdict had loss ≥ nativeLossMid, loss < nativeLossLow, delay ≥
	// nativeDelayHigh, and delay < nativeDelayLow or unknown.
	lossMid, lossLow, delayHigh, delayLow int
}

// newNativeUplink returns the native policy of share sh, which starts now, on a clock. Its first tick is the ticker's
// next second.
func newNativeUplink(s *SFU, sh *Share, now func() int64) *nativeUplink {
	return &nativeUplink{m1Uplink: m1Uplink{sfu: s, now: now, wantedAt: now()}, share: sh, gen: s.uplinkGen.Load()}
}

func (p *nativeUplink) Tick(s ShareUplinkStats) UplinkDecision {
	d := p.m1Uplink.Tick(s)
	now := p.now()
	if d.Active[ridFull] {
		p.resumedAt = now
	}
	if s.gen != p.gen { // a new second of stats: the rows run once on it
		p.gen = s.gen
		p.step(s, now)
	}
	if p.estimate > 0 && (d.MaxBitrate == 0 || p.estimate < d.MaxBitrate) {
		d.MaxBitrate, d.Reason = p.estimate, HintReasonCongestion
	}
	return d
}

// step runs the rows of M2 01 §10.1 on one tick's stats.
func (p *nativeUplink) step(s ShareUplinkStats, now int64) {
	var (
		rate  int
		lossW float64
		delay = time.Duration(-1)
		full  bool
	)
	for _, l := range s.Layers {
		if l.RID == ridFull && p.paused {
			continue // the policy paused it: what it measures is the sharer stopping
		}
		rate += l.Bitrate
		lossW += l.RawLossPct * float64(l.Bitrate)
		switch l.RID {
		case ridFull:
			delay, full = l.QueueDelay, true
		case ridPreview:
			if !full {
				delay = l.QueueDelay
			}
		}
	}
	if rate < nativeMinRate || p.share.sharerPaused.Load() || p.resuming(now) {
		p.lossMid, p.lossLow, p.delayHigh, p.delayLow = 0, 0, 0, 0
		return
	}
	loss := lossW / float64(rate)
	streak(&p.lossMid, loss >= nativeLossMid)
	streak(&p.lossLow, loss < nativeLossLow)
	streak(&p.delayHigh, delay >= nativeDelayHigh)
	streak(&p.delayLow, delay < nativeDelayLow) // unknown (−1) counts as low
	switch {
	case loss >= nativeLossHigh || p.lossMid >= 2:
		p.backOff(max(nativeMinCap, int(math.Round(nativeBackoff*float64(rate)*(1-loss/100)))), now)
	case p.delayHigh >= 2:
		p.backOff(max(nativeMinCap, int(math.Round(nativeDelayBackoff*float64(rate)))), now)
	case p.estimate > 0 && p.lossLow >= 2 && p.delayLow >= 2 &&
		float64(rate) >= nativeGrowthRate*float64(p.estimate) &&
		(p.grewAt == 0 || now-p.grewAt >= int64(nativeGrowthEvery)) && now >= p.heldUntil:
		next := int(math.Round(float64(p.estimate) * nativeGrowth))
		if float64(next) >= nativeCeilingSlack*float64(p.ceilings()) {
			next = 0
		}
		p.estimate, p.grewAt = next, now
	}
}

// resuming reports whether a layer resumed less than nativeResumeGrace ago: one the policy resumed (layer pausing),
// or every layer at once when the sharer's own pause ended (M2 01 §10.3).
func (p *nativeUplink) resuming(now int64) bool {
	for _, at := range [...]int64{p.resumedAt, p.share.sharerResumedAt.Load()} {
		if at != 0 && now-at < int64(nativeResumeGrace) {
			return true
		}
	}
	return false
}

// backOff applies a backoff row's value: the estimate never rises by one, and growth is held.
func (p *nativeUplink) backOff(bps int, now int64) {
	if p.estimate == 0 || bps < p.estimate {
		p.estimate = bps
	}
	p.heldUntil = now + int64(nativeHold)
}

// ceilings returns the sum of the share's encodings' MaxBitrate (its preset's ceilings) under the admin's cap.
func (p *nativeUplink) ceilings() int {
	preset, _ := p.share.currentPreset()
	sum := 0
	for _, e := range presetEncodings(preset, p.sfu.currentLimits().shareCap(), 0) {
		sum += e.MaxBitrate
	}
	return sum
}

// streak counts a tick into a streak of consecutive ticks on which a condition held.
func streak(n *int, held bool) {
	if held {
		*n++
	} else {
		*n = 0
	}
}

// ---- the queueing delay (M2 01 §10.1) ----

// delayTrend is a video Layer's queueing-delay trend (M2 01 §10.1, LayerUplinkStats.QueueDelay). A native publisher's
// RTP timestamps come from capture times (M2 01 §8.2), so a frame's arrival time minus its RTP time (its one-way delay,
// owd) changes only with the queue on the path, plus the drift between the two machines' clocks. The constant offset
// (a random RTP base, unrelated clocks) cancels against the baseline.
//
//   - Sample: the first packet of each video frame, which is the packet sampleJitter takes too: the newest packet of
//     the stream with a timestamp newer than the last sample's. The Layer's RTP loop samples it with its monotonic
//     receive time; retransmissions (never the newest), padding (never sampled) and a timestamp older than the last
//     sample's (reordering) never sample. owd = arrival − ext_ts × 10⁹ / 90 000 in ns, with ext_ts the timestamp
//     unwrapped to 64 bits (ext += int32(ts − last_ts)), counted from the first sample's.
//   - Window: the minimum of the samples of the last nativeDelayWindow, kept in a monotonic min-deque of at most
//     nativeDelaySlots entries; a ring of as many arrival times counts the samples in the window.
//   - Baseline: the minimum since the Layer's first sample, allowed to rise slowly. On each tick it becomes
//     min(baseline + nativeBaselineRise × the seconds since the last tick, the minimum of the samples since the last
//     tick). It follows a sharer clock that runs slower than the server's and absorbs a standing queue only slowly
//     (a 40 ms queue after 80 s); a queue that drains brings it down at once. A tick without a sample since the last
//     one (a static screen, a paused layer) raises it by the rise alone, the minimum of no samples being none: the
//     clocks drift on meanwhile, and the next sample brings it down to where the path is.
//   - QueueDelay = window minimum − baseline, computed on the tick: −1 (unknown) with fewer than
//     nativeDelayMinSamples samples in the window, and during the first nativeDelayWarmup.
//
// A new Layer (a new SSRC) starts a new trend; a layer pause keeps it, and so do an encoder re-init and a profile
// switch, which keep the publisher's RTP timeline. mu is a leaf: the Layer's RTP loop takes it to sample (once per
// frame), the ticker to compute QueueDelay (once a second).
type delayTrend struct {
	mu      sync.Mutex
	started bool
	startAt int64  // the first sample's arrival
	lastTS  uint32 // the last sample's RTP timestamp
	ext     int64  // lastTS unwrapped, counted from the first sample's timestamp

	// The window: minq is a monotonic min-deque of the samples (owd increasing from its front), times a ring of the
	// samples' arrivals. Both are rings of nativeDelaySlots with a head and a length.
	minq          [nativeDelaySlots]owdSample
	minHead, minN int
	times         [nativeDelaySlots]int64
	tHead, tN     int

	// sinceTick is the minimum owd of the samples since the last tick, valid while fresh.
	sinceTick int64
	fresh     bool
	baseline  int64
	based     bool
	lastTick  int64
	ticked    bool
}

// owdSample is one sample of a delayTrend: its arrival and its one-way delay, in ns.
type owdSample struct{ at, owd int64 }

// sample takes the first packet of a frame: its RTP timestamp and its arrival (monoNow). A timestamp that isn't newer
// than the last sample's takes nothing. Only the Layer's RTP loop calls it.
func (d *delayTrend) sample(ts uint32, at int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.started {
		step := int32(ts - d.lastTS)
		if step <= 0 {
			return
		}
		d.ext += int64(step)
	} else {
		d.started, d.startAt = true, at
	}
	d.lastTS = ts
	owd := at - d.ext*videoTickNS/videoTickDiv
	d.evict(at)
	for d.minN > 0 && d.minq[(d.minHead+d.minN-1)%nativeDelaySlots].owd >= owd {
		d.minN--
	}
	if d.minN == nativeDelaySlots { // more than 512 frames a second: the oldest goes
		d.minHead, d.minN = (d.minHead+1)%nativeDelaySlots, d.minN-1
	}
	d.minq[(d.minHead+d.minN)%nativeDelaySlots] = owdSample{at: at, owd: owd}
	d.minN++
	if d.tN == nativeDelaySlots {
		d.tHead, d.tN = (d.tHead+1)%nativeDelaySlots, d.tN-1
	}
	d.times[(d.tHead+d.tN)%nativeDelaySlots] = at
	d.tN++
	if !d.fresh || owd < d.sinceTick {
		d.sinceTick, d.fresh = owd, true
	}
}

// evict drops the samples that arrived more than nativeDelayWindow before now. d.mu is held.
func (d *delayTrend) evict(now int64) {
	from := now - int64(nativeDelayWindow)
	for d.minN > 0 && d.minq[d.minHead].at < from {
		d.minHead, d.minN = (d.minHead+1)%nativeDelaySlots, d.minN-1
	}
	for d.tN > 0 && d.times[d.tHead] < from {
		d.tHead, d.tN = (d.tHead+1)%nativeDelaySlots, d.tN-1
	}
}

// tick moves the baseline on and returns QueueDelay at now: the window's minimum above the baseline, or −1 when it
// is unknown. Only the ticker calls it, once a second (Layer.tick).
func (d *delayTrend) tick(now int64) time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.based && d.ticked && now > d.lastTick {
		d.baseline += int64(float64(now-d.lastTick) * float64(nativeBaselineRise) / float64(time.Second))
	}
	if d.fresh && (!d.based || d.sinceTick < d.baseline) {
		d.baseline, d.based = d.sinceTick, true
	}
	d.fresh, d.lastTick, d.ticked = false, now, true
	d.evict(now)
	if !d.based || d.tN < nativeDelayMinSamples || d.minN == 0 || now-d.startAt < int64(nativeDelayWarmup) {
		return -1
	}
	return time.Duration(max(d.minq[d.minHead].owd-d.baseline, 0))
}
