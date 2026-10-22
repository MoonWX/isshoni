package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// The revocation matrix of 03 §15 and README S58: for every row of 03 §7.7, a fake ConnCloser records the exact
// selector and reason, the rows the table names are gone and no others, and the session cache is invalidated before
// the connections are closed. With M2 (README WA39) every device of the fixture holds real tokens: the bearer cache
// holds them when an event starts, a row that takes the device forgets it before the connections are closed and its
// access token is refused afterwards, and the M2 rows (revoking a device, the app's own sign-out, a refresh-token
// reuse) are in the table.

// IDs of the devices the fixture links (the store's own IDs are 12 characters too).
const (
	alexDevice store.DeviceID = "da1da1da1da1"
	samDevice  store.DeviceID = "d5a5d5a5d5a5"
)

// seededTokens is how many device_tokens rows seedDevice gives a device.
const seededTokens = 3

// seededToken is a fixed token of the fixture's devices: prefix and 43 characters of the byte b (b's high nibble
// names the device, its low nibble the token).
func seededToken(prefix string, b byte) string {
	return prefix + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{b}, deviceTokenBytes))
}

// deviceAccess is the live access token the fixture gives a device.
func deviceAccess(id store.DeviceID) string {
	return seededToken(accessTokenPrefix, deviceByte(id)|1)
}

// deviceRefresh is the live refresh token the fixture gives a device.
func deviceRefresh(id store.DeviceID) string {
	return seededToken(refreshTokenPrefix, deviceByte(id)|2)
}

// deviceSpent is a spent refresh token of the device whose child is unknown: presenting it is a reuse.
func deviceSpent(id store.DeviceID) string {
	return seededToken(refreshTokenPrefix, deviceByte(id)|3)
}

func deviceByte(id store.DeviceID) byte {
	if id == alexDevice {
		return 0xa0
	}
	return 0xb0
}

// seedDevice inserts, on the closed database, what a linked app leaves behind: the device, its tokens
// (deviceAccess, deviceRefresh, deviceSpent, hashed with the service's key), the device's push subscription and the
// device code the user approved for it.
func (e *svcEnv) seedDevice(user store.UserID, id store.DeviceID) {
	e.t.Helper()
	ms := e.clk.now().UnixMilli()
	k := e.svc.keys
	e.rawExec(`INSERT INTO devices (id, user_id, name, client_kind, os, app_version, linked_via, created_at,
		last_seen_at, last_ip) VALUES (?, ?, 'Alex-PC', 'desktop', 'windows', '0.2.0', 'device_flow', ?, ?, '203.0.113.7')`,
		string(id), string(user), ms, ms)
	e.rawExec(`INSERT INTO device_tokens (token_hash, device_id, kind, created_at, expires_at)
		VALUES (?, ?, 'access', ?, ?)`, k.deviceHash(deviceAccess(id)), string(id), ms, ms+900_000)
	e.rawExec(`INSERT INTO device_tokens (token_hash, device_id, kind, created_at, expires_at)
		VALUES (?, ?, 'refresh', ?, ?)`, k.deviceHash(deviceRefresh(id)), string(id), ms, ms+7_776_000_000)
	e.rawExec(`INSERT INTO device_tokens (token_hash, device_id, kind, created_at, expires_at, spent_at, replaced_by)
		VALUES (?, ?, 'refresh', ?, ?, ?, x'0badc0de')`, k.deviceHash(deviceSpent(id)), string(id), ms-3_600_000,
		ms+7_000_000_000, ms-3_000_000)
	e.rawExec(`INSERT INTO push_subscriptions (id, user_id, device_id, endpoint, p256dh, auth_secret, name, created_at)
		VALUES (?, ?, ?, ?, 'p', 'a', 'isshoni for Windows', ?)`,
		"p"+string(id[1:]), string(user), string(id), "https://push.example/device/"+string(id), ms)
	e.rawExec(`INSERT INTO device_codes (device_code_hash, user_code_hash, client_kind, device_name, os, app_version,
		request_ip, created_at, expires_at, status, user_id, decided_at)
		VALUES (?, ?, 'desktop', 'Alex-PC', 'windows', '0.2.0', '203.0.113.7', ?, ?, 'approved', ?, ?)`,
		[]byte("device-code-"+id), []byte("user-code-"+id), ms, ms+600_000, string(user), ms)
}

// revFixture is where every row of the matrix starts: Alex with everything a revocation can take, and Sam, whose
// rows no event of Alex's may touch.
//
//   - Alex has three web sessions, s[0] (the caller's own, and the least recently seen), s[1] and s[2], each with a
//     push subscription; a linked device with a token, a push subscription and the device code Alex approved; and
//     a pending password reset link.
//   - Sam has one session and the same of everything else.
//   - One device code that nobody decided yet belongs to no user.
//
// Every session is in the session cache when the fixture returns.
type revFixture struct {
	*svcEnv
	alex, sam  store.User
	s          [3]LoginResult
	caller     Principal // of s[0]
	samSession LoginResult
	// alexApp is the principal of Alex's device (its access token, deviceAccess(alexDevice)).
	alexApp Principal
	// newToken is the token the event gave the caller's session, for a row that rotates it.
	newToken string
	// admin is the admin who acts in an admin's row: Kim, from address C (withAdmin).
	admin store.Actor
	// resetToken is the token of the reset link that a row issued.
	resetToken string
}

func newRevFixture(t *testing.T, mods ...func(*store.User)) *revFixture {
	t.Helper()
	e := newSvcEnv(t)
	f := &revFixture{svcEnv: e}
	f.alex = e.addUser("Alex", mods...)
	f.sam = e.addUser("Sam")
	e.restart(func() {
		e.seedDevice(f.alex.ID, alexDevice)
		e.seedDevice(f.sam.ID, samDevice)
		ms := e.clk.now().UnixMilli()
		e.rawExec(`INSERT INTO device_codes (device_code_hash, user_code_hash, client_kind, device_name, os, app_version,
			request_ip, created_at, expires_at) VALUES (x'03', x'04', 'mobile', 'Phone', 'ios', '0.2.0', '203.0.113.7', ?, ?)`,
			ms, ms+600_000)
	})
	for i, ip := range []string{ipA, ipB, ipC} {
		f.s[i] = e.login("Alex", ip)
		e.advance(time.Second) // each one seen later than the one before
	}
	f.samSession = e.login("Sam", ipB)
	now := e.clk.now()
	e.write(func(q *store.Q) error {
		for i, sess := range []store.Session{f.s[0].Session, f.s[1].Session, f.s[2].Session, f.samSession.Session} {
			sub := store.PushSubscription{UserID: sess.UserID, SessionID: sess.ID,
				Endpoint: fmt.Sprintf("https://push.example/session/%d", i), P256dh: "p", Auth: "a", Name: sess.Name}
			if _, err := q.UpsertPushSubscription(&sub); err != nil {
				return err
			}
		}
		for i, u := range []store.User{f.alex, f.sam} {
			err := q.ReplacePasswordReset(store.PasswordReset{TokenHash: fmt.Appendf(nil, "reset-hash-%d", i),
				UserID: u.ID, ExpiresAt: now.Add(24 * time.Hour)})
			if err != nil {
				return err
			}
		}
		return nil
	})
	for _, tok := range []string{f.s[0].Token, f.s[1].Token, f.s[2].Token, f.samSession.Token} {
		if _, err := e.svc.Authenticate(e.request(http.MethodGet, tok)); err != nil {
			t.Fatal(err)
		}
	}
	// Both devices' access tokens are in the bearer cache too.
	for _, id := range []store.DeviceID{alexDevice, samDevice} {
		p, err := e.svc.AuthenticateBearerToken(context.Background(), deviceAccess(id))
		if err != nil || p.DeviceID != id {
			t.Fatalf("the seeded device %s: %+v, %v", id, p, err)
		}
		if id == alexDevice {
			f.alexApp = p
		}
	}
	f.caller = principalOf(f.s[0].Session, f.s[0].User)
	e.conns.take()
	e.hashes.Store(0)
	return f
}

// withCookie is the ReqMeta of a request from ip that arrives with the cookie of Alex's session s[i].
func (f *revFixture) withCookie(ip string, i int) ReqMeta {
	m := meta(ip)
	m.SessionToken = f.s[i].Token
	return m
}

// withAdmin is the prepare of an admin's row: Kim, an admin, who acts from address C. (The fixture has no admin of
// its own: the setup row needs a server without one.)
func withAdmin(_ *testing.T, f *revFixture) {
	f.admin = actorFor(f.addAdmin("Kim"), ipC)
}

// revRow is one row of the table of 03 §7.7: the event, and what the table says about it.
type revRow struct {
	// name is the event as the table names it.
	name string
	// prepare, if set, runs first and puts in place what the event needs (an admin who acts, a reset link to
	// complete). Nothing it does counts as the event: the connections it closes are forgotten.
	prepare func(t *testing.T, f *revFixture)
	// run makes the event happen to Alex.
	run func(t *testing.T, f *revFixture)
	// after, if set, checks what the row says beyond its columns.
	after func(t *testing.T, f *revFixture)

	// gone are the indexes of Alex's sessions that the event deletes (the table's "Web sessions" column).
	gone []int
	// rotated: the caller's session stays under a new token; its old one is the previous token of a rotation
	// (03 §7.4), which TestChangePassword follows to the end of its 60 s.
	rotated bool
	// others is how many session rows Alex has afterwards besides the fixture's three.
	others int
	// device: Alex's device stays, with its tokens and the approved device code ("Devices and tokens").
	device bool
	// codesStay: the device goes but the device code Alex approved stays (the M2 rows that take one device; 03 §7.7:
	// another app may be waiting for its approval).
	codesStay bool
	// resetLink: Alex has a pending reset link afterwards ("Pending reset link").
	resetLink bool
	// deleted: the account itself is gone.
	deleted bool
	// calls are the CloseConnections calls, in order ("Live connections closed (reason)").
	calls func(f *revFixture) []closedConn
}

// oneSession is the call that closes the connections of Alex's session s[i].
func oneSession(i int, reason string) func(f *revFixture) []closedConn {
	return func(f *revFixture) []closedConn {
		return []closedConn{{ConnSelector{UserID: f.alex.ID, SessionID: f.s[i].Session.ID}, reason}}
	}
}

// everything is the call that closes every connection of Alex's.
func everything(reason string) func(f *revFixture) []closedConn {
	return func(f *revFixture) []closedConn {
		return []closedConn{{ConnSelector{UserID: f.alex.ID}, reason}}
	}
}

// nothing is the row that closes no connection.
func nothing(*revFixture) []closedConn { return nil }

// alexsDevice is the call that closes the connections of Alex's device (M2 02 §6.5).
func alexsDevice(f *revFixture) []closedConn {
	return []closedConn{{ConnSelector{UserID: f.alex.ID, DeviceID: alexDevice}, ReasonDeviceRevoked}}
}

// everySessionSees is the after of a row that changes Alex's account and keeps the sessions: the session cache
// was invalidated, so the next request of each session gets the account as it is now.
func everySessionSees(what string, ok func(p Principal) bool) func(t *testing.T, f *revFixture) {
	return func(t *testing.T, f *revFixture) {
		for i := range f.s {
			p, err := f.svc.Authenticate(f.request(http.MethodGet, f.s[i].Token))
			if err != nil || !ok(p) {
				t.Errorf("s[%d] right after the change: %+v, %v; want %s (03 §7.4: the cache is invalidated)", i, p, err, what)
			}
		}
	}
}

var all3 = []int{0, 1, 2}

// revMatrix is the table of 03 §7.7, in its order, with the rows M2 02 §6.5 adds after it. Not in it:
//   - "session key rotated": the startup purge of TestKeyFingerprints, which closes nothing (no connection exists
//     yet);
//   - M2's device cap eviction and device expiry: TestDeviceCap and TestBearerForgottenBeforeConnCloser cover the
//     first, the janitor's tests the second (it closes nothing).
//
// "Role change or rename" is two rows here, one per change. Neither takes a session, a device or a connection: the
// hub is told by httpapi (Signal.UserChanged), and auth invalidates the session cache.
var revMatrix = []revRow{
	{
		name: "Logout",
		run: func(t *testing.T, f *revFixture) {
			if err := f.svc.Logout(context.Background(), f.caller, meta(ipA)); err != nil {
				t.Fatal(err)
			}
		},
		gone: []int{0}, device: true, resetLink: true,
		calls: oneSession(0, ReasonLoggedOut),
	},
	{
		name: "Revoke one session",
		run: func(t *testing.T, f *revFixture) {
			if err := f.svc.RevokeSession(context.Background(), f.caller, f.s[1].Session.ID, meta(ipA)); err != nil {
				t.Fatal(err)
			}
		},
		gone: []int{1}, device: true, resetLink: true,
		calls: oneSession(1, ReasonSessionRevoked),
	},
	{
		name: "Login that arrives with an existing session cookie",
		run: func(t *testing.T, f *revFixture) {
			if _, err := f.svc.Login(context.Background(), "Alex", testPassword, f.withCookie(ipC, 1)); err != nil {
				t.Fatal(err)
			}
		},
		gone: []int{1}, others: 1, device: true, resetLink: true,
		calls: oneSession(1, ReasonSessionRevoked),
	},
	{
		name: "Registration that arrives with an existing session cookie",
		run: func(t *testing.T, f *revFixture) {
			_, tok := f.invite(store.CLIActor, InviteInput{})
			res, err := f.svc.Register(context.Background(),
				RegisterInput{InviteToken: tok, Username: "Robin", Password: testPassword}, f.withCookie(ipC, 1))
			if err != nil || res.Login == nil {
				t.Fatalf("Register = %+v, %v", res, err)
			}
		},
		gone: []int{1}, device: true, resetLink: true,
		calls: oneSession(1, ReasonSessionRevoked),
	},
	{
		name: "Setup completion that arrives with an existing session cookie",
		run: func(t *testing.T, f *revFixture) {
			_, err := f.svc.CompleteSetup(context.Background(),
				SetupInput{Token: f.setupToken(), Username: "Robin", Password: testPassword}, f.withCookie(ipC, 1))
			if err != nil {
				t.Fatal(err)
			}
		},
		gone: []int{1}, device: true, resetLink: true,
		calls: oneSession(1, ReasonSessionRevoked),
	},
	{
		name: "Reset completion that arrives with an existing session cookie",
		// The link is for another account, Robin's: a browser that is signed in as Alex opens it.
		prepare: func(_ *testing.T, f *revFixture) {
			f.resetToken = f.resetLink(store.CLIActor, f.addUser("Robin").ID, "")
		},
		run: func(t *testing.T, f *revFixture) {
			res, err := f.svc.CompletePasswordReset(context.Background(), f.resetToken, newPassword, f.withCookie(ipC, 1))
			if err != nil || res.User.Username != "Robin" {
				t.Fatalf("CompletePasswordReset = %+v, %v", res, err)
			}
		},
		gone: []int{1}, device: true, resetLink: true,
		calls: oneSession(1, ReasonSessionRevoked),
	},
	{
		name: "Session cap eviction (the 51st session)",
		run: func(t *testing.T, f *revFixture) {
			// 47 more sessions, all seen later than the fixture's: Alex has 50, and s[0] is the least recently seen.
			now := f.clk.now()
			f.write(func(q *store.Q) error {
				for i := range maxSessionsPerUser - len(f.s) {
					s := store.Session{UserID: f.alex.ID, TokenHash: fmt.Appendf(nil, "hash-%02d", i), Name: "Firefox on Linux",
						CreatedAt: now, LastIP: ipB, IdleExpiresAt: now.Add(sessionIdleTTL), ExpiresAt: now.Add(sessionMaxTTL)}
					if err := q.CreateSession(&s); err != nil {
						return err
					}
				}
				return nil
			})
			f.login("Alex", ipC)
		},
		gone: []int{0}, others: maxSessionsPerUser - len(all3) + 1, device: true, resetLink: true,
		calls: oneSession(0, ReasonSessionRevoked),
	},
	{
		name: "Sign out other browsers",
		run: func(t *testing.T, f *revFixture) {
			n, err := f.svc.RevokeOtherSessions(context.Background(), f.caller, meta(ipA))
			if err != nil || n != 2 {
				t.Fatalf("RevokeOtherSessions = %d, %v; want 2", n, err)
			}
		},
		gone: []int{1, 2}, device: true, resetLink: true,
		// Those sessions, one selector each, in the order of their IDs; the devices' connections stay.
		calls: func(f *revFixture) []closedConn {
			ids := []store.SessionID{f.s[1].Session.ID, f.s[2].Session.ID}
			slices.Sort(ids)
			var out []closedConn
			for _, id := range ids {
				out = append(out, closedConn{ConnSelector{UserID: f.alex.ID, SessionID: id}, ReasonSessionRevoked})
			}
			return out
		},
	},
	{
		name: "Log out everywhere",
		run: func(t *testing.T, f *revFixture) {
			if err := f.svc.LogoutEverywhere(context.Background(), f.caller, meta(ipA)); err != nil {
				t.Fatal(err)
			}
		},
		gone: all3, resetLink: true,
		calls: everything(ReasonLoggedOut),
	},
	{
		name: "Change own password",
		run: func(t *testing.T, f *revFixture) {
			res, err := f.svc.ChangePassword(context.Background(), f.caller, testPassword, "another long passphrase", meta(ipA))
			if err != nil {
				t.Fatal(err)
			}
			f.newToken = res.Token
		},
		gone: []int{1, 2}, rotated: true,
		calls: func(f *revFixture) []closedConn {
			return []closedConn{{ConnSelector{UserID: f.alex.ID, ExceptSessionID: f.s[0].Session.ID}, ReasonPasswordChanged}}
		},
	},
	{
		name:    "Admin: password reset",
		prepare: withAdmin,
		run: func(_ *testing.T, f *revFixture) {
			f.resetToken = f.resetLink(f.admin, f.alex.ID, "")
		},
		gone: all3, resetLink: true,
		calls: everything(ReasonPasswordReset),
		// 03 §7.10: the hash is cleared, and the user's reset row is the new link's, not the one before.
		after: func(t *testing.T, f *revFixture) {
			hash, _, _, ok := f.resetRow(f.alex.ID)
			if !ok || !bytes.Equal(hash, f.svc.keys.inviteTokenHash(f.resetToken)) {
				t.Errorf("Alex's reset link is not the one that was issued (found %v)", ok)
			}
			if u := f.userRow(f.alex.ID); u == nil || u.PasswordHash != "" {
				t.Errorf("Alex's password after the reset was issued: %+v, want none", u)
			}
		},
	},
	{
		name:    "Admin: sign out everywhere",
		prepare: withAdmin,
		run: func(t *testing.T, f *revFixture) {
			sessions, devices, err := f.svc.SignOutUser(context.Background(), f.admin, f.alex.ID)
			if err != nil || sessions != len(f.s) || devices != 1 {
				t.Fatalf("SignOutUser = %d sessions, %d devices, %v; want %d and 1", sessions, devices, err, len(f.s))
			}
		},
		gone: all3, resetLink: true,
		calls: everything(ReasonSessionRevoked),
	},
	{
		name:    "Admin: disable user",
		prepare: withAdmin,
		run: func(t *testing.T, f *revFixture) {
			u, err := f.svc.UpdateUser(context.Background(), f.admin, f.alex.ID, UserChange{Status: ptr(store.StatusDisabled)})
			if err != nil || u.Status != store.StatusDisabled {
				t.Fatalf("UpdateUser = %+v, %v", u, err)
			}
		},
		gone:  all3,
		calls: everything(ReasonAccountDisabled),
	},
	{
		name: "Delete user (self)",
		run: func(t *testing.T, f *revFixture) {
			if err := f.svc.DeleteSelf(context.Background(), f.caller, testPassword, meta(ipA)); err != nil {
				t.Fatal(err)
			}
		},
		gone: all3, deleted: true,
		calls: everything(ReasonAccountDeleted),
	},
	{
		name:    "Delete user (admin)",
		prepare: withAdmin,
		run: func(t *testing.T, f *revFixture) {
			if err := f.svc.DeleteUser(context.Background(), f.admin, f.alex.ID); err != nil {
				t.Fatal(err)
			}
		},
		gone: all3, deleted: true,
		calls: everything(ReasonAccountDeleted),
	},
	{
		name:    "Role change",
		prepare: withAdmin,
		run: func(t *testing.T, f *revFixture) {
			u, err := f.svc.UpdateUser(context.Background(), f.admin, f.alex.ID,
				UserChange{Role: ptr(store.RoleAdmin), ActorPassword: testPassword})
			if err != nil || u.Role != store.RoleAdmin {
				t.Fatalf("UpdateUser = %+v, %v", u, err)
			}
		},
		device: true, resetLink: true,
		calls: nothing,
		after: everySessionSees("an admin", Principal.IsAdmin),
	},
	{
		name:    "Rename",
		prepare: withAdmin,
		run: func(t *testing.T, f *revFixture) {
			u, err := f.svc.UpdateUser(context.Background(), f.admin, f.alex.ID, UserChange{Username: ptr("Alexandra")})
			if err != nil || u.Username != "Alexandra" {
				t.Fatalf("UpdateUser = %+v, %v", u, err)
			}
		},
		device: true, resetLink: true,
		calls: nothing,
		after: everySessionSees("the new name", func(p Principal) bool { return p.Username == "Alexandra" }),
	},
	{
		name: "Revoke device (M2)",
		run: func(t *testing.T, f *revFixture) {
			if err := f.svc.RevokeDevice(context.Background(), f.caller, alexDevice, meta(ipA)); err != nil {
				t.Fatal(err)
			}
		},
		codesStay: true, resetLink: true,
		calls: alexsDevice,
	},
	{
		name: "The app's own sign-out (M2)",
		run: func(t *testing.T, f *revFixture) {
			if err := f.svc.RevokeOwnDevice(context.Background(), f.alexApp, meta(ipA)); err != nil {
				t.Fatal(err)
			}
		},
		codesStay: true, resetLink: true,
		calls: alexsDevice,
	},
	{
		name: "Refresh-token reuse (M2)",
		run: func(t *testing.T, f *revFixture) {
			_, err := f.refreshFrom(deviceSpent(alexDevice), "", ipC)
			var re *RefreshReusedError
			if !errors.As(err, &re) || re.DeviceID != alexDevice {
				t.Fatalf("replaying a spent refresh token: %v, want a reuse", err)
			}
		},
		codesStay: true, resetLink: true,
		calls: alexsDevice,
	},
}

func TestRevocationMatrix(t *testing.T) {
	for _, row := range revMatrix {
		t.Run(row.name, func(t *testing.T) {
			f := newRevFixture(t)
			ctx := context.Background()
			if row.prepare != nil {
				row.prepare(t, f)
				f.conns.take()
			}

			// What the service had done by the time it asked for the connections to be closed: the rows must be
			// gone from the database (the Write committed) and from the session cache.
			var early []string
			f.conns.during = func(ConnSelector, string) {
				for _, i := range row.gone {
					id := f.s[i].Session.ID
					if f.session(f.alex.ID, id) != nil {
						early = append(early, fmt.Sprintf("s[%d] was still in the database", i))
					}
					if _, _, ok := f.svc.sessions.byID(id, f.clk.now()); ok {
						early = append(early, fmt.Sprintf("s[%d] was still in the session cache", i))
					}
				}
				if !row.device {
					if f.rawInt("SELECT count(*) FROM devices WHERE id = ?", string(alexDevice)) != 0 {
						early = append(early, "Alex's device was still in the database")
					}
					hash := f.svc.keys.deviceHash(deviceAccess(alexDevice))
					if _, _, hit := f.svc.bearers.byToken(hash, f.clk.now()); hit != cacheMiss {
						early = append(early, "Alex's access token was still in the bearer cache")
					}
					if _, _, ok := f.svc.bearers.byDevice(alexDevice, f.clk.now()); ok {
						early = append(early, "Alex's device was still in the bearer cache")
					}
				}
			}
			row.run(t, f)
			f.conns.during = nil

			// Live connections closed (reason): the exact selectors, in order.
			want := row.calls(f)
			if calls := f.conns.take(); fmt.Sprint(calls) != fmt.Sprint(want) {
				t.Errorf("CloseConnections calls = %+v, want %+v", calls, want)
			}
			if len(early) != 0 {
				t.Errorf("when CloseConnections was called: %v (03 §7.7: commit, then the cache, then the connections)", early)
			}

			// Web sessions, and what the session cache answers right away.
			for i := range f.s {
				id, tok := f.s[i].Session.ID, f.s[i].Token
				isGone := slices.Contains(row.gone, i)
				if got := f.session(f.alex.ID, id) == nil; got != isGone {
					t.Errorf("s[%d]: row gone = %v, want %v", i, got, isGone)
				}
				old, err := f.svc.Authenticate(f.request(http.MethodGet, tok))
				touchErr := f.svc.Touch(ctx, principalOf(f.s[i].Session, f.s[i].User), netip.MustParseAddr(ipA))
				switch {
				case isGone:
					wantCode(t, err, api.CodeUnauthenticated)
					wantCode(t, touchErr, api.CodeUnauthenticated)
				case i == 0 && row.rotated:
					// The session is the same one under a new token, and the old token is still let in as that session:
					// the browser's other tabs hold it until the new cookie reaches them.
					if err != nil || old.SessionID != id {
						t.Errorf("the caller's old token right after the rotation: %+v, %v; want the same session", old, err)
					}
					p, err := f.svc.Authenticate(f.request(http.MethodGet, f.newToken))
					if err != nil || p.SessionID != id || touchErr != nil {
						t.Errorf("the caller's session under its new token: %+v, %v; Touch: %v", p, err, touchErr)
					}
				default:
					if err != nil || touchErr != nil {
						t.Errorf("s[%d] stays, but Authenticate says %v and Touch %v", i, err, touchErr)
					}
				}
			}
			if got, want := len(f.sessions(f.alex.ID)), len(f.s)-len(row.gone)+row.others; got != want {
				t.Errorf("Alex has %d session rows, want %d", got, want)
			}

			// Devices and tokens, push subscriptions, the pending reset link.
			count := func(query string, args ...any) int { return f.rawInt(query, args...) }
			b2i := map[bool]int{true: 1}
			alex := string(f.alex.ID)
			for what, got := range map[string][2]int{
				"devices": {count("SELECT count(*) FROM devices WHERE user_id = ?", alex), b2i[row.device]},
				"device tokens": {count("SELECT count(*) FROM device_tokens WHERE device_id = ?", string(alexDevice)),
					seededTokens * b2i[row.device]},
				"approved device codes": {count("SELECT count(*) FROM device_codes WHERE user_id = ?", alex),
					b2i[row.device || row.codesStay]},
				// Each one goes with its session or its device, and only with it.
				"push subscriptions": {count("SELECT count(*) FROM push_subscriptions WHERE user_id = ?", alex),
					len(f.s) - len(row.gone) + b2i[row.device]},
				"reset links": {count("SELECT count(*) FROM password_resets WHERE user_id = ?", alex), b2i[row.resetLink]},
				"users rows":  {count("SELECT count(*) FROM users WHERE id = ?", alex), b2i[!row.deleted]},
			} {
				if got[0] != got[1] {
					t.Errorf("Alex has %d %s afterwards, want %d", got[0], what, got[1])
				}
			}

			// Nothing of anybody else's went: Sam's rows, and the device code nobody decided.
			sam := string(f.sam.ID)
			for what, got := range map[string][2]int{
				"sessions": {count("SELECT count(*) FROM sessions WHERE user_id = ?", sam), 1},
				"devices":  {count("SELECT count(*) FROM devices WHERE user_id = ?", sam), 1},
				"device tokens": {count("SELECT count(*) FROM device_tokens WHERE device_id = ?", string(samDevice)),
					seededTokens},
				"device codes":       {count("SELECT count(*) FROM device_codes WHERE user_id = ?", sam), 1},
				"push subscriptions": {count("SELECT count(*) FROM push_subscriptions WHERE user_id = ?", sam), 2},
				"reset links":        {count("SELECT count(*) FROM password_resets WHERE user_id = ?", sam), 1},
				"undecided codes":    {count("SELECT count(*) FROM device_codes WHERE user_id IS NULL"), 1},
			} {
				if got[0] != got[1] {
					t.Errorf("Sam has %d %s afterwards, want %d", got[0], what, got[1])
				}
			}
			if p, err := f.svc.Authenticate(f.request(http.MethodGet, f.samSession.Token)); err != nil || p.UserID != f.sam.ID {
				t.Errorf("Sam's session afterwards: %+v, %v", p, err)
			}

			// The devices' bearers, right away: Alex's is refused exactly when the row took the device.
			p, err := f.svc.AuthenticateBearerToken(ctx, deviceAccess(alexDevice))
			switch {
			case row.device && (err != nil || p.DeviceID != alexDevice):
				t.Errorf("Alex's device stays, but its bearer gets %+v, %v", p, err)
			case !row.device && !api.IsCode(err, api.CodeInvalidToken):
				t.Errorf("Alex's device is gone, but its bearer gets %+v, %v; want invalid_token", p, err)
			}
			_, touchErr := f.svc.TouchPrincipal(ctx, Principal{UserID: f.alex.ID, Method: MethodBearer, DeviceID: alexDevice},
				netip.MustParseAddr(ipA))
			if row.device != (touchErr == nil) {
				t.Errorf("Touch for Alex's device afterwards: %v (device stays: %v)", touchErr, row.device)
			}
			if p, err := f.svc.AuthenticateBearerToken(ctx, deviceAccess(samDevice)); err != nil || p.UserID != f.sam.ID {
				t.Errorf("Sam's device afterwards: %+v, %v", p, err)
			}
			if row.after != nil {
				row.after(t, f)
			}
		})
	}
}

// TestRevocationRules pins the table of 03 §7.7 as revoke.go has it: one rule per row that revokes a user's
// credentials as a whole, with the row's reason and columns.
func TestRevocationRules(t *testing.T) {
	for name, tc := range map[string]struct{ got, want revocation }{
		"Sign out other browsers":    {revokeOtherBrowsers, revocation{reason: "session_revoked", keepCurrent: true}},
		"Log out everywhere":         {revokeEverywhere, revocation{reason: "logged_out", devices: true}},
		"Change own password":        {revokePasswordChanged, revocation{reason: "password_changed", keepCurrent: true, devices: true, resetLink: true}},
		"Admin: password reset":      {revokePasswordReset, revocation{reason: "password_reset", devices: true}},
		"Admin: sign out everywhere": {revokeSignedOut, revocation{reason: "session_revoked", devices: true}},
		"Admin: disable user":        {revokeAccountDisabled, revocation{reason: "account_disabled", devices: true, resetLink: true}},
		"Delete user":                {revokeAccountDeleted, revocation{reason: "account_deleted", devices: true, resetLink: true}},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: rule %+v, want %+v", name, tc.got, tc.want)
		}
	}
}

// TestRevocationKeepsOnlyANamedSession: a rule that keeps the caller's session refuses to run without that
// session's ID, instead of deleting every session of the user.
func TestRevocationKeepsOnlyANamedSession(t *testing.T) {
	f := newRevFixture(t)
	for _, rule := range []revocation{revokeOtherBrowsers, revokePasswordChanged} {
		err := f.db.Write(context.Background(), func(q *store.Q) error {
			_, err := rule.apply(q, f.alex.ID, "")
			return err
		})
		if !errors.Is(err, errNoCurrentSession) {
			t.Errorf("apply(%+v) without the current session: %v, want errNoCurrentSession", rule, err)
		}
	}
	if n := len(f.sessions(f.alex.ID)); n != len(f.s) {
		t.Errorf("Alex has %d sessions after the refused rules, want %d", n, len(f.s))
	}
	// A rule that keeps nothing ignores the ID it is given.
	var rv revoked
	f.write(func(q *store.Q) error {
		var err error
		rv, err = revokeSignedOut.apply(q, f.alex.ID, f.s[0].Session.ID)
		return err
	})
	if len(rv.sessions) != len(f.s) || rv.kept != "" || len(rv.devices) != 1 || rv.devices[0] != alexDevice {
		t.Errorf("revokeSignedOut took %+v, want every session and the device", rv)
	}
}

// TestRevocationWithoutConnCloser: with no ConnCloser (unit tests and offline CLI commands, 03 §7.13) a revocation
// still deletes its rows and invalidates the cache.
func TestRevocationWithoutConnCloser(t *testing.T) {
	e := newSvcEnv(t, func(o *Options) { o.Conns = nil })
	alex := e.addUser("Alex")
	a, b := e.login("Alex", ipA), e.login("Alex", ipB)
	for _, tok := range []string{a.Token, b.Token} {
		if _, err := e.svc.Authenticate(e.request(http.MethodGet, tok)); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	p := principalOf(a.Session, a.User)
	if n, err := e.svc.RevokeOtherSessions(ctx, p, meta(ipA)); err != nil || n != 1 {
		t.Fatalf("RevokeOtherSessions = %d, %v", n, err)
	}
	_, err := e.svc.Authenticate(e.request(http.MethodGet, b.Token))
	wantCode(t, err, api.CodeUnauthenticated)
	if err := e.svc.LogoutEverywhere(ctx, p, meta(ipA)); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Authenticate(e.request(http.MethodGet, a.Token))
	wantCode(t, err, api.CodeUnauthenticated)
	if n := len(e.sessions(alex.ID)); n != 0 {
		t.Errorf("%d sessions left", n)
	}
}
