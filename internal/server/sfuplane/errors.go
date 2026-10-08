package sfuplane

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"maps"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/sfu"
)

// This file maps the SFU's errors to the wire (01 §15.4, 02 §6.3). An sfu.* code never goes on the wire.

// sharesPerParticipant is the SFU's guard behind sfu.too_many_shares (02 §12): the limit of the share_limit error
// that it maps to. The hub enforces its own 4 per user first, so the SFU's guard is defense in depth.
const sharesPerParticipant = 4

// wireCode is one row of the error table of 01 §15.4: the wire code of an sfu.* code, and its params.
type wireCode struct {
	code   protocol.ErrorCode
	params map[string]any // copied into each error
}

// wireCodes is the error table of 01 §15.4. It has no row for:
//   - sfu.busy, sfu.internal and any code this package doesn't know (sfu.probe_limit and sfu.transport_disabled
//     among them: only SFU.Probe returns those, to 04's connection test, never to signaling): they become internal;
//   - sfu.stale_answer, which sends nothing. Only HandleAnswer returns it, where no client waits for an answer.
//
// sfu.closed has its row for the calls that must answer; elsewhere it sends nothing either (site.reply).
var wireCodes = map[string]wireCode{
	sfu.CodeRoleForbidden: {code: protocol.ErrorCodeForbidden},
	sfu.CodeNotOwner:      {code: protocol.ErrorCodeForbidden},
	sfu.CodeBadPC: {
		code:   protocol.ErrorCodeBadRequest,
		params: map[string]any{"field": "pc", "reason": protocol.FieldInvalid},
	},
	sfu.CodePCLimit: {
		code:   protocol.ErrorCodeBadRequest,
		params: map[string]any{"field": "pc", "reason": protocol.FieldTooMany},
	},
	sfu.CodeUnknownTrack: {
		code:   protocol.ErrorCodeBadRequest,
		params: map[string]any{"field": "tracks", "reason": protocol.FieldInvalid},
	},
	sfu.CodeTooManySubscriptions: {
		code:   protocol.ErrorCodeBadRequest,
		params: map[string]any{"field": "subs", "reason": protocol.FieldTooMany},
	},
	sfu.CodeBadSDP:        {code: protocol.ErrorCodeSDPInvalid},
	sfu.CodeBadRID:        {code: protocol.ErrorCodeSDPInvalid},
	sfu.CodeStaleOffer:    {code: protocol.ErrorCodeStaleNegotiation},
	sfu.CodeNoH264:        {code: protocol.ErrorCodeCodecNotSupported},
	sfu.CodePCRateLimited: {code: protocol.ErrorCodeRateLimited},
	sfu.CodeShareNotFound: {code: protocol.ErrorCodeShareNotFound},
	sfu.CodeTooManyShares: {
		code:   protocol.ErrorCodeShareLimit,
		params: map[string]any{"limit": sharesPerParticipant, "per": "user"},
	},
	sfu.CodeClosed: {code: protocol.ErrorCodeNotInRoom},
}

// site says where an SFU error came from: a call of the Conn, or an ErrorEvent. It decides the wire error's scope,
// the fields that name its subject, and whether the error is sent at all.
type site struct {
	call  string              // the *sfu.Conn method, or "ErrorEvent"; for logs
	scope protocol.ErrorScope // request for the share and subscription calls, pc for the six PC methods
	pc    protocol.PCKind     // scope pc: the PC, and the counters of the message that caused the call
	gen   uint32
	neg   uint32
	// reply says that the client waits for an answer, so every error is sent: the requests (share.start,
	// share.update, subscribe.update) and a pub offer. Elsewhere sfu.closed and sfu.stale_answer send nothing.
	reply bool
}

// requestSite is the site of a call behind a request: its errors are the request's reply.
func requestSite(call string) site {
	return site{call: call, scope: protocol.ErrorScopeRequest, reply: true}
}

// pcSite is the site of one of the six PC methods: its errors have scope pc with the call's pc, gen and neg.
func pcSite(call string, pc protocol.PCKind, gen, neg uint32) site {
	return site{call: call, scope: protocol.ErrorScopePC, pc: pc, gen: gen, neg: neg}
}

// eventSite is the site of an ErrorEvent with the SFU's scope; ok is false for a scope this package doesn't know.
// An event carries no gen or neg.
func eventSite(scope string) (at site, ok bool) {
	at = site{call: "ErrorEvent"}
	switch scope {
	case sfu.ScopePCPub:
		at.scope, at.pc = protocol.ErrorScopePC, protocol.PCKindPub
	case sfu.ScopePCSub:
		at.scope, at.pc = protocol.ErrorScopePC, protocol.PCKindSub
	case sfu.ScopeShare:
		at.scope = protocol.ErrorScopeShare
	case sfu.ScopeSubscription:
		at.scope = protocol.ErrorScopeSubscription
	default:
		return at, false
	}
	return at, true
}

// badRequest is the error for a value that has no SFU counterpart: bad_request with params {field, "invalid"}. The
// hub validates every payload before it calls a MediaPeer, so this only guards the adapter against passing a zero
// value on.
func (at site) badRequest(field string) *protocol.Error {
	fe := protocol.FieldError{Field: field, Reason: protocol.FieldInvalid}
	e := fe.BadRequest(at.scope)
	at.subject(&e, "")
	return &e
}

// subject sets the fields that name what e is about, by its scope: the PC and its counters, or the share.
func (at site) subject(e *protocol.Error, share sfu.ShareID) {
	switch e.Scope {
	case protocol.ErrorScopePC:
		e.PC, e.Gen, e.Neg = at.pc, at.gen, at.neg
	case protocol.ErrorScopeShare, protocol.ErrorScopeSubscription:
		e.ShareID = string(share)
	}
}

// wireError maps err, which a call of the peer's Conn returned or an ErrorEvent carried, to the wire error of
// 01 §15.4. sent is false for an error that sends nothing: sfu.stale_answer, and sfu.closed where no client waits for
// an answer; both are logged at debug.
//
// An error without a wire code of its own becomes internal, with a new ref in its params that is logged with the
// error. Its retryable flag is the SFU's, not the catalog's: an unexpected Pion error and sfu.busy may pass on a
// second try, while a call that broke the SFU's contract or reached a method that isn't implemented yet can't
// (02 §6.3). Every other code's flag is the catalog's (01 §12.1).
func (p *peer) wireError(err error, at site) (e protocol.Error, sent bool) {
	var se *sfu.Error
	if !errors.As(err, &se) {
		// Every method of *sfu.Conn returns an *sfu.Error (02 §6.3).
		return p.internalError(err, nil, at), true
	}
	if !at.reply && (se.Code == sfu.CodeClosed || se.Code == sfu.CodeStaleAnswer) {
		p.log.Debug("SFU error not sent", "call", at.call, "code", se.Code)
		return protocol.Error{}, false
	}
	row, ok := wireCodes[se.Code]
	if !ok {
		return p.internalError(err, se, at), true
	}
	e = protocol.NewError(row.code, at.scope)
	e.Params = maps.Clone(row.params)
	if se.Code == sfu.CodeNoH264 && se.Share != "" {
		// The one error of a PC method that isn't about the PC: the share can't be published, and the hub ends it.
		e.Scope = protocol.ErrorScopeShare
	}
	if se.RetryAfter > 0 {
		e.RetryAfterMs = retryAfterMs(se.RetryAfter)
	}
	at.subject(&e, se.Share)
	p.log.Debug("SFU error", "call", at.call, "code", se.Code, "wire_code", string(e.Code), "err", err)
	return e, true
}

// internalError returns error{internal} with a new ref and logs err with it, so a user can report the ref
// (01 §12.1). se is err as an *sfu.Error, or nil when it isn't one.
func (p *peer) internalError(err error, se *sfu.Error, at site) protocol.Error {
	ref := newRef()
	e := protocol.NewError(protocol.ErrorCodeInternal, at.scope)
	e.Params = map[string]any{"ref": ref}
	var share sfu.ShareID
	if se != nil {
		e.Retryable = se.Retryable
		share = se.Share
		if se.RetryAfter > 0 {
			e.RetryAfterMs = retryAfterMs(se.RetryAfter)
		}
	}
	at.subject(&e, share)
	if se != nil && se.Code == sfu.CodeBusy {
		p.log.Warn("the SFU connection is busy", "call", at.call, "ref", ref, "err", err)
	} else {
		p.log.Error("internal error", "call", at.call, "ref", ref, "err", err)
	}
	return e
}

// failed returns what a MediaPeer method returns for err, the result of its SFU call: nil for nil, else the
// *protocol.Error of 01 §15.4, or nil when the error sends nothing.
func (p *peer) failed(err error, at site) error {
	if err == nil {
		return nil
	}
	e, sent := p.wireError(err, at)
	if !sent {
		return nil
	}
	return &e
}

// dropped logs the error of an SFU call whose MediaPeer method returns none (EndShare, SetCaps), so nothing can be
// sent: at debug when the Conn is closed or the share has already ended, at warn when a second try could pass, and
// at error otherwise.
func (p *peer) dropped(call string, err error, attrs ...any) {
	if err == nil {
		return
	}
	attrs = append([]any{"call", call, "err", err}, attrs...)
	var se *sfu.Error
	switch {
	case !errors.As(err, &se):
		p.log.Error("SFU call failed", attrs...)
	case se.Code == sfu.CodeClosed || se.Code == sfu.CodeShareNotFound:
		p.log.Debug("SFU call had nothing to do", attrs...)
	case se.Retryable:
		p.log.Warn("SFU call failed", attrs...)
	default:
		p.log.Error("SFU call failed", attrs...)
	}
}

// retryAfterMs converts a wait into the wire's retryAfterMs: whole milliseconds, rounded up, at least 1.
func retryAfterMs(d time.Duration) int {
	return max(1, int((d+time.Millisecond-1)/time.Millisecond))
}

// refEncoding is lowercase Crockford base32 without padding, the alphabet of the hub's ids (README "Naming").
var refEncoding = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

// newRef returns the 8-character reference of an internal error, which goes to the client and into the log.
func newRef() string {
	var raw [5]byte
	_, _ = rand.Read(raw[:])
	return refEncoding.EncodeToString(raw[:])
}
