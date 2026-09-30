package store

import "time"

// AuditEntry is a row of the audit log (03 §10). Rows are written in the same transaction as the change they
// describe; actor and target names are snapshots.
type AuditEntry struct {
	ID         int64
	At         time.Time
	Action     string
	Outcome    string // "ok" | "denied"
	Actor      Actor
	TargetKind string
	TargetID   string
	TargetName string
	Detail     map[string]any // marshalled ≤ 1 KiB; larger → truncated with "truncated": true
}

// AuditQuery selects a page of the audit log.
type AuditQuery struct {
	Before       int64  // id cursor; 0 = newest
	Limit        int    // 1..200, default 50
	ActionPrefix string // e.g. "user." ; "" = all
	ActorID      UserID
	TargetID     string
}

// AppendAudit writes one audit row. Mutating methods take their audit entry in the same call; this is for
// composite flows.
func (q *Q) AppendAudit(e AuditEntry) error { return notImplemented("AppendAudit") }

// ListAudit returns a page of the audit log, newest first.
func (q *Q) ListAudit(f AuditQuery) ([]AuditEntry, error) { return nil, notImplemented("ListAudit") }

// SecurityEvents returns the newest rows of the actions 03 §10 flags as security events.
func (q *Q) SecurityEvents(since time.Time, limit int) ([]AuditEntry, error) {
	return nil, notImplemented("SecurityEvents")
}
