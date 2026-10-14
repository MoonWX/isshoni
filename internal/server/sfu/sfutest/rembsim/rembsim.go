// Package rembsim is a small copy of the receive-side bandwidth estimator that Chrome runs on a sub PC with
// abs-send-time and no transport-cc (libwebrtc's AIMD rate control), driven by a scripted link: what the SFU forwarded
// towards the viewer, and how much of it arrived. The SFU's tests take their REMBs from it (docs/m1/02-sfu.md §17):
// allocator_test in fake time, and sfutest.Viewer on a real link (sfutest.Link). It imports neither the SFU nor Pion.
//
// The rules (02 §17), which are what makes REMB only an overuse signal (02 §10.2):
//   - The received rate is what arrived over the last 0.5 s: on a link of some capacity, min(forwarded, capacity).
//   - The estimate starts at the received rate, once 0.5 s of it has arrived.
//   - Overuse, the link losing what was forwarded above its capacity, sets the estimate to 0.85 × the received rate
//     when that is lower, at most once per 200 ms. The estimate only ever falls on overuse.
//   - Otherwise it grows by 8 % a second, but never above max(itself, 1.5 × the received rate + 10 kbit/s): a viewer
//     that receives little can't report much, whatever its link would carry.
//   - A REMB goes out with the first estimate, then every 1 s, and at once when the estimate has fallen by 3 % or more
//     since the last one.
//
// Overuse is judged on each step by itself: a step in which the link lost more than 1 % of what was forwarded.
// Chrome sees overuse in the delay of the packets that queue at the bottleneck, which stops growing as soon as the
// sender has slowed down; the model's equivalent is the link dropping now, not the average of the last 0.5 s.
package rembsim

import (
	"math"
	"time"
)

// The model's numbers (02 §17).
const (
	// Window is the time the received rate is measured over.
	Window = 500 * time.Millisecond
	// Beta: on overuse the estimate falls to Beta × the received rate, at most once per CutEvery.
	Beta     = 0.85
	CutEvery = 200 * time.Millisecond
	// Growth is how much the estimate grows a second without overuse, never above Headroom × the received rate +
	// HeadroomBps (unless it is above that already).
	Growth      = 1.08
	Headroom    = 1.5
	HeadroomBps = 10_000
	// A REMB goes out every Interval, and at once on a fall of FallPct % or more since the last one.
	Interval = time.Second
	FallPct  = 3
	// overuseLoss is the share of a step's forwarded bits the link must lose for the step to be an overuse.
	overuseLoss = 0.01
)

// Model is the estimator of one viewer. Its zero value is ready to use; it is for one goroutine.
type Model struct {
	started bool
	first   time.Duration // the first Step
	last    time.Duration // the last Step
	steps   []step        // the steps of the last Window

	estimating bool
	est        float64 // bit/s
	cut        bool
	cutAt      time.Duration // the last time the estimate fell

	sentAny bool
	sent    float64 // the last REMB
	sentAt  time.Duration
}

type step struct {
	at                  time.Duration
	forwarded, received float64 // bits
}

// Step advances the model to now, a time since an origin of the caller's, with the bits forwarded towards the viewer
// since the last Step and the bits of them that arrived. It returns the estimate in bit/s (0 before it starts), and
// whether a REMB with it goes out now. Steps must come in order, at most a few hundred milliseconds apart.
func (m *Model) Step(now time.Duration, forwarded, received float64) (bps float64, send bool) {
	if !m.started {
		m.started, m.first, m.last = true, now, now
	}
	dt := max(now-m.last, 0)
	m.last = now
	m.steps = append(m.steps, step{at: now, forwarded: forwarded, received: received})
	old := 0
	for old < len(m.steps) && now-m.steps[old].at >= Window {
		old++
	}
	m.steps = m.steps[old:]
	rate := m.Received()

	switch {
	case !m.estimating:
		if now-m.first < Window {
			return 0, false
		}
		m.estimating, m.est = true, rate
	case forwarded > 0 && received < forwarded*(1-overuseLoss):
		if !m.cut || now-m.cutAt >= CutEvery {
			if target := Beta * rate; target < m.est {
				m.est, m.cut, m.cutAt = target, true, now
			}
		}
	default:
		grown := m.est * math.Pow(Growth, dt.Seconds())
		m.est = min(grown, max(m.est, Headroom*rate+HeadroomBps))
	}

	if !m.sentAny || m.est <= m.sent*(1-FallPct/100.0) || now-m.sentAt >= Interval {
		m.sentAny, m.sent, m.sentAt = true, m.est, now
		return m.est, true
	}
	return m.est, false
}

// Received returns the received rate of the last Window, in bit/s.
func (m *Model) Received() float64 {
	var bits float64
	for _, s := range m.steps {
		bits += s.received
	}
	return bits / Window.Seconds()
}

// Estimate returns the estimate in bit/s, 0 before it starts.
func (m *Model) Estimate() float64 { return m.est }
