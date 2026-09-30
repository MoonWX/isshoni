package store

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestInviteCRUD(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	t0 := e.clock.Now()
	alex := e.newUser(db, "Alex", func(u *User) { u.Role = RoleAdmin })
	inv := Invite{TokenHash: []byte("tok"), Note: "for Sam", CreatedBy: &UserRef{ID: alex.ID, Username: "Alex"},
		ExpiresAt: t0.Add(7 * 24 * time.Hour), MaxUses: 10}
	mustWrite(t, db, func(q *Q) error { return q.CreateInvite(&inv) })
	if len(inv.ID) != idLen || !inv.CreatedAt.Equal(t0) {
		t.Errorf("created invite: %+v", inv)
	}
	mustRead(t, db, func(q *Q) error {
		for name, get := range map[string]func() (Invite, error){
			"InviteByID":        func() (Invite, error) { return q.InviteByID(inv.ID) },
			"InviteByTokenHash": func() (Invite, error) { return q.InviteByTokenHash([]byte("tok")) },
		} {
			got, err := get()
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(got, inv) {
				t.Errorf("%s:\n got %+v\nwant %+v", name, got, inv)
			}
		}
		if _, err := q.InviteByID("nope"); !errors.Is(err, ErrNotFound) {
			t.Errorf("InviteByID(unknown) = %v", err)
		}
		if _, err := q.InviteByTokenHash([]byte("nope")); !errors.Is(err, ErrNotFound) {
			t.Errorf("InviteByTokenHash(unknown) = %v", err)
		}
		if _, err := q.InviteByTokenHash(nil); !errors.Is(err, ErrNotFound) {
			t.Errorf("InviteByTokenHash(nil) = %v", err)
		}
		return nil
	})

	// A CLI invite has no creator; required fields and the max_uses CHECK are enforced.
	cli := Invite{TokenHash: []byte("cli"), ExpiresAt: t0.Add(time.Hour), MaxUses: 1, CreatedBy: &UserRef{}}
	mustWrite(t, db, func(q *Q) error { return q.CreateInvite(&cli) })
	mustRead(t, db, func(q *Q) error {
		got, err := q.InviteByID(cli.ID)
		if err == nil && got.CreatedBy != nil {
			t.Errorf("CLI invite creator = %+v", got.CreatedBy)
		}
		return err
	})
	for name, bad := range map[string]Invite{
		"no hash":       {ExpiresAt: t0, MaxUses: 1},
		"no expiry":     {TokenHash: []byte("x"), MaxUses: 1},
		"max_uses 0":    {TokenHash: []byte("x"), ExpiresAt: t0, MaxUses: 0},
		"max_uses 1001": {TokenHash: []byte("x"), ExpiresAt: t0, MaxUses: 1001},
		"same hash":     {TokenHash: []byte("tok"), ExpiresAt: t0, MaxUses: 1},
	} {
		if err := db.Write(context.Background(), func(q *Q) error { return q.CreateInvite(&bad) }); err == nil {
			t.Errorf("CreateInvite(%s) succeeded", name)
		}
	}

	// Revoke: idempotent, keeps the first revocation; unknown → ErrNotFound.
	t1 := t0.Add(time.Hour)
	mustWrite(t, db, func(q *Q) error {
		if err := q.RevokeInvite(inv.ID, alex.ID, t1); err != nil {
			return err
		}
		if err := q.RevokeInvite(inv.ID, "", t1.Add(time.Hour)); err != nil {
			t.Errorf("revoking twice: %v", err)
		}
		if err := q.RevokeInvite("nope", alex.ID, t1); !errors.Is(err, ErrNotFound) {
			t.Errorf("RevokeInvite(unknown) = %v", err)
		}
		got, err := q.InviteByID(inv.ID)
		if err != nil {
			return err
		}
		if !got.RevokedAt.Equal(t1) || got.RevokedBy != alex.ID || got.State(t1) != "revoked" {
			t.Errorf("revoked invite: %+v", got)
		}
		if err := q.UseInvite(inv.ID, t1); !errors.Is(err, ErrInviteUnusable) {
			t.Errorf("UseInvite(revoked) = %v", err)
		}
		return nil
	})
}

func TestUseInvite(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	t0 := e.clock.Now()
	exp := t0.Add(time.Hour)
	inv := Invite{TokenHash: []byte("tok"), ExpiresAt: exp, MaxUses: 2}
	mustWrite(t, db, func(q *Q) error { return q.CreateInvite(&inv) })
	mustWrite(t, db, func(q *Q) error {
		if err := q.UseInvite(inv.ID, exp.Add(-time.Millisecond)); err != nil {
			t.Errorf("UseInvite 1 ms before expiry: %v", err)
		}
		// expires_at > now: at exactly expires_at the invite is expired, as Invite.State says.
		if err := q.UseInvite(inv.ID, exp); !errors.Is(err, ErrInviteUnusable) {
			t.Errorf("UseInvite at expires_at = %v", err)
		}
		if err := q.UseInvite(inv.ID, t0); err != nil {
			t.Errorf("second use: %v", err)
		}
		if err := q.UseInvite(inv.ID, t0); !errors.Is(err, ErrInviteUnusable) {
			t.Errorf("UseInvite(used up) = %v", err)
		}
		if err := q.UseInvite("nope", t0); !errors.Is(err, ErrInviteUnusable) {
			t.Errorf("UseInvite(unknown) = %v", err)
		}
		got, err := q.InviteByID(inv.ID)
		if err != nil {
			return err
		}
		if got.Uses != 2 || got.State(t0) != "used_up" {
			t.Errorf("after two uses: %+v", got)
		}
		return nil
	})
}

// TestUseInviteConcurrent: 25 goroutines redeem a 10-use invite (UseInvite and CreateUser in one Write), and exactly
// 10 succeed (03 §15).
func TestUseInviteConcurrent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	now := e.clock.Now()
	inv := Invite{TokenHash: []byte("tok"), ExpiresAt: now.Add(time.Hour), MaxUses: 10}
	mustWrite(t, db, func(q *Q) error { return q.CreateInvite(&inv) })
	var ok, unusable atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 25 {
		wg.Go(func() {
			<-start
			name := "friend" + strconv.Itoa(i)
			err := db.Write(context.Background(), func(q *Q) error {
				if err := q.UseInvite(inv.ID, now); err != nil {
					return err
				}
				return q.CreateUser(&User{Username: name, UsernameKey: name, CreatedVia: "invite", InviteID: inv.ID})
			})
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ErrInviteUnusable):
				unusable.Add(1)
			default:
				t.Errorf("%s: %v", name, err)
			}
		})
	}
	close(start)
	wg.Wait()
	if ok.Load() != 10 || unusable.Load() != 15 {
		t.Errorf("succeeded %d, unusable %d, want 10 and 15", ok.Load(), unusable.Load())
	}
	if n := queryInt(t, db, "SELECT uses FROM invites WHERE id = ?", string(inv.ID)); n != 10 {
		t.Errorf("uses = %d", n)
	}
	if n := queryInt(t, db, "SELECT count(*) FROM users WHERE invite_id = ?", string(inv.ID)); n != 10 {
		t.Errorf("users created with the invite = %d", n)
	}
}

// TestUseInviteRollsBackWithCreateUser: a taken username rolls the use back (the Write fails as a whole).
func TestUseInviteRollsBackWithCreateUser(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	now := e.clock.Now()
	e.newUser(db, "Sam")
	inv := Invite{TokenHash: []byte("tok"), ExpiresAt: now.Add(time.Hour), MaxUses: 1}
	mustWrite(t, db, func(q *Q) error { return q.CreateInvite(&inv) })
	err := db.Write(context.Background(), func(q *Q) error {
		if err := q.UseInvite(inv.ID, now); err != nil {
			return err
		}
		return q.CreateUser(&User{Username: "SAM", UsernameKey: "sam", CreatedVia: "invite", InviteID: inv.ID})
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("register with a taken name = %v", err)
	}
	if n := queryInt(t, db, "SELECT uses FROM invites WHERE id = ?", string(inv.ID)); n != 0 {
		t.Errorf("uses = %d after a failed registration, want 0", n)
	}
}

func TestListInvites(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	t0 := e.clock.Now()
	alex := e.newUser(db, "Alex", func(u *User) { u.Role = RoleAdmin })
	sam := e.newUser(db, "Sam")
	mk := func(h string, by *User, created time.Time, exp time.Duration, maxUses int) Invite {
		inv := Invite{TokenHash: []byte(h), CreatedAt: created, ExpiresAt: t0.Add(exp), MaxUses: maxUses}
		if by != nil {
			inv.CreatedBy = &UserRef{ID: by.ID, Username: by.Username}
		}
		mustWrite(t, db, func(q *Q) error { return q.CreateInvite(&inv) })
		return inv
	}
	active := mk("a", &alex, t0.Add(-4*time.Hour), time.Hour, 5)
	expired := mk("b", &alex, t0.Add(-3*time.Hour), 0, 5) // expires exactly now: expired
	usedUp := mk("c", &sam, t0.Add(-2*time.Hour), time.Hour, 1)
	revoked := mk("d", nil, t0.Add(-time.Hour), time.Hour, 5)
	samActive := mk("e", &sam, t0.Add(-30*time.Minute), time.Hour, 5)
	redeem := func(inv Invite, name string, at time.Time) {
		e.newUser(db, name, func(u *User) { u.InviteID, u.CreatedAt = inv.ID, at })
	}
	mustWrite(t, db, func(q *Q) error {
		if err := q.UseInvite(usedUp.ID, t0); err != nil {
			return err
		}
		return q.RevokeInvite(revoked.ID, "", t0)
	})
	redeem(active, "Zoe", t0.Add(time.Minute))
	redeem(active, "Bob", t0.Add(2*time.Minute))
	redeem(usedUp, "Kim", t0)

	list := func(by UserID, inactive bool) []Invite {
		var out []Invite
		mustRead(t, db, func(q *Q) error {
			var err error
			out, err = q.ListInvites(by, inactive)
			return err
		})
		return out
	}
	ids := func(list []Invite) []InviteID {
		var out []InviteID
		for _, inv := range list {
			out = append(out, inv.ID)
		}
		return out
	}
	if got, want := ids(list("", true)), []InviteID{samActive.ID, revoked.ID, usedUp.ID, expired.ID, active.ID}; !slices.Equal(got, want) {
		t.Errorf("ListInvites(all, inactive) = %v, want newest first %v", got, want)
	}
	all := list("", false)
	if got, want := ids(all), []InviteID{samActive.ID, active.ID}; !slices.Equal(got, want) {
		t.Errorf("ListInvites(all, active only) = %v, want %v", got, want)
	}
	if got := all[1].RedeemedBy; !slices.Equal(got, []UserRef{{ID: got[0].ID, Username: "Zoe"}, {ID: got[1].ID, Username: "Bob"}}) {
		t.Errorf("RedeemedBy = %+v, want Zoe then Bob (sign-up order)", got)
	}
	if got := all[0].RedeemedBy; got == nil || len(got) != 0 {
		t.Errorf("RedeemedBy of an unused invite = %#v, want an empty slice", got)
	}
	if got, want := ids(list(sam.ID, true)), []InviteID{samActive.ID, usedUp.ID}; !slices.Equal(got, want) {
		t.Errorf("ListInvites(sam, inactive) = %v, want %v", got, want)
	}
	samAll := list(sam.ID, true)
	if len(samAll[1].RedeemedBy) != 1 || samAll[1].RedeemedBy[0].Username != "Kim" {
		t.Errorf("used-up invite's redeemers = %+v", samAll[1].RedeemedBy)
	}
	if got := ids(list(sam.ID, false)); !slices.Equal(got, []InviteID{samActive.ID}) {
		t.Errorf("ListInvites(sam, active only) = %v", got)
	}
	// The active filter uses the store clock: once the clock passes the expiry, nothing is active.
	e.clock.Advance(time.Hour)
	if got := list("", false); len(got) != 0 {
		t.Errorf("active invites after the clock passed every expiry: %v", ids(got))
	}

	mustRead(t, db, func(q *Q) error {
		for _, c := range []struct {
			by   UserID
			at   time.Time
			want int
		}{
			{"", t0, 2},
			{sam.ID, t0, 1},
			{alex.ID, t0, 1},
			{"", t0.Add(time.Hour - time.Millisecond), 2},
			{"", t0.Add(time.Hour), 0},
		} {
			n, err := q.CountActiveInvites(c.by, c.at)
			if err != nil {
				return err
			}
			if n != c.want {
				t.Errorf("CountActiveInvites(%q, %v) = %d, want %d", c.by, c.at.Sub(t0), n, c.want)
			}
		}
		return nil
	})
}
