//go:build !windows

package update

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
)

// Other operating systems have no apply method in M2 (06 §8.9; M3 adds apply_darwin.go, the .app swap): Apply
// returns ErrUnsupported, no copy counts as installed, and no manifest has an installer for them (manifest.go's
// artifactKinds), so a check ends with no_artifact before any download.

func defaultSystem() system {
	return system{
		goos:      runtime.GOOS,
		goarch:    runtime.GOARCH,
		installed: func() (bool, error) { return false, ErrUnsupported },
		freeSpace: func(string) (uint64, error) { return 0, errors.ErrUnsupported },
		launch: func(context.Context, string, []string, *slog.Logger) error {
			return ErrUnsupported
		},
		policy: PolicyDisabled,
	}
}

// PolicyDisabled reports whether the machine policy turns the updater off (06 §8.8). Only Windows has one.
func PolicyDisabled() bool { return false }
