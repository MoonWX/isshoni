// Package store is isshoni's SQLite database (03 §4–6): one file in the data directory, opened through the CGO-free
// modernc.org/sqlite driver with one writer connection (BEGIN IMMEDIATE, queued in Go) and a small read-only pool,
// in WAL mode with synchronous=NORMAL.
//
// Open migrates the file forward (there are no down migrations), takes a backup before migrating an existing
// database, and refuses a schema newer than this binary. Every Open error that a restart can't fix wraps
// ErrNeedsOperator. All reads and writes go through DB.Read and DB.Write, which hand the callback a *Q bound to one
// transaction.
//
// The store imports only the standard library, modernc.org/sqlite and internal/protocol/api; it never reads config
// (the wiring fills Options).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// slowWrite is the Write duration above which a WARN names the caller (03 §4.2).
const slowWrite = 250 * time.Millisecond

// busyTimeoutMS is SQLite's busy_timeout. The writer is one connection, so it only matters for other processes
// (offline CLI commands) and the readers during a checkpoint.
const busyTimeoutMS = 5000

// Options configures Open. The wiring fills it from config; the store never reads config itself.
type Options struct {
	Path       string           // <data_dir>/isshoni.db
	BackupDir  string           // <data_dir>/backups; "" = "backups" next to Path
	AppVersion string           // SemVer of this build, recorded in schema_migrations and meta.last_app_version
	Readers    int              // size of the read-only pool; default min(4, NumCPU)
	Clock      func() time.Time // default time.Now
	Logger     *slog.Logger     // default slog.Default(); the store adds component=store

	// Test hooks; nil means the production behavior.
	migrations []migration                                        // default: the embedded migrations
	freeSpace  func(dir string) (free uint64, ok bool, err error) // default: statfs; ok=false skips the check
}

func (o Options) withDefaults() (Options, error) {
	if o.Path == "" {
		return o, errors.New("store: Options.Path is empty")
	}
	if o.BackupDir == "" {
		o.BackupDir = filepath.Join(filepath.Dir(o.Path), "backups")
	}
	if o.AppVersion == "" {
		o.AppVersion = "unknown"
	}
	if o.Readers <= 0 {
		o.Readers = min(4, runtime.NumCPU())
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	o.Logger = o.Logger.With("component", "store")
	if o.migrations == nil {
		o.migrations = embeddedMigrations()
	}
	if o.freeSpace == nil {
		o.freeSpace = diskFree
	}
	return o, nil
}

// DB is an open store. It is safe for concurrent use. Close it last, after everything that uses it has stopped.
type DB struct {
	opts     Options
	writer   *sql.DB // exactly one connection, BEGIN IMMEDIATE
	reader   *sql.DB // query_only pool, deferred transactions; nil until Open's step 9
	version  int     // schema version after Open
	settings *SettingsCache
	log      *slog.Logger
	now      func() time.Time

	mu        sync.Mutex
	closed    bool
	active    sync.WaitGroup // running Read, Write, Ping, … calls
	closeOnce sync.Once
	closeErr  error
}

// Open opens (creating if needed) the database at o.Path and brings it to this binary's schema (03 §4.3):
//  1. create the backup directory;
//  2. open the writer and check WAL mode;
//  3. create schema_migrations;
//  4. read the current version and check the history against the embedded migrations;
//  5. if the file is newer than this binary, return *SchemaTooNewError;
//  6. if it is older, take the pre-migration backup (03 §4.4), then migrate;
//  7. upsert the meta keys created_at (if missing) and last_app_version;
//  8. EnsureDefaultRoom;
//  9. open the reader pool;
//  10. run PRAGMA optimize=0x10002.
//
// Errors a restart can't fix wrap ErrNeedsOperator. A cancelled ctx never does.
func Open(ctx context.Context, o Options) (_ *DB, err error) {
	o, err = o.withDefaults()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}

	// 1. The backup directory (0700) and the database file (0600; SQLite gives -wal and -shm the same mode).
	if err := ensureDir(o.BackupDir); err != nil {
		return nil, err
	}
	if err := ensureDBFile(o.Path); err != nil {
		return nil, err
	}

	// 2. The writer.
	writer, err := sql.Open("sqlite", writerDSN(o.Path))
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", o.Path, err)
	}
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	writer.SetConnMaxLifetime(0)
	writer.SetConnMaxIdleTime(0)
	db := &DB{
		opts:     o,
		writer:   writer,
		settings: newSettingsCache(),
		log:      o.Logger,
		now:      o.Clock,
	}
	defer func() {
		if err != nil {
			_ = db.closePools() // the Open error is the one that matters
		}
	}()
	if err := checkWAL(ctx, writer, o.Path); err != nil {
		return nil, err
	}

	// 3–6. Bookkeeping, history, newer-schema refusal, backup and migrations.
	m := &migrator{db: db, migs: o.migrations}
	if err := m.run(ctx); err != nil {
		return nil, err
	}
	db.version = len(o.migrations)

	// 7–8. Meta keys and the default room.
	now := db.now()
	err = db.Write(ctx, func(q *Q) error {
		if err := q.setMetaIfMissing("created_at", strconv.FormatInt(unixMS(now), 10)); err != nil {
			return err
		}
		if err := q.SetMeta("last_app_version", o.AppVersion); err != nil {
			return err
		}
		return q.EnsureDefaultRoom(now)
	})
	if err != nil {
		return nil, openErr(ctx, o.Path, "store: initialize meta and default room", err)
	}

	// 9. The reader pool.
	reader, err := sql.Open("sqlite", readerDSN(o.Path))
	if err != nil {
		return nil, fmt.Errorf("store: open readers for %s: %w", o.Path, err)
	}
	reader.SetMaxOpenConns(o.Readers)
	reader.SetMaxIdleConns(o.Readers)
	db.reader = reader
	if err := reader.PingContext(ctx); err != nil {
		return nil, openErr(ctx, o.Path, "store: open readers", err)
	}

	// 10. Analyze the tables that need it, as SQLite recommends right after opening (cheap on a small DB).
	if _, err := writer.ExecContext(ctx, "PRAGMA optimize=0x10002"); err != nil {
		return nil, openErr(ctx, o.Path, "store: PRAGMA optimize", err)
	}
	return db, nil
}

// checkWAL switches the file to WAL mode and fails when SQLite refuses (a network filesystem, for example).
func checkWAL(ctx context.Context, writer *sql.DB, path string) error {
	var mode string
	if err := writer.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return openErr(ctx, path, "store: set WAL mode", err)
	}
	return checkJournalMode(mode, path)
}

func checkJournalMode(mode, path string) error {
	if !strings.EqualFold(mode, "wal") {
		return needsOperator(fmt.Sprintf("store: WAL mode unavailable at %s (network filesystem?)", path), nil)
	}
	return nil
}

// Close rejects new Read/Write calls, waits for running transactions, closes the reader pool, runs
// PRAGMA wal_checkpoint(TRUNCATE) on the writer, then closes it, so the .db file alone is complete after a clean
// shutdown. Idempotent: later calls return the first call's result. 04 calls it after stopping the janitor and the
// push queue.
func (db *DB) Close() error {
	db.closeOnce.Do(func() {
		db.mu.Lock()
		db.closed = true
		db.mu.Unlock()
		db.active.Wait()
		db.closeErr = db.closePools()
	})
	return db.closeErr
}

// closePools closes the readers, checkpoints and closes the writer.
func (db *DB) closePools() error {
	var errs []error
	if db.reader != nil {
		if err := db.reader.Close(); err != nil {
			errs = append(errs, fmt.Errorf("store: close readers: %w", err))
		}
	}
	if db.writer != nil {
		if _, err := db.writer.ExecContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			errs = append(errs, fmt.Errorf("store: checkpoint: %w", err))
		}
		if err := db.writer.Close(); err != nil {
			errs = append(errs, fmt.Errorf("store: close writer: %w", err))
		}
	}
	return errors.Join(errs...)
}

// enter registers a running call; it fails once Close has begun.
func (db *DB) enter() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return errClosed
	}
	db.active.Add(1)
	return nil
}

func (db *DB) leave() { db.active.Done() }

// Read runs fn in a deferred transaction on a reader, so fn sees one consistent snapshot. Write methods of q fail.
// The transaction always rolls back (it changes nothing).
func (db *DB) Read(ctx context.Context, fn func(q *Q) error) error {
	if err := db.enter(); err != nil {
		return err
	}
	defer db.leave()
	tx, err := db.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("store: begin read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	return fn(&Q{ctx: ctx, tx: tx})
}

// Write runs fn in a BEGIN IMMEDIATE transaction on the single writer connection. It commits if fn returns nil and
// rolls back otherwise (also when fn panics). Concurrent Writes queue in Go.
//
// Rules (03 §4.2): no password hashing, network I/O or hub calls inside fn; hooks run after the commit. A Write
// that holds the writer longer than 250 ms logs a WARN with the caller's name. fn must not call Write (the one
// writer connection is taken, so that deadlocks); a Read inside fn does not see fn's uncommitted changes.
func (db *DB) Write(ctx context.Context, fn func(q *Q) error) error {
	if err := db.enter(); err != nil {
		return err
	}
	defer db.leave()
	tx, err := db.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin write: %w", err)
	}
	start := time.Now()
	done := false
	defer func() {
		if !done {
			_ = tx.Rollback()
		}
		if d := time.Since(start); d > slowWrite {
			db.log.Warn("slow write transaction", "caller", callerName(2), "duration", d)
		}
	}()
	if err := fn(&Q{ctx: ctx, tx: tx, writable: true}); err != nil {
		return err
	}
	done = true
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// callerName returns the function name skip frames above its caller, e.g. "…/auth.(*Service).Login".
func callerName(skip int) string {
	pc, _, _, ok := runtime.Caller(skip + 1)
	if !ok {
		return "unknown"
	}
	if f := runtime.FuncForPC(pc); f != nil {
		return f.Name()
	}
	return "unknown"
}

// Ping runs SELECT 1 on the writer and on a reader (04's /readyz check).
func (db *DB) Ping(ctx context.Context) error {
	if err := db.enter(); err != nil {
		return err
	}
	defer db.leave()
	var one int
	if err := db.writer.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("store: ping writer: %w", err)
	}
	if err := db.reader.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("store: ping reader: %w", err)
	}
	return nil
}

// QuickCheck runs PRAGMA quick_check on a reader (doctor). It returns nil when SQLite answers "ok", and otherwise
// an error listing the first problems it found.
func (db *DB) QuickCheck(ctx context.Context) error {
	if err := db.enter(); err != nil {
		return err
	}
	defer db.leave()
	problems, err := pragmaCheck(ctx, db.reader, "PRAGMA quick_check")
	if err != nil {
		return fmt.Errorf("store: quick_check: %w", err)
	}
	if problems != "" {
		return fmt.Errorf("store: quick_check: %s", problems)
	}
	return nil
}

// maxCheckLines is how many problem lines of integrity_check/quick_check go into an error.
const maxCheckLines = 10

// queryer is the read side shared by *sql.DB, *sql.Conn and *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// pragmaCheck runs an integrity_check-like pragma and returns "" for "ok", or the first problem lines joined.
func pragmaCheck(ctx context.Context, q queryer, pragma string) (string, error) {
	rows, err := q.QueryContext(ctx, pragma)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	var lines []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return "", err
		}
		if len(lines) < maxCheckLines {
			lines = append(lines, s)
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(lines) == 1 && lines[0] == "ok" {
		return "", nil
	}
	if len(lines) == 0 {
		return "no result", nil
	}
	return strings.Join(lines, "; "), nil
}

// SchemaVersion is the schema version of the open database (always this binary's latest after Open).
func (db *DB) SchemaVersion() int { return db.version }

// Stats is a summary for doctor and the admin dashboard.
type Stats struct {
	SchemaVersion int
	DBBytes       int64
	WALBytes      int64
	Backups       []string // pre-migration backup file names (pre-<ver>-<ts>.db), newest first
	Users         int      // accounts that are not pending (active or disabled)
	Pending       int      // pending sign-ups (the approval queue)
	Sessions      int
	Devices       int
	ActiveInvites int // not revoked, not expired, not used up
	Rooms         int
	PushSubs      int
	AuditRows     int
}

// Stats reads file sizes, the backup list and row counts (from one reader snapshot).
func (db *DB) Stats(ctx context.Context) (Stats, error) {
	s := Stats{SchemaVersion: db.version}
	var err error
	if s.DBBytes, err = fileSize(db.opts.Path); err != nil {
		return Stats{}, err
	}
	if s.WALBytes, err = fileSize(db.opts.Path + "-wal"); err != nil {
		return Stats{}, err
	}
	backups, err := listBackups(db.opts.BackupDir)
	if err != nil {
		return Stats{}, err
	}
	for _, b := range backups {
		s.Backups = append(s.Backups, b.name)
	}
	now := unixMS(db.now())
	err = db.Read(ctx, func(q *Q) error {
		counts := []struct {
			dst   *int
			query string
			args  []any
		}{
			{&s.Users, "SELECT count(*) FROM users WHERE status <> 'pending'", nil},
			{&s.Pending, "SELECT count(*) FROM users WHERE status = 'pending'", nil},
			{&s.Sessions, "SELECT count(*) FROM sessions", nil},
			{&s.Devices, "SELECT count(*) FROM devices", nil},
			{&s.ActiveInvites, "SELECT count(*) FROM invites WHERE revoked_at IS NULL AND expires_at > ? AND uses < max_uses",
				[]any{now}},
			{&s.Rooms, "SELECT count(*) FROM rooms", nil},
			{&s.PushSubs, "SELECT count(*) FROM push_subscriptions", nil},
			{&s.AuditRows, "SELECT count(*) FROM audit_log", nil},
		}
		for _, c := range counts {
			if err := q.tx.QueryRowContext(q.ctx, c.query, c.args...).Scan(c.dst); err != nil {
				return fmt.Errorf("store: stats: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return Stats{}, err
	}
	return s, nil
}

// fileSize returns the size of path, or 0 if it does not exist.
func fileSize(path string) (int64, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: %w", err)
	}
	return fi.Size(), nil
}

// BackupTo writes a consistent copy of the database to path with VACUUM INTO (for `isshoni admin backup`). It fails
// if path exists. The copy is mode 0600 and synced to disk.
func (db *DB) BackupTo(ctx context.Context, path string) error {
	if err := db.enter(); err != nil {
		return err
	}
	defer db.leave()
	return vacuumInto(ctx, db.writer, path)
}

// BackupFile writes a consistent copy of the database file src to dst with VACUUM INTO, for 04's offline backup. It
// opens src read-only (mode=ro), never migrates it and never writes it; it fails if dst exists. A newer schema works
// too.
//
// After a clean shutdown there is no -wal file, and src is opened immutable as well, so a read-only open can't
// leave new -wal and -shm files behind (owned by whoever ran the CLI). A -wal left by a crash is read, so the copy
// holds every committed transaction.
func BackupFile(ctx context.Context, src, dst string) error {
	if err := isRegularFile(src); err != nil {
		return err
	}
	params := []string{"mode=ro", "_pragma=busy_timeout(" + strconv.Itoa(busyTimeoutMS) + ")"}
	if _, err := os.Lstat(src + "-wal"); errors.Is(err, os.ErrNotExist) {
		params = append(params, "immutable=1")
	}
	conn, err := sql.Open("sqlite", sqliteURI(src, params...))
	if err != nil {
		return fmt.Errorf("store: backup %s: %w", src, err)
	}
	defer func() { _ = conn.Close() }()
	conn.SetMaxOpenConns(1)
	return vacuumInto(ctx, conn, dst)
}

// Settings returns the runtime settings cache (03 §9).
func (db *DB) Settings() *SettingsCache { return db.settings }

// Q is one transaction, handed to the callback of DB.Read or DB.Write. It must not be used after the callback
// returns. Read methods work in both; write methods fail inside Read.
type Q struct {
	ctx      context.Context
	tx       *sql.Tx
	writable bool
}

// exec runs a statement that changes the database; it fails inside Read.
func (q *Q) exec(query string, args ...any) (sql.Result, error) {
	if !q.writable {
		return nil, errReadOnlyTx
	}
	return q.tx.ExecContext(q.ctx, query, args...)
}

// unixMS converts t to the DB's time format: INTEGER unix milliseconds, UTC (03 §3.3).
func unixMS(t time.Time) int64 { return t.UnixMilli() }

// DSNs (03 §4.1). The pragmas are in the DSN, so every pooled connection gets them.

func writerDSN(path string) string {
	return sqliteURI(path,
		"_pragma=busy_timeout("+strconv.Itoa(busyTimeoutMS)+")",
		"_pragma=journal_mode(WAL)",
		"_pragma=synchronous(NORMAL)",
		"_pragma=foreign_keys(1)",
		"_pragma=temp_store(MEMORY)",
		"_pragma=journal_size_limit(67108864)",
		"_txlock=immediate",
	)
}

func readerDSN(path string) string {
	return sqliteURI(path,
		"_pragma=busy_timeout("+strconv.Itoa(busyTimeoutMS)+")",
		"_pragma=foreign_keys(1)",
		"_pragma=query_only(1)",
		"_txlock=deferred",
	)
}

// sqliteURI builds a file: URI. The path is percent-encoded where a URI needs it; SQLite decodes it again.
func sqliteURI(path string, params ...string) string {
	p := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(filepath.ToSlash(path))
	return "file:" + p + "?" + strings.Join(params, "&")
}
