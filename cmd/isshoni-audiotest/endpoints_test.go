package main

import (
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// These tests cover case 22's scoring (special.go), the endpoint naming the report allows (endpoints.go), and the
// WAV helpers WA62b added (the soak's seamless loop, record's streamed file). They run on every OS.

// recForMultiEndpoint is a case-22 recording: the game's 1000 Hz on the default endpoint, and 683 Hz from the
// second endpoint at level683 dBFS (nil: not captured).
func recForMultiEndpoint(level683 *float64) recording {
	dur := 8.0
	sig := fadedTone(freqGame, dbToAmp(-18), dur, 0.02)
	if level683 != nil {
		sig = mixTones(sig, fadedTone(freqLateGame, dbToAmp(*level683), dur, 0.02))
	}
	return baseRec(sig, dur, map[string]*roleTrack{
		"game":  trackActive("heard", "", dur),
		"game2": trackActive("heard", "", dur),
	}, "include-set")
}

// TestScoreMultiEndpoint is case 22 (INFO): it records whether the second endpoint's 683 Hz was captured and its
// level, and the endpoint count; the result is INFO either way, and an unheard game is recorded as a problem.
func TestScoreMultiEndpoint(t *testing.T) {
	cs := mustCase(t, "multi-endpoint")
	cfg := gateConfig{FastPath: true}
	lvl := -18.0
	cr := scoreMultiEndpoint(cs, cfg, recForMultiEndpoint(&lvl), 2)
	if cr.Result != resultInfo || cr.Required || cr.Info["captured"] != true || cr.Info["endpoints"] != 2 {
		t.Fatalf("captured: %+v", cr)
	}
	if l, _ := cr.Info["levelDb"].(float64); math.Abs(l+18) > 0.5 {
		t.Fatalf("level %v, want about −18", cr.Info["levelDb"])
	}
	cr = scoreMultiEndpoint(cs, cfg, recForMultiEndpoint(nil), 3)
	if cr.Result != resultInfo || cr.Info["captured"] != false || len(cr.Problems) != 0 {
		t.Fatalf("not captured: %+v", cr)
	}
	silent := recForMultiEndpoint(&lvl)
	silent.left = make([]float64, len(silent.left))
	silent.right = silent.left
	cr = scoreMultiEndpoint(cs, cfg, silent, 2)
	if cr.Result != resultInfo || len(cr.Problems) == 0 {
		t.Fatalf("nothing heard: want INFO with a problem, got %+v", cr)
	}
	lost := recForMultiEndpoint(&lvl)
	lost.reparented = false
	if cr := scoreMultiEndpoint(cs, cfg, lost, 2); cr.Result != resultInfo || len(cr.Problems) != 1 {
		t.Fatalf("a premise failed: want INFO with the problem, got %+v", cr)
	}
}

func TestEndpointNames(t *testing.T) {
	for v, want := range map[uint32]string{1: "Speakers", 3: "Headphones", 9: "DigitalAudioDisplayDevice",
		10: "UnknownFormFactor", 11: "unknown", 1 << 31: "unknown"} {
		if got := formFactorName(v); got != want {
			t.Errorf("formFactorName(%d) = %q, want %q", v, got, want)
		}
	}
	for e, want := range map[string]string{"USB": "usb", "HDAUDIO": "hdaudio", "BTHENUM": "bluetooth",
		"BTHHFENUM": "bluetooth", "SWD": "virtual", "ROOT": "virtual", "PCI": "pci", "": "unknown",
		"VBAudioVACWDM": "other", "Ada's Cable": "other"} {
		if got := busName(e); got != want {
			t.Errorf("busName(%q) = %q, want %q", e, got, want)
		}
	}
	// Every name these return is one the redactor keeps.
	for v := range uint32(12) {
		if n := formFactorName(v); !slices.Contains(knownFormFactors, n) {
			t.Errorf("form factor %q is not in the redactor's list", n)
		}
	}
	for _, e := range []string{"USB", "HDAUDIO", "BTHENUM", "SWD", "PCI", "", "X"} {
		if b := busName(e); !slices.Contains(knownBuses, b) {
			t.Errorf("bus %q is not in the redactor's list", b)
		}
	}
}

// TestLoopToneWAV: the soak's loop is one AM period long, plays seamlessly when repeated (no click at the seam,
// both partials present), and refuses a partial without a whole number of cycles in the loop.
func TestLoopToneWAV(t *testing.T) {
	wav, err := loopToneWAV([]float64{freqGame, freqLateGame}, 0.125, soakAMHz)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "loop.wav")
	if err := os.WriteFile(p, wav, 0o600); err != nil {
		t.Fatal(err)
	}
	l, _, err := readWAV(p)
	if err != nil || len(l) != int(sampleRate/soakAMHz) {
		t.Fatalf("loop of %d frames, err %v; want %d", len(l), err, int(sampleRate/soakAMHz))
	}
	twice := append(append([]float64(nil), l...), l...)
	if ev := clickEvents(twice[1:], twice[1:], sampleRate, clickThreshold); len(ev) != 0 {
		t.Fatalf("the loop clicks at %v", ev)
	}
	seam := segment(twice, sampleRate, 9, 11)
	for _, f := range []float64{freqGame, freqLateGame} {
		if db := toneLevelDB(seam, sampleRate, f); db < -25 || db > -17 {
			t.Fatalf("%.0f Hz reads %.1f dB across the seam", f, db)
		}
	}
	if _, err := loopToneWAV([]float64{1000.05}, 0.125, soakAMHz); err == nil {
		t.Fatal("a partial with a fractional cycle count in the loop was accepted")
	}
	if _, err := loopToneWAV([]float64{1000}, 0.125, 0); err == nil {
		t.Fatal("am 0 was accepted")
	}
}

// TestWAVStream: record's WAV written chunk by chunk reads back sample for sample.
func TestWAVStream(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rec.wav")
	ws, err := newWAVStream(p)
	if err != nil {
		t.Fatal(err)
	}
	a := sineAt(pcmChunkFrames, freqGame, 0.25)
	b := sineAt(pcmChunkFrames, freqVoice, 0.5)
	for range 3 {
		if err := ws.write(a, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := ws.close(); err != nil {
		t.Fatal(err)
	}
	l, r, err := readWAV(p)
	if err != nil || len(l) != 3*pcmChunkFrames {
		t.Fatalf("read back %d frames, err %v", len(l), err)
	}
	for i := range l {
		if math.Abs(l[i]-a[i%pcmChunkFrames]) > 1e-6 || math.Abs(r[i]-b[i%pcmChunkFrames]) > 1e-6 {
			t.Fatalf("frame %d: %g/%g", i, l[i], r[i])
		}
	}
}
