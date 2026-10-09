package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/server/store"
)

// `isshoni serve` as its own process (serve_test.go has the harness), for what only a real process shows: the
// signal it must survive, the re-exec that restarts it in place, and the exit 78 that only an offline restore
// gets it out of.

// waitForLog waits until the server's log holds text n times, and fails when the server ends first.
func (p *serveProc) waitForLog(t *testing.T, text string, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for strings.Count(p.logs(), text) < n {
		select {
		case <-p.exited:
			t.Fatalf("isshoni serve ended while the test waited for %q in its log: %v\n%s", text, p.err, p.logs())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %q (%d times) in the log within 30 s:\n%s", text, n, p.logs())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// stop sends SIGTERM and checks that the server stops cleanly.
func (p *serveProc) stop(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := p.wait(t); code != exitOK {
		t.Errorf("exit %d after SIGTERM, want 0\n%s", code, p.logs())
	}
}

// SIGHUP does not stop serve. The reload it stands for (04 §6.5) is not built yet, and a process that does not
// handle the signal dies of it, which systemd would count as a clean stop after `systemctl reload isshoni`. serve
// says that a reload is not supported, once per signal, and goes on serving.
func TestServeIgnoresSIGHUP(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs signals")
	}
	t.Parallel()
	p := startServe(t)
	base := p.waitReady(t)
	const line = `"level":"INFO","msg":"reload is not supported yet; restart to apply changes"`
	for n := 1; n <= 3; n++ {
		if err := p.cmd.Process.Signal(syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}
		p.waitForLog(t, line, n)
		if code, body := httpGet(t, base+"/healthz"); code != 200 || body != `{"status":"ok"}`+"\n" {
			t.Errorf("GET /healthz after %d SIGHUPs = %d %q, want 200 ok", n, code, body)
		}
	}
	if code, body := httpGet(t, base+"/readyz"); code != 200 || !strings.HasPrefix(body, `{"status":"ready"`) {
		t.Errorf("GET /readyz after the SIGHUPs = %d %q, want 200 ready", code, body)
	}
	if logs := p.logs(); strings.Contains(logs, `"msg":"shutting down"`) {
		t.Errorf("SIGHUP began a shutdown:\n%s", logs)
	}
	// It still stops when it is told to.
	p.stop(t)
	if n := strings.Count(p.logs(), line); n != 3 {
		t.Errorf("%d lines about the reload for 3 SIGHUPs:\n%s", n, p.logs())
	}
}

// A server that shuts down for a restart starts again in place (04 §6.5): the process re-execs itself, so the PID
// that systemd and Docker watch stays, and the new run finds what the restart was for. The three commands that
// make a server restart are run here against the real server: rotate-secrets, and a backup followed by a restore.
//
// The server under test is this test binary under the name isshoni, started by testscript's command table, which
// makes argv[0] the bare word "isshoni", as it is under the systemd unit: a re-exec by argv[0] would not find the
// program.
func TestServeRestartsInPlace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs signals, and a platform with re-exec")
	}
	t.Parallel()
	dataDir, sock := filepath.Join(t.TempDir(), "data"), socketPath(t)
	p := startServe(t, "--data-dir", dataDir, "--listen.admin-socket", sock)
	first := p.waitReady(t)
	env := []string{"ISSHONI_LISTEN_ADMIN_SOCKET=" + sock}
	secrets := func() string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dataDir, "secrets.json"))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	running := func(what string) {
		t.Helper()
		select {
		case <-p.exited:
			t.Fatalf("%s: the process ended (%v); a restart in place keeps it\n%s", what, p.err, p.logs())
		default:
		}
	}
	before := secrets()

	// rotate-secrets: the command returns when the restarted server answers.
	code, stdout, stderr := runCLIEnv(t, env, "admin", "rotate-secrets", "--yes")
	if code != exitOK || !strings.HasPrefix(stdout, "Rotated: session, invite, resume, vapid. The server is back (isshoni ") {
		t.Fatalf("admin rotate-secrets: exit %d, stdout %q, stderr %q\n%s", code, stdout, stderr, p.logs())
	}
	second := p.waitReady(t)
	running("after rotate-secrets")
	if code, body := httpGet(t, second+"/healthz"); code != 200 || body != `{"status":"ok"}`+"\n" {
		t.Errorf("GET /healthz on the restarted server = %d %q", code, body)
	}
	if second == first {
		t.Errorf("the restarted server listens on %s again; with port 0 a new start binds a new port", first)
	}
	rotated := secrets()
	if rotated == before {
		t.Error("secrets.json is unchanged after rotate-secrets")
	}
	logs := p.logs()
	for _, line := range []string{
		`"msg":"secrets rotated through the admin socket; restarting to apply them"`,
		`"msg":"shutting down","reason":"restart"`,
		`"msg":"restarting in place"`,
	} {
		if strings.Count(logs, line) != 1 {
			t.Errorf("the log has %d of %s, want 1\n%s", strings.Count(logs, line), line, logs)
		}
	}
	if strings.Contains(logs, "start it again") {
		t.Errorf("the server asked to be started again instead of restarting in place:\n%s", logs)
	}

	// backup, then a change, then restore: the restore restarts the server in place too, onto the backup's state.
	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	code, stdout, stderr = runCLIEnv(t, env, "admin", "backup", "--out", archive)
	if code != exitOK || !strings.HasPrefix(stdout, "Wrote "+archive) {
		t.Fatalf("admin backup: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if fi, err := os.Stat(archive); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the archive: %v, mode %v; want 0600", err, fi.Mode().Perm())
	}
	if code, _, stderr := runCLIEnv(t, env, "admin", "rotate-secrets", "--yes"); code != exitOK {
		t.Fatalf("the second rotate-secrets: exit %d, stderr %q", code, stderr)
	}
	p.waitReady(t)
	if secrets() == rotated {
		t.Fatal("secrets.json is unchanged after the second rotation")
	}
	code, stdout, stderr = runCLIEnv(t, env, "admin", "restore", "--yes", archive)
	if code != exitOK || !strings.HasPrefix(stdout, "Restored. The server is back (isshoni ") ||
		!strings.Contains(stdout, "The state from before the restore is in the data directory: backups/pre-restore-") {
		t.Fatalf("admin restore: exit %d, stdout %q, stderr %q\n%s", code, stdout, stderr, p.logs())
	}
	fourth := p.waitReady(t)
	running("after the restore")
	if got := secrets(); got != rotated {
		t.Error("secrets.json after the restore is not the backup's")
	}
	if code, body := httpGet(t, fourth+"/readyz"); code != 200 || !strings.HasPrefix(body, `{"status":"ready"`) {
		t.Errorf("GET /readyz after the restore = %d %q", code, body)
	}
	if left, _ := os.ReadDir(filepath.Join(dataDir, "restore")); len(left) != 0 {
		t.Errorf("restore/ after the restarted server is up: %v", left)
	}
	if n := strings.Count(p.logs(), `"msg":"restarting in place"`); n != 3 {
		t.Errorf("%d restarts in place, want 3 (two rotations and a restore)\n%s", n, p.logs())
	}

	// A reload is still only answered: the process that the re-exec made watches SIGHUP like the first.
	if err := p.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	p.waitForLog(t, reloadNotSupported, 1)
	// The offline commands refuse while it runs: its admin socket answers, and it holds the data directory, which
	// a command that looks at another socket finds out by the lock.
	for _, args := range [][]string{
		{"admin", "backup", "--offline", "--data-dir", dataDir, "--out", "-"},
		{"admin", "restore", "--offline", "--yes", "--data-dir", dataDir, archive},
		{"admin", "backup", "--offline", "--data-dir", dataDir, "--socket", sock + ".none", "--out", "-"},
	} {
		code, stdout, stderr := runCLIEnv(t, env, args...)
		if code != exitRefused || stdout != "" || !strings.Contains(stderr, "the server is running; drop --offline") {
			t.Errorf("isshoni %s while the server runs: exit %d, stdout %q, stderr %q", strings.Join(args, " "), code, stdout, stderr)
		}
	}
	p.stop(t)
}

// The refusal of 04 §6.3 and its way out (04 §12.6, the plan's "refuses to start on a newer schema and tells the
// user which backup to restore"): on a database that a newer isshoni has migrated, serve exits 78 with 03's
// message and the restore command; an offline restore of the pre-migration database file brings the server back.
func TestServeNewerSchemaAndOfflineRestore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs signals")
	}
	t.Parallel()
	dataDir, sock := filepath.Join(t.TempDir(), "data"), socketPath(t)
	flags := []string{"--data-dir", dataDir, "--listen.admin-socket", sock}
	p := startServe(t, flags...)
	p.waitReady(t)
	p.stop(t)

	// What the upgrade to a newer isshoni leaves behind: 03's backup from before its migration, and a database
	// with one schema version more than this build knows.
	latest := store.LatestSchemaVersion()
	db := filepath.Join(dataDir, "isshoni.db")
	backup := filepath.Join(dataDir, "backups", fmt.Sprintf("pre-%d-20261014T021500Z.db", latest))
	if err := os.MkdirAll(filepath.Dir(backup), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.BackupFile(t.Context(), db, backup); err != nil {
		t.Fatal(err)
	}
	alterDB(t, db, `INSERT INTO schema_migrations (version, name, applied_at, app_version) VALUES (?, 'from_the_future', 0, '9.9.9')`, latest+1)

	refused := startServe(t, flags...)
	if code := refused.wait(t); code != exitConfig {
		t.Fatalf("serve on a newer schema: exit %d, want 78\n%s", code, refused.logs())
	}
	logs := refused.logs()
	want03 := fmt.Sprintf("isshoni serve: database schema %d is newer than this isshoni build supports (%d); ", latest+1, latest)
	if !strings.Contains(logs, want03) || !strings.Contains(logs, "or restore the database from before the upgrade: "+backup) {
		t.Errorf("the refusal lacks 03's message with the backup:\n%s", logs)
	}
	// The command for the environment follows it: the systemd form, or the Docker form when the tests run in a
	// container.
	if !strings.Contains(logs, "\n  fix: sudo -u isshoni isshoni admin restore --offline "+backup+" && sudo systemctl start isshoni\n") &&
		!strings.Contains(logs, "\n  fix: docker compose stop && docker compose run --rm isshoni admin restore --offline "+backup+" && docker compose up -d\n") {
		t.Errorf("the refusal lacks the restore command:\n%s", logs)
	}
	// It refuses every time, and it has changed nothing: no restart would help.
	again := startServe(t, flags...)
	if code := again.wait(t); code != exitConfig {
		t.Errorf("serve on a newer schema, again: exit %d, want 78", code)
	}

	// The way out that the message names.
	code, stdout, stderr := runCLI(t, append([]string{"admin", "restore", "--offline", "--yes"}, append(flags, backup)...)...)
	if code != exitOK || !strings.HasPrefix(stdout, "Restored into "+dataDir+". Start the server: ") ||
		!strings.Contains(stdout, "The state from before the restore is in the data directory: backups/pre-restore-") {
		t.Fatalf("admin restore --offline: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	back := startServe(t, flags...)
	base := back.waitReady(t)
	if code, body := httpGet(t, base+"/readyz"); code != 200 || !strings.HasPrefix(body, `{"status":"ready"`) {
		t.Errorf("GET /readyz after the offline restore = %d %q", code, body)
	}
	// The start that succeeded has removed the newer database from restore/; its copy is the pre-restore backup.
	if left, _ := os.ReadDir(filepath.Join(dataDir, "restore")); len(left) != 0 {
		t.Errorf("restore/ after the start: %v", left)
	}
	pre, _ := filepath.Glob(filepath.Join(dataDir, "backups", "pre-restore-*.tar.gz"))
	if len(pre) != 1 {
		t.Fatalf("pre-restore backups: %v", pre)
	}
	archive, err := os.ReadFile(pre[0])
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := archiveNames(t, archive); m.SchemaVersion != latest+1 {
		t.Errorf("the pre-restore backup's schema = %d, want the newer database's %d", m.SchemaVersion, latest+1)
	}
	back.stop(t)
}
