package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// capturer grabs screenshots of the primary monitor (capture_windows.go: GDI).
type capturer interface {
	// grab captures the monitor. The frame is valid until the next grab or close.
	grab() (*Frame, error)
	// refreshHz is the monitor's refresh rate, 0 when unknown.
	refreshHz() int
	close() error
}

// openCapturer opens the screen capture; the tests replace it.
var openCapturer = openGDI

// The measurement's timing (01 §12.2): 20 screenshots a second, after a 2 s calibration.
const (
	captureInterval = 50 * time.Millisecond
	calibrationTime = 2 * time.Second
)

// Plausibility checks: a reading that passes decode can still be wrong (a cell flipped by video compression), and
// one wrong high bit moves the value by seconds. Such screenshots count as decode failures ("implausible"), not as
// samples.
const (
	clockTolMs   = 150   // the clock's value follows the capture time within this (rendering and capture delays)
	maxDeltaMs   = 30000 // a larger Δ is a copy ahead of the clock (impossible) or no measurement
	outlierMs    = 3000  // Δ is within this of the median of the last recentDeltas accepted ones
	recentDeltas = 21
	recoverAfter = 20 // this many implausible readings in a row that agree with each other are the new normal
	minSeenShare = 0.3
	clockScaleTo = 0.03 // the clock's strip is at scale 1 within this
	copyScaleTol = 0.15 // a copy that moved keeps its scale within this
	maxDumps     = 20
)

// Why a screenshot was not decoded (report.go counts them).
const (
	failCapture     = "capture"     // the screenshot could not be taken
	failClock       = "clock"       // the clock's own strip could not be read
	failCopy        = "copy"        // the copy could not be read (covered, moved away, or blurred)
	failImplausible = "implausible" // the readings decoded but disagree with the time or the other samples
)

// point is one screenshot of the measurement.
type point struct {
	TMs        int64  `json:"tMs"`                  // when it was taken, from the measurement's start
	DeltaMs    *int   `json:"deltaMs,omitempty"`    // v_clock − v_copy mod 65536: the latency; absent when not decoded
	Fail       string `json:"fail,omitempty"`       // why it was not decoded
	Transition bool   `json:"transition,omitempty"` // a strip had cells between two frames (resolve)
}

// meter calibrates and then tracks the clock's strip and its copy.
type meter struct {
	cap      capturer
	interval time.Duration
	base     time.Time // the capture times' origin
	warn     func(format string, a ...any)
	dump     *dumper

	screen      image.Point
	clock, copy strip
	anchor      int   // the clock's value minus the capture time in ms since base, mod 65536
	recent      []int // the last accepted Δ
	moves       int   // times the copy was found again somewhere else (more than half a cell away)

	clockMiss, deltaMiss  int // implausible readings in a row
	lastMissOff, lastMiss int
}

func newMeter(c capturer, warn func(string, ...any), d *dumper) *meter {
	return &meter{cap: c, interval: captureInterval, base: time.Now(), warn: warn, dump: d}
}

// grab takes a screenshot and returns it with its capture time in ms since m.base (the middle of the call).
func (m *meter) grab() (*Frame, int, error) {
	t0 := time.Now()
	f, err := m.cap.grab()
	t1 := time.Now()
	if err != nil {
		return nil, 0, err
	}
	m.screen = image.Pt(f.W, f.H)
	return f, int(t0.Add(t1.Sub(t0) / 2).Sub(m.base).Milliseconds()), nil
}

// seen is a strip found at the same place in several calibration screenshots.
type seen struct {
	s      strip
	n      int
	values []int // the clock: value − capture time; a copy: Δ
}

func addSeen(list []*seen, s strip, v int, ok bool) []*seen {
	for _, e := range list {
		if e.s.overlaps(s) {
			e.s, e.n = s, e.n+1
			if ok {
				e.values = append(e.values, v)
			}
			return list
		}
	}
	e := &seen{s: s, n: 1}
	if ok {
		e.values = append(e.values, v)
	}
	return append(list, e)
}

// errNotFound is a calibration that did not find the clock or a copy; its message says what to check.
var errNotFound = errors.New("calibration failed")

// calibrate looks for every strip in the screenshots of d (01 §12.2): the clock's own strip, at scale 1 and
// normally at its known position, and the largest copy at scales 0.25–1.0 that is in at least 30 % of them.
func (m *meter) calibrate(ctx context.Context, d time.Duration) error {
	tick := time.NewTicker(m.interval)
	defer tick.Stop()
	end := time.Now().Add(d)
	var clocks, copies []*seen
	frames := 0
	var lastFrame *Frame
	for {
		if f, tms, err := m.grab(); err == nil {
			frames++
			lastFrame = f
			all := find(f, f.bounds())
			var clk *found
			for i := range all {
				c := &all[i]
				if math.Abs(c.scale()-1) > clockScaleTo {
					continue
				}
				if clk == nil || dist(c.strip, clockStrip()) < dist(clk.strip, clockStrip()) {
					clk = c
				}
			}
			if clk != nil {
				clocks = addSeen(clocks, clk.strip, wrap16(int(clk.Value)-tms), true)
			}
			for _, c := range all {
				if clk != nil && (c.strip == clk.strip || c.rect().Overlaps(clk.rect())) {
					continue
				}
				delta := 0
				if clk != nil {
					delta = wrap16(int(clk.Value) - int(c.Value))
				}
				copies = addSeen(copies, c.strip, delta, clk != nil && delta <= maxDeltaMs)
			}
		} else {
			m.warn("a screenshot failed: %v", err)
		}
		if !time.Now().Before(end) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
	if frames == 0 {
		return fmt.Errorf("%w: no screenshot could be taken", errNotFound)
	}
	slices.SortStableFunc(clocks, func(a, b *seen) int { return b.n - a.n })
	if len(clocks) == 0 {
		return fmt.Errorf("%w: the clock's strip is not on the screen: open the clock page (g2g serve) on the "+
			"primary monitor, full screen (F11), with nothing over its top-left corner", errNotFound)
	}
	m.clock = clocks[0].s
	m.anchor = circularMedian(clocks[0].values)
	if dist(m.clock, clockStrip()) > 4 {
		m.warn("the clock's strip is at (%.0f, %.0f), not at the top-left corner: is the page full screen (F11)?",
			m.clock.X0, m.clock.Y0)
	}
	var best *seen
	var outside []string
	for _, c := range copies {
		sc := c.s.scale()
		if float64(c.n) < max(1, minSeenShare*float64(frames)) {
			continue
		}
		if sc < minCopyScale-0.01 || sc > maxCopyScale+clockScaleTo {
			outside = append(outside, fmt.Sprintf("%.2f", sc))
			continue
		}
		if best == nil || sc > best.s.scale() {
			best = c
		}
	}
	if best == nil {
		msg := "no copy of the clock's strip at scale 0.25–1.0: is the viewer's video on this monitor, with the " +
			"strip visible? If it is, start Chrome with --disable-direct-composition-video-overlays (a video on a " +
			"hardware overlay is not in the screenshot)"
		if len(outside) > 0 {
			msg += "; there are copies at scale " + strings.Join(outside, ", ") + ": make the viewer larger"
		}
		return fmt.Errorf("%w: %s", errNotFound, msg)
	}
	m.copy = best.s
	for _, v := range best.values {
		m.push(v)
	}
	if m.dump != nil && lastFrame != nil {
		m.dump.save("calibration-clock", lastFrame, m.clock)
		m.dump.save("calibration-copy", lastFrame, m.copy)
	}
	return nil
}

// measure takes a screenshot every interval for d and decodes each. interrupted is true when ctx ended first.
func (m *meter) measure(ctx context.Context, d time.Duration) (pts []point, interrupted bool) {
	tick := time.NewTicker(m.interval)
	defer tick.Stop()
	start := time.Now()
	for {
		p := point{TMs: time.Since(start).Milliseconds()}
		f, tms, err := m.grab()
		if err != nil {
			p.Fail = failCapture
			m.warn("a screenshot failed: %v", err)
		} else {
			m.sample(f, tms, &p)
			if p.Fail != "" && p.Fail != failImplausible && m.dump != nil && m.dump.n < maxDumps {
				name := fmt.Sprintf("fail-%06d", p.TMs)
				m.dump.save(name+"-clock", f, m.clock)
				m.dump.save(name+"-copy", f, m.copy)
			}
		}
		pts = append(pts, p)
		select {
		case <-ctx.Done():
			return pts, true
		case <-tick.C:
		}
		if time.Since(start) >= d {
			return pts, false
		}
	}
}

// sample decodes one screenshot into p.
func (m *meter) sample(f *Frame, tms int, p *point) {
	rc, err := m.read(f, &m.clock, clockScaleTo)
	if err != nil {
		p.Fail = failClock
		return
	}
	if off := wrapDiff(int(rc.Value) - tms - m.anchor); abs(off) > clockTolMs {
		// The clock disagrees with the time. Many such readings that agree with each other mean the clock
		// really jumped (the page was reloaded), and it is followed from then on.
		if abs(wrapDiff(off-m.lastMissOff)) <= clockTolMs {
			m.clockMiss++
		} else {
			m.clockMiss = 1
		}
		m.lastMissOff = off
		if m.clockMiss >= recoverAfter {
			m.anchor = wrap16(m.anchor + off)
			m.clockMiss = 0
			m.warn("the clock jumped by %d ms (was the page reloaded?); following it", off)
		}
		p.Fail = failImplausible
		return
	}
	m.clockMiss = 0
	was := m.copy
	rp, err := m.read(f, &m.copy, copyScaleTol)
	if err != nil {
		p.Fail = failCopy
		return
	}
	if !was.near(m.copy) { // not the same strip with other sub-pixel ends (a decode that failed in place)
		m.moves++
	}
	delta := wrap16(int(rc.Value) - int(rp.Value))
	if delta > maxDeltaMs {
		p.Fail = failImplausible
		return
	}
	if len(m.recent) >= 5 && abs(delta-median(m.recent)) > outlierMs {
		if abs(delta-m.lastMiss) <= outlierMs {
			m.deltaMiss++
		} else {
			m.deltaMiss = 1
		}
		m.lastMiss = delta
		if m.deltaMiss >= recoverAfter {
			m.recent, m.deltaMiss = nil, 0
			m.warn("the latency moved to about %d ms and stayed there; following it", delta)
		}
		p.Fail = failImplausible
		return
	}
	m.deltaMiss = 0
	m.push(delta)
	p.DeltaMs = &delta
	p.Transition = rc.Ambiguous > 0 || rp.Ambiguous > 0
}

// push adds an accepted Δ to the recent window.
func (m *meter) push(delta int) {
	m.recent = append(m.recent, delta)
	if len(m.recent) > recentDeltas {
		m.recent = m.recent[1:]
	}
}

// read decodes the strip at *s. When that fails, it looks for the strip around where it was (the viewer window
// moved a little) at the same scale within tol, and follows it there.
func (m *meter) read(f *Frame, s *strip, tol float64) (reading, error) {
	r, err := decode(f, *s)
	if err == nil {
		return r, nil
	}
	w, h := s.X1-s.X0, s.Y1-s.Y0
	roi := image.Rect(int(s.X0-w/2), int(s.Y0-3*h), int(s.X1+w/2), int(s.Y1+3*h))
	for _, c := range find(f, roi) {
		if math.Abs(c.scale()/s.scale()-1) <= tol {
			*s = c.strip
			return c.reading, nil
		}
	}
	return reading{}, err
}

// near reports whether o is s found again in the same place: each of its edges within half a cell of s's.
func (s strip) near(o strip) bool {
	tol := s.cell() / 2
	return math.Abs(o.X0-s.X0) <= tol && math.Abs(o.X1-s.X1) <= tol && math.Abs(o.Y0-s.Y0) <= tol &&
		math.Abs(o.Y1-s.Y1) <= tol
}

// dist is how far apart two strips' top-left corners are, in pixels.
func dist(a, b strip) float64 { return math.Hypot(a.X0-b.X0, a.Y0-b.Y0) }

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// wrap16 is x mod 65536 in [0, 65535].
func wrap16(x int) int { return x & 0xffff }

// median of a non-empty slice (the upper one of an even count).
func median(v []int) int {
	s := slices.Clone(v)
	slices.Sort(s)
	return s[len(s)/2]
}

// circularMedian is the median of clock values mod 65536 that lie close together, taken around the first one so
// that a wrap between them doesn't matter; 0 for none.
func circularMedian(v []int) int {
	if len(v) == 0 {
		return 0
	}
	d := make([]int, len(v))
	for i, x := range v {
		d[i] = wrapDiff(x - v[0])
	}
	return wrap16(v[0] + median(d))
}

// dumper saves crops of screenshots as PNG files (measure -dump): only where a strip is (or was), the strip with
// its grey frame, so nothing else of the desktop is in the file unless something covers the strip.
type dumper struct {
	dir  string
	n    int
	warn func(format string, a ...any)
}

func (d *dumper) save(name string, f *Frame, s strip) {
	d.n++
	r := s.rect().Intersect(f.bounds())
	if r.Empty() {
		return
	}
	img := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	for y := range r.Dy() {
		for x := range r.Dx() {
			i := (r.Min.Y+y)*f.Stride + 4*(r.Min.X+x)
			j := img.PixOffset(x, y)
			img.Pix[j], img.Pix[j+1], img.Pix[j+2], img.Pix[j+3] = f.Pix[i+2], f.Pix[i+1], f.Pix[i], 0xff
		}
	}
	path := filepath.Join(d.dir, name+".png")
	out, err := os.Create(path) //nolint:gosec // G304: a file in the directory the operator named with -dump
	if err == nil {
		err = png.Encode(out, img)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		d.warn("-dump: %v", err)
	}
}
