package ratectl

import (
	"math"
	"time"
)

// The upload and CPU hints (01 §9.5, D11). Resolution and frame rate stay fixed per share in M2, so when the
// server's cap binds below what the target size needs, the sharer is told: "Your upload allows about 720p" (03).
//
//   - A height h at the full layer's frame rate needs req(h) = BitsPerPixel × (h × aspect) × h × fps bit/s (1080p60:
//     6.2 Mbit/s; 720p60: 2.8; 540p60: 1.6; 360p60: 0.7; half of each at 30 fps). The frame rate is the encodings'
//     MaxFramerate within 04 §6.4's range (1–240; defaultFPS when none is given), the aspect Config.SourceAspect
//     within [minAspect, maxAspect] (16/9 otherwise), and req is compared as a float, so no server value can
//     overflow the arithmetic.
//   - approx is the largest of 360, 480, 540, 720, 900 and 1080 whose req fits the applied full layer (360 if none).
//   - The condition: the full layer is active, the cap binds (the cap's split for f is below f's ceiling), and approx
//     is below UploadBelow % of TargetHeight. Tick samples it. The hint turns on once it held on every Tick for
//     UploadOn (the web sharer's 3 samples of 2 s, M1 05 §13.7), and off UploadOff after the last Tick it held. While
//     on, approx follows the applied full layer.
//   - The ramp alone never shows it: while the ramp is below the cap's split (it climbs toward the cap, or a backoff
//     holds it), a Tick that sees the condition is neutral. It keeps a run that is going on (the cap still binds:
//     a cap raised during a backoff's hold doesn't restart the 6 s), but starts none.

// uploadHeights are the heights an upload hint names, ascending.
var uploadHeights = [...]int{360, 480, 540, 720, 900, 1080}

// The source aspects the upload hint accepts; Config.SourceAspect outside them counts as unknown (16/9).
const (
	minAspect = 0.25
	maxAspect = 8
)

// maxFPS is the top of 04 §6.4's frame rate range; the encodings' MaxFramerate is bounded to it.
const maxFPS = 240

// uploadState is the upload hint's state between Ticks.
type uploadState struct {
	held   bool      // the last Tick saw the condition (neutral Ticks keep it)
	since  time.Time // the first Tick of the current run of the condition
	last   time.Time // the last Tick that saw it
	on     bool
	approx int // approx at the last Tick that saw the condition
}

// Upload returns the upload hint as of now. It changes only at Ticks, except that it also reads as off once
// UploadOff passed since the condition last held.
func (c *Controller) Upload(now time.Time) UploadHint {
	u := &c.up
	if !u.on || (!u.held && now.Sub(u.last) >= UploadOff) {
		return UploadHint{}
	}
	return UploadHint{Limited: true, ApproxHeight: u.approx}
}

// CPU reports the CPU hint: whether the last SetLimit was "cpu" or "gpu". 04 already applies the hysteresis
// (limited after 3 s below 90 % of the target rate, clear after 10 s above 95 %), so the controller adds none and
// the answer doesn't depend on now. The share snapshot's Overloaded shows it, with the web's CPU hint text.
func (c *Controller) CPU(now time.Time) bool {
	return c.limited()
}

// sampleUpload takes the upload condition at a Tick.
func (c *Controller) sampleUpload(now time.Time) {
	u := &c.up
	// Ticks pause while the share isn't live (a reconnect): a hint whose condition ended UploadOff ago is off before
	// this Tick's sample, so a new run has to last UploadOn again.
	if u.on && !u.held && now.Sub(u.last) >= UploadOff {
		u.on = false
	}
	approx, ok, counts := c.uploadCondition()
	switch {
	case !ok:
		u.held = false
		return
	case !counts:
		// Neutral: a run that is going on continues, none starts.
		if !u.held {
			return
		}
	case !u.held:
		u.held, u.since = true, now
	}
	u.last = now
	u.approx = approx
	if now.Sub(u.since) >= UploadOn {
		u.on = true
	}
}

// uploadCondition returns approx, whether the upload condition holds now, and whether this Tick counts toward a run
// (false while the ramp is below the cap's split: neutral).
func (c *Controller) uploadCondition() (approx int, holds, counts bool) {
	f := c.fullLayer()
	if f == nil || !f.on || c.targetHeight <= 0 {
		return 0, false, true
	}
	capF, capped := c.capSplitFull()
	if !capped || capF >= f.ceiling {
		return 0, false, true
	}
	approx = approxHeight(f.applied, f.fps, c.aspect)
	if int64(approx)*100 >= int64(c.targetHeight)*UploadBelow {
		return approx, false, true
	}
	return approx, true, capF <= c.ramp
}

// approxHeight returns the largest upload height whose required bitrate at fps and aspect fits bps, 360 if none.
// fps is bounded to 04 §6.4's range, defaultFPS when it is 0 or less.
func approxHeight(bps, fps int, aspect float64) int {
	if fps <= 0 {
		fps = defaultFPS
	}
	fps = min(fps, maxFPS)
	best := uploadHeights[0]
	for _, h := range uploadHeights {
		if required(h, fps, aspect) <= float64(bps) {
			best = h
		}
	}
	return best
}

// required returns req(h): the bit/s that height h needs at fps, for a source of aspect width/height, rounded to a
// whole bit/s. It stays a float, so an absurd aspect gives a value beyond any bitrate, or +Inf (nothing fits), rather
// than an out-of-range conversion to int.
func required(h, fps int, aspect float64) float64 {
	w := math.Round(float64(h) * aspect)
	return math.Round(BitsPerPixel * w * float64(h) * float64(fps))
}

// sourceAspect returns Config.SourceAspect when it is a plausible width/height, else 16/9.
func sourceAspect(a float64) float64 {
	if !(a >= minAspect && a <= maxAspect) {
		return 16.0 / 9
	}
	return a
}
