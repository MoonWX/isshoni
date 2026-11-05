package winos

import (
	"bytes"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// countingFiles wraps the OS's secret files (the seam where Windows sets the user-only DACL) and counts what goes
// through it, so the tests see that every file is created by createNew.
type countingFiles struct {
	created, published, replaced atomic.Int32
}

func (c *countingFiles) files(t *testing.T) secretFiles {
	real := testSecretFiles(t)
	return secretFiles{
		createNew: func(path string) (*os.File, error) {
			c.created.Add(1)
			return real.createNew(path)
		},
		publish: func(tmp, path string) error {
			c.published.Add(1)
			return real.publish(tmp, path)
		},
		replace: func(tmp, path string) error {
			c.replaced.Add(1)
			return real.replace(tmp, path)
		},
	}
}

func secretLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

// onlySecretFile fails unless root holds the secret's file and nothing else (no temp file left behind).
func onlySecretFile(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != instanceSecretFile {
		t.Errorf("the data root holds %q, want only %s", names, instanceSecretFile)
	}
}

// TestInstanceSecretCreateAndReuse: the first launch creates the data root and a secret of 32 lowercase hex digits
// in it through createNew (the user-only file); later launches read the same value and write nothing; the value is
// never logged (03 §13.3; README §3 item 118).
func TestInstanceSecretCreateAndReuse(t *testing.T) {
	root := filepath.Join(t.TempDir(), "isshoni")
	var c countingFiles
	log, buf := secretLog()
	secret, err := instanceSecret(root, c.files(t), log)
	if err != nil {
		t.Fatal(err)
	}
	if !validSecret(secret) {
		t.Fatalf("the secret %q isn't 32 lowercase hex digits", secret)
	}
	if c.created.Load() != 1 || c.published.Load() != 1 || c.replaced.Load() != 0 {
		t.Errorf("created %d, published %d, replaced %d; want 1, 1, 0", c.created.Load(), c.published.Load(),
			c.replaced.Load())
	}
	data, err := os.ReadFile(filepath.Join(root, instanceSecretFile))
	if err != nil || string(data) != secret {
		t.Errorf("the file holds %q (%v), want the secret", data, err)
	}
	if runtime.GOOS != "windows" { // Windows' DACL is TestInstanceSecretFileSecurity's
		if fi, err := os.Stat(filepath.Join(root, instanceSecretFile)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("the file's mode: %v %v, want 0600", fi.Mode(), err)
		}
	}
	onlySecretFile(t, root)

	again, err := instanceSecret(root, c.files(t), log)
	if err != nil || again != secret {
		t.Errorf("a second read: %q %v, want the same secret", again, err)
	}
	if c.created.Load() != 1 {
		t.Errorf("a second read wrote a file")
	}
	if strings.Contains(buf.String(), secret) {
		t.Errorf("the secret was logged:\n%s", buf)
	}
	if _, err := instanceSecret("relative", c.files(t), log); err == nil {
		t.Error("a relative data root was accepted")
	}
}

// TestInstanceSecretMalformed: a file that isn't a valid secret (or can't be read) is replaced by a new one, with a
// warning that names neither value; a secret with surrounding whitespace (an edit by hand) still counts.
func TestInstanceSecretMalformed(t *testing.T) {
	const old = "not a secret, but long enough to be noticed if it were logged"
	for _, content := range []string{
		"", old, "0123456789ABCDEF0123456789ABCDEF", "0123456789abcdef", strings.Repeat("a", 4096),
	} {
		root := t.TempDir()
		path := filepath.Join(root, instanceSecretFile)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		var c countingFiles
		log, buf := secretLog()
		secret, err := instanceSecret(root, c.files(t), log)
		if err != nil {
			t.Fatalf("%.20q: %v", content, err)
		}
		if !validSecret(secret) || c.replaced.Load() != 1 {
			t.Errorf("%.20q: secret %q, replaced %d", content, secret, c.replaced.Load())
		}
		if data, _ := os.ReadFile(path); string(data) != secret { //nolint:gosec // G304: the test's temp file
			t.Errorf("%.20q: the file holds %q, want the new secret", content, data)
		}
		out := buf.String()
		if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "a new one replaces it") {
			t.Errorf("%.20q: no warning:\n%s", content, out)
		}
		if strings.Contains(out, secret) || (content != "" && strings.Contains(out, content[:16])) {
			t.Errorf("%.20q: a value was logged:\n%s", content, out)
		}
		onlySecretFile(t, root)
	}

	root := t.TempDir()
	const valid = "00112233445566778899aabbccddeeff"
	if err := os.WriteFile(filepath.Join(root, instanceSecretFile), []byte(" "+valid+"\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := instanceSecret(root, testSecretFiles(t), slog.New(slog.DiscardHandler)); err != nil || got != valid {
		t.Errorf("a secret with whitespace: %q %v", got, err)
	}
}

// TestInstanceSecretUnreadable: a file this user can't read is replaced (the rename needs only the folder).
func TestInstanceSecretUnreadable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("mode 0 denies reading only to a non-root user on Unix")
	}
	root := t.TempDir()
	path := filepath.Join(root, instanceSecretFile)
	if err := os.WriteFile(path, []byte("00112233445566778899aabbccddeeff"), 0o000); err != nil {
		t.Fatal(err)
	}
	log, buf := secretLog()
	secret, err := instanceSecret(root, testSecretFiles(t), log)
	if err != nil || !validSecret(secret) || secret == "00112233445566778899aabbccddeeff" {
		t.Fatalf("an unreadable file: %q %v", secret, err)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("no warning:\n%s", buf)
	}
}

// TestInstanceSecretConcurrent: launches that create the secret at once all end with one value (the losers of the
// publish read the winner's), and no temp file is left behind.
func TestInstanceSecretConcurrent(t *testing.T) {
	for range 20 {
		root := filepath.Join(t.TempDir(), "isshoni")
		const n = 16
		var wg sync.WaitGroup
		got := make([]string, n)
		errs := make([]error, n)
		start := make(chan struct{})
		var c countingFiles
		for i := range n {
			wg.Go(func() {
				<-start
				got[i], errs[i] = instanceSecret(root, c.files(t), slog.New(slog.DiscardHandler))
			})
		}
		close(start)
		wg.Wait()
		for i := range n {
			if errs[i] != nil {
				t.Fatalf("launch %d: %v", i, errs[i])
			}
			if got[i] != got[0] {
				t.Fatalf("launch %d read %q, launch 0 %q", i, got[i], got[0])
			}
		}
		if c.replaced.Load() != 0 {
			t.Errorf("%d replacements while creating", c.replaced.Load())
		}
		onlySecretFile(t, root)
	}
}

// TestInstanceSecretWriteFails: when the secret can't be written the error says so, and a failed publish other than
// "exists" leaves no temp file.
func TestInstanceSecretWriteFails(t *testing.T) {
	root := t.TempDir()
	files := testSecretFiles(t)
	files.publish = func(string, string) error { return errors.New("fake: the disk is full") }
	if _, err := instanceSecret(root, files, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("instanceSecret succeeded without a file")
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}
	files = testSecretFiles(t)
	files.createNew = func(string) (*os.File, error) { return nil, fs.ErrPermission }
	if _, err := instanceSecret(root, files, slog.New(slog.DiscardHandler)); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("createNew denied: %v", err)
	}
}
