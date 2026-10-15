package server_test

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server"
	"github.com/MoonWX/isshoni/internal/server/httpapi"
	"github.com/MoonWX/isshoni/internal/server/servertest"
)

// Hardening of the running server (README S90): the log lines of the 503s that the server gives on purpose, and the
// per-IP connection limit of the off-mode listener.

// failedLines returns the router's "http request failed" records by level.
func failedLines(t *testing.T, srv *servertest.Server) map[string][]map[string]any {
	t.Helper()
	out := map[string][]map[string]any{}
	for _, rec := range logRecords(t, srv.Logs()) {
		if rec["msg"] == "http request failed" {
			level, _ := rec["level"].(string)
			out[level] = append(out[level], rec)
		}
	}
	return out
}

// TestExpected503sAreQuiet: httpapi logs a warning for every 5xx whose cause no handler logged. A not-ready
// /readyz, which a monitor polls, and the 503s of the shutdown are what the server answers on purpose: those lines
// are debug lines. Every other 5xx keeps its warning.
func TestExpected503sAreQuiet(t *testing.T) {
	busy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpapi.WriteError(w, r, &api.Error{Code: api.CodeServerBusy, RetryAfter: 1})
	})
	srv := servertest.Start(t, servertest.Options{Deps: server.Deps{API: busy, SPA: testSPA()}})
	srv.Srv.Health().AddCheck("canary", func() (bool, string) { return false, "not yet" })
	for range 5 {
		if res := get(t, srv, "/readyz"); res.status != http.StatusServiceUnavailable {
			t.Fatalf("/readyz with a failing check = %d", res.status)
		}
	}
	if res := get(t, srv, "/api/v1/anything"); res.status != http.StatusServiceUnavailable || errorCode(res.body) != api.CodeServerBusy {
		t.Fatalf("the stub API = %d %q", res.status, res.body)
	}

	// The shutdown's 503s: the gate answers every request in the draining window.
	srv.Srv.SetDrainingHook(func() {
		for _, path := range []string{"/", "/api/v1/anything", "/readyz", "/healthz"} {
			if res, err := tryDo(srv, http.MethodGet, path); err != nil || res.status != http.StatusServiceUnavailable {
				t.Errorf("GET %s while shutting down = %d, %v", path, res.status, err)
			}
		}
	})
	srv.Stop(t)

	lines := failedLines(t, srv)
	warned := lines["WARN"]
	if len(warned) != 1 || warned[0]["route"] != "/api/v1/" || warned[0]["status"] != float64(http.StatusServiceUnavailable) {
		t.Errorf("warnings about failed requests: %v; want one, server_busy's", warned)
	}
	debug := map[string]int{}
	for _, rec := range lines["DEBUG"] {
		route, _ := rec["route"].(string)
		debug[route]++
	}
	// Five polls and one in the shutdown for /readyz, one /healthz, and the gate's answers to the app and the API.
	if debug["GET /readyz"] != 6 || debug["GET /healthz"] != 1 || debug["/api/v1/"] != 1 || len(lines["DEBUG"]) != 9 {
		t.Errorf("debug lines about failed requests by route: %v", debug)
	}
	if len(lines["ERROR"]) != 0 {
		t.Errorf("errors about failed requests: %v", lines["ERROR"])
	}
}

// TestOffModeConnectionLimit: in off mode the app's own listener has limits.conns_per_ip too (04 §7.8), for everyone
// but the trusted proxies, whose connections carry every client of the site.
func TestOffModeConnectionLimit(t *testing.T) {
	t.Run("a client that is no proxy", func(t *testing.T) {
		// The loopback peer of the test is no trusted proxy here.
		srv := servertest.Start(t, servertest.Options{Flags: []string{"--limits.conns-per-ip=3", "--network.trusted-proxies=192.0.2.0/24"}})
		addr := srv.Srv.Addrs().HTTP
		// The harness's own keep-alive connections count too: they go first.
		srv.Client.CloseIdleConnections()
		hold := func() []net.Conn {
			held := make([]net.Conn, 3)
			for i := range held {
				held[i] = dialRaw(t, addr)
			}
			return held
		}
		var held []net.Conn
		waitFor(t, "the server to accept three connections", func() bool {
			held = hold()
			deadline := time.Now().Add(time.Second)
			for srv.Srv.PendingConns() < 3 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if srv.Srv.PendingConns() >= 3 {
				return true
			}
			for _, c := range held {
				_ = c.Close()
			}
			return false
		})
		extra := dialRaw(t, addr)
		if _, err := io.WriteString(extra, "GET /healthz HTTP/1.1\r\nHost: localhost\r\n\r\n"); err == nil {
			_ = extra.SetReadDeadline(time.Now().Add(5 * time.Second))
			if n, err := extra.Read(make([]byte, 16)); n != 0 || errors.Is(err, os.ErrDeadlineExceeded) {
				t.Errorf("the connection over the limit got an answer or stayed open (%d bytes, %v)", n, err)
			}
		}
		_ = held[0].Close()
		waitFor(t, "a request to get through after a connection closed", func() bool {
			return healthzOver(addr) == http.StatusOK
		})
		for _, c := range held[1:] {
			_ = c.Close()
		}
	})
	t.Run("a trusted proxy", func(t *testing.T) {
		// The default of a loopback listen.http: the loopback proxies are trusted (04 §4.4).
		srv := servertest.Start(t, servertest.Options{Flags: []string{"--limits.conns-per-ip=3"}})
		addr := srv.Srv.Addrs().HTTP
		for range 5 {
			c := dialRaw(t, addr)
			t.Cleanup(func() { _ = c.Close() })
		}
		waitFor(t, "the server to accept the five connections", func() bool { return srv.Srv.PendingConns() >= 5 })
		if code := healthzOver(addr); code != http.StatusOK {
			t.Errorf("a request from the proxy with five connections open = %d, want 200", code)
		}
	})
}

// healthzOver sends GET /healthz on a connection of its own and returns the status, or 0 when there was none.
func healthzOver(addr net.Addr) int {
	c, err := tryDial(addr)
	if err != nil {
		return 0
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.WriteString(c, "GET /healthz HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"); err != nil {
		return 0
	}
	res, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return 0
	}
	_ = res.Body.Close()
	return res.StatusCode
}
