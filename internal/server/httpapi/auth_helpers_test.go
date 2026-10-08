package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/auth"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// The integration fixture of 03 §15: 04's router and the /api/v1 chain over a real store and a real auth.Service,
// with a manual clock and fake Signal, ConnCloser and AdminAlerter.

const (
	testPassword = "correct horse battery"
	// The session cookie of an https origin (03 §7.4).
	sessionCookieName = "__Host-isshoni_session"
	// Client addresses of the documentation ranges.
	addrA = "203.0.113.7"
	addrB = "198.51.100.23"
	addrC = "192.0.2.99"

	uaChromeWindows = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) " +
		"Chrome/141.0.0.0 Safari/537.36"
)

// manualClock is a clock the test moves.
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func newManualClock() *manualClock {
	return &manualClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *manualClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// closedConn is one CloseConnections call.
type closedConn struct {
	sel    auth.ConnSelector
	reason string
}

// recConns is an auth.ConnCloser that records its calls.
type recConns struct {
	mu    sync.Mutex
	calls []closedConn
}

func (c *recConns) CloseConnections(sel auth.ConnSelector, reason string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, closedConn{sel, reason})
	return 1
}

func (c *recConns) take() []closedConn {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.calls
	c.calls = nil
	return out
}

// recAlerts is an auth.AdminAlerter that records its alerts.
type recAlerts struct {
	mu     sync.Mutex
	alerts []auth.AdminAlert
}

func (a *recAlerts) AdminAlert(_ context.Context, al auth.AdminAlert) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.alerts = append(a.alerts, al)
}

// take returns the Signal hooks recorded so far and clears them.
func (f *fakeSignal) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
}

// authFixture is the whole stack with the real auth.Service.
type authFixture struct {
	*fixture
	t      *testing.T
	clk    *manualClock
	dbPath string
	db     *store.DB
	svc    *auth.Service
	api    *API
	conns  *recConns
	alerts *recAlerts
	signal *fakeSignal
}

// testAuthOptions are auth.Options for the test origin: fast argon2 parameters and a hash budget that is out of the
// way (03 §15: the throttle tests set Options.Hashes large, except where auth-hash is emptied).
func testAuthOptions(clk *manualClock) auth.Options {
	return auth.Options{
		Keys:     auth.Keys{Session: bytes.Repeat([]byte{0x11}, 32), Invite: bytes.Repeat([]byte{0x22}, 32)},
		Origins:  func() auth.Origins { return auth.Origins{Primary: testOrigin, Public: []string{testOrigin}} },
		ClientIP: ClientIP, // as the wiring does (04 §6.6)
		Argon:    auth.ArgonParams{MemoryKiB: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32},
		Hashes:   auth.HashBudget{Burst: 1 << 20, PerSecond: 1 << 20},
		Clock:    clk.now,
		Logger:   slog.New(slog.DiscardHandler),
	}
}

func newAuthFixture(t *testing.T, mods ...func(*auth.Options)) *authFixture {
	t.Helper()
	f := &authFixture{t: t, clk: newManualClock(), conns: &recConns{}, alerts: &recAlerts{}, signal: &fakeSignal{}}
	f.dbPath = filepath.Join(t.TempDir(), "isshoni.db")
	db, err := store.Open(context.Background(), store.Options{
		Path:       f.dbPath,
		AppVersion: "0.1.0",
		Clock:      f.clk.now,
		Logger:     slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	f.db = db
	o := testAuthOptions(f.clk)
	o.Conns, o.Alerts = f.conns, f.alerts
	for _, mod := range mods {
		mod(&o)
	}
	if f.svc, err = auth.New(context.Background(), db, o); err != nil {
		t.Fatal(err)
	}
	f.api = New(Deps{
		DB:     db,
		Auth:   f.svc,
		Signal: f.signal,
		Push:   &fakePush{key: testVAPIDKey},
		Info:   fakeInfo{version: "1.2.3", current: 3, minimum: 2},
		Site:   domainSite(),
		Clock:  f.clk.now,
		Logger: slog.New(slog.DiscardHandler),
	})
	f.fixture = newFixture(t, func(o *RouterOptions) { o.API = f.api })
	return f
}

// reqOpt changes a request before it is served.
type reqOpt func(*http.Request)

// from sets the client address.
func from(ip string) reqOpt {
	return func(r *http.Request) {
		if strings.Contains(ip, ":") {
			ip = "[" + ip + "]"
		}
		r.RemoteAddr = ip + ":50000"
	}
}

// with sends back a cookie the server set; nil sends none.
func with(c *http.Cookie) reqOpt {
	if c == nil {
		return func(*http.Request) {}
	}
	return cookieValue(c.Name, c.Value)
}

// cookieValue sends a cookie with that name and value.
func cookieValue(name, value string) reqOpt {
	return func(r *http.Request) { r.Header.Add("Cookie", name+"="+value) }
}

// header sets a header; an empty value removes it.
func header(key, value string) reqOpt {
	return func(r *http.Request) {
		if value == "" {
			r.Header.Del(key)
			return
		}
		r.Header.Set(key, value)
	}
}

// call serves one request through the whole chain, as the SPA sends it: same-origin, with a JSON Content-Type on
// unsafe methods, from address A with a Chrome-on-Windows User-Agent.
func (f *authFixture) call(method, path, body string, opts ...reqOpt) *httptest.ResponseRecorder {
	f.t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	var req *http.Request
	if rd != nil {
		req = httptest.NewRequestWithContext(context.Background(), method, testOrigin+path, rd)
	} else {
		req = httptest.NewRequestWithContext(context.Background(), method, testOrigin+path, nil)
	}
	req.Header.Set("User-Agent", uaChromeWindows)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("Origin", testOrigin)
		req.Header.Set("Content-Type", "application/json")
	}
	from(addrA)(req)
	for _, opt := range opts {
		opt(req)
	}
	rec := f.serve(req)
	if cc := rec.Header().Values("Cache-Control"); len(cc) != 1 || cc[0] != cacheNoStore {
		f.t.Errorf("%s %s: Cache-Control %q, want exactly no-store (03 §12.1)", method, path, cc)
	}
	return rec
}

// jsonBody marshals a request body.
func jsonBody(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// decodeBody decodes a JSON response body of the expected status.
func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder, status int) T {
	t.Helper()
	var v T
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, status, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != jsonContentType {
		t.Errorf("Content-Type = %q, want %q", ct, jsonContentType)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("body %q: %v", rec.Body, err)
	}
	return v
}

// setCookies returns the raw Set-Cookie headers of the session cookie.
func setCookies(rec *httptest.ResponseRecorder) []string {
	var out []string
	for _, v := range rec.Header().Values("Set-Cookie") {
		if strings.HasPrefix(v, sessionCookieName+"=") {
			out = append(out, v)
		}
	}
	return out
}

// sessionCookie returns the session cookie a response set, and fails the test unless it set exactly one with the
// attributes of 03 §7.4 and the given Max-Age.
func (f *authFixture) sessionCookie(rec *httptest.ResponseRecorder, maxAge int) *http.Cookie {
	f.t.Helper()
	raw := setCookies(rec)
	if len(raw) != 1 {
		f.t.Fatalf("Set-Cookie headers for the session: %q, want one", rec.Header().Values("Set-Cookie"))
	}
	var c *http.Cookie
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == sessionCookieName {
			c = ck
		}
	}
	if c == nil || len(c.Value) != 43 || c.Path != "/" || c.Domain != "" || !c.HttpOnly || !c.Secure ||
		c.SameSite != http.SameSiteLaxMode || c.MaxAge != maxAge {
		f.t.Fatalf("session cookie %q: want a 43-character token with Path=/, Max-Age=%d, HttpOnly, Secure and "+
			"SameSite=Lax, and no Domain", raw[0], maxAge)
	}
	return c
}

// clearedCookie is the Set-Cookie of a logout (03 §12.4.2).
const clearedCookie = sessionCookieName + "=; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=Lax"

// thirtyDays is the Max-Age of a fresh session cookie.
const thirtyDays = 30 * 24 * 60 * 60

// setupAdmin runs first-run setup over REST, as `isshoni setup-url` and the wizard do, and returns the admin and its
// session cookie.
func (f *authFixture) setupAdmin(username string) (api.User, *http.Cookie) {
	f.t.Helper()
	token := f.setupToken()
	rec := f.call(http.MethodPost, "/api/v1/auth/setup/complete", jsonBody(f.t, api.SetupCompleteRequest{
		Token: token, Username: username, Password: testPassword}))
	res := decodeBody[api.UserResponse](f.t, rec, http.StatusCreated)
	return res.User, f.sessionCookie(rec, thirtyDays)
}

// setupToken issues a setup link through the CLI path and returns the token of its fragment.
func (f *authFixture) setupToken() string {
	f.t.Helper()
	link, err := f.svc.IssueSetupToken(context.Background(), store.CLIActor)
	if err != nil {
		f.t.Fatal(err)
	}
	token, ok := strings.CutPrefix(link.URL, testOrigin+"/setup#")
	if !ok || len(token) != 43 {
		f.t.Fatalf("setup link %q: want %s/setup#<token>", link.URL, testOrigin)
	}
	return token
}

// login logs in over REST from address ip and returns the response.
func (f *authFixture) login(username, password, ip string, opts ...reqOpt) *httptest.ResponseRecorder {
	f.t.Helper()
	opts = append([]reqOpt{from(ip)}, opts...)
	return f.call(http.MethodPost, "/api/v1/auth/login",
		jsonBody(f.t, api.LoginRequest{Username: username, Password: password}), opts...)
}

// write runs fn in a store Write.
func (f *authFixture) write(fn func(q *store.Q) error) {
	f.t.Helper()
	if err := f.db.Write(context.Background(), fn); err != nil {
		f.t.Fatal(err)
	}
}

// read runs fn in a store Read.
func (f *authFixture) read(fn func(q *store.Q) error) {
	f.t.Helper()
	if err := f.db.Read(context.Background(), fn); err != nil {
		f.t.Fatal(err)
	}
}

// audit returns the actions of the audit log, oldest first.
func (f *authFixture) audit() []store.AuditEntry {
	f.t.Helper()
	var out []store.AuditEntry
	f.read(func(q *store.Q) error {
		rows, err := q.ListAudit(store.AuditQuery{Limit: 200})
		for i := len(rows) - 1; i >= 0; i-- {
			out = append(out, rows[i])
		}
		return err
	})
	return out
}

// rawDB opens a second connection to the database file (the "sqlite" driver is the store's).
func (f *authFixture) rawDB() *sql.DB {
	f.t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(f.dbPath)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		f.t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	f.t.Cleanup(func() { _ = db.Close() })
	return db
}
