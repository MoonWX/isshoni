package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// tryLogin logs in and returns only the error.
func (e *svcEnv) tryLogin(name, password, ip string) error {
	_, err := e.svc.Login(context.Background(), name, password, meta(ip))
	return err
}

func TestLogin(t *testing.T) {
	e := newSvcEnv(t)
	alex := e.addUser("Alex", func(u *store.User) { u.Role = store.RoleAdmin })
	now := e.clk.now()

	res := e.login("Alex", ipA)
	if n := e.hashes.Load(); n != 1 {
		t.Errorf("a login hashed %d times, want 1", n)
	}
	if res.User.ID != alex.ID || res.User.Username != "Alex" || res.User.Role != store.RoleAdmin ||
		!res.User.LastLoginAt.Equal(now) {
		t.Errorf("LoginResult.User = %+v", res.User)
	}
	if !tokenWellFormed(res.Token, sessionTokenBytes) || len(res.Token) != 43 {
		t.Errorf("token %q is not a 43-character session token", res.Token)
	}
	s := e.session(alex.ID, res.Session.ID)
	if s == nil {
		t.Fatal("no session row")
	}
	if !bytes.Equal(s.TokenHash, e.svc.keys.sessionTokenHash(res.Token)) || bytes.Contains(s.TokenHash, []byte(res.Token)) {
		t.Error("the row does not hold the keyed hash of the token")
	}
	if s.Name != "Chrome on Windows" || s.LastIP != ipA || !s.CreatedAt.Equal(now) || !s.RotatedAt.Equal(now) ||
		!s.LastSeenAt.Equal(now) || !s.IdleExpiresAt.Equal(now.Add(30*24*time.Hour)) ||
		!s.ExpiresAt.Equal(now.Add(180*24*time.Hour)) || s.PrevTokenHash != nil {
		t.Errorf("session row = %+v", s)
	}
	if got := *s; fmt.Sprint(got) != fmt.Sprint(res.Session) {
		t.Errorf("LoginResult.Session = %+v, the row is %+v", res.Session, got)
	}
	var stored store.User
	e.read(func(q *store.Q) error {
		var err error
		stored, err = q.UserByID(alex.ID)
		return err
	})
	if !stored.LastLoginAt.Equal(now) || stored.PasswordHash != alex.PasswordHash {
		t.Errorf("after the login: last login %v, hash changed %v", stored.LastLoginAt, stored.PasswordHash != alex.PasswordHash)
	}
	rows := e.audit(auditLogin)
	if len(rows) != 1 {
		t.Fatalf("%d auth.login rows, want 1", len(rows))
	}
	if r := rows[0]; r.Actor != (store.Actor{Kind: store.ActorUser, UserID: alex.ID, Name: "Alex", IP: ipA}) ||
		r.TargetKind != "session" || r.TargetID != string(res.Session.ID) || r.TargetName != "Chrome on Windows" ||
		len(r.Detail) != 0 || !r.At.Equal(now) {
		t.Errorf("auth.login row = %+v", r)
	}
	if calls := e.conns.take(); len(calls) != 0 {
		t.Errorf("a plain login closed connections: %+v", calls)
	}

	// The username is compared by its key (case, width, surrounding spaces), the password after PRECIS
	// normalization (a non-breaking space is a space).
	for _, name := range []string{"alex", "ALEX", "  Alex\t", "Ａｌｅｘ"} {
		if _, err := e.svc.Login(context.Background(), name, testPassword, meta(ipA)); err != nil {
			t.Errorf("Login(%q): %v", name, err)
		}
	}
	if err := e.tryLogin("Alex", strings.ReplaceAll(testPassword, " ", " "), ipA); err != nil {
		t.Errorf("a password typed with non-breaking spaces: %v", err)
	}
	// Every login issues a new token and a new session.
	if got := e.sessions(alex.ID); len(got) != 6 {
		t.Errorf("%d sessions after 6 logins", len(got))
	}
}

func TestLoginFailures(t *testing.T) {
	e := newSvcEnv(t)
	alex := e.addUser("Alex")
	noHash := e.addUser("Reset", func(u *store.User) { u.PasswordHash = "" })
	broken := e.addUser("Broken", func(u *store.User) { u.PasswordHash = "$argon2id$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA" })
	pending := e.addUser("Pending", func(u *store.User) { u.Status = store.StatusPending })
	disabled := e.addUser("Disabled", func(u *store.User) { u.Status = store.StatusDisabled })

	// Each case comes from its own address, so no bucket of another case is in the way.
	for i, tc := range []struct {
		name, username, password string
		code                     string
		reason                   string       // of the auth.login_failed row
		target                   store.UserID // of that row; "" = none
	}{
		{"wrong password", "Alex", "not the password", api.CodeInvalidCredentials, failWrongPassword, alex.ID},
		{"unknown user", "Nobody", testPassword, api.CodeInvalidCredentials, failUnknownUser, ""},
		{"a name no account can have", "sam smith", testPassword, api.CodeInvalidCredentials, failUnknownUser, ""},
		{"a reserved name", "system", testPassword, api.CodeInvalidCredentials, failUnknownUser, ""},
		{"reset pending (NULL hash)", "Reset", testPassword, api.CodeInvalidCredentials, failWrongPassword, noHash.ID},
		{"malformed stored hash", "Broken", testPassword, api.CodeInvalidCredentials, failWrongPassword, broken.ID},
		{"pending, wrong password", "Pending", "not the password", api.CodeInvalidCredentials, failWrongPassword, pending.ID},
		{"pending, right password", "Pending", testPassword, api.CodeAccountPending, failAccountPending, pending.ID},
		{"disabled, wrong password", "Disabled", "not the password", api.CodeInvalidCredentials, failWrongPassword, disabled.ID},
		{"disabled, right password", "Disabled", testPassword, api.CodeAccountDisabled, failAccountDisabled, disabled.ID},
	} {
		ip := fmt.Sprintf("192.0.2.%d", i+1)
		e.hashes.Store(0)
		before := e.auditCount(auditLoginFailed)
		err := e.tryLogin(tc.username, tc.password, ip)
		wantCode(t, err, tc.code)
		// Every attempt that reaches the password check costs exactly one hash: a real one, or the dummy for an
		// unknown user and for an account without a usable hash (03 §7.2). A counter shows it, not timing.
		if n := e.hashes.Load(); n != 1 {
			t.Errorf("%s: %d hashes, want exactly 1", tc.name, n)
		}
		rows := e.audit(auditLoginFailed)
		if len(rows) != before+1 {
			t.Fatalf("%s: %d auth.login_failed rows, want %d", tc.name, len(rows), before+1)
		}
		r := rows[len(rows)-1]
		if r.Actor != (store.Actor{Kind: store.ActorAnonymous, IP: ip}) || fmt.Sprint(r.Detail) != "map[reason:"+tc.reason+"]" {
			t.Errorf("%s: auth.login_failed row = %+v", tc.name, r)
		}
		if tc.target == "" {
			// The name tried for an unknown account is never stored (a typo there is often the password).
			if r.TargetKind != "" || r.TargetID != "" || r.TargetName != "" {
				t.Errorf("%s: the row names a target: %+v", tc.name, r)
			}
		} else if r.TargetKind != "user" || r.TargetID != string(tc.target) || r.TargetName != tc.username {
			t.Errorf("%s: target = %s %s %q", tc.name, r.TargetKind, r.TargetID, r.TargetName)
		}
	}
	if e.rawCount("sessions") != 0 || e.auditCount(auditLogin) != 0 {
		t.Error("a failed login created a session or an auth.login row")
	}
	if !strings.Contains(e.logs.String(), "a stored password hash is malformed") {
		t.Errorf("the malformed hash was not logged:\n%s", e.logs.String())
	}
	for _, typed := range []string{"Nobody", "sam smith"} {
		if n := e.rawInt("SELECT count(*) FROM audit_log WHERE detail LIKE ? OR target_name LIKE ? OR actor_name LIKE ?",
			"%"+typed+"%", "%"+typed+"%", "%"+typed+"%"); n != 0 {
			t.Errorf("the audit log holds the tried name %q", typed)
		}
		if strings.Contains(e.logs.String(), typed) {
			t.Errorf("the log holds the tried name %q", typed)
		}
	}

	// Fields that cannot be a login are refused before any hashing, with a code per field.
	long := strings.Repeat("x", 1025)
	for _, tc := range []struct {
		username, password string
		want               map[string]string
	}{
		{"", testPassword, map[string]string{"username": "required"}},
		{" \t ", testPassword, map[string]string{"username": "required"}},
		{"Alex", "", map[string]string{"password": "required"}},
		{"", "", map[string]string{"username": "required", "password": "required"}},
		{long[:129], testPassword, map[string]string{"username": "too_long"}},
		{"Alex", long, map[string]string{"password": "too_long"}},
		{"Alex", "with a \x00 control", map[string]string{"password": "invalid"}},
	} {
		e.hashes.Store(0)
		ae := wantCode(t, e.tryLogin(tc.username, tc.password, ipC), api.CodeValidationFailed)
		if fmt.Sprint(ae.Fields) != fmt.Sprint(tc.want) {
			t.Errorf("Login(%.10q, %.10q): fields %v, want %v", tc.username, tc.password, ae.Fields, tc.want)
		}
		if e.hashes.Load() != 0 {
			t.Errorf("Login(%.10q, %.10q) hashed", tc.username, tc.password)
		}
	}
	// A password of exactly 1024 bytes is a password like any other (and wrong here).
	wantCode(t, e.tryLogin("Alex", long[:1024], ipC), api.CodeInvalidCredentials)
}

// TestLoginRehash: a stored hash with other argon2 parameters is replaced after a successful login (03 §7.2).
func TestLoginRehash(t *testing.T) {
	e := newSvcEnv(t)
	older, err := newHasher(ArgonParams{MemoryKiB: 32, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 32}, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldHash := older.compute([]byte(testPassword))
	alex := e.addUser("Alex", func(u *store.User) { u.PasswordHash = oldHash })
	hash := func() string {
		var u store.User
		e.read(func(q *store.Q) error {
			var err error
			u, err = q.UserByID(alex.ID)
			return err
		})
		return u.PasswordHash
	}

	// A wrong password leaves the hash alone.
	wantCode(t, e.tryLogin("Alex", "not the password", ipA), api.CodeInvalidCredentials)
	if hash() != oldHash {
		t.Fatal("a failed login changed the hash")
	}
	e.hashes.Store(0)
	res := e.login("Alex", ipA)
	if n := e.hashes.Load(); n != 2 {
		t.Errorf("a login with a rehash derived %d times, want 2 (verify, then the new hash)", n)
	}
	now := hash()
	ph, err := parsePHC(now)
	if err != nil || ph.Params != testArgon || now == oldHash {
		t.Fatalf("stored hash %q: %v; want the current parameters %+v", now, err, testArgon)
	}
	if res.User.PasswordHash != now {
		t.Error("LoginResult.User has the old hash")
	}
	// The next login verifies against the new hash and has nothing to rehash.
	e.hashes.Store(0)
	e.login("Alex", ipA)
	if n := e.hashes.Load(); n != 1 || hash() != now {
		t.Errorf("the second login derived %d times and changed the hash: %v", n, hash() != now)
	}
}

// TestLoginRecheckInWrite: an account that changes between the password check and the Write gets no session for the
// old password (an admin reset or a disable that lands in between).
func TestLoginRecheckInWrite(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(q *store.Q, id store.UserID, now time.Time) error
		code   string
	}{
		{"password cleared", func(q *store.Q, id store.UserID, now time.Time) error {
			return q.SetPasswordHash(id, "", now)
		}, api.CodeInvalidCredentials},
		{"disabled", func(q *store.Q, id store.UserID, now time.Time) error {
			return q.SetStatus(id, store.StatusDisabled, now)
		}, api.CodeAccountDisabled},
		{"deleted", func(q *store.Q, id store.UserID, _ time.Time) error { return q.DeleteUser(id) },
			api.CodeInvalidCredentials},
	} {
		e := newSvcEnv(t)
		alex := e.addUser("Alex")
		var once sync.Once
		// The hook runs while the password is being verified: after Login read the account, before its Write.
		e.svc.hasher.derive = func(pw, salt []byte, tm, mem uint32, th uint8, kl uint32) []byte {
			once.Do(func() {
				e.write(func(q *store.Q) error { return tc.change(q, alex.ID, e.clk.now()) })
			})
			return argon2.IDKey(pw, salt, tm, mem, th, kl)
		}
		wantCode(t, e.tryLogin("Alex", testPassword, ipA), tc.code)
		if n := e.rawCount("sessions"); n != 0 {
			t.Errorf("%s between the check and the write: %d sessions were created", tc.name, n)
		}
	}
}

// TestLoginWithOldCookie is the 03 §7.7 row "login that arrives with an existing session cookie": that old session
// is deleted, and its connections are closed with session_revoked.
func TestLoginWithOldCookie(t *testing.T) {
	e := newSvcEnv(t)
	alex := e.addUser("Alex")
	sam := e.addUser("Sam")
	first := e.login("Alex", ipA)
	if _, err := e.svc.Authenticate(e.request(http.MethodGet, first.Token)); err != nil { // now in the session cache
		t.Fatal(err)
	}

	// A failed login keeps the old session.
	m := meta(ipA)
	m.SessionToken = first.Token
	_, err := e.svc.Login(context.Background(), "Alex", "not the password", m)
	wantCode(t, err, api.CodeInvalidCredentials)
	if e.session(alex.ID, first.Session.ID) == nil || len(e.conns.take()) != 0 {
		t.Fatal("a failed login revoked the old session")
	}

	second, err := e.svc.Login(context.Background(), "Alex", testPassword, m)
	if err != nil {
		t.Fatal(err)
	}
	if second.Session.ID == first.Session.ID || second.Token == first.Token {
		t.Fatal("the login reused the old session or token")
	}
	if e.session(alex.ID, first.Session.ID) != nil {
		t.Error("the old session row is still there")
	}
	want := []closedConn{{ConnSelector{UserID: alex.ID, SessionID: first.Session.ID}, ReasonSessionRevoked}}
	if calls := e.conns.take(); fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("CloseConnections calls = %+v, want %+v", calls, want)
	}
	// The cache was invalidated before the connections were closed: the old cookie is dead at once.
	_, err = e.svc.Authenticate(e.request(http.MethodGet, first.Token))
	wantCode(t, err, api.CodeUnauthenticated)
	if _, err := e.svc.Authenticate(e.request(http.MethodGet, second.Token)); err != nil {
		t.Errorf("the new session: %v", err)
	}

	// The cookie of another account (a shared browser): that account's session goes, not one of the new user's.
	m.SessionToken = second.Token
	third, err := e.svc.Login(context.Background(), "Sam", testPassword, m)
	if err != nil {
		t.Fatal(err)
	}
	if e.session(alex.ID, second.Session.ID) != nil || e.session(sam.ID, third.Session.ID) == nil {
		t.Error("after Sam's login with Alex's cookie: Alex's session should be gone and Sam's there")
	}
	want = []closedConn{{ConnSelector{UserID: alex.ID, SessionID: second.Session.ID}, ReasonSessionRevoked}}
	if calls := e.conns.take(); fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("CloseConnections calls = %+v, want %+v", calls, want)
	}

	// A cookie without a live session behind it changes nothing.
	for _, tok := range []string{newToken(sessionTokenBytes), "garbage", first.Token} {
		m.SessionToken = tok
		if _, err := e.svc.Login(context.Background(), "Sam", testPassword, m); err != nil {
			t.Fatal(err)
		}
	}
	if calls := e.conns.take(); len(calls) != 0 {
		t.Errorf("a stale cookie closed connections: %+v", calls)
	}
	if got := e.sessions(sam.ID); len(got) != 4 {
		t.Errorf("Sam has %d sessions, want 4", len(got))
	}
}

// TestSessionCap is the 03 §7.7 row "session cap eviction": the 51st session deletes the least recently seen one and
// closes its connections with session_revoked.
func TestSessionCap(t *testing.T) {
	e := newSvcEnv(t)
	alex := e.addUser("Alex")
	now := e.clk.now()
	var oldest, second store.SessionID
	e.write(func(q *store.Q) error {
		for i := range maxSessionsPerUser {
			seen := now.Add(-time.Duration(maxSessionsPerUser-i) * time.Hour) // i = 0 was seen the longest ago
			s := store.Session{UserID: alex.ID, TokenHash: fmt.Appendf(nil, "hash-%02d", i), Name: "Firefox on Linux",
				CreatedAt: seen, LastIP: ipB, IdleExpiresAt: now.Add(sessionIdleTTL), ExpiresAt: now.Add(sessionMaxTTL)}
			if err := q.CreateSession(&s); err != nil {
				return err
			}
			switch i {
			case 0:
				oldest = s.ID
			case 1:
				second = s.ID
			}
		}
		return nil
	})

	res := e.login("Alex", ipA)
	got := e.sessions(alex.ID)
	if len(got) != maxSessionsPerUser || got[0].ID != res.Session.ID {
		t.Fatalf("%d sessions after the 51st login, want %d with the new one first", len(got), maxSessionsPerUser)
	}
	if e.session(alex.ID, oldest) != nil || e.session(alex.ID, second) == nil {
		t.Error("the cap did not evict exactly the least recently seen session")
	}
	want := []closedConn{{ConnSelector{UserID: alex.ID, SessionID: oldest}, ReasonSessionRevoked}}
	if calls := e.conns.take(); fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("CloseConnections calls = %+v, want %+v", calls, want)
	}
}

// TestLogout is the 03 §7.7 row "Logout": this session and its push subscriptions go, and its connections are
// closed with logged_out.
func TestLogout(t *testing.T) {
	e := newSvcEnv(t)
	alex := e.addUser("Alex")
	res := e.login("Alex", ipA)
	other := e.login("Alex", ipB)
	e.write(func(q *store.Q) error {
		for i, sid := range []store.SessionID{res.Session.ID, other.Session.ID} {
			sub := store.PushSubscription{UserID: alex.ID, SessionID: sid, Endpoint: fmt.Sprintf("https://push.example/%d", i),
				P256dh: "p", Auth: "a", Name: "Chrome on Windows"}
			if _, err := q.UpsertPushSubscription(&sub); err != nil {
				return err
			}
		}
		return nil
	})
	p, err := e.svc.Authenticate(e.request(http.MethodGet, res.Token))
	if err != nil {
		t.Fatal(err)
	}
	e.advance(time.Minute)
	now := e.clk.now()

	if err := e.svc.Logout(context.Background(), p, meta(ipC)); err != nil {
		t.Fatal(err)
	}
	if e.session(alex.ID, res.Session.ID) != nil || e.session(alex.ID, other.Session.ID) == nil {
		t.Fatal("Logout must delete this session and only this one")
	}
	if n := e.rawInt("SELECT count(*) FROM push_subscriptions WHERE session_id = ?", string(res.Session.ID)); n != 0 {
		t.Error("the session's push subscription survived the logout")
	}
	if n := e.rawCount("push_subscriptions"); n != 1 {
		t.Errorf("%d push subscriptions left, want the other session's", n)
	}
	want := []closedConn{{ConnSelector{UserID: alex.ID, SessionID: res.Session.ID}, ReasonLoggedOut}}
	if calls := e.conns.take(); fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("CloseConnections calls = %+v, want %+v", calls, want)
	}
	rows := e.audit(auditLogout)
	if len(rows) != 1 {
		t.Fatalf("%d auth.logout rows, want 1", len(rows))
	}
	if r := rows[0]; r.Actor != (store.Actor{Kind: store.ActorUser, UserID: alex.ID, Name: "Alex", IP: ipC}) ||
		r.TargetKind != "session" || r.TargetID != string(res.Session.ID) || r.TargetName != "Chrome on Windows" ||
		!r.At.Equal(now) || len(r.Detail) != 0 {
		t.Errorf("auth.logout row = %+v", r)
	}
	// The cached session is gone at once, on both paths; the other session is untouched.
	_, err = e.svc.Authenticate(e.request(http.MethodGet, res.Token))
	wantCode(t, err, api.CodeUnauthenticated)
	wantCode(t, e.svc.Touch(context.Background(), p, netip.MustParseAddr(ipA)), api.CodeUnauthenticated)
	if _, err := e.svc.Authenticate(e.request(http.MethodGet, other.Token)); err != nil {
		t.Errorf("the other session after the logout: %v", err)
	}

	// Idempotent: a second logout is no error and writes no second row.
	if err := e.svc.Logout(context.Background(), p, meta(ipC)); err != nil {
		t.Fatalf("a second Logout: %v", err)
	}
	if n := e.auditCount(auditLogout); n != 1 {
		t.Errorf("%d auth.logout rows after a second logout, want 1", n)
	}

	wantCode(t, e.svc.Logout(context.Background(), Principal{Method: MethodSession}, meta(ipC)), api.CodeUnauthenticated)
	err = e.svc.Logout(context.Background(), Principal{UserID: alex.ID, Method: MethodBearer, DeviceID: "d1d1d1d1d1d1"}, meta(ipC))
	if !api.IsCode(err, api.CodeInternal) || !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("Logout of a device (M2): %v", err)
	}
}

// TestLoginThrottles is 03 §15's login-throttle list at the service, with the counting hasher: a blocked attempt
// never hashes.
func TestLoginThrottles(t *testing.T) {
	e := newSvcEnv(t)
	alex := e.addUser("Alex")
	const v6 = "2001:db8:1:2::10"
	hashed := func() int32 { return e.hashes.Load() }

	// A live session from an IPv6 address, for the known-IP rule below.
	e.login("Alex", v6)
	e.hashes.Store(0)

	// 5 wrong passwords for one username from address A, then the 6th from A → 429 with Retry-After, and the hasher
	// counter doesn't move. Not even for the right password.
	for range 5 {
		wantCode(t, e.tryLogin("Alex", "not the password", ipA), api.CodeInvalidCredentials)
	}
	if hashed() != 5 {
		t.Fatalf("%d hashes for 5 attempts", hashed())
	}
	for _, pw := range []string{"not the password", testPassword} {
		ae := wantCode(t, e.tryLogin("alex", pw, ipA), api.CodeRateLimited)
		if ae.RetryAfter < 1 || ae.RetryAfter > 120 {
			t.Errorf("retryAfter = %d s, want within the 2-minute refill", ae.RetryAfter)
		}
	}
	if hashed() != 5 {
		t.Fatalf("a blocked attempt hashed (%d hashes)", hashed())
	}

	// The correct password from address B → 200: a lockout from A doesn't block B. B is now the last_ip of a live
	// session.
	e.login("Alex", ipB)
	if hashed() != 6 {
		t.Fatalf("%d hashes, want 6", hashed())
	}

	// 5 more addresses with 5 wrong passwords each empty auth-user (30 failures in all).
	for i := range 5 {
		for range 5 {
			wantCode(t, e.tryLogin("Alex", "not the password", fmt.Sprintf("192.0.2.%d", 10+i)), api.CodeInvalidCredentials)
		}
	}
	if hashed() != 31 {
		t.Fatalf("%d hashes, want 31", hashed())
	}

	// The correct password from a new address C → 429 without a hash.
	ae := wantCode(t, e.tryLogin("Alex", testPassword, ipC), api.CodeRateLimited)
	if ae.RetryAfter < 1 || ae.RetryAfter > 120 {
		t.Errorf("retryAfter = %d s", ae.RetryAfter)
	}
	// So is another address next to a known IPv6 /64, and a username nobody has.
	wantCode(t, e.tryLogin("Alex", testPassword, "2001:db8:1:3::10"), api.CodeRateLimited)
	if hashed() != 31 {
		t.Fatalf("a blocked attempt hashed (%d hashes)", hashed())
	}

	// From B, the last_ip of a live session → 200; and from another address in the /64 of a live session's IPv6
	// last_ip → 200.
	e.login("Alex", ipB)
	e.login("Alex", "2001:db8:1:2:aaaa:bbbb:cccc:dddd")
	if hashed() != 33 {
		t.Fatalf("%d hashes, want 33", hashed())
	}
	// A known address is still limited by its own auth-user-ip bucket: after 5 failures there, it is blocked too.
	for range 5 {
		wantCode(t, e.tryLogin("Alex", "not the password", ipB), api.CodeInvalidCredentials)
	}
	wantCode(t, e.tryLogin("Alex", testPassword, ipB), api.CodeRateLimited)

	// The audit: an auth.login_failed row per failed check, one auth.throttled {scope: username} row for the
	// emptied auth-user bucket with the account's name as the key, and none for auth-user-ip.
	if n := e.auditCount(auditLoginFailed); n != 35 {
		t.Errorf("%d auth.login_failed rows, want 35", n)
	}
	rows := e.audit(auditThrottled)
	if len(rows) != 1 {
		t.Fatalf("%d auth.throttled rows, want 1: %+v", len(rows), rows)
	}
	if r := rows[0]; r.Actor != (store.Actor{Kind: store.ActorAnonymous, IP: ipC}) ||
		fmt.Sprint(r.Detail) != "map[key:Alex scope:username]" || r.TargetID != "" {
		t.Errorf("auth.throttled row = %+v", r)
	}

	// An expired session's address is not a known IP: when every session is gone, B waits like everyone else.
	e.write(func(q *store.Q) error {
		_, err := q.DeleteSessions(alex.ID, "")
		return err
	})
	e.advance(2 * time.Minute) // one token back in every bucket of failed checks
	wantCode(t, e.tryLogin("Alex", "not the password", "192.0.2.50"), api.CodeInvalidCredentials)
	wantCode(t, e.tryLogin("Alex", testPassword, "2001:db8:1:2::10"), api.CodeRateLimited)

	// The warn line of a blocked attempt has the address and never a username; one per address per minute.
	logs := e.logs.String()
	if n := strings.Count(logs, "remote_ip="+ipA); n != 1 {
		t.Errorf("%d warn lines for address A, want 1 per minute:\n%s", n, logs)
	}
	if strings.Contains(strings.ToLower(logs), "alex") {
		t.Errorf("a log line carries the username:\n%s", logs)
	}
}

// TestLoginThrottleUnknownUser: an unknown username is throttled like a real one, and writes no auth.throttled row.
func TestLoginThrottleUnknownUser(t *testing.T) {
	e := newSvcEnv(t)
	for i := range 6 {
		for range 5 {
			wantCode(t, e.tryLogin("Nobody", testPassword, fmt.Sprintf("192.0.2.%d", 10+i)), api.CodeInvalidCredentials)
		}
	}
	e.hashes.Store(0)
	wantCode(t, e.tryLogin("nobody", testPassword, "192.0.2.10"), api.CodeRateLimited) // auth-user-ip
	wantCode(t, e.tryLogin("NOBODY", testPassword, ipC), api.CodeRateLimited)          // auth-user, no known IPs
	wantCode(t, e.tryLogin("sam smith", testPassword, ipC), api.CodeInvalidCredentials)
	if n := e.hashes.Load(); n != 1 {
		t.Errorf("%d hashes, want only the one of the other name", n)
	}
	if n := e.auditCount(auditThrottled); n != 0 {
		t.Errorf("%d auth.throttled rows for a username nobody has", n)
	}
}

// TestLoginSuccessRefillsAddressOnly: a successful login refills that address's auth-user-ip bucket and leaves
// auth-user as it was, so a friend's login never hands an attacker a fresh budget (03 §7.3).
func TestLoginSuccessRefillsAddressOnly(t *testing.T) {
	e := newSvcEnv(t)
	e.addUser("Alex")
	for range 4 {
		wantCode(t, e.tryLogin("Alex", "not the password", ipA), api.CodeInvalidCredentials)
	}
	e.login("Alex", ipA)
	// A full bucket again for A: 5 more failures pass the check, the 6th is blocked.
	for range 5 {
		wantCode(t, e.tryLogin("Alex", "not the password", ipA), api.CodeInvalidCredentials)
	}
	wantCode(t, e.tryLogin("Alex", "not the password", ipA), api.CodeRateLimited)

	// auth-user counted all 9 failures and got nothing back: 21 more empty it.
	failures := 0
	for i := 0; ; i++ {
		err := e.tryLogin("Alex", "not the password", fmt.Sprintf("192.0.2.%d", 10+i/5))
		if api.IsCode(err, api.CodeRateLimited) {
			break
		}
		wantCode(t, err, api.CodeInvalidCredentials)
		failures++
	}
	if failures != 21 {
		t.Fatalf("auth-user allowed %d more failures, want 30 - 9 = 21", failures)
	}
}

// TestLoginIPBucket: the auth-ip bucket blocks the 21st attempt of an address, before any other work.
func TestLoginIPBucket(t *testing.T) {
	e := newSvcEnv(t)
	e.addUser("Alex")
	// Every attempt counts, whatever its outcome: a success, a failure and a refused field.
	e.login("Alex", ipA)
	wantCode(t, e.tryLogin("", "", ipA), api.CodeValidationFailed)
	for i := range 18 {
		wantCode(t, e.tryLogin(fmt.Sprintf("nobody-%d", i), testPassword, ipA), api.CodeInvalidCredentials)
	}
	e.hashes.Store(0)
	for range 3 {
		ae := wantCode(t, e.tryLogin("Alex", testPassword, ipA), api.CodeRateLimited)
		if ae.RetryAfter != 15 {
			t.Errorf("retryAfter = %d s, want 15 (one token per 15 s)", ae.RetryAfter)
		}
	}
	if e.hashes.Load() != 0 {
		t.Fatal("an attempt blocked by auth-ip hashed")
	}
	// The first block of the episode writes one auth.throttled {scope: ip} row; the later ones write none.
	rows := e.audit(auditThrottled)
	if len(rows) != 1 || fmt.Sprint(rows[0].Detail) != "map[key:"+ipA+" scope:ip]" ||
		rows[0].Actor != (store.Actor{Kind: store.ActorAnonymous, IP: ipA}) {
		t.Fatalf("auth.throttled rows = %+v, want one for the address", rows)
	}
	if n := strings.Count(e.logs.String(), "auth attempt throttled"); n != 1 {
		t.Errorf("%d warn lines for 3 blocked attempts in one minute, want 1", n)
	}
	// Another address is not affected; A gets one attempt back after 15 s.
	e.login("Alex", ipB)
	e.advance(15 * time.Second)
	e.login("Alex", ipA)
	wantCode(t, e.tryLogin("Alex", testPassword, ipA), api.CodeRateLimited)
	if n := e.auditCount(auditThrottled); n != 2 {
		t.Errorf("%d auth.throttled rows after a second episode, want 2", n)
	}

	// IPv6: the bucket is the /64, and so is the key of the audit row.
	for i := range 20 {
		wantCode(t, e.tryLogin(fmt.Sprintf("nobody-%d", i), testPassword, fmt.Sprintf("2001:db8:7:7::%x", i+1)),
			api.CodeInvalidCredentials)
	}
	wantCode(t, e.tryLogin("Alex", testPassword, "2001:db8:7:7:ffff::1"), api.CodeRateLimited)
	if err := e.tryLogin("Alex", testPassword, "2001:db8:7:8::1"); err != nil {
		t.Errorf("the neighbouring /64: %v", err)
	}
	rows = e.audit(auditThrottled)
	if got := fmt.Sprint(rows[len(rows)-1].Detail); got != "map[key:2001:db8:7:7::/64 scope:ip]" {
		t.Errorf("auth.throttled detail for an IPv6 address = %s", got)
	}

	// The setup endpoints draw on the same bucket.
	for range 20 {
		_ = e.svc.CheckSetupToken(context.Background(), "x", meta("192.0.2.77"))
	}
	wantCode(t, e.svc.CheckSetupToken(context.Background(), "x", meta("192.0.2.77")), api.CodeRateLimited)
	_, err := e.svc.CompleteSetup(context.Background(), SetupInput{}, meta("192.0.2.77"))
	wantCode(t, err, api.CodeRateLimited)
	wantCode(t, e.tryLogin("Alex", testPassword, "192.0.2.77"), api.CodeRateLimited)
}

// TestLoginHashBudgetEmpty: with the auth-hash bucket empty, a login → 503 server_busy with Retry-After, and the
// hasher counter doesn't move (03 §15).
func TestLoginHashBudgetEmpty(t *testing.T) {
	e := newSvcEnv(t, func(o *Options) { o.Hashes = HashBudget{Burst: 3, PerSecond: 2} })
	e.addUser("Alex")
	e.login("Alex", "192.0.2.1")
	wantCode(t, e.tryLogin("Alex", "not the password", "192.0.2.2"), api.CodeInvalidCredentials)
	wantCode(t, e.tryLogin("Nobody", testPassword, "192.0.2.3"), api.CodeInvalidCredentials) // the dummy hash counts too
	e.hashes.Store(0)
	for i := range 3 {
		ae := wantCode(t, e.tryLogin("Alex", testPassword, fmt.Sprintf("192.0.2.%d", 10+i)), api.CodeServerBusy)
		if ae.RetryAfter != 1 {
			t.Errorf("retryAfter = %d s, want 1 (the next token is 500 ms away)", ae.RetryAfter)
		}
	}
	if e.hashes.Load() != 0 {
		t.Fatal("a login refused by the hash budget hashed")
	}
	// A refused field never reaches the budget, and neither does an attempt another bucket blocks.
	wantCode(t, e.tryLogin("", "", "192.0.2.20"), api.CodeValidationFailed)
	rows := e.audit(auditThrottled)
	if len(rows) != 1 || fmt.Sprint(rows[0].Detail) != "map[scope:hash]" {
		t.Fatalf("auth.throttled rows = %+v, want one {scope: hash}", rows)
	}
	e.advance(500 * time.Millisecond)
	e.login("Alex", "192.0.2.30")
	wantCode(t, e.tryLogin("Alex", testPassword, "192.0.2.31"), api.CodeServerBusy)
}

// TestHashBudgetCapsLogins is 03 §15's hash-budget case at the service: 1000 logins for random usernames from 1000
// random /64s, spread over 10 simulated seconds, hash at most 20 + 5 × 10 = 70 times. The others get 503
// server_busy with Retry-After ≥ 1 and no hash.
func TestHashBudgetCapsLogins(t *testing.T) {
	e := newSvcEnv(t, func(o *Options) { o.Hashes = HashBudget{} }) // the default: burst 20, 5 per second
	busy := 0
	for range 1000 {
		var a [16]byte
		a[0], a[1] = 0x20, 0x01
		_, _ = rand.Read(a[2:8]) // a random /64 in 2001::/16
		m := ReqMeta{IP: netip.AddrFrom16(a), UserAgent: uaChromeWindows}
		_, err := e.svc.Login(context.Background(), "user-"+rand.Text(), testPassword, m)
		switch {
		case api.IsCode(err, api.CodeInvalidCredentials):
		case api.IsCode(err, api.CodeServerBusy):
			busy++
			if ae := wantCode(t, err, api.CodeServerBusy); ae.RetryAfter < 1 {
				t.Fatalf("retryAfter = %d", ae.RetryAfter)
			}
		default:
			t.Fatalf("Login: %v", err)
		}
		e.advance(10 * time.Millisecond)
	}
	hashes := int(e.hashes.Load())
	if hashes > 70 || hashes < 65 {
		t.Fatalf("%d hashes over 10 simulated seconds, want at most 70 (and close to it)", hashes)
	}
	if hashes+busy != 1000 {
		t.Fatalf("%d hashes + %d refusals != 1000 attempts", hashes, busy)
	}
}

// TestLoginFailedAuditCap: auth.login_failed rows have a server-wide cap of 600 per hour; beyond it, one
// auth.throttled {scope: global} row is written per hour (03 §7.3).
func TestLoginFailedAuditCap(t *testing.T) {
	if testing.Short() {
		t.Skip("605 failed logins")
	}
	e := newSvcEnv(t)
	fail := func(i int) {
		t.Helper()
		ip := netip.MustParseAddr(fmt.Sprintf("10.%d.%d.%d", i>>16&0xff, i>>8&0xff, i&0xff))
		_, err := e.svc.Login(context.Background(), fmt.Sprintf("nobody-%d", i), testPassword, ReqMeta{IP: ip})
		wantCode(t, err, api.CodeInvalidCredentials)
	}
	for i := range 605 {
		fail(i)
	}
	if n := e.auditCount(auditLoginFailed); n != 600 {
		t.Errorf("%d auth.login_failed rows, want the cap of 600", n)
	}
	rows := e.audit(auditThrottled)
	if len(rows) != 1 || fmt.Sprint(rows[0].Detail) != "map[scope:global]" {
		t.Fatalf("auth.throttled rows = %+v, want one {scope: global}", rows)
	}
	// The cap refills at 600 per hour: 6 s later one more row fits.
	e.advance(6 * time.Second)
	fail(1000)
	fail(1001)
	if n, g := e.auditCount(auditLoginFailed), e.auditCount(auditThrottled); n != 601 || g != 1 {
		t.Errorf("after 6 s: %d auth.login_failed rows and %d auth.throttled, want 601 and 1", n, g)
	}
}

// TestHashQueueFull: when the hash semaphore's queue is full, login and setup answer 503 server_busy with
// Retry-After: 5 at once (03 §7.2).
func TestHashQueueFull(t *testing.T) {
	e := newSvcEnv(t)
	e.addUser("Alex")
	token := e.setupToken()
	gate := make(chan struct{})
	var maxInFlight atomic.Int32
	h := blockingHasher(t, 1, gate, &maxInFlight)
	e.svc.hasher = h

	// One hash holds the only slot and 32 callers wait for it.
	var wg sync.WaitGroup
	for range 1 + maxHashWaiters {
		wg.Go(func() { _, _ = h.hash(context.Background(), "a long enough password") })
	}
	deadline := time.Now().Add(10 * time.Second)
	for h.waiting.Load() != maxHashWaiters {
		if time.Now().After(deadline) {
			t.Fatalf("%d callers wait for the hash slot, want %d", h.waiting.Load(), maxHashWaiters)
		}
		time.Sleep(time.Millisecond)
	}

	for _, name := range []string{"Alex", "Nobody"} {
		ae := wantCode(t, e.tryLogin(name, testPassword, ipA), api.CodeServerBusy)
		if ae.RetryAfter != 5 {
			t.Errorf("Login(%s) with a full hash queue: retryAfter %d, want 5", name, ae.RetryAfter)
		}
	}
	_, err := e.svc.CompleteSetup(context.Background(), SetupInput{Token: token, Username: "Admin",
		Password: testPassword}, meta(ipA))
	if ae := wantCode(t, err, api.CodeServerBusy); ae.RetryAfter != 5 {
		t.Errorf("CompleteSetup with a full hash queue: retryAfter %d, want 5", ae.RetryAfter)
	}
	// A busy queue is not a failed password check: no bucket was spent and no row written.
	if n := e.auditCount(auditLoginFailed); n != 0 {
		t.Errorf("%d auth.login_failed rows for attempts that never hashed", n)
	}
	close(gate)
	wg.Wait()
}

func TestLoginKey(t *testing.T) {
	for in, want := range map[string]string{
		"Alex":    "alex",
		" ALEX ":  "alex",
		"Ａｌｅｘ":    "alex",
		"太郎":      "太郎",
		"system":  "system", // reserved names have a key; no account has it
		"sam..k":  "sam..k", // so do names the shape rules refuse
		"sam_k.9": "sam_k.9",
	} {
		key, ok := loginKey(in)
		if !ok || key != want {
			t.Errorf("loginKey(%q) = %q, %v; want %q", in, key, ok, want)
		}
		if _, norm, err := NormalizeUsername(in); err == nil && norm != key {
			t.Errorf("loginKey(%q) = %q, but NormalizeUsername gives %q", in, key, norm)
		}
	}
	for _, in := range []string{"sam smith", "a\x00b", "😀"} {
		key, ok := loginKey(in)
		if ok || !strings.HasPrefix(key, "\x00") {
			t.Errorf("loginKey(%q) = %q, %v; want no key", in, key, ok)
		}
	}
	if got := ipKeyText(netip.Addr{}); got != "" {
		t.Errorf("ipKeyText of an unknown peer = %q", got)
	}
}

// FuzzLoginKey: loginKey never panics; every name NormalizeUsername accepts gets exactly the key it was registered
// under, so an account can always be found; and an input without a key never collides with a real key.
func FuzzLoginKey(f *testing.F) {
	for _, s := range []string{"Alex", " ALEX ", "Ａｌｅｘ", "太郎", "Ёжик", "राजे", "sam_k.99", "system", "sam..k", "sam smith",
		"", "   ", "a\x00b", "😀", "\xff\xfe", strings.Repeat("x", 200)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		key, ok := loginKey(in)
		if again, okAgain := loginKey(in); again != key || okAgain != ok {
			t.Fatalf("loginKey(%q) is not deterministic", in)
		}
		if ok == strings.HasPrefix(key, "\x00") {
			t.Fatalf("loginKey(%q) = %q, %v: a real key never starts with NUL, a placeholder always does", in, key, ok)
		}
		if _, norm, err := NormalizeUsername(in); err == nil && (!ok || key != norm) {
			t.Fatalf("loginKey(%q) = %q, %v, but the name registers under the key %q", in, key, ok, norm)
		}
	})
}
