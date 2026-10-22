//go:build !windows

package audio

// readDucking reports the ducking preference as unknown off Windows (e2ehost, CI, the Mac), which hides the tip.
func readDucking() duckingPref { return duckingUnknown }
