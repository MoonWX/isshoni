//go:build !unix

package main

import "io/fs"

// fileOwner has no answer where files have no Unix owner (Windows is a development platform only, 04 §6.5): a
// backup then never writes into a file that exists (backupOutOwner, 04 §12.3).
func fileOwner(fs.FileInfo) (uid int, ok bool) { return 0, false }
