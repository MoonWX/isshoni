package server_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// The graceful shutdown of 04 §6.4 as the clients see it (04 §17 "Graceful shutdown"; README S90). The order of
// the steps inside the server is TestShutdownOrder's, the budgets are TestShutdownReturnsInTime's and
// TestSFUShutdownBudget's, and a shutdown under live media is itest's TestShutdownWithMedia.

// goodbye is what one connection got from the server once the shutdown began.
type goodbye struct {
	mu       sync.Mutex
	shutdown []protocol.ServerShutdown
	errs     []protocol.Error
	order    []protocol.MessageType // server.shutdown and error, in the order they came
	closed   websocket.StatusCode   // the close code; -1 for a close without a frame
	closedAt time.Duration          // after the start of the shutdown
	done     chan struct{}
}

// watchGoodbye reads c from now on, in a goroutine, and keeps what the server says before it closes the socket.
// begin is when the shutdown starts; it is read once the close has come.
func watchGoodbye(c *signaltest.Client, begin *time.Time, mu *sync.Mutex) *goodbye {
	g := &goodbye{done: make(chan struct{})}
	go func() {
		defer close(g.done)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for {
			env, err := c.Recv(ctx)
			if err != nil {
				break
			}
			g.mu.Lock()
			switch env.Type {
			case protocol.MessageTypeServerShutdown:
				if v, err := protocol.Decode[protocol.ServerShutdown](env); err == nil {
					g.shutdown = append(g.shutdown, v)
				}
				g.order = append(g.order, env.Type)
			case protocol.MessageTypeError:
				if v, err := protocol.Decode[protocol.Error](env); err == nil {
					g.errs = append(g.errs, v)
				}
				g.order = append(g.order, env.Type)
			}
			g.mu.Unlock()
		}
		code, err := c.CloseStatus(ctx)
		if err != nil {
			code = -1
		}
		mu.Lock()
		at := time.Since(*begin)
		mu.Unlock()
		g.mu.Lock()
		g.closed, g.closedAt = code, at
		g.mu.Unlock()
	}()
	return g
}

// TestShutdownWhatClientsSee: SIGTERM (Run's context ends) stops a server with two people in the Lounge, one of
// them sharing. Readiness says shutting_down at once, every REST call and new WebSocket gets 503 server_shutdown
// with Retry-After: 5, every connection gets server.shutdown (a restart, come back in a moment) and the
// connection's server_shutdown error and is closed with 1012 within 2 s, and Run returns well within
// shutdown_timeout, with nothing forced and no warning of the server's own. The package's leak check runs after.
func TestShutdownWhatClientsSee(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{Deps: wiredDeps()})
	setUp(t, srv)
	sam := register(t, srv, "Sam")
	alex := dialWS(t, srv)
	joinLounge(t, alex, signaltest.DefaultHello())
	samWS := dialWSAs(t, srv, sam)
	joinLounge(t, samWS, signaltest.DefaultHello())
	startShare(t, alex, "r1")

	var mu sync.Mutex
	var begin time.Time
	bye := map[string]*goodbye{"Alex": watchGoodbye(alex, &begin, &mu), "Sam": watchGoodbye(samWS, &begin, &mu)}

	// What new requests get in the moment between the gate closing and the hub's goodbye (steps 1 and 2): the hook
	// runs on Run's goroutine, so it only records; the test looks once Run has returned.
	type answer struct {
		what string
		res  response
		err  error
	}
	var during []answer
	srv.Srv.SetDrainingHook(func() {
		ask := func(what string, c *http.Client, method, path string, headers ...string) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, method, srv.URL+path, nil)
			if err != nil {
				during = append(during, answer{what: what, err: err})
				return
			}
			req.Header.Set("Origin", srv.URL)
			for i := 0; i+1 < len(headers); i += 2 {
				req.Header.Set(headers[i], headers[i+1])
			}
			res, err := c.Do(req)
			if err != nil {
				during = append(during, answer{what: what, err: err})
				return
			}
			defer func() { _ = res.Body.Close() }()
			var b strings.Builder
			buf := make([]byte, 4<<10)
			for {
				n, rerr := res.Body.Read(buf)
				b.Write(buf[:n])
				if rerr != nil {
					break
				}
			}
			during = append(during, answer{what: what, res: response{status: res.StatusCode, header: res.Header, body: b.String()}})
		}
		ask("GET /readyz", srv.Client, http.MethodGet, "/readyz")
		ask("GET /api/v1/me (Alex)", srv.Client, http.MethodGet, "/api/v1/me")
		ask("GET /api/v1/rooms (Sam)", sam, http.MethodGet, "/api/v1/rooms")
		ask("POST /api/v1/auth/logout (Sam)", sam, http.MethodPost, "/api/v1/auth/logout", "Content-Type", "application/json")
		ask("GET / (the web app)", sam, http.MethodGet, "/")
		ask("a new WebSocket (Sam)", sam, http.MethodGet, "/ws", "Connection", "Upgrade", "Upgrade", "websocket",
			"Sec-WebSocket-Version", "13", "Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	})

	mu.Lock()
	begin = time.Now()
	mu.Unlock()
	srv.Stop(t) // what SIGTERM does: Run's context ends. Stop fails the test if Run returns an error
	took := time.Since(begin)
	if took > 3*time.Second {
		t.Errorf("the shutdown took %v with two connections and a share; shutdown_timeout is %v", took, srv.Cfg.ShutdownTimeout.Duration)
	}

	if len(during) != 6 {
		t.Fatalf("the draining hook made %d requests, want 6", len(during))
	}
	for _, a := range during {
		switch {
		case a.err != nil:
			t.Errorf("%s while shutting down: %v", a.what, a.err)
		case a.what == "GET /readyz":
			if a.res.status != http.StatusServiceUnavailable || !strings.HasPrefix(a.res.body, `{"status":"shutting_down"`) {
				t.Errorf("%s while shutting down = %d %q, want 503 shutting_down", a.what, a.res.status, a.res.body)
			}
		default:
			if a.res.status != http.StatusServiceUnavailable || errorCode(a.res.body) != api.CodeServerShutdown ||
				a.res.header.Get("Retry-After") != "5" {
				t.Errorf("%s while shutting down = %d %q (Retry-After %q), want 503 server_shutdown with Retry-After 5",
					a.what, a.res.status, a.res.body, a.res.header.Get("Retry-After"))
			}
		}
	}

	for name, g := range bye {
		select {
		case <-g.done:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s's socket was not closed", name)
		}
		g.mu.Lock()
		if len(g.shutdown) != 1 || g.shutdown[0].Reason != protocol.ShutdownReasonRestart ||
			g.shutdown[0].ReconnectInMs < 500 || g.shutdown[0].ReconnectInMs > 3000 {
			t.Errorf("%s: server.shutdown = %+v, want one that announces a restart", name, g.shutdown)
		}
		var shutdownErr bool
		for _, e := range g.errs {
			if e.Code == protocol.ErrorCodeServerShutdown && e.Scope == protocol.ErrorScopeConnection && e.Retryable {
				shutdownErr = true
			}
		}
		if !shutdownErr {
			t.Errorf("%s: errors %+v, want server_shutdown for the connection, retryable", name, g.errs)
		}
		if len(g.order) == 0 || g.order[0] != protocol.MessageTypeServerShutdown {
			t.Errorf("%s: the goodbye came as %v, want server.shutdown first", name, g.order)
		}
		if g.closed != websocket.StatusCode(protocol.CloseCodeServiceRestart) {
			t.Errorf("%s: close code %d, want 1012", name, g.closed)
		}
		if g.closedAt > 2*time.Second {
			t.Errorf("%s: the socket closed %v after the shutdown began, want within 2 s", name, g.closedAt)
		}
		g.mu.Unlock()
	}

	// The server's own account: the shutdown ran to its end, nothing was forced, nothing was worth a warning.
	// (Pion's lines carry a scope; the machine's own warnings are hostWarning's.)
	var complete bool
	for _, rec := range logRecords(t, srv.Logs()) {
		if level := rec["level"]; rec["scope"] == nil && (level == "ERROR" || level == "WARN") && !hostWarning(rec) {
			t.Errorf("the server logged %v", rec)
		}
		complete = complete || rec["msg"] == "shutdown complete"
	}
	if !complete {
		t.Error("the log has no \"shutdown complete\"")
	}
}

// hostWarning reports whether a log record is one of the warnings a server makes about the machine it runs on, not
// about itself (as in itest): UDP buffers smaller than asked for (stock Linux sysctls), the data-volume check that
// a test in a container turns off, and a database write that a loaded machine made slow.
func hostWarning(rec map[string]any) bool {
	msg, _ := rec["msg"].(string)
	switch rec["component"] {
	case "store":
		return msg == "slow write transaction"
	case "netx":
		return strings.HasPrefix(msg, "UDP socket buffers are smaller than network.udp_buffer_bytes")
	case "config":
		return strings.HasPrefix(msg, "the container data-volume check is off")
	}
	return false
}
