//go:build unix

package netx

import (
	"errors"
	"runtime"
	"syscall"
)

// readSockBufs returns the effective SO_RCVBUF and SO_SNDBUF of a socket, read back with getsockopt. Linux reports
// twice the usable size (the kernel adds its bookkeeping overhead, socket(7)), so the value is halved there and
// compares directly with network.udp_buffer_bytes.
func readSockBufs(c syscall.Conn) (rcv, snd int, err error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var rerr, serr error
	cerr := rc.Control(func(fd uintptr) {
		rcv, rerr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF) //nolint:gosec // G115: a descriptor fits in an int
		snd, serr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF) //nolint:gosec // G115: as above
	})
	if err := errors.Join(cerr, rerr, serr); err != nil {
		return 0, 0, err
	}
	if runtime.GOOS == "linux" {
		rcv, snd = rcv/2, snd/2
	}
	return rcv, snd, nil
}
