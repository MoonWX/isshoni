package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/MoonWX/isshoni/internal/client/deeplink"
	"github.com/MoonWX/isshoni/internal/client/deviceauth"
	"github.com/MoonWX/isshoni/internal/desktop/settings"
	"github.com/MoonWX/isshoni/internal/desktop/share"
)

// The server list as the page changes it (03 §8.1–§8.3, README WA71): adding a server, switching to another,
// removing one, Sign out, the auth.state event, and the approval page of a new link code in the browser. Every
// change of the active server reloads the window once (03 §8.1), so a page only ever runs with the bridge.js, CSP
// and token origin of the active server; the page puts the address it wants to come back at into its own history
// first (Shell.Reload reloads the window's current address, which is the SPA's: a window off the asset origin gets
// the start page).

// ErrNotFound is a server origin the list doesn't have (the bridge's not_found).
var ErrNotFound = errors.New("app: no such server")

// ErrInvalidAddress is an origin that deeplink.NormalizeOrigin refuses (the bridge's invalid_address).
var ErrInvalidAddress = errors.New("app: not a server address")

// AddServer adds the server the page probed and confirmed (03 §8.2's confirm step's Link, or the add step's Add when
// the probe found the same host) and makes it active: 02's Servers.Add, which asks the server's /info and stores its
// own public origin. Then the window reloads, the new active server's manager starts and C2 looks again (03 §9). The
// pending link of an add question for this server is answered: its room waits in PendingLink for the page after the
// link step. A server that is already listed just becomes active (same reload).
//
// Errors: ErrBusy while a share is live (a change of the active server would end it), ErrInvalidAddress, 02's
// ErrTooManyServers, and the /info request's errors (deviceauth.ErrNetwork, ErrBadResponse).
func (a *App) AddServer(ctx context.Context, origin string) error {
	if _, live := a.sharing(); live {
		return ErrBusy
	}
	norm, err := deeplink.NormalizeOrigin(origin, a.d.InsecureLoopback)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidAddress, err)
	}
	res, err := a.d.Servers.Add(ctx, norm)
	switch {
	case errors.Is(err, deeplink.ErrInvalidOrigin):
		return fmt.Errorf("%w: %w", ErrInvalidAddress, err)
	case err != nil:
		return fmt.Errorf("app: add the server: %w", err)
	}
	a.mu.Lock()
	var next *PendingLink
	if p := a.pending; p != nil && p.Add != nil && (p.Add.Origin == norm || p.Add.Origin == res.Origin) {
		if p.Add.RoomID != "" {
			next = &PendingLink{RoomID: p.Add.RoomID}
		}
		a.pending = next
	}
	a.mu.Unlock()
	a.log.Info("app: server added", "existing", res.Existing)
	a.activeChanged()
	return nil
}

// SwitchServer makes a listed server active and reloads the window (03 §8.1); the page of a server that isn't linked
// then shows its link step. Switching to the active server changes nothing. ErrNotFound for an origin that isn't
// listed; ErrBusy while a share is live.
//
// A pending {switch} link for this server is the answer to its question (03 §13.2's "server known, not active, live
// share" row: the page's Open stops the share, then calls SwitchServer): its room stays pending as {roomId} for the
// reloaded page, which opens it (README WA83-R2), as HandleLink's own switch does. Any other pending link is
// dropped: it was about the page that goes.
func (a *App) SwitchServer(origin string) error {
	if _, live := a.sharing(); live {
		return ErrBusy
	}
	o, ok := a.listed(origin)
	if !ok {
		return ErrNotFound
	}
	if a.activeOrigin() == o {
		return nil
	}
	var next *PendingLink
	a.mu.Lock()
	if p := a.pending; p != nil && p.Switch != nil && p.Switch.Origin == o {
		next = &PendingLink{RoomID: p.Switch.RoomID}
	}
	a.mu.Unlock()
	return a.switchServer(o, next)
}

// RemoveServer signs this PC out of a server and drops it from the list (02's Servers.Remove: Unlink first when
// linked, then the entry), deletes its serverPrefs entry (03 §8.3; its prefixed Web Storage keys go at the next
// boot, §14.3) and, when it was the active server, reloads the window for the next one (or for none). The entry goes
// whether or not the server confirmed the revoke. ErrNotFound for an origin that isn't listed; ErrBusy while a share
// is live on it (a share is always on the active server).
func (a *App) RemoveServer(ctx context.Context, origin string) error {
	o, ok := a.listed(origin)
	if !ok {
		return ErrNotFound
	}
	wasActive := a.activeOrigin() == o
	if _, live := a.sharing(); live && wasActive {
		return ErrBusy
	}
	err := a.d.Servers.Remove(ctx, o)
	switch {
	case errors.Is(err, deviceauth.ErrRevokeNotConfirmed):
		a.log.Info("app: server removed; the server didn't confirm the sign-out")
	case err != nil && a.d.Servers.Has(o):
		return fmt.Errorf("app: remove the server: %w", err)
	case err != nil:
		// The entry is gone; only a local step after it failed (the credential or servers.json).
		a.log.Warn("app: server removed, but a local step failed", "err", err)
	}
	if err := a.d.Settings.Update(func(s *settings.Settings) { delete(s.ServerPrefs, o) }); err != nil {
		a.log.Warn("app: the removed server's preferences stay in settings.json", "err", err)
	}
	a.mu.Lock()
	if p := a.pending; p != nil && p.Switch != nil && p.Switch.Origin == o {
		a.pending = nil
	}
	a.mu.Unlock()
	if wasActive {
		a.activeChanged()
	}
	return nil
}

// Unlink is Sign out of the active server (03 §8.3): a live share on it is stopped first, then 02's Manager.Unlink
// revokes this device (POST /api/v1/device/revoke, best effort) and deletes the credential; the server stays in the
// list as "Not linked", and the page follows auth.state to its link step. confirmed is false when the server
// wasn't reached ("Remove {device} on your Devices page to finish"). Without a server there is nothing to sign out
// of: confirmed true.
func (a *App) Unlink(ctx context.Context) (confirmed bool, err error) {
	m := a.d.Servers.Active()
	if m == nil {
		return true, nil
	}
	if _, live := a.sharing(); live {
		a.shares.Stop(ctx, share.CauseUser)
	}
	err = m.Unlink(ctx)
	switch {
	case errors.Is(err, deviceauth.ErrRevokeNotConfirmed):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("app: sign out: %w", err)
	}
	return true, nil
}

// SubscribeAuth calls fn with every link state of the active server (02's OnState through OnAuthState), the
// auth.state event's source (03 §6.3); the bridge forwards them to the page. Another server's states aren't passed
// on: the page of the active server ignores them anyway (03 §5.8). fn must not block. cancel stops the calls.
func (a *App) SubscribeAuth(fn func(deviceauth.State)) (cancel func()) {
	return a.authSubs.add(fn)
}

// activeChanged follows a change of the active server: the window reloads once, the new active server's manager
// starts (02 §11.6) and C2 builds a client for it (03 §9). A pending {error: too_many_servers} goes: the page it
// was offered to has shown it (or goes now), and the reloaded one must not send the user back to it (README
// WA83-R4; SetupPage also calls LinkHandled once it shows it).
func (a *App) activeChanged() {
	a.mu.Lock()
	if p := a.pending; p != nil && p.Error != "" {
		a.pending = nil
	}
	a.mu.Unlock()
	a.d.Shell.ShowWindow(true)
	a.d.Shell.Reload()
	a.startActive()
	if a.pub != nil {
		a.pub.Refresh()
	}
}

// listed returns the listed origin that origin names (any spelling NormalizeOrigin turns into it).
func (a *App) listed(origin string) (string, bool) {
	if origin == "" || len(origin) > maxOrigin {
		return "", false
	}
	norm, err := deeplink.NormalizeOrigin(origin, a.d.InsecureLoopback)
	if err != nil {
		return "", false
	}
	if !slices.ContainsFunc(a.d.Servers.List(), func(e deviceauth.ServerEntry) bool { return e.Origin == norm }) {
		return "", false
	}
	return norm, true
}

// maxOrigin bounds an origin from the page.
const maxOrigin = 512

// activeOrigin is the active server's origin, "" without one.
func (a *App) activeOrigin() string {
	if m := a.d.Servers.Active(); m != nil {
		return m.State().Origin
	}
	return ""
}

// authSubscribers are SubscribeAuth's callbacks.
type authSubscribers struct {
	mu   sync.Mutex
	next int
	fns  map[int]func(deviceauth.State)
}

func (s *authSubscribers) add(fn func(deviceauth.State)) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fns == nil {
		s.fns = map[int]func(deviceauth.State){}
	}
	id := s.next
	s.next++
	s.fns[id] = fn
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.fns, id)
	}
}

func (s *authSubscribers) call(st deviceauth.State) {
	s.mu.Lock()
	fns := make([]func(deviceauth.State), 0, len(s.fns))
	ids := make([]int, 0, len(s.fns))
	for id := range s.fns {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		fns = append(fns, s.fns[id])
	}
	s.mu.Unlock()
	for _, fn := range fns {
		fn(st)
	}
}

// openedCodes remembers the link code whose approval page the app opened last, per server.
type openedCodes struct {
	mu   sync.Mutex
	last map[string]string // origin → user code
}

// first reports whether code is new for origin, and remembers it.
func (o *openedCodes) first(origin, code string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.last == nil {
		o.last = map[string]string{}
	}
	if o.last[origin] == code {
		return false
	}
	o.last[origin] = code
	return true
}

// openLinkPage opens the approval page of a new link code of the active server in the default browser, once per code
// (03 §8.2: the click on Link a moment earlier is the user gesture 02 §11.3 asks for; [Open browser again] repeats it
// through App.OpenExternal). The URI is 02's canonical LinkState value, which Go built after §11.2's check, never a
// string from the server. It runs on its own goroutine: OnAuthState never blocks.
func (a *App) openLinkPage(s deviceauth.State) {
	if s.Status != deviceauth.StatusLinking || s.Link == nil || s.Link.UserCode == "" {
		return
	}
	if !a.opened.first(s.Origin, s.Link.UserCode) {
		return
	}
	url := s.Link.VerificationURIComplete
	go func() {
		if err := a.OpenExternal(url); err != nil {
			a.log.Warn("app: couldn't open the link page in the browser", "err", err)
		}
	}()
}
