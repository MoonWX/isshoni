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
	// The first instance of the pipe must be ours (FILE_FLAG_FIRST_PIPE_INSTANCE: nobody squats the name). Later
	// instances don't take the flag: a client that still holds a handle keeps its old instance alive for a while.
	flags := uint32(windows.PIPE_ACCESS_INBOUND | windows.FILE_FLAG_OVERLAPPED | windows.FILE_FLAG_FIRST_PIPE_INSTANCE)
	for {
		var h windows.Handle
		for attempt := 0; ; attempt++ {
			h, err = windows.CreateNamedPipe(name, flags,
				windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
				windows.PIPE_UNLIMITED_INSTANCES, 0, pipeBuffer, 0, sa)
			if err == nil || attempt == 2 || flags&windows.FILE_FLAG_FIRST_PIPE_INSTANCE != 0 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err != nil {
			s.log.Warn("winos: the single-instance pipe can't be created; second launches can't reach this one",
				"err", err)
			return
		}
		flags &^= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
		data, err := s.accept(h)
		_ = windows.DisconnectNamedPipe(h)
		_ = windows.CloseHandle(h)
		switch {
		case errors.Is(err, errStopped):
			return
		case err != nil:
			s.log.Info("winos: a second launch's message was dropped", "err", err)
			continue
		}
		args, err := decodeArgs(data)
		if err != nil {
			s.log.Info("winos: a second launch's message was dropped", "err", err)
			continue
		}
		onArgs(args)
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
	case err == nil, errors.Is(err, windows.ERROR_PIPE_CONNECTED):
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
		h, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
		if err == nil {
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

// Close stops serving and releases the lock.
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
		<-done
		_ = windows.CloseHandle(stop)
	}
	if mutex != 0 {
		return windows.CloseHandle(mutex)
	}
	return nil
}
