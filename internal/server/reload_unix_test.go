//go:build unix

package server_test

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/server"
	"github.com/MoonWX/isshoni/internal/server/config"
	"github.com/MoonWX/isshoni/internal/server/servertest"
)

// TestReloadReadDoesNotHoldUpShutdown: a reload reads the manual certificate files without the server's lifecycle
// lock, so a read that hangs (a mount that stopped answering) does not hold up the shutdown that SIGTERM begins,
// which takes that lock first. tls.cert_file is a FIFO here: opening it blocks until a writer comes. Once the read
// ends, the reload applies nothing more and says so.
func TestReloadReadDoesNotHoldUpShutdown(t *testing.T) {
	srv := servertest.Start(t, servertest.Options{TLS: true})
	certFile := srv.Cfg.TLS.CertFile
	pem, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(certFile); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(certFile, 0o600); err != nil {
		t.Fatal(err)
	}

	// A key that takes a restart: its line is the reload's last before the read.
	cfg := reloaded(srv, "info")
	cfg.ShutdownTimeout = config.Duration{Duration: 15 * time.Second}
	reloadDone := make(chan error, 1)
	go func() { reloadDone <- srv.Srv.Reload(context.Background(), cfg) }()
	waitFor(t, "the reload to name the keys for a restart", func() bool {
		return len(records(t, srv, "the config has changes that a reload does not apply; restart isshoni to apply them")) == 1
	})

	stopped := make(chan error, 1)
	go func() { stopped <- srv.Srv.Shutdown(context.Background(), server.ShutdownStop) }()
	var shutdownErr error
	select {
	case shutdownErr = <-stopped:
		stopped = nil
	case <-time.After(20 * time.Second):
		t.Error("the shutdown waited for the reload's read of the certificate files")
	}

	// A writer comes with the certificate as it was, and the read ends. O_NONBLOCK: the open fails while no reader
	// has the FIFO open yet, instead of waiting for one.
	waitFor(t, "the reload to open tls.cert_file", func() bool {
		f, err := os.OpenFile(certFile, os.O_WRONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: the test's own FIFO
		if errors.Is(err, syscall.ENXIO) {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(pem); err != nil {
			t.Error(err)
		}
		if err := f.Close(); err != nil {
			t.Error(err)
		}
		return true
	})
	select {
	case err := <-reloadDone:
		if err != nil {
			t.Errorf("Reload: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Reload did not return once the certificate file could be read")
	}
	if stopped != nil { // the shutdown waited for the reload: it goes on now
		select {
		case shutdownErr = <-stopped:
		case <-time.After(20 * time.Second):
			t.Fatal("the shutdown did not end")
		}
	}
	if shutdownErr != nil {
		t.Errorf("Shutdown: %v", shutdownErr)
	}
	if recs := records(t, srv, "reload ignored: the server is shutting down"); len(recs) != 1 || recs[0]["certificate"] != "read again" {
		t.Errorf("the line of the reload that the shutdown overtook = %v", recs)
	}
	if recs := records(t, srv, "reloaded (SIGHUP) without the log level"); len(recs) != 0 {
		t.Errorf("a reload that the shutdown overtook went on: %v", recs)
	}
}
