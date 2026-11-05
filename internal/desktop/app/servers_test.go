package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/client/credstore"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/desktop/settings"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// withPub gives the app a recording C2 hook (Refresh after every change of the active server, 03 §9).
func withPub(e *env) *fakePub {
	p := &fakePub{}
	e.app.pub = p
	return p
}

func (p *fakePub) refreshCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.refreshes
}

// loopbackOnly reaches the tests' httptest servers and fails every other request, so no test depends on the network.
type loopbackOnly struct{}

var loopbackTransport = &http.Transport{DisableKeepAlives: true}

func (loopbackOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Hostname() != "127.0.0.1" {
		return nil, fmt.Errorf("test: no network request to %s", r.URL.Host)
	}
	return loopbackTransport.RoundTrip(r)
}

// TestAddServer: Add makes the probed server active through 02's Servers.Add (its own public origin), reloads the
// window once, starts its manager and has C2 look again; the add question's room waits in PendingLink.
func TestAddServer(t *testing.T) {
	ts := newLinkServer(t)
	e := newEnvWith(t, []string{"https://a.example.com"}, map[string]bool{"https://a.example.com": true},
		&http.Client{Transport: loopbackOnly{}}, true)
	pub := withPub(e)
	e.app.offer(&PendingLink{Add: &LinkAdd{Origin: ts.URL, Host: ts.URL[len("http://"):], RoomID: "lounge"}})
	e.shell.reset()

	if err := e.app.AddServer(context.Background(), ts.URL+"/"); err != nil {
		t.Fatal(err)
	}
	if got := e.srv.Active().State().Origin; got != ts.URL {
		t.Errorf("active %s, want %s", got, ts.URL)
	}
	if n := e.shell.count("reload"); n != 1 {
		t.Errorf("%d reloads, want 1", n)
	}
	if n := e.shell.count("emit " + EventDeepLinkOpen); n != 0 {
		t.Error("the page about to reload got deeplink.open")
	}
	if pub.refreshCount() != 1 {
		t.Errorf("C2 refreshed %d times", pub.refreshCount())
	}
	if p := e.app.PendingLink(); p == nil || p.RoomID != "lounge" || p.Add != nil {
		t.Errorf("PendingLink = %+v, want the add question's room", p)
	}

	// Adding a listed server again only makes it active (one more reload).
	if err := e.app.AddServer(context.Background(), "https://a.example.com"); err == nil {
		t.Error("a server whose /info fails was added")
	}
	if err := e.app.SwitchServer("https://a.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := e.app.AddServer(context.Background(), ts.URL); err != nil {
		t.Fatal(err)
	}
	if n := e.shell.count("reload"); n != 3 || len(e.srv.List()) != 2 {
		t.Errorf("%d reloads, %d servers", n, len(e.srv.List()))
	}
}

// TestAddServerOtherHost: the add question's server calls itself another host (03 §8.2's confirm step). The page
// confirms under the question's origin, Servers.Add adopts the other host, and the question is answered all the
// same: its room waits in PendingLink, and the next link isn't ignored.
func TestAddServerOtherHost(t *testing.T) {
	b := newLinkServer(t)
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/info" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.Info{
			Server:   api.InfoServer{Name: "S", Version: "0.2.0", PublicURL: b.URL},
			Protocol: api.InfoProtocol{Current: protocol.Version, Min: protocol.MinVersion},
			Features: []string{api.InfoFeatureDeviceFlow},
		})
	}))
	t.Cleanup(a.Close)
	e := newEnvWith(t, nil, nil, &http.Client{Transport: loopbackOnly{}}, true)
	e.app.offer(&PendingLink{Add: &LinkAdd{Origin: a.URL, Host: a.URL[len("http://"):], RoomID: "lounge"}})

	if err := e.app.AddServer(context.Background(), a.URL); err != nil {
		t.Fatal(err)
	}
	if got := e.srv.Active().State().Origin; got != b.URL {
		t.Errorf("active %s, want the server's own origin %s", got, b.URL)
	}
	if p := e.app.PendingLink(); p == nil || p.RoomID != "lounge" || p.Add != nil {
		t.Fatalf("PendingLink = %+v, want the add question's room", p)
	}
	e.app.LinkHandled()
	e.clock.Advance(5 * time.Second)
	e.shell.reset()
	e.app.HandleLink(link(t, "isshoni://other.example.com/"))
	if got := lastEvent(t, e.shell); got.Add == nil || got.Add.Origin != "https://other.example.com" {
		t.Errorf("the next link after the answer: %+v", got)
	}
}

// TestAddServerRefusals: a live share, an address that isn't an origin and a 21st server are refused, and nothing
// reloads.
func TestAddServerRefusals(t *testing.T) {
	var origins []string
	for i := range MaxServers {
		origins = append(origins, fmt.Sprintf("https://s%d.example.com", i))
	}
	ts := newLinkServer(t)
	e := newEnvWith(t, origins, nil, &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}, true)
	ctx := context.Background()
	if err := e.app.AddServer(ctx, ts.URL); !errors.Is(err, deviceauth.ErrTooManyServers) {
		t.Errorf("21st server: %v", err)
	}
	for _, bad := range []string{"", "ftp://x.example.com", "http://example.com", "https://a.example.com/r/x?y"} {
		if err := e.app.AddServer(ctx, bad); !errors.Is(err, ErrInvalidAddress) {
			t.Errorf("AddServer(%q) = %v", bad, err)
		}
	}
	e.app.sharing = func() (string, bool) { return "lounge", true }
	if err := e.app.AddServer(ctx, ts.URL); !errors.Is(err, ErrBusy) {
		t.Errorf("while live: %v", err)
	}
	if n := e.shell.count("reload"); n != 0 {
		t.Errorf("%d reloads", n)
	}
}

// TestSwitchServer: a listed server becomes active with one reload and a C2 refresh; the active one, an unknown one
// and a switch during a live share change nothing.
func TestSwitchServer(t *testing.T) {
	e := newEnv(t, []string{"https://a.example.com", "https://b.example.com"}, nil)
	pub := withPub(e)
	if err := e.app.SwitchServer("https://A.example.com:443"); err != nil || e.shell.count("reload") != 0 {
		t.Errorf("switch to the active server: %v, %d reloads", err, e.shell.count("reload"))
	}
	if err := e.app.SwitchServer("https://c.example.com"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown server: %v", err)
	}
	if err := e.app.SwitchServer("not an origin"); !errors.Is(err, ErrNotFound) {
		t.Errorf("not an origin: %v", err)
	}
	e.app.sharing = func() (string, bool) { return "lounge", true }
	if err := e.app.SwitchServer("https://b.example.com"); !errors.Is(err, ErrBusy) {
		t.Errorf("while live: %v", err)
	}
	e.app.sharing = func() (string, bool) { return "", false }
	if err := e.app.SwitchServer("https://b.example.com"); err != nil {
		t.Fatal(err)
	}
	if e.srv.Active().State().Origin != "https://b.example.com" || e.shell.count("reload") != 1 || pub.refreshCount() != 1 {
		t.Errorf("active %s, %d reloads, %d refreshes", e.srv.Active().State().Origin, e.shell.count("reload"),
			pub.refreshCount())
	}
}

// TestRemoveServer: removing another server drops it and its serverPrefs without a reload; removing the active one
// reloads for the next; a live share refuses removing its server only.
func TestRemoveServer(t *testing.T) {
	e := newEnv(t, []string{"https://a.example.com", "https://b.example.com", "https://c.example.com"}, nil)
	pub := withPub(e)
	ctx := context.Background()
	if err := e.app.d.Settings.Update(func(s *settings.Settings) {
		s.ServerPrefs["https://b.example.com"] = settings.ServerPrefs{Preset: "game"}
		s.ServerPrefs["https://c.example.com"] = settings.ServerPrefs{Preset: "game"}
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.app.RemoveServer(ctx, "https://x.example.com"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	e.app.sharing = func() (string, bool) { return "lounge", true }
	if err := e.app.RemoveServer(ctx, "https://a.example.com"); !errors.Is(err, ErrBusy) {
		t.Errorf("the live share's server: %v", err)
	}
	if err := e.app.RemoveServer(ctx, "https://b.example.com"); err != nil {
		t.Fatalf("another server while live: %v", err)
	}
	if e.srv.Has("https://b.example.com") || e.shell.count("reload") != 0 {
		t.Errorf("b still listed or a reload: %v", e.shell.calls)
	}
	if _, ok := e.app.d.Settings.Get().ServerPrefs["https://b.example.com"]; ok {
		t.Error("b's serverPrefs stayed")
	}
	e.app.sharing = func() (string, bool) { return "", false }
	if err := e.app.RemoveServer(ctx, "https://a.example.com"); err != nil {
		t.Fatal(err)
	}
	if got := e.srv.Active().State().Origin; got != "https://c.example.com" || e.shell.count("reload") != 1 ||
		pub.refreshCount() != 1 {
		t.Errorf("active %s, %d reloads, %d refreshes", got, e.shell.count("reload"), pub.refreshCount())
	}
	if _, ok := e.app.d.Settings.Get().ServerPrefs["https://c.example.com"]; !ok {
		t.Error("c's serverPrefs went too")
	}
	if err := e.app.RemoveServer(ctx, "https://c.example.com"); err != nil {
		t.Fatal(err)
	}
	if e.srv.Active() != nil || e.shell.count("reload") != 2 {
		t.Errorf("after the last server: %v, %d reloads", e.srv.Active(), e.shell.count("reload"))
	}
}

// TestUnlink: Sign out without a server or a credential is confirmed; with a credential whose revoke the server
// refuses it is unconfirmed, and the credential is gone either way. The server stays listed and active, without a
// reload.
func TestUnlink(t *testing.T) {
	ctx := context.Background()
	empty := newEnv(t, nil, nil)
	if ok, err := empty.app.Unlink(ctx); !ok || err != nil {
		t.Errorf("no server: %v %v", ok, err)
	}

	ts := newLinkServer(t)
	store := credstore.NewMemory()
	if err := store.Put(ts.URL, credstore.Credential{RefreshToken: "isr_" + strings.Repeat("A", 43),
		DeviceID: "d_1", SavedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	e := newEnvStore(t, []string{ts.URL}, map[string]bool{ts.URL: true},
		&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}, true, store)
	ok, err := e.app.Unlink(ctx)
	if err != nil || ok {
		t.Errorf("revoke refused: confirmed %v, %v", ok, err)
	}
	if _, err := e.store.Get(ts.URL); !errors.Is(err, credstore.ErrNotFound) {
		t.Errorf("the credential stayed: %v", err)
	}
	if !e.srv.Has(ts.URL) || e.srv.Active().State().Status != deviceauth.StatusUnlinked || e.shell.count("reload") != 0 {
		t.Errorf("after Sign out: listed %v, %s, %d reloads", e.srv.Has(ts.URL), e.srv.Active().State().Status,
			e.shell.count("reload"))
	}
	if ok, err := e.app.Unlink(ctx); !ok || err != nil {
		t.Errorf("again, without a credential: %v %v", ok, err)
	}
}

// TestAuthStateSubscribers: only the active server's states reach SubscribeAuth, in order, until cancel.
func TestAuthStateSubscribers(t *testing.T) {
	e := newEnv(t, []string{"https://a.example.com", "https://b.example.com"}, nil)
	var (
		mu  sync.Mutex
		got []string
	)
	cancel := e.app.SubscribeAuth(func(s deviceauth.State) {
		mu.Lock()
		got = append(got, s.Origin+" "+string(s.Status))
		mu.Unlock()
	})
	e.app.OnAuthState(deviceauth.State{Origin: "https://a.example.com", Status: deviceauth.StatusLinking})
	e.app.OnAuthState(deviceauth.State{Origin: "https://b.example.com", Status: deviceauth.StatusLinked})
	e.app.OnAuthState(deviceauth.State{Origin: "https://a.example.com", Status: deviceauth.StatusLinked})
	cancel()
	e.app.OnAuthState(deviceauth.State{Origin: "https://a.example.com", Status: deviceauth.StatusUnlinked})
	mu.Lock()
	defer mu.Unlock()
	want := []string{"https://a.example.com linking", "https://a.example.com linked"}
	if !slices.Equal(got, want) {
		t.Errorf("states %v, want %v", got, want)
	}
}

// TestLinkPageOpened: a new code of the active server opens its approval page once (03 §8.2); the same code again,
// another server's code and a state without a code open nothing.
func TestLinkPageOpened(t *testing.T) {
	e := newEnv(t, []string{"https://a.example.com", "https://b.example.com"}, nil)
	code := func(origin, c string) deviceauth.State {
		return deviceauth.State{Origin: origin, Status: deviceauth.StatusLinking, Link: &deviceauth.LinkState{
			UserCode: c, VerificationURI: origin + "/link", VerificationURIComplete: origin + "/link?code=" + c}}
	}
	e.app.OnAuthState(code("https://a.example.com", "WDJB-MJHT"))
	e.app.OnAuthState(code("https://a.example.com", "WDJB-MJHT"))
	e.app.OnAuthState(code("https://b.example.com", "BBBB-CCCC"))
	e.app.OnAuthState(deviceauth.State{Origin: "https://a.example.com", Status: deviceauth.StatusLinking})
	e.app.OnAuthState(code("https://a.example.com", "XKCD-QRST"))
	want := []string{"open https://a.example.com/link?code=WDJB-MJHT", "open https://a.example.com/link?code=XKCD-QRST"}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var got []string
		calls, _ := e.shell.snapshot()
		for _, c := range calls {
			if strings.HasPrefix(c, "open ") {
				got = append(got, c)
			}
		}
		slices.Sort(got)
		if slices.Equal(got, want) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("opened %v, want %v", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
