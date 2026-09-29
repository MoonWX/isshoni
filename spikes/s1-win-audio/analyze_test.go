package main

import (
	"math"
	"path/filepath"
	"testing"
)

func sine(n int, freq, amp float64) []float64 {
	x := make([]float64, n)
	for i := range x {
		x[i] = amp * math.Sin(2*math.Pi*freq*float64(i)/sampleRate)
	}
	return x
}

func add(a, b []float64) []float64 {
	out := make([]float64, len(a))
	for i := range a {
		out[i] = a[i] + b[i]
	}
	return out
}

func TestToneLevelMeasuresAmplitude(t *testing.T) {
	x := sine(sampleRate, 1000, 0.25) // -12.04 dBFS
	if got := toneLevelDB(x, sampleRate, 1000); math.Abs(got-(-12.04)) > 0.2 {
		t.Fatalf("1 kHz level %.2f dB, want -12.04", got)
	}
	if got := toneLevelDB(x, sampleRate, 440); got > -80 {
		t.Fatalf("absent 440 Hz measured %.2f dB, want < -80", got)
	}
}

func TestEvaluatePresentAbsent(t *testing.T) {
	// "Game" at 1 kHz plus a voice-app leak at 440 Hz only 30 dB down: must fail.
	game := sine(2*sampleRate, 1000, 0.25)
	leak := sine(2*sampleRate, 440, 0.25*math.Pow(10, -30.0/20))
	m := add(game, leak)
	res, _ := evaluate(m, m, []check{{Freq: 1000, Expect: "present"}, {Freq: 440, Expect: "absent"}})
	if !res[0].Pass || res[1].Pass {
		t.Fatalf("want present pass + absent fail, got %+v", res)
	}
	// 60 dB down: passes.
	leak = sine(2*sampleRate, 440, 0.25*math.Pow(10, -60.0/20))
	m = add(game, leak)
	res, _ = evaluate(m, m, []check{{Freq: 1000, Expect: "present"}, {Freq: 440, Expect: "absent"}})
	if !res[0].Pass || !res[1].Pass {
		t.Fatalf("want both pass, got %+v", res)
	}
}

func TestClickDetection(t *testing.T) {
	x := add(sine(sampleRate, 1000, 0.25), add(sine(sampleRate, 660, 0.25), sine(sampleRate, 440, 0.25)))
	if ev := clickEvents(x, x, sampleRate, clickThreshold); len(ev) != 0 {
		t.Fatalf("smooth three-tone mix flagged clicks at %v", ev)
	}
	// Abrupt removal of a stream mid-wave (what a missing fade would produce),
	// at a non-zero phase of the removed tone (a zero-crossing cut makes no step).
	y := append([]float64(nil), x...)
	cut := sampleRate/2 + 20
	tone := sine(sampleRate, 660, 0.25)
	for i := cut; i < len(y); i++ {
		y[i] -= tone[i]
	}
	if ev := clickEvents(y, y, sampleRate, clickThreshold); len(ev) != 1 || math.Abs(ev[0]-0.5) > 0.01 {
		t.Fatalf("want one click at 0.5 s, got %v", ev)
	}
	// The same removal with a 10 ms linear fade (what the mixer does): no click.
	z := append([]float64(nil), x...)
	for i := cut; i < len(z); i++ {
		g := math.Max(0, 1-float64(i-cut)/480)
		z[i] -= tone[i] * (1 - g)
	}
	if ev := clickEvents(z, z, sampleRate, clickThreshold); len(ev) != 0 {
		t.Fatalf("faded removal flagged clicks at %v", ev)
	}
}

func TestWAVRoundTripAndToneWAV(t *testing.T) {
	dir := t.TempDir()
	// float32 writer
	inter := make([]float32, 2*sampleRate)
	for i := 0; i < sampleRate; i++ {
		v := float32(0.25 * math.Sin(2*math.Pi*1000*float64(i)/sampleRate))
		inter[2*i], inter[2*i+1] = v, v
	}
	p := filepath.Join(dir, "a.wav")
	if err := writeWAV(p, inter); err != nil {
		t.Fatal(err)
	}
	l, r, err := readWAV(p)
	if err != nil || len(l) != sampleRate || len(r) != sampleRate {
		t.Fatalf("read back %d/%d frames, err %v", len(l), len(r), err)
	}
	if got := toneLevelDB(l, sampleRate, 1000); math.Abs(got+12.04) > 0.2 {
		t.Fatalf("round-trip level %.2f", got)
	}
	// 16-bit tone generator used by the fake apps: correct level, no clicks at its fades.
	tw := filepath.Join(dir, "tone.wav")
	if err := writeFile(tw, toneWAV(440, 0.25, 1)); err != nil {
		t.Fatal(err)
	}
	l, r, err = readWAV(tw)
	if err != nil {
		t.Fatal(err)
	}
	if got := toneLevelDB(segment(l, sampleRate, 0.1, 0.9), sampleRate, 440); math.Abs(got+12.04) > 0.3 {
		t.Fatalf("tone WAV level %.2f", got)
	}
	padded := append(make([]float64, 100), l...)
	if ev := clickEvents(padded, padded, sampleRate, clickThreshold); len(ev) != 0 {
		t.Fatalf("tone WAV fades produce clicks at %v", ev)
	}
}
