package httpapi

import (
	"fmt"
	"net/http"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// The room list (03 §8, §12.3 #19, §12.4.4), a User route: every signed-in user reads it. Creating, renaming and
// deleting rooms is the admins' (admin_rooms.go).

// apiRoom is a room as REST shows it, with its live counts. The counts come from the signaling hub and are never
// stored (03 §8).
func apiRoom(room store.Room, live RoomPresence) api.Room {
	return api.Room{
		ID:        string(room.ID),
		Name:      room.Name,
		IsDefault: room.IsDefault,
		CreatedAt: room.CreatedAt,
		Live:      api.RoomPresence{Participants: live.Participants, Shares: live.Shares},
	}
}

// liveRoom is apiRoom with the room's counts of this moment. A room nobody is in has no entry in the hub's
// snapshot, which reads as zero participants and zero shares.
func (a *API) liveRoom(room store.Room) api.Room {
	return apiRoom(room, a.d.Signal.RoomPresence()[room.ID])
}

// getRooms is GET /api/v1/rooms (03 §12.3 #19, §12.4.4): {defaultRoomId, showRoomList, rooms}, the default room
// first, then the others in the order they were created, each with its live counts from Signal.RoomPresence.
//
// showRoomList is the plan's rule "more than one room exists" (03 §8): while it is false the SPA hides the room
// list, and deleting down to one room hides it again. defaultRoomId is where a client lands, and where it goes back
// to when its room is deleted or unknown (01 §12.1).
func (a *API) getRooms(w http.ResponseWriter, r *http.Request) {
	var rows []store.Room
	err := a.d.DB.Read(r.Context(), func(q *store.Q) error {
		var err error
		rows, err = q.ListRooms()
		return err
	})
	if err != nil {
		WriteError(w, r, fmt.Errorf("httpapi: rooms: %w", err))
		return
	}
	live := a.d.Signal.RoomPresence()
	out := api.Rooms{
		DefaultRoomID: string(store.DefaultRoomID),
		ShowRoomList:  len(rows) > 1,
		Rooms:         make([]api.Room, 0, len(rows)),
	}
	for _, room := range rows {
		if room.IsDefault {
			out.DefaultRoomID = string(room.ID)
		}
		out.Rooms = append(out.Rooms, apiRoom(room, live[room.ID]))
	}
	WriteJSON(w, http.StatusOK, out)
}
