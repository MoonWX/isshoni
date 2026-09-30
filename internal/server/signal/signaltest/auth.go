package signaltest

import (
	"context"
	"net/http"
	"net/netip"
	"sync"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/signal"
)

// CookieName is the session cookie that Auth reads. The hub never parses cookie names (03 owns them); tests send it
// with CookieHeader.
const CookieName = "signaltest_session"

// CookieHeader returns request headers carrying the session cookie value (a cookie token: no spaces, quotes,
// commas or semicolons).
func CookieHeader(value string) http.Header {
	h := http.Header{}
	h.Set("Cookie", CookieName+"="+value)
	return h
}

// Auth is a fake signal.Authenticator backed by in-memory cookie sessions and bearer tokens.
//
//   - AuthenticateRequest: no CookieName cookie → ErrNoCredentials; an unknown or revoked session → ErrInvalid.
//   - AuthenticateBearer: an unknown or revoked token → ErrInvalid (like M1's wiring for every token).
//   - Revalidate: counts the calls; a revoked session or device → ErrInvalid; otherwise the identity with the name
//     and admin flag of SetUser, if any.
//
// Each Fail* method injects an error that the method returns until it is called again with nil; HoldRevalidate
// makes Revalidate slow.
type Auth struct {
	mu             sync.Mutex
	sessions       map[string]signal.Identity // cookie value → identity
	bearers        map[string]signal.Identity // token → identity
	revoked        map[string]bool            // session and device ids
	users          map[string]userInfo        // SetUser overrides
	requestErr     error
	bearerErr      error
	revalidateErr  error
	revalidateGate chan struct{} // HoldRevalidate: Revalidate waits until it is closed
	revalidations  int
}

type userInfo struct {
	name  string
	admin bool
}

// NewAuth returns an Auth without sessions.
func NewAuth() *Auth {
	return &Auth{
		sessions: make(map[string]signal.Identity),
		bearers:  make(map[string]signal.Identity),
		revoked:  make(map[string]bool),
		users:    make(map[string]userInfo),
	}
}

// AddSession registers a cookie session: requests with the cookie value authenticate as id.
func (a *Auth) AddSession(cookie string, id signal.Identity) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions[cookie] = id
}

// AddBearer registers a bearer token that authenticates as id (the M2 device flow).
func (a *Auth) AddBearer(token string, id signal.Identity) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.bearers[token] = id
}

// Revoke invalidates a session or device id: its cookie or token no longer authenticates, and Revalidate returns
// ErrInvalid for it.
func (a *Auth) Revoke(sessionOrDeviceID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.revoked[sessionOrDeviceID] = true
}

// SetUser makes Revalidate report a new name and admin flag for the user.
func (a *Auth) SetUser(userID, name string, admin bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.users[userID] = userInfo{name: name, admin: admin}
}

// FailRequests makes AuthenticateRequest return err; nil restores the normal behavior.
func (a *Auth) FailRequests(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requestErr = err
}

// FailBearer makes AuthenticateBearer return err; nil restores the normal behavior.
func (a *Auth) FailBearer(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.bearerErr = err
}

// FailRevalidate makes Revalidate return err; nil restores the normal behavior.
func (a *Auth) FailRevalidate(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.revalidateErr = err
}

// HoldRevalidate makes the following Revalidate calls block, like a busy store, until release is called or the
// call's context ends (then they return the context's error). release may be called more than once.
func (a *Auth) HoldRevalidate() (release func()) {
	gate := make(chan struct{})
	a.mu.Lock()
	a.revalidateGate = gate
	a.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			if a.revalidateGate == gate {
				a.revalidateGate = nil
			}
			a.mu.Unlock()
			close(gate)
		})
	}
}

// Revalidations returns the number of Revalidate calls so far.
func (a *Auth) Revalidations() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.revalidations
}

// AuthenticateRequest implements signal.Authenticator.
func (a *Auth) AuthenticateRequest(r *http.Request) (signal.Identity, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.requestErr != nil {
		return signal.Identity{}, a.requestErr
	}
	ck, err := r.Cookie(CookieName)
	if err != nil {
		return signal.Identity{}, signal.ErrNoCredentials
	}
	id, ok := a.sessions[ck.Value]
	if !ok || a.revoked[id.SessionID] {
		return signal.Identity{}, signal.ErrInvalid
	}
	return a.current(id), nil
}

// AuthenticateBearer implements signal.Authenticator.
func (a *Auth) AuthenticateBearer(_ context.Context, token protocol.Secret) (signal.Identity, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.bearerErr != nil {
		return signal.Identity{}, a.bearerErr
	}
	id, ok := a.bearers[token.Reveal()]
	if !ok || a.revoked[id.DeviceID] {
		return signal.Identity{}, signal.ErrInvalid
	}
	return a.current(id), nil
}

// Revalidate implements signal.Authenticator.
func (a *Auth) Revalidate(ctx context.Context, id signal.Identity, _ netip.Addr) (signal.Identity, error) {
	a.mu.Lock()
	a.revalidations++
	gate := a.revalidateGate
	a.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return signal.Identity{}, ctx.Err()
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.revalidateErr != nil {
		return signal.Identity{}, a.revalidateErr
	}
	if (id.SessionID != "" && a.revoked[id.SessionID]) || (id.DeviceID != "" && a.revoked[id.DeviceID]) {
		return signal.Identity{}, signal.ErrInvalid
	}
	return a.current(id), nil
}

// current applies SetUser's override to id. a.mu is held.
func (a *Auth) current(id signal.Identity) signal.Identity {
	if u, ok := a.users[id.UserID]; ok {
		id.Name, id.Admin = u.name, u.admin
	}
	return id
}
