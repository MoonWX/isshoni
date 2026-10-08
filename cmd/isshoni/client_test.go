package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/ops"
)

// wireTime matches a timestamp in the API's fixed form (03 §3.3).
const wireTime = `\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z`

// setup-url on a terminal prints the block of 04 §3.3: the heading, the indented link, the QR code, the note.
func TestSetupURLOnATerminal(t *testing.T) {
	_, env := startTestServer(t, false)
	code, stdout, stderr := runCLITTY(t, true, env, "setup-url")
	if code != exitOK || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	lines := strings.Split(stdout, "\n")
	if lines[0] != "Open this link in your browser to create the admin account (valid 24 hours, works once):" ||
		lines[1] != "" || lines[2] != "  https://watch.example.com/setup#token1" || lines[3] != "" {
		t.Errorf("the block starts with:\n%s", strings.Join(lines[:min(4, len(lines))], "\n"))
	}
	if !strings.HasSuffix(stdout, "\n\nRunning `isshoni setup-url` again makes a new link and cancels this one.\n") {
		t.Errorf("the block does not end with the note:\n%s", stdout)
	}
	// The QR code sits between the link and the note, indented like the link, and is the link.
	qr := lines[4 : len(lines)-3]
	for i, l := range qr {
		if !strings.HasPrefix(l, "  ") {
			t.Fatalf("QR line %d is not indented: %q", i, l)
		}
		qr[i] = strings.TrimPrefix(l, "  ")
	}
	if got, want := strings.Join(qr, "\n")+"\n", mustQR(t, "https://watch.example.com/setup#token1"); got != want {
		t.Errorf("the QR code is not that of the link:\n%s\nwant:\n%s", got, want)
	}

	// --no-qr leaves it out.
	code, stdout, _ = runCLITTY(t, true, env, "setup-url", "--no-qr")
	want := "Open this link in your browser to create the admin account (valid 24 hours, works once):\n\n" +
		"  https://watch.example.com/setup#token2\n\n" +
		"Running `isshoni setup-url` again makes a new link and cancels this one.\n"
	if code != exitOK || stdout != want {
		t.Errorf("--no-qr: exit %d, stdout:\n%s\nwant:\n%s", code, stdout, want)
	}
}

func mustQR(t *testing.T, text string) string {
	t.Helper()
	s, err := qrText(text)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Off a terminal stdout holds the link alone, for scripts and install.sh; --qr adds the QR code there, and --json
// prints the one document of 04 §3.3.
func TestSetupURLPiped(t *testing.T) {
	f, env := startTestServer(t, false)
	code, stdout, stderr := runCLIEnv(t, env, "setup-url")
	if code != exitOK || stdout != "https://watch.example.com/setup#token1\n" {
		t.Errorf("exit %d, stdout %q; want the link alone", code, stdout)
	}
	if want := "Open this link in your browser to create the admin account (valid 24 hours, works once):\n" +
		"Running `isshoni setup-url` again makes a new link and cancels this one.\n"; stderr != want {
		t.Errorf("stderr %q, want %q", stderr, want)
	}

	code, stdout, _ = runCLIEnv(t, env, "setup-url", "--qr")
	if want := "https://watch.example.com/setup#token2\n" + mustQR(t, "https://watch.example.com/setup#token2"); code != exitOK || stdout != want {
		t.Errorf("--qr: exit %d, stdout:\n%s\nwant:\n%s", code, stdout, want)
	}

	code, stdout, stderr = runCLIEnv(t, env, "setup-url", "--json")
	if !regexp.MustCompile(`^\{"url":"https://watch\.example\.com/setup#token3","expiresAt":"`+wireTime+`","tlsReady":true\}\n$`).MatchString(stdout) ||
		code != exitOK || stderr != "" {
		t.Errorf("--json: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	var doc ops.AdminSetupURL
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatal(err)
	}
	if d := time.Until(time.Time(doc.ExpiresAt)); d < 23*time.Hour || d > 24*time.Hour {
		t.Errorf("expiresAt is %v from now, want 24 h", d)
	}

	// The certificate is not there yet: the link is printed all the same, with the note (04 §3.3).
	f.tlsReady.Store(false)
	const certNote = "The certificate isn't ready yet. The link works once it is; check with `isshoni doctor`.\n"
	code, stdout, stderr = runCLIEnv(t, env, "setup-url", "--json")
	if code != exitOK || !strings.Contains(stdout, `"tlsReady":false`) || stderr != certNote {
		t.Errorf("--json with a pending certificate: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	code, stdout, stderr = runCLIEnv(t, env, "setup-url")
	if code != exitOK || stdout != "https://watch.example.com/setup#token5\n" || !strings.Contains(stderr, "\n"+certNote) {
		t.Errorf("pending certificate: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	_, stdout, _ = runCLITTY(t, true, env, "setup-url", "--no-qr")
	if !strings.HasSuffix(stdout, "\n\n"+certNote+"Running `isshoni setup-url` again makes a new link and cancels this one.\n") {
		t.Errorf("on a terminal the note is missing:\n%s", stdout)
	}

	// --json --qr: the document stays alone on stdout, the code goes to stderr.
	f.tlsReady.Store(true)
	code, stdout, stderr = runCLIEnv(t, env, "setup-url", "--json", "--qr")
	if code != exitOK || strings.Count(stdout, "\n") != 1 || stderr != mustQR(t, "https://watch.example.com/setup#token7") {
		t.Errorf("--json --qr: exit %d, stdout %q, stderr:\n%s", code, stdout, stderr)
	}
}

// After the setup the link is gone: exit 7 with the reset-password fix, and nothing on stdout (04 §12.5).
func TestSetupURLAfterSetup(t *testing.T) {
	f, env := startTestServer(t, false)
	f.accounts.completeSetup()
	want := "isshoni setup-url: setup is already done: an admin account exists, so there is no setup link\n" +
		"  fix: use `isshoni admin users reset-password <name>` to get back into an admin account\n"
	for _, args := range [][]string{{"setup-url"}, {"setup-url", "--json"}, {"setup-url", "--qr", "--wait", "30s"}} {
		start := time.Now()
		code, stdout, stderr := runCLIEnv(t, env, args...)
		if code != exitRefused || stdout != "" || stderr != want {
			t.Errorf("isshoni %s: exit %d, stdout %q, stderr %q; want exit 7 and %q", strings.Join(args, " "), code, stdout, stderr, want)
		}
		if time.Since(start) > 10*time.Second {
			t.Errorf("isshoni %s waited although the setup is done", strings.Join(args, " "))
		}
	}
}

// --wait: the CLI keeps trying while no server listens yet, and the server waits for readiness before it mints the
// link.
func TestSetupURLWait(t *testing.T) {
	t.Parallel() // the cases wait; each has a server and a socket of its own
	t.Run("the server starts later", func(t *testing.T) {
		t.Parallel()
		sock := socketPath(t)
		started := make(chan *fakeServer, 1)
		timer := time.AfterFunc(700*time.Millisecond, func() {
			f, err := startFakeServer(sock, false)
			if err != nil {
				t.Errorf("starting the fake server: %v", err)
			}
			started <- f
		})
		defer func() {
			if timer.Stop() { // it never ran
				return
			}
			if f := <-started; f != nil {
				_ = f.stop()
			}
		}()
		code, stdout, stderr := runCLIEnv(t, []string{"ISSHONI_LISTEN_ADMIN_SOCKET=" + sock}, "setup-url", "--wait", "30s", "--json")
		if code != exitOK || !strings.Contains(stdout, `"url":"https://watch.example.com/setup#token1"`) {
			t.Errorf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
		}
	})
	t.Run("the certificate arrives later", func(t *testing.T) {
		t.Parallel()
		f, env := startTestServer(t, false)
		f.tlsReady.Store(false)
		timer := time.AfterFunc(600*time.Millisecond, func() { f.tlsReady.Store(true) })
		defer timer.Stop()
		start := time.Now()
		code, stdout, stderr := runCLIEnv(t, env, "setup-url", "--wait", "30s", "--json")
		if code != exitOK || !strings.Contains(stdout, `"tlsReady":true`) || stderr != "" {
			t.Errorf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		if d := time.Since(start); d < 500*time.Millisecond || d > 10*time.Second {
			t.Errorf("took %v, want it to wait for the certificate and no longer", d)
		}
	})
	t.Run("the wait runs out", func(t *testing.T) {
		t.Parallel()
		f, env := startTestServer(t, false)
		f.tlsReady.Store(false)
		code, stdout, stderr := runCLIEnv(t, env, "setup-url", "--wait", "1s", "--json")
		if code != exitOK || !strings.Contains(stdout, `"tlsReady":false`) || !strings.Contains(stderr, "The certificate isn't ready yet.") {
			t.Errorf("exit %d, stdout %q, stderr %q; want the link with the note", code, stdout, stderr)
		}
	})
	t.Run("a long wait says so", func(t *testing.T) {
		if testing.Short() {
			t.Skip("waits 3 s")
		}
		t.Parallel()
		f, env := startTestServer(t, false)
		f.tlsReady.Store(false)
		code, _, stderr := runCLIEnv(t, env, "setup-url", "--wait", "3s", "--json")
		if code != exitOK || !strings.HasPrefix(stderr, "Waiting for the server to be ready (up to 3s)…\n") {
			t.Errorf("exit %d, stderr %q", code, stderr)
		}
	})
	t.Run("no server ever", func(t *testing.T) {
		t.Parallel()
		start := time.Now()
		code, stdout, stderr := runCLI(t, "setup-url", "--wait", "1s")
		if code != exitUnreachable || stdout != "" || !strings.HasPrefix(stderr, "isshoni is not running (no server on ") {
			t.Errorf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		if d := time.Since(start); d < time.Second || d > 10*time.Second {
			t.Errorf("gave up after %v, want after the 1 s wait", d)
		}
	})
}

// The permission case of 04 §12.1, §17: exit 4 with the sudo message that repeats the command as typed;
// healthcheck prints it too and exits 1.
func TestPermissionDenied(t *testing.T) {
	check := func(t *testing.T, env []string, sock string) {
		t.Helper()
		tests := []struct {
			args []string
			code int
			sudo string
		}{
			{[]string{"setup-url"}, exitUnreachable, "sudo isshoni setup-url"},
			{[]string{"setup-url", "--json", "--wait", "30s"}, exitUnreachable, "sudo isshoni setup-url --json --wait 30s"},
			{[]string{"admin", "status"}, exitUnreachable, "sudo isshoni admin status"},
			{[]string{"admin", "users", "list", "--json"}, exitUnreachable, "sudo isshoni admin users list --json"},
			{[]string{"admin", "users", "set-role", "o'brien x", "admin"}, exitUnreachable, `sudo isshoni admin users set-role 'o'\''brien x' admin`},
			{[]string{"admin", "invite", "create", "--uses", "3"}, exitUnreachable, "sudo isshoni admin invite create --uses 3"},
			{[]string{"admin", "log-level", "debug"}, exitUnreachable, "sudo isshoni admin log-level debug"},
			{[]string{"healthcheck"}, exitRuntime, "sudo isshoni healthcheck"},
			{[]string{"healthcheck", "--ready", "--wait", "30s"}, exitRuntime, "sudo isshoni healthcheck --ready --wait 30s"},
		}
		for _, tt := range tests {
			start := time.Now()
			code, stdout, stderr := runCLIEnv(t, env, tt.args...)
			want := "Permission denied on " + sock + ": run it with sudo (" + tt.sudo + ")\n"
			if code != tt.code || stdout != "" || stderr != want {
				t.Errorf("isshoni %s: exit %d, stdout %q, stderr %q; want exit %d and %q",
					strings.Join(tt.args, " "), code, stdout, stderr, tt.code, want)
			}
			if time.Since(start) > 10*time.Second {
				t.Errorf("isshoni %s kept waiting; waiting does not help against a refusal", strings.Join(tt.args, " "))
			}
		}
	}
	t.Run("the server's peer-credential filter rejects the caller", func(t *testing.T) {
		if !peerCreds {
			t.Skipf("no peer credentials on %s", runtime.GOOS)
		}
		f, env := startTestServer(t, true)
		check(t, env, f.path)
	})
	t.Run("a socket directory the caller can't enter", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs a user that file modes apply to")
		}
		f, env := startTestServer(t, false)
		if err := os.Chmod(filepath.Dir(f.path), 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Dir(f.path), 0o700) }) //nolint:gosec // G302: a directory needs its x bit
		check(t, env, f.path)
	})
}

func TestHealthcheck(t *testing.T) {
	t.Parallel() // it waits
	f, env := startTestServer(t, false)
	for args, want := range map[string]string{"healthcheck": "ok\n", "healthcheck --ready": "ready\n", "healthcheck --ready --wait 5s": "ready\n"} {
		if code, stdout, stderr := runCLIEnv(t, env, strings.Fields(args)...); code != exitOK || stdout != want || stderr != "" {
			t.Errorf("isshoni %s: exit %d, stdout %q, stderr %q; want exit 0 and %q", args, code, stdout, stderr, want)
		}
	}

	// Alive but not ready: liveness passes, readiness names the check that holds it back.
	f.tlsReady.Store(false)
	if code, stdout, _ := runCLIEnv(t, env, "healthcheck"); code != exitOK || stdout != "ok\n" {
		t.Errorf("liveness while not ready: exit %d, stdout %q", code, stdout)
	}
	code, stdout, stderr := runCLIEnv(t, env, "healthcheck", "--ready")
	if want := "isshoni healthcheck: not ready: tls: waiting: obtaining certificate for 203.0.113.7\n"; code != exitRuntime || stdout != "" || stderr != want {
		t.Errorf("--ready while not ready: exit %d, stdout %q, stderr %q; want exit 1 and %q", code, stdout, stderr, want)
	}

	// --wait polls until the server is ready, and gives up when the time is over.
	timer := time.AfterFunc(1200*time.Millisecond, func() { f.tlsReady.Store(true) })
	defer timer.Stop()
	start := time.Now()
	if code, stdout, _ := runCLIEnv(t, env, "healthcheck", "--ready", "--wait", "30s"); code != exitOK || stdout != "ready\n" {
		t.Errorf("--ready --wait: exit %d, stdout %q", code, stdout)
	}
	if d := time.Since(start); d < time.Second || d > 15*time.Second {
		t.Errorf("--wait returned after %v, want once the server was ready", d)
	}
	f.tlsReady.Store(false)
	start = time.Now()
	if code, _, stderr := runCLIEnv(t, env, "healthcheck", "--ready", "--wait", "1500ms"); code != exitRuntime || !strings.Contains(stderr, "not ready: tls:") {
		t.Errorf("--wait that runs out: exit %d, stderr %q", code, stderr)
	}
	if d := time.Since(start); d < 1500*time.Millisecond || d > 10*time.Second {
		t.Errorf("--wait 1500ms gave up after %v", d)
	}

	// A server that shuts down is not healthy.
	f.health.SetShuttingDown()
	for _, args := range [][]string{{"healthcheck"}, {"healthcheck", "--ready"}} {
		code, stdout, stderr := runCLIEnv(t, env, args...)
		if want := "isshoni healthcheck: the server is shutting down\n"; code != exitRuntime || stdout != "" || stderr != want {
			t.Errorf("isshoni %s during shutdown: exit %d, stdout %q, stderr %q", strings.Join(args, " "), code, stdout, stderr)
		}
	}
}

func TestHealthText(t *testing.T) {
	tests := []struct {
		h    ops.AdminHealth
		want string
	}{
		{ops.AdminHealth{Status: ops.StatusShuttingDown}, "the server is shutting down"},
		{ops.AdminHealth{Status: ops.StatusNotReady}, "not ready"},
		{ops.AdminHealth{Status: ops.StatusNotReady, Checks: map[string]string{"db": "ok", "tls": "waiting", "media": "not ready"}}, "not ready: media: not ready; tls: waiting"},
		{ops.AdminHealth{Status: ops.StatusNotReady, Checks: map[string]string{"maintenance": "restore"}}, "not ready: maintenance: restore"},
	}
	for _, tt := range tests {
		if got := healthText(tt.h); got != tt.want {
			t.Errorf("healthText(%+v) = %q, want %q", tt.h, got, tt.want)
		}
	}
}

func TestUsersCommands(t *testing.T) {
	f, env := startTestServer(t, false)

	// Before the setup there are no accounts.
	code, stdout, _ := runCLIEnv(t, env, "admin", "users", "list")
	if code != exitOK || !strings.HasPrefix(stdout, "No accounts yet.") {
		t.Errorf("users list without users: exit %d, stdout %q", code, stdout)
	}
	if code, stdout, _ := runCLIEnv(t, env, "admin", "users", "list", "--json"); code != exitOK || stdout != `{"users":[]}`+"\n" {
		t.Errorf("users list --json without users: exit %d, stdout %q", code, stdout)
	}

	f.accounts.completeSetup()
	code, stdout, stderr := runCLIEnv(t, env, "admin", "users", "list", "--json")
	wantJSON := `{"users":[` +
		`{"id":"0123456789ab","username":"alex","role":"admin","status":"active","createdAt":"2026-09-30T10:00:00.000Z","lastSeenAt":"2026-10-01T12:00:00.000Z"},` +
		`{"id":"cdefghjkmnpq","username":"sam","role":"user","status":"active","createdAt":"2026-10-01T08:30:00.000Z"}]}` + "\n"
	if code != exitOK || stdout != wantJSON || stderr != "" {
		t.Errorf("users list --json: exit %d, stderr %q\n got %s\nwant %s", code, stderr, stdout, wantJSON)
	}
	code, stdout, _ = runCLIEnv(t, env, "admin", "users", "list")
	wantTable := "NAME  ROLE   STATUS  CREATED               LAST SEEN\n" +
		"alex  admin  active  2026-09-30 10:00 UTC  2026-10-01 12:00 UTC\n" +
		"sam   user   active  2026-10-01 08:30 UTC  never\n"
	if code != exitOK || stdout != wantTable {
		t.Errorf("users list: exit %d\n got:\n%s\nwant:\n%s", code, stdout, wantTable)
	}

	// set-role, with the last-admin rule (exit 7) and an unknown user (exit 1).
	if code, stdout, _ := runCLIEnv(t, env, "admin", "users", "set-role", "sam", "admin"); code != exitOK || stdout != "sam is now an admin.\n" {
		t.Errorf("set-role admin: exit %d, stdout %q", code, stdout)
	}
	if code, stdout, _ := runCLIEnv(t, env, "admin", "users", "set-role", "sam", "user"); code != exitOK || stdout != "sam is now a regular user.\n" {
		t.Errorf("set-role user: exit %d, stdout %q", code, stdout)
	}
	code, stdout, stderr = runCLIEnv(t, env, "admin", "users", "set-role", "alex", "user")
	wantLast := `"alex" is the last active admin, and the server must keep one` + "\n" +
		"  fix: make another user an admin first: `isshoni admin users set-role <name> admin`\n"
	if code != exitRefused || stdout != "" || stderr != "isshoni admin users set-role: "+wantLast {
		t.Errorf("set-role on the last admin: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	code, stdout, stderr = runCLIEnv(t, env, "admin", "users", "set-role", "nobody", "admin")
	if want := "isshoni admin users set-role: there is no user named \"nobody\"\n  fix: `isshoni admin users list` shows the accounts\n"; code != exitRuntime || stdout != "" || stderr != want {
		t.Errorf("set-role on an unknown user: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	// disable and enable.
	if code, stdout, _ := runCLIEnv(t, env, "admin", "users", "disable", "sam"); code != exitOK || stdout != "sam can no longer sign in and is signed out everywhere.\n" {
		t.Errorf("disable: exit %d, stdout %q", code, stdout)
	}
	if _, stdout, _ := runCLIEnv(t, env, "admin", "users", "list", "--json"); !strings.Contains(stdout, `"username":"sam","role":"user","status":"disabled"`) {
		t.Errorf("sam is not disabled: %s", stdout)
	}
	if code, stdout, _ := runCLIEnv(t, env, "admin", "users", "enable", "sam"); code != exitOK || stdout != "sam can sign in again.\n" {
		t.Errorf("enable: exit %d, stdout %q", code, stdout)
	}
	if code, _, stderr := runCLIEnv(t, env, "admin", "users", "disable", "alex"); code != exitRefused || stderr != "isshoni admin users disable: "+wantLast {
		t.Errorf("disable the last admin: exit %d, stderr %q", code, stderr)
	}
	if code, _, stderr := runCLIEnv(t, env, "admin", "users", "enable", "nobody"); code != exitRuntime || !strings.Contains(stderr, `no user named "nobody"`) {
		t.Errorf("enable an unknown user: exit %d, stderr %q", code, stderr)
	}

	// reset-password prints the link, and says that it has revoked everything.
	code, stdout, stderr = runCLIEnv(t, env, "admin", "users", "reset-password", "alex")
	if code != exitOK || stdout != "https://watch.example.com/reset#reset-alex\n" {
		t.Errorf("reset-password: exit %d, stdout %q", code, stdout)
	}
	if !strings.HasPrefix(stderr, "Password-reset link for alex (valid 24 hours, works once):\n") ||
		!strings.Contains(stderr, "A password reset revokes everything") || !strings.Contains(stderr, "signed out") {
		t.Errorf("reset-password: stderr %q", stderr)
	}
	code, stdout, stderr = runCLITTY(t, true, env, "admin", "users", "reset-password", "alex")
	want := "Password-reset link for alex (valid 24 hours, works once):\n\n  https://watch.example.com/reset#reset-alex\n\n" +
		"A password reset revokes everything: alex's old password no longer works, and alex is signed out\n" +
		"everywhere (browsers, devices and push notifications).\n"
	if code != exitOK || stdout != want || stderr != "" {
		t.Errorf("reset-password on a terminal: exit %d, stderr %q, stdout:\n%s\nwant:\n%s", code, stderr, stdout, want)
	}
	if code, stdout, stderr := runCLIEnv(t, env, "admin", "users", "reset-password", "nobody"); code != exitRuntime || stdout != "" || !strings.Contains(stderr, `no user named "nobody"`) {
		t.Errorf("reset-password for an unknown user: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

// invite create sends only the flags the operator set (04 §12.5).
func TestInviteCreate(t *testing.T) {
	f, env := startTestServer(t, false)
	code, stdout, stderr := runCLIEnv(t, env, "admin", "invite", "create")
	if code != exitOK || stdout != "https://watch.example.com/invite#invite1\n" || stderr != "Invite link (valid 7 days):\n" {
		t.Errorf("invite create: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	code, stdout, stderr = runCLIEnv(t, env, "admin", "invite", "create", "--uses", "10", "--ttl", "48h")
	if code != exitOK || stdout != "https://watch.example.com/invite#invite2\n" || stderr != "Invite link (valid 48 hours):\n" {
		t.Errorf("invite create --uses 10 --ttl 48h: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	runCLIEnv(t, env, "admin", "invite", "create", "--ttl", "720h")
	runCLIEnv(t, env, "admin", "invite", "create", "--uses", "1")
	code, stdout, _ = runCLITTY(t, true, env, "admin", "invite", "create", "--ttl", "1h")
	if want := "Invite link (valid 1 hour):\n\n  https://watch.example.com/invite#invite5\n"; code != exitOK || stdout != want {
		t.Errorf("invite create on a terminal: exit %d, stdout %q, want %q", code, stdout, want)
	}
	f.accounts.mu.Lock()
	got := strings.Join(f.accounts.invites, "; ")
	f.accounts.closed = true
	f.accounts.mu.Unlock()
	if want := "uses=0 ttlHours=0; uses=10 ttlHours=48; uses=0 ttlHours=720; uses=1 ttlHours=0; uses=0 ttlHours=1"; got != want {
		t.Errorf("the server got %q, want %q", got, want)
	}

	code, stdout, stderr = runCLIEnv(t, env, "admin", "invite", "create")
	want := "isshoni admin invite create: registration is closed, so an invite link would not work\n" +
		"  fix: set the registration mode to invite or open first (Admin → Settings, or registration.mode in the config)\n"
	if code != exitRefused || stdout != "" || stderr != want {
		t.Errorf("invite create with registration closed: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

// log-level changes the level for --for, then the configured one is back (04 §3.1).
func TestLogLevel(t *testing.T) {
	t.Parallel() // it waits
	f, env := startTestServer(t, false)
	code, stdout, stderr := runCLIEnv(t, env, "admin", "log-level", "debug")
	if want := "The log level is debug for 30m, then back to the configured level.\n"; code != exitOK || stdout != want || stderr != "" {
		t.Errorf("log-level debug: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if level, until := f.logLevel.Level(); level.String() != "DEBUG" || time.Until(until) < 29*time.Minute || time.Until(until) > 30*time.Minute {
		t.Errorf("the server's level is %v until %v, want debug for 30 minutes", level, until)
	}
	code, stdout, _ = runCLIEnv(t, env, "admin", "log-level", "--for", "1s", "error")
	if want := "The log level is error for 1s, then back to the configured level.\n"; code != exitOK || stdout != want {
		t.Errorf("log-level --for 1s error: exit %d, stdout %q", code, stdout)
	}
	if level, _ := f.logLevel.Level(); level.String() != "ERROR" {
		t.Errorf("the server's level is %v, want error", level)
	}
	deadline := time.Now().Add(10 * time.Second)
	for f.level.Level().String() != "INFO" {
		if time.Now().After(deadline) {
			t.Fatalf("the level is still %v long after --for 1s", f.level.Level())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAdminStatus(t *testing.T) {
	_, env := startTestServer(t, false)
	code, stdout, stderr := runCLIEnv(t, env, "admin", "status")
	want := "isshoni 0.3.0, up 3h12m (since 2026-09-29 10:00 UTC)\n" +
		"  site        https://203.0.113.7\n" +
		"  tls         ip: certificate ready, expires 2026-10-06\n" +
		"  public IP   203.0.113.7 (stun); NAT: none\n" +
		"  listening   tcp [::]:443, unix /run/isshoni/admin.sock\n" +
		"  media       udp 203.0.113.7:7882, tcp 203.0.113.7:443\n" +
		"  live        1 room, 3 participants, 2 shares\n" +
		"  database    schema 7\n" +
		"  transfer    2026-09: 12.3 GB out, 0.4 GB in\n" +
		"  update      0.3.1 is available (security release): https://github.com/MoonWX/isshoni/releases/tag/v0.3.1\n"
	if code != exitOK || stdout != want || stderr != "" {
		t.Errorf("admin status: exit %d, stderr %q\n got:\n%s\nwant:\n%s", code, stderr, stdout, want)
	}

	code, stdout, stderr = runCLIEnv(t, env, "admin", "status", "--json")
	if code != exitOK || stderr != "" || strings.Count(stdout, "\n") != 1 {
		t.Fatalf("admin status --json: exit %d, stderr %q, stdout %q", code, stderr, stdout)
	}
	var st api.ServerStatus
	if err := json.Unmarshal([]byte(stdout), &st); err != nil {
		t.Fatal(err)
	}
	if st.Version != "0.3.0" || st.UptimeS != 11520 || st.TLS.Mode != api.TLSModeIP || len(st.Listeners) != 2 || st.Transfer == nil {
		t.Errorf("admin status --json: %+v", st)
	}
	for _, field := range []string{`"startedAt":"2026-09-29T10:00:00.000Z"`, `"publicIpv4":"203.0.113.7"`, `"schemaVersion":7`, `"uptimeS":11520`} {
		if !strings.Contains(stdout, field) {
			t.Errorf("admin status --json lacks %s:\n%s", field, stdout)
		}
	}
}

// What a server that knows little reports: the parts it left out are not printed.
func TestStatusText(t *testing.T) {
	got := statusText(api.ServerStatus{Version: "0.0.0-dev", StartedAt: time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC), UptimeS: 59})
	if want := "isshoni 0.0.0-dev, up 59s (since 2026-09-29 10:00 UTC)\n  live        0 rooms, 0 participants, 0 shares\n"; got != want {
		t.Errorf("statusText of a bare status:\n%s\nwant:\n%s", got, want)
	}
	for _, tt := range []struct {
		tls  api.TLSInfo
		want string
	}{
		{api.TLSInfo{Mode: api.TLSModeOff, Ready: true}, "tls         off: the proxy in front has the certificate\n"},
		{api.TLSInfo{Mode: api.TLSModeAuto}, "tls         auto: certificate not ready yet\n"},
		{api.TLSInfo{Mode: api.TLSModeAuto, LastErrorCode: "tls.dns_wrong"}, "tls         auto: certificate not ready yet (tls.dns_wrong)\n"},
		{api.TLSInfo{Mode: api.TLSModeManual, Ready: true}, "tls         manual: certificate ready\n"},
		{api.TLSInfo{Mode: api.TLSModeIP, Ready: true, LastErrorCode: "tls.rate"}, "tls         ip: certificate ready\n"},
	} {
		if got := statusText(api.ServerStatus{TLS: tt.tls}); !strings.Contains(got, tt.want) {
			t.Errorf("statusText with %+v:\n%s\nwant a line %q", tt.tls, got, tt.want)
		}
	}
	// An update that is not newer than what runs is not news.
	if got := statusText(api.ServerStatus{Version: "0.3.1", Update: &api.UpdateInfo{Latest: "0.3.1"}}); strings.Contains(got, "update") {
		t.Errorf("statusText shows an update to the running version:\n%s", got)
	}
}

// An answer the CLI has no words for still names the code; a timeout is a runtime error.
func TestAdminFailure(t *testing.T) {
	inv := &invocation{args: []string{"admin", "status"}}
	var stderr strings.Builder
	inv.stderr = &stderr
	c := ops.DialAdmin("/run/isshoni/admin.sock")
	for code, want := range map[string]int{
		api.CodeSetupUnavailable: exitRefused, api.CodeLastAdmin: exitRefused, api.CodeRegistrationClosed: exitRefused,
		api.CodeLimitReached: exitRefused, api.CodeBackupNewer: exitRefused, api.CodeRestoreInProgress: exitRefused,
		api.CodeUserNotFound: exitRuntime, api.CodeValidationFailed: exitRuntime, api.CodeInternal: exitRuntime,
		api.CodeServerShutdown: exitRuntime, api.CodeBackupInvalid: exitRuntime, "": exitRuntime,
	} {
		err := adminFailure(inv, clientOptions{}, c, &ops.AdminError{Status: 500, API: api.Error{Code: code}})
		if got := exitCodeOf(t.Context(), err); got != want {
			t.Errorf("code %q exits %d, want %d", code, got, want)
		}
	}
	err := adminFailure(inv, clientOptions{}, c, &ops.AdminError{Status: 404, API: api.Error{Code: api.CodeRoomNotFound}})
	if err.Error() != "the server answered room_not_found" {
		t.Errorf("an answer without a message: %q", err)
	}
	timeout := errors.New("ops: no answer from the server on the admin socket: context deadline exceeded")
	if err := adminFailure(inv, clientOptions{}, c, timeout); !errors.Is(err, timeout) || exitCodeOf(t.Context(), err) != exitRuntime {
		t.Errorf("a timeout: %v", err)
	}
	if stderr.Len() != 0 {
		t.Errorf("adminFailure printed %q for errors that report prints", stderr.String())
	}
}

// A config with errors still finds the socket (the default path), and says so when nothing answers there.
func TestClientWithBrokenConfig(t *testing.T) {
	cfg := writeConfig(t, "[tls]\nmdoe = \"auto\"\n")
	sock := socketPath(t)
	code, _, stderr := runCLIEnv(t, []string{"ISSHONI_CONFIG=" + cfg, "ISSHONI_LISTEN_ADMIN_SOCKET=" + sock}, "admin", "status")
	want := "isshoni is not running (no server on " + sock + ")\n" +
		"The config has errors, so that path may not be the configured one: run `isshoni config check`, or pass --socket PATH.\n"
	if code != exitUnreachable || stderr != want {
		t.Errorf("exit %d, stderr %q, want exit 4 and %q", code, stderr, want)
	}
	// With --socket the config's path is not used, so there is no note; and a server that answers is all it needs.
	f, _ := startTestServer(t, false)
	code, stdout, stderr := runCLIEnv(t, []string{"ISSHONI_CONFIG=" + cfg}, "healthcheck", "--socket", f.path)
	if code != exitOK || stdout != "ok\n" || stderr != "" {
		t.Errorf("with --socket: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestQuoteArgs(t *testing.T) {
	got := strings.Join(quoteArgs([]string{"admin", "users", "set-role", "--socket=/run/x.sock", "o'brien", "a b", "", "$HOME", "ok-1_2.3:4,5@6%7+8"}), " ")
	if want := `admin users set-role --socket=/run/x.sock 'o'\''brien' 'a b' '' '$HOME' ok-1_2.3:4,5@6%7+8`; got != want {
		t.Errorf("quoteArgs = %s\n         want %s", got, want)
	}
}

func TestValidFor(t *testing.T) {
	for d, want := range map[time.Duration]string{
		24 * time.Hour: "24 hours", 168 * time.Hour: "7 days", 720 * time.Hour: "30 days", 72 * time.Hour: "3 days",
		48 * time.Hour: "48 hours", time.Hour: "1 hour", 2 * time.Hour: "2 hours", 30 * time.Minute: "30 minutes",
	} {
		if got := validFor(time.Now().Add(d)); got != want {
			t.Errorf("validFor(now + %v) = %q, want %q", d, got, want)
		}
	}
	if got := validFor(time.Now().Add(20 * time.Second)); got != "1 minute" {
		t.Errorf("validFor(now + 20s) = %q", got)
	}
	past := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	if got := validFor(past); got != "until 2026-09-29 10:00 UTC" {
		t.Errorf("validFor(a past time) = %q", got)
	}
	if clock(time.Time{}) != "never" || shortDuration(90*time.Minute) != "1h30m" || shortDuration(2*time.Hour) != "2h" || shortDuration(1500*time.Millisecond) != "1.5s" {
		t.Errorf("clock/shortDuration: %q %q %q %q", clock(time.Time{}), shortDuration(90*time.Minute), shortDuration(2*time.Hour), shortDuration(1500*time.Millisecond))
	}
}

// The QR code is drawn with half blocks and a quiet zone of 2 modules, and decodes back to the library's bitmap.
func TestQRText(t *testing.T) {
	const link = "https://203.0.113.7/setup#Qm9fS2tXb0p6c1F4dVZ3UzBqWmN5Z2hB"
	q, err := qrcode.New(link, qrcode.Medium)
	if err != nil {
		t.Fatal(err)
	}
	q.DisableBorder = true
	bits := q.Bitmap()
	n := len(bits)
	if n%2 != 1 || n < 21 {
		t.Fatalf("the library's bitmap has %d rows", n)
	}

	got := mustQR(t, link)
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	size := n + 2*qrQuietZone
	if len(lines) != (size+1)/2 {
		t.Fatalf("%d lines for %d rows of modules, want %d", len(lines), size, (size+1)/2)
	}
	if size > 76 {
		t.Errorf("the code is %d columns wide: too wide for an 80-column terminal with its indentation", size)
	}
	// Decode the half blocks back into modules (true = dark).
	module := func(x, y int) bool {
		r := []rune(lines[y/2])
		if len(r) != size {
			t.Fatalf("line %d has %d characters, want %d", y/2, len(r), size)
		}
		switch r[x] {
		case '█':
			return false
		case ' ':
			return true
		case '▀': // the upper half is light
			return y%2 == 1
		case '▄':
			return y%2 == 0
		default:
			t.Fatalf("unexpected character %q", r[x])
			return false
		}
	}
	for y := range size {
		for x := range size {
			inside := x >= qrQuietZone && x < size-qrQuietZone && y >= qrQuietZone && y < size-qrQuietZone
			want := inside && bits[y-qrQuietZone][x-qrQuietZone] // the quiet zone is light
			if got := module(x, y); got != want {
				t.Fatalf("module (%d,%d) is dark=%v, want %v", x, y, got, want)
			}
		}
	}
	// The quiet zone is drawn, light: the first line is all blocks, and the last one upper halves.
	if lines[0] != strings.Repeat("█", size) || lines[len(lines)-1] != strings.Repeat("▀", size) {
		t.Errorf("quiet zone lines: %q and %q", lines[0], lines[len(lines)-1])
	}
	// Text too long for a QR code is an error, not a panic.
	if _, err := qrText(strings.Repeat("x", 5000)); err == nil {
		t.Error("qrText of 5000 characters succeeded")
	}
	// A bitmap with an even number of rows (not a QR code, but halfBlocks takes any) ends on a full line.
	if got := halfBlocks([][]bool{{true, false}, {false, true}}, 0); got != "▄▀\n" {
		t.Errorf("halfBlocks of a 2x2 bitmap = %q", got)
	}
}
