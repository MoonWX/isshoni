package httpapi

import (
	"fmt"
	"log/slog"
	"net/http"
	"slices"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/auth"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// The admin's user endpoints (README S64; 03 §12.3 #29–#33, §12.4.8, §7.10–7.11) and the approval queue of 03 §7.9
// (README S42; #34–#36), all Admin routes. auth.Service owns every change: the last-admin and self rules, the
// password an admin types again to make another admin or to reset one, the revocation rows of 03 §7.7, the audit
// rows and the admin alerts. The handlers decode the request, call it and tell the hub and the SPAs (03 §12.5). The
// list is a read of the store. Pending sign-ups are ordinary users rows with the status pending.

// rejectAllID stands in the {id} of the reject route for "every pending sign-up" (03 §7.9). User IDs are 12
// characters, so it is never one.
const rejectAllID = "all"

// queryStatus is the query parameter of GET /api/v1/admin/users that narrows the list to one status.
const queryStatus = "status"

// apiAdminUser is a user as the admin pages show it (03 §12.4.8). online comes from the signaling hub. It has no
// IPs: admins see those in the audit log and the approval queue only.
func apiAdminUser(row store.UserRow, online bool) api.AdminUser {
	out := api.AdminUser{
		ID:           string(row.ID),
		Username:     row.Username,
		Role:         api.Role(row.Role),
		Status:       api.UserStatus(row.Status),
		CreatedVia:   api.CreatedVia(row.CreatedVia),
		CreatedAt:    row.CreatedAt,
		LastLoginAt:  row.LastLoginAt,
		LastSeenAt:   row.LastSeenAt,
		Sessions:     row.Sessions,
		Devices:      row.Devices,
		Online:       online,
		ResetPending: row.ResetPending,
	}
	if row.InvitedBy != nil {
		out.InvitedBy = &api.UserRef{ID: string(row.InvitedBy.ID), Username: row.InvitedBy.Username}
	}
	return out
}

// approvalsChanged tells every admin's SPA that the approval queue and the user list changed (03 §12.5: a sign-up
// requested, approved or rejected).
func (a *API) approvalsChanged() {
	a.d.Signal.Notify(NotifyTarget{Admins: true}, protocol.TopicAdminApprovals, protocol.TopicAdminUsers)
}

// usersChanged tells every admin's SPA that the user list changed (03 §12.5: a user renamed, a role changed, a
// user disabled, enabled, deleted or signed out, a reset link issued).
func (a *API) usersChanged() {
	a.d.Signal.Notify(NotifyTarget{Admins: true}, protocol.TopicAdminUsers)
}

// adminUser is an account a handler just changed, as the user list shows it: with its sessions, devices and
// inviter, read after the commit, and whether it is online. Should that read fail, or the account be gone again,
// the answer is built from the account alone: the change went through, and a 500 would say that it did not.
func (a *API) adminUser(r *http.Request, user store.User) api.AdminUser {
	row := store.UserRow{User: user, ResetPending: user.PasswordHash == ""}
	err := a.d.DB.Read(r.Context(), func(q *store.Q) error {
		rows, err := q.ListUsers("")
		for i := range rows {
			if rows[i].ID == user.ID {
				row = rows[i]
			}
		}
		return err
	})
	if err != nil {
		level := slog.LevelWarn
		if r.Context().Err() != nil {
			level = slog.LevelDebug // the client went away
		}
		a.d.Logger.LogAttrs(r.Context(), level, "could not read a changed account back for the answer",
			slog.String("user_id", string(user.ID)), logx.Err(err))
	}
	_, online := a.d.Signal.OnlineUserIDs()[user.ID]
	return apiAdminUser(row, online)
}

// getUsers is GET /api/v1/admin/users (03 §12.3 #29, §12.4.8): {users}, every account by name, pending sign-ups
// included, each with its role, status, how it was created, when it last logged in and was last seen, who invited
// it, how many sessions and devices it has, whether it is online (Signal.OnlineUserIDs) and whether a password
// reset is pending. No IPs: admins see those in the audit log and the approval queue. ?status=active, pending or
// disabled narrows the list; any other value, or the parameter twice, is 400 bad_request. It reads only.
func (a *API) getUsers(w http.ResponseWriter, r *http.Request) {
	var status store.UserStatus
	if values, ok := r.URL.Query()[queryStatus]; ok {
		if len(values) != 1 {
			WriteError(w, r, api.NewError(api.CodeBadRequest))
			return
		}
		switch status = store.UserStatus(values[0]); status {
		case store.StatusActive, store.StatusPending, store.StatusDisabled:
		default:
			WriteError(w, r, api.NewError(api.CodeBadRequest))
			return
		}
	}
	var rows []store.UserRow
	err := a.d.DB.Read(r.Context(), func(q *store.Q) error {
		var err error
		rows, err = q.ListUsers(status)
		return err
	})
	if err != nil {
		WriteError(w, r, opErr("users", err))
		return
	}
	online := a.d.Signal.OnlineUserIDs()
	users := make([]api.AdminUser, 0, len(rows))
	for _, row := range rows {
		_, on := online[row.ID]
		users = append(users, apiAdminUser(row, on))
	}
	WriteJSON(w, http.StatusOK, api.AdminUsersResponse{Users: users})
}

// patchUser is PATCH /api/v1/admin/users/{id} (03 §12.3 #30, §12.4.8, §7.11): {username?, role?, status?,
// currentPassword?} → 200 {user}, the account after the change. It is a JSON merge (03 §12.1): only the fields
// present change, and one that is given the value it has changes nothing. Errors:
//   - 422 validation_failed with a code per field: the username rules; a role other than admin or user (invalid);
//     a status other than active or disabled, or any status for a pending sign-up, which goes through the approval
//     queue (not_allowed);
//   - 404 user_not_found;
//   - 409 self_action_forbidden (an admin can't disable the own account), last_admin (the only active admin can't
//     be demoted or disabled), username_taken;
//   - 403 wrong_password: making a user an admin needs the acting admin's own password in currentPassword, so a
//     stolen admin cookie alone can't mint a second admin; also 429 rate_limited after too many wrong ones
//     (03 §7.3) and 503 server_busy.
//
// After the change (03 §12.5): for a request with a name or a role, the hub refreshes the user's open connections
// (Signal.UserChanged, with the name and role the account has now) and the user's own SPA refetches me; every
// admin's SPA refetches the user list. Disabling closes the user's connections (auth's ConnCloser, 03 §7.7). The
// handler can't tell a request that changed nothing from one that did, so it notifies for both; the values it
// sends are always the account's current ones.
func (a *API) patchUser(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	var req api.PatchUserRequest
	if err := DecodeJSON(w, r, &req, otherBodyLimit); err != nil {
		WriteError(w, r, err)
		return
	}
	ch := auth.UserChange{Username: req.Username, ActorPassword: req.CurrentPassword}
	if req.Role != nil {
		role := store.Role(*req.Role)
		ch.Role = &role
	}
	if req.Status != nil {
		status := store.UserStatus(*req.Status)
		ch.Status = &status
	}
	user, err := a.d.Auth.UpdateUser(r.Context(), a.actorOf(r, p), store.UserID(r.PathValue("id")), ch)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// The account as it is now, read after the commit: what the answer shows, and what the hub is told.
	current := a.adminUser(r, user)
	if identity := ch.Username != nil || ch.Role != nil; identity {
		a.d.Signal.UserChanged(user.ID, current.Username, current.Role == api.RoleAdmin)
		a.d.Signal.Notify(NotifyTarget{UserID: user.ID}, protocol.TopicMe)
		a.usersChanged()
	} else if ch.Status != nil {
		a.usersChanged()
	}
	WriteJSON(w, http.StatusOK, api.AdminUserResponse{User: current})
}

// deleteUser is DELETE /api/v1/admin/users/{id} (03 §12.3 #31; 03 §7.7 "Delete user", §7.11): 204. The account is
// gone with everything that hangs on it, its username is free again, and every connection of the user is closed.
// Errors: 404 user_not_found, 409 self_action_forbidden (POST /me/delete is the place for the own account) and
// last_admin.
func (a *API) deleteUser(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	if err := a.d.Auth.DeleteUser(r.Context(), a.actorOf(r, p), store.UserID(r.PathValue("id"))); err != nil {
		WriteError(w, r, err)
		return
	}
	a.usersChanged()
	w.WriteHeader(http.StatusNoContent)
}

// postPasswordReset is POST /api/v1/admin/users/{id}/password-reset (03 §12.3 #32, §12.4.8, §7.10):
// {currentPassword?} → 201 {url, expiresAt}, a one-time link <origin>/reset#<token> that works for 24 h. This
// answer is the only place the link is ever shown: the admin passes it on privately. Issuing it clears the user's
// password and signs the user out everywhere, at once. Errors: 404 user_not_found, 409 self_action_forbidden
// (POST /me/password is the place for the own account), and 403 wrong_password: a reset of another admin needs the
// acting admin's own password in currentPassword, since the link would hand over that admin account; also 429
// rate_limited and 503 server_busy. The body may be left out when no password is needed: no body, an empty one and
// one of white space only send no password (decodeOptionalJSON); a body that is not one JSON object is 400
// bad_request.
func (a *API) postPasswordReset(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	var req api.PasswordResetRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	id := store.UserID(r.PathValue("id"))
	link, err := a.d.Auth.IssuePasswordReset(r.Context(), a.actorOf(r, p), id, req.CurrentPassword)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	a.usersChanged() // a reset is pending, and the user's sessions are gone
	WriteJSON(w, http.StatusCreated, api.ResetLink{URL: link.URL, ExpiresAt: link.ExpiresAt})
}

// postSignOut is POST /api/v1/admin/users/{id}/sign-out (03 §12.3 #33, §12.4.8; 03 §7.7 "Admin: sign out
// everywhere"): 200 {sessions, devices}, how many of each were ended. Every web session and device of the user is
// gone and every connection closed; the password stays. 404 user_not_found. The body ({}) is not read.
//
// An admin may sign the own account out everywhere this way; the session of this request is then one of them, and
// the answer clears its cookie.
func (a *API) postSignOut(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	id := store.UserID(r.PathValue("id"))
	sessions, devices, err := a.d.Auth.SignOutUser(r.Context(), a.actorOf(r, p), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	a.usersChanged()
	if id == p.UserID {
		a.endSessionCookie(w)
	}
	WriteJSON(w, http.StatusOK, api.SignOutResponse{Sessions: sessions, Devices: devices})
}

// getApprovals is GET /api/v1/admin/approvals (03 §12.3 #34): {pending}, the sign-ups that await a decision, the
// oldest first, each with its username, the time of the request and the IP it came from (the IP of its
// user.signup_requested audit row).
func (a *API) getApprovals(w http.ResponseWriter, r *http.Request) {
	var rows []store.UserRow
	err := a.d.DB.Read(r.Context(), func(q *store.Q) error {
		var err error
		rows, err = q.ListUsers(store.StatusPending)
		return err
	})
	if err != nil {
		WriteError(w, r, fmt.Errorf("httpapi: approvals: %w", err))
		return
	}
	// The store lists by name; a queue is read in the order of arrival. The sort is stable, so equal times keep the
	// name order.
	slices.SortStableFunc(rows, func(x, y store.UserRow) int { return x.CreatedAt.Compare(y.CreatedAt) })
	pending := make([]api.PendingUser, 0, len(rows))
	for _, row := range rows {
		pending = append(pending, api.PendingUser{
			ID:          string(row.ID),
			Username:    row.Username,
			RequestedAt: row.CreatedAt,
			IP:          row.SignupIP,
		})
	}
	WriteJSON(w, http.StatusOK, api.ApprovalsResponse{Pending: pending})
}

// postApprove is POST /api/v1/admin/approvals/{id}/approve (03 §12.3 #35): 200 {user}; the account is active and
// can log in. A user that does not exist or is not pending is 404 user_not_found. The body ({}) is not read.
func (a *API) postApprove(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	user, err := a.d.Auth.Approve(r.Context(), a.actorOf(r, p), store.UserID(r.PathValue("id")))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	a.approvalsChanged()
	// A sign-up that was pending until now has no session, no device and no connection, and came without an invite.
	row := store.UserRow{User: user, ResetPending: user.PasswordHash == ""}
	WriteJSON(w, http.StatusOK, api.AdminUserResponse{User: apiAdminUser(row, false)})
}

// postReject is POST /api/v1/admin/approvals/{id}/reject (03 §12.3 #36): 204; the sign-up is deleted and its
// username is free again. A user that does not exist or is not pending is 404 user_not_found.
//
// Reject all (03 §7.9): with all in place of the ID and the body {"all": true}, it deletes every sign-up that is
// pending and answers 200 {rejected}. Both are required, so a stray request can't empty the queue: all without that
// body is looked up as an ID, which no user has, and gets the 404. For any other {id} the body ({}) is not read.
func (a *API) postReject(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	id := r.PathValue("id")
	if id == rejectAllID {
		all, err := rejectAllRequested(r)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		if all {
			n, err := a.d.Auth.RejectAll(r.Context(), a.actorOf(r, p))
			if err != nil {
				WriteError(w, r, err)
				return
			}
			if n > 0 {
				a.approvalsChanged()
			}
			WriteJSON(w, http.StatusOK, api.RejectAllResponse{Rejected: n})
			return
		}
	}
	if err := a.d.Auth.Reject(r.Context(), a.actorOf(r, p), store.UserID(id)); err != nil {
		WriteError(w, r, err)
		return
	}
	a.approvalsChanged()
	w.WriteHeader(http.StatusNoContent)
}

// rejectAllRequested reads the body of a reject request for the ID all and reports whether it is {"all": true}. No
// body, an empty or blank one, {} and {"all": false} are not; a body that is not one JSON object is 400 bad_request
// (03 §12.1; decodeOptionalJSON).
func rejectAllRequested(r *http.Request) (bool, error) {
	var req api.RejectRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		return false, err
	}
	return req.All, nil
}
