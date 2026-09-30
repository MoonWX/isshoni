package store

import (
	"database/sql"
	"errors"
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

// roomCols are the columns scanned by roomScan, in its order.
const roomCols = `id, name, name_key, is_default, created_by, created_at, updated_at`

type roomScan struct {
	r                Room
	id               string
	createdBy        sql.NullString
	created, updated int64
}

func (s *roomScan) dest() []any {
	return []any{&s.id, &s.r.Name, &s.r.NameKey, &s.r.IsDefault, &s.createdBy, &s.created, &s.updated}
}

func (s *roomScan) room() Room {
	out := s.r
	out.ID = RoomID(s.id)
	out.CreatedBy = UserID(s.createdBy.String)
	out.CreatedAt = fromMS(s.created)
	out.UpdatedAt = fromMS(s.updated)
	return out
}

// ListRooms returns every room, the default room first, then by created_at (ties in creation order).
func (q *Q) ListRooms() ([]Room, error) {
	var out []Room
	err := q.queryAll("list rooms", `SELECT `+roomCols+` FROM rooms ORDER BY is_default DESC, created_at, rowid`, nil,
		func(r scanner) error {
			var s roomScan
			if err := r.Scan(s.dest()...); err != nil {
				return err
			}
			out = append(out, s.room())
			return nil
		})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RoomByID returns one room, or ErrNotFound.
func (q *Q) RoomByID(id RoomID) (Room, error) {
	var s roomScan
	if err := q.queryOne("room by id", `SELECT `+roomCols+` FROM rooms WHERE id = ?`, []any{string(id)},
		s.dest()...); err != nil {
		return Room{}, err
	}
	return s.room(), nil
}

// CountRooms returns the number of rooms.
func (q *Q) CountRooms() (int, error) {
	var n int
	err := q.queryOne("count rooms", `SELECT count(*) FROM rooms`, nil, &n)
	return n, err
}

// CreateRoom inserts r as a new, non-default room and sets r.ID (only EnsureDefaultRoom creates the default room,
// so IsDefault must be false). A zero CreatedAt is the store's clock and a zero UpdatedAt is CreatedAt; r is updated
// with them. A taken name returns *ConflictError{"name_key"}.
func (q *Q) CreateRoom(r *Room) error {
	if r.IsDefault {
		return errors.New("store: CreateRoom: only EnsureDefaultRoom creates the default room")
	}
	r.CreatedAt = q.orNow(r.CreatedAt)
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = r.CreatedAt
	}
	r.UpdatedAt = normMS(r.UpdatedAt)
	id, err := q.withNewID("rooms", func(id string) error {
		_, err := q.exec(`INSERT INTO rooms (`+roomCols+`) VALUES (?, ?, ?, 0, ?, ?, ?)`,
			id, r.Name, r.NameKey, strOrNull(string(r.CreatedBy)), unixMS(r.CreatedAt), unixMS(r.UpdatedAt))
		return err
	})
	if err != nil {
		return conflictOr("create room", err)
	}
	r.ID = RoomID(id)
	return nil
}

// RenameRoom sets a room's name and name key; the default room can be renamed too. ErrNotFound for an unknown room;
// a taken name returns *ConflictError{"name_key"}.
func (q *Q) RenameRoom(id RoomID, name, key string, now time.Time) error {
	return q.execOne("rename room", `UPDATE rooms SET name = ?, name_key = ?, updated_at = ? WHERE id = ?`,
		name, key, unixMS(now), string(id))
}

// DeleteRoom deletes a room. The default room returns ErrDefaultRoom, an unknown room ErrNotFound.
func (q *Q) DeleteRoom(id RoomID) error {
	n, err := q.execCount("delete room", `DELETE FROM rooms WHERE id = ? AND is_default = 0`, string(id))
	if err != nil || n > 0 {
		return err
	}
	if _, err := q.RoomByID(id); err != nil {
		return err
	}
	return ErrDefaultRoom
}
