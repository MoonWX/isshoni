package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/MoonWX/isshoni/internal/server/store"
)

// `isshoni serve` as its own process (serve_test.go has the harness), for what only a real process shows: the
// signal it must survive, the re-exec that restarts it in place, and the exit 78 that only an offline restore or
// the operator's own hands get it out of.

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

// reloadedLine is the start of the log line of a reload that applied log.level (server.Reload).
const reloadedLine = `"level":"INFO","msg":"reloaded (SIGHUP)"`

// openSocket opens an anonymous WebSocket on the server at base and closes it again. The hub logs the close at
// debug level ("websocket closed"), which makes it a probe of the level the process logs at.
func openSocket(t *testing.T, base string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	origin := strings.Replace(base, "127.0.0.1", "localhost", 1) // the dev site's origin
	c, res, err := websocket.Dial(ctx, strings.Replace(base, "http://", "ws://", 1)+"/ws",
		&websocket.DialOptions{HTTPHeader: http.Header{"Origin": {origin}}})
	if res != nil && res.Body != nil { // nil after a successful Dial
		_ = res.Body.Close()
	}
	if err != nil {
		t.Fatalf("WebSocket to %s: %v", base, err)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
}

// SIGHUP reloads (04 §6.5): serve loads the config again, from its flags and its environment and the file as it is
// now, and the server applies log.level and reads manual TLS files again (server's TestReloadManualCertificate);
// every other change waits for a restart, and the log names those keys. A file with errors changes nothing, and
// says so; it names no key for a restart either (the keys it could not use hold their defaults, which a restart
// would not apply: it would refuse to start). The server never stops for the signal: a process that does not
// handle it dies of it, which systemd would count as a clean stop after `systemctl reload isshoni`.
func TestServeReloadsOnSIGHUP(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs signals")
	}
	t.Parallel()
	file := filepath.Join(t.TempDir(), "isshoni.toml")
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("shutdown_timeout = \"12s\"\n[log]\nlevel = \"info\"\n")
	p := startServe(t, "--config", file)
	base := p.waitReady(t)
	hup := func() {
		t.Helper()
		if err := p.cmd.Process.Signal(syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}
	}
	const closed = `"msg":"websocket closed"`
	const restartKeys = `"msg":"the config has changes that a reload does not apply; restart isshoni to apply them"`

	// At info, the probe's debug line stays out of the log.
	openSocket(t, base)
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(p.logs(), closed) {
		t.Fatalf("a debug line at log.level = info:\n%s", p.logs())
	}

	// The file says debug now, and takes a longer shutdown_timeout, which a reload does not apply.
	write("shutdown_timeout = \"15s\"\n[log]\nlevel = \"debug\"\n")
	hup()
	p.waitForLog(t, reloadedLine, 1)
	logs := p.logs()
	if !strings.Contains(logs, `"msg":"reloaded (SIGHUP)","certificate":"not read: the certificate is not from files (tls.mode is not manual)","log_level":"debug"`) {
		t.Errorf("the reload's line:\n%s", logs)
	}
	if !strings.Contains(logs, `"level":"WARN","msg":"the config has changes that a reload does not apply; restart isshoni to apply them","keys":["shutdown_timeout"]`) {
		t.Errorf("no warning that names shutdown_timeout:\n%s", logs)
	}
	openSocket(t, base)
	p.waitForLog(t, closed, 1)

	// A file with an error is what a restart would refuse: nothing changes, and the log says why. It no longer sets
	// shutdown_timeout, which is no key to restart for: the restart would not start.
	write("[log]\nlevel = \"loud\"\n")
	hup()
	p.waitForLog(t, `"msg":"reloaded (SIGHUP) without the log level"`, 1)
	if logs := p.logs(); !strings.Contains(logs, `"msg":"reload: the config has errors: log.level is not reloaded`) ||
		!strings.Contains(logs, `"key":"log.level"`) {
		t.Errorf("the reload of a file with an error:\n%s", logs)
	}
	openSocket(t, base)
	p.waitForLog(t, closed, 2) // still debug

	// A file that is no TOML: every key holds its default, shutdown_timeout too, and the problem is the file's,
	// with no key.
	write("shutdown_timeout = \"12s\"\n[log\nlevel = \"info\"\n")
	hup()
	p.waitForLog(t, `"msg":"reloaded (SIGHUP) without the log level"`, 2)
	if logs := p.logs(); !strings.Contains(logs, `is not valid TOML`) || strings.Contains(logs, `"key":""`) {
		t.Errorf("the reload of a file that is no TOML:\n%s", logs)
	}
	if n := strings.Count(p.logs(), restartKeys); n != 1 {
		t.Errorf("%d lines that name keys for a restart, want 1 (a config with errors names none):\n%s", n, p.logs())
	}

	// Back to info, and a few signals in a row: the server serves on.
	write("shutdown_timeout = \"12s\"\n[log]\nlevel = \"info\"\n")
	seen := strings.Count(p.logs(), reloadedLine) // the lines without the level start the same way
	for n := 1; n <= 3; n++ {
		hup()
		p.waitForLog(t, reloadedLine, seen+n)
	}
	if n := strings.Count(p.logs(), restartKeys); n != 1 {
		t.Errorf("%d lines that name keys for a restart, want 1:\n%s", n, p.logs())
	}
	if code, body := httpGet(t, base+"/healthz"); code != 200 || body != `{"status":"ok"}`+"\n" {
		t.Errorf("GET /healthz after the SIGHUPs = %d %q, want 200 ok", code, body)
	}
	if code, body := httpGet(t, base+"/readyz"); code != 200 || !strings.HasPrefix(body, `{"status":"ready"`) {
		t.Errorf("GET /readyz after the SIGHUPs = %d %q, want 200 ready", code, body)
	}
	openSocket(t, base)
	time.Sleep(200 * time.Millisecond)
	if n := strings.Count(p.logs(), closed); n != 2 {
		t.Errorf("%d debug lines of the probe, want 2: the level is info again\n%s", n, p.logs())
	}
	if logs := p.logs(); strings.Contains(logs, `"msg":"shutting down"`) {
		t.Errorf("SIGHUP began a shutdown:\n%s", logs)
	}
	// It still stops when it is told to.
	p.stop(t)
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

	// The process that the re-exec made reloads on SIGHUP like the first: the signal was ignored across the exec,
	// and the new serve takes it again.
	if err := p.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	p.waitForLog(t, reloadedLine, 1)
	running("after a SIGHUP to the restarted server")
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

// A restore that the next start can't complete by itself is a refusal too (04 §3.2: a data problem that a restart
// can't fix): with a restore/plan.json that is no plan of a restore, the data directory may be half swapped, and
// only the operator can tell. serve exits 78 with the one message that says what to do, every time, so that
// systemd does not start it five times over; once the operator has removed the file, the server starts.
func TestServeRefusesARestoreItCannotComplete(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs signals")
	}
	t.Parallel()
	dataDir, sock := filepath.Join(t.TempDir(), "data"), socketPath(t)
	flags := []string{"--data-dir", dataDir, "--listen.admin-socket", sock}
	p := startServe(t, flags...)
	p.waitReady(t)
	p.stop(t)

	// What is left of a plan that was damaged: half a JSON document, and the files it was about.
	restore := filepath.Join(dataDir, "restore")
	plan := filepath.Join(restore, "plan.json")
	if err := os.MkdirAll(filepath.Join(restore, "new"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plan, []byte(`{"ts":"20261014T021500Z","state":"swap`), 0o600); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(restore, "new", "isshoni.db")
	if err := os.WriteFile(staged, []byte("the database that was to be restored"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := "isshoni serve: " + plan + " is the record of a restore, and it can't be used (it is not valid JSON). " +
		"Compare the data directory with " + restore + ", put the files where they belong, then remove " + plan + "\n"
	for range 2 { // it refuses every time: no restart helps
		refused := startServe(t, flags...)
		if code := refused.wait(t); code != exitConfig {
			t.Fatalf("serve with a plan that can't be used: exit %d, want 78\n%s", code, refused.logs())
		}
		if logs := refused.logs(); !strings.HasSuffix(logs, want) || strings.Contains(logs, " ready: ") {
			t.Errorf("the refusal:\n%s\nwant it to end with\n%s", logs, want)
		}
	}
	// It has touched nothing: what the operator has to compare is all there.
	if data, err := os.ReadFile(staged); err != nil || string(data) != "the database that was to be restored" {
		t.Errorf("the staged file after the refusals: %q, %v", data, err)
	}

	// The operator's part, and the server is back. The start removes the staging that no plan claims any more.
	if err := os.Remove(plan); err != nil {
		t.Fatal(err)
	}
	back := startServe(t, flags...)
	base := back.waitReady(t)
	if code, body := httpGet(t, base+"/readyz"); code != 200 || !strings.HasPrefix(body, `{"status":"ready"`) {
		t.Errorf("GET /readyz once the plan is removed = %d %q", code, body)
	}
	if left, _ := os.ReadDir(restore); len(left) != 0 {
		t.Errorf("restore/ after the start: %v", left)
	}
	back.stop(t)
}
