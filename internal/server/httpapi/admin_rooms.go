package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/secure/precis"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/auth"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// Room administration (03 §8, §12.3 #37–#39, §12.4.4), Admin routes: admins create, rename and delete rooms. The
// rules live here, not in auth.Service: rooms have no credentials and no CLI command, only these three handlers.
// Each change is one Write that also writes its audit row (03 §10); the hub hears about it after the commit.

const (
	// maxRooms is the room cap, an abuse guard (03 §8, §14): the 201st room is 409 limit_reached {limit: rooms}.
	maxRooms = 200
	// roomNameMaxRunes is the longest room name, in characters of its normalized form (03 §8).
	roomNameMaxRunes = 40

	// fieldRoomName is the JSON name of the one field of the room requests, the key of their validation_failed answer.
	fieldRoomName = "name"

	// Audit actions of 03 §10 and their target kind.
	auditRoomCreated = "room.created" // detail {name}
	auditRoomRenamed = "room.renamed" // detail {from, to}
	auditRoomDeleted = "room.deleted" // detail {name}
	targetRoom       = "room"
)

// normalizeRoomName applies the room name rules of 03 §8 and returns the name as it is stored and shown, with its
// comparison key, or the field code of the rule that failed:
//
//  1. valid UTF-8 without U+FFFD, else invalid: the profile below works on valid UTF-8 only, encoding/json turns
//     bad bytes of a request body into U+FFFD, and a room name has no use for that rune (the serverName setting
//     has the same rule);
//  2. nothing, or only spaces, is too_short;
//  3. name = PRECIS Nickname (RFC 8266): non-ASCII spaces become U+0020, leading and trailing spaces go, inner runs
//     collapse to one, NFKC. Emoji are allowed ("🎬 Movie night"); control characters, a line break among them, and
//     the other runes the profile refuses are invalid. The name must map to itself again (a fixed point, so a
//     stored name normalizes unchanged), else invalid;
//  4. at most 40 characters, else too_long;
//  5. key = Nickname.CompareKey(name), the case-insensitive form that is unique among the rooms.
//
// Both outputs are stable: normalizeRoomName(name) returns the same name and key.
func normalizeRoomName(in string) (name, key, code string) {
	if !utf8.ValidString(in) || strings.ContainsRune(in, utf8.RuneError) {
		return "", "", api.FieldInvalid
	}
	if strings.TrimFunc(in, func(r rune) bool { return unicode.Is(unicode.Zs, r) }) == "" {
		return "", "", api.FieldTooShort
	}
	name, err := precis.Nickname.String(in)
	if err != nil || name == "" {
		return "", "", api.FieldInvalid
	}
	if again, err := precis.Nickname.String(name); err != nil || again != name {
		return "", "", api.FieldInvalid
	}
	if utf8.RuneCountInString(name) > roomNameMaxRunes {
		return "", "", api.FieldTooLong
	}
	key, err = precis.Nickname.CompareKey(name)
	if err != nil || key == "" {
		return "", "", api.FieldInvalid
	}
	return name, key, ""
}

// roomNameInvalid is 422 validation_failed for the name of a room request.
func roomNameInvalid(code string) error {
	return &api.Error{Code: api.CodeValidationFailed, Fields: map[string]string{fieldRoomName: code}}
}

// stillAdmin is the Admin rule of a route again, at the moment of the change and inside its transaction: the
// caller's account as stored now must be an active admin, else *api.Error{forbidden}. The chain let the request in
// on a principal that may be up to 30 s old (auth's session cache), and the account can have been demoted, disabled
// or deleted since; auth.Service applies the same rule to its admin calls. Reading the row in the Write also keeps
// rooms.created_by from failing its foreign key for an account that was deleted a moment ago.
func stillAdmin(q *store.Q, p auth.Principal) error {
	u, err := q.UserByID(p.UserID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return api.NewError(api.CodeForbidden)
	case err != nil:
		return err
	case u.Status != store.StatusActive || u.Role != store.RoleAdmin:
		return api.NewError(api.CodeForbidden)
	}
	return nil
}

// roomErr is the answer for a failed room change: a taken name is 409 room_name_taken (the store reports the
// UNIQUE name key), and everything else is opErr's.
func roomErr(op string, err error) error {
	var ce *store.ConflictError
	if errors.As(err, &ce) {
		return api.NewError(api.CodeRoomNameTaken)
	}
	return opErr(op, err)
}

// roomsChanged tells every SPA to refetch the room list, after the commit (03 §8, §12.5: a room created, renamed or
// deleted → {All}: rooms).
func (a *API) roomsChanged() {
	a.d.Signal.Notify(NotifyTarget{All: true}, protocol.TopicRooms)
}

// postRoom is POST /api/v1/admin/rooms (03 §12.3 #37, §12.4.4): {name} → 201 {room}. The room gets a new random
// ID, which stays the same across renames and is never used again (03 §8). Errors: 422 validation_failed {name:
// too_short | too_long | invalid} (normalizeRoomName), 409 limit_reached {limit: rooms} at 200 rooms, 409
// room_name_taken for a name that differs from an existing one at most in case.
//
// One Write: the admin rule again, the cap, the row, and the room.created audit row {name}. Then every SPA refetches
// its room list; with the second room, showRoomList turns true.
func (a *API) postRoom(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	var req api.CreateRoomRequest
	if err := DecodeJSON(w, r, &req, otherBodyLimit); err != nil {
		WriteError(w, r, err)
		return
	}
	name, key, code := normalizeRoomName(req.Name)
	if code != "" {
		WriteError(w, r, roomNameInvalid(code))
		return
	}
	now := a.d.Clock()
	room := store.Room{Name: name, NameKey: key, CreatedBy: p.UserID, CreatedAt: now}
	err := a.d.DB.Write(r.Context(), func(q *store.Q) error {
		if err := stillAdmin(q, p); err != nil {
			return err
		}
		n, err := q.CountRooms()
		if err != nil {
			return err
		}
		if n >= maxRooms {
			return &api.Error{Code: api.CodeLimitReached, Params: map[string]any{api.ParamLimit: string(api.LimitKindRooms)}}
		}
		if err := q.CreateRoom(&room); err != nil {
			return err
		}
		return q.AppendAudit(store.AuditEntry{At: now, Action: auditRoomCreated, Actor: a.actorOf(r, p),
			TargetKind: targetRoom, TargetID: string(room.ID), TargetName: room.Name,
			Detail: map[string]any{"name": room.Name}})
	})
	if err != nil {
		WriteError(w, r, roomErr("create room", err))
		return
	}
	a.roomsChanged()
	WriteJSON(w, http.StatusCreated, api.RoomResponse{Room: a.liveRoom(room)})
}

// patchRoom is PATCH /api/v1/admin/rooms/{id} (03 §12.3 #38, §12.4.4): {name} → 200 {room}, the room with its new
// name. The default room can be renamed too; it stays the default, with its ID. Errors: 422 validation_failed {name}
// (normalizeRoomName), 404 room_not_found, 409 room_name_taken.
//
// It is a JSON merge (03 §12.1): a body without a name (or with null for it) changes nothing and answers 200 with
// the room as it is, and so does the name the room already has. A change of case only is a rename. A rename is one
// Write with its room.renamed audit row {from, to}; then every SPA refetches its room list. The hub needs no other
// hook: it reads a room's name when it uses it (03 §8).
func (a *API) patchRoom(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	var req api.PatchRoomRequest
	if err := DecodeJSON(w, r, &req, otherBodyLimit); err != nil {
		WriteError(w, r, err)
		return
	}
	var name, key string
	if req.Name != nil {
		var code string
		if name, key, code = normalizeRoomName(*req.Name); code != "" {
			WriteError(w, r, roomNameInvalid(code))
			return
		}
	}
	id := store.RoomID(r.PathValue("id"))
	now := a.d.Clock()
	var room store.Room
	renamed := false
	err := a.d.DB.Write(r.Context(), func(q *store.Q) error {
		if err := stillAdmin(q, p); err != nil {
			return err
		}
		var err error
		if room, err = q.RoomByID(id); err != nil {
			return roomNotFoundOr(err)
		}
		if req.Name == nil || room.Name == name {
			return nil
		}
		if err := q.RenameRoom(id, name, key, now); err != nil {
			return roomNotFoundOr(err)
		}
		from := room.Name
		room.Name, room.NameKey, room.UpdatedAt = name, key, now
		renamed = true
		return q.AppendAudit(store.AuditEntry{At: now, Action: auditRoomRenamed, Actor: a.actorOf(r, p),
			TargetKind: targetRoom, TargetID: string(room.ID), TargetName: room.Name,
			Detail: map[string]any{"from": from, "to": room.Name}})
	})
	if err != nil {
		WriteError(w, r, roomErr("rename room", err))
		return
	}
	if renamed {
		a.roomsChanged()
	}
	WriteJSON(w, http.StatusOK, api.RoomResponse{Room: a.liveRoom(room)})
}

// deleteRoom is DELETE /api/v1/admin/rooms/{id} (03 §12.3 #39): 204. Errors: 404 room_not_found (also for a room
// that was deleted before), 409 room_is_default: the default room is never deleted, so there is always a room to
// land in (03 §8).
//
// One Write with the room.deleted audit row {name}. After the commit the hub closes the room (Signal.RoomDeleted:
// its shares end and its connections get error{room_closed}, then rejoin the default room themselves, 01 §12.1),
// and every SPA refetches its room list; down at one room, showRoomList is false again.
func (a *API) deleteRoom(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	id := store.RoomID(r.PathValue("id"))
	now := a.d.Clock()
	err := a.d.DB.Write(r.Context(), func(q *store.Q) error {
		if err := stillAdmin(q, p); err != nil {
			return err
		}
		room, err := q.RoomByID(id)
		if err != nil {
			return roomNotFoundOr(err)
		}
		if err := q.DeleteRoom(id); err != nil {
			if errors.Is(err, store.ErrDefaultRoom) {
				return api.NewError(api.CodeRoomIsDefault)
			}
			return roomNotFoundOr(err)
		}
		return q.AppendAudit(store.AuditEntry{At: now, Action: auditRoomDeleted, Actor: a.actorOf(r, p),
			TargetKind: targetRoom, TargetID: string(room.ID), TargetName: room.Name,
			Detail: map[string]any{"name": room.Name}})
	})
	if err != nil {
		WriteError(w, r, roomErr("delete room", err))
		return
	}
	a.d.Signal.RoomDeleted(id)
	a.roomsChanged()
	w.WriteHeader(http.StatusNoContent)
}

// roomNotFoundOr turns the store's ErrNotFound for a room into 404 room_not_found and passes any other error on.
func roomNotFoundOr(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return api.NewError(api.CodeRoomNotFound)
	}
	return err
}
