package ops

import (
	"context"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// This file declares what ops needs from the rest of the server (04 §7.7, §11.3–§11.5). ops may not import store,
// auth, signal or sfu (04 §2), so the wiring (internal/server, 04 §6.6) implements each of these in a few lines,
// and this package compiles and tests without them. TransferStore is in transfer.go and AdminAccounts in
// adminsock.go, next to their only users.

// Policy is the only way ops reads policy settings (04 §11.3, §11.5), never config.Config: an admin changes them at
// runtime, and a TOML policy key can pin them. The wiring backs it with 03's SettingsCache.Get(), and ops calls it
// on every tick, so a change applies without a restart. Both methods must be quick and safe for concurrent use.
type Policy interface {
	// TransferAlertGB is the setting transferAlertGb: the monthly egress, in GB of 10⁹ bytes, at 80 % and 100 % of
	// which the admins are alerted. 0 means no alerts.
	TransferAlertGB() int
	// ReleaseCheck is the setting updateCheck: whether the server may ask GitHub for the list of releases.
	ReleaseCheck() bool
}

// LiveSource is the live state of the dashboard (04 §11.4): the wiring adapts 01's Hub.Snapshot() and 02's
// SFU.Snapshot(). The dashboard that reads it comes with README slice S85.
type LiveSource interface {
	// Rooms returns the rooms that have participants, with their connections, shares, layers and viewers.
	Rooms(ctx context.Context) []api.RoomLive
	// Media returns the SFU's totals; probe peer connections are not counted.
	Media(ctx context.Context) api.MediaTotals
}

// AccountsSource is the accounts part of the dashboard (04 §11.4): 03's API.DashboardAccounts, which the dashboard
// calls with a 1 s timeout. The dashboard that reads it comes with README slice S85.
type AccountsSource interface {
	Accounts(ctx context.Context) (api.DashboardAccounts, error)
}

// Prober starts a connection-test probe on the SFU (04 §7.7). The wiring adapts 02's SFU.Probe: it converts userID
// and transport to sfu.UserID and sfu.ProbeTransport, discards the result channel, and maps sfu.probe_limit to 429
// rate_limited and sfu.transport_disabled to 409 transport_disabled. The endpoint that calls it,
// POST /api/v1/conntest, comes with README slice S80.
type Prober interface {
	// Probe answers offerSDP, an offer with one data channel, with an answer that holds only the candidates of
	// transport ("udp", "tcp443" or "tcp7882").
	Probe(ctx context.Context, userID string, transport string, offerSDP string) (answerSDP string, err error)
}

// MetaStore is 03's meta table (03 §5) as ops uses it: one small JSON value per key, for the state that must
// survive a restart (MetaTransferAlertSent, MetaReleaseCheck, MetaDoctorLast). The wiring implements it over
// (*store.Q).GetMeta and SetMeta, each call one short transaction.
type MetaStore interface {
	// Meta returns the value of a key. A key that does not exist is "" with a nil error: the adapter turns
	// store.ErrNotFound into that.
	Meta(ctx context.Context, key string) (string, error)
	// SetMeta inserts or replaces a key.
	SetMeta(ctx context.Context, key, value string) error
}

// The meta keys of ops (03 §5 lists them as 04's internal state).
const (
	// MetaTransferAlertSent remembers the highest transfer threshold that was alerted and its month, as the JSON
	// string "2026-09:80" (04 §11.3); "2026-09:0" after the limit was raised above what the month has used.
	MetaTransferAlertSent = "ops.transfer_alert_sent"
	// MetaReleaseCheck holds the last release check as a JSON object: when it ran, the feed's ETag and the releases
	// it listed (04 §11.5).
	MetaReleaseCheck = "ops.release_check"
	// MetaDoctorLast holds the last full doctor report as a JSON object (04 §13.1), so that the dashboard has its
	// summary right after a restart.
	MetaDoctorLast = "ops.doctor_last"
)

// AdminAlert is an alert that ops raises for the admins (03 §7.11). The only one is transfer_threshold (04 §11.3).
// Its fields are those of 03's auth.AdminAlert and of push.AdminAlert.
type AdminAlert struct {
	Kind   api.AdminAlertKind
	Actor  string // AlertActorSystem: the server itself raised it
	Target string // transfer_threshold: the threshold that was crossed, "80" or "100"
	At     time.Time
}

// AlertActorSystem is the Actor of an alert that no user caused.
const AlertActorSystem = "system"

// AdminAlerter delivers an AdminAlert to the admins: as a Web Push message admin.alert to those who want admin
// alerts (04 §14.3), through the same path as 03's alerts. The wiring implements it over push.Service.AdminAlert;
// with push.enabled = false there is none, and the alert is only logged and shown on the dashboard. AdminAlert
// must not block: the push service queues.
type AdminAlerter interface {
	AdminAlert(ctx context.Context, a AdminAlert)
}
