package store

import "time"

// Invite is a row of invites. Only the token's hash is stored.
type Invite struct {
	ID         InviteID
	TokenHash  []byte
	Note       string
	CreatedBy  *UserRef // nil: created from the CLI, or the creator was deleted
	CreatedAt  time.Time
	ExpiresAt  time.Time
	MaxUses    int
	Uses       int
	RevokedAt  time.Time // zero = not revoked
	RevokedBy  UserID
	RedeemedBy []UserRef // filled by ListInvites
}

// Invite states, as State returns them.
const (
	inviteActive  = "active"
	inviteExpired = "expired"
	inviteUsedUp  = "used_up"
	inviteRevoked = "revoked"
)

// State returns "active", "expired", "used_up" or "revoked" at now. A revoked invite is "revoked" whatever else
// holds, then "expired" wins over "used_up". It is "active" exactly when UseInvite would accept it.
func (i Invite) State(now time.Time) string {
	switch {
	case !i.RevokedAt.IsZero():
		return inviteRevoked
	case !i.ExpiresAt.After(now):
		return inviteExpired
	case i.Uses >= i.MaxUses:
		return inviteUsedUp
	default:
		return inviteActive
	}
}

// CreateInvite inserts inv and sets inv.ID.
func (q *Q) CreateInvite(inv *Invite) error { return notImplemented("CreateInvite") }

// InviteByTokenHash finds an invite by its token hash, or ErrNotFound.
func (q *Q) InviteByTokenHash(h []byte) (Invite, error) {
	return Invite{}, notImplemented("InviteByTokenHash")
}

// InviteByID returns one invite, or ErrNotFound.
func (q *Q) InviteByID(id InviteID) (Invite, error) { return Invite{}, notImplemented("InviteByID") }

// ListInvites returns the invites created by createdBy ("" = all), optionally with the inactive ones.
func (q *Q) ListInvites(createdBy UserID, includeInactive bool) ([]Invite, error) {
	return nil, notImplemented("ListInvites")
}

// CountActiveInvites counts the active invites created by createdBy ("" = all).
func (q *Q) CountActiveInvites(createdBy UserID, now time.Time) (int, error) {
	return 0, notImplemented("CountActiveInvites")
}

// RevokeInvite revokes an invite.
func (q *Q) RevokeInvite(id InviteID, by UserID, now time.Time) error {
	return notImplemented("RevokeInvite")
}

// UseInvite counts one use: UPDATE … SET uses = uses + 1 WHERE id = ? AND revoked_at IS NULL AND expires_at > now
// AND uses < max_uses. No row → ErrInviteUnusable. Callers run it in the same Write as CreateUser.
func (q *Q) UseInvite(id InviteID, now time.Time) error { return notImplemented("UseInvite") }
