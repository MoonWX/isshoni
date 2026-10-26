//go:build !windows

package winos

// HoldRunningMutex does nothing off Windows: no installer waits for the macOS dev build.
func HoldRunningMutex(string) error { return nil }

// MutexExists is always false off Windows.
func MutexExists(string) (bool, error) { return false, nil }
