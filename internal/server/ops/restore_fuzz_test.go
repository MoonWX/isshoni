package ops

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The body of a restore is the one input of this package that is a file from somewhere else (04 §12.4). Whatever
// it holds, PrepareRestore must not panic and must keep its promises:
//
//   - a refusal is a *DataError, and it leaves nothing behind: the server's files as they were, no staging, no
//     plan, no pre-restore backup;
//   - an accepted body has staged nothing but files of the format, all of them inside restore/new, and the swap
//     that follows puts nothing but those into the data directory.
//
// Nothing is ever written outside the data directory: every test data directory is alone in its parent, which the
// check looks at too.

// fuzzRestore runs one body through a restore and checks the promises above.
func fuzzRestore(t *testing.T, body []byte) {
	td := newTestData(t, fakeDB{Schema: latestSchema, Note: "beta"})
	parent := filepath.Dir(td.dir)
	before := td.serverFiles(t)

	_, err := td.PrepareRestore(t.Context(), bytes.NewReader(body), int64(len(body)))
	if now := td.serverFiles(t); !reflect.DeepEqual(now, before) {
		t.Fatalf("PrepareRestore changed the server's files: %v, before %v", keys(now), keys(before))
	}
	if outside := keys(tree(t, parent, func(rel string) bool { return rel == "data" })); len(outside) != 0 {
		t.Fatalf("PrepareRestore wrote outside the data directory: %v", outside)
	}
	if err != nil {
		var de *DataError
		if !errors.As(err, &de) || de.Message == "" {
			t.Fatalf("a refusal that is no DataError with a message: %v", err)
		}
		if left := tree(t, td.Paths.Restore, nil); len(left) != 0 {
			t.Fatalf("the refused restore left %v in restore/", keys(left))
		}
		if _, err := os.Stat(td.Paths.Backups); err == nil {
			t.Fatal("the refused restore left a pre-restore backup")
		}
		return
	}

	allowed := []string{"isshoni.db", "isshoni.db-wal", "isshoni.db-shm", "secrets.json", "certmagic"}
	staged := filepath.Join(td.Paths.Restore, "new")
	err = filepath.WalkDir(staged, func(path string, e fs.DirEntry, err error) error {
		if err != nil || path == staged {
			return err
		}
		rel, _ := filepath.Rel(staged, path)
		if top, _, _ := strings.Cut(filepath.ToSlash(rel), "/"); !slices.Contains(allowed, top) || (!e.IsDir() && !e.Type().IsRegular()) {
			t.Fatalf("an accepted restore staged %q (%v)", rel, e.Type())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := readPlan(td.Paths)
	if err != nil || plan == nil || plan.State != planSwapping {
		t.Fatalf("the plan of an accepted restore: %+v, %v", plan, err)
	}
	if done, err := CompleteRestore(td.Paths, td.Logger); err != nil || !done {
		t.Fatalf("CompleteRestore of an accepted restore = %v, %v", done, err)
	}
	for name := range td.serverFiles(t) {
		if top, _, _ := strings.Cut(name, "/"); !slices.Contains(allowed, top) {
			t.Fatalf("after the swap the data directory holds %q", name)
		}
	}
	if outside := keys(tree(t, parent, func(rel string) bool { return rel == "data" })); len(outside) != 0 {
		t.Fatalf("the swap wrote outside the data directory: %v", outside)
	}
	// The database that was accepted is one that this build may open.
	if d, err := readFakeDB(td.Paths.DB); err != nil || d.Schema < 1 || d.Schema > latestSchema || d.NoHistory || d.Damage != "" {
		t.Fatalf("the restored database: %+v, %v", d, err)
	}
}

// restoreSeeds are bodies of every kind that TestRestoreValidation knows: good ones, and one of each refusal.
func restoreSeeds(t testing.TB) [][]byte {
	good := goodEntries(t)
	m := manifestFor(good...)
	raw := []tarEntry{good[0], {name: "isshoni.db-wal", body: "pages"}, good[1]}
	rawManifest := manifestFor(raw...)
	rawManifest.Raw = true
	with := func(extra ...tarEntry) []byte { return makeArchive(t, m, append(slices.Clone(good), extra...)...) }
	return [][]byte{
		makeArchive(t, m, good...),
		makeArchive(t, rawManifest, raw...),
		makeArchive(t, manifestFor(good[:2]...), good[:2]...),
		fakeDB{Schema: latestSchema - 1, App: "0.2.9"}.bytes(),
		fakeDB{Schema: latestSchema + 1, App: "0.5.0"}.bytes(),
		[]byte("SQLite format 3\x00 garbage"),
		nil,
		[]byte("plain text"),
		gzipOf("not a tar"),
		makeArchive(t, nil, good...),
		makeArchive(t, "{", good...),
		with(tarEntry{name: "../evil", body: "x"}),
		with(tarEntry{name: "/etc/evil", body: "x"}),
		with(tarEntry{name: "certmagic/../../evil", body: "x"}),
		with(tarEntry{name: "certmagic/link", kind: tar.TypeSymlink, link: "/etc/passwd"}),
		with(tarEntry{name: "certmagic/hard", kind: tar.TypeLink, link: "secrets.json"}),
		with(tarEntry{name: "extra.txt", body: "x"}),
		with(good[0]),
		makeArchive(t, m, good[1:]...),
	}
}

// FuzzRestoreBody feeds arbitrary bytes to a restore as its body.
func FuzzRestoreBody(f *testing.F) {
	for _, seed := range restoreSeeds(f) {
		f.Add(seed)
	}
	f.Fuzz(fuzzRestore)
}

// FuzzRestoreTar feeds an arbitrary tar stream to a restore, inside a gzip stream that is always whole: the
// compression's own checksum would otherwise keep the fuzzer away from everything behind it, the names and types
// of the entries above all.
func FuzzRestoreTar(f *testing.F) {
	for _, seed := range restoreSeeds(f) {
		gz, err := gzip.NewReader(bytes.NewReader(seed))
		if err != nil {
			continue // a database file, or no gzip at all
		}
		plain, err := io.ReadAll(gz)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(plain)
	}
	f.Fuzz(func(t *testing.T, plain []byte) {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		_, _ = gz.Write(plain)
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		fuzzRestore(t, buf.Bytes())
	})
}
