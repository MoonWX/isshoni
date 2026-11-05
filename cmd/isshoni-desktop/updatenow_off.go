//go:build (windows || (darwin && cgo)) && !updatetest

package main

import "log/slog"

// updateNowMode is nil without the updatetest tag: --update-now then logs an error and exits 2 (03 §13.4, 06 §8.11).
func updateNowMode(*slog.Logger) func() int { return nil }
