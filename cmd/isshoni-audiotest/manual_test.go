package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These are the `record` and `merge` tests (05 §18.2, §18.8 step 5): WA62b's acceptance "a merge round trip on golden
// reports" (testdata/merge: a redacted gate report, two manual entries, and the merged report), the refusals (another
// OS build, a conflicting answer, an unredacted report, an unknown field), and record's flow with a stub driver.
// Golden files are compared with line ends normalized: a Windows checkout may turn them into CRLF.

var update = flag.Bool("update", false, "rewrite the golden files of testdata/merge")

const mergeDir = "testdata/merge"

// goldenReport is the input report of the golden merge: a redacted Win11 gate report as `run -gate` writes it.
func goldenReport() report {
	vol, muted := 0.42, false
	cs2, _ := caseByName("two-voice-apps")
	cs15, _ := caseByName("runtime-rule")
	cs27, _ := caseByName("pid-reuse-tree")
	cfg := gateConfig{FastPath: true}
	c2 := newCaseReport(cs2, cfg)
	c2.Result = resultPass
	c2.Checks = []checkReport{
		{Freq: 1000, From: 1.5, Expect: expectPresent, LevelDB: -18.1, Pass: true},
		{Freq: 440, From: 1.5, Expect: expectAbsent, LevelDB: -141.7, MaxShortDB: -128.4, MaxShortAtS: 6.31,
			Pass: true},
	}
	c2.Engine = []string{"include-set"}
	c2.Counters = map[string]int{"underruns": 0, "trims": 0}
	c2.Events = []reportEvent{{TMs: 1210, Code: "stream_running", App: "fixture:game"},
		{TMs: 1500, Code: "session_created", App: otherRedacted}}
	c15 := newCaseReport(cs15, cfg)
	c15.Result = resultPass
	c15.Checks = []checkReport{
		{Freq: 440, From: 1.5, To: 4, Expect: expectPresent, LevelDB: -18.2, Pass: true},
		{Freq: 440, From: 4.153, Expect: expectAbsent, LevelDB: -139, MaxShortDB: -61.7, MaxShortAtS: 4.178,
			Pass: true},
	}
	c15.Measured = map[string]measure{measureCut: {ValueMs: 58, BudgetMs: budgetCutMs}}
	c15.Engine = []string{"include-set"}
	c27 := newCaseReport(cs27, cfg)
	c27.Result = resultInfo
	c27.Info = map[string]any{"matched": true, "attempts": 37, "captured": false, "gameReason": "pid_reuse"}
	cases := []caseReport{c2, c15, c27}
	return report{
		V: reportVersion, Harness: "v0.2.0-14-g1a2b3c4",
		DLL: dllInfo{Version: "0.2.0", SHA256: strings.Repeat("3f9a1c0b", 8)},
		OS:  osInfo{Product: "win11", Build: 26200, UBR: 6899, Edition: "Professional"},
		Machine: machineInfo{CPU: "AuthenticAMD", RenderEndpoints: []endpointInfo{{FormFactor: "Headphones",
			Bus: "usb"}}, Mic: true, MasterVolume: &vol, Muted: &muted, DuckingPreference: 3},
		Gate: true, GateCfg: cfg, Summary: summarize(cases, exitCode(cases)), Cases: cases,
	}
}

// goldenManuals are the two manual entries of the golden merge.
func goldenManuals() []manualFile {
	osi := osInfo{Product: "win11", Build: 26200, UBR: 6899, Edition: "Professional"}
	return []manualFile{{Label: "R1", Pass: true, OS: osi}, {Label: "R3", Pass: true, OS: osi}}
}

// readGolden reads a golden file with its line ends normalized to LF.
func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(mergeDir, name))
	if err != nil {
		t.Fatalf("golden %s: %v (run go test -run TestMergeGolden -update to write it)", name, err)
	}
	return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
}

// writeGoldens rewrites testdata/merge from goldenReport and goldenManuals (go test -update).
func writeGoldens(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(mergeDir, 0o750); err != nil {
		t.Fatal(err)
	}
	b, err := encodeReport(goldenReport())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mergeDir, "report.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, m := range goldenManuals() {
		mb, _ := json.MarshalIndent(m, "", "  ")
		if err := os.WriteFile(filepath.Join(mergeDir, manualFileName(m.Label)), append(mb, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	merged := goldenReport()
	if err := mergeManual(&merged, goldenManuals()); err != nil {
		t.Fatal(err)
	}
	mb, err := encodeReport(merged)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mergeDir, "merged.json"), mb, 0o600); err != nil {
		t.Fatal(err)
	}
}

// copyGolden copies a golden file (LF) into dir and returns its path.
func copyGolden(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, readGolden(t, name), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestMergeGolden is the merge round trip on the golden reports: `merge report.json manual-*.json` gives exactly
// merged.json; merging the same entries again changes nothing; merged.json reads back and writes out byte for byte
// (no field is lost); and the golden input is what this harness writes.
func TestMergeGolden(t *testing.T) {
	if *update {
		writeGoldens(t)
	}
	want := readGolden(t, "merged.json")
	if b, _ := encodeReport(goldenReport()); !bytes.Equal(b, readGolden(t, "report.json")) {
		t.Fatal("testdata/merge/report.json is not what this harness writes for goldenReport (run with -update)")
	}

	dir := t.TempDir()
	rep := copyGolden(t, dir, "report.json")
	copyGolden(t, dir, "manual-R1.json")
	copyGolden(t, dir, "manual-R3.json")
	var out strings.Builder
	// The pattern is expanded by merge itself (PowerShell passes it as it is).
	if got := cmdMerge(&out, []string{rep, filepath.Join(dir, "manual-*.json")}); got != exitAllPass {
		t.Fatalf("merge: exit %d\n%s", got, out.String())
	}
	got, err := os.ReadFile(rep)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("merged report differs from merged.json:\n%s", got)
	}

	// Merging again (into another file with -o) changes nothing.
	again := filepath.Join(dir, "again.json")
	if got := cmdMerge(&out, []string{"-o", again, rep, filepath.Join(dir, "manual-R1.json")}); got != exitAllPass {
		t.Fatalf("merge again: exit %d\n%s", got, out.String())
	}
	if b, _ := os.ReadFile(again); !bytes.Equal(b, want) {
		t.Fatalf("merging the same entry twice changed the report:\n%s", b)
	}

	// The merged report reads back and writes out byte for byte.
	r, err := readReport(again)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := encodeReport(r); !bytes.Equal(b, want) {
		t.Fatalf("merged.json does not round-trip:\n%s", b)
	}
	if len(r.Manual) != 2 || r.Manual[0] != (manualEntry{Label: "R1", Pass: true}) || r.Manual[1].Label != "R3" {
		t.Fatalf("manual entries %+v", r.Manual)
	}
}

// TestMergeRefuses: an entry from another OS build (product, build or UBR), a label already merged with the other
// answer, an unredacted report, a report with a field this harness doesn't know, another schema version and a
// pattern that matches nothing are refused, and the report on disk stays as it was.
func TestMergeRefuses(t *testing.T) {
	dir := t.TempDir()
	rep := copyGolden(t, dir, "report.json")
	original, _ := os.ReadFile(rep)
	writeManual := func(name string, m manualFile) string {
		p := filepath.Join(dir, name)
		b, _ := json.Marshal(m)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	osi := osInfo{Product: "win11", Build: 26200, UBR: 6899}
	cases := []struct {
		name string
		args func() []string
		want string
	}{
		{"another build", func() []string {
			return []string{rep, writeManual("m1.json", manualFile{Label: "R1", Pass: true,
				OS: osInfo{Product: "win11", Build: 26100, UBR: 6899}})}
		}, "another OS build"},
		{"another UBR", func() []string {
			return []string{rep, writeManual("m2.json", manualFile{Label: "R1", Pass: true,
				OS: osInfo{Product: "win11", Build: 26200, UBR: 7000}})}
		}, "26200.7000"},
		{"another product", func() []string {
			return []string{rep, writeManual("m3.json", manualFile{Label: "R1", Pass: true,
				OS: osInfo{Product: "win10", Build: 26200, UBR: 6899}})}
		}, "win10"},
		{"conflicting answers", func() []string {
			return []string{rep, writeManual("m4.json", manualFile{Label: "R1", Pass: true, OS: osi}),
				writeManual("m5.json", manualFile{Label: "R1", Pass: false, OS: osi})}
		}, "pass=true"},
		{"bad label", func() []string {
			return []string{rep, writeManual("m6.json", manualFile{Label: `..\x`, Pass: true, OS: osi})}
		}, "invalid label"},
		{"no match", func() []string { return []string{rep, filepath.Join(dir, "nothing-*.json")} }, "no file"},
	}
	for _, tc := range cases {
		var out strings.Builder
		if got := cmdMerge(&out, tc.args()); got != exitSetup || !strings.Contains(out.String(), tc.want) {
			t.Errorf("%s: exit %d, output %q; want exit %d naming %q", tc.name, got, out.String(), exitSetup, tc.want)
		}
		if b, _ := os.ReadFile(rep); !bytes.Equal(b, original) {
			t.Fatalf("%s: a refused merge changed the report", tc.name)
		}
	}

	// A report that isn't redacted, one with an unknown field, and one of another schema version are refused.
	m := writeManual("ok.json", manualFile{Label: "R1", Pass: true, OS: osi})
	bad := []struct{ name, edit, want string }{
		{"unredacted", `"app": "exe:spotify.exe"`, "-no-redact"},
		{"unknown field", `"app": "other", "extra": 1`, "can read"},
	}
	for _, tc := range bad {
		p := filepath.Join(dir, tc.name+".json")
		js := strings.Replace(string(original), `"app": "other"`, tc.edit, 1)
		if err := os.WriteFile(p, []byte(js), 0o600); err != nil { //nolint:gosec // G703: the test's temp folder
			t.Fatal(err)
		}
		var out strings.Builder
		if got := cmdMerge(&out, []string{p, m}); got != exitSetup || !strings.Contains(out.String(), tc.want) {
			t.Errorf("%s report: exit %d, output %q", tc.name, got, out.String())
		}
	}
	v2 := filepath.Join(dir, "v2.json")
	v2body := bytes.Replace(original, []byte(`"v": 1`), []byte(`"v": 2`), 1)
	if err := os.WriteFile(v2, v2body, 0o600); err != nil { //nolint:gosec // G703: the test's temp folder
		t.Fatal(err)
	}
	var out strings.Builder
	if got := cmdMerge(&out, []string{v2, m}); got != exitSetup || !strings.Contains(out.String(), "v2") {
		t.Errorf("schema v2: exit %d, output %q", got, out.String())
	}
}

// TestMergeManualOrder: new labels are appended after the report's own entries in argument order; an entry the
// report already holds with the same answer is not added twice.
func TestMergeManualOrder(t *testing.T) {
	osi := osInfo{Product: "win10", Build: 19045, UBR: 6456}
	r := report{OS: osi, Manual: []manualEntry{{Label: "R3", Pass: true}}}
	err := mergeManual(&r, []manualFile{{Label: "R1", Pass: true, OS: osi}, {Label: "R3", Pass: true, OS: osi},
		{Label: "R7-teams", Pass: false, OS: osi}})
	if err != nil {
		t.Fatal(err)
	}
	want := []manualEntry{{Label: "R3", Pass: true}, {Label: "R1", Pass: true}, {Label: "R7-teams", Pass: false}}
	if len(r.Manual) != len(want) {
		t.Fatalf("manual %+v, want %+v", r.Manual, want)
	}
	for i := range want {
		if r.Manual[i] != want[i] {
			t.Fatalf("manual %+v, want %+v", r.Manual, want)
		}
	}
}

// TestRecord: `record` streams the tap into the WAV, prints what friends hear every second, asks until it gets yes
// or no, and stores {label, pass, os} in manual-<label>.json next to the WAV; the exit code is the answer.
func TestRecord(t *testing.T) {
	dir := t.TempDir()
	chunk := sineAt(pcmChunkFrames, freqGame, 0.125)
	osi := osInfo{Product: "win10", Build: 19045, UBR: 6456, Edition: "Core"}
	drv := &stubDriver{osi: osi, recordChunks: [][2][]float64{{chunk, chunk}, {chunk, chunk}},
		recordStatus: []hearStatus{{t: 1, heard: []string{"Game"},
			keptOut: []namedReason{{name: "Discord", reason: "builtin"}}}, {t: 2}}}
	open := func(driverOptions) (driver, error) { return drv, nil }
	wav := filepath.Join(dir, "r1.wav")
	var out strings.Builder
	got := recordWith(&out, strings.NewReader("maybe\nY\n"), []string{"-label", "R1", "-d", "2", "-o", wav,
		"-exclude", "Foo.exe, bar.exe"}, open)
	if got != exitAllPass {
		t.Fatalf("record: exit %d\n%s", got, out.String())
	}
	for _, want := range []string{"friends hear: Game / kept out: Discord (builtin)",
		"friends hear: nothing / kept out: nothing", "Pass? [y/n] Listen to"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if drv.recorded == nil || drv.recorded.dur != 2 || strings.Join(drv.recorded.exclude, ",") != "foo.exe,bar.exe" {
		t.Fatalf("the driver recorded %+v", drv.recorded)
	}
	l, r, err := readWAV(wav)
	if err != nil || len(l) != 2*pcmChunkFrames || len(r) != len(l) {
		t.Fatalf("WAV: %d/%d frames, err %v", len(l), len(r), err)
	}
	if math.Abs(l[100]-chunk[100]) > 1e-6 {
		t.Fatalf("WAV sample %g, want %g", l[100], chunk[100])
	}
	m, err := readManual(filepath.Join(dir, "manual-R1.json"))
	if err != nil || m != (manualFile{Label: "R1", Pass: true, OS: osi}) {
		t.Fatalf("manual entry %+v, err %v", m, err)
	}

	// "n" stores a failed check and exits 1; no answer stores nothing and exits 4.
	got = recordWith(&out, strings.NewReader("no\n"), []string{"-label", "R7-teams", "-d", "1", "-o",
		filepath.Join(dir, "r7.wav")}, open)
	if m, _ := readManual(filepath.Join(dir, "manual-R7-teams.json")); got != exitFail || m.Pass {
		t.Fatalf("answer no: exit %d, entry %+v", got, m)
	}
	got = recordWith(&out, strings.NewReader(""), []string{"-label", "R2", "-o", filepath.Join(dir, "r2.wav")}, open)
	if _, err := os.Stat(filepath.Join(dir, "manual-R2.json")); got != exitSetup || err == nil {
		t.Fatalf("no answer: exit %d, entry stored: %v", got, err == nil)
	}
}

// TestParseRecord: the label is required and must be a plain word; -d is bounded; -o defaults to the label.
func TestParseRecord(t *testing.T) {
	var out strings.Builder
	ro, err := parseRecord(&out, []string{"-label", "R4"})
	if err != nil || ro.out != "r4.wav" || ro.dur != 30 {
		t.Fatalf("defaults: %+v, err %v", ro, err)
	}
	for _, args := range [][]string{{}, {"-label", `R1\..\x`}, {"-label", "R1", "-d", "0"},
		{"-label", "R1", "-d", "7200"}, {"-label", "R1", "extra"}} {
		if _, err := parseRecord(&out, args); err == nil {
			t.Errorf("parseRecord(%q) accepted", args)
		}
	}
}
