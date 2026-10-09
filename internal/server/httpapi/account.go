package httpapi

import (
	"net/http"
	"slices"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// Self-service (README S58; 03 §12.3 #4 and #12–#18, §12.4.3): what a signed-in user does to the own account, all
// User routes. auth.Service owns every change: the rules, the re-authentication with its throttles, the revocation
// rows of 03 §7.7 (which sessions and devices go, and which connections are closed, with which reason) and the
// audit rows. The handlers decode the request, call it, set or clear the session cookie and tell the SPAs
// (03 §12.5). The two lists are reads of the store.

// useSessionCookie sets the session cookie to a new token, as the only Set-Cookie of the response.
//
// The chain's rotation step runs before the handler and may have set the cookie already, to the token of the daily
// rotation (03 §7.4). The handler's own cookie is the one that counts, and a response should not set one cookie
// twice (RFC 6265 §4.1.1), so the earlier header goes. The API sets no other cookie.
func (a *API) useSessionCookie(w http.ResponseWriter, token string, idleExpires time.Time) {
	w.Header().Del("Set-Cookie")
	a.d.Auth.SetSessionCookie(w, token, idleExpires)
}

// endSessionCookie clears the session cookie, as the only Set-Cookie of the response (see useSessionCookie).
func (a *API) endSessionCookie(w http.ResponseWriter) {
	w.Header().Del("Set-Cookie")
	a.d.Auth.ClearSessionCookie(w)
}

// sessionLive reports whether a session row is unexpired at now (both limits inclusive, like the store's). The
// janitor removes expired rows within the hour (03 §4.7); until then they are no sessions any more.
func sessionLive(s store.Session, now time.Time) bool {
	return !now.After(s.IdleExpiresAt) && !now.After(s.ExpiresAt)
}

// postLogoutEverywhere is POST /api/v1/auth/logout-everywhere (03 §12.3 #4; 03 §7.7 "Log out everywhere"): 204
// with the cookie cleared. Every web session and device of the caller's account is gone, this one included, with
// their push subscriptions, and every connection of the user is closed. The body ({}) is not read.
func (a *API) postLogoutEverywhere(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	if err := a.d.Auth.LogoutEverywhere(r.Context(), p, a.d.Auth.RequestMeta(r)); err != nil {
		WriteError(w, r, err)
		return
	}
	a.sessionChanged(p.UserID)
	a.endSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// postPassword is POST /api/v1/me/password (03 §12.3 #12, §12.4.3; 03 §7.7 "Change own password"):
// {currentPassword, newPassword} → 200 {} with this session's cookie set to a new token. The user's other web
// sessions and every linked device are signed out, and their connections closed. Errors: 403 wrong_password (the
// current password does not match, or is missing), 422 validation_failed {newPassword: code}, 429 rate_limited
// (too many wrong passwords, 03 §7.3), 503 server_busy.
//
// The new token is a rotation of 03 §7.4: the cookie the request arrived with works for 60 s more. This browser's
// other tabs hear of the change (devices) before the response with the new cookie is written, and what they fetch
// under the old cookie in between is still let in.
func (a *API) postPassword(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	var req api.ChangePasswordRequest
	if err := DecodeJSON(w, r, &req, otherBodyLimit); err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.d.Auth.ChangePassword(r.Context(), p, req.CurrentPassword, req.NewPassword, a.d.Auth.RequestMeta(r))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	a.useSessionCookie(w, res.Token, res.Session.IdleExpiresAt)
	a.sessionChanged(p.UserID) // the other sessions are gone: this browser's other tabs refetch the lists
	WriteJSON(w, http.StatusOK, api.Empty{})
}

// postDeleteSelf is POST /api/v1/me/delete (03 §12.3 #13, §12.4.3; 03 §7.7 "Delete user"): {password} → 204 with
// the cookie cleared. The account is gone with everything that hangs on it, its username is free again, and every
// connection of the user is closed. Errors: 403 wrong_password (also for a missing password), 409 last_admin (the
// only active admin can't leave, 03 §7.11), 429 rate_limited, 503 server_busy.
func (a *API) postDeleteSelf(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	var req api.DeleteSelfRequest
	if err := DecodeJSON(w, r, &req, otherBodyLimit); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.d.Auth.DeleteSelf(r.Context(), p, req.Password, a.d.Auth.RequestMeta(r)); err != nil {
		WriteError(w, r, err)
		return
	}
	// A user was deleted: the admins' user list changed (03 §12.5).
	a.d.Signal.Notify(NotifyTarget{Admins: true}, protocol.TopicAdminUsers)
	a.endSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// getSessions is GET /api/v1/me/sessions (03 §12.3 #14, §12.4.3): {sessions}, the caller's web sessions with their
// browser label, creation and last-seen times and last IP. This request's session is marked current and comes
// first; the others follow, the most recently seen first. Expired rows the janitor has not removed yet are left
// out. It reads only.
func (a *API) getSessions(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	var rows []store.Session
	err := a.d.DB.Read(r.Context(), func(q *store.Q) error {
		var err error
		rows, err = q.ListSessions(p.UserID)
		return err
	})
	if err != nil {
		WriteError(w, r, opErr("sessions", err))
		return
	}
	now := a.d.Clock()
	sessions := make([]api.SessionInfo, 0, len(rows))
	for _, s := range rows {
		if !sessionLive(s, now) {
			continue
		}
		sessions = append(sessions, api.SessionInfo{
			ID:         string(s.ID),
			Name:       s.Name,
			CreatedAt:  s.CreatedAt,
			LastSeenAt: s.LastSeenAt,
			LastIP:     s.LastIP,
			Current:    s.ID == p.SessionID,
		})
	}
	current := slices.IndexFunc(sessions, func(s api.SessionInfo) bool { return s.Current })
	if current < 0 {
		// The session went away between the chain's authentication and this read. (M2: a device's principal has
		// no session of its own in the list.)
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	// The store lists by last use; the caller's own session leads whatever its place.
	mine := sessions[current]
	copy(sessions[1:current+1], sessions[:current])
	sessions[0] = mine
	WriteJSON(w, http.StatusOK, api.SessionsResponse{Sessions: sessions})
}

// deleteSession is DELETE /api/v1/me/sessions/{id} (03 §12.3 #15; 03 §7.7 "Revoke one session"): 204; that
// browser is signed out and its connections are closed. A session that is not one of the caller's own (unknown,
// another user's, signed out already) is 404 not_found, which the SPA counts as done.
//
// The Devices page signs this browser out through the logout flow, not here. Should the ID be the caller's own
// session all the same, it is revoked like any other and the cookie is cleared.
func (a *API) deleteSession(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	id := store.SessionID(r.PathValue("id"))
	if err := a.d.Auth.RevokeSession(r.Context(), p, id, a.d.Auth.RequestMeta(r)); err != nil {
		WriteError(w, r, err)
		return
	}
	a.sessionChanged(p.UserID)
	if id == p.SessionID {
		a.endSessionCookie(w)
	}
	w.WriteHeader(http.StatusNoContent)
}

// postRevokeOthers is POST /api/v1/me/sessions/revoke-others (03 §12.3 #16, §12.4.3; 03 §7.7 "Sign out other
// browsers"): 200 {revoked}, the number of browsers that were signed out: the other sessions that GET /me/sessions
// listed. An expired row the janitor has not removed yet goes with them and is not counted. This session stays. The
// body ({}) is not read.
func (a *API) postRevokeOthers(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	n, err := a.d.Auth.RevokeOtherSessions(r.Context(), p, a.d.Auth.RequestMeta(r))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if n > 0 {
		a.sessionChanged(p.UserID)
	}
	WriteJSON(w, http.StatusOK, api.RevokeOthersResponse{Revoked: n})
}

// getDevices is GET /api/v1/me/devices (03 §12.3 #17, §12.4.3): {devices}, the caller's linked apps, the most
// recently seen first. Linking an app is M2, so the list is empty until then. It reads only.
func (a *API) getDevices(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	var rows []store.Device
	err := a.d.DB.Read(r.Context(), func(q *store.Q) error {
		if err := callerLive(q, p); err != nil {
			return err
		}
		var err error
		rows, err = q.ListDevices(p.UserID)
		return err
	})
	if err != nil {
		WriteError(w, r, opErr("devices", err))
		return
	}
	devices := make([]api.DeviceInfo, 0, len(rows))
	for _, d := range rows {
		devices = append(devices, api.DeviceInfo{
			ID:         string(d.ID),
			Name:       d.Name,
			ClientKind: d.ClientKind,
			OS:         d.OS,
			AppVersion: d.AppVersion,
			CreatedAt:  d.CreatedAt,
			LastSeenAt: d.LastSeenAt,
			LastIP:     d.LastIP,
		})
	}
	WriteJSON(w, http.StatusOK, api.DevicesResponse{Devices: devices})
}

// deleteDevice is DELETE /api/v1/me/devices/{id} (03 §12.3 #18; 03 §7.7 "Revoke device"): 204; that app is signed
// out and its connections are closed. A device that is not one of the caller's own is 404 not_found, which is the
// answer for every ID until M2 links the first app.
func (a *API) deleteDevice(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	id := store.DeviceID(r.PathValue("id"))
	if err := a.d.Auth.RevokeDevice(r.Context(), p, id, a.d.Auth.RequestMeta(r)); err != nil {
		WriteError(w, r, err)
		return
	}
	a.sessionChanged(p.UserID)
	w.WriteHeader(http.StatusNoContent)
}
