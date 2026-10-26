//go:build (windows || (darwin && cgo)) && !isshoni_hw

package main

// The bound services as the pinned Wails binds them (03 §3.2 W11, W12, W15), without a window: application.New
// creates no native app (Run does), so Wails' own Bindings and BoundMethod.Call run here on every OS that builds the
// app. The page's half of W12 (the runtime rejects with the JSON as the message) is WailsBridge.test.ts.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/desktop/app"
	"github.com/MoonWX/isshoni/internal/desktop/bridge"
	"github.com/MoonWX/isshoni/internal/media"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// bindingsDir holds the generated TypeScript of the bridge package (task gen:bindings).
var bindingsDir = filepath.Join("..", "..", "web", "src", "platform", "desktop", "bindings", "github.com",
	"MoonWX", "isshoni", "internal", "desktop", "bridge")

const bridgePkg = "github.com/MoonWX/isshoni/internal/desktop/bridge"

var wailsOnce sync.Once

// testWailsApp is the process's one Wails application (Wails keeps it in a global), created once without running.
func testWailsApp() *application.App {
	var a *application.App
	wailsOnce.Do(func() {
		a = application.New(application.Options{Name: "isshoni-bindings-test", Logger: slog.New(slog.DiscardHandler)})
	})
	if a == nil {
		a = application.Get()
	}
	return a
}

// testServices are the real bridge services over test fakes, bound as Wails binds them.
func testServices(t *testing.T, m *testManager) (*bridge.Services, *application.Bindings) {
	t.Helper()
	testWailsApp()
	svcs, err := bridge.New(bridge.Deps{App: testCore{}, Servers: testServers{m}, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	b := application.NewBindings(nil, nil)
	for _, s := range []application.Service{
		application.NewService(svcs.App), application.NewService(svcs.Auth), application.NewService(svcs.Share),
		application.NewService(svcs.Audio), application.NewService(svcs.Settings),
		application.NewService(svcs.Window), application.NewService(svcs.Updates),
	} {
		if err := b.Add(s); err != nil {
			t.Fatal(err)
		}
	}
	return svcs, b
}

// tsBinding matches one generated function and its method id.
var tsBinding = regexp.MustCompile(`export function (\w+)\([^)]*\)[^{]*\{\s*return \$Call\.ByID\((\d+)`)

// TestGeneratedBindingsMatchTheServices (W15's other half): every function of the committed bindings calls an id
// that Wails gives the same method of the same service, and every bound method has one. A binding out of step with
// the Go side fails here even without the wails3 CLI (task gen:bindings:check is the byte-for-byte drift check).
func TestGeneratedBindingsMatchTheServices(t *testing.T) {
	svcs, b := testServices(t, &testManager{origin: "https://a.example.com"})
	files, err := filepath.Glob(filepath.Join(bindingsDir, "*.ts"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no generated bindings in %s (run task gen:bindings): %v", bindingsDir, err)
	}
	found := 0
	for _, f := range files {
		base := strings.TrimSuffix(filepath.Base(f), ".ts")
		if base == "index" || base == "models" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range tsBinding.FindAllStringSubmatch(string(src), -1) {
			id, err := strconv.ParseUint(m[2], 10, 32)
			if err != nil {
				t.Fatal(err)
			}
			bm := b.GetByID(uint32(id))
			if bm == nil {
				t.Errorf("%s.%s calls id %d, which no bound method has", base, m[1], id)
				continue
			}
			if !strings.EqualFold(bm.FQN, bridgePkg+"."+base+"."+m[1]) {
				t.Errorf("%s.%s calls id %d, which is %s", base, m[1], id, bm.FQN)
			}
			found++
		}
	}
	bound := 0
	v := reflect.ValueOf(svcs).Elem()
	for i := range v.NumField() {
		f := v.Type().Field(i)
		if !f.IsExported() {
			continue
		}
		for j := range f.Type.NumMethod() {
			name := bridgePkg + "." + f.Name + "." + f.Type.Method(j).Name
			if b.Get(&application.CallOptions{MethodName: name}) == nil {
				t.Errorf("%s isn't bound", name)
			}
			bound++
		}
	}
	if found != bound {
		t.Fatalf("the bindings have %d functions, the services %d methods", found, bound)
	}
}

// TestBindingErrorIsVerbatim is W12: a bound method's *bridge.Error reaches the page as a RuntimeError whose
// message is the error's JSON exactly, and whose cause is the same object.
func TestBindingErrorIsVerbatim(t *testing.T) {
	_, b := testServices(t, &testManager{origin: "https://a.example.com"})
	m := b.Get(&application.CallOptions{MethodName: bridgePkg + ".Auth.SessionEnded"})
	if m == nil {
		t.Fatal("Auth.SessionEnded isn't bound")
	}
	_, err := m.Call(context.Background(), []json.RawMessage{json.RawMessage(`"https://b.example.com"`),
		json.RawMessage(`"session_revoked"`)})
	var cerr *application.CallError
	if !errors.As(err, &cerr) {
		t.Fatalf("%T %v is not a CallError", err, err)
	}
	const want = `{"code":"not_found","params":{}}`
	if cerr.Message != want || cerr.Kind != application.RuntimeError {
		t.Fatalf("message %q kind %q; want %q, RuntimeError", cerr.Message, cerr.Kind, want)
	}
	// What the HTTP transport answers (transport_http.go's httpError), which runtime.js turns into
	// new RuntimeError(json.message) with json.cause.
	wire, err := json.Marshal(cerr)
	if err != nil {
		t.Fatal(err)
	}
	var reply struct {
		Message string          `json:"message"`
		Cause   json.RawMessage `json:"cause"`
		Kind    string          `json:"kind"`
	}
	if err := json.Unmarshal(wire, &reply); err != nil || reply.Message != want || string(reply.Cause) != want ||
		reply.Kind != "RuntimeError" {
		t.Fatalf("the transport's reply %s", wire)
	}
}

// TestBindingContextIsTheCalls is W11: a method that takes a context gets the call's, and returns when it is
// cancelled (Wails cancels it when the page cancels the call, messageprocessor_call.go).
func TestBindingContextIsTheCalls(t *testing.T) {
	m := &testManager{origin: "https://a.example.com", block: make(chan error, 1)}
	_, b := testServices(t, m)
	link := b.Get(&application.CallOptions{MethodName: bridgePkg + ".Auth.Link"})
	if link == nil {
		t.Fatal("Auth.Link isn't bound")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := link.Call(ctx, nil)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		var cerr *application.CallError
		if !errors.As(err, &cerr) || !strings.Contains(cerr.Message, `"reason":"cancelled"`) {
			t.Fatalf("the cancelled call answered %v", err)
		}
		if seen := <-m.block; !errors.Is(seen, context.Canceled) {
			t.Fatalf("Link's context ended with %v, not the call's cancellation", seen)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the bound method didn't return after the call's context was cancelled")
	}
}

// TestRegisterServices registers every service with the application, its events through the shell's Emit.
func TestRegisterServices(t *testing.T) {
	a := testWailsApp()
	before := len(a.Config().Services)
	s := &wailsShell{app: a}
	svcs, err := registerServices(s, bridge.Deps{App: testCore{}, Servers: testServers{},
		Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	defer svcs.Close()
	if got := len(a.Config().Services) - before; got != 7 {
		t.Fatalf("%d services registered, want 7", got)
	}
	if _, err := registerServices(s, bridge.Deps{}); err == nil {
		t.Fatal("registerServices without the app worked")
	}
}

// TestShellOpenURL: the shell's own check refuses what Wails' Browser.OpenURL would let through, whoever calls it
// (03 §22.1's bridge row), and opens nothing then.
func TestShellOpenURL(t *testing.T) {
	var opened []string
	s := &wailsShell{openURL: func(u string) error { opened = append(opened, u); return nil }}
	for _, u := range []string{"file:///C:/Windows/win.ini", "ms-settings:display", "search-ms:query=x",
		"javascript:alert(1)", `\\host\share`, "//host/share", "isshoni://watch.example.com/", "", "https://"} {
		if err := s.OpenURL(u); !errors.Is(err, app.ErrInvalidArgument) {
			t.Errorf("OpenURL(%q) = %v, want invalid argument", u, err)
		}
	}
	if len(opened) != 0 {
		t.Fatalf("opened %v", opened)
	}
	if err := s.OpenURL("https://example.com/download"); err != nil || len(opened) != 1 {
		t.Fatalf("https: %v %v", err, opened)
	}
	err := s.OpenURL("http://localhost:5173/")
	if insecureLoopback() != (err == nil) {
		t.Fatalf("http loopback in a build with insecureLoopback %v: %v", insecureLoopback(), err)
	}
	s.openURL = func(string) error { return errors.New("no browser") }
	if err := s.OpenURL("https://example.com/"); err == nil || errors.Is(err, app.ErrInvalidArgument) {
		t.Fatalf("a failing browser: %v", err)
	}
}

// testCore is the app as the bindings tests need it: nothing is live.
type testCore struct{}

func (testCore) Info() app.Info                             { return app.Info{} }
func (testCore) Heartbeat()                                 {}
func (testCore) OpenExternal(string) error                  { return nil }
func (testCore) OpenOSSettings(app.OSPage) error            { return nil }
func (testCore) Live() (string, bool)                       { return "", false }
func (testCore) Quit(bool)                                  {}
func (testCore) Log(string, string, map[string]string)      {}
func (testCore) OpenLogsFolder() error                      { return nil }
func (testCore) OpenNotices() error                         { return nil }
func (testCore) PendingLink() *app.PendingLink              { return nil }
func (testCore) LinkHandled()                               {}
func (testCore) OwnConnections([]string) error              { return nil }
func (testCore) Probe(context.Context) (media.Probe, error) { return media.Probe{}, nil }
func (s testServers) List() []deviceauth.ServerEntry        { return nil }
func (m *testManager) Origin() string                       { return m.origin }
func (m *testManager) State() deviceauth.State              { return deviceauth.State{Origin: m.origin} }
func (m *testManager) CancelLink()                          {}
func (m *testManager) HelloRefused(*protocol.Error)         {}
func (m *testManager) Unlink(context.Context) error         { return nil }
func (m *testManager) Recheck(context.Context) (deviceauth.State, error) {
	return m.State(), nil
}

// testServers has at most one server, active.
type testServers struct{ m *testManager }

func (s testServers) Active() bridge.Manager {
	if s.m == nil {
		return nil
	}
	return s.m
}

// testManager is one server's manager; with block, Link waits for its context and reports how it ended.
type testManager struct {
	origin string
	block  chan error
}

func (m *testManager) Link(ctx context.Context) error {
	if m.block == nil {
		return nil
	}
	<-ctx.Done()
	m.block <- ctx.Err()
	return ctx.Err()
}

func (m *testManager) LinkWithPassword(context.Context, string, protocol.Secret) error { return nil }

func (m *testManager) AccessTokenWithExpiry(context.Context, bool) (protocol.Secret, time.Time, error) {
	return "", time.Time{}, deviceauth.ErrNotLinked
}

func (m *testManager) SessionEnded(context.Context, protocol.ErrorCode) deviceauth.State {
	return m.State()
}
