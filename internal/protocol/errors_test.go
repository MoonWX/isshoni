package protocol

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

// catalogRow is one row of 01 §12.1: code, scope, retryable, close code (0 = none).
type catalogRow struct {
	code      ErrorCode
	scope     ErrorScope
	retryable bool
	close     CloseCode
}

// catalogRows is 01 §12.1 as data.
var catalogRows = []catalogRow{
	{ErrorCodeBadMessage, ErrorScopeConnection, false, 4400},
	{ErrorCodeHelloRequired, ErrorScopeConnection, false, 4400},
	{ErrorCodeHelloTimeout, ErrorScopeConnection, true, 4408},
	{ErrorCodeIdleTimeout, ErrorScopeConnection, true, 4408},
	{ErrorCodeProtocolUnsupported, ErrorScopeConnection, false, 4426},
	{ErrorCodeClientOutdated, ErrorScopeConnection, false, 4426},
	{ErrorCodeUnauthenticated, ErrorScopeSession, false, 4401},
	{ErrorCodeSessionRevoked, ErrorScopeSession, false, 4401},
	{ErrorCodeAccountDisabled, ErrorScopeSession, false, 4403},
	{ErrorCodeTooManyConnections, ErrorScopeConnection, false, 4429},
	{ErrorCodeRateLimited, ErrorScopeRequest, true, 0},
	{ErrorCodeRateLimited, ErrorScopePC, true, 0},
	{ErrorCodeRateLimited, ErrorScopeConnection, true, 4429},
	{ErrorCodeSlowConnection, ErrorScopeConnection, true, 4503},
	{ErrorCodeReplaced, ErrorScopeConnection, false, 4409},
	{ErrorCodeServerShutdown, ErrorScopeConnection, true, 1012},
	{ErrorCodeInternal, ErrorScopeRequest, true, 0},
	{ErrorCodeInternal, ErrorScopeConnection, true, 1011},
	{ErrorCodeInternal, ErrorScopePC, true, 0},
	{ErrorCodeBadRequest, ErrorScopeRequest, false, 0},
	{ErrorCodeBadRequest, ErrorScopePC, false, 0},
	{ErrorCodeBadRequest, ErrorScopeConnection, false, 4400},
	{ErrorCodeUnknownType, ErrorScopeRequest, false, 0},
	{ErrorCodeMessageTooLarge, ErrorScopeRequest, false, 0},
	{ErrorCodeForbidden, ErrorScopeRequest, false, 0},
	{ErrorCodeForbidden, ErrorScopePC, false, 0},
	{ErrorCodeFeatureDisabled, ErrorScopeRequest, false, 0},
	{ErrorCodeNotInRoom, ErrorScopeRequest, false, 0},
	{ErrorCodeRoomNotFound, ErrorScopeRequest, false, 0},
	{ErrorCodeRoomFull, ErrorScopeRequest, true, 0},
	{ErrorCodeKicked, ErrorScopeRoom, false, 0},
	{ErrorCodeKicked, ErrorScopeRequest, true, 0},
	{ErrorCodeRoomClosed, ErrorScopeRoom, false, 0},
	{ErrorCodeShareNotFound, ErrorScopeRequest, false, 0},
	{ErrorCodeShareLimit, ErrorScopeRequest, false, 0},
	{ErrorCodeCodecNotSupported, ErrorScopeShare, false, 0},
	{ErrorCodeSDPInvalid, ErrorScopePC, false, 0},
	{ErrorCodeStaleNegotiation, ErrorScopePC, false, 0},
	{ErrorCodeAgentTargetNotFound, ErrorScopeRequest, false, 0},
}

func TestErrorCatalog(t *testing.T) {
	for _, r := range catalogRows {
		e := NewError(r.code, r.scope)
		if e.Code != r.code || e.Scope != r.scope || e.Retryable != r.retryable {
			t.Errorf("NewError(%s, %s) = %+v, want retryable %v", r.code, r.scope, e, r.retryable)
		}
		if got := CloseCodeFor(r.code, r.scope); got != r.close {
			t.Errorf("CloseCodeFor(%s, %s) = %d, want %d", r.code, r.scope, got, r.close)
		}
	}
	// Every code is in the table above, and the table has no code outside ErrorCodes.
	codes := ErrorCodes()
	for _, c := range codes {
		if !slices.ContainsFunc(catalogRows, func(r catalogRow) bool { return r.code == c }) {
			t.Errorf("%s has no row in the 01 §12.1 test table", c)
		}
	}
	for _, r := range catalogRows {
		if !r.code.Valid() {
			t.Errorf("%s is not in ErrorCodes", r.code)
		}
	}
	if len(codes) != 31 || len(errorCatalog) != len(codes) {
		t.Errorf("%d codes, %d catalog entries; want 31 each", len(codes), len(errorCatalog))
	}
	sorted := slices.Clone(codes)
	slices.Sort(sorted)
	if len(slices.Compact(sorted)) != len(codes) {
		t.Error("ErrorCodes has duplicates")
	}
	// ErrorCodes returns a copy.
	codes[0] = "changed"
	if ErrorCodes()[0] != ErrorCodeBadMessage {
		t.Error("ErrorCodes exposes its backing array")
	}
}

func TestNewErrorUnknownCode(t *testing.T) {
	e := NewError("brand_new_code", ErrorScopeRequest)
	if e.Retryable || e.Code != "brand_new_code" {
		t.Errorf("got %+v", e)
	}
	if CloseCodeFor("brand_new_code", ErrorScopeConnection) != 0 {
		t.Error("unknown code has a close code")
	}
	if ErrorCode("brand_new_code").Valid() {
		t.Error("unknown code is valid")
	}
}

func TestErrorIsAnError(t *testing.T) {
	pe := NewError(ErrorCodeShareLimit, ErrorScopeRequest)
	pe.Params = map[string]any{"limit": 4, "per": "user"}
	err := fmt.Errorf("share.start: %w", &pe)
	var got *Error
	if !errors.As(err, &got) || got.Code != ErrorCodeShareLimit {
		t.Fatalf("errors.As: %v", err)
	}
	if s := got.Error(); s != "protocol: error share_limit (scope request)" {
		t.Errorf("Error() = %q", s)
	}
}

func TestCloseCodes(t *testing.T) {
	// 01 §12.2.
	want := map[CloseCode]int{
		CloseCodeNormal: 1000, CloseCodeGoingAway: 1001, CloseCodeUnsupportedData: 1003, CloseCodeAbnormal: 1006,
		CloseCodeMessageTooBig: 1009, CloseCodeInternalError: 1011, CloseCodeServiceRestart: 1012,
		CloseCodeProtocolViolation: 4400, CloseCodeUnauthenticated: 4401, CloseCodeForbidden: 4403,
		CloseCodeTimeout: 4408, CloseCodeReplaced: 4409, CloseCodeVersionUnsupported: 4426,
		CloseCodeRateLimited: 4429, CloseCodeSlowConnection: 4503,
	}
	for c, n := range want {
		if int(c) != n {
			t.Errorf("%d != %d", c, n)
		}
	}
	// Only scopes connection and session close the socket.
	for _, c := range ErrorCodes() {
		for _, s := range []ErrorScope{ErrorScopeRequest, ErrorScopePC, ErrorScopeShare, ErrorScopeRoom, ErrorScopeSubscription} {
			if CloseCodeFor(c, s) != 0 {
				t.Errorf("%s in scope %s closes the socket", c, s)
			}
		}
	}
}
