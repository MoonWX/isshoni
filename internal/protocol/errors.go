package protocol

import "slices"

// Error is the payload of an error message (01 §8.13). It is a reply when Re is set on the envelope (scope request),
// otherwise a notification. It carries codes, never English text; Params holds machine-readable values for i18n
// interpolation.
type Error struct {
	Code         ErrorCode      `json:"code"`
	Retryable    bool           `json:"retryable"`
	Scope        ErrorScope     `json:"scope"`
	RetryAfterMs int            `json:"retryAfterMs,omitempty"`
	ShareID      string         `json:"shareId,omitempty"`
	RoomID       string         `json:"roomId,omitempty"`
	PC           PCKind         `json:"pc,omitempty"`
	Gen          uint32         `json:"gen,omitempty"`
	Neg          uint32         `json:"neg,omitempty"`
	Params       map[string]any `json:"params,omitempty"` // machine-readable values for i18n interpolation; never prose
}

// ErrorScope says what an error affects, and so what an older client does with a code it doesn't know (01 §12.3).
type ErrorScope string

const (
	ErrorScopeRequest      ErrorScope = "request"      // only the request `re` failed (never used for notifications)
	ErrorScopeSubscription ErrorScope = "subscription" // reserved: no M1 code uses it; subscribe.status reports subscriptions
	ErrorScopeShare        ErrorScope = "share"        // one of the user's shares failed or can't be published
	ErrorScopePC           ErrorScope = "pc"           // negotiation (pc, gen, neg) failed
	ErrorScopeRoom         ErrorScope = "room"         // the connection is no longer in the room
	ErrorScopeConnection   ErrorScope = "connection"   // the server closes the socket next; retryable = reconnect helps
	ErrorScopeSession      ErrorScope = "session"      // credentials are gone; go to the login page
)

// Valid reports whether s is a known scope.
func (s ErrorScope) Valid() bool {
	switch s {
	case ErrorScopeRequest, ErrorScopeSubscription, ErrorScopeShare, ErrorScopePC, ErrorScopeRoom,
		ErrorScopeConnection, ErrorScopeSession:
		return true
	}
	return false
}

// ErrorCode is a stable snake_case error code (01 §12.1). A code is never renamed or reused with another meaning.
// 05 maps each to errors.<code> in en.json. Codes shared with 03's REST table (bad_request, unauthenticated,
// forbidden, rate_limited, internal, account_disabled, room_not_found, server_shutdown) mean the same in both.
type ErrorCode string

const (
	ErrorCodeBadMessage          ErrorCode = "bad_message"            // frame isn't a JSON object, or the envelope is invalid
	ErrorCodeHelloRequired       ErrorCode = "hello_required"         // first message isn't hello
	ErrorCodeHelloTimeout        ErrorCode = "hello_timeout"          // no hello within 10 s
	ErrorCodeIdleTimeout         ErrorCode = "idle_timeout"           // no frame (message or pong) received for 45 s
	ErrorCodeProtocolUnsupported ErrorCode = "protocol_unsupported"   // no common version; params {serverMin, serverMax, serverVersion}
	ErrorCodeClientOutdated      ErrorCode = "client_outdated"        // native client below minClientVersion
	ErrorCodeUnauthenticated     ErrorCode = "unauthenticated"        // no valid cookie and no valid bearer
	ErrorCodeSessionRevoked      ErrorCode = "session_revoked"        // every 03 revocation reason except account_disabled
	ErrorCodeAccountDisabled     ErrorCode = "account_disabled"       // admin disabled the account
	ErrorCodeTooManyConnections  ErrorCode = "too_many_connections"   // more than 16 connections for this user
	ErrorCodeRateLimited         ErrorCode = "rate_limited"           // a rate limit; retryAfterMs
	ErrorCodeSlowConnection      ErrorCode = "slow_connection"        // send queue overflow
	ErrorCodeReplaced            ErrorCode = "replaced"               // the same connection resumed on another socket
	ErrorCodeServerShutdown      ErrorCode = "server_shutdown"        // the hub is stopping (after server.shutdown)
	ErrorCodeInternal            ErrorCode = "internal"               // unexpected server error; params {ref}
	ErrorCodeBadRequest          ErrorCode = "bad_request"            // payload invalid; params {field, reason}
	ErrorCodeUnknownType         ErrorCode = "unknown_type"           // a request type the server doesn't know
	ErrorCodeMessageTooLarge     ErrorCode = "message_too_large"      // a non-SDP message over 64 KiB after hello; agent.send payload over 16 KiB
	ErrorCodeForbidden           ErrorCode = "forbidden"              // role, ownership or CanJoin check failed
	ErrorCodeFeatureDisabled     ErrorCode = "feature_disabled"       // a message behind a feature that isn't active
	ErrorCodeNotInRoom           ErrorCode = "not_in_room"            // share or PC message without a room
	ErrorCodeRoomNotFound        ErrorCode = "room_not_found"         // unknown, deleted or malformed room id
	ErrorCodeRoomFull            ErrorCode = "room_full"              // admin soft limit; params {limit}
	ErrorCodeKicked              ErrorCode = "kicked"                 // later: reserved for an admin kick
	ErrorCodeRoomClosed          ErrorCode = "room_closed"            // an admin deleted the room
	ErrorCodeShareNotFound       ErrorCode = "share_not_found"        // share.update of an unknown share
	ErrorCodeShareLimit          ErrorCode = "share_limit"            // params {limit, per: "user"|"room"}
	ErrorCodeCodecNotSupported   ErrorCode = "codec_not_supported"    // publisher offered no usable H.264
	ErrorCodeSDPInvalid          ErrorCode = "sdp_invalid"            // SDP failed to apply (pc, gen, neg)
	ErrorCodeStaleNegotiation    ErrorCode = "stale_negotiation"      // pub offer with an older gen or neg (pc, gen, neg)
	ErrorCodeAgentTargetNotFound ErrorCode = "agent_target_not_found" // no same-user connection matches
)

// errorInfo is one code of the catalog (01 §12.1): whether it is retryable, and the close code that follows it in
// scope connection or session (0 = the code is never sent with that scope, or does not close).
type errorInfo struct {
	retryable bool
	connClose CloseCode
	sessClose CloseCode
}

// errorCodes lists the catalog in its order (01 §12.1).
var errorCodes = []ErrorCode{
	ErrorCodeBadMessage, ErrorCodeHelloRequired, ErrorCodeHelloTimeout, ErrorCodeIdleTimeout,
	ErrorCodeProtocolUnsupported, ErrorCodeClientOutdated, ErrorCodeUnauthenticated, ErrorCodeSessionRevoked,
	ErrorCodeAccountDisabled, ErrorCodeTooManyConnections, ErrorCodeRateLimited, ErrorCodeSlowConnection,
	ErrorCodeReplaced, ErrorCodeServerShutdown, ErrorCodeInternal, ErrorCodeBadRequest, ErrorCodeUnknownType,
	ErrorCodeMessageTooLarge, ErrorCodeForbidden, ErrorCodeFeatureDisabled, ErrorCodeNotInRoom,
	ErrorCodeRoomNotFound, ErrorCodeRoomFull, ErrorCodeKicked, ErrorCodeRoomClosed, ErrorCodeShareNotFound,
	ErrorCodeShareLimit, ErrorCodeCodecNotSupported, ErrorCodeSDPInvalid, ErrorCodeStaleNegotiation,
	ErrorCodeAgentTargetNotFound,
}

var errorCatalog = map[ErrorCode]errorInfo{
	ErrorCodeBadMessage:          {retryable: false, connClose: CloseCodeProtocolViolation},
	ErrorCodeHelloRequired:       {retryable: false, connClose: CloseCodeProtocolViolation},
	ErrorCodeHelloTimeout:        {retryable: true, connClose: CloseCodeTimeout},
	ErrorCodeIdleTimeout:         {retryable: true, connClose: CloseCodeTimeout},
	ErrorCodeProtocolUnsupported: {retryable: false, connClose: CloseCodeVersionUnsupported},
	ErrorCodeClientOutdated:      {retryable: false, connClose: CloseCodeVersionUnsupported},
	ErrorCodeUnauthenticated:     {retryable: false, sessClose: CloseCodeUnauthenticated},
	ErrorCodeSessionRevoked:      {retryable: false, sessClose: CloseCodeUnauthenticated},
	ErrorCodeAccountDisabled:     {retryable: false, sessClose: CloseCodeForbidden},
	ErrorCodeTooManyConnections:  {retryable: false, connClose: CloseCodeRateLimited},
	ErrorCodeRateLimited:         {retryable: true, connClose: CloseCodeRateLimited}, // scopes request and pc don't close
	ErrorCodeSlowConnection:      {retryable: true, connClose: CloseCodeSlowConnection},
	ErrorCodeReplaced:            {retryable: false, connClose: CloseCodeReplaced},
	ErrorCodeServerShutdown:      {retryable: true, connClose: CloseCodeServiceRestart},
	ErrorCodeInternal:            {retryable: true, connClose: CloseCodeInternalError},
	ErrorCodeBadRequest:          {retryable: false, connClose: CloseCodeProtocolViolation}, // scope connection: a second hello
	ErrorCodeUnknownType:         {retryable: false},
	ErrorCodeMessageTooLarge:     {retryable: false},
	ErrorCodeForbidden:           {retryable: false},
	ErrorCodeFeatureDisabled:     {retryable: false},
	ErrorCodeNotInRoom:           {retryable: false},
	ErrorCodeRoomNotFound:        {retryable: false},
	ErrorCodeRoomFull:            {retryable: true},
	ErrorCodeKicked:              {retryable: false}, // scope room; scope request (the rejoin block) is retryable, see NewError
	ErrorCodeRoomClosed:          {retryable: false},
	ErrorCodeShareNotFound:       {retryable: false},
	ErrorCodeShareLimit:          {retryable: false},
	ErrorCodeCodecNotSupported:   {retryable: false},
	ErrorCodeSDPInvalid:          {retryable: false},
	ErrorCodeStaleNegotiation:    {retryable: false},
	ErrorCodeAgentTargetNotFound: {retryable: false},
}

// ErrorCodes returns every error code of the catalog (01 §12.1), in catalog order.
func ErrorCodes() []ErrorCode { return slices.Clone(errorCodes) }

// Valid reports whether c is a code of the catalog.
func (c ErrorCode) Valid() bool {
	_, ok := errorCatalog[c]
	return ok
}

// NewError builds an Error with the retryable flag from the catalog (§12.1) for code and scope. An error caused by
// a pc.* notification has scope pc whichever row its code comes from, so the flag depends on the code alone, except
// kicked (reserved): scope room is final, scope request (the rejoin block) is retryable. Unknown codes are not
// retryable.
func NewError(code ErrorCode, scope ErrorScope) Error {
	retryable := errorCatalog[code].retryable
	if code == ErrorCodeKicked && scope == ErrorScopeRequest {
		retryable = true
	}
	return Error{Code: code, Retryable: retryable, Scope: scope}
}

// Error implements the error interface, so MediaPeer and hub code can return a *protocol.Error. The text is for
// logs only; it holds the code and scope, never params.
func (e *Error) Error() string {
	return "protocol: error " + string(e.Code) + " (scope " + string(e.Scope) + ")"
}

// CloseCode is a WebSocket close code (01 §12.2). The preceding error (scope connection or session) decides the
// client's action; the close code is the fallback.
type CloseCode int

const (
	CloseCodeNormal             CloseCode = 1000 // client logout or page unload; the server skips grace
	CloseCodeGoingAway          CloseCode = 1001 // the browser's own close on unload, Go clients, server stop
	CloseCodeUnsupportedData    CloseCode = 1003 // binary frame
	CloseCodeAbnormal           CloseCode = 1006 // no close frame (network); never sent
	CloseCodeMessageTooBig      CloseCode = 1009 // over the socket read limit (coder/websocket)
	CloseCodeInternalError      CloseCode = 1011 // internal error
	CloseCodeServiceRestart     CloseCode = 1012 // service restart (server_shutdown)
	CloseCodeProtocolViolation  CloseCode = 4400 // bad_message, hello_required, a second hello
	CloseCodeUnauthenticated    CloseCode = 4401 // unauthenticated, session_revoked
	CloseCodeForbidden          CloseCode = 4403 // account_disabled
	CloseCodeTimeout            CloseCode = 4408 // hello_timeout, idle_timeout
	CloseCodeReplaced           CloseCode = 4409 // replaced by a resume on another socket
	CloseCodeVersionUnsupported CloseCode = 4426 // protocol_unsupported, client_outdated
	CloseCodeRateLimited        CloseCode = 4429 // rate_limited (connection), too_many_connections
	CloseCodeSlowConnection     CloseCode = 4503 // slow_connection (send queue full)
)

// CloseCodeFor returns the close code that follows an error with this code and scope (the "Close" column of
// 01 §12.1), or 0 when the error does not close the socket.
func CloseCodeFor(code ErrorCode, scope ErrorScope) CloseCode {
	switch scope {
	case ErrorScopeConnection:
		return errorCatalog[code].connClose
	case ErrorScopeSession:
		return errorCatalog[code].sessClose
	}
	return 0
}

// FieldError is returned by Validate methods (and Decode); the hub maps it to bad_request with params
// {field, reason}.
type FieldError struct {
	Field  string // JSON path, e.g. "subs[3].video"
	Reason string // required|invalid|too_long|too_many|duplicate
}

// Values of FieldError.Reason (params.reason of bad_request). Single-line consts: tygo turns a const group whose
// names share a prefix into a TS union type, which these don't need.

const FieldRequired = "required"

const FieldInvalid = "invalid"

const FieldTooLong = "too_long"

const FieldTooMany = "too_many"

const FieldDuplicate = "duplicate"

func (e *FieldError) Error() string {
	return "protocol: field " + e.Field + ": " + e.Reason
}

// BadRequest returns the bad_request error the hub sends for e: params {field, reason}, with the given scope
// (request, pc for pc.* notifications, connection for hello).
func (e *FieldError) BadRequest(scope ErrorScope) Error {
	pe := NewError(ErrorCodeBadRequest, scope)
	pe.Params = map[string]any{"field": e.Field, "reason": e.Reason}
	return pe
}
