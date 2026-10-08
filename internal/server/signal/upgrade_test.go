package signal_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/signal"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// withWails allows the M2 desktop app's webview origin (accepted only for bearer connections).
func withWails(c *signal.Config) { c.AllowedOrigins = []string{wailsOrigin} }

// The Origin matrix of 01 §19 (moved here from 03).
func TestUpgradeOriginMatrix(t *testing.T) {
	cases := []struct {
		name   string
		origin []string // Origin header values
		cookie bool
		ok     bool
	}{
		{"public origin", []string{testOrigin}, true, true},
		{"host case differs", []string{"https://WATCH.Example.COM"}, true, true},
		{"scheme case differs", []string{"HTTPS://watch.example.com"}, true, true},
		{"explicit default port", []string{"https://watch.example.com:443"}, true, true},
		{"missing origin with cookie (non-browser)", nil, true, true},
		{"missing origin without cookie (pre-auth)", nil, false, true},
		{"other host", []string{"https://evil.example.com"}, true, false},
		{"other scheme", []string{"http://watch.example.com"}, true, false},
		{"other port", []string{"https://watch.example.com:8443"}, true, false},
		{"subdomain", []string{"https://a.watch.example.com"}, true, false},
		{"opaque origin", []string{"null"}, true, false},
		{"empty origin", []string{""}, true, false},
		{"with a path", []string{testOrigin + "/"}, true, false},
		{"two origins", []string{testOrigin, testOrigin}, true, false},
		{"wails origin with cookie", []string{wailsOrigin}, true, false},
		{"wails origin without cookie", []string{wailsOrigin}, false, true},
		{"bad origin without cookie", []string{"https://evil.example.com"}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t, withWails)
				defer e.close()
				cookie, _ := e.user(false)
				h := http.Header{}
				if tc.cookie {
					h = signaltest.CookieHeader(cookie)
				}
				for _, o := range tc.origin {
					h.Add("Origin", o)
				}
				c, err := e.dial(signaltest.DialOptions{Header: h})
				if !tc.ok {
					var re *signaltest.RefusedError
					if !errors.As(err, &re) || re.Status != http.StatusForbidden {
						t.Fatalf("dial: %v, want 403", err)
					}
					if n := e.logs.count("level=WARN", "websocket origin refused", "remote_ip=192.0.2.1"); n != 1 {
						t.Errorf("%d security log lines, want 1:\n%s", n, e.logs)
					}
					return
				}
				if err != nil {
					t.Fatalf("dial: %v", err)
				}
				if tc.cookie {
					if _, err := c.Hello(ctxT(t), signaltest.DefaultHello()); err != nil {
						t.Fatalf("hello: %v", err)
					}
				}
				expectOpen(t, c)
			})
		})
	}
}

// A refused Origin is logged at most once per client IP per minute (01 §17).
func TestUpgradeOriginLogThrottled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		for range 5 {
			_ = e.refused(headers("", "https://evil.example.com", "198.51.100.9"))
		}
		_ = e.refused(headers("", "https://evil.example.com", "198.51.100.10"))
		if n := e.logs.count("websocket origin refused", "remote_ip=198.51.100.9"); n != 1 {
			t.Errorf("%d lines for the first IP, want 1", n)
		}
		if n := e.logs.count("websocket origin refused", "remote_ip=198.51.100.10"); n != 1 {
			t.Errorf("%d lines for the second IP, want 1", n)
		}
		time.Sleep(time.Minute)
		_ = e.refused(headers("", "https://evil.example.com", "198.51.100.9"))
		if n := e.logs.count("websocket origin refused", "remote_ip=198.51.100.9"); n != 2 {
			t.Errorf("%d lines for the first IP after a minute, want 2", n)
		}
		if n := e.logs.count("level=INFO", "remote_ip"); n != 0 {
			t.Errorf("%d INFO lines with remote_ip, want none", n)
		}
	})
}

// The 21st pre-auth upgrade of one IP within a minute gets 429; others are not affected.
func TestUpgradePreAuthPerIP(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		const ip = "198.51.100.7"
		for i := range 20 {
			if _, err := e.dial(signaltest.DialOptions{Header: headers("", "", ip)}); err != nil {
				t.Fatalf("upgrade %d: %v", i+1, err)
			}
		}
		re := e.refused(headers("", "", ip))
		if re.Status != http.StatusTooManyRequests || re.RetryAfter != "3" {
			t.Fatalf("21st upgrade: %d, Retry-After %q; want 429 and 3 (one upgrade per 3 s)", re.Status, re.RetryAfter)
		}
		// A bad cookie is pre-auth too.
		re = e.refused(headers("unknown-cookie", "", ip))
		if re.Status != http.StatusTooManyRequests {
			t.Fatalf("bad cookie: %d, want 429", re.Status)
		}
		// Other addresses and cookie upgrades are not limited.
		e.mustDial(headers("", "", "198.51.100.8"))
		cookie, _ := e.user(false)
		for range 3 {
			e.mustDial(headers(cookie, "", ip))
		}
		if n := e.logs.count("level=WARN", "websocket pre-auth upgrades throttled", "remote_ip="+ip); n != 1 {
			t.Errorf("%d throttle log lines, want 1:\n%s", n, e.logs)
		}
		// One more token after 3 s.
		time.Sleep(3 * time.Second)
		e.mustDial(headers("", "", ip))
		if re := e.refused(headers("", "", ip)); re.Status != http.StatusTooManyRequests {
			t.Fatalf("after one refill: %d, want 429", re.Status)
		}
	})
}

// Origin refusals and pre-auth throttling have separate log budgets (01 §17): one of each per client IP per minute,
// in either order.
func TestUpgradeSecurityLogPerKind(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		throttle := func(ip string) {
			t.Helper()
			for i := range 20 {
				if _, err := e.dial(signaltest.DialOptions{Header: headers("", "", ip)}); err != nil {
					t.Fatalf("upgrade %d: %v", i+1, err)
				}
			}
			if re := e.refused(headers("", "", ip)); re.Status != http.StatusTooManyRequests {
				t.Fatalf("21st upgrade: %d, want 429", re.Status)
			}
		}
		refuseOrigin := func(ip string) {
			t.Helper()
			if re := e.refused(headers("", "https://evil.example.com", ip)); re.Status != http.StatusForbidden {
				t.Fatalf("bad origin: %d, want 403", re.Status)
			}
		}

		const first, second = "198.51.100.11", "2001:db8:5:6::1"
		refuseOrigin(first) // an Origin 403, then a 429
		throttle(first)
		throttle(second) // a 429, then an Origin 403
		refuseOrigin(second)
		for _, ip := range []string{first, second} {
			if n := e.logs.count("level=WARN", "websocket origin refused", "remote_ip="+ip); n != 1 {
				t.Errorf("%s: %d origin log lines, want 1", ip, n)
			}
			if n := e.logs.count("level=WARN", "websocket pre-auth upgrades throttled", "remote_ip="+ip); n != 1 {
				t.Errorf("%s: %d throttle log lines, want 1", ip, n)
			}
		}
		if t.Failed() {
			t.Logf("log:\n%s", e.logs)
		}
	})
}

// Pre-auth upgrades are keyed by the client's IPv6 /64 (01 §3.1, as 03 §7.3).
func TestUpgradePreAuthIPv6Prefix(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		for i := range 20 {
			ip := fmt.Sprintf("2001:db8:1:2::%x", i+1)
			if _, err := e.dial(signaltest.DialOptions{Header: headers("", "", ip)}); err != nil {
				t.Fatalf("upgrade %d: %v", i+1, err)
			}
		}
		if re := e.refused(headers("", "", "2001:db8:1:2:ffff:ffff:ffff:ffff")); re.Status != http.StatusTooManyRequests {
			t.Fatalf("21st upgrade in the /64: %d, want 429", re.Status)
		}
		e.mustDial(headers("", "", "2001:db8:1:3::1"))      // another /64
		e.mustDial(headers("", "", "::ffff:198.51.100.20")) // IPv4-mapped: its own /32
		e.mustDial(headers("", "", "198.51.100.20"))        // the same /32
		if re := e.refused(headers("", "", "2001:db8:1:2::abcd")); re.Status != http.StatusTooManyRequests {
			t.Fatalf("still limited: %d", re.Status)
		}
	})
}

// At most Limits.MaxPreAuthConns pre-auth sockets at once; cookie sockets don't count.
func TestUpgradePreAuthGlobal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, func(c *signal.Config) { c.Limits.MaxPreAuthConns = 3 })
		defer e.close()
		var open []*signaltest.Client
		for i := range 3 {
			open = append(open, e.mustDial(headers("", "", fmt.Sprintf("198.51.100.%d", i+1))))
		}
		re := e.refused(headers("", "", "198.51.100.50"))
		if re.Status != http.StatusServiceUnavailable || re.RetryAfter != "2" {
			t.Fatalf("4th pre-auth socket: %d, Retry-After %q; want 503 and 2", re.Status, re.RetryAfter)
		}
		cookie, _ := e.user(false)
		e.mustDial(headers(cookie, "", "198.51.100.50"))

		open[0].Close()
		synctest.Wait()
		e.mustDial(headers("", "", "198.51.100.50"))
		if _, _, preAuth, _ := signal.Counts(e.hub, ""); preAuth != 3 {
			t.Errorf("%d pre-auth sockets, want 3", preAuth)
		}

		// A pre-auth socket stops counting when its hello timeout closes it.
		time.Sleep(11 * time.Second)
		synctest.Wait()
		if _, _, preAuth, _ := signal.Counts(e.hub, ""); preAuth != 0 {
			t.Errorf("%d pre-auth sockets after the hello timeout, want 0", preAuth)
		}
	})
}

// The 17th cookie upgrade of one user is accepted and then gets too_many_connections and 4429. The error is the
// reply to the socket's hello: the hub waits for the hello, because one that resumes a connection of the user needs
// no slot (TestResumeAtTheCap).
func TestUpgradeConnectionsPerUser(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, id := e.user(false)
		var clients []*signaltest.Client
		for i := range 16 {
			c := e.mustDial(headers(cookie, testOrigin, ""))
			if i < 10 { // connections and handshaking sockets both count
				if _, err := c.Hello(ctxT(t), signaltest.DefaultHello()); err != nil {
					t.Fatalf("hello %d: %v", i, err)
				}
			}
			clients = append(clients, c)
		}
		c17 := e.mustDial(headers(cookie, testOrigin, ""))
		synctest.Wait()
		expectOpen(t, c17) // nothing before its hello
		if slots, over := slotsOf(e, id), signal.OverCap(e.hub, id.UserID); slots != 16 || over != 1 {
			t.Errorf("%d slots, %d sockets waiting at the cap; want 16 and 1", slots, over)
		}
		reqID := request(t, c17, protocol.MessageTypeHello, signaltest.DefaultHello())
		e17, re := expectError(t, c17, protocol.ErrorCodeTooManyConnections, protocol.ErrorScopeConnection)
		if e17.Retryable || re != reqID {
			t.Errorf("too_many_connections retryable %v, re %q (want %q)", e17.Retryable, re, reqID)
		}
		expectClose(t, c17, protocol.CloseCodeRateLimited)
		synctest.Wait()
		if slots, over := slotsOf(e, id), signal.OverCap(e.hub, id.UserID); slots != 16 || over != 0 {
			t.Errorf("%d slots, %d sockets waiting at the cap; want 16 and 0", slots, over)
		}

		// Another user is not affected. A connection whose socket just drops keeps its slot for the grace; one that
		// the client closes on purpose frees it at once.
		other, _ := e.user(false)
		e.connect(other, signaltest.DefaultHello())
		clients[0].Close()
		synctest.Wait()
		if slots := slotsOf(e, id); slots != 16 {
			t.Errorf("%d slots after a socket dropped, want 16 (the connection is detached)", slots)
		}
		if err := clients[1].CloseWith(websocket.StatusNormalClosure); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if slots := slotsOf(e, id); slots != 15 {
			t.Errorf("%d slots after a deliberate close, want 15", slots)
		}
		c, err := e.dial(signaltest.DialOptions{Header: headers(cookie, testOrigin, "")})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Hello(ctxT(t), signaltest.DefaultHello()); err != nil {
			t.Fatalf("hello after a slot was freed: %v", err)
		}
		// The six sockets that never said hello give their slots back at the hello timeout, the dropped connection
		// when its grace ends.
		time.Sleep(11 * time.Second)
		synctest.Wait()
		if slots := slotsOf(e, id); slots != 10 {
			t.Errorf("%d slots after the hello timeout, want 10", slots)
		}
		time.Sleep(grace - 11*time.Second)
		synctest.Wait()
		if slots := slotsOf(e, id); slots != 9 {
			t.Errorf("%d slots after the grace, want 9", slots)
		}
	})
}

// slotsOf returns the per-user slots that id's connections and handshaking sockets hold.
func slotsOf(e *env, id signal.Identity) int {
	_, _, _, slots := signal.Counts(e.hub, id.UserID)
	return slots
}

// A transient authentication error gets 503; a hub that shuts down refuses upgrades with 503.
func TestUpgradeUnavailable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		e.auth.FailRequests(errors.New("database is locked"))
		re := e.refused(headers(cookie, testOrigin, ""))
		if re.Status != http.StatusServiceUnavailable || re.RetryAfter != "2" {
			t.Fatalf("auth error: %d, Retry-After %q; want 503 and 2", re.Status, re.RetryAfter)
		}
		e.auth.FailRequests(nil)
		e.connect(cookie, signaltest.DefaultHello())

		if !e.hub.Ready() {
			t.Fatal("not ready before Shutdown")
		}
		go func() { _ = e.hub.Shutdown(ctxT(t), protocol.ShutdownReasonRestart) }()
		synctest.Wait()
		if e.hub.Ready() {
			t.Fatal("ready after Shutdown")
		}
		re = e.refused(headers(cookie, testOrigin, ""))
		if re.Status != http.StatusServiceUnavailable || re.RetryAfter != "2" {
			t.Fatalf("shutting down: %d, Retry-After %q; want 503 and 2", re.Status, re.RetryAfter)
		}
	})
}

// A ResponseWriter that can't hijack (HTTP/2, a recorder) gets 501 from websocket.Accept, and holds nothing.
func TestUpgradeNotHijackable(t *testing.T) {
	cfg, deps := validConfig()
	h, err := signal.New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ws", nil)
	for k, v := range map[string]string{"Connection": "Upgrade", "Upgrade": "websocket", "Sec-WebSocket-Version": "13",
		"Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ=="} {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status %d, want 501", rec.Code)
	}
	if conns, socks, preAuth, _ := signal.Counts(h, ""); conns+socks+preAuth != 0 {
		t.Errorf("conns %d, sockets %d, pre-auth %d; want none", conns, socks, preAuth)
	}
	if err := h.Shutdown(t.Context(), protocol.ShutdownReasonStop); err != nil {
		t.Error(err)
	}
}

// A request that isn't a WebSocket upgrade is answered by websocket.Accept and holds nothing.
func TestUpgradeNotWebSocket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		req, err := http.NewRequestWithContext(ctxT(t), http.MethodGet, "http://signaltest/ws", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := e.hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUpgradeRequired {
			t.Errorf("status %d, want 426", resp.StatusCode)
		}
		synctest.Wait()
		if conns, socks, preAuth, _ := signal.Counts(e.hub, ""); conns+socks+preAuth != 0 {
			t.Errorf("conns %d, sockets %d, pre-auth %d; want none", conns, socks, preAuth)
		}
	})
}
