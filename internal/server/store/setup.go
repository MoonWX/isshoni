package store

import "time"

// Setup tokens: at most one row; issuing a new token deletes the others (03 §7.8).

// ReplaceSetupToken stores a new setup token hash and deletes all others.
func (q *Q) ReplaceSetupToken(h []byte, now, expires time.Time) error {
	return notImplemented("ReplaceSetupToken")
}

// SetupTokenValid reports whether h is the hash of an unexpired setup token.
func (q *Q) SetupTokenValid(h []byte, now time.Time) (bool, error) {
	return false, notImplemented("SetupTokenValid")
}

// DeleteSetupTokens deletes every setup token.
func (q *Q) DeleteSetupTokens() error { return notImplemented("DeleteSetupTokens") }
