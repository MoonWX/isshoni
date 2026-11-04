//go:build !windows

package main

// processSetup has nothing to do off Windows: there is no engine DLL to protect (04 §12.1: the native engine exists
// on Windows only in M2), and every engine command answers exit code 2.
func processSetup() error { return nil }
