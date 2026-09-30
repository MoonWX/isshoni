package signal_test

import (
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/signal"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// Handshake failures of 01 §19 and §8.2: each gets its error and close code.
func TestHandshakeFailures(t *testing.T) {
	bigHello := func() []byte {
		b, _ := protocol.Marshal(protocol.MessageTypeHello, "1", "", map[string]any{
			"protocol": 1, "minProtocol": 1, "role": "full", "client": map[string]string{"kind": "web"},
			"pad": strings.Repeat("x", 65*1024),
		})
		return b
	}
	helloWith := func(f func(*protocol.Hello)) []byte {
		h := signaltest.DefaultHello()
		f(&h)
		b, _ := protocol.Marshal(protocol.MessageTypeHello, "1", "", h)
		return b
	}
	cases := []struct {
		name      string
		cookie    bool // a valid session cookie
		badCookie bool // a cookie that authenticates nobody: the socket is pre-auth
		frame     []byte
		code      protocol.ErrorCode
		scope     protocol.ErrorScope
		closeCode protocol.CloseCode // for a close without an error message
		check     func(t *testing.T, e protocol.Error, re string)
	}{
		{name: "not hello first", cookie: true, frame: []byte(`{"type":"ping","data":{"t":1}}`),
			code: protocol.ErrorCodeHelloRequired, scope: protocol.ErrorScopeConnection},
		{name: "room.join first", cookie: true, frame: []byte(`{"type":"room.join","id":"1","data":{"roomId":"lounge"}}`),
			code: protocol.ErrorCodeHelloRequired, scope: protocol.ErrorScopeConnection},
		{name: "not JSON", cookie: true, frame: []byte(`hello`),
			code: protocol.ErrorCodeBadMessage, scope: protocol.ErrorScopeConnection},
		{name: "hello without id", cookie: true, frame: []byte(`{"type":"hello","data":{}}`),
			code: protocol.ErrorCodeBadMessage, scope: protocol.ErrorScopeConnection},
		{name: "unknown role", cookie: true, frame: helloWith(func(h *protocol.Hello) { h.Role = "boss" }),
			code: protocol.ErrorCodeBadRequest, scope: protocol.ErrorScopeConnection,
			check: func(t *testing.T, e protocol.Error, re string) {
				if re != "1" || e.Params["field"] != "role" || e.Params["reason"] != "invalid" {
					t.Errorf("re %q, params %v", re, e.Params)
				}
			}},
		{name: "protocol 2..2", cookie: true,
			frame: helloWith(func(h *protocol.Hello) { h.Protocol, h.MinProtocol = 2, 2 }),
			code:  protocol.ErrorCodeProtocolUnsupported, scope: protocol.ErrorScopeConnection,
			check: func(t *testing.T, e protocol.Error, re string) {
				// JSON numbers decode as float64.
				if re != "1" || e.Params["serverMin"] != 1.0 || e.Params["serverMax"] != 1.0 ||
					e.Params["serverVersion"] != serverVersion {
					t.Errorf("re %q, params %v", re, e.Params)
				}
				if e.Retryable {
					t.Error("protocol_unsupported is retryable")
				}
			}},
		{name: "no cookie and no bearer", frame: helloWith(func(*protocol.Hello) {}),
			code: protocol.ErrorCodeUnauthenticated, scope: protocol.ErrorScopeSession},
		{name: "bad cookie and no bearer", badCookie: true, frame: helloWith(func(*protocol.Hello) {}),
			code: protocol.ErrorCodeUnauthenticated, scope: protocol.ErrorScopeSession},
		{name: "cookie and bearer", cookie: true,
			frame: helloWith(func(h *protocol.Hello) {
				h.Auth = &protocol.HelloAuth{Scheme: protocol.AuthSchemeBearer, Token: "token"}
			}),
			code: protocol.ErrorCodeBadRequest, scope: protocol.ErrorScopeConnection,
			check: func(t *testing.T, e protocol.Error, _ string) {
				if e.Params["field"] != "auth" {
					t.Errorf("params %v", e.Params)
				}
			}},
		{name: "unknown bearer", frame: helloWith(func(h *protocol.Hello) {
			h.Auth = &protocol.HelloAuth{Scheme: protocol.AuthSchemeBearer, Token: "nope"}
		}), code: protocol.ErrorCodeUnauthenticated, scope: protocol.ErrorScopeSession},
		{name: "65 KiB before welcome", cookie: true, frame: bigHello(), closeCode: protocol.CloseCodeMessageTooBig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t)
				defer e.close()
				cookie := ""
				switch {
				case tc.cookie:
					cookie, _ = e.user(false)
				case tc.badCookie:
					cookie = "unknown-cookie"
				}
				c := e.mustDial(headers(cookie, testOrigin, ""))
				if err := c.SendRaw(tc.frame); err != nil {
					t.Fatalf("send: %v", err)
				}
				if tc.code == "" {
					expectClose(t, c, tc.closeCode)
					return
				}
				pe, re := expectError(t, c, tc.code, tc.scope)
				if tc.check != nil {
					tc.check(t, pe, re)
				}
				expectClose(t, c, protocol.CloseCodeFor(tc.code, tc.scope))
				if e.metric("isshoni_ws_errors_total", "code", string(tc.code)) != 1 {
					t.Errorf("isshoni_ws_errors_total{code=%q} not counted", tc.code)
				}
			})
		})
	}
}

// No hello within HelloTimeout: hello_timeout and 4408 (01 §3.1).
func TestHandshakeHelloTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, id := e.user(false)
		c := e.mustDial(headers(cookie, testOrigin, ""))
		time.Sleep(10*time.Second - time.Millisecond)
		synctest.Wait()
		expectOpen(t, c)
		time.Sleep(time.Millisecond)
		pe := expectFail(t, c, protocol.ErrorCodeHelloTimeout, protocol.ErrorScopeConnection)
		if !pe.Retryable {
			t.Error("hello_timeout is not retryable")
		}
		synctest.Wait()
		if conns, socks, _, slots := signal.Counts(e.hub, id.UserID); conns+socks+slots != 0 {
			t.Errorf("conns %d, sockets %d, slots %d after the timeout; want none", conns, socks, slots)
		}
	})
}

// A binary frame closes the socket with 1003 (01 §3.3), before and after welcome.
func TestHandshakeBinaryFrame(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		c := e.mustDial(headers(cookie, testOrigin, ""))
		if err := c.SendBinary([]byte("{}")); err != nil {
			t.Fatal(err)
		}
		expectClose(t, c, protocol.CloseCodeUnsupportedData)

		c, _ = e.connect(cookie, signaltest.DefaultHello())
		if err := c.SendBinary([]byte("{}")); err != nil {
			t.Fatal(err)
		}
		expectClose(t, c, protocol.CloseCodeUnsupportedData)
	})
}

// Bearer hellos (the M2 path, 01 §3.2): a valid token connects, and the per-user cap applies at hello.
func TestHandshakeBearer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, withWails)
		defer e.close()
		cookie, id := e.user(false)
		dev := id
		dev.SessionID, dev.DeviceID = "", "device01"
		e.auth.AddBearer("token01", dev)
		h := signaltest.DefaultHello()
		h.Client = protocol.ClientInfo{Kind: protocol.ClientKindDesktop, Version: "0.2.0", OS: protocol.ClientOSWindows}
		h.Role = protocol.RolePublisher
		h.Auth = &protocol.HelloAuth{Scheme: protocol.AuthSchemeBearer, Token: "token01"}

		c := e.mustDial(headers("", wailsOrigin, ""))
		w, err := c.Hello(ctxT(t), h)
		if err != nil {
			t.Fatalf("bearer hello: %v", err)
		}
		if w.User.ID != id.UserID {
			t.Errorf("user %+v", w.User)
		}
		if _, _, preAuth, slots := signal.Counts(e.hub, id.UserID); preAuth != 0 || slots != 1 {
			t.Errorf("pre-auth %d, slots %d; want 0 and 1", preAuth, slots)
		}

		// Fill the user's 16 slots with cookie sockets: the next bearer hello gets too_many_connections.
		for range 15 {
			e.mustDial(headers(cookie, testOrigin, ""))
		}
		c2 := e.mustDial(headers("", "", ""))
		if err := c2.Send(protocol.MessageTypeHello, "1", h); err != nil {
			t.Fatal(err)
		}
		expectFail(t, c2, protocol.ErrorCodeTooManyConnections, protocol.ErrorScopeConnection)

		// A transient bearer error is internal (1011), not unauthenticated.
		e.auth.FailBearer(errors.New("database is locked"))
		c3 := e.mustDial(headers("", "", ""))
		if err := c3.Send(protocol.MessageTypeHello, "1", h); err != nil {
			t.Fatal(err)
		}
		pe := expectFail(t, c3, protocol.ErrorCodeInternal, protocol.ErrorScopeConnection)
		if ref, _ := pe.Params["ref"].(string); len(ref) != 8 || e.logs.count("level=ERROR", "ref="+ref) != 1 {
			t.Errorf("ref %q not logged at ERROR:\n%s", ref, e.logs)
		}
	})
}

// Revalidation at connect (01 §3.2): ErrInvalid closes with session_revoked; other errors don't stop the hello.
func TestHandshakeRevalidate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, id := e.user(false)
		c := e.mustDial(headers(cookie, testOrigin, ""))
		e.auth.Revoke(id.SessionID) // logged out between the upgrade and hello
		if err := c.Send(protocol.MessageTypeHello, "1", signaltest.DefaultHello()); err != nil {
			t.Fatal(err)
		}
		expectFail(t, c, protocol.ErrorCodeSessionRevoked, protocol.ErrorScopeSession)

		cookie2, _ := e.user(false)
		e.auth.FailRevalidate(errors.New("database is locked"))
		e.connect(cookie2, signaltest.DefaultHello())
		if n := e.logs.count("level=WARN", "revalidate at connect"); n != 1 {
			t.Errorf("%d WARN lines, want 1", n)
		}
	})
}

// The client floor (01 §6.1): native and tool clients below Policy.MinClientVersion get client_outdated; web
// clients are never checked.
func TestHandshakeClientOutdated(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		e.policy.Set(signal.Policy{MinClientVersion: "0.2.0"})
		cookie, _ := e.user(false)
		for _, tc := range []struct {
			kind    protocol.ClientKind
			version string
			ok      bool
		}{
			{protocol.ClientKindDesktop, "0.1.9", false},
			{protocol.ClientKindDesktop, "0.2.0-rc.1", false},
			{protocol.ClientKindDesktop, "dev", false},
			{protocol.ClientKindTool, "0.1.0", false},
			{protocol.ClientKindDesktop, "0.2.0", true},
			{protocol.ClientKindMobile, "v1.0.0", true},
			{protocol.ClientKindWeb, "0.1.0", true},
			{"futurekind", "0.0.1", true},
		} {
			h := signaltest.DefaultHello()
			h.Client.Kind, h.Client.Version = tc.kind, tc.version
			c := e.mustDial(headers(cookie, testOrigin, ""))
			w, err := c.Hello(ctxT(t), h)
			var pe *protocol.Error
			switch {
			case tc.ok && err != nil:
				t.Errorf("%s %s: %v", tc.kind, tc.version, err)
			case tc.ok && w.MinClientVersion != "0.2.0":
				t.Errorf("%s %s: minClientVersion %q", tc.kind, tc.version, w.MinClientVersion)
			case !tc.ok && (!errors.As(err, &pe) || pe.Code != protocol.ErrorCodeClientOutdated):
				t.Errorf("%s %s: %v, want client_outdated", tc.kind, tc.version, err)
			case !tc.ok:
				expectClose(t, c, protocol.CloseCodeVersionUnsupported)
			}
			c.Close()
		}
	})
}

// A failing room directory makes hello fail with internal and 1011.
func TestHandshakeDefaultRoomError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		e.rooms.FailDefault(errors.New("database is locked"))
		c := e.mustDial(headers(cookie, testOrigin, ""))
		if err := c.Send(protocol.MessageTypeHello, "1", signaltest.DefaultHello()); err != nil {
			t.Fatal(err)
		}
		pe := expectFail(t, c, protocol.ErrorCodeInternal, protocol.ErrorScopeConnection)
		if !pe.Retryable {
			t.Error("internal is not retryable")
		}
	})
}

// A second hello gets bad_request (scope connection) and 4400 (01 §8.2 step 1).
func TestHandshakeSecondHello(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		c, _ := e.connect(cookie, signaltest.DefaultHello())
		id := request(t, c, protocol.MessageTypeHello, signaltest.DefaultHello())
		if _, re := expectError(t, c, protocol.ErrorCodeBadRequest, protocol.ErrorScopeConnection); re != id {
			t.Errorf("re %q, want %q", re, id)
		}
		expectClose(t, c, protocol.CloseCodeProtocolViolation)
	})
}

// A resume token is accepted but resumes nothing yet (README S28): a new connection, resumed false.
func TestHandshakeResumeTokenNotResumed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		c, w := e.connect(cookie, signaltest.DefaultHello())
		c.Close()
		h := signaltest.DefaultHello()
		h.ResumeToken = w.ResumeToken
		_, w2 := e.connect(cookie, h)
		if w2.Resumed || w2.ConnectionID == w.ConnectionID || w2.ResumeToken == w.ResumeToken {
			t.Errorf("resumed %v, same id %v, same token %v", w2.Resumed, w2.ConnectionID == w.ConnectionID,
				w2.ResumeToken == w.ResumeToken)
		}
		if got := e.metric("isshoni_ws_resume_total", "result", "not_resumed"); got != 1 {
			t.Errorf("isshoni_ws_resume_total{not_resumed} %v, want 1", got)
		}
	})
}
