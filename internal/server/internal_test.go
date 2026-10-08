package server

import (
	"context"
	"flag"
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
	srv := newMainServer(h, discardLog())
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
}

// TestMainServerErrorLogIsDebug: net/http's own error lines reach the logger at debug level only (04 §9.1).
func TestMainServerErrorLogIsDebug(t *testing.T) {
	var seen []slog.Level
	log := slog.New(recordLevels{levels: &seen})
	srv := newMainServer(http.NotFoundHandler(), log)
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
		if o.API != nil || o.WS != nil {
			t.Error("API or WS is set without the wiring: the router must answer 404 there")
		}
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
