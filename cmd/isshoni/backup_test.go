package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/ops"
	"github.com/MoonWX/isshoni/internal/server/store"
	"github.com/MoonWX/isshoni/internal/version"
)

// backup, restore and rotate-secrets as commands (04 §3.1, §12.3, §12.4, §12.6): against the fake server of
// fake_test.go, which has a data directory of its own and "restarts" after a restore or a rotation, and offline
// against a data directory with a real database. serve_test.go runs them against the real server, with its
// re-exec.

// archiveNames lists the entries of a backup archive and returns its manifest.
func archiveNames(t *testing.T, archive []byte) (ops.Manifest, []string) {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("not a gzip stream: %v", err)
	}
	tr := tar.NewReader(gz)
	var m ops.Manifest
	var names []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reading the archive: %v", err)
		}
		if hdr.Name == "manifest.json" {
			if err := json.NewDecoder(tr).Decode(&m); err != nil {
				t.Fatal(err)
			}
			continue
		}
		names = append(names, hdr.Name)
	}
	return m, names
}

// `isshoni admin backup` writes the server's archive to a private file and says that it holds the keys.
func TestBackupCommand(t *testing.T) {
	f, env := startTestServer(t, false)
	out := filepath.Join(t.TempDir(), "b.tar.gz")
	code, stdout, stderr := runCLIEnv(t, env, "admin", "backup", "--out", out)
	if code != exitOK || stderr != "" || !strings.HasPrefix(stdout, "Wrote "+out+" (") ||
		!strings.HasSuffix(stdout, ").\nThis file contains your server's keys. Keep it private.\n") {
		t.Fatalf("admin backup --out: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	fi, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("the archive's mode is %v, want 0600", fi.Mode().Perm())
	}
	archive, _ := os.ReadFile(out)
	m, names := archiveNames(t, archive)
	wantNames := []string{"isshoni.db", "secrets.json", "certmagic/certificates/", "certmagic/certificates/site.crt"}
	if m.SchemaVersion != fakeSchema || m.Format != 1 || !slices.Equal(names, wantNames) {
		t.Errorf("the archive: manifest %+v, entries %v", m, names)
	}

	// A backup never overwrites a file (exit 7), and the file stays what it was.
	code, stdout, stderr = runCLIEnv(t, env, "admin", "backup", "--out", out)
	if code != exitRefused || stdout != "" || stderr != "isshoni admin backup: "+out+" exists, and a backup never overwrites a file: remove it, or name another --out\n" {
		t.Errorf("a second backup to the same file: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if again, _ := os.ReadFile(out); !bytes.Equal(again, archive) {
		t.Error("the refused backup changed the file")
	}

	// --out -: the archive on stdout and the note on stderr; --no-certs leaves the certificates out.
	code, stdout, stderr = runCLIEnv(t, env, "admin", "backup", "--out", "-", "--no-certs")
	if code != exitOK || stderr != "This file contains your server's keys. Keep it private.\n" {
		t.Fatalf("admin backup --out -: exit %d, stderr %q", code, stderr)
	}
	if m, names := archiveNames(t, []byte(stdout)); !slices.Equal(names, []string{"isshoni.db", "secrets.json"}) || slices.Contains(m.Contents, "certmagic/") {
		t.Errorf("--no-certs: manifest %+v, entries %v", m, names)
	}

	// Without --out the file is named for the version and the time, in the working directory.
	dir := t.TempDir()
	t.Chdir(dir)
	code, stdout, stderr = runCLIEnv(t, env, "admin", "backup")
	entries, _ := os.ReadDir(dir)
	if code != exitOK || len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "isshoni-backup-"+version.Version()+"-") ||
		!strings.HasSuffix(entries[0].Name(), "Z.tar.gz") || !strings.HasPrefix(stdout, "Wrote "+entries[0].Name()+" (") {
		t.Errorf("admin backup without --out: exit %d, stdout %q, stderr %q, files %v", code, stdout, stderr, entries)
	}
	if n := f.restarts.Load(); n != 0 {
		t.Errorf("backups asked the server for %d restarts", n)
	}
}

// What `admin backup` refuses before it asks the server (04 §12.3): a terminal as the archive's destination, and
// in a container a backup without --out. Both print the form that works.
func TestBackupRefusals(t *testing.T) {
	_, env := startTestServer(t, false)
	container := append([]string{"ISSHONI_IN_CONTAINER=1"}, env...)
	const docker = "  (umask 077; docker compose exec -T isshoni isshoni admin backup --out - > isshoni-backup.tar.gz)\n"

	code, stdout, stderr := runCLIWith(t, cliRun{environ: env, stdoutTTY: true}, "admin", "backup", "--out", "-")
	if code != exitUsage || stdout != "" || stderr != "isshoni admin backup: stdout is a terminal; redirect it to a file:\n"+
		"  (umask 077; sudo isshoni admin backup --out - > isshoni-backup.tar.gz)\n" {
		t.Errorf("--out - on a terminal: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	// In a container that is `docker compose exec` without -T, whose pseudo-terminal would corrupt the archive.
	code, stdout, stderr = runCLIWith(t, cliRun{environ: container, stdoutTTY: true}, "admin", "backup", "--out", "-")
	if code != exitUsage || stdout != "" || stderr != "isshoni admin backup: stdout is a terminal; redirect it to a file:\n"+docker {
		t.Errorf("--out - on a terminal in a container: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	code, stdout, stderr = runCLIEnv(t, container, "admin", "backup")
	if code != exitUsage || stdout != "" || !strings.HasPrefix(stderr, "isshoni admin backup: --out is required in a container") || !strings.HasSuffix(stderr, docker) {
		t.Errorf("no --out in a container: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	// With --out it works there too.
	if code, stdout, _ := runCLIEnv(t, container, "admin", "backup", "--out", "-"); code != exitOK || !strings.HasPrefix(stdout, "\x1f\x8b") {
		t.Errorf("--out - into a pipe in a container: exit %d", code)
	}
	// A destination that can't be written is a runtime error, and nothing is left there.
	missing := filepath.Join(t.TempDir(), "no-such-dir", "b.tar.gz")
	if code, _, stderr := runCLIEnv(t, env, "admin", "backup", "--out", missing); code != exitRuntime || !strings.Contains(stderr, "the backup can't be written") {
		t.Errorf("--out in a missing directory: exit %d, stderr %q", code, stderr)
	}
}

// fakeArchive is a backup of the fake server as `admin backup --out -` gives it.
func fakeArchive(t *testing.T, env []string) []byte {
	t.Helper()
	code, stdout, stderr := runCLIEnv(t, env, "admin", "backup", "--out", "-")
	if code != exitOK {
		t.Fatalf("admin backup: exit %d, %s", code, stderr)
	}
	return []byte(stdout)
}

// writeTemp writes data to a new file named name and returns its path.
func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeDB reads the fake server's database.
func (f *fakeServer) database(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.data.Paths.DB)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// `isshoni admin restore` hands the file to the server, waits for the restarted server and says what it did.
func TestRestoreCommand(t *testing.T) {
	const preRestore = "The state from before the restore is in the data directory: backups/pre-restore-"
	t.Run("archive", func(t *testing.T) {
		f, env := startTestServer(t, false)
		file := writeTemp(t, "backup.tar.gz", fakeArchive(t, env))
		// The server's database moves on after the backup.
		if err := os.WriteFile(f.data.Paths.DB, fakeDatabase(fakeSchema, "0.3.1"), 0o600); err != nil {
			t.Fatal(err)
		}
		code, stdout, stderr := runCLIEnv(t, env, "admin", "restore", "--yes", file)
		if code != exitOK || stderr != "The server accepted the backup and is restarting…\n" ||
			!strings.HasPrefix(stdout, "Restored. The server is back (isshoni 0.3.0).\n"+preRestore) || !strings.HasSuffix(stdout, ".tar.gz\n") {
			t.Fatalf("admin restore: exit %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		if got := f.database(t); got != string(fakeDatabase(fakeSchema, "0.3.0")) || f.restarts.Load() != 1 {
			t.Errorf("after the restore: database %q, %d restarts", got, f.restarts.Load())
		}
	})
	t.Run("database file", func(t *testing.T) {
		f, env := startTestServer(t, false)
		secrets, _ := os.ReadFile(f.data.Paths.Secrets)
		file := writeTemp(t, "pre-6-20261014T021500Z.db", fakeDatabase(6, "0.2.9"))
		code, stdout, stderr := runCLIEnv(t, env, "admin", "restore", "--yes", file)
		if code != exitOK || !strings.HasPrefix(stdout, "Restored. The server is back (isshoni 0.3.0).\n"+preRestore) {
			t.Fatalf("admin restore of a database file: exit %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		if got := f.database(t); got != string(fakeDatabase(6, "0.2.9")) {
			t.Errorf("the database after the restore: %q", got)
		}
		if now, _ := os.ReadFile(f.data.Paths.Secrets); !bytes.Equal(now, secrets) {
			t.Error("the restore of a database file changed secrets.json")
		}
	})
	t.Run("from stdin", func(t *testing.T) {
		// The Docker form: docker compose exec -T isshoni isshoni admin restore --yes - < backup.tar.gz
		f, env := startTestServer(t, false)
		archive := fakeArchive(t, env)
		code, stdout, stderr := runCLIWith(t, cliRun{environ: env, stdin: string(archive)}, "admin", "restore", "--yes", "-")
		if code != exitOK || !strings.HasPrefix(stdout, "Restored. The server is back") || f.restarts.Load() != 1 {
			t.Errorf("admin restore --yes -: exit %d, stdout %q, stderr %q", code, stdout, stderr)
		}
	})
	t.Run("refused by the server", func(t *testing.T) {
		f, env := startTestServer(t, false)
		before := f.database(t)
		// A backup from a newer isshoni: a precondition is not met (exit 7).
		newer := writeTemp(t, "newer.db", fakeDatabase(fakeSchema+1, "0.5.0"))
		code, stdout, stderr := runCLIEnv(t, env, "admin", "restore", "--yes", newer)
		if code != exitRefused || stdout != "" || stderr != "isshoni admin restore: This backup is from isshoni 0.5.0; install 0.5.0 or newer first\n" {
			t.Errorf("a newer backup: exit %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		// A file that only looks like a database at its start: the server finds out (exit 1).
		broken := writeTemp(t, "broken.db", []byte("SQLite format 3\x00 and nothing else"))
		code, _, stderr = runCLIEnv(t, env, "admin", "restore", "--yes", broken)
		if code != exitRuntime || !strings.HasPrefix(stderr, "isshoni admin restore: this is not a backup that isshoni can restore: its database can't be read") ||
			!strings.Contains(stderr, "\n  fix: pass a backup archive written by `isshoni admin backup`") {
			t.Errorf("a broken database: exit %d, stderr %q", code, stderr)
		}
		if f.database(t) != before || f.restarts.Load() != 0 {
			t.Error("a refused restore changed the server")
		}
	})
	t.Run("refused by the command", func(t *testing.T) {
		f, env := startTestServer(t, false)
		for name, tt := range map[string]struct {
			file string
			want string
		}{
			"a text file":        {writeTemp(t, "notes.txt", []byte("not a backup")), "it is neither a backup archive (.tar.gz) nor an SQLite database file"},
			"an empty file":      {writeTemp(t, "empty", nil), "it is empty"},
			"a gzip of a text":   {writeTemp(t, "text.gz", gzipped(t, "not a tar")), "it is not a tar archive"},
			"a file that is not": {filepath.Join(t.TempDir(), "nowhere.tar.gz"), "the backup can't be read"},
		} {
			code, stdout, stderr := runCLIEnv(t, env, "admin", "restore", "--yes", tt.file)
			if code != exitRuntime || stdout != "" || !strings.Contains(stderr, tt.want) {
				t.Errorf("%s: exit %d, stdout %q, stderr %q; want exit 1 and %q", name, code, stdout, stderr, tt.want)
			}
		}
		if f.restarts.Load() != 0 {
			t.Error("a file that the command refused reached the server")
		}
	})
	t.Run("the server does not come back", func(t *testing.T) {
		defer func(d time.Duration) { restartWait = d }(restartWait)
		restartWait = 600 * time.Millisecond
		f, env := startTestServer(t, false)
		f.stuck.Store(true)
		file := writeTemp(t, "pre-6.db", fakeDatabase(6, "0.2.9"))
		code, stdout, stderr := runCLIEnv(t, env, "admin", "restore", "--yes", file)
		if code != exitRuntime || stdout != "" || !strings.Contains(stderr, "isshoni admin restore: the restore is in place, but the server has not answered within 600ms") ||
			!strings.Contains(stderr, "journalctl -u isshoni") || !strings.Contains(stderr, "`isshoni doctor`") {
			t.Errorf("a server that does not come back: exit %d, stdout %q, stderr %q", code, stdout, stderr)
		}
	})
}

func gzipped(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = io.WriteString(gz, s)
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// restore's argument: "-" is stdin, and a file whose name starts with '-' follows "--".
func TestRestoreArguments(t *testing.T) {
	_, env := startTestServer(t, false)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "-backup.db"), fakeDatabase(6, "0.2.9"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	code, stdout, stderr := runCLIEnv(t, env, "admin", "restore", "--yes", "--", "-backup.db")
	if code != exitOK || !strings.HasPrefix(stdout, "Restored.") {
		t.Errorf("admin restore --yes -- -backup.db: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	// An empty stdin is no backup.
	if code, _, stderr := runCLIEnv(t, env, "admin", "restore", "--yes", "-"); code != exitRuntime || !strings.Contains(stderr, "it is empty") {
		t.Errorf("admin restore --yes - with an empty stdin: exit %d, stderr %q", code, stderr)
	}
}

// Destructive commands ask `Continue? [y/N]` on a terminal and need --yes without one (04 §3.1): the refusal is
// exit 2 with the command to run, in the environment's form.
func TestConfirmation(t *testing.T) {
	f, env := startTestServer(t, false)
	container := append([]string{"ISSHONI_IN_CONTAINER=1"}, env...)
	file := writeTemp(t, "backup.tar.gz", fakeArchive(t, env))
	const needs = ": it needs a confirmation, and there is no terminal to ask on. Add --yes:\n  "

	for name, tt := range map[string]struct {
		run  cliRun
		args []string
		want string // the command that the refusal prints
	}{
		"restore":                       {cliRun{environ: env}, []string{"admin", "restore", file}, "sudo isshoni admin restore --yes " + file},
		"restore, with flags":           {cliRun{environ: env}, []string{"admin", "restore", "--socket", "/run/x y/admin.sock", file}, "sudo isshoni admin restore --yes --socket '/run/x y/admin.sock' " + file},
		"restore from stdin":            {cliRun{environ: env, stdin: "x"}, []string{"admin", "restore", "-"}, "sudo isshoni admin restore --yes - < backup.tar.gz"},
		"restore from stdin, on a tty":  {cliRun{environ: env, stdinTTY: true}, []string{"admin", "restore", "-"}, "sudo isshoni admin restore --yes - < backup.tar.gz"},
		"restore in a container":        {cliRun{environ: container, stdin: "x"}, []string{"admin", "restore", "-"}, "docker compose exec -T isshoni isshoni admin restore --yes - < backup.tar.gz"},
		"restore a file in a container": {cliRun{environ: container}, []string{"admin", "restore", file}, "docker compose exec -T isshoni isshoni admin restore --yes " + file},
		"offline restore":               {cliRun{environ: env}, []string{"admin", "restore", "--offline", file}, "sudo -u isshoni isshoni admin restore --yes --offline " + file},
		"offline restore in a container": {cliRun{environ: container}, []string{"admin", "restore", "--offline", "/var/lib/isshoni/backups/x.db"},
			"docker compose run --rm isshoni admin restore --yes --offline /var/lib/isshoni/backups/x.db"},
		"offline restore from stdin in a container": {cliRun{environ: container}, []string{"admin", "restore", "--offline", "-"},
			"docker compose run --rm -T isshoni admin restore --yes --offline - < backup.tar.gz"},
		"rotate-secrets":                {cliRun{environ: env}, []string{"admin", "rotate-secrets"}, "sudo isshoni admin rotate-secrets --yes"},
		"rotate-secrets in a container": {cliRun{environ: container}, []string{"admin", "rotate-secrets"}, "docker compose exec -T isshoni isshoni admin rotate-secrets --yes"},
	} {
		code, stdout, stderr := runCLIWith(t, tt.run, tt.args...)
		want := "isshoni " + strings.Join(tt.args[:2], " ") + needs + tt.want + "\n"
		if code != exitUsage || stdout != "" || stderr != want {
			t.Errorf("%s: exit %d, stdout %q\nstderr %q\n  want %q", name, code, stdout, stderr, want)
		}
	}
	if n := f.restarts.Load(); n != 0 {
		t.Fatalf("an unconfirmed command reached the server: %d restarts", n)
	}

	// On a terminal the command says what it is about to do and asks.
	ask := func(answer string, args ...string) (int, string, string) {
		return runCLIWith(t, cliRun{environ: env, stdin: answer, stdinTTY: true}, args...)
	}
	code, stdout, stderr := ask("n\n", "admin", "restore", file)
	wantPrompt := file + ": a backup of isshoni " + version.Version() + " from "
	if code != exitRuntime || stdout != "" || !strings.HasPrefix(stderr, wantPrompt) ||
		!strings.Contains(stderr, "(database schema 7), with database, secrets and certificates.\n") ||
		!strings.Contains(stderr, "The backup replaces the server's database, its secrets.json and, when the backup has them, its certificates.\n") ||
		!strings.Contains(stderr, "The server restarts. The current state is saved first, as backups/pre-restore-<time>.tar.gz in the data directory.\n") ||
		!strings.HasSuffix(stderr, "Continue? [y/N] isshoni admin restore: cancelled; nothing was changed\n") {
		t.Errorf("restore, answered n: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	for _, answer := range []string{"\n", "no\n", "yes please\n", "x"} {
		if code, _, stderr := ask(answer, "admin", "rotate-secrets"); code != exitRuntime || !strings.HasSuffix(stderr, "cancelled; nothing was changed\n") {
			t.Errorf("rotate-secrets, answered %q: exit %d, stderr %q", answer, code, stderr)
		}
	}
	// A "terminal" that ends without an answer was none (/dev/null): the refusal with --yes.
	if code, _, stderr := ask("", "admin", "rotate-secrets"); code != exitUsage || !strings.Contains(stderr, "Add --yes:\n  sudo isshoni admin rotate-secrets --yes\n") {
		t.Errorf("rotate-secrets on a terminal that ends: exit %d, stderr %q", code, stderr)
	}
	if n := f.restarts.Load(); n != 0 {
		t.Fatalf("a command that was not confirmed reached the server: %d restarts", n)
	}

	// Nobody is asked about what can't happen: without a server the answer is "not running", before any question.
	code, _, stderr = runCLIWith(t, cliRun{stdin: "y\n", stdinTTY: true}, "admin", "rotate-secrets")
	if code != exitUnreachable || strings.Contains(stderr, "Continue?") {
		t.Errorf("rotate-secrets on a terminal without a server: exit %d, stderr %q", code, stderr)
	}

	// y and yes go on.
	code, stdout, stderr = ask("y\n", "admin", "rotate-secrets")
	if code != exitOK || !strings.HasPrefix(stdout, "Rotated: session, invite, resume, vapid.") ||
		!strings.HasPrefix(stderr, "This replaces the session, invite, resume and push (VAPID) keys and restarts the server.\n") ||
		!strings.Contains(stderr, "Continue? [y/N] The server has new keys and is restarting…\n") {
		t.Errorf("rotate-secrets, answered y: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	f2, env2 := startTestServer(t, false)
	code, stdout, stderr = runCLIWith(t, cliRun{environ: env2, stdin: "YES\n", stdinTTY: true}, "admin", "restore", writeTemp(t, "pre-6.db", fakeDatabase(6, "0.2.9")))
	if code != exitOK || !strings.HasPrefix(stdout, "Restored.") || f2.restarts.Load() != 1 ||
		!strings.Contains(stderr, "pre-6.db is a database file: only the database is replaced; secrets.json and the certificates stay.\n") {
		t.Errorf("restore of a database file, answered YES: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

// `isshoni admin rotate-secrets` asks for all four secrets and waits for the restarted server.
func TestRotateSecretsCommand(t *testing.T) {
	f, env := startTestServer(t, false)
	before, _ := os.ReadFile(f.data.Paths.Secrets)
	code, stdout, stderr := runCLIEnv(t, env, "admin", "rotate-secrets", "--yes")
	if code != exitOK || stderr != "The server has new keys and is restarting…\n" ||
		stdout != "Rotated: session, invite, resume, vapid. The server is back (isshoni 0.3.0); everyone has to sign in again.\n" {
		t.Fatalf("admin rotate-secrets --yes: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	after, _ := os.ReadFile(f.data.Paths.Secrets)
	if bytes.Equal(before, after) || f.restarts.Load() != 1 {
		t.Errorf("after the rotation: secrets.json changed = %v, %d restarts", !bytes.Equal(before, after), f.restarts.Load())
	}
	var doc struct {
		Keys map[string]struct{ ID string }
	}
	if err := json.Unmarshal(after, &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"session", "invite", "resume"} {
		if doc.Keys[key].ID != "2" {
			t.Errorf("the %s key has the id %q after the rotation, want 2", key, doc.Keys[key].ID)
		}
	}
	// The fake is "restarting" for good, as far as its socket knows: a second rotation is told so (exit 1, since
	// a retry after the restart works).
	code, _, stderr = runCLIEnv(t, env, "admin", "rotate-secrets", "--yes")
	if code != exitRuntime || !strings.Contains(stderr, "the server is shutting down") {
		t.Errorf("a rotation during the restart: exit %d, stderr %q", code, stderr)
	}

	t.Run("the server does not come back", func(t *testing.T) {
		defer func(d time.Duration) { restartWait = d }(restartWait)
		restartWait = 600 * time.Millisecond
		f, env := startTestServer(t, false)
		f.stuck.Store(true)
		code, stdout, stderr := runCLIWith(t, cliRun{environ: append([]string{"ISSHONI_IN_CONTAINER=1"}, env...)}, "admin", "rotate-secrets", "--yes")
		if code != exitRuntime || stdout != "" || !strings.Contains(stderr, "the new keys are in place, but the server has not answered within 600ms") ||
			!strings.Contains(stderr, "docker compose logs isshoni") {
			t.Errorf("a server that does not come back: exit %d, stdout %q, stderr %q", code, stdout, stderr)
		}
	})
}

// realDataDir makes a data directory as a stopped server leaves it, with a real database, and returns the flags
// that point a command at it and at a socket where nothing listens.
func realDataDir(t *testing.T) (dir string, flags []string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := (&config.Config{DataDir: dir}).Paths()
	db, err := store.Open(context.Background(), store.Options{
		Path: p.DB, BackupDir: p.Backups, AppVersion: version.Version(), Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := config.OpenSecrets(p.Secrets, nil); err != nil {
		t.Fatal(err)
	}
	return dir, []string{"--data-dir", dir, "--listen.admin-socket", socketPath(t)}
}

// countRows runs a query for one number on the database file of a stopped server, without leaving a file next to
// it.
func countRows(t *testing.T, file, query string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(file)+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRowContext(context.Background(), query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// alterDB runs a statement on a database file.
func alterDB(t *testing.T, file, stmt string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(context.Background(), stmt, args...); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

// The offline commands act on the data directory of a stopped server (04 §12.6): a backup with 03's
// store.BackupFile, and both halves of a restore, after which the operator starts the server.
func TestOfflineBackupAndRestore(t *testing.T) {
	dir, flags := realDataDir(t)
	p := (&config.Config{DataDir: dir}).Paths()
	run := func(args ...string) (int, string, string) {
		t.Helper()
		return runCLI(t, append(append([]string{"admin"}, args[0]), append(flags, args[1:]...)...)...)
	}
	rooms := func() int { return countRows(t, p.DB, `SELECT count(*) FROM rooms`) }
	addRoom := func(id string) {
		alterDB(t, p.DB, `INSERT INTO rooms (id, name, name_key, created_at, updated_at) VALUES (?, ?, ?, 0, 0)`, id, id, id)
	}
	addRoom("cinema") // next to Lounge, which every database has

	out := filepath.Join(t.TempDir(), "offline.tar.gz")
	code, stdout, stderr := run("backup", "--offline", "--out", out)
	if code != exitOK || stderr != "" || !strings.HasPrefix(stdout, "Wrote "+out+" (") {
		t.Fatalf("admin backup --offline: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	archive, _ := os.ReadFile(out)
	m, names := archiveNames(t, archive)
	if m.Raw || m.SchemaVersion != store.LatestSchemaVersion() || !slices.Equal(names, []string{"isshoni.db", "secrets.json"}) {
		t.Errorf("the offline backup: manifest %+v, entries %v", m, names)
	}
	// It left the data directory as it was: no -wal, no -shm, nothing staged.
	if left, _ := filepath.Glob(filepath.Join(dir, "isshoni.db-*")); len(left) != 0 {
		t.Errorf("the offline backup left %v", left)
	}
	if left, _ := os.ReadDir(p.Restore); len(left) != 0 {
		t.Errorf("the offline backup left %v in restore/", left)
	}

	// Changes after the backup; then the restore brings the backup's state back and keeps the changed one.
	addRoom("stage")
	if n := rooms(); n != 3 {
		t.Fatalf("%d rooms before the restore, want 3", n)
	}
	code, stdout, stderr = run("restore", "--offline", "--yes", out)
	if code != exitOK || !strings.HasPrefix(stdout, "Restored into "+dir+". Start the server: sudo systemctl start isshoni\n"+
		"The state from before the restore is in the data directory: backups/pre-restore-") {
		t.Fatalf("admin restore --offline: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if n := rooms(); n != 2 {
		t.Errorf("%d rooms after the restore, want the backup's 2", n)
	}
	pre, _ := filepath.Glob(filepath.Join(p.Backups, "pre-restore-*.tar.gz"))
	if len(pre) != 1 {
		t.Fatalf("pre-restore backups: %v", pre)
	}
	// The files from before the restore wait in restore/old-<ts> for the server's next successful start.
	if left, _ := os.ReadDir(p.Restore); len(left) != 2 || !strings.HasPrefix(left[0].Name(), "old-") || left[1].Name() != "plan.json" {
		t.Errorf("restore/ after the offline restore: %v", left)
	}

	// Undo with the pre-restore backup; in a container the hint names compose.
	code, stdout, stderr = runCLIEnv(t, []string{"ISSHONI_IN_CONTAINER=1"}, append([]string{"admin", "restore"}, append(flags, "--offline", "--yes", pre[0])...)...)
	if code != exitOK || !strings.HasPrefix(stdout, "Restored into "+dir+". Start the server: docker compose up -d\n") {
		t.Fatalf("the second offline restore: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if n := rooms(); n != 3 {
		t.Errorf("%d rooms after restoring the pre-restore backup, want 3", n)
	}

	// From stdin, as `docker compose run --rm -T isshoni admin restore --offline --yes - < backup.tar.gz` gives it.
	code, stdout, stderr = runCLIWith(t, cliRun{stdin: string(archive)}, append([]string{"admin", "restore"}, append(flags, "--offline", "--yes", "-")...)...)
	if code != exitOK || rooms() != 2 {
		t.Errorf("offline restore from stdin: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	// A backup from a newer isshoni is refused (exit 7), and one that is none is an error (exit 1); neither
	// changes anything.
	newer := writeTemp(t, "newer.db", nil)
	_ = os.Remove(newer)
	if err := store.BackupFile(context.Background(), p.DB, newer); err != nil {
		t.Fatal(err)
	}
	alterDB(t, newer, `INSERT INTO schema_migrations (version, name, applied_at, app_version) VALUES (?, 'from_the_future', 0, '9.9.9')`, store.LatestSchemaVersion()+1)
	code, _, stderr = run("restore", "--offline", "--yes", newer)
	if code != exitRefused || !strings.HasPrefix(stderr, "isshoni admin restore: This backup is from ") {
		t.Errorf("an offline restore of a newer database: exit %d, stderr %q", code, stderr)
	}
	code, _, stderr = run("restore", "--offline", "--yes", writeTemp(t, "x.tar.gz", gzipped(t, "junk")))
	if code != exitRuntime || !strings.Contains(stderr, "this is not a backup that isshoni can restore") {
		t.Errorf("an offline restore of junk: exit %d, stderr %q", code, stderr)
	}
	if n := rooms(); n != 2 {
		t.Errorf("%d rooms after the refused restores", n)
	}
}

// An offline backup of a database that can't be read as one holds its raw files, and says so (04 §12.3).
func TestOfflineBackupRaw(t *testing.T) {
	dir, flags := realDataDir(t)
	p := (&config.Config{DataDir: dir}).Paths()
	if err := os.WriteFile(p.DB, []byte("this was a database once"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.DB+"-wal", []byte("and this its WAL"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI(t, append([]string{"admin", "backup"}, append(flags, "--offline", "--out", "-")...)...)
	if code != exitOK || !strings.Contains(stderr, "the backup holds its raw files instead") ||
		!strings.Contains(stderr, "this backup holds its files as they are") || !strings.HasSuffix(stderr, "This file contains your server's keys. Keep it private.\n") {
		t.Fatalf("offline backup of a damaged database: exit %d, stderr %q", code, stderr)
	}
	// (SQLite may have left a -shm next to the WAL when it tried to read the file; a raw backup takes what is there.)
	m, names := archiveNames(t, []byte(stdout))
	if !m.Raw || len(names) < 3 || names[0] != "isshoni.db" || names[1] != "isshoni.db-wal" || names[len(names)-1] != "secrets.json" {
		t.Errorf("manifest %+v, entries %v; want a raw backup with the WAL", m, names)
	}
	// Restoring it is refused: its database fails the checks.
	code, _, stderr = runCLIWith(t, cliRun{stdin: stdout}, append([]string{"admin", "restore"}, append(flags, "--offline", "--yes", "-")...)...)
	if code != exitRuntime || !strings.Contains(stderr, "this is not a backup that isshoni can restore") {
		t.Errorf("restoring the raw backup of a damaged database: exit %d, stderr %q", code, stderr)
	}

	// An empty data directory has nothing to back up.
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runCLI(t, "admin", "backup", "--offline", "--out", "-", "--data-dir", empty)
	if code != exitRuntime || stdout != "" || stderr != "isshoni admin backup: there is nothing to back up in "+empty+": no database, no secrets.json and no certificates\n" {
		t.Errorf("offline backup of an empty data directory: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	// And one that does not exist is the operator's typo.
	code, _, stderr = runCLI(t, "admin", "backup", "--offline", "--out", "-", "--data-dir", filepath.Join(empty, "nope"))
	if code != exitRuntime || !strings.Contains(stderr, "there is no data directory at "+filepath.Join(empty, "nope")+" (data_dir)") {
		t.Errorf("offline backup without a data directory: exit %d, stderr %q", code, stderr)
	}
}

// The offline commands refuse (exit 7) while a server answers on the admin socket, while another process holds
// the data-directory lock, and as root outside a container (04 §12.6). Nothing is changed.
func TestOfflineRefusals(t *testing.T) {
	dir, flags := realDataDir(t)
	p := (&config.Config{DataDir: dir}).Paths()
	file := writeTemp(t, "pre.db", nil)
	_ = os.Remove(file)
	if err := store.BackupFile(context.Background(), p.DB, file); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadDir(dir)
	commands := map[string][]string{
		"backup":  {"admin", "backup", "--offline", "--out", "-"},
		"restore": {"admin", "restore", "--offline", "--yes", file},
	}
	args := func(name string, flags []string) []string {
		c := commands[name]
		return append(append(slices.Clone(c[:2]), flags...), c[2:]...)
	}

	// A server answers on the socket: this check is first, because its message is the friendliest.
	_, env := startTestServer(t, false)
	for name := range commands {
		code, stdout, stderr := runCLIEnv(t, env, args(name, []string{"--data-dir", dir})...)
		if want := "isshoni admin " + name + ": the server is running; drop --offline\n"; code != exitRefused || stdout != "" || stderr != want {
			t.Errorf("%s --offline with a server on the socket: exit %d, stdout %q, stderr %q", name, code, stdout, stderr)
		}
	}
	// The data-directory lock is held: a server whose socket this command does not see (another container on the
	// same volume).
	lock, err := config.LockDataDir(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	for name := range commands {
		code, stdout, stderr := runCLI(t, args(name, flags)...)
		want := "isshoni admin " + name + ": the server is running; drop --offline (another isshoni process is using " + dir + ")\n"
		if code != exitRefused || stdout != "" || stderr != want {
			t.Errorf("%s --offline with the lock held: exit %d, stdout %q, stderr %q", name, code, stdout, stderr)
		}
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}

	// Root outside a container would leave files that the service user can't open.
	for name := range commands {
		a := args(name, flags)
		code, stdout, stderr := runCLIWith(t, cliRun{root: true}, a...)
		want := "isshoni admin " + name + ": run as the service user, so that the files stay the server's:\n  sudo -u isshoni isshoni " + strings.Join(a, " ") + "\n"
		if code != exitRefused || stdout != "" || stderr != want {
			t.Errorf("%s --offline as root: exit %d, stdout %q\nstderr %q\n  want %q", name, code, stdout, stderr, want)
		}
	}
	after, _ := os.ReadDir(dir)
	if _, err := os.Lstat(p.Lock); err != nil || len(after) != len(before)+1 { // the lock file, from the test's own lock
		t.Errorf("the refused commands changed the data directory: %v (lock file: %v), before %v", after, err, before)
	}
	// Root in a container is the documented host-network setup, and allowed.
	code, stdout, stderr := runCLIWith(t, cliRun{root: true, environ: []string{"ISSHONI_IN_CONTAINER=1"}}, args("backup", flags)...)
	if code != exitOK || !strings.HasPrefix(stdout, "\x1f\x8b") {
		t.Errorf("backup --offline as root in a container: exit %d, stderr %q", code, stderr)
	}
}

// An offline command first completes a restore that was interrupted, as the server would at its start, and says
// so on stderr.
func TestOfflineCompletesAnInterruptedRestore(t *testing.T) {
	dir, flags := realDataDir(t)
	p := (&config.Config{DataDir: dir}).Paths()
	snapshot := writeTemp(t, "pre.db", nil)
	_ = os.Remove(snapshot)
	if err := store.BackupFile(context.Background(), p.DB, snapshot); err != nil {
		t.Fatal(err)
	}
	alterDB(t, p.DB, `INSERT INTO rooms (id, name, name_key, created_at, updated_at) VALUES ('cinema', 'Cinema', 'cinema', 0, 0)`)
	// The first half of a restore, and no second.
	data := &ops.DataDir{Paths: p, DB: dbFiles(p)}
	body, err := os.Open(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	_, err = data.PrepareRestore(context.Background(), body, -1)
	_ = body.Close()
	if err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCLI(t, append([]string{"admin", "backup"}, append(flags, "--offline", "--out", "-")...)...)
	if code != exitOK || !strings.HasPrefix(stderr, "isshoni: a restore was interrupted before its files were swapped in; completing it now\n") {
		t.Fatalf("offline backup over an interrupted restore: exit %d, stderr %q", code, stderr)
	}
	// The backup is of the restored state: the database without the room.
	_, names := archiveNames(t, []byte(stdout))
	if !slices.Contains(names, "isshoni.db") {
		t.Fatalf("entries %v", names)
	}
	if n := countRows(t, p.DB, `SELECT count(*) FROM rooms WHERE id = 'cinema'`); n != 0 {
		t.Errorf("the room from after the snapshot is there (%d): want the restored database", n)
	}
}

// stdin counts as a terminal only when it is a character device other than the null device.
func TestStdinTerminal(t *testing.T) {
	file, err := os.Open(writeTemp(t, "f", []byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = null.Close() }()
	for name, in := range map[string]io.Reader{"nothing": nil, "a buffer": strings.NewReader("y"), "a file": file, "the null device": null} {
		if (&invocation{stdin: in}).stdinTerminal() {
			t.Errorf("%s counts as a terminal", name)
		}
	}
	if !(&invocation{stdinTTY: func() bool { return true }}).stdinTerminal() {
		t.Error("the test's own answer is not taken")
	}
	// The same for stdout: `--out - > /dev/null` writes to no terminal, and a backup has no reason to refuse it.
	sink, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sink.Close() }()
	if inv := (&invocation{}); inv.terminal(sink) || inv.terminal(file) || inv.terminal(io.Discard) {
		t.Error("the null device, a file or a writer that is no file counts as a terminal for stdout")
	}
}

func TestByteSize(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 999: "999 B", 1000: "1.0 kB", 3_400_000: "3.4 MB", 2_500_000_000: "2.5 GB"} {
		if got := byteSize(n); got != want {
			t.Errorf("byteSize(%d) = %q, want %q", n, got, want)
		}
	}
}
