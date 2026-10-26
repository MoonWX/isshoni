package ratectl

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// Every test runs at fixed instants: seconds after t0.
var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func at(s float64) time.Time { return t0.Add(time.Duration(math.Round(s * float64(time.Second)))) }

// event is an Action and the second at which a call returned it.
type event struct {
	At float64
	Action
}

func (e event) String() string {
	return fmt.Sprintf("%gs %s %s bps=%d active=%v reinit=%v", e.At, e.Op, e.Layer, e.Bps, e.Active, e.Reinit)
}

// tickRange calls Tick every 500 ms from from through to and returns what the ticks did.
func tickRange(c *Controller, from, to float64) []event {
	var out []event
	for s := from; s <= to+1e-9; s += 0.5 {
		out = append(out, events(s, c.Tick(at(s)))...)
	}
	return out
}

func events(s float64, acts []Action) []event {
	out := make([]event, 0, len(acts))
	for _, a := range acts {
		out = append(out, event{s, a})
	}
	return out
}

func checkEvents(t *testing.T, what string, got, want []event) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s:\n got  %s\n want %s", what, joinEvents(got), joinEvents(want))
	}
}

func joinEvents(es []event) string {
	if len(es) == 0 {
		return "(none)"
	}
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = e.String()
	}
	return strings.Join(parts, "\n      ")
}

func checkActions(t *testing.T, what string, got []Action, want ...Action) {
	t.Helper()
	checkEvents(t, what, events(0, got), events(0, want))
}

func bitrate(layer string, bps int) Action { return Action{Op: OpBitrate, Layer: layer, Bps: bps} }

func reinitBitrate(layer string, bps int) Action {
	return Action{Op: OpBitrate, Layer: layer, Bps: bps, Reinit: true}
}

func audioBitrate(bps int) Action {
	return Action{Op: OpAudioBitrate, Layer: media.LayerAudio, Bps: bps}
}

// encs is a complete encodings list as the server builds it (M1 02 §8.6): f with fMax and fps, q at 0.3 Mbit/s 15 fps.
func encs(fMax int64, fps int, fActive, qActive bool) []protocol.Encoding {
	return []protocol.Encoding{
		{RID: protocol.RIDHigh, Layer: protocol.VideoLayerHigh, Active: fActive, MaxBitrate: fMax, MaxFramerate: fps,
			MaxPixels: 2_073_600},
		{RID: protocol.RIDLow, Layer: protocol.VideoLayerLow, Active: qActive, MaxBitrate: 300_000, MaxFramerate: 15,
			MaxPixels: 230_400},
	}
}

// presetParams is StartShare's ShareParams for a preset under an admin cap in kbit/s (0 = none), M1 02 §8.6.
func presetParams(p protocol.Preset, adminKbps int64) protocol.ShareParams {
	fMax := int64(8_000_000)
	if adminKbps > 0 && adminKbps*1000 < fMax {
		fMax = adminKbps * 1000
	}
	fps, audio := 60, 128_000
	if p == protocol.PresetText {
		fps = 30
	}
	if p == protocol.PresetMovie {
		audio = 256_000
	}
	return protocol.ShareParams{
		ShareID: "s_q7m2x9c4v8b1n5k3", Codec: protocol.CodecH264High, Encodings: encs(fMax, fps, true, true),
		AudioBitrate: audio,
	}
}

var (
	bothLive   = map[string]bool{"f": true, "q": true}
	fReinit    = map[string]bool{"f": false, "q": true}
	qReinit    = map[string]bool{"f": true, "q": false}
	autoParams = presetParams(protocol.PresetAuto, 0)
)

func newController(live map[string]bool, p protocol.ShareParams) *Controller {
	return New(Config{Live: live, Params: p, TargetHeight: 1080})
}

// ramped returns an Auto share whose full layer reached its ceiling (8 Mbit/s) at the Tick at 10 s: its last change.
// A live full layer went live RampDelay and four RampEvery earlier (its five steps), a re-init one OneStepDelay
// earlier (its one step).
func ramped(t *testing.T, live map[string]bool) *Controller {
	t.Helper()
	c := newController(live, autoParams)
	c.Initial()
	from := 10 - (RampDelay + 4*RampEvery).Seconds()
	if !live["f"] {
		from = 10 - OneStepDelay.Seconds()
	}
	c.Live(at(from))
	tickRange(c, from+0.5, 10)
	if got := c.State().Applied["f"]; got != 8_000_000 {
		t.Fatalf("ramped: f applied %d after 10 s, want 8000000", got)
	}
	return c
}

func congestion(capBps int64) protocol.QualityHint {
	return protocol.QualityHint{ShareID: autoParams.ShareID, Reason: protocol.HintReasonCongestion, MaxBitrate: capBps,
		Encodings: encs(8_000_000, 60, true, true)}
}

func TestInitialPerPreset(t *testing.T) {
	cases := []struct {
		name   string
		params protocol.ShareParams
		start  int
		want   []Action
	}{
		{"auto", presetParams(protocol.PresetAuto, 0), 0,
			[]Action{bitrate("f", 4_000_000), bitrate("q", 300_000), audioBitrate(128_000)}},
		{"game", presetParams(protocol.PresetGame, 0), 0,
			[]Action{bitrate("f", 4_000_000), bitrate("q", 300_000), audioBitrate(128_000)}},
		{"movie", presetParams(protocol.PresetMovie, 0), 0,
			[]Action{bitrate("f", 4_000_000), bitrate("q", 300_000), audioBitrate(256_000)}},
		{"text", presetParams(protocol.PresetText, 0), 0,
			[]Action{bitrate("f", 4_000_000), bitrate("q", 300_000), audioBitrate(128_000)}},
		{"auto with an admin cap of 5000 kbit/s", presetParams(protocol.PresetAuto, 5000), 0,
			[]Action{bitrate("f", 4_000_000), bitrate("q", 300_000), audioBitrate(128_000)}},
		{"auto with an admin cap of 2000 kbit/s", presetParams(protocol.PresetAuto, 2000), 0,
			[]Action{bitrate("f", 2_000_000), bitrate("q", 300_000), audioBitrate(128_000)}},
		{"a start of 3 Mbit/s", presetParams(protocol.PresetAuto, 0), 3_000_000,
			[]Action{bitrate("f", 3_000_000), bitrate("q", 300_000), audioBitrate(128_000)}},
		{"no audio bitrate", protocol.ShareParams{Encodings: encs(8_000_000, 60, true, true)}, 0,
			[]Action{bitrate("f", 4_000_000), bitrate("q", 300_000)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := New(Config{Live: bothLive, Params: tc.params, StartBps: tc.start})
			checkActions(t, "Initial", c.Initial(), tc.want...)
			st := c.State()
			for _, a := range tc.want {
				if st.Applied[a.Layer] != a.Bps || st.Wanted[a.Layer] != a.Bps {
					t.Errorf("State %s: applied %d wanted %d, want %d", a.Layer, st.Applied[a.Layer],
						st.Wanted[a.Layer], a.Bps)
				}
			}
			// The first targets need nothing more.
			checkEvents(t, "ticks before Live", tickRange(c, 0, 30), nil)
		})
	}
}

func TestInitialInactiveLayer(t *testing.T) {
	p := autoParams
	p.Encodings = encs(8_000_000, 60, false, true)
	c := newController(bothLive, p)
	// The paused layer gets the target it resumes with; q alone gets min(ceiling, total).
	checkActions(t, "Initial", c.Initial(), bitrate("f", 4_000_000), bitrate("q", 300_000), audioBitrate(128_000))
	if st := c.State(); st.Active["f"] || !st.Active["q"] {
		t.Errorf("Active = %v, want f off and q on", st.Active)
	}
	c.Live(at(0))
	// The ramp waits while f is paused.
	checkEvents(t, "ticks while f is paused", tickRange(c, 0.5, 20), nil)
	h := protocol.QualityHint{Reason: protocol.HintReasonViewers, Encodings: encs(8_000_000, 60, true, true)}
	checkActions(t, "resume", c.Hint(h, at(20.2)),
		Action{Op: OpActive, Layer: "f", Active: true, Bps: 4_000_000})
	// The resume was f's change: the ramp's next step applies 2 s later.
	checkEvents(t, "ticks after the resume", tickRange(c, 20.5, 22.5), []event{{22.5, bitrate("f", 4_600_000)}})
}

func TestRamp(t *testing.T) {
	steps := []event{
		{2, bitrate("f", 4_600_000)},
		{5, bitrate("f", 5_290_000)},
		{8, bitrate("f", 6_083_500)},
		{11, bitrate("f", 6_996_025)},
		{14, bitrate("f", 8_000_000)}, // 14.4 %: the last step to the ceiling passes
	}
	c := newController(bothLive, autoParams)
	c.Initial()
	c.Live(at(0))
	var got []event
	for s := 0.5; s <= 30; s += 0.5 {
		got = append(got, events(s, c.Tick(at(s)))...)
		if want := s < 14; c.State().Ramping != want {
			t.Errorf("Ramping at %gs = %v, want %v", s, !want, want)
		}
	}
	checkEvents(t, "ramp 4 → 8 Mbit/s in 5 steps and 14 s", got, steps)

	// Live at an instant between Ticks: five steps, 3 s apart, the first one 2 s after Live (within a Tick).
	c = newController(bothLive, autoParams)
	c.Live(at(0.3))
	got = tickRange(c, 0.5, 30)
	if len(got) != 5 || got[0].At != 2.5 || got[4].At != 14.5 || got[4].Bps != 8_000_000 {
		t.Errorf("ramp from Live at 0.3 s:\n %s", joinEvents(got))
	}

	// No ramp before Live, and Live counts once.
	c = newController(bothLive, autoParams)
	checkEvents(t, "ticks before Live", tickRange(c, 0, 10), nil)
	c.Live(at(10))
	c.Live(at(15))
	got = tickRange(c, 10.5, 12)
	checkEvents(t, "after Live at 10 s", got, []event{{12, bitrate("f", 4_600_000)}})
}

func TestRampCongestionHold(t *testing.T) {
	c := ramped(t, bothLive)
	// T = 2.6 Mbit/s: q keeps 0.3, f gets 2.3. The hint lowers f: the ramp drops to 2.3 and holds for 10 s.
	checkActions(t, "congestion hint", c.Hint(congestion(2_600_000), at(20.2)), bitrate("f", 2_300_000))
	// The server lifts the cap; f stays where the ramp is.
	checkActions(t, "cap lifted", c.Hint(congestion(0), at(22.2)))
	checkEvents(t, "the hold", tickRange(c, 20.5, 30), nil)

	got := tickRange(c, 30.5, 60)
	if len(got) != 9 {
		t.Fatalf("after the hold: %d steps, want 9:\n %s", len(got), joinEvents(got))
	}
	prev := 2_300_000
	for i, e := range got {
		if want := 30.5 + RampEvery.Seconds()*float64(i); e.At != want || e.Op != OpBitrate || e.Layer != "f" {
			t.Errorf("step %d: %v, want f at %gs", i, e, want)
		}
		if i < len(got)-1 && e.Bps != grow(prev) {
			t.Errorf("step %d: %d after %d, want ×1.15", i, e.Bps, prev)
		}
		prev = e.Bps
	}
	if got[0].Bps != 2_645_000 || prev != 8_000_000 {
		t.Errorf("ramp after the hold: first %d, last %d; want 2645000 and 8000000", got[0].Bps, prev)
	}

	// A congestion cap above the ramp lowers nothing and holds nothing: the ramp grows toward it.
	c = newController(bothLive, autoParams)
	c.Live(at(0))
	tickRange(c, 0.5, 2) // f at 4.6
	checkActions(t, "a cap above the ramp", c.Hint(congestion(7_000_000), at(2.2)))
	// The ramp's last step stops at the cap's split (7.0 − 0.3), 10.1 %: a ramp step, so it applies.
	checkEvents(t, "the ramp toward a cap", tickRange(c, 2.5, 30), []event{
		{5, bitrate("f", 5_290_000)},
		{8, bitrate("f", 6_083_500)},
		{11, bitrate("f", 6_700_000)},
	})
	if st := c.State(); !st.Ramping || c.ramp != 6_700_000 {
		t.Errorf("Ramping %v, ramp %d; want the ramp held at the cap's split 6700000", st.Ramping, c.ramp)
	}
	// The server lifts the cap: the ramp goes on to the ceiling, 6.7 × 1.15 = 7.705, then 8.
	checkEvents(t, "cap lifted", events(30.2, c.Hint(congestion(0), at(30.2))), nil)
	checkEvents(t, "the rest of the ramp", tickRange(c, 30.5, 40),
		[]event{{30.5, bitrate("f", 7_705_000)}, {33.5, bitrate("f", 8_000_000)}})
}

func TestLiveGuards(t *testing.T) {
	t.Run("15 percent", func(t *testing.T) {
		c := ramped(t, bothLive)
		// T = 7.3: f 7.0, 12.5 % below 8.0. A live layer keeps MinChange for a decrease to the cap (README WF6
		// waived ReinitMinChange only).
		checkActions(t, "a 12.5 % change", c.Hint(congestion(7_300_000), at(13)))
		checkEvents(t, "ticks", tickRange(c, 13.5, 40), nil)
		if st := c.State(); st.Wanted["f"] != 7_000_000 || st.Applied["f"] != 8_000_000 {
			t.Errorf("wanted %d applied %d, want 7000000 waiting at 8000000", st.Wanted["f"], st.Applied["f"])
		}
	})
	t.Run("2 seconds and latest wins", func(t *testing.T) {
		c := ramped(t, bothLive) // f's last change at 10
		checkActions(t, "22.5 % after 3 s", c.Hint(congestion(6_500_000), at(13)), bitrate("f", 6_200_000))
		checkActions(t, "24 % after 1 s", c.Hint(congestion(5_000_000), at(14)))
		checkActions(t, "a later hint after 1.5 s", c.Hint(congestion(4_000_000), at(14.5)))
		checkEvents(t, "ticks", tickRange(c, 14.5, 30), []event{{15, bitrate("f", 3_700_000)}})
	})
	t.Run("a waiting change that shrinks below 15 percent", func(t *testing.T) {
		c := ramped(t, bothLive)
		checkActions(t, "first", c.Hint(congestion(6_500_000), at(13)), bitrate("f", 6_200_000))
		checkActions(t, "too early", c.Hint(congestion(5_000_000), at(14)))
		checkActions(t, "back near the applied value", c.Hint(congestion(6_800_000), at(14.5)))
		checkEvents(t, "ticks", tickRange(c, 14.5, 23.5), nil)
		// The lower target never applied, so the ramp stayed at 6.2; after the hold of the hint at 14 it steps to
		// the cap's split (a ramp step: 4.8 %).
		checkEvents(t, "after the hold", tickRange(c, 24, 40), []event{{24, bitrate("f", 6_500_000)}})
	})
	t.Run("per layer", func(t *testing.T) {
		c := ramped(t, bothLive)
		// T = 600 k: q 180 k, f 420 k. q never changed, f changed at 10: both apply at once.
		checkActions(t, "600 kbit/s", c.Hint(congestion(600_000), at(12)),
			bitrate("f", 420_000), bitrate("q", 180_000))
		// T = 500 k: q 150 k, f 350 k, both 16.7 %: only after 2 s.
		checkActions(t, "500 kbit/s after 1 s", c.Hint(congestion(500_000), at(13)))
		checkEvents(t, "ticks", tickRange(c, 13.5, 20),
			[]event{{14, bitrate("f", 350_000)}, {14, bitrate("q", 150_000)}})
	})
}

func TestReinitGuards(t *testing.T) {
	t.Run("one-step ramp", func(t *testing.T) {
		c := newController(fReinit, autoParams)
		checkActions(t, "Initial", c.Initial(), bitrate("f", 4_000_000), bitrate("q", 300_000), audioBitrate(128_000))
		c.Live(at(0))
		checkEvents(t, "ticks", tickRange(c, 0.5, 30), []event{{10, reinitBitrate("f", 8_000_000)}})
	})
	t.Run("30 percent and 10 seconds", func(t *testing.T) {
		c := ramped(t, fReinit) // the one step at 10
		// T = 5.0: f 4.7, 41 % below: 10 s after the one step.
		checkActions(t, "41 % after 3 s", c.Hint(congestion(5_000_000), at(13)))
		checkEvents(t, "ticks", tickRange(c, 13.5, 25), []event{{20, reinitBitrate("f", 4_700_000)}})
		// T = 5.8: f's share of the cap is 5.5, 17 % up. The cap allows the rise but doesn't force it, and 5.5 is not
		// the ceiling: it waits for 30 %, for good.
		checkActions(t, "17 % up", c.Hint(congestion(5_800_000), at(25.2)))
		checkEvents(t, "ticks", tickRange(c, 25.5, 40), nil)
		if st := c.State(); st.Wanted["f"] != 5_500_000 || st.Applied["f"] != 4_700_000 {
			t.Errorf("wanted %d applied %d, want 5500000 waiting at 4700000", st.Wanted["f"], st.Applied["f"])
		}
		// T = 6.5: f 6.2, 32 % up: the ramp's next step, to the cap's split, applies at once.
		checkActions(t, "32 % up", c.Hint(congestion(6_500_000), at(40.2)))
		checkEvents(t, "ticks", tickRange(c, 40.5, 50), []event{{40.5, reinitBitrate("f", 6_200_000)}})
	})
	t.Run("a decrease to the cap below 30 percent", func(t *testing.T) {
		// README WF6, with WF5-R3's numbers: the full layer came down to 2.72 Mbit/s under a 3 Mbit/s policer, and
		// the estimator's next backoff, to 80 % of what got through (2.28 Mbit/s), asks for 1.98 Mbit/s, 27 % below.
		// The cap forces it: it applies 10 s after the layer's last change, not a Tick earlier, and so does a
		// decrease of 2.5 %.
		c := ramped(t, fReinit) // the one step at 10
		checkActions(t, "T = 3.02 Mbit/s", c.Hint(congestion(3_020_000), at(15)))
		checkEvents(t, "ticks", tickRange(c, 15.5, 27), []event{{20, reinitBitrate("f", 2_720_000)}})
		checkActions(t, "T = 2.28 Mbit/s: 27 % below", c.Hint(congestion(2_280_000), at(27.2)))
		checkEvents(t, "ticks", tickRange(c, 27.5, 31), []event{{30, reinitBitrate("f", 1_980_000)}})
		checkActions(t, "T = 2.23 Mbit/s: 2.5 % below", c.Hint(congestion(2_230_000), at(31.2)))
		checkEvents(t, "ticks", tickRange(c, 31.5, 50), []event{{40, reinitBitrate("f", 1_930_000)}})

		// A decrease of 25 %; and the same one cancelled by a higher cap while it waits for its interval (latest
		// wins).
		c = ramped(t, fReinit)
		checkActions(t, "T = 6.3 Mbit/s: 25 % below, 2 s after the one step", c.Hint(congestion(6_300_000), at(12)))
		checkEvents(t, "ticks", tickRange(c, 12.5, 20), []event{{20, reinitBitrate("f", 6_000_000)}})
		c = ramped(t, fReinit)
		checkActions(t, "T = 6.3 Mbit/s", c.Hint(congestion(6_300_000), at(12)))
		checkActions(t, "T = 9 Mbit/s: f's share is its ceiling", c.Hint(congestion(9_000_000), at(15)))
		checkEvents(t, "ticks", tickRange(c, 15.5, 40), nil)

		// Any cap, the admin's total too: f's ceiling is the admin's 5000 kbit/s, which the one step reaches, and a
		// hint with that total gives f 4.7, 6 % below.
		c = newController(fReinit, presetParams(protocol.PresetAuto, 5000))
		c.Live(at(0))
		checkEvents(t, "the one step", tickRange(c, 0.5, 11), []event{{10, reinitBitrate("f", 5_000_000)}})
		admin := protocol.QualityHint{Reason: protocol.HintReasonAdmin, MaxBitrate: 5_000_000,
			Encodings: encs(5_000_000, 60, true, true)}
		checkActions(t, "the admin's total", c.Hint(admin, at(12)))
		checkEvents(t, "ticks", tickRange(c, 12.5, 30), []event{{20, reinitBitrate("f", 4_700_000)}})
	})
	t.Run("the last step up to the ceiling below 30 percent", func(t *testing.T) {
		// README WF6: a re-init full layer less than 30 % below its ceiling when the server lifts the cap (WF5's D10
		// runs ended at 6.57 and 7.00 Mbit/s for good) steps up to it, 10 s after its last change.
		c := ramped(t, fReinit) // the one step at 10
		checkActions(t, "T = 7.0 Mbit/s: 16 % below", c.Hint(congestion(7_000_000), at(12)))
		checkEvents(t, "ticks", tickRange(c, 12.5, 25), []event{{20, reinitBitrate("f", 6_700_000)}})
		// The hold ended at 22; once the cap is gone the ramp steps to the ceiling at the next Tick: 19 % up.
		checkActions(t, "cap lifted", c.Hint(congestion(0), at(25.2)))
		checkEvents(t, "ticks", tickRange(c, 25.5, 40), []event{{30, reinitBitrate("f", 8_000_000)}})
		if st := c.State(); st.Ramping || st.Applied["f"] != 8_000_000 {
			t.Errorf("Ramping %v, f applied %d; want the ramp done at 8000000", st.Ramping, st.Applied["f"])
		}

		// A cap that gives f its ceiling is the same step: 7.0 → 8 Mbit/s is 14.3 %.
		c = ramped(t, fReinit)
		checkActions(t, "T = 7.3 Mbit/s", c.Hint(congestion(7_300_000), at(12)))
		checkEvents(t, "ticks", tickRange(c, 12.5, 25), []event{{20, reinitBitrate("f", 7_000_000)}})
		checkActions(t, "T = 8.5 Mbit/s: f's share is its ceiling", c.Hint(congestion(8_500_000), at(25.2)))
		checkEvents(t, "ticks", tickRange(c, 25.5, 40), []event{{30, reinitBitrate("f", 8_000_000)}})
	})
	t.Run("the one step to a ceiling below 30 percent", func(t *testing.T) {
		// The admin's cap of 5000 kbit/s: the one step goes 4 → 5 Mbit/s, 25 %, to the ceiling: it applies.
		c := newController(fReinit, presetParams(protocol.PresetAuto, 5000))
		c.Live(at(0))
		checkEvents(t, "ticks", tickRange(c, 0.5, 40), []event{{10, reinitBitrate("f", 5_000_000)}})
		if st := c.State(); st.Applied["f"] != 5_000_000 || st.Ramping {
			t.Errorf("applied %d, ramping %v; want 5000000, the ramp done", st.Applied["f"], st.Ramping)
		}
	})
	t.Run("a step to a cap below 30 percent, then the cap lifted", func(t *testing.T) {
		// A congestion cap of 5.3 Mbit/s before the one step: the step to its split, 5.0, is 25 % and not to the
		// ceiling, so it never applies; once the server lifts the cap, the ramp steps to the ceiling at the next Tick,
		// which is 100 % above the start.
		c := newController(fReinit, autoParams)
		c.Live(at(0))
		checkActions(t, "T = 5.3 Mbit/s", c.Hint(congestion(5_300_000), at(5)))
		checkEvents(t, "ticks", tickRange(c, 5.5, 20), nil)
		if st := c.State(); st.Wanted["f"] != 5_000_000 || st.Applied["f"] != 4_000_000 {
			t.Errorf("wanted %d applied %d, want 5000000 waiting at 4000000", st.Wanted["f"], st.Applied["f"])
		}
		checkActions(t, "cap lifted", c.Hint(congestion(0), at(20.2)))
		checkEvents(t, "ticks", tickRange(c, 20.5, 40), []event{{20.5, reinitBitrate("f", 8_000_000)}})
	})
	t.Run("the one step waits for a limit", func(t *testing.T) {
		c := newController(fReinit, autoParams)
		c.Live(at(0))
		c.SetLimit("gpu", at(5))
		c.SetLimit("none", at(8))
		checkEvents(t, "ticks", tickRange(c, 0.5, 30), []event{{18, reinitBitrate("f", 8_000_000)}})
	})
}

func TestSetLiveSwitch(t *testing.T) {
	t.Run("live to re-init and back", func(t *testing.T) {
		c := ramped(t, bothLive) // f's last change at 10
		c.SetLive("f", false, at(12))
		checkActions(t, "22.5 % on a re-init encoder", c.Hint(congestion(6_500_000), at(13)))
		checkActions(t, "41 %, 2 s after the switch", c.Hint(congestion(5_000_000), at(14)))
		checkEvents(t, "ticks", tickRange(c, 14.5, 25), []event{{22, reinitBitrate("f", 4_700_000)}})

		// q stays live: it follows the live guards while f follows the re-init ones.
		checkActions(t, "600 kbit/s", c.Hint(congestion(600_000), at(30)), bitrate("q", 180_000))
		checkEvents(t, "ticks", tickRange(c, 30.5, 35), []event{{32, reinitBitrate("f", 420_000)}})

		// Back to live: 16.7 % 2 s after f's last change applies.
		c.SetLive("f", true, at(40))
		checkActions(t, "500 kbit/s", c.Hint(congestion(500_000), at(41)),
			bitrate("f", 350_000), bitrate("q", 150_000))
	})
	t.Run("q re-init, f live", func(t *testing.T) {
		c := ramped(t, qReinit)
		checkActions(t, "600 kbit/s", c.Hint(congestion(600_000), at(12)),
			bitrate("f", 420_000), reinitBitrate("q", 180_000))
		// 16.7 % down each: f's applies after its 2 s; q's too, since the cap forces it (README WF6), but only 10 s
		// after q's last change.
		checkActions(t, "500 kbit/s", c.Hint(congestion(500_000), at(15)), bitrate("f", 350_000))
		checkEvents(t, "ticks", tickRange(c, 15.5, 40), []event{{22, reinitBitrate("q", 150_000)}})
	})
	t.Run("q re-init: its share of a cap, then its ceiling", func(t *testing.T) {
		c := ramped(t, qReinit)
		// 800 kbit/s: q's share is 240 kbit/s, 20 % down; the cap forces it, and q never changed: at once.
		checkActions(t, "800 kbit/s", c.Hint(congestion(800_000), at(12)),
			bitrate("f", 560_000), reinitBitrate("q", 240_000))
		// 900 kbit/s: 270 kbit/s, 12.5 % up, is a rise the cap allows but doesn't force: it waits for 30 %. f (live,
		// its hold over) ramps to its new split at the next Tick.
		checkActions(t, "900 kbit/s", c.Hint(congestion(900_000), at(23)))
		checkEvents(t, "ticks", tickRange(c, 23.5, 30), []event{{23.5, bitrate("f", 630_000)}})
		if st := c.State(); st.Wanted["q"] != 270_000 || st.Applied["q"] != 240_000 {
			t.Errorf("q wanted %d applied %d, want 270000 waiting at 240000", st.Wanted["q"], st.Applied["q"])
		}
		// The server lifts the cap: q's way back to its ceiling is 25 % up, its last step there: at once.
		checkActions(t, "cap lifted", c.Hint(congestion(0), at(30.2)), reinitBitrate("q", 300_000))
	})
	t.Run("an unchanged flag is no change", func(t *testing.T) {
		c := ramped(t, fReinit) // the one step at 10
		c.SetLive("f", false, at(19))
		checkActions(t, "41 %", c.Hint(congestion(5_000_000), at(19.5)))
		checkEvents(t, "ticks", tickRange(c, 20, 21), []event{{20, reinitBitrate("f", 4_700_000)}})
	})
}

func TestOpActive(t *testing.T) {
	pause := protocol.QualityHint{Reason: protocol.HintReasonViewers, Encodings: encs(8_000_000, 60, false, true)}
	resume := protocol.QualityHint{Reason: protocol.HintReasonViewers, Encodings: encs(8_000_000, 60, true, true)}

	t.Run("at once, with the resume target", func(t *testing.T) {
		c := ramped(t, bothLive) // f's last change at 10
		checkActions(t, "pause", c.Hint(pause, at(10.5)), Action{Op: OpActive, Layer: "f", Active: false})
		checkEvents(t, "ticks while paused", tickRange(c, 11, 12), nil)
		checkActions(t, "resume", c.Hint(resume, at(12.2)),
			Action{Op: OpActive, Layer: "f", Active: true, Bps: 8_000_000})
		// The resume is f's change: a 22.5 % change waits 2 s from it.
		checkActions(t, "too early", c.Hint(congestion(6_500_000), at(13)))
		checkEvents(t, "ticks", tickRange(c, 13.5, 15), []event{{14.5, bitrate("f", 6_200_000)}})
	})
	t.Run("the resume target under a cap", func(t *testing.T) {
		c := ramped(t, fReinit)
		h := pause
		h.MaxBitrate = 3_000_000
		checkActions(t, "pause", c.Hint(h, at(12)), Action{Op: OpActive, Layer: "f", Active: false})
		if st := c.State(); st.Wanted["f"] != 2_700_000 || st.Wanted["q"] != 300_000 {
			t.Errorf("wanted %v, want f to resume at 2700000 and q at 300000", st.Wanted)
		}
		h = resume
		h.MaxBitrate = 3_000_000
		// Within the re-init encoder's 10 s and below nothing: a resume applies at once and costs a re-init.
		checkActions(t, "resume", c.Hint(h, at(13)),
			Action{Op: OpActive, Layer: "f", Active: true, Bps: 2_700_000, Reinit: true})
	})
	t.Run("wire order", func(t *testing.T) {
		c := ramped(t, bothLive)
		h := pause
		h.MaxBitrate = 200_000 // q alone: min(0.3, 0.2) = 0.2
		checkActions(t, "pause f and lower q", c.Hint(h, at(12)),
			Action{Op: OpActive, Layer: "f", Active: false}, bitrate("q", 200_000))
	})
}

func TestParams(t *testing.T) {
	t.Run("audio bitrate", func(t *testing.T) {
		c := ramped(t, bothLive)
		checkActions(t, "movie", c.Params(presetParams(protocol.PresetMovie, 0), at(12)), audioBitrate(256_000))
		checkActions(t, "the same again", c.Params(presetParams(protocol.PresetMovie, 0), at(13)))
		checkActions(t, "back to auto", c.Params(presetParams(protocol.PresetAuto, 0), at(13.5)), audioBitrate(128_000))
		if st := c.State(); st.Applied[media.LayerAudio] != 128_000 {
			t.Errorf("audio applied %d, want 128000", st.Applied[media.LayerAudio])
		}
	})
	t.Run("new ceilings apply at once", func(t *testing.T) {
		c := ramped(t, bothLive) // f's last change at 10
		checkActions(t, "0.5 s after a change", c.Params(presetParams(protocol.PresetAuto, 5000), at(10.5)),
			bitrate("f", 5_000_000))
		c = ramped(t, fReinit)
		checkActions(t, "re-init encoder", c.Params(presetParams(protocol.PresetAuto, 5000), at(10.5)),
			reinitBitrate("f", 5_000_000))
	})
	t.Run("a pending change keeps its guards", func(t *testing.T) {
		// Every M2 preset has the same ceilings (M1 02 §8.6): a preset change only changes the audio, and a change
		// waiting for its guards goes on waiting: here a decrease to the cap, for its interval.
		c := ramped(t, fReinit) // the one step at 10
		checkActions(t, "25 % down to the cap", c.Hint(congestion(6_300_000), at(12)))
		checkActions(t, "movie, 3 s after the one step", c.Params(presetParams(protocol.PresetMovie, 0), at(13)),
			audioBitrate(256_000))
		if st := c.State(); st.Applied["f"] != 8_000_000 || st.Wanted["f"] != 6_000_000 {
			t.Errorf("f applied %d wanted %d, want 6000000 still waiting at 8000000", st.Applied["f"], st.Wanted["f"])
		}
		checkEvents(t, "ticks", tickRange(c, 13.5, 40), []event{{20, reinitBitrate("f", 6_000_000)}})

		c = ramped(t, bothLive) // f's last change at 10
		checkActions(t, "12.5 %", c.Hint(congestion(7_300_000), at(10.5)))
		checkActions(t, "movie, 1 s after f's change", c.Params(presetParams(protocol.PresetMovie, 0), at(11)),
			audioBitrate(256_000))
		if st := c.State(); st.Applied["f"] != 8_000_000 {
			t.Errorf("f applied %d, want 8000000", st.Applied["f"])
		}
		// 22.5 % within 2 s of f's change still waits for the interval, too.
		checkActions(t, "22.5 %", c.Hint(congestion(6_500_000), at(11.2)))
		checkActions(t, "back to auto", c.Params(presetParams(protocol.PresetAuto, 0), at(11.5)),
			audioBitrate(128_000))
		checkEvents(t, "ticks", tickRange(c, 12, 13), []event{{12, bitrate("f", 6_200_000)}})
	})
	t.Run("only the layers whose ceiling changed", func(t *testing.T) {
		// q's change (24 %, 0.5 s after its last one) keeps its guards while f's new ceiling applies at once.
		c := ramped(t, bothLive)
		checkActions(t, "600 kbit/s", c.Hint(congestion(600_000), at(12)),
			bitrate("f", 420_000), bitrate("q", 180_000))
		checkActions(t, "800 kbit/s", c.Hint(congestion(800_000), at(12.2)))
		p := presetParams(protocol.PresetAuto, 0)
		p.Encodings[0].MaxBitrate = 400_000
		checkActions(t, "f's ceiling 400 kbit/s", c.Params(p, at(12.5)), bitrate("f", 400_000))
		checkEvents(t, "ticks", tickRange(c, 13, 15), []event{{14, bitrate("q", 240_000)}})
	})
}

func TestHintWithoutRateInfo(t *testing.T) {
	c := ramped(t, bothLive)
	before := c.State()
	codec := protocol.QualityHint{Reason: protocol.HintReasonCodec, Codec: protocol.CodecH264ConstrainedBaseline}
	checkActions(t, "codec hint", c.Hint(codec, at(12)))
	if !reflect.DeepEqual(c.State(), before) {
		t.Errorf("a codec hint changed the state: %+v → %+v", before, c.State())
	}
	// A cap alone: T = 3 Mbit/s, f 2.7.
	capOnly := protocol.QualityHint{Reason: protocol.HintReasonCongestion, MaxBitrate: 3_000_000}
	checkActions(t, "a cap without encodings", c.Hint(capOnly, at(12)), bitrate("f", 2_700_000))
	if c.State().Cap != 3_000_000 {
		t.Errorf("Cap = %d, want 3000000", c.State().Cap)
	}
}

func TestSetLimit(t *testing.T) {
	for _, limit := range []string{"cpu", "gpu"} {
		t.Run(limit, func(t *testing.T) {
			c := newController(bothLive, autoParams)
			c.Live(at(0))
			checkEvents(t, "first step", tickRange(c, 0.5, 2), []event{{2, bitrate("f", 4_600_000)}})
			c.SetLimit(limit, at(2.2))
			if !c.CPU(at(2.2)) || c.State().Limit != limit {
				t.Errorf("CPU %v, Limit %q; want true and %q", c.CPU(at(2.2)), c.State().Limit, limit)
			}
			checkEvents(t, "while limited", tickRange(c, 2.5, 7), nil)
			c.SetLimit("none", at(7.2))
			if c.CPU(at(7.2)) {
				t.Error("CPU after none")
			}
			// Held for 10 s after the limit cleared.
			checkEvents(t, "after none", tickRange(c, 7.5, 20.5),
				[]event{{17.5, bitrate("f", 5_290_000)}, {20.5, bitrate("f", 6_083_500)}})
		})
	}
	t.Run("other values", func(t *testing.T) {
		c := newController(bothLive, autoParams)
		c.SetLimit("none", at(0))
		c.SetLimit("source", at(0.2))
		if c.CPU(at(0.2)) {
			t.Error("CPU for source")
		}
		c.Live(at(0))
		checkEvents(t, "no hold without a limit", tickRange(c, 0.5, 2), []event{{2, bitrate("f", 4_600_000)}})
	})
}

func TestStateIsACopy(t *testing.T) {
	c := ramped(t, bothLive)
	st := c.State()
	st.Applied["f"] = 1
	st.Wanted["f"] = 1
	st.Ceiling["f"] = 1
	st.Active["f"] = false
	if again := c.State(); again.Applied["f"] != 8_000_000 || again.Wanted["f"] != 8_000_000 ||
		again.Ceiling["f"] != 8_000_000 || !again.Active["f"] {
		t.Errorf("State shares its maps: %+v", again)
	}
}

func TestOpString(t *testing.T) {
	names := map[Op]string{OpBitrate: "bitrate", OpActive: "active", OpAudioBitrate: "audio-bitrate", 9: "Op(9)"}
	for op, want := range names {
		if op.String() != want {
			t.Errorf("Op(%d).String() = %q, want %q", uint8(op), op.String(), want)
		}
	}
}
