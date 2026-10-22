//go:build windows && !race && !isshoni_hw

package main

// raceEnabled: this test binary runs without the race detector.
const raceEnabled = false
