//go:build !windows

package credstore

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func openFile(t *testing.T, o Options) *fileStore {
	t.Helper()
	if o.Logger == nil {
		o.Logger = discardLogger()
	}
	s, err := Open(o)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s.(*fileStore)
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Base(path), err)
	}
	return data
}

func writeFile(t *testing.T, path string, data []byte, mode fs.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatalf("write %s: %v", filepath.Base(path), err)
	}
	if err := os.Chmod(path, mode); err != nil { // past the umask
		t.Fatal(err)
	}
}

func assertMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("%s has mode %#o, want %#o", filepath.Base(path), got, want)
	}
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestFileStore(t *testing.T) {
	testStoreContract(t, func(t *testing.T) Store { return openFile(t, Options{Dir: t.TempDir()}) })
}

func TestFileStoreFormat(t *testing.T) {
	dir := t.TempDir()
	s := openFile(t, Options{Dir: dir})
	mustPut(t, s, originA, sample("isr_test-format"))
	const want = `{
  "version": 1,
  "credentials": {
    "isshoni:v1:https://watch.example.com": {
      "v": 1,
      "refreshToken": "isr_test-format",
      "deviceId": "m4q8x2c6v0bn",
      "savedAt": "2026-11-02T19:05:01.004Z",
      "userName": "Alex"
    }
  }
}
`
	if got := string(readFile(t, filepath.Join(dir, credentialsFile))); got != want {
		t.Errorf("credentials.json:\n%s\nwant:\n%s", got, want)
	}
}

func TestFileStoreModes(t *testing.T) {
	t.Run("new directory", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "a", "isshoni-dev")
		s := openFile(t, Options{Dir: dir})
		if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Open created the directory (err = %v); only a write should", err)
		}
		mustPut(t, s, originA, sample("isr_test-mode"))
		assertMode(t, dir, 0o700)
		assertMode(t, filepath.Join(dir, credentialsFile), 0o600)
	})
	t.Run("existing file with a wider mode", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, credentialsFile)
		writeFile(t, path, []byte(`{"version":1,"credentials":{}}`), 0o644)
		s := openFile(t, Options{Dir: dir})
		mustPut(t, s, originA, sample("isr_test-mode"))
		assertMode(t, path, 0o600)
		if err := s.Delete(originA); err != nil {
			t.Fatal(err)
		}
		assertMode(t, path, 0o600)
	})
}

func TestFileStoreDefaultDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	base, err := os.UserConfigDir()
	if err != nil {
		t.Skipf("no user config directory on this OS: %v", err)
	}
	if !strings.HasPrefix(base, home) {
		t.Fatalf("os.UserConfigDir() is outside the test's HOME")
	}
	s := openFile(t, Options{})
	mustPut(t, s, originA, sample("isr_test-default"))
	path := filepath.Join(base, "isshoni-dev", credentialsFile)
	if s.path != path {
		t.Errorf("the store's file is not <UserConfigDir>/isshoni-dev/credentials.json")
	}
	assertMode(t, path, 0o600)
}

func TestFileStoreWarnsAtOpen(t *testing.T) {
	log, buf := newLogBuffer()
	dir := t.TempDir()
	openFile(t, Options{Dir: dir, Logger: log})
	recs := buf.records(t, "WARN", "plaintext credential store, development only")
	if len(recs) != 1 {
		t.Fatalf("got %d plaintext WARNs, want 1: %s", len(recs), buf.String())
	}
	if recs[0]["component"] != "credstore" {
		t.Errorf("component = %v, want credstore", recs[0]["component"])
	}
	if strings.Contains(buf.String(), dir) {
		t.Errorf("the WARN names the directory: %s", buf.String())
	}
}

func TestFileStoreAtomicReplace(t *testing.T) {
	failures := map[string]func(s *fileStore){
		"write fails halfway": func(s *fileStore) {
			s.writeTemp = func(f *os.File, data []byte) error {
				if _, err := f.Write(data[:len(data)/2]); err != nil {
					return err
				}
				return errors.New("disk full")
			}
		},
		"rename fails": func(s *fileStore) {
			s.rename = func(string, string) error { return errors.New("rename refused") }
		},
	}
	for name, inject := range failures {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, credentialsFile)
			s := openFile(t, Options{Dir: dir})
			mustPut(t, s, originA, sample("isr_test-old"))
			before := readFile(t, path)

			inject(s)
			if err := s.Put(originA, sample("isr_test-new")); err == nil {
				t.Fatal("Put succeeded despite the fault")
			}
			if err := s.Put(originB, sample("isr_test-other")); err == nil {
				t.Fatal("Put succeeded despite the fault")
			}
			if err := s.Delete(originA); err == nil {
				t.Fatal("Delete succeeded despite the fault")
			}
			if got := readFile(t, path); string(got) != string(before) {
				t.Errorf("a failed write changed credentials.json:\n%s", got)
			}
			if got := dirEntries(t, dir); !slices.Equal(got, []string{credentialsFile}) {
				t.Errorf("the directory holds %q, want only credentials.json (no temporary file left)", got)
			}
			assertCredential(t, mustGet(t, s, originA), sample("isr_test-old"))
		})
	}
}

func TestFileStoreCorruptFile(t *testing.T) {
	const token = "isr_test-in-a-corrupt-file" //nolint:gosec // G101: made up, what the log must not repeat
	files := map[string]string{
		"not JSON":         `{"version":1,"credentials":{"isshoni:v1:https://watch.example.com":{"refreshToken":"` + token,
		"empty":            ``,
		"JSON null":        `null`,
		"another version":  `{"version":2,"credentials":{"isshoni:v1:https://watch.example.com":{"v":1,"refreshToken":"` + token + `"}}}`,
		"no version":       `{"credentials":{}}`,
		"wrong field type": `{"version":1,"credentials":[]}`,
	}
	for name, content := range files {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, credentialsFile)
			writeFile(t, path, []byte(content), 0o600)
			log, buf := newLogBuffer()
			s := openFile(t, Options{Dir: dir, Logger: log})

			if _, err := s.Get(originA); !errors.Is(err, ErrNotFound) {
				t.Errorf("Get: err = %v, want ErrNotFound", err)
			}
			assertList(t, s)
			if err := s.Delete(originA); err != nil {
				t.Errorf("Delete: %v, want nil", err)
			}
			if got := readFile(t, path); string(got) != content {
				t.Errorf("reading or deleting changed the corrupt file")
			}
			if n := len(buf.records(t, "WARN", "can't be read")); n != 3 {
				t.Errorf("got %d WARNs for Get, List and Delete, want 3: %s", n, buf.String())
			}

			mustPut(t, s, originB, sample("isr_test-fresh"))
			if got := readFile(t, path+".bad"); string(got) != content {
				t.Errorf("credentials.json.bad doesn't hold the corrupt file")
			}
			assertMode(t, path, 0o600)
			assertList(t, s, originB)
			assertCredential(t, mustGet(t, s, originB), sample("isr_test-fresh"))
			if strings.Contains(buf.String(), token) {
				t.Errorf("the log carries the token: %s", buf.String())
			}
		})
	}
}

func TestFileStoreUnreadableEntry(t *testing.T) {
	const token = "isr_test-unreadable-entry" //nolint:gosec // G101: made up, what the log must not repeat
	dir := t.TempDir()
	path := filepath.Join(dir, credentialsFile)
	writeFile(t, path, []byte(`{"version":1,"credentials":{
		"isshoni:v1:https://watch.example.com": {"v":2,"refreshToken":"`+token+`","deviceId":"d","savedAt":"2026-11-02T19:05:01.004Z"},
		"isshoni:v1:https://alt.example.org:8443": {"v":1,"refreshToken":"isr_test-good","deviceId":"m4q8x2c6v0bn","savedAt":"2026-11-02T19:05:01.004Z","userName":"Alex"}
	}}`), 0o600)
	log, buf := newLogBuffer()
	s := openFile(t, Options{Dir: dir, Logger: log})

	if _, err := s.Get(originA); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get of an unreadable entry: err = %v, want ErrNotFound", err)
	}
	recs := buf.records(t, "WARN", "can't be read")
	if len(recs) != 1 || recs[0]["origin"] != originA || recs[0]["reason"] != "the blob has version 2" {
		t.Errorf("want one WARN naming the origin and the reason: %s", buf.String())
	}
	if strings.Contains(buf.String(), token) {
		t.Errorf("the log carries the token: %s", buf.String())
	}
	assertList(t, s, originB, originA) // still listed, so ForgetAll deletes it
	if err := s.Delete(originA); err != nil {
		t.Fatal(err)
	}
	assertList(t, s, originB)
	assertCredential(t, mustGet(t, s, originB), sample("isr_test-good"))
	if strings.Contains(string(readFile(t, path)), token) {
		t.Errorf("Delete left the unreadable entry in the file")
	}
}

func TestFileStorePrefixes(t *testing.T) {
	dir := t.TempDir()
	app := openFile(t, Options{Dir: dir})
	test := openFile(t, Options{Dir: dir, Prefix: "isshoni-test:3f9a1c:"})
	appAgain := openFile(t, Options{Dir: dir, Prefix: defaultPrefix})

	mustPut(t, app, originA, sample("isr_test-app"))
	mustPut(t, test, originA, sample("isr_test-test"))
	mustPut(t, test, originB, sample("isr_test-test-b"))

	assertCredential(t, mustGet(t, app, originA), sample("isr_test-app"))
	assertCredential(t, mustGet(t, test, originA), sample("isr_test-test"))
	assertList(t, app, originA)
	assertList(t, test, originB, originA)
	assertCredential(t, mustGet(t, appAgain, originA), sample("isr_test-app")) // same file, same prefix

	if err := test.Delete(originA); err != nil {
		t.Fatal(err)
	}
	assertCredential(t, mustGet(t, app, originA), sample("isr_test-app"))
	assertList(t, test, originB)
}
