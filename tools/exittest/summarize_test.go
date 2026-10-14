package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSummarizeFixture summarizes the recorded fixture: the output is testdata/summary.md, which shows what the
// owner pastes into the results file.
func TestSummarizeFixture(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"summarize", filepath.Join("testdata", "run.csv")}, &stdout, &stderr); code != 0 {
		t.Fatalf("summarize = %d: %s", code, stderr.String())
	}
	golden := filepath.Join("testdata", "summary.md")
	if *update {
		if err := os.WriteFile(golden, stdout.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden) //nolint:gosec // G304: the test's own golden file
	if err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != string(want) {
		t.Errorf("summarize testdata/run.csv differs from %s (run with -update):\n%s", golden, got)
	}
	// What the summary must say about the simulated session.
	for _, want := range []string{
		"| Session | 2 h; the server restarts only at T+60 | recorded T-15:20 to T+119:44; restart at T+60:00; no isshoni process in 1 sample | pass |",
		"| UDP socket errors | RcvbufErrors + SndbufErrors = 0, or < 0.01% of datagrams | 3 of ",
		// The simulated outage: SIGTERM at T+59:38, all 5 back at T+60:04.
		"5 participants in the old process's last full scrape (T+59:10) and again from T+60:10: the outage lasted " +
			"between 10s and 1m0s (scraped every 30s)",
		"| Viewing quality | freezes < 1% of watch time on desktop, < 3% on phones; no decode errors that need a reload | " +
			"130.6 s frozen in 16.1 h of video tracks watched (0.23%, every device together)",
		"none at T+18:40, T+19:10, T+19:40, T+20:10",
		"| 1 | 4127 | T-15:20 | T+59:40 |",
		"| 2 | 31877 | T+60:00 | T+119:40 |",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("the summary has no %q", want)
		}
	}
	for _, row := range []string{"| Server CPU |", "| Memory |", "| Goroutines |"} {
		if !strings.Contains(lineWith(stdout.String(), row), "| pass |") {
			t.Errorf("%s does not pass: %s", row, lineWith(stdout.String(), row))
		}
	}
	// No address, host name or person in it.
	for _, banned := range []string{"127.0.0.1", "http", "localhost"} {
		if strings.Contains(stdout.String(), banned) {
			t.Errorf("the summary contains %q", banned)
		}
	}
}

func lineWith(s, prefix string) string {
	for line := range strings.Lines(s) {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

func TestParseT0(t *testing.T) {
	first := time.Date(2026, 10, 15, 23, 45, 0, 0, time.UTC)
	for s, want := range map[string]time.Time{
		"00:00":                     time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC), // across midnight
		"23:59:30":                  time.Date(2026, 10, 15, 23, 59, 30, 0, time.UTC),
		"12:00":                     time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC),
		"2026-10-16T02:00:00+02:00": time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC),
	} {
		got, err := parseT0(s, first)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseT0(%q) = %v, %v; want %v", s, got, err, want)
		}
	}
	for _, bad := range []string{"noon", "25:00", "2026-10-16", "18h00"} {
		if _, err := parseT0(bad, first); err == nil {
			t.Errorf("parseT0(%q) succeeded", bad)
		}
	}
}

func TestSummarizeT0(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "run.csv"))
	if err != nil {
		t.Fatal(err)
	}
	rf, err := readRun(f)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		t0, session, result string
	}{
		{"18:00", "recorded T-15:00 to T+120:04; restart at T+60:20", "pass"},
		{"18:00:00", "recorded T-15:00 to T+120:04; restart at T+60:20", "pass"},
		{"2026-10-15T20:00:00+02:00", "recorded T-15:00 to T+120:04; restart at T+60:20", "pass"},
		{"18:10", "recorded T-25:00 to T+110:04; restart at T+50:20", "check"}, // the restart is 10 minutes early and the end is missing
	} {
		a, err := analyze(rf, tc.t0)
		if err != nil {
			t.Fatal(err)
		}
		if a.anchor != "set with -t0" {
			t.Errorf("-t0 %s: anchor %q", tc.t0, a.anchor)
		}
		v := a.sessionVerdict()
		if !strings.HasPrefix(v.measured, tc.session) || v.result != tc.result {
			t.Errorf("-t0 %s: session %q %s, want %q %s", tc.t0, v.measured, v.result, tc.session, tc.result)
		}
	}
}

// row builds a run.csv row at simT0+at.
func row(at time.Duration, kind string, pid int, vals map[string]float64) *runRow {
	if vals == nil {
		vals = map[string]float64{}
	}
	return &runRow{t: simT0.Add(at), kind: kind, pid: pid, vals: vals}
}

func runOf(t *testing.T, rows ...*runRow) *runFile {
	t.Helper()
	head := encodeHeader()
	var b bytes.Buffer
	b.Write(head)
	for _, r := range rows {
		line, err := encodeRow(r)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
	}
	rf, err := readRun(&b)
	if err != nil {
		t.Fatal(err)
	}
	if len(rf.skipped) > 0 {
		t.Fatal(rf.skipped)
	}
	return rf
}

// TestCounterResets: totals per process and in all, Prometheus style. The first process's first scrape is its
// baseline; a counter that goes down within a process counts as reset; a new process's first scrape counts whole,
// with a restart row or without one; the machine's counters run across the restart and only reset on a reboot.
func TestCounterResets(t *testing.T) {
	const egress, resumed = "sfu_egress_bytes_total", "ws_resume_resumed_total"
	s := time.Second
	rf := runOf(t,
		row(0, kindStart, 0, nil),
		row(0, kindSample, 10, map[string]float64{colNICTx: 1000, colTCPRetrans: 10}),
		row(0, kindScrape, 10, map[string]float64{egress: 5000}),
		row(10*s, kindSample, 10, map[string]float64{colNICTx: 2000}),
		row(20*s, kindSample, 10, map[string]float64{colNICTx: 500, colTCPRetrans: 15}), // the NIC's counter reset
		row(30*s, kindScrape, 10, map[string]float64{egress: 7000, resumed: 2}),
		row(60*s, kindScrape, 10, map[string]float64{egress: 1000, resumed: 3}), // a reset inside the process
		row(90*s, kindScrape, 10, map[string]float64{egress: 1500}),
		row(95*s, kindDown, 0, map[string]float64{colNICTx: 600}),
		row(95*s, kindScrapeError, 0, nil),
		row(100*s, kindRestart, 20, nil),
		row(100*s, kindSample, 20, map[string]float64{colNICTx: 900}),
		row(120*s, kindScrape, 20, map[string]float64{egress: 300}),
		row(150*s, kindScrape, 20, map[string]float64{egress: 900}),
		row(180*s, kindScrape, 30, map[string]float64{egress: 50}), // a new PID without a restart row
		row(190*s, kindStop, 0, nil),
	)
	a, err := analyze(rf, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(a.segs) != 3 {
		t.Fatalf("%d processes, want 3", len(a.segs))
	}
	for i, want := range []float64{3500, 900, 50} {
		if got := a.segs[i].metric[egress].total; got != want {
			t.Errorf("process %d: egress %v, want %v", i+1, got, want)
		}
	}
	if got := a.metric[egress].total; got != 4450 {
		t.Errorf("egress in all %v, want 4450", got)
	}
	// A labeled series that first shows in the second scrape of the process counts whole: 2, then 1 more.
	if got := a.segs[0].metric[resumed].total; got != 3 {
		t.Errorf("process 1: resumed %v, want 3", got)
	}
	// A machine counter missing from one sample adds up from the last sample that had it: 10 to 15.
	if got := a.host[colTCPRetrans].total; got != 5 {
		t.Errorf("TCP retransmissions %v, want 5", got)
	}
	// The peak rate leaves out the resets: 2000 in 30 s, 500 in 30 s, 600 in 30 s.
	if got := a.metric[egress].peak; got != 2000.0/30 {
		t.Errorf("egress peak %v/s, want %v", got, 2000.0/30)
	}
	// The machine: +1000, +500 (reboot), +100 (the down row: in all only), +300.
	if got := a.host[colNICTx].total; got != 1900 {
		t.Errorf("NIC sent in all %v, want 1900", got)
	}
	if got := a.segs[0].host[colNICTx].total; got != 1500 {
		t.Errorf("NIC sent by process 1 %v, want 1500", got)
	}
	if got := a.segs[1].host[colNICTx].total; got != 300 {
		t.Errorf("NIC sent by process 2 %v, want 300", got)
	}
	if a.scrapeErrs != 1 || a.downs != 1 || a.samples != 4 || a.scrapes != 7 || a.starts != 1 {
		t.Errorf("counts: %d scrape errors, %d downs, %d samples, %d scrapes, %d starts", a.scrapeErrs, a.downs,
			a.samples, a.scrapes, a.starts)
	}
	// T+0 from the first restart.
	if want := simT0.Add(100*s - time.Hour); !a.t0.Equal(want) {
		t.Errorf("T+0 = %v, want %v", a.t0, want)
	}
	if v := a.sessionVerdict(); v.result != resFail || !strings.Contains(v.measured, "restarts at T+60:00, T+61:20") {
		t.Errorf("session with two restarts: %+v", v)
	}
	// One counter that went down is no restart in place: that takes two of resetCounters at the same scrape.
	if len(a.resets) != 0 {
		t.Errorf("in-place restarts at %v, want none", a.resets)
	}
	const trOut = "transfer_egress_bytes_total"
	a, err = analyze(runOf(t,
		row(0, kindSample, 10, nil),
		row(0, kindScrape, 10, map[string]float64{egress: 5000, trOut: 6000, "process_cpu_seconds_total": 10}),
		row(30*s, kindScrape, 10, map[string]float64{egress: 7000, trOut: 8000, "process_cpu_seconds_total": 11}),
		row(60*s, kindScrape, 10, map[string]float64{egress: 100, trOut: 200, "process_cpu_seconds_total": 12}),
	), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(a.resets) != 1 || !a.resets[0].Equal(simT0.Add(60*s)) || a.metric[egress].total != 2100 {
		t.Errorf("in-place restarts at %v, egress %v; want one at %v and 2100", a.resets, a.metric[egress].total,
			simT0.Add(60*s))
	}
}

func TestSummarizeVerdicts(t *testing.T) {
	m := time.Minute
	// One process, no restart: T+0 is 15 minutes after the first row. The CPU is too high, RSS grows by more than
	// a fifth, goroutines don't come back, UDP buffers overflow, a third of the time is frozen, nobody on TCP 443.
	var rows []*runRow
	rows = append(rows, row(-15*m, kindStart, 0, nil))
	ticks := 0.0
	for i := 0; i <= 135*6; i++ {
		at := -15*m + time.Duration(i)*10*time.Second
		cpuPct := 30.0
		if at >= 90*m && at < 91*m {
			cpuPct = 95
		}
		ticks += cpuPct / 100 * 2 * 10 * userHZ
		rss := 100e6 + float64(i)*1e5
		rows = append(rows, row(at, kindSample, 42, map[string]float64{
			colCPUTicks: ticks, colCPUs: 2, colRSS: rss, colHostCPUPct: cpuPct + 2,
			colUDPIn: float64(i) * 1000, colUDPOut: float64(i) * 1000, colUDPRcvbufErrs: float64(i), colUDPSndbufErrs: 0,
		}))
		if i%3 == 0 {
			rows = append(rows, row(at, kindScrape, 42, map[string]float64{
				"go_goroutines": 100 + float64(i), "participants": 5, "shares": 2, "transport_tcp443": 0,
				colVideoHigh: 4, colVideoLow: 4, "client_freeze_seconds_total": float64(i) * 50,
			}))
		}
	}
	a, err := analyze(runOf(t, rows...), "")
	if err != nil {
		t.Fatal(err)
	}
	if !a.t0.Equal(simT0) {
		t.Errorf("T+0 = %v, want %v (15 minutes after the first row)", a.t0, simT0)
	}
	got := map[string]verdict{}
	for _, v := range a.verdicts() {
		got[v.measure] = v
	}
	for measure, want := range map[string]string{
		"Session": resCheck, "Server CPU": resFail, "Memory": resFail, "Goroutines": resCheck,
		"UDP socket errors": resFail, "Recovery": resManual, "Viewing quality": resFail, "ICE-TCP": resFail,
		"Manual fixes": resManual, "Phones": resManual, "Survey (1-5)": resManual,
	} {
		if got[measure].result != want {
			t.Errorf("%s: %s (%s), want %s", measure, got[measure].result, got[measure].measured, want)
		}
	}
	if !strings.Contains(got["Server CPU"].measured, "average 30.5%") || !strings.Contains(got["Server CPU"].measured, "peak 95.0%") {
		t.Errorf("Server CPU: %s", got["Server CPU"].measured)
	}
	if !strings.Contains(got["Session"].measured, "no restart") {
		t.Errorf("Session: %s", got["Session"].measured)
	}
}

// sessionKnobs change healthySession.
type sessionKnobs struct {
	drop      func(at time.Duration) bool // leaves out the rows at these offsets from T+0
	resetAt   time.Duration               // the counters start again at 0 here, the PID unchanged (0: never)
	backAt    time.Duration               // the new process has all 5 participants from here (0: T+60:30)
	freezePct float64                     // the share of the video tracks' time that is frozen
	traffic   float64                     // UDP datagrams in and out per sample (0: none)
}

// healthySession records a session that passes: a sample every 10 s and a scrape every 30 s from T-15 to T+120,
// the restart at T+60 (PID 100, then 200; the 10 s between the two processes' samples are not covered), 5
// participants who watch 2 shares (8 video tracks), 20% CPU, a flat
// RSS and goroutine count, no UDP socket error, a PeerConnection on TCP 443.
func healthySession(k sessionKnobs) []*runRow {
	if k.backAt == 0 {
		k.backAt = mm(60, 30)
	}
	rows := []*runRow{row(-15*time.Minute, kindStart, 0, nil)}
	add := func(r *runRow) {
		if k.drop == nil || !k.drop(r.t.Sub(simT0)) {
			rows = append(rows, r)
		}
	}
	var ticks, counters, freeze, udp float64
	for i := 0; i <= 135*6; i++ {
		at := -15*time.Minute + time.Duration(i)*10*time.Second
		pid := 100
		if at >= 60*time.Minute {
			pid = 200
		}
		if at == 60*time.Minute || k.resetAt != 0 && at == k.resetAt {
			ticks, counters, freeze = 0, 0, 0
			if at == 60*time.Minute {
				add(row(at, kindRestart, pid, nil))
			}
		}
		ticks += 0.2 * 2 * 10 * userHZ
		if at == k.resetAt {
			ticks += 1000 // the kernel's CPU time survives an exec
		}
		udp += k.traffic
		add(row(at, kindSample, pid, map[string]float64{
			colCPUTicks: ticks, colCPUs: 2, colRSS: 100e6, colHostCPUPct: 25, colUDPIn: udp, colUDPOut: udp,
			colUDPRcvbufErrs: 0, colUDPSndbufErrs: 0,
		}))
		if i%3 != 0 {
			continue
		}
		participants := 5.0
		if at >= 60*time.Minute && at < k.backAt {
			participants = 2
		}
		counters += 1e6
		freeze += 8 * 30 * k.freezePct / 100
		add(row(at, kindScrape, pid, map[string]float64{
			"go_goroutines": 200, "participants": participants, "shares": 2, "transport_tcp443": 1,
			colVideoHigh: 4, colVideoLow: 4, "client_freeze_seconds_total": freeze,
			"sfu_ingress_bytes_total": counters, "sfu_egress_bytes_total": 4 * counters,
			"transfer_ingress_bytes_total": counters, "transfer_egress_bytes_total": 4 * counters,
		}))
	}
	return rows
}

func verdictsOf(t *testing.T, t0 string, rows []*runRow) map[string]verdict {
	t.Helper()
	a, err := analyze(runOf(t, rows...), t0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]verdict{}
	for _, v := range a.verdicts() {
		got[v.measure] = v
	}
	return got
}

// TestSummarizeCoverage: a verdict passes only on what the recording measured. Gaps, a short recording, a missed
// stress window and a restart that kept its PID make the rows "check"; no traffic is "n/a".
func TestSummarizeCoverage(t *testing.T) {
	between := func(from, to time.Duration) func(time.Duration) bool {
		return func(at time.Duration) bool { return at > from && at < to }
	}
	for _, tc := range []struct {
		name string
		t0   string
		rows []*runRow
		want map[string]string // measure: result, then what the measured text must contain
	}{
		{"healthy", "", healthySession(sessionKnobs{freezePct: 0.2, traffic: 1000}), map[string]string{
			"Session": "pass", "Server CPU": "pass|the samples cover 99.9% of T+0 to T+120", "Memory": "pass",
			"Goroutines": "pass", "UDP socket errors": "pass|0 of ", "Recovery": "manual|lasted between 0s and 1m0s",
			"Viewing quality": "manual|(0.20%, every device together)", "ICE-TCP": "manual",
		}},
		{"a 53-minute gap", "18:00", healthySession(sessionKnobs{traffic: 1000, drop: between(mm(5, 0), mm(58, 0))}),
			map[string]string{
				"Session":    "check|no sample from T+5:00 to T+58:00",
				"Server CPU": "check|the samples cover 55.7% of T+0 to T+120", "Memory": "pass", "UDP socket errors": "pass",
			}},
		{"the stress missed", "", healthySession(sessionKnobs{traffic: 1000, drop: between(mm(90, 0), mm(100, 10))}),
			map[string]string{
				"Session":    "check|no sample from T+90:00 to T+100:10",
				"Server CPU": "check|the samples cover 91.4% of T+0 to T+120, none of the stress (T+90 to T+100)",
			}},
		{"ends at T+70", "", healthySession(sessionKnobs{traffic: 1000, drop: func(at time.Duration) bool { return at > mm(70, 0) }}),
			map[string]string{
				"Session": "check|recorded T-15:00 to T+70:00", "Server CPU": "check|the samples cover 58.2%",
				"Memory": "check|short of T+65 to T+120", "UDP socket errors": "check|does not cover T+0 to T+120",
			}},
		{"in-place restart", "", healthySession(sessionKnobs{traffic: 1000, resetAt: mm(30, 0)}), map[string]string{
			"Session": "check|counters reset at T+30:00 without a PID change (in-place restart?)",
		}},
		{"9 seconds without traffic", "18:00", []*runRow{
			row(0, kindStart, 0, nil),
			row(0, kindSample, 7, map[string]float64{colCPUTicks: 100, colCPUs: 2, colUDPIn: 5, colUDPOut: 5,
				colUDPRcvbufErrs: 0, colUDPSndbufErrs: 0}),
			row(9*time.Second, kindSample, 7, map[string]float64{colCPUTicks: 101, colCPUs: 2, colUDPIn: 5, colUDPOut: 5,
				colUDPRcvbufErrs: 0, colUDPSndbufErrs: 0}),
		}, map[string]string{
			"Session": "check", "Server CPU": "check|the samples cover 0.1% of T+0 to T+120", "UDP socket errors": "n/a|0 of 0",
		}},
		{"slow recovery", "", healthySession(sessionKnobs{traffic: 1000, backAt: mm(61, 30)}), map[string]string{
			"Recovery": "check|last full scrape (T+59:30) and again from T+61:30: the outage lasted between 1m0s and 2m0s",
		}},
		{"freezes per track", "", healthySession(sessionKnobs{traffic: 1000, freezePct: 2.5}), map[string]string{
			// 4% of the participants' time, which a viewer of two shares counts twice: under the phones' 3%.
			"Viewing quality": "manual|(2.51%, every device together)",
		}},
		{"freezes over 3%", "", healthySession(sessionKnobs{traffic: 1000, freezePct: 3.5}), map[string]string{
			"Viewing quality": "FAIL|(3.51%, every device together)",
		}},
	} {
		got := verdictsOf(t, tc.t0, tc.rows)
		for measure, want := range tc.want {
			result, text, _ := strings.Cut(want, "|")
			if v := got[measure]; v.result != result || !strings.Contains(v.measured, text) {
				t.Errorf("%s: %s: %s (%s), want %s with %q", tc.name, measure, v.result, v.measured, result, text)
			}
		}
	}
}

func TestSummarizeErrors(t *testing.T) {
	head := encodeHeader()
	for _, tc := range []struct {
		name, data, want string
	}{
		{"empty", "", "the file is empty"},
		{"other CSV", "a,b\n1,2\n", "no time or kind column"},
		{"header only", string(head), "no rows"},
		{"only down rows", string(head) + "2026-10-15T18:00:00.000Z,down" + strings.Repeat(",", len(header())-2) + "\n",
			"no sample of an isshoni process"},
	} {
		rf, err := readRun(strings.NewReader(tc.data))
		if err == nil {
			_, err = analyze(rf, "")
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestReadRunTolerance(t *testing.T) {
	head := encodeHeader()
	// A byte order mark, columns in another order and one unknown column, rows that don't parse.
	data := byteOrderMark + "kind,time,pid,extra,rss_bytes,note\n" +
		"sample,2026-10-15T18:00:00Z,5,x,100,\"a, quoted note\"\n" +
		"sample,yesterday,5,x,100,\n" +
		"teleport,2026-10-15T18:00:10Z,5,x,100,\n" +
		"sample,2026-10-15T18:00:20Z,-3,x,100,\n" +
		"sample,2026-10-15T18:00:30Z,5,x,NaN,\n" +
		"sample,2026-10-15T18:00:40Z,5\n" +
		"sample,2026-10-15T18:00:50Z,5,x,1e3,\"cut sh"
	rf, err := readRun(strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(rf.rows) != 2 || rf.rows[0].note != "a, quoted note" || rf.rows[0].vals[colRSS] != 100 || rf.rows[1].has(colRSS) {
		t.Errorf("rows %+v", rf.rows)
	}
	if len(rf.skipped) != 5 {
		t.Errorf("skipped %q, want 5", rf.skipped)
	}
	// What the collector writes reads back exactly.
	r := &runRow{t: simT0, kind: kindSample, pid: 7, vals: map[string]float64{colRSS: 123456789012, colCPUPct: 12.34,
		colLoad1: 0.07, "client_freeze_seconds_total": 41.5}, note: `pid 1 -> 2; "quoted", and, commas`}
	line, err := encodeRow(r)
	if err != nil {
		t.Fatal(err)
	}
	rf, err = readRun(bytes.NewReader(append(head, line...)))
	if err != nil || len(rf.rows) != 1 {
		t.Fatalf("read back: %v %v", rf, err)
	}
	back := rf.rows[0]
	back.line = 0
	if !back.t.Equal(r.t) || back.kind != r.kind || back.pid != r.pid || back.note != r.note || len(back.vals) != len(r.vals) {
		t.Errorf("read back %+v, want %+v", back, r)
	}
	for k, v := range r.vals {
		if back.vals[k] != v {
			t.Errorf("%s read back as %v, want %v", k, back.vals[k], v)
		}
	}
}

func TestFormatting(t *testing.T) {
	for v, want := range map[float64]string{
		0: "0", 7: "7", 999: "999", 1000: "1,000", 1234567: "1,234,567", 12.34: "12.3", 0.96: "1", 1999.95: "2,000",
		-1500: "-1,500", -0.01: "0",
	} {
		if got := count(v); got != want {
			t.Errorf("count(%v) = %q, want %q", v, got, want)
		}
	}
	for v, want := range map[float64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KiB", 52 << 20: "52.0 MiB", 41.2 * (1 << 30): "41.2 GiB"} {
		if got := bytesStr(v); got != want {
			t.Errorf("bytesStr(%v) = %q, want %q", v, got, want)
		}
	}
	a := &analysis{t0: simT0}
	for d, want := range map[time.Duration]string{
		0: "T+0:00", -15 * time.Minute: "T-15:00", 60*time.Minute + 5*time.Second: "T+60:05", 125 * time.Minute: "T+125:00",
		-1400 * time.Millisecond: "T-0:01",
	} {
		if got := a.tOff(simT0.Add(d)); got != want {
			t.Errorf("tOff(%v) = %q, want %q", d, got, want)
		}
	}
	if got := bitRate(45.3e6 / 8); got != "45.3 Mbit/s" {
		t.Errorf("bitRate = %q", got)
	}
}

func FuzzSummarize(f *testing.F) {
	head := encodeHeader()
	fixture, err := os.ReadFile(filepath.Join("testdata", "run.csv"))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(fixture[:min(len(fixture), 6000)], "")
	f.Add(append(head, "2026-10-15T18:00:00Z,sample,5,1,,1,,,2\n2026-10-15T18:00:10Z,restart,6\n"...), "18:00")
	f.Add([]byte("time,kind,pid,cpu_ticks\n2026-10-15T18:00:00Z,sample,1,5\n2026-10-15T17:00:00Z,sample,1,1\n"), "")
	f.Fuzz(func(t *testing.T, data []byte, t0 string) {
		rf, err := readRun(bytes.NewReader(data))
		if err != nil {
			return
		}
		a, err := analyze(rf, t0)
		if err != nil {
			return
		}
		var out bytes.Buffer
		if err := a.write(&out, "run.csv"); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(out.String(), "run.csv: recorded ") {
			t.Fatalf("output starts %q", out.String()[:min(out.Len(), 80)])
		}
	})
}
