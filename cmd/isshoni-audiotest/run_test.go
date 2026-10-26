package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubDriver is a driver whose cases return canned recordings, so the runner's rules run on every OS.
type stubDriver struct {
	endpoints int
	mic       bool
	recs      map[string]recording  // by case name; a missing one is an error
	specials  map[string]caseReport // by case name; a missing one is a PASS
	ran       []string
	redact    redactInfo
	osi       osInfo

	// record: the chunks and statuses it hands to the sink.
	recordChunks [][2][]float64
	recordStatus []hearStatus
	recordErr    error
	recorded     *recordOptions
}

func (s *stubDriver) env() (dllInfo, osInfo, machineInfo, error) {
	m := machineInfo{RenderEndpoints: make([]endpointInfo, s.endpoints), Mic: s.mic}
	osi := s.osi
	if osi.Product == "" {
		osi = osInfo{Product: "win11", Build: 26200}
	}
	return dllInfo{Version: "0.2.0"}, osi, m, nil
}
func (s *stubDriver) selfCheck(io.Writer) error { return nil }
func (s *stubDriver) probe(io.Writer) error     { return nil }
func (s *stubDriver) apps(io.Writer) error      { return nil }
func (s *stubDriver) close() error              { return nil }
func (s *stubDriver) redaction() redactInfo     { return s.redact }

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
	if cr, ok := s.specials[cs.name]; ok {
		return cr, nil
	}
	cr := newCaseReport(cs, cfg)
	cr.Result = resultPass
	return cr, nil
}

func (s *stubDriver) record(ro recordOptions, sink recordSink) error {
	s.recorded = &ro
	for _, c := range s.recordChunks {
		if err := sink.pcm(c[0], c[1]); err != nil {
			return err
		}
	}
	for _, st := range s.recordStatus {
		sink.status(st)
	}
	return s.recordErr
}

// allPre is a machine with an endpoint and a microphone.
var allPre = preconditions{renderEndpoints: 1, mic: true}

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
	reports := runSelected(io.Discard, drv, sel, gateConfig{FastPath: true}, allPre)
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
	reports = runSelected(io.Discard, drv, sel, gateConfig{FastPath: true}, allPre)
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
	reports := runSelected(io.Discard, drv, sel, gateConfig{FastPath: false}, allPre)
	if reports[0].Result != resultInconclusive || reports[1].Result != resultInfo {
		t.Fatalf("results %s, %s; want INCONCLUSIVE, INFO", reports[0].Result, reports[1].Result)
	}
	for _, r := range reports {
		if r.Checks == nil || r.Engine == nil || r.Events == nil || r.Problems == nil {
			t.Fatalf("%s has a nil list (JSON null): %+v", r.Name, r)
		}
	}
}

// TestMicCasesSkipWithoutAMic is 05 §18.4: without a microphone with desktop-app access the mic cases (9, 12–14,
// and the soak, whose voice app holds the mic) SKIP without running, and a required SKIP makes the gate exit 3; the
// cases that need no microphone still run.
func TestMicCasesSkipWithoutAMic(t *testing.T) {
	drv := &stubDriver{endpoints: 1, recs: map[string]recording{"builtin-voice": recForCase2(8)}}
	sel, err := selectCases(runOptions{cases: []string{"2", "9", "12", "13", "14", "23"}, long: true})
	if err != nil {
		t.Fatal(err)
	}
	reports := runSelected(io.Discard, drv, sel, gateConfig{FastPath: true},
		preconditions{renderEndpoints: 1, mic: false})
	if reports[0].Result != resultPass {
		t.Fatalf("case 2: want PASS, got %s: %v", reports[0].Result, reports[0].Problems)
	}
	for _, r := range reports[1:] {
		if r.Result != resultSkip || !strings.Contains(r.Problems[0], "microphone") {
			t.Fatalf("%s without a mic: want SKIP naming the microphone, got %s %v", r.Name, r.Result, r.Problems)
		}
	}
	if len(drv.ran) != 1 {
		t.Fatalf("ran %v, want only case 2", drv.ran)
	}
	if got := exitCode(reports); got != exitSkipped {
		t.Fatalf("exit %d, want %d (a required mic case SKIPPED)", got, exitSkipped)
	}
}

// TestMultiEndpointNeedsTwoEndpoints is 05 §18.5 case 22: SKIP with one endpoint, INFO otherwise, and never a
// change of the exit code.
func TestMultiEndpointNeedsTwoEndpoints(t *testing.T) {
	sel, _ := selectCases(runOptions{cases: []string{"multi-endpoint"}})
	drv := &stubDriver{}
	reports := runSelected(io.Discard, drv, sel, gateConfig{FastPath: true}, allPre)
	if reports[0].Result != resultSkip || reports[0].Required || len(drv.ran) != 0 {
		t.Fatalf("one endpoint: want an unrequired SKIP that didn't run, got %+v (ran %v)", reports[0], drv.ran)
	}
	if got := exitCode(reports); got != exitAllPass {
		t.Fatalf("one endpoint: exit %d, want 0", got)
	}
	cs := sel[0]
	failing := newCaseReport(cs, gateConfig{})
	failing.Result = resultFail
	drv = &stubDriver{specials: map[string]caseReport{"multi-endpoint": failing}}
	reports = runSelected(io.Discard, drv, sel, gateConfig{FastPath: true},
		preconditions{renderEndpoints: 2, mic: true})
	if reports[0].Result != resultInfo || exitCode(reports) != exitAllPass {
		t.Fatalf("two endpoints: want INFO and exit 0, got %s exit %d", reports[0].Result, exitCode(reports))
	}
}

// TestSoakOnlyWithLong is 05 §18.2: the soak runs only with -long (then last), and never changes the exit code.
func TestSoakOnlyWithLong(t *testing.T) {
	all, err := selectCases(runOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, cs := range all {
		if cs.long {
			t.Fatalf("case %q selected without -long", cs.name)
		}
	}
	if len(all) != 26 {
		t.Fatalf("%d cases without -long, want 26 (every case of 05 §18.5 but the soak)", len(all))
	}
	long, _ := selectCases(runOptions{long: true})
	if len(long) != 27 || long[len(long)-1].name != "soak" {
		t.Fatalf("with -long: %d cases, the last %q; want 27 ending with the soak", len(long), long[len(long)-1].name)
	}
	if _, err := selectCases(runOptions{cases: []string{"23"}}); err == nil {
		t.Fatal("case 23 without -long should be refused")
	}
	soak, _ := caseByName("soak")
	if soak.requiredUnder(gateConfig{FastPath: true}) {
		t.Fatal("the soak (release tier minor) must not count toward the exit code")
	}
}

// TestRunWritesARedactedReport: runWith redacts the report it writes (05 §18.7): a non-fixture app in the events is
// "other", a work-folder path in a problem is relative, a name the driver collected is gone; -no-redact keeps them
// and names the file "-unredacted".
func TestRunWritesARedactedReport(t *testing.T) {
	work := `C:\Users\Ada Example\AppData\Local\Temp\isshoni-audiotest-41`
	broken := inconclusive(mustCase(t, "timestamps"), gateConfig{FastPath: true},
		"the case could not run: copy %s\\Bursts\\bursts.exe: denied by Spotify Deluxe", work)
	r2 := recForCase2(8)
	r2.roleApp = map[string]string{"game": "exe:game.exe", "voice": "discord"}
	r2.events = []recEvent{{t: 2, code: "session_created", app: "exe:spotify.exe"},
		{t: 2.5, code: "stream_running", app: "exe:game.exe"}}
	drv := &stubDriver{endpoints: 1, mic: true,
		recs:     map[string]recording{"builtin-voice": r2},
		specials: map[string]caseReport{"timestamps": broken},
		redact:   redactInfo{workDirs: []string{work}, names: []string{"Spotify Deluxe", "spotify.exe"}}}

	for _, noRedact := range []bool{false, true} {
		dir := t.TempDir()
		var b strings.Builder
		runWith(&b, drv, runOptions{cases: []string{"2", "20"}, outDir: dir, noRedact: noRedact})
		files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
		if len(files) != 1 {
			t.Fatalf("noRedact=%v: %d report files", noRedact, len(files))
		}
		raw, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		js := string(raw)
		unredactedName := strings.HasSuffix(files[0], unredactedSuffix+".json")
		if noRedact {
			if !unredactedName || !strings.Contains(js, "exe:spotify.exe") {
				t.Fatalf("-no-redact: file %s, want the -unredacted name and the raw app id", files[0])
			}
			continue
		}
		if unredactedName {
			t.Fatalf("a redacted report is named %s", files[0])
		}
		for _, leak := range []string{"Ada Example", "Spotify", "spotify", `C:\\`, "AppData"} {
			if strings.Contains(js, leak) {
				t.Errorf("the report leaks %q:\n%s", leak, js)
			}
		}
		for _, want := range []string{`"app": "other"`, `"app": "fixture:game"`, `Bursts\\bursts.exe`} {
			if !strings.Contains(js, want) {
				t.Errorf("the report lacks %q:\n%s", want, js)
			}
		}
	}
}
