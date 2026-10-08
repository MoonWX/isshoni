package auth

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// TestSessionCookieAttributes is 03 §15's cookie matrix: https gets __Host-, Secure, HttpOnly, Lax, Path=/ and
// Max-Age; a plain-http dev origin gets no prefix and no Secure.
func TestSessionCookieAttributes(t *testing.T) {
	token := newToken(sessionTokenBytes)
	for _, tc := range []struct {
		origin, set, clear string
	}{
		{"https://watch.example.com",
			"__Host-isshoni_session=" + token + "; Path=/; Max-Age=2592000; HttpOnly; Secure; SameSite=Lax",
			"__Host-isshoni_session=; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=Lax"},
		{"https://203.0.113.7:8443", // ip mode
			"__Host-isshoni_session=" + token + "; Path=/; Max-Age=2592000; HttpOnly; Secure; SameSite=Lax",
			"__Host-isshoni_session=; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=Lax"},
		{"http://localhost:5173", // task dev
			"isshoni_session=" + token + "; Path=/; Max-Age=2592000; HttpOnly; SameSite=Lax",
			"isshoni_session=; Path=/; Max-Age=0; HttpOnly; SameSite=Lax"},
	} {
		clk := newFakeClock()
		svc, err := New(context.Background(), openStore(t, t.TempDir(), clk), testOptions(clk, tc.origin))
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		svc.SetSessionCookie(w, token, clk.now().Add(sessionIdleTTL))
		if got := w.Header().Values("Set-Cookie"); len(got) != 1 || got[0] != tc.set {
			t.Errorf("%s: Set-Cookie = %q, want %q", tc.origin, got, tc.set)
		}
		w = httptest.NewRecorder()
		svc.ClearSessionCookie(w)
		if got := w.Header().Values("Set-Cookie"); len(got) != 1 || got[0] != tc.clear {
			t.Errorf("%s: cleared Set-Cookie = %q, want %q", tc.origin, got, tc.clear)
		}

		// Max-Age is the idle time that is left, in whole seconds, and never a deletion.
		for left, want := range map[time.Duration]string{
			90*time.Second + 900*time.Millisecond: "Max-Age=90;",
			500 * time.Millisecond:                "Max-Age=1;",
			-time.Hour:                            "Max-Age=1;",
		} {
			w = httptest.NewRecorder()
			svc.SetSessionCookie(w, token, clk.now().Add(left))
			if got := w.Header().Get("Set-Cookie"); !strings.Contains(got, want) {
				t.Errorf("%s: %v of idle time left: Set-Cookie = %q, want %s", tc.origin, left, got, want)
			}
		}
	}
}

func TestAuthenticate(t *testing.T) {
	e := newSvcEnv(t)
	alex := e.addUser("Alex", func(u *store.User) { u.Role = store.RoleAdmin })
	res := e.login("Alex", ipA)
	want := Principal{UserID: alex.ID, Username: "Alex", Role: store.RoleAdmin, Method: MethodSession,
		SessionID: res.Session.ID, ClientKind: "web"}

	p, err := e.svc.Authenticate(e.request(http.MethodGet, res.Token))
	if err != nil || p != want {
		t.Fatalf("Authenticate = %+v, %v; want %+v", p, err, want)
	}
	if !p.IsAdmin() {
		t.Error("the admin's principal is not an admin")
	}
	p, err = e.svc.AuthenticateCookie(e.request(http.MethodGet, res.Token))
	if err != nil || p != want {
		t.Fatalf("AuthenticateCookie = %+v, %v; want %+v", p, err, want)
	}

	// No cookie: REST says unauthenticated, the /ws path says ErrNoCookie.
	_, err = e.svc.Authenticate(e.request(http.MethodGet, ""))
	wantCode(t, err, api.CodeUnauthenticated)
	if _, err = e.svc.AuthenticateCookie(e.request(http.MethodGet, "")); !errors.Is(err, ErrNoCookie) {
		t.Fatalf("AuthenticateCookie without a cookie: %v, want ErrNoCookie", err)
	}

	// M1 has no bearer tokens: any Authorization header is unauthenticated on REST, and ignored on /ws.
	r := e.request(http.MethodGet, res.Token)
	r.Header.Set("Authorization", "Bearer isa_"+newToken(32))
	_, err = e.svc.Authenticate(r)
	wantCode(t, err, api.CodeUnauthenticated)
	if p, err = e.svc.AuthenticateCookie(r); err != nil || p != want {
		t.Fatalf("AuthenticateCookie with an Authorization header = %+v, %v", p, err)
	}

	// Tokens that are not this server's.
	for name, tok := range map[string]string{
		"unknown":      newToken(sessionTokenBytes),
		"too short":    res.Token[:42],
		"too long":     res.Token + "A",
		"not base64":   strings.Repeat("*", 43),
		"with padding": res.Token[:42] + "=",
	} {
		_, err := e.svc.Authenticate(e.request(http.MethodGet, tok))
		wantCode(t, err, api.CodeUnauthenticated)
		if _, err = e.svc.AuthenticateCookie(e.request(http.MethodGet, tok)); !api.IsCode(err, api.CodeUnauthenticated) {
			t.Errorf("AuthenticateCookie with a token that is %s: %v", name, err)
		}
	}

	// Only this server's cookie name counts: on an https origin, the dev cookie is no cookie.
	r = httptest.NewRequestWithContext(context.Background(), http.MethodGet, testOrigin+"/", nil)
	r.Header.Add("Cookie", sessionCookieDev+"="+res.Token)
	if _, err = e.svc.AuthenticateCookie(r); !errors.Is(err, ErrNoCookie) {
		t.Fatalf("the dev cookie on an https origin: %v, want ErrNoCookie", err)
	}
}

// TestAuthenticateDevOrigin: on a plain-http public origin (task dev) the session travels in isshoni_session, and
// the __Host- name is not this server's cookie.
func TestAuthenticateDevOrigin(t *testing.T) {
	e := newSvcEnv(t, func(o *Options) {
		o.Origins = func() Origins {
			return Origins{Primary: "http://localhost:5173", Public: []string{"http://localhost:5173"}}
		}
	})
	e.addUser("Alex")
	res := e.login("Alex", "127.0.0.1")
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://localhost:5173/api/v1/me", nil)
	r.Header.Add("Cookie", sessionCookieDev+"="+res.Token)
	if p, err := e.svc.Authenticate(r); err != nil || p.SessionID != res.Session.ID {
		t.Fatalf("Authenticate with the dev cookie: %+v, %v", p, err)
	}
	if m := e.svc.RequestMeta(r); m.SessionToken != res.Token {
		t.Fatalf("RequestMeta did not read the dev cookie: %+v", m)
	}
	r = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://localhost:5173/api/v1/me", nil)
	r.Header.Add("Cookie", sessionCookieSecure+"="+res.Token)
	_, err := e.svc.Authenticate(r)
	wantCode(t, err, api.CodeUnauthenticated)
	link, err := e.svc.IssueSetupToken(context.Background(), store.CLIActor)
	if err != nil || !strings.HasPrefix(link.URL, "http://localhost:5173/setup#") {
		t.Fatalf("setup link on the dev origin = %q, %v", link.URL, err)
	}
}

// TestAuthenticateUserStatus: a user whose status isn't active is always unauthenticated (03 §7.4).
func TestAuthenticateUserStatus(t *testing.T) {
	for _, status := range []store.UserStatus{store.StatusDisabled, store.StatusPending} {
		e := newSvcEnv(t)
		sam := e.addUser("Sam")
		res := e.login("Sam", ipA)
		e.write(func(q *store.Q) error { return q.SetStatus(sam.ID, status, e.clk.now()) })
		e.svc.sessions.invalidateUser(sam.ID) // what the status path of the admin slice does after its commit
		_, err := e.svc.Authenticate(e.request(http.MethodGet, res.Token))
		wantCode(t, err, api.CodeUnauthenticated)
		_, err = e.svc.AuthenticateCookie(e.request(http.MethodGet, res.Token))
		wantCode(t, err, api.CodeUnauthenticated)
		err = e.svc.Touch(context.Background(), principalOf(res.Session, res.User), netip.MustParseAddr(ipA))
		wantCode(t, err, api.CodeUnauthenticated)
	}
}

// TestSessionExpiry: a session lives 30 days idle and 180 days at most; both limits are inclusive.
func TestSessionExpiry(t *testing.T) {
	e := newSvcEnv(t)
	e.addUser("Alex")

	// Idle: no use for 30 days. (AuthenticateCookie records no use.)
	res := e.login("Alex", ipA)
	e.advance(sessionIdleTTL)
	if _, err := e.svc.AuthenticateCookie(e.request(http.MethodGet, res.Token)); err != nil {
		t.Fatalf("exactly 30 days idle: %v", err)
	}
	e.advance(time.Millisecond) // the rows are still in the cache, which checks the expiry itself
	_, err := e.svc.AuthenticateCookie(e.request(http.MethodGet, res.Token))
	wantCode(t, err, api.CodeUnauthenticated)
	err = e.svc.Touch(context.Background(), principalOf(res.Session, res.User), netip.MustParseAddr(ipA))
	wantCode(t, err, api.CodeUnauthenticated)

	// Absolute: used every 29 days, the session still ends 180 days after its creation.
	res = e.login("Alex", ipA)
	for range 6 {
		e.advance(29 * 24 * time.Hour)
		if _, err := e.svc.Authenticate(e.request(http.MethodGet, res.Token)); err != nil {
			t.Fatalf("a session in regular use: %v", err)
		}
	}
	sess := e.session(res.User.ID, res.Session.ID)
	if !sess.IdleExpiresAt.Equal(sess.ExpiresAt) || !sess.ExpiresAt.Equal(res.Session.CreatedAt.Add(sessionMaxTTL)) {
		t.Fatalf("idle expiry %v, expiry %v: the idle expiry must stop at created + 180 d", sess.IdleExpiresAt, sess.ExpiresAt)
	}
	e.advance(6 * 24 * time.Hour) // day 180
	if _, err := e.svc.Authenticate(e.request(http.MethodGet, res.Token)); err != nil {
		t.Fatalf("exactly 180 days old: %v", err)
	}
	e.advance(time.Millisecond)
	_, err = e.svc.Authenticate(e.request(http.MethodGet, res.Token))
	wantCode(t, err, api.CodeUnauthenticated)
}

// TestSessionCache: lookups are cached for 30 s, and an invalidation takes effect at once.
func TestSessionCache(t *testing.T) {
	e := newSvcEnv(t)
	sam := e.addUser("Sam")
	res := e.login("Sam", ipA)
	auth := func() error {
		_, err := e.svc.Authenticate(e.request(http.MethodGet, res.Token))
		return err
	}
	if err := auth(); err != nil {
		t.Fatal(err)
	}

	// A change behind the service's back (no invalidation) shows only when the cached rows are 30 s old.
	e.write(func(q *store.Q) error { return q.SetStatus(sam.ID, store.StatusDisabled, e.clk.now()) })
	e.advance(sessionCacheTTL - time.Millisecond)
	if err := auth(); err != nil {
		t.Fatalf("29.999 s after the lookup, the cache should still answer: %v", err)
	}
	e.advance(time.Millisecond)
	wantCode(t, auth(), api.CodeUnauthenticated)

	// Back to active: a refused lookup is not cached, so the next one succeeds at once.
	e.write(func(q *store.Q) error { return q.SetStatus(sam.ID, store.StatusActive, e.clk.now()) })
	if err := auth(); err != nil {
		t.Fatalf("after re-enabling: %v", err)
	}

	// An invalidation by user takes effect at once.
	e.write(func(q *store.Q) error { return q.SetRole(sam.ID, store.RoleAdmin, e.clk.now()) })
	if p, _ := e.svc.Authenticate(e.request(http.MethodGet, res.Token)); p.IsAdmin() {
		t.Fatal("the test expects the cached role here")
	}
	e.svc.sessions.invalidateUser(sam.ID)
	if p, err := e.svc.Authenticate(e.request(http.MethodGet, res.Token)); err != nil || !p.IsAdmin() {
		t.Fatalf("after invalidateUser: %+v, %v; want the new role", p, err)
	}

	// So does an invalidation by session.
	e.write(func(q *store.Q) error {
		_, err := q.DeleteSession(sam.ID, res.Session.ID)
		return err
	})
	if err := auth(); err != nil {
		t.Fatalf("the test expects the cached session here: %v", err)
	}
	e.svc.sessions.invalidateSession(res.Session.ID)
	wantCode(t, auth(), api.CodeUnauthenticated)
}

// TestSessionCacheUnit covers the cache's own rules: rows read before an invalidation are not cached after it, and
// states nobody refreshes are swept.
func TestSessionCacheUnit(t *testing.T) {
	c := newSessionCache()
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	mk := func(id, user, hash string) (store.Session, store.User) {
		return store.Session{ID: store.SessionID(id), UserID: store.UserID(user), TokenHash: []byte(hash),
				IdleExpiresAt: t0.Add(sessionIdleTTL), ExpiresAt: t0.Add(sessionMaxTTL), LastSeenAt: t0},
			store.User{ID: store.UserID(user), Status: store.StatusActive}
	}

	// A reader that took its generation before an invalidation must not cache what it read.
	gen := c.generation()
	c.invalidateSession("other")
	s1, u1 := mk("s1", "u1", "h1")
	c.put(gen, s1.TokenHash, s1, u1, t0)
	if _, _, ok := c.byHash(s1.TokenHash, t0); ok {
		t.Fatal("rows read before an invalidation were cached after it")
	}
	c.put(c.generation(), s1.TokenHash, s1, u1, t0)
	if _, _, ok := c.byHash(s1.TokenHash, t0); !ok {
		t.Fatal("a put at the current generation was dropped")
	}
	if _, _, ok := c.byID("s1", t0); !ok {
		t.Fatal("byID misses a cached session")
	}
	if _, _, ok := c.byHash([]byte("h2"), t0); ok {
		t.Fatal("a hash nobody cached is a hit")
	}
	// A clock that went backwards makes the rows stale, not immortal.
	if _, _, ok := c.byHash(s1.TokenHash, t0.Add(-time.Second)); ok {
		t.Fatal("rows loaded in the future are a hit")
	}

	// invalidateUser drops every session of that user and only those.
	s2, u2 := mk("s2", "u1", "h2")
	s3, u3 := mk("s3", "u3", "h3")
	c.put(c.generation(), s2.TokenHash, s2, u2, t0)
	c.put(c.generation(), s3.TokenHash, s3, u3, t0)
	c.invalidateUser("u1")
	if _, _, ok := c.byID("s1", t0); ok {
		t.Fatal("invalidateUser left a session of the user")
	}
	if _, _, ok := c.byHash(s2.TokenHash, t0); ok {
		t.Fatal("invalidateUser left a token of the user")
	}
	if _, _, ok := c.byID("s3", t0); !ok {
		t.Fatal("invalidateUser dropped another user's session")
	}

	// claimTouch: the row's last_seen_at decides first, then the in-memory timestamp; a claim blocks the next one.
	if c.claimTouch("s3", t0, t0.Add(sessionTouchEvery-time.Millisecond)) {
		t.Fatal("a use 4:59.999 after last_seen_at was claimed")
	}
	if !c.claimTouch("s3", t0, t0.Add(sessionTouchEvery)) {
		t.Fatal("a use 5 min after last_seen_at was not claimed")
	}
	if c.claimTouch("s3", t0, t0.Add(sessionTouchEvery+time.Second)) {
		t.Fatal("a second claim right after the first")
	}
	c.touched("s3", t0.Add(sessionTouchEvery), t0.Add(sessionMaxTTL+time.Hour), ipB)
	if s, _, ok := c.byID("s3", t0); !ok || !s.IdleExpiresAt.Equal(s.ExpiresAt) || s.LastIP != ipB {
		t.Fatalf("touched: %+v; the idle expiry stops at the expiry", s)
	}
	c.touched("gone", t0, t0, ipB) // an unknown session is no error
	if !c.claimTouch("gone", t0, t0.Add(sessionTouchEvery)) {
		t.Fatal("a session without a state is always due once its last_seen_at is old")
	}

	// States that nobody refreshed for sessionStateKeep are swept by a later put.
	late := t0.Add(sessionStateKeep + sessionSweepEvery)
	s4, u4 := mk("s4", "u4", "h4")
	c.put(c.generation(), s4.TokenHash, s4, u4, late)
	if len(c.byIDs) != 1 || len(c.byKey) != 1 {
		t.Fatalf("after the sweep: %d states, %d keys; want only the new one", len(c.byIDs), len(c.byKey))
	}
}

// TestTouch is the hub's Revalidate (03 §7.4, §7.6): it validates on every call and writes at most once per 5 min.
func TestTouch(t *testing.T) {
	e := newSvcEnv(t)
	e.addUser("Alex")
	sam := e.addUser("Sam")
	res := e.login("Alex", ipA)
	p := principalOf(res.Session, res.User)
	ctx := context.Background()
	t0 := e.clk.now()
	row := func() store.Session { return *e.session(res.User.ID, res.Session.ID) }

	// Within 5 minutes of the last recorded use: valid, and nothing is written.
	e.advance(sessionTouchEvery - time.Second)
	if err := e.svc.Touch(ctx, p, netip.MustParseAddr(ipB)); err != nil {
		t.Fatal(err)
	}
	if s := row(); !s.LastSeenAt.Equal(t0) || s.LastIP != ipA {
		t.Fatalf("a use after 4:59 was written: %+v", s)
	}

	// At 5 minutes: last_seen_at, last_ip and idle_expires_at move.
	e.advance(time.Second)
	t1 := e.clk.now()
	if err := e.svc.Touch(ctx, p, netip.MustParseAddr("::ffff:"+ipB)); err != nil {
		t.Fatal(err)
	}
	if s := row(); !s.LastSeenAt.Equal(t1) || s.LastIP != ipB || !s.IdleExpiresAt.Equal(t1.Add(sessionIdleTTL)) {
		t.Fatalf("after a use at 5:00: %+v; want last seen %v from %s", s, t1, ipB)
	}
	// Again at once, and without an IP: nothing is written.
	if err := e.svc.Touch(ctx, p, netip.Addr{}); err != nil {
		t.Fatal(err)
	}
	if s := row(); !s.LastSeenAt.Equal(t1) || s.LastIP != ipB {
		t.Fatalf("a second use at 5:00 was written: %+v", s)
	}
	// An unknown peer address keeps the last IP.
	e.advance(sessionTouchEvery)
	if err := e.svc.Touch(ctx, p, netip.Addr{}); err != nil {
		t.Fatal(err)
	}
	if s := row(); !s.LastSeenAt.Equal(e.clk.now()) || s.LastIP != ipB {
		t.Fatalf("a use without an address: %+v; want the old last_ip", s)
	}

	// A REST request records its use the same way, with the request's client IP.
	e.advance(sessionTouchEvery)
	if _, err := e.svc.Authenticate(e.request(http.MethodGet, res.Token)); err != nil {
		t.Fatal(err)
	}
	if s := row(); !s.LastSeenAt.Equal(e.clk.now()) || s.LastIP != ipA {
		t.Fatalf("after a REST request: %+v; want last seen now from %s", s, ipA)
	}
	// The cookie-only path of /ws records nothing.
	e.advance(sessionTouchEvery)
	if _, err := e.svc.AuthenticateCookie(e.request(http.MethodGet, res.Token)); err != nil {
		t.Fatal(err)
	}
	if s := row(); s.LastSeenAt.Equal(e.clk.now()) {
		t.Fatal("AuthenticateCookie recorded a use")
	}

	// Principals that are not this session.
	for name, bad := range map[string]Principal{
		"another user's ID": {UserID: sam.ID, Method: MethodSession, SessionID: res.Session.ID},
		"unknown session":   {UserID: res.User.ID, Method: MethodSession, SessionID: "nosuchsessio"},
		"unknown user":      {UserID: "nosuchuserid", Method: MethodSession, SessionID: res.Session.ID},
		"no session ID":     {UserID: res.User.ID, Method: MethodSession},
		"a bearer (M2)":     {UserID: res.User.ID, Method: MethodBearer, DeviceID: "d1d1d1d1d1d1"},
	} {
		for _, cached := range []bool{true, false} {
			if !cached {
				e.svc.sessions.invalidateSession(res.Session.ID)
			}
			if err := e.svc.Touch(ctx, bad, netip.MustParseAddr(ipA)); !api.IsCode(err, api.CodeUnauthenticated) {
				t.Errorf("Touch with %s (cached %v): %v, want unauthenticated", name, cached, err)
			}
		}
	}

	// A revoked session.
	if err := e.svc.Logout(ctx, p, meta(ipA)); err != nil {
		t.Fatal(err)
	}
	wantCode(t, e.svc.Touch(ctx, p, netip.MustParseAddr(ipA)), api.CodeUnauthenticated)
}

// TestTouchWriteFailure: the last-seen write is best effort. When it fails, Touch still says the session is valid.
func TestTouchWriteFailure(t *testing.T) {
	e := newSvcEnv(t)
	e.addUser("Alex")
	res := e.login("Alex", ipA)
	p := principalOf(res.Session, res.User)

	// The use is due (6 minutes since the login) and the session is in the cache; then the store goes away.
	e.advance(6 * time.Minute)
	if _, err := e.svc.AuthenticateCookie(e.request(http.MethodGet, res.Token)); err != nil {
		t.Fatal(err)
	}
	if err := e.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Touch(context.Background(), p, netip.MustParseAddr(ipB)); err != nil {
		t.Fatalf("Touch with a failing last-seen write: %v, want nil", err)
	}
	if !strings.Contains(e.logs.String(), "could not record the session's last use") {
		t.Errorf("the failed write was not logged:\n%s", e.logs.String())
	}
	// The failure is not retried on every call: the next attempt is 5 minutes later.
	logged := strings.Count(e.logs.String(), "could not record")
	if err := e.svc.Touch(context.Background(), p, netip.MustParseAddr(ipB)); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(e.logs.String(), "could not record"); n != logged {
		t.Errorf("the failed write was retried at once (%d log lines, want %d)", n, logged)
	}
}

// rotate runs the REST chain's two auth steps for a request with token and returns the Set-Cookie headers.
func (e *svcEnv) rotate(token string) ([]*http.Cookie, error) {
	p, err := e.svc.Authenticate(e.request(http.MethodGet, token))
	if err != nil {
		return nil, err
	}
	w := httptest.NewRecorder()
	e.svc.MaybeRotate(w, p)
	return w.Result().Cookies(), nil
}

// TestMaybeRotate is 03 §15's rotation case: after 24 h a REST request rotates, the old token works for 60 s more,
// and a WebSocket upgrade never rotates.
func TestMaybeRotate(t *testing.T) {
	e := newSvcEnv(t)
	e.addUser("Alex")
	res := e.login("Alex", ipA)
	old := res.Token
	oldHash := e.svc.keys.sessionTokenHash(old)

	// Younger than 24 h: no new cookie.
	e.advance(sessionRotateAfter - time.Second)
	if cookies, err := e.rotate(old); err != nil || len(cookies) != 0 {
		t.Fatalf("a 23:59:59 old token: cookies %v, err %v; want none", cookies, err)
	}

	// The /ws path never rotates, however old the token is: neither the cookie check nor the hub's Touch.
	e.advance(time.Second)
	for range 3 {
		p, err := e.svc.AuthenticateCookie(e.request(http.MethodGet, old))
		if err != nil {
			t.Fatal(err)
		}
		if err := e.svc.Touch(context.Background(), p, netip.MustParseAddr(ipA)); err != nil {
			t.Fatal(err)
		}
	}
	if s := e.session(res.User.ID, res.Session.ID); !bytes.Equal(s.TokenHash, oldHash) || !s.RotatedAt.Equal(res.Session.RotatedAt) {
		t.Fatalf("a WebSocket upgrade rotated the token: %+v", s)
	}

	// 24 h old: the next REST request gets a new token.
	now := e.clk.now()
	cookies, err := e.rotate(old)
	if err != nil || len(cookies) != 1 {
		t.Fatalf("a 24 h old token: cookies %v, err %v; want one", cookies, err)
	}
	c := cookies[0]
	if c.Name != sessionCookieSecure || c.Value == old || !tokenWellFormed(c.Value, sessionTokenBytes) ||
		c.MaxAge != int(sessionIdleTTL/time.Second) || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode ||
		c.Path != "/" {
		t.Fatalf("rotated cookie = %+v", c)
	}
	fresh := c.Value
	s := e.session(res.User.ID, res.Session.ID)
	if s == nil || !bytes.Equal(s.TokenHash, e.svc.keys.sessionTokenHash(fresh)) || !bytes.Equal(s.PrevTokenHash, oldHash) ||
		!s.PrevValidUntil.Equal(now.Add(sessionPrevGrace)) || !s.RotatedAt.Equal(now) ||
		!s.IdleExpiresAt.Equal(now.Add(sessionIdleTTL)) || !s.LastSeenAt.Equal(now) {
		t.Fatalf("after the rotation: %+v; want the same session ID with the new hash, the old one as previous for 60 s", s)
	}

	// Both tokens work now, and neither rotates again.
	for name, tok := range map[string]string{"new": fresh, "old": old} {
		if cookies, err := e.rotate(tok); err != nil || len(cookies) != 0 {
			t.Fatalf("the %s token right after the rotation: cookies %v, err %v", name, cookies, err)
		}
	}
	// The old token works until the grace ends, the new one after it.
	e.advance(sessionPrevGrace)
	if _, err := e.svc.Authenticate(e.request(http.MethodGet, old)); err != nil {
		t.Fatalf("the old token 60 s after the rotation: %v", err)
	}
	e.advance(time.Millisecond)
	_, err = e.svc.Authenticate(e.request(http.MethodGet, old))
	wantCode(t, err, api.CodeUnauthenticated)
	p, err := e.svc.Authenticate(e.request(http.MethodGet, fresh))
	if err != nil || p.SessionID != res.Session.ID {
		t.Fatalf("the new token after the grace: %+v, %v", p, err)
	}

	// A day later it rotates again; near the absolute expiry Max-Age is what is left of the 180 days.
	e.advance(sessionRotateAfter)
	cookies, err = e.rotate(fresh)
	if err != nil || len(cookies) != 1 || cookies[0].Value == fresh {
		t.Fatalf("a day after the first rotation: cookies %v, err %v", cookies, err)
	}
	fresh = cookies[0].Value
	for e.clk.now().Before(res.Session.CreatedAt.Add(sessionMaxTTL - 20*24*time.Hour)) {
		e.advance(20 * 24 * time.Hour)
		if cookies, err = e.rotate(fresh); err != nil || len(cookies) != 1 {
			t.Fatalf("a rotation every 20 days: cookies %v, err %v", cookies, err)
		}
		fresh = cookies[0].Value
	}
	left := res.Session.CreatedAt.Add(sessionMaxTTL).Sub(e.clk.now())
	if got := time.Duration(cookies[0].MaxAge) * time.Second; got != left.Truncate(time.Second) || left >= sessionIdleTTL {
		t.Fatalf("Max-Age near the end = %v, want the %v left of the 180 days", got, left)
	}

	// A principal that is not a cached web session rotates nothing.
	if _, err := e.svc.Authenticate(e.request(http.MethodGet, fresh)); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	e.svc.MaybeRotate(w, Principal{UserID: res.User.ID, Method: MethodBearer, DeviceID: "d1d1d1d1d1d1"})
	e.svc.MaybeRotate(w, Principal{UserID: res.User.ID, Method: MethodSession, SessionID: "nosuchsessio"})
	e.svc.MaybeRotate(w, Principal{UserID: "nosuchuserid", Method: MethodSession, SessionID: res.Session.ID})
	if len(w.Result().Cookies()) != 0 {
		t.Fatal("MaybeRotate set a cookie for a principal without a cached session")
	}
}

// TestMaybeRotateParallel: of several parallel requests with a 24 h old token exactly one rotates, so the browser
// never ends up with a token that is already the previous one.
func TestMaybeRotateParallel(t *testing.T) {
	e := newSvcEnv(t)
	e.addUser("Alex")
	res := e.login("Alex", ipA)
	e.advance(sessionRotateAfter)

	const n = 16
	principals := make([]Principal, n)
	for i := range principals {
		p, err := e.svc.Authenticate(e.request(http.MethodGet, res.Token))
		if err != nil {
			t.Fatal(err)
		}
		principals[i] = p
	}
	var mu sync.Mutex
	var tokens []string
	var wg sync.WaitGroup
	for _, p := range principals {
		wg.Go(func() {
			w := httptest.NewRecorder()
			e.svc.MaybeRotate(w, p)
			for _, c := range w.Result().Cookies() {
				mu.Lock()
				tokens = append(tokens, c.Value)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(tokens) != 1 {
		t.Fatalf("%d of %d parallel requests rotated, want exactly 1", len(tokens), n)
	}
	s := e.session(res.User.ID, res.Session.ID)
	if !bytes.Equal(s.TokenHash, e.svc.keys.sessionTokenHash(tokens[0])) {
		t.Fatal("the cookie that went out is not the session's current token")
	}
	for _, tok := range []string{res.Token, tokens[0]} {
		if _, err := e.svc.Authenticate(e.request(http.MethodGet, tok)); err != nil {
			t.Fatalf("a token right after the parallel rotation: %v", err)
		}
	}
}

// TestMaybeRotateRevoked: a session revoked between the chain's two steps is not rotated, and neither is one whose
// write fails; the request goes on either way.
func TestMaybeRotateRevoked(t *testing.T) {
	e := newSvcEnv(t)
	e.addUser("Alex")
	res := e.login("Alex", ipA)
	e.advance(sessionRotateAfter)
	p, err := e.svc.Authenticate(e.request(http.MethodGet, res.Token))
	if err != nil {
		t.Fatal(err)
	}
	// Deleted behind the cache's back: MaybeRotate's own check inside the write finds it gone.
	e.write(func(q *store.Q) error {
		_, err := q.DeleteSession(p.UserID, p.SessionID)
		return err
	})
	w := httptest.NewRecorder()
	e.svc.MaybeRotate(w, p)
	if len(w.Result().Cookies()) != 0 {
		t.Fatal("MaybeRotate set a cookie for a deleted session")
	}

	res = e.login("Alex", ipA)
	e.advance(sessionRotateAfter)
	if p, err = e.svc.Authenticate(e.request(http.MethodGet, res.Token)); err != nil {
		t.Fatal(err)
	}
	if err := e.db.Close(); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	e.svc.MaybeRotate(w, p)
	if len(w.Result().Cookies()) != 0 {
		t.Fatal("MaybeRotate set a cookie although its write failed")
	}
	if !strings.Contains(e.logs.String(), "could not rotate the session token") {
		t.Errorf("the failed rotation was not logged:\n%s", e.logs.String())
	}
}

// TestServiceCSRF: Service.CSRF is the guard of 03 §7.5 with Origins.Public trusted (S24 left it as a fail-closed
// stub that answered 500 to everything).
func TestServiceCSRF(t *testing.T) {
	e := newSvcEnv(t, func(o *Options) {
		o.Origins = func() Origins {
			return Origins{Primary: testOrigin, Public: []string{testOrigin, "http://localhost:5173"}}
		}
	})
	reached := 0
	h := e.svc.CSRF(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, tc := range []struct {
		name, method string
		header       []string
		status       int
		code         string
	}{
		{"GET", "GET", []string{"Sec-Fetch-Site", "cross-site"}, 204, ""},
		{"same-origin JSON POST", "POST", []string{"Sec-Fetch-Site", "same-origin", "Content-Type", "application/json"}, 204, ""},
		{"non-browser JSON POST", "POST", []string{"Content-Type", "application/json; charset=utf-8"}, 204, ""},
		{"cross-site POST", "POST", []string{"Sec-Fetch-Site", "cross-site", "Origin", "https://evil.example",
			"Content-Type", "application/json"}, 403, api.CodeCSRFFailed},
		{"same-site POST", "POST", []string{"Sec-Fetch-Site", "same-site", "Origin", "https://other.example.com",
			"Content-Type", "application/json"}, 403, api.CodeCSRFFailed},
		{"Origin mismatch without Sec-Fetch-Site", "DELETE", []string{"Origin", "https://evil.example",
			"Content-Type", "application/json"}, 403, api.CodeCSRFFailed},
		{"a trusted public origin (the Vite dev server)", "POST", []string{"Sec-Fetch-Site", "cross-site",
			"Origin", "http://localhost:5173", "Content-Type", "application/json"}, 204, ""},
		{"POST without a Content-Type", "POST", []string{"Sec-Fetch-Site", "same-origin"}, 415, api.CodeUnsupportedMediaType},
		{"form POST", "POST", []string{"Sec-Fetch-Site", "same-origin", "Content-Type",
			"application/x-www-form-urlencoded"}, 415, api.CodeUnsupportedMediaType},
		{"PATCH with another charset", "PATCH", []string{"Content-Type", "application/json; charset=iso-8859-1"},
			415, api.CodeUnsupportedMediaType},
	} {
		r := httptest.NewRequestWithContext(context.Background(), tc.method, testOrigin+"/api/v1/x", nil)
		for i := 0; i+1 < len(tc.header); i += 2 {
			r.Header.Add(tc.header[i], tc.header[i+1])
		}
		before := reached
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Errorf("%s: status %d, want %d (body %q)", tc.name, w.Code, tc.status, w.Body)
			continue
		}
		if tc.code == "" {
			if reached != before+1 {
				t.Errorf("%s: the handler did not run", tc.name)
			}
			continue
		}
		if reached != before {
			t.Errorf("%s: the handler ran", tc.name)
		}
		if want := `{"error":{"code":"` + tc.code + `"}}`; w.Body.String() != want {
			t.Errorf("%s: body %q, want %q", tc.name, w.Body, want)
		}
	}

	// A Service that New did not build fails closed.
	for _, svc := range []*Service{nil, new(Service)} {
		w := httptest.NewRecorder()
		before := reached
		svc.CSRF(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached++ })).ServeHTTP(w,
			httptest.NewRequestWithContext(context.Background(), http.MethodGet, testOrigin+"/api/v1/x", nil))
		if w.Code != http.StatusInternalServerError || reached != before || w.Body.String() != `{"error":{"code":"internal"}}` {
			t.Errorf("CSRF of a Service without a guard: status %d, body %q, handler ran %v", w.Code, w.Body, reached != before)
		}
	}
}

func TestRequestMeta(t *testing.T) {
	e := newSvcEnv(t, func(o *Options) {
		// 04's ClientIP honours the proxy header; here a fixed answer shows that the service asks it.
		o.ClientIP = func(*http.Request) netip.Addr { return netip.MustParseAddr(ipC) }
	})
	r := e.request(http.MethodPost, "the-cookie-value")
	r.Header.Set("User-Agent", uaChromeWindows)
	if got, want := e.svc.RequestMeta(r), (ReqMeta{IP: netip.MustParseAddr(ipC), UserAgent: uaChromeWindows,
		SessionToken: "the-cookie-value"}); got != want {
		t.Fatalf("RequestMeta = %+v, want %+v", got, want)
	}
	if got := e.svc.RequestMeta(e.request(http.MethodPost, "")); got.SessionToken != "" || got.UserAgent != "" {
		t.Fatalf("RequestMeta without a cookie = %+v", got)
	}
	a := ActorOf(Principal{UserID: "u1u1u1u1u1u1", Username: "Sam"}, netip.MustParseAddr("::ffff:"+ipA))
	if a != (store.Actor{Kind: store.ActorUser, UserID: "u1u1u1u1u1u1", Name: "Sam", IP: ipA}) {
		t.Fatalf("ActorOf = %+v", a)
	}
	if a := ActorOf(Principal{}, netip.Addr{}); a.IP != "" {
		t.Fatalf("ActorOf with an unknown peer: IP %q, want empty", a.IP)
	}
}
