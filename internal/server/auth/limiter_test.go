package auth

import (
	"crypto/rand"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manual clock for the limiter tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestIPKey(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"192.0.2.1", "192.0.2.1/32"},
		{"192.0.2.2", "192.0.2.2/32"},                             // a neighbouring IPv4 address is its own key
		{"::ffff:192.0.2.1", "192.0.2.1/32"},                      // IPv4-mapped IPv6 is unmapped
		{"2001:db8:1:2:3:4:5:6", "2001:db8:1:2::/64"},             // two addresses in one /64 …
		{"2001:db8:1:2:ffff:ffff:ffff:ffff", "2001:db8:1:2::/64"}, // … share a key
		{"2001:db8:1:3::1", "2001:db8:1:3::/64"},                  // the neighbouring /64 does not
		{"2001:db8:1:1:ffff:ffff:ffff:ffff", "2001:db8:1:1::/64"},
		{"fe80::1%eth0", "fe80::/64"}, // no zone
		{"::1", "::/64"},
		{"127.0.0.1", "127.0.0.1/32"},
	} {
		got := IPKey(netip.MustParseAddr(tc.in))
		if got.String() != tc.want {
			t.Errorf("IPKey(%s) = %s, want %s", tc.in, got, tc.want)
		}
		if !got.Contains(netip.MustParseAddr(tc.in).Unmap().WithZone("")) {
			t.Errorf("IPKey(%s) = %s does not contain the address", tc.in, got)
		}
		if IPKey(got.Addr()) != got {
			t.Errorf("IPKey is not idempotent for %s", tc.in)
		}
	}
	if got := IPKey(netip.Addr{}); got != (netip.Prefix{}) {
		t.Errorf("IPKey(zero) = %v, want the zero Prefix", got)
	}
}

func TestLimiterBurstAndRefill(t *testing.T) {
	clk := newFakeClock()
	l := newLimiter[string](rate{Burst: 20, Every: 15 * time.Second}, 100, clk.now)
	for i := range 20 {
		if v := l.take("k"); !v.OK {
			t.Fatalf("take %d refused: %+v", i+1, v)
		}
	}
	v := l.take("k")
	if v.OK || v.RetryAfter != 15*time.Second || !v.First || v.retryAfterSeconds() != 15 {
		t.Fatalf("21st take = %+v, want a first refusal with RetryAfter 15s", v)
	}
	if v := l.take("k"); v.OK || v.First {
		t.Fatalf("22nd take = %+v, want a later refusal of the same episode", v)
	}
	if v := l.take("other"); !v.OK {
		t.Fatal("another key shares the bucket")
	}

	clk.advance(7 * time.Second)
	if v := l.take("k"); v.OK || v.RetryAfter != 8*time.Second {
		t.Fatalf("after 7s: %+v, want RetryAfter 8s", v)
	}
	clk.advance(8 * time.Second)
	if v := l.take("k"); !v.OK {
		t.Fatalf("after 15s: %+v, want one token", v)
	}
	v = l.take("k")
	if v.OK || !v.First {
		t.Fatalf("after the refilled token: %+v, want a first refusal of a new episode", v)
	}

	// A full refill (20 × 15 s) gives the whole burst back, and no more.
	clk.advance(time.Hour)
	for i := range 20 {
		if v := l.take("k"); !v.OK {
			t.Fatalf("after an hour, take %d refused", i+1)
		}
	}
	if v := l.take("k"); v.OK {
		t.Fatal("the bucket held more than its burst")
	}
}

func TestLimiterCheckSpendReset(t *testing.T) {
	clk := newFakeClock()
	l := newLimiter[string](rate{Burst: 5, Every: 2 * time.Minute}, 100, clk.now)
	if v := l.check("u"); !v.OK || l.size() != 0 {
		t.Fatalf("check on a new key = %+v with %d keys; want OK and no entry", v, l.size())
	}
	for i := range 5 {
		if v := l.check("u"); !v.OK {
			t.Fatalf("check before failure %d refused", i+1)
		}
		l.spend("u")
	}
	v := l.check("u")
	if v.OK || v.RetryAfter != 2*time.Minute || !v.First {
		t.Fatalf("check after 5 failures = %+v, want a first refusal with RetryAfter 2m", v)
	}
	if v := l.check("u"); v.OK || v.First {
		t.Fatalf("second check = %+v, want a refusal of the same episode", v)
	}
	l.spend("u") // spending an empty bucket keeps it empty, not deeper
	clk.advance(2 * time.Minute)
	if v := l.check("u"); !v.OK {
		t.Fatalf("check after 2m = %+v, want OK", v)
	}
	l.spend("u")
	if v := l.check("u"); v.OK || !v.First {
		t.Fatalf("check after the next failure = %+v, want a first refusal of a new episode", v)
	}

	l.reset("u")
	if v := l.check("u"); !v.OK || l.size() != 0 {
		t.Fatalf("after reset: %+v with %d keys", v, l.size())
	}
	l.reset("absent") // no-op
}

func TestLimiterOneKeyPerIPv6Slash64(t *testing.T) {
	clk := newFakeClock()
	l := newLimiter[netip.Prefix](rateAuthIP, maxLimiterKeys, clk.now)
	base := netip.MustParseAddr("2001:db8:aa:bb::").As16()
	for i := range 20 {
		a := base
		a[15], a[14], a[8] = byte(i), byte(i*7), byte(i*13) // different addresses in one /64
		if v := l.take(IPKey(netip.AddrFrom16(a))); !v.OK {
			t.Fatalf("attempt %d from the /64 refused", i+1)
		}
	}
	if v := l.take(IPKey(netip.MustParseAddr("2001:db8:aa:bb:dead:beef:1:2"))); v.OK {
		t.Fatal("the 21st attempt from one /64 passed")
	}
	if v := l.take(IPKey(netip.MustParseAddr("2001:db8:aa:bc::1"))); !v.OK {
		t.Fatal("the neighbouring /64 is blocked too")
	}
	if l.size() != 2 {
		t.Fatalf("%d keys, want 2", l.size())
	}
}

func TestLimiterEvictsFullBucketsFirst(t *testing.T) {
	clk := newFakeClock()
	l := newLimiter[string](rate{Burst: 3, Every: time.Minute}, 3, clk.now)
	for range 3 {
		l.take("a") // a: empty, full again in 3 minutes
	}
	l.take("b") // b: one token short, full again in a minute
	for range 3 {
		l.take("c")
	}
	clk.advance(time.Minute + time.Second) // b is full now; a and c are not
	l.check("b")                           // b becomes the most recently used key; a is the least
	if got := keysOf(l); !slices.Equal(got, []string{"b", "c", "a"}) {
		t.Fatalf("LRU order %v, want [b c a]", got)
	}
	l.take("d")
	if got := keysOf(l); !slices.Equal(got, []string{"d", "c", "a"}) {
		t.Fatalf("keys after inserting d: %v; want the full bucket b evicted before the least recently used a", got)
	}

	// No full bucket left: least recently used keys go, and a refused attempt counts as a use.
	l.take("e")
	if got := keysOf(l); !slices.Equal(got, []string{"e", "d", "c"}) {
		t.Fatalf("keys after inserting e: %v; want a evicted", got)
	}
	l.take("d")          // d: used after c …
	if !l.take("c").OK { // … and c has one refilled token left: take it
		t.Fatal("c has no token after a minute")
	}
	l.take("e")
	if l.take("c").OK { // this refusal still counts as a use of c
		t.Fatal("c passed more than its burst")
	}
	l.take("f")
	if got := keysOf(l); !slices.Equal(got, []string{"f", "c", "e"}) {
		t.Fatalf("keys after inserting f: %v; want d evicted", got)
	}
}

func keysOf[K comparable](l *limiter[K]) []K {
	l.mu.Lock()
	defer l.mu.Unlock()
	var ks []K
	for b := l.head; b != nil; b = b.next {
		ks = append(ks, b.key)
	}
	return ks
}

func TestLimiterListStaysConsistent(t *testing.T) {
	clk := newFakeClock()
	l := newLimiter[int](rate{Burst: 2, Every: time.Second}, 50, clk.now)
	x := 1 // a deterministic pseudo-random walk (LCG), so a failure reproduces
	next := func(n int) int {
		x = (x*1103515245 + 12345) % (1 << 31)
		return (x >> 8) % n
	}
	for i := range 5000 {
		k := next(80)
		switch next(4) {
		case 0:
			l.take(k)
		case 1:
			l.check(k)
		case 2:
			l.spend(k)
		case 3:
			l.reset(k)
		}
		if i%7 == 0 {
			clk.advance(time.Duration(next(1500)) * time.Millisecond)
		}
		n := 0
		var prev *bucket[int]
		for b := l.head; b != nil; b = b.next {
			if b.prev != prev || l.m[b.key] != b {
				t.Fatalf("step %d: list and map disagree at key %d", i, b.key)
			}
			prev, n = b, n+1
		}
		if l.tail != prev || n != len(l.m) || n > 50 {
			t.Fatalf("step %d: list has %d entries, map %d (cap 50)", i, n, len(l.m))
		}
	}
}

func TestLimiterConcurrent(t *testing.T) {
	clk := newFakeClock()
	l := newLimiter[string](rate{Burst: 100, Every: time.Hour}, 1000, clk.now)
	small := newLimiter[string](rate{Burst: 2, Every: time.Second}, 10, clk.now) // evicts all the time
	var wg sync.WaitGroup
	var mu sync.Mutex
	passed := 0
	for g := range 8 {
		wg.Go(func() {
			for i := range 200 {
				if l.take("shared").OK {
					mu.Lock()
					passed++
					mu.Unlock()
				}
				l.check("shared")
				key := fmt.Sprint(g, i%20)
				small.take(key)
				small.spend(key)
				small.check(key)
				if i%50 == 0 {
					small.reset(key)
					clk.advance(time.Second)
				}
			}
		})
	}
	wg.Wait()
	if passed != 100 {
		t.Fatalf("%d takes passed, want exactly the burst of 100", passed)
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want int
	}{
		{0, 1}, {time.Millisecond, 1}, {200 * time.Millisecond, 1}, {time.Second, 1},
		{time.Second + time.Nanosecond, 2}, {7100 * time.Millisecond, 8}, {12 * time.Minute, 720},
	} {
		if got := (verdict{RetryAfter: tc.d}).retryAfterSeconds(); got != tc.want {
			t.Errorf("retryAfterSeconds(%v) = %d, want %d", tc.d, got, tc.want)
		}
	}
}

func TestHashBudget(t *testing.T) {
	r, err := HashBudget{}.rate()
	if err != nil || r != (rate{Burst: 20, Every: 200 * time.Millisecond}) {
		t.Fatalf("zero HashBudget rate = %+v, %v; want DefaultHashBudget", r, err)
	}
	for _, b := range []HashBudget{{Burst: 0, PerSecond: 5}, {Burst: 5, PerSecond: 0}, {Burst: -1, PerSecond: -1}} {
		if _, err := b.rate(); err == nil {
			t.Errorf("%+v.rate() = nil error", b)
		}
		if _, err := newThrottles(time.Now, b); err == nil {
			t.Errorf("newThrottles(%+v) = nil error", b)
		}
	}
}

// TestThrottleRates checks every bucket of 03 §7.3 against its burst and refill.
func TestThrottleRates(t *testing.T) {
	clk := newFakeClock()
	th, err := newThrottles(clk.now, HashBudget{})
	if err != nil {
		t.Fatal(err)
	}
	ip := IPKey(netip.MustParseAddr("198.51.100.7"))
	takes := []struct {
		name  string
		burst int
		every time.Duration
		take  func() verdict
	}{
		{"auth-ip", 20, 15 * time.Second, func() verdict { return th.authIP.take(ip) }},
		{"register-ip", 5, 12 * time.Minute, func() verdict { return th.registerIP.take(ip) }},
		{"auth-hash", 20, 200 * time.Millisecond, func() verdict { return th.authHash.take(struct{}{}) }},
		{"push-test", 1, 10 * time.Second, func() verdict { return th.pushTest.take("k3m9p2qxw7ht") }},
		{"block log line", 1, time.Minute, func() verdict { return th.blockLog.take(ip) }},
		{"login_failed audit", 600, 6 * time.Second, func() verdict { return th.loginFailedAudit.take(struct{}{}) }},
		{"global throttled audit", 1, time.Hour, func() verdict { return th.globalThrottledAudit.take(struct{}{}) }},
	}
	for _, tc := range takes {
		for i := range tc.burst {
			if v := tc.take(); !v.OK {
				t.Fatalf("%s: take %d refused", tc.name, i+1)
			}
		}
		if v := tc.take(); v.OK || v.RetryAfter != tc.every {
			t.Errorf("%s: take %d = %+v, want a refusal with RetryAfter %v", tc.name, tc.burst+1, v, tc.every)
		}
	}
	failures := []struct {
		name         string
		burst        int
		every        time.Duration
		check        func() verdict
		spend        func()
		otherKeyIsOK func() bool
	}{
		{"auth-user-ip", 5, 2 * time.Minute,
			func() verdict { return th.authUserIP.check(userIPKey{"alex", ip}) },
			func() { th.authUserIP.spend(userIPKey{"alex", ip}) },
			func() bool {
				other := IPKey(netip.MustParseAddr("203.0.113.9"))
				return th.authUserIP.check(userIPKey{"alex", other}).OK && th.authUserIP.check(userIPKey{"sam", ip}).OK
			}},
		{"auth-user", 30, 2 * time.Minute,
			func() verdict { return th.authUser.check("alex") },
			func() { th.authUser.spend("alex") },
			func() bool { return th.authUser.check("sam").OK }},
	}
	for _, tc := range failures {
		for range tc.burst {
			if v := tc.check(); !v.OK {
				t.Fatalf("%s: refused before %d failures", tc.name, tc.burst)
			}
			tc.spend()
		}
		if v := tc.check(); v.OK || v.RetryAfter != tc.every {
			t.Errorf("%s: after %d failures = %+v, want a refusal with RetryAfter %v", tc.name, tc.burst, v, tc.every)
		}
		if !tc.otherKeyIsOK() {
			t.Errorf("%s: another key is blocked too", tc.name)
		}
	}
}

// TestHashBudgetCapsAnonymousHashes is 03 §15's hash-budget case at the bucket level: 1000 logins for random
// usernames from 1000 random /64s over 10 simulated seconds reach the hash at most 20 + 5 × 10 = 70 times. (The
// service-level version, with the counting hasher and the 503 answer, comes with Login.)
func TestHashBudgetCapsAnonymousHashes(t *testing.T) {
	clk := newFakeClock()
	th, err := newThrottles(clk.now, HashBudget{})
	if err != nil {
		t.Fatal(err)
	}
	hashes, refused := 0, 0
	for range 1000 {
		var a [16]byte
		a[0], a[1] = 0x20, 0x01
		_, _ = rand.Read(a[2:8]) // a random /64 in 2001::/16
		ip := IPKey(netip.AddrFrom16(a))
		user := "user-" + rand.Text()
		if !th.authIP.take(ip).OK || !th.authUserIP.check(userIPKey{user, ip}).OK || !th.authUser.check(user).OK {
			t.Fatal("a fresh address and username was throttled")
		}
		if v := th.authHash.take(struct{}{}); v.OK {
			hashes++
		} else {
			refused++
			if v.retryAfterSeconds() < 1 {
				t.Fatalf("Retry-After %d", v.retryAfterSeconds())
			}
		}
		clk.advance(10 * time.Millisecond)
	}
	if hashes > 70 || hashes < 65 {
		t.Fatalf("%d hashes over 10 s, want at most 70 (and close to it)", hashes)
	}
	if hashes+refused != 1000 {
		t.Fatal("lost attempts")
	}
}
