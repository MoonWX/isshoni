package signal

import (
	"math"
	"net/netip"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
)

// rate is a token bucket's shape: at most burst tokens, refilled at burst tokens per per.
type rate struct {
	burst float64
	per   time.Duration
}

func perMinute(n int) rate { return rate{burst: float64(n), per: time.Minute} }

func perSecond(n int) rate { return rate{burst: float64(n), per: time.Second} }

// epsilon absorbs floating-point error in refills, so that a bucket refilled for exactly one token's time holds one.
const epsilon = 1e-9

// tokenBucket is a token bucket that starts full. It is not safe for concurrent use.
type tokenBucket struct {
	r      rate
	tokens float64
	last   time.Time
}

func newBucket(r rate, now time.Time) tokenBucket {
	return tokenBucket{r: r, tokens: r.burst, last: now}
}

func (b *tokenBucket) refill(now time.Time) {
	if el := now.Sub(b.last); el > 0 {
		b.tokens = min(b.r.burst, b.tokens+float64(el)*b.r.burst/float64(b.r.per))
		b.last = now
	}
}

// has reports whether the bucket, refilled up to now, holds n tokens.
func (b *tokenBucket) has(now time.Time, n float64) bool {
	b.refill(now)
	return b.tokens+epsilon >= n
}

// waitFor returns how long until the bucket holds n tokens (after a refill up to now), rounded up to the
// nanosecond. Sub-nanosecond floating-point error is ignored, so that one token of a 20-per-minute bucket is 3 s,
// not 3 s and 1 ns (which a Retry-After would round up to 4).
func (b *tokenBucket) waitFor(n float64) time.Duration {
	missing := n - b.tokens
	if missing <= epsilon {
		return 0
	}
	return time.Duration(math.Ceil(missing*float64(b.r.per)/b.r.burst - 1e-3))
}

// take removes n tokens (at most the burst) if the bucket holds them. Otherwise it removes nothing and returns how
// long until it will hold them.
func (b *tokenBucket) take(now time.Time, n float64) (bool, time.Duration) {
	n = min(n, b.r.burst)
	if !b.has(now, n) {
		return false, b.waitFor(n)
	}
	b.tokens = max(0, b.tokens-n)
	return true, 0
}

// full reports whether the bucket has refilled completely by now.
func (b *tokenBucket) full(now time.Time) bool { return b.has(now, b.r.burst) }

// ipKey is the rate-limit key of a client IP (01 §3.1 step 4, as 03 §7.3): an IPv4 address (an IPv4-mapped IPv6
// address is unmapped first) as its /32, an IPv6 address as its /64, without a zone. One IPv6 host usually owns a
// whole /64, so keying by address would give it 2^64 buckets. The zero Addr gives the zero Prefix.
func ipKey(a netip.Addr) netip.Prefix {
	a = a.Unmap()
	bits := 64
	if a.Is4() {
		bits = 32
	}
	p, err := a.WithZone("").Prefix(bits)
	if err != nil {
		return netip.Prefix{}
	}
	return p
}

// maxLimiterKeys caps each keyed limiter's map, as 03 §7.3 does for its throttles.
const maxLimiterKeys = 100_000

// keyedLimiter is a map of token buckets with one rate, in memory only. A key without an entry has a full bucket.
// When the map is full, full buckets are dropped (at most one scan per second), and otherwise an arbitrary entry.
type keyedLimiter struct {
	r       rate
	maxKeys int

	mu        sync.Mutex
	m         map[netip.Prefix]*tokenBucket
	lastSweep time.Time
}

func newKeyedLimiter(r rate, maxKeys int) *keyedLimiter {
	return &keyedLimiter{r: r, maxKeys: maxKeys, m: make(map[netip.Prefix]*tokenBucket)}
}

// allow takes one token from key's bucket. When the bucket is empty it returns false and the time until the next
// token.
func (l *keyedLimiter) allow(key netip.Prefix, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.m[key]
	if !ok {
		if len(l.m) >= l.maxKeys {
			l.evict(now)
		}
		nb := newBucket(l.r, now)
		b = &nb
		l.m[key] = b
	}
	return b.take(now, 1)
}

func (l *keyedLimiter) evict(now time.Time) {
	if now.Sub(l.lastSweep) >= time.Second {
		l.lastSweep = now
		for k, b := range l.m {
			if b.full(now) {
				delete(l.m, k)
			}
		}
	}
	for k := range l.m {
		if len(l.m) < l.maxKeys {
			break
		}
		delete(l.m, k)
	}
}

// size returns the number of keys, for tests.
func (l *keyedLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.m)
}

// floodWindow: a connection whose global bucket stays exhausted this long, and that still sends, is closed with
// rate_limited (scope connection) and 4429 (01 §12.1).
const floodWindow = 10 * time.Second

// typeRates are the per-type rate limits of 01 §13, per connection. Excess stats are dropped silently; the others
// get rate_limited. pc.restart has 12 per minute per PC kind (pcRestartRate). The limit on pub offers with a new
// gen (6 per minute) belongs to the pc.offer routing (README S40), which tracks the gen.
var typeRates = map[protocol.MessageType]rate{
	protocol.MessageTypeRoomJoin:   perMinute(10),
	protocol.MessageTypeShareStart: perMinute(10),
	protocol.MessageTypeCapsUpdate: perMinute(12),
	protocol.MessageTypeStats:      {burst: 1, per: 5 * time.Second},
	protocol.MessageTypeAgentSend:  perSecond(10),
}

// pcRestartRate limits pc.restart per PC kind (01 §13).
var pcRestartRate = perMinute(12)

// connLimits are one connection's rate limits (01 §13): the global message and byte buckets, and the per-type
// buckets. Owned by the connection's actor.
type connLimits struct {
	msgs, bytes tokenBucket
	// floodSince is when the global bucket first refused a message in the current flood; zero outside a flood. A
	// flood ends when a message passes with both buckets at least half full.
	floodSince time.Time
	perType    map[string]*tokenBucket
}

func newConnLimits(l Limits, now time.Time) connLimits {
	return connLimits{
		msgs:    newBucket(rate{burst: float64(l.MessageBurst), per: perRefill(l.MessageBurst, l.MessagesPerSecond)}, now),
		bytes:   newBucket(rate{burst: float64(l.BytesBurst), per: perRefill(l.BytesBurst, l.BytesPerSecond)}, now),
		perType: make(map[string]*tokenBucket),
	}
}

// perRefill is the time a bucket of burst tokens refilled at perSecond tokens per second takes to fill from empty.
func perRefill(burst, perSecond int) time.Duration {
	return time.Duration(float64(time.Second) * float64(burst) / float64(perSecond))
}

// global charges one message of size bytes to the global buckets. When refused it returns the time until the
// message would pass, and flood is true when the buckets have been refusing for floodWindow.
func (l *connLimits) global(now time.Time, size int) (ok bool, wait time.Duration, flood bool) {
	n := min(float64(size), l.bytes.r.burst)
	haveMsg, haveBytes := l.msgs.has(now, 1), l.bytes.has(now, n) // both refilled, for the waits below
	if haveMsg && haveBytes {
		l.msgs.tokens = max(0, l.msgs.tokens-1)
		l.bytes.tokens = max(0, l.bytes.tokens-n)
		if !l.floodSince.IsZero() && l.msgs.tokens >= l.msgs.r.burst/2 && l.bytes.tokens >= l.bytes.r.burst/2 {
			l.floodSince = time.Time{}
		}
		return true, 0, false
	}
	wait = max(l.msgs.waitFor(1), l.bytes.waitFor(n))
	if l.floodSince.IsZero() {
		l.floodSince = now
	}
	return false, wait, now.Sub(l.floodSince) >= floodWindow
}

// refillTime returns how long the global buckets need to refill completely.
func (l *connLimits) refillTime() time.Duration {
	return max(l.msgs.waitFor(l.msgs.r.burst), l.bytes.waitFor(l.bytes.r.burst))
}

// typed charges one message to the per-type bucket key, created with rate r on first use.
func (l *connLimits) typed(now time.Time, key string, r rate) (bool, time.Duration) {
	b, ok := l.perType[key]
	if !ok {
		nb := newBucket(r, now)
		b = &nb
		l.perType[key] = b
	}
	return b.take(now, 1)
}

// retryAfterMs converts a wait into the wire's retryAfterMs: whole milliseconds, rounded up, at least 1.
func retryAfterMs(d time.Duration) int {
	return max(1, int((d+time.Millisecond-1)/time.Millisecond))
}

// retryAfterSeconds converts a wait into an HTTP Retry-After value: whole seconds, rounded up, at least 1.
func retryAfterSeconds(d time.Duration) int {
	return max(1, int((d+time.Second-1)/time.Second))
}
