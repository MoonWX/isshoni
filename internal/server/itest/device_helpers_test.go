package itest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/client/credstore"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/client/signal"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/servertest"
)

// The helpers of docs/m2/02-device-auth.md §16.2 (device_test.go; README WA56): a device linked with raw HTTP, the
// app's REST calls, and its signaling connections on 01's Go signal client with the device's access token. They
// live in itest, not servertest, so that servertest never imports client packages. WA64 adds linkedManager, the
// same link through the real deviceauth.Manager, and the app's connections that take their tokens from it.

// deviceAppVersion is the version the test app reports: in its device code request and in its hello.
const deviceAppVersion = "0.2.0"

// deviceHostName is the name the test app proposes for itself (its Windows host name, M2 02 §5.6), and linkedName
// the one the user gives it on the approval page.
const (
	deviceHostName = "DESKTOP-TEST"
	linkedName     = "Test PC"
)

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
	rest(t, srv, restCall{method: http.MethodPost, path: "/api/v1/device/code", body: appCodeRequest(deviceHostName)}).
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

// ---- the app's deviceauth.Manager (DA11) ----

// pollInterval is the device flow's polling interval (M2 02 §5.2: interval 5). The Manager waits it out on the real
// clock before each poll: ManagerOptions.Clock reads the time but schedules nothing, so the server's clock hook can't
// shorten it, and every link through the Manager takes one interval.
const pollInterval = 5 * time.Second

// newManager returns a started deviceauth.Manager for srv on store, as the desktop app makes one per server: the
// identity of appCodeRequest (deviceAppVersion, desktop on Windows, deviceHostName), its requests on the harness's
// transport, without an Origin and from the loopback address. It is unlinked unless store holds a credential for
// srv. It is closed by t.Cleanup, before the server stops, and its log is printed when the test failed.
func newManager(t *testing.T, srv *servertest.Server, store credstore.Store) *deviceauth.Manager {
	t.Helper()
	dc, err := deviceauth.NewClient(srv.URL, deviceauth.AppInfo{Version: deviceAppVersion,
		ClientKind: protocol.ClientKindDesktop, OS: protocol.ClientOSWindows, DeviceName: deviceHostName},
		deviceauth.ClientOptions{HTTP: &http.Client{Transport: srv.Client.Transport}, AllowInsecureLoopback: true})
	if err != nil {
		t.Fatalf("deviceauth.NewClient(%s): %v", srv.URL, err)
	}
	logs := &lockedBuffer{}
	mgr := deviceauth.NewManager(deviceauth.ManagerOptions{Client: dc, Store: store,
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	t.Cleanup(func() {
		if err := mgr.Close(); err != nil {
			t.Errorf("Manager.Close: %v", err)
		}
		if t.Failed() {
			t.Logf("the device manager's log:\n%s", logs.String())
		}
	})
	if err := mgr.Start(waitCtx(t)); err != nil {
		t.Fatalf("Manager.Start: %v", err)
	}
	return mgr
}

// linkManager links mgr for the user behind cookie as the app and the user's browser do (M2 02 §16.2 case 1):
// Manager.Link returns at linking with the code; the test plays the browser, which looks the code up (network same:
// the app and the browser both come from the loopback address) and approves it with deviceName linkedName; the
// manager's first poll, one interval after Link, takes the pair. It fails t unless the manager is linked to that
// device within one poll interval of the approval, and returns the linked state.
func linkManager(t *testing.T, srv *servertest.Server, mgr *deviceauth.Manager, cookie string) deviceauth.State {
	t.Helper()
	if err := mgr.Link(waitCtx(t)); err != nil {
		t.Fatalf("Manager.Link: %v", err)
	}
	started := time.Now()
	st := mgr.State()
	if st.Status != deviceauth.StatusLinking || st.Link == nil ||
		st.Link.VerificationURIComplete != srv.URL+"/link?code="+st.Link.UserCode {
		t.Fatalf("after Link the manager is %s with code %v, want linking with a code to open on %s/link", st.Status,
			st.Link != nil, srv.URL)
	}
	var lookup api.DeviceLookup
	rest(t, srv, restCall{method: http.MethodPost, path: "/api/v1/device/lookup", cookie: cookie,
		body: api.DeviceLookupRequest{UserCode: st.Link.UserCode}}).decodeAs(t, "the browser's lookup", http.StatusOK, &lookup)
	if lookup.UserCode != st.Link.UserCode || lookup.Network != api.NetworkMatchSame || lookup.DeviceName != deviceHostName ||
		lookup.ClientKind != string(protocol.ClientKindDesktop) || lookup.OS != string(protocol.ClientOSWindows) ||
		lookup.AppVersion != deviceAppVersion {
		t.Fatalf("the lookup = the manager's code %v, network %q, kind %q, os %q, version %q; want same, desktop, windows, %s",
			lookup.UserCode == st.Link.UserCode, lookup.Network, lookup.ClientKind, lookup.OS, lookup.AppVersion,
			deviceAppVersion)
	}
	name := linkedName
	rest(t, srv, restCall{method: http.MethodPost, path: "/api/v1/device/decide", cookie: cookie,
		body: api.DeviceDecideRequest{UserCode: st.Link.UserCode, Approve: true, DeviceName: &name}}).
		expect(t, "the browser's approval", http.StatusNoContent)
	eventually(t, func() bool {
		st = mgr.State()
		return st.Status == deviceauth.StatusLinked
	}, func() string {
		return fmt.Sprintf("the manager is %s after the approval, want linked", mgr.State().Status)
	})
	// The first poll comes one interval after Link and the approval came before it; a second poll could come no
	// sooner than another interval later.
	if took := time.Since(started); took >= 2*pollInterval {
		t.Errorf("the manager was linked %v after Link, want within one poll interval (%v) of the approval", took,
			pollInterval)
	}
	if st.Device == nil || st.Device.ID == "" || st.Device.Name != name || st.User == nil || st.Notice != deviceauth.NoticeNone {
		t.Fatalf("the linked state has device %+v, user %v, notice %q; want the device named %q, a user, no notice",
			st.Device, st.User != nil, st.Notice, name)
	}
	return st
}

// linkedManager returns a started deviceauth.Manager (memory credential store) that is already linked for the user
// behind cookie, through Manager.Link and the cookie's lookup and decide (linkManager), and that store, in which a
// test sees what the app saved. Its AccessToken is core.Auth's; a test that hands it to 01's core wraps
// SessionEnded, as 03's session adapter does. Closed by t.Cleanup. (DA11)
func linkedManager(t *testing.T, srv *servertest.Server, cookie string) (*deviceauth.Manager, credstore.Store) {
	t.Helper()
	store := credstore.NewMemory()
	mgr := newManager(t, srv, store)
	linkManager(t, srv, mgr, cookie)
	return mgr, store
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
	// resumes makes the connection resume after a dropped socket, as the app's connections do: the client
	// reconnects (Options.NoReconnect off), and its sockets are recorded, so that the test can cut one
	// (deviceConn.killSocket). Without it a drop or a refusal ends the connection.
	resumes bool
}

// deviceConn is one signaling connection of a linked device: 01's Go signal client with the device's access token
// in hello.auth (Options.Token), as the app's core and its SPA connect (M2 02 §16.2 case 2). Its notifications and
// its stop are recorded for as long as it runs. It does not reconnect unless it was dialed with deviceDial.resumes.
type deviceConn struct {
	t     *testing.T
	name  string
	sig   *signal.Client
	sock  *sockets // the TCP connections of its WebSocket upgrades (deviceDial.resumes only)
	alive sync.WaitGroup

	mu        sync.Mutex
	rooms     []protocol.RoomState
	conns     []protocol.UserConnections
	resumed   int                 // the welcomes that resumed the connection
	stopped   *signal.StateChange // the client's StateStopped; nil while it runs
	stoppedAt time.Time           // when the recorder saw it
	failed    []error
}

// dialDevice connects one of a device's connections with token, with the hello of the desktop app: client.kind
// desktop on Windows, the user.connections feature. It returns Dial's error as it is: a *protocol.Error when the
// server refused the hello. The connection is closed when the test ends.
func dialDevice(t *testing.T, srv *servertest.Server, token protocol.Secret, name string, o deviceDial) (*deviceConn, error) {
	t.Helper()
	return dialWithToken(t, srv, func(context.Context) (protocol.Secret, error) { return token, nil }, name, o)
}

// dialApp is dialDevice for a connection of the app that takes its token from the app's deviceauth.Manager before
// every hello, a resuming one too: Options.Token is AccessToken(ctx, false), as 01's core and 03's binding for the
// SPA hand them out (M2 02 §16.2 case 2).
func dialApp(t *testing.T, srv *servertest.Server, mgr *deviceauth.Manager, name string, o deviceDial) (*deviceConn, error) {
	t.Helper()
	return dialWithToken(t, srv, func(ctx context.Context) (protocol.Secret, error) { return mgr.AccessToken(ctx, false) },
		name, o)
}

// dialWithToken is dialDevice with Options.Token.
func dialWithToken(t *testing.T, srv *servertest.Server, token func(context.Context) (protocol.Secret, error), name string,
	o deviceDial) (*deviceConn, error) {
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
	sock := &sockets{}
	transport := srv.Client.Transport
	if o.resumes {
		transport = sock.transport(t, transport)
	}
	sig, err := signal.Dial(waitCtx(t), signal.Options{
		URL:    srv.WSURL,
		Header: h,
		Token:  token,
		Client: protocol.ClientInfo{Kind: protocol.ClientKindDesktop, Version: version, OS: protocol.ClientOSWindows},
		Role:   o.role,
		Caps:   caps,
		// The list of the user's own connections with their device names (M2 01 §10.4).
		Features: []protocol.Feature{protocol.FeatureUserConnections},
		// No cookie jar: a bearer socket carries no cookie (a cookie from a desktop origin gets 403).
		HTTPClient:  &http.Client{Transport: transport},
		NoReconnect: !o.resumes,
	})
	if err != nil {
		return nil, err
	}
	d := &deviceConn{t: t, name: name, sig: sig, sock: sock}
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
	d.join(room)
	return d
}

// connectApp is dialApp for a connection that must come up; it joins room unless room is "".
func connectApp(t *testing.T, srv *servertest.Server, mgr *deviceauth.Manager, name string, o deviceDial, room string) *deviceConn {
	t.Helper()
	d, err := dialApp(t, srv, mgr, name, o)
	if err != nil {
		t.Fatalf("%s: dial: %v", name, err)
	}
	d.join(room)
	return d
}

// join joins room, unless room is "".
func (d *deviceConn) join(room string) {
	d.t.Helper()
	if room == "" {
		return
	}
	var joined protocol.RoomJoinResult
	if err := d.sig.Request(waitCtx(d.t), protocol.MessageTypeRoomJoin, protocol.RoomJoin{RoomID: room}, &joined); err != nil {
		d.t.Fatalf("%s: room.join %s: %v", d.name, room, err)
	}
	if joined.Room.ID != room {
		d.t.Fatalf("%s: joined %+v, want %s", d.name, joined.Room, room)
	}
}

// killSocket closes the TCP connection under the connection's WebSocket, without a close frame (client.killSocket's
// cut). A connection dialed with deviceDial.resumes resumes it at once.
func (d *deviceConn) killSocket() {
	d.t.Helper()
	d.sock.mu.Lock()
	conn := d.sock.last
	d.sock.mu.Unlock()
	if conn == nil {
		d.t.Fatalf("%s has no socket to kill (dial it with resumes)", d.name)
	}
	if err := conn.Close(); err != nil {
		d.t.Fatalf("%s: closing its socket: %v", d.name, err)
	}
}

// resumes returns how many welcomes have resumed the connection.
func (d *deviceConn) resumes() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.resumed
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

// watch records the client's stop, the error that ended it and when, and the welcomes that resumed it.
func (d *deviceConn) watch() {
	for sc := range d.sig.States() {
		switch {
		case sc.State == signal.StateStopped:
			d.mu.Lock()
			d.stopped, d.stoppedAt = &sc, time.Now()
			d.mu.Unlock()
		case sc.State == signal.StateReady && sc.Resumed:
			d.mu.Lock()
			d.resumed++
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
