package auth

import (
	"fmt"
	"math"
	"net/netip"
	"sync"
	"time"
)

// IPKey is the limiter's key for a client IP (03 §7.3): an IPv4 address (IPv4-mapped IPv6 is unmapped first) as its
// /32, an IPv6 address as its /64, without a zone. One IPv6 host owns a whole /64, so keying by address would give it
// 2^64 buckets. The zero Addr gives the zero Prefix. 04's wiring test pins netx.IPKey to it (04 §17).
func IPKey(a netip.Addr) netip.Prefix {
	a = a.Unmap()
	bits := 64
	if a.Is4() {
		bits = 32
	}
	p, err := a.Prefix(bits)
	if err != nil {
		return netip.Prefix{}
	}
	return p
}

// HashBudget is the server-wide auth-hash bucket (03 §7.3): at most Burst anonymous hashes at once, refilled at
// PerSecond. The zero value means DefaultHashBudget; only tests change it. Both must be at least 1, PerSecond at most
// 1e9 (one token per nanosecond), and the time to refill an empty bucket, Burst ÷ PerSecond seconds, at most about
// 146 years (maxWindow). A test that wants no budget sets something like {Burst: 1 << 20, PerSecond: 1 << 20}.
type HashBudget struct{ Burst, PerSecond int }

// DefaultHashBudget allows a burst of 20 anonymous hashes and 5 per second after that: about a quarter of one core.
var DefaultHashBudget = HashBudget{Burst: 20, PerSecond: 5}

func (b HashBudget) rate() (rate, error) {
	if b == (HashBudget{}) {
		b = DefaultHashBudget
	}
	if b.Burst < 1 || b.PerSecond < 1 {
		return rate{}, fmt.Errorf("auth: hash budget %+v: burst and rate must be at least 1", b)
	}
	if int64(b.PerSecond) > int64(time.Second) {
		return rate{}, fmt.Errorf("auth: hash budget %+v: the rate may be at most %d per second", b, time.Second)
	}
	r := rate{Burst: b.Burst, Every: time.Second / time.Duration(b.PerSecond)}
	if err := r.validate(); err != nil {
		return rate{}, fmt.Errorf("auth: hash budget %+v: %w", b, err)
	}
	return r, nil
}

// rate is a token bucket's shape: at most Burst tokens, one more every Every.
type rate struct {
	Burst int
	Every time.Duration
}

// maxWindow bounds a bucket's window, Burst × Every (about 146 years). Every ≤ window, so the limiter's sums of a
// window and Every never overflow a time.Duration.
const maxWindow = time.Duration(math.MaxInt64 / 2)

// validate reports a rate the limiter can't use: Burst or Every below 1, or a window beyond maxWindow.
func (r rate) validate() error {
	if r.Burst < 1 || r.Every <= 0 {
		return fmt.Errorf("auth: limiter rate %+v: burst and interval must be positive", r)
	}
	if int64(r.Burst) > int64(maxWindow/r.Every) { // Burst × Every > maxWindow, without the overflow
		return fmt.Errorf("auth: limiter rate %+v: burst × interval exceeds %v", r, maxWindow)
	}
	return nil
}

// verdict is a bucket's answer.
type verdict struct {
	OK bool
	// RetryAfter is the time until the next token, when !OK.
	RetryAfter time.Duration
	// First is set on the first refusal since the key last passed or its bucket was refilled: one throttling
	// episode writes one auth.throttled audit row (03 §7.3).
	First bool
}

// retryAfterSeconds is the Retry-After value of a refusal: whole seconds, rounded up, at least 1.
func (v verdict) retryAfterSeconds() int {
	s := int((v.RetryAfter + time.Second - 1) / time.Second)
	return max(s, 1)
}

// maxLimiterKeys caps each bucket map (03 §7.3, §14).
const maxLimiterKeys = 100_000

// limiter is a map of token buckets with one rate, kept in memory only (03 decision 8). A key without an entry has
// a full bucket; entries go away when they are refilled on purpose (reset), when a refund fills them, or when they
// are evicted.
//
// Each bucket is a GCRA: tat, its "theoretical arrival time", is when the bucket will be full again, and each token
// taken moves it Every into the future. A request passes when that stays within Burst × Every of now. Integer time
// keeps the arithmetic exact.
//
// When the map is full, full buckets (idle keys) are dropped first, then the least recently used ones. Finding the
// full ones takes a scan, done at most once per Every; in between, eviction takes the least recently used key.
type limiter[K comparable] struct {
	rate    rate
	maxKeys int
	now     func() time.Time

	mu        sync.Mutex
	m         map[K]*bucket[K]
	head      *bucket[K] // most recently used
	tail      *bucket[K] // least recently used
	lastSweep time.Time
}

type bucket[K comparable] struct {
	key        K
	tat        time.Time
	refused    bool // a refusal was reported since the key last passed
	prev, next *bucket[K]
}

// newLimiter panics on an invalid rate: the rates are constants, and HashBudget.rate validates the only one that
// comes from outside.
func newLimiter[K comparable](r rate, maxKeys int, now func() time.Time) *limiter[K] {
	if err := r.validate(); err != nil {
		panic(err.Error()) // only reached through a programming error
	}
	return &limiter[K]{rate: r, maxKeys: max(maxKeys, 1), now: now, m: make(map[K]*bucket[K])}
}

// window is how far tat may run ahead of now: Burst tokens. It is at least Every and at most maxWindow (validate).
func (l *limiter[K]) window() time.Duration { return time.Duration(l.rate.Burst) * l.rate.Every }

// take consumes one token for key if there is one. Buckets that count every attempt (auth-ip, register-ip,
// auth-hash) only take. The buckets of failed password checks (auth-user-ip, auth-user) take the token before the
// password is hashed and give it back with refund when the attempt was not a failure, so attempts that arrive while
// earlier ones are still hashing see those in the bucket.
func (l *limiter[K]) take(key K) verdict {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.m[key]
	start := now
	if b != nil {
		l.touch(b)
		if b.tat.After(now) {
			start = b.tat
		}
	}
	if wait := start.Add(l.rate.Every).Sub(now) - l.window(); wait > 0 {
		return l.refuse(b, wait) // b is non-nil: an absent key is full (window ≥ Every) and always passes
	}
	if b == nil {
		b = l.insert(key, now)
	}
	b.tat = start.Add(l.rate.Every)
	b.refused = false
	return verdict{OK: true}
}

// refund gives back the one token a take for key consumed: the bucket is as it was before that take. A bucket that
// is full again loses its entry. When the entry is gone (reset refilled the bucket, or it was evicted) the bucket is
// full already and there is nothing to give back.
func (l *limiter[K]) refund(key K) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.m[key]
	if b == nil {
		return
	}
	b.tat = b.tat.Add(-l.rate.Every)
	if !b.tat.After(l.now()) {
		l.remove(b)
	}
}

// reset refills key's bucket (a successful login refills that address's auth-user-ip bucket, 03 §7.3).
func (l *limiter[K]) reset(key K) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b := l.m[key]; b != nil {
		l.remove(b)
	}
}

// size returns the number of keys held.
func (l *limiter[K]) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.m)
}

// refuse reports a refusal. Refused keys count as used, so a key under attack stays recent and keeps its state
// (least-recently-used eviction would otherwise hand a persistent attacker a fresh bucket).
func (l *limiter[K]) refuse(b *bucket[K], wait time.Duration) verdict {
	first := !b.refused
	b.refused = true
	return verdict{RetryAfter: wait, First: first}
}

// insert adds a full bucket for key as the most recently used, evicting first when the map is full.
func (l *limiter[K]) insert(key K, now time.Time) *bucket[K] {
	if len(l.m) >= l.maxKeys {
		l.evict(now)
	}
	b := &bucket[K]{key: key, tat: now}
	l.m[key] = b
	l.pushFront(b)
	return b
}

// evict makes room for one key: every full bucket goes (at most one scan per Every), then least recently used ones.
func (l *limiter[K]) evict(now time.Time) {
	if l.lastSweep.IsZero() || now.Sub(l.lastSweep) >= l.rate.Every {
		l.lastSweep = now
		for b := l.tail; b != nil; {
			prev := b.prev
			if !b.tat.After(now) {
				l.remove(b)
			}
			b = prev
		}
	}
	for len(l.m) >= l.maxKeys && l.tail != nil {
		l.remove(l.tail)
	}
}

func (l *limiter[K]) remove(b *bucket[K]) {
	l.unlink(b)
	delete(l.m, b.key)
}

func (l *limiter[K]) touch(b *bucket[K]) {
	if l.head == b {
		return
	}
	l.unlink(b)
	l.pushFront(b)
}

func (l *limiter[K]) pushFront(b *bucket[K]) {
	b.prev, b.next = nil, l.head
	if l.head != nil {
		l.head.prev = b
	}
	l.head = b
	if l.tail == nil {
		l.tail = b
	}
}

func (l *limiter[K]) unlink(b *bucket[K]) {
	if b.prev != nil {
		b.prev.next = b.next
	} else {
		l.head = b.next
	}
	if b.next != nil {
		b.next.prev = b.prev
	} else {
		l.tail = b.prev
	}
	b.prev, b.next = nil, nil
}

// userIPKey keys the auth-user-ip bucket: a username key (existing or not) at one client IPKey.
type userIPKey struct {
	user string
	ip   netip.Prefix
}

// Throttle rates of 03 §7.3. The table's push-test bucket (one POST /api/v1/push/test per user per 10 s) is not
// among them: httpapi keeps it next to its only user, the handler (httpapi/push.go, pushTestLimiter).
var (
	rateAuthIP     = rate{Burst: 20, Every: 15 * time.Second}
	rateAuthUserIP = rate{Burst: 5, Every: 2 * time.Minute}
	rateAuthUser   = rate{Burst: 30, Every: 2 * time.Minute}
	rateRegisterIP = rate{Burst: 5, Every: 12 * time.Minute}
	// A blocked attempt logs one warn line per IPKey per minute.
	rateBlockLog = rate{Burst: 1, Every: time.Minute}
	// auth.login_failed audit rows: 600 per hour server-wide; beyond that one auth.throttled {scope:"global"} row
	// per hour.
	rateLoginFailedAudit = rate{Burst: 600, Every: 6 * time.Second}
	rateGlobalThrottled  = rate{Burst: 1, Every: time.Hour}
	// The signup_pending admin alert: at most one per 10 min, however many sign-ups arrive (03 §7.9).
	rateSignupAlert = rate{Burst: 1, Every: 10 * time.Minute}
)

// throttles are the in-memory buckets of 03 §7.3. Each is checked before any hashing or DB access; the service
// decides the order (auth-ip first, auth-hash last).
type throttles struct {
	authIP *limiter[netip.Prefix] // every public auth attempt, by IPKey
	// The two buckets of failed password checks: a login's, and the password a logged-in user types again
	// (verifyOwnPassword). An attempt takes its tokens before the hash and gets them back when it was no failure
	// (passwordAttempt in login.go).
	authUserIP *limiter[userIPKey]    // by username key and IPKey; a hard block
	authUser   *limiter[string]       // from any IP, by username key; known IPs pass
	registerIP *limiter[netip.Prefix] // sign-ups without an invite, by IPKey
	authHash   *limiter[struct{}]     // every public request that reaches the hash, server-wide

	blockLog             *limiter[netip.Prefix] // the warn line of a blocked attempt, by IPKey
	loginFailedAudit     *limiter[struct{}]     // auth.login_failed rows
	globalThrottledAudit *limiter[struct{}]     // the auth.throttled {scope:"global"} row past that cap
	signupAlert          *limiter[struct{}]     // the signup_pending admin alert
}

func newThrottles(now func() time.Time, hb HashBudget) (*throttles, error) {
	hashRate, err := hb.rate()
	if err != nil {
		return nil, err
	}
	return &throttles{
		authIP:               newLimiter[netip.Prefix](rateAuthIP, maxLimiterKeys, now),
		authUserIP:           newLimiter[userIPKey](rateAuthUserIP, maxLimiterKeys, now),
		authUser:             newLimiter[string](rateAuthUser, maxLimiterKeys, now),
		registerIP:           newLimiter[netip.Prefix](rateRegisterIP, maxLimiterKeys, now),
		authHash:             newLimiter[struct{}](hashRate, 1, now),
		blockLog:             newLimiter[netip.Prefix](rateBlockLog, maxLimiterKeys, now),
		loginFailedAudit:     newLimiter[struct{}](rateLoginFailedAudit, 1, now),
		globalThrottledAudit: newLimiter[struct{}](rateGlobalThrottled, 1, now),
		signupAlert:          newLimiter[struct{}](rateSignupAlert, 1, now),
	}, nil
}
