//go:build !windows

package main

import (
	"errors"
	"runtime"
)

// errNoCapture: the screen capture is GDI's (01 §12.2). The decoder, the report and serve work everywhere.
var errNoCapture = errors.New("measure captures the screen with GDI, so it runs on Windows only")

func openGDI() (capturer, error) { return nil, errNoCapture }

func osVersion() string { return runtime.GOOS + "/" + runtime.GOARCH }
