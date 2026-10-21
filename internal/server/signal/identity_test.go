package signal_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/server/signal"
)

// A bearer identity's device name (M2 02 §6.3) is user content: LogValue leaves it out, with either handler, at the
// top level and inside a group.
func TestIdentityLogValueOmitsDeviceName(t *testing.T) {
	id := signal.Identity{UserID: "u1", Name: "alice", DeviceID: "dev-secret", DeviceName: "Gaming PC of alice"}
	for name, mk := range map[string]func(*bytes.Buffer) slog.Handler{
		"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
		"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
	} {
		var buf bytes.Buffer
		log := slog.New(mk(&buf))
		log.Info("x", "id", id)
		log.Info("x", slog.Group("g", slog.Any("id", id)))
		log.With("id", id).Info("with")
		out := buf.String()
		for _, hidden := range []string{"Gaming PC", "alice", "dev-secret"} {
			if strings.Contains(out, hidden) {
				t.Errorf("%s handler logs %q:\n%s", name, hidden, out)
			}
		}
		if strings.Count(out, "u1") != 3 {
			t.Errorf("%s handler: want the user id in each of the 3 lines:\n%s", name, out)
		}
	}
	if s := fmt.Sprint(slog.AnyValue(id).Resolve()); strings.Contains(s, "Gaming PC") {
		t.Errorf("the resolved value shows the device name: %s", s)
	}
}
