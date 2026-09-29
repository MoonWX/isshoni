package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// Audio analysis used by the self-test (and runnable on any OS against a WAV):
// measure pure-tone levels with the Goertzel algorithm and detect clicks.

const sampleRate = 48000

// toneLevelDB returns the level of a sine at freq in x (mono), in dBFS of its
// peak amplitude (a full-scale sine is 0 dB). Uses a Hann window.
func toneLevelDB(x []float64, fs, freq float64) float64 {
	n := len(x)
	if n < 2 {
		return -200
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

// clickEvents finds discontinuities: samples whose second difference exceeds
// threshold (a smooth mix of tones stays far below it), merged within 10 ms.
// Returns event times in seconds relative to the start of x.
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

// segment returns [from, to) seconds of a channel (to <= 0 means the end).
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

func mono(left, right []float64) []float64 {
	m := make([]float64, len(left))
	for i := range m {
		m[i] = (left[i] + right[i]) / 2
	}
	return m
}

// Expectation for one tone in one time window of a recording.
type check struct {
	Freq   float64 `json:"freq"`
	From   float64 `json:"from"`
	To     float64 `json:"to"`     // 0 = end
	Expect string  `json:"expect"` // present | absent | info
	Why    string  `json:"why"`
}

type checkResult struct {
	check
	LevelDB float64 `json:"level_db"`
	Pass    bool    `json:"pass"`
}

// Thresholds: a played test tone is about -12 dBFS (less if the Windows volume
// slider is low or communications ducking kicks in); "absent" means at least
// 40 dB below the quietest present tone in the recording and below -50 dBFS.
const (
	presentMinDB   = -45.0
	absentMarginDB = 40.0
	absentMaxDB    = -50.0
	clickThreshold = 0.03
)

func evaluate(left, right []float64, checks []check) ([]checkResult, float64) {
	m := mono(left, right)
	results := make([]checkResult, len(checks))
	quietestPresent := 0.0
	for i, c := range checks {
		results[i] = checkResult{check: c, LevelDB: toneLevelDB(segment(m, sampleRate, c.From, c.To), sampleRate, c.Freq)}
		if c.Expect == "present" && results[i].LevelDB < quietestPresent {
			quietestPresent = results[i].LevelDB
		}
	}
	for i := range results {
		r := &results[i]
		switch r.Expect {
		case "present":
			r.Pass = r.LevelDB >= presentMinDB
		case "absent":
			r.Pass = r.LevelDB <= absentMaxDB && r.LevelDB <= quietestPresent-absentMarginDB
		default:
			r.Pass = true
		}
	}
	return results, quietestPresent
}

// cmdAnalyze: s1 analyze file.wav -present 1000 -absent 440,550 [-from 1.5 -to 0]
func cmdAnalyze(args []string) error {
	fs := flag.NewFlagSet("analyze", flag.ExitOnError)
	present := fs.String("present", "", "comma-separated frequencies that must be audible")
	absent := fs.String("absent", "", "comma-separated frequencies that must be excluded")
	from := fs.Float64("from", 1.5, "analysis window start (s)")
	to := fs.Float64("to", 0, "analysis window end (s, 0 = end)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: s1 analyze [flags] file.wav")
	}
	left, right, err := readWAV(fs.Arg(0))
	if err != nil {
		return err
	}
	var checks []check
	for kind, list := range map[string]string{"present": *present, "absent": *absent} {
		for _, f := range splitFloats(list) {
			checks = append(checks, check{Freq: f, From: *from, To: *to, Expect: kind})
		}
	}
	results, _ := evaluate(left, right, checks)
	clicks := clickEvents(segment(left, sampleRate, *from, *to), segment(right, sampleRate, *from, *to), sampleRate, clickThreshold)
	out, _ := json.MarshalIndent(map[string]any{"results": results, "clicks_at_s": clicks}, "", "  ")
	fmt.Println(string(out))
	for _, r := range results {
		if !r.Pass {
			os.Exit(1)
		}
	}
	return nil
}

func splitFloats(s string) []float64 {
	var out []float64
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			if f, err := strconv.ParseFloat(p, 64); err == nil {
				out = append(out, f)
			}
		}
	}
	return out
}
