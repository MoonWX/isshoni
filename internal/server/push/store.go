package push

import (
	"context"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
)

// Store is what the Service needs from 03's tables (push_subscriptions, push_preferences and the meta key
// vapid_key_fp; 04 §14.7). The wiring implements it over 03's *store.Q methods (04 §6.6), so push never imports
// store. Every method is one short transaction.
type Store interface {
	// Recipients returns the subscriptions f selects, of active users only (ListPushSubscriptions(PushFilter)).
	Recipients(ctx context.Context, f RecipientFilter) ([]Subscription, error)
	// RecordResult records a delivery (RecordPushResult): ok sets last_success_at to at and resets the failure
	// count, !ok adds a failure. A subscription that no longer exists is not an error: the adapter turns
	// store.ErrNotFound into nil.
	RecordResult(ctx context.Context, id string, ok bool, at time.Time) error
	// Delete deletes one subscription (DeletePushSubscriptionByID); a missing row is not an error.
	Delete(ctx context.Context, id string) error
	// DeleteAll deletes every subscription and returns how many there were (DeleteAllPushSubscriptions).
	DeleteAll(ctx context.Context) (int, error)
	// Prune deletes the subscriptions with at least minFailures failures in a row and no success since
	// noSuccessSince, and returns how many (PrunePushSubscriptions).
	Prune(ctx context.Context, minFailures int, noSuccessSince time.Time) (int, error)
	// Meta returns the value of a meta key (GetMeta). A key that does not exist is "" with a nil error: the
	// adapter turns store.ErrNotFound into that.
	Meta(ctx context.Context, key string) (string, error)
	// SetMeta inserts or replaces a meta key (SetMeta).
	SetMeta(ctx context.Context, key, value string) error
}

// RecipientFilter selects subscriptions; its fields map 1:1 to 03's store.PushFilter.
type RecipientFilter struct {
	ExcludeUserIDs []string
	AdminsOnly     bool
	Pref           string // PrefShareStarted | PrefAdminAlerts | "": keep only users whose preference allows it
	SessionID      string // "" = any; set = only that web session's subscriptions (push.test)
}

// Values of RecipientFilter.Pref (03's store.PrefShareStarted and store.PrefAdminAlerts).
const (
	PrefShareStarted = "share_started"
	PrefAdminAlerts  = "admin_alerts"
)

// Subscription is one browser's Web Push subscription (the wiring converts 03's store.PushSubscription).
type Subscription struct {
	ID, UserID, SessionID string
	Endpoint              logx.Secret // an https URL at the browser's push service; a capability, so never logged
	P256dh, Auth          string      // base64url: the browser's P-256 public key (65 bytes) and auth secret (16)
}
