package store

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"
)

func TestCreateUserRoundTrip(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	now := e.clock.Now()
	admin := e.newUser(db, "Alex", func(u *User) { u.Role, u.CreatedVia = RoleAdmin, "setup" })
	if len(admin.ID) != idLen || admin.Role != RoleAdmin || admin.Status != StatusActive {
		t.Errorf("created admin = %+v", admin)
	}
	full := User{
		Username: "Sam", UsernameKey: "sam", PasswordHash: testPHC, Role: RoleUser, Status: StatusDisabled,
		CreatedVia: "signup", ApprovedBy: admin.ID, ApprovedAt: now.Add(time.Minute + 123456789*time.Nanosecond),
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now, PasswordChangedAt: now.Add(2 * time.Minute),
		LastLoginAt: now.Add(3 * time.Minute),
	}
	mustWrite(t, db, func(q *Q) error { return q.CreateUser(&full) })
	// The struct was rounded to what the DB keeps, so it equals the row read back.
	if full.ApprovedAt.Nanosecond()%int(time.Millisecond) != 0 || full.ApprovedAt.Location() != time.UTC {
		t.Errorf("ApprovedAt not normalized: %v", full.ApprovedAt)
	}
	mustRead(t, db, func(q *Q) error {
		got, err := q.UserByID(full.ID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(got, full) {
			t.Errorf("UserByID:\n got %+v\nwant %+v", got, full)
		}
		byKey, err := q.UserByUsernameKey("sam")
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(byKey, full) {
			t.Errorf("UserByUsernameKey = %+v", byKey)
		}
		if _, err := q.UserByID("nobody"); !errors.Is(err, ErrNotFound) {
			t.Errorf("UserByID(unknown) = %v", err)
		}
		if _, err := q.UserByUsernameKey("nobody"); !errors.Is(err, ErrNotFound) {
			t.Errorf("UserByUsernameKey(unknown) = %v", err)
		}
		return nil
	})

	// Defaults: role user, status active, CreatedAt from the store clock, UpdatedAt = CreatedAt, NULL columns.
	bare := User{Username: "Kim", UsernameKey: "kim", CreatedVia: "invite"}
	mustWrite(t, db, func(q *Q) error { return q.CreateUser(&bare) })
	if bare.Role != RoleUser || bare.Status != StatusActive || !bare.CreatedAt.Equal(now) || bare.UpdatedAt != bare.CreatedAt {
		t.Errorf("defaults: %+v", bare)
	}
	if n := queryInt(t, db, `SELECT count(*) FROM users WHERE id = ? AND password_hash IS NULL AND invite_id IS NULL
		AND approved_by IS NULL AND approved_at IS NULL AND password_changed_at IS NULL AND last_login_at IS NULL`,
		string(bare.ID)); n != 1 {
		t.Error("empty optional fields are not NULL")
	}
}

func TestCreateUserConflicts(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	e.newUser(db, "Alex")
	err := db.Write(context.Background(), func(q *Q) error {
		return q.CreateUser(&User{Username: "ALEX", UsernameKey: "alex", CreatedVia: "signup"})
	})
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Column != "username_key" || !errors.Is(err, ErrConflict) {
		t.Errorf("CreateUser with a taken key = %v, want *ConflictError{username_key}", err)
	}
	// A CHECK violation is a plain error, not a conflict.
	err = db.Write(context.Background(), func(q *Q) error {
		return q.CreateUser(&User{Username: "Zed", UsernameKey: "zed", CreatedVia: "magic"})
	})
	if err == nil || errors.Is(err, ErrConflict) {
		t.Errorf("CreateUser with a bad created_via = %v", err)
	}
}

func TestNewIDCollisionRetriesOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	o := e.opts(nil)
	o.newID = idSequence("aaaaaaaaaaaa", "aaaaaaaaaaaa", "bbbbbbbbbbbb", "cccccccccccc", "cccccccccccc", "cccccccccccc")
	db, err := Open(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	create := func(name string) (User, error) {
		u := User{Username: name, UsernameKey: name, CreatedVia: "invite"}
		err := db.Write(ctx, func(q *Q) error { return q.CreateUser(&u) })
		return u, err
	}
	if u, err := create("a"); err != nil || u.ID != "aaaaaaaaaaaa" {
		t.Fatalf("first user: %v, %v", u.ID, err)
	}
	// The second user draws the taken ID first and retries with the next one.
	if u, err := create("b"); err != nil || u.ID != "bbbbbbbbbbbb" {
		t.Fatalf("second user: %v, %v", u.ID, err)
	}
	if u, err := create("c"); err != nil || u.ID != "cccccccccccc" {
		t.Fatalf("third user: %v, %v", u.ID, err)
	}
	// Two collisions in a row fail, and not as a user-visible conflict.
	_, err = create("d")
	if err == nil || errors.Is(err, ErrConflict) {
		t.Errorf("two ID collisions: %v, want a plain error", err)
	}
	if n := queryInt(t, db, "SELECT count(*) FROM users"); n != 3 {
		t.Errorf("users = %d, want 3", n)
	}
}

func TestUserUpdates(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	admin := e.newUser(db, "Alex", func(u *User) { u.Role = RoleAdmin })
	sam := e.newUser(db, "Sam")
	later := e.clock.Now().Add(time.Hour)
	mustWrite(t, db, func(q *Q) error {
		if err := q.RenameUser(sam.ID, "Samuel", "samuel", later); err != nil {
			return err
		}
		var ce *ConflictError
		if err := q.RenameUser(sam.ID, "ALEX", "alex", later); !errors.As(err, &ce) || ce.Column != "username_key" {
			t.Errorf("RenameUser to a taken name = %v", err)
		}
		if err := q.SetRole(sam.ID, RoleAdmin, later); err != nil {
			return err
		}
		if err := q.SetStatus(sam.ID, StatusDisabled, later); err != nil {
			return err
		}
		if err := q.TouchLogin(sam.ID, later.Add(time.Second)); err != nil {
			return err
		}
		for name, err := range map[string]error{
			"RenameUser":      q.RenameUser("nobody", "x", "x", later),
			"SetRole":         q.SetRole("nobody", RoleUser, later),
			"SetStatus":       q.SetStatus("nobody", StatusActive, later),
			"TouchLogin":      q.TouchLogin("nobody", later),
			"SetPasswordHash": q.SetPasswordHash("nobody", testPHC, later),
			"DeleteUser":      q.DeleteUser("nobody"),
			"Approve":         q.Approve("nobody", admin.ID, later),
		} {
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("%s(unknown) = %v, want ErrNotFound", name, err)
			}
		}
		return nil
	})
	mustRead(t, db, func(q *Q) error {
		u, err := q.UserByID(sam.ID)
		if err != nil {
			return err
		}
		if u.Username != "Samuel" || u.UsernameKey != "samuel" || u.Role != RoleAdmin || u.Status != StatusDisabled ||
			!u.UpdatedAt.Equal(later) || !u.LastLoginAt.Equal(later.Add(time.Second)) {
			t.Errorf("after updates: %+v", u)
		}
		return nil
	})

	// SetPasswordHash: "" clears it (reset pending) without touching password_changed_at; a hash sets both.
	mustWrite(t, db, func(q *Q) error { return q.SetPasswordHash(sam.ID, "", later.Add(time.Minute)) })
	mustRead(t, db, func(q *Q) error {
		u, err := q.UserByID(sam.ID)
		if u.PasswordHash != "" || !u.PasswordChangedAt.IsZero() || !u.UpdatedAt.Equal(later.Add(time.Minute)) {
			t.Errorf("after clearing the password: %+v", u)
		}
		return err
	})
	mustWrite(t, db, func(q *Q) error { return q.SetPasswordHash(sam.ID, testPHC, later.Add(2*time.Minute)) })
	mustRead(t, db, func(q *Q) error {
		u, err := q.UserByID(sam.ID)
		if u.PasswordHash != testPHC || !u.PasswordChangedAt.Equal(later.Add(2*time.Minute)) {
			t.Errorf("after setting the password: %+v", u)
		}
		return err
	})
}

func TestApproveAndPending(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	admin := e.newUser(db, "Alex", func(u *User) { u.Role = RoleAdmin })
	pending := func(u *User) { u.Status, u.CreatedVia = StatusPending, "signup" }
	kim := e.newUser(db, "Kim", pending)
	e.newUser(db, "Lee", pending)
	e.newUser(db, "Max", pending)
	now := e.clock.Now().Add(time.Hour)
	mustWrite(t, db, func(q *Q) error {
		if n, err := q.CountPending(); err != nil || n != 3 {
			t.Errorf("CountPending = %d, %v", n, err)
		}
		if err := q.Approve(kim.ID, admin.ID, now); err != nil {
			return err
		}
		// Only a pending user can be approved.
		if err := q.Approve(kim.ID, admin.ID, now); !errors.Is(err, ErrNotFound) {
			t.Errorf("approving an active user = %v", err)
		}
		if err := q.Approve(admin.ID, admin.ID, now); !errors.Is(err, ErrNotFound) {
			t.Errorf("approving an admin = %v", err)
		}
		u, err := q.UserByID(kim.ID)
		if err != nil {
			return err
		}
		if u.Status != StatusActive || u.ApprovedBy != admin.ID || !u.ApprovedAt.Equal(now) {
			t.Errorf("approved user: %+v", u)
		}
		ids, err := q.DeletePending()
		if err != nil {
			return err
		}
		if len(ids) != 2 || !slices.IsSorted(ids) {
			t.Errorf("DeletePending = %v", ids)
		}
		if n, err := q.CountPending(); err != nil || n != 0 {
			t.Errorf("CountPending after reject all = %d, %v", n, err)
		}
		ids, err = q.DeletePending()
		if err != nil || len(ids) != 0 {
			t.Errorf("DeletePending on an empty queue = %v, %v", ids, err)
		}
		return nil
	})
	// Rejecting frees the username.
	e.newUser(db, "Lee")
}

func TestAdminCounts(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	check := func(wantActive int, wantAny bool) {
		t.Helper()
		mustRead(t, db, func(q *Q) error {
			n, err := q.CountActiveAdmins()
			if err != nil {
				return err
			}
			anyAdmin, err := q.AnyAdmin()
			if err != nil {
				return err
			}
			if n != wantActive || anyAdmin != wantAny {
				t.Errorf("CountActiveAdmins = %d, AnyAdmin = %v, want %d, %v", n, anyAdmin, wantActive, wantAny)
			}
			return nil
		})
	}
	check(0, false)
	e.newUser(db, "Sam")
	check(0, false)
	alex := e.newUser(db, "Alex", func(u *User) { u.Role = RoleAdmin })
	check(1, true)
	mustWrite(t, db, func(q *Q) error { return q.SetStatus(alex.ID, StatusDisabled, e.clock.Now()) })
	check(0, true) // a disabled admin still makes setup unavailable
	e.newUser(db, "Kim", func(u *User) { u.Role = RoleAdmin })
	check(1, true)
}

func TestListUsers(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	t0 := e.clock.Now()
	alex := e.newUser(db, "Alex", func(u *User) { u.Role, u.CreatedVia = RoleAdmin, "setup" })
	var inv, cliInv Invite
	mustWrite(t, db, func(q *Q) error {
		inv = Invite{TokenHash: []byte("inv"), CreatedBy: &UserRef{ID: alex.ID}, ExpiresAt: t0.Add(time.Hour), MaxUses: 5}
		if err := q.CreateInvite(&inv); err != nil {
			return err
		}
		cliInv = Invite{TokenHash: []byte("cli"), ExpiresAt: t0.Add(time.Hour), MaxUses: 5}
		return q.CreateInvite(&cliInv)
	})
	sam := e.newUser(db, "sam", func(u *User) { u.InviteID = inv.ID })
	bea := e.newUser(db, "Bea", func(u *User) { u.InviteID = cliInv.ID })
	kim := e.newUser(db, "Kim", func(u *User) { u.Status, u.CreatedVia = StatusPending, "signup" })
	zed := e.newUser(db, "Zed", func(u *User) { u.PasswordHash = "" })
	e.newSession(db, sam.ID, "s1", t0.Add(time.Minute))
	e.newSession(db, sam.ID, "s2", t0.Add(2*time.Minute))
	newDevice(t, db, "dev1", sam.ID, t0.Add(3*time.Minute))
	e.newSession(db, bea.ID, "s3", t0.Add(5*time.Minute))
	newDevice(t, db, "dev2", bea.ID, t0.Add(4*time.Minute))
	mustWrite(t, db, func(q *Q) error {
		// An older and a newer sign-up request of Kim: the newest row's IP counts.
		for _, ip := range []string{"198.51.100.1", "198.51.100.23"} {
			err := q.AppendAudit(AuditEntry{Action: "user.signup_requested",
				Actor:      Actor{Kind: ActorAnonymous, Name: "Kim", IP: ip},
				TargetKind: "user", TargetID: string(kim.ID), TargetName: "Kim"})
			if err != nil {
				return err
			}
		}
		// Sam's own audit rows never become a SignupIP (Sam is not pending).
		return q.AppendAudit(AuditEntry{Action: "user.signup_requested",
			Actor: Actor{Kind: ActorAnonymous, IP: "203.0.113.9"}, TargetID: string(sam.ID)})
	})

	var all []UserRow
	mustRead(t, db, func(q *Q) error {
		var err error
		all, err = q.ListUsers("")
		return err
	})
	var names []string
	byName := map[string]UserRow{}
	for _, r := range all {
		names = append(names, r.Username)
		byName[r.Username] = r
	}
	if want := []string{"Alex", "Bea", "Kim", "sam", "Zed"}; !slices.Equal(names, want) {
		t.Fatalf("ListUsers order = %v, want %v (by username_key)", names, want)
	}
	if r := byName["sam"]; r.InvitedBy == nil || *r.InvitedBy != (UserRef{ID: alex.ID, Username: "Alex"}) ||
		r.Sessions != 2 || r.Devices != 1 || !r.LastSeenAt.Equal(t0.Add(3*time.Minute)) || r.ResetPending ||
		r.SignupIP != "" || !reflect.DeepEqual(r.User, sam) {
		t.Errorf("sam: %+v invitedBy=%v", r, r.InvitedBy)
	}
	if r := byName["Bea"]; r.InvitedBy != nil || !r.LastSeenAt.Equal(t0.Add(5*time.Minute)) {
		t.Errorf("Bea (CLI invite, session newer than device): %+v", r)
	}
	if r := byName["Kim"]; r.SignupIP != "198.51.100.23" || r.Status != StatusPending || !r.LastSeenAt.IsZero() ||
		r.Sessions != 0 {
		t.Errorf("Kim: %+v", r)
	}
	if r := byName["Zed"]; !r.ResetPending {
		t.Errorf("Zed has no password hash, ResetPending = false")
	}
	if r := byName["Alex"]; r.InvitedBy != nil || r.ResetPending {
		t.Errorf("Alex: %+v", r)
	}
	_ = zed

	mustRead(t, db, func(q *Q) error {
		rows, err := q.ListUsers(StatusPending)
		if err != nil {
			return err
		}
		if len(rows) != 1 || rows[0].ID != kim.ID {
			t.Errorf("ListUsers(pending) = %+v", rows)
		}
		rows, err = q.ListUsers(StatusDisabled)
		if err != nil || len(rows) != 0 {
			t.Errorf("ListUsers(disabled) = %+v, %v", rows, err)
		}
		return nil
	})
}

func TestDeleteUserCascades(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	now := e.clock.Now()
	alex := e.newUser(db, "Alex", func(u *User) { u.Role = RoleAdmin })
	sam := e.newUser(db, "Sam")
	s := e.newSession(db, alex.ID, "a1", now)
	newDevice(t, db, "dev1", alex.ID, now)
	var inv Invite
	var room Room
	mustWrite(t, db, func(q *Q) error {
		inv = Invite{TokenHash: []byte("inv"), CreatedBy: &UserRef{ID: alex.ID}, ExpiresAt: now.Add(time.Hour), MaxUses: 1}
		if err := q.CreateInvite(&inv); err != nil {
			return err
		}
		if err := q.RevokeInvite(inv.ID, alex.ID, now); err != nil {
			return err
		}
		room = Room{Name: "Games", NameKey: "games", CreatedBy: alex.ID}
		if err := q.CreateRoom(&room); err != nil {
			return err
		}
		if err := q.ReplacePasswordReset(PasswordReset{TokenHash: []byte("r"), UserID: sam.ID, CreatedBy: alex.ID,
			ExpiresAt: now.Add(time.Hour)}); err != nil {
			return err
		}
		if err := q.Approve(sam.ID, alex.ID, now); !errors.Is(err, ErrNotFound) { // Sam is active already
			t.Errorf("Approve(active) = %v", err)
		}
		if _, err := q.exec("UPDATE users SET approved_by = ? WHERE id = ?", string(alex.ID), string(sam.ID)); err != nil {
			return err
		}
		if _, err := q.UpsertPushSubscription(&PushSubscription{UserID: alex.ID, SessionID: s.ID,
			Endpoint: "https://push.example/1", P256dh: "k", Auth: "a", Name: "Chrome"}); err != nil {
			return err
		}
		if err := q.SetPushPreferences(alex.ID, PushPreferences{ShareStarted: "off"}, now); err != nil {
			return err
		}
		return q.DeleteUser(alex.ID)
	})
	for query, want := range map[string]int{
		"SELECT count(*) FROM sessions":           0,
		"SELECT count(*) FROM devices":            0,
		"SELECT count(*) FROM push_subscriptions": 0,
		"SELECT count(*) FROM push_preferences":   0,
	} {
		if n := queryInt(t, db, query); n != want {
			t.Errorf("after DeleteUser: %s = %d, want %d", query, n, want)
		}
	}
	mustRead(t, db, func(q *Q) error {
		got, err := q.InviteByID(inv.ID)
		if err != nil {
			return err
		}
		if got.CreatedBy != nil || got.RevokedBy != "" || got.RevokedAt.IsZero() {
			t.Errorf("invite after its creator was deleted: %+v", got)
		}
		r, err := q.RoomByID(room.ID)
		if err != nil {
			return err
		}
		if r.CreatedBy != "" {
			t.Errorf("room creator after deletion = %q", r.CreatedBy)
		}
		u, err := q.UserByID(sam.ID)
		if err != nil {
			return err
		}
		if u.ApprovedBy != "" {
			t.Errorf("approved_by after the approver was deleted = %q", u.ApprovedBy)
		}
		reset, err := q.PasswordResetByHash([]byte("r"), now)
		if err != nil {
			return err
		}
		if reset.CreatedBy != "" {
			t.Errorf("reset creator after deletion = %q", reset.CreatedBy)
		}
		return nil
	})
}

func TestKnownIPs(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	t0 := e.clock.Now()
	alex := e.newUser(db, "Alex")
	sam := e.newUser(db, "Sam")
	ip := func(s string) func(*Session) { return func(x *Session) { x.LastIP = s } }
	e.newSession(db, alex.ID, "a1", t0, ip("203.0.113.7"))
	e.newSession(db, alex.ID, "a2", t0, ip("203.0.113.7"))
	e.newSession(db, alex.ID, "a3", t0, ip("2001:db8::1"))
	e.newSession(db, alex.ID, "a4", t0, ip(""))
	// Idle-expired at t0+1h, and absolutely expired at t0+2h (with a later idle expiry, clamped).
	e.newSession(db, alex.ID, "a5", t0, ip("198.51.100.5"), func(s *Session) { s.IdleExpiresAt = t0.Add(time.Hour) })
	e.newSession(db, alex.ID, "a6", t0, ip("198.51.100.6"), func(s *Session) { s.ExpiresAt = t0.Add(2 * time.Hour) })
	e.newSession(db, sam.ID, "s1", t0, ip("192.0.2.99"))
	cases := []struct {
		key  string
		at   time.Time
		want []string
	}{
		{"alex", t0.Add(time.Hour), []string{"198.51.100.5", "198.51.100.6", "2001:db8::1", "203.0.113.7"}},
		{"alex", t0.Add(time.Hour + time.Millisecond), []string{"198.51.100.6", "2001:db8::1", "203.0.113.7"}},
		{"alex", t0.Add(2*time.Hour + time.Millisecond), []string{"2001:db8::1", "203.0.113.7"}},
		{"sam", t0, []string{"192.0.2.99"}},
		{"nobody", t0, nil},
	}
	mustRead(t, db, func(q *Q) error {
		for _, c := range cases {
			got, err := q.KnownIPs(c.key, c.at)
			if err != nil {
				return err
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("KnownIPs(%s, %v) = %v, want %v", c.key, c.at.Sub(t0), got, c.want)
			}
		}
		return nil
	})
}

func TestSessionLifecycle(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	t0 := e.clock.Now()
	alex := e.newUser(db, "Alex")
	s := e.newSession(db, alex.ID, "tok1", t0)
	if len(s.ID) != idLen || !s.RotatedAt.Equal(t0) || !s.LastSeenAt.Equal(t0) {
		t.Errorf("created session: %+v", s)
	}
	ctx := context.Background()
	lookup := func(h string, at time.Time) (Session, error) {
		var got Session
		err := db.Read(ctx, func(q *Q) error {
			var err error
			var u User
			got, u, err = q.SessionByTokenHash([]byte(h), at)
			if err == nil && u.ID != alex.ID {
				t.Errorf("session user = %+v", u)
			}
			return err
		})
		return got, err
	}
	got, err := lookup("tok1", t0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, s) {
		t.Errorf("SessionByTokenHash:\n got %+v\nwant %+v", got, s)
	}

	// Rotation after 24 h: the old token stays valid until prevValidUntil, inclusive, and not 1 ms later.
	t1 := t0.Add(24 * time.Hour)
	grace := t1.Add(60 * time.Second)
	mustWrite(t, db, func(q *Q) error { return q.RotateSession(s.ID, []byte("tok2"), grace, t1) })
	if got, err := lookup("tok1", grace); err != nil || got.ID != s.ID || !got.RotatedAt.Equal(t1) ||
		string(got.TokenHash) != "tok2" || string(got.PrevTokenHash) != "tok1" || !got.PrevValidUntil.Equal(grace) {
		t.Errorf("old token at prev_valid_until: %+v, %v", got, err)
	}
	if _, err := lookup("tok1", grace.Add(time.Millisecond)); !errors.Is(err, ErrNotFound) {
		t.Errorf("old token 1 ms after prev_valid_until: %v, want ErrNotFound", err)
	}
	if _, err := lookup("tok2", grace.Add(time.Hour)); err != nil {
		t.Errorf("new token: %v", err)
	}
	// A second rotation drops the first token completely.
	t2 := t1.Add(24 * time.Hour)
	mustWrite(t, db, func(q *Q) error { return q.RotateSession(s.ID, []byte("tok3"), t2.Add(time.Minute), t2) })
	if _, err := lookup("tok1", t2); !errors.Is(err, ErrNotFound) {
		t.Errorf("the token of two rotations ago: %v", err)
	}
	if _, err := lookup("tok2", t2); err != nil {
		t.Errorf("the previous token within its grace: %v", err)
	}

	// Idle expiry: valid at idle_expires_at, not 1 ms later; a touch moves it (clamped to expires_at).
	idle := s.IdleExpiresAt
	if _, err := lookup("tok3", idle); err != nil {
		t.Errorf("at idle_expires_at: %v", err)
	}
	if _, err := lookup("tok3", idle.Add(time.Millisecond)); !errors.Is(err, ErrNotFound) {
		t.Errorf("1 ms past idle_expires_at: %v", err)
	}
	// The previous token is also dead once the session idles out, grace or not.
	mustWrite(t, db, func(q *Q) error { return q.RotateSession(s.ID, []byte("tok4"), idle.Add(time.Hour), idle) })
	if _, err := lookup("tok3", idle.Add(time.Millisecond)); !errors.Is(err, ErrNotFound) {
		t.Errorf("previous token of an idle-expired session: %v", err)
	}
	touch := idle.Add(-time.Hour)
	mustWrite(t, db, func(q *Q) error {
		if err := q.TouchSession(s.ID, "198.51.100.23", touch, s.ExpiresAt.Add(24*time.Hour)); err != nil {
			return err
		}
		if err := q.TouchSession("nope", "", touch, touch); !errors.Is(err, ErrNotFound) {
			t.Errorf("TouchSession(unknown) = %v", err)
		}
		if err := q.RotateSession("nope", []byte("x"), touch, touch); !errors.Is(err, ErrNotFound) {
			t.Errorf("RotateSession(unknown) = %v", err)
		}
		if err := q.RotateSession(s.ID, nil, touch, touch); err == nil {
			t.Error("RotateSession without a new hash succeeded")
		}
		return nil
	})
	got, err = lookup("tok4", s.ExpiresAt)
	if err != nil {
		t.Fatalf("at expires_at after a touch: %v", err)
	}
	if !got.IdleExpiresAt.Equal(s.ExpiresAt) || !got.LastSeenAt.Equal(touch) || got.LastIP != "198.51.100.23" {
		t.Errorf("after touch: %+v", got)
	}
	if _, err := lookup("tok4", s.ExpiresAt.Add(time.Millisecond)); !errors.Is(err, ErrNotFound) {
		t.Errorf("1 ms past expires_at: %v", err)
	}
	// An empty IP keeps the last one.
	mustWrite(t, db, func(q *Q) error { return q.TouchSession(s.ID, "", touch.Add(time.Minute), touch) })
	if got, _ := lookup("tok4", touch); got.LastIP != "198.51.100.23" || !got.IdleExpiresAt.Equal(touch) {
		t.Errorf("touch without an IP: %+v", got)
	}
	if _, err := lookup("", t0); !errors.Is(err, ErrNotFound) {
		t.Errorf("empty hash: %v", err)
	}
}

func TestCreateSessionValidates(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	t0 := e.clock.Now()
	alex := e.newUser(db, "Alex")
	for name, s := range map[string]Session{
		"no hash":        {UserID: alex.ID, IdleExpiresAt: t0, ExpiresAt: t0},
		"no expiry":      {UserID: alex.ID, TokenHash: []byte("x"), IdleExpiresAt: t0},
		"no idle expiry": {UserID: alex.ID, TokenHash: []byte("x"), ExpiresAt: t0},
		"unknown user":   {UserID: "nobody", TokenHash: []byte("x"), IdleExpiresAt: t0, ExpiresAt: t0},
	} {
		err := db.Write(context.Background(), func(q *Q) error { return q.CreateSession(&s) })
		if err == nil {
			t.Errorf("CreateSession(%s) succeeded", name)
		}
	}
	// The idle expiry is clamped to the absolute one; CreatedAt defaults to the store clock.
	s := Session{UserID: alex.ID, TokenHash: []byte("x"), IdleExpiresAt: t0.Add(48 * time.Hour),
		ExpiresAt: t0.Add(24 * time.Hour)}
	mustWrite(t, db, func(q *Q) error { return q.CreateSession(&s) })
	if !s.IdleExpiresAt.Equal(s.ExpiresAt) || !s.CreatedAt.Equal(t0) || s.PrevTokenHash != nil {
		t.Errorf("created: %+v", s)
	}
	if n := queryInt(t, db, "SELECT count(*) FROM sessions WHERE prev_token_hash IS NULL AND prev_valid_until IS NULL"); n != 1 {
		t.Error("prev_token_hash/prev_valid_until are not NULL for a new session")
	}
	// A session created with a previous hash (a restored or migrated one) round-trips it.
	withPrev := Session{UserID: alex.ID, TokenHash: []byte("y"), PrevTokenHash: []byte("old"),
		PrevValidUntil: t0.Add(time.Minute), IdleExpiresAt: t0.Add(time.Hour), ExpiresAt: t0.Add(time.Hour)}
	mustWrite(t, db, func(q *Q) error { return q.CreateSession(&withPrev) })
	mustRead(t, db, func(q *Q) error {
		got, _, err := q.SessionByTokenHash([]byte("old"), t0.Add(time.Minute))
		if err == nil && !reflect.DeepEqual(got, withPrev) {
			t.Errorf("session with a previous hash:\n got %+v\nwant %+v", got, withPrev)
		}
		return err
	})
}

func TestSessionListsAndDeletes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	t0 := e.clock.Now()
	alex := e.newUser(db, "Alex")
	sam := e.newUser(db, "Sam")
	var ids []SessionID
	for i, h := range []string{"a", "b", "c", "d", "e"} {
		// Seen in the order c (newest), e, a, d, b.
		seen := map[string]time.Duration{"a": 3, "b": 1, "c": 5, "d": 2, "e": 4}[h]
		s := e.newSession(db, alex.ID, h, t0.Add(time.Duration(i)*time.Second),
			func(s *Session) { s.LastSeenAt = t0.Add(seen * time.Minute) })
		ids = append(ids, s.ID)
	}
	samS := e.newSession(db, sam.ID, "s", t0)
	name := map[SessionID]string{}
	for i, h := range []string{"a", "b", "c", "d", "e"} {
		name[ids[i]] = h
	}
	order := func() string {
		var out string
		mustRead(t, db, func(q *Q) error {
			list, err := q.ListSessions(alex.ID)
			for _, s := range list {
				out += name[s.ID]
			}
			return err
		})
		return out
	}
	if got := order(); got != "ceadb" {
		t.Errorf("ListSessions order = %q, want most recently seen first (ceadb)", got)
	}
	mustWrite(t, db, func(q *Q) error {
		trimmed, err := q.TrimSessions(alex.ID, 3)
		if err != nil {
			return err
		}
		want := []SessionID{ids[1], ids[3]} // b and d, the least recently seen
		slices.Sort(want)
		if !slices.Equal(trimmed, want) {
			t.Errorf("TrimSessions = %v, want %v", trimmed, want)
		}
		if again, err := q.TrimSessions(alex.ID, 3); err != nil || len(again) != 0 {
			t.Errorf("TrimSessions under the cap = %v, %v", again, err)
		}
		ok, err := q.DeleteSession(sam.ID, ids[0]) // not Sam's
		if err != nil || ok {
			t.Errorf("DeleteSession of another user's session = %v, %v", ok, err)
		}
		ok, err = q.DeleteSession(alex.ID, ids[0])
		if err != nil || !ok {
			t.Errorf("DeleteSession = %v, %v", ok, err)
		}
		deleted, err := q.DeleteSessions(alex.ID, ids[2])
		if err != nil || !slices.Equal(deleted, []SessionID{ids[4]}) {
			t.Errorf("DeleteSessions(except c) = %v, %v", deleted, err)
		}
		deleted, err = q.DeleteSessions(alex.ID, "")
		if err != nil || !slices.Equal(deleted, []SessionID{ids[2]}) {
			t.Errorf("DeleteSessions(all) = %v, %v", deleted, err)
		}
		return nil
	})
	if got := order(); got != "" {
		t.Errorf("sessions left: %q", got)
	}
	if n := queryInt(t, db, "SELECT count(*) FROM sessions WHERE id = ?", string(samS.ID)); n != 1 {
		t.Error("Sam's session was deleted")
	}

	// Ties in last_seen_at and created_at: the newest inserted session is kept.
	var tie []SessionID
	for _, h := range []string{"t1", "t2", "t3"} {
		tie = append(tie, e.newSession(db, alex.ID, h, t0).ID)
	}
	mustWrite(t, db, func(q *Q) error {
		trimmed, err := q.TrimSessions(alex.ID, 1)
		want := []SessionID{tie[0], tie[1]}
		slices.Sort(want)
		if err != nil || !slices.Equal(trimmed, want) {
			t.Errorf("TrimSessions with ties = %v, %v, want %v", trimmed, err, want)
		}
		trimmed, err = q.TrimSessions(alex.ID, -1)
		if err != nil || !slices.Equal(trimmed, []SessionID{tie[2]}) {
			t.Errorf("TrimSessions(-1) = %v, %v", trimmed, err)
		}
		return nil
	})
}

func TestDevicesM1(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	t0 := e.clock.Now()
	alex := e.newUser(db, "Alex")
	sam := e.newUser(db, "Sam")
	newDevice(t, db, "d1", alex.ID, t0.Add(time.Minute))
	newDevice(t, db, "d2", alex.ID, t0.Add(2*time.Minute))
	newDevice(t, db, "d3", alex.ID, t0)
	newDevice(t, db, "d4", sam.ID, t0)
	mustExecW(t, db, `INSERT INTO device_codes (device_code_hash, user_code_hash, client_kind, device_name, os,
		app_version, request_ip, created_at, expires_at, status, user_id)
		VALUES (x'01', x'02', 'desktop', 'n', 'linux', 'v', '', 0, 1, 'approved', ?),
		       (x'03', x'04', 'desktop', 'n', 'linux', 'v', '', 0, 1, 'approved', ?),
		       (x'05', x'06', 'desktop', 'n', 'linux', 'v', '', 0, 1, 'pending', NULL)`, string(alex.ID), string(sam.ID))
	mustWrite(t, db, func(q *Q) error {
		list, err := q.ListDevices(alex.ID)
		if err != nil {
			return err
		}
		var got []DeviceID
		for _, d := range list {
			got = append(got, d.ID)
		}
		if !slices.Equal(got, []DeviceID{"d2", "d1", "d3"}) {
			t.Errorf("ListDevices order = %v", got)
		}
		want := Device{ID: "d2", UserID: alex.ID, Name: "Alex-PC", ClientKind: "desktop", OS: "windows",
			AppVersion: "0.2.0", LinkedVia: "password", CreatedAt: t0.Add(2 * time.Minute),
			LastSeenAt: t0.Add(2 * time.Minute), LastIP: "203.0.113.7"}
		if !reflect.DeepEqual(list[0], want) {
			t.Errorf("device:\n got %+v\nwant %+v", list[0], want)
		}
		if ok, err := q.DeleteDevice(sam.ID, "d1"); err != nil || ok {
			t.Errorf("DeleteDevice of another user's device = %v, %v", ok, err)
		}
		if ok, err := q.DeleteDevice(alex.ID, "d1"); err != nil || !ok {
			t.Errorf("DeleteDevice = %v, %v", ok, err)
		}
		ids, err := q.DeleteDevices(alex.ID)
		if err != nil || !slices.Equal(ids, []DeviceID{"d2", "d3"}) {
			t.Errorf("DeleteDevices = %v, %v", ids, err)
		}
		if list, err := q.ListDevices(alex.ID); err != nil || len(list) != 0 {
			t.Errorf("devices left: %v, %v", list, err)
		}
		return q.DeleteDeviceCodesOf(alex.ID)
	})
	if n := queryInt(t, db, "SELECT count(*) FROM device_codes"); n != 2 {
		t.Errorf("device codes left = %d, want 2 (Sam's and the undecided one)", n)
	}
	if n := queryInt(t, db, "SELECT count(*) FROM devices WHERE id = 'd4'"); n != 1 {
		t.Error("Sam's device was deleted")
	}
}

func TestSetupTokens(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	t0 := e.clock.Now()
	exp := t0.Add(24 * time.Hour)
	valid := func(h string, at time.Time) bool {
		var ok bool
		mustRead(t, db, func(q *Q) error {
			var err error
			ok, err = q.SetupTokenValid([]byte(h), at)
			return err
		})
		return ok
	}
	mustWrite(t, db, func(q *Q) error { return q.ReplaceSetupToken([]byte("first"), t0, exp) })
	if !valid("first", exp) || valid("first", exp.Add(time.Millisecond)) || valid("other", t0) || valid("", t0) {
		t.Error("SetupTokenValid boundaries")
	}
	// Only the newest link works.
	mustWrite(t, db, func(q *Q) error { return q.ReplaceSetupToken([]byte("second"), t0, exp) })
	if valid("first", t0) || !valid("second", t0) {
		t.Error("ReplaceSetupToken kept the old token")
	}
	if n := queryInt(t, db, "SELECT count(*) FROM setup_tokens"); n != 1 {
		t.Errorf("setup tokens = %d", n)
	}
	mustWrite(t, db, func(q *Q) error { return q.DeleteSetupTokens() })
	if valid("second", t0) {
		t.Error("DeleteSetupTokens kept a token")
	}
	if err := db.Write(context.Background(), func(q *Q) error { return q.ReplaceSetupToken(nil, t0, exp) }); err == nil {
		t.Error("ReplaceSetupToken without a hash succeeded")
	}
}

func TestPasswordResets(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	t0 := e.clock.Now()
	alex := e.newUser(db, "Alex", func(u *User) { u.Role = RoleAdmin })
	sam := e.newUser(db, "Sam")
	exp := t0.Add(24 * time.Hour)
	first := PasswordReset{TokenHash: []byte("r1"), UserID: sam.ID, CreatedBy: alex.ID, ExpiresAt: exp}
	second := PasswordReset{TokenHash: []byte("r2"), UserID: sam.ID, CreatedAt: t0.Add(time.Minute), ExpiresAt: exp}
	mustWrite(t, db, func(q *Q) error {
		if err := q.ReplacePasswordReset(first); err != nil {
			return err
		}
		got, err := q.PasswordResetByHash([]byte("r1"), exp)
		if err != nil {
			return err
		}
		want := first
		want.CreatedAt = t0
		if !reflect.DeepEqual(got, want) {
			t.Errorf("PasswordResetByHash = %+v, want %+v", got, want)
		}
		if _, err := q.PasswordResetByHash([]byte("r1"), exp.Add(time.Millisecond)); !errors.Is(err, ErrNotFound) {
			t.Errorf("1 ms past expiry: %v", err)
		}
		// One live link per user: the new one replaces the old one.
		if err := q.ReplacePasswordReset(second); err != nil {
			return err
		}
		if _, err := q.PasswordResetByHash([]byte("r1"), t0); !errors.Is(err, ErrNotFound) {
			t.Errorf("the replaced link still works: %v", err)
		}
		got, err = q.PasswordResetByHash([]byte("r2"), t0)
		if err != nil || got.CreatedBy != "" || !got.CreatedAt.Equal(t0.Add(time.Minute)) {
			t.Errorf("CLI reset: %+v, %v", got, err)
		}
		if err := q.DeletePasswordReset(sam.ID); err != nil {
			return err
		}
		if err := q.DeletePasswordReset(sam.ID); err != nil {
			t.Errorf("deleting a missing link: %v", err)
		}
		if _, err := q.PasswordResetByHash([]byte("r2"), t0); !errors.Is(err, ErrNotFound) {
			t.Errorf("after DeletePasswordReset: %v", err)
		}
		if _, err := q.PasswordResetByHash(nil, t0); !errors.Is(err, ErrNotFound) {
			t.Errorf("empty hash: %v", err)
		}
		if err := q.ReplacePasswordReset(PasswordReset{UserID: sam.ID, ExpiresAt: exp}); err == nil {
			t.Error("ReplacePasswordReset without a hash succeeded")
		}
		return nil
	})
}

func TestTransferMonths(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	now := e.clock.Now()
	mustWrite(t, db, func(q *Q) error {
		eg, in, err := q.TransferMonth("2026-10")
		if err != nil || eg != 0 || in != 0 {
			t.Errorf("TransferMonth(missing) = %d, %d, %v", eg, in, err)
		}
		for range 3 {
			if err := q.AddTransfer("2026-10", 1<<40, 5, now); err != nil {
				return err
			}
		}
		if err := q.AddTransfer("2026-11", 7, 8, now); err != nil {
			return err
		}
		eg, in, err = q.TransferMonth("2026-10")
		if err != nil || eg != 3<<40 || in != 15 {
			t.Errorf("TransferMonth = %d, %d, %v", eg, in, err)
		}
		if err := q.AddTransfer("", 1, 1, now); err == nil {
			t.Error("AddTransfer without a month succeeded")
		}
		return nil
	})
}

// TestReadRejectsEveryWrite: every write method fails inside Read and changes nothing (03 §15).
func TestReadRejectsEveryWrite(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	now := e.clock.Now()
	alex := e.newUser(db, "Alex")
	s := e.newSession(db, alex.ID, "tok", now)
	mustRead(t, db, func(q *Q) error {
		u := User{Username: "Sam", UsernameKey: "sam", CreatedVia: "invite"}
		inv := Invite{TokenHash: []byte("i"), ExpiresAt: now.Add(time.Hour), MaxUses: 1}
		calls := map[string]func() error{
			"CreateUser":      func() error { return q.CreateUser(&u) },
			"RenameUser":      func() error { return q.RenameUser(alex.ID, "A", "a", now) },
			"SetRole":         func() error { return q.SetRole(alex.ID, RoleAdmin, now) },
			"SetStatus":       func() error { return q.SetStatus(alex.ID, StatusDisabled, now) },
			"Approve":         func() error { return q.Approve(alex.ID, alex.ID, now) },
			"SetPasswordHash": func() error { return q.SetPasswordHash(alex.ID, "", now) },
			"TouchLogin":      func() error { return q.TouchLogin(alex.ID, now) },
			"DeleteUser":      func() error { return q.DeleteUser(alex.ID) },
			"DeletePending":   func() error { _, err := q.DeletePending(); return err },
			"CreateSession": func() error {
				return q.CreateSession(&Session{UserID: alex.ID, TokenHash: []byte("x"), IdleExpiresAt: now,
					ExpiresAt: now})
			},
			"RotateSession":       func() error { return q.RotateSession(s.ID, []byte("y"), now, now) },
			"TouchSession":        func() error { return q.TouchSession(s.ID, "", now, now) },
			"DeleteSession":       func() error { _, err := q.DeleteSession(alex.ID, s.ID); return err },
			"DeleteSessions":      func() error { _, err := q.DeleteSessions(alex.ID, ""); return err },
			"TrimSessions":        func() error { _, err := q.TrimSessions(alex.ID, 0); return err },
			"DeleteDevice":        func() error { _, err := q.DeleteDevice(alex.ID, "d"); return err },
			"DeleteDevices":       func() error { _, err := q.DeleteDevices(alex.ID); return err },
			"DeleteDeviceCodesOf": func() error { return q.DeleteDeviceCodesOf(alex.ID) },
			"ReplaceSetupToken":   func() error { return q.ReplaceSetupToken([]byte("s"), now, now) },
			"DeleteSetupTokens":   func() error { return q.DeleteSetupTokens() },
			"CreateInvite":        func() error { return q.CreateInvite(&inv) },
			"RevokeInvite":        func() error { return q.RevokeInvite("i", alex.ID, now) },
			"UseInvite":           func() error { return q.UseInvite("i", now) },
			"ReplacePasswordReset": func() error {
				return q.ReplacePasswordReset(PasswordReset{TokenHash: []byte("r"), UserID: alex.ID, ExpiresAt: now})
			},
			"DeletePasswordReset": func() error { return q.DeletePasswordReset(alex.ID) },
			"CreateRoom":          func() error { return q.CreateRoom(&Room{Name: "G", NameKey: "g"}) },
			"RenameRoom":          func() error { return q.RenameRoom(DefaultRoomID, "L", "l", now) },
			"DeleteRoom":          func() error { return q.DeleteRoom("r") },
			"UpsertPushSubscription": func() error {
				_, err := q.UpsertPushSubscription(&PushSubscription{UserID: alex.ID, SessionID: s.ID, Endpoint: "https://p/1"})
				return err
			},
			"DeletePushSubscription":           func() error { _, err := q.DeletePushSubscription(alex.ID, "p"); return err },
			"DeletePushSubscriptionByEndpoint": func() error { _, err := q.DeletePushSubscriptionByEndpoint(alex.ID, "e"); return err },
			"DeletePushSubscriptionByID":       func() error { return q.DeletePushSubscriptionByID("p") },
			"DeleteAllPushSubscriptions":       func() error { _, err := q.DeleteAllPushSubscriptions(); return err },
			"PrunePushSubscriptions":           func() error { _, err := q.PrunePushSubscriptions(1, now); return err },
			"RecordPushResult":                 func() error { return q.RecordPushResult("p", true, now) },
			"TrimPushSubscriptions":            func() error { return q.TrimPushSubscriptions(alex.ID, 0) },
			"SetPushPreferences":               func() error { return q.SetPushPreferences(alex.ID, PushPreferences{ShareStarted: "off"}, now) },
			"AddTransfer":                      func() error { return q.AddTransfer("2026-10", 1, 1, now) },
			"AppendAudit":                      func() error { return q.AppendAudit(AuditEntry{Action: "x", Actor: CLIActor}) },
			"SetMeta":                          func() error { return q.SetMeta("k", "v") },
		}
		for name, call := range calls {
			if err := call(); !errors.Is(err, errReadOnlyTx) {
				t.Errorf("%s inside Read = %v, want errReadOnlyTx", name, err)
			}
		}
		return nil
	})
	for query, want := range map[string]int{
		"SELECT count(*) FROM users": 1,
		"SELECT count(*) FROM users WHERE username = 'Alex' AND role = 'user' AND status = 'active'": 1,
		"SELECT count(*) FROM sessions WHERE token_hash = x'746f6b'":                                 1,
		"SELECT count(*) FROM audit_log":                                                             0,
		"SELECT count(*) FROM rooms":                                                                 1,
	} {
		if n := queryInt(t, db, query); n != want {
			t.Errorf("after writes inside Read: %s = %d, want %d", query, n, want)
		}
	}
}
