package app

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/client/core"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/desktop/session"
	"github.com/MoonWX/isshoni/internal/desktop/share"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// The app's wiring of C2's session and the share controller (README WA58).

// TestNewWiresSessionAndShares: with an engine and no Deps.Session, New builds internal/desktop/session and the
// share controller over it; the picker is owned by the main window; a Start on a server that isn't linked is 02's
// ErrNotLinked; Run and Quit leave nothing running.
func TestNewWiresSessionAndShares(t *testing.T) {
	e := newEnv(t, []string{"https://a.example.com"}, nil)
	eng := fake.NewEngine(fake.EngineConfig{})
	defer func() { _ = eng.Close() }()
	a, err := New(Deps{Settings: e.app.d.Settings, Servers: e.srv, Engine: eng, Shell: e.shell, OS: fakeOS{},
		Log: e.app.d.Log})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.d.Session.(*session.Session); !ok || a.pub == nil || a.Shares() == nil {
		t.Fatalf("session %T, publisher %v", a.d.Session, a.pub)
	}
	if _, live := a.Live(); live || a.Info().PublisherConnectionID != "" {
		t.Fatal("live without a share")
	}
	p, err := a.Shares().Pick(context.Background())
	if err != nil || p == nil || len(p.PickID) != 16 {
		t.Fatalf("Pick = %+v, %v", p, err)
	}
	if e.shell.count("show(true)") != 1 {
		t.Error("the window wasn't shown before the picker")
	}
	_, err = a.Shares().Start(context.Background(), share.StartRequest{PickID: p.PickID, RoomID: "lounge"})
	if !errors.Is(err, deviceauth.ErrNotLinked) {
		t.Fatalf("Start on an unlinked server: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- a.Run(context.Background()) }()
	time.Sleep(50 * time.Millisecond)
	a.Quit(true)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * QuitTimeout):
		t.Fatal("Run didn't return after Quit")
	}
}

// TestNoEngineNoSession: without an engine there is no C2; the controller answers unsupported_os.
func TestNoEngineNoSession(t *testing.T) {
	e := newEnv(t, nil, nil)
	if e.app.d.Session != nil || e.app.pub != nil {
		t.Fatalf("session %v", e.app.d.Session)
	}
	if _, err := e.app.Shares().Pick(context.Background()); !errors.Is(err, media.ErrUnsupportedOS) {
		t.Fatalf("Pick: %v", err)
	}
}

// fakePub is a C2 session for the app's hooks.
type fakePub struct {
	ran       atomic.Bool
	mu        sync.Mutex
	states    []deviceauth.State
	refreshes int
}

func (p *fakePub) Run(ctx context.Context) error {
	p.ran.Store(true)
	<-ctx.Done()
	return nil
}
func (p *fakePub) Ready(context.Context) (session.Client, string, error) {
	return nil, "", deviceauth.ErrNotLinked
}
func (p *fakePub) ConnectionID() string { return "c_k3v9q2m7xw4pa8d1" }
func (p *fakePub) OnAuthState(s deviceauth.State) {
	p.mu.Lock()
	p.states = append(p.states, s)
	p.mu.Unlock()
}
func (p *fakePub) Refresh() { p.mu.Lock(); p.refreshes++; p.mu.Unlock() }

// TestSessionHooks: 02's OnState reaches the session, and App.Info's publisherConnectionId is C2's.
func TestSessionHooks(t *testing.T) {
	e := newEnv(t, nil, nil)
	pub := &fakePub{}
	a, err := New(Deps{Settings: e.app.d.Settings, Servers: e.srv, Session: pub, Shell: e.shell, OS: fakeOS{},
		Log: e.app.d.Log})
	if err != nil {
		t.Fatal(err)
	}
	if a.pub != pub {
		t.Fatal("the session's hooks aren't wired")
	}
	st := deviceauth.State{Origin: "https://a.example.com", Status: deviceauth.StatusLinked}
	a.OnAuthState(st)
	pub.mu.Lock()
	states := slices.Clone(pub.states)
	pub.mu.Unlock()
	if len(states) != 1 || states[0].Origin != st.Origin {
		t.Errorf("OnAuthState %v", states)
	}
	if got := a.Info().PublisherConnectionID; got != "c_k3v9q2m7xw4pa8d1" {
		t.Errorf("publisherConnectionId %q", got)
	}
}

// appClient, appShare and appPublisher are C2 as the controller sees it, recording into the shell's call list so a
// test sees the share's stop next to the shell's quit.
type appClient struct {
	shell *fakeShell
	mu    sync.Mutex
	sh    *appShare
}

func (c *appClient) Status() core.Status {
	return core.Status{Conn: core.ConnOnline, ConnectionID: "c_k3v9q2m7xw4pa8d1"}
}
func (c *appClient) Subscribe() (<-chan core.Event, func()) {
	ch := make(chan core.Event)
	var once sync.Once
	return ch, func() { once.Do(func() { close(ch) }) }
}
func (c *appClient) StartShare(_ context.Context, r core.ShareRequest) (share.Share, error) {
	s := &appShare{shell: c.shell, done: make(chan struct{}), snap: core.ShareSnapshot{ID: "s_q7m2k9x4w3bd8c1a",
		RoomID: r.RoomID, State: core.StateLive}}
	c.mu.Lock()
	c.sh = s
	c.mu.Unlock()
	return s, nil
}
func (c *appClient) StopOther(context.Context, string, string) error { return nil }
func (c *appClient) WatchServerStats(bool)                           {}

type appShare struct {
	shell *fakeShell
	done  chan struct{}
	once  sync.Once
	mu    sync.Mutex
	snap  core.ShareSnapshot
}

func (s *appShare) Snapshot() core.ShareSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap
}
func (s *appShare) SetPreset(context.Context, protocol.Preset) error { return nil }
func (s *appShare) SetPaused(context.Context, bool) error            { return nil }
func (s *appShare) Stop(ctx context.Context) error {
	s.shell.record("share.stop")
	s.once.Do(func() {
		s.mu.Lock()
		s.snap.State, s.snap.End = core.StateEnded, &core.EndInfo{Reason: core.EndUser, ByThisApp: true}
		s.mu.Unlock()
		close(s.done)
	})
	return ctx.Err()
}
func (s *appShare) Done() <-chan struct{} { return s.done }

type appPublisher struct{ c *appClient }

func (p appPublisher) Ready(context.Context) (share.Client, string, error) {
	return p.c, "https://a.example.com", nil
}

// appAudio is 05's service as the app runs it and the controller calls it.
type appAudio struct{ shell *fakeShell }

func (a appAudio) Run(ctx context.Context) error { <-ctx.Done(); return nil }
func (a appAudio) Close() error                  { a.shell.record("audio.close"); return nil }
func (a appAudio) OnShareStarted(pid uint32, withAudio bool) {
	a.shell.record("audio.started")
}
func (a appAudio) OnShareStopped()          { a.shell.record("audio.stopped") }
func (a appAudio) SetPaused(bool)           {}
func (a appAudio) SetShareSound(bool) error { return nil }

// TestQuitStopsShareFirst is 03 §22.1's quit order: the share stops (01's Share.Stop, then 05's OnShareStopped)
// before 05's service closes and the shell quits; Live follows the controller.
func TestQuitStopsShareFirst(t *testing.T) {
	e := newEnv(t, []string{"https://a.example.com"}, nil)
	eng := fake.NewEngine(fake.EngineConfig{})
	defer func() { _ = eng.Close() }()
	d := e.app.d
	d.Engine, d.Audio = eng, appAudio{shell: e.shell}
	ctl := newShares(d, appPublisher{c: &appClient{shell: e.shell}})
	t.Cleanup(ctl.Close)
	e.app.d.Audio, e.app.shares, e.app.sharing = d.Audio, ctl, ctl.Live

	p, err := ctl.Pick(context.Background())
	if err != nil || p == nil {
		t.Fatalf("Pick = %+v, %v", p, err)
	}
	if _, err := ctl.Start(context.Background(), share.StartRequest{PickID: p.PickID, RoomID: "lounge",
		WithAudio: true}); err != nil {
		t.Fatal(err)
	}
	if room, live := e.app.Live(); room != "lounge" || !live {
		t.Fatalf("Live = %q, %v", room, live)
	}
	e.shell.reset()
	start := time.Now()
	e.app.Quit(true)
	if time.Since(start) > QuitTimeout {
		t.Errorf("Quit took %v", time.Since(start))
	}
	calls, _ := e.shell.snapshot()
	if want := []string{"share.stop", "audio.stopped", "audio.close", "quit"}; !slices.Equal(calls, want) {
		t.Fatalf("calls %q, want %q", calls, want)
	}
	if _, live := e.app.Live(); live {
		t.Error("live after Quit")
	}
}
