package store

import (
	"database/sql"
	"time"
)

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

// inviteSelect reads an invite with its creator's name; inviteScan takes its columns in this order.
const inviteSelect = `SELECT i.id, i.token_hash, i.note, i.created_by, c.username, i.created_at, i.expires_at,
	i.max_uses, i.uses, i.revoked_at, i.revoked_by FROM invites i LEFT JOIN users c ON c.id = i.created_by`

// inviteActiveSQL is the condition under which UseInvite accepts invite i at the time bound to the placeholder.
const inviteActiveSQL = `i.revoked_at IS NULL AND i.expires_at > ? AND i.uses < i.max_uses`

type inviteScan struct {
	inv                    Invite
	id                     string
	creatorID, creatorName sql.NullString
	revokedBy              sql.NullString
	revokedAt              sql.NullInt64
	created, expires       int64
}

func (s *inviteScan) dest() []any {
	return []any{&s.id, &s.inv.TokenHash, &s.inv.Note, &s.creatorID, &s.creatorName, &s.created, &s.expires,
		&s.inv.MaxUses, &s.inv.Uses, &s.revokedAt, &s.revokedBy}
}

func (s *inviteScan) invite() Invite {
	out := s.inv
	out.ID = InviteID(s.id)
	if s.creatorID.Valid {
		out.CreatedBy = &UserRef{ID: UserID(s.creatorID.String), Username: s.creatorName.String}
	}
	out.CreatedAt = fromMS(s.created)
	out.ExpiresAt = fromMS(s.expires)
	out.RevokedAt = fromNullMS(s.revokedAt)
	out.RevokedBy = UserID(s.revokedBy.String)
	return out
}

// CreateInvite inserts inv and sets inv.ID. TokenHash and ExpiresAt are required, and MaxUses must be 1–1000 (a
// CHECK). A nil CreatedBy (or one with an empty ID) is the CLI. A zero CreatedAt is the store's clock; inv is updated
// with it.
func (q *Q) CreateInvite(inv *Invite) error {
	const method = "CreateInvite"
	if err := requireBytes(method, "TokenHash", inv.TokenHash); err != nil {
		return err
	}
	if err := requireTime(method, "ExpiresAt", inv.ExpiresAt); err != nil {
		return err
	}
	inv.CreatedAt = q.orNow(inv.CreatedAt)
	inv.ExpiresAt, inv.RevokedAt = normMS(inv.ExpiresAt), normMS(inv.RevokedAt)
	var createdBy any
	if inv.CreatedBy != nil {
		createdBy = strOrNull(string(inv.CreatedBy.ID))
	}
	id, err := q.withNewID("invites", func(id string) error {
		_, err := q.exec(`INSERT INTO invites (id, token_hash, note, created_by, created_at, expires_at, max_uses, uses,
			revoked_at, revoked_by) VALUES (`+placeholders(10)+`)`,
			id, inv.TokenHash, inv.Note, createdBy, unixMS(inv.CreatedAt), unixMS(inv.ExpiresAt), inv.MaxUses, inv.Uses,
			msOrNull(inv.RevokedAt), strOrNull(string(inv.RevokedBy)))
		return err
	})
	if err != nil {
		return conflictOr("create invite", err)
	}
	inv.ID = InviteID(id)
	return nil
}

// InviteByTokenHash finds an invite by its token hash, whatever its state, or ErrNotFound. RedeemedBy is not filled.
func (q *Q) InviteByTokenHash(h []byte) (Invite, error) {
	if len(h) == 0 {
		return Invite{}, ErrNotFound
	}
	var s inviteScan
	if err := q.queryOne("invite by token", inviteSelect+` WHERE i.token_hash = ?`, []any{h}, s.dest()...); err != nil {
		return Invite{}, err
	}
	return s.invite(), nil
}

// InviteByID returns one invite, whatever its state, or ErrNotFound. RedeemedBy is not filled.
func (q *Q) InviteByID(id InviteID) (Invite, error) {
	var s inviteScan
	if err := q.queryOne("invite by id", inviteSelect+` WHERE i.id = ?`, []any{string(id)}, s.dest()...); err != nil {
		return Invite{}, err
	}
	return s.invite(), nil
}

// ListInvites returns the invites created by createdBy ("" = all), the newest first, each with RedeemedBy (the
// users who registered with it, in sign-up order; never nil). Without includeInactive it returns only the invites
// that are active at the store's clock. Inactive invites stay listed until the janitor prunes them, 30 days after
// they stop working (03 §4.7).
func (q *Q) ListInvites(createdBy UserID, includeInactive bool) ([]Invite, error) {
	where := `1 = 1`
	var args []any
	if createdBy != "" {
		where += ` AND i.created_by = ?`
		args = append(args, string(createdBy))
	}
	if !includeInactive {
		where += ` AND ` + inviteActiveSQL
		args = append(args, unixMS(q.now()))
	}
	var out []Invite
	index := map[InviteID]int{}
	err := q.queryAll("list invites", inviteSelect+` WHERE `+where+` ORDER BY i.created_at DESC, i.rowid DESC`, args,
		func(r scanner) error {
			var s inviteScan
			if err := r.Scan(s.dest()...); err != nil {
				return err
			}
			inv := s.invite()
			inv.RedeemedBy = []UserRef{}
			index[inv.ID] = len(out)
			out = append(out, inv)
			return nil
		})
	if err != nil || len(out) == 0 {
		return out, err
	}
	err = q.queryAll("list invite redeemers", `SELECT u.invite_id, u.id, u.username FROM users u
		JOIN invites i ON i.id = u.invite_id WHERE `+where+` ORDER BY u.created_at, u.rowid`, args,
		func(r scanner) error {
			var inviteID, id, name string
			if err := r.Scan(&inviteID, &id, &name); err != nil {
				return err
			}
			if k, ok := index[InviteID(inviteID)]; ok {
				out[k].RedeemedBy = append(out[k].RedeemedBy, UserRef{ID: UserID(id), Username: name})
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CountActiveInvites counts the invites created by createdBy ("" = all) that are active at now.
func (q *Q) CountActiveInvites(createdBy UserID, now time.Time) (int, error) {
	query := `SELECT count(*) FROM invites i WHERE ` + inviteActiveSQL
	args := []any{unixMS(now)}
	if createdBy != "" {
		query += ` AND i.created_by = ?`
		args = append(args, string(createdBy))
	}
	var n int
	err := q.queryOne("count active invites", query, args, &n)
	return n, err
}

// RevokeInvite revokes an invite (by "" = the CLI). Revoking a revoked invite is a no-op that keeps the first
// revocation. ErrNotFound for an unknown invite. Accounts created with it stay.
func (q *Q) RevokeInvite(id InviteID, by UserID, now time.Time) error {
	n, err := q.execCount("revoke invite", `UPDATE invites SET revoked_at = ?, revoked_by = ?
		WHERE id = ? AND revoked_at IS NULL`, unixMS(now), strOrNull(string(by)), string(id))
	if err != nil || n > 0 {
		return err
	}
	_, err = q.InviteByID(id)
	return err
}

// UseInvite counts one use: UPDATE … SET uses = uses + 1 WHERE id = ? AND revoked_at IS NULL AND expires_at > now
// AND uses < max_uses. No row (an unknown invite too) → ErrInviteUnusable. Callers run it in the same Write as
// CreateUser, so concurrent registrations never exceed max_uses.
func (q *Q) UseInvite(id InviteID, now time.Time) error {
	n, err := q.execCount("use invite", `UPDATE invites SET uses = uses + 1
		WHERE id = ? AND revoked_at IS NULL AND expires_at > ? AND uses < max_uses`, string(id), unixMS(now))
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrInviteUnusable
	}
	return nil
}
