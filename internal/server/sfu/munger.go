package sfu

// Ported from spikes/s4-sfu/munger.go (ours, Apache-2.0) with the M1 changes of 02 §9.4: epochs and the sequence
// map, SR-aligned timestamps on a layer switch, padding gaps closed, Layer instances instead of rids, and profile
// epochs.

import (
	"math"
	"math/rand/v2"
	"time"
)

// Munger limits (02 §9.4).
const (
	epochRing     = 64                       // epochs kept for NACK lookups and late packets
	nackLookback  = 2 * time.Second          // lookup ignores epochs that ended longer ago
	srMaxAge      = 10 * time.Second         // an older sender report doesn't align timestamps
	maxLateSeq    = cacheMaxSlots            // a packet further behind is dropped: no cache holds it anyway
	maxEpochSpan  = 1 << 14                  // an epoch's start trails its newest packet by at most this (int16 safety)
	maxWallJump   = 1 << 30                  // cap on a wall-clock timestamp step, so int32 comparisons stay valid
	maxWallJumpNs = int64(8 * time.Hour)     // longer pauses step by maxWallJump without overflowing the product
	nsPerSecond   = int64(time.Second)       // for tick conversions
	ntpFracUnit   = float64(uint64(1) << 32) // NTP 32.32: units per second
)

// verdict is what the munger decides for one upstream packet.
type verdict uint8

const (
	// verdictDrop: not forwarded.
	verdictDrop verdict = iota
	// verdictWaitKeyframe: not forwarded, because the munger waits for a keyframe of the target layer and this
	// packet is from that layer. The DownTrack requests a keyframe (throttled by Share.requestKeyframe, 02 §9.7),
	// which heals a lost PLI.
	verdictWaitKeyframe
	// verdictForward: forwarded with the returned seq and ts.
	verdictForward
	// verdictNewEpoch: forwarded, and the packet started an epoch. The writer then sends a translated SR of the new
	// layer (02 §9.6).
	verdictNewEpoch
)

// forwarded reports whether the packet goes out.
func (v verdict) forwarded() bool { return v == verdictForward || v == verdictNewEpoch }

// epoch is one stretch of output that maps upstream packets of one Layer, with one profile, to own sequence numbers
// and timestamps by fixed offsets: own seq = upstream seq + seqOff, own ts = upstream ts + tsOff. An epoch starts at
// every discontinuity: a layer switch, a resume after a pause, a publisher rebuild (a new Layer instance), a profile
// change, and each in-order padding packet (02 §9.4).
type epoch struct {
	layer   *Layer
	profile ProfileKey
	startS  uint16 // first own seq the epoch maps (startS = startU + seqOff)
	startU  uint16 // first upstream seq it maps
	seqOff  uint16
	tsOff   uint32
	at      int64 // monotonic ns when it started
}

// munger rewrites a DownTrack's sequence numbers and timestamps so the viewer sees one continuous stream across layer
// switches, pauses, publisher rebuilds and profile changes (02 §9.4). Its epoch ring is the sequence map that NACKs
// are translated through (02 §9.5). It is not safe for concurrent use: DownTrack.mu guards it.
type munger struct {
	clock  uint32 // RTP clock rate: 90000 for video, 48000 for audio
	seq0   uint16 // the first epoch's first own seq: random (RFC 3550), from newMunger
	ts0    uint32 // the first epoch's first own ts: random, from newMunger
	active bool   // false while paused (quality off, or audio off): nothing is forwarded
	target Slot   // the slot the viewer should get; SlotAudio for audio

	epochs [epochRing]epoch // ring; the newest is epochs[(n-1)%epochRing]
	n      int              // epochs started so far
	// forwarding: the newest epoch is current and forwards. False before the first epoch and after a pause; the next
	// epoch then starts on a keyframe of the target.
	forwarding bool

	lastS    uint16 // highest own seq sent
	lastTS   uint32 // highest own ts sent
	lastAt   int64  // when lastTS last moved
	lastU    uint16 // highest upstream seq processed in the current epoch
	started  bool   // at least one packet forwarded: lastS, lastTS and lastAt are set
	switches int    // epochs that started on another Layer than the previous epoch (the first epoch counts)
}

// newMunger returns a paused munger for a stream with the given RTP clock rate and a random start.
func newMunger(clock uint32) *munger {
	// Not secrets: RFC 3550 asks for random initial values so that known-plaintext attacks on SRTP get harder.
	return &munger{clock: clock, seq0: uint16(rand.Uint32()), ts0: rand.Uint32()} //nolint:gosec // see above
}

// setTarget sets the slot the viewer should get and whether anything is forwarded. A new target keeps the old layer
// flowing until the new one's keyframe arrives; a pause (active false) is immediate, and a resume waits for a
// keyframe (audio packets always are one). It reports whether anything changed.
func (m *munger) setTarget(slot Slot, active bool) bool {
	changed := m.active != active || (active && m.target != slot)
	m.active = active
	if active {
		m.target = slot
	} else {
		m.forwarding = false
	}
	return changed
}

// restart ends the current epoch without pausing: the output starts again, right after the last own seq, with the
// next keyframe of the target. The DownTrack calls it when the viewer didn't get what the munger numbered, or can't
// get what comes next (02 §9.3): the start of its stream never left the server, its sub PC is no longer connected,
// or its track was unbound. To the viewer the stream then simply begins, or goes on, with a keyframe, as after a
// pause.
func (m *munger) restart() { m.forwarding = false }

// unforwardable tells the munger of a packet that process never sees, because the viewer has no payload type for its
// profile. If it is a newer packet of the Layer forwarded now, that Layer has moved on without the viewer and the
// current epoch is over, as it is when process meets another profile (02 §9.4, "Further rules"): going on within the
// Layer later would leave a sequence gap that NACKs fill from the cache, with packets the viewer can't decode under
// the old profile's payload type. The output then starts again with the next keyframe the viewer has a payload type
// for. It reports whether it ended the epoch. A late packet, or one of another Layer, changes nothing.
func (m *munger) unforwardable(p *packet) bool {
	if !m.active || !m.forwarding || m.newest().layer != p.layer || int16(p.seq-m.lastU) <= 0 {
		return false
	}
	m.forwarding = false
	return true
}

// current returns the slot of the layer forwarded now, and false while nothing is forwarded.
func (m *munger) current() (Slot, bool) {
	if !m.active || !m.forwarding {
		return 0, false
	}
	return m.newest().layer.slot, true
}

// forwards reports whether the current epoch is on Layer l: l is what the viewer gets now.
func (m *munger) forwards(l *Layer) bool { return m.forwarding && m.newest().layer == l }

// waitingForKeyframe reports whether the munger waits for a keyframe of the target slot before it forwards it: it is
// active and forwards nothing, or another slot. The DownTrack requests a keyframe when this becomes true. (A new
// Layer instance or a new profile in the target slot is noticed only when its packets arrive: process then returns
// verdictWaitKeyframe.)
func (m *munger) waitingForKeyframe() bool {
	return m.active && (!m.forwarding || m.newest().layer.slot != m.target)
}

// process decides whether an upstream packet is forwarded, and returns its own seq and ts if it is (02 §9.4):
//   - a packet of the target slot starts an epoch when nothing is forwarded, or when it comes from another Layer
//     instance or with another profile than the current epoch's, but only if it starts a keyframe. Until then the
//     current layer keeps flowing, and target packets return verdictWaitKeyframe;
//   - a newer packet of the current Layer with another profile ends forwarding: the old profile's stream is over, and
//     skipping ahead within the same Layer would leave a sequence gap that NACKs can't fill with the right profile;
//   - a late packet of the newest epoch's Layer (lateForNewest) never starts an epoch: one with the old profile after
//     a profile change, or one from before a pause, is dropped;
//   - a packet of the current epoch's Layer and profile is mapped by the epoch's offsets. lastS and lastTS only move
//     forward, so reordered packets keep their mapped numbers. A repeat of the newest upstream seq is dropped;
//   - a late packet from before the current epoch's first upstream seq is mapped through an earlier epoch of the same
//     Layer (padding and resume epochs), unless its own seq would collide with a later epoch's, or a different Layer
//     comes first;
//   - everything else is dropped: other layers, other profiles, packets more than maxLateSeq behind, and every packet
//     while paused.
func (m *munger) process(p *packet, now int64) (seq uint16, ts uint32, v verdict) {
	if !m.active {
		return 0, 0, verdictDrop
	}
	var cur *epoch
	if m.forwarding {
		cur = m.newest()
		if cur.layer == p.layer && cur.profile != p.profile && int16(p.seq-m.lastU) > 0 {
			// The current layer moved on to another profile: its epoch is over, and the output restarts with a
			// keyframe. (A late packet with the old profile after the switch is only dropped: lateForNewest.)
			m.forwarding, cur = false, nil
		}
	}
	if p.layer.slot == m.target && (cur == nil || cur.layer != p.layer || cur.profile != p.profile) {
		if m.lateForNewest(p, now) {
			// A late packet from the newest epoch's Layer, from before a profile change or a pause: switching to it
			// would take the output back (to the old profile, which the next packet of the new one then ends).
			return 0, 0, verdictDrop
		}
		if !p.keyStart {
			return 0, 0, verdictWaitKeyframe
		}
		m.startEpoch(p, now)
		return m.lastS, m.lastTS, verdictNewEpoch
	}
	if cur == nil || cur.layer != p.layer || cur.profile != p.profile {
		return 0, 0, verdictDrop
	}

	behind := int16(m.lastU - p.seq)
	switch {
	case behind == 0:
		// A repeat of the newest upstream seq that the cache couldn't catch: media reusing the seq of an in-order
		// padding packet (padding is never cached). Mapping it would send a second packet with the same own seq.
		return 0, 0, verdictDrop
	case behind < 0: // in order, or a jump ahead
		m.lastU = p.seq
		if span := m.lastU - cur.startU; span > maxEpochSpan {
			// Keep the start within int16 reach of the newest packet. Packets and NACKs for the seqs it passes are
			// more than maxLateSeq behind, which nothing serves anyway.
			cur.startU += span - maxEpochSpan
			cur.startS += span - maxEpochSpan
		}
		seq, ts = p.seq+cur.seqOff, p.ts+cur.tsOff
	case int(behind) > maxLateSeq:
		return 0, 0, verdictDrop
	case int16(p.seq-cur.startU) >= 0: // reordered within the current epoch
		seq, ts = p.seq+cur.seqOff, p.ts+cur.tsOff
	default:
		e, ok := m.lateEpoch(p)
		if !ok {
			return 0, 0, verdictDrop
		}
		return p.seq + e.seqOff, p.ts + e.tsOff, verdictForward
	}
	if int16(seq-m.lastS) > 0 {
		m.lastS = seq
	}
	if int32(ts-m.lastTS) > 0 {
		m.lastTS, m.lastAt = ts, now
	}
	return seq, ts, verdictForward
}

// skipPadding handles a padding-only packet (02 §9.4). In order (the upstream seq right after the newest one of the
// current epoch), it closes the gap it would leave: a new epoch of the same Layer and profile with seqOff − 1 and the
// same tsOff, so the next media packet gets the next own seq. A run of padding packets extends that epoch while it
// has mapped nothing yet, so padding bursts don't flood the epoch ring. Out-of-order padding is ignored: the viewer
// may NACK that seq, it misses the cache, and nothing is lost.
func (m *munger) skipPadding(p *packet, now int64) {
	if !m.active || !m.forwarding {
		return
	}
	cur := m.newest()
	if p.layer != cur.layer || p.seq != m.lastU+1 {
		return
	}
	m.lastU = p.seq
	if cur.startS == m.lastS+1 { // a padding epoch that has mapped nothing yet
		cur.seqOff--
		cur.startU = p.seq + 1
		return
	}
	m.push(epoch{
		layer: cur.layer, profile: cur.profile, startS: m.lastS + 1, startU: p.seq + 1,
		seqOff: cur.seqOff - 1, tsOff: cur.tsOff, at: now,
	})
}

// lookup finds the epoch that sent own seq s, for a NACK (02 §9.5): the newest epoch that starts at or before s. The
// upstream seq is s − seqOff. It fails for a seq not sent yet or more than maxLateSeq behind, and it doesn't look past
// an epoch that started more than 2 s ago: every earlier epoch ended before that.
func (m *munger) lookup(s uint16, now int64) (epoch, bool) {
	if !m.started || int16(s-m.lastS) > 0 || int(int16(m.lastS-s)) > maxLateSeq {
		return epoch{}, false
	}
	for i := range min(m.n, epochRing) {
		e := m.nth(i)
		if int16(s-e.startS) >= 0 {
			return *e, true
		}
		if now-e.at > int64(nackLookback) {
			break
		}
	}
	return epoch{}, false
}

// translateSR returns the own RTP timestamp for a sender report of layer (02 §9.6): the report's RTP time shifted by
// the current epoch's offset. It succeeds only while the munger forwards that Layer, because SRs are sent only while
// forwarding and another layer's timeline doesn't match the output. The NTP time passes through unchanged.
func (m *munger) translateSR(layer *Layer, sr *srInfo) (uint32, bool) {
	if !m.active || !m.forwarding || sr == nil {
		return 0, false
	}
	cur := m.newest()
	if cur.layer != layer {
		return 0, false
	}
	return sr.rtp + cur.tsOff, true
}

// startEpoch starts an epoch at packet p and records p as forwarded. The first epoch starts at seq0 and ts0. Later
// ones continue the sequence right after lastS, and the timestamp by nextTS.
func (m *munger) startEpoch(p *packet, now int64) {
	e := epoch{layer: p.layer, profile: p.profile, startU: p.seq, at: now}
	outTS := m.ts0
	e.seqOff = m.seq0 - p.seq
	if m.started {
		e.seqOff = m.lastS + 1 - p.seq
		outTS = m.nextTS(p, now)
	}
	e.tsOff = outTS - p.ts
	e.startS = p.seq + e.seqOff
	if m.n == 0 || m.newest().layer != p.layer {
		m.switches++
	}
	m.push(e)
	m.forwarding, m.started = true, true
	m.lastS, m.lastU, m.lastTS, m.lastAt = e.startS, p.seq, outTS, now
}

// nextTS returns the own timestamp for the first packet of a new (not the first) epoch, in order of preference
// (02 §9.4):
//  1. the same Layer instance as the previous epoch (a resume, audio after off, a profile change): keep its tsOff,
//     because upstream timestamps already advanced in media time;
//  2. SR-aligned: the capture time of p through the new layer's SR, mapped onto the old layer's timeline through the
//     old layer's SR, both less than 10 s old;
//  3. wall clock (the S4 rule): lastTS plus the time since lastTS moved, at least 1 tick.
//
// The first two must move the timestamp forward (the monotonic guard); SR alignment may also not jump further than
// the time since lastTS moved plus 1 s. Otherwise the wall clock decides.
func (m *munger) nextTS(p *packet, now int64) uint32 {
	prev := m.newest()
	elapsed := max(now-m.lastAt, 0)
	if prev.layer == p.layer {
		if ts := p.ts + prev.tsOff; int32(ts-m.lastTS) > 0 {
			return ts
		}
	} else if ts, ok := m.srAligned(prev, p, now, elapsed); ok {
		return ts
	}
	return m.lastTS + m.wallTicks(elapsed)
}

// srAligned maps the capture time of p onto the previous epoch's timeline through both layers' sender reports:
// t = ntp_new + (p.ts − rtp_new)/clock, then rtp_old + (t − ntp_old)·clock + tsOff_old. It fails when either SR is
// missing or more than 10 s old, or when the result isn't in (lastTS, lastTS + (elapsed + 1 s)·clock].
func (m *munger) srAligned(prev *epoch, p *packet, now, elapsed int64) (uint32, bool) {
	old, cur := prev.layer.lastSR.Load(), p.layer.lastSR.Load()
	if old == nil || cur == nil || now-old.arrival > int64(srMaxAge) || now-cur.arrival > int64(srMaxAge) {
		return 0, false
	}
	// The NTP difference in ticks. Publisher clocks are not trusted: an absurd difference fails the range check.
	ticks := math.Round(float64(int64(cur.ntp-old.ntp)) / ntpFracUnit * float64(m.clock))
	if math.Abs(ticks) > math.MaxInt32 {
		return 0, false
	}
	ts := old.rtp + (p.ts - cur.rtp) + uint32(int64(ticks)) + prev.tsOff
	step := int64(int32(ts - m.lastTS))
	if step <= 0 || elapsed > maxWallJumpNs || step > (elapsed+nsPerSecond)*int64(m.clock)/nsPerSecond {
		return 0, false
	}
	return ts, true
}

// wallTicks converts the time since lastTS moved to clock ticks: at least 1, at most maxWallJump.
func (m *munger) wallTicks(elapsed int64) uint32 {
	if elapsed > maxWallJumpNs {
		return maxWallJump
	}
	t := elapsed * int64(m.clock) / nsPerSecond
	return uint32(min(max(t, 1), maxWallJump))
}

// lateEpoch finds the epoch for a late packet of the current Layer from before the current epoch's start: walking
// back through earlier epochs of the same Layer, the first whose start is at or before p.seq, if p's own seq in it
// stays below the next epoch's start (those seqs are already used). A different Layer ends the walk.
func (m *munger) lateEpoch(p *packet) (*epoch, bool) {
	next := m.newest().startS
	for i := 1; i < min(m.n, epochRing); i++ {
		e := m.nth(i)
		if e.layer != p.layer {
			return nil, false
		}
		if int16(p.seq-e.startU) >= 0 {
			return e, int16(p.seq+e.seqOff-next) < 0
		}
		next = e.startS
	}
	return nil, false
}

// lateForNewest reports whether p is a late packet of the newest epoch's Layer: not ahead of lastU, the newest upstream
// seq processed in that epoch. process asks only for a packet that would otherwise start an epoch (or wait for a
// keyframe to start one): one with another profile than the current epoch's (a late packet from before a profile
// change), or one that arrives while nothing is forwarded (from before a profile change that ended forwarding, or
// before a pause). The comparison with lastU is trusted only while the upstream seq can't have wrapped past it: while
// forwarding, lastU follows every packet of the Layer; otherwise at most nackLookback after lastTS last moved. After a
// longer pause the Layer's packets start an epoch as usual, whatever their seq.
func (m *munger) lateForNewest(p *packet, now int64) bool {
	return m.n > 0 && m.newest().layer == p.layer && int16(p.seq-m.lastU) <= 0 &&
		(m.forwarding || now-m.lastAt <= int64(nackLookback))
}

// push adds an epoch to the ring as the newest.
func (m *munger) push(e epoch) {
	m.epochs[m.n%epochRing] = e
	m.n++
}

// newest returns the newest epoch. Callers check that there is one (n > 0).
func (m *munger) newest() *epoch { return m.nth(0) }

// nth returns the i-th newest epoch (0 = newest), for i < min(n, epochRing).
func (m *munger) nth(i int) *epoch { return &m.epochs[(m.n-1-i)%epochRing] }
