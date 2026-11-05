package main

import (
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

const ms = int64(time.Millisecond)

var (
	keyTypes   = []uint8{nalSPS, nalPPS, nalIDR}
	sliceTypes = []uint8{nalSlice}
)

// goodRun is a 5 s test:pattern run with the test tone: f at 60 fps, q at 15 fps, audio every 20 ms on the epoch's
// grid, keyframes at the start only, every video packet 5 ms after its capture.
func goodRun(live bool) *runLog {
	l := &runLog{
		StartNS: 10 * ms, EndNS: 5000 * ms, Layers: runLayers(8_000_000), Profile: "6400", Test: true, Tone: true,
		StepsBase: 4_000_000,
		Stats:     &media.EngineStats{Layers: []media.EngineLayerStats{{RID: "f", FPSOut: 59.5}, {RID: "q", FPSOut: 15}}},
	}
	for _, rid := range []string{"f", "q"} {
		l.Recs = append(l.Recs, rec{Kind: media.KindEvent, Event: &media.Event{Code: media.EventLayer, Layer: rid,
			Profile: "6400", Encoder: "nvenc", LiveBitrate: live}})
	}
	type ev struct {
		at int64
		r  rec
	}
	var evs []ev
	for _, layer := range []struct {
		rid string
		fps int64
	}{{"f", 60}, {"q", 15}} {
		for k := int64(0); k*int64(time.Second)/layer.fps < 4990*ms; k++ {
			c := k * int64(time.Second) / layer.fps
			// on the pattern's grid, plus the session's 20 µs RTP bump (04 §6.1)
			r := rec{Kind: media.KindVideo, Layer: layer.rid, CaptureNS: c + 20_000, RecvNS: c + 5*ms,
				Size: 8_000_000 / 8 / 60, Types: sliceTypes}
			if layer.rid == "q" {
				r.Size = 300_000 / 8 / 15
			}
			if k == 0 {
				r.Key, r.Reinit, r.Types, r.SPSIDC = true, true, keyTypes, 100
			}
			evs = append(evs, ev{c + 20_000, r})
		}
	}
	for c := int64(0); c < 4990*ms; c += 20 * ms {
		evs = append(evs, ev{c, rec{Kind: media.KindAudio, CaptureNS: c, RecvNS: c + 25*ms, Reinit: c == 0}})
	}
	// in capture order (the streams' order is what the checks care about)
	for i := 1; i < len(evs); i++ {
		for j := i; j > 0 && evs[j].at < evs[j-1].at; j-- {
			evs[j], evs[j-1] = evs[j-1], evs[j]
		}
	}
	for _, e := range evs {
		l.Recs = append(l.Recs, e.r)
	}
	return l
}

func fails(t *testing.T, l *runLog, want ...string) {
	t.Helper()
	got := checkRun(l)
	if len(want) == 0 {
		if len(got) != 0 {
			t.Errorf("unexpected failures: %q", got)
		}
		return
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			found = found || strings.Contains(g, w)
		}
		if !found {
			t.Errorf("no failure containing %q in %q", w, got)
		}
	}
}

func findVideo(l *runLog, layer string, captureNS int64) *rec {
	for i := range l.Recs {
		r := &l.Recs[i]
		if r.Kind == media.KindVideo && r.Layer == layer && r.CaptureNS >= captureNS {
			return r
		}
	}
	return nil
}

func TestCheckGoodRun(t *testing.T) {
	fails(t, goodRun(true))
}

func TestCheckFirstPacket(t *testing.T) {
	l := goodRun(true)
	r := findVideo(l, "q", 0)
	r.Reinit = false
	fails(t, l, "layer q: the first packet is not KEYFRAME|REINIT")
	l = goodRun(true)
	for i := range l.Recs {
		if l.Recs[i].Kind == media.KindVideo {
			l.Recs[i].RecvNS += 600 * ms
		}
	}
	fails(t, l, "after Start returned")
	l = goodRun(true)
	l.Recs = l.Recs[1:] // f's EventLayer gone
	fails(t, l, "layer f: no EventLayer before the first packet")
}

func TestCheckStreamShapes(t *testing.T) {
	l := goodRun(true)
	findVideo(l, "f", 100*ms).Types = []uint8{nalSlice, nalIDR}
	fails(t, l, "layer f: 1 access units break SPS, PPS, IDR")
	l = goodRun(true)
	findVideo(l, "f", 0).SPSIDC = 66
	fails(t, l, "SPSs don't match profile 6400")
	l = goodRun(true)
	findVideo(l, "q", 1000*ms).CaptureNS = 0
	fails(t, l, `video "q": capture time`)
	l = goodRun(true)
	for i := range l.Recs {
		if l.Recs[i].Kind == media.KindAudio && l.Recs[i].CaptureNS == 1000*ms {
			l.Recs[i].CaptureNS += ms
		}
	}
	fails(t, l, "audio: 2 packets off the 20 ms grid")
	l = goodRun(true)
	l.Stats.Layers[0].FPSOut = 50
	fails(t, l, "layer f: fpsOut 50.0 < 90 %")
}

func TestCheckKeyframeRequests(t *testing.T) {
	l := goodRun(true)
	l.Actions = []action{{Kind: "keyframe", Layer: "f", AtNS: 1000 * ms}}
	r := findVideo(l, "f", 1010*ms) // captured 16.7 ms after the request: within one interval + 5 ms
	r.Key, r.Types = true, keyTypes
	fails(t, l)
	r.Key, r.Types = false, sliceTypes
	late := findVideo(l, "f", 1030*ms) // 33 ms after it
	late.Key, late.Types = true, keyTypes
	fails(t, l, "answered by a frame captured")
	l = goodRun(true)
	l.Actions = []action{{Kind: "keyframe", Layer: "q", AtNS: 2000 * ms}}
	fails(t, l, "was never answered")
}

func stepRun(live bool) *runLog {
	l := goodRun(live)
	l.Tone = false
	l.EndNS = 25_000 * ms
	l.Steps = []int{8_000_000, 2_000_000, 8_000_000}
	// Stretch the run to 25 s of f only, sized by the target in effect.
	var recs []rec
	for _, r := range l.Recs {
		if r.Kind == media.KindEvent || (r.Kind == media.KindVideo && r.CaptureNS < 1000*ms) {
			recs = append(recs, r)
		}
	}
	l.Recs = recs
	l.Layers = l.Layers[:1]
	return l
}

func addF(l *runLog, from, to int64, bps int, keyAt map[int64]bool, reinitAt map[int64]bool) {
	for c := from; c < to; c += int64(time.Second) / 60 {
		r := rec{Kind: media.KindVideo, Layer: "f", CaptureNS: c, RecvNS: c + 5*ms, Size: bps / 8 / 60, Types: sliceTypes}
		for at := range keyAt {
			if c <= at && at < c+int64(time.Second)/60 {
				r.Key, r.Types = true, keyTypes
			}
		}
		for at := range reinitAt {
			if c <= at && at < c+int64(time.Second)/60 {
				r.Key, r.Reinit, r.Types = true, true, keyTypes
			}
		}
		l.Recs = append(l.Recs, r)
	}
}

func TestCheckLiveSteps(t *testing.T) {
	l := stepRun(true)
	l.EndNS = 9000 * ms
	l.Actions = []action{{Kind: "bitrate", Layer: "f", AtNS: 1000 * ms, Bps: 8_000_000},
		{Kind: "bitrate", Layer: "f", AtNS: 3500 * ms, Bps: 2_000_000},
		{Kind: "bitrate", Layer: "f", AtNS: 6000 * ms, Bps: 8_000_000}}
	l.Recs = l.Recs[:0]
	l.Recs = append(l.Recs, rec{Kind: media.KindEvent, Event: &media.Event{Code: media.EventLayer, Layer: "f",
		Profile: "6400", LiveBitrate: true}})
	addF(l, 20*ms, 1000*ms, 4_000_000, nil, map[int64]bool{20 * ms: true})
	addF(l, 1000*ms, 3500*ms, 8_000_000, nil, nil)
	addF(l, 3500*ms, 6000*ms, 2_000_000, nil, nil)
	addF(l, 6000*ms, 9000*ms, 8_000_000, nil, nil)
	if got := checkBitrateSteps(l); len(got) != 0 {
		t.Errorf("a good live run: %q", got)
	}
	// A periodic IDR between the steps fails too: capture runs f with stepsGOP (10 s, 04 §14), so none comes there;
	// one would mean the GOP didn't reach the engine.
	for i := range l.Recs {
		if r := &l.Recs[i]; r.Kind == media.KindVideo && r.CaptureNS >= 4020*ms && r.CaptureNS < 4020*ms+int64(time.Second)/60 {
			r.Key, r.Types = true, keyTypes
		}
	}
	if got := checkBitrateSteps(l); len(got) != 1 || !strings.Contains(got[0], "between the steps") {
		t.Errorf("a periodic IDR 4 s after the first keyframe: %q", got)
	}
	for i := range l.Recs {
		if r := &l.Recs[i]; r.Kind == media.KindVideo && r.CaptureNS >= 4020*ms && r.CaptureNS < 4020*ms+int64(time.Second)/60 {
			r.Key, r.Types = false, sliceTypes
		}
	}
	// An IDR right after a step (QSV's reset starting a new sequence) fails it.
	for i := range l.Recs {
		if r := &l.Recs[i]; r.Kind == media.KindVideo && r.CaptureNS >= 3500*ms && r.CaptureNS < 3520*ms {
			r.Key, r.Types = true, keyTypes
		}
	}
	if got := checkBitrateSteps(l); len(got) == 0 || !strings.Contains(got[0], "between the steps") {
		t.Errorf("an IDR after a live step: %q", got)
	}
}

func TestReinitSchedule(t *testing.T) {
	steps := []action{{AtNS: 1000 * ms, Bps: 8_000_000}, {AtNS: 3500 * ms, Bps: 2_000_000},
		{AtNS: 6000 * ms, Bps: 5_000_000}}
	got := reinitSchedule(4_000_000, steps)
	// 8M at once; 2M deferred, then replaced by 5M, applied at 11 s.
	if len(got) != 2 || got[0] != (appliedStep{1000 * ms, 8_000_000}) || got[1] != (appliedStep{11_000 * ms, 5_000_000}) {
		t.Errorf("schedule %+v", got)
	}
	// A deferred value equal to the current one is a no-op.
	steps[2].Bps = 8_000_000
	if got := reinitSchedule(4_000_000, steps); len(got) != 1 {
		t.Errorf("schedule with a no-op %+v", got)
	}
	// Steps 10 s apart each apply.
	spaced := []action{{AtNS: 1000 * ms, Bps: 8_000_000}, {AtNS: 11_000 * ms, Bps: 2_000_000}}
	if got := reinitSchedule(4_000_000, spaced); len(got) != 2 || got[1].AtNS != 11_000*ms {
		t.Errorf("spaced schedule %+v", got)
	}
}

func TestCheckReinitSteps(t *testing.T) {
	l := stepRun(false)
	l.Steps = []int{8_000_000, 2_000_000, 5_000_000}
	l.Actions = []action{{Kind: "bitrate", Layer: "f", AtNS: 1000 * ms, Bps: 8_000_000},
		{Kind: "bitrate", Layer: "f", AtNS: 3500 * ms, Bps: 2_000_000},
		{Kind: "bitrate", Layer: "f", AtNS: 6000 * ms, Bps: 5_000_000}}
	l.Recs = l.Recs[:0]
	l.Recs = append(l.Recs, rec{Kind: media.KindEvent, Event: &media.Event{Code: media.EventLayer, Layer: "f",
		Profile: "6400"}})
	addF(l, 20*ms, 1050*ms, 4_000_000, nil, map[int64]bool{20 * ms: true})
	addF(l, 1050*ms, 11_050*ms, 8_000_000, nil, map[int64]bool{1050 * ms: true})
	addF(l, 11_050*ms, 25_000*ms, 5_000_000, nil, map[int64]bool{11_050 * ms: true})
	if got := checkBitrateSteps(l); len(got) != 0 {
		t.Errorf("a good re-init run: %q", got)
	}
	// One REINIT too many (a re-init that ignored the 10 s spacing).
	for i := range l.Recs {
		if r := &l.Recs[i]; r.CaptureNS >= 3540*ms && r.CaptureNS < 3560*ms {
			r.Key, r.Reinit, r.Types = true, true, keyTypes
		}
	}
	if got := checkBitrateSteps(l); len(got) == 0 || !strings.Contains(strings.Join(got, "|"), "want one per applied step (2)") {
		t.Errorf("an extra re-init: %q", got)
	}
	// A run too short for the deferred step.
	l = stepRun(false)
	l.EndNS = 8000 * ms
	l.Actions = []action{{Kind: "bitrate", Layer: "f", AtNS: 1000 * ms, Bps: 8_000_000},
		{Kind: "bitrate", Layer: "f", AtNS: 3500 * ms, Bps: 2_000_000}}
	if got := checkBitrateSteps(l); !strings.Contains(strings.Join(got, "|"), "-seconds must cover it") {
		t.Errorf("a short run: %q", got)
	}
}

func TestCheckToneFlash(t *testing.T) {
	l := goodRun(true)
	// Move f's frames near 2 s away by 30 ms (more than one 16.7 ms interval).
	var recs []rec
	for _, r := range l.Recs {
		if r.Kind == media.KindVideo && r.Layer == "f" && r.CaptureNS > 1960*ms && r.CaptureNS < 2040*ms {
			continue
		}
		recs = append(recs, r)
	}
	l.Recs = recs
	fails(t, l, "the beep at 2 s")
}
