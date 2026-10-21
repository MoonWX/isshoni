package main

import "context"

// runWinver is `desktoppkg winver <semver>` (06 §5.3): prints the four 16-bit numbers of the PE version resources
// and NSIS's VIProductVersion (0.2.0 → 0.2.0.65000, 0.2.0-rc.2 → 0.2.0.2, 0.0.0-dev+x → 0.0.0.0). Slice WA67 fills
// this file.
func runWinver(_ context.Context, _ *env, _ []string) error {
	return notImplemented("WA67")
}
