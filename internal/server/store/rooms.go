package store

import (
	"fmt"
	"time"
)

// Room is a row of rooms (03 §8). Room IDs are stable across renames and never reused.
type Room struct {
	ID        RoomID
	Name      string // PRECIS Nickname, 1–40 characters
	NameKey   string // Nickname compare key (case-insensitive), unique
	IsDefault bool
	CreatedBy UserID // "" = the system (Lounge) or a deleted user
	CreatedAt time.Time
	UpdatedAt time.Time
}

// The default room as EnsureDefaultRoom creates it. defaultRoomNameKey is precis.Nickname.CompareKey("Lounge"),
// written out because the store does not depend on the PRECIS package.
const (
	defaultRoomName    = "Lounge"
	defaultRoomNameKey = "lounge"
)

// EnsureDefaultRoom inserts ('lounge', 'Lounge') as the default room if there is no default room. Open runs it on
// every start; it is idempotent.
func (q *Q) EnsureDefaultRoom(now time.Time) error {
	_, err := q.exec(`INSERT INTO rooms (id, name, name_key, is_default, created_by, created_at, updated_at)
		SELECT ?, ?, ?, 1, NULL, ?, ?
		WHERE NOT EXISTS (SELECT 1 FROM rooms WHERE is_default = 1)`,
		string(DefaultRoomID), defaultRoomName, defaultRoomNameKey, unixMS(now), unixMS(now))
	if err != nil {
		return fmt.Errorf("store: ensure default room: %w", err)
	}
	return nil
}

// ListRooms returns every room, the default room first, then by created_at.
func (q *Q) ListRooms() ([]Room, error) { return nil, notImplemented("ListRooms") }

// RoomByID returns one room, or ErrNotFound.
func (q *Q) RoomByID(id RoomID) (Room, error) { return Room{}, notImplemented("RoomByID") }

// CountRooms returns the number of rooms.
func (q *Q) CountRooms() (int, error) { return 0, notImplemented("CountRooms") }

// CreateRoom inserts r and sets r.ID. A taken name returns *ConflictError{"name_key"}.
func (q *Q) CreateRoom(r *Room) error { return notImplemented("CreateRoom") }

// RenameRoom sets a room's name and name key.
func (q *Q) RenameRoom(id RoomID, name, key string, now time.Time) error {
	return notImplemented("RenameRoom")
}

// DeleteRoom deletes a room; the default room returns ErrDefaultRoom.
func (q *Q) DeleteRoom(id RoomID) error { return notImplemented("DeleteRoom") }
