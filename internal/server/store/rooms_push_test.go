package store

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"
)

func TestRooms(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	t0 := e.clock.Now()
	alex := e.newUser(db, "Alex", func(u *User) { u.Role = RoleAdmin })
	var games, movies Room
	mustWrite(t, db, func(q *Q) error {
		movies = Room{Name: "🎬 Movie night", NameKey: "🎬 movie night", CreatedBy: alex.ID, CreatedAt: t0.Add(time.Minute)}
		if err := q.CreateRoom(&movies); err != nil {
			return err
		}
		games = Room{Name: "Games", NameKey: "games"} // created by the system clock, no creator
		if err := q.CreateRoom(&games); err != nil {
			return err
		}
		var ce *ConflictError
		if err := q.CreateRoom(&Room{Name: "GAMES", NameKey: "games"}); !errors.As(err, &ce) || ce.Column != "name_key" {
			t.Errorf("CreateRoom with a taken name = %v", err)
		}
		if err := q.CreateRoom(&Room{Name: "X", NameKey: "x", IsDefault: true}); err == nil {
			t.Error("CreateRoom of a second default room succeeded")
		}
		return nil
	})
	if len(games.ID) != idLen || !games.CreatedAt.Equal(t0) || games.UpdatedAt != games.CreatedAt || games.IsDefault {
		t.Errorf("created room: %+v", games)
	}
	mustRead(t, db, func(q *Q) error {
		rooms, err := q.ListRooms()
		if err != nil {
			return err
		}
		var ids []RoomID
		for _, r := range rooms {
			ids = append(ids, r.ID)
		}
		// Default first, then by created_at: Games (t0) before the movie room (t0+1m).
		if !slices.Equal(ids, []RoomID{DefaultRoomID, games.ID, movies.ID}) {
			t.Errorf("ListRooms order = %v", ids)
		}
		if !rooms[0].IsDefault || rooms[0].Name != "Lounge" || rooms[0].CreatedBy != "" {
			t.Errorf("Lounge = %+v", rooms[0])
		}
		got, err := q.RoomByID(movies.ID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(got, movies) {
			t.Errorf("RoomByID:\n got %+v\nwant %+v", got, movies)
		}
		if _, err := q.RoomByID("nope"); !errors.Is(err, ErrNotFound) {
			t.Errorf("RoomByID(unknown) = %v", err)
		}
		if n, err := q.CountRooms(); err != nil || n != 3 {
			t.Errorf("CountRooms = %d, %v", n, err)
		}
		return nil
	})
	t1 := t0.Add(time.Hour)
	mustWrite(t, db, func(q *Q) error {
		// The default room can be renamed but not deleted.
		if err := q.RenameRoom(DefaultRoomID, "Couch", "couch", t1); err != nil {
			return err
		}
		var ce *ConflictError
		if err := q.RenameRoom(movies.ID, "games", "games", t1); !errors.As(err, &ce) || ce.Column != "name_key" {
			t.Errorf("RenameRoom to a taken name = %v", err)
		}
		if err := q.RenameRoom("nope", "a", "a", t1); !errors.Is(err, ErrNotFound) {
			t.Errorf("RenameRoom(unknown) = %v", err)
		}
		if err := q.DeleteRoom(DefaultRoomID); !errors.Is(err, ErrDefaultRoom) {
			t.Errorf("DeleteRoom(lounge) = %v", err)
		}
		if err := q.DeleteRoom("nope"); !errors.Is(err, ErrNotFound) {
			t.Errorf("DeleteRoom(unknown) = %v", err)
		}
		if err := q.DeleteRoom(games.ID); err != nil {
			return err
		}
		lounge, err := q.RoomByID(DefaultRoomID)
		if err != nil {
			return err
		}
		if lounge.Name != "Couch" || lounge.NameKey != "couch" || !lounge.UpdatedAt.Equal(t1) || !lounge.IsDefault {
			t.Errorf("renamed Lounge = %+v", lounge)
		}
		if n, err := q.CountRooms(); err != nil || n != 2 {
			t.Errorf("CountRooms after a delete = %d, %v", n, err)
		}
		// A deleted room's name is free again, under a new ID.
		again := Room{Name: "Games", NameKey: "games"}
		if err := q.CreateRoom(&again); err != nil {
			return err
		}
		if again.ID == games.ID {
			t.Error("a room ID was reused")
		}
		return nil
	})
}

// pushFixture: an active admin (Alex), an active user (Sam) and a disabled user (Kim), each with a session and a
// subscription.
type pushFixture struct {
	e                 *testEnv
	db                *DB
	alex, sam, kim    User
	sAlex, sSam, sKim Session
	pAlex, pSam, pKim PushSubscription
}

func newPushFixture(t *testing.T) *pushFixture {
	t.Helper()
	e := newEnv(t)
	f := &pushFixture{e: e, db: e.open(nil)}
	t0 := e.clock.Now()
	f.alex = e.newUser(f.db, "Alex", func(u *User) { u.Role = RoleAdmin })
	f.sam = e.newUser(f.db, "Sam")
	f.kim = e.newUser(f.db, "Kim", func(u *User) { u.Status = StatusDisabled })
	f.sAlex = e.newSession(f.db, f.alex.ID, "a", t0)
	f.sSam = e.newSession(f.db, f.sam.ID, "s", t0)
	f.sKim = e.newSession(f.db, f.kim.ID, "k", t0)
	sub := func(u User, s Session, n string) PushSubscription {
		p := PushSubscription{UserID: u.ID, SessionID: s.ID, Endpoint: "https://push.example/" + n,
			P256dh: "key-" + n, Auth: "auth-" + n, Name: "Chrome on Windows"}
		mustWrite(t, f.db, func(q *Q) error {
			created, err := q.UpsertPushSubscription(&p)
			if err == nil && !created {
				t.Errorf("subscription %s was not created", n)
			}
			return err
		})
		return p
	}
	f.pAlex, f.pSam, f.pKim = sub(f.alex, f.sAlex, "alex"), sub(f.sam, f.sSam, "sam"), sub(f.kim, f.sKim, "kim")
	return f
}

func (f *pushFixture) list(t *testing.T, filter PushFilter) []PushSubID {
	t.Helper()
	var out []PushSubID
	mustRead(t, f.db, func(q *Q) error {
		subs, err := q.ListPushSubscriptions(filter)
		for _, s := range subs {
			out = append(out, s.ID)
		}
		return err
	})
	return out
}

func TestUpsertPushSubscription(t *testing.T) {
	t.Parallel()
	f := newPushFixture(t)
	db, t0 := f.db, f.e.clock.Now()
	mustRead(t, db, func(q *Q) error {
		subs, err := q.ListPushSubscriptions(PushFilter{UserIDs: []UserID{f.alex.ID}})
		if err != nil {
			return err
		}
		want := f.pAlex
		if len(subs) != 1 || !reflect.DeepEqual(subs[0], want) || !want.CreatedAt.Equal(t0) {
			t.Errorf("stored subscription:\n got %+v\nwant %+v", subs, want)
		}
		return nil
	})
	mustWrite(t, db, func(q *Q) error { return q.RecordPushResult(f.pAlex.ID, true, t0.Add(time.Minute)) })
	mustWrite(t, db, func(q *Q) error { return q.RecordPushResult(f.pAlex.ID, false, t0.Add(2*time.Minute)) })

	// The same endpoint from a new session of the same user: rebound, keys replaced, failures reset, created_at and
	// last_success_at kept, same ID.
	s2 := f.e.newSession(db, f.alex.ID, "a2", t0.Add(time.Hour))
	again := PushSubscription{UserID: f.alex.ID, SessionID: s2.ID, Endpoint: f.pAlex.Endpoint, P256dh: "new-key",
		Auth: "new-auth", Name: "Chrome on macOS", CreatedAt: t0.Add(time.Hour)}
	mustWrite(t, db, func(q *Q) error {
		created, err := q.UpsertPushSubscription(&again)
		if err != nil {
			return err
		}
		if created || again.ID != f.pAlex.ID {
			t.Errorf("re-subscribe: created=%v id=%s, want the existing %s", created, again.ID, f.pAlex.ID)
		}
		subs, err := q.ListPushSubscriptions(PushFilter{UserIDs: []UserID{f.alex.ID}})
		if err != nil {
			return err
		}
		want := PushSubscription{ID: f.pAlex.ID, UserID: f.alex.ID, SessionID: s2.ID, Endpoint: f.pAlex.Endpoint,
			P256dh: "new-key", Auth: "new-auth", Name: "Chrome on macOS", CreatedAt: t0,
			LastSuccessAt: t0.Add(time.Minute), Failures: 0}
		if len(subs) != 1 || !reflect.DeepEqual(subs[0], want) {
			t.Errorf("after re-subscribe:\n got %+v\nwant %+v", subs, want)
		}
		return nil
	})
	// The old session's logout no longer takes the subscription with it.
	mustWrite(t, db, func(q *Q) error { _, err := q.DeleteSession(f.alex.ID, f.sAlex.ID); return err })
	if got := f.list(t, PushFilter{UserIDs: []UserID{f.alex.ID}}); len(got) != 1 {
		t.Errorf("subscription gone with the old session: %v", got)
	}

	// The same endpoint for another user (another account in the same browser): a new subscription for that user.
	moved := PushSubscription{UserID: f.sam.ID, SessionID: f.sSam.ID, Endpoint: f.pAlex.Endpoint, P256dh: "k3",
		Auth: "a3", Name: "Chrome", CreatedAt: t0.Add(2 * time.Hour)}
	mustWrite(t, db, func(q *Q) error {
		created, err := q.UpsertPushSubscription(&moved)
		if err != nil || created || moved.ID != f.pAlex.ID {
			t.Errorf("rebind to another user: %v, %v, %s", created, err, moved.ID)
		}
		subs, err := q.ListPushSubscriptions(PushFilter{SessionID: f.sSam.ID})
		if err != nil {
			return err
		}
		if len(subs) != 2 {
			t.Fatalf("Sam's subscriptions = %+v", subs)
		}
		got := subs[1]
		if got.ID != f.pAlex.ID || !got.CreatedAt.Equal(t0.Add(2*time.Hour)) || !got.LastSuccessAt.IsZero() {
			t.Errorf("rebound to another user: %+v", got)
		}
		return nil
	})

	// Exactly one of session and device.
	for name, bad := range map[string]PushSubscription{
		"neither": {UserID: f.sam.ID, Endpoint: "https://push.example/x", P256dh: "k", Auth: "a", Name: "n"},
		"both": {UserID: f.sam.ID, SessionID: f.sSam.ID, DeviceID: "d", Endpoint: "https://push.example/y", P256dh: "k",
			Auth: "a", Name: "n"},
	} {
		err := db.Write(context.Background(), func(q *Q) error { _, err := q.UpsertPushSubscription(&bad); return err })
		if err == nil {
			t.Errorf("UpsertPushSubscription(%s) succeeded", name)
		}
	}
}

func TestListPushSubscriptionsFilters(t *testing.T) {
	t.Parallel()
	f := newPushFixture(t)
	now := f.e.clock.Now()
	mustWrite(t, f.db, func(q *Q) error {
		if err := q.SetPushPreferences(f.sam.ID, PushPreferences{ShareStarted: "off", AdminAlerts: true}, now); err != nil {
			return err
		}
		return q.SetPushPreferences(f.kim.ID, PushPreferences{ShareStarted: "all", AdminAlerts: true}, now)
	})
	// A second subscription of Sam, created later.
	later := PushSubscription{UserID: f.sam.ID, SessionID: f.sSam.ID, Endpoint: "https://push.example/sam2",
		P256dh: "k", Auth: "a", Name: "n", CreatedAt: now.Add(time.Minute)}
	mustWrite(t, f.db, func(q *Q) error { _, err := q.UpsertPushSubscription(&later); return err })

	sorted := func(ids ...PushSubID) []PushSubID {
		// ListPushSubscriptions orders by user ID, then creation.
		type pair struct {
			u  UserID
			id PushSubID
		}
		owner := map[PushSubID]UserID{f.pAlex.ID: f.alex.ID, f.pSam.ID: f.sam.ID, later.ID: f.sam.ID}
		var ps []pair
		for _, id := range ids {
			ps = append(ps, pair{owner[id], id})
		}
		slices.SortStableFunc(ps, func(a, b pair) int {
			switch {
			case a.u < b.u:
				return -1
			case a.u > b.u:
				return 1
			}
			return 0
		})
		var out []PushSubID
		for _, p := range ps {
			out = append(out, p.id)
		}
		return out
	}
	cases := []struct {
		name string
		f    PushFilter
		want []PushSubID
	}{
		{"all active users", PushFilter{}, sorted(f.pAlex.ID, f.pSam.ID, later.ID)},
		{"one user", PushFilter{UserIDs: []UserID{f.sam.ID}}, []PushSubID{f.pSam.ID, later.ID}},
		{"a disabled user is never returned", PushFilter{UserIDs: []UserID{f.kim.ID}}, nil},
		{"exclude", PushFilter{ExcludeUserIDs: []UserID{f.sam.ID}}, []PushSubID{f.pAlex.ID}},
		{"admins", PushFilter{AdminsOnly: true}, []PushSubID{f.pAlex.ID}},
		{"share_started", PushFilter{Pref: PrefShareStarted}, []PushSubID{f.pAlex.ID}},
		{"admin_alerts", PushFilter{Pref: PrefAdminAlerts}, sorted(f.pAlex.ID, f.pSam.ID, later.ID)},
		{"session", PushFilter{SessionID: f.sSam.ID}, []PushSubID{f.pSam.ID, later.ID}},
		{"everything", PushFilter{UserIDs: []UserID{f.alex.ID, f.sam.ID}, ExcludeUserIDs: []UserID{f.sam.ID},
			AdminsOnly: true, Pref: PrefAdminAlerts, SessionID: f.sAlex.ID}, []PushSubID{f.pAlex.ID}},
	}
	for _, c := range cases {
		if got := f.list(t, c.f); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	mustWrite(t, f.db, func(q *Q) error {
		return q.SetPushPreferences(f.alex.ID, PushPreferences{ShareStarted: "all", AdminAlerts: false}, now)
	})
	if got := f.list(t, PushFilter{AdminsOnly: true, Pref: PrefAdminAlerts}); len(got) != 0 {
		t.Errorf("admins with admin alerts off: %v", got)
	}
	err := f.db.Read(context.Background(), func(q *Q) error {
		_, err := q.ListPushSubscriptions(PushFilter{Pref: "sometimes"})
		return err
	})
	if err == nil {
		t.Error("an unknown preference was accepted")
	}
}

func TestPushSubscriptionDeletes(t *testing.T) {
	t.Parallel()
	f := newPushFixture(t)
	db, t0 := f.db, f.e.clock.Now()
	mustWrite(t, db, func(q *Q) error {
		if ok, err := q.DeletePushSubscription(f.sam.ID, f.pAlex.ID); err != nil || ok {
			t.Errorf("deleting another user's subscription = %v, %v", ok, err)
		}
		if ok, err := q.DeletePushSubscription(f.alex.ID, f.pAlex.ID); err != nil || !ok {
			t.Errorf("DeletePushSubscription = %v, %v", ok, err)
		}
		if ok, err := q.DeletePushSubscriptionByEndpoint(f.alex.ID, f.pSam.Endpoint); err != nil || ok {
			t.Errorf("unsubscribing another user's endpoint = %v, %v", ok, err)
		}
		if ok, err := q.DeletePushSubscriptionByEndpoint(f.sam.ID, f.pSam.Endpoint); err != nil || !ok {
			t.Errorf("DeletePushSubscriptionByEndpoint = %v, %v", ok, err)
		}
		if err := q.DeletePushSubscriptionByID(f.pKim.ID); err != nil {
			return err
		}
		if err := q.DeletePushSubscriptionByID(f.pKim.ID); err != nil {
			t.Errorf("DeletePushSubscriptionByID(gone) = %v, want nil", err)
		}
		return nil
	})
	if n := queryInt(t, db, "SELECT count(*) FROM push_subscriptions"); n != 0 {
		t.Errorf("subscriptions left = %d", n)
	}

	// Trim keeps the newest; DeleteAll counts.
	for i := range 4 {
		p := PushSubscription{UserID: f.sam.ID, SessionID: f.sSam.ID, Endpoint: "https://push.example/t" + itoa(i),
			P256dh: "k", Auth: "a", Name: "n", CreatedAt: t0.Add(time.Duration(i) * time.Minute)}
		mustWrite(t, db, func(q *Q) error { _, err := q.UpsertPushSubscription(&p); return err })
	}
	mustWrite(t, db, func(q *Q) error { return q.TrimPushSubscriptions(f.sam.ID, 2) })
	mustRead(t, db, func(q *Q) error {
		subs, err := q.ListPushSubscriptions(PushFilter{})
		var eps []string
		for _, s := range subs {
			eps = append(eps, s.Endpoint)
		}
		if !slices.Equal(eps, []string{"https://push.example/t2", "https://push.example/t3"}) {
			t.Errorf("after trim: %v", eps)
		}
		return err
	})
	mustWrite(t, db, func(q *Q) error {
		n, err := q.DeleteAllPushSubscriptions()
		if err != nil || n != 2 {
			t.Errorf("DeleteAllPushSubscriptions = %d, %v", n, err)
		}
		return nil
	})
	// A deleted session takes its subscriptions along.
	p := PushSubscription{UserID: f.sam.ID, SessionID: f.sSam.ID, Endpoint: "https://push.example/s", P256dh: "k",
		Auth: "a", Name: "n"}
	mustWrite(t, db, func(q *Q) error {
		if _, err := q.UpsertPushSubscription(&p); err != nil {
			return err
		}
		_, err := q.DeleteSession(f.sam.ID, f.sSam.ID)
		return err
	})
	if n := queryInt(t, db, "SELECT count(*) FROM push_subscriptions"); n != 0 {
		t.Errorf("subscriptions after their session was deleted = %d", n)
	}
}

func TestPushResultsAndPrune(t *testing.T) {
	t.Parallel()
	f := newPushFixture(t)
	db, t0 := f.db, f.e.clock.Now()
	fail := func(id PushSubID, n int) {
		for range n {
			mustWrite(t, db, func(q *Q) error { return q.RecordPushResult(id, false, t0) })
		}
	}
	// Alex: 5 failures after a success long ago. Sam: 5 failures, recent success before them. Kim: 5 failures, never
	// a success. Sam's second subscription: 2 failures.
	mustWrite(t, db, func(q *Q) error { return q.RecordPushResult(f.pAlex.ID, true, t0.Add(-10*24*time.Hour)) })
	mustWrite(t, db, func(q *Q) error { return q.RecordPushResult(f.pSam.ID, true, t0.Add(-time.Hour)) })
	fail(f.pAlex.ID, 5)
	fail(f.pSam.ID, 5)
	fail(f.pKim.ID, 5)
	second := PushSubscription{UserID: f.sam.ID, SessionID: f.sSam.ID, Endpoint: "https://push.example/2",
		P256dh: "k", Auth: "a", Name: "n"}
	mustWrite(t, db, func(q *Q) error { _, err := q.UpsertPushSubscription(&second); return err })
	fail(second.ID, 2)
	if n := queryInt(t, db, "SELECT failures FROM push_subscriptions WHERE id = ?", string(f.pAlex.ID)); n != 5 {
		t.Errorf("failures = %d", n)
	}
	mustWrite(t, db, func(q *Q) error {
		if err := q.RecordPushResult("gone", true, t0); !errors.Is(err, ErrNotFound) {
			t.Errorf("RecordPushResult(unknown) = %v", err)
		}
		n, err := q.PrunePushSubscriptions(5, t0.Add(-24*time.Hour))
		if err != nil || n != 2 {
			t.Errorf("PrunePushSubscriptions = %d, %v, want 2 (Alex and Kim)", n, err)
		}
		return nil
	})
	var left []PushSubID
	mustRead(t, db, func(q *Q) error {
		return q.queryAll("left", "SELECT id FROM push_subscriptions ORDER BY rowid", nil, func(r scanner) error {
			var id string
			err := r.Scan(&id)
			left = append(left, PushSubID(id))
			return err
		})
	})
	if !slices.Equal(left, []PushSubID{f.pSam.ID, second.ID}) {
		t.Errorf("left after prune: %v", left)
	}
	// A success resets the failures.
	mustWrite(t, db, func(q *Q) error { return q.RecordPushResult(f.pSam.ID, true, t0) })
	if n := queryInt(t, db, "SELECT failures FROM push_subscriptions WHERE id = ? AND last_success_at = ?",
		string(f.pSam.ID), t0.UnixMilli()); n != 0 {
		t.Errorf("failures after a success = %d", n)
	}
}

func TestPushPreferences(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	sam := e.newUser(db, "Sam")
	now := e.clock.Now()
	mustWrite(t, db, func(q *Q) error {
		p, err := q.PushPreferences(sam.ID)
		if err != nil || p != (PushPreferences{ShareStarted: "all", AdminAlerts: true}) {
			t.Errorf("defaults = %+v, %v", p, err)
		}
		for _, want := range []PushPreferences{{"off", false}, {"all", false}, {"off", true}} {
			if err := q.SetPushPreferences(sam.ID, want, now); err != nil {
				return err
			}
			if got, err := q.PushPreferences(sam.ID); err != nil || got != want {
				t.Errorf("PushPreferences = %+v, %v, want %+v", got, err, want)
			}
		}
		if err := q.SetPushPreferences(sam.ID, PushPreferences{ShareStarted: "some"}, now); err == nil {
			t.Error("a bad shareStarted value was stored")
		}
		if err := q.SetPushPreferences("nobody", PushPreferences{ShareStarted: "all"}, now); err == nil {
			t.Error("preferences for an unknown user were stored")
		}
		return nil
	})
}
