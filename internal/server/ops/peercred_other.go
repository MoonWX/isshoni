//go:build !linux && !darwin

package ops

import (
	"errors"
	"net"
)

// The server ships for Linux, and macOS is the development platform; both check peer credentials. Everywhere else
// (Windows and the BSDs build the server for development only) there is no check: ListenAdmin says so once in the
// log, and the socket's file mode is the only guard.

// peerCredSupported is false: this build can't tell who is at the other end of a unix socket.
const peerCredSupported = false

var errPeerCredUnsupported = errors.New("ops: peer credentials are not available on this platform")

func peerUID(*net.UnixConn) (uint32, error) { return 0, errPeerCredUnsupported }
