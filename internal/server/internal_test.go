package server

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"testing/fstest"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/httpapi"
)

// loadFlags builds an off-mode config from flags alone. Nothing in this file starts a server, so the data
// directory is never created and the default admin socket path is never touched.
func loadFlags(t *testing.T, args ...string) *config.Config {
	t.Helper()
	base := []string{
		"--tls.mode=off",
		"--listen.http=127.0.0.1:0",
		"--data-dir=" + filepath.Join(t.TempDir(), "data"),
	}
	set := flag.NewFlagSet("test", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	cfg, err := config.LoadFlags(set, append(base, args...))
	if cfg == nil {
		t.Fatalf("LoadFlags: %v", err)
	}
	return cfg
}

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// TestMainServerLimits pins the main http.Server to 04 §7.8: header and idle timeouts, the header size, and no
// server-wide read or write timeout (they would cut WebSockets).
func TestMainServerLimits(t *testing.T) {
	h := http.NotFoundHandler()
	srv := newMainServer(h, discardLog(), &pendingConns{})
	if srv.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 10s", srv.ReadHeaderTimeout)
	}
	if srv.IdleTimeout != 120*time.Second {
		t.Errorf("IdleTimeout = %v, want 120s", srv.IdleTimeout)
	}
	if srv.MaxHeaderBytes != 16<<10 {
		t.Errorf("MaxHeaderBytes = %d, want 16 KiB", srv.MaxHeaderBytes)
	}
	if srv.ReadTimeout != 0 || srv.WriteTimeout != 0 {
		t.Errorf("ReadTimeout = %v, WriteTimeout = %v: both must be 0 for WebSockets", srv.ReadTimeout, srv.WriteTimeout)
	}
	if srv.Protocols == nil || !srv.Protocols.HTTP1() || !srv.Protocols.HTTP2() || srv.Protocols.UnencryptedHTTP2() {
		t.Errorf("Protocols = %v, want HTTP/1 and HTTP/2 over TLS only", srv.Protocols)
	}
	if srv.ErrorLog == nil {
		t.Error("ErrorLog is nil: net/http would write to stderr past the logger")
	}
	if srv.Handler == nil {
		t.Error("Handler is nil")
	}
	if srv.ConnState == nil {
		t.Error("ConnState is nil: the shutdown could not close the connections that never sent a request")
	}
}

// TestMainServerErrorLogIsDebug: net/http's own error lines reach the logger at debug level only (04 §9.1).
func TestMainServerErrorLogIsDebug(t *testing.T) {
	var seen []slog.Level
	log := slog.New(recordLevels{levels: &seen})
	srv := newMainServer(http.NotFoundHandler(), log, &pendingConns{})
	srv.ErrorLog.Print("http: TLS handshake error from 203.0.113.9:4711: EOF") //nolint:forbidigo // net/http's own logger, as net/http calls it
	if !slices.Equal(seen, []slog.Level{slog.LevelDebug}) {
		t.Errorf("levels = %v, want one debug line", seen)
	}
}

// recordLevels is a slog.Handler that records the level of every record.
type recordLevels struct {
	slog.Handler
	levels *[]slog.Level
}

func (recordLevels) Enabled(context.Context, slog.Level) bool { return true }
func (h recordLevels) Handle(_ context.Context, r slog.Record) error {
	*h.levels = append(*h.levels, r.Level)
	return nil
}
func (h recordLevels) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h recordLevels) WithGroup(string) slog.Handler      { return h }

// TestRouterOptionsFromConfig: the router gets the config's effective trusted proxies and the HSTS switch
// (04 §8.5, §9.6), the site, the gate, and the seams of Deps.
func TestRouterOptionsFromConfig(t *testing.T) {
	loopback := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	spa := fstest.MapFS{"index.html": {Data: []byte("<!doctype html>")}}
	api := http.NotFoundHandler()
	ws := http.NotFoundHandler()

	for _, tc := range []struct {
		name        string
		flags       []string
		wantProxies []netip.Prefix
		wantNoHSTS  bool
	}{
		{"defaults: loopback proxies, HSTS on", nil, loopback, false},
		{"explicit proxies", []string{"--network.trusted-proxies=172.30.89.0/24"},
			[]netip.Prefix{netip.MustParsePrefix("172.30.89.0/24")}, false},
		{"no proxies", []string{"--network.trusted-proxies="}, nil, false},
		{"hsts off", []string{"--tls.hsts=false"}, loopback, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadFlags(t, tc.flags...)
			s, err := New(cfg, discardLog(), Deps{SPA: spa, API: api, WS: ws})
			if err != nil {
				t.Fatal(err)
			}
			s.site = config.Site{Origin: "http://localhost:8080", Host: "localhost:8080", Hostname: "localhost", TLSMode: config.TLSOff, Dev: true}
			s.gate = &httpapi.Gate{}
			o := s.routerOptions()
			if !slices.Equal(o.TrustedProxies, tc.wantProxies) {
				t.Errorf("TrustedProxies = %v, want %v", o.TrustedProxies, tc.wantProxies)
			}
			if !slices.Equal(o.TrustedProxies, cfg.Network.TrustedProxies) {
				t.Errorf("TrustedProxies = %v, config has %v", o.TrustedProxies, cfg.Network.TrustedProxies)
			}
			if o.DisableHSTS != tc.wantNoHSTS || o.DisableHSTS != !cfg.TLS.HSTS {
				t.Errorf("DisableHSTS = %v with tls.hsts = %v", o.DisableHSTS, cfg.TLS.HSTS)
			}
			if !reflect.DeepEqual(o.Site, s.site) {
				t.Errorf("Site = %+v, want %+v", o.Site, s.site)
			}
			if o.Gate != s.gate {
				t.Error("Gate is not the server's gate")
			}
			if o.SPA == nil || o.API == nil || o.WS == nil || o.Logger == nil {
				t.Errorf("SPA, API, WS or Logger is nil: %+v", o)
			}
			if o.SPAStatus == nil {
				t.Error("SPAStatus is nil: /setup would stay there after setup (03 §12.6)")
			}
			if _, err := o.SPA.Open("index.html"); err != nil {
				t.Errorf("SPA is not Deps.SPA: %v", err)
			}
		})
	}

	t.Run("embedded web app by default", func(t *testing.T) {
		s, err := New(loadFlags(t), discardLog(), Deps{})
		if err != nil {
			t.Fatal(err)
		}
		o := s.routerOptions()
		if o.SPA == nil {
			t.Fatal("SPA is nil without Deps.SPA, want web.Dist()")
		}
		// A server that has built neither the API nor the hub (before Start, or one without a site) leaves both nil,
		// not a nil pointer in an interface: the router then answers JSON 404 there.
		if o.API != nil || o.WS != nil {
			t.Error("API or WS is set on a server that has built neither: the router must answer 404 there")
		}
	})
}

// fakeConn is a connection of which only Close matters.
type fakeConn struct {
	net.Conn
	closed bool
}

func (c *fakeConn) Close() error {
	c.closed = true
	return nil
}

// TestPendingConns: a connection is tracked from StateNew until its first request header or its end; closeAll
// closes the tracked ones, and one that is accepted afterwards is closed on the spot.
func TestPendingConns(t *testing.T) {
	var p pendingConns
	p.track(&fakeConn{}, http.StateClosed) // a state without a StateNew before it changes nothing
	silent, answered, gone := &fakeConn{}, &fakeConn{}, &fakeConn{}
	for _, c := range []*fakeConn{silent, answered, gone} {
		p.track(c, http.StateNew)
	}
	if len(p.conns) != 3 {
		t.Fatalf("%d connections tracked, want 3", len(p.conns))
	}
	p.track(answered, http.StateActive) // its request header arrived
	p.track(answered, http.StateIdle)
	p.track(gone, http.StateClosed)
	if _, tracked := p.conns[silent]; len(p.conns) != 1 || !tracked {
		t.Fatalf("tracked = %v, want only the connection without a request", p.conns)
	}

	p.closeAll()
	if !silent.closed || answered.closed || gone.closed {
		t.Errorf("after closeAll: silent closed %v, answered closed %v, gone closed %v; want only the silent one closed",
			silent.closed, answered.closed, gone.closed)
	}
	if len(p.conns) != 0 {
		t.Errorf("%d connections still tracked after closeAll", len(p.conns))
	}

	// Accepted between closeAll and the listener closing.
	late := &fakeConn{}
	p.track(late, http.StateNew)
	if !late.closed || len(p.conns) != 0 {
		t.Errorf("a connection accepted after closeAll: closed %v, %d tracked; want it closed and not tracked", late.closed, len(p.conns))
	}
	// net/http still reports the end of the connections that closeAll closed.
	p.track(silent, http.StateClosed)
	p.track(late, http.StateClosed)
	p.closeAll()

	var unused pendingConns
	unused.closeAll() // a server without connections
}

// TestStepError: a shutdown step that returns its ended context's error was forced; any other error is the step's
// own failure.
func TestStepError(t *testing.T) {
	ended, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	<-ended.Done()
	cancelled, cancel2 := context.WithCancel(context.Background())
	cancel2()
	failure := errors.New("disk full")

	for name, tc := range map[string]struct {
		ctx    context.Context
		err    error
		forced bool
	}{
		"out of time":                    {ended, context.DeadlineExceeded, true},
		"out of time, wrapped":           {ended, fmt.Errorf("requests still running were cut off: %w", context.DeadlineExceeded), true},
		"the caller gave up":             {cancelled, context.Canceled, true},
		"a failure in time":              {context.Background(), failure, false},
		"a failure after the time ended": {ended, failure, false},
		"another context's error":        {context.Background(), context.DeadlineExceeded, false},
	} {
		err := stepError(tc.ctx, "http", tc.err)
		if !errors.Is(err, tc.err) {
			t.Errorf("%s: %q does not wrap the step's error", name, err)
		}
		if got := errors.Is(err, ErrShutdownForced); got != tc.forced {
			t.Errorf("%s: %q: forced = %v, want %v", name, err, got, tc.forced)
		}
	}
	got := stepError(ended, "http", fmt.Errorf("requests still running were cut off: %w", context.DeadlineExceeded)).Error()
	if want := "server: shutdown finished by force: http: requests still running were cut off: context deadline exceeded"; got != want {
		t.Errorf("forced step error = %q, want %q", got, want)
	}
	if got, want := stepError(context.Background(), "store", failure).Error(), "server: shutdown: store: disk full"; got != want {
		t.Errorf("failed step error = %q, want %q", got, want)
	}
}

// TestWithin: the shutdown's wait for the store has an end. What finishes in time gives its own result; what does
// not is given up with the context's error, which stepError counts as "closed by force".
func TestWithin(t *testing.T) {
	failure := errors.New("disk full")
	if err := within(context.Background(), func() error { return nil }); err != nil {
		t.Errorf("a wait that ends = %v", err)
	}
	if err := within(context.Background(), func() error { return failure }); !errors.Is(err, failure) {
		t.Errorf("a wait that fails = %v, want its error", err)
	}

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), storeCloseBudget)
		defer cancel()
		release := make(chan struct{})
		begin := time.Now()
		err := within(ctx, func() error {
			<-release
			return nil
		})
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(begin) != storeCloseBudget {
			t.Errorf("a wait that hangs = %v after %v, want the deadline's error after %v", err, time.Since(begin), storeCloseBudget)
		}
		if !errors.Is(stepError(ctx, "store", err), ErrShutdownForced) {
			t.Error("giving up on the store does not count as a forced shutdown")
		}
		// What was given up finishes on its own.
		close(release)
		synctest.Wait()
	})
}

func TestWithBoundPort(t *testing.T) {
	bound := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 54321}
	for _, tc := range []struct {
		configured string
		bound      net.Addr
		want       string
	}{
		{"127.0.0.1:0", bound, "127.0.0.1:54321"},
		{"localhost:0", bound, "localhost:54321"}, // the host stays as configured
		{":0", bound, ":54321"},
		{"[::1]:0", &net.TCPAddr{IP: net.IPv6loopback, Port: 7}, "[::1]:7"},
		{"127.0.0.1:00", bound, "127.0.0.1:54321"},
		{"127.0.0.1:8080", bound, "127.0.0.1:8080"}, // a real port is kept
		{"not an address", bound, "not an address"},
		{"127.0.0.1:0", &net.UnixAddr{Name: "/run/x.sock", Net: "unix"}, "127.0.0.1:0"},
	} {
		if got := withBoundPort(tc.configured, tc.bound); got != tc.want {
			t.Errorf("withBoundPort(%q, %v) = %q, want %q", tc.configured, tc.bound, got, tc.want)
		}
	}
}

// TestConfigProblems: New sees Load's problems and those of values changed after Load, errors first, each once.
func TestConfigProblems(t *testing.T) {
	cfg := loadFlags(t, "--listen.https=:8443") // off mode ignores listen.https: a warning
	all, warnings := configProblems(cfg)
	if len(all) != 1 || len(warnings) != 1 || warnings[0].Key != "listen.https" {
		t.Fatalf("problems = %v, warnings = %v, want the listen.https warning once", all, warnings)
	}

	cfg.ShutdownTimeout = config.Duration{} // changed after Load: only Validate sees it
	all, warnings = configProblems(cfg)
	if len(all) != 2 || len(warnings) != 1 {
		t.Fatalf("problems = %v, warnings = %v, want one error and one warning", all, warnings)
	}
	if all[0].Severity != config.SeverityError || all[0].Key != "shutdown_timeout" {
		t.Errorf("first problem = %+v, want the shutdown_timeout error", all[0])
	}

	// A value Load could not parse is in Problems only (the field holds the default).
	cfg = loadFlags(t, "--shutdown-timeout=soon")
	all, warnings = configProblems(cfg)
	if len(all) != 1 || len(warnings) != 0 || all[0].Key != "shutdown_timeout" {
		t.Errorf("problems = %v, warnings = %v, want the shutdown_timeout parse error", all, warnings)
	}
}

func TestShutdownReasonRestarts(t *testing.T) {
	for reason, want := range map[ShutdownReason]bool{
		ShutdownStop: false, ShutdownRestart: true, ShutdownRestore: true, "": false, "bogus": false,
	} {
		if got := reason.restarts(); got != want {
			t.Errorf("%q.restarts() = %v, want %v", reason, got, want)
		}
	}
}
