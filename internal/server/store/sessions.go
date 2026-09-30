package store

import "time"

// Session is a web session (cookie). Its ID is stable across token rotations.
type Session struct {
	ID             SessionID
	UserID         UserID
	TokenHash      []byte
	PrevTokenHash  []byte // previous token, valid until PrevValidUntil
	PrevValidUntil time.Time
	Name           string // "Chrome on Windows"; the raw User-Agent is never stored
	CreatedAt      time.Time
	RotatedAt      time.Time
	LastSeenAt     time.Time
	LastIP         string
	IdleExpiresAt  time.Time // min(last use + 30 d, ExpiresAt)
	ExpiresAt      time.Time // CreatedAt + 180 d
}

// CreateSession inserts s and sets s.ID.
func (q *Q) CreateSession(s *Session) error { return notImplemented("CreateSession") }

// SessionByTokenHash finds an unexpired session by its token hash, or by its previous hash within the grace period.
func (q *Q) SessionByTokenHash(h []byte, now time.Time) (Session, User, error) {
	return Session{}, User{}, notImplemented("SessionByTokenHash")
}

// RotateSession replaces a session's token hash, keeping the old one valid until prevValidUntil.
func (q *Q) RotateSession(id SessionID, newHash []byte, prevValidUntil, now time.Time) error {
	return notImplemented("RotateSession")
}

// TouchSession records a use of the session.
func (q *Q) TouchSession(id SessionID, ip string, now, idleExpires time.Time) error {
	return notImplemented("TouchSession")
}

// ListSessions returns a user's sessions, the most recently seen first.
func (q *Q) ListSessions(u UserID) ([]Session, error) { return nil, notImplemented("ListSessions") }

// DeleteSession deletes one of the user's sessions and reports whether it existed.
func (q *Q) DeleteSession(u UserID, id SessionID) (bool, error) {
	return false, notImplemented("DeleteSession")
}

// DeleteSessions deletes the user's sessions except one ("" = none) and returns the deleted IDs.
func (q *Q) DeleteSessions(u UserID, except SessionID) ([]SessionID, error) {
	return nil, notImplemented("DeleteSessions")
}

// TrimSessions deletes the least recently seen sessions beyond keep and returns their IDs.
func (q *Q) TrimSessions(u UserID, keep int) ([]SessionID, error) {
	return nil, notImplemented("TrimSessions")
}
