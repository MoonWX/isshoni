package sfu

import (
	"errors"
	"time"
)

// Error codes (02 §6.3). They are stable and never sent on the wire: 01's sfuplane maps each one to a protocol
// ErrorCode. A code is never renamed or reused.
const (
	CodeClosed               = "sfu.closed"                 // Conn or SFU closed
	CodeBusy                 = "sfu.busy"                   // actor queue full for 5 s (retryable)
	CodeRoleForbidden        = "sfu.role_forbidden"         // the Conn's role can't do that
	CodeBadPC                = "sfu.bad_pc"                 // method not valid for that PC kind, or no such PC
	CodeBadSDP               = "sfu.bad_sdp"                // unparsable, data channel, too many m-lines, too large
	CodeNoH264               = "sfu.no_h264"                // a video m-line offers no H.264 packetization-mode=1
	CodeBadRID               = "sfu.bad_rid"                // a rid outside {f,q} (M1), or more than 2
	CodeUnknownTrack         = "sfu.unknown_track"          // a malformed binding in tracks (a missing or foreign one isn't)
	CodeStaleAnswer          = "sfu.stale_answer"           // gen/neg don't match the outstanding sub offer
	CodeStaleOffer           = "sfu.stale_offer"            // pub offer with a lower gen, or a lower neg in this gen
	CodePCLimit              = "sfu.pc_limit"               // a third PC or a second PC of the same kind
	CodePCRateLimited        = "sfu.pc_rate_limited"        // too many client-caused PC creations (retryable)
	CodeShareNotFound        = "sfu.share_not_found"        // unknown or ended share
	CodeNotOwner             = "sfu.not_owner"              // the share belongs to another Conn
	CodeTooManyShares        = "sfu.too_many_shares"        // 4 per participant
	CodeTooManySubscriptions = "sfu.too_many_subscriptions" // 256 per Conn, 64 items per call
	CodeProbeLimit           = "sfu.probe_limit"            // 20 probes running (retryable)
	CodeTransportDisabled    = "sfu.transport_disabled"     // the probe's transport has no mux
	CodeInternal             = "sfu.internal"               // unexpected Pion error (retryable)
)

// Error is the error type of every exported SFU method that can fail (02 §6.3). Callers match it with errors.As and
// switch on Code, or use errors.Is with an *Error that has only Code set.
type Error struct {
	Code      string // one of the Code* constants
	Retryable bool
	// Share is set for no_h264, unknown_track and bad_rid when the failing m-section maps to a share.
	Share ShareID
	// RetryAfter is set for pc_rate_limited, busy (1 s) and probe_limit.
	RetryAfter time.Duration

	msg   string // for logs only; never sent, never holds SDP, candidates or other client data
	cause error  // what Unwrap returns: ErrNotImplemented, or nil
}

// ErrNotImplemented is wrapped by the error of an API method that a later slice of the plan fills in (README §4
// "Interfaces first"): the whole interface of 02 §6 is declared from the first core slice on, so adapters never
// chase a moving one. The error itself is an *Error with CodeInternal that is not retryable, so 01's sfuplane needs
// no special case. The sentinel goes away with the last such method.
var ErrNotImplemented = errors.New("sfu: not implemented yet")

// errNotImplemented returns the error of a method that the named README slice implements.
func errNotImplemented(method, slice string) *Error {
	return &Error{
		Code:  CodeInternal,
		msg:   method + " is not implemented yet (README " + slice + ")",
		cause: ErrNotImplemented,
	}
}

// errClosed is the error of every Conn method after Close, and of Join after SFU.Close.
func errClosed(what string) *Error { return newError(CodeClosed, what+" is closed") }

// errBusy is the error of a signal call that could not reach the Conn's actor in time (02 §5.4).
func errBusy() *Error {
	e := newError(CodeBusy, "the connection's command queue is full")
	e.RetryAfter = busyRetryAfter
	return e
}

// newError returns an *Error for code with the retryable flag of 02 §6.3 and a log message.
func newError(code, msg string) *Error {
	return &Error{Code: code, Retryable: retryableCode(code), msg: msg}
}

// retryableCode reports the Retryable column of 02 §6.3.
func retryableCode(code string) bool {
	switch code {
	case CodeBusy, CodePCRateLimited, CodeProbeLimit, CodeInternal:
		return true
	}
	return false
}

// Error returns the code and, when there is one, the log message.
func (e *Error) Error() string {
	if e.msg == "" {
		return e.Code
	}
	return e.Code + ": " + e.msg
}

// Unwrap returns ErrNotImplemented for the error of a method that a later slice fills in, else nil.
func (e *Error) Unwrap() error { return e.cause }

// Is makes errors.Is(err, &sfu.Error{Code: sfu.CodeBadSDP}) match any *Error with that code.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}
