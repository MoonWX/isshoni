package ratectl

import (
	"math"
	"time"
)

// The upload and CPU hints (01 §9.5, D11). Resolution and frame rate stay fixed per share in M2, so when the
// server's cap binds below what the target size needs, the sharer is told: "Your upload allows about 720p" (03).
//
//   - A height h at the full layer's frame rate needs req(h) = BitsPerPixel × (h × aspect) × h × fps bit/s (1080p60:
//     6.2 Mbit/s; 720p60: 2.8; 540p60: 1.6; 360p60: 0.7; half of each at 30 fps).
//   - approx is the largest of 360, 480, 540, 720, 900 and 1080 whose req fits the applied full layer (360 if none).
//   - The condition: the full layer is active, the cap binds (the cap's split for f is below f's ceiling, and the
//     ramp is not below it: the ramp alone never shows the hint), and approx is below UploadBelow % of TargetHeight.
//     Tick samples it. The hint turns on once it held on every Tick for UploadOn (the web sharer's 3 samples of 2 s,
//     M1 05 §13.7), and off UploadOff after the last Tick it held. While on, approx follows the applied full layer.

// uploadHeights are the heights an upload hint names, ascending.
var uploadHeights = [...]int{360, 480, 540, 720, 900, 1080}

// uploadState is the upload hint's state between Ticks.
type uploadState struct {
	held   bool      // the last Tick saw the condition
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
	approx, ok := c.uploadCondition()
	if !ok {
		u.held = false
		if u.on && now.Sub(u.last) >= UploadOff {
			u.on = false
		}
		return
	}
	if !u.held {
		u.held, u.since = true, now
	}
	u.last = now
	u.approx = approx
	if now.Sub(u.since) >= UploadOn {
		u.on = true
	}
}

// uploadCondition returns approx and whether the upload condition holds now.
func (c *Controller) uploadCondition() (int, bool) {
	f := c.fullLayer()
	if f == nil || !f.on || c.targetHeight <= 0 {
		return 0, false
	}
	capF, capped := c.capSplitFull()
	if !capped || capF >= f.ceiling || capF > c.ramp {
		return 0, false
	}
	fps := f.fps
	if fps <= 0 {
		fps = defaultFPS
	}
	approx := approxHeight(f.applied, fps, c.aspect)
	return approx, int64(approx)*100 < int64(c.targetHeight)*UploadBelow
}

// approxHeight returns the largest upload height whose required bitrate at fps and aspect fits bps, 360 if none.
func approxHeight(bps, fps int, aspect float64) int {
	best := uploadHeights[0]
	for _, h := range uploadHeights {
		if required(h, fps, aspect) <= bps {
			best = h
		}
	}
	return best
}

// required returns req(h): the bit/s that height h needs at fps, for a source of aspect width/height.
func required(h, fps int, aspect float64) int {
	w := math.Round(float64(h) * aspect)
	return int(math.Round(BitsPerPixel * w * float64(h) * float64(fps)))
}
