package auth

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite" // the "sqlite" driver, for fixtures the store's API cannot write (devices are M2)

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

const (
	testOrigin   = "https://watch.example.com"
	testPassword = "correct horse battery"
	// Addresses of the documentation ranges (RFC 5737, RFC 3849).
	ipA = "203.0.113.7"
	ipB = "198.51.100.23"
	ipC = "192.0.2.99"
)

// bigHashBudget keeps the auth-hash bucket out of the way of tests that are not about it (03 §15).
var bigHashBudget = HashBudget{Burst: 1 << 20, PerSecond: 1 << 20}

// closedConn is one CloseConnections call.
type closedConn struct {
	sel    ConnSelector
	reason string
}

// fakeConns is a ConnCloser that records its calls.
type fakeConns struct {
	mu    sync.Mutex
	calls []closedConn
	// during, if set, runs inside every call, where the hub would be closing connections: a test looks at what
	// the service had done by then (03 §7.7: the commit and the session cache come first).
	during func(sel ConnSelector, reason string)
}

func (f *fakeConns) CloseConnections(sel ConnSelector, reason string) int {
	f.mu.Lock()
	f.calls = append(f.calls, closedConn{sel, reason})
	during := f.during
	f.mu.Unlock()
	if during != nil {
		during(sel, reason)
	}
	return 1
}

// take returns the recorded calls and clears them.
func (f *fakeConns) take() []closedConn {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
}

// fakeAlerts is an AdminAlerter that records its alerts.
type fakeAlerts struct {
	mu     sync.Mutex
	alerts []AdminAlert
}

func (f *fakeAlerts) AdminAlert(_ context.Context, a AdminAlert) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alerts = append(f.alerts, a)
}

func (f *fakeAlerts) take() []AdminAlert {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.alerts
	f.alerts = nil
	return out
}

// lockedBuffer collects log output written from several goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// svcEnv is a Service over a real store in a temporary directory, with a manual clock, a counting hasher and fake
// ConnCloser and AdminAlerter.
type svcEnv struct {
	t      *testing.T
	clk    *fakeClock
	dir    string
	db     *store.DB
	svc    *Service
	conns  *fakeConns
	alerts *fakeAlerts
	logs   *lockedBuffer
	hashes atomic.Int32 // argon2 derivations since the last reset
}

// dbPath is the database file of a test directory.
func dbPath(dir string) string { return filepath.Join(dir, "isshoni.db") }

// openStore opens the store of dir on the test clock; the test closes it at its end (Close is idempotent).
func openStore(t *testing.T, dir string, clk *fakeClock) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), store.Options{
		Path:       dbPath(dir),
		AppVersion: "0.1.0",
		Clock:      clk.now,
		Logger:     slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// testOptions are Options for origin with fast argon2 parameters and no hash budget to speak of.
func testOptions(clk *fakeClock, origin string) Options {
	return Options{
		Keys:    testKeys(),
		Origins: func() Origins { return Origins{Primary: origin, Public: []string{origin}} },
		Argon:   testArgon,
		Hashes:  bigHashBudget,
		Clock:   clk.now,
		Logger:  slog.New(slog.DiscardHandler),
	}
}

// newSvcEnv builds the environment; mods change the Options before New.
func newSvcEnv(t *testing.T, mods ...func(*Options)) *svcEnv {
	t.Helper()
	e := &svcEnv{t: t, clk: newFakeClock(), dir: t.TempDir(), conns: &fakeConns{}, alerts: &fakeAlerts{},
		logs: &lockedBuffer{}}
	e.db = openStore(t, e.dir, e.clk)
	e.start(mods...)
	return e
}

// start builds the Service over e.db (again, after a restart).
func (e *svcEnv) start(mods ...func(*Options)) {
	e.t.Helper()
	o := testOptions(e.clk, testOrigin)
	o.Conns, o.Alerts = e.conns, e.alerts
	o.Logger = slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	for _, mod := range mods {
		mod(&o)
	}
	svc, err := New(context.Background(), e.db, o)
	if err != nil {
		e.t.Fatal(err)
	}
	svc.hasher.derive = countingDerive(&e.hashes)
	e.svc = svc
	e.hashes.Store(0)
}

// restart closes the store, runs between (raw SQL fixtures, if any) and opens the store and the Service again, as a
// server restart does.
func (e *svcEnv) restart(between func(), mods ...func(*Options)) {
	e.t.Helper()
	if err := e.db.Close(); err != nil {
		e.t.Fatal(err)
	}
	if between != nil {
		between()
	}
	e.db = openStore(e.t, e.dir, e.clk)
	e.start(mods...)
}

// rawExec runs SQL on the closed database file.
func (e *svcEnv) rawExec(query string, args ...any) {
	e.t.Helper()
	db := e.raw()
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		e.t.Fatalf("%s: %v", query, err)
	}
}

// rawCount returns SELECT count(*) of a table, on the open or the closed database file.
func (e *svcEnv) rawCount(table string) int {
	e.t.Helper()
	return e.rawInt("SELECT count(*) FROM " + table)
}

// rawInt runs a one-value query on the open or the closed database file.
func (e *svcEnv) rawInt(query string, args ...any) int {
	e.t.Helper()
	db := e.raw()
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// auditCount counts the audit rows of an action.
func (e *svcEnv) auditCount(action string) int {
	e.t.Helper()
	return e.rawInt("SELECT count(*) FROM audit_log WHERE action = ?", action)
}

func (e *svcEnv) raw() *sql.DB {
	e.t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath(e.dir))+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		e.t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	return db
}

func (e *svcEnv) write(fn func(q *store.Q) error) {
	e.t.Helper()
	if err := e.db.Write(context.Background(), fn); err != nil {
		e.t.Fatal(err)
	}
}

func (e *svcEnv) read(fn func(q *store.Q) error) {
	e.t.Helper()
	if err := e.db.Read(context.Background(), fn); err != nil {
		e.t.Fatal(err)
	}
}

// addUser creates an active user with testPassword, hashed with the service's hasher; mods change the row first.
func (e *svcEnv) addUser(name string, mods ...func(*store.User)) store.User {
	e.t.Helper()
	display, key, err := NormalizeUsername(name)
	if err != nil {
		e.t.Fatal(err)
	}
	before := e.hashes.Load()
	u := store.User{Username: display, UsernameKey: key, PasswordHash: e.svc.hasher.compute([]byte(testPassword)),
		Role: store.RoleUser, Status: store.StatusActive, CreatedVia: "invite", CreatedAt: e.clk.now()}
	e.hashes.Store(before) // the fixture's hash does not count
	for _, mod := range mods {
		mod(&u)
	}
	e.write(func(q *store.Q) error { return q.CreateUser(&u) })
	return u
}

// meta is the ReqMeta of a request from ip with a Chrome-on-Windows User-Agent.
func meta(ip string) ReqMeta {
	return ReqMeta{IP: netip.MustParseAddr(ip), UserAgent: uaChromeWindows}
}

const uaChromeWindows = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) " +
	"Chrome/141.0.0.0 Safari/537.36"

// login logs name in with testPassword from ip and fails the test on error.
func (e *svcEnv) login(name, ip string) LoginResult {
	e.t.Helper()
	res, err := e.svc.Login(context.Background(), name, testPassword, meta(ip))
	if err != nil {
		e.t.Fatalf("Login(%s) from %s: %v", name, ip, err)
	}
	return res
}

// sessions returns a user's session rows.
func (e *svcEnv) sessions(u store.UserID) []store.Session {
	e.t.Helper()
	var out []store.Session
	e.read(func(q *store.Q) error {
		var err error
		out, err = q.ListSessions(u)
		return err
	})
	return out
}

// session returns one session row, or nil.
func (e *svcEnv) session(u store.UserID, id store.SessionID) *store.Session {
	e.t.Helper()
	for _, s := range e.sessions(u) {
		if s.ID == id {
			return &s
		}
	}
	return nil
}

// audit returns the audit rows of an action, oldest first.
func (e *svcEnv) audit(action string) []store.AuditEntry {
	e.t.Helper()
	var out []store.AuditEntry
	e.read(func(q *store.Q) error {
		rows, err := q.ListAudit(store.AuditQuery{ActionPrefix: action, Limit: 200})
		for i := len(rows) - 1; i >= 0; i-- {
			if rows[i].Action == action {
				out = append(out, rows[i])
			}
		}
		return err
	})
	return out
}

// request builds a request to the test origin that carries the session cookie with value token ("" for none).
func (e *svcEnv) request(method, token string) *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), method, testOrigin+"/api/v1/me", nil)
	r.RemoteAddr = ipA + ":50000"
	if token != "" {
		r.Header.Add("Cookie", e.svc.cookie.name+"="+token)
	}
	return r
}

// wantCode fails the test unless err is an *api.Error with that code; it returns that error's fields.
func wantCode(t *testing.T, err error, code string) api.Error {
	t.Helper()
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != code {
		t.Fatalf("error = %v, want *api.Error{%s}", err, code)
	}
	return *ae
}

// advance moves the test clock.
func (e *svcEnv) advance(d time.Duration) { e.clk.advance(d) }
