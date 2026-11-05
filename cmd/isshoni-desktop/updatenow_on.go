//go:build (windows || (darwin && cgo)) && updatetest

package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/MoonWX/isshoni/internal/update"
	"github.com/MoonWX/isshoni/internal/version"
)

// updateNowTimeout bounds --update-now: a check with its download, then the Apply (WA88's end to end allows 120 s
// for the whole update).
const updateNowTimeout = 10 * time.Minute

// updateNowMode is --update-now in builds with the updatetest tag (06 §8.11): the updater e2e's CI builds, never a
// release (06 §5.2 step 3 fails a binary whose tags aren't exactly production).
func updateNowMode(log *slog.Logger) func() int {
	return func() int {
		ctx, cancel := context.WithTimeout(context.Background(), updateNowTimeout)
		defer cancel()
		return updateNow(ctx, log, openNowUpdater)
	}
}

// openNowUpdater is --update-now's updater: the data root's, with 06's defaults (enabled unless the machine policy
// says otherwise, the stable channel, which a test build widens to every channel).
func openNowUpdater() (nowUpdater, error) {
	root, err := dataRoot()
	if err != nil {
		return nil, err
	}
	u, err := update.New(update.Options{CurrentVersion: version.Version(), DataDir: root, Logger: lateLogger()})
	if err != nil {
		return nil, err
	}
	return u, nil
}
