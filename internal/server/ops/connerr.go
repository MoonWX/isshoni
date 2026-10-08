package ops

import (
	"errors"
	"syscall"
)

// The error numbers of a unix socket that nobody listens on, and of a connection that the other side closed, differ
// by platform: POSIX has ECONNREFUSED, ECONNRESET and friends, Winsock has its own WSAE* numbers, and the POSIX
// names that package syscall also declares on Windows are invented values that no call there returns. The two
// lists live in connerr_other.go and connerr_windows.go.

// isConnRefused reports whether a dial failed because nobody listens on the socket: a socket file that a server
// left behind (04 §12.1, "not running").
func isConnRefused(err error) bool { return isErrno(err, connRefusedErrnos) }

// isConnReset reports whether a read or write failed because the other side closed the connection.
func isConnReset(err error) bool { return isErrno(err, connResetErrnos) }

func isErrno(err error, errnos []syscall.Errno) bool {
	for _, e := range errnos {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}
