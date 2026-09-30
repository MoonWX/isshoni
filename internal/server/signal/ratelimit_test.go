package signal_test

import (
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/signal"
	"github.com/MoonWX/isshoni/internal/server/signal/signaltest"
)

// A burst of 101 messages: the 101st gets rate_limited with retryAfterMs (01 §13: burst 100, 20/s).
func TestRateLimitBurst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		c, _ := e.connect(cookie, signaltest.DefaultHello())
		for i := 1; i <= 101; i++ {
			if err := c.Send(protocol.MessageTypeRoomLeave, strconv.Itoa(i), protocol.Empty{}); err != nil {
				t.Fatal(err)
			}
		}
		for i := 1; i <= 100; i++ {
			if env := expectType(t, c, protocol.MessageTypeOK); env.Re != strconv.Itoa(i) {
				t.Fatalf("reply %d: re %q", i, env.Re)
			}
		}
		pe, re := expectError(t, c, protocol.ErrorCodeRateLimited, protocol.ErrorScopeRequest)
		if re != "101" || pe.RetryAfterMs != 50 || !pe.Retryable {
			t.Errorf("re %q, retryAfterMs %d, retryable %v; want 101, 50 (one message per 50 ms), true", re,
				pe.RetryAfterMs, pe.Retryable)
		}
		// 50 ms later one more passes.
		time.Sleep(50 * time.Millisecond)
		id := request(t, c, protocol.MessageTypeRoomLeave, protocol.Empty{})
		if env := expectType(t, c, protocol.MessageTypeOK); env.Re != id {
			t.Errorf("re %q", env.Re)
		}
	})
}

// A continuous flood: once the global bucket has been refusing for 10 s the connection gets
// error{rate_limited, scope connection} and 4429 (01 §12.1).
func TestRateLimitFlood(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		c, _ := e.connect(cookie, signaltest.DefaultHello())
		start := time.Now()
		for time.Since(start) < time.Minute { // 100 messages per second against a refill of 20
			if err := c.Send(protocol.MessageTypePing, "", protocol.Ping{T: 1}); err != nil {
				break // the server has closed the socket
			}
			time.Sleep(10 * time.Millisecond)
		}
		elapsed := time.Since(start)
		// The bucket empties after 1.25 s; the close comes 10 s later.
		if elapsed < 11*time.Second || elapsed > 12*time.Second {
			t.Errorf("closed after %v, want about 11.25 s", elapsed)
		}
		pongs := 0
		var last protocol.Envelope
		for {
			env, err := c.Recv(ctxT(t))
			if err != nil {
				break
			}
			if env.Type == protocol.MessageTypePong {
				pongs++
				continue
			}
			last = env
		}
		pe, err := protocol.Decode[protocol.Error](last)
		if err != nil || pe.Code != protocol.ErrorCodeRateLimited || pe.Scope != protocol.ErrorScopeConnection ||
			pe.RetryAfterMs <= 0 {
			t.Errorf("last message %s %s, want error{rate_limited, scope connection, retryAfterMs}", last.Type, last.Data)
		}
		expectClose(t, c, protocol.CloseCodeRateLimited)
		// Refused notifications are dropped silently: 100 of the burst plus about 20 per second.
		if pongs < 300 || pongs > 340 {
			t.Errorf("%d pongs, want about 325", pongs)
		}
	})
}

// Bursts that exceed the bucket, with traffic under the refill rate in between, are not a flood: a flood ends when
// the bucket is half full again, so a second burst 12 s after the first starts a new one.
func TestRateLimitBurstThenCalm(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		c, _ := e.connect(cookie, signaltest.DefaultHello())
		burst := func() {
			for range 150 {
				if err := c.Send(protocol.MessageTypePing, "", protocol.Ping{T: 1}); err != nil {
					t.Fatal(err)
				}
			}
		}
		calm := func(d time.Duration) { // 10 per second
			for end := time.Now().Add(d); time.Now().Before(end); {
				if err := c.Send(protocol.MessageTypePing, "", protocol.Ping{T: 1}); err != nil {
					t.Fatal(err)
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
		burst()
		calm(12 * time.Second)
		burst()
		calm(5 * time.Second)
		synctest.Wait()
		select {
		case <-c.Closed():
			t.Fatalf("closed: %v", c.Err())
		default:
		}
	})
}

// share.start is limited to 10 per minute (01 §13).
func TestRateLimitShareStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		c, _ := e.connect(cookie, signaltest.DefaultHello())
		start := protocol.ShareStart{Kind: protocol.ShareKindScreen, Preset: protocol.PresetAuto, Ref: "r1"}
		for range 10 {
			request(t, c, protocol.MessageTypeShareStart, start)
			expectError(t, c, protocol.ErrorCodeNotInRoom, protocol.ErrorScopeRequest)
		}
		id := request(t, c, protocol.MessageTypeShareStart, start)
		pe, re := expectError(t, c, protocol.ErrorCodeRateLimited, protocol.ErrorScopeRequest)
		if re != id || pe.RetryAfterMs != 6000 {
			t.Errorf("re %q, retryAfterMs %d; want %s and 6000", re, pe.RetryAfterMs, id)
		}
		time.Sleep(6 * time.Second)
		request(t, c, protocol.MessageTypeShareStart, start)
		expectError(t, c, protocol.ErrorCodeNotInRoom, protocol.ErrorScopeRequest)
	})
}

// stats: at most one per 5 s; the second within 5 s is dropped silently (01 §8.11).
func TestRateLimitStats(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		c, w := e.connect(cookie, signaltest.DefaultHello())
		send := func(interval int) {
			t.Helper()
			if err := c.Send(protocol.MessageTypeStats, "", protocol.ClientStats{IntervalMs: interval}); err != nil {
				t.Fatal(err)
			}
			ping(t, c) // nothing but the pong comes back, and the actor has handled the report
		}
		kept := func() int {
			s := signal.LastStats(e.hub, w.ConnectionID)
			if s == nil {
				return 0
			}
			return s.IntervalMs
		}
		send(1)
		send(2)
		if got := kept(); got != 1 {
			t.Errorf("kept report %d, want 1 (the second is dropped)", got)
		}
		time.Sleep(5 * time.Second)
		send(3)
		if got := kept(); got != 3 {
			t.Errorf("kept report %d, want 3", got)
		}
	})
}

// pc.restart: 12 per minute per PC kind, errors with scope pc (01 §13).
func TestRateLimitPCRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		defer e.close()
		cookie, _ := e.user(false)
		c, _ := e.connect(cookie, signaltest.DefaultHello())
		restart := protocol.PCRestart{PC: protocol.PCKindSub, Gen: 3, Mode: protocol.RestartModeICE,
			Reason: protocol.RestartReasonDisconnected}
		for range 12 {
			if err := c.Send(protocol.MessageTypePCRestart, "", restart); err != nil {
				t.Fatal(err)
			}
			expectError(t, c, protocol.ErrorCodeNotInRoom, protocol.ErrorScopePC)
		}
		if err := c.Send(protocol.MessageTypePCRestart, "", restart); err != nil {
			t.Fatal(err)
		}
		pe, _ := expectError(t, c, protocol.ErrorCodeRateLimited, protocol.ErrorScopePC)
		if pe.PC != protocol.PCKindSub || pe.Gen != 3 || pe.RetryAfterMs != 5000 {
			t.Errorf("pc %q, gen %d, retryAfterMs %d; want sub, 3, 5000", pe.PC, pe.Gen, pe.RetryAfterMs)
		}
	})
}
