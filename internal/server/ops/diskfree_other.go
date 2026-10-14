//go:build !linux && !darwin

package ops

// diskFree is not implemented on this platform, so a backup and a restore do not check the free space before they
// write; a disk that fills up is still reported when a write fails. The server ships for Linux; other platforms
// only build it for development.
func diskFree(string) (free uint64, ok bool) { return 0, false }
