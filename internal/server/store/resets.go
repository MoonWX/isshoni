package store

import (
	"database/sql"
	"time"
)

// PasswordReset is an admin-issued one-time reset link; one live link per user (03 §7.10). A link is valid while now
// is not past ExpiresAt (inclusive); the janitor deletes it once ExpiresAt < now.
type PasswordReset struct {
	TokenHash []byte
	UserID    UserID
	CreatedBy UserID // "" = CLI (or the admin was deleted)
	CreatedAt time.Time
	ExpiresAt time.Time
}

// ReplacePasswordReset stores r, replacing the user's previous link. TokenHash and ExpiresAt are required; a zero
// CreatedAt is the store's clock.
func (q *Q) ReplacePasswordReset(r PasswordReset) error {
	const method = "ReplacePasswordReset"
	if err := requireBytes(method, "TokenHash", r.TokenHash); err != nil {
		return err
	}
	if err := requireTime(method, "ExpiresAt", r.ExpiresAt); err != nil {
		return err
	}
	if err := q.DeletePasswordReset(r.UserID); err != nil {
		return err
	}
	_, err := q.execCount("insert password reset", `INSERT INTO password_resets
		(token_hash, user_id, created_by, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		r.TokenHash, string(r.UserID), strOrNull(string(r.CreatedBy)), unixMS(q.orNow(r.CreatedAt)),
		unixMS(r.ExpiresAt))
	return err
}

// PasswordResetByHash finds an unexpired reset link by its token hash, or ErrNotFound.
func (q *Q) PasswordResetByHash(h []byte, now time.Time) (PasswordReset, error) {
	if len(h) == 0 {
		return PasswordReset{}, ErrNotFound
	}
	var r PasswordReset
	var user string
	var by sql.NullString
	var created, expires int64
	err := q.queryOne("password reset by token", `SELECT token_hash, user_id, created_by, created_at, expires_at
		FROM password_resets WHERE token_hash = ? AND expires_at >= ?`, []any{h, unixMS(now)},
		&r.TokenHash, &user, &by, &created, &expires)
	if err != nil {
		return PasswordReset{}, err
	}
	r.UserID, r.CreatedBy = UserID(user), UserID(by.String)
	r.CreatedAt, r.ExpiresAt = fromMS(created), fromMS(expires)
	return r, nil
}

// DeletePasswordReset deletes the user's reset link, if there is one.
func (q *Q) DeletePasswordReset(u UserID) error {
	_, err := q.execCount("delete password reset", `DELETE FROM password_resets WHERE user_id = ?`, string(u))
	return err
}
