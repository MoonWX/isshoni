package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// register calls Register and returns only the error.
func (e *svcEnv) register(inviteToken, username, password, ip string) error {
	_, err := e.svc.Register(context.Background(),
		RegisterInput{InviteToken: inviteToken, Username: username, Password: password}, meta(ip))
	return err
}

// userByName reads a user by any spelling of its name, or nil.
func (e *svcEnv) userByName(name string) *store.User {
	e.t.Helper()
	key, _ := loginKey(name)
	var out *store.User
	e.read(func(q *store.Q) error {
		u, err := q.UserByUsernameKey(key)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		out = &u
		return err
	})
	return out
}

// docIP is the i-th address of a documentation range: enough distinct clients for a test that must stay clear of
// the auth-ip bucket.
func docIP(i int) string { return fmt.Sprintf("198.51.100.%d", 1+i%250) }

func TestRegisterWithInvite(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	alex := e.addAdmin("Alex")
	sam := e.addUser("Sam")
	samSession := e.login("Sam", ipB)
	inv, tok := e.invite(actorFor(alex, ipA), InviteInput{MaxUses: 3})
	e.conns.take()
	e.hashes.Store(0)

	// The invite's state is judged before the fields, and the fields before the name is looked up.
	wantCode(t, e.register(newToken(inviteTokenBytes), "", "", docIP(0)), api.CodeInviteInvalid)
	wantCode(t, e.register("garbage", "Sam", testPassword, docIP(0)), api.CodeInviteInvalid)
	for _, tc := range []struct {
		username, password string
		want               map[string]string
	}{
		{"", "", map[string]string{"username": "required", "password": "required"}},
		{"a", "short", map[string]string{"username": "too_short", "password": "too_short"}},
		{"system", testPassword, map[string]string{"username": "reserved"}},
		{"-kim", testPassword, map[string]string{"username": "invalid"}},
		{"Kim", "PassWord1", map[string]string{"password": "too_common"}},
		{"kimberly", "Ｋimberly", map[string]string{"password": "same_as_username"}},
		{"Kim", "1234567", map[string]string{"password": "too_short"}}, // the minimum is 8 (owner decision)
		{"Sam", "1234567", map[string]string{"password": "too_short"}}, // a taken name with a bad field: the field
	} {
		err := e.register(tok, tc.username, tc.password, docIP(1))
		if ae := wantCode(t, err, api.CodeValidationFailed); fmt.Sprint(ae.Fields) != fmt.Sprint(tc.want) {
			t.Errorf("Register(%q, %q): fields %v, want %v", tc.username, tc.password, ae.Fields, tc.want)
		}
	}
	// A taken name, in any spelling: 409, and the invite is not used.
	for _, name := range []string{"Sam", "SAM", " ｓａｍ ", "alex"} {
		wantCode(t, e.register(tok, name, testPassword, docIP(2)), api.CodeUsernameTaken)
	}
	if e.hashes.Load() != 0 {
		t.Fatalf("refused registrations hashed %d times", e.hashes.Load())
	}
	if got := e.inviteRow(inv.ID); got.Uses != 0 {
		t.Fatalf("refused registrations used the invite %d times", got.Uses)
	}
	if e.rawCount("users") != 2 || len(e.conns.take()) != 0 || e.auditCount(auditUserRegistered) != 0 {
		t.Fatal("a refused registration left something behind")
	}

	// The real thing, from a browser that still has Sam's session cookie.
	e.advance(time.Minute)
	now := e.clk.now()
	m := meta(ipC)
	m.SessionToken = samSession.Token
	res, err := e.svc.Register(ctx, RegisterInput{InviteToken: tok, Username: " 太郎 ", Password: testPassword}, m)
	if err != nil {
		t.Fatal(err)
	}
	if res.Pending || res.Login == nil {
		t.Fatalf("Register with an invite = %+v, want an active account with its session", res)
	}
	if n := e.hashes.Load(); n != 1 {
		t.Errorf("the registration hashed %d times, want 1", n)
	}
	var taro store.User
	e.read(func(q *store.Q) error {
		var err error
		taro, err = q.UserByID(res.Login.User.ID)
		return err
	})
	if taro.Username != "太郎" || taro.Role != store.RoleUser || taro.Status != store.StatusActive ||
		taro.CreatedVia != "invite" || taro.InviteID != inv.ID || !taro.CreatedAt.Equal(now) ||
		!taro.PasswordChangedAt.Equal(now) || !taro.LastLoginAt.Equal(now) || taro.PasswordHash == "" ||
		strings.Contains(taro.PasswordHash, testPassword) || taro.ApprovedBy != "" || !taro.ApprovedAt.IsZero() {
		t.Errorf("the new user's row = %+v", taro)
	}
	if res.Login.User.Username != "太郎" || res.Login.User.ID != taro.ID || !tokenWellFormed(res.Login.Token, sessionTokenBytes) {
		t.Errorf("LoginResult = %+v", res.Login)
	}
	s := e.session(taro.ID, res.Login.Session.ID)
	if s == nil || s.Name != "Chrome on Windows" || s.LastIP != ipC || !s.IdleExpiresAt.Equal(now.Add(sessionIdleTTL)) {
		t.Errorf("the new user's session = %+v", s)
	}
	if got := e.inviteRow(inv.ID); got.Uses != 1 {
		t.Errorf("the invite has %d uses, want 1", got.Uses)
	}
	rows := e.audit(auditUserRegistered)
	if len(rows) != 1 {
		t.Fatalf("%d user.registered rows, want 1", len(rows))
	}
	if r := rows[0]; r.Actor != (store.Actor{Kind: store.ActorUser, UserID: taro.ID, Name: "太郎", IP: ipC}) ||
		r.TargetKind != "user" || r.TargetID != string(taro.ID) || r.TargetName != "太郎" || !r.At.Equal(now) ||
		fmt.Sprint(r.Detail) != "map[inviteId:"+string(inv.ID)+"]" {
		t.Errorf("user.registered row = %+v", r)
	}
	// The 03 §7.7 row: the session of the cookie the request arrived with is gone, closed with session_revoked.
	if e.session(sam.ID, samSession.Session.ID) != nil {
		t.Error("the old cookie's session survived the registration")
	}
	want := []closedConn{{ConnSelector{UserID: sam.ID, SessionID: samSession.Session.ID}, ReasonSessionRevoked}}
	if calls := e.conns.take(); fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("CloseConnections calls = %+v, want %+v", calls, want)
	}
	// The session works, and so does the password.
	if p, err := e.svc.Authenticate(e.request("GET", res.Login.Token)); err != nil || p.UserID != taro.ID || p.IsAdmin() {
		t.Errorf("the registration's session: %+v, %v", p, err)
	}
	if got := e.login("太郎", ipC); got.User.ID != taro.ID {
		t.Error("the new user cannot log in")
	}
	// The invite page now counts one use less, and the invite lists who used it.
	if info, err := e.svc.CheckInvite(ctx, tok, meta(docIP(3))); err != nil || info.UsesLeft != 2 {
		t.Errorf("CheckInvite after a registration = %+v, %v", info, err)
	}
	e.read(func(q *store.Q) error {
		list, err := q.ListInvites("", false)
		if err == nil && (len(list) != 1 || len(list[0].RedeemedBy) != 1 || list[0].RedeemedBy[0].ID != taro.ID) {
			t.Errorf("ListInvites = %+v, want the invite with its one redeemer", list)
		}
		return err
	})
	if len(e.alerts.take()) != 0 {
		t.Error("a registration with an invite raised an admin alert")
	}

	// A password of exactly 8 characters is long enough (the owner's minimum, 03 §19).
	const eight = "hT4!rz9Q"
	if _, err := e.svc.Register(ctx, RegisterInput{InviteToken: tok, Username: "Kim", Password: eight}, meta(docIP(10))); err != nil {
		t.Fatalf("a registration with an 8-character password: %v", err)
	}
	if _, err := e.svc.Login(ctx, "Kim", eight, meta(docIP(10))); err != nil {
		t.Fatalf("a login with the 8-character password: %v", err)
	}
	// It runs out: the 4th registration gets invite_used_up, and no hash.
	if err := e.register(tok, "Lee", testPassword, docIP(11)); err != nil {
		t.Fatalf("registration 3: %v", err)
	}
	e.hashes.Store(0)
	wantCode(t, e.register(tok, "Max", testPassword, docIP(20)), api.CodeInviteUsedUp)
	if e.hashes.Load() != 0 || e.userByName("Max") != nil {
		t.Error("a used-up invite cost a hash or created a user")
	}
}

// TestRegisterModes is the mode table of 03 §7.9.
func TestRegisterModes(t *testing.T) {
	e := newSvcEnv(t)
	alex := e.addAdmin("Alex")
	_, tok := e.invite(actorFor(alex, ipA), InviteInput{})
	e.hashes.Store(0)

	// invite (the default): links work, a public sign-up is off.
	wantCode(t, e.register("", "Sam", testPassword, docIP(0)), api.CodeInviteRequired)
	if e.hashes.Load() != 0 || e.userByName("Sam") != nil {
		t.Fatal("a sign-up in the invite mode cost a hash or created a user")
	}
	if err := e.register(tok, "Sam", testPassword, docIP(1)); err != nil {
		t.Fatal(err)
	}

	// closed: both are refused, before the invite and the fields are looked at.
	e.set("registrationMode", "closed")
	e.hashes.Store(0)
	for _, invite := range []string{tok, "", "garbage"} {
		wantCode(t, e.register(invite, "Kim", testPassword, docIP(2)), api.CodeRegistrationClosed)
		wantCode(t, e.register(invite, "", "", docIP(3)), api.CodeRegistrationClosed)
	}
	if e.hashes.Load() != 0 || e.userByName("Kim") != nil {
		t.Fatal("the closed mode cost a hash or created a user")
	}

	// approval: links work and the account is active at once; without one the sign-up is pending.
	e.set("registrationMode", "approval")
	res, err := e.svc.Register(context.Background(), RegisterInput{InviteToken: tok, Username: "Kim", Password: testPassword},
		meta(docIP(4)))
	if err != nil || res.Pending || res.Login == nil || res.Login.User.Status != store.StatusActive {
		t.Fatalf("Register with an invite in the approval mode = %+v, %v", res, err)
	}
	res, err = e.svc.Register(context.Background(), RegisterInput{Username: "Lee", Password: testPassword}, meta(docIP(5)))
	if err != nil || !res.Pending || res.Login != nil {
		t.Fatalf("Register without an invite in the approval mode = %+v, %v", res, err)
	}
	// A bad invite is not a sign-up: the link's error comes back.
	wantCode(t, e.register("garbage", "Max", testPassword, docIP(6)), api.CodeInviteInvalid)
	if e.userByName("Max") != nil {
		t.Error("a bad invite in the approval mode became a sign-up")
	}
}

// TestRegisterRechecksInWrite: what changes between the checks and the Write, while the password is hashed, still
// counts (03 §7.9 step 6).
func TestRegisterRechecksInWrite(t *testing.T) {
	for _, tc := range []struct {
		name     string
		invite   bool
		approval bool
		change   func(e *svcEnv, inv store.Invite)
		code     string
	}{
		{"the invite is revoked", true, false, func(e *svcEnv, inv store.Invite) {
			e.write(func(q *store.Q) error { return q.RevokeInvite(inv.ID, "", e.clk.now()) })
		}, api.CodeInviteRevoked},
		{"the invite expires", true, false, func(e *svcEnv, _ store.Invite) { e.advance(2 * time.Hour) }, api.CodeInviteExpired},
		{"the invite is used up", true, false, func(e *svcEnv, inv store.Invite) {
			e.write(func(q *store.Q) error { return q.UseInvite(inv.ID, e.clk.now()) })
		}, api.CodeInviteUsedUp},
		{"the name is taken", true, false, func(e *svcEnv, _ store.Invite) { e.addUser("kim") }, api.CodeUsernameTaken},
		{"the mode closes", true, false, func(e *svcEnv, _ store.Invite) { e.set("registrationMode", "closed") },
			api.CodeRegistrationClosed},
		{"a sign-up's name is taken", false, true, func(e *svcEnv, _ store.Invite) { e.addUser("kim") },
			api.CodeUsernameTaken},
		{"a sign-up's mode goes back to invite", false, true, func(e *svcEnv, _ store.Invite) {
			e.set("registrationMode", "invite")
		}, api.CodeInviteRequired},
		{"the queue fills up", false, true, func(e *svcEnv, _ store.Invite) {
			for i := range maxPendingSignups {
				e.addUser(fmt.Sprintf("pending%d", i), func(u *store.User) { u.Status = store.StatusPending })
			}
		}, api.CodeLimitReached},
	} {
		e := newSvcEnv(t)
		var inv store.Invite
		tok := ""
		if tc.invite {
			inv, tok = e.invite(store.CLIActor, InviteInput{MaxUses: 1, ExpiresInHours: 1})
		}
		if tc.approval {
			e.set("registrationMode", "approval")
		}
		// The hook runs while the password is hashed: after Register's checks, before its Write. Only this test's
		// goroutine hashes, and a change that hashes itself (addUser) must not start the hook again.
		fired := false
		e.svc.hasher.derive = func(pw, salt []byte, tm, mem uint32, th uint8, kl uint32) []byte {
			if !fired {
				fired = true
				tc.change(e, inv)
			}
			return argon2.IDKey(pw, salt, tm, mem, th, kl)
		}
		wantCode(t, e.register(tok, "Kim", testPassword, ipA), tc.code)
		if !fired {
			t.Fatalf("%s: the registration was refused before its hash", tc.name)
		}
		// The "name is taken" cases add a user kim themselves, through addUser: an active user without a session.
		if u := e.userByName("Kim"); (u != nil && tc.code != api.CodeUsernameTaken) || e.rawCount("sessions") != 0 {
			t.Errorf("%s between the checks and the write: an account or a session was created", tc.name)
		}
		if n := e.auditCount(auditUserRegistered) + e.auditCount(auditSignupRequested); n != 0 {
			t.Errorf("%s between the checks and the write: %d audit rows", tc.name, n)
		}
		if tc.invite && tc.code == api.CodeUsernameTaken {
			if got := e.inviteRow(inv.ID); got.Uses != 0 {
				t.Errorf("%s: the invite was used although nobody registered", tc.name)
			}
		}
		if len(e.alerts.take()) != 0 {
			t.Errorf("%s: a refused sign-up raised an alert", tc.name)
		}
	}
}

// TestRegisterParallel: registrations that race for the last uses of an invite never pass maxUses (03 §7.9), and
// registrations that race for one name leave one account, with one use counted.
func TestRegisterParallel(t *testing.T) {
	e := newSvcEnv(t)
	inv, tok := e.invite(store.CLIActor, InviteInput{MaxUses: 5})
	const n = 16
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			errs[i] = e.register(tok, fmt.Sprintf("friend%d", i), testPassword, docIP(i))
		})
	}
	close(start)
	wg.Wait()
	ok := 0
	for i, err := range errs {
		switch {
		case err == nil:
			ok++
		case !api.IsCode(err, api.CodeInviteUsedUp):
			t.Errorf("registration %d: %v, want success or invite_used_up", i, err)
		}
	}
	if got := e.inviteRow(inv.ID); ok != 5 || got.Uses != 5 || e.rawCount("users") != 5 || e.rawCount("sessions") != 5 {
		t.Fatalf("%d registrations succeeded, the invite has %d uses, %d users, %d sessions; want 5 of each",
			ok, got.Uses, e.rawCount("users"), e.rawCount("sessions"))
	}

	// One name, many requests: one account.
	inv, tok = e.invite(store.CLIActor, InviteInput{MaxUses: 10})
	start = make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			errs[i] = e.register(tok, "Taro", testPassword, docIP(100+i))
		})
	}
	close(start)
	wg.Wait()
	ok = 0
	for i, err := range errs {
		switch {
		case err == nil:
			ok++
		case !api.IsCode(err, api.CodeUsernameTaken):
			t.Errorf("registration %d of one name: %v, want success or username_taken", i, err)
		}
	}
	if got := e.inviteRow(inv.ID); ok != 1 || got.Uses != 1 {
		t.Fatalf("%d registrations of one name succeeded with %d uses counted, want 1 and 1", ok, got.Uses)
	}
}

// TestSignupRequest is the approval mode's sign-up (03 §7.9): a pending user, no session, the audit row with the
// IP, and the admin alert.
func TestSignupRequest(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	alex := e.addAdmin("Alex")
	e.set("registrationMode", "approval")
	e.alerts.take()
	e.hashes.Store(0)
	now := e.clk.now()

	m := meta(ipB)
	m.SessionToken = e.login("Alex", ipA).Token // a signed-in browser asks for another account
	e.hashes.Store(0)
	e.conns.take()
	res, err := e.svc.Register(ctx, RegisterInput{Username: "sam_k", Password: testPassword}, m)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Pending || res.Login != nil {
		t.Fatalf("Register without an invite = %+v, want a pending sign-up", res)
	}
	if n := e.hashes.Load(); n != 1 {
		t.Errorf("the sign-up hashed %d times, want 1", n)
	}
	sam := e.userByName("sam_k")
	if sam == nil || sam.Status != store.StatusPending || sam.Role != store.RoleUser || sam.CreatedVia != "signup" ||
		sam.InviteID != "" || sam.PasswordHash == "" || !sam.CreatedAt.Equal(now) || !sam.LastLoginAt.IsZero() {
		t.Fatalf("the pending user's row = %+v", sam)
	}
	// No session for the pending account, and the browser's own session stays.
	if n := e.rawCount("sessions"); n != 1 || len(e.conns.take()) != 0 {
		t.Errorf("%d sessions after a sign-up (the admin's own is one), or a closed connection", n)
	}
	rows := e.audit(auditSignupRequested)
	if len(rows) != 1 {
		t.Fatalf("%d user.signup_requested rows, want 1", len(rows))
	}
	if r := rows[0]; r.Actor != (store.Actor{Kind: store.ActorAnonymous, Name: "sam_k", IP: ipB}) ||
		r.TargetKind != "user" || r.TargetID != string(sam.ID) || r.TargetName != "sam_k" || len(r.Detail) != 0 {
		t.Errorf("user.signup_requested row = %+v", r)
	}
	// The approval queue reads the sign-up's IP from that row.
	e.read(func(q *store.Q) error {
		pending, err := q.ListUsers(store.StatusPending)
		if err == nil && (len(pending) != 1 || pending[0].SignupIP != ipB || pending[0].ID != sam.ID) {
			t.Errorf("pending users = %+v, want sam_k with its sign-up IP", pending)
		}
		return err
	})
	// The alert reaches the alerter: one, with the name as its target.
	alerts := e.alerts.take()
	if len(alerts) != 1 || alerts[0].Kind != AlertSignupPending || alerts[0].Actor != "system" ||
		alerts[0].Target != "sam_k" || !alerts[0].At.Equal(now) {
		t.Fatalf("alerts = %+v, want one signup_pending for sam_k", alerts)
	}
	if !strings.Contains(e.logs.String(), "kind=signup_pending") || strings.Contains(e.logs.String(), "sam_k") {
		t.Error("the alert's WARN line is missing, or it names the user")
	}

	// A pending account cannot log in: 403 account_pending after the correct password, 401 after a wrong one.
	wantCode(t, e.tryLogin("sam_k", testPassword, ipB), api.CodeAccountPending)
	wantCode(t, e.tryLogin("sam_k", "not the password", ipB), api.CodeInvalidCredentials)

	// Its name is taken, for sign-ups and for invites.
	wantCode(t, e.register("", "SAM_K", testPassword, ipB), api.CodeUsernameTaken)
	_, tok := e.invite(actorFor(alex, ipA), InviteInput{})
	wantCode(t, e.register(tok, "Sam_K", testPassword, ipC), api.CodeUsernameTaken)

	// Field errors, as everywhere; they cost no hash.
	e.hashes.Store(0)
	ae := wantCode(t, e.register("", "x", "short", ipB), api.CodeValidationFailed)
	if fmt.Sprint(ae.Fields) != "map[password:too_short username:too_short]" {
		t.Errorf("fields = %v", ae.Fields)
	}
	if e.hashes.Load() != 0 {
		t.Error("a refused sign-up hashed")
	}
}

// TestSignupAlertCoalesced: the signup_pending alert goes out at most once per 10 minutes (03 §7.9, §15).
func TestSignupAlertCoalesced(t *testing.T) {
	e := newSvcEnv(t)
	e.set("registrationMode", "approval")
	signup := func(i int) {
		t.Helper()
		if err := e.register("", fmt.Sprintf("friend%d", i), testPassword, docIP(i)); err != nil {
			t.Fatalf("sign-up %d: %v", i, err)
		}
	}
	signup(0)
	signup(1)
	e.advance(10*time.Minute - time.Second)
	signup(2)
	if alerts := e.alerts.take(); len(alerts) != 1 || alerts[0].Target != "friend0" {
		t.Fatalf("alerts for 3 sign-ups within 10 minutes = %+v, want one, for the first", alerts)
	}
	e.advance(time.Second)
	signup(3)
	signup(4)
	if alerts := e.alerts.take(); len(alerts) != 1 || alerts[0].Kind != AlertSignupPending || alerts[0].Target != "friend3" {
		t.Fatalf("alerts after 10 minutes = %+v, want one more, for the next sign-up", alerts)
	}
	// A refused sign-up raises none, and does not use up the next alert.
	e.advance(10 * time.Minute)
	wantCode(t, e.register("", "friend0", testPassword, docIP(50)), api.CodeUsernameTaken)
	if alerts := e.alerts.take(); len(alerts) != 0 {
		t.Fatalf("a refused sign-up raised %+v", alerts)
	}
	signup(5)
	if alerts := e.alerts.take(); len(alerts) != 1 {
		t.Fatalf("alerts = %+v, want the one of the sign-up after a refused one", alerts)
	}
	if n := e.auditCount(auditSignupRequested); n != 6 {
		t.Errorf("%d user.signup_requested rows, want 6: every sign-up is audited", n)
	}
}

// TestSignupWithoutAlerter: without an alerter the alert is only logged.
func TestSignupWithoutAlerter(t *testing.T) {
	e := newSvcEnv(t, func(o *Options) { o.Alerts = nil })
	e.set("registrationMode", "approval")
	if err := e.register("", "Sam", testPassword, ipA); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.logs.String(), "kind=signup_pending") {
		t.Error("the alert was not logged")
	}
}

// TestSignupQueueLimit: at most 50 pending sign-ups (03 §7.9, §14).
func TestSignupQueueLimit(t *testing.T) {
	e := newSvcEnv(t)
	e.set("registrationMode", "approval")
	for i := range maxPendingSignups {
		if err := e.register("", fmt.Sprintf("friend%d", i), testPassword, docIP(i)); err != nil {
			t.Fatalf("sign-up %d: %v", i+1, err)
		}
	}
	e.hashes.Store(0)
	ae := wantCode(t, e.register("", "onemore", testPassword, docIP(60)), api.CodeLimitReached)
	if fmt.Sprint(ae.Params) != "map[limit:pending_signups]" {
		t.Errorf("limit_reached params = %v", ae.Params)
	}
	// A full queue answers before the name is looked up, and costs no hash.
	wantCode(t, e.register("", "friend0", testPassword, docIP(61)), api.CodeLimitReached)
	if e.hashes.Load() != 0 || e.userByName("onemore") != nil {
		t.Error("a sign-up beyond the limit cost a hash or created a user")
	}
	// A registration with an invite is not a sign-up: it does not count against the queue.
	_, tok := e.invite(store.CLIActor, InviteInput{})
	if err := e.register(tok, "invited", testPassword, docIP(62)); err != nil {
		t.Errorf("a registration with an invite while the queue is full: %v", err)
	}
	// One decision makes room for one more.
	if err := e.svc.Reject(context.Background(), store.CLIActor, e.userByName("friend7").ID); err != nil {
		t.Fatal(err)
	}
	if err := e.register("", "onemore", testPassword, docIP(63)); err != nil {
		t.Errorf("a sign-up after a rejection: %v", err)
	}
}

// TestRegisterThrottles: the buckets of a registration in their order (03 §7.3, §7.9 step 1).
func TestRegisterThrottles(t *testing.T) {
	e := newSvcEnv(t)
	e.set("registrationMode", "approval")
	_, tok := e.invite(store.CLIActor, InviteInput{MaxUses: 100})

	// register-ip: 5 sign-ups without an invite per address, whatever their outcome; then one per 12 minutes.
	if err := e.register("", "friend0", testPassword, ipA); err != nil {
		t.Fatal(err)
	}
	wantCode(t, e.register("", "friend0", testPassword, ipA), api.CodeUsernameTaken) // probing a name counts
	wantCode(t, e.register("", "", "", ipA), api.CodeValidationFailed)
	wantCode(t, e.register("", "friend0", testPassword, ipA), api.CodeUsernameTaken)
	if err := e.register("", "friend1", testPassword, ipA); err != nil {
		t.Fatal(err)
	}
	e.hashes.Store(0)
	for range 2 {
		ae := wantCode(t, e.register("", "friend2", testPassword, ipA), api.CodeRateLimited)
		if ae.RetryAfter != 12*60 {
			t.Errorf("retryAfter = %d s, want 720 (one token per 12 min)", ae.RetryAfter)
		}
	}
	if e.hashes.Load() != 0 || e.userByName("friend2") != nil {
		t.Fatal("a sign-up blocked by register-ip cost a hash or created a user")
	}
	rows := e.audit(auditThrottled)
	if len(rows) != 1 || fmt.Sprint(rows[0].Detail) != "map[key:"+ipA+" scope:ip]" || rows[0].Outcome != "denied" ||
		rows[0].Actor != (store.Actor{Kind: store.ActorAnonymous, IP: ipA}) {
		t.Fatalf("auth.throttled rows = %+v, want one for the address", rows)
	}
	// Registrations with an invite don't draw on it: the invite's maxUses limits them.
	if err := e.register(tok, "invited0", testPassword, ipA); err != nil {
		t.Fatalf("a registration with an invite from an address register-ip blocks: %v", err)
	}
	// Another address has its own bucket; this one gets a token back after 12 minutes.
	if err := e.register("", "friend2", testPassword, ipB); err != nil {
		t.Fatal(err)
	}
	e.advance(12 * time.Minute)
	if err := e.register("", "friend3", testPassword, ipA); err != nil {
		t.Fatal(err)
	}
	wantCode(t, e.register("", "friend4", testPassword, ipA), api.CodeRateLimited)

	// The bucket comes before the mode: in the invite mode the sixth sign-up of an address is rate_limited too.
	e.set("registrationMode", "invite")
	for range 5 {
		wantCode(t, e.register("", "friend5", testPassword, ipC), api.CodeInviteRequired)
	}
	wantCode(t, e.register("", "friend5", testPassword, ipC), api.CodeRateLimited)

	// auth-ip: every registration takes a token, with or without an invite: the 21st attempt of an address is
	// blocked before anything else.
	const busy = "192.0.2.200"
	for i := range 20 {
		if err := e.register(tok, fmt.Sprintf("crowd%d", i), testPassword, busy); err != nil {
			t.Fatalf("registration %d from one address: %v", i+1, err)
		}
	}
	e.hashes.Store(0)
	ae := wantCode(t, e.register(tok, "crowd20", testPassword, busy), api.CodeRateLimited)
	if ae.RetryAfter != 15 || e.hashes.Load() != 0 {
		t.Errorf("the 21st registration: retryAfter %d s, %d hashes; want 15 and none", ae.RetryAfter, e.hashes.Load())
	}
	_, err := e.svc.CheckInvite(context.Background(), tok, meta(busy))
	wantCode(t, err, api.CodeRateLimited)
}

// TestRegisterHashBudget: a registration takes a token of the auth-hash budget right before its hash, and only
// then (03 §7.3).
func TestRegisterHashBudget(t *testing.T) {
	e := newSvcEnv(t, func(o *Options) { o.Hashes = HashBudget{Burst: 2, PerSecond: 1} })
	e.set("registrationMode", "approval")
	_, tok := e.invite(store.CLIActor, InviteInput{})
	// Refused requests take nothing from the budget.
	wantCode(t, e.register(tok, "", "", docIP(0)), api.CodeValidationFailed)
	wantCode(t, e.register("garbage", "Sam", testPassword, docIP(1)), api.CodeInviteInvalid)
	if err := e.register(tok, "Sam", testPassword, docIP(2)); err != nil {
		t.Fatal(err)
	}
	wantCode(t, e.register(tok, "sam", testPassword, docIP(3)), api.CodeUsernameTaken)
	if err := e.register("", "Kim", testPassword, docIP(4)); err != nil {
		t.Fatal(err)
	}
	e.hashes.Store(0)
	for i, invite := range []string{tok, ""} {
		ae := wantCode(t, e.register(invite, fmt.Sprintf("late%d", i), testPassword, docIP(5+i)), api.CodeServerBusy)
		if ae.RetryAfter != 1 {
			t.Errorf("retryAfter = %d s, want 1", ae.RetryAfter)
		}
	}
	if e.hashes.Load() != 0 || e.userByName("late0") != nil || e.userByName("late1") != nil {
		t.Fatal("a registration refused by the hash budget hashed or created a user")
	}
	if rows := e.audit(auditThrottled); len(rows) != 1 || fmt.Sprint(rows[0].Detail) != "map[scope:hash]" {
		t.Errorf("auth.throttled rows = %+v, want one {scope: hash}", rows)
	}
	e.advance(time.Second)
	if err := e.register(tok, "late0", testPassword, docIP(7)); err != nil {
		t.Errorf("a registration after the budget refilled: %v", err)
	}
}

func TestApproveAndReject(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	alex := e.addAdmin("Alex")
	admin := actorFor(alex, ipA)
	e.set("registrationMode", "approval")
	for i, name := range []string{"Sam", "Kim", "Lee"} {
		if err := e.register("", name, testPassword, docIP(i)); err != nil {
			t.Fatal(err)
		}
	}
	sam, kim, lee := e.userByName("Sam"), e.userByName("Kim"), e.userByName("Lee")

	// Only pending sign-ups can be decided: anything else is user_not_found, and nothing changes.
	for name, id := range map[string]store.UserID{"an unknown user": "nobody000000", "the admin": alex.ID, "no ID": "", "all": "all"} {
		if _, err := e.svc.Approve(ctx, admin, id); !api.IsCode(err, api.CodeUserNotFound) {
			t.Errorf("Approve of %s: %v, want user_not_found", name, err)
		}
		if err := e.svc.Reject(ctx, admin, id); !api.IsCode(err, api.CodeUserNotFound) {
			t.Errorf("Reject of %s: %v, want user_not_found", name, err)
		}
	}
	if e.rawCount("users") != 4 || e.auditCount(auditUserApproved)+e.auditCount(auditSignupRejected) != 0 {
		t.Fatal("a refused decision changed something")
	}

	// Approve: active from now on, with the approver and the time.
	e.advance(time.Hour)
	now := e.clk.now()
	got, err := e.svc.Approve(ctx, admin, sam.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != sam.ID || got.Status != store.StatusActive || got.ApprovedBy != alex.ID || !got.ApprovedAt.Equal(now) ||
		got.Username != "Sam" || got.CreatedVia != "signup" {
		t.Errorf("Approve = %+v", got)
	}
	if row := e.userByName("Sam"); row.Status != store.StatusActive || row.ApprovedBy != alex.ID || !row.ApprovedAt.Equal(now) {
		t.Errorf("the approved user's row = %+v", row)
	}
	rows := e.audit(auditUserApproved)
	if len(rows) != 1 || rows[0].Actor != admin || rows[0].TargetKind != "user" || rows[0].TargetID != string(sam.ID) ||
		rows[0].TargetName != "Sam" || len(rows[0].Detail) != 0 {
		t.Fatalf("user.approved rows = %+v", rows)
	}
	if res := e.login("Sam", ipB); res.User.ID != sam.ID {
		t.Error("the approved user cannot log in")
	}
	// Approving twice, or rejecting an approved user: not pending any more.
	_, err = e.svc.Approve(ctx, admin, sam.ID)
	wantCode(t, err, api.CodeUserNotFound)
	wantCode(t, e.svc.Reject(ctx, admin, sam.ID), api.CodeUserNotFound)
	if e.userByName("Sam") == nil {
		t.Fatal("Reject deleted an active user")
	}

	// Reject: the row goes, and the name is free again.
	if err := e.svc.Reject(ctx, admin, kim.ID); err != nil {
		t.Fatal(err)
	}
	if e.userByName("Kim") != nil {
		t.Fatal("the rejected sign-up is still there")
	}
	rows = e.audit(auditSignupRejected)
	if len(rows) != 1 || rows[0].Actor != admin || rows[0].TargetKind != "user" || rows[0].TargetID != string(kim.ID) ||
		rows[0].TargetName != "Kim" || len(rows[0].Detail) != 0 {
		t.Fatalf("user.signup_rejected rows = %+v", rows)
	}
	wantCode(t, e.tryLogin("Kim", testPassword, ipB), api.CodeInvalidCredentials)
	if err := e.register("", "Kim", testPassword, docIP(10)); err != nil {
		t.Errorf("signing up again with a rejected name: %v", err)
	}

	// The CLI decides too, with no approver recorded.
	got, err = e.svc.Approve(ctx, store.CLIActor, lee.ID)
	if err != nil || got.ApprovedBy != "" || got.ApprovedAt.IsZero() || got.Status != store.StatusActive {
		t.Errorf("Approve from the CLI = %+v, %v", got, err)
	}

	// Only an active admin decides.
	kim = e.userByName("Kim")
	for name, a := range map[string]store.Actor{
		"a member":       actorFor(*e.userByName("Sam"), ipB),
		"anonymous":      {Kind: store.ActorAnonymous},
		"the system":     store.SystemActor,
		"a deleted user": {Kind: store.ActorUser, UserID: "gone00000000", Name: "Alex"},
	} {
		if _, err := e.svc.Approve(ctx, a, kim.ID); !api.IsCode(err, api.CodeForbidden) {
			t.Errorf("Approve as %s: %v, want forbidden", name, err)
		}
		if err := e.svc.Reject(ctx, a, kim.ID); !api.IsCode(err, api.CodeForbidden) {
			t.Errorf("Reject as %s: %v, want forbidden", name, err)
		}
		if _, err := e.svc.RejectAll(ctx, a); !api.IsCode(err, api.CodeForbidden) {
			t.Errorf("RejectAll as %s: %v, want forbidden", name, err)
		}
	}
	e.write(func(q *store.Q) error { return q.SetStatus(alex.ID, store.StatusDisabled, e.clk.now()) })
	_, err = e.svc.Approve(ctx, admin, kim.ID)
	wantCode(t, err, api.CodeForbidden)
	if row := e.userByName("Kim"); row == nil || row.Status != store.StatusPending {
		t.Fatalf("a refused decision changed the sign-up: %+v", row)
	}
}

func TestRejectAll(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	alex := e.addAdmin("Alex")
	admin := actorFor(alex, ipA)
	e.set("registrationMode", "approval")
	e.addUser("Active")
	e.addUser("Disabled", func(u *store.User) { u.Status = store.StatusDisabled })

	// An empty queue: nothing to do, and no row.
	if n, err := e.svc.RejectAll(ctx, admin); err != nil || n != 0 {
		t.Fatalf("RejectAll on an empty queue = %d, %v", n, err)
	}
	if n := e.auditCount(auditSignupRejected); n != 0 {
		t.Fatalf("%d user.signup_rejected rows for an empty queue", n)
	}

	for i := range 3 {
		if err := e.register("", fmt.Sprintf("fake%d", i), testPassword, docIP(i)); err != nil {
			t.Fatal(err)
		}
	}
	n, err := e.svc.RejectAll(ctx, admin)
	if err != nil || n != 3 {
		t.Fatalf("RejectAll = %d, %v; want 3", n, err)
	}
	// The queue is empty; every other account stays.
	if got := e.rawInt("SELECT count(*) FROM users WHERE status = 'pending'"); got != 0 || e.rawCount("users") != 3 {
		t.Errorf("%d pending users and %d users in all after RejectAll, want 0 and 3", got, e.rawCount("users"))
	}
	rows := e.audit(auditSignupRejected)
	if len(rows) != 1 {
		t.Fatalf("%d user.signup_rejected rows, want one for all three", len(rows))
	}
	if r := rows[0]; r.Actor != admin || r.TargetKind != "" || r.TargetID != "" || r.TargetName != "" ||
		fmt.Sprint(r.Detail) != "map[all:true count:3]" {
		t.Errorf("user.signup_rejected row = %+v", r)
	}
	// The names are free again.
	if err := e.register("", "fake0", testPassword, docIP(9)); err != nil {
		t.Errorf("signing up again after RejectAll: %v", err)
	}
	if n, err := e.svc.RejectAll(ctx, store.CLIActor); err != nil || n != 1 {
		t.Errorf("RejectAll from the CLI = %d, %v; want 1", n, err)
	}
}
