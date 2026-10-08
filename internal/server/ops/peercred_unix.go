//go:build linux || darwin

package ops

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// callerMayBeRefused reports whether the server on the socket at path can have turned this process away for its
// peer credentials (04 §12.1). It serves root and its own user, and its own user is the owner of the socket file,
// so only a caller who is neither can be refused. The AdminClient asks when a server hung up before its first
// answer: for root and for the socket's owner that was no refusal, the server went away.
func callerMayBeRefused(path string) bool {
	euid := os.Geteuid()
	if euid == 0 {
		return false
	}
	fi, err := os.Stat(path)
	if err != nil {
		// No socket file any more: the server stopped, and nobody was refused. Any other error leaves the question
		// open, and then the hang-up keeps the meaning that 04 §12.1 gives it.
		return !errors.Is(err, fs.ErrNotExist)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return !ok || int64(st.Uid) != int64(euid)
}
