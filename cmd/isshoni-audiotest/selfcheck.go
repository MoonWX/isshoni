package main

import (
	"fmt"
	"io"
	"math"
)

// analyzerSelfCheck runs the portable analyzer over synthetic signals and fails if any of 05 §18.6's rules misbehave
// (docs/m2/05-windows-audio.md §18.2: selfcheck "runs the analyzer on synthetic signals"). It needs no engine, so it
// runs on every OS; the DLL and watcher part of selfcheck is the driver's (Windows only). It writes a line per check
// to w.
func analyzerSelfCheck(w io.Writer) error {
	// A full-scale 1 kHz sine is 0 dB; a −18 dBFS one reads about −18.
	tone := sineAt(2*sampleRate, freqGame, dbToAmp(-18))
	if got := toneLevelDB(tone, sampleRate, freqGame); math.Abs(got-(-18)) > 0.3 {
		return fmt.Errorf("1 kHz level %.2f dB, want about −18", got)
	}
	fmt.Fprintln(w, "  analyzer: Goertzel level ok")

	// Present/absent: a 1 kHz tone with a 440 Hz leak 30 dB down fails absent; 60 dB down passes.
	leak30 := mixTones(tone, sineAt(2*sampleRate, freqVoice, dbToAmp(-18-30)))
	if _, ab := presentAbsent(leak30, freqGame, freqVoice); ab {
		return fmt.Errorf("a 30 dB-down 440 Hz leak passed absent")
	}
	leak60 := mixTones(tone, sineAt(2*sampleRate, freqVoice, dbToAmp(-18-60)))
	if _, ab := presentAbsent(leak60, freqGame, freqVoice); !ab {
		return fmt.Errorf("a 60 dB-down 440 Hz leak failed absent")
	}
	fmt.Fprintln(w, "  analyzer: present/absent rule ok")

	// The short-window rule: a 20 ms burst of 440 Hz in a 10 s silent window fails absent though the whole-window
	// level is tiny.
	burst := make([]float64, 10*sampleRate)
	copyInto(burst, sineAt(int(0.02*sampleRate), freqVoice, dbToAmp(-18)), 3*sampleRate)
	if maxShort, _ := shortWindow(burst, sampleRate, freqVoice); maxShort <= absentMaxDB {
		return fmt.Errorf("a 20 ms 440 Hz burst read %.1f dB in the short window, want > %.0f", maxShort, absentMaxDB)
	}
	fmt.Fprintln(w, "  analyzer: short-window rule ok")

	// Clicks: a smooth tone has none; a mid-wave cut makes one.
	if ev := clickEvents(tone, tone, sampleRate, clickThreshold); len(ev) != 0 {
		return fmt.Errorf("a smooth tone flagged clicks at %v", ev)
	}
	cut := make([]float64, len(tone))
	copy(cut, tone)
	for i := sampleRate + 123; i < len(cut); i++ { // cut mid-wave (a non-zero phase makes a step)
		cut[i] = 0
	}
	if ev := clickEvents(cut, cut, sampleRate, clickThreshold); len(ev) == 0 {
		return fmt.Errorf("a mid-wave cut flagged no click")
	}
	fmt.Fprintln(w, "  analyzer: click detection ok")
	return nil
}

// presentAbsent reports whether freqP is present and freqA absent in the mono signal m (the §18.6 rule, with the
// short-window rule for absent).
func presentAbsent(m []float64, freqP, freqA float64) (present, absent bool) {
	lp := toneLevelDB(m, sampleRate, freqP)
	la := toneLevelDB(m, sampleRate, freqA)
	maxShort, _ := shortWindow(m, sampleRate, freqA)
	present = lp >= presentMinDB
	absent = la <= absentMaxDB && la <= lp-absentMarginDB && maxShort <= absentMaxDB
	return present, absent
}

// sineAt returns n samples of a sine at freq and linear amplitude amp.
func sineAt(n int, freq, amp float64) []float64 {
	x := make([]float64, n)
	for i := range x {
		x[i] = amp * math.Sin(2*math.Pi*freq*float64(i)/sampleRate)
	}
	return x
}

// mixTones sums two equal-length signals.
func mixTones(a, b []float64) []float64 {
	out := make([]float64, min(len(a), len(b)))
	for i := range out {
		out[i] = a[i] + b[i]
	}
	return out
}

// copyInto copies src into dst starting at off (clamped to dst's length).
func copyInto(dst, src []float64, off int) {
	for i := range src {
		if off+i >= len(dst) {
			return
		}
		dst[off+i] += src[i]
	}
}

// dbToAmp is the linear amplitude of a dBFS level.
func dbToAmp(db float64) float64 { return math.Pow(10, db/20) }
