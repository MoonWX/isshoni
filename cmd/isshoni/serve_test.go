package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/server"
	"github.com/MoonWX/isshoni/internal/server/config"
)

// serveProc is `isshoni serve` running as its own process: this test binary under the name isshoni, which
// testscript.Main put on PATH (TestMain). A real process takes real signals, and its umask and memory limit are
// its own.
type serveProc struct {
	cmd    *exec.Cmd
	listen string // the bound listen.http address, from the "ready" log line
	exited chan struct{}
	err    error // of cmd.Wait; set before exited is closed

	mu     sync.Mutex
	stderr bytes.Buffer
	ready  chan string // receives listen at every start of the server: the first, and each restart in place
}

// startServe starts `isshoni serve` in off mode on an ephemeral loopback port with a fresh data directory, plus
// args, which win. The media sockets get ephemeral loopback ports too and no STUN server is asked: serve binds
// listen.ice_udp and listen.ice_tcp and detects its public address in every mode, and a test must neither hold the
// default port 7882 nor send packets to the public STUN servers. It does not wait for the server to be ready.
func startServe(t *testing.T, args ...string) *serveProc {
	t.Helper()
	bin, err := exec.LookPath("isshoni")
	if err != nil {
		t.Fatalf("isshoni is not on PATH (testscript.Main puts it there): %v", err)
	}
	empty := filepath.Join(t.TempDir(), "empty.toml")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	all := append([]string{
		"serve", "--tls.mode", "off", "--listen.http", "127.0.0.1:0", "--log.format", "json",
		"--listen.ice-udp", "127.0.0.1:0", "--listen.ice-tcp", "127.0.0.1:0", "--network.include-loopback", "--network.stun-servers=",
		"--data-dir", filepath.Join(t.TempDir(), "data"), "--listen.admin-socket", socketPath(t),
	}, args...)
	p := &serveProc{cmd: exec.CommandContext(t.Context(), bin, all...), exited: make(chan struct{}), ready: make(chan string, 16)}
	p.cmd.Env = append(os.Environ(),
		"ISSHONI_CONFIG="+empty,
		"ISSHONI_ALLOW_EPHEMERAL_DATA=1", // the tests may run in a container, where a temporary data directory is refused
		"GORACE="+strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"),
	)
	stderr, err := p.cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(p.exited)
		p.follow(stderr)
		p.err = p.cmd.Wait()
	}()
	t.Cleanup(func() {
		_ = p.cmd.Process.Kill()
		<-p.exited
	})
	return p
}

// follow copies the server's log and looks for the lines that say where it listens:
// {"level":"INFO","msg":"isshoni … ready: http://localhost:PORT (tls=off)","listen":"127.0.0.1:PORT"}. A server that
// restarts in place writes one at every start, into the same pipe: the process is the same.
func (p *serveProc) follow(stderr io.Reader) {
	sc := bufio.NewScanner(stderr)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		p.mu.Lock()
		p.stderr.Write(line)
		p.stderr.WriteByte('\n')
		p.mu.Unlock()
		var rec struct{ Msg, Listen string }
		if json.Unmarshal(line, &rec) == nil && rec.Listen != "" && strings.Contains(rec.Msg, " ready: ") {
			select {
			case p.ready <- rec.Listen:
			default: // more restarts than any test makes
			}
		}
	}
}

func (p *serveProc) logs() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stderr.String()
}

// waitReady waits for the next "ready" log line and returns the base URL of the server: the first call for the
// server's start, a later one for its next restart in place, which binds a new ephemeral port.
func (p *serveProc) waitReady(t *testing.T) string {
	t.Helper()
	select {
	case p.listen = <-p.ready:
		return "http://" + p.listen
	case <-p.exited:
		t.Fatalf("isshoni serve exited before it was ready: %v\n%s", p.err, p.logs())
	case <-time.After(30 * time.Second):
		t.Fatalf("isshoni serve was not ready within 30 s:\n%s", p.logs())
	}
	return ""
}

// wait waits for the process to exit and returns its exit code.
func (p *serveProc) wait(t *testing.T) int {
	t.Helper()
	select {
	case <-p.exited:
	case <-time.After(30 * time.Second):
		t.Fatalf("isshoni serve did not exit within 30 s:\n%s", p.logs())
	}
	var ee *exec.ExitError
	switch {
	case p.err == nil:
		return 0
	case errors.As(p.err, &ee):
		return ee.ExitCode() // -1 when a signal killed it
	default:
		t.Fatalf("isshoni serve: %v", p.err)
		return -1
	}
}

func httpGet(t *testing.T, url string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body)
}

// `isshoni serve` runs the server (04 §6): in off mode it answers /healthz with 200, and SIGTERM (systemd, Docker)
// or SIGINT (Ctrl-C) stops it gracefully with exit 0.
func TestServe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs signals")
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			t.Parallel()
			p := startServe(t)
			base := p.waitReady(t)
			if code, body := httpGet(t, base+"/healthz"); code != 200 || body != `{"status":"ok"}`+"\n" {
				t.Errorf("GET /healthz = %d %q, want 200 ok", code, body)
			}
			if code, body := httpGet(t, base+"/readyz"); code != 200 || !strings.HasPrefix(body, `{"status":"ready"`) {
				t.Errorf("GET /readyz = %d %q, want 200 ready", code, body)
			}
			if err := p.cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			if code := p.wait(t); code != exitOK {
				t.Errorf("exit %d after %v, want 0\n%s", code, sig, p.logs())
			}
			logs := p.logs()
			if !strings.Contains(logs, `"msg":"shutting down"`) || !strings.Contains(logs, `"msg":"shutdown complete"`) {
				t.Errorf("the log does not show a graceful shutdown:\n%s", logs)
			}
			// The logger is the one of 04 §10: JSON lines here, at the configured level.
			if strings.Contains(logs, `"level":"DEBUG"`) {
				t.Errorf("debug lines at log.level = info:\n%s", logs)
			}
			d := net.Dialer{Timeout: time.Second}
			if c, err := d.DialContext(t.Context(), "tcp", p.listen); err == nil {
				_ = c.Close()
				t.Errorf("%s still accepts connections after the exit", p.listen)
			}
		})
	}
}

// log.level and log.format reach the logger, and the config's warnings are logged, not printed.
func TestServeLogger(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs signals")
	}
	t.Parallel()
	p := startServe(t, "--log.level", "debug", "--log.format", "text", "--metrics.enabled")
	// The text format has no "listen" JSON field to find: wait for the line itself.
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(p.logs(), " ready: http://localhost:") {
		select {
		case <-p.exited:
			t.Fatalf("isshoni serve exited: %v\n%s", p.err, p.logs())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no ready line within 30 s:\n%s", p.logs())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := p.wait(t); code != exitOK {
		t.Errorf("exit %d, want 0\n%s", code, p.logs())
	}
	logs := p.logs()
	if !strings.Contains(logs, "level=INFO msg=\"isshoni ") || strings.Contains(logs, `{"time"`) {
		t.Errorf("log.format = text gave:\n%s", logs)
	}
	if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "metrics.enabled") {
		t.Errorf("the warning about metrics.enabled is not in the log:\n%s", logs)
	}
}

// What a restart can help with exits 1: a second server on the same data directory (the lock), and a busy port.
func TestServeRuntimeErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs signals")
	}
	t.Parallel()
	dataDir := filepath.Join(t.TempDir(), "data")
	first := startServe(t, "--data-dir", dataDir)
	first.waitReady(t)

	second := startServe(t, "--data-dir", dataDir)
	if code := second.wait(t); code != exitRuntime {
		t.Errorf("a second server on the same data directory: exit %d, want 1", code)
	}
	if logs := second.logs(); !strings.Contains(logs, "isshoni serve: another isshoni process is using "+dataDir) {
		t.Errorf("the second server said:\n%s", logs)
	}

	busy := startServe(t, "--listen.http", first.listen)
	if code := busy.wait(t); code != exitRuntime {
		t.Errorf("a server on a busy port: exit %d, want 1", code)
	}
	_, port, _ := net.SplitHostPort(first.listen)
	if logs := busy.logs(); !strings.Contains(logs, "isshoni serve: port "+port+" is in use") {
		t.Errorf("the server on the busy port said:\n%s", logs)
	}

	// The first one is unharmed and stops cleanly.
	if code, _ := httpGet(t, "http://"+first.listen+"/healthz"); code != 200 {
		t.Errorf("the first server answers %d after the failed starts", code)
	}
	if err := first.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := first.wait(t); code != exitOK {
		t.Errorf("the first server: exit %d, want 0\n%s", code, first.logs())
	}
}

// The exit mapping of 04 §6: nil and a forced stop → 0, a refusal → 78, a requested restart → 75 in an invocation
// that can't re-exec (TestRestartInPlace has the one that can), anything else → 1.
func TestServeExit(t *testing.T) {
	refusal := &config.OperatorError{
		Reason: config.ReasonSecretsOwner, Path: "/var/lib/isshoni/secrets.json",
		Message: "/var/lib/isshoni/secrets.json is owned by uid 0, but isshoni runs as uid 998",
		Fix:     "run: sudo chown isshoni:isshoni /var/lib/isshoni/secrets.json",
	}
	invalid := &config.ValidationError{Problems: []config.Problem{{
		Key: "shutdown_timeout", Value: `"0s"`, Severity: config.SeverityError, Message: "is too short", Fix: "use 10s",
	}}}
	tests := []struct {
		name       string
		err        error
		wantCode   int
		wantStderr string // what report prints for the mapped error, after what serveExit printed itself
	}{
		{"stopped", nil, exitOK, ""},
		{"stopped by force", fmt.Errorf("%w: http: requests still running were cut off: context deadline exceeded", server.ErrShutdownForced), exitOK, ""},
		{"refusal", refusal, exitConfig, "isshoni serve: /var/lib/isshoni/secrets.json is owned by uid 0, but isshoni runs as uid 998.\n" +
			"  fix: run: sudo chown isshoni:isshoni /var/lib/isshoni/secrets.json\n"},
		{"refusal, wrapped", fmt.Errorf("server: %w", refusal), exitConfig, "isshoni serve: server: /var/lib/isshoni/secrets.json is owned by uid 0, but isshoni runs as uid 998.\n" +
			"  fix: run: sudo chown isshoni:isshoni /var/lib/isshoni/secrets.json\n"},
		{"invalid config", invalid, exitConfig, "config error: shutdown_timeout = \"0s\" (default) is too short.\n  fix: use 10s.\n"},
		{"restart requested", server.ErrRestartRequested, exitRestart, "isshoni serve: the server stopped for a restart (after a restore or rotate-secrets, " +
			"or because it found the public address it was waiting for): start it again\n"},
		{"data directory locked", fmt.Errorf("another isshoni process is using /var/lib/isshoni: %w", config.ErrDataDirLocked), exitRuntime,
			"isshoni serve: another isshoni process is using /var/lib/isshoni: config: the data directory is locked by another process\n"},
		{"busy port", errors.New("port 443 is in use"), exitRuntime, "isshoni serve: port 443 is in use\n"},
	}
	cmd := root().sub("serve")
	for _, tt := range tests {
		var stderr bytes.Buffer
		inv := &invocation{stderr: &stderr}
		// After SIGINT the context is done; a failure of the server must still exit with its own code, not 130.
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		// serve's own context is the one that a stop signal ends; here none came.
		err := serveExit(t.Context(), inv, slog.New(slog.DiscardHandler), tt.err)
		if got := exitCodeOf(ctx, err); got != tt.wantCode {
			t.Errorf("%s: exit %d, want %d", tt.name, got, tt.wantCode)
		}
		if err != nil {
			report(inv, cmd, err)
		}
		if stderr.String() != tt.wantStderr {
			t.Errorf("%s: stderr %q\n%swant %q", tt.name, stderr.String(), strings.Repeat(" ", len(tt.name)+3), tt.wantStderr)
		}
	}
	if !server.NeedsOperator(refusal) || server.NeedsOperator(server.ErrRestartRequested) {
		t.Error("NeedsOperator disagrees with the test's cases")
	}
}

// What serve does once the server has shut down for a restart (04 §6.5): it re-execs, whatever the restart was
// for (Run's result is the same after a restore, after rotate-secrets and when the server found the public
// address it was waiting for). Only when there is no new process does it exit: 0 after a stop, 75 otherwise.
func TestRestartInPlace(t *testing.T) {
	const startAgain = "isshoni serve: the server stopped for a restart (after a restore or rotate-secrets, or because it found the " +
		"public address it was waiting for)"
	stopped, stop := context.WithCancel(t.Context())
	stop()
	tests := []struct {
		name       string
		ctx        context.Context
		reexec     func() error // nil: the invocation has none
		wantCalls  int
		wantCode   int
		wantStderr string
		wantLog    string
	}{
		// In the real program the re-exec does not come back; here it "succeeds" by returning what a failed exec
		// would, so that the test sees that it was called, once.
		{"re-exec", t.Context(), func() error { return errors.New("exec format error") }, 1, exitRestart,
			startAgain + " and could not start itself again (exec format error): start it again\n", "restarting in place"},
		{"a platform without re-exec", t.Context(), func() error { return server.ErrNoReexec }, 1, exitRestart, startAgain + ": start it again\n", "restarting in place"},
		{"an invocation that can't", t.Context(), nil, 0, exitRestart, startAgain + ": start it again\n", ""},
		{"a stop came during the shutdown", stopped, func() error { return errors.New("must not be called") }, 0, exitOK, "",
			"stopped instead of restarting: a stop signal came during the shutdown"},
	}
	cmd := root().sub("serve")
	for _, tt := range tests {
		var stderr, logs bytes.Buffer
		calls := 0
		inv := &invocation{stderr: &stderr}
		if tt.reexec != nil {
			inv.reexec = func() error { calls++; return tt.reexec() }
		}
		err := serveExit(tt.ctx, inv, slog.New(slog.NewTextHandler(&logs, nil)), server.ErrRestartRequested)
		// A restart the server asked for is no interrupt: after a stop it is a clean exit, 0 and not 130.
		if got := exitCodeOf(tt.ctx, err); got != tt.wantCode || calls != tt.wantCalls {
			t.Errorf("%s: exit %d after %d re-execs, want %d after %d", tt.name, got, calls, tt.wantCode, tt.wantCalls)
		}
		if err != nil {
			report(inv, cmd, err)
		}
		if stderr.String() != tt.wantStderr {
			t.Errorf("%s: stderr %q\nwant %q", tt.name, stderr.String(), tt.wantStderr)
		}
		if got := logs.String(); (tt.wantLog == "") != (got == "") || !strings.Contains(got, tt.wantLog) {
			t.Errorf("%s: log %q, want %q in it", tt.name, got, tt.wantLog)
		}
	}
}

// logSignals says its line once per signal and stops when told to.
func TestLogSignals(t *testing.T) {
	sigs := make(chan os.Signal, 1)
	said := make(chan struct{}, 8)
	stop := logSignals(sigs, func() { said <- struct{}{} })
	for range 3 {
		sigs <- syscall.SIGHUP
		select {
		case <-said:
		case <-time.After(5 * time.Second):
			t.Fatal("a signal was not answered")
		}
	}
	stop()
	sigs <- syscall.SIGHUP // nobody is watching any more
	select {
	case <-said:
		t.Error("a signal after stop was answered")
	case <-time.After(20 * time.Millisecond):
	}
}

// The first signal ends the context; the second one calls for the immediate exit; stop ends the watching.
func TestStopOnSignal(t *testing.T) {
	t.Run("two signals", func(t *testing.T) {
		sigs := make(chan os.Signal, 2)
		again := make(chan struct{})
		ctx, stop := stopOnSignal(t.Context(), sigs, func() { close(again) })
		defer stop()
		select {
		case <-ctx.Done():
			t.Fatal("the context is done before any signal")
		case <-again:
			t.Fatal("the second-signal call came before any signal")
		case <-time.After(20 * time.Millisecond):
		}
		sigs <- syscall.SIGTERM
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("the first signal did not end the context")
		}
		select {
		case <-again:
			t.Fatal("the first signal counted as the second")
		case <-time.After(20 * time.Millisecond):
		}
		sigs <- os.Interrupt
		select {
		case <-again:
		case <-time.After(5 * time.Second):
			t.Fatal("the second signal did not call for the exit")
		}
	})
	t.Run("SIGINT through the parent context too", func(t *testing.T) {
		// main's context ends with SIGINT, and the same signal is in sigs: one Ctrl-C, not two.
		parent, cancel := context.WithCancel(t.Context())
		sigs := make(chan os.Signal, 2)
		again := make(chan struct{})
		ctx, stop := stopOnSignal(parent, sigs, func() { close(again) })
		defer stop()
		cancel()
		sigs <- os.Interrupt
		<-ctx.Done()
		select {
		case <-again:
			t.Fatal("one SIGINT counted as two")
		case <-time.After(50 * time.Millisecond):
		}
	})
	t.Run("stop without a signal", func(t *testing.T) {
		sigs := make(chan os.Signal, 2)
		ctx, stop := stopOnSignal(t.Context(), sigs, func() { t.Error("second-signal call without signals") })
		stop()
		<-ctx.Done()
		sigs <- syscall.SIGTERM // nobody is watching any more
	})
	t.Run("stop after one signal", func(t *testing.T) {
		sigs := make(chan os.Signal, 2)
		ctx, stop := stopOnSignal(t.Context(), sigs, func() { t.Error("second-signal call after one signal") })
		sigs <- syscall.SIGTERM
		<-ctx.Done()
		stop()
	})
}
