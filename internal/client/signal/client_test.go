package signal_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"

	"github.com/MoonWX/isshoni/internal/client/signal"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// The client against a scripted server: the parts of 01 §10.1–§10.2, §3.4 and §12 that a healthy hub never shows
// (timeouts, a server that stops answering, every close code and error scope, waits that the server dictates).

// bubble runs a test in a synctest bubble with a fake server.
func bubble(t *testing.T, test func(t *testing.T, f *fakeServer)) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		f := newFakeServer(t)
		defer f.close()
		test(t, f)
	})
}

// ready connects a client and reads its first three state changes.
func ready(t *testing.T, f *fakeServer, o signal.Options) (*signal.Client, *fakeConn) {
	t.Helper()
	c, fc := f.dial(o)
	fc.expect(protocol.MessageTypeHello)
	expectStates(t, c, signal.StateConnecting, signal.StateHandshaking, signal.StateReady)
	return c, fc
}

// reconnected waits for the client to come back after a drop: backoff, connecting, handshaking, ready. It returns
// the backoff and the ready changes and the new socket.
func reconnected(t *testing.T, f *fakeServer, c *signal.Client) (back, up signal.StateChange, fc *fakeConn) {
	t.Helper()
	back = expectBackoff(t, c)
	up = expectStates(t, c, signal.StateHandshaking, signal.StateReady)
	return back, up, f.conn()
}

// request runs c.Request on its own goroutine.
func request(ctx context.Context, c *signal.Client, typ protocol.MessageType, data, result any) <-chan error {
	done := make(chan error, 1)
	go func() { done <- c.Request(ctx, typ, data, result) }()
	return done
}

// result waits for a request started with request.
func result(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(wait):
		t.Fatal("the request did not return")
	}
	return nil
}

// The handshake (01 §8.2): hello is the first message, with the protocol versions, the client's options and no
// resume token; the upgrade carries Options.Header and no Origin. Every later hello carries the token of the last
// welcome.
func TestHello(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		o := f.options()
		o.Header = http.Header{"Cookie": {"isshoni_session=abc"}, "X-Probe": {"1"}}
		c, fc := f.dial(o)
		defer c.Close()

		env := fc.expect(protocol.MessageTypeHello)
		h, err := protocol.Decode[protocol.Hello](env)
		if err != nil || env.ID == "" {
			t.Fatalf("hello %s (id %q): %v", env.Data, env.ID, err)
		}
		if h.Protocol != protocol.Version || h.MinProtocol != protocol.MinVersion || h.Client != o.Client ||
			h.Role != o.Role || h.ResumeToken != "" || h.Auth != nil {
			t.Errorf("hello %+v", h)
		}
		caps, _ := json.Marshal(h.Caps)
		want, _ := json.Marshal(o.Caps)
		if !bytes.Equal(caps, want) {
			t.Errorf("hello caps %s, want %s", caps, want)
		}
		if !bytes.Contains(env.Data, []byte(`"features":[]`)) {
			t.Errorf("hello %s, want features: []", env.Data)
		}
		if got := fc.header.Get("Cookie"); got != "isshoni_session=abc" || fc.header.Get("X-Probe") != "1" {
			t.Errorf("upgrade headers %v, want the cookie and X-Probe", fc.header)
		}
		if fc.header.Get("Origin") != "" || fc.header.Get("Authorization") != "" {
			t.Errorf("upgrade headers %v, want no Origin and no Authorization", fc.header)
		}
		if w := c.Welcome(); w.ConnectionID != "c_aaaaaaaaaaaaaaaa" || w.ResumeToken != "r1.token1" || w.Resumed {
			t.Errorf("welcome %+v", w)
		}
		expectStates(t, c, signal.StateConnecting, signal.StateHandshaking, signal.StateReady)

		// The caller's later changes to its header don't reach the client.
		o.Header.Set("Cookie", "changed")
		for n := int32(1); n <= 3; n++ {
			fc.kill()
			var up signal.StateChange
			_, up, fc = reconnected(t, f, c)
			h := fc.hello()
			if want := protocol.Secret(fmt.Sprintf("r1.token%d", n)); h.ResumeToken != want {
				t.Errorf("hello %d resumes with %q, want the token of welcome %d", n+1, h.ResumeToken.Reveal(), n)
			}
			if !up.Resumed || c.Welcome().ResumeToken != protocol.Secret(fmt.Sprintf("r1.token%d", n+1)) {
				t.Errorf("reconnect %d: ready %+v, token %q", n, up, c.Welcome().ResumeToken.Reveal())
			}
			if got := fc.header.Get("Cookie"); got != "isshoni_session=abc" {
				t.Errorf("reconnect %d: cookie %q", n, got)
			}
		}
	})
}

func TestHelloFeatures(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		o := f.options()
		o.Features = []protocol.Feature{protocol.FeatureAgentRelay, "from.the.future"}
		o.Role = protocol.RoleViewer
		c, fc := f.dial(o)
		defer c.Close()
		h := fc.hello()
		if len(h.Features) != 2 || h.Features[0] != protocol.FeatureAgentRelay || h.Role != protocol.RoleViewer {
			t.Errorf("hello %+v", h)
		}
	})
}

// Dial's own failures: options that can't work, and a context that is over.
func TestDialOptions(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		for _, url := range []string{"", "example.com/ws", "ftp://example.com/ws", "ws:///ws", "ws://exa mple.com/ws"} {
			o := f.options()
			o.URL = url
			if c, err := signal.Dial(ctxT(t), o); c != nil || err == nil || !strings.Contains(err.Error(), "options") {
				t.Errorf("Dial(%q) = %v, %v; want an options error", url, c, err)
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if c, err := signal.Dial(ctx, f.options()); c != nil || !errors.Is(err, context.Canceled) {
			t.Errorf("Dial with a canceled context = %v, %v", c, err)
		}
		if n := f.upgrades.Load(); n != 0 {
			t.Errorf("%d upgrade requests, want none", n)
		}
	})
}

// The client outlives Dial's context: canceling it afterwards changes nothing.
func TestDialContextEndsWithDial(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		c, err := signal.Dial(ctx, f.options())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		fc := f.conn()
		fc.expect(protocol.MessageTypeHello)
		expectStates(t, c, signal.StateConnecting, signal.StateHandshaking, signal.StateReady)
		cancel()
		time.Sleep(time.Minute) // far beyond the context's deadline and the connect timeout
		noStates(t, c)
		done := request(ctxT(t), c, protocol.MessageTypeRoomLeave, protocol.Empty{}, nil)
		fc.send(protocol.MessageTypeOK, fc.expect(protocol.MessageTypeRoomLeave).ID, nil)
		if err := result(t, done); err != nil {
			t.Errorf("request after Dial's context ended: %v", err)
		}
	})
}

// A context that ends while Dial still waits for its first welcome stops the client, with a normal close.
func TestDialContextDuringHandshake(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		f.manual.Store(true)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		start := time.Now()
		c, err := signal.Dial(ctx, f.options())
		if c != nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Dial = %v, %v; want the deadline", c, err)
		}
		if got := time.Since(start); got != 3*time.Second {
			t.Errorf("Dial returned after %v, want 3 s", got)
		}
		fc := f.conn()
		fc.expect(protocol.MessageTypeHello)
		if code := fc.closed(); code != websocket.StatusNormalClosure {
			t.Errorf("the client closed with %d, want 1000", code)
		}
	})
}

// No welcome within 10 s: the attempt fails and the client backs off (01 §10.1). A welcome that does not answer the
// hello is not one.
func TestHandshakeTimeout(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		f.manual.Store(true)
		type dialed struct {
			c   *signal.Client
			err error
		}
		res := make(chan dialed, 1)
		go func() {
			c, err := signal.Dial(context.Background(), f.options())
			res <- dialed{c, err}
		}()
		fc := f.conn()
		start := time.Now()
		hello := fc.expect(protocol.MessageTypeHello)
		fc.send(protocol.MessageTypeWelcome, "not-"+hello.ID, f.welcome(1))
		fc.send(protocol.MessageTypeWelcome, "", f.welcome(1))
		if code := fc.closed(); code != -1 {
			t.Errorf("the client ended the socket with close code %d, want none (the server keeps the grace)", code)
		}
		if got := time.Since(start); got != signal.HandshakeTimeout {
			t.Errorf("the client gave up after %v, want %v", got, signal.HandshakeTimeout)
		}
		select {
		case d := <-res:
			t.Fatalf("Dial returned %v, %v before any welcome", d.c, d.err)
		default:
		}

		fc = f.conn()
		if got := time.Since(start) - signal.HandshakeTimeout; got < 500*time.Millisecond || got > 600*time.Millisecond {
			t.Errorf("next attempt %v after the timeout, want the first backoff delay", got)
		}
		hello = fc.expect(protocol.MessageTypeHello)
		if h, _ := protocol.Decode[protocol.Hello](hello); h.ResumeToken != "" {
			t.Error("hello after a failed handshake has a resume token")
		}
		fc.send(protocol.MessageTypeWelcome, hello.ID, f.welcome(1))
		d := <-res
		if d.err != nil {
			t.Fatalf("Dial: %v", d.err)
		}
		defer d.c.Close()
		back := expectStates(t, d.c, signal.StateConnecting, signal.StateHandshaking, signal.StateBackoff)
		if back.Err != nil {
			t.Errorf("backoff after a handshake timeout carries %v", back.Err)
		}
		expectStates(t, d.c, signal.StateConnecting, signal.StateHandshaking, signal.StateReady)
	})
}

// An upgrade that hangs is given up after 10 s.
func TestConnectTimeout(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		f.hang.Store(true)
		o := f.options()
		o.NoReconnect = true
		start := time.Now()
		c, err := signal.Dial(ctxT(t), o)
		if c != nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Dial = %v, %v; want the connect timeout", c, err)
		}
		if got := time.Since(start); got != signal.ConnectTimeout {
			t.Errorf("Dial gave up after %v, want %v", got, signal.ConnectTimeout)
		}
	})
}

// A socket that dies right after the upgrade, before or while the client says hello, is a failed attempt.
func TestSocketDiesBeforeWelcome(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		f.slam.Store(true)
		o := f.options()
		o.NoReconnect = true
		for range 20 { // the hello's write and the reader both notice; either may be first
			if c, err := signal.Dial(ctxT(t), o); c != nil || err == nil {
				t.Fatalf("Dial = %v, %v; want an error", c, err)
			}
		}
		o.NoReconnect = false
		go func() {
			time.Sleep(2500 * time.Millisecond)
			f.slam.Store(false)
		}()
		// Attempts at 0, about 0.5 and about 1.5 s fail; the one at 2.9 to 4.2 s gets through.
		start := time.Now()
		c, _ := f.dial(o)
		defer c.Close()
		if got := time.Since(start); got < 2900*time.Millisecond || got > 4200*time.Millisecond {
			t.Errorf("Dial returned after %v, want the fourth attempt's time", got)
		}
		for {
			sc := nextState(t, c)
			if sc.State == signal.StateReady {
				break
			}
			if sc.Err != nil || sc.State == signal.StateStopped {
				t.Fatalf("state change %+v (%v) before the first ready", sc, sc.Err)
			}
		}
	})
}

// A welcome that arrives in the instant the handshake timeout fires either counts or doesn't, and the client ends
// up ready either way: at once, or after one backoff.
func TestWelcomeRacesHandshakeTimeout(t *testing.T) {
	var late atomic.Int32
	for range 200 {
		bubble(t, func(t *testing.T, f *fakeServer) {
			f.manual.Store(true)
			quit := make(chan struct{})
			defer close(quit)
			go func() {
				fc := f.conn()
				id := fc.expect(protocol.MessageTypeHello).ID
				time.Sleep(signal.HandshakeTimeout)
				_ = fc.write(protocol.MessageTypeWelcome, id, f.welcome(1))
				select {
				case fc := <-f.conns: // the first welcome came too late
					late.Add(1)
					_ = fc.write(protocol.MessageTypeWelcome, (<-fc.msgs).ID, f.welcome(2))
				case <-quit:
				}
			}()
			c, err := signal.Dial(context.Background(), f.options())
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			defer c.Close()
			expectStates(t, c, signal.StateConnecting, signal.StateHandshaking)
			sc := nextState(t, c)
			if sc.State == signal.StateBackoff {
				sc = expectStates(t, c, signal.StateConnecting, signal.StateHandshaking, signal.StateReady)
			}
			if sc.State != signal.StateReady {
				t.Fatalf("state %+v, want ready", sc)
			}
			// The connection that made it works.
			done := request(ctxT(t), c, protocol.MessageTypeRoomLeave, protocol.Empty{}, nil)
			f.mu.Lock()
			fc := f.all[len(f.all)-1]
			f.mu.Unlock()
			fc.send(protocol.MessageTypeOK, fc.expect(protocol.MessageTypeRoomLeave).ID, nil)
			if err := result(t, done); err != nil {
				t.Errorf("request: %v", err)
			}
		})
	}
	t.Logf("the welcome came too late in %d of 200 runs", late.Load())
}

// A welcome followed at once by a revocation: Dial returns the client, stopped, and States tells why.
func TestWelcomeThenStop(t *testing.T) {
	for range 50 {
		bubble(t, func(t *testing.T, f *fakeServer) {
			f.manual.Store(true)
			go func() {
				fc := f.conn()
				_ = fc.write(protocol.MessageTypeWelcome, fc.expect(protocol.MessageTypeHello).ID, f.welcome(1))
				_ = fc.write(protocol.MessageTypeError, "", protocol.NewError(protocol.ErrorCodeSessionRevoked, protocol.ErrorScopeSession))
			}()
			c, err := signal.Dial(ctxT(t), f.options())
			if err != nil || c == nil {
				t.Fatalf("Dial = %v, %v; want the client that was welcomed", c, err)
			}
			sc := expectStates(t, c, signal.StateConnecting, signal.StateHandshaking, signal.StateReady, signal.StateStopped)
			if sc.Err == nil || sc.Err.Code != protocol.ErrorCodeSessionRevoked {
				t.Errorf("stopped with %v, want session_revoked", sc.Err)
			}
			expectStopped(t, c)
			if err := c.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
	}
}

// A welcome without a connection id or a resume token is a server bug: the attempt fails, with a warning.
func TestMalformedWelcome(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		f.manual.Store(true)
		var logs syncBuffer
		o := f.options()
		o.NoReconnect = true
		o.Logger = slog.New(slog.NewTextHandler(&logs, nil))
		for _, frame := range []string{
			`{"type":"welcome","re":"%s","data":{"protocol":1,"resumeToken":"r1.x"}}`,
			`{"type":"welcome","re":"%s","data":{"protocol":1,"connectionId":"c_aaaaaaaaaaaaaaaa"}}`,
			`{"type":"welcome","re":"%s","data":{"protocol":"one","connectionId":"c_aaaaaaaaaaaaaaaa","resumeToken":"r1.x"}}`,
			`{"type":"welcome","re":"%s"}`,
		} {
			go func() {
				fc := f.conn()
				fc.sendRaw(fmt.Sprintf(frame, fc.expect(protocol.MessageTypeHello).ID))
			}()
			c, err := signal.Dial(ctxT(t), o)
			if c != nil || err == nil || !strings.Contains(err.Error(), "malformed welcome") {
				t.Errorf("Dial after %s = %v, %v; want a malformed welcome", frame, c, err)
			}
			synctest.Wait()
		}
		if n := strings.Count(logs.String(), "level=WARN"); n != 4 {
			t.Errorf("%d warnings, want 4:\n%s", n, logs.String())
		}
	})
}

// The backoff sequence (01 §10.2) while the server refuses every upgrade: about 0.5, 1, 2, 4, 8, 10, 10, … s, each
// within ±20 % and within [0.5 s, 10 s], with the jitter drawn anew for each delay.
func TestBackoffSequence(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		c, fc := ready(t, f, f.options())
		defer c.Close()
		f.status.Store(http.StatusServiceUnavailable)
		fc.kill()
		var delays []time.Duration
		for n := range 12 {
			sc := expectBackoff(t, c)
			if sc.Err != nil {
				t.Errorf("backoff %d carries %v", n, sc.Err)
			}
			inBackoffBounds(t, n, sc.Delay)
			delays = append(delays, sc.Delay)
		}
		// Delays 1 to 4 are never clamped: with jitter they are not their nominal values.
		if delays[1] == time.Second && delays[2] == 2*time.Second && delays[3] == 4*time.Second && delays[4] == 8*time.Second {
			t.Errorf("delays %v are the nominal values: no jitter", delays)
		}
		synctest.Wait()
		if n := f.upgrades.Load(); n != 13 {
			t.Errorf("%d upgrade requests, want 13 (the first, then one per delay)", n)
		}
		// The server is back: the client resumes, and after 10 s ready the sequence starts over.
		f.status.Store(0)
		_, up, fc := reconnected(t, f, c)
		if !up.Resumed {
			t.Errorf("ready %+v, want resumed", up)
		}
		time.Sleep(signal.BackoffReset)
		fc.kill()
		back, _, _ := reconnected(t, f, c)
		inBackoffBounds(t, 0, back.Delay)
	})
}

// The reset rule (01 §10.2): n resets only after the connection has been ready for 10 s.
func TestBackoffReset(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		c, fc := ready(t, f, f.options())
		defer c.Close()
		for n := range 6 {
			time.Sleep(signal.BackoffReset - time.Millisecond)
			fc.kill()
			var back signal.StateChange
			back, _, fc = reconnected(t, f, c)
			inBackoffBounds(t, n, back.Delay)
		}
		time.Sleep(signal.BackoffReset)
		fc.kill()
		back, _, _ := reconnected(t, f, c)
		inBackoffBounds(t, 0, back.Delay)
	})
}

// RetryNow skips the current backoff wait once (01 §10.2); outside a backoff it does nothing.
func TestRetryNow(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		c, fc := ready(t, f, f.options())
		defer c.Close()
		c.RetryNow() // ready: nothing to skip
		noStates(t, c)
		fc.open()

		f.status.Store(http.StatusServiceUnavailable)
		fc.kill()
		for range 4 {
			expectBackoff(t, c)
		}
		sc := expectStates(t, c, signal.StateBackoff)
		inBackoffBounds(t, 4, sc.Delay) // about 8 s
		start := time.Now()
		c.RetryNow()
		c.RetryNow()
		expectStates(t, c, signal.StateConnecting)
		if got := time.Since(start); got != 0 {
			t.Errorf("RetryNow: the next attempt came after %v, want at once", got)
		}
		// Once: the wait after the attempt it started runs its course (the second call above did not stick).
		sc = expectBackoff(t, c)
		inBackoffBounds(t, 5, sc.Delay)
	})
}

// The heartbeat (01 §3.4): a ping every limits.pingIntervalMs carrying the client's clock, and the socket stays up
// as long as pongs come.
func TestHeartbeat(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		f.noPong.Store(true)
		c, fc := ready(t, f, f.options())
		defer c.Close()
		start := time.Now()
		for i := 1; i <= 4; i++ {
			env := fc.expect(protocol.MessageTypePing)
			p, err := protocol.Decode[protocol.Ping](env)
			if err != nil || env.ID != "" || p.T != time.Now().UnixMilli() {
				t.Errorf("ping %d: %s (id %q), %v; want t = now", i, env.Data, env.ID, err)
			}
			if got, want := time.Since(start), time.Duration(i)*signal.DefaultPingInterval; got != want {
				t.Errorf("ping %d after %v, want %v", i, got, want)
			}
			time.Sleep(signal.PongTimeout - time.Millisecond) // just in time
			fc.send(protocol.MessageTypePong, "", protocol.Pong{T: p.T, ServerTimeMs: p.T})
		}
		fc.open()
		noStates(t, c)
		noEvents(t, c) // pongs are the client's own business
	})
}

func TestHeartbeatInterval(t *testing.T) {
	for _, tc := range []struct {
		ms   int32
		want time.Duration
	}{
		{5000, 5 * time.Second},
		{-1, signal.DefaultPingInterval}, // no usable value: the default
		{20, time.Second},                // 50 pings a second would be a flood: once a second
	} {
		bubble(t, func(t *testing.T, f *fakeServer) {
			f.pingMs.Store(tc.ms)
			c, fc := ready(t, f, f.options())
			defer c.Close()
			time.Sleep(10*tc.want + tc.want/2)
			synctest.Wait()
			if n := fc.pings.Load(); n != 10 {
				t.Errorf("pingIntervalMs %d: %d pings in %v, want 10", tc.ms, n, 10*tc.want)
			}
		})
	}
}

// A ping without a pong within 10 s: the client drops the socket, without a close code so the server keeps the
// grace period, and resumes (01 §3.4, §11.5 A).
func TestPongTimeout(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		f.noPong.Store(true)
		c, fc := ready(t, f, f.options())
		defer c.Close()
		fc.expect(protocol.MessageTypePing)
		sent := time.Now()
		if code := fc.closed(); code != -1 {
			t.Errorf("the client ended the socket with close code %d, want none", code)
		}
		if got := time.Since(sent); got != signal.PongTimeout {
			t.Errorf("the client gave up %v after its ping, want %v", got, signal.PongTimeout)
		}
		f.noPong.Store(false)
		back, up, fc := reconnected(t, f, c)
		if back.Err != nil || !up.Resumed {
			t.Errorf("backoff %+v, ready %+v; want no error and a resume", back, up)
		}
		if h := fc.hello(); h.ResumeToken != "r1.token1" {
			t.Errorf("the hello after the timeout resumes with %q", h.ResumeToken.Reveal())
		}
	})
}

// A network path that goes black (01 §11.5): pings vanish, no pong comes, and 10 s after its ping the client drops
// the socket and resumes on a new one.
func TestBlackHole(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		path := &cutNet{net: f.srv.Net}
		o := f.options()
		o.HTTPClient = path.client()
		c, fc := ready(t, f, o)
		defer c.Close()
		start := time.Now()
		path.cut.Store(true)
		// A request made into the hole is lost with the socket, not sooner: nothing tells the client.
		done := request(ctxT(t), c, protocol.MessageTypeRoomJoin, protocol.RoomJoin{RoomID: "lounge"}, nil)
		back := expectStates(t, c, signal.StateBackoff)
		if got, want := time.Since(start), signal.DefaultPingInterval+signal.PongTimeout; got != want || back.Err != nil {
			t.Errorf("backoff %+v after %v, want a plain drop after %v", back, got, want)
		}
		// The request timed out first (10 s), 15 s before the drop.
		if err := result(t, done); !errors.Is(err, signal.ErrRequestTimeout) {
			t.Errorf("the request into the hole: %v, want ErrRequestTimeout", err)
		}
		path.cut.Store(false)
		if up := expectStates(t, c, signal.StateConnecting, signal.StateHandshaking, signal.StateReady); !up.Resumed {
			t.Errorf("ready %+v, want resumed", up)
		}
		if code := fc.closed(); code != -1 {
			t.Errorf("the server saw close code %d on the old socket, want none", code)
		}
	})
}

// A ping that can't be written ends the socket at once.
func TestPingWriteFails(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		path := &cutNet{net: f.srv.Net}
		o := f.options()
		o.HTTPClient = path.client()
		c, _ := ready(t, f, o)
		defer c.Close()
		start := time.Now()
		path.broken.Store(true)
		back := expectStates(t, c, signal.StateBackoff)
		if got := time.Since(start); got != signal.DefaultPingInterval || back.Err != nil {
			t.Errorf("backoff %+v after %v, want a plain drop at the first ping", back, got)
		}
		if err := c.Send(protocol.MessageTypeStats, protocol.ClientStats{}); !errors.Is(err, signal.ErrConnectionLost) {
			t.Errorf("Send: %v", err)
		}
		path.broken.Store(false)
		expectStates(t, c, signal.StateConnecting, signal.StateHandshaking, signal.StateReady)
		// A notification that can't be written fails with ErrConnectionLost, and the socket goes with it.
		path.broken.Store(true)
		if err := c.Send(protocol.MessageTypeStats, protocol.ClientStats{}); !errors.Is(err, signal.ErrConnectionLost) {
			t.Errorf("Send on a broken socket: %v, want ErrConnectionLost", err)
		}
		if err := c.Request(ctxT(t), protocol.MessageTypeRoomLeave, protocol.Empty{}, nil); !errors.Is(err, signal.ErrConnectionLost) {
			t.Errorf("Request on a broken socket: %v, want ErrConnectionLost", err)
		}
		expectStates(t, c, signal.StateBackoff)
		path.broken.Store(false)
	})
}

// A pong that arrives in the instant its deadline passes: the client stays up, or drops the socket and resumes.
func TestPongRacesDeadline(t *testing.T) {
	dropped := 0
	for range 100 {
		bubble(t, func(t *testing.T, f *fakeServer) {
			f.noPong.Store(true)
			c, fc := ready(t, f, f.options())
			defer c.Close()
			p, _ := protocol.Decode[protocol.Ping](fc.expect(protocol.MessageTypePing))
			time.Sleep(signal.PongTimeout)
			f.noPong.Store(false)
			_ = fc.write(protocol.MessageTypePong, "", protocol.Pong{T: p.T})
			synctest.Wait()
			select {
			case sc := <-c.States():
				dropped++
				if sc.State != signal.StateBackoff || sc.Err != nil {
					t.Fatalf("state change %+v, want a plain backoff", sc)
				}
				if up := expectStates(t, c, signal.StateConnecting, signal.StateHandshaking, signal.StateReady); !up.Resumed {
					t.Errorf("ready %+v, want resumed", up)
				}
			default:
				time.Sleep(time.Minute)
				fc.open()
				noStates(t, c)
			}
		})
	}
	t.Logf("the pong came too late in %d of 100 runs", dropped)
}

// Probe (01 §3.4): a ping at once with a 3 s timeout. It can shorten the deadline of a ping that is out and never
// lengthens it; a pong answers every ping out; outside ready it does nothing.
func TestProbe(t *testing.T) {
	t.Run("times out after 3 s", func(t *testing.T) {
		bubble(t, func(t *testing.T, f *fakeServer) {
			f.noPong.Store(true)
			c, fc := ready(t, f, f.options())
			defer c.Close()
			time.Sleep(time.Second)
			start := time.Now()
			c.Probe()
			fc.expect(protocol.MessageTypePing)
			if got := time.Since(start); got != 0 {
				t.Errorf("the probe's ping came after %v", got)
			}
			fc.closed()
			if got := time.Since(start); got != signal.ProbeTimeout {
				t.Errorf("the client gave up after %v, want %v", got, signal.ProbeTimeout)
			}
		})
	})
	t.Run("shortens the deadline of a ping that is out", func(t *testing.T) {
		bubble(t, func(t *testing.T, f *fakeServer) {
			f.noPong.Store(true)
			c, fc := ready(t, f, f.options())
			defer c.Close()
			start := time.Now()
			fc.expect(protocol.MessageTypePing) // at 15 s, due at 25 s
			time.Sleep(time.Second)
			c.Probe() // at 16 s, due at 19 s
			fc.expect(protocol.MessageTypePing)
			fc.closed()
			if got, want := time.Since(start), signal.DefaultPingInterval+time.Second+signal.ProbeTimeout; got != want {
				t.Errorf("the client gave up after %v, want %v", got, want)
			}
		})
	})
	t.Run("never lengthens it", func(t *testing.T) {
		bubble(t, func(t *testing.T, f *fakeServer) {
			f.noPong.Store(true)
			c, fc := ready(t, f, f.options())
			defer c.Close()
			start := time.Now()
			time.Sleep(signal.DefaultPingInterval - time.Second)
			c.Probe()                           // at 14 s, due at 17 s
			fc.expect(protocol.MessageTypePing) // the probe's
			fc.expect(protocol.MessageTypePing) // the periodic one at 15 s, which would be due at 25 s
			fc.closed()
			if got, want := time.Since(start), signal.DefaultPingInterval-time.Second+signal.ProbeTimeout; got != want {
				t.Errorf("the client gave up after %v, want %v", got, want)
			}
		})
	})
	t.Run("a pong answers every ping out", func(t *testing.T) {
		bubble(t, func(t *testing.T, f *fakeServer) {
			f.noPong.Store(true)
			c, fc := ready(t, f, f.options())
			defer c.Close()
			fc.expect(protocol.MessageTypePing)
			c.Probe()
			p, _ := protocol.Decode[protocol.Ping](fc.expect(protocol.MessageTypePing))
			fc.send(protocol.MessageTypePong, "", protocol.Pong{T: p.T})
			f.noPong.Store(false)
			time.Sleep(time.Minute)
			fc.open()
			noStates(t, c)
		})
	})
	t.Run("does nothing unless ready", func(t *testing.T) {
		bubble(t, func(t *testing.T, f *fakeServer) {
			c, fc := ready(t, f, f.options())
			defer c.Close()
			f.status.Store(http.StatusServiceUnavailable)
			fc.kill()
			expectStates(t, c, signal.StateBackoff)
			c.Probe()
			f.status.Store(0)
			expectStates(t, c, signal.StateConnecting, signal.StateHandshaking, signal.StateReady)
			fc = f.conn()
			time.Sleep(signal.DefaultPingInterval - time.Second)
			synctest.Wait()
			if n := fc.pings.Load(); n != 0 {
				t.Errorf("%d pings before the first interval: the probe made in backoff was kept", n)
			}
		})
	})
}

// Requests (01 §5, §12.4): each has its own id and gets its own reply, whatever the order.
func TestRequestReplies(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		c, fc := ready(t, f, f.options())
		defer c.Close()
		ctx := ctxT(t)

		var joined protocol.RoomJoinResult
		a := request(ctx, c, protocol.MessageTypeRoomJoin, protocol.RoomJoin{RoomID: "lounge"}, &joined)
		reqA := fc.expect(protocol.MessageTypeRoomJoin)
		var params protocol.ShareParams
		b := request(ctx, c, protocol.MessageTypeShareStart, protocol.ShareStart{Kind: protocol.ShareKindScreen,
			Preset: protocol.PresetAuto, Ref: "r1"}, &params)
		reqB := fc.expect(protocol.MessageTypeShareStart)
		empty := protocol.Empty{}
		d := request(ctx, c, protocol.MessageTypeRoomLeave, protocol.Empty{}, &empty)
		reqD := fc.expect(protocol.MessageTypeRoomLeave)
		if reqA.ID == "" || reqB.ID == "" || reqD.ID == "" || reqA.ID == reqB.ID || reqB.ID == reqD.ID || reqA.ID == reqD.ID {
			t.Fatalf("request ids %q, %q, %q; want three different ones", reqA.ID, reqB.ID, reqD.ID)
		}
		if j, err := protocol.Decode[protocol.RoomJoin](reqA); err != nil || j.RoomID != "lounge" {
			t.Errorf("room.join payload %s, %v", reqA.Data, err)
		}

		// Answered in another order, with a notification and replies to nobody in between.
		fc.send(protocol.MessageTypeOK, reqD.ID, nil)
		fc.send(protocol.MessageTypeOK, "12345", protocol.RoomJoinResult{})
		fc.send(protocol.MessageTypeError, "12345", protocol.NewError(protocol.ErrorCodeForbidden, protocol.ErrorScopeRequest))
		fc.send(protocol.MessageTypeRoomState, "", protocol.RoomState{RoomID: "lounge", Rev: 1})
		e := protocol.NewError(protocol.ErrorCodeShareLimit, protocol.ErrorScopeRequest)
		e.Params = map[string]any{"limit": 4, "per": "user"}
		fc.send(protocol.MessageTypeError, reqB.ID, e)
		fc.send(protocol.MessageTypeOK, reqA.ID, protocol.RoomJoinResult{Room: protocol.RoomInfo{ID: "lounge", Name: "Lounge"}})

		if err := result(t, d); err != nil {
			t.Errorf("room.leave: %v", err)
		}
		err := result(t, b)
		var pe *protocol.Error
		if !errors.As(err, &pe) || pe.Code != protocol.ErrorCodeShareLimit || pe.Scope != protocol.ErrorScopeRequest ||
			pe.Params["per"] != "user" || pe.Params["limit"] != float64(4) {
			t.Errorf("share.start: %v (%+v), want share_limit with its params", err, pe)
		}
		if err := result(t, a); err != nil || joined.Room.Name != "Lounge" {
			t.Errorf("room.join: %v, result %+v", err, joined)
		}
		// Replies go to their requests only; the notification is the one thing in Events.
		expectEvent(t, c, protocol.MessageTypeRoomState)
		noEvents(t, c)
		noStates(t, c)
	})
}

// Errors of codes and scopes this build doesn't know come back as they are (01 §12.3), and a reply that can't be
// decoded is an error, not a zero result.
func TestRequestOddReplies(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		c, fc := ready(t, f, f.options())
		defer c.Close()
		ctx := ctxT(t)

		done := request(ctx, c, protocol.MessageTypeRoomLeave, protocol.Empty{}, nil)
		fc.sendRaw(fmt.Sprintf(`{"type":"error","re":"%s","data":{"code":"from_the_future","scope":"request",`+
			`"retryable":true,"retryAfterMs":1500,"extra":[1,2]}}`, fc.expect(protocol.MessageTypeRoomLeave).ID))
		var pe *protocol.Error
		if err := result(t, done); !errors.As(err, &pe) || pe.Code != "from_the_future" || !pe.Retryable ||
			pe.RetryAfterMs != 1500 {
			t.Errorf("unknown code: %v", err)
		}

		// A malformed error payload: internal in scope request (like the web client).
		done = request(ctx, c, protocol.MessageTypeRoomLeave, protocol.Empty{}, nil)
		fc.sendRaw(fmt.Sprintf(`{"type":"error","re":"%s","data":{"retryable":"yes"}}`,
			fc.expect(protocol.MessageTypeRoomLeave).ID))
		if err := result(t, done); !errors.As(err, &pe) || pe.Code != protocol.ErrorCodeInternal ||
			pe.Scope != protocol.ErrorScopeRequest || pe.Retryable {
			t.Errorf("malformed error: %v", err)
		}
		done = request(ctx, c, protocol.MessageTypeRoomLeave, protocol.Empty{}, nil)
		fc.sendRaw(fmt.Sprintf(`{"type":"error","re":"%s"}`, fc.expect(protocol.MessageTypeRoomLeave).ID))
		if err := result(t, done); !errors.As(err, &pe) || pe.Code != protocol.ErrorCodeInternal {
			t.Errorf("error without data: %v", err)
		}

		// An ok whose payload does not fit the result.
		var joined protocol.RoomJoinResult
		done = request(ctx, c, protocol.MessageTypeRoomJoin, protocol.RoomJoin{RoomID: "lounge"}, &joined)
		fc.sendRaw(fmt.Sprintf(`{"type":"ok","re":"%s","data":{"room":"lounge"}}`,
			fc.expect(protocol.MessageTypeRoomJoin).ID))
		if err := result(t, done); err == nil || errors.As(err, &pe) || !strings.Contains(err.Error(), "decode the reply") {
			t.Errorf("undecodable ok: %v, want a decode error", err)
		}
		// An ok without data is an empty result (01 §5: absent means {}).
		var empty protocol.RoomJoinResult
		done = request(ctx, c, protocol.MessageTypeRoomJoin, protocol.RoomJoin{RoomID: "lounge"}, &empty)
		fc.send(protocol.MessageTypeOK, fc.expect(protocol.MessageTypeRoomJoin).ID, nil)
		if err := result(t, done); err != nil || empty != (protocol.RoomJoinResult{}) {
			t.Errorf("ok without data: %v, result %+v", err, empty)
		}
		// A payload that can't be encoded never leaves.
		if err := c.Request(ctx, protocol.MessageTypeRoomJoin, func() {}, nil); err == nil {
			t.Error("Request with an unencodable payload succeeded")
		}
		if err := c.Send(protocol.MessageTypeStats, func() {}); err == nil {
			t.Error("Send with an unencodable payload succeeded")
		}
		fc.open()
		noStates(t, c)
	})
}

// request_timeout after 10 s without a reply; a late reply is dropped. The caller's context ends the wait too,
// without taking the socket with it.
func TestRequestTimeout(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		c, fc := ready(t, f, f.options())
		defer c.Close()

		start := time.Now()
		done := request(ctxT(t), c, protocol.MessageTypeRoomLeave, protocol.Empty{}, nil)
		late := fc.expect(protocol.MessageTypeRoomLeave)
		if err := result(t, done); !errors.Is(err, signal.ErrRequestTimeout) {
			t.Errorf("request without a reply: %v, want ErrRequestTimeout", err)
		}
		if got := time.Since(start); got != signal.RequestTimeout {
			t.Errorf("the request timed out after %v, want %v", got, signal.RequestTimeout)
		}
		fc.send(protocol.MessageTypeOK, late.ID, nil)

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		start = time.Now()
		done = request(ctx, c, protocol.MessageTypeRoomLeave, protocol.Empty{}, nil)
		late = fc.expect(protocol.MessageTypeRoomLeave)
		if err := result(t, done); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("request with a 2 s context: %v, want its deadline", err)
		}
		if got := time.Since(start); got != 2*time.Second {
			t.Errorf("the request returned after %v, want 2 s", got)
		}
		fc.send(protocol.MessageTypeError, late.ID, protocol.NewError(protocol.ErrorCodeForbidden, protocol.ErrorScopeRequest))
		if err := c.Request(ctx, protocol.MessageTypeRoomLeave, protocol.Empty{}, nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("request with a context that is over: %v", err)
		}

		// The socket is still the same and still works.
		done = request(ctxT(t), c, protocol.MessageTypeRoomLeave, protocol.Empty{}, nil)
		fc.send(protocol.MessageTypeOK, fc.expect(protocol.MessageTypeRoomLeave).ID, nil)
		if err := result(t, done); err != nil {
			t.Errorf("request after the timeouts: %v", err)
		}
		noEvents(t, c)
		noStates(t, c)
	})
}

// connection_lost (01 §12.4): requests that wait for a reply when the socket goes away fail at once and are not
// sent again; while the client is not ready, requests and notifications fail without being sent.
func TestConnectionLost(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		c, fc := ready(t, f, f.options())
		defer c.Close()
		a := request(ctxT(t), c, protocol.MessageTypeRoomJoin, protocol.RoomJoin{RoomID: "lounge"}, nil)
		b := request(ctxT(t), c, protocol.MessageTypeRoomLeave, protocol.Empty{}, nil)
		fc.next()
		fc.next()
		start := time.Now()
		f.status.Store(http.StatusServiceUnavailable)
		fc.kill()
		for _, done := range []<-chan error{a, b} {
			if err := result(t, done); !errors.Is(err, signal.ErrConnectionLost) {
				t.Errorf("request cut off by the drop: %v, want ErrConnectionLost", err)
			}
		}
		if got := time.Since(start); got != 0 {
			t.Errorf("the requests failed %v after the drop, want at once", got)
		}
		expectStates(t, c, signal.StateBackoff)
		if err := c.Request(ctxT(t), protocol.MessageTypeRoomLeave, protocol.Empty{}, nil); !errors.Is(err, signal.ErrConnectionLost) {
			t.Errorf("Request in backoff: %v", err)
		}
		expectStates(t, c, signal.StateConnecting)
		if err := c.Send(protocol.MessageTypeStats, protocol.ClientStats{}); !errors.Is(err, signal.ErrConnectionLost) {
			t.Errorf("Send while connecting: %v", err)
		}

		synctest.Wait() // the attempt that is out gets its 503
		f.status.Store(0)
		_, _, fc = reconnected(t, f, c)
		fc.expect(protocol.MessageTypeHello)
		// Nothing was queued and nothing is retried: the new socket is silent.
		synctest.Wait()
		select {
		case env := <-fc.msgs:
			t.Errorf("the client sent %s on the new socket by itself", env.Type)
		default:
		}
	})
}

// Notifications (01 §7, §8.13): Events carries every known server notification in order, error notifications of
// the scopes that don't end the socket included; replies, pongs, unknown types and malformed frames never show up.
func TestEvents(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		c, fc := ready(t, f, f.options())
		defer c.Close()

		fc.send(protocol.MessageTypeRoomState, "", protocol.RoomState{RoomID: "lounge", Rev: 7})
		fc.sendRaw(`{"type":"brand.new","data":{"x":1}}`)
		fc.sendRaw(`not json`)
		fc.sendRaw(`{"type":""}`)
		fc.sendRaw(`[]`)
		if err := fc.ws.Write(context.Background(), websocket.MessageBinary, []byte{0, 1, 2}); err != nil {
			t.Fatal(err)
		}
		fc.send(protocol.MessageTypePong, "", protocol.Pong{T: 1})
		fc.send(protocol.MessageTypeOK, "999", nil)
		fc.send(protocol.MessageTypeWelcome, "999", f.welcome(9))
		fc.send(protocol.MessageTypePCOffer, "", protocol.PCOffer{PC: protocol.PCKindSub, Gen: 1, Neg: 1, SDP: "v=0"})
		pcErr := protocol.NewError(protocol.ErrorCodeSDPInvalid, protocol.ErrorScopePC)
		pcErr.PC, pcErr.Gen, pcErr.Neg = protocol.PCKindSub, 1, 1
		fc.send(protocol.MessageTypeError, "", pcErr)
		fc.sendRaw(`{"type":"error","data":{"code":"from_the_future","scope":"galaxy","retryable":false}}`)
		fc.send(protocol.MessageTypeError, "", protocol.NewError(protocol.ErrorCodeRoomClosed, protocol.ErrorScopeRoom))
		fc.send(protocol.MessageTypeInvalidate, "", protocol.Invalidate{Topics: []protocol.Topic{protocol.TopicRooms}})
		fc.sendRaw(`{"type":"room.event"}`)

		if st, err := protocol.Decode[protocol.RoomState](nextEvent(t, c)); err != nil || st.Rev != 7 {
			t.Errorf("first notification: %+v, %v; want room.state rev 7", st, err)
		}
		if o, err := protocol.Decode[protocol.PCOffer](expectEvent(t, c, protocol.MessageTypePCOffer)); err != nil ||
			o.SDP.Reveal() != "v=0" {
			t.Errorf("pc.offer: %+v, %v", o, err)
		}
		if e, err := protocol.Decode[protocol.Error](expectEvent(t, c, protocol.MessageTypeError)); err != nil ||
			e.Code != protocol.ErrorCodeSDPInvalid || e.PC != protocol.PCKindSub || e.Gen != 1 {
			t.Errorf("pc error: %+v, %v", e, err)
		}
		if e, err := protocol.Decode[protocol.Error](expectEvent(t, c, protocol.MessageTypeError)); err != nil ||
			e.Code != "from_the_future" || e.Scope != "galaxy" {
			t.Errorf("error of an unknown scope: %+v, %v", e, err)
		}
		if e, err := protocol.Decode[protocol.Error](expectEvent(t, c, protocol.MessageTypeError)); err != nil ||
			e.Code != protocol.ErrorCodeRoomClosed {
			t.Errorf("room error: %+v, %v", e, err)
		}
		expectEvent(t, c, protocol.MessageTypeInvalidate)
		if env := expectEvent(t, c, protocol.MessageTypeRoomEvent); len(env.Data) != 0 {
			t.Errorf("room.event without data has data %s", env.Data)
		}
		noEvents(t, c)
		noStates(t, c)
		fc.open()
		if w := c.Welcome(); w.ResumeToken != "r1.token1" {
			t.Errorf("a welcome that answers nothing replaced the welcome: token %q", w.ResumeToken.Reveal())
		}
	})
}

// roomStates sends n room.state notifications with revs from, from+1, …
func roomStates(fc *fakeConn, from, n uint64) {
	for rev := from; rev < from+n; rev++ {
		if fc.write(protocol.MessageTypeRoomState, "", protocol.RoomState{RoomID: "lounge", Rev: rev}) != nil {
			return
		}
	}
}

// revs reads n room.state notifications and checks their revs: from, from+1, …
func revs(t *testing.T, c *signal.Client, from, n uint64) {
	t.Helper()
	for rev := from; rev < from+n; rev++ {
		st, err := protocol.Decode[protocol.RoomState](expectEvent(t, c, protocol.MessageTypeRoomState))
		if err != nil || st.Rev != rev {
			t.Fatalf("got room.state rev %d, %v; want rev %d", st.Rev, err, rev)
		}
	}
}

// What a connection left in Events survives a resume, in order: it is the same connection. It does not survive a
// welcome that is not resumed (01 §10.5): by the time that state is reported, Events holds nothing of the
// connection that is gone.
func TestEventsAcrossReconnects(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		c, fc := ready(t, f, f.options())
		defer c.Close()

		roomStates(fc, 1, 3)
		synctest.Wait()
		fc.kill()
		_, up, fc := reconnected(t, f, c)
		if !up.Resumed {
			t.Fatalf("ready %+v, want resumed", up)
		}
		roomStates(fc, 4, 2)
		revs(t, c, 1, 5)

		roomStates(fc, 6, 3)
		synctest.Wait()
		f.noResume.Store(true)
		fc.kill()
		_, up, fc = reconnected(t, f, c)
		if up.Resumed || c.Welcome().ConnectionID == "c_aaaaaaaaaaaaaaaa" {
			t.Fatalf("ready %+v on connection %s, want a new connection", up, c.Welcome().ConnectionID)
		}
		noEvents(t, c)
		roomStates(fc, 100, 2)
		revs(t, c, 100, 2)
	})
}

// A caller that does not receive from Events holds the client's reader back once 256 notifications wait; nothing
// is dropped or reordered, and the client goes on when the caller does.
func TestEventsBackpressure(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		f.pingMs.Store(300000)
		c, fc := ready(t, f, f.options())
		defer c.Close()
		sent := make(chan struct{})
		go func() {
			defer close(sent)
			roomStates(fc, 1, 600)
		}()
		synctest.Wait()
		if n := len(c.Events()); n != 256 {
			t.Errorf("%d notifications wait in Events, want 256", n)
		}
		select {
		case <-sent:
			t.Error("the server could send 600 notifications to a client that receives none")
		default:
		}
		revs(t, c, 1, 600)
		<-sent
		noEvents(t, c)
		noStates(t, c)
	})
}

// Close does not wait for a caller that has stopped receiving.
func TestCloseWithFullEvents(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		c, fc := ready(t, f, f.options())
		sent := make(chan struct{})
		go func() {
			defer close(sent)
			roomStates(fc, 1, 400)
		}()
		synctest.Wait()
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		<-sent
		if code := fc.closed(); code != websocket.StatusNormalClosure {
			t.Errorf("the client closed with %d, want 1000", code)
		}
		n := 0
		for range c.Events() {
			n++
		}
		if n < 256 || n > 257 {
			t.Errorf("%d notifications were left in Events, want the 256 it holds (and at most the one in hand)", n)
		}
	})
}

// States never blocks the client: with nobody receiving, it keeps the last 64 changes, in order.
func TestStatesKeepTheLatest(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		c, fc := f.dial(f.options())
		defer c.Close()
		const drops = 30 // 3 changes for the first connection, then 4 per drop
		for range drops {
			synctest.Wait() // ready
			fc.kill()
			fc = f.conn()
		}
		synctest.Wait()
		var got []signal.State
		for len(c.States()) > 0 {
			got = append(got, nextState(t, c).State)
		}
		if len(got) != 64 {
			t.Fatalf("%d changes wait in States, want 64", len(got))
		}
		cycle := []signal.State{signal.StateBackoff, signal.StateConnecting, signal.StateHandshaking, signal.StateReady}
		for i, s := range got {
			if want := cycle[i%4]; s != want {
				t.Fatalf("change %d of the last 64 is %s, want %s: %v", i, s, want, got)
			}
		}
		if w := c.Welcome(); w.ResumeToken != protocol.Secret(fmt.Sprintf("r1.token%d", drops+1)) {
			t.Errorf("the client is at welcome %q, want number %d", w.ResumeToken.Reveal(), drops+1)
		}
	})
}

// closeCase is a way for a socket to end, and what the client makes of it.
type closeCase struct {
	name string
	// end ends the socket from the server side.
	end func(fc *fakeConn)
	// stop: the client stops; otherwise it backs off.
	stop bool
	// code is the error the state carries, "" for none.
	code  protocol.ErrorCode
	scope protocol.ErrorScope
	// delay is the backoff delay; 0 means the first delay of the normal sequence.
	delay time.Duration
}

func (tc closeCase) run(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		c, fc := ready(t, f, f.options())
		defer c.Close()
		done := request(ctxT(t), c, protocol.MessageTypeRoomLeave, protocol.Empty{}, nil)
		fc.expect(protocol.MessageTypeRoomLeave)
		tc.end(fc)
		if err := result(t, done); !errors.Is(err, signal.ErrConnectionLost) {
			t.Errorf("the request that waited: %v, want ErrConnectionLost", err)
		}
		sc := nextState(t, c)
		switch {
		case tc.code == "" && sc.Err != nil:
			t.Errorf("%s carries %v, want no error", sc.State, sc.Err)
		case tc.code != "" && (sc.Err == nil || sc.Err.Code != tc.code || sc.Err.Scope != tc.scope):
			t.Errorf("%s carries %v, want %s in scope %s", sc.State, sc.Err, tc.code, tc.scope)
		}
		if tc.stop {
			if sc.State != signal.StateStopped {
				t.Fatalf("state %s, want stopped", sc.State)
			}
			if rest := expectStopped(t, c); len(rest) != 0 {
				t.Errorf("state changes after stopped: %+v", rest)
			}
			time.Sleep(time.Minute)
			synctest.Wait()
			if n := f.upgrades.Load(); n != 1 {
				t.Errorf("%d upgrade requests, want 1: a stopped client does not come back", n)
			}
			return
		}
		if sc.State != signal.StateBackoff {
			t.Fatalf("state %s, want backoff", sc.State)
		}
		if tc.delay == 0 {
			inBackoffBounds(t, 0, sc.Delay)
		} else if sc.Delay != tc.delay {
			t.Errorf("backoff delay %v, want %v", sc.Delay, tc.delay)
		}
		start := time.Now()
		up := expectStates(t, c, signal.StateConnecting, signal.StateHandshaking, signal.StateReady)
		if got := time.Since(start); got != sc.Delay || !up.Resumed {
			t.Errorf("ready %+v after %v, want resumed after %v", up, got, sc.Delay)
		}
	})
}

// Close codes without a preceding error (01 §12.2, the column "Client without a preceding error").
func TestCloseCodes(t *testing.T) {
	closing := func(code protocol.CloseCode) func(*fakeConn) {
		return func(fc *fakeConn) { fc.closeWith(code) }
	}
	conn, sess := protocol.ErrorScopeConnection, protocol.ErrorScopeSession
	for _, tc := range []closeCase{
		{name: "no close frame (1006)", end: (*fakeConn).kill},
		{name: "1000", end: closing(protocol.CloseCodeNormal)},
		{name: "1001", end: closing(protocol.CloseCodeGoingAway)},
		{name: "1003", end: closing(protocol.CloseCodeUnsupportedData), stop: true, code: protocol.ErrorCodeBadMessage, scope: conn},
		{name: "1009", end: closing(protocol.CloseCodeMessageTooBig)},
		{name: "1011", end: closing(protocol.CloseCodeInternalError)},
		{name: "1012", end: closing(protocol.CloseCodeServiceRestart)},
		{name: "4400", end: closing(protocol.CloseCodeProtocolViolation), stop: true, code: protocol.ErrorCodeBadMessage, scope: conn},
		{name: "4401", end: closing(protocol.CloseCodeUnauthenticated), stop: true, code: protocol.ErrorCodeUnauthenticated, scope: sess},
		{name: "4403", end: closing(protocol.CloseCodeForbidden), stop: true, code: protocol.ErrorCodeForbidden, scope: conn},
		{name: "4408", end: closing(protocol.CloseCodeTimeout)},
		{name: "4409", end: closing(protocol.CloseCodeReplaced), stop: true, code: protocol.ErrorCodeReplaced, scope: conn},
		{name: "4426", end: closing(protocol.CloseCodeVersionUnsupported), stop: true, code: protocol.ErrorCodeProtocolUnsupported, scope: conn},
		{name: "4429", end: closing(protocol.CloseCodeRateLimited), code: protocol.ErrorCodeRateLimited, scope: conn, delay: signal.RateLimitMinWait},
		{name: "4503", end: closing(protocol.CloseCodeSlowConnection)},
		{name: "a code from the future (4777)", end: closing(4777)},
	} {
		t.Run(tc.name, tc.run)
	}
}

// Errors in scope connection and session (01 §12.1, §12.3): the error decides, the close code that follows is only
// the fallback, and the client does not wait for it.
func TestConnectionErrors(t *testing.T) {
	failing := func(code protocol.ErrorCode, scope protocol.ErrorScope) func(*fakeConn) {
		return func(fc *fakeConn) { fc.fail(protocol.NewError(code, scope), "") }
	}
	raw := func(code string, scope protocol.ErrorScope, retryable bool, closeCode protocol.CloseCode) func(*fakeConn) {
		return func(fc *fakeConn) {
			fc.send(protocol.MessageTypeError, "", protocol.Error{Code: protocol.ErrorCode(code), Scope: scope, Retryable: retryable})
			if closeCode != 0 {
				fc.closeWith(closeCode)
			}
		}
	}
	conn, sess := protocol.ErrorScopeConnection, protocol.ErrorScopeSession
	for _, tc := range []closeCase{
		{name: "idle_timeout", end: failing(protocol.ErrorCodeIdleTimeout, conn), code: protocol.ErrorCodeIdleTimeout, scope: conn},
		{name: "slow_connection", end: failing(protocol.ErrorCodeSlowConnection, conn), code: protocol.ErrorCodeSlowConnection, scope: conn},
		{name: "internal", end: failing(protocol.ErrorCodeInternal, conn), code: protocol.ErrorCodeInternal, scope: conn},
		{name: "server_shutdown without the notice", end: failing(protocol.ErrorCodeServerShutdown, conn), code: protocol.ErrorCodeServerShutdown, scope: conn},
		{name: "bad_message", end: failing(protocol.ErrorCodeBadMessage, conn), stop: true, code: protocol.ErrorCodeBadMessage, scope: conn},
		{name: "bad_request for a second hello", end: failing(protocol.ErrorCodeBadRequest, conn), stop: true, code: protocol.ErrorCodeBadRequest, scope: conn},
		{name: "too_many_connections", end: failing(protocol.ErrorCodeTooManyConnections, conn), stop: true, code: protocol.ErrorCodeTooManyConnections, scope: conn},
		{name: "replaced", end: failing(protocol.ErrorCodeReplaced, conn), stop: true, code: protocol.ErrorCodeReplaced, scope: conn},
		{name: "session_revoked", end: failing(protocol.ErrorCodeSessionRevoked, sess), stop: true, code: protocol.ErrorCodeSessionRevoked, scope: sess},
		{name: "account_disabled", end: failing(protocol.ErrorCodeAccountDisabled, sess), stop: true, code: protocol.ErrorCodeAccountDisabled, scope: sess},
		{name: "unknown code, connection, retryable", end: raw("from_the_future", conn, true, 4777), code: "from_the_future", scope: conn},
		{name: "unknown code, connection, final", end: raw("from_the_future", conn, false, 4777), stop: true, code: "from_the_future", scope: conn},
		{name: "unknown code, session, retryable", end: raw("from_the_future", sess, true, 4777), stop: true, code: "from_the_future", scope: sess},
		// The error decides: a retryable one backs off even before a close code that alone would stop, and the
		// other way round.
		{name: "retryable error, then 4400", end: raw("internal", conn, true, protocol.CloseCodeProtocolViolation), code: protocol.ErrorCodeInternal, scope: conn},
		{name: "final error, then 1012", end: raw("bad_message", conn, false, protocol.CloseCodeServiceRestart), stop: true, code: protocol.ErrorCodeBadMessage, scope: conn},
		// And it acts at once: the server never closes here.
		{name: "retryable error, no close", end: raw("internal", conn, true, 0), code: protocol.ErrorCodeInternal, scope: conn},
		{name: "final error, no close", end: raw("session_revoked", sess, false, 0), stop: true, code: protocol.ErrorCodeSessionRevoked, scope: sess},
	} {
		t.Run(tc.name, tc.run)
	}
}

// An error that answers a request in scope connection goes to the request and ends the socket.
func TestRequestErrorInScopeConnection(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		c, fc := ready(t, f, f.options())
		defer c.Close()
		done := request(ctxT(t), c, protocol.MessageTypeRoomLeave, protocol.Empty{}, nil)
		fc.fail(protocol.NewError(protocol.ErrorCodeInternal, protocol.ErrorScopeConnection),
			fc.expect(protocol.MessageTypeRoomLeave).ID)
		var pe *protocol.Error
		if err := result(t, done); !errors.As(err, &pe) || pe.Code != protocol.ErrorCodeInternal {
			t.Errorf("the request: %v, want the server's internal error", err)
		}
		if sc := expectStates(t, c, signal.StateBackoff); sc.Err == nil || sc.Err.Code != protocol.ErrorCodeInternal {
			t.Errorf("backoff carries %v, want internal", sc.Err)
		}
	})
}

// Errors that answer hello (01 §8.2, §12.3): a final one stops whatever its scope, and Dial returns it; a
// retryable one is a failed attempt.
func TestHelloErrors(t *testing.T) {
	answer := func(f *fakeServer, e protocol.Error) {
		go func() {
			fc := f.conn()
			fc.fail(e, fc.expect(protocol.MessageTypeHello).ID)
		}()
	}
	t.Run("protocol_unsupported", func(t *testing.T) {
		bubble(t, func(t *testing.T, f *fakeServer) {
			f.manual.Store(true)
			e := protocol.NewError(protocol.ErrorCodeProtocolUnsupported, protocol.ErrorScopeConnection)
			e.Params = map[string]any{"serverMin": 2, "serverMax": 3, "serverVersion": "0.9.0"}
			answer(f, e)
			c, err := signal.Dial(ctxT(t), f.options())
			var pe *protocol.Error
			if c != nil || !errors.As(err, &pe) || pe.Code != protocol.ErrorCodeProtocolUnsupported ||
				pe.Params["serverVersion"] != "0.9.0" || pe.Params["serverMin"] != float64(2) {
				t.Errorf("Dial = %v, %v; want protocol_unsupported with its params", c, err)
			}
			synctest.Wait()
			if n := f.upgrades.Load(); n != 1 {
				t.Errorf("%d upgrade requests, want 1", n)
			}
		})
	})
	t.Run("final in any scope", func(t *testing.T) {
		for _, e := range []protocol.Error{
			protocol.NewError(protocol.ErrorCodeClientOutdated, protocol.ErrorScopeConnection),
			protocol.NewError(protocol.ErrorCodeUnauthenticated, protocol.ErrorScopeSession),
			protocol.NewError(protocol.ErrorCodeBadRequest, protocol.ErrorScopeRequest),
			{Code: "from_the_future", Scope: "galaxy"},
		} {
			bubble(t, func(t *testing.T, f *fakeServer) {
				f.manual.Store(true)
				answer(f, e)
				c, err := signal.Dial(ctxT(t), f.options())
				var pe *protocol.Error
				if c != nil || !errors.As(err, &pe) || pe.Code != e.Code || pe.Scope != e.Scope {
					t.Errorf("Dial after %s = %v, %v", e.Code, c, err)
				}
			})
		}
	})
	t.Run("retryable", func(t *testing.T) {
		for _, e := range []protocol.Error{
			protocol.NewError(protocol.ErrorCodeInternal, protocol.ErrorScopeConnection),
			protocol.NewError(protocol.ErrorCodeHelloTimeout, protocol.ErrorScopeConnection),
			{Code: protocol.ErrorCodeInternal, Scope: protocol.ErrorScopeRequest, Retryable: true},
		} {
			bubble(t, func(t *testing.T, f *fakeServer) {
				f.manual.Store(true)
				go func() {
					fc := f.conn()
					fc.fail(e, fc.expect(protocol.MessageTypeHello).ID)
					fc = f.conn()
					fc.send(protocol.MessageTypeWelcome, fc.expect(protocol.MessageTypeHello).ID, f.welcome(1))
				}()
				c, err := signal.Dial(ctxT(t), f.options())
				if err != nil {
					t.Fatalf("Dial after a retryable %s: %v", e.Code, err)
				}
				defer c.Close()
				back := expectStates(t, c, signal.StateConnecting, signal.StateHandshaking, signal.StateBackoff)
				if back.Err == nil || back.Err.Code != e.Code {
					t.Errorf("backoff carries %v, want %s", back.Err, e.Code)
				}
				inBackoffBounds(t, 0, back.Delay)
				expectStates(t, c, signal.StateConnecting, signal.StateHandshaking, signal.StateReady)
			})
		}
	})
	// Once the welcome has arrived, an error with the hello's id is like any other.
	t.Run("after the welcome", func(t *testing.T) {
		bubble(t, func(t *testing.T, f *fakeServer) {
			c, fc := f.dial(f.options())
			defer c.Close()
			id := fc.expect(protocol.MessageTypeHello).ID
			expectStates(t, c, signal.StateConnecting, signal.StateHandshaking, signal.StateReady)
			fc.send(protocol.MessageTypeError, id, protocol.NewError(protocol.ErrorCodeForbidden, protocol.ErrorScopeRequest))
			noStates(t, c)
			noEvents(t, c)
			fc.open()
		})
	})
}

// After rate_limited in scope connection the next delay is max(30 s, retryAfterMs), and RetryNow doesn't shorten
// it (01 §10.2). A request-scope rate limit is the request's error only.
func TestRateLimitWait(t *testing.T) {
	for _, tc := range []struct {
		retryAfterMs int
		want         time.Duration
	}{
		{0, 30 * time.Second},
		{1000, 30 * time.Second},
		{45000, 45 * time.Second},
		{1 << 40, 5 * time.Minute}, // a bug on the server must not park the client for years
		{-5, 30 * time.Second},
	} {
		bubble(t, func(t *testing.T, f *fakeServer) {
			c, fc := ready(t, f, f.options())
			defer c.Close()
			e := protocol.NewError(protocol.ErrorCodeRateLimited, protocol.ErrorScopeConnection)
			e.RetryAfterMs = tc.retryAfterMs
			fc.fail(e, "")
			sc := expectStates(t, c, signal.StateBackoff)
			if sc.Err == nil || sc.Err.Code != protocol.ErrorCodeRateLimited || sc.Delay != tc.want {
				t.Fatalf("retryAfterMs %d: backoff %+v (%v), want rate_limited and %v", tc.retryAfterMs, sc, sc.Err, tc.want)
			}
			start := time.Now()
			c.RetryNow()
			time.Sleep(time.Second)
			c.RetryNow()
			expectStates(t, c, signal.StateConnecting)
			if got := time.Since(start); got != tc.want {
				t.Errorf("retryAfterMs %d: the client waited %v, want %v", tc.retryAfterMs, got, tc.want)
			}
			expectStates(t, c, signal.StateHandshaking, signal.StateReady)

			// The RetryNow calls of the rate-limit wait are spent: the next wait runs its course.
			f.conn().kill()
			back := expectBackoff(t, c)
			inBackoffBounds(t, 1, back.Delay) // the rate-limit wait counted as a delay of the sequence
		})
	}
	t.Run("scope request", func(t *testing.T) {
		bubble(t, func(t *testing.T, f *fakeServer) {
			c, fc := ready(t, f, f.options())
			defer c.Close()
			done := request(ctxT(t), c, protocol.MessageTypeRoomJoin, protocol.RoomJoin{RoomID: "lounge"}, nil)
			e := protocol.NewError(protocol.ErrorCodeRateLimited, protocol.ErrorScopeRequest)
			e.RetryAfterMs = 2500
			fc.send(protocol.MessageTypeError, fc.expect(protocol.MessageTypeRoomJoin).ID, e)
			var pe *protocol.Error
			if err := result(t, done); !errors.As(err, &pe) || pe.Code != protocol.ErrorCodeRateLimited || pe.RetryAfterMs != 2500 {
				t.Errorf("the request: %v", err)
			}
			noStates(t, c)
			fc.open()
		})
	})
}

// After server.shutdown the first delay is exactly reconnectInMs (01 §10.2, §11.6); the notice is a notification
// too. RetryNow may skip the wait; the sequence then goes on as usual.
func TestShutdownWait(t *testing.T) {
	shutdown := func(fc *fakeConn, ms int) {
		fc.send(protocol.MessageTypeServerShutdown, "", protocol.ServerShutdown{Reason: protocol.ShutdownReasonRestart, ReconnectInMs: ms})
		fc.fail(protocol.NewError(protocol.ErrorCodeServerShutdown, protocol.ErrorScopeConnection), "")
	}
	for _, tc := range []struct {
		ms    int
		exact bool // the delay is want; otherwise it is the first delay of the normal sequence
		want  time.Duration
	}{
		{1840, true, 1840 * time.Millisecond},
		{500, true, 500 * time.Millisecond},
		{3000, true, 3 * time.Second},
		{0, true, 0},                     // at once
		{-1, false, 0},                   // no server sends this: the normal sequence
		{1 << 40, true, 5 * time.Minute}, // a bug on the server must not park the client for years
	} {
		bubble(t, func(t *testing.T, f *fakeServer) {
			c, fc := ready(t, f, f.options())
			defer c.Close()
			f.status.Store(http.StatusServiceUnavailable)
			shutdown(fc, tc.ms)
			sd, err := protocol.Decode[protocol.ServerShutdown](expectEvent(t, c, protocol.MessageTypeServerShutdown))
			if err != nil || sd.ReconnectInMs != tc.ms || sd.Reason != protocol.ShutdownReasonRestart {
				t.Errorf("server.shutdown notification %+v, %v", sd, err)
			}
			sc := expectBackoff(t, c)
			if sc.Err == nil || sc.Err.Code != protocol.ErrorCodeServerShutdown {
				t.Errorf("reconnectInMs %d: backoff carries %v", tc.ms, sc.Err)
			}
			n := 0
			if tc.exact {
				if sc.Delay != tc.want {
					t.Errorf("reconnectInMs %d: delay %v, want exactly %v", tc.ms, sc.Delay, tc.want)
				}
			} else {
				inBackoffBounds(t, 0, sc.Delay)
				n = 1
			}
			// Then the normal sequence, from where it was: the shutdown wait is not one of its delays.
			inBackoffBounds(t, n, expectBackoff(t, c).Delay)
			inBackoffBounds(t, n+1, expectBackoff(t, c).Delay)
		})
	}
	t.Run("RetryNow skips it", func(t *testing.T) {
		bubble(t, func(t *testing.T, f *fakeServer) {
			c, fc := ready(t, f, f.options())
			defer c.Close()
			shutdown(fc, 3000)
			expectStates(t, c, signal.StateBackoff)
			start := time.Now()
			c.RetryNow()
			if up := expectStates(t, c, signal.StateConnecting, signal.StateHandshaking, signal.StateReady); !up.Resumed {
				t.Errorf("ready %+v", up)
			}
			if got := time.Since(start); got != 0 {
				t.Errorf("RetryNow: ready after %v, want at once", got)
			}
		})
	})
	// The hub also announces a shutdown to a socket that is still in its handshake (01 §8.2 step 6).
	t.Run("before the welcome", func(t *testing.T) {
		bubble(t, func(t *testing.T, f *fakeServer) {
			f.manual.Store(true)
			go func() {
				fc := f.conn()
				id := fc.expect(protocol.MessageTypeHello).ID
				fc.send(protocol.MessageTypeServerShutdown, "", protocol.ServerShutdown{Reason: protocol.ShutdownReasonRestart, ReconnectInMs: 2222})
				fc.fail(protocol.NewError(protocol.ErrorCodeServerShutdown, protocol.ErrorScopeConnection), id)
				fc = f.conn()
				fc.send(protocol.MessageTypeWelcome, fc.expect(protocol.MessageTypeHello).ID, f.welcome(1))
			}()
			c, err := signal.Dial(ctxT(t), f.options())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			back := expectStates(t, c, signal.StateConnecting, signal.StateHandshaking, signal.StateBackoff)
			if back.Delay != 2222*time.Millisecond || back.Err == nil || back.Err.Code != protocol.ErrorCodeServerShutdown {
				t.Errorf("backoff %+v (%v), want server_shutdown and 2.222 s", back, back.Err)
			}
			expectStates(t, c, signal.StateConnecting, signal.StateHandshaking, signal.StateReady)
			noEvents(t, c) // the notice belonged to a connection that never was
		})
	})
}

// NoReconnect: the client stops where it would back off.
func TestNoReconnect(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		o := f.options()
		o.NoReconnect = true
		f.status.Store(http.StatusServiceUnavailable)
		if c, err := signal.Dial(ctxT(t), o); c != nil || err == nil || !strings.Contains(err.Error(), "upgrade refused with 503") {
			t.Fatalf("Dial = %v, %v; want the refusal", c, err)
		}
		f.status.Store(0)

		c, fc := ready(t, f, o)
		defer c.Close()
		done := request(ctxT(t), c, protocol.MessageTypeRoomLeave, protocol.Empty{}, nil)
		fc.expect(protocol.MessageTypeRoomLeave)
		fc.kill()
		if err := result(t, done); !errors.Is(err, signal.ErrConnectionLost) {
			t.Errorf("the request that waited: %v", err)
		}
		if sc := expectStates(t, c, signal.StateStopped); sc.Err != nil {
			t.Errorf("stopped carries %v, want no error for a plain drop", sc.Err)
		}
		expectStopped(t, c)

		// A retryable error stops too, and is reported.
		c, fc = ready(t, f, o)
		defer c.Close()
		fc.fail(protocol.NewError(protocol.ErrorCodeIdleTimeout, protocol.ErrorScopeConnection), "")
		if sc := expectStates(t, c, signal.StateStopped); sc.Err == nil || sc.Err.Code != protocol.ErrorCodeIdleTimeout {
			t.Errorf("stopped carries %v, want idle_timeout", sc.Err)
		}
		expectStopped(t, c)
		if n := f.upgrades.Load(); n != 3 {
			t.Errorf("%d upgrade requests, want 3", n)
		}
	})
}

// Close in every state: it is prompt, it is final, and it closes with 1000 whenever there is a socket.
func TestClose(t *testing.T) {
	t.Run("ready, with a request waiting", func(t *testing.T) {
		bubble(t, func(t *testing.T, f *fakeServer) {
			c, fc := ready(t, f, f.options())
			done := request(ctxT(t), c, protocol.MessageTypeRoomLeave, protocol.Empty{}, nil)
			fc.expect(protocol.MessageTypeRoomLeave)
			start := time.Now()
			var wg sync.WaitGroup
			for range 4 { // concurrent calls all wait for the same close
				wg.Go(func() {
					if err := c.Close(); err != nil {
						t.Errorf("Close: %v", err)
					}
				})
			}
			wg.Wait()
			if got := time.Since(start); got != 0 {
				t.Errorf("Close took %v", got)
			}
			if code := fc.closed(); code != websocket.StatusNormalClosure {
				t.Errorf("the client closed with %d, want 1000", code)
			}
			if err := result(t, done); !errors.Is(err, signal.ErrConnectionLost) {
				t.Errorf("the request that waited: %v, want ErrConnectionLost", err)
			}
			if sc := expectStates(t, c, signal.StateStopped); sc != (signal.StateChange{State: signal.StateStopped}) {
				t.Errorf("last change %+v", sc)
			}
			expectStopped(t, c)
			c.Probe()
			c.RetryNow()
			time.Sleep(time.Minute)
			synctest.Wait()
			if n := f.upgrades.Load(); n != 1 {
				t.Errorf("%d upgrade requests, want 1", n)
			}
		})
	})
	t.Run("backing off", func(t *testing.T) {
		bubble(t, func(t *testing.T, f *fakeServer) {
			c, fc := ready(t, f, f.options())
			f.status.Store(http.StatusServiceUnavailable)
			fc.kill()
			for range 5 {
				expectBackoff(t, c)
			}
			expectStates(t, c, signal.StateBackoff) // about 10 s
			start := time.Now()
			if err := c.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
			if got := time.Since(start); got != 0 {
				t.Errorf("Close took %v", got)
			}
			expectStates(t, c, signal.StateStopped)
			expectStopped(t, c)
		})
	})
	t.Run("connecting", func(t *testing.T) {
		bubble(t, func(t *testing.T, f *fakeServer) {
			c, fc := ready(t, f, f.options())
			f.hang.Store(true)
			fc.kill()
			expectBackoff(t, c)
			synctest.Wait() // the upgrade request hangs
			start := time.Now()
			if err := c.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
			if got := time.Since(start); got != 0 {
				t.Errorf("Close took %v", got)
			}
			expectStates(t, c, signal.StateStopped)
			expectStopped(t, c)
		})
	})
	// The network has gone black when Close is called: the close frame reaches nobody and no answer comes. Close
	// says so after coder/websocket's 5 s; the server will keep the connection for its grace period.
	t.Run("without an answer", func(t *testing.T) {
		bubble(t, func(t *testing.T, f *fakeServer) {
			path := &cutNet{net: f.srv.Net}
			o := f.options()
			o.HTTPClient = path.client()
			c, fc := ready(t, f, o)
			path.cut.Store(true)
			start := time.Now()
			err := c.Close()
			if err == nil || !strings.Contains(err.Error(), "signal: close") {
				t.Errorf("Close = %v, want the failed close handshake", err)
			}
			if got := time.Since(start); got != 5*time.Second {
				t.Errorf("Close took %v, want 5 s", got)
			}
			if err2 := c.Close(); !errors.Is(err2, err) {
				t.Errorf("second Close = %v, want the first one's error", err2)
			}
			if code := fc.closed(); code != -1 {
				t.Errorf("the server saw close code %d, want none", code)
			}
			expectStates(t, c, signal.StateStopped)
			expectStopped(t, c)
		})
	})
}

// The client under concurrent use (run with -race): requests, notifications, probes and retries from many
// goroutines while the server answers and drops sockets.
func TestConcurrentUse(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		c, fc := f.dial(f.options())
		// The server answers every room.leave on every socket.
		serve := func(fc *fakeConn) {
			for {
				select {
				case env := <-fc.msgs:
					if env.Type == protocol.MessageTypeRoomLeave {
						_ = fc.write(protocol.MessageTypeOK, env.ID, nil)
						_ = fc.write(protocol.MessageTypeRoomState, "", protocol.RoomState{RoomID: "lounge"})
					}
				case <-fc.done:
					return
				}
			}
		}
		var servers sync.WaitGroup
		servers.Go(func() { serve(fc) })
		stop := make(chan struct{})
		servers.Go(func() {
			for {
				select {
				case next := <-f.conns:
					servers.Go(func() { serve(next) })
				case <-stop:
					return
				}
			}
		})

		var users sync.WaitGroup
		var ok, lost int
		var mu sync.Mutex
		for range 8 {
			users.Go(func() {
				for range 50 {
					err := c.Request(context.Background(), protocol.MessageTypeRoomLeave, protocol.Empty{}, nil)
					mu.Lock()
					switch {
					case err == nil:
						ok++
					case errors.Is(err, signal.ErrConnectionLost):
						lost++
					default:
						t.Errorf("request: %v", err)
					}
					mu.Unlock()
					_ = c.Send(protocol.MessageTypeStats, protocol.ClientStats{})
					c.Probe()
					c.RetryNow()
					_ = c.Welcome()
					time.Sleep(100 * time.Millisecond)
				}
			})
		}
		users.Go(func() { // a caller that receives
			for range c.Events() {
			}
		})
		users.Go(func() {
			for range c.States() {
			}
		})
		for range 6 {
			time.Sleep(700 * time.Millisecond)
			f.mu.Lock()
			last := f.all[len(f.all)-1]
			f.mu.Unlock()
			last.kill()
		}
		time.Sleep(10 * time.Second)
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		users.Wait()
		close(stop)
		f.mu.Lock()
		all := f.all
		f.mu.Unlock()
		for _, fc := range all {
			fc.kill()
		}
		servers.Wait()
		if ok == 0 || lost == 0 || ok+lost != 400 {
			t.Errorf("%d requests answered, %d lost; want some of each, 400 together", ok, lost)
		}
	})
}

// Nothing the client logs holds a credential, an SDP or a payload (README "Logging"), at any level.
func TestLogsHoldNoSecrets(t *testing.T) {
	bubble(t, func(t *testing.T, f *fakeServer) {
		var logs syncBuffer
		o := f.options()
		o.Logger = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		o.Header = http.Header{"Cookie": {"isshoni_session=cookie-canary"}}
		o.Token = func(context.Context) (protocol.Secret, error) { return "bearer-canary", nil }
		c, fc := ready(t, f, o)
		defer c.Close()

		done := request(ctxT(t), c, protocol.MessageTypeShareStart, protocol.ShareStart{Kind: protocol.ShareKindScreen,
			Preset: protocol.PresetAuto, Label: "label-canary", Ref: "r1"}, nil)
		fc.send(protocol.MessageTypeOK, fc.expect(protocol.MessageTypeShareStart).ID, nil)
		if err := result(t, done); err != nil {
			t.Fatal(err)
		}
		if err := c.Send(protocol.MessageTypePCOffer, protocol.PCOffer{PC: protocol.PCKindPub, Gen: 1, Neg: 1, SDP: "v=0 sdp-canary"}); err != nil {
			t.Fatal(err)
		}
		fc.send(protocol.MessageTypePCAnswer, "", protocol.PCAnswer{PC: protocol.PCKindPub, Gen: 1, Neg: 1, SDP: "v=0 sdp-canary"})
		expectEvent(t, c, protocol.MessageTypePCAnswer)
		fc.sendRaw(`{"type":"junk-canary`)
		fc.sendRaw(`{"type":"unknown.canary","data":{"x":"payload-canary"}}`)
		e := protocol.NewError(protocol.ErrorCodeInternal, protocol.ErrorScopeConnection)
		e.Params = map[string]any{"ref": "params-canary"}
		fc.fail(e, "")
		_, _, fc = reconnected(t, f, c)
		fc.closeWith(protocol.CloseCodeUnauthenticated)
		expectStates(t, c, signal.StateStopped)
		expectStopped(t, c)

		out := logs.String()
		for _, canary := range []string{"cookie-canary", "bearer-canary", "label-canary", "sdp-canary", "junk-canary",
			"payload-canary", "params-canary", "r1.token"} {
			if strings.Contains(out, canary) {
				t.Errorf("the log holds %q:\n%s", canary, out)
			}
		}
		for _, want := range []string{`"state":"ready"`, `"conn_id":"c_aaaaaaaaaaaaaaaa"`, `"code":"internal"`,
			`"state":"stopped"`, `"code":"unauthenticated"`, "dropped a malformed server message"} {
			if !strings.Contains(out, want) {
				t.Errorf("the log lacks %s:\n%s", want, out)
			}
		}
	})
}

// syncBuffer is a log sink that a test reads while the client writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
