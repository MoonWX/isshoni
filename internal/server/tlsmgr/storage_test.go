package tlsmgr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func isQuiet(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// TestStorageCountsLocksAndWrites: the storage is certmagic's file storage, and close reports when the work in
// flight is done and refuses what would start new work.
func TestStorageCountsLocksAndWrites(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := newStorage(dir)

	// It stores where certmagic's file storage stores.
	if err := s.Store(ctx, "acme/account.json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "acme", "account.json")); err != nil || string(b) != "{}" {
		t.Fatalf("the stored file: %q, %v", b, err)
	}
	if b, err := s.Load(ctx, "acme/account.json"); err != nil || string(b) != "{}" || !s.Exists(ctx, "acme/account.json") {
		t.Fatalf("Load = %q, %v", b, err)
	}

	// A job holds its lock; another lock nests inside it.
	if err := s.Lock(ctx, "issue_cert_watch.example.com"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.TryLock(ctx, "ari_abc"); !ok || err != nil {
		t.Fatalf("TryLock = %v, %v", ok, err)
	}
	if ok, err := s.TryLock(ctx, "ari_abc"); ok || err != nil {
		t.Fatalf("TryLock of a held lock = %v, %v", ok, err)
	}

	quiet := s.close()
	if isQuiet(quiet) {
		t.Fatal("the storage is quiet while locks are held")
	}
	// No new locks.
	if err := s.Lock(ctx, "issue_cert_other.example.com"); !errors.Is(err, errStorageClosed) {
		t.Errorf("Lock after close = %v, want errStorageClosed", err)
	}
	if ok, err := s.TryLock(ctx, "ari_def"); ok || !errors.Is(err, errStorageClosed) {
		t.Errorf("TryLock after close = %v, %v, want errStorageClosed", ok, err)
	}
	// The job that holds a lock still finishes what it began.
	if err := s.Store(ctx, "certificates/x/x.crt", []byte("cert")); err != nil {
		t.Errorf("Store under a held lock after close = %v", err)
	}
	if err := s.Delete(ctx, "acme/account.json"); err != nil {
		t.Errorf("Delete under a held lock after close = %v", err)
	}
	if err := s.Unlock(ctx, "ari_abc"); err != nil {
		t.Fatal(err)
	}
	if isQuiet(quiet) {
		t.Fatal("the storage is quiet while one lock is still held")
	}
	if err := s.Unlock(ctx, "issue_cert_watch.example.com"); err != nil {
		t.Fatal(err)
	}
	if !isQuiet(quiet) {
		t.Fatal("the storage is not quiet after the last lock was released")
	}

	// Quiet for good: nothing writes any more, reads still work.
	if err := s.Store(ctx, "rw_test_1", []byte("x")); !errors.Is(err, errStorageClosed) {
		t.Errorf("Store after close = %v, want errStorageClosed", err)
	}
	if err := s.Delete(ctx, "certificates/x/x.crt"); !errors.Is(err, errStorageClosed) {
		t.Errorf("Delete after close = %v, want errStorageClosed", err)
	}
	if b, err := s.Load(ctx, "certificates/x/x.crt"); err != nil || string(b) != "cert" {
		t.Errorf("Load after close = %q, %v", b, err)
	}
	if s.close() != quiet || !isQuiet(s.close()) {
		t.Error("a second close differs from the first")
	}
	if locks, _ := filepath.Glob(filepath.Join(dir, "locks", "*")); len(locks) != 0 {
		t.Errorf("lock files left: %v", locks)
	}
}

// TestStorageCloseWhenIdle: with nothing in flight the storage is quiet at once.
func TestStorageCloseWhenIdle(t *testing.T) {
	s := newStorage(t.TempDir())
	if !isQuiet(s.close()) {
		t.Error("an idle storage is not quiet after close")
	}
}

// TestStorageLockAttemptCounts: an attempt to take a lock that someone else holds is work in flight; when it gives
// up, the storage is quiet.
func TestStorageLockAttemptCounts(t *testing.T) {
	dir := t.TempDir()
	other := newStorage(dir) // another user of the same directory holds the lock
	if err := other.Lock(context.Background(), "issue_cert_x"); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Unlock(context.Background(), "issue_cert_x") }()

	s := newStorage(dir)
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() { got <- s.Lock(ctx, "issue_cert_x") }()
	waitFor(t, "the lock attempt to begin", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.busy == 1
	})
	quiet := s.close()
	if isQuiet(quiet) {
		t.Fatal("the storage is quiet while a lock attempt is in flight")
	}
	cancel()
	select {
	case err := <-got:
		if err == nil {
			t.Fatal("the attempt got a lock that another holder has")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the lock attempt did not end with its context")
	}
	select {
	case <-quiet:
	case <-time.After(10 * time.Second):
		t.Fatal("the storage is not quiet after the attempt gave up")
	}
}

// TestShutdownLeavesTheStorageQuiet: a manager whose order waits for its next attempt holds certmagic's storage
// lock. Shutdown waits until certmagic has let go, so the directory can be replaced or removed right after.
func TestShutdownLeavesTheStorageQuiet(t *testing.T) {
	m, _ := acmeManager(t, Options{})
	start(t, m)
	waitFor(t, "the failed order", func() bool { return m.Status().LastErrorCode != "" })
	dir := m.opts.StorageDir
	if locks, _ := filepath.Glob(filepath.Join(dir, "locks", "*.lock")); len(locks) == 0 {
		t.Fatal("no storage lock while the order waits for its next attempt: the test's premise is gone")
	}

	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if locks, _ := filepath.Glob(filepath.Join(dir, "locks", "*")); len(locks) != 0 {
		t.Errorf("lock files after Shutdown: %v", locks)
	}
	st := m.acme.Load()
	if err := st.storage.Store(context.Background(), "late", []byte("x")); !errors.Is(err, errStorageClosed) {
		t.Errorf("a write after Shutdown = %v, want errStorageClosed", err)
	}
	// The shutdown is no failure of the certificate's: Status keeps what the CA (here: nobody) said before.
	if code := m.Status().LastErrorCode; code != CodeACMEFailed {
		t.Errorf("LastErrorCode after Shutdown = %q", code)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Errorf("removing the storage directory right after Shutdown: %v", err)
	}
}
