package main

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

// TestToneWAV checks the fixture tone generator (05 §18.3): a −18 dBFS tone reads about −18 dBFS once read back,
// summed partials stay separable, and the 20 ms fades produce no click (S1's TestWAVRoundTripAndToneWAV).
func TestToneWAV(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tone.wav")
	if err := os.WriteFile(p, toneWAV([]float64{freqGame, freqVoice}, 0.125, 1), 0o600); err != nil {
		t.Fatal(err)
	}
	l, r, err := readWAV(p)
	if err != nil || len(l) == 0 || len(r) == 0 {
		t.Fatalf("read back %d/%d frames, err %v", len(l), len(r), err)
	}
	m := mono(l, r)
	if got := toneLevelDB(segment(m, sampleRate, 0.1, 0.9), sampleRate, freqGame); math.Abs(got-(-18)) > 0.5 {
		t.Fatalf("1000 Hz partial %.2f dB, want about −18", got)
	}
	if got := toneLevelDB(segment(m, sampleRate, 0.1, 0.9), sampleRate, freqVoice); math.Abs(got-(-18)) > 0.5 {
		t.Fatalf("440 Hz partial %.2f dB, want about −18", got)
	}
	padded := append(make([]float64, 100), l...)
	if ev := clickEvents(padded, padded, sampleRate, clickThreshold); len(ev) != 0 {
		t.Fatalf("tone WAV fades produce clicks at %v", ev)
	}
}

// TestWAVRoundTrip checks writeWAV/readWAV round-trip a 48 kHz float WAV (used by -keep-wav).
func TestWAVRoundTrip(t *testing.T) {
	dir := t.TempDir()
	inter := make([]float32, 2*sampleRate)
	for i := range sampleRate {
		v := float32(0.25 * math.Sin(2*math.Pi*freqGame*float64(i)/sampleRate))
		inter[2*i], inter[2*i+1] = v, v
	}
	p := filepath.Join(dir, "cap.wav")
	if err := writeWAV(p, inter); err != nil {
		t.Fatal(err)
	}
	l, r, err := readWAV(p)
	if err != nil || len(l) != sampleRate || len(r) != sampleRate {
		t.Fatalf("read back %d/%d frames, err %v", len(l), len(r), err)
	}
	if got := toneLevelDB(l, sampleRate, freqGame); math.Abs(got-(-12.04)) > 0.3 {
		t.Fatalf("round-trip level %.2f dB", got)
	}
}
