package server_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server"
	"github.com/MoonWX/isshoni/internal/server/netx"
	"github.com/MoonWX/isshoni/internal/server/ops"
	"github.com/MoonWX/isshoni/internal/server/servertest"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// Backup, restore and rotate-secrets in a running server (04 §17; README S65): the admin socket's three endpoints
// over the real store, the swap at the end of the shutdown, and the start that follows. servertest's Restart
// stands in for the re-exec (04 §6.5); cmd/isshoni's tests run the real one.

// query runs a statement on the server's database file through a connection of the test's own, as a second
// process would. A running server's writer is busy only for moments, so the connection waits for it.
func query(t *testing.T, srv *servertest.Server, readOnly bool, stmt string, args ...any) [][]string {
	t.Helper()
	dsn := "file:" + filepath.ToSlash(srv.Cfg.Paths().DB) + "?_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)"
	if readOnly {
		dsn += "&mode=ro"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if !readOnly {
		if _, err := db.ExecContext(ctx, stmt, args...); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
		return nil
	}
	rows, err := db.QueryContext(ctx, stmt, args...)
	if err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
	defer func() { _ = rows.Close() }()
	cols, _ := rows.Columns()
	var out [][]string
	for rows.Next() {
		vals := make([]any, len(cols))
		for i := range vals {
			vals[i] = new(sql.NullString)
		}
		if err := rows.Scan(vals...); err != nil {
			t.Fatal(err)
		}
		row := make([]string, len(cols))
		for i, v := range vals {
			row[i] = v.(*sql.NullString).String
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// accountState is what the tests compare of a database: the rows that make the accounts, without the columns that
// every request moves (last seen, rotation times).
func accountState(t *testing.T, srv *servertest.Server) map[string][][]string {
	t.Helper()
	return map[string][][]string{
		"users":    query(t, srv, true, `SELECT id, username, role, status FROM users ORDER BY id`),
		"sessions": query(t, srv, true, `SELECT id, user_id, hex(token_hash) FROM sessions ORDER BY id`),
		"rooms":    query(t, srv, true, `SELECT id, name FROM rooms ORDER BY id`),
		"invites":  query(t, srv, true, `SELECT id, hex(token_hash), max_uses, uses FROM invites ORDER BY id`),
		"push":     query(t, srv, true, `SELECT id, user_id, session_id, endpoint FROM push_subscriptions ORDER BY id`),
		"codes":    query(t, srv, true, `SELECT hex(device_code_hash) FROM device_codes ORDER BY 1`),
	}
}

func count(t *testing.T, srv *servertest.Server, table string) int {
	t.Helper()
	return len(query(t, srv, true, "SELECT 1 FROM "+table))
}

// seedPush gives the admin's session a push subscription and the server a pending device code, written directly:
// device codes come with M2, and the rows are what a backup and a rotation have to deal with, not what a browser
// would send (push_test.go subscribes through the REST API).
func seedPush(t *testing.T, srv *servertest.Server) {
	t.Helper()
	session := query(t, srv, true, `SELECT s.id, s.user_id FROM sessions s JOIN users u ON u.id = s.user_id WHERE u.username = ?`, adminName)
	if len(session) != 1 {
		t.Fatalf("sessions of %s: %v", adminName, session)
	}
	now := time.Now().UnixMilli()
	query(t, srv, false, `INSERT INTO push_subscriptions (id, user_id, session_id, endpoint, p256dh, auth_secret, name, created_at)
		VALUES ('pushpushpush', ?, ?, 'https://push.example.com/send/abc', 'p256dh', 'auth', 'Chrome on macOS', ?)`,
		session[0][1], session[0][0], now)
	query(t, srv, false, `INSERT INTO device_codes (device_code_hash, user_code_hash, client_kind, device_name, os, app_version,
		request_ip, created_at, expires_at) VALUES (x'01', x'02', 'desktop', 'Desk', 'windows', '0.3.0', '203.0.113.9', ?, ?)`,
		now, now+600_000)
}

// browser is a second browser for srv: a cookie jar of its own on the server's transport.
func browser(t *testing.T, srv *servertest.Server) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: srv.Client.Transport, Jar: jar}
}

// callAs is call with another browser than srv.Client.
func callAs(t *testing.T, c *http.Client, srv *servertest.Server, method, path string, body any) response {
	t.Helper()
	own := srv.Client
	srv.Client = c
	defer func() { srv.Client = own }()
	return call(t, srv, method, path, body)
}

// inviteToken mints an invite on the admin socket and returns its token.
func inviteToken(t *testing.T, srv *servertest.Server) string {
	t.Helper()
	admin, ctx := adminSocket(t, srv)
	link, err := admin.CreateInvite(ctx, 0, 0)
	if err != nil {
		t.Fatalf("invite create: %v", err)
	}
	token, ok := strings.CutPrefix(link.URL, srv.URL+"/invite#")
	if !ok || token == "" {
		t.Fatalf("invite link %q", link.URL)
	}
	return token
}

// register creates an account with an invite, in a browser of its own, and returns that browser.
func register(t *testing.T, srv *servertest.Server, name string) *http.Client {
	t.Helper()
	c := browser(t, srv)
	res := callAs(t, c, srv, http.MethodPost, "/api/v1/auth/register", api.RegisterRequest{
		InviteToken: inviteToken(t, srv), Username: name, Password: "a long enough password",
	})
	wantStatus(t, "register "+name, res, http.StatusCreated, "")
	return c
}

// whoAmI is GET /api/v1/me in a browser: the user's name, or the error's code.
func whoAmI(t *testing.T, c *http.Client, srv *servertest.Server) string {
	t.Helper()
	res := callAs(t, c, srv, http.MethodGet, "/api/v1/me", nil)
	if res.status != http.StatusOK {
		return errorCode(res.body)
	}
	var me api.Me
	if err := json.Unmarshal([]byte(res.body), &me); err != nil {
		t.Fatalf("GET /me: %v: %s", err, res.body)
	}
	return me.User.Username
}

// takeBackup downloads a backup archive from the admin socket.
func takeBackup(t *testing.T, srv *servertest.Server) []byte {
	t.Helper()
	admin, ctx := adminSocket(t, srv)
	b, err := admin.Backup(ctx, true)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	defer func() { _ = b.Close() }()
	data, err := io.ReadAll(b)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	return data
}

// restoreAndRestart sends body to the admin socket's restore, waits for the server to end as a restore ends a
// server (Run returns ErrRestartRequested) and starts the next one on the same data directory.
func restoreAndRestart(t *testing.T, srv *servertest.Server, body []byte, contentType string) ops.AdminRestoreResult {
	t.Helper()
	admin, ctx := adminSocket(t, srv)
	res, err := admin.Restore(ctx, bytes.NewReader(body), contentType)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !res.Restarting {
		t.Errorf("restore answered %+v, want restarting", res)
	}
	if err := srv.Wait(t); !errors.Is(err, server.ErrRestartRequested) {
		t.Fatalf("Run after a restore = %v, want ErrRestartRequested", err)
	}
	srv.Restart(t)
	return res
}

// dirNames lists a directory's entries, or nothing when there is no such directory.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// TestBackupRestoreRoundTrip is the round trip of 04 §17: accounts, a push subscription and certificates; a
// backup; changes; the restore. Afterwards the state is the backup's, and a browser that was signed in when the
// backup was taken still is. The restore itself can be undone with the pre-restore backup it left.
func TestBackupRestoreRoundTrip(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{Deps: wiredDeps()})
	paths := srv.Cfg.Paths()
	setUp(t, srv)
	sam := register(t, srv, "Sam")
	wantStatus(t, "create room", call(t, srv, http.MethodPost, "/api/v1/admin/rooms", api.CreateRoomRequest{Name: "Cinema"}), http.StatusCreated, "")
	seedPush(t, srv)
	unused := inviteToken(t, srv)
	// An off-mode server keeps no certificates; these stand for the ones of a server that does.
	cert := filepath.Join(paths.CertMagic, "certificates", "acme-v02", "watch.example.com", "watch.example.com.crt")
	if err := os.MkdirAll(filepath.Dir(cert), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cert, []byte("the certificate at backup time"), 0o600); err != nil {
		t.Fatal(err)
	}
	secrets, err := os.ReadFile(paths.Secrets)
	if err != nil {
		t.Fatal(err)
	}
	want := accountState(t, srv)
	if len(want["users"]) != 2 || len(want["sessions"]) != 2 || len(want["push"]) != 1 || len(want["rooms"]) != 2 || len(want["invites"]) != 2 {
		t.Fatalf("the state to back up: %v", want)
	}

	archive := takeBackup(t, srv)
	m, err := ops.ReadManifest(bytes.NewReader(archive))
	if err != nil || m.SchemaVersion != store.LatestSchemaVersion() || !slices.Equal(m.Contents, []string{"isshoni.db", "secrets.json", "certmagic/"}) {
		t.Fatalf("the backup's manifest = %+v, %v", m, err)
	}
	if got := accountState(t, srv); !reflect.DeepEqual(got, want) {
		t.Fatalf("the backup changed the database: %v", got)
	}

	// What happens after the backup: a third account, a room less, Sam signs out, the push subscription goes, and
	// the certificate is renewed.
	register(t, srv, "Robin")
	rooms := query(t, srv, true, `SELECT id FROM rooms WHERE name = 'Cinema'`)
	wantStatus(t, "delete room", call(t, srv, http.MethodDelete, "/api/v1/admin/rooms/"+rooms[0][0], nil), http.StatusNoContent, "")
	site, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	samsCookies := sam.Jar.Cookies(site) // signing out takes the cookie from the browser; a backup of the browser has it
	wantStatus(t, "Sam logs out", callAs(t, sam, srv, http.MethodPost, "/api/v1/auth/logout", struct{}{}), http.StatusNoContent, "")
	query(t, srv, false, `DELETE FROM push_subscriptions`)
	if err := os.WriteFile(cert, []byte("a renewed certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed := accountState(t, srv)
	if reflect.DeepEqual(changed, want) || whoAmI(t, sam, srv) != api.CodeUnauthenticated {
		t.Fatal("the changes after the backup did not change anything")
	}

	// A friend who is watching sees the restart notice of 04 §6.4, like at any restart.
	ws := dialWS(t, srv)
	joinLounge(t, ws, signaltest.DefaultHello())

	res := restoreAndRestart(t, srv, archive, ops.AdminContentArchive)
	if code := closeCode(t, ws); code != websocket.StatusServiceRestart {
		t.Errorf("the WebSocket closed with %d at the restore, want 1012", code)
	}
	if got := accountState(t, srv); !reflect.DeepEqual(got, want) {
		t.Errorf("the state after the restore:\n got %v\nwant the backup's %v", got, want)
	}
	if got, _ := os.ReadFile(cert); string(got) != "the certificate at backup time" {
		t.Errorf("the certificate after the restore: %q", got)
	}
	if got, _ := os.ReadFile(paths.Secrets); !bytes.Equal(got, secrets) {
		t.Error("secrets.json after the restore is not the backup's")
	}
	// Sessions from the backup still work: the admin's, and Sam's, who had signed out since.
	if got := whoAmI(t, srv.Client, srv); got != adminName {
		t.Errorf("the admin after the restore: %s", got)
	}
	sam.Jar.SetCookies(site, samsCookies)
	if got := whoAmI(t, sam, srv); got != "Sam" {
		t.Errorf("Sam after the restore: %s, want the session from the backup to work", got)
	}
	// So does a link that was open at backup time: its key is the backup's too.
	if res := call(t, srv, http.MethodPost, "/api/v1/auth/invite/check", api.TokenRequest{Token: unused}); res.status != http.StatusOK {
		t.Errorf("the invite from the backup after the restore: %d %s", res.status, res.body)
	}

	// The start that followed has cleaned up: no staging, no plan, no old files; the pre-restore backup stays.
	if left := dirNames(t, paths.Restore); len(left) != 0 {
		t.Errorf("restore/ after the restart: %v, want nothing", left)
	}
	if !strings.HasPrefix(res.PreRestoreBackup, "backups/pre-restore-") || !strings.HasSuffix(res.PreRestoreBackup, ".tar.gz") {
		t.Fatalf("preRestoreBackup = %q", res.PreRestoreBackup)
	}
	pre, err := os.ReadFile(filepath.Join(srv.DataDir, filepath.FromSlash(res.PreRestoreBackup)))
	if err != nil {
		t.Fatal(err)
	}
	logs := srv.Logs()
	for _, line := range []string{
		`"msg":"restore accepted through the admin socket; restarting to swap the files in"`,
		`"msg":"shutting down","reason":"restore"`,
		`"msg":"restore: the data directory holds the restored files; the previous ones are kept until the next successful start"`,
		`"msg":"restore finished: removed the files from before the restore"`,
	} {
		if !strings.Contains(logs, line) {
			t.Errorf("the log lacks %s", line)
		}
	}

	// Undo: the pre-restore backup holds what the restore replaced.
	restoreAndRestart(t, srv, pre, ops.AdminContentArchive)
	if got := accountState(t, srv); !reflect.DeepEqual(got, changed) {
		t.Errorf("the state after restoring the pre-restore backup:\n got %v\nwant the one from before the restore %v", got, changed)
	}
	if got, _ := os.ReadFile(cert); string(got) != "a renewed certificate" {
		t.Errorf("the certificate after the undo: %q", got)
	}
	if got := dirNames(t, paths.Backups); len(got) != 2 {
		t.Errorf("backups/ after two restores: %v, want two pre-restore backups", got)
	}
}

// snapshotDB copies the server's database into a file of its own, as 03 does before a migration
// (backups/pre-<schema>-<ts>.db).
func snapshotDB(t *testing.T, srv *servertest.Server, dst string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.BackupFile(context.Background(), srv.Cfg.Paths().DB, dst); err != nil {
		t.Fatal(err)
	}
}

// TestRestoreDatabaseFile: a bare database file replaces the database alone (04 §12.4): the keys stay, so the
// sessions in that database work with them.
func TestRestoreDatabaseFile(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{Deps: wiredDeps()})
	paths := srv.Cfg.Paths()
	setUp(t, srv)
	want := accountState(t, srv)
	file := filepath.Join(paths.Backups, fmt.Sprintf("pre-%d-20261014T021500Z.db", store.LatestSchemaVersion()))
	snapshotDB(t, srv, file)
	secrets, _ := os.ReadFile(paths.Secrets)

	register(t, srv, "Sam")
	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	res := restoreAndRestart(t, srv, body, ops.AdminContentSQLite)
	if got := accountState(t, srv); !reflect.DeepEqual(got, want) {
		t.Errorf("the state after restoring the database file:\n got %v\nwant %v", got, want)
	}
	if got, _ := os.ReadFile(paths.Secrets); !bytes.Equal(got, secrets) {
		t.Error("restoring a database file changed secrets.json")
	}
	if got := whoAmI(t, srv.Client, srv); got != adminName {
		t.Errorf("the admin after the restore: %s", got)
	}
	// 03's backup is still there, next to the pre-restore backup that this restore wrote.
	names := dirNames(t, paths.Backups)
	if !slices.Contains(names, filepath.Base(file)) || !slices.Contains(names, strings.TrimPrefix(res.PreRestoreBackup, "backups/")) || len(names) != 2 {
		t.Errorf("backups/ = %v", names)
	}
}

// TestRestoreRefused: what is no backup this build can restore is refused with 04 §12.2's codes, and the server
// goes on serving what it has. The database is asked itself, through 03's InspectFile.
func TestRestoreRefused(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{Deps: wiredDeps()})
	paths := srv.Cfg.Paths()
	setUp(t, srv)
	before := accountState(t, srv)
	admin, ctx := adminSocket(t, srv)

	alter := func(file, stmt string, args ...any) []byte {
		t.Helper()
		db, err := sql.Open("sqlite", file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(context.Background(), stmt, args...); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	dir := t.TempDir()
	newer := filepath.Join(dir, "newer.db")
	snapshotDB(t, srv, newer)
	future := store.LatestSchemaVersion() + 1
	other := filepath.Join(dir, "other.db")
	snapshotDB(t, srv, other)
	empty := filepath.Join(dir, "empty.db")

	for name, tt := range map[string]struct {
		body []byte
		code string
		text string
	}{
		"a database from a newer isshoni": {
			alter(newer, `INSERT INTO schema_migrations (version, name, applied_at, app_version) VALUES (?, 'from_the_future', 0, '9.9.9')`, future),
			api.CodeBackupNewer, "This backup is from isshoni",
		},
		"a database of another build": {
			alter(other, `UPDATE schema_migrations SET name = 'something_else' WHERE version = 1`),
			api.CodeBackupInvalid, "a migration history that this isshoni does not know",
		},
		"an SQLite database that is not isshoni's": {
			alter(empty, `CREATE TABLE notes (body TEXT)`),
			api.CodeBackupInvalid, "this is not a backup that isshoni can restore",
		},
		"a text file": {[]byte("isshoni backup, honest"), api.CodeBackupInvalid, "neither a backup archive"},
	} {
		_, err := admin.Restore(ctx, bytes.NewReader(tt.body), ops.AdminContentSQLite)
		var ae *ops.AdminError
		if !errors.As(err, &ae) || ae.API.Code != tt.code || !strings.Contains(ae.Message, tt.text) {
			t.Errorf("%s: %v, want %s with %q", name, err, tt.code, tt.text)
		}
	}
	// Nothing happened: the server runs, with its state, and neither staging nor a pre-restore backup is left.
	if got := whoAmI(t, srv.Client, srv); got != adminName {
		t.Errorf("GET /me after the refused restores: %s", got)
	}
	if got := accountState(t, srv); !reflect.DeepEqual(got, before) {
		t.Errorf("the refused restores changed the state: %v", got)
	}
	if left := append(dirNames(t, paths.Restore), dirNames(t, paths.Backups)...); len(left) != 0 {
		t.Errorf("the refused restores left %v", left)
	}
	if h, err := admin.Health(ctx); err != nil || h.Status != ops.StatusOK {
		t.Errorf("health after the refused restores = %+v, %v", h, err)
	}
}

// TestRestoreCompletedAtStartup is the crash between a restore's plan and its swap (04 §12.4 "Crash safety"), with
// the real store: the next start finds the plan, swaps the files before it opens anything, and serves the restored
// state. A start that finds only staging removes it.
func TestRestoreCompletedAtStartup(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{Deps: wiredDeps()})
	paths := srv.Cfg.Paths()
	setUp(t, srv)
	want := accountState(t, srv)
	file := filepath.Join(t.TempDir(), "before.db")
	snapshotDB(t, srv, file)
	register(t, srv, "Sam")
	srv.Stop(t)

	// The first half of a restore, and then the process dies: the plan says "swapping" and nothing is swapped.
	data := &ops.DataDir{Paths: paths, DB: server.DBFiles(paths.Backups)}
	body, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	info, err := data.PrepareRestore(context.Background(), body, -1)
	_ = body.Close()
	if err != nil || info.Kind != ops.RestoreDatabase || info.SchemaVersion != store.LatestSchemaVersion() {
		t.Fatalf("PrepareRestore = %+v, %v", info, err)
	}
	if got := dirNames(t, paths.Restore); !slices.Equal(got, []string{"new", "plan.json"}) {
		t.Fatalf("restore/ before the start: %v", got)
	}

	srv.Restart(t)
	if got := accountState(t, srv); !reflect.DeepEqual(got, want) {
		t.Errorf("the state after the start that completed the restore:\n got %v\nwant %v", got, want)
	}
	if got := whoAmI(t, srv.Client, srv); got != adminName {
		t.Errorf("the admin after the start: %s", got)
	}
	if left := dirNames(t, paths.Restore); len(left) != 0 {
		t.Errorf("restore/ after the start: %v", left)
	}
	if logs := srv.Logs(); !strings.Contains(logs, `"msg":"a restore was interrupted before its files were swapped in; completing it now"`) ||
		!strings.Contains(logs, `"msg":"restore finished: removed the files from before the restore"`) {
		t.Error("the log does not tell of the completed restore")
	}

	// Staging without a plan is what a restore leaves that died before it was accepted.
	srv.Stop(t)
	if err := os.MkdirAll(filepath.Join(paths.Restore, "new"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Restore, "new", "isshoni.db"), []byte("half a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv.Restart(t)
	if left := dirNames(t, paths.Restore); len(left) != 0 {
		t.Errorf("restore/ after a start with leftover staging: %v", left)
	}
	if got := accountState(t, srv); !reflect.DeepEqual(got, want) {
		t.Error("leftover staging changed the state")
	}
	if !strings.Contains(srv.Logs(), `"msg":"removed what an unfinished restore or backup left in the staging directory"`) {
		t.Error("the log does not tell of the removed staging")
	}

	// A plan that can't be read stops the start: the data directory may be half swapped.
	srv.Stop(t)
	if err := os.WriteFile(filepath.Join(paths.Restore, "plan.json"), []byte(`{"ts":"20261014T021500Z","state":"swap`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = servertest.Try(t, servertest.Options{DataDir: srv.DataDir, Deps: wiredDeps()})
	if err == nil || !strings.Contains(err.Error(), "plan.json is the record of a restore, and it can't be used") {
		t.Errorf("Start with an unreadable plan: %v", err)
	}
	// No restart makes the plan readable: it is a refusal, on which serve exits 78 (04 §3.2).
	if !server.NeedsOperator(err) || !errors.Is(err, ops.ErrRestoreNeedsOperator) {
		t.Errorf("Start with an unreadable plan: %v, want a refusal that needs the operator", err)
	}
	if err := os.Remove(filepath.Join(paths.Restore, "plan.json")); err != nil {
		t.Fatal(err)
	}
	// It left nothing open: the data directory starts again once the plan is dealt with.
	servertest.Start(t, servertest.Options{DataDir: srv.DataDir, Deps: wiredDeps()})
}

// TestRestoreFinishedBeforeTheSocketAnswers: the start after a restore removes what that restore kept (04 §12.4
// step 7) before the admin socket takes its first request. `isshoni admin restore` returns as soon as that socket
// answers again, so the operator's next command, another restore for one, arrives right then. Were the cleanup
// still under way, it and the new restore would both work on restore/, and the cleanup could take the new
// restore's plan with it: a restore that was answered 202 and never happened.
func TestRestoreFinishedBeforeTheSocketAnswers(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{Deps: wiredDeps()})
	paths := srv.Cfg.Paths()
	setUp(t, srv)
	dir := t.TempDir()
	snapshot := func(name string) (map[string][][]string, []byte) {
		t.Helper()
		file := filepath.Join(dir, name)
		snapshotDB(t, srv, file)
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		return accountState(t, srv), body
	}
	alone, first := snapshot("first.db")
	register(t, srv, "Sam")
	withSam, second := snapshot("second.db")
	register(t, srv, "Robin")

	// A restore, as far as the end of its shutdown: the files are swapped, and those from before wait in
	// restore/old-<ts>/ for the next start. They get company, so that removing them takes a moment.
	admin, ctx := adminSocket(t, srv)
	if _, err := admin.Restore(ctx, bytes.NewReader(first), ops.AdminContentSQLite); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if err := srv.Wait(t); !errors.Is(err, server.ErrRestartRequested) {
		t.Fatalf("Run after a restore = %v, want ErrRestartRequested", err)
	}
	old, _ := filepath.Glob(filepath.Join(paths.Restore, "old-*"))
	if len(old) != 1 {
		t.Fatalf("restore/ after the restore's shutdown: %v, want one old-<ts>", dirNames(t, paths.Restore))
	}
	if err := os.Mkdir(filepath.Join(old[0], "more"), 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range 2000 {
		if err := os.WriteFile(filepath.Join(old[0], "more", strconv.Itoa(i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// A client that is at the socket before the server is: it is answered the moment the socket is served, and
	// looks at restore/ right then.
	type look struct {
		left []string
		err  error
	}
	answered := make(chan look, 1)
	go func() {
		for {
			_, err := admin.Health(ctx)
			if errors.Is(err, ops.ErrAdminNotRunning) {
				time.Sleep(time.Millisecond)
				continue
			}
			var left []string
			entries, _ := os.ReadDir(paths.Restore)
			for _, e := range entries {
				left = append(left, e.Name())
			}
			answered <- look{left, err}
			return
		}
	}()
	srv.Restart(t)
	if got := <-answered; got.err != nil || len(got.left) != 0 {
		t.Errorf("restore/ when the admin socket first answered: %v (health: %v), want the last restore finished", got.left, got.err)
	}
	if got := accountState(t, srv); !reflect.DeepEqual(got, alone) {
		t.Errorf("the state after the first restore:\n got %v\nwant %v", got, alone)
	}

	// The next restore, sent at once, is carried out like the first.
	restoreAndRestart(t, srv, second, ops.AdminContentSQLite)
	if got := accountState(t, srv); !reflect.DeepEqual(got, withSam) {
		t.Errorf("the state after the second restore:\n got %v\nwant %v", got, withSam)
	}
	if left := dirNames(t, paths.Restore); len(left) != 0 {
		t.Errorf("restore/ after the second restore: %v, want nothing", left)
	}
	logs := srv.Logs()
	for _, line := range []string{
		`"msg":"restore: the data directory holds the restored files; the previous ones are kept until the next successful start"`,
		`"msg":"restore finished: removed the files from before the restore"`,
	} {
		if n := strings.Count(logs, line); n != 2 {
			t.Errorf("the log has %d of %s, want one for each of the two restores", n, line)
		}
	}
}

// A restore whose shutdown is a stop (SIGTERM came first) is swapped in all the same: the plan stands, and the
// server that is started next serves the restored state.
func TestRestoreSwapsAtAnyShutdown(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{Deps: wiredDeps()})
	paths := srv.Cfg.Paths()
	setUp(t, srv)
	want := accountState(t, srv)
	file := filepath.Join(t.TempDir(), "before.db")
	snapshotDB(t, srv, file)
	register(t, srv, "Sam")

	// The plan is written while the server runs, as the socket's handler does, and then the server is stopped.
	data := &ops.DataDir{Paths: paths, DB: server.DBFiles(paths.Backups)}
	body, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	_, err = data.PrepareRestore(context.Background(), body, -1)
	_ = body.Close()
	if err != nil {
		t.Fatal(err)
	}
	srv.Stop(t) // Run returns nil: a stop
	if got := dirNames(t, paths.Restore); len(got) != 2 || got[1] != "plan.json" || !strings.HasPrefix(got[0], "old-") {
		t.Errorf("restore/ after the stop: %v, want the old files and the done plan", got)
	}
	if _, err := os.Stat(paths.DB + "-wal"); err == nil {
		t.Error("a WAL sits next to the restored database")
	}
	srv.Restart(t)
	if got := accountState(t, srv); !reflect.DeepEqual(got, want) {
		t.Errorf("the state after the stop and the next start:\n got %v\nwant %v", got, want)
	}
}

// secretIDs are the ids of the keys in secrets.json: a rotation moves a key's id on by one.
func secretIDs(t *testing.T, srv *servertest.Server) map[string]string {
	t.Helper()
	data, err := os.ReadFile(srv.Cfg.Paths().Secrets)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Keys  map[string]struct{ ID, Key string }
		VAPID struct {
			PublicKey string `json:"public_key"`
		}
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{"vapid": doc.VAPID.PublicKey}
	for name, k := range doc.Keys {
		out[name] = k.ID
	}
	return out
}

// rotateAndRestart asks the admin socket for a rotation, waits for the server to end as a rotation ends a server
// and starts the next one.
func rotateAndRestart(t *testing.T, srv *servertest.Server, in ops.AdminRotateRequest) ops.AdminRotateResult {
	t.Helper()
	admin, ctx := adminSocket(t, srv)
	res, err := admin.RotateSecrets(ctx, in)
	if err != nil {
		t.Fatalf("rotate-secrets: %v", err)
	}
	if !res.Restarting {
		t.Errorf("rotate-secrets answered %+v, want restarting", res)
	}
	if err := srv.Wait(t); !errors.Is(err, server.ErrRestartRequested) {
		t.Fatalf("Run after rotate-secrets = %v, want ErrRestartRequested", err)
	}
	srv.Restart(t)
	return res
}

// vapidPurges returns what the push service's VAPID fingerprint check deleted at each start that found a rotated key
// (04 §5.2), from the server's log so far: one count per such start, across restarts.
func vapidPurges(t *testing.T, srv *servertest.Server) []int {
	t.Helper()
	var out []int
	for line := range strings.Lines(srv.Logs()) {
		if !strings.Contains(line, `"msg":"the VAPID key changed: deleted every push subscription`) {
			continue
		}
		var rec struct {
			Component string `json:"component"`
			Count     int    `json:"count"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil || rec.Component != "push" {
			t.Fatalf("the purge's log line %q: %+v, %v", line, rec, err)
		}
		out = append(out, rec.Count)
	}
	return out
}

// infoVAPID is the VAPID public key that GET /api/v1/info publishes, "" when it has no push object.
func infoVAPID(t *testing.T, srv *servertest.Server) string {
	t.Helper()
	res := call(t, srv, http.MethodGet, "/api/v1/info", nil)
	var info api.Info
	if res.status != http.StatusOK || json.Unmarshal([]byte(res.body), &info) != nil {
		t.Fatalf("GET /info = %d %s", res.status, res.body)
	}
	if info.Push == nil {
		return ""
	}
	return info.Push.VAPIDPublicKey
}

// TestRotateSecrets is the rotation of 04 §17 as `isshoni admin rotate-secrets` asks for it, all four: the server
// restarts, and before the next one serves its first request the web sessions, the device codes and the invites
// are gone, and the push subscriptions twice over: the push service, built first, deletes them all because the
// VAPID key changed (04 §5.2), and the session rotation would have taken them with the sessions anyway.
func TestRotateSecrets(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{Deps: wiredDeps()})
	setUp(t, srv)
	sam := register(t, srv, "Sam")
	seedPush(t, srv)
	invite := inviteToken(t, srv)
	if res := call(t, srv, http.MethodPost, "/api/v1/auth/invite/check", api.TokenRequest{Token: invite}); res.status != http.StatusOK {
		t.Fatalf("the invite before the rotation: %d %s", res.status, res.body)
	}
	if n := [4]int{count(t, srv, "sessions"), count(t, srv, "device_codes"), count(t, srv, "invites"), count(t, srv, "push_subscriptions")}; n != [4]int{2, 1, 2, 1} {
		t.Fatalf("rows before the rotation (sessions, device codes, invites, push): %v", n)
	}
	before := secretIDs(t, srv)
	if got := infoVAPID(t, srv); got != before["vapid"] || got == "" {
		t.Fatalf("GET /info publishes the VAPID key %q, secrets.json has %q", got, before["vapid"])
	}
	fingerprint := query(t, srv, true, `SELECT value FROM meta WHERE key = 'vapid_key_fp'`)
	if len(fingerprint) != 1 || len(vapidPurges(t, srv)) != 0 {
		t.Fatalf("the first start: VAPID fingerprint %v, purges %v; want a fingerprint and no purge", fingerprint,
			vapidPurges(t, srv))
	}
	ws := dialWS(t, srv)
	joinLounge(t, ws, signaltest.DefaultHello())

	res := rotateAndRestart(t, srv, ops.AdminRotateRequest{Keys: []string{"session", "invite", "resume"}, VAPID: true})
	if !slices.Equal(res.Rotated, []string{"session", "invite", "resume", "vapid"}) {
		t.Errorf("rotated = %v", res.Rotated)
	}
	if code := closeCode(t, ws); code != websocket.StatusServiceRestart {
		t.Errorf("the WebSocket closed with %d at the rotation, want 1012", code)
	}
	after := secretIDs(t, srv)
	for _, name := range []string{"session", "invite", "resume"} {
		if before[name] != "1" || after[name] != "2" {
			t.Errorf("key %s: id %q before, %q after; want 1 and 2", name, before[name], after[name])
		}
	}
	if after["vapid"] == before["vapid"] || after["vapid"] == "" {
		t.Error("the VAPID pair was not replaced")
	}
	// The purge is done when the server is ready: these are the first looks at it.
	if n := [4]int{count(t, srv, "sessions"), count(t, srv, "device_codes"), count(t, srv, "invites"), count(t, srv, "push_subscriptions")}; n != [4]int{} {
		t.Errorf("rows after the rotation (sessions, device codes, invites, push): %v, want none", n)
	}
	// The push service came first, so it found the subscription and deleted it for the new VAPID key; it keeps that
	// key's fingerprint, and the browsers subscribe again with the key that GET /info now publishes.
	if got := vapidPurges(t, srv); !slices.Equal(got, []int{1}) {
		t.Errorf("the VAPID purges in the log: %v, want one of 1 subscription", got)
	}
	if got := query(t, srv, true, `SELECT value FROM meta WHERE key = 'vapid_key_fp'`); len(got) != 1 || reflect.DeepEqual(got, fingerprint) {
		t.Errorf("the VAPID fingerprint %v after the rotation, %v before; want a new one", got, fingerprint)
	}
	if got := infoVAPID(t, srv); got != after["vapid"] {
		t.Errorf("GET /info publishes the VAPID key %q after the rotation, want the new %q", got, after["vapid"])
	}
	if got := whoAmI(t, srv.Client, srv); got != api.CodeUnauthenticated {
		t.Errorf("the admin's session after the rotation: %s", got)
	}
	if got := whoAmI(t, sam, srv); got != api.CodeUnauthenticated {
		t.Errorf("Sam's session after the rotation: %s", got)
	}
	if res := call(t, srv, http.MethodPost, "/api/v1/auth/invite/check", api.TokenRequest{Token: invite}); res.status == http.StatusOK {
		t.Errorf("the invite still works after the rotation: %s", res.body)
	}
	// The accounts themselves are untouched, and the rotation is in the audit log (03 §4.6).
	if n := count(t, srv, "users"); n != 2 {
		t.Errorf("%d users after the rotation, want 2", n)
	}
	audit := query(t, srv, true, `SELECT actor_kind, detail FROM audit_log WHERE action = 'secrets.rotated'`)
	if len(audit) != 1 || audit[0][0] != "system" || !strings.Contains(audit[0][1], "session") || !strings.Contains(audit[0][1], "invite") {
		t.Errorf("audit rows for the rotation: %v", audit)
	}
	// Signing in again works with the new keys.
	wantStatus(t, "login after the rotation", call(t, srv, http.MethodPost, "/api/v1/auth/login",
		api.LoginRequest{Username: adminName, Password: adminPassword}), http.StatusOK, "")
	if got := whoAmI(t, srv.Client, srv); got != adminName {
		t.Errorf("the admin after signing in again: %s", got)
	}
	logs := srv.Logs()
	if !strings.Contains(logs, `"msg":"secrets rotated through the admin socket; restarting to apply them"`) ||
		!strings.Contains(logs, `"msg":"shutting down","reason":"restart"`) {
		t.Error("the log does not tell of the rotation and its restart")
	}
}

// TestRotateExactlyTheNamedKeys: a rotation invalidates exactly the tokens of the keys it names (04 §5.2). The
// invite key takes the links and leaves the sessions; the session key takes the sessions and leaves the links;
// the resume key takes only the resume tokens; the VAPID pair takes the push subscriptions and leaves the sessions.
func TestRotateExactlyTheNamedKeys(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{Deps: wiredDeps()})
	setUp(t, srv)
	seedPush(t, srv)
	inviteWorks := func(token string) bool {
		t.Helper()
		return call(t, srv, http.MethodPost, "/api/v1/auth/invite/check", api.TokenRequest{Token: token}).status == http.StatusOK
	}

	// invite: the links go, the session stays.
	invite := inviteToken(t, srv)
	rotateAndRestart(t, srv, ops.AdminRotateRequest{Keys: []string{"invite"}})
	if inviteWorks(invite) {
		t.Error("an invite survived the rotation of the invite key")
	}
	if got := whoAmI(t, srv.Client, srv); got != adminName {
		t.Errorf("the session after the rotation of the invite key: %s, want it to work", got)
	}
	if n := [2]int{count(t, srv, "sessions"), count(t, srv, "push_subscriptions")}; n != [2]int{1, 1} {
		t.Errorf("sessions and push subscriptions after the rotation of the invite key: %v", n)
	}
	if ids := secretIDs(t, srv); ids["invite"] != "2" || ids["session"] != "1" || ids["resume"] != "1" {
		t.Errorf("key ids = %v", ids)
	}

	// resume: neither goes. (What that key protects, a connection's resume token, never outlives a restart
	// anyway: the connection it would resume is gone with the process.)
	invite = inviteToken(t, srv)
	rotateAndRestart(t, srv, ops.AdminRotateRequest{Keys: []string{"resume"}})
	if !inviteWorks(invite) || whoAmI(t, srv.Client, srv) != adminName || count(t, srv, "push_subscriptions") != 1 {
		t.Error("the rotation of the resume key took an invite, a session or a push subscription")
	}

	// vapid alone: the push subscriptions go, made for a key that is no more (04 §5.2), and nothing else does.
	vapid := secretIDs(t, srv)["vapid"]
	if len(vapidPurges(t, srv)) != 0 {
		t.Errorf("VAPID purges before the VAPID pair was rotated: %v", vapidPurges(t, srv))
	}
	rotateAndRestart(t, srv, ops.AdminRotateRequest{VAPID: true})
	if secretIDs(t, srv)["vapid"] == vapid || !inviteWorks(invite) || whoAmI(t, srv.Client, srv) != adminName {
		t.Error("the rotation of the VAPID pair: the pair did not change, or an invite or a session went with it")
	}
	if n := [2]int{count(t, srv, "sessions"), count(t, srv, "push_subscriptions")}; n != [2]int{1, 0} {
		t.Errorf("sessions and push subscriptions after the rotation of the VAPID pair: %v, want the session alone", n)
	}
	if got := vapidPurges(t, srv); !slices.Equal(got, []int{1}) {
		t.Errorf("the VAPID purges in the log: %v, want one of 1 subscription", got)
	}
	// A subscription for the new key, which the session rotation below takes with the session (seedPush's device code
	// is still there: only the session key takes those).
	query(t, srv, false, `DELETE FROM device_codes`)
	seedPush(t, srv)

	// session: the session and its push subscription go, the invite stays.
	rotateAndRestart(t, srv, ops.AdminRotateRequest{Keys: []string{"session"}})
	if got := whoAmI(t, srv.Client, srv); got != api.CodeUnauthenticated {
		t.Errorf("the session after the rotation of the session key: %s", got)
	}
	if n := [2]int{count(t, srv, "sessions"), count(t, srv, "push_subscriptions")}; n != [2]int{} {
		t.Errorf("sessions and push subscriptions after the rotation of the session key: %v", n)
	}
	if got := vapidPurges(t, srv); !slices.Equal(got, []int{1}) {
		t.Errorf("the VAPID purges in the log at the end: %v, want only the one of the VAPID rotation", got)
	}
	if !inviteWorks(invite) {
		t.Error("the invite went with the session key")
	}
	if ids := secretIDs(t, srv); ids["invite"] != "2" || ids["session"] != "2" || ids["resume"] != "2" {
		t.Errorf("key ids at the end = %v", ids)
	}
}

// What the socket refuses of a rotation changes nothing and restarts nothing.
func TestRotateSecretsRefused(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{Deps: wiredDeps()})
	setUp(t, srv)
	before := secretIDs(t, srv)
	admin, ctx := adminSocket(t, srv)
	for _, in := range []ops.AdminRotateRequest{{}, {Keys: []string{"session", "master"}}} {
		_, err := admin.RotateSecrets(ctx, in)
		var ae *ops.AdminError
		if !errors.As(err, &ae) || ae.API.Code != api.CodeBadRequest {
			t.Errorf("rotate-secrets %+v: %v, want bad_request", in, err)
		}
	}
	if got := secretIDs(t, srv); !reflect.DeepEqual(got, before) {
		t.Errorf("refused rotations changed secrets.json: %v", got)
	}
	if got := whoAmI(t, srv.Client, srv); got != adminName {
		t.Errorf("GET /me after the refused rotations: %s", got)
	}
}

// A server without a site (ip mode before it knows its address) has no accounts, but it has a data directory:
// backup and restore work on its admin socket too (04 §6.6).
func TestBackupWithoutASite(t *testing.T) {
	cfg := testConfig(t, tlsFlags("--tls.mode=ip", closedCA)...)
	deps := testDeps()
	deps.Argon = testArgon
	startDirect(t, cfg, deps, netx.PublicAddrs{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	b, err := ops.DialAdmin(cfg.Listen.AdminSocket).Backup(ctx, true)
	if err != nil {
		t.Fatalf("backup on a server without a site: %v", err)
	}
	defer func() { _ = b.Close() }()
	m, err := ops.ReadManifest(b)
	if err != nil || m.SchemaVersion != store.LatestSchemaVersion() || !slices.Contains(m.Contents, "secrets.json") {
		t.Errorf("manifest = %+v, %v", m, err)
	}
}
