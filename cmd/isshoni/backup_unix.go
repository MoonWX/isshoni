//go:build unix

package main

import (
	"io/fs"
	"syscall"
)

// fileOwner returns the uid that owns fi, for the owner check of an existing --out (backupOutOwner, 04 §12.3).
func fileOwner(fi fs.FileInfo) (uid int, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
