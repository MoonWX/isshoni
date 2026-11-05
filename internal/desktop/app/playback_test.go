package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/MoonWX/isshoni/internal/client/core"
	"github.com/MoonWX/isshoni/internal/client/credstore"
	"github.com/MoonWX/isshoni/internal/desktop/session"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/relay"
)

// README WA83-R3: the app follows C2's events from before C2's welcome, so a tab's playback.state that the server
// relays right after the welcome reaches 05 (03 §12.2: the tab sends it once).

// welcomeServer is a signaling server that answers each hello with a welcome and, in the same breath, the relayed
// reports of `after`; then it answers pings until the socket closes. It counts the hellos.
type welcomeServer struct {
	t     *testing.T
	srv   *httptest.Server
	after []protocol.AgentRecv

	mu     sync.Mutex
	hellos int
}

func newWelcomeServer(t *testing.T, after ...protocol.AgentRecv) *welcomeServer {
	t.Helper()
	ws := &welcomeServer{t: t, after: after}
	ws.srv = httptest.NewServer(http.HandlerFunc(ws.serve))
	t.Cleanup(ws.srv.Close)
	return ws
}

func (s *welcomeServer) helloCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hellos
}

func (s *welcomeServer) serve(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = c.CloseNow() }()
	ctx := r.Context()
	_, b, err := c.Read(ctx)
	if err != nil {
		return
	}
	env, err := protocol.ParseEnvelope(b)
	if err != nil || env.Type != protocol.MessageTypeHello {
		return
	}
	s.mu.Lock()
	s.hellos++
	s.mu.Unlock()
	s.write(ctx, c, protocol.MessageTypeWelcome, env.ID, protocol.Welcome{
		Protocol: protocol.Version, ServerVersion: "0.2.0-test",
		Features:      []protocol.Feature{protocol.FeatureAgentRelay},
		Limits:        protocol.Limits{PingIntervalMs: 15000, GraceMs: 30000},
		ConnectionID:  "c_pub",
		ResumeToken:   protocol.Secret("rt-c_pub"),
		DefaultRoomID: "lounge",
		User:          protocol.UserInfo{ID: "u_alice", Name: "alice"},
		ServerTime:    time.Now(),
	})
	for _, m := range s.after {
		s.write(ctx, c, protocol.MessageTypeAgentRecv, "", m)
	}
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			return
		}
		if env, err := protocol.ParseEnvelope(b); err == nil && env.Type == protocol.MessageTypePing {
			s.write(ctx, c, protocol.MessageTypePong, "", protocol.Pong{})
		}
	}
}

func (s *welcomeServer) write(ctx context.Context, c *websocket.Conn, t protocol.MessageType, re string, data any) {
	b, err := protocol.Marshal(t, "", re, data)
	if err != nil {
		s.t.Errorf("welcome server: marshal %s: %v", t, err)
		return
	}
	_ = c.Write(ctx, websocket.MessageText, b)
}

// staticAuth is a core.Auth with a token that the welcome server takes.
type staticAuth struct{}

func (staticAuth) AccessToken(context.Context, bool) (protocol.Secret, error) {
	return protocol.Secret("isa_" + strings.Repeat("A", 43)), nil
}
func (staticAuth) SessionEnded(context.Context, protocol.ErrorCode) {}

// TestPlaybackStateRightAfterWelcome: with the session New builds (the real internal/desktop/session and 01's core
// client over a server), the tab's report that the server relays right after C2's welcome, decoded by the core
// before anything could have asked Ready for the client, reaches 05's SetViewerBrowser, every time. Before WA83-R3
// the app subscribed only once Ready returned, and missed it (desktop-handoff.spec failed 2 runs in 3 without the
// tab's repeat).
func TestPlaybackStateRightAfterWelcome(t *testing.T) {
	report := func(from string) protocol.AgentRecv {
		return protocol.AgentRecv{From: from, Kind: relay.KindPlaybackState,
			Payload: json.RawMessage(`{"playing": true, "browser": "firefox", "os": "windows"}`)}
	}
	// 01's real core client, with a token of its own (02's refresh isn't what this test is about). Set before any
	// app runs, and put back after the last one returned.
	built := make(chan session.Client, 1)
	prev := newCoreClient
	newCoreClient = func(o core.Options) (session.Client, error) {
		o.Auth = staticAuth{}
		c, err := core.New(o)
		if err != nil {
			return nil, err
		}
		select {
		case built <- c:
		default:
		}
		return c, nil
	}
	defer func() { newCoreClient = prev }()
	const runs = 5
	for run := range runs {
		srv := newWelcomeServer(t, report("c_tab"))
		origin := srv.srv.URL
		store := credstore.NewMemory()
		if err := store.Put(origin, credstore.Credential{RefreshToken: "isr_" + strings.Repeat("A", 43),
			DeviceID: "d_1", SavedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		e := newEnvStore(t, []string{origin}, map[string]bool{origin: true}, nil, true, store)
		eng := fake.NewEngine(fake.EngineConfig{})
		rec := &recAudio{}
		a, err := New(Deps{Settings: e.app.d.Settings, Servers: e.srv, Engine: eng, Audio: rec, Shell: e.shell,
			OS: fakeOS{}, Log: e.app.d.Log, InsecureLoopback: true})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = a.Run(ctx)
		}()
		select {
		case <-built:
		case <-time.After(5 * time.Second):
			t.Fatalf("run %d: C2 built no core client", run)
		}
		waitFor(t, "the report right after the welcome", func() bool { return len(viewerCalls(rec)) > 0 })
		if got := viewerCalls(rec); !slices.Equal(got, []string{"viewer(c_tab,firefox,windows,true)"}) {
			t.Errorf("run %d: calls %q", run, got)
		}
		if n := srv.helloCount(); n != 1 {
			t.Errorf("run %d: %d hellos", run, n)
		}
		cancel()
		<-done
		_ = eng.Close()
	}
}
