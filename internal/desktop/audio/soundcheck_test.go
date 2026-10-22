package audio

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// TestRunSoundCheckHelperOffWindows: off Windows the helper plays nothing and returns 2 (05 §11.3).
func TestRunSoundCheckHelperOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("on Windows the helper plays the chime (helper_windows.go)")
	}
	if got := RunSoundCheckHelper(); got != 2 {
		t.Errorf("RunSoundCheckHelper() = %d, want 2", got)
	}
}

// TestSoundCheckWithoutHelper: with SelfInfo.Exe "" the service starts nothing and asks the engine with helper pid 0
// and the 3 s window; the fake answers after 1.5 s.
func TestSoundCheckWithoutHelper(t *testing.T) {
	ctl := newStub()
	ctl.sound = media.SoundCheckResult{Captured: true, LevelDB: -14, Millis: 1500, Backend: "process-loopback"}
	s := New(ctl, newStore(DefaultSettings()), SelfInfo{PID: 1}, nil)
	defer func() { _ = s.Close() }()
	res, err := s.SoundCheck(t.Context())
	if err != nil || res != ctl.sound {
		t.Fatalf("SoundCheck: %+v, %v", res, err)
	}
	if c := ctl.ops("soundcheck"); len(c) != 1 || c[0].pid != 0 {
		t.Errorf("calls %+v", c)
	}

	synctest.Test(t, func(t *testing.T) {
		eng := newFake("no-device")
		defer func() { _ = eng.Close() }()
		s := New(eng, newStore(DefaultSettings()), SelfInfo{}, nil)
		defer func() { _ = s.Close() }()
		start := time.Now()
		res, err := s.SoundCheck(t.Context())
		if err != nil || res.Captured || res.Reason != "no_device" || res.Backend != "none" ||
			time.Since(start) != 1500*time.Millisecond {
			t.Errorf("the fake's no-device sound check: %+v, %v after %v", res, err, time.Since(start))
		}
	})
}

// TestSoundCheckHelper: with SelfInfo.Exe the service starts `<Exe> --sound-check-helper` (here the test binary,
// which TestMain turns into the helper) and passes its pid to the engine; a binary that doesn't exist is an error
// that doesn't quote its path.
func TestSoundCheckHelper(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	ctl := newStub()
	ctl.sound = media.SoundCheckResult{Captured: true, LevelDB: -14.3, Millis: 1460, Backend: "endpoint"}
	s := New(ctl, newStore(DefaultSettings()), SelfInfo{PID: 1, Exe: exe}, nil)
	defer func() { _ = s.Close() }()
	res, err := s.SoundCheck(t.Context())
	if err != nil || res != ctl.sound {
		t.Fatalf("SoundCheck: %+v, %v", res, err)
	}
	if c := ctl.ops("soundcheck"); len(c) != 1 || c[0].pid == 0 || int(c[0].pid) == os.Getpid() {
		t.Errorf("calls %+v", c)
	}

	missing := filepath.Join(t.TempDir(), "no-such-binary")
	s2 := New(ctl, newStore(DefaultSettings()), SelfInfo{Exe: missing}, nil)
	defer func() { _ = s2.Close() }()
	_, err = s2.SoundCheck(t.Context())
	if err == nil || strings.Contains(err.Error(), missing) || strings.Contains(err.Error(), filepath.Dir(missing)) {
		t.Errorf("a missing helper: %v", err)
	}
}

// TestChimeWAV: the chime is an 800 ms 48 kHz 16-bit stereo WAVE with both partials at about −14 dBFS (the sound
// check needs −40), silent at its ends (the 10 ms ramps) and without clipping.
func TestChimeWAV(t *testing.T) {
	b := chimeWAV()
	le := binary.LittleEndian
	if string(b[0:4]) != "RIFF" || string(b[8:16]) != "WAVEfmt " || le.Uint32(b[16:]) != 16 || le.Uint16(b[20:]) != 1 ||
		le.Uint16(b[22:]) != 2 || le.Uint32(b[24:]) != 48000 || le.Uint32(b[28:]) != 192000 ||
		le.Uint16(b[32:]) != 4 || le.Uint16(b[34:]) != 16 || string(b[36:40]) != "data" {
		t.Fatalf("header % x", b[:44])
	}
	data := b[44:]
	if int(le.Uint32(b[40:])) != len(data) || len(data) != 38400*4 || int(le.Uint32(b[4:])) != 36+len(data) {
		t.Fatalf("sizes: data %d, header %d, riff %d", len(data), le.Uint32(b[40:]), le.Uint32(b[4:]))
	}
	samples := make([]int16, len(data)/2)
	if err := binary.Read(bytes.NewReader(data), le, samples); err != nil {
		t.Fatal(err)
	}
	left := make([]float64, len(samples)/2)
	peak := 0.0
	for i := range left {
		l, r := samples[2*i], samples[2*i+1]
		if r != l {
			t.Fatalf("frame %d: left %d, right %d", i, l, r)
		}
		left[i] = float64(l) / math.MaxInt16
		peak = max(peak, math.Abs(left[i]))
	}
	if left[0] != 0 || left[len(left)-1] != 0 || peak > 0.45 || peak < 0.3 {
		t.Errorf("ends %v %v, peak %.3f", left[0], left[len(left)-1], peak)
	}
	for _, hz := range []float64{chimeLowHz, chimeHighHz} {
		if db := toneDB(left[480:len(left)-480], hz); db < -15 || db > -13 {
			t.Errorf("%v Hz at %.1f dBFS", hz, db)
		}
	}
	if db := toneDB(left[480:len(left)-480], 1000); db > -40 {
		t.Errorf("1000 Hz at %.1f dBFS", db)
	}
}

// toneDB is the level of hz in x in dBFS (a full-scale sine is 0): a Goertzel filter over a Hann window.
func toneDB(x []float64, hz float64) float64 {
	w := 2 * math.Pi * hz / chimeRate
	coeff := 2 * math.Cos(w)
	var s1, s2, wsum float64
	for i, v := range x {
		hann := 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(len(x)-1))
		wsum += hann
		s0 := v*hann + coeff*s1 - s2
		s2, s1 = s1, s0
	}
	power := s1*s1 + s2*s2 - coeff*s1*s2
	amp := 2 * math.Sqrt(power) / wsum
	return 20 * math.Log10(amp)
}

// TestDuckingStub: off Windows the ducking preference is unknown, which hides the tip even with a mic user.
func TestDuckingStub(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows reads the registry (ducking_windows.go)")
	}
	if readDucking() != duckingUnknown {
		t.Fatal("the stub knows a preference")
	}
	mic := app("kook.exe", "KOOK", verdictKeptOut, reasonMic)
	mic.UsesMic = true
	if fh := buildFriendsHear(fhInput{report: okReport(mic), haveReport: true, ducking: readDucking()}); len(fh.Tips) != 0 {
		t.Errorf("tips %+v", fh.Tips)
	}
}
