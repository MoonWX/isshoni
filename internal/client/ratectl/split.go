package ratectl

import (
	"math"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// Ceilings, the cap and the split (01 §9.2).
//
//   - A layer's ceiling is its MaxBitrate in the latest complete encodings list (ShareParams at the start and from
//     share.update, or a hint's); it already includes the admin's cap (M1 02 §8.6). A list without a MaxBitrate
//     gives the engine range's top.
//   - The cap is QualityHint.MaxBitrate, the server's total for the share's video layers (audio excluded).
//   - A total T (the cap when set, else the sum of the active layers' ceilings) is split over the active layers: the
//     preview q gets min(q.ceiling, max(FloorQ, QShare × T)), the full layer f gets min(f.ceiling, T − q), at least
//     FloorF; a single active layer gets min(ceiling, T), at least its floor. So a cap of 8 Mbit/s leaves f
//     7.7 Mbit/s, a cap of 600 kbit/s gives q 180 kbit/s and f 420 kbit/s, and a cap below 400 kbit/s sends the
//     floors (150 + 250). Any layer other than f (none in M2) is split like q.
//   - f's target is min(its split, the ramp); every target is clamped to 04 §6.4's engine ranges.

// set takes the layer's entry of a complete encodings list.
func (l *layer) set(e protocol.Encoding) {
	lo, hi := l.bounds()
	if e.MaxBitrate <= 0 {
		l.ceiling = hi
	} else {
		l.ceiling = int(min(max(e.MaxBitrate, int64(lo)), int64(hi)))
	}
	l.fps = e.MaxFramerate
	l.active = e.Active
}

// bounds returns the layer's engine range (04 §6.4).
func (l *layer) bounds() (lo, hi int) {
	if l.full {
		return MinBitrateF, MaxBitrateF
	}
	return MinBitrateLayer, MaxBitrateLayer
}

// floor returns the layer's smallest share of a split.
func (l *layer) floor() int {
	if l.full {
		return FloorF
	}
	return FloorQ
}

// target returns l's target: its share of the split over the active layers (an inactive l counted among them, which
// gives the target it resumes with), bounded by the ramp for f, clamped to the engine range.
func (c *Controller) target(l *layer) int {
	set := c.activeWith(l)
	bps := 0
	for i, share := range splitTotal(c.total(set), set) {
		if set[i] == l {
			bps = share
		}
	}
	if l.full {
		bps = min(bps, c.ramp)
	}
	lo, hi := l.bounds()
	return clamp(bps, lo, hi)
}

// capSplitFull returns the full layer's share of the cap's split (clamped to its engine range, without the ramp),
// and false when there is no cap or no active full layer.
func (c *Controller) capSplitFull() (int, bool) {
	f := c.fullLayer()
	if c.capBps <= 0 || f == nil || !f.active {
		return 0, false
	}
	set := c.activeWith(nil)
	for i, share := range splitTotal(c.capBps, set) {
		if set[i] == f {
			lo, hi := f.bounds()
			return clamp(share, lo, hi), true
		}
	}
	return 0, false
}

// activeWith returns the active layers, and also extra when it is inactive, in wire order.
func (c *Controller) activeWith(extra *layer) []*layer {
	set := make([]*layer, 0, len(c.layers))
	for _, l := range c.layers {
		if l.active || l == extra {
			set = append(set, l)
		}
	}
	return set
}

// total returns the split's total over set: the cap when set, else the sum of the layers' ceilings.
func (c *Controller) total(set []*layer) int {
	if c.capBps > 0 {
		return c.capBps
	}
	sum := 0
	for _, l := range set {
		sum += l.ceiling
	}
	return sum
}

// splitTotal splits t over set (§9.2) and returns each layer's share, aligned with set and not yet clamped to the
// engine ranges.
func splitTotal(t int, set []*layer) []int {
	out := make([]int, len(set))
	if len(set) == 1 {
		out[0] = max(set[0].floor(), min(set[0].ceiling, t))
		return out
	}
	full, rest := -1, t
	for i, l := range set {
		if l.full && full < 0 {
			full = i
			continue
		}
		out[i] = min(l.ceiling, max(FloorQ, int(math.Round(QShare*float64(t)))))
		rest -= out[i]
	}
	if full >= 0 {
		out[full] = max(FloorF, min(set[full].ceiling, rest))
	}
	return out
}

// audioTarget returns the Opus target for ShareParams.AudioBitrate, within the engine range; 0 when none is given.
func audioTarget(bps int) int {
	if bps <= 0 {
		return 0
	}
	return clamp(bps, MinBitrateAudio, MaxBitrateAudio)
}

// clamp returns v within [lo, hi].
func clamp(v, lo, hi int) int { return min(max(v, lo), hi) }
