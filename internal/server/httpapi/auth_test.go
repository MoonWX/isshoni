package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/auth"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// TestSetupOverREST is 03 §15's setup case: the CLI path issues a token; setup/check → 204; complete → 201 with the
// cookie; a second complete → 404 setup_unavailable; SetupAvailable becomes false; issuing again →
// setup_unavailable.
func TestSetupOverREST(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	if info := decodeBody[api.Info](t, f.call(http.MethodGet, "/api/v1/info", ""), http.StatusOK); !info.SetupRequired {
		t.Fatal("info.setupRequired is false on a new server")
	}
	token := f.setupToken()

	// setup/check: 204 without a body for the live token.
	rec := f.call(http.MethodPost, "/api/v1/auth/setup/check", jsonBody(t, api.TokenRequest{Token: token}))
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("setup/check: status %d, body %q; want 204 and no body", rec.Code, rec.Body)
	}
	for _, body := range []string{`{"token":"` + strings.Repeat("A", 43) + `"}`, `{"token":"nope"}`, `{}`} {
		wantError(t, f.call(http.MethodPost, "/api/v1/auth/setup/check", body), http.StatusNotFound, api.CodeSetupTokenInvalid)
	}
	// The body rules of the chain apply: malformed JSON is 400, more than 16 KiB is 413.
	wantError(t, f.call(http.MethodPost, "/api/v1/auth/setup/check", `{"token":`), http.StatusBadRequest, api.CodeBadRequest)
	wantError(t, f.call(http.MethodPost, "/api/v1/auth/setup/complete", `{"token":"`+strings.Repeat("x", authBodyLimit)+`"}`),
		http.StatusRequestEntityTooLarge, api.CodePayloadTooLarge)

	// Field errors come as codes per field (the minimum password length is 8).
	e := wantError(t, f.call(http.MethodPost, "/api/v1/auth/setup/complete", jsonBody(t, api.SetupCompleteRequest{
		Token: token, Username: "a", Password: "1234567"})), http.StatusUnprocessableEntity, api.CodeValidationFailed)
	if fmt.Sprint(e.Fields) != "map[password:too_short username:too_short]" {
		t.Errorf("fields = %v", e.Fields)
	}
	wantError(t, f.call(http.MethodPost, "/api/v1/auth/setup/complete", jsonBody(t, api.SetupCompleteRequest{
		Token: strings.Repeat("A", 43), Username: "Alex", Password: testPassword})),
		http.StatusNotFound, api.CodeSetupTokenInvalid)
	f.signal.take()

	// complete: 201 {user} with the session cookie.
	rec = f.call(http.MethodPost, "/api/v1/auth/setup/complete", jsonBody(t, api.SetupCompleteRequest{
		Token: token, Username: "Alex", Password: testPassword, ServerName: "Alex's server"}))
	cookie := f.sessionCookie(rec, thirtyDays)
	res := decodeBody[api.UserResponse](t, rec, http.StatusCreated)
	if res.User.Username != "Alex" || res.User.Role != api.RoleAdmin || len(res.User.ID) != 12 {
		t.Fatalf("setup/complete user = %+v", res.User)
	}
	var raw map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil || len(raw) != 1 || len(raw["user"]) != 3 {
		t.Errorf("setup/complete body %q: want exactly {user: {id, username, role}}", rec.Body)
	}
	if strings.Contains(rec.Body.String(), cookie.Value) || strings.Contains(rec.Body.String(), token) {
		t.Error("the response body carries a token")
	}
	if calls := f.signal.take(); fmt.Sprint(calls) != "[notify [devices]]" {
		t.Errorf("Signal calls = %v, want one devices notification", calls)
	}

	// The admin is logged in, and setup is over.
	me := decodeBody[api.Me](t, f.call(http.MethodGet, "/api/v1/me", "", with(cookie)), http.StatusOK)
	if me.User.ID != res.User.ID || !me.Permissions.Admin {
		t.Fatalf("me after setup = %+v", me)
	}
	body := jsonBody(t, api.SetupCompleteRequest{Token: token, Username: "Eve", Password: testPassword})
	wantError(t, f.call(http.MethodPost, "/api/v1/auth/setup/complete", body), http.StatusNotFound, api.CodeSetupUnavailable)
	wantError(t, f.call(http.MethodPost, "/api/v1/auth/setup/check", jsonBody(t, api.TokenRequest{Token: token})),
		http.StatusNotFound, api.CodeSetupUnavailable)
	if ok, err := f.svc.SetupAvailable(ctx); err != nil || ok {
		t.Fatalf("SetupAvailable = %v, %v after setup", ok, err)
	}
	if _, err := f.svc.IssueSetupToken(ctx, store.CLIActor); !api.IsCode(err, api.CodeSetupUnavailable) {
		t.Fatalf("IssueSetupToken after setup: %v, want setup_unavailable", err)
	}
	info := decodeBody[api.Info](t, f.call(http.MethodGet, "/api/v1/info", ""), http.StatusOK)
	if info.SetupRequired || info.Server.Name != "Alex's server" {
		t.Errorf("info after setup: setupRequired %v, server name %q", info.SetupRequired, info.Server.Name)
	}

	// The audit trail, each row with the IP from ClientIP: the CLI's token, the settings change and the setup.
	var got []string
	for _, row := range f.audit() {
		got = append(got, row.Action+" "+string(row.Actor.Kind)+" "+row.Actor.IP)
	}
	want := []string{"setup.token_issued cli ", "settings.changed user " + addrA, "setup.completed user " + addrA}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("audit rows = %q, want %q", got, want)
	}
}

// TestSetupParallelOverREST: two parallel complete calls (here eight) leave exactly one admin; the losers get 404
// setup_unavailable (03 §7.8, §15).
func TestSetupParallelOverREST(t *testing.T) {
	f := newAuthFixture(t)
	token := f.setupToken()
	const n = 8
	recs := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			recs[i] = f.call(http.MethodPost, "/api/v1/auth/setup/complete", jsonBody(t, api.SetupCompleteRequest{
				Token: token, Username: fmt.Sprintf("admin%d", i), Password: testPassword}),
				from(fmt.Sprintf("192.0.2.%d", i+1)))
		})
	}
	close(start)
	wg.Wait()
	created := 0
	for i, rec := range recs {
		switch rec.Code {
		case http.StatusCreated:
			created++
			f.sessionCookie(rec, thirtyDays)
		case http.StatusNotFound:
			wantError(t, rec, http.StatusNotFound, api.CodeSetupUnavailable)
			if len(setCookies(rec)) != 0 {
				t.Errorf("request %d lost the race and still got a cookie", i)
			}
		default:
			t.Errorf("request %d: status %d (body %q)", i, rec.Code, rec.Body)
		}
	}
	if created != 1 {
		t.Fatalf("%d requests answered 201, want exactly 1", created)
	}
	f.read(func(q *store.Q) error {
		users, err := q.ListUsers("")
		if err == nil && (len(users) != 1 || users[0].Role != store.RoleAdmin || users[0].Sessions != 1) {
			t.Errorf("users after the race = %+v, want exactly one admin with one session", users)
		}
		return err
	})
}

// TestLoginLogoutMe walks one browser through login, me and logout (03 §12.4.2–12.4.3, §7.7 "Logout").
func TestLoginLogoutMe(t *testing.T) {
	f := newAuthFixture(t)
	admin, setupCookie := f.setupAdmin("Alex")
	f.conns.take()
	f.signal.take()
	created := f.clk.now()
	f.clk.advance(time.Hour)

	// login: 200 {user} with a new session cookie.
	rec := f.login("alex", testPassword, addrB)
	cookie := f.sessionCookie(rec, thirtyDays)
	res := decodeBody[api.UserResponse](t, rec, http.StatusOK)
	if res.User != (api.User{ID: admin.ID, Username: "Alex", Role: api.RoleAdmin}) {
		t.Fatalf("login user = %+v", res.User)
	}
	if got, want := rec.Body.String(), `{"user":{"id":"`+admin.ID+`","username":"Alex","role":"admin"}}`+"\n"; got != want {
		t.Errorf("login body = %q, want %q", got, want)
	}
	if cookie.Value == setupCookie.Value {
		t.Fatal("the login reused the setup session's token")
	}
	if calls := f.signal.take(); fmt.Sprint(calls) != "[notify [devices]]" {
		t.Errorf("Signal calls after a login = %v", calls)
	}

	// me: the account, this session, the permissions and the admin badges.
	rec = f.call(http.MethodGet, "/api/v1/me", "", with(cookie), from(addrB))
	me := decodeBody[api.Me](t, rec, http.StatusOK)
	if me.User.ID != admin.ID || me.User.Username != "Alex" || me.User.Role != api.RoleAdmin || !me.User.CreatedAt.Equal(created) {
		t.Errorf("me.user = %+v", me.User)
	}
	loggedIn := f.clk.now()
	if s := me.Session; s == nil || len(s.ID) != 12 || s.Name != "Chrome on Windows" || !s.Current ||
		!s.CreatedAt.Equal(loggedIn) || !s.ExpiresAt.Equal(loggedIn.Add(180*24*time.Hour)) {
		t.Errorf("me.session = %+v", me.Session)
	}
	if me.Permissions != (api.Permissions{Admin: true, CreateInvites: true}) || me.Badges == nil || me.Badges.PendingApprovals != 0 ||
		me.Device != nil {
		t.Errorf("me permissions %+v, badges %+v, device %+v", me.Permissions, me.Badges, me.Device)
	}
	var shape map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &shape); err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(keysOf(shape)), "[badges permissions session user]"; got != want {
		t.Errorf("me keys = %s, want %s", got, want)
	}
	if got := shape["user"]["createdAt"]; got != "2026-10-01T12:00:00.000Z" {
		t.Errorf("me.user.createdAt = %v, want the fixed REST form", got)
	}
	if got, want := fmt.Sprint(keysOf(shape["session"])), "[createdAt current expiresAt id name]"; got != want {
		t.Errorf("me.session keys = %s, want %s (03 §12.4.3)", got, want)
	}
	if len(setCookies(rec)) != 0 {
		t.Error("a fresh session was rotated")
	}
	// HEAD is served like GET (net/http drops the body on a real connection).
	if rec := f.call(http.MethodHead, "/api/v1/me", "", with(cookie)); rec.Code != http.StatusOK {
		t.Errorf("HEAD /me: status %d", rec.Code)
	}
	// A pending sign-up shows in the admin's badge.
	f.write(func(q *store.Q) error {
		return q.CreateUser(&store.User{Username: "Sam", UsernameKey: "sam", Status: store.StatusPending, CreatedVia: "signup"})
	})
	if me := decodeBody[api.Me](t, f.call(http.MethodGet, "/api/v1/me", "", with(cookie)), http.StatusOK); me.Badges.PendingApprovals != 1 {
		t.Errorf("pendingApprovals = %d, want 1", me.Badges.PendingApprovals)
	}

	// Without a session: 401 unauthenticated.
	wantError(t, f.call(http.MethodGet, "/api/v1/me", ""), http.StatusUnauthorized, api.CodeUnauthenticated)
	wantError(t, f.call(http.MethodGet, "/api/v1/me", "", cookieValue(sessionCookieName, strings.Repeat("A", 43))),
		http.StatusUnauthorized, api.CodeUnauthenticated)
	wantError(t, f.call(http.MethodGet, "/api/v1/me", "", cookieValue("isshoni_session", cookie.Value)),
		http.StatusUnauthorized, api.CodeUnauthenticated)

	// logout: 204, the cookie cleared, this session gone and its connections closed with logged_out.
	f.signal.take()
	rec = f.call(http.MethodPost, "/api/v1/auth/logout", "{}", with(cookie), from(addrB))
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("logout: status %d, body %q", rec.Code, rec.Body)
	}
	if got := rec.Header().Values("Set-Cookie"); len(got) != 1 || got[0] != clearedCookie {
		t.Errorf("logout Set-Cookie = %q, want %q", got, clearedCookie)
	}
	wantCalls := []closedConn{{auth.ConnSelector{UserID: store.UserID(admin.ID), SessionID: store.SessionID(me.Session.ID)},
		auth.ReasonLoggedOut}}
	if calls := f.conns.take(); fmt.Sprint(calls) != fmt.Sprint(wantCalls) {
		t.Errorf("CloseConnections calls = %+v, want %+v", calls, wantCalls)
	}
	if calls := f.signal.take(); fmt.Sprint(calls) != "[notify [devices]]" {
		t.Errorf("Signal calls after a logout = %v", calls)
	}
	wantError(t, f.call(http.MethodGet, "/api/v1/me", "", with(cookie)), http.StatusUnauthorized, api.CodeUnauthenticated)
	// The setup session is another session: it still works.
	decodeBody[api.Me](t, f.call(http.MethodGet, "/api/v1/me", "", with(setupCookie)), http.StatusOK)

	// logout is idempotent: with the dead cookie, with none and with an empty body it answers the same, and closes
	// nothing.
	for name, opts := range map[string][]reqOpt{
		"dead cookie": {with(cookie)},
		"no cookie":   nil,
		"garbage":     {cookieValue(sessionCookieName, "garbage")},
	} {
		for _, body := range []string{"{}", ""} {
			rec := f.call(http.MethodPost, "/api/v1/auth/logout", body, opts...)
			if got := rec.Header().Values("Set-Cookie"); rec.Code != http.StatusNoContent || len(got) != 1 || got[0] != clearedCookie {
				t.Errorf("logout with %s and body %q: status %d, Set-Cookie %q", name, body, rec.Code, got)
			}
		}
	}
	if calls := f.conns.take(); len(calls) != 0 {
		t.Errorf("a logout without a session closed connections: %+v", calls)
	}

	// The audit rows of the walk, with the client IP.
	var got []string
	for _, row := range f.audit() {
		if strings.HasPrefix(row.Action, "auth.") {
			got = append(got, row.Action+" "+row.Actor.Name+" "+row.Actor.IP+" "+row.TargetKind)
		}
	}
	want := []string{"auth.login Alex " + addrB + " session", "auth.logout Alex " + addrB + " session"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("audit rows = %q, want %q", got, want)
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// fmt prints maps sorted, slices not: sort for a stable message.
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// TestMeForMember: a member sees no badges and may create invites only while membersCanInvite is on.
func TestMeForMember(t *testing.T) {
	f := newAuthFixture(t)
	admin, cookie := f.setupAdmin("Alex")
	// There is no REST path to a second account before the registration slice: turn the admin into a member behind
	// the service's back, and let the 30 s session cache run out.
	f.write(func(q *store.Q) error { return q.SetRole(store.UserID(admin.ID), store.RoleUser, f.clk.now()) })
	f.clk.advance(31 * time.Second)

	rec := f.call(http.MethodGet, "/api/v1/me", "", with(cookie))
	me := decodeBody[api.Me](t, rec, http.StatusOK)
	if me.User.Role != api.RoleUser || me.Permissions != (api.Permissions{}) || me.Badges != nil {
		t.Fatalf("a member's me = %+v (badges %+v)", me, me.Badges)
	}
	if strings.Contains(rec.Body.String(), "badges") {
		t.Errorf("a member's me has badges: %s", rec.Body)
	}
	if _, err := f.db.Settings().Update(context.Background(),
		map[string]json.RawMessage{"membersCanInvite": json.RawMessage("true")}, store.CLIActor); err != nil {
		t.Fatal(err)
	}
	me = decodeBody[api.Me](t, f.call(http.MethodGet, "/api/v1/me", "", with(cookie)), http.StatusOK)
	if me.Permissions != (api.Permissions{CreateInvites: true}) {
		t.Fatalf("with membersCanInvite: permissions %+v", me.Permissions)
	}

	// An account that is disabled, or a session that is gone, is 401 even inside the cache window: me reads fresh.
	f.write(func(q *store.Q) error { return q.SetStatus(store.UserID(admin.ID), store.StatusDisabled, f.clk.now()) })
	wantError(t, f.call(http.MethodGet, "/api/v1/me", "", with(cookie)), http.StatusUnauthorized, api.CodeUnauthenticated)
	f.write(func(q *store.Q) error {
		if err := q.SetStatus(store.UserID(admin.ID), store.StatusActive, f.clk.now()); err != nil {
			return err
		}
		_, err := q.DeleteSessions(store.UserID(admin.ID), "")
		return err
	})
	wantError(t, f.call(http.MethodGet, "/api/v1/me", "", with(cookie)), http.StatusUnauthorized, api.CodeUnauthenticated)
}

// TestLoginErrors: the login answers of 03 §12.3 #2.
func TestLoginErrors(t *testing.T) {
	f := newAuthFixture(t)
	admin, _ := f.setupAdmin("Alex")
	id := store.UserID(admin.ID)

	// Wrong password and unknown user: the same 401, byte for byte.
	wrong := f.login("Alex", "not the password", addrB)
	wantError(t, wrong, http.StatusUnauthorized, api.CodeInvalidCredentials)
	unknown := f.login("Nobody", testPassword, addrB)
	wantError(t, unknown, http.StatusUnauthorized, api.CodeInvalidCredentials)
	if wrong.Body.String() != unknown.Body.String() || len(setCookies(wrong))+len(setCookies(unknown)) != 0 {
		t.Errorf("wrong password %q and unknown user %q must look the same, without a cookie", wrong.Body, unknown.Body)
	}

	// Missing fields: 422 with a code per field.
	for body, want := range map[string]string{
		`{}`:                                  "map[password:required username:required]",
		`{"username":"Alex"}`:                 "map[password:required]",
		`{"password":"` + testPassword + `"}`: "map[username:required]",
		`{"username":"Alex","password":"` + strings.Repeat("x", 1025) + `"}`: "map[password:too_long]",
	} {
		e := wantError(t, f.call(http.MethodPost, "/api/v1/auth/login", body, from(addrC)),
			http.StatusUnprocessableEntity, api.CodeValidationFailed)
		if fmt.Sprint(e.Fields) != want {
			t.Errorf("login %.40s: fields %v, want %s", body, e.Fields, want)
		}
	}
	// A body that is not one JSON object.
	for _, body := range []string{``, `[]`, `{"username":1}`, `{"username":"Alex"} trailing`} {
		wantError(t, f.call(http.MethodPost, "/api/v1/auth/login", body, from(addrC)), http.StatusBadRequest, api.CodeBadRequest)
	}

	// An account that awaits approval or is disabled says so only after the correct password.
	for status, code := range map[store.UserStatus]string{
		store.StatusPending:  api.CodeAccountPending,
		store.StatusDisabled: api.CodeAccountDisabled,
	} {
		f.write(func(q *store.Q) error { return q.SetStatus(id, status, f.clk.now()) })
		rec := f.login("Alex", testPassword, addrB)
		wantError(t, rec, http.StatusForbidden, code)
		if len(setCookies(rec)) != 0 {
			t.Errorf("a %s account got a cookie", status)
		}
		wantError(t, f.login("Alex", "not the password", addrB), http.StatusUnauthorized, api.CodeInvalidCredentials)
	}
	f.write(func(q *store.Q) error { return q.SetStatus(id, store.StatusActive, f.clk.now()) })
	f.sessionCookie(f.login("Alex", testPassword, addrB), thirtyDays)

	// The audit holds a row per failed check, with its reason and never the name tried for an unknown account.
	var reasons []string
	for _, row := range f.audit() {
		if row.Action == "auth.login_failed" {
			reasons = append(reasons, fmt.Sprint(row.Detail["reason"]))
			if row.Actor.Kind != store.ActorAnonymous || row.Actor.IP != addrB || row.Actor.Name != "" {
				t.Errorf("auth.login_failed actor = %+v", row.Actor)
			}
			if strings.Contains(row.TargetName+fmt.Sprint(row.Detail), "Nobody") {
				t.Errorf("the audit row holds the name tried for an unknown account: %+v", row)
			}
		}
	}
	want := "[wrong_password unknown_user account_pending wrong_password account_disabled wrong_password]"
	if got := fmt.Sprint(reasons); got != want && got != "[wrong_password unknown_user account_disabled wrong_password account_pending wrong_password]" {
		t.Errorf("auth.login_failed reasons = %s", got)
	}
}

// TestLoginWithOldCookieOverREST is the 03 §7.7 row "login that arrives with an existing session cookie": the old
// session is deleted and ConnCloser gets that session with session_revoked.
func TestLoginWithOldCookieOverREST(t *testing.T) {
	f := newAuthFixture(t)
	admin, old := f.setupAdmin("Alex")
	oldMe := decodeBody[api.Me](t, f.call(http.MethodGet, "/api/v1/me", "", with(old)), http.StatusOK)
	f.conns.take()

	// A failed login keeps the old session.
	wantError(t, f.login("Alex", "not the password", addrA, with(old)), http.StatusUnauthorized, api.CodeInvalidCredentials)
	decodeBody[api.Me](t, f.call(http.MethodGet, "/api/v1/me", "", with(old)), http.StatusOK)
	if calls := f.conns.take(); len(calls) != 0 {
		t.Fatalf("a failed login closed connections: %+v", calls)
	}

	rec := f.login("Alex", testPassword, addrA, with(old))
	fresh := f.sessionCookie(rec, thirtyDays)
	decodeBody[api.UserResponse](t, rec, http.StatusOK)
	want := []closedConn{{auth.ConnSelector{UserID: store.UserID(admin.ID), SessionID: store.SessionID(oldMe.Session.ID)},
		auth.ReasonSessionRevoked}}
	if calls := f.conns.take(); fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("CloseConnections calls = %+v, want %+v", calls, want)
	}
	wantError(t, f.call(http.MethodGet, "/api/v1/me", "", with(old)), http.StatusUnauthorized, api.CodeUnauthenticated)
	me := decodeBody[api.Me](t, f.call(http.MethodGet, "/api/v1/me", "", with(fresh)), http.StatusOK)
	if me.Session.ID == oldMe.Session.ID {
		t.Error("the login kept the old session ID")
	}
	f.read(func(q *store.Q) error {
		sessions, err := q.ListSessions(store.UserID(admin.ID))
		if err == nil && len(sessions) != 1 {
			t.Errorf("%d sessions after the second login, want 1", len(sessions))
		}
		return err
	})
}

// TestLoginThrottlesOverREST is the REST face of 03 §15's login throttles (the service tests count the hashes): a
// blocked attempt gets 429 rate_limited with retryAfter and a Retry-After header.
func TestLoginThrottlesOverREST(t *testing.T) {
	f := newAuthFixture(t)
	f.setupAdmin("Alex")

	// 5 wrong passwords for one username from address A, then the 6th from A → 429 with Retry-After.
	for range 5 {
		wantError(t, f.login("Alex", "not the password", addrA), http.StatusUnauthorized, api.CodeInvalidCredentials)
	}
	rec := f.login("Alex", testPassword, addrA)
	e := wantError(t, rec, http.StatusTooManyRequests, api.CodeRateLimited)
	if e.RetryAfter != 120 || rec.Header().Get("Retry-After") != "120" || len(setCookies(rec)) != 0 {
		t.Errorf("blocked login: retryAfter %d, Retry-After %q, cookies %q; want 120 s and no cookie",
			e.RetryAfter, rec.Header().Get("Retry-After"), setCookies(rec))
	}
	if got, want := rec.Body.String(), `{"error":{"code":"rate_limited","retryAfter":120}}`+"\n"; got != want {
		t.Errorf("429 body = %q, want %q", got, want)
	}
	// The correct password from address B then → 200: a lockout from A doesn't block B.
	f.sessionCookie(f.login("Alex", testPassword, addrB), thirtyDays)

	// The IP bucket blocks the 21st attempt of an address, whatever it tries.
	for i := range 20 {
		wantError(t, f.login(fmt.Sprintf("nobody%d", i), testPassword, addrC), http.StatusUnauthorized, api.CodeInvalidCredentials)
	}
	rec = f.login("Alex", testPassword, addrC)
	e = wantError(t, rec, http.StatusTooManyRequests, api.CodeRateLimited)
	if e.RetryAfter != 15 || rec.Header().Get("Retry-After") != "15" {
		t.Errorf("auth-ip block: retryAfter %d, Retry-After %q; want 15", e.RetryAfter, rec.Header().Get("Retry-After"))
	}
	wantError(t, f.call(http.MethodPost, "/api/v1/auth/setup/check", `{"token":"x"}`, from(addrC)),
		http.StatusTooManyRequests, api.CodeRateLimited)
	// Two addresses of one IPv6 /64 are one client.
	for i := range 20 {
		wantError(t, f.login(fmt.Sprintf("nobody%d", i), testPassword, fmt.Sprintf("2001:db8:1:2::%x", i+1)),
			http.StatusUnauthorized, api.CodeInvalidCredentials)
	}
	wantError(t, f.login("Alex", testPassword, "2001:db8:1:2:ffff::1"), http.StatusTooManyRequests, api.CodeRateLimited)
	// Time gives the tokens back: A's lockout for this username ends after 2 minutes.
	f.clk.advance(2 * time.Minute)
	f.sessionCookie(f.login("Alex", testPassword, addrA), thirtyDays)

	// One auth.throttled row per address that hit auth-ip, and none for the username-and-address block.
	var throttled []string
	for _, row := range f.audit() {
		if row.Action == "auth.throttled" {
			throttled = append(throttled, fmt.Sprint(row.Detail))
		}
	}
	want := "[map[key:" + addrC + " scope:ip] map[key:2001:db8:1:2::/64 scope:ip]]"
	if fmt.Sprint(throttled) != want {
		t.Errorf("auth.throttled rows = %v, want %s", throttled, want)
	}
}

// TestHashBudgetOverREST: with the auth-hash bucket empty, a login → 503 server_busy with Retry-After. The budget
// also proves, from the outside, that a throttled attempt never reaches the hash: every hash takes a budget token
// right before it, and the blocked attempts take none.
func TestHashBudgetOverREST(t *testing.T) {
	f := newAuthFixture(t, func(o *auth.Options) { o.Hashes = auth.HashBudget{Burst: 9, PerSecond: 1} })
	f.setupAdmin("Alex") // hash 1 of the budget of 9 (the clock stands still)

	for range 5 { // hashes 2–6
		wantError(t, f.login("Alex", "not the password", addrA), http.StatusUnauthorized, api.CodeInvalidCredentials)
	}
	for range 10 { // blocked by auth-user-ip: no hash, no budget
		wantError(t, f.login("Alex", testPassword, addrA), http.StatusTooManyRequests, api.CodeRateLimited)
	}
	for i := range 3 { // hashes 7–9: exactly what the budget has left
		wantError(t, f.login("Nobody", testPassword, fmt.Sprintf("192.0.2.%d", i+1)), http.StatusUnauthorized,
			api.CodeInvalidCredentials)
	}
	rec := f.login("Alex", testPassword, addrB)
	e := wantError(t, rec, http.StatusServiceUnavailable, api.CodeServerBusy)
	if e.RetryAfter != 1 || rec.Header().Get("Retry-After") != "1" || len(setCookies(rec)) != 0 {
		t.Errorf("empty hash budget: retryAfter %d, Retry-After %q", e.RetryAfter, rec.Header().Get("Retry-After"))
	}
	f.clk.advance(time.Second)
	f.sessionCookie(f.login("Alex", testPassword, addrB), thirtyDays)
}

// TestSessionRotationOverREST is 03 §15's rotation case through the chain: after 24 h a REST request rotates, and
// the old token works for 60 s.
func TestSessionRotationOverREST(t *testing.T) {
	f := newAuthFixture(t)
	admin, old := f.setupAdmin("Alex")
	me := func(c *http.Cookie) *httptest.ResponseRecorder {
		return f.call(http.MethodGet, "/api/v1/me", "", with(c))
	}

	f.clk.advance(24*time.Hour - time.Second)
	if rec := me(old); rec.Code != http.StatusOK || len(setCookies(rec)) != 0 {
		t.Fatalf("23:59:59 after the login: status %d, cookies %q", rec.Code, setCookies(rec))
	}
	f.clk.advance(time.Second)
	// A Public route does not rotate, with or without a cookie; neither does a logout elsewhere.
	if rec := f.call(http.MethodGet, "/api/v1/info", "", with(old)); len(setCookies(rec)) != 0 {
		t.Fatal("GET /info rotated the session")
	}

	rec := me(old)
	before := decodeBody[api.Me](t, rec, http.StatusOK)
	fresh := f.sessionCookie(rec, thirtyDays)
	if fresh.Value == old.Value {
		t.Fatal("the rotation kept the token")
	}
	// The session ID stays; both tokens work, and neither rotates again.
	for name, c := range map[string]*http.Cookie{"old": old, "new": fresh} {
		rec := me(c)
		got := decodeBody[api.Me](t, rec, http.StatusOK)
		if got.Session.ID != before.Session.ID || got.User.ID != admin.ID || len(setCookies(rec)) != 0 {
			t.Errorf("the %s token after the rotation: session %s, cookies %q", name, got.Session.ID, setCookies(rec))
		}
	}
	f.clk.advance(60 * time.Second)
	if rec := me(old); rec.Code != http.StatusOK {
		t.Fatalf("the old token 60 s after the rotation: status %d", rec.Code)
	}
	f.clk.advance(time.Second)
	wantError(t, me(old), http.StatusUnauthorized, api.CodeUnauthenticated)
	if rec := me(fresh); rec.Code != http.StatusOK || len(setCookies(rec)) != 0 {
		t.Fatalf("the new token after the grace: status %d, cookies %q", rec.Code, setCookies(rec))
	}
	if calls := f.conns.take(); len(calls) != 0 {
		t.Errorf("a rotation closed connections: %+v", calls)
	}
}

// TestRealServiceChain runs the cross-cutting rules of 03 §15 against the real auth.Service. S24 left Service.CSRF
// as a stub that answered 500 to every routed request; now the guard of 03 §7.5 is behind it.
func TestRealServiceChain(t *testing.T) {
	f := newAuthFixture(t)
	_, cookie := f.setupAdmin("Alex")
	login := jsonBody(t, api.LoginRequest{Username: "Alex", Password: testPassword})

	// Routed requests reach their handlers.
	decodeBody[api.Info](t, f.call(http.MethodGet, "/api/v1/info", ""), http.StatusOK)
	decodeBody[api.Me](t, f.call(http.MethodGet, "/api/v1/me", "", with(cookie)), http.StatusOK)

	// A cross-site POST with a cookie → 403 csrf_failed, and nothing happened.
	for name, opts := range map[string][]reqOpt{
		"cross-site":                 {header("Sec-Fetch-Site", "cross-site"), header("Origin", "https://evil.example")},
		"same-site":                  {header("Sec-Fetch-Site", "same-site"), header("Origin", "https://other.example.com")},
		"Origin of another host":     {header("Sec-Fetch-Site", ""), header("Origin", "https://evil.example")},
		"cross-site without Origin":  {header("Sec-Fetch-Site", "cross-site"), header("Origin", "")},
		"cross-site with JSON, null": {header("Sec-Fetch-Site", "cross-site"), header("Origin", "null")},
	} {
		for _, path := range []string{"/api/v1/auth/login", "/api/v1/auth/logout", "/api/v1/auth/setup/check"} {
			rec := f.call(http.MethodPost, path, login, append([]reqOpt{with(cookie)}, opts...)...)
			wantError(t, rec, http.StatusForbidden, api.CodeCSRFFailed)
			if len(rec.Header().Values("Set-Cookie")) != 0 {
				t.Errorf("%s POST %s set a cookie", name, path)
			}
		}
	}
	decodeBody[api.Me](t, f.call(http.MethodGet, "/api/v1/me", "", with(cookie)), http.StatusOK) // not logged out

	// A POST without a JSON Content-Type → 415.
	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded", "application/json; charset=iso-8859-1"} {
		for _, path := range []string{"/api/v1/auth/login", "/api/v1/auth/logout"} {
			wantError(t, f.call(http.MethodPost, path, login, with(cookie), header("Content-Type", ct)),
				http.StatusUnsupportedMediaType, api.CodeUnsupportedMediaType)
		}
	}
	// A non-browser client (no Sec-Fetch-Site, no Origin) passes with JSON.
	rec := f.call(http.MethodPost, "/api/v1/auth/login", login, header("Sec-Fetch-Site", ""), header("Origin", ""))
	decodeBody[api.UserResponse](t, rec, http.StatusOK)

	// An Authorization header in M1 → 401, on every route and before anything else.
	bearer := header("Authorization", "Bearer isa_"+strings.Repeat("A", 43))
	wantError(t, f.call(http.MethodGet, "/api/v1/me", "", with(cookie), bearer), http.StatusUnauthorized, api.CodeUnauthenticated)
	wantError(t, f.call(http.MethodGet, "/api/v1/info", "", bearer), http.StatusUnauthorized, api.CodeUnauthenticated)
	wantError(t, f.call(http.MethodPost, "/api/v1/auth/login", login, bearer), http.StatusUnauthorized, api.CodeUnauthenticated)
	wantError(t, f.call(http.MethodPost, "/api/v1/auth/logout", "{}", with(cookie), bearer), http.StatusUnauthorized,
		api.CodeUnauthenticated)
	decodeBody[api.Me](t, f.call(http.MethodGet, "/api/v1/me", "", with(cookie)), http.StatusOK) // not logged out

	// Unknown paths and wrong methods of the new routes answer in JSON.
	wantError(t, f.call(http.MethodPost, "/api/v1/auth/nope", "{}"), http.StatusNotFound, api.CodeNotFound)
	for path, allow := range map[string]string{
		"/api/v1/auth/login":          "POST",
		"/api/v1/auth/logout":         "POST",
		"/api/v1/auth/setup/check":    "POST",
		"/api/v1/auth/setup/complete": "POST",
	} {
		rec := f.call(http.MethodGet, path, "")
		wantError(t, rec, http.StatusMethodNotAllowed, api.CodeMethodNotAllowed)
		if got := rec.Header().Get("Allow"); got != allow {
			t.Errorf("GET %s: Allow %q, want %q", path, got, allow)
		}
	}
	rec = f.call(http.MethodPost, "/api/v1/me", "{}", with(cookie))
	wantError(t, rec, http.StatusMethodNotAllowed, api.CodeMethodNotAllowed)
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("POST /me: Allow %q, want GET, HEAD", got)
	}
}

// TestGETsDoNotChangeTheDB is 03 §15's "no GET changes the DB": every GET route runs with a fresh session (so the
// 5-minute touch and the daily rotation are not due), and PRAGMA data_version, which changes whenever another
// connection commits, stays the same. The later slices add their GET routes to the list.
func TestGETsDoNotChangeTheDB(t *testing.T) {
	f := newAuthFixture(t)
	_, cookie := f.setupAdmin("Alex")
	raw := f.rawDB()
	version := func() int {
		var v int
		if err := raw.QueryRowContext(context.Background(), "PRAGMA data_version").Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	before := version()
	for _, path := range []string{"/api/v1/info", "/api/v1/me", "/api/v1/nope"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			for _, session := range []reqOpt{with(cookie), with(nil), cookieValue(sessionCookieName, strings.Repeat("A", 43))} {
				f.call(method, path, "", session)
			}
		}
	}
	if after := version(); after != before {
		t.Fatalf("PRAGMA data_version went from %d to %d: a GET wrote to the database", before, after)
	}
	// The check sees a write when there is one.
	f.call(http.MethodPost, "/api/v1/auth/logout", "{}", with(cookie))
	if after := version(); after == before {
		t.Fatal("PRAGMA data_version did not move after a logout: the check is blind")
	}
}
