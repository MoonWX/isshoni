//go:build !windows

package main

import (
	"fmt"
	"os"
)

// On every OS but Windows the native audio engine does not exist yet (05 §18.1, §20): the harness builds (so the
// analyzer and verdict tests run on the Mac and CI ubuntu/macOS), but the subcommands that need the engine report
// that it is unavailable. M3 and M4 add driver_darwin.go and driver_linux.go.

// newDriver returns errUnsupportedOS: no engine on this OS.
func newDriver(driverOptions) (driver, error) { return nil, errUnsupportedOS }

// runFixture is never reached off Windows (a fixture is a Windows child of explorer.exe); it reports the error so a
// misdirected call fails cleanly rather than silently.
func runFixture([]string) int {
	fmt.Fprintln(os.Stderr, errUnsupportedOS)
	return exitSetup
}
