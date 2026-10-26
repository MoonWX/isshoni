package main

import (
	"fmt"
	"math"
	"sort"
)

// This file scores the special cases of docs/m2/05-windows-audio.md §18.5 that the tone verdict can't: case 20
// (`timestamps`: PCM-tap onsets against im_test_render_bursts' predicted device-position times) and case 21 (`opus`:
// the decoded Opus stream against the tap). The Windows driver records and decodes; the scoring is portable, so it
// is tested on every OS.

// burstSearchSec is how far from a predicted burst time case 20 looks for the burst in the tap: the bursts are 1 s
// apart, so ±400 ms finds the right one even when the timestamps are off by far more than the 15 ms budget (a FAIL,
// never "too few bursts").
const burstSearchSec = 0.4

// burstThreshold is the sample magnitude that marks a burst's onset in the tap (the bursts play at 0.5).
const burstThreshold = 0.05

// burstErrorsMs pairs each predicted burst onset (session clock ns) with the first tap sample above burstThreshold
// within ±400 ms of it and returns the signed errors in ms (measured − predicted). m is the zero-padded tap timeline,
// whose sample 0 is at tap0 (session clock).
func burstErrorsMs(m []float64, tap0 int64, predicted []int64) []float64 {
	var errs []float64
	for _, ps := range predicted {
		centre := float64(ps-tap0) / 1e9
		lo := max(0, int((centre-burstSearchSec)*sampleRate))
		hi := min(len(m), int((centre+burstSearchSec)*sampleRate))
		for i := lo; i < hi; i++ {
			if math.Abs(m[i]) > burstThreshold {
				t := tap0 + int64(i)*1e9/sampleRate
				errs = append(errs, float64(t-ps)/1e6)
				break
			}
		}
	}
	return errs
}

// medianAbs is the median of the absolute values.
func medianAbs(xs []float64) float64 {
	abs := make([]float64, len(xs))
	for i, x := range xs {
		abs[i] = math.Abs(x)
	}
	sort.Float64s(abs)
	return abs[len(abs)/2]
}

// p95MinusP5 is the p95 − p5 spread.
func p95MinusP5(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	p := func(q float64) float64 { return s[min(int(q*float64(len(s))), len(s)-1)] }
	return p(0.95) - p(0.05)
}

// opusSpacing reports whether consecutive Opus packet capture times differ by exactly 20 ms within each timestamp grid
// (a Reinit packet starts a new grid, 05 §10.4) and returns the packet rate per second over dur.
func opusSpacing(pkts []opusPkt, dur float64) (bool, float64) {
	ok := len(pkts) > 1
	for i := 1; i < len(pkts); i++ {
		if !pkts[i].reinit && pkts[i].captureNS-pkts[i-1].captureNS != 20_000_000 {
			ok = false
		}
	}
	return ok, float64(len(pkts)) / dur
}

// scoreTimestamps is case 20's verdict (05 §18.5): median abs error ≤ 15 ms, p95 − p5 ≤ 5 ms, Opus packet spacing
// exactly 20 000 000 ns, 50 ± 1 packets/s. predicted are the bursts' onsets on the session clock.
func scoreTimestamps(cs caseSpec, cfg gateConfig, rec recording, predicted []int64) caseReport {
	if p := premiseProblem(cs, rec); p != "" {
		return inconclusive(cs, cfg, "%s", p)
	}
	if len(predicted) < 2 {
		return inconclusive(cs, cfg, "the bursts fixture logged no onsets")
	}
	cr := newCaseReport(cs, cfg)
	cr.Counters = cloneCounters(rec.counters)
	cr.Engine = enginesSeen(rec)
	errsMs := burstErrorsMs(mono(rec.left, rec.right), rec.tap0NS, predicted)
	fail := false
	if len(errsMs) < len(predicted) {
		cr.Problems = append(cr.Problems, fmt.Sprintf("%d of %d bursts were not found within ±%.0f ms",
			len(predicted)-len(errsMs), len(predicted), burstSearchSec*1000))
		fail = true
	}
	if len(errsMs) > 0 {
		medAbs, spread := medianAbs(errsMs), p95MinusP5(errsMs)
		cr.Measured["timestampMedianAbsMs"] = measure{ValueMs: int(math.Round(medAbs)), BudgetMs: 15}
		cr.Measured["timestampSpreadMs"] = measure{ValueMs: int(math.Round(spread)), BudgetMs: 5}
		if medAbs > 15 {
			cr.Problems = append(cr.Problems, fmt.Sprintf("median abs timestamp error %.1f ms > 15", medAbs))
			fail = true
		}
		if spread > 5 {
			cr.Problems = append(cr.Problems, fmt.Sprintf("timestamp spread %.1f ms > 5", spread))
			fail = true
		}
	}
	spacingOK, rate := opusSpacing(rec.opus, rec.recordDur)
	if !spacingOK {
		cr.Problems = append(cr.Problems, "Opus packet spacing is not exactly 20 ms")
		fail = true
	}
	if rate < 49 || rate > 51 {
		cr.Problems = append(cr.Problems, fmt.Sprintf("Opus rate %.1f packets/s, want 50 ± 1", rate))
		fail = true
	}
	result := resultPass
	if fail {
		result = resultFail
	}
	return withResult(cs, cfg, cr, result)
}

// opusWindowFrom is where case 21's levels start (s): past the encoder's start and the fixtures' settle.
const opusWindowFrom = 2.0

// scoreOpus is case 21's verdict (05 §18.5): the tap's 1000 Hz is present; the decoded 1000 Hz lies within ±1.5 dB of
// the tap's; 440 Hz is absent from the decoded audio by the full absent rule (whole window ≤ −50 dBFS and ≤ P_min −
// 40 dB, every 50 ms window ≤ −50 dBFS); the bitrate change at t = 4 s shows in the packet sizes; no packet after the
// first carries Reinit; and the fixtures' positive controls hold. decoded is the stereo-averaged output of every
// packet, whose sample 0 is at decoded0 (session clock).
func scoreOpus(cs caseSpec, cfg gateConfig, rec recording, decoded []float64, decoded0 int64) caseReport {
	if p := premiseProblem(cs, rec); p != "" {
		return inconclusive(cs, cfg, "%s", p)
	}
	if len(rec.opus) < 50 {
		return inconclusive(cs, cfg, "too few Opus packets (%d)", len(rec.opus))
	}
	cr := newCaseReport(cs, cfg)
	cr.Counters = cloneCounters(rec.counters)
	cr.Engine = enginesSeen(rec)
	cr.Events = redactEvents(rec.events, rec.roleApp)
	fail := false
	problem := func(format string, args ...any) {
		cr.Problems = append(cr.Problems, fmt.Sprintf(format, args...))
		fail = true
	}

	tap := segment(mono(rec.left, rec.right), sampleRate, opusWindowFrom, rec.recordDur)
	// The decoded audio over the same stretch of the tap's timeline.
	shift := float64(rec.tap0NS-decoded0) / 1e9
	dec := segment(decoded, sampleRate, opusWindowFrom+shift, rec.recordDur+shift)

	tap1000 := toneLevelDB(tap, sampleRate, freqGame)
	dec1000 := toneLevelDB(dec, sampleRate, freqGame)
	dec440 := toneLevelDB(dec, sampleRate, freqVoice)
	short440, shortAt := shortWindow(dec, sampleRate, freqVoice)
	cr.Checks = append(cr.Checks,
		checkReport{Freq: freqGame, From: opusWindowFrom, Expect: expectPresent, LevelDB: roundDB(dec1000),
			Pass: dec1000 >= presentMinDB && math.Abs(dec1000-tap1000) <= 1.5},
		checkReport{Freq: freqVoice, From: opusWindowFrom, Expect: expectAbsent, LevelDB: roundDB(dec440),
			MaxShortDB: roundDB(short440), MaxShortAtS: round3(opusWindowFrom + shortAt + shortWindowSec/2),
			Pass: dec440 <= absentMaxDB && dec440 <= dec1000-absentMarginDB && short440 <= absentMaxDB})
	if tap1000 < presentMinDB {
		problem("the tap's 1000 Hz is %.1f dB, not present", tap1000)
	}
	if math.Abs(dec1000-tap1000) > 1.5 {
		problem("decoded 1000 Hz %.1f dB vs tap %.1f dB (> 1.5 dB)", dec1000, tap1000)
	}
	for _, c := range cr.Checks {
		if !c.Pass {
			fail = true
		}
	}
	for _, c := range cs.checks {
		if p := rec.controlProblem(c, 1.5, rec.recordDur); p != "" {
			problem("%s", p)
		}
	}
	for i, p := range rec.opus {
		if i > 0 && p.reinit {
			problem("an IM_PKT_REINIT came after the first packet")
			break
		}
	}
	if !bitrateVisible(rec.opus, rec.tap0NS) {
		problem("the 128→256 kbps change is not visible in packet sizes")
	}
	result := resultPass
	if fail {
		result = resultFail
	}
	return withResult(cs, cfg, cr, result)
}

// bitrateVisible reports whether the mean Opus packet size after the change at t = 4 s (from 4.5 s on, past the
// encoder's ramp) exceeds the mean over [1 s, 4 s).
func bitrateVisible(pkts []opusPkt, tap0 int64) bool {
	var beforeSum, afterSum float64
	var beforeN, afterN int
	for _, p := range pkts {
		t := float64(p.captureNS-tap0) / 1e9
		switch {
		case t >= 1 && t < 4:
			beforeSum += float64(len(p.data))
			beforeN++
		case t >= 4.5:
			afterSum += float64(len(p.data))
			afterN++
		}
	}
	if beforeN == 0 || afterN == 0 {
		return false
	}
	return afterSum/float64(afterN) > beforeSum/float64(beforeN)
}
