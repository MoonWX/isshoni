package store

import (
	"context"
	"time"
)

// PruneStats counts the rows Prune deleted.
type PruneStats struct{ Sessions, Tokens, Invites, Pending, DeviceCodes, DeviceTokens, Audit int }

// Prune deletes expired rows by the rules of 03 §4.7. auth's janitor runs it every hour.
func (db *DB) Prune(ctx context.Context, now time.Time) (PruneStats, error) {
	return PruneStats{}, notImplemented("Prune")
}
