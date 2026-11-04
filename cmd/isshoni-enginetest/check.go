package main

import (
	"fmt"
	"sort"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// What a capture run recorded, and 04 §14's assertions over it. Everything here is portable, so the assertions are
// tested on every OS with synthetic runs (check_test.go).

// rec is one packet or event from Session.Next, in the order Next returned them.
type rec struct {
	Kind      media.Kind
	Layer     string
	Key       bool
	Reinit    bool
	CaptureNS int64 // the session clock (Session.Now's)
	RecvNS    int64 // Session.Now() when Next returned it
	Size      int
	Types     []uint8 // video: the NAL unit types
	SPSIDC    uint8   // video: the first SPS's profile_idc (0 without one)
	SPSFlags  uint8   // video: its constraint byte
	Event     *media.Event
}

// action is one control call the run made, at Session.Now() just before the call.
type action struct {
	Kind  string `json:"kind"` // "keyframe" | "bitrate"
	Layer string `json:"layer"`
	AtNS  int64  `json:"atNs"`
	Bps   int    `json:"bps,omitempty"`
}

// runLog is a whole capture run.
type runLog struct {
	StartNS   int64 // Session.Now() right after Start returned
	EndNS     int64 // Session.Now() when the run stopped reading
	Layers    []media.LayerConfig
	Profile   string // the requested profile key
	Test      bool   // a test:pattern source
	Tone      bool   // -audio tone
	Recs      []rec
	Actions   []action
	Stats     *media.EngineStats // sampled just before Stop
	GOPMS     int64              // f's GOP (the engine's default: 4000 ms; media.Config has no GOP field)
	StepsBase int                // f's bitrate before the first -bitrate-steps step
	Steps     []int              // -bitrate-steps' targets
}

const (
	firstPacketBound = 500 * time.Millisecond // 04 §14, §6.3
	keyframeSlack    = 5 * time.Millisecond   // -keyframes: one frame interval + 5 ms
	stepWindow       = 2 * time.Second        // -bitrate-steps: the rate over the 2 s after each applied step
	stepTolerance    = 0.25
	reinitSpacing    = 10 * time.Second // 04 §9.5: at most one re-init per 10 s per layer
	audioPacket      = 20 * time.Millisecond
)

func (l *runLog) fps(layer string) int {
	for _, c := range l.Layers {
		if c.RID == layer {
			return c.MaxFPS
		}
	}
	return 0
}

func interval(fps int) int64 {
	if fps <= 0 {
		return int64(time.Second)
	}
	return int64(time.Second) / int64(fps)
}

// layerEvent is the first EventLayer of a layer, or nil.
func (l *runLog) layerEvent(layer string) *media.Event {
	for i := range l.Recs {
		r := &l.Recs[i]
		if r.Kind == media.KindEvent && r.Event != nil && r.Event.Code == media.EventLayer && r.Event.Layer == layer {
			return r.Event
		}
	}
	return nil
}

// checkRun runs every assertion that applies to the run and returns the failures.
func checkRun(l *runLog) []string {
	var fails []string
	fails = append(fails, checkFirstPackets(l)...)
	fails = append(fails, checkKeyframeAUs(l)...)
	fails = append(fails, checkProfiles(l)...)
	fails = append(fails, checkCaptureTimes(l)...)
	fails = append(fails, checkAudioGrid(l)...)
	if l.Test {
		fails = append(fails, checkFPSOut(l)...)
	}
	fails = append(fails, checkKeyframeRequests(l)...)
	if len(l.Steps) > 0 {
		fails = append(fails, checkBitrateSteps(l)...)
	}
	if l.Test && l.Tone {
		fails = append(fails, checkToneFlash(l)...)
	}
	return fails
}

// Each layer's first packet is KEYFRAME|REINIT, preceded by its EventLayer, within 500 ms of Start returning.
func checkFirstPackets(l *runLog) []string {
	var fails []string
	for _, lc := range l.Layers {
		if lc.Inactive {
			continue
		}
		event := false
		found := false
		for _, r := range l.Recs {
			if r.Kind == media.KindEvent && r.Event != nil && r.Event.Code == media.EventLayer && r.Event.Layer == lc.RID {
				event = true
			}
			if r.Kind != media.KindVideo || r.Layer != lc.RID {
				continue
			}
			found = true
			if !r.Key || !r.Reinit {
				fails = append(fails, fmt.Sprintf("layer %s: the first packet is not KEYFRAME|REINIT", lc.RID))
			}
			if !event {
				fails = append(fails, fmt.Sprintf("layer %s: no EventLayer before the first packet", lc.RID))
			}
			if d := time.Duration(r.RecvNS - l.StartNS); d > firstPacketBound {
				fails = append(fails, fmt.Sprintf("layer %s: the first packet came %v after Start returned (≤ %v)",
					lc.RID, d.Round(time.Millisecond), firstPacketBound))
			}
			break
		}
		if !found {
			fails = append(fails, fmt.Sprintf("layer %s: no video packet", lc.RID))
		}
	}
	return fails
}

// Every keyframe access unit is SPS, PPS, IDR; no other access unit holds an IDR slice.
func checkKeyframeAUs(l *runLog) []string {
	bad := map[string]int{}
	for _, r := range l.Recs {
		if r.Kind != media.KindVideo {
			continue
		}
		hasIDR := false
		for _, t := range r.Types {
			hasIDR = hasIDR || t == nalIDR
		}
		if (r.Key && !isKeyframeAU(r.Types)) || (!r.Key && hasIDR) {
			bad[r.Layer]++
		}
	}
	var fails []string
	for layer, n := range bad {
		fails = append(fails, fmt.Sprintf("layer %s: %d access units break SPS, PPS, IDR (04 §9.4)", layer, n))
	}
	sort.Strings(fails)
	return fails
}

// The session runs the requested profile, and every SPS says so (100 for 6400; 66 with constraint byte e0 for 42e0).
func checkProfiles(l *runLog) []string {
	var fails []string
	for _, lc := range l.Layers {
		ev := l.layerEvent(lc.RID)
		if ev == nil {
			continue // checkFirstPackets reports it
		}
		if ev.Profile != l.Profile {
			fails = append(fails, fmt.Sprintf("layer %s: profile %s, asked for %s", lc.RID, ev.Profile, l.Profile))
		}
	}
	mismatch := map[string]int{}
	for _, r := range l.Recs {
		if r.Kind != media.KindVideo || r.SPSIDC == 0 {
			continue
		}
		switch {
		case l.Profile == "6400" && r.SPSIDC != 100,
			l.Profile == "42e0" && (r.SPSIDC != 66 || r.SPSFlags != 0xE0):
			mismatch[r.Layer]++
		}
	}
	for layer, n := range mismatch {
		fails = append(fails, fmt.Sprintf("layer %s: %d SPSs don't match profile %s", layer, n, l.Profile))
	}
	sort.Strings(fails)
	return fails
}

// Capture times are ≥ 0 and increase per stream (kind and layer).
func checkCaptureTimes(l *runLog) []string {
	type key struct {
		kind  media.Kind
		layer string
	}
	last := map[key]int64{}
	bad := map[key]string{}
	for _, r := range l.Recs {
		if r.Kind == media.KindEvent {
			continue
		}
		k := key{r.Kind, r.Layer}
		if _, seen := bad[k]; seen {
			continue
		}
		if r.CaptureNS < 0 {
			bad[k] = fmt.Sprintf("%v %q: a capture time before the session's epoch (%d ns)", r.Kind, r.Layer, r.CaptureNS)
			continue
		}
		if prev, ok := last[k]; ok && r.CaptureNS <= prev {
			bad[k] = fmt.Sprintf("%v %q: capture time %d ns after %d ns", r.Kind, r.Layer, r.CaptureNS, prev)
		}
		last[k] = r.CaptureNS
	}
	var fails []string
	for _, f := range bad {
		fails = append(fails, f)
	}
	sort.Strings(fails)
	return fails
}

// Audio packets are exactly 20 ms apart, except where a new grid starts (REINIT).
func checkAudioGrid(l *runLog) []string {
	var prev *rec
	off := 0
	for i := range l.Recs {
		r := &l.Recs[i]
		if r.Kind != media.KindAudio {
			continue
		}
		if prev != nil && !r.Reinit && r.CaptureNS-prev.CaptureNS != int64(audioPacket) {
			off++
		}
		prev = r
	}
	if off > 0 {
		return []string{fmt.Sprintf("audio: %d packets off the 20 ms grid", off)}
	}
	return nil
}

// test:pattern: fpsOut ≥ 90 % of each layer's target (the engine's stats at the end of the run).
func checkFPSOut(l *runLog) []string {
	if l.Stats == nil {
		return []string{"no stats sampled before Stop"}
	}
	var fails []string
	for _, s := range l.Stats.Layers {
		want := float64(l.fps(s.RID))
		if want > 0 && s.FPSOut < 0.9*want {
			fails = append(fails, fmt.Sprintf("layer %s: fpsOut %.1f < 90 %% of %v", s.RID, s.FPSOut, want))
		}
	}
	return fails
}

// -keyframes: every request is answered within one frame interval + 5 ms: the layer's first keyframe that arrives
// after the request was captured at most that long after it.
func checkKeyframeRequests(l *runLog) []string {
	var fails []string
	for _, a := range l.Actions {
		if a.Kind != "keyframe" {
			continue
		}
		bound := interval(l.fps(a.Layer)) + int64(keyframeSlack)
		answered := false
		for _, r := range l.Recs {
			if r.Kind != media.KindVideo || r.Layer != a.Layer || !r.Key || r.RecvNS <= a.AtNS {
				continue
			}
			if r.CaptureNS-a.AtNS > bound {
				fails = append(fails, fmt.Sprintf("layer %s: a keyframe request at %v answered by a frame captured %v later (≤ %v)",
					a.Layer, time.Duration(a.AtNS-l.StartNS).Round(time.Millisecond),
					time.Duration(r.CaptureNS-a.AtNS).Round(100*time.Microsecond), time.Duration(bound)))
			}
			answered = true
			break
		}
		if !answered && a.AtNS+bound < l.EndNS {
			fails = append(fails, fmt.Sprintf("layer %s: a keyframe request at %v was never answered", a.Layer,
				time.Duration(a.AtNS-l.StartNS).Round(time.Millisecond)))
		}
	}
	return fails
}

// appliedStep is a bitrate target in effect from AtNS on.
type appliedStep struct {
	AtNS int64
	Bps  int
}

// reinitSchedule is 04 §9.5's rule for a re-init encoder, applied to the run's steps: the first re-init after Start
// applies at once, later ones at most one per 10 s per layer (a request inside the window is stored and applied when
// it ends, the latest value winning), and a value equal to the current one is a no-op. It returns when each applied
// step should take effect (the REINIT packets then confirm the real times).
func reinitSchedule(base int, steps []action) []appliedStep {
	var out []appliedStep
	cur := base
	lastNS := int64(0)
	applied := false
	var pending *action
	apply := func(at int64, bps int) {
		if bps == cur {
			return
		}
		out = append(out, appliedStep{AtNS: at, Bps: bps})
		cur = bps
		lastNS = at
		applied = true
	}
	for i := range steps {
		s := steps[i]
		if pending != nil && s.AtNS >= lastNS+int64(reinitSpacing) {
			apply(lastNS+int64(reinitSpacing), pending.Bps)
			pending = nil
		}
		if !applied || s.AtNS >= lastNS+int64(reinitSpacing) {
			apply(s.AtNS, s.Bps)
			continue
		}
		pending = &steps[i]
	}
	if pending != nil {
		apply(lastNS+int64(reinitSpacing), pending.Bps)
	}
	return out
}

// -bitrate-steps on layer f (04 §9.5, §14): for a live encoder, no KEYFRAME or REINIT from the first step until 2 s
// after the last (a periodic GOP IDR, which carries no REINIT and comes a whole GOP after the previous keyframe, is
// tolerated: media.Config can't set f's GOP to 10 s), and the rate within ±25 % of each target over the 2 s after its
// step; for a re-init encoder, exactly one REINIT|KEYFRAME per applied step (the deferred ones included) and the rate
// within ±25 % over the 2 s after each REINIT.
func checkBitrateSteps(l *runLog) []string {
	var steps []action
	for _, a := range l.Actions {
		if a.Kind == "bitrate" && a.Layer == "f" {
			steps = append(steps, a)
		}
	}
	if len(steps) == 0 {
		return []string{"-bitrate-steps: no step was applied"}
	}
	ev := l.layerEvent("f")
	if ev == nil {
		return []string{"-bitrate-steps: no EventLayer for f"}
	}
	var fs []rec
	for _, r := range l.Recs {
		if r.Kind == media.KindVideo && r.Layer == "f" {
			fs = append(fs, r)
		}
	}
	var fails []string
	rateFrom := func(from int64, target int, what string) {
		if from+int64(stepWindow) > l.EndNS {
			fails = append(fails, fmt.Sprintf("-bitrate-steps: the run ended before 2 s after %s (raise -seconds)", what))
			return
		}
		bytes := 0
		for _, r := range fs {
			if r.CaptureNS >= from && r.CaptureNS < from+int64(stepWindow) {
				bytes += r.Size
			}
		}
		bps := float64(bytes) * 8 / stepWindow.Seconds()
		if bps < float64(target)*(1-stepTolerance) || bps > float64(target)*(1+stepTolerance) {
			fails = append(fails, fmt.Sprintf("-bitrate-steps: %.0f bit/s over the 2 s after %s, target %d (±25 %%)",
				bps, what, target))
		}
	}
	iv := interval(l.fps("f"))
	if ev.LiveBitrate {
		first, last := steps[0].AtNS, steps[len(steps)-1].AtNS+int64(stepWindow)
		prevKey := int64(-1)
		for _, r := range fs {
			if r.Key {
				periodic := !r.Reinit && prevKey >= 0 && l.GOPMS > 0 && r.CaptureNS-prevKey >= l.GOPMS*1e6-iv
				if r.CaptureNS >= first && r.CaptureNS <= last && !periodic {
					fails = append(fails, fmt.Sprintf("-bitrate-steps: live encoder: a KEYFRAME (reinit %t) at %v, between the steps",
						r.Reinit, time.Duration(r.CaptureNS).Round(time.Millisecond)))
				}
				prevKey = r.CaptureNS
			}
		}
		for i, s := range steps {
			rateFrom(s.AtNS, s.Bps, fmt.Sprintf("step %d", i+1))
		}
		return fails
	}
	want := reinitSchedule(l.StepsBase, steps)
	var reinits []rec
	for _, r := range fs {
		if r.Reinit && r.CaptureNS+iv >= steps[0].AtNS {
			reinits = append(reinits, r)
			if !r.Key {
				fails = append(fails, "-bitrate-steps: a REINIT packet that is no keyframe")
			}
		}
	}
	if len(reinits) != len(want) {
		fails = append(fails, fmt.Sprintf("-bitrate-steps: re-init encoder: %d REINIT|KEYFRAME packets, want one per applied step (%d)",
			len(reinits), len(want)))
	}
	for i := range min(len(reinits), len(want)) {
		rateFrom(reinits[i].CaptureNS, want[i].Bps, fmt.Sprintf("applied step %d (re-init)", i+1))
	}
	if len(want) > 0 && want[len(want)-1].AtNS+int64(stepWindow) > l.EndNS {
		fails = append(fails, fmt.Sprintf("-bitrate-steps: the last deferred step applies at %v; -seconds must cover it and 2 s more",
			time.Duration(want[len(want)-1].AtNS-l.StartNS).Round(time.Second)))
	}
	return fails
}

// -audio tone on test:pattern: every beep packet's capture time equals a flash frame's within one frame interval. The
// beeps start at the session's epoch + k s (04 §10.3), the audio packet that starts there; the flash frame is f's
// frame captured nearest to that instant.
func checkToneFlash(l *runLog) []string {
	iv := interval(l.fps("f"))
	var audio, video []int64
	for _, r := range l.Recs {
		switch {
		case r.Kind == media.KindAudio:
			audio = append(audio, r.CaptureNS)
		case r.Kind == media.KindVideo && r.Layer == "f":
			video = append(video, r.CaptureNS)
		}
	}
	if len(audio) == 0 || len(video) == 0 {
		return []string{"-audio tone: no audio or no f packets"}
	}
	var fails []string
	for k := int64(1); k*int64(time.Second)+int64(time.Second)/2 < l.EndNS; k++ {
		at := k * int64(time.Second)
		beep := int64(-1)
		for _, c := range audio {
			if c <= at && at < c+int64(audioPacket) {
				beep = c
				break
			}
		}
		if beep < 0 {
			continue // the packet was dropped or the run ended
		}
		best := video[0]
		for _, c := range video {
			if abs(c-at) < abs(best-at) {
				best = c
			}
		}
		if abs(beep-best) > iv {
			fails = append(fails, fmt.Sprintf("-audio tone: the beep at %d s is %v from its flash frame (≤ %v)", k,
				time.Duration(abs(beep-best)), time.Duration(iv)))
		}
	}
	return fails
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
