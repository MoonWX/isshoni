package signaltest

import (
	"context"
	"sync"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/signal"
)

// Lounge is the room every new store has (03 §8), and Rooms' default room.
var Lounge = protocol.RoomInfo{ID: "lounge", Name: "Lounge"}

// Rooms is a fake signal.RoomDirectory: an in-memory room list with Lounge as the default room.
type Rooms struct {
	mu         sync.Mutex
	rooms      map[string]protocol.RoomInfo
	defaultID  string
	defaultErr error
	canJoin    func(id signal.Identity, roomID string) error
}

// NewRooms returns a directory that holds Lounge.
func NewRooms() *Rooms {
	return &Rooms{rooms: map[string]protocol.RoomInfo{Lounge.ID: Lounge}, defaultID: Lounge.ID}
}

// Add adds or renames a room.
func (r *Rooms) Add(info protocol.RoomInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rooms[info.ID] = info
}

// Remove deletes a room.
func (r *Rooms) Remove(roomID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.rooms, roomID)
}

// SetDefault changes the default room id.
func (r *Rooms) SetDefault(roomID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defaultID = roomID
}

// FailDefault makes DefaultRoomID return err; nil restores the normal behavior.
func (r *Rooms) FailDefault(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defaultErr = err
}

// SetCanJoin replaces CanJoin's answer (nil: always allowed, as in M1).
func (r *Rooms) SetCanJoin(f func(id signal.Identity, roomID string) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.canJoin = f
}

// GetRoom implements signal.RoomDirectory.
func (r *Rooms) GetRoom(_ context.Context, roomID string) (protocol.RoomInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	info, ok := r.rooms[roomID]
	if !ok {
		return protocol.RoomInfo{}, signal.ErrNotFound
	}
	return info, nil
}

// DefaultRoomID implements signal.RoomDirectory.
func (r *Rooms) DefaultRoomID(context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.defaultErr != nil {
		return "", r.defaultErr
	}
	return r.defaultID, nil
}

// CanJoin implements signal.RoomDirectory.
func (r *Rooms) CanJoin(_ context.Context, id signal.Identity, roomID string) error {
	r.mu.Lock()
	f := r.canJoin
	r.mu.Unlock()
	if f == nil {
		return nil
	}
	return f(id, roomID)
}
