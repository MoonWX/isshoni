package server_test

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/server"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/ops"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/server/tlsmgr"
)

// The reload of 04 §6.5 (README S90): what SIGHUP changes in a running server. cmd/isshoni loads the config again
// and calls Server.Reload with it; its TestServeReloadsOnSIGHUP sends the real signal to a real process.

// reloaded returns a copy of the running server's config with level as log.level, as cmd/isshoni loads it again
// after an edit of the file.
func reloaded(srv *servertest.Server, level string) *config.Config {
	cfg := *srv.Cfg
	cfg.Log.Level = level
	return &cfg
}

// records returns the server's log records with this message.
func records(t *testing.T, srv *servertest.Server, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rec := range logRecords(t, srv.Logs()) {
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// TestReloadLogLevel: a reload makes the config's log.level the configured level; a level that `isshoni admin
// log-level` set for a while stays until it ends, and then the reloaded one is back, not the one of the start. Keys
// that changed besides log.level are named with the advice to restart, and a config with errors changes nothing.
func TestReloadLogLevel(t *testing.T) {
	level := new(slog.LevelVar) // the process's level: cmd/isshoni's, here the test's
	srv := servertest.Start(t, servertest.Options{Deps: server.Deps{LogLevel: level}})
	ctx := context.Background()

	if err := srv.Srv.Reload(ctx, reloaded(srv, "warn")); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := level.Level(); got != slog.LevelWarn {
		t.Errorf("the level after a reload to warn = %v", got)
	}
	recs := records(t, srv, "reloaded (SIGHUP)")
	if len(recs) != 1 || recs[0]["log_level"] != "warn" || recs[0]["level"] != "INFO" ||
		recs[0]["certificate"] != "not read: the certificate is not from files (tls.mode is not manual)" {
		t.Errorf("the reload's line = %v", recs)
	}
	if got := records(t, srv, "the config has changes that a reload does not apply; restart isshoni to apply them"); len(got) != 0 {
		t.Errorf("a reload that changed log.level alone names keys: %v", got)
	}

	// The admin's level for a while wins over a reload, which becomes the level it falls back to.
	admin, actx := adminSocket(t, srv)
	if err := admin.SetLogLevel(actx, "debug", 500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := srv.Srv.Reload(ctx, reloaded(srv, "error")); err != nil {
		t.Fatal(err)
	}
	if got := level.Level(); got != slog.LevelDebug {
		t.Errorf("a reload during an override set the level to %v; the override (debug) stays", got)
	}
	waitFor(t, "the override to fall back to the reloaded level", func() bool { return level.Level() == slog.LevelError })

	// Keys that a reload does not apply are named, never with their values.
	cfg := reloaded(srv, "info")
	cfg.ShutdownTimeout = config.Duration{Duration: 15 * time.Second}
	cfg.SFU.PauseUnwatchedLayers = !cfg.SFU.PauseUnwatchedLayers
	if err := srv.Srv.Reload(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if got := level.Level(); got != slog.LevelInfo {
		t.Errorf("the level after a reload to info = %v", got)
	}
	recs = records(t, srv, "the config has changes that a reload does not apply; restart isshoni to apply them")
	if len(recs) != 1 || recs[0]["level"] != "WARN" ||
		!slices.Equal(anyStrings(recs[0]["keys"]), []string{"shutdown_timeout", "sfu.pause_unwatched_layers"}) {
		t.Errorf("the line about the keys that take a restart = %v", recs)
	}

	// A config with errors is what a restart would refuse: the level stays, and the log says why.
	bad := reloaded(srv, "debug")
	bad.Log.Format = "xml"
	if err := srv.Srv.Reload(ctx, bad); err != nil {
		t.Fatal(err)
	}
	if got := level.Level(); got != slog.LevelInfo {
		t.Errorf("a config with errors changed the level to %v", got)
	}
	if recs := records(t, srv, "reload: the config has errors: log.level is not reloaded, and isshoni would refuse to "+
		"start with this config (exit 78) until they are fixed"); len(recs) != 1 || recs[0]["errors"] != float64(1) {
		t.Errorf("the line about the config's errors = %v", recs)
	}
	if !strings.Contains(srv.Logs(), `"key":"log.format"`) {
		t.Errorf("the config's error is not in the log:\n%s", srv.Logs())
	}
	// A config that could not be loaded at all: no level either.
	if err := srv.Srv.Reload(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got := len(records(t, srv, "reloaded (SIGHUP) without the log level")); got != 2 {
		t.Errorf("%d reloads without the level, want 2", got)
	}

	// The server serves on: a reload is no restart.
	if res := get(t, srv, "/readyz"); res.status != 200 {
		t.Errorf("/readyz after the reloads = %d", res.status)
	}
	if strings.Contains(srv.Logs(), `"msg":"shutting down"`) {
		t.Error("a reload began a shutdown")
	}

	// Once the shutdown has begun, a reload does nothing.
	srv.Stop(t)
	if err := srv.Srv.Reload(ctx, reloaded(srv, "debug")); err != nil {
		t.Fatal(err)
	}
	if got := level.Level(); got != slog.LevelInfo {
		t.Errorf("a reload after the shutdown set the level to %v", got)
	}
	if len(records(t, srv, "reload ignored: the server is shutting down")) != 1 {
		t.Error("the reload after the shutdown is not in the log")
	}
}

// anyStrings converts a JSON array of strings, as a log record holds it.
func anyStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, s := range list {
		str, _ := s.(string)
		out = append(out, str)
	}
	return out
}

// TestReloadBeforeStart: a reload that comes before the server started (serve takes SIGHUP before the start, which
// may be slow) sets the level, which the start then takes as the configured one.
func TestReloadBeforeStart(t *testing.T) {
	level := new(slog.LevelVar)
	logs := &logCapture{}
	cfg := testConfig(t)
	deps := testDeps()
	deps.LogLevel, deps.SPA = level, testSPA()
	srv, err := server.New(cfg, logs.logger(), deps)
	if err != nil {
		t.Fatal(err)
	}
	next := *cfg
	next.Log.Level = "warn"
	if err := srv.Reload(context.Background(), &next); err != nil {
		t.Fatal(err)
	}
	if got := level.Level(); got != slog.LevelWarn {
		t.Errorf("the level after a reload before the start = %v", got)
	}
	if !strings.Contains(logs.String(), `"certificate":"not read: the server has not started"`) {
		t.Errorf("the reload's line:\n%s", logs.String())
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := srv.Shutdown(context.Background(), server.ShutdownStop); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	}()
	// The admin's level for a while falls back to the reloaded level, the configured one.
	admin := ops.DialAdmin(cfg.Listen.AdminSocket)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := admin.SetLogLevel(ctx, "debug", 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if got := level.Level(); got != slog.LevelDebug {
		t.Errorf("the override's level is %v", got)
	}
	waitFor(t, "the override to fall back to the level of the reload before the start", func() bool {
		return level.Level() == slog.LevelWarn
	})
}

// TestReloadManualCertificate: in manual mode a reload reads tls.cert_file and tls.key_file again (04 §8.4), and the
// new certificate serves; files that can't be used are rejected, the old pair keeps serving, and Reload returns
// the TLS manager's error.
func TestReloadManualCertificate(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{TLS: true})
	ctx := context.Background()
	first, err := handshakeWith(srv.Srv, srv.Roots, servertest.TLSDomain)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile := srv.Cfg.TLS.CertFile, srv.Cfg.TLS.KeyFile
	oldKey, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}

	newRoots := testCert(t, certFile, keyFile, servertest.TLSDomain)
	if err := srv.Srv.Reload(ctx, reloaded(srv, "debug")); err != nil {
		t.Fatalf("Reload with a new certificate: %v", err)
	}
	second, err := handshakeWith(srv.Srv, newRoots, servertest.TLSDomain)
	if err != nil || second.Equal(first) {
		t.Fatalf("after the reload: %v; the new certificate is served: %v", err, err == nil && !second.Equal(first))
	}
	if recs := records(t, srv, "reloaded (SIGHUP)"); len(recs) != 1 || recs[0]["certificate"] != "read again" {
		t.Errorf("the reload's line = %v", recs)
	}

	// The old key with the new certificate: rejected, and the certificate loaded before keeps serving.
	if err := os.WriteFile(keyFile, oldKey, 0o600); err != nil { //nolint:gosec // G703: the harness's own file, under the test's temp directory
		t.Fatal(err)
	}
	if err := srv.Srv.Reload(ctx, reloaded(srv, "debug")); err == nil {
		t.Error("Reload accepted a certificate with another certificate's key")
	}
	if got, err := handshakeWith(srv.Srv, newRoots, servertest.TLSDomain); err != nil || !got.Equal(second) {
		t.Errorf("after a rejected reload: %v; want the certificate loaded before", err)
	}
	if st := srv.Srv.TLSManager().Status(); st.LastErrorCode != tlsmgr.CodeCertInvalid {
		t.Errorf("TLS status = %+v, want the load error", st)
	}
	if recs := records(t, srv, "reloaded (SIGHUP)"); len(recs) != 2 ||
		recs[1]["certificate"] != "rejected: the files can't be used (the line before says why)" {
		t.Errorf("the second reload's line = %v", recs)
	}
	if ok, checks := srv.Srv.Health().Ready(); !ok {
		t.Errorf("not ready after a rejected reload: %v", checks)
	}
}
