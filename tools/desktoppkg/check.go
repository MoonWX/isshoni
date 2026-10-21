package main

import "context"

// runCheck is `desktoppkg check` (06 §6.10, §9.3, §16): every binary in the stage is isshoni's own or matches a
// files glob of deploy/notices/native-windows.json, the NSIS sources have no quoted literal user-visible strings, and
// the installer stays within its size budget. Slice WA67 fills this file.
func runCheck(_ context.Context, _ *env, _ []string) error {
	return notImplemented("WA67")
}
