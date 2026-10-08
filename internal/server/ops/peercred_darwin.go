//go:build darwin

package ops

import (
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// peerCredSupported says that this platform can tell who is at the other end of a unix socket.
const peerCredSupported = true

// peerUID returns the effective uid of the process that connected c, as the kernel recorded it when the connection
// was made (LOCAL_PEERCRED, 04 §12.1). The peer can't forge it.
func peerUID(c *net.UnixConn) (uint32, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var (
		cred    *unix.Xucred
		credErr error
	)
	err = raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED) //nolint:gosec // G115: a file descriptor fits in an int
	})
	if err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, os.NewSyscallError("getsockopt LOCAL_PEERCRED", credErr)
	}
	return cred.Uid, nil
}
