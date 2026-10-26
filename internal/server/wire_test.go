package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/auth"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/httpapi"
	"github.com/MoonWX/isshoni/internal/server/netx"
	"github.com/MoonWX/isshoni/internal/server/ops"
	"github.com/MoonWX/isshoni/internal/server/ops/doctor"
	"github.com/MoonWX/isshoni/internal/server/push"
	"github.com/MoonWX/isshoni/internal/server/sfu"
	"github.com/MoonWX/isshoni/internal/server/signal"
	"github.com/MoonWX/isshoni/internal/server/store"
	"github.com/MoonWX/isshoni/internal/server/tlsmgr"
	"github.com/MoonWX/isshoni/internal/version"
)

// The adapters of 04 §6.6, each with a table test against a fake of what it calls, and the ones over 03's store and
// account service once more against the real thing.

const (
	wireOrigin   = "https://watch.example.com"
	wirePassword = "correct horse battery"
)

// wireArgon is a password hash that costs next to nothing.
var wireArgon = auth.ArgonParams{MemoryKiB: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

var wireClientIP = netip.MustParseAddr("203.0.113.9")

// openTestStore opens a new database in the test's directory and closes it with the test.
func openTestStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), store.Options{
		Path:       filepath.Join(t.TempDir(), "isshoni.db"),
		AppVersion: version.Version(),
		Logger:     discardLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// newTestAccounts builds 03's account service on db, as wireAccounts does but with the tests' hash cost.
func newTestAccounts(t *testing.T, db *store.DB, conns auth.ConnCloser) *auth.Service {
	t.Helper()
	svc, err := auth.New(context.Background(), db, auth.Options{
		Keys:    auth.Keys{Session: bytes.Repeat([]byte{0x11}, 32), Invite: bytes.Repeat([]byte{0x22}, 32)},
		Origins: func() auth.Origins { return auth.Origins{Primary: wireOrigin, Public: []string{wireOrigin}} },
		Conns:   conns,
		Argon:   wireArgon,
		Logger:  discardLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// setupToken is the token of a setup, invite or reset link: its fragment.
func setupToken(t *testing.T, link string) string {
	t.Helper()
	_, token, ok := strings.Cut(link, "#")
	if !ok || token == "" {
		t.Fatalf("the link %q has no token in its fragment", link)
	}
	return token
}

// setUpAdmin creates the first admin through a setup link, as the operator does, and returns its session.
func setUpAdmin(t *testing.T, svc *auth.Service, username string) auth.LoginResult {
	t.Helper()
	ctx := context.Background()
	link, err := svc.IssueSetupToken(ctx, store.CLIActor)
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.CompleteSetup(ctx, auth.SetupInput{
		Token: setupToken(t, link.URL), Username: username, Password: wirePassword,
	}, auth.ReqMeta{IP: wireClientIP})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// upgradeRequest is a GET /ws request with the session cookie that svc sets for token ("" for none).
func upgradeRequest(t *testing.T, svc *auth.Service, token string) *http.Request {
	t.Helper()
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, wireOrigin+"/ws", nil)
	if token == "" {
		return r
	}
	rec := httptest.NewRecorder()
	svc.SetSessionCookie(rec, token, time.Now().Add(time.Hour))
	res := rec.Result()
	defer func() { _ = res.Body.Close() }()
	cookies := res.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("SetSessionCookie set %d cookies, want 1", len(cookies))
	}
	r.AddCookie(cookies[0]) // the name and the value: what a browser sends back
	return r
}

// ---- signal.Authenticator ----

// fakeSessions is a sessionService that answers what the test says and records what AuthenticateBearerToken and
// TouchPrincipal were asked.
type fakeSessions struct {
	principal auth.Principal // AuthenticateCookie's and AuthenticateBearerToken's answer
	cookieErr error
	bearerErr error
	touchErr  error
	// current is what TouchPrincipal returns: the principal as the rows have it now. nil returns the principal it
	// was given, as auth does for a session.
	current *auth.Principal

	tokens  []string // the tokens AuthenticateBearerToken was given
	touched []auth.Principal
	touchIP netip.Addr
}

func (f *fakeSessions) AuthenticateCookie(*http.Request) (auth.Principal, error) {
	if f.cookieErr != nil {
		return auth.Principal{}, f.cookieErr
	}
	return f.principal, nil
}

func (f *fakeSessions) AuthenticateBearerToken(_ context.Context, token string) (auth.Principal, error) {
	f.tokens = append(f.tokens, token)
	if f.bearerErr != nil {
		return auth.Principal{}, f.bearerErr
	}
	return f.principal, nil
}

func (f *fakeSessions) TouchPrincipal(_ context.Context, p auth.Principal, ip netip.Addr) (auth.Principal, error) {
	f.touched, f.touchIP = append(f.touched, p), ip
	switch {
	case f.touchErr != nil:
		return auth.Principal{}, f.touchErr
	case f.current != nil:
		return *f.current, nil
	}
	return p, nil
}

// errStoreDown stands for a failure of the store under the account service.
var errStoreDown = errors.New("database is locked")

// internalFailure is an unexpected failure as auth returns it: the cause, wrapped with *api.Error{internal}.
func internalFailure() error {
	return fmt.Errorf("auth: authenticate: %w (%w)", errStoreDown, api.NewError(api.CodeInternal))
}

func TestAuthenticateRequest(t *testing.T) {
	admin := auth.Principal{UserID: "u1", Username: "Alex", Role: store.RoleAdmin, Method: auth.MethodSession,
		SessionID: "s1", ClientKind: "web"}
	member := admin
	member.Role = store.RoleUser

	for _, tc := range []struct {
		name      string
		principal auth.Principal
		err       error
		want      signal.Identity
		wantErr   error // the sentinel the hub tests for; nil with err set means "passed through"
	}{
		{name: "an admin's session", principal: admin,
			want: signal.Identity{UserID: "u1", Name: "Alex", Admin: true, SessionID: "s1"}},
		{name: "a member's session", principal: member,
			want: signal.Identity{UserID: "u1", Name: "Alex", SessionID: "s1"}},
		{name: "no cookie", err: auth.ErrNoCookie, wantErr: signal.ErrNoCredentials},
		{name: "no cookie, wrapped", err: fmt.Errorf("ws: %w", auth.ErrNoCookie), wantErr: signal.ErrNoCredentials},
		{name: "an unknown, expired or disabled session", err: api.NewError(api.CodeUnauthenticated), wantErr: signal.ErrInvalid},
		{name: "unauthenticated, wrapped", err: fmt.Errorf("ws: %w", api.NewError(api.CodeUnauthenticated)), wantErr: signal.ErrInvalid},
		{name: "the store fails", err: internalFailure()},
		{name: "another api error", err: api.NewError(api.CodeServerBusy)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &authenticator{sessions: &fakeSessions{principal: tc.principal, cookieErr: tc.err}}
			got, err := a.AuthenticateRequest(httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/ws", nil))
			switch {
			case tc.err == nil:
				if err != nil || got != tc.want {
					t.Errorf("AuthenticateRequest = %+v, %v; want %+v", got, err, tc.want)
				}
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) || got != (signal.Identity{}) {
					t.Errorf("AuthenticateRequest = %+v, %v; want %v", got, err, tc.wantErr)
				}
			default:
				// Transient for the hub: neither of its sentinels, and the cause is still there for the log.
				if err == nil || errors.Is(err, signal.ErrInvalid) || errors.Is(err, signal.ErrNoCredentials) || !errors.Is(err, tc.err) {
					t.Errorf("AuthenticateRequest error = %v, want %v passed through", err, tc.err)
				}
			}
		})
	}
}

// The bearer adapter of M2 02 §6.3 item 2: auth.AuthenticateBearerToken with the revealed token; invalid_token and
// unauthenticated are the hub's ErrInvalid, anything else passes through; the identity is the device's, its name and
// the admin flag included.
func TestAuthenticateBearer(t *testing.T) {
	const token = "isa_" + "0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"
	admin := auth.Principal{UserID: "u1", Username: "Alex", Role: store.RoleAdmin, Method: auth.MethodBearer,
		DeviceID: "d1", ClientKind: "desktop", DeviceName: "Gaming PC"}
	member := admin
	member.Role = store.RoleUser

	for _, tc := range []struct {
		name      string
		principal auth.Principal
		err       error
		want      signal.Identity
		wantErr   error // signal.ErrInvalid; nil with err set means "passed through"
	}{
		{name: "an admin's device", principal: admin,
			want: signal.Identity{UserID: "u1", Name: "Alex", Admin: true, DeviceID: "d1", DeviceName: "Gaming PC"}},
		{name: "a member's device", principal: member,
			want: signal.Identity{UserID: "u1", Name: "Alex", DeviceID: "d1", DeviceName: "Gaming PC"}},
		{name: "an unknown or expired token", err: api.NewError(api.CodeInvalidToken), wantErr: signal.ErrInvalid},
		{name: "invalid_token, wrapped", err: fmt.Errorf("auth: %w", api.NewError(api.CodeInvalidToken)), wantErr: signal.ErrInvalid},
		{name: "the credential went away meanwhile", err: api.NewError(api.CodeUnauthenticated), wantErr: signal.ErrInvalid},
		{name: "the store fails", err: internalFailure()},
		{name: "another api error", err: api.NewError(api.CodeServerBusy)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions := &fakeSessions{principal: tc.principal, bearerErr: tc.err}
			a := &authenticator{sessions: sessions}
			got, err := a.AuthenticateBearer(context.Background(), protocol.Secret(token))
			switch {
			case tc.err == nil:
				if err != nil || got != tc.want {
					t.Errorf("AuthenticateBearer = %+v, %v; want %+v", got, err, tc.want)
				}
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) || got != (signal.Identity{}) {
					t.Errorf("AuthenticateBearer = %+v, %v; want %v", got, err, tc.wantErr)
				}
			default:
				// Transient for the hub (it answers internal): not ErrInvalid, and the cause is still there.
				if err == nil || errors.Is(err, signal.ErrInvalid) || !errors.Is(err, tc.err) || got != (signal.Identity{}) {
					t.Errorf("AuthenticateBearer = %+v, %v; want %v passed through", got, err, tc.err)
				}
			}
			if len(sessions.tokens) != 1 || sessions.tokens[0] != token {
				t.Errorf("AuthenticateBearerToken was given %d tokens, want the revealed token once", len(sessions.tokens))
			}
		})
	}
}

func TestRevalidate(t *testing.T) {
	session := signal.Identity{UserID: "u1", Name: "Alex", Admin: true, SessionID: "s1"}
	device := signal.Identity{UserID: "u1", Name: "Alex", Admin: true, DeviceID: "d1", DeviceName: "Gaming PC"}
	active := func(name string, role store.Role) store.User {
		return store.User{ID: "u1", Username: name, Role: role, Status: store.StatusActive}
	}
	// deviceNow is the bearer principal as auth.TouchPrincipal returns it: the device row's current name.
	deviceNow := func(name string) *auth.Principal {
		return &auth.Principal{UserID: "u1", Username: "Alex", Role: store.RoleAdmin, Method: auth.MethodBearer,
			DeviceID: "d1", ClientKind: "desktop", DeviceName: name}
	}
	with := func(id signal.Identity, f func(*signal.Identity)) signal.Identity {
		f(&id)
		return id
	}

	for _, tc := range []struct {
		name     string
		id       signal.Identity
		touchErr error
		current  *auth.Principal // TouchPrincipal's principal; nil: the one it was given
		user     store.User
		userErr  error
		want     signal.Identity
		wantErr  error // signal.ErrInvalid, or nil with an error expected: passed through
		passed   error // the cause that must still be in a passed-through error
		noRead   bool  // the user is not read
	}{
		{name: "still valid", id: session, user: active("Alex", store.RoleAdmin), want: session},
		{name: "renamed", id: session, user: active("Alexandra", store.RoleAdmin),
			want: signal.Identity{UserID: "u1", Name: "Alexandra", Admin: true, SessionID: "s1"}},
		{name: "no longer an admin", id: session, user: active("Alex", store.RoleUser),
			want: signal.Identity{UserID: "u1", Name: "Alex", SessionID: "s1"}},
		{name: "the session is gone", id: session, touchErr: api.NewError(api.CodeUnauthenticated),
			wantErr: signal.ErrInvalid, noRead: true},
		{name: "touch fails for another reason", id: session, touchErr: internalFailure(), passed: errStoreDown, noRead: true},
		{name: "the user was deleted meanwhile", id: session, userErr: store.ErrNotFound, wantErr: signal.ErrInvalid},
		{name: "the user was disabled", id: session,
			user: store.User{ID: "u1", Username: "Alex", Role: store.RoleAdmin, Status: store.StatusDisabled}, wantErr: signal.ErrInvalid},
		{name: "the user is pending", id: session,
			user: store.User{ID: "u1", Username: "Alex", Status: store.StatusPending}, wantErr: signal.ErrInvalid},
		{name: "the user can't be read", id: session, userErr: errStoreDown, passed: errStoreDown},
		// Devices (M2 02 §6.3 item 3): the device's current name comes from TouchPrincipal, the name and role from
		// the user's row, as for a session.
		{name: "a device, unchanged", id: device, current: deviceNow("Gaming PC"), user: active("Alex", store.RoleAdmin),
			want: device},
		{name: "a device renamed", id: device, current: deviceNow("Living room PC"), user: active("Alex", store.RoleAdmin),
			want: with(device, func(id *signal.Identity) { id.DeviceName = "Living room PC" })},
		{name: "a device whose user was renamed and demoted", id: device, current: deviceNow("Gaming PC"),
			user: active("Alexandra", store.RoleUser),
			want: with(device, func(id *signal.Identity) { id.Name, id.Admin = "Alexandra", false })},
		{name: "a device gone", id: device, touchErr: api.NewError(api.CodeUnauthenticated), wantErr: signal.ErrInvalid,
			noRead: true},
		{name: "a device, touch fails for another reason", id: device, touchErr: internalFailure(), passed: errStoreDown,
			noRead: true},
		{name: "a device whose user was disabled", id: device, current: deviceNow("Gaming PC"),
			user:    store.User{ID: "u1", Username: "Alex", Role: store.RoleAdmin, Status: store.StatusDisabled},
			wantErr: signal.ErrInvalid},
		{name: "a session never takes a device name", id: session, current: deviceNow("Gaming PC"),
			user: active("Alex", store.RoleAdmin), want: session},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions := &fakeSessions{touchErr: tc.touchErr, current: tc.current}
			reads := 0
			a := &authenticator{sessions: sessions, user: func(_ context.Context, id store.UserID) (store.User, error) {
				reads++
				if id != store.UserID(tc.id.UserID) {
					t.Errorf("read user %q, want %q", id, tc.id.UserID)
				}
				return tc.user, tc.userErr
			}}
			got, err := a.Revalidate(context.Background(), tc.id, wireClientIP)
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) || got != (signal.Identity{}) {
					t.Errorf("Revalidate = %+v, %v; want %v", got, err, tc.wantErr)
				}
			case tc.passed != nil:
				if err == nil || errors.Is(err, signal.ErrInvalid) || !errors.Is(err, tc.passed) {
					t.Errorf("Revalidate error = %v, want %v passed through (the hub keeps the connection)", err, tc.passed)
				}
			default:
				if err != nil || got != tc.want {
					t.Errorf("Revalidate = %+v, %v; want %+v", got, err, tc.want)
				}
			}
			// The session is marked as seen from the connection's address: one Touch with the identity's ids.
			wantTouch := auth.Principal{UserID: store.UserID(tc.id.UserID), Method: auth.MethodSession, SessionID: store.SessionID(tc.id.SessionID)}
			if tc.id.SessionID == "" {
				wantTouch = auth.Principal{UserID: store.UserID(tc.id.UserID), Method: auth.MethodBearer, DeviceID: store.DeviceID(tc.id.DeviceID)}
			}
			if len(sessions.touched) != 1 || sessions.touched[0] != wantTouch || sessions.touchIP != wireClientIP {
				t.Errorf("Touch calls = %+v from %v, want one of %+v from %v", sessions.touched, sessions.touchIP, wantTouch, wireClientIP)
			}
			if wantReads := map[bool]int{true: 0, false: 1}[tc.noRead]; reads != wantReads {
				t.Errorf("the user was read %d times, want %d", reads, wantReads)
			}
		})
	}
}

// TestAuthenticatorOverAuthService runs the adapter against 03's real service: the cookie it sets is the cookie the
// upgrade reads, and what happens to the session and the user afterwards reaches the hub at the next Revalidate.
func TestAuthenticatorOverAuthService(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	svc := newTestAccounts(t, db, nil)
	a := &authenticator{sessions: svc, user: userReader(db)}
	login := setUpAdmin(t, svc, "Alex")

	if _, err := a.AuthenticateRequest(upgradeRequest(t, svc, "")); !errors.Is(err, signal.ErrNoCredentials) {
		t.Errorf("without a cookie: %v, want ErrNoCredentials", err)
	}
	if _, err := a.AuthenticateRequest(upgradeRequest(t, svc, "not-a-session-token")); !errors.Is(err, signal.ErrInvalid) {
		t.Errorf("with a cookie that is no session: %v, want ErrInvalid", err)
	}
	// An Authorization header counts for nothing on /ws, with or without the cookie (03 §7.6).
	withHeader := upgradeRequest(t, svc, login.Token)
	withHeader.Header.Set("Authorization", "Bearer abc")
	id, err := a.AuthenticateRequest(withHeader)
	want := signal.Identity{UserID: string(login.User.ID), Name: "Alex", Admin: true, SessionID: string(login.Session.ID)}
	if err != nil || id != want {
		t.Fatalf("AuthenticateRequest = %+v, %v; want %+v", id, err, want)
	}

	if got, err := a.Revalidate(ctx, id, wireClientIP); err != nil || got != want {
		t.Errorf("Revalidate = %+v, %v; want %+v", got, err, want)
	}

	// A change made behind the service's back (the CLI's set-role goes through the store too) shows at the next
	// Revalidate: the user is read again each time.
	write := func(fn func(q *store.Q) error) {
		t.Helper()
		if err := db.Write(ctx, fn); err != nil {
			t.Fatal(err)
		}
	}
	write(func(q *store.Q) error { return q.SetRole(login.User.ID, store.RoleUser, time.Now()) })
	demoted := want
	demoted.Admin = false
	if got, err := a.Revalidate(ctx, id, wireClientIP); err != nil || got != demoted {
		t.Errorf("Revalidate after the role change = %+v, %v; want %+v", got, err, demoted)
	}
	write(func(q *store.Q) error { return q.SetStatus(login.User.ID, store.StatusDisabled, time.Now()) })
	if _, err := a.Revalidate(ctx, id, wireClientIP); !errors.Is(err, signal.ErrInvalid) {
		t.Errorf("Revalidate of a disabled user: %v, want ErrInvalid", err)
	}
	write(func(q *store.Q) error { return q.SetStatus(login.User.ID, store.StatusActive, time.Now()) })

	// Logout deletes the session: both calls say so.
	p := auth.Principal{UserID: login.User.ID, Username: "Alex", Role: store.RoleUser, Method: auth.MethodSession, SessionID: login.Session.ID}
	if err := svc.Logout(ctx, p, auth.ReqMeta{IP: wireClientIP}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Revalidate(ctx, id, wireClientIP); !errors.Is(err, signal.ErrInvalid) {
		t.Errorf("Revalidate after logout: %v, want ErrInvalid", err)
	}
	if _, err := a.AuthenticateRequest(upgradeRequest(t, svc, login.Token)); !errors.Is(err, signal.ErrInvalid) {
		t.Errorf("AuthenticateRequest after logout: %v, want ErrInvalid", err)
	}

	// A store that does not answer is no verdict on the credential: the error passes through, and the hub answers
	// 503 or keeps the connection.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = a.AuthenticateRequest(upgradeRequest(t, svc, login.Token))
	if err == nil || errors.Is(err, signal.ErrInvalid) || errors.Is(err, signal.ErrNoCredentials) {
		t.Errorf("AuthenticateRequest on a closed store: %v, want a transient error", err)
	}
}

// TestAuthenticatorOverAuthServiceDevice runs the bearer half of the adapter against 03's real service (M2 02
// §6.3): a device linked through the device flow says hello with its access token, and its identity carries the
// device's ID and name; a rename reaches the next Revalidate, a revocation makes both calls say ErrInvalid.
func TestAuthenticatorOverAuthServiceDevice(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	svc := newTestAccounts(t, db, nil)
	a := &authenticator{sessions: svc, user: userReader(db)}
	login := setUpAdmin(t, svc, "Alex")
	browser := auth.Principal{UserID: login.User.ID, Username: "Alex", Role: store.RoleAdmin,
		Method: auth.MethodSession, SessionID: login.Session.ID, ClientKind: "web"}
	meta := auth.ReqMeta{IP: wireClientIP}

	// The device flow (M2 02 §5): the app starts, the browser approves, the app polls.
	code, err := svc.StartDeviceFlow(ctx, api.DeviceCodeRequest{ClientKind: "desktop", DeviceName: "Gaming PC",
		OS: "windows", AppVersion: "0.2.0"}, meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DecideUserCode(ctx, browser, api.DeviceDecideRequest{UserCode: code.UserCode, Approve: true}, meta); err != nil {
		t.Fatal(err)
	}
	pair, err := svc.DeviceToken(ctx, api.DeviceTokenRequest{GrantType: api.GrantTypeDeviceCode, DeviceCode: code.DeviceCode}, meta)
	if err != nil {
		t.Fatal(err)
	}

	id, err := a.AuthenticateBearer(ctx, protocol.Secret(pair.AccessToken))
	want := signal.Identity{UserID: string(login.User.ID), Name: "Alex", Admin: true, DeviceID: pair.Device.ID,
		DeviceName: "Gaming PC"}
	if err != nil || id != want {
		t.Fatalf("AuthenticateBearer = %+v, %v; want %+v", id, err, want)
	}
	if got, err := a.Revalidate(ctx, id, wireClientIP); err != nil || got != want {
		t.Errorf("Revalidate = %+v, %v; want %+v", got, err, want)
	}

	// A rename on the Devices page reaches the next Revalidate (and a new hello).
	if _, err := svc.RenameDevice(ctx, browser, store.DeviceID(pair.Device.ID), "Living room PC", meta); err != nil {
		t.Fatal(err)
	}
	renamed := want
	renamed.DeviceName = "Living room PC"
	if got, err := a.Revalidate(ctx, id, wireClientIP); err != nil || got != renamed {
		t.Errorf("Revalidate after the rename = %+v, %v; want %+v", got, err, renamed)
	}
	if got, err := a.AuthenticateBearer(ctx, protocol.Secret(pair.AccessToken)); err != nil || got != renamed {
		t.Errorf("AuthenticateBearer after the rename = %+v, %v; want %+v", got, err, renamed)
	}

	// Tokens that are no access token of a live device: unknown, a refresh token, not a token at all.
	for _, tok := range []string{"isa_" + strings.Repeat("A", 43), pair.RefreshToken, "device-token", ""} {
		if got, err := a.AuthenticateBearer(ctx, protocol.Secret(tok)); !errors.Is(err, signal.ErrInvalid) || got != (signal.Identity{}) {
			t.Errorf("AuthenticateBearer(a token of length %d) = %+v, %v; want ErrInvalid", len(tok), got, err)
		}
	}

	// The device is removed on the Devices page: its connections are revoked at the next Revalidate, and its token no
	// longer says hello.
	if err := svc.RevokeDevice(ctx, browser, store.DeviceID(pair.Device.ID), meta); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Revalidate(ctx, id, wireClientIP); !errors.Is(err, signal.ErrInvalid) {
		t.Errorf("Revalidate after the revocation: %v, want ErrInvalid", err)
	}
	if _, err := a.AuthenticateBearer(ctx, protocol.Secret(pair.AccessToken)); !errors.Is(err, signal.ErrInvalid) {
		t.Errorf("AuthenticateBearer after the revocation: %v, want ErrInvalid", err)
	}

	// A store that does not answer is no verdict on the token: the error passes through (the hub answers internal).
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	other := "isa_" + strings.Repeat("B", 42) + "A" // well-formed (canonical base64url), not in the bearer cache
	if _, err := a.AuthenticateBearer(ctx, protocol.Secret(other)); err == nil || errors.Is(err, signal.ErrInvalid) {
		t.Errorf("AuthenticateBearer on a closed store: %v, want a transient error", err)
	}
}

// ---- signal.RoomDirectory and Policy ----

func TestRoomDirectory(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	rooms := roomDirectory{db: db}
	movie := store.Room{Name: "🎬 Movie night", NameKey: "🎬 movie night"}
	if err := db.Write(ctx, func(q *store.Q) error { return q.CreateRoom(&movie) }); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, id string
		want     protocol.RoomInfo
		wantErr  error
	}{
		{name: "the default room", id: "lounge", want: protocol.RoomInfo{ID: "lounge", Name: "Lounge"}},
		{name: "a room an admin made", id: string(movie.ID), want: protocol.RoomInfo{ID: string(movie.ID), Name: "🎬 Movie night"}},
		{name: "an unknown room", id: "0123456789ab", wantErr: signal.ErrNotFound},
		{name: "no id", id: "", wantErr: signal.ErrNotFound},
		{name: "the name is no id", id: "Lounge", wantErr: signal.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rooms.GetRoom(ctx, tc.id)
			if !errors.Is(err, tc.wantErr) || got != tc.want {
				t.Errorf("GetRoom(%q) = %+v, %v; want %+v, %v", tc.id, got, err, tc.want, tc.wantErr)
			}
		})
	}

	// The default room is the store's, and a new database has it.
	def, err := rooms.DefaultRoomID(ctx)
	if err != nil || def != string(store.DefaultRoomID) {
		t.Errorf("DefaultRoomID = %q, %v; want %q", def, err, store.DefaultRoomID)
	}
	if _, err := rooms.GetRoom(ctx, def); err != nil {
		t.Errorf("the default room does not exist: %v", err)
	}
	if err := rooms.CanJoin(ctx, signal.Identity{UserID: "u1"}, def); err != nil {
		t.Errorf("CanJoin = %v, want nil (no room locks in M1)", err)
	}

	// A renamed room is read with its new name: the directory keeps nothing.
	if err := db.Write(ctx, func(q *store.Q) error {
		return q.RenameRoom(store.DefaultRoomID, "Living room", "living room", time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := rooms.GetRoom(ctx, "lounge"); err != nil || got.Name != "Living room" {
		t.Errorf("GetRoom after a rename = %+v, %v", got, err)
	}

	// A store that does not answer is not "no such room": the hub answers internal, not room_not_found.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := rooms.GetRoom(ctx, "lounge"); err == nil || errors.Is(err, signal.ErrNotFound) {
		t.Errorf("GetRoom on a closed store: %v, want an error that is not ErrNotFound", err)
	}
}

func TestPolicyOf(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  store.Settings
		want signal.Policy
	}{
		{name: "the defaults: no limits", set: store.Settings{RegistrationMode: store.ModeInvite, UpdateCheck: true}},
		{name: "every limit set",
			set:  store.Settings{MinClientVersion: "0.3.0", MaxParticipantsPerRoom: 12, MaxSharesPerRoom: 4, MaxShareBitrateKbps: 2500},
			want: signal.Policy{MinClientVersion: "0.3.0", MaxRoomParticipants: 12, MaxRoomShares: 4, MaxVideoBitrate: 2_500_000}},
		{name: "the highest bitrate fits", set: store.Settings{MaxShareBitrateKbps: 100_000},
			want: signal.Policy{MaxVideoBitrate: 100_000_000}},
		{name: "other settings are not policy", set: store.Settings{ServerName: "Movie club", TransferAlertGB: 500, MembersCanInvite: true}},
	} {
		if got := policyOf(tc.set); got != tc.want {
			t.Errorf("%s: policyOf = %+v, want %+v", tc.name, got, tc.want)
		}
	}
	// The hub reads the settings as they are now: the defaults of a new database set no limit.
	if got := policyOf(openTestStore(t).Settings().Get()); got != (signal.Policy{}) {
		t.Errorf("policy of a new database = %+v, want no limits", got)
	}
}

// ---- auth.ConnCloser ----

// fakeHub records the calls of the adapters over the hub.
type fakeHub struct {
	snapshot signal.LiveSnapshot
	closes   int // what CloseConnections returns

	selectors []signal.ConnSelector
	codes     []protocol.ErrorCode
	calls     []string
}

func (f *fakeHub) CloseConnections(sel signal.ConnSelector, code protocol.ErrorCode) int {
	f.selectors, f.codes = append(f.selectors, sel), append(f.codes, code)
	return f.closes
}

func (f *fakeHub) Snapshot() signal.LiveSnapshot { return f.snapshot }

func (f *fakeHub) CloseRoom(roomID string) { f.calls = append(f.calls, "CloseRoom "+roomID) }

func (f *fakeHub) UpdateUser(userID, name string, admin bool) {
	f.calls = append(f.calls, fmt.Sprintf("UpdateUser %s %s %t", userID, name, admin))
}

func (f *fakeHub) Notify(t signal.Target, topics ...protocol.Topic) {
	f.calls = append(f.calls, fmt.Sprintf("Notify %+v %v", t, topics))
}

func TestConnCloser(t *testing.T) {
	// Every reason of 03 §7.7 with its code on the wire (01 §15.4): only account_disabled has its own.
	for reason, want := range map[string]protocol.ErrorCode{
		auth.ReasonLoggedOut:        protocol.ErrorCodeSessionRevoked,
		auth.ReasonSessionRevoked:   protocol.ErrorCodeSessionRevoked,
		auth.ReasonPasswordChanged:  protocol.ErrorCodeSessionRevoked,
		auth.ReasonPasswordReset:    protocol.ErrorCodeSessionRevoked,
		auth.ReasonAccountDisabled:  protocol.ErrorCodeAccountDisabled,
		auth.ReasonAccountDeleted:   protocol.ErrorCodeSessionRevoked,
		auth.ReasonDeviceRevoked:    protocol.ErrorCodeSessionRevoked,
		"a_reason_of_a_later_slice": protocol.ErrorCodeSessionRevoked,
		"":                          protocol.ErrorCodeSessionRevoked,
	} {
		hub := &fakeHub{closes: 3}
		closer := &connCloser{}
		closer.bind(hub)
		if n := closer.CloseConnections(auth.ConnSelector{UserID: "u1"}, reason); n != 3 {
			t.Errorf("reason %q: closed %d, want the hub's 3", reason, n)
		}
		if len(hub.codes) != 1 || hub.codes[0] != want {
			t.Errorf("reason %q: codes %v, want %s", reason, hub.codes, want)
		}
	}

	for _, tc := range []struct {
		name string
		sel  auth.ConnSelector
		want signal.ConnSelector
	}{
		{"every connection of a user", auth.ConnSelector{UserID: "u1"}, signal.ConnSelector{UserID: "u1"}},
		{"one session", auth.ConnSelector{UserID: "u1", SessionID: "s1"}, signal.ConnSelector{UserID: "u1", SessionID: "s1"}},
		{"one device", auth.ConnSelector{UserID: "u1", DeviceID: "d1"}, signal.ConnSelector{UserID: "u1", DeviceID: "d1"}},
		{"all but this session", auth.ConnSelector{UserID: "u1", ExceptSessionID: "s2"}, signal.ConnSelector{UserID: "u1", ExceptSessionID: "s2"}},
		{"every field", auth.ConnSelector{UserID: "u1", SessionID: "s1", DeviceID: "d1", ExceptSessionID: "s2"},
			signal.ConnSelector{UserID: "u1", SessionID: "s1", DeviceID: "d1", ExceptSessionID: "s2"}},
	} {
		hub := &fakeHub{}
		closer := &connCloser{}
		closer.bind(hub)
		closer.CloseConnections(tc.sel, auth.ReasonLoggedOut)
		if len(hub.selectors) != 1 || hub.selectors[0] != tc.want {
			t.Errorf("%s: selectors %+v, want %+v", tc.name, hub.selectors, tc.want)
		}
	}

	// Before the hub exists (auth's own startup) there is nothing to close.
	if n := (&connCloser{}).CloseConnections(auth.ConnSelector{UserID: "u1"}, auth.ReasonLoggedOut); n != 0 {
		t.Errorf("an unbound closer closed %d connections", n)
	}
}

// ---- httpapi.Signal ----

func TestSignalAdapter(t *testing.T) {
	conn := func(id string, status protocol.ConnectionStatus) signal.LiveConnection {
		return signal.LiveConnection{ID: id, Status: status}
	}
	share := func(id string, status protocol.ShareStatus) signal.LiveShare {
		return signal.LiveShare{Info: protocol.ShareInfo{ID: id, Status: status}}
	}
	hub := &fakeHub{snapshot: signal.LiveSnapshot{Rooms: []signal.LiveRoom{
		{ID: "lounge", Name: "Lounge",
			Participants: []signal.LiveParticipant{
				{UserID: "u1", Connections: []signal.LiveConnection{conn("c1", protocol.ConnectionStatusOnline), conn("c2", protocol.ConnectionStatusOnline)}},
				{UserID: "u2", Connections: []signal.LiveConnection{conn("c3", protocol.ConnectionStatusReconnecting)}},
			},
			Shares: []signal.LiveShare{share("sh1", protocol.ShareStatusLive), share("sh2", protocol.ShareStatusStarting)},
		},
		{ID: "movies", Name: "Movies",
			Participants: []signal.LiveParticipant{
				{UserID: "u1", Connections: []signal.LiveConnection{conn("c4", protocol.ConnectionStatusOnline)}},
				{UserID: "u3"}, // listed without a connection: not online
			},
			Shares: []signal.LiveShare{},
		},
	}}}
	a := signalAdapter{hub: hub}

	wantPresence := map[store.RoomID]httpapi.RoomPresence{
		"lounge": {Participants: 2, Shares: 2},
		"movies": {Participants: 2, Shares: 0},
	}
	if got := a.RoomPresence(); !reflect.DeepEqual(got, wantPresence) {
		t.Errorf("RoomPresence = %v, want %v", got, wantPresence)
	}
	wantOnline := map[store.UserID]struct{}{"u1": {}, "u2": {}}
	if got := a.OnlineUserIDs(); !reflect.DeepEqual(got, wantOnline) {
		t.Errorf("OnlineUserIDs = %v, want %v", got, wantOnline)
	}

	// Nobody connected: empty maps, never nil (the handlers index them).
	empty := signalAdapter{hub: &fakeHub{}}
	if got := empty.RoomPresence(); got == nil || len(got) != 0 {
		t.Errorf("RoomPresence of an empty hub = %v", got)
	}
	if got := empty.OnlineUserIDs(); got == nil || len(got) != 0 {
		t.Errorf("OnlineUserIDs of an empty hub = %v", got)
	}

	for _, tc := range []struct {
		name string
		call func()
		want string
	}{
		{"RoomDeleted", func() { a.RoomDeleted("movies") }, "CloseRoom movies"},
		{"UserChanged to admin", func() { a.UserChanged("u1", "Alexandra", true) }, "UpdateUser u1 Alexandra true"},
		{"UserChanged to member", func() { a.UserChanged("u2", "Sam", false) }, "UpdateUser u2 Sam false"},
		{"Notify a user", func() { a.Notify(httpapi.NotifyTarget{UserID: "u1"}, protocol.TopicDevices) },
			"Notify {UserID:u1 RoomID: Admins:false All:false} [devices]"},
		{"Notify the admins", func() {
			a.Notify(httpapi.NotifyTarget{Admins: true}, protocol.TopicAdminUsers, protocol.TopicAdminApprovals)
		}, "Notify {UserID: RoomID: Admins:true All:false} [admin.users admin.approvals]"},
		{"Notify everyone", func() { a.Notify(httpapi.NotifyTarget{All: true}, protocol.TopicRooms) },
			"Notify {UserID: RoomID: Admins:false All:true} [rooms]"},
		{"Notify a user and the admins", func() { a.Notify(httpapi.NotifyTarget{UserID: "u2", Admins: true}, protocol.TopicMe) },
			"Notify {UserID:u2 RoomID: Admins:true All:false} [me]"},
	} {
		hub.calls = nil
		tc.call()
		if !slices.Equal(hub.calls, []string{tc.want}) {
			t.Errorf("%s: hub calls %q, want [%q]", tc.name, hub.calls, tc.want)
		}
	}
}

func TestBuildInfo(t *testing.T) {
	var info httpapi.InfoSource = buildInfo{}
	if got := info.ServerVersion(); got != version.Version() || got == "" {
		t.Errorf("ServerVersion = %q, want %q", got, version.Version())
	}
	if cur, minimum := info.Protocol(); cur != protocol.Version || minimum != protocol.MinVersion || minimum > cur {
		t.Errorf("Protocol = %d, %d; want %d, %d", cur, minimum, protocol.Version, protocol.MinVersion)
	}
}

// ---- ops.AdminAccounts ----

// fakeAccounts is an accountService that records its calls.
type fakeAccounts struct {
	available bool
	user      store.User
	userErr   error // of UserByUsername
	err       error // of every other call
	link      auth.Link

	calls []string
}

func (f *fakeAccounts) SetupAvailable(context.Context) (bool, error) { return f.available, f.err }

func (f *fakeAccounts) IssueSetupToken(_ context.Context, a store.Actor) (auth.Link, error) {
	f.calls = append(f.calls, fmt.Sprintf("IssueSetupToken as %s", a.Kind))
	return f.link, f.err
}

func (f *fakeAccounts) UserByUsername(_ context.Context, username string) (store.User, error) {
	f.calls = append(f.calls, "UserByUsername "+username)
	return f.user, f.userErr
}

func (f *fakeAccounts) IssuePasswordReset(_ context.Context, a store.Actor, id store.UserID, password string) (auth.Link, error) {
	f.calls = append(f.calls, fmt.Sprintf("IssuePasswordReset as %s for %s with password %q", a.Kind, id, password))
	return f.link, f.err
}

func (f *fakeAccounts) UpdateUser(_ context.Context, a store.Actor, id store.UserID, ch auth.UserChange) (store.User, error) {
	change := ""
	if ch.Username != nil {
		change += " username=" + *ch.Username
	}
	if ch.Role != nil {
		change += " role=" + string(*ch.Role)
	}
	if ch.Status != nil {
		change += " status=" + string(*ch.Status)
	}
	if ch.ActorPassword != "" {
		change += " with a password"
	}
	f.calls = append(f.calls, fmt.Sprintf("UpdateUser as %s for %s:%s", a.Kind, id, change))
	return f.user, f.err
}

func (f *fakeAccounts) CreateInvite(_ context.Context, a store.Actor, in auth.InviteInput) (store.Invite, auth.Link, error) {
	f.calls = append(f.calls, fmt.Sprintf("CreateInvite as %s: uses=%d hours=%d note=%q", a.Kind, in.MaxUses, in.ExpiresInHours, in.Note))
	return store.Invite{}, f.link, f.err
}

func TestAdminAccounts(t *testing.T) {
	ctx := context.Background()
	expires := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	alex := store.User{ID: "u1", Username: "Alex", Role: store.RoleAdmin, Status: store.StatusActive}
	link := auth.Link{URL: wireOrigin + "/reset#tok", ExpiresAt: expires}
	notFound := api.NewError(api.CodeUserNotFound)
	lastAdmin := api.NewError(api.CodeLastAdmin)

	for _, tc := range []struct {
		name      string
		fake      fakeAccounts
		call      func(a *adminAccounts) (ops.IssuedLink, error)
		wantCalls []string
		wantLink  string
		wantErr   error
	}{
		{name: "setup link", fake: fakeAccounts{link: auth.Link{URL: wireOrigin + "/setup#tok", ExpiresAt: expires}},
			call:      func(a *adminAccounts) (ops.IssuedLink, error) { return a.IssueSetupLink(ctx) },
			wantCalls: []string{"IssueSetupToken as cli"}, wantLink: wireOrigin + "/setup#tok"},
		{name: "setup link once an admin exists", fake: fakeAccounts{err: api.NewError(api.CodeSetupUnavailable)},
			call:      func(a *adminAccounts) (ops.IssuedLink, error) { return a.IssueSetupLink(ctx) },
			wantCalls: []string{"IssueSetupToken as cli"}, wantErr: api.NewError(api.CodeSetupUnavailable)},
		{name: "reset link", fake: fakeAccounts{user: alex, link: link},
			call: func(a *adminAccounts) (ops.IssuedLink, error) { return a.IssueResetLink(ctx, "alex") },
			// The CLI passes no password: the socket's peer check is its authentication (04 §12.1).
			wantCalls: []string{"UserByUsername alex", `IssuePasswordReset as cli for u1 with password ""`},
			wantLink:  link.URL},
		{name: "reset link for an unknown user", fake: fakeAccounts{userErr: notFound},
			call:      func(a *adminAccounts) (ops.IssuedLink, error) { return a.IssueResetLink(ctx, "nobody") },
			wantCalls: []string{"UserByUsername nobody"}, wantErr: notFound},
		{name: "make an admin", fake: fakeAccounts{user: alex},
			call: func(a *adminAccounts) (ops.IssuedLink, error) {
				return ops.IssuedLink{}, a.SetRole(ctx, "alex", api.RoleAdmin)
			},
			wantCalls: []string{"UserByUsername alex", "UpdateUser as cli for u1: role=admin"}},
		{name: "make a member", fake: fakeAccounts{user: alex},
			call: func(a *adminAccounts) (ops.IssuedLink, error) {
				return ops.IssuedLink{}, a.SetRole(ctx, "alex", api.RoleUser)
			},
			wantCalls: []string{"UserByUsername alex", "UpdateUser as cli for u1: role=user"}},
		{name: "the last admin stays one", fake: fakeAccounts{user: alex, err: lastAdmin},
			call: func(a *adminAccounts) (ops.IssuedLink, error) {
				return ops.IssuedLink{}, a.SetRole(ctx, "alex", api.RoleUser)
			},
			wantCalls: []string{"UserByUsername alex", "UpdateUser as cli for u1: role=user"}, wantErr: lastAdmin},
		{name: "set the role of an unknown user", fake: fakeAccounts{userErr: notFound},
			call: func(a *adminAccounts) (ops.IssuedLink, error) {
				return ops.IssuedLink{}, a.SetRole(ctx, "nobody", api.RoleAdmin)
			},
			wantCalls: []string{"UserByUsername nobody"}, wantErr: notFound},
		{name: "disable", fake: fakeAccounts{user: alex},
			call: func(a *adminAccounts) (ops.IssuedLink, error) {
				return ops.IssuedLink{}, a.SetDisabled(ctx, "alex", true)
			},
			wantCalls: []string{"UserByUsername alex", "UpdateUser as cli for u1: status=disabled"}},
		{name: "enable", fake: fakeAccounts{user: alex},
			call: func(a *adminAccounts) (ops.IssuedLink, error) {
				return ops.IssuedLink{}, a.SetDisabled(ctx, "alex", false)
			},
			wantCalls: []string{"UserByUsername alex", "UpdateUser as cli for u1: status=active"}},
		{name: "invite with the server's defaults", fake: fakeAccounts{link: link},
			call:      func(a *adminAccounts) (ops.IssuedLink, error) { return a.CreateInvite(ctx, 0, 0) },
			wantCalls: []string{`CreateInvite as cli: uses=0 hours=0 note=""`}, wantLink: link.URL},
		{name: "invite with uses and a lifetime", fake: fakeAccounts{link: link},
			call:      func(a *adminAccounts) (ops.IssuedLink, error) { return a.CreateInvite(ctx, 5, 48) },
			wantCalls: []string{`CreateInvite as cli: uses=5 hours=48 note=""`}, wantLink: link.URL},
		{name: "invite while registration is closed", fake: fakeAccounts{err: api.NewError(api.CodeRegistrationClosed)},
			call:      func(a *adminAccounts) (ops.IssuedLink, error) { return a.CreateInvite(ctx, 0, 0) },
			wantCalls: []string{`CreateInvite as cli: uses=0 hours=0 note=""`}, wantErr: api.NewError(api.CodeRegistrationClosed)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := tc.fake
			got, err := tc.call(&adminAccounts{auth: &fake})
			if !slices.Equal(fake.calls, tc.wantCalls) {
				t.Errorf("calls = %q, want %q", fake.calls, tc.wantCalls)
			}
			if tc.wantErr != nil {
				// 03's error reaches the socket unchanged: the same code, nothing wrapped around it.
				var want, ae *api.Error
				errors.As(tc.wantErr, &want)
				if !errors.As(err, &ae) || ae.Code != want.Code || got != (ops.IssuedLink{}) {
					t.Errorf("= %+v, %v; want the error %v", got, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("error = %v", err)
			}
			if tc.wantLink == "" {
				return
			}
			if got.URL.Reveal() != tc.wantLink || !got.ExpiresAt.Equal(expires) {
				t.Errorf("link = %q until %v, want %q until %v", got.URL.Reveal(), got.ExpiresAt, tc.wantLink, expires)
			}
			// The link carries its token: printed or logged by accident it shows nothing.
			if s := fmt.Sprintf("%v %+v %s", got, got, got.URL); strings.Contains(s, "tok") {
				t.Errorf("a printed link shows its token: %s", s)
			}
		})
	}

	t.Run("setup available", func(t *testing.T) {
		for _, want := range []bool{true, false} {
			if got, err := (&adminAccounts{auth: &fakeAccounts{available: want}}).SetupAvailable(ctx); err != nil || got != want {
				t.Errorf("SetupAvailable = %v, %v; want %v", got, err, want)
			}
		}
		if _, err := (&adminAccounts{auth: &fakeAccounts{err: internalFailure()}}).SetupAvailable(ctx); !errors.Is(err, errStoreDown) {
			t.Errorf("SetupAvailable error = %v, want the service's", err)
		}
	})

	t.Run("users", func(t *testing.T) {
		created := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
		seen := created.Add(36 * time.Hour)
		rows := []store.UserRow{
			{User: store.User{ID: "u1", Username: "Alex", Role: store.RoleAdmin, Status: store.StatusActive, CreatedAt: created}, LastSeenAt: seen},
			{User: store.User{ID: "u2", Username: "Sam", Role: store.RoleUser, Status: store.StatusDisabled, CreatedAt: created}},
			{User: store.User{ID: "u3", Username: "Kim", Role: store.RoleUser, Status: store.StatusPending, CreatedAt: created}},
		}
		a := &adminAccounts{users: func(context.Context) ([]store.UserRow, error) { return rows, nil }}
		got, err := a.Users(ctx)
		want := []ops.AdminUser{
			{ID: "u1", Username: "Alex", Role: api.RoleAdmin, Status: api.UserStatusActive, CreatedAt: api.WireTime(created), LastSeenAt: api.WireTime(seen)},
			{ID: "u2", Username: "Sam", Role: api.RoleUser, Status: api.UserStatusDisabled, CreatedAt: api.WireTime(created)},
			{ID: "u3", Username: "Kim", Role: api.RoleUser, Status: api.UserStatusPending, CreatedAt: api.WireTime(created)},
		}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("Users = %+v, %v; want %+v", got, err, want)
		}
		// A user who never signed in has no lastSeenAt on the socket (04 §12.2).
		b, err := json.Marshal(got[1])
		if err != nil || strings.Contains(string(b), "lastSeenAt") || !strings.Contains(string(b), `"createdAt":"2026-09-30T10:00:00.000Z"`) {
			t.Errorf("user JSON = %s, %v", b, err)
		}

		none := &adminAccounts{users: func(context.Context) ([]store.UserRow, error) { return nil, nil }}
		if got, err := none.Users(ctx); err != nil || got == nil || len(got) != 0 {
			t.Errorf("Users without rows = %#v, %v; want an empty list", got, err)
		}
		failing := &adminAccounts{users: func(context.Context) ([]store.UserRow, error) { return nil, errStoreDown }}
		if _, err := failing.Users(ctx); !errors.Is(err, errStoreDown) {
			t.Errorf("Users error = %v, want the store's", err)
		}
	})
}

// TestAdminAccountsOverAuthService: the admin socket's calls against 03's real service and store. The setup link
// that the adapter hands out is the one that creates the admin.
func TestAdminAccountsOverAuthService(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	svc := newTestAccounts(t, db, nil)
	a := &adminAccounts{auth: svc, users: userLister(db)}

	if ok, err := a.SetupAvailable(ctx); err != nil || !ok {
		t.Fatalf("SetupAvailable on a new database = %v, %v", ok, err)
	}
	if users, err := a.Users(ctx); err != nil || len(users) != 0 {
		t.Errorf("Users on a new database = %+v, %v", users, err)
	}
	first, err := a.IssueSetupLink(ctx)
	if err != nil || !strings.HasPrefix(first.URL.Reveal(), wireOrigin+"/setup#") {
		t.Fatalf("IssueSetupLink = %v, %v", first, err)
	}
	if left := time.Until(first.ExpiresAt); left < 23*time.Hour || left > 24*time.Hour+time.Minute {
		t.Errorf("the setup link is valid for %v, want 24 h", left)
	}
	// A second link cancels the first (04 §12.5).
	second, err := a.IssueSetupLink(ctx)
	if err != nil {
		t.Fatal(err)
	}
	in := auth.SetupInput{Token: setupToken(t, first.URL.Reveal()), Username: "Alex", Password: wirePassword}
	if _, err := svc.CompleteSetup(ctx, in, auth.ReqMeta{IP: wireClientIP}); !api.IsCode(err, api.CodeSetupTokenInvalid) {
		t.Errorf("setup with the replaced link: %v, want setup_token_invalid", err)
	}
	in.Token = setupToken(t, second.URL.Reveal())
	if _, err := svc.CompleteSetup(ctx, in, auth.ReqMeta{IP: wireClientIP}); err != nil {
		t.Fatalf("setup with the newest link: %v", err)
	}

	if ok, err := a.SetupAvailable(ctx); err != nil || ok {
		t.Errorf("SetupAvailable after setup = %v, %v", ok, err)
	}
	if _, err := a.IssueSetupLink(ctx); !api.IsCode(err, api.CodeSetupUnavailable) {
		t.Errorf("IssueSetupLink after setup: %v, want setup_unavailable", err)
	}
	users, err := a.Users(ctx)
	if err != nil || len(users) != 1 {
		t.Fatalf("Users = %+v, %v; want the admin", users, err)
	}
	if u := users[0]; u.Username != "Alex" || u.Role != api.RoleAdmin || u.Status != api.UserStatusActive ||
		u.ID == "" || time.Time(u.CreatedAt).IsZero() || time.Time(u.LastSeenAt).IsZero() {
		t.Errorf("the admin on the socket = %+v", u)
	}

	invite, err := a.CreateInvite(ctx, 0, 0)
	if err != nil || !strings.HasPrefix(invite.URL.Reveal(), wireOrigin+"/invite#") {
		t.Fatalf("CreateInvite = %v, %v", invite, err)
	}
	// 0 means the server's setting: a week (03 §9).
	if left := time.Until(invite.ExpiresAt); left < 167*time.Hour || left > 168*time.Hour+time.Minute {
		t.Errorf("the invite is valid for %v, want the setting's 168 h", left)
	}
	if _, err := a.CreateInvite(ctx, 5000, 0); !api.IsCode(err, api.CodeValidationFailed) {
		t.Errorf("CreateInvite with 5000 uses: %v, want validation_failed", err)
	}
	// Every call acted as the CLI (03 §12.6): that is how the audit log names it.
	var actors []string
	if err := db.Read(ctx, func(q *store.Q) error {
		page, err := q.ListAudit(store.AuditQuery{Limit: 50})
		for _, e := range page {
			if e.Action == "setup.token_issued" || e.Action == "invite.created" {
				actors = append(actors, string(e.Actor.Kind))
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(actors, []string{"cli", "cli", "cli"}) {
		t.Errorf("audit actors of the socket's calls = %v, want three times cli", actors)
	}
}

// ---- policy pins ----

// TestPolicyKeysMatchRegistry: the wiring pins exactly the policy keys of the config registry, each into a setting
// that 03 has.
func TestPolicyKeysMatchRegistry(t *testing.T) {
	var registry, wired []string
	for _, k := range config.Keys() {
		if k.Policy {
			registry = append(registry, k.Path)
		}
	}
	for _, pk := range policyKeys {
		wired = append(wired, pk.path)
	}
	slices.Sort(registry)
	slices.Sort(wired)
	if !slices.Equal(wired, registry) {
		t.Fatalf("the wiring pins %v, the registry's policy keys are %v", wired, registry)
	}

	// Pinning each key's default is accepted, so every Setting names a field of 03's settings.
	cfg := loadFlags(t)
	settings := openTestStore(t).Settings()
	for _, pk := range policyKeys {
		key, _ := config.Lookup(pk.path)
		if key.Setting == "" {
			t.Errorf("%s has no setting to pin", pk.path)
			continue
		}
		if err := settings.Pin(key.Setting, pk.value(cfg)); err != nil {
			t.Errorf("Pin(%s) with the default of %s: %v", key.Setting, pk.path, err)
		}
	}
}

// TestPolicyDefaultsMatchSettings is the check of 04 §4.6 and §17: the registry repeats 03's defaults for the docs
// and `config example` only, so the two must agree, and a config without policy keys must hold exactly them.
func TestPolicyDefaultsMatchSettings(t *testing.T) {
	raw, err := json.Marshal(openTestStore(t).Settings().Defaults())
	if err != nil {
		t.Fatal(err)
	}
	var defaults map[string]json.RawMessage
	if err := json.Unmarshal(raw, &defaults); err != nil {
		t.Fatal(err)
	}
	cfg := loadFlags(t)
	for _, pk := range policyKeys {
		key, _ := config.Lookup(pk.path)
		want, ok := defaults[key.Setting]
		if !ok {
			t.Errorf("%s pins %q, which is no field of store.Settings", pk.path, key.Setting)
			continue
		}
		for what, v := range map[string]any{"registry default": key.Default, "value of an unset key": pk.value(cfg)} {
			got, err := json.Marshal(v)
			if err != nil || string(got) != string(want) {
				t.Errorf("%s: the %s is %s, 03's default of %s is %s", pk.path, what, got, key.Setting, want)
			}
		}
	}
}

func TestPinPolicy(t *testing.T) {
	t.Run("nothing set, nothing pinned", func(t *testing.T) {
		settings := openTestStore(t).Settings()
		if err := pinPolicy(loadFlags(t), settings); err != nil {
			t.Fatal(err)
		}
		if locked := settings.Locked(); len(locked) != 0 {
			t.Errorf("Locked = %v, want none", locked)
		}
	})

	t.Run("a key the operator set is pinned", func(t *testing.T) {
		settings := openTestStore(t).Settings()
		cfg := loadFlags(t,
			"--registration.mode=closed", "--clients.min-version=0.3.0",
			"--limits.max-participants-per-room=12", "--limits.max-shares-per-room=3",
			"--limits.max-bitrate-kbps=2500", "--limits.transfer-alert-gb=500", "--updates.release-check=false")
		if err := pinPolicy(cfg, settings); err != nil {
			t.Fatal(err)
		}
		want := settings.Defaults()
		want.RegistrationMode, want.MinClientVersion = store.ModeClosed, "0.3.0"
		want.MaxParticipantsPerRoom, want.MaxSharesPerRoom, want.MaxShareBitrateKbps = 12, 3, 2500
		want.TransferAlertGB, want.UpdateCheck = 500, false
		if got := settings.Get(); got != want {
			t.Errorf("settings = %+v, want %+v", got, want)
		}
		wantLocked := []string{"registrationMode", "maxParticipantsPerRoom", "maxSharesPerRoom", "maxShareBitrateKbps",
			"transferAlertGb", "updateCheck", "minClientVersion"}
		if got := settings.Locked(); !slices.Equal(got, wantLocked) {
			t.Errorf("Locked = %v, want %v", got, wantLocked)
		}
		// The admin UI can't change a pinned field (03 §9).
		_, err := settings.Update(context.Background(), map[string]json.RawMessage{"registrationMode": json.RawMessage(`"invite"`)}, store.CLIActor)
		if !api.IsCode(err, api.CodeSettingLocked) {
			t.Errorf("Update of a pinned field: %v, want setting_locked", err)
		}
	})

	t.Run("a key set to its default is pinned too", func(t *testing.T) {
		// "Set by the operator" decides, not the value: registration.mode = "invite" in the file keeps an admin from
		// opening sign-ups.
		settings := openTestStore(t).Settings()
		if err := pinPolicy(loadFlags(t, "--registration.mode=invite", "--limits.max-bitrate-kbps=0"), settings); err != nil {
			t.Fatal(err)
		}
		if got := settings.Locked(); !slices.Equal(got, []string{"registrationMode", "maxShareBitrateKbps"}) {
			t.Errorf("Locked = %v", got)
		}
		if got := settings.Get(); got != settings.Defaults() {
			t.Errorf("settings = %+v, want the defaults", got)
		}
	})

	t.Run("a value changed in code is not pinned", func(t *testing.T) {
		// servertest.Options.Config changes values after the load: they are not the operator's (04 §18).
		settings := openTestStore(t).Settings()
		cfg := loadFlags(t)
		cfg.Registration.Mode = "closed"
		if err := pinPolicy(cfg, settings); err != nil {
			t.Fatal(err)
		}
		if got := settings.Locked(); len(got) != 0 || settings.Get().RegistrationMode != store.ModeInvite {
			t.Errorf("Locked = %v, mode %q; want nothing pinned", got, settings.Get().RegistrationMode)
		}
	})

	t.Run("a value the settings refuse is a config error", func(t *testing.T) {
		settings := openTestStore(t).Settings()
		cfg := loadFlags(t, "--limits.max-bitrate-kbps=100", "--limits.max-shares-per-room=-1", "--registration.mode=closed")
		err := pinPolicy(cfg, settings)
		var ve *config.ValidationError
		if !errors.As(err, &ve) || !NeedsOperator(err) {
			t.Fatalf("pinPolicy = %v, want a *config.ValidationError (exit 78)", err)
		}
		if len(ve.Problems) != 2 {
			t.Fatalf("problems = %+v, want one per refused key", ve.Problems)
		}
		want := map[string]string{"limits.max_shares_per_room": "-1", "limits.max_bitrate_kbps": "100"}
		for _, p := range ve.Problems {
			value, ok := want[p.Key]
			if !ok || p.Value != value || p.Severity != config.SeverityError || p.Source.Kind != config.SourceFlag ||
				!strings.Contains(p.Message, api.FieldOutOfRange) || p.Fix == "" {
				t.Errorf("problem = %+v", p)
			}
		}
		// As the operator reads it: the key, its value, where it was set, 03's field code, and what the key takes.
		text := err.Error()
		for _, part := range []string{
			"config error: limits.max_bitrate_kbps = 100 (flag --limits.max-bitrate-kbps) is not a value the maxShareBitrateKbps setting takes (out_of_range).",
			"fix: A cap on any share's full layer in kbps (500 to 100000).",
		} {
			if !strings.Contains(text, part) {
				t.Errorf("the error does not say %q:\n%s", part, text)
			}
		}
	})

	t.Run("a string value is quoted in the error", func(t *testing.T) {
		if got := tomlValue("closed"); got != `"closed"` {
			t.Errorf("tomlValue of a string = %s", got)
		}
		if got := tomlValue(false); got != "false" {
			t.Errorf("tomlValue of a boolean = %s", got)
		}
	})
}

// TestIPKeysAgree is the check of 04 §17: netx's key of the per-IP connection limits and auth's key of the login
// throttles are the same for every address, so "one client" means one thing on every guard.
func TestIPKeysAgree(t *testing.T) {
	for _, addr := range []string{
		"203.0.113.7",
		"::ffff:203.0.113.7", // IPv4-mapped IPv6 is unmapped first
		"2001:db8:1:2::1",
		"2001:db8:1:2:ffff:ffff:ffff:ffff", // the same /64
		"2001:db8:1:3::1",                  // the neighbouring /64
		"fe80::1%eth0",
		"::1",
		"127.0.0.1",
		"100.64.0.1",
	} {
		a := netip.MustParseAddr(addr)
		if n, au := netx.IPKey(a), auth.IPKey(a); n != au || !n.IsValid() {
			t.Errorf("IPKey(%s): netx %v, auth %v", addr, n, au)
		}
	}
	if n, au := netx.IPKey(netip.Addr{}), auth.IPKey(netip.Addr{}); n != au || n.IsValid() {
		t.Errorf("IPKey(zero): netx %v, auth %v; want the zero Prefix from both", n, au)
	}
	same := func(a, b string) bool {
		return auth.IPKey(netip.MustParseAddr(a)) == netx.IPKey(netip.MustParseAddr(b))
	}
	if !same("2001:db8:1:2::1", "2001:db8:1:2:ffff:ffff:ffff:ffff") || same("2001:db8:1:2::1", "2001:db8:1:3::1") ||
		!same("::ffff:203.0.113.7", "203.0.113.7") {
		t.Error("the two keys group addresses differently")
	}
}

// ---- the store's refusal ----

func TestStoreOpenError(t *testing.T) {
	backup := "/var/lib/isshoni/backups/pre-1-20261014T021500Z.db"
	tooNew := &store.SchemaTooNewError{DBVersion: 3, BinaryVersion: 1, LastAppVersion: "0.6.1", Backup: backup}

	systemd := storeOpenError(tooNew, false)
	if !errors.Is(systemd, store.ErrNeedsOperator) || !NeedsOperator(systemd) {
		t.Errorf("the completed error is no refusal any more: %v", systemd)
	}
	// 03's message first, then the command for the environment (04 §6.1 step 4).
	wantSystemd := tooNew.Error() + "\n  fix: sudo -u isshoni isshoni admin restore --offline " + backup + " && sudo systemctl start isshoni"
	if systemd.Error() != wantSystemd {
		t.Errorf("systemd form:\n%s\nwant:\n%s", systemd, wantSystemd)
	}
	docker := storeOpenError(fmt.Errorf("server: %w", tooNew), true)
	wantDocker := "docker compose stop && docker compose run --rm isshoni admin restore --offline " + backup + " && docker compose up -d"
	if !strings.HasSuffix(docker.Error(), "\n  fix: "+wantDocker) || !NeedsOperator(docker) {
		t.Errorf("Docker form: %s", docker)
	}

	// unchanged: the error comes back as it went in, with nothing added to its text.
	unchanged := func(got, in error) bool { return errors.Is(got, in) && got.Error() == in.Error() }
	// Without a backup there is nothing to restore: 03's message stands (install the newer isshoni).
	noBackup := &store.SchemaTooNewError{DBVersion: 3, BinaryVersion: 1, LastAppVersion: "0.6.1"}
	if got := storeOpenError(noBackup, false); !unchanged(got, noBackup) {
		t.Errorf("without a backup: %v", got)
	}
	// Every other error is the store's own: a refusal stays one, a runtime error stays one.
	corrupt := fmt.Errorf("store: file is not a database: %w", store.ErrNeedsOperator)
	if got := storeOpenError(corrupt, false); !unchanged(got, corrupt) || !NeedsOperator(got) {
		t.Errorf("a corrupt database: %v", got)
	}
	busy := errors.New("store: open: disk I/O error")
	if got := storeOpenError(busy, true); !unchanged(got, busy) || NeedsOperator(got) {
		t.Errorf("a runtime error: %v (refusal: %v)", got, NeedsOperator(got))
	}

	// The backup's path is one word of the command, whatever is in it: the operator pastes the line into a shell.
	for in, want := range map[string]string{
		backup:                           backup,
		"/srv/my data/backups/pre-1.db":  `'/srv/my data/backups/pre-1.db'`,
		"/srv/it's/pre-1.db":             `'/srv/it'\''s/pre-1.db'`,
		"/srv/$(reboot)/backups/pre.db":  `'/srv/$(reboot)/backups/pre.db'`,
		"C:/isshoni/backups/pre-1-x.db":  "C:/isshoni/backups/pre-1-x.db",
		"/srv/data;rm/backups/pre-1.db":  `'/srv/data;rm/backups/pre-1.db'`,
		"/srv/tab\there/backups/pre.db":  "'/srv/tab\there/backups/pre.db'",
		"/srv/ünï/backups/pre-1-2026.db": `'/srv/ünï/backups/pre-1-2026.db'`,
	} {
		got := storeOpenError(&store.SchemaTooNewError{DBVersion: 3, BinaryVersion: 1, LastAppVersion: "0.6.1", Backup: in}, false)
		if suffix := "\n  fix: sudo -u isshoni isshoni admin restore --offline " + want + " && sudo systemctl start isshoni"; !strings.HasSuffix(got.Error(), suffix) {
			t.Errorf("the backup %q in the command:\n%s\nwant the end:%s", in, got, suffix)
		}
	}
	// The command is doctor's own, so `isshoni doctor` on the stopped server prints the same one (04 §13.2 schema).
	if want := tooNew.Error() + "\n  fix: " + doctor.RestoreCommand(backup, true); storeOpenError(tooNew, true).Error() != want {
		t.Errorf("the server's command differs from doctor's:\n%s\nwant:\n%s", storeOpenError(tooNew, true), want)
	}
}

// ---- ops.MetaStore ----

// TestMetaStore: ops keeps its state in 03's meta table through metaStore. A key that was never written reads as
// "", which is not an error; a written one comes back, and a second write replaces it.
func TestMetaStore(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	meta := metaStore{db: db}

	if v, err := meta.Meta(ctx, ops.MetaDoctorLast); v != "" || err != nil {
		t.Errorf("a key that is not there = %q, %v; want \"\" and no error", v, err)
	}
	for _, value := range []string{`{"v":1}`, `{"v":1,"report":{}}`, ""} {
		if err := meta.SetMeta(ctx, ops.MetaDoctorLast, value); err != nil {
			t.Fatalf("SetMeta(%q): %v", value, err)
		}
		if v, err := meta.Meta(ctx, ops.MetaDoctorLast); v != value || err != nil {
			t.Errorf("after SetMeta(%q): %q, %v", value, v, err)
		}
	}
	// It is the store's own key: what metaStore wrote is what 03 reads.
	if err := meta.SetMeta(ctx, ops.MetaDoctorLast, "kept"); err != nil {
		t.Fatal(err)
	}
	var direct string
	err := db.Read(ctx, func(q *store.Q) error {
		var err error
		direct, err = q.GetMeta(ops.MetaDoctorLast)
		return err
	})
	if direct != "kept" || err != nil {
		t.Errorf("the meta table has %q, %v", direct, err)
	}

	// Any other failure of the store is passed on: a caller that ended, a closed database.
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := meta.Meta(gone, ops.MetaDoctorLast); err == nil {
		t.Error("Meta with an ended context returned no error")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := meta.Meta(ctx, ops.MetaDoctorLast); err == nil {
		t.Error("Meta on a closed store returned no error")
	}
	if err := meta.SetMeta(ctx, ops.MetaDoctorLast, "late"); err == nil {
		t.Error("SetMeta on a closed store returned no error")
	}
}

// ---- the router's SPAStatus hook ----

func TestSPAStatus(t *testing.T) {
	db := openTestStore(t)
	svc := newTestAccounts(t, db, nil)
	s := &Server{log: discardLog(), accounts: svc, run: context.Background()}
	check := func(when string, want map[string]int) {
		t.Helper()
		for path, status := range want {
			if got := s.spaStatus(path); got != status {
				t.Errorf("%s: spaStatus(%q) = %d, want %d", when, path, got, status)
			}
		}
	}
	// While no admin exists every page of the web app is served, the setup page included.
	check("before setup", map[string]int{
		"/": 200, "/login": 200, "/r/lounge": 200, "/setup": 200, "/setup/": 200, "/setup/done": 200, "/setups": 200,
	})
	setUpAdmin(t, svc, "Alex")
	// Afterwards the setup page is gone, and only that page (03 §12.6).
	after := map[string]int{
		"/": 200, "/login": 200, "/r/lounge": 200, "/setup": 404, "/setup/": 404, "/setup/done": 200, "/setups": 200,
		"/Setup": 200,
	}
	check("after setup", after)
	// "Done" is kept: it costs no read any more, so it also holds while the database is busy or gone.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	check("after setup, without the store", after)

	// Before setup, a store that does not answer serves the page: its own API calls then say what is wrong.
	gone := openTestStore(t)
	unknown := &Server{log: discardLog(), accounts: newTestAccounts(t, gone, nil), run: context.Background()}
	if err := gone.Close(); err != nil {
		t.Fatal(err)
	}
	if got := unknown.spaStatus("/setup"); got != 200 {
		t.Errorf("spaStatus(/setup) with a store that fails = %d, want 200", got)
	}
	// A server without a site has no accounts and serves no page anyway.
	if got := (&Server{log: discardLog(), run: context.Background()}).spaStatus("/setup"); got != 200 {
		t.Errorf("spaStatus(/setup) without accounts = %d, want 200", got)
	}
}

// ---- the connection test: ops.Prober over 02's SFU.Probe ----

// fakeProbeSFU records the probes it is asked for and answers with answer, or with err.
type fakeProbeSFU struct {
	asked  []string // "user transport offer"
	answer string
	err    error
}

func (f *fakeProbeSFU) Probe(_ context.Context, user sfu.UserID, t sfu.ProbeTransport, offerSDP string,
) (string, <-chan sfu.ProbeResult, error) {
	f.asked = append(f.asked, string(user)+" "+string(t)+" "+offerSDP)
	if f.err != nil {
		return "", nil, f.err
	}
	return f.answer, make(chan sfu.ProbeResult, 1), nil
}

// TestProber is the adapter row by row (04 §6.6, §7.7): the user and the transport reach the SFU unchanged, the
// result channel is dropped, and each of the SFU's errors becomes its answer.
func TestProber(t *testing.T) {
	f := &fakeProbeSFU{answer: "v=0 answer"}
	answer, err := prober{sfu: f}.Probe(context.Background(), "0123456789ab", "tcp7882", "v=0 offer")
	if err != nil || answer != "v=0 answer" {
		t.Errorf("Probe = %q, %v; want the SFU's answer", answer, err)
	}
	if want := []string{"0123456789ab tcp7882 v=0 offer"}; !slices.Equal(f.asked, want) {
		t.Errorf("the SFU was asked %q, want %q", f.asked, want)
	}

	for _, tc := range []struct {
		name   string
		err    error
		want   *api.Error // nil: a failure of the server's (500 internal)
		status int
	}{
		{name: "20 probes run", err: &sfu.Error{Code: sfu.CodeProbeLimit, Retryable: true, RetryAfter: 2500 * time.Millisecond},
			want: &api.Error{Code: api.CodeRateLimited, RetryAfter: 3}, status: http.StatusTooManyRequests},
		{name: "20 probes run, a wait under a second", err: &sfu.Error{Code: sfu.CodeProbeLimit, Retryable: true, RetryAfter: time.Millisecond},
			want: &api.Error{Code: api.CodeRateLimited, RetryAfter: 1}, status: http.StatusTooManyRequests},
		{name: "no mux for the transport", err: &sfu.Error{Code: sfu.CodeTransportDisabled},
			want:   &api.Error{Code: api.CodeTransportDisabled, Params: map[string]any{api.ParamTransport: api.TransportUDP}},
			status: http.StatusConflict},
		{name: "a bad offer", err: fmt.Errorf("wrapped: %w", &sfu.Error{Code: sfu.CodeBadSDP}),
			want: api.NewError(api.CodeBadSDP), status: http.StatusBadRequest},
		{name: "the SFU is closed", err: &sfu.Error{Code: sfu.CodeClosed},
			want: api.NewError(api.CodeNotReady), status: http.StatusServiceUnavailable},
		{name: "Pion failed", err: &sfu.Error{Code: sfu.CodeInternal, Retryable: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := prober{sfu: &fakeProbeSFU{err: tc.err}}.Probe(context.Background(), "u1", "udp", "v=0")
			var ae *api.Error
			if tc.want == nil {
				if !errors.Is(err, tc.err) || errors.As(err, &ae) {
					t.Errorf("err = %v, want %v wrapped, without an *api.Error", err, tc.err)
				}
				// The request is still on: a 500 of the server's, which httpapi logs at error.
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("err = %v, want no context error while the request is on", err)
				}
				return
			}
			if !errors.As(err, &ae) || !reflect.DeepEqual(ae, tc.want) {
				t.Fatalf("err = %#v, want %#v", err, tc.want)
			}
			if s := api.StatusOf(ae.Code); s != tc.status {
				t.Errorf("%s is %d, want %d", ae.Code, s, tc.status)
			}
		})
	}

	// The browser left while the SFU gathered: setLocalComplete returns the context's error, which the SFU answers
	// as sfu.internal without the cause. The prober joins the request's context error in, so that httpapi logs the
	// 500 of a client that went away at debug, not as a failure of the server's.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	timedOut, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancel()
	for _, end := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{"cancelled", cancelled, context.Canceled},
		{"timed out", timedOut, context.DeadlineExceeded},
	} {
		t.Run("the request ended during gathering, "+end.name, func(t *testing.T) {
			internal := &sfu.Error{Code: sfu.CodeInternal, Retryable: true}
			_, err := prober{sfu: &fakeProbeSFU{err: internal}}.Probe(end.ctx, "u1", "udp", "v=0")
			var ae *api.Error
			if !errors.Is(err, end.want) || !errors.Is(err, internal) || errors.As(err, &ae) {
				t.Errorf("err = %v, want %v and %v wrapped, without an *api.Error", err, internal, end.want)
			}
			// An error the prober maps stays that answer: the client is gone, and a 4xx or 503 logs no error.
			_, err = prober{sfu: &fakeProbeSFU{err: &sfu.Error{Code: sfu.CodeClosed}}}.Probe(end.ctx, "u1", "udp", "v=0")
			if !errors.As(err, &ae) || ae.Code != api.CodeNotReady {
				t.Errorf("sfu.closed: err = %#v, want %s", err, api.CodeNotReady)
			}
		})
	}
}

// TestConnTestPorts: a transport has a port when the Transport has its mux, and then it is the bound one.
func TestConnTestPorts(t *testing.T) {
	addrs := Addrs{
		HTTP:   &net.TCPAddr{IP: net.IPv4zero, Port: 80},
		HTTPS:  &net.TCPAddr{IP: net.IPv4zero, Port: 8443},
		ICEUDP: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 50000},
		ICETCP: &net.TCPAddr{Port: 50001},
	}
	socket := fakeUDPMux{addrs.ICEUDP}
	for _, tc := range []struct {
		name string
		tr   *netx.Transport
		want ops.ConnTestPorts
	}{
		{"no Transport", nil, ops.ConnTestPorts{}},
		{"every transport", &netx.Transport{UDPMux: socket, TCPMux443: fakeTCPMux{}, TCPMux7882: fakeTCPMux{}},
			ops.ConnTestPorts{UDP: 50000, TCP443: 8443, TCP7882: 50001}},
		{"off mode", &netx.Transport{UDPMux: socket, TCPMux7882: fakeTCPMux{}},
			ops.ConnTestPorts{UDP: 50000, TCP7882: 50001}},
		{"UDP off", &netx.Transport{TCPMux443: fakeTCPMux{}, TCPMux7882: fakeTCPMux{}},
			ops.ConnTestPorts{TCP443: 8443, TCP7882: 50001}},
	} {
		if got := connTestPorts(tc.tr, addrs); got != tc.want {
			t.Errorf("%s: connTestPorts = %+v, want %+v", tc.name, got, tc.want)
		}
	}
	// A mux whose listener has no address has no port: the endpoint answers transport_disabled for it.
	if got := connTestPorts(&netx.Transport{UDPMux: socket}, Addrs{}); got != (ops.ConnTestPorts{}) {
		t.Errorf("without addresses: connTestPorts = %+v, want none", got)
	}
	for addr, want := range map[net.Addr]int{
		nil: 0,
		&net.UnixAddr{Name: "/run/isshoni/admin.sock"}: 0,
		&net.IPAddr{IP: net.IPv4(192, 0, 2, 1)}:        0,
		&net.TCPAddr{IP: net.IPv6loopback, Port: 7882}: 7882,
	} {
		if got := portOf(addr); got != want {
			t.Errorf("portOf(%v) = %d, want %d", addr, got, want)
		}
	}
}

// ---- the admin socket's status ----

func TestStatusSource(t *testing.T) {
	clock := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	cfg := loadFlags(t, "--public-url=https://watch.example.com", "--listen.admin-socket=/run/isshoni/admin.sock")
	s, err := New(cfg, discardLog(), Deps{Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	// What Start has in place when it builds the status source: a server behind the operator's proxy, on a machine
	// with a 1:1 NAT.
	s.store = openTestStore(t)
	s.site = config.Site{Origin: "https://watch.example.com", Host: "watch.example.com", Hostname: "watch.example.com", TLSMode: config.TLSOff}
	s.public = netx.PublicAddrs{
		V4: netip.MustParseAddr("203.0.113.7"), V4Method: netx.MethodSTUN,
		V6: netip.MustParseAddr("2001:db8::7"), V6Method: netx.MethodInterface,
		LocalV4: netip.MustParseAddr("10.0.0.5"), NAT: api.NATKindOneToOne,
	}
	// The Transport as netx builds it there: one UDP socket on the local address, the ICE-TCP listener on every
	// address, and candidates that name the public addresses.
	s.transport = &netx.Transport{
		UDPMux: fakeUDPMux{&net.UDPAddr{IP: net.IPv4(10, 0, 0, 5), Port: 7882}},
		Advertised: []netx.AdvertisedAddr{
			{Proto: "udp", Addr: netip.MustParseAddrPort("203.0.113.7:7882"), Via: netx.ViaUDP},
			{Proto: "udp", Addr: netip.MustParseAddrPort("[2001:db8::7]:7882"), Via: netx.ViaUDP},
			{Proto: "tcp", Addr: netip.MustParseAddrPort("203.0.113.7:7882"), Via: netx.ViaTCP7882},
		},
		RcvBuf: 8 << 20, SndBuf: 4 << 20,
	}
	s.addrs = Addrs{HTTP: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080}}
	s.addrs.ICEUDP, s.addrs.ICETCP = iceAddrs(cfg.Listen.ICETCP, s.transport)
	s.adminLn = listenStandIn(t)
	if s.tls, err = tlsmgr.New(tlsmgr.Options{Mode: config.TLSOff, Logger: discardLog()}); err != nil {
		t.Fatal(err)
	}
	status := s.statusSource()

	clock = clock.Add(90 * time.Second)
	got, err := status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := api.ServerStatus{
		Version:          version.Version(),
		StartedAt:        clock.Add(-90 * time.Second),
		UptimeS:          90,
		Origin:           "https://watch.example.com",
		TLS:              api.TLSInfo{Mode: api.TLSModeOff, Names: []string{}, Ready: true},
		PublicIPv4:       "203.0.113.7",
		PublicIPv4Method: "stun",
		PublicIPv6:       "2001:db8::7",
		PublicIPv6Method: "interface",
		LocalIPv4:        "10.0.0.5",
		NAT:              api.NATKindOneToOne,
		Advertised: []api.AdvertisedAddr{
			{Proto: "udp", Addr: "203.0.113.7:7882", Via: api.TransportUDP},
			{Proto: "udp", Addr: "[2001:db8::7]:7882", Via: api.TransportUDP},
			{Proto: "tcp", Addr: "203.0.113.7:7882", Via: api.TransportTCP7882},
		},
		Listeners: []api.ListenerInfo{
			{Key: "listen.http", Network: "tcp", Addr: "127.0.0.1:8080"},
			{Key: "listen.ice_udp", Network: "udp", Addr: "10.0.0.5:7882"},
			{Key: "listen.ice_tcp", Network: "tcp", Addr: ":7882"},
			{Key: "listen.admin_socket", Network: "unix", Addr: "/run/isshoni/admin.sock"},
		},
		UDPRcvBufBytes: 8 << 20,
		UDPSndBufBytes: 4 << 20,
		SchemaVersion:  store.LatestSchemaVersion(),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("status:\n got %+v\nwant %+v", got, want)
	}
	// The document is JSON for `isshoni admin status --json`: lists are lists, never null.
	b, err := json.Marshal(got)
	if err != nil || strings.Contains(string(b), "null") {
		t.Errorf("status JSON = %s, %v", b, err)
	}

	// In a TLS mode the 443 multiplexer comes first; a server that found no public address reports none. With UDP
	// and listen.ice_tcp both off, media has the multiplexer's port alone: no listener of its own, no UDP buffers.
	s.addrs.HTTPS = &net.TCPAddr{IP: net.IPv4zero, Port: 443}
	s.public = netx.PublicAddrs{}
	s.transport = &netx.Transport{Advertised: []netx.AdvertisedAddr{
		{Proto: "tcp", Addr: netip.MustParseAddrPort("10.0.0.5:443"), Via: netx.ViaTCP443, LAN: true},
	}}
	s.addrs.ICEUDP, s.addrs.ICETCP = iceAddrs("", s.transport)
	got, err = s.statusSource()(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Listeners[0] != (api.ListenerInfo{Key: "listen.https", Network: "tcp", Addr: "0.0.0.0:443"}) || len(got.Listeners) != 3 {
		t.Errorf("listeners in a TLS mode = %+v", got.Listeners)
	}
	if got.PublicIPv4 != "" || got.PublicIPv6 != "" || got.LocalIPv4 != "" || got.PublicIPv4Method != "" {
		t.Errorf("public addresses of a server that has none = %+v", got)
	}
	if got.UDPRcvBufBytes != 0 || got.UDPSndBufBytes != 0 ||
		!reflect.DeepEqual(got.Advertised, []api.AdvertisedAddr{{Proto: "tcp", Addr: "10.0.0.5:443", Via: api.TransportTCP443}}) {
		t.Errorf("media of a server on TCP 443 alone = %+v, buffers %d/%d", got.Advertised, got.UDPRcvBufBytes, got.UDPSndBufBytes)
	}

	// A Transport that advertises nothing still gives a list.
	if got := advertisedAddrs(nil); got == nil || len(got) != 0 {
		t.Errorf("advertisedAddrs(nil) = %#v, want an empty list", got)
	}

	// A server that started without media sockets (it waits for its public address on a machine without a local
	// one, listenICE) has no Transport: nothing is advertised, and the lists are still lists.
	s.transport = nil
	s.addrs.ICEUDP, s.addrs.ICETCP = iceAddrs(cfg.Listen.ICETCP, s.transport)
	got, err = s.statusSource()(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Advertised == nil || len(got.Advertised) != 0 || got.UDPRcvBufBytes != 0 || got.UDPSndBufBytes != 0 {
		t.Errorf("media of a server without a Transport = %#v, buffers %d/%d", got.Advertised, got.UDPRcvBufBytes, got.UDPSndBufBytes)
	}
	if len(got.Listeners) != 3 || got.Listeners[0].Key != "listen.https" || got.Listeners[1].Key != "listen.http" ||
		got.Listeners[2].Key != "listen.admin_socket" {
		t.Errorf("listeners of a server without a Transport = %+v", got.Listeners)
	}
	if b, err := json.Marshal(got); err != nil || strings.Contains(string(b), "null") {
		t.Errorf("status JSON = %s, %v", b, err)
	}

	// With the ops data (README S85): the metrics listener before the admin socket, the month's transfer as it is
	// counted, and no update before the release check's first look.
	counter := &netx.TransferCounter{}
	counter.Add(netx.PathWeb, true, 4000)
	counter.Add(netx.PathMediaUDP, false, 1000)
	s.transfer = ops.NewTransfer(ops.TransferOptions{Counter: counter, Store: transferStore{db: s.store},
		Now: func() time.Time { return clock }, Logger: discardLog()})
	if s.releases, err = ops.NewReleaseCheck(ops.ReleaseCheckOptions{Policy: opsPolicy{settings: s.store.Settings()},
		Logger: discardLog()}); err != nil {
		t.Fatal(err)
	}
	s.addrs.Metrics = &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9469}
	got, err = s.statusSource()(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tr := got.Transfer; tr == nil || tr.Month != "2026-10" || tr.EgressBytes != 4000 || tr.IngressBytes != 1000 ||
		tr.ProjectedEgressBytes <= tr.EgressBytes {
		t.Errorf("transfer = %+v, want October with 4000 bytes out, 1000 in, and a projection past day 3", tr)
	}
	if got.Update != nil {
		t.Errorf("update before the first release check = %+v", got.Update)
	}
	if len(got.Listeners) != 4 || got.Listeners[2] != (api.ListenerInfo{Key: "metrics.listen", Network: "tcp", Addr: "127.0.0.1:9469"}) {
		t.Errorf("listeners with the metrics listener = %+v", got.Listeners)
	}
}

// fakeUDPMux is an ice.UDPMux of which only the listen addresses are asked.
type fakeUDPMux []net.Addr

func (fakeUDPMux) Close() error { return nil }

func (fakeUDPMux) GetConn(string, net.Addr) (net.PacketConn, error) {
	return nil, errors.New("not asked in this test")
}

func (fakeUDPMux) RemoveConnByUfrag(string) {}

func (m fakeUDPMux) GetListenAddresses() []net.Addr { return m }

// fakeTCPMux is an ice.TCPMux that is only there.
type fakeTCPMux struct{}

func (fakeTCPMux) Close() error { return nil }

func (fakeTCPMux) GetConnByUfrag(string, bool, net.IP) (net.PacketConn, error) {
	return nil, errors.New("not asked in this test")
}

func (fakeTCPMux) RemoveConnByUfrag(string) {}

// listenStandIn is a listener that only stands for "the admin socket is bound".
func listenStandIn(t *testing.T) net.Listener {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// ---- the SFU behind the hub ----

func TestSFULimits(t *testing.T) {
	for kbps, want := range map[int]sfu.Limits{
		0:    {}, // no cap, in 03's settings as in the SFU
		4000: {MaxShareKbps: 4000},
	} {
		if got := sfuLimits(store.Settings{MaxShareBitrateKbps: kbps, MaxSharesPerRoom: 6}); got != want {
			t.Errorf("sfuLimits(maxShareBitrateKbps %d) = %+v, want %+v", kbps, got, want)
		}
	}
}

// TestMediaCheck is the readiness check "media" row by row (04 §6.2): a UDP socket, or an ICE-TCP mux when the
// operator turned UDP off, and an SFU that takes connections.
func TestMediaCheck(t *testing.T) {
	socket := fakeUDPMux{&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 7882}}
	open := func() error { return nil }
	closed := func() error { return &sfu.Error{Code: sfu.CodeClosed} }
	for _, tc := range []struct {
		name   string
		udp    bool // listen.ice_udp is set
		tr     *netx.Transport
		ready  func() error
		detail string // "" = ready
	}{
		{name: "UDP and TCP", udp: true, tr: &netx.Transport{UDPMux: socket, TCPMux: fakeTCPMux{}}, ready: open},
		{name: "UDP alone", udp: true, tr: &netx.Transport{UDPMux: socket}, ready: open},
		{name: "UDP turned off, an ICE-TCP mux", tr: &netx.Transport{TCPMux: fakeTCPMux{}}, ready: open},
		{name: "UDP wanted, no mux", udp: true, tr: &netx.Transport{TCPMux: fakeTCPMux{}}, ready: open,
			detail: "no UDP socket is bound for media (listen.ice_udp)"},
		{name: "UDP wanted, a mux without a socket", udp: true, tr: &netx.Transport{UDPMux: fakeUDPMux{}}, ready: open,
			detail: "no UDP socket is bound for media (listen.ice_udp)"},
		{name: "UDP turned off, no ICE-TCP mux", tr: &netx.Transport{}, ready: open,
			detail: "UDP is off (listen.ice_udp) and no ICE-TCP listener is up"},
		{name: "the SFU is closed", udp: true, tr: &netx.Transport{UDPMux: socket}, ready: closed,
			detail: "the media server is closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, detail := mediaCheck(tc.udp, tc.tr, tc.ready)()
			if ok != (tc.detail == "") || detail != tc.detail {
				t.Errorf("media check = %v %q, want %q", ok, detail, tc.detail)
			}
		})
	}
}

// ---- the readiness check "db" ----

func TestDBCheck(t *testing.T) {
	now := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	pings := 0
	var pingErr error
	check := newDBCheck(context.Background(), func(ctx context.Context) error {
		pings++
		if _, ok := ctx.Deadline(); !ok {
			t.Error("the ping has no deadline")
		}
		return pingErr
	}, func() time.Time { return now })

	want := func(what string, wantOK bool, wantDetail string, wantPings int) {
		t.Helper()
		ok, detail := check.ready()
		if ok != wantOK || detail != wantDetail || pings != wantPings {
			t.Errorf("%s: ready = %v, %q after %d pings; want %v, %q after %d", what, ok, detail, pings, wantOK, wantDetail, wantPings)
		}
	}
	want("the first call pings", true, "", 1)
	want("the answer is kept", true, "", 1)
	now = now.Add(dbPingEvery - time.Millisecond)
	want("until the interval is over", true, "", 1)

	now = now.Add(time.Millisecond)
	pingErr = errStoreDown
	want("then the database is asked again", false, "the database does not answer: database is locked", 2)
	pingErr = nil
	want("a failure is kept as long", false, "the database does not answer: database is locked", 2)
	now = now.Add(dbPingEvery)
	want("and it recovers with the next ping", true, "", 3)

	// A clock that steps back must not keep an old answer for good.
	now = now.Add(-time.Hour)
	want("after a clock step", true, "", 4)

	// A real store: ready while it is open, not ready once it is closed.
	db := openTestStore(t)
	real := newDBCheck(context.Background(), db.Ping, func() time.Time { return now })
	if ok, detail := real.ready(); !ok || detail != "" {
		t.Errorf("an open store: %v, %q", ok, detail)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(dbPingEvery)
	if ok, detail := real.ready(); ok || !strings.HasPrefix(detail, "the database does not answer: ") {
		t.Errorf("a closed store: %v, %q", ok, detail)
	}

	// Once the server's life has ended (the shutdown's last step) no ping goes out with a live context.
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	gone := newDBCheck(ended, func(ctx context.Context) error { return ctx.Err() }, time.Now)
	if ok, _ := gone.ready(); ok {
		t.Error("ready after the server's context ended")
	}
}

// TestDBCheckBehindASlowPing: a database that does not answer costs one ping, not one for each caller. The answer
// of a ping that took its whole timeout is kept from the moment it came, so the callers that waited for it take it
// instead of each waiting out a ping of their own, one after the other.
//
// The clock is the test's, and a ping moves it on by the timeout it uses up: the check holds its mutex over the
// ping, and testing/synctest does not move its time on while a goroutine waits for a mutex.
func TestDBCheckBehindASlowPing(t *testing.T) {
	start := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	var elapsed atomic.Int64 // what the check's clock shows, in nanoseconds after start
	var pings atomic.Int32
	out, queued := make(chan struct{}), make(chan struct{})
	check := newDBCheck(context.Background(), func(context.Context) error {
		if pings.Add(1) == 1 {
			close(out)
			<-queued // the first ping is out while the other callers come
		}
		elapsed.Add(int64(dbPingTimeout)) // no answer: the ping gives up when its time is over
		return context.DeadlineExceeded
	}, func() time.Time { return start.Add(time.Duration(elapsed.Load())) })

	const callers = 8
	type answer struct {
		ok     bool
		detail string
	}
	answers := make(chan answer, callers)
	for range callers {
		go func() {
			ok, detail := check.ready()
			answers <- answer{ok, detail}
		}()
	}
	select {
	case <-out:
	case <-time.After(10 * time.Second):
		t.Fatal("no ping went out")
	}
	close(queued)
	want := answer{false, "the database does not answer: context deadline exceeded"}
	for range callers {
		select {
		case got := <-answers:
			if got != want {
				t.Errorf("ready = %+v, want %+v", got, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a caller got no answer")
		}
	}
	if n := pings.Load(); n != 1 {
		t.Errorf("%d callers of a database that does not answer sent %d pings, want one", callers, n)
	}

	// The answer is as old as its arrival: the database is asked again one interval after that, not sooner.
	elapsed.Add(int64(dbPingEvery - time.Millisecond))
	if ok, _ := check.ready(); ok || pings.Load() != 1 {
		t.Errorf("just before the interval is over: ready = %v after %d pings, want the kept answer", ok, pings.Load())
	}
	elapsed.Add(int64(time.Millisecond))
	if ok, _ := check.ready(); ok || pings.Load() != 2 {
		t.Errorf("one interval after the answer: ready = %v after %d pings, want a second ping", ok, pings.Load())
	}
}

// ---- 04's push service: signal.PushNotifier, auth.AdminAlerter, httpapi.Push and push.Store ----

// fakePush is a pushService that records what the adapters pass on and answers what the test says.
type fakePush struct {
	shares    []push.ShareStarted
	alerts    []push.AdminAlert
	key       string
	endpoints []string
	tests     [][]push.Subscription
	err       error // ValidateEndpoint's and SendTest's
}

func (f *fakePush) ShareStarted(ev push.ShareStarted) { f.shares = append(f.shares, ev) }

func (f *fakePush) AdminAlert(a push.AdminAlert) { f.alerts = append(f.alerts, a) }

func (f *fakePush) VAPIDPublicKey() string { return f.key }

func (f *fakePush) ValidateEndpoint(_ context.Context, endpoint string) error {
	f.endpoints = append(f.endpoints, endpoint)
	return f.err
}

func (f *fakePush) SendTest(_ context.Context, subs []push.Subscription) error {
	f.tests = append(f.tests, subs)
	return f.err
}

// The hub's event becomes push's, field by field; the hub's context does not matter to a queue.
func TestPushNotifier(t *testing.T) {
	f := &fakePush{}
	at := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	pushNotifier{svc: f}.ShareStarted(gone, signal.PushShareStarted{
		RoomID: "lounge", RoomName: "Lounge", ShareID: "s_q7m2x9c4v8b1n5k3", UserID: "k3m9p2qxw7ht", UserName: "Alex",
		PresentUserIDs: []string{"k3m9p2qxw7ht", "a1b2c3d4e5f6"}, At: at,
	})
	pushNotifier{svc: f}.ShareStarted(context.Background(), signal.PushShareStarted{RoomID: "movies", ShareID: "s_2"})
	want := []push.ShareStarted{
		{
			RoomID: "lounge", RoomName: "Lounge", ShareID: "s_q7m2x9c4v8b1n5k3", UserID: "k3m9p2qxw7ht", UserName: "Alex",
			PresentUserIDs: []string{"k3m9p2qxw7ht", "a1b2c3d4e5f6"}, At: at,
		},
		{RoomID: "movies", ShareID: "s_2"},
	}
	if !reflect.DeepEqual(f.shares, want) {
		t.Errorf("push got %+v, want %+v", f.shares, want)
	}
	if len(f.alerts)+len(f.endpoints)+len(f.tests) != 0 {
		t.Error("a share event reached another method of the push service")
	}
}

// Every alert of 03 becomes push's admin.alert with its kind, actor, target and time. The kinds are the wire's
// (api.AdminAlertKind), which the service worker renders.
func TestAdminAlerter(t *testing.T) {
	at := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for _, in := range []auth.AdminAlert{
		{Kind: auth.AlertAdminGranted, Actor: "Alex", Target: "Kim", At: at},
		{Kind: auth.AlertAdminRevoked, Actor: "cli", Target: "Kim", At: at},
		{Kind: auth.AlertAdminPasswordReset, Actor: "Alex", Target: "Sam", At: at},
		{Kind: auth.AlertRegistrationModeChanged, Actor: "Alex", At: at},
		{Kind: auth.AlertSignupPending, Actor: "system", Target: "sam_k", At: at},
		{Kind: auth.AlertSecretsRotated, Actor: "system", At: at},
		{Kind: auth.AlertTransferThreshold, Actor: "system", Target: "80"},
		{Kind: auth.AlertRefreshTokenReused, Actor: "system", Target: "Kim", At: at},
	} {
		f := &fakePush{}
		adminAlerter{svc: f}.AdminAlert(context.Background(), in)
		want := []push.AdminAlert{{Kind: in.Kind, Actor: in.Actor, Target: in.Target, At: in.At}}
		if !reflect.DeepEqual(f.alerts, want) || len(f.shares) != 0 {
			t.Errorf("%s: push got %+v, want %+v", in.Kind, f.alerts, want)
		}
		if !slices.Contains([]api.AdminAlertKind{
			api.AdminAlertKindAdminGranted, api.AdminAlertKindAdminRevoked, api.AdminAlertKindAdminPasswordReset,
			api.AdminAlertKindRegistrationModeChanged, api.AdminAlertKindSignupPending,
			api.AdminAlertKindSecretsRotated, api.AdminAlertKindTransferThreshold, api.AdminAlertKindRefreshTokenReused,
		}, api.AdminAlertKind(in.Kind)) {
			t.Errorf("auth's alert kind %q is not one of api.AdminAlertKind", in.Kind)
		}
	}
}

// httpapi.Push: the key and the endpoint checks pass straight through, and the test notification takes 03's rows as
// push's subscriptions. The push service's errors are the REST answers, so they are passed on as they are.
func TestPushAPI(t *testing.T) {
	ctx := context.Background()
	f := &fakePush{key: "BPublicKey"}
	p := pushAPI{svc: f}
	if got := p.VAPIDPublicKey(); got != "BPublicKey" {
		t.Errorf("VAPIDPublicKey = %q", got)
	}
	if err := p.ValidateEndpoint(ctx, "https://push.example.net/send/1"); err != nil {
		t.Errorf("ValidateEndpoint = %v", err)
	}
	rows := []store.PushSubscription{
		{ID: "pushsub00001", UserID: "k3m9p2qxw7ht", SessionID: "sess00000001", Endpoint: "https://push.example.net/send/1",
			P256dh: "BP256", Auth: "auth1", Name: "Chrome on macOS", CreatedAt: time.Now(), Failures: 3},
		{ID: "pushsub00002", UserID: "k3m9p2qxw7ht", SessionID: "sess00000001", Endpoint: "https://push.example.org/2",
			P256dh: "BP256b", Auth: "auth2"},
	}
	if err := p.SendTest(ctx, rows); err != nil {
		t.Errorf("SendTest = %v", err)
	}
	want := [][]push.Subscription{{
		{ID: "pushsub00001", UserID: "k3m9p2qxw7ht", SessionID: "sess00000001", Endpoint: "https://push.example.net/send/1",
			P256dh: "BP256", Auth: "auth1"},
		{ID: "pushsub00002", UserID: "k3m9p2qxw7ht", SessionID: "sess00000001", Endpoint: "https://push.example.org/2",
			P256dh: "BP256b", Auth: "auth2"},
	}}
	if !reflect.DeepEqual(f.tests, want) || !slices.Equal(f.endpoints, []string{"https://push.example.net/send/1"}) {
		t.Errorf("push got tests %+v and endpoints %v", f.tests, f.endpoints)
	}

	for _, code := range []string{api.CodePushEndpointRejected, api.CodeServerBusy, api.CodeServerShutdown} {
		f.err = &api.Error{Code: code, RetryAfter: 5}
		if err := p.ValidateEndpoint(ctx, "https://push.example.net/x"); !errors.Is(err, f.err) {
			t.Errorf("ValidateEndpoint with %s = %v", code, err)
		}
		if err := p.SendTest(ctx, rows[:1]); !errors.Is(err, f.err) {
			t.Errorf("SendTest with %s = %v", code, err)
		}
	}
}

// push.RecipientFilter's preference names are 03's PushFilter's: the store adapter passes them as they are.
func TestPushPrefNamesMatchStore(t *testing.T) {
	if push.PrefShareStarted != store.PrefShareStarted || push.PrefAdminAlerts != store.PrefAdminAlerts {
		t.Errorf("push's preferences %q and %q, 03's %q and %q", push.PrefShareStarted, push.PrefAdminAlerts,
			store.PrefShareStarted, store.PrefAdminAlerts)
	}
}

// pushWorld is a database with users of every kind for the store adapter, each with a web session and a push
// subscription: two admins (Ana does not want admin alerts), two members (Kim has a second browser, Pat does not
// want share.started) and a disabled member.
type pushWorld struct {
	db       *store.DB
	users    map[string]store.UserID           // by username
	sessions map[string]store.SessionID        // by username: the session of its first subscription
	subs     map[string]store.PushSubscription // "<username>", and "Kim-2"
}

func newPushWorld(t *testing.T) *pushWorld {
	t.Helper()
	w := &pushWorld{db: openTestStore(t), users: map[string]store.UserID{}, sessions: map[string]store.SessionID{},
		subs: map[string]store.PushSubscription{}}
	now := time.Now()
	accounts := []struct {
		name   string
		role   store.Role
		status store.UserStatus
		prefs  *store.PushPreferences
		subs   int
	}{
		{name: "Alex", role: store.RoleAdmin, status: store.StatusActive, subs: 1},
		{name: "Ana", role: store.RoleAdmin, status: store.StatusActive, subs: 1,
			prefs: &store.PushPreferences{ShareStarted: "all", AdminAlerts: false}},
		{name: "Kim", role: store.RoleUser, status: store.StatusActive, subs: 2},
		{name: "Pat", role: store.RoleUser, status: store.StatusActive, subs: 1,
			prefs: &store.PushPreferences{ShareStarted: "off", AdminAlerts: true}},
		{name: "Dan", role: store.RoleUser, status: store.StatusDisabled, subs: 1},
	}
	err := w.db.Write(context.Background(), func(q *store.Q) error {
		for i, a := range accounts {
			u := store.User{Username: a.name, UsernameKey: strings.ToLower(a.name), PasswordHash: "x", Role: a.role,
				Status: a.status, CreatedVia: "invite"}
			if err := q.CreateUser(&u); err != nil {
				return err
			}
			w.users[a.name] = u.ID
			if a.prefs != nil {
				if err := q.SetPushPreferences(u.ID, *a.prefs, now); err != nil {
					return err
				}
			}
			for n := range a.subs {
				sess := store.Session{UserID: u.ID, TokenHash: []byte{byte(i), byte(n)}, IdleExpiresAt: now.Add(time.Hour),
					ExpiresAt: now.Add(time.Hour)}
				if err := q.CreateSession(&sess); err != nil {
					return err
				}
				name := a.name
				if n == 0 {
					w.sessions[a.name] = sess.ID
				} else {
					name += "-" + strconv.Itoa(n+1)
				}
				sub := store.PushSubscription{UserID: u.ID, SessionID: sess.ID, Endpoint: "https://push.example.net/" + name,
					P256dh: "p256dh-" + name, Auth: "auth-" + name, Name: "Firefox on Linux"}
				if _, err := q.UpsertPushSubscription(&sub); err != nil {
					return err
				}
				w.subs[name] = sub
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// sub is a subscription of the world as push sees it.
func (w *pushWorld) sub(name string) push.Subscription {
	s := w.subs[name]
	return push.Subscription{ID: string(s.ID), UserID: string(s.UserID), SessionID: string(s.SessionID),
		Endpoint: logx.Secret(s.Endpoint), P256dh: s.P256dh, Auth: s.Auth}
}

// row is a subscription's failures and last success as 03's table has them; ok is false when it is gone.
func (w *pushWorld) row(t *testing.T, name string) (failures int, lastSuccess time.Time, ok bool) {
	t.Helper()
	err := w.db.Read(context.Background(), func(q *store.Q) error {
		subs, err := q.ListPushSubscriptions(store.PushFilter{UserIDs: []store.UserID{w.subs[name].UserID}})
		for _, s := range subs {
			if s.ID == w.subs[name].ID {
				failures, lastSuccess, ok = s.Failures, s.LastSuccessAt, true
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return failures, lastSuccess, ok
}

// TestPushStore: the store adapter over 03's real tables (04 §14.7). Recipients applies every field of the filter
// and returns active users only; a result recorded for, or a deletion of, a subscription that is gone is no error;
// a meta key that is not there reads ""; any other error of the store is passed on.
func TestPushStore(t *testing.T) {
	ctx := context.Background()
	w := newPushWorld(t)
	st := pushStore{metaStore{db: w.db}}
	user := func(name string) string { return string(w.users[name]) }
	subID := func(name string) string { return string(w.subs[name].ID) }

	recipients := func(f push.RecipientFilter) []string {
		t.Helper()
		subs, err := st.Recipients(ctx, f)
		if err != nil {
			t.Fatalf("Recipients(%+v): %v", f, err)
		}
		var names []string
		for _, s := range subs {
			for name := range w.subs {
				if w.sub(name) == s {
					names = append(names, name)
				}
			}
		}
		if len(names) != len(subs) {
			t.Errorf("Recipients(%+v) returned subscriptions that are not the world's: %+v", f, subs)
		}
		slices.Sort(names)
		return names
	}
	for _, c := range []struct {
		name   string
		filter push.RecipientFilter
		want   []string
	}{
		{"every active user's", push.RecipientFilter{}, []string{"Alex", "Ana", "Kim", "Kim-2", "Pat"}},
		{"share.started: not the sharer, not the room, not Pat, who switched it off",
			push.RecipientFilter{ExcludeUserIDs: []string{user("Alex"), user("Kim")}, Pref: push.PrefShareStarted},
			[]string{"Ana"}},
		{"admin.alert: the admins who want them", push.RecipientFilter{AdminsOnly: true, Pref: push.PrefAdminAlerts},
			[]string{"Alex"}},
		{"push.test: one session's", push.RecipientFilter{SessionID: string(w.subs["Kim-2"].SessionID)}, []string{"Kim-2"}},
		{"a disabled user's, never", push.RecipientFilter{SessionID: string(w.sessions["Dan"])}, nil},
	} {
		if got := recipients(c.filter); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if _, err := st.Recipients(ctx, push.RecipientFilter{Pref: "sometimes"}); err == nil {
		t.Error("Recipients with an unknown preference returned no error")
	}

	at := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	for range 2 {
		if err := st.RecordResult(ctx, subID("Kim"), false, at); err != nil {
			t.Fatal(err)
		}
	}
	if failures, last, ok := w.row(t, "Kim"); !ok || failures != 2 || !last.IsZero() {
		t.Errorf("Kim's after two failures: %d failures, last success %v, there %v", failures, last, ok)
	}
	if err := st.RecordResult(ctx, subID("Kim"), true, at); err != nil {
		t.Fatal(err)
	}
	if failures, last, ok := w.row(t, "Kim"); !ok || failures != 0 || !last.Equal(at) {
		t.Errorf("Kim's after a success at %v: %d failures, last success %v, there %v", at, failures, last, ok)
	}
	if err := st.Delete(ctx, subID("Kim")); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := w.row(t, "Kim"); ok {
		t.Error("Kim's subscription is still there after Delete")
	}
	// Gone is gone: the service may record a result for a subscription that was deleted meanwhile.
	if err := st.RecordResult(ctx, subID("Kim"), true, at); err != nil {
		t.Errorf("RecordResult for a deleted subscription = %v, want nil", err)
	}
	if err := st.Delete(ctx, subID("Kim")); err != nil {
		t.Errorf("Delete of a deleted subscription = %v, want nil", err)
	}

	// The daily prune: 20 failures in a row and no success since the cut-off.
	for range 20 {
		if err := st.RecordResult(ctx, subID("Pat"), false, at); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := st.Prune(ctx, 20, at.Add(time.Hour)); n != 1 || err != nil {
		t.Errorf("Prune = %d, %v; want Pat's", n, err)
	}
	if _, _, ok := w.row(t, "Pat"); ok {
		t.Error("Pat's subscription is still there after the prune")
	}
	// A VAPID rotation deletes the rest, the disabled user's too.
	if n, err := st.DeleteAll(ctx); n != 4 || err != nil {
		t.Errorf("DeleteAll = %d, %v; want the 4 that were left", n, err)
	}

	if v, err := st.Meta(ctx, "vapid_key_fp"); v != "" || err != nil {
		t.Errorf("a meta key that is not there = %q, %v; want \"\" and no error", v, err)
	}
	if err := st.SetMeta(ctx, "vapid_key_fp", "0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if v, err := st.Meta(ctx, "vapid_key_fp"); v != "0123456789abcdef" || err != nil {
		t.Errorf("the meta key after SetMeta = %q, %v", v, err)
	}

	if err := w.db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Recipients(ctx, push.RecipientFilter{}); err == nil {
		t.Error("Recipients on a closed store returned no error")
	}
	if err := st.RecordResult(ctx, subID("Alex"), true, at); err == nil {
		t.Error("RecordResult on a closed store returned no error")
	}
	if err := st.Delete(ctx, subID("Alex")); err == nil {
		t.Error("Delete on a closed store returned no error")
	}
	if _, err := st.DeleteAll(ctx); err == nil {
		t.Error("DeleteAll on a closed store returned no error")
	}
	if _, err := st.Prune(ctx, 20, at); err == nil {
		t.Error("Prune on a closed store returned no error")
	}
}

// pushSubjectProblem turns the subject that config lets through and push.New refuses into a config error for the
// key, with its source and config's fix; every other subject is push.New's or config's business, and with push off
// no subject is. New reports it with config's own problems, errors first, before anything is opened: also on an
// ip-mode server that would start without a site, and so without a push service, until it finds its address.
func TestPushSubjectProblem(t *testing.T) {
	for _, subject := range []string{"", "mailto:ops@example.com", "https://watch.example.com", "https:x"} {
		if p, bad := pushSubjectProblem(loadFlags(t, "--push.subject="+subject)); bad {
			t.Errorf("push.subject = %q: %+v", subject, p)
		}
	}
	for _, subject := range []string{"mailto:", "https:"} {
		p, bad := pushSubjectProblem(loadFlags(t, "--push.subject="+subject))
		if !bad || p.Key != "push.subject" || p.Value != `"`+subject+`"` || p.Severity != config.SeverityError ||
			p.Source != (config.Source{Kind: config.SourceFlag, Name: "--push.subject"}) || p.Fix != pushSubjectFix {
			t.Errorf("push.subject = %q: %v %+v", subject, bad, p)
		}
		if p, bad := pushSubjectProblem(loadFlags(t, "--push.subject="+subject, "--push.enabled=false")); bad {
			t.Errorf("push.subject = %q with push off: %+v", subject, p)
		}
	}

	// New: one config error with config's own, sorted with them; nothing is opened.
	cfg := loadFlags(t, "--tls.mode=ip", "--listen.https=127.0.0.1:0", "--push.subject=mailto:", "--shutdown-timeout=soon")
	_, err := New(cfg, discardLog(), Deps{})
	var ve *config.ValidationError
	if !errors.As(err, &ve) || !NeedsOperator(err) {
		t.Fatalf("New with push.subject = \"mailto:\": %v, want a config error", err)
	}
	var errKeys []string
	for _, p := range ve.Problems {
		if p.Severity == config.SeverityError {
			errKeys = append(errKeys, p.Key)
		}
	}
	slices.Sort(errKeys)
	if !slices.Equal(errKeys, []string{"push.subject", "shutdown_timeout"}) || !slices.IsSortedFunc(ve.Problems,
		func(a, b config.Problem) int { return strings.Compare(a.Severity, b.Severity) }) {
		t.Errorf("New's problems: %v, want the push.subject and shutdown_timeout errors first", ve.Problems)
	}
	if _, err := os.Stat(cfg.DataDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("New refused the config, and the data directory is there: %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, `push.subject = "mailto:" (flag --push.subject) names no contact`) {
		t.Errorf("the error says %q", err)
	}

	// The fix is config's own, word for word: one text for the two halves of the rule.
	found := false
	for _, p := range loadFlags(t, "--push.subject=smtp:ops").Validate() {
		if p.Key == "push.subject" {
			found = true
			if p.Fix != pushSubjectFix {
				t.Errorf("config's fix for push.subject is %q, the wiring's %q", p.Fix, pushSubjectFix)
			}
		}
	}
	if !found {
		t.Error("config took push.subject = \"smtp:ops\"")
	}
}
