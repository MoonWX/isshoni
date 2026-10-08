package signal

import (
	"context"
	"testing"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// FuzzHandle feeds the reader's message handling with arbitrary server frames, before and after the welcome: whatever
// a server sends, the client must not panic or wait, and it is ready only on a welcome it can use.
func FuzzHandle(f *testing.F) {
	for _, seed := range []string{
		`{"type":"welcome","re":"1","data":{"protocol":1,"connectionId":"c_aaaaaaaaaaaaaaaa","resumeToken":"r1.x","resumed":true,"limits":{"pingIntervalMs":15000}}}`,
		`{"type":"welcome","re":"1","data":{"connectionId":"c_aaaaaaaaaaaaaaaa"}}`,
		`{"type":"welcome","re":"1"}`,
		`{"type":"welcome","re":"7","data":{"connectionId":"c_aaaaaaaaaaaaaaaa","resumeToken":"r1.x"}}`,
		`{"type":"ok","re":"2","data":{"room":{"id":"lounge","name":"Lounge"}}}`,
		`{"type":"ok","re":"2"}`,
		`{"type":"ok"}`,
		`{"type":"error","re":"1","data":{"code":"unauthenticated","scope":"session","retryable":false}}`,
		`{"type":"error","re":"2","data":{"code":"share_limit","scope":"request","retryable":false,"params":{"limit":4}}}`,
		`{"type":"error","data":{"code":"rate_limited","scope":"connection","retryable":true,"retryAfterMs":99999999999}}`,
		`{"type":"error","data":{"code":"sdp_invalid","scope":"pc","pc":"sub","gen":2,"neg":3}}`,
		`{"type":"error","data":{"code":7,"scope":[],"retryable":"yes"}}`,
		`{"type":"error"}`,
		`{"type":"pong","data":{"t":1,"serverTimeMs":2}}`,
		`{"type":"server.shutdown","data":{"reason":"restart","reconnectInMs":1840}}`,
		`{"type":"server.shutdown","data":{"reason":7,"reconnectInMs":-1}}`,
		`{"type":"room.state","data":{"roomId":"lounge","rev":1,"participants":[],"shares":[]}}`,
		`{"type":"pc.offer","data":{"pc":"sub","gen":1,"neg":1,"sdp":"v=0","tracks":[]}}`,
		`{"type":"brand.new","id":"9","data":{"x":[1,2,{"y":null}]}}`,
		`{"type":"hello","id":"1","data":{}}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, frame []byte) {
		env, err := protocol.ParseEnvelope(frame)
		if err != nil {
			return // the reader drops it
		}
		c := newClient(context.Background(), Options{})
		defer c.cancel()
		s := newSocket(c, nil, "1")
		c.sock, c.state = s, StateHandshaking
		waiting := make(chan reply, 1)
		c.pending["2"] = waiting

		for range 2 { // the second time after the welcome, if it was one
			goOn := s.handle(env)
			if !goOn && s.srvErr == nil && s.readErr == nil {
				t.Fatalf("the reader ends without a reason after %s", frame)
			}
		}
		switch c.state {
		case StateReady:
			if c.welcome.ConnectionID == "" || c.welcome.ResumeToken == "" || !s.welcomed {
				t.Fatalf("ready on the welcome %s", frame)
			}
			select {
			case <-s.ready:
			default:
				t.Fatal("ready, but the socket does not say so")
			}
			select {
			case <-c.first:
			default:
				t.Fatal("ready, but Dial is not told")
			}
		case StateHandshaking:
		default:
			t.Fatalf("state %s after %s", c.state, frame)
		}
		if n := len(c.states); (c.state == StateReady) != (n == 1) {
			t.Fatalf("%d state changes reported in state %s", n, c.state)
		}
		if len(c.events) > 2 {
			t.Fatalf("%d notifications from one message handled twice", len(c.events))
		}
		if len(waiting) == 1 {
			if r := <-waiting; (r.err == nil) == (env.Type == protocol.MessageTypeError) {
				t.Fatalf("the request got %+v for %s", r, frame)
			}
			if _, still := c.pending["2"]; still {
				t.Fatal("an answered request is still waiting")
			}
		}
	})
}
