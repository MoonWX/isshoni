package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server"
	"github.com/MoonWX/isshoni/internal/server/ops"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/version"
)

// The tool against an in-process server (servertest, 04 §17): the smoke scenario exactly as CI's test-go runs it
// (02 §15.2, README S89), a short focus run that moves the focus, all-high's steps, an interrupted run, and the exit
// codes of a server that can't be reached, a refused login and a bad command line.

const (
	adminName     = "Alex"
	adminPassword = "correct horse battery"
	runTimeout    = 3 * time.Minute
)

// syncBuffer is a bytes.Buffer that the tool's goroutines may write to at once.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// testEnv is the process the tool sees: the admin's password in its environment, no terminal.
func testEnv(stdout, stderr io.Writer, password string) env {
	return env{stdout: stdout, stderr: stderr, getenv: func(name string) string {
		if name == passwordEnv {
			return password
		}
		return ""
	}}
}

// startServer starts an in-process server and creates its admin the way an operator does: a setup link from the
// admin socket, then the setup form.
func startServer(t *testing.T) *servertest.Server {
	t.Helper()
	srv := servertest.Start(t, servertest.Options{Deps: serverDeps()})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	link, err := ops.DialAdmin(srv.AdminSocket).SetupURL(ctx, 0)
	if err != nil {
		t.Fatalf("setup-url: %v", err)
	}
	token, ok := strings.CutPrefix(link.URL, srv.URL+"/setup#")
	if !ok {
		t.Fatalf("setup link %q, want %s/setup#<token>", link.URL, srv.URL)
	}
	//nolint:gosec // G117: the test server's admin, made up here
	body, _ := json.Marshal(api.SetupCompleteRequest{Token: token, Username: adminName, Password: adminPassword})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/api/v1/auth/setup/complete", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := srv.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("setup/complete = %d", res.StatusCode)
	}
	return srv
}

// serverDeps gives the test server a one-page web app built for this binary's version, as internal/server/itest
// does: without it the server serves whatever the working tree's web/dist holds, and warns about its version.
func serverDeps() (d server.Deps) {
	d.SPA = fstest.MapFS{
		"index.html":   &fstest.MapFile{Data: []byte("<!doctype html><title>isshoni</title><div id=root></div>\n")},
		"version.json": &fstest.MapFile{Data: []byte(`{"version":"` + version.Version() + `"}`)},
	}
	return d
}

// usersOf lists the server's accounts by name through the admin socket.
func usersOf(t *testing.T, srv *servertest.Server) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	users, err := ops.DialAdmin(srv.AdminSocket).Users(ctx)
	if err != nil {
		t.Fatalf("users: %v", err)
	}
	var names []string
	for _, u := range users.Users {
		names = append(names, u.Username)
	}
	return names
}

// readReport reads a JSON report.
func readReport(t *testing.T, path string) *report {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the JSON report: %v", err)
	}
	var rep report
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatalf("the JSON report: %v\n%s", err, b)
	}
	return &rep
}

// checkOf returns a report's check by name.
func checkOf(t *testing.T, rep *report, name string) checkReport {
	t.Helper()
	for _, c := range rep.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("the report has no check %q: %+v", name, rep.Checks)
	return checkReport{}
}

// TestSmoke runs `-scenario smoke` against an in-process server, as CI does: it exits 0 with a JSON report that
// passes, and the accounts it made from its invite are gone afterwards.
func TestSmoke(t *testing.T) {
	t.Parallel()
	srv := startServer(t)
	out := filepath.Join(t.TempDir(), "report.json")
	var stdout, stderr syncBuffer
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	// The sign-ups come from the test's own address, whose auth-ip bucket has room for them all: no pacing.
	code := run(ctx, []string{"-server", srv.URL, "-admin", adminName, "-scenario", "smoke", "-login-interval", "0s",
		"-json", out}, testEnv(&stdout, &stderr, adminPassword))
	if code != exitPass {
		t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitPass, stdout.String(), stderr.String())
	}
	t.Logf("summary:\n%s", stdout.String())
	rep := readReport(t, out)
	if !rep.Pass || len(rep.Failures) != 0 || rep.Scenario != "smoke" {
		t.Fatalf("report: pass %v, scenario %q, failures %q", rep.Pass, rep.Scenario, rep.Failures)
	}
	p := rep.Params
	if p.Publishers != 2 || p.Subscribers != 2 || p.Duration != "30s" || !p.Decodable || p.Profile != "42e0" {
		t.Errorf("params = %+v, want the smoke defaults: 2×2, 30s, decodable 42e0", p)
	}
	if rep.Users != (usersReport{Created: 4, Deleted: 4}) {
		t.Errorf("users = %+v, want 4 created and deleted", rep.Users)
	}
	if rep.Loss.ExpectedPackets == 0 || rep.Loss.Tracks < 4 || rep.AVOffsetMs.Count == 0 || rep.Server.Samples < 2 ||
		rep.Server.ExpectedEgressMbps <= 0 || rep.Server.ReceivedMbps <= 0 {
		t.Errorf("the run measured too little: loss %+v, a/v %+v, server %+v", rep.Loss, rep.AVOffsetMs, rep.Server)
	}
	for _, name := range []string{"loss", "egress", "pli rate", "pli storms", "a/v"} {
		if c := checkOf(t, rep, name); !c.Checked || !c.Pass {
			t.Errorf("check %s = %+v, want checked and passed", name, c)
		}
	}
	for _, name := range []string{"cpu avg", "memory"} {
		if c := checkOf(t, rep, name); c.Checked {
			t.Errorf("check %s = %+v: smoke only reports it", name, c)
		}
	}
	if names := usersOf(t, srv); len(names) != 1 || names[0] != adminName {
		t.Errorf("the server's accounts after the run: %q, want only %s", names, adminName)
	}
}

// TestFocusRotation runs a short focus scenario in which the subscribers move their focus every 3 s: every switch
// is measured and starts on a keyframe, and the run passes the focus criteria. CPU and memory are not checked
// against an in-process server, and the PLI and egress limits allow for the quick rotation.
func TestFocusRotation(t *testing.T) {
	t.Parallel()
	srv := startServer(t)
	var stderr syncBuffer
	cfg, err := parseFlags([]string{"-server", srv.URL, "-admin", adminName, "-scenario", "focus", "-publishers", "3",
		"-subscribers", "2", "-duration", "16s", "-warmup", "3s", "-focus-rotate", "3s", "-full", "640x360@30:500k",
		"-preview", "320x180@15:150k", "-gop", "10s", "-login-interval", "0s"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	cfg.password = logx.Secret(adminPassword)
	cfg.dashboardEvery = 2 * time.Second
	cfg.scenario.limits.cpuAvgPct, cfg.scenario.limits.cpuSamplePct, cfg.scenario.limits.rssPerDownTrack = 0, 0, 0
	cfg.scenario.limits.pliPerSecond, cfg.scenario.limits.pliPer10s = 1, 20
	// Every move asks for two keyframes, which one viewer receives whole while the expected egress spreads them over
	// the run: with a move every 3 s the subscribers receive about 7 % more than expected (about 0.5 % at 30 s).
	cfg.scenario.limits.egressLow, cfg.scenario.limits.egressHigh = 0.75, 1.25
	log := logx.New(logx.Options{Out: &stderr, Format: "text"})
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	rep, err := newLoadTest(cfg, log).run(ctx)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, stderr.String())
	}
	var summary bytes.Buffer
	writeSummary(&summary, rep)
	t.Logf("summary:\n%s", summary.String())
	if !rep.Pass {
		t.Fatalf("failures: %q\n%s", rep.Failures, stderr.String())
	}
	// 13 s measured, a move every 3 s per subscriber, two switches per move.
	if rep.SwitchMs.Count < 8 || rep.SwitchMs.WithoutKeyframe != 0 {
		t.Errorf("switches = %+v, want at least 8, each on a keyframe", rep.SwitchMs)
	}
	if c := checkOf(t, rep, "switch latency"); !c.Checked || !c.Pass {
		t.Errorf("check switch latency = %+v", c)
	}
	if rep.Users.Created != 5 || rep.Users.Deleted != 5 {
		t.Errorf("users = %+v, want 5 created and deleted", rep.Users)
	}
}

// TestAllHighSteps runs all-high with short steps of one thumbnail each: it records a step with 2, 3 and 4 full
// layers (each of the 2 subscribers starts with its focus and has one thumbnail), and ends when nothing is left to
// raise, long before -duration.
func TestAllHighSteps(t *testing.T) {
	t.Parallel()
	srv := startServer(t)
	var stderr syncBuffer
	cfg, err := parseFlags([]string{"-server", srv.URL, "-admin", adminName, "-scenario", "all-high", "-publishers", "3",
		"-subscribers", "2", "-duration", "2m", "-warmup", "2s", "-full", "640x360@30:500k", "-preview", "320x180@15:150k",
		"-login-interval", "0s"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	cfg.password = logx.Secret(adminPassword)
	cfg.dashboardEvery, cfg.stepEvery, cfg.stepSize = time.Second, 3*time.Second, 1
	log := logx.New(logx.Options{Out: &stderr, Format: "text"})
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	rep, err := newLoadTest(cfg, log).run(ctx)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, stderr.String())
	}
	var summary bytes.Buffer
	writeSummary(&summary, rep)
	t.Logf("summary:\n%s", summary.String())
	if !rep.Pass || rep.Knee != nil {
		t.Fatalf("pass %v, knee %+v, failures %q\n%s", rep.Pass, rep.Knee, rep.Failures, stderr.String())
	}
	var high []int
	for _, s := range rep.Steps {
		high = append(high, s.High)
		if s.EgressMbps <= 0 || s.ExpectedEgressMbps <= 0 || s.Seconds < 2 {
			t.Errorf("step %+v measured nothing", s)
		}
	}
	if len(high) != 3 || high[0] != 2 || high[1] != 3 || high[2] != 4 {
		t.Errorf("the steps' full layers %v, want [2 3 4]", high)
	}
	if rep.MeasuredSeconds > 30 {
		t.Errorf("measured %v s: all-high did not stop when nothing was left to raise", rep.MeasuredSeconds)
	}
	for _, c := range rep.Checks {
		if c.Checked {
			t.Errorf("check %+v: all-high only reports", c)
		}
	}
}

// TestInterrupted: SIGINT (the context's end) in the middle of a run exits 130, and the teardown deletes the
// accounts all the same.
func TestInterrupted(t *testing.T) {
	t.Parallel()
	srv := startServer(t)
	out := filepath.Join(t.TempDir(), "report.json")
	var stdout, stderr syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	interrupted := make(chan struct{})
	go func() {
		// A second into the warm-up (5 s), once every client is up.
		defer close(interrupted)
		deadline := time.Now().Add(runTimeout)
		for !strings.Contains(stderr.String(), `"msg":"running"`) && ctx.Err() == nil && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		_ = sleep(ctx, time.Second)
		cancel()
	}()
	code := run(ctx, []string{"-server", srv.URL, "-admin", adminName, "-scenario", "smoke", "-login-interval", "0s",
		"-json", out}, testEnv(&stdout, &stderr, adminPassword))
	cancel()
	<-interrupted
	if code != exitInterrupted {
		t.Fatalf("exit %d, want %d\n%s", code, exitInterrupted, stderr.String())
	}
	rep := readReport(t, out)
	if rep.Pass || rep.Users != (usersReport{Created: 4, Deleted: 4}) || !slices.Contains(rep.Failures, "interrupted") {
		t.Errorf("report: pass %v, users %+v, failures %q", rep.Pass, rep.Users, rep.Failures)
	}
	if names := usersOf(t, srv); len(names) != 1 || names[0] != adminName {
		t.Errorf("the server's accounts after the run: %q, want only %s", names, adminName)
	}
}

// TestUnreachableServer: a server that doesn't answer is a setup error, exit 2, and nothing is left to tear down.
func TestUnreachableServer(t *testing.T) {
	t.Parallel()
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close() // nothing listens there now
	out := filepath.Join(t.TempDir(), "report.json")
	var stdout, stderr syncBuffer
	code := run(context.Background(), []string{"-server", "http://127.0.0.1:" + strconv.Itoa(port), "-admin", adminName,
		"-scenario", "smoke", "-json", out}, testEnv(&stdout, &stderr, adminPassword))
	if code != exitSetup {
		t.Fatalf("exit %d, want %d (setup error)\n%s", code, exitSetup, stderr.String())
	}
	if !strings.Contains(stderr.String(), "cannot reach the server") {
		t.Errorf("stderr does not say why:\n%s", stderr.String())
	}
	rep := readReport(t, out)
	if rep.Pass || rep.SetupError == "" || rep.Users.Created != 0 {
		t.Errorf("report: pass %v, setupError %q, users %+v", rep.Pass, rep.SetupError, rep.Users)
	}
}

// TestWrongPassword: a login the server refuses is a setup error, exit 2.
func TestWrongPassword(t *testing.T) {
	t.Parallel()
	srv := startServer(t)
	var stdout, stderr syncBuffer
	code := run(context.Background(), []string{"-server", srv.URL, "-admin", adminName, "-scenario", "smoke"},
		testEnv(&stdout, &stderr, "not the password at all"))
	if code != exitSetup || !strings.Contains(stderr.String(), "invalid_credentials") {
		t.Fatalf("exit %d, want %d with invalid_credentials\n%s", code, exitSetup, stderr.String())
	}
}

// TestUsage: usage errors exit 2 without touching the network, and -h exits 0.
func TestUsage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no server", []string{"-admin", "a"}, "-server is required"},
		{"no admin", []string{"-server", "http://127.0.0.1:1"}, "-admin is required"},
		{"bad server", []string{"-server", "ftp://x", "-admin", "a"}, "want http(s)://host[:port]"},
		{"server with a path", []string{"-server", "https://x/isshoni", "-admin", "a"}, "want http(s)://host[:port]"},
		{"bad scenario", []string{"-server", "http://x", "-admin", "a", "-scenario", "nope"}, "want focus, all-high or smoke"},
		{"no publishers", []string{"-server", "http://x", "-admin", "a", "-publishers", "0"}, "-publishers 0"},
		{"too many accounts", []string{"-server", "http://x", "-admin", "a", "-publishers", "10", "-subscribers", "991"}, "at most 1000"},
		{"warm-up too long", []string{"-server", "http://x", "-admin", "a", "-duration", "10s", "-warmup", "10s"}, "-warmup 10s"},
		{"bad layer", []string{"-server", "http://x", "-admin", "a", "-full", "1920x1080"}, "-full"},
		{"no bitrate", []string{"-server", "http://x", "-admin", "a", "-preview", "640x360@15"}, "needs a bitrate"},
		{"decodable high", []string{"-server", "http://x", "-admin", "a", "-decodable", "-profile", "6400"}, "always 42e0"},
		{"all-high without subscribers", []string{"-server", "http://x", "-admin", "a", "-scenario", "all-high", "-subscribers", "0"}, "needs subscribers"},
		{"unknown flag", []string{"-nope"}, "flag provided but not defined"},
		{"argument", []string{"-server", "http://x", "-admin", "a", "extra"}, "unexpected argument"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr syncBuffer
			if code := run(context.Background(), tc.args, testEnv(&stdout, &stderr, "pw")); code != exitSetup {
				t.Errorf("exit %d, want %d", code, exitSetup)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr %q does not contain %q", stderr.String(), tc.want)
			}
		})
	}
	var stdout, stderr syncBuffer
	if code := run(context.Background(), []string{"-h"}, testEnv(&stdout, &stderr, "")); code != exitPass {
		t.Errorf("-h: exit %d, want %d", code, exitPass)
	}
	if !strings.Contains(stderr.String(), "Exit codes:") {
		t.Errorf("-h prints no usage:\n%s", stderr.String())
	}
	// No password, and no terminal to ask on.
	stderr = syncBuffer{}
	if code := run(context.Background(), []string{"-server", "http://127.0.0.1:1", "-admin", "a"}, testEnv(&stdout, &stderr, "")); code != exitSetup ||
		!strings.Contains(stderr.String(), passwordEnv) {
		t.Errorf("without a password: exit %d, stderr %q", code, stderr.String())
	}
}

// TestFlagDefaults: a scenario's defaults, and explicit flags over them.
func TestFlagDefaults(t *testing.T) {
	t.Parallel()
	parse := func(args ...string) *config {
		t.Helper()
		cfg, err := parseFlags(append([]string{"-server", "https://watch.example.com/", "-admin", "admin"}, args...), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	focus := parse()
	if focus.scenario.name != "focus" || focus.publishers != 10 || focus.subscribers != 10 || focus.duration != 10*time.Minute ||
		focus.warmup != time.Minute || focus.decodable || focus.profile != "6400" || focus.server.String() != "https://watch.example.com" {
		t.Errorf("focus defaults: %+v", focus)
	}
	if got := formatLayer(focus.layers[0]) + " " + formatLayer(focus.layers[1]); got != "1920x1080@60:8M 640x360@15:300k" {
		t.Errorf("focus layers %s", got)
	}
	smoke := parse("-scenario", "smoke", "-publishers", "1", "-subscribers", "0")
	if smoke.publishers != 1 || smoke.subscribers != 0 || smoke.duration != 30*time.Second || !smoke.decodable ||
		smoke.profile != "42e0" || smoke.layers[0].Width != 640 || smoke.layers[1].Width != 320 {
		t.Errorf("smoke with -publishers 1 -subscribers 0: %+v", smoke)
	}
	nl := newLoadTest(smoke, slog.New(slog.DiscardHandler))
	if nl.wsURL != "wss://watch.example.com/ws" || nl.loopback {
		t.Errorf("ws URL %q, loopback %v", nl.wsURL, nl.loopback)
	}
	nl.stopMedia()
	nl.transport.CloseIdleConnections()
	explicit := parse("-scenario", "smoke", "-decodable=false", "-full", "1280x720@30:2.5M", "-duration", "1m")
	if explicit.decodable || explicit.profile != "6400" || explicit.layers[0].Bitrate != 2_500_000 || explicit.duration != time.Minute {
		t.Errorf("smoke with explicit flags: %+v", explicit)
	}
}
