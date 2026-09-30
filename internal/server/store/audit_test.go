package store

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAppendAuditRoundTrip(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	t0 := e.clock.Now()
	alex := e.newUser(db, "Alex", func(u *User) { u.Role = RoleAdmin })
	full := AuditEntry{
		At: t0.Add(-time.Minute), Action: "user.role_changed", Outcome: "ok",
		Actor:      Actor{Kind: ActorUser, UserID: alex.ID, Name: "Alex", IP: "203.0.113.7"},
		TargetKind: "user", TargetID: "b8f2n4r6t0vz", TargetName: "Sam",
		Detail: map[string]any{"from": "user", "to": "admin", "count": 12345678901234},
	}
	mustWrite(t, db, func(q *Q) error {
		if err := q.AppendAudit(full); err != nil {
			return err
		}
		// Defaults: At from the store clock, outcome ok, detail {}.
		return q.AppendAudit(AuditEntry{Action: "setup.token_issued", Actor: CLIActor})
	})
	var rows []AuditEntry
	mustRead(t, db, func(q *Q) error {
		var err error
		rows, err = q.ListAudit(AuditQuery{})
		return err
	})
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	want := full
	want.ID = 1
	want.Detail = map[string]any{"from": "user", "to": "admin", "count": json.Number("12345678901234")}
	if !reflect.DeepEqual(rows[1], want) {
		t.Errorf("round trip:\n got %+v\nwant %+v", rows[1], want)
	}
	cli := rows[0]
	if cli.ID != 2 || !cli.At.Equal(t0) || cli.Outcome != "ok" || cli.Actor != CLIActor || cli.TargetKind != "" ||
		cli.Detail == nil || len(cli.Detail) != 0 {
		t.Errorf("defaults: %+v", cli)
	}
	if n := queryInt(t, db, "SELECT count(*) FROM audit_log WHERE detail = '{}' AND actor_id IS NULL"); n != 1 {
		t.Error("an empty detail is not '{}' or the CLI actor_id is not NULL")
	}
	err := db.Write(context.Background(), func(q *Q) error { return q.AppendAudit(AuditEntry{Actor: CLIActor}) })
	if err == nil {
		t.Error("AppendAudit without an action succeeded")
	}
	err = db.Write(context.Background(), func(q *Q) error {
		return q.AppendAudit(AuditEntry{Action: "x", Actor: Actor{Kind: "robot"}})
	})
	if err == nil {
		t.Error("AppendAudit with a bad actor kind succeeded")
	}
}

func TestAuditDetailTruncation(t *testing.T) {
	t.Parallel()
	big := map[string]any{
		"a":    "small",
		"b":    strings.Repeat("x", 2000),
		"c":    42,
		"name": "Games",
	}
	s, err := encodeDetail(big)
	if err != nil {
		t.Fatal(err)
	}
	if len(s) > maxAuditDetail {
		t.Fatalf("encoded detail is %d bytes", len(s))
	}
	got, err := decodeDetail(s)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"a": "small", "c": json.Number("42"), "name": "Games", "truncated": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("truncated detail = %v, want %v", got, want)
	}
	// Many small keys: the ones that fit are kept, in key order.
	many := map[string]any{}
	for i := range 200 {
		many["k"+itoa(1000+i)] = "0123456789"
	}
	s, err = encodeDetail(many)
	if err != nil {
		t.Fatal(err)
	}
	got, err = decodeDetail(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(s) > maxAuditDetail || got["truncated"] != true || got["k1000"] != "0123456789" || got["k1199"] != nil {
		t.Errorf("%d bytes, %d keys: %s", len(s), len(got), s)
	}
	if s, err := encodeDetail(map[string]any{"a": 1}); err != nil || s != `{"a":1}` {
		t.Errorf("small detail = %s, %v", s, err)
	}
	if _, err := encodeDetail(map[string]any{"f": func() {}}); err == nil {
		t.Error("an unencodable detail was accepted")
	}
}

func TestListAudit(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	t0 := e.clock.Now()
	actions := []struct {
		action, actor, target string
	}{
		{"setup.token_issued", "", ""},
		{"user.approved", "alex", "sam"},
		{"auth.login", "sam", "ses1"},
		{"userx.fake", "alex", "sam"}, // "user." must not match
		{"user.role_changed", "alex", "kim"},
		{"auth.logout", "sam", "ses1"},
		{"user.renamed", "kim", "kim"},
	}
	mustWrite(t, db, func(q *Q) error {
		for i, a := range actions {
			actor := CLIActor
			if a.actor != "" {
				actor = Actor{Kind: ActorUser, UserID: UserID(a.actor), Name: a.actor}
			}
			err := q.AppendAudit(AuditEntry{At: t0.Add(time.Duration(i) * time.Second), Action: a.action,
				Actor: actor, TargetID: a.target})
			if err != nil {
				return err
			}
		}
		return nil
	})
	ids := func(f AuditQuery) []int64 {
		t.Helper()
		var out []int64
		mustRead(t, db, func(q *Q) error {
			rows, err := q.ListAudit(f)
			for _, r := range rows {
				out = append(out, r.ID)
			}
			return err
		})
		return out
	}
	cases := []struct {
		name string
		f    AuditQuery
		want []int64
	}{
		{"all", AuditQuery{}, []int64{7, 6, 5, 4, 3, 2, 1}},
		{"page 1", AuditQuery{Limit: 3}, []int64{7, 6, 5}},
		{"page 2", AuditQuery{Limit: 3, Before: 5}, []int64{4, 3, 2}},
		{"page 3", AuditQuery{Limit: 3, Before: 2}, []int64{1}},
		{"prefix", AuditQuery{ActionPrefix: "user."}, []int64{7, 5, 2}},
		{"prefix is not a pattern", AuditQuery{ActionPrefix: "user_"}, nil},
		{"prefix is case-sensitive", AuditQuery{ActionPrefix: "USER."}, nil},
		{"actor", AuditQuery{ActorID: "alex"}, []int64{5, 4, 2}},
		{"target", AuditQuery{TargetID: "ses1"}, []int64{6, 3}},
		{"combined", AuditQuery{ActionPrefix: "user.", ActorID: "alex", TargetID: "sam", Before: 5}, []int64{2}},
	}
	for _, c := range cases {
		if got := ids(c.f); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	// A page stays stable while rows are added and pruned.
	mustWrite(t, db, func(q *Q) error {
		if err := q.AppendAudit(AuditEntry{Action: "auth.login", Actor: CLIActor}); err != nil {
			return err
		}
		_, err := q.exec("DELETE FROM audit_log WHERE id = 1")
		return err
	})
	if got := ids(AuditQuery{Limit: 3, Before: 5}); !slices.Equal(got, []int64{4, 3, 2}) {
		t.Errorf("page 2 after changes: %v", got)
	}

	// The limit is clamped to 1..200, with 50 by default.
	mustWrite(t, db, func(q *Q) error {
		for range 250 {
			if err := q.AppendAudit(AuditEntry{Action: "auth.login", Actor: CLIActor}); err != nil {
				return err
			}
		}
		return nil
	})
	for limit, want := range map[int]int{0: 50, -1: 50, 1: 1, 200: 200, 201: 200, 10000: 200} {
		if got := len(ids(AuditQuery{Limit: limit})); got != want {
			t.Errorf("Limit %d: %d rows, want %d", limit, got, want)
		}
	}
}

func TestSecurityEvents(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	db := e.open(nil)
	t0 := e.clock.Now()
	since := t0.Add(-7 * 24 * time.Hour)
	rows := []AuditEntry{
		{At: since.Add(-time.Millisecond), Action: "user.role_changed"}, // too old
		{At: since, Action: "setup.completed"},
		{At: t0, Action: "auth.login"},
		{At: t0, Action: "auth.throttled", Outcome: "denied", Detail: map[string]any{"scope": "hash"}},
		{At: t0, Action: "settings.changed", Detail: map[string]any{"changes": map[string]any{
			"maxSharesPerRoom": map[string]any{"from": 0, "to": 4}}}},
		{At: t0, Action: "settings.changed", Detail: map[string]any{"changes": map[string]any{
			"registrationMode": map[string]any{"from": "invite", "to": "approval"}}}},
		{At: t0, Action: "user.disabled"},
		{At: t0, Action: "user.enabled"},
		{At: t0, Action: "user.deleted"},
		{At: t0, Action: "user.password_reset_issued"},
		{At: t0, Action: "secrets.rotated"},
		{At: t0, Action: "device.refresh_reused"},
		{At: t0, Action: "user.role_changed"},
	}
	mustWrite(t, db, func(q *Q) error {
		for _, r := range rows {
			r.Actor = SystemActor
			if err := q.AppendAudit(r); err != nil {
				return err
			}
		}
		return nil
	})
	get := func(limit int) []string {
		var out []string
		mustRead(t, db, func(q *Q) error {
			evs, err := q.SecurityEvents(since, limit)
			for _, ev := range evs {
				out = append(out, ev.Action)
			}
			return err
		})
		return out
	}
	want := []string{"user.role_changed", "device.refresh_reused", "secrets.rotated", "user.password_reset_issued",
		"user.deleted", "user.disabled", "settings.changed", "auth.throttled", "setup.completed"}
	if got := get(0); !slices.Equal(got, want) {
		t.Errorf("SecurityEvents =\n %v\nwant\n %v", got, want)
	}
	if got := get(2); !slices.Equal(got, want[:2]) {
		t.Errorf("SecurityEvents(limit 2) = %v", got)
	}
}
