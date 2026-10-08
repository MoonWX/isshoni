package ops

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
)

// The platform's error numbers for "nobody listens on the socket" and "the other side closed the connection",
// wrapped the way package net returns them, are what ListenAdmin and the client classify by (04 §12.1).
func TestConnErrnos(t *testing.T) {
	if len(connRefusedErrnos) == 0 || len(connResetErrnos) == 0 {
		t.Fatalf("no error numbers for this platform: refused %v, reset %v", connRefusedErrnos, connResetErrnos)
	}
	wrap := func(op, call string, e syscall.Errno) error {
		return &net.OpError{Op: op, Net: "unix", Err: os.NewSyscallError(call, e)}
	}
	c := DialAdmin("admin.sock")
	for _, e := range connRefusedErrnos {
		err := wrap("dial", "connect", e)
		if !isConnRefused(err) || isConnReset(err) || isHangup(err) {
			t.Errorf("errno %d (%v): refused=%v reset=%v hangup=%v, want a refused dial only", e, e, isConnRefused(err), isConnReset(err), isHangup(err))
		}
		// A stale socket file is "not running".
		if got := c.dialError(t.Context(), err); !errors.Is(got, ErrAdminNotRunning) {
			t.Errorf("dialError(errno %d) = %v, want ErrAdminNotRunning", e, got)
		}
	}
	for _, e := range connResetErrnos {
		err := wrap("read", "read", e)
		if !isConnReset(err) || !isHangup(err) || isConnRefused(err) {
			t.Errorf("errno %d (%v): reset=%v hangup=%v refused=%v, want a closed connection only", e, e, isConnReset(err), isHangup(err), isConnRefused(err))
		}
	}
	for _, err := range []error{io.EOF, io.ErrUnexpectedEOF} {
		if !isHangup(err) || isConnReset(err) {
			t.Errorf("%v: hangup=%v reset=%v, want a hang-up that is no error number", err, isHangup(err), isConnReset(err))
		}
	}
	for _, err := range []error{nil, context.DeadlineExceeded, os.ErrDeadlineExceeded, errors.New("malformed HTTP response"), wrap("read", "read", syscall.EINVAL)} {
		if isConnRefused(err) || isConnReset(err) || isHangup(err) {
			t.Errorf("%v: refused=%v reset=%v hangup=%v, want none of them", err, isConnRefused(err), isConnReset(err), isHangup(err))
		}
	}
}
