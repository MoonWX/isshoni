package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// childArgsEnv makes the test binary run exittest itself (realMain, with its signal handling) instead of the
// tests: TestCollectStopsOnSIGTERM starts it so and sends it a real SIGTERM.
const childArgsEnv = "EXITTEST_TEST_CHILD_ARGS"

// TestMain fails the tests when a goroutine outlives them: the collector's loop, a scrape, a test server.
func TestMain(m *testing.M) {
	if args := os.Getenv(childArgsEnv); args != "" {
		var a []string
		if err := json.Unmarshal([]byte(args), &a); err != nil {
			panic(err)
		}
		os.Exit(realMain(a, os.Stdout, os.Stderr))
	}
	goleak.VerifyTestMain(m)
}

// TestStdlibOnly: the tool builds from the standard library alone (06 §12.3), so the binary copied to the VPS
// is one static file. The tests may use goleak.
func TestStdlibOnly(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if first, _, _ := strings.Cut(path, "/"); strings.Contains(first, ".") {
				t.Errorf("%s imports %s, which is not in the standard library", name, path)
			}
		}
	}
}

func TestUsage(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	for _, tc := range []struct {
		args []string
		code int
		out  string // in stdout or stderr
	}{
		{nil, 2, "usage: exittest collect"},
		{[]string{"help"}, 0, "usage: exittest collect"},
		{[]string{"frobnicate"}, 2, `unknown command "frobnicate"`},
		{[]string{"collect", "-h"}, 0, "-scrape-interval"},
		{[]string{"collect", "-bogus"}, 2, "flag provided but not defined"},
		{[]string{"collect", "extra"}, 2, `unexpected argument "extra"`},
		{[]string{"collect", "-o", ""}, 2, "-o is empty"},
		{[]string{"collect", "-interval", "0s"}, 2, "must be positive"},
		{[]string{"collect", "-unit", "isshoni", "-pid-file", pidFile}, 2, "exclude each other"},
		{[]string{"collect", "-unit", "--user"}, 2, "is not a systemd unit name"},
		{[]string{"collect", "-metrics", "127.0.0.1:9469/metrics"}, 2, "is not an http or https URL"},
		{[]string{"collect", "-proc", filepath.Join(t.TempDir(), "none"), "-pid-file", pidFile, "-o",
			filepath.Join(t.TempDir(), "run.csv")}, 1, "the proc file system"},
		{[]string{"summarize"}, 2, "name one run.csv"},
		{[]string{"summarize", "-h"}, 0, "-t0"},
		{[]string{"summarize", "a.csv", "b.csv"}, 2, "name one run.csv"},
		{[]string{"summarize", filepath.Join(t.TempDir(), "none.csv")}, 1, "no such file"},
		{[]string{"summarize", "-t0", "noon", filepath.Join("testdata", "run.csv")}, 2, "neither RFC 3339 nor HH:MM"},
		{[]string{"summarize", "main.go"}, 1, "not a run.csv"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), tc.args, &stdout, &stderr)
		if code != tc.code || !strings.Contains(stdout.String()+stderr.String(), tc.out) {
			t.Errorf("exittest %q = %d, stdout %q, stderr %q; want %d and %q", tc.args, code, stdout.String(),
				stderr.String(), tc.code, tc.out)
		}
	}
}

// TestCollectStopsOnSIGTERM runs `exittest collect` as a process of its own against a fake proc tree and a fake
// metrics endpoint, the way the owner runs it under systemd-run, and stops it with SIGTERM as `systemctl stop`
// does: it exits 0 and the file ends with a stop row.
func TestCollectStopsOnSIGTERM(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no SIGTERM to send")
	}
	h := newFakeHost(t)
	h.setProcess(procState{pid: 321, ticks: 10, start: 1, rssKiB: 51200, threads: 12, fds: 30})
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "isshoni.pid")
	if err := os.WriteFile(pidFile, []byte("321\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(metricsPage(3, 1, 1, 1)))
	}))
	defer srv.Close()
	out := filepath.Join(dir, "run.csv")
	args, err := json.Marshal([]string{"collect", "-o", out, "-metrics", srv.URL + "/metrics", "-proc", h.dir,
		"-pid-file", pidFile, "-interval", "50ms", "-scrape-interval", "100ms"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$") //nolint:gosec // G204: this test binary
	cmd.Env = append(os.Environ(), childArgsEnv+"="+string(args))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	// Wait for a few samples and scrapes, then a second PID, as a restart would bring.
	waitRows := func(what string, ok func([]*runRow) bool) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			data, _ := os.ReadFile(out) //nolint:gosec // G304: the test's own file
			if rf, err := readRun(bytes.NewReader(data)); err == nil && ok(rf.rows) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("no %s in run.csv after 15 s:\n%s\nstderr:\n%s", what, data, stderr.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	countKind := func(rows []*runRow, kind string) int {
		n := 0
		for _, r := range rows {
			if r.kind == kind {
				n++
			}
		}
		return n
	}
	waitRows("3 samples and a scrape", func(rows []*runRow) bool {
		return countKind(rows, kindSample) >= 3 && countKind(rows, kindScrape) >= 1
	})
	h.setProcess(procState{pid: 654, ticks: 1, start: 2, rssKiB: 40960, threads: 10, fds: 20})
	if err := os.WriteFile(pidFile, []byte("654\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitRows("restart", func(rows []*runRow) bool { return countKind(rows, kindRestart) == 1 })

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waitErr:
		var ee *exec.ExitError
		if errors.As(err, &ee) || err != nil {
			t.Fatalf("exittest collect after SIGTERM: %v\nstderr:\n%s", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("exittest collect still runs 10 s after SIGTERM\nstderr:\n%s", stderr.String())
	}
	rows := readRows(t, out)
	last := rows[len(rows)-1]
	if last.kind != kindStop || last.note != "stopped by a signal" {
		t.Errorf("last row = %s %q, want the stop row", last.kind, last.note)
	}
	if !strings.Contains(stderr.String(), "msg=stopped") {
		t.Errorf("stderr has no stop line:\n%s", stderr.String())
	}
}
