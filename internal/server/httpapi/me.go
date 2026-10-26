package httpapi

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/auth"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// getMe is GET /api/v1/me (03 §12.3 #11, §12.4.3), User: the caller's account, its web session, what it may do and,
// for admins, the navigation badges. The SPA calls it at start, which is also what rotates the session token at
// least daily for an active user (the chain's MaybeRotate step, 03 §7.4). It reads only: the session bookkeeping of
// the chain is the one write a GET may cause (03 §7.5).
//
// A device's bearer (M2 02 §7.2) gets its device, marked current, in place of the session: its ID, name, kind, OS,
// app version and creation time.
//
// The account and the session or device are read fresh, not taken from the principal (which may come from auth's
// 30 s caches).
func (a *API) getMe(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	device := p.Method == auth.MethodBearer
	var user store.User
	var sess *store.Session
	var dev *store.Device
	pending := 0
	err := a.d.DB.Read(r.Context(), func(q *store.Q) error {
		var err error
		if user, err = q.UserByID(p.UserID); err != nil {
			return err
		}
		if device {
			if dev, err = ownDevice(q, p); err != nil {
				return err
			}
		} else if sess, err = ownSession(q, p); err != nil {
			return err
		}
		if user.Role == store.RoleAdmin {
			pending, err = q.CountPending()
		}
		return err
	})
	if errors.Is(err, store.ErrNotFound) || (err == nil && (sess == nil && dev == nil || user.Status != store.StatusActive)) {
		// The account, the session or the device went away between the chain's authentication and this read.
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
		Permissions: api.Permissions{
			Admin:         admin,
			CreateInvites: admin || a.d.DB.Settings().Get().MembersCanInvite,
		},
	}
	if dev != nil {
		me.Device = &api.DeviceInfo{
			ID:         string(dev.ID),
			Name:       dev.Name,
			ClientKind: dev.ClientKind,
			OS:         dev.OS,
			AppVersion: dev.AppVersion,
			CreatedAt:  dev.CreatedAt,
			Current:    true,
		}
	} else {
		me.Session = &api.SessionInfo{
			ID:        string(sess.ID),
			Name:      sess.Name,
			CreatedAt: sess.CreatedAt,
			ExpiresAt: sess.ExpiresAt,
			Current:   true,
		}
	}
	if admin {
		me.Badges = &api.Badges{PendingApprovals: pending}
	}
	WriteJSON(w, http.StatusOK, me)
}

// ownSession returns the principal's web session, or nil when it is gone.
func ownSession(q *store.Q, p auth.Principal) (*store.Session, error) {
	sessions, err := q.ListSessions(p.UserID)
	if err != nil {
		return nil, err
	}
	for i := range sessions {
		if sessions[i].ID == p.SessionID {
			return &sessions[i], nil
		}
	}
	return nil, nil
}

// ownDevice returns the principal's device, or nil when it is gone or is no longer the principal's user's.
func ownDevice(q *store.Q, p auth.Principal) (*store.Device, error) {
	if p.DeviceID == "" {
		return nil, nil
	}
	dev, _, err := q.DeviceByID(p.DeviceID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	case dev.UserID != p.UserID:
		return nil, nil
	}
	return &dev, nil
}
