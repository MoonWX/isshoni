package signal

import (
	"net/netip"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 12, 19, 0, 0, 0, time.UTC)

func TestTokenBucket(t *testing.T) {
	b := newBucket(perMinute(20), t0) // one token per 3 s
	for i := range 20 {
		if ok, _ := b.take(t0, 1); !ok {
			t.Fatalf("take %d refused", i+1)
		}
	}
	ok, wait := b.take(t0, 1)
	if ok || wait != 3*time.Second {
		t.Fatalf("21st: ok %v, wait %v; want refused, 3s", ok, wait)
	}
	if ok, wait := b.take(t0.Add(time.Second), 1); ok || wait != 2*time.Second {
		t.Errorf("after 1 s: ok %v, wait %v; want refused, 2s", ok, wait)
	}
	if ok, _ := b.take(t0.Add(3*time.Second), 1); !ok {
		t.Error("refused after exactly one refill period")
	}
	if b.full(t0.Add(time.Minute)) {
		t.Error("full after 57 s of refill")
	}
	if !b.full(t0.Add(time.Minute + 3*time.Second)) {
		t.Error("not full after a whole minute of refill")
	}
	// A request larger than the burst needs a full bucket.
	if ok, _ := b.take(t0.Add(time.Hour), 50); !ok {
		t.Error("an over-burst request refused on a full bucket")
	}
}

func TestIPKey(t *testing.T) {
	for _, tc := range []struct{ addr, want string }{
		{"198.51.100.7", "198.51.100.7/32"},
		{"::ffff:198.51.100.7", "198.51.100.7/32"},
		{"2001:db8:1:2:3:4:5:6", "2001:db8:1:2::/64"},
		{"2001:db8:1:2::1%eth0", "2001:db8:1:2::/64"},
		{"::1", "::/64"},
	} {
		if got := ipKey(netip.MustParseAddr(tc.addr)); got != netip.MustParsePrefix(tc.want) {
			t.Errorf("ipKey(%s) = %v, want %s", tc.addr, got, tc.want)
		}
	}
	if got := ipKey(netip.Addr{}); got != (netip.Prefix{}) {
		t.Errorf("ipKey(zero) = %v", got)
	}
}

func TestKeyedLimiterEviction(t *testing.T) {
	l := newKeyedLimiter(perMinute(2), 3)
	key := func(i byte) netip.Prefix { return netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 0, 2, i}), 32) }
	for i := range byte(3) {
		l.allow(key(i), t0)
	}
	// A fourth key evicts one entry, never growing past the cap.
	l.allow(key(3), t0)
	if n := l.size(); n != 3 {
		t.Errorf("%d keys, want 3", n)
	}
	// Full buckets go first: after a minute every other bucket has refilled.
	l.allow(key(3), t0.Add(30*time.Second))
	l.allow(key(4), t0.Add(2*time.Minute))
	if n := l.size(); n > 3 {
		t.Errorf("%d keys, want at most 3", n)
	}
	if ok, _ := l.allow(key(3), t0.Add(2*time.Minute)); !ok {
		t.Error("a refilled key refused")
	}
}

func TestConnLimitsFlood(t *testing.T) {
	l := newConnLimits(DefaultConfig().Limits, t0)
	now := t0
	for range 100 {
		if ok, _, _ := l.global(now, 100); !ok {
			t.Fatal("burst refused")
		}
	}
	ok, wait, flood := l.global(now, 100)
	if ok || flood || wait != 50*time.Millisecond {
		t.Fatalf("101st: ok %v, flood %v, wait %v", ok, flood, wait)
	}
	// 9.9 s of flooding: not yet.
	for now = t0; now.Before(t0.Add(9900 * time.Millisecond)); now = now.Add(10 * time.Millisecond) {
		if _, _, flood := l.global(now, 100); flood {
			t.Fatalf("flood after %v", now.Sub(t0))
		}
	}
	for ; now.Before(t0.Add(11 * time.Second)); now = now.Add(10 * time.Millisecond) {
		if _, _, flood := l.global(now, 100); flood {
			if d := now.Sub(t0); d < 10*time.Second {
				t.Fatalf("flood after %v", d)
			}
			return
		}
	}
	t.Fatal("no flood after 11 s")
}

func TestConnLimitsBytes(t *testing.T) {
	l := newConnLimits(DefaultConfig().Limits, t0)
	for range 4 {
		if ok, _, _ := l.global(t0, 256<<10); !ok {
			t.Fatal("1 MiB burst refused")
		}
	}
	ok, wait, _ := l.global(t0, 256<<10)
	if ok || wait != 500*time.Millisecond {
		t.Errorf("5th 256 KiB message: ok %v, wait %v; want refused, 500ms", ok, wait)
	}
	if got := l.refillTime(); got != 2*time.Second {
		t.Errorf("refill time %v, want 2s (1 MiB at 512 KiB/s)", got)
	}
}

func TestRetryAfterRounding(t *testing.T) {
	for _, tc := range []struct {
		d       time.Duration
		ms, sec int
	}{
		{0, 1, 1},
		{time.Nanosecond, 1, 1},
		{50 * time.Millisecond, 50, 1},
		{50*time.Millisecond + 1, 51, 1},
		{3 * time.Second, 3000, 3},
		{3*time.Second + 1, 3001, 4},
	} {
		if got := retryAfterMs(tc.d); got != tc.ms {
			t.Errorf("retryAfterMs(%v) = %d, want %d", tc.d, got, tc.ms)
		}
		if got := retryAfterSeconds(tc.d); got != tc.sec {
			t.Errorf("retryAfterSeconds(%v) = %d, want %d", tc.d, got, tc.sec)
		}
	}
}
