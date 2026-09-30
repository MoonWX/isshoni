package store

import (
	"cmp"
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"time"

	sqlite3 "modernc.org/sqlite/lib"
)

// Migrations are forward-only (03 §4.3). Each file is migrations/NNNN_snake_name.sql; the versions start at 1 and
// are contiguous (a test checks this). A released migration never changes: a schema change is a new file.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// goSteps holds the Go data rewrites that SQL can't express, by version. A step runs after that version's SQL, in
// the same transaction.
var goSteps = map[int]func(ctx context.Context, tx *sql.Tx) error{}

// migration is one schema version.
type migration struct {
	version int
	name    string // file name without extension, e.g. "0001_init"
	sql     string
	goStep  func(ctx context.Context, tx *sql.Tx) error
}

var migrationName = regexp.MustCompile(`^([0-9]{4})_[a-z0-9]+(?:_[a-z0-9]+)*$`)

// embeddedMigrations returns the embedded migrations, parsed once. Callers must not modify the elements; the slice
// is clipped, so an append copies it. It panics on a malformed set: that is a build defect, and the unit tests catch
// it before any release.
var embeddedMigrations = sync.OnceValue(func() []migration {
	ms, err := parseMigrations(migrationFiles, "migrations")
	if err != nil {
		panic(err)
	}
	return slices.Clip(ms)
})

func parseMigrations(fsys fs.FS, dir string) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("store: read migrations: %w", err)
	}
	var ms []migration
	for _, e := range entries {
		ext := path.Ext(e.Name())
		if e.IsDir() || ext != ".sql" {
			return nil, fmt.Errorf("store: unexpected file in migrations: %s", e.Name())
		}
		name := e.Name()[:len(e.Name())-len(ext)]
		m := migrationName.FindStringSubmatch(name)
		if m == nil {
			return nil, fmt.Errorf("store: migration file %s is not named NNNN_snake_name.sql", e.Name())
		}
		v, _ := strconv.Atoi(m[1])
		body, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("store: read migration %s: %w", e.Name(), err)
		}
		ms = append(ms, migration{version: v, name: name, sql: string(body), goStep: goSteps[v]})
	}
	slices.SortFunc(ms, func(a, b migration) int { return cmp.Compare(a.version, b.version) })
	for i, m := range ms {
		if m.version != i+1 {
			return nil, fmt.Errorf("store: migration versions are not contiguous from 1: found %s at position %d",
				m.name, i+1)
		}
	}
	return ms, nil
}

// LatestSchemaVersion is the highest schema version this binary knows. It opens nothing.
func LatestSchemaVersion() int { return len(embeddedMigrations()) }

// migrator runs steps 3–6 of Open on the writer.
type migrator struct {
	db   *DB
	migs []migration
}

// appliedRow is one row of schema_migrations.
type appliedRow struct {
	version int
	name    string
}

const createBookkeeping = `CREATE TABLE IF NOT EXISTS schema_migrations (
  version     INTEGER PRIMARY KEY,
  name        TEXT    NOT NULL,
  applied_at  INTEGER NOT NULL,
  app_version TEXT    NOT NULL
) STRICT`

func (m *migrator) run(ctx context.Context) error {
	db, o := m.db, m.db.opts
	// 3. Bookkeeping.
	if _, err := db.writer.ExecContext(ctx, createBookkeeping); err != nil {
		return openErr(ctx, o.Path, "store: create schema_migrations", err)
	}
	// 4. The current version and its history.
	applied, err := readApplied(ctx, db.writer)
	if err != nil {
		return openErr(ctx, o.Path, "store: read schema_migrations", err)
	}
	if err := checkHistory(applied, m.migs); err != nil {
		return err
	}
	cur, latest := len(applied), len(m.migs)
	// 5. Refuse a newer schema.
	if cur > latest {
		return tooNew(ctx, db.writer, cur, latest, o.BackupDir)
	}
	if cur == latest {
		return nil
	}
	// 6. Back up an existing database, then migrate.
	backup := ""
	if cur >= 1 {
		if backup, err = m.backup(ctx, cur); err != nil {
			return err
		}
	}
	for _, mig := range m.migs[cur:] {
		if err := m.apply(ctx, mig); err != nil {
			return err
		}
	}
	if cur == 0 {
		db.log.Info("database created", "schema_version", latest)
	} else {
		db.log.Info("database migrated", "from_version", cur, "to_version", latest, "backup", backup)
	}
	return nil
}

func readApplied(ctx context.Context, q queryer) ([]appliedRow, error) {
	rows, err := q.QueryContext(ctx, "SELECT version, name FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []appliedRow
	for rows.Next() {
		var r appliedRow
		if err := rows.Scan(&r.version, &r.name); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// checkHistory compares schema_migrations with the migrations this binary has. Versions beyond the binary's are not
// compared (that is the newer-schema case). A gap in the recorded versions is a mismatch too.
func checkHistory(applied []appliedRow, migs []migration) error {
	for i, a := range applied {
		want := i + 1
		if a.version != want {
			return historyMismatch(want, "", binaryName(migs, want))
		}
		if want <= len(migs) && a.name != migs[want-1].name {
			return historyMismatch(want, a.name, migs[want-1].name)
		}
	}
	return nil
}

func binaryName(migs []migration, v int) string {
	if v >= 1 && v <= len(migs) {
		return migs[v-1].name
	}
	return ""
}

func historyMismatch(v int, dbName, binName string) error {
	return needsOperator(fmt.Sprintf("store: schema history mismatch at version %d (DB has %q, binary has %q)",
		v, dbName, binName), nil)
}

// tooNew builds the *SchemaTooNewError for a file at version cur. An unreadable last_app_version only makes the
// message less specific; the refusal stands.
func tooNew(ctx context.Context, q queryer, cur, latest int, backupDir string) error {
	last, err := readLastAppVersion(ctx, q)
	if err != nil {
		last = ""
	}
	e := &SchemaTooNewError{DBVersion: cur, BinaryVersion: latest, LastAppVersion: last, backupDir: backupDir}
	if b := newestBackup(backupDir, latest); b != "" {
		e.Backup = filepath.Join(backupDir, b)
	}
	return e
}

// readLastAppVersion reads meta.last_app_version, or "" when there is no meta table or no such key.
func readLastAppVersion(ctx context.Context, q queryer) (string, error) {
	var tables int
	err := q.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'meta'").
		Scan(&tables)
	if err != nil || tables == 0 {
		return "", err
	}
	var v string
	err = q.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'last_app_version'").Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// apply runs one migration with SQLite's table-rebuild procedure: foreign keys off (outside the transaction),
// BEGIN IMMEDIATE, the SQL and the Go step, PRAGMA foreign_key_check, the bookkeeping row and user_version, COMMIT,
// foreign keys on.
func (m *migrator) apply(ctx context.Context, mig migration) (err error) {
	db, path := m.db, m.db.opts.Path
	conn, err := db.writer.Conn(ctx)
	if err != nil {
		return openErr(ctx, path, "store: migration "+mig.name, err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return openErr(ctx, path, "store: migration "+mig.name, err)
	}
	defer func() {
		// Always re-enable foreign keys on the pooled writer connection, even after a failure or a cancelled ctx.
		if _, fkErr := conn.ExecContext(context.WithoutCancel(ctx), "PRAGMA foreign_keys=ON"); fkErr != nil && err == nil {
			err = fmt.Errorf("store: migration %s: re-enable foreign keys: %w", mig.name, fkErr)
		}
	}()

	tx, err := conn.BeginTx(ctx, nil) // BEGIN IMMEDIATE (the writer DSN's _txlock)
	if err != nil {
		return openErr(ctx, path, "store: migration "+mig.name+": begin", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// A failure of the step itself is an operator problem: the binary can't reach its schema. A cancelled start
	// is not.
	stepErr := func(what string, err error) error {
		if ctx.Err() != nil {
			return openErr(ctx, path, "store: migration "+mig.name+": "+what, err)
		}
		return needsOperator("store: migration "+mig.name+" failed: "+what, err)
	}
	if _, err := tx.ExecContext(ctx, mig.sql); err != nil {
		return stepErr("SQL", err)
	}
	if mig.goStep != nil {
		if err := mig.goStep(ctx, tx); err != nil {
			return stepErr("Go step", err)
		}
	}
	if err := foreignKeyCheck(ctx, tx); err != nil {
		return stepErr("foreign key check", err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, name, applied_at, app_version) VALUES (?, ?, ?, ?)",
		mig.version, mig.name, unixMS(db.now()), db.opts.AppVersion); err != nil {
		return openErr(ctx, path, "store: migration "+mig.name+": record", err)
	}
	// PRAGMA takes no parameters; the version is an int.
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(mig.version)); err != nil {
		return openErr(ctx, path, "store: migration "+mig.name+": user_version", err)
	}
	if err := tx.Commit(); err != nil {
		return openErr(ctx, path, "store: migration "+mig.name+": commit", err)
	}
	committed = true
	return nil
}

// foreignKeyCheck fails on the first row PRAGMA foreign_key_check reports.
func foreignKeyCheck(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		var (
			table, parent string
			rowid         sql.NullInt64
			fkid          int
		)
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return err
		}
		return fmt.Errorf("row %d of table %s violates its foreign key %d to %s", rowid.Int64, table, fkid, parent)
	}
	return rows.Err()
}

// ---- pre-migration backups (03 §4.4) ----

// keepBackups is how many of the newest pre-migration backups rotation keeps (plus the newest one per version).
const keepBackups = 5

// maxBackupNameTries bounds the search for a free backup name (one second later per try).
const maxBackupNameTries = 60

// backupTimeFormat is the UTC timestamp in backup names, e.g. 20261014T021500Z.
const backupTimeFormat = "20060102T150405Z"

var backupName = regexp.MustCompile(`^pre-([0-9]+)-([0-9]{8}T[0-9]{6}Z)\.db$`)

// backupFile is one parsed pre-migration backup name.
type backupFile struct {
	name    string
	version int
	at      time.Time
}

// backup writes backups/pre-<cur>-<ts>.db after the free-space check, then rotates. It returns the file name.
func (m *migrator) backup(ctx context.Context, cur int) (string, error) {
	db, o := m.db, m.db.opts
	dbBytes, err := fileSize(o.Path)
	if err != nil {
		return "", err
	}
	walBytes, err := fileSize(o.Path + "-wal")
	if err != nil {
		return "", err
	}
	need := 2 * uint64(dbBytes+walBytes) //nolint:gosec // file sizes are never negative
	free, ok, err := o.freeSpace(o.BackupDir)
	if err != nil {
		return "", fmt.Errorf("store: free space of %s: %w", o.BackupDir, err)
	}
	if ok && free < need {
		const mb = 1 << 20
		return "", needsOperator(fmt.Sprintf(
			"store: not enough free disk space for the pre-migration backup (need %d MB, have %d MB)",
			(need+mb-1)/mb, free/mb), nil)
	}
	// Two starts within one second would get the same name. The later one moves its timestamp forward instead,
	// which keeps the names unique and in order.
	at := db.now().UTC()
	name := ""
	for try := range maxBackupNameTries {
		name = fmt.Sprintf("pre-%d-%s.db", cur, at.Add(time.Duration(try)*time.Second).Format(backupTimeFormat))
		err = vacuumInto(ctx, db.writer, filepath.Join(o.BackupDir, name))
		if !errors.Is(err, os.ErrExist) {
			break
		}
	}
	if err != nil {
		return "", openErr(ctx, o.Path, "store: pre-migration backup", err)
	}
	if err := rotateBackups(o.BackupDir); err != nil {
		// The backup exists; a file we failed to delete is not a reason to stop.
		db.log.Warn("could not rotate pre-migration backups", "err", err)
	}
	return name, nil
}

// listBackups returns the pre-migration backups in dir, newest first (by the timestamp in the name, then by name).
// A missing directory has none. Other files are ignored.
func listBackups(dir string) ([]backupFile, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: list backups: %w", err)
	}
	var out []backupFile
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		m := backupName.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		at, err := time.Parse(backupTimeFormat, m[2])
		if err != nil {
			continue
		}
		out = append(out, backupFile{name: e.Name(), version: v, at: at})
	}
	slices.SortFunc(out, func(a, b backupFile) int {
		if c := b.at.Compare(a.at); c != 0 {
			return c
		}
		return cmp.Compare(b.name, a.name)
	})
	return out, nil
}

// newestBackup returns the name of the newest pre-<version>-*.db in dir, or "".
func newestBackup(dir string, version int) string {
	bs, err := listBackups(dir)
	if err != nil {
		return ""
	}
	for _, b := range bs {
		if b.version == version {
			return b.name
		}
	}
	return ""
}

// rotateBackups keeps the newest 5 pre-migration backups plus the newest one of each version, and deletes the rest.
// The per-version rule keeps the file an older binary needs through a restart loop (03 §4.4).
func rotateBackups(dir string) error {
	bs, err := listBackups(dir)
	if err != nil {
		return err
	}
	seen := map[int]bool{}
	var errs []error
	for i, b := range bs {
		newestOfVersion := !seen[b.version]
		seen[b.version] = true
		if i < keepBackups || newestOfVersion {
			continue
		}
		if err := os.Remove(filepath.Join(dir, b.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// execer is what vacuumInto needs from *sql.DB and *sql.Conn.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// vacuumInto writes a consistent copy of the database behind e to dst. It fails if dst exists: the file is created
// first (0600, O_EXCL), and VACUUM INTO accepts an empty target. The copy is synced, with its directory.
func vacuumInto(ctx context.Context, e execer, dst string) error {
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // dst is chosen by the caller
	if err != nil {
		return fmt.Errorf("store: create backup file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("store: create backup file: %w", err)
	}
	if _, err := e.ExecContext(ctx, "VACUUM INTO ?", dst); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("store: VACUUM INTO %s: %w", dst, err)
	}
	if err := syncFile(dst); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dst))
}

func syncFile(name string) error {
	f, err := os.OpenFile(name, os.O_RDWR, 0) //nolint:gosec // our own backup file
	if err != nil {
		return fmt.Errorf("store: sync %s: %w", name, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("store: sync %s: %w", name, err)
	}
	return f.Close()
}

// syncDir makes a new directory entry durable. Some platforms can't sync a directory; that is not an error.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // our own directory
	if err != nil {
		return fmt.Errorf("store: sync %s: %w", dir, err)
	}
	_ = d.Sync()
	return d.Close()
}

// ensureDir creates dir with mode 0700 and tightens an existing one.
func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("store: create %s: %w", dir, err)
	}
	return tighten(dir, 0o700)
}

// ensureDBFile creates the database file with mode 0600 if it is missing (SQLite then gives -wal and -shm the same
// mode) and tightens an existing one.
func ensureDBFile(name string) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // path from config
	switch {
	case err == nil:
		return f.Close()
	case errors.Is(err, os.ErrExist):
		return tighten(name, 0o600)
	default:
		return fmt.Errorf("store: create %s: %w", name, err)
	}
}

// tighten removes permission bits beyond perm.
func tighten(name string, perm os.FileMode) error {
	fi, err := os.Stat(name)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if fi.Mode().Perm()&^perm == 0 {
		return nil
	}
	if err := os.Chmod(name, fi.Mode().Perm()&perm); err != nil {
		return fmt.Errorf("store: tighten the mode of %s: %w", name, err)
	}
	return nil
}

// ---- file-level functions (03 §4.4): never migrate, never create backups/, never write meta ----

// FileInfo describes a database file without opening it for writing.
type FileInfo struct {
	SchemaVersion  int
	LastAppVersion string             // meta.last_app_version
	HistoryOK      bool               // schema_migrations names match the embedded files
	Integrity      error              // PRAGMA integrity_check; nil = ok
	TooNew         *SchemaTooNewError // non-nil when SchemaVersion > LatestSchemaVersion()
}

// InspectFile reports the schema version, history, integrity and newer-schema status of a database file, for 04's
// restore validation and offline doctor. It opens dbPath read-only and immutable (mode=ro&immutable=1), so it never
// creates -wal or -shm files and never changes the file; with a leftover WAL the reported state may be one step old.
// backupDir is only read, to fill TooNew.Backup. It returns an error only when the file can't be read as a database
// at all; corruption found while reading is reported in Integrity.
func InspectFile(ctx context.Context, dbPath, backupDir string) (FileInfo, error) {
	return inspectFile(ctx, dbPath, backupDir, embeddedMigrations())
}

func inspectFile(ctx context.Context, dbPath, backupDir string, migs []migration) (FileInfo, error) {
	if err := isRegularFile(dbPath); err != nil {
		return FileInfo{}, err
	}
	conn, err := sql.Open("sqlite", sqliteURI(dbPath, "mode=ro", "immutable=1", "_pragma=query_only(1)"))
	if err != nil {
		return FileInfo{}, fmt.Errorf("store: inspect %s: %w", dbPath, err)
	}
	defer func() { _ = conn.Close() }()
	conn.SetMaxOpenConns(1)

	var info FileInfo
	var tables int
	err = conn.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'").Scan(&tables)
	if err != nil {
		if ctx.Err() == nil && sqliteCode(err) == sqlite3.SQLITE_CORRUPT { // readable header, damaged schema
			info.Integrity = fmt.Errorf("store: %w", err)
			return info, nil
		}
		return FileInfo{}, fmt.Errorf("store: inspect %s: %w", dbPath, err)
	}
	if tables == 0 {
		// Not an isshoni database, or an empty file: version 0, and the history matches only when it has no tables.
		var n int
		if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master").Scan(&n); err != nil {
			return FileInfo{}, fmt.Errorf("store: inspect %s: %w", dbPath, err)
		}
		info.HistoryOK = n == 0
	} else {
		applied, err := readApplied(ctx, conn)
		if err != nil {
			if ctx.Err() != nil || !isCorrupt(err) {
				return FileInfo{}, fmt.Errorf("store: inspect %s: %w", dbPath, err)
			}
			info.Integrity = fmt.Errorf("store: %w", err)
			return info, nil
		}
		if n := len(applied); n > 0 {
			info.SchemaVersion = applied[n-1].version
		}
		info.HistoryOK = checkHistory(applied, migs) == nil
		if info.LastAppVersion, err = readLastAppVersion(ctx, conn); err != nil {
			if ctx.Err() != nil || !isCorrupt(err) {
				return FileInfo{}, fmt.Errorf("store: inspect %s: %w", dbPath, err)
			}
			info.Integrity = fmt.Errorf("store: %w", err)
		}
	}
	if info.Integrity == nil {
		problems, err := pragmaCheck(ctx, conn, "PRAGMA integrity_check")
		switch {
		case err != nil && (ctx.Err() != nil || !isCorrupt(err)):
			return FileInfo{}, fmt.Errorf("store: inspect %s: integrity_check: %w", dbPath, err)
		case err != nil:
			info.Integrity = fmt.Errorf("store: integrity_check: %w", err)
		case problems != "":
			info.Integrity = fmt.Errorf("store: integrity_check: %s", problems)
		}
	}
	if latest := len(migs); info.SchemaVersion > latest {
		info.TooNew = &SchemaTooNewError{
			DBVersion:      info.SchemaVersion,
			BinaryVersion:  latest,
			LastAppVersion: info.LastAppVersion,
			backupDir:      backupDir,
		}
		if b := newestBackup(backupDir, latest); b != "" {
			info.TooNew.Backup = filepath.Join(backupDir, b)
		}
	}
	return info, nil
}

// isRegularFile fails unless name exists and is a regular file. It keeps the read-only opens from creating files.
func isRegularFile(name string) error {
	fi, err := os.Stat(name)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("store: %s is not a regular file", name)
	}
	return nil
}
