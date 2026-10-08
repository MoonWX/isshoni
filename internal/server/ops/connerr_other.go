//go:build !windows

package ops

import "syscall"

var (
	connRefusedErrnos = []syscall.Errno{syscall.ECONNREFUSED}
	// Which one a closed connection gives depends on the platform and on who was faster: a reset, a broken pipe
	// under a write, or "not connected" (macOS).
	connResetErrnos = []syscall.Errno{syscall.ECONNRESET, syscall.EPIPE, syscall.ENOTCONN, syscall.ECONNABORTED}
)
