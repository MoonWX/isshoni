//go:build !linux && !darwin

package doctor

import (
	"errors"
	"io/fs"
)

// The server ships for Linux, and macOS is where it is developed; the other platforms only build it. There, file
// modes and owners are not checked, and the questions below have no answer: the checks skip that part.

const unixPerms = false

func diskFree(string) (uint64, error) { return 0, errors.ErrUnsupported }

func noFile() (uint64, error) { return 0, errors.ErrUnsupported }

func writable(string) error { return nil }

func fileOwner(fs.FileInfo) (uid int, ok bool) { return 0, false }

func kernelRelease() string { return "" }
