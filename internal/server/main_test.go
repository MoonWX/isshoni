package server_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the run when a goroutine outlives the tests: every server here must stop completely.
//
// certmagic, which the TLS manager runs in auto and ip mode, keeps a little process-wide state of its own that a
// stopped server can't take down with it (see the TestMain of internal/server/tlsmgr):
//   - the goroutine that refreshes a storage lock file sleeps for up to five seconds after the lock is released
//     before it notices and ends;
//   - with a real ACME server (the Pebble test of acme_test.go only): one rate limiter per CA and account, and the
//     idle keep-alive connections of its HTTP client.
func TestMain(m *testing.M) {
	opts := []goleak.Option{
		goleak.IgnoreAnyFunction("github.com/caddyserver/certmagic.keepLockfileFresh"),
	}
	if _, _, ok := acmeTestCA(); ok {
		opts = append(opts,
			goleak.IgnoreAnyFunction("github.com/caddyserver/certmagic.(*RingBufferRateLimiter).loop"),
			goleak.IgnoreAnyFunction("net/http.(*persistConn).readLoop"),
			goleak.IgnoreAnyFunction("net/http.(*persistConn).writeLoop"),
			goleak.IgnoreAnyFunction("net/http/internal/http2.(*ClientConn).readLoop"),
		)
	}
	goleak.VerifyTestMain(m, opts...)
}
