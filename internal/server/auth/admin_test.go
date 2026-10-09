package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// Admin users (README S64; 03 §7.11). The rows of 03 §7.7 that these calls are, are in the matrix of
// revoke_test.go; these tests are about each call's own rules, answers, audit rows and admin alerts.

// ptr is a pointer to v, for the fields of a UserChange.
func ptr[T any](v T) *T { return &v }

// adminEnv is a service with an admin, Alex, and a member, Sam, each logged in and in the session cache.
type adminEnv struct {
	*svcEnv
	alex, sam     store.User
	alexIn, samIn LoginResult
	asAlex        store.Actor // Alex acting from ipA
}

func newAdminEnv(t *testing.T) *adminEnv {
	t.Helper()
	e := newSvcEnv(t)
	a := &adminEnv{svcEnv: e}
	a.alex = e.addAdmin("Alex")
	a.sam = e.addUser("Sam")
	a.alexIn, a.samIn = e.login("Alex", ipA), e.login("Sam", ipB)
	a.asAlex = actorFor(a.alex, ipA)
	a.cache(a.alexIn, a.samIn)
	e.conns.take()
	e.alerts.take()
	e.hashes.Store(0)
	return a
}

// cache puts sessions into the session cache, where a request a moment ago would have left them.
func (a *adminEnv) cache(logins ...LoginResult) {
	a.t.Helper()
	for _, in := range logins {
		if _, err := a.svc.Authenticate(a.request(http.MethodGet, in.Token)); err != nil {
			a.t.Fatal(err)
		}
	}
}

// later is a minute later, with these sessions in the session cache again, as a request of each a moment ago
// would have left them: the cache keeps a row for 30 s, so what a call does to it shows only on a row that is in it.
func (a *adminEnv) later(logins ...LoginResult) {
	a.t.Helper()
	a.advance(time.Minute)
	a.cache(logins...)
}

// who is the principal a request with this session's cookie gets right now.
func (a *adminEnv) who(in LoginResult) Principal {
	a.t.Helper()
	p, err := a.svc.Authenticate(a.request(http.MethodGet, in.Token))
	if err != nil {
		a.t.Fatalf("Authenticate: %v", err)
	}
	return p
}

// update is UpdateUser as Alex.
func (a *adminEnv) update(id store.UserID, ch UserChange) (store.User, error) {
	return a.svc.UpdateUser(context.Background(), a.asAlex, id, ch)
}

// wantFields checks a validation_failed answer's field codes.
func wantFields(t *testing.T, err error, want map[string]string) {
	t.Helper()
	if ae := wantCode(t, err, api.CodeValidationFailed); fmt.Sprint(ae.Fields) != fmt.Sprint(want) {
		t.Errorf("fields %v, want %v", ae.Fields, want)
	}
}

// wantAudit checks the newest audit row of an action: its actor, its target and its detail.
func (e *svcEnv) wantAudit(action string, rows int, actor store.Actor, target store.User, detail string) {
	e.t.Helper()
	got := e.audit(action)
	if len(got) != rows {
		e.t.Fatalf("%d %s rows, want %d: %+v", len(got), action, rows, got)
	}
	last := got[len(got)-1]
	if last.Actor != actor || last.TargetKind != targetUser || last.TargetID != string(target.ID) ||
		last.TargetName != target.Username || fmt.Sprint(last.Detail) != detail || last.Outcome != "ok" ||
		!last.At.Equal(e.clk.now()) {
		e.t.Errorf("%s row = %+v; want actor %+v, target %s %q, detail %s", action, last, actor, target.ID,
			target.Username, detail)
	}
}

func TestUpdateUserRename(t *testing.T) {
	a := newAdminEnv(t)
	pending := a.addUser("kim_p", func(u *store.User) { u.Status, u.CreatedVia = store.StatusPending, "signup" })

	// The username rules, before anything else is looked at; a taken name, a pending sign-up's too.
	for name, want := range map[string]map[string]string{
		"":                      {"username": "required"},
		"a":                     {"username": "too_short"},
		"-sam":                  {"username": "invalid"},
		"sam smith":             {"username": "invalid"},
		"system":                {"username": "reserved"},
		strings.Repeat("x", 33): {"username": "too_long"},
	} {
		_, err := a.update(a.sam.ID, UserChange{Username: &name})
		wantFields(t, err, want)
	}
	for _, name := range []string{"ALEX", "Kim_P"} {
		_, err := a.update(a.sam.ID, UserChange{Username: &name})
		wantCode(t, err, api.CodeUsernameTaken)
	}
	_, err := a.update("nobody000000", UserChange{Username: ptr("Robin")})
	wantCode(t, err, api.CodeUserNotFound)
	// A bad value is refused before the target is looked for.
	_, err = a.update("nobody000000", UserChange{Username: ptr("-")})
	wantFields(t, err, map[string]string{"username": "too_short"})
	if a.auditCount(auditUserRenamed) != 0 || a.who(a.samIn).Username != "Sam" {
		t.Fatal("a refused rename changed something")
	}

	// The name it has: nothing happens.
	got, err := a.update(a.sam.ID, UserChange{Username: ptr(" Sam ")})
	if err != nil || got.Username != "Sam" || !got.UpdatedAt.Equal(a.sam.UpdatedAt) || a.auditCount(auditUserRenamed) != 0 {
		t.Fatalf("a rename to the own name: %+v, %v", got, err)
	}

	// A rename: the row, the audit row with both names, and the session cache at once.
	a.later(a.alexIn, a.samIn)
	got, err = a.update(a.sam.ID, UserChange{Username: ptr(" Samuel ")})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != a.sam.ID || got.Username != "Samuel" || got.UsernameKey != "samuel" || !got.UpdatedAt.Equal(a.clk.now()) ||
		got.Role != store.RoleUser || got.Status != store.StatusActive || got.PasswordHash != a.sam.PasswordHash {
		t.Errorf("the renamed account = %+v", got)
	}
	a.wantAudit(auditUserRenamed, 1, a.asAlex, got, "map[from:Sam to:Samuel]")
	if p := a.who(a.samIn); p.Username != "Samuel" || p.SessionID != a.samIn.Session.ID {
		t.Errorf("Sam's next request after the rename: %+v, want the new name on the same session", p)
	}
	// A rename closes no connection and raises no alert, and the old name is free; the new one logs in.
	if calls, alerts := a.conns.take(), a.alerts.take(); len(calls) != 0 || len(alerts) != 0 {
		t.Errorf("a rename closed %+v and alerted %+v", calls, alerts)
	}
	if _, err := a.svc.UserByUsername(context.Background(), "sam"); !api.IsCode(err, api.CodeUserNotFound) {
		t.Errorf("the old name after the rename: %v", err)
	}
	a.login("samuel", ipB)

	// Another spelling of the own name is a rename; an admin may rename the own account.
	got, err = a.update(a.sam.ID, UserChange{Username: ptr("SAMUEL")})
	if err != nil || got.Username != "SAMUEL" || got.UsernameKey != "samuel" {
		t.Fatalf("a change of case: %+v, %v", got, err)
	}
	a.wantAudit(auditUserRenamed, 2, a.asAlex, got, "map[from:Samuel to:SAMUEL]")
	got, err = a.update(a.alex.ID, UserChange{Username: ptr("Alexandra")})
	if err != nil || got.Username != "Alexandra" || a.who(a.alexIn).Username != "Alexandra" {
		t.Fatalf("renaming oneself: %+v, %v", got, err)
	}
	// A pending sign-up can be renamed before it is approved.
	if got, err = a.update(pending.ID, UserChange{Username: ptr("Kim")}); err != nil || got.Username != "Kim" || got.Status != store.StatusPending {
		t.Fatalf("renaming a pending sign-up: %+v, %v", got, err)
	}
	if a.hashes.Load() != 1 {
		t.Errorf("%d hashes, want only the one of the login under the new name", a.hashes.Load())
	}
}

func TestUpdateUserRole(t *testing.T) {
	a := newAdminEnv(t)
	ctx := context.Background()
	admin, member := store.RoleAdmin, store.RoleUser

	// A role that is none, and the role it has.
	_, err := a.update(a.sam.ID, UserChange{Role: ptr(store.Role("owner")), ActorPassword: testPassword})
	wantFields(t, err, map[string]string{"role": "invalid"})
	if got, err := a.update(a.sam.ID, UserChange{Role: &member}); err != nil || got.Role != member {
		t.Fatalf("the role it has: %+v, %v", got, err)
	}
	if got, err := a.update(a.alex.ID, UserChange{Role: &admin}); err != nil || got.Role != admin {
		t.Fatalf("admin for an admin, without a password: %+v, %v", got, err)
	}

	// Granting admin needs the acting admin's own password: none and a wrong one are 403 wrong_password.
	_, err = a.update(a.sam.ID, UserChange{Role: &admin})
	wantCode(t, err, api.CodeWrongPassword)
	if a.hashes.Load() != 0 {
		t.Errorf("a grant without a password hashed %d times", a.hashes.Load())
	}
	_, err = a.update(a.sam.ID, UserChange{Role: &admin, ActorPassword: "not the password"})
	wantCode(t, err, api.CodeWrongPassword)
	_, err = a.update(a.sam.ID, UserChange{Role: &admin, ActorPassword: strings.Repeat("x", passwordMaxBytes+1)})
	wantCode(t, err, api.CodeWrongPassword)
	if a.hashes.Load() != 1 || a.auditCount(auditUserRoleChanged) != 0 || len(a.alerts.take()) != 0 || a.who(a.samIn).IsAdmin() {
		t.Fatalf("refused grants: %d hashes, or something changed", a.hashes.Load())
	}
	// The target's password is not the actor's.
	a.write(func(q *store.Q) error {
		return q.SetPasswordHash(a.sam.ID, a.svc.hasher.compute([]byte(newPassword)), a.clk.now())
	})
	_, err = a.update(a.sam.ID, UserChange{Role: &admin, ActorPassword: newPassword})
	wantCode(t, err, api.CodeWrongPassword)

	// With it → the role, the audit row, the alert, and the session cache at once.
	a.later(a.alexIn, a.samIn)
	a.hashes.Store(0)
	got, err := a.update(a.sam.ID, UserChange{Role: &admin, ActorPassword: testPassword})
	if err != nil || got.Role != admin || !got.UpdatedAt.Equal(a.clk.now()) {
		t.Fatalf("the grant: %+v, %v", got, err)
	}
	if a.hashes.Load() != 1 {
		t.Errorf("the grant hashed %d times, want 1", a.hashes.Load())
	}
	a.wantAudit(auditUserRoleChanged, 1, a.asAlex, got, "map[from:user to:admin]")
	want := AdminAlert{Kind: AlertAdminGranted, Actor: "Alex", Target: "Sam", At: a.clk.now()}
	if alerts := a.alerts.take(); len(alerts) != 1 || alerts[0] != want {
		t.Errorf("alerts after the grant = %+v, want %+v", alerts, want)
	}
	if p := a.who(a.samIn); !p.IsAdmin() {
		t.Errorf("Sam's next request after the grant: %+v, want an admin", p)
	}
	if calls := a.conns.take(); len(calls) != 0 {
		t.Errorf("a role change closed connections: %+v", calls)
	}
	// The wrong passwords before are failed password checks, as a login's are; the right one left this address's
	// bucket for the admin's name full again (03 §7.3).
	if got := a.heldTokens(); got != [2]int{0, 1} {
		t.Errorf("%v keys with tokens taken after the grant, want [0 1]", got)
	}

	// Demoting needs no password: the audit row, the alert, and the demoted admin is none on the next call.
	asSam := actorFor(a.sam, ipB)
	if _, err := a.svc.UpdateUser(ctx, asSam, a.alex.ID, UserChange{Username: ptr("Alexander")}); err != nil {
		t.Fatalf("the new admin acting: %v", err)
	}
	a.later(a.alexIn, a.samIn)
	got, err = a.update(a.sam.ID, UserChange{Role: &member})
	if err != nil || got.Role != member {
		t.Fatalf("the demotion: %+v, %v", got, err)
	}
	a.wantAudit(auditUserRoleChanged, 2, a.asAlex, got, "map[from:admin to:user]")
	want = AdminAlert{Kind: AlertAdminRevoked, Actor: "Alex", Target: "Sam", At: a.clk.now()}
	if alerts := a.alerts.take(); len(alerts) != 1 || alerts[0] != want {
		t.Errorf("alerts after the demotion = %+v, want %+v", alerts, want)
	}
	if p := a.who(a.samIn); p.IsAdmin() {
		t.Errorf("Sam's next request after the demotion: %+v, want a member (the cache was invalidated)", p)
	}
	_, err = a.svc.UpdateUser(ctx, asSam, a.alex.ID, UserChange{Username: ptr("Al")})
	wantCode(t, err, api.CodeForbidden)

	// The CLI grants without a password, and is the alert's actor.
	got, err = a.svc.UpdateUser(ctx, store.CLIActor, a.sam.ID, UserChange{Role: &admin})
	if err != nil || got.Role != admin {
		t.Fatalf("set-role admin from the CLI: %+v, %v", got, err)
	}
	a.wantAudit(auditUserRoleChanged, 3, store.CLIActor, got, "map[from:user to:admin]")
	if alerts := a.alerts.take(); len(alerts) != 1 || alerts[0].Kind != AlertAdminGranted || alerts[0].Actor != "cli" {
		t.Errorf("alerts after the CLI's grant = %+v", alerts)
	}
	if a.hashes.Load() != 1 {
		t.Errorf("%d hashes in all, want the one of Alex's grant", a.hashes.Load())
	}
}

// TestLastAdmin is 03 §7.11's rule: at least one active admin always exists. Demoting, disabling or deleting the
// last one is 409 last_admin, whoever asks.
func TestLastAdmin(t *testing.T) {
	a := newAdminEnv(t)
	ctx := context.Background()
	admin, member, off, on := store.RoleAdmin, store.RoleUser, store.StatusDisabled, store.StatusActive

	// Alex is the only admin: nobody demotes, disables or deletes that account.
	for _, actor := range []store.Actor{a.asAlex, store.CLIActor} {
		_, err := a.svc.UpdateUser(ctx, actor, a.alex.ID, UserChange{Role: &member})
		wantCode(t, err, api.CodeLastAdmin)
		_, err = a.svc.UpdateUser(ctx, actor, a.alex.ID, UserChange{Role: &member, Username: ptr("Al")})
		wantCode(t, err, api.CodeLastAdmin)
	}
	_, err := a.svc.UpdateUser(ctx, store.CLIActor, a.alex.ID, UserChange{Status: &off})
	wantCode(t, err, api.CodeLastAdmin)
	wantCode(t, a.svc.DeleteUser(ctx, store.CLIActor, a.alex.ID), api.CodeLastAdmin)
	// The admin's own endpoints refuse the own account first (the self rule), with the same result.
	_, err = a.update(a.alex.ID, UserChange{Status: &off})
	wantCode(t, err, api.CodeSelfActionForbidden)
	wantCode(t, a.svc.DeleteUser(ctx, a.asAlex, a.alex.ID), api.CodeSelfActionForbidden)

	// A second admin who is disabled does not count: Alex is still the last active one.
	kim := a.addAdmin("Kim")
	if _, err := a.update(kim.ID, UserChange{Status: &off}); err != nil {
		t.Fatal(err)
	}
	_, err = a.update(a.alex.ID, UserChange{Role: &member})
	wantCode(t, err, api.CodeLastAdmin)
	// Demoting or deleting the disabled one is fine: it was no active admin.
	if got, err := a.update(kim.ID, UserChange{Role: &member}); err != nil || got.Role != member {
		t.Fatalf("demoting a disabled admin: %+v, %v", got, err)
	}
	if u := a.userRow(a.alex.ID); u == nil || u.Role != admin || u.Status != on || u.Username != "Alex" {
		t.Fatalf("the last admin after the refused calls: %+v", u)
	}
	a.alerts.take()

	// With another active admin, an admin may demote the own account.
	if _, err := a.update(a.sam.ID, UserChange{Role: &admin, ActorPassword: testPassword}); err != nil {
		t.Fatal(err)
	}
	a.alerts.take()
	got, err := a.update(a.alex.ID, UserChange{Role: &member})
	if err != nil || got.Role != member {
		t.Fatalf("demoting oneself with another admin around: %+v, %v", got, err)
	}
	if alerts := a.alerts.take(); len(alerts) != 1 || alerts[0].Kind != AlertAdminRevoked || alerts[0].Target != "Alex" {
		t.Errorf("alerts after the self-demotion = %+v", alerts)
	}
	// Now Sam is the last one, and Alex is no admin any more.
	_, err = a.update(a.sam.ID, UserChange{Role: &member})
	wantCode(t, err, api.CodeForbidden)
	wantCode(t, a.svc.DeleteUser(ctx, store.CLIActor, a.sam.ID), api.CodeLastAdmin)
	// Both changes that would end it at once are one refusal.
	_, err = a.svc.UpdateUser(ctx, store.CLIActor, a.sam.ID, UserChange{Role: &member, Status: &off})
	wantCode(t, err, api.CodeLastAdmin)
}

// TestLastAdminRace: two admins who demote their own accounts at the same moment can both pass the first check.
// The Write plans again on the rows as they are then, so one of them stays an admin, whatever the interleaving.
func TestLastAdminRace(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	admins := []store.User{e.addAdmin("Alex"), e.addAdmin("Robin")}
	admin, member := store.RoleAdmin, store.RoleUser
	for round := range 20 {
		var wg sync.WaitGroup
		errs := make([]error, len(admins))
		for i, u := range admins {
			wg.Go(func() { _, errs[i] = e.svc.UpdateUser(ctx, actorFor(u, ipA), u.ID, UserChange{Role: &member}) })
		}
		wg.Wait()
		codes := map[string]int{}
		for _, err := range errs {
			switch {
			case err == nil:
				codes["ok"]++
			case api.IsCode(err, api.CodeLastAdmin):
				codes[api.CodeLastAdmin]++
			default:
				t.Errorf("round %d: UpdateUser: %v", round, err)
			}
		}
		var left int
		e.read(func(q *store.Q) error {
			var err error
			left, err = q.CountActiveAdmins()
			return err
		})
		if codes["ok"] != 1 || codes[api.CodeLastAdmin] != 1 || left != 1 {
			t.Fatalf("round %d: results %v with %d active admins left; want one demotion, one last_admin and one admin",
				round, codes, left)
		}
		for _, u := range admins { // two admins again for the next round
			if _, err := e.svc.UpdateUser(ctx, store.CLIActor, u.ID, UserChange{Role: &admin}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestUpdateUserStatus(t *testing.T) {
	a := newAdminEnv(t)
	off, on := store.StatusDisabled, store.StatusActive
	pending := a.addUser("kim_p", func(u *store.User) { u.Status, u.CreatedVia = store.StatusPending, "signup" })
	a.write(func(q *store.Q) error {
		return q.ReplacePasswordReset(store.PasswordReset{TokenHash: []byte("a-pending-link"), UserID: a.sam.ID,
			ExpiresAt: a.clk.now().Add(time.Hour)})
	})

	// Only active and disabled can be set, and never for a pending sign-up.
	for _, status := range []store.UserStatus{store.StatusPending, "banned", ""} {
		_, err := a.update(a.sam.ID, UserChange{Status: &status})
		wantFields(t, err, map[string]string{"status": "not_allowed"})
	}
	for _, status := range []store.UserStatus{on, off} {
		_, err := a.update(pending.ID, UserChange{Status: &status})
		wantFields(t, err, map[string]string{"status": "not_allowed"})
	}
	if u := a.userRow(pending.ID); u == nil || u.Status != store.StatusPending {
		t.Fatalf("the pending sign-up after refused changes: %+v", u)
	}
	// The status it has: nothing happens.
	if got, err := a.update(a.sam.ID, UserChange{Status: &on}); err != nil || got.Status != on || a.auditCount(auditUserEnabled) != 0 {
		t.Fatalf("enabling an active account: %+v, %v", got, err)
	}
	if calls := a.conns.take(); len(calls) != 0 {
		t.Fatalf("no change closed connections: %+v", calls)
	}

	// Disable: the status, every credential of the account, the audit row, and the connections.
	a.later(a.samIn)
	got, err := a.update(a.sam.ID, UserChange{Status: &off})
	if err != nil || got.Status != off || !got.UpdatedAt.Equal(a.clk.now()) || got.PasswordHash != a.sam.PasswordHash {
		t.Fatalf("disabling: %+v, %v", got, err)
	}
	a.wantAudit(auditUserDisabled, 1, a.asAlex, got, "map[]")
	wantCalls := []closedConn{{ConnSelector{UserID: a.sam.ID}, ReasonAccountDisabled}}
	if calls := a.conns.take(); fmt.Sprint(calls) != fmt.Sprint(wantCalls) {
		t.Errorf("CloseConnections calls = %+v, want %+v", calls, wantCalls)
	}
	if n := len(a.sessions(a.sam.ID)); n != 0 || a.rawInt("SELECT count(*) FROM password_resets") != 0 {
		t.Errorf("after disabling: %d sessions, or the reset link is still there", n)
	}
	_, err = a.svc.Authenticate(a.request(http.MethodGet, a.samIn.Token))
	wantCode(t, err, api.CodeUnauthenticated)
	wantCode(t, a.tryLogin("Sam", testPassword, ipB), api.CodeAccountDisabled)
	if alerts := a.alerts.take(); len(alerts) != 0 {
		t.Errorf("disabling a member alerted: %+v", alerts)
	}
	// Again: nothing more happens.
	if _, err := a.update(a.sam.ID, UserChange{Status: &off}); err != nil || a.auditCount(auditUserDisabled) != 1 || len(a.conns.take()) != 0 {
		t.Errorf("disabling a disabled account did something: %v", err)
	}

	// Enable: the status and the audit row; the password works again.
	a.advance(time.Minute)
	got, err = a.update(a.sam.ID, UserChange{Status: &on})
	if err != nil || got.Status != on {
		t.Fatalf("enabling: %+v, %v", got, err)
	}
	a.wantAudit(auditUserEnabled, 1, a.asAlex, got, "map[]")
	if calls := a.conns.take(); len(calls) != 0 {
		t.Errorf("enabling closed connections: %+v", calls)
	}
	a.login("Sam", ipB)
	if a.hashes.Load() != 2 {
		t.Errorf("%d hashes, want the two logins only", a.hashes.Load())
	}
}

// TestUpdateUserAllAtOnce: one call may rename, change the role and the status together; each change has its own
// audit row, under the new name.
func TestUpdateUserAllAtOnce(t *testing.T) {
	a := newAdminEnv(t)
	admin, off := store.RoleAdmin, store.StatusDisabled
	a.later(a.samIn)
	got, err := a.update(a.sam.ID, UserChange{Username: ptr("Samuel"), Role: &admin, Status: &off, ActorPassword: testPassword})
	if err != nil || got.Username != "Samuel" || got.Role != admin || got.Status != off {
		t.Fatalf("UpdateUser with all three: %+v, %v", got, err)
	}
	a.wantAudit(auditUserRenamed, 1, a.asAlex, got, "map[from:Sam to:Samuel]")
	a.wantAudit(auditUserRoleChanged, 1, a.asAlex, got, "map[from:user to:admin]")
	a.wantAudit(auditUserDisabled, 1, a.asAlex, got, "map[]")
	want := AdminAlert{Kind: AlertAdminGranted, Actor: "Alex", Target: "Samuel", At: a.clk.now()}
	if alerts := a.alerts.take(); len(alerts) != 1 || alerts[0] != want {
		t.Errorf("alerts = %+v, want %+v", alerts, want)
	}
	wantCalls := []closedConn{{ConnSelector{UserID: a.sam.ID}, ReasonAccountDisabled}}
	if calls := a.conns.take(); fmt.Sprint(calls) != fmt.Sprint(wantCalls) {
		t.Errorf("CloseConnections calls = %+v, want %+v", calls, wantCalls)
	}
	// An empty change is none.
	before := a.rawCount("audit_log")
	if got, err := a.update(a.sam.ID, UserChange{ActorPassword: "ignored"}); err != nil || got.Username != "Samuel" {
		t.Fatalf("an empty change: %+v, %v", got, err)
	}
	if a.rawCount("audit_log") != before || a.hashes.Load() != 1 {
		t.Errorf("an empty change wrote a row or hashed (%d hashes, want the grant's one)", a.hashes.Load())
	}
}

// TestGrantAdminThrottled: the password an admin types to make another admin is a password check like a login's
// (03 §7.3): the sixth wrong one from an address is refused without a hash.
func TestGrantAdminThrottled(t *testing.T) {
	a := newAdminEnv(t)
	admin := store.RoleAdmin
	for i := range 5 {
		_, err := a.update(a.sam.ID, UserChange{Role: &admin, ActorPassword: fmt.Sprintf("guess number %d", i)})
		wantCode(t, err, api.CodeWrongPassword)
	}
	_, err := a.update(a.sam.ID, UserChange{Role: &admin, ActorPassword: testPassword})
	if ae := wantCode(t, err, api.CodeRateLimited); ae.RetryAfter != 120 {
		t.Errorf("the blocked grant: retryAfter %d, want 120", ae.RetryAfter)
	}
	_, err = a.svc.IssuePasswordReset(context.Background(), a.asAlex, a.addAdmin("Kim").ID, testPassword)
	wantCode(t, err, api.CodeRateLimited)
	if a.hashes.Load() != 5 {
		t.Errorf("%d hashes for 5 wrong passwords and 2 blocked attempts, want 5", a.hashes.Load())
	}
	if u := a.userRow(a.sam.ID); u == nil || u.Role != store.RoleUser {
		t.Fatalf("a blocked grant changed the account: %+v", u)
	}
	// From the admin's other address the right password works.
	if _, err := a.svc.UpdateUser(context.Background(), actorFor(a.alex, ipC), a.sam.ID,
		UserChange{Role: &admin, ActorPassword: testPassword}); err != nil {
		t.Fatalf("the grant from another address: %v", err)
	}
}

// TestGrantAdminRechecksInWrite: the accounts are judged as they are when the grant is written, not as they were
// when the password was checked.
func TestGrantAdminRechecksInWrite(t *testing.T) {
	admin := store.RoleAdmin
	for _, tc := range []struct {
		name   string
		change func(q *store.Q, a *adminEnv, now time.Time) error
		code   string // "" = the call succeeds, changing nothing
	}{
		{"the actor was demoted", func(q *store.Q, a *adminEnv, now time.Time) error {
			return q.SetRole(a.alex.ID, store.RoleUser, now)
		}, api.CodeForbidden},
		{"the actor was disabled", func(q *store.Q, a *adminEnv, now time.Time) error {
			return q.SetStatus(a.alex.ID, store.StatusDisabled, now)
		}, api.CodeForbidden},
		{"the actor's password changed", func(q *store.Q, a *adminEnv, now time.Time) error {
			return q.SetPasswordHash(a.alex.ID, strings.Replace(a.alex.PasswordHash, "$m=64", "$m=65", 1), now)
		}, api.CodeWrongPassword},
		{"the target was deleted", func(q *store.Q, a *adminEnv, _ time.Time) error {
			return q.DeleteUser(a.sam.ID)
		}, api.CodeUserNotFound},
		{"the target is an admin already", func(q *store.Q, a *adminEnv, now time.Time) error {
			return q.SetRole(a.sam.ID, store.RoleAdmin, now)
		}, ""},
	} {
		a := newAdminEnv(t)
		a.changeDuringCheck(func(q *store.Q, now time.Time) error { return tc.change(q, a, now) })
		got, err := a.update(a.sam.ID, UserChange{Role: &admin, ActorPassword: testPassword})
		if tc.code != "" {
			wantCode(t, err, tc.code)
		} else if err != nil || got.Role != admin {
			t.Errorf("%s: %+v, %v", tc.name, got, err)
		}
		if a.auditCount(auditUserRoleChanged) != 0 || len(a.alerts.take()) != 0 || len(a.conns.take()) != 0 {
			t.Errorf("%s: the call wrote a row, alerted or closed connections", tc.name)
		}
	}
}

// changeBeforeWrite makes the service's clock run change once, the next time it is read. A call of UpdateUser or
// IssuePasswordReset that asks for no password reads the clock exactly once between its planning Read and its Write
// (now := s.now()), so that is where the change lands: after the call planned on the accounts as they were, before
// its Write plans again. changeDuringCheck can't get there: it runs in the hasher, which only a call that already
// asked for a password reaches.
func (e *svcEnv) changeBeforeWrite(change func(q *store.Q, now time.Time) error) {
	var once sync.Once
	e.svc.now = func() time.Time {
		once.Do(func() { e.write(func(q *store.Q) error { return change(q, e.clk.now()) }) })
		return e.clk.now()
	}
}

// TestGrantAdminNeedArisesBeforeWrite: a patch with a name and the role admin for an account that is an admin
// already is planned as a rename, which needs no password. If the account is demoted before the Write, the same
// patch makes an admin after all: the Write needs the acting admin's password, which nobody verified, and answers
// wrong_password without writing anything. That goes for a request that carries the right password too: it was
// never checked (no hash), and the next attempt is planned as a grant from the start.
func TestGrantAdminNeedArisesBeforeWrite(t *testing.T) {
	for _, password := range []string{"", testPassword} {
		a := newAdminEnv(t)
		kim := a.addAdmin("Kim")
		a.changeBeforeWrite(func(q *store.Q, now time.Time) error { return q.SetRole(kim.ID, store.RoleUser, now) })
		ch := UserChange{Username: ptr("Kimberly"), Role: ptr(store.RoleAdmin), ActorPassword: password}
		_, err := a.update(kim.ID, ch)
		if a.hashes.Load() != 0 {
			t.Fatalf("password %q: %d hashes; the call asked for the password, so the change did not land between "+
				"its Read and its Write", password, a.hashes.Load())
		}
		wantCode(t, err, api.CodeWrongPassword)
		if u := a.userRow(kim.ID); u == nil || u.Username != "Kim" || u.Role != store.RoleUser {
			t.Errorf("password %q: the account after the refused patch: %+v; want Kim, demoted and not renamed", password, u)
		}
		if a.auditCount(auditUserRoleChanged) != 0 || a.auditCount(auditUserRenamed) != 0 || len(a.alerts.take()) != 0 ||
			len(a.conns.take()) != 0 {
			t.Errorf("password %q: the refused patch wrote a row, alerted or closed connections", password)
		}

		// Asked again, the patch is a grant: it takes the password, and then it goes through.
		if password == "" {
			_, err := a.update(kim.ID, ch)
			wantCode(t, err, api.CodeWrongPassword)
			continue
		}
		if got, err := a.update(kim.ID, ch); err != nil || got.Username != "Kimberly" || got.Role != store.RoleAdmin ||
			a.hashes.Load() != 1 {
			t.Errorf("the patch again, with the password: %+v, %v, %d hashes", got, err, a.hashes.Load())
		}
	}
}

// TestConfirmedBy: a change that needs the acting admin's password is written only when that password was verified
// against the hash the account has at the Write. One that was never asked for (the need arose after the first read)
// is not confirmed.
func TestConfirmedBy(t *testing.T) {
	alex := store.User{ID: "a1a1a1a1a1a1", PasswordHash: katDefaultPHC}
	changed, other := alex, alex
	changed.PasswordHash = katForeignPHC
	other.ID = "b2b2b2b2b2b2"
	for name, tc := range map[string]struct {
		checked *store.User
		current store.User
		want    bool
	}{
		"verified, and unchanged since": {&alex, alex, true},
		"never asked for":               {nil, alex, false},
		"the password changed since":    {&alex, changed, false},
		"verified for another account":  {&other, alex, false},
	} {
		if got := confirmedBy(tc.checked, tc.current); got != tc.want {
			t.Errorf("%s: confirmedBy = %v, want %v", name, got, tc.want)
		}
	}
}

func TestDeleteUser(t *testing.T) {
	a := newAdminEnv(t)
	ctx := context.Background()
	wantCode(t, a.svc.DeleteUser(ctx, a.asAlex, "nobody000000"), api.CodeUserNotFound)
	wantCode(t, a.svc.DeleteUser(ctx, a.asAlex, a.alex.ID), api.CodeSelfActionForbidden)
	if a.rawCount("users") != 2 || len(a.conns.take()) != 0 || a.auditCount(auditUserDeleted) != 0 {
		t.Fatal("a refused deletion changed something")
	}

	a.later(a.samIn)
	if err := a.svc.DeleteUser(ctx, a.asAlex, a.sam.ID); err != nil {
		t.Fatal(err)
	}
	a.wantAudit(auditUserDeleted, 1, a.asAlex, a.sam, "map[self:false]")
	wantCalls := []closedConn{{ConnSelector{UserID: a.sam.ID}, ReasonAccountDeleted}}
	if calls := a.conns.take(); fmt.Sprint(calls) != fmt.Sprint(wantCalls) {
		t.Errorf("CloseConnections calls = %+v, want %+v", calls, wantCalls)
	}
	if a.userRow(a.sam.ID) != nil || a.rawCount("sessions") != 1 {
		t.Error("the account or its sessions are still there")
	}
	_, err := a.svc.Authenticate(a.request(http.MethodGet, a.samIn.Token))
	wantCode(t, err, api.CodeUnauthenticated)
	wantCode(t, a.tryLogin("Sam", testPassword, ipB), api.CodeInvalidCredentials)
	wantCode(t, a.svc.DeleteUser(ctx, a.asAlex, a.sam.ID), api.CodeUserNotFound)
	// The name is free again.
	a.addUser("Sam")

	// Another admin can be deleted while one stays; a pending sign-up too; the CLI deletes like an admin.
	kim := a.addAdmin("Kim")
	pending := a.addUser("robin", func(u *store.User) { u.Status, u.CreatedVia = store.StatusPending, "signup" })
	if err := a.svc.DeleteUser(ctx, a.asAlex, kim.ID); err != nil {
		t.Fatalf("deleting another admin: %v", err)
	}
	if err := a.svc.DeleteUser(ctx, store.CLIActor, pending.ID); err != nil {
		t.Fatalf("deleting a pending sign-up from the CLI: %v", err)
	}
	a.wantAudit(auditUserDeleted, 3, store.CLIActor, pending, "map[self:false]")
	if alerts := a.alerts.take(); len(alerts) != 0 || a.hashes.Load() != 1 {
		t.Errorf("deleting alerted %+v or hashed %d times (want the one refused login)", alerts, a.hashes.Load())
	}
}

func TestSignOutUser(t *testing.T) {
	a := newAdminEnv(t)
	ctx := context.Background()
	second := a.login("Sam", ipC)
	a.cache(second)
	a.restart(func() { a.seedDevice(a.sam.ID, samDevice) })
	a.cache(a.alexIn, a.samIn, second)
	a.write(func(q *store.Q) error {
		return q.ReplacePasswordReset(store.PasswordReset{TokenHash: []byte("a-pending-link"), UserID: a.sam.ID,
			ExpiresAt: a.clk.now().Add(time.Hour)})
	})
	a.conns.take()

	_, _, err := a.svc.SignOutUser(ctx, a.asAlex, "nobody000000")
	wantCode(t, err, api.CodeUserNotFound)

	a.later(a.alexIn, a.samIn, second)
	sessions, devices, err := a.svc.SignOutUser(ctx, a.asAlex, a.sam.ID)
	if err != nil || sessions != 2 || devices != 1 {
		t.Fatalf("SignOutUser = %d sessions, %d devices, %v; want 2 and 1", sessions, devices, err)
	}
	a.wantAudit(auditUserSignedOut, 1, a.asAlex, a.sam, "map[devices:1 sessions:2]")
	wantCalls := []closedConn{{ConnSelector{UserID: a.sam.ID}, ReasonSessionRevoked}}
	if calls := a.conns.take(); fmt.Sprint(calls) != fmt.Sprint(wantCalls) {
		t.Errorf("CloseConnections calls = %+v, want %+v", calls, wantCalls)
	}
	for _, in := range []LoginResult{a.samIn, second} {
		_, err := a.svc.Authenticate(a.request(http.MethodGet, in.Token))
		wantCode(t, err, api.CodeUnauthenticated)
	}
	// The password and a pending reset link stay; so does everybody else's session.
	if u := a.userRow(a.sam.ID); u == nil || u.PasswordHash != a.sam.PasswordHash || u.Status != store.StatusActive ||
		a.rawInt("SELECT count(*) FROM password_resets") != 1 {
		t.Errorf("the account after the sign-out: %+v", u)
	}
	if p := a.who(a.alexIn); p.UserID != a.alex.ID {
		t.Errorf("the admin's session: %+v", p)
	}
	a.login("Sam", ipB)

	// Nothing left to end is still an answer, and a row.
	if sessions, devices, err = a.svc.SignOutUser(ctx, store.CLIActor, a.sam.ID); err != nil || sessions != 1 || devices != 0 {
		t.Fatalf("a second sign-out = %d, %d, %v; want the one new session", sessions, devices, err)
	}
	if sessions, devices, err = a.svc.SignOutUser(ctx, store.CLIActor, a.sam.ID); err != nil || sessions != 0 || devices != 0 {
		t.Fatalf("a third sign-out = %d, %d, %v; want nothing", sessions, devices, err)
	}
	a.wantAudit(auditUserSignedOut, 3, store.CLIActor, a.sam, "map[devices:0 sessions:0]")

	// An admin may sign the own account out everywhere.
	a.conns.take()
	if sessions, _, err = a.svc.SignOutUser(ctx, a.asAlex, a.alex.ID); err != nil || sessions != 1 {
		t.Fatalf("signing oneself out = %d, %v", sessions, err)
	}
	_, err = a.svc.Authenticate(a.request(http.MethodGet, a.alexIn.Token))
	wantCode(t, err, api.CodeUnauthenticated)
}

// adminCalls is every admin call of this slice on target, as the actor a.
func adminCalls(svc *Service, target store.UserID) map[string]func(a store.Actor) error {
	ctx := context.Background()
	return map[string]func(a store.Actor) error{
		"UpdateUser": func(a store.Actor) error {
			_, err := svc.UpdateUser(ctx, a, target, UserChange{Username: ptr("Renamed")})
			return err
		},
		"DeleteUser": func(a store.Actor) error { return svc.DeleteUser(ctx, a, target) },
		"SignOutUser": func(a store.Actor) error {
			_, _, err := svc.SignOutUser(ctx, a, target)
			return err
		},
		"IssuePasswordReset": func(a store.Actor) error {
			_, err := svc.IssuePasswordReset(ctx, a, target, testPassword)
			return err
		},
	}
}

// TestAdminCallsNeedAnAdmin: every admin call reads its actor in its own transaction. Anyone but an active admin
// and the CLI is forbidden, whatever the request's principal said a moment ago, and changes nothing.
func TestAdminCallsNeedAnAdmin(t *testing.T) {
	a := newAdminEnv(t)
	kim := a.addAdmin("Kim")
	disabled := a.addAdmin("Robin")
	gone := a.addAdmin("Gone")
	a.write(func(q *store.Q) error {
		if err := q.SetStatus(disabled.ID, store.StatusDisabled, a.clk.now()); err != nil {
			return err
		}
		return q.DeleteUser(gone.ID)
	})
	before := a.rawCount("audit_log")
	for name, actor := range map[string]store.Actor{
		"a member":         actorFor(a.sam, ipB),
		"a disabled admin": actorFor(disabled, ipB),
		"a deleted admin":  actorFor(gone, ipB),
		"nobody":           {Kind: store.ActorAnonymous, IP: ipB},
		"the system":       store.SystemActor,
		"no actor":         {},
	} {
		for call, run := range adminCalls(a.svc, kim.ID) {
			wantCode(t, run(actor), api.CodeForbidden)
			// Also for a target that does not exist: the actor comes first.
			if err := adminCalls(a.svc, "nobody000000")[call](actor); !api.IsCode(err, api.CodeForbidden) {
				t.Errorf("%s as %s for an unknown user: %v, want forbidden", call, name, err)
			}
		}
	}
	if u := a.userRow(kim.ID); u == nil || u.Username != "Kim" || u.PasswordHash != kim.PasswordHash ||
		a.rawCount("audit_log") != before || len(a.conns.take()) != 0 || a.hashes.Load() != 0 {
		t.Errorf("a forbidden call changed something: %+v", u)
	}
}

func TestUserByUsername(t *testing.T) {
	a := newAdminEnv(t)
	ctx := context.Background()
	for _, name := range []string{"Alex", "alex", " ALEX ", "Ａｌｅｘ"} {
		u, err := a.svc.UserByUsername(ctx, name)
		if err != nil || u.ID != a.alex.ID || u.Username != "Alex" {
			t.Errorf("UserByUsername(%q) = %+v, %v", name, u, err)
		}
	}
	// A name nobody has, and names no account can have: the same answer.
	for _, name := range []string{"nobody", "", ".", "sam smith", "system", "\x00"} {
		_, err := a.svc.UserByUsername(ctx, name)
		wantCode(t, err, api.CodeUserNotFound)
	}
}

// TestAdminInternalErrors: on a closed store every admin call answers internal, never a verdict, and closes
// nothing.
func TestAdminInternalErrors(t *testing.T) {
	a := newAdminEnv(t)
	link, err := a.svc.IssuePasswordReset(context.Background(), a.asAlex, a.sam.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimPrefix(link.URL, testOrigin+"/reset#")
	a.conns.take()
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for call, run := range adminCalls(a.svc, a.sam.ID) {
		for _, actor := range []store.Actor{a.asAlex, store.CLIActor} {
			if err := run(actor); !api.IsCode(err, api.CodeInternal) {
				t.Errorf("%s on a closed store: %v", call, err)
			}
		}
	}
	if _, err := a.svc.UserByUsername(ctx, "Sam"); !api.IsCode(err, api.CodeInternal) {
		t.Errorf("UserByUsername on a closed store: %v", err)
	}
	if _, err := a.svc.CheckPasswordReset(ctx, token, meta(ipB)); !api.IsCode(err, api.CodeInternal) {
		t.Errorf("CheckPasswordReset on a closed store: %v", err)
	}
	if _, err := a.svc.CompletePasswordReset(ctx, token, newPassword, meta(ipB)); !api.IsCode(err, api.CodeInternal) {
		t.Errorf("CompletePasswordReset on a closed store: %v", err)
	}
	if calls := a.conns.take(); len(calls) != 0 {
		t.Errorf("failed calls closed connections: %+v", calls)
	}
}
