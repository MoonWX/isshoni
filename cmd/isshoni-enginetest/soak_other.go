//go:build !windows

package main

import "errors"

// sampleProcess reads the Windows process metrics of 04 §14's soak; elsewhere there is no engine to soak.
func sampleProcess() (procSample, error) {
	return procSample{}, errors.New("soak samples Windows process metrics: unsupported OS")
}
