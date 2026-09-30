package store

import (
	"context"
	"fmt"
	"time"
)

// Retention periods of the janitor's rules (03 §4.7).
const (
	inviteRetention  = 30 * 24 * time.Hour // after an invite stops working
	pendingRetention = 14 * 24 * time.Hour // pending sign-ups
	deviceCodeGrace  = time.Hour           // device codes after their expiry (later: M2)
	auditRetention   = 30 * 24 * time.Hour
	optimizeEvery    = 24 * time.Hour // PRAGMA optimize in Prune's upkeep
)

// PruneStats counts the rows Prune deleted. Tokens counts setup tokens and password reset links together.
type PruneStats struct{ Sessions, Tokens, Invites, Pending, DeviceCodes, DeviceTokens, Audit int }

// Prune deletes expired rows by the rules of 03 §4.7 in one Write, then does the database upkeep of the janitor:
// PRAGMA wal_checkpoint(PASSIVE) and, at most once per 24 h of now, PRAGMA optimize. auth's janitor runs it at
// startup and then every hour.
//
// A row is deleted when, strictly (a row exactly at its limit stays):
//   - sessions: idle_expires_at < now (idle expiry is never later than absolute expiry); push subscriptions go with
//     them (cascade);
//   - setup_tokens, password_resets: expires_at < now;
//   - invites: inactive (revoked, expired or used up) since before now − 30 d. The time an invite became inactive is
//     the earliest of revoked_at, expires_at and, when used up, the sign-up time of its newest redeemer (created_at
//     when no redeemer is left);
//   - users with status pending: created_at < now − 14 d, each with a user.signup_expired audit row (actor system) in
//     the same transaction;
//   - device_codes (later: M2): expires_at < now − 1 h;
//   - device_tokens (later: M2): expires_at < now; spent refresh tokens are kept until then, for reuse detection;
//   - audit_log: at < now − 30 d.
//
// When only the upkeep fails, the error is returned with the counts of the committed deletions.
func (db *DB) Prune(ctx context.Context, now time.Time) (PruneStats, error) {
	var st PruneStats
	err := db.Write(ctx, func(q *Q) error {
		st = PruneStats{}
		return q.prune(now, &st)
	})
	if err != nil {
		return PruneStats{}, err
	}
	return st, db.upkeep(ctx, now)
}

func (q *Q) prune(now time.Time, st *PruneStats) error {
	ms := unixMS(now)
	var err error
	if st.Sessions, err = q.execCount("prune sessions", `DELETE FROM sessions WHERE idle_expires_at < ?`, ms); err != nil {
		return err
	}
	setup, err := q.execCount("prune setup tokens", `DELETE FROM setup_tokens WHERE expires_at < ?`, ms)
	if err != nil {
		return err
	}
	resets, err := q.execCount("prune password resets", `DELETE FROM password_resets WHERE expires_at < ?`, ms)
	if err != nil {
		return err
	}
	st.Tokens = setup + resets

	// never is later than every real time: MIN() ignores the conditions that don't hold.
	const never = int64(1<<63 - 1)
	st.Invites, err = q.execCount("prune invites", `DELETE FROM invites WHERE MIN(
			COALESCE(revoked_at, ?1),
			expires_at,
			CASE WHEN uses >= max_uses THEN COALESCE(
				(SELECT MAX(u.created_at) FROM users u WHERE u.invite_id = invites.id), created_at) ELSE ?1 END
		) < ?2`, never, unixMS(now.Add(-inviteRetention)))
	if err != nil {
		return err
	}

	type expired struct{ id, name string }
	var pending []expired
	err = q.writeReturning("prune pending users",
		`DELETE FROM users WHERE status = 'pending' AND created_at < ? RETURNING id, username`,
		[]any{unixMS(now.Add(-pendingRetention))}, func(r scanner) error {
			var e expired
			if err := r.Scan(&e.id, &e.name); err != nil {
				return err
			}
			pending = append(pending, e)
			return nil
		})
	if err != nil {
		return err
	}
	for _, e := range pending {
		err := q.AppendAudit(AuditEntry{At: now, Action: "user.signup_expired", Actor: SystemActor,
			TargetKind: "user", TargetID: e.id, TargetName: e.name})
		if err != nil {
			return err
		}
	}
	st.Pending = len(pending)

	if st.DeviceCodes, err = q.execCount("prune device codes", `DELETE FROM device_codes WHERE expires_at < ?`,
		unixMS(now.Add(-deviceCodeGrace))); err != nil {
		return err
	}
	if st.DeviceTokens, err = q.execCount("prune device tokens", `DELETE FROM device_tokens WHERE expires_at < ?`,
		ms); err != nil {
		return err
	}
	// Last, so that the signup_expired rows above (at now) are never candidates.
	st.Audit, err = q.execCount("prune audit log", `DELETE FROM audit_log WHERE at < ?`,
		unixMS(now.Add(-auditRetention)))
	return err
}

// upkeep checkpoints the WAL without blocking anyone and runs PRAGMA optimize when the last run is 24 h old.
func (db *DB) upkeep(ctx context.Context, now time.Time) error {
	if err := db.enter(); err != nil {
		return err
	}
	defer db.leave()
	db.maintMu.Lock()
	defer db.maintMu.Unlock()
	if _, err := db.writer.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)"); err != nil {
		return fmt.Errorf("store: prune: checkpoint: %w", err)
	}
	if now.Sub(db.optimizedAt) >= optimizeEvery {
		if _, err := db.writer.ExecContext(ctx, "PRAGMA optimize"); err != nil {
			return fmt.Errorf("store: prune: optimize: %w", err)
		}
		db.optimizedAt = now
	}
	return nil
}
