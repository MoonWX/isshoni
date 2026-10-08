package tlsmgr

import (
	"context"
	"errors"
	"sync"

	"github.com/caddyserver/certmagic"
)

// errStorageClosed is what certmagic gets from the storage once the manager shuts down.
var errStorageClosed = errors.New("tlsmgr: the certificate storage is closed: the server is shutting down")

// storage is certmagic's file storage with a count of what is going on in it, so that Shutdown can tell when the
// directory is quiet for good.
//
// certmagic runs its orders and renewals as jobs of a process-wide queue that gives nobody a handle to wait on.
// What a job does to the storage it does while holding a storage lock ("issue_cert_<name>", also across the hours
// between two attempts), or in single writes around it. So the storage counts the locks that are held and the
// writes in flight. close then refuses new locks, refuses writes that come without a lock (a job that is winding
// down still deletes what it put there), and reports when the count has reached zero: from then on nothing of the
// manager's writes below the directory, and the caller may move it away (a restore, 04 §12.4) or delete it (a
// test).
//
// Reads are not counted: a late one fails or succeeds, and changes nothing.
type storage struct {
	*certmagic.FileStorage

	mu      sync.Mutex
	closing bool
	locks   map[string]struct{} // the storage locks held through this storage
	busy    int                 // writes and lock attempts in flight
	quiet   chan struct{}       // closed once closing is set and nothing is held or in flight
	isQuiet bool
}

func newStorage(dir string) *storage {
	return &storage{
		FileStorage: &certmagic.FileStorage{Path: dir},
		locks:       map[string]struct{}{},
		quiet:       make(chan struct{}),
	}
}

// close makes the storage refuse new work and returns a channel that is closed once the work in flight is done.
func (s *storage) close() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closing = true
	s.settle()
	return s.quiet
}

// settle closes quiet when there is nothing left to wait for. The caller holds mu.
func (s *storage) settle() {
	if s.closing && !s.isQuiet && s.busy == 0 && len(s.locks) == 0 {
		s.isQuiet = true
		close(s.quiet)
	}
}

// begin counts a write. Once the storage is closing, only the holder of a lock may still write.
func (s *storage) begin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing && len(s.locks) == 0 {
		return errStorageClosed
	}
	s.busy++
	return nil
}

func (s *storage) end() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.busy--
	s.settle()
}

func (s *storage) Store(ctx context.Context, key string, value []byte) error {
	if err := s.begin(); err != nil {
		return err
	}
	defer s.end()
	return s.FileStorage.Store(ctx, key, value)
}

func (s *storage) Delete(ctx context.Context, key string) error {
	if err := s.begin(); err != nil {
		return err
	}
	defer s.end()
	return s.FileStorage.Delete(ctx, key)
}

// beginLock counts an attempt to take a lock. A closing storage hands out no new locks.
func (s *storage) beginLock() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return errStorageClosed
	}
	s.busy++
	return nil
}

// endLock ends the attempt and, when it got the lock, counts the lock as held.
func (s *storage) endLock(name string, held bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.busy--
	if held {
		s.locks[name] = struct{}{}
	}
	s.settle()
}

func (s *storage) Lock(ctx context.Context, name string) error {
	if err := s.beginLock(); err != nil {
		return err
	}
	err := s.FileStorage.Lock(ctx, name)
	s.endLock(name, err == nil)
	return err
}

func (s *storage) TryLock(ctx context.Context, name string) (bool, error) {
	if err := s.beginLock(); err != nil {
		return false, err
	}
	ok, err := s.FileStorage.TryLock(ctx, name)
	s.endLock(name, ok && err == nil)
	return ok, err
}

func (s *storage) Unlock(ctx context.Context, name string) error {
	err := s.FileStorage.Unlock(ctx, name)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.locks, name)
	s.settle()
	return err
}
