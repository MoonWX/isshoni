package ratectl

import (
	"testing"

	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/protocol"
)

func TestSplitExamples(t *testing.T) {
	cases := []struct {
		name         string
		capBps       int64
		fOn, qOn     bool
		wantF, wantQ int
	}{
		{"a cap of 8 Mbit/s leaves f 7.7", 8_000_000, true, true, 7_700_000, 300_000},
		{"a cap of 600 kbit/s: q 180, f 420", 600_000, true, true, 420_000, 180_000},
		{"a cap below 400 kbit/s sends the floors", 300_000, true, true, FloorF, FloorQ},
		{"a cap of 1 bit/s sends the floors", 1, true, true, FloorF, FloorQ},
		{"no cap: the sum of the ceilings", 0, true, true, 8_000_000, 300_000},
		{"a huge cap is no limit", 1 << 50, true, true, 8_000_000, 300_000},
		{"q alone gets min(ceiling, T)", 200_000, false, true, 8_000_000, 200_000},
		{"q alone, no cap", 0, false, true, 8_000_000, 300_000},
		{"q alone, at least FloorQ", 100_000, false, true, 8_000_000, FloorQ},
		{"f alone gets min(ceiling, T)", 5_000_000, true, false, 5_000_000, 300_000},
		{"f alone, at least FloorF", 100_000, true, false, FloorF, 300_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ramped(t, bothLive)
			// The pause state goes with a viewers hint, so the hint holds nothing.
			c.Hint(protocol.QualityHint{Reason: protocol.HintReasonViewers, MaxBitrate: tc.capBps,
				Encodings: encs(8_000_000, 60, tc.fOn, tc.qOn)}, at(12))
			st := c.State()
			// A paused layer's Wanted is the target it resumes with: its share with both layers active.
			if tc.fOn && st.Wanted["f"] != tc.wantF {
				t.Errorf("f wanted %d, want %d", st.Wanted["f"], tc.wantF)
			}
			if tc.qOn && st.Wanted["q"] != tc.wantQ {
				t.Errorf("q wanted %d, want %d", st.Wanted["q"], tc.wantQ)
			}
			if st.Active["f"] != tc.fOn || st.Active["q"] != tc.qOn {
				t.Errorf("Active = %v", st.Active)
			}
		})
	}
}

func TestSplitTotal(t *testing.T) {
	f := &layer{rid: "f", full: true, ceiling: 8_000_000}
	q := &layer{rid: "q", ceiling: 300_000}
	cases := []struct {
		total int
		set   []*layer
		want  []int
	}{
		{8_000_000, []*layer{f, q}, []int{7_700_000, 300_000}},
		{600_000, []*layer{f, q}, []int{420_000, 180_000}},
		{300_000, []*layer{f, q}, []int{250_000, 150_000}},
		{20_000_000, []*layer{f, q}, []int{8_000_000, 300_000}},
		{600_000, []*layer{q, f}, []int{180_000, 420_000}}, // the order of the set doesn't matter
		{5_000_000, []*layer{f}, []int{5_000_000}},
		{9_000_000, []*layer{f}, []int{8_000_000}},
		{100_000, []*layer{q}, []int{150_000}},
	}
	for _, tc := range cases {
		got := splitTotal(tc.total, tc.set)
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitTotal(%d, %d layers) = %v, want %v", tc.total, len(tc.set), got, tc.want)
				break
			}
		}
	}
}

func TestClampsToEngineRanges(t *testing.T) {
	t.Run("high", func(t *testing.T) {
		p := protocol.ShareParams{Encodings: []protocol.Encoding{
			{RID: "f", Active: true, MaxBitrate: 60_000_000, MaxFramerate: 60},
			{RID: "q", Active: true, MaxBitrate: 3_000_000, MaxFramerate: 15},
		}, AudioBitrate: 600_000}
		c := New(Config{Live: bothLive, Params: p, StartBps: 80_000_000})
		checkActions(t, "Initial", c.Initial(),
			bitrate("f", MaxBitrateF), bitrate("q", MaxBitrateLayer), audioBitrate(MaxBitrateAudio))
		st := c.State()
		if st.Ceiling["f"] != MaxBitrateF || st.Ceiling["q"] != MaxBitrateLayer {
			t.Errorf("Ceiling = %v", st.Ceiling)
		}
	})
	t.Run("low", func(t *testing.T) {
		p := protocol.ShareParams{Encodings: []protocol.Encoding{
			{RID: "f", Active: true, MaxBitrate: 50_000, MaxFramerate: 60},
			{RID: "q", Active: true, MaxBitrate: 10_000, MaxFramerate: 15},
		}, AudioBitrate: 8_000}
		c := New(Config{Live: bothLive, Params: p})
		// f's ceiling is the range's bottom (100 k) but its split is at least FloorF; q is held at the range's
		// bottom by its ceiling; Opus at 32 k.
		checkActions(t, "Initial", c.Initial(),
			bitrate("f", FloorF), bitrate("q", MinBitrateLayer), audioBitrate(MinBitrateAudio))
		st := c.State()
		if st.Ceiling["f"] != MinBitrateF || st.Ceiling["q"] != MinBitrateLayer {
			t.Errorf("Ceiling = %v", st.Ceiling)
		}
		// A start below the range starts at its bottom.
		c = New(Config{Live: bothLive, Params: p, StartBps: 1})
		checkActions(t, "Initial with a start of 1 bit/s", c.Initial(),
			bitrate("f", MinBitrateF), bitrate("q", MinBitrateLayer), audioBitrate(MinBitrateAudio))
	})
	t.Run("a preset change", func(t *testing.T) {
		c := ramped(t, bothLive)
		p := presetParams(protocol.PresetMovie, 0)
		p.AudioBitrate = 1_000_000
		checkActions(t, "Params", c.Params(p, at(12)), audioBitrate(MaxBitrateAudio))
		if got := c.State().Applied[media.LayerAudio]; got != MaxBitrateAudio {
			t.Errorf("audio applied %d", got)
		}
	})
	t.Run("missing ceilings", func(t *testing.T) {
		p := protocol.ShareParams{Encodings: []protocol.Encoding{{RID: "f", Active: true}, {RID: "q", Active: true}}}
		c := New(Config{Params: p})
		if st := c.State(); st.Ceiling["f"] != MaxBitrateF || st.Ceiling["q"] != MaxBitrateLayer {
			t.Errorf("Ceiling = %v, want the ranges' tops", st.Ceiling)
		}
	})
	t.Run("unknown and repeated rids", func(t *testing.T) {
		c := ramped(t, bothLive)
		h := protocol.QualityHint{Reason: protocol.HintReasonAdmin, Encodings: []protocol.Encoding{
			{RID: "h", Active: true, MaxBitrate: 1_000_000},
			{RID: "f", Active: true, MaxBitrate: 6_000_000},
			{RID: "f", Active: false, MaxBitrate: 1_000_000},
			{RID: "q", Active: true, MaxBitrate: 300_000},
		}}
		checkActions(t, "admin ceiling", c.Hint(h, at(12)), bitrate("f", 6_000_000))
		if st := c.State(); len(st.Ceiling) != 2 || st.Ceiling["f"] != 6_000_000 || !st.Active["f"] {
			t.Errorf("State = %+v", st)
		}
	})
}

func TestAdminCap(t *testing.T) {
	admin := func(kbps int64) protocol.QualityHint {
		fMax := int64(8_000_000)
		if kbps > 0 && kbps*1000 < fMax {
			fMax = kbps * 1000
		}
		return protocol.QualityHint{Reason: protocol.HintReasonAdmin, MaxBitrate: kbps * 1000,
			Encodings: encs(fMax, 60, true, true)}
	}
	t.Run("at the start", func(t *testing.T) {
		c := newController(bothLive, presetParams(protocol.PresetAuto, 5000))
		checkActions(t, "Initial", c.Initial(), bitrate("f", 4_000_000), bitrate("q", 300_000), audioBitrate(128_000))
		c.Live(at(0))
		// The ramp stops at the ceiling: 4.6, then 5.0 (8.7 %, the step to the ceiling).
		checkEvents(t, "ramp", tickRange(c, 0.5, 30),
			[]event{{2, bitrate("f", 4_600_000)}, {5, bitrate("f", 5_000_000)}})
		if c.State().Ramping {
			t.Error("Ramping at the ceiling")
		}
	})
	t.Run("below the start", func(t *testing.T) {
		c := newController(bothLive, presetParams(protocol.PresetAuto, 2000))
		checkActions(t, "Initial", c.Initial(), bitrate("f", 2_000_000), bitrate("q", 300_000), audioBitrate(128_000))
		c.Live(at(0))
		checkEvents(t, "ticks", tickRange(c, 0.5, 30), nil)
		if c.State().Ramping {
			t.Error("Ramping above the ceiling")
		}
	})
	t.Run("mid-share, then lifted", func(t *testing.T) {
		c := ramped(t, bothLive)
		// T = 5 Mbit/s: f 4.7 under the cap, q 0.3.
		checkActions(t, "admin cap", c.Hint(admin(5000), at(12)), bitrate("f", 4_700_000))
		if st := c.State(); st.Cap != 5_000_000 || st.Ceiling["f"] != 5_000_000 {
			t.Errorf("Cap %d, f ceiling %d", st.Cap, st.Ceiling["f"])
		}
		// The admin's cap holds no ramp, but f applied 4.7 under it, so the ramp starts again from there: once the cap
		// is lifted, f ramps back up, a step every RampEvery, rather than jumping (§9.3: the ramp is the uplink's
		// probe).
		checkActions(t, "lifted", c.Hint(admin(0), at(14)))
		if st := c.State(); st.Cap != 0 || !st.Ramping {
			t.Errorf("Cap %d, Ramping %v", st.Cap, st.Ramping)
		}
		checkEvents(t, "the ramp back", tickRange(c, 14.5, 30), []event{
			{14.5, bitrate("f", 5_405_000)},
			{17.5, bitrate("f", 6_215_750)},
			{20.5, bitrate("f", 7_148_113)},
			{23.5, bitrate("f", 8_000_000)},
		})
	})
	t.Run("a raised cap is no ramp step", func(t *testing.T) {
		// The ramp at 6.0835 when an admin cap of 6000 kbit/s brings f to 5.7: the ramp starts again from 5.7, so
		// when the admin raises the cap to 6500, f doesn't jump to the old ramp (6.7 %, no ramp step) but ramps to the
		// new cap's split, 6.2, in one step (8.8 %, the ramp's step clamped at the cap).
		c := newController(bothLive, autoParams)
		c.Live(at(0))
		checkEvents(t, "the ramp", tickRange(c, 0.5, 8), []event{
			{2, bitrate("f", 4_600_000)}, {5, bitrate("f", 5_290_000)}, {8, bitrate("f", 6_083_500)},
		})
		checkActions(t, "admin 6000 kbit/s, 0.2 s after a change", c.Hint(admin(6000), at(8.2)))
		checkEvents(t, "down to the cap's split", tickRange(c, 8.5, 12), []event{{10, bitrate("f", 5_700_000)}})
		checkActions(t, "admin 6500 kbit/s", c.Hint(admin(6500), at(12.2)))
		checkEvents(t, "the ramp to the new cap", tickRange(c, 12.5, 22), []event{{12.5, bitrate("f", 6_200_000)}})
	})
	t.Run("a raised ceiling is no ramp step", func(t *testing.T) {
		// A ceiling alone (no cap) lowers the ramp in the same way: from 7 back up to 9, f ramps 7 → 8.05 → 9, and
		// never steps 14.3 % to the old ramp of 8.
		ceiling := func(fMax int64) protocol.QualityHint {
			return protocol.QualityHint{Reason: protocol.HintReasonAdmin, Encodings: encs(fMax, 60, true, true)}
		}
		c := ramped(t, bothLive)
		checkActions(t, "ceiling 7 Mbit/s", c.Hint(ceiling(7_000_000), at(12)), bitrate("f", 7_000_000))
		checkActions(t, "ceiling 9 Mbit/s", c.Hint(ceiling(9_000_000), at(20)))
		checkEvents(t, "the ramp to the new ceiling", tickRange(c, 20.5, 30),
			[]event{{20.5, bitrate("f", 8_050_000)}, {23.5, bitrate("f", 9_000_000)}})

		// The same from the start: f starts at its ceiling of 3.6, below DefaultStartBps, and the ramp with it.
		c = newController(bothLive, presetParams(protocol.PresetAuto, 3600))
		checkActions(t, "Initial", c.Initial(), bitrate("f", 3_600_000), bitrate("q", 300_000), audioBitrate(128_000))
		c.Live(at(0))
		checkEvents(t, "at the ceiling", tickRange(c, 0.5, 20), nil)
		checkActions(t, "ceiling 5 Mbit/s", c.Hint(ceiling(5_000_000), at(20)))
		checkEvents(t, "the ramp to the new ceiling", tickRange(c, 20.5, 30),
			[]event{{20.5, bitrate("f", 4_140_000)}, {23.5, bitrate("f", 4_761_000)}, {26.5, bitrate("f", 5_000_000)}})
	})
	t.Run("a small step down to the cap", func(t *testing.T) {
		// 7500 kbit/s: f must come down from 8 to 7.2, although that is only 10 %.
		c := ramped(t, bothLive)
		checkActions(t, "live encoder", c.Hint(admin(7500), at(12)), bitrate("f", 7_200_000))
		c = ramped(t, fReinit) // the one step at 10
		checkActions(t, "re-init encoder", c.Hint(admin(7500), at(12)))
		checkEvents(t, "ticks", tickRange(c, 12.5, 30), []event{{20, reinitBitrate("f", 7_200_000)}})
	})
}
