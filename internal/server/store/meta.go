package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Meta keys are facts about this database (03 §5): created_at (unix ms), last_app_version, session_key_fp,
// invite_key_fp, and 04's internal ops state (vapid_key_fp and the ops.* keys, whose values are JSON).

// GetMeta returns the value of a meta key, or ErrNotFound.
func (q *Q) GetMeta(key string) (string, error) {
	var v string
	err := q.tx.QueryRowContext(q.ctx, "SELECT value FROM meta WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: get meta %s: %w", key, err)
	}
	return v, nil
}

// SetMeta inserts or replaces a meta key.
func (q *Q) SetMeta(key, value string) error {
	_, err := q.exec(
		"INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value",
		key, value)
	if err != nil {
		return fmt.Errorf("store: set meta %s: %w", key, err)
	}
	return nil
}

// setMetaIfMissing inserts a meta key unless it exists.
func (q *Q) setMetaIfMissing(key, value string) error {
	_, err := q.exec("INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO NOTHING", key, value)
	if err != nil {
		return fmt.Errorf("store: set meta %s: %w", key, err)
	}
	return nil
}

// ---- transfer accounting (04 §11.3): socket-level byte totals per UTC month, no per-user data ----

// AddTransfer adds byte counts to a UTC month ("2026-09") of 04's transfer accounting.
func (q *Q) AddTransfer(month string, egress, ingress int64, now time.Time) error {
	return notImplemented("AddTransfer")
}

// TransferMonth returns the byte counts of a month (zero when there is no row).
func (q *Q) TransferMonth(month string) (egress, ingress int64, err error) {
	return 0, 0, notImplemented("TransferMonth")
}
