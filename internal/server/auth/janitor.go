package auth

import (
	"context"
	"log/slog"
	"time"

	"github.com/MoonWX/isshoni/internal/server/store"
)

// The janitor of 03 §4.7 (README S70): expired rows go away at startup and then every hour.

// janitorEvery is the time between two runs of the janitor after the one at startup (03 §4.7).
const janitorEvery = time.Hour

// RunJanitor prunes expired rows at once and then every hour, and blocks until ctx is done (03 §4.7). 04's serve
// starts it in its own goroutine once the server serves and cancels ctx on shutdown, before it closes the store.
//
// Each run is one store.DB.Prune at the service's clock: expired sessions (their push subscriptions go with them),
// setup tokens and reset links, invites that stopped working more than 30 days ago, pending sign-ups older than
// 14 days (each with a user.signup_expired audit row, actor system, in the same transaction), M2's device codes,
// devices without a live refresh token (each with a device.expired row, M2 02 §6.7) and tokens, and audit rows older
// than 30 days; then the store's upkeep (a passive WAL checkpoint, and PRAGMA optimize once a day). The debug line of
// a run counts each kind, the expired devices included; it names no row. Nothing else follows a run:
//   - no connection is closed and the session cache is left alone: an expired session is no credential any more
//     wherever it is looked at, and the hub finds a live connection's expired session at its next Revalidate
//     (03 §7.6, §7.7);
//   - no SPA is notified: an expired sign-up leaves the approval queue at the admins' next refetch (03 §12.5).
//
// A run that fails is logged at warn with the counts of what it committed, and the next one comes an hour later as
// usual: the rules only ever delete what is past its time, so a missed run costs nothing but an hour. A run that
// ctx ends is not logged; RunJanitor then returns.
func (s *Service) RunJanitor(ctx context.Context) {
	tick := time.NewTicker(janitorEvery)
	defer tick.Stop()
	for ctx.Err() == nil {
		s.janitorRun(ctx)
		select {
		case <-ctx.Done():
		case <-tick.C:
		}
	}
}

// janitorRun is one run of the janitor: Prune at s.now(), and one log line with the counts.
func (s *Service) janitorRun(ctx context.Context) {
	st, err := s.prune(ctx, s.now())
	switch {
	case err != nil && ctx.Err() != nil:
		// The server is shutting down; the store rolled the deletions back or committed them, both are fine.
		return
	case err != nil:
		s.log.LogAttrs(ctx, slog.LevelWarn, "janitor: pruning expired rows failed; it runs again in an hour",
			append(pruneAttrs(st), slog.String("err", err.Error()))...)
	default:
		s.log.LogAttrs(ctx, slog.LevelDebug, "janitor: pruned expired rows", pruneAttrs(st)...)
	}
}

// pruneAttrs are the counts of a prune as log attributes. No names, no IDs: the audit log has the expired sign-ups.
func pruneAttrs(st store.PruneStats) []slog.Attr {
	return []slog.Attr{
		slog.Int("sessions", st.Sessions),
		slog.Int("tokens", st.Tokens),
		slog.Int("invites", st.Invites),
		slog.Int("pending", st.Pending),
		slog.Int("device_codes", st.DeviceCodes),
		slog.Int("devices", st.Devices),
		slog.Int("device_tokens", st.DeviceTokens),
		slog.Int("audit", st.Audit),
	}
}
