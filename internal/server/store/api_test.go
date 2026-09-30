package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNewID(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	counts := map[rune]int{}
	for range 20000 {
		id := NewID()
		if len(id) != 12 {
			t.Fatalf("NewID() = %q: length %d", id, len(id))
		}
		for _, c := range id {
			if !strings.ContainsRune(idAlphabet, c) {
				t.Fatalf("NewID() = %q: %q is not lowercase Crockford base32", id, c)
			}
			counts[c]++
		}
		if seen[id] {
			t.Fatalf("NewID() repeated %q", id)
		}
		seen[id] = true
	}
	// 60 random bits: every character of the alphabet shows up.
	if len(counts) != 32 {
		t.Errorf("only %d distinct characters in 20000 IDs", len(counts))
	}
	for _, c := range "ilou" {
		if counts[c] != 0 {
			t.Errorf("ambiguous character %q in IDs", c)
		}
	}
	if len(string(DefaultRoomID)) != 6 || DefaultRoomID != "lounge" {
		t.Errorf("DefaultRoomID = %q", DefaultRoomID)
	}
}

func TestErrors(t *testing.T) {
	t.Parallel()
	ce := &ConflictError{Column: "username_key"}
	if !errors.Is(ce, ErrConflict) || ce.Error() != "store: username_key is already taken" {
		t.Errorf("ConflictError: %v", ce)
	}
	var target *ConflictError
	if !errors.As(error(ce), &target) || target.Column != "username_key" {
		t.Error("errors.As(*ConflictError)")
	}
	tooNew := &SchemaTooNewError{DBVersion: 5, BinaryVersion: 4, LastAppVersion: "0.6.1",
		Backup: "/var/lib/isshoni/backups/pre-4-20261014T021500Z.db"}
	want := "database schema 5 is newer than this isshoni build supports (4); it was last used by isshoni 0.6.1. " +
		"Install isshoni 0.6.1 or newer, or restore the database from before the upgrade: " +
		"/var/lib/isshoni/backups/pre-4-20261014T021500Z.db (changes made after that backup are lost)"
	if tooNew.Error() != want {
		t.Errorf("SchemaTooNewError:\n%s\nwant:\n%s", tooNew.Error(), want)
	}
	if !errors.Is(tooNew, ErrNeedsOperator) {
		t.Error("SchemaTooNewError does not wrap ErrNeedsOperator")
	}
	for _, e := range []error{ErrNotFound, ErrConflict, ErrInviteUnusable, ErrDefaultRoom, ErrNeedsOperator} {
		if !strings.HasPrefix(e.Error(), "store: ") {
			t.Errorf("%q has no store: prefix", e)
		}
		if errors.Is(e, ErrNeedsOperator) != (e == ErrNeedsOperator) { //nolint:errorlint // identity is the point
			t.Errorf("%v wraps ErrNeedsOperator", e)
		}
	}
}

func TestInviteState(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	base := Invite{ExpiresAt: now.Add(time.Millisecond), MaxUses: 10, Uses: 9}
	cases := []struct {
		name string
		mod  func(*Invite)
		want string
	}{
		{"active", func(*Invite) {}, "active"},
		{"expired at now", func(i *Invite) { i.ExpiresAt = now }, "expired"},
		{"used up", func(i *Invite) { i.Uses = 10 }, "used_up"},
		{"revoked", func(i *Invite) { i.RevokedAt = now }, "revoked"},
		{"revoked beats expired and used up", func(i *Invite) {
			i.RevokedAt, i.ExpiresAt, i.Uses = now, now, 10
		}, "revoked"},
		{"expired beats used up", func(i *Invite) { i.ExpiresAt, i.Uses = now, 10 }, "expired"},
	}
	for _, c := range cases {
		inv := base
		c.mod(&inv)
		if got := inv.State(now); got != c.want {
			t.Errorf("%s: State = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSettingsCacheDefaults(t *testing.T) {
	t.Parallel()
	db := newEnv(t).open(nil)
	c := db.Settings()
	want := Settings{
		RegistrationMode:      ModeInvite,
		InviteDefaultTTLHours: 168,
		InviteDefaultMaxUses:  10,
		UpdateCheck:           true,
	}
	if got := c.Defaults(); got != want {
		t.Errorf("Defaults() = %+v, want %+v", got, want)
	}
	if got := c.Get(); got != want {
		t.Errorf("Get() = %+v, want the defaults", got)
	}
	if len(c.Locked()) != 0 {
		t.Errorf("Locked() = %v", c.Locked())
	}
	c.OnChange(func(Settings, Settings) { t.Error("OnChange ran") })()
	if err := c.Pin("registrationMode", "closed"); !errors.Is(err, errNotImplemented) {
		t.Errorf("Pin = %v, want not implemented", err)
	}
	if _, err := c.Update(context.Background(), nil, CLIActor); !errors.Is(err, errNotImplemented) {
		t.Errorf("Update = %v, want not implemented", err)
	}
}

// TestLaterMethodsSayNotImplemented: the declared API that later slices fill in fails loudly, never silently.
func TestLaterMethodsSayNotImplemented(t *testing.T) {
	t.Parallel()
	db := newEnv(t).open(nil)
	ctx := context.Background()
	now := time.Now()
	if _, err := db.Prune(ctx, now); !errors.Is(err, errNotImplemented) {
		t.Errorf("Prune = %v", err)
	}
	err := db.Write(ctx, func(q *Q) error {
		calls := map[string]error{}
		calls["CreateUser"] = q.CreateUser(&User{})
		_, calls["UserByID"] = q.UserByID("x")
		_, calls["ListUsers"] = q.ListUsers("")
		calls["CreateSession"] = q.CreateSession(&Session{})
		_, _, calls["SessionByTokenHash"] = q.SessionByTokenHash(nil, now)
		_, calls["ListDevices"] = q.ListDevices("x")
		calls["ReplaceSetupToken"] = q.ReplaceSetupToken(nil, now, now)
		calls["UseInvite"] = q.UseInvite("x", now)
		calls["ReplacePasswordReset"] = q.ReplacePasswordReset(PasswordReset{})
		_, calls["ListRooms"] = q.ListRooms()
		_, calls["RoomByID"] = q.RoomByID(DefaultRoomID)
		_, calls["UpsertPushSubscription"] = q.UpsertPushSubscription(&PushSubscription{})
		_, calls["ListPushSubscriptions"] = q.ListPushSubscriptions(PushFilter{})
		calls["AddTransfer"] = q.AddTransfer("2026-10", 1, 1, now)
		calls["AppendAudit"] = q.AppendAudit(AuditEntry{})
		_, calls["SecurityEvents"] = q.SecurityEvents(now, 10)
		for name, err := range calls {
			if !errors.Is(err, errNotImplemented) || !strings.Contains(err.Error(), name) {
				t.Errorf("%s = %v, want a not-implemented error naming it", name, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
