package httpapi

import (
	"context"
	"fmt"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// The accounts part of 04's admin dashboard (README S70; 03 §12.4.8, 04 §11.4).

// Security events on the dashboard (03 §12.4.8): the newest 20 security-flagged audit rows of the last 7 days.
const (
	dashboardSecurityWindow = 7 * 24 * time.Hour
	dashboardSecurityMax    = 20
)

// DashboardAccounts builds the "accounts" object of 04's admin dashboard (03 §12.4.8), which 04's wiring calls for
// GET /api/v1/admin/dashboard (with a timeout of its own, 04 §11.4). It reads one snapshot of the store at the API's
// clock:
//   - users: the accounts by status (active, pending, disabled), and admins, the active admins (the ones the
//     last-admin rule counts, 03 §7.11);
//   - invites.active: invites that still work (not revoked, not expired, not used up);
//   - sessions.active: web sessions that are not past their idle or absolute expiry. A row the janitor has not
//     pruned yet is no session any more (03 §4.7, §12.4.3);
//   - devices.linked: the linked native apps, 0 until M2;
//   - registrationMode: the setting in force;
//   - securityEvents: up to 20 security-flagged audit rows (03 §10, column Sec) of the last 7 days, newest first,
//     in the form of GET /api/v1/admin/audit.
//
// It reads only. An error (the store, ctx) wraps the cause; 04's dashboard then leaves the part out and raises its
// dashboard.source_failed alert, it never fails the response (04 §11.4).
func (a *API) DashboardAccounts(ctx context.Context) (api.DashboardAccounts, error) {
	now := a.d.Clock()
	out := api.DashboardAccounts{RegistrationMode: api.RegistrationMode(a.d.DB.Settings().Get().RegistrationMode)}
	var events []store.AuditEntry
	err := a.d.DB.Read(ctx, func(q *store.Q) error {
		users, err := q.ListUsers("")
		if err != nil {
			return err
		}
		for _, u := range users {
			switch u.Status {
			case store.StatusActive:
				out.Users.Active++
				if u.Role == store.RoleAdmin {
					out.Users.Admins++
				}
			case store.StatusPending:
				out.Users.Pending++
			case store.StatusDisabled:
				out.Users.Disabled++
			}
			out.Devices.Linked += u.Devices
			if u.Sessions == 0 {
				continue
			}
			sessions, err := q.ListSessions(u.ID)
			if err != nil {
				return err
			}
			for _, s := range sessions {
				if sessionLive(s, now) {
					out.Sessions.Active++
				}
			}
		}
		if out.Invites.Active, err = q.CountActiveInvites("", now); err != nil {
			return err
		}
		events, err = q.SecurityEvents(now.Add(-dashboardSecurityWindow), dashboardSecurityMax)
		return err
	})
	if err != nil {
		return api.DashboardAccounts{}, fmt.Errorf("httpapi: dashboard accounts: %w", err)
	}
	out.SecurityEvents = apiAuditEntries(events)
	return out, nil
}
