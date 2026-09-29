package main

import (
	"math"
	"testing"
)

// A 44.1 kHz tap (common on USB DACs and some Bluetooth devices) must come out
// of the live read path as 48 kHz with the same tone and level.
func TestResample44100To48000(t *testing.T) {
	const inRate = 44100.0
	in := make([]float32, int(inRate)*2) // 1 s stereo
	for i := 0; i < len(in)/2; i++ {
		v := float32(0.25 * math.Sin(2*math.Pi*1000*float64(i)/inRate))
		in[2*i], in[2*i+1] = v, v
	}
	out := resample(in, inRate, 60000)
	frames := len(out) / 2
	if frames < 47000 || frames > 48000 || frames%chunkFrames != 0 {
		t.Fatalf("got %d frames at 48 kHz, want ~48000 in 480-frame chunks", frames)
	}
	mid := make([]float64, 24000) // skip the converter's start-up
	for i := range mid {
		mid[i] = float64(out[2*(12000+i)])
	}
	if got := toneLevelDB(mid, sampleRate, 1000); math.Abs(got-(-12.04)) > 0.5 {
		t.Fatalf("1 kHz after resampling: %.2f dB, want -12.04", got)
	}
	if got := toneLevelDB(mid, sampleRate, 1088); got > -60 { // 1000 Hz * 48000/44100: the tone if the rate were ignored
		t.Fatalf("energy at 1088 Hz (rate not converted?): %.2f dB", got)
	}
}
