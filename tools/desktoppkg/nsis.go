package main

import "context"

// runNSIS is `desktoppkg nsis` (06 §6.2): the defines passed to makensis for the installer. Slice WA67 fills this
// file.
func runNSIS(_ context.Context, _ *env, _ []string) error {
	return notImplemented("WA67")
}
