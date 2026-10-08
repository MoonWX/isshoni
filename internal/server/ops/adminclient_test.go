package ops

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// serveOn serves h on a unix socket until the test ends and returns a client for it. It stands in for something
// that listens on the path and is not, or not quite, isshoni's admin server.
func serveOn(t *testing.T, h http.Handler) *AdminClient {
	t.Helper()
	path := socketPath(t)
	ln := listenUnix(t, path)
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		<-done
	})
	return DialAdmin(path)
}

// The dial errors of 04 §12.1: ENOENT and ECONNREFUSED mean "not running", EACCES means "permission denied".
func TestAdminClientUnreachable(t *testing.T) {
	notRunning := func(t *testing.T, path string) {
		t.Helper()
		c := DialAdmin(path)
		if c.Path() != path {
			t.Errorf("Path() = %q", c.Path())
		}
		_, err := c.Health(t.Context())
		var ue *AdminUnreachableError
		if !errors.Is(err, ErrAdminNotRunning) || errors.Is(err, ErrAdminPermission) || !errors.As(err, &ue) {
			t.Fatalf("Health on %s: %v, want ErrAdminNotRunning", path, err)
		}
		if want := "isshoni is not running (no server on " + path + ")"; err.Error() != want {
			t.Errorf("error text %q, want %q", err.Error(), want)
		}
		if ue.Err == nil {
			t.Error("the dial error is not kept")
		}
		// Every call classifies alike.
		if _, err := c.SetupURL(t.Context(), 0); !errors.Is(err, ErrAdminNotRunning) {
			t.Errorf("SetupURL: %v", err)
		}
		if err := c.SetLogLevel(t.Context(), "debug", time.Minute); !errors.Is(err, ErrAdminNotRunning) {
			t.Errorf("SetLogLevel: %v", err)
		}
	}
	t.Run("no socket", func(t *testing.T) {
		notRunning(t, socketPath(t))
	})
	t.Run("no directory", func(t *testing.T) { // /run/isshoni does not exist before the first start
		notRunning(t, filepath.Join(filepath.Dir(socketPath(t)), "missing", "admin.sock"))
	})
	t.Run("stale socket file", func(t *testing.T) {
		path := socketPath(t)
		ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		ln.SetUnlinkOnClose(false)
		_ = ln.Close()
		notRunning(t, path)
	})
	t.Run("a regular file", func(t *testing.T) {
		path := socketPath(t)
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		notRunning(t, path)
	})
	t.Run("a directory the caller can't enter", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs a user that file modes apply to")
		}
		a := startAdmin(t, nil)
		dir := filepath.Dir(a.path)
		if err := os.Chmod(dir, 0); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(dir, 0o700) }() //nolint:gosec // G302: a directory needs its x bit
		_, err := a.client.Health(t.Context())
		var ue *AdminUnreachableError
		if !errors.Is(err, ErrAdminPermission) || errors.Is(err, ErrAdminNotRunning) || !errors.As(err, &ue) {
			t.Fatalf("Health behind a closed directory: %v, want ErrAdminPermission", err)
		}
		if want := "permission denied on " + a.path; err.Error() != want {
			t.Errorf("error text %q, want %q", err.Error(), want)
		}
	})
	t.Run("a socket the caller can't write", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs a user that file modes apply to")
		}
		a := startAdmin(t, nil)
		if err := os.Chmod(a.path, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := a.client.Health(t.Context()); !errors.Is(err, ErrAdminPermission) {
			t.Fatalf("Health on a 0000 socket: %v, want ErrAdminPermission", err)
		}
	})
}

// A server that hangs up before its first answer has refused the peer (04 §12.1), whether it closes before or after
// it read the request.
func TestAdminClientHangup(t *testing.T) {
	for _, readFirst := range []bool{false, true} {
		path := socketPath(t)
		ln := listenUnix(t, path)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				if readFirst {
					_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
					_, _ = c.Read(make([]byte, 16))
				}
				_ = c.Close()
			}
		}()
		c := DialAdmin(path)
		for range 300 { // the two sides race: every outcome of the race must classify alike
			if _, err := c.Users(t.Context()); !errors.Is(err, ErrAdminPermission) {
				t.Errorf("readFirst=%v: %v, want ErrAdminPermission", readFirst, err)
			}
		}
		_ = ln.Close()
		<-done
	}
}

// The context bounds a call: a server that never answers is a timeout, not "not running" or "permission denied".
func TestAdminClientTimeout(t *testing.T) {
	release := make(chan struct{})
	c := serveOn(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer close(release)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Health(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrAdminPermission) || errors.Is(err, ErrAdminNotRunning) {
		t.Fatalf("Health with a silent server: %v, want the deadline error", err)
	}
	if !strings.Contains(err.Error(), "no answer from the server on the admin socket "+c.Path()) {
		t.Errorf("error text %q", err.Error())
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("the call took %v", time.Since(start))
	}
	// A context that is done before the call fails the dial.
	if _, err := c.Health(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Health with a dead context: %v", err)
	}
}

// Something else that listens on the path (--socket /var/run/docker.sock) gives a clear error, not a parse error.
func TestAdminClientNotOurServer(t *testing.T) {
	c := serveOn(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			http.Error(w, `{"message":"page not found"}`, http.StatusNotFound)
		case "/v1/ready":
			w.WriteHeader(http.StatusServiceUnavailable) // no health document
		case "/v1/users":
			_, _ = io.WriteString(w, "<html>hello</html>")
		default:
			http.Error(w, "nope", http.StatusBadGateway)
		}
	}))
	_, err := c.Health(t.Context())
	var ae *AdminError
	if !errors.As(err, &ae) || ae.API.Code != "" || ae.Status != 404 || !strings.Contains(ae.Message, "unexpected answer (HTTP 404) from "+c.Path()) || ae.Fix == "" {
		t.Errorf("Health against another server: %v (%+v)", err, ae)
	}
	if _, err := c.Ready(t.Context()); !errors.As(err, &ae) || ae.Status != 503 || ae.API.Code != "" {
		t.Errorf("Ready with a 503 that is no health document: %v", err)
	}
	if _, err := c.Users(t.Context()); err == nil || !strings.Contains(err.Error(), "unexpected answer") {
		t.Errorf("Users with an HTML answer: %v", err)
	}
	if _, err := c.Status(t.Context()); !errors.As(err, &ae) || ae.Status != 502 {
		t.Errorf("Status: %v", err)
	}
	// A listener that does not speak HTTP at all.
	path := socketPath(t)
	ln := listenUnix(t, path)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = io.WriteString(conn, "220 smtp.example.com ESMTP\r\n")
		_ = conn.Close()
	}()
	defer func() { _ = ln.Close() }()
	_, err = DialAdmin(path).Health(t.Context())
	if err == nil || errors.Is(err, ErrAdminPermission) || errors.Is(err, ErrAdminNotRunning) {
		t.Errorf("Health against a non-HTTP listener: %v, want a plain error", err)
	}
}

// The streaming calls of the later slices (README S65) on the client side: Backup hands out the body and the
// proposed file name, Restore sends a body of unknown length, and both turn the socket's error document into an
// *AdminError.
func TestAdminClientStreams(t *testing.T) {
	// The handler's goroutines write what they saw; the test reads it after each call.
	var (
		mu  sync.Mutex
		got struct{ certs, contentType, body string }
	)
	var fail atomic.Bool
	seen := func() (certs, contentType, body string) {
		mu.Lock()
		defer mu.Unlock()
		return got.certs, got.contentType, got.body
	}
	c := serveOn(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if fail.Load() {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":{"code":"backup_newer"},"message":"This backup is from isshoni 0.5.0; install 0.5.0 or newer first"}`)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/backup":
			got.certs = r.URL.Query().Get("certs")
			w.Header().Set("Content-Type", "application/gzip")
			w.Header().Set("Content-Disposition", `attachment; filename="../isshoni-backup-0.3.0-20260929T101500Z.tar.gz"`)
			_, _ = io.WriteString(w, "archive bytes")
		case "POST /v1/restore":
			data, _ := io.ReadAll(r.Body)
			got.contentType, got.body = r.Header.Get("Content-Type"), string(data)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"restarting":true,"preRestoreBackup":"backups/pre-restore-20260929T101500Z.tar.gz"}`)
		case "POST /v1/rotate-secrets":
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"rotated":["session","invite","resume","vapid"],"restarting":true}`)
		case "POST /v1/doctor":
			_, _ = io.WriteString(w, `{"schema":1,"checks":[{"id":"dns","status":"ok"}]}`)
		}
	}))

	b, err := c.Backup(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(b)
	if certs, _, _ := seen(); err != nil || string(data) != "archive bytes" || certs != "1" {
		t.Errorf("Backup body %q, %v; certs=%q", data, err, certs)
	}
	if b.Filename != "isshoni-backup-0.3.0-20260929T101500Z.tar.gz" {
		t.Errorf("Filename %q, want the base name", b.Filename)
	}
	if err := b.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	b, err = c.Backup(t.Context(), false)
	if certs, _, _ := seen(); err != nil || certs != "0" {
		t.Fatalf("Backup without certs: %v, certs=%q", err, certs)
	}
	_ = b.Close()

	// A reader that is not a file or a buffer: the length is not known up front.
	res, err := c.Restore(t.Context(), io.MultiReader(strings.NewReader("SQLite format 3\x00"), strings.NewReader("pages")), AdminContentSQLite)
	if err != nil || !res.Restarting || res.PreRestoreBackup != "backups/pre-restore-20260929T101500Z.tar.gz" {
		t.Errorf("Restore = %+v, %v", res, err)
	}
	if _, contentType, body := seen(); contentType != AdminContentSQLite || body != "SQLite format 3\x00pages" {
		t.Errorf("the server got %q as %q", body, contentType)
	}
	rot, err := c.RotateSecrets(t.Context(), AdminRotateRequest{Keys: []string{"session", "invite", "resume"}, VAPID: true})
	if err != nil || !rot.Restarting || len(rot.Rotated) != 4 {
		t.Errorf("RotateSecrets = %+v, %v", rot, err)
	}
	rep, err := c.Doctor(t.Context(), nil)
	if err != nil || len(rep.Checks) != 1 || rep.Checks[0].ID != "dns" {
		t.Errorf("Doctor = %+v, %v", rep, err)
	}

	fail.Store(true)
	if _, err := c.Backup(t.Context(), true); !api.IsCode(err, api.CodeBackupNewer) {
		t.Errorf("Backup error: %v", err)
	}
	_, err = c.Restore(t.Context(), strings.NewReader("x"), AdminContentArchive)
	var ae *AdminError
	if !errors.As(err, &ae) || ae.API.Code != api.CodeBackupNewer || ae.Status != 409 || !strings.Contains(ae.Message, "install 0.5.0 or newer first") {
		t.Errorf("Restore error: %v", err)
	}
}
