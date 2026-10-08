package ops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// socketPath returns a path for a test's admin socket in a fresh directory with a short name: t.TempDir() is too
// long for a unix socket path on macOS (sun_path is 104 bytes, 04 §17).
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "isshoni")
	if err == nil && len(filepath.Join(dir, "admin.sock")) > 100 {
		_ = os.RemoveAll(dir)
		dir, err = os.MkdirTemp("/tmp", "isshoni")
	}
	if err != nil {
		t.Fatalf("directory for the admin socket: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "admin.sock")
}

// syncBuffer collects log lines from the server's goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// testLogger returns a JSON logger at debug level and its output.
func testLogger() (*slog.Logger, *syncBuffer) {
	out := &syncBuffer{}
	level := new(slog.LevelVar)
	level.Set(slog.LevelDebug)
	return logx.New(logx.Options{Level: level, Format: "json", Out: out}), out
}

// fakeAccounts is the AdminAccounts of the tests: 03's service in a few fields.
type fakeAccounts struct {
	mu        sync.Mutex
	setupDone bool
	issued    int // setup links minted
	users     []AdminUser
	admins    map[string]bool
	closed    bool  // registration is closed
	fail      error // returned by every call when set
	invites   [][2]int
	disabled  map[string]bool
	now       time.Time
}

func newFakeAccounts() *fakeAccounts {
	return &fakeAccounts{
		now:      time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC),
		admins:   map[string]bool{},
		disabled: map[string]bool{},
	}
}

// with runs fn with the fake's state locked: tests change it while the server reads it.
func (f *fakeAccounts) with(fn func(*fakeAccounts)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeAccounts) isAdmin(name string) (is bool) {
	f.with(func(f *fakeAccounts) { is = f.admins[name] })
	return is
}

func (f *fakeAccounts) isDisabled(name string) (is bool) {
	f.with(func(f *fakeAccounts) { is = f.disabled[name] })
	return is
}

func (f *fakeAccounts) invitesSeen() (seen [][2]int) {
	f.with(func(f *fakeAccounts) { seen = append(seen, f.invites...) })
	return seen
}

func (f *fakeAccounts) SetupAvailable(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.setupDone, f.fail
}

func (f *fakeAccounts) IssueSetupLink(context.Context) (IssuedLink, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.fail != nil:
		return IssuedLink{}, f.fail
	case f.setupDone:
		return IssuedLink{}, api.NewError(api.CodeSetupUnavailable)
	}
	f.issued++
	return IssuedLink{
		URL:       logx.Secret(fmt.Sprintf("https://watch.example.com/setup#token%d", f.issued)),
		ExpiresAt: f.now.Add(24 * time.Hour),
	}, nil
}

func (f *fakeAccounts) Users(context.Context) ([]AdminUser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.users, f.fail
}

// known reports whether a user with this name exists; the caller holds f.mu.
func (f *fakeAccounts) known(name string) bool {
	for _, u := range f.users {
		if u.Username == name {
			return true
		}
	}
	return false
}

func (f *fakeAccounts) IssueResetLink(_ context.Context, name string) (IssuedLink, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.known(name) {
		return IssuedLink{}, api.NewError(api.CodeUserNotFound)
	}
	return IssuedLink{URL: "https://watch.example.com/reset#reset-" + logx.Secret(name), ExpiresAt: f.now.Add(24 * time.Hour)}, nil
}

func (f *fakeAccounts) SetRole(_ context.Context, name string, role api.Role) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case !f.known(name):
		return api.NewError(api.CodeUserNotFound)
	case role == api.RoleUser && f.admins[name] && len(f.admins) == 1:
		return api.NewError(api.CodeLastAdmin)
	case role == api.RoleAdmin:
		f.admins[name] = true
	default:
		delete(f.admins, name)
	}
	return nil
}

func (f *fakeAccounts) SetDisabled(_ context.Context, name string, disabled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case !f.known(name):
		return api.NewError(api.CodeUserNotFound)
	case disabled && f.admins[name] && len(f.admins) == 1:
		return api.NewError(api.CodeLastAdmin)
	}
	f.disabled[name] = disabled
	return nil
}

func (f *fakeAccounts) CreateInvite(_ context.Context, maxUses, ttlHours int) (IssuedLink, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return IssuedLink{}, api.NewError(api.CodeRegistrationClosed)
	}
	f.invites = append(f.invites, [2]int{maxUses, ttlHours})
	if ttlHours == 0 {
		ttlHours = 168 // the setting's default
	}
	return IssuedLink{URL: "https://watch.example.com/invite#inv", ExpiresAt: f.now.Add(time.Duration(ttlHours) * time.Hour)}, nil
}

// testAdmin is a running admin server of a test.
type testAdmin struct {
	path     string
	srv      *AdminServer
	client   *AdminClient
	health   *Health
	accounts *fakeAccounts
	level    *slog.LevelVar
	logs     *syncBuffer
	served   chan error
}

// startAdmin serves an admin socket with fake accounts until the test ends. adjust may change the options.
func startAdmin(t *testing.T, adjust func(*AdminOptions, *AdminListenOptions)) *testAdmin {
	t.Helper()
	log, logs := testLogger()
	a := &testAdmin{
		path:     socketPath(t),
		health:   NewHealth(),
		accounts: newFakeAccounts(),
		level:    new(slog.LevelVar),
		logs:     logs,
		served:   make(chan error, 1),
	}
	logLevel := NewLogLevel(a.level, log)
	t.Cleanup(logLevel.Close)
	opts := AdminOptions{Health: a.health, Accounts: a.accounts, LogLevel: logLevel, Logger: log}
	listen := AdminListenOptions{Path: a.path, Logger: log}
	if adjust != nil {
		adjust(&opts, &listen)
	}
	ln, err := ListenAdmin(t.Context(), listen)
	if err != nil {
		t.Fatalf("ListenAdmin: %v", err)
	}
	a.srv = NewAdminServer(opts)
	go func() { a.served <- a.srv.Serve(ln) }()
	a.client = DialAdmin(a.path)
	t.Cleanup(func() { a.stop(t) })
	return a
}

// stop shuts the server down and checks that Serve returned nil. It may run more than once.
func (a *testAdmin) stop(t *testing.T) {
	t.Helper()
	if a.served == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.srv.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	if err := <-a.served; err != nil {
		t.Errorf("Serve returned %v, want nil after Shutdown", err)
	}
	a.served = nil
}

// The socket file is a socket with mode 0600 (04 §12.1); closing the listener removes it.
func TestListenAdminSocketFile(t *testing.T) {
	path := socketPath(t)
	ln, err := ListenAdmin(t.Context(), AdminListenOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&fs.ModeSocket == 0 {
		t.Errorf("%s has mode %v, want a socket", path, fi.Mode())
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode %#o, want 0600", fi.Mode().Perm())
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("after Close the socket file is still there (Lstat: %v)", err)
	}
}

func TestListenAdminPathInUse(t *testing.T) {
	t.Run("stale socket file is replaced", func(t *testing.T) {
		path := socketPath(t)
		// A socket file that nobody listens on, as a killed server leaves it.
		stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		stale.SetUnlinkOnClose(false)
		if err := stale.Close(); err != nil {
			t.Fatal(err)
		}
		ln, err := ListenAdmin(t.Context(), AdminListenOptions{Path: path})
		if err != nil {
			t.Fatalf("ListenAdmin over a stale socket: %v", err)
		}
		_ = ln.Close()
	})
	t.Run("a live server is not replaced", func(t *testing.T) {
		a := startAdmin(t, nil)
		_, err := ListenAdmin(t.Context(), AdminListenOptions{Path: a.path})
		if err == nil || !strings.Contains(err.Error(), "another process is listening on the admin socket "+a.path) {
			t.Fatalf("ListenAdmin on a socket in use: %v", err)
		}
		if _, err := a.client.Health(t.Context()); err != nil {
			t.Errorf("the first server no longer answers: %v", err)
		}
	})
	t.Run("a file that is not a socket stays", func(t *testing.T) {
		path := socketPath(t)
		if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := ListenAdmin(t.Context(), AdminListenOptions{Path: path})
		if err == nil || !strings.Contains(err.Error(), "exists and is not a socket") {
			t.Fatalf("ListenAdmin on a regular file: %v", err)
		}
		if data, _ := os.ReadFile(path); string(data) != "keep me" {
			t.Errorf("the file changed: %q", data)
		}
	})
	t.Run("missing directory", func(t *testing.T) {
		path := filepath.Join(filepath.Dir(socketPath(t)), "missing", "admin.sock")
		if _, err := ListenAdmin(t.Context(), AdminListenOptions{Path: path}); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("ListenAdmin in a missing directory: %v", err)
		}
	})
	t.Run("empty path", func(t *testing.T) {
		if _, err := ListenAdmin(t.Context(), AdminListenOptions{}); err == nil {
			t.Fatal("ListenAdmin with an empty path succeeded")
		}
	})
}

// The kernel reports this process's own uid for a connection it made itself: SO_PEERCRED on Linux,
// LOCAL_PEERCRED on macOS (04 §12.1).
func TestPeerUID(t *testing.T) {
	if !peerCredSupported {
		t.Skipf("no peer credentials on %s", runtime.GOOS)
	}
	path := socketPath(t)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	client := dialUnix(t, path)
	defer func() { _ = client.Close() }()
	server, err := ln.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()

	uid, err := peerUID(server)
	if err != nil {
		t.Fatalf("peerUID: %v", err)
	}
	if int(uid) != os.Geteuid() {
		t.Errorf("peer uid %d, want this process's euid %d", uid, os.Geteuid())
	}
	// It works in the other direction too: the dialing side sees the listener's uid.
	uid, err = peerUID(client.(*net.UnixConn))
	if err != nil || int(uid) != os.Geteuid() {
		t.Errorf("peer uid of the listener = %d, %v; want %d", uid, err, os.Geteuid())
	}
}

// Only uid 0 and the server's own uid are served (04 §12.1).
func TestDefaultPeerRule(t *testing.T) {
	rule := defaultPeerRule(998)
	for uid, want := range map[uint32]bool{0: true, 998: true, 997: false, 1000: false, 65534: false, 4294967295: false} {
		if got := rule(uid); got != want {
			t.Errorf("server uid 998: peer uid %d admitted = %v, want %v", uid, got, want)
		}
	}
	if rule := defaultPeerRule(0); !rule(0) || rule(1) {
		t.Error("a server running as root admits only root")
	}
	// Windows has no uids (os.Geteuid is -1): no peer matches "own uid".
	if rule := defaultPeerRule(-1); rule(4294967295) {
		t.Error("server uid -1 admits uid 4294967295")
	}
}

// Without the peer check the log says what still guards the socket: its file mode, or on Windows nothing (the mode
// means nothing there, so the message must not promise it).
func TestNoPeerCheckWarning(t *testing.T) {
	windows := noPeerCheckWarning("windows")
	if !strings.Contains(windows, "every local user who can open the socket can administer this server") ||
		strings.Contains(windows, "only guard") {
		t.Errorf("the Windows warning: %q", windows)
	}
	for _, goos := range []string{"freebsd", "openbsd", "netbsd"} {
		if got := noPeerCheckWarning(goos); !strings.Contains(got, "its file mode (0600) is the only guard") {
			t.Errorf("the %s warning: %q", goos, got)
		}
	}
	// ListenAdmin writes it exactly when this platform has no check.
	log, logs := testLogger()
	ln, err := ListenAdmin(t.Context(), AdminListenOptions{Path: socketPath(t), Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	if warned := strings.Contains(logs.String(), "can't check who connects to the admin socket"); warned == peerCredSupported {
		t.Errorf("warned = %v on a platform where the peer check is %v:\n%s", warned, peerCredSupported, logs)
	}
}

// The filter with injected credentials: the listener serves root and its own uid, and hangs up on everyone else
// before the first response, which the client reports as ErrAdminPermission. The client is told that it may be
// refused (AssumeStranger): by itself it knows that this process owns the socket file.
func TestPeerCredFilter(t *testing.T) {
	if !peerCredSupported {
		t.Skipf("no peer credentials on %s", runtime.GOOS)
	}
	self := uint32(os.Geteuid()) //nolint:gosec // G115: a uid
	tests := []struct {
		name    string
		uid     uint32
		credErr error
		allow   func(uint32) bool // nil = the default rule
		want    bool
	}{
		{name: "own uid", uid: self, want: true},
		{name: "root", uid: 0, want: true},
		{name: "another user", uid: self + 1000, want: false},
		{name: "nobody", uid: 65534, want: self == 65534},
		{name: "credentials unreadable", uid: self, credErr: errors.New("getsockopt: bad file descriptor"), want: false},
		{name: "custom rule refuses the own uid", uid: self, allow: func(uint32) bool { return false }, want: false},
		{name: "custom rule admits a stranger", uid: 4242, allow: func(uid uint32) bool { return uid == 4242 }, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log, logs := testLogger()
			path := socketPath(t)
			ln, err := ListenAdmin(t.Context(), AdminListenOptions{Path: path, Allow: tt.allow, Logger: log})
			if err != nil {
				t.Fatal(err)
			}
			ln.(*adminListener).peerUID = func(*net.UnixConn) (uint32, error) { return tt.uid, tt.credErr }
			srv := NewAdminServer(AdminOptions{Health: NewHealth(), Logger: log})
			served := make(chan error, 1)
			go func() { served <- srv.Serve(ln) }()
			defer func() {
				if err := srv.Shutdown(context.Background()); err != nil {
					t.Errorf("Shutdown: %v", err)
				}
				<-served
			}()

			_, err = DialAdmin(path).AssumeStranger().Health(t.Context())
			if tt.want {
				if err != nil {
					t.Fatalf("an admitted peer got %v", err)
				}
				if strings.Contains(logs.String(), "refused a connection") {
					t.Errorf("an admitted peer was logged as refused:\n%s", logs)
				}
				return
			}
			var ue *AdminUnreachableError
			if !errors.Is(err, ErrAdminPermission) || !errors.As(err, &ue) || ue.Path != path {
				t.Fatalf("a refused peer got %v, want ErrAdminPermission for %s", err, path)
			}
			if got := err.Error(); got != "permission denied on "+path {
				t.Errorf("error text %q", got)
			}
			waitFor(t, func() bool { return strings.Contains(logs.String(), "refused a connection to the admin socket") })
			if tt.credErr == nil && !strings.Contains(logs.String(), `"peer_uid":`) {
				t.Errorf("the refusal line has no peer_uid:\n%s", logs)
			}
		})
	}
}

// With the real credentials, this process is the server's own user and is served; a rule that refuses it closes
// the connection.
func TestPeerCredReal(t *testing.T) {
	if !peerCredSupported {
		t.Skipf("no peer credentials on %s", runtime.GOOS)
	}
	var seen []uint32
	var mu sync.Mutex
	a := startAdmin(t, func(_ *AdminOptions, l *AdminListenOptions) {
		l.Allow = func(uid uint32) bool {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, uid)
			return len(seen) == 1 // the first connection only
		}
	})
	a.client.AssumeStranger() // the rule refuses the socket's owner, which the real one never does
	if _, err := a.client.Health(t.Context()); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := a.client.Health(t.Context()); !errors.Is(err, ErrAdminPermission) {
		t.Fatalf("second call: %v, want ErrAdminPermission", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, uid := range seen {
		if int(uid) != os.Geteuid() {
			t.Errorf("the rule saw uid %d, want %d", uid, os.Geteuid())
		}
	}
}

// A user who keeps connecting gets one log line per window, with the number of refusals in between.
func TestRefusalLogIsRateLimited(t *testing.T) {
	log, logs := testLogger()
	l := &adminListener{log: log}
	for range 5 {
		l.logRefusal(1234, nil)
	}
	if n := strings.Count(logs.String(), "refused a connection"); n != 1 {
		t.Fatalf("%d lines for 5 refusals in one window, want 1:\n%s", n, logs)
	}
	l.mu.Lock()
	l.lastLine = time.Now().Add(-2 * refusalLogEvery)
	l.mu.Unlock()
	l.logRefusal(1234, nil)
	out := logs.String()
	if strings.Count(out, "refused a connection") != 2 || !strings.Contains(out, `"refused_since_last_line":4`) {
		t.Errorf("the next window's line should count the 4 refusals in between:\n%s", out)
	}
}

// Shutdown closes the listener (the socket file goes away), lets Serve return nil, and does not wait for a
// connection that never sent a request.
func TestAdminServerShutdown(t *testing.T) {
	a := startAdmin(t, nil)
	idle := dialUnix(t, a.path)
	defer func() { _ = idle.Close() }()
	// Let the server see the connection before the shutdown begins.
	if _, err := a.client.Health(t.Context()); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	a.stop(t)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Shutdown took %v with an idle connection open; it should close it at once", d)
	}
	if _, err := os.Lstat(a.path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the socket file is still there after Shutdown (Lstat: %v)", err)
	}
	_ = idle.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := idle.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("the idle connection was not closed: read error %v", err)
	}
	if _, err := a.client.Health(t.Context()); !errors.Is(err, ErrAdminNotRunning) {
		t.Errorf("after Shutdown the client got %v, want ErrAdminNotRunning", err)
	}
}

// A request that outlives the shutdown's context is cut off, and Shutdown says so with the context's error.
func TestAdminServerShutdownForced(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	a := startAdmin(t, func(o *AdminOptions, _ *AdminListenOptions) {
		o.Status = func(ctx context.Context) (api.ServerStatus, error) {
			close(entered)
			<-release
			return api.ServerStatus{}, nil
		}
	})
	done := make(chan error, 1)
	go func() {
		_, err := a.client.Status(context.Background())
		done <- err
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := a.srv.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Shutdown = %v, want the context's deadline error", err)
	}
	close(release)
	if err := <-done; err == nil {
		t.Error("the request that was cut off returned no error")
	}
	if err := <-a.served; err != nil {
		t.Errorf("Serve returned %v", err)
	}
	a.served = nil
}

// HTTP/1.1 only, and the handler is reachable without the socket.
func TestAdminServerHandler(t *testing.T) {
	srv := NewAdminServer(AdminOptions{Health: NewHealth()})
	code, _, body := get(t, srv.Handler(), http.MethodGet, "")
	if code != http.StatusNotFound || !strings.Contains(body, `"code":"not_found"`) {
		t.Errorf("GET / = %d %s", code, body)
	}
	defer func() {
		if recover() == nil {
			t.Error("NewAdminServer without Health did not panic")
		}
	}()
	NewAdminServer(AdminOptions{})
}

// dialUnix connects to the unix socket at path.
func dialUnix(t *testing.T, path string) net.Conn {
	t.Helper()
	var d net.Dialer
	c, err := d.DialContext(t.Context(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// listenUnix listens on a plain unix socket at path: no peer filter, no admin server.
func listenUnix(t *testing.T, path string) net.Listener {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// waitFor polls cond for up to 5 s.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5 s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
