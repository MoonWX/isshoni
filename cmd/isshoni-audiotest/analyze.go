package main

import "math"

// This file is the portable audio analyzer of docs/m2/05-windows-audio.md §18.6, ported from spike S1's analyze.go
// (spikes/s1-win-audio/analyze.go) and extended with the 50 ms short-window rule and the measured cut/onset. It runs
// on every OS and is the base of the analyzer and verdict tests (05 §19.1), so it has no build tag and no engine
// dependency: it takes recorded samples and returns levels and verdicts.
//
// Analysis runs on the PCM tap (pre-Opus, what the Opus encoder gets), mono (L + R)/2, at 48 kHz.

// sampleRate is the tap's sample rate (05 §18.6: 48 kHz).
const sampleRate = 48000

// The level thresholds of 05 §18.6 (the plan's "1 kHz present, 440 Hz at least 40 dB lower", made robust to
// ducking). A played test tone is about −18 dBFS (05 §18.3), less under a communications duck.
const (
	// presentMinDB is the floor of a "present" check: level ≥ −45 dBFS.
	presentMinDB = -45.0
	// absentMaxDB is the ceiling of an "absent" check: level ≤ −50 dBFS.
	absentMaxDB = -50.0
	// absentMarginDB is the second half of "absent": level ≤ P_min − 40 dB, P_min the lowest present level.
	absentMarginDB = 40.0
	// clickThreshold is the second-difference bound of a click (|x[i] − 2x[i−1] + x[i−2]| > 0.03). Five −18 dBFS
	// tones up to 1 kHz stay below 0.013.
	clickThreshold = 0.03
)

// The short-window rule's geometry (05 §18.6): 50 ms Hann windows hopped every 10 ms. A 50 ms Hann window's main
// lobe is ±40 Hz, so the closest tone pair (907 and 1000 Hz, 93 Hz apart) stays separable.
const (
	shortWindowSec = 0.050
	shortHopSec    = 0.010
)

// silenceFloorDB is the level the analyzer reports for a window with no signal (below it, the dB value is clamped so
// JSON stays finite).
const silenceFloorDB = -200.0

// toneLevelDB returns the level of a sine at freq in x (mono), in dBFS of its peak amplitude (a full-scale sine is
// 0 dB), with a Hann window (Goertzel). This is S1's toneLevelDB unchanged.
func toneLevelDB(x []float64, fs, freq float64) float64 {
	n := len(x)
	if n < 2 {
		return silenceFloorDB
	}
	coeff := 2 * math.Cos(2*math.Pi*freq/fs)
	var s1, s2, sumW float64
	for i, v := range x {
		w := 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n-1))
		sumW += w
		s0 := v*w + coeff*s1 - s2
		s2, s1 = s1, s0
	}
	power := s1*s1 + s2*s2 - coeff*s1*s2
	amp := 2 * math.Sqrt(math.Max(power, 0)) / sumW
	return 20 * math.Log10(math.Max(amp, 1e-10))
}

// shortWindow is the short-window rule of 05 §18.6: the maximum level of freq over 50 ms Hann windows hopped every
// 10 ms across x (only windows that lie fully inside it), and the offset in seconds (from x's start) of the window
// with that maximum. A short leak that a single whole-window Goertzel barely moves reads high here. x is mono.
func shortWindow(x []float64, fs, freq float64) (maxDB, atS float64) {
	n := len(x)
	win := int(math.Round(shortWindowSec * fs))
	hop := int(math.Round(shortHopSec * fs))
	if win < 2 || hop < 1 || n < win {
		return silenceFloorDB, 0
	}
	maxDB = silenceFloorDB
	for start := 0; start+win <= n; start += hop {
		db := toneLevelDB(x[start:start+win], fs, freq)
		if db > maxDB {
			maxDB, atS = db, float64(start)/fs
		}
	}
	return maxDB, atS
}

// clickEvents finds discontinuities on either channel: samples whose second difference exceeds threshold (a smooth
// mix of tones stays far below it), merged within 10 ms. Returns event times in seconds relative to the start of the
// channels. This is S1's clickEvents unchanged.
func clickEvents(left, right []float64, fs, threshold float64) []float64 {
	var events []float64
	last := -1
	for _, ch := range [][]float64{left, right} {
		for i := 2; i < len(ch); i++ {
			d2 := math.Abs(ch[i] - 2*ch[i-1] + ch[i-2])
			if d2 > threshold && (last < 0 || i-last > int(fs/100)) {
				events = append(events, float64(i)/fs)
				last = i
			}
		}
		last = -1
	}
	return dedupeTimes(events, 0.01)
}

func dedupeTimes(ts []float64, within float64) []float64 {
	var out []float64
	for _, t := range ts {
		dup := false
		for _, o := range out {
			dup = dup || math.Abs(o-t) < within
		}
		if !dup {
			out = append(out, t)
		}
	}
	return out
}

// segment returns [from, to) seconds of a channel (to ≤ 0 means the end). This is S1's segment unchanged.
func segment(ch []float64, fs, from, to float64) []float64 {
	a := int(from * fs)
	b := len(ch)
	if to > 0 && int(to*fs) < b {
		b = int(to * fs)
	}
	if a < 0 {
		a = 0
	}
	if a >= b {
		return nil
	}
	return ch[a:b]
}

// mono is (L + R)/2.
func mono(left, right []float64) []float64 {
	n := min(len(left), len(right))
	m := make([]float64, n)
	for i := range m {
		m[i] = (left[i] + right[i]) / 2
	}
	return m
}

// measuredCutOnset reports the measured cut and onset of freq in the mono channel m after the event at time tEvent
// (seconds from m's start), from 50 ms Hann windows each placed at its centre (05 §18.6): the cut is the centre of
// the last window after the event whose level is above −50 dBFS, and the onset the centre of the first window after
// the event at or above −45 dBFS. A missing value is reported as not-found. With −18 dBFS tones the centre reads a
// cut about 17 ms late and an onset about 15 ms early.
func measuredCutOnset(m []float64, fs, freq, tEvent float64) (cutS float64, cutOK bool, onsetS float64, onsetOK bool) {
	n := len(m)
	win := int(math.Round(shortWindowSec * fs))
	hop := int(math.Round(shortHopSec * fs))
	if win < 2 || hop < 1 || n < win {
		return 0, false, 0, false
	}
	half := float64(win) / 2 / fs
	for start := 0; start+win <= n; start += hop {
		centre := float64(start)/fs + half
		if centre < tEvent {
			continue
		}
		db := toneLevelDB(m[start:start+win], fs, freq)
		if db > absentMaxDB {
			cutS, cutOK = centre, true // keep the last such window: the cut
		}
		if db >= presentMinDB && !onsetOK {
			onsetS, onsetOK = centre, true // keep the first such window: the onset
		}
	}
	return cutS, cutOK, onsetS, onsetOK
}

// The silence pre-check's limits (05 §18.4): before a case, 1 s of endpoint loopback must have every case frequency
// at or below −70 dBFS and an overall RMS at or below −60 dBFS.
const (
	silenceToneMaxDB = -70.0
	silenceRMSMaxDB  = -60.0
)

// silenceProblem checks the silence pre-check on 1 s of endpoint loopback (mono): it returns "" when every frequency
// of freqs is at or below −70 dBFS and the RMS at or below −60 dBFS, else what was heard.
func silenceProblem(m []float64, freqs []float64) string {
	if len(m) < sampleRate/2 {
		return "the silence pre-check captured too little audio"
	}
	for _, f := range freqs {
		if db := toneLevelDB(m, sampleRate, f); db > silenceToneMaxDB {
			return "a case frequency is already playing"
		}
	}
	if db := rmsDB(m); db > silenceRMSMaxDB {
		return "the loopback is not silent"
	}
	return ""
}

// rmsDB is the RMS of x in dBFS (relative to full scale 1.0).
func rmsDB(x []float64) float64 {
	if len(x) == 0 {
		return silenceFloorDB
	}
	var sum float64
	for _, v := range x {
		sum += v * v
	}
	return 20 * math.Log10(math.Max(math.Sqrt(sum/float64(len(x))), 1e-10))
}
