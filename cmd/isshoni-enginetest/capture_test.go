package main

import (
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/media/native"
)

// TestCaptureStartOptions: capture -bitrate-steps starts f with a 10 s GOP (04 §14), so checkBitrateSteps needs no
// periodic-IDR tolerance; a run without steps passes no options and takes the engine's default GOPs.
func TestCaptureStartOptions(t *testing.T) {
	g := native.GOPOf(captureStartOptions(captureOptions{steps: []int{8_000_000, 4_000_000}})...)
	if len(g) != 1 || g["f"] != 10*time.Second {
		t.Errorf("-bitrate-steps: GOPs %v, want only f at 10s", g)
	}
	if opts := captureStartOptions(captureOptions{}); len(opts) != 0 {
		t.Errorf("no steps: %d start options, want none", len(opts))
	}
}
