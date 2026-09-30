package signal_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/goleak"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/signal"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

func validConfig() (signal.Config, signal.Deps) {
	cfg := signal.DefaultConfig()
	cfg.PublicOrigin = testOrigin
	cfg.ServerVersion = testVersion
	cfg.ResumeKey = bytes.Repeat([]byte{1}, 32)
	return cfg, signal.Deps{
		Auth:     signaltest.NewAuth(),
		Rooms:    signaltest.NewRooms(),
		Media:    signaltest.NewMedia(),
		ClientIP: signaltest.ClientIP,
	}
}

func TestDefaultConfig(t *testing.T) {
	c := signal.DefaultConfig()
	if c.Grace != 30*time.Second || c.HelloTimeout != 10*time.Second || c.IdleTimeout != 45*time.Second ||
		c.PingInterval != 15*time.Second || c.StateCoalesce != 200*time.Millisecond ||
		c.ShareStartTimeout != 30*time.Second || c.StalledTimeout != 30*time.Second ||
		c.RevalidateEvery != 5*time.Minute {
		t.Errorf("durations %+v", c)
	}
	want := signal.Limits{MaxSharesPerUser: 4, MaxConnectionsPerUser: 16, PreAuthPerIPPerMinute: 20,
		MaxPreAuthConns: 500, MessagesPerSecond: 20, MessageBurst: 100, BytesPerSecond: 512 << 10,
		BytesBurst: 1 << 20, SendQueueMessages: 512, SendQueueBytes: 8 << 20}
	if c.Limits != want {
		t.Errorf("limits %+v, want %+v", c.Limits, want)
	}
}

func TestNewValidates(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*signal.Config, *signal.Deps)
		want string
	}{
		{"short resume key", func(c *signal.Config, _ *signal.Deps) { c.ResumeKey = c.ResumeKey[:16] },
			"ResumeKey must be 32 bytes"},
		{"no public origin", func(c *signal.Config, _ *signal.Deps) { c.PublicOrigin = "" }, "PublicOrigin"},
		{"public origin with a path", func(c *signal.Config, _ *signal.Deps) { c.PublicOrigin = testOrigin + "/app" },
			"PublicOrigin"},
		{"public origin not http", func(c *signal.Config, _ *signal.Deps) { c.PublicOrigin = "wss://watch.example.com" },
			"PublicOrigin"},
		{"bad allowed origin", func(c *signal.Config, _ *signal.Deps) { c.AllowedOrigins = []string{"wails://"} },
			"AllowedOrigins"},
		{"zero duration", func(c *signal.Config, _ *signal.Deps) { c.HelloTimeout = 0 }, "HelloTimeout must be positive"},
		{"zero limit", func(c *signal.Config, _ *signal.Deps) { c.Limits.MessageBurst = 0 },
			"limit MessageBurst must be positive"},
		{"byte burst below the largest message", func(c *signal.Config, _ *signal.Deps) { c.Limits.BytesBurst = 1000 },
			"limit BytesBurst must be at least"},
		{"missing deps", func(_ *signal.Config, d *signal.Deps) { d.Auth, d.Media = nil, nil },
			"missing Auth, Media"},
		{"missing client IP", func(_ *signal.Config, d *signal.Deps) { d.ClientIP = nil }, "missing ClientIP"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, deps := validConfig()
			tc.mut(&cfg, &deps)
			_, err := signal.New(cfg, deps)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("New: %v, want an error with %q", err, tc.want)
			}
		})
	}
}

// New starts nothing; the metrics can be registered once per registry.
func TestNewStartsNothing(t *testing.T) {
	defer goleak.VerifyNone(t)
	cfg, deps := validConfig()
	reg := prometheus.NewRegistry()
	deps.Metrics = reg
	h, err := signal.New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	goleak.VerifyNone(t)
	if !h.Ready() {
		t.Error("a new hub is not ready")
	}
	if _, err := signal.New(cfg, deps); err == nil || !strings.Contains(err.Error(), "register metrics") {
		t.Errorf("second New on one registry: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	// Vectors without children are not gathered; the plain gauges and counters are.
	for _, name := range []string{"isshoni_rooms", "isshoni_participants", "isshoni_client_frames_decoded_total"} {
		found := false
		for _, mf := range mfs {
			found = found || mf.GetName() == name
		}
		if !found {
			t.Errorf("%s not registered", name)
		}
	}
	if err := h.Shutdown(context.Background(), protocol.ShutdownReasonStop); err != nil {
		t.Errorf("Shutdown of an idle hub: %v", err)
	}
	if h.Ready() {
		t.Error("ready after Shutdown")
	}
}

// Identity never logs the username or the session, with either handler.
func TestIdentityLogValue(t *testing.T) {
	for _, mk := range []func(*bytes.Buffer) slog.Handler{
		func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
		func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
	} {
		var buf bytes.Buffer
		slog.New(mk(&buf)).Info("x", "id", signal.Identity{UserID: "u1", Name: "alice", SessionID: "sess-secret"})
		out := buf.String()
		if !strings.Contains(out, "u1") || strings.Contains(out, "alice") || strings.Contains(out, "sess-secret") {
			t.Errorf("log line %q", out)
		}
	}
}
