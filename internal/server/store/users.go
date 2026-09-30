package store

import (
	"database/sql"
	"time"
)

// User is a row of users. Pending sign-ups are ordinary rows with Status StatusPending (the approval queue).
type User struct {
	ID                UserID
	Username          string // display form (case preserved, PRECIS-enforced)
	UsernameKey       string // PRECIS UsernameCaseMapped compare key, unique
	PasswordHash      string // PHC argon2id; "" = admin reset pending
	Role              Role
	Status            UserStatus
	CreatedVia        string // setup | invite | signup | cli
	InviteID          InviteID
	ApprovedBy        UserID
	ApprovedAt        time.Time // zero = never
	CreatedAt         time.Time
	UpdatedAt         time.Time
	PasswordChangedAt time.Time
	LastLoginAt       time.Time
}

// UserRow is a User plus the derived fields of the admin list.
type UserRow struct {
	User
	InvitedBy    *UserRef  // creator of the invite this user redeemed; nil for none, a CLI invite or a deleted creator
	LastSeenAt   time.Time // max over sessions and devices; zero when there are none
	Sessions     int
	Devices      int
	ResetPending bool   // the password hash is NULL: the user can't log in until a reset link is completed
	SignupIP     string // pending users only: IP of the user.signup_requested audit row
}

// UserRef names a user.
type UserRef struct {
	ID       UserID
	Username string
}

// userCols are the columns scanned by userScan, in its order.
const userCols = `id, username, username_key, password_hash, role, status, created_via, invite_id, approved_by,
	approved_at, created_at, updated_at, password_changed_at, last_login_at`

// userScan holds the scan destinations of userCols.
type userScan struct {
	id, username, key, role, status, via string
	hash, invite, approvedBy             sql.NullString
	approvedAt, pwChanged, lastLogin     sql.NullInt64
	createdAt, updatedAt                 int64
}

func (s *userScan) dest() []any {
	return []any{&s.id, &s.username, &s.key, &s.hash, &s.role, &s.status, &s.via, &s.invite, &s.approvedBy,
		&s.approvedAt, &s.createdAt, &s.updatedAt, &s.pwChanged, &s.lastLogin}
}

func (s *userScan) user() User {
	return User{
		ID:                UserID(s.id),
		Username:          s.username,
		UsernameKey:       s.key,
		PasswordHash:      s.hash.String,
		Role:              Role(s.role),
		Status:            UserStatus(s.status),
		CreatedVia:        s.via,
		InviteID:          InviteID(s.invite.String),
		ApprovedBy:        UserID(s.approvedBy.String),
		ApprovedAt:        fromNullMS(s.approvedAt),
		CreatedAt:         fromMS(s.createdAt),
		UpdatedAt:         fromMS(s.updatedAt),
		PasswordChangedAt: fromNullMS(s.pwChanged),
		LastLoginAt:       fromNullMS(s.lastLogin),
	}
}

// CreateUser inserts u and sets u.ID. An empty Role is RoleUser and an empty Status is StatusActive; a zero
// CreatedAt is the store's clock and a zero UpdatedAt is CreatedAt (u is updated with these). A taken username
// returns *ConflictError{"username_key"}.
func (q *Q) CreateUser(u *User) error {
	if u.Role == "" {
		u.Role = RoleUser
	}
	if u.Status == "" {
		u.Status = StatusActive
	}
	u.CreatedAt = q.orNow(u.CreatedAt)
	if u.UpdatedAt.IsZero() {
		u.UpdatedAt = u.CreatedAt
	}
	u.UpdatedAt = normMS(u.UpdatedAt)
	u.ApprovedAt = normMS(u.ApprovedAt)
	u.PasswordChangedAt = normMS(u.PasswordChangedAt)
	u.LastLoginAt = normMS(u.LastLoginAt)
	id, err := q.withNewID("users", func(id string) error {
		_, err := q.exec(`INSERT INTO users (`+userCols+`) VALUES (`+placeholders(14)+`)`,
			id, u.Username, u.UsernameKey, strOrNull(u.PasswordHash), string(u.Role), string(u.Status), u.CreatedVia,
			strOrNull(string(u.InviteID)), strOrNull(string(u.ApprovedBy)), msOrNull(u.ApprovedAt), unixMS(u.CreatedAt),
			unixMS(u.UpdatedAt), msOrNull(u.PasswordChangedAt), msOrNull(u.LastLoginAt))
		return err
	})
	if err != nil {
		return conflictOr("create user", err)
	}
	u.ID = UserID(id)
	return nil
}

// UserByID returns one user, or ErrNotFound.
func (q *Q) UserByID(id UserID) (User, error) {
	var s userScan
	if err := q.queryOne("user by id", `SELECT `+userCols+` FROM users WHERE id = ?`, []any{string(id)},
		s.dest()...); err != nil {
		return User{}, err
	}
	return s.user(), nil
}

// UserByUsernameKey returns the user with that compare key, or ErrNotFound.
func (q *Q) UserByUsernameKey(key string) (User, error) {
	var s userScan
	if err := q.queryOne("user by username", `SELECT `+userCols+` FROM users WHERE username_key = ?`, []any{key},
		s.dest()...); err != nil {
		return User{}, err
	}
	return s.user(), nil
}

// ListUsers returns the users with status s ("" = all), ordered by username_key, with the derived fields of UserRow.
// Sessions and Devices count every row, including expired sessions the janitor has not pruned yet.
func (q *Q) ListUsers(s UserStatus) ([]UserRow, error) {
	query := `SELECT ` + prefixCols("u", userCols) + `, c.id, c.username,
			(SELECT MAX(last_seen_at) FROM sessions WHERE user_id = u.id),
			(SELECT MAX(last_seen_at) FROM devices WHERE user_id = u.id),
			(SELECT count(*) FROM sessions WHERE user_id = u.id),
			(SELECT count(*) FROM devices WHERE user_id = u.id),
			CASE WHEN u.status = 'pending' THEN (SELECT ip FROM audit_log
				WHERE target_id = u.id AND action = 'user.signup_requested' ORDER BY id DESC LIMIT 1) END
		FROM users u
		LEFT JOIN invites i ON i.id = u.invite_id
		LEFT JOIN users c ON c.id = i.created_by`
	var args []any
	if s != "" {
		query += ` WHERE u.status = ?`
		args = append(args, string(s))
	}
	query += ` ORDER BY u.username_key`
	var out []UserRow
	err := q.queryAll("list users", query, args, func(r scanner) error {
		var us userScan
		var inviterID, inviterName, signupIP sql.NullString
		var sessSeen, devSeen sql.NullInt64
		var row UserRow
		dest := append(us.dest(), &inviterID, &inviterName, &sessSeen, &devSeen, &row.Sessions, &row.Devices, &signupIP)
		if err := r.Scan(dest...); err != nil {
			return err
		}
		row.User = us.user()
		if inviterID.Valid {
			row.InvitedBy = &UserRef{ID: UserID(inviterID.String), Username: inviterName.String}
		}
		seen := sessSeen
		if devSeen.Valid && (!seen.Valid || devSeen.Int64 > seen.Int64) {
			seen = devSeen
		}
		row.LastSeenAt = fromNullMS(seen)
		row.ResetPending = row.PasswordHash == ""
		row.SignupIP = signupIP.String
		out = append(out, row)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RenameUser sets a user's display name and compare key. ErrNotFound for an unknown user; a taken name returns
// *ConflictError{"username_key"}.
func (q *Q) RenameUser(id UserID, display, key string, now time.Time) error {
	return q.execOne("rename user", `UPDATE users SET username = ?, username_key = ?, updated_at = ? WHERE id = ?`,
		display, key, unixMS(now), string(id))
}

// SetRole sets a user's role. ErrNotFound for an unknown user.
func (q *Q) SetRole(id UserID, r Role, now time.Time) error {
	return q.execOne("set role", `UPDATE users SET role = ?, updated_at = ? WHERE id = ?`,
		string(r), unixMS(now), string(id))
}

// SetStatus sets a user's status. ErrNotFound for an unknown user. It changes only the column: revoking sessions,
// devices and reset links is the caller's (auth, 03 §7.7).
func (q *Q) SetStatus(id UserID, s UserStatus, now time.Time) error {
	return q.execOne("set status", `UPDATE users SET status = ?, updated_at = ? WHERE id = ?`,
		string(s), unixMS(now), string(id))
}

// Approve turns a pending user active and records the approver ("" = the CLI); ErrNotFound if the user does not
// exist or is not pending.
func (q *Q) Approve(id UserID, by UserID, now time.Time) error {
	return q.execOne("approve user", `UPDATE users SET status = 'active', approved_by = ?, approved_at = ?, updated_at = ?
		WHERE id = ? AND status = 'pending'`, strOrNull(string(by)), unixMS(now), unixMS(now), string(id))
}

// SetPasswordHash sets a user's PHC hash. "" stores NULL: an admin reset is pending and every login fails until the
// reset link is completed. A non-empty hash also sets password_changed_at (a rehash on login moves it too).
// ErrNotFound for an unknown user.
func (q *Q) SetPasswordHash(id UserID, phc string, now time.Time) error {
	if phc == "" {
		return q.execOne("clear password", `UPDATE users SET password_hash = NULL, updated_at = ? WHERE id = ?`,
			unixMS(now), string(id))
	}
	return q.execOne("set password", `UPDATE users SET password_hash = ?, password_changed_at = ?, updated_at = ?
		WHERE id = ?`, phc, unixMS(now), unixMS(now), string(id))
}

// TouchLogin records a successful login (last_login_at). ErrNotFound for an unknown user.
func (q *Q) TouchLogin(id UserID, now time.Time) error {
	return q.execOne("touch login", `UPDATE users SET last_login_at = ? WHERE id = ?`, unixMS(now), string(id))
}

// DeleteUser deletes a user; sessions, devices, device codes, push subscriptions and preferences, and the reset link
// go with it (cascade), and references from invites, rooms, resets and other users become NULL. ErrNotFound for an
// unknown user.
func (q *Q) DeleteUser(id UserID) error {
	return q.execOne("delete user", `DELETE FROM users WHERE id = ?`, string(id))
}

// CountActiveAdmins counts admins with status active (the last-admin rule, 03 §7.11).
func (q *Q) CountActiveAdmins() (int, error) {
	var n int
	err := q.queryOne("count active admins", `SELECT count(*) FROM users WHERE role = 'admin' AND status = 'active'`,
		nil, &n)
	return n, err
}

// AnyAdmin reports whether any admin exists, whatever its status (setup is available only while none does, 03 §7.8).
func (q *Q) AnyAdmin() (bool, error) {
	var b bool
	err := q.queryOne("any admin", `SELECT EXISTS (SELECT 1 FROM users WHERE role = 'admin')`, nil, &b)
	return b, err
}

// CountPending counts pending sign-ups.
func (q *Q) CountPending() (int, error) {
	var n int
	err := q.queryOne("count pending", `SELECT count(*) FROM users WHERE status = 'pending'`, nil, &n)
	return n, err
}

// DeletePending deletes every user that is pending when it runs (reject all, 03 §7.9) and returns their IDs, sorted.
func (q *Q) DeletePending() ([]UserID, error) {
	return deleteReturningIDs[UserID](q, "delete pending users",
		`DELETE FROM users WHERE status = 'pending' RETURNING id`)
}

// KnownIPs returns the distinct last_ip values of the user's live sessions (not past idle_expires_at or
// expires_at), sorted; none for an unknown name (03 §7.3). The caller compares by auth.IPKey.
func (q *Q) KnownIPs(usernameKey string, now time.Time) ([]string, error) {
	var ips []string
	err := q.queryAll("known ips", `SELECT DISTINCT s.last_ip FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE u.username_key = ? AND s.idle_expires_at >= ? AND s.expires_at >= ? AND s.last_ip <> ''
		ORDER BY s.last_ip`, []any{usernameKey, unixMS(now), unixMS(now)}, func(r scanner) error {
		var ip string
		if err := r.Scan(&ip); err != nil {
			return err
		}
		ips = append(ips, ip)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ips, nil
}
