package httpapi

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// getMe is GET /api/v1/me (03 §12.3 #11, §12.4.3), User: the caller's account, its web session, what it may do and,
// for admins, the navigation badges. The SPA calls it at start, which is also what rotates the session token at
// least daily for an active user (the chain's MaybeRotate step, 03 §7.4). It reads only: the session bookkeeping of
// the chain is the one write a GET may cause (03 §7.5).
//
// The account and the session are read fresh, not taken from the principal (which may come from auth's 30 s cache).
func (a *API) getMe(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	var user store.User
	var sess *store.Session
	pending := 0
	err := a.d.DB.Read(r.Context(), func(q *store.Q) error {
		var err error
		if user, err = q.UserByID(p.UserID); err != nil {
			return err
		}
		sessions, err := q.ListSessions(p.UserID)
		if err != nil {
			return err
		}
		for i := range sessions {
			if sessions[i].ID == p.SessionID {
				sess = &sessions[i]
			}
		}
		if user.Role == store.RoleAdmin {
			pending, err = q.CountPending()
		}
		return err
	})
	if errors.Is(err, store.ErrNotFound) || (err == nil && (sess == nil || user.Status != store.StatusActive)) {
		// The account or the session went away between the chain's authentication and this read.
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	if err != nil {
		WriteError(w, r, fmt.Errorf("httpapi: me: %w", err))
		return
	}
	admin := user.Role == store.RoleAdmin
	me := api.Me{
		User: api.User{ID: string(user.ID), Username: user.Username, Role: api.Role(user.Role), CreatedAt: user.CreatedAt},
		Session: &api.SessionInfo{
			ID:        string(sess.ID),
			Name:      sess.Name,
			CreatedAt: sess.CreatedAt,
			ExpiresAt: sess.ExpiresAt,
			Current:   true,
		},
		Permissions: api.Permissions{
			Admin:         admin,
			CreateInvites: admin || a.d.DB.Settings().Get().MembersCanInvite,
		},
	}
	if admin {
		me.Badges = &api.Badges{PendingApprovals: pending}
	}
	WriteJSON(w, http.StatusOK, me)
}
