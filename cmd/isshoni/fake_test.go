package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rogpeppe/go-internal/testscript"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/ops"
)

// The client commands are tested against a real admin socket, served by ops.AdminServer inside the test process,
// with fake accounts behind it (04 §17). The CLI under test runs in-process (runCLI) or as its own process
// (testscript), and reaches the socket through listen.admin_socket like the real one.

// peerCreds says whether this platform checks peer credentials on the admin socket (ops has them for Linux and
// macOS); the tests of a refused peer need them.
var peerCreds = runtime.GOOS == "linux" || runtime.GOOS == "darwin"

// strangerEnv is a switch of the tests, not of isshoni: with strangerEnv=1 in its environment the CLI under test
// talks to the admin socket as a caller that the server may refuse (dialAsStranger). A fake server that refuses
// its peers sets it (startTestServer, "fakeserver start -reject-peers"). The CLI by itself knows better: the
// tests run it as the user who owns the socket, whom a real server always serves, so it would take a server that
// hangs up for one that is going away. The name is not an ISSHONI_* one: the config would warn about those.
const strangerEnv = "TEST_ADMIN_SOCKET_STRANGER"

// dialAsStranger is the invocation's dialAdmin under strangerEnv.
func dialAsStranger(path string) *ops.AdminClient { return ops.DialAdmin(path).AssumeStranger() }

// newSocketDir returns a fresh directory with a short path: t.TempDir() and testscript's work directory are too
// long for a unix socket path on macOS (sun_path is 104 bytes).
func newSocketDir() (string, error) {
	dir, err := os.MkdirTemp("", "isshoni")
	if err == nil && len(filepath.Join(dir, "admin.sock")) > 100 {
		_ = os.RemoveAll(dir)
		dir, err = os.MkdirTemp("/tmp", "isshoni")
	}
	return dir, err
}

// removeSocketDir removes a directory of newSocketDir, also after a test took its permissions away.
func removeSocketDir(dir string) {
	_ = os.Chmod(dir, 0o700) //nolint:gosec // G302: a directory needs its x bit
	_ = os.RemoveAll(dir)
}

// socketPath returns a socket path for one test, in a directory that is removed when the test ends. Nothing
// listens there until the test starts a fakeServer.
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := newSocketDir()
	if err != nil {
		t.Fatalf("directory for the admin socket: %v", err)
	}
	t.Cleanup(func() { removeSocketDir(dir) })
	return filepath.Join(dir, "admin.sock")
}

// fakeAccounts is 03's account service for the CLI tests (ops.AdminAccounts): no admin at first, so the setup link
// is available; completing the setup adds alex (admin) and sam.
type fakeAccounts struct {
	mu        sync.Mutex
	setupDone bool
	closed    bool // registration is closed
	links     int  // setup links minted
	users     []ops.AdminUser
	invites   []string // what CreateInvite got: "uses=10 ttlHours=168"
}

func (f *fakeAccounts) completeSetup() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setupDone = true
	f.users = []ops.AdminUser{
		{
			ID: "0123456789ab", Username: "alex", Role: api.RoleAdmin, Status: api.UserStatusActive,
			CreatedAt:  api.WireTime(time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)),
			LastSeenAt: api.WireTime(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)),
		},
		{
			ID: "cdefghjkmnpq", Username: "sam", Role: api.RoleUser, Status: api.UserStatusActive,
			CreatedAt: api.WireTime(time.Date(2026, 10, 1, 8, 30, 0, 0, time.UTC)),
		},
	}
}

func (f *fakeAccounts) SetupAvailable(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.setupDone, nil
}

func (f *fakeAccounts) IssueSetupLink(context.Context) (ops.IssuedLink, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setupDone {
		return ops.IssuedLink{}, api.NewError(api.CodeSetupUnavailable)
	}
	f.links++
	return ops.IssuedLink{
		URL:       logx.Secret(fmt.Sprintf("https://watch.example.com/setup#token%d", f.links)),
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}, nil
}

func (f *fakeAccounts) Users(context.Context) ([]ops.AdminUser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ops.AdminUser(nil), f.users...), nil
}

// user returns the user with this name; the caller holds f.mu.
func (f *fakeAccounts) user(name string) (*ops.AdminUser, error) {
	for i := range f.users {
		if f.users[i].Username == name {
			return &f.users[i], nil
		}
	}
	return nil, api.NewError(api.CodeUserNotFound)
}

// lastAdmin reports whether u is the only active admin; the caller holds f.mu.
func (f *fakeAccounts) lastAdmin(u *ops.AdminUser) bool {
	if u.Role != api.RoleAdmin || u.Status != api.UserStatusActive {
		return false
	}
	for i := range f.users {
		if o := &f.users[i]; o != u && o.Role == api.RoleAdmin && o.Status == api.UserStatusActive {
			return false
		}
	}
	return true
}

func (f *fakeAccounts) IssueResetLink(_ context.Context, name string) (ops.IssuedLink, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.user(name); err != nil {
		return ops.IssuedLink{}, err
	}
	return ops.IssuedLink{URL: logx.Secret("https://watch.example.com/reset#reset-" + name), ExpiresAt: time.Now().Add(24 * time.Hour)}, nil
}

func (f *fakeAccounts) SetRole(_ context.Context, name string, role api.Role) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, err := f.user(name)
	switch {
	case err != nil:
		return err
	case role != api.RoleAdmin && f.lastAdmin(u):
		return api.NewError(api.CodeLastAdmin)
	}
	u.Role = role
	return nil
}

func (f *fakeAccounts) SetDisabled(_ context.Context, name string, disabled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, err := f.user(name)
	switch {
	case err != nil:
		return err
	case disabled && f.lastAdmin(u):
		return api.NewError(api.CodeLastAdmin)
	case disabled:
		u.Status = api.UserStatusDisabled
	default:
		u.Status = api.UserStatusActive
	}
	return nil
}

func (f *fakeAccounts) CreateInvite(_ context.Context, maxUses, ttlHours int) (ops.IssuedLink, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return ops.IssuedLink{}, api.NewError(api.CodeRegistrationClosed)
	}
	f.invites = append(f.invites, fmt.Sprintf("uses=%d ttlHours=%d", maxUses, ttlHours))
	if ttlHours == 0 {
		ttlHours = 168 // the invite setting's default
	}
	return ops.IssuedLink{
		URL:       logx.Secret(fmt.Sprintf("https://watch.example.com/invite#invite%d", len(f.invites))),
		ExpiresAt: time.Now().Add(time.Duration(ttlHours) * time.Hour),
	}, nil
}

// fakeServer is a running admin socket with fake accounts.
type fakeServer struct {
	path     string
	health   *ops.Health
	accounts *fakeAccounts
	level    *slog.LevelVar // the "process log level" that log-level changes; info at first
	logLevel *ops.LogLevel
	admin    *ops.AdminServer
	served   chan error
	tlsReady atomic.Bool // the tls readiness check
}

// startFakeServer serves the admin socket at path. With rejectPeers the peer-credential filter turns everyone
// away, as the real one does with a user who is neither root nor the server's.
func startFakeServer(path string, rejectPeers bool) (*fakeServer, error) {
	f := &fakeServer{
		path:     path,
		health:   ops.NewHealth(),
		accounts: &fakeAccounts{},
		level:    new(slog.LevelVar),
		served:   make(chan error, 1),
	}
	f.tlsReady.Store(true)
	f.health.AddCheck("db", func() (bool, string) { return true, "" })
	f.health.AddCheck("tls", func() (bool, string) {
		return f.tlsReady.Load(), "waiting: obtaining certificate for 203.0.113.7"
	})
	log := slog.New(slog.DiscardHandler)
	listen := ops.AdminListenOptions{Path: path, Logger: log}
	if rejectPeers {
		listen.Allow = func(uint32) bool { return false }
	}
	ln, err := ops.ListenAdmin(context.Background(), listen)
	if err != nil {
		return nil, err
	}
	f.logLevel = ops.NewLogLevel(f.level, log)
	f.admin = ops.NewAdminServer(ops.AdminOptions{
		Health: f.health, Accounts: f.accounts, LogLevel: f.logLevel, Logger: log,
		Status: func(context.Context) (api.ServerStatus, error) { return fakeStatus(), nil },
	})
	go func() { f.served <- f.admin.Serve(ln) }()
	return f, nil
}

// stop shuts the server down and waits for it.
func (f *fakeServer) stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := f.admin.Shutdown(ctx)
	f.logLevel.Close()
	return errors.Join(err, <-f.served)
}

// fakeStatus is the status of a small server in ip mode, with every part filled in.
func fakeStatus() api.ServerStatus {
	return api.ServerStatus{
		Version: "0.3.0", StartedAt: time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC), UptimeS: 11520,
		Origin: "https://203.0.113.7",
		TLS: api.TLSInfo{
			Mode: api.TLSModeIP, Names: []string{"203.0.113.7"}, Ready: true,
			NotAfter: time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC),
		},
		PublicIPv4: "203.0.113.7", PublicIPv4Method: "stun", NAT: "none",
		Advertised: []api.AdvertisedAddr{{Proto: "udp", Addr: "203.0.113.7:7882", Via: "udp"}, {Proto: "tcp", Addr: "203.0.113.7:443", Via: "tcp443"}},
		Listeners: []api.ListenerInfo{
			{Key: "listen.https", Network: "tcp", Addr: "[::]:443"},
			{Key: "listen.admin_socket", Network: "unix", Addr: "/run/isshoni/admin.sock"},
		},
		Rooms: 1, Participants: 3, Shares: 2, SchemaVersion: 7,
		Transfer: &api.TransferInfo{Month: "2026-09", EgressBytes: 12_300_000_000, IngressBytes: 400_000_000},
		Update:   &api.UpdateInfo{Latest: "0.3.1", URL: "https://github.com/MoonWX/isshoni/releases/tag/v0.3.1", Security: true},
	}
}

// startTestServer starts a fakeServer for a Go test, stopped when the test ends, and returns it with the
// environment that points the CLI at it. With rejectPeers the environment also makes the CLI a caller that may be
// refused (strangerEnv).
func startTestServer(t *testing.T, rejectPeers bool) (*fakeServer, []string) {
	t.Helper()
	f, err := startFakeServer(socketPath(t), rejectPeers)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.stop(); err != nil {
			t.Errorf("stopping the fake server: %v", err)
		}
	})
	env := []string{"ISSHONI_LISTEN_ADMIN_SOCKET=" + f.path}
	if rejectPeers {
		env = append(env, strangerEnv+"=1")
	}
	return f, env
}

// scriptFake is the fake server of one test script, driven by the script command "fakeserver".
type scriptFake struct {
	path string // the script's listen.admin_socket

	mu     sync.Mutex
	srv    *fakeServer  // nil while no server runs
	deaf   net.Listener // "fakeserver deaf": listens and never accepts; nil otherwise
	timers []*time.Timer
	later  sync.WaitGroup // the "fakeserver after" actions that are running
	closed bool
}

type scriptFakeKey struct{}

// setupScriptFake gives a script its own short socket directory and points the CLI's environment at a socket in
// it. No server runs until the script says "fakeserver start".
func setupScriptFake(env *testscript.Env) error {
	dir, err := newSocketDir()
	if err != nil {
		return err
	}
	sf := &scriptFake{path: filepath.Join(dir, "admin.sock")}
	env.Values[scriptFakeKey{}] = sf
	env.Setenv("ISSHONI_LISTEN_ADMIN_SOCKET", sf.path)
	env.Setenv("SOCKDIR", dir)
	env.Defer(func() {
		sf.close()
		removeSocketDir(dir)
	})
	return nil
}

// close stops what the script left running.
func (sf *scriptFake) close() {
	sf.mu.Lock()
	sf.closed = true
	for _, t := range sf.timers {
		if t.Stop() { // it never runs now, so its Done comes from here
			sf.later.Done()
		}
	}
	sf.mu.Unlock()
	sf.later.Wait()
	sf.mu.Lock()
	defer sf.mu.Unlock()
	if sf.srv != nil {
		_ = sf.srv.stop()
		sf.srv = nil
	}
	if sf.deaf != nil {
		_ = sf.deaf.Close()
		sf.deaf = nil
	}
}

// cmdFakeServer is the script command "fakeserver":
//
//	fakeserver start [-reject-peers]     serve the admin socket at $ISSHONI_LISTEN_ADMIN_SOCKET; with -reject-peers
//	                                     the server turns every peer away, and the script's CLI knows that it
//	                                     may be refused (strangerEnv) until the next start or stop
//	fakeserver deaf                      listen on the socket without ever accepting: a server that has bound
//	                                     its socket and does not serve it. stop makes it go away, which cuts
//	                                     the connections that wait for it; start does too, then serves
//	fakeserver stop
//	fakeserver set admin yes             the setup is done: alex (admin) and sam exist
//	fakeserver set tls ready|pending     the tls readiness check
//	fakeserver set registration open|closed
//	fakeserver set shutting-down         the graceful shutdown has begun
//	fakeserver after DUR start|set …     the same, DUR from now, while the script goes on
//	fakeserver show level|invites|links  print the log level, what invite create sent, or the setup links minted
//	fakeserver wait level NAME           wait until the log level is NAME
func cmdFakeServer(ts *testscript.TestScript, neg bool, args []string) {
	sf, _ := ts.Value(scriptFakeKey{}).(*scriptFake)
	if neg || sf == nil || len(args) == 0 {
		ts.Fatalf("usage: fakeserver start|stop|set|after|show|wait …")
	}
	switch args[0] {
	case "after":
		if len(args) < 3 {
			ts.Fatalf("usage: fakeserver after DUR start|set …")
		}
		d, err := time.ParseDuration(args[1])
		ts.Check(err)
		if slices.Contains(args, "-reject-peers") {
			ts.Fatalf("fakeserver after … start -reject-peers: start it directly, the script's environment changes with it")
		}
		sf.mu.Lock()
		defer sf.mu.Unlock()
		sf.later.Add(1)
		sf.timers = append(sf.timers, time.AfterFunc(d, func() {
			defer sf.later.Done()
			// The script has moved on: an error here shows up as the failure of the command that waits for it.
			_ = sf.do(args[2:], io.Discard)
		}))
	default:
		ts.Check(sf.do(args, ts.Stdout()))
		switch args[0] {
		case "start":
			if slices.Contains(args, "-reject-peers") {
				ts.Setenv(strangerEnv, "1")
			} else {
				ts.Setenv(strangerEnv, "")
			}
		case "stop", "deaf":
			ts.Setenv(strangerEnv, "")
		}
	}
}

// do runs one fakeserver action.
func (sf *scriptFake) do(args []string, stdout io.Writer) error {
	if len(args) == 3 && args[0] == "wait" && args[1] == "level" {
		return sf.waitLevel(args[2])
	}
	sf.mu.Lock()
	defer sf.mu.Unlock()
	if sf.closed {
		return errors.New("fakeserver: the script is over")
	}
	arg := func(i int) string {
		if i < len(args) {
			return args[i]
		}
		return ""
	}
	switch {
	case args[0] == "deaf":
		if sf.srv != nil || sf.deaf != nil || len(args) != 1 {
			return errors.New("fakeserver deaf: takes no arguments, and nothing may be running")
		}
		var lc net.ListenConfig
		ln, err := lc.Listen(context.Background(), "unix", sf.path)
		if err != nil {
			return err
		}
		sf.deaf = ln
		return nil
	case len(args) == 1 && args[0] == "stop" && sf.deaf != nil:
		// Closing the listener cuts the connections that wait for it, and removes the socket file.
		ln := sf.deaf
		sf.deaf = nil
		return ln.Close()
	}
	if args[0] == "start" {
		if sf.srv != nil {
			return errors.New("fakeserver start: already running")
		}
		reject := arg(1) == "-reject-peers"
		if len(args) > 2 || (len(args) == 2 && !reject) {
			return errors.New("usage: fakeserver start [-reject-peers]")
		}
		if sf.deaf != nil { // it goes away, and a server that answers takes its place
			_ = sf.deaf.Close()
			sf.deaf = nil
		}
		srv, err := startFakeServer(sf.path, reject)
		if err != nil {
			return err
		}
		sf.srv = srv
		return nil
	}
	srv := sf.srv
	if srv == nil {
		return fmt.Errorf("fakeserver %s: no server is running (fakeserver start)", args[0])
	}
	switch strings.Join(args, " ") {
	case "stop":
		sf.srv = nil
		return srv.stop()
	case "set admin yes":
		srv.accounts.completeSetup()
	case "set tls ready":
		srv.tlsReady.Store(true)
	case "set tls pending":
		srv.tlsReady.Store(false)
	case "set registration open", "set registration closed":
		srv.accounts.mu.Lock()
		srv.accounts.closed = arg(2) == "closed"
		srv.accounts.mu.Unlock()
	case "set shutting-down":
		srv.health.SetShuttingDown()
	case "show level":
		_, _ = fmt.Fprintln(stdout, strings.ToLower(srv.level.Level().String()))
	case "show invites":
		srv.accounts.mu.Lock()
		defer srv.accounts.mu.Unlock()
		for _, inv := range srv.accounts.invites {
			_, _ = fmt.Fprintln(stdout, inv)
		}
	case "show links":
		srv.accounts.mu.Lock()
		defer srv.accounts.mu.Unlock()
		_, _ = fmt.Fprintln(stdout, srv.accounts.links)
	default:
		return fmt.Errorf("fakeserver: unknown action %q", strings.Join(args, " "))
	}
	return nil
}

// waitLevel waits until the running server's log level is name. It does not hold the lock while it waits, so a
// "fakeserver after" action can run meanwhile.
func (sf *scriptFake) waitLevel(name string) error {
	sf.mu.Lock()
	srv := sf.srv
	sf.mu.Unlock()
	if srv == nil {
		return errors.New("fakeserver wait: no server is running (fakeserver start)")
	}
	deadline := time.Now().Add(10 * time.Second)
	for strings.ToLower(srv.level.Level().String()) != name {
		if time.Now().After(deadline) {
			return fmt.Errorf("fakeserver wait level %s: still %s after 10 s", name, srv.level.Level())
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}
