package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// readRows reads a run.csv back with summarize's reader, which must not have to skip anything.
func readRows(t *testing.T, path string) []*runRow {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // G304: the test's own file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	rf, err := readRun(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(rf.skipped) > 0 {
		t.Fatalf("%s: rows skipped: %v", path, rf.skipped)
	}
	return rf.rows
}

func kinds(rows []*runRow) string {
	var k []string
	for _, r := range rows {
		k = append(k, r.kind)
	}
	return strings.Join(k, " ")
}

// fixedPID returns a pidFunc that answers what *pid holds (0: no process; -1: the lookup fails).
func fixedPID(pid *atomic.Int64) pidFunc {
	return func(context.Context) (int, error) {
		p := pid.Load()
		if p < 0 {
			return 0, errors.New("systemctl: Failed to connect to bus")
		}
		return int(p), nil
	}
}

// TestCollectSample drives single steps at chosen times: the CPU percentages, the machine's columns, a down row
// for each way a process can be missing, and a reused PID.
func TestCollectSample(t *testing.T) {
	h := newFakeHost(t)
	h.setProcess(procState{pid: 4127, ticks: 1000, start: 500, rssKiB: 51200, threads: 12, fds: 30})
	h.setHost(hostState{cpus: 2, busy: 10_000, idle: 90_000, load: [3]float64{0.5, 0.4, 0.3}, rx: 1000, tx: 2000,
		udpIn: 10, udpOut: 20, retrans: 3})
	var pid atomic.Int64
	pid.Store(4127)
	dir := t.TempDir()
	t0 := time.Date(2026, 10, 15, 18, 0, 0, 0, time.UTC)
	c, err := newCollector(collectOptions{
		out: filepath.Join(dir, "run.csv"), procDir: h.dir, interval: 10 * time.Second,
		scrapeInterval: 30 * time.Second, pid: fixedPID(&pid),
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	step := func(at time.Duration) {
		t.Helper()
		if err := c.sample(ctx, t0.Add(at)); err != nil {
			t.Fatal(err)
		}
	}
	step(0)
	// 10 s later: the process used 5 s of CPU on a 2-CPU machine (25%), the machine 8 s of 20 (40%).
	h.setProcess(procState{pid: 4127, ticks: 1500, start: 500, rssKiB: 52224, threads: 13, fds: 31})
	h.setHost(hostState{cpus: 2, busy: 10_800, idle: 91_200, load: [3]float64{0.6, 0.4, 0.3}, rx: 5000, tx: 9000,
		udpIn: 15, udpOut: 30, retrans: 4})
	step(10 * time.Second)
	// systemd names no process, then the lookup fails, then the process ends between the lookup and the read.
	pid.Store(0)
	step(20 * time.Second)
	pid.Store(-1)
	step(30 * time.Second)
	pid.Store(4127)
	h.removeProcess(4127)
	step(40 * time.Second)
	// The same PID, a process that started later: a restart, and no CPU % across it. Its fds can't be listed.
	h.setProcess(procState{pid: 4127, ticks: 20, start: 9000, rssKiB: 40960, threads: 9, fds: 5, unreadFDs: true})
	step(50 * time.Second)
	// A new PID.
	h.setProcess(procState{pid: 5000, ticks: 10, start: 9100, rssKiB: 30720, threads: 8, fds: 12})
	pid.Store(5000)
	step(60 * time.Second)
	if err := c.close(t0.Add(70*time.Second), "stopped by a signal"); err != nil {
		t.Fatal(err)
	}

	rows := readRows(t, filepath.Join(dir, "run.csv"))
	if got, want := kinds(rows), "start sample sample down down down restart sample restart sample stop"; got != want {
		t.Fatalf("kinds = %s, want %s", got, want)
	}
	if !strings.Contains(rows[0].note, "nic=eth0") || !strings.Contains(rows[0].note, "interval=10s") {
		t.Errorf("start note %q: want the interval and the default route's interface", rows[0].note)
	}
	first, second := rows[1], rows[2]
	if first.has(colCPUPct) || first.has(colHostCPUPct) {
		t.Errorf("the first sample has a CPU %% without a previous sample: %v", first.vals)
	}
	for col, want := range map[string]float64{
		colCPUTicks: 1500, colCPUPct: 25, colRSS: 52224 * 1024, colThreads: 13, colFDs: 31, colCPUs: 2,
		colHostCPUPct: 40, colLoad1: 0.6, colLoad5: 0.4, colLoad15: 0.3, colNICRx: 5000, colNICTx: 9000,
		colUDPIn: 15, colUDPOut: 30, colTCPRetrans: 4, colUDPRcvbufErrs: 0,
	} {
		if got, ok := second.vals[col]; !ok || got != want {
			t.Errorf("second sample %s = %v (present %t), want %v", col, got, ok, want)
		}
	}
	if second.pid != 4127 {
		t.Errorf("second sample pid %d", second.pid)
	}
	for i, want := range []string{"the service has no main process", "Failed to connect to bus", "pid 4127: the process ended"} {
		r := rows[3+i]
		if r.pid != 0 || r.has(colRSS) || !r.has(colNICRx) || !strings.Contains(r.note, want) {
			t.Errorf("down row %d = pid %d %v %q; want no process, the machine's columns, a note with %q", i, r.pid,
				r.vals, r.note, want)
		}
	}
	if r := rows[6]; r.pid != 4127 || !strings.Contains(r.note, "started later") {
		t.Errorf("restart row = pid %d %q, want the reused PID", r.pid, r.note)
	}
	if r := rows[7]; r.has(colCPUPct) || r.has(colFDs) || !strings.Contains(r.note, "fds:") || r.vals[colRSS] != 40960*1024 {
		t.Errorf("sample after the reuse = %v %q; want no CPU %%, no fds and a note why", r.vals, r.note)
	}
	if r := rows[8]; r.pid != 5000 || r.note != "pid 4127 -> 5000" {
		t.Errorf("restart row = pid %d %q, want 4127 -> 5000", r.pid, r.note)
	}
	if r := rows[10]; r.note != "stopped by a signal" || r.pid != 0 {
		t.Errorf("stop row = %+v", r)
	}
}

// TestCollectNIC: -nic names an interface or "all"; without a default route the collector counts all.
func TestCollectNIC(t *testing.T) {
	h := newFakeHost(t)
	h.setHost(hostState{cpus: 1, busy: 1, idle: 1, rx: 100, tx: 200, loRx: 7, loTx: 7, defaultRouteNone: true})
	h.setProcess(procState{pid: 9, ticks: 1, start: 1, rssKiB: 4, threads: 1, fds: 1})
	var pid atomic.Int64
	pid.Store(9)
	for _, tc := range []struct{ flag, note string }{{"", "nic=all"}, {"lo", "nic=lo"}, {"all", "nic=all"}} {
		path := filepath.Join(t.TempDir(), "run.csv")
		c, err := newCollector(collectOptions{out: path, procDir: h.dir, nic: tc.flag, interval: time.Second,
			scrapeInterval: time.Second, pid: fixedPID(&pid)}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if err := c.sample(context.Background(), time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := c.close(time.Now(), "done"); err != nil {
			t.Fatal(err)
		}
		rows := readRows(t, path)
		want := 100.0
		if tc.flag == "lo" {
			want = 7
		}
		if !strings.Contains(rows[0].note, tc.note) || rows[1].vals[colNICRx] != want {
			t.Errorf("-nic %q: note %q, rx %v; want %s and %v", tc.flag, rows[0].note, rows[1].vals[colNICRx], tc.note, want)
		}
	}
	// A named interface that isn't there: the sample notes it and goes on.
	path := filepath.Join(t.TempDir(), "run.csv")
	c, err := newCollector(collectOptions{out: path, procDir: h.dir, nic: "wlan9", interval: time.Second,
		scrapeInterval: time.Second, pid: fixedPID(&pid)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.sample(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := c.close(time.Now(), "done"); err != nil {
		t.Fatal(err)
	}
	if r := readRows(t, path)[1]; r.has(colNICRx) || !strings.Contains(r.note, "wlan9") || !r.has(colRSS) {
		t.Errorf("sample with a missing interface: %v %q", r.vals, r.note)
	}
}

// TestCollectAppends: a collector started again appends after a new start row, and marks the restart that
// happened while it was down.
func TestCollectAppends(t *testing.T) {
	h := newFakeHost(t)
	h.setProcess(procState{pid: 100, ticks: 1, start: 1, rssKiB: 4, threads: 1, fds: 1})
	var pid atomic.Int64
	pid.Store(100)
	path := filepath.Join(t.TempDir(), "run.csv")
	o := collectOptions{out: path, procDir: h.dir, interval: time.Second, scrapeInterval: time.Second, pid: fixedPID(&pid)}
	t0 := time.Date(2026, 10, 15, 18, 0, 0, 0, time.UTC)
	c, err := newCollector(o, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.sample(context.Background(), t0); err != nil {
		t.Fatal(err)
	}
	_ = c.closeFiles() // killed: no stop row
	// A row cut short by the kill.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0) //nolint:gosec // G304: the test's own file
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("2026-10-15T18:00:01.000Z,sam"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	h.setProcess(procState{pid: 200, ticks: 1, start: 2, rssKiB: 4, threads: 1, fds: 1})
	pid.Store(200)
	c, err = newCollector(o, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.sample(context.Background(), t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := c.close(t0.Add(2*time.Minute), "stopped by a signal"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: the test's own file
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "time,kind,pid,") != 1 {
		t.Errorf("the header is written again:\n%s", data)
	}
	rf, err := readRun(strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(rf.skipped) != 1 || !strings.Contains(rf.skipped[0], "line 4") {
		t.Errorf("skipped %v, want the row cut short on line 4", rf.skipped)
	}
	if got, want := kinds(rf.rows), "start sample start restart sample stop"; got != want {
		t.Errorf("kinds = %s, want %s", got, want)
	}
	if n := rf.rows[2].note; !strings.Contains(n, "appending last_pid=100") {
		t.Errorf("second start note %q", n)
	}
	if r := rf.rows[3]; r.note != "pid 100 -> 200" {
		t.Errorf("restart note %q", r.note)
	}

	// Another file is never appended to.
	other := filepath.Join(t.TempDir(), "notes.csv")
	if err := os.WriteFile(other, []byte("a,b\n1,2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o.out = other
	if _, err := newCollector(o, t0); err == nil || !strings.Contains(err.Error(), "not a run.csv") {
		t.Errorf("newCollector over another CSV: %v", err)
	}
	if data, _ := os.ReadFile(other); string(data) != "a,b\n1,2\n" { //nolint:gosec // G304: the test's own file
		t.Errorf("the other file changed: %q", data)
	}
}

// switchableMetrics is a metrics endpoint that a test can take away (connection refused) and bring back on the
// same address, and make answer 503.
type switchableMetrics struct {
	t       *testing.T
	addr    string
	mu      sync.Mutex
	srv     *http.Server
	done    chan struct{}
	failing atomic.Bool
	page    atomic.Value // string
}

func newSwitchableMetrics(t *testing.T) *switchableMetrics {
	s := &switchableMetrics{t: t, addr: "127.0.0.1:0"}
	s.page.Store(metricsPage(5, 2, 1, 1))
	s.start()
	t.Cleanup(s.stop)
	return s
}

func (s *switchableMetrics) url() string { return "http://" + s.addr + "/metrics" }

func (s *switchableMetrics) start() {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var l net.Listener
	var err error
	for range 50 { // the port was just closed; it is free again at once on Linux and macOS, but be patient
		if l, err = (&net.ListenConfig{}).Listen(context.Background(), "tcp", s.addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		s.t.Fatal(err)
	}
	s.addr = l.Addr().String()
	s.srv = &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if s.failing.Load() {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(s.page.Load().(string)))
	})}
	s.done = make(chan struct{})
	go func(srv *http.Server, done chan struct{}) {
		defer close(done)
		_ = srv.Serve(l)
	}(s.srv, s.done)
}

func (s *switchableMetrics) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv == nil {
		return
	}
	_ = s.srv.Close()
	<-s.done
	s.srv = nil
}

// rowWatcher follows the rows a running collector writes.
type rowWatcher struct {
	t    *testing.T
	ch   chan *runRow
	seen []*runRow
}

func newRowWatcher(t *testing.T) *rowWatcher {
	return &rowWatcher{t: t, ch: make(chan *runRow, 100_000)}
}

func (w *rowWatcher) onRow(r *runRow) {
	cp := *r
	w.ch <- &cp
}

// waitFor reads rows until n more rows since now match, or fails after 10 s.
func (w *rowWatcher) waitFor(what string, n int, match func(*runRow) bool) []*runRow {
	w.t.Helper()
	var got []*runRow
	timeout := time.After(10 * time.Second)
	for len(got) < n {
		select {
		case r := <-w.ch:
			w.seen = append(w.seen, r)
			if match(r) {
				got = append(got, r)
			}
		case <-timeout:
			w.t.Fatalf("waiting for %d× %s: got %d; rows so far: %s", n, what, len(got), kinds(w.seen))
		}
	}
	return got
}

func isKind(kind string, pid int) func(*runRow) bool {
	return func(r *runRow) bool { return r.kind == kind && (pid < 0 || r.pid == pid) }
}

// TestCollectSurvivesRestartAndOutage runs the collector's loop in real time with short intervals through what
// the exit test does to it: the server restarts (a gap with no process, then a new PID), and the metrics endpoint
// answers 503 and then goes away entirely for a while. The collector goes on sampling through all of it, and
// stops cleanly when its context ends.
func TestCollectSurvivesRestartAndOutage(t *testing.T) {
	h := newFakeHost(t)
	h.setProcess(procState{pid: 100, ticks: 10, start: 1, rssKiB: 51200, threads: 12, fds: 30})
	h.setProcess(procState{pid: 200, ticks: 5, start: 2, rssKiB: 40960, threads: 10, fds: 20})
	var pid atomic.Int64
	pid.Store(100)
	ms := newSwitchableMetrics(t)
	w := newRowWatcher(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "run.csv")
	c, err := newCollector(collectOptions{
		out: out, metricsURL: ms.url(), metricsDir: filepath.Join(dir, "run-metrics"), procDir: h.dir,
		interval: 20 * time.Millisecond, scrapeInterval: 30 * time.Millisecond, pid: fixedPID(&pid), onRow: w.onRow,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.run(ctx) }()

	w.waitFor("sample of 100", 3, isKind(kindSample, 100))
	w.waitFor("scrape of 100", 1, isKind(kindScrape, 100))
	// The restart: no main process for a moment, then another one.
	pid.Store(0)
	w.waitFor("down", 2, isKind(kindDown, -1))
	pid.Store(200)
	w.waitFor("restart to 200", 1, isKind(kindRestart, 200))
	w.waitFor("sample of 200", 2, isKind(kindSample, 200))
	// The endpoint fails, then is gone (connection refused); samples go on meanwhile.
	ms.failing.Store(true)
	w.waitFor("scrape_error (503)", 2, func(r *runRow) bool {
		return r.kind == kindScrapeError && strings.Contains(r.note, "503")
	})
	ms.stop()
	w.waitFor("scrape_error (refused)", 2, func(r *runRow) bool {
		return r.kind == kindScrapeError && !strings.Contains(r.note, "503")
	})
	w.waitFor("sample of 200 while the endpoint is gone", 2, isKind(kindSample, 200))
	ms.failing.Store(false)
	ms.start()
	w.waitFor("scrape of 200", 2, isKind(kindScrape, 200))
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after its context ended")
	}
	if err := c.close(time.Now(), "stopped by a signal"); err != nil {
		t.Fatal(err)
	}

	rows := readRows(t, out)
	if rows[0].kind != kindStart || rows[len(rows)-1].kind != kindStop {
		t.Fatalf("kinds = %s, want start ... stop", kinds(rows))
	}
	restarts := 0
	for i, r := range rows {
		switch r.kind {
		case kindRestart:
			restarts++
			if r.note != "pid 100 -> 200" {
				t.Errorf("restart note %q", r.note)
			}
		case kindSample:
			if r.pid != 100 && r.pid != 200 || !r.has(colRSS) || !r.has(colNICRx) {
				t.Errorf("row %d: sample %d %v", i, r.pid, r.vals)
			}
			if r.pid == 100 && restarts > 0 || r.pid == 200 && restarts == 0 {
				t.Errorf("row %d: sample of %d on the wrong side of the restart", i, r.pid)
			}
		case kindScrape:
			if len(r.vals) != len(metricColumns) || r.vals["participants"] != 5 {
				t.Errorf("row %d: scrape %v", i, r.vals)
			}
			if _, err := os.Stat(filepath.Join(dir, "run-metrics", r.note)); err != nil {
				t.Errorf("row %d: the scrape file: %v", i, err)
			}
		case kindScrapeError:
			if len(r.vals) != 0 || r.note == "" {
				t.Errorf("row %d: scrape_error %v %q", i, r.vals, r.note)
			}
		}
	}
	if restarts != 1 {
		t.Errorf("%d restart rows, want 1", restarts)
	}
	// Every saved scrape is complete: no temporary file is left behind.
	entries, err := os.ReadDir(filepath.Join(dir, "run-metrics"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".prom") {
			t.Errorf("left in the scrape directory: %s", e.Name())
		}
	}
}

// TestScrapeRows covers what a scrape writes for each kind of answer, and that a scrape cut short by the stop
// writes nothing.
func TestScrapeRows(t *testing.T) {
	h := newFakeHost(t)
	h.setProcess(procState{pid: 7, ticks: 1, start: 1, rssKiB: 4, threads: 1, fds: 1})
	var pid atomic.Int64
	pid.Store(7)
	var mode atomic.Value
	mode.Store("ok")
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load().(string) {
		case "ok":
			w.Header().Set("Content-Type", "text/plain; version=0.0.4")
			_, _ = w.Write([]byte(metricsPage(2, 1, 0, 1) + "a broken line\n"))
		case "unrelated":
			w.Header().Set("Content-Type", "text/plain; version=0.0.4")
			_, _ = w.Write([]byte("node_load1 0.5\n"))
		case "html":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html></html>"))
		case "hang":
			select {
			case <-block:
			case <-r.Context().Done():
			}
		}
	}))
	defer srv.Close()
	defer close(block)
	dir := t.TempDir()
	c, err := newCollector(collectOptions{
		out: filepath.Join(dir, "run.csv"), metricsURL: srv.URL + "/metrics", metricsDir: filepath.Join(dir, "m"),
		procDir: h.dir, interval: 200 * time.Millisecond, scrapeInterval: time.Second, pid: fixedPID(&pid),
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 15, 18, 0, 0, 0, time.UTC)
	ctx := context.Background()
	for i, m := range []string{"ok", "unrelated", "html", "hang"} {
		mode.Store(m)
		if err := c.scrape(ctx, t0.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	// A scrape that the stop cuts short.
	cctx, cancel := context.WithCancel(ctx)
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if err := c.scrape(cctx, t0.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := c.close(t0.Add(11*time.Second), "done"); err != nil {
		t.Fatal(err)
	}
	rows := readRows(t, filepath.Join(dir, "run.csv"))
	if got, want := kinds(rows), "start scrape scrape_error scrape_error scrape_error stop"; got != want {
		t.Fatalf("kinds = %s, want %s", got, want)
	}
	if r := rows[1]; r.note != "20261015T180000.000Z.prom; 1 lines skipped, the first: line 48: value \"broken\"" ||
		r.vals["participants"] != 2 || r.pid != 7 {
		t.Errorf("scrape row = pid %d %v %q", r.pid, r.vals, r.note)
	}
	for i, want := range []string{"holds none of the exit test's metrics", "is not the text format", "deadline exceeded"} {
		if r := rows[2+i]; !strings.Contains(r.note, want) {
			t.Errorf("scrape_error %d: %q, want %q", i, r.note, want)
		}
	}
	saved, err := os.ReadFile(filepath.Join(dir, "m", "20261015T180000.000Z.prom"))
	if err != nil || !strings.Contains(string(saved), "isshoni_participants 2") {
		t.Errorf("the saved scrape: %v", err)
	}
}
