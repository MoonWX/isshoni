package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/auth"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/httpapi"
	"github.com/MoonWX/isshoni/internal/server/netx"
	"github.com/MoonWX/isshoni/internal/server/ops"
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

// openTestStore opens a new database in the test's directory and closes it with the test. closeNow closes it early,
// for the "store is gone" rows.
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

// fakeSessions is a sessionService that answers what the test says and records what Touch was asked.
type fakeSessions struct {
	principal auth.Principal
	cookieErr error
	touchErr  error

	touched []auth.Principal
	touchIP netip.Addr
}

func (f *fakeSessions) AuthenticateCookie(*http.Request) (auth.Principal, error) {
	if f.cookieErr != nil {
		return auth.Principal{}, f.cookieErr
	}
	return f.principal, nil
}

func (f *fakeSessions) Touch(_ context.Context, p auth.Principal, ip netip.Addr) error {
	f.touched, f.touchIP = append(f.touched, p), ip
	return f.touchErr
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

func TestAuthenticateBearer(t *testing.T) {
	a := &authenticator{sessions: &fakeSessions{}}
	if id, err := a.AuthenticateBearer(context.Background(), protocol.Secret("device-token")); !errors.Is(err, signal.ErrInvalid) || id != (signal.Identity{}) {
		t.Errorf("AuthenticateBearer = %+v, %v; want ErrInvalid (no device tokens before M2)", id, err)
	}
}

func TestRevalidate(t *testing.T) {
	session := signal.Identity{UserID: "u1", Name: "Alex", Admin: true, SessionID: "s1"}
	active := func(name string, role store.Role) store.User {
		return store.User{ID: "u1", Username: name, Role: role, Status: store.StatusActive}
	}

	for _, tc := range []struct {
		name     string
		id       signal.Identity
		touchErr error
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
		{name: "a device (M2)", id: signal.Identity{UserID: "u1", Name: "Alex", DeviceID: "d1"},
			touchErr: api.NewError(api.CodeUnauthenticated), wantErr: signal.ErrInvalid, noRead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions := &fakeSessions{touchErr: tc.touchErr}
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

	for in, want := range map[string]string{
		backup:                           backup,
		"/srv/my data/backups/pre-1.db":  `'/srv/my data/backups/pre-1.db'`,
		"/srv/it's/pre-1.db":             `'/srv/it'\''s/pre-1.db'`,
		"":                               `''`,
		"/srv/$(reboot)/backups/pre.db":  `'/srv/$(reboot)/backups/pre.db'`,
		"C:/isshoni/backups/pre-1-x.db":  "C:/isshoni/backups/pre-1-x.db",
		"/srv/data;rm/backups/pre-1.db":  `'/srv/data;rm/backups/pre-1.db'`,
		"/srv/tab\there/backups/pre.db":  "'/srv/tab\there/backups/pre.db'",
		"/srv/ünï/backups/pre-1-2026.db": `'/srv/ünï/backups/pre-1-2026.db'`,
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
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
	s.addrs = Addrs{HTTP: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080}}
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
		Advertised:       []api.AdvertisedAddr{},
		Listeners: []api.ListenerInfo{
			{Key: "listen.http", Network: "tcp", Addr: "127.0.0.1:8080"},
			{Key: "listen.admin_socket", Network: "unix", Addr: "/run/isshoni/admin.sock"},
		},
		SchemaVersion: store.LatestSchemaVersion(),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("status:\n got %+v\nwant %+v", got, want)
	}
	// The document is JSON for `isshoni admin status --json`: lists are lists, never null.
	b, err := json.Marshal(got)
	if err != nil || strings.Contains(string(b), "null") {
		t.Errorf("status JSON = %s, %v", b, err)
	}

	// In a TLS mode the 443 multiplexer comes first; a server that found no public address reports none.
	s.addrs.HTTPS = &net.TCPAddr{IP: net.IPv4zero, Port: 443}
	s.public = netx.PublicAddrs{}
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
}

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

// ---- the media plane before the SFU ----

func TestNoMedia(t *testing.T) {
	var plane signal.MediaPlane = noMedia{}
	peer, err := plane.NewPeer(signal.PeerParams{ConnectionID: "c1", UserID: "u1", RoomID: "lounge"}, nil)
	if err != nil || peer == nil {
		t.Fatalf("NewPeer = %v, %v: a connection must be able to join a room", peer, err)
	}

	wantError := func(what string, err error, scope protocol.ErrorScope, pc protocol.PCKind, gen, neg uint32) {
		t.Helper()
		var pe *protocol.Error
		if !errors.As(err, &pe) {
			t.Errorf("%s: error %v, want a *protocol.Error (anything else is logged as an internal error)", what, err)
			return
		}
		want := protocol.Error{Code: protocol.ErrorCodeFeatureDisabled, Scope: scope, PC: pc, Gen: gen, Neg: neg}
		if !reflect.DeepEqual(*pe, want) {
			t.Errorf("%s: error %+v, want %+v", what, *pe, want)
		}
	}
	_, err = peer.CreateShare("sh1", protocol.ShareStart{})
	wantError("CreateShare", err, protocol.ErrorScopeRequest, "", 0, 0)
	_, err = peer.UpdateShare("sh1", protocol.ShareUpdate{})
	wantError("UpdateShare", err, protocol.ErrorScopeRequest, "", 0, 0)
	_, err = peer.HandleOffer(protocol.PCOffer{PC: protocol.PCKindPub, Gen: 2, Neg: 3})
	wantError("HandleOffer", err, protocol.ErrorScopePC, protocol.PCKindPub, 2, 3)
	wantError("HandleAnswer", peer.HandleAnswer(protocol.PCAnswer{PC: protocol.PCKindSub, Gen: 1, Neg: 4}),
		protocol.ErrorScopePC, protocol.PCKindSub, 1, 4)
	wantError("AddICE", peer.AddICE(protocol.PCICE{PC: protocol.PCKindSub, Gen: 5}), protocol.ErrorScopePC, protocol.PCKindSub, 5, 0)
	wantError("Restart", peer.Restart(protocol.PCRestart{PC: protocol.PCKindSub, Gen: 6}), protocol.ErrorScopePC, protocol.PCKindSub, 6, 0)
	if err := peer.ClosePC(protocol.PCClose{PC: protocol.PCKindPub, Gen: 1}); err != nil {
		t.Errorf("ClosePC = %v: there is nothing to close", err)
	}

	// No share exists, so a subscription finds none of what it asks for.
	ignored, err := peer.Subscribe([]protocol.SubscriptionWant{{ShareID: "sh1"}, {ShareID: "sh2"}})
	if err != nil || !slices.Equal(ignored, []string{"sh1", "sh2"}) {
		t.Errorf("Subscribe = %v, %v; want both shares ignored", ignored, err)
	}
	if ignored, err := peer.Subscribe(nil); err != nil || ignored == nil || len(ignored) != 0 {
		t.Errorf("Subscribe to nothing = %#v, %v; want an empty list", ignored, err)
	}
	// The stats go on the wire as they are: their lists are lists.
	if b, err := json.Marshal(peer.Stats()); err != nil || string(b) != `{"subs":[],"layers":[]}` {
		t.Errorf("Stats JSON = %s, %v", b, err)
	}
	peer.EndShare("sh1", protocol.EndReasonStopped)
	peer.SetCaps(protocol.Caps{})
	peer.Resync()
	peer.Close()
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
