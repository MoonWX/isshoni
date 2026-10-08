package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/auth"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// TestInviteModeOverREST is 03 §15's invite-mode case: create an invite (the URL has a fragment and the DB holds no
// plaintext token); check; register 10 users; the 11th → 410 invite_used_up; revoked → 410 invite_revoked; expired
// (clock) → 410 invite_expired; register without an invite → 403 invite_required; a taken username → 409 without
// using the invite.
func TestInviteModeOverREST(t *testing.T) {
	f := newAuthFixture(t)
	admin, cookie := f.setupAdmin("Alex")
	count := f.counter()
	f.signal.takeNotes()

	// Create: 201 {invite, url}, in the shape of 03 §12.4.5.
	rec := f.call(http.MethodPost, "/api/v1/invites", `{"note":"for Sam","expiresInHours":168,"maxUses":10}`, with(cookie))
	res := decodeBody[api.CreateInviteResponse](t, rec, http.StatusCreated)
	inv := res.Invite
	if len(inv.ID) != 12 || inv.Note != "for Sam" || inv.MaxUses != 10 || inv.Uses != 0 || inv.State != api.InviteStateActive ||
		inv.CreatedBy == nil || *inv.CreatedBy != (api.UserRef{ID: admin.ID, Username: "Alex"}) ||
		!inv.CreatedAt.Equal(f.clk.now()) || !inv.ExpiresAt.Equal(f.clk.now().Add(7*24*time.Hour)) {
		t.Errorf("created invite = %+v", inv)
	}
	if got, want := fmt.Sprint(jsonKeys(t, rec.Body.Bytes())), "[invite url]"; got != want {
		t.Errorf("body keys = %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(jsonKeys(t, rec.Body.Bytes(), "invite")),
		"[createdAt createdBy expiresAt id maxUses note redeemedBy state uses]"; got != want {
		t.Errorf("invite keys = %s, want %s", got, want)
	}
	if !strings.Contains(rec.Body.String(), `"redeemedBy":[]`) || !strings.Contains(rec.Body.String(), `"expiresAt":"2026-10-08T12:00:00.000Z"`) {
		t.Errorf("body %s: want an empty redeemedBy list and the fixed timestamp form", rec.Body)
	}
	token, ok := strings.CutPrefix(res.URL, testOrigin+"/invite#")
	if !ok || len(token) != 32 {
		t.Fatalf("url = %q, want %s/invite#<32-character token>", res.URL, testOrigin)
	}
	// The token is in the fragment only, and nowhere in the database.
	if n := count("SELECT count(*) FROM invites WHERE instr(token_hash, ?) > 0 OR instr(note, ?) > 0", []byte(token), token); n != 0 {
		t.Error("the invites table holds the plaintext token")
	}
	if n := count("SELECT count(*) FROM audit_log WHERE instr(detail, ?) > 0 OR instr(target_id, ?) > 0", token, token); n != 0 {
		t.Error("the audit log holds the plaintext token")
	}
	if n := count("SELECT count(*) FROM invites WHERE length(token_hash) = 32"); n != 1 {
		t.Errorf("%d invites with a 32-byte token hash, want 1", n)
	}
	if notes := f.signal.takeNotes(); fmt.Sprint(notes) != "[admins [admin.invites]]" {
		t.Errorf("Signal after an admin created an invite = %v", notes)
	}

	// Check: what the invite page shows.
	rec = f.checkInvite(token, clientIP(0))
	info := decodeBody[api.InviteInfo](t, rec, http.StatusOK)
	if info.ServerName != testHost || info.InvitedBy != "Alex" || info.UsesLeft != 10 || !info.ExpiresAt.Equal(inv.ExpiresAt) {
		t.Errorf("invite/check = %+v", info)
	}
	if got, want := rec.Body.String(),
		`{"serverName":"watch.example.com","invitedBy":"Alex","usesLeft":10,"expiresAt":"2026-10-08T12:00:00.000Z"}`+"\n"; got != want {
		t.Errorf("invite/check body = %s, want %s", got, want)
	}
	wantError(t, f.checkInvite(strings.Repeat("A", 32), clientIP(0)), http.StatusNotFound, api.CodeInviteInvalid)
	wantError(t, f.checkInvite("", clientIP(0)), http.StatusNotFound, api.CodeInviteInvalid)
	wantError(t, f.call(http.MethodPost, "/api/v1/auth/invite/check", `{}`, from(clientIP(0))), http.StatusNotFound, api.CodeInviteInvalid)

	// Register without an invite → 403 invite_required. Field errors come as codes per field.
	wantError(t, f.register("", "Sam", clientIP(1)), http.StatusForbidden, api.CodeInviteRequired)
	e := wantError(t, f.call(http.MethodPost, "/api/v1/auth/register",
		`{"inviteToken":"`+token+`","username":"a","password":"1234567"}`, from(clientIP(1))),
		http.StatusUnprocessableEntity, api.CodeValidationFailed)
	if fmt.Sprint(e.Fields) != "map[password:too_short username:too_short]" {
		t.Errorf("fields = %v", e.Fields)
	}
	wantError(t, f.register(strings.Repeat("A", 32), "Sam", clientIP(1)), http.StatusNotFound, api.CodeInviteInvalid)
	// A taken username → 409, without using the invite.
	rec = f.register(token, "ALEX", clientIP(1))
	wantError(t, rec, http.StatusConflict, api.CodeUsernameTaken)
	if len(setCookies(rec)) != 0 {
		t.Error("a refused registration set a cookie")
	}
	if got := f.invites(cookie, ""); len(got) != 1 || got[0].Uses != 0 {
		t.Fatalf("invites after refused registrations = %+v, want the invite unused", got)
	}
	f.signal.takeNotes()

	// Register 10 users: each is 201 {status: "active", user} with a session cookie, and lands logged in.
	var friends []api.User
	for i := range 10 {
		name := fmt.Sprintf("friend%d", i)
		rec := f.register(token, name, clientIP(10+i))
		reg := decodeBody[api.RegisterResponse](t, rec, http.StatusCreated)
		c := f.sessionCookie(rec, thirtyDays)
		if reg.Status != api.UserStatusActive || reg.User == nil || reg.User.Username != name || reg.User.Role != api.RoleUser ||
			len(reg.User.ID) != 12 {
			t.Fatalf("registration %d = %+v", i+1, reg)
		}
		if i == 0 {
			if got, want := rec.Body.String(),
				`{"status":"active","user":{"id":"`+reg.User.ID+`","username":"friend0","role":"user"}}`+"\n"; got != want {
				t.Errorf("register body = %s, want %s", got, want)
			}
			// The new user's tabs and every admin hear about it (03 §12.5).
			want := "[user " + reg.User.ID + " [devices] admins [admin.invites admin.users]]"
			if notes := f.signal.takeNotes(); fmt.Sprint(notes) != want {
				t.Errorf("Signal after a registration = %v, want %s", notes, want)
			}
			me := decodeBody[api.Me](t, f.call(http.MethodGet, "/api/v1/me", "", with(c)), http.StatusOK)
			if me.User.ID != reg.User.ID || me.Permissions != (api.Permissions{}) || me.Badges != nil || me.Session == nil {
				t.Errorf("me of a new member = %+v", me)
			}
			if uses := decodeBody[api.InviteInfo](t, f.checkInvite(token, clientIP(0)), http.StatusOK).UsesLeft; uses != 9 {
				t.Errorf("usesLeft = %d after one registration, want 9", uses)
			}
		}
		friends = append(friends, *reg.User)
	}
	// The 11th → 410 invite_used_up, on the page and at the form.
	rec = f.register(token, "friend10", clientIP(30))
	wantError(t, rec, http.StatusGone, api.CodeInviteUsedUp)
	if got, want := rec.Body.String(), `{"error":{"code":"invite_used_up"}}`+"\n"; got != want || len(setCookies(rec)) != 0 {
		t.Errorf("the 11th registration: body %s, cookies %q", got, setCookies(rec))
	}
	wantError(t, f.checkInvite(token, clientIP(30)), http.StatusGone, api.CodeInviteUsedUp)

	// The list: the used-up invite is inactive, so only state=all shows it, with the ten who used it.
	if got := f.invites(cookie, ""); len(got) != 0 {
		t.Errorf("active invites = %+v, want none", got)
	}
	rec = f.call(http.MethodGet, "/api/v1/invites", "", with(cookie))
	if got, want := rec.Body.String(), `{"invites":[]}`+"\n"; got != want {
		t.Errorf("an empty list = %s, want %s", got, want)
	}
	all := f.invites(cookie, "?state=all")
	if len(all) != 1 || all[0].ID != inv.ID || all[0].Uses != 10 || all[0].State != api.InviteStateUsedUp || len(all[0].RedeemedBy) != 10 {
		t.Fatalf("invites?state=all = %+v", all)
	}
	for i, u := range all[0].RedeemedBy {
		if u != (api.UserRef{ID: friends[i].ID, Username: friends[i].Username}) {
			t.Errorf("redeemedBy[%d] = %+v, want %+v (sign-up order)", i, u, friends[i])
		}
	}

	// Revoked → 410 invite_revoked.
	revoked, revokedToken := f.createInvite(cookie, `{}`)
	if revoked.MaxUses != 10 || !revoked.ExpiresAt.Equal(f.clk.now().Add(168*time.Hour)) || revoked.Note != "" {
		t.Errorf("an invite with every field left out = %+v, want the settings' defaults", revoked)
	}
	f.signal.takeNotes()
	rec = f.call(http.MethodDelete, "/api/v1/invites/"+revoked.ID, "", with(cookie))
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("DELETE invite: status %d, body %q", rec.Code, rec.Body)
	}
	if notes := f.signal.takeNotes(); fmt.Sprint(notes) != "[admins [admin.invites]]" {
		t.Errorf("Signal after an admin revoked its own invite = %v", notes)
	}
	wantError(t, f.checkInvite(revokedToken, clientIP(31)), http.StatusGone, api.CodeInviteRevoked)
	wantError(t, f.register(revokedToken, "late", clientIP(31)), http.StatusGone, api.CodeInviteRevoked)
	// Revoking again is the same answer; an unknown invite is 404.
	if rec := f.call(http.MethodDelete, "/api/v1/invites/"+revoked.ID, "", with(cookie)); rec.Code != http.StatusNoContent {
		t.Errorf("a second DELETE: status %d", rec.Code)
	}
	wantError(t, f.call(http.MethodDelete, "/api/v1/invites/nope00000000", "", with(cookie)), http.StatusNotFound, api.CodeNotFound)

	// Expired (clock) → 410 invite_expired.
	short, shortToken := f.createInvite(cookie, `{"expiresInHours":1,"maxUses":1}`)
	f.clk.advance(time.Hour - time.Millisecond)
	decodeBody[api.InviteInfo](t, f.checkInvite(shortToken, clientIP(32)), http.StatusOK)
	f.clk.advance(time.Millisecond)
	wantError(t, f.checkInvite(shortToken, clientIP(32)), http.StatusGone, api.CodeInviteExpired)
	wantError(t, f.register(shortToken, "late", clientIP(32)), http.StatusGone, api.CodeInviteExpired)
	states := map[string]api.InviteState{}
	for _, i := range f.invites(cookie, "?state=all") {
		states[i.ID] = i.State
	}
	want := map[string]api.InviteState{inv.ID: api.InviteStateUsedUp, revoked.ID: api.InviteStateRevoked, short.ID: api.InviteStateExpired}
	if fmt.Sprint(states) != fmt.Sprint(want) {
		t.Errorf("invite states = %v, want %v", states, want)
	}
	if f.tableRows("users") != 11 {
		t.Errorf("%d users, want the admin and the ten friends", f.tableRows("users"))
	}

	// The audit trail: who created and revoked, and every registration with its invite and the IP from ClientIP.
	if rows := f.auditOf("invite.created"); len(rows) != 3 || rows[0].Actor.Name != "Alex" || rows[0].Actor.IP != addrA ||
		rows[0].TargetKind != "invite" || rows[0].TargetID != inv.ID ||
		fmt.Sprint(rows[0].Detail) != "map[expiresAt:2026-10-08T12:00:00.000Z maxUses:10 note:for Sam]" {
		t.Errorf("invite.created rows = %+v", rows)
	}
	if rows := f.auditOf("invite.revoked"); len(rows) != 1 || rows[0].TargetID != revoked.ID || rows[0].Actor.IP != addrA {
		t.Errorf("invite.revoked rows = %+v, want one (the second DELETE changed nothing)", rows)
	}
	rows := f.auditOf("user.registered")
	if len(rows) != 10 {
		t.Fatalf("%d user.registered rows, want 10", len(rows))
	}
	for i, row := range rows {
		if row.Actor.Kind != store.ActorUser || row.Actor.Name != friends[i].Username || row.Actor.IP != clientIP(10+i) ||
			row.TargetID != friends[i].ID || fmt.Sprint(row.Detail) != "map[inviteId:"+inv.ID+"]" {
			t.Errorf("user.registered row %d = %+v", i, row)
		}
	}
}

// TestInvitePermissionsOverREST: admins see and revoke every invite; members create, see and revoke their own while
// membersCanInvite is on (03 §7.9, §12.3 #21–#23).
func TestInvitePermissionsOverREST(t *testing.T) {
	f := newAuthFixture(t)
	_, admin := f.setupAdmin("Alex")
	adminInvite, token := f.createInvite(admin, `{"note":"the admin's"}`)
	sam, samCookie := f.join(token, "Sam", clientIP(0))
	kim, kimCookie := f.join(token, "Kim", clientIP(1))
	f.signal.takeNotes()

	// Without a session: 401 on all three.
	wantError(t, f.call(http.MethodGet, "/api/v1/invites", ""), http.StatusUnauthorized, api.CodeUnauthenticated)
	wantError(t, f.call(http.MethodPost, "/api/v1/invites", `{}`), http.StatusUnauthorized, api.CodeUnauthenticated)
	wantError(t, f.call(http.MethodDelete, "/api/v1/invites/"+adminInvite.ID, ""), http.StatusUnauthorized, api.CodeUnauthenticated)

	// A member while membersCanInvite is off: 403 forbidden, also for a bad body or query.
	wantError(t, f.call(http.MethodGet, "/api/v1/invites", "", with(samCookie)), http.StatusForbidden, api.CodeForbidden)
	wantError(t, f.call(http.MethodGet, "/api/v1/invites?state=bogus", "", with(samCookie)), http.StatusForbidden, api.CodeForbidden)
	wantError(t, f.call(http.MethodPost, "/api/v1/invites", `{}`, with(samCookie)), http.StatusForbidden, api.CodeForbidden)
	wantError(t, f.call(http.MethodPost, "/api/v1/invites", `{"maxUses":-1}`, with(samCookie)), http.StatusForbidden, api.CodeForbidden)
	// Somebody else's invite does not exist for a member.
	wantError(t, f.call(http.MethodDelete, "/api/v1/invites/"+adminInvite.ID, "", with(samCookie)), http.StatusNotFound, api.CodeNotFound)
	if notes := f.signal.takeNotes(); len(notes) != 0 {
		t.Errorf("refused requests notified %v", notes)
	}

	// The admin turns membersCanInvite on: every SPA refetches me, whose permissions.createInvites follows.
	decodeBody[api.SettingsResponse](t, f.patchSettings(admin, `{"membersCanInvite":true}`), http.StatusOK)
	if notes := f.signal.takeNotes(); fmt.Sprint(notes) != "[admins [admin.settings] all [me]]" {
		t.Errorf("Signal after membersCanInvite changed = %v", notes)
	}
	if me := decodeBody[api.Me](t, f.call(http.MethodGet, "/api/v1/me", "", with(samCookie)), http.StatusOK); !me.Permissions.CreateInvites {
		t.Error("me.permissions.createInvites is false for a member while membersCanInvite is on")
	}

	// A member creates: the admins and the member's own tabs refetch.
	samInvite, samToken := f.createInvite(samCookie, `{"note":"for Lee","maxUses":2}`)
	if samInvite.CreatedBy == nil || *samInvite.CreatedBy != (api.UserRef{ID: sam.ID, Username: "Sam"}) {
		t.Errorf("a member's invite has the creator %+v", samInvite.CreatedBy)
	}
	want := "[admins [admin.invites] user " + sam.ID + " [admin.invites]]"
	if notes := f.signal.takeNotes(); fmt.Sprint(notes) != want {
		t.Errorf("Signal after a member created an invite = %v, want %s", notes, want)
	}
	// The lists: a member sees its own, the admin all of them, the newest first.
	if got := f.invites(samCookie, "?state=all"); len(got) != 1 || got[0].ID != samInvite.ID {
		t.Errorf("a member's list = %+v, want its own invite only", got)
	}
	if got := f.invites(kimCookie, ""); len(got) != 0 {
		t.Errorf("another member's list = %+v, want it empty", got)
	}
	if got := f.invites(admin, "?state=active"); len(got) != 2 || got[0].ID != samInvite.ID || got[1].ID != adminInvite.ID ||
		len(got[1].RedeemedBy) != 2 {
		t.Errorf("the admin's list = %+v, want both invites, the newest first", got)
	}
	wantError(t, f.call(http.MethodGet, "/api/v1/invites?state=bogus", "", with(admin)), http.StatusBadRequest, api.CodeBadRequest)

	// A registration with a member's invite reaches that member's tabs too.
	lee, _ := f.join(samToken, "Lee", clientIP(2))
	want = "[user " + lee.ID + " [devices] admins [admin.invites admin.users] user " + sam.ID + " [admin.invites]]"
	if notes := f.signal.takeNotes(); fmt.Sprint(notes) != want {
		t.Errorf("Signal after a registration with a member's invite = %v, want %s", notes, want)
	}

	// Members revoke their own invite and nobody else's; the admin revokes a member's, and that member hears.
	wantError(t, f.call(http.MethodDelete, "/api/v1/invites/"+samInvite.ID, "", with(kimCookie)), http.StatusNotFound, api.CodeNotFound)
	wantError(t, f.call(http.MethodDelete, "/api/v1/invites/"+adminInvite.ID, "", with(samCookie)), http.StatusNotFound, api.CodeNotFound)
	kimInvite, _ := f.createInvite(kimCookie, `{}`)
	f.signal.takeNotes()
	if rec := f.call(http.MethodDelete, "/api/v1/invites/"+kimInvite.ID, "", with(admin)); rec.Code != http.StatusNoContent {
		t.Fatalf("the admin's DELETE of a member's invite: status %d (body %q)", rec.Code, rec.Body)
	}
	want = "[admins [admin.invites] user " + kim.ID + " [admin.invites]]"
	if notes := f.signal.takeNotes(); fmt.Sprint(notes) != want {
		t.Errorf("Signal after the admin revoked a member's invite = %v, want %s", notes, want)
	}
	if rec := f.call(http.MethodDelete, "/api/v1/invites/"+samInvite.ID, "", with(samCookie)); rec.Code != http.StatusNoContent {
		t.Fatalf("a member's DELETE of its own invite: status %d (body %q)", rec.Code, rec.Body)
	}
	if got := f.invites(samCookie, "?state=all"); len(got) != 1 || got[0].State != api.InviteStateRevoked || got[0].Uses != 1 ||
		len(got[0].RedeemedBy) != 1 || got[0].RedeemedBy[0].ID != lee.ID {
		t.Errorf("the member's revoked invite = %+v", got)
	}
	// The account made with it stays.
	wantError(t, f.login("Lee", "not the password", clientIP(2)), http.StatusUnauthorized, api.CodeInvalidCredentials)
	f.sessionCookie(f.login("Lee", testPassword, clientIP(2)), thirtyDays)

	// Bodies: every field is optional; ranges and the note are checked; a body that is not one JSON object is 400.
	for body, wantFields := range map[string]string{
		`{"expiresInHours":721}`:                       "map[expiresInHours:out_of_range]",
		`{"expiresInHours":-1,"maxUses":1001}`:         "map[expiresInHours:out_of_range maxUses:out_of_range]",
		`{"note":"` + strings.Repeat("x", 65) + `"}`:   "map[note:too_long]",
		`{"note":"two\nlines","maxUses":1000000000}`:   "map[maxUses:out_of_range note:invalid]",
		`{"note":"tab\there","expiresInHours":100000}`: "map[expiresInHours:out_of_range note:invalid]",
	} {
		e := wantError(t, f.call(http.MethodPost, "/api/v1/invites", body, with(admin)), http.StatusUnprocessableEntity, api.CodeValidationFailed)
		if fmt.Sprint(e.Fields) != wantFields {
			t.Errorf("POST /invites %.50s: fields %v, want %s", body, e.Fields, wantFields)
		}
	}
	for _, body := range []string{``, `[]`, `{"maxUses":"ten"}`, `{"maxUses":1.5}`, `{} {}`} {
		wantError(t, f.call(http.MethodPost, "/api/v1/invites", body, with(admin)), http.StatusBadRequest, api.CodeBadRequest)
	}
	wantError(t, f.call(http.MethodPost, "/api/v1/invites", `{"note":"`+strings.Repeat("x", otherBodyLimit)+`"}`, with(admin)),
		http.StatusRequestEntityTooLarge, api.CodePayloadTooLarge)

	// A member's 11th active invite → 409 limit_reached {limit: member_invites}.
	for range 10 {
		f.createInvite(kimCookie, `{}`)
	}
	rec := f.call(http.MethodPost, "/api/v1/invites", `{}`, with(kimCookie))
	wantError(t, rec, http.StatusConflict, api.CodeLimitReached)
	if got, wantBody := rec.Body.String(), `{"error":{"code":"limit_reached","params":{"limit":"member_invites"}}}`+"\n"; got != wantBody {
		t.Errorf("409 body = %s, want %s", got, wantBody)
	}

	// membersCanInvite off again: the member's list is forbidden, and createInvites is false.
	decodeBody[api.SettingsResponse](t, f.patchSettings(admin, `{"membersCanInvite":false}`), http.StatusOK)
	wantError(t, f.call(http.MethodGet, "/api/v1/invites", "", with(kimCookie)), http.StatusForbidden, api.CodeForbidden)
	wantError(t, f.call(http.MethodPost, "/api/v1/invites", `{}`, with(kimCookie)), http.StatusForbidden, api.CodeForbidden)
	if me := decodeBody[api.Me](t, f.call(http.MethodGet, "/api/v1/me", "", with(kimCookie)), http.StatusOK); me.Permissions.CreateInvites {
		t.Error("me.permissions.createInvites is still true")
	}
}

// TestRegisterWithOldCookieOverREST is the 03 §7.7 row "registration that arrives with an existing session cookie":
// a signed-in friend who creates another account loses the old session, which ConnCloser gets with session_revoked.
// A refused registration, and a sign-up that stays pending, leave the old session alone.
func TestRegisterWithOldCookieOverREST(t *testing.T) {
	f := newAuthFixture(t)
	_, admin := f.setupAdmin("Alex")
	_, token := f.createInvite(admin, `{}`)
	sam, old := f.join(token, "Sam", clientIP(0))
	oldMe := decodeBody[api.Me](t, f.call(http.MethodGet, "/api/v1/me", "", with(old)), http.StatusOK)
	f.conns.take()

	wantError(t, f.register(token, "sam", clientIP(0), with(old)), http.StatusConflict, api.CodeUsernameTaken)
	f.setMode(admin, "approval")
	if rec := f.register("", "pending", clientIP(0), with(old)); rec.Code != http.StatusAccepted || len(setCookies(rec)) != 0 {
		t.Fatalf("a sign-up from a signed-in browser: status %d, cookies %q", rec.Code, setCookies(rec))
	}
	decodeBody[api.Me](t, f.call(http.MethodGet, "/api/v1/me", "", with(old)), http.StatusOK)
	if calls := f.conns.take(); len(calls) != 0 {
		t.Fatalf("a registration that created no session closed connections: %+v", calls)
	}

	rec := f.register(token, "Second", clientIP(0), with(old))
	fresh := f.sessionCookie(rec, thirtyDays)
	second := decodeBody[api.RegisterResponse](t, rec, http.StatusCreated).User
	want := []closedConn{{auth.ConnSelector{UserID: store.UserID(sam.ID), SessionID: store.SessionID(oldMe.Session.ID)},
		auth.ReasonSessionRevoked}}
	if calls := f.conns.take(); fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("CloseConnections calls = %+v, want %+v", calls, want)
	}
	wantError(t, f.call(http.MethodGet, "/api/v1/me", "", with(old)), http.StatusUnauthorized, api.CodeUnauthenticated)
	if me := decodeBody[api.Me](t, f.call(http.MethodGet, "/api/v1/me", "", with(fresh)), http.StatusOK); me.User.ID != second.ID {
		t.Errorf("me with the new cookie = %+v, want the new account", me.User)
	}
}

// TestInviteServerLimitOverREST: the 101st active invite → 409 limit_reached {limit: invites} (03 §7.9).
func TestInviteServerLimitOverREST(t *testing.T) {
	f := newAuthFixture(t)
	_, admin := f.setupAdmin("Alex")
	for range 100 {
		f.createInvite(admin, `{"maxUses":1}`)
	}
	rec := f.call(http.MethodPost, "/api/v1/invites", `{}`, with(admin))
	wantError(t, rec, http.StatusConflict, api.CodeLimitReached)
	if got, want := rec.Body.String(), `{"error":{"code":"limit_reached","params":{"limit":"invites"}}}`+"\n"; got != want {
		t.Errorf("409 body = %s, want %s", got, want)
	}
	if got := f.invites(admin, ""); len(got) != 100 {
		t.Errorf("%d active invites, want 100", len(got))
	}
}

// TestClosedModeOverREST is 03 §15's closed-mode case: invite check, register and invite creation → 403
// registration_closed.
func TestClosedModeOverREST(t *testing.T) {
	f := newAuthFixture(t)
	_, admin := f.setupAdmin("Alex")
	invite, token := f.createInvite(admin, `{}`)
	f.setMode(admin, "closed")
	if info := decodeBody[api.Info](t, f.call(http.MethodGet, "/api/v1/info", ""), http.StatusOK); info.Registration != api.RegistrationModeClosed {
		t.Fatalf("info.registration = %q", info.Registration)
	}

	// Existing links are refused, at the page and at the form; so is a sign-up without one.
	rec := f.checkInvite(token, clientIP(0))
	wantError(t, rec, http.StatusForbidden, api.CodeRegistrationClosed)
	if got, want := rec.Body.String(), `{"error":{"code":"registration_closed"}}`+"\n"; got != want {
		t.Errorf("403 body = %s, want %s", got, want)
	}
	wantError(t, f.checkInvite("garbage", clientIP(0)), http.StatusForbidden, api.CodeRegistrationClosed)
	for _, tok := range []string{token, "", "garbage"} {
		rec := f.register(tok, "Sam", clientIP(1))
		wantError(t, rec, http.StatusForbidden, api.CodeRegistrationClosed)
		if len(setCookies(rec)) != 0 {
			t.Error("a refused registration set a cookie")
		}
	}
	// Creating one also gets 403.
	wantError(t, f.call(http.MethodPost, "/api/v1/invites", `{}`, with(admin)), http.StatusForbidden, api.CodeRegistrationClosed)
	if f.tableRows("users") != 1 || f.tableRows("invites") != 1 {
		t.Fatalf("the closed mode created something: %d users, %d invites", f.tableRows("users"), f.tableRows("invites"))
	}
	if notes := f.signal.takeNotes(); len(notes) != 0 {
		t.Errorf("refused requests notified %v", notes)
	}
	// The invite is still listed, and can be revoked: the freeze changes no row.
	if got := f.invites(admin, ""); len(got) != 1 || got[0].ID != invite.ID || got[0].State != api.InviteStateActive {
		t.Errorf("invites in the closed mode = %+v", got)
	}

	// Closed differs from invite only in refusing existing links: back in the invite mode, the link works again.
	f.setMode(admin, "invite")
	decodeBody[api.InviteInfo](t, f.checkInvite(token, clientIP(2)), http.StatusOK)
	f.join(token, "Sam", clientIP(2))
}

// tableRows returns the number of rows of a table.
func (f *authFixture) tableRows(table string) int {
	f.t.Helper()
	n := 0
	f.read(func(q *store.Q) error {
		var err error
		switch table {
		case "users":
			var rows []store.UserRow
			rows, err = q.ListUsers("")
			n = len(rows)
		case "invites":
			var rows []store.Invite
			rows, err = q.ListInvites("", true)
			n = len(rows)
		default:
			f.t.Fatalf("tableRows: unknown table %q", table)
		}
		return err
	})
	return n
}
