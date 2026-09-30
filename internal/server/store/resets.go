package store

import "time"

// PasswordReset is an admin-issued one-time reset link; one live link per user (03 §7.10).
type PasswordReset struct {
	TokenHash []byte
	UserID    UserID
	CreatedBy UserID // "" = CLI
	CreatedAt time.Time
	ExpiresAt time.Time
}

// ReplacePasswordReset stores r, replacing the user's previous link.
func (q *Q) ReplacePasswordReset(r PasswordReset) error {
	return notImplemented("ReplacePasswordReset")
}

// PasswordResetByHash finds an unexpired reset link by its token hash, or ErrNotFound.
func (q *Q) PasswordResetByHash(h []byte, now time.Time) (PasswordReset, error) {
	return PasswordReset{}, notImplemented("PasswordResetByHash")
}

// DeletePasswordReset deletes the user's reset link.
func (q *Q) DeletePasswordReset(u UserID) error { return notImplemented("DeletePasswordReset") }
