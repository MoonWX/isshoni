package session

import (
	"context"
	"errors"
	"sync"

	"github.com/MoonWX/isshoni/internal/client/core"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/protocol"
)

// auth is 01's core.Auth over one server's 02 Manager (03 §9). 02's *deviceauth.Manager isn't a core.Auth itself:
// its SessionEnded returns the resulting State, 01's returns nothing (02 §11.3, 01 §11.2). It is bound to the
// Manager of the origin its core was built for.
type auth struct {
	m              Manager
	onIncompatible func() // the session closes this core; nil outside a session

	mu    sync.Mutex
	ended *deviceauth.State // what the last SessionEnded left
}

var _ core.Auth = (*auth)(nil)

// NewAuth returns 01's core.Auth over m, as the session gives its core clients: AccessToken is
// m.AccessToken(ctx, forceRefresh) (02's own forced-refresh rule); ErrNotLinked comes back in an error whose
// RelinkRequired() is true, ErrIncompatible in one whose UpdateRequired() is true, and every other error is
// transient; SessionEnded is m.SessionEnded.
func NewAuth(m Manager) core.Auth { return &auth{m: m} }

// AccessToken is the Manager's, with 01's two error methods made explicit.
func (a *auth) AccessToken(ctx context.Context, forceRefresh bool) (protocol.Secret, error) {
	tok, err := a.m.AccessToken(ctx, forceRefresh)
	switch {
	case err == nil:
		return tok, nil
	case errors.Is(err, deviceauth.ErrNotLinked):
		return "", &relinkError{err}
	case errors.Is(err, deviceauth.ErrIncompatible):
		if a.onIncompatible != nil {
			a.onIncompatible() // 03 §9: the session closes the core; the version screen follows auth.state
		}
		return "", &updateError{err}
	}
	return "", err
}

// SessionEnded tells the Manager that the core was stopped with code (session_revoked, account_disabled, a second
// unauthenticated): it refreshes once and keeps the link or drops the credential (02 §11.3). The session reads the
// resulting state to decide whether to build a new core client.
func (a *auth) SessionEnded(ctx context.Context, code protocol.ErrorCode) {
	st := a.m.SessionEnded(ctx, code)
	a.mu.Lock()
	a.ended = &st
	a.mu.Unlock()
}

// endedState returns the state the last SessionEnded left, if one ran.
func (a *auth) endedState() (deviceauth.State, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ended == nil {
		return deviceauth.State{}, false
	}
	return *a.ended, true
}

// relinkError says the device is no longer linked (01's RelinkRequired): the core stops with StopUnlinked.
type relinkError struct{ err error }

func (e *relinkError) Error() string        { return "session: relink required: " + e.err.Error() }
func (e *relinkError) Unwrap() error        { return e.err }
func (e *relinkError) RelinkRequired() bool { return true }

// updateError says the app and the server are incompatible (01's UpdateRequired): the core stops with
// StopUpdateRequired.
type updateError struct{ err error }

func (e *updateError) Error() string        { return "session: update required: " + e.err.Error() }
func (e *updateError) Unwrap() error        { return e.err }
func (e *updateError) UpdateRequired() bool { return true }
