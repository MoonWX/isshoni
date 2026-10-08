//go:build linux || darwin

package ops

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The peer check with real credentials and three real users, as on a server: the service user owns the socket, root
// is the operator with sudo, and anybody else is refused (04 §12.1). The test needs root, since it gives up its
// effective uid to act as the other two. The other tests can only pretend: they inject a uid on the server's side
// and tell the client that it may be refused.
//
// A test run as root is a run in a container, for example
//
//	docker run --rm -v "$PWD":/src -w /src golang:1.27 go test -run TestPeerCredUsers -v ./internal/server/ops/
func TestPeerCredUsers(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: it runs the server and the calls as other users")
	}
	const (
		serviceUID  = 65534 // nobody
		strangerUID = 65533
	)
	// as runs fn with uid as the process's effective uid. Go changes it for every thread, and nothing else runs
	// meanwhile: this test is not parallel, and the only other goroutines are its own server's.
	as := func(uid int, fn func()) {
		t.Helper()
		if err := syscall.Seteuid(uid); err != nil {
			t.Fatalf("seteuid(%d): %v", uid, err)
		}
		defer func() {
			if err := syscall.Seteuid(0); err != nil {
				t.Fatalf("back to root: %v", err)
			}
		}()
		fn()
	}

	// The other users must get to the temporary directory, as they do to a container's /tmp.
	var probeErr error
	as(serviceUID, func() {
		dir, err := os.MkdirTemp("", "isshoni")
		if err == nil {
			err = os.Remove(dir)
		}
		probeErr = err
	})
	if probeErr != nil {
		t.Skipf("the temporary directory is not open to other users: %v", probeErr)
	}

	var a *testAdmin
	as(serviceUID, func() { a = startAdmin(t, nil) }) // the socket belongs to the service user, and the rule names it
	health := func() error {
		_, err := a.client.Health(t.Context())
		return err
	}
	// Open the way to the socket for everybody, so that the peer check is all that stands between the stranger and
	// the server. In production the file modes come first (/run/isshoni is 0750, the socket 0600).
	if err := os.Chmod(filepath.Dir(a.path), 0o755); err != nil { //nolint:gosec // G302: the point of this test
		t.Fatal(err)
	}
	if err := os.Chmod(a.path, 0o666); err != nil { //nolint:gosec // G302: the point of this test
		t.Fatal(err)
	}

	if err := health(); err != nil {
		t.Errorf("root: %v, want an answer", err)
	}
	as(serviceUID, func() {
		if err := health(); err != nil {
			t.Errorf("the service user: %v, want an answer", err)
		}
		if callerMayBeRefused(a.path) {
			t.Error("the service user owns the socket and can't be refused")
		}
	})
	if strings.Contains(a.logs.String(), "refused a connection") {
		t.Errorf("root or the service user was refused:\n%s", a.logs)
	}

	as(strangerUID, func() {
		if !callerMayBeRefused(a.path) {
			t.Error("a stranger can be refused")
		}
		err := health()
		var ue *AdminUnreachableError
		if !errors.Is(err, ErrAdminPermission) || !errors.As(err, &ue) {
			t.Fatalf("a stranger: %v, want ErrAdminPermission", err)
		}
		// It was the server that hung up, not the file system that said no.
		if errors.Is(ue.Err, os.ErrPermission) {
			t.Errorf("the stranger did not get as far as the peer check: %v", ue.Err)
		}
	})
	waitFor(t, func() bool { return strings.Contains(a.logs.String(), `"peer_uid":65533`) })

	// With the socket's own mode the stranger does not even connect; the answer is the same.
	if err := os.Chmod(a.path, 0o600); err != nil {
		t.Fatal(err)
	}
	as(strangerUID, func() {
		err := health()
		var ue *AdminUnreachableError
		if !errors.Is(err, ErrAdminPermission) || !errors.As(err, &ue) || !errors.Is(ue.Err, os.ErrPermission) {
			t.Errorf("a stranger at a 0600 socket: %v, want ErrAdminPermission from the connect", err)
		}
	})
	if err := health(); err != nil {
		t.Errorf("root at a 0600 socket: %v, want an answer", err)
	}
}
