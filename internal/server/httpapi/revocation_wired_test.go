package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/ops"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// The revocation rows of 03 §7.7 on a running server (README S58): the REST handlers of this package, the real
// auth.Service, the wiring's ConnCloser and 01's hub, in one process (servertest). The matrix with a fake ConnCloser
// (internal/server/auth) says which selector and reason each row asks for; these tests say that the sockets a user
// would have open are the ones that close.

const (
	wiredAdmin    = "Alex"
	wiredPassword = "correct horse battery"
	wiredTimeout  = 10 * time.Second
)

// browser is one browser at the server: a cookie jar of its own on the server's client.
type browser struct {
	t   *testing.T
	srv *servertest.Server
	hc  *http.Client
}

func newBrowser(t *testing.T, srv *servertest.Server) *browser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	hc := *srv.Client // the same transport, which dials the server whatever the URL says
	hc.Jar = jar
	return &browser{t: t, srv: srv, hc: &hc}
}

// call sends a request as the web app does: with the site's Origin, and a JSON body (nil for none) under
// Content-Type: application/json. It returns the status and the body.
func (b *browser) call(method, path string, body any) (int, string) {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wiredTimeout)
	defer cancel()
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			b.t.Fatal(err)
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.srv.URL+path, payload)
	if err != nil {
		b.t.Fatal(err)
	}
	req.Header.Set("Origin", b.srv.URL)
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := b.hc.Do(req)
	if err != nil {
		b.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		b.t.Fatalf("%s %s: %v", method, path, err)
	}
	return res.StatusCode, string(raw)
}

// must is call for a request that has to answer status.
func (b *browser) must(status int, method, path string, body any) string {
	b.t.Helper()
	got, text := b.call(method, path, body)
	if got != status {
		b.t.Fatalf("%s %s = %d %s, want %d", method, path, got, text, status)
	}
	return text
}

// signedIn reports whether the browser's cookie is a session.
func (b *browser) signedIn() bool {
	b.t.Helper()
	status, body := b.call(http.MethodGet, "/api/v1/me", nil)
	if status != http.StatusOK && status != http.StatusUnauthorized {
		b.t.Fatalf("GET /me = %d %s", status, body)
	}
	return status == http.StatusOK
}

// socket opens the browser's signaling connection, as the web app does after its page loaded: /ws with the session
// cookie, then hello.
func (b *browser) socket() *signaltest.Client {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wiredTimeout)
	defer cancel()
	c, err := signaltest.Dial(ctx, b.hc, b.srv.WSURL, signaltest.DialOptions{Header: http.Header{"Origin": {b.srv.URL}}})
	if err != nil {
		b.t.Fatalf("dial %s: %v", b.srv.WSURL, err)
	}
	b.t.Cleanup(c.Close)
	if _, err := c.Hello(ctx, signaltest.DefaultHello()); err != nil {
		b.t.Fatalf("hello: %v", err)
	}
	return c
}

// nextOf returns the socket's next message of one of the given types, skipping the others (invalidations).
func nextOf(t *testing.T, c *signaltest.Client, types ...protocol.MessageType) protocol.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wiredTimeout)
	defer cancel()
	for {
		env, err := c.Recv(ctx)
		if err != nil {
			t.Fatalf("waiting for %v: %v", types, err)
		}
		if slices.Contains(types, env.Type) {
			return env
		}
	}
}

// wantRevoked checks that the server ended the socket as 01 §12 says for a revoked session: error{session_revoked,
// scope session, not retryable}, then close 4401.
func wantRevoked(t *testing.T, what string, c *signaltest.Client) {
	t.Helper()
	pe, err := protocol.Decode[protocol.Error](nextOf(t, c, protocol.MessageTypeError))
	if err != nil || pe.Code != protocol.ErrorCodeSessionRevoked || pe.Scope != protocol.ErrorScopeSession || pe.Retryable {
		t.Fatalf("%s: the socket got %+v, %v; want session_revoked", what, pe, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wiredTimeout)
	defer cancel()
	code, err := c.CloseStatus(ctx)
	if err != nil || code != websocket.StatusCode(protocol.CloseCodeUnauthenticated) {
		t.Errorf("%s: close code %d, %v; want %d", what, code, err, protocol.CloseCodeUnauthenticated)
	}
}

// wantOpen checks that the socket is still open and served: the server answers its ping. An error message, which
// is how the server would end the socket, fails the test.
func wantOpen(t *testing.T, what string, c *signaltest.Client) {
	t.Helper()
	sent := time.Now().UnixMilli()
	if err := c.Send(protocol.MessageTypePing, "", protocol.Ping{T: sent}); err != nil {
		t.Fatalf("%s: the socket is closed: %v", what, err)
	}
	reply := nextOf(t, c, protocol.MessageTypePong, protocol.MessageTypeError)
	if reply.Type != protocol.MessageTypePong {
		t.Fatalf("%s: the server ended the socket: %s", what, reply.Data)
	}
	if pong, err := protocol.Decode[protocol.Pong](reply); err != nil || pong.T != sent {
		t.Fatalf("%s: pong %+v, %v; want the answer to the ping of %d", what, pong, err, sent)
	}
}

// wiredServer starts a server and sets it up: the returned browser is signed in as the admin.
func wiredServer(t *testing.T) (*servertest.Server, *browser) {
	t.Helper()
	srv := servertest.Start(t, servertest.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), wiredTimeout)
	defer cancel()
	link, err := ops.DialAdmin(srv.AdminSocket).SetupURL(ctx, 0)
	if err != nil {
		t.Fatalf("setup-url: %v", err)
	}
	token, ok := strings.CutPrefix(link.URL, srv.URL+"/setup#")
	if !ok {
		t.Fatalf("setup link %q, want %s/setup#<token>", link.URL, srv.URL)
	}
	b := newBrowser(t, srv)
	b.must(http.StatusCreated, http.MethodPost, "/api/v1/auth/setup/complete",
		api.SetupCompleteRequest{Token: token, Username: wiredAdmin, Password: wiredPassword})
	return srv, b
}

// login signs a new browser in.
func login(t *testing.T, srv *servertest.Server, username, password string) *browser {
	t.Helper()
	b := newBrowser(t, srv)
	b.must(http.StatusOK, http.MethodPost, "/api/v1/auth/login", api.LoginRequest{Username: username, Password: password})
	return b
}

// friend registers another account with an invite of the admin's and returns its browser.
func friend(t *testing.T, srv *servertest.Server, admin *browser, username string) *browser {
	t.Helper()
	var inv api.CreateInviteResponse
	if err := json.Unmarshal([]byte(admin.must(http.StatusCreated, http.MethodPost, "/api/v1/invites", struct{}{})), &inv); err != nil {
		t.Fatal(err)
	}
	token, ok := strings.CutPrefix(inv.URL, srv.URL+"/invite#")
	if !ok {
		t.Fatalf("invite link %q, want %s/invite#<token>", inv.URL, srv.URL)
	}
	b := newBrowser(t, srv)
	b.must(http.StatusCreated, http.MethodPost, "/api/v1/auth/register",
		api.RegisterRequest{InviteToken: token, Username: username, Password: wiredPassword})
	return b
}

// TestLogoutEverywhereOnAServer: "Log out everywhere" in one browser closes the sockets of every browser of that
// user, and of nobody else (03 §7.7).
func TestLogoutEverywhereOnAServer(t *testing.T) {
	srv, here := wiredServer(t)
	there := login(t, srv, wiredAdmin, wiredPassword)
	sam := friend(t, srv, here, "Sam")
	hereWS, thereWS, samWS := here.socket(), there.socket(), sam.socket()

	here.must(http.StatusNoContent, http.MethodPost, "/api/v1/auth/logout-everywhere", struct{}{})
	wantRevoked(t, "the browser that logged out everywhere", hereWS)
	wantRevoked(t, "the user's other browser", thereWS)
	wantOpen(t, "another user's browser", samWS)
	if here.signedIn() || there.signedIn() || !sam.signedIn() {
		t.Errorf("signed in afterwards: here %v, there %v, Sam %v; want only Sam", here.signedIn(), there.signedIn(), sam.signedIn())
	}
}

// TestSelfServiceOnAServer: revoking one session, signing out the other browsers, changing the password and
// deleting the account each close the sockets their row of 03 §7.7 names, and leave the others open.
func TestSelfServiceOnAServer(t *testing.T) {
	srv, admin := wiredServer(t)
	adminWS := admin.socket()

	// Sam in three browsers.
	here := friend(t, srv, admin, "Sam")
	second, third := login(t, srv, "Sam", wiredPassword), login(t, srv, "Sam", wiredPassword)
	hereWS, secondWS, thirdWS := here.socket(), second.socket(), third.socket()

	// Revoke one session: that browser's socket.
	var list api.SessionsResponse
	if err := json.Unmarshal([]byte(second.must(http.StatusOK, http.MethodGet, "/api/v1/me/sessions", nil)), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Sessions) != 3 || !list.Sessions[0].Current {
		t.Fatalf("Sam's sessions = %+v, want three with this browser's first", list.Sessions)
	}
	here.must(http.StatusNoContent, http.MethodDelete, "/api/v1/me/sessions/"+list.Sessions[0].ID, struct{}{})
	wantRevoked(t, "the revoked browser", secondWS)
	wantOpen(t, "the browser that revoked it", hereWS)
	wantOpen(t, "the user's third browser", thirdWS)

	// Sign out other browsers: every socket of the user's but this browser's.
	fourth := login(t, srv, "Sam", wiredPassword)
	fourthWS := fourth.socket()
	if got := here.must(http.StatusOK, http.MethodPost, "/api/v1/me/sessions/revoke-others", struct{}{}); got != `{"revoked":2}`+"\n" {
		t.Errorf("revoke-others answered %s", got)
	}
	wantRevoked(t, "the third browser", thirdWS)
	wantRevoked(t, "the fourth browser", fourthWS)
	wantOpen(t, "the browser that signed the others out", hereWS)

	// Change the password: the other browsers' sockets close, this browser keeps its socket and gets a new cookie.
	fifth := login(t, srv, "Sam", wiredPassword)
	fifthWS := fifth.socket()
	const changed = "another long passphrase"
	here.must(http.StatusOK, http.MethodPost, "/api/v1/me/password",
		api.ChangePasswordRequest{CurrentPassword: wiredPassword, NewPassword: changed})
	wantRevoked(t, "the other browser after a password change", fifthWS)
	wantOpen(t, "the browser that changed the password", hereWS)
	if !here.signedIn() || fifth.signedIn() {
		t.Errorf("signed in after the password change: here %v, the other browser %v; want only here", here.signedIn(), fifth.signedIn())
	}

	// Delete the account: every socket of the user's. The admin's was never touched.
	sixth := login(t, srv, "Sam", changed)
	sixthWS := sixth.socket()
	here.must(http.StatusNoContent, http.MethodPost, "/api/v1/me/delete", api.DeleteSelfRequest{Password: changed})
	wantRevoked(t, "the browser that deleted the account", hereWS)
	wantRevoked(t, "the deleted user's other browser", sixthWS)
	wantOpen(t, "the admin's browser", adminWS)
	if here.signedIn() || sixth.signedIn() || !admin.signedIn() {
		t.Error("only the admin should be signed in after Sam deleted the account")
	}
}
