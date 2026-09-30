//go:build !unix

package netx

import (
	"errors"
	"syscall"
)

// readSockBufs is not implemented off Unix (Windows is compile-only in M1): the sizes are unknown, so NewTransport
// reports 0 and logs no buffer warning.
func readSockBufs(syscall.Conn) (rcv, snd int, err error) {
	return 0, 0, errors.ErrUnsupported
}
