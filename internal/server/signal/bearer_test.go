package signal_test

import (
	"fmt"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/signal"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// The desktop app's sockets on the hub as the wiring configures it (M2 02 §6.3, README WA56): the desktop origins,
// accepted only for bearer connections, and the pre-auth token that a bearer hello gives back.

// withDesktopOrigins allows every desktop origin, as the wiring does (site.ExtraOrigins = protocol.DesktopOrigins(),
// which hubCfg.AllowedOrigins takes).
func withDesktopOrigins(c *signal.Config) { c.AllowedOrigins = protocol.DesktopOrigins() }

// desktopHello is the hello of the desktop app with a bearer token: its Go connection (publisher) or its SPA
// (viewer); client.kind is desktop for both (M2 02 §6.3 item 5).
func desktopHello(token string, role protocol.Role) protocol.Hello {
	h := signaltest.DefaultHello()
	h.Client = protocol.ClientInfo{Kind: protocol.ClientKindDesktop, Version: "0.2.0", OS: protocol.ClientOSWindows}
	h.Role = role
	h.Auth = &protocol.HelloAuth{Scheme: protocol.AuthSchemeBearer, Token: protocol.Secret(token)}
	return h
}

// addDevice gives a user a device with a bearer token and returns the token.
func (e *env) addDevice(id signal.Identity, deviceID string) string {
	dev := id
	dev.SessionID, dev.DeviceID, dev.DeviceName = "", deviceID, "PC "+deviceID
	token := "token-" + deviceID
	e.auth.AddBearer(token, dev)
	return token
}

// A socket from the Windows app's SPA origin, http://wails.localhost, with hello.auth is accepted; with a valid
// cookie it gets 403 (01 §3.1 step 2, as built). Every other desktop origin is treated the same, and so is the
// app's Go connection, which sends no Origin.
func TestDesktopOriginsBearerOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, withDesktopOrigins)
		defer e.close()
		cookie, id := e.user(false)
		token := e.addDevice(id, "device01")

		if protocol.DesktopOrigins()[0] != "http://wails.localhost" {
			t.Fatalf("DesktopOrigins()[0] = %q, want http://wails.localhost", protocol.DesktopOrigins()[0])
		}
		for _, origin := range append(protocol.DesktopOrigins(), "") {
			c := e.mustDial(headers("", origin, ""))
			w, err := c.Hello(ctxT(t), desktopHello(token, protocol.RoleViewer))
			if err != nil {
				t.Fatalf("Origin %q: bearer hello: %v", origin, err)
			}
			if w.User.ID != id.UserID || w.User.Name != id.Name {
				t.Errorf("Origin %q: welcome user %+v, want %s", origin, w.User, id.UserID)
			}
			expectOpen(t, c)
			c.Close()
			if origin == "" {
				continue // a non-browser client may bring a cookie (01 §3.2)
			}
			if re := e.refused(headers(cookie, origin, "")); re.Status != http.StatusForbidden {
				t.Errorf("Origin %q with a cookie: %d, want 403", origin, re.Status)
			}
		}
		if n := e.logs.count("level=WARN", "websocket origin refused for a cookie connection"); n != 1 {
			t.Errorf("%d security log lines for the cookie refusals, want 1 (one per client IP per minute):\n%s", n, e.logs)
		}
	})
}

// The pre-auth refund (M2 02 §6.3 item 4): 21 bearer sockets of two users from one IP, opened one after another
// within a minute, all succeed, because each bearer hello gives its upgrade's token back; 21 sockets whose hello
// fails get 429 at the 21st upgrade, as in M1.
func TestPreAuthRefund(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, withDesktopOrigins)
		defer e.close()
		_, alice := e.user(false)
		_, bob := e.user(false)
		tokens := []string{e.addDevice(alice, "deviceA"), e.addDevice(bob, "deviceB")}

		const ip = "198.51.100.7"
		start := time.Now()
		var open []*signaltest.Client
		for i := range 21 {
			// Each app opens its Go connection (no Origin) and its SPA's (the webview origin), as after a server
			// restart; 11 connections of one user and 10 of the other stay below the per-user cap of 16.
			origin, role := "", protocol.RolePublisher
			if i%4 >= 2 {
				origin, role = protocol.DesktopAssetOriginWindows, protocol.RoleViewer
			}
			c, err := e.dial(signaltest.DialOptions{Header: headers("", origin, ip)})
			if err != nil {
				t.Fatalf("bearer socket %d: %v", i+1, err)
			}
			if _, err := c.Hello(ctxT(t), desktopHello(tokens[i%2], role)); err != nil {
				t.Fatalf("bearer socket %d: hello: %v", i+1, err)
			}
			open = append(open, c)
		}
		if took := time.Since(start); took >= time.Minute {
			t.Fatalf("the 21 sockets took %v, want them within a minute", took)
		}
		for _, c := range open {
			expectOpen(t, c)
		}
		if _, _, preAuth, _ := signal.Counts(e.hub, ""); preAuth != 0 {
			t.Errorf("%d pre-auth sockets, want 0", preAuth)
		}
		if n := e.logs.count("websocket pre-auth upgrades throttled"); n != 0 {
			t.Errorf("%d throttle log lines, want none:\n%s", n, e.logs)
		}

		// Sockets whose hello fails keep their token: an unknown token, then a cookie-less hello without auth.
		const other = "198.51.100.8"
		for i := range 20 {
			c := e.mustDial(headers("", protocol.DesktopAssetOriginWindows, other))
			h := desktopHello(fmt.Sprintf("revoked-%02d", i), protocol.RoleViewer)
			if i%2 == 1 {
				h.Auth = nil
			}
			if err := c.Send(protocol.MessageTypeHello, "1", h); err != nil {
				t.Fatal(err)
			}
			expectFail(t, c, protocol.ErrorCodeUnauthenticated, protocol.ErrorScopeSession)
		}
		re := e.refused(headers("", protocol.DesktopAssetOriginWindows, other))
		if re.Status != http.StatusTooManyRequests {
			t.Fatalf("the 21st upgrade after 20 failed hellos: %d, want 429", re.Status)
		}
		// A bearer socket of the first address still passes: its bucket is untouched by the refunds of the others.
		c := e.mustDial(headers("", "", ip))
		if _, err := c.Hello(ctxT(t), desktopHello(tokens[1], protocol.RoleViewer)); err != nil {
			t.Fatalf("the 22nd bearer socket: %v", err)
		}
	})
}
