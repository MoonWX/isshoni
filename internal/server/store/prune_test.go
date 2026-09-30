package store

import (
	"context"
	"testing"
	"time"
)

// TestPruneBoundaries checks each rule of 03 §4.7 at its limit t: at t−1ms and at t the row stays, at t+1ms it is
// deleted (03 §15).
func TestPruneBoundaries(t *testing.T) {
	t.Parallel()
	const day = 24 * time.Hour
	type rule struct {
		name  string
		setup func(e *testEnv, db *DB) (limit time.Time)
		count func(PruneStats) int
		left  string // counts the rows the rule targets
	}
	rules := []rule{
		{
			name: "sessions: idle_expires_at < now",
			setup: func(e *testEnv, db *DB) time.Time {
				u := e.newUser(db, "Alex")
				s := e.newSession(db, u.ID, "tok", e.clock.Now())
				return s.IdleExpiresAt
			},
			count: func(s PruneStats) int { return s.Sessions },
			left:  "SELECT count(*) FROM sessions",
		},
		{
			name: "setup tokens: expires_at < now",
			setup: func(e *testEnv, db *DB) time.Time {
				exp := e.clock.Now().Add(day)
				mustWrite(e.t, db, func(q *Q) error { return q.ReplaceSetupToken([]byte("s"), e.clock.Now(), exp) })
				return exp
			},
			count: func(s PruneStats) int { return s.Tokens },
			left:  "SELECT count(*) FROM setup_tokens",
		},
		{
			name: "password resets: expires_at < now",
			setup: func(e *testEnv, db *DB) time.Time {
				u := e.newUser(db, "Sam")
				exp := e.clock.Now().Add(day)
				mustWrite(e.t, db, func(q *Q) error {
					return q.ReplacePasswordReset(PasswordReset{TokenHash: []byte("r"), UserID: u.ID, ExpiresAt: exp})
				})
				return exp
			},
			count: func(s PruneStats) int { return s.Tokens },
			left:  "SELECT count(*) FROM password_resets",
		},
		{
			name: "invites: expired more than 30 days ago",
			setup: func(e *testEnv, db *DB) time.Time {
				exp := e.clock.Now().Add(7 * day)
				inv := Invite{TokenHash: []byte("i"), ExpiresAt: exp, MaxUses: 10}
				mustWrite(e.t, db, func(q *Q) error { return q.CreateInvite(&inv) })
				return exp.Add(30 * day)
			},
			count: func(s PruneStats) int { return s.Invites },
			left:  "SELECT count(*) FROM invites",
		},
		{
			name: "invites: revoked more than 30 days ago",
			setup: func(e *testEnv, db *DB) time.Time {
				revoked := e.clock.Now().Add(time.Hour)
				inv := Invite{TokenHash: []byte("i"), ExpiresAt: revoked.Add(365 * day), MaxUses: 10}
				mustWrite(e.t, db, func(q *Q) error {
					if err := q.CreateInvite(&inv); err != nil {
						return err
					}
					return q.RevokeInvite(inv.ID, "", revoked)
				})
				return revoked.Add(30 * day)
			},
			count: func(s PruneStats) int { return s.Invites },
			left:  "SELECT count(*) FROM invites",
		},
		{
			name: "invites: used up more than 30 days ago (the last redeemer's sign-up)",
			setup: func(e *testEnv, db *DB) time.Time {
				now := e.clock.Now()
				inv := Invite{TokenHash: []byte("i"), ExpiresAt: now.Add(365 * day), MaxUses: 2}
				mustWrite(e.t, db, func(q *Q) error { return q.CreateInvite(&inv) })
				usedUp := now.Add(3 * day)
				for i, at := range []time.Time{now.Add(day), usedUp} {
					mustWrite(e.t, db, func(q *Q) error {
						if err := q.UseInvite(inv.ID, at); err != nil {
							return err
						}
						name := "friend" + itoa(i)
						return q.CreateUser(&User{Username: name, UsernameKey: name, CreatedVia: "invite",
							InviteID: inv.ID, CreatedAt: at})
					})
				}
				return usedUp.Add(30 * day)
			},
			count: func(s PruneStats) int { return s.Invites },
			left:  "SELECT count(*) FROM invites",
		},
		{
			name: "invites: used up, redeemers deleted (the invite's created_at)",
			setup: func(e *testEnv, db *DB) time.Time {
				now := e.clock.Now()
				inv := Invite{TokenHash: []byte("i"), ExpiresAt: now.Add(365 * day), MaxUses: 1, Uses: 1}
				mustWrite(e.t, db, func(q *Q) error { return q.CreateInvite(&inv) })
				return now.Add(30 * day)
			},
			count: func(s PruneStats) int { return s.Invites },
			left:  "SELECT count(*) FROM invites",
		},
		{
			name: "pending sign-ups: created more than 14 days ago",
			setup: func(e *testEnv, db *DB) time.Time {
				u := e.newUser(db, "Kim", func(u *User) { u.Status, u.CreatedVia = StatusPending, "signup" })
				return u.CreatedAt.Add(14 * day)
			},
			count: func(s PruneStats) int { return s.Pending },
			left:  "SELECT count(*) FROM users WHERE status = 'pending'",
		},
		{
			name: "device codes: expired more than 1 h ago",
			setup: func(e *testEnv, db *DB) time.Time {
				exp := e.clock.Now().Add(10 * time.Minute)
				mustExecW(e.t, db, `INSERT INTO device_codes (device_code_hash, user_code_hash, client_kind,
					device_name, os, app_version, request_ip, created_at, expires_at)
					VALUES (x'01', x'02', 'desktop', 'n', 'linux', 'v', '', 0, ?)`, exp.UnixMilli())
				return exp.Add(time.Hour)
			},
			count: func(s PruneStats) int { return s.DeviceCodes },
			left:  "SELECT count(*) FROM device_codes",
		},
		{
			name: "device tokens: expires_at < now",
			setup: func(e *testEnv, db *DB) time.Time {
				u := e.newUser(db, "Alex")
				newDevice(e.t, db, "dev", u.ID, e.clock.Now())
				exp := e.clock.Now().Add(15 * time.Minute)
				mustExecW(e.t, db, `INSERT INTO device_tokens (token_hash, device_id, kind, created_at, expires_at,
					spent_at) VALUES (x'01', 'dev', 'refresh', 0, ?, 0)`, exp.UnixMilli())
				return exp
			},
			count: func(s PruneStats) int { return s.DeviceTokens },
			left:  "SELECT count(*) FROM device_tokens",
		},
		{
			name: "audit log: older than 30 days",
			setup: func(e *testEnv, db *DB) time.Time {
				at := e.clock.Now()
				mustWrite(e.t, db, func(q *Q) error {
					return q.AppendAudit(AuditEntry{At: at, Action: "auth.login", Actor: CLIActor})
				})
				return at.Add(30 * day)
			},
			count: func(s PruneStats) int { return s.Audit },
			left:  "SELECT count(*) FROM audit_log WHERE action = 'auth.login'",
		},
	}
	for _, r := range rules {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			db := e.open(nil)
			limit := r.setup(e, db)
			ctx := context.Background()
			for _, at := range []time.Time{limit.Add(-time.Millisecond), limit} {
				st, err := db.Prune(ctx, at)
				if err != nil {
					t.Fatal(err)
				}
				if st != (PruneStats{}) {
					t.Errorf("Prune(limit%+v) = %+v, want nothing deleted", at.Sub(limit), st)
				}
				if n := queryInt(t, db, r.left); n != 1 {
					t.Fatalf("Prune(limit%+v) left %d rows, want 1", at.Sub(limit), n)
				}
			}
			st, err := db.Prune(ctx, limit.Add(time.Millisecond))
			if err != nil {
				t.Fatal(err)
			}
			if r.count(st) != 1 {
				t.Errorf("Prune(limit+1ms) = %+v, want 1 row of this rule", st)
			}
			if n := queryInt(t, db, r.left); n != 0 {
				t.Errorf("Prune(limit+1ms) left %d rows", n)
			}
		})
	}
}

// TestPruneLeavesLiveRows: live rows of every table survive a prune long after they were created, and an expired
// pending sign-up leaves a user.signup_expired audit row.
func TestPruneLeavesLiveRows(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	t0 := e.clock.Now()
	later := t0.Add(100 * 24 * time.Hour)
	alex := e.newUser(db, "Alex", func(u *User) { u.Role = RoleAdmin })
	kim := e.newUser(db, "Kim", func(u *User) { u.Status, u.CreatedVia = StatusPending, "signup" })
	e.newSession(db, alex.ID, "live", later, func(s *Session) { s.LastSeenAt = later })
	inv := Invite{TokenHash: []byte("i"), CreatedAt: t0, ExpiresAt: later.Add(time.Hour), MaxUses: 5}
	mustWrite(t, db, func(q *Q) error {
		if err := q.CreateInvite(&inv); err != nil {
			return err
		}
		return q.UseInvite(inv.ID, t0)
	})
	st, err := db.Prune(context.Background(), later)
	if err != nil {
		t.Fatal(err)
	}
	if st != (PruneStats{Pending: 1}) {
		t.Errorf("Prune = %+v, want only the pending sign-up", st)
	}
	for query, want := range map[string]int{
		"SELECT count(*) FROM users":    1,
		"SELECT count(*) FROM sessions": 1,
		"SELECT count(*) FROM invites":  1,
		"SELECT count(*) FROM rooms":    1,
	} {
		if n := queryInt(t, db, query); n != want {
			t.Errorf("%s = %d, want %d", query, n, want)
		}
	}
	mustRead(t, db, func(q *Q) error {
		rows, err := q.ListAudit(AuditQuery{ActionPrefix: "user.signup_expired"})
		if err != nil {
			return err
		}
		if len(rows) != 1 {
			t.Fatalf("signup_expired rows = %+v", rows)
		}
		r := rows[0]
		if !r.At.Equal(later) || r.Actor != SystemActor || r.TargetKind != "user" || r.TargetID != string(kim.ID) ||
			r.TargetName != "Kim" || r.Outcome != "ok" {
			t.Errorf("signup_expired row = %+v", r)
		}
		return nil
	})
}

// TestPruneUpkeep: PRAGMA optimize runs at most once per 24 h of Prune's now, counted from Open's own run.
func TestPruneUpkeep(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	t0 := e.clock.Now()
	ctx := context.Background()
	optimized := func() time.Time {
		db.maintMu.Lock()
		defer db.maintMu.Unlock()
		return db.optimizedAt
	}
	if !optimized().Equal(t0) {
		t.Fatalf("optimizedAt after Open = %v, want the open time", optimized())
	}
	mustExecW(t, db, "INSERT INTO meta (key, value) VALUES ('x', 'y')")
	for _, c := range []struct {
		at   time.Duration
		want time.Duration
	}{
		{time.Hour, 0},
		{24*time.Hour - time.Millisecond, 0},
		{24 * time.Hour, 24 * time.Hour},
		{47 * time.Hour, 24 * time.Hour},
		{48 * time.Hour, 48 * time.Hour},
	} {
		if _, err := db.Prune(ctx, t0.Add(c.at)); err != nil {
			t.Fatal(err)
		}
		if got := optimized(); !got.Equal(t0.Add(c.want)) {
			t.Errorf("after Prune at +%v: optimized at +%v, want +%v", c.at, got.Sub(t0), c.want)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Prune(ctx, t0); err == nil {
		t.Error("Prune after Close succeeded")
	}
}
