package store

import (
	"database/sql"
	"time"
)

// Session is a web session (cookie). Its ID is stable across token rotations.
//
// A session is live while now is not past IdleExpiresAt or ExpiresAt (both inclusive); the janitor deletes it once
// IdleExpiresAt < now (03 §4.7). IdleExpiresAt is never later than ExpiresAt: the store clamps it.
type Session struct {
	ID             SessionID
	UserID         UserID
	TokenHash      []byte
	PrevTokenHash  []byte // previous token, valid until PrevValidUntil (inclusive)
	PrevValidUntil time.Time
	Name           string // "Chrome on Windows"; the raw User-Agent is never stored
	CreatedAt      time.Time
	RotatedAt      time.Time
	LastSeenAt     time.Time
	LastIP         string
	IdleExpiresAt  time.Time // min(last use + 30 d, ExpiresAt)
	ExpiresAt      time.Time // CreatedAt + 180 d
}

// sessionCols are the columns scanned by sessionScan, in its order.
const sessionCols = `id, user_id, token_hash, prev_token_hash, prev_valid_until, name, created_at, rotated_at,
	last_seen_at, last_ip, idle_expires_at, expires_at`

type sessionScan struct {
	s                                     Session
	id, user                              string
	prevUntil                             sql.NullInt64
	created, rotated, seen, idle, expires int64
}

func (s *sessionScan) dest() []any {
	return []any{&s.id, &s.user, &s.s.TokenHash, &s.s.PrevTokenHash, &s.prevUntil, &s.s.Name, &s.created, &s.rotated,
		&s.seen, &s.s.LastIP, &s.idle, &s.expires}
}

func (s *sessionScan) session() Session {
	out := s.s
	out.ID = SessionID(s.id)
	out.UserID = UserID(s.user)
	out.PrevValidUntil = fromNullMS(s.prevUntil)
	out.CreatedAt = fromMS(s.created)
	out.RotatedAt = fromMS(s.rotated)
	out.LastSeenAt = fromMS(s.seen)
	out.IdleExpiresAt = fromMS(s.idle)
	out.ExpiresAt = fromMS(s.expires)
	return out
}

// CreateSession inserts s and sets s.ID. TokenHash, IdleExpiresAt and ExpiresAt are required; a zero CreatedAt is
// the store's clock, and zero RotatedAt and LastSeenAt are CreatedAt. IdleExpiresAt is clamped to ExpiresAt. s is
// updated with these values. The caller enforces the per-user cap with TrimSessions.
func (q *Q) CreateSession(s *Session) error {
	const method = "CreateSession"
	if err := requireBytes(method, "TokenHash", s.TokenHash); err != nil {
		return err
	}
	if err := requireTime(method, "ExpiresAt", s.ExpiresAt); err != nil {
		return err
	}
	if err := requireTime(method, "IdleExpiresAt", s.IdleExpiresAt); err != nil {
		return err
	}
	s.CreatedAt = q.orNow(s.CreatedAt)
	if s.RotatedAt.IsZero() {
		s.RotatedAt = s.CreatedAt
	}
	if s.LastSeenAt.IsZero() {
		s.LastSeenAt = s.CreatedAt
	}
	if s.IdleExpiresAt.After(s.ExpiresAt) {
		s.IdleExpiresAt = s.ExpiresAt
	}
	s.RotatedAt, s.LastSeenAt = normMS(s.RotatedAt), normMS(s.LastSeenAt)
	s.IdleExpiresAt, s.ExpiresAt = normMS(s.IdleExpiresAt), normMS(s.ExpiresAt)
	s.PrevValidUntil = normMS(s.PrevValidUntil)
	id, err := q.withNewID("sessions", func(id string) error {
		_, err := q.exec(`INSERT INTO sessions (`+sessionCols+`) VALUES (`+placeholders(12)+`)`,
			id, string(s.UserID), s.TokenHash, bytesOrNull(s.PrevTokenHash), msOrNull(s.PrevValidUntil), s.Name,
			unixMS(s.CreatedAt), unixMS(s.RotatedAt), unixMS(s.LastSeenAt), s.LastIP, unixMS(s.IdleExpiresAt),
			unixMS(s.ExpiresAt))
		return err
	})
	if err != nil {
		return conflictOr("create session", err)
	}
	s.ID = SessionID(id)
	return nil
}

// SessionByTokenHash finds a live session by its token hash, or by its previous hash while now ≤ PrevValidUntil,
// together with its user. A session past IdleExpiresAt or ExpiresAt is ErrNotFound. The user's status is not
// checked here: auth treats a user who is not active as unauthenticated.
func (q *Q) SessionByTokenHash(h []byte, now time.Time) (Session, User, error) {
	if len(h) == 0 {
		return Session{}, User{}, ErrNotFound
	}
	var ss sessionScan
	var us userScan
	ms := unixMS(now)
	err := q.queryOne("session by token", `SELECT `+prefixCols("s", sessionCols)+`, `+prefixCols("u", userCols)+`
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE (s.token_hash = ? OR (s.prev_token_hash = ? AND s.prev_valid_until >= ?))
			AND s.idle_expires_at >= ? AND s.expires_at >= ?`,
		[]any{h, h, ms, ms, ms}, append(ss.dest(), us.dest()...)...)
	if err != nil {
		return Session{}, User{}, err
	}
	return ss.session(), us.user(), nil
}

// RotateSession gives a session a new token hash. The current hash becomes the previous one, valid until
// prevValidUntil (03 §7.4). ErrNotFound for an unknown session.
func (q *Q) RotateSession(id SessionID, newHash []byte, prevValidUntil, now time.Time) error {
	if err := requireBytes("RotateSession", "newHash", newHash); err != nil {
		return err
	}
	return q.execOne("rotate session", `UPDATE sessions SET prev_token_hash = token_hash, prev_valid_until = ?,
		token_hash = ?, rotated_at = ? WHERE id = ?`, unixMS(prevValidUntil), newHash, unixMS(now), string(id))
}

// TouchSession records a use of the session: last_seen_at, last_ip ("" keeps the old one) and idle_expires_at,
// clamped to expires_at. ErrNotFound for an unknown session.
func (q *Q) TouchSession(id SessionID, ip string, now, idleExpires time.Time) error {
	return q.execOne("touch session", `UPDATE sessions SET last_seen_at = ?,
		last_ip = CASE WHEN ? = '' THEN last_ip ELSE ? END, idle_expires_at = MIN(?, expires_at) WHERE id = ?`,
		unixMS(now), ip, ip, unixMS(idleExpires), string(id))
}

// ListSessions returns a user's sessions, the most recently seen first (expired ones the janitor has not pruned
// yet included).
func (q *Q) ListSessions(u UserID) ([]Session, error) {
	var out []Session
	err := q.queryAll("list sessions", `SELECT `+sessionCols+` FROM sessions WHERE user_id = ?
		ORDER BY last_seen_at DESC, created_at DESC, rowid DESC`, []any{string(u)}, func(r scanner) error {
		var s sessionScan
		if err := r.Scan(s.dest()...); err != nil {
			return err
		}
		out = append(out, s.session())
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteSession deletes one of the user's sessions and reports whether it existed. Its push subscriptions go with it.
func (q *Q) DeleteSession(u UserID, id SessionID) (bool, error) {
	n, err := q.execCount("delete session", `DELETE FROM sessions WHERE id = ? AND user_id = ?`, string(id), string(u))
	return n > 0, err
}

// DeleteSessions deletes the user's sessions except one ("" = none) and returns the deleted IDs, sorted.
func (q *Q) DeleteSessions(u UserID, except SessionID) ([]SessionID, error) {
	return deleteReturningIDs[SessionID](q, "delete sessions",
		`DELETE FROM sessions WHERE user_id = ? AND id <> ? RETURNING id`, string(u), string(except))
}

// TrimSessions keeps the user's keep most recently seen sessions (ties: the newest created) and deletes the rest,
// returning their IDs, sorted (03 §7.4 cap; 03 §7.7 closes their connections).
func (q *Q) TrimSessions(u UserID, keep int) ([]SessionID, error) {
	return deleteReturningIDs[SessionID](q, "trim sessions",
		`DELETE FROM sessions WHERE id IN (SELECT id FROM sessions WHERE user_id = ?
			ORDER BY last_seen_at DESC, created_at DESC, rowid DESC LIMIT -1 OFFSET ?) RETURNING id`,
		string(u), max(keep, 0))
}
