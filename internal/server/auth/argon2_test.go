package auth

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/crypto/argon2"
)

// testArgon keeps unit tests fast; DefaultArgon is covered by the known-answer test.
var testArgon = ArgonParams{MemoryKiB: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

// Known answers computed with an independent argon2id implementation (Node's crypto.argon2Sync, which uses
// OpenSSL), so a swapped algorithm or encoding shows up here.
const (
	katDefaultPassword = "correct horse battery staple"
	katDefaultPHC      = "$argon2id$v=19$m=19456,t=2,p=1$aXNzaG9uaS1rYXQtc2FsdA$ZJP3fRX6kA2m+YutmbXw3eWqIF+Fou9Br8PMF1mnw+8"
	katForeignPassword = "password"
	katForeignPHC      = "$argon2id$v=19$m=65536,t=3,p=4$c29tZXNhbHRzb21lc2FsdA$gduXp+Z6iReEolmbyHn5V8s1EtJzmEvZfYoY/Fn/AeI"
)

// countingDerive wraps argon2.IDKey and counts calls.
func countingDerive(n *atomic.Int32) deriveFunc {
	return func(pw, salt []byte, t, m uint32, p uint8, k uint32) []byte {
		n.Add(1)
		return argon2.IDKey(pw, salt, t, m, p, k)
	}
}

func TestPHCRoundTrip(t *testing.T) {
	for _, s := range []string{katDefaultPHC, katForeignPHC} {
		h, err := parsePHC(s)
		if err != nil {
			t.Fatalf("parsePHC(%q) = %v", s, err)
		}
		if got := h.encode(); got != s {
			t.Fatalf("encode(parsePHC(%q)) = %q", s, got)
		}
	}
	h, err := parsePHC(katForeignPHC)
	if err != nil {
		t.Fatal(err)
	}
	want := ArgonParams{MemoryKiB: 65536, Time: 3, Threads: 4, SaltLen: 16, KeyLen: 32}
	if h.Params != want || string(h.Salt) != "somesaltsomesalt" || len(h.Hash) != 32 {
		t.Fatalf("parsePHC(foreign) = %+v", h)
	}
	if d, _ := parsePHC(katDefaultPHC); d.Params != DefaultArgon {
		t.Fatalf("parsePHC(default) params = %+v, want DefaultArgon", d.Params)
	}
}

func TestParsePHCRejects(t *testing.T) {
	salt, sum := "c29tZXNhbHRzb21lc2FsdA", "gduXp+Z6iReEolmbyHn5V8s1EtJzmEvZfYoY/Fn/AeI"
	for _, s := range []string{
		"",
		"plaintext",
		"$argon2i$v=19$m=65536,t=3,p=4$" + salt + "$" + sum,  // argon2i
		"$argon2d$v=19$m=65536,t=3,p=4$" + salt + "$" + sum,  // argon2d
		"$argon2id$v=16$m=65536,t=3,p=4$" + salt + "$" + sum, // old version
		"$argon2id$m=65536,t=3,p=4$" + salt + "$" + sum,      // no version
		"$argon2id$v=19$t=3,m=65536,p=4$" + salt + "$" + sum, // parameter order
		"$argon2id$v=19$m=65536,t=3$" + salt + "$" + sum,     // missing p
		"$argon2id$v=19$m=65536,t=3,p=4,k=1$" + salt + "$" + sum,
		"$argon2id$v=19$m=065536,t=3,p=4$" + salt + "$" + sum, // leading zero
		"$argon2id$v=19$m=+65536,t=3,p=4$" + salt + "$" + sum,
		"$argon2id$v=19$m=,t=3,p=4$" + salt + "$" + sum,
		"$argon2id$v=19$m=65536,t=0,p=4$" + salt + "$" + sum,   // t < 1
		"$argon2id$v=19$m=65536,t=3,p=0$" + salt + "$" + sum,   // p < 1
		"$argon2id$v=19$m=16,t=3,p=4$" + salt + "$" + sum,      // m < 8p
		"$argon2id$v=19$m=4194304,t=3,p=4$" + salt + "$" + sum, // 4 GiB
		"$argon2id$v=19$m=65536,t=1000,p=4$" + salt + "$" + sum,
		"$argon2id$v=19$m=65536,t=3,p=255$" + salt + "$" + sum,
		"$argon2id$v=19$m=65536,t=3,p=256$" + salt + "$" + sum,
		"$argon2id$v=19$m=4294967296,t=3,p=4$" + salt + "$" + sum,
		"$argon2id$v=19$m=65536,t=3,p=4$" + salt + "==$" + sum, // padding
		"$argon2id$v=19$m=65536,t=3,p=4$" + salt + "$" + sum + "=",
		"$argon2id$v=19$m=65536,t=3,p=4$c29tZQ$" + sum,        // 4-byte salt
		"$argon2id$v=19$m=65536,t=3,p=4$" + salt + "$c2hvcnQ", // 5-byte hash
		"$argon2id$v=19$m=65536,t=3,p=4$" + salt + "$" + sum + "$",
		"$argon2id$v=19$m=65536,t=3,p=4$" + salt,
		"$argon2id$v=19$m=65536,t=3,p=4$" + strings.ReplaceAll(salt, "c", "-") + "$" + sum, // url alphabet
		"$argon2id$v=19$m=65536,t=3,p=4$" + salt + "$" + sum[:len(sum)-1] + "J",            // non-canonical tail
		"$argon2id$v=19$m=65536,t=3,p=4$" + strings.Repeat("A", 90) + "$" + sum,            // 67-byte salt
		"$argon2id$v=19$m=65536,t=3,p=4$" + salt + "$" + strings.Repeat("A", 180),          // 135-byte hash
	} {
		if h, err := parsePHC(s); !errors.Is(err, errBadHash) {
			t.Errorf("parsePHC(%q) = %+v, %v; want errBadHash", s, h, err)
		}
	}
}

func TestArgonParamsValidate(t *testing.T) {
	if err := DefaultArgon.validate(); err != nil {
		t.Fatalf("DefaultArgon.validate() = %v", err)
	}
	for _, p := range []ArgonParams{
		{},
		{MemoryKiB: 19456, Time: 2, Threads: 0, SaltLen: 16, KeyLen: 32},
		{MemoryKiB: 19456, Time: 0, Threads: 1, SaltLen: 16, KeyLen: 32},
		{MemoryKiB: 7, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 32},
		{MemoryKiB: 19456, Time: 2, Threads: 1, SaltLen: 4, KeyLen: 32},
		{MemoryKiB: 19456, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 8},
		{MemoryKiB: 2 << 20, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 32},
	} {
		if err := p.validate(); err == nil {
			t.Errorf("%+v.validate() = nil", p)
		}
	}
	if _, err := newHasher(ArgonParams{MemoryKiB: 1, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}, 1, nil); err == nil {
		t.Error("newHasher accepted invalid parameters")
	}
}

func TestVerifyKnownAnswers(t *testing.T) {
	h, err := newHasher(DefaultArgon, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, tc := range []struct {
		name, pw, phc string
		ok, rehash    bool
	}{
		{"current parameters", katDefaultPassword, katDefaultPHC, true, false},
		{"current parameters, wrong password", katDefaultPassword + "!", katDefaultPHC, false, false},
		{"foreign parameters rehash", katForeignPassword, katForeignPHC, true, true},
		{"foreign parameters, wrong password", "Password", katForeignPHC, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, rehash, err := h.verify(ctx, tc.pw, tc.phc)
			if err != nil || ok != tc.ok || rehash != tc.rehash {
				t.Fatalf("verify = %v, %v, %v; want %v, %v, nil", ok, rehash, err, tc.ok, tc.rehash)
			}
		})
	}
}

func TestHashAndVerify(t *testing.T) {
	var calls atomic.Int32
	h, err := newHasher(testArgon, 2, countingDerive(&calls))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	phc, err := h.hash(ctx, "a long enough password")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Fatalf("hash = %q", phc)
	}
	parsed, err := parsePHC(phc)
	if err != nil || parsed.Params != testArgon {
		t.Fatalf("parsePHC(hash) = %+v, %v", parsed, err)
	}
	if other, _ := h.hash(ctx, "a long enough password"); other == phc {
		t.Fatal("two hashes of one password share a salt")
	}
	if ok, rehash, err := h.verify(ctx, "a long enough password", phc); !ok || rehash || err != nil {
		t.Fatalf("verify(right) = %v, %v, %v", ok, rehash, err)
	}
	if ok, _, err := h.verify(ctx, "a long enough passwore", phc); ok || err != nil {
		t.Fatalf("verify(wrong) = %v, %v", ok, err)
	}

	// Different current parameters: a match asks for a rehash.
	h2, err := newHasher(ArgonParams{MemoryKiB: 128, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok, rehash, err := h2.verify(ctx, "a long enough password", phc); !ok || !rehash || err != nil {
		t.Fatalf("verify with new parameters = %v, %v, %v; want a match that asks for a rehash", ok, rehash, err)
	}

	// A malformed stored hash is an error and costs no hashing.
	before := calls.Load()
	if ok, _, err := h.verify(ctx, "x", "$argon2id$v=19$garbage"); ok || !errors.Is(err, errBadHash) {
		t.Fatalf("verify(malformed) = %v, %v", ok, err)
	}
	if calls.Load() != before {
		t.Fatal("verify hashed for a malformed stored hash")
	}
}

func TestNewHasherDefaults(t *testing.T) {
	h, err := newHasher(ArgonParams{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if h.params != DefaultArgon {
		t.Fatalf("zero params gave %+v, want DefaultArgon", h.params)
	}
	if cap(h.slots) != hashConcurrency() || hashConcurrency() < 2 {
		t.Fatalf("concurrency %d, want max(2, NumCPU/2) = %d", cap(h.slots), hashConcurrency())
	}
	d, err := parsePHC(h.dummy)
	if err != nil || d.Params != DefaultArgon {
		t.Fatalf("dummy hash %q: %+v, %v", h.dummy, d, err)
	}
}

func TestDummyHash(t *testing.T) {
	var calls atomic.Int32
	h, err := newHasher(testArgon, 2, countingDerive(&calls))
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("newHasher hashed %d times, want 1 (the dummy)", calls.Load())
	}
	if _, err := parsePHC(h.dummy); err != nil {
		t.Fatalf("dummy %q: %v", h.dummy, err)
	}
	h2, err := newHasher(testArgon, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if h2.dummy == h.dummy {
		t.Fatal("two hashers share a dummy hash")
	}
	// An unknown user costs exactly one hash, like a real one, and never matches.
	for _, pw := range []string{"", "anything at all", h.dummy} {
		before := calls.Load()
		if err := h.verifyDummy(context.Background(), pw); err != nil {
			t.Fatalf("verifyDummy = %v", err)
		}
		if n := calls.Load() - before; n != 1 {
			t.Fatalf("verifyDummy hashed %d times, want 1", n)
		}
	}
}

// blockingHasher returns a hasher whose hashes block until gate closes, and counts how many run at once.
func blockingHasher(t *testing.T, concurrency int, gate <-chan struct{}, maxInFlight *atomic.Int32) *hasher {
	t.Helper()
	h, err := newHasher(testArgon, concurrency, nil)
	if err != nil {
		t.Fatal(err)
	}
	var inFlight atomic.Int32
	h.derive = func(_, _ []byte, _, _ uint32, _ uint8, k uint32) []byte {
		n := inFlight.Add(1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		<-gate
		inFlight.Add(-1)
		return make([]byte, k)
	}
	return h
}

func TestHasherSemaphore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		var maxInFlight atomic.Int32
		h := blockingHasher(t, 2, gate, &maxInFlight)
		ctx := context.Background()
		results := make(chan error, 2+maxHashWaiters)
		hash := func() {
			_, err := h.hash(ctx, "a long enough password")
			results <- err
		}

		for range 2 { // both slots busy
			go hash()
		}
		synctest.Wait()
		for range maxHashWaiters { // 32 callers wait
			go hash()
		}
		synctest.Wait()
		if n := h.waiting.Load(); n != maxHashWaiters {
			t.Fatalf("%d waiters, want %d", n, maxHashWaiters)
		}

		// The 33rd waiter is refused at once, and so is a verify.
		start := time.Now()
		if _, err := h.hash(ctx, "x"); !errors.Is(err, errHashBusy) {
			t.Fatalf("33rd waiter: %v, want errHashBusy", err)
		}
		if _, _, err := h.verify(ctx, "x", h.dummy); !errors.Is(err, errHashBusy) {
			t.Fatalf("33rd verify: %v, want errHashBusy", err)
		}
		if waited := time.Since(start); waited != 0 {
			t.Fatalf("the 33rd waiter waited %v", waited)
		}

		// Everyone else gets through, never more than 2 at once.
		close(gate)
		for range 2 + maxHashWaiters {
			if err := <-results; err != nil {
				t.Fatalf("hash: %v", err)
			}
		}
		if m := maxInFlight.Load(); m != 2 {
			t.Fatalf("at most %d hashes ran at once, want 2", m)
		}
		if n := h.waiting.Load(); n != 0 {
			t.Fatalf("%d waiters left", n)
		}
	})
}

func TestHasherWaitTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		var maxInFlight atomic.Int32
		h := blockingHasher(t, 1, gate, &maxInFlight)
		ctx := context.Background()
		done := make(chan error, 1)
		go func() {
			_, err := h.hash(ctx, "holder")
			done <- err
		}()
		synctest.Wait()

		start := time.Now()
		_, err := h.hash(ctx, "waiter")
		if !errors.Is(err, errHashBusy) {
			t.Fatalf("waiter: %v, want errHashBusy", err)
		}
		if waited := time.Since(start); waited != maxHashWait {
			t.Fatalf("waited %v, want %v", waited, maxHashWait)
		}

		// A slot freed within the 10 s is taken.
		go func() {
			time.Sleep(maxHashWait - time.Second)
			close(gate)
		}()
		start = time.Now()
		if _, err := h.hash(ctx, "second waiter"); err != nil {
			t.Fatalf("second waiter: %v", err)
		}
		if waited := time.Since(start); waited != maxHashWait-time.Second {
			t.Fatalf("second waiter waited %v", waited)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func TestHasherWaitCancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		var maxInFlight atomic.Int32
		h := blockingHasher(t, 1, gate, &maxInFlight)
		done := make(chan struct{})
		go func() {
			_, _ = h.hash(context.Background(), "holder")
			close(done)
		}()
		synctest.Wait()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := h.hash(ctx, "waiter")
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errHashBusy) {
			t.Fatalf("cancelled waiter: %v, want context.DeadlineExceeded", err)
		}
		close(gate)
		<-done
	})
}

func FuzzParsePHC(f *testing.F) {
	for _, s := range []string{
		katDefaultPHC, katForeignPHC, "$argon2id$v=19$m=64,t=1,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=19$", "$argon2id$v=19$m=1,t=1,p=1$$", "$argon2id$v=19$m=08,t=1,p=1$A$A",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		h, err := parsePHC(s)
		if err != nil {
			if !errors.Is(err, errBadHash) {
				t.Fatalf("parsePHC(%q) error %v does not wrap errBadHash", s, err)
			}
			return
		}
		if err := h.Params.validate(); err != nil {
			t.Fatalf("parsePHC(%q) accepted invalid parameters: %v", s, err)
		}
		// Only the canonical form parses, so encoding gives the input back.
		if got := h.encode(); got != s {
			t.Fatalf("encode(parsePHC(%q)) = %q", s, got)
		}
	})
}
