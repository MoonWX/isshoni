package ops

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/version"
)

// makeArchive writes an archive by hand: the manifest as given (a Manifest, or a string for one that is no
// manifest), then the entries. With it a test builds the archives that isshoni itself never writes.
func makeArchive(t testing.TB, manifest any, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	put := func(e tarEntry) {
		kind := e.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		hdr := &tar.Header{Typeflag: kind, Name: e.name, Mode: 0o600, Linkname: e.link, ModTime: testNow}
		if kind == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %q: %v", e.name, err)
		}
		if kind == tar.TypeReg {
			if _, err := io.WriteString(tw, e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	switch m := manifest.(type) {
	case nil:
	case string:
		put(tarEntry{name: "manifest.json", body: m})
	default:
		doc, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		put(tarEntry{name: "manifest.json", body: string(doc)})
	}
	for _, e := range entries {
		put(e)
	}
	if err := errors.Join(tw.Close(), gz.Close()); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// manifestFor is the manifest that isshoni would write for the entries: every file's hash, and the contents by
// their top-level names.
func manifestFor(entries ...tarEntry) Manifest {
	m := Manifest{
		Format: 1, IsshoniVersion: "0.3.0", SchemaVersion: latestSchema,
		CreatedAt: api.WireTime(testNow), Contents: []string{}, SHA256: map[string]string{},
	}
	for _, e := range entries {
		top, _, nested := strings.Cut(e.name, "/")
		if nested {
			top += "/"
		}
		if !slices.Contains(m.Contents, top) {
			m.Contents = append(m.Contents, top)
		}
		if e.kind == 0 || e.kind == tar.TypeReg {
			m.SHA256[e.name] = sumOf(e.body)
		}
	}
	return m
}

// goodEntries are the entries of a valid archive: a database of this build's schema, a secrets.json that loads,
// and a certificate.
func goodEntries(t testing.TB) []tarEntry {
	t.Helper()
	src := newTestData(t, fakeDB{Schema: latestSchema, App: "0.3.0", Note: "from the archive"})
	return []tarEntry{
		{name: "isshoni.db", body: src.read(t, "isshoni.db")},
		{name: "secrets.json", body: src.read(t, "secrets.json")},
		{name: "certmagic/", kind: tar.TypeDir},
		{name: "certmagic/certificates/", kind: tar.TypeDir},
		{name: "certmagic/certificates/site.crt", body: "certificate from the archive"},
	}
}

// replace returns entries with the one named name replaced by e, or with e added when there is none.
func replace(entries []tarEntry, name string, e tarEntry) []tarEntry {
	out := slices.Clone(entries)
	for i := range out {
		if out[i].name == name {
			out[i] = e
			return out
		}
	}
	return append(out, e)
}

// wantRefused checks that PrepareRestore refuses body with the code and a message that contains text, and that the
// refusal left nothing behind: the data directory as it was, no staging, no plan, no pre-restore backup.
func wantRefused(t *testing.T, td *testData, body []byte, code, text string) {
	t.Helper()
	before := tree(t, td.dir, nil)
	_, err := td.PrepareRestore(t.Context(), bytes.NewReader(body), int64(len(body)))
	var de *DataError
	if !errors.As(err, &de) || de.Code != code || !api.IsCode(err, code) || !strings.Contains(de.Message, text) {
		t.Errorf("PrepareRestore: %v\nwant %s with %q in its message", err, code, text)
	}
	after := tree(t, td.dir, func(rel string) bool { return rel == "restore" && before["restore"] == "" })
	if !reflect.DeepEqual(after, before) {
		t.Errorf("the refused restore changed the data directory:\n got %v\nwant %v", keys(after), keys(before))
	}
	if left := tree(t, td.Paths.Restore, nil); len(left) != 0 {
		t.Errorf("the refused restore left %v in restore/", keys(left))
	}
}

func keys(m map[string]string) []string { return slices.Sorted(maps.Keys(m)) }

// restore runs both halves of a restore of body on td.
func restore(t *testing.T, td *testData, body []byte) RestoreInfo {
	t.Helper()
	info, err := td.PrepareRestore(t.Context(), bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("PrepareRestore: %v", err)
	}
	if done, err := CompleteRestore(td.Paths, td.Logger); err != nil || !done {
		t.Fatalf("CompleteRestore = %v, %v", done, err)
	}
	return info
}

// planOf reads restore/plan.json.
func planOf(t *testing.T, td *testData) *restorePlan {
	t.Helper()
	plan, err := readPlan(td.Paths)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

// A backup of one server restored on another makes the second hold the first's state (04 §12.4): database,
// secrets and certificates are the archive's, what the data directory held before is in restore/old-<ts>/ and in
// the pre-restore backup, and the next successful start removes the first of the two.
func TestRestoreArchive(t *testing.T) {
	for _, mode := range []string{"running server", "offline"} {
		t.Run(mode, func(t *testing.T) {
			src := newTestData(t, fakeDB{Schema: 6, App: "0.2.9", Note: "alpha"})
			dst := newTestData(t, fakeDB{Schema: latestSchema, App: "0.3.0", Note: "beta"})
			if mode == "running server" {
				dst.online()
			}
			// A crash left the old database a WAL; and beta has a certificate that alpha's archive does not name.
			dst.write(t, "isshoni.db-wal", []byte("beta's uncheckpointed pages"))
			dst.write(t, "isshoni.db-shm", []byte("beta's wal index"))
			dst.write(t, "certmagic/certificates/acme-v02/extra.crt", []byte("only on beta"))
			want := src.serverFiles(t)
			delete(want, "certmagic/locks") // no backup holds the locks
			delete(want, "certmagic/locks/issue_cert_203.0.113.7.lock")
			previous := dst.serverFiles(t)

			archive := archiveOf(t, src, BackupOptions{})
			info, err := dst.PrepareRestore(t.Context(), bytes.NewReader(archive), -1)
			if err != nil {
				t.Fatal(err)
			}
			const ts = "20260929T101500Z"
			if info.Kind != RestoreArchive || info.SchemaVersion != 6 || info.Manifest.IsshoniVersion != version.Version() ||
				info.PreRestoreBackup != "backups/pre-restore-"+ts+".tar.gz" {
				t.Errorf("RestoreInfo = %+v", info)
			}
			// Half one changes nothing of the server's own files: it stages, backs up and plans.
			if now := dst.serverFiles(t); !reflect.DeepEqual(now, previous) {
				t.Errorf("PrepareRestore changed the server's files:\n got %v\nwant %v", keys(now), keys(previous))
			}
			plan := planOf(t, dst)
			wantPlan := &restorePlan{
				TS: ts, State: "swapping",
				Install: []string{"isshoni.db", "secrets.json", "certmagic"}, Evict: []string{"isshoni.db-wal", "isshoni.db-shm"},
			}
			if !reflect.DeepEqual(plan, wantPlan) {
				t.Errorf("plan = %+v, want %+v", plan, wantPlan)
			}
			if raw := dst.read(t, "restore/plan.json"); !strings.HasPrefix(raw, `{"ts":"`+ts+`","state":"swapping",`) {
				t.Errorf("plan.json = %s", raw)
			}
			// The pre-restore backup is a full archive of what the data directory held.
			pre, entries := readArchive(t, []byte(dst.read(t, info.PreRestoreBackup)))
			if pre.SchemaVersion != latestSchema || !slices.Equal(pre.Contents, []string{"isshoni.db", "secrets.json", "certmagic/"}) {
				t.Errorf("the pre-restore backup's manifest: %+v", pre)
			}
			for _, e := range entries {
				if !strings.HasSuffix(e.name, "/") && e.body != previous[e.name] {
					t.Errorf("the pre-restore backup's %s is not the file from before the restore", e.name)
				}
			}

			// Half two, when nothing has the files open any more.
			if done, err := CompleteRestore(dst.Paths, dst.Logger); err != nil || !done {
				t.Fatalf("CompleteRestore = %v, %v", done, err)
			}
			if got := dst.serverFiles(t); !reflect.DeepEqual(got, want) {
				t.Errorf("the data directory after the restore:\n got %v\nwant %v", got, want)
			}
			old := tree(t, filepath.Join(dst.Paths.Restore, "old-"+ts), nil)
			renamed := map[string]string{}
			for name, body := range previous {
				renamed[name] = body
			}
			if !reflect.DeepEqual(old, renamed) {
				t.Errorf("restore/old-%s:\n got %v\nwant what the data directory held: %v", ts, keys(old), keys(renamed))
			}
			if plan := planOf(t, dst); plan == nil || plan.State != "done" {
				t.Errorf("the plan after the swap: %+v, want done", plan)
			}
			if left := keys(tree(t, dst.Paths.Restore, func(rel string) bool { return strings.HasPrefix(rel, "old-"+ts+"/") })); !slices.Equal(left, []string{"old-" + ts, "plan.json"}) {
				t.Errorf("restore/ after the swap: %v, want the old files and the plan", left)
			}
			// A second call has nothing to do.
			if done, err := CompleteRestore(dst.Paths, dst.Logger); err != nil || done {
				t.Errorf("CompleteRestore again = %v, %v; want false", done, err)
			}
			// The start that follows: recovery has nothing to complete, and its success removes the old files.
			if err := RecoverRestore(dst.Paths, dst.Logger); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(dst.Paths.Restore, "old-"+ts)); err != nil {
				t.Errorf("recovery removed the old files before the start succeeded: %v", err)
			}
			if err := FinishRestore(dst.Paths, dst.Logger); err != nil {
				t.Fatal(err)
			}
			if left := tree(t, dst.Paths.Restore, nil); len(left) != 0 {
				t.Errorf("restore/ after the successful start: %v, want nothing", keys(left))
			}
			if _, err := os.Stat(filepath.Join(dst.dir, filepath.FromSlash(info.PreRestoreBackup))); err != nil {
				t.Errorf("the pre-restore backup is gone after the start: %v", err)
			}
			if err := FinishRestore(dst.Paths, dst.Logger); err != nil {
				t.Errorf("FinishRestore without a plan: %v", err)
			}
		})
	}
}

// A bare database file (03's backups/pre-<schema>-<ts>.db) replaces the database alone: secrets.json and the
// certificates stay, and the old database's WAL does not stay next to the new database (04 §12.4).
func TestRestoreDatabaseFile(t *testing.T) {
	td := newTestData(t, fakeDB{Schema: latestSchema + 1, App: "0.4.0", Note: "after the upgrade"})
	td.write(t, "isshoni.db-wal", []byte("the newer database's pages"))
	before := td.serverFiles(t)
	pre := fakeDB{Schema: latestSchema - 1, App: "0.2.9", Note: "before the upgrade"}.bytes()

	info := restore(t, td, pre)
	if info.Kind != RestoreDatabase || info.SchemaVersion != latestSchema-1 || info.PreRestoreBackup == "" {
		t.Errorf("RestoreInfo = %+v", info)
	}
	want := maps.Clone(before)
	want["isshoni.db"] = string(pre)
	delete(want, "isshoni.db-wal")
	if got := td.serverFiles(t); !reflect.DeepEqual(got, want) {
		t.Errorf("after the restore of a database file:\n got %v\nwant only the database replaced: %v", keys(got), keys(want))
	}
	old := tree(t, filepath.Join(td.Paths.Restore, "old-20260929T101500Z"), nil)
	if !reflect.DeepEqual(old, map[string]string{"isshoni.db": before["isshoni.db"], "isshoni.db-wal": before["isshoni.db-wal"]}) {
		t.Errorf("restore/old-…: %v, want the old database with its WAL", keys(old))
	}
	// The pre-restore backup holds the newer database: the upgrade can be gone back to.
	m, _ := readArchive(t, []byte(td.read(t, info.PreRestoreBackup)))
	if m.SchemaVersion != latestSchema+1 {
		t.Errorf("the pre-restore backup's schema = %d, want the newer database's %d", m.SchemaVersion, latestSchema+1)
	}
}

// A raw backup carries the database with its WAL: both are installed, the database first, and the -shm never.
func TestRestoreRawArchive(t *testing.T) {
	good := goodEntries(t)
	entries := []tarEntry{good[0], {name: "isshoni.db-wal", body: "pages"}, {name: "isshoni.db-shm", body: "index"}, good[1]}
	m := manifestFor(entries...)
	m.Raw = true
	td := newTestData(t, fakeDB{Schema: latestSchema, Note: "beta"})
	td.write(t, "isshoni.db-shm", []byte("beta's index"))
	restore(t, td, makeArchive(t, m, entries...))
	got := td.serverFiles(t)
	if got["isshoni.db"] != good[0].body || got["isshoni.db-wal"] != "pages" {
		t.Errorf("after a raw restore: database %q, WAL %q", got["isshoni.db"], got["isshoni.db-wal"])
	}
	if _, has := got["isshoni.db-shm"]; has {
		t.Errorf("a -shm file is in the data directory after the restore: %q", got["isshoni.db-shm"])
	}
	if plan := planOf(t, td); !slices.Equal(plan.Install, []string{"isshoni.db", "isshoni.db-wal", "secrets.json"}) ||
		!slices.Equal(plan.Evict, []string{"isshoni.db-shm"}) {
		t.Errorf("plan = %+v", plan)
	}
	// Without "raw" a WAL is no part of a backup.
	m.Raw = false
	wantRefused(t, newTestData(t, fakeDB{Schema: latestSchema}), makeArchive(t, m, entries...), api.CodeBackupInvalid, `"isshoni.db-wal"`)
}

// An archive without certificates (--no-certs) leaves the certificates that are there.
func TestRestoreKeepsCertificatesWithoutNewOnes(t *testing.T) {
	good := goodEntries(t)[:2]
	td := newTestData(t, fakeDB{Schema: latestSchema, Note: "beta"})
	certs := tree(t, td.Paths.CertMagic, nil)
	restore(t, td, makeArchive(t, manifestFor(good...), good...))
	if got := tree(t, td.Paths.CertMagic, nil); !reflect.DeepEqual(got, certs) {
		t.Errorf("the certificate storage after a restore without certificates: %v, want it untouched", keys(got))
	}
	if td.read(t, "isshoni.db") != good[0].body || td.read(t, "secrets.json") != good[1].body {
		t.Error("the database and secrets.json are not the archive's")
	}
}

// Restore validation (04 §12.4 step 1): what is no backup of ours is refused before anything changes.
func TestRestoreValidation(t *testing.T) {
	good := goodEntries(t)
	goodManifest := manifestFor(good...)
	with := func(change func(*Manifest)) Manifest {
		m := goodManifest
		m.Contents = slices.Clone(m.Contents)
		m.SHA256 = maps.Clone(m.SHA256)
		change(&m)
		return m
	}
	db := func(d fakeDB) []tarEntry {
		return replace(good, "isshoni.db", tarEntry{name: "isshoni.db", body: string(d.bytes())})
	}
	archive := func(entries []tarEntry) []byte { return makeArchive(t, manifestFor(entries...), entries...) }
	// extra is the good archive with one more entry, which the manifest has a hash for and no word about otherwise.
	extra := func(e tarEntry) []byte {
		m := with(func(m *Manifest) {
			if e.kind == 0 {
				m.SHA256[e.name] = sumOf(e.body)
			}
		})
		return makeArchive(t, m, append(slices.Clone(good), e)...)
	}
	whole := archive(good)
	many := slices.Clone(good)
	for i := range restoreMaxEntries {
		many = append(many, tarEntry{name: fmt.Sprintf("certmagic/d%05d/", i), kind: tar.TypeDir})
	}

	invalid, newer := api.CodeBackupInvalid, api.CodeBackupNewer
	tests := []struct {
		name string
		body []byte
		code string
		text string
	}{
		{"empty", nil, invalid, "it is empty"},
		{"neither an archive nor a database", []byte("PK\x03\x04 a zip file"), invalid, "neither a backup archive"},
		{"gzip of something else", gzipOf("just text"), invalid, "it is not a tar archive"},
		{"cut off", whole[:len(whole)/2], invalid, "damaged or cut off"},
		{"a flipped bit", flipBit(t, whole), invalid, ""},
		{"megabytes after the tar's end", padded(t, whole, 2<<20), invalid, "data after the archive's end"},
		{"no manifest first", makeArchive(t, nil, good...), invalid, `its first entry is "isshoni.db", not manifest.json`},
		{"manifest is not JSON", makeArchive(t, "format: 1", good...), invalid, "its manifest is not valid JSON"},
		{"format 0", makeArchive(t, with(func(m *Manifest) { m.Format = 0 }), good...), invalid, "the format 0"},
		{"a newer format", makeArchive(t, with(func(m *Manifest) { m.Format, m.IsshoniVersion = 2, "0.9.0" }), good...), newer,
			"This backup is from isshoni 0.9.0; install 0.9.0 or newer first"},
		{"a name the manifest does not know", makeArchive(t, with(func(m *Manifest) { m.Contents = append(m.Contents, "extra.db") }), good...),
			invalid, `its manifest lists "extra.db"`},
		{"contents listed twice", makeArchive(t, with(func(m *Manifest) { m.Contents = append(m.Contents, "isshoni.db") }), good...),
			invalid, `lists "isshoni.db" twice`},
		{"no database", archive(good[1:]), invalid, "it holds no isshoni.db"},
		{"no secrets", archive(slices.Concat(good[:1], good[2:])), invalid, "it holds no secrets.json"},

		// Names: nothing may leave the staging directory, and nothing may be there that the manifest does not list.
		{"traversal", extra(tarEntry{name: "../evil"}), invalid, `"../evil" has a name that no backup holds`},
		{"traversal below certmagic", extra(tarEntry{name: "certmagic/../../evil"}), invalid, "has a name that no backup holds"},
		{"traversal that stays inside", extra(tarEntry{name: "certmagic/../secrets.json"}), invalid, "has a name that no backup holds"},
		{"a directory that leaves", extra(tarEntry{name: "certmagic/../../evil/", kind: tar.TypeDir}), invalid, "has a name that no backup holds"},
		{"absolute path", extra(tarEntry{name: "/etc/cron.d/evil"}), invalid, "has a name that no backup holds"},
		{"not in its shortest form", extra(tarEntry{name: "certmagic//x"}), invalid, "has a name that no backup holds"},
		{"a dot", extra(tarEntry{name: "./isshoni.db"}), invalid, "has a name that no backup holds"},
		{"a backslash", extra(tarEntry{name: `certmagic\..\x`}), invalid, "has a name that no backup holds"},
		{"a second manifest", extra(tarEntry{name: "manifest.json", body: "{}"}), invalid, "has a name that no backup holds"},
		{"an extra name", extra(tarEntry{name: "extra.txt", body: "x"}), invalid, `it holds "extra.txt", which its manifest does not list`},
		{"certificates the manifest does not list", makeArchive(t, manifestFor(good[:2]...), good...), invalid, `it holds "certmagic", which its manifest does not list`},
		{"a top-level directory", makeArchive(t, goodManifest, append(slices.Clone(good), tarEntry{name: "isshoni.d/", kind: tar.TypeDir})...), invalid, "does not list"},
		{"certmagic as a file", makeArchive(t, goodManifest, good[0], good[1], tarEntry{name: "certmagic", body: "x"}), invalid, "does not list"},
		{"twice the same file", makeArchive(t, goodManifest, append(slices.Clone(good), good[0])...), invalid, `it holds "isshoni.db" twice`},
		{"a listed file is missing", makeArchive(t, goodManifest, good[1:]...), invalid, `it lacks "isshoni.db"`},
		{"a hashed file is missing", makeArchive(t, goodManifest, good[:4]...), invalid, `it lacks "certmagic/certificates/site.crt"`},
		{"too many entries", archive(many), invalid, "more than 10000 entries"},

		// Entries that would collide in the staging directory are the archive's fault, not the disk's.
		{"an entry below a file", archive(append(slices.Clone(good), tarEntry{name: "certmagic/x", body: "x"}, tarEntry{name: "certmagic/x/y", body: "y"})),
			invalid, `it holds "certmagic/x/y" below the file "certmagic/x"`},
		{"an entry far below a file", archive(append(slices.Clone(good), tarEntry{name: "certmagic/x", body: "x"}, tarEntry{name: "certmagic/x/a/b/c", body: "y"})),
			invalid, `it holds "certmagic/x/a/b/c" below the file "certmagic/x"`},
		{"a directory below a file", archive(append(slices.Clone(good), tarEntry{name: "certmagic/x", body: "x"}, tarEntry{name: "certmagic/x/d/", kind: tar.TypeDir})),
			invalid, `it holds "certmagic/x/d" below the file "certmagic/x"`},
		{"a file where a directory is", archive(append(slices.Clone(good), tarEntry{name: "certmagic/certificates", body: "x"})),
			invalid, `it holds "certmagic/certificates" twice`},
		{"a file where an entry's parent directory is", archive([]tarEntry{good[0], good[1], {name: "certmagic/a/b/site.crt", body: "c"}, {name: "certmagic/a/b", body: "x"}}),
			invalid, `it holds "certmagic/a/b" as a file and as a directory`},
		{"a directory where a file is", archive(append(slices.Clone(good), tarEntry{name: "certmagic/certificates/site.crt/", kind: tar.TypeDir})),
			invalid, `it holds "certmagic/certificates/site.crt" twice`},
		{"a directory twice", archive(append(slices.Clone(good), good[3])), invalid, `it holds "certmagic/certificates" twice`},

		// Names that a filesystem refuses are refused before it is asked.
		{"a name that is not UTF-8", archive(append(slices.Clone(good), tarEntry{name: "certmagic/\xff\xfe/", kind: tar.TypeDir})), invalid,
			`its entry "certmagic/\xff\xfe/" has a name that no backup holds`},
		{"a name longer than a filesystem takes", archive(append(slices.Clone(good), tarEntry{name: "certmagic/" + strings.Repeat("n", 256), body: "x"})), invalid,
			"has a name that no backup holds"},
		{"a directory name longer than a filesystem takes", archive(append(slices.Clone(good), tarEntry{name: "certmagic/" + strings.Repeat("n", 256) + "/x/", kind: tar.TypeDir})), invalid,
			"has a name that no backup holds"},
		{"a path longer than the kernel takes", archive(append(slices.Clone(good), tarEntry{name: "certmagic/" + strings.Repeat("d/", 600) + "x", body: "x"})), invalid,
			"has a name that no backup holds"},

		// Types: regular files and directories only.
		{"symlink", archive(append(slices.Clone(good), tarEntry{name: "certmagic/link", kind: tar.TypeSymlink, link: "/etc/passwd"})), invalid,
			`"certmagic/link" is neither a regular file nor a directory`},
		{"a symlink in place of the database", makeArchive(t, goodManifest, replace(good, "isshoni.db", tarEntry{name: "isshoni.db", kind: tar.TypeSymlink, link: "/etc/shadow"})...),
			invalid, "neither a regular file nor a directory"},
		{"hard link", archive(append(slices.Clone(good), tarEntry{name: "certmagic/hard", kind: tar.TypeLink, link: "secrets.json"})), invalid, "neither a regular file nor a directory"},
		{"device", archive(append(slices.Clone(good), tarEntry{name: "certmagic/null", kind: tar.TypeChar})), invalid, "neither a regular file nor a directory"},
		{"fifo", archive(append(slices.Clone(good), tarEntry{name: "certmagic/pipe", kind: tar.TypeFifo})), invalid, "neither a regular file nor a directory"},

		// Hashes.
		{"a bad hash", makeArchive(t, with(func(m *Manifest) { m.SHA256["secrets.json"] = sumOf("something else") }), good...), invalid,
			`the checksum of "secrets.json" does not match`},
		{"a bad hash below certmagic", makeArchive(t, with(func(m *Manifest) { m.SHA256["certmagic/certificates/site.crt"] = sumOf("x") }), good...), invalid,
			`the checksum of "certmagic/certificates/site.crt" does not match`},
		{"no hash", makeArchive(t, with(func(m *Manifest) { delete(m.SHA256, "isshoni.db") }), good...), invalid, `no checksum for "isshoni.db"`},

		// The database is asked itself, whatever the manifest says (which is "schema 7" in all of these).
		{"a newer schema", archive(db(fakeDB{Schema: latestSchema + 2, App: "0.5.0"})), newer, "This backup is from isshoni 0.5.0; install 0.5.0 or newer first"},
		{"a newer schema of an unknown version", archive(db(fakeDB{Schema: latestSchema + 1})), newer, "from a newer isshoni"},
		{"another build's migration history", archive(db(fakeDB{Schema: latestSchema, NoHistory: true})), invalid, "a migration history that this isshoni does not know"},
		{"a failed integrity check", archive(db(fakeDB{Schema: latestSchema, Damage: "row 7 missing from index"})), invalid, "its database is damaged"},
		{"not an isshoni database", archive(db(fakeDB{Schema: 0})), invalid, "its database is not an isshoni database"},
		{"a database that is none", archive(replace(good, "isshoni.db", tarEntry{name: "isshoni.db", body: "plain text"})), invalid, "can't be read as an SQLite database"},
		{"secrets that do not load", archive(replace(good, "secrets.json", tarEntry{name: "secrets.json", body: `{"format": 1, "keys": {"session": {"key": "short"}}}`})),
			invalid, "its secrets.json is not one the server can load"},

		// Bare database files get the database checks alone.
		{"database file: a newer schema", fakeDB{Schema: latestSchema + 1, App: "0.4.0"}.bytes(), newer, "This backup is from isshoni 0.4.0"},
		{"database file: another build's history", fakeDB{Schema: latestSchema, NoHistory: true}.bytes(), invalid, "a migration history"},
		{"database file: a failed integrity check", fakeDB{Schema: 3, Damage: "page 4 is never used"}.bytes(), invalid, "its database is damaged"},
		{"database file: header and garbage", []byte("SQLite format 3\x00 garbage"), invalid, "can't be read as an SQLite database"},
		{"database file: empty database", fakeDB{}.bytes(), invalid, "not an isshoni database"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantRefused(t, newTestData(t, fakeDB{Schema: latestSchema, Note: "beta"}).online(), tt.body, tt.code, tt.text)
		})
	}
	// The archive these were made from is itself a good one.
	restore(t, newTestData(t, fakeDB{Schema: latestSchema, Note: "beta"}), whole)
	// So is one that names a directory after the entries below it, and one whose names are as long as they may be.
	late := []tarEntry{good[0], good[1], {name: "certmagic/a/b/site.crt", body: "c"}, {name: "certmagic/a/b/", kind: tar.TypeDir},
		{name: "certmagic/a/", kind: tar.TypeDir}, {name: "certmagic/", kind: tar.TypeDir}, {name: "certmagic/" + strings.Repeat("n", 255), body: "long"}}
	td := newTestData(t, fakeDB{Schema: latestSchema, Note: "beta"})
	restore(t, td, archive(late))
	if got := tree(t, td.Paths.CertMagic, nil); !reflect.DeepEqual(got, map[string]string{
		"a": "dir", "a/b": "dir", "a/b/site.crt": "c", strings.Repeat("n", 255): "long",
	}) {
		t.Errorf("the certificate storage after a restore with late directory entries: %v", keys(got))
	}
}

// caseInsensitive reports whether dir is on a filesystem that takes "A" and "a" for one name.
func caseInsensitive(t *testing.T, dir string) bool {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "CaseProbe"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(filepath.Join(dir, "CaseProbe")) }()
	_, err := os.Lstat(filepath.Join(dir, "caseprobe"))
	return err == nil
}

// Two names of an archive can be one name on the disk that unpacks it: the default filesystems of macOS and
// Windows fold case. The second entry then finds its name taken, in a directory that only this archive has written
// to: that is the archive's fault (backup_invalid), not a failure of the disk. Where the names are two, as on the
// filesystems a server runs on, the archive is restored.
func TestRestoreNamesThatAreOneOnDisk(t *testing.T) {
	good := goodEntries(t)
	archive := func(extra ...tarEntry) []byte {
		entries := append(slices.Clone(good), extra...)
		return makeArchive(t, manifestFor(entries...), entries...)
	}
	folds := caseInsensitive(t, t.TempDir())
	for name, tt := range map[string]struct {
		body []byte
		text string
	}{
		"two files":                 {archive(tarEntry{name: "certmagic/A", body: "upper"}, tarEntry{name: "certmagic/a", body: "lower"}), `it holds "certmagic/a", which is the name of another of its entries on this filesystem`},
		"a file, then below it":     {archive(tarEntry{name: "certmagic/A", body: "upper"}, tarEntry{name: "certmagic/a/y", body: "lower"}), `it holds "certmagic/a/y", which is the name of another`},
		"a file, then a directory":  {archive(tarEntry{name: "certmagic/A", body: "upper"}, tarEntry{name: "certmagic/a/", kind: tar.TypeDir}), `it holds "certmagic/a", which is the name of another`},
		"a directory, then a file":  {archive(tarEntry{name: "certmagic/CERTIFICATES", body: "upper"}), `it holds "certmagic/CERTIFICATES", which is the name of another`},
		"a file in a folded parent": {archive(tarEntry{name: "certmagic/Certificates/site.crt", body: "again"}), `it holds "certmagic/Certificates/site.crt", which is the name of another`},
	} {
		t.Run(name, func(t *testing.T) {
			td := newTestData(t, fakeDB{Schema: latestSchema, Note: "beta"}).online()
			if folds {
				wantRefused(t, td, tt.body, api.CodeBackupInvalid, tt.text)
				return
			}
			restore(t, td, tt.body)
		})
	}
}

// ustarHeader is one 512-byte tar header, written by hand for the entries that archive/tar refuses to write.
func ustarHeader(name string, typeflag byte, size int) []byte {
	h := make([]byte, 512)
	copy(h[0:100], name)
	copy(h[100:108], "0000600\x00")
	copy(h[108:116], "0000000\x00")
	copy(h[116:124], "0000000\x00")
	copy(h[124:136], fmt.Sprintf("%011o\x00", size))
	copy(h[136:148], "00000000000\x00")
	copy(h[148:156], "        ") // the checksum counts its own field as spaces
	h[156] = typeflag
	copy(h[257:265], "ustar\x0000")
	sum := 0
	for _, b := range h {
		sum += int(b)
	}
	copy(h[148:156], fmt.Sprintf("%06o\x00 ", sum))
	return h
}

// paxRecord is one record of a PAX extended header: "<length> <key>=<value>\n", the length counting itself.
func paxRecord(key, value string) string {
	rest := " " + key + "=" + value + "\n"
	n := len(rest) + 1
	for len(fmt.Sprint(n))+len(rest) != n {
		n = len(fmt.Sprint(n)) + len(rest)
	}
	return fmt.Sprint(n) + rest
}

// block pads data with zeros to a multiple of tar's 512 bytes.
func block(data string) []byte {
	out := []byte(data)
	return append(out, make([]byte, (512-len(out)%512)%512)...)
}

// A sparse file claims a size and holds only a part of it; unpacking would write the rest as zeros. A few hundred
// bytes of archive must not get to ask for gigabytes of disk: such an entry is refused before anything is written.
func TestRestoreRefusesSparseFiles(t *testing.T) {
	good := goodEntries(t)
	// The manifest and the good entries, without the end of the archive.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	m := manifestFor(good...)
	m.SHA256["certmagic/hole"] = sumOf("")
	doc, _ := json.Marshal(m)
	for _, e := range append([]tarEntry{{name: "manifest.json", body: string(doc)}}, good...) {
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: e.name, Mode: 0o600, Size: int64(len(e.body))}
		if e.kind == tar.TypeDir {
			hdr.Typeflag, hdr.Size = tar.TypeDir, 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Flush(); err != nil {
		t.Fatal(err)
	}
	// Then a sparse file in the PAX form (GNU sparse 1.0): 8 GiB, all of it a hole.
	records := paxRecord("GNU.sparse.major", "1") + paxRecord("GNU.sparse.minor", "0") +
		paxRecord("GNU.sparse.name", "certmagic/hole") + paxRecord("GNU.sparse.realsize", "8589934592")
	raw := buf.Bytes()
	raw = append(raw, ustarHeader("PaxHeaders.0/hole", tar.TypeXHeader, len(records))...)
	raw = append(raw, block(records)...)
	raw = append(raw, ustarHeader("GNUSparseFile.0/hole", tar.TypeReg, 512)...)
	raw = append(raw, block("0\n")...) // the sparse map: no data at all
	raw = append(raw, make([]byte, 1024)...)

	// archive/tar reads it as what it claims to be.
	tr := tar.NewReader(bytes.NewReader(raw))
	var last *tar.Header
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("the hand-made archive does not parse: %v", err)
		}
		last = hdr
	}
	if last.Name != "certmagic/hole" || last.Size != 8<<30 {
		t.Fatalf("the hand-made entry reads as %q with %d bytes, want the 8 GiB sparse file", last.Name, last.Size)
	}

	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write(raw)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if gz.Len() > 4<<10 {
		t.Fatalf("the archive has %d bytes; the point is that it is small", gz.Len())
	}
	wantRefused(t, newTestData(t, fakeDB{Schema: latestSchema, Note: "beta"}), gz.Bytes(), api.CodeBackupInvalid, `"certmagic/hole" is a sparse file`)
}

func gzipOf(s string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = io.WriteString(gz, s)
	_ = gz.Close()
	return buf.Bytes()
}

// padded is the archive with n more zero bytes after its tar, inside the gzip stream.
func padded(t *testing.T, archive []byte, n int) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	out := gzip.NewWriter(&buf)
	_, _ = out.Write(plain)
	_, _ = out.Write(make([]byte, n))
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// gunzip unpacks a gzip stream; ok is false when it does not unpack to its end.
func gunzip(data []byte) (plain []byte, ok bool) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, false
	}
	plain, err = io.ReadAll(gz)
	return plain, err == nil
}

// flipBit damages one bit of the compressed stream: the first one from the middle on whose damage reaches the
// content. Not every bit does. A tar is full of runs of zeros, which the compression writes as "the same bytes as
// a little earlier", and a bit that only changes how much earlier unpacks to the very same archive.
func flipBit(t testing.TB, archive []byte) []byte {
	t.Helper()
	want, ok := gunzip(archive)
	if !ok {
		t.Fatal("flipBit: the archive does not unpack")
	}
	for i := len(archive) / 2; i < len(archive); i++ {
		out := bytes.Clone(archive)
		out[i] ^= 0x10
		if got, ok := gunzip(out); !ok || !bytes.Equal(got, want) {
			return out
		}
	}
	t.Fatal("flipBit: no bit in the second half of the archive changes what it unpacks to")
	return nil
}

// The limits on a restore's input: the body's size and what an archive unpacks to.
func TestRestoreLimits(t *testing.T) {
	good := goodEntries(t)
	whole := makeArchive(t, manifestFor(good...), good...)
	defer func(body, unpacked int64) { restoreMaxBytes, restoreMaxUnpackedBytes = body, unpacked }(restoreMaxBytes, restoreMaxUnpackedBytes)

	restoreMaxBytes = int64(len(whole)) - 1
	td := newTestData(t, fakeDB{Schema: latestSchema, Note: "beta"})
	wantRefused(t, td, whole, api.CodePayloadTooLarge, "larger than 2 GiB")
	// The same when the caller does not know the size up front.
	if _, err := td.PrepareRestore(t.Context(), bytes.NewReader(whole), -1); !api.IsCode(err, api.CodePayloadTooLarge) {
		t.Errorf("a body over the limit of unknown size: %v", err)
	}
	if left := tree(t, td.Paths.Restore, nil); len(left) != 0 {
		t.Errorf("restore/ after a body over the limit: %v", keys(left))
	}

	restoreMaxBytes = int64(len(whole))
	restoreMaxUnpackedBytes = int64(len(good[0].body) + len(good[1].body))
	wantRefused(t, td, whole, api.CodeBackupInvalid, "it unpacks to more than")
	restoreMaxUnpackedBytes += int64(len(good[4].body))
	restore(t, td, whole)
}

// One restore at a time: while a plan waits for its swap, another restore is refused; and a restore that is done
// but not yet followed by a start makes room for the next one.
func TestRestoreInProgress(t *testing.T) {
	td := newTestData(t, fakeDB{Schema: latestSchema, Note: "beta"})
	first := fakeDB{Schema: latestSchema, Note: "first"}.bytes()
	second := fakeDB{Schema: latestSchema, Note: "second"}.bytes()
	if _, err := td.PrepareRestore(t.Context(), bytes.NewReader(first), -1); err != nil {
		t.Fatal(err)
	}
	staged := tree(t, td.Paths.Restore, nil)
	_, err := td.PrepareRestore(t.Context(), bytes.NewReader(second), -1)
	if !api.IsCode(err, api.CodeRestoreInProgress) {
		t.Fatalf("a second restore while the first waits: %v, want restore_in_progress", err)
	}
	if now := tree(t, td.Paths.Restore, nil); !reflect.DeepEqual(now, staged) {
		t.Errorf("the refused restore touched the first one's staging: %v", keys(now))
	}
	if _, err := CompleteRestore(td.Paths, td.Logger); err != nil {
		t.Fatal(err)
	}
	// Done, and no start since: the next restore takes over, with a timestamp of its own.
	info := restore(t, td, second)
	if info.PreRestoreBackup != "backups/pre-restore-20260929T101501Z.tar.gz" {
		t.Errorf("the second restore's pre-restore backup = %q, want the next second's name", info.PreRestoreBackup)
	}
	if td.read(t, "isshoni.db") != string(second) {
		t.Error("the database is not the second restore's")
	}
	if left := keys(tree(t, td.Paths.Restore, func(rel string) bool { return strings.Contains(rel, "/") })); !slices.Equal(left, []string{"old-20260929T101501Z", "plan.json"}) {
		t.Errorf("restore/ = %v, want only the second restore's old files and plan", left)
	}
	// The first restore's pre-restore backup holds the state from before both.
	m, entries := readArchive(t, []byte(td.read(t, "backups/pre-restore-20260929T101500Z.tar.gz")))
	if d, _ := readFakeDBBytes(entries[0].body); m.SchemaVersion != latestSchema || d.Note != "beta" {
		t.Errorf("the first pre-restore backup holds %+v", d)
	}
}

// A restore that is accepted while the restore before it is being finished keeps its plan. A server finishes a
// restore when it has started (FinishRestore removes restore/old-<ts>/ and then the plan), and its admin socket may
// get the next restore at that moment: `isshoni admin restore` returns as soon as the restarted server answers, so
// a script's next command is right there. The plan that FinishRestore removes must be the one it read, never the
// one that PrepareRestore has written since: the request was answered 202, and without its plan nothing would be
// restored.
func TestRestoreWhileTheLastOneIsFinished(t *testing.T) {
	second := fakeDB{Schema: latestSchema, Note: "second"}.bytes()
	// done leaves a data directory as the shutdown of a restore leaves it: the plan is "done", and the files from
	// before are in restore/old-<ts>/.
	done := func(t *testing.T) *testData {
		td := newTestData(t, fakeDB{Schema: latestSchema, Note: "beta"}).online()
		restore(t, td, fakeDB{Schema: latestSchema, Note: "first"}.bytes())
		return td
	}
	// check is what must hold once both have returned: the accepted restore has its plan, and its swap happens.
	check := func(t *testing.T, td *testData, finishErr, prepareErr error) {
		t.Helper()
		if finishErr != nil || prepareErr != nil {
			t.Fatalf("FinishRestore: %v; PrepareRestore: %v", finishErr, prepareErr)
		}
		const ts = "20260929T101501Z" // the second restore's: one second after the first
		if plan := planOf(t, td); plan == nil || plan.State != planSwapping || plan.TS != ts {
			t.Fatalf("the plan after PrepareRestore returned nil: %+v, want the accepted restore's (swapping, %s)\n%s", plan, ts, td.logs.String())
		}
		if done, err := CompleteRestore(td.Paths, td.Logger); err != nil || !done {
			t.Fatalf("CompleteRestore = %v, %v", done, err)
		}
		if got := td.read(t, "isshoni.db"); got != string(second) {
			t.Errorf("the database after the swap is not the second restore's: %q", got)
		}
		if left := keys(tree(t, td.Paths.Restore, func(rel string) bool { return strings.Contains(rel, "/") })); !slices.Equal(left, []string{"old-" + ts, "plan.json"}) {
			t.Errorf("restore/ = %v, want only the second restore's old files and plan", left)
		}
	}

	// FinishRestore has read the done plan and is still removing the old files when the next restore arrives.
	t.Run("in the middle of the cleanup", func(t *testing.T) {
		td := done(t)
		removing, goOn := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		removeTree = func(path string) error {
			if calls.Add(1) == 1 {
				close(removing)
				<-goOn
			}
			return os.RemoveAll(path)
		}
		defer func() { removeTree = os.RemoveAll }()
		finished, prepared := make(chan error, 1), make(chan error, 1)
		go func() { finished <- FinishRestore(td.Paths, td.Logger) }()
		<-removing
		go func() {
			_, err := td.PrepareRestore(t.Context(), bytes.NewReader(second), -1)
			prepared <- err
		}()
		// The restore has to wait for the cleanup. Were it to go ahead, it would be through long before this time is
		// up, and the cleanup would then remove its plan.
		var prepareErr error
		waiting := true
		select {
		case prepareErr = <-prepared:
			waiting = false
		case <-time.After(300 * time.Millisecond):
		}
		close(goOn)
		finishErr := <-finished
		if waiting {
			prepareErr = <-prepared
		}
		check(t, td, finishErr, prepareErr)
		if n := strings.Count(td.logs.String(), "restore finished: removed the files from before the restore"); n != 1 {
			t.Errorf("the first restore was finished %d times, want once", n)
		}
	})

	// The same without a hand in it, many times over: whichever of the two gets there first.
	t.Run("side by side", func(t *testing.T) {
		rounds := 10
		if testing.Short() {
			rounds = 3
		}
		for range rounds {
			td := done(t)
			finished, prepared := make(chan error, 1), make(chan error, 1)
			go func() { finished <- FinishRestore(td.Paths, td.Logger) }()
			go func() {
				_, err := td.PrepareRestore(t.Context(), bytes.NewReader(second), -1)
				prepared <- err
			}()
			check(t, td, <-finished, <-prepared)
		}
	})
}

func readFakeDBBytes(s string) (fakeDB, error) {
	var d fakeDB
	doc, _ := strings.CutPrefix(s, "SQLite format 3\x00")
	return d, json.Unmarshal([]byte(doc), &d)
}

// backups/ keeps the newest five pre-restore backups, and nothing of 03's (04 §5.1).
func TestPreRestoreRotation(t *testing.T) {
	td := newTestData(t, fakeDB{Schema: latestSchema, Note: "beta"})
	for _, name := range []string{"pre-6-20260101T000000Z.db", "pre-restore-notes.txt", "pre-restore-20250101T000000Z.tar.gz.part"} {
		td.write(t, "backups/"+name, []byte("not a pre-restore backup"))
	}
	at := testNow
	td.Now = func() time.Time { return at }
	for i := range 7 {
		at = testNow.Add(time.Duration(i) * time.Hour)
		restore(t, td, fakeDB{Schema: latestSchema, Note: fmt.Sprint("restore ", i)}.bytes())
	}
	got := keys(tree(t, td.Paths.Backups, nil))
	want := []string{
		"pre-6-20260101T000000Z.db",
		"pre-restore-20250101T000000Z.tar.gz.part",
		"pre-restore-20260929T121500Z.tar.gz", "pre-restore-20260929T131500Z.tar.gz", "pre-restore-20260929T141500Z.tar.gz",
		"pre-restore-20260929T151500Z.tar.gz", "pre-restore-20260929T161500Z.tar.gz",
		"pre-restore-notes.txt",
	}
	if !slices.Equal(got, want) {
		t.Errorf("backups/ after seven restores:\n got %v\nwant %v", got, want)
	}
}

// Restoring into a data directory that has nothing yet needs no pre-restore backup.
func TestRestoreIntoEmptyDataDir(t *testing.T) {
	good := goodEntries(t)
	td := newTestData(t, fakeDB{})
	for _, name := range []string{"isshoni.db", "secrets.json", "certmagic"} {
		if err := os.RemoveAll(filepath.Join(td.dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	info := restore(t, td, makeArchive(t, manifestFor(good...), good...))
	if info.PreRestoreBackup != "" {
		t.Errorf("PreRestoreBackup = %q for an empty data directory", info.PreRestoreBackup)
	}
	if _, err := os.Stat(td.Paths.Backups); err == nil {
		t.Error("backups/ was created for nothing")
	}
	got := td.serverFiles(t)
	if got["isshoni.db"] != good[0].body || got["secrets.json"] != good[1].body || got["certmagic/certificates/site.crt"] != good[4].body {
		t.Errorf("the data directory after the restore: %v", keys(got))
	}
}

// swapState is what the swap of a restore must end in, whatever interrupted it.
type swapState struct {
	data map[string]string // the server's files
	old  map[string]string // restore/old-<ts>
}

// Crash safety (04 §12.4): a swap that stops after any number of its moves is completed by the recovery of the
// next start, to the same end as a swap that ran through. And at no point in between does a WAL sit next to a
// database it does not belong to.
func TestRestoreSwapRecovery(t *testing.T) {
	good := goodEntries(t)
	rawEntries := []tarEntry{good[0], {name: "isshoni.db-wal", body: "the archive's pages"}, good[1]}
	rawManifest := manifestFor(rawEntries...)
	rawManifest.Raw = true
	for name, body := range map[string][]byte{
		"archive":       makeArchive(t, manifestFor(good...), good...),
		"database file": fakeDB{Schema: latestSchema - 1, Note: "from the file"}.bytes(),
		"raw archive":   makeArchive(t, rawManifest, rawEntries...),
	} {
		t.Run(name, func(t *testing.T) {
			setup := func() *testData {
				td := newTestData(t, fakeDB{Schema: latestSchema, Note: "beta"})
				td.write(t, "isshoni.db-wal", []byte("beta's pages"))
				td.write(t, "isshoni.db-shm", []byte("beta's index"))
				if _, err := td.PrepareRestore(t.Context(), bytes.NewReader(body), -1); err != nil {
					t.Fatal(err)
				}
				return td
			}
			// Every data directory of this test has keys of its own; they read as "its own keys", so that the end of
			// one swap can be compared with the end of another.
			state := func(td *testData) swapState {
				s := swapState{data: td.serverFiles(t), old: tree(t, filepath.Join(td.Paths.Restore, "old-20260929T101500Z"), nil)}
				for _, files := range []map[string]string{s.data, s.old} {
					if files["secrets.json"] == td.ownSecrets {
						files["secrets.json"] = "its own keys"
					}
				}
				return s
			}
			defer func() { renameFile = os.Rename }()

			// The swap that nothing interrupts, with a count of its moves.
			moves := 0
			renameFile = func(from, to string) error { moves++; return os.Rename(from, to) }
			td := setup()
			oldDB, oldWAL := td.read(t, "isshoni.db"), td.read(t, "isshoni.db-wal")
			if done, err := CompleteRestore(td.Paths, td.Logger); err != nil || !done {
				t.Fatalf("CompleteRestore = %v, %v", done, err)
			}
			want := state(td)
			if moves < 3 || want.data["isshoni.db"] == oldDB || want.old["isshoni.db"] != oldDB || want.old["isshoni.db-wal"] != oldWAL {
				t.Fatalf("the uninterrupted swap: %d moves, data %v, old %v", moves, keys(want.data), keys(want.old))
			}

			for stop := range moves {
				td := setup()
				crash := errors.New("power cut")
				n := 0
				renameFile = func(from, to string) error {
					if n == stop {
						return crash
					}
					n++
					return os.Rename(from, to)
				}
				if done, err := CompleteRestore(td.Paths, td.Logger); !errors.Is(err, crash) || done {
					t.Fatalf("stop after %d moves: CompleteRestore = %v, %v; want the crash", stop, done, err)
				}
				// The state a crash leaves: the plan still says "swapping", and the database and the WAL in the data
				// directory, if both are there, belong together.
				if plan := planOf(t, td); plan == nil || plan.State != "swapping" {
					t.Fatalf("stop after %d moves: plan %+v, want swapping", stop, plan)
				}
				mid := td.serverFiles(t)
				dbIsOld, walIsOld := mid["isshoni.db"] == oldDB, mid["isshoni.db-wal"] == oldWAL
				if _, hasDB := mid["isshoni.db"]; hasDB {
					if _, hasWAL := mid["isshoni.db-wal"]; hasWAL && dbIsOld != walIsOld {
						t.Errorf("stop after %d moves: a database and a WAL that do not belong together are side by side", stop)
					}
				}
				// The next start.
				renameFile = os.Rename
				if err := RecoverRestore(td.Paths, td.Logger); err != nil {
					t.Fatalf("stop after %d moves: RecoverRestore: %v", stop, err)
				}
				if got := state(td); !reflect.DeepEqual(got, want) {
					t.Errorf("stop after %d moves, then recovery:\n got data %v old %v\nwant data %v old %v",
						stop, keys(got.data), keys(got.old), keys(want.data), keys(want.old))
				}
				if plan := planOf(t, td); plan == nil || plan.State != "done" {
					t.Errorf("stop after %d moves: the plan after recovery is %+v, want done", stop, plan)
				}
				if !strings.Contains(td.logs.String(), "a restore was interrupted before its files were swapped in; completing it now") {
					t.Errorf("stop after %d moves: the log does not say that a restore was completed:\n%s", stop, td.logs.String())
				}
				// Recovering again changes nothing.
				if err := RecoverRestore(td.Paths, td.Logger); err != nil || !reflect.DeepEqual(state(td), want) {
					t.Errorf("stop after %d moves: a second recovery: %v", stop, err)
				}
			}
		})
	}
}

// What recovery does in each state of restore/ (04 §12.4): nothing without the directory; staging without a plan
// is removed with a log line; a plan that is "done" waits for the successful start; a plan that can't be read
// stops the start.
func TestRecoverRestoreStates(t *testing.T) {
	t.Run("no restore directory", func(t *testing.T) {
		td := newTestData(t, fakeDB{Schema: latestSchema})
		before := tree(t, td.dir, nil)
		if err := errors.Join(RecoverRestore(td.Paths, td.Logger), FinishRestore(td.Paths, td.Logger)); err != nil {
			t.Fatal(err)
		}
		if done, err := CompleteRestore(td.Paths, td.Logger); err != nil || done {
			t.Errorf("CompleteRestore = %v, %v", done, err)
		}
		if after := tree(t, td.dir, nil); !reflect.DeepEqual(after, before) {
			t.Errorf("recovery without anything to recover changed the data directory: %v", keys(after))
		}
	})
	t.Run("staging without a plan", func(t *testing.T) {
		td := newTestData(t, fakeDB{Schema: latestSchema})
		before := td.serverFiles(t)
		td.write(t, "restore/incoming", []byte("half a body"))
		td.write(t, "restore/new/isshoni.db", fakeDB{Schema: 1}.bytes())
		td.write(t, "restore/new/certmagic/x.crt", []byte("x"))
		td.write(t, "restore/snapshot-0a1b2c3d.db", []byte("a backup's snapshot"))
		td.write(t, "restore/plan.json.tmp-0a1b2c3d", []byte(`{"ts":`))
		td.write(t, "restore/notes.txt", []byte("the operator's"))
		if err := RecoverRestore(td.Paths, td.Logger); err != nil {
			t.Fatal(err)
		}
		if left := keys(tree(t, td.Paths.Restore, nil)); !slices.Equal(left, []string{"notes.txt"}) {
			t.Errorf("restore/ after recovery: %v, want only what is not a restore's", left)
		}
		if after := td.serverFiles(t); !reflect.DeepEqual(after, before) {
			t.Error("recovery of staging without a plan changed the server's files")
		}
		logs := td.logs.String()
		if !strings.Contains(logs, "removed what an unfinished restore or backup left in the staging directory") ||
			!strings.Contains(logs, `"removed":["incoming","new","snapshot-0a1b2c3d.db","plan.json.tmp-0a1b2c3d"]`) {
			t.Errorf("the log line about the removed staging:\n%s", logs)
		}
	})
	t.Run("a plan that is done", func(t *testing.T) {
		td := newTestData(t, fakeDB{Schema: latestSchema, Note: "beta"})
		restore(t, td, fakeDB{Schema: latestSchema, Note: "restored"}.bytes())
		td.write(t, "restore/new/isshoni.db", []byte("staging that the crash kept from being removed"))
		after := td.serverFiles(t)
		if err := RecoverRestore(td.Paths, td.Logger); err != nil {
			t.Fatal(err)
		}
		if left := keys(tree(t, td.Paths.Restore, func(rel string) bool { return strings.Contains(rel, "/") })); !slices.Equal(left, []string{"old-20260929T101500Z", "plan.json"}) {
			t.Errorf("restore/ after recovery of a done plan: %v", left)
		}
		if now := td.serverFiles(t); !reflect.DeepEqual(now, after) {
			t.Error("recovery of a done plan changed the server's files")
		}
	})
	t.Run("a plan that can't be read", func(t *testing.T) {
		for name, plan := range map[string]string{
			"not JSON":        `{"ts":"20260929T101500Z","state":`,
			"an unknown name": `{"ts":"20260929T101500Z","state":"swapping","install":["../../etc/passwd"],"evict":[]}`,
			"a bad timestamp": `{"ts":"../../x","state":"swapping","install":["isshoni.db"],"evict":[]}`,
			"a bad state":     `{"ts":"20260929T101500Z","state":"rolling-back","install":["isshoni.db"],"evict":[]}`,
		} {
			td := newTestData(t, fakeDB{Schema: latestSchema})
			td.write(t, "restore/plan.json", []byte(plan))
			td.write(t, "restore/new/isshoni.db", fakeDB{Schema: 1}.bytes())
			before := tree(t, td.dir, nil)
			err := RecoverRestore(td.Paths, td.Logger)
			if err == nil || !strings.Contains(err.Error(), "plan.json is the record of a restore, and it can't be used") {
				t.Errorf("%s: RecoverRestore: %v", name, err)
			}
			// No restart makes the file readable: it is the operator's, and the text is the whole message.
			if !errors.Is(err, ErrRestoreNeedsOperator) || strings.Contains(err.Error(), ErrRestoreNeedsOperator.Error()) ||
				!strings.HasSuffix(err.Error(), "then remove "+filepath.Join(td.Paths.Restore, "plan.json")) {
				t.Errorf("%s: RecoverRestore: %v, want an error for the operator that ends with what to do", name, err)
			}
			if _, err := CompleteRestore(td.Paths, td.Logger); !errors.Is(err, ErrRestoreNeedsOperator) {
				t.Errorf("%s: CompleteRestore: %v, want it to stop for the operator", name, err)
			}
			if _, err := td.PrepareRestore(t.Context(), bytes.NewReader(fakeDB{Schema: 1}.bytes()), -1); err == nil {
				t.Errorf("%s: PrepareRestore went on", name)
			}
			if err := FinishRestore(td.Paths, td.Logger); !errors.Is(err, ErrRestoreNeedsOperator) {
				t.Errorf("%s: FinishRestore: %v, want it to stop for the operator", name, err)
			}
			if after := tree(t, td.dir, nil); !reflect.DeepEqual(after, before) {
				t.Errorf("%s: a plan that can't be read led to changes: %v", name, keys(after))
			}
		}
	})
	t.Run("both the old and the current file where one should be", func(t *testing.T) {
		td := newTestData(t, fakeDB{Schema: latestSchema, Note: "beta"})
		if _, err := td.PrepareRestore(t.Context(), bytes.NewReader(fakeDB{Schema: latestSchema, Note: "restored"}.bytes()), -1); err != nil {
			t.Fatal(err)
		}
		td.write(t, "restore/old-20260929T101500Z/isshoni.db", []byte("someone put a file here"))
		err := RecoverRestore(td.Paths, td.Logger)
		if err == nil || !strings.Contains(err.Error(), "both") {
			t.Errorf("RecoverRestore: %v, want it to stop rather than overwrite", err)
		}
		// The next start would find the same two files: only the operator can say which one counts.
		if !errors.Is(err, ErrRestoreNeedsOperator) || !strings.HasSuffix(err.Error(), "move the other away, and start again") {
			t.Errorf("RecoverRestore: %v, want an error for the operator that ends with what to do", err)
		}
		if _, err := CompleteRestore(td.Paths, td.Logger); !errors.Is(err, ErrRestoreNeedsOperator) {
			t.Errorf("CompleteRestore: %v, want the same refusal", err)
		}
		if d, _ := readFakeDB(td.Paths.DB); d.Note != "beta" {
			t.Errorf("the database after the refused swap: %+v", d)
		}
	})
	t.Run("a swap that fails is not the operator's yet", func(t *testing.T) {
		td := newTestData(t, fakeDB{Schema: latestSchema, Note: "beta"})
		if _, err := td.PrepareRestore(t.Context(), bytes.NewReader(fakeDB{Schema: latestSchema, Note: "restored"}.bytes()), -1); err != nil {
			t.Fatal(err)
		}
		busy := errors.New("resource busy")
		renameFile = func(string, string) error { return busy }
		defer func() { renameFile = os.Rename }()
		// A file that can't be moved now may move at the next start: a restart is the right answer to that.
		if err := RecoverRestore(td.Paths, td.Logger); !errors.Is(err, busy) || errors.Is(err, ErrRestoreNeedsOperator) {
			t.Errorf("RecoverRestore: %v, want the rename's error and no refusal", err)
		}
		renameFile = os.Rename
		if err := RecoverRestore(td.Paths, td.Logger); err != nil {
			t.Errorf("RecoverRestore at the next start: %v", err)
		}
		if d, _ := readFakeDB(td.Paths.DB); d.Note != "restored" {
			t.Errorf("the database after the recovered swap: %+v", d)
		}
	})
}

// KindOf tells the two inputs apart by their first bytes.
func TestKindOf(t *testing.T) {
	for in, want := range map[string]RestoreKind{
		"SQLite format 3\x00\x10\x00": RestoreDatabase,
		"\x1f\x8b\x08\x00":            RestoreArchive,
		"\x1f\x8b":                    RestoreArchive,
	} {
		if got, err := KindOf([]byte(in)); err != nil || got != want {
			t.Errorf("KindOf(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "SQLite format 3", "SQLite format 4\x00", "\x1f", "PK\x03\x04", "{}"} {
		if _, err := KindOf([]byte(in)); !api.IsCode(err, api.CodeBackupInvalid) {
			t.Errorf("KindOf(%q): %v, want backup_invalid", in, err)
		}
	}
}
