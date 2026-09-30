package sfu

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrorCodes(t *testing.T) {
	// 02 §6.3: every code and its Retryable column.
	table := map[string]bool{
		"sfu.closed":                 false,
		"sfu.busy":                   true,
		"sfu.role_forbidden":         false,
		"sfu.bad_pc":                 false,
		"sfu.bad_sdp":                false,
		"sfu.no_h264":                false,
		"sfu.bad_rid":                false,
		"sfu.unknown_track":          false,
		"sfu.stale_answer":           false,
		"sfu.stale_offer":            false,
		"sfu.pc_limit":               false,
		"sfu.pc_rate_limited":        true,
		"sfu.share_not_found":        false,
		"sfu.not_owner":              false,
		"sfu.too_many_shares":        false,
		"sfu.too_many_subscriptions": false,
		"sfu.probe_limit":            true,
		"sfu.transport_disabled":     false,
		"sfu.internal":               true,
	}
	codes := []string{
		CodeClosed, CodeBusy, CodeRoleForbidden, CodeBadPC, CodeBadSDP, CodeNoH264, CodeBadRID, CodeUnknownTrack,
		CodeStaleAnswer, CodeStaleOffer, CodePCLimit, CodePCRateLimited, CodeShareNotFound, CodeNotOwner,
		CodeTooManyShares, CodeTooManySubscriptions, CodeProbeLimit, CodeTransportDisabled, CodeInternal,
	}
	if len(codes) != len(table) {
		t.Fatalf("%d constants, %d codes in 02 §6.3", len(codes), len(table))
	}
	seen := map[string]bool{}
	for _, c := range codes {
		retryable, ok := table[c]
		if !ok || seen[c] {
			t.Errorf("constant %q is not a 02 §6.3 code, or is repeated", c)
		}
		seen[c] = true
		if e := newError(c, "x"); e.Retryable != retryable || e.Code != c {
			t.Errorf("newError(%s) = %+v, want retryable %v", c, e, retryable)
		}
	}
}

func TestErrorMatching(t *testing.T) {
	e := shareError(CodeNoH264, "s_1", "m-line 0 offers no H.264")
	if e.Error() != "sfu.no_h264: m-line 0 offers no H.264" {
		t.Errorf("Error() = %q", e.Error())
	}
	if (&Error{Code: CodeBusy}).Error() != "sfu.busy" {
		t.Error("an Error without a message is its code")
	}
	wrapped := fmt.Errorf("handle offer: %w", e)
	if !errors.Is(wrapped, &Error{Code: CodeNoH264}) {
		t.Error("errors.Is must match by code through wrapping")
	}
	if errors.Is(wrapped, &Error{Code: CodeBadSDP}) || errors.Is(wrapped, errors.New(CodeNoH264)) {
		t.Error("errors.Is must not match another code or another type")
	}
	var got *Error
	if !errors.As(wrapped, &got) || got.Share != "s_1" || got.Retryable {
		t.Errorf("errors.As: %+v", got)
	}
}
