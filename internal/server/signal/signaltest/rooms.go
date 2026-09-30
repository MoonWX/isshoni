package signaltest

import (
	"context"
	"sync"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/signal"
)

// Lounge is the room every new store has (03 §8), and Rooms' default room.
var Lounge = protocol.RoomInfo{ID: "lounge", Name: "Lounge"}

// Rooms is a fake signal.RoomDirectory: an in-memory room list with Lounge as the default room. FailGetRoom and
// FailDefault inject errors; HoldGetRoom makes GetRoom slow.
type Rooms struct {
	mu         sync.Mutex
	rooms      map[string]protocol.RoomInfo
	defaultID  string
	defaultErr error
	getErr     error
	getGate    chan struct{} // HoldGetRoom: GetRoom waits until it is closed
	gets       int
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

// FailGetRoom makes GetRoom return err; nil restores the normal behavior.
func (r *Rooms) FailGetRoom(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.getErr = err
}

// HoldGetRoom makes the following GetRoom calls block, like a busy store, until release is called or the call's
// context ends (then they return the context's error). The room is looked up after the wait. release may be called
// more than once.
func (r *Rooms) HoldGetRoom() (release func()) {
	gate := make(chan struct{})
	r.mu.Lock()
	r.getGate = gate
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			if r.getGate == gate {
				r.getGate = nil
			}
			r.mu.Unlock()
			close(gate)
		})
	}
}

// GetRoomCalls returns the number of GetRoom calls so far.
func (r *Rooms) GetRoomCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gets
}

// SetCanJoin replaces CanJoin's answer (nil: always allowed, as in M1).
func (r *Rooms) SetCanJoin(f func(id signal.Identity, roomID string) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.canJoin = f
}

// GetRoom implements signal.RoomDirectory.
func (r *Rooms) GetRoom(ctx context.Context, roomID string) (protocol.RoomInfo, error) {
	r.mu.Lock()
	r.gets++
	gate := r.getGate
	r.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return protocol.RoomInfo{}, ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getErr != nil {
		return protocol.RoomInfo{}, r.getErr
	}
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
