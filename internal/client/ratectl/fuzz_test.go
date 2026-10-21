package ratectl

import (
	"slices"
	"testing"

	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// FuzzController drives a Controller with any sequence of calls (ticks, hints, share.update replies, encoder flags,
// limits, target heights) at non-decreasing instants and checks, after every call, what must hold whatever the
// server sends:
//   - every Bps is within 04 §6.4's range for its layer, and the ops are consistent (a layer is resumed only when it
//     is off, paused only when it is on, and gets OpBitrate only while it is on);
//   - Tick and Hint keep the guards: a layer's OpBitrate comes at least MinInterval (live) or ReinitMinInterval
//     (re-init) after its last change, and is at least MinChange or ReinitMinChange of the applied value, except a
//     live layer's step up to its ceiling or a live full layer's ramp step, and a step down of a layer applied
//     above its ceiling;
//   - Reinit is set exactly for layers whose encoder re-initializes;
//   - State reports what the Actions told the engine, every Wanted is within range, and an upload hint names one of
//     the six heights.
//
// Seeds are in testdata/fuzz/FuzzController.
func FuzzController(f *testing.F) {
	f.Add([]byte{1, 1, 0, 40, 0, 7, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{0, 1, 2, 0, 20, 7, 0, 0, 0, 0, 2, 0, 13, 80, 1, 3, 0, 0, 0, 0, 4, 1, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		r := &fuzzReader{data: data}
		live := map[string]bool{"f": r.next()&1 == 1, "q": r.next()&1 == 1}
		presets := []protocol.Preset{protocol.PresetAuto, protocol.PresetGame, protocol.PresetMovie, protocol.PresetText}
		params := presetParams(presets[r.next()%4], int64(r.next())*100)
		c := New(Config{
			Live: live, Params: params, StartBps: int(r.next()) * 100_000,
			SourceAspect: float64(r.next()) / 64, TargetHeight: int(r.next()) * 8,
		})
		m := &fuzzModel{t: t, c: c, live: live, applied: map[string]int{}, on: map[string]bool{},
			changed: map[string]float64{}}
		for _, e := range params.Encodings {
			m.on[e.RID] = e.Active
		}
		m.check("Initial", 0, c.Initial(), false, true)

		now := 0.0
		for ops := 0; r.more() && ops < 4000; ops++ {
			switch r.next() % 8 {
			case 0:
				now += 0.5
				m.check("Tick", now, c.Tick(at(now)), true, false)
			case 1:
				now += float64(r.next()) / 10
			case 2:
				m.check("Hint", now, c.Hint(fuzzHint(r), at(now)), true, false)
			case 3:
				p := presetParams(presets[r.next()%4], int64(r.next())*100)
				p.AudioBitrate = int(r.next()) * 4_000
				p.Encodings[0].Active, p.Encodings[1].Active = r.next()&1 == 1, r.next()&1 == 1
				m.check("Params", now, c.Params(p, at(now)), false, false)
			case 4:
				layer := []string{"f", "q", "h"}[r.next()%3]
				l := r.next()&1 == 1
				last, changed := m.changed[layer]
				if was := m.live[layer]; was && !l && m.has(layer) && (!changed || last < now) {
					m.changed[layer] = now // a switch to re-init is the layer's change
				}
				m.live[layer] = l
				c.SetLive(layer, l, at(now))
			case 5:
				c.SetLimit([]string{"cpu", "gpu", "none", "source", ""}[r.next()%5], at(now))
			case 6:
				c.SetTargetHeight(int(r.next()) * 8)
			case 7:
				c.Live(at(now))
			}
			if u := c.Upload(at(now)); u.Limited && !slices.Contains(uploadHeights[:], u.ApproxHeight) {
				t.Fatalf("Upload at %gs = %+v", now, u)
			}
		}
	})
}

// fuzzHint builds a quality.hint from the fuzzer's bytes.
func fuzzHint(r *fuzzReader) protocol.QualityHint {
	reasons := []protocol.HintReason{protocol.HintReasonCongestion, protocol.HintReasonViewers,
		protocol.HintReasonAdmin, protocol.HintReasonCodec, ""}
	h := protocol.QualityHint{Reason: reasons[r.next()%5], MaxBitrate: int64(r.next()) * 50_000}
	if b := r.next(); b&1 == 1 {
		h.Encodings = encs(int64(r.next())*100_000, int(r.next()%61), b&2 == 2, b&4 == 4)
		if b&8 == 8 {
			h.Encodings[1].MaxBitrate = int64(r.next()) * 20_000
		}
	}
	return h
}

type fuzzReader struct {
	data []byte
	i    int
}

func (r *fuzzReader) more() bool { return r.i < len(r.data) }

func (r *fuzzReader) next() byte {
	if r.i >= len(r.data) {
		return 0
	}
	b := r.data[r.i]
	r.i++
	return b
}

// fuzzModel is what the engine was told, rebuilt from the Actions alone.
type fuzzModel struct {
	t       *testing.T
	c       *Controller
	live    map[string]bool
	applied map[string]int
	on      map[string]bool
	changed map[string]float64 // seconds; absent: no change yet
}

func (m *fuzzModel) has(layer string) bool { _, ok := m.on[layer]; return ok }

// check checks the Actions one call returned at now; guarded calls keep the guards, initial ones set the values.
func (m *fuzzModel) check(call string, now float64, acts []Action, guarded, initial bool) {
	t := m.t
	st := m.c.State()
	seen := map[string]bool{}
	for _, a := range acts {
		if a.Op == OpAudioBitrate {
			if a.Layer != media.LayerAudio || a.Bps < MinBitrateAudio || a.Bps > MaxBitrateAudio || seen[a.Layer] {
				t.Fatalf("%s at %gs: %+v", call, now, a)
			}
			seen[a.Layer] = true
			m.applied[a.Layer] = a.Bps
			continue
		}
		if !m.has(a.Layer) || seen[a.Layer] {
			t.Fatalf("%s at %gs: %+v for an unknown or repeated layer", call, now, a)
		}
		seen[a.Layer] = true
		lo, hi := MinBitrateLayer, MaxBitrateLayer
		if a.Layer == protocol.RIDHigh {
			lo, hi = MinBitrateF, MaxBitrateF
		}
		inRange := a.Bps >= lo && a.Bps <= hi
		switch {
		case initial:
			if a.Op != OpBitrate || !inRange {
				t.Fatalf("%s: %+v", call, a)
			}
			m.applied[a.Layer] = a.Bps
		case a.Op == OpActive && a.Active:
			if m.on[a.Layer] || !inRange || a.Reinit != (!m.live[a.Layer] && a.Bps != m.applied[a.Layer]) {
				t.Fatalf("%s at %gs: resume %+v (on %v, applied %d)", call, now, a, m.on[a.Layer], m.applied[a.Layer])
			}
			m.on[a.Layer], m.applied[a.Layer], m.changed[a.Layer] = true, a.Bps, now
		case a.Op == OpActive:
			if !m.on[a.Layer] || a.Bps != 0 || a.Reinit {
				t.Fatalf("%s at %gs: pause %+v (on %v)", call, now, a, m.on[a.Layer])
			}
			m.on[a.Layer] = false
		case a.Op == OpBitrate:
			if !m.on[a.Layer] || !inRange || a.Bps == m.applied[a.Layer] || a.Reinit != !m.live[a.Layer] {
				t.Fatalf("%s at %gs: %+v (on %v, applied %d, live %v)", call, now, a, m.on[a.Layer],
					m.applied[a.Layer], m.live[a.Layer])
			}
			if guarded {
				m.checkGuards(call, now, a, st)
			}
			m.applied[a.Layer], m.changed[a.Layer] = a.Bps, now
		default:
			t.Fatalf("%s at %gs: unknown op %+v", call, now, a)
		}
	}
	for layer, on := range m.on {
		if st.Active[layer] != on || st.Applied[layer] != m.applied[layer] {
			t.Fatalf("%s at %gs: State %+v, the Actions say on %v applied %d for %s", call, now, st, on,
				m.applied[layer], layer)
		}
	}
	if a, ok := m.applied[media.LayerAudio]; ok && st.Applied[media.LayerAudio] != a {
		t.Fatalf("%s at %gs: audio applied %d, the Actions say %d", call, now, st.Applied[media.LayerAudio], a)
	}
	for layer, w := range st.Wanted {
		lo, hi := MinBitrateLayer, MaxBitrateLayer
		switch layer {
		case protocol.RIDHigh:
			lo, hi = MinBitrateF, MaxBitrateF
		case media.LayerAudio:
			lo, hi = MinBitrateAudio, MaxBitrateAudio
		}
		if w < lo || w > hi {
			t.Fatalf("%s at %gs: %s wanted %d outside [%d, %d]", call, now, layer, w, lo, hi)
		}
	}
}

// checkGuards checks a guarded OpBitrate against §9.4 and its exceptions.
func (m *fuzzModel) checkGuards(call string, now float64, a Action, st State) {
	t := m.t
	live := m.live[a.Layer]
	pct, gap := MinChange, MinInterval
	if !live {
		pct, gap = ReinitMinChange, ReinitMinInterval
	}
	if last, ok := m.changed[a.Layer]; ok && at(now).Sub(at(last)) < gap {
		t.Fatalf("%s at %gs: %+v only %v after the layer's change at %gs", call, now, a, at(now).Sub(at(last)), last)
	}
	prev := m.applied[a.Layer]
	ceiling := st.Ceiling[a.Layer]
	switch {
	case live && a.Bps > prev && (a.Bps == ceiling || (a.Layer == protocol.RIDHigh && a.Bps == m.c.ramp)):
		return // the step up to the ceiling, or a ramp step
	case a.Bps < prev && prev > ceiling:
		return // down toward the admin's cap
	}
	diff := int64(a.Bps) - int64(prev)
	if diff < 0 {
		diff = -diff
	}
	if diff*100 < int64(prev)*int64(pct) {
		t.Fatalf("%s at %gs: %+v is %.1f %% from %d, below %d %%", call, now, a, float64(diff)*100/float64(prev),
			prev, pct)
	}
}

// TestFuzzModelSeesTheRamp makes sure the fuzz model's guard check accepts the spec's own sequences: the ramp, a
// backoff and its recovery (a failing model would make the fuzz target useless).
func TestFuzzModelSeesTheRamp(t *testing.T) {
	c := newController(bothLive, autoParams)
	m := &fuzzModel{t: t, c: c, live: map[string]bool{"f": true, "q": true}, applied: map[string]int{},
		on: map[string]bool{"f": true, "q": true}, changed: map[string]float64{}}
	m.check("Initial", 0, c.Initial(), false, true)
	c.Live(at(0))
	for s := 0.5; s <= 60; s += 0.5 {
		if s == 20 {
			m.check("Hint", s, c.Hint(congestion(2_600_000), at(s)), true, false)
		}
		if s == 25 {
			m.check("Hint", s, c.Hint(congestion(0), at(s)), true, false)
		}
		m.check("Tick", s, c.Tick(at(s)), true, false)
	}
	if m.applied["f"] != 8_000_000 {
		t.Errorf("f at %d after the recovery", m.applied["f"])
	}
}
