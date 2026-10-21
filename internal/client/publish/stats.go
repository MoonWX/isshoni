package publish

import (
	"slices"
	"time"

	"github.com/pion/rtcp"
)

// The windows of TrackStats' live numbers (docs/m2/01-client-core.md §8.7).
const (
	rateWindow    = 2 * time.Second  // Bitrate and FPS
	latencyWindow = 10 * time.Second // CaptureToSend
	// maxSamples bounds a window's memory whatever the packet rate: 10 s of 1 kHz frames.
	maxSamples = 10_000
	// srHistory is how many of its last sender reports a track remembers, to match receiver reports against.
	srHistory = 8
)

// LatencyStats summarize a latency over the last 10 s.
type LatencyStats struct {
	P50, P95, Max time.Duration
	Samples       int // in the window
}

// sample is one event of a window: a frame's size in bytes, or a latency in ns.
type sample struct {
	at time.Time
	v  int64
}

// window holds the samples of the last span. It is not safe for concurrent use: a track guards its windows with its
// mutex.
type window struct {
	span time.Duration
	s    []sample // oldest first
}

func newWindow(span time.Duration) window { return window{span: span} }

// add records v at at, which never goes back.
func (w *window) add(at time.Time, v int64) {
	w.trim(at)
	if len(w.s) == maxSamples {
		w.s = w.s[1:]
	}
	w.s = append(w.s, sample{at, v})
}

// trim forgets the samples that are at least span older than now.
func (w *window) trim(now time.Time) {
	cut := now.Add(-w.span)
	i := 0
	for i < len(w.s) && !w.s[i].at.After(cut) {
		i++
	}
	if i == len(w.s) {
		w.s = w.s[:0] // keeps the array
		return
	}
	w.s = w.s[i:]
}

// sum returns the sum of the samples in the window at now, and their number.
func (w *window) sum(now time.Time) (sum int64, n int) {
	w.trim(now)
	for _, s := range w.s {
		sum += s.v
	}
	return sum, len(w.s)
}

// latency returns the window's samples, as durations, summarized at now.
func (w *window) latency(now time.Time) LatencyStats {
	w.trim(now)
	if len(w.s) == 0 {
		return LatencyStats{}
	}
	ds := make([]time.Duration, len(w.s))
	for i, s := range w.s {
		ds[i] = time.Duration(s.v)
	}
	slices.Sort(ds)
	return LatencyStats{P50: rank(ds, 50), P95: rank(ds, 95), Max: ds[len(ds)-1], Samples: len(ds)}
}

// rank returns the nearest-rank p-th percentile of sorted, non-empty ds.
func rank(ds []time.Duration, p int) time.Duration {
	i := (p*len(ds)+99)/100 - 1 // ceil(p/100 × n) − 1
	return ds[max(i, 0)]
}

// rates returns a track's bit rate and frame rate over the last rateWindow at now (t.mu held).
func (t *track) rates(now time.Time) (bps int, fps float64) {
	bytes, frames := t.rate.sum(now)
	return int(float64(bytes*8) / rateWindow.Seconds()), float64(frames) / rateWindow.Seconds()
}

// srSent is one sender report a track wrote: the middle 32 bits of its NTP time, which a receiver report quotes as
// its LSR, and when it was written.
type srSent struct {
	lsr uint32
	at  time.Time
}

// noteSRLocked remembers a sender report the track wrote at at. t.mu is held.
func (t *track) noteSRLocked(ntp uint64, at time.Time) {
	if len(t.srs) == srHistory {
		t.srs = append(t.srs[:0], t.srs[1:]...)
	}
	t.srs = append(t.srs, srSent{lsr: uint32(ntp >> 16), at: at})
}

// onReceptionReport takes the RTT from a reception report about the track's stream that arrived at at (RFC 3550
// §6.4.1): at − the time the quoted SR was written − DLSR. A Publisher's SRs carry the capture time of their frame, not
// the time they are written (§8.2), so the LSR is looked up among the SRs the track wrote rather than read as a time.
// Two SRs carry the same NTP time when no frame went out between them (a static screen): the newest one that gives a
// non-negative RTT is the one quoted, since SRs are a second apart and an RTT is far shorter. A report without an LSR,
// or quoting an SR the track no longer remembers, changes nothing. t.mu is held.
func (t *track) onReceptionReport(r rtcp.ReceptionReport, at time.Time) {
	if r.LastSenderReport == 0 {
		return
	}
	dlsr := time.Duration(uint64(r.Delay) * uint64(time.Second) >> 16)
	for i := len(t.srs) - 1; i >= 0; i-- {
		sr := t.srs[i]
		if sr.lsr != r.LastSenderReport {
			continue
		}
		if rtt := at.Sub(sr.at) - dlsr; rtt >= 0 {
			t.stats.RTT = rtt
			return
		}
	}
}
