package main

import (
	"math"
	"testing"
)

// These tests cover the scoring of cases 20 and 21 (special.go) on synthetic recordings.

// burstRec is a case-20 recording: 10 bursts of 5 ms at 0.5, one per second from 1 s, each lagging its predicted
// time by lagMs (+ a jitter of up to jitterMs), with clean Opus packets every 20 ms. tap0 is 7 s on the session
// clock.
func burstRec(lagMs, jitterMs float64) (recording, []int64) {
	const tap0 = 7_000_000_000
	dur := 13.0
	m := make([]float64, int(dur*sampleRate))
	var predicted []int64
	for k := range 10 {
		pt := 1.0 + float64(k)
		predicted = append(predicted, tap0+int64(pt*1e9))
		at := pt + (lagMs+jitterMs*float64(k%3-1))/1000
		copyInto(m, toneBurst(freqBursts, 0.5, int(0.005*sampleRate)), int(at*sampleRate))
	}
	rec := recording{left: m, right: m, recordDur: dur, tap0NS: tap0, reparented: true, roles: map[string]*roleTrack{},
		counters: map[string]int{}}
	for i := range int(dur * 50) {
		rec.opus = append(rec.opus, opusPkt{captureNS: tap0 + int64(i)*20_000_000, data: []byte{1}, reinit: i == 0})
	}
	return rec, predicted
}

// TestTimestampsScoring is case 20: onsets within budget PASS; a 50 ms timestamp error (more than the old ±30 ms
// search window) is found and FAILs on the 15 ms median, never "too few bursts"; a jittery spread FAILs.
func TestTimestampsScoring(t *testing.T) {
	cs := mustCase(t, "timestamps")
	cfg := gateConfig{FastPath: true}
	rec, predicted := burstRec(4, 0)
	cr := scoreTimestamps(cs, cfg, rec, predicted)
	if cr.Result != resultPass {
		t.Fatalf("4 ms lag: want PASS, got %s: %v", cr.Result, cr.Problems)
	}
	rec, predicted = burstRec(50, 0)
	cr = scoreTimestamps(cs, cfg, rec, predicted)
	if cr.Result != resultFail || cr.Measured["timestampMedianAbsMs"].ValueMs != 50 {
		t.Fatalf("50 ms lag: want FAIL measuring 50 ms, got %s %+v: %v", cr.Result, cr.Measured, cr.Problems)
	}
	rec, predicted = burstRec(4, 4)
	if cr := scoreTimestamps(cs, cfg, rec, predicted); cr.Result != resultFail {
		t.Fatalf("±4 ms jitter (8 ms spread): want FAIL, got %s", cr.Result)
	}
	// A new grid (Reinit) may jump; a jump inside a grid is not exactly 20 ms.
	rec, predicted = burstRec(4, 0)
	rec.opus[100].captureNS += 3_000_000
	if cr := scoreTimestamps(cs, cfg, rec, predicted); cr.Result != resultFail {
		t.Fatalf("a 23 ms packet step: want FAIL, got %s", cr.Result)
	}
	for i := 100; i < len(rec.opus); i++ {
		rec.opus[i].captureNS = rec.opus[99].captureNS + 30_000_000 + int64(i-100)*20_000_000
	}
	rec.opus[100].reinit = true
	if cr := scoreTimestamps(cs, cfg, rec, predicted); cr.Result != resultPass {
		t.Fatalf("a jump at a new grid: want PASS, got %s: %v", cr.Result, cr.Problems)
	}
	if cr := scoreTimestamps(cs, cfg, rec, nil); cr.Result != resultInconclusive {
		t.Fatalf("no onsets: want INCONCLUSIVE, got %s", cr.Result)
	}
}

// opusRec is a case-21 recording: the tap holds the game at 1000 Hz, the packets grow at 4 s, and the decoded audio
// (returned) is the tap plus leak440 of 440 Hz.
func opusRec(leak440 float64) (recording, []float64) {
	dur := 8.0
	game := fadedTone(freqGame, dbToAmp(-18), dur, 0.02)
	rec := baseRec(game, dur, map[string]*roleTrack{
		"game":    trackActive("heard", "", dur),
		"discord": trackActive("kept_out", "builtin", dur),
	}, "include-set")
	rec.tap0NS = 1_000_000_000
	for i := range int(dur * 50) {
		size := 320
		if float64(i)*0.02 >= 4.1 {
			size = 640
		}
		rec.opus = append(rec.opus, opusPkt{captureNS: rec.tap0NS + int64(i)*20_000_000,
			data: make([]byte, size), reinit: i == 0})
	}
	decoded := append([]float64(nil), game...)
	if leak440 > 0 {
		decoded = mixTones(decoded, sineAt(len(decoded), freqVoice, leak440))
	}
	return rec, decoded
}

// TestOpusScoring is case 21: a faithful encode PASSes; 440 Hz in the decoded audio, a decode far from the tap's
// level, a REINIT after the first packet, an invisible bitrate change, or a silent tap FAIL.
func TestOpusScoring(t *testing.T) {
	cs := mustCase(t, "opus")
	cfg := gateConfig{FastPath: true}
	rec, dec := opusRec(0)
	if cr := scoreOpus(cs, cfg, rec, dec, rec.tap0NS); cr.Result != resultPass {
		t.Fatalf("want PASS, got %s: %v", cr.Result, cr.Problems)
	}
	rec, dec = opusRec(dbToAmp(-40))
	if cr := scoreOpus(cs, cfg, rec, dec, rec.tap0NS); cr.Result != resultFail {
		t.Fatalf("440 Hz at −40 dBFS in the decode: want FAIL, got %s", cr.Result)
	}
	rec, dec = opusRec(0)
	for i := range dec {
		dec[i] *= 0.7 // −3.1 dB
	}
	if cr := scoreOpus(cs, cfg, rec, dec, rec.tap0NS); cr.Result != resultFail {
		t.Fatalf("decode 3 dB low: want FAIL, got %s", cr.Result)
	}
	rec, dec = opusRec(0)
	rec.opus[30].reinit = true
	if cr := scoreOpus(cs, cfg, rec, dec, rec.tap0NS); cr.Result != resultFail {
		t.Fatalf("a REINIT after the first packet: want FAIL, got %s", cr.Result)
	}
	rec, dec = opusRec(0)
	for i := range rec.opus {
		rec.opus[i].data = make([]byte, 320)
	}
	if cr := scoreOpus(cs, cfg, rec, dec, rec.tap0NS); cr.Result != resultFail {
		t.Fatalf("no bitrate change: want FAIL, got %s", cr.Result)
	}
	// A silent capture: both sides read the floor, a level difference of 0, but nothing is present.
	rec, _ = opusRec(0)
	silent := make([]float64, len(rec.left))
	rec.left, rec.right = silent, silent
	cr := scoreOpus(cs, cfg, rec, make([]float64, len(silent)), rec.tap0NS)
	if cr.Result != resultFail {
		t.Fatalf("a silent capture: want FAIL, got %s", cr.Result)
	}
	if math.IsInf(cr.Checks[0].LevelDB, 0) {
		t.Fatal("a level is infinite")
	}
}
