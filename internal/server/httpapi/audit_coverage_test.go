package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/auth"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// The audit coverage test of 03 §15 (README S70): every action of 03 §10 that M1 can trigger is produced by a real
// request (or by the CLI path, the janitor or a key rotation), shows up in GET /api/v1/admin/audit with the actor and
// target kinds of §10's table and, for a request, with the IP that ClientIP gave. The actions §10 marks "later" are
// listed with the reason they have no code yet, and nothing fakes them.

// auditAction is a row of the action table of 03 §10.
type auditAction struct {
	action string
	// actors are the actor kinds of §10's Actor column that the merged tree writes, each of which TestAuditCoverage
	// produces: user (an admin or the user), anonymous, cli (the admin socket's commands, store.CLIActor) or system.
	actors []store.ActorKind
	// targets are the target kinds of §10's Target column, "" for a row without a target, each of which
	// TestAuditCoverage produces for the actions M1 writes. Two actions have a variant without one: a failed login
	// names the account only if it exists, and reject all has none.
	targets []string
	sec     bool // a security event (§10's Sec column); settings.changed only when registrationMode changed
	// later is why M1 has no code that writes the action (§10 marks it later); "" for the actions M1 writes.
	later string
}

// Actor kinds of the table.
var (
	byUser      = []store.ActorKind{store.ActorUser}
	byUserOrCLI = []store.ActorKind{store.ActorUser, store.ActorCLI}
	byAnonymous = []store.ActorKind{store.ActorAnonymous}
	byCLI       = []store.ActorKind{store.ActorCLI}
	bySystem    = []store.ActorKind{store.ActorSystem}
)

// auditActions is the table of 03 §10, in its order.
var auditActions = []auditAction{
	{action: "setup.token_issued", actors: byCLI, targets: []string{""}},
	{action: "setup.completed", actors: byUser, targets: []string{"user"}, sec: true},
	{action: "auth.login", actors: byUser, targets: []string{"session"}},
	{action: "auth.login_failed", actors: byAnonymous, targets: []string{"user", ""}},
	{action: "auth.throttled", actors: byAnonymous, targets: []string{""}, sec: true},
	{action: "auth.logout", actors: byUser, targets: []string{"session"}},
	{action: "auth.logout_everywhere", actors: byUser, targets: []string{"user"}},
	{action: "auth.password_changed", actors: byUser, targets: []string{"user"}},
	{action: "session.revoked", actors: byUser, targets: []string{"session"}},
	{action: "user.registered", actors: byUser, targets: []string{"user"}},
	{action: "user.signup_requested", actors: byAnonymous, targets: []string{"user"}},
	{action: "user.approved", actors: byUser, targets: []string{"user"}},
	{action: "user.signup_rejected", actors: byUser, targets: []string{"user", ""}},
	{action: "user.signup_expired", actors: bySystem, targets: []string{"user"}},
	// §10 also names the CLI as the actor of a rename and a settings change; M1's admin socket has neither command
	// (03 §12.6), so only the admin pages write them.
	{action: "user.renamed", actors: byUser, targets: []string{"user"}},
	{action: "user.role_changed", actors: byUserOrCLI, targets: []string{"user"}, sec: true},
	{action: "user.disabled", actors: byUserOrCLI, targets: []string{"user"}, sec: true},
	{action: "user.enabled", actors: byUserOrCLI, targets: []string{"user"}},
	{action: "user.deleted", actors: byUser, targets: []string{"user"}, sec: true},
	{action: "user.signed_out", actors: byUser, targets: []string{"user"}},
	{action: "user.password_reset_issued", actors: byUserOrCLI, targets: []string{"user"}, sec: true},
	{action: "user.password_reset_completed", actors: byUser, targets: []string{"user"}},
	{action: "invite.created", actors: byUserOrCLI, targets: []string{"invite"}},
	{action: "invite.revoked", actors: byUser, targets: []string{"invite"}},
	{action: "room.created", actors: byUser, targets: []string{"room"}},
	{action: "room.renamed", actors: byUser, targets: []string{"room"}},
	{action: "room.deleted", actors: byUser, targets: []string{"room"}},
	{action: "settings.changed", actors: byUser, targets: []string{"settings"}, sec: true},
	{action: "secrets.rotated", actors: bySystem, targets: []string{""}, sec: true},
	{action: "device.linked", targets: []string{"device"},
		later: "M2: no M1 code links a device; the device flow is 03 slice 15"},
	// §10 marks it later, but S58's DELETE /api/v1/me/devices/{id} writes it for any device row. No M1 path links
	// a device, so TestAuditCoverage inserts one row, as M2's linking would, and revokes it over REST.
	{action: "device.revoked", actors: byUser, targets: []string{"device"}},
	{action: "device.refresh_reused", targets: []string{"device"}, sec: true,
		later: "M2: no refresh tokens exist before the device flow (03 slice 15)"},
}

// auditActionOf returns the table row of an action.
func auditActionOf(action string) (auditAction, bool) {
	i := slices.IndexFunc(auditActions, func(a auditAction) bool { return a.action == action })
	if i < 0 {
		return auditAction{}, false
	}
	return auditActions[i], true
}

// isSecurityEvent is §10's Sec column for one row.
func isSecurityEvent(action string, detail map[string]any) bool {
	a, ok := auditActionOf(action)
	if !ok || !a.sec {
		return false
	}
	if action == "settings.changed" {
		changes, _ := detail["changes"].(map[string]any)
		_, mode := changes["registrationMode"]
		return mode
	}
	return true
}

// behindProxy is the ClientIP of TestAuditCoverage, for both the API and auth.Service, as the wiring passes one
// ClientIP to both (04 §6.6): for the request's peer a.b.c.d it answers an address in 2001:db8::/32 made of those
// four bytes, as a trusted proxy's header might. A row that shows the peer's own address did not take its IP from
// ClientIP. Each peer gets a /64 of its own, so the per-IP throttles stay apart.
func behindProxy(r *http.Request) netip.Addr {
	return proxied(ClientIP(r))
}

func proxied(a netip.Addr) netip.Addr {
	if !a.Is4() {
		return a
	}
	v := a.As4()
	return netip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, v[2], v[3], 0, 0, 0, 0, 0, 0, v[0], v[1]})
}

// proxiedText is the IP a row must show for a request from the IPv4 address ip.
func proxiedText(ip string) string { return proxied(netip.MustParseAddr(ip)).String() }

// targetKind is the target kind of an audit entry, "" when it has none.
func targetKind(e api.AuditEntry) string {
	if e.Target == nil {
		return ""
	}
	return e.Target.Kind
}

// rowShape tells apart the variants of one action: the entry's target kind ("none" without a target) and its detail.
func rowShape(e api.AuditEntry) string {
	kind := targetKind(e)
	if kind == "" {
		kind = "none"
	}
	return kind + " " + fmt.Sprint(e.Detail)
}

// runJanitor runs the service's janitor until done reports true (its first run, at startup, is due at once), then
// stops it.
func (f *authFixture) runJanitor(svc *auth.Service, done func() bool) {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		svc.RunJanitor(ctx)
	}()
	defer func() {
		cancel()
		<-stopped
	}()
	for deadline := time.Now().Add(20 * time.Second); !done(); {
		if time.Now().After(deadline) {
			f.t.Fatal("the janitor's run at startup did not happen within 20 s")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// auditPages reads the whole audit log through GET /api/v1/admin/audit, limit rows at a time, following nextBefore,
// and returns the entries newest first.
func (f *authFixture) auditPages(c *http.Cookie, limit int) []api.AuditEntry {
	f.t.Helper()
	var out []api.AuditEntry
	query := "?limit=" + strconv.Itoa(limit)
	for range 1000 {
		page := decodeBody[api.AuditPage](f.t, f.call(http.MethodGet, "/api/v1/admin/audit"+query, "", with(c)),
			http.StatusOK)
		if len(page.Entries) > limit || len(page.Entries) == 0 && page.NextBefore != nil {
			f.t.Fatalf("a page of %d entries with nextBefore %v for limit %d", len(page.Entries), page.NextBefore, limit)
		}
		out = append(out, page.Entries...)
		if page.NextBefore == nil {
			return out
		}
		if last := page.Entries[len(page.Entries)-1].ID; *page.NextBefore != last {
			f.t.Fatalf("nextBefore = %d, want the id of the page's last entry, %d", *page.NextBefore, last)
		}
		query = "?limit=" + strconv.Itoa(limit) + "&before=" + strconv.FormatInt(*page.NextBefore, 10)
	}
	f.t.Fatal("the audit log did not end after 1000 pages")
	return nil
}

// TestAuditCoverage is 03 §15's audit case: every action of 03 §10 that M1 can trigger is produced at least once,
// each row has the actor and target kinds of §10, a request's row has the IP from ClientIP and the CLI's and the
// server's own rows have none, GET /api/v1/admin/audit pages through all of them, and the dashboard's security events
// are exactly the rows of §10's Sec column.
func TestAuditCoverage(t *testing.T) {
	f := newAuthFixtureDeps(t, func(d *Deps) { d.ClientIP = behindProxy },
		func(o *auth.Options) { o.ClientIP = behindProxy })
	ctx := context.Background()

	// What the scenario produces: a request's row, with the IP ClientIP gives for the address it came from, or a row
	// of the CLI or the server itself, without an IP.
	type want struct {
		action string
		actor  store.ActorKind // ActorCLI or ActorSystem; "" for a request's row (a user or anonymous)
		ip     string
		shape  string // the row's rowShape, for an action with variants; "" = not checked
	}
	var wants []want
	expect := func(action, from string) { wants = append(wants, want{action: action, ip: proxiedText(from)}) }
	expectShaped := func(action, from, shape string) {
		wants = append(wants, want{action: action, ip: proxiedText(from), shape: shape})
	}
	expectBy := func(action string, actor store.ActorKind) { wants = append(wants, want{action: action, actor: actor}) }
	ok := func(rec *httptest.ResponseRecorder, status int) {
		t.Helper()
		if rec.Code != status {
			t.Fatalf("status %d, want %d (body %q)", rec.Code, status, rec.Body)
		}
	}
	ip := clientIP

	// Setup: the CLI issues the link, the wizard completes it.
	token := f.setupToken()
	expectBy("setup.token_issued", store.ActorCLI)
	rec := f.call(http.MethodPost, "/api/v1/auth/setup/complete", jsonBody(t, api.SetupCompleteRequest{
		Token: token, Username: "Alex", Password: testPassword}), from(ip(1)))
	decodeBody[api.UserResponse](t, rec, http.StatusCreated)
	admin := f.sessionCookie(rec, thirtyDays)
	expect("setup.completed", ip(1))

	// Logins.
	decodeBody[api.UserResponse](t, f.login("Alex", testPassword, ip(2)), http.StatusOK)
	expect("auth.login", ip(2))
	// A failed login names the account only if it exists, and never the name tried for an unknown one (03 §10).
	wantError(t, f.login("Alex", "not the password", ip(3)), http.StatusUnauthorized, api.CodeInvalidCredentials)
	expectShaped("auth.login_failed", ip(3), "user map[reason:wrong_password]")
	wantError(t, f.login("Nobody", "not the password", ip(34)), http.StatusUnauthorized, api.CodeInvalidCredentials)
	expectShaped("auth.login_failed", ip(34), "none map[reason:unknown_user]")

	// Invites and a registration; a second invite is revoked.
	newInvite := func(addr string) api.CreateInviteResponse {
		t.Helper()
		res := decodeBody[api.CreateInviteResponse](t, f.call(http.MethodPost, "/api/v1/invites", `{}`, with(admin),
			from(addr)), http.StatusCreated)
		expect("invite.created", addr)
		return res
	}
	invite := newInvite(ip(4))
	inviteToken, _ := strings.CutPrefix(invite.URL, testOrigin+"/invite#")
	sam, _ := f.join(inviteToken, "Sam", ip(5)) // the reset below ends this session
	expect("user.registered", ip(5))
	second := newInvite(ip(4))
	wantNoContent(t, f.call(http.MethodDelete, "/api/v1/invites/"+second.Invite.ID, "", with(admin), from(ip(6))))
	expect("invite.revoked", ip(6))

	// A throttle: sign-ups without an invite from one address, until register-ip is empty (03 §7.3).
	for range 5 {
		wantError(t, f.register("", "Nobody", ip(7)), http.StatusForbidden, api.CodeInviteRequired)
	}
	wantError(t, f.register("", "Nobody", ip(7)), http.StatusTooManyRequests, api.CodeRateLimited)
	expect("auth.throttled", ip(7))

	// Settings: the approval mode, then three sign-ups. Kim is approved, Lee rejected, Max left to expire.
	ok(f.call(http.MethodPatch, "/api/v1/admin/settings", `{"registrationMode":"approval"}`, with(admin), from(ip(8))),
		http.StatusOK)
	expect("settings.changed", ip(8))
	for i, name := range []string{"Kim", "Lee", "Max"} {
		f.signup(name, ip(9+i))
		expect("user.signup_requested", ip(9+i))
	}
	pendingID := map[string]string{}
	for _, p := range f.pending(admin) {
		pendingID[p.Username] = p.ID
	}
	kim := pendingID["Kim"]
	ok(f.call(http.MethodPost, "/api/v1/admin/approvals/"+kim+"/approve", `{}`, with(admin), from(ip(12))),
		http.StatusOK)
	expect("user.approved", ip(12))
	wantNoContent(t, f.call(http.MethodPost, "/api/v1/admin/approvals/"+pendingID["Lee"]+"/reject", `{}`,
		with(admin), from(ip(13))))
	expectShaped("user.signup_rejected", ip(13), "user map[]")

	// The admin's user changes.
	ok(f.patchUser(admin, sam.ID, `{"username":"Samuel"}`, from(ip(14))), http.StatusOK)
	expect("user.renamed", ip(14))
	ok(f.patchUser(admin, kim, `{"role":"admin","currentPassword":"`+testPassword+`"}`, from(ip(15))), http.StatusOK)
	expect("user.role_changed", ip(15))
	ok(f.patchUser(admin, kim, `{"status":"disabled"}`, from(ip(16))), http.StatusOK)
	expect("user.disabled", ip(16))
	ok(f.patchUser(admin, kim, `{"status":"active"}`, from(ip(17))), http.StatusOK)
	expect("user.enabled", ip(17))

	// A reset link for Sam, completed; then Sam's own changes.
	rec = f.issueReset(admin, sam.ID, `{}`, from(ip(18)))
	link := decodeBody[api.ResetLink](t, rec, http.StatusCreated)
	expect("user.password_reset_issued", ip(18))
	resetToken, _ := strings.CutPrefix(link.URL, testOrigin+"/reset#")
	rec = f.resetComplete(resetToken, "a fresh passphrase", ip(19))
	decodeBody[api.UserResponse](t, rec, http.StatusOK)
	samCookie := f.sessionCookie(rec, thirtyDays)
	expect("user.password_reset_completed", ip(19))
	rec = f.changePassword(samCookie, "a fresh passphrase", "another passphrase", from(ip(20)))
	ok(rec, http.StatusOK)
	samCookie = f.sessionCookie(rec, thirtyDays)
	expect("auth.password_changed", ip(20))
	other := f.sessionCookie(f.login("Samuel", "another passphrase", ip(21)), thirtyDays)
	expect("auth.login", ip(21))
	wantNoContent(t, f.call(http.MethodDelete, "/api/v1/me/sessions/"+string(f.sessionID(other)), "",
		with(samCookie), from(ip(22))))
	expect("session.revoked", ip(22))
	f.linkDevice(sam.ID, "d1d1d1d1d1d1") // what M2's device flow would have done
	wantNoContent(t, f.call(http.MethodDelete, "/api/v1/me/devices/d1d1d1d1d1d1", "", with(samCookie), from(ip(23))))
	expect("device.revoked", ip(23))
	wantNoContent(t, f.call(http.MethodPost, "/api/v1/auth/logout-everywhere", `{}`, with(samCookie), from(ip(24))))
	expect("auth.logout_everywhere", ip(24))
	last := f.sessionCookie(f.login("Samuel", "another passphrase", ip(25)), thirtyDays)
	expect("auth.login", ip(25))
	wantNoContent(t, f.call(http.MethodPost, "/api/v1/auth/logout", `{}`, with(last), from(ip(26))))
	expect("auth.logout", ip(26))

	// Kim signs in and the admin signs her out; Sam's account is deleted.
	f.sessionCookie(f.login("Kim", testPassword, ip(27)), thirtyDays)
	expect("auth.login", ip(27))
	ok(f.call(http.MethodPost, "/api/v1/admin/users/"+kim+"/sign-out", `{}`, with(admin), from(ip(28))), http.StatusOK)
	expect("user.signed_out", ip(28))

	// The admin socket's commands act as the CLI, through the same calls (03 §12.6; the wiring's adminAccounts):
	// users set-role, disable, enable and reset-password, and invite create.
	cli := store.CLIActor
	kimRow, err := f.svc.UserByUsername(ctx, "Kim")
	if err != nil {
		t.Fatal(err)
	}
	member, disabled, active := store.RoleUser, store.StatusDisabled, store.StatusActive
	for _, ch := range []auth.UserChange{{Role: &member}, {Status: &disabled}, {Status: &active}} {
		if _, err := f.svc.UpdateUser(ctx, cli, kimRow.ID, ch); err != nil {
			t.Fatal(err)
		}
	}
	expectBy("user.role_changed", store.ActorCLI)
	expectBy("user.disabled", store.ActorCLI)
	expectBy("user.enabled", store.ActorCLI)
	if _, err := f.svc.IssuePasswordReset(ctx, cli, kimRow.ID, ""); err != nil {
		t.Fatal(err)
	}
	expectBy("user.password_reset_issued", store.ActorCLI)
	_, cliInvite, err := f.svc.CreateInvite(ctx, cli, auth.InviteInput{MaxUses: 1, ExpiresInHours: 24})
	if err != nil {
		t.Fatal(err)
	}
	expectBy("invite.created", store.ActorCLI)

	// The admin deletes Sam's account; Ivy joins with the CLI's invite and deletes her own (03 §7.7).
	wantNoContent(t, f.call(http.MethodDelete, "/api/v1/admin/users/"+sam.ID, "", with(admin), from(ip(29))))
	expectShaped("user.deleted", ip(29), "user map[self:false]")
	cliToken, _ := strings.CutPrefix(cliInvite.URL, testOrigin+"/invite#")
	_, ivy := f.join(cliToken, "Ivy", ip(35))
	expect("user.registered", ip(35))
	wantNoContent(t, f.call(http.MethodPost, "/api/v1/me/delete", jsonBody(t, api.DeleteSelfRequest{
		Password: testPassword}), with(ivy), from(ip(36))))
	expectShaped("user.deleted", ip(36), "user map[self:true]")

	// Rooms.
	room := decodeBody[api.RoomResponse](t, f.call(http.MethodPost, "/api/v1/admin/rooms", `{"name":"Games"}`,
		with(admin), from(ip(30))), http.StatusCreated).Room
	expect("room.created", ip(30))
	ok(f.call(http.MethodPatch, "/api/v1/admin/rooms/"+room.ID, `{"name":"Movies"}`, with(admin), from(ip(31))),
		http.StatusOK)
	expect("room.renamed", ip(31))
	wantNoContent(t, f.call(http.MethodDelete, "/api/v1/admin/rooms/"+room.ID, "", with(admin), from(ip(32))))
	expect("room.deleted", ip(32))

	// The dashboard's security events (03 §12.4.8) are §10's Sec rows, newest first, all of them so far.
	wantSecurity := func(when string) {
		t.Helper()
		acc, err := f.api.DashboardAccounts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		since := f.clk.now().Add(-7 * 24 * time.Hour)
		var want, got []string
		rows := f.audit()
		for i := len(rows) - 1; i >= 0 && len(want) < 20; i-- {
			if e := rows[i]; !e.At.Before(since) && isSecurityEvent(e.Action, e.Detail) {
				want = append(want, e.Action+"#"+strconv.FormatInt(e.ID, 10))
			}
		}
		for _, e := range acc.SecurityEvents {
			got = append(got, e.Action+"#"+strconv.FormatInt(e.ID, 10))
		}
		if !slices.Equal(got, want) || len(want) == 0 {
			t.Errorf("%s: the dashboard's security events are %v, want %v (03 §10, column Sec)", when, got, want)
		}
	}
	wantSecurity("after the requests")

	// The janitor, two weeks later: Max's sign-up expires.
	f.clk.advance(14*24*time.Hour + time.Millisecond)
	f.runJanitor(f.svc, func() bool { return len(f.auditOf("user.signup_expired")) > 0 })
	expectBy("user.signup_expired", store.ActorSystem)

	// A restart with a new invite key (`isshoni admin rotate-secrets`, 04 §5.3): auth.New finds the change.
	o := testAuthOptions(f.clk)
	o.Keys.Invite = bytes.Repeat([]byte{0x33}, 32)
	if _, err := auth.New(ctx, f.db, o); err != nil {
		t.Fatal(err)
	}
	expectBy("secrets.rotated", store.ActorSystem)
	wantSecurity("after the rotation, two weeks on") // the rotation alone: the others are older than 7 days

	// The admin signs in again and finds one new sign-up (the approval mode is still on, and Max's has expired):
	// reject all (03 §7.9), whose row names no user (03 §10).
	reader := f.sessionCookie(f.login("Alex", testPassword, ip(33)), thirtyDays)
	expect("auth.login", ip(33))
	f.signup("Noa", ip(37))
	expect("user.signup_requested", ip(37))
	if res := decodeBody[api.RejectAllResponse](t, f.call(http.MethodPost, "/api/v1/admin/approvals/all/reject",
		`{"all":true}`, with(reader), from(ip(38))), http.StatusOK); res.Rejected != 1 {
		t.Fatalf("reject all rejected %d sign-ups, want Noa's alone", res.Rejected)
	}
	expectShaped("user.signup_rejected", ip(38), "none map[all:true count:1]")

	// Read the whole log over REST, 7 rows a page, and compare it with the store's.
	entries := f.auditPages(reader, 7)
	stored := f.audit()
	if len(entries) != len(stored) || len(stored) >= 200 {
		t.Fatalf("GET /admin/audit paged out %d entries, the store holds %d", len(entries), len(stored))
	}
	for i, e := range entries {
		if s := stored[len(stored)-1-i]; e.ID != s.ID || e.Action != s.Action || e.IP != s.Actor.IP {
			t.Fatalf("entry %d is %d %s, the store's is %d %s", i, e.ID, e.Action, s.ID, s.Action)
		}
	}

	// Every row: an action of §10 with its actor and target kinds, and an IP exactly when a request made it.
	rows := map[string][]api.AuditEntry{}
	for _, e := range entries {
		a, known := auditActionOf(e.Action)
		if !known {
			t.Errorf("row %d has the action %q, which 03 §10 does not list", e.ID, e.Action)
			continue
		}
		rows[e.Action] = append(rows[e.Action], e)
		if target := targetKind(e); !slices.Contains(a.actors, store.ActorKind(e.Actor.Kind)) ||
			!slices.Contains(a.targets, target) {
			t.Errorf("row %d %s: actor %q, target %q; want one of %q and one of %q (03 §10)", e.ID, e.Action,
				e.Actor.Kind, target, a.actors, a.targets)
		}
		// The name tried for an unknown account is never stored (03 §10).
		if e.Action == "auth.login_failed" && strings.Contains(fmt.Sprint(e), "Nobody") {
			t.Errorf("row %d %s holds the name tried for an unknown account: %+v", e.ID, e.Action, e)
		}
		fromRequest := e.Actor.Kind == string(store.ActorUser) || e.Actor.Kind == string(store.ActorAnonymous)
		if fromRequest != (e.IP != "") || e.IP != "" && !strings.HasPrefix(e.IP, "2001:db8:") {
			t.Errorf("row %d %s by %s has the IP %q; want the one ClientIP gave for a request, none otherwise",
				e.ID, e.Action, e.Actor.Kind, e.IP)
		}
	}
	for _, w := range wants {
		if !slices.ContainsFunc(rows[w.action], func(e api.AuditEntry) bool {
			return e.IP == w.ip && (w.actor == "" || e.Actor.Kind == string(w.actor)) &&
				(w.shape == "" || rowShape(e) == w.shape)
		}) {
			t.Errorf("no %s row with the IP %q (actor %q, shape %q)", w.action, w.ip, w.actor, w.shape)
		}
	}
	for _, a := range auditActions {
		produced := slices.ContainsFunc(wants, func(w want) bool { return w.action == a.action })
		switch {
		case a.later != "" && (produced || len(rows[a.action]) > 0):
			t.Errorf("%s is marked later, but the scenario has %d rows of it", a.action, len(rows[a.action]))
		case a.later != "":
			t.Logf("03 §10 %s: no code yet (%s); not produced", a.action, a.later)
		case !produced:
			t.Errorf("03 §10 %s: M1 writes it, but the scenario does not produce it", a.action)
		}
		for _, kind := range a.actors {
			if !slices.ContainsFunc(rows[a.action], func(e api.AuditEntry) bool { return e.Actor.Kind == string(kind) }) {
				t.Errorf("03 §10 %s: no row by %s, which the merged tree writes", a.action, kind)
			}
		}
		for _, kind := range a.targets {
			if a.later == "" && !slices.ContainsFunc(rows[a.action], func(e api.AuditEntry) bool {
				return targetKind(e) == kind
			}) {
				t.Errorf("03 §10 %s: no row with the target %q, which the merged tree writes", a.action, kind)
			}
		}
	}
}

// auditActionShape matches a string literal that has the shape of an audit action of 03 §10.
var auditActionShape = regexp.MustCompile(`^(setup|auth|session|user|invite|room|settings|secrets|device)\.[a-z_]+$`)

// notAuditActions are literals of that shape that name something else.
var notAuditActions = map[string]string{
	"setup.wizard_done": "the DB key of the setupWizardDone setting (store/settings.go)",
}

// actionLiterals returns the literals of the non-test Go files of dir that have the shape of an audit action, each
// with where it is.
func actionLiterals(t *testing.T, dir string) map[string][]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no Go files in %s (%v)", dir, err)
	}
	fset := token.NewFileSet()
	out := map[string][]string{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if s, err := strconv.Unquote(lit.Value); err == nil && auditActionShape.MatchString(s) {
				out[s] = append(out[s], fset.Position(lit.Pos()).String())
			}
			return true
		})
	}
	return out
}

// TestAuditActionsInSource keeps the table of TestAuditCoverage in step with the code: every audit action that the
// packages writing audit rows (auth, httpapi, store) name is a row of 03 §10's table, and an action marked later has
// no code in auth or httpapi yet. When a later slice adds one, this test asks for it in TestAuditCoverage.
func TestAuditActionsInSource(t *testing.T) {
	writers := map[string]map[string][]string{}
	for _, dir := range []string{".", "../auth", "../store"} {
		writers[dir] = actionLiterals(t, dir)
		for lit, where := range writers[dir] {
			if _, ok := notAuditActions[lit]; ok {
				continue
			}
			if _, ok := auditActionOf(lit); !ok {
				t.Errorf("%s names the audit action %q, which is not in 03 §10's table (auditActions)", where[0], lit)
			}
		}
	}
	for _, a := range auditActions {
		if a.later == "" {
			if len(writers["."][a.action])+len(writers["../auth"][a.action])+len(writers["../store"][a.action]) == 0 {
				t.Errorf("03 §10 %s: no code names it, but the table says M1 writes it", a.action)
			}
			continue
		}
		for _, dir := range []string{".", "../auth"} {
			if where := writers[dir][a.action]; len(where) > 0 {
				t.Errorf("%s names %s, which the table marks later (%s): produce it in TestAuditCoverage", where[0],
					a.action, a.later)
			}
		}
	}
	if len(auditActions) != 32 {
		t.Errorf("the table has %d actions; 03 §10 lists 32", len(auditActions))
	}
}
