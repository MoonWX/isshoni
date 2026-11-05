//go:build windows

package winos

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// pipe server limits.
const (
	pipeBuffer  = 64 << 10
	readTimeout = 2 * time.Second // a client that connects and sends nothing is dropped after this
	asfwAny     = 0xFFFFFFFF      // ASFW_ANY
	// closeWait bounds Close's wait for the serving goroutine: onArgs is the caller's code and may block.
	closeWait = time.Second
	// pipeNameAttempts bounds the random names the first pipe instance tries: each is new, so another process holds
	// one only by chance.
	pipeNameAttempts = 8
	// lockOffset is the locked byte of instance.lock: far past the content, so reading the pipe's name never waits.
	lockOffset = 1 << 40
	// forwardPause spaces Forward's attempts.
	forwardPause = 25 * time.Millisecond
	// lockFile is the lock's file in the data root.
	lockFile = "instance.lock"
	// lockReadLimit bounds what a second launch reads of instance.lock: a valid content is one pipe name.
	lockReadLimit = 512
)

var (
	user32                       = windows.NewLazySystemDLL("user32.dll")
	procAllowSetForegroundWindow = user32.NewProc("AllowSetForegroundWindow")
)

// SingleInstance is the single-instance lock and its pipe. Safe for concurrent use.
type SingleInstance struct {
	id, root, sid string
	log           *slog.Logger

	mu       sync.Mutex
	lock     windows.Handle // instance.lock, its range held by the first instance until Close
	writable bool           // lock was opened for writing: the pipe's name can be published
	stop     windows.Handle // Serve's stop event
	done     chan struct{}
	closed   bool
}

// NewSingleInstance prepares the lock for id (InstanceID; tests use their own; it names the pipe) and the data root
// root (%LOCALAPPDATA%\isshoni, or the e2e build's own), whose instance.lock is the lock (03 §13.3). It touches no
// file: Acquire does. log nil means slog.Default().
func NewSingleInstance(id, root string, log *slog.Logger) (*SingleInstance, error) {
	if log == nil {
		log = slog.Default()
	}
	if !filepath.IsAbs(root) {
		return nil, errors.New("winos: the data root is not an absolute path")
	}
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("winos: the user's SID: %w", err)
	}
	return &SingleInstance{id: id, root: root, sid: u.User.Sid.String(), log: log.With("component", "winos")}, nil
}

// userOnly is the security descriptor of instance.lock and the pipe: owned by the user (explicitly, so an elevated
// instance's object isn't owned by BUILTIN\Administrators), open to the user and, with system, SYSTEM.
func userOnly(sid string, system bool) (*windows.SecurityAttributes, error) {
	sddl := "O:" + sid + "D:P(A;;GA;;;" + sid + ")"
	if system {
		sddl += "(A;;GA;;;SY)"
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, fmt.Errorf("winos: a security descriptor: %w", err)
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	return sa, nil
}

// lockRange is the Overlapped of LockFileEx and UnlockFileEx: the one byte at lockOffset.
func lockRange() *windows.Overlapped {
	return &windows.Overlapped{Offset: uint32(lockOffset & 0xFFFFFFFF), OffsetHigh: uint32(lockOffset >> 32)}
}

// openLock opens path, instance.lock. With create it opens or creates it (OPEN_ALWAYS; a new file gets sa) for
// reading and writing, or, when writing is denied, an existing one for reading only (LockFileEx needs either);
// without, an existing one for reading. FILE_FLAG_OPEN_REPARSE_POINT opens a link as itself, which checkLock refuses.
// The handle shares reading and writing, never deletion, so the file can't go while an instance holds it. Its errors
// hold no path.
func openLock(path string, create bool, sa *windows.SecurityAttributes) (h windows.Handle, writable bool, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, false, errors.New("winos: the data root's path has a NUL")
	}
	const share = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE
	const flags = windows.FILE_ATTRIBUTE_NORMAL | windows.FILE_FLAG_OPEN_REPARSE_POINT
	if create {
		h, err = windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, share, sa, windows.OPEN_ALWAYS,
			flags, 0)
		if err == nil {
			return h, true, nil
		}
		if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return windows.InvalidHandle, false, err
		}
	}
	h, err = windows.CreateFile(p, windows.GENERIC_READ, share, nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return windows.InvalidHandle, false, err
	}
	return h, false, nil
}

// checkLock reads what lockState needs to know about an open instance.lock apart from the lock: whether it is a plain
// file (not a reparse point, a directory or a file with another hard link elsewhere) and whether this user owns it.
func (s *SingleInstance) checkLock(h windows.Handle) (lockFacts, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return lockFacts{}, fmt.Errorf("winos: the single-instance lock file: %w", err)
	}
	const notPlain = windows.FILE_ATTRIBUTE_REPARSE_POINT | windows.FILE_ATTRIBUTE_DIRECTORY
	owner, err := ownerOf(h, windows.SE_FILE_OBJECT)
	return lockFacts{
		reparse:     info.FileAttributes&notPlain != 0 || info.NumberOfLinks != 1,
		ownerIsUser: err == nil && owner == s.sid,
	}, nil
}

// Acquire takes the lock: first is true when no other process of this user and data root, in any of the user's
// sessions, elevated or not, holds it. A second instance calls Forward and exits. An instance.lock that isn't this
// user's plain file never makes this launch a second instance: Acquire returns an error, so the caller starts as the
// first instance without a lock (03 §13.3).
func (s *SingleInstance) Acquire() (first bool, err error) {
	if err := os.MkdirAll(s.root, 0o700); err != nil { //nolint:gosec // G703: the app's own data root
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err // without the path
		}
		return false, fmt.Errorf("winos: the data root can't be created: %w", err)
	}
	sa, err := userOnly(s.sid, true)
	if err != nil {
		return false, err
	}
	h, writable, err := openLock(filepath.Join(s.root, lockFile), true, sa)
	if err != nil {
		return false, fmt.Errorf("winos: the single-instance lock file: %w", err)
	}
	facts, err := s.checkLock(h)
	if err == nil && !facts.reparse && facts.ownerIsUser {
		facts.lockErr = windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0,
			1, 0, lockRange())
		facts.violation = errors.Is(facts.lockErr, windows.ERROR_LOCK_VIOLATION)
	}
	if err == nil {
		first, err = lockState(facts)
	}
	if err != nil || !first {
		_ = windows.CloseHandle(h)
		return false, err
	}
	if writable {
		// The last run's pipe name goes: a second launch waits for this run's (Serve publishes it).
		if err := writeLock(h, nil); err != nil {
			s.log.Warn("winos: the single-instance lock file wasn't cleared", "err", err)
		}
	} else {
		s.log.Warn("winos: the single-instance lock file is read-only; second launches can't find this one")
	}
	s.mu.Lock()
	s.lock, s.writable = h, writable
	s.mu.Unlock()
	return true, nil
}

// writeLock replaces instance.lock's content with data, from offset 0, and flushes it. Its errors hold neither.
func writeLock(h windows.Handle, data []byte) error {
	if _, err := windows.Seek(h, 0, io.SeekStart); err != nil {
		return err
	}
	for len(data) > 0 {
		var n uint32
		if err := windows.WriteFile(h, data, &n, nil); err != nil {
			return err
		}
		data = data[n:]
	}
	if err := windows.SetEndOfFile(h); err != nil {
		return err
	}
	return windows.FlushFileBuffers(h)
}

// publish writes the pipe's name into instance.lock (the first pipe instance exists by then). Close ran: errStopped.
func (s *SingleInstance) publish(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.lock == 0:
		return errStopped
	case !s.writable:
		return errors.New("winos: the single-instance lock file is read-only")
	}
	if err := writeLock(s.lock, []byte(name)); err != nil {
		return fmt.Errorf("winos: the pipe's name wasn't written: %w", err)
	}
	return nil
}

// readPipeName reads the first instance's pipe name from instance.lock: ok false while there is none (the first
// instance still starts), and for a file that isn't this user's plain file.
func (s *SingleInstance) readPipeName() (pipe string, ok bool) {
	h, _, err := openLock(filepath.Join(s.root, lockFile), false, nil)
	if err != nil {
		return "", false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if facts, err := s.checkLock(h); err != nil || facts.reparse || !facts.ownerIsUser {
		return "", false
	}
	data := make([]byte, lockReadLimit)
	got := 0
	for got < len(data) {
		var n uint32
		err := windows.ReadFile(h, data[got:], &n, nil)
		if err != nil && !errors.Is(err, windows.ERROR_HANDLE_EOF) {
			return "", false
		}
		if n == 0 {
			break
		}
		got += int(n)
	}
	return pipeFromLock(s.id, data[:got])
}

// LockHeld reports whether a process holds the single-instance lock of the data root root, by trying to take it for a
// moment (cmd/isshoni-desktop's TestAutostartRoundTrip checks that nothing does before it starts the app). No
// instance.lock is no lock.
func LockHeld(root string) (bool, error) {
	h, _, err := openLock(filepath.Join(root, lockFile), false, nil)
	switch {
	case errors.Is(err, windows.ERROR_FILE_NOT_FOUND), errors.Is(err, windows.ERROR_PATH_NOT_FOUND):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("winos: the single-instance lock file: %w", err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	err = windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, lockRange())
	switch {
	case errors.Is(err, windows.ERROR_LOCK_VIOLATION):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("winos: the single-instance lock: %w", err)
	}
	return false, windows.UnlockFileEx(h, 0, 1, 0, lockRange())
}

// ownerOf reads the owner SID of h (a handle with READ_CONTROL) of the object type typ.
func ownerOf(h windows.Handle, typ windows.SE_OBJECT_TYPE) (string, error) {
	sd, err := windows.GetSecurityInfo(h, typ, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return "", err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return "", err
	}
	if owner == nil {
		return "", errors.New("winos: the object has no owner")
	}
	return owner.String(), nil
}

// Serve delivers each forwarded command line to onArgs, one at a time, on a goroutine of its own, until Close. Only
// the first instance serves.
func (s *SingleInstance) Serve(onArgs func(args []string)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == 0 {
		return ErrNotFirst
	}
	if s.closed || s.stop != 0 {
		return errors.New("winos: Serve called twice or after Close")
	}
	stop, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return fmt.Errorf("winos: %w", err)
	}
	// Owned by the user, set explicitly (Forward's serverCheck reads it), and open to the user only.
	sa, err := userOnly(s.sid, false)
	if err != nil {
		_ = windows.CloseHandle(stop)
		return err
	}
	s.stop = stop
	s.done = make(chan struct{})
	go func() {
		defer close(s.done)
		serveLoop(&winPipeServer{s: s, sa: sa}, onArgs, s.log)
	}()
	return nil
}

// winPipeServer is serveLoop's pipe: instances of the named pipe, in byte mode. A client may connect to an instance
// as soon as it exists (it is listening from CreateNamedPipe on), and what the client wrote stays readable after it
// closed its end, until the instance is disconnected.
type winPipeServer struct {
	s    *SingleInstance
	sa   *windows.SecurityAttributes
	name *uint16 // set by the first create
}

// create makes the first instance under a new random name, with FILE_FLAG_FIRST_PIPE_INSTANCE so that nobody else
// holds it, and only then publishes the name in instance.lock; later instances take that name.
func (p *winPipeServer) create(first bool) (pipeInstance, error) {
	if !first {
		h, err := p.s.createPipe(p.name, p.sa, false)
		if err != nil {
			return nil, err
		}
		return winPipe{s: p.s, h: h}, nil
	}
	var last error
	for range pipeNameAttempts {
		name, err := newPipeName(p.s.id)
		if err != nil {
			return nil, err
		}
		u, err := windows.UTF16PtrFromString(name)
		if err != nil {
			return nil, errors.New("winos: the pipe's name has a NUL")
		}
		h, err := p.s.createPipe(u, p.sa, true)
		if errors.Is(err, errStopped) {
			return nil, err
		}
		if err != nil {
			last = err
			continue
		}
		if err := p.s.publish(name); err != nil {
			_ = windows.CloseHandle(h)
			return nil, err
		}
		p.name = u
		return winPipe{s: p.s, h: h}, nil
	}
	return nil, fmt.Errorf("winos: no pipe name of its own in %d tries: %w", pipeNameAttempts, last)
}

func (p *winPipeServer) stopped() bool { return p.s.stopped() }

type winPipe struct {
	s *SingleInstance
	h windows.Handle
}

func (p winPipe) accept() ([]byte, error) { return p.s.accept(p.h) }

func (p winPipe) close() {
	_ = windows.DisconnectNamedPipe(p.h) // ERROR_PIPE_NOT_CONNECTED when no client connected: nothing to do
	_ = windows.CloseHandle(p.h)
}

// stopped reports whether Close has set the stop event.
func (s *SingleInstance) stopped() bool {
	r, err := windows.WaitForSingleObject(s.stop, 0)
	return err == nil && r == windows.WAIT_OBJECT_0
}

// createPipe creates an instance of the pipe. The first one takes FILE_FLAG_FIRST_PIPE_INSTANCE and is tried once (a
// name someone holds is given up for another); later ones don't take the flag and are retried briefly. Close ends the
// retries (errStopped).
func (s *SingleInstance) createPipe(name *uint16, sa *windows.SecurityAttributes, first bool) (windows.Handle, error) {
	if s.stopped() {
		return windows.InvalidHandle, errStopped
	}
	flags := uint32(windows.PIPE_ACCESS_INBOUND | windows.FILE_FLAG_OVERLAPPED)
	deadline := time.Now().Add(150 * time.Millisecond)
	if first {
		flags |= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
		deadline = time.Now()
	}
	for {
		h, err := windows.CreateNamedPipe(name, flags,
			windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
			windows.PIPE_UNLIMITED_INSTANCES, 0, pipeBuffer, 0, sa)
		if err == nil || !time.Now().Before(deadline) {
			return h, err
		}
		if r, _ := windows.WaitForSingleObject(s.stop, 50); r == windows.WAIT_OBJECT_0 {
			return windows.InvalidHandle, errStopped
		}
	}
}

// accept waits for a client and reads its message until it closes the pipe.
func (s *SingleInstance) accept(h windows.Handle) ([]byte, error) {
	ev, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = windows.CloseHandle(ev) }()
	ov := &windows.Overlapped{HEvent: ev}
	switch err := windows.ConnectNamedPipe(h, ov); {
	// ERROR_NO_DATA: a client connected, wrote and closed before this call (while onArgs ran); what it wrote is
	// still read below.
	case err == nil, errors.Is(err, windows.ERROR_PIPE_CONNECTED), errors.Is(err, windows.ERROR_NO_DATA):
	case errors.Is(err, windows.ERROR_IO_PENDING):
		if err := s.wait(h, ov, windows.INFINITE); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	deadline := time.Now().Add(readTimeout)
	var data []byte
	buf := make([]byte, 4096)
	for {
		if err := windows.ResetEvent(ev); err != nil {
			return nil, err
		}
		var n uint32
		err := windows.ReadFile(h, buf, &n, ov)
		if errors.Is(err, windows.ERROR_IO_PENDING) {
			left := time.Until(deadline)
			if left <= 0 {
				s.cancel(h, ov)
				return nil, errors.New("winos: a second launch sent nothing in time")
			}
			err = s.wait(h, ov, uint32(left/time.Millisecond)+1) //nolint:gosec // G115: at most readTimeout, 2000 ms
			if err == nil {
				err = windows.GetOverlappedResult(h, ov, &n, false)
			}
		}
		switch {
		case errors.Is(err, windows.ERROR_BROKEN_PIPE): // the client closed: the message is complete
			return data, nil
		case errors.Is(err, errStopped):
			return nil, err
		case err != nil && !errors.Is(err, windows.ERROR_MORE_DATA):
			return nil, err
		}
		data = append(data, buf[:n]...)
		if len(data) > maxForwardBytes {
			return nil, errors.New("winos: a forwarded message is too long")
		}
	}
}

// wait waits for ov's operation or Close; on Close (or a timeout) it cancels the operation and waits until the
// kernel let go of ov and its buffer.
func (s *SingleInstance) wait(h windows.Handle, ov *windows.Overlapped, ms uint32) error {
	r, err := windows.WaitForMultipleObjects([]windows.Handle{ov.HEvent, s.stop}, false, ms)
	switch {
	case err != nil:
		s.cancel(h, ov)
		return err
	case r == windows.WAIT_OBJECT_0:
		return nil
	case r == windows.WAIT_OBJECT_0+1:
		s.cancel(h, ov)
		return errStopped
	default: // WAIT_TIMEOUT
		s.cancel(h, ov)
		return errors.New("winos: a second launch sent nothing in time")
	}
}

func (s *SingleInstance) cancel(h windows.Handle, ov *windows.Overlapped) {
	_ = windows.CancelIoEx(h, ov)
	var n uint32
	_ = windows.GetOverlappedResult(h, ov, &n, true)
}

// errNoPipeName: instance.lock holds no pipe name yet (the first instance still starts).
var errNoPipeName = errors.New("winos: the first instance hasn't published its pipe")

// Forward sends args to the first instance, within ForwardTimeout (03 §13.3). It calls
// AllowSetForegroundWindow(ASFW_ANY) first, so the first instance may come to the front. It reads the pipe's name
// from instance.lock and retries until the bound while there is none, the pipe isn't there or busy, or the pipe
// isn't this user's (a stale name another account took: the first instance may still be publishing its own).
func (s *SingleInstance) Forward(args []string) error {
	data, err := encodeArgs(args)
	if err != nil {
		return err
	}
	_, _, _ = procAllowSetForegroundWindow.Call(uintptr(asfwAny))
	deadline := time.Now().Add(ForwardTimeout)
	for {
		h, err := s.openServer()
		if err == nil {
			return writeAll(h, data)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("winos: the first instance didn't answer within %v: %w", ForwardTimeout, err)
		}
		time.Sleep(forwardPause)
	}
}

// openServer opens the pipe whose name instance.lock holds and checks that it is this user's. Its errors hold no
// name.
func (s *SingleInstance) openServer() (windows.Handle, error) {
	pipe, ok := s.readPipeName()
	if !ok {
		return windows.InvalidHandle, errNoPipeName
	}
	name, err := windows.UTF16PtrFromString(pipe)
	if err != nil {
		return windows.InvalidHandle, errNoPipeName
	}
	// SECURITY_IDENTIFICATION: the server may learn who this is, but never act as this user. READ_CONTROL: to read
	// the pipe's owner (serverCheck).
	h, err := windows.CreateFile(name, windows.GENERIC_WRITE|windows.READ_CONTROL, 0, nil, windows.OPEN_EXISTING,
		windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
	if err != nil {
		return windows.InvalidHandle, fmt.Errorf("winos: the first instance's pipe: %w", err)
	}
	if err := s.checkServer(h); err != nil {
		_ = windows.CloseHandle(h)
		return windows.InvalidHandle, err
	}
	return h, nil
}

// writeAll writes the forwarded message and closes the pipe: its end is the message's end.
func writeAll(h windows.Handle, data []byte) error {
	defer func() { _ = windows.CloseHandle(h) }()
	for len(data) > 0 {
		var n uint32
		if err := windows.WriteFile(h, data, &n, nil); err != nil {
			return fmt.Errorf("winos: forwarding the arguments: %w", err)
		}
		data = data[n:]
	}
	return nil
}

// checkServer makes sure that the pipe is this user's: its owner, which the first instance sets to the user's SID and
// no other account can, or, when the owner can't be read, the server process's user (serverCheck, 03 §13.3).
func (s *SingleInstance) checkServer(pipe windows.Handle) error {
	owner, ownerErr := ownerOf(pipe, windows.SE_KERNEL_OBJECT)
	return serverCheck(s.sid, owner, ownerErr, func() (string, error) { return serverUser(pipe) })
}

// serverUser is the SID of the user the pipe's server process runs as. Its token can't be opened from a non-elevated
// client when the server runs elevated (ERROR_ACCESS_DENIED).
func serverUser(pipe windows.Handle) (string, error) {
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(pipe, &pid); err != nil {
		return "", err
	}
	p, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", err
	}
	defer func() { _ = windows.CloseHandle(p) }()
	var tok windows.Token
	if err := windows.OpenProcessToken(p, windows.TOKEN_QUERY, &tok); err != nil {
		return "", err
	}
	defer func() { _ = tok.Close() }()
	u, err := tok.GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String(), nil
}

// Close stops serving and releases the lock, clearing the pipe's name from instance.lock. It waits at most closeWait
// for a forwarded command line that onArgs is still handling; that goroutine then ends by itself once onArgs returns.
func (s *SingleInstance) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	stop, done, lock, writable := s.stop, s.done, s.lock, s.writable
	s.lock = 0 // publish, from here on, writes nothing
	s.mu.Unlock()
	if stop != 0 {
		_ = windows.SetEvent(stop)
		select {
		case <-done:
			_ = windows.CloseHandle(stop)
		case <-time.After(closeWait):
			// The goroutine still waits on the event when onArgs returns: the handle stays open for it.
			s.log.Warn("winos: a forwarded command line is still being handled; the lock is released without it")
		}
	}
	if lock == 0 {
		return nil
	}
	if writable {
		_ = writeLock(lock, nil)
	}
	return windows.CloseHandle(lock) // the lock goes with the handle
}
