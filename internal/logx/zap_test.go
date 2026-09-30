package logx

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestZapBridge(t *testing.T) {
	var buf bytes.Buffer
	zl := NewZapBridge(newJSON(&buf, slog.LevelInfo).With("component", "tls"))

	zl.Debug("hidden")
	zl.Info("certificate obtained successfully",
		zap.String("identifier", "203.0.113.7"),
		zap.Int("attempt", 2),
		zap.Duration("took", 1500*time.Millisecond),
		zap.Strings("names", []string{"a", "b"}),
		zap.Error(errors.New("boom")),
	)
	zl.With(zap.String("issuer", "acme-v02")).Named("acme").Warn("renewing", zap.Bool("ari", true))
	zl.Error("failed", zap.NamedError("cause", errors.New("rate limited")))
	zl.DPanic("dpanic is an error in production") // zap.New without Development: no panic
	zl.Info("challenge", zap.String("token", "s3cr3t"), zap.String("key_auth_token", "s3cr3t"))
	zl.Info("nested", zap.Namespace("ns"), zap.String("inner", "x"))

	if strings.Contains(buf.String(), "s3cr3t") {
		t.Fatalf("zap fields bypassed the safety net:\n%s", buf.String())
	}
	lines := jsonLines(t, &buf)
	if len(lines) != 6 {
		t.Fatalf("got %d lines, want 6:\n%s", len(lines), buf.String())
	}

	info := lines[0]
	want := map[string]any{
		"level": "INFO", "msg": "certificate obtained successfully", "component": "tls",
		"identifier": "203.0.113.7", "attempt": 2.0, "took": float64(1500 * time.Millisecond), "err": "boom",
	}
	for k, v := range want {
		if info[k] != v {
			t.Errorf("info line %s = %v (%T), want %v", k, info[k], info[k], v)
		}
	}
	if names, ok := info["names"].([]any); !ok || len(names) != 2 {
		t.Errorf("info line names = %v", info["names"])
	}
	// Fields keep their order.
	if raw := strings.SplitN(buf.String(), "\n", 2)[0]; strings.Index(raw, `"identifier"`) > strings.Index(raw, `"err"`) {
		t.Errorf("fields out of order: %s", raw)
	}

	warn := lines[1]
	if warn["level"] != "WARN" || warn["issuer"] != "acme-v02" || warn["logger"] != "acme" || warn["ari"] != true {
		t.Errorf("warn line = %v", warn)
	}
	if e := lines[2]; e["level"] != "ERROR" || e["cause"] != "rate limited" {
		t.Errorf("error line = %v", e)
	}
	if e := lines[3]; e["level"] != "ERROR" {
		t.Errorf("dpanic line = %v, want level ERROR", e)
	}
	if c := lines[4]; c["token"] != redacted || c["key_auth_token"] != redacted {
		t.Errorf("challenge line = %v, want tokens redacted", c)
	}
	if ns, ok := lines[5]["ns"].(map[string]any); !ok || ns["inner"] != "x" {
		t.Errorf("namespace line = %v", lines[5])
	}
}

func TestZapBridgeLevels(t *testing.T) {
	var buf bytes.Buffer
	lv := new(slog.LevelVar)
	lv.Set(slog.LevelWarn)
	zl := NewZapBridge(New(Options{Level: lv, Format: "json", Out: &buf}))
	core := zl.Core()
	for _, tt := range []struct {
		level zapcore.Level
		want  bool
	}{
		{zapcore.DebugLevel, false}, {zapcore.InfoLevel, false}, {zapcore.WarnLevel, true},
		{zapcore.ErrorLevel, true}, {zapcore.DPanicLevel, true},
	} {
		if got := core.Enabled(tt.level); got != tt.want {
			t.Errorf("Enabled(%v) = %v at warn, want %v", tt.level, got, tt.want)
		}
	}
	zl.Info("hidden")
	lv.Set(slog.LevelDebug)
	if !core.Enabled(zapcore.DebugLevel) {
		t.Error("Enabled(debug) = false after switching the LevelVar to debug")
	}
	zl.Debug("shown")
	if got := jsonLines(t, &buf); len(got) != 1 || got[0]["msg"] != "shown" || got[0]["level"] != "DEBUG" {
		t.Errorf("lines = %v, want only the debug line", got)
	}
	if err := zl.Sync(); err != nil {
		t.Errorf("Sync() = %v", err)
	}
}
