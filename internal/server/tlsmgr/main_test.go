package tlsmgr

import (
	"os"
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the run when a goroutine outlives the tests: a manager that was shut down leaves nothing behind.
//
// certmagic keeps a little process-wide state of its own, which the check has to know:
//   - the goroutine that refreshes a storage lock file sleeps for up to five seconds after the lock is released
//     before it notices and ends;
//   - with a real ACME server (the Pebble tests of acme_test.go only): one rate limiter per CA and account, which
//     certmagic never stops, and the idle keep-alive connections of its HTTP client, which it never closes.
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

// acmeTestCA returns the ACME test server that the environment names, for the tests that need one (acme_test.go).
func acmeTestCA() (directory, rootFile string, ok bool) {
	directory, rootFile = os.Getenv(envACMECA), os.Getenv(envACMECARoot)
	return directory, rootFile, directory != "" && rootFile != ""
}
