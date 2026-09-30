package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/argon2"
)

// ArgonParams are the argon2id parameters of new password hashes (03 §7.2). A stored hash keeps the parameters it
// was made with, in its PHC string; a successful login with other parameters rehashes the password.
type ArgonParams struct {
	MemoryKiB, Time uint32
	Threads         uint8
	SaltLen, KeyLen uint32
}

// DefaultArgon is m = 19 MiB, t = 2, p = 1 with a 16-byte salt and a 32-byte key (03 §7.2, §14).
var DefaultArgon = ArgonParams{MemoryKiB: 19456, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 32}

// Bounds for parameters, both configured and parsed from a stored hash. The upper bounds keep a corrupted or
// hostile hash string from allocating gigabytes or running for minutes.
const (
	argonMaxMemoryKiB = 1 << 20 // 1 GiB
	argonMaxTime      = 64
	argonMaxThreads   = 16
	argonMinSaltLen   = 8
	argonMaxSaltLen   = 64
	argonMinKeyLen    = 16
	argonMaxKeyLen    = 128
)

// validate checks p against the bounds above and argon2's own rules (m ≥ 8·p, t ≥ 1, p ≥ 1).
func (p ArgonParams) validate() error {
	switch {
	case p.Threads < 1 || p.Threads > argonMaxThreads:
		return fmt.Errorf("auth: argon2 threads %d outside 1–%d", p.Threads, argonMaxThreads)
	case p.Time < 1 || p.Time > argonMaxTime:
		return fmt.Errorf("auth: argon2 time %d outside 1–%d", p.Time, argonMaxTime)
	case p.MemoryKiB < 8*uint32(p.Threads) || p.MemoryKiB > argonMaxMemoryKiB:
		return fmt.Errorf("auth: argon2 memory %d KiB outside %d–%d", p.MemoryKiB, 8*uint32(p.Threads), argonMaxMemoryKiB)
	case p.SaltLen < argonMinSaltLen || p.SaltLen > argonMaxSaltLen:
		return fmt.Errorf("auth: argon2 salt length %d outside %d–%d", p.SaltLen, argonMinSaltLen, argonMaxSaltLen)
	case p.KeyLen < argonMinKeyLen || p.KeyLen > argonMaxKeyLen:
		return fmt.Errorf("auth: argon2 key length %d outside %d–%d", p.KeyLen, argonMinKeyLen, argonMaxKeyLen)
	}
	return nil
}

// phcHash is a parsed PHC string: $argon2id$v=19$m=<m>,t=<t>,p=<p>$<salt>$<hash>, salt and hash in standard base64
// without padding. Params.SaltLen and Params.KeyLen are the decoded lengths.
type phcHash struct {
	Params     ArgonParams
	Salt, Hash []byte
}

const phcPrefix = "$argon2id$v=19$"

var phcB64 = base64.RawStdEncoding.Strict()

// encode returns the canonical PHC string.
func (h phcHash) encode() string {
	return fmt.Sprintf("%sm=%d,t=%d,p=%d$%s$%s", phcPrefix, h.Params.MemoryKiB, h.Params.Time, h.Params.Threads,
		phcB64.EncodeToString(h.Salt), phcB64.EncodeToString(h.Hash))
}

// parsePHC parses a stored argon2id hash. It accepts only the canonical form that encode writes (argon2id, version
// 19, the parameters m, t, p in that order, no optional fields) with parameters inside the bounds of validate.
// Every failure wraps errBadHash.
func parsePHC(s string) (phcHash, error) {
	bad := func(what string) (phcHash, error) { return phcHash{}, fmt.Errorf("%w: %s", errBadHash, what) }
	rest, ok := strings.CutPrefix(s, phcPrefix)
	if !ok {
		return bad("not an argon2id v=19 PHC string")
	}
	if strings.ContainsAny(rest, "\r\n") { // the base64 decoder skips them, even in strict mode
		return bad("line break")
	}
	parts := strings.Split(rest, "$")
	if len(parts) != 3 {
		return bad("want parameters, salt and hash")
	}
	fields := strings.Split(parts[0], ",")
	if len(fields) != 3 {
		return bad("want the parameters m, t and p")
	}
	m, okM := phcParam(fields[0], "m=", argonMaxMemoryKiB)
	t, okT := phcParam(fields[1], "t=", argonMaxTime)
	p, okP := phcParam(fields[2], "p=", argonMaxThreads)
	if !okM || !okT || !okP {
		return bad("parameters")
	}
	salt, err := phcB64.DecodeString(parts[1])
	if err != nil || len(salt) > argonMaxSaltLen {
		return bad("salt")
	}
	sum, err := phcB64.DecodeString(parts[2])
	if err != nil || len(sum) > argonMaxKeyLen {
		return bad("hash")
	}
	h := phcHash{Salt: salt, Hash: sum}
	h.Params = ArgonParams{
		MemoryKiB: m,
		Time:      t,
		Threads:   uint8(p),          //nolint:gosec // G115: phcParam bounds p by argonMaxThreads (16)
		SaltLen:   uint32(len(salt)), //nolint:gosec // G115: at most argonMaxSaltLen (64), checked above
		KeyLen:    uint32(len(sum)),  //nolint:gosec // G115: at most argonMaxKeyLen (128), checked above
	}
	if err := h.Params.validate(); err != nil {
		return bad(err.Error())
	}
	return h, nil
}

// phcParam parses one "<name><decimal>" PHC parameter: canonical decimal (no sign, no leading zero), at most limit.
func phcParam(field, name string, limit uint32) (uint32, bool) {
	num, ok := strings.CutPrefix(field, name)
	if !ok || num == "" || len(num) > 10 || (len(num) > 1 && num[0] == '0') {
		return 0, false
	}
	var v uint32
	for i := range len(num) {
		c := num[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		d := uint32(c - '0')
		if d > limit || v > (limit-d)/10 { // v*10 + d would exceed limit
			return 0, false
		}
		v = v*10 + d
	}
	return v, true
}

// Semaphore limits of 03 §7.2.
const (
	maxHashWaiters = 32               // the 33rd waiting caller gets errHashBusy at once
	maxHashWait    = 10 * time.Second // a longer wait gets errHashBusy
)

// hashConcurrency is how many hashes may run at once: max(2, NumCPU/2), so peak hashing memory stays N × 19 MiB.
func hashConcurrency() int { return max(2, runtime.NumCPU()/2) }

// deriveFunc has the signature of argon2.IDKey; tests substitute a counting or blocking one.
type deriveFunc func(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte

// hasher hashes and verifies passwords with argon2id under a semaphore (03 §7.2). Callers pass the password as
// normalized by CheckPassword or normalizePassword. Anonymous callers must also pass the auth-hash budget
// (throttles.authHash) first: that bucket caps the rate, the semaphore only how many run at once.
type hasher struct {
	params  ArgonParams
	slots   chan struct{} // one token per running hash
	waiting atomic.Int32  // callers blocked in acquire
	derive  deriveFunc
	dummy   string // PHC of 32 random bytes, made by newHasher
}

// newHasher returns a hasher for params (the zero value means DefaultArgon) that runs at most concurrency hashes at
// once (≤ 0 means hashConcurrency()). It computes the dummy hash right away, so it takes one hash's time.
func newHasher(params ArgonParams, concurrency int, derive deriveFunc) (*hasher, error) {
	if params == (ArgonParams{}) {
		params = DefaultArgon
	}
	if err := params.validate(); err != nil {
		return nil, err
	}
	if concurrency <= 0 {
		concurrency = hashConcurrency()
	}
	if derive == nil {
		derive = argon2.IDKey
	}
	h := &hasher{params: params, slots: make(chan struct{}, concurrency), derive: derive}
	random := make([]byte, 32)
	_, _ = rand.Read(random) // never fails since Go 1.24
	h.dummy = h.compute(random)
	return h, nil
}

// compute hashes pw with a fresh salt and the current parameters, without the semaphore.
func (h *hasher) compute(pw []byte) string {
	salt := make([]byte, h.params.SaltLen)
	_, _ = rand.Read(salt)
	sum := h.derive(pw, salt, h.params.Time, h.params.MemoryKiB, h.params.Threads, h.params.KeyLen)
	return phcHash{Params: h.params, Salt: salt, Hash: sum}.encode()
}

// acquire takes a hash slot. When none is free it waits, unless maxHashWaiters callers already wait; it gives up
// after maxHashWait or when ctx ends.
func (h *hasher) acquire(ctx context.Context) error {
	select {
	case h.slots <- struct{}{}:
		return nil
	default:
	}
	if h.waiting.Add(1) > maxHashWaiters {
		h.waiting.Add(-1)
		return errHashBusy
	}
	defer h.waiting.Add(-1)
	t := time.NewTimer(maxHashWait)
	defer t.Stop()
	select {
	case h.slots <- struct{}{}:
		return nil
	case <-t.C:
		return errHashBusy
	case <-ctx.Done():
		return fmt.Errorf("auth: waiting to hash: %w", ctx.Err())
	}
}

func (h *hasher) release() { <-h.slots }

// hash returns the PHC string of pw with the current parameters. It fails only with errHashBusy or a context error.
func (h *hasher) hash(ctx context.Context, pw string) (string, error) {
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()
	return h.compute([]byte(pw)), nil
}

// verify reports whether pw matches the stored PHC string, and whether the stored parameters differ from the
// current ones (rehash is only set on a match: the login then stores a new hash, 03 §7.2). A malformed stored hash
// returns an error wrapping errBadHash without hashing; a full semaphore returns errHashBusy.
func (h *hasher) verify(ctx context.Context, pw, stored string) (ok, rehash bool, err error) {
	ph, err := parsePHC(stored)
	if err != nil {
		return false, false, err
	}
	if err := h.acquire(ctx); err != nil {
		return false, false, err
	}
	sum := func() []byte {
		defer h.release()
		return h.derive([]byte(pw), ph.Salt, ph.Params.Time, ph.Params.MemoryKiB, ph.Params.Threads, ph.Params.KeyLen)
	}()
	if subtle.ConstantTimeCompare(sum, ph.Hash) != 1 {
		return false, false, nil
	}
	return true, ph.Params != h.params, nil
}

// verifyDummy spends exactly one verification on the dummy hash. Logins for unknown usernames, and for users whose
// hash is NULL, call it so that they cost the same CPU and time as a real user (03 §7.2). The answer is always "no
// match"; the error is errHashBusy or a context error.
func (h *hasher) verifyDummy(ctx context.Context, pw string) error {
	_, _, err := h.verify(ctx, pw, h.dummy)
	return err
}
