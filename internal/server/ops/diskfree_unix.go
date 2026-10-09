//go:build linux || darwin

package ops

import "golang.org/x/sys/unix"

// freeBytes returns the bytes that the process may still write on the filesystem that holds dir. ok is false when
// the platform can't tell.
func freeBytes(dir string) (free uint64, ok bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), true //nolint:gosec,unconvert // the field types differ per OS
}
