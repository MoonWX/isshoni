package main

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// stubDriver is a driver whose cases return canned recordings, so the runner's rules run on every OS.
type stubDriver struct {
	endpoints int
	recs      map[string]recording // by case name; a missing one is an error
	ran       []string
}

func (s *stubDriver) env() (dllInfo, osInfo, machineInfo, error) {
	m := machineInfo{RenderEndpoints: make([]endpointInfo, s.endpoints)}
	return dllInfo{Version: "0.2.0"}, osInfo{Product: "win11", Build: 26200}, m, nil
}
func (s *stubDriver) selfCheck(io.Writer) error { return nil }
func (s *stubDriver) probe(io.Writer) error     { return nil }
func (s *stubDriver) apps(io.Writer) error      { return nil }
func (s *stubDriver) close() error              { return nil }

func (s *stubDriver) runCase(cs caseSpec, _ gateConfig) (recording, error) {
	s.ran = append(s.ran, cs.name)
	rec, ok := s.recs[cs.name]
	if !ok {
		return recording{}, errors.New("no recording")
	}
	return rec, nil
}

func (s *stubDriver) runSpecial(cs caseSpec, cfg gateConfig) (caseReport, error) {
	s.ran = append(s.ran, cs.name)
	cr := newCaseReport(cs, cfg)
	cr.Result = resultPass
	return cr, nil
}

// TestBaselineFailStopsTheRun is 05 §18.5 case 1: if it FAILs, every later case is INCONCLUSIVE ("the audio setup is
// broken, not isshoni") and doesn't run.
func TestBaselineFailStopsTheRun(t *testing.T) {
	broken := recForEndpoint(8)
	broken.left = make([]float64, len(broken.left)) // nothing captured: case 1 FAILs
	broken.right = broken.left
	drv := &stubDriver{endpoints: 1, recs: map[string]recording{"baseline-endpoint": broken}}
	sel, err := selectCases(runOptions{cases: []string{"1", "2", "20"}})
	if err != nil {
		t.Fatal(err)
	}
	reports := runSelected(drv, sel, gateConfig{FastPath: true}, true)
	if reports[0].Result != resultFail {
		t.Fatalf("case 1: want FAIL, got %s: %v", reports[0].Result, reports[0].Problems)
	}
	for _, r := range reports[1:] {
		if r.Result != resultInconclusive || !strings.Contains(r.Problems[0], "audio setup is broken") {
			t.Fatalf("%s after a failed case 1: want INCONCLUSIVE, got %s %v", r.Name, r.Result, r.Problems)
		}
	}
	if len(drv.ran) != 1 {
		t.Fatalf("ran %v, want only case 1", drv.ran)
	}
	if got := exitCode(reports); got != exitFail {
		t.Fatalf("exit %d, want %d", got, exitFail)
	}

	// A passing case 1 lets the run go on.
	drv = &stubDriver{endpoints: 1, recs: map[string]recording{"baseline-endpoint": recForEndpoint(8),
		"builtin-voice": recForCase2(8)}}
	reports = runSelected(drv, sel, gateConfig{FastPath: true}, true)
	for _, r := range reports {
		if r.Result != resultPass {
			t.Fatalf("%s: want PASS, got %s: %v", r.Name, r.Result, r.Problems)
		}
	}
}

// TestNoRenderEndpointSkips is 05 §18.4: without an active render endpoint every case is SKIP and the exit code is
// 3 (§18.8's experimental CI job expects 3), and no case runs.
func TestNoRenderEndpointSkips(t *testing.T) {
	drv := &stubDriver{endpoints: 0}
	var b strings.Builder
	got := runWith(&b, drv, runOptions{gate: true, outDir: t.TempDir()})
	if got != exitSkipped {
		t.Fatalf("exit %d, want %d\n%s", got, exitSkipped, b.String())
	}
	if len(drv.ran) != 0 {
		t.Fatalf("ran %v without an endpoint", drv.ran)
	}
	if !strings.Contains(b.String(), "SKIP") || !strings.Contains(b.String(), "no active render endpoint") {
		t.Fatalf("output doesn't explain the skip:\n%s", b.String())
	}
}

// TestRunReportsEveryCaseResult: a case the driver can't run is INCONCLUSIVE, and a fast-path case under a gate
// that ships the fast path off is INFO whatever happened (05 §18.10).
func TestRunReportsEveryCaseResult(t *testing.T) {
	drv := &stubDriver{endpoints: 1, recs: map[string]recording{}}
	sel, _ := selectCases(runOptions{cases: []string{"2", "4"}})
	reports := runSelected(drv, sel, gateConfig{FastPath: false}, true)
	if reports[0].Result != resultInconclusive || reports[1].Result != resultInfo {
		t.Fatalf("results %s, %s; want INCONCLUSIVE, INFO", reports[0].Result, reports[1].Result)
	}
	for _, r := range reports {
		if r.Checks == nil || r.Engine == nil || r.Events == nil || r.Problems == nil {
			t.Fatalf("%s has a nil list (JSON null): %+v", r.Name, r)
		}
	}
}
