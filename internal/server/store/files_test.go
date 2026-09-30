package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"
)

// dirState maps every file in dir to its SHA-256, to prove that a read-only function changed nothing.
func dirState(t *testing.T, dir string) map[string][32]byte {
	t.Helper()
	out := map[string][32]byte{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path) //nolint:gosec // a file of the test's own temp dir
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		out[rel] = sha256.Sum256(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameState(t *testing.T, what string, before, after map[string][32]byte) {
	t.Helper()
	for name, sum := range after {
		b, ok := before[name]
		switch {
		case !ok:
			t.Errorf("%s created %s", what, name)
		case b != sum:
			t.Errorf("%s changed %s", what, name)
		}
	}
	for name := range before {
		if _, ok := after[name]; !ok {
			t.Errorf("%s deleted %s", what, name)
		}
	}
}

// fixture is a closed database file in its own directory, with a backups dir that InspectFile may only read.
type fixture struct {
	env  *testEnv
	path string
}

// newFixture creates a database at version n (fake migrations beyond the embedded ones) and closes it.
func newFixture(t *testing.T, n int) fixture {
	t.Helper()
	e := newEnv(t)
	if err := e.openClose(fakeMigrations(n)); err != nil {
		t.Fatal(err)
	}
	return fixture{env: e, path: e.dbPath()}
}

func TestInspectFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	latest := LatestSchemaVersion()

	t.Run("current", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, latest)
		missingBackups := filepath.Join(f.env.dir, "no-backups-here")
		before := dirState(t, f.env.dir)
		info, err := InspectFile(ctx, f.path, missingBackups)
		if err != nil {
			t.Fatal(err)
		}
		if info.SchemaVersion != latest || info.LastAppVersion != "0.1.0" || !info.HistoryOK ||
			info.Integrity != nil || info.TooNew != nil {
			t.Errorf("InspectFile = %+v", info)
		}
		sameState(t, "InspectFile", before, dirState(t, f.env.dir))
		if _, err := os.Stat(missingBackups); !errors.Is(err, os.ErrNotExist) {
			t.Error("InspectFile created the backup directory")
		}
	})

	t.Run("newer", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		all := fakeMigrations(latest + 1)
		if err := e.openClose(all[:latest]); err != nil {
			t.Fatal(err)
		}
		e.clock.Advance(time.Second)
		o := e.opts(all)
		o.AppVersion = "9.9.9"
		db, err := Open(ctx, o) // the newer binary migrates and writes pre-<latest>
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		before := dirState(t, e.dir)
		info, err := InspectFile(ctx, e.dbPath(), e.backupDir())
		if err != nil {
			t.Fatal(err)
		}
		wantBackup := filepath.Join(e.backupDir(),
			"pre-"+strconv.Itoa(latest)+"-"+e.clock.Now().UTC().Format("20060102T150405Z")+".db")
		if info.SchemaVersion != latest+1 || info.LastAppVersion != "9.9.9" || !info.HistoryOK || info.Integrity != nil {
			t.Errorf("InspectFile = %+v", info)
		}
		if info.TooNew == nil || info.TooNew.DBVersion != latest+1 || info.TooNew.BinaryVersion != latest ||
			info.TooNew.LastAppVersion != "9.9.9" || info.TooNew.Backup != wantBackup {
			t.Errorf("TooNew = %+v, want backup %s", info.TooNew, wantBackup)
		}
		// The same error Open returns.
		_, openErr := Open(ctx, e.opts(nil))
		if openErr == nil || info.TooNew == nil || openErr.Error() != info.TooNew.Error() {
			t.Errorf("Open: %v\nInspectFile: %v", openErr, info.TooNew)
		}
		sameState(t, "InspectFile", before, dirState(t, e.dir))
	})

	t.Run("renamed migration", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, latest)
		raw := rawDB(t, f.path)
		if _, err := raw.ExecContext(ctx, "UPDATE schema_migrations SET name = '0001_forked' WHERE version = 1"); err != nil {
			t.Fatal(err)
		}
		if _, err := raw.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			t.Fatal(err)
		}
		_ = raw.Close()
		info, err := InspectFile(ctx, f.path, f.env.backupDir())
		if err != nil {
			t.Fatal(err)
		}
		if info.SchemaVersion != latest || info.HistoryOK || info.Integrity != nil || info.TooNew != nil {
			t.Errorf("InspectFile = %+v", info)
		}
	})

	t.Run("corrupted page", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, latest)
		var root int
		raw := rawDB(t, f.path)
		if err := raw.QueryRowContext(ctx, "SELECT rootpage FROM sqlite_master WHERE name = 'audit_at'").Scan(&root); err != nil {
			t.Fatal(err)
		}
		_ = raw.Close()
		corruptPage(t, f.path, root)
		before := dirState(t, f.env.dir)
		info, err := InspectFile(ctx, f.path, f.env.backupDir())
		if err != nil {
			t.Fatal(err)
		}
		if info.Integrity == nil {
			t.Errorf("Integrity = nil for a corrupted page: %+v", info)
		}
		if info.SchemaVersion != latest || !info.HistoryOK {
			t.Errorf("InspectFile = %+v", info)
		}
		sameState(t, "InspectFile", before, dirState(t, f.env.dir))
	})

	t.Run("corrupted schema", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, latest)
		corruptPage(t, f.path, 1)
		info, err := InspectFile(ctx, f.path, f.env.backupDir())
		if err != nil {
			t.Fatal(err)
		}
		if info.Integrity == nil || info.HistoryOK {
			t.Errorf("InspectFile = %+v, want Integrity set and HistoryOK false", info)
		}
	})

	t.Run("not a database", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "x.db")
		if err := os.WriteFile(path, []byte("SQLite format 2\x00 and then some garbage that is long enough"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := InspectFile(ctx, path, dir); err == nil {
			t.Error("InspectFile accepted a file that is not a database")
		}
	})

	t.Run("missing", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "missing.db")
		if _, err := InspectFile(ctx, path, dir); err == nil {
			t.Error("InspectFile of a missing file succeeded")
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("InspectFile created files: %v", entries)
		}
		if _, err := InspectFile(ctx, dir, dir); err == nil {
			t.Error("InspectFile of a directory succeeded")
		}
	})

	t.Run("empty and foreign", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		empty := filepath.Join(dir, "empty.db")
		if err := os.WriteFile(empty, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := InspectFile(ctx, empty, dir)
		if err != nil || info.SchemaVersion != 0 || !info.HistoryOK || info.Integrity != nil || info.TooNew != nil {
			t.Errorf("empty file: %+v, %v", info, err)
		}
		foreign := filepath.Join(dir, "foreign.db")
		raw := rawDB(t, foreign)
		if _, err := raw.ExecContext(ctx, "CREATE TABLE photos (id INTEGER PRIMARY KEY)"); err != nil {
			t.Fatal(err)
		}
		_ = raw.Close()
		info, err = InspectFile(ctx, foreign, dir)
		if err != nil || info.SchemaVersion != 0 || info.HistoryOK {
			t.Errorf("foreign DB: %+v, %v", info, err)
		}
	})
}

func TestBackupFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	latest := LatestSchemaVersion()

	t.Run("copy opens with the same version", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, latest)
		before := dirState(t, f.env.dir)
		dstDir := t.TempDir()
		dst := filepath.Join(dstDir, "copy.db")
		if err := BackupFile(ctx, f.path, dst); err != nil {
			t.Fatal(err)
		}
		sameState(t, "BackupFile", before, dirState(t, f.env.dir))
		fi, err := os.Stat(dst)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("copy mode %o, want 600", fi.Mode().Perm())
		}
		e := &testEnv{t: t, dir: dstDir, clock: newTestClock()}
		o := e.opts(nil)
		o.Path = dst
		db, err := Open(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		if db.SchemaVersion() != latest {
			t.Errorf("copy has version %d", db.SchemaVersion())
		}
		if b := e.backups(); len(b) != 0 {
			t.Errorf("opening the copy migrated it: %v", b)
		}
	})

	t.Run("includes a WAL left by a crash", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		db := e.open(nil)
		mustExecW(t, db, "INSERT INTO meta (key, value) VALUES ('in_wal', 'yes')")
		// The server is still running (as after a crash): the row is only in the WAL.
		dst := filepath.Join(t.TempDir(), "copy.db")
		if err := BackupFile(ctx, e.dbPath(), dst); err != nil {
			t.Fatal(err)
		}
		var v string
		if err := rawDB(t, dst).QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'in_wal'").Scan(&v); err != nil || v != "yes" {
			t.Errorf("the copy misses the WAL content: %q, %v", v, err)
		}
	})

	t.Run("newer schema", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, latest+1)
		dst := filepath.Join(t.TempDir(), "copy.db")
		if err := BackupFile(ctx, f.path, dst); err != nil {
			t.Fatal(err)
		}
		info, err := InspectFile(ctx, dst, "")
		if err != nil || info.SchemaVersion != latest+1 || info.TooNew == nil {
			t.Errorf("copy of a newer DB: %+v, %v", info, err)
		}
	})

	t.Run("refuses an existing destination", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, latest)
		dst := filepath.Join(t.TempDir(), "copy.db")
		if err := os.WriteFile(dst, []byte("keep me"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := BackupFile(ctx, f.path, dst); err == nil {
			t.Error("BackupFile overwrote an existing file")
		}
		if b, _ := os.ReadFile(dst); string(b) != "keep me" { //nolint:gosec // the test's own temp file
			t.Error("BackupFile changed an existing file")
		}
	})

	t.Run("missing source", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		dst := filepath.Join(dir, "copy.db")
		if err := BackupFile(ctx, filepath.Join(dir, "missing.db"), dst); err == nil {
			t.Error("BackupFile of a missing file succeeded")
		}
		entries, _ := os.ReadDir(dir)
		var names []string
		for _, en := range entries {
			names = append(names, en.Name())
		}
		if len(names) != 0 {
			t.Errorf("files created: %v", slices.Clip(names))
		}
	})
}
