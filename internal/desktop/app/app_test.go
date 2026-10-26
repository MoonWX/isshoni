package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/client/credstore"
	"github.com/MoonWX/isshoni/internal/client/deeplink"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/desktop/hotkey"
	"github.com/MoonWX/isshoni/internal/desktop/settings"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/media/fake"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// fakeShell records what the app asked of the shell, in order.
type fakeShell struct {
	mu     sync.Mutex
	calls  []string
	events []any
}

func (s *fakeShell) record(c string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, c)
}

func (s *fakeShell) ShowWindow(focus bool)     { s.record(fmt.Sprintf("show(%v)", focus)) }
func (s *fakeShell) HideWindow()               { s.record("hide") }
func (s *fakeShell) WindowVisible() bool       { return true }
func (s *fakeShell) SetFullscreen(on bool)     { s.record(fmt.Sprintf("fullscreen(%v)", on)) }
func (s *fakeShell) Reload()                   { s.record("reload") }
func (s *fakeShell) MainWindowHandle() uintptr { return 0 }
func (s *fakeShell) OpenURL(string) error      { return nil }
func (s *fakeShell) Notify(Notification) error { return nil }
func (s *fakeShell) Quit()                     { s.record("quit") }
func (s *fakeShell) Emit(name string, data any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "emit "+name)
	s.events = append(s.events, data)
}

func (s *fakeShell) snapshot() ([]string, []any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...), append([]any(nil), s.events...)
}

func (s *fakeShell) count(c string) int {
	calls, _ := s.snapshot()
	n := 0
	for _, x := range calls {
		if x == c {
			n++
		}
	}
	return n
}

func (s *fakeShell) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls, s.events = nil, nil
}

// fakeOS is OSHooks that does nothing.
type fakeOS struct{}

func (fakeOS) OSBuild() int                                { return 0 }
func (fakeOS) RegisterHotkey(HotkeyID, hotkey.Combo) error { return nil }
func (fakeOS) UnregisterHotkey(HotkeyID)                   {}
func (fakeOS) Hotkeys() <-chan HotkeyID                    { return nil }
func (fakeOS) Pill() Pill                                  { return nil }
func (fakeOS) ExcludeFromCapture(uintptr) error            { return nil }
func (fakeOS) SetAutostart(bool, string) error             { return nil }
func (fakeOS) Autostart() (bool, error)                    { return false, nil }
func (fakeOS) OpenSettingsPage(OSPage) error               { return nil }
func (fakeOS) KeepAwake(bool)                              {}
func (fakeOS) Events() <-chan SystemEvent                  { return nil }

// countingTransport fails every request and counts it: tests that must make no network request check the count.
type countingTransport struct{ n atomic.Int64 }

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.n.Add(1)
	return nil, errors.New("no network in this test")
}

type env struct {
	dir   string
	app   *App
	shell *fakeShell
	srv   *deviceauth.Servers
	net   *countingTransport
	clock *fakeClock
	logs  *bytes.Buffer
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// writeServers writes a servers.json with these origins; the first is active, and origins in linked have a user.
func writeServers(t *testing.T, dir string, origins []string, linked map[string]bool) {
	t.Helper()
	type entry struct {
		Origin  string    `json:"origin"`
		Name    string    `json:"name"`
		AddedAt time.Time `json:"addedAt"`
		User    any       `json:"user,omitempty"`
	}
	doc := struct {
		Version int     `json:"version"`
		Active  string  `json:"active"`
		Servers []entry `json:"servers"`
	}{Version: 1}
	for i, o := range origins {
		if i == 0 {
			doc.Active = o
		}
		e := entry{Origin: o, Name: "Server " + o, AddedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
		if linked[o] {
			e.User = map[string]string{"id": "u1", "username": "alex", "role": "user"}
		}
		doc.Servers = append(doc.Servers, e)
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "servers.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func newEnv(t *testing.T, origins []string, linked map[string]bool) *env {
	t.Helper()
	return newEnvWith(t, origins, linked, nil, false)
}

// newEnvWith is newEnv with an HTTP client for 02's managers (nil: every request fails and is counted) and the dev
// builds' loopback rule.
func newEnvWith(t *testing.T, origins []string, linked map[string]bool, hc *http.Client, loopback bool) *env {
	t.Helper()
	dir := t.TempDir()
	if len(origins) > 0 {
		writeServers(t, dir, origins, linked)
	}
	e := &env{dir: dir, shell: &fakeShell{}, net: &countingTransport{}, logs: &bytes.Buffer{},
		clock: &fakeClock{now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}}
	if hc == nil {
		hc = &http.Client{Transport: e.net}
	}
	log := slog.New(slog.NewTextHandler(&lockedWriter{w: e.logs}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv, err := deviceauth.OpenServers(deviceauth.ServersOptions{
		Dir:                   dir,
		Store:                 credstore.NewMemory(),
		App:                   deviceauth.AppInfo{Version: "0.2.0", ClientKind: protocol.ClientKindDesktop, OS: protocol.ClientOSWindows},
		HTTP:                  hc,
		AllowInsecureLoopback: loopback,
		Logger:                log,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.srv = srv
	st, err := settings.Open(settings.Options{Dir: dir, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(Deps{Settings: st, Servers: srv, Shell: e.shell, OS: fakeOS{}, Clock: e.clock.Now, Log: log,
		InsecureLoopback: loopback})
	if err != nil {
		t.Fatal(err)
	}
	e.app = a
	t.Cleanup(func() {
		a.Quit(false)
		a.wg.Wait()
		_ = srv.Close()
		_ = st.Close()
	})
	return e
}

// newLinkServer is a server that answers /info and starts device-flow links whose polls stay pending.
func newLinkServer(t *testing.T) *httptest.Server {
	t.Helper()
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/info":
			_ = json.NewEncoder(w).Encode(api.Info{
				Server:   api.InfoServer{Name: "S", Version: "0.2.0", PublicURL: ts.URL},
				Protocol: api.InfoProtocol{Current: protocol.Version, Min: protocol.MinVersion},
				Features: []string{api.InfoFeatureDeviceFlow},
			})
		case "/api/v1/device/code":
			_ = json.NewEncoder(w).Encode(api.DeviceCodeResponse{
				DeviceCode:              strings.Repeat("A", 43),
				UserCode:                "WDJB-MJHT",
				VerificationURI:         ts.URL + "/link",
				VerificationURIComplete: ts.URL + "/link?code=WDJB-MJHT",
				ExpiresIn:               600,
				Interval:                5,
			})
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"authorization_pending"}}`))
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (l *lockedWriter) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.String()
}

func link(t *testing.T, raw string) deeplink.Link {
	t.Helper()
	l, err := deeplink.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func lastEvent(t *testing.T, s *fakeShell) PendingLink {
	t.Helper()
	_, events := s.snapshot()
	if len(events) == 0 {
		t.Fatal("no event emitted")
	}
	p, ok := events[len(events)-1].(PendingLink)
	if !ok {
		t.Fatalf("event %T", events[len(events)-1])
	}
	return p
}

// TestUnknownServerAsksFirst: a link to a server that isn't listed shows the window and asks "Add server {host}?"
// (deeplink.open {add}) before any network request (03 §13.2, 02 §12.3).
func TestUnknownServerAsksFirst(t *testing.T) {
	e := newEnv(t, []string{"https://a.example.com"}, nil)
	e.app.HandleLink(link(t, "isshoni://xn--bcher-kva.example:8443/r/lounge"))
	want := PendingLink{Add: &LinkAdd{Origin: "https://xn--bcher-kva.example:8443", Host: "xn--bcher-kva.example:8443",
		HostUnicode: "bücher.example:8443", RoomID: "lounge"}}
	got := lastEvent(t, e.shell)
	if got.Add == nil || *got.Add != *want.Add || got.RoomID != "" || got.Switch != nil {
		t.Errorf("deeplink.open = %+v", got)
	}
	if calls, _ := e.shell.snapshot(); len(calls) != 2 || calls[0] != "show(true)" || calls[1] != "emit "+EventDeepLinkOpen {
		t.Errorf("shell calls %v", calls)
	}
	if p := e.app.PendingLink(); p == nil || p.Add == nil || p.Add.Origin != want.Add.Origin {
		t.Errorf("PendingLink = %+v", p)
	}
	if e.net.n.Load() != 0 {
		t.Errorf("%d network requests before Add", e.net.n.Load())
	}
	if e.srv.Has("https://xn--bcher-kva.example:8443") {
		t.Error("the server was added without the user")
	}
	// JSON as the SPA gets it.
	data, _ := json.Marshal(got)
	if !strings.Contains(string(data), `"add":{"origin":"https://xn--bcher-kva.example:8443"`) {
		t.Errorf("JSON %s", data)
	}

	// While the question is open, a newer link is ignored, also after the rate limit's second.
	e.clock.Advance(5 * time.Second)
	e.shell.reset()
	e.app.HandleLink(link(t, "isshoni://other.example.com/"))
	if calls, _ := e.shell.snapshot(); len(calls) != 0 {
		t.Errorf("a link during a confirmation did %v", calls)
	}
	if p := e.app.PendingLink(); p.Add.Origin != want.Add.Origin {
		t.Error("the host changed under the user's cursor")
	}
	// Answered: the next link works again.
	e.app.LinkHandled()
	if e.app.PendingLink() != nil {
		t.Error("LinkHandled kept the link")
	}
	e.app.HandleLink(link(t, "isshoni://other.example.com/"))
	if got := lastEvent(t, e.shell); got.Add == nil || got.Add.Origin != "https://other.example.com" {
		t.Errorf("after the answer: %+v", got)
	}
	if e.net.n.Load() != 0 {
		t.Error("a network request")
	}
}

// TestPendingServerOpensAddStep: a PendingServer value (the installer's /SERVER=) opens the "Add server" step with no
// network request (README WA41's acceptance).
func TestPendingServerOpensAddStep(t *testing.T) {
	e := newEnv(t, nil, nil)
	store := &fakePendingStore{p: Pending{Server: "Watch.Example.com", Room: "lounge"}}
	p, err := TakePending(store)
	if err != nil {
		t.Fatal(err)
	}
	if store.takes != 1 || store.p != (Pending{}) {
		t.Fatalf("TakePending: %d reads, left %+v (must read and delete)", store.takes, store.p)
	}
	e.app.Boot(nil, p)
	got := lastEvent(t, e.shell)
	if got.Add == nil || got.Add.Origin != "https://watch.example.com" || got.Add.Host != "watch.example.com" ||
		got.Add.RoomID != "lounge" {
		t.Errorf("deeplink.open = %+v", got)
	}
	if e.net.n.Load() != 0 {
		t.Errorf("%d network requests", e.net.n.Load())
	}
}

type fakePendingStore struct {
	p     Pending
	takes int
	err   error
}

func (f *fakePendingStore) TakePending() (Pending, error) {
	f.takes++
	p := f.p
	f.p = Pending{}
	return p, f.err
}

// TestBootOrder: Boot handles the command line first, then the Pending values that step 6 read and deleted.
func TestBootOrder(t *testing.T) {
	e := newEnv(t, []string{"https://a.example.com"}, nil)
	// The command line's link (the active server's room) is handled; the Pending link that follows within the
	// same second is dropped by the rate limit (03 §13.2), so the newer one, the command line's, wins.
	e.app.Boot([]string{"--open-url", "isshoni://a.example.com/r/one"}, Pending{Server: "https://a.example.com", Room: "two"})
	if p := e.app.PendingLink(); p == nil || p.RoomID != "one" {
		t.Errorf("PendingLink = %+v", p)
	}
	if n := e.shell.count("emit " + EventDeepLinkOpen); n != 1 {
		t.Errorf("%d deeplink.open events", n)
	}
	// Without a command-line link the Pending link is handled.
	e2 := newEnv(t, []string{"https://a.example.com"}, nil)
	e2.app.Boot([]string{"--updated-from=0.1.9"}, Pending{Server: "https://a.example.com", Room: "two"})
	if p := e2.app.PendingLink(); p == nil || p.RoomID != "two" {
		t.Errorf("PendingLink = %+v", p)
	}
}

func TestPendingValues(t *testing.T) {
	for _, tc := range []struct {
		p        Pending
		loopback bool
		origin   string
		room     string
		err      error
	}{
		{p: Pending{Server: "https://watch.example.com", Room: "lounge"}, origin: "https://watch.example.com", room: "lounge"},
		{p: Pending{Server: "watch.example.com:443"}, origin: "https://watch.example.com"},
		{p: Pending{Server: "https://watch.example.com", Room: "bad room"}, origin: "https://watch.example.com", err: errInvalidRoom},
		{p: Pending{Server: "http://localhost:5173", Room: "r1"}, loopback: true, origin: "http://localhost:5173", room: "r1"},
		{p: Pending{Server: "http://localhost:5173"}, err: deeplink.ErrInvalidOrigin}, // a release build drops it
		{p: Pending{Server: "https://watch.example.com/r/lounge"}, err: deeplink.ErrInvalidOrigin},
		{p: Pending{Room: "lounge"}, err: errNoPending},
		{p: Pending{}, err: errNoPending},
	} {
		l, err := tc.p.Link(tc.loopback)
		if !errors.Is(err, tc.err) {
			t.Errorf("%+v: err %v, want %v", tc.p, err, tc.err)
		}
		if l.Origin != tc.origin || l.RoomID != tc.room {
			t.Errorf("%+v: %+v", tc.p, l)
		}
	}
	// The same value deeplink.Parse returns for the same link.
	l, err := Pending{Server: "https://xn--bcher-kva.example", Room: "r"}.Link(false)
	if err != nil {
		t.Fatal(err)
	}
	if want := link(t, "isshoni://xn--bcher-kva.example/r/r"); l != want {
		t.Errorf("%+v, want %+v", l, want)
	}
	if p, err := TakePending(nil); err != nil || p != (Pending{}) {
		t.Errorf("TakePending(nil) = %+v, %v", p, err)
	}
	// An invalid value is dropped with a log line; Boot handles nothing for it.
	e := newEnv(t, nil, nil)
	e.app.Boot([]string{"--autostart"}, Pending{Server: "javascript:alert(1)"})
	if calls, _ := e.shell.snapshot(); len(calls) != 0 {
		t.Errorf("an invalid pending server did %v", calls)
	}
	if !strings.Contains(e.logs.String(), "pending server is not a server address") {
		t.Errorf("no log line:\n%s", e.logs.String())
	}
}

// TestKnownServerNotLinked: a link to a listed server that isn't linked makes it active and reloads the window once
// (its link step shows with that server's bridge.js); the room waits in PendingLink.
func TestKnownServerNotLinked(t *testing.T) {
	e := newEnv(t, []string{"https://a.example.com", "https://b.example.com"}, map[string]bool{"https://a.example.com": true})
	e.app.HandleLink(link(t, "isshoni://b.example.com/r/lounge"))
	if got := e.srv.Active().State().Origin; got != "https://b.example.com" {
		t.Errorf("active = %s", got)
	}
	if n := e.shell.count("reload"); n != 1 {
		t.Errorf("%d reloads, want 1", n)
	}
	if n := e.shell.count("emit " + EventDeepLinkOpen); n != 0 {
		t.Error("the page about to reload got deeplink.open")
	}
	if p := e.app.PendingLink(); p == nil || p.RoomID != "lounge" || p.Add != nil {
		t.Errorf("PendingLink = %+v", p)
	}
}

// TestKnownServerLinked: a linked server that isn't active becomes active (one reload), then the room.
func TestKnownServerLinked(t *testing.T) {
	both := map[string]bool{"https://a.example.com": true, "https://b.example.com": true}
	e := newEnv(t, []string{"https://a.example.com", "https://b.example.com"}, both)
	e.app.HandleLink(link(t, "isshoni://b.example.com/"))
	if e.srv.Active().State().Origin != "https://b.example.com" || e.shell.count("reload") != 1 {
		t.Errorf("active %s, %d reloads", e.srv.Active().State().Origin, e.shell.count("reload"))
	}
	if p := e.app.PendingLink(); p == nil || p.RoomID != "" {
		t.Errorf("PendingLink = %+v", p)
	}
}

// TestActiveServerLink: the active server's link focuses the window and asks the SPA to open the room.
func TestActiveServerLink(t *testing.T) {
	e := newEnv(t, []string{"https://a.example.com"}, map[string]bool{"https://a.example.com": true})
	e.app.HandleLink(link(t, "ISSHONI://A.EXAMPLE.COM:443/r/lounge?code=x#y"))
	if got := lastEvent(t, e.shell); got.RoomID != "lounge" || got.Add != nil || got.Switch != nil {
		t.Errorf("deeplink.open = %+v", got)
	}
	if e.shell.count("reload") != 0 || e.shell.count("show(true)") != 1 {
		t.Errorf("calls %v", e.shell.calls)
	}
	// A {roomId} isn't a question: a newer link replaces it, once the second is over.
	e.app.HandleLink(link(t, "isshoni://a.example.com/r/other"))
	if p := e.app.PendingLink(); p.RoomID != "lounge" {
		t.Error("a second link within a second was handled")
	}
	e.clock.Advance(time.Second)
	e.app.HandleLink(link(t, "isshoni://a.example.com/r/other"))
	if p := e.app.PendingLink(); p.RoomID != "other" {
		t.Errorf("PendingLink = %+v", p)
	}
}

// TestLiveShareAsks: another known server while a share is live asks; a link never stops a share silently.
func TestLiveShareAsks(t *testing.T) {
	e := newEnv(t, []string{"https://a.example.com", "https://b.example.com"}, nil)
	e.app.sharing = func() (string, bool) { return "lounge", true }
	e.app.HandleLink(link(t, "isshoni://b.example.com/r/movies"))
	got := lastEvent(t, e.shell)
	if got.Switch == nil || got.Switch.Origin != "https://b.example.com" || got.Switch.RoomID != "movies" ||
		got.Live == nil || got.Live.RoomID != "lounge" {
		t.Errorf("deeplink.open = %+v", got)
	}
	if e.srv.Active().State().Origin != "https://a.example.com" || e.shell.count("reload") != 0 {
		t.Error("the active server changed without the user")
	}
}

// TestTooManyServers: a 21st server is refused with "You've added 20 servers. Remove one first."
func TestTooManyServers(t *testing.T) {
	var origins []string
	for i := range MaxServers {
		origins = append(origins, fmt.Sprintf("https://s%d.example.com", i))
	}
	e := newEnv(t, origins, nil)
	e.app.HandleLink(link(t, "isshoni://new.example.com/"))
	if got := lastEvent(t, e.shell); got.Error != "too_many_servers" || got.Add != nil {
		t.Errorf("deeplink.open = %+v", got)
	}
	// A listed server still works.
	e.app.LinkHandled()
	e.clock.Advance(time.Second)
	e.app.HandleLink(link(t, "isshoni://s0.example.com/r/x"))
	if got := lastEvent(t, e.shell); got.RoomID != "x" {
		t.Errorf("deeplink.open = %+v", got)
	}
}

// TestHandleArgs: the command line's rules in the app (03 §13.1, §13.3, §22.1).
func TestHandleArgs(t *testing.T) {
	t.Run("--open-url with --purge-data handles the link and never purges", func(t *testing.T) {
		e := newEnv(t, []string{"https://watch.example.com"}, nil)
		e.app.HandleArgs([]string{"--open-url", "isshoni://watch.example.com/r/lounge", "--purge-data"})
		if got := lastEvent(t, e.shell); got.RoomID != "lounge" {
			t.Errorf("deeplink.open = %+v", got)
		}
		if !e.srv.Has("https://watch.example.com") {
			t.Error("data went away")
		}
		if !strings.Contains(e.logs.String(), "--purge-data") {
			t.Error("the ignored flag wasn't logged by name")
		}
	})
	t.Run("a bare positional URL is ignored", func(t *testing.T) {
		e := newEnv(t, nil, nil)
		e.app.HandleArgs([]string{"isshoni://watch.example.com/r/secret-room"})
		if n := e.shell.count("emit " + EventDeepLinkOpen); n != 0 {
			t.Error("a positional URL was handled as a link")
		}
		if strings.Contains(e.logs.String(), "secret-room") || !strings.Contains(e.logs.String(), "(positional)") {
			t.Errorf("log:\n%s", e.logs.String())
		}
	})
	t.Run("one-shot modes are ignored in forwarded arguments", func(t *testing.T) {
		e := newEnv(t, []string{"https://watch.example.com"}, nil)
		for _, mode := range []string{"--purge-data", "--forget-credentials", "--version", "--self-check=x", "-Embedding", "--update-now"} {
			e.app.HandleArgs([]string{mode})
		}
		if !e.srv.Has("https://watch.example.com") || e.shell.count("quit") != 0 {
			t.Error("a forwarded one-shot mode did something")
		}
		if n := e.shell.count("show(true)"); n != 6 {
			t.Errorf("%d window shows, want one per forwarded start", n)
		}
	})
	t.Run("a broken link shows the window and logs the host only", func(t *testing.T) {
		e := newEnv(t, nil, nil)
		e.app.HandleArgs([]string{"--open-url", "isshoni://user:pw@watch.example.com/r/a b"})
		e.app.HandleArgs([]string{"--open-url", "isshoni://watch.example.com/x/secret-path"})
		out := e.logs.String()
		if strings.Contains(out, "secret-path") || strings.Contains(out, "pw@") {
			t.Errorf("the log holds a link's path or userinfo:\n%s", out)
		}
		if !strings.Contains(out, "host=watch.example.com") {
			t.Errorf("the host is missing:\n%s", out)
		}
		if e.shell.count("show(true)") != 2 || e.shell.count("emit "+EventDeepLinkOpen) != 0 {
			t.Errorf("calls %v", e.shell.calls)
		}
	})
	t.Run("--server is a link that carries only a server", func(t *testing.T) {
		e := newEnv(t, nil, nil)
		e.app.HandleArgs([]string{"--server", "https://watch.example.com/invite#t"})
		if got := lastEvent(t, e.shell); got.Add == nil || got.Add.Origin != "https://watch.example.com" || got.Add.RoomID != "" {
			t.Errorf("deeplink.open = %+v", got)
		}
	})
	t.Run("--server http loopback only in dev and e2e builds", func(t *testing.T) {
		e := newEnv(t, nil, nil)
		e.app.HandleArgs([]string{"--server", "http://localhost:5173"})
		if e.shell.count("emit "+EventDeepLinkOpen) != 0 {
			t.Error("a release build accepted an http server")
		}
		e.app.d.InsecureLoopback = true
		e.clock.Advance(time.Second)
		e.app.HandleArgs([]string{"--server", "http://localhost:5173"})
		if got := lastEvent(t, e.shell); got.Add == nil || got.Add.Origin != "http://localhost:5173" || got.Add.Host != "localhost:5173" {
			t.Errorf("deeplink.open = %+v", got)
		}
	})
	t.Run("a link never carries http, even in a dev build", func(t *testing.T) {
		e := newEnv(t, nil, nil)
		e.app.d.InsecureLoopback = true
		e.app.HandleArgs([]string{"--open-url", "isshoni://localhost:5173/"})
		if got := lastEvent(t, e.shell); got.Add == nil || got.Add.Origin != "https://localhost:5173" {
			t.Errorf("deeplink.open = %+v", got)
		}
	})
	t.Run("--autostart opens no window", func(t *testing.T) {
		e := newEnv(t, nil, nil)
		e.app.HandleArgs([]string{"--autostart"})
		if calls, _ := e.shell.snapshot(); len(calls) != 0 {
			t.Errorf("calls %v", calls)
		}
	})
	t.Run("a plain second start shows the window", func(t *testing.T) {
		e := newEnv(t, nil, nil)
		e.app.HandleArgs(nil)
		if calls, _ := e.shell.snapshot(); len(calls) != 1 || calls[0] != "show(true)" {
			t.Errorf("calls %v", calls)
		}
	})
}

// TestLinkStepOpenIgnoresLinks: while the active server shows a code (linking), a link is ignored.
func TestLinkStepOpenIgnoresLinks(t *testing.T) {
	ts := newLinkServer(t)
	e := newEnvWith(t, []string{ts.URL}, nil, &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}, true)
	m := e.srv.Active()
	if err := m.Link(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.CancelLink()
	if m.State().Status != deviceauth.StatusLinking {
		t.Fatalf("status %s", m.State().Status)
	}
	e.app.HandleLink(link(t, "isshoni://other.example.com/"))
	if calls, _ := e.shell.snapshot(); len(calls) != 0 {
		t.Errorf("a link during the link step did %v", calls)
	}
}

// TestPendingLinkJSON: the deeplink.open payload has exactly one shape (03 §6.3).
func TestPendingLinkJSON(t *testing.T) {
	add := &LinkAdd{Origin: "https://a.example.com", Host: "a.example.com", HostUnicode: "a.example.com", RoomID: "r"}
	for _, tc := range []struct {
		p    PendingLink
		want string
	}{
		{PendingLink{}, `{"roomId":""}`},
		{PendingLink{RoomID: "lounge"}, `{"roomId":"lounge"}`},
		{PendingLink{RoomID: "ignored", Add: add},
			`{"add":{"origin":"https://a.example.com","host":"a.example.com","hostUnicode":"a.example.com","roomId":"r"}}`},
		{PendingLink{Switch: add, Live: &LiveShare{RoomID: "x"}},
			`{"switch":{"origin":"https://a.example.com","host":"a.example.com","hostUnicode":"a.example.com","roomId":"r"},"live":{"roomId":"x"}}`},
		{PendingLink{Error: "too_many_servers"}, `{"error":"too_many_servers"}`},
	} {
		data, err := json.Marshal(tc.p)
		if err != nil || string(data) != tc.want {
			t.Errorf("%+v → %s (%v), want %s", tc.p, data, err, tc.want)
		}
	}
}

func TestNewRequires(t *testing.T) {
	st, err := settings.Open(settings.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	srv, err := deviceauth.OpenServers(deviceauth.ServersOptions{Dir: t.TempDir(), Store: credstore.NewMemory()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	for _, d := range []Deps{
		{Servers: srv, Shell: &fakeShell{}, OS: fakeOS{}},
		{Settings: st, Shell: &fakeShell{}, OS: fakeOS{}},
		{Settings: st, Servers: srv, OS: fakeOS{}},
		{Settings: st, Servers: srv, Shell: &fakeShell{}},
	} {
		if _, err := New(d); err == nil {
			t.Errorf("New(%+v) succeeded", d)
		}
	}
}

// fakePart is a Session, AudioService or Updater that runs until ctx ends.
type fakePart struct {
	ran, closed atomic.Bool
}

func (f *fakePart) Run(ctx context.Context) error {
	f.ran.Store(true)
	<-ctx.Done()
	return nil
}

func (f *fakePart) Close() error {
	f.closed.Store(true)
	return nil
}

// TestRunAndQuit: Run starts the parts and the engine's probe and returns after Quit, which closes the audio
// service, flushes the settings and quits the shell.
func TestRunAndQuit(t *testing.T) {
	e := newEnv(t, []string{"https://a.example.com"}, nil)
	session, audio, updater := &fakePart{}, &fakePart{}, &fakePart{}
	e.app.d.Session, e.app.d.Audio, e.app.d.Updater = session, audio, updater
	eng := fake.NewEngine(fake.EngineConfig{})
	defer func() { _ = eng.Close() }()
	e.app.d.Engine = eng
	done := make(chan error, 1)
	go func() { done <- e.app.Run(context.Background()) }()
	p, err := e.app.Probe(context.Background())
	if err != nil || len(p.Encoders) == 0 {
		t.Fatalf("Probe = %+v, %v", p, err)
	}
	// Run starts the active server's manager, whose Start asks /info (here a transport that fails and counts).
	for deadline := time.Now().Add(5 * time.Second); e.net.n.Load() == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the active server's manager wasn't started")
		}
	}
	if err := e.app.d.Settings.Update(func(s *settings.Settings) { s.WizardDone = true }); err != nil {
		t.Fatal(err)
	}
	e.app.Quit(true)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(QuitTimeout):
		t.Fatal("Run didn't return after Quit")
	}
	if !session.ran.Load() || !audio.ran.Load() || !updater.ran.Load() || !audio.closed.Load() {
		t.Error("a part didn't run or the audio service wasn't closed")
	}
	if e.shell.count("quit") != 1 {
		t.Error("the shell didn't quit")
	}
	e.app.Quit(false) // once only
	if e.shell.count("quit") != 1 {
		t.Error("Quit ran twice")
	}
	if err := e.app.Run(context.Background()); err == nil {
		t.Error("a second Run succeeded")
	}
	// The flush happened at Quit.
	again, err := settings.Open(settings.Options{Dir: e.dir})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = again.Close() }()
	if !again.WizardDone() {
		t.Error("settings weren't flushed at quit")
	}
}

func TestRunEndsWithCtxAndProbeWithoutEngine(t *testing.T) {
	e := newEnv(t, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.app.Run(ctx) }()
	if _, err := e.app.Probe(context.Background()); !errors.Is(err, media.ErrUnsupportedOS) {
		t.Errorf("Probe without an engine: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// failingPart is a part whose Run fails at once with err.
type failingPart struct{ err error }

func (f failingPart) Run(context.Context) error { return f.err }
func (f failingPart) Close() error              { return f.err }

// TestPartErrorsLoggedByKind: the line for a part that stopped, and Quit's line for an audio service that didn't
// close cleanly, name the error's kind, never its text (03 §17).
func TestPartErrorsLoggedByKind(t *testing.T) {
	e := newEnv(t, nil, nil)
	logs := &lockedWriter{w: &bytes.Buffer{}}
	e.app.log = slog.New(slog.NewTextHandler(logs, nil)).With("component", "app")
	secret := errors.New("GET https://a.example.com/api/v1/device/token: Authorization: Bearer isa_" +
		strings.Repeat("A", 43))
	e.app.d.Session = failingPart{err: secret}
	e.app.d.Audio = failingPart{err: fmt.Errorf("audio: unwatch: %w", &media.Error{Code: media.CodeAudioUnavailable,
		Op: "unwatch_audio", Reason: "blocked", Detail: `C:\Users\alex\AppData\Local\discord.exe`})}
	done := make(chan error, 1)
	go func() { done <- e.app.Run(context.Background()) }()
	// The session's line comes as soon as its Run has returned; the audio service runs too, and fails the same way.
	for deadline := time.Now().Add(5 * time.Second); strings.Count(logs.String(), "a part stopped") < 2; time.Sleep(
		5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no part stopped:\n%s", logs.String())
		}
	}
	e.app.Quit(false)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(QuitTimeout):
		t.Fatal("Run didn't return after Quit")
	}
	out := logs.String()
	for _, want := range []string{
		`msg="app: a part stopped" component=app part=session err=error`,
		`msg="app: a part stopped" component=app part=audio err="unwatch_audio: audio_unavailable (blocked)"`,
		`msg="app: the audio service didn't close cleanly" component=app err="unwatch_audio: audio_unavailable (blocked)"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %s in:\n%s", want, out)
		}
	}
	for _, never := range []string{"isa_", "Bearer", "Authorization", "a.example.com", "alex", "discord"} {
		if strings.Contains(out, never) {
			t.Errorf("%q logged:\n%s", never, out)
		}
	}
}

func TestErrAttr(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{&media.Error{Code: media.CodeDependency, Op: "probe", Reason: "abi", Detail: "isshoni_media.dll 3.1"},
			"probe: dependency (abi)"},
		{fmt.Errorf("session: %w", &media.Error{Code: media.CodeTimeout}), "timeout"},
		{fmt.Errorf("session: %w", context.Canceled), "context canceled"},
		{fmt.Errorf("session: %w", context.DeadlineExceeded), "context deadline exceeded"},
		{errors.New(`Cookie: isshoni_session=s3cr3t`), "error"},
		{fmt.Errorf(`open C:\Users\alex\settings.json: %w`, os.ErrPermission), "error"},
	} {
		if a := errAttr(c.err); a.Key != "err" || a.Value.String() != c.want {
			t.Errorf("errAttr(%q) = %s=%q, want err=%q", c.err, a.Key, a.Value.String(), c.want)
		}
	}
}
