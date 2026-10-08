package auth

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/secure/precis"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// Invite links (03 §7.9): creating, revoking and checking them. Register (register.go) redeems them.

// Invite limits of 03 §7.9 and §14.
const (
	inviteMaxHours     = 720  // expiresInHours is 1–720
	inviteMaxUses      = 1000 // maxUses is 1–1000
	inviteNoteMaxRunes = 64   // the note column (03 §5)
	inviteNoteMaxBytes = 4 * inviteNoteMaxRunes
	maxActiveInvites   = 100 // active invites per server
	maxMemberInvites   = 10  // active invites per member; admins and the CLI have only the server's limit
)

// Audit actions written here (03 §10).
const (
	auditInviteCreated = "invite.created"
	auditInviteRevoked = "invite.revoked"
)

// targetInvite is the audit target kind of an invite (03 §5).
const targetInvite = "invite"

// JSON names of the fields of POST /api/v1/invites, the keys of its validation_failed answer.
const (
	fieldNote           = "note"
	fieldExpiresInHours = "expiresInHours"
	fieldMaxUses        = "maxUses"
)

// limitReached is 409 limit_reached with params.limit.
func limitReached(kind api.LimitKind) error {
	return &api.Error{Code: api.CodeLimitReached, Params: map[string]any{api.ParamLimit: string(kind)}}
}

// errRegistrationClosed is the answer of every invite and registration call in the closed mode.
func errRegistrationClosed() error { return api.NewError(api.CodeRegistrationClosed) }

// normalizeInviteNote applies the rules of an invite's note, free text such as "for Sam", and returns the form that
// is stored, or the field code of the rule that failed:
//
//  1. at most 256 bytes of input (64 characters of 4 bytes), else too_long, before any other work;
//  2. surrounding white space is dropped; an empty note is fine;
//  3. valid UTF-8 without U+FFFD (what a JSON decoder leaves of bad bytes) that passes PRECIS OpaqueString, the
//     profile of free text (03 §3.3): NFC, and non-ASCII spaces become U+0020. Control characters, a line break
//     among them, are refused, else invalid;
//  4. at most 64 characters, else too_long.
//
// The result is stable: a stored note normalizes to itself.
func normalizeInviteNote(in string) (note, code string) {
	if len(in) > inviteNoteMaxBytes {
		return "", FieldTooLong
	}
	// The profile works on valid UTF-8 only, and would turn bad bytes into U+FFFD instead of refusing them.
	// encoding/json does the same to a request body, so U+FFFD itself is refused too, like in the server name.
	if !utf8.ValidString(in) || strings.ContainsRune(in, utf8.RuneError) {
		return "", FieldInvalid
	}
	trimmed := strings.TrimSpace(in)
	if trimmed == "" {
		return "", ""
	}
	out, err := precis.OpaqueString.String(trimmed)
	if err != nil {
		return "", FieldInvalid
	}
	// The profile turned non-ASCII spaces into U+0020 and may have changed the length; trimming again is a no-op
	// after the trim above, and keeps the result a fixed point whatever the profile does at the edges.
	out = strings.TrimSpace(out)
	if again, err := precis.OpaqueString.String(out); out != "" && (err != nil || again != out) {
		return "", FieldInvalid
	}
	if utf8.RuneCountInString(out) > inviteNoteMaxRunes {
		return "", FieldTooLong
	}
	return out, ""
}

// invitePermission is the rule of who may create an invite now (03 §7.9): admins and the CLI, and members while
// the setting membersCanInvite is on, else *api.Error{forbidden}; and nobody in the closed mode, which answers
// registration_closed. The permission comes first, so a member without it learns nothing else.
func invitePermission(who acting, set store.Settings) error {
	if !who.admin && !set.MembersCanInvite {
		return api.NewError(api.CodeForbidden)
	}
	if set.RegistrationMode == store.ModeClosed {
		return errRegistrationClosed()
	}
	return nil
}

// CreateInvite creates an invite link and returns its row and the link, Origins.Primary + "/invite#" + token
// (03 §7.9). The link is returned only here: the database keeps the keyed hash of the token, so a lost link means
// creating a new one. a is the creator: a user (auth.ActorOf of the request's principal) or store.CLIActor for
// `isshoni admin invite create`. In order:
//
//  1. who may: admins and the CLI, and members while membersCanInvite is on → 403 forbidden; nobody in the closed
//     mode → 403 registration_closed;
//  2. the fields: expiresInHours is 1–720 and maxUses is 1–1000 (out_of_range), 0 meaning the settings
//     inviteDefaultTtlHours and inviteDefaultMaxUses; the note is free text of at most 64 characters (too_long,
//     invalid; normalizeInviteNote) → 422 validation_failed;
//  3. one Write: the permission again, read at the moment of the change; at most 10 active invites per member → 409
//     limit_reached {limit: member_invites}; at most 100 active invites per server → 409 limit_reached
//     {limit: invites}; the row; the invite.created audit row {maxUses, expiresAt, note}.
//
// The returned invite has its creator (nil for the CLI) and an empty RedeemedBy.
func (s *Service) CreateInvite(ctx context.Context, a store.Actor, in InviteInput) (store.Invite, Link, error) {
	set := s.db.Settings().Get()
	// Read-only first, so a call that will be refused never takes the writer.
	err := s.db.Read(ctx, func(q *store.Q) error {
		who, err := actingAs(q, a)
		if err != nil {
			return err
		}
		return invitePermission(who, set)
	})
	if err != nil {
		return store.Invite{}, Link{}, serviceErr("create invite", err)
	}

	fields := map[string]string{}
	hours := in.ExpiresInHours
	switch {
	case hours == 0:
		hours = set.InviteDefaultTTLHours
	case hours < 1 || hours > inviteMaxHours:
		fields[fieldExpiresInHours] = FieldOutOfRange
	}
	uses := in.MaxUses
	switch {
	case uses == 0:
		uses = set.InviteDefaultMaxUses
	case uses < 1 || uses > inviteMaxUses:
		fields[fieldMaxUses] = FieldOutOfRange
	}
	note, code := normalizeInviteNote(in.Note)
	if code != "" {
		fields[fieldNote] = code
	}
	if len(fields) > 0 {
		return store.Invite{}, Link{}, validationFailed(fields)
	}

	now := s.now()
	token := newToken(inviteTokenBytes)
	var inv store.Invite
	err = s.db.Write(ctx, func(q *store.Q) error {
		who, err := actingAs(q, a)
		if err != nil {
			return err
		}
		// The settings as they are now: a mode or permission change that committed since the check above counts.
		if err := invitePermission(who, s.db.Settings().Get()); err != nil {
			return err
		}
		if !who.admin {
			n, err := q.CountActiveInvites(who.id(), now)
			if err != nil {
				return err
			}
			if n >= maxMemberInvites {
				return limitReached(api.LimitKindMemberInvites)
			}
		}
		n, err := q.CountActiveInvites("", now)
		if err != nil {
			return err
		}
		if n >= maxActiveInvites {
			return limitReached(api.LimitKindInvites)
		}
		inv = store.Invite{
			TokenHash: s.keys.inviteTokenHash(token),
			Note:      note,
			CreatedAt: now,
			ExpiresAt: now.Add(time.Duration(hours) * time.Hour),
			MaxUses:   uses,
		}
		if who.id() != "" {
			inv.CreatedBy = &store.UserRef{ID: who.user.ID, Username: who.user.Username}
		}
		if err := q.CreateInvite(&inv); err != nil {
			return err
		}
		inv.RedeemedBy = []store.UserRef{}
		return q.AppendAudit(store.AuditEntry{At: now, Action: auditInviteCreated, Actor: a,
			TargetKind: targetInvite, TargetID: string(inv.ID),
			Detail: map[string]any{
				"maxUses":   inv.MaxUses,
				"expiresAt": api.WireTime(inv.ExpiresAt).String(),
				"note":      inv.Note,
			}})
	})
	if err != nil {
		return store.Invite{}, Link{}, serviceErr("create invite", err)
	}
	return inv, Link{URL: s.origins.Primary + "/invite#" + token, ExpiresAt: inv.ExpiresAt}, nil
}

// RevokeInvite makes an invite link stop working for good (DELETE /api/v1/invites/{id}, 03 §7.9): it sets
// revoked_at and writes invite.revoked. The accounts created with the invite stay. Admins and the CLI revoke any
// invite; a member only its own, whether or not membersCanInvite is still on. An unknown invite, and for a member
// an invite of somebody else, is *api.Error{not_found}.
//
// An invite that has expired or is used up can be revoked too: it then counts as revoked, whatever the clock says
// later. It is idempotent: an invite that was revoked before keeps its first revocation, no audit row is written,
// and the answer is the same.
func (s *Service) RevokeInvite(ctx context.Context, a store.Actor, id store.InviteID) error {
	now := s.now()
	err := s.db.Write(ctx, func(q *store.Q) error {
		who, err := actingAs(q, a)
		if err != nil {
			return err
		}
		inv, err := q.InviteByID(id)
		if errors.Is(err, store.ErrNotFound) {
			return api.NewError(api.CodeNotFound)
		}
		if err != nil {
			return err
		}
		if !who.admin && (inv.CreatedBy == nil || inv.CreatedBy.ID != who.id()) {
			return api.NewError(api.CodeNotFound)
		}
		if !inv.RevokedAt.IsZero() {
			return nil
		}
		if err := q.RevokeInvite(inv.ID, who.id(), now); err != nil {
			return err
		}
		return q.AppendAudit(store.AuditEntry{At: now, Action: auditInviteRevoked, Actor: a,
			TargetKind: targetInvite, TargetID: string(inv.ID)})
	})
	if err != nil {
		return serviceErr("revoke invite", err)
	}
	return nil
}

// CheckInvite answers POST /api/v1/auth/invite/check (03 §7.9): what the invite page shows before its form, or why
// the link doesn't work. Only somebody who holds the token learns this, so the detail is safe to give. In order:
//
//  1. the auth-ip bucket → 429 rate_limited;
//  2. the closed mode refuses existing links → 403 registration_closed;
//  3. the token: unknown (or of the wrong shape, which costs no lookup) → 404 invite_invalid; revoked → 410
//     invite_revoked; expired → 410 invite_expired; used up → 410 invite_used_up.
//
// It reads only and uses nothing up: Register counts the use.
func (s *Service) CheckInvite(ctx context.Context, token string, m ReqMeta) (InviteInfo, error) {
	if err := s.takeAuthIP(ctx, m); err != nil {
		return InviteInfo{}, err
	}
	set := s.db.Settings().Get()
	if set.RegistrationMode == store.ModeClosed {
		return InviteInfo{}, errRegistrationClosed()
	}
	now := s.now()
	var inv store.Invite
	err := s.db.Read(ctx, func(q *store.Q) error {
		var err error
		inv, err = s.liveInvite(q, token, now)
		return err
	})
	if err != nil {
		return InviteInfo{}, serviceErr("check invite", err)
	}
	info := InviteInfo{
		ServerName: s.serverName(set),
		ExpiresAt:  inv.ExpiresAt,
		UsesLeft:   inv.MaxUses - inv.Uses,
	}
	if inv.CreatedBy != nil {
		info.InvitedBy = inv.CreatedBy.Username
	}
	return info, nil
}

// liveInvite finds the invite of a raw token and checks that it works at now: the rule both the check and the
// registering Write apply. A token of the wrong shape is refused without a lookup.
func (s *Service) liveInvite(q *store.Q, token string, now time.Time) (store.Invite, error) {
	if !tokenWellFormed(token, inviteTokenBytes) {
		return store.Invite{}, api.NewError(api.CodeInviteInvalid)
	}
	inv, err := q.InviteByTokenHash(s.keys.inviteTokenHash(token))
	if errors.Is(err, store.ErrNotFound) {
		return store.Invite{}, api.NewError(api.CodeInviteInvalid)
	}
	if err != nil {
		return store.Invite{}, err
	}
	if err := inviteStateErr(inv, now); err != nil {
		return store.Invite{}, err
	}
	return inv, nil
}

// inviteStateErr says why an invite doesn't work at now, nil when it does (03 §12.2). store.Invite.State decides: a
// revoked invite is revoked whatever else holds, then expired wins over used up.
func inviteStateErr(inv store.Invite, now time.Time) error {
	switch api.InviteState(inv.State(now)) {
	case api.InviteStateActive:
		return nil
	case api.InviteStateRevoked:
		return api.NewError(api.CodeInviteRevoked)
	case api.InviteStateExpired:
		return api.NewError(api.CodeInviteExpired)
	case api.InviteStateUsedUp:
		return api.NewError(api.CodeInviteUsedUp)
	}
	return api.NewError(api.CodeInviteInvalid)
}

// serverName is the server's name as the invite page shows it: the serverName setting, or else the host of the
// primary origin (03 §9), the rule of GET /api/v1/info.
func (s *Service) serverName(set store.Settings) string {
	if set.ServerName != "" {
		return set.ServerName
	}
	if u, err := url.Parse(s.origins.Primary); err == nil {
		return u.Hostname()
	}
	return ""
}
