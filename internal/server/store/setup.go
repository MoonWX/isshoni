package store

import "time"

// Setup tokens: at most one row; issuing a new token deletes the others (03 §7.8). A token is valid while now is not
// past its expires_at (inclusive); the janitor deletes it once expires_at < now.

// ReplaceSetupToken stores a new setup token hash, valid until expires, and deletes all others.
func (q *Q) ReplaceSetupToken(h []byte, now, expires time.Time) error {
	if err := requireBytes("ReplaceSetupToken", "hash", h); err != nil {
		return err
	}
	if err := q.DeleteSetupTokens(); err != nil {
		return err
	}
	_, err := q.execCount("insert setup token",
		`INSERT INTO setup_tokens (token_hash, created_at, expires_at) VALUES (?, ?, ?)`,
		h, unixMS(now), unixMS(expires))
	return err
}

// SetupTokenValid reports whether h is the hash of an unexpired setup token.
func (q *Q) SetupTokenValid(h []byte, now time.Time) (bool, error) {
	if len(h) == 0 {
		return false, nil
	}
	var ok bool
	err := q.queryOne("setup token valid",
		`SELECT EXISTS (SELECT 1 FROM setup_tokens WHERE token_hash = ? AND expires_at >= ?)`,
		[]any{h, unixMS(now)}, &ok)
	return ok, err
}

// DeleteSetupTokens deletes every setup token.
func (q *Q) DeleteSetupTokens() error {
	_, err := q.execCount("delete setup tokens", `DELETE FROM setup_tokens`)
	return err
}
