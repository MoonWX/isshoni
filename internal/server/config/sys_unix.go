//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd

package config

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// unixPerms reports that file modes and owners mean what they say on this platform, so the data-directory and
// secrets.json checks of §5.1 and §5.2 look at them.
const unixPerms = true

// setUmask sets the process umask and returns the previous one.
func setUmask(mask int) int { return syscall.Umask(mask) }

// isReadOnlyFS reports whether err says the filesystem is read-only.
func isReadOnlyFS(err error) bool { return errors.Is(err, syscall.EROFS) }

// fileOwner returns the uid that owns fi.
func fileOwner(fi fs.FileInfo) (uid int, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// lockFile takes an exclusive flock on f without blocking. It returns errLockHeld when another open file
// description holds the lock: another process, or another LockDataDir in this one. Closing f releases the lock, and
// so does exec (the descriptor is close-on-exec), so a re-exec'd server takes it again (§6.5).
func lockFile(f *os.File) error {
	fd := int(f.Fd()) //nolint:gosec // G115: a file descriptor fits in an int
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			return errLockHeld
		default:
			return err
		}
	}
}
