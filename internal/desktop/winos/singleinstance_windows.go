//go:build windows

package winos

import (
	"errors"
	"fmt"
	"log/slog"
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

var errStopped = errors.New("winos: stopped")

// SingleInstance is the single-instance lock and its pipe. Safe for concurrent use.
type SingleInstance struct {
	mutexName string
	pipeName  string
	sid       string
	log       *slog.Logger

	mu     sync.Mutex
	mutex  windows.Handle // held by the first instance until Close
	stop   windows.Handle // Serve's stop event
	done   chan struct{}
	closed bool
}

// NewSingleInstance prepares the lock for id (InstanceID; tests use their own). log nil means slog.Default().
func NewSingleInstance(id string, log *slog.Logger) (*SingleInstance, error) {
	if log == nil {
		log = slog.Default()
	}
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("winos: the user's SID: %w", err)
	}
	sid := u.User.Sid.String()
	return &SingleInstance{
		mutexName: `Local\` + id + ".v1",
		pipeName:  `\\.\pipe\` + id + ".v1." + sid,
		sid:       sid,
		log:       log.With("component", "winos"),
	}, nil
}

// Acquire takes the lock: first is true when no other instance of this user session holds it. A second instance
// calls Forward and exits.
func (s *SingleInstance) Acquire() (first bool, err error) {
	name, err := windows.UTF16PtrFromString(s.mutexName)
	if err != nil {
		return false, err
	}
	h, err := windows.CreateMutex(nil, false, name)
	switch {
	case errors.Is(err, windows.ERROR_ALREADY_EXISTS):
		_ = windows.CloseHandle(h) // the first instance holds the name; this process needn't
		return false, nil
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		return false, nil // a mutex of the same name we may not open: another instance (elevated) holds it
	case err != nil:
		return false, fmt.Errorf("winos: the single-instance mutex: %w", err)
	}
	s.mu.Lock()
	s.mutex = h
	s.mu.Unlock()
	return true, nil
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
	go s.serve(onArgs, sa)
	return nil
}

// pipeSecurity allows the current user only.
func (s *SingleInstance) pipeSecurity() (*windows.SecurityAttributes, error) {
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;" + s.sid + ")")
	if err != nil {
		return nil, fmt.Errorf("winos: the pipe's security descriptor: %w", err)
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	return sa, nil
}

func (s *SingleInstance) serve(onArgs func([]string), sa *windows.SecurityAttributes) {
	defer close(s.done)
	name, err := windows.UTF16PtrFromString(s.pipeName)
	if err != nil {
		return
	}
	// The first instance of the pipe must be ours (FILE_FLAG_FIRST_PIPE_INSTANCE: nobody squats the name).
	h, err := s.createPipe(name, sa, true)
	if err != nil {
		if !errors.Is(err, errStopped) {
			s.log.Warn("winos: the single-instance pipe can't be created; second launches can't reach this one",
				"err", err)
		}
		return
	}
	for {
		if s.stopped() { // Close ran while onArgs handled the last command line
			_ = windows.CloseHandle(h)
			return
		}
		data, err := s.accept(h)
		quit := errors.Is(err, errStopped)
		// The next instance is created before this one is closed, so the name is never free for another account to
		// create while this process serves; a launch that comes while onArgs runs connects to it and waits there.
		var next windows.Handle
		var nextErr error
		if !quit {
			next, nextErr = s.createPipe(name, sa, false)
		}
		_ = windows.DisconnectNamedPipe(h)
		_ = windows.CloseHandle(h)
		if quit {
			return
		}
		var args []string
		if err == nil {
			args, err = decodeArgs(data)
		}
		if err != nil {
			s.log.Info("winos: a second launch's message was dropped", "err", err)
		} else {
			onArgs(args)
		}
		if nextErr != nil {
			if !errors.Is(nextErr, errStopped) {
				s.log.Warn("winos: the single-instance pipe can't be created; second launches can't reach this one",
					"err", nextErr)
			}
			return
		}
		h = next
	}
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
	name, err := windows.UTF16PtrFromString(s.pipeName)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(ForwardTimeout)
	for {
		// SECURITY_IDENTIFICATION: the server may learn who this is, but never act as this user.
		h, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING,
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

// checkServer makes sure that the pipe's server runs as this user: another account that created the name while no
// first instance held it gets no command line.
func (s *SingleInstance) checkServer(pipe windows.Handle) error {
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(pipe, &pid); err != nil {
		return fmt.Errorf("winos: the pipe's server: %w", err)
	}
	p, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return fmt.Errorf("winos: the pipe's server: %w", err)
	}
	defer func() { _ = windows.CloseHandle(p) }()
	var tok windows.Token
	if err := windows.OpenProcessToken(p, windows.TOKEN_QUERY, &tok); err != nil {
		return fmt.Errorf("winos: the pipe's server: %w", err)
	}
	defer func() { _ = tok.Close() }()
	u, err := tok.GetTokenUser()
	if err != nil {
		return fmt.Errorf("winos: the pipe's server: %w", err)
	}
	if u.User.Sid.String() != s.sid {
		return errors.New("winos: the pipe is served by another account; nothing was forwarded")
	}
	return nil
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
