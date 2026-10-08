package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// actorFor is the audit actor of user u acting from ip, as httpapi builds it with ActorOf.
func actorFor(u store.User, ip string) store.Actor {
	return store.Actor{Kind: store.ActorUser, UserID: u.ID, Name: u.Username, IP: ip}
}

// addAdmin creates an active admin with testPassword.
func (e *svcEnv) addAdmin(name string) store.User {
	e.t.Helper()
	return e.addUser(name, func(u *store.User) { u.Role, u.CreatedVia = store.RoleAdmin, "setup" })
}

// set changes one setting as the CLI does, behind the service's back.
func (e *svcEnv) set(name string, value any) {
	e.t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.db.Settings().Update(context.Background(), map[string]json.RawMessage{name: raw}, store.CLIActor); err != nil {
		e.t.Fatalf("set %s = %s: %v", name, raw, err)
	}
}

// invite creates an invite as actor a and returns its row and the token of its link.
func (e *svcEnv) invite(a store.Actor, in InviteInput) (store.Invite, string) {
	e.t.Helper()
	inv, link, err := e.svc.CreateInvite(context.Background(), a, in)
	if err != nil {
		e.t.Fatalf("CreateInvite(%+v): %v", in, err)
	}
	tok, ok := strings.CutPrefix(link.URL, testOrigin+"/invite#")
	if !ok || len(tok) != 32 || !tokenWellFormed(tok, inviteTokenBytes) {
		e.t.Fatalf("invite link %q: want %s/invite#<32-character token>", link.URL, testOrigin)
	}
	if !link.ExpiresAt.Equal(inv.ExpiresAt) {
		e.t.Fatalf("the link expires at %v, the invite at %v", link.ExpiresAt, inv.ExpiresAt)
	}
	return inv, tok
}

// inviteRow reads an invite back.
func (e *svcEnv) inviteRow(id store.InviteID) store.Invite {
	e.t.Helper()
	var inv store.Invite
	e.read(func(q *store.Q) error {
		var err error
		inv, err = q.InviteByID(id)
		return err
	})
	return inv
}

func TestCreateInvite(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	alex := e.addAdmin("Alex")
	admin := actorFor(alex, ipA)
	now := e.clk.now()

	// Every field left out: the settings' defaults, 7 days and 10 uses.
	inv, tok := e.invite(admin, InviteInput{})
	if inv.MaxUses != 10 || inv.Uses != 0 || inv.Note != "" || !inv.CreatedAt.Equal(now) ||
		!inv.ExpiresAt.Equal(now.Add(168*time.Hour)) || len(inv.ID) != 12 || inv.State(now) != "active" {
		t.Errorf("invite with defaults = %+v", inv)
	}
	if inv.CreatedBy == nil || *inv.CreatedBy != (store.UserRef{ID: alex.ID, Username: "Alex"}) ||
		inv.RedeemedBy == nil || len(inv.RedeemedBy) != 0 {
		t.Errorf("creator %+v, redeemedBy %#v; want Alex and an empty list", inv.CreatedBy, inv.RedeemedBy)
	}
	// The database holds the keyed hash, never the token, and the token is not logged.
	if n := e.rawInt("SELECT count(*) FROM invites WHERE token_hash = ?", e.svc.keys.inviteTokenHash(tok)); n != 1 {
		t.Error("invites does not hold the invite-keyed hash of the token")
	}
	if n := e.rawInt("SELECT count(*) FROM invites WHERE instr(token_hash, ?) > 0 OR instr(note, ?) > 0", []byte(tok), tok); n != 0 {
		t.Error("invites holds the token itself")
	}
	if strings.Contains(e.logs.String(), tok) {
		t.Error("the token reached the log")
	}
	rows := e.audit(auditInviteCreated)
	if len(rows) != 1 {
		t.Fatalf("%d invite.created rows, want 1", len(rows))
	}
	if r := rows[0]; r.Actor != admin || r.TargetKind != "invite" || r.TargetID != string(inv.ID) || !r.At.Equal(now) ||
		fmt.Sprint(r.Detail) != "map[expiresAt:2026-10-08T12:00:00.000Z maxUses:10 note:]" {
		t.Errorf("invite.created row = %+v", r)
	}
	if strings.Contains(fmt.Sprint(rows[0]), tok) {
		t.Error("the audit row holds the token")
	}

	// Explicit values, at both ends of their ranges; the note is trimmed and normalized.
	inv, _ = e.invite(admin, InviteInput{Note: "  for Sam\u3000& Kim ", ExpiresInHours: 1, MaxUses: 1000})
	if inv.Note != "for Sam & Kim" || inv.MaxUses != 1000 || !inv.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Errorf("invite = %+v", inv)
	}
	if got := e.inviteRow(inv.ID); got.Note != "for Sam & Kim" || got.MaxUses != 1000 || got.CreatedBy == nil {
		t.Errorf("stored invite = %+v", got)
	}
	inv, _ = e.invite(admin, InviteInput{Note: strings.Repeat("招", 64), ExpiresInHours: 720, MaxUses: 1})
	if utf8.RuneCountInString(inv.Note) != 64 || inv.MaxUses != 1 || !inv.ExpiresAt.Equal(now.Add(720*time.Hour)) {
		t.Errorf("invite at the limits = %+v", inv)
	}

	// The settings are the defaults.
	e.set("inviteDefaultTtlHours", 24)
	e.set("inviteDefaultMaxUses", 3)
	if inv, _ = e.invite(admin, InviteInput{}); inv.MaxUses != 3 || !inv.ExpiresAt.Equal(now.Add(24*time.Hour)) {
		t.Errorf("invite with changed defaults = %+v", inv)
	}

	// Field errors, a code per field; nothing is created.
	before := e.rawCount("invites")
	for _, tc := range []struct {
		in   InviteInput
		want string
	}{
		{InviteInput{ExpiresInHours: -1}, "map[expiresInHours:out_of_range]"},
		{InviteInput{ExpiresInHours: 721}, "map[expiresInHours:out_of_range]"},
		{InviteInput{MaxUses: -1}, "map[maxUses:out_of_range]"},
		{InviteInput{MaxUses: 1001}, "map[maxUses:out_of_range]"},
		{InviteInput{Note: strings.Repeat("x", 65)}, "map[note:too_long]"},
		{InviteInput{Note: strings.Repeat("招", 65)}, "map[note:too_long]"},
		{InviteInput{Note: strings.Repeat("x", 4096)}, "map[note:too_long]"},
		{InviteInput{Note: "two\nlines"}, "map[note:invalid]"},
		{InviteInput{Note: "nul\x00"}, "map[note:invalid]"},
		{InviteInput{Note: "\xff\xfe"}, "map[note:invalid]"},
		{InviteInput{Note: "tab\there", ExpiresInHours: 1 << 30, MaxUses: -5},
			"map[expiresInHours:out_of_range maxUses:out_of_range note:invalid]"},
	} {
		_, _, err := e.svc.CreateInvite(ctx, admin, tc.in)
		if ae := wantCode(t, err, api.CodeValidationFailed); fmt.Sprint(ae.Fields) != tc.want {
			t.Errorf("CreateInvite(%.40q…): fields %v, want %s", fmt.Sprint(tc.in), ae.Fields, tc.want)
		}
	}
	if got := e.rawCount("invites"); got != before {
		t.Errorf("refused invites left %d rows behind", got-before)
	}

	// The CLI: no creator.
	inv, _ = e.invite(store.CLIActor, InviteInput{Note: "from the shell"})
	if inv.CreatedBy != nil || e.inviteRow(inv.ID).CreatedBy != nil {
		t.Errorf("a CLI invite has the creator %+v", inv.CreatedBy)
	}
	if rows := e.audit(auditInviteCreated); rows[len(rows)-1].Actor != store.CLIActor {
		t.Errorf("the CLI's invite.created row has the actor %+v", rows[len(rows)-1].Actor)
	}
}

// TestCreateInvitePermissions: admins and the CLI create invites; members only while membersCanInvite is on; the
// closed mode refuses everyone (03 §7.9).
func TestCreateInvitePermissions(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	alex := e.addAdmin("Alex")
	sam := e.addUser("Sam")
	admin, member := actorFor(alex, ipA), actorFor(sam, ipB)
	create := func(a store.Actor) error {
		_, _, err := e.svc.CreateInvite(ctx, a, InviteInput{})
		return err
	}

	wantCode(t, create(member), api.CodeForbidden)
	// The permission comes first: a member without it gets forbidden for bad fields too, and in the closed mode.
	_, _, err := e.svc.CreateInvite(ctx, member, InviteInput{MaxUses: -1})
	wantCode(t, err, api.CodeForbidden)
	if n := e.rawCount("invites") + e.auditCount(auditInviteCreated); n != 0 {
		t.Fatalf("a refused member left %d rows", n)
	}

	e.set("membersCanInvite", true)
	inv, _ := e.invite(member, InviteInput{Note: "for Kim"})
	if inv.CreatedBy == nil || inv.CreatedBy.ID != sam.ID || inv.CreatedBy.Username != "Sam" {
		t.Errorf("a member's invite has the creator %+v", inv.CreatedBy)
	}

	// Who the actor is comes from the account as stored, not from the actor's name.
	for name, a := range map[string]store.Actor{
		"an unknown user": {Kind: store.ActorUser, UserID: "nobody000000", Name: "Alex"},
		"no user ID":      {Kind: store.ActorUser, Name: "Alex"},
		"anonymous":       {Kind: store.ActorAnonymous},
		"the system":      store.SystemActor,
		"the zero actor":  {},
	} {
		if err := create(a); !api.IsCode(err, api.CodeForbidden) {
			t.Errorf("CreateInvite as %s: %v, want forbidden", name, err)
		}
	}
	for _, st := range []store.UserStatus{store.StatusDisabled, store.StatusPending} {
		e.write(func(q *store.Q) error { return q.SetStatus(alex.ID, st, e.clk.now()) })
		wantCode(t, create(admin), api.CodeForbidden)
	}
	e.write(func(q *store.Q) error { return q.SetStatus(alex.ID, store.StatusActive, e.clk.now()) })
	// A demoted admin is a member from the next call on.
	e.set("membersCanInvite", false)
	e.write(func(q *store.Q) error { return q.SetRole(alex.ID, store.RoleUser, e.clk.now()) })
	wantCode(t, create(admin), api.CodeForbidden)
	e.write(func(q *store.Q) error { return q.SetRole(alex.ID, store.RoleAdmin, e.clk.now()) })
	if err := create(admin); err != nil {
		t.Fatal(err)
	}

	// The closed mode: creating one gets 403 registration_closed, for the admin and the CLI too.
	e.set("membersCanInvite", true)
	e.set("registrationMode", "closed")
	before := e.rawCount("invites")
	for name, a := range map[string]store.Actor{"the admin": admin, "a member": member, "the CLI": store.CLIActor} {
		if err := create(a); !api.IsCode(err, api.CodeRegistrationClosed) {
			t.Errorf("CreateInvite in the closed mode as %s: %v, want registration_closed", name, err)
		}
	}
	e.set("membersCanInvite", false)
	wantCode(t, create(member), api.CodeForbidden)
	if got := e.rawCount("invites"); got != before {
		t.Errorf("the closed mode created %d invites", got-before)
	}
	e.set("registrationMode", "approval")
	if err := create(admin); err != nil {
		t.Errorf("CreateInvite in the approval mode: %v", err)
	}
}

// TestInviteLimits: at most 10 active invites per member and 100 per server (03 §7.9, §14); invites that no longer
// work don't count.
func TestInviteLimits(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	alex := e.addAdmin("Alex")
	sam := e.addUser("Sam")
	kim := e.addUser("Kim")
	admin, member := actorFor(alex, ipA), actorFor(sam, ipB)
	e.set("membersCanInvite", true)

	wantLimit := func(err error, kind api.LimitKind) {
		t.Helper()
		ae := wantCode(t, err, api.CodeLimitReached)
		if fmt.Sprint(ae.Params) != "map[limit:"+string(kind)+"]" {
			t.Errorf("limit_reached params = %v, want limit %s", ae.Params, kind)
		}
	}
	var mine []store.Invite
	for i := range maxMemberInvites {
		hours := 48
		if i == 0 {
			hours = 1 // the first one expires early
		}
		inv, _ := e.invite(member, InviteInput{ExpiresInHours: hours})
		mine = append(mine, inv)
	}
	_, _, err := e.svc.CreateInvite(ctx, member, InviteInput{})
	wantLimit(err, api.LimitKindMemberInvites)
	// Another member has its own ten, and admins have no such limit.
	e.invite(actorFor(kim, ipC), InviteInput{})
	for range 3 {
		e.invite(admin, InviteInput{})
	}
	// A revoked invite and an expired one free their places.
	if err := e.svc.RevokeInvite(ctx, member, mine[1].ID); err != nil {
		t.Fatal(err)
	}
	e.invite(member, InviteInput{})
	_, _, err = e.svc.CreateInvite(ctx, member, InviteInput{})
	wantLimit(err, api.LimitKindMemberInvites)
	e.advance(time.Hour) // mine[0] expires now: an invite works until just before expires_at
	e.invite(member, InviteInput{})
	_, _, err = e.svc.CreateInvite(ctx, member, InviteInput{})
	wantLimit(err, api.LimitKindMemberInvites)

	// The server's limit: 100 active invites, whoever asks.
	active := func() int {
		var n int
		e.read(func(q *store.Q) error {
			var err error
			n, err = q.CountActiveInvites("", e.clk.now())
			return err
		})
		return n
	}
	for active() < maxActiveInvites {
		e.invite(admin, InviteInput{})
	}
	for name, a := range map[string]store.Actor{"the admin": admin, "the CLI": store.CLIActor, "a member with room": actorFor(kim, ipC)} {
		_, _, err := e.svc.CreateInvite(ctx, a, InviteInput{})
		if !api.IsCode(err, api.CodeLimitReached) {
			t.Fatalf("the 101st invite as %s: %v", name, err)
		}
		wantLimit(err, api.LimitKindInvites)
	}
	// A member at its own limit hears about that one.
	_, _, err = e.svc.CreateInvite(ctx, member, InviteInput{})
	wantLimit(err, api.LimitKindMemberInvites)
	if got := active(); got != maxActiveInvites {
		t.Errorf("%d active invites, want %d", got, maxActiveInvites)
	}
}

func TestRevokeInvite(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	alex := e.addAdmin("Alex")
	sam := e.addUser("Sam")
	kim := e.addUser("Kim")
	admin, member, other := actorFor(alex, ipA), actorFor(sam, ipB), actorFor(kim, ipC)
	e.set("membersCanInvite", true)

	byAdmin, adminTok := e.invite(admin, InviteInput{})
	bySam, samTok := e.invite(member, InviteInput{})
	byCLI, _ := e.invite(store.CLIActor, InviteInput{})

	// Unknown invites, and for a member the invites of others: not_found, and nothing changes.
	for name, call := range map[string]func() error{
		"an unknown ID":           func() error { return e.svc.RevokeInvite(ctx, admin, "nope00000000") },
		"an empty ID":             func() error { return e.svc.RevokeInvite(ctx, admin, "") },
		"an admin's, by a member": func() error { return e.svc.RevokeInvite(ctx, member, byAdmin.ID) },
		"the CLI's, by a member":  func() error { return e.svc.RevokeInvite(ctx, member, byCLI.ID) },
		"a member's, by another":  func() error { return e.svc.RevokeInvite(ctx, other, bySam.ID) },
	} {
		if err := call(); !api.IsCode(err, api.CodeNotFound) {
			t.Errorf("RevokeInvite of %s: %v, want not_found", name, err)
		}
	}
	if n := e.auditCount(auditInviteRevoked); n != 0 {
		t.Fatalf("%d invite.revoked rows after refused calls", n)
	}

	// A member revokes its own, also after membersCanInvite went off again.
	e.set("membersCanInvite", false)
	e.advance(time.Minute)
	revokedAt := e.clk.now()
	if err := e.svc.RevokeInvite(ctx, member, bySam.ID); err != nil {
		t.Fatal(err)
	}
	got := e.inviteRow(bySam.ID)
	if got.State(e.clk.now()) != "revoked" || !got.RevokedAt.Equal(revokedAt) || got.RevokedBy != sam.ID {
		t.Errorf("the revoked invite = %+v", got)
	}
	rows := e.audit(auditInviteRevoked)
	if len(rows) != 1 || rows[0].Actor != member || rows[0].TargetKind != "invite" || rows[0].TargetID != string(bySam.ID) ||
		len(rows[0].Detail) != 0 {
		t.Fatalf("invite.revoked rows = %+v", rows)
	}
	_, err := e.svc.CheckInvite(ctx, samTok, meta(ipA))
	wantCode(t, err, api.CodeInviteRevoked)

	// Idempotent: revoking again changes nothing and writes no row.
	e.advance(time.Minute)
	if err := e.svc.RevokeInvite(ctx, admin, bySam.ID); err != nil {
		t.Fatalf("revoking a revoked invite: %v", err)
	}
	if got := e.inviteRow(bySam.ID); !got.RevokedAt.Equal(revokedAt) || got.RevokedBy != sam.ID {
		t.Errorf("a second revocation changed the row: %+v", got)
	}
	if n := e.auditCount(auditInviteRevoked); n != 1 {
		t.Errorf("%d invite.revoked rows after a second revocation, want 1", n)
	}

	// An admin revokes anybody's; the CLI too, with no revoker.
	if err := e.svc.RevokeInvite(ctx, store.CLIActor, byAdmin.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.inviteRow(byAdmin.ID); got.RevokedAt.IsZero() || got.RevokedBy != "" {
		t.Errorf("an invite revoked from the CLI = %+v", got)
	}
	_, err = e.svc.CheckInvite(ctx, adminTok, meta(ipA))
	wantCode(t, err, api.CodeInviteRevoked)
	if err := e.svc.RevokeInvite(ctx, admin, byCLI.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.inviteRow(byCLI.ID); got.RevokedBy != alex.ID {
		t.Errorf("an invite revoked by the admin = %+v", got)
	}

	// An invite that expired can be revoked too: it is revoked from then on, whatever the clock says later.
	expired, _ := e.invite(admin, InviteInput{ExpiresInHours: 1})
	e.advance(time.Hour)
	n := e.auditCount(auditInviteRevoked)
	if got := e.inviteRow(expired.ID); got.State(e.clk.now()) != "expired" {
		t.Fatalf("the invite's state after an hour = %s", got.State(e.clk.now()))
	}
	if err := e.svc.RevokeInvite(ctx, admin, expired.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.inviteRow(expired.ID); got.State(e.clk.now()) != "revoked" || !got.RevokedAt.Equal(e.clk.now()) ||
		got.State(e.clk.now().Add(-2*time.Hour)) != "revoked" {
		t.Errorf("an expired invite after a revocation = %+v", got)
	}
	if e.auditCount(auditInviteRevoked) != n+1 {
		t.Error("revoking an expired invite wrote no audit row")
	}

	// An account that is no longer active revokes nothing.
	live, _ := e.invite(admin, InviteInput{})
	e.write(func(q *store.Q) error { return q.SetStatus(alex.ID, store.StatusDisabled, e.clk.now()) })
	wantCode(t, e.svc.RevokeInvite(ctx, admin, live.ID), api.CodeForbidden)
	if got := e.inviteRow(live.ID); !got.RevokedAt.IsZero() {
		t.Error("a disabled admin revoked an invite")
	}
}

func TestCheckInvite(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	alex := e.addAdmin("Alex")
	admin := actorFor(alex, ipA)
	now := e.clk.now()
	inv, tok := e.invite(admin, InviteInput{MaxUses: 2, ExpiresInHours: 24})

	info, err := e.svc.CheckInvite(ctx, tok, meta(ipB))
	if err != nil {
		t.Fatal(err)
	}
	// Without the serverName setting, the server is called by the host of its primary origin.
	want := InviteInfo{ServerName: "watch.example.com", InvitedBy: "Alex", ExpiresAt: now.Add(24 * time.Hour), UsesLeft: 2}
	if info.ServerName != want.ServerName || info.InvitedBy != want.InvitedBy || !info.ExpiresAt.Equal(want.ExpiresAt) ||
		info.UsesLeft != want.UsesLeft {
		t.Errorf("CheckInvite = %+v, want %+v", info, want)
	}
	e.set("serverName", "Alex's server")
	if info, _ := e.svc.CheckInvite(ctx, tok, meta(ipB)); info.ServerName != "Alex's server" {
		t.Errorf("serverName = %q with the setting", info.ServerName)
	}
	// An invite from the CLI has nobody to name.
	_, cliTok := e.invite(store.CLIActor, InviteInput{})
	if info, err := e.svc.CheckInvite(ctx, cliTok, meta(ipB)); err != nil || info.InvitedBy != "" || info.UsesLeft != 10 {
		t.Errorf("CheckInvite of a CLI invite = %+v, %v", info, err)
	}
	// The check uses nothing up and writes nothing.
	if got := e.inviteRow(inv.ID); got.Uses != 0 {
		t.Errorf("the check counted %d uses", got.Uses)
	}
	if n := e.rawInt("SELECT count(*) FROM audit_log WHERE action NOT IN ('invite.created', 'settings.changed')"); n != 0 {
		t.Errorf("the check wrote %d audit rows", n)
	}

	// Tokens that are no invite: invite_invalid, whatever their shape.
	for name, bad := range map[string]string{
		"unknown":       newToken(inviteTokenBytes),
		"malformed":     "not-a-token",
		"empty":         "",
		"a setup token": newToken(setupTokenBytes),
		"padded":        tok + "=",
		"with a prefix": " " + tok,
	} {
		if _, err := e.svc.CheckInvite(ctx, bad, meta(ipB)); !api.IsCode(err, api.CodeInviteInvalid) {
			t.Errorf("CheckInvite of a token that is %s: %v, want invite_invalid", name, err)
		}
	}

	// It works until just before expires_at.
	e.advance(24*time.Hour - time.Millisecond)
	if _, err := e.svc.CheckInvite(ctx, tok, meta(ipC)); err != nil {
		t.Fatalf("1 ms before the expiry: %v", err)
	}
	e.advance(time.Millisecond)
	_, err = e.svc.CheckInvite(ctx, tok, meta(ipC))
	wantCode(t, err, api.CodeInviteExpired)

	// Used up, and the order when several reasons hold: revoked, then expired, then used up.
	used, usedTok := e.invite(admin, InviteInput{MaxUses: 1, ExpiresInHours: 1})
	e.write(func(q *store.Q) error { return q.UseInvite(used.ID, e.clk.now()) })
	_, err = e.svc.CheckInvite(ctx, usedTok, meta(ipC))
	wantCode(t, err, api.CodeInviteUsedUp)
	e.advance(time.Hour)
	_, err = e.svc.CheckInvite(ctx, usedTok, meta(ipC))
	wantCode(t, err, api.CodeInviteExpired)
	e.write(func(q *store.Q) error { return q.RevokeInvite(used.ID, alex.ID, e.clk.now()) })
	_, err = e.svc.CheckInvite(ctx, usedTok, meta(ipC))
	wantCode(t, err, api.CodeInviteRevoked)

	// The closed mode refuses existing links, the live ones too; reopening brings them back.
	_, liveTok := e.invite(admin, InviteInput{})
	e.set("registrationMode", "closed")
	for _, tk := range []string{liveTok, usedTok, "garbage"} {
		_, err = e.svc.CheckInvite(ctx, tk, meta(ipC))
		wantCode(t, err, api.CodeRegistrationClosed)
	}
	e.set("registrationMode", "approval")
	if _, err := e.svc.CheckInvite(ctx, liveTok, meta(ipC)); err != nil {
		t.Errorf("CheckInvite in the approval mode: %v", err)
	}
}

// TestCheckInviteIPBucket: every check takes a token of the auth-ip bucket, whatever its answer (03 §7.3).
func TestCheckInviteIPBucket(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	_, tok := e.invite(store.CLIActor, InviteInput{})
	for i := range 20 {
		token := tok
		if i%2 == 1 {
			token = "garbage"
		}
		_, _ = e.svc.CheckInvite(ctx, token, meta(ipA))
	}
	_, err := e.svc.CheckInvite(ctx, tok, meta(ipA))
	if ae := wantCode(t, err, api.CodeRateLimited); ae.RetryAfter != 15 {
		t.Errorf("retryAfter = %d s, want 15", ae.RetryAfter)
	}
	if _, err := e.svc.CheckInvite(ctx, tok, meta(ipB)); err != nil {
		t.Errorf("another address: %v", err)
	}
	if rows := e.audit(auditThrottled); len(rows) != 1 || fmt.Sprint(rows[0].Detail) != "map[key:"+ipA+" scope:ip]" {
		t.Errorf("auth.throttled rows = %+v", rows)
	}
}

func TestNormalizeInviteNote(t *testing.T) {
	for _, tc := range []struct{ in, want, code string }{
		{"", "", ""},
		{"   ", "", ""},
		{"\u3000\u00a0", "", ""},
		{"for Sam", "for Sam", ""},
		{"  for Sam \t", "for Sam", ""},
		{"for\u00a0Sam", "for Sam", ""}, // a non-ASCII space becomes U+0020
		{"cafe\u0301", "café", ""},      // NFC
		{"🎬 movie night <3 & more", "🎬 movie night <3 & more", ""}, // symbols and punctuation are fine
		{"Ａlex", "Ａlex", ""}, // free text keeps its width
		{strings.Repeat("é", 64), strings.Repeat("é", 64), ""},
		{strings.Repeat("e\u0301", 64), strings.Repeat("é", 64), ""}, // 64 characters after NFC
		{strings.Repeat("é", 65), "", FieldTooLong},
		{strings.Repeat("x", 257), "", FieldTooLong},
		{"a\tb", "", FieldInvalid},
		{"a\nb", "", FieldInvalid},
		{"a\x7fb", "", FieldInvalid},
		{"a\u0085b", "", FieldInvalid},
		{"bad\xffutf8", "", FieldInvalid},
		{"replaced" + string(utf8.RuneError), "", FieldInvalid}, // what encoding/json makes of bad bytes
	} {
		got, code := normalizeInviteNote(tc.in)
		if got != tc.want || code != tc.code {
			t.Errorf("normalizeInviteNote(%q) = %q, %q; want %q, %q", tc.in, got, code, tc.want, tc.code)
		}
	}
}

// FuzzNormalizeInviteNote: a note the rules accept is valid UTF-8 without control characters, at most 64
// characters, has no surrounding space and normalizes to itself.
func FuzzNormalizeInviteNote(f *testing.F) {
	for _, seed := range []string{"", "for Sam", "  x  ", "for\u00a0Sam", "e\u0301", "\u3000", "a\nb", "\xff", "🎬",
		strings.Repeat("招", 64), strings.Repeat("x", 300), "\u200d", "a\u0301\u0301", "ǆ", "\u1680x\u2003"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		note, code := normalizeInviteNote(in)
		if code != "" {
			if note != "" {
				t.Fatalf("normalizeInviteNote(%q) = %q with the code %s", in, note, code)
			}
			if code != FieldTooLong && code != FieldInvalid {
				t.Fatalf("normalizeInviteNote(%q): unexpected code %q", in, code)
			}
			return
		}
		if !utf8.ValidString(note) || utf8.RuneCountInString(note) > inviteNoteMaxRunes || note != strings.TrimSpace(note) {
			t.Fatalf("normalizeInviteNote(%q) = %q: not valid, too long or with surrounding space", in, note)
		}
		for _, r := range note {
			if r < 0x20 || (r >= 0x7f && r < 0xa0) || r == utf8.RuneError {
				t.Fatalf("normalizeInviteNote(%q) = %q: holds the control character %U", in, note, r)
			}
		}
		again, code := normalizeInviteNote(note)
		if code != "" || again != note {
			t.Fatalf("normalizeInviteNote(%q) = %q, which normalizes to %q (%s)", in, note, again, code)
		}
	})
}
