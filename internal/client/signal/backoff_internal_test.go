package signal

import (
	"math"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// backoffDelay is min(10 s, 0.5 s × 2ⁿ) × U(0.8, 1.2), clamped to [0.5 s, 10 s] (01 §10.2): about 0.5, 1, 2, 4, 8,
// 10, 10, … s, like the web client's backoffDelay (signal-client.ts).
func TestBackoffDelay(t *testing.T) {
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	for _, tc := range []struct {
		n      int
		u      float64
		wantMs float64
	}{
		{0, 0, 500}, // 400 ms, clamped up
		{0, 0.5, 500},
		{0, 1, 600},
		{1, 0, 800},
		{1, 0.5, 1000},
		{1, 1, 1200},
		{2, 0.5, 2000},
		{3, 0.5, 4000},
		{4, 0, 6400},
		{4, 0.5, 8000},
		{4, 1, 9600},
		{5, 0, 8000}, // 16 s is capped to 10 s before the jitter
		{5, 0.5, 10000},
		{5, 1, 10000}, // 12 s, clamped down
		{6, 0.25, 9000},
		{60, 0.5, 10000},
		{math.MaxInt, 0.5, 10000},
		{-1, 0.5, 500}, // never asked for; like n = 0
	} {
		if got := ms(backoffDelay(tc.n, tc.u)); math.Abs(got-tc.wantMs) > 0.001 {
			t.Errorf("backoffDelay(%d, %v) = %v ms, want %v ms", tc.n, tc.u, got, tc.wantMs)
		}
	}
	// Whatever the attempt and the sample, the delay stays within the bounds, and within ±20 % of its base.
	for n := range 80 {
		base := BackoffMax
		if n < 5 {
			base = BackoffMin << n
		}
		for i := range 1000 {
			d := backoffDelay(n, jitter())
			if d < BackoffMin || d > BackoffMax {
				t.Fatalf("backoffDelay(%d) sample %d = %v, outside [%v, %v]", n, i, d, BackoffMin, BackoffMax)
			}
			if f := float64(d) / float64(base); (f < 0.8 || f > 1.2) && d != BackoffMin && d != BackoffMax {
				t.Fatalf("backoffDelay(%d) sample %d = %v, more than 20 %% off %v", n, i, d, base)
			}
		}
	}
}

func TestJitter(t *testing.T) {
	seen := map[float64]bool{}
	for range 1000 {
		u := jitter()
		if u < 0 || u >= 1 {
			t.Fatalf("jitter() = %v, want it in [0, 1)", u)
		}
		seen[u] = true
	}
	if len(seen) < 990 {
		t.Errorf("1000 samples have %d distinct values", len(seen))
	}
}

func TestServerWait(t *testing.T) {
	for _, tc := range []struct {
		ms   int
		want time.Duration
		ok   bool
	}{
		{0, 0, true},
		{1, time.Millisecond, true},
		{1840, 1840 * time.Millisecond, true},
		{300000, 5 * time.Minute, true},
		{300001, 5 * time.Minute, true},
		{math.MaxInt, 5 * time.Minute, true}, // no overflow into a negative wait
		{-1, 0, false},
		{math.MinInt, 0, false},
	} {
		if got, ok := serverWait(tc.ms); got != tc.want || ok != tc.ok {
			t.Errorf("serverWait(%d) = %v, %v; want %v, %v", tc.ms, got, ok, tc.want, tc.ok)
		}
	}
}

func TestPingInterval(t *testing.T) {
	for ms, want := range map[int]time.Duration{
		15000:       15 * time.Second,
		5000:        5 * time.Second,
		0:           DefaultPingInterval, // an old or broken welcome
		-15000:      DefaultPingInterval,
		1:           time.Second, // not a flood
		999:         time.Second,
		1000:        time.Second,
		math.MaxInt: 5 * time.Minute,
	} {
		if got := pingInterval(ms); got != want {
			t.Errorf("pingInterval(%d) = %v, want %v", ms, got, want)
		}
	}
}

// errorForClose is the column "Client without a preceding error" of 01 §12.2, with the web client's codes.
func TestErrorForClose(t *testing.T) {
	type want struct {
		code      protocol.ErrorCode
		scope     protocol.ErrorScope
		retryable bool
	}
	conn, sess := protocol.ErrorScopeConnection, protocol.ErrorScopeSession
	mapped := map[protocol.CloseCode]want{
		protocol.CloseCodeUnsupportedData:    {protocol.ErrorCodeBadMessage, conn, false},
		protocol.CloseCodeProtocolViolation:  {protocol.ErrorCodeBadMessage, conn, false},
		protocol.CloseCodeUnauthenticated:    {protocol.ErrorCodeUnauthenticated, sess, false},
		protocol.CloseCodeForbidden:          {protocol.ErrorCodeForbidden, conn, false},
		protocol.CloseCodeReplaced:           {protocol.ErrorCodeReplaced, conn, false},
		protocol.CloseCodeVersionUnsupported: {protocol.ErrorCodeProtocolUnsupported, conn, false},
		protocol.CloseCodeRateLimited:        {protocol.ErrorCodeRateLimited, conn, true},
	}
	for code, w := range mapped {
		e := errorForClose(websocket.StatusCode(code))
		if e == nil || e.Code != w.code || e.Scope != w.scope || e.Retryable != w.retryable || e.Params != nil {
			t.Errorf("errorForClose(%d) = %+v, want %+v", code, e, w)
		}
	}
	// Every other code is a plain backoff: the rest of the table, no close frame (-1), no code at all (0), and codes
	// from a newer server.
	for _, code := range []protocol.CloseCode{protocol.CloseCodeNormal, protocol.CloseCodeGoingAway,
		protocol.CloseCodeAbnormal, protocol.CloseCodeMessageTooBig, protocol.CloseCodeInternalError,
		protocol.CloseCodeServiceRestart, protocol.CloseCodeTimeout, protocol.CloseCodeSlowConnection,
		-1, 0, 1005, 1013, 3000, 4000, 4777, 4999} {
		if _, isMapped := mapped[code]; isMapped {
			t.Fatalf("close code %d is in both lists", code)
		}
		if e := errorForClose(websocket.StatusCode(code)); e != nil {
			t.Errorf("errorForClose(%d) = %+v, want nil", code, e)
		}
	}
}

// decodeError tolerates what a broken server sends, like the web client's ProtocolError.fromWire.
func TestDecodeError(t *testing.T) {
	for _, tc := range []struct {
		data string
		want protocol.Error
	}{
		{`{"code":"share_limit","scope":"request","retryable":false,"params":{"limit":4}}`,
			protocol.Error{Code: protocol.ErrorCodeShareLimit, Scope: protocol.ErrorScopeRequest}},
		{`{"code":"rate_limited","scope":"connection","retryable":true,"retryAfterMs":900}`,
			protocol.Error{Code: protocol.ErrorCodeRateLimited, Scope: protocol.ErrorScopeConnection, Retryable: true, RetryAfterMs: 900}},
		{`{"code":"sdp_invalid","scope":"pc","pc":"sub","gen":2,"neg":3,"shareId":"s_1","roomId":"lounge"}`,
			protocol.Error{Code: protocol.ErrorCodeSDPInvalid, Scope: protocol.ErrorScopePC, PC: protocol.PCKindSub, Gen: 2, Neg: 3, ShareID: "s_1", RoomID: "lounge"}},
		{`{"code":"from_the_future","scope":"galaxy","retryable":true,"more":{"a":[1]}}`,
			protocol.Error{Code: "from_the_future", Scope: "galaxy", Retryable: true}},
		{``, protocol.Error{Code: protocol.ErrorCodeInternal, Scope: protocol.ErrorScopeRequest}},
		{`{}`, protocol.Error{Code: protocol.ErrorCodeInternal, Scope: protocol.ErrorScopeRequest}},
		{`{"scope":"session"}`, protocol.Error{Code: protocol.ErrorCodeInternal, Scope: protocol.ErrorScopeSession}},
		{`{"code":"forbidden"}`, protocol.Error{Code: protocol.ErrorCodeForbidden, Scope: protocol.ErrorScopeRequest}},
		{`{"code":7,"scope":"connection","retryable":"yes"}`,
			protocol.Error{Code: protocol.ErrorCodeInternal, Scope: protocol.ErrorScopeConnection}},
	} {
		got := decodeError(protocol.Envelope{Type: protocol.MessageTypeError, Data: []byte(tc.data)})
		got.Params = nil
		if got.Code != tc.want.Code || got.Scope != tc.want.Scope || got.Retryable != tc.want.Retryable ||
			got.RetryAfterMs != tc.want.RetryAfterMs || got.PC != tc.want.PC || got.Gen != tc.want.Gen ||
			got.Neg != tc.want.Neg || got.ShareID != tc.want.ShareID || got.RoomID != tc.want.RoomID {
			t.Errorf("decodeError(%s) = %+v, want %+v", tc.data, *got, tc.want)
		}
	}
}

func TestStateString(t *testing.T) {
	for s, want := range map[State]string{
		StateConnecting: "connecting", StateHandshaking: "handshaking", StateReady: "ready",
		StateBackoff: "backoff", StateStopped: "stopped", 0: "state(0)", 9: "state(9)",
	} {
		if got := s.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", s, got, want)
		}
	}
}
