package main

import (
	"math/rand/v2"
	"time"
)

// munger decides which simulcast layer a DownTrack forwards and rewrites RTP
// sequence numbers and timestamps so the subscriber sees one continuous stream
// across layer switches and pauses. It is not safe for concurrent use.
type munger struct {
	clockRate uint32

	active bool   // false = paused ("off"); nothing is forwarded
	target string // rid the subscriber wants

	current    string // rid currently forwarded
	hasCurrent bool
	switchSeq  uint16 // first upstream seq of current layer since the switch

	started bool // at least one packet has been forwarded
	lastSeq uint16
	lastTS  uint32
	lastAt  time.Time
	seqOff  uint16
	tsOff   uint32

	switches int
}

// setTarget changes the desired layer. It returns true if anything changed.
func (m *munger) setTarget(rid string, active bool) bool {
	changed := m.active != active || m.target != rid
	m.active, m.target = active, rid
	if !active {
		m.hasCurrent = false
	}
	return changed
}

// waitingForKeyframe reports whether the munger needs a keyframe on the target
// layer before it can forward (again).
func (m *munger) waitingForKeyframe() bool {
	return m.active && (!m.hasCurrent || m.current != m.target)
}

// process decides whether an upstream packet from layer rid is forwarded and,
// if so, returns its rewritten sequence number and timestamp. keyframe must be
// true only for the first packet of a decodable keyframe (always true for audio).
func (m *munger) process(rid string, seq uint16, ts uint32, keyframe bool, now time.Time) (uint16, uint32, bool) {
	if !m.active {
		return 0, 0, false
	}
	if rid == m.target && (!m.hasCurrent || m.current != rid) {
		if !keyframe {
			return 0, 0, false // keep forwarding the old layer until the new one has a keyframe
		}
		m.switchTo(rid, seq, ts, now)
	}
	if !m.hasCurrent || rid != m.current {
		return 0, 0, false
	}
	if int16(seq-m.switchSeq) < 0 {
		// A reordered packet from before the switch point would collide with
		// sequence numbers already used by the previous layer.
		return 0, 0, false
	}
	outSeq, outTS := seq+m.seqOff, ts+m.tsOff
	if !m.started || int16(outSeq-m.lastSeq) > 0 {
		m.lastSeq = outSeq
	}
	if !m.started || int32(outTS-m.lastTS) > 0 {
		m.lastTS, m.lastAt = outTS, now
	}
	m.started = true
	return outSeq, outTS, true
}

func (m *munger) switchTo(rid string, seq uint16, ts uint32, now time.Time) {
	if m.started {
		// Continue right after the last forwarded packet; advance the timestamp
		// by the wall-clock time that passed so playout timing stays sane.
		m.seqOff = m.lastSeq + 1 - seq
		elapsed := uint32(now.Sub(m.lastAt).Seconds() * float64(m.clockRate))
		if elapsed == 0 {
			elapsed = 1
		}
		m.tsOff = m.lastTS + elapsed - ts
	} else {
		m.seqOff = uint16(rand.Uint32()) - seq
		m.tsOff = rand.Uint32() - ts
	}
	m.current, m.hasCurrent, m.switchSeq = rid, true, seq
	m.switches++
}
