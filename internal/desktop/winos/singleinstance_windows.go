//go:build windows

package winos

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
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
	// firstPipeWait bounds how long the first instance tries to create the pipe's name: an instance that is still
	// finishing (its client may still hold its last pipe instance, or a forwarded command line is still being
	// handled) can hold the name for a few seconds after it released the lock.
	firstPipeWait = 5 * time.Second
)

var (
	user32                       = windows.NewLazySystemDLL("user32.dll")
	procAllowSetForegroundWindow = user32.NewProc("AllowSetForegroundWindow")
)

// SingleInstance is the single-instance lock and its pipe. Safe for concurrent use.
type SingleInstance struct {
	names lockNames // from the user's instance secret (03 §13.3): never logged
	sid   string
	log   *slog.Logger

	mu     sync.Mutex
	mutex  windows.Handle // held by the first instance until Close
	stop   windows.Handle // Serve's stop event
	done   chan struct{}
	closed bool
}

// NewSingleInstance prepares the lock for id (InstanceID; tests use their own) and the data root root
// (%LOCALAPPDATA%\isshoni, or the e2e build's own): it reads the user's instance secret there, creating it on the first
// launch (instanceSecret). log nil means slog.Default().
func NewSingleInstance(id, root string, log *slog.Logger) (*SingleInstance, error) {
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "winos")
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("winos: the user's SID: %w", err)
	}
	sid := u.User.Sid.String()
	secret, err := instanceSecret(root, osSecretFiles(sid), log)
	if err != nil {
		return nil, err
	}
	return &SingleInstance{names: singleInstanceNames(id, secret), sid: sid, log: log}, nil
}

// Names returns the mutex's and the pipe's names (cmd/isshoni-desktop's TestAutostartRoundTrip checks that nothing
// holds them). They hold the user's instance secret: never log them.
func (s *SingleInstance) Names() (mutex, pipe string) { return s.names.mutex, s.names.pipe }

// userOnly is the security descriptor of the single-instance objects and the secret's files: owned by the user
// (explicitly, so an elevated instance's object isn't owned by BUILTIN\Administrators), open to the user and, with
// system, SYSTEM.
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

// osSecretFiles are the secret's files on Windows: created with CREATE_NEW and userOnly's security descriptor, and
// published with MoveFileExW, which fails on an existing name unless asked to replace it (os.Rename does).
func osSecretFiles(sid string) secretFiles {
	return secretFiles{
		createNew: func(path string) (*os.File, error) {
			sa, err := userOnly(sid, true)
			if err != nil {
				return nil, err
			}
			p, err := windows.UTF16PtrFromString(path)
			if err != nil {
				return nil, err
			}
			h, err := windows.CreateFile(p, windows.GENERIC_WRITE, 0, sa, windows.CREATE_NEW,
				windows.FILE_ATTRIBUTE_NORMAL, 0)
			if err != nil {
				return nil, &os.PathError{Op: "create", Path: path, Err: err} // ERROR_FILE_EXISTS is fs.ErrExist
			}
			return os.NewFile(uintptr(h), path), nil
		},
		publish: func(tmp, path string) error {
			from, err := windows.UTF16PtrFromString(tmp)
			if err != nil {
				return err
			}
			to, err := windows.UTF16PtrFromString(path)
			if err != nil {
				return err
			}
			if err := windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH); err != nil {
				return &os.LinkError{Op: "move", Old: tmp, New: path, Err: err} // ERROR_ALREADY_EXISTS is fs.ErrExist
			}
			return nil
		},
		replace: os.Rename,
	}
}

// errNotOurs is Acquire's error when an object under the secret name isn't this user's mutex.
var errNotOurs = errors.New("winos: the single-instance mutex's name holds an object that isn't this user's " +
	"(a corrupted or leaked instance secret)")

// Acquire takes the lock: first is true when no other instance of this user and data root, in any of the user's
// sessions, holds it. A second instance calls Forward and exits. An object under the name that isn't this user's
// mutex never makes this launch a second instance: Acquire returns an error, so the caller starts as the first instance
// without a lock (03 §13.3).
func (s *SingleInstance) Acquire() (first bool, err error) {
	sa, err := userOnly(s.sid, true)
	if err != nil {
		return false, err
	}
	state, h, err := s.createLock(s.names.mutex, sa)
	switch {
	case err != nil:
		return false, err
	case state == mutexCreated:
		s.mu.Lock()
		s.mutex = h
		s.mu.Unlock()
		return true, nil
	case state == mutexOfUser:
		return false, nil
	default:
		return false, errNotOurs
	}
}

// lockAccess is what a launch asks of a mutex it finds: to hold a handle and read the owner. Both are outside the
// write rights that an elevated instance's mutex label would deny a non-elevated one.
const lockAccess = windows.SYNCHRONIZE | windows.READ_CONTROL

// createLock creates the mutex name with sa, or finds whom an existing one belongs to. It returns the handle only
// when this process created the mutex (mutexCreated); a handle to an existing one is closed here. Its errors never
// hold the name.
func (s *SingleInstance) createLock(name string, sa *windows.SecurityAttributes) (mutexState, windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, 0, errors.New("winos: the single-instance mutex's name has a NUL")
	}
	h, err := windows.CreateMutexEx(sa, p, 0, lockAccess)
	switch {
	case err == nil:
		return stateOf(createdNew, false), h, nil
	case errors.Is(err, windows.ERROR_ALREADY_EXISTS):
		defer func() { _ = windows.CloseHandle(h) }()
		return stateOf(openedExists, s.ownedByUser(h)), 0, nil
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		return stateOf(deniedAccess, false), 0, nil
	case errors.Is(err, windows.ERROR_INVALID_HANDLE):
		return stateOf(otherObject, false), 0, nil
	default:
		return 0, 0, fmt.Errorf("winos: the single-instance mutex: %w", err)
	}
}

// ownerOf reads the owner SID of the kernel object or pipe h (a handle with READ_CONTROL).
func ownerOf(h windows.Handle) (string, error) {
	sd, err := windows.GetSecurityInfo(h, windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION)
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

// ownedByUser reports whether the kernel object h's owner is this user's SID; false when it can't be read.
func (s *SingleInstance) ownedByUser(h windows.Handle) bool {
	owner, err := ownerOf(h)
	return err == nil && owner == s.sid
}

// Serve delivers each forwarded command line to onArgs, one at a time, on a goroutine of its own, until Close. Only
// the first instance serves.
func (s *SingleInstance) Serve(onArgs func(args []string)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mutex == 0 {
		return ErrNotFirst
	}
	if s.closed || s.stop != 0 {
		return errors.New("winos: Serve called twice or after Close")
	}
	stop, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return fmt.Errorf("winos: %w", err)
	}
	sa, err := s.pipeSecurity()
	if err != nil {
		_ = windows.CloseHandle(stop)
		return err
	}
	s.stop = stop
	s.done = make(chan struct{})
	go s.serve(onArgs, sa, s.names.pipe)
	return nil
}

// pipeSecurity: owned by the user, set explicitly (Forward's serverCheck reads it), and open to the user only.
func (s *SingleInstance) pipeSecurity() (*windows.SecurityAttributes, error) {
	return userOnly(s.sid, false)
}

func (s *SingleInstance) serve(onArgs func([]string), sa *windows.SecurityAttributes, pipeName string) {
	defer close(s.done)
	name, err := windows.UTF16PtrFromString(pipeName)
	if err != nil {
		return
	}
	serveLoop(winPipeServer{s: s, name: name, sa: sa}, onArgs, s.log)
}

// winPipeServer is serveLoop's pipe: instances of the named pipe, in byte mode. A client may connect to an instance
// as soon as it exists (it is listening from CreateNamedPipe on), and what the client wrote stays readable after it
// closed its end, until the instance is disconnected.
type winPipeServer struct {
	s    *SingleInstance
	name *uint16
	sa   *windows.SecurityAttributes
}

func (p winPipeServer) create(first bool) (pipeInstance, error) {
	h, err := p.s.createPipe(p.name, p.sa, first)
	if err != nil {
		return nil, err
	}
	return winPipe{s: p.s, h: h}, nil
}

func (p winPipeServer) stopped() bool { return p.s.stopped() }

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

// createPipe creates an instance of the pipe. The first one takes FILE_FLAG_FIRST_PIPE_INSTANCE and is retried for
// up to firstPipeWait (another instance of this app that is finishing may still hold the name); later ones don't
// take the flag and are retried briefly. Close ends the retries (errStopped).
func (s *SingleInstance) createPipe(name *uint16, sa *windows.SecurityAttributes, first bool) (windows.Handle, error) {
	flags := uint32(windows.PIPE_ACCESS_INBOUND | windows.FILE_FLAG_OVERLAPPED)
	pauseMS, deadline := uint32(50), time.Now().Add(150*time.Millisecond)
	if first {
		flags |= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
		pauseMS, deadline = 100, time.Now().Add(firstPipeWait)
	}
	for {
		h, err := windows.CreateNamedPipe(name, flags,
			windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
			windows.PIPE_UNLIMITED_INSTANCES, 0, pipeBuffer, 0, sa)
		if err == nil || time.Now().After(deadline) {
			return h, err
		}
		if r, _ := windows.WaitForSingleObject(s.stop, pauseMS); r == windows.WAIT_OBJECT_0 {
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

// Forward sends args to the first instance, within ForwardTimeout (03 §13.3). It calls
// AllowSetForegroundWindow(ASFW_ANY) first, so the first instance may come to the front.
func (s *SingleInstance) Forward(args []string) error {
	data, err := encodeArgs(args)
	if err != nil {
		return err
	}
	_, _, _ = procAllowSetForegroundWindow.Call(uintptr(asfwAny))
	name, err := windows.UTF16PtrFromString(s.names.pipe)
	if err != nil {
		return errors.New("winos: the single-instance pipe's name has a NUL")
	}
	deadline := time.Now().Add(ForwardTimeout)
	for {
		// SECURITY_IDENTIFICATION: the server may learn who this is, but never act as this user. READ_CONTROL: to read
		// the pipe's owner (serverCheck).
		h, err := windows.CreateFile(name, windows.GENERIC_WRITE|windows.READ_CONTROL, 0, nil, windows.OPEN_EXISTING,
			windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
		if err == nil {
			defer func() { _ = windows.CloseHandle(h) }()
			if err := s.checkServer(h); err != nil {
				return err
			}
			for len(data) > 0 {
				var n uint32
				if err := windows.WriteFile(h, data, &n, nil); err != nil {
					return fmt.Errorf("winos: forwarding the arguments: %w", err)
				}
				data = data[n:]
			}
			return nil
		}
		// The first instance may not serve yet (it has just started), or be busy with another launch.
		if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) && !errors.Is(err, windows.ERROR_PIPE_BUSY) {
			return fmt.Errorf("winos: the first instance's pipe: %w", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("winos: the first instance didn't answer within %v: %w", ForwardTimeout, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// checkServer makes sure that the pipe is this user's: its owner, which the first instance sets to the user's SID and
// no other account can, or, when the owner can't be read, the server process's user (serverCheck, 03 §13.3).
func (s *SingleInstance) checkServer(pipe windows.Handle) error {
	owner, ownerErr := ownerOf(pipe)
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

// Close stops serving and releases the lock. It waits at most closeWait for a forwarded command line that onArgs is
// still handling; that goroutine then ends by itself once onArgs returns.
func (s *SingleInstance) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	stop, done, mutex := s.stop, s.done, s.mutex
	s.mutex = 0
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
	if mutex != 0 {
		return windows.CloseHandle(mutex)
	}
	return nil
}
