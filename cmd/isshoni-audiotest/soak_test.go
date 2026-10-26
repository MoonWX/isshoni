package main

import (
	"maps"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/media"
)

// pcmMediaOpus is an Opus packet at captureNS (session clock).
func pcmMediaOpus(captureNS int64, data []byte) media.Packet {
	return media.Packet{Kind: media.KindAudio, CaptureNS: captureNS, Data: data}
}

// These tests cover the soak's streaming analysis (soak.go, 05 §18.5 case 23) on synthetic signals: it agrees with
// the batch analyzer, finds clicks across chunk boundaries and a short 440 Hz leak, and the soak's verdict checks the
// counters and the controls. A minute of signal stands for the 30 minutes.

// soakSignal is dur seconds of the soak's music (1000 + 683 Hz, the loop's AM) as the tap would carry it.
func soakSignal(dur float64) []float64 {
	wav, err := loopToneWAV([]float64{freqGame, freqLateGame}, 0.125, soakAMHz)
	if err != nil {
		panic(err)
	}
	loop := make([]float64, (len(wav)-44)/4)
	for i := range loop {
		loop[i] = float64(int16(uint16(wav[44+i*4])|uint16(wav[45+i*4])<<8)) / 32768 //nolint:gosec // G115
	}
	n := int(dur * sampleRate)
	out := make([]float64, n)
	for i := range out {
		out[i] = loop[i%len(loop)]
	}
	return out
}

// feedChunks feeds m to sa in 10 ms tap chunks.
func feedChunks(sa *soakAnalyzer, m []float64) {
	for i := 0; i < len(m); i += pcmChunkFrames {
		c := m[i:min(i+pcmChunkFrames, len(m))]
		sa.feed(c, c)
	}
}

func soakAnalyzerFor(t *testing.T) (*soakAnalyzer, caseSpec) {
	t.Helper()
	cs := mustCase(t, "soak")
	all, short := soakFreqs(cs)
	return newSoakAnalyzer(1.5, all, short), cs
}

// TestSoakAnalyzerMatchesBatch: on a clean minute the streaming results equal what the batch analyzer finds (the
// 50 ms windows' maximum at 440 Hz, the block levels), the loop is seamless (no click at its 10 s seams), and both
// tones read present in every block.
func TestSoakAnalyzerMatchesBatch(t *testing.T) {
	sa, _ := soakAnalyzerFor(t)
	m := soakSignal(61)
	feedChunks(sa, m)
	res := sa.result()
	if res.clickCount != 0 {
		t.Fatalf("the looped music clicks at %v", res.clicks)
	}
	if res.blocks != 6 || math.Abs(res.analyzedSec-59.5) > 0.01 {
		t.Fatalf("%d blocks over %.2f s, want 6 over 59.5", res.blocks, res.analyzedSec)
	}
	seg := segment(m, sampleRate, 1.5, 0)
	batchShort, batchAt := shortWindow(seg, sampleRate, freqVoice)
	if math.Abs(res.maxShort[freqVoice]-batchShort) > 1e-9 ||
		math.Abs(res.maxShortAt[freqVoice]-(1.5+batchAt+shortWindowSec/2)) > 1e-9 {
		t.Fatalf("440 Hz short-window max %.2f dB at %.3f s, batch %.2f at %.3f", res.maxShort[freqVoice],
			res.maxShortAt[freqVoice], batchShort, 1.5+batchAt+shortWindowSec/2)
	}
	first := toneLevelDB(seg[:int(soakBlockSec*sampleRate)], sampleRate, freqGame)
	if res.blockMax[freqGame] < first-1e-9 || res.blockMin[freqGame] > first+1e-9 {
		t.Fatalf("the first block's 1000 Hz level %.2f lies outside [%.2f, %.2f]", first, res.blockMin[freqGame],
			res.blockMax[freqGame])
	}
	for _, f := range []float64{freqGame, freqLateGame} {
		if res.blockMin[f] < presentMinDB {
			t.Fatalf("%.0f Hz reads %.1f dB in a block: not present", f, res.blockMin[f])
		}
	}
	if res.blockMax[freqVoice] > -90 || res.maxShort[freqVoice] > -90 { // 16-bit quantization only
		t.Fatalf("440 Hz reads %.1f / %.1f dB in clean music", res.blockMax[freqVoice], res.maxShort[freqVoice])
	}
}

// TestSoakAnalyzerFindsFaults: a mid-wave cut (a click) that straddles a chunk boundary, and a 20 ms 440 Hz burst
// (whose unfaded end is a click too), are found, at their times; a fault before the window (1.5 s) is not counted.
func TestSoakAnalyzerFindsFaults(t *testing.T) {
	sa, _ := soakAnalyzerFor(t)
	m := soakSignal(30)
	cut := 20*sampleRate + pcmChunkFrames - 1 // the last sample of a chunk
	for i := cut; i < cut+200; i++ {
		m[i] = 0
	}
	copyInto(m, toneBurst(freqVoice, dbToAmp(-18), int(0.020*sampleRate)), 12*sampleRate)
	for i := sampleRate / 2; i < sampleRate/2+100; i++ {
		m[i] = 0 // before the window
	}
	feedChunks(sa, m)
	res := sa.result()
	foundCut := false
	for _, c := range res.clicks {
		if c < 1.5 {
			t.Fatalf("a click before the window was counted: %v", res.clicks)
		}
		foundCut = foundCut || math.Abs(c-float64(cut)/sampleRate) <= 0.002
	}
	if !foundCut || res.clickCount != len(res.clicks) {
		t.Fatalf("clicks %v (%d), want the cut at %.3f s", res.clicks, res.clickCount, float64(cut)/sampleRate)
	}
	if res.maxShort[freqVoice] <= absentMaxDB || math.Abs(res.maxShortAt[freqVoice]-12.01) > 0.03 {
		t.Fatalf("the 440 Hz burst read %.1f dB at %.3f s, want > %.0f near 12.01", res.maxShort[freqVoice],
			res.maxShortAt[freqVoice], absentMaxDB)
	}
}

// soakRec is a clean soak recording's samples (the audio went to the analyzer): the music heard and active
// throughout, the voice app's mic session active.
func soakRec(dur float64) recording {
	rec := baseRec(make([]float64, int(dur*sampleRate)), dur, map[string]*roleTrack{
		"music": trackActive("heard", "", dur),
	}, "include-set")
	rec.left, rec.right = nil, nil
	rec.counters = map[string]int{"underruns": 0, "trims": 0, "mixerResyncs": 0, "maxResamplePpm": 41}
	return rec
}

// TestScoreSoak: a clean soak PASSes; a click, a counter above zero, maxResamplePpm over 1000, the music kept out,
// or a 440 Hz leak FAILs; a fixture that could not open the mic SKIPs; an unproven premise is INCONCLUSIVE; the
// result never counts toward the exit code.
func TestScoreSoak(t *testing.T) {
	cfg := gateConfig{FastPath: true}
	sa, _ := soakAnalyzerFor(t)
	feedChunks(sa, soakSignal(31))
	base := sa.result()
	clean := func() soakResult { // a copy each case may change
		res := base
		res.blockMin, res.blockMax = maps.Clone(base.blockMin), maps.Clone(base.blockMax)
		res.maxShort, res.maxShortAt = maps.Clone(base.maxShort), maps.Clone(base.maxShortAt)
		res.clicks = slices.Clone(base.clicks)
		return res
	}
	cs := mustCase(t, "soak")
	cr := scoreSoak(cs, cfg, soakRec(31), clean())
	if cr.Result != resultPass || cr.Required {
		t.Fatalf("clean soak: want an unrequired PASS, got required=%v %s %v", cr.Required, cr.Result, cr.Problems)
	}
	if len(cr.Checks) != 3 || cr.Info["clicks"] != 0 || cr.Info["blocks"] != 3 {
		t.Fatalf("checks %+v info %v", cr.Checks, cr.Info)
	}

	res := clean()
	res.clickCount, res.clicks = 2, []float64{7.5, 9}
	if cr := scoreSoak(cs, cfg, soakRec(31), res); cr.Result != resultFail || len(cr.ClicksAt) != 2 {
		t.Fatalf("clicks: want FAIL with 2 click times, got %s %v", cr.Result, cr.ClicksAt)
	}
	for _, key := range []string{"underruns", "trims", "mixerResyncs"} {
		r := soakRec(31)
		r.counters[key] = 1
		if cr := scoreSoak(cs, cfg, r, clean()); cr.Result != resultFail || !strings.Contains(cr.Problems[0], key) {
			t.Fatalf("%s 1: want FAIL naming it, got %s %v", key, cr.Result, cr.Problems)
		}
	}
	r := soakRec(31)
	r.counters["maxResamplePpm"] = 1001
	if cr := scoreSoak(cs, cfg, r, clean()); cr.Result != resultFail {
		t.Fatalf("maxResamplePpm 1001: want FAIL, got %s", cr.Result)
	}
	r = soakRec(31)
	r.roles["music"].samples[40].verdict = "kept_out"
	if cr := scoreSoak(cs, cfg, r, clean()); cr.Result != resultFail {
		t.Fatalf("music kept out: want FAIL, got %s", cr.Result)
	}
	res = clean()
	res.maxShort[freqVoice] = -30
	if cr := scoreSoak(cs, cfg, soakRec(31), res); cr.Result != resultFail {
		t.Fatalf("440 Hz leak: want FAIL, got %s", cr.Result)
	}
	r = soakRec(31)
	r.micFail = true
	if cr := scoreSoak(cs, cfg, r, clean()); cr.Result != resultSkip {
		t.Fatalf("no mic: want SKIP, got %s", cr.Result)
	}
	r = soakRec(31)
	r.lostGapMs = 80
	if cr := scoreSoak(cs, cfg, r, clean()); cr.Result != resultInconclusive {
		t.Fatalf("lost audio: want INCONCLUSIVE, got %s", cr.Result)
	}
}

// TestStreamCapture: a streaming capture hands the timeline (gaps zero-padded) to its sink, keeps no audio and no
// Opus bytes, and still measures lost audio.
func TestStreamCapture(t *testing.T) {
	var got []float64
	c := newStreamCapture(0, 0, func(l, r []float64) { got = append(got, l...) })
	for i := range int64(10) {
		c.packet(pcmChunk(i*pcmChunkNS, 0.25))
	}
	c.packet(pcmChunk(13*pcmChunkNS, 0.5)) // 30 ms skipped
	c.packet(pcmMediaOpus(14*pcmChunkNS, []byte{1, 2, 3}))
	if len(c.left) != 0 || len(c.right) != 0 {
		t.Fatal("a streaming capture kept audio")
	}
	if len(got) != 14*pcmChunkFrames || c.nSamples != len(got) || got[11*pcmChunkFrames] != 0 ||
		got[13*pcmChunkFrames] != 0.5 {
		t.Fatalf("the sink got %d samples (nSamples %d)", len(got), c.nSamples)
	}
	if len(c.opus) != 1 || c.opus[0].data != nil {
		t.Fatalf("Opus packets %+v: want one without bytes", c.opus)
	}
	if lost := c.lostGapMs(0.14); lost != 30 {
		t.Fatalf("lost %d ms, want 30", lost)
	}
}
