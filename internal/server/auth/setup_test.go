package auth

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// setupToken issues a setup link as the CLI does and returns the token of its fragment.
func (e *svcEnv) setupToken() string {
	e.t.Helper()
	link, err := e.svc.IssueSetupToken(context.Background(), store.CLIActor)
	if err != nil {
		e.t.Fatal(err)
	}
	tok, ok := strings.CutPrefix(link.URL, testOrigin+"/setup#")
	if !ok || !tokenWellFormed(tok, setupTokenBytes) {
		e.t.Fatalf("setup link %q: want %s/setup#<43-character token>", link.URL, testOrigin)
	}
	return tok
}

func (e *svcEnv) setupAvailable() bool {
	e.t.Helper()
	ok, err := e.svc.SetupAvailable(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return ok
}

func TestSetupToken(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	if !e.setupAvailable() {
		t.Fatal("setup is not available on a new database")
	}
	now := e.clk.now()
	link, err := e.svc.IssueSetupToken(ctx, store.CLIActor)
	if err != nil {
		t.Fatal(err)
	}
	if !link.ExpiresAt.Equal(now.Add(24 * time.Hour)) {
		t.Errorf("the link expires at %v, want 24 h after %v", link.ExpiresAt, now)
	}
	first := strings.TrimPrefix(link.URL, testOrigin+"/setup#")
	if len(first) != 43 || !tokenWellFormed(first, setupTokenBytes) {
		t.Fatalf("link %q: want %s/setup#<43-character token>", link.URL, testOrigin)
	}
	// The database holds the keyed hash, never the token.
	if n := e.rawInt("SELECT count(*) FROM setup_tokens WHERE token_hash = ?", e.svc.keys.inviteTokenHash(first)); n != 1 {
		t.Error("setup_tokens does not hold the invite-keyed hash of the token")
	}
	if n := e.rawInt("SELECT count(*) FROM setup_tokens WHERE instr(token_hash, ?) > 0", []byte(first)); n != 0 {
		t.Error("setup_tokens holds the token itself")
	}
	rows := e.audit(auditSetupTokenIssued)
	if len(rows) != 1 || rows[0].Actor != store.CLIActor || rows[0].TargetKind != "" || len(rows[0].Detail) != 0 {
		t.Fatalf("setup.token_issued rows = %+v, want one by the CLI", rows)
	}
	if strings.Contains(e.logs.String(), first) {
		t.Error("the token reached the log")
	}

	if err := e.svc.CheckSetupToken(ctx, first, meta(ipA)); err != nil {
		t.Fatalf("CheckSetupToken of a live token: %v", err)
	}
	for name, tok := range map[string]string{
		"unknown":   newToken(setupTokenBytes),
		"malformed": "not-a-token",
		"empty":     "",
		"an invite": newToken(inviteTokenBytes),
	} {
		if err := e.svc.CheckSetupToken(ctx, tok, meta(ipA)); !api.IsCode(err, api.CodeSetupTokenInvalid) {
			t.Errorf("CheckSetupToken of a token that is %s: %v, want setup_token_invalid", name, err)
		}
	}

	// Only the newest link works.
	e.advance(time.Hour)
	second := e.setupToken()
	wantCode(t, e.svc.CheckSetupToken(ctx, first, meta(ipA)), api.CodeSetupTokenInvalid)
	if err := e.svc.CheckSetupToken(ctx, second, meta(ipA)); err != nil {
		t.Fatal(err)
	}
	if n := e.rawCount("setup_tokens"); n != 1 {
		t.Errorf("%d setup tokens, want one live at a time", n)
	}

	// It works for 24 h, to the millisecond.
	e.advance(setupTokenTTL)
	if err := e.svc.CheckSetupToken(ctx, second, meta(ipB)); err != nil {
		t.Fatalf("exactly 24 h after it was issued: %v", err)
	}
	e.advance(time.Millisecond)
	wantCode(t, e.svc.CheckSetupToken(ctx, second, meta(ipB)), api.CodeSetupTokenInvalid)
	e.hashes.Store(0)
	_, err = e.svc.CompleteSetup(ctx, SetupInput{Token: second, Username: "Alex", Password: testPassword}, meta(ipB))
	wantCode(t, err, api.CodeSetupTokenInvalid)
	if e.hashes.Load() != 0 || e.rawCount("users") != 0 {
		t.Error("an expired token cost a hash or created a user")
	}
}

func TestCompleteSetup(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	sam := e.addUser("Sam") // an ordinary user is no admin: setup stays available
	samSession := e.login("Sam", ipB)
	if !e.setupAvailable() {
		t.Fatal("setup must be available while no admin exists")
	}
	token := e.setupToken()
	e.hashes.Store(0)

	// Field rules, a code per field, before any hashing.
	for _, tc := range []struct {
		username, password string
		want               map[string]string
	}{
		{"", "", map[string]string{"username": "required", "password": "required"}},
		{"a", "short", map[string]string{"username": "too_short", "password": "too_short"}},
		{"system", testPassword, map[string]string{"username": "reserved"}},
		{"-alex", testPassword, map[string]string{"username": "invalid"}},
		{"Alex", "PassWord1", map[string]string{"password": "too_common"}},
		{"alexander", "Ａlexander", map[string]string{"password": "same_as_username"}},
		{"Alex", "1234567", map[string]string{"password": "too_short"}}, // the minimum is 8 (owner decision)
	} {
		_, err := e.svc.CompleteSetup(ctx, SetupInput{Token: token, Username: tc.username, Password: tc.password}, meta(ipA))
		ae := wantCode(t, err, api.CodeValidationFailed)
		if fmt.Sprint(ae.Fields) != fmt.Sprint(tc.want) {
			t.Errorf("CompleteSetup(%q, %q): fields %v, want %v", tc.username, tc.password, ae.Fields, tc.want)
		}
	}
	// A token that is not live is refused before the fields are looked at, and costs no hash either.
	_, err := e.svc.CompleteSetup(ctx, SetupInput{Token: newToken(setupTokenBytes), Username: "Alex", Password: testPassword}, meta(ipA))
	wantCode(t, err, api.CodeSetupTokenInvalid)
	_, err = e.svc.CompleteSetup(ctx, SetupInput{Token: "", Username: "", Password: ""}, meta(ipA))
	wantCode(t, err, api.CodeSetupTokenInvalid)
	if e.hashes.Load() != 0 {
		t.Fatalf("refused requests hashed %d times", e.hashes.Load())
	}

	// A taken username, and a server name the settings refuse: nothing is created, and the token still works.
	_, err = e.svc.CompleteSetup(ctx, SetupInput{Token: token, Username: "SAM", Password: testPassword}, meta(ipA))
	wantCode(t, err, api.CodeUsernameTaken)
	_, err = e.svc.CompleteSetup(ctx, SetupInput{Token: token, Username: "Alex", Password: testPassword,
		ServerName: "bad\x00name"}, meta(ipA))
	if ae := wantCode(t, err, api.CodeValidationFailed); fmt.Sprint(ae.Fields) != "map[serverName:invalid]" {
		t.Errorf("a bad server name: fields %v", ae.Fields)
	}
	if e.rawCount("users") != 1 || e.rawCount("sessions") != 1 || !e.setupAvailable() || len(e.conns.take()) != 0 {
		t.Fatal("a refused setup left something behind")
	}
	if err := e.svc.CheckSetupToken(ctx, token, meta(ipA)); err != nil {
		t.Fatalf("the token after refused attempts: %v", err)
	}

	// The real thing, from a browser that still has Sam's session cookie.
	e.hashes.Store(0)
	e.advance(time.Minute)
	now := e.clk.now()
	m := meta(ipA)
	m.SessionToken = samSession.Token
	res, err := e.svc.CompleteSetup(ctx, SetupInput{Token: token, Username: " Alex ", Password: testPassword,
		ServerName: "  Alex's  server "}, m)
	if err != nil {
		t.Fatal(err)
	}
	if n := e.hashes.Load(); n != 1 {
		t.Errorf("setup hashed %d times, want 1", n)
	}
	var admin store.User
	var rooms []store.Room
	e.read(func(q *store.Q) error {
		var err error
		if admin, err = q.UserByID(res.User.ID); err != nil {
			return err
		}
		rooms, err = q.ListRooms()
		return err
	})
	if admin.Username != "Alex" || admin.UsernameKey != "alex" || admin.Role != store.RoleAdmin ||
		admin.Status != store.StatusActive || admin.CreatedVia != "setup" || !admin.CreatedAt.Equal(now) ||
		!admin.PasswordChangedAt.Equal(now) || !admin.LastLoginAt.Equal(now) || admin.PasswordHash == "" ||
		strings.Contains(admin.PasswordHash, testPassword) {
		t.Errorf("the admin row = %+v", admin)
	}
	if res.User.Username != "Alex" || res.User.Role != store.RoleAdmin || !tokenWellFormed(res.Token, sessionTokenBytes) {
		t.Errorf("LoginResult = %+v", res)
	}
	s := e.session(admin.ID, res.Session.ID)
	if s == nil || s.Name != "Chrome on Windows" || s.LastIP != ipA || !s.IdleExpiresAt.Equal(now.Add(sessionIdleTTL)) {
		t.Errorf("the admin's session = %+v", s)
	}
	if len(rooms) != 1 || rooms[0].ID != store.DefaultRoomID || !rooms[0].IsDefault {
		t.Errorf("rooms = %+v, want Lounge", rooms)
	}
	if n := e.rawCount("setup_tokens"); n != 0 {
		t.Errorf("%d setup tokens left after setup", n)
	}
	// The server name went through the settings (normalized, cached, audited by the settings themselves).
	if got := e.db.Settings().Get().ServerName; got != "Alex's server" {
		t.Errorf("serverName = %q, want it set and normalized", got)
	}
	if rows := e.audit("settings.changed"); len(rows) != 1 || rows[0].Actor.UserID != admin.ID {
		t.Errorf("settings.changed rows = %+v, want one by the new admin", rows)
	}
	rows := e.audit(auditSetupCompleted)
	if len(rows) != 1 {
		t.Fatalf("%d setup.completed rows, want 1", len(rows))
	}
	if r := rows[0]; r.Actor != (store.Actor{Kind: store.ActorUser, UserID: admin.ID, Name: "Alex", IP: ipA}) ||
		r.TargetKind != "user" || r.TargetID != string(admin.ID) || r.TargetName != "Alex" || !r.At.Equal(now) {
		t.Errorf("setup.completed row = %+v", r)
	}
	// The 03 §7.7 row: the session of the cookie the request arrived with is gone, closed with session_revoked.
	if e.session(sam.ID, samSession.Session.ID) != nil {
		t.Error("the old cookie's session survived the setup")
	}
	want := []closedConn{{ConnSelector{UserID: sam.ID, SessionID: samSession.Session.ID}, ReasonSessionRevoked}}
	if calls := e.conns.take(); fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("CloseConnections calls = %+v, want %+v", calls, want)
	}
	// The admin can log in with the password, and the session works.
	if got := e.login("alex", ipB); got.User.ID != admin.ID {
		t.Error("the new admin cannot log in")
	}
	if p, err := e.svc.Authenticate(e.request("GET", res.Token)); err != nil || !p.IsAdmin() {
		t.Errorf("the setup session: %+v, %v", p, err)
	}

	// Setup is over: every entry point says setup_unavailable, whatever the token.
	if e.setupAvailable() {
		t.Fatal("setup is still available with an admin")
	}
	for _, tok := range []string{token, newToken(setupTokenBytes), ""} {
		wantCode(t, e.svc.CheckSetupToken(ctx, tok, meta(ipC)), api.CodeSetupUnavailable)
		_, err = e.svc.CompleteSetup(ctx, SetupInput{Token: tok, Username: "Eve", Password: testPassword}, meta(ipC))
		wantCode(t, err, api.CodeSetupUnavailable)
	}
	_, err = e.svc.IssueSetupToken(ctx, store.CLIActor)
	wantCode(t, err, api.CodeSetupUnavailable)
	// An admin row counts whatever its status.
	e.write(func(q *store.Q) error { return q.SetStatus(admin.ID, store.StatusDisabled, e.clk.now()) })
	if e.setupAvailable() {
		t.Fatal("setup became available again with a disabled admin")
	}
	_, err = e.svc.IssueSetupToken(ctx, store.CLIActor)
	wantCode(t, err, api.CodeSetupUnavailable)
	if n := e.rawInt("SELECT count(*) FROM users WHERE role = 'admin'"); n != 1 {
		t.Fatalf("%d admins", n)
	}
}

// TestCompleteSetupWithoutServerName: the server name is optional; without it no setting is written.
func TestCompleteSetupWithoutServerName(t *testing.T) {
	e := newSvcEnv(t)
	token := e.setupToken()
	if _, err := e.svc.CompleteSetup(context.Background(), SetupInput{Token: token, Username: "太郎",
		Password: testPassword}, meta(ipA)); err != nil {
		t.Fatal(err)
	}
	if got := e.db.Settings().Get().ServerName; got != "" {
		t.Errorf("serverName = %q, want the default", got)
	}
	if n := e.auditCount("settings.changed"); n != 0 {
		t.Errorf("%d settings.changed rows without a server name", n)
	}
}

// TestCompleteSetupRace is 03 §15's parallel-complete case: two tabs (here eight) complete setup at once, exactly
// one admin exists afterwards, and every loser gets setup_unavailable.
func TestCompleteSetupRace(t *testing.T) {
	e := newSvcEnv(t)
	token := e.setupToken()
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			_, errs[i] = e.svc.CompleteSetup(context.Background(), SetupInput{Token: token,
				Username: fmt.Sprintf("admin%d", i), Password: testPassword}, meta(fmt.Sprintf("192.0.2.%d", i+1)))
		})
	}
	close(start)
	wg.Wait()
	won := 0
	for i, err := range errs {
		switch {
		case err == nil:
			won++
		case !api.IsCode(err, api.CodeSetupUnavailable):
			t.Errorf("request %d: %v, want setup_unavailable", i, err)
		}
	}
	if won != 1 {
		t.Fatalf("%d requests completed setup, want exactly 1", won)
	}
	if admins, users, sessions := e.rawInt("SELECT count(*) FROM users WHERE role = 'admin'"), e.rawCount("users"),
		e.rawCount("sessions"); admins != 1 || users != 1 || sessions != 1 {
		t.Fatalf("%d admins, %d users, %d sessions; want 1 of each", admins, users, sessions)
	}
	if rows := e.auditCount(auditSetupCompleted); rows != 1 {
		t.Fatalf("%d setup.completed rows, want 1", rows)
	}
	if e.setupAvailable() {
		t.Fatal("setup is still available")
	}
}
