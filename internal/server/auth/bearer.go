package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/store"
)

// Device access tokens on REST and in 01's hello (M2 02 §6.1, §6.3; README WA39): the grammar of the Authorization
// header, the lookup of an access token with its 30 s cache, the bearer principal, and the device half of Touch with
// its last-seen bookkeeping. The refresh_token grant that issues the tokens is refresh.go.

// Limits of M2 02 §6.1 and §15.
const (
	bearerCacheTTL   = 30 * time.Second // a cached access token counts this long, and never past its own expiry
	deviceTouchEvery = 5 * time.Minute  // last_seen_at and last_ip of a device are written at most this often
)

// bearerScheme is the auth-scheme of the Authorization header (RFC 6750 §2.1), compared without regard to case.
const bearerScheme = "Bearer"

// errInvalidToken is 401 invalid_token (M2 02 §6.1): an Authorization header that is not one bearer access token, or
// an access token that is unknown, expired, or a user's who is not active. httpapi adds the WWW-Authenticate header.
func errInvalidToken() error { return api.NewError(api.CodeInvalidToken) }

// accessTokenWellFormed reports whether tok has an access token's shape (M2 02 §3): "isa_" and 43 canonical
// base64url characters that decode to 32 bytes. Anything else is refused before any hashing or DB access.
func accessTokenWellFormed(tok string) bool {
	rest, ok := strings.CutPrefix(tok, accessTokenPrefix)
	return ok && tokenWellFormed(rest, deviceTokenBytes)
}

// refreshTokenWellFormed is accessTokenWellFormed for a refresh token: "isr_" and 43 characters.
func refreshTokenWellFormed(tok string) bool {
	rest, ok := strings.CutPrefix(tok, refreshTokenPrefix)
	return ok && tokenWellFormed(rest, deviceTokenBytes)
}

// bearerToken returns the access token of a request's Authorization header by M2 02 §6.1's grammar: exactly one
// Authorization header, whose value is the scheme Bearer (any case), one space and an access token of the shape of
// §3. ok is false for anything else: no header or two, another scheme, a refresh token, a wrong length, extra
// spaces.
func bearerToken(h http.Header) (string, bool) {
	vals := h.Values("Authorization")
	if len(vals) != 1 {
		return "", false
	}
	scheme, tok, ok := strings.Cut(vals[0], " ")
	if !ok || !strings.EqualFold(scheme, bearerScheme) || !accessTokenWellFormed(tok) {
		return "", false
	}
	return tok, true
}

// bearerPrincipal is the Principal of a device (M2 02 §6.1): the user as the row has it now, and the device's ID,
// kind and current name.
func bearerPrincipal(d store.Device, u store.User) Principal {
	return Principal{
		UserID:     u.ID,
		Username:   u.Username,
		Role:       u.Role,
		Method:     MethodBearer,
		DeviceID:   d.ID,
		ClientKind: d.ClientKind,
		DeviceName: d.Name,
	}
}

// ---- authentication ----

// AuthenticateBearerToken checks a device access token (M2 02 §6.1, §6.3): 01's hello.auth through the wiring, and
// the REST bearer path of Authenticate. A token of the wrong shape is refused before any hashing or DB access; then
// the bearer cache answers, or one indexed read (DeviceTokenByHash). An unknown or expired token, or a user who is not
// active, is *api.Error{invalid_token}; any other failure wraps *api.Error{internal}. The principal carries the
// device's current name in DeviceName.
//
// It does not record the use: the hub calls Touch at connect and every 5 minutes, as for cookies
// (AuthenticateCookie). An expired access token does not matter after hello: the connection lives until its device
// is revoked (M2 02 §6.3).
func (s *Service) AuthenticateBearerToken(ctx context.Context, token string) (Principal, error) {
	dev, user, err := s.resolveBearer(ctx, token)
	if err != nil {
		return Principal{}, err
	}
	return bearerPrincipal(dev, user), nil
}

// authenticateBearer is Authenticate's bearer path (M2 02 §6.1): the header's grammar, the token, and the use of the
// device. The request's cookies are not read.
func (s *Service) authenticateBearer(r *http.Request) (Principal, error) {
	tok, ok := bearerToken(r.Header)
	if !ok {
		return Principal{}, errInvalidToken()
	}
	dev, user, err := s.resolveBearer(r.Context(), tok)
	if err != nil {
		return Principal{}, err
	}
	s.recordDeviceUse(r.Context(), dev, s.clientIP(r))
	return bearerPrincipal(dev, user), nil
}

// resolveBearer turns an access token into its device and the device's active user: from the cache, or with one
// read. A token the cache knows to have expired is refused without a read (an access token's expiry never moves).
func (s *Service) resolveBearer(ctx context.Context, token string) (store.Device, store.User, error) {
	if !accessTokenWellFormed(token) {
		return store.Device{}, store.User{}, errInvalidToken()
	}
	hash := s.keys.deviceHash(token)
	now := s.now()
	switch dev, user, hit := s.bearers.byToken(hash, now); hit {
	case cacheHit:
		return dev, user, nil
	case cacheExpired:
		return store.Device{}, store.User{}, errInvalidToken()
	}
	gen := s.bearers.generation()
	tok, dev, user, err := s.accessToken(ctx, hash, now)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return store.Device{}, store.User{}, errInvalidToken()
	case err != nil:
		return store.Device{}, store.User{}, internalErr("authenticate bearer", err)
	case user.Status != store.StatusActive:
		return store.Device{}, store.User{}, errInvalidToken()
	}
	s.bearers.put(gen, hash, tok.ExpiresAt, dev, user, now)
	return dev, user, nil
}

// readAccessToken is Service.accessToken outside tests: the unexpired access token with this hash, with its device
// and the device's user (DeviceTokenByHash), in a Read of its own.
func (s *Service) readAccessToken(ctx context.Context, hash []byte, now time.Time) (store.DeviceToken, store.Device,
	store.User, error,
) {
	var tok store.DeviceToken
	var dev store.Device
	var user store.User
	err := s.db.Read(ctx, func(q *store.Q) error {
		var err error
		tok, dev, user, err = q.DeviceTokenByHash(hash, tokenKindAccess, now)
		return err
	})
	return tok, dev, user, err
}

// ---- Touch for devices (M2 02 §6.3 item 3) ----

// touchDevice is TouchPrincipal for a bearer principal: the device must exist, be the principal's user's, and that
// user must be active (the cache, or one read), else *api.Error{unauthenticated}; then the use is recorded at most
// once per 5 minutes, best effort. It returns the principal as the rows have it now, with the device's current name.
func (s *Service) touchDevice(ctx context.Context, p Principal, ip netip.Addr) (Principal, error) {
	if p.UserID == "" || p.DeviceID == "" {
		return Principal{}, errUnauthenticated()
	}
	dev, user, err := s.deviceOf(ctx, p)
	if err != nil {
		return Principal{}, err
	}
	s.recordDeviceUse(ctx, dev, ip)
	return bearerPrincipal(dev, user), nil
}

// deviceOf finds the principal's device and its active user, from the cache or with a read (DeviceByID).
func (s *Service) deviceOf(ctx context.Context, p Principal) (store.Device, store.User, error) {
	now := s.now()
	if dev, user, ok := s.bearers.byDevice(p.DeviceID, now); ok {
		if dev.UserID != p.UserID {
			return store.Device{}, store.User{}, errUnauthenticated()
		}
		return dev, user, nil
	}
	gen := s.bearers.generation()
	var dev store.Device
	var user store.User
	err := s.db.Read(ctx, func(q *store.Q) error {
		var err error
		dev, user, err = q.DeviceByID(p.DeviceID)
		return err
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		return store.Device{}, store.User{}, errUnauthenticated()
	case err != nil:
		return store.Device{}, store.User{}, internalErr("touch", err)
	case dev.UserID != p.UserID || user.Status != store.StatusActive:
		return store.Device{}, store.User{}, errUnauthenticated()
	}
	s.bearers.putDevice(gen, dev, user, now)
	return dev, user, nil
}

// recordDeviceUse writes a use of the device: last_seen_at = now and last_ip (an unknown address keeps the stored
// one). It writes at most once per 5 minutes per device (M2 02 §6.1); an in-memory timestamp decides, starting from
// the row's last_seen_at. The write is best effort: a failure is logged and the next use after 5 minutes tries again.
func (s *Service) recordDeviceUse(ctx context.Context, dev store.Device, ip netip.Addr) {
	now := s.now()
	if !s.bearers.claimTouch(dev.ID, dev.LastSeenAt, now) {
		return
	}
	lastIP := ipString(ip)
	err := s.db.Write(ctx, func(q *store.Q) error { return q.TouchDevice(dev.ID, lastIP, "", now) })
	switch {
	case err == nil:
		s.bearers.touched(dev.ID, now, lastIP)
	case errors.Is(err, store.ErrNotFound):
		// Revoked a moment ago: the next lookup says so.
	default:
		level := slog.LevelWarn
		if ctx.Err() != nil {
			level = slog.LevelDebug // the client went away
		}
		s.log.LogAttrs(ctx, level, "could not record the device's last use",
			slog.String("user_id", string(dev.UserID)), slog.String("device_id", string(dev.ID)),
			slog.String("err", err.Error()))
	}
}

// ---- revocation (M2 02 §6.5) ----

// forgetUser drops every cached web session and device of a user. Every path that changes a user (role, status,
// name) or takes the user's credentials as a whole calls it after its commit and before ConnCloser (03 §7.4, §7.7;
// M2 02 §6.1).
func (s *Service) forgetUser(u store.UserID) {
	s.sessions.invalidateUser(u)
	s.bearers.invalidateUser(u)
}

// devicesRevoked runs the after-commit steps of 03 §7.7 for devices whose rows were just deleted (M2 02 §6.5): the
// bearer cache forgets them first, and only then are their live connections closed with ReasonDeviceRevoked. Revoking
// a device, the app's own sign-out, the device cap's eviction and a refresh-token reuse end with it.
func (s *Service) devicesRevoked(user store.UserID, ids ...store.DeviceID) {
	for _, id := range ids {
		s.bearers.invalidateDevice(id)
	}
	if s.conns == nil {
		return
	}
	for _, id := range ids {
		s.conns.CloseConnections(ConnSelector{UserID: user, DeviceID: id}, ReasonDeviceRevoked)
	}
}

// ---- the cache ----

// cacheAnswer is what the bearer cache knows about an access token.
type cacheAnswer uint8

const (
	cacheMiss    cacheAnswer = iota // nothing usable: read the rows
	cacheHit                        // the token, its device and its active user, read less than 30 s ago
	cacheExpired                    // the token's own expires_at has passed: invalid without a read
)

// bearerCache is the in-memory cache of M2 02 §6.1: access-token hash → (device, user, token expiry), each entry
// valid for min(30 s, token expiry − now). Only tokens of active users are cached. It also holds, per device, the
// rows Touch reads by device ID and the in-memory "last use recorded" timestamp. Every path that deletes a device or
// changes a user forgets that device's or user's entries synchronously, after its commit and before ConnCloser runs
// (forgetUser, devicesRevoked); there is only one server process.
type bearerCache struct {
	mu      sync.Mutex
	tokens  map[string]*accessEntry         // access-token hash → entry
	devices map[store.DeviceID]*deviceState // device ID → state
	gen     uint64                          // incremented by every invalidation
	sweptT  time.Time
}

// deviceState is one cached device.
type deviceState struct {
	device  store.Device
	user    store.User
	loaded  time.Time // when device and user were read; they count for bearerCacheTTL
	touched time.Time // the last use this process recorded (zero: none yet)
	tokens  []string  // the tokens entries that point here
}

// accessEntry is one cached access token.
type accessEntry struct {
	state   *deviceState
	expires time.Time // the token's expires_at: it is valid while now < expires
	loaded  time.Time // when the token was read
}

// The sweep drops states and tokens nobody read again for bearerStateKeep, at most once per bearerSweepEvery.
const (
	bearerSweepEvery = time.Minute
	bearerStateKeep  = 2 * deviceTouchEvery
)

func newBearerCache() *bearerCache {
	return &bearerCache{tokens: map[string]*accessEntry{}, devices: map[store.DeviceID]*deviceState{}}
}

// fresh reports whether rows read at loaded still count at now.
func fresh(loaded, now time.Time) bool {
	age := now.Sub(loaded)
	return age >= 0 && age < bearerCacheTTL
}

// byToken returns the cached device and user of an access-token hash.
func (c *bearerCache) byToken(hash []byte, now time.Time) (store.Device, store.User, cacheAnswer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.tokens[string(hash)]
	switch {
	case e == nil:
		return store.Device{}, store.User{}, cacheMiss
	case !now.Before(e.expires):
		return store.Device{}, store.User{}, cacheExpired
	case !fresh(e.loaded, now):
		return store.Device{}, store.User{}, cacheMiss
	}
	return e.state.device, e.state.user, cacheHit
}

// byDevice returns the cached rows of a device.
func (c *bearerCache) byDevice(id store.DeviceID, now time.Time) (store.Device, store.User, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.devices[id]
	if st == nil || !fresh(st.loaded, now) {
		return store.Device{}, store.User{}, false
	}
	return st.device, st.user, true
}

// generation returns a number that every invalidation changes. A reader takes it before its DB read and hands it to
// put or putDevice, which then drop rows that an invalidation has overtaken (sessionCache.generation has the why).
func (c *bearerCache) generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

// state returns the device's state, creating it, with the rows read at now. The caller holds mu.
func (c *bearerCache) state(dev store.Device, user store.User, now time.Time) *deviceState {
	st := c.devices[dev.ID]
	if st == nil {
		st = &deviceState{}
		c.devices[dev.ID] = st
	}
	st.device, st.user, st.loaded = dev, user, now
	return st
}

// put caches an access token's rows read at generation gen.
func (c *bearerCache) put(gen uint64, hash []byte, expires time.Time, dev store.Device, user store.User, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != c.gen {
		return
	}
	c.sweep(now)
	st := c.state(dev, user, now)
	key := string(hash)
	if e := c.tokens[key]; e == nil || e.state != st {
		st.tokens = append(st.tokens, key)
	}
	c.tokens[key] = &accessEntry{state: st, expires: expires, loaded: now}
}

// putDevice caches a device's rows read at generation gen (Touch's read by device ID).
func (c *bearerCache) putDevice(gen uint64, dev store.Device, user store.User, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != c.gen {
		return
	}
	c.sweep(now)
	c.state(dev, user, now)
}

// claimTouch reports whether a use at now should be written, and if so claims it, so that parallel requests write
// once. lastSeen is the row's last_seen_at, which decides for a device this process has not recorded yet.
func (c *bearerCache) claimTouch(id store.DeviceID, lastSeen, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.devices[id]
	last := lastSeen
	if st != nil && st.touched.After(last) {
		last = st.touched
	}
	if now.Sub(last) < deviceTouchEvery {
		return false
	}
	if st != nil {
		st.touched = now
	}
	return true
}

// touched updates the cached row after a recorded use.
func (c *bearerCache) touched(id store.DeviceID, now time.Time, ip string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.devices[id]
	if st == nil {
		return
	}
	st.device.LastSeenAt = now
	if ip != "" {
		st.device.LastIP = ip
	}
}

// invalidateDevice drops one device with its tokens.
func (c *bearerCache) invalidateDevice(id store.DeviceID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	c.drop(id)
}

// invalidateUser drops every device of a user.
func (c *bearerCache) invalidateUser(u store.UserID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	for id, st := range c.devices {
		if st.user.ID == u || st.device.UserID == u {
			c.drop(id)
		}
	}
}

func (c *bearerCache) drop(id store.DeviceID) {
	st := c.devices[id]
	if st == nil {
		return
	}
	for _, k := range st.tokens {
		if e := c.tokens[k]; e != nil && e.state == st {
			delete(c.tokens, k)
		}
	}
	delete(c.devices, id)
}

// size returns how many tokens and devices the cache holds (tests).
func (c *bearerCache) size() (tokens, devices int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.tokens), len(c.devices)
}

// sweep drops, at most once per bearerSweepEvery, the tokens and devices nobody read again for bearerStateKeep. A
// state outlives its 30 s of rows because of its touched timestamp, and a token entry because it knows its expiry;
// after bearerStateKeep the rows say as much. The caller holds mu.
func (c *bearerCache) sweep(now time.Time) {
	if d := now.Sub(c.sweptT); d >= 0 && d < bearerSweepEvery {
		return
	}
	c.sweptT = now
	stale := func(loaded time.Time) bool { age := now.Sub(loaded); return age < 0 || age >= bearerStateKeep }
	for k, e := range c.tokens {
		if stale(e.loaded) {
			delete(c.tokens, k)
		}
	}
	for id, st := range c.devices {
		if stale(st.loaded) {
			c.drop(id)
			continue
		}
		kept := st.tokens[:0]
		for _, k := range st.tokens {
			if e := c.tokens[k]; e != nil && e.state == st {
				kept = append(kept, k)
			}
		}
		st.tokens = kept
	}
}
