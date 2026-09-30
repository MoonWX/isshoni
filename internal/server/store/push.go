package store

import "time"

// PushSubscription is a Web Push subscription of a web session (or, later, a device). 04's sender uses it.
type PushSubscription struct {
	ID            PushSubID
	UserID        UserID
	SessionID     SessionID // web: dies with its session
	DeviceID      DeviceID  // later (native apps)
	Endpoint      string    // https URL, ≤ 2048 bytes
	P256dh        string    // base64url, 65-byte uncompressed P-256 point
	Auth          string    // base64url, 16 bytes
	Name          string    // "Safari on iPhone"
	CreatedAt     time.Time
	LastSuccessAt time.Time
	Failures      int
}

// PushFilter selects subscriptions for ListPushSubscriptions. Only users with status active are ever returned.
type PushFilter struct {
	UserIDs        []UserID // empty = all users
	ExcludeUserIDs []UserID
	AdminsOnly     bool
	Pref           string    // "" | "share_started" | "admin_alerts": keep only users whose preference allows it
	SessionID      SessionID // "" = any; set = only that session's subscriptions (push test)
}

// PushPreferences are a user's notification choices; a missing row means the defaults (all, true).
type PushPreferences struct {
	ShareStarted string // "all" | "off"
	AdminAlerts  bool
}

// UpsertPushSubscription inserts s (setting s.ID) or, on an endpoint conflict, rebinds that row to s.UserID and
// s.SessionID and replaces its keys.
func (q *Q) UpsertPushSubscription(s *PushSubscription) (created bool, err error) {
	return false, notImplemented("UpsertPushSubscription")
}

// ListPushSubscriptions returns the subscriptions f selects.
func (q *Q) ListPushSubscriptions(f PushFilter) ([]PushSubscription, error) {
	return nil, notImplemented("ListPushSubscriptions")
}

// DeletePushSubscription deletes one of the user's subscriptions and reports whether it existed.
func (q *Q) DeletePushSubscription(u UserID, id PushSubID) (bool, error) {
	return false, notImplemented("DeletePushSubscription")
}

// DeletePushSubscriptionByEndpoint deletes the user's subscription with that endpoint.
func (q *Q) DeletePushSubscriptionByEndpoint(u UserID, endpoint string) (bool, error) {
	return false, notImplemented("DeletePushSubscriptionByEndpoint")
}

// DeletePushSubscriptionByID deletes a subscription (04's sender, on 404/410/401/403).
func (q *Q) DeletePushSubscriptionByID(id PushSubID) error {
	return notImplemented("DeletePushSubscriptionByID")
}

// DeleteAllPushSubscriptions deletes every subscription (04: the VAPID key changed).
func (q *Q) DeleteAllPushSubscriptions() (int, error) {
	return 0, notImplemented("DeleteAllPushSubscriptions")
}

// PrunePushSubscriptions deletes subscriptions with at least minFailures failures and no success since
// noSuccessSince (04's daily job).
func (q *Q) PrunePushSubscriptions(minFailures int, noSuccessSince time.Time) (int, error) {
	return 0, notImplemented("PrunePushSubscriptions")
}

// RecordPushResult counts a failure, or resets the failures and sets last_success_at.
func (q *Q) RecordPushResult(id PushSubID, ok bool, now time.Time) error {
	return notImplemented("RecordPushResult")
}

// TrimPushSubscriptions deletes the user's oldest subscriptions beyond keep.
func (q *Q) TrimPushSubscriptions(u UserID, keep int) error {
	return notImplemented("TrimPushSubscriptions")
}

// PushPreferences returns the user's preferences, or the defaults when there is no row.
func (q *Q) PushPreferences(u UserID) (PushPreferences, error) {
	return PushPreferences{}, notImplemented("PushPreferences")
}

// SetPushPreferences stores the user's preferences.
func (q *Q) SetPushPreferences(u UserID, p PushPreferences, now time.Time) error {
	return notImplemented("SetPushPreferences")
}
