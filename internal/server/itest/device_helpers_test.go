package itest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/client/signal"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/servertest"
)

// The helpers of docs/m2/02-device-auth.md §16.2 (device_test.go; README WA56): a device linked with raw HTTP, the
// app's REST calls, and its signaling connections on 01's Go signal client with the device's access token. They
// live in itest, not servertest, so that servertest never imports client packages. WA64 adds linkedManager, the
// same link through the real deviceauth.Manager.

// deviceAppVersion is the version the test app reports: in its device code request and in its hello.
const deviceAppVersion = "0.2.0"

// linkedDevice is a device that linkDevice linked: its id and its first token pair.
type linkedDevice struct {
	DeviceID        string
	Access, Refresh protocol.Secret
}

// linkDevice links a device for the user behind cookie with raw HTTP: POST /device/code, POST /device/decide with
// the cookie (approve, deviceName name), POST /device/token (device_code grant). Fails t on any other answer. (DA6)
//
// cookie is the Cookie header of the user's browser (sessionCookie). The app's requests come as its Go client sends
// them, without an Origin and from the loopback address; the decide comes as the approval page sends it.
func linkDevice(t *testing.T, srv *servertest.Server, cookie, name string) linkedDevice {
	t.Helper()
	var code api.DeviceCodeResponse
	rest(t, srv, restCall{method: http.MethodPost, path: "/api/v1/device/code", body: appCodeRequest("DESKTOP-TEST")}).
		decodeAs(t, "POST /device/code", http.StatusOK, &code)
	rest(t, srv, restCall{method: http.MethodPost, path: "/api/v1/device/decide", cookie: cookie,
		body: api.DeviceDecideRequest{UserCode: code.UserCode, Approve: true, DeviceName: &name}}).
		expect(t, "POST /device/decide", http.StatusNoContent)
	var pair api.DeviceTokenResponse
	rest(t, srv, restCall{method: http.MethodPost, path: "/api/v1/device/token",
		body: api.DeviceTokenRequest{GrantType: api.GrantTypeDeviceCode, DeviceCode: code.DeviceCode}}).
		decodeAs(t, "POST /device/token", http.StatusOK, &pair)
	checkPair(t, "the link", pair)
	if pair.Device.Name != name {
		t.Fatalf("the link created the device %q, want %q", pair.Device.Name, name)
	}
	return linkedDevice{DeviceID: pair.Device.ID, Access: protocol.Secret(pair.AccessToken),
		Refresh: protocol.Secret(pair.RefreshToken)}
}

// appCodeRequest is the test app's POST /api/v1/device/code, proposing the name name (M2 02 §5.2).
func appCodeRequest(name string) api.DeviceCodeRequest {
	return api.DeviceCodeRequest{ClientKind: string(protocol.ClientKindDesktop), DeviceName: name,
		OS: string(protocol.ClientOSWindows), AppVersion: deviceAppVersion}
}

// checkPair fails t unless a token answer has the shape of M2 02 §3 and §5.4: an access and a refresh token, the
// Bearer type, both lifetimes, and the device's id. Token values are never printed.
func checkPair(t *testing.T, what string, pair api.DeviceTokenResponse) {
	t.Helper()
	if !strings.HasPrefix(pair.AccessToken, "isa_") || len(pair.AccessToken) != 47 ||
		!strings.HasPrefix(pair.RefreshToken, "isr_") || len(pair.RefreshToken) != 47 ||
		pair.TokenType != "Bearer" || pair.ExpiresIn <= 0 || pair.RefreshExpiresIn <= 0 || pair.Device.ID == "" {
		t.Fatalf("%s answered tokens of lengths %d and %d, type %q, lifetimes %d and %d, device %q", what,
			len(pair.AccessToken), len(pair.RefreshToken), pair.TokenType, pair.ExpiresIn, pair.RefreshExpiresIn,
			pair.Device.ID)
	}
}

// refreshDevice is the app's POST /api/v1/device/token with the refresh_token grant (M2 02 §6.2); exchangeID ""
// sends none.
func refreshDevice(t *testing.T, srv *servertest.Server, refresh protocol.Secret, exchangeID string) restAnswer {
	t.Helper()
	return rest(t, srv, restCall{method: http.MethodPost, path: "/api/v1/device/token", body: api.DeviceTokenRequest{
		GrantType: api.GrantTypeRefreshToken, RefreshToken: refresh.Reveal(), ExchangeID: exchangeID,
		AppVersion: deviceAppVersion,
	}})
}

// newExchangeID draws an exchangeId as the app does for one logical refresh: 16 random bytes, base64url without
// padding (M2 02 §6.2, §11.4).
func newExchangeID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// sessionCookie returns the Cookie header that the browser whose jar this is sends to srv: the session cookie that
// a login, a registration or the setup put there.
func sessionCookie(t *testing.T, srv *servertest.Server, jar http.CookieJar) string {
	t.Helper()
	site, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	var cookies []string
	for _, c := range jar.Cookies(site) {
		cookies = append(cookies, c.Name+"="+c.Value)
	}
	if len(cookies) == 0 {
		t.Fatal("the browser has no session cookie")
	}
	return strings.Join(cookies, "; ")
}

// testClock is server.Deps.Now for a test that moves the accounts' time forward ("the server's clock hook" of
// M2 02 §16.2): the wall clock plus an offset that only grows. The store, auth and the REST API read it; the hub
// and the SFU keep the real time.
type testClock struct{ offset atomic.Int64 }

func (c *testClock) now() time.Time { return time.Now().Add(time.Duration(c.offset.Load())) }

// advance moves the clock forward by d.
func (c *testClock) advance(d time.Duration) { c.offset.Add(int64(d)) }

// ---- REST ----

// restCall is one REST request of a device test: the app's, which sends no Origin, or a browser's, which sends the
// site's Origin with its cookie, as the web app does. Every unsafe method carries Content-Type: application/json,
// which 03 §7.5 asks of the cookie path and M2 02 §6.1 of the bearer path.
type restCall struct {
	method, path string
	body         any             // sent as JSON; nil sends no body
	cookie       string          // a browser session's Cookie header (sessionCookie), with the site's Origin; "" for none
	bearer       protocol.Secret // Authorization: Bearer <token>, as the app sends it; "" for none
	from         string          // the client address the harness's proxy reports (X-Forwarded-For); "" for loopback
}

// restAnswer is the answer to a restCall.
type restAnswer struct {
	what   string
	status int
	header http.Header
	body   []byte
}

// rest sends a restCall to srv's main listener, on a client without a cookie jar: only what the call says goes
// with it.
func rest(t *testing.T, srv *servertest.Server, c restCall) restAnswer {
	t.Helper()
	var body io.Reader
	if c.body != nil {
		b, err := json.Marshal(c.body)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(waitCtx(t), c.method, srv.URL+c.path, body)
	if err != nil {
		t.Fatal(err)
	}
	if c.method != http.MethodGet && c.method != http.MethodHead {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cookie != "" {
		req.Header.Set("Cookie", c.cookie)
		req.Header.Set("Origin", srv.URL)
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer.Reveal())
	}
	if c.from != "" {
		req.Header.Set("X-Forwarded-For", c.from)
	}
	res, err := (&http.Client{Transport: srv.Client.Transport}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", c.method, c.path, err)
	}
	defer func() { _ = res.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		t.Fatalf("%s %s: reading the answer: %v", c.method, c.path, err)
	}
	return restAnswer{what: c.method + " " + c.path, status: res.StatusCode, header: res.Header, body: answer}
}

// expect fails t unless the answer has the status. The body is printed only for an error answer, which carries no
// token.
func (a restAnswer) expect(t *testing.T, what string, status int) {
	t.Helper()
	if a.status == status {
		return
	}
	if a.status >= 400 {
		t.Fatalf("%s (%s) = %d %s, want %d", what, a.what, a.status, a.body, status)
	}
	t.Fatalf("%s (%s) = %d, want %d", what, a.what, a.status, status)
}

// decodeAs checks the status and decodes the JSON body into v.
func (a restAnswer) decodeAs(t *testing.T, what string, status int, v any) {
	t.Helper()
	a.expect(t, what, status)
	if err := json.Unmarshal(a.body, v); err != nil {
		t.Fatalf("%s (%s): decoding the answer: %v", what, a.what, err)
	}
}

// apiError checks the status and returns the REST error of the body (03 §12.2's envelope).
func (a restAnswer) apiError(t *testing.T, what string, status int) api.Error {
	t.Helper()
	a.expect(t, what, status)
	var e api.ErrorResponse
	if err := json.Unmarshal(a.body, &e); err != nil || e.Error.Code == "" {
		t.Fatalf("%s (%s): %d %s is no error envelope: %v", what, a.what, a.status, a.body, err)
	}
	return e.Error
}

// wantForbiddenFor fails t unless the answer is 403 forbidden whose params name the only credential the route
// takes (M2 02 §6.4).
func (a restAnswer) wantForbiddenFor(t *testing.T, what string, kind api.CredentialKind) {
	t.Helper()
	e := a.apiError(t, what, http.StatusForbidden)
	if e.Code != api.CodeForbidden || len(e.Params) != 1 || e.Params[api.ParamCredential] != string(kind) {
		t.Fatalf("%s (%s) = 403 %s, want forbidden with params {credential: %s}", what, a.what, a.body, kind)
	}
}

// wantInvalidToken fails t unless the answer is 401 invalid_token with its challenge (M2 02 §6.1).
func (a restAnswer) wantInvalidToken(t *testing.T, what string) {
	t.Helper()
	if e := a.apiError(t, what, http.StatusUnauthorized); e.Code != api.CodeInvalidToken {
		t.Fatalf("%s (%s) = 401 %s, want invalid_token", what, a.what, a.body)
	}
	if got := a.header.Values("WWW-Authenticate"); len(got) != 1 || got[0] != `Bearer error="invalid_token"` {
		t.Fatalf("%s (%s): 401 invalid_token with WWW-Authenticate %q", what, a.what, got)
	}
}

// devices returns GET /api/v1/me/devices for the user behind cookie.
func devices(t *testing.T, srv *servertest.Server, cookie string) []api.DeviceInfo {
	t.Helper()
	var res api.DevicesResponse
	rest(t, srv, restCall{method: http.MethodGet, path: "/api/v1/me/devices", cookie: cookie}).
		decodeAs(t, "the Devices page's list", http.StatusOK, &res)
	return res.Devices
}

// ---- signaling ----

// deviceDial configures one of a device's signaling connections.
type deviceDial struct {
	role    protocol.Role
	origin  string // the Origin header: "" for the app's Go connection, protocol.DesktopAssetOriginWindows for its SPA
	version string // client.version; "" means deviceAppVersion
	from    string // the client address the harness's proxy reports (X-Forwarded-For); "" for loopback
}

// deviceConn is one signaling connection of a linked device: 01's Go signal client with the device's access token
// in hello.auth (Options.Token), as the app's core and its SPA connect (M2 02 §16.2 case 2). Its notifications and
// its stop are recorded for as long as it runs. It does not reconnect: a drop or a refusal ends it.
type deviceConn struct {
	t     *testing.T
	name  string
	sig   *signal.Client
	alive sync.WaitGroup

	mu        sync.Mutex
	rooms     []protocol.RoomState
	conns     []protocol.UserConnections
	stopped   *signal.StateChange // the client's StateStopped; nil while it runs
	stoppedAt time.Time           // when the recorder saw it
	failed    []error
}

// dialDevice connects one of a device's connections with token, with the hello of the desktop app: client.kind
// desktop on Windows, the user.connections feature. It returns Dial's error as it is: a *protocol.Error when the
// server refused the hello. The connection is closed when the test ends.
func dialDevice(t *testing.T, srv *servertest.Server, token protocol.Secret, name string, o deviceDial) (*deviceConn, error) {
	t.Helper()
	h := http.Header{}
	if o.origin != "" {
		h.Set("Origin", o.origin)
	}
	if o.from != "" {
		h.Set("X-Forwarded-For", o.from)
	}
	version := o.version
	if version == "" {
		version = deviceAppVersion
	}
	sig, err := signal.Dial(waitCtx(t), signal.Options{
		URL:    srv.WSURL,
		Header: h,
		Token:  func(context.Context) (protocol.Secret, error) { return token, nil },
		Client: protocol.ClientInfo{Kind: protocol.ClientKindDesktop, Version: version, OS: protocol.ClientOSWindows},
		Role:   o.role,
		Caps:   caps,
		// The list of the user's own connections with their device names (M2 01 §10.4).
		Features: []protocol.Feature{protocol.FeatureUserConnections},
		// No cookie jar: a bearer socket carries no cookie (a cookie from a desktop origin gets 403).
		HTTPClient:  &http.Client{Transport: srv.Client.Transport},
		NoReconnect: true,
	})
	if err != nil {
		return nil, err
	}
	d := &deviceConn{t: t, name: name, sig: sig}
	d.alive.Go(d.pump)
	d.alive.Go(d.watch)
	t.Cleanup(d.close)
	return d, nil
}

// connectDevice is dialDevice for a connection that must come up; it joins room unless room is "".
func connectDevice(t *testing.T, srv *servertest.Server, token protocol.Secret, name string, o deviceDial, room string) *deviceConn {
	t.Helper()
	d, err := dialDevice(t, srv, token, name, o)
	if err != nil {
		t.Fatalf("%s: dial: %v", name, err)
	}
	if room != "" {
		var joined protocol.RoomJoinResult
		if err := d.sig.Request(waitCtx(t), protocol.MessageTypeRoomJoin, protocol.RoomJoin{RoomID: room}, &joined); err != nil {
			t.Fatalf("%s: room.join %s: %v", name, room, err)
		}
		if joined.Room.ID != room {
			t.Fatalf("%s: joined %+v, want %s", name, joined.Room, room)
		}
	}
	return d
}

// pump records the room.state and user.connections notifications until the client has stopped.
func (d *deviceConn) pump() {
	for env := range d.sig.Events() {
		var err error
		switch env.Type {
		case protocol.MessageTypeRoomState:
			var st protocol.RoomState
			if st, err = protocol.Decode[protocol.RoomState](env); err == nil {
				d.mu.Lock()
				d.rooms = append(d.rooms, st)
				d.mu.Unlock()
			}
		case protocol.MessageTypeUserConnections:
			var uc protocol.UserConnections
			if uc, err = protocol.Decode[protocol.UserConnections](env); err == nil {
				d.mu.Lock()
				d.conns = append(d.conns, uc)
				d.mu.Unlock()
			}
		}
		if err != nil {
			d.mu.Lock()
			d.failed = append(d.failed, fmt.Errorf("%s: %s: %w", d.name, env.Type, err))
			d.mu.Unlock()
		}
	}
}

// watch records the client's stop: the error that ended it, and when.
func (d *deviceConn) watch() {
	for sc := range d.sig.States() {
		if sc.State == signal.StateStopped {
			d.mu.Lock()
			d.stopped, d.stoppedAt = &sc, time.Now()
			d.mu.Unlock()
		}
	}
}

// close closes the client, if the server has not ended it already, and waits for its recorders.
func (d *deviceConn) close() {
	_ = d.sig.Close() // after a revocation the socket is gone: nothing to close
	d.alive.Wait()
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, err := range d.failed {
		d.t.Errorf("%v", err)
	}
}

// connID returns the connection id of the welcome.
func (d *deviceConn) connID() string { return d.sig.Welcome().ConnectionID }

// roomState returns the newest room.state; the zero RoomState before the first.
func (d *deviceConn) roomState() protocol.RoomState {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.rooms) == 0 {
		return protocol.RoomState{}
	}
	return d.rooms[len(d.rooms)-1]
}

// userConns returns the newest user.connections; nil before the first.
func (d *deviceConn) userConns() []protocol.OwnConnection {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.conns) == 0 {
		return nil
	}
	return d.conns[len(d.conns)-1].Connections
}

// waitStopped waits until the client has stopped and returns its last state change and when it came.
func (d *deviceConn) waitStopped() (signal.StateChange, time.Time) {
	d.t.Helper()
	var sc *signal.StateChange
	var at time.Time
	eventually(d.t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		sc, at = d.stopped, d.stoppedAt
		return sc != nil
	}, func() string { return fmt.Sprintf("%s: the signaling client did not stop", d.name) })
	return *sc, at
}
