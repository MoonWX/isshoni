//go:build linux || darwin

package doctor

import (
	"fmt"
	"io/fs"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// unixPerms reports that file modes and owners mean what they say on this platform.
const unixPerms = true

// diskFree returns the bytes available to an unprivileged user on the filesystem that holds dir.
func diskFree(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("statfs: %w", err)
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil //nolint:gosec,unconvert // field types differ per OS
}

// noFile returns the soft limit of open files. Go raises it to the hard limit when the process starts, so this is
// what the process can really open.
func noFile() (uint64, error) {
	var lim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &lim); err != nil {
		return 0, fmt.Errorf("getrlimit: %w", err)
	}
	return uint64(lim.Cur), nil //nolint:gosec,unconvert // the field type differs per OS
}

// writable asks the kernel whether the process may create files in dir, without creating one: doctor never writes
// to the data directory.
func writable(dir string) error {
	return unix.Access(dir, unix.W_OK|unix.X_OK)
}

// fileOwner returns the uid that owns fi.
func fileOwner(fi fs.FileInfo) (uid int, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t) // what package os puts there
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// kernelRelease returns the kernel's release (uname -r), or "".
func kernelRelease() string {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return ""
	}
	return strings.TrimRight(string(u.Release[:]), "\x00")
}
