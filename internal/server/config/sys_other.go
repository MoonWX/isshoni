//go:build !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd && !windows

package config

import (
	"io/fs"
	"os"
)

// The server ships for Linux; the other platforms build it for development only. On these, file modes, owners, the
// umask and the data-directory lock are not checked.

const unixPerms = false

func setUmask(int) int { return 0 }

func isReadOnlyFS(error) bool { return false }

func fileOwner(fs.FileInfo) (uid int, ok bool) { return 0, false }

func lockFile(*os.File) error { return nil }
