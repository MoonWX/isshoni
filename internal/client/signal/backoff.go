package signal

import (
	"math/rand/v2"
	"time"
)

// Timings of the client (01 §3.4, §10.1–§10.2, §13).
const (
	// RequestTimeout: a request without a reply for this long fails with ErrRequestTimeout.
	RequestTimeout = 10 * time.Second
	// ConnectTimeout bounds one attempt's bearer token call and WebSocket upgrade.
	ConnectTimeout = 10 * time.Second
	// HandshakeTimeout: no welcome within this after the socket opened fails the attempt.
	HandshakeTimeout = 10 * time.Second
	// DefaultPingInterval is the ping interval when welcome.limits.pingIntervalMs is missing.
	DefaultPingInterval = 15 * time.Second
	// PongTimeout: a periodic ping without a pong within this drops the socket; the client reconnects.
	PongTimeout = 10 * time.Second
	// ProbeTimeout is the pong timeout of Probe's immediate ping.
	ProbeTimeout = 3 * time.Second
	// BackoffMin and BackoffMax bound every backoff delay (01 §10.2).
	BackoffMin = 500 * time.Millisecond
	BackoffMax = 10 * time.Second
	// BackoffReset: the backoff sequence starts over only after the connection has been ready this long, so a
	// crash-looping server keeps the delays high.
	BackoffReset = 10 * time.Second
	// RateLimitMinWait is the shortest wait after a connection-scope rate limit (rate_limited, or close 4429).
	RateLimitMinWait = 30 * time.Second
)

const (
	// writeTimeout bounds the write of one message, like the server's (01 §3.3).
	writeTimeout = 10 * time.Second
	// minPingInterval is the shortest ping interval the client takes from a welcome: the server allows 20 messages
	// per second, and a welcome that asks for more pings than that is a bug.
	minPingInterval = time.Second
	// maxServerWait caps the waits the server dictates (server.shutdown.reconnectInMs, error.retryAfterMs): a value
	// beyond it is a bug, and the client must not wait for hours on one.
	maxServerWait = 5 * time.Minute
	// backoffDoublings is the number of doublings after which 0.5 s × 2ⁿ is past BackoffMax (0.5 s × 2⁵ = 16 s).
	backoffDoublings = 5
)

// backoffDelay returns the delay before reconnect attempt n (n = 0 for the first retry after a drop, 01 §10.2):
// min(10 s, 0.5 s × 2ⁿ) × U(0.8, 1.2), clamped to [0.5 s, 10 s]. That is about 0.5, 1, 2, 4, 8, 10, 10, … s.
// u is the uniform sample in [0, 1).
func backoffDelay(n int, u float64) time.Duration {
	base := BackoffMax
	if n < backoffDoublings {
		base = min(BackoffMax, BackoffMin<<max(n, 0))
	}
	d := time.Duration(float64(base) * (0.8 + 0.4*u))
	return min(BackoffMax, max(BackoffMin, d))
}

// jitter returns the uniform sample of one backoff delay.
func jitter() float64 {
	return rand.Float64() //nolint:gosec // G404: reconnect jitter, not a secret
}

// serverWait converts a wait that the server sent in milliseconds, capped at maxServerWait. ok is false for a
// negative value, which no server sends.
func serverWait(ms int) (d time.Duration, ok bool) {
	if ms < 0 {
		return 0, false
	}
	if ms > int(maxServerWait/time.Millisecond) {
		return maxServerWait, true
	}
	return time.Duration(ms) * time.Millisecond, true
}

// pingInterval returns the heartbeat interval for a welcome's limits.pingIntervalMs.
func pingInterval(ms int) time.Duration {
	if ms <= 0 {
		return DefaultPingInterval
	}
	d, _ := serverWait(ms)
	return max(d, minPingInterval)
}
