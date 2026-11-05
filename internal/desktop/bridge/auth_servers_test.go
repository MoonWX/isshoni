package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/MoonWX/isshoni/internal/client/deeplink"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/bridge/codes"
)

// controlCore is a Core with the app's server changes (ServerControl) and its auth.state source.
type controlCore struct {
	*fakeCore

	mu        sync.Mutex
	calls     []string
	err       error
	confirmed bool
	sub       func(deviceauth.State)
}

var (
	_ ServerControl = (*controlCore)(nil)
	_ authSource    = (*controlCore)(nil)
)

func (c *controlCore) record(call string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, call)
	return c.err
}

func (c *controlCore) AddServer(_ context.Context, origin string) error {
	return c.record("add " + origin)
}
func (c *controlCore) SwitchServer(origin string) error { return c.record("switch " + origin) }
func (c *controlCore) RemoveServer(_ context.Context, origin string) error {
	return c.record("remove " + origin)
}

func (c *controlCore) Unlink(context.Context) (bool, error) {
	err := c.record("unlink")
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.confirmed, err
}

func (c *controlCore) SubscribeAuth(fn func(deviceauth.State)) func() {
	c.mu.Lock()
	c.sub = fn
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		c.sub = nil
		c.mu.Unlock()
	}
}

func (c *controlCore) send(st deviceauth.State) {
	c.mu.Lock()
	fn := c.sub
	c.mu.Unlock()
	if fn != nil {
		fn(st)
	}
}

// TestServerChanges: AddServer, SwitchServer, RemoveServer and Unlink call the app's ServerControl with the origin as
// given, and map its errors to 03 §6.4's codes; an origin that can't be one is refused before the app is asked.
func TestServerChanges(t *testing.T) {
	var buf bytes.Buffer
	core := &controlCore{fakeCore: &fakeCore{}, confirmed: true}
	s := newTestServices2(t, core, &buf)
	defer s.Close()
	ctx := context.Background()

	if err := s.Auth.AddServer(ctx, originB); err != nil {
		t.Fatal(err)
	}
	if err := s.Auth.SwitchServer(originB); err != nil {
		t.Fatal(err)
	}
	if err := s.Auth.RemoveServer(ctx, originB); err != nil {
		t.Fatal(err)
	}
	res, err := s.Auth.Unlink(ctx)
	if err != nil || !res.Confirmed {
		t.Fatalf("Unlink = %+v, %v", res, err)
	}
	core.mu.Lock()
	core.confirmed = false
	core.mu.Unlock()
	if res, err := s.Auth.Unlink(ctx); err != nil || res.Confirmed {
		t.Errorf("an unconfirmed Unlink = %+v, %v", res, err)
	}
	want := fmt.Sprintf("add %s|switch %s|remove %s|unlink|unlink", originB, originB, originB)
	if got := strings.Join(core.calls, "|"); got != want {
		t.Errorf("calls %s, want %s", got, want)
	}
	// {"confirmed": …} is the JSON the page reads.
	if b, _ := json.Marshal(UnlinkResult{Confirmed: true}); string(b) != `{"confirmed":true}` {
		t.Errorf("UnlinkResult JSON %s", b)
	}

	long := "https://" + strings.Repeat("a", maxOrigin) + ".example.com"
	for name, c := range map[string]struct {
		call func() error
		want codes.Code
	}{
		"AddServer empty":    {func() error { return s.Auth.AddServer(ctx, "") }, codes.CodeInvalidAddress},
		"AddServer long":     {func() error { return s.Auth.AddServer(ctx, long) }, codes.CodeInvalidAddress},
		"SwitchServer empty": {func() error { return s.Auth.SwitchServer("") }, codes.CodeNotFound},
		"RemoveServer long":  {func() error { return s.Auth.RemoveServer(ctx, long) }, codes.CodeNotFound},
	} {
		core.calls = nil
		if got := codeOf(t, c.call()); got != c.want || len(core.calls) != 0 {
			t.Errorf("%s: %s (calls %v), want %s and no call", name, got, core.calls, c.want)
		}
	}

	for _, c := range []struct {
		err  error
		want codes.Code
	}{
		{app.ErrBusy, codes.CodeBusy},
		{app.ErrNotFound, codes.CodeNotFound},
		{fmt.Errorf("%w: %w", app.ErrInvalidAddress, deeplink.ErrInvalidOrigin), codes.CodeInvalidAddress},
		{fmt.Errorf("app: add the server: %w", deviceauth.ErrTooManyServers), codes.CodeTooManyServers},
		{fmt.Errorf("app: add the server: %w", deviceauth.ErrNetwork), codes.CodeServerUnreachable},
		{fmt.Errorf("app: add the server: %w", context.DeadlineExceeded), codes.CodeServerUnreachable},
	} {
		core.mu.Lock()
		core.err = c.err
		core.mu.Unlock()
		for name, call := range map[string]func() error{
			"AddServer":    func() error { return s.Auth.AddServer(ctx, originB) },
			"SwitchServer": func() error { return s.Auth.SwitchServer(originB) },
			"RemoveServer": func() error { return s.Auth.RemoveServer(ctx, originB) },
			"Unlink":       func() error { _, err := s.Auth.Unlink(ctx); return err },
		} {
			if got := codeOf(t, call()); got != c.want {
				t.Errorf("%s with %v: %s, want %s", name, c.err, got, c.want)
			}
		}
	}
}

// TestAuthStateEvent: the app's active-server states go to the page as isshoni:auth.state in the binding's JSON,
// and stop after Close.
func TestAuthStateEvent(t *testing.T) {
	var (
		buf    bytes.Buffer
		mu     sync.Mutex
		events []string
	)
	core := &controlCore{fakeCore: &fakeCore{}}
	s, err := New(Deps{App: core, Servers: &fakeServers{}, Log: testLog(&buf), Emit: func(name string, data any) {
		b, _ := json.Marshal(data)
		mu.Lock()
		events = append(events, name+" "+string(b))
		mu.Unlock()
	}})
	if err != nil {
		t.Fatal(err)
	}
	core.send(deviceauth.State{Origin: originA, ServerName: "Friends", Status: deviceauth.StatusLinking,
		Notice: deviceauth.NoticeDenied})
	s.Close()
	core.send(deviceauth.State{Origin: originA, Status: deviceauth.StatusLinked})
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 {
		t.Fatalf("events %v", events)
	}
	want := EventAuthState + ` {"origin":"` + originA + `","serverName":"Friends","status":"linking","link":null,` +
		`"user":null,"device":null,"notice":"denied","compat":{"verdict":"ok","reason":"","serverVersion":"",` +
		`"minClientVersion":""}}`
	if events[0] != want {
		t.Errorf("event\n%s\nwant\n%s", events[0], want)
	}
}
