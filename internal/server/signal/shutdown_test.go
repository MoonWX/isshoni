package signal_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/signal"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// expectShutdown reads server.shutdown{reason, reconnectInMs}, then error{server_shutdown}, then close 1012.
func expectShutdown(t *testing.T, c *signaltest.Client, reason protocol.ShutdownReason) {
	t.Helper()
	ss, err := protocol.Decode[protocol.ServerShutdown](expectType(t, c, protocol.MessageTypeServerShutdown))
	if err != nil {
		t.Fatal(err)
	}
	if ss.Reason != reason || ss.ReconnectInMs < 500 || ss.ReconnectInMs > 3000 {
		t.Errorf("server.shutdown %+v, want reason %s and reconnectInMs in [500, 3000]", ss, reason)
	}
	pe := expectFail(t, c, protocol.ErrorCodeServerShutdown, protocol.ErrorScopeConnection)
	if !pe.Retryable {
		t.Error("server_shutdown is not retryable")
	}
}

// Shutdown (01 §19, 04 §6.4): every connection, and every socket still in its handshake, gets server.shutdown and
// error{server_shutdown} and 1012; Shutdown returns before its deadline; new upgrades get 503.
func TestShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookieA, _ := e.user(false)
		cookieB, _ := e.user(true)
		a, _ := e.connect(cookieA, signaltest.DefaultHello())
		b, _ := e.connect(cookieB, signaltest.DefaultHello())
		b2, _ := e.connect(cookieB, signaltest.DefaultHello())
		pending := e.mustDial(headers(cookieA, testOrigin, "")) // no hello yet

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		start := time.Now()
		if err := e.hub.Shutdown(ctx, protocol.ShutdownReasonRestart); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		if d := time.Since(start); d >= 2*time.Second {
			t.Errorf("Shutdown took %v", d)
		}
		for _, c := range []*signaltest.Client{a, b, b2, pending} {
			expectShutdown(t, c, protocol.ShutdownReasonRestart)
		}
		if e.hub.Ready() {
			t.Error("ready after Shutdown")
		}
		if conns, socks, preAuth, _ := signal.Counts(e.hub, ""); conns+socks+preAuth != 0 {
			t.Errorf("conns %d, sockets %d, pre-auth %d after Shutdown", conns, socks, preAuth)
		}
		if re := e.refused(headers(cookieA, testOrigin, "")); re.Status != http.StatusServiceUnavailable {
			t.Errorf("upgrade after Shutdown: %d, want 503", re.Status)
		}
		// A second call returns at once.
		if err := e.hub.Shutdown(ctx, protocol.ShutdownReasonRestart); err != nil {
			t.Errorf("second Shutdown: %v", err)
		}
		if got := e.metric("isshoni_ws_close_total", "code", "1012"); got != 3 {
			t.Errorf("isshoni_ws_close_total{code=1012} %v, want 3", got)
		}
	})
}

// The stop reason goes on the wire as is; an unknown reason is sent as restart.
func TestShutdownReasons(t *testing.T) {
	for _, tc := range []struct {
		give, want protocol.ShutdownReason
	}{
		{protocol.ShutdownReasonStop, protocol.ShutdownReasonStop},
		{"reboot", protocol.ShutdownReasonRestart},
	} {
		t.Run(string(tc.give), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t)
				defer e.close()
				cookie, _ := e.user(false)
				c, _ := e.connect(cookie, signaltest.DefaultHello())
				if err := e.hub.Shutdown(ctxT(t), tc.give); err != nil {
					t.Fatal(err)
				}
				expectShutdown(t, c, tc.want)
			})
		})
	}
}

// A client that doesn't read can't hold Shutdown past its deadline: the remaining sockets are closed without a
// handshake, and every goroutine of the hub ends.
func TestShutdownDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		stuck, _ := e.connect(cookie, signaltest.DefaultHello())
		fine, _ := e.connect(cookie, signaltest.DefaultHello())
		stuck.Pause()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		start := time.Now()
		err := e.hub.Shutdown(ctx, protocol.ShutdownReasonRestart)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Shutdown: %v, want the deadline", err)
		}
		if d := time.Since(start); d != time.Second {
			t.Errorf("Shutdown returned after %v, want 1s", d)
		}
		if conns, socks, _, _ := signal.Counts(e.hub, ""); conns+socks != 0 {
			t.Errorf("conns %d, sockets %d after a forced shutdown", conns, socks)
		}
		expectShutdown(t, fine, protocol.ShutdownReasonRestart)
	})
}
