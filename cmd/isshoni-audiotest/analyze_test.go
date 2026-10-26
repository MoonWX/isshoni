package main

import (
	"fmt"
	"math"
	"slices"
	"testing"
)

// These tests are 05 §19.1's analyzer tests on synthetic recordings: Goertzel levels, the present/absent rule, the
// short-window rule (a 20 ms 440 Hz burst FAILs absent; a faded 1000 Hz tone never trips isshoni's tone), clicks,
// and the measured cut and onset. They run on every OS.

func TestToneLevelMeasuresAmplitude(t *testing.T) {
	x := sineAt(sampleRate, freqGame, 0.25) // −12.04 dBFS
	if got := toneLevelDB(x, sampleRate, freqGame); math.Abs(got-(-12.04)) > 0.2 {
		t.Fatalf("1 kHz level %.2f dB, want −12.04", got)
	}
	if got := toneLevelDB(x, sampleRate, freqVoice); got > -80 {
		t.Fatalf("absent 440 Hz measured %.2f dB, want < −80", got)
	}
	y := sineAt(sampleRate, freqGame, dbToAmp(-18))
	if got := toneLevelDB(y, sampleRate, freqGame); math.Abs(got-(-18)) > 0.3 {
		t.Fatalf("−18 dBFS tone measured %.2f dB", got)
	}
}

func TestPresentAbsentRule(t *testing.T) {
	game := sineAt(2*sampleRate, freqGame, 0.25)
	// A 440 Hz leak only 30 dB down fails absent.
	leak := mixTones(game, sineAt(2*sampleRate, freqVoice, 0.25*dbToAmp(-30)))
	if p, a := presentAbsent(leak, freqGame, freqVoice); !p || a {
		t.Fatalf("30 dB down: want present + not-absent, got present=%v absent=%v", p, a)
	}
	// 60 dB down passes absent.
	leak = mixTones(game, sineAt(2*sampleRate, freqVoice, 0.25*dbToAmp(-60)))
	if p, a := presentAbsent(leak, freqGame, freqVoice); !p || !a {
		t.Fatalf("60 dB down: want present + absent, got present=%v absent=%v", p, a)
	}
}

// TestShortWindowBurstFailsAbsent is 05 §19.1's "a 20 ms burst of 440 Hz at −18 dBFS inside a 10 s window FAILs
// absent (the whole-window level alone would pass)".
func TestShortWindowBurstFailsAbsent(t *testing.T) {
	m := make([]float64, 10*sampleRate)
	burst := toneBurst(freqVoice, dbToAmp(-18), int(0.020*sampleRate))
	copyInto(m, burst, int(1.7*sampleRate)) // 17 % into the window

	// The whole-window Goertzel is tiny: the burst would pass -50 and P_min-40 on its own.
	whole := toneLevelDB(m, sampleRate, freqVoice)
	if whole > absentMaxDB {
		t.Fatalf("the whole-window level %.1f dB is not small; the test premise is wrong", whole)
	}
	// The short-window rule catches it.
	maxShort, _ := shortWindow(m, sampleRate, freqVoice)
	if maxShort <= absentMaxDB {
		t.Fatalf("the 20 ms burst read %.1f dB in the short window, want > %.0f (FAIL absent)", maxShort, absentMaxDB)
	}
	// And a full absent evaluation of a present 1 kHz + this 440 burst FAILs absent.
	mix := mixTones(m, sineAt(len(m), freqGame, dbToAmp(-18)))
	if _, a := presentAbsent(mix, freqGame, freqVoice); a {
		t.Fatalf("the burst passed a full absent check")
	}
}

// TestFadedToneNeverTripsSelf is 05 §19.1's "a −18 dBFS 1000 Hz tone with 5 ms and 10 ms linear fade-ins and
// fade-outs, and with its onsets, never trips a short-window absent check" at isshoni's own frequency; "a tone with
// S1's 20 ms WAV fades doesn't either". The tone sits inside silence, so the 50 ms windows span its onset and its
// cut, at every offset of the 10 ms hop (7-sample steps). The same holds for 683 Hz, the other tone heard in the
// cases where isshoni plays (4 and 26), and for the 797 Hz tab of case 10.
//
// The self check runs at freqSelf (1700 Hz), not 05 §18.6's 907 Hz: there a 5 ms linear onset of 1000 Hz reads about
// −44 dBFS, over the −50 dBFS limit (WA62a's measurement, the integrator's decision for group 6; cases.go). The
// whole-window and short-window maxima must stay below the limit with a margin of 10 dB.
func TestFadedToneNeverTripsSelf(t *testing.T) {
	for _, src := range []float64{freqGame, freqLateGame, freqBrowserTab} {
		for _, fadeMs := range []float64{5, 10, 20} {
			t.Run(fmt.Sprintf("%.0fHz-%.0fms", src, fadeMs), func(t *testing.T) {
				t.Parallel()
				// 200 ms of tone inside 400 ms: every 50 ms window spans silence, the onset, the steady tone or
				// the cut.
				tone := fadedTone(src, dbToAmp(-18), 0.2, fadeMs/1000)
				worst, worstAt := silenceFloorDB, 0.0
				for k := range int(shortHopSec*sampleRate) / 7 {
					m := make([]float64, int(0.4*sampleRate))
					copyInto(m, tone, int(0.1*sampleRate)+k*7)
					if whole := toneLevelDB(m, sampleRate, freqSelf); whole > absentMaxDB-10 {
						t.Fatalf("%d Hz whole-window %.1f dB is within 10 dB of the absent limit", freqSelf, whole)
					}
					if maxShort, atS := shortWindow(m, sampleRate, freqSelf); maxShort > worst {
						worst, worstAt = maxShort, atS
					}
				}
				if worst > absentMaxDB-10 {
					t.Fatalf("%d Hz short-window max %.1f dB at %.3f s is within 10 dB of the absent limit %.0f",
						freqSelf, worst, worstAt, absentMaxDB)
				}
			})
		}
	}
}

// TestSelfToneSpacing pins the group 6 decision: isshoni's own tone lies at least 200 Hz from the engine's 1000 Hz
// test tone and from every other tone a fixture plays or a case checks, and every other pair stays 05 §18.3's 90 Hz
// apart.
func TestSelfToneSpacing(t *testing.T) {
	tones := []float64{freqGame, 997 /* the startup probe's tone, 05 §11.2 */, freqBursts}
	for _, cs := range cases() {
		for _, f := range caseFreqs(cs) {
			if f != freqSelf && !slices.Contains(tones, f) {
				tones = append(tones, f)
			}
		}
	}
	for _, f := range tones {
		if d := math.Abs(f - freqSelf); d < selfSpacingHz {
			t.Errorf("isshoni's tone %d Hz is %.0f Hz from %.0f Hz, want ≥ %d", freqSelf, d, f, selfSpacingHz)
		}
	}
	plan := []float64{freqGame, freqLateGame, freqVoice, freqVoice2, freqBrowserTab, freqSelf}
	for i, a := range plan {
		for _, b := range plan[i+1:] {
			if math.Abs(a-b) < 90 {
				t.Errorf("tones %.0f and %.0f Hz are closer than 90 Hz", a, b)
			}
		}
	}
}

func TestClickDetection(t *testing.T) {
	x := mixTones(sineAt(sampleRate, freqGame, 0.25),
		mixTones(sineAt(sampleRate, 660, 0.25), sineAt(sampleRate, freqVoice, 0.25)))
	if ev := clickEvents(x, x, sampleRate, clickThreshold); len(ev) != 0 {
		t.Fatalf("smooth three-tone mix flagged clicks at %v", ev)
	}
	// Abrupt removal of a stream mid-wave, at a non-zero phase (a zero-crossing cut makes no step).
	y := append([]float64(nil), x...)
	cut := sampleRate/2 + 20
	tone := sineAt(sampleRate, 660, 0.25)
	for i := cut; i < len(y); i++ {
		y[i] -= tone[i]
	}
	if ev := clickEvents(y, y, sampleRate, clickThreshold); len(ev) != 1 || math.Abs(ev[0]-0.5) > 0.01 {
		t.Fatalf("want one click at 0.5 s, got %v", ev)
	}
	// The same with a 10 ms linear fade (what the mixer does): no click.
	z := append([]float64(nil), x...)
	for i := cut; i < len(z); i++ {
		g := math.Max(0, 1-float64(i-cut)/480)
		z[i] -= tone[i] * (1 - g)
	}
	if ev := clickEvents(z, z, sampleRate, clickThreshold); len(ev) != 0 {
		t.Fatalf("faded removal flagged clicks at %v", ev)
	}
}

// TestMeasuredCutOnset is 05 §19.1's "measured cut and onset of a synthetic step lie within 20 ms of the true time
// (cut late, onset early, as §18.6 says)".
func TestMeasuredCutOnset(t *testing.T) {
	fs := float64(sampleRate)
	// Onset: silent until 1.0 s, then a −18 dBFS 1 kHz tone to 2 s. Event at 0.95 s.
	onsetSig := make([]float64, 2*sampleRate)
	copyInto(onsetSig, fadedTone(freqGame, dbToAmp(-18), 1.0, 0.005), sampleRate)
	_, _, onsetS, ok := measuredCutOnset(onsetSig, fs, freqGame, 0.95)
	if !ok {
		t.Fatal("no onset found")
	}
	if onsetS < 1.0-0.02 || onsetS > 1.0 {
		t.Fatalf("onset at %.3f s, want in [0.980, 1.000] (at most 20 ms early)", onsetS)
	}

	// Cut: a −18 dBFS tone for 1.05 s then silence. Event at 1.0 s.
	cutSig := make([]float64, 2*sampleRate)
	copyInto(cutSig, fadedTone(freqGame, dbToAmp(-18), 1.05, 0.005), 0)
	cutS, cutOK, _, _ := measuredCutOnset(cutSig, fs, freqGame, 1.0)
	if !cutOK {
		t.Fatal("no cut found")
	}
	if cutS < 1.05 || cutS > 1.05+0.02 {
		t.Fatalf("cut at %.3f s, want in [1.050, 1.070] (at most 20 ms late)", cutS)
	}
}

// toneBurst is n samples of a sine at freq and linear amplitude amp (no fades: a worst-case click/leak probe).
func toneBurst(freq, amp float64, n int) []float64 {
	x := make([]float64, n)
	for i := range x {
		x[i] = amp * math.Sin(2*math.Pi*freq*float64(i)/sampleRate)
	}
	return x
}

// fadedTone is seconds of a sine at freq and amplitude amp with linear fades of fadeSec at both ends (the mixer's
// onset/offset shape; 05 §9.4, §9.5).
func fadedTone(freq, amp, seconds, fadeSec float64) []float64 {
	n := int(seconds * sampleRate)
	fade := int(fadeSec * sampleRate)
	if fade < 1 {
		fade = 1
	}
	x := make([]float64, n)
	for i := range x {
		g := 1.0
		if i < fade {
			g = float64(i) / float64(fade)
		} else if n-i < fade {
			g = float64(n-i) / float64(fade)
		}
		x[i] = amp * g * math.Sin(2*math.Pi*freq*float64(i)/sampleRate)
	}
	return x
}

// TestSilenceProblem is 05 §18.4's silence pre-check: a case frequency above −70 dBFS or an RMS above −60 dBFS fails
// it; digital silence and a faint off-frequency hum pass.
func TestSilenceProblem(t *testing.T) {
	freqs := []float64{freqGame, freqVoice}
	if p := silenceProblem(make([]float64, sampleRate), freqs); p != "" {
		t.Fatalf("digital silence: %s", p)
	}
	hum := sineAt(sampleRate, 50, dbToAmp(-75))
	if p := silenceProblem(hum, freqs); p != "" {
		t.Fatalf("a −75 dBFS 50 Hz hum: %s", p)
	}
	if p := silenceProblem(sineAt(sampleRate, freqVoice, dbToAmp(-65)), freqs); p == "" {
		t.Fatal("a −65 dBFS 440 Hz tone passed the pre-check")
	}
	if p := silenceProblem(sineAt(sampleRate, 3000, dbToAmp(-40)), freqs); p == "" {
		t.Fatal("a −40 dBFS 3 kHz tone (RMS −43 dBFS) passed the pre-check")
	}
	if p := silenceProblem(make([]float64, 100), freqs); p == "" {
		t.Fatal("too little audio passed the pre-check")
	}
}
