// Package ratectl is the native sharer's uplink rate control (docs/m2/01-client-core.md §9). The server estimates
// and the client applies (01 D9): the SFU's native estimator sends quality.hint{maxBitrate} with its own hysteresis,
// and a Controller turns those hints, the share's ShareParams and the engine's reports into per-layer bitrate
// targets for media.Session, within 04 §6.4's engine ranges. It also ramps the full layer from DefaultStartBps up to
// its ceiling while no cap binds (the plan's Auto preset: "starts at ~4 Mbps and ramps to 8 Mbps"), spaces changes
// as the plan says (at least 15 % and 2 s apart for an encoder that changes bitrate live, 30 % and 10 s for one that
// re-initializes), and says when the sharer's upload limits the picture (UploadHint, 01 D11) or the encoder can't
// keep up (the CPU hint).
//
// A Controller is pure logic owned by one goroutine, the share's actor (01 §11.4): no Pion, no I/O, no goroutines and
// no clock. Time comes in as arguments, so tests drive it with fixed instants. The caller applies the Actions it
// returns, in order: OpBitrate with Session.SetBitrate; OpActive with Publisher.SetActive and
// Session.SetLayerActive, plus Session.SetBitrate(Layer, Bps) in the same step when it resumes a layer;
// OpAudioBitrate with Session.SetBitrate(media.LayerAudio, Bps) (01 §11.8).
//
// The files: ratectl.go holds the API, the guards and the ramp; split.go the ceilings, the cap's split and the
// engine ranges (§9.2); upload.go the upload and CPU hints (§9.5).
package ratectl

import (
	"fmt"
	"math"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// The controller's constants (01 §9.6). They are tuned only with itest D9, D10 and D19 passing (01 §10.1, WA54).
const (
	// DefaultStartBps is where the full layer starts when Config.StartBps is 0, for every preset in M2.
	DefaultStartBps = 4_000_000

	// RampFactor is the ramp's growth per step in percent of the ramp (×1.15): 4 → 8 Mbit/s in 5 steps, 10 s. A
	// step rounds up, so every step is at least MinChange and passes the live guard.
	RampFactor = 115
	// RampEvery is the time between two ramp steps.
	RampEvery = 2 * time.Second
	// RampDelay is the time from Live to the ramp's first step.
	RampDelay = 2 * time.Second
	// OneStepDelay is when the full layer of an encoder without live bitrate ramps, in its one step, after Live.
	OneStepDelay = 10 * time.Second
	// HoldAfterCongestion holds the ramp after a hint that lowered the full layer.
	HoldAfterCongestion = 10 * time.Second
	// HoldAfterLimit holds the ramp after the encoder's limit (SetLimit cpu or gpu) cleared.
	HoldAfterLimit = 10 * time.Second

	// MinChange is the smallest change, in percent of the applied value, of a layer whose encoder changes bitrate
	// live (the plan's "changes of at least 15 %").
	MinChange = 15
	// MinInterval is the shortest time between two changes of such a layer ("at most every 2 s").
	MinInterval = 2 * time.Second
	// ReinitMinChange is the smallest change, in percent of the applied value, of a layer whose encoder re-initializes
	// (and sends an IDR) for a new bitrate.
	ReinitMinChange = 30
	// ReinitMinInterval is the shortest time between two changes of such a layer.
	ReinitMinInterval = 10 * time.Second

	// FloorF is the full layer's smallest share of a split.
	FloorF = 250_000
	// FloorQ is the smallest share of a split for every other layer (the preview).
	FloorQ = 150_000
	// QShare is the preview's part of a split's total, before its ceiling and FloorQ.
	QShare = 0.3

	// The engine's ranges (04 §6.4), to which every output is clamped: the full layer, every other video layer, Opus.
	MinBitrateF     = 100_000
	MaxBitrateF     = 50_000_000
	MinBitrateLayer = 50_000
	MaxBitrateLayer = 2_000_000
	MinBitrateAudio = 32_000
	MaxBitrateAudio = 510_000

	// TickEvery is how often the share's actor calls Tick while the share is live.
	TickEvery = 500 * time.Millisecond

	// UploadOn is how long the upload condition must hold, on every Tick, before UploadHint turns on: the web
	// sharer's 3 samples of 2 s (M1 05 §13.7).
	UploadOn = 6 * time.Second
	// UploadOff is how long after the condition last held UploadHint turns off.
	UploadOff = 10 * time.Second
	// UploadBelow is "below the target size": an approximate height under this percentage of TargetHeight (encoders
	// round sizes a little).
	UploadBelow = 95
	// BitsPerPixel is the bit/s per pixel per frame a height needs (§9.5): 1080p60 needs 6.2 Mbit/s.
	BitsPerPixel = 0.05

	// defaultFPS is the full layer's frame rate for the upload hint when the encodings carry none.
	defaultFPS = 60
	// maxCap bounds QualityHint.MaxBitrate, far above the sum of the engine's ranges, so the arithmetic can't
	// overflow on any platform.
	maxCap = 1_000_000_000
)

// Config is what a Controller starts from.
type Config struct {
	// Live says, per layer (rid), that its encoder changes bitrate live: each layer's first EventLayer.LiveBitrate
	// (01 D10). A layer that is missing counts as one that re-initializes, the cautious choice; SetLive updates it.
	Live map[string]bool
	// Params is the share.start reply. Its Encodings fix the layers the Controller controls (the session's
	// layers, in wire order): later lists update those and ignore other rids.
	Params protocol.ShareParams
	// StartBps is the full layer's start in bit/s; 0 = DefaultStartBps.
	StartBps int
	// SourceAspect is the source's width/height, for the upload hint; 0 = 16/9.
	SourceAspect float64
	// TargetHeight is the full layer's encoded height (from the engine's EventLayer); SetTargetHeight updates it.
	// 0 (unknown) shows no upload hint.
	TargetHeight int
}

// Controller is one share's rate control. It is not safe for concurrent use: one goroutine owns it.
type Controller struct {
	layers []*layer // in the wire order of the share.start encodings (f, then q)
	live   map[string]bool

	capBps       int  // the server's total for the video layers (QualityHint.MaxBitrate); 0 = none
	capAdmin     bool // the hint that set capBps was an admin hint (the admin's cap, not an estimate)
	audioWanted  int  // ShareParams.AudioBitrate within the Opus range; 0 = none given
	audioApplied int

	aspect       float64
	targetHeight int

	isLive bool
	liveAt time.Time

	ramp      int       // the ramp's bound on the full layer, in bit/s
	rampAt    time.Time // the earliest time of the next ramp step
	holdUntil time.Time // a hint that lowered the full layer holds the ramp until then
	limit     string    // the last SetLimit value
	limitHold time.Time // a cleared limit holds the ramp until then

	up uploadState
}

// layer is one video layer the Controller controls.
type layer struct {
	rid     string
	full    bool // rid f: it ramps, has FloorF and the full layer's engine range
	ceiling int  // the latest encodings' MaxBitrate (it includes the admin cap), within the engine range
	fps     int  // the latest encodings' MaxFramerate
	active  bool // the latest encodings' Active flag
	on      bool // what the engine and the Publisher were last told (Config.Params, then OpActive)
	wanted  int  // where the controller is heading; for an inactive layer, the target it resumes with
	applied int  // what the engine was last told
	// changed is the time of the layer's last change: an OpBitrate, a resume, or a switch to an encoder that
	// re-initializes. hasChanged is false until the first one (Initial's targets carry no time).
	changed    time.Time
	hasChanged bool
}

// New returns a Controller for a share whose start is cfg. It starts nothing; the share's targets are Initial.
func New(cfg Config) *Controller {
	c := &Controller{live: make(map[string]bool, len(cfg.Live)), targetHeight: cfg.TargetHeight}
	for rid, live := range cfg.Live {
		c.live[rid] = live
	}
	for _, e := range cfg.Params.Encodings {
		if e.RID == "" || c.layerOf(e.RID) != nil {
			continue
		}
		l := &layer{rid: e.RID, full: e.RID == protocol.RIDHigh}
		l.set(e)
		l.on = l.active
		c.layers = append(c.layers, l)
	}
	c.aspect = cfg.SourceAspect
	if !(c.aspect > 0) || math.IsInf(c.aspect, 0) {
		c.aspect = 16.0 / 9
	}
	start := cfg.StartBps
	if start <= 0 {
		start = DefaultStartBps
	}
	c.ramp = clamp(start, MinBitrateF, MaxBitrateF)
	c.audioWanted = audioTarget(cfg.Params.AudioBitrate)
	c.audioApplied = c.audioWanted
	c.recompute()
	for _, l := range c.layers {
		l.applied = l.wanted
	}
	return c
}

// Initial returns the share's first targets, for media.Config.Layers (LayerConfig.Bitrate) and Audio
// (AudioConfig.BitrateBPS): an OpBitrate per layer in wire order and an OpAudioBitrate when ShareParams.AudioBitrate
// is set. An inactive layer gets the target it would resume with. Like every first target, they apply at once.
func (c *Controller) Initial() []Action {
	acts := make([]Action, 0, len(c.layers)+1)
	for _, l := range c.layers {
		acts = append(acts, Action{Op: OpBitrate, Layer: l.rid, Bps: l.applied})
	}
	if c.audioApplied > 0 {
		acts = append(acts, Action{Op: OpAudioBitrate, Layer: media.LayerAudio, Bps: c.audioApplied})
	}
	return acts
}

// Live says the share went live (room.state shows it live): the ramp's first step comes RampDelay later, and the
// one step of an encoder without live bitrate OneStepDelay later. Only the first call counts.
func (c *Controller) Live(now time.Time) {
	if c.isLive {
		return
	}
	c.isLive = true
	c.liveAt = now
	c.rampAt = now.Add(RampDelay)
}

// Params applies a share.update reply (a preset change): new ceilings and Active flags from its encodings, and its
// AudioBitrate. Like the first targets, the resulting changes apply at once, without the guards (§9.4).
func (c *Controller) Params(p protocol.ShareParams, now time.Time) []Action {
	c.applyEncodings(p.Encodings)
	if a := audioTarget(p.AudioBitrate); a > 0 {
		c.audioWanted = a
	}
	c.recompute()
	return c.apply(now, true)
}

// Hint applies a quality.hint for the share. A hint with encodings is complete: they set the ceilings and the
// Active flags, and MaxBitrate the cap (0 or absent: none). A hint without encodings sets only a cap, when it has
// one; one with neither (a codec hint) changes nothing. Layers turned on or off change at once (OpActive, a resume
// with its target); bitrate changes pass the guards now or wait for a later Tick, the latest target winning.
//
// A hint that lowers the full layer through its cap is a backoff (§9.3): it holds the ramp for HoldAfterCongestion,
// and once the lower target applies, the ramp starts again from it, toward the cap, or the ceiling once the server
// lifts the cap. That is a congestion hint, but also any other non-admin hint that carries a lower cap: when a pause
// change and a lower estimate go out together, the server names the pause (M1 02 §11). The admin's cap is a policy,
// not a measurement, and holds nothing. A lower target that is still waiting for its guards when a later hint raises
// the cap again never applies and leaves the ramp where it was: the latest target wins.
func (c *Controller) Hint(h protocol.QualityHint, now time.Time) []Action {
	if len(h.Encodings) == 0 && h.MaxBitrate <= 0 {
		return nil
	}
	before := -1
	if f := c.fullLayer(); f != nil {
		before = f.wanted
	}
	c.applyEncodings(h.Encodings)
	c.capBps = int(min(max(h.MaxBitrate, 0), maxCap))
	c.capAdmin = h.Reason == protocol.HintReasonAdmin
	if !c.capAdmin {
		c.backoff(before, now)
	}
	c.recompute()
	return c.apply(now, false)
}

// SetLive records whether a layer's encoder changes bitrate live, from a later EventLayer (an encoder fallback or a
// re-init); the layer's guards follow it from now on. A switch to an encoder that re-initializes counts as the
// layer's change at now: the new encoder just started, so ReinitMinInterval runs from then.
func (c *Controller) SetLive(layer string, live bool, now time.Time) {
	was := c.live[layer]
	c.live[layer] = live
	if l := c.layerOf(layer); l != nil && was && !live && (!l.hasChanged || l.changed.Before(now)) {
		l.changed, l.hasChanged = now, true
	}
}

// SetLimit records the encoder's limit from media.EventLimit: "cpu" or "gpu" while it can't keep up, "none" when it
// can again (04 applies the hysteresis). A limit holds the ramp while it lasts and for HoldAfterLimit after it
// cleared, and makes CPU true. Any other value counts as no limit.
func (c *Controller) SetLimit(limit string, now time.Time) {
	was := c.limited()
	c.limit = limit
	if was && !c.limited() {
		c.limitHold = now.Add(HoldAfterLimit)
	}
}

// SetTargetHeight updates the full layer's encoded height, from an EventLayer of the full layer.
func (c *Controller) SetTargetHeight(h int) {
	c.targetHeight = h
}

// Tick runs the ramp and the guards; the share's actor calls it every TickEvery while the share is live. It returns
// the changes that are due now, and samples the upload hint's condition.
func (c *Controller) Tick(now time.Time) []Action {
	c.rampStep(now)
	c.recompute()
	acts := c.apply(now, false)
	c.sampleUpload(now)
	return acts
}

// State returns a copy of the controller's state, for the share's snapshot, logs and tests.
func (c *Controller) State() State {
	s := State{
		Cap:     c.capBps,
		Ceiling: make(map[string]int, len(c.layers)),
		Applied: make(map[string]int, len(c.layers)+1),
		Wanted:  make(map[string]int, len(c.layers)+1),
		Active:  make(map[string]bool, len(c.layers)),
		Limit:   c.limit,
	}
	for _, l := range c.layers {
		s.Ceiling[l.rid] = l.ceiling
		s.Applied[l.rid] = l.applied
		s.Wanted[l.rid] = l.wanted
		s.Active[l.rid] = l.on
	}
	if c.audioApplied > 0 {
		s.Applied[media.LayerAudio] = c.audioApplied
		s.Wanted[media.LayerAudio] = c.audioWanted
	}
	if f := c.fullLayer(); f != nil {
		s.Ramping = c.ramp < f.ceiling
	}
	return s
}

// Op is what an Action asks for.
type Op uint8

// The ops.
const (
	// OpBitrate is Session.SetBitrate(Layer, Bps).
	OpBitrate Op = iota + 1
	// OpActive is Publisher.SetActive(Layer, Active) and Session.SetLayerActive(Layer, Active). A resume carries the
	// layer's target in Bps, applied with Session.SetBitrate in the same step.
	OpActive
	// OpAudioBitrate is Session.SetBitrate(media.LayerAudio, Bps).
	OpAudioBitrate
)

func (o Op) String() string {
	switch o {
	case OpBitrate:
		return "bitrate"
	case OpActive:
		return "active"
	case OpAudioBitrate:
		return "audio-bitrate"
	}
	return fmt.Sprintf("Op(%d)", uint8(o))
}

// Action is one change for the engine and the Publisher.
type Action struct {
	Op     Op
	Layer  string // the rid; media.LayerAudio for OpAudioBitrate
	Bps    int    // OpBitrate, OpAudioBitrate, and OpActive when it resumes; 0 when it pauses
	Active bool   // OpActive
	Reinit bool   // the change costs an encoder re-init and an IDR (logs and stats)
}

// State is the controller's view of the share.
type State struct {
	Cap     int            // the server's total video cap in bit/s; 0 = none
	Ceiling map[string]int // per layer: its encodings' MaxBitrate (which includes the admin cap), in the engine range
	Applied map[string]int // what the engine was last told (with media.LayerAudio for Opus)
	Wanted  map[string]int // where it is heading, applied after the guards; for an inactive layer, its resume target
	Active  map[string]bool
	Ramping bool   // the ramp is still below the full layer's ceiling
	Limit   string // the last SetLimit value ("" = none reported)
}

// UploadHint is "Your upload allows about {ApproxHeight}p" (03's share panel; the web's ShareHint
// {kind: 'upload-limited', approxHeight}).
type UploadHint struct {
	Limited      bool
	ApproxHeight int // 360 | 480 | 540 | 720 | 900 | 1080; 0 when not limited
}

// layerOf returns the layer with rid, or nil.
func (c *Controller) layerOf(rid string) *layer {
	for _, l := range c.layers {
		if l.rid == rid {
			return l
		}
	}
	return nil
}

// fullLayer returns the full layer (rid f), or nil when the share has none.
func (c *Controller) fullLayer() *layer { return c.layerOf(protocol.RIDHigh) }

// applyEncodings takes a complete encodings list: each known layer's ceiling, frame rate and Active flag. The first
// entry of a rid counts; unknown rids are ignored, and layers the list doesn't name keep their values.
func (c *Controller) applyEncodings(list []protocol.Encoding) {
	seen := make(map[string]bool, len(list))
	for _, e := range list {
		if seen[e.RID] {
			continue
		}
		seen[e.RID] = true
		if l := c.layerOf(e.RID); l != nil {
			l.set(e)
		}
	}
}

// backoff holds the ramp when the cap now set lowered the full layer's target below before (its value before the
// hint).
func (c *Controller) backoff(before int, now time.Time) {
	capF, ok := c.capSplitFull()
	if !ok || capF >= before {
		return
	}
	c.holdUntil = now.Add(HoldAfterCongestion)
}

// settled records that the full layer's applied value is now bps: when a cap other than the admin's brought it below
// the ramp (a backoff, or a resume under such a cap), the ramp starts again from there.
func (c *Controller) settled(l *layer, bps int) {
	if l.full && c.capBps > 0 && !c.capAdmin && bps < c.ramp {
		c.ramp = bps
	}
}

// limited reports whether the last SetLimit said the encoder can't keep up.
func (c *Controller) limited() bool { return c.limit == "cpu" || c.limit == "gpu" }

// rampStep grows the ramp when a step is due (§9.3): every RampEvery from RampDelay after Live, by RampFactor up to
// the full layer's ceiling and toward the cap, while the full layer is active, no cap binds at or below the ramp,
// no congestion hold runs and the encoder isn't limited now or in the last HoldAfterLimit. The full layer of an
// encoder without live bitrate takes one step instead, OneStepDelay after Live, straight to its ceiling or the cap;
// its guards (30 %, 10 s) then decide whether the step applies. A live full layer's ramp steps only from a value in
// effect (applied at the ramp or above), so a step the guards hold back is not followed by another; a ramp held by
// the cap takes its next step at the first Tick after the cap rises or goes.
func (c *Controller) rampStep(now time.Time) {
	f := c.fullLayer()
	if f == nil || !c.isLive || !f.active || c.ramp >= f.ceiling {
		return
	}
	if c.live[f.rid] && f.applied < c.ramp {
		return
	}
	if now.Before(c.rampAt) || now.Before(c.holdUntil) || c.limited() || now.Before(c.limitHold) {
		return
	}
	capF, capped := c.capSplitFull()
	if capped && capF <= c.ramp {
		return // the cap binds at or below the ramp: it holds the ramp
	}
	next := f.ceiling
	if c.live[f.rid] {
		next = min(grow(c.ramp), f.ceiling)
	} else if now.Before(c.liveAt.Add(OneStepDelay)) {
		return
	}
	if capped {
		next = min(next, capF)
	}
	c.ramp = max(next, c.ramp)
	c.rampAt = now.Add(RampEvery)
}

// grow returns the ramp after one step: RampFactor percent of r, rounded up.
func grow(r int) int {
	return int((int64(r)*RampFactor + 99) / 100)
}

// recompute sets every layer's wanted target from the ceilings, the cap and the ramp.
func (c *Controller) recompute() {
	for _, l := range c.layers {
		l.wanted = c.target(l)
	}
}

// apply turns the differences between wanted and applied into Actions: OpActive at once for layers turned on or
// off (a resume with its target), OpBitrate for the active layers whose change passes the guards (or all of them
// with bypass), and OpAudioBitrate at once. Actions come in wire order, audio last.
func (c *Controller) apply(now time.Time, bypass bool) []Action {
	var acts []Action
	for _, l := range c.layers {
		reinit := !c.live[l.rid]
		switch {
		case l.active && !l.on:
			acts = append(acts, Action{Op: OpActive, Layer: l.rid, Active: true, Bps: l.wanted,
				Reinit: reinit && l.wanted != l.applied})
			l.on = true
			l.applied = l.wanted
			l.changed, l.hasChanged = now, true
			c.settled(l, l.applied)
		case !l.active && l.on:
			acts = append(acts, Action{Op: OpActive, Layer: l.rid, Active: false})
			l.on = false
		case l.active && l.wanted != l.applied && (bypass || c.passes(l, now)):
			acts = append(acts, Action{Op: OpBitrate, Layer: l.rid, Bps: l.wanted, Reinit: reinit})
			l.applied = l.wanted
			l.changed, l.hasChanged = now, true
			c.settled(l, l.applied)
		}
	}
	if c.audioWanted > 0 && c.audioWanted != c.audioApplied {
		acts = append(acts, Action{Op: OpAudioBitrate, Layer: media.LayerAudio, Bps: c.audioWanted})
		c.audioApplied = c.audioWanted
	}
	return acts
}

// passes reports whether the active layer l's change from applied to wanted may apply now (§9.4), by the guards of
// its encoder: live, at least MinChange and MinInterval since the layer's last change; re-init, ReinitMinChange
// and ReinitMinInterval. The interval always holds. These changes pass without the minimum step:
//   - a ramp step of a live full layer (wanted is the ramp): the ramp grows by 15 %, rounded up, so only its last
//     step, clamped at the ceiling or the cap, is smaller (6.996 → 8 Mbit/s is 14.4 %). Held back, it would leave
//     f short of the ceiling for good, or short of the cap with an upload hint the cap doesn't call for;
//   - a live layer's step up to exactly its ceiling, such as the preview's way back once the server lifts a cap;
//   - a decrease of a layer applied above its ceiling, whatever its encoder: the ceilings carry the admin's cap,
//     which the layer must come down to, however small the step.
//
// So for a re-init encoder only the admin's cap ever passes a step below ReinitMinChange.
func (c *Controller) passes(l *layer, now time.Time) bool {
	live := c.live[l.rid]
	pct, gap := MinChange, MinInterval
	if !live {
		pct, gap = ReinitMinChange, ReinitMinInterval
	}
	if l.hasChanged && now.Sub(l.changed) < gap {
		return false
	}
	switch {
	case live && l.wanted > l.applied && (l.wanted == l.ceiling || (l.full && l.wanted == c.ramp)):
		return true
	case l.wanted < l.applied && l.applied > l.ceiling:
		return true
	}
	diff := int64(l.wanted) - int64(l.applied)
	if diff < 0 {
		diff = -diff
	}
	return diff*100 >= int64(l.applied)*int64(pct)
}
