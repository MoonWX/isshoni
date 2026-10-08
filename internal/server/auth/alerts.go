package auth

import (
	"context"
	"log/slog"
)

// alert raises an admin alert (03 §7.11). The caller runs it after its commit. It logs one WARN line with the kind
// (never the actor or target: usernames are not logged) and hands the alert to Options.Alerts, 04's push service,
// which sends it to every admin whose adminAlerts preference is on. Without an alerter the log line is all there
// is; the audit row behind the alert still shows in the dashboard's security events.
func (s *Service) alert(ctx context.Context, a AdminAlert) {
	s.log.LogAttrs(ctx, slog.LevelWarn, "admin alert", slog.String("kind", a.Kind))
	if s.alerts != nil {
		s.alerts.AdminAlert(ctx, a)
	}
}
