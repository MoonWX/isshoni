package servertest_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/MoonWX/isshoni/internal/server"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/ops"
	"github.com/MoonWX/isshoni/internal/server/servertest"
)

// TestMain fails the run when a goroutine outlives the tests: a stopped harness leaves nothing behind.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func status(t *testing.T, srv *servertest.Server, path string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := srv.Client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// TestStart: Start returns a ready server with every field of 04 §18 filled in.
func TestStart(t *testing.T) {
	begin := time.Now()
	srv := servertest.Start(t, servertest.Options{})
	t.Logf("ready after %v", time.Since(begin))

	if code, body := status(t, srv, "/readyz"); code != 200 || !strings.Contains(body, `"status":"ready"`) {
		t.Errorf("/readyz right after Start = %d %q", code, body)
	}
	if code, body := status(t, srv, "/healthz"); code != 200 || body != `{"status":"ok"}`+"\n" {
		t.Errorf("/healthz = %d %q", code, body)
	}

	site := srv.Srv.Site()
	if srv.URL != site.Origin || !strings.HasPrefix(srv.URL, "http://localhost:") || !site.Dev {
		t.Errorf("URL = %q, site = %+v: want the dev site's origin", srv.URL, site)
	}
	if want := "ws://" + strings.TrimPrefix(srv.URL, "http://") + "/ws"; srv.WSURL != want {
		t.Errorf("WSURL = %q, want %q", srv.WSURL, want)
	}
	if srv.Client == nil || srv.Client.Jar == nil {
		t.Error("Client has no cookie jar")
	}
	if srv.Srv == nil || srv.Cfg == nil {
		t.Fatal("Srv or Cfg is nil")
	}

	// The config is the off-mode test setup of 04 §17: loopback, ephemeral ports, loopback candidates.
	c := srv.Cfg
	if c.EffectiveTLSMode() != config.TLSOff || c.Listen.HTTP != "127.0.0.1:0" || c.Listen.ICEUDP != "127.0.0.1:0" ||
		c.Listen.ICETCP != "127.0.0.1:0" || !c.Network.IncludeLoopback {
		t.Errorf("config: tls %q, listen %+v, include_loopback %v", c.EffectiveTLSMode(), c.Listen, c.Network.IncludeLoopback)
	}
	if c.DataDir != srv.DataDir || c.Listen.AdminSocket != srv.AdminSocket {
		t.Errorf("config data_dir %q and admin socket %q differ from the harness's %q and %q",
			c.DataDir, c.Listen.AdminSocket, srv.DataDir, srv.AdminSocket)
	}
	// No policy key is set by the harness: a flag would pin it against the admin UI (04 §4.6).
	for _, k := range config.Keys() {
		if k.Policy && c.IsSet(k.Path) {
			t.Errorf("the harness sets the policy key %s", k.Path)
		}
	}

	if fi, err := os.Stat(srv.DataDir); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Errorf("DataDir %q: %v, %v", srv.DataDir, fi, err)
	}
	if n := len(srv.AdminSocket); n == 0 || n > 103 {
		t.Errorf("AdminSocket %q is %d bytes long; a unix socket path holds at most 103", srv.AdminSocket, n)
	}
	// The media ports are the ones the server bound: ephemeral, and the ones its Addrs name.
	addrs := srv.Srv.Addrs()
	udp, _ := addrs.ICEUDP.(*net.UDPAddr)
	tcp, _ := addrs.ICETCP.(*net.TCPAddr)
	if srv.UDPPort == 0 || srv.TCPPort == 0 || udp == nil || tcp == nil || srv.UDPPort != udp.Port || srv.TCPPort != tcp.Port {
		t.Errorf("UDPPort = %d, TCPPort = %d, Addrs = %v and %v: want the bound ICE ports", srv.UDPPort, srv.TCPPort,
			addrs.ICEUDP, addrs.ICETCP)
	}
	if !strings.Contains(srv.Logs(), " ready: "+srv.URL) {
		t.Errorf("Logs() has no ready line:\n%s", srv.Logs())
	}
}

// TestMediaPortsOfAListenerThatIsOff: a media listener that the test turns off has port 0, the other one keeps its
// own.
func TestMediaPortsOfAListenerThatIsOff(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{Flags: []string{"--listen.ice-tcp="}})
	if srv.UDPPort == 0 || srv.TCPPort != 0 {
		t.Errorf("without listen.ice_tcp: UDPPort = %d, TCPPort = %d, want a port and 0", srv.UDPPort, srv.TCPPort)
	}
	srv = servertest.Start(t, servertest.Options{Flags: []string{"--listen.ice-udp="}})
	if srv.UDPPort != 0 || srv.TCPPort == 0 {
		t.Errorf("without listen.ice_udp: UDPPort = %d, TCPPort = %d, want 0 and a port", srv.UDPPort, srv.TCPPort)
	}
}

// TestClientReachesTheServerUnderAnyHost: Client dials the server's listener whatever the URL says, so a test can
// probe the Host check.
func TestClientReachesTheServerUnderAnyHost(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://elsewhere.example.net/", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := srv.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("GET / as elsewhere.example.net = %d, want the server's 421", res.StatusCode)
	}
}

// TestStop: Stop is graceful, idempotent, and leaves nothing listening.
func TestStop(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{})
	status(t, srv, "/healthz")
	srv.Stop(t)
	srv.Stop(t)
	if err := srv.Wait(t); err != nil {
		t.Errorf("Wait after Stop = %v, want nil", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := srv.Client.Do(req); err == nil {
		_ = res.Body.Close()
		t.Errorf("GET /healthz after Stop = %d, want a connection error", res.StatusCode)
	}
	if !strings.Contains(srv.Logs(), "shutdown complete") {
		t.Error("the server did not log a complete shutdown")
	}
}

// TestRestart: the new server has the same address, data directory and secrets; Cfg and Srv are new.
func TestRestart(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{})
	url, wsURL, dataDir, sock, old := srv.URL, srv.WSURL, srv.DataDir, srv.AdminSocket, srv.Srv
	secrets, err := os.ReadFile(filepath.Join(dataDir, "secrets.json"))
	if err != nil {
		t.Fatal(err)
	}

	srv.Restart(t)
	if srv.URL != url || srv.WSURL != wsURL || srv.DataDir != dataDir || srv.AdminSocket != sock {
		t.Errorf("after Restart: URL %q, WSURL %q, DataDir %q, AdminSocket %q changed", srv.URL, srv.WSURL, srv.DataDir, srv.AdminSocket)
	}
	if srv.Srv == old {
		t.Error("Restart kept the old Server")
	}
	if code, _ := status(t, srv, "/readyz"); code != 200 {
		t.Errorf("/readyz after Restart = %d", code)
	}
	after, err := os.ReadFile(filepath.Join(dataDir, "secrets.json"))
	if err != nil || string(after) != string(secrets) {
		t.Errorf("secrets.json changed across Restart (%v)", err)
	}
	// Restart also works on a server that already stopped.
	srv.Stop(t)
	srv.Restart(t)
	if code, _ := status(t, srv, "/readyz"); code != 200 {
		t.Errorf("/readyz after Stop and Restart = %d", code)
	}
	if n := strings.Count(srv.Logs(), " ready: "); n != 3 {
		t.Errorf("Logs() has %d ready lines, want 3 (it spans the restarts)", n)
	}
}

// TestWaitAfterServerShutdown: Wait collects Run's result when the shutdown began elsewhere.
func TestWaitAfterServerShutdown(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{})
	if err := srv.Srv.Shutdown(context.Background(), server.ShutdownRestore); err != nil {
		t.Fatal(err)
	}
	if err := srv.Wait(t); !errors.Is(err, server.ErrRestartRequested) {
		t.Errorf("Wait = %v, want ErrRestartRequested", err)
	}
	if err := srv.Wait(t); err != nil {
		t.Errorf("second Wait = %v, want nil", err)
	}
}

// TestStopAcceptsRestartRequest: a restart the test left uncollected is not a failure at cleanup.
func TestStopAcceptsRestartRequest(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{})
	if err := srv.Srv.Shutdown(context.Background(), server.ShutdownRestart); err != nil {
		t.Fatal(err)
	}
	srv.Stop(t) // Run returned ErrRestartRequested; Stop must not report it
}

// TestOptions: Flags count as the operator's values, Config changes the loaded config, DataDir is used as given.
func TestOptions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my-data")
	srv := servertest.Start(t, servertest.Options{
		Flags:   []string{"--registration.mode=closed", "--shutdown-timeout=3s"},
		Config:  func(c *config.Config) { c.Limits.WSHandshakesPerIPPerMinute = 1000 },
		DataDir: dir,
	})
	c := srv.Cfg
	if c.Registration.Mode != "closed" || !c.IsSet("registration.mode") {
		t.Errorf("registration.mode = %q, IsSet %v: a flag must set and pin it", c.Registration.Mode, c.IsSet("registration.mode"))
	}
	if c.ShutdownTimeout.Duration != 3*time.Second {
		t.Errorf("shutdown_timeout = %v", c.ShutdownTimeout.Duration)
	}
	if c.Limits.WSHandshakesPerIPPerMinute != 1000 || c.IsSet("limits.ws_handshakes_per_ip_per_minute") {
		t.Errorf("Config hook: value %d, IsSet %v: want the value changed and the key not set",
			c.Limits.WSHandshakesPerIPPerMinute, c.IsSet("limits.ws_handshakes_per_ip_per_minute"))
	}
	if srv.DataDir != dir {
		t.Errorf("DataDir = %q, want %q", srv.DataDir, dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "secrets.json")); err != nil {
		t.Errorf("the server did not use Options.DataDir: %v", err)
	}
}

// TestAdminSocket: the admin socket of a test server is live, and `admin log-level` works there without the test
// passing a level variable: the harness gives the server its own logger's (04 §12, §18).
func TestAdminSocket(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin := ops.DialAdmin(srv.AdminSocket)
	if h, err := admin.Ready(ctx); err != nil || h.Status != "ready" {
		t.Fatalf("ready over the admin socket: %+v, %v", h, err)
	}
	// A server without the variable refuses the command (ops.LogLevel is nil then).
	if err := admin.SetLogLevel(ctx, "info", time.Minute); err != nil {
		t.Fatalf("log-level info: %v", err)
	}
	if logs := srv.Logs(); !strings.Contains(logs, `"log_level":"info"`) {
		t.Errorf("the server did not log the change of its level:\n%s", logs)
	}
}

// TestTry: a server that can't start comes back as an error, not as a failed test, and leaves nothing running.
func TestTry(t *testing.T) {
	first := servertest.Start(t, servertest.Options{})

	srv, err := servertest.Try(t, servertest.Options{DataDir: first.DataDir})
	if srv != nil || !errors.Is(err, config.ErrDataDirLocked) {
		t.Fatalf("Try on a data directory in use = %v, %v; want nil and ErrDataDirLocked", srv, err)
	}

	// An invalid config is server.New's error.
	srv, err = servertest.Try(t, servertest.Options{Config: func(c *config.Config) { c.DataDir = "" }})
	var ve *config.ValidationError
	if srv != nil || !errors.As(err, &ve) || !server.NeedsOperator(err) {
		t.Fatalf("Try with an empty data_dir = %v, %v; want a *config.ValidationError", srv, err)
	}
	if _, statErr := os.Stat(first.DataDir); statErr != nil {
		t.Errorf("the first server's data directory: %v", statErr)
	}
	if code, _ := status(t, first, "/readyz"); code != 200 {
		t.Errorf("the first server's /readyz = %d", code)
	}
}

// TestInProcess: the harness never changes the test binary's umask (the real server sets 0077, 04 §5.1), so files
// that other tests create keep their modes.
func TestInProcess(t *testing.T) {
	dir := t.TempDir()
	before := filepath.Join(dir, "before")
	if err := os.WriteFile(before, nil, 0o666); err != nil { //nolint:gosec // G306: the umask decides, which is the test
		t.Fatal(err)
	}
	srv := servertest.Start(t, servertest.Options{})
	after := filepath.Join(dir, "after")
	if err := os.WriteFile(after, nil, 0o666); err != nil { //nolint:gosec // G306: as above
		t.Fatal(err)
	}
	srv.Stop(t)
	mode := func(path string) fs.FileMode {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Mode().Perm()
	}
	if mode(before) != mode(after) {
		t.Errorf("a file created while the server ran has mode %v, before it %v: the umask changed", mode(after), mode(before))
	}
}
