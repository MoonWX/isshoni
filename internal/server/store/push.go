package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

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
	CreatedAt     time.Time // the latest subscribe (UpsertPushSubscription), not the first
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

// Push preference names for PushFilter.Pref.
const (
	PrefShareStarted = "share_started"
	PrefAdminAlerts  = "admin_alerts"
)

// PushPreferences are a user's notification choices; a missing row means the defaults (all, true).
type PushPreferences struct {
	ShareStarted string // "all" | "off"
	AdminAlerts  bool
}

// defaultPushPreferences are the preferences of a user without a push_preferences row.
var defaultPushPreferences = PushPreferences{ShareStarted: "all", AdminAlerts: true}

// pushCols are the columns scanned by pushScan, in its order.
const pushCols = `id, user_id, session_id, device_id, endpoint, p256dh, auth_secret, name, created_at, last_success_at,
	failures`

type pushScan struct {
	p               PushSubscription
	id, user        string
	session, device sql.NullString
	created         int64
	lastSuccess     sql.NullInt64
}

func (s *pushScan) dest() []any {
	return []any{&s.id, &s.user, &s.session, &s.device, &s.p.Endpoint, &s.p.P256dh, &s.p.Auth, &s.p.Name, &s.created,
		&s.lastSuccess, &s.p.Failures}
}

func (s *pushScan) sub() PushSubscription {
	out := s.p
	out.ID = PushSubID(s.id)
	out.UserID = UserID(s.user)
	out.SessionID = SessionID(s.session.String)
	out.DeviceID = DeviceID(s.device.String)
	out.CreatedAt = fromMS(s.created)
	out.LastSuccessAt = fromNullMS(s.lastSuccess)
	return out
}

// UpsertPushSubscription inserts s and sets s.ID (created = true), or, when a row with s.Endpoint exists, rebinds
// that row to s.UserID, s.SessionID and s.DeviceID, replaces its keys and name, resets its failure count and sets
// s.ID to its ID (created = false). Exactly one of SessionID and DeviceID must be set (a CHECK). A zero CreatedAt is
// the store's clock.
//
// Every upsert, a rebind included, sets the row's CreatedAt to s.CreatedAt: the SPA subscribes again at every app
// start (03 §12.4.6), so CreatedAt is the latest subscription and TrimPushSubscriptions evicts the browsers that
// have not re-subscribed for the longest time, not the ones that subscribed first. A rebind to the same user keeps
// LastSuccessAt; a rebind to another user clears it, since it is a new subscription for that user. s is updated
// from the stored row (ID, CreatedAt, LastSuccessAt, Failures).
func (q *Q) UpsertPushSubscription(s *PushSubscription) (created bool, err error) {
	s.CreatedAt = q.orNow(s.CreatedAt)
	var gotID string
	var gotCreated int64
	var gotSuccess sql.NullInt64
	var gotFailures int
	newID, err := q.withNewID("push_subscriptions", func(id string) error {
		return q.writeReturning("upsert push subscription", `INSERT INTO push_subscriptions
				(id, user_id, session_id, device_id, endpoint, p256dh, auth_secret, name, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (endpoint) DO UPDATE SET
				user_id = excluded.user_id, session_id = excluded.session_id, device_id = excluded.device_id,
				p256dh = excluded.p256dh, auth_secret = excluded.auth_secret, name = excluded.name, failures = 0,
				created_at = excluded.created_at,
				last_success_at = CASE WHEN user_id = excluded.user_id THEN last_success_at END
			RETURNING id, created_at, last_success_at, failures`,
			[]any{id, string(s.UserID), strOrNull(string(s.SessionID)), strOrNull(string(s.DeviceID)), s.Endpoint,
				s.P256dh, s.Auth, s.Name, unixMS(s.CreatedAt)},
			func(r scanner) error { return r.Scan(&gotID, &gotCreated, &gotSuccess, &gotFailures) })
	})
	if err != nil {
		return false, err
	}
	s.ID = PushSubID(gotID)
	s.CreatedAt = fromMS(gotCreated)
	s.LastSuccessAt = fromNullMS(gotSuccess)
	s.Failures = gotFailures
	return gotID == newID, nil
}

// ListPushSubscriptions returns the subscriptions f selects, of active users only, ordered by user and then by
// creation. An unknown f.Pref is an error.
func (q *Q) ListPushSubscriptions(f PushFilter) ([]PushSubscription, error) {
	var where []string
	var args []any
	where = append(where, `u.status = 'active'`)
	if len(f.UserIDs) > 0 {
		where = append(where, `p.user_id IN (`+placeholders(len(f.UserIDs))+`)`)
		for _, id := range f.UserIDs {
			args = append(args, string(id))
		}
	}
	if len(f.ExcludeUserIDs) > 0 {
		where = append(where, `p.user_id NOT IN (`+placeholders(len(f.ExcludeUserIDs))+`)`)
		for _, id := range f.ExcludeUserIDs {
			args = append(args, string(id))
		}
	}
	if f.AdminsOnly {
		where = append(where, `u.role = 'admin'`)
	}
	switch f.Pref {
	case "":
	case PrefShareStarted:
		where = append(where, `COALESCE(pp.share_started, 'all') = 'all'`)
	case PrefAdminAlerts:
		where = append(where, `COALESCE(pp.admin_alerts, 1) = 1`)
	default:
		return nil, fmt.Errorf("store: ListPushSubscriptions: unknown preference %q", f.Pref)
	}
	if f.SessionID != "" {
		where = append(where, `p.session_id = ?`)
		args = append(args, string(f.SessionID))
	}
	var out []PushSubscription
	err := q.queryAll("list push subscriptions", `SELECT `+prefixCols("p", pushCols)+` FROM push_subscriptions p
		JOIN users u ON u.id = p.user_id LEFT JOIN push_preferences pp ON pp.user_id = p.user_id
		WHERE `+strings.Join(where, ` AND `)+` ORDER BY p.user_id, p.created_at, p.rowid`, args,
		func(r scanner) error {
			var s pushScan
			if err := r.Scan(s.dest()...); err != nil {
				return err
			}
			out = append(out, s.sub())
			return nil
		})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DeletePushSubscription deletes one of the user's subscriptions and reports whether it existed.
func (q *Q) DeletePushSubscription(u UserID, id PushSubID) (bool, error) {
	n, err := q.execCount("delete push subscription", `DELETE FROM push_subscriptions WHERE id = ? AND user_id = ?`,
		string(id), string(u))
	return n > 0, err
}

// DeletePushSubscriptionByEndpoint deletes the user's subscription with that endpoint and reports whether it existed.
func (q *Q) DeletePushSubscriptionByEndpoint(u UserID, endpoint string) (bool, error) {
	n, err := q.execCount("delete push subscription",
		`DELETE FROM push_subscriptions WHERE endpoint = ? AND user_id = ?`, endpoint, string(u))
	return n > 0, err
}

// DeletePushSubscriptionByID deletes a subscription (04's sender, on 404/410/401/403). A missing row is not an
// error: the subscription is gone either way.
func (q *Q) DeletePushSubscriptionByID(id PushSubID) error {
	_, err := q.execCount("delete push subscription", `DELETE FROM push_subscriptions WHERE id = ?`, string(id))
	return err
}

// DeleteAllPushSubscriptions deletes every subscription (04: the VAPID key changed) and returns how many there were.
func (q *Q) DeleteAllPushSubscriptions() (int, error) {
	return q.execCount("delete all push subscriptions", `DELETE FROM push_subscriptions`)
}

// PrunePushSubscriptions deletes the subscriptions with at least minFailures failures in a row and no success since
// noSuccessSince (never, or before it), and returns how many (04's daily job).
func (q *Q) PrunePushSubscriptions(minFailures int, noSuccessSince time.Time) (int, error) {
	return q.execCount("prune push subscriptions", `DELETE FROM push_subscriptions
		WHERE failures >= ? AND (last_success_at IS NULL OR last_success_at < ?)`,
		minFailures, unixMS(noSuccessSince))
}

// RecordPushResult counts a failed delivery, or, for ok, resets the failure count and sets last_success_at.
// ErrNotFound when the subscription is gone.
func (q *Q) RecordPushResult(id PushSubID, ok bool, now time.Time) error {
	if ok {
		return q.execOne("record push result", `UPDATE push_subscriptions SET failures = 0, last_success_at = ?
			WHERE id = ?`, unixMS(now), string(id))
	}
	return q.execOne("record push result", `UPDATE push_subscriptions SET failures = failures + 1 WHERE id = ?`,
		string(id))
}

// TrimPushSubscriptions keeps the user's keep newest subscriptions by CreatedAt, which every re-subscribe moves
// forward (UpsertPushSubscription), and deletes the older ones.
func (q *Q) TrimPushSubscriptions(u UserID, keep int) error {
	_, err := q.execCount("trim push subscriptions", `DELETE FROM push_subscriptions WHERE id IN (
		SELECT id FROM push_subscriptions WHERE user_id = ? ORDER BY created_at DESC, rowid DESC LIMIT -1 OFFSET ?)`,
		string(u), max(keep, 0))
	return err
}

// PushPreferences returns the user's preferences, or the defaults (all, true) when there is no row.
func (q *Q) PushPreferences(u UserID) (PushPreferences, error) {
	var p PushPreferences
	err := q.queryOne("push preferences", `SELECT share_started, admin_alerts FROM push_preferences WHERE user_id = ?`,
		[]any{string(u)}, &p.ShareStarted, &p.AdminAlerts)
	if errors.Is(err, ErrNotFound) {
		return defaultPushPreferences, nil
	}
	if err != nil {
		return PushPreferences{}, err
	}
	return p, nil
}

// SetPushPreferences stores the user's preferences. ShareStarted must be "all" or "off" (a CHECK).
func (q *Q) SetPushPreferences(u UserID, p PushPreferences, now time.Time) error {
	_, err := q.execCount("set push preferences", `INSERT INTO push_preferences
			(user_id, share_started, admin_alerts, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (user_id) DO UPDATE SET share_started = excluded.share_started,
			admin_alerts = excluded.admin_alerts, updated_at = excluded.updated_at`,
		string(u), p.ShareStarted, boolInt(p.AdminAlerts), unixMS(now))
	return err
}
