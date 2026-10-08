package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// The approval queue of 03 §7.9 (03 §12.3 #34–#36, §12.4.8), Admin routes. The pending sign-ups are ordinary users
// rows with the status pending; auth.Service owns approving and rejecting them. The other admin user endpoints
// (#29–#33) come with the admin users slice.

// rejectAllID stands in the {id} of the reject route for "every pending sign-up" (03 §7.9). User IDs are 12
// characters, so it is never one.
const rejectAllID = "all"

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
// body, {} and {"all": false} are not; a body that is not one JSON object is 400 bad_request (03 §12.1). The chain
// has checked the Content-Type and capped the body (03 §12.1).
func rejectAllRequested(r *http.Request) (bool, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return false, nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return false, decodeError(err)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return false, nil
	}
	var req api.RejectRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return false, api.NewError(api.CodeBadRequest)
	}
	return req.All, nil
}
