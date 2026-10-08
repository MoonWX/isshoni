package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/auth"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// The invite endpoints (03 §12.3 #21–#23, §12.4.5, §7.9). All three are User routes: admins see and revoke every
// invite; members create, see and revoke their own while the setting membersCanInvite is on. auth.Service owns the
// rules of creating and revoking; the list is a read of the store.

// actorOf is the audit actor of an authenticated request: its principal, acting from the client's IP.
func (a *API) actorOf(r *http.Request, p auth.Principal) store.Actor {
	return auth.ActorOf(p, a.d.ClientIP(r))
}

// apiInvite is an invite's metadata as REST shows it; its state is the one at now. The token hash stays behind.
func apiInvite(inv store.Invite, now time.Time) api.Invite {
	out := api.Invite{
		ID:         string(inv.ID),
		Note:       inv.Note,
		CreatedAt:  inv.CreatedAt,
		ExpiresAt:  inv.ExpiresAt,
		MaxUses:    inv.MaxUses,
		Uses:       inv.Uses,
		State:      api.InviteState(inv.State(now)),
		RedeemedBy: make([]api.UserRef, 0, len(inv.RedeemedBy)),
	}
	if inv.CreatedBy != nil {
		out.CreatedBy = &api.UserRef{ID: string(inv.CreatedBy.ID), Username: inv.CreatedBy.Username}
	}
	for _, u := range inv.RedeemedBy {
		out.RedeemedBy = append(out.RedeemedBy, api.UserRef{ID: string(u.ID), Username: u.Username})
	}
	return out
}

// inviteChanged tells the SPAs about a change of invite id, after its commit (03 §12.5: an invite created, revoked
// or used by a registration): every admin gets topics, and the invite's creator gets admin.invites when the creator
// is a member, whom the admins' notification does not reach (members list only their own invites). An invite from
// the CLI, or one whose creator is gone, has nobody else to tell.
func (a *API) inviteChanged(r *http.Request, id store.InviteID, topics ...protocol.Topic) {
	a.d.Signal.Notify(NotifyTarget{Admins: true}, topics...)
	if id == "" {
		return
	}
	// The change is committed: the hook does not depend on the client staying connected.
	ctx := context.WithoutCancel(r.Context())
	var member store.UserID
	err := a.d.DB.Read(ctx, func(q *store.Q) error {
		inv, err := q.InviteByID(id)
		if err != nil || inv.CreatedBy == nil {
			return err
		}
		creator, err := q.UserByID(inv.CreatedBy.ID)
		if err != nil {
			return err
		}
		if creator.Role != store.RoleAdmin {
			member = creator.ID
		}
		return nil
	})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		a.d.Logger.LogAttrs(ctx, slog.LevelWarn, "could not find an invite's creator to notify", logx.Err(err))
		return
	}
	if member != "" {
		a.d.Signal.Notify(NotifyTarget{UserID: member}, protocol.TopicAdminInvites)
	}
}

// getInvites is GET /api/v1/invites?state=active|all (03 §12.3 #21, §12.4.5): {invites}, the newest first, each
// with the users who registered with it. The default, state=active, lists only the invites that still work; all
// adds the revoked, expired and used-up ones, which stay listed for 30 days. An admin gets every invite; a member
// gets its own, and 403 forbidden while membersCanInvite is off. Another state value is 400 bad_request. The link
// itself is never listed: it was shown once, when the invite was created.
func (a *API) getInvites(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	var createdBy store.UserID // "" = every invite
	if !p.IsAdmin() {
		if !a.d.DB.Settings().Get().MembersCanInvite {
			WriteError(w, r, api.NewError(api.CodeForbidden))
			return
		}
		createdBy = p.UserID
	}
	includeInactive := false
	switch r.URL.Query().Get("state") {
	case "", string(api.InviteStateActive):
	case "all":
		includeInactive = true
	default:
		WriteError(w, r, api.NewError(api.CodeBadRequest))
		return
	}
	var rows []store.Invite
	err := a.d.DB.Read(r.Context(), func(q *store.Q) error {
		var err error
		rows, err = q.ListInvites(createdBy, includeInactive)
		return err
	})
	if err != nil {
		WriteError(w, r, fmt.Errorf("httpapi: invites: %w", err))
		return
	}
	now := a.d.Clock()
	invites := make([]api.Invite, 0, len(rows))
	for _, inv := range rows {
		invites = append(invites, apiInvite(inv, now))
	}
	WriteJSON(w, http.StatusOK, api.InvitesResponse{Invites: invites})
}

// postInvite is POST /api/v1/invites (03 §12.3 #22, §12.4.5): {note?, expiresInHours?, maxUses?} → 201 {invite,
// url}. Every field is optional; a number left out or 0 is the setting's default. url is the invite link with the
// token in its fragment, shown this once. Errors: 403 forbidden (a member while membersCanInvite is off) or
// registration_closed, 409 limit_reached {limit: invites | member_invites}, 422 validation_failed.
func (a *API) postInvite(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	var req api.CreateInviteRequest
	if err := DecodeJSON(w, r, &req, otherBodyLimit); err != nil {
		WriteError(w, r, err)
		return
	}
	inv, link, err := a.d.Auth.CreateInvite(r.Context(), a.actorOf(r, p), auth.InviteInput{
		Note:           req.Note,
		ExpiresInHours: req.ExpiresInHours,
		MaxUses:        req.MaxUses,
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// The creator is the caller, so its role is known without a lookup (inviteChanged's rule).
	a.d.Signal.Notify(NotifyTarget{Admins: true}, protocol.TopicAdminInvites)
	if !p.IsAdmin() {
		a.d.Signal.Notify(NotifyTarget{UserID: p.UserID}, protocol.TopicAdminInvites)
	}
	WriteJSON(w, http.StatusCreated, api.CreateInviteResponse{Invite: apiInvite(inv, a.d.Clock()), URL: link.URL})
}

// deleteInvite is DELETE /api/v1/invites/{id} (03 §12.3 #23): 204. The link stops working; the accounts created
// with it stay. An admin revokes any invite, a member its own; anything else is 404 not_found. It is idempotent: an
// invite that was revoked before answers 204 too.
func (a *API) deleteInvite(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	id := store.InviteID(r.PathValue("id"))
	if err := a.d.Auth.RevokeInvite(r.Context(), a.actorOf(r, p), id); err != nil {
		WriteError(w, r, err)
		return
	}
	a.inviteChanged(r, id, protocol.TopicAdminInvites)
	w.WriteHeader(http.StatusNoContent)
}
