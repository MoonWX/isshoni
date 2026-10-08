package httpapi

import (
	"context"
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

// signup sends a sign-up without an invite from address ip and fails the test unless it is accepted as pending.
func (f *authFixture) signup(username, ip string) {
	f.t.Helper()
	rec := f.register("", username, ip)
	if res := decodeBody[api.RegisterResponse](f.t, rec, http.StatusAccepted); res.Status != api.UserStatusPending || res.User != nil {
		f.t.Fatalf("sign-up %s = %+v, want {status: pending}", username, res)
	}
	if len(rec.Header().Values("Set-Cookie")) != 0 {
		f.t.Fatalf("sign-up %s set a cookie", username)
	}
}

// decide sends POST /api/v1/admin/approvals/{id}/{verb} with a raw body.
func (f *authFixture) decide(c *http.Cookie, id, verb, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.call(http.MethodPost, "/api/v1/admin/approvals/"+id+"/"+verb, body, with(c))
}

// TestApprovalModeOverREST is 03 §15's approval-mode case: register without an invite → 202; login → 403
// account_pending, and a wrong password → 401; the admin approves → login 200; rejecting frees the username; the
// signup_pending alert reaches the alerter.
func TestApprovalModeOverREST(t *testing.T) {
	f := newAuthFixture(t)
	admin, cookie := f.setupAdmin("Alex")
	_, token := f.createInvite(cookie, `{}`)
	member, memberCookie := f.join(token, "Member", clientIP(80))

	// A sign-up without an invite is refused until the admin turns the approval mode on.
	wantError(t, f.register("", "sam_k", addrB), http.StatusForbidden, api.CodeInviteRequired)
	f.setMode(cookie, "approval")
	if info := decodeBody[api.Info](t, f.call(http.MethodGet, "/api/v1/info", ""), http.StatusOK); info.Registration != api.RegistrationModeApproval {
		t.Fatalf("info.registration = %q", info.Registration)
	}

	// Register without an invite → 202 {status: "pending"}, with no cookie.
	requested := f.clk.now()
	rec := f.register("", "sam_k", addrB)
	if got, want := rec.Body.String(), `{"status":"pending"}`+"\n"; rec.Code != http.StatusAccepted || got != want {
		t.Fatalf("sign-up: status %d, body %s; want 202 %s", rec.Code, got, want)
	}
	if len(rec.Header().Values("Set-Cookie")) != 0 {
		t.Error("a pending sign-up got a cookie")
	}
	// Every admin's approval queue and user list changed; the alert reached the alerter.
	if notes := f.signal.takeNotes(); fmt.Sprint(notes) != "[admins [admin.approvals admin.users]]" {
		t.Errorf("Signal after a sign-up = %v", notes)
	}
	alerts := f.alerts.take()
	if len(alerts) != 1 || alerts[0] != (auth.AdminAlert{Kind: auth.AlertSignupPending, Actor: "system", Target: "sam_k", At: requested}) {
		t.Fatalf("alerts = %+v, want one signup_pending for sam_k", alerts)
	}

	// Login → 403 account_pending after the correct password, 401 after a wrong one.
	wantError(t, f.login("sam_k", testPassword, addrB), http.StatusForbidden, api.CodeAccountPending)
	wantError(t, f.login("sam_k", "not the password", addrB), http.StatusUnauthorized, api.CodeInvalidCredentials)
	// The name is taken while the sign-up waits.
	wantError(t, f.register("", "SAM_K", addrC), http.StatusConflict, api.CodeUsernameTaken)
	wantError(t, f.register(token, "Sam_K", addrC), http.StatusConflict, api.CodeUsernameTaken)

	// The queue: username, request time and the sign-up's IP; the admin's badge counts it.
	f.clk.advance(time.Minute)
	f.signup("kim", clientIP(1))
	rec = f.call(http.MethodGet, "/api/v1/admin/approvals", "", with(cookie))
	queue := decodeBody[api.ApprovalsResponse](t, rec, http.StatusOK).Pending
	if len(queue) != 2 || queue[0].Username != "sam_k" || queue[0].IP != addrB || !queue[0].RequestedAt.Equal(requested) ||
		len(queue[0].ID) != 12 || queue[1].Username != "kim" || queue[1].IP != clientIP(1) {
		t.Fatalf("approvals = %+v, want sam_k then kim (the oldest first), each with its IP", queue)
	}
	if got, want := fmt.Sprint(jsonKeys(t, rec.Body.Bytes())), "[pending]"; got != want {
		t.Errorf("approvals keys = %s, want %s", got, want)
	}
	if want := `{"pending":[{"id":"` + queue[0].ID + `","username":"sam_k","ip":"198.51.100.23",` +
		`"requestedAt":"2026-10-01T12:00:00.000Z"},`; !strings.HasPrefix(rec.Body.String(), want) {
		t.Errorf("approvals body = %s, want it to start with %s", rec.Body, want)
	}
	if me := decodeBody[api.Me](t, f.call(http.MethodGet, "/api/v1/me", "", with(cookie)), http.StatusOK); me.Badges == nil ||
		me.Badges.PendingApprovals != 2 {
		t.Errorf("me.badges = %+v, want 2 pending approvals", me.Badges)
	}
	samID, kimID := queue[0].ID, queue[1].ID

	// Only admins decide; a member gets 403 on every admin route, and nobody without a session gets in.
	for _, path := range []string{"/api/v1/admin/approvals/" + samID + "/approve", "/api/v1/admin/approvals/" + samID + "/reject",
		"/api/v1/admin/approvals/all/reject"} {
		wantError(t, f.call(http.MethodPost, path, `{"all":true}`, with(memberCookie)), http.StatusForbidden, api.CodeForbidden)
		wantError(t, f.call(http.MethodPost, path, `{"all":true}`), http.StatusUnauthorized, api.CodeUnauthenticated)
	}
	wantError(t, f.call(http.MethodGet, "/api/v1/admin/approvals", "", with(memberCookie)), http.StatusForbidden, api.CodeForbidden)
	wantError(t, f.call(http.MethodGet, "/api/v1/admin/approvals", ""), http.StatusUnauthorized, api.CodeUnauthenticated)
	// Users that are not pending, and IDs nobody has → 404 user_not_found.
	for _, id := range []string{"nobody000000", admin.ID, member.ID, "all"} {
		wantError(t, f.decide(cookie, id, "approve", `{}`), http.StatusNotFound, api.CodeUserNotFound)
	}
	for _, id := range []string{"nobody000000", admin.ID, member.ID} {
		wantError(t, f.decide(cookie, id, "reject", `{}`), http.StatusNotFound, api.CodeUserNotFound)
	}
	if got := f.pending(cookie); len(got) != 2 || f.tableRows("users") != 4 {
		t.Fatalf("refused decisions changed something: %d pending, %d users", len(got), f.tableRows("users"))
	}
	f.signal.takeNotes()

	// The admin approves → 200 {user}; the user can log in.
	f.clk.advance(time.Minute)
	rec = f.decide(cookie, samID, "approve", `{}`)
	approved := decodeBody[api.AdminUserResponse](t, rec, http.StatusOK).User
	if approved.ID != samID || approved.Username != "sam_k" || approved.Role != api.RoleUser || approved.Status != api.UserStatusActive ||
		approved.CreatedVia != api.CreatedViaSignup || !approved.CreatedAt.Equal(requested) || approved.Sessions != 0 ||
		approved.Devices != 0 || approved.Online || approved.ResetPending || approved.InvitedBy != nil ||
		!approved.LastLoginAt.IsZero() || !approved.LastSeenAt.IsZero() {
		t.Errorf("approved user = %+v", approved)
	}
	if got, want := fmt.Sprint(jsonKeys(t, rec.Body.Bytes(), "user")),
		"[createdAt createdVia devices id online resetPending role sessions status username]"; got != want {
		t.Errorf("approved user keys = %s, want %s (no IPs, no hash)", got, want)
	}
	if notes := f.signal.takeNotes(); fmt.Sprint(notes) != "[admins [admin.approvals admin.users]]" {
		t.Errorf("Signal after an approval = %v", notes)
	}
	samCookie := f.sessionCookie(f.login("sam_k", testPassword, addrB), thirtyDays)
	if me := decodeBody[api.Me](t, f.call(http.MethodGet, "/api/v1/me", "", with(samCookie)), http.StatusOK); me.User.ID != samID {
		t.Errorf("me of the approved user = %+v", me.User)
	}
	// It is no longer pending: a second approval, or a rejection, finds nobody.
	wantError(t, f.decide(cookie, samID, "approve", `{}`), http.StatusNotFound, api.CodeUserNotFound)
	wantError(t, f.decide(cookie, samID, "reject", `{}`), http.StatusNotFound, api.CodeUserNotFound)

	// Rejecting → 204, and the username is free again. The body is optional.
	f.signal.takeNotes()
	rec = f.decide(cookie, kimID, "reject", "")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("reject: status %d, body %q", rec.Code, rec.Body)
	}
	if notes := f.signal.takeNotes(); fmt.Sprint(notes) != "[admins [admin.approvals admin.users]]" {
		t.Errorf("Signal after a rejection = %v", notes)
	}
	wantError(t, f.login("kim", testPassword, clientIP(1)), http.StatusUnauthorized, api.CodeInvalidCredentials)
	f.signup("kim", clientIP(2))
	if got := f.pending(cookie); len(got) != 1 || got[0].Username != "kim" || got[0].ID == kimID {
		t.Errorf("the queue after signing up again = %+v", got)
	}

	// The audit rows, with the IPs from ClientIP: the sign-up is anonymous, named after the user it asks for.
	if rows := f.auditOf("user.signup_requested"); len(rows) != 3 || rows[0].Actor != (store.Actor{Kind: store.ActorAnonymous,
		Name: "sam_k", IP: addrB}) || rows[0].TargetKind != "user" || rows[0].TargetID != samID {
		t.Errorf("user.signup_requested rows = %+v", rows)
	}
	if rows := f.auditOf("user.approved"); len(rows) != 1 || rows[0].Actor.Name != "Alex" || rows[0].Actor.IP != addrA ||
		rows[0].TargetID != samID || rows[0].TargetName != "sam_k" {
		t.Errorf("user.approved rows = %+v", rows)
	}
	if rows := f.auditOf("user.signup_rejected"); len(rows) != 1 || rows[0].Actor.Name != "Alex" || rows[0].TargetID != kimID ||
		rows[0].TargetName != "kim" || len(rows[0].Detail) != 0 {
		t.Errorf("user.signup_rejected rows = %+v", rows)
	}

	// A pending user older than 14 d is pruned (the janitor's rule, 03 §4.7).
	f.clk.advance(14*24*time.Hour + time.Millisecond)
	if st, err := f.db.Prune(context.Background(), f.clk.now()); err != nil || st.Pending != 1 {
		t.Fatalf("Prune = %+v, %v; want one pending sign-up expired", st, err)
	}
	if got := f.pending(cookie); len(got) != 0 {
		t.Errorf("the queue after 14 days = %+v", got)
	}
	if rows := f.auditOf("user.signup_expired"); len(rows) != 1 || rows[0].TargetName != "kim" || rows[0].Actor != store.SystemActor {
		t.Errorf("user.signup_expired rows = %+v", rows)
	}
}

// TestSignupLimitsOverREST: the 51st pending sign-up → 409 limit_reached; the signup_pending alert is coalesced to
// one per 10 min; the sixth sign-up of one address → 429 (03 §15, §7.3).
func TestSignupLimitsOverREST(t *testing.T) {
	f := newAuthFixture(t)
	_, cookie := f.setupAdmin("Alex")
	f.setMode(cookie, "approval")

	for i := range 50 {
		f.signup(fmt.Sprintf("friend%d", i), clientIP(i))
	}
	if alerts := f.alerts.take(); len(alerts) != 1 || alerts[0].Kind != auth.AlertSignupPending || alerts[0].Target != "friend0" {
		t.Fatalf("alerts for 50 sign-ups at once = %+v, want one", alerts)
	}
	rec := f.register("", "friend50", clientIP(50))
	wantError(t, rec, http.StatusConflict, api.CodeLimitReached)
	if got, want := rec.Body.String(), `{"error":{"code":"limit_reached","params":{"limit":"pending_signups"}}}`+"\n"; got != want {
		t.Errorf("409 body = %s, want %s", got, want)
	}
	if got := f.pending(cookie); len(got) != 50 {
		t.Fatalf("%d pending sign-ups, want 50", len(got))
	}
	if me := decodeBody[api.Me](t, f.call(http.MethodGet, "/api/v1/me", "", with(cookie)), http.StatusOK); me.Badges.PendingApprovals != 50 {
		t.Errorf("pendingApprovals = %d", me.Badges.PendingApprovals)
	}
	// One rejection makes room; 10 minutes later the next sign-up alerts again.
	if rec := f.decide(cookie, f.pending(cookie)[0].ID, "reject", `{}`); rec.Code != http.StatusNoContent {
		t.Fatalf("reject: status %d", rec.Code)
	}
	f.clk.advance(10 * time.Minute)
	f.signup("friend50", clientIP(50))
	if alerts := f.alerts.take(); len(alerts) != 1 || alerts[0].Target != "friend50" {
		t.Fatalf("alerts 10 minutes later = %+v, want one more", alerts)
	}

	// register-ip: an address gets 5 sign-ups without an invite, then 429 with Retry-After (12 minutes per token).
	const busy = "203.0.113.200"
	for range 5 {
		wantError(t, f.register("", "friend50", busy), http.StatusConflict, api.CodeLimitReached)
	}
	rec = f.register("", "friend50", busy)
	e := wantError(t, rec, http.StatusTooManyRequests, api.CodeRateLimited)
	if e.RetryAfter != 720 || rec.Header().Get("Retry-After") != "720" {
		t.Errorf("register-ip block: retryAfter %d, Retry-After %q; want 720", e.RetryAfter, rec.Header().Get("Retry-After"))
	}
}

// TestRejectAllOverREST is 03 §15's reject-all case: with 3 pending → 200 {rejected: 3}, the queue is empty, and one
// user.signup_rejected {all: true, count: 3} row is written; all without the {"all": true} body → 404 and nothing
// is deleted.
func TestRejectAllOverREST(t *testing.T) {
	f := newAuthFixture(t)
	_, cookie := f.setupAdmin("Alex")
	f.setMode(cookie, "approval")
	for i := range 3 {
		f.signup(fmt.Sprintf("fake%d", i), clientIP(i))
	}
	f.signal.takeNotes()

	// all without the body {"all": true} is looked up as an ID → 404, and nothing is deleted.
	for _, body := range []string{``, `{}`, `{"all":false}`, `null`, `  `} {
		wantError(t, f.decide(cookie, "all", "reject", body), http.StatusNotFound, api.CodeUserNotFound)
	}
	// The body alone is not enough either: with an ID, only that sign-up is judged.
	wantError(t, f.decide(cookie, "nobody000000", "reject", `{"all":true}`), http.StatusNotFound, api.CodeUserNotFound)
	// A body that is not one JSON object → 400.
	for _, body := range []string{`{"all":`, `[]`, `{"all":"yes"}`, `{"all":true} x`} {
		wantError(t, f.decide(cookie, "all", "reject", body), http.StatusBadRequest, api.CodeBadRequest)
	}
	if got := f.pending(cookie); len(got) != 3 {
		t.Fatalf("%d pending sign-ups after refused requests, want 3", len(got))
	}
	if rows := f.auditOf("user.signup_rejected"); len(rows) != 0 || len(f.signal.takeNotes()) != 0 {
		t.Fatalf("refused requests wrote %d audit rows or notified", len(rows))
	}

	rec := f.decide(cookie, "all", "reject", `{"all": true}`)
	if got, want := rec.Body.String(), `{"rejected":3}`+"\n"; rec.Code != http.StatusOK || got != want {
		t.Fatalf("reject all: status %d, body %s; want 200 %s", rec.Code, got, want)
	}
	if got := f.pending(cookie); len(got) != 0 || f.tableRows("users") != 1 {
		t.Errorf("after reject all: %d pending, %d users; want none and the admin", len(got), f.tableRows("users"))
	}
	rows := f.auditOf("user.signup_rejected")
	if len(rows) != 1 || rows[0].Actor.Name != "Alex" || rows[0].Actor.IP != addrA || rows[0].TargetKind != "" ||
		rows[0].TargetID != "" || fmt.Sprint(rows[0].Detail) != "map[all:true count:3]" {
		t.Fatalf("user.signup_rejected rows = %+v, want one {all: true, count: 3} without a target", rows)
	}
	if notes := f.signal.takeNotes(); fmt.Sprint(notes) != "[admins [admin.approvals admin.users]]" {
		t.Errorf("Signal after reject all = %v", notes)
	}
	// An empty queue: 200 {rejected: 0}, and nothing happens.
	rec = f.decide(cookie, "all", "reject", `{"all":true}`)
	if got, want := rec.Body.String(), `{"rejected":0}`+"\n"; rec.Code != http.StatusOK || got != want {
		t.Errorf("reject all on an empty queue: status %d, body %s", rec.Code, got)
	}
	if len(f.auditOf("user.signup_rejected")) != 1 || len(f.signal.takeNotes()) != 0 {
		t.Error("reject all on an empty queue wrote a row or notified")
	}
	// The names are free again.
	f.signup("fake0", clientIP(10))
}
