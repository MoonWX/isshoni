package auth

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

func TestNewRejectsBadOptions(t *testing.T) {
	clk := newFakeClock()
	db := openStore(t, t.TempDir(), clk)
	good := func() Options { return testOptions(clk, testOrigin) }
	for _, tc := range []struct {
		name string
		mod  func(*Options)
		want string
	}{
		{"short session key", func(o *Options) { o.Keys.Session = make([]byte, 31) }, "session key has 31 bytes"},
		{"no invite key", func(o *Options) { o.Keys.Invite = nil }, "invite key has 0 bytes"},
		{"no Origins", func(o *Options) { o.Origins = nil }, "Options.Origins is nil"},
		{"empty primary origin", func(o *Options) {
			o.Origins = func() Origins { return Origins{} }
		}, "primary origin"},
		{"primary origin with a path", func(o *Options) {
			o.Origins = func() Origins { return Origins{Primary: testOrigin + "/app"} }
		}, "primary origin"},
		{"bad public origin", func(o *Options) {
			o.Origins = func() Origins { return Origins{Primary: testOrigin, Public: []string{"ftp://x.example"}} }
		}, "trusted origin"},
		{"bad argon2 parameters", func(o *Options) { o.Argon = ArgonParams{MemoryKiB: 1, Time: 1, Threads: 1} }, "argon2"},
		{"bad hash budget", func(o *Options) { o.Hashes = HashBudget{Burst: -1, PerSecond: 1} }, "hash budget"},
	} {
		o := good()
		tc.mod(&o)
		svc, err := New(context.Background(), db, o)
		if err == nil || svc != nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: New = %v, %v; want an error with %q", tc.name, svc, err, tc.want)
		}
		var ae *api.Error
		if errors.As(err, &ae) {
			t.Errorf("%s: a startup error is an *api.Error: %v", tc.name, err)
		}
	}
	if _, err := New(context.Background(), nil, good()); err == nil {
		t.Error("New accepted a nil store")
	}
	// A refused New leaves the database alone: no fingerprint was stored.
	if err := db.Read(context.Background(), func(q *store.Q) error {
		_, err := q.GetMeta(metaSessionKeyFP)
		return err
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a refused New stored a fingerprint (GetMeta: %v)", err)
	}
}

// TestNewDefaults: a nil clock, logger and ClientIP have defaults, the zero Argon and Hashes are the production
// values, and the keys are copied.
func TestNewDefaults(t *testing.T) {
	clk := newFakeClock()
	db := openStore(t, t.TempDir(), clk)
	keys := testKeys()
	svc, err := New(context.Background(), db, Options{
		Keys:    keys,
		Origins: func() Origins { return Origins{Primary: "HTTPS://Watch.Example.com:443", Public: []string{testOrigin}} },
		Argon:   testArgon,
	})
	if err != nil {
		t.Fatal(err)
	}
	if svc.origins.Primary != testOrigin {
		t.Errorf("primary origin = %q, want it normalized to %q", svc.origins.Primary, testOrigin)
	}
	if svc.throttle.authHash.rate != (rate{Burst: 20, Every: 200 * time.Millisecond}) {
		t.Errorf("auth-hash rate = %+v, want the default budget", svc.throttle.authHash.rate)
	}
	if d := time.Since(svc.now()); d < 0 || d > time.Minute {
		t.Errorf("the default clock is not time.Now (off by %v)", d)
	}
	keys.Session[0] ^= 0xff
	if bytes.Equal(svc.keys.Session, keys.Session) {
		t.Error("the service shares the caller's key slice")
	}

	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, testOrigin+"/", nil)
	for remote, want := range map[string]string{
		"203.0.113.7:51234":          "203.0.113.7",
		"[::ffff:203.0.113.7]:51234": "203.0.113.7",
		"[2001:db8::1%eth0]:51234":   "2001:db8::1",
		"@":                          "",
	} {
		r.RemoteAddr = remote
		if got := ipString(svc.clientIP(r)); got != want {
			t.Errorf("default ClientIP(%q) = %q, want %q", remote, got, want)
		}
	}
}

// keyFixture is what the rotation tests put in the database: for each key, rows that key protects.
type keyFixture struct {
	alex, sam store.User
	invite    store.Invite
	revoked   store.Invite
}

// seedKeyRows fills every table a key protects (03 §4.6): two users with sessions and a push subscription, an
// active and a revoked invite, a setup token and a reset link; then, with the store closed (the store's API has no
// device writers before M2), a device with its tokens and two device codes.
func seedKeyRows(t *testing.T, e *svcEnv) keyFixture {
	t.Helper()
	var f keyFixture
	f.alex = e.addUser("Alex", func(u *store.User) { u.Role = store.RoleAdmin })
	f.sam = e.addUser("Sam", func(u *store.User) { u.Status = store.StatusDisabled })
	now := e.clk.now()
	e.write(func(q *store.Q) error {
		for i, u := range []store.User{f.alex, f.alex, f.sam} {
			s := store.Session{UserID: u.ID, TokenHash: fmt.Appendf(nil, "session-hash-%d", i), Name: "Chrome on Windows",
				LastIP: ipA, IdleExpiresAt: now.Add(sessionIdleTTL), ExpiresAt: now.Add(sessionMaxTTL)}
			if err := q.CreateSession(&s); err != nil {
				return err
			}
			if i == 0 {
				sub := store.PushSubscription{UserID: u.ID, SessionID: s.ID, Endpoint: "https://push.example/sub/1",
					P256dh: "p256dh", Auth: "auth", Name: "Chrome on Windows"}
				if _, err := q.UpsertPushSubscription(&sub); err != nil {
					return err
				}
			}
		}
		f.invite = store.Invite{TokenHash: []byte("invite-hash-1"), CreatedBy: &store.UserRef{ID: f.alex.ID},
			ExpiresAt: now.Add(7 * 24 * time.Hour), MaxUses: 10}
		if err := q.CreateInvite(&f.invite); err != nil {
			return err
		}
		f.revoked = store.Invite{TokenHash: []byte("invite-hash-2"), ExpiresAt: now.Add(7 * 24 * time.Hour), MaxUses: 1}
		if err := q.CreateInvite(&f.revoked); err != nil {
			return err
		}
		if err := q.RevokeInvite(f.revoked.ID, f.alex.ID, now.Add(-time.Hour)); err != nil {
			return err
		}
		if err := q.ReplaceSetupToken([]byte("setup-hash"), now, now.Add(setupTokenTTL)); err != nil {
			return err
		}
		return q.ReplacePasswordReset(store.PasswordReset{TokenHash: []byte("reset-hash"), UserID: f.sam.ID,
			ExpiresAt: now.Add(24 * time.Hour)})
	})
	return f
}

// seedDeviceRows inserts, on the closed database, a device of user with an access token, a device code that user
// approved and a device code nobody decided yet.
func seedDeviceRows(e *svcEnv, user store.UserID) {
	ms := e.clk.now().UnixMilli()
	e.rawExec(`INSERT INTO devices (id, user_id, name, client_kind, os, app_version, linked_via, created_at,
		last_seen_at, last_ip) VALUES ('d1d1d1d1d1d1', ?, 'Alex-PC', 'desktop', 'windows', '0.2.0', 'device_flow', ?, ?,
		'203.0.113.7')`, string(user), ms, ms)
	e.rawExec(`INSERT INTO device_tokens (token_hash, device_id, kind, created_at, expires_at)
		VALUES (x'aa', 'd1d1d1d1d1d1', 'access', ?, ?)`, ms, ms+900_000)
	e.rawExec(`INSERT INTO push_subscriptions (id, user_id, device_id, endpoint, p256dh, auth_secret, name, created_at)
		VALUES ('p2p2p2p2p2p2', ?, 'd1d1d1d1d1d1', 'https://push.example/sub/2', 'p', 'a', 'isshoni for Windows', ?)`,
		string(user), ms)
	e.rawExec(`INSERT INTO device_codes (device_code_hash, user_code_hash, client_kind, device_name, os, app_version,
		request_ip, created_at, expires_at, status, user_id, decided_at)
		VALUES (x'01', x'02', 'desktop', 'Alex-PC', 'windows', '0.2.0', '203.0.113.7', ?, ?, 'approved', ?, ?)`,
		ms, ms+600_000, string(user), ms)
	e.rawExec(`INSERT INTO device_codes (device_code_hash, user_code_hash, client_kind, device_name, os, app_version,
		request_ip, created_at, expires_at) VALUES (x'03', x'04', 'mobile', 'Phone', 'ios', '0.2.0', '203.0.113.7', ?, ?)`,
		ms, ms+600_000)
}

// rowCounts counts the rows of the tables the two keys protect.
func (e *svcEnv) rowCounts() map[string]int {
	out := map[string]int{}
	for _, table := range []string{"sessions", "devices", "device_tokens", "device_codes", "push_subscriptions",
		"invites", "setup_tokens", "password_resets", "users"} {
		out[table] = e.rawCount(table)
	}
	return out
}

// activeInvites counts the invites that still work.
func (e *svcEnv) activeInvites() int {
	e.t.Helper()
	n := 0
	e.read(func(q *store.Q) error {
		var err error
		n, err = q.CountActiveInvites("", e.clk.now())
		return err
	})
	return n
}

func otherKey(b byte) []byte { return bytes.Repeat([]byte{b}, keySize) }

// TestKeyFingerprints is 03 §15's key-fingerprint case (03 §4.6): the first start stores the fingerprints; a changed
// session key deletes the sessions and the device tables; a changed invite key removes the invites and the setup
// and reset tokens; both write the secrets.rotated audit row and raise the secrets_rotated alert.
func TestKeyFingerprints(t *testing.T) {
	e := newSvcEnv(t)

	// The first start stored both fingerprints and found nothing to purge.
	metaValue := func(key string) string {
		var v string
		e.read(func(q *store.Q) error {
			var err error
			v, err = q.GetMeta(key)
			return err
		})
		return v
	}
	keys := testKeys()
	if got, want := metaValue(metaSessionKeyFP), keyFingerprint(keys.Session); got != want {
		t.Fatalf("session_key_fp = %q, want %q", got, want)
	}
	if got, want := metaValue(metaInviteKeyFP), keyFingerprint(keys.Invite); got != want {
		t.Fatalf("invite_key_fp = %q, want %q", got, want)
	}
	if rows, alerts := e.audit(auditSecretsRotated), e.alerts.take(); len(rows) != 0 || len(alerts) != 0 {
		t.Fatalf("a first start audited %d rows and raised %d alerts", len(rows), len(alerts))
	}

	f := seedKeyRows(t, e)
	full := map[string]int{"sessions": 3, "devices": 1, "device_tokens": 1, "device_codes": 2,
		"push_subscriptions": 2, "invites": 2, "setup_tokens": 1, "password_resets": 1, "users": 2}

	// A restart with the same keys changes nothing.
	e.restart(func() { seedDeviceRows(e, f.alex.ID) })
	if got := e.rowCounts(); fmt.Sprint(got) != fmt.Sprint(full) {
		t.Fatalf("after a restart with the same keys: rows %v, want %v", got, full)
	}
	if rows, alerts := e.audit(auditSecretsRotated), e.alerts.take(); len(rows) != 0 || len(alerts) != 0 {
		t.Fatalf("unchanged keys audited %d rows and raised %d alerts", len(rows), len(alerts))
	}

	// A new session key: every session and device row goes, with the push subscriptions that hang on them; the
	// invite key's rows stay.
	e.advance(time.Minute)
	rotatedAt := e.clk.now()
	e.restart(nil, func(o *Options) { o.Keys.Session = otherKey(0x33) })
	got := e.rowCounts()
	for table, want := range map[string]int{"sessions": 0, "devices": 0, "device_tokens": 0, "push_subscriptions": 0,
		"invites": 2, "setup_tokens": 1, "password_resets": 1, "users": 2} {
		if got[table] != want {
			t.Errorf("after a session-key change: %d %s rows, want %d", got[table], table, want)
		}
	}
	// The device code a user decided is gone. The one nobody decided has no user to select it by, and the store has
	// no method that deletes it (purgeRotated); it expires in 10 minutes and the janitor prunes it.
	if got["device_codes"] != 1 {
		t.Errorf("after a session-key change: %d device_codes rows, want only the undecided one", got["device_codes"])
	}
	if n := e.activeInvites(); n != 1 {
		t.Errorf("a session-key change touched the invites: %d active, want 1", n)
	}
	rows := e.audit(auditSecretsRotated)
	if len(rows) != 1 {
		t.Fatalf("%d secrets.rotated rows, want 1", len(rows))
	}
	if r := rows[0]; r.Actor != store.SystemActor || fmt.Sprint(r.Detail["keys"]) != "[session]" || !r.At.Equal(rotatedAt) ||
		r.TargetKind != "" {
		t.Errorf("secrets.rotated row = %+v, want actor system and keys [session]", r)
	}
	alerts := e.alerts.take()
	if len(alerts) != 1 || alerts[0] != (AdminAlert{Kind: AlertSecretsRotated, Actor: "system", At: rotatedAt}) {
		t.Fatalf("alerts = %+v, want one secrets_rotated from system", alerts)
	}
	if calls := e.conns.take(); len(calls) != 0 {
		t.Errorf("the startup purge closed connections: %+v (none exists yet, 03 §7.7)", calls)
	}
	if got, want := metaValue(metaSessionKeyFP), keyFingerprint(otherKey(0x33)); got != want {
		t.Fatalf("session_key_fp = %q after the rotation, want %q", got, want)
	}
	// The row is a security event (03 §10).
	e.read(func(q *store.Q) error {
		events, err := q.SecurityEvents(rotatedAt.Add(-time.Hour), 10)
		if err == nil && (len(events) != 1 || events[0].Action != auditSecretsRotated) {
			t.Errorf("security events = %+v, want the secrets.rotated row", events)
		}
		return err
	})

	// The next start with the new key purges nothing more.
	e.restart(nil, func(o *Options) { o.Keys.Session = otherKey(0x33) })
	if rows, alerts := e.audit(auditSecretsRotated), e.alerts.take(); len(rows) != 1 || len(alerts) != 0 {
		t.Fatalf("a second start with the rotated key: %d rows, %d alerts", len(rows), len(alerts))
	}

	// A new invite key: the setup token and the reset link go and no invite works any more; the sessions stay.
	alex := e.login("Alex", ipA)
	e.advance(time.Minute)
	e.restart(nil, func(o *Options) { o.Keys.Session, o.Keys.Invite = otherKey(0x33), otherKey(0x44) })
	got = e.rowCounts()
	for table, want := range map[string]int{"setup_tokens": 0, "password_resets": 0, "sessions": 1, "users": 2} {
		if got[table] != want {
			t.Errorf("after an invite-key change: %d %s rows, want %d", got[table], table, want)
		}
	}
	if n := e.activeInvites(); n != 0 {
		t.Errorf("after an invite-key change: %d invites still work, want 0", n)
	}
	// The store's API cannot delete an invite, so the purge revokes them (purgeRotated): both rows are still there,
	// revoked, the already revoked one with its first revocation.
	e.read(func(q *store.Q) error {
		invites, err := q.ListInvites("", true)
		if err != nil {
			return err
		}
		if len(invites) != 2 {
			t.Errorf("%d invite rows, want 2", len(invites))
		}
		for _, inv := range invites {
			if inv.State(e.clk.now()) != "revoked" {
				t.Errorf("invite %s is %s, want revoked", inv.ID, inv.State(e.clk.now()))
			}
			if inv.ID == f.revoked.ID && inv.RevokedBy != f.alex.ID {
				t.Errorf("the purge overwrote an earlier revocation: %+v", inv)
			}
		}
		return nil
	})
	if s := e.session(f.alex.ID, alex.Session.ID); s == nil {
		t.Error("an invite-key change deleted a session")
	}
	rows = e.audit(auditSecretsRotated)
	if len(rows) != 2 || fmt.Sprint(rows[1].Detail["keys"]) != "[invite]" {
		t.Fatalf("secrets.rotated rows = %+v, want a second one with keys [invite]", rows)
	}
	if alerts := e.alerts.take(); len(alerts) != 1 || alerts[0].Kind != AlertSecretsRotated {
		t.Fatalf("alerts = %+v, want one secrets_rotated", alerts)
	}

	// Both keys at once: one audit row names both, and one alert goes out.
	e.write(func(q *store.Q) error {
		return q.ReplaceSetupToken([]byte("setup-hash-2"), e.clk.now(), e.clk.now().Add(setupTokenTTL))
	})
	e.restart(nil, func(o *Options) { o.Keys.Session, o.Keys.Invite = otherKey(0x55), otherKey(0x66) })
	got = e.rowCounts()
	if got["sessions"] != 0 || got["setup_tokens"] != 0 {
		t.Errorf("after both keys changed: rows %v, want no sessions and no setup tokens", got)
	}
	rows = e.audit(auditSecretsRotated)
	if len(rows) != 3 || fmt.Sprint(rows[2].Detail["keys"]) != "[session invite]" {
		t.Fatalf("secrets.rotated rows = %+v, want a third one with keys [session invite]", rows)
	}
	if alerts := e.alerts.take(); len(alerts) != 1 {
		t.Fatalf("%d alerts for one start, want 1", len(alerts))
	}
	if !strings.Contains(e.logs.String(), `msg="admin alert" component=auth kind=secrets_rotated`) {
		t.Errorf("no WARN line for the alert in the log:\n%s", e.logs.String())
	}
}

// TestKeyRotationInvalidatesOldTokens: after a session-key change, a cookie of the old key is unauthenticated, and
// the account logs in again under the new key.
func TestKeyRotationInvalidatesOldTokens(t *testing.T) {
	e := newSvcEnv(t)
	e.addUser("Alex")
	old := e.login("Alex", ipA)
	if _, err := e.svc.Authenticate(e.request(http.MethodGet, old.Token)); err != nil {
		t.Fatal(err)
	}
	e.restart(nil, func(o *Options) { o.Keys.Session = otherKey(0x33) })
	_, err := e.svc.Authenticate(e.request(http.MethodGet, old.Token))
	wantCode(t, err, api.CodeUnauthenticated)
	fresh := e.login("Alex", ipA)
	if _, err := e.svc.Authenticate(e.request(http.MethodGet, fresh.Token)); err != nil {
		t.Fatalf("a session under the new key: %v", err)
	}
}

// TestNewWithoutAlerter: a nil Options.Alerts only logs the alert.
func TestNewWithoutAlerter(t *testing.T) {
	e := newSvcEnv(t)
	e.restart(nil, func(o *Options) { o.Keys.Invite, o.Alerts = otherKey(0x77), nil })
	if rows := e.audit(auditSecretsRotated); len(rows) != 1 {
		t.Fatalf("%d secrets.rotated rows, want 1", len(rows))
	}
	if !strings.Contains(e.logs.String(), "kind=secrets_rotated") {
		t.Errorf("the alert was not logged:\n%s", e.logs.String())
	}
}

// TestReqMetaNeverPrintsTheToken: a ReqMeta carries the raw session cookie, and no fmt verb, slog handler or JSON
// encoding shows it (README §4: tokens and cookies are never logged). The other two fields stay readable.
func TestReqMetaNeverPrintsTheToken(t *testing.T) {
	token := newToken(sessionTokenBytes)
	m := ReqMeta{IP: netip.MustParseAddr(ipA), UserAgent: "TestBrowser/1.0", SessionToken: token}
	type call struct {
		Name string
		Meta ReqMeta
	}
	nested := call{Name: "login", Meta: m}
	leaks := func(out string) bool {
		return strings.Contains(out, token) || strings.Contains(out, token[:12]) ||
			strings.Contains(strings.ToLower(out), hex.EncodeToString([]byte(token))[:24])
	}

	outputs := map[string]string{}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
		for name, v := range map[string]any{"ReqMeta": m, "*ReqMeta": &m, "struct": nested, "*struct": &nested,
			"slice": []ReqMeta{m}, "map": map[string]ReqMeta{"k": m}} {
			outputs["fmt "+verb+" "+name] = fmt.Sprintf(verb, v)
		}
	}
	for name, h := range map[string]func(*bytes.Buffer) slog.Handler{
		"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
		"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
	} {
		var b bytes.Buffer
		slog.New(h(&b)).Info("x", "meta", m, "ptr", &m, "call", nested, "callptr", &nested,
			"group", slog.GroupValue(slog.Any("meta", m)))
		outputs["slog "+name+" handler"] = b.String()
	}
	for name, v := range map[string]any{"ReqMeta": m, "*ReqMeta": &m, "struct": nested} {
		j, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("json.Marshal(%s): %v", name, err)
		}
		outputs["json.Marshal "+name] = string(j)
	}
	for name, out := range outputs {
		if leaks(out) {
			t.Errorf("%s printed the session token: %s", name, out)
		}
	}

	// What is printed instead: the placeholder, next to the address and the User-Agent.
	if got, want := fmt.Sprintf("%v", m), "{"+ipA+" TestBrowser/1.0 [redacted]}"; got != want {
		t.Errorf("%%v = %s, want %s", got, want)
	}
	if got, want := fmt.Sprintf("%+v", &nested),
		"&{Name:login Meta:{IP:"+ipA+" UserAgent:TestBrowser/1.0 SessionToken:[redacted]}}"; got != want {
		t.Errorf("%%+v of a struct that holds one = %s, want %s", got, want)
	}
	if got := fmt.Sprintf("%#v", m); !strings.HasPrefix(got, "auth.ReqMeta{IP:") ||
		!strings.HasSuffix(got, `UserAgent:"TestBrowser/1.0", SessionToken:"[redacted]"}`) {
		t.Errorf("%%#v = %s", got)
	}
	if got, want := outputs["json.Marshal ReqMeta"], `{"IP":"`+ipA+`","UserAgent":"TestBrowser/1.0"}`; got != want {
		t.Errorf("json.Marshal = %s, want %s", got, want)
	}
	var b bytes.Buffer
	slog.New(slog.NewTextHandler(&b, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}})).Info("x", "meta", m)
	if got, want := b.String(), "level=INFO msg=x meta.remote_ip="+ipA+
		" meta.user_agent=TestBrowser/1.0 meta.session_token=[redacted]\n"; got != want {
		t.Errorf("slog text = %q, want %q", got, want)
	}

	// No cookie, no placeholder: an empty token stays empty.
	m.SessionToken = ""
	if got, want := fmt.Sprintf("%+v", m), "{IP:"+ipA+" UserAgent:TestBrowser/1.0 SessionToken:}"; got != want {
		t.Errorf("%%+v without a token = %s, want %s", got, want)
	}
	if got := m.LogValue().String(); strings.Contains(got, "redacted") {
		t.Errorf("LogValue without a token = %s", got)
	}
}

// TestInternalErrors: an unexpected failure is an *api.Error{internal} for callers and keeps its cause.
func TestInternalErrors(t *testing.T) {
	err := internalErr("login", io.ErrUnexpectedEOF)
	if !api.IsCode(err, api.CodeInternal) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("internalErr = %v; want *api.Error{internal} that wraps its cause", err)
	}
	if got := serviceErr("login", io.ErrUnexpectedEOF); !api.IsCode(got, api.CodeInternal) {
		t.Fatalf("serviceErr of a plain error = %v", got)
	}
	taken := fmt.Errorf("wrapped: %w", api.NewError(api.CodeUsernameTaken))
	if got := serviceErr("setup", taken); !api.IsCode(got, api.CodeUsernameTaken) || !errors.Is(got, taken) {
		t.Fatalf("serviceErr changed an *api.Error: %v", got)
	}

	// A closed store: every entry point answers internal, never a wrong verdict.
	e := newSvcEnv(t)
	e.addUser("Alex")
	res := e.login("Alex", ipA)
	if err := e.db.Close(); err != nil {
		t.Fatal(err)
	}
	e.advance(sessionCacheTTL) // past the session cache
	ctx := context.Background()
	if _, err := e.svc.Authenticate(e.request(http.MethodGet, res.Token)); !api.IsCode(err, api.CodeInternal) {
		t.Errorf("Authenticate on a closed store: %v", err)
	}
	if _, err := e.svc.AuthenticateCookie(e.request(http.MethodGet, res.Token)); !api.IsCode(err, api.CodeInternal) {
		t.Errorf("AuthenticateCookie on a closed store: %v", err)
	}
	p := principalOf(res.Session, res.User)
	if err := e.svc.Touch(ctx, p, netip.MustParseAddr(ipA)); !api.IsCode(err, api.CodeInternal) {
		t.Errorf("Touch on a closed store: %v", err)
	}
	if err := e.svc.Logout(ctx, p, meta(ipA)); !api.IsCode(err, api.CodeInternal) {
		t.Errorf("Logout on a closed store: %v", err)
	}
	if _, err := e.svc.Login(ctx, "Alex", testPassword, meta(ipB)); !api.IsCode(err, api.CodeInternal) {
		t.Errorf("Login on a closed store: %v", err)
	}
	if _, err := e.svc.SetupAvailable(ctx); !api.IsCode(err, api.CodeInternal) {
		t.Errorf("SetupAvailable on a closed store: %v", err)
	}
	if _, err := e.svc.IssueSetupToken(ctx, store.CLIActor); !api.IsCode(err, api.CodeInternal) {
		t.Errorf("IssueSetupToken on a closed store: %v", err)
	}
	if err := e.svc.CheckSetupToken(ctx, newToken(setupTokenBytes), meta(ipB)); !api.IsCode(err, api.CodeInternal) {
		t.Errorf("CheckSetupToken on a closed store: %v", err)
	}
}

// TestLaterMethodsSayNotImplemented: the methods of the later auth slices still answer with an error that names
// the method and wraps internal.
func TestLaterMethodsSayNotImplemented(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	var p Principal
	var m ReqMeta
	check := func(method string, err error) {
		t.Helper()
		if !api.IsCode(err, api.CodeInternal) || !strings.Contains(err.Error(), method+" not implemented") {
			t.Errorf("%s: %v", method, err)
		}
	}
	check("LogoutEverywhere", e.svc.LogoutEverywhere(ctx, p, m))
	_, err := e.svc.CheckInvite(ctx, "", m)
	check("CheckInvite", err)
	_, err = e.svc.Register(ctx, RegisterInput{}, m)
	check("Register", err)
	_, err = e.svc.ChangePassword(ctx, p, "", "", m)
	check("ChangePassword", err)
	check("DeleteSelf", e.svc.DeleteSelf(ctx, p, "", m))
	check("RevokeSession", e.svc.RevokeSession(ctx, p, "", m))
	_, err = e.svc.RevokeOtherSessions(ctx, p, m)
	check("RevokeOtherSessions", err)
	check("RevokeDevice", e.svc.RevokeDevice(ctx, p, "", m))
	_, _, err = e.svc.CreateInvite(ctx, store.CLIActor, InviteInput{})
	check("CreateInvite", err)
	check("RevokeInvite", e.svc.RevokeInvite(ctx, store.CLIActor, ""))
	_, err = e.svc.Approve(ctx, store.CLIActor, "")
	check("Approve", err)
	check("Reject", e.svc.Reject(ctx, store.CLIActor, ""))
	_, err = e.svc.RejectAll(ctx, store.CLIActor)
	check("RejectAll", err)
	_, err = e.svc.UpdateUser(ctx, store.CLIActor, "", UserChange{})
	check("UpdateUser", err)
	check("DeleteUser", e.svc.DeleteUser(ctx, store.CLIActor, ""))
	_, _, err = e.svc.SignOutUser(ctx, store.CLIActor, "")
	check("SignOutUser", err)
	_, err = e.svc.IssuePasswordReset(ctx, store.CLIActor, "", "")
	check("IssuePasswordReset", err)
	_, err = e.svc.CheckPasswordReset(ctx, "", m)
	check("CheckPasswordReset", err)
	_, err = e.svc.CompletePasswordReset(ctx, "", "", m)
	check("CompletePasswordReset", err)
	_, err = e.svc.UserByUsername(ctx, "")
	check("UserByUsername", err)
	_, err = e.svc.AuthenticateBearerToken(ctx, "")
	check("AuthenticateBearerToken", err)
	check("DecideUserCode", e.svc.DecideUserCode(ctx, p, "", false, m))

	// RunJanitor blocks until its context ends.
	jctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		e.svc.RunJanitor(jctx)
		close(done)
	}()
	cancel()
	<-done
}
