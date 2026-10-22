package auth

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// The device calls of the Devices page and the app (M2 02 §6.4, §6.5, §7.2; README WA39): RenameDevice,
// RevokeOwnDevice, and the bearer cases of RevokeDevice, RevokeSession and LogoutEverywhere, each with its audit row,
// its caller check inside the Write and, for a revocation, the bearer cache forgotten before ConnCloser.

// TestRenameDevice: the owner renames a device with a cookie or from the app; the name is normalized like an app's,
// device.renamed {from, to} is written, and the same name again writes nothing. A device that is not the caller's
// is not_found, and a bad name is 422 {name}.
func TestRenameDevice(t *testing.T) {
	e, alex, app := newLinkEnv(t)
	ctx := context.Background()
	sam := e.addUser("Sam")
	samApp := e.linkApp(e.login("Sam", ipC), ipC)
	e.advance(time.Minute)
	now := e.clk.now()

	d, err := e.svc.RenameDevice(ctx, principal(alex), app.device, "  Gaming  PC ", meta(ipB))
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != app.device || d.Name != "Gaming PC" || d.UserID != app.user.ID {
		t.Fatalf("RenameDevice = %+v", d)
	}
	if row := e.deviceRow(app.device); row.Name != "Gaming PC" {
		t.Fatalf("the row after the rename: %+v", row)
	}
	rows := e.audit("device.renamed")
	if len(rows) != 1 {
		t.Fatalf("%d device.renamed rows, want 1", len(rows))
	}
	r := rows[0]
	if r.Actor != (store.Actor{Kind: store.ActorUser, UserID: app.user.ID, Name: "Alex", IP: ipB}) ||
		r.TargetKind != "device" || r.TargetID != string(app.device) || r.TargetName != "Gaming PC" || !r.At.Equal(now) {
		t.Errorf("device.renamed row = %+v", r)
	}
	wantDetail(t, "device.renamed", r.Detail, map[string]any{"from": app.name, "to": "Gaming PC"})

	// The same name again (after normalization): nothing is written.
	if d, err := e.svc.RenameDevice(ctx, principal(alex), app.device, "Gaming PC ", meta(ipB)); err != nil || d.Name != "Gaming PC" {
		t.Fatalf("the same name again: %+v, %v", d, err)
	}
	if n := e.auditCount("device.renamed"); n != 1 {
		t.Errorf("%d device.renamed rows after a rename to the same name, want 1", n)
	}

	// From the app, with its bearer; its own principal shows the new name at once.
	if _, err := e.svc.RenameDevice(ctx, app.principal(), app.device, "Living room", meta(ipA)); err != nil {
		t.Fatal(err)
	}
	if p, err := e.svc.AuthenticateBearerToken(ctx, app.access); err != nil || p.DeviceName != "Living room" {
		t.Errorf("the app's principal after its rename: %+v, %v", p, err)
	}

	for name, tc := range map[string]struct {
		p    Principal
		id   store.DeviceID
		name string
		code string
		want map[string]string
	}{
		"another user's device":     {principal(alex), samApp.device, "Mine now", api.CodeNotFound, nil},
		"another user's, from app":  {app.principal(), samApp.device, "Mine now", api.CodeNotFound, nil},
		"an unknown device":         {principal(alex), "nosuchdevice", "x", api.CodeNotFound, nil},
		"no device ID":              {principal(alex), "", "x", api.CodeNotFound, nil},
		"an empty name":             {principal(alex), app.device, "   ", api.CodeValidationFailed, map[string]string{"name": "required"}},
		"a name of 65":              {principal(alex), app.device, strings.Repeat("x", 65), api.CodeValidationFailed, map[string]string{"name": "too_long"}},
		"a control character":       {principal(alex), app.device, "PC\n2", api.CodeValidationFailed, map[string]string{"name": "invalid"}},
		"no principal":              {Principal{}, app.device, "x", api.CodeUnauthenticated, nil},
		"a bearer without a device": {Principal{Method: MethodBearer, UserID: app.user.ID}, app.device, "x", api.CodeUnauthenticated, nil},
	} {
		_, err := e.svc.RenameDevice(ctx, tc.p, tc.id, tc.name, meta(ipB))
		if ae := wantCode(t, err, tc.code); !reflect.DeepEqual(ae.Fields, tc.want) {
			t.Errorf("%s: fields %v, want %v", name, ae.Fields, tc.want)
		}
	}
	if row := e.deviceRow(samApp.device); row.Name != samApp.name || row.UserID != sam.ID {
		t.Errorf("Sam's device after refused renames: %+v", row)
	}
	if n := e.auditCount("device.renamed"); n != 2 {
		t.Errorf("%d device.renamed rows, want 2", n)
	}
	if calls := e.conns.take(); len(calls) != 0 {
		t.Errorf("a rename closed connections: %+v", calls)
	}
	if logs := e.logs.String(); strings.Contains(logs, "Gaming PC") || strings.Contains(logs, "Living room") {
		t.Errorf("the log names a device:\n%s", logs)
	}
}

// TestRevokeOwnDevice: the app's sign-out (POST /device/revoke) deletes its device with its tokens and push
// subscription, writes device.revoked {via: "app"}, forgets the cache and then closes the device's connections. The
// user's sessions and other devices stay; a cookie is 403 {credential: device}; the device gone, the call is
// unauthenticated.
func TestRevokeOwnDevice(t *testing.T) {
	e, alex, app := newLinkEnv(t)
	ctx := context.Background()
	other := e.linkApp(alex, ipC)
	e.write(func(q *store.Q) error {
		sub := store.PushSubscription{UserID: app.user.ID, DeviceID: app.device, Endpoint: "https://push.example/app",
			P256dh: "p", Auth: "a", Name: "isshoni for Windows"}
		_, err := q.UpsertPushSubscription(&sub)
		return err
	})
	if _, err := e.svc.AuthenticateBearerToken(ctx, app.access); err != nil {
		t.Fatal(err)
	}

	err := e.svc.RevokeOwnDevice(ctx, principal(alex), meta(ipB))
	if ae := wantCode(t, err, api.CodeForbidden); ae.Params[api.ParamCredential] != "device" {
		t.Errorf("RevokeOwnDevice with a cookie: %+v, want forbidden {credential: device}", ae)
	}
	wantCode(t, e.svc.RevokeOwnDevice(ctx, Principal{}, meta(ipB)), api.CodeUnauthenticated)
	if len(e.devices(app.user.ID)) != 2 || e.auditCount("device.revoked") != 0 {
		t.Fatal("a refused sign-out changed something")
	}

	e.advance(time.Minute)
	now := e.clk.now()
	var early []string
	e.conns.during = func(ConnSelector, string) {
		if e.deviceRow(app.device) != nil {
			early = append(early, "the device row was there")
		}
		if _, _, hit := e.svc.bearers.byToken(e.svc.keys.deviceHash(app.access), e.clk.now()); hit != cacheMiss {
			early = append(early, "the access token was cached")
		}
	}
	if err := e.svc.RevokeOwnDevice(ctx, app.principal(), meta(ipA)); err != nil {
		t.Fatal(err)
	}
	e.conns.during = nil
	if len(early) != 0 {
		t.Errorf("when ConnCloser ran: %v", early)
	}
	want := []closedConn{{ConnSelector{UserID: app.user.ID, DeviceID: app.device}, ReasonDeviceRevoked}}
	if calls := e.conns.take(); !reflect.DeepEqual(calls, want) {
		t.Errorf("ConnCloser calls = %+v, want %+v", calls, want)
	}
	for what, got := range map[string][2]int{
		"device tokens":      {e.rawInt("SELECT count(*) FROM device_tokens WHERE device_id = ?", string(app.device)), 0},
		"push subscriptions": {e.rawInt("SELECT count(*) FROM push_subscriptions WHERE device_id = ?", string(app.device)), 0},
		"other devices":      {len(e.devices(app.user.ID)), 1},
		"sessions":           {len(e.sessions(app.user.ID)), 1},
	} {
		if got[0] != got[1] {
			t.Errorf("%d %s afterwards, want %d", got[0], what, got[1])
		}
	}
	rows := e.audit("device.revoked")
	if len(rows) != 1 {
		t.Fatalf("%d device.revoked rows, want 1", len(rows))
	}
	if r := rows[0]; r.Actor != (store.Actor{Kind: store.ActorUser, UserID: app.user.ID, Name: "Alex", IP: ipA}) ||
		r.TargetKind != "device" || r.TargetID != string(app.device) || r.TargetName != app.name || !r.At.Equal(now) {
		t.Errorf("device.revoked row = %+v", r)
	}
	wantDetail(t, "device.revoked", rows[0].Detail, map[string]any{"via": "app"})
	_, err = e.svc.AuthenticateBearerToken(ctx, app.access)
	wantCode(t, err, api.CodeInvalidToken)
	_, err = e.refreshFrom(app.refresh, "", ipA)
	wantCode(t, err, api.CodeInvalidGrant)
	wantCode(t, e.svc.RevokeOwnDevice(ctx, app.principal(), meta(ipA)), api.CodeUnauthenticated)
	if _, err := e.svc.AuthenticateBearerToken(ctx, other.access); err != nil {
		t.Errorf("the other app after the sign-out: %v", err)
	}
	if _, err := e.svc.Authenticate(e.request(http.MethodGet, alex.Token)); err != nil {
		t.Errorf("the web session after the sign-out: %v", err)
	}
}

// TestRevokeDeviceByBearer: DELETE /me/devices/{id} with a bearer (M2 02 §6.4 #18): its own ID is the app's sign-out
// (via app), another device of its user's is removed as on the Devices page (via devices_page), another user's is
// not_found.
func TestRevokeDeviceByBearer(t *testing.T) {
	e, alex, app := newLinkEnv(t)
	ctx := context.Background()
	other := e.linkApp(alex, ipC)
	samApp := e.linkApp(e.addLogin("Sam", ipC), ipC)

	wantCode(t, e.svc.RevokeDevice(ctx, app.principal(), samApp.device, meta(ipA)), api.CodeNotFound)
	if err := e.svc.RevokeDevice(ctx, app.principal(), other.device, meta(ipA)); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RevokeDevice(ctx, app.principal(), app.device, meta(ipA)); err != nil {
		t.Fatal(err)
	}
	rows := e.audit("device.revoked")
	if len(rows) != 2 || rows[0].TargetID != string(other.device) || rows[1].TargetID != string(app.device) {
		t.Fatalf("device.revoked rows = %+v", rows)
	}
	wantDetail(t, "the other device's row", rows[0].Detail, map[string]any{"via": "devices_page"})
	wantDetail(t, "the own device's row", rows[1].Detail, map[string]any{"via": "app"})
	want := []closedConn{
		{ConnSelector{UserID: app.user.ID, DeviceID: other.device}, ReasonDeviceRevoked},
		{ConnSelector{UserID: app.user.ID, DeviceID: app.device}, ReasonDeviceRevoked},
	}
	if calls := e.conns.take(); !reflect.DeepEqual(calls, want) {
		t.Errorf("ConnCloser calls = %+v, want %+v", calls, want)
	}
	if e.deviceRow(samApp.device) == nil {
		t.Error("Sam's device went")
	}
	// The device is gone: its principal can't call again.
	wantCode(t, e.svc.RevokeDevice(ctx, app.principal(), app.device, meta(ipA)), api.CodeUnauthenticated)
}

// TestLogoutEverywhereByBearer: from the app, "log out everywhere" signs out every web session and every device of
// the user, the app's own included; both caches forget the user before ConnCloser closes all of the user's
// connections.
func TestLogoutEverywhereByBearer(t *testing.T) {
	e, alex, app := newLinkEnv(t)
	ctx := context.Background()
	other := e.linkApp(alex, ipC)
	second := e.login("Alex", ipC)
	for _, tok := range []string{app.access, other.access} {
		if _, err := e.svc.AuthenticateBearerToken(ctx, tok); err != nil {
			t.Fatal(err)
		}
	}
	var early []string
	e.conns.during = func(ConnSelector, string) {
		for _, tok := range []string{alex.Token, second.Token} {
			if _, _, ok := e.svc.sessions.byHash(e.svc.keys.sessionTokenHash(tok), e.clk.now()); ok {
				early = append(early, "a session was cached")
			}
		}
		if tokens, devices := e.svc.bearers.size(); tokens+devices != 0 {
			early = append(early, fmt.Sprintf("the bearer cache held %d tokens and %d devices", tokens, devices))
		}
	}
	if err := e.svc.LogoutEverywhere(ctx, app.principal(), meta(ipA)); err != nil {
		t.Fatal(err)
	}
	e.conns.during = nil
	if len(early) != 0 {
		t.Errorf("when ConnCloser ran: %v", early)
	}
	want := []closedConn{{ConnSelector{UserID: app.user.ID}, ReasonLoggedOut}}
	if calls := e.conns.take(); !reflect.DeepEqual(calls, want) {
		t.Errorf("ConnCloser calls = %+v, want %+v", calls, want)
	}
	if n, m := len(e.sessions(app.user.ID)), len(e.devices(app.user.ID)); n != 0 || m != 0 {
		t.Errorf("%d sessions and %d devices left, want none", n, m)
	}
	rows := e.audit("auth.logout_everywhere")
	if len(rows) != 1 || rows[0].Actor.IP != ipA || rows[0].TargetID != string(app.user.ID) {
		t.Fatalf("auth.logout_everywhere rows = %+v", rows)
	}
	if got := fmt.Sprint(rows[0].Detail); got != "map[devices:2 sessions:2]" {
		t.Errorf("auth.logout_everywhere detail = %s, want {sessions: 2, devices: 2}", got)
	}
	for _, tok := range []string{app.access, other.access} {
		_, err := e.svc.AuthenticateBearerToken(ctx, tok)
		wantCode(t, err, api.CodeInvalidToken)
	}
	_, err := e.svc.Authenticate(e.request(http.MethodGet, alex.Token))
	wantCode(t, err, api.CodeUnauthenticated)
}

// TestRevokeSessionByBearer: DELETE /me/sessions/{id} is a User route, so the app may revoke one of its user's web
// sessions; another user's session is not_found.
func TestRevokeSessionByBearer(t *testing.T) {
	e, alex, app := newLinkEnv(t)
	ctx := context.Background()
	sam := e.addLogin("Sam", ipC)
	wantCode(t, e.svc.RevokeSession(ctx, app.principal(), sam.Session.ID, meta(ipA)), api.CodeNotFound)
	if err := e.svc.RevokeSession(ctx, app.principal(), alex.Session.ID, meta(ipA)); err != nil {
		t.Fatal(err)
	}
	if n := len(e.sessions(app.user.ID)); n != 0 {
		t.Errorf("%d sessions left, want 0", n)
	}
	want := []closedConn{{ConnSelector{UserID: app.user.ID, SessionID: alex.Session.ID}, ReasonSessionRevoked}}
	if calls := e.conns.take(); !reflect.DeepEqual(calls, want) {
		t.Errorf("ConnCloser calls = %+v, want %+v", calls, want)
	}
	if rows := e.audit("session.revoked"); len(rows) != 1 || rows[0].Actor.IP != ipA {
		t.Errorf("session.revoked rows = %+v", rows)
	}
	if _, err := e.svc.AuthenticateBearerToken(ctx, app.access); err != nil {
		t.Errorf("the app after it revoked a session: %v", err)
	}
}

// TestDeviceCallerRechecksInWrite: a device's call checks its caller again inside its Write, whatever the chain let
// in a moment ago: with the device gone or the account no longer active, every call a device may make is
// unauthenticated and changes nothing.
func TestDeviceCallerRechecksInWrite(t *testing.T) {
	for name, spoil := range map[string]func(e *svcEnv, app linked){
		"the device is gone": func(e *svcEnv, app linked) {
			e.write(func(q *store.Q) error {
				_, err := q.DeleteDevice(app.user.ID, app.device)
				return err
			})
		},
		"the account is disabled": func(e *svcEnv, app linked) {
			e.write(func(q *store.Q) error { return q.SetStatus(app.user.ID, store.StatusDisabled, e.clk.now()) })
		},
	} {
		t.Run(name, func(t *testing.T) {
			e, alex, app := newLinkEnv(t)
			other := e.linkApp(alex, ipC)
			ctx := context.Background()
			spoil(e, app)
			before := e.rowCounts()
			p := app.principal()
			for call, run := range map[string]func() error{
				"RevokeSession":    func() error { return e.svc.RevokeSession(ctx, p, alex.Session.ID, meta(ipA)) },
				"LogoutEverywhere": func() error { return e.svc.LogoutEverywhere(ctx, p, meta(ipA)) },
				"RevokeDevice":     func() error { return e.svc.RevokeDevice(ctx, p, other.device, meta(ipA)) },
				"RevokeOwnDevice":  func() error { return e.svc.RevokeOwnDevice(ctx, p, meta(ipA)) },
				"RenameDevice": func() error {
					_, err := e.svc.RenameDevice(ctx, p, other.device, "x", meta(ipA))
					return err
				},
			} {
				if err := run(); !api.IsCode(err, api.CodeUnauthenticated) {
					t.Errorf("%s: %v, want unauthenticated", call, err)
				}
			}
			if after := e.rowCounts(); fmt.Sprint(after) != fmt.Sprint(before) || len(e.conns.take()) != 0 {
				t.Errorf("refused calls changed something: %v → %v", before, after)
			}
		})
	}
}

// addLogin adds an active user and logs it in from ip.
func (e *svcEnv) addLogin(name, ip string) LoginResult {
	e.t.Helper()
	e.addUser(name)
	return e.login(name, ip)
}
