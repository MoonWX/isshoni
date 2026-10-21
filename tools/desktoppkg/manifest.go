package main

import "context"

// runManifest is `desktoppkg manifest` (06 §8.2, §11.1): writes the update manifest isshoni-desktop-update.json
// from the installer file in the unprivileged packaging job. Slice WA25 fills this file.
func runManifest(_ context.Context, _ *env, _ []string) error {
	return notImplemented("WA25")
}
