package auth

import (
	"bytes"
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

// Web sessions (03 §7.4): the cookie, the lookup with its 30 s cache, the use bookkeeping, the daily token rotation
// and the after-commit steps of a revocation (03 §7.7). The CSRF wrapper of 03 §7.5 is here too.

// Session limits of 03 §7.4 and §14.
const (
	sessionIdleTTL     = 30 * 24 * time.Hour  // idle_expires_at = min(last use + 30 d, expires_at)
	sessionMaxTTL      = 180 * 24 * time.Hour // expires_at = created_at + 180 d
	sessionRotateAfter = 24 * time.Hour       // a REST request rotates a token this old
	sessionPrevGrace   = 60 * time.Second     // the previous token stays valid this long after a rotation
	sessionTouchEvery  = 5 * time.Minute      // last_seen_at, last_ip and idle_expires_at are written at most this often
	sessionCacheTTL    = 30 * time.Second
	maxSessionsPerUser = 50 // the 51st session evicts the least recently seen one
)

// rotateTimeout bounds MaybeRotate's write. MaybeRotate has no request and so no context (03 §7.13), and the
// rotation should not depend on the client staying connected anyway.
const rotateTimeout = 5 * time.Second

// Cookie names (03 §7.4). The __Host- prefix pins the cookie to exactly this host: no Domain, Path=/, Secure.
const (
	sessionCookieSecure = "__Host-isshoni_session" // an https public origin: every TLS mode, and off behind an HTTPS proxy
	sessionCookieDev    = "isshoni_session"        // a plain-http public origin (task dev on http://localhost)
)

// clientKindWeb is Principal.ClientKind of a web session.
const clientKindWeb = "web"

// ---- the cookie ----

// sessionCookie is this server's session cookie: its name and whether it is Secure, both from the scheme of the
// primary origin.
type sessionCookie struct {
	name   string
	secure bool
}

// cookieFor picks the cookie for a normalized primary origin.
func cookieFor(primary string) sessionCookie {
	if strings.HasPrefix(primary, "https://") {
		return sessionCookie{name: sessionCookieSecure, secure: true}
	}
	return sessionCookie{name: sessionCookieDev}
}

// read returns the value of the request's session cookie. Only this server's cookie name counts.
func (c sessionCookie) read(r *http.Request) (string, bool) {
	if c.name == "" {
		return "", false
	}
	ck, err := r.Cookie(c.name)
	if err != nil {
		return "", false
	}
	return ck.Value, true
}

// build returns the cookie with the attributes of 03 §7.4: Path=/, HttpOnly, SameSite=Lax (not Strict: invite and
// notification links opened from other apps must arrive logged in), Secure on https, no Domain. maxAge < 0 expires
// the cookie.
func (c sessionCookie) build(value string, maxAge int) *http.Cookie {
	//nolint:gosec // G124: HttpOnly and SameSite=Lax always; Secure exactly when the public origin is https (a
	// plain-http dev origin cannot use a Secure cookie, 03 §7.4).
	return &http.Cookie{
		Name:     c.name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   c.secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// SetSessionCookie sets the session cookie (03 §7.4):
//
//	Set-Cookie: __Host-isshoni_session=<43-char token>; Path=/; Max-Age=2592000; HttpOnly; Secure; SameSite=Lax
//
// The name is __Host-isshoni_session for an https public origin and isshoni_session, without Secure, for a
// plain-http one (dev). Max-Age is the time left until idleExpires, in whole seconds.
func (s *Service) SetSessionCookie(w http.ResponseWriter, token string, idleExpires time.Time) {
	maxAge := int(idleExpires.Sub(s.now()) / time.Second)
	http.SetCookie(w, s.cookie.build(token, max(maxAge, 1)))
}

// ClearSessionCookie expires the session cookie: the same name and attributes with an empty value and Max-Age=0.
func (s *Service) ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, s.cookie.build("", -1))
}

// ---- CSRF (03 §7.5) ----

// CSRF wraps next in the REST cross-origin and Content-Type check of 03 §7.5: a CSRFGuard that trusts
// Options.Origins().Public. A refused request gets 403 csrf_failed or 415 unsupported_media_type in 03's error
// envelope; safe methods (GET, HEAD, OPTIONS) always pass. httpapi's /api/v1 chain puts it in front of every route.
//
// A Service that New did not build has no guard: its handler fails closed with 500 internal, without calling next.
func (s *Service) CSRF(next http.Handler) http.Handler {
	if s == nil || s.csrf == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeErrorEnvelope(w, http.StatusInternalServerError, api.CodeInternal)
		})
	}
	return s.csrf.Handler(next)
}

// ---- authentication ----

// Authenticate returns the caller of a REST request: the session cookie in M1 (bearer tokens in M2). A missing,
// invalid or expired credential, or a user who is not active, is *api.Error{unauthenticated}. In M1 so is any
// Authorization header (03 §7.5): httpapi's /api/v1 chain already answers such a request with 401 before its CSRF
// step, on every route, and Authenticate rejects the header too for any other caller.
//
// It also records the use (03 §7.4 "Touch"): last_seen_at, last_ip and idle_expires_at, at most once per 5 minutes
// per session and best effort. It never rotates the token; the REST chain calls MaybeRotate for that.
func (s *Service) Authenticate(r *http.Request) (Principal, error) {
	if _, ok := r.Header["Authorization"]; ok {
		return Principal{}, errUnauthenticated()
	}
	tok, ok := s.cookie.read(r)
	if !ok {
		return Principal{}, errUnauthenticated()
	}
	sess, user, err := s.resolve(r.Context(), tok)
	if err != nil {
		return Principal{}, err
	}
	s.recordUse(r.Context(), sess, s.clientIP(r))
	return principalOf(sess, user), nil
}

// AuthenticateCookie is Authenticate for /ws only (03 §7.6): the session cookie, never rotated, ignoring
// Authorization. A request without the cookie returns ErrNoCookie; an invalid, expired or non-active session returns
// *api.Error{unauthenticated}; any other error (the store) is not unauthenticated, and the hub answers 503. It does
// not record the use: the hub calls Touch at connect and every 5 minutes.
func (s *Service) AuthenticateCookie(r *http.Request) (Principal, error) {
	tok, ok := s.cookie.read(r)
	if !ok {
		return Principal{}, ErrNoCookie
	}
	sess, user, err := s.resolve(r.Context(), tok)
	if err != nil {
		return Principal{}, err
	}
	return principalOf(sess, user), nil
}

// principalOf is the Principal of a web session.
func principalOf(sess store.Session, user store.User) Principal {
	return Principal{
		UserID:     user.ID,
		Username:   user.Username,
		Role:       user.Role,
		Method:     MethodSession,
		SessionID:  sess.ID,
		ClientKind: clientKindWeb,
	}
}

// resolve turns a raw session token into its live session and its active user: from the cache, or with one indexed
// read. Anything else is *api.Error{unauthenticated}: a token of the wrong shape (refused before any hashing or DB
// access), an unknown or expired session, a previous token past its 60 s grace, a user who is not active.
func (s *Service) resolve(ctx context.Context, token string) (store.Session, store.User, error) {
	if !tokenWellFormed(token, sessionTokenBytes) {
		return store.Session{}, store.User{}, errUnauthenticated()
	}
	hash := s.keys.sessionTokenHash(token)
	now := s.now()
	if sess, user, ok := s.sessions.byHash(hash, now); ok {
		return sess, user, nil
	}
	gen := s.sessions.generation()
	var sess store.Session
	var user store.User
	err := s.db.Read(ctx, func(q *store.Q) error {
		var err error
		sess, user, err = q.SessionByTokenHash(hash, now)
		return err
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		return store.Session{}, store.User{}, errUnauthenticated()
	case err != nil:
		return store.Session{}, store.User{}, internalErr("authenticate", err)
	}
	if user.Status != store.StatusActive {
		return store.Session{}, store.User{}, errUnauthenticated()
	}
	s.sessions.put(gen, hash, sess, user, now)
	return sess, user, nil
}

// Touch is the hub's Revalidate (03 §7.4, §7.6). On every call it checks, from the cache or with a read, that the
// session exists, is unexpired and belongs to an active user, and returns *api.Error{unauthenticated} otherwise:
// that is the only error that means the credential is gone, and the hub keeps the connection on any other. It then
// records the use at most once per 5 minutes; that write is best effort, so its failure is logged and never
// returned. A 2-hour session therefore never idles out. (M2: a bearer principal checks its device row.)
func (s *Service) Touch(ctx context.Context, p Principal, ip netip.Addr) error {
	if p.Method != MethodSession || p.UserID == "" || p.SessionID == "" {
		return errUnauthenticated() // no devices before M2
	}
	sess, err := s.sessionOf(ctx, p)
	if err != nil {
		return err
	}
	s.recordUse(ctx, sess, ip)
	return nil
}

// sessionOf finds the principal's live session of an active user, from the cache or with a read.
func (s *Service) sessionOf(ctx context.Context, p Principal) (store.Session, error) {
	now := s.now()
	if sess, _, ok := s.sessions.byID(p.SessionID, now); ok {
		if sess.UserID != p.UserID {
			return store.Session{}, errUnauthenticated()
		}
		return sess, nil
	}
	gen := s.sessions.generation()
	var sess store.Session
	var user store.User
	err := s.db.Read(ctx, func(q *store.Q) error {
		var err error
		if user, err = q.UserByID(p.UserID); err != nil {
			return err
		}
		sess, err = sessionByID(q, p.UserID, p.SessionID)
		return err
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		return store.Session{}, errUnauthenticated()
	case err != nil:
		return store.Session{}, internalErr("touch", err)
	}
	if user.Status != store.StatusActive || !sessionLive(sess, now) {
		return store.Session{}, errUnauthenticated()
	}
	s.sessions.put(gen, sess.TokenHash, sess, user, now)
	return sess, nil
}

// sessionByID returns one session of a user, or store.ErrNotFound. The store finds sessions by token hash or by
// user, so this walks the user's list (at most 50 rows).
func sessionByID(q *store.Q, u store.UserID, id store.SessionID) (store.Session, error) {
	list, err := q.ListSessions(u)
	if err != nil {
		return store.Session{}, err
	}
	for _, sess := range list {
		if sess.ID == id {
			return sess, nil
		}
	}
	return store.Session{}, store.ErrNotFound
}

// sessionLive reports whether a session is unexpired at now. Both limits are inclusive, like the store's.
func sessionLive(sess store.Session, now time.Time) bool {
	return !now.After(sess.IdleExpiresAt) && !now.After(sess.ExpiresAt)
}

// recordUse writes a use of the session: last_seen_at, last_ip and idle_expires_at = min(now + 30 d, expires_at). It
// writes at most once per 5 minutes per session; an in-memory timestamp decides, starting from the row's
// last_seen_at. The write is best effort: a failure is logged and the next use after 5 minutes tries again.
func (s *Service) recordUse(ctx context.Context, sess store.Session, ip netip.Addr) {
	now := s.now()
	if !s.sessions.claimTouch(sess.ID, sess.LastSeenAt, now) {
		return
	}
	idle := now.Add(sessionIdleTTL)
	lastIP := ipString(ip)
	err := s.db.Write(ctx, func(q *store.Q) error { return q.TouchSession(sess.ID, lastIP, now, idle) })
	switch {
	case err == nil:
		s.sessions.touched(sess.ID, now, idle, lastIP)
	case errors.Is(err, store.ErrNotFound):
		// Deleted or expired a moment ago: the next lookup says so.
	default:
		level := slog.LevelWarn
		if ctx.Err() != nil {
			level = slog.LevelDebug // the client went away
		}
		s.log.LogAttrs(ctx, level, "could not record the session's last use",
			slog.String("user_id", string(sess.UserID)), slog.String("err", err.Error()))
	}
}

// MaybeRotate is the REST chain's rotation step (03 §7.4): once the session's token is 24 h old, it gives the
// session a new token and sets the cookie on w. The old token stays valid for 60 s, so parallel requests and other
// tabs keep working. The session ID stays, and so do the push subscriptions tied to it. WebSocket upgrades never
// call it.
//
// The decision is re-checked inside the write, so of several parallel requests with an old token exactly one
// rotates; the others keep the old token, which the browser replaces when that one response arrives. The rotation
// also counts as a use: Max-Age is a fresh 30 days (or what is left of the 180). A failure is logged and the request
// goes on with its current token; the next request tries again.
func (s *Service) MaybeRotate(w http.ResponseWriter, p Principal) {
	if p.Method != MethodSession || p.SessionID == "" {
		return
	}
	now := s.now()
	cached, _, ok := s.sessions.byID(p.SessionID, now)
	if !ok || cached.UserID != p.UserID || now.Sub(cached.RotatedAt) < sessionRotateAfter {
		return
	}
	token := newToken(sessionTokenBytes)
	hash := s.keys.sessionTokenHash(token)
	idle := now.Add(sessionIdleTTL)
	rotated := false

	// MaybeRotate has no request context (03 §7.13); see rotateTimeout.
	ctx, cancel := context.WithTimeout(context.Background(), rotateTimeout)
	defer cancel()
	err := s.db.Write(ctx, func(q *store.Q) error {
		cur, err := sessionByID(q, p.UserID, p.SessionID)
		if errors.Is(err, store.ErrNotFound) {
			return nil // revoked a moment ago
		}
		if err != nil {
			return err
		}
		if !sessionLive(cur, now) || now.Sub(cur.RotatedAt) < sessionRotateAfter {
			return nil // expired, or another request rotated it first
		}
		if err := q.RotateSession(cur.ID, hash, now.Add(sessionPrevGrace), now); err != nil {
			return err
		}
		if err := q.TouchSession(cur.ID, "", now, idle); err != nil {
			return err
		}
		if idle.After(cur.ExpiresAt) {
			idle = cur.ExpiresAt
		}
		rotated = true
		return nil
	})
	if err != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "could not rotate the session token",
			slog.String("user_id", string(p.UserID)), slog.String("err", err.Error()))
		return
	}
	// The cached row has the old hashes: drop it, so the next request reads the rotated one.
	s.sessions.invalidateSession(p.SessionID)
	if rotated {
		s.SetSessionCookie(w, token, idle)
	}
}

// ---- revocation (03 §7.7) ----

// sessionsRevoked runs the after-commit steps of 03 §7.7 for sessions whose rows were just deleted: the session
// cache is invalidated first, and only then are their live connections closed with reason. No code path deletes a
// session row without this call or revocationCommitted (revoke.go: the rows that take a user's credentials as a
// whole), except the janitor's prune of expired sessions and the startup purge after a session-key change.
func (s *Service) sessionsRevoked(user store.UserID, ids []store.SessionID, reason string) {
	for _, id := range ids {
		s.sessions.invalidateSession(id)
	}
	if s.conns == nil {
		return
	}
	for _, id := range ids {
		s.conns.CloseConnections(ConnSelector{UserID: user, SessionID: id}, reason)
	}
}

// ---- new sessions ----

// newSession is what a Write that creates a session leaves for the steps after its commit.
type newSession struct {
	result LoginResult
	// replaced is the session of the cookie the request arrived with, deleted in the same Write; nil for none.
	replaced *store.Session
	// evicted are the sessions the 50-session cap deleted.
	evicted []store.SessionID
}

// openSession creates a web session for user inside a Write and fills ns: it first deletes the session of the
// cookie the request arrived with (03 §7.4: a login always issues a new token, and the old session goes), then
// inserts the new row and applies the cap of 50 sessions per user, which deletes the least recently seen ones.
// token is the new raw token; only its hash is stored.
func (s *Service) openSession(q *store.Q, user store.User, token string, m ReqMeta, now time.Time, ns *newSession) error {
	if tokenWellFormed(m.SessionToken, sessionTokenBytes) {
		old, _, err := q.SessionByTokenHash(s.keys.sessionTokenHash(m.SessionToken), now)
		switch {
		case errors.Is(err, store.ErrNotFound):
			// No live session behind the cookie: an expired one is the janitor's.
		case err != nil:
			return err
		default:
			if _, err := q.DeleteSession(old.UserID, old.ID); err != nil {
				return err
			}
			ns.replaced = &old
		}
	}
	sess := store.Session{
		UserID:        user.ID,
		TokenHash:     s.keys.sessionTokenHash(token),
		Name:          DescribeUserAgent(m.UserAgent),
		CreatedAt:     now,
		LastIP:        ipString(m.IP),
		IdleExpiresAt: now.Add(sessionIdleTTL),
		ExpiresAt:     now.Add(sessionMaxTTL),
	}
	if err := q.CreateSession(&sess); err != nil {
		return err
	}
	evicted, err := q.TrimSessions(user.ID, maxSessionsPerUser)
	if err != nil {
		return err
	}
	ns.evicted = evicted
	ns.result = LoginResult{User: user, Session: sess, Token: token}
	return nil
}

// sessionOpened runs the after-commit steps of openSession (03 §7.7): the replaced session and the sessions the cap
// evicted leave the cache, and their connections are closed with ReasonSessionRevoked.
func (s *Service) sessionOpened(ns *newSession) {
	if ns.replaced != nil {
		s.sessionsRevoked(ns.replaced.UserID, []store.SessionID{ns.replaced.ID}, ReasonSessionRevoked)
	}
	if len(ns.evicted) > 0 {
		s.sessionsRevoked(ns.result.User.ID, ns.evicted, ReasonSessionRevoked)
	}
}

// ---- the cache ----

// sessionCache is the in-memory session cache of 03 §7.4: token hash → (session, user), valid for 30 s. Only live
// sessions of active users are cached. Every revoke, role, status and rename path invalidates it synchronously, by
// session or by user, after its commit; there is only one server process. It also holds each session's in-memory
// "last use recorded" timestamp.
type sessionCache struct {
	mu     sync.Mutex
	byKey  map[string]*sessionState          // token hash (current or previous) → state
	byIDs  map[store.SessionID]*sessionState // session ID → state
	gen    uint64                            // incremented by every invalidation
	sweptT time.Time
}

// sessionState is one cached session.
type sessionState struct {
	sess    store.Session
	user    store.User
	loaded  time.Time // when the rows were read; they count for sessionCacheTTL
	touched time.Time // the last use this process recorded (zero: none yet)
	keys    []string  // the byKey entries that point here
}

// sessionSweepEvery is how often put drops states nobody refreshed for sessionStateKeep.
const (
	sessionSweepEvery = time.Minute
	sessionStateKeep  = 2 * sessionTouchEvery
)

func newSessionCache() *sessionCache {
	return &sessionCache{byKey: map[string]*sessionState{}, byIDs: map[store.SessionID]*sessionState{}}
}

// fresh reports whether a state's rows still count at now, and the session is unexpired.
func (st *sessionState) fresh(now time.Time) bool {
	age := now.Sub(st.loaded)
	return age >= 0 && age < sessionCacheTTL && sessionLive(st.sess, now)
}

// byHash returns the cached session for a token hash. A hash that is the session's previous one counts only until
// prev_valid_until.
func (c *sessionCache) byHash(hash []byte, now time.Time) (store.Session, store.User, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.byKey[string(hash)]
	if st == nil || !st.fresh(now) {
		return store.Session{}, store.User{}, false
	}
	if !bytes.Equal(hash, st.sess.TokenHash) &&
		(!bytes.Equal(hash, st.sess.PrevTokenHash) || now.After(st.sess.PrevValidUntil)) {
		return store.Session{}, store.User{}, false
	}
	return st.sess, st.user, true
}

// byID returns the cached session with that ID.
func (c *sessionCache) byID(id store.SessionID, now time.Time) (store.Session, store.User, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.byIDs[id]
	if st == nil || !st.fresh(now) {
		return store.Session{}, store.User{}, false
	}
	return st.sess, st.user, true
}

// generation returns a number that every invalidation changes. A reader takes it before its DB read and hands it to
// put, which then drops rows that an invalidation has overtaken: without it, a lookup that read just before a
// revocation committed could cache the revoked session after the revocation had cleared the cache.
func (c *sessionCache) generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

// put caches rows read at generation gen under the token hash they were found by.
func (c *sessionCache) put(gen uint64, hash []byte, sess store.Session, user store.User, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != c.gen {
		return
	}
	c.sweep(now)
	st := c.byIDs[sess.ID]
	if st == nil {
		st = &sessionState{}
		c.byIDs[sess.ID] = st
	}
	st.sess, st.user, st.loaded = sess, user, now
	key := string(hash)
	if c.byKey[key] != st {
		c.byKey[key] = st
		st.keys = append(st.keys, key)
	}
}

// claimTouch reports whether a use at now should be written, and if so claims it, so that parallel requests write
// once. lastSeen is the row's last_seen_at, which decides for a session this process has not recorded yet.
func (c *sessionCache) claimTouch(id store.SessionID, lastSeen, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.byIDs[id]
	last := lastSeen
	if st != nil && st.touched.After(last) {
		last = st.touched
	}
	if now.Sub(last) < sessionTouchEvery {
		return false
	}
	if st != nil {
		st.touched = now
	}
	return true
}

// touched updates the cached row after a recorded use.
func (c *sessionCache) touched(id store.SessionID, now, idle time.Time, ip string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.byIDs[id]
	if st == nil {
		return
	}
	st.sess.LastSeenAt = now
	if idle.After(st.sess.ExpiresAt) {
		idle = st.sess.ExpiresAt
	}
	st.sess.IdleExpiresAt = idle
	if ip != "" {
		st.sess.LastIP = ip
	}
}

// invalidateSession drops one session.
func (c *sessionCache) invalidateSession(id store.SessionID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	c.drop(id)
}

// invalidateUser drops every session of a user (role, status and rename changes; revoking all of a user's sessions).
func (c *sessionCache) invalidateUser(u store.UserID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	for id, st := range c.byIDs {
		if st.sess.UserID == u {
			c.drop(id)
		}
	}
}

func (c *sessionCache) drop(id store.SessionID) {
	st := c.byIDs[id]
	if st == nil {
		return
	}
	for _, k := range st.keys {
		delete(c.byKey, k)
	}
	delete(c.byIDs, id)
}

// sweep drops the states nobody refreshed for sessionStateKeep, at most once per sessionSweepEvery. A state outlives
// its 30 s of rows because of its touched timestamp; after sessionStateKeep the row's own last_seen_at is as good.
func (c *sessionCache) sweep(now time.Time) {
	if d := now.Sub(c.sweptT); d >= 0 && d < sessionSweepEvery {
		return
	}
	c.sweptT = now
	for id, st := range c.byIDs {
		if age := now.Sub(st.loaded); age < 0 || age >= sessionStateKeep {
			c.drop(id)
		}
	}
}
