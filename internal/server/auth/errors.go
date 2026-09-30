package auth

import "errors"

// Field codes of 03 §12.2 returned by the rules in this package. They are wire values: the SPA maps each one to
// fieldErrors.<field>.<code>, so they are never renamed.
const (
	FieldRequired       = "required"
	FieldTooShort       = "too_short"
	FieldTooLong        = "too_long"
	FieldInvalid        = "invalid"
	FieldReserved       = "reserved"
	FieldTooCommon      = "too_common"
	FieldSameAsUsername = "same_as_username"
)

// FieldError reports why one input field failed a rule. Field is the JSON field the rule checks by default
// ("username" or "password"); a caller whose request names the field differently (for example "newPassword") keeps
// Code and uses its own name. The REST layer turns it into api.Error{Code: "validation_failed", Fields: {...}}.
type FieldError struct {
	Field string
	Code  string
}

func (e *FieldError) Error() string { return "auth: " + e.Field + ": " + e.Code }

func fieldErr(field, code string) *FieldError { return &FieldError{Field: field, Code: code} }

// ErrNoCookie is returned by AuthenticateCookie (README S30) when the request carries no session cookie. Only the
// /ws adapter sees it (03 §7.6).
var ErrNoCookie = errors.New("auth: no session cookie")

// errHashBusy means the hash semaphore is full: more than maxHashWaiters callers are waiting, or a caller waited
// longer than maxHashWait (03 §7.2). The service answers 503 server_busy with Retry-After: 5.
var errHashBusy = errors.New("auth: password hashing is busy")

// errBadHash wraps every parse failure of a stored PHC string.
var errBadHash = errors.New("auth: malformed password hash")
