//go:build !linux && !darwin

package ops

// freeBytes is not implemented on this platform, so a restore does not check the free space before it receives its
// body; a disk that fills up is still reported when a write fails. The server ships for Linux; other platforms
// only build it for development.
func freeBytes(string) (free uint64, ok bool) { return 0, false }
