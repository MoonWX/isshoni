package httpapi

import (
	"errors"
	"net/http"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/auth"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// The auth endpoints of 03 §12.3: login (#2), logout (#3), setup/check (#7) and setup/complete (#8) (README S30),
// register (#5) and invite/check (#6) (README S42), and reset/check (#9) and reset/complete (#10) (README S64). All
// are Public: the chain's CSRF step still applies, and the throttles are auth's (03 §7.3). logout-everywhere (#4)
// is a User route and sits with the rest of self-service in account.go.

// apiUser is the public identity shape {id, username, role} of a user (03 §12.4.2).
func apiUser(u store.User) api.User {
	return api.User{ID: string(u.ID), Username: u.Username, Role: api.Role(u.Role)}
}

// devicesChanged tells the user's SPAs that the Devices page changed: {UserID}: devices after a session was
// created or revoked (03 §12.5) and after a device was linked, renamed or revoked (M2 02 §8).
func (a *API) devicesChanged(id store.UserID) {
	a.d.Signal.Notify(NotifyTarget{UserID: id}, protocol.TopicDevices)
}

// postLogin is POST /api/v1/auth/login (03 §12.3 #2, §12.4.2): {username, password} → 200 {user} with the session
// cookie. Errors: 401 invalid_credentials, 403 account_pending or account_disabled, 422 validation_failed, 429
// rate_limited, 503 server_busy. A request that arrives with a session cookie loses that old session (03 §7.4).
func (a *API) postLogin(w http.ResponseWriter, r *http.Request) {
	var req api.LoginRequest
	if err := DecodeJSON(w, r, &req, authBodyLimit); err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.d.Auth.Login(r.Context(), req.Username, req.Password, a.d.Auth.RequestMeta(r))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	a.d.Auth.SetSessionCookie(w, res.Token, res.Session.IdleExpiresAt)
	a.devicesChanged(res.User.ID)
	WriteJSON(w, http.StatusOK, api.UserResponse{User: apiUser(res.User)})
}

// postLogout is POST /api/v1/auth/logout (03 §12.3 #3, §12.4.2): 204 with the cookie cleared. It deletes this
// session and its push subscriptions and closes the session's WebSockets. It is Public and idempotent: without a
// valid session there is nothing to delete, and the answer is the same. The body ({}) is not read.
func (a *API) postLogout(w http.ResponseWriter, r *http.Request) {
	// The cookie alone: no use is recorded for a session that is about to go.
	p, err := a.d.Auth.AuthenticateCookie(r)
	switch {
	case err == nil:
		if err := a.d.Auth.Logout(r.Context(), p, a.d.Auth.RequestMeta(r)); err != nil {
			WriteError(w, r, err)
			return
		}
		a.devicesChanged(p.UserID)
	case errors.Is(err, auth.ErrNoCookie), api.IsCode(err, api.CodeUnauthenticated):
		// Not logged in.
	default:
		WriteError(w, r, err)
		return
	}
	a.d.Auth.ClearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// postSetupCheck is POST /api/v1/auth/setup/check (03 §12.3 #7): {token} → 204, or 404 setup_unavailable (an admin
// exists) or setup_token_invalid.
func (a *API) postSetupCheck(w http.ResponseWriter, r *http.Request) {
	var req api.TokenRequest
	if err := DecodeJSON(w, r, &req, authBodyLimit); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.d.Auth.CheckSetupToken(r.Context(), req.Token, a.d.Auth.RequestMeta(r)); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// postSetupComplete is POST /api/v1/auth/setup/complete (03 §12.3 #8, §7.8): {token, username, password,
// serverName?} → 201 {user} with the session cookie: the first admin, logged in. Errors: 404 setup_unavailable or
// setup_token_invalid, 422 validation_failed, 429 rate_limited, 503 server_busy.
func (a *API) postSetupComplete(w http.ResponseWriter, r *http.Request) {
	var req api.SetupCompleteRequest
	if err := DecodeJSON(w, r, &req, authBodyLimit); err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.d.Auth.CompleteSetup(r.Context(), auth.SetupInput{
		Token:      req.Token,
		Username:   req.Username,
		Password:   req.Password,
		ServerName: req.ServerName,
	}, a.d.Auth.RequestMeta(r))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	a.d.Auth.SetSessionCookie(w, res.Token, res.Session.IdleExpiresAt)
	a.devicesChanged(res.User.ID)
	WriteJSON(w, http.StatusCreated, api.UserResponse{User: apiUser(res.User)})
}

// postResetCheck is POST /api/v1/auth/reset/check (03 §12.3 #9, §7.10): {token} → 200 {username}, the account the
// link resets, so the page says whose password it is before the form is filled in. Errors: 404 reset_token_invalid
// (unknown, used, replaced or expired), 403 account_pending or account_disabled (a live link of an account that
// can't use it yet), 429 rate_limited.
func (a *API) postResetCheck(w http.ResponseWriter, r *http.Request) {
	var req api.TokenRequest
	if err := DecodeJSON(w, r, &req, authBodyLimit); err != nil {
		WriteError(w, r, err)
		return
	}
	username, err := a.d.Auth.CheckPasswordReset(r.Context(), req.Token, a.d.Auth.RequestMeta(r))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, api.ResetCheckResponse{Username: username})
}

// postResetComplete is POST /api/v1/auth/reset/complete (03 §12.3 #10, §7.10): {token, password} → 200 {user} with
// the session cookie: the password is set, the link is used up, and its holder is logged in. Errors: 404
// reset_token_invalid (checked first, before the password and the hash), 403 account_pending or account_disabled,
// 422 validation_failed {password: code}, 429 rate_limited, 503 server_busy. A request that arrives with a session
// cookie loses that old session (03 §7.4).
//
// The user's tabs hear of the new session, and the admins' user lists of the reset that is no longer pending.
func (a *API) postResetComplete(w http.ResponseWriter, r *http.Request) {
	var req api.ResetCompleteRequest
	if err := DecodeJSON(w, r, &req, authBodyLimit); err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.d.Auth.CompletePasswordReset(r.Context(), req.Token, req.Password, a.d.Auth.RequestMeta(r))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	a.d.Auth.SetSessionCookie(w, res.Token, res.Session.IdleExpiresAt)
	a.devicesChanged(res.User.ID)
	a.usersChanged()
	WriteJSON(w, http.StatusOK, api.UserResponse{User: apiUser(res.User)})
}

// postInviteCheck is POST /api/v1/auth/invite/check (03 §12.3 #6, §12.4.2): {token} → 200 InviteInfo, what the
// invite page shows before its form. Errors, each saying why the link doesn't work: 403 registration_closed, 404
// invite_invalid, 410 invite_expired, invite_used_up or invite_revoked; and 429 rate_limited.
func (a *API) postInviteCheck(w http.ResponseWriter, r *http.Request) {
	var req api.TokenRequest
	if err := DecodeJSON(w, r, &req, authBodyLimit); err != nil {
		WriteError(w, r, err)
		return
	}
	info, err := a.d.Auth.CheckInvite(r.Context(), req.Token, a.d.Auth.RequestMeta(r))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, api.InviteInfo{
		ServerName: info.ServerName,
		InvitedBy:  info.InvitedBy,
		ExpiresAt:  info.ExpiresAt,
		UsesLeft:   info.UsesLeft,
	})
}

// postRegister is POST /api/v1/auth/register (03 §12.3 #5, §7.9): {inviteToken?, username, password}.
//   - With an invite: 201 {status: "active", user} with the session cookie; the SPA lands in the default room.
//   - Without one (approval mode only): 202 {status: "pending"} and no cookie; an admin approves or rejects.
//
// Errors: 403 registration_closed or invite_required, 404 invite_invalid, 410 invite_expired, invite_used_up or
// invite_revoked, 409 username_taken or limit_reached {limit: pending_signups}, 422 validation_failed, 429
// rate_limited, 503 server_busy. A request that arrives with a session cookie loses that old session when it
// creates an account (03 §7.4).
func (a *API) postRegister(w http.ResponseWriter, r *http.Request) {
	var req api.RegisterRequest
	if err := DecodeJSON(w, r, &req, authBodyLimit); err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.d.Auth.Register(r.Context(), auth.RegisterInput{
		InviteToken: req.InviteToken,
		Username:    req.Username,
		Password:    req.Password,
	}, a.d.Auth.RequestMeta(r))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Login == nil {
		// A sign-up was requested: every admin's approval queue and user list changed (03 §12.5).
		a.d.Signal.Notify(NotifyTarget{Admins: true}, protocol.TopicAdminApprovals, protocol.TopicAdminUsers)
		WriteJSON(w, http.StatusAccepted, api.RegisterResponse{Status: api.UserStatusPending})
		return
	}
	a.d.Auth.SetSessionCookie(w, res.Login.Token, res.Login.Session.IdleExpiresAt)
	a.devicesChanged(res.Login.User.ID)
	// The invite was used, and there is one more user.
	a.inviteChanged(r, res.Login.User.InviteID, protocol.TopicAdminInvites, protocol.TopicAdminUsers)
	user := apiUser(res.Login.User)
	WriteJSON(w, http.StatusCreated, api.RegisterResponse{Status: api.UserStatusActive, User: &user})
}
