package sfuplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/sfu"
	"github.com/MoonWX/isshoni/internal/server/signal"
)

// The "Error codes" table of 01 §15.4.

// sfuErr builds an error as the SFU returns it: the code with the Retryable flag of 02 §6.3, and the RetryAfter of
// the codes that have one.
func sfuErr(code string) *sfu.Error {
	e := &sfu.Error{Code: code}
	switch code {
	case sfu.CodeBusy:
		e.Retryable, e.RetryAfter = true, time.Second
	case sfu.CodePCRateLimited:
		e.Retryable, e.RetryAfter = true, 6500*time.Millisecond
	case sfu.CodeProbeLimit:
		e.Retryable, e.RetryAfter = true, 3*time.Second
	case sfu.CodeInternal:
		e.Retryable = true
	}
	return e
}

// errRow is what one sfu.* code becomes on the wire.
type errRow struct {
	code   protocol.ErrorCode
	params map[string]any // the row's params; an internal error has params {ref} instead
	// silent: the error is not sent where no client waits for an answer (a notification, an event). code is then
	// what goes out where one does wait.
	silent bool
}

var errTable = map[string]errRow{
	sfu.CodeRoleForbidden: {code: protocol.ErrorCodeForbidden},
	sfu.CodeNotOwner:      {code: protocol.ErrorCodeForbidden},
	sfu.CodeBadPC:         {code: protocol.ErrorCodeBadRequest, params: map[string]any{"field": "pc", "reason": "invalid"}},
	sfu.CodePCLimit:       {code: protocol.ErrorCodeBadRequest, params: map[string]any{"field": "pc", "reason": "too_many"}},
	sfu.CodeUnknownTrack:  {code: protocol.ErrorCodeBadRequest, params: map[string]any{"field": "tracks", "reason": "invalid"}},
	sfu.CodeTooManySubscriptions: {
		code: protocol.ErrorCodeBadRequest, params: map[string]any{"field": "subs", "reason": "too_many"},
	},
	sfu.CodeBadSDP:        {code: protocol.ErrorCodeSDPInvalid},
	sfu.CodeBadRID:        {code: protocol.ErrorCodeSDPInvalid},
	sfu.CodeStaleOffer:    {code: protocol.ErrorCodeStaleNegotiation},
	sfu.CodeNoH264:        {code: protocol.ErrorCodeCodecNotSupported},
	sfu.CodePCRateLimited: {code: protocol.ErrorCodeRateLimited},
	sfu.CodeShareNotFound: {code: protocol.ErrorCodeShareNotFound},
	sfu.CodeTooManyShares: {code: protocol.ErrorCodeShareLimit, params: map[string]any{"limit": 4, "per": "user"}},
	sfu.CodeBusy:          {code: protocol.ErrorCodeInternal},
	sfu.CodeInternal:      {code: protocol.ErrorCodeInternal},
	sfu.CodeClosed:        {code: protocol.ErrorCodeNotInRoom, silent: true},
	sfu.CodeStaleAnswer:   {code: protocol.ErrorCodeInternal, silent: true}, // only HandleAnswer returns it, where it is silent
	// Only SFU.Probe returns these two, to 04's connection test: signaling never sees them.
	sfu.CodeProbeLimit:        {code: protocol.ErrorCodeInternal},
	sfu.CodeTransportDisabled: {code: protocol.ErrorCodeInternal},
}

// checkRef fails unless e is an internal error whose only param is an 8-character ref, and returns the ref.
func checkRef(t *testing.T, e protocol.Error) string {
	t.Helper()
	ref, _ := e.Params["ref"].(string)
	if e.Code != protocol.ErrorCodeInternal || len(e.Params) != 1 || len(ref) != 8 ||
		strings.Trim(ref, "0123456789abcdefghjkmnpqrstvwxyz") != "" {
		t.Errorf("error %+v, want internal with params {ref} of 8 lowercase base32 characters", e)
	}
	return ref
}

// TestErrorTable maps every error code that the SFU declares from every place an error can come from: a request, a
// PC notification, a pub offer and the four scopes of an ErrorEvent.
func TestErrorTable(t *testing.T) {
	// An error code is an untyped Code* constant with an sfu.* value (02 §6.3), in whichever file of the package.
	declared := 0
	for _, c := range sfuConsts(t) {
		if c.Type != "" || !strings.HasPrefix(c.Name, "Code") || !strings.HasPrefix(c.Value, "sfu.") {
			continue
		}
		declared++
		if _, ok := errTable[c.Value]; !ok {
			t.Errorf("%s (%q) has no row: add it to wireCodes and to this table", c.Name, c.Value)
		}
	}
	if declared != len(errTable) {
		t.Errorf("the SFU declares %d codes, the table has %d rows", declared, len(errTable))
	}

	pubOffer := pcSite("HandleOffer", protocol.PCKindPub, 3, 2)
	pubOffer.reply = true
	sites := []site{
		requestSite("StartShare"),
		pcSite("HandleAnswer", protocol.PCKindSub, 2, 5),
		pubOffer,
	}
	for _, scope := range []string{sfu.ScopePCPub, sfu.ScopePCSub, sfu.ScopeShare, sfu.ScopeSubscription} {
		at, ok := eventSite(scope)
		if !ok {
			t.Fatalf("eventSite(%q) is unknown", scope)
		}
		sites = append(sites, at)
	}

	for code, row := range errTable {
		for _, at := range sites {
			t.Run(fmt.Sprintf("%s/%s/%s", code, at.call, at.scope), func(t *testing.T) {
				tp := newTestPlane(t)
				pr := tp.newPeer("c_1", "u_1")
				in := sfuErr(code)
				in.Share = "s_err" // the SFU sets it for some codes; only the share scopes and no_h264 use it
				e, sent := pr.peer.wireError(fmt.Errorf("wrapped: %w", in), at)
				logs := tp.logs.take()

				if row.silent && !at.reply {
					if sent {
						t.Errorf("sent %+v, want nothing", e)
					}
					if len(atLeast(logs, slog.LevelInfo)) != 0 || len(logs) != 1 || logs[0].Level != slog.LevelDebug {
						t.Errorf("logged %+v, want one debug line", logs)
					}
					return
				}
				if !sent {
					t.Fatal("nothing sent")
				}

				// The scope is the site's, except for the one error that names a share instead of its PC.
				want := protocol.NewError(row.code, at.scope)
				if code == sfu.CodeNoH264 {
					want.Scope = protocol.ErrorScopeShare
				}
				switch want.Scope {
				case protocol.ErrorScopePC:
					want.PC, want.Gen, want.Neg = at.pc, at.gen, at.neg
				case protocol.ErrorScopeShare, protocol.ErrorScopeSubscription:
					want.ShareID = "s_err"
				}
				if in.RetryAfter > 0 {
					want.RetryAfterMs = int(in.RetryAfter / time.Millisecond)
				}
				want.Params = row.params
				if row.code == protocol.ErrorCodeInternal {
					ref := checkRef(t, e)
					want.Params = map[string]any{"ref": ref}
					// The ref is in the log with the SFU's error, at error (at warn when the Conn was only busy).
					level := slog.LevelError
					if code == sfu.CodeBusy {
						level = slog.LevelWarn
					}
					found := false
					for _, l := range atLeast(logs, slog.LevelWarn) {
						if l.Attrs["ref"] == ref && l.Level == level && strings.Contains(l.Attrs["err"], code) &&
							l.Attrs["call"] == at.call && l.Attrs["conn_id"] == "c_1" {
							found = true
						}
					}
					if !found {
						t.Errorf("no %v log line with ref %q and the SFU's error: %+v", level, ref, logs)
					}
				} else if lines := atLeast(logs, slog.LevelInfo); len(lines) != 0 {
					t.Errorf("logged %+v, want debug only", lines)
				}
				// The wire's retryable flag is the SFU's: for the catalog's codes the two tables agree (01 §12.1, 02 §6.3).
				want.Retryable = in.Retryable
				if !reflect.DeepEqual(e, want) {
					t.Errorf("wire error = %+v\nwant         %+v", e, want)
				}
				if !e.Code.Valid() || !e.Scope.Valid() {
					t.Errorf("%+v is not in the wire's catalog", e)
				}
				if catalog := protocol.NewError(e.Code, e.Scope).Retryable; e.Code != protocol.ErrorCodeInternal && e.Retryable != catalog {
					t.Errorf("%s: retryable = %v, the catalog says %v", e.Code, e.Retryable, catalog)
				}
				// No sfu.* code, and nothing of the SFU's error text, goes on the wire.
				b, err := json.Marshal(e)
				if err != nil || strings.Contains(string(b), "sfu") || strings.Contains(string(b), "wrapped") {
					t.Errorf("on the wire: %s (%v)", b, err)
				}
			})
		}
	}
}

// TestCodecNotSupported: sfu.no_h264 is the one error of a PC method that is not about the PC. It names the share
// that can't be published (scope share), so the hub ends it. Without a share it stays an error of the offer.
func TestCodecNotSupported(t *testing.T) {
	tp := newTestPlane(t)
	pr := tp.newPeer("c_1", "u_1")
	pr.conn.fail("HandleOffer", &sfu.Error{Code: sfu.CodeNoH264, Share: "s_vp8"})
	_, err := pr.HandleOffer(protocol.PCOffer{PC: protocol.PCKindPub, Gen: 3, Neg: 2, SDP: "v=0"})
	var pe *protocol.Error
	want := protocol.Error{Code: protocol.ErrorCodeCodecNotSupported, Scope: protocol.ErrorScopeShare, ShareID: "s_vp8"}
	if !errors.As(err, &pe) || !reflect.DeepEqual(*pe, want) {
		t.Errorf("HandleOffer = %v, want %+v", err, want)
	}

	pr.conn.fail("HandleOffer", &sfu.Error{Code: sfu.CodeNoH264})
	_, err = pr.HandleOffer(protocol.PCOffer{PC: protocol.PCKindPub, Gen: 3, Neg: 2, SDP: "v=0"})
	want = protocol.Error{Code: protocol.ErrorCodeCodecNotSupported, Scope: protocol.ErrorScopePC, PC: protocol.PCKindPub, Gen: 3, Neg: 2}
	if !errors.As(err, &pe) || !reflect.DeepEqual(*pe, want) {
		t.Errorf("HandleOffer without a share = %v, want %+v", err, want)
	}
}

// TestInternalErrors: internal carries the SFU's retryable flag, not the catalog's: an unexpected Pion error may
// pass on a second try, while a call that broke the SFU's contract, or reached a method that a later slice
// implements, can't (02 §6.3). An error that isn't the SFU's at all is unexpected, and retryable like the catalog's.
func TestInternalErrors(t *testing.T) {
	tp := newTestPlane(t)
	pr := tp.newPeer("c_1", "u_1")
	refs := map[string]bool{}
	for _, tc := range []struct {
		name      string
		err       error
		retryable bool
		level     slog.Level
	}{
		{"an unexpected Pion error", &sfu.Error{Code: sfu.CodeInternal, Retryable: true}, true, slog.LevelError},
		{"a contract violation, or a method of a later slice", &sfu.Error{Code: sfu.CodeInternal}, false, slog.LevelError},
		{"a code this package doesn't know, not retryable", &sfu.Error{Code: "sfu.from_the_future"}, false, slog.LevelError},
		{"a code this package doesn't know, retryable", &sfu.Error{Code: "sfu.from_the_future", Retryable: true}, true, slog.LevelError},
		{"the Conn's queue was full", &sfu.Error{Code: sfu.CodeBusy, Retryable: true, RetryAfter: time.Second}, true, slog.LevelWarn},
		{"not an SFU error", errors.New("boom"), true, slog.LevelError},
	} {
		pr.conn.fail("StartShare", tc.err)
		_, err := pr.CreateShare("s_1", protocol.ShareStart{Kind: protocol.ShareKindScreen, Preset: protocol.PresetAuto})
		var pe *protocol.Error
		if !errors.As(err, &pe) {
			t.Fatalf("%s: CreateShare = %v, want a wire error", tc.name, err)
		}
		ref := checkRef(t, *pe)
		if refs[ref] {
			t.Errorf("%s: ref %q was used before", tc.name, ref)
		}
		refs[ref] = true
		if pe.Retryable != tc.retryable || pe.Scope != protocol.ErrorScopeRequest {
			t.Errorf("%s: %+v, want retryable %v with scope request", tc.name, *pe, tc.retryable)
		}
		lines := atLeast(tp.logs.take(), slog.LevelInfo)
		if len(lines) != 1 || lines[0].Level != tc.level || lines[0].Attrs["ref"] != ref ||
			lines[0].Attrs["err"] != tc.err.Error() || lines[0].Attrs["call"] != "StartShare" {
			t.Errorf("%s: logged %+v, want one %v line with the ref and the error", tc.name, lines, tc.level)
		}
	}
}

// TestMethodErrors: an error of each MediaPeer method goes out from the right place. The six PC methods give scope
// pc with the call's pc, gen and neg; the share and subscription calls give scope request (the hub puts the reply's
// id on it). Where nobody waits for an answer, sfu.closed and sfu.stale_answer return nil: nothing is sent.
func TestMethodErrors(t *testing.T) {
	pub, sub := protocol.PCKindPub, protocol.PCKindSub
	pcErr := func(code protocol.ErrorCode, pc protocol.PCKind, gen, neg uint32) protocol.Error {
		e := protocol.NewError(code, protocol.ErrorScopePC)
		e.PC, e.Gen, e.Neg = pc, gen, neg
		return e
	}
	reqErr := func(code protocol.ErrorCode) protocol.Error {
		return protocol.NewError(code, protocol.ErrorScopeRequest)
	}
	withParams := func(e protocol.Error, k1 string, v1 any, k2 string, v2 any) protocol.Error {
		e.Params = map[string]any{k1: v1, k2: v2}
		return e
	}
	retryAfter := func(e protocol.Error, ms int) protocol.Error { e.RetryAfterMs = ms; return e }

	createShare := func(p signal.MediaPeer) error {
		_, err := p.CreateShare("s_1", protocol.ShareStart{Kind: protocol.ShareKindScreen, Preset: protocol.PresetAuto})
		return err
	}
	updateShare := func(p signal.MediaPeer) error {
		_, err := p.UpdateShare("s_1", protocol.ShareUpdate{ShareID: "s_1", Preset: protocol.PresetGame})
		return err
	}
	subscribe := func(p signal.MediaPeer) error {
		_, err := p.Subscribe([]protocol.SubscriptionWant{{ShareID: "s_1", Video: protocol.VideoLayerHigh, Audio: protocol.AudioStateOn}})
		return err
	}
	handleOffer := func(p signal.MediaPeer) error {
		answer, err := p.HandleOffer(protocol.PCOffer{PC: pub, Gen: 3, Neg: 2, SDP: "v=0"})
		if err != nil && answer != (protocol.PCAnswer{}) {
			return fmt.Errorf("an answer %+v came with the error %w", answer, err)
		}
		return err
	}
	handleAnswer := func(p signal.MediaPeer) error {
		return p.HandleAnswer(protocol.PCAnswer{PC: sub, Gen: 2, Neg: 5, SDP: "v=0"})
	}
	addICE := func(p signal.MediaPeer) error {
		return p.AddICE(protocol.PCICE{PC: pub, Gen: 3, Candidate: &protocol.ICECandidate{Candidate: "candidate:1"}})
	}
	restartICE := func(p signal.MediaPeer) error {
		return p.Restart(protocol.PCRestart{PC: sub, Gen: 2, Mode: protocol.RestartModeICE, Reason: protocol.RestartReasonDisconnected})
	}
	rebuild := func(p signal.MediaPeer) error {
		return p.Restart(protocol.PCRestart{PC: sub, Gen: 2, Mode: protocol.RestartModeRebuild, Reason: protocol.RestartReasonFailed})
	}
	closePC := func(p signal.MediaPeer) error { return p.ClosePC(protocol.PCClose{PC: pub, Gen: 3}) }

	for _, tc := range []struct {
		name   string
		method string // of the Conn
		err    *sfu.Error
		do     func(signal.MediaPeer) error
		want   *protocol.Error // nil: nothing is sent
	}{
		// The requests.
		{"share.start as a viewer", "StartShare", sfuErr(sfu.CodeRoleForbidden), createShare, ptr(reqErr(protocol.ErrorCodeForbidden))},
		{"share.start past the SFU's guard", "StartShare", sfuErr(sfu.CodeTooManyShares), createShare,
			ptr(withParams(reqErr(protocol.ErrorCodeShareLimit), "limit", 4, "per", "user"))},
		{"share.start on a closed Conn", "StartShare", sfuErr(sfu.CodeClosed), createShare, ptr(reqErr(protocol.ErrorCodeNotInRoom))},
		{"share.update of an ended share", "UpdateShare", sfuErr(sfu.CodeShareNotFound), updateShare, ptr(reqErr(protocol.ErrorCodeShareNotFound))},
		{"share.update of another Conn's share", "UpdateShare", sfuErr(sfu.CodeNotOwner), updateShare, ptr(reqErr(protocol.ErrorCodeForbidden))},
		{"share.update on a closed Conn", "UpdateShare", sfuErr(sfu.CodeClosed), updateShare, ptr(reqErr(protocol.ErrorCodeNotInRoom))},
		{"subscribe.update on a closed Conn", "UpdateSubscriptions", sfuErr(sfu.CodeClosed), subscribe, ptr(reqErr(protocol.ErrorCodeNotInRoom))},
		{"subscribe.update as a publisher", "UpdateSubscriptions", sfuErr(sfu.CodeRoleForbidden), subscribe, ptr(reqErr(protocol.ErrorCodeForbidden))},

		// HandleOffer: the client waits for its answer, so every error goes out.
		{"a stale pub offer", "HandleOffer", sfuErr(sfu.CodeStaleOffer), handleOffer, ptr(pcErr(protocol.ErrorCodeStaleNegotiation, pub, 3, 2))},
		{"a pub offer that doesn't parse", "HandleOffer", sfuErr(sfu.CodeBadSDP), handleOffer, ptr(pcErr(protocol.ErrorCodeSDPInvalid, pub, 3, 2))},
		{"a pub offer with a third rid", "HandleOffer", sfuErr(sfu.CodeBadRID), handleOffer, ptr(pcErr(protocol.ErrorCodeSDPInvalid, pub, 3, 2))},
		{"a pub offer with a malformed binding", "HandleOffer", sfuErr(sfu.CodeUnknownTrack), handleOffer,
			ptr(withParams(pcErr(protocol.ErrorCodeBadRequest, pub, 3, 2), "field", "tracks", "reason", "invalid"))},
		{"a pub offer for a closed PC of this gen", "HandleOffer", sfuErr(sfu.CodeBadPC), handleOffer,
			ptr(withParams(pcErr(protocol.ErrorCodeBadRequest, pub, 3, 2), "field", "pc", "reason", "invalid"))},
		{"a third PC", "HandleOffer", sfuErr(sfu.CodePCLimit), handleOffer,
			ptr(withParams(pcErr(protocol.ErrorCodeBadRequest, pub, 3, 2), "field", "pc", "reason", "too_many"))},
		{"a pub offer from a viewer", "HandleOffer", sfuErr(sfu.CodeRoleForbidden), handleOffer, ptr(pcErr(protocol.ErrorCodeForbidden, pub, 3, 2))},
		{"too many new pub PCs", "HandleOffer", sfuErr(sfu.CodePCRateLimited), handleOffer,
			ptr(retryAfter(pcErr(protocol.ErrorCodeRateLimited, pub, 3, 2), 6500))},
		{"a pub offer on a closed Conn", "HandleOffer", sfuErr(sfu.CodeClosed), handleOffer, ptr(pcErr(protocol.ErrorCodeNotInRoom, pub, 3, 2))},

		// The notifications.
		{"a sub answer that doesn't parse", "HandleAnswer", sfuErr(sfu.CodeBadSDP), handleAnswer, ptr(pcErr(protocol.ErrorCodeSDPInvalid, sub, 2, 5))},
		{"a sub answer without a sub PC", "HandleAnswer", sfuErr(sfu.CodeBadPC), handleAnswer,
			ptr(withParams(pcErr(protocol.ErrorCodeBadRequest, sub, 2, 5), "field", "pc", "reason", "invalid"))},
		{"a stale sub answer", "HandleAnswer", sfuErr(sfu.CodeStaleAnswer), handleAnswer, nil},
		{"a sub answer on a closed Conn", "HandleAnswer", sfuErr(sfu.CodeClosed), handleAnswer, nil},
		{"a candidate for an unknown PC", "AddICECandidate", sfuErr(sfu.CodeBadPC), addICE,
			ptr(withParams(pcErr(protocol.ErrorCodeBadRequest, pub, 3, 0), "field", "pc", "reason", "invalid"))},
		{"a candidate on a closed Conn", "AddICECandidate", sfuErr(sfu.CodeClosed), addICE, nil},
		{"pc.restart{ice} without a sub PC", "RestartICE", sfuErr(sfu.CodeBadPC), restartICE,
			ptr(withParams(pcErr(protocol.ErrorCodeBadRequest, sub, 2, 0), "field", "pc", "reason", "invalid"))},
		{"pc.restart{ice} on a closed Conn", "RestartICE", sfuErr(sfu.CodeClosed), restartICE, nil},
		{"too many rebuilds", "ResetPC", sfuErr(sfu.CodePCRateLimited), rebuild,
			ptr(retryAfter(pcErr(protocol.ErrorCodeRateLimited, sub, 2, 0), 6500))},
		{"pc.restart{rebuild} on a closed Conn", "ResetPC", sfuErr(sfu.CodeClosed), rebuild, nil},
		{"pc.close for the wrong kind", "ClosePC", sfuErr(sfu.CodeBadPC), closePC,
			ptr(withParams(pcErr(protocol.ErrorCodeBadRequest, pub, 3, 0), "field", "pc", "reason", "invalid"))},
		{"pc.close on a closed Conn", "ClosePC", sfuErr(sfu.CodeClosed), closePC, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp := newTestPlane(t)
			pr := tp.newPeer("c_1", "u_1")
			pr.conn.fail(tc.method, tc.err)
			err := tc.do(pr)
			if calls := pr.conn.take(); len(calls) != 1 || calls[0].Method != tc.method {
				t.Fatalf("the Conn was called with %+v, want one %s", calls, tc.method)
			}
			if tc.want == nil {
				if err != nil {
					t.Errorf("error = %v (%T), want nil", err, err)
				}
				return
			}
			var pe *protocol.Error
			if !errors.As(err, &pe) {
				t.Fatalf("error = %v (%T), want a *protocol.Error", err, err)
			}
			if !reflect.DeepEqual(*pe, *tc.want) {
				t.Errorf("error = %+v\nwant    %+v", *pe, *tc.want)
			}
		})
	}

	// An internal error of each PC method has the same scope and subject, with its ref.
	for name, do := range map[string]func(signal.MediaPeer) error{
		"HandleOffer": handleOffer, "HandleAnswer": handleAnswer, "AddICECandidate": addICE,
		"RestartICE": restartICE, "ResetPC": rebuild, "ClosePC": closePC,
	} {
		tp := newTestPlane(t)
		pr := tp.newPeer("c_1", "u_1")
		pr.conn.fail(name, &sfu.Error{Code: sfu.CodeInternal}) // e.g. a method that a later slice implements
		var pe *protocol.Error
		if err := do(pr); !errors.As(err, &pe) {
			t.Fatalf("%s: error = %v", name, err)
		}
		checkRef(t, *pe)
		if pe.Scope != protocol.ErrorScopePC || !pe.PC.Valid() || pe.Gen == 0 || pe.Retryable {
			t.Errorf("%s: %+v, want scope pc with the call's pc and gen, not retryable", name, *pe)
		}
	}
}

// TestCallsWithoutAnAnswer: EndShare and SetCaps return nothing, so their errors only reach the log: at debug when
// there was nothing left to do, at warn when a second try could pass, at error otherwise.
func TestCallsWithoutAnAnswer(t *testing.T) {
	endShare := func(p signal.MediaPeer) { p.EndShare("s_1", protocol.EndReasonStopped) }
	setCaps := func(p signal.MediaPeer) { p.SetCaps(protocol.Caps{Decode: []protocol.CodecKey{"h264/6400"}}) }
	for _, tc := range []struct {
		name   string
		method string
		err    error
		do     func(signal.MediaPeer)
		level  slog.Level
	}{
		{"the share has already ended", "StopShare", sfuErr(sfu.CodeShareNotFound), endShare, slog.LevelDebug},
		{"the Conn is closed", "StopShare", sfuErr(sfu.CodeClosed), endShare, slog.LevelDebug},
		{"the Conn is busy", "StopShare", sfuErr(sfu.CodeBusy), endShare, slog.LevelWarn},
		{"another Conn's share", "StopShare", sfuErr(sfu.CodeNotOwner), endShare, slog.LevelError},
		{"not an SFU error", "StopShare", errors.New("boom"), endShare, slog.LevelError},
		{"caps on a closed Conn", "SetDecodeCaps", sfuErr(sfu.CodeClosed), setCaps, slog.LevelDebug},
		{"caps: a method of a later slice", "SetDecodeCaps", &sfu.Error{Code: sfu.CodeInternal}, setCaps, slog.LevelError},
		{"caps: an unexpected Pion error", "SetDecodeCaps", sfuErr(sfu.CodeInternal), setCaps, slog.LevelWarn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp := newTestPlane(t)
			pr := tp.newPeer("c_1", "u_1")
			pr.conn.fail(tc.method, tc.err)
			tc.do(pr)
			logs := tp.logs.take()
			if len(logs) != 1 || logs[0].Level != tc.level || logs[0].Attrs["call"] != tc.method ||
				logs[0].Attrs["err"] != tc.err.Error() || logs[0].Attrs["conn_id"] != "c_1" {
				t.Errorf("logged %+v, want one %v line for %s", logs, tc.level, tc.method)
			}
			if events := pr.sink.Events(); len(events) != 0 {
				t.Errorf("the sink got %+v", events)
			}
		})
	}

	// Nothing is logged when they succeed.
	tp := newTestPlane(t)
	pr := tp.newPeer("c_1", "u_1")
	endShare(pr)
	setCaps(pr)
	if logs := tp.logs.take(); len(logs) != 0 {
		t.Errorf("logged %+v", logs)
	}
}
