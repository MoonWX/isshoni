package sfu

import (
	"cmp"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

// This file is the downlink allocator of 02 §10.2: one per subscriber Conn, Galene-style (the design, not its code).
// It caps the full layer of a viewer's high requests at the preview layer when its downlink is congested, and tries
// an upgrade again after a backoff.
//
// REMB is only an overuse signal, never permission to upgrade. Chrome's receive-side estimator (libwebrtc's AIMD on
// the sub PC's abs-send-time) never raises its estimate above about 1.5 × the rate it receives now + 10 kbit/s, and
// by only about 8 % a second; it lowers it only on overuse, to about 0.85 × the received rate, and then sends a REMB
// at once. So a REMB below a layer the viewer isn't receiving yet says nothing about the link, and a viewer held on
// low could never report a REMB high enough to earn high back: upgrades are tried (probing), not granted.
//
// The inputs, and where they come from:
//   - the viewer's REMBs, from the RTCP readers of its DownTracks (onREMB). The same REMB reaches every DownTrack it
//     names; the allocator keeps the latest and compares each new one with it, so a copy changes nothing;
//   - the loss its receiver reports give for its video DownTracks (onLoss, DownTrack.noteReport), as the link lost it:
//     the sequence numbers the SFU itself never sent (an upstream gap, a packet the queue dropped) are taken out
//     (seqGaps), and a report that comes long after the one before covers an outage of the viewer's network rather
//     than congestion, and only starts the count again. One that comes right after the one before (reportMinGap)
//     is folded into the next, so a viewer that reports at any rate costs no more than one that reports five
//     times a second. A report says something about a decision of the allocator only when all of the time it
//     covers came after the decision;
//   - what the queues of its DownTracks dropped (drops; at these rates the writer is starved of CPU);
//   - the rate the SFU forwards to it now: its DownTracks' bitrates, 2 s moving averages (the actor's step);
//   - the measured bitrate of each layer, 2 s moving averages (02 §9.1), for what fits the budget.
//
// The decisions are its Conn's actor's (Conn.adapt): once a second, when the ticker wakes it, and at once when a
// REMB falls or a report shows loss. The caps it sets are the subscriptions' server cap (Conn.capSubscription, 02
// §10.1), with the reason bandwidth.

// allocState is the allocator's state (02 §10.2).
type allocState uint8

// The states: normal (no caps), limited (the high requests that don't fit the budget are capped) and probing (a
// trial upgrade: the caps are gone for up to 6 s).
const (
	allocNormal allocState = iota
	allocLimited
	allocProbing
)

func (s allocState) String() string {
	switch s {
	case allocNormal:
		return "normal"
	case allocLimited:
		return "limited"
	case allocProbing:
		return "probing"
	}
	return "state?"
}

// The allocator's numbers (02 §10.2).
const (
	// A REMB at least rembFallPct % below the previous one from the viewer falls; the previous one counts only if it
	// came less than rembFresh before. After a longer gap (no video was forwarded, say) a REMB is only a new baseline,
	// and the latest REMB is known only for rembFresh.
	rembFallPct = 3
	rembFresh   = 5 * time.Second
	// maxREMB (100 Gbit/s): a REMB above it is ignored, like one of 0. No real estimate comes near it, and what Pion
	// parses goes up to 2^18 × 2^63, past an int64: converting such a float32 gives MaxInt64 on arm64 and MinInt64 on
	// amd64, and the products of onREMB and step would overflow.
	maxREMB = 100_000_000_000
	// Loss is averaged over lossWindow. At lossCongestedPct % or more it is congestion; during a trial upgrade,
	// lossProbePct % fails the trial; and the backoff ends only while it stays under lossRecoveredPct % over
	// recoverWindow. A verdict needs lossMinPackets packets: one lost of three is no 33 % of anything.
	lossWindow       = 2 * time.Second
	recoverWindow    = 3 * time.Second
	lossCongestedPct = 10
	lossProbePct     = 5
	lossRecoveredPct = 2
	lossMinPackets   = 20
	// reportGap: a receiver report of a stream that comes more than reportGap after the last one covers an outage
	// of the viewer's network: its RTCP didn't reach the SFU either, which downlink congestion doesn't do (the
	// reports travel the other way). Such a report only starts the count again. Browsers send one about every second.
	reportGap = 2 * time.Second
	// reportMinGap: a receiver report of a stream that comes less than reportMinGap after the last one that counted
	// is folded into the next one, which then covers both intervals and the gaps in them. The viewer chooses how
	// often it reports, and each report that counts is kept for recoverWindow: this keeps that to about 15 a stream.
	// maxLossReports bounds the reports kept, of all of the Conn's streams; past it the oldest go.
	reportMinGap   = 200 * time.Millisecond
	maxLossReports = 1024
	// More than dropsCongested queue drops a second is congestion.
	dropsCongested = 50
	// The budget, on congestion: budgetREMBPct % of an overuse REMB; on loss or drops, budgetForwardedPct % of the
	// rate forwarded now, or budgetREMBPct % of the latest REMB when that is less.
	budgetREMBPct      = 90
	budgetForwardedPct = 85
	// The backoff before a trial upgrade starts at backoffStart, doubles with each trial that fails, up to
	// backoffMax, and starts again at backoffStart once a trial has passed. A trial passes after probeTime without
	// a fall of the REMB, loss or drops.
	backoffStart = 10 * time.Second
	backoffMax   = 120 * time.Second
	probeTime    = 6 * time.Second
)

// Why the allocator capped (allocDecision.cause), for its log line and the tests.
const (
	causeREMB  = "remb"
	causeLoss  = "loss"
	causeDrops = "drops"
)

// allocator is one viewer Conn's downlink allocator (02 §10.2). The RTCP readers of the Conn's DownTracks hand it what
// the viewer reports (onREMB, onLoss) under its own lock, a leaf; the decisions are the Conn's actor's (step, then
// Conn.adapt applies them), and their state below "the actor's" has no lock.
type allocator struct {
	// forwarded is the rate the SFU forwards to the viewer, in bit/s: the bitrates of its DownTracks, video and audio
	// (2 s moving averages), as the actor's last step summed them. A REMB that falls is an overuse only below it.
	forwarded atomic.Int64
	// drops counts the packets the queues of the Conn's DownTracks dropped (DownTrack.enqueue).
	drops atomic.Uint64
	// queued: a look by the actor is in its event queue and hasn't run yet (Conn.wakeAllocator).
	queued atomic.Bool
	// estimate is ConnStats.DownlinkEstimate: the budget while limited, else the latest REMB while it is fresh, else
	// 0. The actor's step stores it.
	estimate atomic.Int64

	// mu guards the inputs below, and the rr of each of the Conn's DownTracks. It is a leaf: nothing is called while
	// it is held.
	mu     sync.Mutex
	remb   int64 // the latest REMB, bit/s; 0 before the first
	rembAt int64 // when it came, on the allocator's clock (SFU.allocNow)
	// fall is the lowest REMB that fell since the actor's last step, 0 for none; overuse the lowest of those that
	// was also below the forwarded rate (an overuse REMB).
	fall, overuse int64
	// losses are what the receiver reports said, the last recoverWindow of them (at most maxLossReports), oldest
	// first.
	losses []reportLoss

	// The actor's.
	state   allocState
	budget  int64 // bit/s, while limited
	backoff time.Duration
	// since is when the backoff started (limited) or the trial began (probing).
	since int64
	// evidence is the time of the last decision: a receiver report that covers any time before it describes what the
	// viewer got before, and is no evidence about what it gets now (a report covers the second or so since the last).
	evidence int64
	// dropsSeen and dropsAt are the drops as the last step that measured their rate saw them, and when.
	dropsSeen uint64
	dropsAt   int64
	dropsInit bool
	// order counts the requests for high (Subscription.highAt): the newer ones are served first.
	order uint64
	// cause is why the allocator capped last (causeREMB, causeLoss or causeDrops), for the tests.
	cause string
}

// reportLoss is one receiver report about one video DownTrack, as the link lost it: the interval it covers (from the
// last report of the stream to this one), how many packets the SFU sent in it, and how many of those the viewer lost.
type reportLoss struct {
	from, at, expected, lost int64
}

// allocDecision is what a step decided.
type allocDecision struct {
	from, to allocState
	// recap: the caps must be decided again within the budget (the allocator entered limited, or lowered it);
	// lift: every cap goes (a trial upgrade).
	recap, lift bool
	cause       string // why it capped: causeREMB, causeLoss or causeDrops
}

// onREMB takes one REMB of the viewer at now (bit/s). It reports whether the REMB fell: at least 3 % below the
// previous one, which came less than 5 s before. Only a fall needs the actor's look before its next tick.
func (a *allocator) onREMB(bps, now int64) (fell bool) {
	if bps <= 0 {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	prev, prevAt := a.remb, a.rembAt
	a.remb, a.rembAt = bps, now
	if prev == 0 || now-prevAt >= int64(rembFresh) || bps*100 > prev*(100-rembFallPct) {
		return false
	}
	if a.fall == 0 || bps < a.fall {
		a.fall = bps
	}
	if bps < a.forwarded.Load() && (a.overuse == 0 || bps < a.overuse) {
		a.overuse = bps
	}
	return true
}

// onLoss takes what one receiver report says of a video stream, which covers the time from from (the stream's last
// report) to now: of expected packets the SFU sent in it, the viewer lost lost. It reports whether that is enough
// loss to fail a trial, which needs the actor's look before its next tick.
//
// The reports come in the order of the clock (near enough: each RTCP reader reads it before it takes the lock), so
// the ones older than recoverWindow are at the front, and go from there; so does the oldest past maxLossReports.
// (One a little out of order stays a little longer; lossSince counts by time, not by place.)
func (a *allocator) onLoss(expected, lost, from, now int64) bool {
	if expected <= 0 {
		return false
	}
	lost = min(max(lost, 0), expected)
	a.mu.Lock()
	old := 0
	for old < len(a.losses) && now-a.losses[old].at > int64(recoverWindow) {
		old++
	}
	old = max(old, len(a.losses)+1-maxLossReports)
	a.losses = append(a.losses[old:], reportLoss{from: from, at: now, expected: expected, lost: lost})
	a.mu.Unlock()
	return lost*100 >= expected*lossProbePct
}

// lossSince returns the packets expected and lost in the reports that came from since to now and cover no time before
// the last decision (evidence). a.mu is held.
func (a *allocator) lossSince(since, now int64) (expected, lost int64) {
	for _, s := range a.losses {
		if s.at >= since && s.at <= now && s.from >= a.evidence {
			expected += s.expected
			lost += s.lost
		}
	}
	return expected, lost
}

// lossy reports whether expected and lost are a verdict of pct % or more.
func lossy(expected, lost int64, pct int64) bool {
	return expected >= lossMinPackets && lost*100 >= expected*pct
}

// dropRate returns the queue drops per second since the last time it measured them, once at least most of a second
// has passed; until then 0. The actor's step calls it with the count so far.
func (a *allocator) dropRate(now int64, drops uint64) float64 {
	if !a.dropsInit {
		a.dropsInit, a.dropsAt, a.dropsSeen = true, now, drops
		return 0
	}
	elapsed := time.Duration(now - a.dropsAt)
	if elapsed < 900*time.Millisecond {
		return 0
	}
	rate := float64(drops-a.dropsSeen) / elapsed.Seconds()
	a.dropsAt, a.dropsSeen = now, drops
	return rate
}

// step is the allocator's state machine (02 §10.2), at now with the rate forwarded to the viewer and the drops of
// its DownTracks' queues so far. The Conn's actor calls it, and applies what it returns (Conn.adapt).
//
//	normal   congestion                                → budget, cap what doesn't fit → limited (backoff 10 s)
//	limited  congestion                                → lower the budget, cap again; the backoff starts again
//	limited  backoff elapsed, loss < 2 % over 3 s      → lift the caps (a trial upgrade) → probing
//	probing  a fall of the REMB, loss ≥ 5 %, or drops   → budget, cap again; backoff = min(2 × backoff, 120 s) → limited
//	probing  6 s pass cleanly                          → backoff = 10 s → normal
//
// Congestion is an overuse REMB (a fall below the forwarded rate), loss ≥ 10 % averaged over 2 s, or more than 50
// queue drops a second. The budget is 0.9 × an overuse REMB; on loss or drops min(0.9 × the latest REMB, 0.85 × the
// forwarded rate), or 0.85 × the forwarded rate when no REMB is known.
func (a *allocator) step(now, forwarded int64, drops uint64) allocDecision {
	a.forwarded.Store(forwarded)
	a.mu.Lock()
	fall, overuse := a.fall, a.overuse
	a.fall, a.overuse = 0, 0
	var remb int64
	if a.remb > 0 && now-a.rembAt < int64(rembFresh) {
		remb = a.remb
	}
	expected, lost := a.lossSince(now-int64(lossWindow), now)
	expected3, lost3 := a.lossSince(now-int64(recoverWindow), now)
	a.mu.Unlock()
	dropping := a.dropRate(now, drops) > dropsCongested

	d := allocDecision{from: a.state, to: a.state}
	var cause string
	if a.state == allocProbing {
		switch {
		case fall > 0:
			cause, overuse = causeREMB, fall
		case lossy(expected, lost, lossProbePct):
			cause = causeLoss
		case dropping:
			cause = causeDrops
		}
	} else {
		switch {
		case overuse > 0:
			cause = causeREMB
		case lossy(expected, lost, lossCongestedPct):
			cause = causeLoss
		case dropping:
			cause = causeDrops
		}
	}

	switch {
	case cause != "":
		budget := forwarded * budgetForwardedPct / 100
		if remb > 0 {
			budget = min(budget, remb*budgetREMBPct/100)
		}
		if cause == causeREMB {
			budget = overuse * budgetREMBPct / 100
		}
		switch a.state {
		case allocNormal:
			a.budget, a.backoff = budget, backoffStart
		case allocLimited:
			a.budget = min(a.budget, budget)
		case allocProbing:
			a.budget, a.backoff = budget, min(2*a.backoff, backoffMax)
		}
		a.state, a.since, a.evidence, a.cause = allocLimited, now, now, cause
		d.to, d.recap, d.cause = allocLimited, true, cause
	case a.state == allocLimited && now-a.since >= int64(a.backoff) && !lossy(expected3, lost3, lossRecoveredPct):
		a.state, a.since, a.evidence = allocProbing, now, now
		d.to, d.lift = allocProbing, true
	case a.state == allocProbing && now-a.since >= int64(probeTime):
		a.state, a.backoff = allocNormal, backoffStart
		d.to = allocNormal
	}

	if a.state == allocLimited {
		a.estimate.Store(a.budget)
	} else {
		a.estimate.Store(remb)
	}
	return d
}

// allocItem is one subscription as the allocation weighs it (all rates in bit/s, measured: 2 s moving averages).
type allocItem struct {
	high  bool   // the client asks for the video at high
	order uint64 // when it asked for high (Subscription.highAt): the newest is served first
	audio int64  // the share's audio, when the client asks for it
	// full is the layer a request for high gets; preview the one a cap at low gives it, or, for a request for low,
	// the layer that serves it.
	full, preview int64
	// cappable: the share has a smaller layer for a cap at low than the one high gets. A share without one keeps
	// its single layer: a cap would leave its viewer with no video at all.
	cappable bool
}

// allocate fills a budget cheapest first (02 §10.2): every audio the viewer asks for, every thumbnail (a request for
// low, which the server never turns off: the client decides how many a phone shows), then the high requests, the
// most recently made first. Each is served at the full layer if that layer's measured rate fits what is left, else
// capped at low (and its preview layer counts instead). So the oldest high requests are capped first. It returns,
// for each item, whether its request for high is capped.
func allocate(budget int64, items []allocItem) []bool {
	capped := make([]bool, len(items))
	left := budget
	var highs []int
	for i, it := range items {
		left -= it.audio
		if it.high {
			highs = append(highs, i)
		} else {
			left -= it.preview
		}
	}
	slices.SortStableFunc(highs, func(x, y int) int { return cmp.Compare(items[y].order, items[x].order) })
	for _, i := range highs {
		it := items[i]
		if !it.cappable || it.full <= left {
			left -= it.full
			continue
		}
		capped[i] = true
		left -= it.preview
	}
	return capped
}

// ---- the Conn's side: inputs from the media path, decisions on the actor ----

// allocNow is the allocator's clock: monoNow, moved on in tests (allocSkew).
func (s *SFU) allocNow() int64 { return monoNow() + s.allocSkew.Load() }

// wakeAllocator has the Conn's actor look at its allocator (adapt), unless a look is queued already. It never blocks:
// the ticker calls it once a second, and the RTCP readers when a REMB falls or a report shows loss.
func (c *Conn) wakeAllocator() {
	if c.alloc.queued.CompareAndSwap(false, true) {
		c.post(c.adapt)
	}
}

// adapt runs the allocator's step on the Conn's actor and applies its decision (02 §10.2): on congestion the high
// requests that don't fit the budget are capped at low, with the reason bandwidth; a trial upgrade lifts every cap.
func (c *Conn) adapt() {
	a := &c.alloc
	a.queued.Store(false)
	var forwarded int64
	for _, sub := range c.subs {
		for _, dt := range sub.tracks() {
			forwarded += dt.stats.bitrate.Load()
		}
	}
	d := a.step(c.sfu.allocNow(), forwarded, a.drops.Load())
	if d.from != d.to || d.recap {
		c.log.Debug("downlink allocator", "from", d.from.String(), "to", d.to.String(), "cause", d.cause,
			"budget", a.budget, "backoff", a.backoff, "forwarded", forwarded)
	}
	switch {
	case d.lift:
		c.liftCaps()
	case d.recap:
		c.capWithin(nil)
	}
}

// capWithin decides the bandwidth caps of every subscription again within the allocator's budget (allocate), and
// applies the ones that changed, each with its report to the client (capSubscription). A request changed by the
// caller, except, gets its new cap without being applied: the caller applies and reports it with the rest of what
// changed (UpdateSubscriptions). It runs on the actor, while limited.
func (c *Conn) capWithin(except *Subscription) {
	ids := slices.Sorted(maps.Keys(c.subs))
	items := make([]allocItem, len(ids))
	for i, id := range ids {
		items[i] = c.subs[id].allocItem()
	}
	capped := allocate(c.alloc.budget, items)
	for i, id := range ids {
		sub := c.subs[id]
		if capped[i] == (sub.capReason != SubReasonNone) {
			continue
		}
		q, reason := QualityHigh, SubReasonNone
		if capped[i] {
			q, reason = QualityLow, SubReasonBandwidth
		}
		if sub == except {
			sub.capQ, sub.capReason = q, reason
			continue
		}
		c.capSubscription(sub, q, reason)
	}
}

// liftCaps removes the bandwidth cap of every subscription that has one: a trial upgrade (02 §10.2). It runs on the
// actor.
func (c *Conn) liftCaps() {
	for _, id := range slices.Sorted(maps.Keys(c.subs)) {
		if sub := c.subs[id]; sub.capReason != SubReasonNone {
			c.capSubscription(sub, QualityHigh, SubReasonNone)
		}
	}
}

// requestChanged runs on the actor when the client changed what a subscription asks for (UpdateSubscriptions), before
// the subscription is applied. A new request for high is the newest from now on. While the allocator is limited the
// caps are decided again: a new high request is served at the full layer only if its measured rate fits the budget
// (02 §10.2). In normal and probing nothing is capped: a new or raised target is forwarded at once.
func (c *Conn) requestChanged(sub *Subscription, wasHigh bool) {
	if sub.reqVideo == QualityHigh && !wasHigh {
		c.alloc.order++
		sub.highAt = c.alloc.order
	}
	if c.alloc.state == allocLimited {
		c.capWithin(sub)
	}
}

// allocItem returns the subscription as the allocation weighs it, from the layers its share has now and their
// measured rates. A subscription whose viewer can't decode the video (Subscription.codec) gets none and weighs only
// its audio.
func (s *Subscription) allocItem() allocItem {
	layers := s.share.layerSet()
	rate := func(slot Slot) int64 {
		if l := layers[slot]; l != nil {
			return l.stats.bitrate.Load()
		}
		return 0
	}
	var present uint32
	for slot, l := range layers {
		if l != nil {
			present |= 1 << slot
		}
	}
	it := allocItem{order: s.highAt}
	if s.reqAudio && layers[SlotAudio] != nil {
		it.audio = rate(SlotAudio)
	}
	if s.codec != SubReasonNone {
		return it
	}
	low, lowOK := selectSlot(webrtc.RTPCodecTypeVideo, QualityLow, present)
	switch s.reqVideo {
	case QualityHigh:
		it.high = true
		if high, ok := selectSlot(webrtc.RTPCodecTypeVideo, QualityHigh, present); ok {
			it.full = rate(high)
			it.cappable = lowOK && low != high
		}
		if it.cappable {
			it.preview = rate(low)
		}
	case QualityLow:
		if lowOK {
			it.preview = rate(low)
		}
	}
	return it
}

// layerSet returns the Layers attached now, by slot.
func (s *Share) layerSet() [SlotAudio + 1]*Layer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.layers
}

// ---- what the viewer's RTCP says (02 §9.3) ----

// rrState is what a video DownTrack keeps of the last receiver report about its stream, to tell what the next one
// adds: its SSRC, the extended highest sequence number, the cumulative loss, and when it came. Its Conn's allocator
// lock guards it.
type rrState struct {
	ok      bool
	ssrc    uint32
	highest uint32
	lost    uint32
	at      int64
}

// noteREMB hands one REMB of the viewer to its Conn's allocator. It reports whether the REMB fell, so that the
// allocator decides before its next tick. A REMB of nothing, or of more than maxREMB, is ignored.
func (d *DownTrack) noteREMB(bps float32, now int64) bool {
	if d.alloc == nil || !(bps > 0 && bps <= maxREMB) {
		return false
	}
	return d.alloc.onREMB(int64(bps), now)
}

// noteReport hands what a receiver report about the video DownTrack's stream adds to the last one to its Conn's
// allocator, as loss on the link: of the sequence numbers the report covers, those the SFU never sent (seqGaps) were
// not the link's to lose, and are taken out of both counts. The first report of a stream, and one that comes more than
// reportGap after the last (an outage, not congestion), only start the count. One that comes less than reportMinGap
// after the last is folded into the next: it changes nothing, so the next covers its interval and its gaps too. It
// reports whether the allocator should decide before its next tick.
func (d *DownTrack) noteReport(r rtcp.ReceptionReport, now int64) bool {
	a := d.alloc
	if a == nil || d.kind != webrtc.RTPCodecTypeVideo {
		return false
	}
	a.mu.Lock()
	prev := d.rr
	if prev.ok && prev.ssrc == r.SSRC && now-prev.at < int64(reportMinGap) {
		a.mu.Unlock()
		return false
	}
	d.rr = rrState{ok: true, ssrc: r.SSRC, highest: r.LastSequenceNumber, lost: r.TotalLost, at: now}
	a.mu.Unlock()
	skipped := d.gaps.take(uint16(r.LastSequenceNumber)) // a leaf of its own, never inside the allocator's lock
	if !prev.ok || prev.ssrc != r.SSRC || now-prev.at > int64(reportGap) {
		return false
	}
	expected := int64(int32(r.LastSequenceNumber - prev.highest))
	lost := max(int64(r.TotalLost)-int64(prev.lost), 0)
	if expected <= 0 {
		return false
	}
	return a.onLoss(expected-skipped, lost-skipped, prev.at, now)
}

// ---- the sequence numbers a video DownTrack passed over ----

// seqGaps are the own sequence numbers a video DownTrack's writer passed over without sending them: a gap the
// publisher left that no repair filled in time, or packets its queue dropped. The viewer counts them as lost, but the
// link didn't lose them, so the allocator takes them out of the loss its receiver reports show (DownTrack.noteReport).
// Each entry is keyed by the sequence number that follows the gap: a report whose highest sequence number has reached
// it covers the gap. A packet the writer sends late, into a gap it passed over, is an entry of -1. Its lock is a
// leaf; the writer adds, the RTCP reader takes.
type seqGaps struct {
	mu      sync.Mutex
	entries []seqGap
}

type seqGap struct {
	seq uint16
	n   int64
}

// seqGapEntries bounds the entries kept: past it the newest takes in the count of the next one.
const seqGapEntries = 64

// add notes n sequence numbers passed over before seq (n < 0: seq was sent late, into one of them).
func (g *seqGaps) add(seq uint16, n int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if l := len(g.entries); l >= seqGapEntries {
		g.entries[l-1].seq, g.entries[l-1].n = seq, g.entries[l-1].n+n
		return
	}
	g.entries = append(g.entries, seqGap{seq: seq, n: n})
}

// take returns the sequence numbers passed over that a report with the highest sequence number upTo covers, and
// forgets them.
func (g *seqGaps) take(upTo uint16) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	var n int64
	g.entries = slices.DeleteFunc(g.entries, func(e seqGap) bool {
		if int16(e.seq-upTo) > 0 {
			return false
		}
		n += e.n
		return true
	})
	return n
}

// noteSeq notes that the writer hands Pion own sequence number seq on the binding it writes to: a gap behind the
// highest one before is the SFU's own, and so is a late number that falls into such a gap (02 §10.2, seqGaps). Only
// video DownTracks with an allocator keep it.
func (d *DownTrack) noteSeq(w *rtpWriteState, seq uint16) {
	if d.alloc == nil || d.kind != webrtc.RTPCodecTypeVideo {
		return
	}
	if !w.seqKnown {
		w.seqKnown, w.seqHigh = true, seq
		return
	}
	switch ahead := int16(seq - w.seqHigh); {
	case ahead > 1:
		d.gaps.add(seq, int64(ahead)-1)
		w.seqHigh = seq
	case ahead == 1:
		w.seqHigh = seq
	case ahead < 0:
		d.gaps.add(seq, -1)
	}
}
