package signal_test

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/signal"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

var (
	connIDRE      = regexp.MustCompile(`^c_[0-9a-hjkmnp-tv-z]{16}$`)
	resumeTokenRE = regexp.MustCompile(`^r1\.[A-Za-z0-9_-]{56}$`)
)

func TestHandshakeWelcome(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		e.policy.Set(signal.Policy{MaxRoomParticipants: 8, MaxRoomShares: 3, MaxVideoBitrate: 4_000_000})
		cookie, id := e.user(true)
		e.auth.SetUser(id.UserID, "renamed", true) // Revalidate at connect picks it up

		c := e.mustDial(headers(cookie, testOrigin, ""))
		h := signaltest.DefaultHello()
		h.Features = []protocol.Feature{protocol.FeatureAgentRelay, protocol.FeatureSharePause, "future.thing"}
		start := time.Now()
		w, err := c.Hello(ctxT(t), h)
		if err != nil {
			t.Fatalf("hello: %v", err)
		}
		if w.Protocol != 1 || w.ServerVersion != serverVersion || w.MinClientVersion != "" {
			t.Errorf("welcome protocol %d, serverVersion %q, minClientVersion %q", w.Protocol, w.ServerVersion,
				w.MinClientVersion)
		}
		if len(w.Features) != 0 {
			t.Errorf("features %v, want none (the server enables none yet)", w.Features)
		}
		want := protocol.Limits{
			MaxMessageBytes: 65536, MaxSDPBytes: 262144, MaxSharesPerUser: 4, MaxRoomParticipants: 8,
			MaxRoomShares: 3, MaxVideoBitrate: 4_000_000, MessagesPerSecond: 20, MessageBurst: 100,
			PingIntervalMs: 15000, IdleTimeoutMs: 45000, GraceMs: 30000,
		}
		if w.Limits != want {
			t.Errorf("limits %+v, want %+v", w.Limits, want)
		}
		if !connIDRE.MatchString(w.ConnectionID) {
			t.Errorf("connectionId %q", w.ConnectionID)
		}
		if !resumeTokenRE.MatchString(w.ResumeToken.Reveal()) || w.Resumed || w.RoomID != "" {
			t.Errorf("resumeToken ok %v, resumed %v, roomId %q", resumeTokenRE.MatchString(w.ResumeToken.Reveal()),
				w.Resumed, w.RoomID)
		}
		if w.DefaultRoomID != "lounge" {
			t.Errorf("defaultRoomId %q", w.DefaultRoomID)
		}
		if w.User != (protocol.UserInfo{ID: id.UserID, Name: "renamed", Admin: true}) {
			t.Errorf("user %+v", w.User)
		}
		if !w.ServerTime.Equal(start) {
			t.Errorf("serverTime %v, want %v", w.ServerTime, start)
		}
		if n := e.auth.Revalidations(); n != 1 {
			t.Errorf("%d Revalidate calls at connect, want 1", n)
		}
		conns, _, _, slots := signal.Counts(e.hub, id.UserID)
		if conns != 1 || slots != 1 {
			t.Errorf("connections %d, slots %d, want 1 and 1", conns, slots)
		}
		if got := e.metric("isshoni_ws_connections", "kind", "web", "role", "full"); got != 1 {
			t.Errorf("isshoni_ws_connections %v, want 1", got)
		}
		if e.logs.count("connection opened", w.ConnectionID, id.UserID) != 1 {
			t.Errorf("no connection opened log line:\n%s", e.logs)
		}
		ping(t, c)
	})
}

// The welcome's arrays are present even when empty (01 §5).
func TestHandshakeWelcomeRawJSON(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		c := e.mustDial(headers(cookie, "", ""))
		if err := c.Send(protocol.MessageTypeHello, "h1", signaltest.DefaultHello()); err != nil {
			t.Fatal(err)
		}
		env := expectType(t, c, protocol.MessageTypeWelcome)
		if env.Re != "h1" {
			t.Errorf("re %q, want h1", env.Re)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(env.Data, &raw); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"features", "iceServers"} {
			if string(raw[k]) != "[]" {
				t.Errorf("%s = %s, want []", k, raw[k])
			}
		}
		if strings.Contains(string(env.Data), "null") {
			t.Errorf("welcome has null: %s", env.Data)
		}
	})
}
