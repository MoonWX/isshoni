package push

import (
	"crypto/sha256"
	"encoding/base64"
	"time"
)

// Dedup and rate limits (04 §14.4). The state lives in memory: after a restart one notification can repeat, which
// is acceptable. Both types belong to the dispatcher goroutine and have no lock.

const (
	// shareDedupWindow: share.started goes out at most once per (room, sharer) in this time, however many
	// connections the sharer has and however often the share restarts.
	shareDedupWindow = 10 * time.Minute

	// Each recipient (a user, with all of their subscriptions) has a token bucket: recipientBurst notifications
	// at once, then one more every recipientRefill. What exceeds it is dropped and counted.
	recipientBurst  = 3
	recipientRefill = 10 * time.Minute

	// limitSweepSize: a map that grows past this many entries drops the ones that no longer matter.
	limitSweepSize = 1024
)

// dedup remembers when each key last passed.
type dedup struct {
	window time.Duration
	last   map[string]time.Time
}

func newDedup(window time.Duration) *dedup {
	return &dedup{window: window, last: map[string]time.Time{}}
}

// seen reports whether key passed less than a window ago.
func (d *dedup) seen(key string, now time.Time) bool {
	t, ok := d.last[key]
	return ok && now.Sub(t) < d.window
}

// mark records that key passed at now.
func (d *dedup) mark(key string, now time.Time) {
	if len(d.last) >= limitSweepSize {
		d.sweep(now)
	}
	d.last[key] = now
}

// sweep forgets the keys whose window is over.
func (d *dedup) sweep(now time.Time) {
	for k, t := range d.last {
		if now.Sub(t) >= d.window {
			delete(d.last, k)
		}
	}
}

// shareDedupKey is the dedup key of a share.started notification: the room and the sharer.
func shareDedupKey(roomID, userID string) string { return roomID + "\x00" + userID }

// buckets holds one token bucket per recipient. A bucket counts in time, not in tokens, so the arithmetic is exact:
// its credit grows by the time that passes, up to burst × refill, and one notification costs one refill.
type buckets struct {
	refill time.Duration // one token per refill
	full   time.Duration // burst × refill: the credit of a full bucket
	m      map[string]*bucket
}

type bucket struct {
	credit time.Duration
	at     time.Time // when credit was computed
}

func newBuckets(burst int, refill time.Duration) *buckets {
	return &buckets{refill: refill, full: time.Duration(burst) * refill, m: map[string]*bucket{}}
}

// take removes one token from id's bucket and reports whether there was one.
func (b *buckets) take(id string, now time.Time) bool {
	k, ok := b.m[id]
	if !ok {
		if len(b.m) >= limitSweepSize {
			b.sweep(now)
		}
		k = &bucket{credit: b.full, at: now}
		b.m[id] = k
	}
	b.fill(k, now)
	if k.credit < b.refill {
		return false
	}
	k.credit -= b.refill
	return true
}

// fill adds the credit that accrued since the bucket was last looked at.
func (b *buckets) fill(k *bucket, now time.Time) {
	if d := now.Sub(k.at); d > 0 {
		k.credit = min(b.full, k.credit+min(d, b.full))
		k.at = now
	}
}

// sweep forgets the buckets that are full again: a missing bucket is a full one.
func (b *buckets) sweep(now time.Time) {
	for id, k := range b.m {
		b.fill(k, now)
		if k.credit >= b.full {
			delete(b.m, id)
		}
	}
}

// shareTopic is the Topic header of share.started for one (room, sharer): "s" and the first 22 characters of
// base64url(SHA-256(roomID + "/" + userID)), so at most 32 URL-safe characters (RFC 8030 §5.4). The push service
// keeps only the newest pending message of a topic, so a phone that was offline gets one notification per sharer.
func shareTopic(roomID, userID string) string {
	sum := sha256.Sum256([]byte(roomID + "/" + userID))
	return "s" + base64.RawURLEncoding.EncodeToString(sum[:])[:22]
}
