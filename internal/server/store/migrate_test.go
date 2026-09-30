package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestEmbeddedMigrations(t *testing.T) {
	t.Parallel()
	ms := embeddedMigrations()
	if len(ms) == 0 {
		t.Fatal("no embedded migrations")
	}
	if ms[0].name != "0001_init" {
		t.Errorf("first migration = %q, want 0001_init", ms[0].name)
	}
	names := map[string]bool{}
	for i, m := range ms {
		if m.version != i+1 {
			t.Errorf("migration %d has version %d: not contiguous from 1", i, m.version)
		}
		if !strings.HasPrefix(m.name, fmt.Sprintf("%04d_", m.version)) {
			t.Errorf("migration %s: name does not start with its version", m.name)
		}
		if names[m.name] {
			t.Errorf("duplicate migration name %s", m.name)
		}
		names[m.name] = true
		if strings.TrimSpace(m.sql) == "" {
			t.Errorf("migration %s is empty", m.name)
		}
	}
	for v := range goSteps {
		if v < 1 || v > len(ms) {
			t.Errorf("goSteps has version %d, which has no migration file", v)
		}
	}
	if LatestSchemaVersion() != len(ms) {
		t.Errorf("LatestSchemaVersion() = %d, want %d", LatestSchemaVersion(), len(ms))
	}
}

func TestParseMigrationsRejectsBadSets(t *testing.T) {
	t.Parallel()
	file := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	cases := map[string]fstest.MapFS{
		"gap":          {"m/0001_a.sql": file("x"), "m/0003_c.sql": file("x")},
		"not from 1":   {"m/0002_b.sql": file("x")},
		"duplicate":    {"m/0001_a.sql": file("x"), "m/0001_b.sql": file("x")},
		"bad name":     {"m/0001-init.sql": file("x")},
		"upper case":   {"m/0001_Init.sql": file("x")},
		"other file":   {"m/0001_a.sql": file("x"), "m/README.md": file("x")},
		"short number": {"m/001_a.sql": file("x")},
	}
	for name, fsys := range cases {
		if _, err := parseMigrations(fsys, "m"); err == nil {
			t.Errorf("%s: parseMigrations accepted the set", name)
		}
	}
	ms, err := parseMigrations(fstest.MapFS{"m/0002_b_c.sql": file("2"), "m/0001_a.sql": file("1")}, "m")
	if err != nil || len(ms) != 2 || ms[0].name != "0001_a" || ms[1].name != "0002_b_c" || ms[1].sql != "2" {
		t.Errorf("parseMigrations = %+v, %v", ms, err)
	}
}

var backupFileName = regexp.MustCompile(`^pre-([0-9]+)-([0-9]{8}T[0-9]{6}Z)\.db$`)

// TestBackupsOnUpgrades upgrades v1 → v2 → … → v8 one version per start. Each upgrade from ≥ 1 writes
// backups/pre-<old>-<ts>.db, which opens and has the old version. Every backup has a distinct <old>, so rotation
// keeps all of them.
func TestBackupsOnUpgrades(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	all := fakeMigrations(8)
	base := LatestSchemaVersion()
	for v := base; v <= 8; v++ {
		if err := e.openClose(all[:v]); err != nil {
			t.Fatalf("open at v%d: %v", v, err)
		}
		e.clock.Advance(time.Second)
	}
	got := e.backups()
	if len(got) != 8-base {
		t.Fatalf("backups = %v, want one per upgrade (%d)", got, 8-base)
	}
	ts := e.clock.Now().Add(-time.Duration(8-base+1) * time.Second)
	for i, name := range got {
		old := base + i
		ts = ts.Add(time.Second)
		want := fmt.Sprintf("pre-%d-%s.db", old, ts.UTC().Format("20060102T150405Z"))
		if name != want {
			t.Errorf("backup %d = %s, want %s", i, name, want)
		}
		path := filepath.Join(e.backupDir(), name)
		recorded, userVersion := fileVersions(t, path)
		if recorded != old || userVersion != old {
			t.Errorf("%s has version %d (user_version %d), want %d", name, recorded, userVersion, old)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %o, want 600", name, fi.Mode().Perm())
		}
	}
	recorded, userVersion := fileVersions(t, e.dbPath())
	if recorded != 8 || userVersion != 8 {
		t.Errorf("DB at %d (user_version %d), want 8", recorded, userVersion)
	}

	// Stats lists them newest first.
	db := e.open(all)
	st, err := db.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := slices.Clone(got)
	slices.Reverse(want)
	if !slices.Equal(st.Backups, want) {
		t.Errorf("Stats.Backups = %v, want %v", st.Backups, want)
	}
}

// TestBackupRotationSameVersion upgrades from the same old version again and again (a restored v1 file each time):
// only the newest 5 remain, and older versions keep their newest file.
func TestBackupRotationSameVersion(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	all := fakeMigrations(3)
	// One pre-2 file from a v2 → v3 upgrade, then seven pre-1 files.
	if err := e.openClose(all[:2]); err != nil { // new DB at v2 (no backup)
		t.Fatal(err)
	}
	if err := e.openClose(all[:3]); err != nil { // v2 → v3: pre-2
		t.Fatal(err)
	}
	pre2 := e.backups()
	if len(pre2) != 1 || !strings.HasPrefix(pre2[0], "pre-2-") {
		t.Fatalf("backups = %v", pre2)
	}
	// Save a v1 database to restore before each start.
	v1 := newEnv(t)
	v1.clock = e.clock
	if err := v1.openClose(all[:1]); err != nil {
		t.Fatal(err)
	}
	v1Bytes, err := os.ReadFile(v1.dbPath())
	if err != nil {
		t.Fatal(err)
	}
	for range 7 {
		e.clock.Advance(time.Second)
		if err := os.WriteFile(e.dbPath(), v1Bytes, 0o600); err != nil { //nolint:gosec // the test's own temp dir
			t.Fatal(err)
		}
		if err := e.openClose(all[:2]); err != nil { // v1 → v2: pre-1
			t.Fatal(err)
		}
	}
	got := e.backups()
	var pre1 []string
	for _, n := range got {
		if strings.HasPrefix(n, "pre-1-") {
			pre1 = append(pre1, n)
		}
	}
	if len(pre1) != 5 {
		t.Errorf("pre-1 backups = %v, want the newest 5", pre1)
	}
	if !slices.Contains(got, pre2[0]) {
		t.Errorf("the only pre-2 backup was rotated away: %v", got)
	}
	if len(got) != 6 {
		t.Errorf("backups = %v, want 5 pre-1 + 1 pre-2", got)
	}
}

// TestBackupRestartLoop: a DB at v3 and a list whose v6 fails. Eight starts (the clock moves 1 s per start) each
// return ErrNeedsOperator. The first writes pre-3-*.db and each later one a pre-5-*.db; afterwards the pre-3 file is
// still there, next to exactly 5 pre-5 files.
func TestBackupRestartLoop(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	all := fakeMigrations(8)
	if err := e.openClose(all[:3]); err != nil {
		t.Fatal(err)
	}
	if b := e.backups(); len(b) != 0 { // a new DB needs no backup
		t.Fatalf("backups after creating the DB: %v", b)
	}
	broken := slices.Clone(all)
	broken[5].sql = "CREATE TABLE fake_6 (x INTEGER) STRICT; INSERT INTO no_such_table VALUES (1);"
	for i := range 8 {
		e.clock.Advance(time.Second)
		err := e.openClose(broken)
		if !errors.Is(err, ErrNeedsOperator) {
			t.Fatalf("start %d: %v, want ErrNeedsOperator", i+1, err)
		}
		if !strings.Contains(err.Error(), "store: migration 0006_fake_6 failed") {
			t.Errorf("start %d: message %q", i+1, err)
		}
	}
	var pre3, pre5 []string
	for _, n := range e.backups() {
		m := backupFileName.FindStringSubmatch(n)
		switch {
		case m == nil:
			t.Errorf("unexpected file %s", n)
		case m[1] == "3":
			pre3 = append(pre3, n)
		case m[1] == "5":
			pre5 = append(pre5, n)
		default:
			t.Errorf("unexpected backup %s", n)
		}
	}
	if len(pre3) != 1 || len(pre5) != 5 {
		t.Errorf("pre-3 = %v, pre-5 = %v; want 1 and 5", pre3, pre5)
	}
	// Migrations 4 and 5 committed; 6 rolled back.
	recorded, userVersion := fileVersions(t, e.dbPath())
	if recorded != 5 || userVersion != 5 {
		t.Errorf("DB at %d (user_version %d), want 5", recorded, userVersion)
	}
	// The newest pre-5 files are the last five starts.
	want := []string{}
	for i := 4; i <= 8; i++ {
		ts := e.clock.Now().Add(-time.Duration(8-i) * time.Second)
		want = append(want, fmt.Sprintf("pre-5-%s.db", ts.UTC().Format("20060102T150405Z")))
	}
	if !slices.Equal(pre5, want) {
		t.Errorf("pre-5 = %v, want %v", pre5, want)
	}
}

func TestRotateBackups(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	files := []string{
		"pre-1-20260101T000000Z.db", // newest of v1: kept
		"pre-2-20260102T000000Z.db", // older v2: deleted
		"pre-2-20260103T000000Z.db", // newest of v2 but not in the newest 5: kept
		"pre-3-20260104T000000Z.db", // newest 5 from here
		"pre-3-20260105T000000Z.db",
		"pre-3-20260106T000000Z.db",
		"pre-3-20260107T000000Z.db",
		"pre-3-20260108T000000Z.db",
		"pre-restore-20260101T000000Z.tar.gz", // not ours
		"pre-x-20260101T000000Z.db",           // not ours
		"notes.txt",
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := rotateBackups(dir); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	want := slices.DeleteFunc(slices.Clone(files), func(s string) bool { return s == "pre-2-20260102T000000Z.db" })
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("after rotation: %v\nwant %v", got, want)
	}
	if err := rotateBackups(filepath.Join(dir, "missing")); err != nil {
		t.Errorf("rotateBackups(missing dir) = %v", err)
	}
}

func TestLowFreeSpaceFailsBeforeAnyChange(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	all := fakeMigrations(2)
	if err := e.openClose(all[:1]); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(e.dbPath())
	if err != nil {
		t.Fatal(err)
	}
	size, _ := fileSize(e.dbPath())
	var askedDir string
	o := e.opts(all)
	o.freeSpace = func(dir string) (uint64, bool, error) {
		askedDir = dir
		return uint64(2*size) - 1, true, nil //nolint:gosec // a small positive size
	}
	_, err = Open(context.Background(), o)
	if !errors.Is(err, ErrNeedsOperator) {
		t.Fatalf("Open = %v, want ErrNeedsOperator", err)
	}
	if !strings.HasPrefix(err.Error(), "store: not enough free disk space for the pre-migration backup (need ") {
		t.Errorf("message = %q", err)
	}
	if askedDir != e.backupDir() {
		t.Errorf("free space checked for %q, want the backup dir", askedDir)
	}
	if b := e.backups(); len(b) != 0 {
		t.Errorf("backups written: %v", b)
	}
	after, _ := os.ReadFile(e.dbPath())
	recorded, _ := fileVersions(t, e.dbPath())
	if recorded != 1 || len(after) != len(before) {
		t.Errorf("the DB changed: version %d", recorded)
	}

	// Exactly enough space works; an unknown amount (ok=false) skips the check.
	o.freeSpace = func(string) (uint64, bool, error) { return uint64(2 * size), true, nil } //nolint:gosec // small
	if db, err := Open(context.Background(), o); err != nil {
		t.Fatalf("Open with exactly enough space: %v", err)
	} else if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	e2 := newEnv(t)
	if err := e2.openClose(all[:1]); err != nil {
		t.Fatal(err)
	}
	o2 := e2.opts(all)
	o2.freeSpace = func(string) (uint64, bool, error) { return 0, false, nil }
	if err := func() error {
		db, err := Open(context.Background(), o2)
		if err != nil {
			return err
		}
		return db.Close()
	}(); err != nil {
		t.Errorf("Open with unknown free space: %v", err)
	}
	// A failing statfs is an error, but not an operator problem.
	e3 := newEnv(t)
	if err := e3.openClose(all[:1]); err != nil {
		t.Fatal(err)
	}
	o3 := e3.opts(all)
	o3.freeSpace = func(string) (uint64, bool, error) { return 0, false, errors.New("statfs failed") }
	if _, err := Open(context.Background(), o3); err == nil || errors.Is(err, ErrNeedsOperator) {
		t.Errorf("Open with a failing statfs = %v", err)
	}
}

func TestDiskFree(t *testing.T) {
	t.Parallel()
	free, ok, err := diskFree(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if ok && free == 0 {
		t.Error("diskFree reports 0 bytes free in the temp dir")
	}
}

func TestSchemaTooNew(t *testing.T) {
	t.Parallel()
	all := fakeMigrations(9)

	t.Run("with backup", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		if err := e.openClose(all[:8]); err != nil {
			t.Fatal(err)
		}
		e.clock.Advance(time.Minute)
		o := e.opts(all)
		o.AppVersion = "0.6.1"
		if err := func() error { // 8 → 9 by the newer binary writes pre-8
			db, err := Open(context.Background(), o)
			if err != nil {
				return err
			}
			return db.Close()
		}(); err != nil {
			t.Fatal(err)
		}
		// An older pre-8 file too: Backup must name the newest.
		older := filepath.Join(e.backupDir(), "pre-8-20200101T000000Z.db")
		if err := os.WriteFile(older, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(e.dbPath())
		_, err := Open(context.Background(), e.opts(all[:8]))
		var tooNew *SchemaTooNewError
		if !errors.As(err, &tooNew) {
			t.Fatalf("Open = %v, want *SchemaTooNewError", err)
		}
		if !errors.Is(err, ErrNeedsOperator) {
			t.Error("SchemaTooNewError does not wrap ErrNeedsOperator")
		}
		wantBackup := filepath.Join(e.backupDir(), "pre-8-"+e.clock.Now().UTC().Format("20060102T150405Z")+".db")
		if tooNew.DBVersion != 9 || tooNew.BinaryVersion != 8 || tooNew.LastAppVersion != "0.6.1" ||
			tooNew.Backup != wantBackup {
			t.Errorf("SchemaTooNewError = %+v, want backup %s", tooNew, wantBackup)
		}
		want := "database schema 9 is newer than this isshoni build supports (8); it was last used by isshoni " +
			"0.6.1. Install isshoni 0.6.1 or newer, or restore the database from before the upgrade: " +
			wantBackup + " (changes made after that backup are lost)"
		if err.Error() != want {
			t.Errorf("message:\n%s\nwant:\n%s", err, want)
		}
		// Refusing changes nothing: the DB is still at 9 with the newer binary's meta.
		after, _ := os.ReadFile(e.dbPath())
		if len(after) != len(before) {
			t.Error("the refused DB changed size")
		}
		var last string
		if err := rawDB(t, e.dbPath()).QueryRowContext(context.Background(),
			"SELECT value FROM meta WHERE key = 'last_app_version'").Scan(&last); err != nil || last != "0.6.1" {
			t.Errorf("last_app_version = %q, %v after a refused start", last, err)
		}
	})

	t.Run("without backup", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		if err := e.openClose(all); err != nil { // a new DB at 9: no backups at all
			t.Fatal(err)
		}
		_, err := Open(context.Background(), e.opts(all[:8]))
		var tooNew *SchemaTooNewError
		if !errors.As(err, &tooNew) {
			t.Fatalf("Open = %v, want *SchemaTooNewError", err)
		}
		if tooNew.Backup != "" {
			t.Errorf("Backup = %q, want empty", tooNew.Backup)
		}
		want := "database schema 9 is newer than this isshoni build supports (8); it was last used by isshoni " +
			"0.1.0. Install isshoni 0.1.0 or newer; no backup for schema 8 was found in " + e.backupDir()
		if err.Error() != want {
			t.Errorf("message:\n%s\nwant:\n%s", err, want)
		}
	})
}

func TestSchemaTooNewMessageWithoutVersion(t *testing.T) {
	t.Parallel()
	want := "database schema 5 is newer than this isshoni build supports (4); it was last used by a newer isshoni. " +
		"Install a newer isshoni; no backup for schema 4 was found"
	// "" is a DB without meta.last_app_version; "unknown" is what a build without version data records.
	for _, last := range []string{"", unknownAppVersion} {
		e := &SchemaTooNewError{DBVersion: 5, BinaryVersion: 4, LastAppVersion: last}
		if e.Error() != want {
			t.Errorf("LastAppVersion %q, message:\n%s\nwant:\n%s", last, e.Error(), want)
		}
	}
}

func TestHistoryMismatch(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	all := fakeMigrations(3)
	if err := e.openClose(all); err != nil {
		t.Fatal(err)
	}
	renamed := slices.Clone(all)
	renamed[1].name = "0002_renamed"
	_, err := Open(context.Background(), e.opts(renamed))
	if !errors.Is(err, ErrNeedsOperator) {
		t.Fatalf("Open = %v, want ErrNeedsOperator", err)
	}
	want := `store: schema history mismatch at version 2 (DB has "0002_fake_2", binary has "0002_renamed")`
	if !strings.HasPrefix(err.Error(), want) {
		t.Errorf("message = %q, want prefix %q", err, want)
	}
	// Also for a DB whose recorded name changed (a fork or a dev build), and for a gap in the history.
	raw := rawDB(t, e.dbPath())
	if _, err := raw.ExecContext(context.Background(), "UPDATE schema_migrations SET name = '0003_other' WHERE version = 3"); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	_, err = Open(context.Background(), e.opts(all))
	if !errors.Is(err, ErrNeedsOperator) || !strings.Contains(err.Error(), `version 3 (DB has "0003_other", binary has "0003_fake_3")`) {
		t.Errorf("renamed row: %v", err)
	}
	if err := checkHistory([]appliedRow{{1, "0001_init"}, {3, "0003_fake_3"}}, all); err == nil ||
		!strings.Contains(err.Error(), "mismatch at version 2") {
		t.Errorf("gap: %v", err)
	}
	if err := checkHistory([]appliedRow{{1, "0001_init"}, {2, "0002_fake_2"}, {3, "0003_fake_3"}, {4, "0004_newer"}}, all); err != nil {
		t.Errorf("versions beyond the binary are not a mismatch: %v", err)
	}
}

// TestNeedsOperator checks every case of 03 §4.3.
func TestNeedsOperator(t *testing.T) {
	t.Parallel()
	all := fakeMigrations(3)
	upgradeWith := func(t *testing.T, mutate func([]migration)) error {
		t.Helper()
		e := newEnv(t)
		if err := e.openClose(all[:2]); err != nil {
			t.Fatal(err)
		}
		ms := slices.Clone(all)
		mutate(ms)
		return e.openClose(ms)
	}
	cases := map[string]func(t *testing.T) (string, error){
		"history mismatch": func(t *testing.T) (string, error) {
			return "schema history mismatch", upgradeWith(t, func(ms []migration) { ms[0].name = "0001_other" })
		},
		"failed migration SQL": func(t *testing.T) (string, error) {
			return "migration 0003_fake_3 failed: SQL",
				upgradeWith(t, func(ms []migration) { ms[2].sql = "THIS IS NOT SQL" })
		},
		"failed Go step": func(t *testing.T) (string, error) {
			return "migration 0003_fake_3 failed: Go step: rewrite failed", upgradeWith(t, func(ms []migration) {
				ms[2].goStep = func(context.Context, *sql.Tx) error { return errors.New("rewrite failed") }
			})
		},
		"failed foreign key check": func(t *testing.T) (string, error) {
			return "migration 0003_fake_3 failed: foreign key check: row 1 of table sessions violates its foreign key",
				upgradeWith(t, func(ms []migration) {
					ms[2].sql = `INSERT INTO sessions (id, user_id, token_hash, name, created_at, rotated_at,
					  last_seen_at, last_ip, idle_expires_at, expires_at)
					  VALUES ('s', 'no-such-user', x'00', 'n', 0, 0, 0, '', 0, 0)`
				})
		},
		"new DB with a failed migration": func(t *testing.T) (string, error) {
			ms := slices.Clone(all)
			ms[1].sql = "CREATE TABLE fake_2 (x INTEGER) STRICT; CREATE TABLE fake_2 (y INTEGER) STRICT;"
			return "migration 0002_fake_2 failed", newEnv(t).openClose(ms)
		},
		"not a database": func(t *testing.T) (string, error) {
			e := newEnv(t)
			if err := os.WriteFile(e.dbPath(), []byte(strings.Repeat("not a database file ", 400)), 0o600); err != nil {
				t.Fatal(err)
			}
			return "is corrupt or not an SQLite database", e.openClose(nil)
		},
		"corrupt schema page": func(t *testing.T) (string, error) {
			e := newEnv(t)
			if err := e.openClose(nil); err != nil {
				t.Fatal(err)
			}
			corruptPage(t, e.dbPath(), 1)
			return "is corrupt or not an SQLite database", e.openClose(nil)
		},
		"WAL unavailable": func(t *testing.T) (string, error) {
			return "store: WAL mode unavailable at /x/isshoni.db (network filesystem?)",
				checkJournalMode("delete", "/x/isshoni.db")
		},
		"low free space": func(t *testing.T) (string, error) {
			e := newEnv(t)
			if err := e.openClose(all[:2]); err != nil {
				t.Fatal(err)
			}
			o := e.opts(all)
			o.freeSpace = func(string) (uint64, bool, error) { return 1, true, nil }
			_, err := Open(context.Background(), o)
			return "not enough free disk space", err
		},
		"schema too new": func(t *testing.T) (string, error) {
			e := newEnv(t)
			if err := e.openClose(all); err != nil {
				t.Fatal(err)
			}
			return "database schema 3 is newer than this isshoni build supports (2)", e.openClose(all[:2])
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			want, err := run(t)
			if !errors.Is(err, ErrNeedsOperator) {
				t.Fatalf("err = %v, want ErrNeedsOperator", err)
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("message %q does not contain %q", err, want)
			}
		})
	}
}

// TestWALUnavailableOnOpen runs the WAL check end to end: an in-memory database can't use WAL.
func TestWALUnavailableOnOpen(t *testing.T) {
	t.Chdir(t.TempDir())
	_, err := Open(context.Background(), Options{Path: ":memory:", Logger: newEnv(t).opts(nil).Logger})
	if !errors.Is(err, ErrNeedsOperator) || !strings.Contains(err.Error(), "WAL mode unavailable") {
		t.Errorf("Open(:memory:) = %v, want the WAL error", err)
	}
}

// corruptPage overwrites the b-tree header of a page (1-based) with an invalid page type.
func corruptPage(t *testing.T, path string, page int) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // a fixture in the test's temp dir
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var hdr [2]byte
	if _, err := f.ReadAt(hdr[:], 16); err != nil {
		t.Fatal(err)
	}
	pageSize := int64(hdr[0])<<8 | int64(hdr[1])
	if pageSize == 1 {
		pageSize = 65536
	}
	off := int64(page-1) * pageSize
	if page == 1 {
		off += 100 // the database header
	}
	if _, err := f.WriteAt([]byte{0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42}, off); err != nil {
		t.Fatal(err)
	}
}

func TestCancelledContextIsNotAnOperatorError(t *testing.T) {
	t.Parallel()
	t.Run("before open", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := Open(ctx, newEnv(t).opts(nil))
		if err == nil || errors.Is(err, ErrNeedsOperator) || !errors.Is(err, context.Canceled) {
			t.Errorf("Open = %v, want context.Canceled without ErrNeedsOperator", err)
		}
	})
	t.Run("during a migration", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		all := fakeMigrations(3)
		if err := e.openClose(all[:2]); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ms := slices.Clone(all)
		ms[2].goStep = func(ctx context.Context, tx *sql.Tx) error {
			cancel()
			_, err := tx.ExecContext(ctx, "SELECT 1")
			return err
		}
		_, err := Open(ctx, e.opts(ms))
		if err == nil || errors.Is(err, ErrNeedsOperator) || !errors.Is(err, context.Canceled) {
			t.Errorf("Open = %v, want context.Canceled without ErrNeedsOperator", err)
		}
		// The migration rolled back and a later start completes it.
		if recorded, _ := fileVersions(t, e.dbPath()); recorded != 2 {
			t.Errorf("DB at %d after a cancelled migration, want 2", recorded)
		}
		// The next start in the same second can't reuse the backup name; it moves its timestamp forward.
		if err := e.openClose(all); err != nil {
			t.Errorf("the next start: %v", err)
		}
		want := []string{"pre-2-20261014T021500Z.db", "pre-2-20261014T021501Z.db"}
		if got := e.backups(); !slices.Equal(got, want) {
			t.Errorf("backups = %v, want %v", got, want)
		}
	})
}

func TestGoStepRunsInTheMigrationTransaction(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	all := fakeMigrations(2)
	if err := e.openClose(all[:1]); err != nil {
		t.Fatal(err)
	}
	ms := slices.Clone(all)
	ms[1].goStep = func(ctx context.Context, tx *sql.Tx) error {
		// The SQL of this version ran first, and foreign keys are off (table rebuilds need that).
		var fk int
		if err := tx.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			return err
		}
		if fk != 0 {
			return fmt.Errorf("foreign_keys = %d during a migration", fk)
		}
		_, err := tx.ExecContext(ctx, "UPDATE fake_2 SET x = 42")
		return err
	}
	db := e.open(ms)
	if n := queryInt(t, db, "SELECT x FROM fake_2"); n != 42 {
		t.Errorf("fake_2.x = %d, want 42", n)
	}
	// Foreign keys are back on for normal writes.
	err := execW(t, db, `INSERT INTO sessions (id, user_id, token_hash, name, created_at, rotated_at, last_seen_at,
		last_ip, idle_expires_at, expires_at) VALUES ('s', 'no-such-user', x'00', 'n', 0, 0, 0, '', 0, 0)`)
	if err == nil {
		t.Error("foreign keys are off after migrating")
	}
}
