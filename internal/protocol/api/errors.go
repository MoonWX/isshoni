package api

import (
	"errors"
	"net/http"
	"slices"
)

// Error is both the REST error body and the Go error value that store, auth, httpapi and 04's handlers return
// (03 §12.2). httpapi.WriteError sends it as ErrorResponse with the status StatusOf(Code), and a Retry-After header
// when RetryAfter is set. It never carries English text.
type Error struct {
	Code       string            `json:"code"`
	Fields     map[string]string `json:"fields,omitempty"`                                     // validation_failed: field JSON name → Field* code
	Params     map[string]any    `json:"params,omitempty" tstype:"{ [key: string]: unknown }"` // e.g. {"limit": "rooms"} (Param* keys); never prose
	RetryAfter int               `json:"retryAfter,omitempty"`                                 // seconds; also sent as the Retry-After header
	RequestID  string            `json:"requestId,omitempty"`                                  // 04's request ID, set on 500 internal (bug reports)
}

// Error implements the error interface.
func (e *Error) Error() string { return "api: " + e.Code }

// ErrorResponse is the only REST error shape: {"error": {code, fields?, params?, retryAfter?, requestId?}}. 03's
// /api/v1 routes, 04's routes and 04's global middleware all use it; the admin socket wraps it (04 §12.1).
type ErrorResponse struct {
	Error Error `json:"error"`
}

// NewError returns an *Error with only a code.
func NewError(code string) *Error { return &Error{Code: code} }

// IsCode reports whether err is, or wraps, an *Error with the given code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// StatusOf returns the HTTP status for an error code (the table of 03 §12.2, including 04's rows). Unknown codes
// get 500.
func StatusOf(code string) int {
	if s, ok := statusByCode[code]; ok {
		return s
	}
	return http.StatusInternalServerError
}

// Codes returns every error code of the table, sorted.
func Codes() []string {
	out := make([]string, 0, len(statusByCode))
	for c := range statusByCode {
		out = append(out, c)
	}
	slices.Sort(out)
	return out
}

// Error codes (03 §12.2). A code is stable forever: it is never renamed or reused with another meaning. Every code is
// M1 unless marked; (04) marks the rows of 04's routes and middleware (04 §9.4). Eight codes are shared with 01's
// WebSocket errors and must mean the same thing there: bad_request, unauthenticated, forbidden, account_disabled,
// room_not_found, rate_limited, internal and server_shutdown.
const (
	CodeBadRequest           = "bad_request"            // 400: malformed JSON (incl. trailing data), or a bad path or query parameter
	CodeValidationFailed     = "validation_failed"      // 422: see Error.Fields
	CodeUnsupportedMediaType = "unsupported_media_type" // 415: unsafe request without Content-Type: application/json
	CodePayloadTooLarge      = "payload_too_large"      // 413: body over the limit
	CodeMethodNotAllowed     = "method_not_allowed"     // 405: known /api/v1 path, wrong method (Allow header lists the methods)
	CodeUnauthenticated      = "unauthenticated"        // 401: no valid session (or bearer)
	//nolint:gosec // G101 false positive: an error code, not a credential.
	CodeInvalidCredentials   = "invalid_credentials"    // 401: wrong username or password (the same answer for unknown users)
	CodeWrongPassword        = "wrong_password"         // 403: re-authentication failed (currentPassword)
	CodeForbidden            = "forbidden"              // 403: not allowed for this role
	CodeCSRFFailed           = "csrf_failed"            // 403: cross-origin unsafe request
	CodeAccountPending       = "account_pending"        // 403: correct password, but the sign-up awaits approval
	CodeAccountDisabled      = "account_disabled"       // 403: correct password, but the account is disabled
	CodeRegistrationClosed   = "registration_closed"    // 403: registration mode closed
	CodeInviteRequired       = "invite_required"        // 403: registration mode invite and no invite token
	CodeNotFound             = "not_found"              // 404: generic; also an unknown /api/ route (03 inside /api/v1, 04 elsewhere)
	CodeUserNotFound         = "user_not_found"         // 404
	CodeRoomNotFound         = "room_not_found"         // 404
	CodeInviteInvalid        = "invite_invalid"         // 404: unknown invite token
	CodeInviteExpired        = "invite_expired"         // 410
	CodeInviteUsedUp         = "invite_used_up"         // 410
	CodeInviteRevoked        = "invite_revoked"         // 410
	CodeSetupUnavailable     = "setup_unavailable"      // 404: an admin already exists
	CodeSetupTokenInvalid    = "setup_token_invalid"    // 404: unknown, replaced or expired setup token
	CodeResetTokenInvalid    = "reset_token_invalid"    // 404: unknown, used or expired reset token
	CodeUsernameTaken        = "username_taken"         // 409
	CodeRoomNameTaken        = "room_name_taken"        // 409
	CodeLastAdmin            = "last_admin"             // 409: would leave no active admin
	CodeSelfActionForbidden  = "self_action_forbidden"  // 409: use the self-service endpoint instead
	CodeRoomIsDefault        = "room_is_default"        // 409: Lounge can't be deleted
	CodeLimitReached         = "limit_reached"          // 409: params.limit is a LimitKind
	CodeSettingLocked        = "setting_locked"         // 409: params.field names the setting pinned by config
	CodePushEndpointRejected = "push_endpoint_rejected" // 422: params.reason is a PushRejectReason
	CodePushUnavailable      = "push_unavailable"       // 503: push is off (push.enabled = false, 04)
	CodeRateLimited          = "rate_limited"           // 429: Error.RetryAfter
	CodeServerBusy           = "server_busy"            // 503: hash queue full or the auth-hash budget is empty; Error.RetryAfter
	CodeInternal             = "internal"               // 500: logged with Error.RequestID (04), never with details on the wire

	CodeBadSDP              = "bad_sdp"              // 400 (04): /conntest offer isn't a data-channel-only SDP
	CodeTransportDisabled   = "transport_disabled"   // 409 (04): /conntest transport has no listener; params.transport
	CodeDoctorBusy          = "doctor_busy"          // 429 (04): a doctor run is in progress or ran < 10 s ago; Error.RetryAfter
	CodeNotReady            = "not_ready"            // 503 (04): the media plane isn't ready yet
	CodeServerShutdown      = "server_shutdown"      // 503 (04): the server is stopping or restarting; Retry-After: 5
	CodeBackupInvalid       = "backup_invalid"       // 400 (04, admin socket only): the archive or DB file failed validation
	CodeBackupNewer         = "backup_newer"         // 409 (04, admin socket only): the backup's schema is newer than this binary
	CodeRestoreInProgress   = "restore_in_progress"  // 409 (04, admin socket only)
	CodeInsufficientStorage = "insufficient_storage" // 507 (04, admin socket only): not enough free disk space

	CodeInvalidToken         = "invalid_token"         // 401, later (M2): bearer access token expired or unknown: refresh
	CodeAuthorizationPending = "authorization_pending" // 400, later (M2): RFC 8628
	CodeSlowDown             = "slow_down"             // 400, later (M2): RFC 8628
	CodeAccessDenied         = "access_denied"         // 400, later (M2): RFC 8628
	CodeExpiredToken         = "expired_token"         // 400, later (M2): RFC 8628
	CodeInvalidGrant         = "invalid_grant"         // 400, later (M2): OAuth refresh token reuse or unknown grant
	CodeDeviceCodeInvalid    = "device_code_invalid"   // 404, later (M2): unknown or expired user code
)

var statusByCode = map[string]int{
	CodeBadRequest:           http.StatusBadRequest,
	CodeValidationFailed:     http.StatusUnprocessableEntity,
	CodeUnsupportedMediaType: http.StatusUnsupportedMediaType,
	CodePayloadTooLarge:      http.StatusRequestEntityTooLarge,
	CodeMethodNotAllowed:     http.StatusMethodNotAllowed,
	CodeUnauthenticated:      http.StatusUnauthorized,
	CodeInvalidCredentials:   http.StatusUnauthorized,
	CodeWrongPassword:        http.StatusForbidden,
	CodeForbidden:            http.StatusForbidden,
	CodeCSRFFailed:           http.StatusForbidden,
	CodeAccountPending:       http.StatusForbidden,
	CodeAccountDisabled:      http.StatusForbidden,
	CodeRegistrationClosed:   http.StatusForbidden,
	CodeInviteRequired:       http.StatusForbidden,
	CodeNotFound:             http.StatusNotFound,
	CodeUserNotFound:         http.StatusNotFound,
	CodeRoomNotFound:         http.StatusNotFound,
	CodeInviteInvalid:        http.StatusNotFound,
	CodeInviteExpired:        http.StatusGone,
	CodeInviteUsedUp:         http.StatusGone,
	CodeInviteRevoked:        http.StatusGone,
	CodeSetupUnavailable:     http.StatusNotFound,
	CodeSetupTokenInvalid:    http.StatusNotFound,
	CodeResetTokenInvalid:    http.StatusNotFound,
	CodeUsernameTaken:        http.StatusConflict,
	CodeRoomNameTaken:        http.StatusConflict,
	CodeLastAdmin:            http.StatusConflict,
	CodeSelfActionForbidden:  http.StatusConflict,
	CodeRoomIsDefault:        http.StatusConflict,
	CodeLimitReached:         http.StatusConflict,
	CodeSettingLocked:        http.StatusConflict,
	CodePushEndpointRejected: http.StatusUnprocessableEntity,
	CodePushUnavailable:      http.StatusServiceUnavailable,
	CodeRateLimited:          http.StatusTooManyRequests,
	CodeServerBusy:           http.StatusServiceUnavailable,
	CodeInternal:             http.StatusInternalServerError,

	CodeBadSDP:              http.StatusBadRequest,
	CodeTransportDisabled:   http.StatusConflict,
	CodeDoctorBusy:          http.StatusTooManyRequests,
	CodeNotReady:            http.StatusServiceUnavailable,
	CodeServerShutdown:      http.StatusServiceUnavailable,
	CodeBackupInvalid:       http.StatusBadRequest,
	CodeBackupNewer:         http.StatusConflict,
	CodeRestoreInProgress:   http.StatusConflict,
	CodeInsufficientStorage: http.StatusInsufficientStorage,

	CodeInvalidToken:         http.StatusUnauthorized,
	CodeAuthorizationPending: http.StatusBadRequest,
	CodeSlowDown:             http.StatusBadRequest,
	CodeAccessDenied:         http.StatusBadRequest,
	CodeExpiredToken:         http.StatusBadRequest,
	CodeInvalidGrant:         http.StatusBadRequest,
	CodeDeviceCodeInvalid:    http.StatusNotFound,
}

// Field codes: the values of Error.Fields for validation_failed (03 §12.2).
const (
	FieldRequired       = "required"
	FieldTooShort       = "too_short"
	FieldTooLong        = "too_long"
	FieldInvalid        = "invalid"
	FieldReserved       = "reserved"
	FieldTooCommon      = "too_common"
	FieldSameAsUsername = "same_as_username"
	FieldOutOfRange     = "out_of_range"
	FieldNotAllowed     = "not_allowed"
)

// Keys of Error.Params.
const (
	ParamLimit     = "limit"     // limit_reached: a LimitKind
	ParamField     = "field"     // setting_locked: the setting's JSON name (Settings)
	ParamReason    = "reason"    // push_endpoint_rejected: a PushRejectReason
	ParamTransport = "transport" // transport_disabled: a Transport
)

// LimitKind is params.limit of limit_reached (03 §12.2).
type LimitKind string

const (
	LimitKindRooms          LimitKind = "rooms"           // 200 rooms per server
	LimitKindInvites        LimitKind = "invites"         // 100 active invites per server
	LimitKindMemberInvites  LimitKind = "member_invites"  // 10 active invites per member
	LimitKindPendingSignups LimitKind = "pending_signups" // 50 pending sign-ups
)

// PushRejectReason is params.reason of push_endpoint_rejected (03 §12.4.6). too_long and bad_keys come from 03's
// handler (body shape); the others from 04's Push.ValidateEndpoint.
type PushRejectReason string

const (
	PushRejectReasonTooLong        PushRejectReason = "too_long"
	PushRejectReasonBadKeys        PushRejectReason = "bad_keys"
	PushRejectReasonNotHTTPS       PushRejectReason = "not_https"
	PushRejectReasonBadPort        PushRejectReason = "bad_port"
	PushRejectReasonUserinfo       PushRejectReason = "userinfo"
	PushRejectReasonIPLiteral      PushRejectReason = "ip_literal"
	PushRejectReasonPrivateAddress PushRejectReason = "private_address"
	PushRejectReasonUnresolvable   PushRejectReason = "unresolvable"
)
