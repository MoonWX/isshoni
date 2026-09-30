package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// AuditEntry is a row of the audit log (03 §10). Rows are written in the same transaction as the change they
// describe; actor and target names are snapshots. Actor.IP is the row's ip column.
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

// Audit log limits.
const (
	maxAuditDetail     = 1024 // bytes of marshalled Detail
	defaultAuditLimit  = 50
	maxAuditLimit      = 200
	defaultSecurityMax = 20 // the dashboard's security events list (03 §12.4.8)
)

// securityActions are the actions 03 §10 flags as security events (column Sec). settings.changed counts only when
// it changed registrationMode (securityEventSQL).
var securityActions = []string{
	"setup.completed",
	"auth.throttled",
	"user.role_changed",
	"user.disabled",
	"user.deleted",
	"user.password_reset_issued",
	"secrets.rotated",
	"device.refresh_reused",
}

// securityEventSQL selects the security events among the audit rows.
var securityEventSQL = `(action IN ('` + strings.Join(securityActions, `', '`) + `') OR (action = 'settings.changed'
	AND CASE WHEN json_valid(detail) THEN json_extract(detail, '$.changes.registrationMode') END IS NOT NULL))`

// auditCols are the columns scanned by auditScan, in its order.
const auditCols = `id, at, action, outcome, actor_kind, actor_id, actor_name, target_kind, target_id, target_name, ip,
	detail`

type auditScan struct {
	e         AuditEntry
	at        int64
	actorKind string
	actorID   sql.NullString
	detail    string
}

func (s *auditScan) dest() []any {
	return []any{&s.e.ID, &s.at, &s.e.Action, &s.e.Outcome, &s.actorKind, &s.actorID, &s.e.Actor.Name,
		&s.e.TargetKind, &s.e.TargetID, &s.e.TargetName, &s.e.Actor.IP, &s.detail}
}

func (s *auditScan) entry() (AuditEntry, error) {
	out := s.e
	out.At = fromMS(s.at)
	out.Actor.Kind = ActorKind(s.actorKind)
	out.Actor.UserID = UserID(s.actorID.String)
	detail, err := decodeDetail(s.detail)
	if err != nil {
		return AuditEntry{}, fmt.Errorf("audit row %d: %w", out.ID, err)
	}
	out.Detail = detail
	return out, nil
}

// encodeDetail marshals d for the detail column: "{}" for none. When the JSON is longer than 1 KiB, it keeps the keys
// that fit, in key order, and adds "truncated": true.
func encodeDetail(d map[string]any) (string, error) {
	if len(d) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(d)
	if err != nil {
		return "", fmt.Errorf("store: audit detail: %w", err)
	}
	if len(b) <= maxAuditDetail {
		return string(b), nil
	}
	keys := make([]string, 0, len(d))
	for k := range d {
		if k != "truncated" {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	out := map[string]any{"truncated": true}
	for _, k := range keys {
		out[k] = d[k]
		if b, err := json.Marshal(out); err != nil || len(b) > maxAuditDetail {
			delete(out, k)
		}
	}
	b, err = json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("store: audit detail: %w", err)
	}
	return string(b), nil
}

// decodeDetail parses a detail column. Numbers stay json.Number, so integers keep every digit.
func decodeDetail(s string) (map[string]any, error) {
	d := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.UseNumber()
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("detail: %w", err)
	}
	if d == nil { // the JSON null
		d = map[string]any{}
	}
	return d, nil
}

// AppendAudit writes one audit row. A zero At is the store's clock and an empty Outcome is "ok"; Detail is limited
// to 1 KiB (encodeDetail). Mutating flows write their audit row in the same Write as the change.
func (q *Q) AppendAudit(e AuditEntry) error {
	if e.Action == "" {
		return errors.New("store: AppendAudit: Action is required")
	}
	if e.Outcome == "" {
		e.Outcome = "ok"
	}
	detail, err := encodeDetail(e.Detail)
	if err != nil {
		return err
	}
	_, err = q.execCount("append audit", `INSERT INTO audit_log (at, action, outcome, actor_kind, actor_id, actor_name,
			target_kind, target_id, target_name, ip, detail) VALUES (`+placeholders(11)+`)`,
		unixMS(q.orNow(e.At)), e.Action, e.Outcome, string(e.Actor.Kind), strOrNull(string(e.Actor.UserID)),
		e.Actor.Name, e.TargetKind, e.TargetID, e.TargetName, e.Actor.IP, detail)
	return err
}

// ListAudit returns a page of the audit log, newest first: rows with id < Before (0 = from the newest), whose action
// starts with ActionPrefix and whose actor and target match when set. Limit is clamped to 1..200 (0 = 50).
func (q *Q) ListAudit(f AuditQuery) ([]AuditEntry, error) {
	where := []string{`1 = 1`}
	var args []any
	if f.Before > 0 {
		where = append(where, `id < ?`)
		args = append(args, f.Before)
	}
	if f.ActionPrefix != "" {
		where = append(where, `substr(action, 1, length(?)) = ?`)
		args = append(args, f.ActionPrefix, f.ActionPrefix)
	}
	if f.ActorID != "" {
		where = append(where, `actor_id = ?`)
		args = append(args, string(f.ActorID))
	}
	if f.TargetID != "" {
		where = append(where, `target_id = ?`)
		args = append(args, f.TargetID)
	}
	limit := f.Limit
	switch {
	case limit <= 0:
		limit = defaultAuditLimit
	case limit > maxAuditLimit:
		limit = maxAuditLimit
	}
	args = append(args, limit)
	return q.auditRows("list audit", `SELECT `+auditCols+` FROM audit_log WHERE `+strings.Join(where, ` AND `)+
		` ORDER BY id DESC LIMIT ?`, args)
}

// SecurityEvents returns the newest rows at or after since of the actions 03 §10 flags as security events
// (settings.changed only when it changed registrationMode), newest first. limit ≤ 0 is 20, and at most 200.
func (q *Q) SecurityEvents(since time.Time, limit int) ([]AuditEntry, error) {
	switch {
	case limit <= 0:
		limit = defaultSecurityMax
	case limit > maxAuditLimit:
		limit = maxAuditLimit
	}
	return q.auditRows("security events", `SELECT `+auditCols+` FROM audit_log WHERE at >= ? AND `+securityEventSQL+
		` ORDER BY id DESC LIMIT ?`, []any{unixMS(since), limit})
}

func (q *Q) auditRows(what, query string, args []any) ([]AuditEntry, error) {
	var out []AuditEntry
	err := q.queryAll(what, query, args, func(r scanner) error {
		var s auditScan
		if err := r.Scan(s.dest()...); err != nil {
			return err
		}
		e, err := s.entry()
		if err != nil {
			return err
		}
		out = append(out, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
