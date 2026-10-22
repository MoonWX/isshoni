//go:build !windows

package winos

// SetAppUserModelID does nothing off Windows.
func SetAppUserModelID(string) error { return nil }
