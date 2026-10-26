package httpapi

import (
	"errors"
	"net/http"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/auth"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// The device routes of M2 02 §7.1 (README WA47): the RFC 8628 device flow (#44–#47), both grants of the token
// endpoint (#45), the in-app password fallback (#48), the app's own sign-out (#49) and the rename of a device (#52).
// auth.Service owns every rule, throttle, audit row, connection close and admin alert; the handlers decode the
// request, call it, and tell the user's SPAs after the call (03 §12.5, M2 02 §8): {UserID}: devices after a device
// was linked (a link may also evict the least recently seen device beyond the cap), renamed or revoked, and after a
// refresh token's reuse revoked one. Approving and denying a code notify nothing: the approval page has the answer.
// Every body is limited to 16 KiB (the chain's body limit for /api/v1/device/).

// postDeviceCode is POST /api/v1/device/code (#44, Public, M2 02 §5.2): {clientKind, deviceName, os, appVersion} →
// 200 DeviceCodeResponse. Errors: 429 rate_limited (auth-ip, device-code-ip, device-code-ip48), 422
// validation_failed {clientKind|os|deviceName|appVersion}, 503 server_busy with Retry-After: 60 (the cap of 5,000
// pending codes).
func (a *API) postDeviceCode(w http.ResponseWriter, r *http.Request) {
	var req api.DeviceCodeRequest
	if err := DecodeJSON(w, r, &req, authBodyLimit); err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.d.Auth.StartDeviceFlow(r.Context(), req, a.d.Auth.RequestMeta(r))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, res)
}

// postDeviceToken is POST /api/v1/device/token (#45, Public, M2 02 §5.4, §6.2): both grants → 200
// DeviceTokenResponse. Errors: 400 authorization_pending, slow_down {interval}, access_denied, expired_token,
// invalid_grant {reason?}; 422 validation_failed {grantType|exchangeId|appVersion}; 429 rate_limited.
//
// A device_code grant that succeeds linked a device: {UserID}: devices. A refresh token that came back after it was
// spent revoked its device (*auth.RefreshReusedError, which unwraps to invalid_grant {reason: reused}): the owner's
// SPAs are told too, and the client gets the invalid_grant alone. A plain rotation notifies nobody.
func (a *API) postDeviceToken(w http.ResponseWriter, r *http.Request) {
	var req api.DeviceTokenRequest
	if err := DecodeJSON(w, r, &req, authBodyLimit); err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.d.Auth.DeviceToken(r.Context(), req, a.d.Auth.RequestMeta(r))
	if err != nil {
		if reused, ok := errors.AsType[*auth.RefreshReusedError](err); ok {
			a.devicesChanged(reused.UserID)
		}
		WriteError(w, r, err)
		return
	}
	if req.GrantType == api.GrantTypeDeviceCode {
		a.devicesChanged(store.UserID(res.User.ID))
	}
	WriteJSON(w, http.StatusOK, res)
}

// postDeviceLookup is POST /api/v1/device/lookup (#46, UserSession, M2 02 §5.3): {userCode} → 200 DeviceLookup, the
// pending request behind the code for the approval page. Errors: 403 forbidden {credential: session} (a bearer: the
// chain, and the service again), 404 device_code_invalid, 422 validation_failed {userCode}, 429 rate_limited
// (device-lookup).
func (a *API) postDeviceLookup(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	var req api.DeviceLookupRequest
	if err := DecodeJSON(w, r, &req, authBodyLimit); err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.d.Auth.LookupUserCode(r.Context(), p, req.UserCode, a.d.Auth.RequestMeta(r))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, res)
}

// postDeviceDecide is POST /api/v1/device/decide (#47, UserSession, M2 02 §5.3): {userCode, approve, deviceName?} →
// 204, also for a repeat of the same decision by the same user. Errors as #46, plus 422 validation_failed
// {deviceName}. It notifies nobody: the device appears at the app's next poll, which notifies.
func (a *API) postDeviceDecide(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	var req api.DeviceDecideRequest
	if err := DecodeJSON(w, r, &req, authBodyLimit); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.d.Auth.DecideUserCode(r.Context(), p, req, a.d.Auth.RequestMeta(r)); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// postDevicePassword is POST /api/v1/device/password (#48, Public, M2 02 §14): {username, password, clientKind,
// deviceName, os, appVersion} → 200 DeviceTokenResponse, a device linked with the account's password and login's
// order and throttles. No web session is created and no cookie set. Errors as #2: 401 invalid_credentials, 403
// account_pending or account_disabled, 422 validation_failed {username|password|clientKind|os|deviceName|appVersion},
// 429 rate_limited, 503 server_busy. Go sends it, never the SPA, so it needs no CORS of its own.
func (a *API) postDevicePassword(w http.ResponseWriter, r *http.Request) {
	var req api.DevicePasswordRequest
	if err := DecodeJSON(w, r, &req, authBodyLimit); err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.d.Auth.PasswordDeviceLogin(r.Context(), req, a.d.Auth.RequestMeta(r))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	a.devicesChanged(store.UserID(res.User.ID))
	WriteJSON(w, http.StatusOK, res)
}

// postDeviceRevoke is POST /api/v1/device/revoke (#49, UserDevice, M2 02 §7.2): the app signs itself out → 204. The
// device goes with its tokens and push subscriptions, device.revoked {via: "app"} is written and its connections
// close (device_revoked). A cookie is 403 forbidden {credential: device}. The body ({}) is not read.
func (a *API) postDeviceRevoke(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	if err := a.d.Auth.RevokeOwnDevice(r.Context(), p, a.d.Auth.RequestMeta(r)); err != nil {
		WriteError(w, r, err)
		return
	}
	a.devicesChanged(p.UserID)
	w.WriteHeader(http.StatusNoContent)
}

// patchDevice is PATCH /api/v1/me/devices/{id} (#52, User, M2 02 §7.2): {name} → 200 {device}, one of the caller's
// own devices under its new name (a web session renames any of them, a device itself or another of its user's).
// device.renamed {from, to} is written; a name equal to the current one changes nothing and writes nothing.
// Errors: 404 not_found (not the caller's), 422 validation_failed {name}. The user's SPAs are told after every 200,
// which costs an unchanged name one refetch.
func (a *API) patchDevice(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteError(w, r, api.NewError(api.CodeUnauthenticated))
		return
	}
	var req api.RenameDeviceRequest
	if err := DecodeJSON(w, r, &req, otherBodyLimit); err != nil {
		WriteError(w, r, err)
		return
	}
	id := store.DeviceID(r.PathValue("id"))
	dev, err := a.d.Auth.RenameDevice(r.Context(), p, id, req.Name, a.d.Auth.RequestMeta(r))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	a.devicesChanged(p.UserID)
	WriteJSON(w, http.StatusOK, api.DeviceResponse{Device: apiDevice(dev, p)})
}

// apiDevice is a device row as the caller sees it (03 §12.4.3, M2 02 §7.2): current marks the caller's own device,
// so it is true only for a device's principal and its own row.
func apiDevice(d store.Device, p auth.Principal) api.DeviceInfo {
	return api.DeviceInfo{
		ID:         string(d.ID),
		Name:       d.Name,
		ClientKind: d.ClientKind,
		OS:         d.OS,
		AppVersion: d.AppVersion,
		CreatedAt:  d.CreatedAt,
		LastSeenAt: d.LastSeenAt,
		LastIP:     d.LastIP,
		Current:    isOwnDevice(p, d.ID),
	}
}

// isOwnDevice reports whether the principal is the device id.
func isOwnDevice(p auth.Principal, id store.DeviceID) bool {
	return p.Method == auth.MethodBearer && p.DeviceID != "" && p.DeviceID == id
}
