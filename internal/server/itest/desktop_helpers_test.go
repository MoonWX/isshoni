package itest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/MoonWX/isshoni/internal/client/core"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/sfu/sfutest"
)

// The desktop cases of docs/m2/01-client-core.md §14 (desktop_*_test.go) run the client core (internal/client/core)
// against the test's server, on fake.Engine, next to the harness's own clients, which watch what it publishes. The
// core authenticates as the desktop app does (01 §11.3, README WA73): with the bearer access token of a device that
// its member linked through the real deviceauth.Manager (linkedManager in device_helpers_test.go, the member's
// browser approving the code with its session cookie), handed to the core by appAuth, as 03's session adapter does.
// Every share starts from fake.Engine.Pick, as in the app.

// closeBound is what Close may take (01 §11.2: 3 s for the share.stop reply), plus the close handshake on loopback.
const closeBound = 3*time.Second + time.Second

// desktopApp is a member's desktop app, as far as its Go side goes: the client core on a fake engine, running from
// its creation until the test ends.
type desktopApp struct {
	t      *testing.T
	name   string
	core   *core.Client
	engine *fake.Engine
	runErr chan error
	logs   *lockedBuffer
	unsub  func()
	rec    sync.WaitGroup
	auth   *appAuth

	mu     sync.Mutex
	events []core.Event
	ran    bool // a test's waitRun has read what Run returned
}

// desktopOptions configure a desktopApp.
type desktopOptions struct {
	engine fake.EngineConfig
}

// desktop links a desktop app for m (linkApp) and starts its core, and waits until its connection is online. Its
// core is closed when the test ends (before the server stops, and before its manager closes), and the close is
// checked: within closeBound, Run returns nil (unless the test read a terminal stop with waitRun), the engine's
// session has stopped. When the test failed, the core's log is printed.
func (w *world) desktop(m *member, o desktopOptions) *desktopApp {
	w.t.Helper()
	t := w.t
	auth := w.linkApp(m)
	logs := &lockedBuffer{}
	engine := fake.NewEngine(o.engine)
	se := sfutest.LoopbackSettings()
	c, err := core.New(core.Options{
		ServerURL:  w.srv.URL,
		Auth:       auth,
		Engine:     engine,
		Settings:   &se,
		HTTPClient: &http.Client{Transport: w.srv.Client.Transport},
		Logger:     slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	d := &desktopApp{t: t, name: m.name + "'s app", core: c, engine: engine, runErr: make(chan error, 1), logs: logs,
		auth: auth}
	var events <-chan core.Event
	events, d.unsub = c.Subscribe()
	d.rec.Go(func() {
		for ev := range events {
			d.mu.Lock()
			d.events = append(d.events, ev)
			d.mu.Unlock()
		}
	})
	go func() { d.runErr <- c.Run(context.Background()) }()
	t.Cleanup(d.close)
	eventually(t, func() bool { return c.Status().Conn == core.ConnOnline },
		func() string { return fmt.Sprintf("%s: status %+v, want online", d.name, c.Status()) })
	return d
}

// close closes the core and checks how it ended.
func (d *desktopApp) close() {
	began := time.Now()
	if err := d.core.Close(); err != nil {
		d.t.Errorf("%s: Close: %v", d.name, err)
	}
	if took := time.Since(began); took > closeBound {
		d.t.Errorf("%s: Close took %v, want at most %v", d.name, took, closeBound)
	}
	d.mu.Lock()
	ran := d.ran
	d.mu.Unlock()
	if !ran {
		select {
		case err := <-d.runErr:
			if err != nil {
				d.t.Errorf("%s: Run = %v, want nil after Close", d.name, err)
			}
		case <-time.After(waitTimeout):
			d.t.Errorf("%s: Run did not return after Close", d.name)
		}
	}
	if st := d.core.Status(); st.Conn != core.ConnStopped || st.Share != nil {
		d.t.Errorf("%s: status after Close = %+v, want stopped without a share", d.name, st)
	}
	if s := d.engine.Session(); s != nil {
		d.t.Errorf("%s: the engine's session still runs after Close", d.name)
	}
	d.unsub()
	d.rec.Wait()
	_ = d.engine.Close()
	if d.t.Failed() {
		d.t.Logf("%s's log:\n%s", d.name, d.logs.String())
	}
}

// waitRun waits until Run returns by itself, after a terminal stop (01 §11.3), and returns what it returned; close
// then checks the rest of the end without asking for nil.
func (d *desktopApp) waitRun() error {
	d.t.Helper()
	select {
	case err := <-d.runErr:
		d.mu.Lock()
		d.ran = true
		d.mu.Unlock()
		return err
	case <-time.After(waitTimeout):
		d.t.Fatalf("%s: Run did not return: status %+v", d.name, d.core.Status())
		return nil
	}
}

// appAuth is core.Auth over the app's deviceauth.Manager, as 03's session adapter makes it (01 §11.2: the Manager's
// SessionEnded returns the resulting State, so the Manager itself is no core.Auth). It counts the core's token calls
// and keeps its SessionEnded calls with the state each one left; it never keeps a token.
type appAuth struct {
	mgr *deviceauth.Manager

	mu     sync.Mutex
	tokens int         // AccessToken calls that returned a token, forced or not
	forced int         // AccessToken(ctx, true) calls
	ended  []endedCall // SessionEnded calls, in order
}

// endedCall is one SessionEnded call of the core: its code, the manager's state when it returned, and when.
type endedCall struct {
	code  protocol.ErrorCode
	state deviceauth.State
	at    time.Time
}

// linkApp links an app of m's to the test's server, as the user does from the app's sign-in screen and their
// browser (linkedManager, m's session cookie approving the code), and returns the core.Auth over its manager.
func (w *world) linkApp(m *member) *appAuth {
	w.t.Helper()
	return &appAuth{mgr: linkedManager(w.t, w.srv, sessionCookie(w.t, w.srv, m.http.Jar))}
}

// AccessToken is the manager's, counted.
func (a *appAuth) AccessToken(ctx context.Context, force bool) (protocol.Secret, error) {
	tok, err := a.mgr.AccessToken(ctx, force)
	a.mu.Lock()
	defer a.mu.Unlock()
	if force {
		a.forced++
	}
	if err == nil {
		a.tokens++
	}
	return tok, err
}

// SessionEnded is the manager's, recorded with the state it returned.
func (a *appAuth) SessionEnded(ctx context.Context, code protocol.ErrorCode) {
	st := a.mgr.SessionEnded(ctx, code)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ended = append(a.ended, endedCall{code: code, state: st, at: time.Now()})
}

// counts returns how many tokens the core got and how many forced refreshes it asked for.
func (a *appAuth) counts() (tokens, forced int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tokens, a.forced
}

// endedCalls returns the core's SessionEnded calls so far.
func (a *appAuth) endedCalls() []endedCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.ended)
}

// start picks a source on the fake engine, as the app's share sheet does, and starts sharing it in room with the
// auto preset and audio.
func (d *desktopApp) start(room string) *core.Share {
	d.t.Helper()
	ctx := waitCtx(d.t)
	src, err := d.engine.Pick(ctx, media.PickRequest{})
	if err != nil {
		d.t.Fatalf("%s: pick: %v", d.name, err)
	}
	sh, err := d.core.StartShare(ctx, core.ShareRequest{RoomID: room, Source: src, Preset: protocol.PresetAuto, Audio: true})
	if err != nil {
		d.t.Fatalf("%s: StartShare in %s: %v", d.name, room, err)
	}
	return sh
}

// waitState waits until the share is in state.
func (d *desktopApp) waitState(sh *core.Share, state core.ShareState) core.ShareSnapshot {
	d.t.Helper()
	var snap core.ShareSnapshot
	eventually(d.t, func() bool {
		snap = sh.Snapshot()
		return snap.State == state
	}, func() string { return fmt.Sprintf("%s: the share is %+v, want it %s", d.name, snap, state) })
	return snap
}

// waitDone waits until the share has ended and returns why.
func (d *desktopApp) waitDone(sh *core.Share) core.EndInfo {
	d.t.Helper()
	select {
	case <-sh.Done():
	case <-time.After(waitTimeout):
		d.t.Fatalf("%s: the share did not end: %+v", d.name, sh.Snapshot())
	}
	snap := sh.Snapshot()
	if snap.State != core.StateEnded || snap.End == nil {
		d.t.Fatalf("%s: the share after Done is %+v, want it ended with an EndInfo", d.name, snap)
	}
	return *snap.End
}

// shareEvents returns the EventShare snapshots so far.
func (d *desktopApp) shareEvents() []core.ShareSnapshot {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []core.ShareSnapshot
	for _, ev := range d.events {
		if ev.Kind == core.EventShare && ev.Share != nil {
			out = append(out, *ev.Share)
		}
	}
	return out
}

// statusEvents returns the EventStatus statuses so far.
func (d *desktopApp) statusEvents() []core.Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []core.Status
	for _, ev := range d.events {
		if ev.Kind == core.EventStatus && ev.Status != nil {
			out = append(out, *ev.Status)
		}
	}
	return out
}

// noLeaks checks, after everything else of the test has been cleaned up, that the test left no goroutine running:
// the core's, its engine session's and the pump's included (01 §14 D17). Pion's goroutines get 2 s to settle, as in
// TestMain. Register it first, so that its cleanup runs last.
func noLeaks(t *testing.T) {
	t.Helper()
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() {
		err := goleak.Find(ignore)
		for deadline := time.Now().Add(2 * time.Second); err != nil && time.Now().Before(deadline); {
			time.Sleep(100 * time.Millisecond)
			err = goleak.Find(ignore)
		}
		if err != nil {
			t.Errorf("goroutines left after the test: %v", err)
		}
	})
}

// defaultRoom is the server's default room, as a welcome names it.
func defaultRoom(c *client) string {
	return c.sig.Welcome().DefaultRoomID
}

// createRoom creates a room as the admin (POST /api/v1/admin/rooms) and returns its id.
func (w *world) createRoom(name string) string {
	w.t.Helper()
	body, err := json.Marshal(api.CreateRoomRequest{Name: name})
	if err != nil {
		w.t.Fatal(err)
	}
	ctx := waitCtx(w.t)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.srv.URL+"/api/v1/admin/rooms", bytes.NewReader(body))
	if err != nil {
		w.t.Fatal(err)
	}
	req.Header.Set("Origin", w.srv.URL)
	req.Header.Set("Content-Type", "application/json")
	res, err := w.srv.Client.Do(req) // its jar has the admin's session from the setup
	if err != nil {
		w.t.Fatalf("create room: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	answer, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
	if res.StatusCode != http.StatusCreated {
		w.t.Fatalf("create room = %d %s, want 201", res.StatusCode, answer)
	}
	var room api.RoomResponse
	if err := json.Unmarshal(answer, &room); err != nil || room.Room.ID == "" {
		w.t.Fatalf("create room answered %s: %v", answer, err)
	}
	return room.Room.ID
}

// lockedBuffer is a bytes.Buffer for a logger that several goroutines write to.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
