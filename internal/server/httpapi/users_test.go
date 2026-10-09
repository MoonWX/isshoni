package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/auth"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// Admin users over REST (README S64; 03 §15 "Roles"), on the fixture of 03 §15: the router and the /api/v1 chain
// over a real store and a real auth.Service, with fake Signal, ConnCloser and AdminAlerter.

// users is GET /api/v1/admin/users as the admin behind c sees it; query is "" or "?status=…".
func (f *authFixture) users(c *http.Cookie, query string) []api.AdminUser {
	f.t.Helper()
	rec := f.call(http.MethodGet, "/api/v1/admin/users"+query, "", with(c))
	return decodeBody[api.AdminUsersResponse](f.t, rec, http.StatusOK).Users
}

// userRow is one account of the admin's user list, by its ID.
func (f *authFixture) userRow(c *http.Cookie, id string) api.AdminUser {
	f.t.Helper()
	for _, u := range f.users(c, "") {
		if u.ID == id {
			return u
		}
	}
	f.t.Fatalf("the user list has no account %s", id)
	return api.AdminUser{}
}

// patchUser sends PATCH /api/v1/admin/users/{id} with a raw JSON body.
func (f *authFixture) patchUser(c *http.Cookie, id, body string, opts ...reqOpt) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.call(http.MethodPatch, "/api/v1/admin/users/"+id, body, append([]reqOpt{with(c)}, opts...)...)
}

// patched is patchUser for a change that goes through: the account of the 200 answer.
func (f *authFixture) patched(c *http.Cookie, id, body string) api.AdminUser {
	f.t.Helper()
	return decodeBody[api.AdminUserResponse](f.t, f.patchUser(c, id, body), http.StatusOK).User
}

// names are the usernames of a user list, in its order.
func names(users []api.AdminUser) string {
	out := make([]string, 0, len(users))
	for _, u := range users {
		out = append(out, u.Username)
	}
	return strings.Join(out, " ")
}

// wantBody checks an error answer's exact body: its code alone, or with fields.
func wantBody(t *testing.T, rec *httptest.ResponseRecorder, status int, body string) {
	t.Helper()
	if got := rec.Body.String(); rec.Code != status || got != body+"\n" {
		t.Errorf("answer = %d %s; want %d %s", rec.Code, got, status, body)
	}
}

// TestAdminUsersListOverREST: GET /api/v1/admin/users (03 §12.4.8): every account by name with its role, status,
// origin, times, inviter, sessions, devices and whether it is online; ?status= narrows it; no IPs.
func TestAdminUsersListOverREST(t *testing.T) {
	f := newAuthFixture(t)
	start := f.clk.now()
	admin, cookie := f.setupAdmin("Alex")
	_, token := f.createInvite(cookie, `{}`)
	f.clk.advance(time.Minute)
	sam, samCookie := f.join(token, "Sam", clientIP(1))
	robin, _ := f.join(token, "robin", clientIP(2))
	f.linkDevice(sam.ID, "d1d1d1d1d1d1")
	f.setMode(cookie, "approval")
	f.clk.advance(time.Minute)
	f.signup("Kim", clientIP(3))
	f.patched(cookie, robin.ID, `{"status":"disabled"}`)
	f.signal.setOnline(sam.ID, "nobody000000")

	rec := f.call(http.MethodGet, "/api/v1/admin/users", "", with(cookie))
	users := decodeBody[api.AdminUsersResponse](t, rec, http.StatusOK).Users
	if got := names(users); got != "Alex Kim robin Sam" {
		t.Fatalf("users = %s; want every account, by name", got)
	}
	alex, kim, rob, s := users[0], users[1], users[2], users[3]
	if alex.ID != admin.ID || alex.Role != api.RoleAdmin || alex.Status != api.UserStatusActive || alex.CreatedVia != api.CreatedViaSetup ||
		!alex.CreatedAt.Equal(start) || !alex.LastLoginAt.Equal(start) || !alex.LastSeenAt.Equal(start) || alex.InvitedBy != nil ||
		alex.Sessions != 1 || alex.Devices != 0 || alex.Online || alex.ResetPending {
		t.Errorf("the admin's row = %+v", alex)
	}
	joined := start.Add(time.Minute)
	if s.ID != sam.ID || s.Role != api.RoleUser || s.Status != api.UserStatusActive || s.CreatedVia != api.CreatedViaInvite ||
		!s.CreatedAt.Equal(joined) || !s.LastLoginAt.Equal(joined) || !s.LastSeenAt.Equal(joined) || s.InvitedBy == nil ||
		*s.InvitedBy != (api.UserRef{ID: admin.ID, Username: "Alex"}) || s.Sessions != 1 || s.Devices != 1 || !s.Online ||
		s.ResetPending {
		t.Errorf("the member's row = %+v", s)
	}
	if kim.Status != api.UserStatusPending || kim.CreatedVia != api.CreatedViaSignup || kim.Role != api.RoleUser ||
		!kim.CreatedAt.Equal(start.Add(2*time.Minute)) || !kim.LastLoginAt.IsZero() || !kim.LastSeenAt.IsZero() ||
		kim.InvitedBy != nil || kim.Sessions != 0 || kim.Online {
		t.Errorf("the pending sign-up's row = %+v", kim)
	}
	if rob.ID != robin.ID || rob.Status != api.UserStatusDisabled || rob.Sessions != 0 || rob.Online {
		t.Errorf("the disabled account's row = %+v", rob)
	}

	// The shape of 03 §12.4.8: times in the wire form, no IPs, no hash.
	var raw struct {
		Users []map[string]json.RawMessage `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(keysOf(raw.Users[3])),
		"[createdAt createdVia devices id invitedBy lastLoginAt lastSeenAt online resetPending role sessions status username]"; got != want {
		t.Errorf("the member's keys = %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(keysOf(raw.Users[1])),
		"[createdAt createdVia devices id online resetPending role sessions status username]"; got != want {
		t.Errorf("the pending sign-up's keys = %s, want %s", got, want)
	}
	if got, want := string(raw.Users[3]["createdAt"]), `"2026-10-01T12:01:00.000Z"`; got != want {
		t.Errorf("createdAt = %s, want %s", got, want)
	}
	for _, secret := range []string{addrA, "192.0.2.", "argon2", "$"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("the user list contains %q: %s", secret, rec.Body)
		}
	}

	// ?status= narrows the list; anything but the three statuses is a bad query.
	for query, want := range map[string]string{
		"?status=active":   "Alex Sam",
		"?status=pending":  "Kim",
		"?status=disabled": "robin",
		"?other=1":         "Alex Kim robin Sam",
	} {
		if got := names(f.users(cookie, query)); got != want {
			t.Errorf("GET /admin/users%s = %s, want %s", query, got, want)
		}
	}
	for _, query := range []string{"?status=", "?status=all", "?status=Active", "?status=active&status=pending"} {
		wantError(t, f.call(http.MethodGet, "/api/v1/admin/users"+query, "", with(cookie)), http.StatusBadRequest, api.CodeBadRequest)
	}
	// An empty list is a list.
	if rec := f.decide(cookie, kim.ID, "reject", `{}`); rec.Code != http.StatusNoContent {
		t.Fatalf("reject: status %d", rec.Code)
	}
	rec = f.call(http.MethodGet, "/api/v1/admin/users?status=pending", "", with(cookie))
	if got := rec.Body.String(); rec.Code != http.StatusOK || got != `{"users":[]}`+"\n" {
		t.Errorf("no pending sign-ups: %d %s", rec.Code, got)
	}

	// Admins only; HEAD is served like GET.
	wantError(t, f.call(http.MethodGet, "/api/v1/admin/users", "", with(samCookie)), http.StatusForbidden, api.CodeForbidden)
	wantError(t, f.call(http.MethodGet, "/api/v1/admin/users", ""), http.StatusUnauthorized, api.CodeUnauthenticated)
	if rec := f.call(http.MethodHead, "/api/v1/admin/users", "", with(cookie)); rec.Code != http.StatusOK {
		t.Errorf("HEAD /admin/users: status %d", rec.Code)
	}
}

// TestRolesOverREST is 03 §15's roles case: demoting, disabling or deleting the last admin → 409 last_admin.
// Granting admin without the password → 403, and with it → 200 plus an admin_granted alert. A demoted admin gets
// 403 on the next admin call (the session cache was invalidated).
func TestRolesOverREST(t *testing.T) {
	f := newAuthFixture(t)
	admin, cookie, sam, samCookie := f.adminAndMember()
	ctx := context.Background()
	alexID, samID := store.UserID(admin.ID), store.UserID(sam.ID)
	f.alerts.take()
	f.conns.take()

	// The last admin stays one. Over REST the only admin can only ask for the own account, where the self rule
	// answers for disabling and deleting; the CLI gets last_admin for all three, and so does POST /me/delete.
	wantBody(t, f.patchUser(cookie, admin.ID, `{"role":"user"}`), http.StatusConflict, `{"error":{"code":"last_admin"}}`)
	wantBody(t, f.patchUser(cookie, admin.ID, `{"status":"disabled"}`), http.StatusConflict, `{"error":{"code":"self_action_forbidden"}}`)
	wantBody(t, f.call(http.MethodDelete, "/api/v1/admin/users/"+admin.ID, "", with(cookie)), http.StatusConflict,
		`{"error":{"code":"self_action_forbidden"}}`)
	wantError(t, f.call(http.MethodPost, "/api/v1/me/delete", `{"password":"`+testPassword+`"}`, with(cookie)),
		http.StatusConflict, api.CodeLastAdmin)
	off, member := store.StatusDisabled, store.RoleUser
	if _, err := f.svc.UpdateUser(ctx, store.CLIActor, alexID, auth.UserChange{Role: &member}); !api.IsCode(err, api.CodeLastAdmin) {
		t.Errorf("the CLI demoting the last admin: %v", err)
	}
	if _, err := f.svc.UpdateUser(ctx, store.CLIActor, alexID, auth.UserChange{Status: &off}); !api.IsCode(err, api.CodeLastAdmin) {
		t.Errorf("the CLI disabling the last admin: %v", err)
	}
	if err := f.svc.DeleteUser(ctx, store.CLIActor, alexID); !api.IsCode(err, api.CodeLastAdmin) {
		t.Errorf("the CLI deleting the last admin: %v", err)
	}
	f.wantSignedIn(cookie)
	if got := f.userRow(cookie, admin.ID); got.Role != api.RoleAdmin || got.Status != api.UserStatusActive {
		t.Fatalf("the last admin after the refused calls: %+v", got)
	}

	// Granting admin without the acting admin's password, or with a wrong one → 403 wrong_password.
	for _, body := range []string{`{"role":"admin"}`, `{"role":"admin","currentPassword":""}`,
		`{"role":"admin","currentPassword":"not the password"}`} {
		wantBody(t, f.patchUser(cookie, sam.ID, body), http.StatusForbidden, `{"error":{"code":"wrong_password"}}`)
	}
	wantBody(t, f.patchUser(cookie, sam.ID, `{"role":"owner","currentPassword":"`+testPassword+`"}`),
		http.StatusUnprocessableEntity, `{"error":{"code":"validation_failed","fields":{"role":"invalid"}}}`)
	if alerts, notes := f.alerts.take(), f.signal.takeNotes(); len(alerts) != 0 || len(notes) != 0 || len(f.auditOf("user.role_changed")) != 0 {
		t.Fatalf("refused grants: alerts %+v, notes %v", alerts, notes)
	}
	// A member can't grant anything, with whatever password.
	wantError(t, f.patchUser(samCookie, sam.ID, `{"role":"admin","currentPassword":"`+testPassword+`"}`),
		http.StatusForbidden, api.CodeForbidden)

	// With it → 200 {user}, the admin_granted alert, the hub and the SPAs are told, and the audit row.
	f.clk.advance(time.Minute)
	f.signal.takeChanges()
	rec := f.patchUser(cookie, sam.ID, `{"role":"admin","currentPassword":"`+testPassword+`"}`)
	got := decodeBody[api.AdminUserResponse](t, rec, http.StatusOK).User
	if got.ID != sam.ID || got.Username != "Sam" || got.Role != api.RoleAdmin || got.Status != api.UserStatusActive ||
		got.Sessions != 1 || got.InvitedBy == nil || got.InvitedBy.Username != "Alex" {
		t.Errorf("the new admin = %+v", got)
	}
	if strings.Contains(rec.Body.String(), "argon2") || strings.Contains(rec.Body.String(), testPassword) {
		t.Errorf("the answer contains a password or a hash: %s", rec.Body)
	}
	wantAlert := auth.AdminAlert{Kind: auth.AlertAdminGranted, Actor: "Alex", Target: "Sam", At: f.clk.now()}
	if alerts := f.alerts.take(); len(alerts) != 1 || alerts[0] != wantAlert {
		t.Errorf("alerts = %+v, want %+v", alerts, wantAlert)
	}
	if changes := f.signal.takeChanges(); fmt.Sprint(changes) != "["+sam.ID+" Sam admin=true]" {
		t.Errorf("Signal.UserChanged calls = %v", changes)
	}
	if notes := f.signal.takeNotes(); fmt.Sprint(notes) != "[user "+sam.ID+" [me] admins [admin.users]]" {
		t.Errorf("Signal notes after the grant = %v", notes)
	}
	if rows := f.auditOf("user.role_changed"); len(rows) != 1 || rows[0].Actor != (store.Actor{Kind: store.ActorUser,
		UserID: alexID, Name: "Alex", IP: addrA}) || rows[0].TargetKind != "user" || rows[0].TargetID != sam.ID ||
		rows[0].TargetName != "Sam" || fmt.Sprint(rows[0].Detail) != "map[from:user to:admin]" {
		t.Errorf("user.role_changed rows = %+v", rows)
	}
	if calls := f.conns.take(); len(calls) != 0 {
		t.Errorf("a role change closed connections: %+v", calls)
	}
	// The new admin is one on the next request, without logging in again.
	if me := decodeBody[api.Me](t, f.call(http.MethodGet, "/api/v1/me", "", with(samCookie)), http.StatusOK); !me.Permissions.Admin {
		t.Errorf("me of the new admin = %+v", me)
	}
	if got := names(f.users(samCookie, "")); got != "Alex Sam" {
		t.Errorf("the new admin's user list = %s", got)
	}

	// Demoting needs no password → 200 and the admin_revoked alert; the demoted admin gets 403 on the next admin
	// call.
	f.signal.takeNotes()
	f.signal.takeChanges()
	if got := f.patched(cookie, sam.ID, `{"role":"user"}`); got.Role != api.RoleUser {
		t.Errorf("the demoted admin = %+v", got)
	}
	if alerts := f.alerts.take(); len(alerts) != 1 || alerts[0].Kind != auth.AlertAdminRevoked || alerts[0].Target != "Sam" {
		t.Errorf("alerts after the demotion = %+v", alerts)
	}
	if changes := f.signal.takeChanges(); fmt.Sprint(changes) != "["+sam.ID+" Sam admin=false]" {
		t.Errorf("Signal.UserChanged calls after the demotion = %v", changes)
	}
	wantError(t, f.call(http.MethodGet, "/api/v1/admin/users", "", with(samCookie)), http.StatusForbidden, api.CodeForbidden)
	wantError(t, f.patchUser(samCookie, admin.ID, `{"role":"user"}`), http.StatusForbidden, api.CodeForbidden)
	f.wantSignedIn(samCookie)

	// With a second active admin, an admin may demote the own account, and is none from then on.
	f.patched(cookie, sam.ID, `{"role":"admin","currentPassword":"`+testPassword+`"}`)
	if got := f.patched(cookie, admin.ID, `{"role":"user"}`); got.Role != api.RoleUser {
		t.Errorf("the admin who demoted the own account = %+v", got)
	}
	wantError(t, f.call(http.MethodGet, "/api/v1/admin/users", "", with(cookie)), http.StatusForbidden, api.CodeForbidden)
	// Sam is the last one now.
	wantError(t, f.patchUser(samCookie, sam.ID, `{"role":"user"}`), http.StatusConflict, api.CodeLastAdmin)
	if err := f.svc.DeleteUser(ctx, store.CLIActor, samID); !api.IsCode(err, api.CodeLastAdmin) {
		t.Errorf("the CLI deleting the last admin: %v", err)
	}
}

// TestGrantAdminThrottledOverREST: the password an admin types to make another admin is a password check
// (03 §7.3): the sixth wrong one from an address is 429 with Retry-After.
func TestGrantAdminThrottledOverREST(t *testing.T) {
	f := newAuthFixture(t)
	_, cookie, sam, _ := f.adminAndMember()
	const busy = "203.0.113.200"
	for range 5 {
		wantError(t, f.patchUser(cookie, sam.ID, `{"role":"admin","currentPassword":"not the password"}`, from(busy)),
			http.StatusForbidden, api.CodeWrongPassword)
	}
	rec := f.patchUser(cookie, sam.ID, `{"role":"admin","currentPassword":"`+testPassword+`"}`, from(busy))
	if e := wantError(t, rec, http.StatusTooManyRequests, api.CodeRateLimited); e.RetryAfter != 120 || rec.Header().Get("Retry-After") != "120" {
		t.Errorf("the throttled grant: retryAfter %d, Retry-After %q; want 120", e.RetryAfter, rec.Header().Get("Retry-After"))
	}
	if got := f.userRow(cookie, sam.ID); got.Role != api.RoleUser {
		t.Errorf("the member after refused grants = %+v", got)
	}
	// From the admin's own address the right password works.
	if got := f.patched(cookie, sam.ID, `{"role":"admin","currentPassword":"`+testPassword+`"}`); got.Role != api.RoleAdmin {
		t.Errorf("the grant from another address = %+v", got)
	}
}

// TestRenameOverREST: PATCH /admin/users/{id} with a username (03 §12.4.8): the rules, the hub's UserChanged, and
// the new name on the user's next request.
func TestRenameOverREST(t *testing.T) {
	f := newAuthFixture(t)
	admin, cookie, sam, samCookie := f.adminAndMember()
	f.alerts.take()

	for body, want := range map[string]string{
		`{"username":""}`:          `{"username":"required"}`,
		`{"username":"s"}`:         `{"username":"too_short"}`,
		`{"username":"sam smith"}`: `{"username":"invalid"}`,
		`{"username":"system"}`:    `{"username":"reserved"}`,
		`{"username":"-sam","role":"boss","status":"pending"}`: `{"role":"invalid","status":"not_allowed","username":"invalid"}`,
	} {
		wantBody(t, f.patchUser(cookie, sam.ID, body), http.StatusUnprocessableEntity,
			`{"error":{"code":"validation_failed","fields":`+want+`}}`)
	}
	wantBody(t, f.patchUser(cookie, sam.ID, `{"username":"ALEX"}`), http.StatusConflict, `{"error":{"code":"username_taken"}}`)
	wantBody(t, f.patchUser(cookie, "nobody000000", `{"username":"Robin"}`), http.StatusNotFound, `{"error":{"code":"user_not_found"}}`)
	for _, body := range []string{``, `{"username":`, `[]`, `{"username":5}`, `{} {}`} {
		wantError(t, f.patchUser(cookie, sam.ID, body), http.StatusBadRequest, api.CodeBadRequest)
	}
	if notes, changes := f.signal.takeNotes(), f.signal.takeChanges(); len(notes) != 0 || len(changes) != 0 || len(f.auditOf("user.renamed")) != 0 {
		t.Fatalf("refused renames: notes %v, changes %v", notes, changes)
	}

	// A body without fields changes nothing, and tells nobody.
	if got := f.patched(cookie, sam.ID, `{}`); got.Username != "Sam" {
		t.Errorf("an empty patch = %+v", got)
	}
	if got := f.patched(cookie, sam.ID, `{"nickname":"Sammy","username":null}`); got.Username != "Sam" {
		t.Errorf("a patch of unknown fields = %+v", got)
	}
	if notes, changes := f.signal.takeNotes(), f.signal.takeChanges(); len(notes) != 0 || len(changes) != 0 {
		t.Errorf("patches without a change: notes %v, changes %v", notes, changes)
	}

	f.clk.advance(time.Minute)
	got := f.patched(cookie, sam.ID, `{"username":" Samuel "}`)
	if got.ID != sam.ID || got.Username != "Samuel" || got.Role != api.RoleUser || got.Sessions != 1 {
		t.Errorf("the renamed account = %+v", got)
	}
	if changes := f.signal.takeChanges(); fmt.Sprint(changes) != "["+sam.ID+" Samuel admin=false]" {
		t.Errorf("Signal.UserChanged calls = %v", changes)
	}
	if notes := f.signal.takeNotes(); fmt.Sprint(notes) != "[user "+sam.ID+" [me] admins [admin.users]]" {
		t.Errorf("Signal notes after the rename = %v", notes)
	}
	if rows := f.auditOf("user.renamed"); len(rows) != 1 || rows[0].Actor.Name != "Alex" || rows[0].Actor.IP != addrA ||
		rows[0].TargetID != sam.ID || rows[0].TargetName != "Samuel" || fmt.Sprint(rows[0].Detail) != "map[from:Sam to:Samuel]" {
		t.Errorf("user.renamed rows = %+v", rows)
	}
	// The user's next request has the new name, on the same session; the new name logs in, the old one is free.
	if me := decodeBody[api.Me](t, f.call(http.MethodGet, "/api/v1/me", "", with(samCookie)), http.StatusOK); me.User.Username != "Samuel" {
		t.Errorf("me after the rename = %+v", me.User)
	}
	decodeBody[api.UserResponse](t, f.login("samuel", testPassword, clientIP(5)), http.StatusOK)
	wantError(t, f.login("Sam", testPassword, clientIP(5)), http.StatusUnauthorized, api.CodeInvalidCredentials)
	if alerts, calls := f.alerts.take(), f.conns.take(); len(alerts) != 0 || len(calls) != 0 {
		t.Errorf("a rename alerted %+v or closed %+v", alerts, calls)
	}

	// An admin may rename the own account. The session cache forgot the old name at once: the admin's next change
	// is audited under the new one.
	if got := f.patched(cookie, admin.ID, `{"username":"Alexandra"}`); got.Username != "Alexandra" {
		t.Errorf("the admin's own rename = %+v", got)
	}
	if me := decodeBody[api.Me](t, f.call(http.MethodGet, "/api/v1/me", "", with(cookie)), http.StatusOK); me.User.Username != "Alexandra" ||
		!me.Permissions.Admin {
		t.Errorf("the admin's me after the own rename = %+v", me)
	}
	f.patched(cookie, sam.ID, `{"username":"Sam"}`)
	if rows := f.auditOf("user.renamed"); len(rows) != 3 || rows[2].Actor.Name != "Alexandra" || rows[2].TargetName != "Sam" {
		t.Errorf("user.renamed rows after the admin's own rename = %+v", rows)
	}
}

// TestUserStatusOverREST: disabling and enabling an account (03 §7.11, 03 §7.7 "Admin: disable user").
func TestUserStatusOverREST(t *testing.T) {
	f := newAuthFixture(t)
	_, cookie, sam, samCookie := f.adminAndMember()
	samID := store.UserID(sam.ID)
	f.linkDevice(sam.ID, "d1d1d1d1d1d1")
	f.setMode(cookie, "approval")
	f.signup("Kim", clientIP(3))
	kim := f.pending(cookie)[0]
	f.conns.take()
	f.signal.takeNotes()
	count := f.counter()

	// A pending sign-up goes through the approval queue; pending is no status to set.
	for _, body := range []string{`{"status":"active"}`, `{"status":"disabled"}`} {
		wantBody(t, f.patchUser(cookie, kim.ID, body), http.StatusUnprocessableEntity,
			`{"error":{"code":"validation_failed","fields":{"status":"not_allowed"}}}`)
	}
	wantBody(t, f.patchUser(cookie, sam.ID, `{"status":"pending"}`), http.StatusUnprocessableEntity,
		`{"error":{"code":"validation_failed","fields":{"status":"not_allowed"}}}`)
	if len(f.pending(cookie)) != 1 || len(f.signal.takeNotes()) != 0 {
		t.Fatal("refused status changes did something")
	}

	// Disable → 200; the account's sessions and devices are gone, its connections closed with account_disabled.
	f.clk.advance(time.Minute)
	f.wantSignedIn(samCookie) // in the session cache, as a request a moment ago leaves it
	got := f.patched(cookie, sam.ID, `{"status":"disabled"}`)
	if got.Status != api.UserStatusDisabled || got.Sessions != 0 || got.Devices != 0 || got.Role != api.RoleUser {
		t.Errorf("the disabled account = %+v", got)
	}
	wantCalls := []closedConn{{auth.ConnSelector{UserID: samID}, auth.ReasonAccountDisabled}}
	if calls := f.conns.take(); fmt.Sprint(calls) != fmt.Sprint(wantCalls) {
		t.Errorf("CloseConnections calls = %+v, want %+v", calls, wantCalls)
	}
	if notes, changes := f.signal.takeNotes(), f.signal.takeChanges(); fmt.Sprint(notes) != "[admins [admin.users]]" || len(changes) != 0 {
		t.Errorf("Signal after disabling: notes %v, changes %v", notes, changes)
	}
	f.wantUnauthenticated(samCookie)
	wantError(t, f.login("Sam", testPassword, clientIP(1)), http.StatusForbidden, api.CodeAccountDisabled)
	if n := count("SELECT count(*) FROM sessions WHERE user_id = ?", sam.ID) + count("SELECT count(*) FROM devices"); n != 0 {
		t.Errorf("%d sessions and devices of the disabled account left", n)
	}
	if rows := f.auditOf("user.disabled"); len(rows) != 1 || rows[0].Actor.Name != "Alex" || rows[0].Actor.IP != addrA ||
		rows[0].TargetID != sam.ID || rows[0].TargetName != "Sam" || len(rows[0].Detail) != 0 {
		t.Errorf("user.disabled rows = %+v", rows)
	}

	// Enable → 200; the password works again.
	got = f.patched(cookie, sam.ID, `{"status":"active"}`)
	if got.Status != api.UserStatusActive {
		t.Errorf("the enabled account = %+v", got)
	}
	if notes := f.signal.takeNotes(); fmt.Sprint(notes) != "[admins [admin.users]]" {
		t.Errorf("Signal notes after enabling = %v", notes)
	}
	if rows := f.auditOf("user.enabled"); len(rows) != 1 || rows[0].TargetID != sam.ID {
		t.Errorf("user.enabled rows = %+v", rows)
	}
	decodeBody[api.UserResponse](t, f.login("Sam", testPassword, clientIP(1)), http.StatusOK)
	if alerts, calls := f.alerts.take(), f.conns.take(); len(calls) != 0 {
		t.Errorf("enabling closed %+v (alerts %+v)", calls, alerts)
	}
}

// TestSignOutAndDeleteOverREST: POST /admin/users/{id}/sign-out and DELETE /admin/users/{id} (03 §12.3 #31, #33;
// 03 §7.7 "Admin: sign out everywhere", "Delete user").
func TestSignOutAndDeleteOverREST(t *testing.T) {
	f := newAuthFixture(t)
	admin, cookie, sam, samCookie := f.adminAndMember()
	samID := store.UserID(sam.ID)
	second := f.sessionCookie(f.login("Sam", testPassword, clientIP(1)), thirtyDays)
	f.linkDevice(sam.ID, "d1d1d1d1d1d1")
	f.conns.take()
	f.signal.takeNotes()

	wantBody(t, f.call(http.MethodPost, "/api/v1/admin/users/nobody000000/sign-out", `{}`, with(cookie)),
		http.StatusNotFound, `{"error":{"code":"user_not_found"}}`)

	// Sign out everywhere → 200 {sessions, devices}. The body is optional.
	rec := f.call(http.MethodPost, "/api/v1/admin/users/"+sam.ID+"/sign-out", "", with(cookie))
	wantBody(t, rec, http.StatusOK, `{"sessions":2,"devices":1}`)
	wantCalls := []closedConn{{auth.ConnSelector{UserID: samID}, auth.ReasonSessionRevoked}}
	if calls := f.conns.take(); fmt.Sprint(calls) != fmt.Sprint(wantCalls) {
		t.Errorf("CloseConnections calls = %+v, want %+v", calls, wantCalls)
	}
	if notes := f.signal.takeNotes(); fmt.Sprint(notes) != "[admins [admin.users]]" {
		t.Errorf("Signal notes after the sign-out = %v", notes)
	}
	f.wantUnauthenticated(samCookie)
	f.wantUnauthenticated(second)
	if rows := f.auditOf("user.signed_out"); len(rows) != 1 || rows[0].Actor.Name != "Alex" || rows[0].TargetID != sam.ID ||
		fmt.Sprint(rows[0].Detail) != "map[devices:1 sessions:2]" {
		t.Errorf("user.signed_out rows = %+v", rows)
	}
	// The password is as it was.
	samCookie = f.sessionCookie(f.login("Sam", testPassword, clientIP(1)), thirtyDays)
	if got := f.userRow(cookie, sam.ID); got.Sessions != 1 || got.Devices != 0 || got.ResetPending {
		t.Errorf("the account after signing in again = %+v", got)
	}
	f.conns.take()
	f.signal.takeNotes()

	// Delete → 204; the account is gone with its sessions, and the name is free.
	wantBody(t, f.call(http.MethodDelete, "/api/v1/admin/users/nobody000000", "", with(cookie)), http.StatusNotFound,
		`{"error":{"code":"user_not_found"}}`)
	wantNoContent(t, f.call(http.MethodDelete, "/api/v1/admin/users/"+sam.ID, "", with(cookie)))
	wantCalls = []closedConn{{auth.ConnSelector{UserID: samID}, auth.ReasonAccountDeleted}}
	if calls := f.conns.take(); fmt.Sprint(calls) != fmt.Sprint(wantCalls) {
		t.Errorf("CloseConnections calls = %+v, want %+v", calls, wantCalls)
	}
	if notes := f.signal.takeNotes(); fmt.Sprint(notes) != "[admins [admin.users]]" {
		t.Errorf("Signal notes after the deletion = %v", notes)
	}
	f.wantUnauthenticated(samCookie)
	if got := names(f.users(cookie, "")); got != "Alex" {
		t.Errorf("users after the deletion = %s", got)
	}
	if rows := f.auditOf("user.deleted"); len(rows) != 1 || rows[0].Actor.Name != "Alex" || rows[0].Actor.IP != addrA ||
		rows[0].TargetID != sam.ID || rows[0].TargetName != "Sam" || fmt.Sprint(rows[0].Detail) != "map[self:false]" {
		t.Errorf("user.deleted rows = %+v", rows)
	}
	wantError(t, f.call(http.MethodDelete, "/api/v1/admin/users/"+sam.ID, "", with(cookie)), http.StatusNotFound, api.CodeUserNotFound)
	wantError(t, f.login("Sam", testPassword, clientIP(1)), http.StatusUnauthorized, api.CodeInvalidCredentials)

	// An admin may sign the own account out everywhere: this session goes too, and the answer clears its cookie.
	other := f.sessionCookie(f.login("Alex", testPassword, addrB), thirtyDays)
	f.conns.take()
	rec = f.call(http.MethodPost, "/api/v1/admin/users/"+admin.ID+"/sign-out", `{}`, with(cookie))
	wantBody(t, rec, http.StatusOK, `{"sessions":2,"devices":0}`)
	wantCleared(t, rec)
	f.wantUnauthenticated(cookie)
	f.wantUnauthenticated(other)
	wantCalls = []closedConn{{auth.ConnSelector{UserID: store.UserID(admin.ID)}, auth.ReasonSessionRevoked}}
	if calls := f.conns.take(); fmt.Sprint(calls) != fmt.Sprint(wantCalls) {
		t.Errorf("CloseConnections calls = %+v, want %+v", calls, wantCalls)
	}
}

// TestAdminUserRoutes: the routes of this slice sit behind the /api/v1 chain like every other: Admin routes for
// the user endpoints (a member is 403, nobody is 401), Public ones for the reset link; JSON 405 with Allow; the CSRF
// check and the Content-Type rule on the unsafe methods; the body limits (03 §12.1).
func TestAdminUserRoutes(t *testing.T) {
	f := newAuthFixture(t)
	_, cookie, sam, samCookie := f.adminAndMember()
	f.conns.take()
	f.alerts.take()
	user := "/api/v1/admin/users/" + sam.ID

	adminRoutes := []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/admin/users", ``},
		{http.MethodPatch, user, `{"username":"Samuel"}`},
		{http.MethodDelete, user, ``},
		{http.MethodPost, user + "/password-reset", `{}`},
		{http.MethodPost, user + "/sign-out", `{}`},
	}
	for _, rt := range adminRoutes {
		for name, opts := range map[string][]reqOpt{
			"no cookie":     nil,
			"a dead cookie": {cookieValue(sessionCookieName, strings.Repeat("A", 43))},
			"Authorization": {with(cookie), header("Authorization", "Bearer isa_"+strings.Repeat("A", 43))},
		} {
			if rec := f.call(rt.method, rt.path, rt.body, opts...); rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with %s: status %d, want 401", rt.method, rt.path, name, rec.Code)
			}
		}
		wantError(t, f.call(rt.method, rt.path, rt.body, with(samCookie)), http.StatusForbidden, api.CodeForbidden)
		if rt.method == http.MethodGet {
			continue
		}
		rec := f.call(rt.method, rt.path, rt.body, with(cookie), header("Sec-Fetch-Site", "cross-site"), header("Origin", "https://evil.example"))
		wantError(t, rec, http.StatusForbidden, api.CodeCSRFFailed)
		rec = f.call(rt.method, rt.path, rt.body, with(cookie), header("Content-Type", "text/plain"))
		wantError(t, rec, http.StatusUnsupportedMediaType, api.CodeUnsupportedMediaType)
		rec = f.call(rt.method, rt.path, rt.body, with(cookie), header("Content-Type", ""))
		wantError(t, rec, http.StatusUnsupportedMediaType, api.CodeUnsupportedMediaType)
	}
	for _, path := range []string{"/api/v1/auth/reset/check", "/api/v1/auth/reset/complete"} {
		rec := f.call(http.MethodPost, path, `{"token":"x"}`, header("Sec-Fetch-Site", "cross-site"), header("Origin", "https://evil.example"))
		wantError(t, rec, http.StatusForbidden, api.CodeCSRFFailed)
		wantError(t, f.call(http.MethodPost, path, `{"token":"x"}`, header("Content-Type", "text/plain")),
			http.StatusUnsupportedMediaType, api.CodeUnsupportedMediaType)
		wantError(t, f.call(http.MethodPost, path, `{"token":"x"}`, header("Authorization", "Bearer x")),
			http.StatusUnauthorized, api.CodeUnauthenticated)
		for _, body := range []string{``, `{"token":`, `[]`, `{} {}`} {
			wantError(t, f.call(http.MethodPost, path, body), http.StatusBadRequest, api.CodeBadRequest)
		}
	}
	// Nothing happened: the member is as it was.
	f.wantSignedIn(samCookie)
	if got := f.userRow(cookie, sam.ID); got.Username != "Sam" || got.ResetPending || got.Sessions != 1 || len(f.conns.take()) != 0 {
		t.Errorf("refused requests changed something: %+v", got)
	}

	for path, allow := range map[string]string{
		user:                           "DELETE, PATCH",
		user + "/password-reset":       "POST",
		user + "/sign-out":             "POST",
		"/api/v1/auth/reset/check":     "POST",
		"/api/v1/auth/reset/complete":  "POST",
		user + "/":                     "",
		user + "/password-reset/again": "",
		"/api/v1/admin/user":           "",
		"/api/v1/auth/reset":           "",
	} {
		rec := f.call(http.MethodGet, path, "", with(cookie))
		if allow == "" {
			wantError(t, rec, http.StatusNotFound, api.CodeNotFound)
			continue
		}
		wantError(t, rec, http.StatusMethodNotAllowed, api.CodeMethodNotAllowed)
		if got := rec.Header().Get("Allow"); got != allow {
			t.Errorf("GET %s: Allow %q, want %q", path, got, allow)
		}
	}
	rec := f.call(http.MethodPost, "/api/v1/admin/users", `{}`, with(cookie))
	wantError(t, rec, http.StatusMethodNotAllowed, api.CodeMethodNotAllowed)
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("POST /admin/users: Allow %q, want GET, HEAD", got)
	}

	// The auth endpoints accept 16 KiB, the others 64 KiB (03 §12.1): the reset link's two are of the first kind.
	big := strings.Repeat("x", authBodyLimit)
	for _, path := range []string{"/api/v1/auth/reset/check", "/api/v1/auth/reset/complete"} {
		wantError(t, f.call(http.MethodPost, path, `{"token":"`+big+`"}`), http.StatusRequestEntityTooLarge, api.CodePayloadTooLarge)
	}
	wantError(t, f.patchUser(cookie, sam.ID, `{"username":"`+big+`"}`), http.StatusUnprocessableEntity, api.CodeValidationFailed)
	wantError(t, f.patchUser(cookie, sam.ID, `{"username":"`+strings.Repeat("x", otherBodyLimit)+`"}`),
		http.StatusRequestEntityTooLarge, api.CodePayloadTooLarge)
}

// TestAdminUserCallerGone: a request the chain let in on an admin who is none any more (the principal may be 30 s
// old) is 403, and changes nothing: auth.Service reads its actor in the Write of the change.
func TestAdminUserCallerGone(t *testing.T) {
	f := newAuthFixture(t)
	admin, cookie, sam, _ := f.adminAndMember()
	f.wantSignedIn(cookie) // the principal is in the session cache
	// Behind the service's back, as nothing in the server does: the cache still says admin.
	f.write(func(q *store.Q) error { return q.SetRole(store.UserID(admin.ID), store.RoleUser, f.clk.now()) })
	user := "/api/v1/admin/users/" + sam.ID
	for _, rt := range []struct{ method, path, body string }{
		{http.MethodPatch, user, `{"username":"Samuel"}`},
		{http.MethodDelete, user, ``},
		{http.MethodPost, user + "/password-reset", `{}`},
		{http.MethodPost, user + "/sign-out", `{}`},
	} {
		wantError(t, f.call(rt.method, rt.path, rt.body, with(cookie)), http.StatusForbidden, api.CodeForbidden)
	}
	f.write(func(q *store.Q) error { return q.SetRole(store.UserID(admin.ID), store.RoleAdmin, f.clk.now()) })
	if got := f.userRow(cookie, sam.ID); got.Username != "Sam" || got.ResetPending || got.Sessions != 1 {
		t.Errorf("forbidden requests changed something: %+v", got)
	}
}
