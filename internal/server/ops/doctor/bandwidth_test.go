package doctor

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

func session(people, sharing int, quality api.BandwidthQuality, preset api.BandwidthPreset, hours float64) api.BandwidthInput {
	return api.BandwidthInput{People: people, Sharing: sharing, Thumbnails: 8, Quality: quality, Preset: preset, Hours: hours}
}

// The worked examples of 04 §13.4.
func TestBandwidthExamples(t *testing.T) {
	// The plan's example: 10 people all sharing, each sees one share in focus and 8 thumbnails.
	// 8 + 8 × 0.3 + 0.128 = 10.528 Mbps per viewer, 105.28 Mbps of egress, ~99 GB on the wire in two hours.
	plan := Bandwidth(session(10, 10, api.BandwidthQuality1080p60, api.BandwidthPresetAuto, 2))
	if plan.PerViewerMbps.Sharer != 10.53 || plan.PerViewerMbps.Viewer != 10.53 {
		t.Errorf("plan example: per viewer %+v, want 10.53 Mbps", plan.PerViewerMbps)
	}
	if plan.EgressMediaMbps != 105.28 || math.Round(plan.EgressMediaMbps) != 105 {
		t.Errorf("plan example: egress %v Mbps, want 105.28 (105)", plan.EgressMediaMbps)
	}
	if plan.EgressWireMbps != 110.54 || plan.TransferPerSessionGB != 99.5 || math.Floor(plan.TransferPerSessionGB) != 99 {
		t.Errorf("plan example: wire %v Mbps, %v GB, want 110.54 and 99.5 (~99 GB)", plan.EgressWireMbps, plan.TransferPerSessionGB)
	}
	if plan.IngressMediaMbps != 84.28 {
		t.Errorf("plan example: ingress %v Mbps, want 84.28", plan.IngressMediaMbps)
	}

	// The M1 exit scenario: 5 people, 2 sharing, 1080p60. A sharer sees the other share (8.128 Mbps), a viewer both
	// (8.428): 2 × 8.128 + 3 × 8.428 = 41.54 Mbps, 43.617 on the wire, ~39 GB in two hours.
	exit := Bandwidth(DefaultBandwidthInput())
	want := api.BandwidthEstimate{
		Input:                session(5, 2, api.BandwidthQuality1080p60, api.BandwidthPresetAuto, 2),
		PerViewerMbps:        api.BandwidthPerViewer{Sharer: 8.13, Viewer: 8.43},
		EgressMediaMbps:      41.54,
		EgressWireMbps:       43.62,
		IngressMediaMbps:     16.86,
		TransferPerSessionGB: 39.3,
	}
	if exit != want {
		t.Errorf("exit scenario:\n got %+v\nwant %+v", exit, want)
	}
	if math.Round(exit.EgressMediaMbps*10)/10 != 41.5 || math.Round(exit.TransferPerSessionGB) != 39 {
		t.Errorf("exit scenario: %v Mbps and %v GB, want 41.5 Mbps and ~39 GB", exit.EgressMediaMbps, exit.TransferPerSessionGB)
	}

	// The JSON of GET /api/v1/admin/bandwidth (04 §13.4).
	data, err := json.Marshal(exit)
	if err != nil {
		t.Fatal(err)
	}
	const wantJSON = `{"input":{"people":5,"sharing":2,"thumbnails":8,"quality":"1080p60","preset":"auto","hours":2},` +
		`"perViewerMbps":{"sharer":8.13,"viewer":8.43},"egressMediaMbps":41.54,"egressWireMbps":43.62,` +
		`"ingressMediaMbps":16.86,"transferPerSessionGb":39.3}`
	if string(data) != wantJSON {
		t.Errorf("JSON:\n got %s\nwant %s", data, wantJSON)
	}
}

func TestBandwidthModel(t *testing.T) {
	q1080, auto := api.BandwidthQuality1080p60, api.BandwidthPresetAuto
	tests := []struct {
		name                   string
		in                     api.BandwidthInput
		sharer, viewer, egress float64
		ingress, gb            float64
	}{
		{"nobody shares", session(5, 0, q1080, auto, 2), 0, 0, 0, 0, 0},
		// The one sharer receives nothing; four viewers get the share and its audio.
		{"one share", session(5, 1, q1080, auto, 2), 0, 8.13, 32.51, 8.43, 30.7},
		{"alone and sharing", session(1, 1, q1080, auto, 2), 0, 8.13, 0, 8.43, 0},
		{"1440p60", session(5, 2, api.BandwidthQuality1440p60, auto, 2), 16.13, 16.43, 81.54, 32.86, 77.1},
		{"2160p60", session(5, 2, api.BandwidthQuality2160p60, auto, 2), 32.13, 32.43, 161.54, 64.86, 152.7},
		// Movie doubles the audio: 256 kbps.
		{"movie", session(5, 2, q1080, api.BandwidthPresetMovie, 2), 8.26, 8.56, 42.18, 17.11, 39.9},
		// Half the time, half the transfer.
		{"one hour", session(5, 2, q1080, auto, 1), 8.13, 8.43, 41.54, 16.86, 19.6},
		{"half an hour", session(5, 2, q1080, auto, 0.5), 8.13, 8.43, 41.54, 16.86, 9.8},
		// Thumbnails are capped: with T = 2, a viewer of 10 shares gets one in focus and two previews.
		{"few thumbnails", api.BandwidthInput{People: 12, Sharing: 10, Thumbnails: 2, Quality: q1080, Preset: auto, Hours: 2},
			8.73, 8.73, 104.74, 84.28, 99},
		{"no thumbnails", api.BandwidthInput{People: 12, Sharing: 10, Thumbnails: 0, Quality: q1080, Preset: auto, Hours: 2},
			8.13, 8.13, 97.54, 84.28, 92.2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Bandwidth(tt.in)
			if got.Input != tt.in {
				t.Errorf("input %+v, want it unchanged", got.Input)
			}
			if got.PerViewerMbps.Sharer != tt.sharer || got.PerViewerMbps.Viewer != tt.viewer {
				t.Errorf("per viewer %+v, want sharer %v, viewer %v", got.PerViewerMbps, tt.sharer, tt.viewer)
			}
			if got.EgressMediaMbps != tt.egress || got.IngressMediaMbps != tt.ingress || got.TransferPerSessionGB != tt.gb {
				t.Errorf("egress %v, ingress %v, %v GB; want %v, %v, %v", got.EgressMediaMbps, got.IngressMediaMbps,
					got.TransferPerSessionGB, tt.egress, tt.ingress, tt.gb)
			}
			if wire := math.Round(tt.egress*1.05*100) / 100; math.Abs(got.EgressWireMbps-wire) > 0.011 {
				t.Errorf("wire %v, want about %v (egress × 1.05)", got.EgressWireMbps, wire)
			}
		})
	}
}

// Bandwidth answers every input: what is out of range is brought into it, and the estimate says what was computed.
func TestBandwidthClamps(t *testing.T) {
	got := Bandwidth(api.BandwidthInput{People: -3, Sharing: 9, Thumbnails: -1, Quality: "720p30", Preset: "game", Hours: math.NaN()})
	want := api.BandwidthInput{Quality: api.BandwidthQuality1080p60, Preset: api.BandwidthPresetAuto}
	if got.Input != want || got.EgressMediaMbps != 0 || got.TransferPerSessionGB != 0 {
		t.Errorf("nonsense in: %+v", got)
	}
	got = Bandwidth(api.BandwidthInput{People: 1 << 40, Sharing: 1 << 40, Thumbnails: 1 << 40, Quality: api.BandwidthQuality2160p60,
		Preset: api.BandwidthPresetMovie, Hours: math.Inf(1)})
	if got.Input.People != MaxBandwidthPeople || got.Input.Sharing != MaxBandwidthPeople || got.Input.Thumbnails != MaxBandwidthThumbnails ||
		got.Input.Hours != MaxBandwidthHours {
		t.Errorf("huge input: %+v", got.Input)
	}
	if math.IsInf(got.TransferPerSessionGB, 0) || math.IsNaN(got.TransferPerSessionGB) || got.EgressMediaMbps <= 0 {
		t.Errorf("huge input: %+v", got)
	}
	if _, err := json.Marshal(got); err != nil {
		t.Errorf("the estimate of a huge input does not encode: %v", err)
	}
}

func TestBandwidthInputProblem(t *testing.T) {
	ok := DefaultBandwidthInput()
	if field, code, fine := BandwidthInputProblem(ok); !fine || field != "" || code != "" {
		t.Errorf("the default input: %q %q %v", field, code, fine)
	}
	edit := func(f func(*api.BandwidthInput)) api.BandwidthInput {
		in := ok
		f(&in)
		return in
	}
	tests := []struct {
		in          api.BandwidthInput
		field, code string
	}{
		{edit(func(in *api.BandwidthInput) { in.People = 0 }), "people", api.FieldOutOfRange},
		{edit(func(in *api.BandwidthInput) { in.People = MaxBandwidthPeople + 1 }), "people", api.FieldOutOfRange},
		{edit(func(in *api.BandwidthInput) { in.Sharing = -1 }), "sharing", api.FieldOutOfRange},
		{edit(func(in *api.BandwidthInput) { in.Sharing = 6 }), "sharing", api.FieldOutOfRange},
		{edit(func(in *api.BandwidthInput) { in.Thumbnails = -1 }), "thumbnails", api.FieldOutOfRange},
		{edit(func(in *api.BandwidthInput) { in.Thumbnails = MaxBandwidthThumbnails + 1 }), "thumbnails", api.FieldOutOfRange},
		{edit(func(in *api.BandwidthInput) { in.Quality = "720p30" }), "quality", api.FieldInvalid},
		{edit(func(in *api.BandwidthInput) { in.Preset = "" }), "preset", api.FieldInvalid},
		{edit(func(in *api.BandwidthInput) { in.Hours = 0 }), "hours", api.FieldOutOfRange},
		{edit(func(in *api.BandwidthInput) { in.Hours = math.NaN() }), "hours", api.FieldOutOfRange},
		{edit(func(in *api.BandwidthInput) { in.Hours = math.Inf(1) }), "hours", api.FieldOutOfRange},
	}
	for _, tt := range tests {
		if field, code, fine := BandwidthInputProblem(tt.in); fine || field != tt.field || code != tt.code {
			t.Errorf("%+v: %q %q %v, want %q %q", tt.in, field, code, fine, tt.field, tt.code)
		}
	}
	// The edges are in.
	for _, in := range []api.BandwidthInput{
		edit(func(in *api.BandwidthInput) { in.People, in.Sharing = 1, 1 }),
		edit(func(in *api.BandwidthInput) { in.People, in.Sharing = MaxBandwidthPeople, MaxBandwidthPeople }),
		edit(func(in *api.BandwidthInput) { in.Thumbnails = 0 }),
		edit(func(in *api.BandwidthInput) { in.Hours = MaxBandwidthHours }),
		edit(func(in *api.BandwidthInput) { in.Hours = 0.25 }),
	} {
		if field, _, fine := BandwidthInputProblem(in); !fine {
			t.Errorf("%+v: refused for %s", in, field)
		}
	}
	// Every quality and preset of the API has its rate: 8, 16 and 32 Mbps, and 128 and 256 kbps (04 §13.4).
	for q, want := range map[api.BandwidthQuality]int64{
		api.BandwidthQuality1080p60: 8_000_000, api.BandwidthQuality1440p60: 16_000_000, api.BandwidthQuality2160p60: 32_000_000,
	} {
		if bps, ok := fullBps(q); !ok || bps != want {
			t.Errorf("quality %s: %d bps, want %d", q, bps, want)
		}
	}
	for p, want := range map[api.BandwidthPreset]int64{api.BandwidthPresetAuto: 128_000, api.BandwidthPresetMovie: 256_000} {
		if bps, ok := presetAudioBps(p); !ok || bps != want {
			t.Errorf("preset %s: %d bps, want %d", p, bps, want)
		}
	}
}

// FuzzBandwidth: whatever the input, the estimate is finite, not negative, and its parts fit together.
func FuzzBandwidth(f *testing.F) {
	f.Add(5, 2, 8, "1080p60", "auto", 2.0)
	f.Add(10, 10, 8, "1080p60", "auto", 2.0)
	f.Add(0, 0, 0, "", "", 0.0)
	f.Add(-1, 7, 1000, "2160p60", "movie", 1e300)
	f.Fuzz(func(t *testing.T, people, sharing, thumbnails int, quality, preset string, hours float64) {
		in := api.BandwidthInput{People: people, Sharing: sharing, Thumbnails: thumbnails,
			Quality: api.BandwidthQuality(quality), Preset: api.BandwidthPreset(preset), Hours: hours}
		got := Bandwidth(in)
		for name, v := range map[string]float64{
			"sharer": got.PerViewerMbps.Sharer, "viewer": got.PerViewerMbps.Viewer, "egress": got.EgressMediaMbps,
			"wire": got.EgressWireMbps, "ingress": got.IngressMediaMbps, "gb": got.TransferPerSessionGB,
		} {
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
				t.Fatalf("%+v: %s = %v", in, name, v)
			}
		}
		if got.EgressWireMbps < got.EgressMediaMbps || got.PerViewerMbps.Sharer > got.PerViewerMbps.Viewer {
			t.Fatalf("%+v: %+v", in, got)
		}
		if _, _, ok := BandwidthInputProblem(got.Input); !ok && got.Input.People > 0 && got.Input.Hours > 0 {
			t.Fatalf("%+v: the computed input %+v is not one the calculator takes", in, got.Input)
		}
		if _, _, ok := BandwidthInputProblem(in); ok && got.Input != in {
			t.Fatalf("a valid input was changed: %+v became %+v", in, got.Input)
		}
	})
}
