package store

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOpenNewDatabase(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)

	latest := LatestSchemaVersion()
	if latest < 1 {
		t.Fatalf("LatestSchemaVersion() = %d", latest)
	}
	if got := db.SchemaVersion(); got != latest {
		t.Errorf("SchemaVersion() = %d, want %d", got, latest)
	}
	recorded, userVersion := fileVersions(t, e.dbPath())
	if recorded != latest || userVersion != latest {
		t.Errorf("schema_migrations max = %d, user_version = %d, want %d", recorded, userVersion, latest)
	}
	if n := queryInt(t, db, "SELECT count(*) FROM schema_migrations WHERE app_version = '0.1.0' AND applied_at = ?",
		e.clock.Now().UnixMilli()); n != latest {
		t.Errorf("schema_migrations rows with app_version and applied_at = %d, want %d", n, latest)
	}

	// A new DB contains Lounge, the only (default) room.
	var room Room
	var isDefault int
	var createdBy *string
	var createdAt, updatedAt int64
	err := db.Read(context.Background(), func(q *Q) error {
		return q.tx.QueryRowContext(q.ctx,
			"SELECT id, name, name_key, is_default, created_by, created_at, updated_at FROM rooms").
			Scan(&room.ID, &room.Name, &room.NameKey, &isDefault, &createdBy, &createdAt, &updatedAt)
	})
	if err != nil {
		t.Fatal(err)
	}
	if room.ID != DefaultRoomID || room.Name != "Lounge" || room.NameKey != "lounge" || isDefault != 1 || createdBy != nil {
		t.Errorf("default room = %+v is_default=%d created_by=%v", room, isDefault, createdBy)
	}
	if want := e.clock.Now().UnixMilli(); createdAt != want || updatedAt != want {
		t.Errorf("default room times = %d, %d, want %d", createdAt, updatedAt, want)
	}
	if n := queryInt(t, db, "SELECT count(*) FROM rooms"); n != 1 {
		t.Errorf("rooms = %d, want 1", n)
	}

	// Meta keys.
	err = db.Read(context.Background(), func(q *Q) error {
		created, err := q.GetMeta("created_at")
		if err != nil {
			return err
		}
		if want := "1791944100000"; created != want {
			t.Errorf("meta created_at = %q, want %q", created, want)
		}
		v, err := q.GetMeta("last_app_version")
		if err != nil {
			return err
		}
		if v != "0.1.0" {
			t.Errorf("meta last_app_version = %q", v)
		}
		if _, err := q.GetMeta("nope"); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetMeta(missing) = %v, want ErrNotFound", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A new DB needs no backup.
	if b := e.backups(); len(b) != 0 {
		t.Errorf("backups after creating a new DB: %v", b)
	}
	// WAL mode.
	var mode string
	if err := db.writer.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q, %v", mode, err)
	}
}

func TestOpenFileModes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	mustExecW(t, db, "INSERT INTO meta (key, value) VALUES ('x', 'y')") // make sure -wal and -shm exist
	check := func(name string, want os.FileMode) {
		t.Helper()
		fi, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s: mode %o, want %o", filepath.Base(name), got, want)
		}
	}
	check(e.dbPath(), 0o600)
	check(e.dbPath()+"-wal", 0o600)
	check(e.dbPath()+"-shm", 0o600)
	check(e.backupDir(), 0o700)
}

func TestOpenTightensModes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.openClose(nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(e.dbPath(), 0o644); err != nil { //nolint:gosec // widened on purpose
		t.Fatal(err)
	}
	if err := os.Chmod(e.backupDir(), 0o755); err != nil { //nolint:gosec // widened on purpose
		t.Fatal(err)
	}
	if err := e.openClose(nil); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{e.dbPath(): 0o600, e.backupDir(): 0o700} {
		fi, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s: mode %o, want %o", filepath.Base(name), got, want)
		}
	}
}

func TestReopenIsNoop(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.openClose(nil); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Hour)
	o := e.opts(nil)
	o.AppVersion = "0.2.0"
	db, err := Open(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if n := queryInt(t, db, "SELECT count(*) FROM schema_migrations"); n != LatestSchemaVersion() {
		t.Errorf("schema_migrations rows = %d after reopening", n)
	}
	if n := queryInt(t, db, "SELECT count(*) FROM schema_migrations WHERE app_version = '0.1.0'"); n != LatestSchemaVersion() {
		t.Errorf("reopening rewrote schema_migrations (%d rows by 0.1.0)", n)
	}
	if n := queryInt(t, db, "SELECT count(*) FROM rooms"); n != 1 {
		t.Errorf("rooms = %d after reopening, want 1", n)
	}
	err = db.Read(context.Background(), func(q *Q) error {
		created, _ := q.GetMeta("created_at")
		last, _ := q.GetMeta("last_app_version")
		if created != "1791944100000" || last != "0.2.0" {
			t.Errorf("meta after reopening: created_at=%q last_app_version=%q", created, last)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if b := e.backups(); len(b) != 0 {
		t.Errorf("reopening wrote backups: %v", b)
	}
}

func TestEnsureDefaultRoomKeepsRenamedLounge(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	mustExecW(t, db, "UPDATE rooms SET name = 'Movie night', name_key = 'movie night' WHERE id = 'lounge'")
	err := db.Write(context.Background(), func(q *Q) error { return q.EnsureDefaultRoom(e.clock.Now()) })
	if err != nil {
		t.Fatal(err)
	}
	if n := queryInt(t, db, "SELECT count(*) FROM rooms WHERE id = 'lounge' AND name = 'Movie night'"); n != 1 {
		t.Errorf("EnsureDefaultRoom changed or duplicated a renamed Lounge")
	}
	if n := queryInt(t, db, "SELECT count(*) FROM rooms"); n != 1 {
		t.Errorf("rooms = %d, want 1", n)
	}
}

func TestReadRejectsWrites(t *testing.T) {
	t.Parallel()
	db := newEnv(t).open(nil)
	ctx := context.Background()
	err := db.Read(ctx, func(q *Q) error { return q.SetMeta("k", "v") })
	if !errors.Is(err, errReadOnlyTx) {
		t.Errorf("SetMeta inside Read = %v, want errReadOnlyTx", err)
	}
	err = db.Read(ctx, func(q *Q) error { return q.EnsureDefaultRoom(time.Now()) })
	if !errors.Is(err, errReadOnlyTx) {
		t.Errorf("EnsureDefaultRoom inside Read = %v, want errReadOnlyTx", err)
	}
	// The reader connections are query_only too, so even raw SQL can't write.
	err = db.Read(ctx, func(q *Q) error {
		_, err := q.tx.ExecContext(q.ctx, "INSERT INTO meta (key, value) VALUES ('k', 'v')")
		return err
	})
	if err == nil {
		t.Error("raw INSERT inside Read succeeded")
	}
	if n := queryInt(t, db, "SELECT count(*) FROM meta WHERE key = 'k'"); n != 0 {
		t.Errorf("a write inside Read changed the DB")
	}
}

func TestWriteCommitsAndRollsBack(t *testing.T) {
	t.Parallel()
	db := newEnv(t).open(nil)
	ctx := context.Background()
	if err := db.Write(ctx, func(q *Q) error { return q.SetMeta("a", "1") }); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	err := db.Write(ctx, func(q *Q) error {
		if err := q.SetMeta("b", "2"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Write = %v, want boom", err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic was swallowed")
			}
		}()
		_ = db.Write(ctx, func(q *Q) error {
			_ = q.SetMeta("c", "3")
			panic("boom")
		})
	}()
	// The writer is usable after the panic, and only "a" was committed.
	if err := db.Write(ctx, func(q *Q) error { return q.SetMeta("d", "4") }); err != nil {
		t.Fatalf("Write after a panic: %v", err)
	}
	if n := queryInt(t, db, "SELECT count(*) FROM meta WHERE key IN ('a', 'b', 'c', 'd')"); n != 2 {
		t.Errorf("committed keys = %d, want 2 (a, d)", n)
	}
}

func TestWritesQueue(t *testing.T) {
	t.Parallel()
	db := newEnv(t).open(nil)
	ctx := context.Background()
	mustExecW(t, db, "INSERT INTO meta (key, value) VALUES ('n', '0')")
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			err := db.Write(ctx, func(q *Q) error {
				v, err := q.GetMeta("n")
				if err != nil {
					return err
				}
				var n int
				for _, c := range v {
					n = n*10 + int(c-'0')
				}
				return q.SetMeta("n", itoa(n+1))
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := queryInt(t, db, "SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'n'"); n != 20 {
		t.Errorf("counter = %d after 20 concurrent writes, want 20", n)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// captureHandler records log records.
type captureHandler struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r.Clone())
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) find(msg string) (slog.Record, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.recs {
		if r.Message == msg {
			return r, true
		}
	}
	return slog.Record{}, false
}

func TestSlowWriteLogsCaller(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	h := &captureHandler{}
	o := e.opts(nil)
	o.Logger = slog.New(h)
	db, err := Open(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, ok := h.find("slow write transaction"); ok {
		t.Fatal("slow-write warning before any slow write")
	}
	err = db.Write(context.Background(), func(q *Q) error {
		time.Sleep(slowWrite + 20*time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	r, ok := h.find("slow write transaction")
	if !ok {
		t.Fatal("no slow-write warning")
	}
	if r.Level != slog.LevelWarn {
		t.Errorf("level = %v", r.Level)
	}
	var caller string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "caller" {
			caller = a.Value.String()
		}
		return true
	})
	if !strings.HasSuffix(caller, "store.TestSlowWriteLogsCaller") {
		t.Errorf("caller = %q, want the test function", caller)
	}
}

func TestCloseIsIdempotentAndRejectsCalls(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db, err := Open(context.Background(), e.opts(nil))
	if err != nil {
		t.Fatal(err)
	}
	mustExecW(t, db, "INSERT INTO meta (key, value) VALUES ('x', 'y')")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	ctx := context.Background()
	calls := map[string]error{
		"Read":       db.Read(ctx, func(*Q) error { return nil }),
		"Write":      db.Write(ctx, func(*Q) error { return nil }),
		"Ping":       db.Ping(ctx),
		"QuickCheck": db.QuickCheck(ctx),
		"BackupTo":   db.BackupTo(ctx, filepath.Join(e.dir, "x.db")),
	}
	for name, err := range calls {
		if !errors.Is(err, errClosed) {
			t.Errorf("%s after Close = %v, want errClosed", name, err)
		}
	}
	// The checkpoint truncated the WAL: the .db file alone is complete.
	if size, _ := fileSize(e.dbPath() + "-wal"); size != 0 {
		t.Errorf("-wal is %d bytes after Close", size)
	}
	recorded, _ := fileVersions(t, e.dbPath())
	if recorded != LatestSchemaVersion() {
		t.Errorf("the .db file alone has version %d", recorded)
	}
}

func TestCloseWaitsForRunningTransactions(t *testing.T) {
	t.Parallel()
	db := newEnv(t).open(nil)
	started := make(chan struct{})
	release := make(chan struct{})
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- db.Write(context.Background(), func(q *Q) error {
			close(started)
			<-release
			return q.SetMeta("late", "1")
		})
	}()
	<-started
	closeDone := make(chan error, 1)
	go func() { closeDone <- db.Close() }()
	select {
	case <-closeDone:
		t.Fatal("Close returned while a Write was running")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-writeDone; err != nil {
		t.Errorf("the running Write failed: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestPingQuickCheckStats(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	ctx := context.Background()
	if err := db.Ping(ctx); err != nil {
		t.Errorf("Ping: %v", err)
	}
	if err := db.QuickCheck(ctx); err != nil {
		t.Errorf("QuickCheck: %v", err)
	}
	now := e.clock.Now().UnixMilli()
	for _, s := range []string{
		`INSERT INTO users (id, username, username_key, role, status, created_via, created_at, updated_at)
		 VALUES ('u1', 'Alex', 'alex', 'admin', 'active', 'setup', 0, 0),
		        ('u2', 'Sam', 'sam', 'user', 'pending', 'signup', 0, 0),
		        ('u3', 'Kim', 'kim', 'user', 'disabled', 'invite', 0, 0)`,
		`INSERT INTO sessions (id, user_id, token_hash, name, created_at, rotated_at, last_seen_at, last_ip,
		   idle_expires_at, expires_at) VALUES ('s1', 'u1', x'01', 'Chrome on Windows', 0, 0, 0, '192.0.2.1', 1, 1)`,
		`INSERT INTO invites (id, token_hash, created_at, expires_at, max_uses, uses)
		 VALUES ('i1', x'01', 0, ` + itoa(int(now)+1) + `, 10, 0),
		        ('i2', x'02', 0, ` + itoa(int(now)) + `, 10, 0),
		        ('i3', x'03', 0, ` + itoa(int(now)+1) + `, 1, 1)`,
		`INSERT INTO audit_log (at, action, actor_kind) VALUES (0, 'setup.token_issued', 'cli')`,
	} {
		mustExecW(t, db, s)
	}
	st, err := db.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.SchemaVersion != LatestSchemaVersion() || st.Users != 2 || st.Pending != 1 || st.Sessions != 1 ||
		st.Devices != 0 || st.ActiveInvites != 1 || st.Rooms != 1 || st.PushSubs != 0 || st.AuditRows != 1 {
		t.Errorf("Stats = %+v", st)
	}
	if st.DBBytes <= 0 || st.WALBytes <= 0 {
		t.Errorf("Stats sizes: db %d, wal %d", st.DBBytes, st.WALBytes)
	}
	if len(st.Backups) != 0 {
		t.Errorf("Stats.Backups = %v", st.Backups)
	}
}

func TestBackupTo(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	ctx := context.Background()
	mustExecW(t, db, "INSERT INTO meta (key, value) VALUES ('marker', 'in the backup')")
	dst := filepath.Join(e.dir, "copy.db")
	if err := db.BackupTo(ctx, dst); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("backup mode %o, want 600", fi.Mode().Perm())
	}
	info, err := InspectFile(ctx, dst, "")
	if err != nil {
		t.Fatal(err)
	}
	if info.SchemaVersion != LatestSchemaVersion() || !info.HistoryOK || info.Integrity != nil || info.LastAppVersion != "0.1.0" {
		t.Errorf("backup: %+v", info)
	}
	raw := rawDB(t, dst)
	var v string
	if err := raw.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'marker'").Scan(&v); err != nil || v != "in the backup" {
		t.Errorf("marker in the backup = %q, %v", v, err)
	}

	// An existing path is never overwritten, even an empty file.
	before, _ := os.ReadFile(dst) //nolint:gosec // the test's own temp file
	if err := db.BackupTo(ctx, dst); err == nil {
		t.Error("BackupTo over an existing file succeeded")
	}
	after, _ := os.ReadFile(dst) //nolint:gosec // the test's own temp file
	if !bytes.Equal(before, after) {
		t.Error("BackupTo changed an existing file")
	}
	empty := filepath.Join(e.dir, "empty.db")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := db.BackupTo(ctx, empty); err == nil {
		t.Error("BackupTo over an existing empty file succeeded")
	}
}

func TestOpenValidatesOptions(t *testing.T) {
	t.Parallel()
	if _, err := Open(context.Background(), Options{}); err == nil {
		t.Error("Open without a path succeeded")
	}
	// BackupDir defaults to "backups" next to the database.
	dir := t.TempDir()
	db, err := Open(context.Background(), Options{Path: filepath.Join(dir, "x.db"), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "backups")); err != nil || !fi.IsDir() {
		t.Errorf("default backup dir: %v", err)
	}
}

func TestSQLiteURIEscapesPath(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "odd ?#% name")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "isshoni.db")
	db, err := Open(context.Background(), Options{Path: path, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the database is not at the given path: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(dir))
	if len(entries) != 1 {
		t.Errorf("files created outside the directory: %v", entries)
	}
}
