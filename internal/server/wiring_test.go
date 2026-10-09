package server_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/netx"
	"github.com/MoonWX/isshoni/internal/server/ops"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
	"github.com/MoonWX/isshoni/internal/server/store"
	"github.com/MoonWX/isshoni/internal/version"
)

// The wiring of 04 §6.6 in a running server (04 §17 "Wiring", README S54): accounts over REST, signaling on /ws
// and the operator's admin socket, all on one database.

const (
	adminName     = "Alex"
	adminPassword = "correct horse battery"
)

// wiredDeps are the Deps of a server whose accounts a test uses: a small web app and the tests' hash cost.
func wiredDeps() server.Deps { return server.Deps{SPA: testSPA(), Argon: testArgon} }

// call sends a request as the web app does: with the site's Origin, and a JSON body (nil for none) under
// Content-Type: application/json, which every unsafe method needs (03 §7.5).
func call(t *testing.T, srv *servertest.Server, method, path string, body any) response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, srv.URL+path, payload)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", srv.URL)
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := srv.Client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return response{status: res.StatusCode, header: res.Header, body: string(b)}
}

// wantStatus fails the test unless res has the status and, for an error, the code.
func wantStatus(t *testing.T, what string, res response, status int, code string) {
	t.Helper()
	if res.status != status || errorCode(res.body) != code {
		t.Fatalf("%s = %d %q, want %d %s", what, res.status, res.body, status, code)
	}
}

// adminSocket returns a client of the server's admin socket and a context for its calls.
func adminSocket(t *testing.T, srv *servertest.Server) (*ops.AdminClient, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ops.DialAdmin(srv.AdminSocket), ctx
}

// setupTokenOf is the token of a setup link, which must be the site's /setup page with the token in the fragment.
func setupTokenOf(t *testing.T, srv *servertest.Server, link string) string {
	t.Helper()
	token, ok := strings.CutPrefix(link, srv.URL+"/setup#")
	if !ok || token == "" {
		t.Fatalf("setup link %q, want %s/setup#<token>", link, srv.URL)
	}
	return token
}

// setUp creates the admin account as the operator does: a setup link from the admin socket, then the setup form.
// srv.Client is signed in as the admin afterwards.
func setUp(t *testing.T, srv *servertest.Server) {
	t.Helper()
	admin, ctx := adminSocket(t, srv)
	link, err := admin.SetupURL(ctx, 0)
	if err != nil {
		t.Fatalf("setup-url: %v", err)
	}
	res := call(t, srv, http.MethodPost, "/api/v1/auth/setup/complete", api.SetupCompleteRequest{
		Token: setupTokenOf(t, srv, link.URL), Username: adminName, Password: adminPassword,
	})
	wantStatus(t, "setup/complete", res, http.StatusCreated, "")
}

// dialWS opens the WebSocket of srv.Client's browser session: the session cookie from its jar and the site's
// Origin.
func dialWS(t *testing.T, srv *servertest.Server) *signaltest.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := signaltest.Dial(ctx, srv.Client, srv.WSURL, signaltest.DialOptions{Header: http.Header{"Origin": {srv.URL}}})
	if err != nil {
		t.Fatalf("dial %s: %v", srv.WSURL, err)
	}
	t.Cleanup(c.Close)
	return c
}

// joinLounge says hello on c and joins the default room. It returns the welcome and the room's first state.
func joinLounge(t *testing.T, c *signaltest.Client, hello protocol.Hello) (protocol.Welcome, protocol.RoomState) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	welcome, err := c.Hello(ctx, hello)
	if err != nil {
		t.Fatalf("hello: %v", err)
	}
	id := c.NextID()
	if err := c.Send(protocol.MessageTypeRoomJoin, id, protocol.RoomJoin{RoomID: welcome.DefaultRoomID}); err != nil {
		t.Fatal(err)
	}
	reply := nextOf(t, c, protocol.MessageTypeOK, protocol.MessageTypeError)
	if reply.Type != protocol.MessageTypeOK || reply.Re != id {
		t.Fatalf("room.join answered %s %s (re %q, want %q)", reply.Type, reply.Data, reply.Re, id)
	}
	joined, err := protocol.Decode[protocol.RoomJoinResult](reply)
	if err != nil || joined.Room != (protocol.RoomInfo{ID: "lounge", Name: "Lounge"}) {
		t.Fatalf("room.join result = %+v, %v; want Lounge", joined, err)
	}
	state, err := protocol.Decode[protocol.RoomState](nextOf(t, c, protocol.MessageTypeRoomState))
	if err != nil {
		t.Fatal(err)
	}
	return welcome, state
}

// nextOf returns the next message of one of the given types, skipping the others (room events, invalidations).
func nextOf(t *testing.T, c *signaltest.Client, types ...protocol.MessageType) protocol.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		env, err := c.Recv(ctx)
		if err != nil {
			t.Fatalf("waiting for %v: %v", types, err)
		}
		if slices.Contains(types, env.Type) {
			return env
		}
	}
}

// closeCode waits for the server to close c and returns the close code.
func closeCode(t *testing.T, c *signaltest.Client) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	code, err := c.CloseStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

// TestSetupLoginJoinLogout is the acceptance test of the first wiring (README S54, 04 W1, 03 slice 14) on an
// off-mode server: the operator's setup link creates the admin, the admin logs in over REST, its WebSocket joins
// Lounge, and logging out closes that socket with session_revoked within 100 ms.
func TestSetupLoginJoinLogout(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{Deps: wiredDeps()})
	admin, ctx := adminSocket(t, srv)

	// A new server: setup is open, the web app's /setup page is there, and nobody can log in.
	var info api.Info
	if res := call(t, srv, http.MethodGet, "/api/v1/info", nil); res.status != 200 || json.Unmarshal([]byte(res.body), &info) != nil {
		t.Fatalf("GET /info = %d %q", res.status, res.body)
	}
	if !info.SetupRequired || info.Server.PublicURL != srv.URL || info.Server.Version != version.Version() ||
		info.Protocol != (api.InfoProtocol{Current: protocol.Version, Min: protocol.MinVersion}) || info.Push != nil {
		t.Errorf("/info of a new server = %+v", info)
	}
	if res := get(t, srv, "/setup"); res.status != 200 || res.body != indexHTML {
		t.Errorf("GET /setup before setup = %d", res.status)
	}
	wantStatus(t, "login before setup", call(t, srv, http.MethodPost, "/api/v1/auth/login",
		api.LoginRequest{Username: adminName, Password: adminPassword}), http.StatusUnauthorized, api.CodeInvalidCredentials)
	wantStatus(t, "GET /me without a session", call(t, srv, http.MethodGet, "/api/v1/me", nil), http.StatusUnauthorized, api.CodeUnauthenticated)

	// The operator's setup link, as `isshoni setup-url` gets it from the admin socket.
	link, err := admin.SetupURL(ctx, 0)
	if err != nil {
		t.Fatalf("setup-url: %v", err)
	}
	if !link.TLSReady || time.Until(time.Time(link.ExpiresAt)) < 23*time.Hour {
		t.Errorf("setup link = tlsReady %v, expires %v", link.TLSReady, time.Time(link.ExpiresAt))
	}
	token := setupTokenOf(t, srv, link.URL)
	wantStatus(t, "setup/check", call(t, srv, http.MethodPost, "/api/v1/auth/setup/check", api.TokenRequest{Token: token}), http.StatusNoContent, "")
	res := call(t, srv, http.MethodPost, "/api/v1/auth/setup/complete",
		api.SetupCompleteRequest{Token: token, Username: adminName, Password: adminPassword, ServerName: "Movie club"})
	wantStatus(t, "setup/complete", res, http.StatusCreated, "")

	// Setup is done: the page is gone (404 with the web app, 03 §12.6), and so is the link.
	if res := get(t, srv, "/setup"); res.status != 404 || res.body != indexHTML {
		t.Errorf("GET /setup after setup = %d %q, want 404 with the web app", res.status, res.body)
	}
	if res := get(t, srv, "/login"); res.status != 200 {
		t.Errorf("GET /login after setup = %d: only /setup goes away", res.status)
	}
	var refused *ops.AdminError
	if _, err := admin.SetupURL(ctx, 0); !errors.As(err, &refused) || refused.API.Code != api.CodeSetupUnavailable {
		t.Errorf("setup-url after setup: %v, want setup_unavailable", err)
	}

	// Log out, then in again over REST.
	wantStatus(t, "logout", call(t, srv, http.MethodPost, "/api/v1/auth/logout", struct{}{}), http.StatusNoContent, "")
	wantStatus(t, "GET /me after logout", call(t, srv, http.MethodGet, "/api/v1/me", nil), http.StatusUnauthorized, api.CodeUnauthenticated)

	// The best of three counts for the 100 ms, so that a busy CI machine doesn't fail it; it takes a few
	// milliseconds.
	var took []time.Duration
	for range 3 {
		res = call(t, srv, http.MethodPost, "/api/v1/auth/login", api.LoginRequest{Username: adminName, Password: adminPassword})
		wantStatus(t, "login", res, http.StatusOK, "")
		var who api.UserResponse
		if err := json.Unmarshal([]byte(res.body), &who); err != nil || who.User.Username != adminName || who.User.Role != api.RoleAdmin {
			t.Fatalf("login answered %q", res.body)
		}

		// /ws with the session cookie: hello, then Lounge.
		c := dialWS(t, srv)
		welcome, state := joinLounge(t, c, signaltest.DefaultHello())
		if welcome.User != (protocol.UserInfo{ID: who.User.ID, Name: adminName, Admin: true}) || welcome.DefaultRoomID != "lounge" ||
			welcome.ServerVersion != version.Version() || welcome.ResumeToken == "" || welcome.Resumed {
			t.Errorf("welcome = %+v", welcome)
		}
		if state.RoomID != "lounge" || len(state.Participants) != 1 || state.Participants[0].UserID != who.User.ID ||
			state.Participants[0].Name != adminName || len(state.Shares) != 0 {
			t.Errorf("room.state = %+v", state)
		}
		// The dashboard's and the room list's view of it: one person in Lounge.
		if st, err := admin.Status(ctx); err != nil || st.Rooms != 1 || st.Participants != 1 || st.Shares != 0 {
			t.Errorf("status with one connection = %+v, %v", st, err)
		}

		// Logging out closes this session's socket: error{session_revoked}, then close 4401 (03 §7.7, 01 §12.2).
		begin := time.Now()
		wantStatus(t, "logout", call(t, srv, http.MethodPost, "/api/v1/auth/logout", struct{}{}), http.StatusNoContent, "")
		revoked, err := protocol.Decode[protocol.Error](nextOf(t, c, protocol.MessageTypeError))
		if err != nil || revoked.Code != protocol.ErrorCodeSessionRevoked || revoked.Scope != protocol.ErrorScopeSession || revoked.Retryable {
			t.Fatalf("after logout the socket got %+v, %v; want session_revoked", revoked, err)
		}
		if code := closeCode(t, c); code != websocket.StatusCode(protocol.CloseCodeUnauthenticated) {
			t.Errorf("close code = %d, want %d", code, protocol.CloseCodeUnauthenticated)
		}
		d := time.Since(begin)
		took = append(took, d)
		if d < 100*time.Millisecond {
			t.Logf("logout closed the socket after %v", d)
			break
		}
	}
	if slices.Min(took) >= 100*time.Millisecond {
		t.Errorf("logout closed the socket after %v, want under 100 ms at least once", took)
	}

	// The session is gone for REST and for a new socket alike: without a valid cookie /ws is pre-auth, and a hello
	// without a bearer token is unauthenticated.
	wantStatus(t, "GET /me after logout", call(t, srv, http.MethodGet, "/api/v1/me", nil), http.StatusUnauthorized, api.CodeUnauthenticated)
	c := dialWS(t, srv)
	helloCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var pe *protocol.Error
	if _, err := c.Hello(helloCtx, signaltest.DefaultHello()); !errors.As(err, &pe) || pe.Code != protocol.ErrorCodeUnauthenticated {
		t.Errorf("hello without a session: %v, want unauthenticated", err)
	}
	if st, err := admin.Status(ctx); err != nil || st.Rooms != 0 || st.Participants != 0 {
		t.Errorf("status after logout = %+v, %v; want nobody", st, err)
	}

	// The server name of the setup form is a setting like any other: /info shows it.
	if res := call(t, srv, http.MethodGet, "/api/v1/info", nil); json.Unmarshal([]byte(res.body), &info) != nil ||
		info.SetupRequired || info.Server.Name != "Movie club" {
		t.Errorf("/info after setup = %q", res.body)
	}
}

// TestSetupURLCommand is the last acceptance item of README S54: `isshoni setup-url --json`, the real binary against
// the running server's admin socket, prints a link whose token completes setup. The test builds cmd/isshoni, so it
// needs the go tool and takes a few seconds: not under -short.
func TestSetupURLCommand(t *testing.T) {
	if testing.Short() {
		t.Skip("builds cmd/isshoni")
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("no go tool to build cmd/isshoni with: %v", err)
	}
	// Built the way this test binary was (with the race detector or without, in the same environment), so every
	// package but cmd/isshoni's own comes from the build cache of the test run.
	bin := filepath.Join(t.TempDir(), "isshoni")
	args := []string{"build", "-o", bin}
	if raceEnabled {
		args = append(args, "-race")
	}
	build := exec.CommandContext(t.Context(), goTool, append(args, "github.com/MoonWX/isshoni/cmd/isshoni")...)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/isshoni: %v\n%s", err, out)
	}
	// The CLI reads the default config file and the environment; this test gives it an empty file and only the
	// socket's path.
	empty := filepath.Join(t.TempDir(), "empty.toml")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (stdout, stderr string, exit int) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...)
		// atexit_sleep_ms: a race-detector binary otherwise waits a second before each exit.
		cmd.Env = []string{config.EnvConfig + "=" + empty, "GORACE=atexit_sleep_ms=0"}
		var out, errOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errOut
		err := cmd.Run()
		var ee *exec.ExitError
		switch {
		case err == nil:
		case errors.As(err, &ee):
			exit = ee.ExitCode()
		default:
			t.Fatalf("isshoni %v: %v", args, err)
		}
		return out.String(), errOut.String(), exit
	}

	srv := servertest.Start(t, servertest.Options{Deps: wiredDeps()})
	stdout, stderr, exit := run("setup-url", "--json", "--socket", srv.AdminSocket)
	if exit != 0 {
		t.Fatalf("isshoni setup-url --json: exit %d\n%s%s", exit, stdout, stderr)
	}
	var link struct {
		URL       string    `json:"url"`
		ExpiresAt time.Time `json:"expiresAt"`
		TLSReady  bool      `json:"tlsReady"`
	}
	if err := json.Unmarshal([]byte(stdout), &link); err != nil || !link.TLSReady || time.Until(link.ExpiresAt) < 23*time.Hour {
		t.Fatalf("setup-url --json printed %q (%v)", stdout, err)
	}
	// The token from the command's output creates the admin, who is signed in at once.
	res := call(t, srv, http.MethodPost, "/api/v1/auth/setup/complete",
		api.SetupCompleteRequest{Token: setupTokenOf(t, srv, link.URL), Username: adminName, Password: adminPassword})
	wantStatus(t, "setup/complete with the command's token", res, http.StatusCreated, "")
	var me api.Me
	if res := call(t, srv, http.MethodGet, "/api/v1/me", nil); res.status != 200 || json.Unmarshal([]byte(res.body), &me) != nil ||
		me.User.Username != adminName || me.User.Role != api.RoleAdmin {
		t.Errorf("GET /me after setup = %d %q", res.status, res.body)
	}

	// The other commands of the first wiring reach the server the same way: no more "isshoni is not running".
	if _, stderr, exit := run("healthcheck", "--ready", "--socket", srv.AdminSocket); exit != 0 {
		t.Errorf("isshoni healthcheck --ready: exit %d\n%s", exit, stderr)
	}
	stdout, stderr, exit = run("admin", "users", "list", "--json", "--socket", srv.AdminSocket)
	var users ops.AdminUsers
	if exit != 0 || json.Unmarshal([]byte(stdout), &users) != nil || len(users.Users) != 1 || users.Users[0].Username != adminName {
		t.Errorf("isshoni admin users list --json: exit %d\n%s%s", exit, stdout, stderr)
	}
	// Once the admin exists the command refuses, with the exit code of 04 §3.2 and the way back in.
	if _, stderr, exit := run("setup-url", "--json", "--socket", srv.AdminSocket); exit != 7 || !strings.Contains(stderr, "reset-password") {
		t.Errorf("isshoni setup-url after setup: exit %d, want 7 with the reset-password hint\n%s", exit, stderr)
	}
	// And without a server it says so.
	srv.Stop(t)
	if _, stderr, exit := run("setup-url", "--json", "--socket", srv.AdminSocket); exit != 4 || !strings.Contains(stderr, "isshoni is not running") {
		t.Errorf("isshoni setup-url without a server: exit %d, want 4\n%s", exit, stderr)
	}
}

// TestAdminSocket: the server serves the admin socket of 04 §12 from its start to the last step of its shutdown.
func TestAdminSocket(t *testing.T) {
	level := new(slog.LevelVar)
	level.Set(slog.LevelWarn)
	deps := wiredDeps()
	deps.LogLevel = level
	srv := servertest.Start(t, servertest.Options{Deps: deps})
	admin, ctx := adminSocket(t, srv)

	if fi, err := os.Stat(srv.AdminSocket); err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Errorf("the admin socket %s: %v, %v; want a socket with mode 0600", srv.AdminSocket, fi, err)
	}
	if h, err := admin.Health(ctx); err != nil || h.Status != ops.StatusOK || h.Version != version.Version() {
		t.Errorf("health = %+v, %v", h, err)
	}
	// The socket shows the readiness checks to whoever reaches it: the operator (04 §11.1).
	ready, err := admin.Ready(ctx)
	if err != nil || ready.Status != ops.StatusReady ||
		!mapsEqual(ready.Checks, map[string]string{"db": "ok", "signal": "ok", "tls": "ok"}) {
		t.Errorf("ready = %+v, %v", ready, err)
	}
	st, err := admin.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantListeners := []api.ListenerInfo{
		{Key: "listen.http", Network: "tcp", Addr: srv.Srv.Addrs().HTTP.String()},
		{Key: "listen.admin_socket", Network: "unix", Addr: srv.AdminSocket},
	}
	if st.Version != version.Version() || st.Origin != srv.URL || st.TLS.Mode != api.TLSModeOff || !st.TLS.Ready ||
		st.SchemaVersion != store.LatestSchemaVersion() || !slices.Equal(st.Listeners, wantListeners) ||
		time.Since(st.StartedAt) > time.Minute || st.Rooms != 0 {
		t.Errorf("status = %+v", st)
	}

	// Accounts: nobody yet, then the admin.
	if users, err := admin.Users(ctx); err != nil || len(users.Users) != 0 {
		t.Errorf("users of a new server = %+v, %v", users, err)
	}
	setUp(t, srv)
	users, err := admin.Users(ctx)
	if err != nil || len(users.Users) != 1 || users.Users[0].Username != adminName || users.Users[0].Role != api.RoleAdmin ||
		users.Users[0].Status != api.UserStatusActive {
		t.Errorf("users after setup = %+v, %v", users, err)
	}
	// An invite from the CLI is a link of the site that registers a member.
	invite, err := admin.CreateInvite(ctx, 2, 48*time.Hour)
	inviteToken, ok := strings.CutPrefix(invite.URL, srv.URL+"/invite#")
	if err != nil || !ok || inviteToken == "" {
		t.Fatalf("invite = %+v, %v", invite, err)
	}
	wantStatus(t, "register with the CLI's invite", call(t, srv, http.MethodPost, "/api/v1/auth/register",
		api.RegisterRequest{InviteToken: inviteToken, Username: "Sam", Password: adminPassword + " too"}), http.StatusCreated, "")
	if users, err := admin.Users(ctx); err != nil || len(users.Users) != 2 {
		t.Errorf("users after the registration = %+v, %v", users, err)
	}

	// The log level changes for a while (04 §10) and is the configured one again when the server has stopped.
	if err := admin.SetLogLevel(ctx, "debug", time.Minute); err != nil || level.Level() != slog.LevelDebug {
		t.Errorf("log-level debug: %v; the level is %v", err, level.Level())
	}

	srv.Stop(t)
	if level.Level() != slog.LevelWarn {
		t.Errorf("the log level after the shutdown is %v, want the configured warn", level.Level())
	}
	// The socket is gone with the server: the CLI then says "isshoni is not running".
	if _, err := os.Lstat(srv.AdminSocket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the socket file after the shutdown: %v", err)
	}
	if _, err := admin.Health(ctx); !errors.Is(err, ops.ErrAdminNotRunning) {
		t.Errorf("health after the shutdown: %v, want ErrAdminNotRunning", err)
	}

	// A restart binds the same path again, and the accounts are still there.
	srv.Restart(t)
	if users, err := admin.Users(ctx); err != nil || len(users.Users) != 2 {
		t.Errorf("users after a restart = %+v, %v", users, err)
	}
}

// TestLogLevelNeedsTheLevelVar: without Deps.LogLevel the level can't change while the server runs, and the socket
// says so instead of pretending. The test builds the server itself, so that Deps.LogLevel is nil whatever the
// harness passes for a test that leaves it out.
func TestLogLevelNeedsTheLevelVar(t *testing.T) {
	cfg := testConfig(t)
	startDirect(t, cfg, testDeps(), netx.PublicAddrs{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var ae *ops.AdminError
	err := ops.DialAdmin(cfg.Listen.AdminSocket).SetLogLevel(ctx, "debug", time.Minute)
	if !errors.As(err, &ae) || !strings.Contains(ae.Message, "log level") {
		t.Errorf("log-level without a level variable: %v", err)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// TestAdminSocketInUse: a second server on the same socket path does not start (exit 1: stopping the other one
// helps), and the first keeps its socket.
func TestAdminSocketInUse(t *testing.T) {
	first := servertest.Start(t, servertest.Options{Deps: wiredDeps()})
	_, err := servertest.Try(t, servertest.Options{
		Deps:  wiredDeps(),
		Flags: []string{"--listen.admin-socket=" + first.AdminSocket},
	})
	if err == nil || server.NeedsOperator(err) || !strings.Contains(err.Error(), "listen.admin_socket") {
		t.Fatalf("a second server on the same admin socket: %v, want a runtime error naming the key", err)
	}
	admin, ctx := adminSocket(t, first)
	if _, err := admin.Health(ctx); err != nil {
		t.Errorf("the first server's socket after that: %v", err)
	}
}

// TestHubShutdown: step 3 of the shutdown (04 §6.4) tells every connection that the server is going away and will
// be back, and closes with 1012; the server is stopped well inside its budgets.
func TestHubShutdown(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{Deps: wiredDeps()})
	setUp(t, srv)
	c := dialWS(t, srv)
	joinLounge(t, c, signaltest.DefaultHello())

	// The shutdown runs beside the test, which reads the socket meanwhile; Run's result is the test's to collect,
	// below (srv.Stop in a goroutine would take it away, and could report to a test that is over).
	begin := time.Now()
	shutdownErr := make(chan error, 1)
	go func() { shutdownErr <- srv.Srv.Shutdown(context.Background(), server.ShutdownStop) }()
	notice, err := protocol.Decode[protocol.ServerShutdown](nextOf(t, c, protocol.MessageTypeServerShutdown))
	// SIGTERM can't tell a stop from a restart: the wire always says restart, and clients reconnect.
	if err != nil || notice.Reason != protocol.ShutdownReasonRestart || notice.ReconnectInMs < 500 || notice.ReconnectInMs > 3000 {
		t.Errorf("server.shutdown = %+v, %v", notice, err)
	}
	gone, err := protocol.Decode[protocol.Error](nextOf(t, c, protocol.MessageTypeError))
	if err != nil || gone.Code != protocol.ErrorCodeServerShutdown || gone.Scope != protocol.ErrorScopeConnection || !gone.Retryable {
		t.Errorf("the error before the close = %+v, %v", gone, err)
	}
	if code := closeCode(t, c); code != websocket.StatusCode(protocol.CloseCodeServiceRestart) {
		t.Errorf("close code = %d, want 1012", code)
	}
	if d := time.Since(begin); d > 2*time.Second {
		t.Errorf("the connection learned of the shutdown after %v, want within 2 s", d)
	}
	if err := srv.Wait(t); err != nil {
		t.Errorf("Run after the stop = %v", err)
	}
	// Run has returned, so the shutdown is over. Nothing was forced: every step finished in its budget.
	if err := <-shutdownErr; err != nil {
		t.Errorf("Shutdown = %v", err)
	}
	if logs := srv.Logs(); !strings.Contains(logs, "shutdown complete") || strings.Contains(logs, "shutdown finished by force") {
		t.Error("the log does not end the shutdown with \"shutdown complete\": a step was forced or failed")
	}
}

// TestSessionsSurviveARestart: the keys in secrets.json and the database are what a session is made of, so a
// restart keeps everyone signed in (04 §5.2), and a connection that the restart cut comes back as a new one: resume
// tokens don't outlive the process.
func TestSessionsSurviveARestart(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{Deps: wiredDeps()})
	setUp(t, srv)
	c := dialWS(t, srv)
	welcome, _ := joinLounge(t, c, signaltest.DefaultHello())

	// A socket that drops without a close frame leaves its connection in grace: a new socket resumes it with the
	// token of the welcome, in its room.
	c.Close()
	c = dialWS(t, srv)
	hello := signaltest.DefaultHello()
	hello.ResumeToken = welcome.ResumeToken
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resumed, err := c.Hello(ctx, hello)
	if err != nil || !resumed.Resumed || resumed.ConnectionID != welcome.ConnectionID || resumed.RoomID != "lounge" {
		t.Fatalf("hello with the resume token = %+v, %v; want the same connection, resumed in Lounge", resumed, err)
	}

	srv.Restart(t)
	var me api.Me
	if res := call(t, srv, http.MethodGet, "/api/v1/me", nil); res.status != 200 || json.Unmarshal([]byte(res.body), &me) != nil ||
		me.User.Username != adminName {
		t.Fatalf("GET /me after a restart = %d %q", res.status, res.body)
	}
	c = dialWS(t, srv)
	hello.ResumeToken = resumed.ResumeToken
	again, err := c.Hello(ctx, hello)
	if err != nil || again.Resumed || again.ConnectionID == welcome.ConnectionID || again.User.Name != adminName {
		t.Errorf("hello after the restart = %+v, %v; want a new connection of the same user", again, err)
	}
}

// TestWiringOverTLS: in a TLS mode the site is https, so the session cookie is the __Host- one and the WebSocket is
// wss on the same port as the web app (04 §7.2, 03 §7.4).
func TestWiringOverTLS(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{TLS: true, Deps: wiredDeps()})
	setUp(t, srv)
	var names []string
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, ck := range srv.Client.Jar.Cookies(u) {
		names = append(names, ck.Name)
	}
	if !slices.Equal(names, []string{"__Host-isshoni_session"}) {
		t.Errorf("cookies of %s = %v, want the __Host- session cookie", srv.URL, names)
	}
	if !strings.HasPrefix(srv.WSURL, "wss://") {
		t.Fatalf("WSURL = %s", srv.WSURL)
	}
	c := dialWS(t, srv)
	welcome, state := joinLounge(t, c, signaltest.DefaultHello())
	if welcome.User.Name != adminName || len(state.Participants) != 1 {
		t.Errorf("welcome %+v, room.state %+v", welcome, state)
	}
	// Another site's page can't open the socket with the user's cookie (01 §3.1).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = signaltest.Dial(ctx, srv.Client, srv.WSURL, signaltest.DialOptions{Header: http.Header{"Origin": {"https://evil.example"}}})
	var refused *signaltest.RefusedError
	if !errors.As(err, &refused) || refused.Status != http.StatusForbidden {
		t.Errorf("a WebSocket from another origin: %v, want 403", err)
	}
}

// TestPolicyPins: a policy key that the operator set is pinned into the settings before the server serves (04
// §4.6): the admin page shows it locked and can't change it. A value that only the test's Config hook changed is
// not the operator's.
func TestPolicyPins(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{
		Deps:  wiredDeps(),
		Flags: []string{"--registration.mode=approval", "--limits.max-bitrate-kbps=2500"},
		Config: func(c *config.Config) {
			c.Limits.MaxSharesPerRoom = 3 // not set by flag, env or file
		},
	})
	setUp(t, srv)

	var settings api.SettingsResponse
	res := call(t, srv, http.MethodGet, "/api/v1/admin/settings", nil)
	if res.status != 200 || json.Unmarshal([]byte(res.body), &settings) != nil {
		t.Fatalf("GET /admin/settings = %d %q", res.status, res.body)
	}
	if !slices.Equal(settings.Locked, []string{"registrationMode", "maxShareBitrateKbps"}) {
		t.Errorf("locked = %v", settings.Locked)
	}
	if settings.Settings.RegistrationMode != api.RegistrationModeApproval || settings.Settings.MaxShareBitrateKbps != 2500 ||
		settings.Settings.MaxSharesPerRoom != 0 {
		t.Errorf("settings = %+v", settings.Settings)
	}
	if res := call(t, srv, http.MethodGet, "/api/v1/info", nil); !strings.Contains(res.body, `"registration":"approval"`) {
		t.Errorf("/info = %q, want the pinned registration mode", res.body)
	}
	wantStatus(t, "PATCH of a pinned setting", call(t, srv, http.MethodPatch, "/api/v1/admin/settings",
		map[string]any{"registrationMode": "invite"}), http.StatusConflict, api.CodeSettingLocked)
	// What is not pinned is the admin's, and survives a restart; the pins come back with the config.
	wantStatus(t, "PATCH of a free setting", call(t, srv, http.MethodPatch, "/api/v1/admin/settings",
		map[string]any{"maxSharesPerRoom": 5}), http.StatusOK, "")
	srv.Restart(t)
	res = call(t, srv, http.MethodGet, "/api/v1/admin/settings", nil)
	if json.Unmarshal([]byte(res.body), &settings) != nil || settings.Settings.MaxSharesPerRoom != 5 ||
		!slices.Equal(settings.Locked, []string{"registrationMode", "maxShareBitrateKbps"}) {
		t.Errorf("settings after a restart = %q", res.body)
	}

	t.Run("a value the settings refuse", func(t *testing.T) {
		// The settings are the only judge of a policy value (04 §4.5): the server refuses to start, like for any
		// other config error, and has let go of the data directory again.
		dir := filepath.Join(t.TempDir(), "data")
		_, err := servertest.Try(t, servertest.Options{DataDir: dir, Deps: wiredDeps(), Flags: []string{"--limits.max-bitrate-kbps=100"}})
		var ve *config.ValidationError
		if !server.NeedsOperator(err) || !errors.As(err, &ve) || len(ve.Problems) != 1 || ve.Problems[0].Key != "limits.max_bitrate_kbps" {
			t.Fatalf("Start with limits.max_bitrate_kbps = 100: %v, want a config error for that key", err)
		}
		servertest.Start(t, servertest.Options{DataDir: dir, Deps: wiredDeps()})
	})
}

// TestRefusesNewerSchema: a database that a newer isshoni has written is not opened (04 §6.3): the server refuses
// with 03's message and, when a backup from before the upgrade exists, with the command that restores it.
func TestRefusesNewerSchema(t *testing.T) {
	// A fake machine, so that the command's form does not depend on where the tests run.
	bare := config.Host{Root: t.TempDir(), Environ: []string{}}
	container := config.Host{Root: bare.Root, Environ: []string{config.EnvInContainer + "=1", config.EnvAllowEphemeralData + "=1"}}
	deps := func(h config.Host) server.Deps {
		d := wiredDeps()
		d.Host = h
		return d
	}

	srv := servertest.Start(t, servertest.Options{Deps: deps(bare)})
	paths := srv.Cfg.Paths()
	srv.Stop(t)
	future := store.LatestSchemaVersion() + 1
	alter := func(query string, args ...any) {
		t.Helper()
		db, err := sql.Open("sqlite", paths.DB)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	alter(`INSERT INTO schema_migrations (version, name, applied_at, app_version) VALUES (?, 'from_the_future', 0, '9.9.9')`, future)

	_, err := servertest.Try(t, servertest.Options{DataDir: srv.DataDir, Deps: deps(bare)})
	var tooNew *store.SchemaTooNewError
	if !server.NeedsOperator(err) || !errors.As(err, &tooNew) || tooNew.DBVersion != future || tooNew.Backup != "" {
		t.Fatalf("Start on a newer schema: %v, want a refusal with a *store.SchemaTooNewError", err)
	}
	if strings.Contains(err.Error(), "fix:") {
		t.Errorf("without a backup there is nothing to restore: %v", err)
	}

	// With the backup that the upgrade to the newer schema left behind, the refusal ends with the restore command.
	backup := filepath.Join(paths.Backups, "pre-"+strconv.Itoa(store.LatestSchemaVersion())+"-20261014T021500Z.db")
	if err := os.WriteFile(backup, []byte("a database from before the upgrade"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = servertest.Try(t, servertest.Options{DataDir: srv.DataDir, Deps: deps(bare)})
	if want := "\n  fix: sudo -u isshoni isshoni admin restore --offline " + backup + " && sudo systemctl start isshoni"; !server.NeedsOperator(err) || !strings.HasSuffix(err.Error(), want) {
		t.Errorf("the refusal with a backup:\n%v\nwant it to end with%s", err, want)
	}
	_, err = servertest.Try(t, servertest.Options{DataDir: srv.DataDir, Deps: deps(container)})
	if want := "\n  fix: docker compose stop && docker compose run --rm isshoni admin restore --offline " + backup + " && docker compose up -d"; !server.NeedsOperator(err) || !strings.HasSuffix(err.Error(), want) {
		t.Errorf("the refusal in a container:\n%v\nwant it to end with%s", err, want)
	}

	// The refusals left nothing open: once the database is this binary's again, the server starts.
	alter(`DELETE FROM schema_migrations WHERE version = ?`, future)
	servertest.Start(t, servertest.Options{DataDir: srv.DataDir, Deps: deps(bare)})
}

// TestSetupHint: while no admin exists, the start says how to finish setup (04 §6.1 step 10). The setup token
// itself, the session token and the password never reach the log, even at debug level.
func TestSetupHint(t *testing.T) {
	const hint = "Finish setup: run `sudo isshoni setup-url` (Docker: `docker compose exec isshoni isshoni setup-url`)"
	hints := func(srv *servertest.Server) (n int) {
		for _, rec := range logRecords(t, srv.Logs()) {
			if rec["msg"] == hint {
				n++
				if rec["level"] != "INFO" {
					t.Errorf("the setup hint = %v, want it at INFO", rec)
				}
			}
		}
		return n
	}
	srv := servertest.Start(t, servertest.Options{Deps: wiredDeps()})
	if n := hints(srv); n != 1 {
		t.Errorf("%d setup hints after the first start, want 1", n)
	}

	admin, ctx := adminSocket(t, srv)
	link, err := admin.SetupURL(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	token := setupTokenOf(t, srv, link.URL)
	wantStatus(t, "setup/complete", call(t, srv, http.MethodPost, "/api/v1/auth/setup/complete",
		api.SetupCompleteRequest{Token: token, Username: adminName, Password: adminPassword}), http.StatusCreated, "")
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	cookies := srv.Client.Jar.Cookies(u)
	if len(cookies) != 1 || cookies[0].Value == "" {
		t.Fatalf("cookies after setup = %v", cookies)
	}
	c := dialWS(t, srv)
	welcome, _ := joinLounge(t, c, signaltest.DefaultHello())

	// With an admin the next start has nothing to hint at.
	srv.Restart(t)
	if n := hints(srv); n != 1 {
		t.Errorf("%d setup hints after setup and a restart, want still 1", n)
	}
	logs := srv.Logs()
	for what, secret := range map[string]string{
		"setup token":   token,
		"session token": cookies[0].Value,
		"resume token":  string(welcome.ResumeToken),
		"password":      adminPassword,
		"username":      `"` + adminName + `"`,
	} {
		if secret == "" || strings.Contains(logs, secret) {
			t.Errorf("the %s is in the log (or the test has none)", what)
		}
	}
}

// TestServerWithoutASite: an ip-mode server that found no public address has no origin to build accounts, links and
// the hub on (04 §7.4). It still opens its database and answers the operator on the admin socket, who sees what is
// missing there.
func TestServerWithoutASite(t *testing.T) {
	cfg := testConfig(t, tlsFlags("--tls.mode=ip", closedCA)...)
	deps := testDeps()
	deps.Argon = testArgon
	s, _ := startDirect(t, cfg, deps, netx.PublicAddrs{})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin := ops.DialAdmin(cfg.Listen.AdminSocket)
	if h, err := admin.Health(ctx); err != nil || h.Status != ops.StatusOK {
		t.Errorf("health = %+v, %v", h, err)
	}
	ready, err := admin.Ready(ctx)
	if err != nil || ready.Status != ops.StatusNotReady || ready.Checks["db"] != "ok" ||
		!strings.HasPrefix(ready.Checks["public_ip"], "no public IP address found") {
		t.Errorf("ready = %+v, %v", ready, err)
	}
	if _, has := ready.Checks["signal"]; has {
		t.Errorf("a server without a hub has a signal check: %v", ready.Checks)
	}
	if st, err := admin.Status(ctx); err != nil || st.Origin != "" || st.SchemaVersion != store.LatestSchemaVersion() {
		t.Errorf("status = %+v, %v", st, err)
	}
	var ae *ops.AdminError
	if _, err := admin.SetupURL(ctx, 0); !errors.As(err, &ae) || ae.API.Code != api.CodeInternal {
		t.Errorf("setup-url without a site: %v, want an error answer", err)
	}
	if _, err := os.Stat(cfg.Paths().DB); err != nil {
		t.Errorf("the database: %v", err)
	}
	_ = s
}

// TestPublicAddressWatch: a running server looks at its public addresses again and says so when they changed; it
// keeps serving with the old one, because only a restart applies the new one (04 §7.4).
func TestPublicAddressWatch(t *testing.T) {
	first := netx.PublicAddrs{V4: netip.MustParseAddr("203.0.113.7"), V4Method: netx.MethodInterface}
	moved := netx.PublicAddrs{V4: netip.MustParseAddr("203.0.113.99"), V4Method: netx.MethodInterface}
	var looks atomic.Int32

	cfg := testConfig(t, tlsFlags("--tls.mode=ip", closedCA)...)
	deps := testDeps()
	deps.Argon = testArgon
	logs := &logCapture{}
	s, err := server.New(cfg, logs.logger(), deps)
	if err != nil {
		t.Fatal(err)
	}
	s.SetPublicAddrsFunc(func() netx.PublicAddrs {
		if looks.Add(1) <= 2 { // the start, and one look that finds the same address
			return first
		}
		return moved
	})
	s.SetRedetectEvery(5 * time.Millisecond)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx, server.ShutdownStop); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	waitFor(t, "the changed address", func() bool { return s.PublicAddrsNow() != nil })
	if now := s.PublicAddrsNow(); now.V4 != moved.V4 {
		t.Errorf("the address the server saw last = %v, want %v", now.V4, moved.V4)
	}
	// The site is still the address the server started with: its certificate and its candidates are for that one.
	if site := s.Site(); site.Hostname != "203.0.113.7" {
		t.Errorf("site = %+v", site)
	}
	waitFor(t, "the warning", func() bool { return strings.Contains(logs.String(), "restart isshoni to apply") })
	// More looks find the same new address: it is reported once.
	before := looks.Load()
	waitFor(t, "more looks", func() bool { return looks.Load() >= before+3 })
	var warnings int
	for _, rec := range logRecords(t, logs.String()) {
		msg, _ := rec["msg"].(string)
		if !strings.Contains(msg, "restart isshoni to apply") {
			continue
		}
		warnings++
		if rec["level"] != "WARN" || msg != "Public IPv4 address changed from 203.0.113.7 to 203.0.113.99; restart isshoni to apply" {
			t.Errorf("warning = %v", rec)
		}
	}
	if warnings != 1 {
		t.Errorf("the change was reported %d times, want once:\n%s", warnings, logs.String())
	}
}
