package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// Tests of the room endpoints (README S53; 03 §8, §12.4.4, §15 "Rooms").

// rooms reads GET /api/v1/rooms.
func (f *authFixture) rooms(c *http.Cookie) api.Rooms {
	f.t.Helper()
	return decodeBody[api.Rooms](f.t, f.call(http.MethodGet, "/api/v1/rooms", "", with(c)), http.StatusOK)
}

// createRoom sends POST /api/v1/admin/rooms with a name.
func (f *authFixture) createRoom(c *http.Cookie, name string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.call(http.MethodPost, "/api/v1/admin/rooms", jsonBody(f.t, api.CreateRoomRequest{Name: name}), with(c))
}

// newRoom creates a room over REST and returns it.
func (f *authFixture) newRoom(c *http.Cookie, name string) api.Room {
	f.t.Helper()
	return decodeBody[api.RoomResponse](f.t, f.createRoom(c, name), http.StatusCreated).Room
}

// renameRoom sends PATCH /api/v1/admin/rooms/{id} with a name.
func (f *authFixture) renameRoom(c *http.Cookie, id, name string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.call(http.MethodPatch, "/api/v1/admin/rooms/"+id, jsonBody(f.t, api.PatchRoomRequest{Name: &name}), with(c))
}

// deleteRoom sends DELETE /api/v1/admin/rooms/{id}.
func (f *authFixture) deleteRoom(c *http.Cookie, id string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.call(http.MethodDelete, "/api/v1/admin/rooms/"+id, "", with(c))
}

// adminAndMember sets the server up with the admin Alex and the member Sam, and drops the hooks of that.
func (f *authFixture) adminAndMember() (admin api.User, adminCookie *http.Cookie, member api.User, memberCookie *http.Cookie) {
	f.t.Helper()
	admin, adminCookie = f.setupAdmin("Alex")
	_, token := f.createInvite(adminCookie, `{}`)
	member, memberCookie = f.join(token, "Sam", clientIP(1))
	f.signal.takeNotes()
	return admin, adminCookie, member, memberCookie
}

// TestRoomsOverREST is 03 §15's rooms case: Lounge exists after the first start; showRoomList is false with 1 room
// and true with 2; a case-insensitive duplicate name → 409; deleting Lounge → 409; deleting another room calls
// Signal.RoomDeleted. It also follows the Signal hooks, the audit rows and the live counts of every step.
func TestRoomsOverREST(t *testing.T) {
	f := newAuthFixture(t)
	admin, cookie, _, member := f.adminAndMember()
	count := f.counter()
	start := f.clk.now()

	// Lounge exists after the first start. With one room the list stays hidden. Its counts are the hub's.
	rec := f.call(http.MethodGet, "/api/v1/rooms", "", with(member))
	if got, want := rec.Body.String(), `{"defaultRoomId":"lounge","showRoomList":false,"rooms":[{"id":"lounge",`+
		`"name":"Lounge","isDefault":true,"live":{"participants":2,"shares":1},"createdAt":"2026-10-01T12:00:00.000Z"}]}`+"\n"; got != want || rec.Code != http.StatusOK {
		t.Fatalf("GET /rooms on a new server = %d %s, want 200 %s", rec.Code, got, want)
	}
	if got, want := fmt.Sprint(jsonKeys(t, rec.Body.Bytes())), "[defaultRoomId rooms showRoomList]"; got != want {
		t.Errorf("body keys = %s, want %s (03 §12.4.4)", got, want)
	}
	if notes := f.signal.takeNotes(); len(notes) != 0 {
		t.Errorf("Signal after a GET = %v, want nothing", notes)
	}

	// Create the second room: 201 {room}, in the shape of 03 §12.4.4. Emoji are fine in a name.
	f.clk.advance(time.Hour)
	rec = f.createRoom(cookie, "🎬 Movie night")
	movie := decodeBody[api.RoomResponse](t, rec, http.StatusCreated).Room
	if len(movie.ID) != 12 || movie.ID == string(store.DefaultRoomID) || movie.Name != "🎬 Movie night" || movie.IsDefault ||
		!movie.CreatedAt.Equal(start.Add(time.Hour)) || movie.Live != (api.RoomPresence{}) {
		t.Errorf("created room = %+v", movie)
	}
	if got, want := rec.Body.String(), `{"room":{"id":"`+movie.ID+`","name":"🎬 Movie night","isDefault":false,`+
		`"live":{"participants":0,"shares":0},"createdAt":"2026-10-01T13:00:00.000Z"}}`+"\n"; got != want {
		t.Errorf("POST /admin/rooms body = %s, want %s", got, want)
	}
	if notes := f.signal.takeNotes(); fmt.Sprint(notes) != "[all [rooms]]" {
		t.Errorf("Signal after a room was created = %v, want [all [rooms]] (03 §8)", notes)
	}
	if n := count("SELECT count(*) FROM rooms WHERE id = ? AND created_by = ? AND name_key = ? AND is_default = 0",
		movie.ID, admin.ID, "🎬 movie night"); n != 1 {
		t.Error("the new room's row: want the creating admin in created_by and the lower-case name key")
	}
	rows := f.auditOf("room.created")
	if len(rows) != 1 || rows[0].Actor.UserID != store.UserID(admin.ID) || rows[0].Actor.Name != "Alex" || rows[0].Actor.IP != addrA ||
		rows[0].TargetKind != "room" || rows[0].TargetID != movie.ID || rows[0].TargetName != "🎬 Movie night" ||
		fmt.Sprint(rows[0].Detail) != "map[name:🎬 Movie night]" || rows[0].Outcome != "ok" {
		t.Errorf("room.created audit rows = %+v", rows)
	}

	// Two rooms: the list shows, the default room first, and each room has its own live counts.
	f.signal.setPresence(map[store.RoomID]RoomPresence{
		store.DefaultRoomID:       {Participants: 3, Shares: 1},
		store.RoomID(movie.ID):    {Participants: 5, Shares: 2},
		store.RoomID("gonegone1"): {Participants: 9, Shares: 9}, // a room the hub still has and the store no more
	})
	list := f.rooms(member)
	if !list.ShowRoomList || list.DefaultRoomID != "lounge" || len(list.Rooms) != 2 ||
		list.Rooms[0].ID != "lounge" || list.Rooms[0].Live != (api.RoomPresence{Participants: 3, Shares: 1}) ||
		list.Rooms[1] != (api.Room{ID: movie.ID, Name: "🎬 Movie night", CreatedAt: movie.CreatedAt, Live: api.RoomPresence{Participants: 5, Shares: 2}}) {
		t.Errorf("GET /rooms with two rooms = %+v", list)
	}
	// A room nobody is in has no entry in the hub's snapshot: zeros.
	f.signal.setPresence(map[store.RoomID]RoomPresence{})
	if list = f.rooms(cookie); list.Rooms[0].Live != (api.RoomPresence{}) || list.Rooms[1].Live != (api.RoomPresence{}) {
		t.Errorf("GET /rooms with an empty hub = %+v, want zero counts", list.Rooms)
	}
	f.signal.setPresence(map[store.RoomID]RoomPresence{store.RoomID(movie.ID): {Participants: 4, Shares: 1}})

	// Names are unique whatever their case, the default room's too.
	for _, name := range []string{"🎬 Movie night", "🎬 MOVIE NIGHT", "  🎬   movie  night ", "lounge", "ＬＯＵＮＧＥ"} {
		e := wantError(t, f.createRoom(cookie, name), http.StatusConflict, api.CodeRoomNameTaken)
		if len(e.Fields) != 0 || len(e.Params) != 0 {
			t.Errorf("create %q: error = %+v, want only the code", name, e)
		}
	}
	// A name that breaks the rules of 03 §8 is 422 with the field code.
	for name, code := range map[string]string{
		"":                      api.FieldTooShort,
		"   ":                   api.FieldTooShort,
		strings.Repeat("x", 41): api.FieldTooLong,
		strings.Repeat("🎬", 41): api.FieldTooLong,
		"Movie\nnight":          api.FieldInvalid,
		"Movie\x00night":        api.FieldInvalid,
	} {
		e := wantError(t, f.createRoom(cookie, name), http.StatusUnprocessableEntity, api.CodeValidationFailed)
		if fmt.Sprint(e.Fields) != "map[name:"+code+"]" {
			t.Errorf("create %q: fields = %v, want name: %s", name, e.Fields, code)
		}
	}
	wantError(t, f.call(http.MethodPost, "/api/v1/admin/rooms", `{}`, with(cookie)), http.StatusUnprocessableEntity, api.CodeValidationFailed)
	wantError(t, f.call(http.MethodPost, "/api/v1/admin/rooms", `{"name":5}`, with(cookie)), http.StatusBadRequest, api.CodeBadRequest)
	wantError(t, f.call(http.MethodPost, "/api/v1/admin/rooms", `{"name":"x"} {}`, with(cookie)), http.StatusBadRequest, api.CodeBadRequest)
	if notes := f.signal.takeNotes(); len(notes) != 0 || len(f.auditOf("room.created")) != 1 || len(f.rooms(cookie).Rooms) != 2 {
		t.Errorf("refused creations left a trace: Signal %v, %d room.created rows", notes, len(f.auditOf("room.created")))
	}

	// The name is stored in its normalized form: spaces trimmed and collapsed, NFKC. 40 characters fit.
	f.clk.advance(time.Minute)
	games := f.newRoom(cookie, " \u3000Ｇａｍｅｓ   night ")
	if games.Name != "Games night" {
		t.Errorf("created name = %q, want %q", games.Name, "Games night")
	}
	f.clk.advance(time.Minute)
	long := f.newRoom(cookie, strings.Repeat("🎬", 40))
	if utf8.RuneCountInString(long.Name) != 40 {
		t.Errorf("a 40-character name was stored as %q", long.Name)
	}
	// The list is the default room first, then the others in the order they were created.
	if got := roomNames(f.rooms(member)); fmt.Sprint(got) != fmt.Sprint([]string{"Lounge", "🎬 Movie night", "Games night", long.Name}) {
		t.Errorf("room order = %q", got)
	}
	f.signal.takeNotes()

	// Rename: 200 {room}; the ID stays, every SPA refetches, and the hub needs no other hook.
	f.clk.advance(time.Minute)
	rec = f.renameRoom(cookie, games.ID, "Board games")
	renamed := decodeBody[api.RoomResponse](t, rec, http.StatusOK).Room
	if renamed != (api.Room{ID: games.ID, Name: "Board games", CreatedAt: games.CreatedAt}) {
		t.Errorf("renamed room = %+v", renamed)
	}
	if got, want := fmt.Sprint(jsonKeys(t, rec.Body.Bytes(), "room")), "[createdAt id isDefault live name]"; got != want {
		t.Errorf("room keys = %s, want %s", got, want)
	}
	if hooks := f.signal.take(); fmt.Sprint(hooks) != "[notify [rooms]]" {
		t.Errorf("Signal hooks after a rename = %v, want only the rooms notification", hooks)
	}
	if notes := f.signal.takeNotes(); fmt.Sprint(notes) != "[all [rooms]]" {
		t.Errorf("Signal after a rename = %v", notes)
	}
	rows = f.auditOf("room.renamed")
	if len(rows) != 1 || rows[0].TargetKind != "room" || rows[0].TargetID != games.ID || rows[0].TargetName != "Board games" ||
		fmt.Sprint(rows[0].Detail) != "map[from:Games night to:Board games]" || rows[0].Actor.UserID != store.UserID(admin.ID) {
		t.Errorf("room.renamed audit rows = %+v", rows)
	}
	if n := count("SELECT count(*) FROM rooms WHERE id = ? AND name = 'Board games' AND name_key = 'board games' AND updated_at > created_at", games.ID); n != 1 {
		t.Error("the renamed room's row: want the new name, its key and a later updated_at")
	}
	// The default room can be renamed; it stays the default, with its ID and its counts.
	lounge := decodeBody[api.RoomResponse](t, f.renameRoom(cookie, "lounge", "Living room"), http.StatusOK).Room
	if lounge.ID != "lounge" || lounge.Name != "Living room" || !lounge.IsDefault {
		t.Errorf("renamed default room = %+v", lounge)
	}
	if list = f.rooms(member); list.DefaultRoomID != "lounge" || list.Rooms[0].Name != "Living room" || !list.Rooms[0].IsDefault {
		t.Errorf("GET /rooms after the default room was renamed = %+v", list)
	}
	// Its old name is free again, and a change of case only is a rename too.
	decodeBody[api.RoomResponse](t, f.renameRoom(cookie, games.ID, "Lounge"), http.StatusOK)
	if got := decodeBody[api.RoomResponse](t, f.renameRoom(cookie, games.ID, "LOUNGE"), http.StatusOK).Room.Name; got != "LOUNGE" {
		t.Errorf("name after a change of case = %q", got)
	}
	if n := len(f.auditOf("room.renamed")); n != 4 {
		t.Errorf("%d room.renamed rows, want 4", n)
	}
	f.signal.takeNotes()

	// A taken name → 409; a bad one → 422; an unknown room → 404 room_not_found. Nothing is written or announced.
	wantError(t, f.renameRoom(cookie, games.ID, "living ROOM"), http.StatusConflict, api.CodeRoomNameTaken)
	wantError(t, f.renameRoom(cookie, "lounge", "🎬 movie night"), http.StatusConflict, api.CodeRoomNameTaken)
	e := wantError(t, f.renameRoom(cookie, games.ID, " "), http.StatusUnprocessableEntity, api.CodeValidationFailed)
	if fmt.Sprint(e.Fields) != "map[name:too_short]" {
		t.Errorf("rename to a blank name: fields = %v", e.Fields)
	}
	wantError(t, f.renameRoom(cookie, "nosuchroom00", "Elsewhere"), http.StatusNotFound, api.CodeRoomNotFound)
	wantError(t, f.renameRoom(cookie, "nosuchroom00", " "), http.StatusUnprocessableEntity, api.CodeValidationFailed)
	// The name the room already has, and a body without a name, change nothing: 200 with the room as it is.
	for _, body := range []string{`{"name":"LOUNGE"}`, `{"name":"  LOUNGE "}`, `{}`, `{"name":null}`, `{"other":1}`} {
		got := decodeBody[api.RoomResponse](t, f.call(http.MethodPatch, "/api/v1/admin/rooms/"+games.ID, body, with(cookie)), http.StatusOK).Room
		if got.ID != games.ID || got.Name != "LOUNGE" {
			t.Errorf("PATCH %s = %+v, want the room unchanged", body, got)
		}
	}
	wantError(t, f.call(http.MethodPatch, "/api/v1/admin/rooms/nosuchroom00", `{}`, with(cookie)), http.StatusNotFound, api.CodeRoomNotFound)
	if notes := f.signal.takeNotes(); len(notes) != 0 || len(f.auditOf("room.renamed")) != 4 {
		t.Errorf("refused and empty renames left a trace: Signal %v, %d room.renamed rows", notes, len(f.auditOf("room.renamed")))
	}

	// The default room is never deleted, whatever it is called.
	wantError(t, f.deleteRoom(cookie, "lounge"), http.StatusConflict, api.CodeRoomIsDefault)
	wantError(t, f.deleteRoom(cookie, "nosuchroom00"), http.StatusNotFound, api.CodeRoomNotFound)
	if hooks := f.signal.take(); len(hooks) != 0 || len(f.auditOf("room.deleted")) != 0 {
		t.Errorf("refused deletions left a trace: Signal %v", hooks)
	}
	// Deleting another room: 204, the hub closes it (RoomDeleted), then every SPA refetches.
	rec = f.deleteRoom(cookie, movie.ID)
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("DELETE = %d %q, want 204 without a body", rec.Code, rec.Body)
	}
	if hooks := f.signal.take(); fmt.Sprint(hooks) != "[room_deleted "+movie.ID+" notify [rooms]]" {
		t.Errorf("Signal hooks after a delete = %v, want RoomDeleted, then the rooms notification", hooks)
	}
	if notes := f.signal.takeNotes(); fmt.Sprint(notes) != "[all [rooms]]" {
		t.Errorf("Signal after a delete = %v", notes)
	}
	rows = f.auditOf("room.deleted")
	if len(rows) != 1 || rows[0].TargetKind != "room" || rows[0].TargetID != movie.ID || rows[0].TargetName != "🎬 Movie night" ||
		fmt.Sprint(rows[0].Detail) != "map[name:🎬 Movie night]" || rows[0].Actor.IP != addrA {
		t.Errorf("room.deleted audit rows = %+v", rows)
	}
	// It is gone for good: deleting or renaming it again is 404, and its name is free.
	wantError(t, f.deleteRoom(cookie, movie.ID), http.StatusNotFound, api.CodeRoomNotFound)
	wantError(t, f.renameRoom(cookie, movie.ID, "Back again"), http.StatusNotFound, api.CodeRoomNotFound)
	if hooks := f.signal.take(); len(hooks) != 0 {
		t.Errorf("Signal hooks after a second delete = %v, want none", hooks)
	}
	again := f.newRoom(cookie, "🎬 Movie night")
	if again.ID == movie.ID {
		t.Error("a new room got the ID of a deleted one")
	}

	// Deleting down to one room hides the list again.
	for _, id := range []string{again.ID, games.ID, long.ID} {
		if rec := f.deleteRoom(cookie, id); rec.Code != http.StatusNoContent {
			t.Fatalf("DELETE %s = %d %s", id, rec.Code, rec.Body)
		}
	}
	if list = f.rooms(member); list.ShowRoomList || len(list.Rooms) != 1 || list.Rooms[0].ID != "lounge" || list.DefaultRoomID != "lounge" {
		t.Errorf("GET /rooms after deleting down to one room = %+v", list)
	}
}

// roomNames returns the names of a room list, in its order.
func roomNames(list api.Rooms) []string {
	out := make([]string, 0, len(list.Rooms))
	for _, r := range list.Rooms {
		out = append(out, r.Name)
	}
	return out
}

// TestRoomLimitOverREST is 03 §15's "the 201st room → 409": 200 rooms, the default room among them, is the cap of
// 03 §8.
func TestRoomLimitOverREST(t *testing.T) {
	f := newAuthFixture(t)
	_, cookie := f.setupAdmin("Alex")
	// 198 rooms straight into the store, next to the Lounge: 199.
	f.write(func(q *store.Q) error {
		for i := range maxRooms - 2 {
			name := fmt.Sprintf("Room %d", i)
			if err := q.CreateRoom(&store.Room{Name: name, NameKey: strings.ToLower(name)}); err != nil {
				return err
			}
		}
		return nil
	})
	f.signal.takeNotes()
	// The 200th fits.
	f.newRoom(cookie, "The last one")
	if list := f.rooms(cookie); len(list.Rooms) != maxRooms || !list.ShowRoomList {
		t.Fatalf("%d rooms, want %d", len(list.Rooms), maxRooms)
	}
	f.signal.takeNotes()
	// The 201st → 409 limit_reached {limit: rooms}, also for a name that is taken: the cap comes first.
	for _, name := range []string{"One too many", "the LAST one"} {
		rec := f.createRoom(cookie, name)
		e := wantError(t, rec, http.StatusConflict, api.CodeLimitReached)
		if fmt.Sprint(e.Params) != "map[limit:rooms]" {
			t.Errorf("create %q at the cap: params = %v, want limit: rooms", name, e.Params)
		}
		if got, want := rec.Body.String(), `{"error":{"code":"limit_reached","params":{"limit":"rooms"}}}`+"\n"; got != want {
			t.Errorf("body = %s, want %s", got, want)
		}
	}
	if notes := f.signal.takeNotes(); len(notes) != 0 || len(f.auditOf("room.created")) != 1 || len(f.rooms(cookie).Rooms) != maxRooms {
		t.Errorf("a creation at the cap left a trace: Signal %v", notes)
	}
	// Renaming still works at the cap, and a deleted room makes space for one more.
	last := f.rooms(cookie).Rooms[maxRooms-1]
	decodeBody[api.RoomResponse](t, f.renameRoom(cookie, last.ID, "Renamed at the cap"), http.StatusOK)
	if rec := f.deleteRoom(cookie, last.ID); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s", rec.Code, rec.Body)
	}
	f.newRoom(cookie, "One too many")
}

// TestRoomRoutes: the room routes sit behind the /api/v1 chain like every other. Everyone signed in reads the list;
// only admins change it (03 §8).
func TestRoomRoutes(t *testing.T) {
	f := newAuthFixture(t)
	_, cookie, _, member := f.adminAndMember()
	room := f.newRoom(cookie, "Games")
	f.signal.takeNotes()

	// Not signed in: 401 everywhere.
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/rooms", ""},
		{http.MethodPost, "/api/v1/admin/rooms", `{"name":"Movies"}`},
		{http.MethodPatch, "/api/v1/admin/rooms/" + room.ID, `{"name":"Movies"}`},
		{http.MethodDelete, "/api/v1/admin/rooms/" + room.ID, ""},
	} {
		wantError(t, f.call(c.method, c.path, c.body), http.StatusUnauthorized, api.CodeUnauthenticated)
		if c.method == http.MethodGet {
			continue
		}
		// A member: 403 forbidden on the admin routes.
		wantError(t, f.call(c.method, c.path, c.body, with(member)), http.StatusForbidden, api.CodeForbidden)
		// A cross-site request with the admin's cookie changes nothing: 403 csrf_failed. Without a JSON
		// Content-Type: 415, also for the DELETE without a body. No bearer tokens in M1.
		rec := f.call(c.method, c.path, c.body, with(cookie), header("Sec-Fetch-Site", "cross-site"), header("Origin", "https://evil.example"))
		wantError(t, rec, http.StatusForbidden, api.CodeCSRFFailed)
		wantError(t, f.call(c.method, c.path, c.body, with(cookie), header("Content-Type", "text/plain")), http.StatusUnsupportedMediaType, api.CodeUnsupportedMediaType)
		wantError(t, f.call(c.method, c.path, c.body, with(cookie), header("Content-Type", "")), http.StatusUnsupportedMediaType, api.CodeUnsupportedMediaType)
		rec = f.call(c.method, c.path, c.body, with(cookie), header("Authorization", "Bearer isa_"+strings.Repeat("A", 43)))
		wantError(t, rec, http.StatusUnauthorized, api.CodeUnauthenticated)
	}
	// The Admin rule of the route comes before anything of the body: a member's bad request is 403 too.
	wantError(t, f.createRoom(member, ""), http.StatusForbidden, api.CodeForbidden)
	wantError(t, f.renameRoom(member, room.ID, ""), http.StatusForbidden, api.CodeForbidden)
	wantError(t, f.call(http.MethodPost, "/api/v1/admin/rooms", `not json`, with(member)), http.StatusForbidden, api.CodeForbidden)
	wantError(t, f.call(http.MethodPatch, "/api/v1/admin/rooms/nosuchroom00", `{}`, with(member)), http.StatusForbidden, api.CodeForbidden)
	if list := f.rooms(member); len(list.Rooms) != 2 || list.Rooms[1].Name != "Games" || len(f.signal.takeNotes()) != 0 {
		t.Errorf("refused requests changed something: rooms %+v", list.Rooms)
	}

	// Other methods: JSON 405 with Allow. Unknown paths under the routes: JSON 404.
	for path, allow := range map[string]string{
		"/api/v1/rooms":                  "GET, HEAD",
		"/api/v1/admin/rooms":            "POST",
		"/api/v1/admin/rooms/" + room.ID: "DELETE, PATCH",
	} {
		rec := f.call(http.MethodPut, path, `{}`, with(cookie))
		wantError(t, rec, http.StatusMethodNotAllowed, api.CodeMethodNotAllowed)
		if got := rec.Header().Get("Allow"); got != allow {
			t.Errorf("PUT %s: Allow %q, want %q", path, got, allow)
		}
	}
	for _, path := range []string{"/api/v1/rooms/" + room.ID, "/api/v1/admin/rooms/" + room.ID + "/more", "/api/v1/admin/rooms/"} {
		wantError(t, f.call(http.MethodGet, path, "", with(cookie)), http.StatusNotFound, api.CodeNotFound)
	}
	// HEAD is GET without the body.
	if rec := f.call(http.MethodHead, "/api/v1/rooms", "", with(member)); rec.Code != http.StatusOK {
		t.Errorf("HEAD /rooms = %d", rec.Code)
	}
	// The body limit is the 64 KiB of every route outside /auth: a larger body is 413, a long name below it 422.
	big := strings.Repeat("x", otherBodyLimit)
	wantError(t, f.call(http.MethodPost, "/api/v1/admin/rooms", `{"name":"`+big+`"}`, with(cookie)), http.StatusRequestEntityTooLarge, api.CodePayloadTooLarge)
	wantError(t, f.createRoom(cookie, big[:authBodyLimit+1]), http.StatusUnprocessableEntity, api.CodeValidationFailed)
}

// TestRoomChangeNeedsAnAdminNow: the admin rule is checked again in the transaction of the change (stillAdmin). The
// fake auth service lets a principal in whose account the store does not have: a session whose account was deleted
// after the chain's (cached) authentication. It gets 403, not a failed foreign key.
func TestRoomChangeNeedsAnAdminNow(t *testing.T) {
	sig := &fakeSignal{}
	f := newAPIFixture(t, func(d *Deps) { d.Signal = sig })
	gone := withCookie("admin", sameOriginJSON...)
	wantError(t, f.do(http.MethodPost, "/api/v1/admin/rooms", strings.NewReader(`{"name":"Games"}`), gone...), http.StatusForbidden, api.CodeForbidden)
	wantError(t, f.do(http.MethodPatch, "/api/v1/admin/rooms/lounge", strings.NewReader(`{"name":"Games"}`), gone...), http.StatusForbidden, api.CodeForbidden)
	wantError(t, f.do(http.MethodDelete, "/api/v1/admin/rooms/lounge", nil, gone...), http.StatusForbidden, api.CodeForbidden)

	// The same account, as the store has it now: a member, a disabled admin, a pending one, and at last an admin.
	ctx := context.Background()
	user := store.User{Username: adminPrincipal.Username, UsernameKey: "alex", Role: store.RoleUser, Status: store.StatusActive, CreatedVia: "setup"}
	if err := f.db.Write(ctx, func(q *store.Q) error { return q.CreateUser(&user) }); err != nil {
		t.Fatal(err)
	}
	p := adminPrincipal
	p.UserID = user.ID
	check := func() error {
		var out error
		if err := f.db.Read(ctx, func(q *store.Q) error { out = stillAdmin(q, p); return nil }); err != nil {
			t.Fatal(err)
		}
		return out
	}
	set := func(fn func(q *store.Q) error) {
		t.Helper()
		if err := f.db.Write(ctx, fn); err != nil {
			t.Fatal(err)
		}
	}
	if err := check(); !api.IsCode(err, api.CodeForbidden) {
		t.Errorf("stillAdmin for a member = %v, want forbidden", err)
	}
	set(func(q *store.Q) error { return q.SetRole(user.ID, store.RoleAdmin, time.Now()) })
	if err := check(); err != nil {
		t.Errorf("stillAdmin for an active admin = %v", err)
	}
	for _, status := range []store.UserStatus{store.StatusDisabled, store.StatusPending} {
		set(func(q *store.Q) error { return q.SetStatus(user.ID, status, time.Now()) })
		if err := check(); !api.IsCode(err, api.CodeForbidden) {
			t.Errorf("stillAdmin for a %s admin = %v, want forbidden", status, err)
		}
	}
	if notes := sig.takeNotes(); len(notes) != 0 {
		t.Errorf("Signal = %v, want nothing", notes)
	}
	var rooms []store.Room
	if err := f.db.Read(ctx, func(q *store.Q) (err error) { rooms, err = q.ListRooms(); return err }); err != nil {
		t.Fatal(err)
	}
	if len(rooms) != 1 || rooms[0].Name != "Lounge" {
		t.Errorf("rooms after refused changes = %+v", rooms)
	}
}

// TestRoomsWithoutSignal: with no hub (Deps.Signal nil) the room endpoints work, with zero live counts and no-op
// hooks (03 §12.5).
func TestRoomsWithoutSignal(t *testing.T) {
	f := newAuthFixtureDeps(t, func(d *Deps) { d.Signal = nil })
	_, cookie := f.setupAdmin("Alex")
	room := f.newRoom(cookie, "Games")
	list := f.rooms(cookie)
	if len(list.Rooms) != 2 || list.Rooms[0].Live != (api.RoomPresence{}) || list.Rooms[1].Live != (api.RoomPresence{}) || !list.ShowRoomList {
		t.Errorf("GET /rooms without a hub = %+v", list)
	}
	if rec := f.deleteRoom(cookie, room.ID); rec.Code != http.StatusNoContent {
		t.Errorf("DELETE without a hub = %d %s", rec.Code, rec.Body)
	}
}

// TestRoomStoreErrors: a store that fails answers 500 internal, with nothing of the cause on the wire and no hook.
func TestRoomStoreErrors(t *testing.T) {
	f := newAuthFixture(t)
	_, cookie := f.setupAdmin("Alex")
	room := f.newRoom(cookie, "Games")
	f.signal.takeNotes()
	// The session is in auth's cache, so the chain still lets the request in after the store is closed.
	f.rooms(cookie)
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/rooms", ""},
		{http.MethodPost, "/api/v1/admin/rooms", `{"name":"Movies"}`},
		{http.MethodPatch, "/api/v1/admin/rooms/" + room.ID, `{"name":"Movies"}`},
		{http.MethodDelete, "/api/v1/admin/rooms/" + room.ID, ""},
	} {
		rec := f.call(c.method, c.path, c.body, with(cookie))
		e := wantError(t, rec, http.StatusInternalServerError, api.CodeInternal)
		if e.RequestID == "" || strings.Contains(rec.Body.String(), "store") {
			t.Errorf("%s %s: body %s, want only the code and a request ID", c.method, c.path, rec.Body)
		}
	}
	if hooks := f.signal.take(); len(hooks) != 0 {
		t.Errorf("Signal hooks after failed changes = %v, want none", hooks)
	}
}

// TestNormalizeRoomName is the name table of 03 §8.
func TestNormalizeRoomName(t *testing.T) {
	for _, c := range []struct{ in, name, key, code string }{
		{"Lounge", "Lounge", "lounge", ""},
		{"🎬 Movie night", "🎬 Movie night", "🎬 movie night", ""},
		{"  Games   night  ", "Games night", "games night", ""},
		{"\u3000Games\u00a0\u2003night", "Games night", "games night", ""}, // non-ASCII spaces become U+0020
		{"Ｇａｍｅｓ", "Games", "games", ""},                                    // NFKC: fullwidth
		{"ﬁlm Ⅷ", "film VIII", "film viii", ""},                            // NFKC: ligature, Roman numeral
		{"太郎の部屋", "太郎の部屋", "太郎の部屋", ""},
		{"Ёжик", "Ёжик", "ёжик", ""},
		{"Straße", "Straße", "straße", ""},
		{"a", "a", "a", ""},
		{"🎬", "🎬", "🎬", ""},
		{"it's (not) a #room!", "it's (not) a #room!", "it's (not) a #room!", ""},
		{strings.Repeat("x", 40), strings.Repeat("x", 40), strings.Repeat("x", 40), ""},
		{strings.Repeat("語", 40), strings.Repeat("語", 40), strings.Repeat("語", 40), ""},
		{" " + strings.Repeat("x", 40) + "  ", strings.Repeat("x", 40), strings.Repeat("x", 40), ""}, // counted after trimming
		{strings.Repeat("x", 41), "", "", api.FieldTooLong},
		{strings.Repeat("🎬", 41), "", "", api.FieldTooLong},
		{strings.Repeat("ﬁ", 21), "", "", api.FieldTooLong}, // 21 ligatures are 42 letters after NFKC
		{strings.Repeat("ﬁ", 20), strings.Repeat("fi", 20), strings.Repeat("fi", 20), ""},
		{"", "", "", api.FieldTooShort},
		{" ", "", "", api.FieldTooShort},
		{"\u3000\u00a0 ", "", "", api.FieldTooShort},
		{"a\nb", "", "", api.FieldInvalid},
		{"a\tb", "", "", api.FieldInvalid},
		{"\t", "", "", api.FieldInvalid},
		{"a\x00b", "", "", api.FieldInvalid},
		{"a\u200bb", "", "", api.FieldInvalid},   // zero-width space
		{"\u202eevil", "", "", api.FieldInvalid}, // right-to-left override
		{"a\u2028b", "", "", api.FieldInvalid},   // line separator
		{"a\ufffdb", "", "", api.FieldInvalid},   // what a JSON decoder leaves of bad bytes
		{"a\xffb", "", "", api.FieldInvalid},     // not UTF-8
		{string([]byte{0xed, 0xa0, 0x80}), "", "", api.FieldInvalid},
	} {
		name, key, code := normalizeRoomName(c.in)
		if name != c.name || key != c.key || code != c.code {
			t.Errorf("normalizeRoomName(%q) = %q, %q, %q; want %q, %q, %q", c.in, name, key, code, c.name, c.key, c.code)
			continue
		}
		if code != "" {
			continue
		}
		// A stored name normalizes to itself, and so does its key's name.
		if n2, k2, c2 := normalizeRoomName(name); n2 != name || k2 != key || c2 != "" {
			t.Errorf("normalizeRoomName(%q) again = %q, %q, %q: not stable", name, n2, k2, c2)
		}
	}
	// The default room's key in the store is the key of its name.
	if _, key, code := normalizeRoomName("Lounge"); key != "lounge" || code != "" {
		t.Errorf("key of Lounge = %q (%s)", key, code)
	}
}

// FuzzNormalizeRoomName: whatever the input, the result is a code of 03 §8 or a name of 1–40 characters that is
// valid UTF-8 and normalizes to itself with the same key.
func FuzzNormalizeRoomName(f *testing.F) {
	for _, s := range []string{"Lounge", "🎬 Movie night", "  Games   night  ", "Ｇａｍｅｓ", "", " ", "a\nb", "a\xffb", "a\ufffdb",
		strings.Repeat("x", 41), strings.Repeat("ﬁ", 21), "\u0301abc", "İstanbul", "ǅ", "a\u00adb", "\u200d", "ﷺ", "e\u0301"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		name, key, code := normalizeRoomName(in)
		switch code {
		case api.FieldTooShort, api.FieldTooLong, api.FieldInvalid:
			if name != "" || key != "" {
				t.Fatalf("normalizeRoomName(%q) = %q, %q with the code %s", in, name, key, code)
			}
			return
		case "":
		default:
			t.Fatalf("normalizeRoomName(%q): unknown code %q", in, code)
		}
		if n := utf8.RuneCountInString(name); n < 1 || n > roomNameMaxRunes || !utf8.ValidString(name) || key == "" || !utf8.ValidString(key) {
			t.Fatalf("normalizeRoomName(%q) = %q, %q: want 1–%d characters of valid UTF-8 and a key", in, name, key, roomNameMaxRunes)
		}
		if strings.TrimSpace(name) != name || strings.Contains(name, "  ") || strings.ContainsRune(name, utf8.RuneError) {
			t.Fatalf("normalizeRoomName(%q) = %q: spaces at an edge, two in a row, or U+FFFD", in, name)
		}
		if n2, k2, c2 := normalizeRoomName(name); n2 != name || k2 != key || c2 != "" {
			t.Fatalf("normalizeRoomName(%q) = %q, %q, but again = %q, %q, %q: not stable", in, name, key, n2, k2, c2)
		}
	})
}
