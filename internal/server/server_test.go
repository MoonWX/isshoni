package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server"
	"github.com/MoonWX/isshoni/internal/server/auth"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/httpapi"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/version"
	"github.com/MoonWX/isshoni/web"
)

const (
	indexHTML = "<!doctype html><html><head><title>isshoni</title></head><body><div id=root></div></body></html>\n"
	appJS     = "assets/index-3f2a1b9c.js"

	// forwardedFor is a client address as a trusted proxy on this host would forward it. In off mode with a loopback
	// listen.http the loopback proxies are trusted by default (04 §4.4), so the server sees this as the client.
	forwardedFor = "203.0.113.9"

	// checksOK is the "checks" object of /readyz on a ready off-mode server, as loopback clients see it: the
	// database, the hub, and the certificate, which is the proxy's in off mode (04 §6.2). The media check joins them
	// with the SFU.
	checksOK = `"checks":{"db":"ok","signal":"ok","tls":"ok"}`
)

// testArgon is a password hash that costs next to nothing (64 KiB, one pass) in place of the real one, which takes a
// good part of a second under the race detector. Tests that create accounts pass it as server.Deps.Argon.
var testArgon = auth.ArgonParams{MemoryKiB: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

// testSPA is a small built web app as Vite lays it out (05 §17).
func testSPA() fstest.MapFS {
	file := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	return fstest.MapFS{
		".gitkeep":             file(""),
		"index.html":           file(indexHTML),
		appJS:                  file("console.log('app')\n"),
		appJS + ".br":          file("BR:js"),
		"sw.js":                file("self.addEventListener('fetch', () => {})\n"),
		"manifest.webmanifest": file(`{"name":"isshoni"}`),
		"version.json":         file(`{"version":"` + version.Version() + `"}`),
		"icons/icon-192.png":   file("\x89PNG"),
	}
}

// response is what the tests look at.
type response struct {
	status int
	header http.Header
	body   string
}

// do sends one request to srv. headers are name, value pairs; a "Host" pair sets the request's Host.
func do(t *testing.T, srv *servertest.Server, method, path string, headers ...string) response {
	t.Helper()
	res, err := tryDo(srv, method, path, headers...)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return res
}

func tryDo(srv *servertest.Server, method, path string, headers ...string) (response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, srv.URL+path, nil)
	if err != nil {
		return response{}, err
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i] == "Host" {
			req.Host = headers[i+1]
			continue
		}
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := srv.Client.Do(req)
	if err != nil {
		return response{}, err
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return response{}, err
	}
	return response{status: res.StatusCode, header: res.Header, body: string(b)}, nil
}

func get(t *testing.T, srv *servertest.Server, path string, headers ...string) response {
	t.Helper()
	return do(t, srv, http.MethodGet, path, headers...)
}

// wantJSON checks a JSON response's status, body and the headers every JSON answer of the router carries.
func wantJSON(t *testing.T, what string, res response, status int, body string) {
	t.Helper()
	if res.status != status || res.body != body {
		t.Errorf("%s = %d %q, want %d %q", what, res.status, res.body, status, body)
	}
	if got := res.header.Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("%s: Content-Type = %q", what, got)
	}
	if got := res.header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("%s: Cache-Control = %q, want no-store", what, got)
	}
}

// errorCode returns the code of a REST error envelope, or "" when the body is not one.
func errorCode(body string) string {
	var env api.ErrorResponse
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return ""
	}
	return env.Error.Code
}

// logRecords parses the server's JSON log lines.
func logRecords(t *testing.T, logs string) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(strings.NewReader(logs))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("log line %q is not JSON: %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	return out
}

// TestStartIsReadyWithinASecond is the boot check of 04 §17: tls.mode = off, ephemeral ports, ready in under 1 s.
// The best of three starts counts, so a busy CI machine doesn't fail it; a normal start takes some tens of
// milliseconds: a new database with its schema, the keys, the listeners. The password hash that the account service
// computes at its start has the tests' cost here: at the real cost it takes 25 ms in the shipped binary and most of
// the second under the race detector.
func TestStartIsReadyWithinASecond(t *testing.T) {
	var took []time.Duration
	for range 3 {
		begin := time.Now()
		srv := servertest.Start(t, servertest.Options{Deps: server.Deps{Argon: testArgon}})
		d := time.Since(begin)
		took = append(took, d)

		// Start returned, so /readyz has answered 200 once; it still does.
		wantJSON(t, "/readyz", get(t, srv, "/readyz", "X-Forwarded-For", forwardedFor), 200, `{"status":"ready"}`+"\n")
		srv.Stop(t)
		if d < time.Second {
			t.Logf("ready after %v", d)
			return
		}
	}
	t.Errorf("servertest.Start took %v, want under 1 s at least once", took)
}

// TestHealthEndpoints checks /healthz and /readyz on the wire against 04 §11.1.
func TestHealthEndpoints(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{})

	t.Run("public", func(t *testing.T) {
		for path, want := range map[string]string{
			"/healthz": `{"status":"ok"}` + "\n",
			"/readyz":  `{"status":"ready"}` + "\n",
		} {
			res := get(t, srv, path, "X-Forwarded-For", forwardedFor)
			wantJSON(t, path, res, 200, want)
			if id := res.header.Get("X-Request-Id"); !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(id) {
				t.Errorf("%s: X-Request-Id = %q, want 16 hex characters", path, id)
			}
			// The router's security headers for a response that is not HTML (04 §9.6).
			for name, value := range map[string]string{
				"Content-Security-Policy":      "default-src 'none'; frame-ancestors 'none'",
				"X-Content-Type-Options":       "nosniff",
				"Referrer-Policy":              "no-referrer",
				"Cross-Origin-Resource-Policy": "same-origin",
				"X-Robots-Tag":                 "noindex",
			} {
				if got := res.header.Get(name); got != value {
					t.Errorf("%s: %s = %q, want %q", path, name, got, value)
				}
			}
			if got := res.header.Get("Strict-Transport-Security"); got != "" {
				t.Errorf("%s: Strict-Transport-Security = %q on plain HTTP", path, got)
			}
		}
	})

	t.Run("loopback clients see the checks", func(t *testing.T) {
		// No X-Forwarded-For: the client is the TCP peer, 127.0.0.1. Off mode's tls check always passes (04 §6.2).
		wantJSON(t, "/readyz", get(t, srv, "/readyz"), 200, `{"status":"ready",`+checksOK+`}`+"\n")
		wantJSON(t, "/healthz", get(t, srv, "/healthz"), 200, `{"status":"ok"}`+"\n")
	})

	t.Run("a forwarded request never sees the checks", func(t *testing.T) {
		// Behind the trusted proxy on this host, each of these X-Forwarded-For values makes the client address a
		// loopback one: an entry that doesn't parse leaves the proxy's own address, and a proxy that passes the
		// header on unchanged lets the client name any address. The request came through a proxy all the same.
		for _, xff := range []string{"garbage", "127.0.0.1", "::1", forwardedFor + ", 127.0.0.1", ""} {
			wantJSON(t, "/readyz with X-Forwarded-For: "+xff, get(t, srv, "/readyz", "X-Forwarded-For", xff), 200,
				`{"status":"ready"}`+"\n")
		}
		// The other forwarding headers count for nothing in the client address (04 §8.5), but they show a proxy too.
		for name, value := range map[string]string{
			"Forwarded":         "for=127.0.0.1;proto=https",
			"X-Forwarded-Proto": "https",
			"X-Forwarded-Host":  "watch.example.com",
			"X-Real-IP":         "127.0.0.1",
			"Via":               "1.1 proxy.example.net",
		} {
			wantJSON(t, "/readyz with "+name, get(t, srv, "/readyz", name, value), 200, `{"status":"ready"}`+"\n")
		}
	})

	t.Run("HEAD", func(t *testing.T) {
		for _, path := range []string{"/healthz", "/readyz"} {
			res := do(t, srv, http.MethodHead, path)
			if res.status != 200 || res.body != "" {
				t.Errorf("HEAD %s = %d %q, want 200 without a body", path, res.status, res.body)
			}
		}
	})

	t.Run("no Host check", func(t *testing.T) {
		// A monitor may ask by IP address or any name; every other route answers 421 to a foreign Host (04 §9.3).
		for _, path := range []string{"/healthz", "/readyz"} {
			if res := get(t, srv, path, "Host", "monitor.example.net"); res.status != 200 {
				t.Errorf("%s with a foreign Host = %d, want 200", path, res.status)
			}
		}
		if res := get(t, srv, "/", "Host", "monitor.example.net"); res.status != http.StatusMisdirectedRequest {
			t.Errorf("/ with a foreign Host = %d, want 421", res.status)
		}
	})

	t.Run("other methods", func(t *testing.T) {
		for _, path := range []string{"/healthz", "/readyz"} {
			res := do(t, srv, http.MethodPost, path)
			if res.status != 404 || errorCode(res.body) != api.CodeNotFound {
				t.Errorf("POST %s = %d %q, want 404 not_found", path, res.status, res.body)
			}
		}
	})

	t.Run("not ready", func(t *testing.T) {
		var mu sync.Mutex
		mediaUp := false
		srv.Srv.Health().AddCheck("media", func() (bool, string) {
			mu.Lock()
			defer mu.Unlock()
			return mediaUp, "waiting: no UDP socket is bound"
		})
		wantJSON(t, "public /readyz", get(t, srv, "/readyz", "X-Forwarded-For", forwardedFor), 503,
			`{"status":"not_ready"}`+"\n")
		wantJSON(t, "loopback /readyz", get(t, srv, "/readyz"), 503,
			`{"status":"not_ready","checks":{"db":"ok","media":"waiting: no UDP socket is bound","signal":"ok","tls":"ok"}}`+"\n")
		// Liveness doesn't depend on the checks.
		wantJSON(t, "/healthz", get(t, srv, "/healthz"), 200, `{"status":"ok"}`+"\n")

		mu.Lock()
		mediaUp = true
		mu.Unlock()
		wantJSON(t, "/readyz", get(t, srv, "/readyz", "X-Forwarded-For", forwardedFor), 200, `{"status":"ready"}`+"\n")
	})
}

// TestReadyzNoDetailThroughUntrustedProxy: without trusted proxies X-Forwarded-For counts for nothing in the client
// address, so a request through a proxy on this host looks local. The header still shows that it came through a
// proxy: public responses carry no detail (04 §11.1).
func TestReadyzNoDetailThroughUntrustedProxy(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{Flags: []string{"--network.trusted-proxies="}})
	wantJSON(t, "/readyz through the proxy", get(t, srv, "/readyz", "X-Forwarded-For", forwardedFor), 200,
		`{"status":"ready"}`+"\n")
	// The operator's own request, straight to the port, gets the checks as ever.
	wantJSON(t, "/readyz from this machine", get(t, srv, "/readyz"), 200, `{"status":"ready",`+checksOK+`}`+"\n")
}

// TestGracefulShutdown walks 04 §6.4 on the HTTP side: readiness and liveness off, the gate's 503 for everything
// else while the listeners still accept, then the HTTP server stops and Run returns. TestHubShutdown has the hub's
// step, with a connection that is told to come back.
func TestGracefulShutdown(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{Deps: server.Deps{SPA: testSPA()}})
	if res := get(t, srv, "/"); res.status != 200 {
		t.Fatalf("GET / before the shutdown = %d", res.status)
	}

	draining, release := make(chan struct{}), make(chan struct{})
	srv.Srv.SetDrainingHook(func() {
		close(draining)
		<-release
	})
	shutdownErr := make(chan error, 1)
	begin := time.Now()
	go func() { shutdownErr <- srv.Srv.Shutdown(context.Background(), server.ShutdownStop) }()
	select {
	case <-draining:
	case <-time.After(5 * time.Second):
		t.Fatal("the shutdown did not reach its draining window")
	}

	// Step 1: both health endpoints answer 503 shutting_down, without the Host check or the gate in their way.
	wantJSON(t, "/readyz", get(t, srv, "/readyz", "X-Forwarded-For", forwardedFor), 503, `{"status":"shutting_down"}`+"\n")
	wantJSON(t, "/healthz", get(t, srv, "/healthz", "X-Forwarded-For", forwardedFor), 503, `{"status":"shutting_down"}`+"\n")
	// The hub's turn is step 3, after this window: its check still passes.
	wantJSON(t, "loopback /readyz", get(t, srv, "/readyz"), 503, `{"status":"shutting_down",`+checksOK+`}`+"\n")

	// Step 2: every other request gets 503 server_shutdown with Retry-After: 5 (04 §9.3 step 5).
	for _, path := range []string{"/", "/r/lounge", "/" + appJS, "/api/v1/info", "/ws"} {
		res := get(t, srv, path)
		if res.status != 503 || errorCode(res.body) != api.CodeServerShutdown {
			t.Errorf("GET %s while shutting down = %d %q, want 503 server_shutdown", path, res.status, res.body)
		}
		if got := res.header.Get("Retry-After"); got != "5" {
			t.Errorf("GET %s while shutting down: Retry-After = %q, want 5", path, got)
		}
	}

	close(release)
	select {
	case err := <-shutdownErr:
		if err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return")
	}
	if err := srv.Wait(t); err != nil {
		t.Errorf("Run after a stop = %v, want nil", err)
	}
	t.Logf("shutdown took %v", time.Since(begin))

	// Step 5 is done: nothing listens any more.
	if _, err := tryDo(srv, http.MethodGet, "/healthz"); err == nil {
		t.Error("GET /healthz after the shutdown succeeded")
	}
	// The lock is released: the data directory can be served again.
	srv.Restart(t)
	if res := get(t, srv, "/readyz"); res.status != 200 {
		t.Errorf("/readyz after the restart = %d", res.status)
	}
}

// TestShutdownLetsRequestsFinish: a request in flight when the shutdown begins gets its answer (04 §6.4 step 5), and
// Shutdown waits for it.
func TestShutdownLetsRequestsFinish(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-finish:
			_, _ = io.WriteString(w, "done")
		case <-r.Context().Done():
		}
	})
	srv := servertest.Start(t, servertest.Options{Deps: server.Deps{API: slow}})

	type result struct {
		res response
		err error
	}
	client := make(chan result, 1)
	go func() {
		res, err := tryDo(srv, http.MethodGet, "/api/v1/slow")
		client <- result{res, err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the request did not reach its handler")
	}

	shutdownErr := make(chan error, 1)
	go func() { shutdownErr <- srv.Srv.Shutdown(context.Background(), server.ShutdownStop) }()
	select {
	case err := <-shutdownErr:
		t.Fatalf("Shutdown returned (%v) while a request was still running", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(finish)
	select {
	case got := <-client:
		if got.err != nil || got.res.status != 200 || got.res.body != "done" {
			t.Errorf("the request in flight = %d %q, %v; want its answer", got.res.status, got.res.body, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request in flight got no answer")
	}
	select {
	case err := <-shutdownErr:
		if err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return after the last request finished")
	}
	if err := srv.Wait(t); err != nil {
		t.Errorf("Run = %v, want nil", err)
	}
}

// TestStopReturnsQuickly: an idle server stops at once (cancelling Run's context is what SIGTERM does).
func TestStopReturnsQuickly(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{})
	get(t, srv, "/healthz") // leaves an idle keep-alive connection behind
	begin := time.Now()
	srv.Stop(t)
	if d := time.Since(begin); d > 2*time.Second {
		t.Errorf("stopping an idle server took %v", d)
	}
	if !strings.Contains(srv.Logs(), `"msg":"shutdown complete"`) {
		t.Errorf("no \"shutdown complete\" line in the log:\n%s", srv.Logs())
	}
}

// hungRequest starts a server whose API never answers, with one request stuck in it. shutdownTimeout is the
// server's shutdown_timeout; 0 keeps the default of 10 s. The channel gets the client's error once the server has
// cut the connection (nil if the request was answered after all).
func hungRequest(t *testing.T, shutdownTimeout time.Duration) (*servertest.Server, <-chan error) {
	t.Helper()
	started := make(chan struct{})
	hang := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done() // ends when the server cuts the connection
	})
	srv := servertest.Start(t, servertest.Options{
		Deps: server.Deps{API: hang},
		Config: func(c *config.Config) {
			if shutdownTimeout > 0 {
				c.ShutdownTimeout = config.Duration{Duration: shutdownTimeout}
			}
		},
	})
	clientErr := make(chan error, 1)
	go func() {
		_, err := tryDo(srv, http.MethodGet, "/api/v1/hang")
		clientErr <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the request did not reach its handler")
	}
	return srv, clientErr
}

// wantCutOff checks that the hung request of hungRequest was cut off, not answered.
func wantCutOff(t *testing.T, clientErr <-chan error) {
	t.Helper()
	select {
	case err := <-clientErr:
		if err == nil {
			t.Error("the hung request got an answer")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the hung request was not cut off")
	}
}

// TestShutdownReturnsInTime: a request that never finishes can't hold the shutdown past shutdown_timeout. The
// connection is cut, Shutdown says so with ErrShutdownForced, and the server is stopped all the same.
func TestShutdownReturnsInTime(t *testing.T) {
	const timeout = 300 * time.Millisecond
	srv, clientErr := hungRequest(t, timeout)

	begin := time.Now()
	err := srv.Srv.Shutdown(context.Background(), server.ShutdownStop)
	took := time.Since(begin)
	if took < timeout || took > timeout+2*time.Second {
		t.Errorf("Shutdown took %v with shutdown_timeout = %v", took, timeout)
	}
	if !errors.Is(err, server.ErrShutdownForced) || !errors.Is(err, context.DeadlineExceeded) ||
		!strings.Contains(err.Error(), "http: requests still running were cut off") {
		t.Errorf("Shutdown = %v, want ErrShutdownForced with the http step's deadline error", err)
	}
	wantCutOff(t, clientErr)
	// After a stop, Run reports the same forced shutdown: the one error for which cmd/isshoni exits 0.
	if runErr := srv.Wait(t); !errors.Is(runErr, server.ErrShutdownForced) || server.NeedsOperator(runErr) ||
		errors.Is(runErr, server.ErrRestartRequested) {
		t.Errorf("Run = %v, want ErrShutdownForced and nothing else", runErr)
	}
	if !strings.Contains(srv.Logs(), `"msg":"shutdown finished by force"`) {
		t.Errorf("no \"shutdown finished by force\" line in the log:\n%s", srv.Logs())
	}
	// Stopped all the same: the data directory is free again.
	again := servertest.Start(t, servertest.Options{DataDir: srv.DataDir})
	again.Stop(t)
}

// TestShutdownHonorsCallerContext: the caller's context can cut the shutdown shorter than shutdown_timeout.
func TestShutdownHonorsCallerContext(t *testing.T) {
	srv, clientErr := hungRequest(t, 0) // shutdown_timeout = 10 s

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	begin := time.Now()
	err := srv.Srv.Shutdown(ctx, server.ShutdownStop)
	if took := time.Since(begin); took > 3*time.Second {
		t.Errorf("Shutdown took %v with a 200 ms context", took)
	}
	if !errors.Is(err, server.ErrShutdownForced) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Shutdown = %v, want ErrShutdownForced with a deadline error", err)
	}
	wantCutOff(t, clientErr)
	if err := srv.Wait(t); !errors.Is(err, server.ErrShutdownForced) {
		t.Errorf("Run = %v, want the forced shutdown's error", err)
	}
}

// TestForcedShutdownIsRunsResultOnlyAfterAStop: ErrShutdownForced in Run's result means "stopped, exit 0". A restart
// request and a dead listener keep their own results when their shutdown had to use force; the force is then in
// the log and in Shutdown's result.
func TestForcedShutdownIsRunsResultOnlyAfterAStop(t *testing.T) {
	const timeout = 300 * time.Millisecond
	forcedLine := `"msg":"shutdown finished by force"`

	t.Run("restart", func(t *testing.T) {
		srv, clientErr := hungRequest(t, timeout)
		if err := srv.Srv.Shutdown(context.Background(), server.ShutdownRestart); !errors.Is(err, server.ErrShutdownForced) {
			t.Errorf("Shutdown = %v, want ErrShutdownForced", err)
		}
		wantCutOff(t, clientErr)
		if err := srv.Wait(t); !errors.Is(err, server.ErrRestartRequested) || errors.Is(err, server.ErrShutdownForced) {
			t.Errorf("Run = %v, want ErrRestartRequested alone", err)
		}
		if !strings.Contains(srv.Logs(), forcedLine) {
			t.Errorf("the forced shutdown is not in the log:\n%s", srv.Logs())
		}
	})

	t.Run("listener failure", func(t *testing.T) {
		srv, clientErr := hungRequest(t, timeout)
		if err := srv.Srv.CloseHTTPListener(); err != nil {
			t.Fatal(err)
		}
		// Exit 1, so that systemd starts the server again: never the forced shutdown's exit 0.
		if err := srv.Wait(t); !errors.Is(err, net.ErrClosed) || errors.Is(err, server.ErrShutdownForced) {
			t.Errorf("Run = %v, want the listener's error alone", err)
		}
		wantCutOff(t, clientErr)
		if err := srv.Srv.Shutdown(context.Background(), server.ShutdownStop); !errors.Is(err, server.ErrShutdownForced) {
			t.Errorf("Shutdown after the run = %v, want the shutdown's own result, ErrShutdownForced", err)
		}
		if !strings.Contains(srv.Logs(), forcedLine) {
			t.Errorf("the forced shutdown is not in the log:\n%s", srv.Logs())
		}
	})
}

// TestShutdownClosesSilentConnections: a connection that has sent no request, or only a part of one, doesn't hold
// the shutdown (04 §6.4 step 5). net/http alone waits until such a connection is 5 s old, the whole budget of the
// step: a browser's spare connection or a port scanner would make every stop take 5 s and end by force.
func TestShutdownClosesSilentConnections(t *testing.T) {
	for name, sent := range map[string]string{
		"nothing sent":  "",
		"half a header": "GET /healthz HTTP/1.1\r\nHost: localhost\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			srv := servertest.Start(t, servertest.Options{})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var d net.Dialer
			conn, err := d.DialContext(ctx, "tcp", srv.Srv.Addrs().HTTP.String())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if _, err := io.WriteString(conn, sent); err != nil {
				t.Fatal(err)
			}
			// The server has accepted the connection and waits for its request. (The harness's own connection has
			// had its request answered.)
			waitFor(t, "the server to accept the connection", func() bool { return srv.Srv.PendingConns() == 1 })

			begin := time.Now()
			err = srv.Srv.Shutdown(context.Background(), server.ShutdownStop)
			if took := time.Since(begin); err != nil || took > time.Second {
				t.Errorf("Shutdown = %v after %v, want nil within a second", err, took)
			}
			if err := srv.Wait(t); err != nil {
				t.Errorf("Run = %v, want nil", err)
			}
			// The server closed the connection without an answer.
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if b, err := io.ReadAll(conn); len(b) != 0 || errors.Is(err, os.ErrDeadlineExceeded) {
				t.Errorf("the connection after the shutdown: read %q, %v; want it closed without an answer", b, err)
			}
		})
	}
}

// TestRestartRequested: a shutdown for a restart or a restore makes Run return ErrRestartRequested after the full
// graceful shutdown (04 §6.5); the harness's Restart stands in for the re-exec.
func TestRestartRequested(t *testing.T) {
	for _, reason := range []server.ShutdownReason{server.ShutdownRestart, server.ShutdownRestore} {
		t.Run(string(reason), func(t *testing.T) {
			srv := servertest.Start(t, servertest.Options{})
			url := srv.URL
			secrets, err := os.ReadFile(filepath.Join(srv.DataDir, "secrets.json"))
			if err != nil {
				t.Fatal(err)
			}

			if err := srv.Srv.Shutdown(context.Background(), reason); err != nil {
				t.Fatalf("Shutdown: %v", err)
			}
			if err := srv.Wait(t); !errors.Is(err, server.ErrRestartRequested) {
				t.Fatalf("Run = %v, want ErrRestartRequested", err)
			}

			old := srv.Srv
			srv.Restart(t)
			if srv.Srv == old {
				t.Error("Restart kept the old Server")
			}
			if srv.URL != url {
				t.Errorf("URL changed across the restart: %s, was %s", srv.URL, url)
			}
			wantJSON(t, "/readyz", get(t, srv, "/readyz", "X-Forwarded-For", forwardedFor), 200, `{"status":"ready"}`+"\n")
			after, err := os.ReadFile(filepath.Join(srv.DataDir, "secrets.json"))
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(secrets) {
				t.Error("secrets.json changed across the restart")
			}
		})
	}
}

// TestShutdownTwice: the first Shutdown decides; concurrent and later calls wait for it and return its result. A
// stopped Server can't be started again, and Run on it only repeats how it ended.
func TestShutdownTwice(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{})
	s := srv.Srv
	errs := make(chan error, 4)
	for range 4 {
		go func() { errs <- s.Shutdown(context.Background(), server.ShutdownStop) }()
	}
	for range 4 {
		select {
		case err := <-errs:
			if err != nil {
				t.Errorf("Shutdown: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a concurrent Shutdown did not return")
		}
	}
	// A later call with another reason changes nothing: Run still reports the stop.
	if err := s.Shutdown(context.Background(), server.ShutdownRestart); err != nil {
		t.Errorf("Shutdown after the shutdown: %v", err)
	}
	if err := srv.Wait(t); err != nil {
		t.Errorf("Run = %v, want nil: the first reason was a stop", err)
	}
	if err := s.Start(context.Background()); err == nil {
		t.Error("Start on a stopped server succeeded")
	}
	// Run starts nothing again either: it reports how this server ended, as it did the first time.
	if err := s.Run(context.Background()); err != nil {
		t.Errorf("Run on a server that served and was stopped = %v, want nil again", err)
	}
	if _, err := tryDo(srv, http.MethodGet, "/healthz"); err == nil {
		t.Error("the stopped server answers after another Run")
	}
}

// TestRunReportsEarlierShutdown: a caller that starts the server itself and calls Run afterwards, as servertest
// does in a goroutine, gets the shutdown's result from Run even when the shutdown began before Run did.
func TestRunReportsEarlierShutdown(t *testing.T) {
	start := func(t *testing.T) *server.Server {
		t.Helper()
		s, err := server.New(testConfig(t), discardLog(), testDeps())
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		return s
	}
	for reason, want := range map[server.ShutdownReason]error{
		server.ShutdownStop:    nil,
		server.ShutdownRestart: server.ErrRestartRequested,
		server.ShutdownRestore: server.ErrRestartRequested,
	} {
		t.Run("finished: "+string(reason), func(t *testing.T) {
			s := start(t)
			if err := s.Shutdown(context.Background(), reason); err != nil {
				t.Fatalf("Shutdown: %v", err)
			}
			for range 2 {
				if err := s.Run(context.Background()); !errors.Is(err, want) {
					t.Errorf("Run after a finished shutdown for %q = %v, want %v", reason, err, want)
				}
			}
		})
	}

	t.Run("still running", func(t *testing.T) {
		s := start(t)
		draining, release := make(chan struct{}), make(chan struct{})
		s.SetDrainingHook(func() {
			close(draining)
			<-release
		})
		shutdownErr := make(chan error, 1)
		go func() { shutdownErr <- s.Shutdown(context.Background(), server.ShutdownRestart) }()
		select {
		case <-draining:
		case <-time.After(5 * time.Second):
			t.Fatal("the shutdown did not reach its draining window")
		}

		runErr := make(chan error, 1)
		go func() { runErr <- s.Run(context.Background()) }()
		select {
		case err := <-runErr:
			t.Fatalf("Run returned (%v) before the shutdown finished", err)
		case <-time.After(100 * time.Millisecond):
		}
		close(release)
		select {
		case err := <-runErr:
			if !errors.Is(err, server.ErrRestartRequested) {
				t.Errorf("Run = %v, want ErrRestartRequested", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Run did not return after the shutdown")
		}
		if err := <-shutdownErr; err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	t.Run("listener failure", func(t *testing.T) {
		s := start(t)
		if err := s.CloseHTTPListener(); err != nil {
			t.Fatal(err)
		}
		// Whether Run finds the failed server still up or already shut down by an earlier Run, the result is the same.
		for range 2 {
			if err := s.Run(context.Background()); !errors.Is(err, net.ErrClosed) {
				t.Errorf("Run after the listener failed = %v, want the listener's error", err)
			}
		}
	})
}

// TestShutdownBeforeStart: giving up a Server that never started opens nothing and leaves it unusable.
func TestShutdownBeforeStart(t *testing.T) {
	cfg := testConfig(t)
	s, err := server.New(cfg, discardLog(), testDeps())
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Site(); got.Origin != "" || s.Addrs().HTTP != nil {
		t.Errorf("before Start: Site = %+v, Addrs = %+v, want zero values", got, s.Addrs())
	}
	if err := s.Shutdown(context.Background(), server.ShutdownStop); err != nil {
		t.Errorf("Shutdown before Start: %v", err)
	}
	if err := s.Start(context.Background()); err == nil {
		t.Error("Start after Shutdown succeeded")
	}
	// There is no run to report: Run refuses like Start.
	if err := s.Run(context.Background()); err == nil {
		t.Error("Run on a server that was shut down before it served returned nil")
	}
	if _, err := os.Stat(cfg.DataDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the data directory exists (%v): New and Shutdown must not create it", err)
	}
}

// TestRun runs the server the way cmd/isshoni does: New, then Run alone, which starts, serves until its context is
// cancelled, shuts down and returns nil.
func TestRun(t *testing.T) {
	cfg := testConfig(t)
	s, err := server.New(cfg, discardLog(), testDeps())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()

	addr := waitForAddr(t, s)
	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr.String()+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d", res.StatusCode)
	}

	// The site carries the bound port, not the configured 0.
	site := s.Site()
	_, port, _ := net.SplitHostPort(addr.String())
	if site.Origin != "http://localhost:"+port || site.Host != "localhost:"+port || !site.Dev || site.TLSMode != config.TLSOff {
		t.Errorf("Site = %+v, want the dev site on port %s", site, port)
	}
	// The caller's config is not changed by the server.
	if cfg.Listen.HTTP != "127.0.0.1:0" {
		t.Errorf("the caller's config now has listen.http = %q", cfg.Listen.HTTP)
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("Run = %v, want nil after its context was cancelled", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// TestRunReturnsStartError: Run on a server that can't start returns Start's error and leaves nothing behind.
func TestRunReturnsStartError(t *testing.T) {
	busy := listenLoopback(t)
	cfg := testConfig(t, "--listen.http="+busy.Addr().String())
	s, err := server.New(cfg, discardLog(), testDeps())
	if err != nil {
		t.Fatal(err)
	}
	err = s.Run(context.Background())
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("Run = %v, want the busy-port error", err)
	}
	// The failed start released the data-directory lock.
	lock, err := config.LockDataDir(context.Background(), cfg.Paths())
	if err != nil {
		t.Fatalf("the data directory is still locked after a failed start: %v", err)
	}
	_ = lock.Close()
}

// TestListenerFailureEndsRun: when the listener dies under the server, Run shuts down and returns the cause.
func TestListenerFailureEndsRun(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{})
	if err := srv.Srv.CloseHTTPListener(); err != nil {
		t.Fatal(err)
	}
	err := srv.Wait(t)
	if err == nil || !strings.Contains(err.Error(), "serving") || !errors.Is(err, net.ErrClosed) {
		t.Errorf("Run = %v, want the listener's error", err)
	}
	if !strings.Contains(srv.Logs(), "the server can't go on") {
		t.Error("the failure was not logged")
	}
}

// TestStartCancelled: a context that is already cancelled stops the startup before it opens anything.
func TestStartCancelled(t *testing.T) {
	cfg := testConfig(t)
	s, err := server.New(cfg, discardLog(), testDeps())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Run with a cancelled context = %v, want context.Canceled", err)
	}
	if server.NeedsOperator(err) {
		t.Error("a cancelled start counts as a refusal")
	}
}

// TestSPA: a built web app is served with its caching rules and its fallback (04 §9.5), through the real server.
func TestSPA(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{Deps: server.Deps{SPA: testSPA()}})
	_, port, _ := net.SplitHostPort(srv.Srv.Addrs().HTTP.String())

	t.Run("index", func(t *testing.T) {
		res := get(t, srv, "/")
		if res.status != 200 || res.body != indexHTML {
			t.Fatalf("GET / = %d %q", res.status, res.body)
		}
		for name, want := range map[string]string{
			"Content-Type":               "text/html; charset=utf-8",
			"Cache-Control":              "no-cache",
			"Cross-Origin-Opener-Policy": "same-origin",
			"X-Robots-Tag":               "noindex",
			"X-Content-Type-Options":     "nosniff",
		} {
			if got := res.header.Get(name); got != want {
				t.Errorf("GET /: %s = %q, want %q", name, got, want)
			}
		}
		// The page may open its WebSocket on this site: the dev site is plain HTTP, so ws://.
		csp := res.header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "connect-src 'self' ws://localhost:"+port+";") {
			t.Errorf("GET /: Content-Security-Policy = %q, want connect-src with ws://localhost:%s", csp, port)
		}
		if res.header.Get("ETag") == "" {
			t.Error("GET /: no ETag")
		}
		if again := get(t, srv, "/", "If-None-Match", res.header.Get("ETag")); again.status != http.StatusNotModified {
			t.Errorf("GET / with If-None-Match = %d, want 304", again.status)
		}
	})

	t.Run("fallback", func(t *testing.T) {
		// Client-side routes get index.html; the SPA router takes it from there.
		for _, path := range []string{"/r/lounge", "/login", "/admin/users", "/setup"} {
			res := get(t, srv, path)
			if res.status != 200 || res.body != indexHTML || res.header.Get("Cache-Control") != "no-cache" {
				t.Errorf("GET %s = %d %q (Cache-Control %q), want index.html", path, res.status, res.body, res.header.Get("Cache-Control"))
			}
		}
		if res := do(t, srv, http.MethodPost, "/r/lounge"); res.status != http.StatusMethodNotAllowed {
			t.Errorf("POST /r/lounge = %d, want 405", res.status)
		}
	})

	t.Run("assets", func(t *testing.T) {
		res := get(t, srv, "/"+appJS)
		if res.status != 200 || res.body != "console.log('app')\n" {
			t.Fatalf("GET /%s = %d %q", appJS, res.status, res.body)
		}
		if got := res.header.Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
			t.Errorf("asset Cache-Control = %q", got)
		}
		if got := res.header.Get("Content-Type"); got != "text/javascript; charset=utf-8" {
			t.Errorf("asset Content-Type = %q", got)
		}
		br := get(t, srv, "/"+appJS, "Accept-Encoding", "br, gzip")
		if br.body != "BR:js" || br.header.Get("Content-Encoding") != "br" {
			t.Errorf("asset with Accept-Encoding br = %q (Content-Encoding %q), want the .br sibling", br.body, br.header.Get("Content-Encoding"))
		}
		// A missing asset is a 404, never index.html: a stale hashed name after an upgrade must fail cleanly.
		for _, path := range []string{"/assets/index-00000000.js", "/favicon.ico", "/.gitkeep"} {
			miss := get(t, srv, path)
			if miss.status != 404 || strings.Contains(miss.body, "<html") || miss.header.Get("Cache-Control") != "no-store" {
				t.Errorf("GET %s = %d %q (Cache-Control %q), want a plain 404", path, miss.status, miss.body, miss.header.Get("Cache-Control"))
			}
		}
	})

	t.Run("root files", func(t *testing.T) {
		sw := get(t, srv, "/sw.js")
		if sw.status != 200 || sw.header.Get("Cache-Control") != "no-cache" {
			t.Errorf("GET /sw.js = %d, Cache-Control %q", sw.status, sw.header.Get("Cache-Control"))
		}
		if icon := get(t, srv, "/icons/icon-192.png"); icon.status != 200 || icon.header.Get("Cache-Control") != "public, max-age=86400" {
			t.Errorf("GET /icons/icon-192.png = %d, Cache-Control %q", icon.status, icon.header.Get("Cache-Control"))
		}
		// The SPA ships no robots.txt: the server keeps friend servers out of search engines.
		if robots := get(t, srv, "/robots.txt"); robots.status != 200 || robots.body != "User-agent: *\nDisallow: /\n" {
			t.Errorf("GET /robots.txt = %d %q", robots.status, robots.body)
		}
	})

	t.Run("reserved paths", func(t *testing.T) {
		// A path of the server's that no route handles answers JSON 404, never index.html: 03's API for what is
		// under /api/v1/, the router for the rest of /api.
		for _, path := range []string{"/api/v1/nothing", "/api/v2/x", "/api", "/api/"} {
			res := get(t, srv, path)
			if res.status != 404 || errorCode(res.body) != api.CodeNotFound {
				t.Errorf("GET %s = %d %q, want 404 not_found", path, res.status, res.body)
			}
			if got := res.header.Get("Cache-Control"); got != "no-store" {
				t.Errorf("GET %s: Cache-Control = %q, want no-store", path, got)
			}
		}
		// /ws is the hub's: a GET that is no WebSocket upgrade gets its refusal, not the web app.
		if res := get(t, srv, "/ws"); res.status != http.StatusUpgradeRequired || strings.Contains(res.body, "<html") {
			t.Errorf("GET /ws without an upgrade = %d %q, want 426", res.status, res.body)
		}
	})
}

// TestWebAppNotBuilt: with only dist/.gitkeep the server still runs and every page says the web app is not built
// (04 §9.5), with status 503; health and the reserved paths are unaffected.
func TestWebAppNotBuilt(t *testing.T) {
	check := func(t *testing.T, srv *servertest.Server) {
		t.Helper()
		for _, path := range []string{"/", "/r/lounge", "/setup"} {
			res := get(t, srv, path)
			if res.status != http.StatusServiceUnavailable || !strings.Contains(res.body, "Web UI not built") ||
				!strings.Contains(res.body, "task build:web") {
				t.Errorf("GET %s = %d %q, want the 503 \"Web UI not built\" page", path, res.status, res.body)
			}
			if got := res.header.Get("Content-Type"); got != "text/html; charset=utf-8" {
				t.Errorf("GET %s: Content-Type = %q", path, got)
			}
			if got := res.header.Get("Cache-Control"); got != "no-store" {
				t.Errorf("GET %s: Cache-Control = %q, want no-store", path, got)
			}
		}
		if res := get(t, srv, "/.gitkeep"); res.status != 404 {
			t.Errorf("GET /.gitkeep = %d, want 404", res.status)
		}
		if res := get(t, srv, "/assets/index-3f2a1b9c.js"); res.status != 404 {
			t.Errorf("GET a missing asset = %d, want 404", res.status)
		}
		wantJSON(t, "/readyz", get(t, srv, "/readyz", "X-Forwarded-For", forwardedFor), 200, `{"status":"ready"}`+"\n")
	}

	t.Run("a dist with only .gitkeep", func(t *testing.T) {
		srv := servertest.Start(t, servertest.Options{
			Deps: server.Deps{SPA: fstest.MapFS{".gitkeep": &fstest.MapFile{}}},
		})
		check(t, srv)
	})

	t.Run("the embedded dist", func(t *testing.T) {
		// Deps.SPA is nil: the server serves web.Dist(), what `go:embed all:dist` put into this test binary. The
		// repository commits only dist/.gitkeep, so that is the not-built page unless someone ran `task build:web`
		// in this checkout.
		srv := servertest.Start(t, servertest.Options{})
		if _, err := fs.Stat(web.Dist(), "index.html"); err == nil {
			if res := get(t, srv, "/"); res.status != 200 || !strings.Contains(res.header.Get("Content-Type"), "text/html") {
				t.Errorf("GET / with a built web/dist = %d (%s), want the app", res.status, res.header.Get("Content-Type"))
			}
			t.Skip("web/dist is built in this checkout: the not-built page is covered by the case above")
		}
		entries, err := fs.ReadDir(web.Dist(), ".")
		if err != nil || len(entries) != 1 || entries[0].Name() != ".gitkeep" {
			t.Logf("web/dist holds %v (%v), not just .gitkeep", entries, err)
		}
		check(t, srv)
	})
}

// TestRefusesToStart covers the refusals of 04 §6.3 that exist so far, and the runtime errors next to them.
// NeedsOperator separates the two: exit 78 (a restart can't help) from exit 1.
func TestRefusesToStart(t *testing.T) {
	t.Run("corrupt secrets.json", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "data")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		secrets := filepath.Join(dir, "secrets.json")
		if err := os.WriteFile(secrets, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := servertest.Try(t, servertest.Options{DataDir: dir})
		if !server.NeedsOperator(err) || config.ReasonOf(err) != config.ReasonSecretsCorrupt {
			t.Fatalf("Start = %v (reason %q), want a secrets_corrupt refusal", err, config.ReasonOf(err))
		}
		if !strings.Contains(err.Error(), "fix:") {
			t.Errorf("the refusal has no fix line: %v", err)
		}
		// The file is never regenerated (new keys would sign everyone out) ...
		if b, _ := os.ReadFile(secrets); string(b) != "{not json" {
			t.Errorf("the corrupt secrets.json was changed: %q", b)
		}
		// ... and the failed start let go of the data directory: once the operator has fixed the file, it starts.
		if err := os.Remove(secrets); err != nil {
			t.Fatal(err)
		}
		servertest.Start(t, servertest.Options{DataDir: dir})
	})

	t.Run("container without a data volume", func(t *testing.T) {
		// A fake machine: a container (the environment says so) whose mount table has nothing at the data directory.
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "proc", "self"), 0o700); err != nil {
			t.Fatal(err)
		}
		mountinfo := "1040 947 0:167 / / rw,relatime - overlay overlay rw\n1041 1040 0:170 / /proc rw - proc proc rw\n"
		if err := os.WriteFile(filepath.Join(root, "proc", "self", "mountinfo"), []byte(mountinfo), 0o600); err != nil {
			t.Fatal(err)
		}
		container := config.Host{Root: root, Environ: []string{config.EnvInContainer + "=1"}}

		_, err := servertest.Try(t, servertest.Options{Deps: server.Deps{Host: container}})
		if !server.NeedsOperator(err) || config.ReasonOf(err) != config.ReasonDataNotMounted {
			t.Fatalf("Start = %v (reason %q), want a data_not_mounted refusal", err, config.ReasonOf(err))
		}
		if !strings.Contains(err.Error(), "Mount a volume at") {
			t.Errorf("the refusal doesn't say how to mount a volume: %v", err)
		}

		// The escape hatch for tests (04 §5.1).
		container.Environ = append(container.Environ, config.EnvAllowEphemeralData+"=1")
		srv := servertest.Start(t, servertest.Options{Deps: server.Deps{Host: container}})
		if !strings.Contains(srv.Logs(), "the container data-volume check is off") {
			t.Error("skipping the data-volume check was not logged")
		}
	})

	t.Run("data directory in use", func(t *testing.T) {
		first := servertest.Start(t, servertest.Options{})
		_, err := servertest.Try(t, servertest.Options{DataDir: first.DataDir})
		if !errors.Is(err, config.ErrDataDirLocked) {
			t.Fatalf("a second server on the same data directory: %v, want ErrDataDirLocked", err)
		}
		if server.NeedsOperator(err) {
			t.Error("a locked data directory counts as a refusal; it is a runtime error (exit 1)")
		}
		// The first server is unharmed, and its shutdown frees the directory.
		if res := get(t, first, "/healthz"); res.status != 200 {
			t.Errorf("the first server's /healthz = %d", res.status)
		}
		first.Stop(t)
		servertest.Start(t, servertest.Options{DataDir: first.DataDir})
	})

	t.Run("port in use", func(t *testing.T) {
		busy := listenLoopback(t)
		_, port, _ := net.SplitHostPort(busy.Addr().String())
		dir := filepath.Join(t.TempDir(), "data")
		_, err := servertest.Try(t, servertest.Options{DataDir: dir, Flags: []string{"--listen.http=" + busy.Addr().String()}})
		if !errors.Is(err, syscall.EADDRINUSE) {
			t.Fatalf("Start on a busy port = %v, want EADDRINUSE", err)
		}
		if server.NeedsOperator(err) {
			t.Error("a busy port counts as a refusal; it is a runtime error (exit 1)")
		}
		for _, want := range []string{"port " + port + " is in use", "listen.http"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error %q doesn't mention %q", err, want)
			}
		}
		// Nothing is left behind: the same data directory starts on a free port.
		servertest.Start(t, servertest.Options{DataDir: dir})
	})
}

// TestNewRejects: New refuses what Start could never serve, without opening anything.
func TestNewRejects(t *testing.T) {
	t.Run("nil config", func(t *testing.T) {
		if _, err := server.New(nil, discardLog(), server.Deps{}); err == nil {
			t.Error("New(nil) succeeded")
		}
	})

	t.Run("invalid config", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.ShutdownTimeout = config.Duration{} // changed after Load
		_, err := server.New(cfg, discardLog(), server.Deps{})
		var ve *config.ValidationError
		if !errors.As(err, &ve) || !server.NeedsOperator(err) {
			t.Fatalf("New = %v, want a *config.ValidationError that needs the operator", err)
		}
		if !strings.Contains(err.Error(), "config error: shutdown_timeout") {
			t.Errorf("the error doesn't name the key: %v", err)
		}
	})

	t.Run("config with load errors", func(t *testing.T) {
		// Load keeps the default in place of a value it can't parse; New must still refuse.
		cfg := testConfig(t, "--limits.conns-per-ip=many")
		_, err := server.New(cfg, discardLog(), server.Deps{})
		var ve *config.ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("New = %v, want a *config.ValidationError", err)
		}
	})

	t.Run("a TLS mode without what it needs", func(t *testing.T) {
		for name, flags := range map[string][]string{
			"auto without a domain":   {"--tls.mode=auto"},
			"manual without files":    {"--tls.mode=manual", "--domain=watch.example.com"},
			"ip on a private address": {"--tls.mode=ip", "--public-ip=192.168.1.20"},
		} {
			_, err := server.New(testConfig(t, flags...), discardLog(), server.Deps{})
			var ve *config.ValidationError
			if !errors.As(err, &ve) || !server.NeedsOperator(err) {
				t.Errorf("%s: New = %v, want a *config.ValidationError that needs the operator", name, err)
			}
		}
	})
}

// TestNewAcceptsEveryTLSMode: New builds a server for each mode of 04 §8.1, and opens nothing for it: no listener,
// no certificate file, no ACME order.
func TestNewAcceptsEveryTLSMode(t *testing.T) {
	for mode, flags := range map[config.TLSMode][]string{
		config.TLSOff:    nil,
		config.TLSAuto:   {"--tls.mode=auto", "--domain=watch.example.com"},
		config.TLSIP:     {"--tls.mode=ip", "--public-ip=8.8.8.8"}, // a public address, as config's tests use
		config.TLSManual: {"--tls.mode=manual", "--domain=watch.example.com", "--tls.cert-file=/nowhere/cert.pem", "--tls.key-file=/nowhere/key.pem"},
	} {
		cfg := testConfig(t, flags...)
		if got := cfg.EffectiveTLSMode(); got != mode {
			t.Fatalf("flags %v give tls mode %q, want %q", flags, got, mode)
		}
		s, err := server.New(cfg, discardLog(), server.Deps{})
		if err != nil {
			t.Errorf("New with tls.mode %q = %v", mode, err)
			continue
		}
		if a := s.Addrs(); a.HTTP != nil || a.HTTPS != nil {
			t.Errorf("tls.mode %q: New bound %+v", mode, a)
		}
		if _, err := os.Stat(cfg.DataDir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("tls.mode %q: New touched the data directory (%v)", mode, err)
		}
	}
}

func TestNeedsOperator(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"nil":               {nil, false},
		"plain":             {errors.New("boom"), false},
		"locked data dir":   {config.ErrDataDirLocked, false},
		"busy port":         {syscall.EADDRINUSE, false},
		"restart requested": {server.ErrRestartRequested, false},
		"operator error":    {&config.OperatorError{Reason: config.ReasonSecretsOwner, Message: "x", Fix: "y"}, true},
		"wrapped sentinel":  {errors.Join(errors.New("ctx"), config.ErrNeedsOperator), true},
		"validation error":  {&config.ValidationError{}, true},
	} {
		if got := server.NeedsOperator(tc.err); got != tc.want {
			t.Errorf("NeedsOperator(%s) = %v, want %v", name, got, tc.want)
		}
	}
}

// TestConfigWarningsAreLogged: the config's warnings reach the server's log, one line each with the key and the
// fix (04 §4.5), instead of only cmd/isshoni's stderr.
func TestConfigWarningsAreLogged(t *testing.T) {
	// Off mode ignores listen.https; setting it is a warning.
	srv := servertest.Start(t, servertest.Options{Flags: []string{"--listen.https=:8443"}})
	var found map[string]any
	for _, rec := range logRecords(t, srv.Logs()) {
		if rec["key"] == "listen.https" {
			found = rec
		}
	}
	if found == nil {
		t.Fatalf("no log line for the listen.https warning:\n%s", srv.Logs())
	}
	if found["level"] != "WARN" || found["component"] != "config" {
		t.Errorf("warning line = %v, want level WARN and component config", found)
	}
	msg, _ := found["msg"].(string)
	if !strings.HasPrefix(msg, `config warning: listen.https = ":8443" (flag --listen.https)`) || strings.Contains(msg, "\n") {
		t.Errorf("warning msg = %q, want the one-line §4.5 form", msg)
	}
	if fix, _ := found["fix"].(string); fix == "" {
		t.Errorf("warning line has no fix: %v", found)
	}

	// A config without warnings logs none. (Other lines of the config component may be warnings: inside a container
	// the harness's "data-volume check is off" is one.)
	quiet := servertest.Start(t, servertest.Options{})
	for _, rec := range logRecords(t, quiet.Logs()) {
		if msg, _ := rec["msg"].(string); strings.HasPrefix(msg, "config warning") || rec["key"] != nil {
			t.Errorf("unexpected config warning: %v", rec)
		}
	}
}

// TestReadyLine: one line says which version serves which site (04 §6.1 step 10).
func TestReadyLine(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{})
	want := "isshoni " + version.Version() + " ready: " + srv.URL + " (tls=off)"
	var found bool
	for _, rec := range logRecords(t, srv.Logs()) {
		if rec["msg"] == want {
			found = true
			if rec["level"] != "INFO" || rec["listen"] != srv.Srv.Addrs().HTTP.String() {
				t.Errorf("ready line = %v", rec)
			}
		}
	}
	if !found {
		t.Errorf("no line %q in the log:\n%s", want, srv.Logs())
	}
}

// TestClientIPBehindProxy: the router gets the config's trusted proxies, so handlers see the real client behind a
// proxy on this host (04 §8.5) and the peer when no proxy is trusted.
func TestClientIPBehindProxy(t *testing.T) {
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		//nolint:gosec // G705: a test handler; an IP address and a bool as plain text
		_, _ = io.WriteString(w, httpapi.ClientIP(r).String()+" secure="+strconv.FormatBool(httpapi.IsSecure(r)))
	})

	t.Run("loopback proxies trusted by default", func(t *testing.T) {
		srv := servertest.Start(t, servertest.Options{Deps: server.Deps{API: echo}})
		if got := get(t, srv, "/api/v1/whoami", "X-Forwarded-For", forwardedFor).body; got != forwardedFor+" secure=true" {
			t.Errorf("behind the proxy: %q", got)
		}
		if got := get(t, srv, "/api/v1/whoami").body; got != "127.0.0.1 secure=true" {
			t.Errorf("direct: %q", got)
		}
	})

	t.Run("no trusted proxies", func(t *testing.T) {
		srv := servertest.Start(t, servertest.Options{
			Deps:  server.Deps{API: echo},
			Flags: []string{"--network.trusted-proxies="},
		})
		if got := get(t, srv, "/api/v1/whoami", "X-Forwarded-For", forwardedFor).body; got != "127.0.0.1 secure=true" {
			t.Errorf("X-Forwarded-For from an untrusted peer was honored: %q", got)
		}
	})

	t.Run("reverse-proxy install", func(t *testing.T) {
		// public_url on a real host name: not a dev site. The test plays the proxy.
		srv := servertest.Start(t, servertest.Options{
			Deps:  server.Deps{API: echo, SPA: testSPA()},
			Flags: []string{"--public-url=https://watch.example.com"},
		})
		site := srv.Srv.Site()
		if site.Origin != "https://watch.example.com" || site.Host != "watch.example.com" || site.Dev {
			t.Fatalf("Site = %+v", site)
		}
		if srv.URL != "http://watch.example.com" || srv.WSURL != "ws://watch.example.com/ws" {
			t.Errorf("URL = %q, WSURL = %q", srv.URL, srv.WSURL)
		}
		got := get(t, srv, "/api/v1/whoami", "X-Forwarded-For", forwardedFor, "X-Forwarded-Proto", "https").body
		if got != forwardedFor+" secure=true" {
			t.Errorf("behind the proxy with X-Forwarded-Proto: %q", got)
		}
		if got := get(t, srv, "/api/v1/whoami", "X-Forwarded-For", forwardedFor).body; got != forwardedFor+" secure=false" {
			t.Errorf("behind the proxy without X-Forwarded-Proto: %q", got)
		}
		// The Host check holds the site's name; localhost is no longer accepted.
		_, port, _ := net.SplitHostPort(srv.Srv.Addrs().HTTP.String())
		if res := get(t, srv, "/", "Host", "localhost:"+port); res.status != http.StatusMisdirectedRequest {
			t.Errorf("GET / with Host localhost = %d, want 421", res.status)
		}
		// The page's WebSocket goes to the public origin: wss.
		if csp := get(t, srv, "/").header.Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src 'self' wss://watch.example.com;") {
			t.Errorf("Content-Security-Policy = %q, want connect-src with wss://watch.example.com", csp)
		}
	})
}

// TestRunningServerLimits: the server that Start runs carries the limits of 04 §7.8, and enforces the header size.
func TestRunningServerLimits(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{})
	hs := srv.Srv.MainServer()
	if hs.ReadHeaderTimeout != 10*time.Second || hs.IdleTimeout != 120*time.Second || hs.MaxHeaderBytes != 16<<10 ||
		hs.ReadTimeout != 0 || hs.WriteTimeout != 0 {
		t.Errorf("main server: ReadHeaderTimeout %v, IdleTimeout %v, MaxHeaderBytes %d, ReadTimeout %v, WriteTimeout %v",
			hs.ReadHeaderTimeout, hs.IdleTimeout, hs.MaxHeaderBytes, hs.ReadTimeout, hs.WriteTimeout)
	}

	if res := get(t, srv, "/healthz", "X-Padding", strings.Repeat("a", 8<<10)); res.status != 200 {
		t.Errorf("a request with 8 KiB of headers = %d, want 200", res.status)
	}
	// Over 16 KiB (plus net/http's 4 KiB of slack) the server answers 431 and closes; a client still writing may see
	// the closed connection instead.
	res, err := tryDo(srv, http.MethodGet, "/healthz", "X-Padding", strings.Repeat("a", 64<<10))
	if err == nil && res.status != http.StatusRequestHeaderFieldsTooLarge {
		t.Errorf("a request with 64 KiB of headers = %d, want 431", res.status)
	}
}

// TestStubHandlers: Deps.API and Deps.WS are mounted in place of 03's API and 01's hub.
func TestStubHandlers(t *testing.T) {
	stub := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = io.WriteString(w, name+" "+r.URL.Path) //nolint:gosec // G705: a test handler; plain text
		})
	}
	srv := servertest.Start(t, servertest.Options{Deps: server.Deps{API: stub("api"), WS: stub("ws")}})
	if got := get(t, srv, "/api/v1/rooms").body; got != "api /api/v1/rooms" {
		t.Errorf("GET /api/v1/rooms = %q", got)
	}
	if got := get(t, srv, "/ws").body; got != "ws /ws" {
		t.Errorf("GET /ws = %q", got)
	}
	// Only GET reaches the hub.
	if res := do(t, srv, http.MethodPost, "/ws"); res.status != 404 {
		t.Errorf("POST /ws = %d, want 404", res.status)
	}
}

// TestDataDirectory: a first start creates the data directory, its lock file, secrets.json and the database, all
// private (04 §5.1, §5.2).
func TestDataDirectory(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{})
	for name, want := range map[string]fs.FileMode{
		"":             0o700 | fs.ModeDir,
		"isshoni.lock": 0o600,
		"secrets.json": 0o600,
		"isshoni.db":   0o600,
		"backups":      0o700 | fs.ModeDir,
	} {
		fi, err := os.Stat(filepath.Join(srv.DataDir, name))
		if err != nil {
			t.Errorf("%q: %v", name, err)
			continue
		}
		if got := fi.Mode() & (fs.ModePerm | fs.ModeDir); got != want {
			t.Errorf("%q has mode %v, want %v", name, got, want)
		}
	}
	// The admin socket is there, for the server's user alone (04 §12.1).
	if fi, err := os.Stat(srv.AdminSocket); err != nil || fi.Mode()&fs.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Errorf("the admin socket: %v, %v", fi, err)
	}
	// While the server runs, it holds the data-directory lock.
	if _, err := config.LockDataDir(context.Background(), srv.Cfg.Paths()); !errors.Is(err, config.ErrDataDirLocked) {
		t.Errorf("LockDataDir while the server runs = %v, want ErrDataDirLocked", err)
	}
}

// testConfig builds an off-mode config on an ephemeral loopback port from flags alone, for the tests that drive
// server.New themselves. Later flags win.
func testConfig(t *testing.T, flags ...string) *config.Config {
	t.Helper()
	sockDir, err := os.MkdirTemp("", "isshoni")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	args := append([]string{
		"--tls.mode=off",
		"--listen.http=127.0.0.1:0",
		"--listen.admin-socket=" + filepath.Join(sockDir, "admin.sock"),
		"--data-dir=" + filepath.Join(t.TempDir(), "data"),
	}, flags...)
	set := flag.NewFlagSet("test", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	cfg, err := config.LoadFlags(set, args)
	if cfg == nil {
		t.Fatalf("LoadFlags: %v", err)
	}
	return cfg
}

// testDeps are the Deps of a server that runs inside the test binary.
func testDeps() server.Deps {
	return server.Deps{
		InProcess: true,
		Host:      config.Host{Environ: append(os.Environ(), config.EnvAllowEphemeralData+"=1")},
		Argon:     testArgon,
	}
}

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// listenLoopback occupies a loopback port for the test.
func listenLoopback(t *testing.T) net.Listener {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// waitFor polls cond until it holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitForAddr waits until a server that Run is starting has bound its HTTP listener.
func waitForAddr(t *testing.T, s *server.Server) net.Addr {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if a := s.Addrs().HTTP; a != nil {
			return a
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the server did not bind its HTTP listener")
	return nil
}
