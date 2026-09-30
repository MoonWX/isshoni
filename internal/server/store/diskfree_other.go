//go:build !linux && !darwin

package store

// diskFree is not implemented on this platform, so the free-space check before a pre-migration backup is skipped.
// The server ships for Linux; other platforms only build it for development.
func diskFree(string) (free uint64, ok bool, err error) { return 0, false, nil }
