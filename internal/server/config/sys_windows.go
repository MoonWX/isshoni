//go:build windows

package config

import (
	"errors"
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

// unixPerms is false: Windows has no Unix file modes or owners, so the mode and owner checks of §5.1 and §5.2 are
// skipped. Windows is a development platform only (§6.5).
const unixPerms = false

// setUmask does nothing: Windows has no umask.
func setUmask(int) int { return 0 }

// isReadOnlyFS reports whether err says the file or the medium is read-only.
func isReadOnlyFS(err error) bool {
	return errors.Is(err, windows.ERROR_WRITE_PROTECT) || errors.Is(err, windows.ERROR_FILE_READ_ONLY)
}

// fileOwner is not available on Windows.
func fileOwner(fs.FileInfo) (uid int, ok bool) { return 0, false }

// lockFile takes an exclusive LockFileEx lock on the first byte of f without blocking (§5.1). It returns errLockHeld
// when another handle holds it. Closing f releases the lock.
func lockFile(f *os.File) error {
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, new(windows.Overlapped))
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return errLockHeld
	}
	return err
}
