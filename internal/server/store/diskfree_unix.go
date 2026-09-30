//go:build linux || darwin

package store

import (
	"fmt"
	"syscall"
)

// diskFree returns the bytes available to an unprivileged user on the filesystem that holds dir.
func diskFree(dir string) (free uint64, ok bool, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false, fmt.Errorf("statfs: %w", err)
	}
	return uint64(st.Bavail) * uint64(st.Bsize), true, nil //nolint:gosec,unconvert // field types differ per OS
}
