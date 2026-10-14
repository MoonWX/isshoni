package ops

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/ops/doctor"
	"github.com/MoonWX/isshoni/internal/version"
)

// The tests of backup and restore run without 03's store, as ops itself compiles without it (04 §2): a database is
// a file that starts with SQLite's header and then says, as JSON, what store.InspectFile would find in it, and the
// functions of doctor.DBFiles read that. What the real functions find in real databases is 03's to test; that the
// server hands the real ones to this code is tested in internal/server.

// latestSchema is the schema of "this build" in these tests.
const latestSchema = 7

// fakeDB describes a database file of these tests.
type fakeDB struct {
	Schema    int    `json:"schema"`
	App       string `json:"app,omitempty"`       // meta.last_app_version
	NoHistory bool   `json:"noHistory,omitempty"` // the migration history is another build's
	Damage    string `json:"damage,omitempty"`    // what PRAGMA integrity_check complains about
	Note      string `json:"note,omitempty"`      // tells two databases apart
}

func (d fakeDB) bytes() []byte {
	doc, err := json.Marshal(d)
	if err != nil {
		panic(err)
	}
	return append([]byte("SQLite format 3\x00"), doc...)
}

// readFakeDB is the fake's store.InspectFile.
func readFakeDB(path string) (fakeDB, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return fakeDB{}, err
	}
	doc, ok := bytes.CutPrefix(data, []byte("SQLite format 3\x00"))
	var d fakeDB
	if !ok || json.Unmarshal(doc, &d) != nil {
		return fakeDB{}, errors.New("file is not a database")
	}
	return d, nil
}

// fakeDBFiles are 03's file-level functions over fakeDB files.
func fakeDBFiles() doctor.DBFiles {
	return doctor.DBFiles{
		LatestSchemaVersion: func() int { return latestSchema },
		Inspect: func(_ context.Context, path string) (doctor.DBInfo, error) {
			d, err := readFakeDB(path)
			if err != nil {
				return doctor.DBInfo{}, err
			}
			info := doctor.DBInfo{SchemaVersion: d.Schema, LastAppVersion: d.App, HistoryOK: !d.NoHistory}
			if d.Damage != "" {
				info.Integrity = errors.New(d.Damage)
			}
			return info, nil
		},
		Backup: copyDB,
	}
}

// copyDB is the fake's store.BackupFile: a copy of a file that is a database, to a path that does not exist yet.
func copyDB(_ context.Context, src, dst string) error {
	if _, err := readFakeDB(src); err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return errors.Join(err, f.Close())
}

// testNow is the clock of the tests' data directories.
var testNow = time.Date(2026, 9, 29, 10, 15, 0, 123e6, time.UTC)

// testData is a data directory of a test with its DataDir.
type testData struct {
	*DataDir
	dir        string
	logs       *syncBuffer
	ownSecrets string // the secrets.json it was made with
}

// newTestData makes a data directory as a server leaves it: a database, a secrets.json with real keys and a
// certificate storage with a certificate, an ACME account and a lock. db is what its database is. The DataDir is
// an offline one; online makes it a running server's.
func newTestData(t testing.TB, db fakeDB) *testData {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := (&config.Config{DataDir: dir}).Paths()
	log, logs := testLogger()
	td := &testData{dir: dir, logs: logs, DataDir: &DataDir{
		Paths: p, DB: fakeDBFiles(), Now: func() time.Time { return testNow }, Logger: log,
	}}
	td.write(t, "isshoni.db", db.bytes())
	if _, err := config.OpenSecrets(p.Secrets, nil); err != nil {
		t.Fatal(err)
	}
	td.ownSecrets = td.read(t, "secrets.json")
	td.write(t, "certmagic/certificates/acme-v02/203.0.113.7/203.0.113.7.crt", []byte("certificate of "+db.Note))
	td.write(t, "certmagic/certificates/acme-v02/203.0.113.7/203.0.113.7.key", []byte("key of "+db.Note))
	td.write(t, "certmagic/acme/acme-v02/users/default/default.json", []byte(`{"account":"`+db.Note+`"}`))
	td.write(t, "certmagic/locks/issue_cert_203.0.113.7.lock", []byte("a lock"))
	return td
}

// online makes the DataDir that of a running server: its snapshot is a copy of the database file.
func (td *testData) online() *testData {
	td.Snapshot = func(ctx context.Context, dst string) error { return copyDB(ctx, td.Paths.DB, dst) }
	return td
}

// write writes a file below the data directory, with its directories.
func (td *testData) write(t testing.TB, rel string, data []byte) {
	t.Helper()
	path := filepath.Join(td.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (td *testData) read(t testing.TB, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(td.dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// tree lists what is below dir: every file with its content, every directory as "dir" and anything else by its
// mode's type, by slash-separated path. Paths for which skip reports true are left out, with what is below them. A
// dir that does not exist has nothing below it.
func tree(t testing.TB, dir string, skip func(rel string) bool) map[string]string {
	t.Helper()
	out := map[string]string{}
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return out
	}
	err := filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		rel = filepath.ToSlash(rel)
		switch {
		case rel == ".":
		case skip != nil && skip(rel):
			if e.IsDir() {
				return fs.SkipDir
			}
		case e.IsDir():
			out[rel] = "dir"
		case e.Type().IsRegular():
			data, err := os.ReadFile(path) //nolint:gosec // G122: the test's own directory
			if err != nil {
				return err
			}
			out[rel] = string(data)
		default:
			out[rel] = e.Type().String()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// serverFiles is tree for the files that are the server's state: the database with its WAL, secrets.json and the
// certificate storage. The lock file, backups/ and restore/ are not.
func (td *testData) serverFiles(t testing.TB) map[string]string {
	t.Helper()
	return tree(t, td.dir, func(rel string) bool {
		return rel == "backups" || rel == "restore" || rel == "isshoni.lock"
	})
}

// tarEntry is one entry of an archive as a test reads or writes it.
type tarEntry struct {
	name string
	kind byte // tar.TypeReg unless set
	mode int64
	body string
	link string
}

// readArchive unpacks a backup archive in memory.
func readArchive(t testing.TB, archive []byte) (Manifest, []tarEntry) {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	var entries []tarEntry
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reading the archive: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Uid != 0 || hdr.Gid != 0 || hdr.Uname != "" || hdr.Gname != "" {
			t.Errorf("%s has an owner (%d:%d %q:%q), want none", hdr.Name, hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname)
		}
		entries = append(entries, tarEntry{name: hdr.Name, kind: hdr.Typeflag, mode: hdr.Mode, body: string(body)})
	}
	if len(entries) == 0 || entries[0].name != "manifest.json" {
		t.Fatalf("the archive does not start with manifest.json: %+v", entries)
	}
	var m Manifest
	if err := json.Unmarshal([]byte(entries[0].body), &m); err != nil {
		t.Fatalf("manifest: %v\n%s", err, entries[0].body)
	}
	return m, entries[1:]
}

// archiveOf writes the backup of td into memory.
func archiveOf(t testing.TB, td *testData, o BackupOptions) []byte {
	t.Helper()
	b, err := td.PrepareBackup(t.Context(), o)
	if err != nil {
		t.Fatalf("PrepareBackup: %v", err)
	}
	defer b.Close()
	var buf bytes.Buffer
	if err := b.WriteTo(t.Context(), &buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	return buf.Bytes()
}

func sumOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func names(entries []tarEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.name
	}
	return out
}

// The archive of 04 §12.3: manifest.json first, then the database's snapshot, secrets.json and the certificate
// storage without locks/, every file 0600 and without an owner, and a manifest that says what is there.
func TestBackupArchive(t *testing.T) {
	for _, mode := range []string{"running server", "offline"} {
		t.Run(mode, func(t *testing.T) {
			td := newTestData(t, fakeDB{Schema: 6, App: "0.2.9", Note: "alpha"})
			if mode == "running server" {
				td.online()
			}
			// What a backup never holds: the WAL of a database it snapshots, and what is no file of certmagic's.
			td.write(t, "isshoni.db-wal", []byte("uncheckpointed pages"))
			if runtime.GOOS != "windows" {
				if err := os.Symlink("/etc/passwd", filepath.Join(td.Paths.CertMagic, "passwd")); err != nil {
					t.Fatal(err)
				}
			}
			before := td.serverFiles(t)

			b, err := td.PrepareBackup(t.Context(), BackupOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if want := "isshoni-backup-" + version.Version() + "-20260929T101500Z.tar.gz"; b.Filename() != want {
				t.Errorf("Filename = %q, want %q", b.Filename(), want)
			}
			if snaps, _ := filepath.Glob(filepath.Join(td.Paths.Restore, "snapshot-*.db")); len(snaps) != 1 {
				t.Errorf("snapshots while the backup is staged: %v, want one", snaps)
			}
			var buf bytes.Buffer
			if err := b.WriteTo(t.Context(), &buf); err != nil {
				t.Fatal(err)
			}
			b.Close()
			if left := tree(t, td.Paths.Restore, nil); len(left) != 0 {
				t.Errorf("restore/ after the backup: %v, want nothing", left)
			}
			if after := td.serverFiles(t); !reflect.DeepEqual(after, before) {
				t.Errorf("the backup changed the data directory:\n got %v\nwant %v", after, before)
			}

			m, entries := readArchive(t, buf.Bytes())
			const certs = "certmagic/certificates/acme-v02/203.0.113.7/"
			wantNames := []string{
				"isshoni.db", "secrets.json",
				"certmagic/acme/", "certmagic/acme/acme-v02/", "certmagic/acme/acme-v02/users/",
				"certmagic/acme/acme-v02/users/default/", "certmagic/acme/acme-v02/users/default/default.json",
				"certmagic/certificates/", "certmagic/certificates/acme-v02/", certs, certs + "203.0.113.7.crt", certs + "203.0.113.7.key",
			}
			if got := names(entries); !slices.Equal(got, wantNames) {
				t.Errorf("entries:\n got %v\nwant %v", got, wantNames)
			}
			wantSums := map[string]string{}
			for _, e := range entries {
				switch {
				case strings.HasSuffix(e.name, "/"):
					if e.kind != tar.TypeDir || e.mode != 0o700 {
						t.Errorf("%s: type %q mode %o, want a directory with 0700", e.name, e.kind, e.mode)
					}
				default:
					if e.kind != tar.TypeReg || e.mode != 0o600 {
						t.Errorf("%s: type %q mode %o, want a regular file with 0600", e.name, e.kind, e.mode)
					}
					wantSums[e.name] = sumOf(e.body)
					if want := td.read(t, e.name); e.body != want {
						t.Errorf("%s holds %q, want the file's %q", e.name, e.body, want)
					}
				}
			}
			want := Manifest{
				Format: 1, IsshoniVersion: version.Version(), SchemaVersion: 6,
				CreatedAt: api.WireTime(time.Date(2026, 9, 29, 10, 15, 0, 0, time.UTC)),
				Contents:  []string{"isshoni.db", "secrets.json", "certmagic/"},
				SHA256:    wantSums,
			}
			if !reflect.DeepEqual(m, want) {
				t.Errorf("manifest:\n got %+v\nwant %+v", m, want)
			}

			// --no-certs: the same without the certificate storage.
			m, entries = readArchive(t, archiveOf(t, td, BackupOptions{NoCerts: true}))
			if got := names(entries); !slices.Equal(got, []string{"isshoni.db", "secrets.json"}) ||
				!slices.Equal(m.Contents, []string{"isshoni.db", "secrets.json"}) || len(m.SHA256) != 2 {
				t.Errorf("--no-certs: entries %v, manifest %+v", got, m)
			}
		})
	}
}

// manifest.json is written as the document of 04 §12.3: camelCase, the timestamp in the wire form.
func TestBackupManifestJSON(t *testing.T) {
	td := newTestData(t, fakeDB{Schema: 7, Note: "alpha"})
	_ = os.RemoveAll(td.Paths.CertMagic)
	gz, err := gzip.NewReader(bytes.NewReader(archiveOf(t, td, BackupOptions{})))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	if _, err := tr.Next(); err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(tr)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	sums, _ := doc["sha256"].(map[string]any)
	want := map[string]any{
		"format": float64(1), "isshoniVersion": version.Version(), "schemaVersion": float64(7),
		"createdAt": "2026-09-29T10:15:00.000Z",
		"contents":  []any{"isshoni.db", "secrets.json"},
		"sha256":    map[string]any{"isshoni.db": sumOf(td.read(t, "isshoni.db")), "secrets.json": sumOf(td.read(t, "secrets.json"))},
	}
	if !reflect.DeepEqual(doc, want) {
		t.Errorf("manifest.json = %s\nwant %v (sha256 has %d entries)", raw, want, len(sums))
	}
	// ReadManifest reads the same from the archive.
	m, err := ReadManifest(bytes.NewReader(archiveOf(t, td, BackupOptions{})))
	if err != nil || m.SchemaVersion != 7 || m.Format != 1 || !slices.Equal(m.Contents, []string{"isshoni.db", "secrets.json"}) {
		t.Errorf("ReadManifest = %+v, %v", m, err)
	}
	for name, data := range map[string]string{"not gzip": "plain text", "empty": ""} {
		if _, err := ReadManifest(strings.NewReader(data)); !api.IsCode(err, api.CodeBackupInvalid) {
			t.Errorf("ReadManifest of %s: %v, want backup_invalid", name, err)
		}
	}
}

// An offline backup snapshots the database with store.BackupFile, and copies the raw files, with "raw": true, only
// when that fails (04 §12.3). A running server's snapshot that fails is an error: its files are in use.
func TestBackupRawFallback(t *testing.T) {
	td := newTestData(t, fakeDB{Schema: 7, Note: "alpha"})
	td.write(t, "isshoni.db-wal", []byte("wal pages"))
	td.write(t, "isshoni.db-shm", []byte("wal index"))

	m, entries := readArchive(t, archiveOf(t, td, BackupOptions{NoCerts: true}))
	if m.Raw || !slices.Equal(names(entries), []string{"isshoni.db", "secrets.json"}) {
		t.Errorf("a database that can be snapshotted: raw=%v entries %v, want a snapshot alone", m.Raw, names(entries))
	}

	// A database that the snapshot can't read: what is left of it is copied as it is.
	td.write(t, "isshoni.db", []byte("SQLite format 3\x00 and then garbage"))
	m, entries = readArchive(t, archiveOf(t, td, BackupOptions{NoCerts: true}))
	wantNames := []string{"isshoni.db", "isshoni.db-wal", "isshoni.db-shm", "secrets.json"}
	if !m.Raw || m.SchemaVersion != 0 || !slices.Equal(names(entries), wantNames) || !slices.Equal(m.Contents, wantNames) {
		t.Errorf("a damaged database: raw=%v schema %d entries %v contents %v", m.Raw, m.SchemaVersion, names(entries), m.Contents)
	}
	for _, e := range entries {
		if want := td.read(t, e.name); e.body != want || m.SHA256[e.name] != sumOf(want) {
			t.Errorf("%s: body %q, sha256 %s; want the raw file", e.name, e.body, m.SHA256[e.name])
		}
	}
	if !strings.Contains(td.logs.String(), "the backup holds its raw files instead") {
		t.Errorf("the log does not say that the backup is a raw one:\n%s", td.logs.String())
	}
	if left := tree(t, td.Paths.Restore, nil); len(left) != 0 {
		t.Errorf("restore/ after a raw backup: %v", left)
	}

	// The running server never falls back: nothing may copy files that it has open.
	failing := errors.New("disk I/O error")
	td.Snapshot = func(context.Context, string) error { return failing }
	if _, err := td.PrepareBackup(t.Context(), BackupOptions{}); !errors.Is(err, failing) {
		t.Errorf("a failing snapshot of a running server: %v, want its error", err)
	}
	// A full disk is the refusal 507.
	td.Snapshot = func(context.Context, string) error { return fmt.Errorf("VACUUM INTO: %w", syscall.ENOSPC) }
	var de *DataError
	if _, err := td.PrepareBackup(t.Context(), BackupOptions{}); !errors.As(err, &de) || de.Code != api.CodeInsufficientStorage {
		t.Errorf("a snapshot on a full disk: %v, want insufficient_storage", err)
	}
	// Flush runs before the snapshot, and its error does not stop the backup.
	var order []string
	td.Flush = func(context.Context) error { order = append(order, "flush"); return errors.New("not now") }
	td.Snapshot = func(ctx context.Context, dst string) error {
		order = append(order, "snapshot")
		return os.WriteFile(dst, fakeDB{Schema: 7}.bytes(), 0o600)
	}
	if b, err := td.PrepareBackup(t.Context(), BackupOptions{}); err != nil || !slices.Equal(order, []string{"flush", "snapshot"}) {
		t.Errorf("PrepareBackup with a Flush: %v, order %v", err, order)
	} else {
		b.Close()
	}
}

// diskWith makes the tests' disk one with free bytes left, wherever backup and restore ask (freeBytes), until the
// test ends.
func diskWith(t *testing.T, free uint64) {
	t.Helper()
	freeBytes = func(string) (uint64, bool) { return free, true }
	t.Cleanup(func() { freeBytes = diskFree })
}

// The snapshot of the database is a second copy of it on the same disk. A disk without the room for it is the
// refusal insufficient_storage (04 §12.2), seen before the snapshot begins: a running server does not fill its own
// disk to find out, and an offline backup does not take the failed snapshot for a database that can't be read and
// fall back to its raw files.
func TestBackupWithoutRoomForTheSnapshot(t *testing.T) {
	for _, mode := range []string{"running server", "offline"} {
		t.Run(mode, func(t *testing.T) {
			td := newTestData(t, fakeDB{Schema: 7, Note: "alpha"})
			td.write(t, "isshoni.db-wal", []byte("pages that are not in the database file yet"))
			need := uint64(len(td.read(t, "isshoni.db")) + len(td.read(t, "isshoni.db-wal")))
			asked := 0
			if mode == "running server" {
				td.Snapshot = func(ctx context.Context, dst string) error { asked++; return copyDB(ctx, td.Paths.DB, dst) }
			} else {
				td.DB.Backup = func(ctx context.Context, src, dst string) error { asked++; return copyDB(ctx, src, dst) }
			}
			before := td.serverFiles(t)

			diskWith(t, need-1)
			_, err := td.PrepareBackup(t.Context(), BackupOptions{})
			var de *DataError
			if !errors.As(err, &de) || de.Code != api.CodeInsufficientStorage || de.Fix == "" {
				t.Fatalf("PrepareBackup with %d bytes free for %d: %v, want insufficient_storage with a fix", need-1, need, err)
			}
			if want := fmt.Sprintf("the database has %d bytes and %s has %d free", need, td.Paths.Restore, need-1); !strings.Contains(fmt.Sprint(de.Err), want) {
				t.Errorf("the cause, for the log: %v, want %q in it", de.Err, want)
			}
			if asked != 0 {
				t.Errorf("the snapshot was begun %d times on a disk without room for it", asked)
			}
			if strings.Contains(td.logs.String(), "raw files") {
				t.Errorf("a full disk was taken for a database that can't be read:\n%s", td.logs.String())
			}
			if left := tree(t, td.Paths.Restore, nil); len(left) != 0 {
				t.Errorf("restore/ after the refusal: %v", keys(left))
			}
			// A restore, whose first step of its own is this backup, is refused the same way and leaves nothing.
			wantRefused(t, td, fakeDB{Schema: latestSchema, Note: "restored"}.bytes(), api.CodeInsufficientStorage, "not enough free disk space")
			if after := td.serverFiles(t); !reflect.DeepEqual(after, before) {
				t.Errorf("the refusals changed the server's files: %v", keys(after))
			}

			// With exactly the room, the backup is a snapshot.
			diskWith(t, need)
			m, entries := readArchive(t, archiveOf(t, td, BackupOptions{NoCerts: true}))
			if m.Raw || asked != 1 || !slices.Equal(names(entries), []string{"isshoni.db", "secrets.json"}) {
				t.Errorf("with room for the snapshot: raw=%v, %d snapshots, entries %v", m.Raw, asked, names(entries))
			}
		})
	}

	// The look ahead is an estimate: the disk can still fill up while the snapshot is written. Offline that is no
	// reason for a raw backup either.
	td := newTestData(t, fakeDB{Schema: 7, Note: "alpha"})
	td.DB.Backup = func(context.Context, string, string) error {
		return fmt.Errorf("VACUUM INTO: database or disk is full (13) (%w)", syscall.ENOSPC)
	}
	var de *DataError
	if _, err := td.PrepareBackup(t.Context(), BackupOptions{}); !errors.As(err, &de) || de.Code != api.CodeInsufficientStorage {
		t.Errorf("an offline snapshot on a disk that fills up: %v, want insufficient_storage", err)
	}
	if strings.Contains(td.logs.String(), "raw files") {
		t.Errorf("a full disk was taken for a database that can't be read:\n%s", td.logs.String())
	}
	// A platform that can't tell the free space goes on to the snapshot.
	freeBytes = func(string) (uint64, bool) { return 0, false }
	t.Cleanup(func() { freeBytes = diskFree })
	td = newTestData(t, fakeDB{Schema: 7, Note: "alpha"})
	if m, _ := readArchive(t, archiveOf(t, td, BackupOptions{})); m.Raw || m.SchemaVersion != 7 {
		t.Errorf("a backup where the free space is unknown: %+v", m)
	}
}

// A data directory that lacks a part gives an archive without it, and an empty one an empty backup.
func TestBackupOfWhatIsThere(t *testing.T) {
	td := newTestData(t, fakeDB{Schema: 7, Note: "alpha"})
	if err := os.Remove(td.Paths.DB); err != nil {
		t.Fatal(err)
	}
	// A certificate storage without a file holds no certificate: the archive says nothing about certificates.
	_ = os.RemoveAll(td.Paths.CertMagic)
	if err := os.MkdirAll(filepath.Join(td.Paths.CertMagic, "locks"), 0o700); err != nil {
		t.Fatal(err)
	}
	m, entries := readArchive(t, archiveOf(t, td, BackupOptions{}))
	if !slices.Equal(m.Contents, []string{"secrets.json"}) || !slices.Equal(names(entries), []string{"secrets.json"}) {
		t.Errorf("without a database and certificates: contents %v, entries %v", m.Contents, names(entries))
	}
	if err := os.Remove(td.Paths.Secrets); err != nil {
		t.Fatal(err)
	}
	b, err := td.PrepareBackup(t.Context(), BackupOptions{})
	if err != nil || !b.Empty() {
		t.Fatalf("PrepareBackup of an empty data directory: %+v, %v; want an empty backup", b, err)
	}
	b.Close()
}

// WriteBackupFile makes a private file, never overwrites one, and leaves none behind when it fails.
func TestWriteBackupFile(t *testing.T) {
	td := newTestData(t, fakeDB{Schema: 7, Note: "alpha"})
	b, err := td.PrepareBackup(t.Context(), BackupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	path := filepath.Join(t.TempDir(), "b.tar.gz")
	if err := b.WriteBackupFile(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Errorf("the archive: %v, mode %v; want 0600", err, fi.Mode())
	}
	data, _ := os.ReadFile(path)
	if m, _ := readArchive(t, data); m.SchemaVersion != 7 {
		t.Errorf("manifest of the written file: %+v", m)
	}
	if err := b.WriteBackupFile(t.Context(), path); !errors.Is(err, fs.ErrExist) {
		t.Errorf("writing over an archive: %v, want fs.ErrExist", err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, data) {
		t.Error("the refused write changed the existing archive")
	}
	// A backup that is interrupted leaves no half archive.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	other := filepath.Join(t.TempDir(), "c.tar.gz")
	if err := b.WriteBackupFile(ctx, other); !errors.Is(err, context.Canceled) {
		t.Errorf("an interrupted write: %v", err)
	}
	if _, err := os.Stat(other); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the interrupted write left its file: %v", err)
	}
}

func TestBackupFileName(t *testing.T) {
	at := time.Date(2026, 9, 29, 12, 15, 0, 0, time.FixedZone("CEST", 2*3600))
	for ver, want := range map[string]string{
		"0.3.0":                   "isshoni-backup-0.3.0-20260929T101500Z.tar.gz",
		"0.0.0-dev+1a2b3c4d5e6f":  "isshoni-backup-0.0.0-dev+1a2b3c4d5e6f-20260929T101500Z.tar.gz",
		"../../etc/passwd\x00 v1": "isshoni-backup-.._.._etc_passwd__v1-20260929T101500Z.tar.gz",
	} {
		if got := BackupFileName(ver, at); got != want || filepath.Base(got) != got {
			t.Errorf("BackupFileName(%q) = %q, want %q", ver, got, want)
		}
	}
}

// TestCopyContext: copyContext copies everything to the end, stops with ctx's error once ctx has ended (checked
// before every read), and returns a write error as it is.
func TestCopyContext(t *testing.T) {
	data := bytes.Repeat([]byte("isshoni "), 100<<10) // 800 KiB: more than one 256 KiB buffer
	var out bytes.Buffer
	if n, err := copyContext(context.Background(), &out, bytes.NewReader(data)); err != nil || n != int64(len(data)) || !bytes.Equal(out.Bytes(), data) {
		t.Errorf("copyContext = %d, %v; copied %d of %d bytes", n, err, out.Len(), len(data))
	}

	ended, cancel := context.WithCancel(context.Background())
	cancel()
	out.Reset()
	if n, err := copyContext(ended, &out, bytes.NewReader(data)); !errors.Is(err, context.Canceled) || n != 0 || out.Len() != 0 {
		t.Errorf("copyContext with an ended ctx = %d, %v; copied %d bytes", n, err, out.Len())
	}

	// ctx ends while the copy runs: the next read stops it. The destination has no ReadFrom (a bytes.Buffer's would
	// take the whole input in its first read), so the copy reads 256 KiB at a time, as into a tar.Writer or a hash.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out.Reset()
	r := &cancelAfterRead{r: bytes.NewReader(data), cancel: cancel}
	if n, err := copyContext(ctx, writerOnly{&out}, r); !errors.Is(err, context.Canceled) || n != 256<<10 {
		t.Errorf("copyContext cancelled after the first read = %d, %v; want the first 256 KiB and Canceled", n, err)
	}

	failed := errors.New("no space left on device")
	if _, err := copyContext(context.Background(), failingWriter{failed}, bytes.NewReader(data)); !errors.Is(err, failed) {
		t.Errorf("copyContext into a failing writer = %v, want %v", err, failed)
	}
}

// cancelAfterRead cancels its context after its first read.
type cancelAfterRead struct {
	r      io.Reader
	cancel context.CancelFunc
}

func (c *cancelAfterRead) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.cancel()
	return n, err
}

// writerOnly hides every method of its writer but Write.
type writerOnly struct{ io.Writer }

// failingWriter fails every write with err.
type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

func TestDataError(t *testing.T) {
	cause := errors.New("gzip: invalid header")
	err := fmt.Errorf("wrapped: %w", invalidBackup("it is not a gzip file", cause))
	if !api.IsCode(err, api.CodeBackupInvalid) || !errors.Is(err, cause) {
		t.Errorf("a DataError does not unwrap to its code and its cause: %v", err)
	}
	if got := newerBackup("0.5.0").Message; got != "This backup is from isshoni 0.5.0; install 0.5.0 or newer first" {
		t.Errorf("backup_newer message = %q", got)
	}
	if got := newerBackup("").Message; !strings.Contains(got, "a newer isshoni") || strings.Contains(got, "isshoni ;") {
		t.Errorf("backup_newer without a version = %q", got)
	}
	// Only "the disk is full" becomes insufficient_storage, and a refusal stays what it is.
	if err := storageError(fmt.Errorf("write: %w", syscall.ENOSPC)); !api.IsCode(err, api.CodeInsufficientStorage) {
		t.Errorf("ENOSPC: %v", err)
	}
	if err := storageError(cause); !errors.Is(err, cause) || api.IsCode(err, api.CodeInsufficientStorage) {
		t.Errorf("another error: %v", err)
	}
	if err := storageError(invalidBackup("x", syscall.ENOSPC)); !api.IsCode(err, api.CodeBackupInvalid) {
		t.Errorf("a refusal that wraps ENOSPC: %v", err)
	}
	if storageError(nil) != nil {
		t.Error("storageError(nil) != nil")
	}
}
