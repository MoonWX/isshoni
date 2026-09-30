package store

import (
	"crypto/rand"
	"encoding/binary"
)

// Public identifiers: 12-character lowercase Crockford base32 strings from NewID (03 §3.1). Integer rowids never
// leave the DB, except audit_log.id as a pagination cursor.
type (
	UserID    string
	SessionID string
	DeviceID  string
	InviteID  string
	RoomID    string
	PushSubID string
)

// DefaultRoomID is the fixed ID of the default room, Lounge. It is never deleted and never reused.
const DefaultRoomID RoomID = "lounge"

// idAlphabet is the lowercase Crockford base32 alphabet (no i, l, o, u).
const idAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// idLen is the length of every ID from NewID: 12 characters of 5 bits = 60 random bits.
const idLen = 12

// NewID returns a new random ID: 12 characters from the lowercase Crockford alphabet, 60 bits from crypto/rand.
// An INSERT that hits a UNIQUE collision retries once with a new ID.
func NewID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error; it crashes the program instead
	v := binary.BigEndian.Uint64(b[:]) >> (64 - 5*idLen)
	var out [idLen]byte
	for i := idLen - 1; i >= 0; i-- {
		out[i] = idAlphabet[v&31]
		v >>= 5
	}
	return string(out[:])
}

// Role is a user's role.
type Role string

// Roles.
const (
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
)

// UserStatus is a user's account status. Pending users are the approval queue.
type UserStatus string

// User statuses.
const (
	StatusActive   UserStatus = "active"
	StatusPending  UserStatus = "pending"
	StatusDisabled UserStatus = "disabled"
)

// RegistrationMode is the setting registration.mode (03 §7.9).
type RegistrationMode string

// Registration modes.
const (
	ModeInvite   RegistrationMode = "invite"
	ModeApproval RegistrationMode = "approval"
	ModeClosed   RegistrationMode = "closed"
)

// ActorKind says who did something, as recorded in the audit log.
type ActorKind string

// Actor kinds.
const (
	ActorUser      ActorKind = "user"
	ActorCLI       ActorKind = "cli"
	ActorSystem    ActorKind = "system"
	ActorAnonymous ActorKind = "anonymous"
)

// Actor is who did something, as recorded in the audit log.
type Actor struct {
	Kind   ActorKind
	UserID UserID // "" unless Kind == ActorUser
	Name   string // username snapshot, "cli", "system", or "" for anonymous
	IP     string // "" for cli/system
}

// CLIActor is the actor of admin socket and CLI commands.
var CLIActor = Actor{Kind: ActorCLI, Name: "cli"}

// SystemActor is the actor of the server's own jobs (janitor, key rotation).
var SystemActor = Actor{Kind: ActorSystem, Name: "system"}
