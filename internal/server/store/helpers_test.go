package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// testClock is an injected clock that tests move by hand.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 10, 14, 2, 15, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// testEnv is a data directory with an injected clock.
type testEnv struct {
	t     *testing.T
	dir   string
	clock *testClock
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	return &testEnv{t: t, dir: t.TempDir(), clock: newTestClock()}
}

func (e *testEnv) dbPath() string    { return filepath.Join(e.dir, "isshoni.db") }
func (e *testEnv) backupDir() string { return filepath.Join(e.dir, "backups") }

// opts returns Options for this env with the given migration list (nil = embedded).
func (e *testEnv) opts(migs []migration) Options {
	return Options{
		Path:       e.dbPath(),
		BackupDir:  e.backupDir(),
		AppVersion: "0.1.0",
		Clock:      e.clock.Now,
		Logger:     slog.New(slog.DiscardHandler),
		migrations: migs,
	}
}

// open opens the store and closes it at the end of the test.
func (e *testEnv) open(migs []migration) *DB {
	e.t.Helper()
	db, err := Open(context.Background(), e.opts(migs))
	if err != nil {
		e.t.Fatalf("Open: %v", err)
	}
	e.t.Cleanup(func() { _ = db.Close() })
	return db
}

// openClose opens and closes the store once (one "server start").
func (e *testEnv) openClose(migs []migration) error {
	db, err := Open(context.Background(), e.opts(migs))
	if err != nil {
		return err
	}
	return db.Close()
}

// backups returns the file names in the backup directory, sorted.
func (e *testEnv) backups() []string {
	e.t.Helper()
	entries, err := os.ReadDir(e.backupDir())
	if err != nil {
		e.t.Fatalf("read backups: %v", err)
	}
	var names []string
	for _, en := range entries {
		names = append(names, en.Name())
	}
	slices.Sort(names)
	return names
}

// fakeMigrations returns the embedded migrations followed by trivial ones up to version n.
func fakeMigrations(n int) []migration {
	ms := slices.Clone(embeddedMigrations())
	for v := len(ms) + 1; v <= n; v++ {
		ms = append(ms, migration{
			version: v,
			name:    fmt.Sprintf("%04d_fake_%d", v, v),
			sql:     fmt.Sprintf("CREATE TABLE fake_%d (x INTEGER NOT NULL) STRICT;\nINSERT INTO fake_%d VALUES (%d);", v, v, v),
		})
	}
	return ms
}

// rawDB opens path directly (read-write, foreign keys on) for fixtures and checks.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteURI(path, "_pragma=foreign_keys(1)", "_pragma=busy_timeout(5000)"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// fileVersions reads MAX(schema_migrations.version) and PRAGMA user_version of a file without changing it.
func fileVersions(t *testing.T, path string) (recorded, userVersion int) {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteURI(path, "mode=ro"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&recorded); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&userVersion); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return recorded, userVersion
}

// execW runs raw SQL inside a Write (the tests use the transaction directly for fixtures).
func execW(t *testing.T, db *DB, query string, args ...any) error {
	t.Helper()
	return db.Write(context.Background(), func(q *Q) error {
		_, err := q.exec(query, args...)
		return err
	})
}

// mustExecW is execW that fails the test on error.
func mustExecW(t *testing.T, db *DB, query string, args ...any) {
	t.Helper()
	if err := execW(t, db, query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// queryInt runs a one-value query in a Read.
func queryInt(t *testing.T, db *DB, query string, args ...any) int {
	t.Helper()
	var n int
	err := db.Read(context.Background(), func(q *Q) error {
		return q.tx.QueryRowContext(q.ctx, query, args...).Scan(&n)
	})
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}
