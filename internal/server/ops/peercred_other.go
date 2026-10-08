//go:build !linux && !darwin

package ops

import (
	"errors"
	"net"
)

// The server ships for Linux, and macOS is the development platform; both check peer credentials. Everywhere else
// there is no check, and ListenAdmin says so once in the log (noPeerCheckWarning). On the BSDs, which build the
// server for development only, the socket file's mode (0600) is then the only guard. On Windows, whose server
// build is experimental (06 §3), there is none: the mode means nothing there, the socket file has the access
// rights of its directory, and every local user who can open it can administer the server. That is a known limit
// of the Windows build, not something this file hides.

// peerCredSupported is false: this build can't tell who is at the other end of a unix socket.
const peerCredSupported = false

var errPeerCredUnsupported = errors.New("ops: peer credentials are not available on this platform")

func peerUID(*net.UnixConn) (uint32, error) { return 0, errPeerCredUnsupported }

// callerMayBeRefused is false: a server that checks nobody refuses nobody, so a server that hangs up before its
// first answer went away.
func callerMayBeRefused(string) bool { return false }
