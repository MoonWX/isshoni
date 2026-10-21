package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestPercentile(t *testing.T) {
	v := make([]int, 100)
	for i := range v {
		v[i] = i + 1
	}
	for p, want := range map[float64]int{50: 50, 95: 95, 99: 99, 100: 100, 0: 1} {
		if got := percentile(v, p); got != want {
			t.Errorf("percentile(1…100, %v) = %d, want %d", p, got, want)
		}
	}
	if got := percentile([]int{7}, 95); got != 7 {
		t.Errorf("percentile of one = %d", got)
	}
	if got := percentile([]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 300}, 95); got != 300 {
		t.Errorf("p95 of ten is the largest: got %d", got)
	}
}

// TestJudge: the exit statuses of 01 §12.2 at their boundaries.
func TestJudge(t *testing.T) {
	l := func(p95 int) *latency { return &latency{P50: p95 / 2, P95: p95, Max: p95 + 10} }
	for _, tc := range []struct {
		samples, screenshots int
		l                    *latency
		interrupted          bool
		result               string
		code                 int
	}{
		{1200, 1200, l(150), false, resultPass, 0},
		{1080, 1200, l(199), false, resultPass, 0}, // exactly 90 % decoded
		{1079, 1200, l(150), false, resultInconclusive, 2},
		{1200, 1200, l(200), false, resultFail, 1}, // p95 must be under 200
		{1100, 1200, l(450), false, resultFail, 1},
		{1000, 1200, l(450), false, resultInconclusive, 2}, // too few decoded outranks the latency
		{0, 1200, nil, false, resultInconclusive, 2},
		{0, 0, nil, false, resultInconclusive, 2},
		{600, 600, l(100), true, resultInconclusive, 2},
	} {
		if r, c := judge(tc.samples, tc.screenshots, tc.l, tc.interrupted); r != tc.result || c != tc.code {
			t.Errorf("judge(%d of %d, %+v, interrupted %v) = %s %d, want %s %d", tc.samples, tc.screenshots, tc.l,
				tc.interrupted, r, c, tc.result, tc.code)
		}
	}
}

// TestReportJSON: the labels and statistics the exit-test record reads.
func TestReportJSON(t *testing.T) {
	d := func(v int) *int { return &v }
	for _, runName := range runNames {
		r := &report{Tool: "g2g", Run: runName}
		r.Versions = versions{G2G: "v0.2.0-rc.1", Go: "go1.27.2", OS: "Windows 10.0.26100"}
		r.Series = []point{
			{TMs: 0, DeltaMs: d(100)}, {TMs: 50, DeltaMs: d(120), Transition: true}, {TMs: 100, Fail: failCopy},
			{TMs: 150, DeltaMs: d(0)}, {TMs: 200, Fail: failImplausible}, {TMs: 250, Fail: failClock},
			{TMs: 300, Fail: failCapture},
		}
		r.summarize()
		var buf bytes.Buffer
		if err := r.writeJSON(&buf); err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got["run"] != runName || got["result"] != resultInconclusive || got["exitCode"] != 2.0 {
			t.Errorf("run %v, result %v, exit %v", got["run"], got["result"], got["exitCode"])
		}
		if got["samples"] != 3.0 || got["screenshots"] != 7.0 || got["transitions"] != 1.0 || got["decodedPct"] != 42.9 {
			t.Errorf("samples %v of %v, transitions %v, decoded %v %%", got["samples"], got["screenshots"],
				got["transitions"], got["decodedPct"])
		}
		f := got["decodeFailures"].(map[string]any)
		if f["total"] != 4.0 || f["copy"] != 1.0 || f["clock"] != 1.0 || f["implausible"] != 1.0 || f["capture"] != 1.0 {
			t.Errorf("decodeFailures %v", f)
		}
		if l := got["latencyMs"].(map[string]any); l["p50"] != 100.0 || l["p95"] != 120.0 || l["max"] != 120.0 {
			t.Errorf("latencyMs %v", l)
		}
		if got["rttMs"] != nil {
			t.Errorf("rttMs %v without -server, want null", got["rttMs"])
		}
		series := got["series"].([]any)
		if first := series[3].(map[string]any); first["deltaMs"] != 0.0 {
			t.Errorf("a zero latency is written: %v", first)
		}
		if failed := series[2].(map[string]any); failed["deltaMs"] != nil || failed["fail"] != "copy" {
			t.Errorf("a failed screenshot: %v", failed)
		}
		if !strings.HasSuffix(buf.String(), "}\n") {
			t.Error("the JSON does not end with a newline")
		}
	}
}

func TestToolVersion(t *testing.T) {
	old := version
	defer func() { version = old }()
	version = "v0.2.0-rc.1"
	if got := currentVersions(); got.G2G != "v0.2.0-rc.1" || got.Go == "" || got.OS == "" {
		t.Errorf("currentVersions() = %+v", got)
	}
	version = ""
	if toolVersion() == "" {
		t.Error("toolVersion is empty without a stamp")
	}
}
