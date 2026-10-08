//go:build windows

package ops

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// Winsock's numbers. syscall.ECONNREFUSED, ECONNRESET, EPIPE, ENOTCONN and ECONNABORTED exist on Windows too, but
// as invented values: connect, recv and send never return them, so a check for them would never match.
var (
	connRefusedErrnos = []syscall.Errno{windows.WSAECONNREFUSED}
	connResetErrnos   = []syscall.Errno{windows.WSAECONNRESET, windows.WSAECONNABORTED, windows.WSAENOTCONN}
)
