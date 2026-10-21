package main

import "context"

// runStage is `desktoppkg stage [-release]` (06 §6.2, §9.1): copies exactly the install tree of 06 §2.2 into
// dist-desktop/stage/, the FFmpeg compliance files included; -release refuses a test build of isshoni.exe and a DLL
// whose --self-check reports test hooks. Slices WA67 and WA78 fill this file.
func runStage(_ context.Context, _ *env, _ []string) error {
	return notImplemented("WA67")
}
