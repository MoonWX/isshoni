package ratectl

import (
	"fmt"
	"testing"

	"github.com/MoonWX/isshoni/internal/protocol"
)

func TestRequired(t *testing.T) {
	cases := []struct {
		h, fps int
		aspect float64
		want   int
	}{
		{1080, 60, 16.0 / 9, 6_220_800}, // 6.2 Mbit/s
		{720, 60, 16.0 / 9, 2_764_800},  // 2.8
		{540, 60, 16.0 / 9, 1_555_200},  // 1.6
		{360, 60, 16.0 / 9, 691_200},    // 0.7
		{1080, 30, 16.0 / 9, 3_110_400}, // half at 30 fps
		{1080, 60, 21.0 / 9, 8_164_800}, // ultrawide: 2520 × 1080
	}
	for _, tc := range cases {
		if got := required(tc.h, tc.fps, tc.aspect); got != tc.want {
			t.Errorf("required(%d, %d, %.3f) = %d, want %d", tc.h, tc.fps, tc.aspect, got, tc.want)
		}
	}
}

func TestApproxHeight(t *testing.T) {
	cases := []struct {
		bps, fps int
		want     int
	}{
		{8_000_000, 60, 1080},
		{6_220_800, 60, 1080},
		{6_220_799, 60, 900},
		{4_700_000, 60, 900},
		{2_764_800, 60, 720},
		{2_300_000, 60, 540},
		{1_555_200, 60, 540},
		{1_500_000, 60, 480},
		{700_000, 60, 360},
		{250_000, 60, 360}, // none fits: 360
		{2_300_000, 30, 900},
		{3_110_400, 30, 1080},
	}
	for _, tc := range cases {
		if got := approxHeight(tc.bps, tc.fps, 16.0/9); got != tc.want {
			t.Errorf("approxHeight(%d, %d) = %d, want %d", tc.bps, tc.fps, got, tc.want)
		}
	}
}

// uploadTimeline ticks every 500 ms from from through to and returns Upload after each Tick, by second.
func uploadTimeline(c *Controller, from, to float64) map[float64]UploadHint {
	out := map[float64]UploadHint{}
	for s := from; s <= to+1e-9; s += 0.5 {
		c.Tick(at(s))
		out[s] = c.Upload(at(s))
	}
	return out
}

func TestUploadHint(t *testing.T) {
	t.Run("on after 6 s of a binding cap, off 10 s after", func(t *testing.T) {
		c := ramped(t, bothLive)
		if c.Upload(at(10)).Limited {
			t.Fatal("limited at the ramp's end")
		}
		// T = 2.6 Mbit/s: f 2.3, about 540p at 60 fps. The first Tick that sees it is at 20.5.
		c.Hint(congestion(2_600_000), at(20.2))
		tl := uploadTimeline(c, 20.5, 40)
		for s := 20.5; s < 26.5; s += 0.5 {
			if tl[s].Limited {
				t.Errorf("limited at %gs, before 6 s of the condition", s)
			}
		}
		for s := 26.5; s <= 40; s += 0.5 {
			if want := (UploadHint{Limited: true, ApproxHeight: 540}); tl[s] != want {
				t.Errorf("Upload at %gs = %+v, want %+v", s, tl[s], want)
			}
		}
		// The server lifts the cap at 40.2; the last Tick that saw the condition was at 40.
		c.Hint(congestion(0), at(40.2))
		tl = uploadTimeline(c, 40.5, 49.5)
		for s := 40.5; s <= 49.5; s += 0.5 {
			if !tl[s].Limited || tl[s].ApproxHeight != 540 {
				t.Errorf("Upload at %gs = %+v, want it still on at 540", s, tl[s])
			}
		}
		// Between Ticks, too: it reads as off 10 s after the condition last held.
		if c.Upload(at(49.9)).Limited == false || c.Upload(at(50)).Limited {
			t.Errorf("Upload at 49.9 s = %+v and at 50 s = %+v, want on then off", c.Upload(at(49.9)),
				c.Upload(at(50)))
		}
		c.Tick(at(50))
		if got := c.Upload(at(50)); got != (UploadHint{}) {
			t.Errorf("Upload after the Tick at 50 s = %+v, want off", got)
		}
	})
	t.Run("never during the ramp", func(t *testing.T) {
		// No cap: the ramp starts at 4 Mbit/s (about 720p) and never shows the hint.
		c := newController(bothLive, autoParams)
		c.Live(at(0))
		for s, u := range uploadTimeline(c, 0.5, 30) {
			if u.Limited {
				t.Errorf("limited at %gs without a cap", s)
			}
		}
		// A cap above the ramp: the ramp binds until it reaches the cap's split (4.7 at 4 s), so the hint comes
		// 6 s after that, not 6 s after the cap.
		c = newController(bothLive, autoParams)
		c.Hint(protocol.QualityHint{Reason: protocol.HintReasonAdmin, MaxBitrate: 5_000_000,
			Encodings: encs(8_000_000, 60, true, true)}, at(-1))
		c.Live(at(0))
		tl := uploadTimeline(c, 0.5, 12)
		for s := 0.5; s < 10; s += 0.5 {
			if tl[s].Limited {
				t.Errorf("limited at %gs, while the ramp bounds f", s)
			}
		}
		if want := (UploadHint{Limited: true, ApproxHeight: 900}); tl[10] != want {
			t.Errorf("Upload at 10 s = %+v, want %+v", tl[10], want)
		}
	})
	t.Run("the condition", func(t *testing.T) {
		cases := []struct {
			name         string
			params       protocol.ShareParams
			targetHeight int
			capBps       int64
			fOn          bool
			want         UploadHint
		}{
			{"540p at 2.3 Mbit/s", autoParams, 1080, 2_600_000, true, UploadHint{true, 540}},
			{"the floors: 360p", autoParams, 1080, 300_000, true, UploadHint{true, 360}},
			{"text at 30 fps: 900p", presetParams(protocol.PresetText, 0), 1080, 2_600_000, true, UploadHint{true, 900}},
			{"a 720 px source at 2.3 Mbit/s", autoParams, 720, 2_600_000, true, UploadHint{true, 540}},
			{"a 540 px source at 2.3 Mbit/s", autoParams, 540, 2_600_000, true, UploadHint{}},
			{"enough for 1080p", autoParams, 1080, 7_000_000, true, UploadHint{}},
			{"no target height", autoParams, 0, 2_600_000, true, UploadHint{}},
			{"f paused", autoParams, 1080, 2_600_000, false, UploadHint{}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				c := New(Config{Live: bothLive, Params: tc.params, TargetHeight: tc.targetHeight})
				c.Live(at(0))
				tickRange(c, 0.5, 20)
				fps := 60
				if tc.params.Encodings[0].MaxFramerate > 0 {
					fps = tc.params.Encodings[0].MaxFramerate
				}
				c.Hint(protocol.QualityHint{Reason: protocol.HintReasonCongestion, MaxBitrate: tc.capBps,
					Encodings: encs(8_000_000, fps, tc.fOn, true)}, at(20.2))
				tickRange(c, 20.5, 40)
				if got := c.Upload(at(40)); got != tc.want {
					t.Errorf("Upload = %+v, want %+v (state %+v)", got, tc.want, c.State())
				}
			})
		}
	})
	t.Run("the height follows the applied full layer", func(t *testing.T) {
		c := ramped(t, bothLive)
		c.Hint(congestion(2_600_000), at(20.2))
		tickRange(c, 20.5, 30)
		// T = 1.6 Mbit/s: f 1.3, about 480p.
		c.Hint(congestion(1_600_000), at(30.2))
		tickRange(c, 30.5, 31)
		if got := c.Upload(at(31)); got != (UploadHint{true, 480}) {
			t.Errorf("Upload = %+v, want on at 480", got)
		}
	})
	t.Run("SetTargetHeight and SourceAspect", func(t *testing.T) {
		c := New(Config{Live: bothLive, Params: autoParams, SourceAspect: 21.0 / 9})
		c.Live(at(0))
		tickRange(c, 0.5, 10)
		// 4.7 Mbit/s at 21:9: 720p needs 3.6, 900p 5.7: about 720p.
		c.Hint(congestion(5_000_000), at(10.2))
		tickRange(c, 10.5, 20)
		if c.Upload(at(20)).Limited {
			t.Error("limited without a target height")
		}
		c.SetTargetHeight(1080)
		tickRange(c, 20.5, 26.5)
		if got := c.Upload(at(26.5)); got != (UploadHint{true, 720}) {
			t.Errorf("Upload = %+v, want on at 720", got)
		}
	})
}

func TestCPUHint(t *testing.T) {
	c := newController(bothLive, autoParams)
	for i, step := range []struct {
		limit string
		want  bool
	}{{"", false}, {"cpu", true}, {"gpu", true}, {"none", false}, {"gpu", true}, {"source", false}} {
		if step.limit != "" {
			c.SetLimit(step.limit, at(float64(i)))
		}
		if got := c.CPU(at(float64(i))); got != step.want {
			t.Errorf("step %d (%q): CPU = %v, want %v", i, step.limit, got, step.want)
		}
		if got := c.State().Limit; got != step.limit {
			t.Errorf("step %d: Limit = %q, want %q", i, got, step.limit)
		}
	}
}

func ExampleController_Upload() {
	c := New(Config{Live: map[string]bool{"f": true, "q": true}, Params: autoParams, TargetHeight: 1080})
	c.Initial()
	c.Live(at(0))
	for s := 0.5; s <= 10; s += 0.5 {
		c.Tick(at(s))
	}
	// The server's estimate: 2.6 Mbit/s for the video.
	for _, a := range c.Hint(congestion(2_600_000), at(20)) {
		fmt.Println(a.Op, a.Layer, a.Bps)
	}
	for s := 20.5; s <= 26.5; s += 0.5 {
		c.Tick(at(s))
	}
	fmt.Printf("%+v\n", c.Upload(at(26.5)))
	// Output:
	// bitrate f 2300000
	// {Limited:true ApproxHeight:540}
}
