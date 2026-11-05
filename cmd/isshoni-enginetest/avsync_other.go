//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
)

var errAVSyncWindows = errors.New("avsync's flash window and helper processes need Windows")

// newAVSyncHelpers starts avsync's helper processes on Windows; elsewhere there are none.
func newAVSyncHelpers(*globals) (avsyncHelpers, error) { return nil, errAVSyncWindows }

// runFlashRole is the flash window on Windows; elsewhere it exits 2 (environment).
func runFlashRole(_ context.Context, _ avsyncOptions, stderr io.Writer) int {
	fmt.Fprintln(stderr, "avsync flash:", errAVSyncWindows)
	return exitEnv
}
