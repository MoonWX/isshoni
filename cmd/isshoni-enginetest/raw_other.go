//go:build !windows

package main

import (
	"runtime"

	"github.com/MoonWX/isshoni/internal/media"
)

// rawProbe has no engine to call off Windows (the binding's Open already failed with unsupported_os).
func rawProbe(string, uint32) (*rawCaps, error) {
	return nil, &media.Error{Code: media.CodeUnsupportedOS, Op: "probe", Detail: "no engine DLL on " + runtime.GOOS}
}
