// Package servertest starts a real isshoni server inside a test (docs/m1/04-server-platform.md §17, §18): the same
// server.New, Start and Run that `isshoni serve` uses, on ephemeral loopback ports with a temporary data directory.
// It is the one harness for every integration test that needs "a server": 01's, 02's, 03's and 04's.
//
//	srv := servertest.Start(t, servertest.Options{})
//	res, err := srv.Client.Get(srv.URL + "/readyz")
//
// The server runs in tls.mode = "off" on 127.0.0.1 (a dev site: http://localhost:<port>). Start returns when
// /readyz answers 200, which takes well under a second, and the test's cleanup stops the server gracefully and
// fails the test if Run returns an error. The server logs at debug level into a buffer (Logs), which a failed test
// prints.
//
// The harness runs the server in the test's process, so it sets server.Deps.InProcess: the server leaves the
// umask and the Go memory limit alone. It also turns off the container data-volume check, so the tests pass inside
// a container.
package servertest

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/server"
	"github.com/MoonWX/isshoni/internal/server/config"
)

// Options configures Start. The zero value is a plain off-mode server.
type Options struct {
	// TLS serves HTTPS and WSS through the 443 multiplexer with a private test CA in manual mode, and makes Client
	// trust that CA (04 §8.6, §17). It comes with the TLS manager (README S44); until then Start fails the test.
	TLS bool
	// Flags are extra config flags in the form `isshoni serve` takes them ("--registration.mode=closed"), applied
	// after the harness's own, so they win. A key set this way counts as set by the operator (config.IsSet), which
	// pins a policy key (04 §4.6); a change made in Config does not.
	Flags []string
	// Config adjusts the loaded config before the server is built, e.g. c.ShutdownTimeout or c.Listen.ICEUDP = "".
	// The server validates the result.
	Config func(*config.Config)
	// Deps are the server's test seams (fake STUN, a test SPA, stub handlers, …). The harness sets InProcess, and
	// a zero Host becomes the process's environment with the container data-volume check off.
	Deps server.Deps
	// DataDir is the data directory. "" means a fresh one under t.TempDir(). A test passes its own to seed files
	// before the start (a secrets.json, a database), or to run a second server on a directory in use.
	DataDir string
}

// Server is a running in-process server. Its fields are set by Start and stay the same across Restart, except Cfg
// and Srv, which Restart replaces. Stop, Restart and Wait are for the test's own goroutine.
type Server struct {
	// URL is where the test sends its requests: "http://" plus the site's host. For the default dev site that is
	// the site's origin, http://localhost:<port>, so it also serves as the Origin header of a browser. With a
	// public_url in the config (a reverse-proxy install) it is http://<public host>: the test plays the proxy, and
	// adds X-Forwarded-Proto and X-Forwarded-For itself.
	URL string
	// WSURL is the WebSocket endpoint: URL with the ws scheme, plus /ws.
	WSURL string
	// Client sends every request to this server's listener, whatever host the URL names (so a test may also probe
	// the Host check with another name). It keeps cookies like a browser and asks for no compression on its own.
	// With Options.TLS it trusts the test CA.
	Client *http.Client
	// AdminSocket is listen.admin_socket: a short path, because a unix socket path is limited to 104 bytes on
	// macOS. Nothing listens there until the admin socket exists (README S43).
	AdminSocket string
	// UDPPort and TCPPort are the bound ICE ports (listen.ice_udp, listen.ice_tcp). 0 until the SFU runs in the
	// server (README S59).
	UDPPort, TCPPort int
	// DataDir is the server's data directory.
	DataDir string
	// Cfg is the config the running server was built from, before the server resolved its ephemeral ports.
	Cfg *config.Config
	// Srv is the running server.
	Srv *server.Server

	opts Options
	logs *logBuffer

	mu       sync.Mutex
	run      *run   // the current Run call; nil once Wait or Stop has collected it
	httpAddr string // the bound listen.http address: Client dials it, Restart binds it again
}

// run is one server.Run call in its goroutine.
type run struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error // set before done is closed
	grace  time.Duration
}

// How long the harness waits before it gives up and fails the test. A healthy start takes milliseconds; the bounds
// only keep a broken server from hanging the test run.
const (
	startTimeout = 10 * time.Second
	stopSlack    = 5 * time.Second // on top of the server's shutdown_timeout
)

// Start starts a server and waits until it is ready. It fails the test when the server can't start; use Try for a
// server that is expected to refuse. The test's cleanup stops the server.
func Start(t testing.TB, opts Options) *Server {
	t.Helper()
	s, err := Try(t, opts)
	if err != nil {
		t.Fatalf("servertest: %v", err)
	}
	return s
}

// Try is Start for a server that may refuse to start: it returns the error of server.New or Server.Start (check it
// with server.NeedsOperator, errors.Is(err, config.ErrDataDirLocked), …) instead of failing the test.
func Try(t testing.TB, opts Options) (*Server, error) {
	t.Helper()
	if opts.TLS {
		t.Fatal("servertest: Options.TLS needs the TLS manager, which this build doesn't have yet (README S44)")
	}
	s := &Server{opts: opts, logs: &logBuffer{}, DataDir: opts.DataDir}
	if s.DataDir == "" {
		// The server creates it, private, as on a first run.
		s.DataDir = filepath.Join(t.TempDir(), "data")
	}
	s.AdminSocket = filepath.Join(socketDir(t), "admin.sock")
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("servertest: cookie jar: %v", err)
	}
	s.Client = &http.Client{
		Jar: jar,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "tcp", s.boundAddr())
			},
			DisableCompression: true, // tests see the encoding the server chose for the headers they sent
		},
	}
	t.Cleanup(func() {
		s.Stop(t)
		if t.Failed() {
			t.Logf("servertest: server log:\n%s", s.Logs())
		}
	})
	if err := s.start(t, "127.0.0.1:0"); err != nil {
		return nil, err
	}
	return s, nil
}

// start builds a server on httpAddr, starts it and waits for readiness.
func (s *Server) start(t testing.TB, httpAddr string) error {
	t.Helper()
	cfg, err := s.loadConfig(httpAddr)
	if err != nil {
		t.Fatalf("servertest: config flags: %v", err)
	}
	deps := s.opts.Deps
	deps.InProcess = true
	if deps.Host.Environ == nil && deps.Host.Root == "" {
		deps.Host.Environ = append(os.Environ(), config.EnvAllowEphemeralData+"=1")
	}
	level := new(slog.LevelVar)
	level.Set(slog.LevelDebug)
	log := logx.New(logx.Options{Level: level, Format: "json", Out: s.logs})

	srv, err := server.New(cfg, log, deps)
	if err != nil {
		return err
	}
	startCtx, cancelStart := context.WithTimeout(context.Background(), startTimeout)
	defer cancelStart()
	if err := srv.Start(startCtx); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	r := &run{cancel: cancel, done: make(chan struct{}), grace: cfg.ShutdownTimeout.Duration + stopSlack}
	go func() {
		defer close(r.done)
		r.err = srv.Run(runCtx)
	}()
	s.mu.Lock()
	s.run = r
	s.httpAddr = srv.Addrs().HTTP.String()
	s.mu.Unlock()

	site := srv.Site()
	s.Srv, s.Cfg = srv, cfg
	s.URL = "http://" + site.Host
	s.WSURL = "ws://" + site.Host + "/ws"
	return s.waitReady(startCtx)
}

// loadConfig builds the config from flags alone, as `isshoni config init` does: no file and no environment, so
// the developer's machine can't leak into a test.
func (s *Server) loadConfig(httpAddr string) (*config.Config, error) {
	args := []string{
		"--tls.mode=off",
		"--listen.http=" + httpAddr,
		"--listen.ice-udp=127.0.0.1:0",
		"--listen.ice-tcp=127.0.0.1:0",
		"--listen.admin-socket=" + s.AdminSocket,
		"--network.include-loopback=true",
		"--data-dir=" + s.DataDir,
		"--log.level=debug",
	}
	args = append(args, s.opts.Flags...)
	set := flag.NewFlagSet("servertest", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	cfg, err := config.LoadFlags(set, args)
	if cfg == nil {
		return nil, err // a flag that doesn't parse: a bug in the test
	}
	// A *config.ValidationError is not returned here: server.New reports the config's errors, also those of the
	// values that Config changes.
	if s.opts.Config != nil {
		s.opts.Config(cfg)
	}
	return cfg, nil
}

// waitReady polls /readyz until it answers 200.
func (s *Server) waitReady(ctx context.Context) error {
	var last string
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+"/readyz", nil)
		if err != nil {
			return fmt.Errorf("servertest: %w", err)
		}
		res, err := s.Client.Do(req)
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return nil
			}
			last = fmt.Sprintf("%d %s", res.StatusCode, bytes.TrimSpace(body))
		} else {
			last = err.Error()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("servertest: the server was not ready within %v (last /readyz: %s)", startTimeout, last)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// Wait waits for the server's Run to return and gives its error: nil after a stop, server.ErrRestartRequested
// after a shutdown for a restart or a restore, and so on. Use it after the test (or the server itself) began a
// shutdown through Srv.Shutdown or the admin socket; Stop is the usual way to stop. It fails the test when Run does
// not return within shutdown_timeout plus a few seconds. After Wait the server is gone: Stop does nothing, and
// Restart starts a new one. A second Wait returns nil.
func (s *Server) Wait(t testing.TB) error {
	t.Helper()
	s.mu.Lock()
	r := s.run
	s.run = nil
	s.mu.Unlock()
	if r == nil {
		return nil
	}
	defer r.cancel()
	select {
	case <-r.done:
	case <-time.After(r.grace):
		t.Fatalf("servertest: Run did not return within %v of the shutdown", r.grace)
	}
	s.Client.CloseIdleConnections()
	return r.err
}

// Stop shuts the server down gracefully, as SIGTERM does (it cancels Run's context), and waits for Run to return.
// It fails the test when Run returns an error other than server.ErrRestartRequested or takes longer than
// shutdown_timeout plus a few seconds. Calling it again does nothing; the test's cleanup calls it too.
func (s *Server) Stop(t testing.TB) {
	t.Helper()
	s.mu.Lock()
	r := s.run
	s.mu.Unlock()
	if r == nil {
		return
	}
	r.cancel()
	if err := s.Wait(t); err != nil && !errors.Is(err, server.ErrRestartRequested) {
		t.Errorf("servertest: Run: %v", err)
	}
}

// Restart stops the server if it still runs and starts a new one with the same options on the same data directory,
// admin socket path and port, so URL stays valid and Client keeps its cookies, as a browser does across a server
// restart. It stands in for the re-exec after a restore or rotate-secrets (04 §6.5). Cfg and Srv are replaced.
func (s *Server) Restart(t testing.TB) {
	t.Helper()
	s.Stop(t)
	if err := s.start(t, s.boundAddr()); err != nil {
		t.Fatalf("servertest: restart: %v", err)
	}
}

// boundAddr returns the address of the server's HTTP listener ("127.0.0.1:<port>").
func (s *Server) boundAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.httpAddr
}

// Logs returns everything the server has logged so far (JSON lines, debug level), across restarts.
func (s *Server) Logs() string { return s.logs.String() }

// socketDir returns a fresh directory with a short path for the admin socket: t.TempDir() is too long for a unix
// socket path on macOS (104 bytes, 04 §12).
func socketDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "isshoni")
	if err == nil && len(filepath.Join(dir, "admin.sock")) > 100 {
		_ = os.RemoveAll(dir)
		dir, err = os.MkdirTemp("/tmp", "isshoni")
	}
	if err != nil {
		t.Fatalf("servertest: directory for the admin socket: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// logBuffer collects the server's log lines from its goroutines.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
