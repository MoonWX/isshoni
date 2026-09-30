package store

import "time"

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
	InvitedBy    *UserRef  // creator of the invite this user redeemed
	LastSeenAt   time.Time // max over sessions and devices
	Sessions     int
	Devices      int
	ResetPending bool
	SignupIP     string // pending users only: IP of the user.signup_requested audit row
}

// UserRef names a user.
type UserRef struct {
	ID       UserID
	Username string
}

// CreateUser inserts u and sets u.ID. A taken username returns *ConflictError{"username_key"}.
func (q *Q) CreateUser(u *User) error { return notImplemented("CreateUser") }

// UserByID returns one user, or ErrNotFound.
func (q *Q) UserByID(id UserID) (User, error) { return User{}, notImplemented("UserByID") }

// UserByUsernameKey returns the user with that compare key, or ErrNotFound.
func (q *Q) UserByUsernameKey(key string) (User, error) {
	return User{}, notImplemented("UserByUsernameKey")
}

// ListUsers returns the users with status s ("" = all), ordered by username_key.
func (q *Q) ListUsers(s UserStatus) ([]UserRow, error) { return nil, notImplemented("ListUsers") }

// RenameUser sets a user's display name and compare key.
func (q *Q) RenameUser(id UserID, display, key string, now time.Time) error {
	return notImplemented("RenameUser")
}

// SetRole sets a user's role.
func (q *Q) SetRole(id UserID, r Role, now time.Time) error { return notImplemented("SetRole") }

// SetStatus sets a user's status.
func (q *Q) SetStatus(id UserID, s UserStatus, now time.Time) error {
	return notImplemented("SetStatus")
}

// Approve turns a pending user active; ErrNotFound if the user is not pending.
func (q *Q) Approve(id UserID, by UserID, now time.Time) error { return notImplemented("Approve") }

// SetPasswordHash sets a user's PHC hash ("" = NULL, a reset is pending).
func (q *Q) SetPasswordHash(id UserID, phc string, now time.Time) error {
	return notImplemented("SetPasswordHash")
}

// TouchLogin records a successful login.
func (q *Q) TouchLogin(id UserID, now time.Time) error { return notImplemented("TouchLogin") }

// DeleteUser deletes a user; sessions, devices, push subscriptions and resets go with it.
func (q *Q) DeleteUser(id UserID) error { return notImplemented("DeleteUser") }

// CountActiveAdmins counts admins with status active (the last-admin rule).
func (q *Q) CountActiveAdmins() (int, error) { return 0, notImplemented("CountActiveAdmins") }

// AnyAdmin reports whether any admin exists, whatever its status.
func (q *Q) AnyAdmin() (bool, error) { return false, notImplemented("AnyAdmin") }

// CountPending counts pending sign-ups.
func (q *Q) CountPending() (int, error) { return 0, notImplemented("CountPending") }

// DeletePending deletes every pending user (reject all, 03 §7.9) and returns their IDs.
func (q *Q) DeletePending() ([]UserID, error) { return nil, notImplemented("DeletePending") }

// KnownIPs returns the last_ip of each live session of that user (03 §7.3); none for an unknown name.
func (q *Q) KnownIPs(usernameKey string, now time.Time) ([]string, error) {
	return nil, notImplemented("KnownIPs")
}
